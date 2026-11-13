#pragma once

#include <cstdint>
#include <optional>

#include "metrics/prometheus_text.h"

// 进程 fd 用量与 accept 降级的指标(gate 准入设计 G4 / G6 的观测面,docs/design/gate-connection-admission-control.md)。
//
// open_file_limit.h 只在启动时把一次关:软限够不够「客户端面准入上限 + 余量」。运行期的 fd 泄漏、余量估小了、
// 以 NODE_OPEN_FILE_LIMIT_MODE=warn 带病运行,都要靠这里的指标看见:
//   - process_open_fds / process_max_fds:用 Prometheus 各语言客户端库的标准名字,Go 服务(client_golang 的进程采集器)
//     导出的是同名指标,一条「用量超过软限 80%」的规则同时管住 C++ 节点与 Go 服务;
//   - cpp_node_open_file_limit_*:启动门槛与豁免是否生效;
//   - cpp_node_accept_*:muduo Acceptor 因资源耗尽降级的累计次数(Acceptor::exhaustionCounts)。
// 告警在 deploy/k8s/cpp-node-resource-alerts.yaml,指标名由 tools/scripts/tests/cpp_node_resource_alerts_contract.tests.ps1 钉住。
//
// 本头文件只含纯逻辑:不做系统调用,Windows 上也能编译做单测
// (cpp/tests/rpc_controller_test/process_fd_metrics_test.cpp)。读 /proc 与 getrlimit 在 node.cpp(仅 Linux)。
namespace process_fd_metrics
{
	struct Snapshot
	{
		// 读不到时为空(非 Linux,或 /proc 不可读),对应的指标族整个不导出:导出 0 会被读成「一个 fd 都没开」。
		std::optional<uint64_t> openFds;
		std::optional<uint64_t> maxFds; // RLIMIT_NOFILE 软限;无界时是 open_file_limit::kUnlimited

		// 启动门槛检查(EnsureOpenFileLimitOrDie)的结果。只有 Linux 执行检查;没执行时这两项不导出。
		bool limitChecked{false};
		uint64_t requiredOpenFiles{0};
		bool exempted{false};

		uint64_t acceptDescriptorExhaustions{0};
		uint64_t acceptMemoryExhaustions{0};
		uint64_t acceptPauses{0};
	};

	// Linux 6.2 起,stat("/proc/<pid>/fd") 的 st_size 就是打开的 fd 数(O(1));更老的内核恒为 0,
	// 调用方要退回逐项数目录。gate 满载时有两万多个 fd,在唯一的 IO 线程上逐项数是毫秒级的停顿,所以能不数就不数。
	inline std::optional<uint64_t> OpenFdsFromProcDirSize(int64_t statSize)
	{
		if (statSize <= 0)
		{
			return std::nullopt;
		}
		return static_cast<uint64_t>(statSize);
	}

	// 逐项数 /proc/self/fd 得到的条数(不含 "." 与 "..")换成打开的 fd 数:遍历本身占着的那个目录 fd 不算。
	inline uint64_t OpenFdsFromDirEntryCount(uint64_t entries)
	{
		return entries > 0 ? entries - 1 : 0;
	}

	inline void Write(prometheus_text::Writer &writer, const Snapshot &snapshot)
	{
		using prometheus_text::MetricType;
		if (snapshot.openFds)
		{
			writer.BeginFamily("process_open_fds", "Number of open file descriptors.", MetricType::kGauge);
			writer.Sample({}, *snapshot.openFds);
		}
		if (snapshot.maxFds)
		{
			writer.BeginFamily("process_max_fds", "Maximum number of open file descriptors (RLIMIT_NOFILE soft limit).",
							   MetricType::kGauge);
			writer.Sample({}, *snapshot.maxFds);
		}
		if (snapshot.limitChecked)
		{
			writer.BeginFamily("cpp_node_open_file_limit_required",
							   "Open files this node needs at startup: client-facing connection cap plus the non-client reserve.",
							   MetricType::kGauge);
			writer.Sample({}, snapshot.requiredOpenFiles);
			writer.BeginFamily("cpp_node_open_file_limit_exempted",
							   "1 when the node started below its required open file limit because "
							   "NODE_OPEN_FILE_LIMIT_MODE=warn; 0 otherwise.",
							   MetricType::kGauge);
			writer.Sample({}, snapshot.exempted ? uint64_t{1} : uint64_t{0});
		}
		// 两个 accept 计数任何平台都导出,进程一启动就是 0:increase() 抓得到第一次。
		writer.BeginFamily("cpp_node_accept_exhausted_total",
						   "accept() calls that failed because a resource ran out: reason=descriptors (EMFILE / ENFILE) "
						   "or reason=memory (ENOBUFS / ENOMEM). The listener sheds or pauses instead of aborting.",
						   MetricType::kCounter);
		writer.Sample({{"reason", "descriptors"}}, snapshot.acceptDescriptorExhaustions);
		writer.Sample({{"reason", "memory"}}, snapshot.acceptMemoryExhaustions);
		writer.BeginFamily("cpp_node_accept_paused_total",
						   "Times a listener stopped accepting for a short interval because it could neither accept nor "
						   "shed a pending connection.",
						   MetricType::kCounter);
		writer.Sample({}, snapshot.acceptPauses);
	}
} // namespace process_fd_metrics
