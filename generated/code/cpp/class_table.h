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
#include "table/proto/class_table.pb.h"

class ClassTableManager {
public:
    using IdMapType = std::unordered_map<uint32_t, const ClassTable*>;
    using LoadSuccessCallback = std::function<void()>;

    // Internal snapshot holding all parsed data and indices.
    // Load() builds a new snapshot and swaps it in, replacing the old one.
    struct Snapshot {
        ClassTableData data;
        IdMapType idMap;
        std::unordered_multimap<uint32_t, const ClassTable*> skillIndex;
    };

    static ClassTableManager& Instance() {
        static ClassTableManager instance;
        return instance;
    }

    const Snapshot& GetSnapshot() const { return *snapshot; }

    const ClassTableData& FindAll() const { return snapshot->data; }

    // ---- 生命周期契约(热更) ----
    //
    // Load() 会整批建好新 Snapshot 再换掉旧的,**旧 Snapshot 当场析构**。
    // 所以下面所有返回指针 / 引用 / 迭代器的接口,返回值只在**下一次 Load() 之前**有效。
    //
    //   ✅ 存 id,用的时候现查
    //   ❌ 把 const ClassTable* 存进成员、容器、闭包、协程帧
    //
    // 存指针在热更那一刻就是野指针,而且不会有任何报错。
    std::pair<const ClassTable*, uint32_t> FindById(uint32_t tableId);
    std::pair<const ClassTable*, uint32_t> FindByIdSilent(uint32_t tableId);
    const IdMapType& GetIdMap() const { return snapshot->idMap; }

    void Load();

    void SetLoadSuccessCallback(const LoadSuccessCallback& callback) {
        loadSuccessCallback = callback;
    }

    void LoadSuccess() { if (loadSuccessCallback) { loadSuccessCallback(); } }

    const std::unordered_multimap<uint32_t, const ClassTable*>& GetSkillIndex() const { return snapshot->skillIndex; }

    // ---- Exists ----

    bool Exists(uint32_t id) const { return snapshot->idMap.count(id) > 0; }

    // ---- Count ----

    std::size_t Count() const { return snapshot->idMap.size(); }
    std::size_t CountBySkillIndex(uint32_t key) const { return snapshot->skillIndex.count(key); }

    // ---- FindByIds (IN) ----

    std::vector<const ClassTable*> FindByIds(const std::vector<uint32_t>& ids) const {
        std::vector<const ClassTable*> result;
        result.reserve(ids.size());
        for (auto id : ids) {
            if (auto it = snapshot->idMap.find(id); it != snapshot->idMap.end()) {
                result.push_back(it->second);
            }
        }
        return result;
    }

    // ---- RandOne ----

    const ClassTable* RandOne() const {
        if (snapshot->data.data_size() == 0) return nullptr;
        thread_local std::mt19937 rng{std::random_device{}()};
        std::uniform_int_distribution<int> dist(0, snapshot->data.data_size() - 1);
        return &snapshot->data.data(dist(rng));
    }

    // ---- Where / First ----

    std::vector<const ClassTable*> Where(const std::function<bool(const ClassTable&)>& pred) const {
        std::vector<const ClassTable*> result;
        for (int i = 0; i < snapshot->data.data_size(); ++i) {
            if (pred(snapshot->data.data(i))) {
                result.push_back(&snapshot->data.data(i));
            }
        }
        return result;
    }

    const ClassTable* First(const std::function<bool(const ClassTable&)>& pred) const {
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

inline const ClassTableData& FindAllClassTable() {
    return ClassTableManager::Instance().FindAll();
}

// ---- Lookup guard macros ----
// Each macro looks up a row by tableId and, on success, injects two locals into
// the current scope:
//   classRow    -> const ClassTable* (the matched row)
//   classResult -> uint32_t status (kInvalidTableId on miss)
// On a miss they log an error and bail out; the suffix spells out HOW they bail:
//   OrReturnError -> return the kInvalidTableId status code
//   OrReturn      -> return a caller-supplied value
//   OrReturnVoid  -> return; (for void functions)
//   OrReturnFalse -> return false;
//   OrContinue    -> continue; (skip to the next loop iteration)

#define LookupClassOrReturnError(tableId) \
    const auto [classRow, classResult] = ClassTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(classRow)) { LOG_ERROR << "Class row not found for ID: " << tableId; return classResult; } } while(0)

#define LookupClassAsOrReturnError(prefix, tableId) \
    const auto [prefix##ClassRow, prefix##ClassResult] = ClassTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(prefix##ClassRow)) { LOG_ERROR << "Class row not found for ID: " << tableId; return prefix##ClassResult; } } while(0)

#define LookupClassOrReturn(tableId, customReturnValue) \
    const auto [classRow, classResult] = ClassTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(classRow)) { LOG_ERROR << "Class row not found for ID: " << tableId; return customReturnValue; } } while(0)

#define LookupClassOrReturnVoid(tableId) \
    const auto [classRow, classResult] = ClassTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(classRow)) { LOG_ERROR << "Class row not found for ID: " << tableId; return; } } while(0)

#define LookupClassOrContinue(tableId) \
    const auto [classRow, classResult] = ClassTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(classRow)) { LOG_ERROR << "Class row not found for ID: " << tableId; continue; } } while(0)

#define LookupClassOrReturnFalse(tableId) \
    const auto [classRow, classResult] = ClassTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(classRow)) { LOG_ERROR << "Class row not found for ID: " << tableId; return false; } } while(0)
