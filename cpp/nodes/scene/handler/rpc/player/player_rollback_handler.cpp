
#include "player_rollback_handler.h"

///<<< BEGIN WRITING YOUR CODE
#include <muduo/base/Logging.h>

#include "core/utils/registry/game_registry.h"
#include "services/scene/player/system/player_lifecycle.h" // IsCrossZoneFrozen — cross-zone-readiness-audit.md §11.1
#include "proto/common/component/battle_comp.pb.h"  // InBattleComp:回合制战斗在途拒绝 GM 写操作
#include "table/proto/tip/common_error_tip.pb.h"

// Convenience: stamp kInvalidParameter into the global TipInfoMessage so
// the GM-side caller's reply carries a generic "rejected" code. We use
// kInvalidParameter rather than minting a kPlayerCrossZoneFrozen tip
// because (a) GM tooling rarely surfaces tip strings — operators read
// the LOG_WARN line — and (b) introducing a tip code touches the
// generated tip-table proto, disproportionate for a guard rail.
//
// The GM business logic in each Gm* handler below is currently empty
// (auto-generated stubs); these guards are placed up-front so that
// whoever fills the TODO blocks already lands inside Frozen-protected
// code without having to remember §11.1 themselves.
namespace
{
    bool RejectIfFrozen(entt::entity player, const char* gmRpcName)
    {
        if (PlayerLifecycleSystem::IsCrossZoneFrozen(player))
        {
            LOG_WARN << gmRpcName << " rejected: player frozen for cross-zone migration. "
                     << "entity=" << entt::to_integral(player);
            tlsEcs.globalRegistry
                .get_or_emplace<TipInfoMessage>(tlsEcs.GlobalEntity())
                .set_id(kInvalidParameter);
            return true;
        }
        // 回合制战斗在途同样拒绝:GM 回档 / 回收会改背包与属性,而战斗快照已经发出去了。
        // 此刻扣掉玩家的药,结算按账本扣除时就会"不足按 0",那几瓶药等于白喝;
        // 回档改属性则会被结算的 HP/MP 终值整体覆盖。
        // 这些入口今天还是 TODO 桩,闸先立在这里,免得实现 P1-B 时漏掉。
        if (tlsEcs.actorRegistry.any_of<InBattleComp>(player))
        {
            LOG_WARN << gmRpcName << " rejected: player is in a turn battle. "
                     << "entity=" << entt::to_integral(player);
            tlsEcs.globalRegistry
                .get_or_emplace<TipInfoMessage>(tlsEcs.GlobalEntity())
                .set_id(kInvalidParameter);
            return true;
        }
        return false;
    }
}
///<<< END WRITING YOUR CODE

void SceneRollbackClientPlayerHandler::GmAttachDebt(entt::entity player,const ::GmAttachDebtRequest* request,
	::GmAttachDebtResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// Write-class GM op: attaches a debt entry to the player's debt list,
	// which the destination zone's snapshot already carries. Reject during
	// migration; ops can re-attach after migration completes.
	if (RejectIfFrozen(player, "GmAttachDebt")) return;
	// TODO(P1-B): implement debt-attach logic.
///<<< END WRITING YOUR CODE

}

void SceneRollbackClientPlayerHandler::GmWaiveDebt(entt::entity player,const ::GmWaiveDebtRequest* request,
	::GmWaiveDebtResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// Write-class GM op: removes a debt entry; same Frozen rationale as Attach.
	if (RejectIfFrozen(player, "GmWaiveDebt")) return;
	// TODO(P1-B): implement debt-waive logic.
///<<< END WRITING YOUR CODE

}

void SceneRollbackClientPlayerHandler::GmAdjustDebt(entt::entity player,const ::GmAdjustDebtRequest* request,
	::GmAdjustDebtResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// Write-class GM op: edits an existing debt entry's amount; same Frozen rationale.
	if (RejectIfFrozen(player, "GmAdjustDebt")) return;
	// TODO(P1-B): implement debt-adjust logic.
///<<< END WRITING YOUR CODE

}

void SceneRollbackClientPlayerHandler::GmFreezeDebt(entt::entity player,const ::GmFreezeDebtRequest* request,
	::GmFreezeDebtResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// Write-class GM op: toggles the deferred-clawback "frozen" flag on a
	// debt; same Frozen rationale as Attach.
	if (RejectIfFrozen(player, "GmFreezeDebt")) return;
	// TODO(P1-B): implement debt-freeze logic.
///<<< END WRITING YOUR CODE

}

void SceneRollbackClientPlayerHandler::GmQueryDebt(entt::entity player,const ::GmQueryDebtRequest* request,
	::GmQueryDebtResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// Read-only: no Frozen guard. Querying during migration returns the
	// source-side snapshot which is by definition the same state the
	// destination will load (we marshalled before freezing) — safe.
	// TODO(P1-B): implement debt-query.
///<<< END WRITING YOUR CODE

}

void SceneRollbackClientPlayerHandler::GmCreateSnapshot(entt::entity player,const ::GmCreateSnapshotRequest* request,
	::GmCreateSnapshotResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// Write-class but Frozen-EXCEPTED: snapshots are point-in-time captures
	// of state the destination is about to load anyway, so creating one on
	// the source side at Frozen time is semantically fine — and arguably
	// useful for forensics. No Frozen guard.
	// TODO(P1-B): implement snapshot creation.
///<<< END WRITING YOUR CODE

}

void SceneRollbackClientPlayerHandler::GmListSnapshots(entt::entity player,const ::GmListSnapshotsRequest* request,
	::GmListSnapshotsResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// Read-only: no Frozen guard.
	// TODO(P1-B): implement snapshot listing.
///<<< END WRITING YOUR CODE

}

void SceneRollbackClientPlayerHandler::GmPreviewRollback(entt::entity player,const ::GmPreviewRollbackRequest* request,
	::GmPreviewRollbackResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// Read-only "what would happen if we rolled back" simulation. No Frozen guard.
	// TODO(P1-B): implement preview.
///<<< END WRITING YOUR CODE

}

void SceneRollbackClientPlayerHandler::GmExecuteRollback(entt::entity player,const ::GmExecuteRollbackRequest* request,
	::GmExecuteRollbackResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// Write-class — actually rewrites player state from a snapshot. Reject
	// during migration: the destination zone is about to spawn the player
	// from a marshalled blob; rolling back the source would diverge from
	// what the destination loads. Operators should wait for the migration
	// to settle (or fail) and rollback on the post-migration zone.
	if (RejectIfFrozen(player, "GmExecuteRollback")) return;
	// TODO(P1-B): implement actual rollback execution.
///<<< END WRITING YOUR CODE

}

void SceneRollbackClientPlayerHandler::GmQueryTransactionLog(entt::entity player,const ::GmQueryTransactionLogRequest* request,
	::GmQueryTransactionLogResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// Read-only: forwards to data_service::QueryTransactionLog. No Frozen guard.
	// TODO(P1-B): implement RPC forward.
///<<< END WRITING YOUR CODE

}

void SceneRollbackClientPlayerHandler::GmTraceItem(entt::entity player,const ::GmTraceItemRequest* request,
	::GmTraceItemResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// Read-only: walks the transaction-log graph for one item_uuid. No Frozen guard.
	// TODO(P1-B): implement item trace.
///<<< END WRITING YOUR CODE

}

void SceneRollbackClientPlayerHandler::GmClawbackItem(entt::entity player,const ::GmClawbackItemRequest* request,
	::GmClawbackItemResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// Write-class — removes an exploit-spawned item from the player's bag
	// (or attaches debt if the item was already consumed). Reject during
	// migration; same rationale as GmExecuteRollback.
	if (RejectIfFrozen(player, "GmClawbackItem")) return;
	// TODO(P1-B): implement clawback execution.
///<<< END WRITING YOUR CODE

}
