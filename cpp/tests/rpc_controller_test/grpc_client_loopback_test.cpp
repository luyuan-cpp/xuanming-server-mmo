// grpc_client_loopback_test — 生成的 gRPC 客户端经真实 muduo_grpc 通道打到同进程 muduo_grpc::Server 的回环用例
// (docs/design/grpc-muduo-transport.md §14、§15)。
//
// 服务端:muduo_grpc::Server 在 127.0.0.1 上提供 /data_service.DataService/GetPlayerHomeZone,方法体走
// core 的 GrpcUnaryCall 胶水(与生成的 scene / battle 骨架同一条路径:Create → ParseRequest → Proceed)。
// 客户端:节点实体上挂真实的 MuduoGrpcChannelPtr,经生成的 SendDataServiceGetPlayerHomeZone(带每次调用 done)发出。
// 锁定的契约:
//   * 成功:done 恰好一次、不在 Send 栈内同步执行,应答字段正确;
//   * 服务端把调用留着不进业务(守护段式延后)而客户端 deadline 到期:DEADLINE_EXCEEDED 恰好一次;服务端随后
//     把这次调用收口(截止定时器或对端 RST),迟到的 Proceed 不再进业务;
//   * 业务已开始、延后完成的调用遇到服务端关机:排空到期回 UNAVAILABLE "server shutting down",onDrained 恰好一次,
//     迟到的 FinishOk 被吞掉,done 不会第二次执行。
//
// 全部在测试线程自己的 EventLoop 上:Server、通道、连接都不起线程。等待一律以 5s 作活性上限,判据只看事件与计数。
// 销毁顺序:通道先于 Server(先转 loop 让连接断开、排队的完成回调跑完),Server 在 loop 外析构(server.h 的前置条件),
// 之后再转 loop 让它排进的 forceClose / connectDestroyed 跑完;EventLoop 最先构造、最后析构。
//
// 与 rpc_controller_test.cpp 同一工程、同一个 main。

#include <gtest/gtest.h>

#ifdef _WIN32
#include <winsock2.h>
#include <ws2tcpip.h>
#else
#include <arpa/inet.h>
#include <netinet/in.h>
#include <sys/socket.h>
#include <unistd.h>
#endif

#include <chrono>
#include <cstdint>
#include <functional>
#include <memory>
#include <string>
#include <string_view>
#include <utility>
#include <vector>

// muduo 头放在标准库 / 系统头之后(它的 Windows 适配头 #define 了 off_t / pid_t 等名字)。
#include "core/network/grpc_client_runtime.h"
#include "core/network/grpc_unary_call.h"
#include "grpc_client/data_service/data_service_grpc_client.h"
#include "muduo/net/EventLoop.h"
#include "muduo/net/TimerId.h"
#include "network/node_utils.h"
#include "thread_context/node_context_manager.h"
#include "muduo_grpc/channel.h"
#include "muduo_grpc/method_table.h"
#include "muduo_grpc/server.h"
#include "muduo_grpc/server_call.h"

using namespace std::chrono_literals;

namespace
{

using data_service::GetPlayerHomeZoneRequest;
using data_service::GetPlayerHomeZoneResponse;
using GetPlayerHomeZoneCall = GrpcUnaryCall<GetPlayerHomeZoneRequest, GetPlayerHomeZoneResponse>;

constexpr auto kLiveness = 5s;   // 只作活性上限,不作判据

// 向 OS 借一个空闲回环端口(Server::Create 不接受 0 端口)。失败返回 0。
uint16_t FindFreeLoopbackPort()
{
#ifdef _WIN32
    const SOCKET s = ::socket(AF_INET, SOCK_STREAM, IPPROTO_TCP);
    if (s == INVALID_SOCKET)
    {
        return 0;
    }
    int len = sizeof(sockaddr_in);
#else
    const int s = ::socket(AF_INET, SOCK_STREAM, IPPROTO_TCP);
    if (s < 0)
    {
        return 0;
    }
    socklen_t len = sizeof(sockaddr_in);
#endif
    sockaddr_in addr{};
    addr.sin_family = AF_INET;
    addr.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    addr.sin_port = 0;
    uint16_t port = 0;
    if (::bind(s, reinterpret_cast<sockaddr *>(&addr), sizeof(addr)) == 0 &&
        ::getsockname(s, reinterpret_cast<sockaddr *>(&addr), &len) == 0)
    {
        port = ntohs(addr.sin_port);
    }
#ifdef _WIN32
    ::closesocket(s);
#else
    ::close(s);
#endif
    return port;
}

// 测试服务:按 mode 决定方法体怎么完成调用。只在 loop 线程上使用,不需要原子量。
// 必须活过注册它的 Server(方法表闭包捕获 this),所以在夹具里先于 server_ 声明。
class LoopbackDataService final : public muduo_grpc::Service
{
public:
    enum class Mode : uint8_t
    {
        kReply,                    // 同步:Proceed → 业务 → FinishOk
        kHoldBeforeBusiness,       // 守护段式延后:不 Proceed,留着句柄
        kHoldAfterBusinessStarted, // 业务已开始、延后完成(§15.3 Q6):TryStartBusiness 后留着 ServerCall
    };

    void RegisterMethods(muduo_grpc::MethodTable &table) override
    {
        table.AddUnary(std::string(data_service::kDataServiceGetPlayerHomeZoneGrpcPath),
                       [this](muduo_grpc::ServerCall serverCall, std::string_view requestBytes)
                       { Dispatch(std::move(serverCall), requestBytes); });
    }

    Mode mode{Mode::kReply};
    int dispatched{0};
    int businessRuns{0};
    muduo_grpc::Metadata receivedMetadata;
    std::vector<std::shared_ptr<GetPlayerHomeZoneCall>> heldBeforeBusiness;
    std::vector<muduo_grpc::ServerCall> heldAfterBusinessStarted;

private:
    void Dispatch(muduo_grpc::ServerCall serverCall, std::string_view requestBytes)
    {
        ++dispatched;
        receivedMetadata = serverCall.metadata();
        const auto call = GetPlayerHomeZoneCall::Create(
            std::move(serverCall),
            [this](const GetPlayerHomeZoneRequest *request, GetPlayerHomeZoneResponse *response)
            {
                ++businessRuns;
                response->set_home_zone_id(static_cast<uint32_t>(request->player_id() + 1));
            });
        if (!call->ParseRequest(requestBytes))
        {
            return;
        }
        switch (mode)
        {
        case Mode::kReply:
            call->Proceed();
            return;
        case Mode::kHoldBeforeBusiness:
            heldBeforeBusiness.push_back(call);
            return;
        case Mode::kHoldAfterBusinessStarted:
            if (call->server_call().TryStartBusiness())
            {
                ++businessRuns;
                heldAfterBusinessStarted.push_back(call->server_call());
            }
            return;
        }
    }
};

// 一次客户端调用的观察结果。回调只在 loop 线程执行。
struct Observed
{
    bool sendReturned{false};
    int calls{0};
    uint32_t code{kGrpcStatusOk};
    std::string message;
    bool hadReply{false};
    uint32_t homeZoneId{0};
    bool sendHadReturnedAtDone{false};
};

class GrpcClientLoopbackTest : public ::testing::Test
{
protected:
    void SetUp() override
    {
        port_ = FindFreeLoopbackPort();
        ASSERT_NE(port_, 0) << "no free loopback port";

        muduo_grpc::ServerOptions options;
        options.name = "grpc_client_loopback";
        options.statsLogInterval = std::chrono::seconds(0);
        server_ = muduo_grpc::Server::Create(loop_, "127.0.0.1", port_, options);
        ASSERT_NE(server_, nullptr);
        ASSERT_TRUE(server_->RegisterService(service_));
        server_->Start();

        channel_ = muduo_grpc::Channel::Create(loop_, "127.0.0.1", port_);
        ASSERT_NE(channel_, nullptr);
        nodeEntity_ = registry_.create();
        registry_.emplace<MuduoGrpcChannelPtr>(nodeEntity_, channel_);
    }

    void TearDown() override
    {
        // 留着的服务端句柄先放掉(记录已不在时什么都不发;还在时按「句柄全部丢弃」收口),此时 Server 仍活着。
        service_.heldBeforeBusiness.clear();
        service_.heldAfterBusinessStarted.clear();
        // 通道先走:~Channel 把在途调用以 UNAVAILABLE 排进队列,连接断开后服务端摘表。
        registry_.clear();
        channel_.reset();
        RunLoopFor(300ms);
        // loop 外析构 Server(server.h 的前置条件),再转 loop 让它排进的 forceClose / connectDestroyed 跑完。
        server_.reset();
        RunLoopFor(200ms);
    }

    // 在 loop 上转到 done() 为真或超时;返回 done() 的最终结果。不能在 loop 自己的回调里调用。
    bool RunLoopUntil(const std::function<bool()> &done, std::chrono::milliseconds timeout)
    {
        if (done())
        {
            return true;
        }
        const auto deadline = std::chrono::steady_clock::now() + timeout;
        const muduo::net::TimerId check = loop_.runEvery(0.005, [this, &done, deadline]
                                                         {
            if (done() || std::chrono::steady_clock::now() >= deadline)
            {
                loop_.quit();
            } });
        loop_.loop();
        loop_.cancel(check);
        return done();
    }

    void RunLoopFor(std::chrono::milliseconds duration)
    {
        RunLoopUntil([] { return false; }, duration);
    }

    // 在 loop 任务里发起一次带 done 的调用(Send 只在 loop 线程上调用);done 只捕获 observed。
    void SendInLoop(const std::shared_ptr<Observed> &observed, uint64_t playerId, std::chrono::milliseconds deadline,
                    GrpcSentMetadata metadata = {})
    {
        loop_.queueInLoop([this, observed, playerId, deadline, metadata = std::move(metadata)]
        {
            GetPlayerHomeZoneRequest request;
            request.set_player_id(playerId);
            GrpcCallOptions options;
            options.deadline = deadline;
            options.metadata = metadata;
            data_service::SendDataServiceGetPlayerHomeZone(
                registry_, nodeEntity_, request,
                [observed](const GrpcCallOutcome &outcome, const GetPlayerHomeZoneResponse *reply)
                {
                    ++observed->calls;
                    observed->code = outcome.code;
                    observed->message = outcome.message;
                    observed->hadReply = reply != nullptr;
                    observed->homeZoneId = reply != nullptr ? reply->home_zone_id() : 0;
                    observed->sendHadReturnedAtDone = observed->sendReturned;
                },
                options);
            observed->sendReturned = true;
            EXPECT_EQ(observed->calls, 0) << "done 不得在 Send 栈内同步调用";
        });
    }

    // 声明顺序 = 析构逆序:EventLoop 最先构造、最后析构;service_ 活过 server_。
    muduo::net::EventLoop loop_;
    LoopbackDataService service_;
    std::shared_ptr<muduo_grpc::Server> server_;
    entt::registry registry_;
    MuduoGrpcChannelPtr channel_;
    entt::entity nodeEntity_{entt::null};
    uint16_t port_{0};
};

// ASCII 认证头必须逐字节保留;仅历史 SessionMD 额外应用编码,其他二进制头只走传输编码。
TEST_F(GrpcClientLoopbackTest, AuthenticationMetadataPreservesWireContract)
{
    const auto observed = std::make_shared<Observed>();
    SendInLoop(observed, 42, kLiveness,
               {{"x-caller-service", "gate"},
                {"x-caller-signature", "0123456789abcdef"},
                {"x-session-detail-bin", "session"},
                {"x-test-bin", std::string("\0\xff", 2)}});
    ASSERT_TRUE(RunLoopUntil([&] { return observed->calls > 0; }, kLiveness));
    ASSERT_EQ(observed->code, kGrpcStatusOk) << observed->message;
    for (const auto& expected : GrpcMetadata{
             {"x-caller-service", "gate"},
             {"x-caller-signature", "0123456789abcdef"},
             {"x-session-detail-bin", "c2Vzc2lvbg=="},
             {"x-test-bin", std::string("\0\xff", 2)}})
    {
        const auto* actual = FindGrpcMetadata(service_.receivedMetadata, expected.first);
        ASSERT_NE(actual, nullptr) << expected.first;
        EXPECT_EQ(*actual, expected.second) << expected.first;
    }
}

// 同步方法体:done 以 OK 恰好一次,应答字段来自业务;服务端在途计数归零。
TEST_F(GrpcClientLoopbackTest, PerCallDoneSucceedsExactlyOnce)
{
    const auto observed = std::make_shared<Observed>();
    SendInLoop(observed, 42, kLiveness);
    ASSERT_TRUE(RunLoopUntil([&] { return observed->calls > 0; }, kLiveness));
    // 再空转一段:「恰好一次」不是刚好赶上 quit 得出的。
    RunLoopFor(200ms);

    EXPECT_EQ(observed->calls, 1) << "恰好一次";
    EXPECT_EQ(observed->code, kGrpcStatusOk) << observed->message;
    EXPECT_TRUE(observed->hadReply);
    EXPECT_EQ(observed->homeZoneId, 43u) << "应答来自服务端业务";
    EXPECT_TRUE(observed->sendHadReturnedAtDone);
    EXPECT_EQ(service_.dispatched, 1);
    EXPECT_EQ(service_.businessRuns, 1);
    EXPECT_EQ(server_->inFlightCalls(), 0u);
}

// 服务端留着调用不进业务,客户端 deadline 到期:DEADLINE_EXCEEDED 恰好一次;服务端随后把调用收口,
// 迟到的 Proceed 不进业务(§15.7「已派发、等许可」一行)。
TEST_F(GrpcClientLoopbackTest, ServerDeferringTheCallYieldsDeadlineExceededExactlyOnce)
{
    service_.mode = LoopbackDataService::Mode::kHoldBeforeBusiness;
    const auto observed = std::make_shared<Observed>();
    // 500ms 足够回环建连 + 派发到方法体;判据是 DEADLINE_EXCEEDED 本身,不是时长。
    SendInLoop(observed, 7, 500ms);
    ASSERT_TRUE(RunLoopUntil([&] { return observed->calls > 0; }, kLiveness));
    ASSERT_EQ(service_.heldBeforeBusiness.size(), 1u) << "请求必须已经派发到方法体";
    const auto held = service_.heldBeforeBusiness.front();
    // 服务端的截止定时器(grpc-timeout)或客户端到期发出的 RST 会让这次调用失效。
    ASSERT_TRUE(RunLoopUntil([&] { return !held->IsActive(); }, kLiveness));
    held->Proceed();
    RunLoopFor(200ms);

    EXPECT_EQ(observed->calls, 1) << "恰好一次";
    EXPECT_EQ(observed->code, kGrpcStatusDeadlineExceeded) << observed->message;
    EXPECT_FALSE(observed->hadReply);
    EXPECT_EQ(service_.businessRuns, 0) << "调用失效后 Proceed 不得进业务";
    EXPECT_EQ(server_->inFlightCalls(), 0u);
}

// 业务已开始、延后完成的调用遇到服务端关机:排空到期回 UNAVAILABLE "server shutting down"(§15.7「业务已开始」一行),
// onDrained 恰好一次且不在 BeginShutdown 栈内;迟到的 FinishOk 被吞掉,done 不会再来一次。
TEST_F(GrpcClientLoopbackTest, ServerShutdownCompletesStartedCallWithUnavailable)
{
    service_.mode = LoopbackDataService::Mode::kHoldAfterBusinessStarted;
    const auto observed = std::make_shared<Observed>();
    SendInLoop(observed, 9, kLiveness);
    ASSERT_TRUE(RunLoopUntil([&] { return service_.heldAfterBusinessStarted.size() == 1; }, kLiveness));
    EXPECT_EQ(observed->calls, 0);

    const auto drainedCalls = std::make_shared<int>(0);
    muduo_grpc::ServerShutdownOptions shutdownOptions;
    shutdownOptions.drainTimeout = 200ms;
    shutdownOptions.goawayPingTimeout = 100ms;
    shutdownOptions.closeGrace = 100ms;
    server_->BeginShutdown(shutdownOptions, [drainedCalls] { ++*drainedCalls; });
    EXPECT_EQ(*drainedCalls, 0) << "onDrained 永不在 BeginShutdown 栈内执行";
    EXPECT_TRUE(server_->draining());
    EXPECT_TRUE(service_.heldAfterBusinessStarted.front().ServerDraining());

    ASSERT_TRUE(RunLoopUntil([&] { return observed->calls > 0 && *drainedCalls > 0; }, kLiveness));
    // 迟到的完成:记录已被排空到期收口,这里必须被吞掉。
    service_.heldAfterBusinessStarted.front().FinishOk(std::string());
    RunLoopFor(200ms);

    EXPECT_EQ(observed->calls, 1) << "恰好一次";
    EXPECT_EQ(observed->code, kGrpcStatusUnavailable) << observed->message;
    EXPECT_EQ(observed->message, "server shutting down");
    EXPECT_FALSE(observed->hadReply);
    EXPECT_EQ(*drainedCalls, 1) << "onDrained 恰好一次";
    EXPECT_EQ(server_->inFlightCalls(), 0u);
    EXPECT_EQ(server_->connectionCount(), 0u);
}


// 合并保留:节点摘通道时,生成客户端的在途调用与随后本地拒绝都异步失败一次;
// 夹具原本另存一份通道,先放掉它,让实体组件成为唯一所有者,才能验证真实析构路径。
TEST_F(GrpcClientLoopbackTest, ChannelRemovalFailsOutstandingCallAndRejectsLaterCallThroughPerCallDone)
{
    const std::weak_ptr<muduo_grpc::Channel> weakChannel = channel_;
    channel_.reset();
    const auto failedPlayers = std::make_shared<std::vector<uint64_t>>();
    const auto successfulCalls = std::make_shared<int>(0);
    const auto failedCalls = std::make_shared<int>(0);
    const auto onDone = [failedPlayers, weakChannel, successfulCalls, failedCalls](
                            uint64_t playerId, const GrpcCallOutcome &outcome,
                            const GetPlayerHomeZoneResponse *reply)
    {
        if (outcome.ok())
        {
            ++*successfulCalls;
        }
        else
        {
            ++*failedCalls;
            failedPlayers->push_back(playerId);
        }
        EXPECT_EQ(outcome.code, kGrpcStatusUnavailable);
        EXPECT_EQ(reply, nullptr);
        EXPECT_TRUE(weakChannel.expired()) << "失败回调在通道析构之后才执行,不得借用通道";
    };
    GetPlayerHomeZoneRequest request;
    request.set_player_id(43);
    data_service::SendDataServiceGetPlayerHomeZone(
        registry_, nodeEntity_, request,
        [onDone, playerId = request.player_id()](const GrpcCallOutcome &outcome,
                                               const GetPlayerHomeZoneResponse *reply)
        {
            onDone(playerId, outcome, reply);
        });
    ASSERT_FALSE(weakChannel.expired());
    EXPECT_EQ(weakChannel.lock()->outstandingCalls(), 1u);
    registry_.remove<MuduoGrpcChannelPtr>(nodeEntity_);
    EXPECT_TRUE(weakChannel.expired()) << "生成层与调用状态都不得持有通道的强引用";
    EXPECT_FALSE(HasUsableGrpcClient(registry_, nodeEntity_));
    request.set_player_id(44);
    data_service::SendDataServiceGetPlayerHomeZone(
        registry_, nodeEntity_, request,
        [onDone, playerId = request.player_id()](const GrpcCallOutcome &outcome,
                                               const GetPlayerHomeZoneResponse *reply)
        {
            onDone(playerId, outcome, reply);
        });
    EXPECT_TRUE(failedPlayers->empty()) << "摘通道与之后的本地拒绝都不得在当前栈内回调";
    EXPECT_FALSE(GrpcClientLocalCompletionsDrained()) << "摘通道后的调用走本地拒绝,不再发往网络";
    ASSERT_TRUE(RunLoopUntil([&] {
        return failedPlayers->size() == 2 && GrpcClientLocalCompletionsDrained();
    }, kLiveness));
    RunLoopFor(50ms);
    ASSERT_EQ(failedPlayers->size(), 2u) << "每个调用恰好一次";
    EXPECT_EQ((*failedPlayers)[0], 43u) << "~Channel 先排队,在途调用先完成";
    EXPECT_EQ((*failedPlayers)[1], 44u);
    EXPECT_EQ(*failedCalls, 2);
    EXPECT_EQ(*successfulCalls, 0);
}

// 合并保留:带 done 的在途调用在通道析构后以 UNAVAILABLE 完成一次;
// 摘通道后的新调用同样异步拒绝一次,各自计数并区分本次调用的成功与失败。
TEST_F(GrpcClientLoopbackTest, PerCallDoneCompletesUnavailableAfterChannelIsRemoved)
{
    const std::weak_ptr<muduo_grpc::Channel> weakChannel = channel_;
    channel_.reset();
    const auto outstanding = std::make_shared<Observed>();
    const auto rejected = std::make_shared<Observed>();
    const auto successfulCalls = std::make_shared<int>(0);
    const auto failedCalls = std::make_shared<int>(0);
    GetPlayerHomeZoneRequest request;
    request.set_player_id(45);
    data_service::SendDataServiceGetPlayerHomeZone(
        registry_, nodeEntity_, request,
        [outstanding, weakChannel, successfulCalls, failedCalls](const GrpcCallOutcome &outcome,
                                                              const GetPlayerHomeZoneResponse *reply)
        {
            if (outcome.ok())
            {
                ++*successfulCalls;
            }
            else
            {
                ++*failedCalls;
            }
            ++outstanding->calls;
            outstanding->code = outcome.code;
            outstanding->hadReply = reply != nullptr;
            EXPECT_TRUE(weakChannel.expired()) << "done 在通道析构之后才执行,不得捕获通道或实体组件";
        });
    ASSERT_FALSE(weakChannel.expired());
    EXPECT_EQ(weakChannel.lock()->outstandingCalls(), 1u);
    registry_.remove<MuduoGrpcChannelPtr>(nodeEntity_);
    EXPECT_TRUE(weakChannel.expired());
    data_service::SendDataServiceGetPlayerHomeZone(
        registry_, nodeEntity_, request,
        [rejected, successfulCalls, failedCalls](const GrpcCallOutcome &outcome,
                                                const GetPlayerHomeZoneResponse *reply)
        {
            if (outcome.ok())
            {
                ++*successfulCalls;
            }
            else
            {
                ++*failedCalls;
            }
            ++rejected->calls;
            rejected->code = outcome.code;
            rejected->hadReply = reply != nullptr;
        });
    EXPECT_EQ(outstanding->calls, 0) << "摘通道不得在当前栈内执行 done";
    EXPECT_EQ(rejected->calls, 0) << "本地拒绝也不得在 Send 栈内同步回调";
    EXPECT_FALSE(GrpcClientLocalCompletionsDrained()) << "摘通道后的调用走本地拒绝,不再发往网络";
    ASSERT_TRUE(RunLoopUntil([&] {
        return outstanding->calls == 1 && rejected->calls == 1 && GrpcClientLocalCompletionsDrained();
    }, kLiveness));
    RunLoopFor(50ms);
    EXPECT_EQ(outstanding->calls, 1) << "恰好一次";
    EXPECT_EQ(outstanding->code, kGrpcStatusUnavailable);
    EXPECT_FALSE(outstanding->hadReply);
    EXPECT_EQ(rejected->calls, 1);
    EXPECT_EQ(rejected->code, kGrpcStatusUnavailable);
    EXPECT_FALSE(rejected->hadReply);
    EXPECT_EQ(*successfulCalls, 0);
    EXPECT_EQ(*failedCalls, 2);
}

// 合并保留:直接调用生产适配层,而非只给纯 Pick 喂候选。
// 当前通道没有公开 Close API;节点摘除 / 关机阶段 0 的关闭动作是先摘组件,原测试也按此契约验证。
TEST_F(GrpcClientLoopbackTest, SceneManagerSelectionRejectsSnapshotOnlyAndClosingEntities)
{
    const auto type = common::base::SceneManagerNodeService;
    auto &registry = tlsNodeContextManager.GetRegistry(type);
    ASSERT_EQ(registry.view<NodeInfo>().size(), 0u) << "本例不覆盖其他测试遗留的节点";
    const auto snapshotOnly = registry.create();
    registry.emplace<NodeInfo>(snapshotOnly).set_node_type(type);
    EXPECT_EQ(GetSceneManagerEntity(77), static_cast<entt::entity>(entt::null));
    const auto connected = registry.create();
    registry.emplace<NodeInfo>(connected).set_node_type(type);
    registry.emplace<MuduoGrpcChannelPtr>(connected, channel_);
    EXPECT_EQ(GetSceneManagerEntity(77), connected);
    registry.remove<MuduoGrpcChannelPtr>(connected);
    EXPECT_EQ(GetSceneManagerEntity(77), static_cast<entt::entity>(entt::null));
    // 本例创建后不用 ASSERT,三个生产查询无论成功与否都只清理本例两实体。
    registry.destroy(connected);
    registry.destroy(snapshotOnly);
}

} // namespace
