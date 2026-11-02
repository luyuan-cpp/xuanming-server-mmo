
#include "player_rollback_handler.h"

///<<< BEGIN WRITING YOUR CODE
#include <muduo/base/Logging.h>

#include "core/utils/registry/game_registry.h"
#include "player_gm_guard.h" // P0-a:GM 客户端指令的 scene 侧第二道锁
#include "services/scene/player/system/player_lifecycle.h" // IsCrossZoneFrozen — cross-zone-readiness-audit.md §11.1
#include "proto/common/component/battle_comp.pb.h"  // InBattleComp:回合制战斗在途拒绝 GM 写操作
#include "table/proto/tip/common_error_tip.pb.h"

// 拒绝直接写本次应答;错误码规则见 services/player/rpc/player_service_interface.h。
// GM 工具沿用 kInvalidParameter 表示冻结或战斗在途拒绝,诊断详情保留在 LOG_WARN。
// 这十二条没有实现,也不打算在这里实现:欠款走 GmSanctionInboxExecutor,回收走 data_service.BatchRecallItems,
// 流水查询走 data_service.QueryTransactionLog,回档已退役(single_player_rollback.md)。放行之后也回 kFeatureUnavailable,
// 不能回空应答让调用方以为做成了。proto 里的这个服务等下一轮协议生成时删掉(docs/handoff/player-save-direct-commit-20261030/progress.md §16)。
namespace
{
    // ── GM 闸:本文件**全部十二条**的第一句都是它,读写不分 ──────────────
    //
    // 这十二条挂在 SceneRollbackClientPlayer 下,它**没有**标
    // OptionIsClientProtocolService,所以 gate 的 GM 清单里没有它们 —— gate 也确实
    // 挡得住(只放行 IsClientMessageId 的号)。但 scene 的分发入口不看这个
    // option:ProcessClientPlayerMessage 只按 message_id 查注册表。任何能连上 scene
    // RPC 端口的进程,直接发 message_id 102-117 就调到这里。等 P1-B 把这些桩填上,
    // 那就是"谁都能回档、回收任意玩家"。
    //
    // 分发入口的统一闸门(guild-phase2 §S4 4.34)盖住客户端那条路。原先的节点路由入口
    // InvokePlayerService 也能打到这里,那条路只有这一层盖得住;它已于批 7 退役并从 proto 删除
    // (docs/design/muduo-rpc-channel.md §20),这一层仍保留作纵深防御 —— 将来若再有
    // 路径能打到玩家 handler,每条路都要盖,漏一条等于没盖。
    //
    // **读类也拦**:GmQueryDebt / GmTraceItem / GmQueryTransactionLog 这几条会把
    // 指定玩家的欠款、物品流转、交易流水读出来。它们不改数据,但按最小权限,
    // "任意进程可查任意玩家的资产流水"本身就是要堵的面。
    //
    // 判据与另外六条 GM handler 同一个 SCENE_RUN_MODE:默认(未设 / prod)拒绝,
    // 本地联调由启动器显式设 dev。线上改玩家数据走 scene_admin / data_service 那条
    // 带 HMAC 签名与审计日志的运维面(cpp/nodes/gate/SECURITY.md §2/§3)。
    template <typename Response>
    bool RejectGmRollbackRpc(const char* gmRpcName, Response& response)
    {
        if (!scene_gm_guard::RejectGmClientRpc(gmRpcName))
        {
            return false;
        }
        response.mutable_error_message()->set_id(kFeatureUnavailable);
        return true;
    }

    template <typename Response>
    bool RejectIfFrozen(entt::entity player, const char* gmRpcName, Response& response)
    {
        if (PlayerLifecycleSystem::IsCrossZoneFrozen(player))
        {
            LOG_WARN << gmRpcName << " rejected: player frozen for cross-zone migration. "
                     << "entity=" << entt::to_integral(player);
            response.mutable_error_message()->set_id(kInvalidParameter);
            return true;
        }
        // 回合制战斗在途同样拒绝:GM 回档 / 回收会改背包与属性,而战斗快照已经发出去了。
        // 此刻扣掉玩家的药,结算按账本扣除时就会"不足按 0",那几瓶药等于白喝;
        // 回档改属性则会被结算的 HP/MP 终值整体覆盖。
        if (tlsEcs.actorRegistry.any_of<InBattleComp>(player))
        {
            LOG_WARN << gmRpcName << " rejected: player is in a turn battle. "
                     << "entity=" << entt::to_integral(player);
            response.mutable_error_message()->set_id(kInvalidParameter);
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
	if (RejectGmRollbackRpc("GmAttachDebt", *response)) return;
	if (RejectIfFrozen(player, "GmAttachDebt", *response)) return;
	response->mutable_error_message()->set_id(kFeatureUnavailable);
///<<< END WRITING YOUR CODE

}

void SceneRollbackClientPlayerHandler::GmWaiveDebt(entt::entity player,const ::GmWaiveDebtRequest* request,
	::GmWaiveDebtResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// Write-class GM op: removes a debt entry; same Frozen rationale as Attach.
	if (RejectGmRollbackRpc("GmWaiveDebt", *response)) return;
	if (RejectIfFrozen(player, "GmWaiveDebt", *response)) return;
	response->mutable_error_message()->set_id(kFeatureUnavailable);
///<<< END WRITING YOUR CODE

}

void SceneRollbackClientPlayerHandler::GmAdjustDebt(entt::entity player,const ::GmAdjustDebtRequest* request,
	::GmAdjustDebtResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// Write-class GM op: edits an existing debt entry's amount; same Frozen rationale.
	if (RejectGmRollbackRpc("GmAdjustDebt", *response)) return;
	if (RejectIfFrozen(player, "GmAdjustDebt", *response)) return;
	response->mutable_error_message()->set_id(kFeatureUnavailable);
///<<< END WRITING YOUR CODE

}

void SceneRollbackClientPlayerHandler::GmFreezeDebt(entt::entity player,const ::GmFreezeDebtRequest* request,
	::GmFreezeDebtResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// Write-class GM op: toggles the deferred-clawback "frozen" flag on a
	// debt; same Frozen rationale as Attach.
	if (RejectGmRollbackRpc("GmFreezeDebt", *response)) return;
	if (RejectIfFrozen(player, "GmFreezeDebt", *response)) return;
	response->mutable_error_message()->set_id(kFeatureUnavailable);
///<<< END WRITING YOUR CODE

}

void SceneRollbackClientPlayerHandler::GmQueryDebt(entt::entity player,const ::GmQueryDebtRequest* request,
	::GmQueryDebtResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// Read-only: no Frozen guard. Querying during migration returns the
	// source-side snapshot which is by definition the same state the
	// destination will load (we marshalled before freezing) — safe.
	if (RejectGmRollbackRpc("GmQueryDebt", *response)) return;
	response->mutable_error_message()->set_id(kFeatureUnavailable);
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
	if (RejectGmRollbackRpc("GmCreateSnapshot", *response)) return;
	response->mutable_error_message()->set_id(kFeatureUnavailable);
///<<< END WRITING YOUR CODE

}

void SceneRollbackClientPlayerHandler::GmListSnapshots(entt::entity player,const ::GmListSnapshotsRequest* request,
	::GmListSnapshotsResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// Read-only: no Frozen guard.
	if (RejectGmRollbackRpc("GmListSnapshots", *response)) return;
	response->mutable_error_message()->set_id(kFeatureUnavailable);
///<<< END WRITING YOUR CODE

}

void SceneRollbackClientPlayerHandler::GmPreviewRollback(entt::entity player,const ::GmPreviewRollbackRequest* request,
	::GmPreviewRollbackResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// Read-only "what would happen if we rolled back" simulation. No Frozen guard.
	if (RejectGmRollbackRpc("GmPreviewRollback", *response)) return;
	response->mutable_error_message()->set_id(kFeatureUnavailable);
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
	if (RejectGmRollbackRpc("GmExecuteRollback", *response)) return;
	if (RejectIfFrozen(player, "GmExecuteRollback", *response)) return;
	response->mutable_error_message()->set_id(kFeatureUnavailable);
///<<< END WRITING YOUR CODE

}

void SceneRollbackClientPlayerHandler::GmQueryTransactionLog(entt::entity player,const ::GmQueryTransactionLogRequest* request,
	::GmQueryTransactionLogResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// Read-only: forwards to data_service::QueryTransactionLog. No Frozen guard.
	if (RejectGmRollbackRpc("GmQueryTransactionLog", *response)) return;
	response->mutable_error_message()->set_id(kFeatureUnavailable);
///<<< END WRITING YOUR CODE

}

void SceneRollbackClientPlayerHandler::GmTraceItem(entt::entity player,const ::GmTraceItemRequest* request,
	::GmTraceItemResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// Read-only: walks the transaction-log graph for one item_uuid. No Frozen guard.
	if (RejectGmRollbackRpc("GmTraceItem", *response)) return;
	response->mutable_error_message()->set_id(kFeatureUnavailable);
///<<< END WRITING YOUR CODE

}

void SceneRollbackClientPlayerHandler::GmClawbackItem(entt::entity player,const ::GmClawbackItemRequest* request,
	::GmClawbackItemResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// Write-class — removes an exploit-spawned item from the player's bag
	// (or attaches debt if the item was already consumed). Reject during
	// migration; same rationale as GmExecuteRollback.
	if (RejectGmRollbackRpc("GmClawbackItem", *response)) return;
	if (RejectIfFrozen(player, "GmClawbackItem", *response)) return;
	response->mutable_error_message()->set_id(kFeatureUnavailable);
///<<< END WRITING YOUR CODE

}
