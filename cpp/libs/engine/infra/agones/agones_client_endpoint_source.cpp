#include "agones/agones_client_endpoint_source.h"

#include <algorithm>
#include <chrono>
#include <string>
#include <thread>
#include <utility>

#include "agones/agones_gameserver_status.h"
#include "muduo/base/Logging.h"

namespace agones
{

	namespace
	{
		// 单次 GET 至少要留的预算。curl 的超时以毫秒计且 0 表示不限时,不足 1ms 就不再发请求。
		constexpr std::chrono::milliseconds kMinAttemptBudget{ 1 };

		// 距截止时间的剩余毫秒数(向下取整;已过期为 0)。
		std::chrono::milliseconds RemainingUntil(std::chrono::steady_clock::time_point deadline)
		{
			const auto left = std::chrono::duration_cast<std::chrono::milliseconds>(
				deadline - std::chrono::steady_clock::now());
			return std::max(left, std::chrono::milliseconds::zero());
		}

		// 单次 GET 的超时截到剩余预算。调用方保证 remaining >= kMinAttemptBudget,
		// 且配置的两项超时都 > 0,因此结果两项都 > 0。
		HttpTimeouts ClampToBudget(const HttpTimeouts& timeouts, std::chrono::milliseconds remaining)
		{
			HttpTimeouts clamped;
			clamped.total = std::min(timeouts.total, remaining);
			clamped.connect = std::min(timeouts.connect, clamped.total);
			return clamped;
		}
	} // namespace

	AgonesClientEndpointSource::AgonesClientEndpointSource(std::unique_ptr<HttpTransport> transport,
		std::string baseUrl,
		ClientEndpointSourceOptions options)
		: transport_(std::move(transport)), options_(options)
	{
		if (transport_ != nullptr)
		{
			client_ = std::make_unique<AgonesRestClient>(std::move(baseUrl), *transport_, options_.timeouts);
		}
	}

	AgonesClientEndpointSource::~AgonesClientEndpointSource() = default;

	bool AgonesClientEndpointSource::Fetch(client_endpoint::Advertised& out, std::string& error)
	{
		if (client_ == nullptr)
		{
			error = "agones client endpoint source has no HTTP transport";
			return false;
		}

		// 次数上限与时间预算同时生效,先到者为准。预算用单调时钟截止时间表达(AGENTS §11.3),
		// 不用"次数 × sleep"代替:sidecar 可连却不响应时,每次 GET 都会吃满自己的超时。
		const std::chrono::steady_clock::time_point deadline = std::chrono::steady_clock::now() + options_.totalBudget;
		auto backoff = options_.initialBackoff;
		std::string lastError = "no attempt was made";
		int attempts = 0;
		bool budgetExhausted = false;
		while (attempts < options_.maxAttempts)
		{
			const std::chrono::milliseconds remaining = RemainingUntil(deadline);
			if (remaining < kMinAttemptBudget)
			{
				budgetExhausted = true;
				break;
			}

			++attempts;
			client_endpoint::Advertised candidate;
			if (FetchOnce(ClampToBudget(options_.timeouts, remaining), candidate, lastError))
			{
				LOG_INFO << "[Agones] client endpoint from GameServer status: host=" << candidate.host
						 << " port=" << candidate.port << " after " << attempts << " attempt(s)";
				out = std::move(candidate);
				return true;
			}

			LOG_WARN << "[Agones] client endpoint attempt " << attempts << "/" << options_.maxAttempts
					 << " failed: " << lastError;
			if (attempts < options_.maxAttempts)
			{
				// 只在 Node 构造阶段调用,阻塞的是启动线程,不是运行中的 EventLoop(见头文件)。
				// 睡眠截到剩余预算;睡完预算耗尽由循环顶部的检查收口。
				std::this_thread::sleep_for(std::min(backoff, RemainingUntil(deadline)));
				backoff = std::min(backoff * 2, options_.maxBackoff);
			}
		}

		if (budgetExhausted)
		{
			error = "agones client endpoint unavailable: time budget of "
				+ std::to_string(options_.totalBudget.count()) + "ms exhausted after "
				+ std::to_string(attempts) + " attempt(s): " + lastError;
		}
		else
		{
			error = "agones client endpoint unavailable after " + std::to_string(attempts)
				+ " attempt(s): " + lastError;
		}
		return false;
	}

	bool AgonesClientEndpointSource::FetchOnce(const HttpTimeouts& timeouts,
		client_endpoint::Advertised& out,
		std::string& error)
	{
		const HttpResponse response = client_->GameServer(timeouts);
		if (!response.Ok())
		{
			error = "GET /gameserver failed (status=" + std::to_string(response.statusCode)
				+ " error=" + response.error + ")";
			return false;
		}

		GameServerView view;
		if (!ParseGameServer(response.body, view, error))
		{
			return false;
		}
		if (view.address.empty())
		{
			// Pod 还没调度到节点时 Agones 尚未填地址,属于可重试的暂态。
			error = "GameServer status.address is empty (state=" + view.state + ")";
			return false;
		}
		const auto port = view.ports.find(kClientPortName);
		if (port == view.ports.end())
		{
			error = std::string("GameServer status.ports has no port named '") + kClientPortName
				+ "' (state=" + view.state + ")";
			return false;
		}
		// ParseGameServer 已保证 <= 65535;0 表示 Agones 还没分配端口,同样可重试。
		if (port->second == 0)
		{
			error = std::string("GameServer port '") + kClientPortName + "' is not in 1..65535: "
				+ std::to_string(port->second);
			return false;
		}

		out.host = view.address;
		out.port = port->second;
		return true;
	}

	std::unique_ptr<client_endpoint::ExternalSource> MakeClientEndpointSourceFromEnv()
	{
		const AgonesEnv env = ReadAgonesEnv();
		if (!env.enabled)
		{
			LOG_ERROR << "[Agones] CLIENT_ENDPOINT_SOURCE=agones but Agones SDK is not enabled "
					  << "(AGONES_SDK_HTTP_PORT missing/invalid or AGONES_ENABLED=0)";
			return nullptr;
		}

		auto transport = MakeDefaultHttpTransport();
		if (transport == nullptr)
		{
			LOG_ERROR << "[Agones] CLIENT_ENDPOINT_SOURCE=agones but this build has no HTTP transport "
					  << "(MMORPG_AGONES_CURL not defined)";
			return nullptr;
		}

		return std::make_unique<AgonesClientEndpointSource>(
			std::move(transport), env.BaseUrl(), ClientEndpointSourceOptions{});
	}

} // namespace agones
