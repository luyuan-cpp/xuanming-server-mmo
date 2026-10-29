#pragma once

// muduo 驱动的 gRPC 客户端通道:一个目标地址(ip:port)一个对象(docs/design/grpc-muduo-transport.md)。
//
// 对调用方的契约:
//   * 只能在构造时给定的 EventLoop 线程上使用(断言)。本模块不创建任何线程、线程池或 CQ。
//   * StartUnaryCall 的完成回调**恰好一次**,并且**永不**在 StartUnaryCall 内同步执行(一律 queueInLoop 投递)。
//   * 回调里的 Status 只有 OK 才代表对端完成了业务;DEADLINE_EXCEEDED / UNAVAILABLE / CANCELLED 等一律是
//     「结果未知」—— 对端可能已经执行。本模块只对可证明未被处理的请求做一次透明重试(§4.6),不做别的重发。
//   * 通道析构时,未完成的调用以 UNAVAILABLE("channel shut down")投递回调;回调不能指望通道还活着。
//
// 生命周期(AGENTS.md §11.7):必须由 shared_ptr 持有(Create);定时器与连接回调只绑 weak_ptr<自己>;
// 通道必须在其 EventLoop 仍在运行时、于 loop 线程上析构(节点关机时 Node::FinalizeShutdownInLoop 负责清理)。

#include <chrono>
#include <cstdint>
#include <deque>
#include <functional>
#include <map>
#include <memory>
#include <mutex>
#include <random>
#include <set>
#include <string>
#include <unordered_map>
#include <utility>
#include <vector>

#include "muduo/net/InetAddress.h"
#include "muduo/net/TimerId.h"

#include "http2_transport.h"
#include "metadata.h"
#include "status.h"

namespace muduo
{
namespace net
{
class EventLoop;
} // namespace net
} // namespace muduo

namespace muduo_grpc
{

// 与 gRPC 的 connectivity state 同名同义(§5)。
enum class ConnectivityState : uint8_t
{
    kIdle,
    kConnecting,
    kReady,
    kTransientFailure,
    kShutdown,
};

const char *ConnectivityStateName(ConnectivityState state);

// gRPC doc/connection-backoff.md 的默认值。
struct BackoffOptions
{
    std::chrono::milliseconds initial{1000};
    double multiplier{1.6};
    double jitter{0.2};
    std::chrono::milliseconds max{120000};
};

class MaintenanceState {
public:
    bool Begin();
    void Complete(bool known);
    void Close();
    bool Closed() const;
    // 本机回调已返回与远端结果已确认是两个问题；UNKNOWN 不阻止本机 join。
    // 这里只核已入账操作/回调，调用方仍须在原 loop 关闭传输和连接 owner。
    bool Joined() const;
    bool Frontier(std::string& value) const;
    // P1 只读当前已接收集合，不关闭 relay 准入；长控制流由实际 etcd owner 显式分账。
    bool SettledFrontier(std::string& value) const;
    void OwnerOpened();
    void OwnerJoined();
    bool NativeJoined() const;
    // 本地拒绝仍会排队调用业务回调，关准入以后也必须计入本机 join。
    void BeginCallback();
    void CompleteCallback(bool returned);
private:
    mutable std::mutex mutex_;
    std::uint64_t accepted_ = 0, completed_ = 0;
    std::uint64_t callbacksAccepted_ = 0, callbacksCompleted_ = 0;
    bool closed_ = false, unknown_ = false;
    bool callbackTrackingLost_ = false;
    std::uint64_t liveOwners_ = 0;
};

struct ChannelOptions
{
    // 默认共用进程账本；测试可以注入独立域，不清空生产历史。
    std::shared_ptr<MaintenanceState> maintenance;
    std::string authority;                              // 空 = "ip:port"
    std::string userAgent{"grpc-c++-muduo/1.0"};
    size_t maxSendMessageBytes{4 * 1024 * 1024};        // grpc-go 服务端默认接收上限
    size_t maxReceiveMessageBytes{4 * 1024 * 1024};     // grpc C++ 默认接收上限
    uint32_t maxHeaderListBytes{16 * 1024};
    uint32_t initialStreamWindowBytes{1024 * 1024};
    uint32_t connectionWindowBytes{16 * 1024 * 1024};
    size_t maxOutstandingCalls{65536};                  // 排队 + 活跃;超出本地 RESOURCE_EXHAUSTED
    size_t maxPendingOutputBytes{1024 * 1024};
    std::chrono::milliseconds connectTimeout{20000};
    std::chrono::milliseconds keepaliveTimeout{20000};
    // 周期 keepalive(§13.3):0 = 关(Go 业务服务通道);etcd 通道设 30s。只给 keepalive 下限明确且远低于本值的
    // 服务端开(etcd 默认 5s);对 grpc-go 默认强制策略(有流时 5min)的通道开启会被 too_many_pings 断开。
    std::chrono::milliseconds keepaliveTime{0};
    // 单个双向流还没交给 nghttp2 的待发字节上限;已有积压时再写超出就以 RESOURCE_EXHAUSTED 结束该流(§13.2)。
    // 缓冲为空时总放行一条(单条另受 maxSendMessageBytes 约束)。流不计入 maxOutstandingCalls。
    size_t maxStreamPendingSendBytes{1024 * 1024};
    BackoffOptions backoff;
};

struct UnaryCallRequest
{
    std::string method;    // ":path",例如 "/client_rpc_router.ClientRpcRouter/Forward"
    std::string message;   // 已序列化的请求 protobuf
    Metadata metadata;     // 应用层值;-bin 键由传输层再编一层 base64
    std::chrono::steady_clock::time_point deadline;  // 必填;线上编码成 grpc-timeout
};

struct UnaryCallResult
{
    Status status;
    Metadata initialMetadata;   // 响应头里的自定义 metadata(-bin 已解掉线上那层);失败时可能为空
    Metadata trailingMetadata;
    std::string response;       // status OK 时是响应 protobuf 的字节
};

using UnaryCallback = std::function<void(UnaryCallResult &&result)>;

// 双向流的回调(§13.2)。全部经 queueInLoop 投递、保持到达顺序;onClose 恰好一次;Cancel 之后一个都不再回调。
struct StreamCallbacks
{
    std::function<void(const Metadata &initialMetadata)> onHeaders;                      // 可空
    std::function<void(std::string &&message)> onMessage;                               // 可空;每条消息一次
    std::function<void(const Status &status, const Metadata &trailingMetadata)> onClose; // 必填
};

class Channel;

namespace detail
{
struct MaintenanceOperation;
// 一个双向流的通道侧记录;ClientStream 只弱持有它,用来在 Cancel 时就地压住还在队列里的回调。
struct StreamRecord
{
    uint64_t id{0};
    std::string path;
    Metadata metadata;
    // operation 先构造、最后析构，保证 callback target 的析构也属于本机工作。
    std::shared_ptr<MaintenanceOperation> maintenance;
    StreamCallbacks callbacks;
    std::vector<std::string> pendingFramed;  // 开流之前写的消息(已带前缀)
    size_t pendingBytes{0};
    bool writesDone{false};
    bool active{false};
    uint64_t transportId{0};
    int32_t streamId{0};
    bool cancelled{false};
    bool closed{false};
};
} // namespace detail

// 双向流句柄。只能在通道的 EventLoop 线程使用与析构;析构等于 Cancel。通道先没了,句柄的操作一律失败、不崩。
// 对端带 END_STREAM 结束流(trailers / trailers-only)时本端立即补 RST_STREAM(NO_ERROR),onClose 不等调用方 WritesDone。
class ClientStream
{
public:
    ~ClientStream();
    ClientStream(const ClientStream &) = delete;
    ClientStream &operator=(const ClientStream &) = delete;

    // 追加一条已序列化的消息。流已结束 / 已取消 / 已半关闭返回 false;待发超过上限时流以 RESOURCE_EXHAUSTED
    // 结束(经 onClose 报告)并返回 false。连上之前写的消息先缓冲,开流后按顺序发出。
    bool Write(std::string message);
    // 半关闭:已写的消息发完后带 END_STREAM。
    void WritesDone();
    // 取消:发 RST_STREAM(CANCEL),之后不再有任何回调(包括已在队列里的 onClose)。幂等;通道没了也照样生效。
    void Cancel();
    bool closed() const;

private:
    friend class Channel;
    ClientStream(std::weak_ptr<Channel> channel, std::weak_ptr<detail::StreamRecord> record, uint64_t id)
        : channel_(std::move(channel)), record_(std::move(record)), id_(id)
    {
    }

    std::weak_ptr<Channel> channel_;
    std::weak_ptr<detail::StreamRecord> record_;
    uint64_t id_;
};

class Channel : public std::enable_shared_from_this<Channel>, public Http2TransportListener
{
    struct PrivateTag
    {
        explicit PrivateTag() = default;
    };

public:
    static std::shared_ptr<MaintenanceState> ProcessMaintenance();
    // 仅实际 etcd owner 显式使用；UNKNOWN 保留，仍须与业务域共同参加 P3 本机 join。
    static std::shared_ptr<MaintenanceState> ProcessControlMaintenance();
    // ip 只接受 IPv4 字面量(节点注册的 grpc_endpoint 都是 POD / 宿主 IP);非法返回 nullptr 并记 ERROR。
    static std::shared_ptr<Channel> Create(muduo::net::EventLoop &loop, const std::string &ip, uint16_t port,
                                           ChannelOptions options = {});

    Channel(PrivateTag, muduo::net::EventLoop &loop, const std::string &ip, uint16_t port, ChannelOptions options);
    ~Channel() override;
    Channel(const Channel &) = delete;
    Channel &operator=(const Channel &) = delete;

    void StartUnaryCall(UnaryCallRequest request, UnaryCallback done);

    // 开一个双向流(§13.2)。method 是 ":path"。总是返回句柄;通道不可用时句柄已处于关闭状态,
    // onClose(UNAVAILABLE) 会在下一轮投递。流不做透明重试,断了由调用方决定是否重建。
    std::shared_ptr<ClientStream> StartStream(std::string method, Metadata metadata, StreamCallbacks callbacks);

    // 与 grpc::Channel::GetState 同义:tryToConnect 且处于 IDLE 时开始连接。
    ConnectivityState GetState(bool tryToConnect);

    const std::string &target() const { return target_; }
    size_t outstandingCalls() const { return calls_.size(); }

private:
    struct Call
    {
        uint64_t id{0};
        std::string path;
        std::shared_ptr<const std::string> framedMessage;
        Metadata metadata;
        std::chrono::steady_clock::time_point deadline;
        UnaryCallback done;
        bool active{false};          // 已在某条连接上开了流
        uint64_t transportId{0};
        int32_t streamId{0};
        std::chrono::steady_clock::time_point startedAt{};
        bool transparentRetried{false};
    };

    using StreamRecord = detail::StreamRecord;
    using StreamRecordPtr = std::shared_ptr<StreamRecord>;

    friend class ClientStream;
    bool StreamWrite(uint64_t id, std::string message);
    void StreamWritesDone(uint64_t id);
    void StreamCancel(uint64_t id);
    bool StreamIsClosed(uint64_t id) const;

    void DispatchStream(uint64_t id);
    void OpenStreamOnCurrentTransport(const StreamRecordPtr &record);
    void CloseStream(const StreamRecordPtr &record, Status status, Metadata trailingMetadata);
    void StreamCancelForError(const StreamRecordPtr &record, Status status);

    // Http2TransportListener
    void OnTransportReady(Http2Transport &transport) override;
    void OnTransportGoaway(Http2Transport &transport, int32_t lastStreamId, uint32_t errorCode,
                           std::string_view debugData) override;
    void OnTransportStreamClosed(Http2Transport &transport, int32_t streamId, StreamOutcome outcome) override;
    void OnTransportStreamHeaders(Http2Transport &transport, int32_t streamId, const Metadata &initialMetadata) override;
    void OnTransportStreamMessage(Http2Transport &transport, int32_t streamId, std::string message) override;
    void OnTransportClosed(Http2Transport &transport, const Status &reason) override;

    void Dispatch(uint64_t callId);
    void StartOnCurrentTransport(Call &call);
    void FlushPending();
    void QueueFlushPending();
    void QueueRedispatch(uint64_t callId);
    void CompleteCall(uint64_t callId, UnaryCallResult result);
    void FailSoon(UnaryCallback done, Status status);
    void ExpireCall(uint64_t callId);

    void StartConnecting();
    void CreateTransport();
    void RetireCurrentTransport();
    void EnterTransientFailure(const Status &reason);
    void ScheduleReconnect();
    void OnBackoffTimer();
    void ReleaseTransport(std::shared_ptr<Http2Transport> transport);
    void MaybeReleaseDrainingTransport(Http2Transport &transport);
    std::shared_ptr<Http2Transport> FindTransport(uint64_t transportId) const;

    void ArmDeadlineTimer();
    void OnDeadlineTimer();
    void SetState(ConnectivityState state);
    bool LoopIsCurrent() const;

    std::shared_ptr<MaintenanceState> maintenance_;
    muduo::net::EventLoop &loop_;
    const std::string target_;
    const muduo::net::InetAddress address_;
    const ChannelOptions options_;
    Http2TransportOptions transportOptions_;

    ConnectivityState state_{ConnectivityState::kIdle};
    Status lastFailure_;

    std::shared_ptr<Http2Transport> current_;                 // 正在连接或 READY 的连接
    std::vector<std::shared_ptr<Http2Transport>> draining_;   // GOAWAY 后等在途流结束的连接
    uint64_t nextTransportId_{1};

    uint64_t nextCallId_{1};
    std::unordered_map<uint64_t, Call> calls_;                // 所有未完成的调用
    std::deque<uint64_t> pending_;                            // 等连接 READY 的调用(可能含已完成的残留 id)
    std::set<std::pair<std::chrono::steady_clock::time_point, uint64_t>> deadlines_;
    std::map<std::pair<uint64_t, int32_t>, uint64_t> streamToCall_;  // (连接, 流) → 调用
    std::unordered_map<uint64_t, StreamRecordPtr> streams_;           // 所有未结束的双向流
    std::deque<uint64_t> pendingStreams_;                             // 等连接 READY 的双向流
    std::map<std::pair<uint64_t, int32_t>, uint64_t> streamToBidi_;   // (连接, 流) → 双向流
    bool flushPendingQueued_{false};

    muduo::net::TimerId deadlineTimer_;
    bool deadlineTimerArmed_{false};
    std::chrono::steady_clock::time_point deadlineTimerFor_{};

    muduo::net::TimerId backoffTimer_;
    bool backoffTimerArmed_{false};
    double nextBackoffMs_;
    std::mt19937 rng_;
};

} // namespace muduo_grpc
