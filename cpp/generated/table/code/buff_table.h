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
#include "table/proto/buff_table.pb.h"

class BuffTableManager {
public:
    using IdMapType = std::unordered_map<uint32_t, const BuffTable*>;
    using LoadSuccessCallback = std::function<void()>;

    // Internal snapshot holding all parsed data and indices.
    // Load() builds a new snapshot and swaps it in, replacing the old one.
    struct Snapshot {
        BuffTableData data;
        IdMapType idMap;
        ExcelExpression<double> healthRegenerationExpr;
        ExcelExpression<double> bonusDamageExpr;
        std::unordered_multimap<double, const BuffTable*> intervalEffectIndex;
        std::unordered_multimap<uint32_t, const BuffTable*> subBuffIndex;
        std::unordered_multimap<uint32_t, const BuffTable*> targetSubBuffIndex;
    };

    static BuffTableManager& Instance() {
        static BuffTableManager instance;
        return instance;
    }

    const Snapshot& GetSnapshot() const { return *snapshot; }

    const BuffTableData& FindAll() const { return snapshot->data; }

    // ---- 生命周期契约(热更) ----
    //
    // Load() 会整批建好新 Snapshot 再换掉旧的,**旧 Snapshot 当场析构**。
    // 所以下面所有返回指针 / 引用 / 迭代器的接口,返回值只在**下一次 Load() 之前**有效。
    //
    //   ✅ 存 id,用的时候现查
    //   ❌ 把 const BuffTable* 存进成员、容器、闭包、协程帧
    //
    // 存指针在热更那一刻就是野指针,而且不会有任何报错。
    std::pair<const BuffTable*, uint32_t> FindById(uint32_t tableId);
    std::pair<const BuffTable*, uint32_t> FindByIdSilent(uint32_t tableId);
    const IdMapType& GetIdMap() const { return snapshot->idMap; }

    void Load();

    void SetLoadSuccessCallback(const LoadSuccessCallback& callback) {
        loadSuccessCallback = callback;
    }

    void LoadSuccess() { if (loadSuccessCallback) { loadSuccessCallback(); } }

    double GetHealthRegeneration(uint32_t tableId) {
        auto [table, fetchResult] = FindById(tableId);
        if (table == nullptr) {
            return double();
        }
        return snapshot->healthRegenerationExpr.Value(table->health_regeneration());
    }
    void SetHealthRegenerationParam(const std::vector<double>& paramList) {
        snapshot->healthRegenerationExpr.SetParam(paramList);
    }

    double GetBonusDamage(uint32_t tableId) {
        auto [table, fetchResult] = FindById(tableId);
        if (table == nullptr) {
            return double();
        }
        return snapshot->bonusDamageExpr.Value(table->bonus_damage());
    }
    void SetBonusDamageParam(const std::vector<double>& paramList) {
        snapshot->bonusDamageExpr.SetParam(paramList);
    }

    const std::unordered_multimap<double, const BuffTable*>& GetIntervalEffectIndex() const { return snapshot->intervalEffectIndex; }
    const std::unordered_multimap<uint32_t, const BuffTable*>& GetSubBuffIndex() const { return snapshot->subBuffIndex; }
    const std::unordered_multimap<uint32_t, const BuffTable*>& GetTargetSubBuffIndex() const { return snapshot->targetSubBuffIndex; }

    // ---- Exists ----

    bool Exists(uint32_t id) const { return snapshot->idMap.count(id) > 0; }

    // ---- Count ----

    std::size_t Count() const { return snapshot->idMap.size(); }
    std::size_t CountByIntervalEffectIndex(double key) const { return snapshot->intervalEffectIndex.count(key); }
    std::size_t CountBySubBuffIndex(uint32_t key) const { return snapshot->subBuffIndex.count(key); }
    std::size_t CountByTargetSubBuffIndex(uint32_t key) const { return snapshot->targetSubBuffIndex.count(key); }

    // ---- FindByIds (IN) ----

    std::vector<const BuffTable*> FindByIds(const std::vector<uint32_t>& ids) const {
        std::vector<const BuffTable*> result;
        result.reserve(ids.size());
        for (auto id : ids) {
            if (auto it = snapshot->idMap.find(id); it != snapshot->idMap.end()) {
                result.push_back(it->second);
            }
        }
        return result;
    }

    // ---- RandOne ----

    const BuffTable* RandOne() const {
        if (snapshot->data.data_size() == 0) return nullptr;
        thread_local std::mt19937 rng{std::random_device{}()};
        std::uniform_int_distribution<int> dist(0, snapshot->data.data_size() - 1);
        return &snapshot->data.data(dist(rng));
    }

    // ---- Where / First ----

    std::vector<const BuffTable*> Where(const std::function<bool(const BuffTable&)>& pred) const {
        std::vector<const BuffTable*> result;
        for (int i = 0; i < snapshot->data.data_size(); ++i) {
            if (pred(snapshot->data.data(i))) {
                result.push_back(&snapshot->data.data(i));
            }
        }
        return result;
    }

    const BuffTable* First(const std::function<bool(const BuffTable&)>& pred) const {
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

inline const BuffTableData& FindAllBuffTable() {
    return BuffTableManager::Instance().FindAll();
}

// ---- Lookup guard macros ----
// Each macro looks up a row by tableId and, on success, injects two locals into
// the current scope:
//   buffRow    -> const BuffTable* (the matched row)
//   buffResult -> uint32_t status (kInvalidTableId on miss)
// On a miss they log an error and bail out; the suffix spells out HOW they bail:
//   OrReturnError -> return the kInvalidTableId status code
//   OrReturn      -> return a caller-supplied value
//   OrReturnVoid  -> return; (for void functions)
//   OrReturnFalse -> return false;
//   OrContinue    -> continue; (skip to the next loop iteration)

#define LookupBuffOrReturnError(tableId) \
    const auto [buffRow, buffResult] = BuffTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(buffRow)) { LOG_ERROR << "Buff row not found for ID: " << tableId; return buffResult; } } while(0)

#define LookupBuffAsOrReturnError(prefix, tableId) \
    const auto [prefix##BuffRow, prefix##BuffResult] = BuffTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(prefix##BuffRow)) { LOG_ERROR << "Buff row not found for ID: " << tableId; return prefix##BuffResult; } } while(0)

#define LookupBuffOrReturn(tableId, customReturnValue) \
    const auto [buffRow, buffResult] = BuffTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(buffRow)) { LOG_ERROR << "Buff row not found for ID: " << tableId; return customReturnValue; } } while(0)

#define LookupBuffOrReturnVoid(tableId) \
    const auto [buffRow, buffResult] = BuffTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(buffRow)) { LOG_ERROR << "Buff row not found for ID: " << tableId; return; } } while(0)

#define LookupBuffOrContinue(tableId) \
    const auto [buffRow, buffResult] = BuffTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(buffRow)) { LOG_ERROR << "Buff row not found for ID: " << tableId; continue; } } while(0)

#define LookupBuffOrReturnFalse(tableId) \
    const auto [buffRow, buffResult] = BuffTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(buffRow)) { LOG_ERROR << "Buff row not found for ID: " << tableId; return false; } } while(0)
