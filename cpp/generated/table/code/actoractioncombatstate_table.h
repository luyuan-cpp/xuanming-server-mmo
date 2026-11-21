#pragma once
#include <algorithm>
#include <cstdint>
#include <functional>
#include <memory>
#include <random>
#include <ranges>
#include <string>
#include <unordered_map>
#include <vector>
#include "table_log.h"
#include "table/proto/actoractioncombatstate_table.pb.h"

// 不可变表快照:独立命名以便 table_loader.h 仅用前向声明组合 TableSet,
// 不向所有节点入口扇出 50+ 表头文件(docs/design/config-table-hot-reload.md §8.2)。
struct ActorActionCombatStateTableSnapshot {
    using IdMapType = std::unordered_multimap<uint32_t, const ActorActionCombatStateTable*>;

    ActorActionCombatStateTableData data;
    IdMapType idMap;
};

class ActorActionCombatStateTableManager {
public:
    using Snapshot = ActorActionCombatStateTableSnapshot;
    using IdMapType = Snapshot::IdMapType;
    using LoadSuccessCallback = std::function<void()>;

    static ActorActionCombatStateTableManager& Instance() {
        static ActorActionCombatStateTableManager instance;
        return instance;
    }

    const Snapshot& GetSnapshot() const { return *snapshot; }
    std::shared_ptr<const Snapshot> CurrentSnapshot() const { return snapshot; }

    // 两阶段解析:只读目录构建新快照,失败返回 false 与英文原因,不触碰现役快照也不 LOG_FATAL。
    static bool ParseSnapshotFromDir(const std::string& configDir,
                                     bool useBinary,
                                     std::shared_ptr<const Snapshot>& out,
                                     std::string& err);

    void InstallSnapshot(std::shared_ptr<const Snapshot> next) {
        if (next) {
            snapshot = std::move(next);
        }
    }

    const ActorActionCombatStateTableData& FindAll() const { return snapshot->data; }

    // ---- 生命周期契约(热更) ----
    //
    // Load() 会整批建好新 Snapshot 再换掉旧的,**旧 Snapshot 当场析构**。
    // 所以下面所有返回指针 / 引用 / 迭代器的接口,返回值只在**下一次 Load() 之前**有效。
    //
    //   ✅ 存 id,用的时候现查
    //   ❌ 把 const ActorActionCombatStateTable* 存进成员、容器、闭包、协程帧
    //
    // 存指针在热更那一刻就是野指针,而且不会有任何报错。
    // 这张表的主键可重复((cfg_multi)),因此**不提供 FindById** —— 单值语义不成立。
    //
    // 返回的是 multimap 的 equal_range 视图,**不拷贝、不分配**:
    //
    //     for (const auto* row : Mgr::Instance().FindAllById(id)) { ... }
    //
    // 视图与它产出的指针一样,只在下一次 Load() 之前有效。
    auto FindAllById(uint32_t tableId) const {
        auto [first, last] = snapshot->idMap.equal_range(tableId);
        return std::ranges::subrange(first, last) | std::views::values;
    }

    std::size_t CountById(uint32_t tableId) const { return snapshot->idMap.count(tableId); }
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

    std::vector<const ActorActionCombatStateTable*> FindByIds(const std::vector<uint32_t>& ids) const {
        std::vector<const ActorActionCombatStateTable*> result;
        result.reserve(ids.size());
        for (auto id : ids) {
            auto [first, last] = snapshot->idMap.equal_range(id);
            for (auto it = first; it != last; ++it) {
                result.push_back(it->second);
            }
        }
        return result;
    }

    // ---- RandOne ----

    const ActorActionCombatStateTable* RandOne() const {
        if (snapshot->data.data_size() == 0) return nullptr;
        thread_local std::mt19937 rng{std::random_device{}()};
        std::uniform_int_distribution<int> dist(0, snapshot->data.data_size() - 1);
        return &snapshot->data.data(dist(rng));
    }

    // ---- Where / First ----

    std::vector<const ActorActionCombatStateTable*> Where(const std::function<bool(const ActorActionCombatStateTable&)>& pred) const {
        std::vector<const ActorActionCombatStateTable*> result;
        for (int i = 0; i < snapshot->data.data_size(); ++i) {
            if (pred(snapshot->data.data(i))) {
                result.push_back(&snapshot->data.data(i));
            }
        }
        return result;
    }

    const ActorActionCombatStateTable* First(const std::function<bool(const ActorActionCombatStateTable&)>& pred) const {
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
    std::shared_ptr<const Snapshot> snapshot = std::make_shared<const Snapshot>();
};

inline const ActorActionCombatStateTableData& FindAllActorActionCombatStateTable() {
    return ActorActionCombatStateTableManager::Instance().FindAll();
}

// ---- Lookup guard macros ----
// Each macro looks up a row by tableId and, on success, injects two locals into
// the current scope:
//   actorActionCombatStateRow    -> const ActorActionCombatStateTable* (the matched row)
//   actorActionCombatStateResult -> uint32_t status (kInvalidTableId on miss)
// On a miss they log an error (via TableLookupLogMissing in table_log.h — the macros
// deliberately do NOT stream through muduo, so this header owes muduo nothing) and
// bail out; the suffix spells out HOW they bail:
//   OrReturnError -> return the kInvalidTableId status code
//   OrReturn      -> return a caller-supplied value
//   OrReturnVoid  -> return; (for void functions)
//   OrReturnFalse -> return false;
//   OrContinue    -> continue; (skip to the next loop iteration)

#define LookupActorActionCombatStateOrReturnError(tableId) \
    const auto [actorActionCombatStateRow, actorActionCombatStateResult] = ActorActionCombatStateTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(actorActionCombatStateRow)) { TableLookupLogMissing("ActorActionCombatState", tableId, __FILE__, __LINE__); return actorActionCombatStateResult; } } while(0)

#define LookupActorActionCombatStateAsOrReturnError(prefix, tableId) \
    const auto [prefix##ActorActionCombatStateRow, prefix##ActorActionCombatStateResult] = ActorActionCombatStateTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(prefix##ActorActionCombatStateRow)) { TableLookupLogMissing("ActorActionCombatState", tableId, __FILE__, __LINE__); return prefix##ActorActionCombatStateResult; } } while(0)

#define LookupActorActionCombatStateOrReturn(tableId, customReturnValue) \
    const auto [actorActionCombatStateRow, actorActionCombatStateResult] = ActorActionCombatStateTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(actorActionCombatStateRow)) { TableLookupLogMissing("ActorActionCombatState", tableId, __FILE__, __LINE__); return customReturnValue; } } while(0)

#define LookupActorActionCombatStateOrReturnVoid(tableId) \
    const auto [actorActionCombatStateRow, actorActionCombatStateResult] = ActorActionCombatStateTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(actorActionCombatStateRow)) { TableLookupLogMissing("ActorActionCombatState", tableId, __FILE__, __LINE__); return; } } while(0)

// OrContinue 是唯一不能套 do{...}while(0) 的一个:continue 会绑定到 do-while 自身,
// 跳去求值 while(0) 判假退出,于是控制流直接落到宏后面 —— 守卫变成空操作,
// 调用方紧接着解引用空行指针。(return 系列不受影响,return 能穿透 do-while。)
// 少了这层包装也没有代价:宏本身要声明两个变量,从来就不能当单语句塞进无括号 if。
#define LookupActorActionCombatStateOrContinue(tableId) \
    const auto [actorActionCombatStateRow, actorActionCombatStateResult] = ActorActionCombatStateTableManager::Instance().FindByIdSilent(tableId); \
    if (!(actorActionCombatStateRow)) { TableLookupLogMissing("ActorActionCombatState", tableId, __FILE__, __LINE__); continue; }

#define LookupActorActionCombatStateOrReturnFalse(tableId) \
    const auto [actorActionCombatStateRow, actorActionCombatStateResult] = ActorActionCombatStateTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(actorActionCombatStateRow)) { TableLookupLogMissing("ActorActionCombatState", tableId, __FILE__, __LINE__); return false; } } while(0)
