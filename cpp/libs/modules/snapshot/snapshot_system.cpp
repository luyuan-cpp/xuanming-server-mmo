#include "snapshot_system.h"
#include "audit_snapshot_policy.h"
#include "engine/core/type_define/player_guid_comp.h"

#include <utility>

#include <muduo/base/Logging.h>

#include "ecs_context.h"
#include "engine/core/type_define/type_define.h"
#include "engine/core/time/system/time.h"
#include "modules/audit/audit_outbox.h"
#include "modules/audit/audit_outbox_stats.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "node_config_manager.h"

uint64_t SnapshotSystem::RequestAuditSnapshot(entt::entity player, SnapshotTrigger trigger)
{
    if (!tlsEcs.actorRegistry.valid(player))
    {
        LOG_ERROR << "[SnapshotSystem] Invalid player entity";
        return 0;
    }

    const auto *guidPtr = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(player);
    if (guidPtr == nullptr || guidPtr->value == kInvalidGuid)
    {
        LOG_ERROR << "[SnapshotSystem] Player entity has no valid Guid";
        return 0;
    }
    const Guid playerId = guidPtr->value;
    if (!audit_snapshot_policy::ShouldCapture(tlsNodeConfigManager.GetGameConfig().audit_snapshot(), trigger, playerId))
    {
        return 0;
    }

    // 快照只能随存盘提交:没开审计发件箱的进程没有存盘,快照没有落库的地方。放在领号之前,不消耗号段。
    if (!AuditOutbox::Enabled())
    {
        audit_outbox_stats::IncDropped(audit_outbox_stats::DropReason::kOutboxDisabled);
        LOG_ERROR << "[SnapshotSystem] Audit outbox is not enabled in this process; skipping snapshot for player " << playerId
                  << " trigger=" << static_cast<int>(trigger) << " (metric=snapshot_refused_outbox_disabled)";
        return 0;
    }

    // snapshot_id 走 snapshot 种类的号段(docs/design/node-id-overhaul-plan-20260908.md §7.5 第 1 条):
    // 它只是 player_snapshot 表的主键 / 去重键,不需要时间序 —— 时间在 snapshot_time 列里。
    Guid snapshotId = kInvalidGuid;
    if (!tlsGuidSegmentRegistry.Get(GuidKind::kSnapshot).TryNext(snapshotId))
    {
        // snapshot 号段没号(种类未启用、两段耗尽且 data_service 还没把续段送回来)。
        // 快照只是回滚兜底,宁可少一条也不能写 snapshot_id=0 —— 那会和别的节点、
        // 别的时刻的 0 号快照互相覆盖,反而毁掉回滚链路。真正的存盘不受影响。没有 snowflake 回退,fail-closed。
        audit_outbox_stats::IncDropped(audit_outbox_stats::DropReason::kNoId);
        LOG_ERROR << "[SnapshotSystem] snapshot id segment not ready; "
                  << "skipping snapshot for player " << playerId
                  << " trigger=" << static_cast<int>(trigger) << " "
                  << tlsGuidSegmentRegistry.Get(GuidKind::kSnapshot).Describe() << " (metric=snapshot_refused_no_id)";
        return 0;
    }

    // 快照请求:整档、snapshot_time、total_bytes 由 go/db 在提交事务里按提交后的行填(docs/design/player-audit-outbox.md §6.2)。
    // 这里的 snapshot_time 只是请求时刻,go/db 会覆盖成提交时刻;player_id 照填,go/db 按存盘的玩家号核对后覆盖。
    PlayerSnapshotEntry request;
    request.set_snapshot_id(snapshotId);
    request.set_player_id(playerId);
    request.set_snapshot_time(TimeSystem::NowSecondsUTC());
    request.set_trigger(trigger);
    request.set_schema_version(kSnapshotSchemaVersion);
    // 请求时刻的 zone 由生产者盖章,消费者不许回查 Router(合服后 home_zone 会变)。
    // 取 GameConfig.zone_id 而非 GetZoneId():两者同源(node.cpp 用它填 NodeInfo.zone_id),
    // 但 modules 工程的包含路径里没有 engine/core,引不到 network/node_utils.h。
    request.set_zone_id(tlsNodeConfigManager.GetGameConfig().zone_id());

    if (!AuditOutbox::EnqueueSnapshot(playerId, std::move(request)))
    {
        LOG_ERROR << "[SnapshotSystem] Audit outbox refused snapshot_id=" << snapshotId << " of player " << playerId
                  << " trigger=" << static_cast<int>(trigger);
        return 0;
    }

    LOG_INFO << "[SnapshotSystem] Snapshot requested: player=" << playerId
             << " snapshot_id=" << snapshotId
             << " trigger=" << static_cast<int>(trigger);
    return snapshotId;
}
