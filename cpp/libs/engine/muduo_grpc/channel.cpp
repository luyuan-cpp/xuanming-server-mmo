#include "channel.h"

#include <algorithm>
#include <limits>

#include "muduo/base/Logging.h"
#include "muduo/net/EventLoop.h"

#include "address.h"
#include "grpc_timeout.h"
#include "message_framing.h"

namespace muduo_grpc
{

namespace
{

constexpr uint32_t kHttp2Cancel = 0x8;

double ToSeconds(std::chrono::steady_clock::duration duration)
{
    return std::max(0.0, std::chrono::duration<double>(duration).count());
}

UnaryCallResult FailureResult(Status status)
{
    UnaryCallResult result;
    result.status = std::move(status);
    return result;
}

} // namespace


std::shared_ptr<MaintenanceState> Channel::ProcessMaintenance()
{
    static const auto state = std::make_shared<MaintenanceState>();
    return state;
}
std::shared_ptr<MaintenanceState> Channel::ProcessControlMaintenance()
{
    static const auto state = std::make_shared<MaintenanceState>();
    return state;
}
bool MaintenanceState::Begin()
{
    std::lock_guard<std::mutex> lock(mutex_);
    if (closed_) return false;
    if (accepted_ == std::numeric_limits<std::uint64_t>::max()) { unknown_ = true; return false; }
    ++accepted_; return true;
}
void MaintenanceState::Complete(bool known)
{
    std::lock_guard<std::mutex> lock(mutex_);
    if (!known || accepted_ == completed_) unknown_ = true;
    if (completed_ < accepted_) ++completed_;
}
void MaintenanceState::Close() { std::lock_guard<std::mutex> lock(mutex_); closed_ = true; }
bool MaintenanceState::Closed() const { std::lock_guard<std::mutex> lock(mutex_); return closed_; }
bool MaintenanceState::Joined() const
{
    std::lock_guard<std::mutex> lock(mutex_);
    return closed_ && !callbackTrackingLost_ && accepted_ == completed_ && callbacksAccepted_ == callbacksCompleted_;
}
bool MaintenanceState::SettledFrontier(std::string& value) const
{
    std::lock_guard<std::mutex> lock(mutex_);
    if (unknown_ || callbackTrackingLost_ || accepted_ != completed_ || callbacksAccepted_ != callbacksCompleted_) return false;
    value = "grpc-call-v2:" + std::to_string(accepted_) + ":" + std::to_string(completed_) + ":" +
        std::to_string(callbacksAccepted_) + ":" + std::to_string(callbacksCompleted_);
    return true;
}
void MaintenanceState::OwnerOpened()
{
    std::lock_guard<std::mutex> lock(mutex_);
    if (liveOwners_ == std::numeric_limits<std::uint64_t>::max()) { callbackTrackingLost_ = true; unknown_ = true; }
    else ++liveOwners_;
}
void MaintenanceState::OwnerJoined()
{
    std::lock_guard<std::mutex> lock(mutex_);
    if (liveOwners_ == 0) { callbackTrackingLost_ = true; unknown_ = true; } else --liveOwners_;
}
bool MaintenanceState::NativeJoined() const
{
    std::lock_guard<std::mutex> lock(mutex_);
    return closed_ && !callbackTrackingLost_ && liveOwners_ == 0;
}
void MaintenanceState::BeginCallback()
{
    std::lock_guard<std::mutex> lock(mutex_);
    if (callbacksAccepted_ == std::numeric_limits<std::uint64_t>::max())
    {
        unknown_ = true;
        callbackTrackingLost_ = true;
        return;
    }
    ++callbacksAccepted_;
}
void MaintenanceState::CompleteCallback(bool returned)
{
    std::lock_guard<std::mutex> lock(mutex_);
    if (!returned || callbacksAccepted_ == callbacksCompleted_) unknown_ = true;
    if (callbacksCompleted_ < callbacksAccepted_) ++callbacksCompleted_;
}
bool MaintenanceState::Frontier(std::string& value) const
{
    std::lock_guard<std::mutex> lock(mutex_);
    if (!closed_ || unknown_ || callbackTrackingLost_ || accepted_ != completed_ || callbacksAccepted_ != callbacksCompleted_) return false;
    value = "grpc-call-v2:" + std::to_string(accepted_) + ":" + std::to_string(completed_) + ":" +
        std::to_string(callbacksAccepted_) + ":" + std::to_string(callbacksCompleted_);
    return true;
}
namespace detail {
struct MaintenanceOperation {
    explicit MaintenanceOperation(std::shared_ptr<MaintenanceState> owner) : state(std::move(owner)) {}
    MaintenanceOperation(const MaintenanceOperation&) = delete;
    std::shared_ptr<MaintenanceState> state;
    bool known = false;
    ~MaintenanceOperation() { state->Complete(known); }
};
// 排队闭包持有此票据；只有实际执行并从业务回调返回，才确认本机完成。
// 它不持有 Channel，回调销毁通道也不会留下裸对象引用。
struct MaintenanceCompletion {
    explicit MaintenanceCompletion(std::shared_ptr<MaintenanceState> owner) : state(std::move(owner))
    {
        state->BeginCallback();
    }
    MaintenanceCompletion(const MaintenanceCompletion&) = delete;
    std::shared_ptr<MaintenanceState> state;
    bool returned = false;
    ~MaintenanceCompletion() { state->CompleteCallback(returned); }
};
struct UnaryCompletionOwner {
    // 显式成员顺序，不依赖 lambda capture 的未指定析构顺序。
    std::shared_ptr<MaintenanceOperation> operation;
    std::shared_ptr<MaintenanceCompletion> completion;
    UnaryCallback callback;
    void Run(UnaryCallResult&& result) {
        const bool known = result.status.ok();
        callback(std::move(result));
        callback = {}; // target 析构可继续派生拒绝回调，票据此时仍然在途。
        if (operation) operation->known = known;
        if (completion) completion->returned = true;
    }
};
struct StreamCompletionOwner {
    StreamCompletionOwner(std::shared_ptr<MaintenanceState> state, std::shared_ptr<StreamRecord> target)
        : completion(std::make_shared<MaintenanceCompletion>(std::move(state))), record(std::move(target)) {}
    std::shared_ptr<MaintenanceCompletion> completion;
    std::shared_ptr<StreamRecord> record;
};
}

const char *ConnectivityStateName(ConnectivityState state)
{
    switch (state)
    {
    case ConnectivityState::kIdle: return "IDLE";
    case ConnectivityState::kConnecting: return "CONNECTING";
    case ConnectivityState::kReady: return "READY";
    case ConnectivityState::kTransientFailure: return "TRANSIENT_FAILURE";
    case ConnectivityState::kShutdown: return "SHUTDOWN";
    }
    return "UNKNOWN";
}

std::shared_ptr<Channel> Channel::Create(muduo::net::EventLoop &loop, const std::string &ip, uint16_t port,
                                         ChannelOptions options)
{
    if (!IsCanonicalIpv4(ip) || port == 0)
    {
        LOG_ERROR << "muduo_grpc::Channel rejects target '" << ip << ":" << port
                  << "': only IPv4 literals with a non-zero port are supported";
        return nullptr;
    }
    return std::make_shared<Channel>(PrivateTag{}, loop, ip, port, std::move(options));
}

Channel::Channel(PrivateTag, muduo::net::EventLoop &loop, const std::string &ip, uint16_t port,
                 ChannelOptions options)
    : maintenance_(options.maintenance ? options.maintenance : ProcessMaintenance()),
      loop_(loop),
      target_(ip + ":" + std::to_string(port)),
      address_(ip, port),
      options_(std::move(options)),
      nextBackoffMs_(static_cast<double>(options_.backoff.initial.count())),
      rng_(std::random_device{}())
{
    maintenance_->OwnerOpened();
    transportOptions_.session.authority = options_.authority.empty() ? target_ : options_.authority;
    transportOptions_.session.userAgent = options_.userAgent;
    transportOptions_.session.maxReceiveMessageBytes = options_.maxReceiveMessageBytes;
    transportOptions_.session.maxHeaderListBytes = options_.maxHeaderListBytes;
    transportOptions_.session.initialStreamWindowBytes = options_.initialStreamWindowBytes;
    transportOptions_.session.connectionWindowBytes = options_.connectionWindowBytes;
    transportOptions_.maxPendingOutputBytes = options_.maxPendingOutputBytes;
    transportOptions_.connectTimeout = options_.connectTimeout;
    transportOptions_.keepaliveTimeout = options_.keepaliveTimeout;
    transportOptions_.keepaliveTime = options_.keepaliveTime;
}

Channel::~Channel()
{
    state_ = ConnectivityState::kShutdown;
    const bool loopIsCurrent = LoopIsCurrent();
    if (!loopIsCurrent)
    {
        // 正常关机路径(Node::FinalizeShutdownInLoop)一定在 loop 线程上、loop 仍在运行时析构;走到这里说明有人漏了清理。
        LOG_ERROR << "muduo_grpc::Channel " << target_
                  << " destroyed outside its EventLoop thread; pending callbacks are dropped";
    }
    else
    {
        if (deadlineTimerArmed_)
        {
            loop_.cancel(deadlineTimer_);
        }
        if (backoffTimerArmed_)
        {
            loop_.cancel(backoffTimer_);
        }
        // 未完成的调用:回调不捕获通道,通道析构后照样安全执行。
        for (auto &[callId, call] : calls_)
        {
            loop_.queueInLoop([done = std::move(call.done)]() mutable
                              { done(FailureResult(Status(StatusCode::kUnavailable, "channel shut down"))); });
        }
        // 未结束的双向流同理:onClose 只捕获流记录,不捕获通道。
        for (auto &[recordId, record] : streams_)
        {
            if (record->closed || record->cancelled)
            {
                continue;
            }
            record->closed = true;
            loop_.queueInLoop([record]
                              {
                if (!record->cancelled && record->callbacks.onClose)
                {
                    record->callbacks.onClose(Status(StatusCode::kUnavailable, "channel shut down"), Metadata());
                } });
        }
    }
    calls_.clear();
    pending_.clear();
    deadlines_.clear();
    streamToCall_.clear();
    streams_.clear();
    pendingStreams_.clear();
    streamToBidi_.clear();
    if (current_)
    {
        draining_.push_back(std::move(current_));
    }
    for (auto &transport : draining_)
    {
        transport->Shutdown();
        if (loopIsCurrent)
        {
            // 连接对象也排到队尾再析构,与 ReleaseTransport 一致,不就地释放(§11.7);关机时也仍在两跳排空之内。
            // 以前这里还要靠 FIFO 让 ~TcpClient 排在 muduo Connector 排队的 resetChannel 之后(上游绑裸 this);
            // 那处已改持 shared_from_this()(muduo LOCAL DELTA),Connector 自己活到 channel_ 释放,不再依赖这里的顺序。
            loop_.queueInLoop([released = std::move(transport)]() mutable { released.reset(); });
        }
    }
    // loop 不在本线程(漏了关机清理)时只能就地析构;这条兜底路径的根治见设计 §10.1 F012 / F022。
    draining_.clear();
    if (loopIsCurrent) {
        // released.reset 先排 forceClose，再排 connectDestroyed；在它们之后两跳才确认本机传输退出。
        auto* loop = &loop_;
        loop->queueInLoop([loop, state = maintenance_] {
            loop->queueInLoop([loop, state] { loop->queueInLoop([state] { state->OwnerJoined(); }); });
        });
    }
}

void Channel::StartUnaryCall(UnaryCallRequest request, UnaryCallback done)
{
    loop_.assertInLoopThread();
    if (!done)
    {
        LOG_ERROR << "muduo_grpc::Channel::StartUnaryCall called without a completion callback, method="
                  << request.method;
        return;
    }
    const auto now = std::chrono::steady_clock::now();
    if (state_ == ConnectivityState::kShutdown)
    {
        FailSoon(std::move(done), Status(StatusCode::kUnavailable, "channel shut down"));
        return;
    }
    if (calls_.size() >= options_.maxOutstandingCalls)
    {
        FailSoon(std::move(done), Status(StatusCode::kResourceExhausted,
                                         "too many outstanding calls on channel " + target_ + " (limit " +
                                             std::to_string(options_.maxOutstandingCalls) + ")"));
        return;
    }
    if (request.message.size() > options_.maxSendMessageBytes)
    {
        FailSoon(std::move(done), Status(StatusCode::kResourceExhausted,
                                         "sent message larger than max (" + std::to_string(request.message.size()) +
                                             " vs. " + std::to_string(options_.maxSendMessageBytes) + ")"));
        return;
    }
    for (const auto &[key, value] : request.metadata)
    {
        if (!IsValidCustomMetadataKey(key) || (!IsBinaryHeaderKey(key) && !IsValidAsciiMetadataValue(value)))
        {
            FailSoon(std::move(done), Status(StatusCode::kInternal, "invalid metadata '" + key + "'"));
            return;
        }
    }
    if (request.deadline <= now)
    {
        FailSoon(std::move(done), Status(StatusCode::kDeadlineExceeded, "deadline exceeded before the call started"));
        return;
    }

    if (!maintenance_->Begin()) { FailSoon(std::move(done), Status(StatusCode::kUnavailable, "maintenance publications closed")); return; }
    const auto operation = std::make_shared<detail::MaintenanceOperation>(maintenance_);
    auto framed = std::make_shared<std::string>();
    AppendLengthPrefixedMessage(*framed, request.message);

    const uint64_t callId = nextCallId_++;
    Call call;
    call.id = callId;
    call.path = std::move(request.method);
    call.framedMessage = std::move(framed);
    call.metadata = std::move(request.metadata);
    call.deadline = request.deadline;
    const auto owner = std::make_shared<detail::UnaryCompletionOwner>();
    owner->operation = operation; owner->callback = std::move(done);
    call.done = [owner](UnaryCallResult&& result) { owner->Run(std::move(result)); };
    calls_.emplace(callId, std::move(call));
    deadlines_.emplace(request.deadline, callId);
    ArmDeadlineTimer();
    Dispatch(callId);
}

ConnectivityState Channel::GetState(bool tryToConnect)
{
    loop_.assertInLoopThread();
    if (tryToConnect && state_ == ConnectivityState::kIdle)
    {
        StartConnecting();
    }
    return state_;
}

void Channel::Dispatch(uint64_t callId)
{
    const auto it = calls_.find(callId);
    if (it == calls_.end() || it->second.active)
    {
        return;
    }
    switch (state_)
    {
    case ConnectivityState::kReady:
        StartOnCurrentTransport(it->second);
        break;
    case ConnectivityState::kIdle:
        pending_.push_back(callId);
        StartConnecting();
        break;
    case ConnectivityState::kConnecting:
        pending_.push_back(callId);
        break;
    case ConnectivityState::kTransientFailure:
        // 未开 wait_for_ready:与现网官方客户端一致,立即失败。
        CompleteCall(callId, FailureResult(Status(StatusCode::kUnavailable,
                                                  "failed to connect to " + target_ + ": " + lastFailure_.message())));
        break;
    case ConnectivityState::kShutdown:
        CompleteCall(callId, FailureResult(Status(StatusCode::kUnavailable, "channel shut down")));
        break;
    }
}

void Channel::StartOnCurrentTransport(Call &call)
{
    if (!current_ || !current_->ready() || current_->draining())
    {
        // 状态与连接对不上(不应发生):当作连接已退役,重新排队并换连接。
        pending_.push_back(call.id);
        RetireCurrentTransport();
        return;
    }
    UnaryRequestSpec spec;
    spec.path = call.path;
    spec.framedMessage = call.framedMessage;
    spec.metadata = call.metadata;
    spec.grpcTimeout = EncodeGrpcTimeout(
        std::chrono::duration_cast<std::chrono::nanoseconds>(call.deadline - std::chrono::steady_clock::now()));

    const std::shared_ptr<Http2Transport> transport = current_;
    const auto result = transport->StartUnaryStream(spec);
    if (result.streamId > 0)
    {
        call.active = true;
        call.transportId = transport->id();
        call.streamId = result.streamId;
        call.startedAt = std::chrono::steady_clock::now();
        // 先登记再发送:发送过程中同步到达的流结束事件要能找到这次调用。
        streamToCall_[{call.transportId, call.streamId}] = call.id;
        transport->FlushOutput();
        return;
    }
    if (result.retryOnNewConnection)
    {
        // 请求没有发出(stream id 用尽 / 连接已 GOAWAY):换新连接重排队是安全的。
        const uint64_t callId = call.id;
        RetireCurrentTransport();
        pending_.push_back(callId);
        if (state_ == ConnectivityState::kIdle)
        {
            StartConnecting();
        }
        return;
    }
    CompleteCall(call.id, FailureResult(result.error));
}

void Channel::FlushPending()
{
    flushPendingQueued_ = false;
    if (state_ != ConnectivityState::kReady)
    {
        return;
    }
    std::deque<uint64_t> pending;
    pending.swap(pending_);
    for (const uint64_t callId : pending)
    {
        Dispatch(callId); // 已完成的残留 id 在 Dispatch 里按 calls_ 查不到而跳过
    }
    std::deque<uint64_t> pendingStreams;
    pendingStreams.swap(pendingStreams_);
    for (const uint64_t streamRecordId : pendingStreams)
    {
        DispatchStream(streamRecordId); // 已取消 / 已结束的残留 id 按 streams_ 查不到而跳过
    }
}

void Channel::QueueFlushPending()
{
    if (flushPendingQueued_)
    {
        return;
    }
    flushPendingQueued_ = true;
    std::weak_ptr<Channel> weakSelf = weak_from_this();
    loop_.queueInLoop([weakSelf]
                      {
        if (const auto self = weakSelf.lock())
        {
            self->FlushPending();
        } });
}

void Channel::QueueRedispatch(uint64_t callId)
{
    std::weak_ptr<Channel> weakSelf = weak_from_this();
    loop_.queueInLoop([weakSelf, callId]
                      {
        if (const auto self = weakSelf.lock())
        {
            self->Dispatch(callId);
        } });
}

void Channel::CompleteCall(uint64_t callId, UnaryCallResult result)
{
    const auto it = calls_.find(callId);
    if (it == calls_.end())
    {
        return;
    }
    Call call = std::move(it->second);
    calls_.erase(it);
    deadlines_.erase({call.deadline, callId});
    if (call.active)
    {
        streamToCall_.erase({call.transportId, call.streamId});
    }
    // pending_ 里可能还留着这个 id:FlushPending / EnterTransientFailure 按 calls_ 查不到就跳过。
    loop_.queueInLoop([done = std::move(call.done), result = std::move(result)]() mutable
                      { done(std::move(result)); });
}

void Channel::FailSoon(UnaryCallback done, Status status)
{
    const auto owner = std::make_shared<detail::UnaryCompletionOwner>();
    owner->completion = std::make_shared<detail::MaintenanceCompletion>(maintenance_); owner->callback = std::move(done);
    loop_.queueInLoop([owner, status = std::move(status)]() mutable { owner->Run(FailureResult(std::move(status))); });
}

void Channel::ExpireCall(uint64_t callId)
{
    const auto it = calls_.find(callId);
    if (it == calls_.end())
    {
        return;
    }
    const Call &call = it->second;
    std::shared_ptr<Http2Transport> transport;
    int32_t streamId = 0;
    std::chrono::steady_clock::time_point startedAt{};
    if (call.active)
    {
        transport = FindTransport(call.transportId);
        streamId = call.streamId;
        startedAt = call.startedAt;
    }
    const bool wasActive = call.active;
    // 先完成调用(恰好一次由 calls_ 保证),再取消流:取消触发的流结束事件会因找不到登记而被忽略。
    CompleteCall(callId, FailureResult(Status(StatusCode::kDeadlineExceeded,
                                              wasActive ? "deadline exceeded"
                                                        : "deadline exceeded while waiting for a connection")));
    if (transport)
    {
        transport->OnCallDeadlineExpired(startedAt);
        transport->ResetStream(streamId, kHttp2Cancel, Status(StatusCode::kDeadlineExceeded, "deadline exceeded"));
    }
}

void Channel::OnTransportReady(Http2Transport &transport)
{
    if (current_.get() != &transport)
    {
        return;
    }
    LOG_INFO << "muduo_grpc channel " << target_ << " is READY (connection #" << transport.id() << ")";
    nextBackoffMs_ = static_cast<double>(options_.backoff.initial.count());
    lastFailure_ = Status::Ok();
    SetState(ConnectivityState::kReady);
    // 在 nghttp2 的回调栈里:不同步提交新流,投递到本轮之后。
    QueueFlushPending();
}

void Channel::OnTransportGoaway(Http2Transport &transport, int32_t lastStreamId, uint32_t errorCode,
                                std::string_view debugData)
{
    constexpr uint32_t kHttp2NoError = 0x0;
    if (errorCode == kHttp2NoError)
    {
        LOG_INFO << "muduo_grpc channel " << target_ << " received GOAWAY on connection #" << transport.id()
                 << " last_stream_id=" << lastStreamId << " debug='" << std::string(debugData)
                 << "'; new calls move to a new connection";
    }
    else
    {
        LOG_WARN << "muduo_grpc channel " << target_ << " received GOAWAY on connection #" << transport.id()
                 << " error_code=" << errorCode << " last_stream_id=" << lastStreamId << " debug='"
                 << std::string(debugData) << "'";
    }
    if (current_.get() == &transport)
    {
        RetireCurrentTransport();
    }
}

void Channel::OnTransportStreamClosed(Http2Transport &transport, int32_t streamId, StreamOutcome outcome)
{
    const auto mapping = streamToCall_.find({transport.id(), streamId});
    if (mapping == streamToCall_.end())
    {
        // 不是 unary 调用:看是不是双向流。流不做透明重试(§13.2),结果原样交给 onClose,由上层决定是否重建。
        if (const auto bidi = streamToBidi_.find({transport.id(), streamId}); bidi != streamToBidi_.end())
        {
            const uint64_t recordId = bidi->second;
            streamToBidi_.erase(bidi);
            if (const auto it = streams_.find(recordId); it != streams_.end())
            {
                const StreamRecordPtr record = it->second;
                record->active = false;
                CloseStream(record, std::move(outcome.status), std::move(outcome.trailingMetadata));
            }
        }
        MaybeReleaseDrainingTransport(transport); // 已提前完成(截止 / 析构)的调用,或刚结束的双向流
        return;
    }
    const uint64_t callId = mapping->second;
    streamToCall_.erase(mapping);
    const auto it = calls_.find(callId);
    if (it == calls_.end())
    {
        MaybeReleaseDrainingTransport(transport);
        return;
    }
    Call &call = it->second;
    call.active = false;
    call.streamId = 0;
    const auto now = std::chrono::steady_clock::now();

    if (outcome.unprocessed && !call.transparentRetried && now < call.deadline &&
        state_ != ConnectivityState::kShutdown)
    {
        // §4.6:对端可证明没处理 → 内部重发一次。投递到下一轮,不在 nghttp2 回调栈里提交新流。
        call.transparentRetried = true;
        QueueRedispatch(callId);
        MaybeReleaseDrainingTransport(transport);
        return;
    }
    if (outcome.status.code() == StatusCode::kCancelled && now >= call.deadline)
    {
        // 对端因 grpc-timeout 到期回 RST_STREAM(CANCEL):与 grpc-go 一样报 DEADLINE_EXCEEDED。
        outcome.status = Status(StatusCode::kDeadlineExceeded, "deadline exceeded (stream cancelled by peer)");
    }
    UnaryCallResult result;
    result.status = std::move(outcome.status);
    result.initialMetadata = std::move(outcome.initialMetadata);
    result.trailingMetadata = std::move(outcome.trailingMetadata);
    result.response = std::move(outcome.response);
    CompleteCall(callId, std::move(result));
    MaybeReleaseDrainingTransport(transport);
}

void Channel::OnTransportClosed(Http2Transport &transport, const Status &reason)
{
    if (current_.get() == &transport)
    {
        std::shared_ptr<Http2Transport> closed = std::move(current_);
        current_.reset();
        const ConnectivityState previous = state_;
        ReleaseTransport(std::move(closed));
        if (previous == ConnectivityState::kShutdown)
        {
            return;
        }
        if (previous == ConnectivityState::kReady)
        {
            // pick_first 语义:READY 连接断开回 IDLE,有排队调用就立即重连。
            SetState(ConnectivityState::kIdle);
            if (!pending_.empty() || !pendingStreams_.empty())
            {
                StartConnecting();
            }
            return;
        }
        EnterTransientFailure(reason);
        return;
    }
    const auto it = std::find_if(draining_.begin(), draining_.end(),
                                 [&transport](const std::shared_ptr<Http2Transport> &candidate)
                                 { return candidate.get() == &transport; });
    if (it != draining_.end())
    {
        std::shared_ptr<Http2Transport> closed = std::move(*it);
        draining_.erase(it);
        ReleaseTransport(std::move(closed));
    }
}

void Channel::StartConnecting()
{
    if (current_ || state_ == ConnectivityState::kShutdown)
    {
        return;
    }
    SetState(ConnectivityState::kConnecting);
    CreateTransport();
}

void Channel::CreateTransport()
{
    const uint64_t transportId = nextTransportId_++;
    std::weak_ptr<Http2TransportListener> listener = std::static_pointer_cast<Http2TransportListener>(shared_from_this());
    current_ = Http2Transport::Create(loop_, address_, "muduo_grpc:" + target_ + "#" + std::to_string(transportId),
                                      transportOptions_, std::move(listener), transportId);
    current_->Connect();
}

void Channel::RetireCurrentTransport()
{
    std::shared_ptr<Http2Transport> retired = std::move(current_);
    current_.reset();
    if (state_ != ConnectivityState::kShutdown && state_ != ConnectivityState::kTransientFailure)
    {
        SetState(ConnectivityState::kIdle);
    }
    if (retired)
    {
        retired->MarkDraining();
        if (retired->ActiveStreamCount() == 0)
        {
            ReleaseTransport(std::move(retired));
        }
        else
        {
            draining_.push_back(std::move(retired));
        }
    }
    if (state_ == ConnectivityState::kTransientFailure)
    {
        // 后台重连的那条连接被退役了(例如握手前就收到 GOAWAY):重新排一次退避,否则通道会卡在 TF。
        ScheduleReconnect();
    }
    else if (state_ == ConnectivityState::kIdle && (!pending_.empty() || !pendingStreams_.empty()))
    {
        StartConnecting();
    }
}

void Channel::EnterTransientFailure(const Status &reason)
{
    if (state_ != ConnectivityState::kTransientFailure)
    {
        LOG_WARN << "muduo_grpc channel " << target_ << " entered TRANSIENT_FAILURE: " << reason.ToString();
    }
    lastFailure_ = reason;
    SetState(ConnectivityState::kTransientFailure);
    std::deque<uint64_t> pending;
    pending.swap(pending_);
    for (const uint64_t callId : pending)
    {
        CompleteCall(callId, FailureResult(Status(StatusCode::kUnavailable,
                                                  "failed to connect to " + target_ + ": " + reason.message())));
    }
    std::deque<uint64_t> pendingStreams;
    pendingStreams.swap(pendingStreams_);
    for (const uint64_t streamRecordId : pendingStreams)
    {
        if (const auto it = streams_.find(streamRecordId); it != streams_.end())
        {
            const StreamRecordPtr record = it->second;
            CloseStream(record, Status(StatusCode::kUnavailable, "failed to connect to " + target_ + ": " + reason.message()),
                        Metadata());
        }
    }
    ScheduleReconnect();
}

void Channel::ScheduleReconnect()
{
    if (backoffTimerArmed_)
    {
        return;
    }
    // gRPC connection-backoff:本次等待 = 当前退避 × (1 ± jitter),然后退避 ×multiplier,封顶 max。
    std::uniform_real_distribution<double> spread(-options_.backoff.jitter, options_.backoff.jitter);
    const double delayMs = std::max(0.0, nextBackoffMs_ * (1.0 + spread(rng_)));
    nextBackoffMs_ = std::min(nextBackoffMs_ * options_.backoff.multiplier,
                              static_cast<double>(options_.backoff.max.count()));
    std::weak_ptr<Channel> weakSelf = weak_from_this();
    backoffTimer_ = loop_.runAfter(delayMs / 1000.0, [weakSelf]
                                   {
        if (const auto self = weakSelf.lock())
        {
            self->OnBackoffTimer();
        } });
    backoffTimerArmed_ = true;
}

void Channel::OnBackoffTimer()
{
    backoffTimerArmed_ = false;
    if (state_ != ConnectivityState::kTransientFailure || current_)
    {
        return;
    }
    // 后台重连期间对外仍报 TRANSIENT_FAILURE(新调用立即失败),连上才转 READY。
    CreateTransport();
}

void Channel::ReleaseTransport(std::shared_ptr<Http2Transport> transport)
{
    if (!transport)
    {
        return;
    }
    transport->Shutdown();
    // 最后一个引用放到排队任务里释放:此刻可能正处在这条连接自己的回调栈上(§11.7)。
    loop_.queueInLoop([transport = std::move(transport)]() mutable { transport.reset(); });
}

void Channel::MaybeReleaseDrainingTransport(Http2Transport &transport)
{
    // 已关闭的连接正在 FailAllStreams 里逐个报告流结束:这时释放会清空它的 listener,
    // 剩下的流结束事件就丢了(调用只能等到截止)。交给紧随其后的 OnTransportClosed 统一释放。
    if (current_.get() == &transport || transport.closed() || transport.ActiveStreamCount() != 0)
    {
        return;
    }
    const auto it = std::find_if(draining_.begin(), draining_.end(),
                                 [&transport](const std::shared_ptr<Http2Transport> &candidate)
                                 { return candidate.get() == &transport; });
    if (it == draining_.end())
    {
        return;
    }
    std::shared_ptr<Http2Transport> drained = std::move(*it);
    draining_.erase(it);
    ReleaseTransport(std::move(drained));
}

std::shared_ptr<Http2Transport> Channel::FindTransport(uint64_t transportId) const
{
    if (current_ && current_->id() == transportId)
    {
        return current_;
    }
    for (const auto &transport : draining_)
    {
        if (transport->id() == transportId)
        {
            return transport;
        }
    }
    return nullptr;
}

void Channel::ArmDeadlineTimer()
{
    if (deadlines_.empty())
    {
        return;
    }
    const auto earliest = deadlines_.begin()->first;
    if (deadlineTimerArmed_ && deadlineTimerFor_ <= earliest)
    {
        return;
    }
    if (deadlineTimerArmed_)
    {
        loop_.cancel(deadlineTimer_);
    }
    // muduo 定时器已按单调时钟到期(muduo-timer-cancellation-hazards.md「定时器时钟」),墙钟跳变不再影响;
    // 到点仍用 steady_clock 复核,未到就重新 arm(§6),兜住秒→微秒截断造成的微秒级早到。
    // 加 1ms 余量,避免这点截断让定时器早到一点而空转一轮。
    const double delaySeconds = ToSeconds(earliest - std::chrono::steady_clock::now()) + 0.001;
    std::weak_ptr<Channel> weakSelf = weak_from_this();
    deadlineTimer_ = loop_.runAfter(delaySeconds, [weakSelf]
                                    {
        if (const auto self = weakSelf.lock())
        {
            self->OnDeadlineTimer();
        } });
    deadlineTimerArmed_ = true;
    deadlineTimerFor_ = earliest;
}

void Channel::OnDeadlineTimer()
{
    deadlineTimerArmed_ = false;
    const auto now = std::chrono::steady_clock::now();
    std::vector<uint64_t> due;
    for (const auto &[deadline, callId] : deadlines_)
    {
        if (deadline > now)
        {
            break;
        }
        due.push_back(callId);
    }
    for (const uint64_t callId : due)
    {
        ExpireCall(callId);
    }
    ArmDeadlineTimer();
}

void Channel::SetState(ConnectivityState state)
{
    if (state_ == state)
    {
        return;
    }
    LOG_DEBUG << "muduo_grpc channel " << target_ << " state " << ConnectivityStateName(state_) << " -> "
              << ConnectivityStateName(state);
    state_ = state;
}

bool Channel::LoopIsCurrent() const
{
    return muduo::net::EventLoop::getEventLoopOfCurrentThread() == &loop_;
}

// ── 双向流(§13.2)──────────────────────────────────────────────────────────

ClientStream::~ClientStream()
{
    Cancel();
}

bool ClientStream::Write(std::string message)
{
    const auto channel = channel_.lock();
    return channel && channel->StreamWrite(id_, std::move(message));
}

void ClientStream::WritesDone()
{
    if (const auto channel = channel_.lock())
    {
        channel->StreamWritesDone(id_);
    }
}

void ClientStream::Cancel()
{
    // 先就地压住回调:流可能已结束、onClose 还在队列里,通道甚至可能已析构(记录由排队的回调持有)。
    if (const auto record = record_.lock())
    {
        record->cancelled = true;
    }
    if (const auto channel = channel_.lock())
    {
        channel->StreamCancel(id_);
    }
}

bool ClientStream::closed() const
{
    const auto channel = channel_.lock();
    const auto record = record_.lock();
    return !channel || !record || record->closed || channel->StreamIsClosed(id_);
}

std::shared_ptr<ClientStream> Channel::StartStream(std::string method, Metadata metadata, StreamCallbacks callbacks)
{
    loop_.assertInLoopThread();
    auto record = std::make_shared<StreamRecord>();
    record->id = nextCallId_++;
    record->path = std::move(method);
    record->metadata = std::move(metadata);
    record->callbacks = std::move(callbacks);
    // ClientStream 的构造函数是私有的(只给 Channel 用),这里不能用 make_shared。
    std::shared_ptr<ClientStream> handle(new ClientStream(weak_from_this(), record, record->id));

    if (!record->callbacks.onClose)
    {
        // onClose 是唯一的结束通知:没有它调用方永远不知道流结束了。按调用方错误拒绝,不开流。
        LOG_ERROR << "muduo_grpc::Channel::StartStream called without an onClose callback, method=" << record->path;
        record->closed = true;
        return handle;
    }
    for (const auto &[key, value] : record->metadata)
    {
        if (!IsValidCustomMetadataKey(key) || (!IsBinaryHeaderKey(key) && !IsValidAsciiMetadataValue(value)))
        {
            streams_.emplace(record->id, record);
            CloseStream(record, Status(StatusCode::kInternal, "invalid metadata '" + key + "'"), Metadata());
            return handle;
        }
    }
    if (!maintenance_->Begin()) {
        streams_.emplace(record->id, record);
        CloseStream(record, Status(StatusCode::kUnavailable, "maintenance publications closed"), Metadata());
        return handle;
    }
    record->maintenance = std::make_shared<detail::MaintenanceOperation>(maintenance_);
    streams_.emplace(record->id, record);
    DispatchStream(record->id);
    return handle;
}

void Channel::DispatchStream(uint64_t id)
{
    const auto it = streams_.find(id);
    if (it == streams_.end() || it->second->active || it->second->closed)
    {
        return;
    }
    const StreamRecordPtr record = it->second;
    switch (state_)
    {
    case ConnectivityState::kReady:
        OpenStreamOnCurrentTransport(record);
        break;
    case ConnectivityState::kIdle:
        pendingStreams_.push_back(id);
        StartConnecting();
        break;
    case ConnectivityState::kConnecting:
        pendingStreams_.push_back(id);
        break;
    case ConnectivityState::kTransientFailure:
        CloseStream(record, Status(StatusCode::kUnavailable, "failed to connect to " + target_ + ": " + lastFailure_.message()),
                    Metadata());
        break;
    case ConnectivityState::kShutdown:
        CloseStream(record, Status(StatusCode::kUnavailable, "channel shut down"), Metadata());
        break;
    }
}

void Channel::OpenStreamOnCurrentTransport(const StreamRecordPtr &record)
{
    if (!current_ || !current_->ready() || current_->draining())
    {
        pendingStreams_.push_back(record->id);
        RetireCurrentTransport();
        return;
    }
    const std::shared_ptr<Http2Transport> transport = current_;
    const auto result = transport->StartBidiStream(record->path, record->metadata);
    if (result.streamId > 0)
    {
        record->active = true;
        record->transportId = transport->id();
        record->streamId = result.streamId;
        // 先登记再发送(理由同 StartOnCurrentTransport)。
        streamToBidi_[{record->transportId, record->streamId}] = record->id;
        for (const auto &framed : record->pendingFramed)
        {
            transport->WriteStreamMessage(record->streamId, framed);
        }
        record->pendingFramed.clear();
        record->pendingBytes = 0;
        if (record->writesDone)
        {
            transport->HalfCloseStream(record->streamId);
        }
        transport->FlushOutput();
        return;
    }
    if (result.retryOnNewConnection)
    {
        // 请求头没有发出:换新连接再开是安全的。
        RetireCurrentTransport();
        pendingStreams_.push_back(record->id);
        if (state_ == ConnectivityState::kIdle)
        {
            StartConnecting();
        }
        return;
    }
    CloseStream(record, result.error, Metadata());
}

void Channel::CloseStream(const StreamRecordPtr &record, Status status, Metadata trailingMetadata)
{
    if (record->closed)
    {
        return;
    }
    record->closed = true;
    if (record->active)
    {
        streamToBidi_.erase({record->transportId, record->streamId});
        record->active = false;
    }
    streams_.erase(record->id);
    // pendingStreams_ 里可能还留着这个 id:FlushPending / EnterTransientFailure 按 streams_ 查不到就跳过。
    const auto owner = std::make_shared<detail::StreamCompletionOwner>(maintenance_, record);
    loop_.queueInLoop([owner, status = std::move(status), trailing = std::move(trailingMetadata)]
                      {
        const auto& record = owner->record;
        if (!record->cancelled && record->callbacks.onClose)
        {
            record->callbacks.onClose(status, trailing);
            if (record->maintenance) record->maintenance->known = status.ok();
        }
        owner->completion->returned = true;
    });
}

bool Channel::StreamWrite(uint64_t id, std::string message)
{
    loop_.assertInLoopThread();
    const auto it = streams_.find(id);
    if (it == streams_.end())
    {
        return false;
    }
    const StreamRecordPtr record = it->second;
    if (maintenance_->Closed() || record->closed || record->cancelled || record->writesDone)
    {
        return false;
    }
    if (message.size() > options_.maxSendMessageBytes)
    {
        StreamCancelForError(record, Status(StatusCode::kResourceExhausted,
                                            "sent message larger than max (" + std::to_string(message.size()) +
                                                " vs. " + std::to_string(options_.maxSendMessageBytes) + ")"));
        return false;
    }
    std::string framed;
    AppendLengthPrefixedMessage(framed, message);

    // 待发上限只在「已经有积压」时生效:缓冲为空时总放行一条(单条大小已受 maxSendMessageBytes 约束),
    // 否则大于 maxStreamPendingSendBytes 的单条消息永远发不出去。内存上界 = max(上限, 一条消息) + 一条消息。
    const auto overLimit = [this, &framed](size_t pending)
    { return pending != 0 && pending + framed.size() > options_.maxStreamPendingSendBytes; };
    if (!record->active)
    {
        if (overLimit(record->pendingBytes))
        {
            StreamCancelForError(record, Status(StatusCode::kResourceExhausted,
                                                "stream send buffer full (" + std::to_string(record->pendingBytes) +
                                                    " bytes pending before the stream opened)"));
            return false;
        }
        record->pendingBytes += framed.size();
        record->pendingFramed.push_back(std::move(framed));
        return true;
    }
    const auto transport = FindTransport(record->transportId);
    if (!transport)
    {
        return false; // 连接正在拆,流结束事件马上会到
    }
    if (const size_t pending = transport->PendingStreamBytes(record->streamId); overLimit(pending))
    {
        StreamCancelForError(record, Status(StatusCode::kResourceExhausted,
                                            "stream send buffer full (" + std::to_string(pending) +
                                                " bytes not yet accepted by flow control)"));
        return false;
    }
    return transport->WriteStreamMessage(record->streamId, framed);
}

void Channel::StreamCancelForError(const StreamRecordPtr &record, Status status)
{
    // 本端放弃这个流:对端发 RST_STREAM(CANCEL),本端经 onClose 报告原因(不是 Cancel 的「静默」语义)。
    if (record->active)
    {
        if (const auto transport = FindTransport(record->transportId))
        {
            const int32_t streamId = record->streamId;
            CloseStream(record, status, Metadata());
            transport->ResetStream(streamId, kHttp2Cancel, std::move(status));
            return;
        }
    }
    CloseStream(record, std::move(status), Metadata());
}

void Channel::StreamWritesDone(uint64_t id)
{
    loop_.assertInLoopThread();
    const auto it = streams_.find(id);
    if (it == streams_.end() || it->second->closed || it->second->writesDone)
    {
        return;
    }
    const StreamRecordPtr record = it->second;
    record->writesDone = true;
    if (record->active)
    {
        if (const auto transport = FindTransport(record->transportId))
        {
            transport->HalfCloseStream(record->streamId);
        }
    }
}

void Channel::StreamCancel(uint64_t id)
{
    if (!LoopIsCurrent())
    {
        // 句柄在 loop 线程之外析构(漏了关机清理):ClientStream::Cancel 已就地压住回调,这里不动 streams_ / 连接,
        // 否则会与 loop 线程竞争。流随通道析构或连接断开结束。
        LOG_ERROR << "muduo_grpc::ClientStream cancelled outside its EventLoop thread on channel " << target_
                  << "; the stream is left to the channel teardown";
        return;
    }
    const auto it = streams_.find(id);
    if (it == streams_.end())
    {
        return; // 已结束或已取消
    }
    const StreamRecordPtr record = it->second;
    record->cancelled = true; // 之后排队中的回调一律不执行
    record->closed = true;
    streams_.erase(it);
    if (record->active)
    {
        streamToBidi_.erase({record->transportId, record->streamId});
        record->active = false;
        if (const auto transport = FindTransport(record->transportId))
        {
            transport->ResetStream(record->streamId, kHttp2Cancel, Status(StatusCode::kCancelled, "cancelled by client"));
        }
    }
}

bool Channel::StreamIsClosed(uint64_t id) const
{
    return streams_.find(id) == streams_.end();
}

void Channel::OnTransportStreamHeaders(Http2Transport &transport, int32_t streamId, const Metadata &initialMetadata)
{
    const auto bidi = streamToBidi_.find({transport.id(), streamId});
    if (bidi == streamToBidi_.end())
    {
        return;
    }
    const auto it = streams_.find(bidi->second);
    if (it == streams_.end() || !it->second->callbacks.onHeaders)
    {
        return;
    }
    const StreamRecordPtr record = it->second;
    const auto owner = std::make_shared<detail::StreamCompletionOwner>(maintenance_, record);
    loop_.queueInLoop([owner, metadata = initialMetadata]
                      {
        const auto& record = owner->record;
        if (!record->cancelled)
        {
            record->callbacks.onHeaders(metadata);
        }
        owner->completion->returned = true;
    });
}

void Channel::OnTransportStreamMessage(Http2Transport &transport, int32_t streamId, std::string message)
{
    const auto bidi = streamToBidi_.find({transport.id(), streamId});
    if (bidi == streamToBidi_.end())
    {
        return;
    }
    const auto it = streams_.find(bidi->second);
    if (it == streams_.end() || !it->second->callbacks.onMessage)
    {
        return;
    }
    const StreamRecordPtr record = it->second;
    const auto owner = std::make_shared<detail::StreamCompletionOwner>(maintenance_, record);
    loop_.queueInLoop([owner, message = std::move(message)]() mutable
                      {
        const auto& record = owner->record;
        if (!record->cancelled)
        {
            record->callbacks.onMessage(std::move(message));
        }
        owner->completion->returned = true;
    });
}

} // namespace muduo_grpc
