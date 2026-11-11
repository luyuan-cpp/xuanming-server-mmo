#pragma once

#include <string>
#include <string_view>

#include <rdkafkacpp.h>

#include "core/tracing/trace_context.h"

// Kafka 消息头上的链路追踪(docs/design/distributed-tracing.md §5.4):键 traceparent,值是 W3C traceparent 字符串,
// 与 Go(otel 传播器)写的同形,三种语言互认。生产与消费只经这里读写。

namespace kafka_trace
{

inline constexpr std::string_view kTraceparentHeader = "traceparent";

// 要写进消息头的值;trace 无效时返回空串,调用方据此不建 headers,消息与改动前逐字节相同。
inline std::string TraceparentOf(const tracing::TraceContext &trace)
{
    return trace.IsValid() ? trace.ToTraceparent() : std::string();
}

// 读消息头上的 trace。没有 headers、没有这个键、值不合法时返回无效值,消费端据此开新根,绝不据此丢弃消息。
// 同一个键出现多次时取最后一个(与 otel 的 kafka-go 载体同口径)。headers 归消息所有,只在回调期间有效。
inline tracing::TraceContext TraceFromHeaders(const RdKafka::Headers *headers)
{
    tracing::TraceContext trace;
    if (headers == nullptr)
    {
        return trace;
    }
    const RdKafka::Headers::Header header = headers->get_last(std::string(kTraceparentHeader));
    if (header.err() != RdKafka::ERR_NO_ERROR || header.value() == nullptr)
    {
        return trace;
    }
    // 解析失败时 trace 保持无效。
    (void)tracing::ParseTraceparent(
        std::string_view(static_cast<const char *>(header.value()), header.value_size()), trace);
    return trace;
}

} // namespace kafka_trace
