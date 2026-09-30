#pragma once

#include <chrono>
#include <memory>
#include <string>

#include "agones/agones_rest_client.h"
#include "node/system/node/client_endpoint.h"

// CLIENT_ENDPOINT_SOURCE=agones 时的客户端地址来源(集群外入口 D79 / D81)。
//
// 做什么:在 etcd 发布之前,向本机 Agones sidecar `GET /gameserver`,取
//   status.address + status.ports[name == "client"] 作为本进程自报的客户端可达地址。
//   CLIENT_ENDPOINT_HOST 的 host 覆盖(kind 填 127.0.0.1)不在这里做,由
//   client_endpoint::Resolve 统一处理 —— 规则只有一份权威实现。
//
// 失败语义(fail-closed):address 为空、没有 "client" 端口(或端口不在 1..65535)、
//   sidecar 不可达 / 非 2xx / JSON 解析失败,都算本次失败并退避重试;次数上限或时间预算
//   先到者耗尽后 Fetch 返回 false,调用方(Node::InitRpcServer)LOG_FATAL 退出,由 Agones 重建 Pod。
//
// 线程与阻塞:Fetch 同步阻塞,最坏约 totalBudget(默认 60s,对应 D79 的"最坏约 60s")。
//   预算按 steady_clock 截止时间计:退避睡眠截到剩余预算,单次 GET 的超时也截到剩余预算,
//   只可能再超出毫秒级的调度 / curl 计时误差。
//   只在 Node 构造阶段(EventLoop 尚未 loop())调用,绝不能在运行中的 EventLoop 上调用。
//
// 所有权:本对象独占自己的 transport 与 REST client,不与 GameServerLifecycle 共享
//   (两者生命周期不同:地址解析在构造期一次性完成,lifecycle worker 在 SetAfterStart 才起)。

namespace agones
{

	// Agones 端口名。Fleet 模板 spec.ports 只列这一个(D81);与部署生成器
	// (tools/scripts/lib/k8s_client_entry.ps1)是跨语言字符串契约,改名必须两边同改。
	inline constexpr char kClientPortName[] = "client";

	struct ClientEndpointSourceOptions
	{
		// 单次 GET 的超时;实际使用时再截到剩余预算。两项都必须 > 0。
		HttpTimeouts timeouts{};
		// 次数上限:30 次,退避 200ms 起翻倍、封顶 2s(与 Ready 重试同一口径)。sidecar 立即拒绝
		// 连接时退避合计约 53s;但 sidecar 可连却不响应时每次 GET 还要吃满 timeouts.total,
		// 只靠次数会拖到约 30 × 2s + 53s ≈ 113s,所以另设总预算兜底。
		int maxAttempts{ 30 };
		std::chrono::milliseconds initialBackoff{ 200 };
		std::chrono::milliseconds maxBackoff{ 2000 };
		// 总时间预算(单调时钟截止时间,AGENTS §11.3),与次数上限同时生效,先到者为准。
		std::chrono::milliseconds totalBudget{ 60000 };
	};

	class AgonesClientEndpointSource final : public client_endpoint::ExternalSource
	{
	public:
		// transport 为空时不崩:Fetch 恒返回 false,由调用方 fail-closed。
		AgonesClientEndpointSource(std::unique_ptr<HttpTransport> transport,
			std::string baseUrl,
			ClientEndpointSourceOptions options);
		~AgonesClientEndpointSource() override;

		AgonesClientEndpointSource(const AgonesClientEndpointSource&) = delete;
		AgonesClientEndpointSource& operator=(const AgonesClientEndpointSource&) = delete;

		// 有界重试直到拿到可用地址。成功时 out.host 非空、out.port 在 1..65535。
		// 失败时 error 写明是次数耗尽还是时间预算耗尽,以及最后一次失败的原因。
		bool Fetch(client_endpoint::Advertised& out, std::string& error) override;

	private:
		bool FetchOnce(const HttpTimeouts& timeouts, client_endpoint::Advertised& out, std::string& error);

		std::unique_ptr<HttpTransport> transport_;
		// 引用 *transport_,声明顺序保证它先于 transport_ 析构。
		std::unique_ptr<AgonesRestClient> client_;
		ClientEndpointSourceOptions options_;
	};

	// 按进程环境构造 Agones 地址来源,供节点的 THooks::ClientEndpointSourceFactory 直接返回。
	// ReadAgonesEnv 判定未启用(无 AGONES_SDK_HTTP_PORT / AGONES_ENABLED=0),或本构建没有
	// HTTP 传输层(Windows / 未定义 MMORPG_AGONES_CURL)时返回 nullptr 并打 ERROR 日志;
	// Node 见到 nullptr 即 LOG_FATAL(配了 agones 来源却不在 Agones 里跑是部署错误)。
	std::unique_ptr<client_endpoint::ExternalSource> MakeClientEndpointSourceFromEnv();

} // namespace agones
