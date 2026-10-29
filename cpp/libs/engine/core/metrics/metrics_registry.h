#pragma once

#include <functional>
#include <mutex>
#include <string>
#include <utility>
#include <vector>

#include "metrics/prometheus_text.h"

// 进程级指标注册表:每个模块登记一个「抓取时把自己的指标写进 Writer」的采集回调,
// /metrics 出口(metrics_http_exporter.h)每次抓取按登记顺序调一遍。
//
// 采集回调的约束:
//   - 在出口所在的 EventLoop 线程执行。C++ 节点的出口挂在 Node 的主 loop 上,所以回调可以读本线程的
//     thread_local 状态(如 scene 的 tlsEcs)和 relaxed 原子计数;
//   - 只读、不阻塞、不发 I/O,也不在回调里登记 / 注销;
//   - 只用低基数标签,玩家号不许做标签(AGENTS.md §9)。
// 现有模块的计数一律保持「relaxed 原子 + 30s 汇总日志」原样,这里只是多一个读者:日志与指标读同一份计数。
class MetricsRegistry
{
public:
	using Collector = std::function<void(prometheus_text::Writer &)>;

	// 同名重复登记返回 false,不覆盖:两个模块抢同一个名字是装配错误,调用方应打 ERROR。
	bool Register(std::string name, Collector collector);
	// 按登记顺序调用全部采集回调,返回整页文本。回调在锁外执行(先拷贝列表)。
	std::string Render() const;

private:
	mutable std::mutex mutex_;
	std::vector<std::pair<std::string, Collector>> collectors_;
};

// 进程唯一的注册表(函数内静态,首次使用时构造)。
MetricsRegistry &GlobalMetricsRegistry();
