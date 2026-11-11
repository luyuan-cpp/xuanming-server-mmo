#pragma once

#include "entt/src/entt/entity/entity.hpp"
#include "proto/common/rollback/player_snapshot.pb.h"

// Current schema version tag — bump when player_database fields change.
constexpr char kSnapshotSchemaVersion[] = "v1";

// 玩家审计快照(回档的依据),随存盘提交(2026-11-11,审计发件箱,docs/design/player-audit-outbox.md §5.5)。
//
// scene 只交一份快照请求(snapshot_id、trigger、schema_version、zone_id),进这名玩家的审计队列(AuditOutbox),
// 随他的下一笔存盘交给 go/db;go/db 在提交这笔存档的同一个 TiDB 事务里,按这一笔写完之后的全部数据行拼出整档,
// 写进本存档库的发件箱,再由转发循环投 player_snapshot topic,data_service 落 player_snapshot 表。所以:
//   - 快照就是库里某一笔提交的原样,不会是从没提交过的内存状态;
//   - Kafka 停多久都不丢;存盘请求也不会因为带整档而变大;
//   - 这名玩家的存盘最终没提交(被废黜、缺持久化组件拒存),快照请求随改动一起作废,不会落成一份零值快照。
//
// Snapshots are the foundation for:
// - Application-level rollback (GM loads a snapshot → diffs → selective restore)
// - Pre-trade / pre-maintenance safeguards
//
// Usage:
//   SnapshotSystem::RequestAuditSnapshot(player, SNAPSHOT_LOGIN);   // 调用方随后要让这名玩家存一次盘
//   SnapshotSystem::RequestAuditSnapshot(player, SNAPSHOT_LOGOUT);  // 随后的最终存盘带走它
class SnapshotSystem
{
public:
    // 核实体与 Guid → 抽样(audit_snapshot_policy.h)→ 领 snapshot_id → 入审计队列。返回 snapshot_id;
    // 没抽中、号段没号、实体不合法、或这个进程没开审计发件箱(没有存盘,快照没有提交的地方)时返回 0。
    // 只读:不改变实体的组件集合,也不序列化存档。快照要等这名玩家下一笔存盘提交才落库。
    static uint64_t RequestAuditSnapshot(entt::entity player, SnapshotTrigger trigger);
};
