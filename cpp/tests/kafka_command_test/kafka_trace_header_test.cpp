#include <gtest/gtest.h>

#include <initializer_list>
#include <memory>
#include <string>
#include <utility>

#include "messaging/kafka/kafka_trace_header.h"

// ---------------------------------------------------------------------------
// kafka_trace_header_test — Kafka 消息头上的 traceparent(docs/design/distributed-tracing.md §5.4)。
//
// 生产端只在有合法 trace 时建 headers,消费端从 headers 取回 trace。两头必须同一口径,
// 并且与 Go 侧 go/shared/tracing/kafkagotrace、saramatrace 写的头互认。要盯住的三件事:
//   1) 往返:TraceparentOf 写出的值经 Headers 读回后,trace_id / span_id / flags 逐位相同。
//   2) 没有 headers、没有这个键、值不合法时得到无效 trace(消费端据此开新根),不抛、不丢消息。
//   3) 同一个键出现多次时取最后一个,与 Go 载体同口径。
// 只用 RdKafka::Headers 这个纯内存对象,不连 broker、不创建 librdkafka 句柄。
//
// main() 在 kafka_command_test.cpp 里,本文件不再定义。
// ---------------------------------------------------------------------------

namespace {

// Go 侧 otel 传播器写出的形状(全小写 W3C traceparent)。kafkagotrace / saramatrace 的用例
// 对同一组 id 断言写出的就是这个字面值,两边改动要一起改。
constexpr const char *kGoWritten = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01";

std::unique_ptr<RdKafka::Headers> HeadersWith(
    std::initializer_list<std::pair<std::string, std::string>> entries)
{
    std::unique_ptr<RdKafka::Headers> headers(RdKafka::Headers::create());
    for (const auto &[key, value] : entries)
    {
        EXPECT_EQ(headers->add(key, value), RdKafka::ERR_NO_ERROR);
    }
    return headers;
}

TEST(KafkaTraceHeader, ProducerValueRoundTripsThroughHeaders)
{
    const tracing::TraceContext produced = tracing::NewRoot();
    const std::string value = kafka_trace::TraceparentOf(produced);
    ASSERT_EQ(value.size(), tracing::kTraceparentChars);

    const auto headers = HeadersWith({{std::string(kafka_trace::kTraceparentHeader), value}});
    const tracing::TraceContext consumed = kafka_trace::TraceFromHeaders(headers.get());
    ASSERT_TRUE(consumed.IsValid());
    EXPECT_EQ(consumed.trace_id.ToHex(), produced.trace_id.ToHex());
    EXPECT_EQ(consumed.span_id.ToHex(), produced.span_id.ToHex());
    EXPECT_EQ(consumed.flags, produced.flags);
}

TEST(KafkaTraceHeader, ReadsTheValueGoWrites)
{
    const auto headers = HeadersWith({{"traceparent", kGoWritten}});
    const tracing::TraceContext consumed = kafka_trace::TraceFromHeaders(headers.get());
    ASSERT_TRUE(consumed.IsValid());
    EXPECT_EQ(consumed.trace_id.ToHex(), "4bf92f3577b34da6a3ce929d0e0e4736");
    EXPECT_EQ(consumed.span_id.ToHex(), "00f067aa0ba902b7");
    EXPECT_EQ(consumed.ToTraceparent(), kGoWritten);
}

// 无效 trace 返回空串,生产端据此不建 headers,消息与改动前逐字节相同。
TEST(KafkaTraceHeader, InvalidTraceWritesNoValue)
{
    EXPECT_TRUE(kafka_trace::TraceparentOf(tracing::TraceContext{}).empty());
}

TEST(KafkaTraceHeader, MissingHeadersOrKeyYieldInvalidTrace)
{
    EXPECT_FALSE(kafka_trace::TraceFromHeaders(nullptr).IsValid());

    const auto empty = HeadersWith({});
    EXPECT_FALSE(kafka_trace::TraceFromHeaders(empty.get()).IsValid());

    const auto otherKey = HeadersWith({{"other", kGoWritten}});
    EXPECT_FALSE(kafka_trace::TraceFromHeaders(otherKey.get()).IsValid());
}

TEST(KafkaTraceHeader, MalformedValueYieldsInvalidTrace)
{
    const std::initializer_list<std::string> malformed = {
        "garbage",
        "",
        // 大写十六进制:W3C 规定必须忽略。
        "00-4BF92F3577B34DA6A3CE929D0E0E4736-00F067AA0BA902B7-01",
        // 全零 trace_id。
        "00-00000000000000000000000000000000-00f067aa0ba902b7-01",
        // 未知版本。
        "01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
    };
    for (const std::string &value : malformed)
    {
        const auto headers = HeadersWith({{"traceparent", value}});
        EXPECT_FALSE(kafka_trace::TraceFromHeaders(headers.get()).IsValid()) << "value=" << value;
    }
}

TEST(KafkaTraceHeader, LastValueWinsWhenKeyRepeats)
{
    const auto headers = HeadersWith({
        {"traceparent", "00-11111111111111111111111111111111-1111111111111111-01"},
        {"traceparent", kGoWritten},
    });
    EXPECT_EQ(kafka_trace::TraceFromHeaders(headers.get()).trace_id.ToHex(),
              "4bf92f3577b34da6a3ce929d0e0e4736");
}

} // namespace
