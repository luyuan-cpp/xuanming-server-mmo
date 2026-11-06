// log_context_suffix_test —— 日志行尾上下文后缀的拼接(docs/design/distributed-tracing.md §5.1)。
//
// 被锁定的契约:
//   * 没有 trace 也没有玩家:返回 0,调用方原样输出;
//   * 后缀插在 muduo 行尾的换行之前;原行没有换行(muduo 单行超长被截断)时补一个;
//   * trace 与玩家各自可以缺省,顺序固定为 trace_id、span_id、player_id;正文逐字节不变;
//   * 输出缓冲按最长后缀预留,不够就返回 0(宁可没有后缀,也不截断正文);
//   * traceSpanHex 长度不是 48:当作没有 trace;
//   * 最长后缀恰好是 kMaxLogContextSuffixBytes;muduo 单行上限(4000)加它一定放得下。
//
// 与 rpc_controller_test.cpp 同一工程、同一个 main。被测的是纯函数,不碰线程局部状态。

#include <gtest/gtest.h>

#include <cstddef>
#include <cstdint>
#include <limits>
#include <string>
#include <string_view>
#include <vector>

#include "core/tracing/log_context_suffix.h"

namespace {

constexpr std::string_view kLine = "20261106 10:00:00.000000 12345 INFO  hello - node.cpp:42\n";
constexpr std::string_view kLineBody = "20261106 10:00:00.000000 12345 INFO  hello - node.cpp:42";
constexpr std::string_view kTraceSpanHex = "4bf92f3577b34da6a3ce929d0e0e4736" "00f067aa0ba902b7";

// 按最长后缀预留缓冲后拼接;没有改写时返回空串。
std::string Rewrite(std::string_view line, const tracing::LogContextFields& fields)
{
    std::vector<char> out(line.size() + tracing::kMaxLogContextSuffixBytes);
    const size_t written = tracing::AppendLogContextSuffix(line, fields, out.data(), out.size());
    return std::string(out.data(), written);
}

} // namespace

TEST(LogContextSuffixTest, NoContextLeavesLineUntouched)
{
    char out[256];
    EXPECT_EQ(tracing::AppendLogContextSuffix(kLine, tracing::LogContextFields{}, out, sizeof(out)), 0u);
}

TEST(LogContextSuffixTest, TraceSuffixGoesBeforeTheNewline)
{
    EXPECT_EQ(Rewrite(kLine, {kTraceSpanHex, 0}),
              std::string(kLineBody) +
                  " [ctx trace_id=4bf92f3577b34da6a3ce929d0e0e4736 span_id=00f067aa0ba902b7]\n");
}

TEST(LogContextSuffixTest, PlayerOnlySuffix)
{
    EXPECT_EQ(Rewrite(kLine, {{}, 1837401234567890ull}), std::string(kLineBody) + " [ctx player_id=1837401234567890]\n");
}

TEST(LogContextSuffixTest, TraceThenPlayer)
{
    EXPECT_EQ(Rewrite(kLine, {kTraceSpanHex, 42}),
              std::string(kLineBody) +
                  " [ctx trace_id=4bf92f3577b34da6a3ce929d0e0e4736 span_id=00f067aa0ba902b7 player_id=42]\n");
}

TEST(LogContextSuffixTest, LineWithoutNewlineGetsOne)
{
    EXPECT_EQ(Rewrite("truncated line without newline", {{}, 7}), "truncated line without newline [ctx player_id=7]\n");
}

TEST(LogContextSuffixTest, MalformedHexLengthMeansNoTrace)
{
    const std::string_view shortHex = kTraceSpanHex.substr(1);  // 47 个字符
    char out[256];
    EXPECT_EQ(tracing::AppendLogContextSuffix(kLine, {shortHex, 0}, out, sizeof(out)), 0u);
    EXPECT_EQ(Rewrite(kLine, {shortHex, 9}), std::string(kLineBody) + " [ctx player_id=9]\n");
}

TEST(LogContextSuffixTest, TooSmallBufferReturnsZero)
{
    std::vector<char> exact(kLineBody.size() + tracing::kMaxLogContextSuffixBytes);
    EXPECT_GT(tracing::AppendLogContextSuffix(kLine, {kTraceSpanHex, 1}, exact.data(), exact.size()), 0u);

    std::vector<char> tooSmall(kLineBody.size() + tracing::kMaxLogContextSuffixBytes - 1);
    EXPECT_EQ(tracing::AppendLogContextSuffix(kLine, {kTraceSpanHex, 1}, tooSmall.data(), tooSmall.size()), 0u);
    EXPECT_EQ(tracing::AppendLogContextSuffix(kLine, {kTraceSpanHex, 1}, nullptr, 0), 0u);
}

TEST(LogContextSuffixTest, LongestSuffixIsExactlyTheReservedSize)
{
    const std::string rewritten = Rewrite(kLine, {kTraceSpanHex, std::numeric_limits<uint64_t>::max()});
    EXPECT_EQ(rewritten.size(), kLineBody.size() + tracing::kMaxLogContextSuffixBytes);
    EXPECT_EQ(rewritten.back(), '\n');
}

TEST(LogContextSuffixTest, LongestMuduoLineStillFits)
{
    // muduo 的 LogStream 单行上限 4000 字节,放得下的行最长 3999;被截断的行尾可能没有换行。
    const std::string line(3999, 'x');
    std::vector<char> out(4000 + tracing::kMaxLogContextSuffixBytes);
    const size_t written = tracing::AppendLogContextSuffix(
        line, {kTraceSpanHex, std::numeric_limits<uint64_t>::max()}, out.data(), out.size());
    ASSERT_EQ(written, line.size() + tracing::kMaxLogContextSuffixBytes);
    EXPECT_EQ(std::string_view(out.data(), line.size()), line);
    EXPECT_EQ(out[written - 1], '\n');
}
