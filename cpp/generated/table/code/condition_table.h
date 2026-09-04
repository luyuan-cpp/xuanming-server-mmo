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
#include "table/proto/condition_table.pb.h"

class ConditionTableManager {
public:
    using IdMapType = std::unordered_map<uint32_t, const ConditionTable*>;
    using LoadSuccessCallback = std::function<void()>;

    // Internal snapshot holding all parsed data and indices.
    // Load() builds a new snapshot and swaps it in, replacing the old one.
    struct Snapshot {
        ConditionTableData data;
        IdMapType idMap;
        std::unordered_multimap<uint32_t, const ConditionTable*> condition1Index;
        std::unordered_multimap<uint32_t, const ConditionTable*> condition2Index;
        std::unordered_multimap<uint32_t, const ConditionTable*> condition3Index;
        std::unordered_multimap<uint32_t, const ConditionTable*> condition4Index;
    };

    static ConditionTableManager& Instance() {
        static ConditionTableManager instance;
        return instance;
    }

    const Snapshot& GetSnapshot() const { return *snapshot; }

    const ConditionTableData& FindAll() const { return snapshot->data; }

    // ---- 生命周期契约(热更) ----
    //
    // Load() 会整批建好新 Snapshot 再换掉旧的,**旧 Snapshot 当场析构**。
    // 所以下面所有返回指针 / 引用 / 迭代器的接口,返回值只在**下一次 Load() 之前**有效。
    //
    //   ✅ 存 id,用的时候现查
    //   ❌ 把 const ConditionTable* 存进成员、容器、闭包、协程帧
    //
    // 存指针在热更那一刻就是野指针,而且不会有任何报错。
    std::pair<const ConditionTable*, uint32_t> FindById(uint32_t tableId);
    std::pair<const ConditionTable*, uint32_t> FindByIdSilent(uint32_t tableId);
    const IdMapType& GetIdMap() const { return snapshot->idMap; }

    void Load();

    void SetLoadSuccessCallback(const LoadSuccessCallback& callback) {
        loadSuccessCallback = callback;
    }

    void LoadSuccess() { if (loadSuccessCallback) { loadSuccessCallback(); } }

    const std::unordered_multimap<uint32_t, const ConditionTable*>& GetCondition1Index() const { return snapshot->condition1Index; }
    const std::unordered_multimap<uint32_t, const ConditionTable*>& GetCondition2Index() const { return snapshot->condition2Index; }
    const std::unordered_multimap<uint32_t, const ConditionTable*>& GetCondition3Index() const { return snapshot->condition3Index; }
    const std::unordered_multimap<uint32_t, const ConditionTable*>& GetCondition4Index() const { return snapshot->condition4Index; }

    // ---- Exists ----

    bool Exists(uint32_t id) const { return snapshot->idMap.count(id) > 0; }

    // ---- Count ----

    std::size_t Count() const { return snapshot->idMap.size(); }
    std::size_t CountByCondition1Index(uint32_t key) const { return snapshot->condition1Index.count(key); }
    std::size_t CountByCondition2Index(uint32_t key) const { return snapshot->condition2Index.count(key); }
    std::size_t CountByCondition3Index(uint32_t key) const { return snapshot->condition3Index.count(key); }
    std::size_t CountByCondition4Index(uint32_t key) const { return snapshot->condition4Index.count(key); }

    // ---- FindByIds (IN) ----

    std::vector<const ConditionTable*> FindByIds(const std::vector<uint32_t>& ids) const {
        std::vector<const ConditionTable*> result;
        result.reserve(ids.size());
        for (auto id : ids) {
            if (auto it = snapshot->idMap.find(id); it != snapshot->idMap.end()) {
                result.push_back(it->second);
            }
        }
        return result;
    }

    // ---- RandOne ----

    const ConditionTable* RandOne() const {
        if (snapshot->data.data_size() == 0) return nullptr;
        thread_local std::mt19937 rng{std::random_device{}()};
        std::uniform_int_distribution<int> dist(0, snapshot->data.data_size() - 1);
        return &snapshot->data.data(dist(rng));
    }

    // ---- Where / First ----

    std::vector<const ConditionTable*> Where(const std::function<bool(const ConditionTable&)>& pred) const {
        std::vector<const ConditionTable*> result;
        for (int i = 0; i < snapshot->data.data_size(); ++i) {
            if (pred(snapshot->data.data(i))) {
                result.push_back(&snapshot->data.data(i));
            }
        }
        return result;
    }

    const ConditionTable* First(const std::function<bool(const ConditionTable&)>& pred) const {
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

inline const ConditionTableData& FindAllConditionTable() {
    return ConditionTableManager::Instance().FindAll();
}

// ---- Lookup guard macros ----
// Each macro looks up a row by tableId and, on success, injects two locals into
// the current scope:
//   conditionRow    -> const ConditionTable* (the matched row)
//   conditionResult -> uint32_t status (kInvalidTableId on miss)
// On a miss they log an error and bail out; the suffix spells out HOW they bail:
//   OrReturnError -> return the kInvalidTableId status code
//   OrReturn      -> return a caller-supplied value
//   OrReturnVoid  -> return; (for void functions)
//   OrReturnFalse -> return false;
//   OrContinue    -> continue; (skip to the next loop iteration)

#define LookupConditionOrReturnError(tableId) \
    const auto [conditionRow, conditionResult] = ConditionTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(conditionRow)) { LOG_ERROR << "Condition row not found for ID: " << tableId; return conditionResult; } } while(0)

#define LookupConditionAsOrReturnError(prefix, tableId) \
    const auto [prefix##ConditionRow, prefix##ConditionResult] = ConditionTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(prefix##ConditionRow)) { LOG_ERROR << "Condition row not found for ID: " << tableId; return prefix##ConditionResult; } } while(0)

#define LookupConditionOrReturn(tableId, customReturnValue) \
    const auto [conditionRow, conditionResult] = ConditionTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(conditionRow)) { LOG_ERROR << "Condition row not found for ID: " << tableId; return customReturnValue; } } while(0)

#define LookupConditionOrReturnVoid(tableId) \
    const auto [conditionRow, conditionResult] = ConditionTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(conditionRow)) { LOG_ERROR << "Condition row not found for ID: " << tableId; return; } } while(0)

#define LookupConditionOrContinue(tableId) \
    const auto [conditionRow, conditionResult] = ConditionTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(conditionRow)) { LOG_ERROR << "Condition row not found for ID: " << tableId; continue; } } while(0)

#define LookupConditionOrReturnFalse(tableId) \
    const auto [conditionRow, conditionResult] = ConditionTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(conditionRow)) { LOG_ERROR << "Condition row not found for ID: " << tableId; return false; } } while(0)
