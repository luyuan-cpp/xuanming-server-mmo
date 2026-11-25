#pragma once

// gRPC 服务端协议引擎:一条 HTTP/2 连接上的 nghttp2 server session(docs/design/grpc-muduo-transport.md §15.4、§15.5)。
//
// 纯协议层,与 ClientSession 对称:不碰 socket、不碰定时器、不碰 EventLoop、不知道方法表与调用表。
// 字节由使用方喂进来(Receive)、取出去(DrainOutput),单测把它和内存里的 nghttp2 客户端会话直接对接。
// 本层负责 §15.5.3 拒绝表里所有「只看这一条流就能判定」的行(头部大小 / :method / content-type /
// grpc-timeout / -bin / grpc-encoding / :path 形状 / 压缩标志 / 声明长度 / 连接内未收完请求体之和 /
// 消息条数 / timeout <= 0);路由、在途上限、排空到期由 Observer 的 Verdict 决定。
//
// 线程:不加锁,只能由同一个线程使用(生产上是连接所属的 EventLoop 线程)。
// 回调:Observer 的方法都在 Receive / DrainOutput 的调用栈里同步触发;Observer 里**不得**销毁本对象,
// 也不得同步调用本对象的 Submit* / RefuseStream / SubmitShutdownNotice / SubmitFinalGoaway(只查表、登记、入队)。
// 析构不回调 Observer:连接拆除时,上层按自己的「streamId → 调用」表逐个撤销(§15.7 第 5 条)。
//
// 恰好一次(本层的部分):
//   * 每个流至多提交一次应答;Submit* / RefuseStream 对已应答或已关闭的流返回 false,不产生任何帧。
//   * OnStreamAborted 只对「OnRequestComplete 返回 kAccept、且本端尚未提交应答」的流触发,且至多一次;
//     提交应答之后流再怎么关闭都不回调。

#include <chrono>
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

struct ServerSessionOptions
{
    size_t maxReceiveMessageBytes{4 * 1024 * 1024};
    uint32_t maxHeaderListBytes{16 * 1024};
    uint32_t maxConcurrentStreams{256};
    uint32_t initialStreamWindowBytes{1024 * 1024};
    uint32_t connectionWindowBytes{16 * 1024 * 1024};
    size_t maxBufferedRequestBytes{16 * 1024 * 1024};   // 本连接所有未收完请求体之和
};

struct RequestHeaders
{
    std::string path;                                 // :path 原值
    Metadata metadata;                                // 只含自定义键,-bin 已解一层
    std::optional<std::chrono::nanoseconds> timeout;  // 没有 grpc-timeout = 无截止;99999999H 这类超大值钳到 nanoseconds::max()
};

class ServerSession
{
public:
    struct Verdict
    {
        enum class Kind : uint8_t
        {
            kAccept,
            kReject,
            kRefuse,
        };
        Kind kind{Kind::kAccept};
        // kReject:回 :status 200 + 该状态的 trailers-only(status 必须非 OK;误给 OK 时按 INTERNAL 回并打 ERROR);
        // kRefuse:回 RST_STREAM(REFUSED_STREAM),status 不用。
        Status status;
    };

    class Observer   // 全部在 Receive / DrainOutput 的 nghttp2 回调栈里同步触发;实现里只查表、登记、入队
    {
    public:
        virtual ~Observer() = default;
        // 收到首个非 ACK SETTINGS(握手完成);之后的 SETTINGS 不再回调。
        virtual void OnClientSettings() = 0;
        // 请求头收完且通过本层校验之后调用(路由 / 排空到期判定)。path 一定能被 SplitMethodPath 拆开。
        // kAccept 之前的任何状态都不需要上层登记:请求没收完就被对端 RST 时不会有后续回调。
        virtual Verdict OnRequestHeaders(int32_t streamId, const RequestHeaders &headers) = 0;
        // 请求收完(END_STREAM)且恰好一条消息、timeout 为正(或无截止)时调用(在途上限检查 / 登记)。
        // 返回 kAccept 表示上层接管了这个流,之后必须用 SubmitResponse / SubmitStatus / RefuseStream 恰好回一次,
        // 或者在 OnStreamAborted 里撤销。
        virtual Verdict OnRequestComplete(int32_t streamId, RequestHeaders headers, std::string message) = 0;
        // 已登记(OnRequestComplete 返回 kAccept)、本端尚未提交应答时流被关闭(对端 RST 等)。
        virtual void OnStreamAborted(int32_t streamId, uint32_t http2ErrorCode) = 0;
        // 收到 SubmitShutdownNotice 发出的 PING 的 ACK;至多一次。
        virtual void OnShutdownPingAck() = 0;
        // 本层自己拒掉了一个请求(§15.5.3 里 Observer 看不到的那些行:头部过大 / :method / content-type /
        // grpc-timeout / -bin / grpc-encoding / :path 形状 / 压缩标志 / 声明长度 / 请求体之和 / 消息条数 /
        // timeout <= 0),供上层计 rejected_protocol 与状态码分布。httpStatus 是回给对端的 :status。
        // 例外:timeout <= 0 那一行(DEADLINE_EXCEEDED)来自合法客户端(截止在发出前已到),不是畸形请求,
        // 上层宜计入 deadline_before_start(§15.11),不计 rejected_protocol、不打「malformed」WARN。
        // Observer 自己给出的 kReject / kRefuse 不会再从这里回报;nghttp2 直接 RST_STREAM(PROTOCOL_ERROR)
        // 的畸形请求也不在其中。默认空实现(§15.4.3 草稿之外的补充)。
        virtual void OnRequestRejected(int32_t /*streamId*/, int /*httpStatus*/, const Status & /*status*/) {}
    };

    ServerSession(ServerSessionOptions options, Observer &observer);
    ~ServerSession();
    ServerSession(const ServerSession &) = delete;
    ServerSession &operator=(const ServerSession &) = delete;

    // 建会话、设 nghttp2 选项(§15.5.2)、提交本端 SETTINGS 与连接级窗口;字节要等 DrainOutput 才写出。
    Status Start();

    // 喂入从 socket 读到的全部字节。非 OK = 连接级致命错误(坏 magic / 洪泛 / nghttp2 内部失败):
    // 此后 Receive 一律返回同一个错误,DrainOutput 不再写任何字节(返回 OK),ShouldClose() 为真;调用方直接关连接。
    // 帧级协议错误不在这里报:nghttp2 只是排一个带错误码的 GOAWAY(Receive 返回 OK),由 DrainOutput 在写出它时报告。
    Status Receive(const uint8_t *data, size_t len);

    // 把待发字节逐块交给 write;write 返回 false 表示先停(输出缓冲已高),已交出的块必须被消费。
    // 收到对端首字节之前不写任何字节(§15.5.1:本端 SETTINGS 推迟到收到对端首字节才写出,
    // 探针连上就关时本端一个字节都不写)。
    // 非 OK = 连接级错误,各只报告一次:
    //   * nghttp2 因对端违约(协议错误 / 洪泛 / rapid reset)发出了带错误码的 GOAWAY:在写出它的那次调用里报告,
    //     GOAWAY 的字节此时已经交给 write,调用方计 protocol_errors 后关闭连接即可;
    //   * mem_send 失败:之后不再读写,与 Receive 的致命错误同样处理。
    Status DrainOutput(const std::function<bool(std::string_view chunk)> &write);

    // HEADERS(:status 200、content-type)+ DATA(5 字节前缀 + 消息)+ trailers(grpc-status 0,不发 grpc-message)。
    // 流不存在 / 已应答 / 消息超过 UINT32_MAX 返回 false。消息大小上限由上层(Server)负责。
    bool SubmitResponse(int32_t streamId, std::string message);
    // 非 OK 的 trailers-only(:status 200)。status.ok() 视为调用方缺陷:返回 false,不提交。
    bool SubmitStatus(int32_t streamId, const Status &status);
    // RST_STREAM(REFUSED_STREAM):告诉对端「没处理」,grpc-go 会透明重试。
    bool RefuseStream(int32_t streamId);
    // 两段式 GOAWAY 第一段:GOAWAY(2^31-1, NO_ERROR) + PING(ACK 经 OnShutdownPingAck 报告)。
    // PING 在 GOAWAY 写出之后才提交,线上一定是先 GOAWAY 后 PING(与 grpc-go 一致)。
    // 必须先于 SubmitFinalGoaway;只生效一次。之后到达的新流照常处理。
    void SubmitShutdownNotice();
    // 第二段:GOAWAY(nghttp2_session_get_last_proc_stream_id, NO_ERROR)。只生效一次。
    // 写出之后到达的新流被 nghttp2 忽略(不回调 Observer),客户端按 last_stream_id 判为未处理。
    // 提交之后、写出之前到达的新流(id 大于本次 last_stream_id;输出被背压时会出现)同样不交给 Observer:
    // GOAWAY 写出时 nghttp2 会把它们以 REFUSED_STREAM 关掉(不发 RST),客户端判为未处理并透明重试;
    // 若交给上层,业务可能已经执行而应答随流被丢弃,重试就会执行两次。
    void SubmitFinalGoaway();

    bool ReceivedAnyBytes() const { return receivedAnyBytes; }
    bool HandshakeComplete() const { return handshakeComplete; }
    bool FinalGoawaySubmitted() const { return finalGoawaySubmitted; }
    // nghttp2 还没关闭的流(含请求没收完的、已派发等应答的)。
    size_t OpenStreamCount() const { return streams.size(); }
    // 头部已被接受(OnRequestHeaders 返回 kAccept)、请求还没收完因而未派发(OnRequestComplete 没返回 kAccept)、
    // 本端也还没应答的流,即请求体还在上传的流;按 id 升序。业务必然没开始:上层排空到期时据此立即 RefuseStream(§15.10),
    // 不依赖自己的旁路记录(那份记录有上限、会被修剪)。头部还没收完的流不在其中,收完头部时由 Observer 判定。
    std::vector<int32_t> AcceptedUndispatchedStreams() const;
    // 既不想读也不想写(GOAWAY 已发出或收到且没有活跃的流,或会话被终止),或者出过致命错误。
    bool ShouldClose() const;

private:
    struct StreamState
    {
        explicit StreamState(size_t maxMessageBytes) : deframer(maxMessageBytes) {}

        // ---- 请求头块 ----
        bool inRequestHeaders{false};       // 正在读请求头块(请求 trailers 块的头部一律忽略)
        size_t headerBytes{0};              // 名 + 值 + 32 累计
        bool headerListTooLarge{false};     // 超限之后的头不再存储
        std::string method;                 // :method
        std::string contentType;            // 最后一个 content-type 的值(只用于拒绝文案)
        bool contentTypeIsGrpc{false};      // 任一 content-type 合法即为真
        std::optional<std::string> grpcTimeout;
        std::optional<std::string> grpcEncoding;
        std::optional<std::string> badBinaryKey;   // 第一个 base64 非法的 -bin 键
        RequestHeaders headers;

        // ---- 请求体 ----
        bool headersAccepted{false};        // 通过本层校验且 OnRequestHeaders 返回 kAccept
        MessageDeframer deframer;
        std::vector<std::string> messages;
        size_t bufferedBytes{0};            // 已计入连接级未收完请求体之和的字节数

        // ---- 应答 ----
        bool dispatched{false};             // OnRequestComplete 返回 kAccept
        bool responded{false};              // 已提交应答(含本层自己的拒绝、RefuseStream)
        std::string responseBody;           // SubmitResponse:5 字节前缀 + 消息
        size_t responseOffset{0};
    };

    // nghttp2 回调的实现体在 .cpp 里(签名带 nghttp2 类型,头文件不暴露 nghttp2.h)。
    // user_data 即 this;回调只会在本对象的方法调用栈里发生,所以 this 一定有效。
    friend struct ServerSessionCallbacks;

    StreamState *FindStream(int32_t streamId);
    void EndOfRequestHeaders(int32_t streamId, StreamState &stream);
    void EndOfRequest(int32_t streamId, StreamState &stream);
    void OnRequestData(int32_t streamId, StreamState &stream, const uint8_t *data, size_t len);
    void ApplyVerdict(int32_t streamId, StreamState &stream, Verdict verdict);
    // 本层自己的拒绝:回报 OnRequestRejected 后按 FinishStreamWithStatus 回应。
    void RejectStream(int32_t streamId, StreamState &stream, int httpStatus, const Status &status,
                      bool advertiseIdentityEncoding = false);
    // 标记已应答、丢弃请求体,回 httpStatus + trailers-only。
    void FinishStreamWithStatus(int32_t streamId, StreamState &stream, int httpStatus, const Status &status,
                                bool advertiseIdentityEncoding);
    bool SubmitTrailersOnly(int32_t streamId, int httpStatus, const Status &status, bool advertiseIdentityEncoding);
    // 把流的请求体从连接级计数里扣掉并释放(交出、拒绝、关闭时各调一次;重复调用无副作用)。
    void DiscardRequestBody(StreamState &stream);
    // 在 on_frame_send 里登记、回调返回之后才提交的帧(在回调里提交会被 nghttp2 随后的流状态更新覆盖):
    //   * 早拒且请求没收完的流:回完紧跟 RST_STREAM(NO_ERROR)(§15.5.3);
    //   * shutdown notice 写出之后的 PING(保证线上先 GOAWAY 后 PING)。
    void SubmitDeferredFrames();

    struct SessionDeleter
    {
        void operator()(nghttp2_session *session) const;
    };

    ServerSessionOptions options;
    Observer &observer;
    std::unique_ptr<nghttp2_session, SessionDeleter> session;
    std::unordered_map<int32_t, std::unique_ptr<StreamState>> streams;
    std::vector<int32_t> pendingNoErrorResets;
    size_t bufferedRequestBytes{0};
    bool receivedAnyBytes{false};
    bool handshakeComplete{false};
    bool shutdownNoticeSubmitted{false};
    bool shutdownPingPending{false};     // notice 已写出、PING 待提交
    bool shutdownPingSubmitted{false};
    bool shutdownPingAcked{false};
    bool finalGoawaySubmitted{false};
    // 最终 GOAWAY 成功提交时的 last_stream_id;id 大于它的新流不交给 Observer(见 SubmitFinalGoaway)。
    // 默认 2^31-1 = 不限制(流 id 不会超过它)。
    int32_t finalGoawayLastStreamId{0x7fffffff};
    std::optional<Status> fatalError;         // Receive / mem_send 的致命错误(粘滞):之后不再调 nghttp2 读写
    std::optional<Status> connectionError;     // nghttp2 发出了带错误码的 GOAWAY(由写出它的那次 DrainOutput 报告)
};

} // namespace muduo_grpc
