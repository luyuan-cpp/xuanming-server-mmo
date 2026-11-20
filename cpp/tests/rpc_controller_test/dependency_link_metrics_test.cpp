// 依赖连接指标的纯逻辑单测(metrics/dependency_link_metrics.h,断线重连 R17)。
// 计数是进程级的,同一个测试进程里的 Redis / gRPC / 节点链路回环用例也会动它们,所以这里只比增量。
// 告警(deploy/k8s/cpp-node-dependency-alerts.yaml)按这里钉住的名字与标签写,改名后规则不报错、只会永远不响。

#include <gtest/gtest.h>

#include <cstdint>
#include <string>

#include "metrics/dependency_link_metrics.h"
#include "metrics/prometheus_text.h"

namespace
{
	using dependency_link_metrics::Dependency;
	using dependency_link_metrics::FailureReason;

	std::string Render()
	{
		prometheus_text::Writer writer;
		dependency_link_metrics::Write(writer);
		EXPECT_EQ(writer.DroppedSamples(), 0u);
		return writer.Text();
	}

	uint64_t Failures(Dependency dependency, FailureReason reason)
	{
		return dependency_link_metrics::Of(dependency).failures[static_cast<size_t>(reason)].load();
	}

	TEST(DependencyLinkMetrics, UpDownAndFailuresMoveOnlyTheirOwnCounters)
	{
		auto &etcd = dependency_link_metrics::Of(Dependency::kEtcd);
		const int64_t connections = etcd.connections.load();
		const uint64_t lost = etcd.lost.load();
		const uint64_t auth = Failures(Dependency::kEtcd, FailureReason::kAuth);
		const uint64_t grpcConnect = Failures(Dependency::kGrpc, FailureReason::kConnect);

		dependency_link_metrics::OnUp(Dependency::kEtcd);
		dependency_link_metrics::OnUp(Dependency::kEtcd);
		EXPECT_EQ(etcd.connections.load(), connections + 2);

		dependency_link_metrics::OnDown(Dependency::kEtcd, /*expected=*/true);
		EXPECT_EQ(etcd.lost.load(), lost) << "本端主动关(停机、换周期、GOAWAY 退役)不算意外断开";
		dependency_link_metrics::OnDown(Dependency::kEtcd, /*expected=*/false);
		EXPECT_EQ(etcd.lost.load(), lost + 1);
		EXPECT_EQ(etcd.connections.load(), connections) << "每次 OnUp 配一次 OnDown,gauge 回到原值";

		dependency_link_metrics::OnConnectFailed(Dependency::kEtcd, FailureReason::kAuth);
		EXPECT_EQ(Failures(Dependency::kEtcd, FailureReason::kAuth), auth + 1);
		EXPECT_EQ(Failures(Dependency::kGrpc, FailureReason::kConnect), grpcConnect) << "别的依赖、别的原因不动";
	}

	// 每类依赖、每个原因都以 0 起步导出:进程一启动就有序列,告警的 increase() 抓得到第一次。
	TEST(DependencyLinkMetrics, ExportsEveryDependencyAndReason)
	{
		const std::string text = Render();
		EXPECT_NE(text.find("# TYPE cpp_node_dependency_connections gauge\n"), std::string::npos);
		EXPECT_NE(text.find("# TYPE cpp_node_dependency_lost_total counter\n"), std::string::npos);
		EXPECT_NE(text.find("# TYPE cpp_node_dependency_connect_failures_total counter\n"), std::string::npos);
		for (const std::string_view name : dependency_link_metrics::kDependencyNames)
		{
			const std::string label = "{dependency=\"" + std::string(name) + "\"";
			EXPECT_NE(text.find("cpp_node_dependency_connections" + label + "}"), std::string::npos) << name;
			EXPECT_NE(text.find("cpp_node_dependency_lost_total" + label + "}"), std::string::npos) << name;
			for (const std::string_view reason : dependency_link_metrics::kFailureReasonNames)
			{
				EXPECT_NE(text.find("cpp_node_dependency_connect_failures_total" + label + ",reason=\"" + std::string(reason) + "\"}"),
						  std::string::npos)
					<< name << "/" << reason;
			}
		}
	}

	// 配对出错(不应发生)时 gauge 不能以 uint64 回绕成天文数字导出,按 0 导出。
	TEST(DependencyLinkMetrics, UnpairedDownIsExportedAsZeroNotAWrappedValue)
	{
		auto &other = dependency_link_metrics::Of(Dependency::kRedisOther);
		const int64_t before = other.connections.load();
		other.connections.store(-1);
		const std::string text = Render();
		other.connections.store(before);
		EXPECT_NE(text.find("cpp_node_dependency_connections{dependency=\"redis_other\"} 0\n"), std::string::npos);
	}
}
