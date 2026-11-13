// 进程 fd 用量与 accept 降级指标的纯逻辑单测(process_fd_metrics.h)。
// 读 /proc 与 getrlimit 在 node.cpp 里、只在 Linux 上跑,这里不碰;这里钉的是「取数规则」与「导出哪些指标族」:
// 告警(deploy/k8s/cpp-node-resource-alerts.yaml)按这些名字写,改名后规则不报错、只会永远不响。

#include <gtest/gtest.h>

#include <cstdint>
#include <string>

#include "metrics/prometheus_text.h"
#include "node/system/node/process_fd_metrics.h"

namespace
{
	std::string Render(const process_fd_metrics::Snapshot &snapshot)
	{
		prometheus_text::Writer writer;
		process_fd_metrics::Write(writer, snapshot);
		EXPECT_EQ(writer.DroppedSamples(), 0u);
		return writer.Text();
	}

	// Linux 6.2 之前 /proc/<pid>/fd 的 st_size 恒为 0:必须认成「内核不给」,由调用方退回逐项数,不能当成 0 个 fd。
	TEST(ProcessFdMetrics, ProcDirSizeIsUsedOnlyWhenKernelReportsIt)
	{
		EXPECT_FALSE(process_fd_metrics::OpenFdsFromProcDirSize(0).has_value());
		EXPECT_FALSE(process_fd_metrics::OpenFdsFromProcDirSize(-1).has_value());
		ASSERT_TRUE(process_fd_metrics::OpenFdsFromProcDirSize(37).has_value());
		EXPECT_EQ(*process_fd_metrics::OpenFdsFromProcDirSize(37), 37u);
	}

	// 逐项数的时候,遍历用的那个目录 fd 自己也在列表里,要扣掉。
	TEST(ProcessFdMetrics, DirEntryCountExcludesTheScanningDescriptor)
	{
		EXPECT_EQ(process_fd_metrics::OpenFdsFromDirEntryCount(0), 0u);
		EXPECT_EQ(process_fd_metrics::OpenFdsFromDirEntryCount(1), 0u);
		EXPECT_EQ(process_fd_metrics::OpenFdsFromDirEntryCount(25), 24u);
	}

	TEST(ProcessFdMetrics, WritesEveryFamilyWhenEverythingIsKnown)
	{
		process_fd_metrics::Snapshot snapshot;
		snapshot.openFds = 1234;
		snapshot.maxFds = 524288;
		snapshot.limitChecked = true;
		snapshot.requiredOpenFiles = 24096;
		snapshot.exempted = true;
		snapshot.acceptDescriptorExhaustions = 5;
		snapshot.acceptMemoryExhaustions = 2;
		snapshot.acceptPauses = 3;

		EXPECT_EQ(Render(snapshot),
				  "# HELP process_open_fds Number of open file descriptors.\n"
				  "# TYPE process_open_fds gauge\n"
				  "process_open_fds 1234\n"
				  "# HELP process_max_fds Maximum number of open file descriptors (RLIMIT_NOFILE soft limit).\n"
				  "# TYPE process_max_fds gauge\n"
				  "process_max_fds 524288\n"
				  "# HELP cpp_node_open_file_limit_required Open files this node needs at startup: client-facing "
				  "connection cap plus the non-client reserve.\n"
				  "# TYPE cpp_node_open_file_limit_required gauge\n"
				  "cpp_node_open_file_limit_required 24096\n"
				  "# HELP cpp_node_open_file_limit_exempted 1 when the node started below its required open file limit "
				  "because NODE_OPEN_FILE_LIMIT_MODE=warn; 0 otherwise.\n"
				  "# TYPE cpp_node_open_file_limit_exempted gauge\n"
				  "cpp_node_open_file_limit_exempted 1\n"
				  "# HELP cpp_node_accept_exhausted_total accept() calls that failed because a resource ran out: "
				  "reason=descriptors (EMFILE / ENFILE) or reason=memory (ENOBUFS / ENOMEM). The listener sheds or pauses "
				  "instead of aborting.\n"
				  "# TYPE cpp_node_accept_exhausted_total counter\n"
				  "cpp_node_accept_exhausted_total{reason=\"descriptors\"} 5\n"
				  "cpp_node_accept_exhausted_total{reason=\"memory\"} 2\n"
				  "# HELP cpp_node_accept_paused_total Times a listener stopped accepting for a short interval because "
				  "it could neither accept nor shed a pending connection.\n"
				  "# TYPE cpp_node_accept_paused_total counter\n"
				  "cpp_node_accept_paused_total 3\n");
	}

	// 读不到的量整族不导出:导出 0 会被读成「一个 fd 都没开」或「软限是 0」,比没有数据更糟。
	// accept 计数任何平台都导出,从 0 起:increase() 才抓得到第一次。
	TEST(ProcessFdMetrics, OmitsUnknownFamiliesButAlwaysExportsAcceptCounters)
	{
		const std::string text = Render(process_fd_metrics::Snapshot{});

		EXPECT_EQ(text.find("process_open_fds"), std::string::npos);
		EXPECT_EQ(text.find("process_max_fds"), std::string::npos);
		EXPECT_EQ(text.find("cpp_node_open_file_limit_required"), std::string::npos);
		EXPECT_EQ(text.find("cpp_node_open_file_limit_exempted"), std::string::npos);
		EXPECT_NE(text.find("cpp_node_accept_exhausted_total{reason=\"descriptors\"} 0\n"), std::string::npos);
		EXPECT_NE(text.find("cpp_node_accept_exhausted_total{reason=\"memory\"} 0\n"), std::string::npos);
		EXPECT_NE(text.find("cpp_node_accept_paused_total 0\n"), std::string::npos);
	}

	// 门槛检查过、没走豁免:exempted 要以 0 导出,告警按 == 1 判。
	TEST(ProcessFdMetrics, CheckedWithoutExemptionExportsZero)
	{
		process_fd_metrics::Snapshot snapshot;
		snapshot.limitChecked = true;
		snapshot.requiredOpenFiles = 4096;

		const std::string text = Render(snapshot);
		EXPECT_NE(text.find("cpp_node_open_file_limit_required 4096\n"), std::string::npos);
		EXPECT_NE(text.find("cpp_node_open_file_limit_exempted 0\n"), std::string::npos);
	}
} // namespace
