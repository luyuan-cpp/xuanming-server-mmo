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
#include "table/proto/skill_table.pb.h"

class SkillTableManager {
public:
    using IdMapType = std::unordered_map<uint32_t, const SkillTable*>;
    using LoadSuccessCallback = std::function<void()>;

    // Internal snapshot holding all parsed data and indices.
    // Load() builds a new snapshot and swaps it in, replacing the old one.
    struct Snapshot {
        SkillTableData data;
        IdMapType idMap;
        ExcelExpression<double> damageExpr;
        std::unordered_multimap<uint32_t, const SkillTable*> skillTypeIndex;
        std::unordered_multimap<uint32_t, const SkillTable*> targetingModeIndex;
        std::unordered_multimap<uint32_t, const SkillTable*> effectIndex;
    };

    static SkillTableManager& Instance() {
        static SkillTableManager instance;
        return instance;
    }

    const Snapshot& GetSnapshot() const { return *snapshot; }

    const SkillTableData& FindAll() const { return snapshot->data; }

    // ---- 生命周期契约(热更) ----
    //
    // Load() 会整批建好新 Snapshot 再换掉旧的,**旧 Snapshot 当场析构**。
    // 所以下面所有返回指针 / 引用 / 迭代器的接口,返回值只在**下一次 Load() 之前**有效。
    //
    //   ✅ 存 id,用的时候现查
    //   ❌ 把 const SkillTable* 存进成员、容器、闭包、协程帧
    //
    // 存指针在热更那一刻就是野指针,而且不会有任何报错。
    std::pair<const SkillTable*, uint32_t> FindById(uint32_t tableId);
    std::pair<const SkillTable*, uint32_t> FindByIdSilent(uint32_t tableId);
    const IdMapType& GetIdMap() const { return snapshot->idMap; }

    void Load();

    void SetLoadSuccessCallback(const LoadSuccessCallback& callback) {
        loadSuccessCallback = callback;
    }

    void LoadSuccess() { if (loadSuccessCallback) { loadSuccessCallback(); } }

    double GetDamage(uint32_t tableId) {
        auto [table, fetchResult] = FindById(tableId);
        if (table == nullptr) {
            return double();
        }
        return snapshot->damageExpr.Value(table->damage());
    }
    void SetDamageParam(const std::vector<double>& paramList) {
        snapshot->damageExpr.SetParam(paramList);
    }

    const std::unordered_multimap<uint32_t, const SkillTable*>& GetSkillTypeIndex() const { return snapshot->skillTypeIndex; }
    const std::unordered_multimap<uint32_t, const SkillTable*>& GetTargetingModeIndex() const { return snapshot->targetingModeIndex; }
    const std::unordered_multimap<uint32_t, const SkillTable*>& GetEffectIndex() const { return snapshot->effectIndex; }

    // ---- Exists ----

    bool Exists(uint32_t id) const { return snapshot->idMap.count(id) > 0; }

    // ---- Count ----

    std::size_t Count() const { return snapshot->idMap.size(); }
    std::size_t CountBySkillTypeIndex(uint32_t key) const { return snapshot->skillTypeIndex.count(key); }
    std::size_t CountByTargetingModeIndex(uint32_t key) const { return snapshot->targetingModeIndex.count(key); }
    std::size_t CountByEffectIndex(uint32_t key) const { return snapshot->effectIndex.count(key); }

    // ---- FindByIds (IN) ----

    std::vector<const SkillTable*> FindByIds(const std::vector<uint32_t>& ids) const {
        std::vector<const SkillTable*> result;
        result.reserve(ids.size());
        for (auto id : ids) {
            if (auto it = snapshot->idMap.find(id); it != snapshot->idMap.end()) {
                result.push_back(it->second);
            }
        }
        return result;
    }

    // ---- RandOne ----

    const SkillTable* RandOne() const {
        if (snapshot->data.data_size() == 0) return nullptr;
        thread_local std::mt19937 rng{std::random_device{}()};
        std::uniform_int_distribution<int> dist(0, snapshot->data.data_size() - 1);
        return &snapshot->data.data(dist(rng));
    }

    // ---- Where / First ----

    std::vector<const SkillTable*> Where(const std::function<bool(const SkillTable&)>& pred) const {
        std::vector<const SkillTable*> result;
        for (int i = 0; i < snapshot->data.data_size(); ++i) {
            if (pred(snapshot->data.data(i))) {
                result.push_back(&snapshot->data.data(i));
            }
        }
        return result;
    }

    const SkillTable* First(const std::function<bool(const SkillTable&)>& pred) const {
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

inline const SkillTableData& FindAllSkillTable() {
    return SkillTableManager::Instance().FindAll();
}

// ---- Lookup guard macros ----
// Each macro looks up a row by tableId and, on success, injects two locals into
// the current scope:
//   skillRow    -> const SkillTable* (the matched row)
//   skillResult -> uint32_t status (kInvalidTableId on miss)
// On a miss they log an error and bail out; the suffix spells out HOW they bail:
//   OrReturnError -> return the kInvalidTableId status code
//   OrReturn      -> return a caller-supplied value
//   OrReturnVoid  -> return; (for void functions)
//   OrReturnFalse -> return false;
//   OrContinue    -> continue; (skip to the next loop iteration)

#define LookupSkillOrReturnError(tableId) \
    const auto [skillRow, skillResult] = SkillTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(skillRow)) { LOG_ERROR << "Skill row not found for ID: " << tableId; return skillResult; } } while(0)

#define LookupSkillAsOrReturnError(prefix, tableId) \
    const auto [prefix##SkillRow, prefix##SkillResult] = SkillTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(prefix##SkillRow)) { LOG_ERROR << "Skill row not found for ID: " << tableId; return prefix##SkillResult; } } while(0)

#define LookupSkillOrReturn(tableId, customReturnValue) \
    const auto [skillRow, skillResult] = SkillTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(skillRow)) { LOG_ERROR << "Skill row not found for ID: " << tableId; return customReturnValue; } } while(0)

#define LookupSkillOrReturnVoid(tableId) \
    const auto [skillRow, skillResult] = SkillTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(skillRow)) { LOG_ERROR << "Skill row not found for ID: " << tableId; return; } } while(0)

#define LookupSkillOrContinue(tableId) \
    const auto [skillRow, skillResult] = SkillTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(skillRow)) { LOG_ERROR << "Skill row not found for ID: " << tableId; continue; } } while(0)

#define LookupSkillOrReturnFalse(tableId) \
    const auto [skillRow, skillResult] = SkillTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(skillRow)) { LOG_ERROR << "Skill row not found for ID: " << tableId; return false; } } while(0)
