// redis_manager_auth_test —— 回归测试,对应 docs/handoff/global-singletons-p0-findings-20261014.md P0-A
// 「C++ 节点连 zone Redis 不发 AUTH」。
//
// 被测契约见 cpp/libs/engine/thread_context/redis_manager.h 中 Connect 的注释(fail-closed):
//  1. 口令为空:不发 AUTH,连上即对业务可见,行为与改动前相同;
//  2. 口令非空:每条新连接(首次建连与每次重连)上的第一条命令就是 AUTH;只有 AUTH 回 +OK 后连接才对业务可见、
//     重连回调才被调用,所以回调里发出的命令排在 AUTH 之后;
//  3. AUTH 被拒或一个重连间隔内没有应答:连接不对业务可见(GetConnection() 非空但 connected()==false)、
//     不调重连回调、握手连接上不出现 AUTH 以外的命令,并在回调栈外重建握手连接重试;
//     被拒当场、没有应答在截止时各打一条带 metric=zone_redis_auth_failed 的 ERROR;
//  4. 日志里永远不出现口令原文,服务端回显口令时只保留错误码、其余遮掉;
//  5. 握手中 Shutdown:安静释放,不回调、不重连;
//  6. 重连间隔非正数时回落到默认值并打 WARN;
//  7. 握手中服务端断开(AUTH 应答前,或回了拒绝之后):空应答不移交、不调回调,断线回调不打乱截止定时器,
//     到点照常重建握手连接;
//  8. 断线日志带 hiredis 记下的原因(reason=<errstr>,P0-E 第 3 批),同样不含口令。
//
// 同一夹具还覆盖 2026-10 加的存活契约(RedisManagerLiveness.*,见 redis_manager.h Connect 注释的「存活契约」):
//  8. 对端不回任何应答、也不断开(模拟代理所在机器整机消失):PING 截止后摘下连接,在途命令以空应答结束,
//     走既有重连(新连接照常先 AUTH),截止前探测 PING 不叠加,打一条带 metric=zone_redis_liveness_timeout 的 ERROR;
//  9. 健康连接上周期发 PING、从不误摘;应答慢但在截止内也不误摘;
// 10. 握手连接(AUTH 未回)上从不发探测 PING,fail-closed 不变;
// 11. 探测间隔 / 截止非正数时回落到默认值并打 WARN。
//
// 做法:被测的 RedisManager(测试自建,不用 tlsRedis)与假 Redis(muduo TcpServer)跑在同一个 loop 线程上。
// 假 Redis 做最小 RESP 解析,按连接记下收到的命令序列,按用例配置对 AUTH 的应答。
// 日志经 muduo::Logger::setOutput 截获:在 loop 线程启动前装上、join 之后卸下,不与 loop 线程竞争全局变量。
// 测试线程等 loop 线程一律用带截止时间的 future(单调时钟),不做无限期等待。
//
// 与 rpc_controller_test.cpp 同一工程、同一个 main(main 把日志级别设为 WARN,本夹具临时调到 INFO);
// 链接桩(InitPlayerService / InitServiceHandler)已由 rpc_client_lifecycle_test.cpp
// 定义,这里不要重复定义。

#include <gtest/gtest.h>

#ifdef _WIN32
#include <winsock2.h>
#include <ws2tcpip.h>
#pragma comment(lib, "ws2_32.lib")
#else
#include <sys/socket.h>
#include <netinet/in.h>
#include <unistd.h>
#endif

#include <charconv>
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <functional>
#include <future>
#include <map>
#include <memory>
#include <mutex>
#include <optional>
#include <string>
#include <string_view>
#include <system_error>
#include <thread>
#include <utility>
#include <vector>

#include "muduo/base/Logging.h"
#include "muduo/base/Timestamp.h"
#include "muduo/net/Buffer.h"
#include "muduo/net/Callbacks.h"
#include "muduo/net/EventLoop.h"
#include "muduo/net/EventLoopThread.h"
#include "muduo/net/InetAddress.h"
#include "muduo/net/TcpConnection.h"
#include "muduo/net/TcpServer.h"
#include "muduo/contrib/hiredis/Hiredis.h"   // 它包含 <hiredis/hiredis.h>,提供 REDIS_OK / redisReply

#include "thread_context/redis_manager.h"

using namespace std::chrono_literals;

namespace
{

// 测试专用的假口令。断言「日志里没有口令原文」就是在找这个串。
constexpr char kTestPassword[] = "p0a-test-pass-01";

// 注入的重连间隔(同时是 AUTH 握手截止):远小于生产默认的 3s,让「断线 / 被拒 / 超时后重建」的用例不必干等。
constexpr double kReconnectIntervalSec = 0.05;
// 不该发生重建的用例用它:整个用例期间截止定时器都不会到点,结果不依赖回环上的应答快慢。
constexpr double kLongReconnectIntervalSec = 30.0;
// 存活探测:不测探测的用例用长间隔,整个用例期间一次都不探测,连接上的命令序列与加探测之前完全相同。
constexpr double kLongLivenessProbeIntervalSec = 30.0;
constexpr double kLongLivenessTimeoutSec = 90.0;
// 测探测的用例:每 50ms 检查一次,PING 发出 300ms 无应答即判定断线。回环上的应答是亚毫秒级,300ms 留足余量。
constexpr double kLivenessProbeIntervalSec = 0.05;
constexpr double kLivenessTimeoutSec = 0.3;
// 断言「健康连接不被误摘」的观察窗口:超过截止的三倍,窗口内误摘必然能看到。
constexpr std::chrono::milliseconds kLivenessQuietWindow = 1s;
// 等「应该发生」的事(建连、应答、重连)的上限。
constexpr std::chrono::milliseconds kSettleTimeout = 5s;
// 断言「不该发生」的事(重连、新连接)时观察的窗口:约 10 个重连周期。
constexpr std::chrono::milliseconds kQuietWindow = 500ms;
constexpr std::chrono::milliseconds kPollInterval = 5ms;

// ---------------------------------------------------------------------------
// 日志截获。muduo 的输出函数是函数指针,状态只能放在本 TU 的全局里,用互斥锁保护。
// ---------------------------------------------------------------------------

std::mutex gLogMutex;
std::string gLogText;

void CaptureLogOutput(const char* msg, int len)
{
    std::lock_guard<std::mutex> lock(gLogMutex);
    gLogText.append(msg, static_cast<std::size_t>(len));
}

// muduo 的默认输出函数(Logging.cc 里的 defaultOutput)在匿名作用域里,不可取址;恢复成等价实现。
void StdoutLogOutput(const char* msg, int len)
{
    std::fwrite(msg, 1, static_cast<std::size_t>(len), stdout);
}

void ClearCapturedLog()
{
    std::lock_guard<std::mutex> lock(gLogMutex);
    gLogText.clear();
}

std::string CapturedLog()
{
    std::lock_guard<std::mutex> lock(gLogMutex);
    return gLogText;
}

bool Contains(std::string_view text, std::string_view needle)
{
    return text.find(needle) != std::string_view::npos;
}

std::size_t CountOccurrences(std::string_view text, std::string_view needle)
{
    std::size_t count = 0;
    for (std::size_t pos = text.find(needle); pos != std::string_view::npos; pos = text.find(needle, pos + needle.size()))
    {
        ++count;
    }
    return count;
}

// 失败信息里要打印日志或命令时先把测试口令换成 <pw>:测试输出同样不得出现口令原文(AGENTS.md §11.3)。
std::string MaskPassword(std::string text)
{
    constexpr std::string_view kSecret(kTestPassword);
    constexpr std::string_view kMask("<pw>");
    for (std::size_t pos = text.find(kSecret); pos != std::string::npos; pos = text.find(kSecret, pos + kMask.size()))
    {
        text.replace(pos, kSecret.size(), kMask);
    }
    return text;
}

// ---------------------------------------------------------------------------
// 最小 RESP 解析:只认 hiredis 发命令用的多条批量格式 "*N\r\n" + N 个 "$len\r\n<bytes>\r\n"。
// ---------------------------------------------------------------------------

using RespCommand = std::vector<std::string>;

enum class ParseResult
{
    kIncomplete,
    kParsed,
    kMalformed,
};

// 读一行 "<prefix><非负十进制>\r\n";成功时把 pos 推到 \r\n 之后。
ParseResult ReadLengthLine(std::string_view data, std::size_t& pos, char prefix, std::size_t& value)
{
    if (pos >= data.size())
    {
        return ParseResult::kIncomplete;
    }
    if (data[pos] != prefix)
    {
        return ParseResult::kMalformed;
    }
    const std::size_t crlf = data.find("\r\n", pos + 1);
    if (crlf == std::string_view::npos)
    {
        return ParseResult::kIncomplete;
    }
    const char* first = data.data() + pos + 1;
    const char* last = data.data() + crlf;
    const std::from_chars_result parsed = std::from_chars(first, last, value);
    if (parsed.ec != std::errc() || parsed.ptr != last)
    {
        return ParseResult::kMalformed;
    }
    pos = crlf + 2;
    return ParseResult::kParsed;
}

// 从 data 开头解析一条命令;成功时 consumed 是这条命令占用的字节数。
ParseResult ParseCommand(std::string_view data, std::size_t& consumed, RespCommand& out)
{
    std::size_t pos = 0;
    std::size_t argc = 0;
    if (const ParseResult r = ReadLengthLine(data, pos, '*', argc); r != ParseResult::kParsed)
    {
        return r;
    }
    RespCommand args;
    for (std::size_t i = 0; i < argc; ++i)
    {
        std::size_t len = 0;
        if (const ParseResult r = ReadLengthLine(data, pos, '$', len); r != ParseResult::kParsed)
        {
            return r;
        }
        const std::size_t remaining = data.size() - pos;
        if (len > remaining || remaining - len < 2)
        {
            return ParseResult::kIncomplete;
        }
        if (data.compare(pos + len, 2, "\r\n") != 0)
        {
            return ParseResult::kMalformed;
        }
        args.emplace_back(data.substr(pos, len));
        pos += len + 2;
    }
    consumed = pos;
    out = std::move(args);
    return ParseResult::kParsed;
}

// 命令名转大写(只处理 ASCII),用于不区分大小写地比较 AUTH / PING。
std::string VerbOf(const RespCommand& cmd)
{
    std::string verb = cmd.empty() ? std::string() : cmd.front();
    for (char& c : verb)
    {
        if (c >= 'a' && c <= 'z')
        {
            c = static_cast<char>(c - 'a' + 'A');
        }
    }
    return verb;
}

// 把一条命令写成 "AUTH <pw>" 这样的文本:等于测试口令的参数显示成 <pw>。
// 于是 EXPECT_EQ(..., "AUTH <pw>") 既证明口令按原样作为单个参数发出,失败信息里又不出现原文。
std::string DescribeCommand(const RespCommand& cmd)
{
    std::string text;
    for (const std::string& arg : cmd)
    {
        if (!text.empty())
        {
            text += ' ';
        }
        text += arg;
    }
    return MaskPassword(std::move(text));
}

std::string DescribeCommands(const std::vector<RespCommand>& cmds)
{
    std::string text;
    for (const RespCommand& cmd : cmds)
    {
        if (!text.empty())
        {
            text += " | ";
        }
        text += DescribeCommand(cmd);
    }
    return text;
}

// 同 hiredis_command_lifecycle_test.cpp;各自在本 TU 的匿名命名空间里,不构成 ODR 冲突。
uint16_t FindFreeLoopbackPort()
{
#ifdef _WIN32
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
#else
    const int s = ::socket(AF_INET, SOCK_STREAM, IPPROTO_TCP);
    EXPECT_NE(s, -1);
    if (s == -1)
    {
        return 0;
    }
    sockaddr_in addr{};
    addr.sin_family = AF_INET;
    addr.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    addr.sin_port = 0;
    const int bindResult = ::bind(s, reinterpret_cast<sockaddr*>(&addr), sizeof(addr));
    EXPECT_EQ(bindResult, 0);
    if (bindResult != 0)
    {
        ::close(s);
        return 0;
    }
    socklen_t len = sizeof(addr);
    const int nameResult = ::getsockname(s, reinterpret_cast<sockaddr*>(&addr), &len);
    EXPECT_EQ(nameResult, 0);
    if (nameResult != 0)
    {
        ::close(s);
        return 0;
    }
    const uint16_t port = ntohs(addr.sin_port);
    ::close(s);
    return port;
#endif
}

// 假 Redis 对 AUTH 的应答方式。
enum class AuthReplyMode
{
    kOk,            // +OK
    kWrongPass,     // -WRONGPASS invalid username-password pair(Redis 6+ 口令错时的原文)
    kEchoPassword,  // -ERR bad password <收到的口令>:模拟会把口令回显进错误文本的代理
    kSwallow,       // 压住不回;此后这条连接上的命令一律不回(真 Redis 按序应答,前一条不回后面都等着)
    kCloseWithoutReply,    // 不回应答,直接半关写端:客户端在 AUTH 应答前读到 EOF
    kWrongPassThenClose,   // 回 -WRONGPASS 后半关写端:客户端先处理拒绝,再读到 EOF
};

// 假 Redis 眼里的一条连接。
struct FakeConnection
{
    std::vector<RespCommand> commands;   // 按到达顺序记录
    bool stalled = false;                // 此后一律不回:AUTH 被压住,或服务端已半关这条连接
    bool down = false;                   // 服务端已看到这条连接断开
};

// 测试线程经 RunInLoopAndWait 拿到的一份状态拷贝。
struct Snapshot
{
    std::vector<FakeConnection> connections;   // 下标 = 连接建立的先后
    int serverDowns = 0;
    int malformedFrames = 0;
    int reconnectCalls = 0;
    int pingReplies = 0;
    int pingNullReplies = 0;
    int pingSendFailures = 0;
    int businessReplies = 0;
    int businessNullReplies = 0;
    int businessSendFailures = 0;
    bool zoneRedisPresent = false;     // GetConnection() 非空
    bool zoneRedisConnected = false;   // GetConnection() 非空且 connected():即对业务可用
};

class RedisManagerAuth : public ::testing::Test
{
protected:
    void SetUp() override
    {
        // 日志级别与输出函数都是 muduo 的普通全局变量:在 loop 线程启动前改、在它 join 之后改回,
        // 线程创建与 join 提供 happens-before,不与 loop 线程竞争。
        savedLogLevel_ = muduo::Logger::logLevel();
        muduo::Logger::setLogLevel(muduo::Logger::INFO);   // "Redis AUTH ok to" 是 INFO
        ClearCapturedLog();
        muduo::Logger::setOutput(&CaptureLogOutput);

        loopThread_ = std::make_unique<muduo::net::EventLoopThread>();
        loop_.emplace(*loopThread_->startLoop());
        port_ = FindFreeLoopbackPort();
        ASSERT_NE(port_, 0);

        RunInLoopAndWait([this] {
            server_ = std::make_unique<muduo::net::TcpServer>(
                &Loop(), muduo::net::InetAddress(port_, /*loopbackOnly=*/true), "fake_redis");
            // 回调只捕获夹具 this:server_ 在 TearDown 里先于夹具销毁,属于 AGENTS.md §11.7 的同寿命例外。
            // 服务端不保存任何 TcpConnectionPtr,按连接名索引自己的记录。
            server_->setConnectionCallback([this](const muduo::net::TcpConnectionPtr& conn) {
                OnServerConnection(conn);
            });
            server_->setMessageCallback(
                [this](const muduo::net::TcpConnectionPtr& conn, muduo::net::Buffer* buf, muduo::Timestamp) {
                    OnServerMessage(conn, buf);
                });
            server_->start();   // 在 loop 线程上即时 listen,随后的 connect 不会撞拒连
        });
    }

    void TearDown() override
    {
        if (loop_)
        {
            RunInLoopAndWait([this] {
                if (manager_)
                {
                    manager_->Shutdown();
                    manager_.reset();
                }
            });
            // 客户端释放 socket 后,先让服务端处理 EOF 并报告 DOWN。AUTH 后主动半关的连接仍是
            // kDisconnecting,在同一闭包内立刻析构 server 会抢在读事件之前移除仍启用的 Channel。
            if (!WaitUntil([this] { return static_cast<std::size_t>(serverDowns_) == connections_.size(); },
                           kSettleTimeout))
            {
                const Snapshot snap = TakeSnapshot();
                ADD_FAILURE() << "fake Redis did not observe all client disconnects before teardown; connections="
                              << snap.connections.size() << ", serverDowns=" << snap.serverDowns
                              << "; aborting before unsafe server destruction\n" << MaskPassword(CapturedLog());
                std::fflush(stdout);
                std::abort();
            }
            RunInLoopAndWait([this] { server_.reset(); });
            // 再转一圈:让 removeChannel 排队的 dummy(持有 Channel)等 functor 在 loop 停下前跑完,
            // 而不是留给 ~EventLoop 随 pendingFunctors_ 一起丢弃(同 hiredis_command_lifecycle_test)。
            RunInLoopAndWait([] {});
        }
        loopThread_.reset();   // quit + join:此后不再有线程写日志
        loop_.reset();
        muduo::Logger::setOutput(&StdoutLogOutput);
        muduo::Logger::setLogLevel(savedLogLevel_);
    }

    muduo::net::EventLoop& Loop() { return loop_->get(); }

    template <typename F>
    void RunInLoopAndWait(F&& fn)
    {
        std::promise<void> done;
        std::future<void> finished = done.get_future();
        Loop().runInLoop([&] { fn(); done.set_value(); });
        if (finished.wait_for(kSettleTimeout) != std::future_status::ready)
        {
            // 不能直接 return:loop 线程上的闭包按引用捕获了本栈帧,提前返回会让它访问已失效的栈。
            // loop 线程卡死时测试也无从恢复,记一条失败后立即终止进程,而不是无限期挂住。
            ADD_FAILURE() << "the loop thread did not run the task within the deadline; aborting";
            std::fflush(stdout);
            std::abort();
        }
    }

    // 在截止时间(单调时钟)前反复到 loop 线程上求值 pred,成立即返回 true。
    template <typename Pred>
    bool WaitUntil(Pred pred, std::chrono::milliseconds timeout)
    {
        const auto deadline = std::chrono::steady_clock::now() + timeout;
        for (;;)
        {
            bool satisfied = false;
            RunInLoopAndWait([&] { satisfied = pred(); });
            if (satisfied)
            {
                return true;
            }
            if (std::chrono::steady_clock::now() >= deadline)
            {
                return false;
            }
            std::this_thread::sleep_for(kPollInterval);
        }
    }

    // 在整个观察窗口内 pred 都成立才返回 true;用于断言「不该发生」的事。
    template <typename Pred>
    bool HoldsFor(Pred pred, std::chrono::milliseconds window)
    {
        const auto deadline = std::chrono::steady_clock::now() + window;
        for (;;)
        {
            bool holds = false;
            RunInLoopAndWait([&] { holds = pred(); });
            if (!holds)
            {
                return false;
            }
            if (std::chrono::steady_clock::now() >= deadline)
            {
                return true;
            }
            std::this_thread::sleep_for(kPollInterval);
        }
    }

    void ConfigureServer(AuthReplyMode mode, bool disconnectAfterFirstAuthOk)
    {
        RunInLoopAndWait([this, mode, disconnectAfterFirstAuthOk] {
            authMode_ = mode;
            disconnectAfterFirstAuthOk_ = disconnectAfterFirstAuthOk;
        });
    }

    // 此后假 Redis 对 PING 的应答一律推迟这么多秒(0 = 立即回)。只影响 PING,AUTH 照常立即回。
    void SetPingReplyDelay(double delaySec)
    {
        RunInLoopAndWait([this, delaySec] { pingReplyDelaySec_ = delaySec; });
    }

    // 在 loop 线程上建被测的 RedisManager 并发起首次连接。
    // 存活探测默认用长间隔(用例期间一次都不探测),只有测探测的用例才传短值。
    void StartManager(const std::string& password, double reconnectIntervalSec,
                      double livenessProbeIntervalSec = kLongLivenessProbeIntervalSec,
                      double livenessTimeoutSec = kLongLivenessTimeoutSec)
    {
        RunInLoopAndWait([this, password, reconnectIntervalSec, livenessProbeIntervalSec, livenessTimeoutSec] {
            manager_ = std::make_unique<RedisManager>();
            manager_->SetReconnectCallback([this] { OnManagerReconnected(); });
            RedisConnectOptions options;
            options.password = password;
            options.reconnectIntervalSec = reconnectIntervalSec;
            options.reconnectJitterSec = 0.0;   // 握手截止与重连时序按 reconnectIntervalSec 断言,不加抖动
            options.livenessProbeIntervalSec = livenessProbeIntervalSec;
            options.livenessTimeoutSec = livenessTimeoutSec;
            manager_->Connect(&Loop(), muduo::net::InetAddress("127.0.0.1", port_), std::move(options));
        });
    }

    Snapshot TakeSnapshot()
    {
        Snapshot snap;
        RunInLoopAndWait([this, &snap] {
            snap.connections = connections_;
            snap.serverDowns = serverDowns_;
            snap.malformedFrames = malformedFrames_;
            snap.reconnectCalls = reconnectCalls_;
            snap.pingReplies = pingReplies_;
            snap.pingNullReplies = pingNullReplies_;
            snap.pingSendFailures = pingSendFailures_;
            snap.businessReplies = businessReplies_;
            snap.businessNullReplies = businessNullReplies_;
            snap.businessSendFailures = businessSendFailures_;
            snap.zoneRedisPresent = ZoneRedisPresent();
            snap.zoneRedisConnected = ZoneRedisConnected();
        });
        return snap;
    }

    // ---- 以下只在 loop 线程上执行 ----

    bool ZoneRedisPresent() const
    {
        return manager_ && manager_->GetConnection();
    }

    bool ZoneRedisConnected() const
    {
        return ZoneRedisPresent() && manager_->GetConnection()->connected();
    }

    // 收到过至少一条命令的连接数:被截止定时器拆掉的半成品连接(建连都没完成)不计。
    std::size_t ConnectionsWithCommands() const
    {
        std::size_t count = 0;
        for (const FakeConnection& connection : connections_)
        {
            if (!connection.commands.empty())
            {
                ++count;
            }
        }
        return count;
    }

    // 收到过 PING 的最后一条连接:只有移交给业务的连接上才会出现 PING(重连回调发的或存活探测发的)。
    std::optional<std::size_t> LastPromotedConnection() const
    {
        for (std::size_t i = connections_.size(); i > 0; --i)
        {
            for (const RespCommand& cmd : connections_[i - 1].commands)
            {
                if (VerbOf(cmd) == "PING")
                {
                    return i - 1;
                }
            }
        }
        return std::nullopt;
    }

    // 照业务用法往 GetConnection() 上发一条命令,记下它最后拿到的是非空应答、空应答,还是根本没发出去。
    void SendBusinessCommand()
    {
        const RedisManager::HiredisPtr& redis = manager_->GetConnection();
        if (!redis)
        {
            ++businessSendFailures_;
            return;
        }
        const int ret = redis->command(
            [this](hiredis::Hiredis*, redisReply* reply) {
                if (reply != nullptr)
                {
                    ++businessReplies_;
                }
                else
                {
                    ++businessNullReplies_;
                }
            },
            "GET %s", "liveness:test");
        if (ret != REDIS_OK)
        {
            ++businessSendFailures_;
        }
    }

    // RedisManager 的重连回调。照生产用法(scene 在回调里补发积压),回调里立刻往新连接上发一条命令。
    void OnManagerReconnected()
    {
        ++reconnectCalls_;
        const RedisManager::HiredisPtr& redis = manager_->GetConnection();
        if (!redis)
        {
            ++pingSendFailures_;
            return;
        }
        const int ret = redis->command(
            [this](hiredis::Hiredis*, redisReply* reply) {
                if (reply != nullptr)
                {
                    ++pingReplies_;
                }
                else
                {
                    ++pingNullReplies_;
                }
            },
            "PING");
        if (ret != REDIS_OK)
        {
            ++pingSendFailures_;
        }
    }

    void OnServerConnection(const muduo::net::TcpConnectionPtr& conn)
    {
        if (conn->connected())
        {
            connectionIndexByName_[conn->name()] = connections_.size();
            connections_.emplace_back();
            return;
        }
        ++serverDowns_;
        if (const auto it = connectionIndexByName_.find(conn->name()); it != connectionIndexByName_.end())
        {
            connections_[it->second].down = true;
        }
    }

    void OnServerMessage(const muduo::net::TcpConnectionPtr& conn, muduo::net::Buffer* buf)
    {
        const auto it = connectionIndexByName_.find(conn->name());
        if (it == connectionIndexByName_.end())
        {
            ++malformedFrames_;
            buf->retrieveAll();
            return;
        }
        const std::size_t index = it->second;
        for (;;)
        {
            std::size_t consumed = 0;
            RespCommand cmd;
            const ParseResult result =
                ParseCommand(std::string_view(buf->peek(), buf->readableBytes()), consumed, cmd);
            if (result == ParseResult::kIncomplete)
            {
                return;
            }
            if (result == ParseResult::kMalformed)
            {
                ++malformedFrames_;
                buf->retrieveAll();
                return;
            }
            buf->retrieve(consumed);
            connections_[index].commands.push_back(cmd);
            ReplyTo(conn, index, cmd);
        }
    }

    void ReplyTo(const muduo::net::TcpConnectionPtr& conn, std::size_t index, const RespCommand& cmd)
    {
        FakeConnection& fake = connections_[index];
        if (fake.stalled)
        {
            return;
        }
        const std::string verb = VerbOf(cmd);
        if (verb == "AUTH")
        {
            switch (authMode_)
            {
            case AuthReplyMode::kOk:
                conn->send("+OK\r\n");
                if (disconnectAfterFirstAuthOk_ && !disconnectDone_)
                {
                    disconnectDone_ = true;
                    // 半关写端:+OK 发完后才发 FIN,客户端先处理应答、再读到 EOF 视为断线。
                    // 此后 send 一律被 muduo 丢弃(state 已是 kDisconnecting)。
                    conn->shutdown();
                }
                return;
            case AuthReplyMode::kWrongPass:
                conn->send("-WRONGPASS invalid username-password pair\r\n");
                return;
            case AuthReplyMode::kEchoPassword:
                conn->send("-ERR bad password " + (cmd.size() > 1 ? cmd.back() : std::string()) + "\r\n");
                return;
            case AuthReplyMode::kSwallow:
                fake.stalled = true;
                return;
            case AuthReplyMode::kCloseWithoutReply:
                fake.stalled = true;   // 此后这条连接上什么都不回
                conn->shutdown();
                return;
            case AuthReplyMode::kWrongPassThenClose:
                fake.stalled = true;
                // 同 kOk 的断开:muduo 先把输出缓冲发完再发 FIN,客户端先处理拒绝、再读到 EOF。
                conn->send("-WRONGPASS invalid username-password pair\r\n");
                conn->shutdown();
                return;
            }
            return;
        }
        if (verb == "PING")
        {
            if (pingReplyDelaySec_ > 0)
            {
                // 定时器闭包只捕获 weak 连接(AGENTS.md §11.7):连接或 server 先没了,推迟的应答就丢掉。
                Loop().runAfter(pingReplyDelaySec_,
                                [weak = std::weak_ptr<muduo::net::TcpConnection>(conn)] {
                                    if (const muduo::net::TcpConnectionPtr locked = weak.lock())
                                    {
                                        locked->send("+PONG\r\n");
                                    }
                                });
                return;
            }
            conn->send("+PONG\r\n");
            return;
        }
        conn->send("-ERR unknown command\r\n");
    }

    // loopThread_ 排第一:最后析构。TearDown 已显式 reset 它,这里只是兜底顺序。
    std::unique_ptr<muduo::net::EventLoopThread> loopThread_;
    // 引用而非裸指针成员(裸指针成员门禁覆盖测试工程);SetUp 里绑定,TearDown 在 join 之后清空。
    std::optional<std::reference_wrapper<muduo::net::EventLoop>> loop_;
    uint16_t port_ = 0;
    muduo::Logger::LogLevel savedLogLevel_ = muduo::Logger::WARN;
    std::unique_ptr<muduo::net::TcpServer> server_;
    std::unique_ptr<RedisManager> manager_;

    // 以下只在 loop 线程上读写;测试线程经 RunInLoopAndWait 读取(promise/future 提供 happens-before)。
    AuthReplyMode authMode_ = AuthReplyMode::kOk;
    bool disconnectAfterFirstAuthOk_ = false;   // 只对第一次 AUTH 成功生效
    bool disconnectDone_ = false;
    double pingReplyDelaySec_ = 0;   // 见 SetPingReplyDelay
    std::map<std::string, std::size_t> connectionIndexByName_;   // TcpServer 生成的连接名在本 server 内唯一
    std::vector<FakeConnection> connections_;
    int serverDowns_ = 0;
    int malformedFrames_ = 0;
    int reconnectCalls_ = 0;      // RedisManager 重连回调被调用的次数
    int pingReplies_ = 0;         // 回调里发出的 PING 拿到非空应答的次数
    int pingNullReplies_ = 0;     // 回调里发出的 PING 以空应答释放的次数(连接断开或被释放)
    int pingSendFailures_ = 0;    // 回调里拿不到连接或 command() 返回非 OK 的次数
    int businessReplies_ = 0;       // SendBusinessCommand 发出的命令拿到非空应答的次数
    int businessNullReplies_ = 0;   // 同上,以空应答结束(连接断开或被释放)的次数
    int businessSendFailures_ = 0;  // 同上,拿不到连接或 command() 返回非 OK 的次数
};

// 存活契约的用例沿用同一个假 Redis 与夹具,只换一个测试套件名,便于单独过滤(--gtest_filter=RedisManagerLiveness.*)。
class RedisManagerLiveness : public RedisManagerAuth
{
};

} // namespace

// ---------------------------------------------------------------------------
// 1. 空口令:不发 AUTH,连上即对业务可见,连接上的第一条命令就是重连回调自己发的;回调恰好一次。
// ---------------------------------------------------------------------------

TEST_F(RedisManagerAuth, EmptyPasswordSendsNoAuth)
{
    StartManager("", kLongReconnectIntervalSec);
    ASSERT_TRUE(WaitUntil([this] { return pingReplies_ >= 1; }, kSettleTimeout))
        << "the PING sent from the reconnect callback never got a reply";

    const Snapshot snap = TakeSnapshot();
    EXPECT_EQ(snap.reconnectCalls, 1);
    EXPECT_EQ(snap.pingSendFailures, 0);
    EXPECT_EQ(snap.malformedFrames, 0);
    EXPECT_TRUE(snap.zoneRedisConnected);
    ASSERT_EQ(snap.connections.size(), 1u);
    EXPECT_EQ(DescribeCommands(snap.connections[0].commands), "PING")
        << "without a password the first command must be the one sent by the reconnect callback";
    for (const FakeConnection& connection : snap.connections)
    {
        for (const RespCommand& cmd : connection.commands)
        {
            EXPECT_NE(VerbOf(cmd), "AUTH") << "AUTH must not be sent when the password is empty";
        }
    }
    EXPECT_FALSE(Contains(CapturedLog(), "Redis AUTH"));
}

// ---------------------------------------------------------------------------
// 2. 有口令:AUTH 是连接上的第一条命令;+OK 之后连接才对业务可见,重连回调里发的 PING 紧随其后;回调恰好一次。
// ---------------------------------------------------------------------------

TEST_F(RedisManagerAuth, AuthIsFirstCommandAndCallbackFollows)
{
    StartManager(kTestPassword, kLongReconnectIntervalSec);
    ASSERT_TRUE(WaitUntil([this] { return pingReplies_ >= 1; }, kSettleTimeout))
        << "the PING sent from the reconnect callback never got a reply";

    const Snapshot snap = TakeSnapshot();
    EXPECT_EQ(snap.reconnectCalls, 1);
    EXPECT_EQ(snap.pingSendFailures, 0);
    EXPECT_EQ(snap.malformedFrames, 0);
    EXPECT_TRUE(snap.zoneRedisConnected);
    ASSERT_EQ(snap.connections.size(), 1u);
    // <pw> 表示该参数与测试口令逐字节相等(见 DescribeCommand)。
    EXPECT_EQ(DescribeCommands(snap.connections[0].commands), "AUTH <pw> | PING");

    // 回调只在 +OK 之后才被调用,所以 PING 拿到应答时 OnAuthReply 的日志已经写入。
    const std::string log = CapturedLog();
    EXPECT_TRUE(Contains(log, "Redis AUTH ok to")) << MaskPassword(log);
    EXPECT_FALSE(Contains(log, kTestPassword)) << "password leaked into the log";
}

TEST_F(RedisManagerAuth, ReconnectCallbackCanDestroyManagerDuringConnectAndAuthReply)
{
    for (const std::string password : {std::string{}, std::string{kTestPassword}})
    {
        std::weak_ptr<int> callbackCapture;
        int calls = 0;
        RunInLoopAndWait([&] {
            manager_ = std::make_unique<RedisManager>();
            auto sentinel = std::make_shared<int>(0);
            callbackCapture = sentinel;
            manager_->SetReconnectCallback([&, sentinel] {
                ++calls;
                manager_.reset();
                EXPECT_FALSE(callbackCapture.expired()) << "清空管理器成员不得销毁正在执行的回调";
            });
            RedisConnectOptions options;
            options.password = password;
            options.reconnectIntervalSec = kLongReconnectIntervalSec;
            manager_->Connect(&Loop(), muduo::net::InetAddress("127.0.0.1", port_), std::move(options));
        });
        ASSERT_TRUE(WaitUntil([&] {
            return calls == 1 && !manager_ && callbackCapture.expired() &&
                static_cast<std::size_t>(serverDowns_) == connections_.size();
        }, kSettleTimeout)) << "管理器销毁后旧连接和回调必须完成清理";
        RunInLoopAndWait([] {});
        EXPECT_EQ(calls, 1);
    }
}

TEST_F(RedisManagerAuth, ReconnectCallbackCanStartNewAuthenticatedConnection)
{
    int promotions = 0;
    int replies = 0;
    RunInLoopAndWait([&] {
        manager_ = std::make_unique<RedisManager>();
        manager_->SetReconnectCallback([&] {
            ++promotions;
            if (promotions == 1)
            {
                RedisConnectOptions options;
                options.password = kTestPassword;
                options.reconnectIntervalSec = kLongReconnectIntervalSec;
                manager_->Connect(&Loop(), muduo::net::InetAddress("127.0.0.1", port_), std::move(options));
                return;
            }
            ASSERT_EQ(promotions, 2);
            ASSERT_EQ(manager_->GetConnection()->command([&](hiredis::Hiredis*, redisReply* reply) {
                if (reply) ++replies;
            }, "PING"), REDIS_OK);
        });
        RedisConnectOptions options;
        options.password = kTestPassword;
        options.reconnectIntervalSec = kLongReconnectIntervalSec;
        manager_->Connect(&Loop(), muduo::net::InetAddress("127.0.0.1", port_), std::move(options));
    });
    ASSERT_TRUE(WaitUntil([&] { return replies == 1; }, kSettleTimeout));
    RunInLoopAndWait([&] {
        EXPECT_EQ(promotions, 2);
        ASSERT_EQ(connections_.size(), 2u);
        EXPECT_EQ(DescribeCommands(connections_[0].commands), "AUTH <pw>");
        EXPECT_EQ(DescribeCommands(connections_[1].commands), "AUTH <pw> | PING");
        manager_->Shutdown();
    });
}

TEST_F(RedisManagerAuth, PendingNullReplyCanDestroyManagerDuringShutdownOrNewConnect)
{
    for (const bool reconnect : {false, true})
    {
        int expectedReplies = 0;
        RunInLoopAndWait([&] { expectedReplies = pingReplies_ + 1; });
        StartManager("", kLongReconnectIntervalSec);
        ASSERT_TRUE(WaitUntil([&] { return pingReplies_ == expectedReplies; }, kSettleTimeout));
        int nullReplies = 0;
        RunInLoopAndWait([&] {
            connections_.back().stalled = true;
            ASSERT_EQ(manager_->GetConnection()->command([&](hiredis::Hiredis*, redisReply* reply) {
                EXPECT_EQ(reply, nullptr);
                if (!reply) ++nullReplies;
                manager_.reset();
            }, "PING"), REDIS_OK);
            if (reconnect)
            {
                RedisConnectOptions options;
                options.reconnectIntervalSec = kLongReconnectIntervalSec;
                manager_->Connect(&Loop(), muduo::net::InetAddress("127.0.0.1", port_), std::move(options));
            }
            else
            {
                manager_->Shutdown();
            }
            EXPECT_EQ(manager_, nullptr);
            EXPECT_EQ(nullReplies, 1);
        });
        ASSERT_TRUE(WaitUntil([&] {
            return static_cast<std::size_t>(serverDowns_) == connections_.size();
        }, kSettleTimeout));
    }
}

TEST_F(RedisManagerLiveness, DroppedConnectionNullReplyCanDestroyManager)
{
    StartManager("", kLongReconnectIntervalSec, 0.02, 0.04);
    ASSERT_TRUE(WaitUntil([&] { return pingReplies_ >= 1; }, kSettleTimeout));
    int nullReplies = 0;
    RunInLoopAndWait([&] {
        connections_.back().stalled = true;
        ASSERT_EQ(manager_->GetConnection()->command([&](hiredis::Hiredis*, redisReply* reply) {
            EXPECT_EQ(reply, nullptr);
            if (!reply) ++nullReplies;
            manager_.reset();
        }, "PING"), REDIS_OK);
    });
    ASSERT_TRUE(WaitUntil([&] {
        return nullReplies == 1 && !manager_ && static_cast<std::size_t>(serverDowns_) == connections_.size();
    }, kSettleTimeout));
    RunInLoopAndWait([] {});
    EXPECT_EQ(nullReplies, 1);
}

// ---------------------------------------------------------------------------
// 3. AUTH 被拒(fail-closed):连接不对业务可见、不调回调、握手连接上只有 AUTH;打 ERROR,日志不含口令;
//    在回调栈外重建握手连接重试,新连接的第一条命令仍是 AUTH。
// ---------------------------------------------------------------------------

TEST_F(RedisManagerAuth, RejectedAuthKeepsZoneRedisUnavailableAndRetries)
{
    ConfigureServer(AuthReplyMode::kWrongPass, /*disconnectAfterFirstAuthOk=*/false);
    StartManager(kTestPassword, kReconnectIntervalSec);
    // 至少两条连接收到过 AUTH = 被拒之后确实重建重试了。
    ASSERT_TRUE(WaitUntil(
        [this] { return ConnectionsWithCommands() >= 2 && Contains(CapturedLog(), "Redis AUTH rejected by"); },
        kSettleTimeout))
        << "a rejected AUTH was not retried on a new connection within the deadline";

    const Snapshot snap = TakeSnapshot();
    EXPECT_EQ(snap.reconnectCalls, 0) << "a connection whose AUTH was rejected must never be handed to the business";
    EXPECT_TRUE(snap.zoneRedisPresent) << "GetConnection() must stay non-null after Connect";
    EXPECT_FALSE(snap.zoneRedisConnected) << "GetConnection() must report not connected while AUTH keeps failing";
    EXPECT_EQ(snap.pingSendFailures, 0);
    EXPECT_EQ(snap.malformedFrames, 0);
    for (std::size_t i = 0; i < snap.connections.size(); ++i)
    {
        const std::vector<RespCommand>& cmds = snap.connections[i].commands;
        if (cmds.empty())
        {
            continue;   // 建连都没完成就被截止定时器拆掉的半成品连接
        }
        EXPECT_EQ(DescribeCommands(cmds), "AUTH <pw>")
            << "connection #" << i << ": nothing but AUTH may be sent before authentication succeeds";
    }

    const std::string log = CapturedLog();
    EXPECT_TRUE(Contains(log, "WRONGPASS invalid username-password pair"))
        << "server text without the password should be logged as is: " << MaskPassword(log);
    EXPECT_TRUE(Contains(log, "metric=zone_redis_auth_failed")) << MaskPassword(log);
    EXPECT_TRUE(Contains(log, "Attempting Redis reconnect")) << MaskPassword(log);
    EXPECT_FALSE(Contains(log, "Redis AUTH ok to"));
    EXPECT_FALSE(Contains(log, kTestPassword)) << "password leaked into the log";
}

// ---------------------------------------------------------------------------
// 4. 服务端把口令回显进错误文本:只保留错误码,其余换成 <redacted>,日志里没有口令原文。
// ---------------------------------------------------------------------------

TEST_F(RedisManagerAuth, PasswordEchoedByServerIsRedacted)
{
    ConfigureServer(AuthReplyMode::kEchoPassword, /*disconnectAfterFirstAuthOk=*/false);
    StartManager(kTestPassword, kLongReconnectIntervalSec);
    ASSERT_TRUE(WaitUntil([] { return Contains(CapturedLog(), "Redis AUTH rejected by"); }, kSettleTimeout))
        << "the rejected AUTH was never logged";

    const Snapshot snap = TakeSnapshot();
    EXPECT_EQ(snap.reconnectCalls, 0);
    ASSERT_EQ(snap.connections.size(), 1u);
    ASSERT_FALSE(snap.connections[0].commands.empty());
    EXPECT_EQ(DescribeCommand(snap.connections[0].commands.front()), "AUTH <pw>")
        << "precondition: the fake server echoes exactly the password it received";

    const std::string log = CapturedLog();
    EXPECT_TRUE(Contains(log, "ERR <redacted>"))
        << "the error code must survive, everything after it must be redacted: " << MaskPassword(log);
    EXPECT_FALSE(Contains(log, kTestPassword)) << "password echoed by the server leaked into the log";
    EXPECT_FALSE(Contains(log, "bad password"))
        << "the server text after the error code must be replaced, not just the password: " << MaskPassword(log);
}

// ---------------------------------------------------------------------------
// 5. AUTH 成功后服务端主动断开:限时内重连,新连接上的第一条命令仍是 AUTH;重连回调累计 2 次。
// ---------------------------------------------------------------------------

TEST_F(RedisManagerAuth, ReauthenticatesAfterServerDisconnect)
{
    ConfigureServer(AuthReplyMode::kOk, /*disconnectAfterFirstAuthOk=*/true);
    StartManager(kTestPassword, kReconnectIntervalSec);
    // 第 1 条连接上的 PING 撞上服务端半关,只会以空应答释放;所以非空的 PING 应答只可能来自重连后的连接。
    ASSERT_TRUE(WaitUntil([this] { return reconnectCalls_ >= 2 && pingReplies_ >= 1; }, kSettleTimeout))
        << "no successful reconnect within the deadline\n" << MaskPassword(CapturedLog());

    const Snapshot snap = TakeSnapshot();
    EXPECT_EQ(snap.reconnectCalls, 2);
    EXPECT_EQ(snap.pingSendFailures, 0);
    EXPECT_EQ(snap.malformedFrames, 0);
    EXPECT_TRUE(snap.zoneRedisConnected);
    ASSERT_GE(snap.connections.size(), 2u);
    ASSERT_FALSE(snap.connections[0].commands.empty());
    EXPECT_EQ(DescribeCommand(snap.connections[0].commands.front()), "AUTH <pw>");

    // 之后每条发过命令的连接,第一条都是 AUTH;只有完成握手、移交给业务的那一条会出现回调发的 PING,且恰好一条。
    // (截止定时器若在某次建连或 AUTH 应答前到点,会拆掉那条半成品连接,服务端多看到一条没有命令或只有 AUTH 的连接。)
    std::size_t promoted = 0;
    for (std::size_t i = 1; i < snap.connections.size(); ++i)
    {
        const std::vector<RespCommand>& cmds = snap.connections[i].commands;
        if (cmds.empty())
        {
            continue;
        }
        EXPECT_EQ(DescribeCommand(cmds.front()), "AUTH <pw>") << "connection #" << i;
        for (const RespCommand& cmd : cmds)
        {
            if (VerbOf(cmd) == "PING")
            {
                ++promoted;
                break;
            }
        }
    }
    EXPECT_EQ(promoted, 1u);

    const std::string log = CapturedLog();
    EXPECT_TRUE(Contains(log, "Redis disconnected")) << MaskPassword(log);
    // 断线日志带 hiredis 记下的原因(P0-E 第 3 批):服务端半关写端,客户端读到 EOF,errstr 是 hiredis net.c 的固定文本。
    // 以前这一行只有 status,到线、被踢、网络断开在日志里分不开。
    EXPECT_TRUE(Contains(log, "reason=Server closed the connection")) << MaskPassword(log);
    EXPECT_EQ(CountOccurrences(log, "Redis AUTH ok to"), 2u) << MaskPassword(log);
    EXPECT_FALSE(Contains(log, kTestPassword)) << "password leaked into the log";
}

// ---------------------------------------------------------------------------
// 6. 握手中(AUTH 被压住不回)Shutdown:握手期间连接不对业务可见;Shutdown 不崩、不回调、不重连,
//    服务端不再出现新连接。
// ---------------------------------------------------------------------------

TEST_F(RedisManagerAuth, ShutdownDuringHandshakeIsQuiet)
{
    ConfigureServer(AuthReplyMode::kSwallow, /*disconnectAfterFirstAuthOk=*/false);
    // 长间隔:截止定时器在整个用例期间不会到点,握手停在「AUTH 已发、没有应答」。
    StartManager(kTestPassword, kLongReconnectIntervalSec);
    ASSERT_TRUE(WaitUntil(
        [this] { return connections_.size() == 1 && !connections_[0].commands.empty(); },
        kSettleTimeout))
        << "AUTH never reached the fake server";

    bool presentDuringHandshake = false;
    bool connectedDuringHandshake = true;
    std::size_t connectionsAtShutdown = 0;
    RunInLoopAndWait([this, &presentDuringHandshake, &connectedDuringHandshake, &connectionsAtShutdown] {
        presentDuringHandshake = ZoneRedisPresent();
        connectedDuringHandshake = ZoneRedisConnected();
        manager_->Shutdown();
        connectionsAtShutdown = connections_.size();
    });
    EXPECT_TRUE(presentDuringHandshake) << "GetConnection() must stay non-null while the handshake is in flight";
    EXPECT_FALSE(connectedDuringHandshake) << "the connection must not be usable before AUTH succeeds";

    const Snapshot snap = TakeSnapshot();
    ASSERT_EQ(snap.connections.size(), 1u);
    EXPECT_EQ(DescribeCommands(snap.connections[0].commands), "AUTH <pw>");
    EXPECT_EQ(snap.reconnectCalls, 0);
    EXPECT_EQ(snap.pingReplies, 0);
    EXPECT_EQ(snap.pingNullReplies, 0);
    EXPECT_FALSE(snap.zoneRedisPresent) << "Shutdown must release the zone Redis object";

    EXPECT_TRUE(WaitUntil([this] { return serverDowns_ >= 1; }, kSettleTimeout))
        << "Shutdown did not close the handshake connection";
    EXPECT_TRUE(HoldsFor(
        [this, connectionsAtShutdown] { return connections_.size() == connectionsAtShutdown && reconnectCalls_ == 0; },
        kQuietWindow))
        << "no new connection and no reconnect callback are allowed after Shutdown";

    const std::string log = CapturedLog();
    EXPECT_FALSE(Contains(log, "Redis AUTH ok to"));
    EXPECT_FALSE(Contains(log, "Redis AUTH rejected by"));
    EXPECT_FALSE(Contains(log, "Redis disconnected")) << "Shutdown must not go through the disconnect/reconnect path";
    EXPECT_FALSE(Contains(log, "Attempting Redis reconnect"));
    EXPECT_FALSE(Contains(log, "Redis AUTH timed out")) << "an explicit Shutdown is not an AUTH failure";
    EXPECT_FALSE(Contains(log, kTestPassword)) << "password leaked into the log";
}

// ---------------------------------------------------------------------------
// 7. AUTH 一直没有应答:一个重连间隔后在回调栈外重建握手连接;全程不对业务可见、不调回调,
//    每条连接上只有一条 AUTH;截止时打带 metric 的超时 ERROR(不能只剩 INFO 级的重连日志)。
// ---------------------------------------------------------------------------

TEST_F(RedisManagerAuth, AuthTimeoutRebuildsConnection)
{
    ConfigureServer(AuthReplyMode::kSwallow, /*disconnectAfterFirstAuthOk=*/false);
    StartManager(kTestPassword, kReconnectIntervalSec);
    ASSERT_TRUE(WaitUntil([this] { return ConnectionsWithCommands() >= 2; }, kSettleTimeout))
        << "an unanswered AUTH did not lead to a rebuilt handshake connection within the deadline";

    const Snapshot snap = TakeSnapshot();
    EXPECT_EQ(snap.reconnectCalls, 0);
    EXPECT_TRUE(snap.zoneRedisPresent);
    EXPECT_FALSE(snap.zoneRedisConnected);
    EXPECT_EQ(snap.pingSendFailures, 0);
    EXPECT_EQ(snap.malformedFrames, 0);
    for (std::size_t i = 0; i < snap.connections.size(); ++i)
    {
        const std::vector<RespCommand>& cmds = snap.connections[i].commands;
        if (cmds.empty())
        {
            continue;
        }
        EXPECT_EQ(DescribeCommands(cmds), "AUTH <pw>") << "connection #" << i;
    }

    // 第 2 条收到 AUTH 的连接只能由截止定时器重建,而重建前上一条连接上的 AUTH 已发出且没有应答,
    // 所以超时 ERROR 一定已经写出(与回环上的应答快慢无关)。
    const std::string log = CapturedLog();
    EXPECT_TRUE(Contains(log, "Attempting Redis reconnect")) << MaskPassword(log);
    EXPECT_TRUE(Contains(log, "Redis AUTH timed out")) << "an unanswered AUTH must be reported: " << MaskPassword(log);
    EXPECT_TRUE(Contains(log, "metric=zone_redis_auth_failed")) << MaskPassword(log);
    EXPECT_FALSE(Contains(log, "Redis AUTH ok to"));
    EXPECT_FALSE(Contains(log, "Redis AUTH rejected by"));
    EXPECT_FALSE(Contains(log, kTestPassword)) << "password leaked into the log";
}

// ---------------------------------------------------------------------------
// 8. 重连间隔非正数:打 WARN 并回落到默认值,连接照常建立。
// ---------------------------------------------------------------------------

TEST_F(RedisManagerAuth, NonPositiveReconnectIntervalFallsBackToDefault)
{
    StartManager("", 0.0);
    // WARN 在 Connect 里同步写出,StartManager 返回时已在截获的日志里。
    EXPECT_TRUE(Contains(CapturedLog(), "Redis reconnect interval 0s is not positive, using 3s")) << CapturedLog();
    ASSERT_TRUE(WaitUntil([this] { return pingReplies_ >= 1; }, kSettleTimeout))
        << "the connection did not come up with the fallback interval";

    const Snapshot snap = TakeSnapshot();
    EXPECT_EQ(snap.reconnectCalls, 1);
    EXPECT_TRUE(snap.zoneRedisConnected);
}

// ---------------------------------------------------------------------------
// 8b. 重连抖动(docs/design/infra-reconnect-overview.md §4 R18):重连周期 = 基础间隔 + [0, 抖动上限),
//     每次 Connect 抽一次;抖动上限为 0 时周期恰是基础间隔。只读周期,不等定时器,结果与机器快慢无关。
// ---------------------------------------------------------------------------

TEST(RedisManagerReconnectJitter, StaysWithinTheConfiguredWindow)
{
    EXPECT_DOUBLE_EQ(RedisManager::JitteredReconnectIntervalSec(3.0, 1.0, 0.0), 3.0);
    EXPECT_DOUBLE_EQ(RedisManager::JitteredReconnectIntervalSec(3.0, 1.0, 0.5), 3.5);
    EXPECT_LT(RedisManager::JitteredReconnectIntervalSec(3.0, 1.0, 0.999999), 4.0);
    EXPECT_DOUBLE_EQ(RedisManager::JitteredReconnectIntervalSec(3.0, 0.0, 0.5), 3.0) << "抖动关闭";
    EXPECT_DOUBLE_EQ(RedisManager::JitteredReconnectIntervalSec(3.0, -1.0, 0.5), 3.0) << "负的抖动上限按关闭算";
    EXPECT_DOUBLE_EQ(RedisManager::JitteredReconnectIntervalSec(3.0, 1.0, 1.5), 3.0) << "越界的随机数收回区间";
    EXPECT_DOUBLE_EQ(RedisManager::JitteredReconnectIntervalSec(3.0, 1.0, -0.5), 3.0);
}

TEST_F(RedisManagerAuth, ReconnectPeriodIsBaseIntervalPlusJitterDrawnAtConnect)
{
    // StartManager 关掉了抖动:周期恰是基础间隔。
    StartManager("", kLongReconnectIntervalSec);
    ASSERT_TRUE(WaitUntil([this] { return pingReplies_ >= 1; }, kSettleTimeout))
        << "the first connection did not come up";
    double withoutJitter = 0;
    RunInLoopAndWait([&] { withoutJitter = manager_->ReconnectPeriodSec(); });
    EXPECT_DOUBLE_EQ(withoutJitter, kLongReconnectIntervalSec);

    // 生产不设 reconnectJitterSec:默认就带抖动。同一个管理器再 Connect 一次,周期重新抽,
    // 落在 [基础间隔, 基础间隔 + 默认上限)。
    double byDefault = 0;
    RunInLoopAndWait([&] {
        RedisConnectOptions options;
        options.reconnectIntervalSec = kLongReconnectIntervalSec;
        manager_->Connect(&Loop(), muduo::net::InetAddress("127.0.0.1", port_), std::move(options));
        byDefault = manager_->ReconnectPeriodSec();
    });
    ASSERT_TRUE(WaitUntil([this] { return pingReplies_ >= 2; }, kSettleTimeout))
        << "the second connection did not come up";
    EXPECT_GE(byDefault, kLongReconnectIntervalSec);
    EXPECT_LT(byDefault, kLongReconnectIntervalSec + RedisConnectOptions::kDefaultReconnectJitterSec);
}

// ---------------------------------------------------------------------------
// 9. 服务端收到 AUTH 后不回应答、直接断开:空应答不移交、不调回调;断线回调里的 ScheduleReconnect 不打乱截止定时器,
//    到点按 AUTH 超时打 ERROR 并重建握手连接,新连接上的第一条也是唯一一条命令仍是 AUTH。
// ---------------------------------------------------------------------------

TEST_F(RedisManagerAuth, ServerClosingBeforeAuthReplyIsRetried)
{
    ConfigureServer(AuthReplyMode::kCloseWithoutReply, /*disconnectAfterFirstAuthOk=*/false);
    StartManager(kTestPassword, kReconnectIntervalSec);
    ASSERT_TRUE(WaitUntil([this] { return ConnectionsWithCommands() >= 2; }, kSettleTimeout))
        << "a handshake connection closed before the AUTH reply was not rebuilt within the deadline";
    // 断线日志单独等:它取决于客户端何时读到 EOF,与重建是否发生无关;失败时直接指向「断线没被察觉」。
    EXPECT_TRUE(WaitUntil([] { return Contains(CapturedLog(), "Redis disconnected"); }, kSettleTimeout))
        << "the server closing the handshake connection was never reported: " << MaskPassword(CapturedLog());

    const Snapshot snap = TakeSnapshot();
    EXPECT_EQ(snap.reconnectCalls, 0) << "a connection lost before the AUTH reply must never be handed to the business";
    EXPECT_TRUE(snap.zoneRedisPresent) << "GetConnection() must stay non-null after Connect";
    EXPECT_FALSE(snap.zoneRedisConnected);
    EXPECT_EQ(snap.pingSendFailures, 0);
    EXPECT_EQ(snap.malformedFrames, 0);
    for (std::size_t i = 0; i < snap.connections.size(); ++i)
    {
        const std::vector<RespCommand>& cmds = snap.connections[i].commands;
        if (cmds.empty())
        {
            continue;
        }
        EXPECT_EQ(DescribeCommands(cmds), "AUTH <pw>") << "connection #" << i;
    }

    // 第 2 条收到 AUTH 的连接只能由截止定时器重建,而上一条连接上的 AUTH 始终没有非空应答,
    // 所以重建时一定按超时报过 ERROR(与断线先于还是晚于截止无关)。
    const std::string log = CapturedLog();
    EXPECT_TRUE(Contains(log, "Redis AUTH timed out")) << MaskPassword(log);
    EXPECT_TRUE(Contains(log, "metric=zone_redis_auth_failed")) << MaskPassword(log);
    EXPECT_FALSE(Contains(log, "Redis AUTH ok to"));
    EXPECT_FALSE(Contains(log, "Redis AUTH rejected by")) << "the server never replied, so nothing can be rejected";
    EXPECT_FALSE(Contains(log, kTestPassword)) << "password leaked into the log";
}

// ---------------------------------------------------------------------------
// 10. AUTH 被拒之后、截止之前服务端断开:拒绝当场打 ERROR;随后的断线不让连接移交、不调回调,
//     断线回调里的 ScheduleReconnect 不打乱截止定时器,到点照常重建握手连接。
// ---------------------------------------------------------------------------

TEST_F(RedisManagerAuth, ServerClosingAfterRejectedAuthIsRetried)
{
    ConfigureServer(AuthReplyMode::kWrongPassThenClose, /*disconnectAfterFirstAuthOk=*/false);
    StartManager(kTestPassword, kReconnectIntervalSec);
    ASSERT_TRUE(WaitUntil(
        [this] { return ConnectionsWithCommands() >= 2 && Contains(CapturedLog(), "Redis AUTH rejected by"); },
        kSettleTimeout))
        << "a rejected AUTH followed by a server close was not retried on a new connection within the deadline";
    EXPECT_TRUE(WaitUntil([] { return Contains(CapturedLog(), "Redis disconnected"); }, kSettleTimeout))
        << "the server closing the handshake connection was never reported: " << MaskPassword(CapturedLog());

    const Snapshot snap = TakeSnapshot();
    EXPECT_EQ(snap.reconnectCalls, 0) << "a connection whose AUTH was rejected must never be handed to the business";
    EXPECT_TRUE(snap.zoneRedisPresent) << "GetConnection() must stay non-null after Connect";
    EXPECT_FALSE(snap.zoneRedisConnected);
    EXPECT_EQ(snap.pingSendFailures, 0);
    EXPECT_EQ(snap.malformedFrames, 0);
    for (std::size_t i = 0; i < snap.connections.size(); ++i)
    {
        const std::vector<RespCommand>& cmds = snap.connections[i].commands;
        if (cmds.empty())
        {
            continue;
        }
        EXPECT_EQ(DescribeCommands(cmds), "AUTH <pw>") << "connection #" << i;
    }

    const std::string log = CapturedLog();
    EXPECT_TRUE(Contains(log, "WRONGPASS invalid username-password pair")) << MaskPassword(log);
    EXPECT_TRUE(Contains(log, "metric=zone_redis_auth_failed")) << MaskPassword(log);
    EXPECT_FALSE(Contains(log, "Redis AUTH ok to"));
    EXPECT_FALSE(Contains(log, kTestPassword)) << "password leaked into the log";
}

// ---------------------------------------------------------------------------
// 11. 存活探测:对端不回任何应答、也不断开(模拟代理所在机器整机消失)。PING 截止后摘下连接:在途的业务命令以空应答
//     结束,服务端看到连接被关闭;截止前探测 PING 不叠加;走既有重连,新连接的第一条命令仍是 AUTH,重连回调累计 2 次;
//     打一条带 metric 的 ERROR,日志不含口令。
// ---------------------------------------------------------------------------

TEST_F(RedisManagerLiveness, SilentPeerIsDroppedAndReconnected)
{
    StartManager(kTestPassword, kReconnectIntervalSec, kLivenessProbeIntervalSec, kLivenessTimeoutSec);
    ASSERT_TRUE(WaitUntil([this] { return reconnectCalls_ >= 1 && pingReplies_ >= 1; }, kSettleTimeout))
        << "the first connection never came up";

    // 同一个 loop 任务里:让移交给业务的那条连接「消失」(此后照收命令,但什么都不回、也不断开),记下此刻它已收到的
    // 命令数,再往上发一条业务命令 —— 这条命令只能等到连接被摘下时以空应答结束。
    std::optional<std::size_t> silentIndex;
    std::size_t commandsBeforeSilence = 0;
    bool connectedBeforeSilence = false;
    RunInLoopAndWait([this, &silentIndex, &commandsBeforeSilence, &connectedBeforeSilence] {
        silentIndex = LastPromotedConnection();
        if (!silentIndex)
        {
            return;
        }
        connectedBeforeSilence = ZoneRedisConnected();
        connections_[*silentIndex].stalled = true;
        commandsBeforeSilence = connections_[*silentIndex].commands.size();
        SendBusinessCommand();
    });
    ASSERT_TRUE(silentIndex.has_value()) << "precondition: a promoted connection must exist";
    ASSERT_TRUE(connectedBeforeSilence) << "precondition: the promoted connection must be usable";

    ASSERT_TRUE(WaitUntil(
        [this] { return reconnectCalls_ >= 2 && pingReplies_ >= 2 && businessNullReplies_ >= 1; }, kSettleTimeout))
        << "the silent connection was not dropped and replaced within the deadline: " << MaskPassword(CapturedLog());
    const std::size_t silent = *silentIndex;
    EXPECT_TRUE(WaitUntil([this, silent] { return connections_[silent].down; }, kSettleTimeout))
        << "the dropped connection was never closed towards the server";

    const Snapshot snap = TakeSnapshot();
    EXPECT_EQ(snap.reconnectCalls, 2);
    EXPECT_EQ(snap.businessSendFailures, 0);
    EXPECT_EQ(snap.businessReplies, 0) << "the silent server never answers";
    EXPECT_EQ(snap.businessNullReplies, 1)
        << "the in-flight command must complete with a null reply when the connection is dropped";
    EXPECT_EQ(snap.pingSendFailures, 0);
    EXPECT_EQ(snap.malformedFrames, 0);
    EXPECT_TRUE(snap.zoneRedisConnected) << "a healthy replacement connection must be handed to the business";

    // 「消失」之后、摘下之前:恰好一条探测 PING(截止前不叠加)和那条业务命令。
    ASSERT_LT(*silentIndex, snap.connections.size());
    const std::vector<RespCommand>& silentCommands = snap.connections[*silentIndex].commands;
    ASSERT_GE(silentCommands.size(), commandsBeforeSilence);
    ASSERT_FALSE(silentCommands.empty());
    EXPECT_EQ(DescribeCommand(silentCommands.front()), "AUTH <pw>");
    std::size_t pingsAfterSilence = 0;
    std::size_t getsAfterSilence = 0;
    for (std::size_t i = commandsBeforeSilence; i < silentCommands.size(); ++i)
    {
        const std::string verb = VerbOf(silentCommands[i]);
        if (verb == "PING")
        {
            ++pingsAfterSilence;
        }
        else if (verb == "GET")
        {
            ++getsAfterSilence;
        }
    }
    EXPECT_EQ(pingsAfterSilence, 1u) << "probe PINGs must not stack while one is outstanding: "
                                     << DescribeCommands(silentCommands);
    EXPECT_EQ(getsAfterSilence, 1u) << DescribeCommands(silentCommands);

    // 之后每条发过命令的连接,第一条都是 AUTH;移交给业务的恰好一条(重连回调与探测的 PING 只出现在它上面)。
    std::size_t promoted = 0;
    for (std::size_t i = *silentIndex + 1; i < snap.connections.size(); ++i)
    {
        const std::vector<RespCommand>& cmds = snap.connections[i].commands;
        if (cmds.empty())
        {
            continue;
        }
        EXPECT_EQ(DescribeCommand(cmds.front()), "AUTH <pw>") << "connection #" << i;
        for (const RespCommand& cmd : cmds)
        {
            if (VerbOf(cmd) == "PING")
            {
                ++promoted;
                break;
            }
        }
    }
    EXPECT_EQ(promoted, 1u);

    const std::string log = CapturedLog();
    EXPECT_EQ(CountOccurrences(log, "Redis liveness probe timed out on"), 1u) << MaskPassword(log);
    EXPECT_TRUE(Contains(log, "metric=zone_redis_liveness_timeout")) << MaskPassword(log);
    EXPECT_TRUE(Contains(log, "Attempting Redis reconnect")) << MaskPassword(log);
    EXPECT_EQ(CountOccurrences(log, "Redis AUTH ok to"), 2u) << MaskPassword(log);
    EXPECT_FALSE(Contains(log, "Redis AUTH rejected by"));
    EXPECT_FALSE(Contains(log, kTestPassword)) << "password leaked into the log";
}

// ---------------------------------------------------------------------------
// 12. 健康连接:探测按间隔周期发 PING,应答及时,观察窗口(截止的三倍以上)内从不误摘。
// ---------------------------------------------------------------------------

TEST_F(RedisManagerLiveness, HealthyConnectionIsProbedButNeverDropped)
{
    StartManager("", kLongReconnectIntervalSec, kLivenessProbeIntervalSec, kLivenessTimeoutSec);
    ASSERT_TRUE(WaitUntil([this] { return pingReplies_ >= 1; }, kSettleTimeout))
        << "the PING sent from the reconnect callback never got a reply";
    // 重连回调的那 1 条 PING 之外至少再看到 3 条:探测确实在周期地发。
    ASSERT_TRUE(WaitUntil(
        [this] { return connections_.size() == 1 && connections_[0].commands.size() >= 4; }, kSettleTimeout))
        << "the liveness probe is not sending periodic PINGs";
    EXPECT_TRUE(HoldsFor(
        [this] {
            return reconnectCalls_ == 1 && connections_.size() == 1 && serverDowns_ == 0 && ZoneRedisConnected();
        },
        kLivenessQuietWindow))
        << "a healthy connection must never be dropped by the liveness probe: " << CapturedLog();

    const Snapshot snap = TakeSnapshot();
    EXPECT_EQ(snap.malformedFrames, 0);
    ASSERT_EQ(snap.connections.size(), 1u);
    for (const RespCommand& cmd : snap.connections[0].commands)
    {
        EXPECT_EQ(VerbOf(cmd), "PING") << DescribeCommands(snap.connections[0].commands);
    }
    const std::string log = CapturedLog();
    EXPECT_FALSE(Contains(log, "Redis liveness probe timed out")) << log;
    EXPECT_FALSE(Contains(log, "Attempting Redis reconnect")) << log;
}

// ---------------------------------------------------------------------------
// 13. 应答慢但在截止内(每条 PONG 推迟截止的一半才回,跨过好几跳检查):截止从 PING 发出时算,不误摘,探测照常继续。
// ---------------------------------------------------------------------------

TEST_F(RedisManagerLiveness, SlowRepliesWithinTimeoutAreNotDropped)
{
    SetPingReplyDelay(kLivenessTimeoutSec / 2);
    StartManager("", kLongReconnectIntervalSec, kLivenessProbeIntervalSec, kLivenessTimeoutSec);
    ASSERT_TRUE(WaitUntil([this] { return pingReplies_ >= 1; }, kSettleTimeout))
        << "the delayed reply to the reconnect callback's PING never arrived";
    // 至少两条探测 PING:第二条只能在第一条拿到(推迟的)应答之后才发,说明慢应答被正确地算作「活着」。
    ASSERT_TRUE(WaitUntil(
        [this] { return connections_.size() == 1 && connections_[0].commands.size() >= 3; }, kSettleTimeout))
        << "the liveness probe stopped after a slow reply";
    EXPECT_TRUE(HoldsFor(
        [this] {
            return reconnectCalls_ == 1 && connections_.size() == 1 && serverDowns_ == 0 && ZoneRedisConnected();
        },
        kLivenessQuietWindow))
        << "a reply that arrives within the timeout must not get the connection dropped: " << CapturedLog();
    EXPECT_FALSE(Contains(CapturedLog(), "Redis liveness probe timed out"));
}

// ---------------------------------------------------------------------------
// 14. 握手中(AUTH 被压住不回):探测从不往握手连接上发 PING —— AUTH 仍是它唯一的命令(fail-closed 不变),
//     也不把握手期间的「没有应答」当成存活超时。
// ---------------------------------------------------------------------------

TEST_F(RedisManagerLiveness, HandshakeConnectionIsNeverProbed)
{
    ConfigureServer(AuthReplyMode::kSwallow, /*disconnectAfterFirstAuthOk=*/false);
    // 长重连间隔:AUTH 截止在整个用例期间不会到点,握手停在「AUTH 已发、没有应答」。
    StartManager(kTestPassword, kLongReconnectIntervalSec, kLivenessProbeIntervalSec, kLivenessTimeoutSec);
    ASSERT_TRUE(WaitUntil(
        [this] { return connections_.size() == 1 && !connections_[0].commands.empty(); }, kSettleTimeout))
        << "AUTH never reached the fake server";
    EXPECT_TRUE(HoldsFor(
        [this] { return connections_.size() == 1 && connections_[0].commands.size() == 1 && reconnectCalls_ == 0; },
        kLivenessQuietWindow))
        << "nothing but AUTH may be sent on a handshake connection, and the probe must not rebuild it";

    const Snapshot snap = TakeSnapshot();
    ASSERT_EQ(snap.connections.size(), 1u);
    EXPECT_EQ(DescribeCommands(snap.connections[0].commands), "AUTH <pw>");
    EXPECT_TRUE(snap.zoneRedisPresent);
    EXPECT_FALSE(snap.zoneRedisConnected);
    const std::string log = CapturedLog();
    EXPECT_FALSE(Contains(log, "Redis liveness")) << MaskPassword(log);
    EXPECT_FALSE(Contains(log, kTestPassword)) << "password leaked into the log";
}

// ---------------------------------------------------------------------------
// 15. 探测间隔 / 截止非正数:各打一条 WARN 并回落到默认值(5s / 15s),连接照常建立。
// ---------------------------------------------------------------------------

TEST_F(RedisManagerLiveness, NonPositiveLivenessSettingsFallBackToDefaults)
{
    StartManager("", kLongReconnectIntervalSec, 0.0, -1.0);
    // WARN 在 Connect 里同步写出,StartManager 返回时已在截获的日志里。
    const std::string log = CapturedLog();
    EXPECT_TRUE(Contains(log, "Redis liveness probe interval 0s is not positive, using 5s")) << log;
    EXPECT_TRUE(Contains(log, "Redis liveness timeout -1s is not positive, using 15s")) << log;
    ASSERT_TRUE(WaitUntil([this] { return pingReplies_ >= 1; }, kSettleTimeout))
        << "the connection did not come up with the fallback settings";

    const Snapshot snap = TakeSnapshot();
    EXPECT_EQ(snap.reconnectCalls, 1);
    EXPECT_TRUE(snap.zoneRedisConnected);
}
