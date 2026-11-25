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
//   * 链路追踪(docs/design/distributed-tracing.md §5.2):REQUEST 带上发起方当前的 trace,不在 trace 里就不带;
//     服务端在收到的 trace 下面建子 span,信封里的 trace 不合法就开新根、照常派发;回包不带 trace,
//     回包回调恢复发起方的 trace。
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
#include "network/rpc_trace_envelope.h"
#include "proto/common/base/empty.pb.h"
#include "proto/common/base/message.pb.h"
#include "proto/common/base/rpc_message.pb.h"
#include "proto/scene/scene.pb.h"
#include "rpc/service_metadata/gate_service_service_metadata.h"
#include "rpc/service_metadata/rpc_event_registry.h"
#include "rpc/service_metadata/scene_admin_service_metadata.h"
#include "rpc/service_metadata/scene_service_metadata.h"
#include "span_test_support.h"
#include "thread_context/trace_context_tls.h"

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

// 信封上四个 trace 字段都是 0(帧上没有 trace)。
bool HasNoTraceFields(const GameRpcMessage& frame)
{
    return frame.trace_id_hi() == 0 && frame.trace_id_lo() == 0 && frame.parent_span_id() == 0 &&
           frame.trace_flags() == 0;
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
            std::lock_guard<std::mutex> lock(mutex);
            entries.push_back(std::move(completion));
        }
        cv.notify_all();
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
        std::unique_lock<std::mutex> lock(mutex);
        return cv.wait_for(lock, kLiveness, [&] { return entries.size() >= count; });
    }

    std::size_t Count() const
    {
        std::lock_guard<std::mutex> lock(mutex);
        return entries.size();
    }

    std::size_t CountOf(const std::string& tag) const
    {
        std::lock_guard<std::mutex> lock(mutex);
        return static_cast<std::size_t>(
            std::count_if(entries.begin(), entries.end(), [&](const Completion& c) { return c.tag == tag; }));
    }

    std::vector<Completion> Entries() const
    {
        std::lock_guard<std::mutex> lock(mutex);
        return entries;
    }

    std::optional<Completion> Find(const std::string& tag) const
    {
        std::lock_guard<std::mutex> lock(mutex);
        for (const auto& entry : entries)
        {
            if (entry.tag == tag)
            {
                return entry;
            }
        }
        return std::nullopt;
    }

private:
    mutable std::mutex mutex;
    mutable std::condition_variable cv;
    std::vector<Completion> entries;
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
        : codec([this](const TcpConnectionPtr&, const RpcMessagePtr& frame, muduo::Timestamp) { Push(*frame); })
    {
    }

    FrameRecorder(const FrameRecorder&) = delete;
    FrameRecorder& operator=(const FrameRecorder&) = delete;

    // loop 线程:作为连接的消息回调,直接消费输入。
    void OnMessage(const TcpConnectionPtr& conn, muduo::net::Buffer* buffer, muduo::Timestamp receiveTime)
    {
        codec.onMessage(conn, buffer, receiveTime);
    }

    // loop 线程:旁路记录,不消费 buffer(字节拷进自持缓冲,半帧留到下次)。
    void Tee(const TcpConnectionPtr& conn, const muduo::net::Buffer& buffer, muduo::Timestamp receiveTime)
    {
        teeBuffer.append(buffer.peek(), buffer.readableBytes());
        codec.onMessage(conn, &teeBuffer, receiveTime);
    }

    // loop 线程:按线上格式发一帧。
    void Send(const TcpConnectionPtr& conn, const GameRpcMessage& frame) { codec.send(conn, frame); }

    std::size_t Count() const
    {
        std::lock_guard<std::mutex> lock(mutex);
        return frames.size();
    }

    std::vector<GameRpcMessage> Since(std::size_t from) const
    {
        std::lock_guard<std::mutex> lock(mutex);
        if (from >= frames.size())
        {
            return {};
        }
        return std::vector<GameRpcMessage>(frames.begin() + static_cast<std::ptrdiff_t>(from), frames.end());
    }

    bool WaitForCount(std::size_t count) const
    {
        std::unique_lock<std::mutex> lock(mutex);
        return cv.wait_for(lock, kLiveness, [&] { return frames.size() >= count; });
    }

    // pred(const std::vector<GameRpcMessage>&) 在锁内求值。
    template <typename Pred>
    bool WaitUntil(Pred pred) const
    {
        std::unique_lock<std::mutex> lock(mutex);
        return cv.wait_for(lock, kLiveness, [&] { return pred(frames); });
    }

private:
    void Push(const GameRpcMessage& frame)
    {
        {
            std::lock_guard<std::mutex> lock(mutex);
            frames.push_back(frame);
        }
        cv.notify_all();
    }

    mutable std::mutex mutex;
    mutable std::condition_variable cv;
    std::vector<GameRpcMessage> frames;
    muduo::net::Buffer teeBuffer;   // 只在 loop 线程上用
    RpcCodec codec;                 // 解码回调绑 this:codec_ 是本对象的成员,寿命相同
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
    // ProcessClientPlayerMessage 执行时的当前 trace,按请求的 session_id 记(链路追踪用例读)。
    std::vector<std::pair<uint32_t, tracing::TraceContext>> processTraces;
    // ProcessClientPlayerMessage 执行时 ctx 上的握手身份,按请求的 session_id 记(握手身份透传用例读)。
    std::vector<std::pair<uint32_t, std::string>> processPeers;

    std::optional<tracing::TraceContext> ProcessTraceOf(uint32_t sessionId) const
    {
        for (const auto& [session, trace] : processTraces)
        {
            if (session == sessionId)
            {
                return trace;
            }
        }
        return std::nullopt;
    }

    // 同步回显:返回即由框架回包。
    void ProcessClientPlayerMessage(::google::protobuf::RpcController* controller,
                                    const ::ProcessClientPlayerMessageRequest* request,
                                    ::ProcessClientPlayerMessageResponse* response,
                                    ::google::protobuf::Closure*) override
    {
        ++processCalls;
        processTraces.emplace_back(request->session_id(), tracing::Current());
        processPeers.emplace_back(request->session_id(), RpcController::From(controller).HandshakePeerUuid());
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
        loop.emplace(*loopThread.startLoop());
        const uint16_t port = FindFreeLoopbackPort();
        ASSERT_NE(port, 0);
        serverAddr = muduo::net::InetAddress("127.0.0.1", port);
        ASSERT_TRUE(IsDispatchableMessageId(kProcessId))
            << "gRpcMethodRegistry is empty: the global RpcRegistryEnvironment did not run";

        RunInLoopAndWait([this, port] {
            server = std::make_unique<muduo::net::TcpServer>(
                &Loop(), muduo::net::InetAddress(port, /*loopbackOnly=*/true), "rpc_channel_loopback_server");
            // 回调捕获夹具 this:server_ 在 TearDown 里先于夹具销毁(AGENTS §11.7 允许的同寿命例外)。
            server->setConnectionCallback([this](const TcpConnectionPtr& conn) { OnServerConnection(conn); });
            server->start();   // 在 loop 线程上即时 listen,随后的 connect 不会撞拒连
        });
    }

    void TearDown() override
    {
        // 顺序同 rpc_controller_test:先拆客户端(各自连接的唯一持有者 → forceClose → 对端 DOWN),
        // 等服务端每条连接都报过 DOWN,再在 loop 线程销毁 TcpServer(它的析构函数断言 in-loop)。
        RunInLoopAndWait([this] {
            downProbe->enabled = false;
            if (ownerBox)
            {
                ownerBox->reset();   // 用例中途失败时 owner 还在盒子里:也在 loop 线程上销毁
            }
            client.reset();
            probe.reset();
        });
        PumpLoop(3);
        EXPECT_TRUE(WaitUntil([this] { return serverDowns.load() == serverUps.load(); }))
            << "server side did not see DOWN for every connection: ups=" << serverUps.load()
            << " downs=" << serverDowns.load();
        RunInLoopAndWait([this] {
            probeChannel.reset();
            // 暂存的回复在 loop 线程上释放:连接已断,只计 reply_channel_gone,不发任何帧。
            service.parked.clear();
            server.reset();
        });
        PumpLoop(2);
        // loopThread_ 析构 = quit + join;排队的 connectDestroyed 早已跑完。
    }

    muduo::net::EventLoop& Loop() { return loop->get(); }

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
            serverConn = conn;
            if (serverMode == ServerMode::kChannel)
            {
                RpcChannelPtr channel = RpcChannel::Create(Loop(), conn, serverServices, serverMaintenance);
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
                serverChannel = channel;
            }
            else
            {
                conn->setMessageCallback(
                    [this](const TcpConnectionPtr& c, muduo::net::Buffer* buffer, muduo::Timestamp receiveTime)
                    {
                        serverRecorder.OnMessage(c, buffer, receiveTime);
                    });
            }
            ++serverUps;
            return;
        }
        if (const auto* channel = boost::any_cast<RpcChannelPtr>(&conn->getContext()); channel != nullptr && *channel)
        {
            (*channel)->OnDisconnect();
        }
        conn->setContext(RpcChannelPtr());
        ++serverDowns;
    }

    void ConnectRpcClient()
    {
        RunInLoopAndWait([this] {
            client = std::make_shared<RpcClient>(&Loop(), serverAddr);
            client->setConnectionObserver([weakProbe = std::weak_ptr<DownProbe>(downProbe)](const TcpConnectionPtr& conn) {
                if (const auto probe = weakProbe.lock(); probe && probe->enabled)
                {
                    probe->OnEvent(conn);
                }
            });
            client->connect();
        });
        ASSERT_TRUE(WaitUntil([this] { return client->connected() && !serverConn.expired(); }))
            << "RpcClient never connected";
    }

    // 启用 connect 前已安装的观察者探针；TearDown 先停探针，用例中途 ASSERT 返回也不会悬空。
    void WatchClientDown(std::shared_ptr<CallLog> log)
    {
        RunInLoopAndWait([this, log = std::move(log)]() mutable {
            downProbe->log = std::move(log);
            downProbe->completionsAtFirstDown.reset();
            downProbe->enabled = true;
        });
    }

    // 把 client_ 的唯一强引用移进一个盒子,回调里经 weak_ptr<盒子> 销毁它 == 业务在回调里摘掉节点实体。
    // 回调只捕获盒子的 weak:不形成 client → channel → 在途 done → 盒子 → client 的环。
    std::weak_ptr<RpcClientPtr> HandClientToOwnerBox()
    {
        RunInLoopAndWait([this] { ownerBox = std::make_shared<RpcClientPtr>(std::move(client)); });
        return ownerBox;
    }

    // 只配 kChannel 模式:被测的是服务端。
    void ConnectProbe()
    {
        RunInLoopAndWait([this] {
            probe = std::make_unique<muduo::net::TcpClient>(&Loop(), serverAddr, "rpc_channel_loopback_probe");
            // 同寿命例外:probe_ 在 TearDown 里先于夹具销毁,之后的 DOWN 回调也在 TearDown 转 loop 时跑完。
            probe->setConnectionCallback([this](const TcpConnectionPtr& conn) { OnProbeConnection(conn); });
            probe->setMessageCallback(
                [this](const TcpConnectionPtr& conn, muduo::net::Buffer* buffer, muduo::Timestamp receiveTime)
                {
                    OnProbeMessage(conn, buffer, receiveTime);
                });
            probe->connect();
        });
        ASSERT_TRUE(WaitUntil([this] {
            return probeChannel && probeChannel->IsConnected() && !serverChannel.expired();
        })) << "probe never connected to the channel server";
    }

    // loop 线程。probe 的客户端侧 channel 与 RpcClient 同样每条连接一个、DOWN 即丢弃。
    void OnProbeConnection(const TcpConnectionPtr& conn)
    {
        if (conn->connected())
        {
            probeChannel = RpcChannel::Create(Loop(), conn, RpcServiceMap{}, probeMaintenance);
            return;
        }
        if (probeChannel && probeChannel->IsBoundTo(conn))
        {
            probeChannel->OnDisconnect();
            probeChannel.reset();
        }
    }

    void OnProbeMessage(const TcpConnectionPtr& conn, muduo::net::Buffer* buffer, muduo::Timestamp receiveTime)
    {
        probeRecorder.Tee(conn, *buffer, receiveTime);
        if (probeChannel && probeChannel->IsBoundTo(conn))
        {
            probeChannel->OnMessage(conn, buffer, receiveTime);
            return;
        }
        buffer->retrieveAll();
    }

    // loop 线程:kRecorder 模式下由服务端回一帧。
    void SendFromServer(const GameRpcMessage& frame)
    {
        const auto conn = serverConn.lock();
        if (!conn)
        {
            ADD_FAILURE() << "server connection is gone";
            return;
        }
        serverRecorder.Send(conn, frame);
    }

    // loop 线程:经 probe 连接发一帧手工构造的请求。
    void SendFromProbe(const GameRpcMessage& frame)
    {
        const TcpConnectionPtr conn = probe ? probe->connection() : TcpConnectionPtr{};
        if (!conn)
        {
            ADD_FAILURE() << "probe connection is gone";
            return;
        }
        probeRecorder.Send(conn, frame);
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
        const bool seen = probeRecorder.WaitUntil([&](const std::vector<GameRpcMessage>& frames) {
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
    muduo::net::EventLoopThread loopThread;
    std::optional<std::reference_wrapper<muduo::net::EventLoop>> loop;
    muduo::net::InetAddress serverAddr;

    // 测试服务声明在服务器之前:server 侧 channel 的服务表借用它,它必须后析构。
    LoopbackSceneService service;
    ServerMode serverMode{ServerMode::kRecorder};   // 连接前设好;UP 时在 loop 线程读取
    RpcServiceMap serverServices{{"Scene", &service}};
    std::unique_ptr<muduo::net::TcpServer> server;
    FrameRecorder serverRecorder;
    // 以下两个 weak 只在 loop 线程上读写。
    std::weak_ptr<muduo::net::TcpConnection> serverConn;
    std::weak_ptr<RpcChannel> serverChannel;
    std::atomic<int> serverUps{0};
    std::atomic<int> serverDowns{0};

    RpcClientPtr client;
    // 以下状态只在 loop 线程上读写。
    std::shared_ptr<RpcClientPtr> ownerBox;
    std::shared_ptr<DownProbe> downProbe{std::make_shared<DownProbe>()};

    std::unique_ptr<muduo::net::TcpClient> probe;
    RpcChannelPtr probeChannel;   // 只在 loop 线程上读写
    FrameRecorder probeRecorder;
    std::shared_ptr<RpcMaintenanceState> serverMaintenance{std::make_shared<RpcMaintenanceState>()};
    std::shared_ptr<RpcMaintenanceState> probeMaintenance{std::make_shared<RpcMaintenanceState>()};
};

} // namespace

// ===========================================================================
// 一、往返(RpcClient → 每连接 RpcChannel + 测试服务)
// ===========================================================================

TEST_F(RpcChannelLoopback, RoundTripCompletesExactlyOnceAfterCallMethodReturned)
{
    serverMode = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectRpcClient());
    auto& stats = GetRpcChannelStats();
    const auto startedBefore = Load(stats.started);
    const auto okBefore = Load(stats.ok);
    auto log = std::make_shared<CallLog>();

    std::size_t synchronousRuns = 99;
    RunInLoopAndWait([&] { synchronousRuns = Issue(*client, log, "call", kProcessId, ProcessRequest(7)); });
    EXPECT_EQ(synchronousRuns, 0u) << "done ran inside CallMethod";
    ASSERT_TRUE(log->WaitForCount(1));

    // 屏障:同一连接上再走一次往返。第一次调用若被重复完成,必然出现在屏障之前。
    RunInLoopAndWait([&] { Issue(*client, log, "barrier", kProcessId, ProcessRequest(8)); });
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
        outstanding = client->Channel() ? client->Channel()->OutstandingCount() : 99;
        handlerCalls = service.processCalls;
    });
    EXPECT_EQ(outstanding, 0u);
    EXPECT_EQ(handlerCalls, 2);
}

TEST_F(RpcChannelLoopback, HandshakePeerRecordedOnTheInboundChannelReachesEveryLaterDispatch)
{
    // 入站侧握手成功时 NodeHandshakeManager::OnNodeHandshake 经 RecordHandshakePeer 记下对端进程;之后这条连接上每次派发的
    // ctx 都带着它(docs/design/muduo-rpc-channel.md §8.3)。scene 按它认 gate 实例、不信消息体自报
    // (docs/design/scene-enter-chain-residuals.md §14)。宿主不构造 Node,这里直接调记录入口,与 OnNodeHandshake 同一个。
    serverMode = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectRpcClient());
    auto log = std::make_shared<CallLog>();
    constexpr const char kPeer[] = "gate-node3-new-process";

    RunInLoopAndWait([&] { Issue(*client, log, "before", kProcessId, ProcessRequest(11)); });
    ASSERT_TRUE(log->WaitForCount(1));

    bool recorded = false;
    bool recordedWithoutConnection = true;
    RunInLoopAndWait([&] {
        recorded = RpcChannel::RecordHandshakePeer(serverConn.lock(), kPeer);
        recordedWithoutConnection = RpcChannel::RecordHandshakePeer(TcpConnectionPtr{}, "ignored");
    });
    EXPECT_TRUE(recorded) << "服务端连接的 context 里放着本连接的 channel";
    EXPECT_FALSE(recordedWithoutConnection) << "空连接什么都不做";

    RunInLoopAndWait([&] { Issue(*client, log, "after", kProcessId, ProcessRequest(12)); });
    ASSERT_TRUE(log->WaitForCount(2));

    std::vector<std::pair<uint32_t, std::string>> peers;
    std::string clientSidePeer = "unread";
    RunInLoopAndWait([&] {
        peers = service.processPeers;
        clientSidePeer = client->Channel() ? client->Channel()->HandshakePeerUuid() : std::string("no channel");
    });
    ASSERT_EQ(peers.size(), 2u);
    EXPECT_EQ(peers[0].first, 11u);
    EXPECT_EQ(peers[0].second, "") << "握手之前:ctx 上没有对端身份";
    EXPECT_EQ(peers[1].first, 12u);
    EXPECT_EQ(peers[1].second, kPeer) << "记下之后的派发都读得到";
    EXPECT_EQ(clientSidePeer, "") << "只记在入站那一侧的 channel 上,出站 channel 恒为空";
}

TEST_F(RpcChannelLoopback, TypedCallMethodChecksTheResponseTypeAgainstTheRegistry)
{
    serverMode = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectRpcClient());
    auto log = std::make_shared<CallLog>();

    RunInLoopAndWait([&] {
        client->CallMethod<TestProcessClientPlayerMessageRpc>(
            ProcessRequest(9), TypedRecorder<ProcessClientPlayerMessageResponse>(log, "typed"),
            RpcCallOptions{kLongTimeout});
        client->CallMethod<TestMismatchedResponseRpc>(
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
    EXPECT_EQ(mismatched->status.Code(), RpcStatusCode::kNotSent) << mismatched->status.name();
    EXPECT_FALSE(mismatched->hasResponse);

    int handlerCalls = -1;
    RunInLoopAndWait([&] { handlerCalls = service.processCalls; });
    EXPECT_EQ(handlerCalls, 1) << "the mismatched call must not reach the wire";
}

TEST_F(RpcChannelLoopback, EmptyResponseAndEarlyReturnStillReply)
{
    serverMode = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectRpcClient());
    auto log = std::make_shared<CallLog>();

    RunInLoopAndWait([&] {
        Issue(*client, log, "empty", kEnterId, PlayerEnterGameNodeRequest{});
        Issue(*client, log, "early_return", kCreateId, CreateSceneRequest{});
        CreateSceneRequest filled;
        filled.set_config_id(1);
        Issue(*client, log, "filled", kCreateId, filled);
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
    serverMode = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectRpcClient());
    auto log = std::make_shared<CallLog>();
    const auto failedBefore = Load(GetRpcChannelStats().remoteFailed);

    RunInLoopAndWait([&] {
        Issue(*client, log, "set_failed", kCancelId, CancelBattlePrepareRequest{});
        Issue(*client, log, "unimplemented", kHandshakeId, NodeHandshakeRequest{});
    });
    ASSERT_TRUE(log->WaitForCount(2));

    for (const char* tag : {"set_failed", "unimplemented"})
    {
        const auto entry = log->Find(tag);
        ASSERT_TRUE(entry.has_value()) << tag;
        EXPECT_EQ(entry->status.Code(), RpcStatusCode::kRemoteFailed) << tag << ": " << entry->status.name();
        EXPECT_EQ(entry->status.RemoteError(), HANDLER_FAILED) << tag;
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
    serverMode = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectRpcClient());
    auto& stats = GetRpcChannelStats();
    const auto lateBefore = Load(stats.lateReply);
    auto log = std::make_shared<CallLog>();

    RunInLoopAndWait([&] {
        PrepareBattleRequest drop;
        drop.set_battle_id(kDropAdoptedReply);
        Issue(*client, log, "dropped_in_handler", kPrepareId, drop);
        PrepareBattleRequest park;
        park.set_battle_node_id(2);   // 存两个副本
        Issue(*client, log, "released_later", kPrepareId, park);
    });
    ASSERT_TRUE(log->WaitForCount(1));
    ASSERT_TRUE(WaitUntil([this] { return service.parked.size() == 2; }));
    EXPECT_EQ(log->CountOf("released_later"), 0u) << "still parked: must not complete yet";

    RunInLoopAndWait([this] { service.parked.clear(); });
    ASSERT_TRUE(log->WaitForCount(2));
    // 屏障:若两个副本各回了一次 REPLY_DROPPED,第二帧会作为迟到回包出现在屏障之前。
    RunInLoopAndWait([&] { Issue(*client, log, "barrier", kProcessId, ProcessRequest(1)); });
    ASSERT_TRUE(log->WaitForCount(3));

    for (const char* tag : {"dropped_in_handler", "released_later"})
    {
        const auto entry = log->Find(tag);
        ASSERT_TRUE(entry.has_value()) << tag;
        EXPECT_EQ(entry->status.Code(), RpcStatusCode::kReplyDropped) << tag << ": " << entry->status.name();
        EXPECT_EQ(entry->status.RemoteError(), REPLY_DROPPED) << tag;
        EXPECT_TRUE(entry->status.OutcomeUnknown()) << tag;
        EXPECT_EQ(log->CountOf(tag), 1u) << tag;
    }
    EXPECT_EQ(Load(stats.lateReply) - lateBefore, 0u) << "a shared reply state must fall back only once";
}

TEST_F(RpcChannelLoopback, ReentrantCallMethodFromResponseDone)
{
    serverMode = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectRpcClient());
    auto log = std::make_shared<CallLog>();

    RunInLoopAndWait([&] {
        // done 只捕获值与 weak(契约 b):weak_ptr<RpcClient>,执行时重新 lock。
        std::weak_ptr<RpcClient> weakClient = this->client;
        this->client->CallMethod(
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
        Issue(*client, log, "first", kProcessId, ProcessRequest(1));
        Issue(*client, log, "second", kProcessId, ProcessRequest(2));
    });
    ASSERT_TRUE(serverRecorder.WaitForCount(2));
    const auto frames = serverRecorder.Since(0);
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
        const RpcChannelPtr channel = client->Channel();
        if (!channel)
        {
            ADD_FAILURE() << "client has no channel";
            return;
        }
        const auto t0 = RpcChannel::Clock::now();
        Issue(*client, log, "slow", kProcessId, ProcessRequest(1), kLongTimeout);
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
    EXPECT_EQ(slow->status.Code(), RpcStatusCode::kTimeout) << slow->status.name();
    EXPECT_TRUE(slow->status.OutcomeUnknown());
    EXPECT_FALSE(slow->hasResponse);
    EXPECT_EQ(Load(stats.timeout) - timeoutBefore, 1u);

    // 对端此时才回包:同 id 的迟到回包必须被丢弃并计数,不得再次完成。屏障调用的回包排在它之后(同连接 FIFO)。
    ASSERT_TRUE(serverRecorder.WaitForCount(1));
    const auto slowFrame = serverRecorder.Since(0).front();
    RunInLoopAndWait([&] {
        SendFromServer(MakeResponseFrame(slowFrame.id(), kProcessId, ProcessResponse(1)));
        Issue(*client, log, "barrier", kProcessId, ProcessRequest(2));
    });
    ASSERT_TRUE(serverRecorder.WaitForCount(2));
    const auto barrierFrame = serverRecorder.Since(1).front();
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

    RunInLoopAndWait([&] { Issue(*client, log, "real", kProcessId, ProcessRequest(1)); });
    ASSERT_TRUE(serverRecorder.WaitForCount(1));
    const uint64_t id = serverRecorder.Since(0).front().id();

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
    RunInLoopAndWait([&] { stillConnected = client->connected(); });
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
        // 热关停(docs/design/rpc-kill-switch.md §7):派发前拒绝,确定未执行。
        {"kill_switch_denied", KILL_SWITCH_DENIED, RpcStatusCode::kRemoteRejected, true, false},
        {"handler_failed", HANDLER_FAILED, RpcStatusCode::kRemoteFailed, false, false},
        {"reply_dropped", REPLY_DROPPED, RpcStatusCode::kReplyDropped, false, true},
    };

    RunInLoopAndWait([&] {
        uint32_t session = 1;
        for (const auto& e : expected)
        {
            Issue(*client, log, e.tag, kProcessId, ProcessRequest(session++));
        }
    });
    ASSERT_TRUE(serverRecorder.WaitForCount(expected.size()));
    const auto frames = serverRecorder.Since(0);
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
        EXPECT_EQ(entry->status.Code(), e.status) << e.tag << ": " << entry->status.name();
        EXPECT_EQ(entry->status.RemoteError(), e.wireError) << e.tag;
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
        Issue(*client, log, "garbled", kProcessId, ProcessRequest(1));
        Issue(*client, log, "mismatched", kProcessId, ProcessRequest(2));
    });
    ASSERT_TRUE(serverRecorder.WaitForCount(2));
    const auto frames = serverRecorder.Since(0);
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
        EXPECT_EQ(entry->status.Code(), RpcStatusCode::kInvalidResponse) << tag << ": " << entry->status.name();
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
        oldChannel = client->Channel();
        for (uint32_t i = 1; i <= 3; ++i)
        {
            Issue(*client, log, "call" + std::to_string(i), kProcessId, ProcessRequest(i));
        }
    });
    ASSERT_TRUE(serverRecorder.WaitForCount(3)) << "the calls must really be on the wire (in flight)";
    const auto disconnectedBefore = Load(stats.disconnected);

    RunInLoopAndWait([&] {
        if (const auto conn = serverConn.lock())
        {
            conn->forceClose();
        }
    });
    ASSERT_TRUE(log->WaitForCount(3));

    std::optional<std::size_t> completionsAtDown;
    bool oldChannelExpired = false;
    RunInLoopAndWait([&] {
        completionsAtDown = downProbe->completionsAtFirstDown;
        oldChannelExpired = oldChannel.expired();
    });
    ASSERT_TRUE(completionsAtDown.has_value()) << "RpcClient never reported DOWN";
    EXPECT_EQ(*completionsAtDown, 0u) << "outstanding calls completed on the DOWN callback stack";
    for (uint32_t i = 1; i <= 3; ++i)
    {
        const auto tag = "call" + std::to_string(i);
        const auto entry = log->Find(tag);
        ASSERT_TRUE(entry.has_value()) << tag;
        EXPECT_EQ(entry->status.Code(), RpcStatusCode::kDisconnected) << tag << ": " << entry->status.name();
        EXPECT_TRUE(entry->status.OutcomeUnknown()) << tag;
        EXPECT_EQ(log->CountOf(tag), 1u) << tag;
    }
    EXPECT_EQ(Load(stats.disconnected) - disconnectedBefore, 3u);
    EXPECT_TRUE(oldChannelExpired) << "the outbound channel must be dropped on DOWN";

    // 自动重连:新连接上是新 channel。
    ASSERT_TRUE(WaitUntil([this] { return client->connected() && serverUps.load() >= 2; }))
        << "RpcClient did not reconnect";
    const auto framesBefore = serverRecorder.Count();
    RunInLoopAndWait([&] { Issue(*client, log, "after_reconnect", kProcessId, ProcessRequest(9)); });
    ASSERT_TRUE(serverRecorder.WaitForCount(framesBefore + 1));
    const auto frame = serverRecorder.Since(framesBefore).front();
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
            Issue(*client, log, "call" + std::to_string(i), kProcessId, ProcessRequest(i));
        }
    });
    ASSERT_TRUE(serverRecorder.WaitForCount(3));
    const auto frames = serverRecorder.Since(0);

    bool expiredAtOnce = false;
    std::size_t completionsAtReset = 99;
    RunInLoopAndWait([&] {
        std::weak_ptr<RpcChannel> weakChannel = client->Channel();
        client.reset();   // 唯一持有者:~RpcClient → ~RpcChannel(仍在 loop 线程)→ 在途批量入队
        expiredAtOnce = weakChannel.expired();
        completionsAtReset = log->Count();
    });
    EXPECT_TRUE(expiredAtOnce) << "something (the timeout tick?) still holds the channel strongly";
    EXPECT_EQ(completionsAtReset, 0u) << "calls completed on the destructor stack";
    ASSERT_TRUE(log->WaitForCount(3));
    for (const auto& entry : log->Entries())
    {
        EXPECT_EQ(entry.status.Code(), RpcStatusCode::kChannelClosed) << entry.tag << ": " << entry.status.name();
        EXPECT_EQ(log->CountOf(entry.tag), 1u) << entry.tag;
    }
    EXPECT_EQ(Load(GetRpcChannelStats().channelClosed) - closedBefore, 3u);

    // 对端此后才回包(连接若还没被拆完):不得再进任何回调。
    RunInLoopAndWait([&] {
        if (const auto conn = serverConn.lock(); conn && conn->connected())
        {
            for (const auto& frame : frames)
            {
                serverRecorder.Send(conn, MakeResponseFrame(frame.id(), kProcessId, ProcessResponse(0)));
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
        idle = std::make_shared<RpcClient>(&Loop(), serverAddr);
        synchronousRuns += Issue(*idle, log, "not_connected", kProcessId, ProcessRequest(1));
        // 2. 消息号越界 / 注册表空槽。
        synchronousRuns += Issue(*client, log, "out_of_range", kMaxRpcMethodCount, ProcessRequest(2));
        if (emptySlot)
        {
            synchronousRuns += Issue(*client, log, "empty_slot", *emptySlot, ProcessRequest(3));
        }
        // 3. 请求类型与注册表不符。
        synchronousRuns += Issue(*client, log, "wrong_request_type", kProcessId, CreateSceneRequest{});
        // 4. 超时必须 > 0(没有「不限时」)。
        synchronousRuns += Issue(*client, log, "zero_timeout", kProcessId, ProcessRequest(4),
                                 std::chrono::milliseconds::zero());
        // 单向的本地拒绝:同步返回 false,不发任何帧。
        onewayOutOfRange = client->SendRequest(kMaxRpcMethodCount, ProcessRequest(5));
        onewayWrongType = client->SendRequest(kProcessId, CreateSceneRequest{});
        completionsAtReturn = log->Count();
    });
    EXPECT_EQ(synchronousRuns, 0u);
    EXPECT_EQ(completionsAtReturn, 0u) << "a local failure completed before the issuing functor returned";

    const std::size_t expectedFailures = emptySlot ? 5u : 4u;
    ASSERT_TRUE(log->WaitForCount(expectedFailures));
    for (const auto& entry : log->Entries())
    {
        EXPECT_EQ(entry.status.Code(), RpcStatusCode::kNotSent) << entry.tag << ": " << entry.status.name();
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
        Issue(*client, log, "sent", kProcessId, ProcessRequest(6));
        idle.reset();
    });
    ASSERT_TRUE(serverRecorder.WaitForCount(1));
    const auto first = serverRecorder.Since(0).front();
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
        Issue(*client, log, "a", kProcessId, ProcessRequest(1));
        Issue(*client, log, "b", kProcessId, ProcessRequest(2));
    });
    ASSERT_TRUE(serverRecorder.WaitForCount(2));

    std::size_t completionsAtClose = 99;
    std::size_t synchronousAfterClose = 99;
    RunInLoopAndWait([&] {
        client->CloseChannel();
        completionsAtClose = log->Count();
        Loop().queueInLoop([log] { log->Mark("marker"); });
        synchronousAfterClose = Issue(*client, log, "after_close", kProcessId, ProcessRequest(3));
    });
    EXPECT_EQ(completionsAtClose, 0u);
    EXPECT_EQ(synchronousAfterClose, 0u);
    ASSERT_TRUE(log->WaitForCount(4));

    const auto entries = log->Entries();
    ASSERT_EQ(entries.size(), 4u);
    const std::set<std::string> closedTags{entries[0].tag, entries[1].tag};
    EXPECT_EQ(closedTags, (std::set<std::string>{"a", "b"})) << "the close batch must run before the marker";
    EXPECT_EQ(entries[0].status.Code(), RpcStatusCode::kChannelClosed) << entries[0].status.name();
    EXPECT_EQ(entries[1].status.Code(), RpcStatusCode::kChannelClosed) << entries[1].status.name();
    EXPECT_EQ(entries[2].tag, "marker");
    EXPECT_EQ(entries[3].tag, "after_close");
    EXPECT_EQ(entries[3].status.Code(), RpcStatusCode::kNotSent) << entries[3].status.name();
}

TEST_F(RpcChannelLoopback, ReentrantCallMethodFromTimeoutDone)
{
    ASSERT_NO_FATAL_FAILURE(ConnectRpcClient());
    auto log = std::make_shared<CallLog>();

    std::size_t completionsAfterSweep = 99;
    std::size_t outstandingAfterSweep = 99;
    RunInLoopAndWait([&] {
        const RpcChannelPtr channel = this->client->Channel();
        if (!channel)
        {
            ADD_FAILURE() << "client has no channel";
            return;
        }
        std::weak_ptr<RpcClient> weakClient = this->client;
        this->client->CallMethod(
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

    ASSERT_TRUE(serverRecorder.WaitForCount(2));
    const auto second = serverRecorder.Since(1).front();
    EXPECT_EQ(RequestSessionOf(second), 2u);
    RunInLoopAndWait([&] { SendFromServer(MakeResponseFrame(second.id(), kProcessId, ProcessResponse(2))); });
    ASSERT_TRUE(log->WaitForCount(2));

    const auto entries = log->Entries();
    ASSERT_EQ(entries.size(), 2u);
    EXPECT_EQ(entries[0].tag, "first");
    EXPECT_EQ(entries[0].status.Code(), RpcStatusCode::kTimeout) << entries[0].status.name();
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
        const RpcChannelPtr channel = (box && *box) ? (*box)->Channel() : RpcChannelPtr{};
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
        EXPECT_EQ(entry->status.Code(), RpcStatusCode::kTimeout) << tag << ": " << entry->status.name();
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
    EXPECT_EQ(entries[0].status.Code(), RpcStatusCode::kChannelClosed) << entries[0].status.name();
    EXPECT_EQ(entries[1].tag, "second");
    EXPECT_EQ(entries[1].status.Code(), RpcStatusCode::kNotSent) << entries[1].status.name();
    EXPECT_TRUE(entries[1].callMethodReturned) << "the re-entrant call completed inside its own CallMethod";
    EXPECT_TRUE(WaitUntil([&] { return weakClient.expired(); })) << "destroying the owner from the queued callback failed";
    EXPECT_TRUE(WaitUntil([this] { return serverDowns.load() == serverUps.load(); }))
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
        Issue(*client, log, "a", kProcessId, ProcessRequest(1));
        Issue(*client, log, "b", kProcessId, ProcessRequest(2));
        held = client->Channel();
        // ~RpcClient:连接 forceClose;它的 DOWN 回调只绑 weak_ptr<RpcClient>,lock 失败,channel 收不到 OnDisconnect。
        client.reset();
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
    serverMode = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    const auto from = probeRecorder.Count();
    const auto onewayBefore = Load(GetRpcChannelStats().onewaySent);

    bool sentEmpty = false;
    bool sentFilled = false;
    bool sentFailing = false;
    RunInLoopAndWait([&] {
        sentEmpty = probeChannel->SendRequest(kEnterId, PlayerEnterGameNodeRequest{});
        CreateSceneRequest filled;
        filled.set_config_id(1);   // 响应非空:以前「非空就回」的规则下这里会回包
        sentFilled = probeChannel->SendRequest(kCreateId, filled);
        sentFailing = probeChannel->SendRequest(kCancelId, CancelBattlePrepareRequest{});   // SetFailed:也不回 RPC_ERROR
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
        enterCalls = service.enterCalls;
        createCalls = service.createCalls;
        cancelCalls = service.cancelCalls;
    });
    EXPECT_EQ(enterCalls, 1);
    EXPECT_EQ(createCalls, 1);
    EXPECT_EQ(cancelCalls, 1);
}

TEST_F(RpcChannelLoopback, ServerRejectionsEchoTheCallId)
{
    serverMode = ServerMode::kChannel;
    // "Gate" 故意指向 Scene 服务对象:Gate 的方法名在 Scene 的描述符里找不到 → NO_METHOD。SceneScene 不注册 → NO_SERVICE。
    serverServices = {{"Scene", &service}, {"Gate", &service}};
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    const auto rejectedBefore = Load(GetRpcChannelStats().requestsRejected);
    const auto from = probeRecorder.Count();

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
    RunInLoopAndWait([&] { processCalls = service.processCalls; });
    EXPECT_EQ(processCalls, 1) << "only the barrier may reach the handler";
}

// id≠0:每个请求恰好一帧 —— Empty / 0 字节 / 早返回也回;SetFailed 与未实现方法回 HANDLER_FAILED;
// handler 同步 done->Run() 后框架不再回(调两次也只有一帧)。
TEST_F(RpcChannelLoopback, ServerRepliesExactlyOncePerCallId)
{
    serverMode = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    const auto runTwiceBefore = Load(GetRpcChannelStats().replyRunTwice);
    const auto from = probeRecorder.Count();

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
    serverMode = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    const auto abandonedBefore = Load(GetRpcChannelStats().replyAbandoned);
    const auto from = probeRecorder.Count();

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
    const auto secondFrom = probeRecorder.Count();
    std::size_t parked = 0;
    RunInLoopAndWait([&] {
        parked = service.parked.size();
        service.parked.clear();   // 在 loop 线程上释放全部副本
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
    serverMode = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    const auto from = probeRecorder.Count();

    RunInLoopAndWait([&] { SendFromProbe(MakeRequestFrame(kCraftedIdBase + 31, kPrepareId, PrepareBattleRequest{})); });
    ASSERT_TRUE(WaitUntil([this] { return service.parked.size() == 1; }));

    auto& stats = GetRpcChannelStats();
    const auto runTwiceBefore = Load(stats.replyRunTwice);
    const auto handlerFailedBefore = Load(stats.handlerFailed);
    bool completedBefore = true;
    bool completedAfter = false;
    uint64_t runTwiceAfterSecondRun = 0;
    RunInLoopAndWait([&] {
        const RpcDoneCallback reply = service.parked.front();
        service.parked.clear();
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
    serverMode = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    const auto from = probeRecorder.Count();

    RunInLoopAndWait([&] { SendFromProbe(MakeRequestFrame(kCraftedIdBase + 41, kPrepareId, PrepareBattleRequest{})); });
    ASSERT_TRUE(WaitUntil([this] { return service.parked.size() == 1; }));

    bool connectedBefore = false;
    RunInLoopAndWait([&] {
        connectedBefore = service.parked.front().IsCallerConnected();
        if (const auto conn = serverConn.lock())
        {
            conn->forceClose();
        }
    });
    EXPECT_TRUE(connectedBefore);
    ASSERT_TRUE(WaitUntil([this] { return serverChannel.expired(); }))
        << "the server-side channel survived DOWN (who still holds it strongly?)";

    const auto goneBefore = Load(GetRpcChannelStats().replyChannelGone);
    bool connectedAfter = true;
    bool completed = false;
    RunInLoopAndWait([&] {
        const RpcDoneCallback reply = service.parked.front();
        service.parked.clear();
        connectedAfter = reply.IsCallerConnected();
        reply.Run();
        completed = reply.IsCompleted();
    });
    EXPECT_FALSE(connectedAfter);
    EXPECT_TRUE(completed);
    EXPECT_EQ(Load(GetRpcChannelStats().replyChannelGone) - goneBefore, 1u);

    ASSERT_TRUE(WaitUntil([this] { return probeChannel == nullptr; })) << "probe never saw DOWN";
    for (const auto& frame : probeRecorder.Since(from))
    {
        EXPECT_NE(frame.id(), kCraftedIdBase + 41) << "a reply went out after the caller's channel was gone";
    }
}

// 接管的回复在 loop 线程之外释放最后一个副本(~EventLoop 丢弃闭包、thread_local 析构等任意栈的形状):
// 不发 REPLY_DROPPED、不碰 loop,只计 reply_channel_gone。连接仍在,所以「没有回包」用屏障证明。
TEST_F(RpcChannelLoopback, AdoptedReplyReleasedOffTheLoopSendsNothing)
{
    serverMode = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    const auto from = probeRecorder.Count();

    RunInLoopAndWait([&] { SendFromProbe(MakeRequestFrame(kCraftedIdBase + 61, kPrepareId, PrepareBattleRequest{})); });
    ASSERT_TRUE(WaitUntil([this] { return service.parked.size() == 1; }));

    auto& stats = GetRpcChannelStats();
    const auto goneBefore = Load(stats.replyChannelGone);
    const auto abandonedBefore = Load(stats.replyAbandoned);
    std::optional<RpcDoneCallback> held;   // RpcDoneCallback 没有默认构造,用 optional 把副本带出 loop 线程
    RunInLoopAndWait([&] {
        held.emplace(service.parked.front());
        service.parked.clear();
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
    serverMode = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());

    long before = -1;
    long after = -1;
    std::size_t parked = 0;
    RunInLoopAndWait([&] {
        const auto conn = serverConn.lock();
        const auto channel = serverChannel.lock();
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
        parked = service.parked.size();
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
    serverMode = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    auto log = std::make_shared<CallLog>();
    RunInLoopAndWait([&] { Issue(*probeChannel, log, "accepted", kPrepareId, PrepareBattleRequest{}); });
    ASSERT_TRUE(WaitUntil([this] { return service.parked.size() == 1; }));
    RunInLoopAndWait([&] {
        serverMaintenance->CloseSources();
        std::string frontier;
        EXPECT_FALSE(serverMaintenance->Frontier(true, frontier));
        EXPECT_FALSE(serverMaintenance->LocalCallbacksJoined());
        Issue(*probeChannel, log, "rejected", kProcessId, ProcessRequest(9));
        service.parked.front().Run();
        EXPECT_TRUE(serverMaintenance->LocalCallbacksJoined());
        EXPECT_TRUE(serverMaintenance->Frontier(true, frontier));
    });
    ASSERT_TRUE(log->WaitForCount(2));
    ASSERT_TRUE(log->Find("accepted").has_value());
    EXPECT_TRUE(log->Find("accepted")->status.ok());
    ASSERT_TRUE(log->Find("rejected").has_value());
    EXPECT_TRUE(log->Find("rejected")->status.DefinitelyNotExecuted());
    RunInLoopAndWait([&] { EXPECT_EQ(service.processCalls, 0); });
}

TEST_F(RpcChannelLoopback, MaintenanceOutputDoesNotJoinInsideTheBusinessCallback)
{
    serverMode = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    std::atomic<bool> callbackReturned{false};
    RunInLoopAndWait([&] {
        probeChannel->CallMethod(kProcessId, ProcessRequest(3),
            [state = probeMaintenance, &callbackReturned](const RpcStatus& status, const MessagePtr&) {
                EXPECT_TRUE(status.ok());
                state->CloseSources();
                state->ClosePublications();
                std::string frontier;
                EXPECT_FALSE(state->LocalCallbacksJoined());
                EXPECT_FALSE(state->Frontier(false, frontier));
                callbackReturned = true;
            });
    });
    ASSERT_TRUE(WaitUntil([&] { return callbackReturned.load() && probeMaintenance->LocalCallbacksJoined(); }));
    std::string frontier;
    EXPECT_TRUE(probeMaintenance->Frontier(false, frontier));
}

TEST_F(RpcChannelLoopback, MaintenanceTimeoutJoinsLocallyButKeepsStickyUnknown)
{
    serverMode = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    auto log = std::make_shared<CallLog>();
    RunInLoopAndWait([&] { Issue(*probeChannel, log, "unknown", kPrepareId, PrepareBattleRequest{}); });
    ASSERT_TRUE(WaitUntil([this] { return service.parked.size() == 1; }));
    RunInLoopAndWait([&] {
        probeMaintenance->CloseSources();
        probeMaintenance->ClosePublications();
        EXPECT_FALSE(probeMaintenance->LocalCallbacksJoined());
        probeChannel->SweepTimeouts(RpcChannel::Clock::now() + kLongTimeout + 1s);
    });
    ASSERT_TRUE(log->WaitForCount(1));
    ASSERT_TRUE(log->Find("unknown").has_value());
    EXPECT_TRUE(log->Find("unknown")->status.OutcomeUnknown());
    ASSERT_TRUE(WaitUntil([this] { return probeMaintenance->LocalCallbacksJoined(); }));
    std::string frontier;
    EXPECT_FALSE(probeMaintenance->Frontier(false, frontier));
    // 已关闭后的本地拒绝仍有排队回调；不能在它返回前拆业务对象。
    RunInLoopAndWait([&] {
        Issue(*probeChannel, log, "local_rejection", kProcessId, ProcessRequest(4));
        EXPECT_FALSE(probeMaintenance->LocalCallbacksJoined());
    });
    ASSERT_TRUE(log->WaitForCount(2));
    ASSERT_TRUE(WaitUntil([this] { return probeMaintenance->LocalCallbacksJoined(); }));
    EXPECT_FALSE(probeMaintenance->Frontier(false, frontier));
}


TEST_F(RpcChannelLoopback, AcknowledgedSendWaitsForActualAdoptedServerCompletion)
{
    ASSERT_TRUE(probeMaintenance->EnableAcknowledgedSends());
    serverMode = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    RunInLoopAndWait([&] { EXPECT_TRUE(probeChannel->SendRequest(kPrepareId, PrepareBattleRequest{})); });
    ASSERT_TRUE(WaitUntil([this] { return service.parked.size() == 1; }));
    RunInLoopAndWait([&] {
        probeMaintenance->CloseSources(); probeMaintenance->ClosePublications();
        EXPECT_FALSE(probeMaintenance->LocalCallbacksJoined());
        std::string frontier; EXPECT_FALSE(probeMaintenance->Frontier(false, frontier));
        service.parked.front().Run();
    });
    ASSERT_TRUE(WaitUntil([this] { return probeMaintenance->LocalCallbacksJoined(); }));
    std::string frontier; EXPECT_TRUE(probeMaintenance->Frontier(false, frontier));
}

TEST_F(RpcChannelLoopback, AcknowledgedSendRetainsServerFailureAsUnknown)
{
    ASSERT_TRUE(probeMaintenance->EnableAcknowledgedSends());
    serverMode = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    RunInLoopAndWait([&] {
        EXPECT_TRUE(probeChannel->SendRequest(kCancelId, CancelBattlePrepareRequest{}));
        probeMaintenance->CloseSources(); probeMaintenance->ClosePublications();
    });
    ASSERT_TRUE(WaitUntil([this] { return service.cancelCalls == 1 && probeMaintenance->LocalCallbacksJoined(); }));
    std::string frontier; EXPECT_FALSE(probeMaintenance->Frontier(false, frontier));
}

TEST_F(RpcChannelLoopback, AcknowledgedSendDisconnectDoesNotForgeRemoteCompletion)
{
    ASSERT_TRUE(probeMaintenance->EnableAcknowledgedSends());
    serverMode = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    RunInLoopAndWait([&] { EXPECT_TRUE(probeChannel->SendRequest(kPrepareId, PrepareBattleRequest{})); });
    ASSERT_TRUE(WaitUntil([this] { return service.parked.size() == 1; }));
    RunInLoopAndWait([&] {
        probeMaintenance->CloseSources(); probeMaintenance->ClosePublications();
        probe->connection()->forceClose();
    });
    ASSERT_TRUE(WaitUntil([this] { return probeMaintenance->LocalCallbacksJoined(); }));
    std::string frontier; EXPECT_FALSE(probeMaintenance->Frontier(false, frontier));
}

TEST_F(RpcChannelLoopback, AcknowledgedSendTimeoutDoesNotForgetAnAcceptedRequest)
{
    ASSERT_TRUE(probeMaintenance->EnableAcknowledgedSends());
    serverMode = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    RunInLoopAndWait([&] { EXPECT_TRUE(probeChannel->SendRequest(kPrepareId, PrepareBattleRequest{})); });
    ASSERT_TRUE(WaitUntil([this] { return service.parked.size() == 1; }));
    RunInLoopAndWait([&] {
        probeMaintenance->CloseSources(); probeMaintenance->ClosePublications();
        probeChannel->SweepTimeouts(RpcChannel::Clock::now() + kDefaultRpcCallTimeout + 1s);
    });
    ASSERT_TRUE(WaitUntil([this] { return probeMaintenance->LocalCallbacksJoined(); }));
    std::string frontier; EXPECT_FALSE(probeMaintenance->Frontier(false, frontier));
    RunInLoopAndWait([&] { service.parked.front().Run(); });
    PumpLoop(3);
    EXPECT_FALSE(probeMaintenance->Frontier(false, frontier));
}

TEST_F(RpcChannelLoopback, MaintenanceJoinIncludesCallbackTargetDestructionAndDerivedRejection)
{
    serverMode = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    auto callbacks = std::make_shared<std::atomic<int>>(0);
    RunInLoopAndWait([&] {
        const auto state = probeMaintenance;
        const std::weak_ptr<RpcChannel> weak = probeChannel;
        state->CloseSources(); state->ClosePublications();
        auto target = std::shared_ptr<int>(new int(0), [state, weak, callbacks](int* value) {
            delete value;
            EXPECT_FALSE(state->LocalCallbacksJoined()) << "target 析构仍可派生工作";
            if (const auto channel = weak.lock()) channel->CallMethod(kProcessId, ProcessRequest(9), [state, callbacks](const RpcStatus& status, const MessagePtr&) {
                EXPECT_TRUE(status.DefinitelyNotExecuted()); EXPECT_FALSE(state->LocalCallbacksJoined()); ++*callbacks;
            });
        });
        probeChannel->CallMethod(kProcessId, ProcessRequest(8), [target, callbacks](const RpcStatus& status, const MessagePtr&) {
            EXPECT_TRUE(status.DefinitelyNotExecuted()); ++*callbacks;
        });
    });
    ASSERT_TRUE(WaitUntil([&] { return callbacks->load() == 2 && probeMaintenance->LocalCallbacksJoined(); }));
    std::string frontier; EXPECT_TRUE(probeMaintenance->Frontier(false, frontier));
}

TEST_F(RpcChannelLoopback, MaintenanceSettledSnapshotKeepsExplicitRelayDispatchOpenUntilP3)
{
    serverMode = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    auto log=std::make_shared<CallLog>();
    RunInLoopAndWait([&]{std::string frontier;EXPECT_TRUE(serverMaintenance->SettledFrontier(frontier));Issue(*probeChannel,log,"relay-after-P1",kProcessId,ProcessRequest(1));});
    ASSERT_TRUE(log->WaitForCount(1)); ASSERT_TRUE(log->Find("relay-after-P1").has_value()); EXPECT_TRUE(log->Find("relay-after-P1")->status.ok());
    RunInLoopAndWait([&]{serverMaintenance->CloseSources();Issue(*probeChannel,log,"after-P3",kProcessId,ProcessRequest(2));});
    ASSERT_TRUE(log->WaitForCount(2)); ASSERT_TRUE(log->Find("after-P3").has_value()); EXPECT_TRUE(log->Find("after-P3")->status.DefinitelyNotExecuted());
    RunInLoopAndWait([&]{EXPECT_EQ(service.processCalls,1);});
}

TEST_F(RpcChannelLoopback, PreparedBootRejectsWireDispatchUntilAtomicOpenAndCannotReopen)
{
    ASSERT_TRUE(serverMaintenance->PrepareSources());
    serverMode=ServerMode::kChannel;ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    auto log=std::make_shared<CallLog>();
    RunInLoopAndWait([&]{Issue(*probeChannel,log,"prepared",kProcessId,ProcessRequest(1));});
    ASSERT_TRUE(log->WaitForCount(1));ASSERT_TRUE(log->Find("prepared").has_value());EXPECT_TRUE(log->Find("prepared")->status.DefinitelyNotExecuted());
    RunInLoopAndWait([&]{EXPECT_EQ(service.processCalls,0);EXPECT_TRUE(serverMaintenance->OpenPreparedSources());EXPECT_FALSE(serverMaintenance->OpenPreparedSources());Issue(*probeChannel,log,"opened",kProcessId,ProcessRequest(2));});
    ASSERT_TRUE(log->WaitForCount(2));ASSERT_TRUE(log->Find("opened").has_value());EXPECT_TRUE(log->Find("opened")->status.ok());
    RunInLoopAndWait([&]{EXPECT_EQ(service.processCalls,1);serverMaintenance->CloseSources();EXPECT_FALSE(serverMaintenance->PrepareSources());EXPECT_FALSE(serverMaintenance->OpenPreparedSources());Issue(*probeChannel,log,"closed",kProcessId,ProcessRequest(3));});
    ASSERT_TRUE(log->WaitForCount(3));ASSERT_TRUE(log->Find("closed").has_value());EXPECT_TRUE(log->Find("closed")->status.DefinitelyNotExecuted());
}

// ===========================================================================
// 五、链路追踪(docs/design/distributed-tracing.md §5.2)
// ===========================================================================

// 被测客户端:REQUEST 帧带发起时的 trace,parent 是这次调用的 client span(当前 span 的子 span,§10.3);
// 不在 trace 里的调用一个字段都不带;单向请求同样带,也有自己的 client span。
TEST_F(RpcChannelLoopback, RequestFramesCarryTheCallerTrace)
{
    ASSERT_NO_FATAL_FAILURE(ConnectRpcClient());   // kRecorder:看被测客户端发出的原始帧
    span_test::RecordingSink sink;
    const span_test::ScopedSinkInstall install(&sink);
    auto log = std::make_shared<CallLog>();
    const std::size_t from = serverRecorder.Count();
    tracing::TraceContext tracedCall;
    tracing::TraceContext tracedOneWay;
    RunInLoopAndWait([&] {
        {
            tracing::ScopedSpan span;
            tracedCall = tracing::Current();
            Issue(*client, log, "traced", kProcessId, ProcessRequest(1));
        }
        EXPECT_FALSE(tracing::Current().IsValid()) << "loop thread leaked a trace";
        Issue(*client, log, "untraced", kProcessId, ProcessRequest(2));
        {
            tracing::ScopedSpan span;
            tracedOneWay = tracing::Current();
            EXPECT_TRUE(client->SendRequest(kProcessId, ProcessRequest(3)));
        }
    });
    ASSERT_TRUE(serverRecorder.WaitForCount(from + 3));

    const auto frames = serverRecorder.Since(from);
    ASSERT_EQ(frames.size(), 3u) << Describe(frames);
    tracing::TraceContext seen;
    ASSERT_TRUE(ReadTraceFromRpcEnvelope(frames[0], seen)) << Describe(frames[0]);
    EXPECT_EQ(seen.trace_id.bytes, tracedCall.trace_id.bytes);
    EXPECT_NE(seen.span_id.bytes, tracedCall.span_id.bytes) << "线上的 parent 是 client span,不是发起方的 span";
    EXPECT_EQ(seen.flags, tracedCall.flags);
    const tracing::TraceContext callOnWire = seen;
    EXPECT_TRUE(HasNoTraceFields(frames[1])) << Describe(frames[1]);
    ASSERT_TRUE(ReadTraceFromRpcEnvelope(frames[2], seen)) << Describe(frames[2]);
    EXPECT_EQ(seen.trace_id.bytes, tracedOneWay.trace_id.bytes);
    EXPECT_NE(seen.span_id.bytes, tracedOneWay.span_id.bytes);
    const tracing::TraceContext oneWayOnWire = seen;

    // 单向请求的 client span 发出即结束;带回包的那条要等 Deliver,kRecorder 不回包,这里只核对单向那条。
    const auto spans = sink.Spans();
    const auto oneWaySpan = std::find_if(spans.begin(), spans.end(), [&](const tracing::SpanRecord& span) {
        return span.span_id.bytes == oneWayOnWire.span_id.bytes;
    });
    ASSERT_NE(oneWaySpan, spans.end()) << "单向请求记了 client span";
    EXPECT_EQ(oneWaySpan->kind, tracing::SpanKind::kClient);
    EXPECT_EQ(oneWaySpan->system, tracing::SpanSystem::kRpc0);
    EXPECT_EQ(oneWaySpan->parent_span_id.bytes, tracedOneWay.span_id.bytes);
    EXPECT_FALSE(oneWaySpan->Name().empty());
    EXPECT_TRUE(std::none_of(spans.begin(), spans.end(), [&](const tracing::SpanRecord& span) {
        return span.span_id.bytes == callOnWire.span_id.bytes;
    })) << "带回包的调用还没 Deliver,client span 不该已经结束";
}

// 一次调用在两端各记一个 span,连成一棵树:client span 的父是发起方的 span,对端 server span 的父是 client span;
// 回包回调原样回到发起方的上下文(§4.2、§10.3)。
TEST_F(RpcChannelLoopback, ClientAndServerSpansFormOneTreeAcrossTheConnection)
{
    serverMode = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectRpcClient());
    span_test::RecordingSink sink;
    const span_test::ScopedSinkInstall install(&sink);
    auto replyTrace = std::make_shared<std::optional<tracing::TraceContext>>();   // 只在 loop 线程上读写
    tracing::TraceContext caller;
    RunInLoopAndWait([&] {
        tracing::ScopedSpan span;
        caller = tracing::Current();
        this->client->CallMethod(
            kProcessId, ProcessRequest(12),
            [replyTrace](const RpcStatus& status, const MessagePtr&)
            {
                EXPECT_TRUE(status.ok()) << status.name();
                *replyTrace = tracing::Current();
            },
            RpcCallOptions{kLongTimeout});
    });
    ASSERT_TRUE(WaitUntil([&] { return replyTrace->has_value(); })) << "reply callback never ran";
    ASSERT_TRUE(WaitUntil([&] { return sink.Count() >= 2; }));

    tracing::TraceContext callbackTrace;
    RunInLoopAndWait([&] { callbackTrace = **replyTrace; });
    EXPECT_EQ(callbackTrace.span_id.bytes, caller.span_id.bytes) << "回包回调原样回到发起方的 span";

    const auto spans = sink.Spans();
    const auto client = std::find_if(spans.begin(), spans.end(),
                                     [](const tracing::SpanRecord& span) { return span.kind == tracing::SpanKind::kClient; });
    const auto server = std::find_if(spans.begin(), spans.end(),
                                     [](const tracing::SpanRecord& span) { return span.kind == tracing::SpanKind::kServer; });
    ASSERT_NE(client, spans.end());
    ASSERT_NE(server, spans.end());
    EXPECT_EQ(client->trace_id.bytes, caller.trace_id.bytes);
    EXPECT_EQ(server->trace_id.bytes, caller.trace_id.bytes);
    EXPECT_EQ(client->parent_span_id.bytes, caller.span_id.bytes);
    EXPECT_FALSE(client->parent_is_remote);
    EXPECT_EQ(server->parent_span_id.bytes, client->span_id.bytes);
    EXPECT_TRUE(server->parent_is_remote);
    EXPECT_EQ(client->Name(), server->Name()) << "两端同一个方法名";
    EXPECT_EQ(client->status, tracing::SpanStatus::kUnset);
    EXPECT_EQ(server->status, tracing::SpanStatus::kUnset);
    EXPECT_LE(client->start_unix_nano, server->start_unix_nano);
}

// 派发前的拒绝把这次派发的 server span 标成错误,说明是错误码名(§10.3)。
TEST_F(RpcChannelLoopback, RejectedDispatchMarksTheServerSpanAsAnError)
{
    serverMode = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    span_test::RecordingSink sink;
    const span_test::ScopedSinkInstall install(&sink);
    const auto emptySlot = FindEmptyRegistrySlot();
    ASSERT_TRUE(emptySlot.has_value()) << "registry has no empty slot to craft a rejected request";
    const tracing::TraceContext upstream = tracing::NewRoot();
    GameRpcMessage rejected = MakeRequestFrame(kCraftedIdBase + 31, *emptySlot, ProcessRequest(31));
    WriteTraceToRpcEnvelope(upstream, rejected);
    const std::size_t from = probeRecorder.Count();
    RunInLoopAndWait([&] { SendFromProbe(rejected); });
    const auto replies = FramesBeforeBarrier(from, kCraftedIdBase + 32);
    ASSERT_EQ(replies.size(), 1u) << Describe(replies);
    EXPECT_EQ(replies[0].type(), RPC_ERROR) << Describe(replies[0]);

    const auto spans = sink.Spans();
    const auto server = std::find_if(spans.begin(), spans.end(), [&](const tracing::SpanRecord& span) {
        return span.kind == tracing::SpanKind::kServer && span.trace_id.bytes == upstream.trace_id.bytes;
    });
    ASSERT_NE(server, spans.end());
    EXPECT_EQ(server->status, tracing::SpanStatus::kError);
    EXPECT_FALSE(server->StatusMessage().empty());
    EXPECT_EQ(server->parent_span_id.bytes, upstream.span_id.bytes);
}

// handler Adopt 之后延后回复(§10.3):server span 跟着完成句柄走,量到回复发出为止,派发返回时不交出;
// 没 Run / Fail 就被丢掉的句柄同样结束 server span,标 REPLY_DROPPED。
TEST_F(RpcChannelLoopback, AdoptedReplyEndsTheServerSpanWhenTheReplyIsSent)
{
    serverMode = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectRpcClient());
    span_test::RecordingSink sink;
    const span_test::ScopedSinkInstall install(&sink);
    auto log = std::make_shared<CallLog>();
    tracing::TraceContext caller;
    RunInLoopAndWait([&] {
        tracing::ScopedSpan span;
        caller = tracing::Current();
        Issue(*client, log, "parked", kPrepareId, PrepareBattleRequest{});   // battle_node_id 为 0:存一个副本
        PrepareBattleRequest drop;
        drop.set_battle_id(kDropAdoptedReply);
        Issue(*client, log, "dropped", kPrepareId, drop);
    });
    const auto serverSpans = [&] {
        std::vector<tracing::SpanRecord> found;
        for (const auto& span : sink.Spans())
        {
            if (span.kind == tracing::SpanKind::kServer && span.trace_id.bytes == caller.trace_id.bytes)
            {
                found.push_back(span);
            }
        }
        return found;
    };
    ASSERT_TRUE(log->WaitForCount(1));
    ASSERT_TRUE(WaitUntil([this] { return service.parked.size() == 1; }));
    ASSERT_TRUE(WaitUntil([&] { return serverSpans().size() == 1; }));
    {
        const auto spans = serverSpans();
        EXPECT_EQ(spans[0].status, tracing::SpanStatus::kError);
        EXPECT_EQ(spans[0].StatusMessage(), "REPLY_DROPPED") << "丢掉的句柄结束 server span 并标错";
    }
    std::this_thread::sleep_for(50ms);
    EXPECT_EQ(serverSpans().size(), 1u) << "存着的那一个还没回复,它的 server span 不该结束";

    RunInLoopAndWait([this] {
        service.parked.front().Run();
        service.parked.clear();
    });
    ASSERT_TRUE(log->WaitForCount(2));
    ASSERT_TRUE(WaitUntil([&] { return serverSpans().size() == 2; }));
    const auto spans = serverSpans();
    const auto replied = std::find_if(spans.begin(), spans.end(),
                                      [](const tracing::SpanRecord& span) { return span.status == tracing::SpanStatus::kUnset; });
    ASSERT_NE(replied, spans.end()) << "Run 发出回复时结束,状态正常";
    EXPECT_TRUE(replied->parent_is_remote);
    EXPECT_GE(replied->end_unix_nano - replied->start_unix_nano, 50'000'000u) << "量到延后的回复,含上面等的 50ms";
    const auto all = sink.Spans();
    EXPECT_TRUE(std::any_of(all.begin(), all.end(), [&](const tracing::SpanRecord& span) {
        return span.kind == tracing::SpanKind::kClient && span.span_id.bytes == replied->parent_span_id.bytes &&
               span.parent_span_id.bytes == caller.span_id.bytes;
    })) << "父是这次调用的 client span";
}

// 本地确定没发出的调用同样记一个 client span 并标错,说明是原因(§10.3:调用被尝试了就有 span):
// 没有 channel 的 RpcClient、不合法的超时、被拒的单向请求各一个。对端一帧都没收到,所以没有 server span。
TEST_F(RpcChannelLoopback, LocalFailuresRecordAnErrorClientSpan)
{
    ASSERT_NO_FATAL_FAILURE(ConnectRpcClient());
    span_test::RecordingSink sink;
    const span_test::ScopedSinkInstall install(&sink);
    auto log = std::make_shared<CallLog>();
    RpcClientPtr idle;
    tracing::TraceContext caller;
    bool onewayAccepted = true;
    RunInLoopAndWait([&] {
        tracing::ScopedSpan span;
        caller = tracing::Current();
        idle = std::make_shared<RpcClient>(&Loop(), serverAddr);   // 没调 connect:没有 channel
        Issue(*idle, log, "not_connected", kProcessId, ProcessRequest(1));
        Issue(*client, log, "zero_timeout", kProcessId, ProcessRequest(2), std::chrono::milliseconds::zero());
        onewayAccepted = client->SendRequest(kProcessId, CreateSceneRequest{});
    });
    ASSERT_TRUE(log->WaitForCount(2));
    EXPECT_FALSE(onewayAccepted);
    RunInLoopAndWait([&] { idle.reset(); });

    std::vector<std::string> reasons;
    for (const auto& span : sink.Spans())
    {
        if (span.trace_id.bytes != caller.trace_id.bytes)
        {
            continue;
        }
        EXPECT_EQ(span.kind, tracing::SpanKind::kClient);
        EXPECT_EQ(span.system, tracing::SpanSystem::kRpc0);
        EXPECT_EQ(span.status, tracing::SpanStatus::kError);
        EXPECT_EQ(span.parent_span_id.bytes, caller.span_id.bytes);
        reasons.emplace_back(span.StatusMessage());
    }
    std::sort(reasons.begin(), reasons.end());
    EXPECT_EQ(reasons, (std::vector<std::string>{"link not ready", "non-positive timeout",
                                                 "request type does not match registry"}));
}

// 端到端:对端 handler 接上同一条 trace(子 span),回包回调回到发起方的 trace。
TEST_F(RpcChannelLoopback, ServerHandlerAndReplyCallbackStayOnTheCallerTrace)
{
    serverMode = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectRpcClient());
    auto replyTrace = std::make_shared<std::optional<tracing::TraceContext>>();   // 只在 loop 线程上读写
    tracing::TraceContext caller;
    RunInLoopAndWait([&] {
        tracing::ScopedSpan span;
        caller = tracing::Current();
        client->CallMethod(
            kProcessId, ProcessRequest(11),
            [replyTrace](const RpcStatus& status, const MessagePtr&)
            {
                EXPECT_TRUE(status.ok()) << status.name();
                *replyTrace = tracing::Current();
            },
            RpcCallOptions{kLongTimeout});
    });
    ASSERT_TRUE(WaitUntil([&] { return replyTrace->has_value(); })) << "reply callback never ran";

    std::optional<tracing::TraceContext> handlerTrace;
    tracing::TraceContext callbackTrace;
    RunInLoopAndWait([&] {
        handlerTrace = service.ProcessTraceOf(11);
        callbackTrace = **replyTrace;
    });
    ASSERT_TRUE(handlerTrace.has_value());
    EXPECT_EQ(handlerTrace->trace_id.bytes, caller.trace_id.bytes);   // 跨连接接上同一条 trace
    EXPECT_NE(handlerTrace->span_id.bytes, caller.span_id.bytes);     // 对端在它下面建了子 span
    EXPECT_EQ(callbackTrace.trace_id.bytes, caller.trace_id.bytes);   // 回包回调恢复发起方的 trace
}

// 被测服务端:合法的信封 trace 被接续;trace 不完整(trace_id 有、span 为 0)开新根且照常派发;
// RESPONSE 与 RPC_ERROR 都不带 trace 字段。
TEST_F(RpcChannelLoopback, ServerContinuesAValidEnvelopeTraceAndRootsAnInvalidOne)
{
    serverMode = ServerMode::kChannel;
    ASSERT_NO_FATAL_FAILURE(ConnectProbe());
    const auto emptySlot = FindEmptyRegistrySlot();
    ASSERT_TRUE(emptySlot.has_value()) << "registry has no empty slot to craft a rejected request";

    const tracing::TraceContext upstream = tracing::NewRoot();
    GameRpcMessage valid = MakeRequestFrame(kCraftedIdBase + 1, kProcessId, ProcessRequest(21));
    WriteTraceToRpcEnvelope(upstream, valid);
    GameRpcMessage spanless = MakeRequestFrame(kCraftedIdBase + 2, kProcessId, ProcessRequest(22));
    spanless.set_trace_id_hi(valid.trace_id_hi());
    spanless.set_trace_id_lo(valid.trace_id_lo());
    spanless.set_trace_flags(valid.trace_flags());
    GameRpcMessage rejected = MakeRequestFrame(kCraftedIdBase + 3, *emptySlot, ProcessRequest(23));
    WriteTraceToRpcEnvelope(upstream, rejected);

    const std::size_t from = probeRecorder.Count();
    RunInLoopAndWait([&] {
        SendFromProbe(valid);
        SendFromProbe(spanless);
        SendFromProbe(rejected);
    });
    const auto replies = FramesBeforeBarrier(from, kCraftedIdBase + 4);
    ASSERT_EQ(replies.size(), 3u) << Describe(replies);
    EXPECT_EQ(replies[0].type(), RESPONSE) << Describe(replies[0]);
    EXPECT_EQ(replies[1].type(), RESPONSE) << Describe(replies[1]);   // 不合法的 trace 不影响派发
    EXPECT_EQ(replies[2].type(), RPC_ERROR) << Describe(replies[2]);
    for (const auto& reply : replies)
    {
        EXPECT_TRUE(HasNoTraceFields(reply)) << Describe(reply);
    }

    std::optional<tracing::TraceContext> continued;
    std::optional<tracing::TraceContext> rooted;
    RunInLoopAndWait([&] {
        continued = service.ProcessTraceOf(21);
        rooted = service.ProcessTraceOf(22);
    });
    ASSERT_TRUE(continued.has_value());
    ASSERT_TRUE(rooted.has_value());
    EXPECT_EQ(continued->trace_id.bytes, upstream.trace_id.bytes);
    EXPECT_NE(continued->span_id.bytes, upstream.span_id.bytes);
    EXPECT_TRUE(rooted->IsValid());
    EXPECT_NE(rooted->trace_id.bytes, upstream.trace_id.bytes);   // 开了新根,没有沿用不完整的 trace
}
