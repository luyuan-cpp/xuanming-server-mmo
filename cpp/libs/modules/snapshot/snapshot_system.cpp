#include "snapshot_system.h"

#include <muduo/base/Logging.h>

#include "ecs_context.h"
#include "engine/core/type_define/type_define.h"
#include "engine/core/time/system/time.h"
#include "engine/infra/messaging/kafka/kafka_producer.h"
#include "thread_context/snow_flake_manager.h"
#include "services/scene/player/system/player_data_loader.h"
#include "proto/common/database/mysql_database_table.pb.h"
#include "proto/common/database/player_cache.pb.h"

uint64_t SnapshotSystem::CaptureAndSend(entt::entity player, SnapshotTrigger trigger)
{
    if (!tlsEcs.actorRegistry.valid(player))
    {
        LOG_ERROR << "[SnapshotSystem] Invalid player entity";
        return 0;
    }

    const auto *guidPtr = tlsEcs.actorRegistry.try_get<Guid>(player);
    if (guidPtr == nullptr || *guidPtr == kInvalidGuid)
    {
        LOG_ERROR << "[SnapshotSystem] Player entity has no valid Guid";
        return 0;
    }
    const Guid playerId = *guidPtr;

    // ── Marshal player state ─────────────────────────────────────────────
    PlayerAllData allData;
    PlayerAllDataMessageFieldsMarshal(player, allData);
    allData.mutable_player_database_data()->set_player_id(playerId);
    allData.mutable_player_database_1_data()->set_player_id(playerId);

    std::string dbBlob;
    if (!allData.player_database_data().SerializeToString(&dbBlob))
    {
        LOG_ERROR << "[SnapshotSystem] Failed to serialize player_database for player " << playerId;
        return 0;
    }

    std::string db1Blob;
    if (!allData.player_database_1_data().SerializeToString(&db1Blob))
    {
        LOG_ERROR << "[SnapshotSystem] Failed to serialize player_database_1 for player " << playerId;
        return 0;
    }

    // ── Build snapshot entry ─────────────────────────────────────────────
    const uint64_t snapshotId = tlsSnowflakeManager.GenerateItemGuid();
    if (snapshotId == kInvalidGuid)
    {
        // 发号器已被 fence(本节点丢了 node_id 身份)或该线程从未 OnNodeStart。
        // 快照只是回滚兜底,宁可少一条也不能写 snapshot_id=0 —— 那会和别的节点、
        // 别的时刻的 0 号快照互相覆盖,反而毁掉回滚链路。真正的存盘走
        // SavePlayerToRedis,不受影响。
        LOG_ERROR << "[SnapshotSystem] snowflake unavailable (fenced or uninitialized); "
                  << "skipping snapshot for player " << playerId
                  << " trigger=" << static_cast<int>(trigger);
        return 0;
    }
    const uint64_t nowSec = TimeSystem::NowSecondsUTC();

    PlayerSnapshotEntry entry;
    entry.set_snapshot_id(snapshotId);
    entry.set_player_id(playerId);
    entry.set_snapshot_time(nowSec);
    entry.set_trigger(trigger);
    entry.set_player_database_blob(std::move(dbBlob));
    entry.set_player_database_1_blob(std::move(db1Blob));
    entry.set_schema_version(kSnapshotSchemaVersion);

    // ── Serialize and send to Kafka ──────────────────────────────────────
    // Serialize once to get the actual size, set total_bytes, then re-serialize
    entry.set_total_bytes(0);
    std::string bytes;
    if (!entry.SerializeToString(&bytes))
    {
        LOG_ERROR << "[SnapshotSystem] Failed to serialize snapshot for player " << playerId;
        return 0;
    }
    entry.set_total_bytes(bytes.size());
    if (!entry.SerializeToString(&bytes))
    {
        LOG_ERROR << "[SnapshotSystem] Failed to re-serialize snapshot for player " << playerId;
        return 0;
    }

    const std::string key = std::to_string(playerId);
    auto err = KafkaProducer::Instance().send(kPlayerSnapshotTopic, bytes, key);
    if (err != RdKafka::ERR_NO_ERROR)
    {
        LOG_ERROR << "[SnapshotSystem] Kafka send failed for player " << playerId
                  << " snapshot_id=" << snapshotId << " err=" << static_cast<int>(err);
        return 0;
    }

    LOG_INFO << "[SnapshotSystem] Snapshot captured: player=" << playerId
             << " snapshot_id=" << snapshotId
             << " trigger=" << static_cast<int>(trigger)
             << " size=" << bytes.size();

    return snapshotId;
}
