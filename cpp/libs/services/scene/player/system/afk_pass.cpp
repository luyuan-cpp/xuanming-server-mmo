#include "afk_pass.h"

#include <algorithm>
#include <limits>
#include <string>
#include <utility>

#include <hiredis/hiredis.h>

#include "muduo/base/Logging.h"

#include "engine/core/time/system/time.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "infra/storage/redis_cluster/redis_cluster_client.h" // tlsPlayerRedis:投影属于玩家域
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "player/comp/player_ownership_comp.h" // AfkPassRedisKey
#include "player/system/afk_pass_rules.h"
#include "player/system/client_request_ledger.h"
#include "player/system/connection_lost_hold.h" // PlayerConnectionLostComp:判定战斗结算是否处于离线托管中
#include "player/system/player_attribute.h"
#include "player/system/player_lifecycle.h" // RequestKeyProgressSave
#include "player/system/player_mutability.h"
#include "thread_context/ecs_context.h"

#include "table/code/afkpassrule_table.h"
#include "table/proto/tip/afk_pass_error_tip.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"

#include "proto/common/component/player_attribute_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"   // PlayerUint64Comp:事实源
#include "proto/common/database/player_cache.pb.h"  // PlayerAllData:提交回调的快照
#include "proto/common/rollback/transaction_log.pb.h" // TX_AFK_PASS_BUY
#include "proto/scene/player_afk_pass.pb.h"

namespace
{
	// 单测注入的投影写出,生产恒为空。
	thread_local AfkPassSystem::ProjectionSender tlsProjectionSenderForTest;
	thread_local bool tlsProjectionFailForTest = false;

	// 本线程投影写失败的累计,随每条失败日志打出(C++ 节点没有 Prometheus 出口,口径同托管在场心跳)。
	thread_local uint64_t tlsProjectionFailures = 0;

	// 规则表第一行。表空或取值不合法返回 false,problem 是原因(只进日志)。
	bool CurrentRule(afk_pass::Rule &rule, const char *&problem)
	{
		const auto &rows = AfkPassRuleTableManager::Instance().FindAll().data();
		if (rows.empty())
		{
			problem = "AfkPassRule table is empty";
			return false;
		}
		const auto &row = *rows.begin();
		rule = afk_pass::Rule{row.period_days(), row.price_currency_type(), row.price(), row.max_remaining_days(),
							  row.remind_before_hours()};
		problem = afk_pass::RuleProblem(rule);
		return problem == nullptr;
	}

	void MarkProjectionConfirmed(Guid playerId, uint64_t seq)
	{
		const auto entity = tlsEcs.GetPlayer(playerId);
		auto &registry = tlsEcs.actorRegistry;
		auto *committedComp = registry.valid(entity) ? registry.try_get<AfkPassCommittedComp>(entity) : nullptr;
		// 仅确认仍匹配最新序列的那次写出:期间若有更新的提交发出了更高 seq,迟到回包不覆盖新目标的未确认状态。
		if (committedComp != nullptr && committedComp->projectionSeq == seq)
		{
			committedComp->confirmedSeq = seq;
		}
	}

	void SendProjection(Guid playerId, uint64_t expiresAtSec, uint64_t seq, uint64_t nowUnixSec, const char *reason)
	{
		const bool set = afk_pass::ProjectionIsSet(expiresAtSec, nowUnixSec);
		if (tlsProjectionSenderForTest)
		{
			tlsProjectionSenderForTest(playerId, expiresAtSec, set);
			if (!tlsProjectionFailForTest)
			{
				MarkProjectionConfirmed(playerId, seq);
			}
			return;
		}
		// 只捕获值:回调到达时实体可能已经没了,按 playerId 回查并核对 seq。
		const auto onReply = [playerId, expiresAtSec, seq, reason](hiredis::Hiredis *, redisReply *reply)
		{
			if (reply != nullptr && reply->type != REDIS_REPLY_ERROR)
			{
				MarkProjectionConfirmed(playerId, seq);
				return;
			}
			++tlsProjectionFailures;
			LOG_WARN << "[AfkPass] metric=afk_pass_projection_failed player_id=" << playerId << " expires_at=" << expiresAtSec
					 << " reason=" << reason << " cause=" << (reply == nullptr ? "no_reply" : "error_reply")
					 << " failures_total=" << tlsProjectionFailures;
		};
		const std::string key = player_ownership::AfkPassRedisKey(playerId);
		int status = REDIS_ERR;
		if (set)
		{
			// 值与 EXAT 同一个数:到期那一刻键自动消失,读方不用再比时间也不会读到过期的月卡。
			const std::string value = std::to_string(expiresAtSec);
			status = tlsPlayerRedis.command(onReply, "SET %b %b EXAT %b", key.data(), key.size(), value.data(), value.size(),
											value.data(), value.size());
		}
		else
		{
			status = tlsPlayerRedis.command(onReply, "DEL %b", key.data(), key.size());
		}
		if (status != REDIS_OK)
		{
			// 没发出去(玩家域 Redis 未连接、该 slot 无主或排队已满):回调不会来,confirmedSeq 保持落后,
			// 下一次存盘提交、托管开始或心跳都会自动重发。
			++tlsProjectionFailures;
			LOG_WARN << "[AfkPass] metric=afk_pass_projection_failed player_id=" << playerId << " expires_at=" << expiresAtSec
					 << " reason=" << reason << " cause=not_sent failures_total=" << tlsProjectionFailures;
		}
	}
	template <typename Unsigned>
	Unsigned SaturatingAdd(Unsigned base, Unsigned delta)
	{
		constexpr Unsigned kMax = std::numeric_limits<Unsigned>::max();
		return delta > kMax - base ? kMax : static_cast<Unsigned>(base + delta);
	}

	void ClearHostedRecapFields(PlayerUint64Comp &profile)
	{
		profile.set_hosted_recap_started_at(0);
		profile.set_hosted_recap_ended_at(0);
		profile.set_hosted_recap_battle_count(0);
		profile.set_hosted_recap_win_count(0);
		profile.set_hosted_recap_exp_gained(0);
		profile.set_hosted_recap_silver_gained(0);
		profile.set_hosted_recap_item_count(0);
		profile.set_hosted_recap_pending_ack(false);
	}

	void FillHostedRecap(const PlayerUint64Comp &profile, AfkPassInfo &info)
	{
		if (profile.hosted_recap_started_at() == 0)
		{
			return;
		}
		auto *recap = info.mutable_hosted_recap();
		recap->set_started_at(profile.hosted_recap_started_at());
		recap->set_ended_at(profile.hosted_recap_ended_at());
		recap->set_battle_count(profile.hosted_recap_battle_count());
		recap->set_win_count(profile.hosted_recap_win_count());
		recap->set_exp_gained(profile.hosted_recap_exp_gained());
		recap->set_silver_gained(profile.hosted_recap_silver_gained());
		recap->set_item_count(profile.hosted_recap_item_count());
		recap->set_pending_ack(profile.hosted_recap_pending_ack());
	}
} // namespace

bool AfkPassSystem::ValidateTables()
{
	afk_pass::Rule rule;
	const char *problem = nullptr;
	if (!CurrentRule(rule, problem))
	{
		LOG_ERROR << "[AfkPass] table validation failed: " << (problem != nullptr ? problem : "unknown");
		return false;
	}
	return true;
}

uint64_t AfkPassSystem::ExpiresAt(entt::entity player)
{
	auto &registry = tlsEcs.actorRegistry;
	if (!registry.valid(player))
	{
		return 0;
	}
	const auto *profile = registry.try_get<PlayerUint64Comp>(player);
	return profile != nullptr ? profile->afk_pass_expires_at() : 0;
}

bool AfkPassSystem::IsActive(entt::entity player, uint64_t nowUnixSec)
{
	return afk_pass::IsActive(ExpiresAt(player), nowUnixSec);
}

bool AfkPassSystem::IsActiveNow(entt::entity player)
{
	return IsActive(player, TimeSystem::NowSecondsUTC());
}

bool AfkPassSystem::AutoReturnCityOnTeamEnd(entt::entity player)
{
	auto &registry = tlsEcs.actorRegistry;
	if (!registry.valid(player))
	{
		return false;
	}
	const auto *profile = registry.try_get<PlayerUint64Comp>(player);
	return profile != nullptr && profile->afk_auto_return_city_on_team_end();
}

void AfkPassSystem::FillInfo(entt::entity player, uint64_t nowUnixSec, AfkPassInfo &info)
{
	info.Clear();
	const uint64_t expiresAt = ExpiresAt(player);
	const bool active = afk_pass::IsActive(expiresAt, nowUnixSec);
	info.set_expires_at(expiresAt);
	info.set_server_now(nowUnixSec);
	info.set_unlocked_loadout_slots(afk_pass::UnlockedLoadoutSlots(active));
	info.set_stage2_tradable_enabled(active);
	if (auto &registry = tlsEcs.actorRegistry; registry.valid(player))
	{
		if (const auto *profile = registry.try_get<PlayerUint64Comp>(player))
		{
			info.set_auto_return_city_on_team_end(profile->afk_auto_return_city_on_team_end());
			FillHostedRecap(*profile, info);
		}
	}
	afk_pass::Rule rule;
	const char *problem = nullptr;
	if (!CurrentRule(rule, problem))
	{
		return; // 售价各项留 0 = 暂不出售;原因在开通时打(这里是纯读取,登录与打开面板都会调)
	}
	info.set_price_currency_type(rule.priceCurrencyType);
	info.set_price(rule.price);
	info.set_period_days(rule.periodDays);
	info.set_max_remaining_days(rule.maxRemainingDays);
	info.set_remind_before_hours(rule.remindBeforeHours);
	info.set_balance(CurrencySystem::GetBalance(player, static_cast<CurrencyType>(rule.priceCurrencyType)));
}

uint32_t AfkPassSystem::Buy(entt::entity player, const BuyAfkPassRequest &request, uint64_t nowUnixSec,
							BuyAfkPassResponse &response)
{
	response.clear_info();
	response.clear_replayed();
	auto &registry = tlsEcs.actorRegistry;
	if (!registry.valid(player))
	{
		return kEntityIsNull;
	}
	const auto *guid = registry.try_get<PlayerGuidComp>(player);
	const Guid playerId = guid != nullptr ? guid->value : kInvalidGuid;

	// ── 第一段:只读预检。这一段里任何一处 return 都保证「什么都没改」 ──────────────────

	// 1. 幂等键(docs/design/client-request-idempotency.md §3.4):查账排在一切依赖状态的校验之前 —— 首次已经成交的话,
	//    余额、剩余时长、规则都可能变了,重放再过一遍预检会被拒掉,客户端就以为那一笔没成。新接口没有旧客户端,键必须非 0。
	const uint64_t clientRequestId = request.client_request_id();
	if (clientRequestId == 0)
	{
		return kInvalidParameter;
	}
	std::string fingerprint = ClientRequestFingerprint(request);
	ClientRequestResult first;
	switch (client_request_dedup::Check(player, clientRequestId, ClientRequestKind::kBuyAfkPass, fingerprint, first))
	{
	case ClientRequestVerdict::kReplay:
		// 首次结果就是开通之后的月卡状态,当前状态里都有,账本不存别的。
		response.set_replayed(true);
		FillInfo(player, nowUnixSec, *response.mutable_info());
		return kSuccess;
	case ClientRequestVerdict::kMismatch:
		return kInvalidParameter;
	case ClientRequestVerdict::kUnavailable:
		return kServiceUnavailable; // 账本缺失或已判坏:分不清是不是重发,不执行(fail-closed)
	default:
		break;
	}

	// 2. 规则表。
	afk_pass::Rule rule;
	const char *problem = nullptr;
	if (!CurrentRule(rule, problem))
	{
		LOG_ERROR << "[AfkPass] purchase refused: AfkPassRule is not usable (" << problem << ") player_id=" << playerId;
		return kAfkPassUnavailable;
	}
	// 3. 报价防呆:价格与币种与玩家看到的不同(配表热更过)就拒绝,让客户端刷新后再买。
	if (request.expected_currency_type() != rule.priceCurrencyType || request.expected_price() != rule.price)
	{
		return kAfkPassPriceChanged;
	}
	// 4. 不可写窗口(冻结、跨区交接、退出在途、无归属围栏):与全局玩家可写闸保持一致。
	//    战斗中不拦:开通不碰战斗快照。
	if (!player_mutability::IsPlayerMutable(player))
	{
		return kAfkPassFrozen;
	}
	// 5. 事实源:持久组件缺失 = 加载不变量被破坏,不现造一份(那等于把已有的月卡清零)。
	auto *profile = registry.try_get<PlayerUint64Comp>(player);
	if (profile == nullptr)
	{
		LOG_ERROR << "[AfkPass] purchase refused: player has no PlayerUint64Comp (load invariant broken) player_id=" << playerId;
		return kAfkPassUnavailable;
	}
	const uint64_t before = profile->afk_pass_expires_at();
	const auto after = afk_pass::RenewedExpiry(before, nowUnixSec, rule.periodDays, rule.maxRemainingDays);
	if (!after)
	{
		return kAfkPassStackLimit;
	}
	// 6. 余额:服务器裁决,客户端不拿本地余额拦。
	const auto currency = static_cast<CurrencyType>(rule.priceCurrencyType);
	const auto price = static_cast<int64_t>(rule.price); // RuleProblem 保证 1..1e9
	if (!CurrencySystem::CanAfford(player, currency, price))
	{
		return kAfkPassCurrencyNotEnough;
	}

	// ── 第二段:修改。扣钱 → 写到期 → 记键,同一次同步调用里完成,中间不让出 EventLoop ─────────
	//
	// 先扣钱、后加天数:后一步是内存赋值,不会失败,所以不需要退款路径。扣钱会失败的条件(金额、币种、冻结、缺组件、
	// 余额)上面都已排除,走到失败分支就是缺陷:什么都还没改,干净地拒绝。
	if (const auto err = CurrencySystem::DeductCurrency(player, currency, price, TX_AFK_PASS_BUY, clientRequestId);
		err != kSuccess)
	{
		LOG_ERROR << "[AfkPass] purchase refused: charge rejected after precheck (nothing changed) player_id=" << playerId
				  << " price=" << price << " currency_type=" << rule.priceCurrencyType << " err=" << err;
		return kAfkPassUnavailable;
	}
	profile->set_afk_pass_expires_at(*after);
	// 成交了才记键(只记成功)。首次结果取当前状态,不另存。
	client_request_dedup::Remember(player, clientRequestId, ClientRequestKind::kBuyAfkPass, std::move(fingerprint),
								   ClientRequestResult{});
	// 尽快落盘:投影随这次提交写出,玩家开通后马上断线也来得及让 player_locator 看到(1 秒节流)。
	PlayerLifecycleSystem::RequestKeyProgressSave(player, "afk_pass_buy");
	if (registry.all_of<PlayerAttributeComp>(player))
	{
		PlayerAttributeSystem::PushPanel(player);
	}

	LOG_INFO << "[AfkPass] metric=afk_pass_bought player_id=" << playerId << " expires_before=" << before
			 << " expires_after=" << *after << " price=" << price << " currency_type=" << rule.priceCurrencyType
			 << " client_request_id=" << clientRequestId;

	// ── 第三段:组应答。开通已经生效,这里出任何问题都不回滚 ──────────────────────────
	FillInfo(player, nowUnixSec, *response.mutable_info());
	return kSuccess;
}

uint32_t AfkPassSystem::GmSet(entt::entity player, uint32_t remainingSeconds, uint64_t nowUnixSec, AfkPassInfo &info)
{
	auto &registry = tlsEcs.actorRegistry;
	if (!registry.valid(player))
	{
		return kEntityIsNull;
	}
	if (remainingSeconds > afk_pass::kGmMaxRemainingSeconds)
	{
		return kInvalidParameter;
	}
	// 与 Buy 共用全局可写闸:冻结、跨区 / 跨节点交接在途、退出在途或无归属围栏时不得改写存档事实源。
	if (!player_mutability::IsPlayerMutable(player))
	{
		return kAfkPassFrozen;
	}
	auto *profile = registry.try_get<PlayerUint64Comp>(player);
	const auto *guid = registry.try_get<PlayerGuidComp>(player);
	if (profile == nullptr || guid == nullptr)
	{
		LOG_ERROR << "[AfkPass] GM set refused: player has no PlayerUint64Comp / PlayerGuidComp (load invariant broken)";
		return kAfkPassUnavailable;
	}
	const uint64_t before = profile->afk_pass_expires_at();
	const uint64_t after = afk_pass::GmExpiry(nowUnixSec, remainingSeconds);
	profile->set_afk_pass_expires_at(after);
	PlayerLifecycleSystem::RequestKeyProgressSave(player, "afk_pass_gm_set");
	if (registry.all_of<PlayerAttributeComp>(player))
	{
		PlayerAttributeSystem::PushPanel(player);
	}
	// GM 改的不是资产,不写流水;这一行带前后值,是唯一的留痕。
	LOG_INFO << "[AfkPass] metric=afk_pass_gm_set player_id=" << guid->value << " expires_before=" << before
			 << " expires_after=" << after << " remaining_seconds=" << remainingSeconds;
	FillInfo(player, nowUnixSec, info);
	return kSuccess;
}

uint32_t AfkPassSystem::SetSettings(entt::entity player, bool autoReturnCityOnTeamEnd, uint64_t nowUnixSec,
									AfkPassInfo &info)
{
	auto &registry = tlsEcs.actorRegistry;
	if (!registry.valid(player))
	{
		return kEntityIsNull;
	}
	if (!player_mutability::IsPlayerMutable(player))
	{
		return kAfkPassFrozen;
	}
	auto *profile = registry.try_get<PlayerUint64Comp>(player);
	if (profile == nullptr)
	{
		LOG_ERROR << "[AfkPass] set settings refused: player has no PlayerUint64Comp (load invariant broken)";
		return kAfkPassUnavailable;
	}
	if (profile->afk_auto_return_city_on_team_end() != autoReturnCityOnTeamEnd)
	{
		profile->set_afk_auto_return_city_on_team_end(autoReturnCityOnTeamEnd);
		PlayerLifecycleSystem::RequestKeyProgressSave(player, "afk_pass_settings");
		const auto *guid = registry.try_get<PlayerGuidComp>(player);
		LOG_INFO << "[AfkPass] metric=afk_pass_settings_updated player_id="
				 << (guid != nullptr ? guid->value : kInvalidGuid)
				 << " auto_return_city=" << (autoReturnCityOnTeamEnd ? 1 : 0);
	}
	FillInfo(player, nowUnixSec, info);
	return kSuccess;
}

uint32_t AfkPassSystem::AckHostedRecap(entt::entity player, uint64_t startedAt, uint64_t nowUnixSec, AfkPassInfo &info)
{
	auto &registry = tlsEcs.actorRegistry;
	if (!registry.valid(player))
	{
		return kEntityIsNull;
	}
	if (!player_mutability::IsPlayerMutable(player))
	{
		return kAfkPassFrozen;
	}
	auto *profile = registry.try_get<PlayerUint64Comp>(player);
	if (profile == nullptr)
	{
		LOG_ERROR << "[AfkPass] ack hosted recap refused: player has no PlayerUint64Comp (load invariant broken)";
		return kAfkPassUnavailable;
	}
	if (profile->hosted_recap_pending_ack() &&
		(startedAt == 0 || profile->hosted_recap_started_at() == startedAt))
	{
		const uint64_t ackedStart = profile->hosted_recap_started_at();
		ClearHostedRecapFields(*profile);
		PlayerLifecycleSystem::RequestKeyProgressSave(player, "afk_hosted_recap_ack");
		const auto *guid = registry.try_get<PlayerGuidComp>(player);
		LOG_INFO << "[AfkPass] metric=afk_hosted_recap_acked player_id="
				 << (guid != nullptr ? guid->value : kInvalidGuid) << " started_at=" << ackedStart;
	}
	FillInfo(player, nowUnixSec, info);
	return kSuccess;
}

void AfkPassSystem::OnHostingStarted(entt::entity player, uint64_t nowUnixSec)
{
	auto &registry = tlsEcs.actorRegistry;
	if (!registry.valid(player))
	{
		return;
	}
	auto *profile = registry.try_get<PlayerUint64Comp>(player);
	if (profile == nullptr)
	{
		return;
	}
	// 跨节点托管落点或排空疏散重建保留时,原会话尚未结束(started_at != 0 && ended_at == 0):沿用原起点与累计收益。
	if (profile->hosted_recap_started_at() != 0 && profile->hosted_recap_ended_at() == 0)
	{
		return;
	}
	ClearHostedRecapFields(*profile);
	profile->set_hosted_recap_started_at(nowUnixSec != 0 ? nowUnixSec : 1);
	if (player_mutability::IsPlayerMutable(player))
	{
		PlayerLifecycleSystem::RequestKeyProgressSave(player, "afk_hosting_recap_start");
	}
}

void AfkPassSystem::RecordHostedBattleSettlement(entt::entity player, bool won, uint64_t expGained,
												 uint64_t silverGained, uint32_t itemCount, uint64_t nowUnixSec)
{
	auto &registry = tlsEcs.actorRegistry;
	if (!registry.valid(player))
	{
		return;
	}
	const auto *held = registry.try_get<PlayerConnectionLostComp>(player);
	if (held == nullptr || !held->hostingStarted)
	{
		return; // 仅统计离线托管期间的真实战斗结算
	}
	auto *profile = registry.try_get<PlayerUint64Comp>(player);
	if (profile == nullptr)
	{
		return;
	}
	if (profile->hosted_recap_started_at() == 0 || profile->hosted_recap_ended_at() != 0)
	{
		ClearHostedRecapFields(*profile);
		profile->set_hosted_recap_started_at(nowUnixSec != 0 ? nowUnixSec : 1);
	}
	profile->set_hosted_recap_battle_count(SaturatingAdd<uint32_t>(profile->hosted_recap_battle_count(), 1));
	if (won)
	{
		profile->set_hosted_recap_win_count(SaturatingAdd<uint32_t>(profile->hosted_recap_win_count(), 1));
	}
	profile->set_hosted_recap_exp_gained(SaturatingAdd<uint64_t>(profile->hosted_recap_exp_gained(), expGained));
	profile->set_hosted_recap_silver_gained(
		SaturatingAdd<uint64_t>(profile->hosted_recap_silver_gained(), silverGained));
	profile->set_hosted_recap_item_count(SaturatingAdd<uint32_t>(profile->hosted_recap_item_count(), itemCount));
}

void AfkPassSystem::OnHostingEnded(entt::entity player, uint64_t nowUnixSec, bool requestSave)
{
	auto &registry = tlsEcs.actorRegistry;
	if (!registry.valid(player))
	{
		return;
	}
	auto *profile = registry.try_get<PlayerUint64Comp>(player);
	if (profile == nullptr || profile->hosted_recap_started_at() == 0 || profile->hosted_recap_ended_at() != 0)
	{
		return;
	}
	const uint64_t endedAt = std::max(nowUnixSec, profile->hosted_recap_started_at());
	profile->set_hosted_recap_ended_at(endedAt);
	profile->set_hosted_recap_pending_ack(true);
	if (requestSave && player_mutability::IsPlayerMutable(player))
	{
		PlayerLifecycleSystem::RequestKeyProgressSave(player, "afk_hosting_recap_end");
	}
}

void AfkPassSystem::OnPlayerLoaded(entt::entity player, uint64_t nowUnixSec)
{
	auto &registry = tlsEcs.actorRegistry;
	const auto *guid = registry.valid(player) ? registry.try_get<PlayerGuidComp>(player) : nullptr;
	if (guid == nullptr)
	{
		return;
	}
	// 刚从库里读出来的就是已提交的值:按它重建投影(到期在将来 SET,否则 DEL —— 回档或投影残留在这里被改正)。
	const uint64_t expiresAt = ExpiresAt(player);
	registry.emplace_or_replace<AfkPassCommittedComp>(player, AfkPassCommittedComp{expiresAt, /*projectionSeq=*/1, /*confirmedSeq=*/0});
	SendProjection(guid->value, expiresAt, /*seq=*/1, nowUnixSec, "loaded");
}

void AfkPassSystem::OnSaveCommitted(Guid playerId, const PlayerAllData &committed, uint64_t nowUnixSec)
{
	// 存盘未落定时不能重新加载(PlayerStoreClient::Seed),所以按 Guid 找到的就是提交这份快照的实体。
	const auto player = tlsEcs.GetPlayer(playerId);
	auto &registry = tlsEcs.actorRegistry;
	auto *committedComp = registry.valid(player) ? registry.try_get<AfkPassCommittedComp>(player) : nullptr;
	if (committedComp == nullptr)
	{
		return; // 实体已经不在,或不是经加载链建出来的:下一个持有者加载时会按库里的值写
	}
	const uint64_t expiresAt = committed.player_database_data().uint64_pb_component().afk_pass_expires_at();
	if (expiresAt == committedComp->expiresAtSec && committedComp->confirmedSeq == committedComp->projectionSeq)
	{
		return; // 月卡没变且上次投影已收到 Redis 确认:每次存盘都写一遍是白占 Redis
	}
	// 同一玩家同时只有一笔在途存盘,回调按提交顺序到达,投影不会倒退。上次写失败时即便到期值未变也在此补发自愈。
	committedComp->expiresAtSec = expiresAt;
	const uint64_t seq = ++committedComp->projectionSeq;
	SendProjection(playerId, expiresAt, seq, nowUnixSec, "committed");
}

void AfkPassSystem::RefreshProjection(entt::entity player, uint64_t nowUnixSec)
{
	auto &registry = tlsEcs.actorRegistry;
	if (!registry.valid(player))
	{
		return;
	}
	auto *committedComp = registry.try_get<AfkPassCommittedComp>(player);
	const auto *guid = registry.try_get<PlayerGuidComp>(player);
	if (committedComp == nullptr || guid == nullptr)
	{
		return;
	}
	const bool active = afk_pass::ProjectionIsSet(committedComp->expiresAtSec, nowUnixSec);
	const bool unconfirmed = committedComp->confirmedSeq != committedComp->projectionSeq;
	// 有效月卡按心跳周期补写 SET(防 Redis 丢键);若先前一次 DEL(清除 / 回档)没收到确认,也在此补发 DEL 清脏键。
	if (!active && !unconfirmed)
	{
		return;
	}
	const uint64_t seq = ++committedComp->projectionSeq;
	SendProjection(guid->value, committedComp->expiresAtSec, seq, nowUnixSec, "hosted_refresh");
}

void AfkPassSystem::SetProjectionSenderForTest(ProjectionSender sender)
{
	tlsProjectionSenderForTest = std::move(sender);
	if (!tlsProjectionSenderForTest)
	{
		tlsProjectionFailForTest = false;
	}
}

void AfkPassSystem::SetProjectionFailForTest(bool fail)
{
	tlsProjectionFailForTest = fail;
}
