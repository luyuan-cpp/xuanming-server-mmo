#pragma once

#include <chrono>
#include <cstdint>
#include <deque>
#include <functional>
#include <string>
#include <unordered_map>

#include "entt/src/entt/entity/entity.hpp"
#include "modules/currency/constants/currency.h"

// ---------------------------------------------------------------------------
// AnomalyDetector — sliding-window rate monitor for currency/item gains.
//
// Tracks per-player, per-currency (or per-item-config) gain events in a
// rolling time window.  When the count or cumulative amount exceeds a
// configured threshold, a warning is logged AND a Kafka alert message is
// emitted so the Go anomaly-query service can surface it.
//
// Designed to be called from CurrencySystem::AddCurrency and Bag::AddItem.
// All methods are thread-local (no mutex needed in single-threaded ECS tick).
// ---------------------------------------------------------------------------

struct AnomalyThreshold
{
    uint32_t maxCountPerWindow{50};      // max discrete gain events
    uint32_t maxAmountPerWindow{100000}; // max cumulative gain amount
    uint32_t windowSeconds{600};         // sliding window size (10 min default)
};

struct AnomalyEvent
{
    uint64_t timestamp;
    uint64_t amount;
};

struct PlayerAnomalyBucket
{
    std::deque<AnomalyEvent> events;
    uint64_t cumulativeAmount{0};
};

// 按需状态,只由 Record* 经 WindowForWrite 首次写入时创建,不入库。
// 缺失表示空窗口;随玩家实体销毁,不保留进程级旁路索引。
struct PlayerAnomalyWindowComp
{
    std::unordered_map<uint32_t, PlayerAnomalyBucket> currencies;
    std::unordered_map<uint32_t, PlayerAnomalyBucket> items;
};

class AnomalyDetector
{
public:
    // ── Configuration ────────────────────────────────────────────────────

    // Set threshold for a specific currency type.
    static void SetCurrencyThreshold(CurrencyType type, const AnomalyThreshold &threshold);

    // Set threshold for item gains (keyed by item_config_id).
    static void SetItemThreshold(uint32_t configId, const AnomalyThreshold &threshold);

    // Set a default threshold used when no per-type/config override exists.
    static void SetDefaultThreshold(const AnomalyThreshold &threshold);

    // ── Recording ────────────────────────────────────────────────────────

    // Record a currency gain event.  Returns true if the event triggered
    // an anomaly alert (threshold exceeded).
    static bool RecordCurrencyGain(entt::entity player, CurrencyType type, uint64_t amount);

    // Record an item gain event.  Returns true if anomaly triggered.
    static bool RecordItemGain(entt::entity player, uint32_t configId, uint32_t quantity);

    // ── Query / Reset ────────────────────────────────────────────────────

    // 只读查询玩家货币窗口计数,不创建窗口;目前仅供测试观测。
    static uint32_t GetCurrencyGainCount(entt::entity player, CurrencyType type);

    // 重置单个玩家的收益统计窗口(货币和物品),须在玩家所属场景线程上调用,
    // 供处置误报等内部流程使用,不对外暴露 RPC。只移除该玩家的窗口组件:
    // 阈值、已发出的告警、流水和其他玩家的窗口都不变,下一笔收益重新开始计数。
    // 空句柄、陈旧句柄和重复调用都是无操作;登出不需要调用,窗口随玩家实体销毁。
    static void ClearPlayer(entt::entity player);

    // 测试隔离用,清空所有玩家窗口但不销毁玩家;登出不需要清理,窗口随玩家实体销毁。
    static void ClearAll();

private:
    static PlayerAnomalyWindowComp* WindowForWrite(entt::entity player, const AnomalyThreshold& threshold);
    static const AnomalyThreshold &GetCurrencyThreshold(CurrencyType type);
    static const AnomalyThreshold &GetItemThreshold(uint32_t configId);

    // Prune events outside the window and return whether threshold exceeded.
    static bool RecordAndCheck(PlayerAnomalyBucket &bucket, uint64_t amount,
                               const AnomalyThreshold &threshold);

    // Emit alert to Kafka for the Go anomaly-query service.
    static void EmitAlert(entt::entity player, const std::string &category,
                          uint32_t subKey, uint64_t count, uint64_t totalAmount,
                          const AnomalyThreshold &threshold);

    // 配置仍归当前线程,窗口数据归玩家实体。
    static thread_local std::unordered_map<uint32_t, AnomalyThreshold> currencyThresholds_;
    static thread_local std::unordered_map<uint32_t, AnomalyThreshold> itemThresholds_;
    static thread_local AnomalyThreshold defaultThreshold_;
};
