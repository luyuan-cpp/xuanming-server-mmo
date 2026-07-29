#pragma once

#include <chrono>
#include <memory>
#include <string>

// Agones SDK Sidecar 的本机 REST 客户端。
//
// 为什么用 REST 而不是 Agones C++ SDK:
//   Agones 官方 C++ SDK 依赖 gRPC 生成的 SDK stub,会把 Agones 的 proto 版本
//   拖进本仓库的 gRPC/protobuf 依赖树里,与现有版本冲突风险高。Sidecar 在
//   127.0.0.1 上同时暴露 gRPC 和 HTTP,我们只需要 5 个无参数的 POST/GET,
//   用 HTTP 反而是依赖最小的选择。
//
// 线程约束(重要):
//   本文件里的所有调用都是**同步阻塞**的。只允许在 SceneLifecycle 的
//   lifecycle worker 线程里调用,绝对不能进 muduo EventLoop —— 一次网络
//   抖动就会卡住整帧逻辑。
//
// 参考:https://agones.dev/site/docs/guides/client-sdks/rest/

namespace agones
{

	// HTTP 结果。transportOk 表示"请求发出去并拿到了响应",与 HTTP 状态码分开,
	// 这样调用方能区分"连不上 sidecar"和"sidecar 拒绝了这次状态转换"。
	struct HttpResponse
	{
		bool transportOk = false;
		long statusCode = 0;
		std::string body;
		std::string error;

		bool Ok() const { return transportOk && statusCode >= 200 && statusCode < 300; }
	};

	// 连接超时与总超时必须都设,只设总超时的话 DNS/连接阶段挂死仍然会拖满超时。
	struct HttpTimeouts
	{
		std::chrono::milliseconds connect{ 500 };
		std::chrono::milliseconds total{ 2000 };
	};

	// 可注入的传输层。业务代码只依赖这个接口,不出现 CURL* / curl_easy_setopt。
	// 单元测试注入 fake 实现,因此测试不需要真的起 Agones sidecar。
	class HttpTransport
	{
	public:
		virtual ~HttpTransport() = default;

		virtual HttpResponse Post(const std::string& url,
			const std::string& body,
			const HttpTimeouts& timeouts) = 0;

		virtual HttpResponse Get(const std::string& url, const HttpTimeouts& timeouts) = 0;
	};

#if defined(MMORPG_AGONES_CURL)
	// libcurl 实现。只在 Linux(Agones 实际运行的平台)编译,链接系统 CURL::libcurl。
	// 所有 curl API 调用都封死在这个类的 .cpp 里。
	//
	// 用的是同步 easy API:Agones REST 只有极少量本机回环请求,引入 multi/事件
	// 循环没有收益,而 easy API 的错误处理路径更少出错。代价是调用会阻塞,所以
	// 只能跑在 lifecycle worker 线程上。
	class CurlAgonesHttpTransport final : public HttpTransport
	{
	public:
		CurlAgonesHttpTransport();
		~CurlAgonesHttpTransport() override;

		HttpResponse Post(const std::string& url,
			const std::string& body,
			const HttpTimeouts& timeouts) override;

		HttpResponse Get(const std::string& url, const HttpTimeouts& timeouts) override;

	private:
		// body == nullptr 表示 GET。
		HttpResponse Perform(const std::string& url, const std::string* body, const HttpTimeouts& timeouts);
	};
#endif

	// Linux 且开启 MMORPG_AGONES_CURL 时返回 libcurl 实现;
	// Windows / 本地开发构建返回 nullptr,由调用方转入 Disabled 模式。
	std::unique_ptr<HttpTransport> MakeDefaultHttpTransport();

	// 从进程环境读取 Agones sidecar 配置。
	//   AGONES_ENABLED=0        -> 显式关闭(即使 sidecar 存在)
	//   AGONES_SDK_HTTP_PORT    -> Agones 注入的 sidecar HTTP 端口,缺失即视为未启用
	//   AGONES_SDK_HOST         -> 可选,默认 127.0.0.1
	struct AgonesEnv
	{
		bool enabled = false;
		std::string host = "127.0.0.1";
		int httpPort = 0;

		std::string BaseUrl() const;
	};

	AgonesEnv ReadAgonesEnv();

	// Agones REST SDK 的薄封装。不持有传输层所有权。
	class AgonesRestClient
	{
	public:
		AgonesRestClient(std::string baseUrl, HttpTransport& transport, HttpTimeouts timeouts);

		HttpResponse Ready();
		HttpResponse Health();
		HttpResponse Allocate();
		HttpResponse Shutdown();
		HttpResponse GameServer();

		const std::string& BaseUrl() const { return baseUrl_; }

	private:
		std::string baseUrl_;
		HttpTransport& transport_;
		HttpTimeouts timeouts_;
	};

} // namespace agones
