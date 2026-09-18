
#include "player_currency_handler.h"

///<<< BEGIN WRITING YOUR CODE
#include <muduo/base/Logging.h>

#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "core/utils/registry/game_registry.h"
#include "player_gm_guard.h" // P0-a:GM 客户端指令的 scene 侧第二道锁
#include "services/scene/player/system/player_lifecycle.h" // IsCrossZoneFrozen — cross-zone-readiness-audit.md §11.1

namespace
{
	// 四条 GM 货币 RPC 的统一前置闸(P0-a)。true = 已写 tip,调用点直接 return。
	// 第一道锁在 gate(消息号 37 / 49 / 94 / 95),这里防绕开 gate 直连 scene。
	bool RejectGmCurrencyRpc(const char* rpcName)
	{
		if (!scene_gm_guard::RejectGmClientRpc(rpcName))
		{
			return false;
		}
		tlsEcs.globalRegistry.get_or_emplace<TipInfoMessage>(tlsEcs.GlobalEntity()).set_id(kFeatureUnavailable);
		return true;
	}
}
///<<< END WRITING YOUR CODE

void SceneCurrencyClientPlayerHandler::GmAddCurrency(entt::entity player,const ::GmAddCurrencyRequest* request,
	::GmAddCurrencyResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (RejectGmCurrencyRpc("GmAddCurrency")) return;
	// Frozen check is enforced inside CurrencySystem::AddCurrency; falls
	// through to error reporting via the err path below. Kept here as a
	// reminder: GM can absolutely fire while a player is mid cross-zone
	// migration, and the source-side write would silently disappear.
	const auto type = static_cast<CurrencyType>(request->currency_type());
	const auto err = CurrencySystem::AddCurrency(player, type, request->amount());
	if (err != kSuccess)
	{
		tlsEcs.globalRegistry.get_or_emplace<TipInfoMessage>(tlsEcs.GlobalEntity()).set_id(err);
		return;
	}
	response->set_balance_after(CurrencySystem::GetBalance(player, type));
///<<< END WRITING YOUR CODE

}

void SceneCurrencyClientPlayerHandler::GmDeductCurrency(entt::entity player,const ::GmDeductCurrencyRequest* request,
	::GmDeductCurrencyResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (RejectGmCurrencyRpc("GmDeductCurrency")) return;
	// Frozen check is enforced inside CurrencySystem::DeductCurrency.
	const auto type = static_cast<CurrencyType>(request->currency_type());
	const auto err = CurrencySystem::DeductCurrency(player, type, request->amount());
	if (err != kSuccess)
	{
		tlsEcs.globalRegistry.get_or_emplace<TipInfoMessage>(tlsEcs.GlobalEntity()).set_id(err);
		return;
	}
	response->set_balance_after(CurrencySystem::GetBalance(player, type));
///<<< END WRITING YOUR CODE

}

void SceneCurrencyClientPlayerHandler::GetCurrencyList(entt::entity player,const ::GetCurrencyListRequest* request,
	::GetCurrencyListResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	const auto* currency = tlsEcs.actorRegistry.try_get<CurrencyComp>(player);
	if (currency != nullptr)
	{
		response->mutable_currency()->CopyFrom(*currency);
	}
///<<< END WRITING YOUR CODE

}

void SceneCurrencyClientPlayerHandler::GmBlockCurrency(entt::entity player,const ::GmBlockCurrencyRequest* request,
	::GmBlockCurrencyResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (RejectGmCurrencyRpc("GmBlockCurrency")) return;
	// Frozen check (cross-zone-readiness-audit.md §11.1): blocking a
	// currency type writes to CurrencyComp.blocked_types, which the
	// destination zone's marshalled snapshot already carries. A source-
	// side block here would diverge from the destination — when the
	// destination zone unfreezes the player, the block we just stamped
	// is gone. Reject early.
	if (PlayerLifecycleSystem::IsCrossZoneFrozen(player))
	{
		LOG_WARN << "GmBlockCurrency rejected: player frozen for cross-zone migration. "
		         << "currency_type=" << request->currency_type()
		         << " entity=" << entt::to_integral(player);
		tlsEcs.globalRegistry.get_or_emplace<TipInfoMessage>(tlsEcs.GlobalEntity()).set_id(kInvalidParameter);
		return;
	}
	const auto type = static_cast<CurrencyType>(request->currency_type());
	const auto err = CurrencySystem::BlockCurrency(player, type);
	if (err != kSuccess)
	{
		tlsEcs.globalRegistry.get_or_emplace<TipInfoMessage>(tlsEcs.GlobalEntity()).set_id(err);
		return;
	}
///<<< END WRITING YOUR CODE

}

void SceneCurrencyClientPlayerHandler::GmUnblockCurrency(entt::entity player,const ::GmUnblockCurrencyRequest* request,
	::GmUnblockCurrencyResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (RejectGmCurrencyRpc("GmUnblockCurrency")) return;
	// Frozen check (cross-zone-readiness-audit.md §11.1): same rationale
	// as GmBlockCurrency above — unblocking on the source side would not
	// reach the destination's snapshot. Reject early.
	if (PlayerLifecycleSystem::IsCrossZoneFrozen(player))
	{
		LOG_WARN << "GmUnblockCurrency rejected: player frozen for cross-zone migration. "
		         << "currency_type=" << request->currency_type()
		         << " entity=" << entt::to_integral(player);
		tlsEcs.globalRegistry.get_or_emplace<TipInfoMessage>(tlsEcs.GlobalEntity()).set_id(kInvalidParameter);
		return;
	}
	const auto type = static_cast<CurrencyType>(request->currency_type());
	const auto err = CurrencySystem::UnblockCurrency(player, type);
	if (err != kSuccess)
	{
		tlsEcs.globalRegistry.get_or_emplace<TipInfoMessage>(tlsEcs.GlobalEntity()).set_id(err);
		return;
	}
///<<< END WRITING YOUR CODE

}
