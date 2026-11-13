#include <gtest/gtest.h>

#include <cstdint>
#include <string>
#include <string_view>
#include <utility>
#include <vector>

#include "core/tracing/trace_context.h"
#include "error_reporter/error_reporter.h"
#include "thread_context/trace_context_tls.h"

// error_reporter 的事件带日志上下文(docs/design/distributed-tracing.md §13 第 4 条,docs/design/error-reporting.md)。
// 只测进程内缓冲与 JSONL 序列化,不碰文件与信号。

namespace
{

constexpr std::string_view kTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01";
constexpr std::string_view kTraceIdHex = "4bf92f3577b34da6a3ce929d0e0e4736";
constexpr std::string_view kSpanIdHex = "00f067aa0ba902b7";

tracing::TraceContext KnownTrace()
{
	tracing::TraceContext ctx;
	EXPECT_TRUE(tracing::ParseTraceparent(kTraceparent, ctx));
	return ctx;
}

error_reporter::Event BaseEvent()
{
	error_reporter::Event e;
	e.wall_ms = 1700000000000;
	e.error_code = 1003;
	e.tag = "rpc_error_out";
	e.message = "method=Scene.Enter";
	return e;
}

constexpr std::string_view kBaseJsonPrefix =
	"{\"wall_ms\":1700000000000,\"error_code\":1003,\"tag\":\"rpc_error_out\",\"message\":\"method=Scene.Enter\"";

// 从全进程缓冲里取出 tag 为 tag 的事件。同一测试进程里别的用例(RPC 回环)也会往这个缓冲里记,
// 所以按本文件独有的 tag 找,不假设缓冲里只有一条。
std::vector<error_reporter::Event> DrainTagged(std::string_view tag)
{
	std::vector<error_reporter::Event> matched;
	for (error_reporter::Event& e : error_reporter::Instance().Drain())
	{
		if (e.tag == tag)
		{
			matched.push_back(std::move(e));
		}
	}
	return matched;
}

} // namespace

TEST(ErrorReporterJsonl, OmitsContextFieldsWhenAbsent)
{
	EXPECT_EQ(error_reporter::EventToJsonl(BaseEvent()), std::string(kBaseJsonPrefix) + "}");
}

TEST(ErrorReporterJsonl, WritesTraceAndPlayerAsStrings)
{
	error_reporter::Event e = BaseEvent();
	e.trace = KnownTrace();
	e.player_id = 18446744073709551615ULL; // uint64 最大值,超过 2^53:写成字符串才不丢精度
	EXPECT_EQ(error_reporter::EventToJsonl(e),
			  std::string(kBaseJsonPrefix) + ",\"trace_id\":\"" + std::string(kTraceIdHex) + "\",\"span_id\":\"" +
				  std::string(kSpanIdHex) + "\",\"player_id\":\"18446744073709551615\"}");
}

TEST(ErrorReporterJsonl, PlayerWithoutTraceWritesOnlyThePlayer)
{
	error_reporter::Event e = BaseEvent();
	e.player_id = 42;
	EXPECT_EQ(error_reporter::EventToJsonl(e), std::string(kBaseJsonPrefix) + ",\"player_id\":\"42\"}");
}

TEST(ErrorReporterBuffer, RecordKeepsTheGivenContext)
{
	error_reporter::Buffer buffer;
	buffer.Record(7, "t", "m", KnownTrace(), 42);
	const std::vector<error_reporter::Event> events = buffer.Drain();
	ASSERT_EQ(events.size(), 1u);
	EXPECT_EQ(events[0].trace.trace_id.ToHex(), kTraceIdHex);
	EXPECT_EQ(events[0].trace.span_id.ToHex(), kSpanIdHex);
	EXPECT_EQ(events[0].player_id, 42u);
}

TEST(ErrorReporterRecord, CapturesTheCallingThreadsTraceAndPlayer)
{
	std::string expectedSpanHex;
	{
		const tracing::ScopedIncoming incoming(KnownTrace());
		const tracing::ScopedPlayerId player(42);
		expectedSpanHex = tracing::Current().span_id.ToHex();
		error_reporter::Record(7, "error_reporter_test_traced", "m");
	}
	const std::vector<error_reporter::Event> events = DrainTagged("error_reporter_test_traced");
	ASSERT_EQ(events.size(), 1u);
	// ScopedIncoming 在收到的 trace 下建子 span:trace_id 不变,span_id 是录入那一刻的当前 span。
	EXPECT_EQ(events[0].trace.trace_id.ToHex(), kTraceIdHex);
	EXPECT_EQ(events[0].trace.span_id.ToHex(), expectedSpanHex);
	EXPECT_EQ(events[0].player_id, 42u);
}

TEST(ErrorReporterRecord, OutsideAnyScopeCarriesNoContext)
{
	error_reporter::Record(7, "error_reporter_test_untraced", "m");
	const std::vector<error_reporter::Event> events = DrainTagged("error_reporter_test_untraced");
	ASSERT_EQ(events.size(), 1u);
	EXPECT_FALSE(events[0].trace.IsValid());
	EXPECT_EQ(events[0].player_id, 0u);
}
