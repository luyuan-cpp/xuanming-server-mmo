#pragma once

#include <cstddef>
#include <cstdint>
#include <functional>
#include <optional>
#include <string>
#include <string_view>

// OTLP/HTTP 导出的纯逻辑部分(docs/design/distributed-tracing.md §10.3):地址解析、开关与地址的取值、请求报文、
// 响应解析。不碰网络、不依赖 muduo,单测在 cpp/tests/rpc_controller_test/otlp_http_test.cpp。
// 网络部分(连接、攒批、超时)在 otlp_span_exporter.h。
//
// 传输按 OTLP 规范的 OTLP/HTTP:POST,Content-Type: application/x-protobuf,请求体是 ExportTraceServiceRequest。
// 只做明文 http:导出目标是本机或本节点上的 Alloy(本机 127.0.0.1:4318,K8s 上经 internalTrafficPolicy: Local 的 Service)。

namespace tracing {

inline constexpr uint16_t kOtlpHttpDefaultPort = 4318;
inline constexpr std::string_view kOtlpTracesPath = "/v1/traces";

struct OtlpHttpEndpoint {
    std::string host;   // 主机名或点分十进制 IPv4
    uint16_t port{kOtlpHttpDefaultPort};
    std::string path;   // 以 '/' 开头

    // 规范化的地址,只用于日志。
    std::string ToUrl() const;
};

// 解析 traces 地址 http://host[:port][/path]。
//   * 只认 http(scheme 不区分大小写);https、别的 scheme、带凭据(user@)、带查询串或片段、IPv6 字面量一律 nullopt;
//   * 端口缺省 4318(OTLP/HTTP 的标准端口),写了就必须是 1..65535 的十进制;
//   * 按 OTel 规范的「按信号的地址原样使用」:没有路径时用 "/",有路径就原样用。
std::optional<OtlpHttpEndpoint> ParseOtlpTracesEndpoint(std::string_view url);

// 由 OTEL_EXPORTER_OTLP_ENDPOINT(不分信号的基地址)推出 traces 地址:基地址的路径后接 "v1/traces"(OTel 规范)。
std::optional<OtlpHttpEndpoint> ParseOtlpBaseEndpoint(std::string_view url);

// 导出开关与地址的取值结果。
struct OtlpExportSettings {
    bool enabled{false};
    OtlpHttpEndpoint endpoint;
    std::string source;   // 地址从哪来,只用于启动日志:"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT" / "OTEL_EXPORTER_OTLP_ENDPOINT" / "Tracing.OtlpTracesEndpoint"
    std::string error;    // 非空 = 配置不合法(调用方按启动配置错误拒启),此时 enabled 为 false
};

// 环境变量的读法(注入,单测用假实现):返回 nullptr 表示没设。
using EnvironmentLookup = std::function<const char*(const char* name)>;

// 决定导出开不开、发到哪(OTel 规范的环境变量优先于配置文件):
//   1. OTEL_SDK_DISABLED=true(不区分大小写)或 OTEL_TRACES_EXPORTER=none → 关;OTEL_TRACES_EXPORTER 只认 otlp / none,
//      别的值是配置错误;
//   2. OTEL_EXPORTER_OTLP_TRACES_PROTOCOL(没设就看 OTEL_EXPORTER_OTLP_PROTOCOL)只认 http/protobuf,别的值是配置错误;
//   3. 地址依次取 OTEL_EXPORTER_OTLP_TRACES_ENDPOINT、OTEL_EXPORTER_OTLP_ENDPOINT、base_deploy_config 的
//      Tracing.OtlpTracesEndpoint(configEndpoint);第一个非空的为准,解析不了是配置错误;
//   4. 三处都空 → 关。与 go-zero 一致:没配地址就不导出(规范的缺省 localhost:4318 不用,免得没起采集器的环境平白连接失败)。
OtlpExportSettings ResolveOtlpExportSettings(std::string_view configEndpoint, const EnvironmentLookup& environment);

// 一次导出的完整请求报文(HTTP/1.1 长连接,不发 Expect)。
std::string BuildOtlpHttpRequest(const OtlpHttpEndpoint& endpoint, std::string_view body);

// 解析收到的响应字节(从连接上第一个未消费的字节开始,可以只到了一部分)。只关心状态码与这条响应在哪结束:
// 正文按 Content-Length 或 chunked 跳过,1xx 临时响应跳过;两者都没有的,正文到连接关闭为止。
struct HttpResponseParse {
    enum class State {
        kNeedMore,   // 还不完整,等更多字节;headersComplete 为真时 statusCode / bodyUntilClose 已有效
        kComplete,   // 一条完整响应,占 consumed 个字节
        kError,      // 不是合法的 HTTP/1.x 响应,连接只能丢弃
    };
    State state{State::kNeedMore};
    bool headersComplete{false};
    int statusCode{0};
    bool keepAlive{true};         // 连接能否复用:HTTP/1.1 缺省可以;Connection: close 或 HTTP/1.0(没写 keep-alive)不行
    bool bodyUntilClose{false};   // 没有 Content-Length 也不是 chunked:正文到连接关闭为止,连接关闭即算完整
    size_t consumed{0};
};

// 头部(状态行加各头)超过这个长度就算错误:采集器的响应头只有几行。
inline constexpr size_t kMaxHttpResponseHeaderBytes = 16 * 1024;

HttpResponseParse ParseHttpResponse(std::string_view data);

} // namespace tracing
