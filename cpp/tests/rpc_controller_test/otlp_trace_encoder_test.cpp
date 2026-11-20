// otlp_trace_encoder_test —— SpanRecord 编成 OTLP ExportTraceServiceRequest 的契约(docs/design/distributed-tracing.md §10.3)。
// 用生成的官方消息(proto/opentelemetry/,v1.3.1)把编码结果解析回来逐项核对:资源属性、scope、id 的字节、父、flags 的
// 远端位、kind、起止时间、通道属性、状态。

#include <gtest/gtest.h>

#include <string>
#include <string_view>
#include <vector>

#include "core/tracing/otlp_trace_encoder.h"
#include "proto/opentelemetry/proto/collector/trace/v1/trace_service.pb.h"
#include "span_test_support.h"

namespace {

namespace otlp_collector = ::opentelemetry::proto::collector::trace::v1;
namespace otlp_trace = ::opentelemetry::proto::trace::v1;
namespace otlp_common = ::opentelemetry::proto::common::v1;

const otlp_common::AnyValue* FindAttribute(const ::google::protobuf::RepeatedPtrField<otlp_common::KeyValue>& attributes,
                                           std::string_view key)
{
    for (const auto& attribute : attributes) {
        if (attribute.key() == key) {
            return &attribute.value();
        }
    }
    return nullptr;
}

std::string StringAttribute(const ::google::protobuf::RepeatedPtrField<otlp_common::KeyValue>& attributes,
                            std::string_view key)
{
    const otlp_common::AnyValue* value = FindAttribute(attributes, key);
    return value != nullptr && value->has_string_value() ? value->string_value() : std::string("<missing>");
}

tracing::OtlpResource MakeResource()
{
    tracing::OtlpResource resource;
    resource.serviceName = "scene";
    resource.serviceNamespace = "mmorpg";
    resource.serviceInstanceId = "uuid-1";
    resource.serviceVersion = "build-7";
    resource.extraAttributes.emplace_back("mmorpg.zone_id", "3");
    return resource;
}

} // namespace

TEST(OtlpTraceEncoderTest, EncodesResourceScopeAndEverySpanField)
{
    std::vector<tracing::SpanRecord> spans;

    tracing::SpanRecord root = span_test::MakeSpanRecord(1, tracing::SpanKind::kServer);
    root.system = tracing::SpanSystem::kGateClient;
    root.status = tracing::SpanStatus::kError;
    root.SetStatusMessage("NO_METHOD");
    spans.push_back(root);

    tracing::SpanRecord child = span_test::MakeSpanRecord(2, tracing::SpanKind::kClient);
    child.parent_span_id = root.span_id;
    child.system = tracing::SpanSystem::kGrpc;
    child.has_grpc_status = true;
    child.grpc_status_code = 14;
    spans.push_back(child);

    tracing::SpanRecord consumer = span_test::MakeSpanRecord(3, tracing::SpanKind::kConsumer);
    consumer.parent_span_id = child.span_id;
    consumer.parent_is_remote = true;
    consumer.system = tracing::SpanSystem::kKafka;
    spans.push_back(consumer);

    const std::string body = tracing::EncodeExportTraceServiceRequest(MakeResource(), spans);
    ASSERT_FALSE(body.empty());
    otlp_collector::ExportTraceServiceRequest request;
    ASSERT_TRUE(request.ParseFromString(body));
    ASSERT_EQ(request.resource_spans_size(), 1);
    const otlp_trace::ResourceSpans& resourceSpans = request.resource_spans(0);

    const auto& resource = resourceSpans.resource().attributes();
    EXPECT_EQ(StringAttribute(resource, "service.name"), "scene");
    EXPECT_EQ(StringAttribute(resource, "service.namespace"), "mmorpg");
    EXPECT_EQ(StringAttribute(resource, "service.instance.id"), "uuid-1");
    EXPECT_EQ(StringAttribute(resource, "service.version"), "build-7");
    EXPECT_EQ(StringAttribute(resource, "telemetry.sdk.language"), "cpp");
    EXPECT_EQ(StringAttribute(resource, "mmorpg.zone_id"), "3");

    ASSERT_EQ(resourceSpans.scope_spans_size(), 1);
    const otlp_trace::ScopeSpans& scopeSpans = resourceSpans.scope_spans(0);
    EXPECT_EQ(scopeSpans.scope().name(), tracing::kOtlpInstrumentationScope);
    ASSERT_EQ(scopeSpans.spans_size(), 3);

    const otlp_trace::Span& encodedRoot = scopeSpans.spans(0);
    EXPECT_EQ(encodedRoot.trace_id(), std::string(reinterpret_cast<const char*>(root.trace_id.bytes.data()), 16));
    EXPECT_EQ(encodedRoot.span_id(), std::string(reinterpret_cast<const char*>(root.span_id.bytes.data()), 8));
    EXPECT_TRUE(encodedRoot.parent_span_id().empty()) << "根 span 不写父";
    EXPECT_EQ(encodedRoot.flags(), 0x101u) << "sampled | HAS_IS_REMOTE,父不在远端";
    EXPECT_EQ(encodedRoot.kind(), otlp_trace::Span_SpanKind_SPAN_KIND_SERVER);
    EXPECT_EQ(encodedRoot.name(), "span-1");
    EXPECT_EQ(encodedRoot.start_time_unix_nano(), root.start_unix_nano);
    EXPECT_EQ(encodedRoot.end_time_unix_nano(), root.end_unix_nano);
    EXPECT_EQ(StringAttribute(encodedRoot.attributes(), "rpc.system"), "mmorpg_client");
    ASSERT_TRUE(encodedRoot.has_status());
    EXPECT_EQ(encodedRoot.status().code(), otlp_trace::Status_StatusCode_STATUS_CODE_ERROR);
    EXPECT_EQ(encodedRoot.status().message(), "NO_METHOD");

    const otlp_trace::Span& encodedChild = scopeSpans.spans(1);
    EXPECT_EQ(encodedChild.parent_span_id(), encodedRoot.span_id());
    EXPECT_EQ(encodedChild.kind(), otlp_trace::Span_SpanKind_SPAN_KIND_CLIENT);
    EXPECT_EQ(StringAttribute(encodedChild.attributes(), "rpc.system"), "grpc");
    const otlp_common::AnyValue* grpcStatus = FindAttribute(encodedChild.attributes(), "rpc.grpc.status_code");
    ASSERT_NE(grpcStatus, nullptr);
    EXPECT_EQ(grpcStatus->int_value(), 14);
    EXPECT_FALSE(encodedChild.has_status()) << "kUnset 不写 Status";

    const otlp_trace::Span& encodedConsumer = scopeSpans.spans(2);
    EXPECT_EQ(encodedConsumer.flags(), 0x301u) << "父在远端:再置 IS_REMOTE";
    EXPECT_EQ(encodedConsumer.kind(), otlp_trace::Span_SpanKind_SPAN_KIND_CONSUMER);
    EXPECT_EQ(StringAttribute(encodedConsumer.attributes(), "messaging.system"), "kafka");
    EXPECT_EQ(FindAttribute(encodedConsumer.attributes(), "rpc.system"), nullptr);
}

TEST(OtlpTraceEncoderTest, OptionalResourceAttributesAreOmittedAndAnEmptyBatchStillParses)
{
    tracing::OtlpResource resource;
    resource.serviceName = "gate";
    const std::string body = tracing::EncodeExportTraceServiceRequest(resource, {});
    ASSERT_FALSE(body.empty()) << "结果固定含 ResourceSpans / ScopeSpans";
    otlp_collector::ExportTraceServiceRequest request;
    ASSERT_TRUE(request.ParseFromString(body));
    ASSERT_EQ(request.resource_spans_size(), 1);
    const auto& attributes = request.resource_spans(0).resource().attributes();
    EXPECT_EQ(StringAttribute(attributes, "service.name"), "gate");
    EXPECT_EQ(FindAttribute(attributes, "service.instance.id"), nullptr);
    EXPECT_EQ(FindAttribute(attributes, "service.version"), nullptr);
    ASSERT_EQ(request.resource_spans(0).scope_spans_size(), 1);
    EXPECT_EQ(request.resource_spans(0).scope_spans(0).spans_size(), 0);
}
