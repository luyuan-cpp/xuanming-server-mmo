#include "rpc_channel.h"

#include <algorithm>
#include <cassert>
#include <cstdio>
#include <ctime>
#include <sstream>
#include <limits>

#include "muduo/base/Logging.h"
#include "muduo/net/EventLoop.h"
#include "muduo/net/TcpConnection.h"

#include "error_reporter/error_reporter.h"
#include "network/rpc_channel_stats.h"
#include "network/rpc_controller.h"
#include "network/rpc_done_callback.h"
#include "network/traffic_statistics.h"
#include "rpc/service_metadata/rpc_event_registry.h"

using namespace std::placeholders;

// ====================== 消息体积告警(沿用) ======================

namespace
{
// Threshold for the "large RPC" dump. Set above the legitimate enter-scene
// AOI batch (SceneSceneClientPlayerNotifyActorListCreate, which can be ~2-3KB
// for a populated grid) so it does not trigger on normal traffic. A real
// concern is messages in the tens of KB.
constexpr size_t kMaxMessageByteSize = 4096;

// 超时 tick 的间隔:超时精度约 ±100ms,RPC 超时是秒级(§4.5)。
constexpr double kTimeoutTickSeconds = 0.1;

uint64_t Bump(std::atomic<uint64_t> &counter, uint64_t delta = 1)
{
    return counter.fetch_add(delta, std::memory_order_relaxed) + delta;
}

const char *MethodNameOf(uint32_t messageId)
{
    return IsDispatchableMessageId(messageId) ? gRpcMethodRegistry[messageId].methodName : "<unknown_method>";
}

std::string PeerOf(const std::weak_ptr<muduo::net::TcpConnection> &connection)
{
    const auto locked = connection.lock();
    return locked ? locked->peerAddress().toIpPort() : std::string("<gone>");
}

bool SerializeMessage(const ProtobufMessage &message, std::string *output)
{
    output->resize(message.ByteSizeLong());
    return message.SerializePartialToArray(output->data(), static_cast<int32_t>(output->size()));
}

size_t LogIfMessageTooLarge(const GameRpcMessage &rpcMessage)
{
    const size_t messageSize = rpcMessage.ByteSizeLong();
    if (messageSize <= kMaxMessageByteSize)
        return messageSize;

    const auto msgId = rpcMessage.message_id();
    const char *methodName = "(unknown)";
    std::string decodedBody;

    if (msgId < gRpcMethodRegistry.size())
    {
        const auto &meta = gRpcMethodRegistry[msgId];

        if (meta.methodName)
            methodName = meta.methodName;

        const bool isRequest = !rpcMessage.request().empty();
        const auto &body = isRequest ? rpcMessage.request() : rpcMessage.response();
        const auto *proto = isRequest ? meta.requestProto.get() : meta.responseProto.get();

        if (proto && !body.empty())
        {
            std::unique_ptr<::google::protobuf::Message> decoded(proto->New());
            if (decoded->ParsePartialFromArray(body.data(), static_cast<int32_t>(body.size())))
                decodedBody = decoded->ShortDebugString();
        }
    }

    if (gDumpOversizedRpc && !decodedBody.empty())
    {
        FILE *f = fopen("logs/oversized_rpc.log", "a");
        if (f)
        {
            std::time_t now = std::time(nullptr);
            char timeBuf[32];
            std::strftime(timeBuf, sizeof(timeBuf), "%Y-%m-%d %H:%M:%S", std::localtime(&now));
            fprintf(f, "[%s] method: %s, msg_id: %u, size: %zu, body(%zu): ",
                    timeBuf, methodName, msgId, messageSize, decodedBody.size());
            fwrite(decodedBody.data(), 1, decodedBody.size(), f);
            fputc('\n', f);
            fclose(f);
        }
    }

    // muduo log: short summary only (safe within 4000-byte buffer).
    // WARN, not ERROR -- a few KB is informational, not a fault. Real anomalies
    // are in the tens of KB and stand out either way.
    LOG_WARN << "RPC message size exceeds threshold (" << kMaxMessageByteSize << " bytes)"
             << ", method: " << methodName
             << ", message ID: " << msgId
             << ", size: " << messageSize;

    return messageSize;
}

// 一次派发的 trace:新根,退出时恢复派发前的 trace。以前直接 tracing::Set(NewRoot()) 且从不恢复,
// TLS 里的 trace 会一直留到下一次派发,之后在同一线程上跑的回包回调、超时回调都会挂到它下面。
class ScopedDispatchTrace
{
public:
    ScopedDispatchTrace() : previous_(tracing::Current()) { tracing::Set(tracing::NewRoot()); }
    ~ScopedDispatchTrace() { tracing::Set(previous_); }

    ScopedDispatchTrace(const ScopedDispatchTrace &) = delete;
    ScopedDispatchTrace &operator=(const ScopedDispatchTrace &) = delete;

private:
    tracing::TraceContext previous_;
};

void RecordOutcome(const RpcStatus &status)
{
    auto &stats = GetRpcChannelStats();
    switch (status.code())
    {
    case RpcStatusCode::kOk:
        Bump(stats.ok);
        break;
    case RpcStatusCode::kNotSent:
        Bump(stats.notSent);
        break;
    case RpcStatusCode::kRemoteRejected:
        Bump(stats.remoteRejected);
        break;
    case RpcStatusCode::kRemoteFailed:
        Bump(stats.remoteFailed);
        break;
    case RpcStatusCode::kReplyDropped:
        Bump(stats.replyDropped);
        break;
    case RpcStatusCode::kTimeout:
        Bump(stats.timeout);
        break;
    case RpcStatusCode::kDisconnected:
        Bump(stats.disconnected);
        break;
    case RpcStatusCode::kChannelClosed:
        Bump(stats.channelClosed);
        break;
    case RpcStatusCode::kInvalidResponse:
        Bump(stats.invalidResponse);
        break;
    case RpcStatusCode::kCount:
        break;
    }
}
} // namespace

// Temporary debug switch: set to true at runtime to dump all oversized RPC bodies to file.
// Usage: set gDumpOversizedRpc = true in debugger, or call EnableOversizedRpcDump(true).
// Default OFF -- enter-scene AOI batches and similar legitimately-large traffic
// would otherwise fill the dump file under load.
bool gDumpOversizedRpc = false;

void EnableOversizedRpcDump(bool enable) { gDumpOversizedRpc = enable; }

bool IsDispatchableMessageId(uint32_t messageId)
{
    if (messageId >= gRpcMethodRegistry.size())
    {
        return false;
    }
    const auto &meta = gRpcMethodRegistry[messageId];
    return meta.serviceName != nullptr && meta.methodName != nullptr;
}


std::shared_ptr<RpcMaintenanceState> RpcChannel::ProcessMaintenance()
{
    static const auto state = std::make_shared<RpcMaintenanceState>();
    return state;
}
bool RpcMaintenanceState::EnableAcknowledgedSends()
{
    std::lock_guard<std::mutex> lock(mutex_);
    if (sourcesClosed_ || publicationsClosed_ || sourceAccepted_ != 0 || outputAccepted_ != 0 || localAccepted_ != 0) return false;
    acknowledgedSends_ = true; return true;
}
bool RpcMaintenanceState::AcknowledgedSends() const { std::lock_guard<std::mutex> lock(mutex_); return acknowledgedSends_; }
bool RpcMaintenanceState::PrepareSources(){std::lock_guard<std::mutex> lock(mutex_);if(sourcesClosed_||sourceAccepted_!=0||preparedOpened_)return false;sourcesPrepared_=true;return true;}
bool RpcMaintenanceState::OpenPreparedSources(){std::lock_guard<std::mutex> lock(mutex_);if(!sourcesPrepared_||preparedOpened_||sourcesClosed_||sourceUnknown_||sourceAccepted_!=0)return false;sourcesPrepared_=false;preparedOpened_=true;return true;}
bool RpcMaintenanceState::Begin(bool source)
{
    std::lock_guard<std::mutex> lock(mutex_);
    if (source ? (sourcesClosed_ || sourcesPrepared_) : publicationsClosed_) return false;
    auto& accepted = source ? sourceAccepted_ : outputAccepted_;
    auto& unknown = source ? sourceUnknown_ : outputUnknown_;
    if (accepted == std::numeric_limits<std::uint64_t>::max()) { unknown = true; return false; }
    ++accepted;
    return true;
}
void RpcMaintenanceState::Complete(bool source, bool known)
{
    std::lock_guard<std::mutex> lock(mutex_);
    auto& accepted = source ? sourceAccepted_ : outputAccepted_;
    auto& completed = source ? sourceCompleted_ : outputCompleted_;
    auto& unknown = source ? sourceUnknown_ : outputUnknown_;
    if (!known || accepted == completed) unknown = true;
    if (completed < accepted) ++completed;
}
void RpcMaintenanceState::CloseSources() { std::lock_guard<std::mutex> lock(mutex_); sourcesClosed_ = true; }
void RpcMaintenanceState::ClosePublications() { std::lock_guard<std::mutex> lock(mutex_); publicationsClosed_ = true; }
bool RpcMaintenanceState::LocalCallbacksJoined() const
{
    std::lock_guard<std::mutex> lock(mutex_);
    return sourceAccepted_ == sourceCompleted_ && outputAccepted_ == outputCompleted_ && localAccepted_ == localCompleted_;
}
bool RpcMaintenanceState::BeginLocalCompletion()
{
    std::lock_guard<std::mutex> lock(mutex_);
    if (localAccepted_ == std::numeric_limits<std::uint64_t>::max()) { outputUnknown_ = true; return false; }
    ++localAccepted_;
    return true;
}
void RpcMaintenanceState::CompleteLocalCompletion(bool delivered)
{
    std::lock_guard<std::mutex> lock(mutex_);
    if (!delivered || localAccepted_ == localCompleted_) outputUnknown_ = true;
    if (localCompleted_ < localAccepted_) ++localCompleted_;
}
bool RpcMaintenanceState::Frontier(bool sourcesOnly, std::string& value) const
{
    std::lock_guard<std::mutex> lock(mutex_);
    if (!sourcesClosed_ || sourceUnknown_ || sourceAccepted_ != sourceCompleted_) return false;
    if (outputUnknown_ || outputAccepted_ != outputCompleted_ || localAccepted_ != localCompleted_) return false;
    if (!sourcesOnly && !publicationsClosed_) return false;
    value = "rpc0-v1:" + std::to_string(sourceAccepted_) + ":" + std::to_string(sourceCompleted_);
    value += ":" + std::to_string(outputAccepted_) + ":" + std::to_string(outputCompleted_);
    return true;
}
bool RpcMaintenanceState::SettledFrontier(std::string& value) const
{
    std::lock_guard<std::mutex> lock(mutex_);
    if(sourceUnknown_ || outputUnknown_ || sourceAccepted_!=sourceCompleted_ || outputAccepted_!=outputCompleted_ || localAccepted_!=localCompleted_)return false;
    value="rpc0-v1:"+std::to_string(sourceAccepted_)+":"+std::to_string(sourceCompleted_)+":"+std::to_string(outputAccepted_)+":"+std::to_string(outputCompleted_);
    return true;
}
struct RpcMaintenanceOperation {
    RpcMaintenanceOperation(std::shared_ptr<RpcMaintenanceState> owner, bool isSource, bool isLocal = false) : state(std::move(owner)), source(isSource), local(isLocal) {}
    RpcMaintenanceOperation(const RpcMaintenanceOperation&) = delete;
    std::shared_ptr<RpcMaintenanceState> state;
    bool source;
    bool local;
    bool known = false;
    ~RpcMaintenanceOperation() { if (local) state->CompleteLocalCompletion(known); else state->Complete(source, known); }
};

// ====================== 服务端:done 与延后完成 ======================

// 延后完成的共享状态。所有 RpcDoneCallback 副本共享一份;只持 weak_ptr<RpcChannel>(§11.7)。
struct RpcDoneCallback::State
{
    std::weak_ptr<RpcChannel> channel;
    uint64_t id{kNoInflightCallId};
    uint32_t messageId{0};
    // 保活:handler 拿到的 request / response 裸指针在完成前一直有效。
    MessagePtr request;
    MessagePtr response;
    bool completed{false};
    std::shared_ptr<RpcMaintenanceOperation> maintenance;

    ~State();
};

// 框架交给 handler 的 done(每次派发一个,在 CallServiceMethod 的栈上)。
//   * handler 同步调 Run() → 立即回包;
//   * handler Adopt → 交出共享状态,框架返回后不再自动回包;
//   * 两者都没有 → CallServiceMethod 在 handler 返回后调 CompleteIfPending,立即回包(既有 handler 的默认语义)。
class RpcChannel::DoneClosure final : public ::google::protobuf::Closure
{
public:
    DoneClosure(RpcChannel &channel, const GameRpcMessage &message, const MessagePtr &request,
                const MessagePtr &response, const RpcController &controller,
                 std::shared_ptr<RpcMaintenanceOperation> maintenance)
        : channel_(channel),
          id_(message.id()),
          messageId_(message.message_id()),
          request_(request),
          response_(response),
          controller_(controller),
          maintenance_(std::move(maintenance))
    {
    }

    DoneClosure(const DoneClosure &) = delete;
    DoneClosure &operator=(const DoneClosure &) = delete;

    void Run() override
    {
        if (state_)
        {
            RpcChannel::CompleteDeferred(*state_, RPC_NO_ERROR);
            return;
        }
        if (completed_)
        {
            const auto n = Bump(GetRpcChannelStats().replyRunTwice);
            if (ShouldLogRpcEvent(n))
            {
                LOG_ERROR << "[RpcChannel] done->Run() called twice, ignored: method=" << MethodNameOf(messageId_)
                          << ", message_id=" << messageId_ << ", occurrences=" << n;
            }
            return;
        }
        CompleteNow();
    }

    std::shared_ptr<RpcDoneCallback::State> Adopt()
    {
        if (state_)
        {
            return state_;
        }
        state_ = std::make_shared<RpcDoneCallback::State>();
        state_->channel = channel_.weak_from_this();
        state_->id = id_;
        state_->messageId = messageId_;
        state_->request = request_;
        state_->response = response_;
        if (!completed_) state_->maintenance = maintenance_;
        if (completed_)
        {
            // handler 已经同步 Run 过:交出一个已完成的句柄,之后的 Run 都是空操作。
            state_->completed = true;
            LOG_ERROR << "[RpcChannel] RpcDoneCallback::Adopt after done->Run(): method=" << MethodNameOf(messageId_)
                      << ", message_id=" << messageId_;
        }
        else
        {
            Bump(GetRpcChannelStats().adopted);
        }
        return state_;
    }

    void CompleteIfPending()
    {
        if (state_ || completed_)
        {
            return;
        }
        CompleteNow();
    }

private:
    void CompleteNow()
    {
        completed_ = true;
        if (controller_.Failed())
        {
            // 必回之后,SetFailed 不能再被忽略:否则调用方收到的是「成功 + 空响应」(fail-open)。
            const auto n = Bump(GetRpcChannelStats().handlerFailed);
            if (ShouldLogRpcEvent(n))
            {
                LOG_WARN << "[RpcChannel] handler failed: method=" << MethodNameOf(messageId_)
                         << ", message_id=" << messageId_ << ", reason=" << controller_.ErrorText();
            }
            channel_.DoneCallback(id_, messageId_, nullptr, HANDLER_FAILED);
            return;
        }
        maintenance_->known = channel_.DoneCallback(id_, messageId_, response_.get(), RPC_NO_ERROR);
    }

    RpcChannel &channel_;
    uint64_t id_;
    uint32_t messageId_;
    const MessagePtr &request_;
    const MessagePtr &response_;
    const RpcController &controller_;
    std::shared_ptr<RpcMaintenanceOperation> maintenance_;
    std::shared_ptr<RpcDoneCallback::State> state_;
    bool completed_{false};
};

RpcDoneCallback::State::~State()
{
    if (completed || id == kNoInflightCallId)
    {
        return;
    }
    // 析构可能发生在任意栈上(~EventLoop 丢弃闭包、thread_local 析构……):只有确认还在该 channel 的 loop 线程上
    // 才发包;不断言、不 LOG_FATAL。
    const auto locked = channel.lock();
    if (!locked || muduo::net::EventLoop::getEventLoopOfCurrentThread() != &locked->loop_)
    {
        Bump(GetRpcChannelStats().replyChannelGone);
        return;
    }
    const auto n = Bump(GetRpcChannelStats().replyAbandoned);
    if (ShouldLogRpcEvent(n))
    {
        LOG_ERROR << "[RpcChannel] adopted reply released without Run/Fail, replying REPLY_DROPPED: method="
                  << MethodNameOf(messageId) << ", message_id=" << messageId << ", occurrences=" << n;
    }
    locked->DoneCallback(id, messageId, nullptr, REPLY_DROPPED);
}

RpcDoneCallback RpcDoneCallback::Adopt(::google::protobuf::Closure *done)
{
    auto *closure = dynamic_cast<RpcChannel::DoneClosure *>(done);   // nullptr 输入也返回 nullptr
    if (closure == nullptr)
    {
        LOG_FATAL << "RpcDoneCallback::Adopt: done is not the closure RpcChannel passed to this dispatch (done="
                  << static_cast<const void *>(done) << ")";
    }
    return RpcDoneCallback(closure->Adopt());
}

::google::protobuf::Message &RpcDoneCallback::ResponseMessage(const ::google::protobuf::Descriptor &expected) const
{
    if (!state_ || !state_->response || state_->response->GetDescriptor() != &expected)
    {
        LOG_FATAL << "RpcDoneCallback::response<T>: type mismatch or empty handle, expected " << std::string(expected.full_name());
    }
    return *state_->response;
}

void RpcDoneCallback::Run() const
{
    if (!state_)
    {
        LOG_ERROR << "[RpcChannel] RpcDoneCallback::Run on an empty (moved-from) handle";
        return;
    }
    RpcChannel::CompleteDeferred(*state_, RPC_NO_ERROR);
}

void RpcDoneCallback::Fail() const
{
    if (!state_)
    {
        LOG_ERROR << "[RpcChannel] RpcDoneCallback::Fail on an empty (moved-from) handle";
        return;
    }
    RpcChannel::CompleteDeferred(*state_, HANDLER_FAILED);
}

bool RpcDoneCallback::IsCallerConnected() const
{
    if (!state_)
    {
        return false;
    }
    const auto locked = state_->channel.lock();
    return locked && locked->IsConnected();
}

bool RpcDoneCallback::IsCompleted() const
{
    return !state_ || state_->completed;
}

void RpcChannel::CompleteDeferred(RpcDoneCallback::State &state, GameErrorCode error)
{
    if (state.completed)
    {
        const auto n = Bump(GetRpcChannelStats().replyRunTwice);
        if (ShouldLogRpcEvent(n))
        {
            LOG_ERROR << "[RpcChannel] RpcDoneCallback completed twice, ignored: method=" << MethodNameOf(state.messageId)
                      << ", message_id=" << state.messageId << ", occurrences=" << n;
        }
        return;
    }
    state.completed = true;
    const auto maintenance = std::move(state.maintenance);
    if (error == HANDLER_FAILED)
    {
        Bump(GetRpcChannelStats().handlerFailed);
    }

    const auto locked = state.channel.lock();
    if (!locked)
    {
        Bump(GetRpcChannelStats().replyChannelGone);
        return;
    }
    locked->loop_.assertInLoopThread();
    if (!locked->IsConnected())
    {
        Bump(GetRpcChannelStats().replyChannelGone);
        return;
    }
    const bool sent = locked->DoneCallback(state.id, state.messageId, error == RPC_NO_ERROR ? state.response.get() : nullptr, error);
    if (maintenance) maintenance->known = sent && error == RPC_NO_ERROR;
}

// ====================== RpcChannel ======================

std::shared_ptr<RpcChannel> RpcChannel::Create(muduo::net::EventLoop &loop,
                                               const TcpConnectionPtr &connection,
                                               RpcServiceMap services, std::shared_ptr<RpcMaintenanceState> maintenance)
{
    return std::make_shared<RpcChannel>(PrivateTag{}, loop, connection, std::move(services), std::move(maintenance));
}

RpcChannel::RpcChannel(PrivateTag, muduo::net::EventLoop &loop, const TcpConnectionPtr &connection,
                       RpcServiceMap services, std::shared_ptr<RpcMaintenanceState> maintenance)
    : maintenance_(std::move(maintenance)),
      loop_(loop),
      connection_(connection),
      services_(std::move(services)),
      codec_(std::bind(&RpcChannel::OnRpcMessage, this, _1, _2, _3))
{
    LOG_DEBUG << "RpcChannel created: " << this;
}

RpcChannel::~RpcChannel()
{
    if (outstandings_.Empty())
    {
        return;
    }
    // 节点摘除、同 uuid 重注册、关机清理都不经过 DOWN 回调(weak lock 失败进不了 onConnection),由这里兜底。
    // 只有确认仍在本 channel 的 loop 线程上(loop 尚未析构)才能 queueInLoop;否则 done 只能随容器销毁 ——
    // 这条路径只在关机收尾遗漏时出现(NodeUtils::CloseAllRpcChannels 应已把表清空),视为缺陷信号。
    if (muduo::net::EventLoop::getEventLoopOfCurrentThread() != &loop_)
    {
        const auto dropped = outstandings_.Size();
        Bump(GetRpcChannelStats().droppedWithoutLoop, dropped);
        LOG_ERROR << "[RpcChannel] destroyed off its EventLoop with " << dropped
                  << " outstanding call(s); their callbacks will never run";
        return;
    }
    FailAll(RpcStatus{RpcStatusCode::kChannelClosed});
}

void RpcChannel::FailNotSent(muduo::net::EventLoop &loop, uint32_t messageId, ClientDoneCallback done,
                             const char *reason, const std::string &peer, std::shared_ptr<RpcMaintenanceState> maintenance)
{
    auto &stats = GetRpcChannelStats();
    Bump(stats.started);
    // 采样用同步递增的计数:notSent 要等排队的投递执行后才加,同一轮里一串本地失败会看到同一个值,采样形同虚设。
    static std::atomic<uint64_t> notSentOccurrences{0};
    if (ShouldLogRpcEvent(Bump(notSentOccurrences)))
    {
        LOG_WARN << "[RpcChannel] call not sent: reason=" << reason << ", method=" << MethodNameOf(messageId)
                 << ", message_id=" << messageId << ", peer=" << peer;
    }
    FailSoon(loop, std::move(done), RpcStatus{RpcStatusCode::kNotSent}, std::move(maintenance));
}

void RpcChannel::FailSoon(muduo::net::EventLoop &loop, ClientDoneCallback done, RpcStatus status, std::shared_ptr<RpcMaintenanceState> maintenance)
{
    std::shared_ptr<RpcMaintenanceOperation> operation;
    if (maintenance && maintenance->BeginLocalCompletion()) operation = std::make_shared<RpcMaintenanceOperation>(maintenance, false, true);
    auto call = std::make_shared<OutstandingCall>();
    call->done = std::move(done); call->trace = tracing::Current(); call->maintenance = std::move(operation);
    loop.queueInLoop([call, status] { Deliver(*call, status, nullptr); });
}

void RpcChannel::Deliver(OutstandingCall &call, const RpcStatus &status, const MessagePtr &response)
{
    const auto operation = std::move(call.maintenance);
    auto done = std::move(call.done); // 后构造先析构；target 释放及其派生工作仍在票据内。
    RecordOutcome(status);
    if (call.trace.IsValid()) {
        tracing::ScopedSpan span(call.trace);
        done(status, response);
    } else { done(status, response); }
    if (operation) operation->known = status.ok() || status.DefinitelyNotExecuted();
}

void RpcChannel::CallMethod(uint32_t messageId, const ProtobufMessage &request, ClientDoneCallback done,
                            const RpcCallOptions &options)
{
    CallMethodWithResponseType(messageId, request, nullptr, std::move(done), options);
}

void RpcChannel::CallMethodWithResponseType(uint32_t messageId, const ProtobufMessage &request,
                                            const ::google::protobuf::Descriptor *expectedResponse,
                                            ClientDoneCallback done, const RpcCallOptions &options)
{
    loop_.assertInLoopThread();
    if (!done)
    {
        LOG_FATAL << "RpcChannel::CallMethod requires a done callback; use SendRequest for one-way messages (message_id="
                  << messageId << ")";
    }
    auto &stats = GetRpcChannelStats();

    // 本地确定没发出:不登记、经 queueInLoop 以 kNotSent 投递,done 绝不在本函数返回前被调用。
    auto notSent = [&](const char *reason)
    {
        FailNotSent(loop_, messageId, std::move(done), reason, PeerOf(connection_), maintenance_);
    };

    if (closed_)
    {
        return notSent("channel closed");
    }
    if (!IsDispatchableMessageId(messageId))
    {
        return notSent("message id not dispatchable");
    }
    const auto &meta = gRpcMethodRegistry[messageId];
    if (!meta.requestProto || !meta.responseProto || request.GetDescriptor() != meta.requestProto->GetDescriptor())
    {
        return notSent("request type does not match registry");
    }
    if (expectedResponse != nullptr && expectedResponse != meta.responseProto->GetDescriptor())
    {
        return notSent("response type does not match registry");
    }
    if (options.timeout <= std::chrono::milliseconds::zero())
    {
        return notSent("non-positive timeout");
    }
    if (outstandings_.Size() >= kMaxOutstandingRpcCalls)
    {
        return notSent("too many outstanding calls");
    }
    const auto connection = LockConnectedConnection();
    if (!connection)
    {
        return notSent("not connected");
    }

    GameRpcMessage message;
    message.set_type(REQUEST);
    message.set_message_id(messageId);
    if (!SerializeMessage(request, message.mutable_request()))
    {
        return notSent("request serialization failed");
    }
    if (!maintenance_ || !maintenance_->Begin(false)) return notSent("maintenance publications closed");
    auto operation = std::make_shared<RpcMaintenanceOperation>(maintenance_, false);
    const InflightCallId id = ++id_;
    message.set_id(id);
    Bump(stats.started);

    OutstandingCall call;
    call.maintenance = std::move(operation);
    call.expiresAt = Clock::now() + options.timeout;
    call.messageId = messageId;
    call.done = std::move(done);
    call.trace = tracing::Current();
    outstandings_.Insert(id, std::move(call));   // id 单调非 0,不会重复
    RaiseMax(stats.maxInFlight, outstandings_.Size());

    // muduo 的 send 在 loop 线程上直接写入或进发送缓冲,不会同步关闭连接、不会同步回调,
    // 所以这里之后 done 仍不会在 CallMethod 返回前被调用;之后丢失由 DOWN / 超时兜底。
    SendFrame(connection, message);
    ArmTimeoutTick();
}

bool RpcChannel::SendRequest(uint32_t messageId, const ProtobufMessage &request)
{
    loop_.assertInLoopThread();
    auto &stats = GetRpcChannelStats();
    auto reject = [&](const char *reason)
    {
        const auto n = Bump(stats.onewayRejected);
        if (ShouldLogRpcEvent(n))
        {
            LOG_WARN << "[RpcChannel] one-way request dropped: reason=" << reason << ", method=" << MethodNameOf(messageId)
                     << ", message_id=" << messageId << ", peer=" << PeerOf(connection_) << ", occurrences=" << n;
        }
        return false;
    };

    if (closed_) return reject("channel closed");
    if (!IsDispatchableMessageId(messageId))
    {
        return reject("message id not dispatchable");
    }
    const auto &meta = gRpcMethodRegistry[messageId];
    if (!meta.requestProto || request.GetDescriptor() != meta.requestProto->GetDescriptor())
    {
        return reject("request type does not match registry");
    }
    const auto connection = LockConnectedConnection();
    if (!connection)
    {
        return reject("not connected");
    }

    GameRpcMessage message;
    message.set_type(REQUEST);
    message.set_message_id(messageId);
    if (!SerializeMessage(request, message.mutable_request()))
    {
        return reject("request serialization failed");
    }
    const bool acknowledged = maintenance_ && maintenance_->AcknowledgedSends();
    if (acknowledged && (outstandings_.Size() >= kMaxOutstandingRpcCalls || id_ == std::numeric_limits<std::uint64_t>::max())) return reject("maintenance acknowledgement capacity exhausted");
    if (!maintenance_ || !maintenance_->Begin(false)) return reject("maintenance publications closed");
    const auto operation = std::make_shared<RpcMaintenanceOperation>(maintenance_, false);
    if (acknowledged) {
        // 仍返回本地接受 bool，但内部用既有 nonzero-id 协议等待对端真实 handler/Adopt 完成。
        // 代价是每个单向业务保留一条有界在途记录，失败或超时保留 UNKNOWN，绝不自动重发。
        message.set_id(++id_);
        OutstandingCall call; call.maintenance = operation; call.messageId = messageId;
        call.expiresAt = Clock::now() + kDefaultRpcCallTimeout; call.trace = tracing::Current();
        call.done = [](const RpcStatus&, const MessagePtr&) {};
        outstandings_.Insert(message.id(), std::move(call));
        Bump(stats.started); RaiseMax(stats.maxInFlight, outstandings_.Size());
    }
    // 默认旧 id0 无远端确认，保留 UNKNOWN；只有显式维护模式才由上面的 RESPONSE/RPC_ERROR 收账。
    SendFrame(connection, message);
    if (acknowledged) ArmTimeoutTick();
    Bump(stats.onewaySent);
    return true;
}

void RpcChannel::OnMessage(const TcpConnectionPtr &connection, muduo::net::Buffer *buffer, muduo::Timestamp receiveTime)
{
    loop_.assertInLoopThread();
    codec_.onMessage(connection, buffer, receiveTime);
}

void RpcChannel::OnDisconnect()
{
    loop_.assertInLoopThread();
    FailAll(RpcStatus{RpcStatusCode::kDisconnected});
}

void RpcChannel::Close()
{
    loop_.assertInLoopThread();
    if (closed_)
    {
        return;
    }
    closed_ = true;
    FailAll(RpcStatus{RpcStatusCode::kChannelClosed});
}

bool RpcChannel::IsConnected() const
{
    const auto locked = connection_.lock();
    return locked && locked->connected();
}

bool RpcChannel::IsBoundTo(const TcpConnectionPtr &connection) const
{
    return connection && connection_.lock() == connection;
}

TcpConnectionPtr RpcChannel::LockConnectedConnection() const
{
    auto locked = connection_.lock();
    if (!locked || !locked->connected())
    {
        return nullptr;
    }
    return locked;
}

void RpcChannel::SendFrame(const TcpConnectionPtr &connection, const GameRpcMessage &message)
{
    const auto size = static_cast<uint32_t>(LogIfMessageTooLarge(message));
    codec_.send(connection, message);
    TrafficStatsCollector::Instance().RecordSend(message.message_id(), size);
}

void RpcChannel::FailAll(RpcStatus status)
{
    if (outstandings_.Empty())
    {
        return;
    }
    auto batch = std::make_shared<OutstandingBatch>(outstandings_.ExtractAll());
    LOG_WARN << "[RpcChannel] failing " << batch->size() << " outstanding call(s): status=" << status.name()
             << ", peer=" << PeerOf(connection_);
    // 先整表摘出(恰好一次),回调延后:避开连接回调栈、RemoveRpcSessionsBoundTo 遍历 registry 的栈、
    // registry.destroy / ~TcpServer 的析构栈。闭包不引用 channel。关机时由 Node 的阶段 0 保证它排在拆除之前。
    loop_.queueInLoop([batch, status]
    {
        for (auto &entry : *batch)
        {
            Deliver(entry.second, status, nullptr);
        }
    });
}

void RpcChannel::ArmTimeoutTick()
{
    if (timeoutTickArmed_ || closed_ || outstandings_.Empty())
    {
        return;
    }
    std::weak_ptr<RpcChannel> weakSelf = weak_from_this();
    if (weakSelf.expired())
    {
        LOG_FATAL << "RpcChannel must be owned by shared_ptr (use RpcChannel::Create)";
    }
    timeoutTickArmed_ = true;
    // 从不 cancel:channel 没了,tick 触发时 lock 失败就结束;表空了就不再续挂。
    loop_.runAfter(kTimeoutTickSeconds, [weakSelf]
    {
        if (const auto self = weakSelf.lock())
        {
            self->OnTimeoutTick();
        }
    });
}

void RpcChannel::OnTimeoutTick()
{
    timeoutTickArmed_ = false;
    SweepTimeouts(Clock::now());
    ArmTimeoutTick();
}

void RpcChannel::SweepTimeouts(Clock::time_point now)
{
    loop_.assertInLoopThread();
    auto expired = outstandings_.ExtractExpired(now);
    if (expired.empty())
    {
        return;
    }
    auto &stats = GetRpcChannelStats();
    std::chrono::milliseconds maxLateness{0};
    for (const auto &entry : expired)
    {
        maxLateness = (std::max)(maxLateness,
                               std::chrono::duration_cast<std::chrono::milliseconds>(now - entry.second.expiresAt));
    }
    RaiseMax(stats.maxSweepLatenessMs, static_cast<uint64_t>(maxLateness.count()));
    if (ShouldLogRpcEvent(stats.timeout.load(std::memory_order_relaxed) + 1))
    {
        LOG_WARN << "[RpcChannel] " << expired.size() << " call(s) timed out: first method="
                 << MethodNameOf(expired.front().second.messageId) << ", peer=" << PeerOf(connection_)
                 << ", max_lateness_ms=" << maxLateness.count();
    }
    // 已整批摘出,逐个回调:done 里重入 CallMethod / Close / 销毁 owner 都不会影响这一批(调用方 OnTimeoutTick 持有 self)。
    for (auto &entry : expired)
    {
        Deliver(entry.second, RpcStatus{RpcStatusCode::kTimeout}, nullptr);
    }
}

void RpcChannel::OnRpcMessage(const TcpConnectionPtr &connection, const RpcMessagePtr &messagePtr,
                              muduo::Timestamp /*receiveTime*/)
{
    assert(connection == connection_.lock());
    const auto &message = *messagePtr;

    const size_t messageSize = LogIfMessageTooLarge(message);
    LOG_TRACE << "RPC message received, type: " << message.type() << ", message ID: " << message.message_id()
              << ", id: " << message.id();
    TrafficStatsCollector::Instance().RecordRecv(message.message_id(), static_cast<uint32_t>(messageSize));

    switch (message.type())
    {
    case RESPONSE:
        OnResponse(message);
        break;
    case REQUEST:
        CallServiceMethod(connection, message);
        break;
    case RPC_ERROR:
        OnRpcError(message);
        break;
    default:
    {
        // 含已删除的 RPC_CLIENT_REQUEST(2) / NODE_ROUTE(3):计数、采样告警、丢弃,不断连。
        const auto n = Bump(GetRpcChannelStats().unknownType);
        if (ShouldLogRpcEvent(n))
        {
            LOG_ERROR << "[RpcChannel] unknown message type " << static_cast<int>(message.type())
                      << ", message_id=" << message.message_id() << ", peer=" << PeerOf(connection_);
        }
        break;
    }
    }
}

void RpcChannel::OnResponse(const GameRpcMessage &message)
{
    auto &stats = GetRpcChannelStats();
    if (message.id() == kNoInflightCallId)
    {
        // 没有调用号的回包:对端是还不认识 id 的旧二进制(同 zone 的 gate / scene 必须一起升级),或对端违反了
        // 「单向不回」。切换完成后应恒为 0。
        const auto n = Bump(stats.legacyId0Reply);
        if (ShouldLogRpcEvent(n))
        {
            LOG_ERROR << "[RpcChannel] reply without call id dropped (peer binary predates RPC0 call ids?): method="
                      << MethodNameOf(message.message_id()) << ", message_id=" << message.message_id()
                      << ", peer=" << PeerOf(connection_) << ", occurrences=" << n;
        }
        return;
    }

    auto call = outstandings_.Extract(message.id());
    if (!call)
    {
        const auto n = Bump(stats.lateReply);
        if (ShouldLogRpcEvent(n))
        {
            LOG_WARN << "[RpcChannel] late or unknown reply dropped: id=" << message.id()
                     << ", method=" << MethodNameOf(message.message_id()) << ", message_id=" << message.message_id()
                     << ", peer=" << PeerOf(connection_) << ", occurrences=" << n;
        }
        return;
    }

    if (message.message_id() != call->messageId)
    {
        LOG_ERROR << "[RpcChannel] reply message_id mismatch: id=" << message.id() << ", expected="
                  << call->messageId << ", got=" << message.message_id() << ", peer=" << PeerOf(connection_);
        Deliver(*call, RpcStatus{RpcStatusCode::kInvalidResponse}, nullptr);
        return;
    }

    // 解析一律按登记的 messageId 取响应原型(CallMethod 时已校验该槽可派发、有响应原型)。
    MessagePtr response(gRpcMethodRegistry[call->messageId].responseProto->New());
    if (!response->ParsePartialFromArray(message.response().data(), static_cast<int32_t>(message.response().size())))
    {
        LOG_ERROR << "[RpcChannel] failed to parse reply: method=" << MethodNameOf(call->messageId)
                  << ", id=" << message.id() << ", peer=" << PeerOf(connection_);
        Deliver(*call, RpcStatus{RpcStatusCode::kInvalidResponse}, nullptr);
        return;
    }
    Deliver(*call, RpcStatus::Ok(), response);
}

void RpcChannel::OnRpcError(const GameRpcMessage &message)
{
    auto &stats = GetRpcChannelStats();
    // Review R3 fix (2026-05-17): NO stacktrace on this path. Under an attack where the peer hammers us with
    // malformed requests that produce RPC_ERROR replies, boost::stacktrace allocation would become a CPU hotspot.
    const auto methodName = MethodNameOf(message.message_id());
    if (message.id() == kNoInflightCallId)
    {
        const auto n = Bump(stats.legacyId0Reply);
        if (ShouldLogRpcEvent(n))
        {
            LOG_ERROR << "[RpcChannel] RPC_ERROR without call id dropped: message_id=" << message.message_id()
                      << " method=" << methodName << " error_code=" << message.error()
                      << ", peer=" << PeerOf(connection_) << ", occurrences=" << n;
        }
        return;
    }

    auto call = outstandings_.Extract(message.id());
    if (!call)
    {
        const auto n = Bump(stats.lateReply);
        if (ShouldLogRpcEvent(n))
        {
            LOG_WARN << "[RpcChannel] late or unknown RPC_ERROR dropped: id=" << message.id()
                     << ", method=" << methodName << ", error_code=" << message.error() << ", occurrences=" << n;
        }
        return;
    }

    const auto rejected = stats.remoteRejected.load(std::memory_order_relaxed) +
                          stats.remoteFailed.load(std::memory_order_relaxed) +
                          stats.replyDropped.load(std::memory_order_relaxed) + 1;
    if (ShouldLogRpcEvent(rejected))
    {
        LOG_WARN << "[RpcChannel] call rejected by peer: message_id=" << message.message_id()
                 << " method=" << methodName << " error_code=" << message.error() << ", peer=" << PeerOf(connection_);
        // todo.md #250 slice A — record into process-wide buffer (only for the sampled ones: Record locks and allocates).
        std::ostringstream detail;
        detail << "method=" << methodName << " message_id=" << message.message_id();
        error_reporter::Record(message.error(), "rpc_error_in", detail.str());
    }
    Deliver(*call, RpcStatus::FromRemoteError(message.error()), nullptr);
}

void RpcChannel::CallServiceMethod(const TcpConnectionPtr &connection, const GameRpcMessage &message)
{
    ScopedDispatchTrace dispatchTrace;

    const uint32_t messageId = message.message_id();
    if (!IsDispatchableMessageId(messageId))
    {
        RejectRequest(message, INVALID_REQUEST);
        return;
    }
    const auto &meta = gRpcMethodRegistry[messageId];
    const auto serviceIt = services_.find(meta.serviceName);
    if (serviceIt == services_.end() || serviceIt->second == nullptr)
    {
        RejectRequest(message, NO_SERVICE);
        return;
    }
    auto *service = serviceIt->second;
    const auto *method = service->GetDescriptor()->FindMethodByName(meta.methodName);
    if (method == nullptr)
    {
        RejectRequest(message, NO_METHOD);
        return;
    }

    MessagePtr request(service->GetRequestPrototype(method).New());
    if (!request->ParsePartialFromArray(message.request().data(), static_cast<int32_t>(message.request().size())))
    {
        RejectRequest(message, INVALID_REQUEST);
        return;
    }
    if (!maintenance_ || !maintenance_->Begin(true)) { RejectRequest(message, NO_SERVICE); return; }
    const auto operation = std::make_shared<RpcMaintenanceOperation>(maintenance_, true);
    MessagePtr response(service->GetResponsePrototype(method).New());
    Bump(GetRpcChannelStats().requestsDispatched);

    // 本次调用的 ctx(Go 的 ctx 语义):栈上构造、CallMethod 返回即析构,随调用链显式下传,不可存储
    // (见 network/rpc_controller.h)。要在返回后完成,用 done:RpcDoneCallback::Adopt(done)。
    // 规则:handler 内不得同步释放本连接所属 RpcClient 的最后一个引用 —— 回调期间 muduo 的 tie_ 守卫与这个
    // 栈对象各持一份强引用,~TcpClient 会因 use_count>1 跳过 forceClose;节点摘除必须经 queueInLoop 延后。
    RpcController controller(connection);
    DoneClosure done(*this, message, request, response, controller, operation);
    service->CallMethod(method, &controller, request.get(), response.get(), &done);
    done.CompleteIfPending();
}

void RpcChannel::RejectRequest(const GameRpcMessage &request, GameErrorCode error)
{
    // Review R3 fix (2026-05-17): NO stacktrace here either — an adversarial peer sending packets that produce
    // rejections on every receive would turn boost::stacktrace into a DoS amplifier. Logging is sampled for the
    // same reason; the exact count is in RpcChannelStats.
    const auto n = Bump(GetRpcChannelStats().requestsRejected);
    if (ShouldLogRpcEvent(n))
    {
        const auto methodName = MethodNameOf(request.message_id());
        const auto requestBodySize = request.request().size();
        LOG_ERROR << "[RpcChannel] rejecting request: message_id=" << request.message_id()
                  << " method=" << methodName << " error_code=" << error << " id=" << request.id()
                  << " request_body_size=" << requestBodySize << ", occurrences=" << n;
        // todo.md #250 slice A — record outgoing rejection into process-wide buffer.
        std::ostringstream detail;
        detail << "method=" << methodName << " message_id=" << request.message_id()
               << " request_body_size=" << requestBodySize;
        error_reporter::Record(static_cast<uint32_t>(error), "rpc_error_out", detail.str());
    }
    DoneCallback(request.id(), request.message_id(), nullptr, error);
}

bool RpcChannel::DoneCallback(uint64_t id, uint32_t messageId, const ProtobufMessage *response, GameErrorCode error)
{
    if (id == kNoInflightCallId)
    {
        return error == RPC_NO_ERROR;   // 单向:调用方声明不要应答,永不回 RESPONSE / RPC_ERROR
    }
    const auto connection = LockConnectedConnection();
    if (!connection)
    {
        Bump(GetRpcChannelStats().replyChannelGone);
        return false;
    }

    GameRpcMessage reply;
    reply.set_id(id);
    reply.set_message_id(messageId);
    if (error != RPC_NO_ERROR)
    {
        reply.set_type(RPC_ERROR);
        reply.set_error(error);
    }
    else
    {
        reply.set_type(RESPONSE);
        if (response != nullptr && !SerializeMessage(*response, reply.mutable_response()))
        {
            // handler 已经运行,回包却发不出去:按「结果未知」告诉调用方。
            LOG_ERROR << "[RpcChannel] failed to serialize response: method=" << MethodNameOf(messageId)
                      << ", id=" << id;
            reply.clear_response();
            reply.set_type(RPC_ERROR);
            reply.set_error(REPLY_DROPPED);
        }
    }
    SendFrame(connection, reply);
    return reply.type() == RESPONSE;
}
