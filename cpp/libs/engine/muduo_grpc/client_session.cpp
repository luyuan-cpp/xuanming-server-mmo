#include "client_session.h"

#include <nghttp2/nghttp2.h>

#include <algorithm>
#include <cstring>
#include <iterator>

#include "muduo/base/Logging.h"

namespace muduo_grpc
{

namespace
{

constexpr std::string_view kGrpcContentType = "application/grpc";

// RFC 9113 §6.5.2:头部列表大小按「名 + 值 + 32」累计。
constexpr size_t kHeaderEntryOverheadBytes = 32;

// 存活探测 PING 的 8 字节负载,只用于日志里辨认;ACK 的匹配不依赖它(同一时刻至多一个探测在途)。
constexpr uint8_t kProbePingPayload[8] = {'m', 'u', 'd', 'u', 'o', 'g', 'r', 'p'};

// 双向流待发缓冲里已交给 nghttp2 的前缀超过这个量、且占缓冲一半以上时挪掉(见 ReadRequestBody)。
constexpr size_t kPendingSendCompactBytes = 64 * 1024;

std::string_view AsView(const uint8_t *data, size_t len)
{
    return std::string_view(reinterpret_cast<const char *>(data), len);
}

// application/grpc 本身,或其后紧跟 '+'(application/grpc+proto)或 ';'(参数)。
bool IsGrpcContentType(std::string_view value)
{
    if (value.size() < kGrpcContentType.size() || value.substr(0, kGrpcContentType.size()) != kGrpcContentType)
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

std::optional<int> ParseHttpStatus(std::string_view value)
{
    if (value.size() != 3)
    {
        return std::nullopt;
    }
    int status = 0;
    for (const char c : value)
    {
        if (c < '0' || c > '9')
        {
            return std::nullopt;
        }
        status = status * 10 + (c - '0');
    }
    return status;
}

nghttp2_nv MakeNv(const std::string &name, const std::string &value)
{
    nghttp2_nv nv;
    nv.name = reinterpret_cast<uint8_t *>(const_cast<char *>(name.data()));
    nv.value = reinterpret_cast<uint8_t *>(const_cast<char *>(value.data()));
    nv.namelen = name.size();
    nv.valuelen = value.size();
    nv.flags = NGHTTP2_NV_FLAG_NONE; // nghttp2 自己拷贝名与值,调用返回后 name / value 即可释放
    return nv;
}

} // namespace

// nghttp2 回调。全部只在 ClientSession 的方法调用栈里被 nghttp2 调用(mem_recv2 / mem_send2 / submit_*)。
struct ClientSessionCallbacks
{
    using Block = ClientSession::StreamState::Block;

    static ClientSession &Self(void *userData) { return *static_cast<ClientSession *>(userData); }

    static int OnBeginHeaders(nghttp2_session *, const nghttp2_frame *frame, void *userData)
    {
        if (frame->hd.type != NGHTTP2_HEADERS)
        {
            return 0;
        }
        auto *stream = Self(userData).FindStream(frame->hd.stream_id);
        if (stream == nullptr)
        {
            return 0;
        }
        const bool endStream = (frame->hd.flags & NGHTTP2_FLAG_END_STREAM) != 0;
        stream->blockBytes = 0;
        // 首个 HEADERS 是响应头;1xx 之后的最终响应头 nghttp2 归到 HCAT_HEADERS,用「还没收到响应头」区分。
        const bool isResponseBlock =
            frame->headers.cat == NGHTTP2_HCAT_RESPONSE || !stream->responseHeadersReceived;
        if (isResponseBlock)
        {
            stream->block = endStream ? Block::kTrailersOnly : Block::kResponseHeaders;
        }
        else
        {
            stream->block = Block::kTrailers;
        }
        return 0;
    }

    static int OnHeader(nghttp2_session *, const nghttp2_frame *frame, const uint8_t *nameData, size_t nameLen,
                        const uint8_t *valueData, size_t valueLen, uint8_t, void *userData)
    {
        if (frame->hd.type != NGHTTP2_HEADERS)
        {
            return 0;
        }
        auto &self = Self(userData);
        const int32_t streamId = frame->hd.stream_id;
        auto *stream = self.FindStream(streamId);
        if (stream == nullptr || stream->block == Block::kNone)
        {
            return 0;
        }

        stream->blockBytes += nameLen + valueLen + kHeaderEntryOverheadBytes;
        if (stream->blockBytes > self.options.maxHeaderListBytes)
        {
            if (!stream->localStatus)
            {
                stream->localStatus = Status(StatusCode::kResourceExhausted,
                                             "received header list larger than max " +
                                                 std::to_string(self.options.maxHeaderListBytes) + " bytes");
            }
            // nghttp2 以 RST_STREAM(INTERNAL_ERROR) 关流;本块剩余头部不再回调。
            return NGHTTP2_ERR_TEMPORAL_CALLBACK_FAILURE;
        }

        const std::string_view name = AsView(nameData, nameLen);
        const std::string_view value = AsView(valueData, valueLen);

        if (name == ":status")
        {
            const auto status = ParseHttpStatus(value);
            stream->httpStatus = status.value_or(0);
            return 0;
        }
        if (!name.empty() && name.front() == ':')
        {
            return 0; // 其他伪头与 gRPC 无关
        }
        if (stream->httpStatus >= 100 && stream->httpStatus < 200)
        {
            return 0; // 1xx 非最终响应:其中的头部不是 gRPC metadata
        }
        if (name == "content-type")
        {
            if (stream->block != Block::kTrailers)
            {
                stream->contentTypeIsGrpc = IsGrpcContentType(value);
            }
            return 0;
        }
        if (name == "grpc-status")
        {
            stream->grpcStatusPresent = true;
            stream->grpcStatus = ParseGrpcStatusValue(value);
            return 0;
        }
        if (name == "grpc-message")
        {
            stream->grpcMessage = PercentDecodeGrpcMessage(value);
            return 0;
        }
        if (name == "grpc-encoding" || name == "grpc-accept-encoding")
        {
            return 0; // 压缩由消息前缀的标志位判定(收到压缩消息即违约,见 MessageDeframer)
        }

        Metadata &target = stream->block == Block::kResponseHeaders ? stream->initialMetadata : stream->trailingMetadata;
        if (IsBinaryHeaderKey(name))
        {
            auto decoded = DecodeBinaryHeaderValue(value);
            if (!decoded)
            {
                if (!stream->localStatus)
                {
                    stream->localStatus = Status(StatusCode::kInternal,
                                                 "invalid base64 in binary metadata '" + std::string(name) + "'");
                }
                return NGHTTP2_ERR_TEMPORAL_CALLBACK_FAILURE;
            }
            target.emplace_back(std::string(name), std::move(*decoded));
        }
        else
        {
            target.emplace_back(std::string(name), std::string(value));
        }
        return 0;
    }

    static int OnFrameRecv(nghttp2_session *session, const nghttp2_frame *frame, void *userData)
    {
        const int result = HandleFrameRecv(frame, userData);
        CloseStreamEndedByPeer(session, frame, Self(userData));
        return result;
    }

    // 双向流:对端已经带 END_STREAM 结束了流(trailers / trailers-only / 非 gRPC 响应的最后一个 DATA),而本端方向还开着
    // (DATA 挂在 DEFERRED、调用方还没 WritesDone)。nghttp2 只把流置为 half-closed(remote),两端都关之前不回调流关闭,
    // 上层就永远收不到 onClose。照 grpc-go 客户端 closeStream(rst=true) 的做法补一个 RST_STREAM(NO_ERROR):
    // 流随即关闭,BuildOutcome 按已收到的 trailers(或 HTTP 状态)给出结果。grpc-go 服务端自己会在 trailers 后补 RST,
    // 这里是为了不依赖服务端的这个行为(代理、其他实现)。
    static void CloseStreamEndedByPeer(nghttp2_session *session, const nghttp2_frame *frame, ClientSession &self)
    {
        if ((frame->hd.type != NGHTTP2_HEADERS && frame->hd.type != NGHTTP2_DATA) ||
            (frame->hd.flags & NGHTTP2_FLAG_END_STREAM) == 0)
        {
            return;
        }
        auto *stream = self.FindStream(frame->hd.stream_id);
        if (stream == nullptr || !stream->streaming || stream->remoteEnded || stream->localStatus)
        {
            return; // localStatus:本端已经发过 RST(截止 / 拆包错误 / 超限),不再补发
        }
        stream->remoteEnded = true;
        if (nghttp2_session_get_stream_local_close(session, frame->hd.stream_id) == 0)
        {
            nghttp2_submit_rst_stream(session, NGHTTP2_FLAG_NONE, frame->hd.stream_id, NGHTTP2_NO_ERROR);
        }
    }

    static int HandleFrameRecv(const nghttp2_frame *frame, void *userData)
    {
        auto &self = Self(userData);
        self.observer.OnFrameReceived(frame->hd.type);
        switch (frame->hd.type)
        {
        case NGHTTP2_SETTINGS:
            if ((frame->hd.flags & NGHTTP2_FLAG_ACK) == 0 && !self.readySignaled)
            {
                self.readySignaled = true;
                self.observer.OnSessionReady();
            }
            break;
        case NGHTTP2_HEADERS:
        {
            auto *stream = self.FindStream(frame->hd.stream_id);
            if (stream == nullptr)
            {
                break;
            }
            const bool informational = stream->httpStatus >= 100 && stream->httpStatus < 200;
            switch (stream->block)
            {
            case Block::kResponseHeaders:
                if (!informational)
                {
                    stream->responseHeadersReceived = true;
                    // 只对合法 gRPC 响应头回调 onHeaders;代理回的 503 等由流结束时的状态映射报告(§4.3)。
                    if (stream->streaming && !stream->localStatus && stream->httpStatus == 200 &&
                        stream->contentTypeIsGrpc)
                    {
                        self.observer.OnStreamHeaders(frame->hd.stream_id, stream->initialMetadata);
                    }
                }
                break;
            case Block::kTrailersOnly:
                stream->responseHeadersReceived = true;
                stream->trailersReceived = true;
                break;
            case Block::kTrailers:
                stream->trailersReceived = true;
                break;
            case Block::kNone:
                break;
            }
            if (informational && stream->block == Block::kResponseHeaders)
            {
                stream->httpStatus = 0; // 等最终响应头重新给出
            }
            stream->block = Block::kNone;
            break;
        }
        case NGHTTP2_PING:
            if ((frame->hd.flags & NGHTTP2_FLAG_ACK) != 0)
            {
                self.observer.OnPingAck();
            }
            break;
        case NGHTTP2_GOAWAY:
            self.goawayReceived = true;
            self.observer.OnGoawayReceived(frame->goaway.last_stream_id, frame->goaway.error_code,
                                            AsView(frame->goaway.opaque_data, frame->goaway.opaque_data_len));
            break;
        default:
            break;
        }
        return 0;
    }

    static int OnFrameSend(nghttp2_session *, const nghttp2_frame *frame, void *userData)
    {
        if (frame->hd.type == NGHTTP2_HEADERS && frame->headers.cat == NGHTTP2_HCAT_REQUEST)
        {
            if (auto *stream = Self(userData).FindStream(frame->hd.stream_id))
            {
                stream->requestHeadersSent = true;
            }
        }
        return 0;
    }

    static int OnDataChunkRecv(nghttp2_session *, uint8_t, int32_t streamId, const uint8_t *data, size_t len,
                               void *userData)
    {
        auto &self = Self(userData);
        auto *stream = self.FindStream(streamId);
        if (stream == nullptr || stream->localStatus)
        {
            return 0; // 本端已经放弃这个流(截止 / 超限):数据只计入流控,直接丢弃
        }
        if (stream->httpStatus != 200 || !stream->contentTypeIsGrpc)
        {
            // 不是 gRPC 响应(如代理回的 503 + text/html):响应体不是长度前缀消息,不拆;
            // 结果按 HTTP 状态合成(§4.3),与 grpc-go 一致。
            return 0;
        }
        Status status = stream->deframer.Feed(data, len, stream->messages);
        if (stream->streaming)
        {
            // 双向流:每条完整消息立即交出去,不在会话里攒(长流上消息数没有上限)。拆包出错时也先交付这个 DATA 块里
            // 违约之前已经完整的消息,再按本地错误结束流。
            for (auto &message : stream->messages)
            {
                self.observer.OnStreamMessage(streamId, std::move(message));
            }
            stream->messages.clear();
            if (!status.ok())
            {
                self.SetLocalErrorAndReset(streamId, *stream, std::move(status), NGHTTP2_CANCEL);
            }
            return 0;
        }
        if (!status.ok())
        {
            self.SetLocalErrorAndReset(streamId, *stream, std::move(status), NGHTTP2_CANCEL);
            return 0;
        }
        if (stream->messages.size() > 1)
        {
            self.SetLocalErrorAndReset(
                streamId, *stream,
                Status(StatusCode::kInternal, "received more than one response message for a unary call"),
                NGHTTP2_CANCEL);
        }
        return 0;
    }

    static int OnStreamClose(nghttp2_session *, int32_t streamId, uint32_t errorCode, void *userData)
    {
        auto &self = Self(userData);
        auto it = self.streams.find(streamId);
        if (it == self.streams.end())
        {
            return 0;
        }
        std::unique_ptr<ClientSession::StreamState> stream = std::move(it->second);
        self.streams.erase(it);
        StreamOutcome outcome = self.BuildOutcome(*stream, errorCode);
        self.observer.OnStreamClosed(streamId, std::move(outcome));
        return 0;
    }

    static int OnError(nghttp2_session *, int libErrorCode, const char *msg, size_t len, void *)
    {
        LOG_DEBUG << "nghttp2 client session error " << libErrorCode << ": " << std::string(msg, len);
        return 0;
    }

    static nghttp2_ssize ReadRequestBody(nghttp2_session *, int32_t streamId, uint8_t *buf, size_t length,
                                         uint32_t *dataFlags, nghttp2_data_source *, void *userData)
    {
        auto *stream = Self(userData).FindStream(streamId);
        if (stream != nullptr && stream->streaming)
        {
            const size_t available = stream->pendingSend.size() - stream->pendingSendOffset;
            if (available == 0)
            {
                if (stream->writesDone)
                {
                    *dataFlags |= NGHTTP2_DATA_FLAG_EOF; // 已半关闭且发完:END_STREAM
                    return 0;
                }
                // 暂时没有数据:挂起,AppendStreamMessage / HalfCloseStream 里 resume。
                stream->dataDeferred = true;
                return NGHTTP2_ERR_DEFERRED;
            }
            const size_t chunk = std::min(available, length);
            std::memcpy(buf, stream->pendingSend.data() + stream->pendingSendOffset, chunk);
            stream->pendingSendOffset += chunk;
            if (stream->pendingSendOffset == stream->pendingSend.size())
            {
                stream->pendingSend.clear();
                stream->pendingSendOffset = 0;
                if (stream->writesDone)
                {
                    *dataFlags |= NGHTTP2_DATA_FLAG_EOF;
                }
            }
            else if (stream->pendingSendOffset >= kPendingSendCompactBytes &&
                     stream->pendingSendOffset * 2 >= stream->pendingSend.size())
            {
                // 流控窗口成为瓶颈、写方持续追加时缓冲一直取不空:已交给 nghttp2 的前缀要及时丢掉,
                // 否则内存随累计发送量增长(待发上限只约束未发部分)。前缀占一半以上才挪,摊还 O(1)。
                stream->pendingSend.erase(0, stream->pendingSendOffset);
                stream->pendingSendOffset = 0;
            }
            return static_cast<nghttp2_ssize>(chunk);
        }
        if (stream == nullptr || !stream->framedRequest)
        {
            *dataFlags |= NGHTTP2_DATA_FLAG_EOF;
            return 0;
        }
        const std::string &body = *stream->framedRequest;
        const size_t remaining = body.size() - stream->sentOffset;
        const size_t chunk = std::min(remaining, length);
        if (chunk > 0)
        {
            std::memcpy(buf, body.data() + stream->sentOffset, chunk);
        }
        stream->sentOffset += chunk;
        if (stream->sentOffset == body.size())
        {
            *dataFlags |= NGHTTP2_DATA_FLAG_EOF; // 请求体发完即 END_STREAM(unary 半关闭)
        }
        return static_cast<nghttp2_ssize>(chunk);
    }
};

void ClientSession::SessionDeleter::operator()(nghttp2_session *session) const
{
    nghttp2_session_del(session);
}

ClientSession::ClientSession(ClientSessionOptions options, Observer &observer)
    : options(std::move(options)), observer(observer)
{
}

ClientSession::~ClientSession() = default;

Status ClientSession::Start()
{
    nghttp2_session_callbacks *rawCallbacks = nullptr;
    if (nghttp2_session_callbacks_new(&rawCallbacks) != 0)
    {
        return Status(StatusCode::kResourceExhausted, "nghttp2_session_callbacks_new failed");
    }
    const std::unique_ptr<nghttp2_session_callbacks, void (*)(nghttp2_session_callbacks *)> callbacks(
        rawCallbacks, &nghttp2_session_callbacks_del);
    nghttp2_session_callbacks_set_on_begin_headers_callback(callbacks.get(), &ClientSessionCallbacks::OnBeginHeaders);
    nghttp2_session_callbacks_set_on_header_callback(callbacks.get(), &ClientSessionCallbacks::OnHeader);
    nghttp2_session_callbacks_set_on_frame_recv_callback(callbacks.get(), &ClientSessionCallbacks::OnFrameRecv);
    nghttp2_session_callbacks_set_on_frame_send_callback(callbacks.get(), &ClientSessionCallbacks::OnFrameSend);
    nghttp2_session_callbacks_set_on_data_chunk_recv_callback(callbacks.get(),
                                                              &ClientSessionCallbacks::OnDataChunkRecv);
    nghttp2_session_callbacks_set_on_stream_close_callback(callbacks.get(), &ClientSessionCallbacks::OnStreamClose);
    nghttp2_session_callbacks_set_error_callback2(callbacks.get(), &ClientSessionCallbacks::OnError);

    nghttp2_option *rawOption = nullptr;
    if (nghttp2_option_new(&rawOption) != 0)
    {
        return Status(StatusCode::kResourceExhausted, "nghttp2_option_new failed");
    }
    const std::unique_ptr<nghttp2_option, void (*)(nghttp2_option *)> option(rawOption, &nghttp2_option_del);
    // 其余保持默认:自动回 PING ACK、自动 WINDOW_UPDATE、开启 HTTP 语义校验(响应必须有 :status 等)。
    // 关掉 RFC 9113 的首尾空白校验:grpc-message 的空格不做百分号编码,grpc-go 发出 "error: " 这类
    // 以空格结尾的消息时,nghttp2 默认会把这个头当非法字段丢掉,状态说明就没了(状态码不受影响)。
    nghttp2_option_set_no_rfc9113_leading_and_trailing_ws_validation(option.get(), 1);

    nghttp2_session *rawSession = nullptr;
    const int created = nghttp2_session_client_new2(&rawSession, callbacks.get(), this, option.get());
    if (created != 0)
    {
        return Status(StatusCode::kResourceExhausted,
                      std::string("nghttp2_session_client_new2 failed: ") + nghttp2_strerror(created));
    }
    session.reset(rawSession);

    const nghttp2_settings_entry settings[] = {
        {NGHTTP2_SETTINGS_ENABLE_PUSH, 0},
        {NGHTTP2_SETTINGS_INITIAL_WINDOW_SIZE, options.initialStreamWindowBytes},
        {NGHTTP2_SETTINGS_MAX_HEADER_LIST_SIZE, options.maxHeaderListBytes},
    };
    int rv = nghttp2_submit_settings(session.get(), NGHTTP2_FLAG_NONE, settings, std::size(settings));
    if (rv != 0)
    {
        return Status(StatusCode::kInternal, std::string("nghttp2_submit_settings failed: ") + nghttp2_strerror(rv));
    }
    // 连接级接收窗口默认 65535,放大以免多个并发响应互相卡流控。
    rv = nghttp2_session_set_local_window_size(session.get(), NGHTTP2_FLAG_NONE, 0,
                                               static_cast<int32_t>(options.connectionWindowBytes));
    if (rv != 0)
    {
        return Status(StatusCode::kInternal,
                      std::string("nghttp2_session_set_local_window_size failed: ") + nghttp2_strerror(rv));
    }
    return Status::Ok();
}

ClientSession::SubmitResult ClientSession::SubmitUnary(const UnaryRequestSpec &spec)
{
    if (!spec.framedMessage)
    {
        SubmitResult result;
        result.error = Status(StatusCode::kInternal, "request message missing");
        return result;
    }
    auto stream = std::make_unique<StreamState>(options.maxReceiveMessageBytes);
    stream->framedRequest = spec.framedMessage;
    return SubmitRequest(spec.path, spec.metadata, spec.grpcTimeout, std::move(stream));
}

ClientSession::SubmitResult ClientSession::SubmitStream(const std::string &path, const Metadata &metadata)
{
    auto stream = std::make_unique<StreamState>(options.maxReceiveMessageBytes);
    stream->streaming = true;
    return SubmitRequest(path, metadata, std::string(), std::move(stream));
}

ClientSession::SubmitResult ClientSession::SubmitRequest(const std::string &path, const Metadata &metadata,
                                                         const std::string &grpcTimeout,
                                                         std::unique_ptr<StreamState> stream)
{
    SubmitResult result;
    if (!session)
    {
        result.error = Status(StatusCode::kInternal, "http2 session not started");
        return result;
    }
    if (goawayReceived || nghttp2_session_check_request_allowed(session.get()) == 0)
    {
        // 收到 GOAWAY,或会话已被本端终止(GOAWAY 还没刷出去):新请求在这条连接上必被 REFUSED,直接换连接。
        result.error = Status(StatusCode::kUnavailable, "connection is draining or closing");
        result.retryOnNewConnection = true;
        return result;
    }

    // 先把全部头部拼成稳定的字符串,再建 nghttp2_nv 引用它们(vector 不再扩容,指针稳定)。
    std::vector<std::pair<std::string, std::string>> headers;
    headers.reserve(8 + metadata.size());
    headers.emplace_back(":method", "POST");
    headers.emplace_back(":scheme", "http");
    headers.emplace_back(":path", path);
    headers.emplace_back(":authority", options.authority);
    headers.emplace_back("te", "trailers");
    if (!grpcTimeout.empty())
    {
        headers.emplace_back("grpc-timeout", grpcTimeout);
    }
    headers.emplace_back("content-type", std::string(kGrpcContentType));
    headers.emplace_back("user-agent", options.userAgent);
    for (const auto &[key, value] : metadata)
    {
        if (!IsValidCustomMetadataKey(key))
        {
            result.error = Status(StatusCode::kInternal, "invalid metadata key '" + key + "'");
            return result;
        }
        if (IsBinaryHeaderKey(key))
        {
            headers.emplace_back(key, Base64EncodeUnpadded(value));
        }
        else
        {
            if (!IsValidAsciiMetadataValue(value))
            {
                result.error = Status(StatusCode::kInternal, "invalid ASCII value for metadata key '" + key + "'");
                return result;
            }
            headers.emplace_back(key, value);
        }
    }

    std::vector<nghttp2_nv> nva;
    nva.reserve(headers.size());
    for (const auto &[name, value] : headers)
    {
        nva.push_back(MakeNv(name, value));
    }

    nghttp2_data_provider2 provider;
    provider.source.ptr = nullptr;
    provider.read_callback = &ClientSessionCallbacks::ReadRequestBody;

    const int32_t streamId =
        nghttp2_submit_request2(session.get(), nullptr, nva.data(), nva.size(), &provider, nullptr);
    if (streamId < 0)
    {
        if (streamId == NGHTTP2_ERR_STREAM_ID_NOT_AVAILABLE)
        {
            result.error = Status(StatusCode::kUnavailable, "stream ids exhausted on this connection");
            result.retryOnNewConnection = true;
            return result;
        }
        result.error =
            Status(StatusCode::kInternal, std::string("failed to submit request: ") + nghttp2_strerror(streamId));
        return result;
    }
    streams.emplace(streamId, std::move(stream));
    result.streamId = streamId;
    return result;
}

bool ClientSession::AppendStreamMessage(int32_t streamId, std::string_view framedMessage)
{
    auto *stream = FindStream(streamId);
    if (stream == nullptr || !stream->streaming || stream->writesDone || stream->localStatus || stream->remoteEnded ||
        !session)
    {
        return false;
    }
    stream->pendingSend.append(framedMessage.data(), framedMessage.size());
    ResumeDeferredData(streamId, *stream);
    return true;
}

void ClientSession::HalfCloseStream(int32_t streamId)
{
    auto *stream = FindStream(streamId);
    if (stream == nullptr || !stream->streaming || stream->writesDone || !session)
    {
        return;
    }
    stream->writesDone = true;
    ResumeDeferredData(streamId, *stream);
}

void ClientSession::ResumeDeferredData(int32_t streamId, StreamState &stream)
{
    if (!stream.dataDeferred)
    {
        return;
    }
    stream.dataDeferred = false;
    const int rv = nghttp2_session_resume_data(session.get(), streamId);
    if (rv != 0)
    {
        // 理论上只有内存不足或流已从发送队列摘除才会失败;不处理的话这个流会静默停写。
        LOG_ERROR << "nghttp2_session_resume_data failed on stream " << streamId << ": " << nghttp2_strerror(rv)
                  << "; resetting the stream";
        SetLocalErrorAndReset(streamId, stream,
                              Status(StatusCode::kInternal, std::string("failed to resume stream data: ") +
                                                                nghttp2_strerror(rv)),
                              NGHTTP2_INTERNAL_ERROR);
    }
}

size_t ClientSession::PendingStreamBytes(int32_t streamId) const
{
    const auto *stream = FindStream(streamId);
    return stream == nullptr ? 0 : stream->pendingSend.size() - stream->pendingSendOffset;
}

void ClientSession::ResetStream(int32_t streamId, uint32_t http2ErrorCode, Status status)
{
    auto *stream = FindStream(streamId);
    if (stream == nullptr || !session)
    {
        return;
    }
    SetLocalErrorAndReset(streamId, *stream, std::move(status), http2ErrorCode);
}

Status ClientSession::Receive(const uint8_t *data, size_t len)
{
    if (!session)
    {
        return Status(StatusCode::kInternal, "http2 session not started");
    }
    const nghttp2_ssize consumed = nghttp2_session_mem_recv2(session.get(), data, len);
    if (consumed < 0)
    {
        return Status(StatusCode::kUnavailable, std::string("http2 receive failed: ") +
                                                    nghttp2_strerror(static_cast<int>(consumed)));
    }
    return Status::Ok();
}

Status ClientSession::DrainOutput(const std::function<bool(std::string_view chunk)> &write)
{
    if (!session)
    {
        return Status(StatusCode::kInternal, "http2 session not started");
    }
    for (;;)
    {
        const uint8_t *data = nullptr;
        const nghttp2_ssize produced = nghttp2_session_mem_send2(session.get(), &data);
        if (produced < 0)
        {
            return Status(StatusCode::kUnavailable,
                          std::string("http2 send failed: ") + nghttp2_strerror(static_cast<int>(produced)));
        }
        if (produced == 0)
        {
            return Status::Ok();
        }
        if (!write(AsView(data, static_cast<size_t>(produced))))
        {
            return Status::Ok();
        }
    }
}

bool ClientSession::ShouldClose() const
{
    if (!session)
    {
        return true;
    }
    return nghttp2_session_want_read(session.get()) == 0 && nghttp2_session_want_write(session.get()) == 0;
}

bool ClientSession::SubmitPing()
{
    return session && nghttp2_submit_ping(session.get(), NGHTTP2_FLAG_NONE, kProbePingPayload) == 0;
}

void ClientSession::SubmitGoaway(uint32_t http2ErrorCode)
{
    if (session)
    {
        nghttp2_session_terminate_session(session.get(), http2ErrorCode);
    }
}

void ClientSession::FailAllStreams(const Status &status)
{
    // 逐个摘除再回调:回调里看到的 ActiveStreamCount() 就是真实剩余的流数。
    while (!streams.empty())
    {
        const auto it = streams.begin();
        const int32_t streamId = it->first;
        const std::unique_ptr<StreamState> stream = std::move(it->second);
        streams.erase(it);
        StreamOutcome outcome;
        if (stream->localStatus)
        {
            outcome.status = *stream->localStatus;
        }
        else
        {
            outcome.status = status;
            // 请求头都没交给 socket,服务端不可能见过这个请求。
            outcome.unprocessed = !stream->requestHeadersSent;
        }
        observer.OnStreamClosed(streamId, std::move(outcome));
    }
}

ClientSession::StreamState *ClientSession::FindStream(int32_t streamId)
{
    const auto it = streams.find(streamId);
    return it == streams.end() ? nullptr : it->second.get();
}

const ClientSession::StreamState *ClientSession::FindStream(int32_t streamId) const
{
    const auto it = streams.find(streamId);
    return it == streams.end() ? nullptr : it->second.get();
}

void ClientSession::SetLocalErrorAndReset(int32_t streamId, StreamState &stream, Status status,
                                          uint32_t http2ErrorCode)
{
    if (!stream.localStatus)
    {
        stream.localStatus = std::move(status);
    }
    stream.messages.clear();
    nghttp2_submit_rst_stream(session.get(), NGHTTP2_FLAG_NONE, streamId, http2ErrorCode);
}

StreamOutcome ClientSession::BuildOutcome(StreamState &stream, uint32_t http2ErrorCode) const
{
    StreamOutcome outcome;
    if (stream.localStatus)
    {
        outcome.status = *stream.localStatus;
        return outcome;
    }
    if (http2ErrorCode == NGHTTP2_REFUSED_STREAM)
    {
        // 对端明确没处理(GOAWAY 之后的流、HEADERS 因 GOAWAY 没发出、或服务端主动 REFUSED_STREAM)。
        outcome.status = Status(StatusCode::kUnavailable, "stream refused by peer before processing");
        outcome.unprocessed = true;
        return outcome;
    }

    outcome.initialMetadata = std::move(stream.initialMetadata);
    outcome.trailingMetadata = std::move(stream.trailingMetadata);
    const bool isGrpcResponse = stream.httpStatus == 200 && stream.contentTypeIsGrpc;

    if (stream.trailersReceived)
    {
        if (!isGrpcResponse)
        {
            outcome.status = stream.httpStatus == 200
                                 ? Status(StatusCode::kUnknown, "response content-type is not application/grpc")
                                 : Status(StatusCodeFromHttpStatus(stream.httpStatus),
                                          "unexpected HTTP status " + std::to_string(stream.httpStatus));
            return outcome;
        }
        if (!stream.grpcStatusPresent)
        {
            outcome.status = Status(StatusCode::kUnknown, "missing grpc-status in trailers");
            return outcome;
        }
        if (!stream.grpcStatus)
        {
            outcome.status = Status(StatusCode::kUnknown, "invalid grpc-status in trailers");
            return outcome;
        }
        outcome.status = Status(*stream.grpcStatus, std::move(stream.grpcMessage));
        if (outcome.status.ok())
        {
            if (stream.deframer.HasPartialMessage())
            {
                outcome.status = Status(StatusCode::kInternal, "response ended in the middle of a message");
            }
            else if (stream.streaming)
            {
                // 双向流:消息已经逐条交出,条数不限。
            }
            else if (stream.messages.size() != 1)
            {
                outcome.status =
                    Status(StatusCode::kInternal, "unary response must contain exactly one message, got " +
                                                      std::to_string(stream.messages.size()));
            }
            else
            {
                outcome.response = std::move(stream.messages.front());
            }
        }
        return outcome;
    }

    if (http2ErrorCode != NGHTTP2_NO_ERROR)
    {
        outcome.status = StatusFromRstStreamError(http2ErrorCode);
        return outcome;
    }
    if (stream.responseHeadersReceived && !isGrpcResponse)
    {
        outcome.status = stream.httpStatus == 200
                             ? Status(StatusCode::kUnknown, "response content-type is not application/grpc")
                             : Status(StatusCodeFromHttpStatus(stream.httpStatus),
                                      "unexpected HTTP status " + std::to_string(stream.httpStatus));
        return outcome;
    }
    outcome.status = Status(StatusCode::kInternal, "server closed the stream without sending trailers");
    return outcome;
}

} // namespace muduo_grpc
