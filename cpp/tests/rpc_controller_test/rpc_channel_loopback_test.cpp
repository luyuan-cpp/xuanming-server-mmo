// rpc_channel_loopback_test —— RpcChannel 的真实回环测试(docs/design/muduo-rpc-channel.md §4、§5、§6、§15 B)。
//
// 被锁定的契约:
//   * 客户端:done 恰好一次,且永不在 CallMethod 返回之前执行(本地失败也经 queueInLoop 以 kNotSent 投递);
//     回包按调用号 id 认领(同 message_id 并发、倒序回包各得其所);超时 / 断连 / 通道销毁 / Close 各以对应状态失败恰好一次;
//     迟到、未知 id、id=0 的回包丢弃并计数;对端 RPC_ERROR 按错误码映射到 RpcStatus;
//     超时 / 排队失败的回调里可重入 CallMethod、可销毁 owner;channel 在 loop 之外释放时不碰 loop(唯一的「不调用」例外)。
//   * 服务端:id≠0 的请求恰好回一帧(Empty / 0 字节 / handler 早返回也回),派发前的拒绝回显 id;SetFailed → HANDLER_FAILED;
//     id=0 永不回包(同 zone 停服一次切换,没有旧规则兼容分支);RpcDoneCallback 的接管、重复完成、丢弃兜底、
//     「连接已断」与「在 loop 之外释放」。
//
// 三种端点,全部跑在同一个 EventLoopThread 上:
//   * server_:原始 TcpServer。kRecorder 模式只记录收到的帧、由测试手工回包(观察被测客户端);
//     kChannel 模式与 muduo::net::RpcServer::onConnection 同形 —— 每条连接一个 RpcChannel,由连接的 context 持有,
//     消息回调只绑 weak —— 并装上测试服务(被测服务端)。不直接用 RpcServer:这里要自定服务表(NO_METHOD 用例)
//     并拿到 server 侧连接与 channel 的 weak 来观察 DOWN;RpcServer 本身在 rpc_chain_test.cpp 里用真身测。
//   * client_:真实的 RpcClient(被测客户端;出站每次连上新建一个 RpcChannel)。
//   * probe_:原始 TcpClient + 一个客户端侧 RpcChannel,另把收到的字节旁路给记录器(观察被测服务端到底回了哪些帧)。
//
// 判据只用事件与计数,不用时间窗(AGENTS §11.4):等待一律以 5s 作活性上限;超时由 SweepTimeouts(合成时刻)驱动,
// 用例超时设 1h,真实的 100ms tick 永远不会误触发。RpcChannelStats 是进程级计数,一律比较前后差值。
// 「没有回包」用屏障证明:同一连接 FIFO,紧接着发一个 id≠0 的请求,它的回包之前出现的帧就是全部回包。
//
// 与 rpc_controller_test.cpp 同一工程、同一个 main;链接桩(InitPlayerService /
// InitServiceHandler)由 rpc_client_lifecycle_test.cpp 定义,这里不要重复定义。

#include <gtest/gtest.h>

#ifdef _WIN32
#include <winsock2.h>
#include <ws2tcpip.h>
#pragma comment(lib, "ws2_32.lib")
#endif

#include <algorithm>
#include <atomic>
#include <chrono>
#include <condition_variable>
#include <cstddef>
#include <cstdint>
#include <functional>
#include <memory>
#include <mutex>
#include <optional>
#include <set>
#include <sstream>
#include <string>
#include <thread>
#include <utility>
#include <vector>

#include <boost/any.hpp>

#include "muduo/base/CountDownLatch.h"
#include "muduo/base/Timestamp.h"
#include "muduo/net/Buffer.h"
#include "muduo/net/EventLoop.h"
#include "muduo/net/EventLoopThread.h"
#include "muduo/net/InetAddress.h"
#include "muduo/net/TcpClient.h"
#include "muduo/net/TcpConnection.h"
#include "muduo/net/TcpServer.h"

#include "network/rpc_channel.h"
#include "network/rpc_channel_stats.h"
#include "network/rpc_client.h"
#include "network/rpc_controller.h"
#include "network/rpc_done_callback.h"
#include "network/rpc_status.h"
#include "proto/common/base/empty.pb.h"
#include "proto/common/base/message.pb.h"
#include "proto/common/base/rpc_message.pb.h"
#include "proto/scene/scene.pb.h"
#include "rpc/service_metadata/gate_service_service_metadata.h"
#include "rpc/service_metadata/rpc_event_registry.h"
#include "rpc/service_metadata/scene_admin_service_metadata.h"
#include "rpc/service_metadata/scene_service_metadata.h"

using namespace std::chrono_literals;

namespace
{

// ---------------------------------------------------------------------------
// 全局:消息号注册表
// ---------------------------------------------------------------------------

// 节点启动时由 Node 调 InitMessageInfo() 填充 gRpcMethodRegistry;单测进程里没有 Node,由这个全局 Environment
// 在任何用例之前填充一次,全进程共用(rpc_chain_test.cpp 也依赖它)。InitMessageInfo 只写注册表、不碰线程状态。
class RpcRegistryEnvironment final : public ::testing::Environment
{
public:
    void SetUp() override { InitMessageInfo(); }
};

// gtest 推荐的写法:在 main 之前(静态初始化期)登记,gtest 接管 Environment 的所有权。
[[maybe_unused]] const bool kRpcRegistryEnvironmentRegistered =
    (::testing::AddGlobalTestEnvironment(new RpcRegistryEnvironment), true);

// ---------------------------------------------------------------------------
// 常量
// ---------------------------------------------------------------------------

constexpr auto kLiveness = 5s;   // 只作活性上限,不作判据
// 用例超时:真实的超时 tick(100ms 一次)在用例寿命内永远不会让它到期,超时一律由 SweepTimeouts 的合成时刻驱动。
constexpr std::chrono::milliseconds kLongTimeout = std::chrono::hours(1);

// 用到的 Scene 方法,各自对应测试服务里的一种 handler 写法(见 LoopbackSceneService)。
constexpr uint32_t kProcessId = SceneProcessClientPlayerMessageMessageId;   // 同步回显
constexpr uint32_t kEnterId = ScenePlayerEnterGameNodeMessageId;            // 响应是 Empty
constexpr uint32_t kCreateId = SceneCreateSceneMessageId;                   // config_id == 0 时早返回
constexpr uint32_t kCancelId = SceneCancelBattlePrepareMessageId;           // controller->SetFailed
constexpr uint32_t kPrepareId = ScenePrepareBattleMessageId;                // RpcDoneCallback::Adopt
constexpr uint32_t kDestroyId = SceneDestroySceneMessageId;                 // 同步 done->Run()
constexpr uint32_t kHandshakeId = SceneNodeHandshakeMessageId;              // 不覆写:生成的默认实现 SetFailed + done->Run()

// 手工构造请求帧用的调用号段:远离 probe 自己那个 channel 从 1 起的号,不会被它误认领。
constexpr uint64_t kCraftedIdBase = 1'000'000;
// 屏障请求的 session_id(只用于诊断)。
constexpr uint32_t kBarrierSession = 4242;
// PrepareBattleRequest.battle_id 取这个值:handler 接管后立即放手(不存副本)。
constexpr uint64_t kDropAdoptedReply = 0xD0D0;
// DestroySceneRequest.scene_id 取这个值:handler 同步调两次 done->Run()。
constexpr uint64_t kRunDoneTwice = 0x2222;
// 截断的 length-delimited 字段:field 1、声明长度 5、实际只有 2 字节 —— 任何以 field 1 为子消息的类型都解析失败。
// 拆成两段字面量:"\x05ab" 会被当成一个十六进制转义。
const std::string kTruncatedBody("\x0a\x05" "ab", 4);

uint64_t Load(const std::atomic<uint64_t>& counter)
{
    return counter.load(std::memory_order_relaxed);
}

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

// 注册表里第一个不可派发的号(空槽);没有空槽时返回 nullopt。以前空槽会拿 nullptr 构造 std::string。
std::optional<uint32_t> FindEmptyRegistrySlot()
{
    for (uint32_t messageId = 0; messageId < kMaxRpcMethodCount; ++messageId)
    {
        if (!IsDispatchableMessageId(messageId))
        {
            return messageId;
        }
    }
    return std::nullopt;
}

// ---------------------------------------------------------------------------
// 帧与消息
// ---------------------------------------------------------------------------

GameRpcMessage MakeFrame(GameMessageType type, uint64_t id, uint32_t messageId)
{
    GameRpcMessage frame;
    frame.set_type(type);
    frame.set_id(id);
    frame.set_message_id(messageId);
    return frame;
}

GameRpcMessage MakeRawRequestFrame(uint64_t id, uint32_t messageId, std::string body)
{
    GameRpcMessage frame = MakeFrame(REQUEST, id, messageId);
    frame.set_request(std::move(body));
    return frame;
}

GameRpcMessage MakeRequestFrame(uint64_t id, uint32_t messageId, const ProtobufMessage& body)
{
    return MakeRawRequestFrame(id, messageId, body.SerializeAsString());
}

GameRpcMessage MakeRawResponseFrame(uint64_t id, uint32_t messageId, std::string body)
{
    GameRpcMessage frame = MakeFrame(RESPONSE, id, messageId);
    frame.set_response(std::move(body));
    return frame;
}

GameRpcMessage MakeResponseFrame(uint64_t id, uint32_t messageId, const ProtobufMessage& body)
{
    return MakeRawResponseFrame(id, messageId, body.SerializeAsString());
}

GameRpcMessage MakeErrorFrame(uint64_t id, uint32_t messageId, GameErrorCode error)
{
    GameRpcMessage frame = MakeFrame(RPC_ERROR, id, messageId);
    frame.set_error(error);
    return frame;
}

// 按线上格式(size | "RPC0" | payload | adler32)编进 Buffer,复用生产 codec。
void EncodeFrame(const GameRpcMessage& frame, muduo::net::Buffer* out)
{
    RpcCodec encoder([](const TcpConnectionPtr&, const RpcMessagePtr&, muduo::Timestamp) {});
    encoder.fillEmptyBuffer(out, frame);
}

std::string Describe(const GameRpcMessage& frame)
{
    std::ostringstream out;
    out << "{type=" << static_cast<int>(frame.type()) << " id=" << frame.id() << " message_id=" << frame.message_id()
        << " error=" << static_cast<int>(frame.error()) << " response_bytes=" << frame.response().size() << "}";
    return out.str();
}

std::string Describe(const std::vector<GameRpcMessage>& frames)
{
    std::string out;
    for (const auto& frame : frames)
    {
        out += Describe(frame);
    }
    return out.empty() ? std::string("<none>") : out;
}

ProcessClientPlayerMessageRequest ProcessRequest(uint32_t sessionId)
{
    ProcessClientPlayerMessageRequest request;
    request.set_session_id(sessionId);
    request.mutable_message_content()->set_id(sessionId);
    return request;
}

ProcessClientPlayerMessageResponse ProcessResponse(uint32_t sessionId)
{
    ProcessClientPlayerMessageResponse response;
    response.set_session_id(sessionId);
    return response;
}

uint32_t RequestSessionOf(const GameRpcMessage& frame)
{
    ProcessClientPlayerMessageRequest request;
    EXPECT_TRUE(request.ParseFromString(frame.request())) << Describe(frame);
    return request.session_id();
}

// ---------------------------------------------------------------------------
// 客户端完成记录
// ---------------------------------------------------------------------------

struct Completion
{
    std::string tag;
    RpcStatus status;
    bool hasResponse{false};
    std::string responseBytes;          // 成功时响应的序列化,测试线程按类型解析
    bool callMethodReturned{false};     // done 执行时发起方的 CallMethod 是否已经返回
};

// done 在 loop 线程上写,测试线程等待并读取(锁 + 条件变量)。
class CallLog
{
public:
    void Push(Completion completion)
    {
        {
            std::lock_guard<std::mutex> lock(mutex_);
            entries_.push_back(std::move(completion));
        }
        cv_.notify_all();
    }

    // 往同一序列里插一个非调用的标记(用于断言 functor 与回调的先后)。
    void Mark(std::string tag)
    {
        Completion marker;
        marker.tag = std::move(tag);
        marker.callMethodReturned = true;
        Push(std::move(marker));
    }

    bool WaitForCount(std::size_t count) const
    {
        std::unique_lock<std::mutex> lock(mutex_);
        return cv_.wait_for(lock, kLiveness, [&] { return entries_.size() >= count; });
    }

    std::size_t Count() const
    {
        std::lock_guard<std::mutex> lock(mutex_);
        return entries_.size();
    }

    std::size_t CountOf(const std::string& tag) const
    {
        std::lock_guard<std::mutex> lock(mutex_);
        return static_cast<std::size_t>(
            std::count_if(entries_.begin(), entries_.end(), [&](const Completion& c) { return c.tag == tag; }));
    }

    std::vector<Completion> Entries() const
    {
        std::lock_guard<std::mutex> lock(mutex_);
        return entries_;
    }

    std::optional<Completion> Find(const std::string& tag) const
    {
        std::lock_guard<std::mutex> lock(mutex_);
        for (const auto& entry : entries_)
        {
            if (entry.tag == tag)
            {
                return entry;
            }
        }
        return std::nullopt;
    }

private:
    mutable std::mutex mutex_;
    mutable std::condition_variable cv_;
    std::vector<Completion> entries_;
};

// 一个记录进 log 的 done。returned 由发起方在 CallMethod 返回之后置 true(loop 线程上读写)。
ClientDoneCallback Recorder(std::shared_ptr<CallLog> log, std::string tag, std::shared_ptr<bool> returned)
{
    return [log = std::move(log), tag = std::move(tag), returned = std::move(returned)](
               const RpcStatus& status, const MessagePtr& response)
    {
        Completion completion;
        completion.tag = tag;
        completion.status = status;
        completion.hasResponse = response != nullptr;
        if (response)
        {
            completion.responseBytes = response->SerializeAsString();
        }
        completion.callMethodReturned = *returned;
        log->Push(std::move(completion));
    };
}

template <typename Response>
auto TypedRecorder(std::shared_ptr<CallLog> log, std::string tag)
{
    return [log = std::move(log), tag = std::move(tag)](const RpcStatus& status, const std::shared_ptr<Response>& response)
    {
        Completion completion;
        completion.tag = tag;
        completion.status = status;
        completion.hasResponse = response != nullptr;
        if (response)
        {
            completion.responseBytes = response->SerializeAsString();
        }
        completion.callMethodReturned = true;
        log->Push(std::move(completion));
    };
}

// 只在 loop 线程调用:发起一次需要应答的调用,返回「CallMethod 返回那一刻,这次调用的 done 已执行的次数」(契约:恒为 0)。
// Caller 是 RpcClient 或 RpcChannel。
template <typename Caller>
std::size_t Issue(Caller& caller, const std::shared_ptr<CallLog>& log, const std::string& tag, uint32_t messageId,
                  const ProtobufMessage& request, std::chrono::milliseconds timeout = kLongTimeout)
{
    auto returned = std::make_shared<bool>(false);
    const std::size_t before = log->CountOf(tag);
    caller.CallMethod(messageId, request, Recorder(log, tag, returned), RpcCallOptions{timeout});
    *returned = true;
    return log->CountOf(tag) - before;
}

// 方法 tag 的测试内替身(生成器的 <KeyName>Rpc 尚未产出时,形状与设计 §10 一致)。
struct TestProcessClientPlayerMessageRpc
{
    static constexpr uint32_t kMessageId = kProcessId;
    using Request = ::ProcessClientPlayerMessageRequest;
    using Response = ::ProcessClientPlayerMessageResponse;
};

// 响应类型与注册表不符的 tag:typed CallMethod 必须在本地以 kNotSent 拒绝。
struct TestMismatchedResponseRpc
{
    static constexpr uint32_t kMessageId = kProcessId;
    using Request = ::ProcessClientPlayerMessageRequest;
    using Response = ::CreateSceneResponse;
};

// ---------------------------------------------------------------------------
// 原始帧记录器
// ---------------------------------------------------------------------------

// 记录从对端收到的 RPC0 帧(RpcCodec 解码)。帧在 loop 线程上写入,测试线程等待并读取。
class FrameRecorder
{
public:
    FrameRecorder()
        : codec_([this](const TcpConnectionPtr&, const RpcMessagePtr& frame, muduo::Timestamp) { Push(*frame); })
    {
    }

    FrameRecorder(const FrameRecorder&) = delete;
    FrameRecorder& operator=(const FrameRecorder&) = delete;

    // loop 线程:作为连接的消息回调,直接消费输入。
    void OnMessage(const TcpConnectionPtr& conn, muduo::net::Buffer* buffer, muduo::Timestamp receiveTime)
    {
        codec_.onMessage(conn, buffer, receiveTime);
    }

    // loop 线程:旁路记录,不消费 buffer(字节拷进自持缓冲,半帧留到下次)。
    void Tee(const TcpConnectionPtr& conn, const muduo::net::Buffer& buffer, muduo::Timestamp receiveTime)
    {
        teeBuffer_.append(buffer.peek(), buffer.readableBytes());
        codec_.onMessage(conn, &teeBuffer_, receiveTime);
    }

    // loop 线程:按线上格式发一帧。
    void Send(const TcpConnectionPtr& conn, const GameRpcMessage& frame) { codec_.send(conn, frame); }

    std::size_t Count() const
    {
        std::lock_guard<std::mutex> lock(mutex_);
        return frames_.size();
    }

    std::vector<GameRpcMessage> Since(std::size_t from) const
    {
        std::lock_guard<std::mutex> lock(mutex_);
        if (from >= frames_.size())
        {
            return {};
        }
        return std::vector<GameRpcMessage>(frames_.begin() + static_cast<std::ptrdiff_t>(from), frames_.end());
    }

    bool WaitForCount(std::size_t count) const
    {
        std::unique_lock<std::mutex> lock(mutex_);
        return cv_.wait_for(lock, kLiveness, [&] { return frames_.size() >= count; });
    }

    // pred(const std::vector<GameRpcMessage>&) 在锁内求值。
    template <typename Pred>
    bool WaitUntil(Pred pred) const
    {
        std::unique_lock<std::mutex> lock(mutex_);
        return cv_.wait_for(lock, kLiveness, [&] { return pred(frames_); });
    }

private:
    void Push(const GameRpcMessage& frame)
    {
        {
            std::lock_guard<std::mutex> lock(mutex_);
            frames_.push_back(frame);
        }
        cv_.notify_all();
    }

    mutable std::mutex mutex_;
    mutable std::condition_variable cv_;
    std::vector<GameRpcMessage> frames_;
    muduo::net::Buffer teeBuffer_;   // 只在 loop 线程上用
    RpcCodec codec_;                 // 解码回调绑 this:codec_ 是本对象的成员,寿命相同
};

// ---------------------------------------------------------------------------
// 被测服务端装的测试服务
// ---------------------------------------------------------------------------

// 派生自生成的 ::Scene(scene.proto 的 cc_generic 服务),按方法给出几种典型的 handler 写法。
// 所有状态只在 loop 线程上读写;测试线程经 RunInLoopAndWait 读取(latch 提供 happens-before)。
class LoopbackSceneService final : public ::Scene
{
public:
    int processCalls{0};
    int enterCalls{0};
    int createCalls{0};
    int cancelCalls{0};
    int prepareCalls{0};
    int destroyCalls{0};
    // PrepareBattle 接管后暂存的回复:测试决定何时 Run / Fail / 释放。
    std::vector<RpcDoneCallback> parked;

    // 同步回显:返回即由框架回包。
    void ProcessClientPlayerMessage(::google::protobuf::RpcController*,
                                    const ::ProcessClientPlayerMessageRequest* request,
                                    ::ProcessClientPlayerMessageResponse* response,
                                    ::google::protobuf::Closure*) override
    {
        ++processCalls;
        response->set_session_id(request->session_id());
        *response->mutable_message_content() = request->message_content();
    }

    // 响应是 Empty:id≠0 时也必须回(0 字节响应)。
    void PlayerEnterGameNode(::google::protobuf::RpcController*, const ::PlayerEnterGameNodeRequest*, ::Empty*,
                             ::google::protobuf::Closure*) override
    {
        ++enterCalls;
    }

    // config_id == 0:早返回、不碰响应;否则写一个非空响应。
    void CreateScene(::google::protobuf::RpcController*, const ::CreateSceneRequest* request,
                     ::CreateSceneResponse* response, ::google::protobuf::Closure*) override
    {
        ++createCalls;
        if (request->config_id() == 0)
        {
            return;
        }
        (void)response->mutable_scene_info();   // 子消息出现即序列化为非空字节
    }

    // 显式失败:框架回 RPC_ERROR(HANDLER_FAILED)。
    void CancelBattlePrepare(::google::protobuf::RpcController* controller, const ::CancelBattlePrepareRequest*,
                             ::Empty*, ::google::protobuf::Closure*) override
    {
        ++cancelCalls;
        controller->SetFailed("cancel rejected by test");
    }

    // 接管 done:battle_id == kDropAdoptedReply 时立即放手;否则按 battle_node_id 存若干副本(至少 1 个)。
    void PrepareBattle(::google::protobuf::RpcController*, const ::PrepareBattleRequest* request,
                       ::PrepareBattleResponse*, ::google::protobuf::Closure* done) override
    {
        ++prepareCalls;
        const auto reply = RpcDoneCallback::Adopt(done);
        if (request->battle_id() == kDropAdoptedReply)
        {
            return;
        }
        const uint32_t copies = std::max<uint32_t>(1, request->battle_node_id());
        for (uint32_t i = 0; i < copies; ++i)
        {
            parked.push_back(reply);
        }
    }

    // 同步完成:handler 自己 done->Run(),框架之后不再回包;scene_id == kRunDoneTwice 时故意调两次。
    void DestroyScene(::google::protobuf::RpcController*, const ::DestroySceneRequest* request, ::Empty*,
                      ::google::protobuf::Closure* done) override
    {
        ++destroyCalls;
        done->Run();
        if (request->scene_id() == kRunDoneTwice)
        {
            done->Run();
        }
    }
};

// 记录 RpcClient 的连接观察者报 DOWN 那一刻已完成的调用数：先 OnDisconnect、重置 channel，
// 再同步通知观察者；排队的失败回调此时尚未执行。只在 loop 线程上使用。
struct DownProbe
{
    bool enabled{false};
    std::shared_ptr<CallLog> log;
    std::optional<std::size_t> completionsAtFirstDown;

    void OnEvent(const TcpConnectionPtr& conn)
    {
        if (conn && conn->connected())
        {
            return;
        }
        if (!completionsAtFirstDown)
        {
            completionsAtFirstDown = log->Count();
        }
    }
};

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

class RpcChannelLoopback : public ::testing::Test
{
protected:
    enum class ServerMode
    {
        kRecorder,   // 记录收到的帧,测试手工回包
        kChannel,    // 每条连接一个 RpcChannel + 测试服务
    };

    void SetUp() override
    {
        loop_.emplace(*loopThread_.startLoop());
        const uint16_t port = FindFreeLoopbackPort();
        ASSERT_NE(port, 0);
        serverAddr_ = muduo::net::InetAddress("127.0.0.1", port);
        ASSERT_TRUE(IsDispatchableMessageId(kProcessId))
            << "gRpcMethodRegistry is empty: the global RpcRegistryEnvironment did not run";

        RunInLoopAndWait([this, port] {
            server_ = std::make_unique<muduo::net::TcpServer>(
                &Loop(), muduo::net::InetAddress(port, /*loopbackOnly=*/true), "rpc_channel_loopback_server");
            // 回调捕获夹具 this:server_ 在 TearDown 里先于夹具销毁(AGENTS §11.7 允许的同寿命例外)。
            server_->setConnectionCallback([this](const TcpConnectionPtr& conn) { OnServerConnection(conn); });
            server_->start();   // 在 loop 线程上即时 listen,随后的 connect 不会撞拒连
        });
    }

    void TearDown() override
    {
        // 顺序同 rpc_controller_test:先拆客户端(各自连接的唯一持有者 → forceClose → 对端 DOWN),
        // 等服务端每条连接都报过 DOWN,再在 loop 线程销毁 TcpServer(它的析构函数断言 in-loop)。
        RunInLoopAndWait([this] {
            downProbe_->enabled = false;
            if (ownerBox_)
            {
                ownerBox_->reset();   // 用例中途失败时 owner 还在盒子里:也在 loop 线程上销毁
            }
            client_.reset();
            probe_.reset();
        });
        PumpLoop(3);
        EXPECT_TRUE(WaitUntil([this] { return serverDowns_.load() == serverUps_.load(); }))
            << "server side did not see DOWN for every connection: ups=" << serverUps_.load()
            << " downs=" << serverDowns_.load();
        RunInLoopAndWait([this] {
            probeChannel_.reset();
            // 暂存的回复在 loop 线程上释放:连接已断,只计 reply_channel_gone,不发任何帧。
            service_.parked.clear();
            server_.reset();
        });
        PumpLoop(2);
        // loopThread_ 析构 = quit + join;排队的 connectDestroyed 早已跑完。
    }

    muduo::net::EventLoop& Loop() { return loop_->get(); }

    template <typename F>
    void RunInLoopAndWait(F&& fn)
    {
        muduo::CountDownLatch done(1);
        Loop().runInLoop([&] {
            fn();
            done.countDown();
        });
        done.wait();
    }

    // 把 loop 转几圈,让排队的 functor(批量失败、forceCloseInLoop、connectDestroyed……)跑完。
    void PumpLoop(int rounds)
    {
        for (int i = 0; i < rounds; ++i)
        {
            RunInLoopAndWait([] {});
        }
    }

    // 在 loop 线程上反复求值 pred,成立即返回 true;5s 只是活性上限。
    template <typename Pred>
    bool WaitUntil(Pred pred)
    {
        const auto deadline = std::chrono::steady_clock::now() + kLiveness;
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
            std::this_thread::sleep_for(1ms);
        }
    }

    // loop 线程。kChannel 模式与 muduo::net::RpcServer::onConnection 同形。
    void OnServerConnection(const TcpConnectionPtr& conn)
    {
        if (conn->connected())
        {
            serverConn_ = conn;
            if (serverMode_ == ServerMode::kChannel)
            {
                RpcChannelPtr channel = RpcChannel::Create(Loop(), conn, serverServices_, serverMaintenance_);
                std::weak_ptr<RpcChannel> weakChannel = channel;
                conn->setMessageCallback(
                    [weakChannel](const TcpConnectionPtr& c, muduo::net::Buffer* buffer, muduo::Timestamp receiveTime)
                    {
                        if (const auto locked = weakChannel.lock())
                        {
                            locked->OnMessage(c, buffer, receiveTime);
                            return;
                        }
                        buffer->retrieveAll();
                    });
                conn->setContext(channel);
                serverChannel_ = channel;
            }
            else
            {
                conn->setMessageCallback(
                    [this](const TcpConnectionPtr& c, muduo::net::Buffer* buffer, muduo::Timestamp receiveTime)
                    {
                        serverRecorder_.OnMessage(c, buffer, receiveTime);
                    });
            }
            ++serverUps_;
            return;
        }
        if (const auto* channel = boost::any_cast<RpcChannelPtr>(&conn->getContext()); channel != nullptr && *channel)
        {
            (*channel)->OnDisconnect();
        }
        conn->setContext(RpcChannelPtr());
        ++serverDowns_;
    }

    void ConnectRpcClient()
    {
        RunInLoopAndWait([this] {
            client_ = std::make_shared<RpcClient>(&Loop(), serverAddr_);
            client_->setConnectionObserver([weakProbe = std::weak_ptr<DownProbe>(downProbe_)](const TcpConnectionPtr& conn) {
                if (const auto probe = weakProbe.lock(); probe && probe->enabled)
                {
                    probe->OnEvent(conn);
                }
            });
            client_->connect();
        });
        ASSERT_TRUE(WaitUntil([this] { return client_->connected() && !serverConn_.expired(); }))
            << "RpcClient never connected";
    }

    // 启用 connect 前已安装的观察者探针；TearDown 先停探针，用例中途 ASSERT 返回也不会悬空。
    void WatchClientDown(std::shared_ptr<CallLog> log)
    {
        RunInLoopAndWait([this, log = std::move(log)]() mutable {
            downProbe_->log = std::move(log);
            downProbe_->completionsAtFirstDown.reset();
            downProbe_->enabled = true;
        });
    }

    // 把 client_ 的唯一强引用移进一个盒子,回调里经 weak_ptr<盒子> 销毁它 == 业务在回调里摘掉节点实体。
    // 回调只捕获盒子的 weak:不形成 client → channel → 在途 done → 盒子 → client 的环。
    std::weak_ptr<RpcClientPtr> HandClientToOwnerBox()
    {
        RunInLoopAndWait([this] { ownerBox_ = std::make_shared<RpcClientPtr>(std::move(client_)); });
        return ownerBox_;
    }

    // 只配 kChannel 模式:被测的是服务端。
    void ConnectProbe()
    {
        RunInLoopAndWait([this] {
            probe_ = std::make_unique<muduo::net::TcpClient>(&Loop(), serverAddr_, "rpc_channel_loopback_probe");
            // 同寿命例外:probe_ 在 TearDown 里先于夹具销毁,之后的 DOWN 回调也在 TearDown 转 loop 时跑完。
            probe_->setConnectionCallback([this](const TcpConnectionPtr& conn) { OnProbeConnection(conn); });
            probe_->setMessageCallback(
                [this](const TcpConnectionPtr& conn, muduo::net::Buffer* buffer, muduo::Timestamp receiveTime)
                {
                    OnProbeMessage(conn, buffer, receiveTime);
                });
            probe_->connect();
        });
        ASSERT_TRUE(WaitUntil([this] {
            return probeChannel_ && probeChannel_->IsConnected() && !serverChannel_.expired();
        })) << "probe never connected to the channel server";
    }

    // loop 线程。probe 的客户端侧 channel 与 RpcClient 同样每条连接一个、DOWN 即丢弃。
    void OnProbeConnection(const TcpConnectionPtr& conn)
    {
        if (conn->connected())
        {
            probeChannel_ = RpcChannel::Create(Loop(), conn, RpcServiceMap{}, probeMaintenance_);
            return;
        }
        if (probeChannel_ && probeChannel_->IsBoundTo(conn))
        {
            probeChannel_->OnDisconnect();
            probeChannel_.reset();
        }
    }

    void OnProbeMessage(const TcpConnectionPtr& conn, muduo::net::Buffer* buffer, muduo::Timestamp receiveTime)
    {
        probeRecorder_.Tee(conn, *buffer, receiveTime);
        if (probeChannel_ && probeChannel_->IsBoundTo(conn))
        {
            probeChannel_->OnMessage(conn, buffer, receiveTime);
            return;
        }
        buffer->retrieveAll();
    }

    // loop 线程:kRecorder 模式下由服务端回一帧。
    void SendFromServer(const GameRpcMessage& frame)
    {
        const auto conn = serverConn_.lock();
        if (!conn)
        {
            ADD_FAILURE() << "server connection is gone";
            return;
        }
        serverRecorder_.Send(conn, frame);
    }

    // loop 线程:经 probe 连接发一帧手工构造的请求。
    void SendFromProbe(const GameRpcMessage& frame)
    {
        const TcpConnectionPtr conn = probe_ ? probe_->connection() : TcpConnectionPtr{};
        if (!conn)
        {
            ADD_FAILURE() << "probe connection is gone";
            return;
        }
        probeRecorder_.Send(conn, frame);
    }

    // 测试线程:经 probe 发一个 id≠0 的同步请求作屏障,等到它的回包,返回 probe 从 from 起、屏障回包之前收到的所有帧。
    // 同一连接 FIFO:屏障之前发出的请求若有回包,必然排在屏障回包之前 —— 「没有回包」由此得证,不靠时间窗。
    std::vector<GameRpcMessage> FramesBeforeBarrier(std::size_t from, uint64_t barrierId)
    {
        RunInLoopAndWait([this, barrierId] {
            SendFromProbe(MakeRequestFrame(barrierId, kProcessId, ProcessRequest(kBarrierSession)));
        });
        std::vector<GameRpcMessage> before;
        std::optional<GameRpcMessage> barrier;
        const bool seen = probeRecorder_.WaitUntil([&](const std::vector<GameRpcMessage>& frames) {
            for (std::size_t i = from; i < frames.size(); ++i)
            {
                if (frames[i].id() == barrierId)
                {
                    before.assign(frames.begin() + static_cast<std::ptrdiff_t>(from),
                                  frames.begin() + static_cast<std::ptrdiff_t>(i));
                    barrier = frames[i];
                    return true;
                }
            }
            return false;
        });
        EXPECT_TRUE(seen) << "barrier reply never arrived";
        if (barrier)
        {
            EXPECT_EQ(barrier->type(), RESPONSE) << Describe(*barrier);
        }
        return before;
    }

    // loopThread_ 排第一:最后析构,保证其余成员析构时 loop 仍在。
    muduo::net::EventLoopThread loopThread_;
    std::optional<std::reference_wrapper<muduo::net::EventLoop>> loop_;
    muduo::net::InetAddress serverAddr_;

    // 测试服务声明在服务器之前:server 侧 channel 的服务表借用它,它必须后析构。
    LoopbackSceneService service_;
    ServerMode serverMode_{ServerMode::kRecorder};   // 连接前设好;UP 时在 loop 线程读取
    RpcServiceMap serverServices_{{"Scene", &service_}};
    std::unique_ptr<muduo::net::TcpServer> server_;
    FrameRecorder serverRecorder_;
    // 以下两个 weak 只在 loop 线程上读写。
    std::weak_ptr<muduo::net::TcpConnection> serverConn_;
    std::weak_ptr<RpcChannel> serverChannel_;
    std::atomic<int> serverUps_{0};
    std::atomic<int> serverDowns_{0};

    RpcClientPtr client_;
    // 以下状态只在 loop 线程上读写。
    std::shared_ptr<RpcClientPtr> ownerBox_;
    std::shared_ptr<DownProbe> downProbe_{std::make_shared<DownProbe>()};

    std::unique_ptr<muduo::net::TcpClient> probe_;
    RpcChannelPtr probeChannel_;   // 只在 loop 线程上读写
    FrameRecorder probeRecorder_;
    std::shared_ptr<RpcMaintenanceState> serverMaintenance_{std::make_shared<RpcMaintenanceState>()};
    std::shared_ptr<RpcMaintenanceState> probeMaintenance_{std::make_shared<RpcMaintenanceState>()};
};

} // namespace

// ===========================================================================
// 一、往返(RpcClient → 每连接 RpcChannel + 测试服务)
// ===========================================================================

TEST_F(RpcChannelLoopback, RoundTripCompletesExactlyOnceAfterCallMethodReturned)
{
    serverMode_ = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectRpcClient());
    auto& stats = GetRpcChannelStats();
    const auto startedBefore = Load(stats.started);
    const auto okBefore = Load(stats.ok);
    auto log = std::make_shared<CallLog>();

    std::size_t synchronousRuns = 99;
    RunInLoopAndWait([&] { synchronousRuns = Issue(*client_, log, "call", kProcessId, ProcessRequest(7)); });
    EXPECT_EQ(synchronousRuns, 0u) << "done ran inside CallMethod";
    ASSERT_TRUE(log->WaitForCount(1));

    // 屏障:同一连接上再走一次往返。第一次调用若被重复完成,必然出现在屏障之前。
    RunInLoopAndWait([&] { Issue(*client_, log, "barrier", kProcessId, ProcessRequest(8)); });
    ASSERT_TRUE(log->WaitForCount(2));

    const auto entries = log->Entries();
    ASSERT_EQ(entries.size(), 2u);
    EXPECT_EQ(entries[0].tag, "call");
    EXPECT_EQ(entries[1].tag, "barrier");
    const auto& call = entries[0];
    ASSERT_TRUE(call.status.ok()) << call.status.name();
    EXPECT_TRUE(call.callMethodReturned);
    ASSERT_TRUE(call.hasResponse);
    ProcessClientPlayerMessageResponse response;
    ASSERT_TRUE(response.ParseFromString(call.responseBytes));
    EXPECT_EQ(response.session_id(), 7u);
    EXPECT_EQ(response.message_content().id(), 7u);

    EXPECT_EQ(Load(stats.started) - startedBefore, 2u);
    EXPECT_EQ(Load(stats.ok) - okBefore, 2u);
    std::size_t outstanding = 99;
    int handlerCalls = -1;
    RunInLoopAndWait([&] {
        outstanding = client_->channel() ? client_->channel()->OutstandingCount() : 99;
        handlerCalls = service_.processCalls;
    });
    EXPECT_EQ(outstanding, 0u);
    EXPECT_EQ(handlerCalls, 2);
}

TEST_F(RpcChannelLoopback, TypedCallMethodChecksTheResponseTypeAgainstTheRegistry)
{
    serverMode_ = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectRpcClient());
    auto log = std::make_shared<CallLog>();

    RunInLoopAndWait([&] {
        client_->CallMethod<TestProcessClientPlayerMessageRpc>(
            ProcessRequest(9), TypedRecorder<ProcessClientPlayerMessageResponse>(log, "typed"),
            RpcCallOptions{kLongTimeout});
        client_->CallMethod<TestMismatchedResponseRpc>(
            ProcessRequest(10), TypedRecorder<CreateSceneResponse>(log, "mismatched"), RpcCallOptions{kLongTimeout});
    });
    ASSERT_TRUE(log->WaitForCount(2));

    const auto typed = log->Find("typed");
    ASSERT_TRUE(typed.has_value());
    ASSERT_TRUE(typed->status.ok()) << typed->status.name();
    ProcessClientPlayerMessageResponse response;
    ASSERT_TRUE(response.ParseFromString(typed->responseBytes));
    EXPECT_EQ(response.session_id(), 9u);

    const auto mismatched = log->Find("mismatched");
    ASSERT_TRUE(mismatched.has_value());
    EXPECT_EQ(mismatched->status.code(), RpcStatusCode::kNotSent) << mismatched->status.name();
    EXPECT_FALSE(mismatched->hasResponse);

    int handlerCalls = -1;
    RunInLoopAndWait([&] { handlerCalls = service_.processCalls; });
    EXPECT_EQ(handlerCalls, 1) << "the mismatched call must not reach the wire";
}

TEST_F(RpcChannelLoopback, EmptyResponseAndEarlyReturnStillReply)
{
    serverMode_ = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectRpcClient());
    auto log = std::make_shared<CallLog>();

    RunInLoopAndWait([&] {
        Issue(*client_, log, "empty", kEnterId, PlayerEnterGameNodeRequest{});
        Issue(*client_, log, "early_return", kCreateId, CreateSceneRequest{});
        CreateSceneRequest filled;
        filled.set_config_id(1);
        Issue(*client_, log, "filled", kCreateId, filled);
    });
    ASSERT_TRUE(log->WaitForCount(3));

    for (const char* tag : {"empty", "early_return", "filled"})
    {
        const auto entry = log->Find(tag);
        ASSERT_TRUE(entry.has_value()) << tag;
        EXPECT_TRUE(entry->status.ok()) << tag << ": " << entry->status.name();
        EXPECT_TRUE(entry->hasResponse) << tag << ": response != nullptr ⇔ status.ok()";
        EXPECT_EQ(log->CountOf(tag), 1u) << tag;
    }
    EXPECT_TRUE(log->Find("empty")->responseBytes.empty()) << "Empty 响应是 0 字节,但仍然要回";
    CreateSceneResponse early;
    ASSERT_TRUE(early.ParseFromString(log->Find("early_return")->responseBytes));
    EXPECT_FALSE(early.has_scene_info());
    CreateSceneResponse filledResponse;
    ASSERT_TRUE(filledResponse.ParseFromString(log->Find("filled")->responseBytes));
    EXPECT_TRUE(filledResponse.has_scene_info());
}

TEST_F(RpcChannelLoopback, HandlerFailureMapsToRemoteFailed)
{
    serverMode_ = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectRpcClient());
    auto log = std::make_shared<CallLog>();
    const auto failedBefore = Load(GetRpcChannelStats().remoteFailed);

    RunInLoopAndWait([&] {
        Issue(*client_, log, "set_failed", kCancelId, CancelBattlePrepareRequest{});
        Issue(*client_, log, "unimplemented", kHandshakeId, NodeHandshakeRequest{});
    });
    ASSERT_TRUE(log->WaitForCount(2));

    for (const char* tag : {"set_failed", "unimplemented"})
    {
        const auto entry = log->Find(tag);
        ASSERT_TRUE(entry.has_value()) << tag;
        EXPECT_EQ(entry->status.code(), RpcStatusCode::kRemoteFailed) << tag << ": " << entry->status.name();
        EXPECT_EQ(entry->status.remoteError(), HANDLER_FAILED) << tag;
        EXPECT_FALSE(entry->status.DefinitelyNotExecuted()) << tag << ": handler 已经运行过";
        EXPECT_FALSE(entry->status.OutcomeUnknown()) << tag;
        EXPECT_FALSE(entry->hasResponse) << tag;
    }
    EXPECT_EQ(Load(GetRpcChannelStats().remoteFailed) - failedBefore, 2u);
}

// 接管后未完成就放手:调用方立即以 kReplyDropped 失败(用例超时 1h 且从不扫描超时,只能来自 REPLY_DROPPED)。
// 多个副本共享一个完成标记:最后一个副本释放时只兜底一次。
TEST_F(RpcChannelLoopback, AdoptedReplyReleasedWithoutRunFailsTheCallerWithReplyDropped)
{
    serverMode_ = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectRpcClient());
    auto& stats = GetRpcChannelStats();
    const auto lateBefore = Load(stats.lateReply);
    auto log = std::make_shared<CallLog>();

    RunInLoopAndWait([&] {
        PrepareBattleRequest drop;
        drop.set_battle_id(kDropAdoptedReply);
        Issue(*client_, log, "dropped_in_handler", kPrepareId, drop);
        PrepareBattleRequest park;
        park.set_battle_node_id(2);   // 存两个副本
        Issue(*client_, log, "released_later", kPrepareId, park);
    });
    ASSERT_TRUE(log->WaitForCount(1));
    ASSERT_TRUE(WaitUntil([this] { return service_.parked.size() == 2; }));
    EXPECT_EQ(log->CountOf("released_later"), 0u) << "still parked: must not complete yet";

    RunInLoopAndWait([this] { service_.parked.clear(); });
    ASSERT_TRUE(log->WaitForCount(2));
    // 屏障:若两个副本各回了一次 REPLY_DROPPED,第二帧会作为迟到回包出现在屏障之前。
    RunInLoopAndWait([&] { Issue(*client_, log, "barrier", kProcessId, ProcessRequest(1)); });
    ASSERT_TRUE(log->WaitForCount(3));

    for (const char* tag : {"dropped_in_handler", "released_later"})
    {
        const auto entry = log->Find(tag);
        ASSERT_TRUE(entry.has_value()) << tag;
        EXPECT_EQ(entry->status.code(), RpcStatusCode::kReplyDropped) << tag << ": " << entry->status.name();
        EXPECT_EQ(entry->status.remoteError(), REPLY_DROPPED) << tag;
        EXPECT_TRUE(entry->status.OutcomeUnknown()) << tag;
        EXPECT_EQ(log->CountOf(tag), 1u) << tag;
    }
    EXPECT_EQ(Load(stats.lateReply) - lateBefore, 0u) << "a shared reply state must fall back only once";
}

TEST_F(RpcChannelLoopback, ReentrantCallMethodFromResponseDone)
{
    serverMode_ = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectRpcClient());
    auto log = std::make_shared<CallLog>();

    RunInLoopAndWait([&] {
        // done 只捕获值与 weak(契约 b):weak_ptr<RpcClient>,执行时重新 lock。
        std::weak_ptr<RpcClient> weakClient = client_;
        client_->CallMethod(
            kProcessId, ProcessRequest(1),
            [log, weakClient](const RpcStatus& status, const MessagePtr&)
            {
                Completion first;
                first.tag = "first";
                first.status = status;
                first.callMethodReturned = true;
                log->Push(std::move(first));
                // 在 RESPONSE 的同步完成栈上重入 CallMethod:新调用号不在正在处理的这一帧里。
                if (const auto client = weakClient.lock())
                {
                    Issue(*client, log, "second", kProcessId, ProcessRequest(2));
                }
            },
            RpcCallOptions{kLongTimeout});
    });
    ASSERT_TRUE(log->WaitForCount(2));

    const auto entries = log->Entries();
    ASSERT_EQ(entries.size(), 2u);
    EXPECT_EQ(entries[0].tag, "first");
    EXPECT_TRUE(entries[0].status.ok()) << entries[0].status.name();
    EXPECT_EQ(entries[1].tag, "second");
    EXPECT_TRUE(entries[1].status.ok()) << entries[1].status.name();
    EXPECT_TRUE(entries[1].callMethodReturned);
}

// ===========================================================================
// 二、被测客户端(RpcClient → 记录帧的服务端,手工回包)
// ===========================================================================

TEST_F(RpcChannelLoopback, ConcurrentCallsWithTheSameMessageIdAreMatchedByCallId)
{
    ASSERT_NO_FATAL_FAILURE(ConnectRpcClient());
    auto log = std::make_shared<CallLog>();

    RunInLoopAndWait([&] {
        Issue(*client_, log, "first", kProcessId, ProcessRequest(1));
        Issue(*client_, log, "second", kProcessId, ProcessRequest(2));
    });
    ASSERT_TRUE(serverRecorder_.WaitForCount(2));
    const auto frames = serverRecorder_.Since(0);
    ASSERT_EQ(frames.size(), 2u);
    for (const auto& frame : frames)
    {
        EXPECT_EQ(frame.type(), REQUEST) << Describe(frame);
        EXPECT_EQ(frame.message_id(), kProcessId) << Describe(frame);
        EXPECT_NE(frame.id(), kNoInflightCallId) << "a call that expects a reply must carry a non-zero id";
    }
    EXPECT_NE(frames[0].id(), frames[1].id());
    ASSERT_EQ(RequestSessionOf(frames[0]), 1u);
    ASSERT_EQ(RequestSessionOf(frames[1]), 2u);

    // 倒序回包;响应里的 session 与各自请求对应 —— 若按 message_id 或到达顺序认领,"first" 会拿到 2。
    RunInLoopAndWait([&] {
        SendFromServer(MakeResponseFrame(frames[1].id(), kProcessId, ProcessResponse(2)));
        SendFromServer(MakeResponseFrame(frames[0].id(), kProcessId, ProcessResponse(1)));
    });
    ASSERT_TRUE(log->WaitForCount(2));

    const auto entries = log->Entries();
    ASSERT_EQ(entries.size(), 2u);
    EXPECT_EQ(entries[0].tag, "second");
    EXPECT_EQ(entries[1].tag, "first");
    for (const auto& entry : entries)
    {
        ASSERT_TRUE(entry.status.ok()) << entry.tag << ": " << entry.status.name();
        ProcessClientPlayerMessageResponse response;
        ASSERT_TRUE(response.ParseFromString(entry.responseBytes));
        EXPECT_EQ(response.session_id(), entry.tag == "first" ? 1u : 2u) << entry.tag << " got another call's reply";
    }
}

TEST_F(RpcChannelLoopback, TimeoutCompletesOnceAndTheLateReplyIsDropped)
{
    ASSERT_NO_FATAL_FAILURE(ConnectRpcClient());
    auto& stats = GetRpcChannelStats();
    const auto timeoutBefore = Load(stats.timeout);
    const auto lateBefore = Load(stats.lateReply);
    auto log = std::make_shared<CallLog>();

    std::size_t afterEarlySweep = 99;
    std::size_t outstandingAfterEarlySweep = 99;
    std::size_t afterSweep = 99;
    std::size_t outstandingAfterSweep = 99;
    RunInLoopAndWait([&] {
        const RpcChannelPtr channel = client_->channel();
        if (!channel)
        {
            ADD_FAILURE() << "client has no channel";
            return;
        }
        const auto t0 = RpcChannel::Clock::now();
        Issue(*client_, log, "slow", kProcessId, ProcessRequest(1), kLongTimeout);
        const auto t1 = RpcChannel::Clock::now();
        // 真实截止时刻必在 [t0+T, t1+T]:t0+T-1ns 必不过期,t1+T 必过期。不依赖墙钟。
        channel->SweepTimeouts(t0 + kLongTimeout - std::chrono::nanoseconds(1));
        afterEarlySweep = log->Count();
        outstandingAfterEarlySweep = channel->OutstandingCount();
        channel->SweepTimeouts(t1 + kLongTimeout);
        afterSweep = log->Count();   // 超时在扫描栈上同步完成(设计 §4.4)
        outstandingAfterSweep = channel->OutstandingCount();
    });
    EXPECT_EQ(afterEarlySweep, 0u);
    EXPECT_EQ(outstandingAfterEarlySweep, 1u);
    EXPECT_EQ(afterSweep, 1u);
    EXPECT_EQ(outstandingAfterSweep, 0u);

    const auto slow = log->Find("slow");
    ASSERT_TRUE(slow.has_value());
    EXPECT_EQ(slow->status.code(), RpcStatusCode::kTimeout) << slow->status.name();
    EXPECT_TRUE(slow->status.OutcomeUnknown());
    EXPECT_FALSE(slow->hasResponse);
    EXPECT_EQ(Load(stats.timeout) - timeoutBefore, 1u);

    // 对端此时才回包:同 id 的迟到回包必须被丢弃并计数,不得再次完成。屏障调用的回包排在它之后(同连接 FIFO)。
    ASSERT_TRUE(serverRecorder_.WaitForCount(1));
    const auto slowFrame = serverRecorder_.Since(0).front();
    RunInLoopAndWait([&] {
        SendFromServer(MakeResponseFrame(slowFrame.id(), kProcessId, ProcessResponse(1)));
        Issue(*client_, log, "barrier", kProcessId, ProcessRequest(2));
    });
    ASSERT_TRUE(serverRecorder_.WaitForCount(2));
    const auto barrierFrame = serverRecorder_.Since(1).front();
    RunInLoopAndWait([&] { SendFromServer(MakeResponseFrame(barrierFrame.id(), kProcessId, ProcessResponse(2))); });
    ASSERT_TRUE(log->WaitForCount(2));

    EXPECT_EQ(log->CountOf("slow"), 1u) << "the late reply completed the call a second time";
    EXPECT_TRUE(log->Find("barrier")->status.ok());
    EXPECT_EQ(Load(stats.lateReply) - lateBefore, 1u);
}

TEST_F(RpcChannelLoopback, UnknownIdAndId0RepliesAreDroppedAndCounted)
{
    ASSERT_NO_FATAL_FAILURE(ConnectRpcClient());
    auto& stats = GetRpcChannelStats();
    const auto lateBefore = Load(stats.lateReply);
    const auto legacyBefore = Load(stats.legacyId0Reply);
    const auto unknownTypeBefore = Load(stats.unknownType);
    auto log = std::make_shared<CallLog>();

    RunInLoopAndWait([&] { Issue(*client_, log, "real", kProcessId, ProcessRequest(1)); });
    ASSERT_TRUE(serverRecorder_.WaitForCount(1));
    const uint64_t id = serverRecorder_.Since(0).front().id();

    RunInLoopAndWait([&] {
        SendFromServer(MakeResponseFrame(id + 1000, kProcessId, ProcessResponse(0)));   // 未知 id 的 RESPONSE
        SendFromServer(MakeErrorFrame(id + 2000, kProcessId, NO_SERVICE));              // 未知 id 的 RPC_ERROR
        SendFromServer(MakeResponseFrame(0, kProcessId, ProcessResponse(0)));            // 没有调用号的回包
        SendFromServer(MakeErrorFrame(0, kProcessId, NO_SERVICE));
        // 已删除的 NODE_ROUTE(3),带着真实调用的 id:计数后丢弃,不断连,也不得认领那次调用。
        SendFromServer(MakeFrame(static_cast<GameMessageType>(3), id, kProcessId));
        SendFromServer(MakeResponseFrame(id, kProcessId, ProcessResponse(5)));          // 真正的回包
    });
    ASSERT_TRUE(log->WaitForCount(1));

    const auto real = log->Find("real");
    ASSERT_TRUE(real.has_value());
    ASSERT_TRUE(real->status.ok()) << real->status.name();
    ProcessClientPlayerMessageResponse response;
    ASSERT_TRUE(response.ParseFromString(real->responseBytes));
    EXPECT_EQ(response.session_id(), 5u);
    EXPECT_EQ(log->Count(), 1u);
    EXPECT_EQ(Load(stats.lateReply) - lateBefore, 2u);
    EXPECT_EQ(Load(stats.legacyId0Reply) - legacyBefore, 2u);
    EXPECT_EQ(Load(stats.unknownType) - unknownTypeBefore, 1u);
    bool stillConnected = false;
    RunInLoopAndWait([&] { stillConnected = client_->connected(); });
    EXPECT_TRUE(stillConnected) << "dropped frames must not tear the link down";
}

TEST_F(RpcChannelLoopback, RemoteErrorsMapToTheCallerStatus)
{
    ASSERT_NO_FATAL_FAILURE(ConnectRpcClient());
    auto log = std::make_shared<CallLog>();
    struct ExpectedMapping
    {
        std::string tag;
        GameErrorCode wireError;
        RpcStatusCode status;
        bool definitelyNotExecuted;
        bool outcomeUnknown;
    };
    const std::vector<ExpectedMapping> expected{
        {"no_service", NO_SERVICE, RpcStatusCode::kRemoteRejected, true, false},
        {"invalid_request", INVALID_REQUEST, RpcStatusCode::kRemoteRejected, true, false},
        {"handler_failed", HANDLER_FAILED, RpcStatusCode::kRemoteFailed, false, false},
        {"reply_dropped", REPLY_DROPPED, RpcStatusCode::kReplyDropped, false, true},
    };

    RunInLoopAndWait([&] {
        uint32_t session = 1;
        for (const auto& e : expected)
        {
            Issue(*client_, log, e.tag, kProcessId, ProcessRequest(session++));
        }
    });
    ASSERT_TRUE(serverRecorder_.WaitForCount(expected.size()));
    const auto frames = serverRecorder_.Since(0);
    ASSERT_EQ(frames.size(), expected.size());
    RunInLoopAndWait([&] {
        for (std::size_t i = 0; i < expected.size(); ++i)
        {
            SendFromServer(MakeErrorFrame(frames[i].id(), kProcessId, expected[i].wireError));
        }
    });
    ASSERT_TRUE(log->WaitForCount(expected.size()));

    for (const auto& e : expected)
    {
        const auto entry = log->Find(e.tag);
        ASSERT_TRUE(entry.has_value()) << e.tag;
        EXPECT_EQ(entry->status.code(), e.status) << e.tag << ": " << entry->status.name();
        EXPECT_EQ(entry->status.remoteError(), e.wireError) << e.tag;
        EXPECT_EQ(entry->status.DefinitelyNotExecuted(), e.definitelyNotExecuted) << e.tag;
        EXPECT_EQ(entry->status.OutcomeUnknown(), e.outcomeUnknown) << e.tag;
        EXPECT_FALSE(entry->hasResponse) << e.tag;
    }
}

TEST_F(RpcChannelLoopback, UnparsableOrMismatchedReplyIsAnInvalidResponse)
{
    ASSERT_NO_FATAL_FAILURE(ConnectRpcClient());
    const auto invalidBefore = Load(GetRpcChannelStats().invalidResponse);
    auto log = std::make_shared<CallLog>();

    RunInLoopAndWait([&] {
        Issue(*client_, log, "garbled", kProcessId, ProcessRequest(1));
        Issue(*client_, log, "mismatched", kProcessId, ProcessRequest(2));
    });
    ASSERT_TRUE(serverRecorder_.WaitForCount(2));
    const auto frames = serverRecorder_.Since(0);
    RunInLoopAndWait([&] {
        SendFromServer(MakeRawResponseFrame(frames[0].id(), kProcessId, kTruncatedBody));
        // id 对得上、message_id 对不上:纵深防御,按「结果未知」完成。
        SendFromServer(MakeResponseFrame(frames[1].id(), kCreateId, ProcessResponse(2)));
    });
    ASSERT_TRUE(log->WaitForCount(2));

    for (const char* tag : {"garbled", "mismatched"})
    {
        const auto entry = log->Find(tag);
        ASSERT_TRUE(entry.has_value()) << tag;
        EXPECT_EQ(entry->status.code(), RpcStatusCode::kInvalidResponse) << tag << ": " << entry->status.name();
        EXPECT_TRUE(entry->status.OutcomeUnknown()) << tag;
        EXPECT_FALSE(entry->hasResponse) << tag;
    }
    EXPECT_EQ(Load(GetRpcChannelStats().invalidResponse) - invalidBefore, 2u);
}

// 对端断开:在途调用在 DOWN 回调栈之外各以 kDisconnected 失败一次;出站 channel 随之丢弃,
// enableRetry 重连后是一个新 channel(调用号从 1 重新开始 —— 对端也是新 channel,不会串号)。
TEST_F(RpcChannelLoopback, DisconnectFailsOutstandingCallsAfterTheDownCallbackReturns)
{
    ASSERT_NO_FATAL_FAILURE(ConnectRpcClient());
    auto& stats = GetRpcChannelStats();
    auto log = std::make_shared<CallLog>();
    WatchClientDown(log);
    std::weak_ptr<RpcChannel> oldChannel;

    RunInLoopAndWait([&] {
        oldChannel = client_->channel();
        for (uint32_t i = 1; i <= 3; ++i)
        {
            Issue(*client_, log, "call" + std::to_string(i), kProcessId, ProcessRequest(i));
        }
    });
    ASSERT_TRUE(serverRecorder_.WaitForCount(3)) << "the calls must really be on the wire (in flight)";
    const auto disconnectedBefore = Load(stats.disconnected);

    RunInLoopAndWait([&] {
        if (const auto conn = serverConn_.lock())
        {
            conn->forceClose();
        }
    });
    ASSERT_TRUE(log->WaitForCount(3));

    std::optional<std::size_t> completionsAtDown;
    bool oldChannelExpired = false;
    RunInLoopAndWait([&] {
        completionsAtDown = downProbe_->completionsAtFirstDown;
        oldChannelExpired = oldChannel.expired();
    });
    ASSERT_TRUE(completionsAtDown.has_value()) << "RpcClient never reported DOWN";
    EXPECT_EQ(*completionsAtDown, 0u) << "outstanding calls completed on the DOWN callback stack";
    for (uint32_t i = 1; i <= 3; ++i)
    {
        const auto tag = "call" + std::to_string(i);
        const auto entry = log->Find(tag);
        ASSERT_TRUE(entry.has_value()) << tag;
        EXPECT_EQ(entry->status.code(), RpcStatusCode::kDisconnected) << tag << ": " << entry->status.name();
        EXPECT_TRUE(entry->status.OutcomeUnknown()) << tag;
        EXPECT_EQ(log->CountOf(tag), 1u) << tag;
    }
    EXPECT_EQ(Load(stats.disconnected) - disconnectedBefore, 3u);
    EXPECT_TRUE(oldChannelExpired) << "the outbound channel must be dropped on DOWN";

    // 自动重连:新连接上是新 channel。
    ASSERT_TRUE(WaitUntil([this] { return client_->connected() && serverUps_.load() >= 2; }))
        << "RpcClient did not reconnect";
    const auto framesBefore = serverRecorder_.Count();
    RunInLoopAndWait([&] { Issue(*client_, log, "after_reconnect", kProcessId, ProcessRequest(9)); });
    ASSERT_TRUE(serverRecorder_.WaitForCount(framesBefore + 1));
    const auto frame = serverRecorder_.Since(framesBefore).front();
    EXPECT_EQ(frame.id(), 1u) << "a fresh channel per connection starts its call ids at 1";
    RunInLoopAndWait([&] { SendFromServer(MakeResponseFrame(frame.id(), kProcessId, ProcessResponse(9))); });
    ASSERT_TRUE(log->WaitForCount(4));
    EXPECT_TRUE(log->Find("after_reconnect")->status.ok());
}

// 销毁最后一个 RpcClientPtr(节点摘除 / 同 uuid 重注册的形状):channel 立即析构(超时 tick 只持 weak),
// 在途调用各以 kChannelClosed 失败一次、且不在销毁栈上执行;对端之后再回包也不会进任何回调。
TEST_F(RpcChannelLoopback, DestroyingTheLastRpcClientExpiresItsChannelAndFailsCalls)
{
    ASSERT_NO_FATAL_FAILURE(ConnectRpcClient());
    const auto closedBefore = Load(GetRpcChannelStats().channelClosed);
    auto log = std::make_shared<CallLog>();

    RunInLoopAndWait([&] {
        for (uint32_t i = 1; i <= 3; ++i)
        {
            Issue(*client_, log, "call" + std::to_string(i), kProcessId, ProcessRequest(i));
        }
    });
    ASSERT_TRUE(serverRecorder_.WaitForCount(3));
    const auto frames = serverRecorder_.Since(0);

    bool expiredAtOnce = false;
    std::size_t completionsAtReset = 99;
    RunInLoopAndWait([&] {
        std::weak_ptr<RpcChannel> weakChannel = client_->channel();
        client_.reset();   // 唯一持有者:~RpcClient → ~RpcChannel(仍在 loop 线程)→ 在途批量入队
        expiredAtOnce = weakChannel.expired();
        completionsAtReset = log->Count();
    });
    EXPECT_TRUE(expiredAtOnce) << "something (the timeout tick?) still holds the channel strongly";
    EXPECT_EQ(completionsAtReset, 0u) << "calls completed on the destructor stack";
    ASSERT_TRUE(log->WaitForCount(3));
    for (const auto& entry : log->Entries())
    {
        EXPECT_EQ(entry.status.code(), RpcStatusCode::kChannelClosed) << entry.tag << ": " << entry.status.name();
        EXPECT_EQ(log->CountOf(entry.tag), 1u) << entry.tag;
    }
    EXPECT_EQ(Load(GetRpcChannelStats().channelClosed) - closedBefore, 3u);

    // 对端此后才回包(连接若还没被拆完):不得再进任何回调。
    RunInLoopAndWait([&] {
        if (const auto conn = serverConn_.lock(); conn && conn->connected())
        {
            for (const auto& frame : frames)
            {
                serverRecorder_.Send(conn, MakeResponseFrame(frame.id(), kProcessId, ProcessResponse(0)));
            }
        }
    });
    PumpLoop(3);
    EXPECT_EQ(log->Count(), 3u);
}

// 本地确定没发出的调用:不登记、不发帧,经 queueInLoop 以 kNotSent 投递恰好一次,CallMethod 返回前绝不执行。
TEST_F(RpcChannelLoopback, LocalFailuresAreDeliveredThroughTheQueue)
{
    ASSERT_NO_FATAL_FAILURE(ConnectRpcClient());
    auto& stats = GetRpcChannelStats();
    const auto notSentBefore = Load(stats.notSent);
    const auto onewayRejectedBefore = Load(stats.onewayRejected);
    const auto emptySlot = FindEmptyRegistrySlot();
    auto log = std::make_shared<CallLog>();

    std::size_t synchronousRuns = 0;
    std::size_t completionsAtReturn = 99;
    bool onewayOutOfRange = true;
    bool onewayWrongType = true;
    RpcClientPtr idle;
    RunInLoopAndWait([&] {
        // 1. RpcClient 还没连上(没调 connect):没有 channel。
        idle = std::make_shared<RpcClient>(&Loop(), serverAddr_);
        synchronousRuns += Issue(*idle, log, "not_connected", kProcessId, ProcessRequest(1));
        // 2. 消息号越界 / 注册表空槽。
        synchronousRuns += Issue(*client_, log, "out_of_range", kMaxRpcMethodCount, ProcessRequest(2));
        if (emptySlot)
        {
            synchronousRuns += Issue(*client_, log, "empty_slot", *emptySlot, ProcessRequest(3));
        }
        // 3. 请求类型与注册表不符。
        synchronousRuns += Issue(*client_, log, "wrong_request_type", kProcessId, CreateSceneRequest{});
        // 4. 超时必须 > 0(没有「不限时」)。
        synchronousRuns += Issue(*client_, log, "zero_timeout", kProcessId, ProcessRequest(4),
                                 std::chrono::milliseconds::zero());
        // 单向的本地拒绝:同步返回 false,不发任何帧。
        onewayOutOfRange = client_->SendRequest(kMaxRpcMethodCount, ProcessRequest(5));
        onewayWrongType = client_->SendRequest(kProcessId, CreateSceneRequest{});
        completionsAtReturn = log->Count();
    });
    EXPECT_EQ(synchronousRuns, 0u);
    EXPECT_EQ(completionsAtReturn, 0u) << "a local failure completed before the issuing functor returned";

    const std::size_t expectedFailures = emptySlot ? 5u : 4u;
    ASSERT_TRUE(log->WaitForCount(expectedFailures));
    for (const auto& entry : log->Entries())
    {
        EXPECT_EQ(entry.status.code(), RpcStatusCode::kNotSent) << entry.tag << ": " << entry.status.name();
        EXPECT_TRUE(entry.status.DefinitelyNotExecuted()) << entry.tag;
        EXPECT_TRUE(entry.callMethodReturned) << entry.tag;
        EXPECT_FALSE(entry.hasResponse) << entry.tag;
        EXPECT_EQ(log->CountOf(entry.tag), 1u) << entry.tag;
    }
    EXPECT_EQ(Load(stats.notSent) - notSentBefore, expectedFailures);
    EXPECT_FALSE(onewayOutOfRange);
    EXPECT_FALSE(onewayWrongType);
    EXPECT_EQ(Load(stats.onewayRejected) - onewayRejectedBefore, 2u);

    // 本地失败什么都没发:对端收到的第一帧就是随后这次正常调用。
    RunInLoopAndWait([&] {
        Issue(*client_, log, "sent", kProcessId, ProcessRequest(6));
        idle.reset();
    });
    ASSERT_TRUE(serverRecorder_.WaitForCount(1));
    const auto first = serverRecorder_.Since(0).front();
    EXPECT_EQ(first.message_id(), kProcessId) << Describe(first);
    EXPECT_EQ(RequestSessionOf(first), 6u);
}

// 关机阶段 0 的形状(NodeUtils::CloseAllRpcChannels → Close):在途调用以 kChannelClosed 失败,回调排在随后
// queueInLoop 的 functor(阶段 1 的拆除)之前;Close 之后的新调用一律 kNotSent。
TEST_F(RpcChannelLoopback, CloseFailsOutstandingCallsBeforeLaterQueuedFunctors)
{
    ASSERT_NO_FATAL_FAILURE(ConnectRpcClient());
    auto log = std::make_shared<CallLog>();

    RunInLoopAndWait([&] {
        Issue(*client_, log, "a", kProcessId, ProcessRequest(1));
        Issue(*client_, log, "b", kProcessId, ProcessRequest(2));
    });
    ASSERT_TRUE(serverRecorder_.WaitForCount(2));

    std::size_t completionsAtClose = 99;
    std::size_t synchronousAfterClose = 99;
    RunInLoopAndWait([&] {
        client_->CloseChannel();
        completionsAtClose = log->Count();
        Loop().queueInLoop([log] { log->Mark("marker"); });
        synchronousAfterClose = Issue(*client_, log, "after_close", kProcessId, ProcessRequest(3));
    });
    EXPECT_EQ(completionsAtClose, 0u);
    EXPECT_EQ(synchronousAfterClose, 0u);
    ASSERT_TRUE(log->WaitForCount(4));

    const auto entries = log->Entries();
    ASSERT_EQ(entries.size(), 4u);
    const std::set<std::string> closedTags{entries[0].tag, entries[1].tag};
    EXPECT_EQ(closedTags, (std::set<std::string>{"a", "b"})) << "the close batch must run before the marker";
    EXPECT_EQ(entries[0].status.code(), RpcStatusCode::kChannelClosed) << entries[0].status.name();
    EXPECT_EQ(entries[1].status.code(), RpcStatusCode::kChannelClosed) << entries[1].status.name();
    EXPECT_EQ(entries[2].tag, "marker");
    EXPECT_EQ(entries[3].tag, "after_close");
    EXPECT_EQ(entries[3].status.code(), RpcStatusCode::kNotSent) << entries[3].status.name();
}

TEST_F(RpcChannelLoopback, ReentrantCallMethodFromTimeoutDone)
{
    ASSERT_NO_FATAL_FAILURE(ConnectRpcClient());
    auto log = std::make_shared<CallLog>();

    std::size_t completionsAfterSweep = 99;
    std::size_t outstandingAfterSweep = 99;
    RunInLoopAndWait([&] {
        const RpcChannelPtr channel = client_->channel();
        if (!channel)
        {
            ADD_FAILURE() << "client has no channel";
            return;
        }
        std::weak_ptr<RpcClient> weakClient = client_;
        client_->CallMethod(
            kProcessId, ProcessRequest(1),
            [log, weakClient](const RpcStatus& status, const MessagePtr&)
            {
                Completion first;
                first.tag = "first";
                first.status = status;
                first.callMethodReturned = true;
                log->Push(std::move(first));
                // 在超时扫描栈上重入:过期批次已整批摘出,新调用不在其中。
                if (const auto client = weakClient.lock())
                {
                    Issue(*client, log, "second", kProcessId, ProcessRequest(2));
                }
            },
            RpcCallOptions{kLongTimeout});
        const auto t1 = RpcChannel::Clock::now();
        channel->SweepTimeouts(t1 + kLongTimeout);
        completionsAfterSweep = log->Count();
        outstandingAfterSweep = channel->OutstandingCount();
    });
    EXPECT_EQ(completionsAfterSweep, 1u);
    EXPECT_EQ(outstandingAfterSweep, 1u) << "the call issued from the timeout callback must stay registered";

    ASSERT_TRUE(serverRecorder_.WaitForCount(2));
    const auto second = serverRecorder_.Since(1).front();
    EXPECT_EQ(RequestSessionOf(second), 2u);
    RunInLoopAndWait([&] { SendFromServer(MakeResponseFrame(second.id(), kProcessId, ProcessResponse(2))); });
    ASSERT_TRUE(log->WaitForCount(2));

    const auto entries = log->Entries();
    ASSERT_EQ(entries.size(), 2u);
    EXPECT_EQ(entries[0].tag, "first");
    EXPECT_EQ(entries[0].status.code(), RpcStatusCode::kTimeout) << entries[0].status.name();
    EXPECT_EQ(entries[1].tag, "second");
    EXPECT_TRUE(entries[1].status.ok()) << entries[1].status.name();
}

// 超时扫描的回调里销毁 owner(业务在回调里摘掉节点实体):过期批次已整批摘出,同一批的其余调用照样恰好完成一次。
// 生产上扫描由超时 tick 发起、tick 持有 self;这里由测试栈上的局部强引用扮演那个 self。
// (RESPONSE / RPC_ERROR 的同步完成栈上不得销毁 owner —— 契约 (c),不测。)
TEST_F(RpcChannelLoopback, TimeoutDoneMayDestroyTheOwnerWithoutLosingTheRestOfTheBatch)
{
    ASSERT_NO_FATAL_FAILURE(ConnectRpcClient());
    auto log = std::make_shared<CallLog>();
    const std::weak_ptr<RpcClientPtr> weakBox = HandClientToOwnerBox();

    std::size_t completionsAfterSweep = 99;
    bool ownerGoneAfterSweep = false;
    std::weak_ptr<RpcChannel> weakChannel;
    RunInLoopAndWait([&] {
        const auto box = weakBox.lock();
        const RpcChannelPtr channel = (box && *box) ? (*box)->channel() : RpcChannelPtr{};
        if (!channel)
        {
            ADD_FAILURE() << "client has no channel";
            return;
        }
        weakChannel = channel;
        std::weak_ptr<RpcClient> weakClient = *box;
        for (uint32_t i = 1; i <= 2; ++i)
        {
            (*box)->CallMethod(
                kProcessId, ProcessRequest(i),
                [log, weakBox, tag = "call" + std::to_string(i)](const RpcStatus& status, const MessagePtr& response)
                {
                    Completion completion;
                    completion.tag = tag;
                    completion.status = status;
                    completion.hasResponse = response != nullptr;
                    completion.callMethodReturned = true;
                    log->Push(std::move(completion));
                    if (const auto owner = weakBox.lock())
                    {
                        owner->reset();   // 第一次:销毁 RpcClient;第二次:空操作
                    }
                },
                RpcCallOptions{kLongTimeout});
        }
        const auto t1 = RpcChannel::Clock::now();
        channel->SweepTimeouts(t1 + kLongTimeout);
        completionsAfterSweep = log->Count();
        ownerGoneAfterSweep = weakClient.expired();
    });
    EXPECT_EQ(completionsAfterSweep, 2u) << "destroying the owner inside one timeout callback lost the rest of the batch";
    EXPECT_TRUE(ownerGoneAfterSweep);
    EXPECT_TRUE(weakChannel.expired()) << "the channel outlived its owner and the sweep";
    for (const char* tag : {"call1", "call2"})
    {
        const auto entry = log->Find(tag);
        ASSERT_TRUE(entry.has_value()) << tag;
        EXPECT_EQ(entry->status.code(), RpcStatusCode::kTimeout) << tag << ": " << entry->status.name();
        EXPECT_FALSE(entry->hasResponse) << tag;
        EXPECT_EQ(log->CountOf(tag), 1u) << tag;
    }
    PumpLoop(3);
    EXPECT_EQ(log->Count(), 2u) << "a timed-out call completed again after its owner was destroyed";
}

// 排队投递的失败(Close 的 kChannelClosed、本地 kNotSent)回调里重入 CallMethod 并销毁 owner:
// 重入的调用照样恰好一次(通道已关闭 → kNotSent,仍经队列、不在当前回调里同步执行),销毁之后不再有任何回调。
TEST_F(RpcChannelLoopback, QueuedFailureDoneMayReenterAndDestroyTheOwner)
{
    ASSERT_NO_FATAL_FAILURE(ConnectRpcClient());
    auto log = std::make_shared<CallLog>();
    const std::weak_ptr<RpcClientPtr> weakBox = HandClientToOwnerBox();
    std::weak_ptr<RpcClient> weakClient;

    std::size_t completionsAtClose = 99;
    RunInLoopAndWait([&] {
        const auto box = weakBox.lock();
        if (!box || !*box)
        {
            ADD_FAILURE() << "owner box is empty";
            return;
        }
        weakClient = *box;
        (*box)->CallMethod(
            kProcessId, ProcessRequest(1),
            [log, weakBox, weakClient](const RpcStatus& status, const MessagePtr&)
            {
                Completion first;
                first.tag = "first";
                first.status = status;
                first.callMethodReturned = true;
                log->Push(std::move(first));
                const auto client = weakClient.lock();
                if (!client)
                {
                    return;
                }
                auto returned = std::make_shared<bool>(false);
                client->CallMethod(
                    kProcessId, ProcessRequest(2),
                    [log, weakBox, returned](const RpcStatus& reentrantStatus, const MessagePtr&)
                    {
                        Completion second;
                        second.tag = "second";
                        second.status = reentrantStatus;
                        second.callMethodReturned = *returned;
                        log->Push(std::move(second));
                        if (const auto owner = weakBox.lock())
                        {
                            owner->reset();   // 排队投递的回调栈上销毁 owner
                        }
                    },
                    RpcCallOptions{kLongTimeout});
                *returned = true;
            },
            RpcCallOptions{kLongTimeout});
        (*box)->CloseChannel();
        completionsAtClose = log->Count();
    });
    EXPECT_EQ(completionsAtClose, 0u) << "Close completed a call on its own stack";
    ASSERT_TRUE(log->WaitForCount(2));

    const auto entries = log->Entries();
    ASSERT_EQ(entries.size(), 2u);
    EXPECT_EQ(entries[0].tag, "first");
    EXPECT_EQ(entries[0].status.code(), RpcStatusCode::kChannelClosed) << entries[0].status.name();
    EXPECT_EQ(entries[1].tag, "second");
    EXPECT_EQ(entries[1].status.code(), RpcStatusCode::kNotSent) << entries[1].status.name();
    EXPECT_TRUE(entries[1].callMethodReturned) << "the re-entrant call completed inside its own CallMethod";
    EXPECT_TRUE(WaitUntil([&] { return weakClient.expired(); })) << "destroying the owner from the queued callback failed";
    EXPECT_TRUE(WaitUntil([this] { return serverDowns_.load() == serverUps_.load(); }))
        << "the destroyed owner did not close its connection";
    PumpLoop(3);
    EXPECT_EQ(log->Count(), 2u);
}

// channel 在其 EventLoop 线程之外释放(关机收尾遗漏的形状,§6.4):不碰 loop、不排任何回调,
// 在途调用的 done 随容器销毁而不调用(「恰好一次」的唯一例外),只计 dropped_without_loop。
TEST_F(RpcChannelLoopback, ChannelReleasedOffItsLoopDropsCallsWithoutTouchingTheLoop)
{
    ASSERT_NO_FATAL_FAILURE(ConnectRpcClient());
    const auto droppedBefore = Load(GetRpcChannelStats().droppedWithoutLoop);
    auto log = std::make_shared<CallLog>();

    RpcChannelPtr held;   // 测试线程持有最后一个强引用(生产代码不得这样做)
    RunInLoopAndWait([&] {
        Issue(*client_, log, "a", kProcessId, ProcessRequest(1));
        Issue(*client_, log, "b", kProcessId, ProcessRequest(2));
        held = client_->channel();
        // ~RpcClient:连接 forceClose;它的 DOWN 回调只绑 weak_ptr<RpcClient>,lock 失败,channel 收不到 OnDisconnect。
        client_.reset();
    });
    ASSERT_NE(held, nullptr);
    PumpLoop(3);
    std::size_t outstanding = 0;
    RunInLoopAndWait([&] { outstanding = held->OutstandingCount(); });
    EXPECT_EQ(outstanding, 2u);

    // 超时 tick(只绑 weak)触发时会在 loop 线程上短暂 lock 住 channel;若恰在此刻放手,最后一个引用会在
    // loop 线程上释放、走另一条路径。先把 loop 线程停在一个 functor 里,再在测试线程放手,保证析构确实发生在 loop 之外。
    muduo::CountDownLatch loopParked(1);
    muduo::CountDownLatch releaseLoop(1);
    Loop().runInLoop([&] {
        loopParked.countDown();
        releaseLoop.wait();
    });
    loopParked.wait();
    held.reset();   // 测试线程不是该 channel 的 loop 线程
    releaseLoop.countDown();
    PumpLoop(3);
    EXPECT_EQ(log->Count(), 0u) << "a call completed although its channel died off the loop";
    EXPECT_EQ(Load(GetRpcChannelStats().droppedWithoutLoop) - droppedBefore, 2u);
}

// ===========================================================================
// 三、被测服务端(probe:原始帧 + 客户端侧 RpcChannel → 每连接 RpcChannel + 测试服务)
// ===========================================================================

TEST_F(RpcChannelLoopback, OneWayRequestsNeverGetAReply)
{
    serverMode_ = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    const auto from = probeRecorder_.Count();
    const auto onewayBefore = Load(GetRpcChannelStats().onewaySent);

    bool sentEmpty = false;
    bool sentFilled = false;
    bool sentFailing = false;
    RunInLoopAndWait([&] {
        sentEmpty = probeChannel_->SendRequest(kEnterId, PlayerEnterGameNodeRequest{});
        CreateSceneRequest filled;
        filled.set_config_id(1);   // 响应非空:以前「非空就回」的规则下这里会回包
        sentFilled = probeChannel_->SendRequest(kCreateId, filled);
        sentFailing = probeChannel_->SendRequest(kCancelId, CancelBattlePrepareRequest{});   // SetFailed:也不回 RPC_ERROR
        // 框架拒绝的单向请求同样不回;SendRequest 本地就会拒不可派发的号,所以手工构造这一帧。
        SendFromProbe(MakeRawRequestFrame(kNoInflightCallId, kMaxRpcMethodCount, {}));
    });
    EXPECT_TRUE(sentEmpty);
    EXPECT_TRUE(sentFilled);
    EXPECT_TRUE(sentFailing);
    EXPECT_EQ(Load(GetRpcChannelStats().onewaySent) - onewayBefore, 3u);

    const auto stray = FramesBeforeBarrier(from, kCraftedIdBase + 1);
    EXPECT_TRUE(stray.empty()) << "one-way requests got replies: " << Describe(stray);

    // 都已派发:没有回包不是因为请求被丢了。
    int enterCalls = -1;
    int createCalls = -1;
    int cancelCalls = -1;
    RunInLoopAndWait([&] {
        enterCalls = service_.enterCalls;
        createCalls = service_.createCalls;
        cancelCalls = service_.cancelCalls;
    });
    EXPECT_EQ(enterCalls, 1);
    EXPECT_EQ(createCalls, 1);
    EXPECT_EQ(cancelCalls, 1);
}

TEST_F(RpcChannelLoopback, ServerRejectionsEchoTheCallId)
{
    serverMode_ = ServerMode::kChannel;
    // "Gate" 故意指向 Scene 服务对象:Gate 的方法名在 Scene 的描述符里找不到 → NO_METHOD。SceneScene 不注册 → NO_SERVICE。
    serverServices_ = {{"Scene", &service_}, {"Gate", &service_}};
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    const auto rejectedBefore = Load(GetRpcChannelStats().requestsRejected);
    const auto from = probeRecorder_.Count();

    struct ExpectedRejection
    {
        uint64_t id;
        uint32_t messageId;
        GameErrorCode error;
    };
    std::vector<ExpectedRejection> expected{
        {kCraftedIdBase + 1, kMaxRpcMethodCount, INVALID_REQUEST},   // 越界号
        {kCraftedIdBase + 2, kProcessId, INVALID_REQUEST},           // 请求体解析失败(坏包)
        {kCraftedIdBase + 3, SceneSceneTestMessageId, NO_SERVICE},
        {kCraftedIdBase + 4, GateBroadcastToAllMessageId, NO_METHOD},
    };
    if (const auto emptySlot = FindEmptyRegistrySlot())
    {
        // 注册表空槽:以前拿 nullptr 构造 std::string,任何能连上节点间端口的进程发一帧就能打崩节点。
        expected.push_back({kCraftedIdBase + 5, *emptySlot, INVALID_REQUEST});
    }
    RunInLoopAndWait([&] {
        for (const auto& e : expected)
        {
            SendFromProbe(MakeRawRequestFrame(e.id, e.messageId, e.messageId == kProcessId ? kTruncatedBody : std::string{}));
        }
    });

    const auto frames = FramesBeforeBarrier(from, kCraftedIdBase + 99);
    ASSERT_EQ(frames.size(), expected.size()) << Describe(frames);
    for (std::size_t i = 0; i < expected.size(); ++i)
    {
        EXPECT_EQ(frames[i].type(), RPC_ERROR) << Describe(frames[i]);
        EXPECT_EQ(frames[i].id(), expected[i].id) << Describe(frames[i]);
        EXPECT_EQ(frames[i].message_id(), expected[i].messageId) << Describe(frames[i]);
        EXPECT_EQ(frames[i].error(), expected[i].error) << Describe(frames[i]);
    }
    EXPECT_EQ(Load(GetRpcChannelStats().requestsRejected) - rejectedBefore, expected.size());
    int processCalls = -1;
    RunInLoopAndWait([&] { processCalls = service_.processCalls; });
    EXPECT_EQ(processCalls, 1) << "only the barrier may reach the handler";
}

// id≠0:每个请求恰好一帧 —— Empty / 0 字节 / 早返回也回;SetFailed 与未实现方法回 HANDLER_FAILED;
// handler 同步 done->Run() 后框架不再回(调两次也只有一帧)。
TEST_F(RpcChannelLoopback, ServerRepliesExactlyOncePerCallId)
{
    serverMode_ = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    const auto runTwiceBefore = Load(GetRpcChannelStats().replyRunTwice);
    const auto from = probeRecorder_.Count();

    CreateSceneRequest filled;
    filled.set_config_id(1);
    DestroySceneRequest runTwice;
    runTwice.set_scene_id(kRunDoneTwice);
    struct ExpectedReply
    {
        GameRpcMessage request;
        GameMessageType type;
        GameErrorCode error;
        bool emptyResponse;
    };
    const std::vector<ExpectedReply> expected{
        {MakeRequestFrame(kCraftedIdBase + 11, kEnterId, PlayerEnterGameNodeRequest{}), RESPONSE, RPC_NO_ERROR, true},
        {MakeRequestFrame(kCraftedIdBase + 12, kCreateId, CreateSceneRequest{}), RESPONSE, RPC_NO_ERROR, true},
        {MakeRequestFrame(kCraftedIdBase + 13, kCreateId, filled), RESPONSE, RPC_NO_ERROR, false},
        {MakeRequestFrame(kCraftedIdBase + 14, kCancelId, CancelBattlePrepareRequest{}), RPC_ERROR, HANDLER_FAILED, true},
        {MakeRequestFrame(kCraftedIdBase + 15, kHandshakeId, NodeHandshakeRequest{}), RPC_ERROR, HANDLER_FAILED, true},
        {MakeRequestFrame(kCraftedIdBase + 16, kDestroyId, DestroySceneRequest{}), RESPONSE, RPC_NO_ERROR, true},
        {MakeRequestFrame(kCraftedIdBase + 17, kDestroyId, runTwice), RESPONSE, RPC_NO_ERROR, true},
    };
    RunInLoopAndWait([&] {
        for (const auto& e : expected)
        {
            SendFromProbe(e.request);
        }
    });

    const auto frames = FramesBeforeBarrier(from, kCraftedIdBase + 99);
    ASSERT_EQ(frames.size(), expected.size()) << Describe(frames);
    for (std::size_t i = 0; i < expected.size(); ++i)
    {
        EXPECT_EQ(frames[i].type(), expected[i].type) << Describe(frames[i]);
        EXPECT_EQ(frames[i].id(), expected[i].request.id()) << Describe(frames[i]);
        EXPECT_EQ(frames[i].message_id(), expected[i].request.message_id()) << Describe(frames[i]);
        EXPECT_EQ(frames[i].error(), expected[i].error) << Describe(frames[i]);
        EXPECT_EQ(frames[i].response().empty(), expected[i].emptyResponse) << Describe(frames[i]);
    }
    EXPECT_EQ(Load(GetRpcChannelStats().replyRunTwice) - runTwiceBefore, 1u);
}

// 接管后不完成:最后一个副本释放时回 RPC_ERROR(REPLY_DROPPED)同 id,恰好一帧(多个副本共享一个完成标记)。
TEST_F(RpcChannelLoopback, AdoptedReplyReleasedWithoutRunRepliesReplyDroppedOnce)
{
    serverMode_ = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    const auto abandonedBefore = Load(GetRpcChannelStats().replyAbandoned);
    const auto from = probeRecorder_.Count();

    PrepareBattleRequest park;
    park.set_battle_node_id(2);   // 存两个副本
    PrepareBattleRequest drop;
    drop.set_battle_id(kDropAdoptedReply);
    RunInLoopAndWait([&] {
        SendFromProbe(MakeRequestFrame(kCraftedIdBase + 21, kPrepareId, park));
        SendFromProbe(MakeRequestFrame(kCraftedIdBase + 22, kPrepareId, drop));
    });
    // 22 在 handler 返回时就兜底了;21 的两个副本还暂存着,不回。
    const auto first = FramesBeforeBarrier(from, kCraftedIdBase + 23);
    ASSERT_EQ(first.size(), 1u) << Describe(first);
    EXPECT_EQ(first[0].type(), RPC_ERROR);
    EXPECT_EQ(first[0].id(), kCraftedIdBase + 22);
    EXPECT_EQ(first[0].message_id(), kPrepareId);
    EXPECT_EQ(first[0].error(), REPLY_DROPPED);

    // 起点必须在释放副本之前取:释放即发帧,帧可能在测试线程再读 Count() 之前就已到达并被记录。
    // 此刻第一道屏障的回包已到,之前的帧都已记录完,没有在途的帧。
    const auto secondFrom = probeRecorder_.Count();
    std::size_t parked = 0;
    RunInLoopAndWait([&] {
        parked = service_.parked.size();
        service_.parked.clear();   // 在 loop 线程上释放全部副本
    });
    EXPECT_EQ(parked, 2u);
    const auto second = FramesBeforeBarrier(secondFrom, kCraftedIdBase + 24);
    ASSERT_EQ(second.size(), 1u) << "two copies of one reply must fall back only once: " << Describe(second);
    EXPECT_EQ(second[0].type(), RPC_ERROR);
    EXPECT_EQ(second[0].id(), kCraftedIdBase + 21);
    EXPECT_EQ(second[0].error(), REPLY_DROPPED);
    EXPECT_EQ(Load(GetRpcChannelStats().replyAbandoned) - abandonedBefore, 2u);
}

TEST_F(RpcChannelLoopback, AdoptedReplyRunTwiceSendsOneFrame)
{
    serverMode_ = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    const auto from = probeRecorder_.Count();

    RunInLoopAndWait([&] { SendFromProbe(MakeRequestFrame(kCraftedIdBase + 31, kPrepareId, PrepareBattleRequest{})); });
    ASSERT_TRUE(WaitUntil([this] { return service_.parked.size() == 1; }));

    auto& stats = GetRpcChannelStats();
    const auto runTwiceBefore = Load(stats.replyRunTwice);
    const auto handlerFailedBefore = Load(stats.handlerFailed);
    bool completedBefore = true;
    bool completedAfter = false;
    uint64_t runTwiceAfterSecondRun = 0;
    RunInLoopAndWait([&] {
        const RpcDoneCallback reply = service_.parked.front();
        service_.parked.clear();
        // 延后完成路径直接写响应里的字段(不经全局 tip)。
        reply.response<PrepareBattleResponse>().set_table_fingerprint("deferred");
        completedBefore = reply.IsCompleted();
        reply.Run();
        reply.Run();    // 第二次:吞掉并计数
        runTwiceAfterSecondRun = Load(stats.replyRunTwice);
        reply.Fail();   // 完成之后的 Fail 同样吞掉,也不改写已发出的结果
        completedAfter = reply.IsCompleted();
    });
    EXPECT_FALSE(completedBefore);
    EXPECT_TRUE(completedAfter);
    EXPECT_EQ(runTwiceAfterSecondRun - runTwiceBefore, 1u) << "the second Run must be swallowed and counted once";
    EXPECT_EQ(Load(stats.replyRunTwice) - runTwiceBefore, 2u) << "a Fail after completion is swallowed too";
    EXPECT_EQ(Load(stats.handlerFailed) - handlerFailedBefore, 0u) << "a swallowed Fail must not count as a handler failure";

    const auto frames = FramesBeforeBarrier(from, kCraftedIdBase + 32);
    ASSERT_EQ(frames.size(), 1u) << Describe(frames);
    EXPECT_EQ(frames[0].type(), RESPONSE);
    EXPECT_EQ(frames[0].id(), kCraftedIdBase + 31);
    PrepareBattleResponse response;
    ASSERT_TRUE(response.ParseFromString(frames[0].response()));
    EXPECT_EQ(response.table_fingerprint(), "deferred");
}

// 请求进来的那条连接已断、其 channel 已析构:Run 静默丢弃(计 reply_channel_gone),不崩、不出帧。
TEST_F(RpcChannelLoopback, RunAfterTheCallerChannelIsGoneIsHarmless)
{
    serverMode_ = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    const auto from = probeRecorder_.Count();

    RunInLoopAndWait([&] { SendFromProbe(MakeRequestFrame(kCraftedIdBase + 41, kPrepareId, PrepareBattleRequest{})); });
    ASSERT_TRUE(WaitUntil([this] { return service_.parked.size() == 1; }));

    bool connectedBefore = false;
    RunInLoopAndWait([&] {
        connectedBefore = service_.parked.front().IsCallerConnected();
        if (const auto conn = serverConn_.lock())
        {
            conn->forceClose();
        }
    });
    EXPECT_TRUE(connectedBefore);
    ASSERT_TRUE(WaitUntil([this] { return serverChannel_.expired(); }))
        << "the server-side channel survived DOWN (who still holds it strongly?)";

    const auto goneBefore = Load(GetRpcChannelStats().replyChannelGone);
    bool connectedAfter = true;
    bool completed = false;
    RunInLoopAndWait([&] {
        const RpcDoneCallback reply = service_.parked.front();
        service_.parked.clear();
        connectedAfter = reply.IsCallerConnected();
        reply.Run();
        completed = reply.IsCompleted();
    });
    EXPECT_FALSE(connectedAfter);
    EXPECT_TRUE(completed);
    EXPECT_EQ(Load(GetRpcChannelStats().replyChannelGone) - goneBefore, 1u);

    ASSERT_TRUE(WaitUntil([this] { return probeChannel_ == nullptr; })) << "probe never saw DOWN";
    for (const auto& frame : probeRecorder_.Since(from))
    {
        EXPECT_NE(frame.id(), kCraftedIdBase + 41) << "a reply went out after the caller's channel was gone";
    }
}

// 接管的回复在 loop 线程之外释放最后一个副本(~EventLoop 丢弃闭包、thread_local 析构等任意栈的形状):
// 不发 REPLY_DROPPED、不碰 loop,只计 reply_channel_gone。连接仍在,所以「没有回包」用屏障证明。
TEST_F(RpcChannelLoopback, AdoptedReplyReleasedOffTheLoopSendsNothing)
{
    serverMode_ = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    const auto from = probeRecorder_.Count();

    RunInLoopAndWait([&] { SendFromProbe(MakeRequestFrame(kCraftedIdBase + 61, kPrepareId, PrepareBattleRequest{})); });
    ASSERT_TRUE(WaitUntil([this] { return service_.parked.size() == 1; }));

    auto& stats = GetRpcChannelStats();
    const auto goneBefore = Load(stats.replyChannelGone);
    const auto abandonedBefore = Load(stats.replyAbandoned);
    std::optional<RpcDoneCallback> held;   // RpcDoneCallback 没有默认构造,用 optional 把副本带出 loop 线程
    RunInLoopAndWait([&] {
        held.emplace(service_.parked.front());
        service_.parked.clear();
    });
    ASSERT_TRUE(held.has_value());
    held.reset();   // 测试线程:最后一个副本在 loop 之外析构

    EXPECT_EQ(Load(stats.replyChannelGone) - goneBefore, 1u);
    EXPECT_EQ(Load(stats.replyAbandoned) - abandonedBefore, 0u) << "REPLY_DROPPED must only be sent from the loop thread";
    const auto frames = FramesBeforeBarrier(from, kCraftedIdBase + 62);
    EXPECT_TRUE(frames.empty()) << "a reply went out from a destructor off the loop: " << Describe(frames);
}

// 接管(Adopt)的共享状态只持 weak_ptr<RpcChannel>:派发返回后,连接的引用计数必须回到派发前(AGENTS §11.7)。
TEST_F(RpcChannelLoopback, AdoptKeepsNoStrongConnectionReference)
{
    serverMode_ = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());

    long before = -1;
    long after = -1;
    std::size_t parked = 0;
    RunInLoopAndWait([&] {
        const auto conn = serverConn_.lock();
        const auto channel = serverChannel_.lock();
        if (!conn || !channel)
        {
            ADD_FAILURE() << "server side is not connected";
            return;
        }
        // 直接喂生产入口(与 rpc_controller_test 同一做法),在同一个栈上量派发前后的引用计数。
        muduo::net::Buffer buffer;
        EncodeFrame(MakeRequestFrame(kCraftedIdBase + 51, kPrepareId, PrepareBattleRequest{}), &buffer);
        before = conn.use_count();
        channel->OnMessage(conn, &buffer, muduo::Timestamp::now());
        after = conn.use_count();
        parked = service_.parked.size();
    });
    EXPECT_EQ(parked, 1u);
    EXPECT_EQ(before, after) << "an adopted reply retained " << (after - before) << " strong connection reference(s)";
}

// ===========================================================================
// 四、线程契约
// ===========================================================================

#if GTEST_HAS_DEATH_TEST

// 公开入口一律 assertInLoopThread(muduo 的实现不受 NDEBUG 影响,Release 下同样 LOG_FATAL)。
TEST(RpcChannelDeathTest, PublicEntryOffTheLoopThreadIsFatal)
{
    EXPECT_DEATH(
        {
            muduo::net::EventLoopThread loopThread;
            muduo::net::EventLoop& loop = *loopThread.startLoop();
            const RpcChannelPtr channel = RpcChannel::Create(loop, TcpConnectionPtr{}, RpcServiceMap{});
            channel->OnDisconnect();   // 测试线程不是 loop 线程
        },
        "");
}

namespace
{
// 不是框架交给本次派发的那个 done。
class ForeignClosure final : public ::google::protobuf::Closure
{
public:
    void Run() override {}
};
} // namespace

// Adopt 只接受框架交给本次派发的 done:nullptr 或别的 Closure 必须当场 LOG_FATAL(Release 下同样),
// 而不是 static_cast 后在延后完成时写坏内存。
TEST(RpcDoneCallbackDeathTest, AdoptRejectsNullAndForeignClosures)
{
    EXPECT_DEATH({ (void)RpcDoneCallback::Adopt(nullptr); }, "");
    ForeignClosure foreign;
    EXPECT_DEATH({ (void)RpcDoneCallback::Adopt(&foreign); }, "");
}

#endif // GTEST_HAS_DEATH_TEST

// 维护关闭不能把本机回调退出与远端确定结果混为一谈；使用每夹具独立账本避免用例顺序污染。
TEST_F(RpcChannelLoopback, MaintenanceSourceWaitsForAdoptedReplyAndRejectsNewDispatch)
{
    serverMode_ = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    auto log = std::make_shared<CallLog>();
    RunInLoopAndWait([&] { Issue(*probeChannel_, log, "accepted", kPrepareId, PrepareBattleRequest{}); });
    ASSERT_TRUE(WaitUntil([this] { return service_.parked.size() == 1; }));
    RunInLoopAndWait([&] {
        serverMaintenance_->CloseSources();
        std::string frontier;
        EXPECT_FALSE(serverMaintenance_->Frontier(true, frontier));
        EXPECT_FALSE(serverMaintenance_->LocalCallbacksJoined());
        Issue(*probeChannel_, log, "rejected", kProcessId, ProcessRequest(9));
        service_.parked.front().Run();
        EXPECT_TRUE(serverMaintenance_->LocalCallbacksJoined());
        EXPECT_TRUE(serverMaintenance_->Frontier(true, frontier));
    });
    ASSERT_TRUE(log->WaitForCount(2));
    ASSERT_TRUE(log->Find("accepted").has_value());
    EXPECT_TRUE(log->Find("accepted")->status.ok());
    ASSERT_TRUE(log->Find("rejected").has_value());
    EXPECT_TRUE(log->Find("rejected")->status.DefinitelyNotExecuted());
    RunInLoopAndWait([&] { EXPECT_EQ(service_.processCalls, 0); });
}

TEST_F(RpcChannelLoopback, MaintenanceOutputDoesNotJoinInsideTheBusinessCallback)
{
    serverMode_ = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    std::atomic<bool> callbackReturned{false};
    RunInLoopAndWait([&] {
        probeChannel_->CallMethod(kProcessId, ProcessRequest(3),
            [state = probeMaintenance_, &callbackReturned](const RpcStatus& status, const MessagePtr&) {
                EXPECT_TRUE(status.ok());
                state->CloseSources();
                state->ClosePublications();
                std::string frontier;
                EXPECT_FALSE(state->LocalCallbacksJoined());
                EXPECT_FALSE(state->Frontier(false, frontier));
                callbackReturned = true;
            });
    });
    ASSERT_TRUE(WaitUntil([&] { return callbackReturned.load() && probeMaintenance_->LocalCallbacksJoined(); }));
    std::string frontier;
    EXPECT_TRUE(probeMaintenance_->Frontier(false, frontier));
}

TEST_F(RpcChannelLoopback, MaintenanceTimeoutJoinsLocallyButKeepsStickyUnknown)
{
    serverMode_ = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    auto log = std::make_shared<CallLog>();
    RunInLoopAndWait([&] { Issue(*probeChannel_, log, "unknown", kPrepareId, PrepareBattleRequest{}); });
    ASSERT_TRUE(WaitUntil([this] { return service_.parked.size() == 1; }));
    RunInLoopAndWait([&] {
        probeMaintenance_->CloseSources();
        probeMaintenance_->ClosePublications();
        EXPECT_FALSE(probeMaintenance_->LocalCallbacksJoined());
        probeChannel_->SweepTimeouts(RpcChannel::Clock::now() + kLongTimeout + 1s);
    });
    ASSERT_TRUE(log->WaitForCount(1));
    ASSERT_TRUE(log->Find("unknown").has_value());
    EXPECT_TRUE(log->Find("unknown")->status.OutcomeUnknown());
    ASSERT_TRUE(WaitUntil([this] { return probeMaintenance_->LocalCallbacksJoined(); }));
    std::string frontier;
    EXPECT_FALSE(probeMaintenance_->Frontier(false, frontier));
    // 已关闭后的本地拒绝仍有排队回调；不能在它返回前拆业务对象。
    RunInLoopAndWait([&] {
        Issue(*probeChannel_, log, "local_rejection", kProcessId, ProcessRequest(4));
        EXPECT_FALSE(probeMaintenance_->LocalCallbacksJoined());
    });
    ASSERT_TRUE(log->WaitForCount(2));
    ASSERT_TRUE(WaitUntil([this] { return probeMaintenance_->LocalCallbacksJoined(); }));
    EXPECT_FALSE(probeMaintenance_->Frontier(false, frontier));
}


TEST_F(RpcChannelLoopback, AcknowledgedSendWaitsForActualAdoptedServerCompletion)
{
    ASSERT_TRUE(probeMaintenance_->EnableAcknowledgedSends());
    serverMode_ = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    RunInLoopAndWait([&] { EXPECT_TRUE(probeChannel_->SendRequest(kPrepareId, PrepareBattleRequest{})); });
    ASSERT_TRUE(WaitUntil([this] { return service_.parked.size() == 1; }));
    RunInLoopAndWait([&] {
        probeMaintenance_->CloseSources(); probeMaintenance_->ClosePublications();
        EXPECT_FALSE(probeMaintenance_->LocalCallbacksJoined());
        std::string frontier; EXPECT_FALSE(probeMaintenance_->Frontier(false, frontier));
        service_.parked.front().Run();
    });
    ASSERT_TRUE(WaitUntil([this] { return probeMaintenance_->LocalCallbacksJoined(); }));
    std::string frontier; EXPECT_TRUE(probeMaintenance_->Frontier(false, frontier));
}

TEST_F(RpcChannelLoopback, AcknowledgedSendRetainsServerFailureAsUnknown)
{
    ASSERT_TRUE(probeMaintenance_->EnableAcknowledgedSends());
    serverMode_ = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    RunInLoopAndWait([&] {
        EXPECT_TRUE(probeChannel_->SendRequest(kCancelId, CancelBattlePrepareRequest{}));
        probeMaintenance_->CloseSources(); probeMaintenance_->ClosePublications();
    });
    ASSERT_TRUE(WaitUntil([this] { return service_.cancelCalls == 1 && probeMaintenance_->LocalCallbacksJoined(); }));
    std::string frontier; EXPECT_FALSE(probeMaintenance_->Frontier(false, frontier));
}

TEST_F(RpcChannelLoopback, AcknowledgedSendDisconnectDoesNotForgeRemoteCompletion)
{
    ASSERT_TRUE(probeMaintenance_->EnableAcknowledgedSends());
    serverMode_ = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    RunInLoopAndWait([&] { EXPECT_TRUE(probeChannel_->SendRequest(kPrepareId, PrepareBattleRequest{})); });
    ASSERT_TRUE(WaitUntil([this] { return service_.parked.size() == 1; }));
    RunInLoopAndWait([&] {
        probeMaintenance_->CloseSources(); probeMaintenance_->ClosePublications();
        probe_->connection()->forceClose();
    });
    ASSERT_TRUE(WaitUntil([this] { return probeMaintenance_->LocalCallbacksJoined(); }));
    std::string frontier; EXPECT_FALSE(probeMaintenance_->Frontier(false, frontier));
}

TEST_F(RpcChannelLoopback, AcknowledgedSendTimeoutDoesNotForgetAnAcceptedRequest)
{
    ASSERT_TRUE(probeMaintenance_->EnableAcknowledgedSends());
    serverMode_ = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    RunInLoopAndWait([&] { EXPECT_TRUE(probeChannel_->SendRequest(kPrepareId, PrepareBattleRequest{})); });
    ASSERT_TRUE(WaitUntil([this] { return service_.parked.size() == 1; }));
    RunInLoopAndWait([&] {
        probeMaintenance_->CloseSources(); probeMaintenance_->ClosePublications();
        probeChannel_->SweepTimeouts(RpcChannel::Clock::now() + kDefaultRpcCallTimeout + 1s);
    });
    ASSERT_TRUE(WaitUntil([this] { return probeMaintenance_->LocalCallbacksJoined(); }));
    std::string frontier; EXPECT_FALSE(probeMaintenance_->Frontier(false, frontier));
    RunInLoopAndWait([&] { service_.parked.front().Run(); });
    PumpLoop(3);
    EXPECT_FALSE(probeMaintenance_->Frontier(false, frontier));
}

TEST_F(RpcChannelLoopback, MaintenanceJoinIncludesCallbackTargetDestructionAndDerivedRejection)
{
    serverMode_ = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    auto callbacks = std::make_shared<std::atomic<int>>(0);
    RunInLoopAndWait([&] {
        const auto state = probeMaintenance_;
        const std::weak_ptr<RpcChannel> weak = probeChannel_;
        state->CloseSources(); state->ClosePublications();
        auto target = std::shared_ptr<int>(new int(0), [state, weak, callbacks](int* value) {
            delete value;
            EXPECT_FALSE(state->LocalCallbacksJoined()) << "target 析构仍可派生工作";
            if (const auto channel = weak.lock()) channel->CallMethod(kProcessId, ProcessRequest(9), [state, callbacks](const RpcStatus& status, const MessagePtr&) {
                EXPECT_TRUE(status.DefinitelyNotExecuted()); EXPECT_FALSE(state->LocalCallbacksJoined()); ++*callbacks;
            });
        });
        probeChannel_->CallMethod(kProcessId, ProcessRequest(8), [target, callbacks](const RpcStatus& status, const MessagePtr&) {
            EXPECT_TRUE(status.DefinitelyNotExecuted()); ++*callbacks;
        });
    });
    ASSERT_TRUE(WaitUntil([&] { return callbacks->load() == 2 && probeMaintenance_->LocalCallbacksJoined(); }));
    std::string frontier; EXPECT_TRUE(probeMaintenance_->Frontier(false, frontier));
}

TEST_F(RpcChannelLoopback, MaintenanceSettledSnapshotKeepsExplicitRelayDispatchOpenUntilP3)
{
    serverMode_ = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    auto log=std::make_shared<CallLog>();
    RunInLoopAndWait([&]{std::string frontier;EXPECT_TRUE(serverMaintenance_->SettledFrontier(frontier));Issue(*probeChannel_,log,"relay-after-P1",kProcessId,ProcessRequest(1));});
    ASSERT_TRUE(log->WaitForCount(1)); ASSERT_TRUE(log->Find("relay-after-P1").has_value()); EXPECT_TRUE(log->Find("relay-after-P1")->status.ok());
    RunInLoopAndWait([&]{serverMaintenance_->CloseSources();Issue(*probeChannel_,log,"after-P3",kProcessId,ProcessRequest(2));});
    ASSERT_TRUE(log->WaitForCount(2)); ASSERT_TRUE(log->Find("after-P3").has_value()); EXPECT_TRUE(log->Find("after-P3")->status.DefinitelyNotExecuted());
    RunInLoopAndWait([&]{EXPECT_EQ(service_.processCalls,1);});
}

TEST_F(RpcChannelLoopback, PreparedBootRejectsWireDispatchUntilAtomicOpenAndCannotReopen)
{
    ASSERT_TRUE(serverMaintenance_->PrepareSources());
    serverMode_=ServerMode::kChannel;ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    auto log=std::make_shared<CallLog>();
    RunInLoopAndWait([&]{Issue(*probeChannel_,log,"prepared",kProcessId,ProcessRequest(1));});
    ASSERT_TRUE(log->WaitForCount(1));ASSERT_TRUE(log->Find("prepared").has_value());EXPECT_TRUE(log->Find("prepared")->status.DefinitelyNotExecuted());
    RunInLoopAndWait([&]{EXPECT_EQ(service_.processCalls,0);EXPECT_TRUE(serverMaintenance_->OpenPreparedSources());EXPECT_FALSE(serverMaintenance_->OpenPreparedSources());Issue(*probeChannel_,log,"opened",kProcessId,ProcessRequest(2));});
    ASSERT_TRUE(log->WaitForCount(2));ASSERT_TRUE(log->Find("opened").has_value());EXPECT_TRUE(log->Find("opened")->status.ok());
    RunInLoopAndWait([&]{EXPECT_EQ(service_.processCalls,1);serverMaintenance_->CloseSources();EXPECT_FALSE(serverMaintenance_->PrepareSources());EXPECT_FALSE(serverMaintenance_->OpenPreparedSources());Issue(*probeChannel_,log,"closed",kProcessId,ProcessRequest(3));});
    ASSERT_TRUE(log->WaitForCount(3));ASSERT_TRUE(log->Find("closed").has_value());EXPECT_TRUE(log->Find("closed")->status.DefinitelyNotExecuted());
}
