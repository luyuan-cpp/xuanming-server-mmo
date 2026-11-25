#include "agones/agones_rest_client.h"

#include <cstdlib>
#include <mutex>
#include <string>
#include <utility>

#include "muduo/base/Logging.h"

#if defined(MMORPG_AGONES_CURL)
#include <curl/curl.h>
#endif

namespace agones
{

	namespace
	{
		const char* GetNonEmptyEnvValue(const char* name)
		{
			const char* value = std::getenv(name);
			if (value == nullptr || value[0] == '\0')
			{
				return nullptr;
			}
			return value;
		}
	} // namespace

	HttpResponse HttpTransport::Post(const std::string& url,
		const std::string& body,
		const HttpTimeouts& timeouts,
		std::stop_token /*cancel*/)
	{
		return Post(url, body, timeouts);
	}

	HttpResponse HttpTransport::Get(const std::string& url, const HttpTimeouts& timeouts, std::stop_token /*cancel*/)
	{
		return Get(url, timeouts);
	}

	std::string AgonesEnv::BaseUrl() const
	{
		return "http://" + host + ":" + std::to_string(httpPort);
	}

	AgonesEnv ReadAgonesEnv()
	{
		AgonesEnv env;

		// AGONES_ENABLED=0 是显式关闭开关:即使 Pod 里真的有 sidecar,也允许运维
		// 把某个 Deployment 退回非 Agones 行为,不用换镜像。
		if (const char* enabled = GetNonEmptyEnvValue("AGONES_ENABLED"))
		{
			if (std::string(enabled) == "0" || std::string(enabled) == "false")
			{
				return env; // enabled 保持 false
			}
		}

		const char* port = GetNonEmptyEnvValue("AGONES_SDK_HTTP_PORT");
		if (port == nullptr)
		{
			// 没有 sidecar 端口 = 不在 Agones 里跑(本地开发 / 普通 Deployment)。
			return env;
		}

		try
		{
			env.httpPort = std::stoi(port);
		}
		catch (const std::exception& ex)
		{
			LOG_WARN << "Invalid AGONES_SDK_HTTP_PORT '" << port << "': " << ex.what()
					 << ", Agones lifecycle stays disabled";
			return env;
		}

		if (env.httpPort <= 0 || env.httpPort > 65535)
		{
			LOG_WARN << "Out-of-range AGONES_SDK_HTTP_PORT " << env.httpPort
					 << ", Agones lifecycle stays disabled";
			env.httpPort = 0;
			return env;
		}

		if (const char* host = GetNonEmptyEnvValue("AGONES_SDK_HOST"))
		{
			env.host = host;
		}

		env.enabled = true;
		return env;
	}

#if defined(MMORPG_AGONES_CURL)

	namespace
	{
		std::once_flag gCurlInitOnce;

		void EnsureCurlGlobalInit()
		{
			// curl_global_init 不是线程安全的,必须只做一次。
			// 刻意不调 curl_global_cleanup:进程内可能还有别的 curl 使用者,
			// 而且退出时清理全局状态收益为零、踩 use-after-free 的风险不为零。
			std::call_once(gCurlInitOnce, [] { curl_global_init(CURL_GLOBAL_DEFAULT); });
		}

		size_t AppendToString(char* ptr, size_t size, size_t nmemb, void* userdata)
		{
			const size_t total = size * nmemb;
			auto* out = static_cast<std::string*>(userdata);
			out->append(ptr, total);
			return total;
		}

		// CURLOPT_XFERINFOFUNCTION:取消令牌置位即返回非 0,libcurl 以 CURLE_ABORTED_BY_CALLBACK 中止本次传输。
		int AbortTransferWhenCancelled(void* clientp, curl_off_t, curl_off_t, curl_off_t, curl_off_t)
		{
			const auto* cancel = static_cast<const std::stop_token*>(clientp);
			return cancel->stop_requested() ? 1 : 0;
		}
	} // namespace

	CurlAgonesHttpTransport::CurlAgonesHttpTransport()
	{
		EnsureCurlGlobalInit();
	}

	CurlAgonesHttpTransport::~CurlAgonesHttpTransport() = default;

	HttpResponse CurlAgonesHttpTransport::Perform(const std::string& url,
		const std::string* body,
		const HttpTimeouts& timeouts,
		std::stop_token cancel)
	{
		HttpResponse result;

		// 发起前已取消:不建连接,直接按传输失败返回。
		if (cancel.stop_requested())
		{
			result.error = "request cancelled before start";
			return result;
		}

		CURL* handle = curl_easy_init();
		if (handle == nullptr)
		{
			result.error = "curl_easy_init failed";
			return result;
		}

		curl_slist* headers = nullptr;
		if (body != nullptr)
		{
			headers = curl_slist_append(headers, "Content-Type: application/json");
		}

		curl_easy_setopt(handle, CURLOPT_URL, url.c_str());
		curl_easy_setopt(handle, CURLOPT_WRITEFUNCTION, &AppendToString);
		curl_easy_setopt(handle, CURLOPT_WRITEDATA, &result.body);
		curl_easy_setopt(handle, CURLOPT_NOSIGNAL, 1L);
		// 连接超时与总超时都必须设:只设总超时的话,连接阶段挂死同样会吃满整个预算。
		curl_easy_setopt(handle, CURLOPT_CONNECTTIMEOUT_MS,
			static_cast<long>(timeouts.connect.count()));
		curl_easy_setopt(handle, CURLOPT_TIMEOUT_MS, static_cast<long>(timeouts.total.count()));
		// sidecar 在 127.0.0.1,不需要也不应该走代理。
		curl_easy_setopt(handle, CURLOPT_PROXY, "");
		if (cancel.stop_possible())
		{
			// 在途取消:传输期间 libcurl 周期性调用进度回调(NOPROGRESS=0 才会调),令牌置位即中止。
			// 粒度:有数据收发时很密;空闲时(如 sidecar 收下请求迟迟不回)约每秒一次 ——
			// 因此中止最坏约 1s 生效,且不晚于本次请求自己的超时。cancel 是本函数的局部副本,活过 curl_easy_perform。
			curl_easy_setopt(handle, CURLOPT_XFERINFOFUNCTION, &AbortTransferWhenCancelled);
			curl_easy_setopt(handle, CURLOPT_XFERINFODATA, &cancel);
			curl_easy_setopt(handle, CURLOPT_NOPROGRESS, 0L);
		}

		if (body != nullptr)
		{
			curl_easy_setopt(handle, CURLOPT_POST, 1L);
			curl_easy_setopt(handle, CURLOPT_POSTFIELDS, body->c_str());
			curl_easy_setopt(handle, CURLOPT_POSTFIELDSIZE, static_cast<long>(body->size()));
			curl_easy_setopt(handle, CURLOPT_HTTPHEADER, headers);
		}

		const CURLcode code = curl_easy_perform(handle);
		if (code == CURLE_OK)
		{
			result.transportOk = true;
			long status = 0;
			curl_easy_getinfo(handle, CURLINFO_RESPONSE_CODE, &status);
			result.statusCode = status;
		}
		else
		{
			result.error = curl_easy_strerror(code);
		}

		if (headers != nullptr)
		{
			curl_slist_free_all(headers);
		}
		curl_easy_cleanup(handle);

		return result;
	}

	HttpResponse CurlAgonesHttpTransport::Post(const std::string& url,
		const std::string& body,
		const HttpTimeouts& timeouts)
	{
		return Perform(url, &body, timeouts, std::stop_token{});
	}

	HttpResponse CurlAgonesHttpTransport::Get(const std::string& url, const HttpTimeouts& timeouts)
	{
		return Perform(url, nullptr, timeouts, std::stop_token{});
	}

	HttpResponse CurlAgonesHttpTransport::Post(const std::string& url,
		const std::string& body,
		const HttpTimeouts& timeouts,
		std::stop_token cancel)
	{
		return Perform(url, &body, timeouts, std::move(cancel));
	}

	HttpResponse CurlAgonesHttpTransport::Get(const std::string& url,
		const HttpTimeouts& timeouts,
		std::stop_token cancel)
	{
		return Perform(url, nullptr, timeouts, std::move(cancel));
	}

#endif // MMORPG_AGONES_CURL

	std::unique_ptr<HttpTransport> MakeDefaultHttpTransport()
	{
#if defined(MMORPG_AGONES_CURL)
		return std::make_unique<CurlAgonesHttpTransport>();
#else
		// Windows / 本地开发构建没有 curl 依赖。返回 nullptr,调用方会转入
		// Disabled 模式,全程不发任何 HTTP —— 本机开发不需要装 Agones。
		return nullptr;
#endif
	}

	AgonesRestClient::AgonesRestClient(std::string baseUrl, HttpTransport& transport, HttpTimeouts timeouts)
		: baseUrl(std::move(baseUrl)), transport(transport), timeouts(timeouts)
	{
	}

	// Agones REST SDK 的这几个端点都接受空 JSON 对象作为 body。
	HttpResponse AgonesRestClient::Ready(std::stop_token cancel)
	{
		return transport.Post(baseUrl + "/ready", "{}", timeouts, std::move(cancel));
	}

	HttpResponse AgonesRestClient::Health(std::stop_token cancel)
	{
		return transport.Post(baseUrl + "/health", "{}", timeouts, std::move(cancel));
	}

	HttpResponse AgonesRestClient::Allocate(std::stop_token cancel)
	{
		return transport.Post(baseUrl + "/allocate", "{}", timeouts, std::move(cancel));
	}

	HttpResponse AgonesRestClient::Shutdown()
	{
		// 显式传空令牌(不可取消),理由见头文件。
		return transport.Post(baseUrl + "/shutdown", "{}", timeouts, std::stop_token{});
	}

	HttpResponse AgonesRestClient::GameServer(std::stop_token cancel)
	{
		return transport.Get(baseUrl + "/gameserver", timeouts, std::move(cancel));
	}

	HttpResponse AgonesRestClient::GameServer(const HttpTimeouts& timeouts)
	{
		return transport.Get(baseUrl + "/gameserver", timeouts);
	}

} // namespace agones
