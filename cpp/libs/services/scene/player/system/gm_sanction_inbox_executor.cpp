#include "player/system/gm_sanction_inbox_executor.h"

#include <cstdint>
#include <limits>

#include "muduo/base/Logging.h"

#include "engine/core/type_define/player_guid_comp.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/player_inbox/player_inbox.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/player_inbox_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace
{
	PlayerInboxExecResult Applied()
	{
		return PlayerInboxExecResult{PlayerInboxExecKind::kApplied, 0};
	}

	PlayerInboxExecResult Rejected(uint32_t tipId)
	{
		return PlayerInboxExecResult{PlayerInboxExecKind::kRejected, tipId};
	}

	// 原语的 tip 码 → 结局。kSuccess 之外的码都是确定性拒绝(原语只在参数 / 状态不可能成功时拒绝)。
	PlayerInboxExecResult FromTip(uint32_t tipId)
	{
		return tipId == static_cast<uint32_t>(kSuccess) ? Applied() : Rejected(tipId);
	}


	// CurrencyComp 与 PlayerCurrencyComp 都是只由加载创建的持久组件。缺了说明加载不变量已被破坏:
	// 不改任何状态、回 kDefer,不把这条 GM 处置因本地缺陷钉成终态 REJECTED(ecs-component-access-rules.md 规则 4)。
	bool HasCurrencyState(entt::entity player, const char *executor)
	{
		if (tlsEcs.actorRegistry.all_of<CurrencyComp, PlayerCurrencyComp>(player))
		{
			return true;
		}
		LOG_ERROR << "[PlayerInbox] " << executor
				  << " deferred: persisted currency component missing (load invariant broken) player_id="
				  << tlsEcs.GetPlayerId(player)
				  << " has_currency=" << tlsEcs.actorRegistry.all_of<CurrencyComp>(player)
				  << " has_player_currency=" << tlsEcs.actorRegistry.all_of<PlayerCurrencyComp>(player);
		return false;
	}

	PlayerInboxExecResult Deferred()
	{
		return PlayerInboxExecResult{PlayerInboxExecKind::kDefer, 0};
	}

	PlayerInboxExecResult VersionSkew()
	{
		return PlayerInboxExecResult{PlayerInboxExecKind::kVersionSkew, 0};
	}

	// 载荷里的币种本二进制不认识。Go 入箱已按同一张取值表(proto/common/base/currency_type.proto)
	// 拒绝写错的值,所以这里只可能是更新版本加的币种:是版本错位,不是确定性拒绝。
	// 记成 REJECTED 这条 GM 处置就永远不会执行了;回 kDefer 则本地续做只会空转 —— 本二进制在会话内
	// 永远不会认识这个值(player-inbox-service.md §7.6 与未知类型同口径)。kVersionSkew 让收件箱停用本域,
	// 实体重建后重拉,落到认识这个币种的节点再执行。币种不进 reason:reason 按约定是 tip id。
	PlayerInboxExecResult CurrencyVersionSkew(entt::entity player, const char *executor, uint32_t currencyType)
	{
		LOG_WARN << "[PlayerInbox] metric=persist_version_skew where=inbox_currency executor=" << executor
				 << " player_id=" << tlsEcs.GetPlayerId(player) << " currency_type=" << currencyType
				 << "; currency unknown to this version, halting the domain";
		return VersionSkew();
	}

	// 两种封锁类型共用同一个载荷;只负责解码,币种是否认识由调用方判(结局不同于解码失败)。
	bool DecodeGainPayload(const std::string &payload, uint32_t &currencyType)
	{
		player_inbox::GmCurrencyGainPayload decoded;
		if (!decoded.ParseFromString(payload))
		{
			return false;
		}
		currencyType = decoded.currency_type();
		return true;
	}
} // namespace

PlayerInboxExecResult GmSanctionInboxExecutor::BlockCurrencyGain(entt::entity player, const std::string &payload)
{
	uint32_t currencyType = 0;
	if (!DecodeGainPayload(payload, currencyType))
	{
		return Rejected(kPlayerInboxPayloadInvalid);
	}
	if (!IsKnownCurrencyType(currencyType))
	{
		return CurrencyVersionSkew(player, "BlockCurrencyGain", currencyType);
	}
	if (!HasCurrencyState(player, "BlockCurrencyGain"))
	{
		return Deferred();
	}
	return FromTip(CurrencySystem::BlockCurrency(player, static_cast<CurrencyType>(currencyType)));
}

PlayerInboxExecResult GmSanctionInboxExecutor::UnblockCurrencyGain(entt::entity player, const std::string &payload)
{
	uint32_t currencyType = 0;
	if (!DecodeGainPayload(payload, currencyType))
	{
		return Rejected(kPlayerInboxPayloadInvalid);
	}
	if (!IsKnownCurrencyType(currencyType))
	{
		return CurrencyVersionSkew(player, "UnblockCurrencyGain", currencyType);
	}
	if (!HasCurrencyState(player, "UnblockCurrencyGain"))
	{
		return Deferred();
	}
	return FromTip(CurrencySystem::UnblockCurrency(player, static_cast<CurrencyType>(currencyType)));
}

PlayerInboxExecResult GmSanctionInboxExecutor::AttachCurrencyDebt(entt::entity player, const std::string &payload)
{
	player_inbox::GmCurrencyDebtPayload decoded;
	if (!decoded.ParseFromString(payload))
	{
		return Rejected(kPlayerInboxPayloadInvalid);
	}
	// 货币在协议里是 uint64(AGENTS §4),AttachDebt 的参数是 int64_t:入箱时已校验 ∈ (0, INT64_MAX],
	// 这里再判一次,越界的载荷不能靠窄化变成负数或 0。与版本无关的确定性校验排在版本错位之前:
	// 金额写错的载荷在任何版本上都不会成功,不能因为币种是新的就停用本域等一个注定的拒绝。
	constexpr uint64_t kMaxOweAmount = static_cast<uint64_t>(std::numeric_limits<int64_t>::max());
	if (decoded.owe_amount() == 0 || decoded.owe_amount() > kMaxOweAmount)
	{
		return Rejected(kPlayerInboxPayloadInvalid);
	}
	if (!IsKnownCurrencyType(decoded.currency_type()))
	{
		return CurrencyVersionSkew(player, "AttachCurrencyDebt", decoded.currency_type());
	}
	if (!HasCurrencyState(player, "AttachCurrencyDebt"))
	{
		return Deferred();
	}
	// 同币种的既有欠款带着本版本不认识的字段:AttachDebt 会拒绝合并(混版本保护,
	// docs/design/mixed-version-persist-field-protection.md §4.4)。这是版本错位,不是确定性拒绝 ——
	// 记成 REJECTED 这笔 GM 欠款就永远不会再挂了(player-inbox-service.md §7.6「版本错位绝不能当成终态的 REJECTED」)。
	// 回 kVersionSkew 而不是 kDefer:这种错位会在会话内消失(那笔欠款被 AddCurrency 还清删除),DEFER 的本地续做
	// 看不到服务端之后挂的 VOID,会把已作废的处置挂上并报 APPLIED(评审 L1)。kVersionSkew 让收件箱停用本域,
	// 实体重建后重拉(退出完成后再登录,或换节点);退出保留期内重登会复用实体,不重拉:已作废就记 SKIPPED,落到新版本节点就正常挂。
	// 在调用原语之前用只读谓词判,不拿原语的通用返回码 kFeatureUnavailable 反推。
	if (CurrencySystem::DebtCarriesUnknownFields(player, static_cast<CurrencyType>(decoded.currency_type())))
	{
		LOG_WARN << "[PlayerInbox] AttachCurrencyDebt version skew: existing debt carries fields unknown to this version"
				 << " player_id=" << tlsEcs.GetPlayerId(player) << " currency_type=" << decoded.currency_type();
		return VersionSkew();
	}
	return FromTip(CurrencySystem::AttachDebt(player, static_cast<CurrencyType>(decoded.currency_type()),
											  static_cast<int64_t>(decoded.owe_amount()), decoded.reason(),
											  decoded.gm_operator(), decoded.expires_at()));
}
