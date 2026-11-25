#pragma once

// 一条 HTTP/2 连接:muduo TcpClient 收发字节,ClientSession 做协议(docs/design/grpc-muduo-transport.md §3、§4.4、§6)。
//
// 生命周期(AGENTS.md §11.7):
//   * 必须由 shared_ptr 持有(Create);Connect() 里才把 weak_ptr<自己> 绑进 TcpClient 回调与定时器。
//   * 连接所有权只在 muduo:本类按值持有 TcpClient,只存 weak_ptr<TcpConnection>。
//   * 一个对象只对应一次连接尝试、一条 TCP 连接;断开后不重连,由 Channel 新建下一个。
//     muduo Connector 报连接失败时立即停掉它的内部重试,下一轮 CloseWithError,通道随即进 TRANSIENT_FAILURE
//     按自己的退避重连;connectTimeout 只兜「TCP 连上但对端迟迟不发 SETTINGS」与未报失败的情形。
//   * 析构前显式 forceClose,随后 ~TcpClient 只替换 closeCallback;析构不得发生在本对象自己的回调栈上
//     (Channel 一律在 queueInLoop 里释放)。
// 线程:全部方法只能在所属 EventLoop 线程调用。

#include <chrono>
#include <cstdint>
#include <memory>
#include <string>
#include <string_view>

#include "muduo/net/Callbacks.h"
#include "muduo/net/InetAddress.h"
#include "muduo/net/TcpClient.h"
#include "muduo/net/TimerId.h"

#include "client_session.h"
#include "status.h"

namespace muduo
{
namespace net
{
class Buffer;
class EventLoop;
} // namespace net
} // namespace muduo

namespace muduo_grpc
{

class Http2Transport;

struct Http2TransportOptions
{
    ClientSessionOptions session;
    // 输出缓冲超过这个量就暂停从 nghttp2 取帧,等 writeComplete 再继续(慢对端不会把内存拖满)。
    size_t maxPendingOutputBytes{1024 * 1024};
    // 一次连接尝试的上限:TCP 建连 + 收到对端 SETTINGS(gRPC MIN_CONNECT_TIMEOUT)。
    std::chrono::milliseconds connectTimeout{20000};
    // 存活探测 PING 的 ACK 等待上限(与 grpc-go 服务端的 keepalive ACK 超时一致)。
    std::chrono::milliseconds keepaliveTimeout{20000};
    // 周期 keepalive(§13.3):0 = 关。>0 时 READY、有活跃流、且距最近收到的帧已满这个时长就发 PING。
    // 只给对端 keepalive 下限明确的通道开(etcd);Go 业务服务通道保持 0,走截止触发的探测(§4.4)。
    std::chrono::milliseconds keepaliveTime{0};
};

// 连接事件的接收方(Channel)。全部在 EventLoop 线程上同步回调;实现里不得同步销毁 Http2Transport,
// 也不得同步向同一条连接提交新流(请投递到下一轮)。
class Http2TransportListener
{
public:
    virtual ~Http2TransportListener() = default;
    virtual void OnTransportReady(Http2Transport &transport) = 0;
    virtual void OnTransportGoaway(Http2Transport &transport, int32_t lastStreamId, uint32_t errorCode,
                                   std::string_view debugData) = 0;
    virtual void OnTransportStreamClosed(Http2Transport &transport, int32_t streamId, StreamOutcome outcome) = 0;
    // 双向流的响应头与逐条消息(unary 流不回调这两项)。
    virtual void OnTransportStreamHeaders(Http2Transport & /*transport*/, int32_t /*streamId*/,
                                          const Metadata & /*initialMetadata*/) {}
    virtual void OnTransportStreamMessage(Http2Transport & /*transport*/, int32_t /*streamId*/,
                                          std::string /*message*/) {}
    // 连接已经不可用(建连失败、握手超时、对端断开、协议错误、探测超时)。在途流此前已逐个 OnTransportStreamClosed。
    virtual void OnTransportClosed(Http2Transport &transport, const Status &reason) = 0;
};

class Http2Transport : public std::enable_shared_from_this<Http2Transport>, private ClientSession::Observer
{
    struct PrivateTag
    {
        explicit PrivateTag() = default;
    };

public:
    static std::shared_ptr<Http2Transport> Create(muduo::net::EventLoop &loop, const muduo::net::InetAddress &address,
                                                  const std::string &name, Http2TransportOptions options,
                                                  std::weak_ptr<Http2TransportListener> listener, uint64_t id);

    Http2Transport(PrivateTag, muduo::net::EventLoop &loop, const muduo::net::InetAddress &address,
                   const std::string &name, Http2TransportOptions options,
                   std::weak_ptr<Http2TransportListener> listener, uint64_t id);
    ~Http2Transport() override;
    Http2Transport(const Http2Transport &) = delete;
    Http2Transport &operator=(const Http2Transport &) = delete;

    uint64_t Id() const { return id; }
    bool Ready() const { return ready && !closed; }
    bool Draining() const { return draining; }
    bool Closed() const { return closed; }
    size_t ActiveStreamCount() const { return session ? session->ActiveStreamCount() : 0; }

    // 开始连接;每个对象只调用一次。
    void Connect();

    // 提交一个 unary 请求(只能在 ready() 且未 draining 时调用)。**不会**自动发送:调用方先登记流 id,
    // 再调 FlushOutput —— 否则发送过程中同步回调的流结束事件会找不到登记。
    ClientSession::SubmitResult StartUnaryStream(const UnaryRequestSpec &spec);

    // 开双向流(§13.2);与 StartUnaryStream 一样不自动发送,调用方登记后调 FlushOutput。
    ClientSession::SubmitResult StartBidiStream(const std::string &path, const Metadata &metadata);
    // 往流上追加一条已带前缀的消息并尝试发出;流已不在返回 false。
    bool WriteStreamMessage(int32_t streamId, std::string_view framedMessage);
    void HalfCloseStream(int32_t streamId);
    size_t PendingStreamBytes(int32_t streamId) const;

    // 把 nghttp2 里待发的帧写进 TCP 输出缓冲(在会话回调栈里调用时自动推迟到回调返回后)。
    void FlushOutput();

    // 本端终止一个流(截止 / 取消),并立即尝试发出 RST_STREAM。
    void ResetStream(int32_t streamId, uint32_t http2ErrorCode, Status status);

    // 某个调用的截止时间到期:若这条连接自该调用发出以来一个帧都没收到,按 §4.4 的放行规则发一次存活探测
    // (正常情况下对 grpc-go 默认强制策略不记 strike,例外与上限见 §4.4)。
    void OnCallDeadlineExpired(std::chrono::steady_clock::time_point callStartedAt);

    // 不再在这条连接上开新流(stream id 用尽等)。
    void MarkDraining() { draining = true; }

    // 通道主动放弃这条连接:不再回调 listener,关闭 TCP。幂等。
    void Shutdown();

private:
    void HandleConnection(const muduo::net::TcpConnectionPtr &conn);
    void HandleMessage(const muduo::net::TcpConnectionPtr &conn, muduo::net::Buffer *buffer);
    // Connector 的连接失败回调(在它自己的 Channel 事件栈或 Connect() 栈上):只停重试并把关闭排到下一轮。
    void HandleConnectFailed(int savedErrno);
    void HandleConnectTimeout();
    void HandleProbeTimeout(uint64_t probeGeneration);
    void ArmConnectTimer();
    void ArmProbeTimer(uint64_t probeGeneration);
    void CloseWithError(const Status &reason);
    void CloseConnection();
    void CancelTimers();

    // ClientSession::Observer
    void OnSessionReady() override;
    void OnGoawayReceived(int32_t lastStreamId, uint32_t errorCode, std::string_view debugData) override;
    void OnFrameReceived(uint8_t frameType) override;
    void OnPingAck() override;
    void OnStreamClosed(int32_t streamId, StreamOutcome outcome) override;
    void OnStreamHeaders(int32_t streamId, const Metadata &initialMetadata) override;
    void OnStreamMessage(int32_t streamId, std::string message) override;

    // delay 之后检查一次是否该发周期 PING(§13.3)。已上弦则不动。
    void ScheduleKeepalive(std::chrono::steady_clock::duration delay);
    void HandleKeepaliveTimer();
    // 发一个存活 PING 并上弦 ACK 超时;调用方已判断过放行条件。
    bool SendProbePing(std::chrono::steady_clock::time_point now);

    muduo::net::EventLoop &loop;
    muduo::net::TcpClient client;
    const std::string target;
    const Http2TransportOptions options;
    std::weak_ptr<Http2TransportListener> listener;
    const uint64_t id;

    std::weak_ptr<muduo::net::TcpConnection> connection;
    std::unique_ptr<ClientSession> session;

    bool connectStarted{false};
    bool ready{false};
    bool draining{false};
    bool closed{false};
    // >0 表示正处在 session_ 的 Receive / DrainOutput / FailAllStreams 调用栈里:此时不重入 nghttp2。
    int sessionDepth{0};
    bool flushDeferred{false};

    // muduo 定时器已按单调时钟到期;截止点仍另记一份 steady_clock,到点复核,未到就按剩余时间重新上弦
    // (兜住秒→微秒截断造成的微秒级早到)。
    muduo::net::TimerId connectTimer;
    bool connectTimerArmed{false};
    std::chrono::steady_clock::time_point connectDeadline{};
    muduo::net::TimerId probeTimer;
    bool probeTimerArmed{false};
    uint64_t probeGeneration{0};
    std::chrono::steady_clock::time_point probeDeadline{};
    muduo::net::TimerId keepaliveTimer;
    bool keepaliveTimerArmed{false};

    // 存活探测(§4.4)
    std::chrono::steady_clock::time_point lastFrameReceivedAt{};
    std::chrono::steady_clock::time_point lastPingSentAt{};
    bool pingEverSent{false};
    bool probeOutstanding{false};
    bool headersOrDataAfterLastPingAck{false};
    // 发出时无法确认 grpc-go 会清零 strike 的探测已连续发了几个;至多 1 个。
    int unconfirmedProbes{0};
};

} // namespace muduo_grpc
