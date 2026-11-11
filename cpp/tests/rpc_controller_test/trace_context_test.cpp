// trace_context_test —— W3C 原语与线程局部日志上下文(docs/design/distributed-tracing.md §4、§5.7)。
//
// 被锁定的契约:
//   * ParseTraceparent 只收版本 00、55 个字符、小写十六进制、trace_id 与 span_id 都非零;失败时不改写输出;
//   * 查表版 WriteLowerHex 与逐字节 snprintf("%02x") 在 0..255 上全部一致;ToTraceparent 只写 sampled 位;
//   * ScopedSpan、ScopedPlayerId 退出时恢复进入前的值,嵌套也一样;ScopedRoot 不接当前 trace,总是开新根;
//     ScopedIncoming 收到合法的上下文就建子 span,不合法就开新根,两种情况都不接线程上残留的上下文;
//   * CurrentTraceHex 跟着当前 trace 走,包括绕过作用域直接 Set 的情况(缓存比对原值,不会读到过期值);
//   * Bind 记下包装那一刻的 trace,执行时在它下面建子 span;包装时没有 trace 就在无 trace 下执行,执行完恢复。
//
// 与 rpc_controller_test.cpp 同一工程、同一个 main。线程局部的用例都在 gtest 主线程上跑,
// 夹具在开始和结束时把本线程的 trace 复位,不让上下文漏给同进程里后面的用例。

#include <gtest/gtest.h>

#include <cstdint>
#include <cstdio>
#include <string>
#include <string_view>

#include "core/tracing/trace_context.h"
#include "thread_context/trace_context_tls.h"

namespace {

constexpr std::string_view kCanonical = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01";

std::string HexOf(const tracing::TraceContext& ctx)
{
    return ctx.trace_id.ToHex() + ctx.span_id.ToHex();
}

class TraceContextTlsTest : public ::testing::Test {
protected:
    void SetUp() override { tracing::Set(tracing::TraceContext{}); }
    void TearDown() override { tracing::Set(tracing::TraceContext{}); }
};

} // namespace

TEST(TraceContextTest, ParsesCanonicalTraceparent)
{
    tracing::TraceContext ctx;
    ASSERT_TRUE(tracing::ParseTraceparent(kCanonical, ctx));
    EXPECT_EQ(ctx.trace_id.ToHex(), "4bf92f3577b34da6a3ce929d0e0e4736");
    EXPECT_EQ(ctx.span_id.ToHex(), "00f067aa0ba902b7");
    EXPECT_EQ(ctx.flags, 0x01);
    EXPECT_EQ(ctx.ToTraceparent(), kCanonical);
}

TEST(TraceContextTest, NewRootRoundTripsThroughTraceparent)
{
    const tracing::TraceContext root = tracing::NewRoot();
    ASSERT_TRUE(root.IsValid());

    tracing::TraceContext parsed;
    ASSERT_TRUE(tracing::ParseTraceparent(root.ToTraceparent(), parsed));
    EXPECT_EQ(parsed.trace_id.bytes, root.trace_id.bytes);
    EXPECT_EQ(parsed.span_id.bytes, root.span_id.bytes);
    EXPECT_EQ(parsed.flags, root.flags);
}

TEST(TraceContextTest, RejectsInvalidTraceparentWithoutTouchingOutput)
{
    const std::string_view invalid[] = {
        "",
        "00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01",   // trace_id 大写
        "00-4bf92f3577b34da6a3ce929d0e0e4736-00F067AA0BA902B7-01",   // span_id 大写
        "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-0A",   // flags 大写
        "ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",   // 版本 ff 非法
        "01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",   // 只认版本 00
        "00-4bf92f3577b34da6a3ce929d0e0e473-00f067aa0ba902b7-01",    // 少一个字符
        "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-",  // 多一个字符
        "00_4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",   // 分隔符错
        "00-4bf92f3577b34da6a3ce929d0e0e4736_00f067aa0ba902b7-01",
        "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7_01",
        "00-00000000000000000000000000000000-00f067aa0ba902b7-01",   // 全零 trace_id
        "00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01",   // 全零 span_id
        "00-4bf92f3577b34da6a3ce929d0e0e473g-00f067aa0ba902b7-01",   // 非十六进制字符
    };

    const tracing::TraceContext sentinel = tracing::NewRoot();
    for (const std::string_view header : invalid) {
        tracing::TraceContext ctx = sentinel;
        EXPECT_FALSE(tracing::ParseTraceparent(header, ctx)) << header;
        EXPECT_EQ(ctx.trace_id.bytes, sentinel.trace_id.bytes) << header;
        EXPECT_EQ(ctx.span_id.bytes, sentinel.span_id.bytes) << header;
        EXPECT_EQ(ctx.flags, sentinel.flags) << header;
    }
}

TEST(TraceContextTest, ToTraceparentWritesOnlySampledFlag)
{
    tracing::TraceContext ctx;
    ASSERT_TRUE(tracing::ParseTraceparent(kCanonical, ctx));
    ctx.flags = 0x03;
    EXPECT_EQ(ctx.ToTraceparent().substr(tracing::kTraceparentChars - 2), "01");
    ctx.flags = 0x02;
    EXPECT_EQ(ctx.ToTraceparent().substr(tracing::kTraceparentChars - 2), "00");
}

TEST(TraceContextTest, WriteLowerHexMatchesSnprintfForEveryByte)
{
    for (int value = 0; value <= 0xFF; ++value) {
        const auto byte = static_cast<uint8_t>(value);
        char actual[2] = {};
        tracing::WriteLowerHex(&byte, 1, actual);
        char expected[3] = {};
        std::snprintf(expected, sizeof(expected), "%02x", value);
        EXPECT_EQ(std::string_view(actual, 2), std::string_view(expected, 2)) << value;
    }
}

TEST(TraceContextTest, ChildSpanKeepsTraceAndFlagsWithFreshSpan)
{
    tracing::TraceContext parent = tracing::NewRoot();
    parent.flags = 0x00;
    const tracing::TraceContext child = tracing::ChildSpan(parent);
    EXPECT_EQ(child.trace_id.bytes, parent.trace_id.bytes);
    EXPECT_EQ(child.flags, parent.flags);
    EXPECT_TRUE(child.span_id.IsValid());
    EXPECT_NE(child.span_id.bytes, parent.span_id.bytes);
}

TEST_F(TraceContextTlsTest, ScopedSpanRestoresPreviousTrace)
{
    ASSERT_FALSE(tracing::Current().IsValid());
    {
        tracing::ScopedSpan root;  // 没有 trace 时开新根
        const tracing::TraceContext rootCtx = tracing::Current();
        ASSERT_TRUE(rootCtx.IsValid());
        {
            tracing::ScopedSpan child;  // 已有 trace 时建它的子 span
            EXPECT_EQ(tracing::Current().trace_id.bytes, rootCtx.trace_id.bytes);
            EXPECT_NE(tracing::Current().span_id.bytes, rootCtx.span_id.bytes);
        }
        EXPECT_EQ(tracing::Current().trace_id.bytes, rootCtx.trace_id.bytes);
        EXPECT_EQ(tracing::Current().span_id.bytes, rootCtx.span_id.bytes);
    }
    EXPECT_FALSE(tracing::Current().IsValid());
}

TEST_F(TraceContextTlsTest, ScopedSpanWithParentInstallsChildOfParent)
{
    const tracing::TraceContext captured = tracing::NewRoot();
    {
        tracing::ScopedSpan restored(captured);
        EXPECT_EQ(tracing::Current().trace_id.bytes, captured.trace_id.bytes);
        EXPECT_NE(tracing::Current().span_id.bytes, captured.span_id.bytes);
    }
    EXPECT_FALSE(tracing::Current().IsValid());

    {
        tracing::ScopedSpan restored{tracing::TraceContext{}};  // 捕获值无效:作用域内没有 trace
        EXPECT_FALSE(tracing::Current().IsValid());
    }
    EXPECT_FALSE(tracing::Current().IsValid());
}

TEST_F(TraceContextTlsTest, ScopedPlayerIdRestoresPreviousPlayer)
{
    ASSERT_EQ(tracing::CurrentPlayerId(), 0u);
    {
        tracing::ScopedPlayerId outer(1001);
        EXPECT_EQ(tracing::CurrentPlayerId(), 1001u);
        {
            tracing::ScopedPlayerId inner(2002);
            EXPECT_EQ(tracing::CurrentPlayerId(), 2002u);
        }
        EXPECT_EQ(tracing::CurrentPlayerId(), 1001u);
    }
    EXPECT_EQ(tracing::CurrentPlayerId(), 0u);
}

TEST_F(TraceContextTlsTest, CurrentTraceHexIsEmptyWithoutTrace)
{
    EXPECT_TRUE(tracing::CurrentTraceHex().empty());
}

TEST_F(TraceContextTlsTest, CurrentTraceHexFollowsEveryChange)
{
    const tracing::TraceContext first = tracing::NewRoot();
    tracing::Set(first);
    EXPECT_EQ(tracing::CurrentTraceHex(), HexOf(first));

    // 绕过作用域直接改:缓存比对原值,不会给出上一个 trace 的十六进制。
    const tracing::TraceContext second = tracing::NewRoot();
    tracing::Set(second);
    EXPECT_EQ(tracing::CurrentTraceHex(), HexOf(second));

    {
        tracing::ScopedSpan child;
        EXPECT_EQ(tracing::CurrentTraceHex(), HexOf(tracing::Current()));
    }
    EXPECT_EQ(tracing::CurrentTraceHex(), HexOf(second));

    tracing::Set(tracing::TraceContext{});
    EXPECT_TRUE(tracing::CurrentTraceHex().empty());
}

TEST_F(TraceContextTlsTest, ScopedIncomingContinuesAValidTraceAndRootsAnInvalidOne)
{
    const tracing::TraceContext leftover = tracing::NewRoot();
    tracing::Set(leftover);   // 线程上碰巧在场的上下文:两种情况都不接它

    const tracing::TraceContext upstream = tracing::NewRoot();
    {
        tracing::ScopedIncoming incoming(upstream);
        EXPECT_EQ(tracing::Current().trace_id.bytes, upstream.trace_id.bytes);
        EXPECT_NE(tracing::Current().span_id.bytes, upstream.span_id.bytes);
    }
    EXPECT_EQ(tracing::Current().span_id.bytes, leftover.span_id.bytes);

    {
        tracing::ScopedIncoming incoming{tracing::TraceContext{}};
        ASSERT_TRUE(tracing::Current().IsValid());
        EXPECT_NE(tracing::Current().trace_id.bytes, leftover.trace_id.bytes);
    }
    EXPECT_EQ(tracing::Current().trace_id.bytes, leftover.trace_id.bytes);
    EXPECT_EQ(tracing::Current().span_id.bytes, leftover.span_id.bytes);
}

TEST_F(TraceContextTlsTest, ScopedRootIgnoresTheCurrentTraceAndRestoresIt)
{
    const tracing::TraceContext leftover = tracing::NewRoot();
    tracing::Set(leftover);
    {
        tracing::ScopedRoot root;   // 入口不接线程上碰巧在场的 trace
        ASSERT_TRUE(tracing::Current().IsValid());
        EXPECT_NE(tracing::Current().trace_id.bytes, leftover.trace_id.bytes);
    }
    EXPECT_EQ(tracing::Current().trace_id.bytes, leftover.trace_id.bytes);
    EXPECT_EQ(tracing::Current().span_id.bytes, leftover.span_id.bytes);
}

TEST_F(TraceContextTlsTest, BindRunsUnderTheTraceCapturedAtBindTime)
{
    const tracing::TraceContext request = tracing::NewRoot();
    tracing::Set(request);
    tracing::TraceContext seen;
    auto continuation = tracing::Bind([&seen] { seen = tracing::Current(); });

    const tracing::TraceContext other = tracing::NewRoot();
    tracing::Set(other);   // 执行时线程上是另一条 trace
    continuation();
    EXPECT_EQ(seen.trace_id.bytes, request.trace_id.bytes);
    EXPECT_NE(seen.span_id.bytes, request.span_id.bytes);   // 在捕获值下建了子 span
    EXPECT_EQ(tracing::Current().trace_id.bytes, other.trace_id.bytes);   // 执行完恢复
}

TEST_F(TraceContextTlsTest, BindWithoutTraceRunsWithoutTraceEvenIfTheCallerHasOne)
{
    tracing::TraceContext seen = tracing::NewRoot();
    auto continuation = tracing::Bind([&seen] { seen = tracing::Current(); });

    const tracing::TraceContext other = tracing::NewRoot();
    tracing::Set(other);
    continuation();
    EXPECT_FALSE(seen.IsValid());   // 宁可丢,不可错:不接上执行线程上的 trace
    EXPECT_EQ(tracing::Current().trace_id.bytes, other.trace_id.bytes);
}

TEST_F(TraceContextTlsTest, BindForwardsArgumentsAndTheResult)
{
    auto add = tracing::Bind([](int a, int b) { return a + b; });
    EXPECT_EQ(add(2, 3), 5);
}
