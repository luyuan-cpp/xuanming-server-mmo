// otlp_http_test —— OTLP/HTTP 导出的纯逻辑(docs/design/distributed-tracing.md §10.3):
//   * 地址:只认 http、端口缺省 4318、按信号的地址原样用(没路径用 "/")、基地址后接 v1/traces;凭据、查询串、IPv6 字面量等一律拒;
//   * 开关与地址的取值:OTEL_* 环境变量优先于配置,OTEL_SDK_DISABLED / OTEL_TRACES_EXPORTER=none 关,协议只认 http/protobuf,
//     报错点名变量、不回显地址;
//   * 请求报文逐字节;响应解析:Content-Length、chunked(含尾部字段)、Connection: close、HTTP/1.0、到关闭为止、1xx、204、
//     各种不合法的头。

#include <gtest/gtest.h>

#include <map>
#include <string>
#include <string_view>

#include "core/tracing/otlp_http.h"

namespace {

using tracing::HttpResponseParse;

tracing::EnvironmentLookup EnvOf(const std::map<std::string, std::string>& values)
{
    return [values](const char* name) -> const char* {
        const auto it = values.find(name);
        return it == values.end() ? nullptr : it->second.c_str();
    };
}

} // namespace

TEST(OtlpHttpEndpointTest, ParsesTracesEndpointsAsIs)
{
    auto endpoint = tracing::ParseOtlpTracesEndpoint("http://127.0.0.1:4318/v1/traces");
    ASSERT_TRUE(endpoint.has_value());
    EXPECT_EQ(endpoint->host, "127.0.0.1");
    EXPECT_EQ(endpoint->port, 4318);
    EXPECT_EQ(endpoint->path, "/v1/traces");
    EXPECT_EQ(endpoint->ToUrl(), "http://127.0.0.1:4318/v1/traces");

    endpoint = tracing::ParseOtlpTracesEndpoint("HTTP://pod-log-collector-otlp.mmorpg-infra.svc.cluster.local");
    ASSERT_TRUE(endpoint.has_value()) << "scheme 不区分大小写";
    EXPECT_EQ(endpoint->host, "pod-log-collector-otlp.mmorpg-infra.svc.cluster.local");
    EXPECT_EQ(endpoint->port, tracing::kOtlpHttpDefaultPort);
    EXPECT_EQ(endpoint->path, "/") << "按信号的地址原样使用,没有路径用 /";

    endpoint = tracing::ParseOtlpTracesEndpoint("http://alloy:14318/custom/path");
    ASSERT_TRUE(endpoint.has_value());
    EXPECT_EQ(endpoint->port, 14318);
    EXPECT_EQ(endpoint->path, "/custom/path");
}

TEST(OtlpHttpEndpointTest, RejectsWhatTheExporterCannotOrMustNotUse)
{
    for (const std::string_view url : {
             "",
             "https://alloy:4318/v1/traces",          // 只做明文
             "ftp://alloy/v1/traces",
             "http://",
             "http://:4318/v1/traces",
             "http://user:secret@alloy:4318/v1/traces", // 带凭据
             "http://alloy:0/v1/traces",
             "http://alloy:65536/v1/traces",
             "http://alloy:04318/v1/traces",           // 前导零
             "http://alloy:43a8/v1/traces",
             "http://[::1]:4318/v1/traces",            // IPv6 字面量
             "http://alloy:4318/v1/traces?x=1",
             "http://alloy:4318/v1/traces#frag",
             "http://alloy :4318/v1/traces",
             "http://alloy:4318/v1/tra ces",
             "http://-alloy:4318/",
             "http://al_loy:4318/",
         }) {
        EXPECT_FALSE(tracing::ParseOtlpTracesEndpoint(url).has_value()) << url;
    }
}

TEST(OtlpHttpEndpointTest, BaseEndpointGetsTheTracesPathAppended)
{
    for (const auto& [base, path] : std::map<std::string, std::string>{
             {"http://alloy:4318", "/v1/traces"},
             {"http://alloy:4318/", "/v1/traces"},
             {"http://alloy:4318/otlp", "/otlp/v1/traces"},
             {"http://alloy:4318/otlp/", "/otlp/v1/traces"},
         }) {
        const auto endpoint = tracing::ParseOtlpBaseEndpoint(base);
        ASSERT_TRUE(endpoint.has_value()) << base;
        EXPECT_EQ(endpoint->path, path) << base;
    }
    EXPECT_FALSE(tracing::ParseOtlpBaseEndpoint("https://alloy:4318").has_value());
}

TEST(OtlpExportSettingsTest, NothingConfiguredMeansDisabledWithoutError)
{
    const auto settings = tracing::ResolveOtlpExportSettings("", EnvOf({}));
    EXPECT_FALSE(settings.enabled);
    EXPECT_TRUE(settings.error.empty());
    EXPECT_TRUE(settings.source.empty());
    EXPECT_FALSE(tracing::ResolveOtlpExportSettings("", tracing::EnvironmentLookup{}).enabled) << "不给环境读法也能用";
}

TEST(OtlpExportSettingsTest, EnvironmentOverridesTheConfigFileInOtelOrder)
{
    const std::string config = "http://127.0.0.1:4318/v1/traces";
    auto settings = tracing::ResolveOtlpExportSettings(config, EnvOf({}));
    ASSERT_TRUE(settings.enabled);
    EXPECT_EQ(settings.source, "Tracing.OtlpTracesEndpoint");
    EXPECT_EQ(settings.endpoint.host, "127.0.0.1");

    settings = tracing::ResolveOtlpExportSettings(config, EnvOf({{"OTEL_EXPORTER_OTLP_ENDPOINT", "http://base:4318/otlp"}}));
    ASSERT_TRUE(settings.enabled);
    EXPECT_EQ(settings.source, "OTEL_EXPORTER_OTLP_ENDPOINT");
    EXPECT_EQ(settings.endpoint.host, "base");
    EXPECT_EQ(settings.endpoint.path, "/otlp/v1/traces");

    settings = tracing::ResolveOtlpExportSettings(
        config, EnvOf({{"OTEL_EXPORTER_OTLP_ENDPOINT", "http://base:4318"},
                       {"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://signal:9999/v1/traces"}}));
    ASSERT_TRUE(settings.enabled);
    EXPECT_EQ(settings.source, "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT");
    EXPECT_EQ(settings.endpoint.host, "signal");
    EXPECT_EQ(settings.endpoint.port, 9999);
}

TEST(OtlpExportSettingsTest, DisableSwitchesWinOverEveryEndpoint)
{
    const std::map<std::string, std::string> endpoint{{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://signal:4318/v1/traces"}};
    auto env = endpoint;
    env["OTEL_SDK_DISABLED"] = " TRUE ";
    auto settings = tracing::ResolveOtlpExportSettings("http://127.0.0.1:4318/v1/traces", EnvOf(env));
    EXPECT_FALSE(settings.enabled);
    EXPECT_TRUE(settings.error.empty());
    EXPECT_EQ(settings.source, "OTEL_SDK_DISABLED");

    env = endpoint;
    env["OTEL_TRACES_EXPORTER"] = "none";
    settings = tracing::ResolveOtlpExportSettings("", EnvOf(env));
    EXPECT_FALSE(settings.enabled);
    EXPECT_EQ(settings.source, "OTEL_TRACES_EXPORTER");

    env["OTEL_TRACES_EXPORTER"] = "otlp";
    EXPECT_TRUE(tracing::ResolveOtlpExportSettings("", EnvOf(env)).enabled);

    env["OTEL_SDK_DISABLED"] = "false";
    EXPECT_TRUE(tracing::ResolveOtlpExportSettings("", EnvOf(env)).enabled) << "只有 true 才关";
}

TEST(OtlpExportSettingsTest, UnsupportedChoicesAreConfigurationErrorsThatDoNotEchoTheValue)
{
    auto settings = tracing::ResolveOtlpExportSettings("", EnvOf({{"OTEL_TRACES_EXPORTER", "zipkin"}}));
    EXPECT_FALSE(settings.enabled);
    EXPECT_NE(settings.error.find("OTEL_TRACES_EXPORTER"), std::string::npos);

    settings = tracing::ResolveOtlpExportSettings("http://127.0.0.1:4318/v1/traces",
                                                  EnvOf({{"OTEL_EXPORTER_OTLP_PROTOCOL", "grpc"}}));
    EXPECT_FALSE(settings.enabled);
    EXPECT_NE(settings.error.find("OTEL_EXPORTER_OTLP_PROTOCOL"), std::string::npos);

    settings = tracing::ResolveOtlpExportSettings(
        "http://127.0.0.1:4318/v1/traces",
        EnvOf({{"OTEL_EXPORTER_OTLP_PROTOCOL", "grpc"}, {"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "http/protobuf"}}));
    EXPECT_TRUE(settings.enabled) << "按信号的协议优先于通用的";

    settings = tracing::ResolveOtlpExportSettings(
        "", EnvOf({{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://user:topsecret@alloy:4318/v1/traces"}}));
    EXPECT_FALSE(settings.enabled);
    EXPECT_NE(settings.error.find("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"), std::string::npos);
    EXPECT_EQ(settings.error.find("topsecret"), std::string::npos) << "报错不回显地址";

    settings = tracing::ResolveOtlpExportSettings("https://alloy:4318/v1/traces", EnvOf({}));
    EXPECT_FALSE(settings.enabled);
    EXPECT_NE(settings.error.find("Tracing.OtlpTracesEndpoint"), std::string::npos);
}

TEST(OtlpHttpRequestTest, BuildsAnHttp11ProtobufPost)
{
    const auto endpoint = tracing::ParseOtlpTracesEndpoint("http://127.0.0.1:4318/v1/traces");
    ASSERT_TRUE(endpoint.has_value());
    const std::string body("ab\0c", 4);   // 正文是二进制,可以带 NUL
    EXPECT_EQ(tracing::BuildOtlpHttpRequest(*endpoint, body),
              std::string("POST /v1/traces HTTP/1.1\r\n"
                          "Host: 127.0.0.1:4318\r\n"
                          "User-Agent: mmorpg-cpp-otlp/1.0\r\n"
                          "Content-Type: application/x-protobuf\r\n"
                          "Content-Length: 4\r\n"
                          "\r\n") +
                  body);

    const auto onPort80 = tracing::ParseOtlpTracesEndpoint("http://collector:80/v1/traces");
    ASSERT_TRUE(onPort80.has_value());
    const std::string request = tracing::BuildOtlpHttpRequest(*onPort80, "");
    EXPECT_NE(request.find("\r\nHost: collector\r\n"), std::string::npos) << "80 端口不写进 Host";
    EXPECT_NE(request.find("\r\nContent-Length: 0\r\n"), std::string::npos);
}

TEST(HttpResponseParseTest, ContentLengthResponseCompletesOnlyWhenTheBodyIsComplete)
{
    const std::string full = "HTTP/1.1 200 OK\r\nContent-Type: application/x-protobuf\r\nContent-Length: 2\r\n\r\nab";
    for (size_t cut = 0; cut < full.size(); ++cut) {
        const HttpResponseParse partial = tracing::ParseHttpResponse(std::string_view(full).substr(0, cut));
        EXPECT_EQ(partial.state, HttpResponseParse::State::kNeedMore) << cut;
    }
    const HttpResponseParse parsed = tracing::ParseHttpResponse(full);
    ASSERT_EQ(parsed.state, HttpResponseParse::State::kComplete);
    EXPECT_EQ(parsed.statusCode, 200);
    EXPECT_TRUE(parsed.keepAlive);
    EXPECT_EQ(parsed.consumed, full.size());

    const HttpResponseParse withExtra = tracing::ParseHttpResponse(full + "HTTP/1.1");
    ASSERT_EQ(withExtra.state, HttpResponseParse::State::kComplete);
    EXPECT_EQ(withExtra.consumed, full.size()) << "多出的字节不算在这条响应里";
}

TEST(HttpResponseParseTest, ChunkedBodiesWithTrailersAndExtensions)
{
    const std::string chunked =
        "HTTP/1.1 400 Bad Request\r\nTransfer-Encoding: gzip, chunked\r\n\r\n2;ext=1\r\nab\r\n1\r\nc\r\n0\r\nX-T: y\r\n\r\n";
    const HttpResponseParse parsed = tracing::ParseHttpResponse(chunked);
    ASSERT_EQ(parsed.state, HttpResponseParse::State::kComplete);
    EXPECT_EQ(parsed.statusCode, 400);
    EXPECT_EQ(parsed.consumed, chunked.size());

    EXPECT_EQ(tracing::ParseHttpResponse(chunked.substr(0, chunked.size() - 2)).state, HttpResponseParse::State::kNeedMore);
    EXPECT_EQ(tracing::ParseHttpResponse("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\nzz\r\n").state,
              HttpResponseParse::State::kError);
    EXPECT_EQ(tracing::ParseHttpResponse("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n2\r\nabX\r\n").state,
              HttpResponseParse::State::kError)
        << "块数据后面必须紧跟 CRLF";
}

TEST(HttpResponseParseTest, ConnectionReuseFollowsHttpVersionAndConnectionHeader)
{
    auto parsed = tracing::ParseHttpResponse("HTTP/1.1 200 OK\r\nConnection: close\r\nContent-Length: 0\r\n\r\n");
    ASSERT_EQ(parsed.state, HttpResponseParse::State::kComplete);
    EXPECT_FALSE(parsed.keepAlive);

    parsed = tracing::ParseHttpResponse("HTTP/1.0 200 OK\r\nContent-Length: 0\r\n\r\n");
    ASSERT_EQ(parsed.state, HttpResponseParse::State::kComplete);
    EXPECT_FALSE(parsed.keepAlive) << "HTTP/1.0 缺省不复用";

    parsed = tracing::ParseHttpResponse("HTTP/1.0 200 OK\r\nConnection: Keep-Alive\r\nContent-Length: 0\r\n\r\n");
    ASSERT_EQ(parsed.state, HttpResponseParse::State::kComplete);
    EXPECT_TRUE(parsed.keepAlive);

    parsed = tracing::ParseHttpResponse("HTTP/1.1 503 Service Unavailable\r\nRetry-After: 1\r\n\r\npartial");
    EXPECT_EQ(parsed.state, HttpResponseParse::State::kNeedMore);
    EXPECT_TRUE(parsed.headersComplete);
    EXPECT_TRUE(parsed.bodyUntilClose) << "没长度也不是 chunked:到连接关闭为止";
    EXPECT_FALSE(parsed.keepAlive);
    EXPECT_EQ(parsed.statusCode, 503);
}

TEST(HttpResponseParseTest, InterimAndBodylessResponses)
{
    const std::string interim = "HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 204 No Content\r\n\r\n";
    const auto parsed = tracing::ParseHttpResponse(interim);
    ASSERT_EQ(parsed.state, HttpResponseParse::State::kComplete);
    EXPECT_EQ(parsed.statusCode, 204);
    EXPECT_EQ(parsed.consumed, interim.size());

    const auto onlyInterim = tracing::ParseHttpResponse("HTTP/1.1 100 Continue\r\n\r\n");
    EXPECT_EQ(onlyInterim.state, HttpResponseParse::State::kNeedMore);
    EXPECT_FALSE(onlyInterim.headersComplete);

    const auto noReason = tracing::ParseHttpResponse("HTTP/1.1 200\r\nContent-Length: 0\r\n\r\n");
    ASSERT_EQ(noReason.state, HttpResponseParse::State::kComplete) << "原因短语可以省略";
    EXPECT_EQ(noReason.statusCode, 200);
}

TEST(HttpResponseParseTest, MalformedResponsesAreErrors)
{
    for (const std::string_view bad : {
             "HTP/1.1 200 OK\r\n\r\n",
             "HTTP/2 200 OK\r\n\r\n",
             "HTTP/1.1 2x0 OK\r\n\r\n",
             "HTTP/1.1 200OK\r\n\r\n",
             "HTTP/1.1 200 OK\r\nNoColonHere\r\n\r\n",
             "HTTP/1.1 200 OK\r\nContent-Length: 1\r\nContent-Length: 2\r\n\r\n",
             "HTTP/1.1 200 OK\r\nContent-Length: -1\r\n\r\n",
             "HTTP/1.1 101 Switching Protocols\r\n\r\n",
         }) {
        EXPECT_EQ(tracing::ParseHttpResponse(bad).state, HttpResponseParse::State::kError) << bad;
    }
    const std::string hugeHeader = "HTTP/1.1 200 OK\r\nX: " + std::string(tracing::kMaxHttpResponseHeaderBytes, 'a');
    EXPECT_EQ(tracing::ParseHttpResponse(hugeHeader).state, HttpResponseParse::State::kError) << "头部过长";
    EXPECT_EQ(tracing::ParseHttpResponse("HTTP/1.1 200 OK\r\nContent-Length: 1\r\nContent-Length: 1\r\n\r\nx").state,
              HttpResponseParse::State::kComplete)
        << "重复但一致的长度可以接受";
}
