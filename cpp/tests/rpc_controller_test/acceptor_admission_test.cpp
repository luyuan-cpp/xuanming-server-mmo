// muduo Acceptor 的准入回调与拒绝时的 RST(本仓对 muduo 的本地改动,docs/design/gate-connection-admission-control.md P1-1、P1-2)。
//
// 真实回环连接:本进程里起一个 TcpServer,用裸 socket 当客户端(阻塞 connect:监听端一 listen,内核就替它完成三次握手,
// 不需要服务端的 loop 在转)。被锁定的契约:
//   * 准入回调在 accept 之后、连接回调之前执行,每条连接恰好问一次,拿到的是对端地址;
//   * 回调返回 false:服务端不建 TcpConnection(连接回调一次都不来),对端读到的是连接被重置(RST)而不是 EOF(FIN);
//   * 回调返回 true、或者根本没设回调:照常建连,关闭照常走 FIN;
//   * 判定逐条生效:同一个监听上先拒后放,互不影响。
// 判据只用事件与计数,等待一律以 5s 作活性上限(AGENTS §11.4)。「对端读到 RST」在 Linux 是 ECONNRESET,
// 在 Windows 是 WSAECONNRESET;读到 0 字节说明服务端走的是 FIN,就是本用例要防的回退。
// Windows 与 Linux 用的是两份 muduo 源码,本工程在 Windows 上编的是 cpp/libs/engine/muduo_windows 那份;
// 两份的一致性由 tools/scripts/tests/muduo_acceptor_copies_contract.tests.ps1 守住。
//
// 与 rpc_controller_test.cpp 同一工程、同一个 main。

#include <gtest/gtest.h>

#ifdef _WIN32
#include <winsock2.h>
#include <ws2tcpip.h>
#pragma comment(lib, "ws2_32.lib")
#else
#include <arpa/inet.h>
#include <cerrno>
#include <netinet/in.h>
#include <sys/socket.h>
#include <sys/time.h>
#include <unistd.h>
#endif

#include <chrono>
#include <cstdint>
#include <functional>
#include <memory>
#include <vector>

#include "muduo/net/EventLoop.h"
#include "muduo/net/InetAddress.h"
#include "muduo/net/TcpConnection.h"
#include "muduo/net/TcpServer.h"
#include "muduo/net/TimerId.h"

using namespace std::chrono_literals;

namespace
{

constexpr auto kLiveness = 5s; // 只作活性上限,不作判据

#ifdef _WIN32
using RawSocket = SOCKET;
constexpr RawSocket kNoSocket = INVALID_SOCKET;
void CloseRawSocket(RawSocket s) { ::closesocket(s); }
#else
using RawSocket = int;
constexpr RawSocket kNoSocket = -1;
void CloseRawSocket(RawSocket s) { ::close(s); }
#endif

uint16_t FindFreeLoopbackPort()
{
    const RawSocket s = ::socket(AF_INET, SOCK_STREAM, IPPROTO_TCP);
    if (s == kNoSocket)
    {
        return 0;
    }
#ifdef _WIN32
    int len = sizeof(sockaddr_in);
#else
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
    CloseRawSocket(s);
    return port;
}

// 阻塞连到本机 port;失败返回 kNoSocket。读超时设成活性上限:对端什么都不发时 recv 不会永远挂住用例。
RawSocket ConnectLoopback(uint16_t port)
{
    const RawSocket s = ::socket(AF_INET, SOCK_STREAM, IPPROTO_TCP);
    if (s == kNoSocket)
    {
        return kNoSocket;
    }
    sockaddr_in addr{};
    addr.sin_family = AF_INET;
    addr.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    addr.sin_port = htons(port);
    if (::connect(s, reinterpret_cast<sockaddr *>(&addr), sizeof(addr)) != 0)
    {
        CloseRawSocket(s);
        return kNoSocket;
    }
    const auto timeoutMs = std::chrono::duration_cast<std::chrono::milliseconds>(kLiveness).count();
#ifdef _WIN32
    const DWORD timeout = static_cast<DWORD>(timeoutMs);
    ::setsockopt(s, SOL_SOCKET, SO_RCVTIMEO, reinterpret_cast<const char *>(&timeout), static_cast<int>(sizeof(timeout)));
#else
    timeval timeout{};
    timeout.tv_sec = static_cast<decltype(timeout.tv_sec)>(timeoutMs / 1000);
    timeout.tv_usec = static_cast<decltype(timeout.tv_usec)>((timeoutMs % 1000) * 1000);
    ::setsockopt(s, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout));
#endif
    return s;
}

// 客户端这一头本地绑定的端口:服务端准入回调拿到的对端端口应当就是它。
uint16_t LocalPortOf(RawSocket s)
{
#ifdef _WIN32
    int len = sizeof(sockaddr_in);
#else
    socklen_t len = sizeof(sockaddr_in);
#endif
    sockaddr_in addr{};
    if (::getsockname(s, reinterpret_cast<sockaddr *>(&addr), &len) != 0)
    {
        return 0;
    }
    return ntohs(addr.sin_port);
}

enum class PeerClose : uint8_t
{
    kReset,   // 读到连接被重置:服务端发的是 RST
    kFin,     // 读到 EOF:服务端发的是 FIN
    kTimeout, // 活性上限内什么都没读到
    kOther,
};

// 在客户端这一头读一次,看服务端是怎么关的。
PeerClose ReadPeerClose(RawSocket s)
{
    char byte = 0;
#ifdef _WIN32
    const int n = ::recv(s, &byte, 1, 0);
    if (n == 0)
    {
        return PeerClose::kFin;
    }
    if (n > 0)
    {
        return PeerClose::kOther;
    }
    const int error = ::WSAGetLastError();
    if (error == WSAECONNRESET || error == WSAECONNABORTED)
    {
        return PeerClose::kReset;
    }
    return error == WSAETIMEDOUT ? PeerClose::kTimeout : PeerClose::kOther;
#else
    const ssize_t n = ::recv(s, &byte, 1, 0);
    if (n == 0)
    {
        return PeerClose::kFin;
    }
    if (n > 0)
    {
        return PeerClose::kOther;
    }
    if (errno == ECONNRESET)
    {
        return PeerClose::kReset;
    }
    return (errno == EAGAIN || errno == EWOULDBLOCK) ? PeerClose::kTimeout : PeerClose::kOther;
#endif
}

// 回调看到的东西。回调只捕获它的 weak(AGENTS §11.7)。
struct Observed
{
    int asked = 0;            // 准入回调被问了几次
    uint16_t lastPeerPort = 0; // 最近一次被问时的对端端口
    int up = 0;               // 连接回调 UP 的次数 = 建出来的 TcpConnection 数
    int down = 0;             // 连接回调 DOWN 的次数
    bool askedBeforeUp = true; // 每次 UP 之前都先问过准入
};

class AcceptorAdmission : public ::testing::Test
{
protected:
    void SetUp() override
    {
        port_ = FindFreeLoopbackPort();
        ASSERT_NE(port_, 0);
        server_ = std::make_unique<muduo::net::TcpServer>(
            &loop_, muduo::net::InetAddress(port_, /*loopbackOnly=*/true), "acceptor_admission_test");
        server_->setConnectionCallback([weak = std::weak_ptr<Observed>(observed_)](const muduo::net::TcpConnectionPtr &conn)
        {
            const auto observed = weak.lock();
            if (!observed)
            {
                return;
            }
            if (conn->connected())
            {
                ++observed->up;
                observed->askedBeforeUp = observed->askedBeforeUp && observed->asked >= observed->up;
            }
            else
            {
                ++observed->down;
            }
        });
    }

    void TearDown() override
    {
        // 先关客户端,等服务端建出来的连接都读到 EOF、走完 DOWN,再析构 TcpServer:它把裸 this 绑进连接的 close 回调。
        for (const RawSocket s : clients_)
        {
            CloseRawSocket(s);
        }
        clients_.clear();
        EXPECT_TRUE(RunLoopUntil([this] { return observed_->down == observed_->up; }));
        server_.reset();
        (void)RunLoopUntil([] { return true; });
    }

    // 装一个按 verdict() 判定的准入回调,并记下被问的次数与对端端口。
    void InstallAdmission(std::function<bool()> verdict)
    {
        server_->setAdmissionCallback(
            [weak = std::weak_ptr<Observed>(observed_), verdict = std::move(verdict)](const muduo::net::InetAddress &peer)
            {
                if (const auto observed = weak.lock())
                {
                    ++observed->asked;
                    // portNetEndian 在 muduo 的各个版本里都有;主机序的取法 Linux 两个 gitlink 版本名字不同(toPort / port)。
                    observed->lastPeerPort = ntohs(peer.portNetEndian());
                }
                return verdict();
            });
    }

    RawSocket Connect()
    {
        const RawSocket s = ConnectLoopback(port_);
        if (s != kNoSocket)
        {
            clients_.push_back(s);
        }
        return s;
    }

    // 在测试线程上驱动 loop,直到 done() 为真或活性上限到期,返回 done() 是否成立。条件满足后经两跳 queueInLoop 再 quit
    // (同 Node 关机,AGENTS §11.7):此前排进队列的 removeConnectionInLoop / connectDestroyed 按 FIFO 先跑完。
    bool RunLoopUntil(std::function<bool()> done)
    {
        struct DriveState
        {
            std::function<bool()> done;
            bool quitting = false;
        };
        const auto state = std::make_shared<DriveState>();
        state->done = std::move(done);
        muduo::net::EventLoop *const loop = &loop_;
        const muduo::net::TimerId check = loop_.runEvery(0.005, [loop, weak = std::weak_ptr<DriveState>(state)]
        {
            const auto current = weak.lock();
            if (!current || current->quitting || !current->done())
            {
                return;
            }
            current->quitting = true;
            loop->queueInLoop([loop, weak]
            {
                loop->queueInLoop([loop, weak]
                {
                    if (weak.lock())
                    {
                        loop->quit();
                    }
                });
            });
        });
        const double livenessSeconds = std::chrono::duration<double>(kLiveness).count();
        const muduo::net::TimerId liveness = loop_.runAfter(livenessSeconds, [loop] { loop->quit(); });
        loop_.loop();
        loop_.cancel(check);
        loop_.cancel(liveness);
        return state->done();
    }

    // EventLoop 归创建它的线程:夹具在测试线程上构造,下面的驱动也都在测试线程上。
    muduo::net::EventLoop loop_;
    uint16_t port_ = 0;
    const std::shared_ptr<Observed> observed_ = std::make_shared<Observed>();
    std::unique_ptr<muduo::net::TcpServer> server_;
    std::vector<RawSocket> clients_;
};

// 被拒的连接:不建 TcpConnection,对端读到 RST。
TEST_F(AcceptorAdmission, RejectedConnectionIsResetAndNeverBecomesATcpConnection)
{
    InstallAdmission([] { return false; });
    server_->start();

    const RawSocket client = Connect();
    ASSERT_NE(client, kNoSocket);
    ASSERT_TRUE(RunLoopUntil([this] { return observed_->asked == 1; }));

    EXPECT_EQ(observed_->up, 0) << "connection callback must not see a refused connection";
    EXPECT_EQ(observed_->lastPeerPort, LocalPortOf(client)) << "admission callback is given the peer address";
    EXPECT_EQ(ReadPeerClose(client), PeerClose::kReset) << "a refused connection is closed with RST, not FIN";
}

// 放行的连接:照常建连,先问准入、后到连接回调;服务端没有主动关,客户端关闭后服务端照常 DOWN。
TEST_F(AcceptorAdmission, AdmittedConnectionIsEstablishedAfterTheAdmissionCheck)
{
    InstallAdmission([] { return true; });
    server_->start();

    const RawSocket client = Connect();
    ASSERT_NE(client, kNoSocket);
    ASSERT_TRUE(RunLoopUntil([this] { return observed_->up == 1; }));

    EXPECT_EQ(observed_->asked, 1) << "asked exactly once per accepted connection";
    EXPECT_TRUE(observed_->askedBeforeUp);
    EXPECT_EQ(observed_->lastPeerPort, LocalPortOf(client));
    EXPECT_EQ(observed_->down, 0);
}

// 没设准入回调:行为与上游一致,全部放行。
TEST_F(AcceptorAdmission, WithoutACallbackEveryConnectionIsAdmitted)
{
    server_->start();

    ASSERT_NE(Connect(), kNoSocket);
    ASSERT_NE(Connect(), kNoSocket);
    ASSERT_TRUE(RunLoopUntil([this] { return observed_->up == 2; }));
    EXPECT_EQ(observed_->asked, 0);
}

// 判定逐条生效:同一个监听上先拒两条再放一条,被拒的不留任何连接对象,放行的那条不受影响。
TEST_F(AcceptorAdmission, VerdictIsPerConnection)
{
    const auto admit = std::make_shared<bool>(false);
    InstallAdmission([admit] { return *admit; });
    server_->start();

    const RawSocket first = Connect();
    const RawSocket second = Connect();
    ASSERT_NE(first, kNoSocket);
    ASSERT_NE(second, kNoSocket);
    ASSERT_TRUE(RunLoopUntil([this] { return observed_->asked == 2; }));
    EXPECT_EQ(observed_->up, 0);
    EXPECT_EQ(ReadPeerClose(first), PeerClose::kReset);
    EXPECT_EQ(ReadPeerClose(second), PeerClose::kReset);

    *admit = true;
    const RawSocket third = Connect();
    ASSERT_NE(third, kNoSocket);
    ASSERT_TRUE(RunLoopUntil([this] { return observed_->up == 1; }));
    EXPECT_EQ(observed_->asked, 3);
    EXPECT_TRUE(observed_->askedBeforeUp);
}

} // namespace
