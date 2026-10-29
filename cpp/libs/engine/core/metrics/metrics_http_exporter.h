#pragma once

#include <cstdint>
#include <functional>
#include <optional>
#include <string>

#include "muduo/base/noncopyable.h"
#include "muduo/net/http/HttpServer.h"

namespace muduo::net
{
	class EventLoop;
}

// C++ 节点的 Prometheus 出口:GET /metrics 返回 render() 的文本,其余路径 404。
//
// 用 muduo 自带的 HttpServer,跑在给定的 EventLoop 上,不另起线程 —— 采集回调因此与业务逻辑同线程
// (metrics_registry.h 的约束)。不引第三方库:抓取只要求文本格式 0.0.4,现有能力足够(AGENTS.md §11.2)。
//
// 生命周期:由持有者(Node)创建、启动、销毁,都在 loop 线程上(muduo TcpServer 的要求)。HTTP 回调只捕获
// render 的拷贝,不捕获 this(AGENTS.md §11.7)。HttpServer 内部绑定自身的回调与它同寿命。
//
// 开关与端口:环境变量 MMORPG_METRICS_PORT(见 ParseMetricsPortEnv)。未设 = 不开出口;同一主机上多个进程要用
// 不同端口(K8s 每个 Pod 一个 IP,按角色固定端口即可)。端口被占用时 muduo 绑定失败会让进程在启动期退出,
// 这是有意的:配置错就别带着看不见的指标上线。
class MetricsHttpExporter : muduo::noncopyable
{
public:
	using Render = std::function<std::string()>;

	MetricsHttpExporter(muduo::net::EventLoop *loop, uint16_t port, Render render);

	// 开始监听。只调一次。
	void Start();
	uint16_t Port() const { return port_; }

private:
	uint16_t port_;
	muduo::net::HttpServer server_;
};

inline constexpr const char *kMetricsPortEnv = "MMORPG_METRICS_PORT";

// 解析 MMORPG_METRICS_PORT 的原文:
//   nullptr / 空串 → 0(不开出口);
//   规范十进制 1..65535 → 端口;
//   其它(非数字、前导零、0、越界、带空白)→ nullopt,调用方按启动配置错误拒启。
std::optional<uint16_t> ParseMetricsPortEnv(const char *raw);
