#pragma once
#include <algorithm>
#include <cstdint>
#include <functional>
#include <memory>
#include <random>
#include <unordered_map>
#include <vector>
#include "table_expression.h"
#include "muduo/base/Logging.h"
#include "table/proto/attributeautoplan_table.pb.h"

class AttributeAutoPlanTableManager {
public:
    using IdMapType = std::unordered_map<uint32_t, const AttributeAutoPlanTable*>;
    using LoadSuccessCallback = std::function<void()>;

    // Internal snapshot holding all parsed data and indices.
    // Load() builds a new snapshot and swaps it in, replacing the old one.
    struct Snapshot {
        AttributeAutoPlanTableData data;
        IdMapType idMap;
        std::unordered_multimap<uint32_t, const AttributeAutoPlanTable*> dimensionIndex;
        std::unordered_multimap<uint32_t, const AttributeAutoPlanTable*> weightIndex;
        std::unordered_map<uint32_t, std::vector<const AttributeAutoPlanTable*>> classIdIndex;
        std::unordered_map<uint32_t, std::vector<const AttributeAutoPlanTable*>> poolIdIndex;
    };

    static AttributeAutoPlanTableManager& Instance() {
        static AttributeAutoPlanTableManager instance;
        return instance;
    }

    const Snapshot& GetSnapshot() const { return *snapshot; }

    const AttributeAutoPlanTableData& FindAll() const { return snapshot->data; }

    std::pair<const AttributeAutoPlanTable*, uint32_t> FindById(uint32_t tableId);
    std::pair<const AttributeAutoPlanTable*, uint32_t> FindByIdSilent(uint32_t tableId);
    const IdMapType& GetIdMap() const { return snapshot->idMap; }

    void Load();

    void SetLoadSuccessCallback(const LoadSuccessCallback& callback) {
        loadSuccessCallback = callback;
    }

    void LoadSuccess() { if (loadSuccessCallback) { loadSuccessCallback(); } }

    // FK: pool_id -> AttributePool.id
    // FK: dimension -> AttributeDimension.id
    const std::unordered_multimap<uint32_t, const AttributeAutoPlanTable*>& GetDimensionIndex() const { return snapshot->dimensionIndex; }
    const std::unordered_multimap<uint32_t, const AttributeAutoPlanTable*>& GetWeightIndex() const { return snapshot->weightIndex; }
    const std::unordered_map<uint32_t, std::vector<const AttributeAutoPlanTable*>>& GetClassIdIndex() const { return snapshot->classIdIndex; }
    const std::vector<const AttributeAutoPlanTable*>& GetByClassId(uint32_t key) const {
        static const std::vector<const AttributeAutoPlanTable*> kEmpty;
        auto it = snapshot->classIdIndex.find(key);
        return it != snapshot->classIdIndex.end() ? it->second : kEmpty;
    }
    const std::unordered_map<uint32_t, std::vector<const AttributeAutoPlanTable*>>& GetPoolIdIndex() const { return snapshot->poolIdIndex; }
    const std::vector<const AttributeAutoPlanTable*>& GetByPoolId(uint32_t key) const {
        static const std::vector<const AttributeAutoPlanTable*> kEmpty;
        auto it = snapshot->poolIdIndex.find(key);
        return it != snapshot->poolIdIndex.end() ? it->second : kEmpty;
    }

    // ---- Exists ----

    bool Exists(uint32_t id) const { return snapshot->idMap.count(id) > 0; }

    // ---- Count ----

    std::size_t Count() const { return snapshot->idMap.size(); }
    std::size_t CountByDimensionIndex(uint32_t key) const { return snapshot->dimensionIndex.count(key); }
    std::size_t CountByWeightIndex(uint32_t key) const { return snapshot->weightIndex.count(key); }
    std::size_t CountByClassIdIndex(uint32_t key) const {
        auto it = snapshot->classIdIndex.find(key);
        return it != snapshot->classIdIndex.end() ? it->second.size() : 0;
    }
    std::size_t CountByPoolIdIndex(uint32_t key) const {
        auto it = snapshot->poolIdIndex.find(key);
        return it != snapshot->poolIdIndex.end() ? it->second.size() : 0;
    }

    // ---- FindByIds (IN) ----

    std::vector<const AttributeAutoPlanTable*> FindByIds(const std::vector<uint32_t>& ids) const {
        std::vector<const AttributeAutoPlanTable*> result;
        result.reserve(ids.size());
        for (auto id : ids) {
            if (auto it = snapshot->idMap.find(id); it != snapshot->idMap.end()) {
                result.push_back(it->second);
            }
        }
        return result;
    }

    // ---- RandOne ----

    const AttributeAutoPlanTable* RandOne() const {
        if (snapshot->data.data_size() == 0) return nullptr;
        thread_local std::mt19937 rng{std::random_device{}()};
        std::uniform_int_distribution<int> dist(0, snapshot->data.data_size() - 1);
        return &snapshot->data.data(dist(rng));
    }

    // ---- Where / First ----

    std::vector<const AttributeAutoPlanTable*> Where(const std::function<bool(const AttributeAutoPlanTable&)>& pred) const {
        std::vector<const AttributeAutoPlanTable*> result;
        for (int i = 0; i < snapshot->data.data_size(); ++i) {
            if (pred(snapshot->data.data(i))) {
                result.push_back(&snapshot->data.data(i));
            }
        }
        return result;
    }

    const AttributeAutoPlanTable* First(const std::function<bool(const AttributeAutoPlanTable&)>& pred) const {
        for (int i = 0; i < snapshot->data.data_size(); ++i) {
            if (pred(snapshot->data.data(i))) {
                return &snapshot->data.data(i);
            }
        }
        return nullptr;
    }

    // ---- Composite Key ----

private:
    LoadSuccessCallback loadSuccessCallback;
    std::unique_ptr<Snapshot> snapshot = std::make_unique<Snapshot>();
};

inline const AttributeAutoPlanTableData& FindAllAttributeAutoPlanTable() {
    return AttributeAutoPlanTableManager::Instance().FindAll();
}

// ---- Lookup guard macros ----
// Each macro looks up a row by tableId and, on success, injects two locals into
// the current scope:
//   attributeAutoPlanRow    -> const AttributeAutoPlanTable* (the matched row)
//   attributeAutoPlanResult -> uint32_t status (kInvalidTableId on miss)
// On a miss they log an error and bail out; the suffix spells out HOW they bail:
//   OrReturnError -> return the kInvalidTableId status code
//   OrReturn      -> return a caller-supplied value
//   OrReturnVoid  -> return; (for void functions)
//   OrReturnFalse -> return false;
//   OrContinue    -> continue; (skip to the next loop iteration)

#define LookupAttributeAutoPlanOrReturnError(tableId) \
    const auto [attributeAutoPlanRow, attributeAutoPlanResult] = AttributeAutoPlanTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(attributeAutoPlanRow)) { LOG_ERROR << "AttributeAutoPlan row not found for ID: " << tableId; return attributeAutoPlanResult; } } while(0)

#define LookupAttributeAutoPlanAsOrReturnError(prefix, tableId) \
    const auto [prefix##AttributeAutoPlanRow, prefix##AttributeAutoPlanResult] = AttributeAutoPlanTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(prefix##AttributeAutoPlanRow)) { LOG_ERROR << "AttributeAutoPlan row not found for ID: " << tableId; return prefix##AttributeAutoPlanResult; } } while(0)

#define LookupAttributeAutoPlanOrReturn(tableId, customReturnValue) \
    const auto [attributeAutoPlanRow, attributeAutoPlanResult] = AttributeAutoPlanTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(attributeAutoPlanRow)) { LOG_ERROR << "AttributeAutoPlan row not found for ID: " << tableId; return customReturnValue; } } while(0)

#define LookupAttributeAutoPlanOrReturnVoid(tableId) \
    const auto [attributeAutoPlanRow, attributeAutoPlanResult] = AttributeAutoPlanTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(attributeAutoPlanRow)) { LOG_ERROR << "AttributeAutoPlan row not found for ID: " << tableId; return; } } while(0)

#define LookupAttributeAutoPlanOrContinue(tableId) \
    const auto [attributeAutoPlanRow, attributeAutoPlanResult] = AttributeAutoPlanTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(attributeAutoPlanRow)) { LOG_ERROR << "AttributeAutoPlan row not found for ID: " << tableId; continue; } } while(0)

#define LookupAttributeAutoPlanOrReturnFalse(tableId) \
    const auto [attributeAutoPlanRow, attributeAutoPlanResult] = AttributeAutoPlanTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(attributeAutoPlanRow)) { LOG_ERROR << "AttributeAutoPlan row not found for ID: " << tableId; return false; } } while(0)
