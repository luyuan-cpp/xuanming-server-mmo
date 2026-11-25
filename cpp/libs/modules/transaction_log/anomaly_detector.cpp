#include "anomaly_detector.h"
#include "engine/core/type_define/player_guid_comp.h"

#include <chrono>
#include <limits>

#include <muduo/base/Logging.h>

#include "engine/infra/messaging/kafka/kafka_producer.h"
#include "transaction_log_system.h"
#include <ecs_context.h>

// ---------------------------------------------------------------------------
// Static storage (thread-local — each Scene/Gate thread has its own copy)
// ---------------------------------------------------------------------------

thread_local std::unordered_map<uint32_t, AnomalyThreshold>
    AnomalyDetector::currencyThresholds;

thread_local std::unordered_map<uint32_t, AnomalyThreshold>
    AnomalyDetector::itemThresholds;

thread_local AnomalyThreshold AnomalyDetector::defaultThreshold{};

namespace
{
thread_local AnomalyDetector::SteadyNowFn tlsSteadyNowForTest = nullptr;

// 滑动窗口与告警冷却统一使用单调时钟(AGENTS.md §11.3),不受 NTP/墙钟回拨影响。
uint64_t SteadyNowSeconds()
{
    if (tlsSteadyNowForTest != nullptr)
    {
        return tlsSteadyNowForTest();
    }
    const auto elapsed = std::chrono::duration_cast<std::chrono::seconds>(
        std::chrono::steady_clock::now().time_since_epoch());
    return elapsed.count() > 0 ? static_cast<uint64_t>(elapsed.count()) : 0;
}

// 仅用于告警 JSON 的对外可读时间戳。
uint64_t WallNowSeconds()
{
    const auto elapsed = std::chrono::duration_cast<std::chrono::seconds>(
        std::chrono::system_clock::now().time_since_epoch());
    return elapsed.count() > 0 ? static_cast<uint64_t>(elapsed.count()) : 0;
}

uint64_t SaturatingAdd(uint64_t lhs, uint64_t rhs)
{
    constexpr uint64_t kMax = (std::numeric_limits<uint64_t>::max)();
    return rhs > kMax - lhs ? kMax : lhs + rhs;
}

uint64_t RecomputeCumulativeAmount(const std::deque<AnomalyEvent> &events)
{
    uint64_t sum = 0;
    for (const auto &ev : events)
    {
        sum = SaturatingAdd(sum, ev.amount);
    }
    return sum;
}

bool IsBucketInCooldown(const PlayerAnomalyBucket &bucket, uint64_t now, uint32_t windowSeconds)
{
    return bucket.hasAlerted && now >= bucket.lastAlertAt && (now - bucket.lastAlertAt) < windowSeconds;
}
} // namespace

void AnomalyDetector::SetSteadyNowForTest(SteadyNowFn fn)
{
    tlsSteadyNowForTest = fn;
}

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

void AnomalyDetector::SetCurrencyThreshold(CurrencyType type, const AnomalyThreshold &threshold)
{
    currencyThresholds[static_cast<uint32_t>(type)] = threshold;
}

void AnomalyDetector::SetItemThreshold(uint32_t configId, const AnomalyThreshold &threshold)
{
    itemThresholds[configId] = threshold;
}

void AnomalyDetector::SetDefaultThreshold(const AnomalyThreshold &threshold)
{
    defaultThreshold = threshold;
}

const AnomalyThreshold &AnomalyDetector::GetCurrencyThreshold(CurrencyType type)
{
    auto it = currencyThresholds.find(static_cast<uint32_t>(type));
    return it != currencyThresholds.end() ? it->second : defaultThreshold;
}

const AnomalyThreshold &AnomalyDetector::GetItemThreshold(uint32_t configId)
{
    auto it = itemThresholds.find(configId);
    return it != itemThresholds.end() ? it->second : defaultThreshold;
}

// ---------------------------------------------------------------------------
// Core: RecordAndCheck — sliding-window prune + threshold check
// ---------------------------------------------------------------------------

bool AnomalyDetector::RecordAndCheck(PlayerAnomalyBucket &bucket, uint64_t amount,
                                     const AnomalyThreshold &threshold)
{
    constexpr uint64_t kMax = (std::numeric_limits<uint64_t>::max)();
    const uint64_t now = SteadyNowSeconds();
    const uint64_t windowStart = (now > threshold.windowSeconds) ? (now - threshold.windowSeconds) : 0;

    const bool wasSaturated = (bucket.cumulativeAmount == kMax);
    bool removedAny = false;

    // 清理窗口外过期事件；饱和状态下弹出旧事件后通过剩余事件重算累计值，防止下溢或虚高。
    while (!bucket.events.empty() && bucket.events.front().timestamp < windowStart)
    {
        const uint64_t evicted = bucket.events.front().amount;
        bucket.events.pop_front();
        removedAny = true;
        if (!wasSaturated)
        {
            bucket.cumulativeAmount = bucket.cumulativeAmount >= evicted ? (bucket.cumulativeAmount - evicted) : 0;
        }
    }

    // 限制单桶队列长度，防止高频刷取造成无界内存增长。
    while (bucket.events.size() >= kMaxEventsPerBucket)
    {
        const uint64_t evicted = bucket.events.front().amount;
        bucket.events.pop_front();
        removedAny = true;
        if (!wasSaturated)
        {
            bucket.cumulativeAmount = bucket.cumulativeAmount >= evicted ? (bucket.cumulativeAmount - evicted) : 0;
        }
    }

    if (wasSaturated && removedAny)
    {
        bucket.cumulativeAmount = RecomputeCumulativeAmount(bucket.events);
    }

    // 记录新事件，使用饱和加法防止 uint64_t 回绕绕过金额阈值。
    bucket.events.push_back({now, amount});
    bucket.cumulativeAmount = SaturatingAdd(bucket.cumulativeAmount, amount);

    const bool countExceeded = bucket.events.size() > threshold.maxCountPerWindow;
    const bool amountExceeded = bucket.cumulativeAmount > threshold.maxAmountPerWindow;
    if (!countExceeded && !amountExceeded)
    {
        return false;
    }

    // 每桶在单个窗口期内至多告警一次，避免超限后每笔收益连续刷爆日志与 Kafka。
    if (IsBucketInCooldown(bucket, now, threshold.windowSeconds))
    {
        return false;
    }
    bucket.hasAlerted = true;
    bucket.lastAlertAt = now;
    return true;
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
                            ",\"timestamp\":" + std::to_string(WallNowSeconds()) + "}";

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

    // 定期清理已完全过期且不在冷却期内的物品桶，防止长会话累计大量无用 configId 键。
    const uint64_t now = SteadyNowSeconds();
    if (!window->items.empty() && (!window->hasPrunedItems || now != window->lastItemPruneAt))
    {
        for (auto it = window->items.begin(); it != window->items.end();)
        {
            if (it->first == configId)
            {
                ++it;
                continue;
            }
            const auto &itemThreshold = GetItemThreshold(it->first);
            const uint64_t itemWindowStart = (now > itemThreshold.windowSeconds) ? (now - itemThreshold.windowSeconds) : 0;
            const bool eventsExpired = it->second.events.empty() || it->second.events.back().timestamp < itemWindowStart;
            if (eventsExpired && !IsBucketInCooldown(it->second, now, itemThreshold.windowSeconds))
            {
                it = window->items.erase(it);
            }
            else
            {
                ++it;
            }
        }
        window->hasPrunedItems = true;
        window->lastItemPruneAt = now;
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
    const uint64_t now = SteadyNowSeconds();
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
