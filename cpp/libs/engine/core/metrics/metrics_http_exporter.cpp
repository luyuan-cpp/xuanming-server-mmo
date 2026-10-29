#include "metrics/metrics_http_exporter.h"

#include <string_view>
#include <utility>

#include "muduo/base/Logging.h"
#include "muduo/net/InetAddress.h"
#include "muduo/net/http/HttpRequest.h"
#include "muduo/net/http/HttpResponse.h"

MetricsHttpExporter::MetricsHttpExporter(muduo::net::EventLoop *loop, uint16_t port, Render render)
	: port_(port),
	  server_(loop, muduo::net::InetAddress(port), "metrics")
{
	server_.setHttpCallback([render = std::move(render)](const muduo::net::HttpRequest &request,
														 muduo::net::HttpResponse *response)
	{
		if (request.method() != muduo::net::HttpRequest::kGet || request.path() != "/metrics")
		{
			response->setStatusCode(muduo::net::HttpResponse::k404NotFound);
			response->setStatusMessage("Not Found");
			response->setCloseConnection(true);
			return;
		}
		response->setStatusCode(muduo::net::HttpResponse::k200Ok);
		response->setStatusMessage("OK");
		response->setContentType("text/plain; version=0.0.4; charset=utf-8");
		response->setBody(render());
	});
}

void MetricsHttpExporter::Start()
{
	server_.start();
	LOG_INFO << "[Metrics] Prometheus exporter listening on 0.0.0.0:" << port_ << "/metrics";
}

std::optional<uint16_t> ParseMetricsPortEnv(const char *raw)
{
	if (raw == nullptr || raw[0] == '\0')
	{
		return uint16_t{0};
	}
	const std::string_view text(raw);
	if (text.size() > 5 || text.front() == '0')
	{
		return std::nullopt;
	}
	uint32_t port = 0;
	for (const char c : text)
	{
		if (c < '0' || c > '9')
		{
			return std::nullopt;
		}
		port = port * 10 + static_cast<uint32_t>(c - '0');
	}
	if (port == 0 || port > 65535)
	{
		return std::nullopt;
	}
	return static_cast<uint16_t>(port);
}
