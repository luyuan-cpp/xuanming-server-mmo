// rpc_client_lifecycle_test — 回归测试,对应事故
//   docs/ops/incident-gate-tcpconnection-dtor-assert-2026-09-13.md §7.1 与 §6.3 的更正
//
// 两条不变量,都围绕"出站节点链路(RpcClient / TcpClient)被拆掉的那一刻":
//
//  1. 握手失败后的延迟断连不得延长连接寿命。0.5s 闭包若按值捕获
//     TcpConnectionPtr / RpcClientPtr(旧代码),就是在 muduo Timer 里多存了一份强引用,与
//     tlsRpc.conn 同形:节点在窗口内被摘除时 ~TcpClient 看到 use_count()==2 → 跳过 forceClose
//     → TcpConnection.cc:71 assert。判据:调用前后 conn.use_count() 不变。
//
//  2. ~RpcClient 之后,连接不得再回调进已释放的 RpcClient / RpcChannel。muduo 的
//     TcpClient::newConnection 会把 connectionCallback / messageCallback 拷到 TcpConnection 上;
//     ~TcpClient 只换 closeCallback、forceClose 又一律 queueInLoop,所以 handleClose →
//     connectionCallback_ 必然发生在 ~RpcClient 返回之后。旧代码绑的是裸 this / 裸 channel 指针
//     —— RpcClient::onConnection 的 DOWN 分支写已释放的堆,再通知连接观察者。
//     现行修法:回调在 RpcClient::connect() 里只绑 weak_ptr<RpcClient>(消息回调只在取 channel 的那一行
//     lock 它,派发期间由局部强引用保活 channel),对象没了 lock() 失败即返回,没有析构函数、也不需要。
//     出站 RpcChannel 每次连上新建、断开即丢弃(docs/design/muduo-rpc-channel.md §6.1),只存 weak 连接。
//     判据:销毁 RpcClient 并把 loop 转空后,连接观察者不得再收到该连接的
//     DOWN 通知;channel 随 RpcClient 立即析构;同时对端(server 侧)必须真的看到 DOWN —— 证明
//     forceClose 确实执行了,拆连路径上没有人把 use_count 抬高而重蹈覆辙。
//
// 修复前跑第 2 条是未定义行为(onConnection 在已释放的对象上执行):Debug CRT 下多半表现为
// 计数 +1 的干净失败,也可能直接崩;两者都算红。
//
// 与 rpc_controller_test.cpp 同一工程、同一个 main;这里的回环夹具多了一个真实的 RpcClient。
// RpcChannel 本身的调用 / 回包 / 超时 / 断连契约在 rpc_channel_loopback_test.cpp 与 rpc_chain_test.cpp。

#include <gtest/gtest.h>

#ifdef _WIN32
#include <winsock2.h>
#include <ws2tcpip.h>
#pragma comment(lib, "ws2_32.lib")
#endif

#include <atomic>
#include <chrono>
#include <cstdint>
#include <cstdio>
#include <functional>
#include <future>
#include <memory>
#include <optional>
#include <string>
#include <string_view>
#include <thread>
#include <utility>
#include <vector>
#include <google/protobuf/util/json_util.h>
#include "network/rpc_controller.h"
#include "node/system/discovery/service_discovery_manager.h"
#include "proto/common/base/message.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"

#include "muduo/base/CountDownLatch.h"
#include "muduo/base/Logging.h"                              // Logger::setOutput / setLogLevel:截获断开日志的级别(第 9 节)
#include "muduo/net/EventLoop.h"
#include "muduo/net/EventLoopThread.h"
#include "muduo/net/InetAddress.h"
#include "muduo/net/TcpClient.h"
#include "muduo/net/TcpConnection.h"
#include "muduo/net/TcpServer.h"

#include "network/rpc_channel.h"                             // RpcChannel::Create / RpcChannelPtr
#include "network/rpc_client.h"                              // RpcClient / RpcClientPtr
#include "network/rpc_session.h"                             // RpcSession
#include "network/rpc_status.h"                              // RpcStatus / RpcStatusCode
#include "node/system/node/node_util.h"                      // NodeUtils::RemoveRpcSessionsBoundTo
#include "node/system/node/node_connector.h"                 // 同 UUID 的通信重连不能重建节点实体
#include "rpc/service_metadata/gate_service_service_metadata.h"  // GateNodeHandshakeRpc
#include "rpc/service_metadata/scene_service_metadata.h"     // SceneNodeHandshakeRpc
#include "node/system/registration/registration_manager.h"  // NodeHandshakeManager
#include "thread_context/node_context_manager.h"             // tlsNodeContextManager(thread_local)
#include "proto/common/base/common.pb.h"                     // NodeInfo
#include "proto/common/base/node.pb.h"                       // common::base::SceneNodeService

using namespace std::chrono_literals;

// 链接桩(整个测试工程只在这里定义一次)。本文件调用 NodeHandshakeManager,
// 于是把 core.lib 里的 registration_manager.obj 链进来;它的其他方法引用全局 gNode,又把 node.obj 拉进来;
// 而 Node::RegisterHandlers 引用的下面两个注册入口是**各节点可执行工程的生成代码**才定义的
// (gate / scene / battle 各一份),任何 lib 里都没有 —— 不给就 LNK2019 ×2。
// 按 message_id 的全局回包表(InitReply)已随 RpcChannel 改造退场,Node 不再要求它(muduo-rpc-channel.md §9.2)。
// 本文件不构造 Node；观战接口夹具只用不调用 RegisterHandlers 的 basic 构造器，空注册桩仍有效。
void InitPlayerService() {}
void InitServiceHandler() {}

namespace
{

constexpr uint32_t kSceneNodeType = static_cast<uint32_t>(common::base::SceneNodeService);

uint16_t FindFreeLoopbackPort()
{
    const SOCKET s = ::socket(AF_INET, SOCK_STREAM, IPPROTO_TCP);
    EXPECT_NE(s, INVALID_SOCKET);
    sockaddr_in addr{};
    addr.sin_family = AF_INET;
    addr.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    addr.sin_port = 0;
    EXPECT_EQ(::bind(s, reinterpret_cast<sockaddr*>(&addr), sizeof(addr)), 0);
    int len = sizeof(addr);
    EXPECT_EQ(::getsockname(s, reinterpret_cast<sockaddr*>(&addr), &len), 0);
    const uint16_t port = ntohs(addr.sin_port);
    ::closesocket(s);
    return port;
}

// 直接记录 RpcClient 在 loop 线程报告的连接状态，生命周期与夹具相同。
struct EventCounter
{
    std::atomic<int> up{ 0 };
    std::atomic<int> down{ 0 };
    std::promise<void> firstUp;
    std::atomic<bool> upSignalled{ false };

    void OnEvent(const muduo::net::TcpConnectionPtr& conn)
    {
        if (conn && conn->connected())
        {
            ++up;
            if (!upSignalled.exchange(true)) firstUp.set_value();
        }
        else
        {
            ++down;
        }
    }
};

class RpcClientLoopback : public ::testing::Test
{
protected:
    void SetUp() override
    {
        loop_.emplace(*loopThread_.startLoop());
        const uint16_t port = FindFreeLoopbackPort();
        ASSERT_NE(port, 0);
        serverAddr_ = muduo::net::InetAddress("127.0.0.1", port);

        RunInLoopAndWait([this, port] {
            server_ = std::make_unique<muduo::net::TcpServer>(
                &Loop(), muduo::net::InetAddress(port, /*loopbackOnly=*/true), "rpc_client_lifecycle_server");
            // server 侧连接只留 weak_ptr(测试 4 要用它构造 RpcSession),不延长它的寿命。
            server_->setConnectionCallback([this](const muduo::net::TcpConnectionPtr& conn) {
                if (conn->connected()) { serverConnWeak_ = conn; return; }
                if (const auto* channel = boost::any_cast<RpcChannelPtr>(&conn->getContext()); channel && *channel)
                {
                    (*channel)->OnDisconnect();
                }
                conn->setContext(RpcChannelPtr{});
                if (!downSignalled_.exchange(true)) serverDown_.set_value();
            });
            server_->start();
        });
    }

    void TearDown() override
    {
        RunInLoopAndWait([this] {
            server_.reset();
        });
    }

    muduo::net::EventLoop& Loop() { return loop_->get(); }

    template <typename F>
    void RunInLoopAndWait(F&& fn)
    {
        muduo::CountDownLatch done(1);
        Loop().runInLoop([&] { fn(); done.countDown(); });
        done.wait();
    }

    // 把 loop 多转几圈,让 forceCloseInLoop / handleClose / connectDestroyed 这些排队的 functor 跑完。
    void PumpLoop(int rounds)
    {
        for (int i = 0; i < rounds; ++i) RunInLoopAndWait([] {});
    }

    // 在 loop 线程上建一个真实的 RpcClient 并等它报 UP。
    RpcClientPtr ConnectClient(RpcClient::Readiness readiness = RpcClient::Readiness::kOnConnect)
    {
        auto upFuture = events_->firstUp.get_future();
        RpcClientPtr client;
        RunInLoopAndWait([&] {
            client = std::make_shared<RpcClient>(&Loop(), serverAddr_, readiness, rpcMaintenance_);
            client->setConnectionObserver([weakEvents = std::weak_ptr<EventCounter>(events_)](const muduo::net::TcpConnectionPtr& conn) {
                if (const auto events = weakEvents.lock())
                {
                    events->OnEvent(conn);
                }
            });
            client->connect();
        });
        EXPECT_EQ(upFuture.wait_for(5s), std::future_status::ready) << "RpcClient never reported UP";
        return client;
    }

    // 截止只防夹具挂住；通过条件始终是 loop 内观测到的事件或状态。
    template <typename Predicate>
    bool WaitInLoop(Predicate&& predicate)
    {
        const auto deadline = std::chrono::steady_clock::now() + 5s;
        while (std::chrono::steady_clock::now() < deadline)
        {
            bool observed = false;
            RunInLoopAndWait([&] { observed = predicate(); });
            if (observed) return true;
            std::this_thread::sleep_for(1ms);
        }
        return false;
    }

    // 销毁 RpcClient,把 loop 转空,并要求对端确实看到 DOWN。
    // 返回 (销毁前, 转空后) 的 DOWN 事件计数。
    std::pair<int, int> DestroyClientAndDrain(RpcClientPtr& client)
    {
        auto downFuture = serverDown_.get_future();
        int downBefore = 0;
        RunInLoopAndWait([&] {
            downBefore = events_->down.load();
            client.reset();   // ~RpcClient → ~TcpClient → (unique 时)forceClose 入队
        });
        PumpLoop(3);
        EXPECT_EQ(downFuture.wait_for(5s), std::future_status::ready)
            << "对端没看到 DOWN:forceClose 没有执行 —— 析构路径上又有人把 use_count 抬高了";
        return { downBefore, events_->down.load() };
    }

    // loopThread_ 排第一:最后析构,保证其余成员析构时 loop 仍在。
    muduo::net::EventLoopThread loopThread_;
    // 引用而非裸指针成员(裸指针成员门禁覆盖测试工程);SetUp 里绑定。
    std::optional<std::reference_wrapper<muduo::net::EventLoop>> loop_;
    muduo::net::InetAddress serverAddr_;
    std::unique_ptr<muduo::net::TcpServer> server_;
    std::shared_ptr<EventCounter> events_{std::make_shared<EventCounter>()};
    std::weak_ptr<muduo::net::TcpConnection> serverConnWeak_;
    std::promise<void> serverDown_;
    std::atomic<bool> downSignalled_{ false };
    // Node 夹具结束会永久关闭真正的进程维护域；本夹具拓扑使用每例私有域，不重开或重置生产门禁。
    std::shared_ptr<RpcMaintenanceState> rpcMaintenance_{std::make_shared<RpcMaintenanceState>()};
};

} // namespace

// ---------------------------------------------------------------------------
// 1. 握手失败后的延迟断连与连接状态
// ---------------------------------------------------------------------------

// 观察者直接报告连接状态，DOWN 时当前 channel 必须已经释放。
TEST_F(RpcClientLoopback, ConnectionObserverReportsUpAndDownWithoutDispatcher)
{
    struct Observation
    {
        std::atomic<int> up{0};
        std::atomic<int> down{0};
        std::atomic<bool> upSignalled{false};
        std::atomic<bool> downSignalled{false};
        std::atomic<bool> channelEmptyAtDown{false};
        std::promise<void> firstUp;
        std::promise<void> firstDown;
    };
    const auto observation = std::make_shared<Observation>();
    auto upFuture = observation->firstUp.get_future();
    auto downFuture = observation->firstDown.get_future();
    RpcClientPtr client;
    RunInLoopAndWait([&] {
        client = std::make_shared<RpcClient>(&Loop(), serverAddr_, RpcClient::Readiness::kOnConnect, rpcMaintenance_);
        const std::weak_ptr<RpcClient> weakClient = client;
        client->setConnectionObserver([observation, weakClient](const muduo::net::TcpConnectionPtr& conn) {
            if (conn->connected())
            {
                ++observation->up;
                if (!observation->upSignalled.exchange(true)) observation->firstUp.set_value();
                return;
            }
            ++observation->down;
            if (!observation->downSignalled.exchange(true))
            {
                const auto liveClient = weakClient.lock();
                observation->channelEmptyAtDown = liveClient && liveClient->channel() == nullptr;
                observation->firstDown.set_value();
            }
        });
        client->connect();
    });

    const bool reportedUp = upFuture.wait_for(5s) == std::future_status::ready;
    EXPECT_TRUE(reportedUp) << "连接观察者没有报告 UP";
    bool serverConnSeen = false;
    if (reportedUp)
    {
        // 不先 forceClose 再析构服务端:forceClose 把连接置成 kDisconnecting,同一 functor 里的 ~TcpServer 随即
        // 同步 connectDestroyed,它只在 kConnected 时 disableAll,Channel::remove 的 assert(isNoneEvent()) 在 Debug
        // 下中止整个测试进程(写法与 OutboundDisconnectRightAfterConnectLogsWarn 相同)。等服务端 accept 之后直接析构
        // 服务端:~TcpServer 以 kConnected 走 connectDestroyed,连接释放即关 fd,客户端随后看到 DOWN;监听在同一
        // functor 里关掉,客户端立即发起的重连只会被拒,UP==1 由夹具控制,不依赖自动重试的时间窗。
        const auto acceptDeadline = std::chrono::steady_clock::now() + 5s;
        bool gaveUp = false;
        while (!serverConnSeen && !gaveUp)
        {
            gaveUp = std::chrono::steady_clock::now() >= acceptDeadline;
            RunInLoopAndWait([&] {
                serverConnSeen = !serverConnWeak_.expired();   // 不 lock:本 functor 不多持一份服务端连接
                if (serverConnSeen || gaveUp)
                {
                    server_.reset();
                }
            });
            if (!serverConnSeen && !gaveUp)
            {
                std::this_thread::sleep_for(10ms);
            }
        }
        EXPECT_TRUE(serverConnSeen) << "真实服务端连接不存在";
        if (serverConnSeen)
        {
            EXPECT_EQ(downFuture.wait_for(5s), std::future_status::ready)
                << "服务端关闭连接后，观察者没有报告 DOWN";
        }
    }
    // EXPECT 失败也走 loop 内清理；回调只持测试状态与 weak 客户端，不借用本例栈变量。
    RunInLoopAndWait([&] {
        client.reset();
        server_.reset();
    });
    PumpLoop(3);
    EXPECT_EQ(observation->up.load(), 1);
    EXPECT_GE(observation->down.load(), 1);
    EXPECT_TRUE(observation->channelEmptyAtDown.load())
        << "DOWN 观察者必须在 OnDisconnect 和 channel.reset 之后执行";
    EXPECT_EQ(events_->up.load(), 0) << "本例不应经过旧 dispatcher UP 接线";
    EXPECT_EQ(events_->down.load(), 0) << "本例不应经过旧 dispatcher DOWN 接线";
}

TEST_F(RpcClientLoopback, HandshakeFailureDelayDoesNotRetainTheConnection)
{
    RpcClientPtr client = ConnectClient(RpcClient::Readiness::kAfterHandshake);
    ASSERT_TRUE(client && client->connected());

    long before = -1, after = -1;
    long clientBefore = -1, clientAfter = -1;
    RunInLoopAndWait([&] {
        const muduo::net::TcpConnectionPtr conn = client->GetConnection();
        if (!conn) { ADD_FAILURE() << "client has no connection"; return; }
        NodeInfo self;
        self.set_node_type(common::base::GateNodeService);
        self.set_node_uuid("local-gate");
        NodeInfo expected;
        expected.set_node_type(common::base::SceneNodeService);
        expected.set_node_uuid("expected-scene");
        before = conn.use_count();
        clientBefore = client.use_count();
        NodeHandshakeManager::BeginHandshake(client, self, expected, conn);
        EXPECT_EQ(client->channel()->OutstandingCount(), 1u);
        EXPECT_EQ(conn.use_count(), before) << "在途握手不得强持连接";
        EXPECT_EQ(client.use_count(), clientBefore) << "在途握手不得强持 RpcClient";
        // 服务端不应答；用合成的单调时刻驱动超时，失败分支在本次 loop 调用内排入延迟关闭。
        client->channel()->SweepTimeouts(RpcChannel::Clock::now() + 4s);
        after = conn.use_count();
        clientAfter = client.use_count();
        EXPECT_FALSE(client->IsReady());
    });

    EXPECT_EQ(before, after)
        << "握手失败后的延迟关闭闭包强持连接，会破坏 TcpClient 析构时的独占判断";
    EXPECT_EQ(clientBefore, clientAfter) << "延迟关闭不得延长 RpcClient 寿命";

    const auto [downBefore, downAfter] = DestroyClientAndDrain(client);
    (void)downBefore; (void)downAfter;   // 这里只要求对端看到 DOWN(在 DestroyClientAndDrain 里断言)
}

TEST_F(RpcClientLoopback, InboundHandshakeRejectsGrpcOnlyTypesBeforeTouchingAnyNodeRegistry)
{
    auto client = ConnectClient();
    RunInLoopAndWait([&]
    {
        const auto conn = client->GetConnection();
        ASSERT_NE(conn, nullptr);
        RpcController context(conn);
        for (const uint32_t type : {uint32_t(common::base::SceneManagerNodeService),
                                   uint32_t(common::base::DataServiceNodeService),
                                   uint32_t(common::base::EtcdNodeService), uint32_t(0), UINT32_MAX})
        {
            NodeHandshakeRequest request;
            request.mutable_self_node()->set_node_type(type);
            request.mutable_self_node()->set_node_uuid("must-not-register");
            NodeHandshakeResponse response;
            // 测试不构造 Node；应在任何 gNode/registry 访问前明确拒绝，而不是误入注册流程。
            NodeHandshakeManager{}.OnNodeHandshake(context, request, response);
            EXPECT_EQ(response.error_message().id(), kFailedToRegisterTheNode);
            if (auto *registry = tlsNodeContextManager.TryGetRegistry(type))
            {
                for (const auto entity : registry->view<NodeInfo>())
                {
                    EXPECT_NE(registry->get<NodeInfo>(entity).node_uuid(), "must-not-register");
                }
            }
        }
    });
    DestroyClientAndDrain(client);
}

namespace
{

NodeInfo HandshakeNode(uint32_t type, const std::string& uuid)
{
    NodeInfo info;
    info.set_node_type(type);
    info.set_node_uuid(uuid);
    info.set_node_id(7);
    info.set_zone_id(1);
    return info;
}

// 服务端只接管回包；测试显式完成，避免把网络调度先后当成就绪判据。全部由 loop 线程访问。
struct HandshakeServerState
{
    std::vector<NodeInfo> requests;
    std::vector<RpcDoneCallback> replies;
};

template <typename Service>
class HeldHandshakeService final : public Service
{
public:
    explicit HeldHandshakeService(std::shared_ptr<HandshakeServerState> state) : state_(std::move(state)) {}

    void NodeHandshake(::google::protobuf::RpcController*, const ::NodeHandshakeRequest* request,
                       ::NodeHandshakeResponse*, ::google::protobuf::Closure* done) override
    {
        state_->requests.push_back(request->self_node());
        state_->replies.push_back(RpcDoneCallback::Adopt(done));
    }

private:
    std::shared_ptr<HandshakeServerState> state_;
};

class RpcClientHandshakeLoopback : public RpcClientLoopback
{
protected:
    void SetUp() override
    {
        RpcClientLoopback::SetUp();
        RunInLoopAndWait([&] {
            sceneService_ = std::make_shared<HeldHandshakeService<::Scene>>(handshakes_);
            gateService_ = std::make_shared<HeldHandshakeService<::Gate>>(handshakes_);
            RpcServiceMap services{
                {std::string(sceneService_->GetDescriptor()->full_name()), sceneService_.get()},
                {std::string(gateService_->GetDescriptor()->full_name()), gateService_.get()}};
            // 连接由服务端持有，context 持有 channel；回调不持连接。服务对象活到服务端拆除之后。
            server_->setMessageCallback([services = std::move(services), maintenance = rpcMaintenance_](
                const muduo::net::TcpConnectionPtr& conn, muduo::net::Buffer* buffer, muduo::Timestamp time) {
                RpcChannelPtr channel;
                if (const auto* existing = boost::any_cast<RpcChannelPtr>(&conn->getContext()); existing && *existing)
                {
                    channel = *existing;
                }
                else
                {
                    channel = RpcChannel::Create(*conn->getLoop(), conn, services, maintenance);
                    conn->setContext(channel);
                }
                channel->OnMessage(conn, buffer, time);
            });
        });
    }

    void TearDown() override
    {
        RunInLoopAndWait([&] {
            server_.reset();
            handshakes_->replies.clear();
        });
        RpcClientLoopback::TearDown();
    }

    void Begin(const RpcClientPtr& client, const NodeInfo& expected)
    {
        RunInLoopAndWait([&] {
            NodeHandshakeManager::BeginHandshake(client, HandshakeNode(common::base::GateNodeService, "local-gate"),
                                                 expected, client->GetConnection());
        });
    }

    bool WaitForRequest(std::size_t count)
    {
        return WaitInLoop([&] { return handshakes_->requests.size() >= count; });
    }

    void Reply(std::size_t index, const NodeInfo& peer, uint32_t error = kCommon_errorOK)
    {
        RunInLoopAndWait([&] {
            ASSERT_LT(index, handshakes_->replies.size());
            const auto& reply = handshakes_->replies[index];
            auto& response = reply.response<NodeHandshakeResponse>();
            *response.mutable_peer_node() = peer;
            response.mutable_error_message()->set_id(error);
            reply.Run();
        });
    }

    bool WaitForCompletion(const RpcClientPtr& client)
    {
        return WaitInLoop([&] { return !client->channel() || client->channel()->OutstandingCount() == 0; });
    }

    std::shared_ptr<HandshakeServerState> handshakes_{std::make_shared<HandshakeServerState>()};
    std::shared_ptr<HeldHandshakeService<::Scene>> sceneService_;
    std::shared_ptr<HeldHandshakeService<::Gate>> gateService_;
};

class RpcClientHandshakeTarget : public RpcClientHandshakeLoopback, public ::testing::WithParamInterface<uint32_t>
{
};

enum class PeerIdentityField : uint8_t { kType, kUuid, kNodeId, kZone, kCount };

class RpcClientHandshakeMismatch : public RpcClientHandshakeLoopback,
                                   public ::testing::WithParamInterface<PeerIdentityField>
{
};

} // namespace

// 两种目标都走真实的生成方法和服务派发；tag 写反或漏 Gate 项会导致握手实际失败。
TEST_P(RpcClientHandshakeTarget, CurrentPeerHandshakeMakesOnlyItsConnectionReady)
{
    auto client = ConnectClient(RpcClient::Readiness::kAfterHandshake);
    const auto expected = HandshakeNode(GetParam(), "expected-peer");
    RunInLoopAndWait([&] {
        EXPECT_TRUE(client->connected());
        EXPECT_FALSE(client->IsReady()) << "TCP UP 不能提前交付业务";
    });
    Begin(client, expected);
    EXPECT_TRUE(WaitForRequest(1)) << "没有收到目标服务的 NodeHandshake";
    RunInLoopAndWait([&] {
        EXPECT_FALSE(client->IsReady()) << "服务端尚未确认，链路必须保持未就绪";
        ASSERT_EQ(handshakes_->requests.size(), 1u);
        EXPECT_EQ(handshakes_->requests.front().node_uuid(), "local-gate");
    });
    Reply(0, expected);
    EXPECT_TRUE(WaitForCompletion(client));
    RunInLoopAndWait([&] {
        EXPECT_TRUE(client->IsReady());
        EXPECT_TRUE(client->IsReadyOn(client->GetConnection()));
        EXPECT_FALSE(client->IsReadyOn(serverConnWeak_.lock()));
    });
    DestroyClientAndDrain(client);
}

INSTANTIATE_TEST_SUITE_P(SceneAndGate, RpcClientHandshakeTarget,
                        ::testing::Values(uint32_t(common::base::SceneNodeService),
                                          uint32_t(common::base::GateNodeService)));

TEST_P(RpcClientHandshakeMismatch, MismatchedPeerIdentityCannotMakeTheLinkReady)
{
    auto client = ConnectClient(RpcClient::Readiness::kAfterHandshake);
    const auto expected = HandshakeNode(common::base::SceneNodeService, "expected-scene");
    auto wrongPeer = expected;
    switch (GetParam())
    {
    case PeerIdentityField::kType: wrongPeer.set_node_type(common::base::GateNodeService); break;
    case PeerIdentityField::kUuid: wrongPeer.set_node_uuid("another-scene-at-the-same-address"); break;
    case PeerIdentityField::kNodeId: wrongPeer.set_node_id(expected.node_id() + 1); break;
    case PeerIdentityField::kZone: wrongPeer.set_zone_id(expected.zone_id() + 1); break;
    default: FAIL() << "身份字段用例未定义";
    }
    Begin(client, expected);
    EXPECT_TRUE(WaitForRequest(1));
    Reply(0, wrongPeer);
    EXPECT_TRUE(WaitForCompletion(client));
    RunInLoopAndWait([&] { EXPECT_FALSE(client->IsReady()); });
    DestroyClientAndDrain(client);
}

INSTANTIATE_TEST_SUITE_P(EveryIdentityField, RpcClientHandshakeMismatch,
                        ::testing::Values(PeerIdentityField::kType, PeerIdentityField::kUuid,
                                          PeerIdentityField::kNodeId, PeerIdentityField::kZone));

TEST_F(RpcClientHandshakeLoopback, PeerRejectionCannotMakeTheLinkReady)
{
    auto client = ConnectClient(RpcClient::Readiness::kAfterHandshake);
    const auto expected = HandshakeNode(common::base::SceneNodeService, "expected-scene");
    Begin(client, expected);
    EXPECT_TRUE(WaitForRequest(1));
    Reply(0, expected, kFailedToRegisterTheNode);
    EXPECT_TRUE(WaitForCompletion(client));
    RunInLoopAndWait([&] { EXPECT_FALSE(client->IsReady()); });
    DestroyClientAndDrain(client);
}

TEST_F(RpcClientHandshakeLoopback, BusinessCallsBeforeHandshakeFailExactlyOnceWithoutSending)
{
    auto client = ConnectClient(RpcClient::Readiness::kAfterHandshake);
    struct Outcomes
    {
        int runs{0};
        bool hasResponse{false};
        std::vector<RpcStatusCode> codes;
    };
    const auto outcomes = std::make_shared<Outcomes>();
    RunInLoopAndWait([&] {
        ProcessClientPlayerMessageRequest request;
        client->CallMethod(SceneProcessClientPlayerMessageRpc::kMessageId, request,
            [outcomes](const RpcStatus& status, const MessagePtr& response) {
                ++outcomes->runs;
                outcomes->codes.push_back(status.code());
                outcomes->hasResponse |= response != nullptr;
            });
        client->CallMethod<SceneProcessClientPlayerMessageRpc>(request,
            [outcomes](const RpcStatus& status, const std::shared_ptr<ProcessClientPlayerMessageResponse>& response) {
                ++outcomes->runs;
                outcomes->codes.push_back(status.code());
                outcomes->hasResponse |= response != nullptr;
            });
        EXPECT_EQ(outcomes->runs, 0) << "未就绪失败也必须异步交付";
        EXPECT_FALSE(client->SendRequest<SceneProcessClientPlayerMessageRpc>(request));
        EXPECT_FALSE(client->SendRequest(SceneProcessClientPlayerMessageRpc::kMessageId, request));
        EXPECT_EQ(client->channel()->OutstandingCount(), 0u) << "业务不应进入未就绪通道";
        EXPECT_FALSE(rpcMaintenance_->LocalCallbacksJoined()) << "未就绪回调仍须由本客户端维护域跟踪";
    });
    PumpLoop(3);
    RunInLoopAndWait([&] {
        EXPECT_EQ(outcomes->runs, 2);
        ASSERT_EQ(outcomes->codes.size(), 2u);
        EXPECT_EQ(outcomes->codes[0], RpcStatusCode::kNotSent);
        EXPECT_EQ(outcomes->codes[1], RpcStatusCode::kNotSent);
        EXPECT_FALSE(outcomes->hasResponse);
        EXPECT_TRUE(handshakes_->requests.empty());
        EXPECT_TRUE(rpcMaintenance_->LocalCallbacksJoined());
    });
    DestroyClientAndDrain(client);
}

TEST_F(RpcClientHandshakeLoopback, ReconnectNeedsANewHandshakeAndRejectsTheOldConnection)
{
    auto client = ConnectClient(RpcClient::Readiness::kAfterHandshake);
    const auto expected = HandshakeNode(common::base::SceneNodeService, "same-scene-process");
    Begin(client, expected);
    EXPECT_TRUE(WaitForRequest(1));
    Reply(0, expected);
    EXPECT_TRUE(WaitForCompletion(client));

    // 仅为模拟晚到的旧连接句柄，在测试中保留已断开的连接；销毁客户端前必须释放。
    muduo::net::TcpConnectionPtr oldConn;
    RunInLoopAndWait([&] {
        EXPECT_TRUE(client->IsReady());
        oldConn = client->GetConnection();
        ASSERT_NE(oldConn, nullptr);
        oldConn->forceClose();
    });
    EXPECT_TRUE(WaitInLoop([&] { return events_->down.load() >= 1 && events_->up.load() >= 2; }))
        << "服务仍监听时，唯一 TcpClient 应自动重连";
    RunInLoopAndWait([&] {
        EXPECT_TRUE(client->connected());
        EXPECT_NE(client->GetConnection(), oldConn);
        EXPECT_FALSE(client->IsReady()) << "同一节点的新 TCP 连接必须重新握手";
        EXPECT_FALSE(client->MarkReady(oldConn));
        EXPECT_FALSE(client->IsReadyOn(oldConn));
        EXPECT_FALSE(client->IsReady());
        oldConn.reset();
    });
    Begin(client, expected);
    EXPECT_TRUE(WaitForRequest(2));
    Reply(1, expected);
    EXPECT_TRUE(WaitForCompletion(client));
    RunInLoopAndWait([&] { EXPECT_TRUE(client->IsReady()); });
    DestroyClientAndDrain(client);
}

TEST_F(RpcClientHandshakeLoopback, ShutdownAndLateHandshakeReplyCannotRestoreReadiness)
{
    auto client = ConnectClient(RpcClient::Readiness::kAfterHandshake);
    const auto expected = HandshakeNode(common::base::SceneNodeService, "expected-scene");
    Begin(client, expected);
    EXPECT_TRUE(WaitForRequest(1));
    RunInLoopAndWait([&] {
        client->CloseChannel();
        EXPECT_FALSE(client->IsReady());
        EXPECT_FALSE(client->MarkReady(client->GetConnection()));
    });
    Reply(0, expected);
    PumpLoop(3);
    RunInLoopAndWait([&] {
        EXPECT_FALSE(client->IsReady());
        EXPECT_FALSE(client->IsReadyOn(client->GetConnection()));
        EXPECT_FALSE(client->MarkReady(client->GetConnection()));
    });
    DestroyClientAndDrain(client);
}

TEST_F(RpcClientLoopback, PlainRpcClientIsReadyOnConnectAndClosedChannelIsNotReady)
{
    auto client = ConnectClient();
    RunInLoopAndWait([&] {
        EXPECT_TRUE(client->IsReady());
        EXPECT_TRUE(client->IsReadyOn(client->GetConnection()));
        EXPECT_FALSE(client->MarkReady({}));
        client->CloseChannel();
        EXPECT_FALSE(client->IsReady());
        EXPECT_FALSE(client->MarkReady(client->GetConnection()));
    });
    DestroyClientAndDrain(client);
}

TEST(ServiceDiscoveryTypeBoundary, MismatchedJsonTypeIsRejectedBeforeSnapshotOrConnectionSideEffects)
{
    ServiceDiscoveryManager manager;
    NodeInfo node;
    node.set_node_type(common::base::SceneManagerNodeService);
    node.set_node_uuid("wrong-prefix-type");
    std::string json;
    ASSERT_TRUE(google::protobuf::util::MessageToJsonString(node, &json).ok());
    // 无 Node 实例时也能验证真正入口：若未先拒绝，取消 pending removal/连接阶段会访问空 gNode。
    manager.AddServiceNode(json, common::base::DataServiceNodeService);
    EXPECT_TRUE(manager.ServiceNodes().NodesOfType(common::base::DataServiceNodeService).empty());
    EXPECT_TRUE(manager.ServiceNodes().NodesOfType(common::base::SceneManagerNodeService).empty());
}

// ---------------------------------------------------------------------------
// 2. ~RpcClient 之后不得再回调进已释放对象(rpc_client.h:connect() 里的 weak_ptr 绑定)
// ---------------------------------------------------------------------------

TEST_F(RpcClientLoopback, DestroyingRpcClientDetachesCallbacksBeforeCloseRuns)
{
    RpcClientPtr client = ConnectClient();
    ASSERT_TRUE(client && client->connected());
    ASSERT_EQ(events_->up.load(), 1);

    std::weak_ptr<RpcChannel> channelWeak;
    RunInLoopAndWait([&] { channelWeak = client->channel(); });
    ASSERT_FALSE(channelWeak.expired()) << "a connected RpcClient must have a channel for its current connection";

    const auto [downBefore, downAfter] = DestroyClientAndDrain(client);
    EXPECT_EQ(downBefore, downAfter)
        << "~RpcClient 之后连接仍回调进已释放的 RpcClient::onConnection(use-after-free):"
           " DOWN 事件多了 " << (downAfter - downBefore) << " 个";
    EXPECT_TRUE(channelWeak.expired())
        << "the channel outlived its destroyed RpcClient (does a message callback or the timeout tick hold it strongly?)";
}

// ---------------------------------------------------------------------------
// 3. 同 UUID 的连接装配保持节点实体稳定；权威节点移除才销毁实体、释放通信资源。
//    普通 TCP DOWN 不清业务路由。同 UUID 的 ConnectToTcpNode 重复调用必须复用原 client，
//    有效的早返回分支不依赖 gNode，可直接验证。实体销毁用例锁定权威移除后的生命周期：
//    RpcClient 立即析构 ⇒ 无 UAF 回调 ⇒ 对端看到 DOWN。
// ---------------------------------------------------------------------------

TEST_F(RpcClientLoopback, DestroyingNodeEntityReleasesItsRpcClientCleanly)
{
    RpcClientPtr client = ConnectClient();
    ASSERT_TRUE(client && client->connected());

    auto downFuture = serverDown_.get_future();
    int downBefore = 0;
    RunInLoopAndWait([&] {
        entt::registry& registry = tlsNodeContextManager.GetRegistry(kSceneNodeType);
        const entt::entity node = registry.create();
        registry.emplace<RpcClientPtr>(node, client);
        registry.emplace<NodeInfo>(node);
        client.reset();                 // 组件现在是唯一持有者,与生产一致
        downBefore = events_->down.load();
        registry.destroy(node);         // 模拟权威节点移除，不是普通 TCP 重连
    });
    PumpLoop(3);

    EXPECT_EQ(downFuture.wait_for(5s), std::future_status::ready)
        << "对端没看到 DOWN:实体销毁没有让旧连接 forceClose(是否又有人把 RpcClient / conn 存起来了?)";
    EXPECT_EQ(downBefore, events_->down.load())
        << "实体销毁后连接仍回调进已释放的 RpcClient(use-after-free)";
}

TEST_F(RpcClientLoopback, RepeatedTcpNodeConnectionKeepsIdentityClientAndBusinessStateAcrossDisconnect)
{
    auto client = ConnectClient();
    struct BusinessStateSentinel { uint64_t routeVersion; }; // 仅测试使用，探测实体是否被销毁重建
    const auto info = [&] {
        auto node = HandshakeNode(common::base::SceneNodeService, "stable-scene-instance");
        node.mutable_endpoint()->set_ip(serverAddr_.toIp());
        node.mutable_endpoint()->set_port(serverAddr_.port());
        return node;
    }();
    entt::entity node = entt::null;
    RunInLoopAndWait([&] {
        auto& registry = tlsNodeContextManager.GetRegistry(kSceneNodeType);
        node = registry.create();
        registry.emplace<NodeInfo>(node, info);
        registry.emplace<RpcClientPtr>(node, client);
        registry.emplace<BusinessStateSentinel>(node, uint64_t{17});
    });

    const auto checkRepeatedConnection = [&] {
        RunInLoopAndWait([&] {
            auto& registry = tlsNodeContextManager.GetRegistry(kSceneNodeType);
            NodeConnector::ConnectToTcpNode(info);
            NodeConnector::ConnectToTcpNode(info);
            ASSERT_TRUE(registry.valid(node));
            const auto found = NodeUtils::FindNodeEntityByUuid(kSceneNodeType, info.node_uuid());
            ASSERT_TRUE(found.has_value());
            EXPECT_EQ(*found, node);
            const auto* trackedClient = registry.try_get<RpcClientPtr>(node);
            ASSERT_NE(trackedClient, nullptr);
            EXPECT_EQ(*trackedClient, client);
            const auto* businessState = registry.try_get<BusinessStateSentinel>(node);
            ASSERT_NE(businessState, nullptr);
            EXPECT_EQ(businessState->routeVersion, 17u);
        });
    };
    checkRepeatedConnection();
    EXPECT_TRUE(WaitInLoop([&] { return !serverConnWeak_.expired(); }));
    RunInLoopAndWait([&] { server_.reset(); });
    EXPECT_TRUE(WaitInLoop([&] { return events_->down.load() >= 1; }));
    RunInLoopAndWait([&] { EXPECT_FALSE(client->connected()); });
    checkRepeatedConnection();

    RunInLoopAndWait([&] {
        auto& registry = tlsNodeContextManager.GetRegistry(kSceneNodeType);
        if (registry.valid(node)) registry.destroy(node);
        client.reset();
    });
    PumpLoop(3);
}

// ---------------------------------------------------------------------------
// 4. 入站链路 DOWN 时摘 RpcSession(rpc_server.cc DOWN 分支 → NodeUtils::RemoveRpcSessionsBoundTo)
//    RpcSession::connection 已是 weak_ptr,不摘不会钉住连接;摘是为了让路由方立刻看到"不可达",
//    而不是留一个 lock() 永远失败的空壳等到 etcd 键过期(≤180s)。
//    直接测抽出来的函数:按连接指针精确匹配、跨所有节点类型注册表、不误伤别的连接。
//    RpcSession 的构造函数从 conn 的 context 里 any_cast RpcChannelPtr(生产由 RpcServer::onConnection
//    设置:每条入站连接一个 channel,归 context 所有),这里手工挂同样的 context。RpcSession 只存
//    weak_ptr<RpcChannel>:清掉 context(== RpcServer 的 DOWN 分支)后 channel 必须立即析构,
//    组件还挂着也钉不住它。
// ---------------------------------------------------------------------------

TEST_F(RpcClientLoopback, RemoveRpcSessionsBoundToDetachesOnlyThatConnection)
{
    RpcClientPtr client = ConnectClient();
    ASSERT_TRUE(client && client->connected());

    std::size_t removed = 0;
    bool aGone = false, bStays = false, cGone = false;
    bool sessionReachedChannel = false;
    bool channelGoneWithSessionsAttached = false;
    bool sessionReportsUnreachable = false;
    RunInLoopAndWait([&] {
        const muduo::net::TcpConnectionPtr serverConn = serverConnWeak_.lock();
        const muduo::net::TcpConnectionPtr clientConn = client->GetConnection();
        if (!serverConn || !clientConn) { ADD_FAILURE() << "missing connections"; return; }
        std::weak_ptr<RpcChannel> serverChannelWeak;
        {
            const RpcChannelPtr serverChannel = RpcChannel::Create(Loop(), serverConn, RpcServiceMap{}, rpcMaintenance_);
            serverChannelWeak = serverChannel;
            serverConn->setContext(serverChannel);   // context 成为唯一的强持有者
        }

        entt::registry& sceneReg = tlsNodeContextManager.GetRegistry(kSceneNodeType);
        entt::registry& gateReg  = tlsNodeContextManager.GetRegistry(static_cast<uint32_t>(common::base::GateNodeService));
        const entt::entity a = sceneReg.create();   // 绑 server 侧连接        → 应被摘
        const entt::entity b = sceneReg.create();   // 绑另一条连接            → 应保留
        const entt::entity c = gateReg.create();    // 另一个注册表、同一连接  → 应被摘
        sceneReg.emplace<RpcSession>(a, RpcSession{ serverConn, "uuid-a" });
        // 出站侧连接的 context 是空的(RpcClient 自持 channel,不用 context):这个 session 没有 channel,
        // 但连接仍活着 —— 摘不摘只看连接,不看 channel。
        sceneReg.emplace<RpcSession>(b, RpcSession{ clientConn, "uuid-b" });
        gateReg.emplace<RpcSession>(c, RpcSession{ serverConn, "uuid-c" });
        sessionReachedChannel = sceneReg.get<RpcSession>(a).IsConnected();

        serverConn->setContext(RpcChannelPtr());
        channelGoneWithSessionsAttached = serverChannelWeak.expired();
        sessionReportsUnreachable = !sceneReg.get<RpcSession>(a).IsConnected();

        removed = NodeUtils::RemoveRpcSessionsBoundTo(serverConn);
        aGone  = sceneReg.try_get<RpcSession>(a) == nullptr;
        bStays = sceneReg.try_get<RpcSession>(b) != nullptr;
        cGone  = gateReg.try_get<RpcSession>(c) == nullptr;

        sceneReg.destroy(a); sceneReg.destroy(b); gateReg.destroy(c);
    });
    EXPECT_TRUE(sessionReachedChannel) << "RpcSession did not pick up the inbound channel from the connection context";
    EXPECT_TRUE(channelGoneWithSessionsAttached)
        << "the channel survived clearing the context: RpcSession holds it strongly (must hold weak_ptr<RpcChannel> only)";
    EXPECT_TRUE(sessionReportsUnreachable) << "RpcSession::IsConnected() still reports reachable after its channel died";
    EXPECT_EQ(removed, 2u);
    EXPECT_TRUE(aGone)  << "绑在断开连接上的 RpcSession 没被摘";
    EXPECT_TRUE(bStays) << "误伤了绑在别的连接上的 RpcSession";
    EXPECT_TRUE(cGone)  << "另一个节点类型注册表里的同连接 RpcSession 没被摘";

    // 上面的临时引用都已释放,常规拆连仍须干净:对端看到 DOWN、无 UAF 回调。
    const auto [downBefore, downAfter] = DestroyClientAndDrain(client);
    EXPECT_EQ(downBefore, downAfter);
}

// ---------------------------------------------------------------------------
// 5. 出站 channel 不得钉住已断开的连接
//    旧写法:RpcClient 终身复用一个 channel 且强持当前连接,DOWN 不清,要等重连成功被新连接覆盖才放 ——
//    对端没了、重连不成功,死连接和它的 fd 就一直被钉着。现在出站 channel 每次连上新建、只存 weak 连接,
//    DOWN 时在途调用以 kDisconnected 失败后 channel 即被丢弃(rpc_client.h onConnection)。
//    这里直接销毁 server 让重连必失败:client 侧收到 DOWN 且 loop 转空后,RpcClient 不再有 channel,
//    旧 channel 与那条连接对象都必须已经析构(weak_ptr expired)。
// ---------------------------------------------------------------------------

TEST_F(RpcClientLoopback, ClientChannelDoesNotPinDeadConnection)
{
    RpcClientPtr client = ConnectClient();
    ASSERT_TRUE(client && client->connected());

    std::weak_ptr<muduo::net::TcpConnection> clientConnWeak;
    std::weak_ptr<RpcChannel> channelWeak;
    RunInLoopAndWait([&] {
        clientConnWeak = client->GetConnection();
        channelWeak = client->channel();
        server_.reset();   // ~TcpServer → 对端所有连接 connectDestroyed → client 收到 DOWN;之后重连必失败
    });
    ASSERT_FALSE(channelWeak.expired()) << "a connected RpcClient must have a channel for its current connection";
    const auto deadline = std::chrono::steady_clock::now() + 5s;   // 只作活性上限,判据是 DOWN 事件本身
    while (events_->down.load() == 0 && std::chrono::steady_clock::now() < deadline)
    {
        std::this_thread::sleep_for(10ms);
    }
    ASSERT_GE(events_->down.load(), 1) << "client 没有收到 DOWN";
    PumpLoop(3);

    bool stillConnected = true;
    bool stillHasChannel = true;
    RunInLoopAndWait([&] {
        stillConnected = client->connected();
        stillHasChannel = client->channel() != nullptr;
    });
    EXPECT_FALSE(stillConnected) << "RpcClient::connected() is still true after DOWN";
    EXPECT_FALSE(stillHasChannel) << "the outbound channel is per connection and must be dropped on DOWN";
    EXPECT_TRUE(channelWeak.expired()) << "the old connection's channel survived DOWN and a drained loop: someone still holds it strongly";
    EXPECT_TRUE(clientConnWeak.expired())
        << "连接已 DOWN 且 loop 已转空,client 侧连接对象仍活着 —— 有人(RpcChannel::connection_?)还攥着强引用";

    RunInLoopAndWait([&] { client.reset(); });   // 此时 client_.connection() 已空,~TcpClient 走 connector 分支
    PumpLoop(2);
}

// ---------------------------------------------------------------------------
// 6. RpcSession 的 channel 已不在时,CallMethod 照样「恰好一次、永不同步」(rpc_session.h FailNotConnected)
//    这是契约测试,不是空连接缺陷的回归测试:构造时连接非空的这条路径,旧实现同样经 loop 投递 kNotSent,
//    本用例改动前后都应是绿的。空连接缺陷(旧实现把 loop_ 留空、done 被就地丢弃)由下面第 7 节的死亡测试锁定。
//    这里人为清掉连接的 context 来模拟「channel 已不在」,锁定 FailNotConnected 这条路径。它不对应 DOWN 时序:
//    RpcServer 的 DOWN 分支清 context 后在同一回调里同步摘掉本组件,loop 线程上看不到「组件还在、channel 已不在」;
//    生产上能走到 FailNotConnected 的只有「构造时连接 context 里就没有 RpcChannelPtr」(rpc_session.h 构造函数)。
//    无类型与 typed 两个入口,CallMethod 返回时回调 0 次;把 loop 转空后各恰好 1 次 kNotSent、response 为空。
//    判据只看 loop 上的排队顺序(FailNotSent 经 queueInLoop 投递,先于随后排进来的 PumpLoop functor),不依赖墙钟。
// ---------------------------------------------------------------------------

TEST_F(RpcClientLoopback, SessionWithoutChannelFailsCallsExactlyOnceThroughTheLoop)
{
    RpcClientPtr client = ConnectClient();
    ASSERT_TRUE(client && client->connected());

    // 回调只在 loop 线程上写;读也放回 loop 线程(RunInLoopAndWait),不需要原子。
    struct Outcome
    {
        int runs{0};
        RpcStatus status;
        bool hasResponse{false};
    };
    const auto untyped = std::make_shared<Outcome>();
    const auto typed = std::make_shared<Outcome>();

    bool channelGone = false;
    bool sessionReportsUnreachable = false;
    int runsAtReturn = -1;
    RunInLoopAndWait([&] {
        const muduo::net::TcpConnectionPtr serverConn = serverConnWeak_.lock();
        if (!serverConn) { ADD_FAILURE() << "missing server connection"; return; }
        std::weak_ptr<RpcChannel> serverChannelWeak;
        {
            const RpcChannelPtr serverChannel = RpcChannel::Create(Loop(), serverConn, RpcServiceMap{}, rpcMaintenance_);
            serverChannelWeak = serverChannel;
            serverConn->setContext(serverChannel);   // context 成为唯一的强持有者
        }
        const RpcSession session{ serverConn, "uuid-channel-gone" };
        serverConn->setContext(RpcChannelPtr());     // 同 RpcServer DOWN 分支的 setContext:channel 随 context 析构(组件不摘)
        channelGone = serverChannelWeak.expired();
        sessionReportsUnreachable = !session.IsConnected();

        NodeHandshakeRequest request;
        session.CallMethod(GateNodeHandshakeRpc::kMessageId, request,
            [untyped](const RpcStatus& status, const MessagePtr& response) {
                ++untyped->runs;
                untyped->status = status;
                untyped->hasResponse = response != nullptr;
            });
        session.CallMethod<GateNodeHandshakeRpc>(request,
            [typed](const RpcStatus& status, const std::shared_ptr<NodeHandshakeResponse>& response) {
                ++typed->runs;
                typed->status = status;
                typed->hasResponse = response != nullptr;
            });
        runsAtReturn = untyped->runs + typed->runs;
    });
    // 前置条件用 EXPECT 而非 ASSERT:提前返回会跳过下面在 loop 线程上的拆连。
    EXPECT_TRUE(channelGone) << "the channel survived clearing the context: the precondition of this test does not hold";
    EXPECT_TRUE(sessionReportsUnreachable) << "RpcSession::IsConnected() still reports reachable after its channel died";
    EXPECT_EQ(runsAtReturn, 0) << "a session call completed before CallMethod returned (synchronous completion)";

    PumpLoop(3);
    Outcome untypedSeen;
    Outcome typedSeen;
    RunInLoopAndWait([&] {
        untypedSeen = *untyped;
        typedSeen = *typed;
    });
    EXPECT_EQ(untypedSeen.runs, 1) << "untyped CallMethod: done must run exactly once (0 = dropped in place)";
    EXPECT_EQ(untypedSeen.status.code(), RpcStatusCode::kNotSent) << untypedSeen.status.name();
    EXPECT_FALSE(untypedSeen.hasResponse) << "a failed call must not carry a response";
    EXPECT_EQ(typedSeen.runs, 1) << "typed CallMethod: done must run exactly once (0 = dropped in place)";
    EXPECT_EQ(typedSeen.status.code(), RpcStatusCode::kNotSent) << typedSeen.status.name();
    EXPECT_FALSE(typedSeen.hasResponse) << "a failed call must not carry a response";

    const auto [downBefore, downAfter] = DestroyClientAndDrain(client);
    EXPECT_EQ(downBefore, downAfter);
}

// ---------------------------------------------------------------------------
// 7. RpcSession 构造即拒空连接(rpc_session.h LoopOf)—— 空连接缺陷的回归测试
//    旧实现:连接为空时构造函数直接返回、loop_ 留空,之后 CallMethod 没有 loop 可投递 kNotSent,
//    done 被就地丢弃(只打一行 ERROR),违反「done 恰好一次」。旧代码在这里不会死 → 红;
//    现在构造即 LOG_FATAL(muduo 在 ~Logger 里 abort,Release 下同样生效)→ 绿。
//    不带夹具,写法照 rpc_controller_test.cpp 的 RpcControllerFromDeathTest:子进程只跑本用例,
//    不受回环夹具的 EventLoopThread 影响。muduo 日志写 stdout,EXPECT_DEATH 只匹配 stderr,所以正则留空。
// ---------------------------------------------------------------------------

#if GTEST_HAS_DEATH_TEST

TEST(RpcSessionDeathTest, NullConnectionIsFatal)
{
    EXPECT_DEATH(
        {
            const RpcSession session(muduo::net::TcpConnectionPtr{}, "uuid-null");
            (void)session;
        },
        "");
}

#endif // GTEST_HAS_DEATH_TEST

// ---------------------------------------------------------------------------
// 8. muduo Connector 排队的 resetChannel 不得悬空(Connector.cc LOCAL DELTA,AGENTS.md §11.7)
//    连上的那一刻,Connector::handleWrite 先排队 resetChannel,再同步调 TcpClient::newConnection → UP 回调。
//    在这条栈上拆掉 TcpClient:有连接时 ~TcpClient 不给 Connector 续命,上游的 resetChannel 又绑裸 this,
//    Connector 当场带着未释放的 channel_ 析构 —— Debug 下 ~Connector 的 assert(!channel_) 直接中止(红),
//    Release 下 handleEvent 余下部分与排队的 resetChannel 都踩已释放内存。现在 resetChannel 持
//    shared_from_this(),Connector 活到 channel_ 释放。判据:不崩,接住的连接随后能正常关掉、对端看到 DOWN。
//    拆 TcpClient 时连接非独占(newConnection 栈上还有一份),~TcpClient 不会 forceClose,所以本例在测试里
//    临时强持 TcpConnectionPtr 接住连接,最后自己 forceClose、等 connectDestroyed 跑完再释放;
//    生产代码不得这样持有连接(§11.7)。
// ---------------------------------------------------------------------------

TEST_F(RpcClientLoopback, DestroyingTcpClientOnItsConnectStackKeepsConnectorAliveUntilChannelReset)
{
    // 只在 loop 线程上读写(回调与 RunInLoopAndWait 内),不需要原子。
    struct State
    {
        std::unique_ptr<muduo::net::TcpClient> client;
        muduo::net::TcpConnectionPtr heldConn;
        std::promise<void> destroyedOnUp;
        bool signalled{ false };
    };
    const auto state = std::make_shared<State>();
    auto destroyedFuture = state->destroyedOnUp.get_future();
    auto serverDownFuture = serverDown_.get_future();
    RunInLoopAndWait([&] {
        state->client = std::make_unique<muduo::net::TcpClient>(&Loop(), serverAddr_, "connector_lifetime_client");
        // 回调会被拷到连接上、可能在本例之后才被调用:只捕获 weak,不借用本例栈变量。
        state->client->setConnectionCallback(
            [weakState = std::weak_ptr<State>(state)](const muduo::net::TcpConnectionPtr& conn) {
                const auto live = weakState.lock();
                if (!live || !conn->connected() || live->signalled) return;
                live->signalled = true;
                live->heldConn = conn;
                // 仍在 Connector::handleWrite → TcpClient::newConnection 栈上,resetChannel 已排队、尚未执行。
                live->client.reset();
                live->destroyedOnUp.set_value();
            });
        state->client->connect();
    });
    const bool destroyed = destroyedFuture.wait_for(5s) == std::future_status::ready;
    EXPECT_TRUE(destroyed) << "TcpClient never reported UP";
    // 排队的 resetChannel 在 UP 那一轮的 pending functor 里就会执行,这里只确保它已跑完。
    PumpLoop(3);
    RunInLoopAndWait([&] {
        if (state->heldConn) state->heldConn->forceClose();
        state->client.reset();   // 没连上时的兜底拆除;连上时已在 UP 回调里拆掉
    });
    PumpLoop(3);   // forceCloseInLoop → handleClose → ~TcpClient 换上的 removeConnection 排的 connectDestroyed
    // handleClose 不关 fd(留给析构):释放最后一份引用才关 socket,对端随后看到 DOWN。
    RunInLoopAndWait([&] { state->heldConn.reset(); });
    if (destroyed)
    {
        EXPECT_EQ(serverDownFuture.wait_for(5s), std::future_status::ready)
            << "对端没看到 DOWN:接住的连接没有正常关掉";
    }
}

// ---------------------------------------------------------------------------
// 9. 出站链路断开一律 WARN,没有「启动宽限」(rpc_client.h onConnection 的 DOWN 分支)
//    旧实现把连上后 5s 内的断开降为 INFO(kStartupGraceSeconds),依据是旧 win_connect 阻塞 select、无条件返回 0
//    造成的假阳性连接。d27176e04 起 win_connect 非阻塞,Connector::handleWrite 以 SO_ERROR 确认连接完成才回调 UP,
//    依据已不存在;而宽限每条连接重新计时,对端反复「接受后立刻关」的抖动会永远停在 INFO,告警看不到。
//    判据:连上后立即由对端停服关闭(远在旧的 5s 窗口之内),截获到恰好一条 WARN 级的
//    "Disconnected from server at <对端>"。旧代码这里只打 INFO → 0 条 → 红。
//    日志输出函数与级别是进程级全局变量:装、卸都在 loop 线程上做(本例只有 loop 线程打日志),
//    计数只由 loop 线程写,测试线程经 RunInLoopAndWait 读取,不与打日志的线程竞争。
// ---------------------------------------------------------------------------

namespace
{

// 只在 loop 线程上读写(日志从 loop 线程打出,装卸与读取都经 RunInLoopAndWait)。
std::string gDisconnectWarnNeedle;
int gDisconnectWarnLines = 0;

void CountDisconnectWarn(const char* msg, int len)
{
    // muduo 每次调用输出函数恰好交出一整行;级别字段是定宽的 "WARN  "(Logging.cc LogLevelName)。
    const std::string_view line(msg, static_cast<std::size_t>(len));
    if (line.find("WARN  ") != std::string_view::npos && line.find(gDisconnectWarnNeedle) != std::string_view::npos)
    {
        ++gDisconnectWarnLines;
    }
    std::fwrite(msg, 1, static_cast<std::size_t>(len), stdout);   // 照常输出,失败时便于排查
}

// muduo 的默认输出函数在 Logging.cc 的匿名作用域里,不可取址;恢复成等价实现(同 redis_manager_auth_test.cpp)。
void WriteLogToStdout(const char* msg, int len)
{
    std::fwrite(msg, 1, static_cast<std::size_t>(len), stdout);
}

} // namespace

TEST_F(RpcClientLoopback, OutboundDisconnectRightAfterConnectLogsWarn)
{
    muduo::Logger::LogLevel savedLevel = muduo::Logger::WARN;
    RunInLoopAndWait([&] {
        savedLevel = muduo::Logger::logLevel();
        muduo::Logger::setLogLevel(muduo::Logger::WARN);   // 判据是 WARN 行:级别不得高于 WARN
        gDisconnectWarnNeedle = "Disconnected from server at " + serverAddr_.toIpPort();
        gDisconnectWarnLines = 0;
        muduo::Logger::setOutput(&CountDisconnectWarn);
    });

    RpcClientPtr client = ConnectClient();

    // 服务端 accept 与客户端 UP 同在一个 loop 上、先后不定:等服务端连接出现再停服。截止时间只作活性上限。
    // 由停服(~TcpServer)关掉这条连接(同第 5 节),不先 forceClose:forceClose 把连接置为 kDisconnecting 并排队
    // forceCloseInLoop,同一 functor 里的 ~TcpServer 随即在 loop 线程同步执行 connectDestroyed —— 它只在 kConnected
    // 时 disableAll,Channel::remove 的 assert(isNoneEvent()) 在 Debug 下中止;Release 下排队的 forceCloseInLoop
    // 之后经 handleClose 调用绑着裸 this 的 TcpServer::removeConnection(TcpServer.cc FIXME: unsafe),踩已析构的
    // TcpServer。~TcpServer 以 kConnected 走 connectDestroyed,连接随最后一份引用释放而关 fd,客户端随后看到 DOWN。
    // 关连接与停监听在同一个 functor 里:客户端看到 FIN 时监听已关,TcpClient::removeConnection → Connector::restart
    // 立即发起的重连只会被拒(restart 不走 500ms 退避),本例只有一条连接、恰好一次 DOWN。
    const auto acceptDeadline = std::chrono::steady_clock::now() + 5s;
    bool serverConnSeen = false;
    bool gaveUp = false;
    while (!serverConnSeen && !gaveUp)
    {
        gaveUp = std::chrono::steady_clock::now() >= acceptDeadline;
        RunInLoopAndWait([&] {
            serverConnSeen = !serverConnWeak_.expired();   // 不 lock:本 functor 不多持一份服务端连接
            if (serverConnSeen || gaveUp)
            {
                server_.reset();
            }
        });
        if (!serverConnSeen && !gaveUp)
        {
            std::this_thread::sleep_for(10ms);
        }
    }
    EXPECT_TRUE(serverConnSeen) << "真实服务端连接不存在";

    // 观察者在 DOWN 分支打完日志之后才被调用:看到 DOWN 计数时,那条断开日志已经过了输出函数。
    const auto downDeadline = std::chrono::steady_clock::now() + 5s;   // 只作活性上限,判据是 DOWN 事件本身
    while (events_->down.load() == 0 && std::chrono::steady_clock::now() < downDeadline)
    {
        std::this_thread::sleep_for(10ms);
    }
    EXPECT_EQ(events_->down.load(), 1) << "client 应恰好收到一次 DOWN";

    // EXPECT 失败也走 loop 内的恢复与拆除,不把截获函数留给后面的用例。
    int warnLines = -1;
    RunInLoopAndWait([&] {
        warnLines = gDisconnectWarnLines;
        muduo::Logger::setOutput(&WriteLogToStdout);
        muduo::Logger::setLogLevel(savedLevel);
        client.reset();   // 此时 client_.connection() 已空,~TcpClient 走 connector 分支
    });
    PumpLoop(2);

    EXPECT_EQ(warnLines, 1)
        << "连上后立刻断开没有打出 WARN 级的 \"Disconnected from server at\":断开日志又被按连接时长降级了"
           "(旧的 kStartupGraceSeconds 启动宽限会把 5s 内的断开降为 INFO)";
}
