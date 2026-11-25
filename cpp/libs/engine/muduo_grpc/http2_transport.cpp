#include "http2_transport.h"

#include <algorithm>

#include "muduo/base/Logging.h"
#include "muduo/net/Buffer.h"
#include "muduo/net/EventLoop.h"
#include "muduo/net/TcpConnection.h"

namespace muduo_grpc
{

namespace
{

// HTTP/2 帧类型与错误码(RFC 9113)。本文件不 include nghttp2.h,只用到这几个常量。
constexpr uint8_t kFrameTypeData = 0x0;
constexpr uint8_t kFrameTypeHeaders = 0x1;

double ToSeconds(std::chrono::steady_clock::duration duration)
{
    return std::max(0.0, std::chrono::duration<double>(duration).count());
}

} // namespace

std::shared_ptr<Http2Transport> Http2Transport::Create(muduo::net::EventLoop &loop,
                                                       const muduo::net::InetAddress &address,
                                                       const std::string &name, Http2TransportOptions options,
                                                       std::weak_ptr<Http2TransportListener> listener, uint64_t id)
{
    return std::make_shared<Http2Transport>(PrivateTag{}, loop, address, name, std::move(options),
                                            std::move(listener), id);
}

Http2Transport::Http2Transport(PrivateTag, muduo::net::EventLoop &loop, const muduo::net::InetAddress &address,
                               const std::string &name, Http2TransportOptions options,
                               std::weak_ptr<Http2TransportListener> listener, uint64_t id)
    : loop(loop),
      client(&loop, address, name),
      target(address.toIpPort()),
      options(std::move(options)),
      listener(std::move(listener)),
      id(id)
{
    // 不 enableRetry:一个对象只对应一次连接;重连与退避由 Channel 按 gRPC 规范做。
}

Http2Transport::~Http2Transport()
{
    CancelTimers();
    // §11.7:拆除前显式 forceClose,不依赖 ~TcpClient 的 use_count()==1 判断。
    // 取出的强引用只活在这个块里,~TcpClient 执行时已经释放。
    {
        const auto conn = client.connection();
        if (conn)
        {
            conn->forceClose();
        }
    }
}

void Http2Transport::Connect()
{
    if (connectStarted)
    {
        return;
    }
    connectStarted = true;
    if (weak_from_this().expired())
    {
        LOG_FATAL << "muduo_grpc::Http2Transport must be owned by shared_ptr before Connect()";
    }

    // muduo 会把回调拷到 TcpConnection 上,可能在本对象析构之后才触发(forceClose 一律排队):只绑 weak_ptr。
    std::weak_ptr<Http2Transport> weakSelf = weak_from_this();
    client.setConnectionCallback([weakSelf](const muduo::net::TcpConnectionPtr &conn)
                                  {
        if (const auto self = weakSelf.lock())
        {
            self->HandleConnection(conn);
        } });
    client.setMessageCallback([weakSelf](const muduo::net::TcpConnectionPtr &conn, muduo::net::Buffer *buffer,
                                          muduo::Timestamp)
                               {
        if (const auto self = weakSelf.lock())
        {
            self->HandleMessage(conn, buffer);
            return;
        }
        buffer->retrieveAll(); });
    client.setWriteCompleteCallback([weakSelf](const muduo::net::TcpConnectionPtr &)
                                     {
        if (const auto self = weakSelf.lock())
        {
            self->FlushOutput();
        } });
    // muduo LOCAL DELTA 补的连接失败回调;回调存进 Connector,同样只绑 weak_ptr。
    client.setConnectFailedCallback([weakSelf](int savedErrno)
                                     {
        if (const auto self = weakSelf.lock())
        {
            self->HandleConnectFailed(savedErrno);
        } });

    connectDeadline = std::chrono::steady_clock::now() + options.connectTimeout;
    ArmConnectTimer();
    client.connect();
}

void Http2Transport::ArmConnectTimer()
{
    // muduo 定时器已按单调时钟到期,墙钟跳变不影响;到点仍按 steady_clock 复核(HandleConnectTimeout),
    // 兜住秒→微秒截断造成的微秒级早到,不会误判超时。
    std::weak_ptr<Http2Transport> weakSelf = weak_from_this();
    const auto remaining = connectDeadline - std::chrono::steady_clock::now();
    connectTimer = loop.runAfter(ToSeconds(remaining) + 0.001, [weakSelf]
                                   {
        if (const auto self = weakSelf.lock())
        {
            self->HandleConnectTimeout();
        } });
    connectTimerArmed = true;
}

void Http2Transport::ArmProbeTimer(uint64_t generation)
{
    std::weak_ptr<Http2Transport> weakSelf = weak_from_this();
    const auto remaining = probeDeadline - std::chrono::steady_clock::now();
    probeTimer = loop.runAfter(ToSeconds(remaining) + 0.001, [weakSelf, generation]
                                 {
        if (const auto self = weakSelf.lock())
        {
            self->HandleProbeTimeout(generation);
        } });
    probeTimerArmed = true;
}

ClientSession::SubmitResult Http2Transport::StartUnaryStream(const UnaryRequestSpec &spec)
{
    loop.assertInLoopThread();
    if (!Ready() || draining || !session)
    {
        ClientSession::SubmitResult result;
        result.error = Status(StatusCode::kUnavailable, "connection is not ready for new streams");
        result.retryOnNewConnection = true;
        return result;
    }
    auto result = session->SubmitUnary(spec);
    if (result.retryOnNewConnection)
    {
        draining = true;
    }
    return result;
}

void Http2Transport::FlushOutput()
{
    if (closed || !session)
    {
        return;
    }
    if (sessionDepth > 0)
    {
        flushDeferred = true;
        return;
    }
    const auto conn = connection.lock();
    if (!conn || !conn->connected())
    {
        return;
    }
    if (conn->outputBuffer()->readableBytes() >= options.maxPendingOutputBytes)
    {
        return; // 输出缓冲已满:等 writeComplete 回调再继续取帧,不让缓冲越过上限
    }
    flushDeferred = false;
    ++sessionDepth;
    const Status status = session->DrainOutput([this, &conn](std::string_view chunk)
                                                {
        conn->send(chunk.data(), static_cast<int>(chunk.size()));
        return conn->outputBuffer()->readableBytes() < options.maxPendingOutputBytes; });
    --sessionDepth;
    if (!status.ok())
    {
        CloseWithError(status);
        return;
    }
    if (flushDeferred)
    {
        // 发送过程中又有新帧被提交(例如流结束回调里提交了 RST):再刷一轮。
        FlushOutput();
        return;
    }
    // writeComplete 路径也要看一眼:会话可能在上次收包时被 nghttp2 终止、而 GOAWAY 直到这次才刷出去。
    if (!closed && session->ShouldClose())
    {
        CloseWithError(Status(StatusCode::kUnavailable, "http2 session terminated"));
    }
}

void Http2Transport::ResetStream(int32_t streamId, uint32_t http2ErrorCode, Status status)
{
    if (closed || !session)
    {
        return;
    }
    session->ResetStream(streamId, http2ErrorCode, std::move(status));
    FlushOutput();
}

void Http2Transport::OnCallDeadlineExpired(std::chrono::steady_clock::time_point callStartedAt)
{
    if (!Ready() || !session || probeOutstanding)
    {
        return;
    }
    if (lastFrameReceivedAt > callStartedAt)
    {
        return; // 连接有动静,只是这个调用慢
    }
    // §4.4 放行条件(对 grpc-go 默认 keepalive 强制策略,正常情况下不记 strike):
    //   1. 这条连接从没发过 PING(第一个 PING 不记);
    //   2. 上个 PING 的 ACK 之后收到过 HEADERS / DATA(服务端写过数据,下一个 PING 清零 strike);
    //   3. 距上个 PING 已超过 grpc-go 默认 MinTime(5min)加余量(有活跃流时不记 strike);
    //   4. 以上都不满足时,至多再放行 1 次「无法确认已清零」的探测 —— 否则一次探测被 ACK 后紧接着网络黑洞,
    //      这条连接就再也探测不到,只能等内核 TCP 重传放弃(Linux 约 15 分钟)。
    // 1/2 也有例外(grpc-go handlePing 先排 ACK 后做 CAS 的竞态、writeEarlyAbort 的 trailers-only 不清零),
    // 所以 4 只给一次额度:这条路径累计的 strike 最多到 1~2,低于 grpc-go 的 >2 才 GOAWAY 的阈值。
    constexpr auto kGrpcGoMinPingInterval = std::chrono::minutes(5) + std::chrono::seconds(10);
    const auto now = std::chrono::steady_clock::now();
    const bool confirmed = !pingEverSent || headersOrDataAfterLastPingAck ||
                           now - lastPingSentAt >= kGrpcGoMinPingInterval;
    if (!confirmed && unconfirmedProbes >= 1)
    {
        return;
    }
    if (!SendProbePing(now))
    {
        return;
    }
    unconfirmedProbes = confirmed ? 0 : unconfirmedProbes + 1;
}

bool Http2Transport::SendProbePing(std::chrono::steady_clock::time_point now)
{
    if (!session->SubmitPing())
    {
        return false;
    }
    pingEverSent = true;
    lastPingSentAt = now;
    probeOutstanding = true;
    headersOrDataAfterLastPingAck = false;
    probeDeadline = now + options.keepaliveTimeout;
    ArmProbeTimer(++probeGeneration);
    FlushOutput();
    return true;
}

void Http2Transport::ScheduleKeepalive(std::chrono::steady_clock::duration delay)
{
    if (options.keepaliveTime.count() <= 0 || keepaliveTimerArmed || closed)
    {
        return;
    }
    std::weak_ptr<Http2Transport> weakSelf = weak_from_this();
    keepaliveTimer = loop.runAfter(ToSeconds(delay), [weakSelf]
                                     {
        if (const auto self = weakSelf.lock())
        {
            self->HandleKeepaliveTimer();
        } });
    keepaliveTimerArmed = true;
}

void Http2Transport::HandleKeepaliveTimer()
{
    keepaliveTimerArmed = false;
    if (closed)
    {
        return;
    }
    // §13.3:有活跃流、且距最近收到的帧已满 keepaliveTime 才发;按 steady_clock 判(muduo 定时器也按单调时钟到期,墙钟跳变不影响检查时机)。
    const auto now = std::chrono::steady_clock::now();
    const auto idle = now - lastFrameReceivedAt;
    if (Ready() && session && ActiveStreamCount() > 0 && !probeOutstanding && idle >= options.keepaliveTime)
    {
        SendProbePing(now);
        ScheduleKeepalive(options.keepaliveTime);
        return;
    }
    // 下一次检查按「最近收到的帧 + keepaliveTime」上弦(与 grpc-go 一致),而不是固定周期:
    // 否则最坏要多等一整个周期才发 PING。没有活跃流 / 探测在途时按整周期再看。
    const bool idleCountdown = Ready() && ActiveStreamCount() > 0 && !probeOutstanding && idle < options.keepaliveTime;
    ScheduleKeepalive(idleCountdown ? options.keepaliveTime - idle : options.keepaliveTime);
}

ClientSession::SubmitResult Http2Transport::StartBidiStream(const std::string &path, const Metadata &metadata)
{
    loop.assertInLoopThread();
    if (!Ready() || draining || !session)
    {
        ClientSession::SubmitResult result;
        result.error = Status(StatusCode::kUnavailable, "connection is not ready for new streams");
        result.retryOnNewConnection = true;
        return result;
    }
    auto result = session->SubmitStream(path, metadata);
    if (result.retryOnNewConnection)
    {
        draining = true;
    }
    return result;
}

bool Http2Transport::WriteStreamMessage(int32_t streamId, std::string_view framedMessage)
{
    if (closed || !session || !session->AppendStreamMessage(streamId, framedMessage))
    {
        return false;
    }
    FlushOutput();
    return true;
}

void Http2Transport::HalfCloseStream(int32_t streamId)
{
    if (closed || !session)
    {
        return;
    }
    session->HalfCloseStream(streamId);
    FlushOutput();
}

size_t Http2Transport::PendingStreamBytes(int32_t streamId) const
{
    return session ? session->PendingStreamBytes(streamId) : 0;
}

void Http2Transport::Shutdown()
{
    listener.reset();
    if (!closed)
    {
        closed = true;
        ready = false;
    }
    CancelTimers();
    CloseConnection();
}

void Http2Transport::HandleConnection(const muduo::net::TcpConnectionPtr &conn)
{
    if (conn->connected())
    {
        if (closed)
        {
            // 已经放弃(握手超时之后才连上):不要这条连接。
            conn->forceClose();
            return;
        }
        connection = conn;
        conn->setTcpNoDelay(true);
        // 先在成员函数里转成私有基类引用:make_unique 内部看不到这个私有基类,直接传 *this 编不过。
        ClientSession::Observer &observer = *this;
        session = std::make_unique<ClientSession>(options.session, observer);
        const Status started = session->Start();
        if (!started.ok())
        {
            CloseWithError(started);
            return;
        }
        // 发出 client magic + SETTINGS;READY 要等对端 SETTINGS(gRPC 语义:TCP 连上只说明端口在监听,
        // 对端是不是可用的 HTTP/2 服务端要看它的 SETTINGS)。
        FlushOutput();
        return;
    }

    connection.reset();
    if (!closed)
    {
        CloseWithError(Status(StatusCode::kUnavailable, ready ? "connection closed by peer"
                                                               : "connection closed during http2 handshake"));
    }
}

void Http2Transport::HandleMessage(const muduo::net::TcpConnectionPtr &, muduo::net::Buffer *buffer)
{
    if (closed || !session)
    {
        buffer->retrieveAll();
        return;
    }
    // 每次读事件 muduo 只做一次 readv(≤ 可写空间 + 64KiB),这里同步解完,单轮 CPU 有界(§7)。
    ++sessionDepth;
    const Status status =
        session->Receive(reinterpret_cast<const uint8_t *>(buffer->peek()), buffer->readableBytes());
    --sessionDepth;
    buffer->retrieveAll();
    if (!status.ok())
    {
        CloseWithError(status);
        return;
    }
    if (closed)
    {
        return;
    }
    FlushOutput(); // WINDOW_UPDATE / PING ACK / SETTINGS ACK / 回调里提交的 RST
    if (!closed && session->ShouldClose())
    {
        CloseWithError(Status(StatusCode::kUnavailable, "http2 session terminated"));
    }
}

void Http2Transport::HandleConnectFailed(int savedErrno)
{
    // 已放弃(连接超时 / Shutdown):CloseConnection 已经停过 Connector,这次失败不再报。
    if (closed)
    {
        return;
    }
    // 1) 停掉 Connector 的内部重试:Connector::retry 在本回调返回后复核 connect_,已停就不再排下一次;
    //    重连只留 Channel 按 gRPC 退避这一套。此时没有连接,stop() 也不会碰到 TcpConnection。
    client.stop();
    // 2) 关闭排到下一轮:CloseWithError 会同步回调 Channel::OnTransportClosed,后者释放本对象、改通道状态;
    //    本回调可能正处在 Connector 自己的 Channel 事件栈上,也可能就在 Connect() → Channel::CreateTransport
    //    里同步发生(connect() 当场失败),都不能在这里做(设计 §6)。
    //    与连接超时只会关一次:先到的一方置 closed_,后到的在 CloseWithError 开头返回。
    std::weak_ptr<Http2Transport> weakSelf = weak_from_this();
    loop.queueInLoop([weakSelf, savedErrno]
                      {
        if (const auto self = weakSelf.lock())
        {
            self->CloseWithError(Status(StatusCode::kUnavailable,
                                        savedErrno != 0 ? "connect failed (errno " + std::to_string(savedErrno) + ")"
                                                        : std::string("connect failed")));
        } });
}

void Http2Transport::HandleConnectTimeout()
{
    connectTimerArmed = false;
    if (ready || closed)
    {
        return;
    }
    if (std::chrono::steady_clock::now() < connectDeadline)
    {
        ArmConnectTimer(); // 定时器早到(秒→微秒截断等):按单调钟剩余时间重新上弦
        return;
    }
    LOG_WARN << "muduo_grpc connect to " << target << " timed out after " << options.connectTimeout.count()
             << "ms (no HTTP/2 SETTINGS from peer)";
    CloseWithError(Status(StatusCode::kUnavailable,
                          "connect timeout after " + std::to_string(options.connectTimeout.count()) + "ms"));
}

void Http2Transport::HandleProbeTimeout(uint64_t probeGeneration)
{
    if (probeGeneration != this->probeGeneration)
    {
        return;
    }
    probeTimerArmed = false;
    if (!probeOutstanding || closed)
    {
        return;
    }
    if (std::chrono::steady_clock::now() < probeDeadline)
    {
        ArmProbeTimer(probeGeneration); // 定时器早到(秒→微秒截断等):按单调钟剩余时间重新上弦
        return;
    }
    LOG_WARN << "muduo_grpc connection to " << target << " did not acknowledge a liveness PING within "
             << options.keepaliveTimeout.count() << "ms; treating it as dead";
    CloseWithError(Status(StatusCode::kUnavailable, "keepalive ping not acknowledged"));
}

void Http2Transport::CloseWithError(const Status &reason)
{
    if (closed)
    {
        return;
    }
    if (sessionDepth > 0)
    {
        // 不在 nghttp2 的回调栈里拆:推迟到本轮回调结束。
        std::weak_ptr<Http2Transport> weakSelf = weak_from_this();
        loop.queueInLoop([weakSelf, reason]
                          {
            if (const auto self = weakSelf.lock())
            {
                self->CloseWithError(reason);
            } });
        return;
    }
    // 若 nghttp2 因协议错误排了 GOAWAY,尽量随手发出(输出缓冲为空时 send 直接写 socket)。
    // 只尽力而为:这里不能走 FlushOutput,它失败时会回到 CloseWithError。
    if (session)
    {
        if (const auto conn = connection.lock(); conn && conn->connected())
        {
            ++sessionDepth;
            (void)session->DrainOutput([this, &conn](std::string_view chunk)
                                        {
                conn->send(chunk.data(), static_cast<int>(chunk.size()));
                return conn->outputBuffer()->readableBytes() < options.maxPendingOutputBytes; });
            --sessionDepth;
        }
    }
    closed = true;
    const bool wasReady = ready;
    ready = false;
    CancelTimers();

    if (session)
    {
        ++sessionDepth;
        session->FailAllStreams(reason);
        --sessionDepth;
    }
    if (wasReady)
    {
        LOG_WARN << "muduo_grpc connection to " << target << " closed: " << reason.ToString();
    }
    if (const auto listener = this->listener.lock())
    {
        listener->OnTransportClosed(*this, reason);
    }
    CloseConnection();
}

void Http2Transport::CloseConnection()
{
    const auto conn = client.connection();
    if (conn)
    {
        conn->forceClose();
        return;
    }
    // 还没连上:停掉 Connector 的内部重试(连接失败回调里已停过的,再停一次无害)。有连接时用不着:
    // 本类不 enableRetry,Connector 连上之后不会再自己发起连接。stop() 排队的 stopInLoop 已改持
    // Connector 的 shared_ptr(muduo LOCAL DELTA),不再有上游裸指针 FIXME 的悬空风险。
    if (connectStarted)
    {
        client.stop();
    }
}

void Http2Transport::CancelTimers()
{
    // 进程退出路径上 EventLoop 可能已不在本线程(或已析构):定时器随 loop 一起释放,闭包只持 weak_ptr。
    if (muduo::net::EventLoop::getEventLoopOfCurrentThread() != &loop)
    {
        connectTimerArmed = false;
        probeTimerArmed = false;
        keepaliveTimerArmed = false;
        return;
    }
    if (connectTimerArmed)
    {
        loop.cancel(connectTimer);
        connectTimerArmed = false;
    }
    if (probeTimerArmed)
    {
        loop.cancel(probeTimer);
        probeTimerArmed = false;
    }
    if (keepaliveTimerArmed)
    {
        loop.cancel(keepaliveTimer);
        keepaliveTimerArmed = false;
    }
}

void Http2Transport::OnSessionReady()
{
    ready = true;
    if (connectTimerArmed)
    {
        loop.cancel(connectTimer);
        connectTimerArmed = false;
    }
    ScheduleKeepalive(options.keepaliveTime);
    if (const auto listener = this->listener.lock())
    {
        listener->OnTransportReady(*this);
    }
}

void Http2Transport::OnGoawayReceived(int32_t lastStreamId, uint32_t errorCode, std::string_view debugData)
{
    draining = true;
    if (const auto listener = this->listener.lock())
    {
        listener->OnTransportGoaway(*this, lastStreamId, errorCode, debugData);
    }
}

void Http2Transport::OnFrameReceived(uint8_t frameType)
{
    lastFrameReceivedAt = std::chrono::steady_clock::now();
    if ((frameType == kFrameTypeData || frameType == kFrameTypeHeaders) && pingEverSent && !probeOutstanding)
    {
        headersOrDataAfterLastPingAck = true;
        unconfirmedProbes = 0;
    }
}

void Http2Transport::OnPingAck()
{
    if (!probeOutstanding)
    {
        return;
    }
    probeOutstanding = false;
    headersOrDataAfterLastPingAck = false;
    if (probeTimerArmed)
    {
        loop.cancel(probeTimer);
        probeTimerArmed = false;
    }
}

void Http2Transport::OnStreamClosed(int32_t streamId, StreamOutcome outcome)
{
    if (const auto listener = this->listener.lock())
    {
        listener->OnTransportStreamClosed(*this, streamId, std::move(outcome));
    }
}

void Http2Transport::OnStreamHeaders(int32_t streamId, const Metadata &initialMetadata)
{
    if (const auto listener = this->listener.lock())
    {
        listener->OnTransportStreamHeaders(*this, streamId, initialMetadata);
    }
}

void Http2Transport::OnStreamMessage(int32_t streamId, std::string message)
{
    if (const auto listener = this->listener.lock())
    {
        listener->OnTransportStreamMessage(*this, streamId, std::move(message));
    }
}

} // namespace muduo_grpc
