#pragma once
#include <algorithm>
#include <cstdint>
#include <functional>
#include <memory>
#include <random>
#include <string>
#include <unordered_map>
#include <vector>
#include "table_log.h"
#include "table/proto/guildwarrule_table.pb.h"

// 不可变表快照:独立命名以便 table_loader.h 仅用前向声明组合 TableSet,
// 不向所有节点入口扇出 50+ 表头文件(docs/design/config-table-hot-reload.md §8.2)。
struct GuildWarRuleTableSnapshot {
    using IdMapType = std::unordered_map<uint32_t, const GuildWarRuleTable*>;

    GuildWarRuleTableData data;
    IdMapType idMap;
    std::unordered_map<uint32_t, std::vector<const GuildWarRuleTable*>> battleConfigIdIndex;
    const GuildWarRuleTable* FindRowById(uint32_t tableId) const {
        const auto it = idMap.find(tableId);
        return it != idMap.end() ? it->second : nullptr;
    }
};

class GuildWarRuleTableManager {
public:
    using Snapshot = GuildWarRuleTableSnapshot;
    using IdMapType = Snapshot::IdMapType;
    using LoadSuccessCallback = std::function<void()>;

    static GuildWarRuleTableManager& Instance() {
        static GuildWarRuleTableManager instance;
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

    const GuildWarRuleTableData& FindAll() const { return snapshot->data; }

    // ---- 生命周期契约(热更) ----
    //
    // Load() 会整批建好新 Snapshot 再换掉旧的,**旧 Snapshot 当场析构**。
    // 所以下面所有返回指针 / 引用 / 迭代器的接口,返回值只在**下一次 Load() 之前**有效。
    //
    //   ✅ 存 id,用的时候现查
    //   ❌ 把 const GuildWarRuleTable* 存进成员、容器、闭包、协程帧
    //
    // 存指针在热更那一刻就是野指针,而且不会有任何报错。
    std::pair<const GuildWarRuleTable*, uint32_t> FindById(uint32_t tableId);
    std::pair<const GuildWarRuleTable*, uint32_t> FindByIdSilent(uint32_t tableId);
    const IdMapType& GetIdMap() const { return snapshot->idMap; }

    void Load();

    void SetLoadSuccessCallback(const LoadSuccessCallback& callback) {
        loadSuccessCallback = callback;
    }

    void LoadSuccess() { if (loadSuccessCallback) { loadSuccessCallback(); } }

    // FK: battle_config_id -> Dungeon.id
    const std::unordered_map<uint32_t, std::vector<const GuildWarRuleTable*>>& GetBattleConfigIdIndex() const { return snapshot->battleConfigIdIndex; }
    const std::vector<const GuildWarRuleTable*>& GetByBattleConfigId(uint32_t key) const {
        static const std::vector<const GuildWarRuleTable*> kEmpty;
        auto it = snapshot->battleConfigIdIndex.find(key);
        return it != snapshot->battleConfigIdIndex.end() ? it->second : kEmpty;
    }

    // ---- Exists ----

    bool Exists(uint32_t id) const { return snapshot->idMap.count(id) > 0; }

    // ---- Count ----

    std::size_t Count() const { return snapshot->idMap.size(); }
    std::size_t CountByBattleConfigIdIndex(uint32_t key) const {
        auto it = snapshot->battleConfigIdIndex.find(key);
        return it != snapshot->battleConfigIdIndex.end() ? it->second.size() : 0;
    }

    // ---- FindByIds (IN) ----

    std::vector<const GuildWarRuleTable*> FindByIds(const std::vector<uint32_t>& ids) const {
        std::vector<const GuildWarRuleTable*> result;
        result.reserve(ids.size());
        for (auto id : ids) {
            if (auto it = snapshot->idMap.find(id); it != snapshot->idMap.end()) {
                result.push_back(it->second);
            }
        }
        return result;
    }

    // ---- RandOne ----

    const GuildWarRuleTable* RandOne() const {
        if (snapshot->data.data_size() == 0) return nullptr;
        thread_local std::mt19937 rng{std::random_device{}()};
        std::uniform_int_distribution<int> dist(0, snapshot->data.data_size() - 1);
        return &snapshot->data.data(dist(rng));
    }

    // ---- Where / First ----

    std::vector<const GuildWarRuleTable*> Where(const std::function<bool(const GuildWarRuleTable&)>& pred) const {
        std::vector<const GuildWarRuleTable*> result;
        for (int i = 0; i < snapshot->data.data_size(); ++i) {
            if (pred(snapshot->data.data(i))) {
                result.push_back(&snapshot->data.data(i));
            }
        }
        return result;
    }

    const GuildWarRuleTable* First(const std::function<bool(const GuildWarRuleTable&)>& pred) const {
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

inline const GuildWarRuleTableData& FindAllGuildWarRuleTable() {
    return GuildWarRuleTableManager::Instance().FindAll();
}

// ---- Lookup guard macros ----
// Each macro looks up a row by tableId and, on success, injects two locals into
// the current scope:
//   guildWarRuleRow    -> const GuildWarRuleTable* (the matched row)
//   guildWarRuleResult -> uint32_t status (kInvalidTableId on miss)
// On a miss they log an error (via TableLookupLogMissing in table_log.h — the macros
// deliberately do NOT stream through muduo, so this header owes muduo nothing) and
// bail out; the suffix spells out HOW they bail:
//   OrReturnError -> return the kInvalidTableId status code
//   OrReturn      -> return a caller-supplied value
//   OrReturnVoid  -> return; (for void functions)
//   OrReturnFalse -> return false;
//   OrContinue    -> continue; (skip to the next loop iteration)

#define LookupGuildWarRuleOrReturnError(tableId) \
    const auto [guildWarRuleRow, guildWarRuleResult] = GuildWarRuleTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(guildWarRuleRow)) { TableLookupLogMissing("GuildWarRule", tableId, __FILE__, __LINE__); return guildWarRuleResult; } } while(0)

#define LookupGuildWarRuleAsOrReturnError(prefix, tableId) \
    const auto [prefix##GuildWarRuleRow, prefix##GuildWarRuleResult] = GuildWarRuleTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(prefix##GuildWarRuleRow)) { TableLookupLogMissing("GuildWarRule", tableId, __FILE__, __LINE__); return prefix##GuildWarRuleResult; } } while(0)

#define LookupGuildWarRuleOrReturn(tableId, customReturnValue) \
    const auto [guildWarRuleRow, guildWarRuleResult] = GuildWarRuleTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(guildWarRuleRow)) { TableLookupLogMissing("GuildWarRule", tableId, __FILE__, __LINE__); return customReturnValue; } } while(0)

#define LookupGuildWarRuleOrReturnVoid(tableId) \
    const auto [guildWarRuleRow, guildWarRuleResult] = GuildWarRuleTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(guildWarRuleRow)) { TableLookupLogMissing("GuildWarRule", tableId, __FILE__, __LINE__); return; } } while(0)

// OrContinue 是唯一不能套 do{...}while(0) 的一个:continue 会绑定到 do-while 自身,
// 跳去求值 while(0) 判假退出,于是控制流直接落到宏后面 —— 守卫变成空操作,
// 调用方紧接着解引用空行指针。(return 系列不受影响,return 能穿透 do-while。)
// 少了这层包装也没有代价:宏本身要声明两个变量,从来就不能当单语句塞进无括号 if。
#define LookupGuildWarRuleOrContinue(tableId) \
    const auto [guildWarRuleRow, guildWarRuleResult] = GuildWarRuleTableManager::Instance().FindByIdSilent(tableId); \
    if (!(guildWarRuleRow)) { TableLookupLogMissing("GuildWarRule", tableId, __FILE__, __LINE__); continue; }

#define LookupGuildWarRuleOrReturnFalse(tableId) \
    const auto [guildWarRuleRow, guildWarRuleResult] = GuildWarRuleTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(guildWarRuleRow)) { TableLookupLogMissing("GuildWarRule", tableId, __FILE__, __LINE__); return false; } } while(0)
