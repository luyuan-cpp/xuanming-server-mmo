#pragma once

#include <span>
#include <string>
#include <utility>
#include <vector>

#include "core/tracing/span_record.h"

// 把一批已结束的 span 编成 OTLP 的 ExportTraceServiceRequest(protobuf 二进制),OTLP/HTTP 的请求体就是它
// (docs/design/distributed-tracing.md §10.3)。消息定义是官方 opentelemetry-proto v1.3.1,拷在 proto/opentelemetry/,
// 由协议生成器生成 C++ 绑定;这里用仓库已有的 protobuf 库编码,不引入 OpenTelemetry C++ SDK。
// 纯函数,单测在 cpp/tests/rpc_controller_test/otlp_trace_encoder_test.cpp。

namespace tracing {

// 进程级的资源属性(OTel 语义约定 service.* / telemetry.sdk.*),每个请求带一份。
struct OtlpResource {
    std::string serviceName;          // service.name:节点角色 gate / scene / battle
    std::string serviceNamespace;     // service.namespace:"mmorpg"
    std::string serviceInstanceId;    // service.instance.id:节点 uuid;空 = 不写
    std::string serviceVersion;       // service.version:部署构建标识;空 = 不写
    // 其余低基数属性(例如 mmorpg.zone_id);键重复时以先写的为准由调用方保证,这里原样写出。
    std::vector<std::pair<std::string, std::string>> extraAttributes;
};

// instrumentation scope 名。所有接缝产生的 span 同属这一个 scope。
inline constexpr const char* kOtlpInstrumentationScope = "mmorpg.cpp.tracing";

// 编成一个 ResourceSpans、一个 ScopeSpans。span 的取值规则:
//   * parent_span_id 全零 = 根 span,不写这个字段;
//   * flags = W3C trace flags | SPAN_FLAGS_CONTEXT_HAS_IS_REMOTE_MASK |(父在别的进程时)SPAN_FLAGS_CONTEXT_IS_REMOTE_MASK;
//   * status 为 kUnset 时不写 Status;
//   * 按通道写 rpc.system(mmorpg_rpc0 / grpc / mmorpg_client)或 messaging.system(kafka),gRPC 另写 rpc.grpc.status_code。
std::string EncodeExportTraceServiceRequest(const OtlpResource& resource, std::span<const SpanRecord> spans);

} // namespace tracing
