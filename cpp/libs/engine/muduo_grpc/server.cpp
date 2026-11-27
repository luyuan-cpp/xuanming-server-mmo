// 标准库头放在最前面:muduo 的 Windows 适配头 #define 了 off_t / pid_t 等名字(server.h 自己也是先标准库后 muduo)。
#include <algorithm>
#include <atomic>
#include <chrono>
#include <cstdint>
#include <map>
#include <memory>
#include <optional>
#include <set>
#include <string>
#include <string_view>
#include <thread>
#include <unordered_map>
#include <utility>
#include <vector>

#include "server.h"

#include "muduo/base/Logging.h"
#include "muduo/net/Acceptor.h"
#include "muduo/net/Buffer.h"
#include "muduo/net/EventLoop.h"
#include "muduo/net/SocketsOps.h"
#include "muduo/net/TcpConnection.h"

#include "address.h"

namespace muduo_grpc
{

namespace
{

using SteadyClock = std::chrono::steady_clock;
using Verdict = ServerSession::Verdict;

// muduo 一行日志的缓冲是 4000 字节,超长的片段会被整段丢弃:汇总行超过这个长度就截断后再打。
constexpr size_t kMaxSummaryLogBytes = 3500;

// 与 core 的 ShouldLogRpcEvent 同一规则(前 8 条逐条,之后每 1024 条一条);muduo_grpc 不依赖 core,在此本地实现。
bool ShouldLogServerEvent(uint64_t occurrence)
{
    return occurrence <= 8 || (occurrence & 1023) == 0;
}

double ToSeconds(SteadyClock::duration duration)
{
    return std::max(0.0, std::chrono::duration<double>(duration).count());
}

// 进程级:句柄在 loop 线程之外析构的次数(此时绝不能 lock Server)。
std::atomic<uint64_t> &DroppedOffLoopCounter()
{
    static std::atomic<uint64_t> counter{0};
    return counter;
}

// 截止 = start + timeout,饱和加法:99999999H 会被钳到 nanoseconds::max(),直接相加会溢出(§15.5.4)。
std::optional<SteadyClock::time_point> DeadlineFrom(SteadyClock::time_point start, std::chrono::nanoseconds timeout)
{
    const auto remaining = std::chrono::duration_cast<std::chrono::nanoseconds>(SteadyClock::time_point::max() - start);
    if (timeout >= remaining)
    {
        return std::nullopt; // 视为无截止
    }
    return start + std::chrono::duration_cast<SteadyClock::duration>(timeout);
}

const std::string &EmptyString()
{
    static const std::string kEmpty;
    return kEmpty;
}

const Metadata &EmptyMetadata()
{
    static const Metadata kEmpty;
    return kEmpty;
}

Verdict AcceptVerdict()
{
    return Verdict{Verdict::Kind::kAccept, Status()};
}

Verdict RejectVerdict(Status status)
{
    return Verdict{Verdict::Kind::kReject, std::move(status)};
}

Verdict RefuseVerdict()
{
    return Verdict{Verdict::Kind::kRefuse, Status()};
}

// ServerCall 除只读访问外只能在 loop 线程调用:与 muduo assertInLoopThread 同样 fail-fast。
void RequireLoopThread(const detail::CallToken &token, const char *what)
{
    if (std::this_thread::get_id() != token.loopThread)
    {
        LOG_FATAL << "muduo_grpc::ServerCall::" << what
                  << " called outside the server EventLoop thread, method=" << token.method;
    }
}

std::string FormatBudget(std::chrono::microseconds budget)
{
    if (budget.count() % 1000 == 0)
    {
        return std::to_string(budget.count() / 1000) + "ms";
    }
    return std::to_string(budget.count()) + "us";
}

// 执行一次 unary handler;装了派发拦截时经它包一层(ServerOptions::dispatchInterceptor)。
// 拦截器多调 invoke 的被忽略,一次都没调的在这里补调:handler 恰好执行一次。
// methodPath 引用方法表里的 ":path",服务端存活期间不变(方法表在 Start 之后不再修改)。
void InvokeUnaryHandler(const UnaryDispatchInterceptor &interceptor, const UnaryMethodHandler &handler,
                        ServerCall call, std::string_view message, const std::string &methodName,
                        const std::string &methodPath)
{
    if (!interceptor)
    {
        handler(std::move(call), message);
        return;
    }
    // metadata 在句柄共享的 CallToken 里(堆上):invoke 把句柄交给 handler 之前一直可读;之后 handler 可能同步完成并释放它。
    const Metadata &metadata = call.metadata();
    bool invoked = false;
    const std::function<void()> invoke = [&]
    {
        if (invoked)
        {
            return;
        }
        invoked = true;
        handler(std::move(call), message);
    };
    interceptor(methodPath, metadata, invoke);
    if (!invoked)
    {
        LOG_ERROR << "[GrpcServer] dispatch interceptor did not invoke the handler; invoking it now, method="
                  << methodName;
        invoke();
    }
}

} // namespace

// ── ServerCall / CallToken ─────────────────────────────────────────────

ServerCall::ServerCall(std::shared_ptr<detail::CallToken> token) : token(std::move(token))
{
}

uint64_t ServerCall::id() const
{
    return token ? token->callId : 0;
}

const std::string &ServerCall::method() const
{
    return token ? token->method : EmptyString();
}

const std::string &ServerCall::peer() const
{
    return token ? token->peer : EmptyString();
}

const Metadata &ServerCall::metadata() const
{
    return token ? token->metadata : EmptyMetadata();
}

std::optional<std::chrono::steady_clock::time_point> ServerCall::deadline() const
{
    return token ? token->deadline : std::nullopt;
}

bool ServerCall::IsActive() const
{
    if (!token)
    {
        return false;
    }
    RequireLoopThread(*token, "IsActive");
    const auto server = token->server.lock();
    return server && server->calls.count(token->callId) != 0;
}

bool ServerCall::ServerDraining() const
{
    if (!token)
    {
        return true;
    }
    RequireLoopThread(*token, "ServerDraining");
    const auto server = token->server.lock();
    return !server || server->draining();
}

bool ServerCall::TryStartBusiness() const
{
    if (!token)
    {
        return false;
    }
    RequireLoopThread(*token, "TryStartBusiness");
    const auto server = token->server.lock();
    return server && server->TryStartBusiness(token->callId);
}

void ServerCall::FinishOk(std::string responseBytes) const
{
    if (!token)
    {
        LOG_ERROR << "muduo_grpc::ServerCall::FinishOk called on an empty call handle";
        return;
    }
    RequireLoopThread(*token, "FinishOk");
    const auto server = token->server.lock();
    if (!server)
    {
        return; // 服务端已销毁:~Server 已静默收口全部调用
    }
    Server::CallEnd end;
    end.kind = Server::CallEndKind::kResponse;
    end.response = std::move(responseBytes);
    server->FinishFromHandler(*token, std::move(end));
}

void ServerCall::FinishError(Status status) const
{
    if (!token)
    {
        LOG_ERROR << "muduo_grpc::ServerCall::FinishError called on an empty call handle";
        return;
    }
    RequireLoopThread(*token, "FinishError");
    if (status.ok())
    {
        LOG_ERROR << "muduo_grpc::ServerCall::FinishError called with an OK status, method=" << token->method
                  << "; replying INTERNAL";
        status = Status(StatusCode::kInternal, "server finished the call with an OK status but no response");
    }
    const auto server = token->server.lock();
    if (!server)
    {
        return;
    }
    Server::CallEnd end;
    end.kind = Server::CallEndKind::kStatus;
    end.status = std::move(status);
    server->FinishFromHandler(*token, std::move(end));
}

detail::CallToken::~CallToken()
{
    if (std::this_thread::get_id() != loopThread)
    {
        // 不在 loop 线程:只计数,绝不 lock Server(lock 出来的临时强引用若是最后一个,Server 会在这个线程析构)。
        // 此时无从判断调用是否已完成,所以每次 loop 外析构都计一次;正常路径上恒为 0。
        DroppedOffLoopCounter().fetch_add(1, std::memory_order_relaxed);
        return;
    }
    if (const auto owner = server.lock())
    {
        owner->OnTokenDropped(*this);
    }
}

// ── detail::ServerConnection ──────────────────────────────────────────

namespace detail
{

// 每条连接一个:ServerSession 的宿主与 Observer,把字节在 muduo 连接与会话之间搬运。
// 只存 weak<TcpConnection>、weak<Server>(AGENTS §11.7);强持有者只有 Server::connections_ 与 connectDestroyed 闭包。
// 连接自己的定时器(握手 / GOAWAY-PING / 关闭宽限)只绑 weak<ServerConnection>。
// 不在它自己的回调栈上析构:摘表时 Server 把它移进 connectDestroyed 闭包一起释放。
class ServerConnection final : public std::enable_shared_from_this<ServerConnection>, private ServerSession::Observer
{
public:
    ServerConnection(muduo::net::EventLoop &loop, std::weak_ptr<Server> server, uint64_t id, std::string peer,
                     const ServerOptions &options)
        : loop(loop),
          server(std::move(server)),
          id(id),
          peer(std::move(peer)),
          maxPendingOutputBytes(options.maxPendingOutputBytes),
          handshakeTimeout(options.handshakeTimeout),
          maxTrackedHeaderTimes(std::max<size_t>(16, static_cast<size_t>(options.session.maxConcurrentStreams) * 2)),
          session(options.session, static_cast<ServerSession::Observer &>(*this))
    {
    }

    ~ServerConnection() override { CancelTimers(); }

    ServerConnection(const ServerConnection &) = delete;
    ServerConnection &operator=(const ServerConnection &) = delete;

    uint64_t Id() const { return id; }
    const std::string &Peer() const { return peer; }
    bool receivedAnyBytes() const { return session.ReceivedAnyBytes(); }

    // connectEstablished 之后调用:建会话、上弦握手超时。本端 SETTINGS 只提交进 nghttp2,
    // 收到对端首字节之前不写(§15.5.1)。
    void Start(const muduo::net::TcpConnectionPtr &conn)
    {
        tcp = conn;
        const Status started = session.Start();
        if (!started.ok())
        {
            LOG_ERROR << "[GrpcServer] failed to start the HTTP/2 session for " << peer << ": " << started.ToString();
            ForceClose();
            return;
        }
        if (handshakeTimeout.count() > 0)
        {
            handshakeDeadline = SteadyClock::now() + handshakeTimeout;
            ArmHandshakeTimer();
        }
    }

    void HandleMessage(muduo::net::Buffer &buffer)
    {
        if (closed)
        {
            buffer.retrieveAll();
            return;
        }
        // muduo 一次读事件只做一次 readv;nghttp2 同步解完,这一步只做分帧和 HPACK(§15.6)。
        ++sessionDepth;
        const Status status =
            session.Receive(reinterpret_cast<const uint8_t *>(buffer.peek()), buffer.readableBytes());
        --sessionDepth;
        buffer.retrieveAll();
        if (closed)
        {
            return;
        }
        if (!status.ok())
        {
            // 坏 magic / 洪泛 / nghttp2 内部失败:会话不再写任何字节,直接关(§15.5.1)。
            ReportProtocolError(status);
            ForceClose();
            return;
        }
        ApplyDeferredResponses();
        if (finalGoawayRequested)
        {
            // PING ACK 在 Receive 的回调栈里到达;最终 GOAWAY 推迟到这里提交。
            finalGoawayRequested = false;
            SubmitFinalGoaway();
        }
        // 首次:本端 SETTINGS;之后:SETTINGS ACK / PING ACK / WINDOW_UPDATE / 早拒,以及对端 WINDOW_UPDATE 放行的应答 DATA;
        // 优雅关闭期间对端 RST / WINDOW_UPDATE 让最后的流关闭后,也由这里判定写半关。
        FlushOutput();
    }

    // 照抄 Http2Transport::FlushOutput 的 sessionDepth_ / flushDeferred_:不在会话回调栈里重入 nghttp2;
    // 输出缓冲达到上限就暂停取帧,writeComplete 后继续。优雅关闭期间同样守这个上限:写半关要等会话把帧全部交出
    // (ShutdownWriteWhenSessionDone),连接在此之前照常读写,不需要「忽略上限一次取完」的最终冲出。
    void FlushOutput()
    {
        for (;;)
        {
            if (closed || closing || !session.ReceivedAnyBytes())
            {
                return; // 已摘表 / 已写半关 / 已 forceClose:不再写;收到对端首字节之前不写(会话层也有同样的检查)
            }
            if (sessionDepth > 0)
            {
                flushDeferred = true;
                return;
            }
            const auto conn = tcp.lock();
            if (!conn || !conn->connected())
            {
                return; // 已断开
            }
            if (conn->outputBuffer()->readableBytes() >= maxPendingOutputBytes)
            {
                return; // writeComplete 后继续;能否写半关也到那时再判断
            }
            flushDeferred = false;
            const Status status = DrainSessionInto(*conn);
            if (!status.ok())
            {
                // nghttp2 因对端违约发出了带错误码的 GOAWAY(字节已交给 send),或 mem_send 失败:会话从此既不读也不写
                // (ShouldClose 为真),下面随即写半关;进入优雅关闭让硬上限照常兜底(§15.5.6「尽力冲出后 forceClose」)。
                ReportProtocolError(status);
                StartGracefulClose();
                break;
            }
            if (!deferred.empty())
            {
                ApplyDeferredResponses();
                flushDeferred = true;
            }
            if (!flushDeferred)
            {
                break;
            }
        }
        ShutdownWriteWhenSessionDone();
    }

    // CompleteCall 的应答出口。对端已走(流已关闭 / 已应答)时 Submit* 返回 false,忽略即可。
    void Respond(int32_t streamId, Server::CallEnd end)
    {
        if (closed || end.kind == Server::CallEndKind::kSilent)
        {
            return;
        }
        if (sessionDepth > 0)
        {
            // 不在 nghttp2 回调栈里提交(ServerSession 的约定);回调返回后由 ApplyDeferredResponses 补交。
            deferred.push_back(DeferredResponse{streamId, std::move(end)});
            flushDeferred = true;
            return;
        }
        SubmitNow(streamId, std::move(end));
    }

    void BindStream(int32_t streamId, uint64_t callId) { streamCalls[streamId] = callId; }
    void ForgetStream(int32_t streamId) { streamCalls.erase(streamId); }

    std::optional<uint64_t> TakeStream(int32_t streamId)
    {
        const auto it = streamCalls.find(streamId);
        if (it == streamCalls.end())
        {
            return std::nullopt;
        }
        const uint64_t callId = it->second;
        streamCalls.erase(it);
        return callId;
    }

    std::vector<uint64_t> TakeAllCalls()
    {
        std::vector<uint64_t> callIds;
        callIds.reserve(streamCalls.size());
        for (const auto &entry : streamCalls)
        {
            callIds.push_back(entry.second);
        }
        streamCalls.clear();
        std::sort(callIds.begin(), callIds.end());
        return callIds;
    }

    // 两段式 GOAWAY 第一段(§15.5.6):GOAWAY(2^31-1) + PING;第二段在 PING ACK 或满 goawayPingTimeout 时发。
    // 冲出由调用方负责(Server::BeginShutdown 标记 dirty)。
    void BeginShutdown(std::chrono::milliseconds goawayPingTimeout, std::chrono::milliseconds closeGrace)
    {
        if (closed || shutdownStarted)
        {
            return;
        }
        shutdownStarted = true;
        this->closeGrace = closeGrace;
        if (closing || gracefulCloseStarted)
        {
            return; // 已在关闭(协议错误 / 对端 GOAWAY 之后):不再发 GOAWAY
        }
        session.SubmitShutdownNotice();
        pingDeadline = SteadyClock::now() + goawayPingTimeout;
        ArmPingTimer();
    }

    // 排空到期(§15.10)。Server 已把调用表里的调用全部收口(业务未开始 → REFUSED_STREAM,已开始 → UNAVAILABLE);
    // 这里再拒掉请求体还在上传的流、补最终 GOAWAY,然后进入优雅关闭 —— 不立即写半关:连接继续读写,已提交的应答
    // (含被对端流控窗口卡住、要等对端 WINDOW_UPDATE 才能发的 DATA 与 trailers)全部写出、所有流都关闭之后才写半关;
    // closeGrace 从这一刻起算,是硬上限,到期仍没关就 forceClose。此后到达的新流(id 大于最终 GOAWAY 的 last_stream_id)
    // 不会交给 Server:写出之前到达的由 nghttp2 在 GOAWAY 写出时关掉,之后到达的直接忽略,客户端都按 last_stream_id
    // 判为未处理(见 ServerSession::SubmitFinalGoaway);到期时头部还没收完的老流,收完头部时由 Server 回 REFUSED_STREAM。
    // 不在这里冲出:调用方(Server::OnDrainTimer)标记 dirty,批处理结束时按背压统一冲出。
    // 返回本次拒掉的上传中流的个数(调用方计入 refused_on_shutdown)。调用方保证不在会话回调栈里。
    size_t ExpireDrain()
    {
        if (closed || closing)
        {
            return 0; // 已写半关或已 forceClose:之后什么都写不出去
        }
        if (!session.ReceivedAnyBytes())
        {
            ForceClose();
            return 0;
        }
        const size_t refusedUploads = RefuseUnfinishedRequests();
        SubmitFinalGoaway();
        StartGracefulClose();
        return refusedUploads;
    }

    void ForceClose()
    {
        closing = true;
        if (const auto conn = tcp.lock())
        {
            conn->forceClose();
        }
    }

    // 摘表时调用:取消定时器,之后不再读写、不再回调 Server。
    void Close()
    {
        closed = true;
        closing = true;
        CancelTimers();
        deferred.clear();
        headersReceivedAt.clear();
    }

private:
    struct DeferredResponse
    {
        int32_t streamId{0};
        Server::CallEnd end;
    };

    // ---- ServerSession::Observer:全部在 Receive / DrainOutput 的 nghttp2 回调栈里,只查表、登记、入队 ----

    void OnClientSettings() override
    {
        if (handshakeTimerArmed)
        {
            loop.cancel(handshakeTimer);
            handshakeTimerArmed = false;
        }
    }

    Verdict OnRequestHeaders(int32_t streamId, const RequestHeaders &headers) override
    {
        const auto server = this->server.lock();
        if (!server)
        {
            return RefuseVerdict();
        }
        Verdict verdict = server->RouteRequest(headers);
        if (verdict.kind == Verdict::Kind::kAccept)
        {
            RememberHeadersTime(streamId);
        }
        return verdict;
    }

    Verdict OnRequestComplete(int32_t streamId, RequestHeaders headers, std::string message) override
    {
        // 截止从头部收完时起算(§15.5.4);记录被修剪掉(见 RememberHeadersTime)时退回「现在」。
        SteadyClock::time_point headersAt = SteadyClock::now();
        if (const auto it = headersReceivedAt.find(streamId); it != headersReceivedAt.end())
        {
            headersAt = it->second;
            headersReceivedAt.erase(it);
        }
        const auto server = this->server.lock();
        if (!server)
        {
            return RefuseVerdict();
        }
        return server->AcceptCall(*this, streamId, std::move(headers), std::move(message), headersAt);
    }

    void OnStreamAborted(int32_t streamId, uint32_t /*http2ErrorCode*/) override
    {
        headersReceivedAt.erase(streamId);
        if (const auto server = this->server.lock())
        {
            server->AbortStream(*this, streamId);
        }
    }

    void OnShutdownPingAck() override
    {
        // 在 Receive 的回调栈里:不得同步提交,HandleMessage 在 Receive 返回后提交最终 GOAWAY。
        finalGoawayRequested = true;
        if (pingTimerArmed)
        {
            loop.cancel(pingTimer);
            pingTimerArmed = false;
        }
    }

    void OnRequestRejected(int32_t streamId, int httpStatus, const Status &status) override
    {
        headersReceivedAt.erase(streamId);
        if (const auto server = this->server.lock())
        {
            server->OnSessionRejected(*this, httpStatus, status);
        }
    }

    // 头部收完时刻按流登记,收完请求时取走;只用于截止起算。头部之后被对端 RST 或被 nghttp2 关掉的流不会再有回调,
    // 记录会残留:超过上限就丢掉最老的一半(流 id 单调递增;被丢的流若仍在上传,截止退回从收完时起算)。
    // 排空到期要拒的上传中流不看这份记录(见 RefuseUnfinishedRequests),修剪不影响停机。
    void RememberHeadersTime(int32_t streamId)
    {
        headersReceivedAt[streamId] = SteadyClock::now();
        if (headersReceivedAt.size() <= maxTrackedHeaderTimes)
        {
            return;
        }
        while (headersReceivedAt.size() > maxTrackedHeaderTimes / 2)
        {
            headersReceivedAt.erase(headersReceivedAt.begin());
        }
    }

    // 每条连接只计一次 protocol_errors(Receive 的致命错误是粘滞的,forceClose 之前可能再报)。
    void ReportProtocolError(const Status &status)
    {
        if (protocolErrorReported)
        {
            return;
        }
        protocolErrorReported = true;
        if (const auto server = this->server.lock())
        {
            server->OnConnectionProtocolError(*this, status);
        }
    }

    void SubmitNow(int32_t streamId, Server::CallEnd end)
    {
        switch (end.kind)
        {
        case Server::CallEndKind::kResponse:
            session.SubmitResponse(streamId, std::move(end.response));
            return;
        case Server::CallEndKind::kStatus:
            session.SubmitStatus(streamId, end.status);
            return;
        case Server::CallEndKind::kRefuse:
            session.RefuseStream(streamId);
            return;
        case Server::CallEndKind::kSilent:
            return;
        }
    }

    void ApplyDeferredResponses()
    {
        if (deferred.empty() || sessionDepth > 0)
        {
            return;
        }
        std::vector<DeferredResponse> pending;
        pending.swap(deferred);
        for (auto &response : pending)
        {
            SubmitNow(response.streamId, std::move(response.end));
        }
    }

    void SubmitFinalGoaway()
    {
        if (sessionDepth > 0)
        {
            finalGoawayRequested = true;
            return;
        }
        if (pingTimerArmed)
        {
            loop.cancel(pingTimer);
            pingTimerArmed = false;
        }
        session.SubmitFinalGoaway();
    }

    // 把 nghttp2 里排队的帧交给 muduo;输出缓冲达到 maxPendingOutputBytes_ 就先停(writeComplete 后由 FlushOutput 继续)。
    // 调用方保证不在会话回调栈里(sessionDepth_ == 0)。
    Status DrainSessionInto(muduo::net::TcpConnection &conn)
    {
        ++sessionDepth;
        const Status status = session.DrainOutput([this, &conn](std::string_view chunk)
                                                   {
            conn.send(chunk.data(), static_cast<int>(chunk.size()));
            return conn.outputBuffer()->readableBytes() < maxPendingOutputBytes; });
        --sessionDepth;
        return status;
    }

    // 排空到期时请求体还在上传的流(头部已被接受、请求还没收完):业务必然没开始,立即回 REFUSED_STREAM
    // (RFC 9113 §8.7「未处理」,grpc-go 透明重试到新连接),不等对端传完再拒,也不让它拖住写半关直到硬上限。
    // 流集合以会话的状态为准(ServerSession::AcceptedUndispatchedStreams),不用 headersReceivedAt_:那份记录有上限,
    // 超限修剪后慢上传的流会漏拒,只能等它传完或等 closeGrace 硬上限。headersReceivedAt_ 只为截止起算,这些流都已拒掉,一并清空。
    // 调用方保证不在会话回调栈里(ServerSession 的约定)。
    size_t RefuseUnfinishedRequests()
    {
        size_t refused = 0;
        for (const int32_t streamId : session.AcceptedUndispatchedStreams())
        {
            if (session.RefuseStream(streamId))
            {
                ++refused;
            }
        }
        headersReceivedAt.clear();
        return refused;
    }

    // 进入优雅关闭(排空到期、协议错误,或会话已无事可做):从这一刻起算 closeGrace 硬上限,到期仍没关就 forceClose
    // (对端始终不给流控窗口、不读导致输出缓冲写不空、或收到 FIN 后迟迟不关)。不在这里写半关:连接照常读
    // (对端的 WINDOW_UPDATE / RST_STREAM / PING ACK)、照常按背压取帧写出,由 ShutdownWriteWhenSessionDone 在会话
    // 既不想读也不想写时写半关(写半关时机与 grpc-go 优雅停机、nghttpx 同口径;硬上限相当于 GracefulStop 超时后的 Stop)。
    // 幂等:硬上限只按第一次进入的时刻算。
    void StartGracefulClose()
    {
        if (closed || closing || gracefulCloseStarted)
        {
            return;
        }
        gracefulCloseStarted = true;
        graceDeadline = SteadyClock::now() + closeGrace;
        ArmGraceTimer();
    }

    // 写半关的唯一入口:会话既不想读也不想写(ServerSession::ShouldClose,即 nghttp2_session_want_read 与 want_write
    // 都为 0)才写半关。正常关闭时这意味着最终 GOAWAY 已写出(或收到了对端 GOAWAY)且所有流都已关闭 —— 已提交应答的
    // DATA 与 trailers 都已交给 muduo。只看 want_write 不够:被对端流控窗口卡住的 DATA 不算「想写」,但它所在的流还没关,
    // want_read 仍为真,所以会一直等到对端给窗口、写完 trailers(或硬上限到期)。会话被终止(带错误码的 GOAWAY 已写出、
    // 致命错误)时也为真。写半关之后 muduo 先把输出缓冲写空再发 FIN(shutdownInLoop 在 isWriting 时推迟到 handleWrite
    // 写空之后),此后 TcpConnection::send 丢弃字节 —— 这时会话已没有要写的帧。
    // 对端 GOAWAY 这类直接满足条件的路径没有经过 StartGracefulClose,在这里补上硬上限(对端不读、写不空时由它 forceClose)。
    // 调用方保证不在会话回调栈里(只从 FlushOutput 的尾部调用)。
    void ShutdownWriteWhenSessionDone()
    {
        if (closed || closing || !session.ShouldClose())
        {
            return;
        }
        const auto conn = tcp.lock();
        if (!conn)
        {
            return;
        }
        StartGracefulClose();
        closing = true;
        conn->shutdown();
    }

    void ArmHandshakeTimer()
    {
        const std::weak_ptr<ServerConnection> weakSelf = weak_from_this();
        handshakeTimer = loop.runAfter(ToSeconds(handshakeDeadline - SteadyClock::now()) + 0.001, [weakSelf]
                                         {
            if (const auto self = weakSelf.lock())
            {
                self->OnHandshakeTimer();
            } });
        handshakeTimerArmed = true;
    }

    void OnHandshakeTimer()
    {
        handshakeTimerArmed = false;
        if (closed || session.HandshakeComplete())
        {
            return;
        }
        if (SteadyClock::now() < handshakeDeadline)
        {
            ArmHandshakeTimer(); // 定时器早到(秒→微秒截断等):按单调钟剩余时间重新上弦
            return;
        }
        if (const auto server = this->server.lock())
        {
            server->OnHandshakeTimeout(*this);
        }
        ForceClose();
    }

    void ArmPingTimer()
    {
        const std::weak_ptr<ServerConnection> weakSelf = weak_from_this();
        pingTimer = loop.runAfter(ToSeconds(pingDeadline - SteadyClock::now()) + 0.001, [weakSelf]
                                    {
            if (const auto self = weakSelf.lock())
            {
                self->OnPingTimer();
            } });
        pingTimerArmed = true;
    }

    void OnPingTimer()
    {
        pingTimerArmed = false;
        if (closed)
        {
            return;
        }
        if (SteadyClock::now() < pingDeadline)
        {
            ArmPingTimer();
            return;
        }
        // 满 goawayPingTimeout 仍没收到 PING ACK(对端慢,或 nghttp2 因已有 GOAWAY 而没发 notice / PING):直接发第二段。
        SubmitFinalGoaway();
        FlushOutput();
    }

    void ArmGraceTimer()
    {
        const std::weak_ptr<ServerConnection> weakSelf = weak_from_this();
        graceTimer = loop.runAfter(ToSeconds(graceDeadline - SteadyClock::now()) + 0.001, [weakSelf]
                                     {
            if (const auto self = weakSelf.lock())
            {
                self->OnGraceTimer();
            } });
        graceTimerArmed = true;
    }

    void OnGraceTimer()
    {
        graceTimerArmed = false;
        if (closed)
        {
            return;
        }
        if (SteadyClock::now() < graceDeadline)
        {
            ArmGraceTimer();
            return;
        }
        ForceClose();
    }

    void CancelTimers()
    {
        // 进程退出路径上 EventLoop 可能已不在本线程(或已析构):定时器随 loop 一起释放,闭包只持 weak_ptr。
        if (muduo::net::EventLoop::getEventLoopOfCurrentThread() != &loop)
        {
            handshakeTimerArmed = false;
            pingTimerArmed = false;
            graceTimerArmed = false;
            return;
        }
        if (handshakeTimerArmed)
        {
            loop.cancel(handshakeTimer);
            handshakeTimerArmed = false;
        }
        if (pingTimerArmed)
        {
            loop.cancel(pingTimer);
            pingTimerArmed = false;
        }
        if (graceTimerArmed)
        {
            loop.cancel(graceTimer);
            graceTimerArmed = false;
        }
    }

    muduo::net::EventLoop &loop;
    const std::weak_ptr<Server> server;
    const uint64_t id;
    const std::string peer;
    const size_t maxPendingOutputBytes;
    const std::chrono::milliseconds handshakeTimeout;
    const size_t maxTrackedHeaderTimes;
    std::weak_ptr<muduo::net::TcpConnection> tcp;   // §11.7:只存 weak,强持有者是 Server::connections_
    ServerSession session;

    std::unordered_map<int32_t, uint64_t> streamCalls;                  // streamId → callId(已登记的调用)
    std::map<int32_t, SteadyClock::time_point> headersReceivedAt;      // 按流 id 有序,便于修剪最老的
    std::vector<DeferredResponse> deferred;

    // >0 表示正处在 session_ 的 Receive / DrainOutput 调用栈里:此时不重入 nghttp2。
    int sessionDepth{0};
    bool flushDeferred{false};
    bool closed{false};                 // 已摘表
    bool closing{false};                // 已写半关或已 forceClose:不再写
    bool gracefulCloseStarted{false};   // 已进入优雅关闭(硬上限已上弦);写半关要等会话既不想读也不想写
    bool shutdownStarted{false};        // 已发第一段 GOAWAY
    bool finalGoawayRequested{false};   // PING ACK 已到,等 Receive 返回后提交第二段
    bool protocolErrorReported{false};

    std::chrono::milliseconds closeGrace{ServerShutdownOptions{}.closeGrace};   // 优雅关闭的硬上限,从进入时起算

    // muduo 定时器已按单调时钟到期;到点仍按 steady_clock 复核(兜住秒→微秒截断造成的早到)。
    muduo::net::TimerId handshakeTimer;
    bool handshakeTimerArmed{false};
    SteadyClock::time_point handshakeDeadline{};
    muduo::net::TimerId pingTimer;
    bool pingTimerArmed{false};
    SteadyClock::time_point pingDeadline{};
    muduo::net::TimerId graceTimer;
    bool graceTimerArmed{false};
    SteadyClock::time_point graceDeadline{};
};

} // namespace detail

// ── Server ────────────────────────────────────────────────────────────

class Server::BatchScope
{
public:
    explicit BatchScope(Server &server) : server(server) { ++this->server.batchDepth; }
    ~BatchScope()
    {
        if (--server.batchDepth == 0)
        {
            server.FlushDirtyConnections();
        }
    }
    BatchScope(const BatchScope &) = delete;
    BatchScope &operator=(const BatchScope &) = delete;

private:
    Server &server;
};

std::shared_ptr<Server> Server::Create(muduo::net::EventLoop &loop, const std::string &ipv4, uint16_t port,
                                       ServerOptions options)
{
    // muduo InetAddress 遇到非法 IP 会静默落成 0.0.0.0(监听全部网卡),必须先校验(§15.3 Q8)。
    if (!IsCanonicalIpv4(ipv4) || port == 0)
    {
        LOG_ERROR << "muduo_grpc::Server rejects listen address '" << ipv4 << ":" << port
                  << "': only canonical IPv4 literals with a non-zero port are supported";
        return nullptr;
    }
    return std::make_shared<Server>(PrivateTag{}, loop, muduo::net::InetAddress(ipv4, port), std::move(options));
}

Server::Server(PrivateTag, muduo::net::EventLoop &loop, const muduo::net::InetAddress &listenAddress,
               ServerOptions options)
    : loop(loop),
      listenInetAddress(listenAddress),
      listenAddress(listenAddress.toIpPort()),
      options(std::move(options))
{
}

Server::~Server()
{
    // 照 TcpServer::~TcpServer:只在 loop 线程析构。
    loop.assertInLoopThread();
    CancelTimers();
    // 剩余调用静默收口:之后的 Finish / 句柄析构经 weak_ptr 落空。
    calls.clear();
    ready.clear();
    deadlines.clear();
    dirtyConnections.clear();
    // 剩余连接逐条 forceClose:forceCloseInLoop 排进队列并持有连接;随后 handleClose 调 close 回调,
    // 回调里 lock 不到 Server,照样无条件排 connectDestroyed(两趟都由节点的两跳 quit 覆盖)。
    for (auto &entry : connections)
    {
        entry.second.connection->Close();
        entry.second.tcp->forceClose();
    }
    connections.clear();
    // 没 BeginShutdown 就析构时 Acceptor 在这里同步销毁(见头文件「析构前置」)。
    acceptor.reset();
    onDrained = nullptr; // 不调 onDrained
}

bool Server::RegisterService(Service &service)
{
    loop.assertInLoopThread();
    if (phase != Phase::kCreated)
    {
        LOG_ERROR << "muduo_grpc::Server " << listenAddress << ": RegisterService must be called before Start";
        return false;
    }
    const bool validBefore = methodTable.Valid();
    service.RegisterMethods(methodTable);
    return validBefore && methodTable.Valid();
}

void Server::Start()
{
    loop.assertInLoopThread();
    if (phase != Phase::kCreated)
    {
        LOG_ERROR << "muduo_grpc::Server " << listenAddress << ": Start ignored (already started or shutting down)";
        return;
    }
    if (!methodTable.Valid())
    {
        LOG_FATAL << "muduo_grpc::Server " << listenAddress
                  << " refuses to start: the method table has malformed or duplicate entries (see earlier errors)";
    }
    if (weak_from_this().expired())
    {
        LOG_FATAL << "muduo_grpc::Server must be owned by shared_ptr (use Server::Create) before Start()";
    }

    // 冻结方法表:下标即 methodIndex(派发与按方法统计用);统计名取方法名,同名时退回完整路径。
    const std::vector<std::string> paths = methodTable.Paths();
    std::map<std::string, size_t, std::less<>> methodNameCount;
    std::set<std::string, std::less<>> services;
    for (const auto &path : paths)
    {
        const auto parts = SplitMethodPath(path);
        if (parts)
        {
            ++methodNameCount[std::string(parts->second)];
            services.emplace(parts->first);
        }
    }
    methods.reserve(paths.size());
    for (const auto &path : paths)
    {
        const auto parts = SplitMethodPath(path);
        const UnaryMethodHandler *handler = methodTable.Find(path);
        if (!parts || handler == nullptr)
        {
            continue; // 不可达:AddUnary 已校验
        }
        MethodEntry entry;
        entry.path = path;
        const std::string methodName(parts->second);
        entry.displayName = methodNameCount[methodName] > 1 ? path : methodName;
        entry.handler = *handler;
        methodIndex.emplace(path, methods.size());
        methods.push_back(std::move(entry));
    }
    stats.methodCodes.assign(methods.size(), {});

    phase = Phase::kRunning;
    const std::weak_ptr<Server> weakSelf = weak_from_this();
    acceptor = std::make_shared<muduo::net::Acceptor>(&loop, listenInetAddress, false);
    acceptor->setNewConnectionCallback([weakSelf](int sockfd, const muduo::net::InetAddress &peerAddress)
                                        {
        if (const auto self = weakSelf.lock())
        {
            self->HandleNewConnection(sockfd, peerAddress);
            return;
        }
        muduo::net::sockets::close(sockfd); });
    acceptor->listen();

    if (options.statsLogInterval.count() > 0)
    {
        statsTimer = loop.runEvery(static_cast<double>(options.statsLogInterval.count()), [weakSelf]
                                     {
            if (const auto self = weakSelf.lock())
            {
                self->LogStatsIfChanged();
            } });
        statsTimerArmed = true;
    }

    std::string serviceList;
    for (const auto &service : services)
    {
        if (!serviceList.empty())
        {
            serviceList += ',';
        }
        serviceList += service;
    }
    LOG_INFO << "gRPC server (muduo_grpc) listening on " << listenAddress << " services=[" << serviceList
             << "] methods=" << methods.size() << " max_streams_per_conn=" << options.session.maxConcurrentStreams
             << " max_in_flight=" << options.maxInFlightCalls << " dispatch_per_turn=" << options.maxDispatchPerTurn
             << "/" << FormatBudget(options.dispatchTurnBudget) << " threads=0";
}

bool Server::draining() const
{
    loop.assertInLoopThread();
    return phase == Phase::kDraining || phase == Phase::kDrainExpired || phase == Phase::kDrained;
}

size_t Server::inFlightCalls() const
{
    loop.assertInLoopThread();
    return calls.size();
}

size_t Server::connectionCount() const
{
    loop.assertInLoopThread();
    return connections.size();
}

void Server::HandleNewConnection(int sockfd, const muduo::net::InetAddress &peerAddress)
{
    loop.assertInLoopThread();
    if (phase != Phase::kRunning)
    {
        // 排空开始之后、Acceptor 被排队销毁之前到达的接入(§15.10 b):直接关掉,不建连接。
        muduo::net::sockets::close(sockfd);
        return;
    }
    // 以下照抄 TcpServer::newConnection 的时序:建连接 → 入表 → 设回调 → connectEstablished。
    const uint64_t connectionId = nextConnectionId++;
    ++stats.accepted;
    const std::string name = options.name + "-" + listenAddress + "#" + std::to_string(connectionId);
    const muduo::net::InetAddress localAddress(muduo::net::sockets::getLocalAddr(sockfd));
    const auto conn = std::make_shared<muduo::net::TcpConnection>(&loop, name, sockfd, localAddress, peerAddress);
    const auto connection = std::make_shared<detail::ServerConnection>(loop, weak_from_this(), connectionId,
                                                                       peerAddress.toIpPort(), options);
    connections.emplace(connectionId, ConnectionEntry{conn, connection});
    stats.connectionsPeak = std::max<uint64_t>(stats.connectionsPeak, connections.size());

    // connectEstablished / handleClose / connectDestroyed 无条件调用 connectionCallback_:空的 std::function 会抛异常,
    // 必须设。连接的建立与断开由本类自己记录,这里给空函数(Linux 上游 TcpServer 每次建连 / 断连各打一行 INFO)。
    conn->setConnectionCallback([](const muduo::net::TcpConnectionPtr &) {});
    const std::weak_ptr<detail::ServerConnection> weakConnection = connection;
    conn->setMessageCallback(
        [weakConnection](const muduo::net::TcpConnectionPtr &, muduo::net::Buffer *buffer, muduo::Timestamp)
        {
            if (const auto self = weakConnection.lock())
            {
                self->HandleMessage(*buffer);
                return;
            }
            buffer->retrieveAll();
        });
    conn->setWriteCompleteCallback([weakConnection](const muduo::net::TcpConnectionPtr &)
                                   {
        if (const auto self = weakConnection.lock())
        {
            self->FlushOutput();
        } });
    // close 回调绑 weak<Server>(上游绑裸 this 并标了 FIXME)。照抄 removeConnectionInLoop:摘表后无条件排 connectDestroyed;
    // 摘下的 ServerConnection 移进同一个闭包,不在它自己的回调栈上释放。排空完成检查排在 connectDestroyed 之后(FIFO)。
    const std::weak_ptr<Server> weakSelf = weak_from_this();
    conn->setCloseCallback([weakSelf, connectionId](const muduo::net::TcpConnectionPtr &closed)
                           {
        std::shared_ptr<detail::ServerConnection> detached;
        const auto self = weakSelf.lock();
        if (self)
        {
            detached = self->DetachConnection(connectionId);
        }
        closed->getLoop()->queueInLoop([closed, detached] { closed->connectDestroyed(); });
        if (self)
        {
            self->MaybeFinishDrain();
        } });
    conn->setTcpNoDelay(true);
    conn->connectEstablished();
    connection->Start(conn);
}

std::shared_ptr<detail::ServerConnection> Server::DetachConnection(uint64_t connectionId)
{
    const auto it = connections.find(connectionId);
    if (it == connections.end())
    {
        return nullptr;
    }
    std::shared_ptr<detail::ServerConnection> connection = std::move(it->second.connection);
    connections.erase(it);
    dirtyConnections.erase(connectionId);
    connection->Close();
    // §15.7 第 5 条:逐个撤销这条连接上的调用,所以连接表为空就意味着调用表为空。
    for (const uint64_t callId : connection->TakeAllCalls())
    {
        const auto phase = CompleteCall(callId, CallEnd{});
        if (!phase)
        {
            continue;
        }
        if (*phase == CallPhase::kStarted)
        {
            ++stats.abortedAfterStart;
        }
        else
        {
            ++stats.cancelledBeforeStart;
        }
    }
    if (!connection->receivedAnyBytes())
    {
        // 探针(kubelet tcpSocket)连上就关:本端一个字节都没写,只打 DEBUG(§15.5.1)。
        ++stats.closedBeforePreface;
        LOG_DEBUG << "[GrpcServer] connection from " << connection->Peer() << " closed before the HTTP/2 preface";
    }
    else
    {
        LOG_DEBUG << "[GrpcServer] connection from " << connection->Peer() << " closed";
    }
    return connection;
}

void Server::MarkConnectionDirty(uint64_t connectionId)
{
    if (batchDepth > 0)
    {
        dirtyConnections.insert(connectionId);
        return;
    }
    const auto it = connections.find(connectionId);
    if (it != connections.end())
    {
        const auto connection = it->second.connection; // 冲出期间保活
        connection->FlushOutput();
    }
}

void Server::FlushDirtyConnections()
{
    while (!dirtyConnections.empty())
    {
        std::set<uint64_t> dirty;
        dirty.swap(dirtyConnections);
        for (const uint64_t connectionId : dirty)
        {
            const auto it = connections.find(connectionId);
            if (it != connections.end())
            {
                const auto connection = it->second.connection;
                connection->FlushOutput();
            }
        }
    }
}

ServerSession::Verdict Server::RouteRequest(const RequestHeaders &headers)
{
    if (phase == Phase::kDrainExpired || phase == Phase::kDrained)
    {
        ++stats.refusedOnShutdown; // 排空到期之后才开的流:没处理,客户端可透明重试(§15.3 Q4)
        return RefuseVerdict();
    }
    if (const auto known = methodIndex.find(std::string_view(headers.path)); known != methodIndex.end())
    {
        // 准入(ServerOptions::admission,节点接的是热关停):业务开始之前拒绝,请求体不收、handler 不执行。
        // 拒绝的状态码由准入决定,计入 codes / methods 统计;逐条不打日志(准入方自己计数与采样)。
        if (options.admission)
        {
            if (auto denied = options.admission(headers.path); denied && denied->Code() != StatusCode::kOk)
            {
                CountStatus(known->second, denied->Code());
                return RejectVerdict(std::move(*denied));
            }
        }
        return AcceptVerdict();
    }
    // ServerSession 保证 path 能拆开;拆不开时按 grpc-go 同文案兜底。
    Status status;
    const auto parts = SplitMethodPath(headers.path);
    if (!parts)
    {
        status = Status(StatusCode::kUnimplemented, "malformed method name: \"" + headers.path + "\"");
    }
    else if (!methodTable.HasService(parts->first))
    {
        status = Status(StatusCode::kUnimplemented, "unknown service " + std::string(parts->first));
    }
    else
    {
        status = Status(StatusCode::kUnimplemented, "unknown method " + std::string(parts->second) + " for service " +
                                                        std::string(parts->first));
    }
    const uint64_t occurrence = ++stats.unimplemented;
    CountStatus(std::nullopt, status.Code());
    if (ShouldLogServerEvent(occurrence))
    {
        LOG_WARN << "[GrpcServer] " << listenAddress << ": " << status.message() << " (occurrence " << occurrence
                 << ")";
    }
    return RejectVerdict(std::move(status));
}

ServerSession::Verdict Server::AcceptCall(detail::ServerConnection &connection, int32_t streamId,
                                          RequestHeaders headers, std::string message,
                                          std::chrono::steady_clock::time_point headersAt)
{
    if (phase == Phase::kDrainExpired || phase == Phase::kDrained)
    {
        // 防御:到期时仍在上传的流已被 ExpireDrain 按会话状态全部拒掉(ServerSession::AcceptedUndispatchedStreams),
        // 到期之后才收完头部的流在 RouteRequest 就被拒,正常走不到这里。真走到了也不派发,同样回 REFUSED_STREAM。
        ++stats.refusedOnShutdown;
        return RefuseVerdict();
    }
    const auto found = this->methodIndex.find(std::string_view(headers.path));
    if (found == this->methodIndex.end())
    {
        return RouteRequest(headers); // 不可达:头部阶段已路由
    }
    const size_t methodIndex = found->second;
    if (calls.size() >= options.maxInFlightCalls)
    {
        // 业务之前回 RESOURCE_EXHAUSTED(不用 REFUSED_STREAM:grpc-go 会立刻在同一连接透明重试),§15.3 Q2。
        const uint64_t occurrence = ++stats.rejectedOverload;
        CountStatus(methodIndex, StatusCode::kResourceExhausted);
        if (ShouldLogServerEvent(occurrence))
        {
            LOG_WARN << "[GrpcServer] " << listenAddress << ": in-flight call limit (" << options.maxInFlightCalls
                     << ") reached, rejecting " << methods[methodIndex].displayName << " (occurrence " << occurrence
                     << ")";
        }
        return RejectVerdict(Status(StatusCode::kResourceExhausted, "server in-flight call limit (" +
                                                                        std::to_string(options.maxInFlightCalls) +
                                                                        ") reached"));
    }

    const uint64_t callId = nextCallId++;
    CallRecord record;
    record.connectionId = connection.Id();
    record.streamId = streamId;
    record.methodIndex = methodIndex;
    record.peer = connection.Peer();
    record.metadata = std::move(headers.metadata);
    if (headers.timeout)
    {
        record.deadline = DeadlineFrom(headersAt, *headers.timeout); // ServerSession 保证 timeout 为正
    }
    record.message = std::move(message);
    const auto deadline = record.deadline;
    calls.emplace(callId, std::move(record));
    connection.BindStream(streamId, callId);
    stats.inFlightPeak = std::max<uint64_t>(stats.inFlightPeak, calls.size());
    if (deadline)
    {
        deadlines.emplace(*deadline, callId);
        ArmDeadlineTimer();
    }
    ready.push_back(callId);
    stats.maxBacklog = std::max<uint64_t>(stats.maxBacklog, ready.size());
    ScheduleDispatch();
    return AcceptVerdict();
}

void Server::AbortStream(detail::ServerConnection &connection, int32_t streamId)
{
    const auto callId = connection.TakeStream(streamId);
    if (!callId)
    {
        return;
    }
    // 对端 RST:不回应答。业务已开始的,之后的 Finish 计 late_finish(§15.5.6)。
    const auto phase = CompleteCall(*callId, CallEnd{});
    if (!phase)
    {
        return;
    }
    if (*phase == CallPhase::kStarted)
    {
        ++stats.abortedAfterStart;
    }
    else
    {
        ++stats.cancelledBeforeStart;
    }
}

void Server::OnSessionRejected(const detail::ServerConnection &connection, int httpStatus, const Status &status)
{
    CountStatus(std::nullopt, status.Code());
    if (status.Code() == StatusCode::kDeadlineExceeded)
    {
        // 会话层只在 grpc-timeout <= 0 时回这个码:截止在客户端发出前就已到,来自合法客户端,不是畸形请求。
        // 计入 deadline_before_start(§15.11),不计 rejected_protocol、不打 WARN(server_session.h OnRequestRejected)。
        ++stats.deadlineBeforeStart;
        return;
    }
    const uint64_t occurrence = ++stats.rejectedProtocol;
    if (ShouldLogServerEvent(occurrence))
    {
        LOG_WARN << "[GrpcServer] " << listenAddress << ": rejected a request from " << connection.Peer()
                 << " before dispatch with HTTP " << httpStatus << " " << status.ToString() << " (occurrence "
                 << occurrence << ")";
    }
}

void Server::OnConnectionProtocolError(const detail::ServerConnection &connection, const Status &status)
{
    const uint64_t occurrence = ++stats.protocolErrors;
    if (ShouldLogServerEvent(occurrence))
    {
        LOG_WARN << "[GrpcServer] " << listenAddress << ": closing connection from " << connection.Peer()
                 << " after a protocol error: " << status.message() << " (occurrence " << occurrence << ")";
    }
}

void Server::OnHandshakeTimeout(const detail::ServerConnection &connection)
{
    ++stats.handshakeTimeout;
    LOG_DEBUG << "[GrpcServer] " << listenAddress << ": HTTP/2 handshake with " << connection.Peer()
              << " timed out after " << options.handshakeTimeout.count() << "ms";
}

void Server::ScheduleDispatch()
{
    if (dispatchQueued || ready.empty())
    {
        return;
    }
    dispatchQueued = true;
    // 泵任务经 queueInLoop 排队,同一时刻至多一个。泵内再排时 callingPendingFunctors_ 为真,会唤醒并落到下一轮,
    // 中间让出给定时器与 I/O(§15.6)。
    const std::weak_ptr<Server> weakSelf = weak_from_this();
    loop.queueInLoop([weakSelf]
                      {
        if (const auto self = weakSelf.lock())
        {
            self->RunDispatchTurn();
        } });
}

void Server::RunDispatchTurn()
{
    dispatchQueued = false;
    BatchScope batch(*this); // 本轮产生的应答在泵结束时统一冲出
    const size_t maxPerTurn = std::max<size_t>(1, options.maxDispatchPerTurn);
    const auto turnStart = SteadyClock::now();
    size_t processed = 0;
    uint64_t dispatchedThisTurn = 0;
    while (!ready.empty() && processed < maxPerTurn)
    {
        if (processed > 0 && SteadyClock::now() - turnStart >= options.dispatchTurnBudget)
        {
            break; // 每轮累计 ≤ 预算,至少处理 1 个
        }
        const uint64_t callId = ready.front();
        ready.pop_front();
        const auto it = calls.find(callId);
        if (it == calls.end() || it->second.phase != CallPhase::kQueued)
        {
            continue; // 排队期间已被撤销(对端 RST / 断连 / 截止)
        }
        ++processed;
        CallRecord &record = it->second;
        if (record.deadline && SteadyClock::now() >= *record.deadline)
        {
            CompleteCall(callId, CallEnd{CallEndKind::kStatus, std::string(),
                                         Status(StatusCode::kDeadlineExceeded,
                                                "deadline exceeded before the call started")});
            ++stats.deadlineBeforeStart;
            continue;
        }
        record.phase = CallPhase::kDispatched;
        auto token = std::make_shared<detail::CallToken>();
        token->server = weak_from_this();
        token->callId = callId;
        token->loopThread = std::this_thread::get_id();
        token->method = methods[record.methodIndex].path;
        token->peer = record.peer;
        token->metadata = std::move(record.metadata);
        token->deadline = record.deadline;
        // 先把 message 移出记录:handler 可能同步完成并删掉记录。methods_ 在 Start 后不再修改,引用稳定。
        const std::string message = std::move(record.message);
        const UnaryMethodHandler &handler = methods[record.methodIndex].handler;
        ++stats.dispatched;
        ++dispatchedThisTurn;
        InvokeUnaryHandler(options.dispatchInterceptor, handler, ServerCall(std::move(token)), std::string_view(message),
                           methods[record.methodIndex].displayName, methods[record.methodIndex].path);
    }
    stats.maxDispatchedPerTurn = std::max(stats.maxDispatchedPerTurn, dispatchedThisTurn);
    if (!ready.empty())
    {
        ScheduleDispatch();
    }
}

std::optional<Server::CallPhase> Server::CompleteCall(uint64_t callId, CallEnd end)
{
    // §15.7:找到即 erase —— 恰好一次的唯一裁决点。先移出记录并删表,再递减在途、提交应答、标记冲出;
    // 记录本身(请求体、metadata)在函数结束时才释放。
    const auto it = calls.find(callId);
    if (it == calls.end())
    {
        return std::nullopt;
    }
    CallRecord record = std::move(it->second);
    calls.erase(it);
    if (record.deadline)
    {
        deadlines.erase({*record.deadline, callId});
    }
    if (end.kind == CallEndKind::kResponse)
    {
        CountStatus(record.methodIndex, StatusCode::kOk);
    }
    else if (end.kind == CallEndKind::kStatus)
    {
        CountStatus(record.methodIndex, end.status.Code());
    }
    const auto connectionIt = connections.find(record.connectionId);
    if (connectionIt != connections.end())
    {
        const auto connection = connectionIt->second.connection;
        connection->ForgetStream(record.streamId);
        if (end.kind != CallEndKind::kSilent)
        {
            connection->Respond(record.streamId, std::move(end));
            MarkConnectionDirty(record.connectionId);
        }
    }
    return record.phase;
}

bool Server::TryStartBusiness(uint64_t callId)
{
    const auto it = calls.find(callId);
    if (it == calls.end())
    {
        return false; // 已被截止 / 取消 / 排空收口
    }
    if (it->second.phase == CallPhase::kStarted)
    {
        return false; // 已开始过一次:不让业务执行两次
    }
    if (it->second.deadline && SteadyClock::now() >= *it->second.deadline)
    {
        // 判定「有效且未到期」与标记「已开始」在同一次 loop 调用里完成(§15.7 第 4 条)。
        CompleteCall(callId, CallEnd{CallEndKind::kStatus, std::string(),
                                     Status(StatusCode::kDeadlineExceeded,
                                            "deadline exceeded before the call started")});
        ++stats.deadlineBeforeStart;
        return false;
    }
    it->second.phase = CallPhase::kStarted;
    return true;
}

void Server::FinishFromHandler(const detail::CallToken &token, CallEnd end)
{
    if (calls.find(token.callId) == calls.end())
    {
        const uint64_t occurrence = ++stats.lateFinish;
        if (ShouldLogServerEvent(occurrence))
        {
            LOG_DEBUG << "[GrpcServer] late finish ignored (call already completed): method=" << token.method
                      << " call_id=" << token.callId << " (occurrence " << occurrence << ")";
        }
        return;
    }
    if (end.kind == CallEndKind::kResponse && end.response.size() > options.maxSendMessageBytes)
    {
        // 发生在业务之后(§15.13):Go 客户端收包时本来也报同一个码。
        LOG_ERROR << "[GrpcServer] response of " << token.method << " is " << end.response.size()
                  << " bytes, larger than max " << options.maxSendMessageBytes << "; replying RESOURCE_EXHAUSTED";
        const std::string message = "grpc: trying to send message larger than max (" +
                                    std::to_string(end.response.size()) + " vs. " +
                                    std::to_string(options.maxSendMessageBytes) + ")";
        end = CallEnd{CallEndKind::kStatus, std::string(), Status(StatusCode::kResourceExhausted, message)};
    }
    CompleteCall(token.callId, std::move(end));
}

void Server::OnTokenDropped(const detail::CallToken &token)
{
    if (calls.find(token.callId) == calls.end())
    {
        return; // 正常路径:调用已完成
    }
    // 业务可能已执行,只能回「结果未知」码(§15.4.3)。
    const uint64_t occurrence = ++stats.dropped;
    if (ShouldLogServerEvent(occurrence))
    {
        LOG_ERROR << "[GrpcServer] call dropped without completion: method=" << token.method
                  << " call_id=" << token.callId << "; replying UNAVAILABLE (occurrence " << occurrence << ")";
    }
    CompleteCall(token.callId, CallEnd{CallEndKind::kStatus, std::string(),
                                       Status(StatusCode::kUnavailable,
                                              "server dropped the call without completing it")});
}

void Server::CountStatus(std::optional<size_t> methodIndex, StatusCode code)
{
    const uint32_t value = static_cast<uint32_t>(code);
    ++stats.codes[value];
    if (methodIndex && *methodIndex < stats.methodCodes.size())
    {
        ++stats.methodCodes[*methodIndex][value];
    }
}

void Server::ArmDeadlineTimer()
{
    if (deadlines.empty())
    {
        return;
    }
    const auto earliest = deadlines.begin()->first;
    if (deadlineTimerArmed && deadlineTimerFor <= earliest)
    {
        return;
    }
    if (deadlineTimerArmed)
    {
        loop.cancel(deadlineTimer);
    }
    // muduo 定时器已按单调时钟到期;到点仍用 steady_clock 复核,未到就重新上弦(照 Channel),
    // 兜住秒→微秒截断造成的微秒级早到。加 1ms 余量,避免这点截断让定时器早到一点而空转一轮。
    const std::weak_ptr<Server> weakSelf = weak_from_this();
    deadlineTimer = loop.runAfter(ToSeconds(earliest - SteadyClock::now()) + 0.001, [weakSelf]
                                    {
        if (const auto self = weakSelf.lock())
        {
            self->OnDeadlineTimer();
        } });
    deadlineTimerArmed = true;
    deadlineTimerFor = earliest;
}

void Server::OnDeadlineTimer()
{
    deadlineTimerArmed = false;
    BatchScope batch(*this);
    const auto now = SteadyClock::now();
    std::vector<uint64_t> due;
    for (const auto &[deadline, callId] : deadlines)
    {
        if (deadline > now)
        {
            break;
        }
        due.push_back(callId);
    }
    for (const uint64_t callId : due)
    {
        const auto it = calls.find(callId);
        if (it == calls.end())
        {
            continue;
        }
        if (it->second.phase == CallPhase::kStarted)
        {
            // 业务已开始(只有延后完成的 handler 会处在这里):立即回,业务不打断,迟到的完成丢弃并计 late_finish。
            CompleteCall(callId, CallEnd{CallEndKind::kStatus, std::string(),
                                         Status(StatusCode::kDeadlineExceeded, "deadline exceeded")});
            ++stats.deadlineAfterStart;
        }
        else
        {
            // 排队中或等许可:不派发 / 不进业务;许可回来后 TryStartBusiness 为假。
            CompleteCall(callId, CallEnd{CallEndKind::kStatus, std::string(),
                                         Status(StatusCode::kDeadlineExceeded,
                                                "deadline exceeded before the call started")});
            ++stats.deadlineBeforeStart;
        }
    }
    ArmDeadlineTimer();
}

void Server::BeginShutdown(ServerShutdownOptions options, std::function<void()> onDrained)
{
    loop.assertInLoopThread();
    if (phase != Phase::kCreated && phase != Phase::kRunning)
    {
        LOG_WARN << "[GrpcServer] " << listenAddress << ": BeginShutdown called again; ignored";
        return;
    }
    // a) 置排空:此后 ServerCall::ServerDraining() 为真。
    phase = Phase::kDraining;
    shutdownOptions = options;
    this->onDrained = std::move(onDrained);
    drainStartedAt = SteadyClock::now();
    drainDeadline = drainStartedAt + options.drainTimeout;
    LOG_INFO << "gRPC server draining: listen=" << listenAddress << " conns=" << connections.size()
             << " inflight=" << calls.size() << " drain_timeout_ms=" << options.drainTimeout.count();

    // b) 停听:Acceptor 移进 queueInLoop 闭包销毁。不能在这里同步销毁 —— 本调用可能在定时器回调里
    //    (Windows 的定时器跑在 eventHandling_ 期间,Linux 的 timerfd 本身就是活跃 channel),
    //    而 Acceptor 的 channel 可能同在本轮 activeChannels_ 里,removeChannel 会撞断言。
    //    销毁前到达的接入由 HandleNewConnection 一律 close。
    if (this->acceptor)
    {
        loop.queueInLoop([acceptor = std::move(this->acceptor)]() mutable { acceptor.reset(); });
        this->acceptor.reset();
    }

    {
        BatchScope batch(*this);
        // c) 每条连接:收到过字节 → 第一段 GOAWAY + PING;没收到过 → forceClose(只排队,不会同步摘表)。
        for (auto &entry : connections)
        {
            if (!entry.second.connection->receivedAnyBytes())
            {
                entry.second.connection->ForceClose();
                continue;
            }
            entry.second.connection->BeginShutdown(options.goawayPingTimeout, options.closeGrace);
            MarkConnectionDirty(entry.first);
        }
    }

    // d) 排空定时器(steady 复核)。
    ArmDrainTimer();
    // e) 已满足排空条件(没有连接)→ queueInLoop(onDrained),永不在本栈内回调。
    MaybeFinishDrain();
}

void Server::ArmDrainTimer()
{
    const std::weak_ptr<Server> weakSelf = weak_from_this();
    drainTimer = loop.runAfter(ToSeconds(drainDeadline - SteadyClock::now()) + 0.001, [weakSelf]
                                 {
        if (const auto self = weakSelf.lock())
        {
            self->OnDrainTimer();
        } });
    drainTimerArmed = true;
}

void Server::OnDrainTimer()
{
    drainTimerArmed = false;
    if (phase != Phase::kDraining)
    {
        return;
    }
    if (SteadyClock::now() < drainDeadline)
    {
        ArmDrainTimer();
        return;
    }
    phase = Phase::kDrainExpired;
    uint64_t refused = 0;
    uint64_t unavailable = 0;
    {
        BatchScope batch(*this);
        std::vector<uint64_t> callIds;
        callIds.reserve(calls.size());
        for (const auto &entry : calls)
        {
            callIds.push_back(entry.first);
        }
        std::sort(callIds.begin(), callIds.end());
        for (const uint64_t callId : callIds)
        {
            const auto it = calls.find(callId);
            if (it == calls.end())
            {
                continue;
            }
            if (it->second.phase == CallPhase::kStarted)
            {
                // 业务已开始:结果未知(§15.3 Q4)。
                CompleteCall(callId, CallEnd{CallEndKind::kStatus, std::string(),
                                             Status(StatusCode::kUnavailable, "server shutting down")});
                ++stats.unavailableOnShutdown;
                ++unavailable;
            }
            else
            {
                // 业务未开始:REFUSED_STREAM = RFC 9113 §8.7 的「未处理」,grpc-go 透明重试到新连接。
                CompleteCall(callId, CallEnd{CallEndKind::kRefuse, std::string(), Status()});
                ++stats.refusedOnShutdown;
                ++refused;
            }
        }
        // 所有连接:拒掉请求体还在上传的流、补最终 GOAWAY,进入优雅关闭(closeGrace 从此刻起算,是硬上限)。
        // 不立即写半关:本作用域结束时 FlushDirtyConnections 按背压把上面提交的 RST / trailers 与最终 GOAWAY 交给输出缓冲,
        // 之后连接继续读写,会话把已提交的帧(含被对端流控窗口卡住的应答 DATA)全部写出、所有流关闭才写半关。
        std::vector<std::shared_ptr<detail::ServerConnection>> remaining;
        remaining.reserve(connections.size());
        for (const auto &entry : connections)
        {
            remaining.push_back(entry.second.connection);
        }
        for (const auto &connection : remaining)
        {
            const size_t refusedUploads = connection->ExpireDrain();
            stats.refusedOnShutdown += refusedUploads;
            refused += refusedUploads;
            MarkConnectionDirty(connection->Id());
        }
    }
    LOG_WARN << "[GrpcServer] " << listenAddress << ": drain timeout after " << shutdownOptions.drainTimeout.count()
             << "ms: refused=" << refused << " unavailable=" << unavailable << " connections=" << connections.size();
}

void Server::MaybeFinishDrain()
{
    if ((phase != Phase::kDraining && phase != Phase::kDrainExpired) || !connections.empty())
    {
        return;
    }
    phase = Phase::kDrained;
    if (drainTimerArmed)
    {
        loop.cancel(drainTimer);
        drainTimerArmed = false;
    }
    LOG_DEBUG << "[GrpcServer] " << listenAddress << " drained after "
              << std::chrono::duration_cast<std::chrono::milliseconds>(SteadyClock::now() - drainStartedAt).count()
              << "ms";
    if (onDrained)
    {
        // 恰好一次,经 queueInLoop:FIFO 排在最后一个 connectDestroyed(close 回调先排)之后。
        std::function<void()> done;
        done.swap(onDrained);
        loop.queueInLoop(std::move(done));
    }
}

ServerStatsSnapshot Server::Stats() const
{
    loop.assertInLoopThread();
    ServerStatsSnapshot snapshot;
    snapshot.listen = listenAddress;
    snapshot.connections = connections.size();
    snapshot.connectionsPeak = stats.connectionsPeak;
    snapshot.accepted = stats.accepted;
    snapshot.closedBeforePreface = stats.closedBeforePreface;
    snapshot.handshakeTimeout = stats.handshakeTimeout;
    snapshot.protocolErrors = stats.protocolErrors;
    snapshot.inFlight = calls.size();
    snapshot.inFlightPeak = stats.inFlightPeak;
    snapshot.dispatched = stats.dispatched;
    snapshot.maxBacklog = stats.maxBacklog;
    snapshot.maxDispatchedPerTurn = stats.maxDispatchedPerTurn;
    snapshot.rejectedOverload = stats.rejectedOverload;
    snapshot.rejectedProtocol = stats.rejectedProtocol;
    snapshot.unimplemented = stats.unimplemented;
    snapshot.deadlineBeforeStart = stats.deadlineBeforeStart;
    snapshot.deadlineAfterStart = stats.deadlineAfterStart;
    snapshot.cancelledBeforeStart = stats.cancelledBeforeStart;
    snapshot.abortedAfterStart = stats.abortedAfterStart;
    snapshot.refusedOnShutdown = stats.refusedOnShutdown;
    snapshot.unavailableOnShutdown = stats.unavailableOnShutdown;
    snapshot.lateFinish = stats.lateFinish;
    snapshot.dropped = stats.dropped;
    snapshot.droppedOffLoop = DroppedOffLoopCounter().load(std::memory_order_relaxed);
    snapshot.codes = stats.codes;
    for (size_t i = 0; i < methods.size(); ++i)
    {
        // 没被调用过的方法也登记一个空的内层(server.h 的 methods 说明)。
        auto &methodCodes = snapshot.methods[methods[i].displayName];
        if (i >= stats.methodCodes.size())
        {
            continue;
        }
        for (const auto &[code, count] : stats.methodCodes[i])
        {
            methodCodes[code] += count;
        }
    }
    return snapshot;
}

void Server::LogStatsIfChanged()
{
    std::string current = FormatServerStats(Stats());
    if (current == lastLoggedStats)
    {
        return;
    }
    lastLoggedStats = current;
    if (current.size() > kMaxSummaryLogBytes)
    {
        current.resize(kMaxSummaryLogBytes);
        current += " ...(truncated)";
    }
    LOG_INFO << "[GrpcServer] summary " << current;
}

void Server::CancelTimers()
{
    if (deadlineTimerArmed)
    {
        loop.cancel(deadlineTimer);
        deadlineTimerArmed = false;
    }
    if (drainTimerArmed)
    {
        loop.cancel(drainTimer);
        drainTimerArmed = false;
    }
    if (statsTimerArmed)
    {
        loop.cancel(statsTimer);
        statsTimerArmed = false;
    }
}

std::string FormatServerStats(const ServerStatsSnapshot &stats)
{
    std::string out;
    out.reserve(640);
    const auto field = [&out](const char *name, uint64_t value)
    {
        out += ' ';
        out += name;
        out += '=';
        out += std::to_string(value);
    };
    const auto currentAndPeak = [&out](const char *name, uint64_t current, uint64_t peak)
    {
        out += ' ';
        out += name;
        out += '=';
        out += std::to_string(current);
        out += '/';
        out += std::to_string(peak);
    };
    out += "listen=";
    out += stats.listen;
    currentAndPeak("conns", stats.connections, stats.connectionsPeak);
    field("accepted", stats.accepted);
    field("closed_before_preface", stats.closedBeforePreface);
    field("handshake_timeout", stats.handshakeTimeout);
    field("protocol_errors", stats.protocolErrors);
    currentAndPeak("inflight", stats.inFlight, stats.inFlightPeak);
    field("dispatched", stats.dispatched);
    field("max_backlog", stats.maxBacklog);
    field("max_dispatched_per_turn", stats.maxDispatchedPerTurn);
    field("rejected_overload", stats.rejectedOverload);
    field("rejected_protocol", stats.rejectedProtocol);
    field("unimplemented", stats.unimplemented);
    field("deadline_before_start", stats.deadlineBeforeStart);
    field("deadline_after_start", stats.deadlineAfterStart);
    field("cancelled_before_start", stats.cancelledBeforeStart);
    field("aborted_after_start", stats.abortedAfterStart);
    field("refused_on_shutdown", stats.refusedOnShutdown);
    field("unavailable_on_shutdown", stats.unavailableOnShutdown);
    field("late_finish", stats.lateFinish);
    field("dropped", stats.dropped);
    field("dropped_off_loop", stats.droppedOffLoop);
    out += " codes=";
    bool first = true;
    for (const auto &[code, count] : stats.codes)
    {
        if (!first)
        {
            out += ',';
        }
        first = false;
        out += std::to_string(code);
        out += ':';
        out += std::to_string(count);
    }
    out += " methods={";
    first = true;
    for (const auto &[method, codes] : stats.methods)
    {
        for (const auto &[code, count] : codes)
        {
            if (!first)
            {
                out += ',';
            }
            first = false;
            out += method;
            out += ':';
            out += std::to_string(code);
            out += '=';
            out += std::to_string(count);
        }
    }
    out += '}';
    return out;
}

} // namespace muduo_grpc
