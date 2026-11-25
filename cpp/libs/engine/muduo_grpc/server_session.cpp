#include "server_session.h"

// nghttp2.h 必须先于任何 muduo 头文件(muduo 的 Windows 适配头 #define 了 off_t 等名字)。
#include <nghttp2/nghttp2.h>

#include <algorithm>
#include <cstring>
#include <iterator>
#include <limits>

#include "muduo/base/Logging.h"

#include "grpc_timeout.h"
#include "method_table.h"

namespace muduo_grpc
{

namespace
{

constexpr std::string_view kGrpcContentType = "application/grpc";

// RFC 9113 §6.5.2:头部列表大小按「名 + 值 + 32」累计。
constexpr size_t kHeaderEntryOverheadBytes = 32;

// 两段式 GOAWAY 第一段附带的 PING 负载;ACK 按它匹配(对端的 BDP ping 由 nghttp2 自动 ACK,不会混进来)。
constexpr uint8_t kShutdownPingPayload[8] = {'g', 'o', 'a', 'w', 'a', 'y', 'p', 'g'};

// shutdown notice 的 last_stream_id(2^31-1)。
constexpr int32_t kShutdownNoticeLastStreamId = 0x7fffffff;

// §15.5.2 显式写出的 nghttp2 选项:取值等于 1.70 的默认值,目的是锁住将来的漂移。
constexpr size_t kMaxOutboundAck = 1000;
constexpr size_t kMaxSettingsEntries = 32;
constexpr uint64_t kStreamResetBurst = 1000;       // rapid-reset(CVE-2023-44487)令牌桶
constexpr uint64_t kStreamResetRatePerSecond = 33;
constexpr size_t kMaxContinuations = 8;
constexpr uint64_t kGlitchBurst = 10000;
constexpr uint64_t kGlitchRatePerSecond = 330;

// §15.5.3 早拒用到的 HTTP 状态(与 grpc-go operateHeaders 一致)。
constexpr int kHttpOk = 200;
constexpr int kHttpBadRequest = 400;
constexpr int kHttpMethodNotAllowed = 405;
constexpr int kHttpUnsupportedMediaType = 415;

std::string_view AsView(const uint8_t *data, size_t len)
{
    return std::string_view(reinterpret_cast<const char *>(data), len);
}

bool StartsWith(std::string_view text, std::string_view prefix)
{
    return text.size() >= prefix.size() && text.substr(0, prefix.size()) == prefix;
}

// application/grpc 本身,或其后紧跟 '+'(application/grpc+proto)或 ';'(参数)。
bool IsGrpcContentType(std::string_view value)
{
    if (!StartsWith(value, kGrpcContentType))
    {
        return false;
    }
    if (value.size() == kGrpcContentType.size())
    {
        return true;
    }
    const char next = value[kGrpcContentType.size()];
    return next == '+' || next == ';';
}

nghttp2_nv MakeNv(std::string_view name, std::string_view value)
{
    nghttp2_nv nv;
    nv.name = reinterpret_cast<uint8_t *>(const_cast<char *>(name.data()));
    nv.value = reinterpret_cast<uint8_t *>(const_cast<char *>(value.data()));
    nv.namelen = name.size();
    nv.valuelen = value.size();
    nv.flags = NGHTTP2_NV_FLAG_NONE; // nghttp2 自己拷贝名与值,调用返回后 name / value 即可释放
    return nv;
}

std::string Quoted(std::string_view value)
{
    std::string quoted;
    quoted.reserve(value.size() + 2);
    quoted.push_back('"');
    quoted.append(value.data(), value.size());
    quoted.push_back('"');
    return quoted;
}

} // namespace

// nghttp2 回调。全部只在 ServerSession 的方法调用栈里被 nghttp2 调用(mem_recv2 / mem_send2)。
struct ServerSessionCallbacks
{
    using StreamState = ServerSession::StreamState;

    static ServerSession &Self(void *userData) { return *static_cast<ServerSession *>(userData); }

    static int OnBeginHeaders(nghttp2_session *, const nghttp2_frame *frame, void *userData)
    {
        if (frame->hd.type != NGHTTP2_HEADERS)
        {
            return 0;
        }
        auto &self = Self(userData);
        if (frame->headers.cat == NGHTTP2_HCAT_REQUEST)
        {
            // nghttp2 只对新的对端流 id 走到这里(MAX_CONCURRENT_STREAMS 拒绝的、最终 GOAWAY 之后的新流都不会)。
            const auto [it, inserted] = self.streams.try_emplace(frame->hd.stream_id);
            if (inserted)
            {
                it->second = std::make_unique<StreamState>(self.options.maxReceiveMessageBytes);
            }
            it->second->inRequestHeaders = true;
            return 0;
        }
        // 请求 trailers(gRPC 客户端不发):头部一律忽略,END_STREAM 照常按请求结束处理。
        if (auto *stream = self.FindStream(frame->hd.stream_id))
        {
            stream->inRequestHeaders = false;
        }
        return 0;
    }

    // 只有通过 nghttp2 HTTP 语义校验的头才会走到这里;非法的头由 nghttp2 直接 RST_STREAM(PROTOCOL_ERROR)。
    static int OnHeader(nghttp2_session *, const nghttp2_frame *frame, const uint8_t *nameData, size_t nameLen,
                        const uint8_t *valueData, size_t valueLen, uint8_t, void *userData)
    {
        if (frame->hd.type != NGHTTP2_HEADERS)
        {
            return 0;
        }
        auto &self = Self(userData);
        auto *stream = self.FindStream(frame->hd.stream_id);
        if (stream == nullptr || !stream->inRequestHeaders || stream->headerListTooLarge)
        {
            return 0;
        }
        stream->headerBytes += nameLen + valueLen + kHeaderEntryOverheadBytes;
        if (stream->headerBytes > self.options.maxHeaderListBytes)
        {
            // 不 RST:头块读完后回 RESOURCE_EXHAUSTED(§15.5.3);超限之后的头不再存储。
            stream->headerListTooLarge = true;
            return 0;
        }

        const std::string_view name = AsView(nameData, nameLen);
        const std::string_view value = AsView(valueData, valueLen);
        if (!name.empty() && name.front() == ':')
        {
            if (name == ":method")
            {
                stream->method.assign(value.data(), value.size());
            }
            else if (name == ":path")
            {
                stream->headers.path.assign(value.data(), value.size());
            }
            return 0; // :scheme / :authority 不校验(§15.5.3)
        }
        if (name == "content-type")
        {
            stream->contentType.assign(value.data(), value.size());
            stream->contentTypeIsGrpc = stream->contentTypeIsGrpc || IsGrpcContentType(value);
            return 0;
        }
        if (name == "grpc-timeout")
        {
            stream->grpcTimeout.emplace(value);
            return 0;
        }
        if (name == "grpc-encoding")
        {
            stream->grpcEncoding.emplace(value);
            return 0;
        }
        // te 由 nghttp2 校验(只允许 trailers);user-agent 与其余 grpc- 头(含 grpc-accept-encoding)不进 metadata。
        if (name == "te" || name == "user-agent" || StartsWith(name, "grpc-"))
        {
            return 0;
        }
        if (IsBinaryHeaderKey(name))
        {
            auto decoded = DecodeBinaryHeaderValue(value);
            if (!decoded)
            {
                if (!stream->badBinaryKey)
                {
                    stream->badBinaryKey.emplace(name);
                }
                return 0;
            }
            stream->headers.metadata.emplace_back(std::string(name), std::move(*decoded));
            return 0;
        }
        stream->headers.metadata.emplace_back(std::string(name), std::string(value));
        return 0;
    }

    static int OnFrameRecv(nghttp2_session *, const nghttp2_frame *frame, void *userData)
    {
        auto &self = Self(userData);
        switch (frame->hd.type)
        {
        case NGHTTP2_SETTINGS:
            if ((frame->hd.flags & NGHTTP2_FLAG_ACK) == 0 && !self.handshakeComplete)
            {
                self.handshakeComplete = true;
                self.observer.OnClientSettings();
            }
            break;
        case NGHTTP2_HEADERS:
        {
            auto *stream = self.FindStream(frame->hd.stream_id);
            if (stream == nullptr)
            {
                break;
            }
            if (frame->headers.cat == NGHTTP2_HCAT_REQUEST)
            {
                self.EndOfRequestHeaders(frame->hd.stream_id, *stream);
            }
            if ((frame->hd.flags & NGHTTP2_FLAG_END_STREAM) != 0)
            {
                self.EndOfRequest(frame->hd.stream_id, *stream);
            }
            break;
        }
        case NGHTTP2_DATA:
            if ((frame->hd.flags & NGHTTP2_FLAG_END_STREAM) != 0)
            {
                if (auto *stream = self.FindStream(frame->hd.stream_id))
                {
                    self.EndOfRequest(frame->hd.stream_id, *stream);
                }
            }
            break;
        case NGHTTP2_PING:
            if ((frame->hd.flags & NGHTTP2_FLAG_ACK) != 0 && self.shutdownPingSubmitted && !self.shutdownPingAcked &&
                std::memcmp(frame->ping.opaque_data, kShutdownPingPayload, sizeof(kShutdownPingPayload)) == 0)
            {
                self.shutdownPingAcked = true;
                self.observer.OnShutdownPingAck();
            }
            break;
        default:
            break;
        }
        return 0;
    }

    static int OnDataChunkRecv(nghttp2_session *, uint8_t, int32_t streamId, const uint8_t *data, size_t len,
                               void *userData)
    {
        auto &self = Self(userData);
        if (auto *stream = self.FindStream(streamId))
        {
            self.OnRequestData(streamId, *stream, data, len);
        }
        return 0;
    }

    static int OnFrameSend(nghttp2_session *session, const nghttp2_frame *frame, void *userData)
    {
        auto &self = Self(userData);
        switch (frame->hd.type)
        {
        case NGHTTP2_HEADERS:
        case NGHTTP2_DATA:
            // 本端已带 END_STREAM 回完、对端请求还没收完(早拒):照 RFC 9113 §8.1 与 grpc-go writeEarlyAbort(rst=!eos)
            // 补一个 RST_STREAM(NO_ERROR)。此刻 nghttp2 还没把本端置为 half-closed,不能在这里提交,先登记。
            if ((frame->hd.flags & NGHTTP2_FLAG_END_STREAM) != 0 &&
                nghttp2_session_get_stream_remote_close(session, frame->hd.stream_id) == 0)
            {
                self.pendingNoErrorResets.push_back(frame->hd.stream_id);
            }
            break;
        case NGHTTP2_GOAWAY:
            // shutdown notice 已写出:现在才提交 PING。nghttp2 的 PING 走紧急队列、会排到同时提交的 GOAWAY 前面;
            // 照 grpc-go(先 GOAWAY 后 PING)必须让 PING 跟在 GOAWAY 之后,ACK 才能证明对端已处理过 GOAWAY
            // 之前发来的全部新流。同样不在回调里提交,先登记。
            if (frame->goaway.error_code == NGHTTP2_NO_ERROR &&
                frame->goaway.last_stream_id == kShutdownNoticeLastStreamId && self.shutdownNoticeSubmitted &&
                !self.shutdownPingSubmitted)
            {
                self.shutdownPingPending = true;
            }
            // 本层自己只发 NO_ERROR 的 GOAWAY;带错误码的只可能是 nghttp2 因对端违约发的。
            if (frame->goaway.error_code != NGHTTP2_NO_ERROR && !self.connectionError)
            {
                std::string message =
                    "http2 connection error: sent GOAWAY(error_code=" + std::to_string(frame->goaway.error_code) + ")";
                if (frame->goaway.opaque_data_len > 0)
                {
                    message += ' ';
                    message.append(AsView(frame->goaway.opaque_data, frame->goaway.opaque_data_len));
                }
                self.connectionError = Status(StatusCode::kUnavailable, std::move(message));
            }
            break;
        default:
            break;
        }
        return 0;
    }

    static int OnStreamClose(nghttp2_session *, int32_t streamId, uint32_t errorCode, void *userData)
    {
        auto &self = Self(userData);
        const auto it = self.streams.find(streamId);
        if (it == self.streams.end())
        {
            return 0;
        }
        const std::unique_ptr<StreamState> stream = std::move(it->second);
        self.streams.erase(it);
        self.DiscardRequestBody(*stream);
        if (stream->dispatched && !stream->responded)
        {
            self.observer.OnStreamAborted(streamId, errorCode);
        }
        return 0;
    }

    static int OnError(nghttp2_session *, int libErrorCode, const char *msg, size_t len, void *)
    {
        LOG_DEBUG << "nghttp2 server session error " << libErrorCode << ": " << std::string(msg, len);
        return 0;
    }

    // 成功应答的 DATA:「5 字节前缀 + 消息」;最后一块置 EOF | NO_END_STREAM 并在这里提交 trailers,
    // 这是 nghttp2 规定的 trailers 写法,保证 trailers 不会插到 DATA 前面(§15.5.5)。
    static nghttp2_ssize ReadResponseBody(nghttp2_session *session, int32_t streamId, uint8_t *buf, size_t length,
                                          uint32_t *dataFlags, nghttp2_data_source *, void *userData)
    {
        auto *stream = Self(userData).FindStream(streamId);
        if (stream == nullptr)
        {
            // 不可达:流关闭后 nghttp2 不会再取数据。不能以 EOF 结束(那会变成没有 trailers 的 END_STREAM)。
            return NGHTTP2_ERR_TEMPORAL_CALLBACK_FAILURE;
        }
        const std::string &body = stream->responseBody;
        const size_t chunk = std::min(body.size() - stream->responseOffset, length);
        if (chunk > 0)
        {
            std::memcpy(buf, body.data() + stream->responseOffset, chunk);
        }
        stream->responseOffset += chunk;
        if (stream->responseOffset == body.size())
        {
            *dataFlags |= NGHTTP2_DATA_FLAG_EOF | NGHTTP2_DATA_FLAG_NO_END_STREAM;
            // 成功时只发 grpc-status: 0,不发 grpc-message(§15.5.5)。
            const nghttp2_nv trailer = MakeNv("grpc-status", "0");
            const int rv = nghttp2_submit_trailer(session, streamId, &trailer, 1);
            if (rv != 0)
            {
                LOG_ERROR << "nghttp2_submit_trailer failed on stream " << streamId << ": " << nghttp2_strerror(rv);
                return NGHTTP2_ERR_TEMPORAL_CALLBACK_FAILURE; // nghttp2 以 RST_STREAM(INTERNAL_ERROR) 关流
            }
            std::string().swap(stream->responseBody);
            stream->responseOffset = 0;
        }
        return static_cast<nghttp2_ssize>(chunk);
    }
};

void ServerSession::SessionDeleter::operator()(nghttp2_session *session) const
{
    nghttp2_session_del(session);
}

ServerSession::ServerSession(ServerSessionOptions options, Observer &observer)
    : options(std::move(options)), observer(observer)
{
}

ServerSession::~ServerSession() = default;

Status ServerSession::Start()
{
    if (session)
    {
        return Status(StatusCode::kInternal, "http2 session already started");
    }
    nghttp2_session_callbacks *rawCallbacks = nullptr;
    if (nghttp2_session_callbacks_new(&rawCallbacks) != 0)
    {
        return Status(StatusCode::kResourceExhausted, "nghttp2_session_callbacks_new failed");
    }
    const std::unique_ptr<nghttp2_session_callbacks, void (*)(nghttp2_session_callbacks *)> callbacks(
        rawCallbacks, &nghttp2_session_callbacks_del);
    nghttp2_session_callbacks_set_on_begin_headers_callback(callbacks.get(), &ServerSessionCallbacks::OnBeginHeaders);
    nghttp2_session_callbacks_set_on_header_callback(callbacks.get(), &ServerSessionCallbacks::OnHeader);
    nghttp2_session_callbacks_set_on_frame_recv_callback(callbacks.get(), &ServerSessionCallbacks::OnFrameRecv);
    nghttp2_session_callbacks_set_on_frame_send_callback(callbacks.get(), &ServerSessionCallbacks::OnFrameSend);
    nghttp2_session_callbacks_set_on_data_chunk_recv_callback(callbacks.get(),
                                                              &ServerSessionCallbacks::OnDataChunkRecv);
    nghttp2_session_callbacks_set_on_stream_close_callback(callbacks.get(), &ServerSessionCallbacks::OnStreamClose);
    nghttp2_session_callbacks_set_error_callback2(callbacks.get(), &ServerSessionCallbacks::OnError);

    nghttp2_option *rawOption = nullptr;
    if (nghttp2_option_new(&rawOption) != 0)
    {
        return Status(StatusCode::kResourceExhausted, "nghttp2_option_new failed");
    }
    const std::unique_ptr<nghttp2_option, void (*)(nghttp2_option *)> option(rawOption, &nghttp2_option_del);
    // 防洪选项显式写出(§15.5.2);其余保持默认:自动回 PING / SETTINGS ACK、自动 WINDOW_UPDATE、HTTP 语义校验。
    nghttp2_option_set_max_outbound_ack(option.get(), kMaxOutboundAck);
    nghttp2_option_set_max_settings(option.get(), kMaxSettingsEntries);
    nghttp2_option_set_stream_reset_rate_limit(option.get(), kStreamResetBurst, kStreamResetRatePerSecond);
    nghttp2_option_set_max_continuations(option.get(), kMaxContinuations);
    nghttp2_option_set_glitch_rate_limit(option.get(), kGlitchBurst, kGlitchRatePerSecond);
    // 与 ClientSession 一致:关掉 RFC 9113 的首尾空白校验,不新增对 grpc-go metadata 的拒绝。
    nghttp2_option_set_no_rfc9113_leading_and_trailing_ws_validation(option.get(), 1);

    nghttp2_session *rawSession = nullptr;
    const int created = nghttp2_session_server_new2(&rawSession, callbacks.get(), this, option.get());
    if (created != 0)
    {
        return Status(StatusCode::kResourceExhausted,
                      std::string("nghttp2_session_server_new2 failed: ") + nghttp2_strerror(created));
    }
    session.reset(rawSession);

    // 不发 ENABLE_PUSH:服务端只能省略或发 0(§15.5.2)。
    const nghttp2_settings_entry settings[] = {
        {NGHTTP2_SETTINGS_MAX_CONCURRENT_STREAMS, options.maxConcurrentStreams},
        {NGHTTP2_SETTINGS_INITIAL_WINDOW_SIZE, options.initialStreamWindowBytes},
        {NGHTTP2_SETTINGS_MAX_HEADER_LIST_SIZE, options.maxHeaderListBytes},
    };
    int rv = nghttp2_submit_settings(session.get(), NGHTTP2_FLAG_NONE, settings, std::size(settings));
    if (rv != 0)
    {
        return Status(StatusCode::kInternal, std::string("nghttp2_submit_settings failed: ") + nghttp2_strerror(rv));
    }
    // 连接级接收窗口默认 65535,放大以免多个并发请求互相卡流控。
    rv = nghttp2_session_set_local_window_size(session.get(), NGHTTP2_FLAG_NONE, 0,
                                               static_cast<int32_t>(options.connectionWindowBytes));
    if (rv != 0)
    {
        return Status(StatusCode::kInternal,
                      std::string("nghttp2_session_set_local_window_size failed: ") + nghttp2_strerror(rv));
    }
    return Status::Ok();
}

Status ServerSession::Receive(const uint8_t *data, size_t len)
{
    if (!session)
    {
        return Status(StatusCode::kInternal, "http2 session not started");
    }
    if (fatalError)
    {
        return *fatalError;
    }
    if (len == 0)
    {
        return Status::Ok();
    }
    receivedAnyBytes = true;
    const nghttp2_ssize consumed = nghttp2_session_mem_recv2(session.get(), data, len);
    if (consumed < 0)
    {
        const int code = static_cast<int>(consumed);
        fatalError = code == NGHTTP2_ERR_BAD_CLIENT_MAGIC
                          ? Status(StatusCode::kUnavailable, "invalid HTTP/2 client preface")
                          : Status(StatusCode::kUnavailable, std::string("http2 receive failed: ") + nghttp2_strerror(code));
        return *fatalError;
    }
    return Status::Ok();
}

Status ServerSession::DrainOutput(const std::function<bool(std::string_view chunk)> &write)
{
    if (!session)
    {
        return Status(StatusCode::kInternal, "http2 session not started");
    }
    if (fatalError || !receivedAnyBytes)
    {
        return Status::Ok();
    }
    const bool hadConnectionError = connectionError.has_value();
    for (;;)
    {
        SubmitDeferredFrames();
        const uint8_t *data = nullptr;
        const nghttp2_ssize produced = nghttp2_session_mem_send2(session.get(), &data);
        if (produced < 0)
        {
            fatalError = Status(StatusCode::kUnavailable,
                                 std::string("http2 send failed: ") + nghttp2_strerror(static_cast<int>(produced)));
            return *fatalError;
        }
        if (produced == 0)
        {
            if (pendingNoErrorResets.empty() && !shutdownPingPending)
            {
                break;
            }
            continue;
        }
        if (!write(AsView(data, static_cast<size_t>(produced))))
        {
            SubmitDeferredFrames(); // 留给下一次 DrainOutput 写出
            break;
        }
    }
    if (!hadConnectionError && connectionError)
    {
        return *connectionError;
    }
    return Status::Ok();
}

bool ServerSession::SubmitResponse(int32_t streamId, std::string message)
{
    auto *stream = FindStream(streamId);
    if (stream == nullptr || stream->responded || !session ||
        static_cast<uint64_t>(message.size()) > std::numeric_limits<uint32_t>::max())
    {
        return false;
    }
    stream->responded = true;
    DiscardRequestBody(*stream);
    stream->responseBody.clear();
    AppendLengthPrefixedMessage(stream->responseBody, message);
    stream->responseOffset = 0;

    const nghttp2_nv headers[] = {MakeNv(":status", "200"), MakeNv("content-type", kGrpcContentType)};
    nghttp2_data_provider2 provider;
    provider.source.ptr = nullptr;
    provider.read_callback = &ServerSessionCallbacks::ReadResponseBody;
    const int rv = nghttp2_submit_response2(session.get(), streamId, headers, std::size(headers), &provider);
    if (rv != 0)
    {
        LOG_ERROR << "nghttp2_submit_response2 failed on stream " << streamId << ": " << nghttp2_strerror(rv);
        std::string().swap(stream->responseBody);
        nghttp2_submit_rst_stream(session.get(), NGHTTP2_FLAG_NONE, streamId, NGHTTP2_INTERNAL_ERROR);
        return false;
    }
    return true;
}

bool ServerSession::SubmitStatus(int32_t streamId, const Status &status)
{
    if (status.ok())
    {
        LOG_ERROR << "ServerSession::SubmitStatus called with an OK status on stream " << streamId;
        return false;
    }
    auto *stream = FindStream(streamId);
    if (stream == nullptr || stream->responded || !session)
    {
        return false;
    }
    stream->responded = true;
    DiscardRequestBody(*stream);
    return SubmitTrailersOnly(streamId, kHttpOk, status, false); // 上层自己的完成,不经 OnRequestRejected 回报
}

bool ServerSession::RefuseStream(int32_t streamId)
{
    auto *stream = FindStream(streamId);
    if (stream == nullptr || stream->responded || !session)
    {
        return false;
    }
    stream->responded = true;
    DiscardRequestBody(*stream);
    return nghttp2_submit_rst_stream(session.get(), NGHTTP2_FLAG_NONE, streamId, NGHTTP2_REFUSED_STREAM) == 0;
}

void ServerSession::SubmitShutdownNotice()
{
    if (!session || shutdownNoticeSubmitted || finalGoawaySubmitted)
    {
        return;
    }
    shutdownNoticeSubmitted = true;
    // 必须先于任何 GOAWAY(nghttp2 在已有 GOAWAY 时静默忽略 shutdown notice,这时也就不会有 PING,
    // 由上层的 1s 计时兜底)。不置 GOAWAY_SENT,新流照常收。PING 等 GOAWAY 写出后再提交(见 OnFrameSend)。
    const int rv = nghttp2_submit_shutdown_notice(session.get());
    if (rv != 0)
    {
        LOG_ERROR << "nghttp2_submit_shutdown_notice failed: " << nghttp2_strerror(rv);
    }
}

void ServerSession::SubmitFinalGoaway()
{
    if (!session || finalGoawaySubmitted)
    {
        return;
    }
    finalGoawaySubmitted = true;
    const int32_t lastStreamId = nghttp2_session_get_last_proc_stream_id(session.get());
    const int rv = nghttp2_submit_goaway(session.get(), NGHTTP2_FLAG_NONE, lastStreamId, NGHTTP2_NO_ERROR, nullptr, 0);
    if (rv != 0)
    {
        LOG_ERROR << "nghttp2_submit_goaway failed: " << nghttp2_strerror(rv);
        return; // 没有 GOAWAY 会去关后来的新流,它们照常处理
    }
    // nghttp2 只在 GOAWAY **写出**之后才忽略新流;从现在到写出之间到达的新流在写出时会被按 last_stream_id
    // 关掉(REFUSED_STREAM),所以从这一刻起就不能再交给上层(见头文件 SubmitFinalGoaway)。
    finalGoawayLastStreamId = lastStreamId;
}

bool ServerSession::ShouldClose() const
{
    if (!session || fatalError)
    {
        return true;
    }
    return nghttp2_session_want_read(session.get()) == 0 && nghttp2_session_want_write(session.get()) == 0;
}

std::vector<int32_t> ServerSession::AcceptedUndispatchedStreams() const
{
    std::vector<int32_t> streamIds;
    for (const auto &[streamId, stream] : streams)
    {
        if (stream->headersAccepted && !stream->dispatched && !stream->responded)
        {
            streamIds.push_back(streamId);
        }
    }
    std::sort(streamIds.begin(), streamIds.end()); // streams_ 是无序表:按 id 排,拒绝顺序与线上帧序确定
    return streamIds;
}

ServerSession::StreamState *ServerSession::FindStream(int32_t streamId)
{
    const auto it = streams.find(streamId);
    return it == streams.end() ? nullptr : it->second.get();
}

void ServerSession::EndOfRequestHeaders(int32_t streamId, StreamState &stream)
{
    stream.inRequestHeaders = false;
    if (stream.responded)
    {
        return;
    }
    if (streamId > finalGoawayLastStreamId)
    {
        // 最终 GOAWAY 已提交、还没写出时才到的新流:写出时 nghttp2 会把它以 REFUSED_STREAM 关掉,客户端判为
        // 未处理并透明重试。不回应、不问 Observer、不计拒绝;标记已应答,之后的 DATA / END_STREAM 一律丢弃。
        stream.responded = true;
        DiscardRequestBody(stream);
        return;
    }
    // §15.5.3 的顺序;全部在业务之前。
    if (stream.headerListTooLarge)
    {
        RejectStream(streamId, stream, kHttpOk,
                     Status(StatusCode::kResourceExhausted, "received header list larger than max (" +
                                                                std::to_string(options.maxHeaderListBytes) + ")"));
        return;
    }
    if (stream.method != "POST")
    {
        RejectStream(streamId, stream, kHttpMethodNotAllowed, Status(StatusCode::kInternal, "method must be POST"));
        return;
    }
    if (!stream.contentTypeIsGrpc)
    {
        RejectStream(streamId, stream, kHttpUnsupportedMediaType,
                     Status(StatusCode::kInvalidArgument,
                            "invalid gRPC request content-type " + Quoted(stream.contentType)));
        return;
    }
    if (stream.grpcTimeout)
    {
        const auto timeout = ParseGrpcTimeout(*stream.grpcTimeout);
        if (!timeout)
        {
            RejectStream(streamId, stream, kHttpBadRequest, Status(StatusCode::kInternal, "malformed grpc-timeout"));
            return;
        }
        stream.headers.timeout = *timeout;
    }
    if (stream.badBinaryKey)
    {
        RejectStream(streamId, stream, kHttpBadRequest,
                     Status(StatusCode::kInternal, "malformed binary metadata " + Quoted(*stream.badBinaryKey)));
        return;
    }
    if (stream.grpcEncoding && *stream.grpcEncoding != "identity")
    {
        RejectStream(streamId, stream, kHttpOk,
                     Status(StatusCode::kUnimplemented, "unsupported grpc-encoding " + Quoted(*stream.grpcEncoding)),
                     true);
        return;
    }
    if (!SplitMethodPath(stream.headers.path))
    {
        RejectStream(streamId, stream, kHttpOk,
                     Status(StatusCode::kUnimplemented, "malformed method name: " + Quoted(stream.headers.path)));
        return;
    }
    Verdict verdict = observer.OnRequestHeaders(streamId, stream.headers);
    if (verdict.kind == Verdict::Kind::kAccept)
    {
        stream.headersAccepted = true;
        return;
    }
    ApplyVerdict(streamId, stream, std::move(verdict));
}

void ServerSession::EndOfRequest(int32_t streamId, StreamState &stream)
{
    if (!stream.headersAccepted || stream.responded)
    {
        return;
    }
    if (stream.messages.size() != 1 || stream.deframer.HasPartialMessage())
    {
        RejectStream(streamId, stream, kHttpOk,
                     Status(StatusCode::kInternal, "unary request must contain exactly one message"));
        return;
    }
    if (stream.headers.timeout && stream.headers.timeout->count() <= 0)
    {
        RejectStream(streamId, stream, kHttpOk,
                     Status(StatusCode::kDeadlineExceeded, "deadline exceeded before dispatch"));
        return;
    }
    std::string message = std::move(stream.messages.front());
    DiscardRequestBody(stream); // 请求体已交出:不再计入「未收完请求体之和」
    Verdict verdict = observer.OnRequestComplete(streamId, std::move(stream.headers), std::move(message));
    if (verdict.kind == Verdict::Kind::kAccept)
    {
        stream.dispatched = true;
        return;
    }
    ApplyVerdict(streamId, stream, std::move(verdict));
}

void ServerSession::OnRequestData(int32_t streamId, StreamState &stream, const uint8_t *data, size_t len)
{
    if (!stream.headersAccepted || stream.responded || len == 0)
    {
        return; // 头部没通过或已应答:数据只计入流控,直接丢弃
    }
    // 不变量 bufferedRequestBytes_ <= maxBufferedRequestBytes,减法不会下溢。
    if (len > options.maxBufferedRequestBytes - bufferedRequestBytes)
    {
        RejectStream(streamId, stream, kHttpOk,
                     Status(StatusCode::kResourceExhausted, "request buffer limit exceeded"));
        return;
    }
    bufferedRequestBytes += len;
    stream.bufferedBytes += len;
    Status status = stream.deframer.Feed(data, len, stream.messages);
    if (!status.ok())
    {
        // 压缩标志非法、或声明长度超过上限:立即回,不等请求收完(§15.5.3)。
        RejectStream(streamId, stream, kHttpOk, status);
        return;
    }
    if (stream.messages.size() > 1)
    {
        RejectStream(streamId, stream, kHttpOk,
                     Status(StatusCode::kInternal, "unary request must contain exactly one message"));
    }
}

void ServerSession::ApplyVerdict(int32_t streamId, StreamState &stream, Verdict verdict)
{
    switch (verdict.kind)
    {
    case Verdict::Kind::kAccept:
        return;
    case Verdict::Kind::kRefuse:
        RefuseStream(streamId);
        return;
    case Verdict::Kind::kReject:
        if (verdict.status.ok())
        {
            LOG_ERROR << "ServerSession: observer rejected stream " << streamId << " with an OK status";
            verdict.status = Status(StatusCode::kInternal, "server rejected the request without a status");
        }
        // Observer 自己的决定,不再经 OnRequestRejected 回报。
        FinishStreamWithStatus(streamId, stream, kHttpOk, verdict.status, false);
        return;
    }
}

void ServerSession::RejectStream(int32_t streamId, StreamState &stream, int httpStatus, const Status &status,
                                 bool advertiseIdentityEncoding)
{
    observer.OnRequestRejected(streamId, httpStatus, status);
    FinishStreamWithStatus(streamId, stream, httpStatus, status, advertiseIdentityEncoding);
}

void ServerSession::FinishStreamWithStatus(int32_t streamId, StreamState &stream, int httpStatus,
                                           const Status &status, bool advertiseIdentityEncoding)
{
    stream.responded = true;
    DiscardRequestBody(stream);
    SubmitTrailersOnly(streamId, httpStatus, status, advertiseIdentityEncoding);
}

bool ServerSession::SubmitTrailersOnly(int32_t streamId, int httpStatus, const Status &status,
                                       bool advertiseIdentityEncoding)
{
    // 单个 HEADERS 带 END_STREAM::status、content-type、grpc-status、非空时的 grpc-message(百分号编码)。
    const std::string httpStatusText = std::to_string(httpStatus);
    const std::string grpcStatusText = std::to_string(status.code_value());
    const std::string grpcMessage = PercentEncodeGrpcMessage(status.message());
    std::vector<nghttp2_nv> nva;
    nva.reserve(5);
    nva.push_back(MakeNv(":status", httpStatusText));
    nva.push_back(MakeNv("content-type", kGrpcContentType));
    nva.push_back(MakeNv("grpc-status", grpcStatusText));
    if (!grpcMessage.empty())
    {
        nva.push_back(MakeNv("grpc-message", grpcMessage));
    }
    if (advertiseIdentityEncoding)
    {
        nva.push_back(MakeNv("grpc-accept-encoding", "identity"));
    }
    const int rv = nghttp2_submit_response2(session.get(), streamId, nva.data(), nva.size(), nullptr);
    if (rv != 0)
    {
        LOG_ERROR << "nghttp2_submit_response2 (trailers-only) failed on stream " << streamId << ": "
                  << nghttp2_strerror(rv);
        nghttp2_submit_rst_stream(session.get(), NGHTTP2_FLAG_NONE, streamId, NGHTTP2_INTERNAL_ERROR);
        return false;
    }
    return true;
}

void ServerSession::DiscardRequestBody(StreamState &stream)
{
    bufferedRequestBytes -= stream.bufferedBytes;
    stream.bufferedBytes = 0;
    stream.messages.clear();
    stream.deframer = MessageDeframer(options.maxReceiveMessageBytes);
}

void ServerSession::SubmitDeferredFrames()
{
    if (shutdownPingPending)
    {
        shutdownPingPending = false;
        shutdownPingSubmitted = true;
        const int rv = nghttp2_submit_ping(session.get(), NGHTTP2_FLAG_NONE, kShutdownPingPayload);
        if (rv != 0)
        {
            LOG_ERROR << "nghttp2_submit_ping failed: " << nghttp2_strerror(rv);
        }
    }
    if (pendingNoErrorResets.empty())
    {
        return;
    }
    std::vector<int32_t> pending;
    pending.swap(pendingNoErrorResets);
    for (const int32_t streamId : pending)
    {
        // 本端已关、对端还开着才补;期间流若已关闭(对端随后也结束了),两个查询都会落空。
        if (nghttp2_session_get_stream_local_close(session.get(), streamId) == 1 &&
            nghttp2_session_get_stream_remote_close(session.get(), streamId) == 0)
        {
            nghttp2_submit_rst_stream(session.get(), NGHTTP2_FLAG_NONE, streamId, NGHTTP2_NO_ERROR);
        }
    }
}

} // namespace muduo_grpc
