// rpc_controller_test — 回归测试,对应事故
//   docs/ops/incident-gate-tcpconnection-dtor-assert-2026-09-13.md
//
// 被锁定的不变量:**一次 GameChannel 派发结束后,不得在任何长生命周期存储里留下
// 这条连接的强引用。** 旧代码在 HandleRpcMessage 里 `tlsRpc.conn = conn;`(thread_local,
// 从不清空),使 muduo TcpClient::~TcpClient 的 `use_count()==1` 判断失真、跳过 forceClose,
// 连接在 kConnected 态被析构(TcpConnection.cc:71 assert;Release 下是 poller 里的悬垂
// Channel*)。现在连接随 per-call RpcController 显式传递,派发返回后引用计数必须回到派发前。
//
// 为什么用真实回环连接而不是 mock:TcpConnection 是具体类,不可替身;且在 Debug 下它必须
// 走完 kConnected → kDisconnected 才能安全析构,所以只能让 TcpServer + TcpClient 在
// EventLoopThread 上真连一次,server 侧连接就是被测对象。

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
#include <map>
#include <memory>
#include <string>
#include <utility>

#include "muduo/base/CountDownLatch.h"
#include "muduo/base/Logging.h"
#include "muduo/net/Buffer.h"
#include "muduo/net/EventLoop.h"
#include "muduo/net/EventLoopThread.h"
#include "muduo/net/InetAddress.h"
#include "muduo/net/TcpClient.h"
#include "muduo/net/TcpServer.h"

#include "network/game_channel.h"
#include "network/rpc_controller.h"
#include "proto/common/base/rpc_message.pb.h"
#include "rpc/service_metadata/rpc_event_registry.h"   // kMaxRpcMethodCount

using namespace std::chrono_literals;

namespace
{

// 向 OS 借一个空闲回环端口。muduo 的 TcpServer 不暴露实际绑定端口,只能这样拿。
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

// 把一条 GameRpcMessage 按线上格式(size | "RPC0" | payload | adler32)编进 Buffer。
// 直接复用生产 codec 的 fillEmptyBuffer,保证喂进去的是 HandleIncomingMessage 的真实入口。
void EncodeRpcMessage(const GameRpcMessage& msg, muduo::net::Buffer* out)
{
    ::RpcCodec encoder([](const TcpConnectionPtr&, const RpcMessagePtr&, muduo::Timestamp) {});
    encoder.fillEmptyBuffer(out, msg);
}

// 真实回环:server 侧连接是被测对象(GameChannel 绑定它),client 侧只负责把它连起来。
class LoopbackFixture : public ::testing::Test
{
protected:
    void SetUp() override
    {
        loop_ = loopThread_.startLoop();
        const uint16_t port = FindFreeLoopbackPort();
        ASSERT_NE(port, 0);

        auto connectedFuture = connected_.get_future();
        RunInLoopAndWait([this, port] {
            server_ = std::make_unique<muduo::net::TcpServer>(
                loop_, muduo::net::InetAddress(port, /*loopbackOnly=*/true), "rpc_controller_test_server");
            server_->setConnectionCallback([this](const TcpConnectionPtr& conn) {
                if (conn->connected())
                {
                    if (!connectedSignalled_.exchange(true)) connected_.set_value(conn);
                }
                else
                {
                    if (!downSignalled_.exchange(true)) serverDown_.set_value();
                }
            });
            server_->start();
            client_ = std::make_unique<muduo::net::TcpClient>(
                loop_, muduo::net::InetAddress("127.0.0.1", port), "rpc_controller_test_client");
            client_->connect();
        });

        ASSERT_EQ(connectedFuture.wait_for(5s), std::future_status::ready) << "loopback connect timed out";
        serverConn_ = connectedFuture.get();
        ASSERT_TRUE(serverConn_ && serverConn_->connected());
    }

    void TearDown() override
    {
        // 顺序很重要,而且本身就是这次事故讲的那件事:
        //  1. 先拆 client(它是自己连接的唯一持有者 → forceClose → 对端 handleClose)
        //  2. 等 server 侧回调报 DOWN(此时 TcpServer 已排队 connectDestroyed)
        //  3. 在 loop 线程销毁 TcpServer(它的析构函数断言 in-loop)
        //  4. 最后放掉我们持有的 serverConn_ —— 此刻它已是 kDisconnected,析构合法
        auto downFuture = serverDown_.get_future();
        RunInLoopAndWait([this] { client_.reset(); });
        if (serverConn_)
        {
            EXPECT_EQ(downFuture.wait_for(5s), std::future_status::ready) << "server side never saw DOWN";
        }
        RunInLoopAndWait([this] {
            server_.reset();
            serverConn_.reset();
        });
        // loopThread_ 析构 = quit + join;上面排队的 connectDestroyed 早已在 DOWN 之后跑完。
    }

    template <typename F>
    void RunInLoopAndWait(F&& fn)
    {
        muduo::CountDownLatch done(1);
        loop_->runInLoop([&] { fn(); done.countDown(); });
        done.wait();
    }

    // 在 loop 线程上把一条消息喂给 GameChannel::HandleIncomingMessage(生产入口),
    // 返回派发前后 serverConn_ 的 use_count。参数全程 const& 传递,派发本身不应改变计数;
    // 计数增加只可能来自"某处把连接存下来了" —— 这正是被修掉的缺陷形态。
    std::pair<long, long> DispatchAndMeasure(const GameRpcMessage& msg, GameChannel& channel)
    {
        std::pair<long, long> counts{};
        RunInLoopAndWait([&] {
            muduo::net::Buffer buf;
            EncodeRpcMessage(msg, &buf);
            counts.first = serverConn_.use_count();
            channel.HandleIncomingMessage(serverConn_, &buf, muduo::Timestamp::now());
            counts.second = serverConn_.use_count();
        });
        return counts;
    }

    muduo::net::EventLoopThread loopThread_;
    muduo::net::EventLoop* loop_ = nullptr;
    std::unique_ptr<muduo::net::TcpServer> server_;
    std::unique_ptr<muduo::net::TcpClient> client_;
    TcpConnectionPtr serverConn_;

    std::promise<TcpConnectionPtr> connected_;
    std::promise<void> serverDown_;
    std::atomic<bool> connectedSignalled_{ false };
    std::atomic<bool> downSignalled_{ false };
};

} // namespace

// ---------------------------------------------------------------------------
// 派发不变量(事故的直接回归)
// ---------------------------------------------------------------------------

// 旧的 `tlsRpc.conn = conn;` 位于 HandleRpcMessage 的 switch 之前,任何消息类型都会触发它。
// 用一个未知的 type 值走 default 分支:只记一条日志,不需要服务表,是最小的红/绿探针。
TEST_F(LoopbackFixture, DispatchLeavesNoStrongRefBehind_UnknownType)
{
    GameChannel channel;
    channel.SetConnection(serverConn_);   // HandleRpcMessage 首行 assert(conn == connection_)

    GameRpcMessage msg;
    msg.set_type(static_cast<GameMessageType>(42));
    msg.set_message_id(0);

    const auto [before, after] = DispatchAndMeasure(msg, channel);
    EXPECT_EQ(before, after)
        << "派发结束后连接多出 " << (after - before) << " 个强引用 —— 有人把它存进了长生命周期存储(旧 tlsRpc.conn 形态)";
}

// REQUEST 走 ProcessMessage(tracing::Set、消息 id 校验)→ 越界 id → SendErrorResponse(INVALID_REQUEST)
// → 真的往连接上发一条 RPC_ERROR 应答 → 返回。覆盖派发层入口与"错误应答发送"这条路径。
//
// 为什么必须用**越界**的 message_id:gRpcMethodRegistry 由节点启动时的 Init*ServiceMetadata 填充,
// 单测进程里它是全空的默认数组,RpcMethodMeta::serviceName / methodName 都是 const char* nullptr;
// 合法 id 会走到 services_->find(meta.serviceName) 与 SendErrorResponse 里的 std::string(nullptr),
// 直接访问违例(与修复无关的测试环境事实)。越界 id 在 IsValidMessageId / RecordRecv / RecordSend /
// LogMessageStatistics 四处都有显式边界检查,是这条路径唯一不碰注册表内容的输入。
TEST_F(LoopbackFixture, DispatchLeavesNoStrongRefBehind_RequestWithInvalidMessageId)
{
    GameChannel channel;
    channel.SetConnection(serverConn_);
    const std::map<std::string, ProtobufService*> noServices;
    channel.SetServiceMap(noServices);

    GameRpcMessage msg;
    msg.set_type(GameMessageType::REQUEST);
    msg.set_message_id(kMaxRpcMethodCount);   // == gRpcMethodRegistry.size(),首个非法 id

    const auto [before, after] = DispatchAndMeasure(msg, channel);
    EXPECT_EQ(before, after);
}

// 自证:上面的测量确实能捕获"thread_local 残留"这一形态 —— 否则前两条通过只是空转。
// 在被测窗口内复刻旧代码的动作(把连接存进 thread_local),计数必须 +1。
TEST_F(LoopbackFixture, HarnessDetectsThreadLocalRetention)
{
    static thread_local TcpConnectionPtr simulatedLegacyStash;   // 模拟旧的 tlsRpc.conn
    long before = 0;
    long after = 0;
    RunInLoopAndWait([&] {
        before = serverConn_.use_count();
        simulatedLegacyStash = serverConn_;   // 旧 HandleRpcMessage:314 干的事
        after = serverConn_.use_count();
    });
    EXPECT_EQ(before + 1, after);
    RunInLoopAndWait([&] { simulatedLegacyStash.reset(); });   // 别把它真的泄漏到 TearDown
}

// ---------------------------------------------------------------------------
// RpcController::From 的契约
// ---------------------------------------------------------------------------

TEST(RpcControllerFrom, ReturnsTheVeryObjectPassedToCallMethod)
{
    ::RpcController ctx{ TcpConnectionPtr{} };
    ::google::protobuf::RpcController* asBase = &ctx;   // 生成代码转发的就是这个基类指针
    EXPECT_EQ(&::RpcController::From(asBase), &ctx);
    EXPECT_FALSE(::RpcController::From(asBase).conn());
}

#if GTEST_HAS_DEATH_TEST

namespace
{
// 一个"别人的" RpcController:类型不对也必须被 From() 当场拒绝,而不是 static_cast 后乱指。
class ForeignController final : public ::google::protobuf::RpcController
{
public:
    void Reset() override {}
    bool Failed() const override { return false; }
    std::string ErrorText() const override { return {}; }
    void StartCancel() override {}
    void SetFailed(const std::string&) override {}
    bool IsCanceled() const override { return false; }
    void NotifyOnCancel(::google::protobuf::Closure*) override {}
};
} // namespace

// From() 用 LOG_FATAL 而不是 assert:Release 下 NDEBUG 抹掉 assert,但"派发点忘了传 ctx"
// 必须在两种构建里都立刻暴露,而不是解引用空指针崩在别处。
TEST(RpcControllerFromDeathTest, NullControllerIsFatal)
{
    EXPECT_DEATH({ (void)::RpcController::From(nullptr); }, "");
}

TEST(RpcControllerFromDeathTest, ForeignControllerTypeIsFatal)
{
    ForeignController foreign;
    EXPECT_DEATH({ (void)::RpcController::From(&foreign); }, "");
}

#endif // GTEST_HAS_DEATH_TEST

int main(int argc, char** argv)
{
    muduo::Logger::setLogLevel(muduo::Logger::WARN);   // 回环握手的 INFO 噪音与断言无关
    testing::InitGoogleTest(&argc, argv);
    return RUN_ALL_TESTS();
}
