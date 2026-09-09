#pragma once

#include "entt/src/entt/entity/entity.hpp"
#include "proto/common/rollback/player_snapshot.pb.h"

// Kafka topic for player snapshot entries.
//
// 带 topic 世代后缀 `_g<N>`:分区数是 topic 的不可变契约(分区键 = player_id 的按玩家有序
// 全靠它),改分区数不许原地改,只能换一代 —— 新名字、新分区数、消费者(go/data_service)
// 同步切到同一代。世代号与 go/data_service/etc/data_service.yaml 的 Kafka.TopicGeneration 对齐;
// 两边不一致就是生产者写进没人消费的 topic,快照静默丢失。
constexpr char kPlayerSnapshotTopic[] = "player_snapshot_topic_g1";

// Current schema version tag — bump when player_database fields change.
constexpr char kSnapshotSchemaVersion[] = "v1";

// Stateless utility that captures a complete serialized snapshot of a player's
// state and sends it to Kafka for persistence.  The Go DB service writes these
// to the `player_snapshot` MySQL table.
//
// Snapshots are the foundation for:
// - Application-level rollback (GM loads a snapshot → diffs → selective restore)
// - Periodic safety nets (timer-driven captures)
// - Pre-trade / pre-maintenance safeguards
//
// Usage:
//   SnapshotSystem::CaptureAndSend(player, SNAPSHOT_LOGIN);
//   SnapshotSystem::CaptureAndSend(player, SNAPSHOT_GM_MANUAL);
class SnapshotSystem
{
public:
    // Marshal current player state, build a PlayerSnapshotEntry, and push to Kafka.
    // Returns the assigned snapshot_id (from the snapshot id segment), or 0 on failure
    // (including "segment not ready": the snapshot is skipped, fail-closed).
    static uint64_t CaptureAndSend(entt::entity player, SnapshotTrigger trigger);
};
