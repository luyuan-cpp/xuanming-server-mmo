// rpc_client_lifecycle_test — 回归测试,对应事故
//   docs/ops/incident-gate-tcpconnection-dtor-assert-2026-09-13.md §7.1 与 §6.3 的更正
//
// 两条不变量,都围绕"出站节点链路(RpcClient / TcpClient)被拆掉的那一刻":
//
//  1. 握手重试定时器不得延长连接寿命。TryRegisterNodeSession 挂的 0.5s 重试闭包若按值捕获
//     TcpConnectionPtr / RpcClientPtr(旧代码),就是在 muduo Timer 里多存了一份强引用,与
//     tlsRpc.conn 同形:节点在窗口内被摘除时 ~TcpClient 看到 use_count()==2 → 跳过 forceClose
//     → TcpConnection.cc:71 assert。判据:调用前后 conn.use_count() 不变。
//
//  2. ~RpcClient 之后,连接不得再回调进已释放的 RpcClient / GameChannel。muduo 的
//     TcpClient::newConnection 会把 connectionCallback / messageCallback 拷到 TcpConnection 上;
//     ~TcpClient 只换 closeCallback、forceClose 又一律 queueInLoop,所以 handleClose →
//     connectionCallback_ 必然发生在 ~RpcClient 返回之后。旧代码绑的是裸 this / 裸 GameChannel*
//     —— RpcClient::onConnection 的 DOWN 分支写已释放的堆,再 trigger OnConnected2TcpServerEvent。
//     现行修法:回调在 RpcClient::connect() 里以 weak_ptr<RpcClient> / weak_ptr<GameChannel> 绑定,
//     对象没了 lock() 失败即返回,没有析构函数、也不需要。
//     判据:销毁 RpcClient 并把 loop 转空后,loop 线程的 tlsEcs.dispatcher 上不得再收到该连接的
//     DOWN 事件;同时对端(server 侧)必须真的看到 DOWN —— 证明 forceClose 确实执行了,拆连路径上
//     没有人把 use_count 抬高而重蹈覆辙。
//
// 修复前跑第 2 条是未定义行为(onConnection 在已释放的对象上执行):Debug CRT 下多半表现为
// 计数 +1 的干净失败,也可能直接崩;两者都算红。
//
// 与 rpc_controller_test.cpp 同一工程、同一个 main;这里的回环夹具多了一个真实的 RpcClient。

#include <gtest/gtest.h>

#ifdef _WIN32
#include <winsock2.h>
#include <ws2tcpip.h>
#pragma comment(lib, "ws2_32.lib")
#endif

#include <atomic>
#include <chrono>
#include <cstdint>
#include <future>
#include <memory>
#include <thread>
#include <utility>

#include "muduo/base/CountDownLatch.h"
#include "muduo/net/EventLoop.h"
#include "muduo/net/EventLoopThread.h"
#include "muduo/net/InetAddress.h"
#include "muduo/net/TcpConnection.h"
#include "muduo/net/TcpServer.h"

#include "network/rpc_client.h"                              // RpcClient / RpcClientPtr
#include "network/rpc_connection_event.h"                    // OnConnected2TcpServerEvent
#include "network/rpc_session.h"                             // RpcSession
#include "node/system/node/node_util.h"                      // NodeUtils::RemoveRpcSessionsBoundTo
#include "node/system/registration/registration_manager.h"  // NodeHandshakeManager
#include "thread_context/ecs_context.h"                      // tlsEcs(thread_local)
#include "thread_context/node_context_manager.h"             // tlsNodeContextManager(thread_local)
#include "proto/common/base/common.pb.h"                     // NodeInfo
#include "proto/common/base/node.pb.h"                       // common::base::SceneNodeService

using namespace std::chrono_literals;

// 链接桩。本文件调用 NodeHandshakeManager::TryRegisterNodeSession,于是把 core.lib 里的
// registration_manager.obj 链进来;它的其他方法引用全局 gNode,又把 node.obj 拉进来;而
// Node::RegisterHandlers 引用的下面四个注册入口是**各节点可执行工程的生成代码**才定义的
// (gate / scene / battle 各一份),任何 lib 里都没有 —— 首次链接即 LNK2019 ×4。
// 本测试永远不会构造 Node,给空实现即可。若日后再报同类未解析符号,按同样方式补。
void InitReply() {}
void InitPlayerService() {}
void InitPlayerServiceReplied() {}
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

// 记录 loop 线程 tlsEcs.dispatcher 上的 OnConnected2TcpServerEvent。
// 只在 loop 线程上 connect / disconnect(dispatcher 是 thread_local 的)。
struct EventCounter
{
    std::atomic<int> up{ 0 };
    std::atomic<int> down{ 0 };
    std::promise<void> firstUp;
    std::atomic<bool> upSignalled{ false };

    void OnEvent(const OnConnected2TcpServerEvent& e)
    {
        if (e.conn_ && e.conn_->connected())
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
        loop_ = loopThread_.startLoop();
        const uint16_t port = FindFreeLoopbackPort();
        ASSERT_NE(port, 0);
        serverAddr_ = muduo::net::InetAddress("127.0.0.1", port);

        RunInLoopAndWait([this, port] {
            server_ = std::make_unique<muduo::net::TcpServer>(
                loop_, muduo::net::InetAddress(port, /*loopbackOnly=*/true), "rpc_client_lifecycle_server");
            // server 侧连接只留 weak_ptr(测试 4 要用它构造 RpcSession),不延长它的寿命。
            server_->setConnectionCallback([this](const muduo::net::TcpConnectionPtr& conn) {
                if (conn->connected()) { serverConnWeak_ = conn; return; }
                if (!downSignalled_.exchange(true)) serverDown_.set_value();
            });
            server_->start();
            tlsEcs.dispatcher.sink<OnConnected2TcpServerEvent>().connect<&EventCounter::OnEvent>(events_);
        });
    }

    void TearDown() override
    {
        RunInLoopAndWait([this] {
            tlsEcs.dispatcher.sink<OnConnected2TcpServerEvent>().disconnect<&EventCounter::OnEvent>(events_);
            server_.reset();
        });
    }

    template <typename F>
    void RunInLoopAndWait(F&& fn)
    {
        muduo::CountDownLatch done(1);
        loop_->runInLoop([&] { fn(); done.countDown(); });
        done.wait();
    }

    // 把 loop 多转几圈,让 forceCloseInLoop / handleClose / connectDestroyed 这些排队的 functor 跑完。
    void PumpLoop(int rounds)
    {
        for (int i = 0; i < rounds; ++i) RunInLoopAndWait([] {});
    }

    // 在 loop 线程上建一个真实的 RpcClient 并等它报 UP。
    RpcClientPtr ConnectClient()
    {
        auto upFuture = events_.firstUp.get_future();
        RpcClientPtr client;
        RunInLoopAndWait([&] {
            client = std::make_shared<RpcClient>(loop_, serverAddr_);
            client->connect();
        });
        EXPECT_EQ(upFuture.wait_for(5s), std::future_status::ready) << "RpcClient never reported UP";
        return client;
    }

    // 销毁 RpcClient,把 loop 转空,并要求对端确实看到 DOWN。
    // 返回 (销毁前, 转空后) 的 DOWN 事件计数。
    std::pair<int, int> DestroyClientAndDrain(RpcClientPtr& client)
    {
        auto downFuture = serverDown_.get_future();
        int downBefore = 0;
        RunInLoopAndWait([&] {
            downBefore = events_.down.load();
            client.reset();   // ~RpcClient → ~TcpClient → (unique 时)forceClose 入队
        });
        PumpLoop(3);
        EXPECT_EQ(downFuture.wait_for(5s), std::future_status::ready)
            << "对端没看到 DOWN:forceClose 没有执行 —— 析构路径上又有人把 use_count 抬高了";
        return { downBefore, events_.down.load() };
    }

    muduo::net::EventLoopThread loopThread_;
    muduo::net::EventLoop* loop_ = nullptr;
    muduo::net::InetAddress serverAddr_;
    std::unique_ptr<muduo::net::TcpServer> server_;
    EventCounter events_;
    std::weak_ptr<muduo::net::TcpConnection> serverConnWeak_;
    std::promise<void> serverDown_;
    std::atomic<bool> downSignalled_{ false };
};

} // namespace

// ---------------------------------------------------------------------------
// 1. 握手重试定时器(registration_manager.cpp TryRegisterNodeSession)
// ---------------------------------------------------------------------------

TEST_F(RpcClientLoopback, HandshakeRetryTimerDoesNotRetainTheConnection)
{
    RpcClientPtr client = ConnectClient();
    ASSERT_TRUE(client && client->connected());

    long before = -1, after = -1, afterDestroy = -1;
    RunInLoopAndWait([&] {
        // tlsNodeContextManager 是 thread_local:实体必须建在 loop 线程的那份注册表里,
        // TryRegisterNodeSession 也在这个线程上按连接指针匹配到它。
        entt::registry& registry = tlsNodeContextManager.GetRegistry(kSceneNodeType);
        const entt::entity node = registry.create();
        registry.emplace<RpcClientPtr>(node, client);
        registry.emplace<NodeInfo>(node);   // 内容无关:匹配只看 GetConnection().get()

        const muduo::net::TcpConnectionPtr conn = client->GetConnection();
        if (!conn) { ADD_FAILURE() << "client has no connection"; return; }

        before = conn.use_count();
        NodeHandshakeManager{}.TryRegisterNodeSession(kSceneNodeType, conn);
        after = conn.use_count();

        // 立刻销毁实体:~TimerTaskComp → Cancel → cancelInLoop 同步 delete 定时器与闭包。
        // 0.5s 后不会再有回调去碰 GetNodeInfo() / gNode(单测进程里没有 Node)。
        registry.destroy(node);
        afterDestroy = conn.use_count();
    });

    EXPECT_EQ(before, after)
        << "TryRegisterNodeSession 之后连接多出 " << (after - before)
        << " 个强引用 —— 重试闭包按值捕获了 TcpConnectionPtr(旧写法),与 tlsRpc.conn 同形";
    EXPECT_EQ(before, afterDestroy) << "实体销毁后引用计数没有回到基线";

    const auto [downBefore, downAfter] = DestroyClientAndDrain(client);
    (void)downBefore; (void)downAfter;   // 这里只要求对端看到 DOWN(在 DestroyClientAndDrain 里断言)
}

// ---------------------------------------------------------------------------
// 2. ~RpcClient 之后不得再回调进已释放对象(rpc_client.h:connect() 里的 weak_ptr 绑定)
// ---------------------------------------------------------------------------

TEST_F(RpcClientLoopback, DestroyingRpcClientDetachesCallbacksBeforeCloseRuns)
{
    RpcClientPtr client = ConnectClient();
    ASSERT_TRUE(client && client->connected());
    ASSERT_EQ(events_.up.load(), 1);

    const auto [downBefore, downAfter] = DestroyClientAndDrain(client);
    EXPECT_EQ(downBefore, downAfter)
        << "~RpcClient 之后连接仍回调进已释放的 RpcClient::onConnection(use-after-free):"
           " DOWN 事件多了 " << (downAfter - downBefore) << " 个";
}

// ---------------------------------------------------------------------------
// 3. 同 uuid 重注册路径(node_connector.cpp ConnectToTcpNode):旧 RpcClient 的最后一根引用
//    由 registry.destroy(existingEntity) 释放。这里曾把旧 client push 进从不排空的
//    Node::disconnectedClientList 来"延迟关闭"(为绕开本事故加的兜底),现已移除。本用例锁定
//    移除后的行为:实体销毁 ⇒ RpcClient 立即析构 ⇒ 无 UAF 回调 ⇒ 对端看到 DOWN。
//    ConnectToTcpNode 本身依赖 gNode,单测里不构造 Node,因此只复刻它对实体做的事。
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
        downBefore = events_.down.load();
        registry.destroy(node);         // == node_connector.cpp 的 targetRegistry.destroy(existingEntity)
    });
    PumpLoop(3);

    EXPECT_EQ(downFuture.wait_for(5s), std::future_status::ready)
        << "对端没看到 DOWN:实体销毁没有让旧连接 forceClose(是否又有人把 RpcClient / conn 存起来了?)";
    EXPECT_EQ(downBefore, events_.down.load())
        << "实体销毁后连接仍回调进已释放的 RpcClient(use-after-free)";
}

// ---------------------------------------------------------------------------
// 4. 入站链路 DOWN 时摘 RpcSession(rpc_server.cc DOWN 分支 → NodeUtils::RemoveRpcSessionsBoundTo)
//    RpcSession::connection 已是 weak_ptr,不摘不会钉住连接;摘是为了让路由方立刻看到"不可达",
//    而不是留一个 lock() 永远失败的空壳等到 etcd 键过期(≤180s)。
//    直接测抽出来的函数:按连接指针精确匹配、跨所有节点类型注册表、不误伤别的连接。
//    RpcSession 的构造函数从 conn 的 context 里 any_cast GameChannelPtr(生产由 RpcServer::onConnection
//    设置),这里手工挂同样的 context;末尾把 context 清掉,断开 conn ↔ GameChannel 的引用环。
// ---------------------------------------------------------------------------

TEST_F(RpcClientLoopback, RemoveRpcSessionsBoundToDetachesOnlyThatConnection)
{
    RpcClientPtr client = ConnectClient();
    ASSERT_TRUE(client && client->connected());

    std::size_t removed = 0;
    bool aGone = false, bStays = false, cGone = false;
    RunInLoopAndWait([&] {
        const muduo::net::TcpConnectionPtr serverConn = serverConnWeak_.lock();
        const muduo::net::TcpConnectionPtr clientConn = client->GetConnection();
        if (!serverConn || !clientConn) { ADD_FAILURE() << "missing connections"; return; }
        serverConn->setContext(std::make_shared<GameChannel>(serverConn));
        clientConn->setContext(std::make_shared<GameChannel>(clientConn));

        entt::registry& sceneReg = tlsNodeContextManager.GetRegistry(kSceneNodeType);
        entt::registry& gateReg  = tlsNodeContextManager.GetRegistry(static_cast<uint32_t>(common::base::GateNodeService));
        const entt::entity a = sceneReg.create();   // 绑 server 侧连接        → 应被摘
        const entt::entity b = sceneReg.create();   // 绑另一条连接            → 应保留
        const entt::entity c = gateReg.create();    // 另一个注册表、同一连接  → 应被摘
        sceneReg.emplace<RpcSession>(a, RpcSession{ serverConn, "uuid-a" });
        sceneReg.emplace<RpcSession>(b, RpcSession{ clientConn, "uuid-b" });
        gateReg.emplace<RpcSession>(c, RpcSession{ serverConn, "uuid-c" });

        removed = NodeUtils::RemoveRpcSessionsBoundTo(serverConn);
        aGone  = sceneReg.try_get<RpcSession>(a) == nullptr;
        bStays = sceneReg.try_get<RpcSession>(b) != nullptr;
        cGone  = gateReg.try_get<RpcSession>(c) == nullptr;

        sceneReg.destroy(a); sceneReg.destroy(b); gateReg.destroy(c);
        serverConn->setContext(GameChannelPtr());
        clientConn->setContext(GameChannelPtr());
    });
    EXPECT_EQ(removed, 2u);
    EXPECT_TRUE(aGone)  << "绑在断开连接上的 RpcSession 没被摘";
    EXPECT_TRUE(bStays) << "误伤了绑在别的连接上的 RpcSession";
    EXPECT_TRUE(cGone)  << "另一个节点类型注册表里的同连接 RpcSession 没被摘";

    // 上面的临时引用都已释放,常规拆连仍须干净:对端看到 DOWN、无 UAF 回调。
    const auto [downBefore, downAfter] = DestroyClientAndDrain(client);
    EXPECT_EQ(downBefore, downAfter);
}

// ---------------------------------------------------------------------------
// 5. GameChannel 不得钉住已断开的出站连接(game_channel.h connection_ 改 weak_ptr)
//    旧写法:RpcClient 的 GameChannel 强持当前连接,DOWN 不清,要等重连成功被新连接覆盖才放 ——
//    对端没了、重连不成功,死连接和它的 fd 就一直被钉着。这里直接销毁 server 让重连必失败:
//    client 侧收到 DOWN 且 loop 转空后,那条连接对象必须已经析构(weak_ptr expired)。
// ---------------------------------------------------------------------------

TEST_F(RpcClientLoopback, ClientChannelDoesNotPinDeadConnection)
{
    RpcClientPtr client = ConnectClient();
    ASSERT_TRUE(client && client->connected());

    std::weak_ptr<muduo::net::TcpConnection> clientConnWeak;
    RunInLoopAndWait([&] {
        clientConnWeak = client->GetConnection();
        server_.reset();   // ~TcpServer → 对端所有连接 connectDestroyed → client 收到 DOWN;之后重连必失败
    });
    const auto deadline = std::chrono::steady_clock::now() + 5s;
    while (events_.down.load() == 0 && std::chrono::steady_clock::now() < deadline)
    {
        std::this_thread::sleep_for(10ms);
    }
    ASSERT_GE(events_.down.load(), 1) << "client 没有收到 DOWN";
    PumpLoop(3);

    EXPECT_TRUE(clientConnWeak.expired())
        << "连接已 DOWN 且 loop 已转空,client 侧连接对象仍活着 —— 有人(GameChannel::connection_?)还攥着强引用";

    RunInLoopAndWait([&] { client.reset(); });   // 此时 client_.connection() 已空,~TcpClient 走 connector 分支
    PumpLoop(2);
}
