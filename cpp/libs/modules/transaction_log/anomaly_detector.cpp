#include "anomaly_detector.h"

#include <chrono>

#include <muduo/base/Logging.h>

#include "engine/infra/messaging/kafka/kafka_producer.h"
#include "transaction_log_system.h"
#include <ecs_context.h>

// ---------------------------------------------------------------------------
// Static storage (thread-local — each Scene/Gate thread has its own copy)
// ---------------------------------------------------------------------------

thread_local std::unordered_map<entt::entity, AnomalyDetector::PlayerBuckets,
                                AnomalyDetector::EntityHash>
    AnomalyDetector::buckets_;

thread_local std::unordered_map<uint32_t, AnomalyThreshold>
    AnomalyDetector::currencyThresholds_;

thread_local std::unordered_map<uint32_t, AnomalyThreshold>
    AnomalyDetector::itemThresholds_;

thread_local AnomalyThreshold AnomalyDetector::defaultThreshold_{};

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

static uint64_t NowSeconds()
{
    return static_cast<uint64_t>(
        std::chrono::duration_cast<std::chrono::seconds>(
            std::chrono::system_clock::now().time_since_epoch())
            .count());
}

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

void AnomalyDetector::SetCurrencyThreshold(CurrencyType type, const AnomalyThreshold &threshold)
{
    currencyThresholds_[static_cast<uint32_t>(type)] = threshold;
}

void AnomalyDetector::SetItemThreshold(uint32_t configId, const AnomalyThreshold &threshold)
{
    itemThresholds_[configId] = threshold;
}

void AnomalyDetector::SetDefaultThreshold(const AnomalyThreshold &threshold)
{
    defaultThreshold_ = threshold;
}

const AnomalyThreshold &AnomalyDetector::GetCurrencyThreshold(CurrencyType type)
{
    auto it = currencyThresholds_.find(static_cast<uint32_t>(type));
    return it != currencyThresholds_.end() ? it->second : defaultThreshold_;
}

const AnomalyThreshold &AnomalyDetector::GetItemThreshold(uint32_t configId)
{
    auto it = itemThresholds_.find(configId);
    return it != itemThresholds_.end() ? it->second : defaultThreshold_;
}

// ---------------------------------------------------------------------------
// Core: RecordAndCheck — sliding-window prune + threshold check
// ---------------------------------------------------------------------------

bool AnomalyDetector::RecordAndCheck(PlayerAnomalyBucket &bucket, uint64_t amount,
                                     const AnomalyThreshold &threshold)
{
    const uint64_t now = NowSeconds();
    const uint64_t windowStart = (now > threshold.windowSeconds) ? (now - threshold.windowSeconds) : 0;

    // Prune expired events
    while (!bucket.events.empty() && bucket.events.front().timestamp < windowStart)
    {
        bucket.cumulativeAmount -= bucket.events.front().amount;
        bucket.events.pop_front();
    }

    // Record new event
    bucket.events.push_back({now, amount});
    bucket.cumulativeAmount += amount;

    // Check thresholds
    const bool countExceeded = bucket.events.size() > threshold.maxCountPerWindow;
    const bool amountExceeded = bucket.cumulativeAmount > threshold.maxAmountPerWindow;

    return countExceeded || amountExceeded;
}

// ---------------------------------------------------------------------------
// EmitAlert — log + send Kafka alert
// ---------------------------------------------------------------------------

constexpr char kAnomalyAlertTopic[] = "anomaly_alert_topic";

void AnomalyDetector::EmitAlert(entt::entity player, const std::string &category,
                                uint32_t subKey, uint64_t count, uint64_t totalAmount,
                                const AnomalyThreshold &threshold)
{
    const auto *guid = tlsEcs.actorRegistry.try_get<Guid>(player);
    const uint64_t playerId = (guid != nullptr) ? *guid : 0;

    LOG_WARN << "AnomalyDetector: ALERT player=" << playerId
             << " category=" << category << " subKey=" << subKey
             << " count=" << count << " totalAmount=" << totalAmount
             << " thresholdCount=" << threshold.maxCountPerWindow
             << " thresholdAmount=" << threshold.maxAmountPerWindow
             << " windowSec=" << threshold.windowSeconds;

    // Build a JSON alert and push to Kafka for the Go anomaly service.
    std::string alertJson = "{\"player_id\":" + std::to_string(playerId) +
                            ",\"category\":\"" + category + "\"" +
                            ",\"sub_key\":" + std::to_string(subKey) +
                            ",\"count\":" + std::to_string(count) +
                            ",\"total_amount\":" + std::to_string(totalAmount) +
                            ",\"threshold_count\":" + std::to_string(threshold.maxCountPerWindow) +
                            ",\"threshold_amount\":" + std::to_string(threshold.maxAmountPerWindow) +
                            ",\"window_seconds\":" + std::to_string(threshold.windowSeconds) +
                            ",\"timestamp\":" + std::to_string(NowSeconds()) + "}";

    const std::string key = std::to_string(playerId);
    auto err = KafkaProducer::Instance().send(kAnomalyAlertTopic, alertJson, key);
    if (err != RdKafka::ERR_NO_ERROR)
    {
        LOG_ERROR << "AnomalyDetector: Kafka send failed for player=" << playerId
                  << " err=" << static_cast<int>(err);
    }
}

// ---------------------------------------------------------------------------
// Public API: RecordCurrencyGain / RecordItemGain
// ---------------------------------------------------------------------------

bool AnomalyDetector::RecordCurrencyGain(entt::entity player, CurrencyType type, uint64_t amount)
{
    const auto &threshold = GetCurrencyThreshold(type);
    if (threshold.maxCountPerWindow == 0 && threshold.maxAmountPerWindow == 0)
    {
        return false; // anomaly detection disabled for this type
    }

    auto &bucket = buckets_[player].currencies[static_cast<uint32_t>(type)];
    bool triggered = RecordAndCheck(bucket, amount, threshold);

    if (triggered)
    {
        EmitAlert(player, "currency", static_cast<uint32_t>(type),
                  static_cast<uint64_t>(bucket.events.size()), bucket.cumulativeAmount,
                  threshold);
    }
    return triggered;
}

bool AnomalyDetector::RecordItemGain(entt::entity player, uint32_t configId, uint32_t quantity)
{
    const auto &threshold = GetItemThreshold(configId);
    if (threshold.maxCountPerWindow == 0 && threshold.maxAmountPerWindow == 0)
    {
        return false;
    }

    auto &bucket = buckets_[player].items[configId];
    bool triggered = RecordAndCheck(bucket, static_cast<uint64_t>(quantity), threshold);

    if (triggered)
    {
        EmitAlert(player, "item", configId,
                  static_cast<uint64_t>(bucket.events.size()), bucket.cumulativeAmount,
                  threshold);
    }
    return triggered;
}

// ---------------------------------------------------------------------------
// Query / Reset
// ---------------------------------------------------------------------------

uint32_t AnomalyDetector::GetCurrencyGainCount(entt::entity player, CurrencyType type)
{
    auto playerIt = buckets_.find(player);
    if (playerIt == buckets_.end())
    {
        return 0;
    }

    auto it = playerIt->second.currencies.find(static_cast<uint32_t>(type));
    if (it == playerIt->second.currencies.end())
    {
        return 0;
    }

    const auto &threshold = GetCurrencyThreshold(type);
    const uint64_t now = NowSeconds();
    const uint64_t windowStart = (now > threshold.windowSeconds) ? (now - threshold.windowSeconds) : 0;

    // Count only events within the window
    uint32_t count = 0;
    for (const auto &e : it->second.events)
    {
        if (e.timestamp >= windowStart)
        {
            ++count;
        }
    }
    return count;
}

void AnomalyDetector::ClearPlayer(entt::entity player)
{
    buckets_.erase(player);
}

void AnomalyDetector::ClearAll()
{
    buckets_.clear();
}
