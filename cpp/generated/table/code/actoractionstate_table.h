#pragma once
#include <algorithm>
#include <cstdint>
#include <functional>
#include <memory>
#include <random>
#include <unordered_map>
#include <vector>
#include "table_log.h"
#include "table/proto/actoractionstate_table.pb.h"

class ActorActionStateTableManager {
public:
    using IdMapType = std::unordered_map<uint32_t, const ActorActionStateTable*>;
    using LoadSuccessCallback = std::function<void()>;

    // Internal snapshot holding all parsed data and indices.
    // Load() builds a new snapshot and swaps it in, replacing the old one.
    struct Snapshot {
        ActorActionStateTableData data;
        IdMapType idMap;
    };

    static ActorActionStateTableManager& Instance() {
        static ActorActionStateTableManager instance;
        return instance;
    }

    const Snapshot& GetSnapshot() const { return *snapshot; }

    const ActorActionStateTableData& FindAll() const { return snapshot->data; }

    // ---- 生命周期契约(热更) ----
    //
    // Load() 会整批建好新 Snapshot 再换掉旧的,**旧 Snapshot 当场析构**。
    // 所以下面所有返回指针 / 引用 / 迭代器的接口,返回值只在**下一次 Load() 之前**有效。
    //
    //   ✅ 存 id,用的时候现查
    //   ❌ 把 const ActorActionStateTable* 存进成员、容器、闭包、协程帧
    //
    // 存指针在热更那一刻就是野指针,而且不会有任何报错。
    std::pair<const ActorActionStateTable*, uint32_t> FindById(uint32_t tableId);
    std::pair<const ActorActionStateTable*, uint32_t> FindByIdSilent(uint32_t tableId);
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

    std::vector<const ActorActionStateTable*> FindByIds(const std::vector<uint32_t>& ids) const {
        std::vector<const ActorActionStateTable*> result;
        result.reserve(ids.size());
        for (auto id : ids) {
            if (auto it = snapshot->idMap.find(id); it != snapshot->idMap.end()) {
                result.push_back(it->second);
            }
        }
        return result;
    }

    // ---- RandOne ----

    const ActorActionStateTable* RandOne() const {
        if (snapshot->data.data_size() == 0) return nullptr;
        thread_local std::mt19937 rng{std::random_device{}()};
        std::uniform_int_distribution<int> dist(0, snapshot->data.data_size() - 1);
        return &snapshot->data.data(dist(rng));
    }

    // ---- Where / First ----

    std::vector<const ActorActionStateTable*> Where(const std::function<bool(const ActorActionStateTable&)>& pred) const {
        std::vector<const ActorActionStateTable*> result;
        for (int i = 0; i < snapshot->data.data_size(); ++i) {
            if (pred(snapshot->data.data(i))) {
                result.push_back(&snapshot->data.data(i));
            }
        }
        return result;
    }

    const ActorActionStateTable* First(const std::function<bool(const ActorActionStateTable&)>& pred) const {
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

inline const ActorActionStateTableData& FindAllActorActionStateTable() {
    return ActorActionStateTableManager::Instance().FindAll();
}

// ---- Lookup guard macros ----
// Each macro looks up a row by tableId and, on success, injects two locals into
// the current scope:
//   actorActionStateRow    -> const ActorActionStateTable* (the matched row)
//   actorActionStateResult -> uint32_t status (kInvalidTableId on miss)
// On a miss they log an error (via TableLookupLogMissing in table_log.h — the macros
// deliberately do NOT stream through muduo, so this header owes muduo nothing) and
// bail out; the suffix spells out HOW they bail:
//   OrReturnError -> return the kInvalidTableId status code
//   OrReturn      -> return a caller-supplied value
//   OrReturnVoid  -> return; (for void functions)
//   OrReturnFalse -> return false;
//   OrContinue    -> continue; (skip to the next loop iteration)

#define LookupActorActionStateOrReturnError(tableId) \
    const auto [actorActionStateRow, actorActionStateResult] = ActorActionStateTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(actorActionStateRow)) { TableLookupLogMissing("ActorActionState", tableId, __FILE__, __LINE__); return actorActionStateResult; } } while(0)

#define LookupActorActionStateAsOrReturnError(prefix, tableId) \
    const auto [prefix##ActorActionStateRow, prefix##ActorActionStateResult] = ActorActionStateTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(prefix##ActorActionStateRow)) { TableLookupLogMissing("ActorActionState", tableId, __FILE__, __LINE__); return prefix##ActorActionStateResult; } } while(0)

#define LookupActorActionStateOrReturn(tableId, customReturnValue) \
    const auto [actorActionStateRow, actorActionStateResult] = ActorActionStateTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(actorActionStateRow)) { TableLookupLogMissing("ActorActionState", tableId, __FILE__, __LINE__); return customReturnValue; } } while(0)

#define LookupActorActionStateOrReturnVoid(tableId) \
    const auto [actorActionStateRow, actorActionStateResult] = ActorActionStateTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(actorActionStateRow)) { TableLookupLogMissing("ActorActionState", tableId, __FILE__, __LINE__); return; } } while(0)

#define LookupActorActionStateOrContinue(tableId) \
    const auto [actorActionStateRow, actorActionStateResult] = ActorActionStateTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(actorActionStateRow)) { TableLookupLogMissing("ActorActionState", tableId, __FILE__, __LINE__); continue; } } while(0)

#define LookupActorActionStateOrReturnFalse(tableId) \
    const auto [actorActionStateRow, actorActionStateResult] = ActorActionStateTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(actorActionStateRow)) { TableLookupLogMissing("ActorActionState", tableId, __FILE__, __LINE__); return false; } } while(0)
