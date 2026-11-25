#pragma once

// gRPC 客户端协议引擎:一条 HTTP/2 连接上的 nghttp2 client session(docs/design/grpc-muduo-transport.md §3、§4)。
//
// 纯协议层:不碰 socket、不碰定时器、不碰 EventLoop。字节由使用方喂进来(Receive)、取出去(DrainOutput),
// 所以单测可以把它和一个内存里的 nghttp2 服务端会话直接对接,完全确定性。
//
// 线程:不加锁,只能由同一个线程使用(生产上是连接所属的 EventLoop 线程)。
// 回调:Observer 的方法都在 Receive / DrainOutput / ResetStream / FailAllStreams 的调用栈里同步触发;
// Observer 里**不得**销毁本对象,也不得同步重入 SubmitUnary(需要重发的请求请投递到下一轮)。

#include <cstddef>
#include <cstdint>
#include <functional>
#include <memory>
#include <optional>
#include <string>
#include <string_view>
#include <unordered_map>
#include <vector>

#include "message_framing.h"
#include "metadata.h"
#include "status.h"

struct nghttp2_session;

namespace muduo_grpc
{

struct ClientSessionOptions
{
    std::string authority;  // :authority,通常是 "ip:port"
    std::string userAgent;  // user-agent
    size_t maxReceiveMessageBytes{4 * 1024 * 1024};
    uint32_t maxHeaderListBytes{16 * 1024};
    uint32_t initialStreamWindowBytes{1024 * 1024};
    uint32_t connectionWindowBytes{16 * 1024 * 1024};
};

// 一次 unary 请求在线上需要的全部输入。
struct UnaryRequestSpec
{
    std::string path;                                     // "/pkg.Service/Method"
    std::shared_ptr<const std::string> framedMessage;     // 已带 5 字节前缀;与通道共享,透明重试时不必再拷贝
    Metadata metadata;                                    // 应用层值;-bin 键由本层再编一层 base64
    std::string grpcTimeout;                              // 已编码的 grpc-timeout;空 = 不发
};

// 一个流结束时的完整结果。
struct StreamOutcome
{
    Status status;
    Metadata initialMetadata;   // 响应 HEADERS 里的自定义头(-bin 已解掉线上那层);trailers-only 时为空
    Metadata trailingMetadata;  // trailers 里的自定义头
    std::string response;       // status OK 时恰好一条消息的字节(不含前缀)
    // 对端可证明没有处理这个请求:被 REFUSED_STREAM 关闭(含 GOAWAY 之后的流、因 GOAWAY 没发出去的 HEADERS),
    // 或者连接断开时请求 HEADERS 根本没交给 socket。只有这种情况允许透明重试(§4.6)。
    bool unprocessed{false};
};

class ClientSession
{
public:
    class Observer
    {
    public:
        virtual ~Observer() = default;
        // 收到对端首个(非 ACK)SETTINGS:握手完成。
        virtual void OnSessionReady() = 0;
        // 收到 GOAWAY;可能出现多次,last_stream_id 只减不增。
        virtual void OnGoawayReceived(int32_t lastStreamId, uint32_t errorCode, std::string_view debugData) = 0;
        // 收到任意帧(用于存活判断;DATA / HEADERS 另有含义,见 Http2Transport 的探测规则)。
        virtual void OnFrameReceived(uint8_t frameType) = 0;
        // 收到我们发出的 PING 的 ACK。
        virtual void OnPingAck() = 0;
        // 一个流结束;每个成功提交的流恰好回调一次。
        virtual void OnStreamClosed(int32_t streamId, StreamOutcome outcome) = 0;
        // 以下两项只对 SubmitStream 开的流回调(unary 流的头部与消息随 OnStreamClosed 一次给出)。
        // 收到响应头(初始 metadata,-bin 已解掉线上那层);trailers-only 时不回调。
        virtual void OnStreamHeaders(int32_t /*streamId*/, const Metadata & /*initialMetadata*/) {}
        // 收到一条完整消息(不含 5 字节前缀),按到达顺序。
        virtual void OnStreamMessage(int32_t /*streamId*/, std::string /*message*/) {}
    };

    struct SubmitResult
    {
        int32_t streamId{0};  // > 0 表示成功
        Status error;         // 失败原因(streamId <= 0 时有效)
        // stream id 用尽或连接已不允许开新流:请求没有发出,换一条连接重发是安全的。
        bool retryOnNewConnection{false};
    };

    ClientSession(ClientSessionOptions options, Observer &observer);
    ~ClientSession();
    ClientSession(const ClientSession &) = delete;
    ClientSession &operator=(const ClientSession &) = delete;

    // 创建 nghttp2 会话,提交本端 SETTINGS 与连接级窗口。client magic 由 nghttp2 在首次 DrainOutput 时自动发出。
    Status Start();

    SubmitResult SubmitUnary(const UnaryRequestSpec &spec);

    // 开一个双向流(§13.2):只发请求头;消息之后用 AppendStreamMessage 逐条追加,HalfCloseStream 半关闭。
    // 不带 grpc-timeout(流没有截止时间)。结果:消息经 OnStreamMessage 逐条到达,结束经 OnStreamClosed。
    SubmitResult SubmitStream(const std::string &path, const Metadata &metadata);

    // 往流上追加一条已带 5 字节前缀的消息。流不存在 / 已半关闭返回 false。真正写出要 DrainOutput。
    bool AppendStreamMessage(int32_t streamId, std::string_view framedMessage);

    // 半关闭:已追加的消息发完后带 END_STREAM。
    void HalfCloseStream(int32_t streamId);

    // 流上还没交给 nghttp2 的待发字节数(背压用);流不存在返回 0。
    size_t PendingStreamBytes(int32_t streamId) const;

    // 本端终止一个流:发 RST_STREAM(http2ErrorCode);之后的 OnStreamClosed 以 status 报告(若对端结果先到,以先到者为准)。
    void ResetStream(int32_t streamId, uint32_t http2ErrorCode, Status status);

    // 喂入从 socket 读到的全部字节。返回非 OK = 连接级致命错误。
    Status Receive(const uint8_t *data, size_t len);

    // 把待发字节逐块交给 write;write 返回 false 表示先停(输出缓冲已高),已交出的块必须被消费。
    Status DrainOutput(const std::function<bool(std::string_view chunk)> &write);

    // nghttp2 既不想读也不想写:连接应当关闭(例如收到 GOAWAY 且流已清空、或本端因协议错误发了 GOAWAY)。
    bool ShouldClose() const;

    bool SubmitPing();

    // 本端主动断开前发 GOAWAY(仍需 DrainOutput 才会真正写出)。
    void SubmitGoaway(uint32_t http2ErrorCode);

    // 连接已死:把所有未结束的流以 status 结束,逐个 OnStreamClosed。HEADERS 没交给 socket 的标为 unprocessed。
    void FailAllStreams(const Status &status);

    size_t ActiveStreamCount() const { return streams.size(); }
    bool GoawayReceived() const { return goawayReceived; }

private:
    struct StreamState
    {
        explicit StreamState(size_t maxMessageBytes) : deframer(maxMessageBytes) {}

        std::shared_ptr<const std::string> framedRequest;  // unary:整个请求
        size_t sentOffset{0};
        bool requestHeadersSent{false};

        // 双向流(SubmitStream):待发字节在 pendingSend 里,数据提供者没数据时返回 DEFERRED 挂起,追加后 resume。
        bool streaming{false};
        std::string pendingSend;
        size_t pendingSendOffset{0};
        bool writesDone{false};
        bool dataDeferred{false};
        // 对端已带 END_STREAM 结束了流(本端随即补 RST_STREAM(NO_ERROR),见 .cpp 的 CloseStreamEndedByPeer);之后不再接受写入。
        bool remoteEnded{false};

        // 当前正在读的头部块
        enum class Block
        {
            kNone,
            kResponseHeaders,  // 首个 HEADERS,不带 END_STREAM
            kTrailersOnly,     // 首个 HEADERS 带 END_STREAM
            kTrailers,         // 后续 HEADERS
        };
        Block block{Block::kNone};
        size_t blockBytes{0};

        bool responseHeadersReceived{false};
        bool trailersReceived{false};
        int httpStatus{0};
        bool contentTypeIsGrpc{false};
        bool grpcStatusPresent{false};
        std::optional<StatusCode> grpcStatus;
        std::string grpcMessage;
        Metadata initialMetadata;
        Metadata trailingMetadata;

        MessageDeframer deframer;
        std::vector<std::string> messages;

        // 本端已决定的结果(截止 / 超限 / 协议违约);一旦设置,对端后续结果不再覆盖。
        std::optional<Status> localStatus;
    };

    // nghttp2 回调的实现体在 .cpp 里(签名带 nghttp2 类型,头文件不暴露 nghttp2.h)。
    // user_data 即 this;回调只会在本对象的方法调用栈里发生,所以 this 一定有效。
    friend struct ClientSessionCallbacks;

    StreamOutcome BuildOutcome(StreamState &stream, uint32_t http2ErrorCode) const;
    void SetLocalErrorAndReset(int32_t streamId, StreamState &stream, Status status, uint32_t http2ErrorCode);
    // 双向流的 DATA 挂在 DEFERRED 时唤醒它;失败则按本地错误重置该流(不让它静默停写)。
    void ResumeDeferredData(int32_t streamId, StreamState &stream);
    StreamState *FindStream(int32_t streamId);
    const StreamState *FindStream(int32_t streamId) const;
    // unary 与双向流共用:拼请求头、提交给 nghttp2、登记流。grpcTimeout 为空则不发该头。
    SubmitResult SubmitRequest(const std::string &path, const Metadata &metadata, const std::string &grpcTimeout,
                               std::unique_ptr<StreamState> stream);

    struct SessionDeleter
    {
        void operator()(nghttp2_session *session) const;
    };

    ClientSessionOptions options;
    Observer &observer;
    std::unique_ptr<nghttp2_session, SessionDeleter> session;
    std::unordered_map<int32_t, std::unique_ptr<StreamState>> streams;
    bool readySignaled{false};
    bool goawayReceived{false};
};

} // namespace muduo_grpc
