#pragma once

#include <array>
#include <chrono>
#include <cstdint>
#include <functional>
#include <memory>
#include <optional>
#include <string>
#include <string_view>
#include <unordered_map>
#include <vector>

#include "muduo/net/EventLoop.h"
#include "table/code/table_loader.h"
#include "time/comp/timer_task_comp.h"

namespace prometheus_text
{
class Writer;
}

namespace table_bundle
{

// 环境变量约定(docs/design/config-table-hot-reload.md §7):
//   - MMORPG_TABLE_BUNDLE_ROOT: sidecar 写入的只读卷(bundles/<digest>/ 与 POINTER.json);未设 = 不启用表包模式。
//   - MMORPG_TABLE_STATUS_ROOT: 业务进程写入 STATUS.json 的卷;未设时尝试写状态跳过但不影响热更。
inline constexpr const char *kBundleRootEnv = "MMORPG_TABLE_BUNDLE_ROOT";
inline constexpr const char *kStatusRootEnv = "MMORPG_TABLE_STATUS_ROOT";
inline constexpr const char *kPointerFilename = "POINTER.json";
inline constexpr const char *kStatusFilename = "STATUS.json";
inline constexpr const char *kManifestFilename = "manifest.json";

struct ManifestArtifact
{
	std::string kind;
	std::string file;
	uint64_t size = 0;
	std::string sha256;
};

struct ManifestTableEntry
{
	std::string name;
	bool hotReload = false;
	bool idsPermanent = false;
	uint64_t rows = 0;
	std::vector<ManifestArtifact> artifacts;
};

struct BundleManifest
{
	uint64_t schemaVersion = 0;
	std::string schemaDigest;
	uint64_t version = 0;
	std::string contentDigest;
	std::unordered_map<std::string, ManifestTableEntry> tables;
};

struct PointerTarget
{
	std::string contentDigest;
	std::string bundleSha256;
	uint64_t version = 0;
	uint64_t activateAtMs = 0;
	bool immediate = false;
};

struct PointerLine
{
	std::optional<PointerTarget> current;
	std::optional<PointerTarget> next;
	uint64_t updatedAtMs = 0;
	std::string updatedBy;
	std::string reason;
};

struct Hooks
{
	// 在 ScopedTableSetOverride 作用域内执行整批校验(主循环单线程);返回 false 并填写英文 err 即拒绝该表包。
	using ValidateFn = std::function<bool(std::string &err)>;
	// 在每次 TableSet 提交生效后调用(启动与热更均会触发):例如刷新战斗配表指纹、重建派生表。
	using CommittedFn = std::function<void(const std::shared_ptr<const TableSet> &)>;

	ValidateFn validate;
	CommittedFn onCommitted;
};

// 校验目录里的 manifest.json(schema_version == 2、schema_digest 匹配、每张表对应格式的文件大小与 sha256)。
bool VerifyTableDirManifest(const std::string &dir,
							bool useBinary,
							std::string_view expectedSchemaDigest,
							BundleManifest &out,
							std::string &err);

// 读取 POINTER.json 并提取 schemaDigest 对应那一条线的指针(若指针里尚无该线返回 true 且 out=nullopt)。
bool ReadPointerLine(const std::string &pointerPath,
					 std::string_view schemaDigest,
					 std::optional<PointerLine> &out,
					 std::string &err);

// sidecar 在 POINTER.json 的 sync_errors 里记着「etcd 里有这条线,但表包这一拍没同步下来」。
// 有 schemaDigest 这条记录时返回 true 并把原因写进 reason;没有记录、读不了文件或 JSON 写坏都返回 false
// (后两种 ReadPointerLine 已经报过错)。
// 只给启动路径用:线在文件里找不到时,靠它分清「etcd 里本来就没有」(可以用镜像自带的表)
// 和「有,但表包没到位」(必须拒绝启动,否则重启的进程会静默退回更旧的表)。
bool ReadPointerSyncError(const std::string &pointerPath,
						  std::string_view schemaDigest,
						  std::string &reason);

// 计算两份清单之间发生变化的表名列表(按 useBinary 对应的 artifact sha256 比对,返回排序结果)。
std::vector<std::string> DiffChangedTables(const BundleManifest &current,
										   const BundleManifest &candidate,
										   bool useBinary);

// CLI 预检入口(--validate-tables <dir>,设计 §5.4 第 2 道):
// 命令行包含 --validate-tables 时同步校验清单、解析全部表并执行 validateFn,返回退出码 0/1;
// 未传该参数返回 std::nullopt,进程按正常路径启动。
std::optional<int> RunValidateTablesCli(int argc,
										char *argv[],
										const Hooks::ValidateFn &validateFn,
										bool useBinary = false);

// 进程内配置表热更管理器(设计 §8.1、§8.2):
//   - 全部逻辑运行在主 EventLoop 上,不新建任何线程,不含裸指针成员;
//   - 每 2 秒轮询 POINTER.json,逐表分片 queueInLoop 解析(仅捕获 weak_ptr),到 activate_at_ms 整批提交;
//   - 旧 TableSet 保留 10 分钟后退役(战斗房间等显式 shared_ptr 持有者可保活更久)。
class TableBundleManager : public std::enable_shared_from_this<TableBundleManager>
{
public:
	static constexpr double kPollIntervalSeconds = 2.0;
	static constexpr std::chrono::minutes kRetainDuration{10};

	// 时间来源可注入:留空的一项用系统时钟。单测注入假时钟,配合 PollOnce 推进,不用 sleep。
	//   - wallUnixMs:判 activate_at_ms 到没到点,也是 STATUS.json 的时间戳;
	//   - steadyNow:判旧 TableSet 的 10 分钟保留期过没过。
	// 准备耗时、单片耗时这类指标不走这里,它们量的是主循环真实被占用的时间。
	struct Clock
	{
		std::function<uint64_t()> wallUnixMs;
		std::function<std::chrono::steady_clock::time_point()> steadyNow;
	};

	explicit TableBundleManager(muduo::net::EventLoop &loop, Clock clock = {});

	// 进程级节点钩子注册:须在 Node 构造之前(ApplyPreConstructionHooks)完成。
	static void SetProcessHooks(Hooks hooks);
	static const Hooks &ProcessHooks();

	// 启动期加载配置表(由 Node::LoadAllConfigData 调用);开启表包模式时自动武装 2s 轮询。
	// serviceName 写进 STATUS.json 的 service 字段(节点类型名,如 SceneNodeService),管理接口用它标出是哪种进程。
	void LoadAtStartup(const std::string &imageConfigDir, bool useBinary, std::string serviceName);

	// 停机时取消轮询定时器并中止在途分片准备。
	void Stop();

	// 供 /metrics 采集回调写出 §11 全部五组表热更指标。
	void WriteMetrics(prometheus_text::Writer &writer) const;

	// 轮询一拍。生产由 2 秒定时器按 wallUnixMs 调用;单测直接调,nowMs 与注入的 wallUnixMs 取同一个值。
	void PollOnce(uint64_t nowMs);

private:
	struct StagingState
	{
		PointerTarget target;
		std::string bundleDir;
		BundleManifest manifest;
		std::shared_ptr<TableSet> tableSet;
		std::size_t nextTableIndex = 0;
		bool prepared = false;
		std::chrono::steady_clock::time_point startedAt;
		double maxSliceSeconds = 0.0;
	};

	struct RetiredEntry
	{
		std::shared_ptr<const TableSet> set;
		std::chrono::steady_clock::time_point expireAt;
	};

	static uint64_t NowUnixMs();
	void ScheduleNextSlice(uint64_t generation);
	void RunOnePrepareSlice(uint64_t generation);
	void FinishPrepare(uint64_t generation);
	void CommitStaged(uint64_t nowMs);
	void ReapRetired(std::chrono::steady_clock::time_point now);
	void ObservePrepareDuration(double seconds);
	void WriteStatusFile(uint64_t nowMs) const;
	void RejectCandidate(const PointerTarget &target,
						 std::string_view pendingState,
						 const std::string &reason,
						 bool isNeedsRestart,
						 uint64_t nowMs);

	std::reference_wrapper<muduo::net::EventLoop> eventLoop;
	// 构造时已把留空的项补成系统时钟,之后两项都可以直接调用。
	Clock timeSource;
	TimerTaskComp pollTimer;
	bool stopped = false;
	bool bundleMode = false;
	bool useBinary = false;
	std::string bundleRoot;
	std::string statusRoot;
	std::string imageConfigDir;
	std::string serviceName;
	// table_bundle_info 的 release_track 标签值(stable / canary),启动时按部署注入的环境变量定下。
	std::string releaseTrackName;

	bool hasActiveManifest = false;
	BundleManifest activeManifest;
	TableBundleIdentity activeIdentity;
	// 现役这一套表生效的时刻(Unix 毫秒,启动加载或热更提交时记),写进 STATUS.json 的 loaded_at_ms。
	uint64_t loadedAtMs = 0;

	// 最近一次已被明确终结(rejected / needs_restart)的目标摘要,指针未变前不重复重试。
	std::string terminalDigest;
	std::string pendingContentDigest;
	uint64_t pendingVersion = 0;
	std::string pendingState;
	std::string pendingReason;

	uint64_t stagingGeneration = 0;
	std::optional<StagingState> staging;
	std::vector<RetiredEntry> retiredSets;

	// 指标状态(全在主 EventLoop 线程读写)。
	uint64_t committedCount = 0;
	uint64_t rejectedCount = 0;
	uint64_t needsRestartCount = 0;
	double lastMaxSliceSeconds = 0.0;
	static constexpr std::array<double, 8> kPrepareBuckets{0.01, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0};
	std::array<uint64_t, 9> prepareBucketCounts{};
	double prepareDurationSumSeconds = 0.0;
};

} // namespace table_bundle
