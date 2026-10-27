#pragma once

// 节点间 RPC0 的双向端点:muduo 原版 RpcChannel 的本仓实现(docs/design/muduo-rpc-channel.md)。
//
// 一条连接一个 RpcChannel,同一个对象既发起调用(CallMethod / SendRequest)也派发对端的请求(CallServiceMethod),
// 与 muduo 原版一致。原版缺的四样在这里补齐:超时、断连 / 销毁 / 关机时让在途调用以失败完成、把对端错误交给调用方、
// 生命周期全部只用 weak。
//
// 线程:所有公开入口只在构造时给定的 EventLoop 线程调用(assertInLoopThread,Release 下同样 LOG_FATAL)。
// 不加锁 —— 原版的 MutexLock / AtomicInt64 是为跨线程 CallMethod 准备的,本仓的节点只有一个 EventLoop。
//
// 生命周期(AGENTS §11.7):必须由 shared_ptr 持有(Create);超时 tick 与连接的消息回调只绑 weak_ptr<RpcChannel>;
// 只存 weak_ptr<TcpConnection>。所有者:入站是连接的 context(RpcServer),出站是 RpcClient(每次连上新建一个)。

#include <chrono>
#include <cstdint>
#include <functional>
#include <map>
#include <memory>
#include <mutex>
#include <string>
#include <type_traits>
#include <utility>
#include <vector>

#include <google/protobuf/descriptor.h>
#include <google/protobuf/message.h>
#include <google/protobuf/service.h>

#include "muduo/net/protobuf/ProtobufCodecLite.h"
#include "muduo/net/TcpConnection.h"
#include "network/codec/dispatcher.h"
#include "network/rpc_done_callback.h"
#include "network/rpc_status.h"
#include "proto/common/base/rpc_message.pb.h"
#include "thread_context/trace_context_tls.h"
#include "utils/data_structures/inflight_index.h"

namespace muduo::net
{
class EventLoop;
}

using RpcMessagePtr = std::shared_ptr<GameRpcMessage>;

// Protocol tag
constexpr char RPC_TAG[] = "RPC0";

// Wire format:
// size      : 4-byte  (N+8) total message length
// RPC_TAG   : 4-byte  fixed tag "RPC0"
// payload   : N-byte  serialized Protobuf data
// checksum  : 4-byte  Adler-32 over "RPC0" + payload
using RpcCodec = muduo::net::ProtobufCodecLiteT<GameRpcMessage, RPC_TAG>;

using muduo::net::TcpConnectionPtr;
using ProtobufService = ::google::protobuf::Service;
using ProtobufMessage = ::google::protobuf::Message;
// 服务表:service full_name → 服务对象。服务对象由节点长期持有,channel 自持这张表的拷贝(不借用外部 map)。
using RpcServiceMap = std::map<std::string, ProtobufService*>;

// Temporary debug: set gDumpOversizedRpc = true to dump full oversized RPC bodies to logs/oversized_rpc.log
extern bool gDumpOversizedRpc;
void EnableOversizedRpcDump(bool enable);

// 消息号能否派发:不越界,且注册表里该槽有服务名与方法名。空槽(如 DB 域被跳过的号)与越界号一律不可派发 ——
// 以前只查越界,空槽会拿 nullptr 构造 std::string(UB,任何能连上节点间端口的进程发一帧就能打崩节点)。
bool IsDispatchableMessageId(uint32_t messageId);

// 客户端完成回调(muduo-protorpc cpp11 的 ClientDoneCallback,多一个 RpcStatus:原版无法表达失败)。
//   (a) response != nullptr ⇔ status.ok();失败路径不得解引用 response。
//   (b) 只在 loop 线程执行;只捕获值与 weak(session_id、incarnation、实体 id、weak_ptr<RpcClient>),执行时重新查找校验;
//       不捕获 RpcChannelPtr / TcpConnectionPtr / controller / request、response 裸指针 / 可能先析构的对象的 this。
//   (c) RESPONSE / RPC_ERROR 的完成在该连接的 I/O 回调栈上同步执行(保持同一连接内回包与后续请求的处理顺序);
//       在其中不得同步销毁本 channel 所属的 RpcClient 或摘节点 —— 需要时 queueInLoop(与 handler 侧规则一致)。
//   (d) OutcomeUnknown() 的失败 = 结果未知(对端可能已执行),只释放本地在途状态、提示客户端、重试幂等操作。
//   (e) 「恰好一次」的唯一例外:EventLoop 已退出后才析构的 channel,在途回调随容器销毁而不调用。
//       所以不得把资源释放只挂在 done 上;gRPC sync handler 也不得用 future 阻塞等待一次 RPC0 调用的结果。
using ClientDoneCallback = std::function<void(const RpcStatus& status, const MessagePtr& response)>;

// 与 gRPC 默认 deadline(grpc_client_call.h kDefaultGrpcCallDeadlineMs)同值。调用点应按预算链显式传值(§4.5)。
inline constexpr std::chrono::milliseconds kDefaultRpcCallTimeout{10000};
// 每个 channel 的在途上限,与 muduo_grpc::ChannelOptions::maxOutstandingCalls 同值;超出以 kNotSent 失败。
inline constexpr std::size_t kMaxOutstandingRpcCalls = 65536;

struct RpcCallOptions
{
    // 必须 > 0;没有「不限时」。精度约 ±100ms(超时 tick 的间隔)。
    std::chrono::milliseconds timeout{kDefaultRpcCallTimeout};
};

struct RpcMaintenanceOperation;

// 进程内运行账本，不是持久化协议；测试可显式注入独立域，生产所有通道默认共用同一域。
class RpcMaintenanceState {
public:
    // 仅显式维护装配在首次发布前启用；既有默认 id0/noreturn 契约不变。
    bool EnableAcknowledgedSends();
    bool AcknowledgedSends() const;
    bool PrepareSources(); // 新boot在首次业务接收前关闭，P9可且只能开放一次。
    bool OpenPreparedSources();
    bool Begin(bool source);
    void Complete(bool source, bool known);
    void CloseSources();
    void ClosePublications();
    // 本机回调已退出与远端结果确定是两个问题；UNKNOWN 不阻止安全析构，但禁止永久闭合收据。
    bool LocalCallbacksJoined() const;
    bool BeginLocalCompletion();
    void CompleteLocalCompletion(bool delivered);
    bool Frontier(bool sourcesOnly, std::string& value) const;
    // P1只读取已知终局，不封仍承接上游accepted工作的RELAY。
    bool SettledFrontier(std::string& value) const;
private:
    mutable std::mutex mutex_;
    std::uint64_t sourceAccepted_ = 0, sourceCompleted_ = 0;
    std::uint64_t outputAccepted_ = 0, outputCompleted_ = 0;
    bool sourcesClosed_ = false, publicationsClosed_ = false;
    bool acknowledgedSends_ = false, sourcesPrepared_ = false, preparedOpened_ = false;
    bool sourceUnknown_ = false, outputUnknown_ = false;
    std::uint64_t localAccepted_ = 0, localCompleted_ = 0;
};

class RpcChannel final : public std::enable_shared_from_this<RpcChannel>
{
    struct PrivateTag
    {
    };

public:
    using Clock = std::chrono::steady_clock;
    static std::shared_ptr<RpcMaintenanceState> ProcessMaintenance();

    static std::shared_ptr<RpcChannel> Create(muduo::net::EventLoop& loop,
                                              const TcpConnectionPtr& connection,
                                              RpcServiceMap services,
                                              std::shared_ptr<RpcMaintenanceState> maintenance = ProcessMaintenance());

    RpcChannel(PrivateTag, muduo::net::EventLoop& loop, const TcpConnectionPtr& connection, RpcServiceMap services,
               std::shared_ptr<RpcMaintenanceState> maintenance = ProcessMaintenance());
    ~RpcChannel();

    RpcChannel(const RpcChannel&) = delete;
    RpcChannel& operator=(const RpcChannel&) = delete;

    // ---- 客户端 ----

    // 发起需要应答的调用(信封 id != 0,对端必回)。done 恰好被调用一次,并且**永不**在 CallMethod 返回之前被调用:
    // 本地失败(未连接 / 已关闭 / 在途已满 / 消息号不可派发 / 请求类型与注册表不符 / 序列化失败)也经 queueInLoop 以 kNotSent 投递,
    // 调用方只需写一条失败路径,没有重入。与 muduo_grpc::Channel 的契约一致。
    void CallMethod(uint32_t messageId, const ProtobufMessage& request, ClientDoneCallback done,
                    const RpcCallOptions& options = {});

    // 类型安全的入口:Method 是生成器产出的方法 tag(`<Svc><Method>Rpc`,含 kMessageId / Request / Response)。
    // Done: void(const RpcStatus&, const std::shared_ptr<typename Method::Response>&),必须可拷贝。
    template <typename Method, typename Done>
    void CallMethod(const typename Method::Request& request, Done&& done, const RpcCallOptions& options = {})
    {
        using Response = typename Method::Response;
        static_assert(std::is_copy_constructible_v<std::decay_t<Done>>,
                      "RpcChannel::CallMethod: done must be copy-constructible (stored in std::function)");
        CallMethodWithResponseType(
            Method::kMessageId, request, Response::descriptor(),
            [typedDone = std::forward<Done>(done)](const RpcStatus& status, const MessagePtr& response) mutable
            {
                typedDone(status, std::static_pointer_cast<Response>(response));
            },
            options);
    }

    // 单向(信封 id == 0,muduo-protorpc 的 noreturn):调用方声明不要应答,对端不回任何帧。按次决定,不按方法。
    // 返回是否已交给连接(之后不保证送达);失败内部计数并采样记录,推送方拿到 false 一般也无事可做。
    bool SendRequest(uint32_t messageId, const ProtobufMessage& request);

    template <typename Method>
    bool SendRequest(const typename Method::Request& request)
    {
        return SendRequest(Method::kMessageId, request);
    }

    // ---- 传输 ----

    // 字节流入口(muduo onMessage):只能由本 channel 所绑连接的消息回调调用。
    void OnMessage(const TcpConnectionPtr& connection, muduo::net::Buffer* buffer, muduo::Timestamp receiveTime);

    // 连接 DOWN:在途调用全部以 kDisconnected 失败(回调 queueInLoop 交付,避开连接回调栈与 registry 遍历栈)。
    void OnDisconnect();

    // 关闭:之后 CallMethod 一律 kNotSent;在途调用以 kChannelClosed 失败(queueInLoop 交付)。
    // 节点关机阶段 0 调用(NodeUtils::CloseAllRpcChannels)。关闭不影响服务端:延后的回复仍可在连接存活时发出。
    void Close();

    bool IsClosed() const { return closed_; }
    // 所绑连接仍处于已连接态。
    bool IsConnected() const;
    bool IsBoundTo(const TcpConnectionPtr& connection) const;
    std::size_t OutstandingCount() const { return outstandings_.Size(); }
    muduo::net::EventLoop& GetLoop() const { return loop_; }

    // 超时扫描:把截止时刻 <= now 的在途调用以 kTimeout 完成。超时 tick 每 100ms 调一次;也是单测接缝(loop 线程调用)。
    void SweepTimeouts(Clock::time_point now);

    // 一次「本地确定没发出」的调用:计入 started、采样记录原因,在 loop 上以 kNotSent 投递给 done
    // (恰好一次、不同步执行)。RpcClient / RpcSession 当前没有可用 channel 时也走这里。
    static void FailNotSent(muduo::net::EventLoop& loop, uint32_t messageId, ClientDoneCallback done,
                            const char* reason, const std::string& peer,
                            std::shared_ptr<RpcMaintenanceState> maintenance = ProcessMaintenance());

private:
    friend class RpcDoneCallback;
    friend struct RpcDoneCallback::State;
    class DoneClosure;   // 框架交给 handler 的 done(rpc_channel.cpp)

    struct OutstandingCall
    {
        InflightSteadyTime expiresAt;   // InflightIndex 要求的成员
        uint32_t messageId{0};
        std::shared_ptr<RpcMaintenanceOperation> maintenance;
        ClientDoneCallback done;
        tracing::TraceContext trace;    // 发起时的 trace,回调前恢复成它的子 span
    };
    using OutstandingBatch = std::vector<std::pair<InflightCallId, OutstandingCall>>;

    void CallMethodWithResponseType(uint32_t messageId, const ProtobufMessage& request,
                                    const ::google::protobuf::Descriptor* expectedResponse,
                                    ClientDoneCallback done, const RpcCallOptions& options);

    // 解码后分发(muduo onRpcMessage)。
    void OnRpcMessage(const TcpConnectionPtr& connection, const RpcMessagePtr& message, muduo::Timestamp receiveTime);
    void OnResponse(const GameRpcMessage& message);
    void OnRpcError(const GameRpcMessage& message);

    // 服务端:查表并调用 handler(muduo-protorpc callServiceMethod)。
    void CallServiceMethod(const TcpConnectionPtr& connection, const GameRpcMessage& message);
    void RejectRequest(const GameRpcMessage& request, GameErrorCode error);

    // 按原 id 回包(muduo doneCallback)。id == 0(单向)什么都不发。response 为空且 error == RPC_NO_ERROR 时回 0 字节响应。
    bool DoneCallback(uint64_t id, uint32_t messageId, const ProtobufMessage* response, GameErrorCode error);

    // 延后完成:RpcDoneCallback::Run / Fail 与 DoneClosure::Run 共用,只有第一次生效。
    static void CompleteDeferred(RpcDoneCallback::State& state, GameErrorCode error);

    // 整表摘出,经 queueInLoop 以 status 批量失败。
    void FailAll(RpcStatus status);
    static void FailSoon(muduo::net::EventLoop& loop, ClientDoneCallback done, RpcStatus status,
                         std::shared_ptr<RpcMaintenanceState> maintenance);
    static void Deliver(OutstandingCall& call, const RpcStatus& status, const MessagePtr& response);

    void ArmTimeoutTick();
    void OnTimeoutTick();

    void SendFrame(const TcpConnectionPtr& connection, const GameRpcMessage& message);
    TcpConnectionPtr LockConnectedConnection() const;

    std::shared_ptr<RpcMaintenanceState> maintenance_;
    muduo::net::EventLoop& loop_;
    std::weak_ptr<muduo::net::TcpConnection> connection_;
    RpcServiceMap services_;
    // 解码回调绑 this:codec_ 是本对象的成员,寿命相同(§11.7 允许的例外)。
    RpcCodec codec_;
    InflightCallId id_{kNoInflightCallId};
    InflightIndex<OutstandingCall> outstandings_;
    bool timeoutTickArmed_{false};
    bool closed_{false};
};

using RpcChannelPtr = std::shared_ptr<RpcChannel>;
