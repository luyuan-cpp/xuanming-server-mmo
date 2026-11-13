#include "core/tracing/otlp_trace_encoder.h"

#include <string_view>

#include "proto/opentelemetry/proto/collector/trace/v1/trace_service.pb.h"

namespace tracing {

namespace {

namespace otlp_collector = ::opentelemetry::proto::collector::trace::v1;
namespace otlp_trace = ::opentelemetry::proto::trace::v1;
namespace otlp_common = ::opentelemetry::proto::common::v1;

// OTLP trace.proto 的 SpanFlags 位(同名枚举里的掩码值)。
constexpr uint32_t kSpanFlagsTraceFlagsMask = 0xFF;
constexpr uint32_t kSpanFlagsContextHasIsRemote = 0x100;
constexpr uint32_t kSpanFlagsContextIsRemote = 0x200;

void AddStringAttribute(::google::protobuf::RepeatedPtrField<otlp_common::KeyValue>* attributes, std::string_view key,
                        std::string_view value)
{
    otlp_common::KeyValue* attribute = attributes->Add();
    attribute->mutable_key()->assign(key.data(), key.size());
    attribute->mutable_value()->mutable_string_value()->assign(value.data(), value.size());
}

void AddIntAttribute(::google::protobuf::RepeatedPtrField<otlp_common::KeyValue>* attributes, std::string_view key,
                     int64_t value)
{
    otlp_common::KeyValue* attribute = attributes->Add();
    attribute->mutable_key()->assign(key.data(), key.size());
    attribute->mutable_value()->set_int_value(value);
}

// 通道 → OTel 语义约定的属性。rpc.system 的值:grpc 是约定里的标准值;两种自有协议用 mmorpg_ 前缀的自定义值。
void AddSystemAttributes(const SpanRecord& record, ::google::protobuf::RepeatedPtrField<otlp_common::KeyValue>* attributes)
{
    switch (record.system) {
    case SpanSystem::kRpc0:
        AddStringAttribute(attributes, "rpc.system", "mmorpg_rpc0");
        break;
    case SpanSystem::kGrpc:
        AddStringAttribute(attributes, "rpc.system", "grpc");
        if (record.has_grpc_status) {
            AddIntAttribute(attributes, "rpc.grpc.status_code", static_cast<int64_t>(record.grpc_status_code));
        }
        break;
    case SpanSystem::kKafka:
        AddStringAttribute(attributes, "messaging.system", "kafka");
        break;
    case SpanSystem::kGateClient:
        AddStringAttribute(attributes, "rpc.system", "mmorpg_client");
        break;
    case SpanSystem::kNone:
    case SpanSystem::kCount:
        break;
    }
}

void FillSpan(const SpanRecord& record, otlp_trace::Span* span)
{
    span->mutable_trace_id()->assign(reinterpret_cast<const char*>(record.trace_id.bytes.data()), kTraceIdBytes);
    span->mutable_span_id()->assign(reinterpret_cast<const char*>(record.span_id.bytes.data()), kSpanIdBytes);
    if (record.parent_span_id.IsValid()) {
        span->mutable_parent_span_id()->assign(reinterpret_cast<const char*>(record.parent_span_id.bytes.data()),
                                               kSpanIdBytes);
    }
    // 运行时的 span 表示本来就知道父在不在别的进程,所以 HAS_IS_REMOTE 一律置位;高 22 位必须为 0(trace.proto 的约定)。
    uint32_t flags = (static_cast<uint32_t>(record.trace_flags) & kSpanFlagsTraceFlagsMask) | kSpanFlagsContextHasIsRemote;
    if (record.parent_is_remote) {
        flags |= kSpanFlagsContextIsRemote;
    }
    span->set_flags(flags);
    const std::string_view name = record.Name();
    span->mutable_name()->assign(name.data(), name.size());
    span->set_kind(static_cast<otlp_trace::Span_SpanKind>(record.kind));
    span->set_start_time_unix_nano(record.start_unix_nano);
    span->set_end_time_unix_nano(record.end_unix_nano);
    AddSystemAttributes(record, span->mutable_attributes());
    if (record.status != SpanStatus::kUnset) {
        otlp_trace::Status* status = span->mutable_status();
        status->set_code(static_cast<otlp_trace::Status_StatusCode>(record.status));
        const std::string_view message = record.StatusMessage();
        if (!message.empty()) {
            status->mutable_message()->assign(message.data(), message.size());
        }
    }
}

} // namespace

std::string EncodeExportTraceServiceRequest(const OtlpResource& resource, std::span<const SpanRecord> spans)
{
    otlp_collector::ExportTraceServiceRequest request;
    otlp_trace::ResourceSpans* resourceSpans = request.add_resource_spans();

    auto* resourceAttributes = resourceSpans->mutable_resource()->mutable_attributes();
    AddStringAttribute(resourceAttributes, "service.name", resource.serviceName);
    if (!resource.serviceNamespace.empty()) {
        AddStringAttribute(resourceAttributes, "service.namespace", resource.serviceNamespace);
    }
    if (!resource.serviceInstanceId.empty()) {
        AddStringAttribute(resourceAttributes, "service.instance.id", resource.serviceInstanceId);
    }
    if (!resource.serviceVersion.empty()) {
        AddStringAttribute(resourceAttributes, "service.version", resource.serviceVersion);
    }
    AddStringAttribute(resourceAttributes, "telemetry.sdk.name", "mmorpg-cpp");
    AddStringAttribute(resourceAttributes, "telemetry.sdk.language", "cpp");
    for (const auto& [key, value] : resource.extraAttributes) {
        AddStringAttribute(resourceAttributes, key, value);
    }

    otlp_trace::ScopeSpans* scopeSpans = resourceSpans->add_scope_spans();
    scopeSpans->mutable_scope()->set_name(kOtlpInstrumentationScope);
    scopeSpans->mutable_spans()->Reserve(static_cast<int>(spans.size()));
    for (const SpanRecord& record : spans) {
        FillSpan(record, scopeSpans->add_spans());
    }

    std::string body;
    request.SerializeToString(&body);
    return body;
}

} // namespace tracing
