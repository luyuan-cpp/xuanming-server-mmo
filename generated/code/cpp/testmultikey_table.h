#pragma once
#include <algorithm>
#include <cstdint>
#include <functional>
#include <memory>
#include <random>
#include <ranges>
#include <unordered_map>
#include <vector>
#include "table_log.h"
#include "table/proto/testmultikey_table.pb.h"

class TestMultiKeyTableManager {
public:
    using IdMapType = std::unordered_multimap<uint32_t, const TestMultiKeyTable*>;
    using LoadSuccessCallback = std::function<void()>;

    // Internal snapshot holding all parsed data and indices.
    // Load() builds a new snapshot and swaps it in, replacing the old one.
    struct Snapshot {
        TestMultiKeyTableData data;
        IdMapType idMap;
        std::unordered_map<std::string, const TestMultiKeyTable*> stringKeyMap;
        std::unordered_map<uint32_t, const TestMultiKeyTable*> uint32KeyMap;
        std::unordered_map<int32_t, const TestMultiKeyTable*> int32KeyMap;
        std::unordered_multimap<std::string, const TestMultiKeyTable*> mStringKeyMap;
        std::unordered_multimap<uint32_t, const TestMultiKeyTable*> mUint32KeyMap;
        std::unordered_multimap<int32_t, const TestMultiKeyTable*> mInt32KeyMap;
        std::unordered_multimap<uint32_t, const TestMultiKeyTable*> effectIndex;
        std::unordered_multimap<uint32_t, const TestMultiKeyTable*> testRefsIndex;
        std::unordered_map<uint32_t, std::vector<const TestMultiKeyTable*>> levelIndex;
        std::unordered_map<uint32_t, std::vector<const TestMultiKeyTable*>> testRefIndex;
    };

    static TestMultiKeyTableManager& Instance() {
        static TestMultiKeyTableManager instance;
        return instance;
    }

    const Snapshot& GetSnapshot() const { return *snapshot; }

    const TestMultiKeyTableData& FindAll() const { return snapshot->data; }

    // ---- 生命周期契约(热更) ----
    //
    // Load() 会整批建好新 Snapshot 再换掉旧的,**旧 Snapshot 当场析构**。
    // 所以下面所有返回指针 / 引用 / 迭代器的接口,返回值只在**下一次 Load() 之前**有效。
    //
    //   ✅ 存 id,用的时候现查
    //   ❌ 把 const TestMultiKeyTable* 存进成员、容器、闭包、协程帧
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

    std::pair<const TestMultiKeyTable*, uint32_t> FindByStringKey(const std::string& key) const;
    const std::unordered_map<std::string, const TestMultiKeyTable*>& GetStringKeyMap() const { return snapshot->stringKeyMap; }

    std::pair<const TestMultiKeyTable*, uint32_t> FindByUint32Key(uint32_t key) const;
    const std::unordered_map<uint32_t, const TestMultiKeyTable*>& GetUint32KeyMap() const { return snapshot->uint32KeyMap; }

    std::pair<const TestMultiKeyTable*, uint32_t> FindByInt32Key(int32_t key) const;
    const std::unordered_map<int32_t, const TestMultiKeyTable*>& GetInt32KeyMap() const { return snapshot->int32KeyMap; }

    std::pair<const TestMultiKeyTable*, uint32_t> FindByMStringKey(const std::string& key) const;
    const std::unordered_multimap<std::string, const TestMultiKeyTable*>& GetMStringKeyMap() const { return snapshot->mStringKeyMap; }

    std::pair<const TestMultiKeyTable*, uint32_t> FindByMUint32Key(uint32_t key) const;
    const std::unordered_multimap<uint32_t, const TestMultiKeyTable*>& GetMUint32KeyMap() const { return snapshot->mUint32KeyMap; }

    std::pair<const TestMultiKeyTable*, uint32_t> FindByMInt32Key(int32_t key) const;
    const std::unordered_multimap<int32_t, const TestMultiKeyTable*>& GetMInt32KeyMap() const { return snapshot->mInt32KeyMap; }

    // FK: test_ref -> Test.id
    const std::unordered_multimap<uint32_t, const TestMultiKeyTable*>& GetEffectIndex() const { return snapshot->effectIndex; }
    const std::unordered_multimap<uint32_t, const TestMultiKeyTable*>& GetTestRefsIndex() const { return snapshot->testRefsIndex; }
    const std::unordered_map<uint32_t, std::vector<const TestMultiKeyTable*>>& GetLevelIndex() const { return snapshot->levelIndex; }
    const std::vector<const TestMultiKeyTable*>& GetByLevel(uint32_t key) const {
        static const std::vector<const TestMultiKeyTable*> kEmpty;
        auto it = snapshot->levelIndex.find(key);
        return it != snapshot->levelIndex.end() ? it->second : kEmpty;
    }
    const std::unordered_map<uint32_t, std::vector<const TestMultiKeyTable*>>& GetTestRefIndex() const { return snapshot->testRefIndex; }
    const std::vector<const TestMultiKeyTable*>& GetByTestRef(uint32_t key) const {
        static const std::vector<const TestMultiKeyTable*> kEmpty;
        auto it = snapshot->testRefIndex.find(key);
        return it != snapshot->testRefIndex.end() ? it->second : kEmpty;
    }

    // ---- Exists ----

    bool Exists(uint32_t id) const { return snapshot->idMap.count(id) > 0; }
    bool ExistsByStringKey(const std::string& key) const { return snapshot->stringKeyMap.count(key) > 0; }
    bool ExistsByUint32Key(uint32_t key) const { return snapshot->uint32KeyMap.count(key) > 0; }
    bool ExistsByInt32Key(int32_t key) const { return snapshot->int32KeyMap.count(key) > 0; }

    // ---- Count ----

    std::size_t Count() const { return snapshot->idMap.size(); }
    std::size_t CountByMStringKey(const std::string& key) const { return snapshot->mStringKeyMap.count(key); }
    std::size_t CountByMUint32Key(uint32_t key) const { return snapshot->mUint32KeyMap.count(key); }
    std::size_t CountByMInt32Key(int32_t key) const { return snapshot->mInt32KeyMap.count(key); }
    std::size_t CountByEffectIndex(uint32_t key) const { return snapshot->effectIndex.count(key); }
    std::size_t CountByTestRefsIndex(uint32_t key) const { return snapshot->testRefsIndex.count(key); }
    std::size_t CountByLevelIndex(uint32_t key) const {
        auto it = snapshot->levelIndex.find(key);
        return it != snapshot->levelIndex.end() ? it->second.size() : 0;
    }
    std::size_t CountByTestRefIndex(uint32_t key) const {
        auto it = snapshot->testRefIndex.find(key);
        return it != snapshot->testRefIndex.end() ? it->second.size() : 0;
    }

    // ---- FindByIds (IN) ----

    std::vector<const TestMultiKeyTable*> FindByIds(const std::vector<uint32_t>& ids) const {
        std::vector<const TestMultiKeyTable*> result;
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

    const TestMultiKeyTable* RandOne() const {
        if (snapshot->data.data_size() == 0) return nullptr;
        thread_local std::mt19937 rng{std::random_device{}()};
        std::uniform_int_distribution<int> dist(0, snapshot->data.data_size() - 1);
        return &snapshot->data.data(dist(rng));
    }

    // ---- Where / First ----

    std::vector<const TestMultiKeyTable*> Where(const std::function<bool(const TestMultiKeyTable&)>& pred) const {
        std::vector<const TestMultiKeyTable*> result;
        for (int i = 0; i < snapshot->data.data_size(); ++i) {
            if (pred(snapshot->data.data(i))) {
                result.push_back(&snapshot->data.data(i));
            }
        }
        return result;
    }

    const TestMultiKeyTable* First(const std::function<bool(const TestMultiKeyTable&)>& pred) const {
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

inline const TestMultiKeyTableData& FindAllTestMultiKeyTable() {
    return TestMultiKeyTableManager::Instance().FindAll();
}

// ---- Lookup guard macros ----
// Each macro looks up a row by tableId and, on success, injects two locals into
// the current scope:
//   testMultiKeyRow    -> const TestMultiKeyTable* (the matched row)
//   testMultiKeyResult -> uint32_t status (kInvalidTableId on miss)
// On a miss they log an error (via TableLookupLogMissing in table_log.h — the macros
// deliberately do NOT stream through muduo, so this header owes muduo nothing) and
// bail out; the suffix spells out HOW they bail:
//   OrReturnError -> return the kInvalidTableId status code
//   OrReturn      -> return a caller-supplied value
//   OrReturnVoid  -> return; (for void functions)
//   OrReturnFalse -> return false;
//   OrContinue    -> continue; (skip to the next loop iteration)

#define LookupTestMultiKeyOrReturnError(tableId) \
    const auto [testMultiKeyRow, testMultiKeyResult] = TestMultiKeyTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(testMultiKeyRow)) { TableLookupLogMissing("TestMultiKey", tableId, __FILE__, __LINE__); return testMultiKeyResult; } } while(0)

#define LookupTestMultiKeyAsOrReturnError(prefix, tableId) \
    const auto [prefix##TestMultiKeyRow, prefix##TestMultiKeyResult] = TestMultiKeyTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(prefix##TestMultiKeyRow)) { TableLookupLogMissing("TestMultiKey", tableId, __FILE__, __LINE__); return prefix##TestMultiKeyResult; } } while(0)

#define LookupTestMultiKeyOrReturn(tableId, customReturnValue) \
    const auto [testMultiKeyRow, testMultiKeyResult] = TestMultiKeyTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(testMultiKeyRow)) { TableLookupLogMissing("TestMultiKey", tableId, __FILE__, __LINE__); return customReturnValue; } } while(0)

#define LookupTestMultiKeyOrReturnVoid(tableId) \
    const auto [testMultiKeyRow, testMultiKeyResult] = TestMultiKeyTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(testMultiKeyRow)) { TableLookupLogMissing("TestMultiKey", tableId, __FILE__, __LINE__); return; } } while(0)

// OrContinue 是唯一不能套 do{...}while(0) 的一个:continue 会绑定到 do-while 自身,
// 跳去求值 while(0) 判假退出,于是控制流直接落到宏后面 —— 守卫变成空操作,
// 调用方紧接着解引用空行指针。(return 系列不受影响,return 能穿透 do-while。)
// 少了这层包装也没有代价:宏本身要声明两个变量,从来就不能当单语句塞进无括号 if。
#define LookupTestMultiKeyOrContinue(tableId) \
    const auto [testMultiKeyRow, testMultiKeyResult] = TestMultiKeyTableManager::Instance().FindByIdSilent(tableId); \
    if (!(testMultiKeyRow)) { TableLookupLogMissing("TestMultiKey", tableId, __FILE__, __LINE__); continue; }

#define LookupTestMultiKeyOrReturnFalse(tableId) \
    const auto [testMultiKeyRow, testMultiKeyResult] = TestMultiKeyTableManager::Instance().FindByIdSilent(tableId); \
    do { if (!(testMultiKeyRow)) { TableLookupLogMissing("TestMultiKey", tableId, __FILE__, __LINE__); return false; } } while(0)
