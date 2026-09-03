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
#include "table/proto/attributerule_table.pb.h"

class AttributeRuleTableManager {
public:
    using IdMapType = std::unordered_map<uint32_t, const AttributeRuleTable*>;
    using LoadSuccessCallback = std::function<void()>;

    // Internal snapshot holding all parsed data and indices.
    // Load() builds a new snapshot and swaps it in, replacing the old one.
    struct Snapshot {
        AttributeRuleTableData data;
        IdMapType idMap;
    };

    static AttributeRuleTableManager& Instance() {
        static AttributeRuleTableManager instance;
        return instance;
    }

    const Snapshot& GetSnapshot() const { return *snapshot; }

    const AttributeRuleTableData& FindAll() const { return snapshot->data; }

    std::pair<const AttributeRuleTable*, uint32_t> FindById(uint32_t tableId);
    std::pair<const AttributeRuleTable*, uint32_t> FindByIdSilent(uint32_t tableId);
    const IdMapType& GetIdMap() const { return snapshot->idMap; }

    void Load();

    void SetLoadSuccessCallback(const LoadSuccessCallback& callback) {
        loadSuccessCallback = callback;
    }

    void LoadSuccess() { if (loadSuccessCallback) { loadSuccessCallback(); } }

    // ---- Exists ----

    bool Exists(uint32_t id) const { return snapshot->idMap.count(id) > 0; }

    // ---- Count ----

    std::size_t Count() const { return snapshot->idMap.size(); }

    // ---- FindByIds (IN) ----

    std::vector<const AttributeRuleTable*> FindByIds(const std::vector<uint32_t>& ids) const {
        std::vector<const AttributeRuleTable*> result;
        result.reserve(ids.size());
        for (auto id : ids) {
            if (auto it = snapshot->idMap.find(id); it != snapshot->idMap.end()) {
                result.push_back(it->second);
            }
        }
        return result;
    }

    // ---- RandOne ----

    const AttributeRuleTable* RandOne() const {
        if (snapshot->data.data_size() == 0) return nullptr;
        thread_local std::mt19937 rng{std::random_device{}()};
        std::uniform_int_distribution<int> dist(0, snapshot->data.data_size() - 1);
        return &snapshot->data.data(dist(rng));
    }

    // ---- Where / First ----

    std::vector<const AttributeRuleTable*> Where(const std::function<bool(const AttributeRuleTable&)>& pred) const {
        std::vector<const AttributeRuleTable*> result;
        for (int i = 0; i < snapshot->data.data_size(); ++i) {
            if (pred(snapshot->data.data(i))) {
                result.push_back(&snapshot->data.data(i));
            }
        }
        return result;
    }

    const AttributeRuleTable* First(const std::function<bool(const AttributeRuleTable&)>& pred) const {
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

inline const AttributeRuleTableData& FindAllAttributeRuleTable() {
    return AttributeRuleTableManager::Instance().FindAll();
}

// ---- Lookup guard macros ----
// Each macro looks up a row by tableId and, on success, injects two locals into
// the current scope:
//   attributeRuleRow    -> const AttributeRuleTable* (the matched row)
//   attributeRuleResult -> uint32_t status (kInvalidTableId on miss)
// On a miss they log an error and bail out; the suffix spells out HOW they bail:
//   OrReturnError -> return the kInvalidTableId status code
//   OrReturn      -> return a caller-supplied value
//   OrReturnVoid  -> return; (for void functions)
//   OrReturnFalse -> return false;
//   OrContinue    -> continue; (skip to the next loop iteration)

#define LookupAttributeRuleOrReturnError(tableId) \
    const auto [attributeRuleRow, attributeRuleResult] = AttributeRuleTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(attributeRuleRow)) { LOG_ERROR << "AttributeRule row not found for ID: " << tableId; return attributeRuleResult; } } while(0)

#define LookupAttributeRuleAsOrReturnError(prefix, tableId) \
    const auto [prefix##AttributeRuleRow, prefix##AttributeRuleResult] = AttributeRuleTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(prefix##AttributeRuleRow)) { LOG_ERROR << "AttributeRule row not found for ID: " << tableId; return prefix##AttributeRuleResult; } } while(0)

#define LookupAttributeRuleOrReturn(tableId, customReturnValue) \
    const auto [attributeRuleRow, attributeRuleResult] = AttributeRuleTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(attributeRuleRow)) { LOG_ERROR << "AttributeRule row not found for ID: " << tableId; return customReturnValue; } } while(0)

#define LookupAttributeRuleOrReturnVoid(tableId) \
    const auto [attributeRuleRow, attributeRuleResult] = AttributeRuleTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(attributeRuleRow)) { LOG_ERROR << "AttributeRule row not found for ID: " << tableId; return; } } while(0)

#define LookupAttributeRuleOrContinue(tableId) \
    const auto [attributeRuleRow, attributeRuleResult] = AttributeRuleTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(attributeRuleRow)) { LOG_ERROR << "AttributeRule row not found for ID: " << tableId; continue; } } while(0)

#define LookupAttributeRuleOrReturnFalse(tableId) \
    const auto [attributeRuleRow, attributeRuleResult] = AttributeRuleTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(attributeRuleRow)) { LOG_ERROR << "AttributeRule row not found for ID: " << tableId; return false; } } while(0)
