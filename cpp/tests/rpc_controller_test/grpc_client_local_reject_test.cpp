// grpc_client_local_reject_test — 生成的 gRPC 客户端「本地拒绝」路径(docs/design/grpc-muduo-transport.md §14)。
//
// 节点实体上没有可用通道(没挂 MuduoGrpcChannelPtr、挂的是空指针、实体已销毁)时,生成的 Send 不碰网络,
// 经 QueueGrpcLocalCompletion 把 UNAVAILABLE 排到下一轮投递。这里锁定调用方契约(grpc_client_call.h 的 GrpcDone 一节):
//   * 完成回调恰好一次,从不在 Send 栈内同步执行,排在发起 Send 的那一批 pending functor 之后;
//   * outcome.code == UNAVAILABLE,reply 为空;
//   * typed / Erased 调用都必须显式传 done;Erased 请求的动态类型不匹配时异步以 INTERNAL 完成;
//   * 回调执行期间 GrpcClientLocalCompletionsDrained() 为假(Node 拆除据此让一轮),执行完为真;
//     回调里再发的那次本地完成同样计入,直到它也执行完。
//
// 不需要任何服务端:被测路径在取通道那一步就结束。经真实 muduo_grpc 通道的成功 / 截止 / 关机用例
// (原 grpc_client_lifecycle_test.cpp 的 PerCallDone* 等)已由第 4 批用 muduo_grpc::Server 回环重写,
// 见同目录 grpc_client_loopback_test.cpp;通道自身的契约(析构时在途调用 UNAVAILABLE、拒连快速失败等)在 muduo_grpc_test。
//
// 与 rpc_controller_test.cpp 同一工程、同一个 main。EventLoop 建在测试线程上(一个线程只能有一个),
// Send 只在 loop 线程上调用:QueueGrpcLocalCompletion 取的是当前线程的 loop。

#include <gtest/gtest.h>
#include <google/protobuf/dynamic_message.h>

#include <cstddef>
#include <cstdint>
#include <memory>
#include <string>
#include <vector>

#include "core/network/grpc_client_runtime.h"
#include "grpc_client/data_service/data_service_grpc_client.h"
#include "muduo/net/EventLoop.h"
#include "thread_context/ecs_context.h"

namespace
{

using data_service::GetPlayerHomeZoneRequest;
using data_service::GetPlayerHomeZoneResponse;

class GrpcClientLocalRejectTest : public ::testing::Test
{
protected:
    // 转 loop 到收尾:调用前已排队的 functor 是同一批,它们再排的(本地完成)在下一批。0.5s 后 quit,
    // 期间完成回调之后还要空转很多轮,「恰好一次」不是刚好赶上 quit 得出的。
    // 5ms 空转定时器只为让 loop 勤转、不依赖 wakeup:在 loop 线程上、pending functor 之外排队不会唤醒 poll。
    void RunLoopUntilQuiet()
    {
        const auto ticker = loop_.runEvery(0.005, [] {});
        loop_.runAfter(0.5, [&loop = loop_] { loop.quit(); });
        loop_.loop();
        loop_.cancel(ticker);
        // 仓库测试约定(.github/copilot-instructions.md 的 TimerTaskComp 段):loop 退出后、loop 仍存活时清掉实体定时器。
        tlsEcs.actorRegistry.clear();
    }

    // 最先构造、最后析构:其余成员与排队的回调都在 loop 还在时处理掉。
    muduo::net::EventLoop loop_;
    // 节点注册表的替身:生成的 Send 只按 (registry, entity) 取通道,不需要 tlsNodeContextManager。
    entt::registry registry_;
};

// 每次调用回调版 Send 打到没挂通道的节点实体:done 以 UNAVAILABLE 异步完成恰好一次,reply 为空;
// 完成排在发起 Send 的那一批 pending functor 之后,执行期间排空判据关闭,执行完打开。
TEST_F(GrpcClientLocalRejectTest, PerCallDoneOnEntityWithoutChannelIsDeferredOnceWithUnavailable)
{
    const auto nodeWithoutChannel = registry_.create();
    // 回调只在 loop 线程执行,不需要原子量。
    struct Observed
    {
        bool sendReturned{false};
        bool laterFunctorInSameBatchRan{false};
        int calls{0};
        uint32_t code{kGrpcStatusOk};
        bool hadReply{true};
        bool sendHadReturnedAtDone{false};
        bool sameBatchHadRunAtDone{false};
        bool drainedDuringDone{true};
    };
    const auto observed = std::make_shared<Observed>();

    loop_.queueInLoop([&registry = registry_, nodeWithoutChannel, observed]
    {
        GetPlayerHomeZoneRequest request;
        request.set_player_id(42);
        data_service::SendDataServiceGetPlayerHomeZone(
            registry, nodeWithoutChannel, request,
            [observed](const GrpcCallOutcome &outcome, const GetPlayerHomeZoneResponse *reply)
            {
                ++observed->calls;
                observed->code = outcome.code;
                observed->hadReply = reply != nullptr;
                observed->sendHadReturnedAtDone = observed->sendReturned;
                observed->sameBatchHadRunAtDone = observed->laterFunctorInSameBatchRan;
                observed->drainedDuringDone = GrpcClientLocalCompletionsDrained();
            });
        observed->sendReturned = true;
        EXPECT_EQ(observed->calls, 0) << "done 不得在 Send 栈内同步调用";
        EXPECT_FALSE(GrpcClientLocalCompletionsDrained()) << "排队中的本地完成要计入排空判据";
    });
    // 与 Send 同一批、排在它后面:本地完成排进的是下一批,这里还不能已经执行。
    loop_.queueInLoop([observed]
    {
        observed->laterFunctorInSameBatchRan = true;
        EXPECT_EQ(observed->calls, 0);
        EXPECT_FALSE(GrpcClientLocalCompletionsDrained());
    });
    RunLoopUntilQuiet();

    EXPECT_EQ(observed->calls, 1) << "恰好一次";
    EXPECT_EQ(observed->code, kGrpcStatusUnavailable);
    EXPECT_FALSE(observed->hadReply) << "非 OK 时 reply 为空";
    EXPECT_TRUE(observed->sendHadReturnedAtDone);
    EXPECT_TRUE(observed->sameBatchHadRunAtDone) << "完成排在下一轮,不在发起 Send 的这批 pending functor 里";
    EXPECT_FALSE(observed->drainedDuringDone) << "回调退栈前排空判据仍须关闭";
    EXPECT_TRUE(GrpcClientLocalCompletionsDrained()) << "回调执行完计数归零";
}

// 三种「没有可用通道」的实体:没挂组件、挂了空指针、实体已销毁。每个显式 done 都以 UNAVAILABLE
// 异步完成恰好一次;完成不依赖请求对象在 Send 返回后继续存活。
TEST_F(GrpcClientLocalRejectTest, PerCallDoneRejectsAllThreeMissingChannelForms)
{
    const auto withoutComponent = registry_.create();
    const auto withNullChannel = registry_.create();
    registry_.emplace<MuduoGrpcChannelPtr>(withNullChannel);
    const auto destroyed = registry_.create();
    registry_.destroy(destroyed);
    const std::vector<entt::entity> targets{withoutComponent, withNullChannel, destroyed};

    struct Observed
    {
        bool allSendsReturned{false};
        std::vector<int> calls;
    };
    const auto observed = std::make_shared<Observed>();
    observed->calls.resize(targets.size(), 0);

    loop_.queueInLoop([&registry = registry_, targets, observed]
    {
        for (std::size_t index = 0; index < targets.size(); ++index)
        {
            GetPlayerHomeZoneRequest request;
            request.set_player_id(42);
            data_service::SendDataServiceGetPlayerHomeZone(registry, targets[index], request,
                [observed, index](const GrpcCallOutcome &outcome, const GetPlayerHomeZoneResponse *reply)
                {
                    ++observed->calls[index];
                    EXPECT_TRUE(observed->allSendsReturned) << "缺通道也不得同步完成";
                    EXPECT_EQ(outcome.code, kGrpcStatusUnavailable);
                    EXPECT_TRUE(outcome.initialMetadata.empty()) << "本地拒绝没有响应头";
                    EXPECT_EQ(reply, nullptr);
                    EXPECT_FALSE(GrpcClientLocalCompletionsDrained());
                });
            EXPECT_EQ(observed->calls[index], 0) << "缺通道也不能在 Send 栈内同步回调";
        }
        observed->allSendsReturned = true;
    });
    RunLoopUntilQuiet();

    for (const int calls : observed->calls)
    {
        EXPECT_EQ(calls, 1) << "每个目标的完成回调都恰好一次";
    }
    EXPECT_TRUE(GrpcClientLocalCompletionsDrained());
}

// Erased 入口在获取通道前校验请求类型。错误动态类型必须异步回 INTERNAL,不得退化成缺通道的 UNAVAILABLE。
TEST_F(GrpcClientLocalRejectTest, ErasedWrongRequestTypeIsDeferredOnceWithInternal)
{
    const auto nodeWithoutChannel = registry_.create();
    struct Observed
    {
        bool sendReturned{false};
        bool laterFunctorInSameBatchRan{false};
        int calls{0};
        uint32_t code{kGrpcStatusOk};
        std::string message;
        bool hadReply{true};
        bool sendHadReturnedAtDone{false};
        bool sameBatchHadRunAtDone{false};
        bool drainedDuringDone{true};
    };
    const auto observed = std::make_shared<Observed>();

    loop_.queueInLoop([&registry = registry_, nodeWithoutChannel, observed]
    {
        const GetPlayerHomeZoneResponse wrongRequest;
        data_service::SendDataServiceGetPlayerHomeZoneErased(registry, nodeWithoutChannel, wrongRequest,
            [observed](const GrpcCallOutcome &outcome, const google::protobuf::Message *reply)
            {
                ++observed->calls;
                observed->code = outcome.code;
                observed->message = outcome.message;
                observed->hadReply = reply != nullptr;
                observed->sendHadReturnedAtDone = observed->sendReturned;
                observed->sameBatchHadRunAtDone = observed->laterFunctorInSameBatchRan;
                observed->drainedDuringDone = GrpcClientLocalCompletionsDrained();
            }, GrpcCallOptions{});
        observed->sendReturned = true;
        EXPECT_EQ(observed->calls, 0) << "类型错误也不能在 Send 栈内同步回调";
        EXPECT_FALSE(GrpcClientLocalCompletionsDrained());
    });
    loop_.queueInLoop([observed]
    {
        observed->laterFunctorInSameBatchRan = true;
        EXPECT_EQ(observed->calls, 0);
    });
    RunLoopUntilQuiet();

    EXPECT_EQ(observed->calls, 1);
    EXPECT_EQ(observed->code, kGrpcStatusInternal);
    EXPECT_EQ(observed->message, "request type mismatch");
    EXPECT_FALSE(observed->hadReply);
    EXPECT_TRUE(observed->sendHadReturnedAtDone);
    EXPECT_TRUE(observed->sameBatchHadRunAtDone);
    EXPECT_FALSE(observed->drainedDuringDone);
    EXPECT_TRUE(GrpcClientLocalCompletionsDrained());
}

// 同一个 descriptor 也可能对应 DynamicMessage。Erased 入口不能仅凭 descriptor 相同就 static_cast 成生成类型。
TEST_F(GrpcClientLocalRejectTest, ErasedDynamicMessageWithMatchingDescriptorIsDeferredOnceWithInternal)
{
    const auto nodeWithoutChannel = registry_.create();
    struct Observed
    {
        bool sendReturned{false};
        int calls{0};
        uint32_t code{kGrpcStatusOk};
        bool hadReply{true};
        bool sendHadReturnedAtDone{false};
        bool drainedDuringDone{true};
    };
    const auto observed = std::make_shared<Observed>();

    loop_.queueInLoop([&registry = registry_, nodeWithoutChannel, observed]
    {
        // 默认不委托 generated factory,New() 返回 DynamicMessage,descriptor 仍是生成请求的同一个对象。
        google::protobuf::DynamicMessageFactory factory;
        const auto *prototype = factory.GetPrototype(GetPlayerHomeZoneRequest::descriptor());
        ASSERT_NE(prototype, nullptr);
        const std::unique_ptr<google::protobuf::Message> dynamicRequest(prototype->New());
        ASSERT_NE(dynamicRequest, nullptr);
        ASSERT_EQ(dynamicRequest->GetDescriptor(), GetPlayerHomeZoneRequest::descriptor());
        ASSERT_EQ(dynamic_cast<const GetPlayerHomeZoneRequest *>(dynamicRequest.get()), nullptr);

        data_service::SendDataServiceGetPlayerHomeZoneErased(registry, nodeWithoutChannel, *dynamicRequest,
            [observed](const GrpcCallOutcome &outcome, const google::protobuf::Message *reply)
            {
                ++observed->calls;
                observed->code = outcome.code;
                observed->hadReply = reply != nullptr;
                observed->sendHadReturnedAtDone = observed->sendReturned;
                observed->drainedDuringDone = GrpcClientLocalCompletionsDrained();
            }, GrpcCallOptions{});
        observed->sendReturned = true;
        EXPECT_EQ(observed->calls, 0) << "动态类型错误也必须异步完成";
        EXPECT_FALSE(GrpcClientLocalCompletionsDrained()) << "已排队的错误完成计入排空";
    });
    RunLoopUntilQuiet();

    EXPECT_EQ(observed->calls, 1);
    EXPECT_EQ(observed->code, kGrpcStatusInternal) << "descriptor 相同不能绕过实际动态类型校验";
    EXPECT_FALSE(observed->hadReply);
    EXPECT_TRUE(observed->sendHadReturnedAtDone);
    EXPECT_FALSE(observed->drainedDuringDone);
    EXPECT_TRUE(GrpcClientLocalCompletionsDrained());
}

// 本地完成的回调里再发一次(失败后重试的形状):重发的本地完成在外层回调退栈前就计入排空判据,外层退栈后判据
// 仍关闭,直到重发那次也执行完 —— Node 拆除(FinalizeTeardownInLoop)靠这条才不会抢在重发的回调之前拆掉 ECS。
// 原 grpc_client_lifecycle_test.cpp 的 QueuedFailureKeepsOwnerAliveDuringReentryAndAfterCqHasDrained 在新传输下
// 仍成立的部分。重发按「重新选点、没有可用节点」写成 entt::null:完成回调不捕获节点实体句柄(§14.2)。
TEST_F(GrpcClientLocalRejectTest, SendFromLocalCompletionKeepsDrainGateClosedUntilItCompletes)
{
    const auto nodeWithoutChannel = registry_.create();
    struct Observed
    {
        int outerCalls{0};
        int innerCalls{0};
        uint32_t innerCode{kGrpcStatusOk};
        int innerCallsWhenSendReturned{-1};
        bool drainedAfterReentrantSend{true};
        bool probeRanBeforeInner{false};
        bool drainedAtProbe{true};
        bool drainedDuringInner{true};
    };
    const auto observed = std::make_shared<Observed>();

    loop_.queueInLoop([this, nodeWithoutChannel = nodeWithoutChannel, observed]
    {
        const GetPlayerHomeZoneRequest request;
        data_service::SendDataServiceGetPlayerHomeZone(
            registry_, nodeWithoutChannel, request,
            [this, observed](const GrpcCallOutcome &, const GetPlayerHomeZoneResponse *)
            {
                ++observed->outerCalls;
                // 先于重发排队:执行时外层回调已退栈,重发的本地完成排在它后面。
                loop_.queueInLoop([observed]
                {
                    observed->probeRanBeforeInner = observed->innerCalls == 0;
                    observed->drainedAtProbe = GrpcClientLocalCompletionsDrained();
                });
                const GetPlayerHomeZoneRequest retry;
                data_service::SendDataServiceGetPlayerHomeZone(
                    registry_, entt::entity{entt::null}, retry,
                    [observed](const GrpcCallOutcome &outcome, const GetPlayerHomeZoneResponse *)
                    {
                        ++observed->innerCalls;
                        observed->innerCode = outcome.code;
                        observed->drainedDuringInner = GrpcClientLocalCompletionsDrained();
                    });
                observed->innerCallsWhenSendReturned = observed->innerCalls;
                observed->drainedAfterReentrantSend = GrpcClientLocalCompletionsDrained();
            });
    });
    RunLoopUntilQuiet();

    EXPECT_EQ(observed->outerCalls, 1);
    EXPECT_EQ(observed->innerCalls, 1) << "重发也恰好完成一次";
    EXPECT_EQ(observed->innerCode, kGrpcStatusUnavailable);
    EXPECT_EQ(observed->innerCallsWhenSendReturned, 0) << "回调里的 Send 同样不得同步完成";
    EXPECT_FALSE(observed->drainedAfterReentrantSend);
    EXPECT_TRUE(observed->probeRanBeforeInner);
    EXPECT_FALSE(observed->drainedAtProbe) << "外层回调退栈后,排队中的重发仍须关住排空判据";
    EXPECT_FALSE(observed->drainedDuringInner);
    EXPECT_TRUE(GrpcClientLocalCompletionsDrained());
}

} // namespace
