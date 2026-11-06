#include "anomaly_detector.h"
#include "engine/core/type_define/player_guid_comp.h"

#include <chrono>

#include <muduo/base/Logging.h>

#include "engine/infra/messaging/kafka/kafka_producer.h"
#include "transaction_log_system.h"
#include <ecs_context.h>

// ---------------------------------------------------------------------------
// Static storage (thread-local — each Scene/Gate thread has its own copy)
// ---------------------------------------------------------------------------

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
    const auto *guid = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(player);
    const uint64_t playerId = (guid != nullptr) ? guid->value : 0;

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
    auto* window = WindowForWrite(player, threshold);
    if (window == nullptr)
    {
        return false;
    }

    auto &bucket = window->currencies[static_cast<uint32_t>(type)];
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
    auto* window = WindowForWrite(player, threshold);
    if (window == nullptr)
    {
        return false;
    }

    auto &bucket = window->items[configId];
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

PlayerAnomalyWindowComp* AnomalyDetector::WindowForWrite(entt::entity player, const AnomalyThreshold& threshold)
{
    // 空主体不记窗口或发告警;陈旧句柄更不能 emplace 覆盖复用该槽位的新实体。
    if (!tlsEcs.actorRegistry.valid(player))
    {
        if (player != entt::null)
        {
            LOG_WARN << "AnomalyDetector: stale player entity ignored=" << entt::to_integral(player);
        }
        return nullptr;
    }
    if (threshold.maxCountPerWindow == 0 && threshold.maxAmountPerWindow == 0)
    {
        return nullptr;
    }
    // 此处是异常窗口唯一的延迟创建写入口;只读查询不会使实体参加该功能。
    return &tlsEcs.actorRegistry.get_or_emplace<PlayerAnomalyWindowComp>(player);
}

uint32_t AnomalyDetector::GetCurrencyGainCount(entt::entity player, CurrencyType type)
{
    const auto* window = tlsEcs.actorRegistry.try_get<PlayerAnomalyWindowComp>(player);
    if (window == nullptr)
    {
        return 0;
    }

    auto it = window->currencies.find(static_cast<uint32_t>(type));
    if (it == window->currencies.end())
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
    // valid() 连版本号一起比较:陈旧句柄碰不到复用同一槽位的新玩家,空句柄同样直接返回。
    if (!tlsEcs.actorRegistry.valid(player))
    {
        return;
    }
    // 没有窗口时 remove 返回 0,所以重复调用安全。下一笔收益经 WindowForWrite 重新建窗口。
    tlsEcs.actorRegistry.remove<PlayerAnomalyWindowComp>(player);
}

void AnomalyDetector::ClearAll()
{
    tlsEcs.actorRegistry.clear<PlayerAnomalyWindowComp>();
}
