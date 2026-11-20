// span_recording_test —— 接缝作用域记录 span 的契约(docs/design/distributed-tracing.md §10.3)。
//
// 被锁定的契约:
//   * 没装导出去处时一律不记录(作用域照常装上下文);装了才记录,flags 不带 sampled 的上下文不记录;
//   * ScopedRoot 记 server 根 span:没有父,parent_is_remote 为假;ScopedIncoming 接续远端上下文时父是对端的 span,
//     parent_is_remote 为真;收到的上下文无效时开根;不带名字的构造不记录;
//   * PendingSpan 是当前 span 的子 span(同 trace、新 span_id),End 恰好一次,移走后原对象不再记录;父无效时是空 span;
//   * ScopedSpan(captured) 原样装回(同一个 span_id),在它里面开的 client span 以捕获的那个已导出 span 为父;
//   * MarkCurrentSpanError 只标最内层的在录 span;恢复作用域与不录的作用域里什么都不标;
//   * TakeCurrentServerSpan 只接最内层、类型与来源都对得上的在录 server span:接走后作用域退出不再结束它,
//     由接手方在回复发出时结束(同一个 span_id、同一个父);对不上的返回空 span,作用域照旧结束自己的;
//   * 名字按 "head/tail" 拼,超长截断;结束时刻不早于开始时刻。
//
// 与 rpc_controller_test.cpp 同一工程、同一个 main。夹具在开始和结束时复位本线程的 trace,并装 / 摘测试用的去处。

#include <gtest/gtest.h>

#include <string>
#include <string_view>

#include "core/tracing/span_record.h"
#include "core/tracing/trace_context.h"
#include "span_test_support.h"
#include "thread_context/trace_context_tls.h"

namespace {

using tracing::SpanKind;
using tracing::SpanName;
using tracing::SpanSystem;

class SpanRecordingTest : public ::testing::Test {
protected:
    void SetUp() override { tracing::Set(tracing::TraceContext{}); }
    void TearDown() override { tracing::Set(tracing::TraceContext{}); }

    span_test::RecordingSink sink_;
};

} // namespace

TEST_F(SpanRecordingTest, NothingIsRecordedWithoutAnInstalledSink)
{
    ASSERT_EQ(tracing::InstalledSpanSink(), nullptr);
    {
        tracing::ScopedRoot root(SpanSystem::kGateClient, SpanName{"gate.connect"});
        EXPECT_TRUE(tracing::Current().IsValid()) << "上下文照常装上,只是不记录";
        auto client = tracing::PendingSpan::StartChildOf(tracing::Current(), SpanKind::kClient, SpanSystem::kRpc0,
                                                         SpanName{"Scene", "Enter"});
        EXPECT_TRUE(client.Context().IsValid());
        EXPECT_FALSE(client.Recording());
        client.End();
    }
    EXPECT_EQ(sink_.Count(), 0u);
}

TEST_F(SpanRecordingTest, ScopedRootRecordsAServerRootSpan)
{
    const span_test::ScopedSinkInstall install(&sink_);
    tracing::TraceContext inside;
    {
        tracing::ScopedRoot root(SpanSystem::kGateClient, SpanName{"ClientPlayerLogin", "EnterGame"});
        inside = tracing::Current();
    }
    ASSERT_EQ(sink_.Count(), 1u);
    const tracing::SpanRecord span = sink_.Spans()[0];
    EXPECT_EQ(span.trace_id.bytes, inside.trace_id.bytes);
    EXPECT_EQ(span.span_id.bytes, inside.span_id.bytes);
    EXPECT_FALSE(span.parent_span_id.IsValid()) << "根 span 没有父";
    EXPECT_FALSE(span.parent_is_remote);
    EXPECT_EQ(span.kind, SpanKind::kServer);
    EXPECT_EQ(span.system, SpanSystem::kGateClient);
    EXPECT_EQ(span.Name(), "ClientPlayerLogin/EnterGame");
    EXPECT_EQ(span.trace_flags, 0x01);
    EXPECT_EQ(span.status, tracing::SpanStatus::kUnset);
    EXPECT_GT(span.start_unix_nano, 0u);
    EXPECT_GE(span.end_unix_nano, span.start_unix_nano);
    EXPECT_FALSE(tracing::Current().IsValid()) << "作用域退出后恢复";
}

TEST_F(SpanRecordingTest, ScopedIncomingContinuesARemoteParentAndRootsAnInvalidOne)
{
    const span_test::ScopedSinkInstall install(&sink_);
    const tracing::TraceContext upstream = tracing::NewRoot();
    {
        tracing::ScopedIncoming incoming(upstream, SpanKind::kServer, SpanSystem::kRpc0, SpanName{"Scene", "Process"});
    }
    {
        tracing::ScopedIncoming incoming(tracing::TraceContext{}, SpanKind::kConsumer, SpanSystem::kKafka,
                                         SpanName{"gate-cmd_g1"});
    }
    const auto spans = sink_.Spans();
    ASSERT_EQ(spans.size(), 2u);
    EXPECT_EQ(spans[0].trace_id.bytes, upstream.trace_id.bytes);
    EXPECT_EQ(spans[0].parent_span_id.bytes, upstream.span_id.bytes);
    EXPECT_NE(spans[0].span_id.bytes, upstream.span_id.bytes);
    EXPECT_TRUE(spans[0].parent_is_remote);
    EXPECT_EQ(spans[0].kind, SpanKind::kServer);

    EXPECT_NE(spans[1].trace_id.bytes, upstream.trace_id.bytes) << "无效上下文开新根";
    EXPECT_FALSE(spans[1].parent_span_id.IsValid());
    EXPECT_FALSE(spans[1].parent_is_remote);
    EXPECT_EQ(spans[1].kind, SpanKind::kConsumer);
    EXPECT_EQ(spans[1].Name(), "gate-cmd_g1");
}

TEST_F(SpanRecordingTest, UnsampledContextIsContinuedButNotRecorded)
{
    const span_test::ScopedSinkInstall install(&sink_);
    tracing::TraceContext upstream = tracing::NewRoot();
    upstream.flags = 0x00;   // 上游显式不采:按 ParentBased 不记录
    {
        tracing::ScopedIncoming incoming(upstream, SpanKind::kServer, SpanSystem::kGrpc, SpanName{"svc/M"});
        EXPECT_EQ(tracing::Current().trace_id.bytes, upstream.trace_id.bytes);
        auto client = tracing::PendingSpan::StartChildOf(tracing::Current(), SpanKind::kClient, SpanSystem::kGrpc,
                                                         SpanName{"svc/N"});
        EXPECT_TRUE(client.Context().IsValid());
        EXPECT_FALSE(client.Recording());
        client.End();
    }
    EXPECT_EQ(sink_.Count(), 0u);
}

TEST_F(SpanRecordingTest, UnnamedScopesDoNotRecord)
{
    const span_test::ScopedSinkInstall install(&sink_);
    {
        tracing::ScopedRoot root;
        tracing::ScopedIncoming incoming(tracing::NewRoot());
        tracing::ScopedSpan child;
    }
    EXPECT_EQ(sink_.Count(), 0u);
}

TEST_F(SpanRecordingTest, PendingSpanIsAChildOfTheCallerAndEndsExactlyOnce)
{
    const span_test::ScopedSinkInstall install(&sink_);
    tracing::TraceContext caller;
    tracing::TraceContext onWire;
    {
        tracing::ScopedRoot root(SpanSystem::kGateClient, SpanName{"root"});
        caller = tracing::Current();
        auto client = tracing::PendingSpan::StartChildOf(caller, SpanKind::kClient, SpanSystem::kRpc0,
                                                         SpanName{"Scene", "EnterScene"});
        ASSERT_TRUE(client.Recording());
        onWire = client.Context();
        EXPECT_EQ(onWire.trace_id.bytes, caller.trace_id.bytes);
        EXPECT_NE(onWire.span_id.bytes, caller.span_id.bytes);
        EXPECT_EQ(tracing::Current().span_id.bytes, caller.span_id.bytes) << "client span 不装进线程上下文";

        tracing::PendingSpan moved = std::move(client);
        client.End();   // 移走之后原对象不再记录
        moved.SetError("kTimeout");
        moved.End();
        moved.End();    // 只生效一次
    }
    const auto spans = sink_.Spans();
    ASSERT_EQ(spans.size(), 2u) << "client span 一条 + 根 span 一条";
    const tracing::SpanRecord& client = spans[0];
    EXPECT_EQ(client.kind, SpanKind::kClient);
    EXPECT_EQ(client.span_id.bytes, onWire.span_id.bytes);
    EXPECT_EQ(client.parent_span_id.bytes, caller.span_id.bytes);
    EXPECT_FALSE(client.parent_is_remote);
    EXPECT_EQ(client.status, tracing::SpanStatus::kError);
    EXPECT_EQ(client.StatusMessage(), "kTimeout");
    EXPECT_EQ(client.Name(), "Scene/EnterScene");
    EXPECT_EQ(spans[1].span_id.bytes, caller.span_id.bytes);
}

TEST_F(SpanRecordingTest, PendingSpanWithoutAParentIsEmpty)
{
    const span_test::ScopedSinkInstall install(&sink_);
    auto client = tracing::PendingSpan::StartChildOf(tracing::TraceContext{}, SpanKind::kProducer, SpanSystem::kKafka,
                                                     SpanName{"topic"});
    EXPECT_FALSE(client.Context().IsValid()) << "不在 trace 里的发送不凭空开根";
    EXPECT_FALSE(client.Recording());
    client.End();
    EXPECT_EQ(sink_.Count(), 0u);
}

TEST_F(SpanRecordingTest, RestoreInstallsTheCapturedSpanSoChildrenAttachToAnExportedParent)
{
    const span_test::ScopedSinkInstall install(&sink_);
    tracing::TraceContext captured;
    {
        tracing::ScopedRoot root(SpanSystem::kGateClient, SpanName{"root"});
        captured = tracing::Current();
    }
    sink_.Clear();
    {
        const tracing::ScopedSpan restore(captured);
        EXPECT_EQ(tracing::Current().trace_id.bytes, captured.trace_id.bytes);
        EXPECT_EQ(tracing::Current().span_id.bytes, captured.span_id.bytes) << "原样装回,不另建只记日志的子 span";
        auto client = tracing::PendingSpan::StartChildOf(tracing::Current(), SpanKind::kClient, SpanSystem::kGrpc,
                                                         SpanName{"pkg.Svc/Method"});
        client.End();
    }
    auto continuation = tracing::Bind([] {
        auto producer = tracing::PendingSpan::StartChildOf(tracing::Current(), SpanKind::kProducer, SpanSystem::kKafka,
                                                           SpanName{"topic"});
        producer.End();
    });
    {
        // 在别的 trace 里执行 Bind 包过的回调:里面看到的仍是包装时(这里是没有 trace)的上下文。
        tracing::ScopedRoot other;
        continuation();
    }
    tracing::Set(captured);
    auto boundUnderCaptured = tracing::Bind([] {
        auto producer = tracing::PendingSpan::StartChildOf(tracing::Current(), SpanKind::kProducer, SpanSystem::kKafka,
                                                           SpanName{"topic"});
        producer.End();
    });
    tracing::Set(tracing::TraceContext{});
    boundUnderCaptured();

    const auto spans = sink_.Spans();
    ASSERT_EQ(spans.size(), 2u) << "没有 trace 的那次续跑不产生 span";
    for (const auto& span : spans) {
        EXPECT_EQ(span.trace_id.bytes, captured.trace_id.bytes);
        EXPECT_EQ(span.parent_span_id.bytes, captured.span_id.bytes) << "父是导出过的那个 span,不是孤儿";
    }
}

TEST_F(SpanRecordingTest, MarkCurrentSpanErrorMarksOnlyTheInnermostRecordingSpan)
{
    const span_test::ScopedSinkInstall install(&sink_);
    tracing::TraceContext captured;
    {
        tracing::ScopedRoot root(SpanSystem::kGateClient, SpanName{"outer"});
        captured = tracing::Current();
        {
            tracing::ScopedIncoming inner(tracing::Current(), SpanKind::kServer, SpanSystem::kRpc0, SpanName{"inner"});
            tracing::MarkCurrentSpanError("NO_METHOD");
        }
        {
            const tracing::ScopedSpan restore(captured);   // 恢复作用域里不标任何在录 span
            tracing::MarkCurrentSpanError("ignored");
        }
        {
            tracing::ScopedIncoming unnamed(tracing::Current());   // 不录的作用域同样不标外层
            tracing::MarkCurrentSpanError("ignored");
        }
    }
    const auto spans = sink_.Spans();
    ASSERT_EQ(spans.size(), 2u);
    EXPECT_EQ(spans[0].Name(), "inner");
    EXPECT_EQ(spans[0].status, tracing::SpanStatus::kError);
    EXPECT_EQ(spans[0].StatusMessage(), "NO_METHOD");
    EXPECT_EQ(spans[1].Name(), "outer");
    EXPECT_EQ(spans[1].status, tracing::SpanStatus::kUnset);

    tracing::MarkCurrentSpanError("no span");   // 作用域都退出了:什么都不做
    EXPECT_EQ(sink_.Count(), 2u);
}

TEST_F(SpanRecordingTest, TakenServerSpanOutlivesItsDispatchScopeAndEndsWhereTheReplyIsSent)
{
    const span_test::ScopedSinkInstall install(&sink_);
    const tracing::TraceContext upstream = tracing::NewRoot();
    tracing::TraceContext dispatched;
    tracing::PendingSpan reply;
    {
        tracing::ScopedIncoming dispatch(upstream, SpanKind::kServer, SpanSystem::kRpc0, SpanName{"Scene", "PrepareBattle"});
        dispatched = tracing::Current();
        reply = tracing::PendingSpan::TakeCurrentServerSpan(SpanSystem::kRpc0);
        ASSERT_TRUE(reply.Recording());
        EXPECT_EQ(reply.Context().span_id.bytes, dispatched.span_id.bytes) << "接过来的就是派发的 server span";
        EXPECT_EQ(tracing::Current().span_id.bytes, dispatched.span_id.bytes) << "作用域照旧装着它的上下文";
        tracing::MarkCurrentSpanError("ignored");   // 接走之后当前线程上没有在录的 span
    }
    EXPECT_EQ(sink_.Count(), 0u) << "派发作用域退出时不结束接走的 span";

    reply.SetError("HANDLER_FAILED");
    reply.End();
    ASSERT_EQ(sink_.Count(), 1u);
    const tracing::SpanRecord span = sink_.Spans()[0];
    EXPECT_EQ(span.span_id.bytes, dispatched.span_id.bytes);
    EXPECT_EQ(span.parent_span_id.bytes, upstream.span_id.bytes);
    EXPECT_TRUE(span.parent_is_remote);
    EXPECT_EQ(span.kind, SpanKind::kServer);
    EXPECT_EQ(span.Name(), "Scene/PrepareBattle");
    EXPECT_EQ(span.status, tracing::SpanStatus::kError);
    EXPECT_EQ(span.StatusMessage(), "HANDLER_FAILED") << "接走之后的 MarkCurrentSpanError 没标到它";
}

TEST_F(SpanRecordingTest, TakeCurrentServerSpanTakesOnlyAMatchingInnermostRecordingServerSpan)
{
    const span_test::ScopedSinkInstall install(&sink_);
    {
        tracing::ScopedIncoming grpc(tracing::NewRoot(), SpanKind::kServer, SpanSystem::kGrpc, SpanName{"pkg.Svc/M"});
        EXPECT_FALSE(tracing::PendingSpan::TakeCurrentServerSpan(SpanSystem::kRpc0).Recording()) << "来源不对";
        {
            const tracing::ScopedSpan nested(tracing::Current());   // 不录的嵌套作用域:最内层没有在录的 span
            EXPECT_FALSE(tracing::PendingSpan::TakeCurrentServerSpan(SpanSystem::kGrpc).Recording());
        }
    }
    {
        tracing::ScopedIncoming consumer(tracing::NewRoot(), SpanKind::kConsumer, SpanSystem::kKafka, SpanName{"topic"});
        EXPECT_FALSE(tracing::PendingSpan::TakeCurrentServerSpan(SpanSystem::kKafka).Recording()) << "consumer 不是 server";
    }
    EXPECT_FALSE(tracing::PendingSpan::TakeCurrentServerSpan(SpanSystem::kGrpc).Recording()) << "作用域外什么都没有";
    EXPECT_EQ(sink_.Count(), 2u) << "没被接走的照旧在作用域退出时结束";
}

TEST_F(SpanRecordingTest, SpanRecordJoinsAndTruncatesNames)
{
    tracing::SpanRecord record;
    record.SetName(SpanName{"Svc", "Method"});
    EXPECT_EQ(record.Name(), "Svc/Method");
    record.SetName(SpanName{"only-head"});
    EXPECT_EQ(record.Name(), "only-head");

    const std::string longHead(tracing::kSpanNameCapacity - 2, 'a');
    record.SetName(SpanName{longHead, "tail"});
    EXPECT_EQ(record.Name().size(), tracing::kSpanNameCapacity);
    EXPECT_EQ(record.Name(), longHead + "/t");

    record.SetStatusMessage(std::string(200, 'x'));
    EXPECT_EQ(record.StatusMessage().size(), tracing::kSpanStatusMessageCapacity);
}
