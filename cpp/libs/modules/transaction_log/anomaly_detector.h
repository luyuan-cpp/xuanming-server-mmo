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

    // Get the current event count in the window for a player+currency.
    static uint32_t GetCurrencyGainCount(entt::entity player, CurrencyType type);

    // Clear all tracking data (e.g. on server restart or player logout).
    static void ClearPlayer(entt::entity player);
    static void ClearAll();

private:
    // Keep player as the outer key so logout cleanup is O(1), not a full scan
    // of every player's buckets. Currency and item maps are intentionally
    // separate: a currency enum value may equal an item config id.
    struct PlayerBuckets
    {
        std::unordered_map<uint32_t, PlayerAnomalyBucket> currencies;
        std::unordered_map<uint32_t, PlayerAnomalyBucket> items;
    };

    struct EntityHash
    {
        size_t operator()(entt::entity entity) const noexcept
        {
            return std::hash<uint32_t>{}(entt::to_integral(entity));
        }
    };

    static const AnomalyThreshold &GetCurrencyThreshold(CurrencyType type);
    static const AnomalyThreshold &GetItemThreshold(uint32_t configId);

    // Prune events outside the window and return whether threshold exceeded.
    static bool RecordAndCheck(PlayerAnomalyBucket &bucket, uint64_t amount,
                               const AnomalyThreshold &threshold);

    // Emit alert to Kafka for the Go anomaly-query service.
    static void EmitAlert(entt::entity player, const std::string &category,
                          uint32_t subKey, uint64_t count, uint64_t totalAmount,
                          const AnomalyThreshold &threshold);

    // Storage — thread-local via TLS, no mutex needed.
    static thread_local std::unordered_map<entt::entity, PlayerBuckets, EntityHash> buckets_;
    static thread_local std::unordered_map<uint32_t, AnomalyThreshold> currencyThresholds_;
    static thread_local std::unordered_map<uint32_t, AnomalyThreshold> itemThresholds_;
    static thread_local AnomalyThreshold defaultThreshold_;
};
