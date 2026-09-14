#pragma once
#include <algorithm>
#include <cstdint>
#include <functional>
#include <memory>
#include <random>
#include <unordered_map>
#include <vector>
#include "table_log.h"
#include "table/proto/attributedimension_table.pb.h"

class AttributeDimensionTableManager {
public:
    using IdMapType = std::unordered_map<uint32_t, const AttributeDimensionTable*>;
    using LoadSuccessCallback = std::function<void()>;

    // Internal snapshot holding all parsed data and indices.
    // Load() builds a new snapshot and swaps it in, replacing the old one.
    struct Snapshot {
        AttributeDimensionTableData data;
        IdMapType idMap;
        std::unordered_map<uint32_t, std::vector<const AttributeDimensionTable*>> poolIdIndex;
    };

    static AttributeDimensionTableManager& Instance() {
        static AttributeDimensionTableManager instance;
        return instance;
    }

    const Snapshot& GetSnapshot() const { return *snapshot; }

    const AttributeDimensionTableData& FindAll() const { return snapshot->data; }

    // ---- 生命周期契约(热更) ----
    //
    // Load() 会整批建好新 Snapshot 再换掉旧的,**旧 Snapshot 当场析构**。
    // 所以下面所有返回指针 / 引用 / 迭代器的接口,返回值只在**下一次 Load() 之前**有效。
    //
    //   ✅ 存 id,用的时候现查
    //   ❌ 把 const AttributeDimensionTable* 存进成员、容器、闭包、协程帧
    //
    // 存指针在热更那一刻就是野指针,而且不会有任何报错。
    std::pair<const AttributeDimensionTable*, uint32_t> FindById(uint32_t tableId);
    std::pair<const AttributeDimensionTable*, uint32_t> FindByIdSilent(uint32_t tableId);
    const IdMapType& GetIdMap() const { return snapshot->idMap; }

    void Load();

    void SetLoadSuccessCallback(const LoadSuccessCallback& callback) {
        loadSuccessCallback = callback;
    }

    void LoadSuccess() { if (loadSuccessCallback) { loadSuccessCallback(); } }

    // FK: pool_id -> AttributePool.id
    const std::unordered_map<uint32_t, std::vector<const AttributeDimensionTable*>>& GetPoolIdIndex() const { return snapshot->poolIdIndex; }
    const std::vector<const AttributeDimensionTable*>& GetByPoolId(uint32_t key) const {
        static const std::vector<const AttributeDimensionTable*> kEmpty;
        auto it = snapshot->poolIdIndex.find(key);
        return it != snapshot->poolIdIndex.end() ? it->second : kEmpty;
    }

    // ---- Exists ----

    bool Exists(uint32_t id) const { return snapshot->idMap.count(id) > 0; }

    // ---- Count ----

    std::size_t Count() const { return snapshot->idMap.size(); }
    std::size_t CountByPoolIdIndex(uint32_t key) const {
        auto it = snapshot->poolIdIndex.find(key);
        return it != snapshot->poolIdIndex.end() ? it->second.size() : 0;
    }

    // ---- FindByIds (IN) ----

    std::vector<const AttributeDimensionTable*> FindByIds(const std::vector<uint32_t>& ids) const {
        std::vector<const AttributeDimensionTable*> result;
        result.reserve(ids.size());
        for (auto id : ids) {
            if (auto it = snapshot->idMap.find(id); it != snapshot->idMap.end()) {
                result.push_back(it->second);
            }
        }
        return result;
    }

    // ---- RandOne ----

    const AttributeDimensionTable* RandOne() const {
        if (snapshot->data.data_size() == 0) return nullptr;
        thread_local std::mt19937 rng{std::random_device{}()};
        std::uniform_int_distribution<int> dist(0, snapshot->data.data_size() - 1);
        return &snapshot->data.data(dist(rng));
    }

    // ---- Where / First ----

    std::vector<const AttributeDimensionTable*> Where(const std::function<bool(const AttributeDimensionTable&)>& pred) const {
        std::vector<const AttributeDimensionTable*> result;
        for (int i = 0; i < snapshot->data.data_size(); ++i) {
            if (pred(snapshot->data.data(i))) {
                result.push_back(&snapshot->data.data(i));
            }
        }
        return result;
    }

    const AttributeDimensionTable* First(const std::function<bool(const AttributeDimensionTable&)>& pred) const {
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

inline const AttributeDimensionTableData& FindAllAttributeDimensionTable() {
    return AttributeDimensionTableManager::Instance().FindAll();
}

// ---- Lookup guard macros ----
// Each macro looks up a row by tableId and, on success, injects two locals into
// the current scope:
//   attributeDimensionRow    -> const AttributeDimensionTable* (the matched row)
//   attributeDimensionResult -> uint32_t status (kInvalidTableId on miss)
// On a miss they log an error (via TableLookupLogMissing in table_log.h — the macros
// deliberately do NOT stream through muduo, so this header owes muduo nothing) and
// bail out; the suffix spells out HOW they bail:
//   OrReturnError -> return the kInvalidTableId status code
//   OrReturn      -> return a caller-supplied value
//   OrReturnVoid  -> return; (for void functions)
//   OrReturnFalse -> return false;
//   OrContinue    -> continue; (skip to the next loop iteration)

#define LookupAttributeDimensionOrReturnError(tableId) \
    const auto [attributeDimensionRow, attributeDimensionResult] = AttributeDimensionTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(attributeDimensionRow)) { TableLookupLogMissing("AttributeDimension", tableId, __FILE__, __LINE__); return attributeDimensionResult; } } while(0)

#define LookupAttributeDimensionAsOrReturnError(prefix, tableId) \
    const auto [prefix##AttributeDimensionRow, prefix##AttributeDimensionResult] = AttributeDimensionTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(prefix##AttributeDimensionRow)) { TableLookupLogMissing("AttributeDimension", tableId, __FILE__, __LINE__); return prefix##AttributeDimensionResult; } } while(0)

#define LookupAttributeDimensionOrReturn(tableId, customReturnValue) \
    const auto [attributeDimensionRow, attributeDimensionResult] = AttributeDimensionTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(attributeDimensionRow)) { TableLookupLogMissing("AttributeDimension", tableId, __FILE__, __LINE__); return customReturnValue; } } while(0)

#define LookupAttributeDimensionOrReturnVoid(tableId) \
    const auto [attributeDimensionRow, attributeDimensionResult] = AttributeDimensionTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(attributeDimensionRow)) { TableLookupLogMissing("AttributeDimension", tableId, __FILE__, __LINE__); return; } } while(0)

// OrContinue 是唯一不能套 do{...}while(0) 的一个:continue 会绑定到 do-while 自身,
// 跳去求值 while(0) 判假退出,于是控制流直接落到宏后面 —— 守卫变成空操作,
// 调用方紧接着解引用空行指针。(return 系列不受影响,return 能穿透 do-while。)
// 少了这层包装也没有代价:宏本身要声明两个变量,从来就不能当单语句塞进无括号 if。
#define LookupAttributeDimensionOrContinue(tableId) \
    const auto [attributeDimensionRow, attributeDimensionResult] = AttributeDimensionTableManager::Instance().FindByIdSilent(tableId); \
    if (!(attributeDimensionRow)) { TableLookupLogMissing("AttributeDimension", tableId, __FILE__, __LINE__); continue; }

#define LookupAttributeDimensionOrReturnFalse(tableId) \
    const auto [attributeDimensionRow, attributeDimensionResult] = AttributeDimensionTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(attributeDimensionRow)) { TableLookupLogMissing("AttributeDimension", tableId, __FILE__, __LINE__); return false; } } while(0)
