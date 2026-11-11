// rpc_trace_envelope_test —— RPC0 信封上的 trace 读写(docs/design/distributed-tracing.md §5.2)。
//
// 被锁定的契约:
//   * 三个 64 位字段按大端拼:%016llx 打出来与 TraceId::ToHex / SpanId::ToHex 一致,抓包能直接对上日志;
//   * 写入再读出得到同一个 trace_id、span_id 与 flags;
//   * 无效的 trace 一个字段都不写,帧不多一个字节;
//   * 读的时候 trace_id 全 0 或 parent_span_id 为 0 都当作没有,输出保持原样;flags 只取低 8 位。
//
// 与 rpc_controller_test.cpp 同一工程、同一个 main。被测的是纯函数,不碰线程局部状态。

#include <gtest/gtest.h>

#include <cstdint>
#include <cstdio>
#include <string>
#include <string_view>

#include "core/tracing/trace_context.h"
#include "network/rpc_trace_envelope.h"
#include "proto/common/base/rpc_message.pb.h"

namespace {

constexpr std::string_view kCanonical = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01";

std::string Hex64(uint64_t value)
{
    char buffer[17] = {};
    std::snprintf(buffer, sizeof(buffer), "%016llx", static_cast<unsigned long long>(value));
    return std::string(buffer, 16);
}

} // namespace

TEST(RpcTraceEnvelopeTest, WritesBigEndianFieldsThatMatchTheHexForm)
{
    tracing::TraceContext trace;
    ASSERT_TRUE(tracing::ParseTraceparent(kCanonical, trace));

    GameRpcMessage message;
    WriteTraceToRpcEnvelope(trace, message);
    EXPECT_EQ(message.trace_id_hi(), 0x4bf92f3577b34da6ull);
    EXPECT_EQ(message.trace_id_lo(), 0xa3ce929d0e0e4736ull);
    EXPECT_EQ(message.parent_span_id(), 0x00f067aa0ba902b7ull);
    EXPECT_EQ(message.trace_flags(), 1u);
    EXPECT_EQ(Hex64(message.trace_id_hi()) + Hex64(message.trace_id_lo()), trace.trace_id.ToHex());
    EXPECT_EQ(Hex64(message.parent_span_id()), trace.span_id.ToHex());
}

TEST(RpcTraceEnvelopeTest, RoundTripsAFreshRoot)
{
    tracing::TraceContext root = tracing::NewRoot();
    root.flags = 0x00;
    GameRpcMessage message;
    WriteTraceToRpcEnvelope(root, message);

    tracing::TraceContext read;
    ASSERT_TRUE(ReadTraceFromRpcEnvelope(message, read));
    EXPECT_EQ(read.trace_id.bytes, root.trace_id.bytes);
    EXPECT_EQ(read.span_id.bytes, root.span_id.bytes);
    EXPECT_EQ(read.flags, root.flags);
}

TEST(RpcTraceEnvelopeTest, InvalidTraceWritesNothing)
{
    GameRpcMessage message;
    WriteTraceToRpcEnvelope(tracing::TraceContext{}, message);
    EXPECT_EQ(message.trace_id_hi(), 0u);
    EXPECT_EQ(message.trace_id_lo(), 0u);
    EXPECT_EQ(message.parent_span_id(), 0u);
    EXPECT_EQ(message.trace_flags(), 0u);
    EXPECT_EQ(message.ByteSizeLong(), 0u);
}

TEST(RpcTraceEnvelopeTest, MissingTraceIdOrSpanReadsAsNoTrace)
{
    const tracing::TraceContext sentinel = tracing::NewRoot();

    GameRpcMessage none;   // 四个字段都是 0
    GameRpcMessage spanOnly;
    spanOnly.set_parent_span_id(0x1234);
    GameRpcMessage traceOnly;
    traceOnly.set_trace_id_lo(0x5678);
    traceOnly.set_trace_flags(1);

    for (const GameRpcMessage* message : {&none, &spanOnly, &traceOnly}) {
        tracing::TraceContext read = sentinel;
        EXPECT_FALSE(ReadTraceFromRpcEnvelope(*message, read));
        EXPECT_EQ(read.trace_id.bytes, sentinel.trace_id.bytes);
        EXPECT_EQ(read.span_id.bytes, sentinel.span_id.bytes);
        EXPECT_EQ(read.flags, sentinel.flags);
    }
}

TEST(RpcTraceEnvelopeTest, OnlyTheLowByteOfFlagsIsKept)
{
    GameRpcMessage message;
    message.set_trace_id_hi(1);
    message.set_parent_span_id(2);
    message.set_trace_flags(0x101);

    tracing::TraceContext read;
    ASSERT_TRUE(ReadTraceFromRpcEnvelope(message, read));
    EXPECT_EQ(read.flags, 0x01);
}
