#include "cross_zone_reaper.h"

#include <muduo/base/Logging.h>
#include <muduo/net/EventLoop.h>
#include <muduo/net/TimerId.h>
#include <muduo/contrib/hiredis/Hiredis.h>
#include <hiredis/hiredis.h>

#include <cstdint>
#include <string>

#include "engine/core/error_handling/error_handling.h"
#include "engine/core/time/system/time.h"
#include "engine/core/utils/encode/sha256.h"
#include "engine/infra/messaging/kafka/kafka_producer.h"
#include "network/node_utils.h"
#include "player/comp/player_frozen_comp.h"
#include "player/system/player_data_loader.h"
#include "player/system/player_lifecycle.h"
#include "player/system/player_tip.h"
#include "proto/common/event/player_migration_event.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/cross_server_error_tip.pb.h"
#include "table/proto/tip/scene_error_tip.pb.h"
#include "thread_context/ecs_context.h"
#include "thread_context/redis_manager.h"

namespace
{
    // Single owned timer id for the periodic Tick. We don't need finer
    // granularity than that; the reaper's whole job is "scan once every
    // ~10 seconds", any sharper would just bash Redis.
    muduo::net::TimerId gTickTimerId;
    bool gTickActive = false;
    muduo::net::EventLoop *gOwningLoop = nullptr;

    constexpr char kKeyPrefix[] = "player_migration:";
    constexpr char kFieldFromZone[] = "from_zone";
    constexpr char kFieldToZone[] = "to_zone";
    constexpr char kFieldToNode[] = "to_node_id";
    constexpr char kFieldStartMs[] = "start_ms";
    constexpr char kFieldAttempt[] = "attempt";

    // We keep TTL slightly larger than (kMaxAttempts * kPerAttemptDeadlineMs)
    // so a series of retries can finish before TTL expiry yanks the key out
    // from under us. 60s is comfortable for 3 * 30s = 90s — wait, that's
    // less than the worst case. Bump TTL to 120s.
    constexpr uint32_t kRedisTtlSec = 120;

    std::string MakeKey(Guid playerId)
    {
        return kKeyPrefix + std::to_string(playerId);
    }

    bool RedisReady()
    {
        auto &redis = tlsRedis.GetZoneRedis();
        return redis && redis->connected();
    }
}

void CrossZoneReaper::StartTick(muduo::net::EventLoop *loop)
{
    if (loop == nullptr)
    {
        LOG_ERROR << "[CrossZoneReaper] StartTick called with null loop";
        return;
    }

    if (gTickActive)
    {
        // Idempotent: cancel before re-arming. Avoids double timers if
        // SetAfterStart fires twice (it shouldn't, but harmless).
        loop->cancel(gTickTimerId);
        gTickActive = false;
    }
    gOwningLoop = loop;

    // Run the recovery sweep once *before* arming the periodic tick. This
    // closes the source-restart hole: if this node restarted after
    // publishing player_migrate but before receiving ACK, the local
    // entity is gone (DestroyPlayer never fired, but the entity wasn't
    // saved either — it died with the process), and the Redis record is
    // a stale leak we should clean up.
    ScanAndRecover();

    gTickTimerId = loop->runEvery(kTickIntervalSec, []() {
        // Each tick is a fresh ScanAndRecover — same logic for both
        // restart-time and steady-state recovery: walk Redis keys this
        // zone owns, action stale ones.
        ScanAndRecover();
    });
    gTickActive = true;

    LOG_INFO << "[CrossZoneReaper] tick started, interval=" << kTickIntervalSec << "s, "
             << "per-attempt deadline=" << kPerAttemptDeadlineMs << "ms, "
             << "max attempts=" << kMaxAttempts;
    // Structured metric: reaper lifecycle event. Operators use this
    // to confirm reaper is actually running on the node (vs silently
    // skipped due to misconfig).  cross-zone-readiness-audit.md §8.
    LOG_INFO << "[CrossZoneReaper] metric=reaper_started";
}

void CrossZoneReaper::StopTick()
{
    if (!gTickActive || gOwningLoop == nullptr)
    {
        return;
    }
    gOwningLoop->cancel(gTickTimerId);
    gTickActive = false;
    LOG_INFO << "[CrossZoneReaper] tick stopped";
}

void CrossZoneReaper::RecordMigrationStart(Guid playerId, uint32_t fromZone,
                                            uint32_t toZone, uint32_t toNodeId,
                                            uint32_t attempt)
{
    if (!RedisReady())
    {
        LOG_ERROR << "[CrossZoneReaper] RecordMigrationStart skipped: Redis not connected. "
                  << "player_id=" << playerId << " (reaper recovery for this migration is impaired; "
                  << "if Kafka send also fails, the player will need manual intervention)";
        return;
    }

    // Structured metric line (cross-zone-readiness-audit.md §8 — until the
    // Prometheus C++ side or data_service Kafka consumer lands, the
    // logs themselves are the metrics source. Loki/Promtail/grep can
    // extract counters from these.  metric=migration_start indicates
    // first-attempt publish or a reaper retry — distinguish by attempt.)
    LOG_INFO << "[CrossZoneReaper] metric=migration_start player_id=" << playerId
             << " from_zone=" << fromZone << " to_zone=" << toZone
             << " attempt=" << attempt;

    const std::string key = MakeKey(playerId);
    const int64_t nowMs = TimeSystem::NowMillisecondsUTC();

    auto &redis = tlsRedis.GetZoneRedis();

    // HMSET in one round-trip — five fields, fire-and-forget callback (the
    // values aren't read in the reply path, only the absence of error).
    redis->command(
        [playerId](hiredis::Hiredis *, redisReply *reply) {
            if (reply == nullptr || reply->type == REDIS_REPLY_ERROR)
            {
                LOG_ERROR << "[CrossZoneReaper] HMSET migration record failed for player " << playerId
                          << " (reply=" << (reply ? reply->str : "null") << ")";
            }
        },
        "HMSET %s %s %u %s %u %s %u %s %lld %s %u",
        key.c_str(),
        kFieldFromZone, fromZone,
        kFieldToZone, toZone,
        kFieldToNode, toNodeId,
        kFieldStartMs, static_cast<long long>(nowMs),
        kFieldAttempt, attempt);

    // Separate EXPIRE call — Hiredis SET with EX option is cleaner but
    // HMSET doesn't support EX, and this matches the project's existing
    // SETEX pattern in etcd_manager.cpp:149.
    redis->command(
        [](hiredis::Hiredis *, redisReply *) {},
        "EXPIRE %s %u", key.c_str(), kRedisTtlSec);
}

void CrossZoneReaper::RecordMigrationDone(Guid playerId)
{
    if (!RedisReady())
    {
        // Not fatal — the key will TTL-expire eventually. Log so we know
        // the reaper invariant has a small leak window if Redis was
        // unhealthy at ACK time.
        LOG_WARN << "[CrossZoneReaper] RecordMigrationDone skipped (Redis disconnected) "
                 << "for player " << playerId << "; key will TTL-expire";
        return;
    }

    // Structured metric: successful ACK round-trip. Reaper-retry-success
    // also lands here (since RecordMigrationDone fires from
    // HandlePlayerMigrationAck regardless of which attempt produced the
    // ACK). To split retry-success from first-try-success the LokI query
    // can join against the matching `migration_start` line with the same
    // player_id and check attempt > 1.
    LOG_INFO << "[CrossZoneReaper] metric=migration_done player_id=" << playerId;

    const std::string key = MakeKey(playerId);
    auto &redis = tlsRedis.GetZoneRedis();
    redis->command(
        [](hiredis::Hiredis *, redisReply *) {},
        "DEL %s", key.c_str());
}

namespace
{
    // Republish the original PlayerMigrationEvent for retry. We don't have
    // the original PlayerAllData payload anymore (it was constructed in
    // HandleCrossZoneTransfer's stack frame and discarded after Kafka
    // send), so we MUST rebuild from the live entity. This requires the
    // entity still exists — which it does, because Frozen blocks
    // DestroyPlayer until ACK or reaper-declared-failure.
    //
    // Returns true iff republish succeeded; false signals "give up,
    // declare migration failed".
    bool RepublishMigration(Guid playerId, uint32_t fromZone, uint32_t toZone)
    {
        const auto entity = tlsEcs.GetPlayer(playerId);
        if (!tlsEcs.actorRegistry.valid(entity))
        {
            LOG_WARN << "[CrossZoneReaper] cannot republish: local entity for player " << playerId
                     << " is gone. Probably a source-side crash recovery — declaring failure.";
            return false;
        }

        if (!tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(entity))
        {
            // Defensive: if Frozen is gone something else already cleaned
            // up (a duplicate ACK that arrived between scan and act, or
            // a manual GM action). Drop the retry intent.
            LOG_WARN << "[CrossZoneReaper] entity for player " << playerId
                     << " is no longer Frozen; skipping republish.";
            return false;
        }

        PlayerAllData payload;
        PlayerAllDataMessageFieldsMarshal(entity, payload);
        payload.mutable_player_database_data()->set_player_id(playerId);
        payload.mutable_player_database_1_data()->set_player_id(playerId);

        PlayerMigrationEvent event;
        event.set_player_id(playerId);
        event.set_from_zone(fromZone);
        event.set_to_zone(toZone);
        const std::string serialized = payload.SerializeAsString();
        // Stamp payload hash so the destination's two-tier dedup can
        // recognise this republish as carrying mutated state vs the
        // original send (cross-zone-readiness-audit.md §7 失败 D and
        // PlayerMigrationEvent.payload_sha256 doc comment). Without
        // this, a destination that already received the original
        // publish would re-emit ACK without applying the latest state
        // — the player would land on the destination with the old
        // attempt's HP/buff/inventory.
        event.set_payload_sha256(Sha256::HashToBytes(serialized));
        event.set_serialized_player_data(serialized);

        std::string bytes;
        if (!event.SerializeToString(&bytes))
        {
            LOG_ERROR << "[CrossZoneReaper] failed to re-serialize PlayerMigrationEvent for player "
                      << playerId;
            return false;
        }

        // partition 留空(PARTITION_UA,按 key=playerId 哈希)。zone 不是
        // partition 号 —— 论证见 HandleCrossZoneTransfer 的发布注释。
        const auto err = KafkaProducer::Instance().send(
            "player_migrate", bytes, std::to_string(playerId));
        if (err != RdKafka::ERR_NO_ERROR)
        {
            LOG_ERROR << "[CrossZoneReaper] Kafka republish failed for player " << playerId
                      << ", err=" << RdKafka::err2str(err);
            return false;
        }

        LOG_INFO << "[CrossZoneReaper] republished player_migrate for player " << playerId
                 << " (from_zone=" << fromZone << " → to_zone=" << toZone << ")";
        // Structured metric: every successful republish bumps the retry
        // counter. attempt-1 because the original send was attempt=1; this
        // log fires AFTER the actual Kafka call so it's the count of
        // *additional* attempts beyond the first.
        LOG_INFO << "[CrossZoneReaper] metric=reaper_retry player_id=" << playerId
                 << " from_zone=" << fromZone << " to_zone=" << toZone;
        return true;
    }

    // Process a single stale entry — either republish (with attempt+1) or
    // declare failure (unfreeze + tip + DEL). Called from the SCAN
    // callback for each player_migration:* key whose start_ms is older
    // than the per-attempt deadline.
    void ProcessStaleEntry(Guid playerId, uint32_t fromZone, uint32_t toZone,
                           uint32_t /*toNodeId*/, uint32_t attempt)
    {
        if (attempt < CrossZoneReaper::kMaxAttempts)
        {
            const uint32_t nextAttempt = attempt + 1;
            if (RepublishMigration(playerId, fromZone, toZone))
            {
                // Bump attempt + reset start_ms so the next deadline is
                // measured from the republish, not the original send.
                CrossZoneReaper::RecordMigrationStart(
                    playerId, fromZone, toZone, /*toNodeId=*/0, nextAttempt);
                return;
            }

            // Republish couldn't even build the payload (entity gone) —
            // fall through to declare-failure, same as max-attempts case.
        }

        // Max attempts exhausted (or republish-rebuild failed): declare
        // the migration failed. Unfreeze so the player can keep playing
        // on the source side, send a client tip so the UI knows,
        // delete the Redis record so the reaper stops re-attacking.
        const auto entity = tlsEcs.GetPlayer(playerId);
        if (tlsEcs.actorRegistry.valid(entity) &&
            tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(entity))
        {
            LOG_WARN << "[CrossZoneReaper] declaring migration FAILED for player " << playerId
                     << " (attempt=" << attempt << ", max=" << CrossZoneReaper::kMaxAttempts
                     << "). Unfreezing, sending tip, keeping player on source zone.";
            // Structured metric: terminal failure after retries exhausted.
            // Operators dashboard this counter to know how often the
            // reaper has to give up — non-zero rate is an alert
            // (Kafka broker down / destination zones unhealthy / ...).
            LOG_WARN << "[CrossZoneReaper] metric=reaper_failed player_id=" << playerId
                     << " from_zone=" << fromZone << " to_zone=" << toZone
                     << " attempt=" << attempt;
            tlsEcs.actorRegistry.remove<PlayerFrozenComp>(entity);
            // The generated table currently exposes the generic enter-scene
            // failure tip; use it so the client can dismiss the in-progress
            // overlay and let the player retry the portal manually.
            PlayerTipSystem::SendToPlayer(entity, kEnterSceneFailed, {});
        }

        if (RedisReady())
        {
            const std::string key = MakeKey(playerId);
            tlsRedis.GetZoneRedis()->command(
                [](hiredis::Hiredis *, redisReply *) {},
                "DEL %s", key.c_str());
        }
    }
}

void CrossZoneReaper::ScanAndRecover()
{
    if (!RedisReady())
    {
        // Reaper can't do its job if Redis is unhealthy. Don't log every
        // tick — Redis disconnection produces its own loud warnings.
        return;
    }

    const uint32_t selfZone = GetZoneId();
    const int64_t nowMs = TimeSystem::NowMillisecondsUTC();

    // SCAN cursor=0 with MATCH pattern. For now we do single-shot SCAN
    // (cursor in the callback isn't followed) — at the scale we care
    // about (active migrations are tens of keys, not thousands), one
    // pass with COUNT 200 is fine. If active-migration count grows past
    // a couple hundred we should follow the cursor across multiple
    // tick frames; current shape pays one extra tick of latency per
    // 200 keys, which doesn't hurt anyone.
    auto &redis = tlsRedis.GetZoneRedis();
    redis->command(
        [selfZone, nowMs](hiredis::Hiredis *hi, redisReply *reply) {
            if (reply == nullptr || reply->type != REDIS_REPLY_ARRAY || reply->elements != 2)
            {
                return;
            }
            // reply->element[0] = next cursor (string), [1] = array of keys
            const redisReply *keysArr = reply->element[1];
            if (keysArr == nullptr || keysArr->type != REDIS_REPLY_ARRAY)
            {
                return;
            }
            for (size_t i = 0; i < keysArr->elements; ++i)
            {
                const redisReply *keyEl = keysArr->element[i];
                if (keyEl == nullptr || keyEl->type != REDIS_REPLY_STRING)
                {
                    continue;
                }
                const std::string key(keyEl->str, keyEl->len);

                // Pull all five fields in one HMGET. Captured selfZone /
                // nowMs let the inner callback decide what to do per-key.
                tlsRedis.GetZoneRedis()->command(
                    [selfZone, nowMs, key](hiredis::Hiredis *, redisReply *r2) {
                        if (r2 == nullptr || r2->type != REDIS_REPLY_ARRAY || r2->elements != 5)
                        {
                            return;
                        }
                        auto readStr = [](const redisReply *e) -> std::string {
                            return (e && e->type == REDIS_REPLY_STRING) ? std::string(e->str, e->len) : std::string{};
                        };
                        const auto fromZoneStr = readStr(r2->element[0]);
                        const auto toZoneStr   = readStr(r2->element[1]);
                        const auto toNodeStr   = readStr(r2->element[2]);
                        const auto startMsStr  = readStr(r2->element[3]);
                        const auto attemptStr  = readStr(r2->element[4]);

                        if (fromZoneStr.empty() || toZoneStr.empty() ||
                            startMsStr.empty() || attemptStr.empty())
                        {
                            // Partial / corrupt record — remove so we don't loop on it.
                            tlsRedis.GetZoneRedis()->command(
                                [](hiredis::Hiredis *, redisReply *) {},
                                "DEL %s", key.c_str());
                            return;
                        }

                        const uint32_t fromZone = static_cast<uint32_t>(std::stoul(fromZoneStr));
                        // Only act on records for migrations *we* originated.
                        // Other zones' records will be reaped by their own
                        // scene nodes' reapers; we just observe and ignore.
                        if (fromZone != selfZone)
                        {
                            return;
                        }

                        const uint32_t toZone   = static_cast<uint32_t>(std::stoul(toZoneStr));
                        const uint32_t toNodeId = toNodeStr.empty()
                                                       ? 0u
                                                       : static_cast<uint32_t>(std::stoul(toNodeStr));
                        const int64_t startMs   = static_cast<int64_t>(std::stoll(startMsStr));
                        const uint32_t attempt  = static_cast<uint32_t>(std::stoul(attemptStr));

                        if (nowMs - startMs < CrossZoneReaper::kPerAttemptDeadlineMs)
                        {
                            return; // still within this attempt's deadline
                        }

                        // Extract player_id from key suffix.
                        const auto pos = key.find_last_of(':');
                        if (pos == std::string::npos)
                        {
                            return;
                        }
                        const Guid playerId =
                            static_cast<Guid>(std::stoull(key.substr(pos + 1)));
                        ProcessStaleEntry(playerId, fromZone, toZone, toNodeId, attempt);
                    },
                    "HMGET %s %s %s %s %s %s",
                    key.c_str(),
                    kFieldFromZone, kFieldToZone, kFieldToNode,
                    kFieldStartMs, kFieldAttempt);
            }
        },
        "SCAN 0 MATCH %s* COUNT 200", kKeyPrefix);
}
