
#include "table_loader.h"

#include <algorithm>
#include <array>
#include <atomic>
#include <cstddef>
#include <thread>
#include <utility>
#include <vector>

#include "mission_table.h"
#include "reward_table.h"


TableSet::TableSet() = default;
TableSet::~TableSet() = default;
TableSet::TableSet(const TableSet&) = default;
TableSet& TableSet::operator=(const TableSet&) = default;
TableSet::TableSet(TableSet&&) noexcept = default;
TableSet& TableSet::operator=(TableSet&&) noexcept = default;

LoadSuccessCallback loadSuccessCallback;

namespace {

constexpr std::size_t kTableCount = 2;

constexpr std::array<std::string_view, 2> kTableNames = {
    "Mission",
    "Reward",
};


constexpr std::array<std::string_view, 1> kHotReloadableTables = {
    "Reward",
};


uint64_t tableLoadVersion = 0;
std::shared_ptr<const TableSet> gCurrentTableSet;

std::shared_ptr<TableSet> CaptureManagersAsTableSet(const TableBundleIdentity& identity) {
    auto set = std::make_shared<TableSet>();
    set->identity = identity;

    set->missionTable = MissionTableManager::Instance().CurrentSnapshot();

    set->rewardTable = RewardTableManager::Instance().CurrentSnapshot();

    return set;
}

void InstallTableSetIntoManagers(const TableSet& set) {

    MissionTableManager::Instance().InstallSnapshot(set.missionTable);

    RewardTableManager::Instance().InstallSnapshot(set.rewardTable);

}

// 按序号加载一张表,序号与上面 LoadTables() 的顺序一致。
void LoadTableAt(std::size_t index) {
    switch (index) {
    case 0: MissionTableManager::Instance().Load(); break;
    case 1: RewardTableManager::Instance().Load(); break;
    }
}

void PublishCapturedCurrentSet() {
    TableBundleIdentity identity;
    if (gCurrentTableSet) {
        identity = gCurrentTableSet->identity;
    } else {
        identity.schemaDigest = std::string(kTableSchemaDigest);
        identity.source = "image";
    }
    identity.loadSeq = tableLoadVersion;
    gCurrentTableSet = CaptureManagersAsTableSet(identity);
}

} // namespace

ScopedTableSetOverride::ScopedTableSetOverride(const std::shared_ptr<const TableSet>& staged) {
    TableBundleIdentity prevIdentity;
    if (gCurrentTableSet) {
        prevIdentity = gCurrentTableSet->identity;
    }
    previous = CaptureManagersAsTableSet(prevIdentity);
    if (staged) {
        InstallTableSetIntoManagers(*staged);
    }
}

ScopedTableSetOverride::~ScopedTableSetOverride() {
    if (previous) {
        InstallTableSetIntoManagers(*previous);
    }
}

bool IsTableHotReloadable(const std::string_view tableName) {

    return std::binary_search(kHotReloadableTables.begin(), kHotReloadableTables.end(), tableName);

}

std::size_t GetTableCount() {
    return kTableCount;
}

std::string_view GetTableNameAt(const std::size_t index) {
    return index < kTableNames.size() ? kTableNames[index] : std::string_view{};
}

bool LoadTableIntoSetAt(const std::size_t index,
                        const std::string& configDir,
                        const bool useBinary,
                        TableSet& target,
                        std::string& err) {
    switch (index) {
    case 0:
        return MissionTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.missionTable, err);
    case 1:
        return RewardTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.rewardTable, err);
    default:
        err = "table index out of range";
        return false;
    }
}

bool PrepareTableSetSync(const std::string& configDir,
                         const bool useBinary,
                         std::shared_ptr<TableSet>& out,
                         std::string& err) {
    auto staged = std::make_shared<TableSet>();
    for (std::size_t i = 0; i < kTableCount; ++i) {
        if (!LoadTableIntoSetAt(i, configDir, useBinary, *staged, err)) {
            return false;
        }
    }
    out = std::move(staged);
    return true;
}

std::shared_ptr<const TableSet> CurrentTableSet() {
    if (!gCurrentTableSet) {
        PublishCapturedCurrentSet();
    }
    return gCurrentTableSet;
}

std::shared_ptr<const TableSet> CommitTableSet(std::shared_ptr<TableSet> next) {
    if (!next) {
        return CurrentTableSet();
    }
    auto previous = CurrentTableSet();
    next->identity.loadSeq = ++tableLoadVersion;
    InstallTableSetIntoManagers(*next);
    gCurrentTableSet = std::move(next);
    return previous;
}

void SetCurrentTableBundleIdentity(const TableBundleIdentity& identity) {
    TableBundleIdentity nextIdentity = identity;
    nextIdentity.loadSeq = tableLoadVersion;
    gCurrentTableSet = CaptureManagersAsTableSet(nextIdentity);
}

void InvokeTablesLoadSuccess() {

    MissionTableManager::Instance().LoadSuccess();

    RewardTableManager::Instance().LoadSuccess();


    if (loadSuccessCallback) {
        loadSuccessCallback();
    }
}

void LoadTables() {

    MissionTableManager::Instance().Load();

    RewardTableManager::Instance().Load();


    PublishCapturedCurrentSet();
    InvokeTablesLoadSuccess();
}

void LoadTablesAsync() {
    // 工作线程数取 CPU 核数与表数的较小者:表再多,线程数和同时在解析的表数都不超过核数。
    const std::size_t workerCount = std::min<std::size_t>(
        std::max<std::size_t>(1, std::thread::hardware_concurrency()), kTableCount);
    std::atomic<std::size_t> nextTable{0};
    {
        // 每次调用各起一组线程,走出这对花括号时全部 join(std::jthread 析构即 join):可以重复调用,
        // LoadSuccess 不会和加载并发。别改回 detach 加函数内静态门闩,那样第二次调用不会等加载完。
        std::vector<std::jthread> workers;
        workers.reserve(workerCount);
        for (std::size_t worker = 0; worker < workerCount; ++worker) {
            workers.emplace_back([&nextTable] {
                void InitThreadLocalConfig();
                InitThreadLocalConfig();
                // 各工作线程循环取号,同一个序号只会被一个线程取到。
                for (;;) {
                    const std::size_t index = nextTable.fetch_add(1);
                    if (index >= kTableCount) {
                        break;
                    }
                    LoadTableAt(index);
                }
            });
        }
    }

    PublishCapturedCurrentSet();
    InvokeTablesLoadSuccess();
}

void OnTablesLoadSuccess(const LoadSuccessCallback& callback) {
    loadSuccessCallback = callback;
}

void ReloadTables() {
    ++tableLoadVersion;
    LoadTables();
}

uint64_t GetTableLoadVersion() {
    return tableLoadVersion;
}