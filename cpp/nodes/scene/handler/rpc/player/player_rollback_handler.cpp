
#include "player_rollback_handler.h"

///<<< BEGIN WRITING YOUR CODE
#include <muduo/base/Logging.h>

#include "core/utils/registry/game_registry.h"
#include "player_gm_guard.h" // P0-a:GM 客户端指令的 scene 侧第二道锁
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
    // ── GM 闸:本文件**全部十二条**的第一句都是它,读写不分 ──────────────
    //
    // 这十二条挂在 SceneRollbackClientPlayer 下,它**没有**标
    // OptionIsClientProtocolService,所以 gate 的 GM 清单里没有它们 —— gate 也确实
    // 挡得住(只放行 IsClientMessageId 的号)。但 scene 的两个分发入口都不看这个
    // option:ProcessClientPlayerMessage 只按 message_id 查注册表,InvokePlayerService
    // 是节点路由。任何能连上 scene RPC 端口的进程,直接发 message_id 102-117 就调到
    // 这里。等 P1-B 把这些桩填上,那就是"谁都能回档、回收任意玩家"。
    //
    // 分发入口的统一闸门(guild-phase2 §S4 4.34)盖住客户端那条路,这一层盖住节点
    // 路由那条 —— 两条路都要盖,漏一条等于没盖。
    //
    // **读类也拦**:GmQueryDebt / GmTraceItem / GmQueryTransactionLog 这几条会把
    // 指定玩家的欠款、物品流转、交易流水读出来。它们不改数据,但按最小权限,
    // "任意进程可查任意玩家的资产流水"本身就是要堵的面。
    //
    // 判据与另外六条 GM handler 同一个 SCENE_RUN_MODE:默认(未设 / prod)拒绝,
    // 本地联调由启动器显式设 dev。线上改玩家数据走 scene_admin / data_service 那条
    // 带 HMAC 签名与审计日志的运维面(cpp/nodes/gate/SECURITY.md §2/§3)。
    bool RejectGmRollbackRpc(const char* gmRpcName)
    {
        if (!scene_gm_guard::RejectGmClientRpc(gmRpcName))
        {
            return false;
        }
        tlsEcs.globalRegistry
            .get_or_emplace<TipInfoMessage>(tlsEcs.GlobalEntity())
            .set_id(kFeatureUnavailable);
        return true;
    }

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
	if (RejectGmRollbackRpc("GmAttachDebt")) return;
	if (RejectIfFrozen(player, "GmAttachDebt")) return;
	// TODO(P1-B): implement debt-attach logic.
///<<< END WRITING YOUR CODE

}

void SceneRollbackClientPlayerHandler::GmWaiveDebt(entt::entity player,const ::GmWaiveDebtRequest* request,
	::GmWaiveDebtResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// Write-class GM op: removes a debt entry; same Frozen rationale as Attach.
	if (RejectGmRollbackRpc("GmWaiveDebt")) return;
	if (RejectIfFrozen(player, "GmWaiveDebt")) return;
	// TODO(P1-B): implement debt-waive logic.
///<<< END WRITING YOUR CODE

}

void SceneRollbackClientPlayerHandler::GmAdjustDebt(entt::entity player,const ::GmAdjustDebtRequest* request,
	::GmAdjustDebtResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// Write-class GM op: edits an existing debt entry's amount; same Frozen rationale.
	if (RejectGmRollbackRpc("GmAdjustDebt")) return;
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
	if (RejectGmRollbackRpc("GmFreezeDebt")) return;
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
	if (RejectGmRollbackRpc("GmQueryDebt")) return;
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
	if (RejectGmRollbackRpc("GmCreateSnapshot")) return;
	// TODO(P1-B): implement snapshot creation.
///<<< END WRITING YOUR CODE

}

void SceneRollbackClientPlayerHandler::GmListSnapshots(entt::entity player,const ::GmListSnapshotsRequest* request,
	::GmListSnapshotsResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// Read-only: no Frozen guard.
	if (RejectGmRollbackRpc("GmListSnapshots")) return;
	// TODO(P1-B): implement snapshot listing.
///<<< END WRITING YOUR CODE

}

void SceneRollbackClientPlayerHandler::GmPreviewRollback(entt::entity player,const ::GmPreviewRollbackRequest* request,
	::GmPreviewRollbackResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// Read-only "what would happen if we rolled back" simulation. No Frozen guard.
	if (RejectGmRollbackRpc("GmPreviewRollback")) return;
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
	if (RejectGmRollbackRpc("GmExecuteRollback")) return;
	if (RejectIfFrozen(player, "GmExecuteRollback")) return;
	// TODO(P1-B): implement actual rollback execution.
///<<< END WRITING YOUR CODE

}

void SceneRollbackClientPlayerHandler::GmQueryTransactionLog(entt::entity player,const ::GmQueryTransactionLogRequest* request,
	::GmQueryTransactionLogResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// Read-only: forwards to data_service::QueryTransactionLog. No Frozen guard.
	if (RejectGmRollbackRpc("GmQueryTransactionLog")) return;
	// TODO(P1-B): implement RPC forward.
///<<< END WRITING YOUR CODE

}

void SceneRollbackClientPlayerHandler::GmTraceItem(entt::entity player,const ::GmTraceItemRequest* request,
	::GmTraceItemResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// Read-only: walks the transaction-log graph for one item_uuid. No Frozen guard.
	if (RejectGmRollbackRpc("GmTraceItem")) return;
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
	if (RejectGmRollbackRpc("GmClawbackItem")) return;
	if (RejectIfFrozen(player, "GmClawbackItem")) return;
	// TODO(P1-B): implement clawback execution.
///<<< END WRITING YOUR CODE

}
