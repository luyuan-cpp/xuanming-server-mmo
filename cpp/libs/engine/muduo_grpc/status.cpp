#include "status.h"

#include <charconv>

namespace muduo_grpc
{

namespace
{

// RFC 9113 §7 的 HTTP/2 错误码。这里不 include nghttp2.h:公共工具层不依赖编解码库。
constexpr uint32_t kH2NoError = 0x0;
constexpr uint32_t kH2ProtocolError = 0x1;
constexpr uint32_t kH2InternalError = 0x2;
constexpr uint32_t kH2FlowControlError = 0x3;
constexpr uint32_t kH2SettingsTimeout = 0x4;
constexpr uint32_t kH2StreamClosed = 0x5;
constexpr uint32_t kH2FrameSizeError = 0x6;
constexpr uint32_t kH2RefusedStream = 0x7;
constexpr uint32_t kH2Cancel = 0x8;
constexpr uint32_t kH2CompressionError = 0x9;
constexpr uint32_t kH2ConnectError = 0xa;
constexpr uint32_t kH2EnhanceYourCalm = 0xb;
constexpr uint32_t kH2InadequateSecurity = 0xc;

const char *Http2ErrorName(uint32_t code)
{
    switch (code)
    {
    case kH2NoError: return "NO_ERROR";
    case kH2ProtocolError: return "PROTOCOL_ERROR";
    case kH2InternalError: return "INTERNAL_ERROR";
    case kH2FlowControlError: return "FLOW_CONTROL_ERROR";
    case kH2SettingsTimeout: return "SETTINGS_TIMEOUT";
    case kH2StreamClosed: return "STREAM_CLOSED";
    case kH2FrameSizeError: return "FRAME_SIZE_ERROR";
    case kH2RefusedStream: return "REFUSED_STREAM";
    case kH2Cancel: return "CANCEL";
    case kH2CompressionError: return "COMPRESSION_ERROR";
    case kH2ConnectError: return "CONNECT_ERROR";
    case kH2EnhanceYourCalm: return "ENHANCE_YOUR_CALM";
    case kH2InadequateSecurity: return "INADEQUATE_SECURITY";
    default: return "UNKNOWN_HTTP2_ERROR";
    }
}

} // namespace

const char *StatusCodeName(StatusCode code)
{
    switch (code)
    {
    case StatusCode::kOk: return "OK";
    case StatusCode::kCancelled: return "CANCELLED";
    case StatusCode::kUnknown: return "UNKNOWN";
    case StatusCode::kInvalidArgument: return "INVALID_ARGUMENT";
    case StatusCode::kDeadlineExceeded: return "DEADLINE_EXCEEDED";
    case StatusCode::kNotFound: return "NOT_FOUND";
    case StatusCode::kAlreadyExists: return "ALREADY_EXISTS";
    case StatusCode::kPermissionDenied: return "PERMISSION_DENIED";
    case StatusCode::kResourceExhausted: return "RESOURCE_EXHAUSTED";
    case StatusCode::kFailedPrecondition: return "FAILED_PRECONDITION";
    case StatusCode::kAborted: return "ABORTED";
    case StatusCode::kOutOfRange: return "OUT_OF_RANGE";
    case StatusCode::kUnimplemented: return "UNIMPLEMENTED";
    case StatusCode::kInternal: return "INTERNAL";
    case StatusCode::kUnavailable: return "UNAVAILABLE";
    case StatusCode::kDataLoss: return "DATA_LOSS";
    case StatusCode::kUnauthenticated: return "UNAUTHENTICATED";
    }
    return "UNKNOWN";
}

std::string Status::ToString() const
{
    std::string text = StatusCodeName(code);
    text += '(';
    text += std::to_string(code_value());
    text += ')';
    if (!detailMessage.empty())
    {
        text += ": ";
        text += detailMessage;
    }
    return text;
}

StatusCode StatusCodeFromHttpStatus(int httpStatus)
{
    switch (httpStatus)
    {
    case 400: return StatusCode::kInternal;
    case 401: return StatusCode::kUnauthenticated;
    case 403: return StatusCode::kPermissionDenied;
    case 404: return StatusCode::kUnimplemented;
    case 429:
    case 502:
    case 503:
    case 504: return StatusCode::kUnavailable;
    default: return StatusCode::kUnknown;
    }
}

Status StatusFromRstStreamError(uint32_t http2ErrorCode)
{
    std::string detail = "stream reset by peer: ";
    detail += Http2ErrorName(http2ErrorCode);
    switch (http2ErrorCode)
    {
    case kH2RefusedStream:
        return Status(StatusCode::kUnavailable, std::move(detail));
    case kH2Cancel:
        return Status(StatusCode::kCancelled, std::move(detail));
    case kH2EnhanceYourCalm:
        // 规范:附带说明被耗尽的资源是带宽。
        detail += " (bandwidth exhausted)";
        return Status(StatusCode::kResourceExhausted, std::move(detail));
    case kH2InadequateSecurity:
        detail += " (transport security is insufficient)";
        return Status(StatusCode::kPermissionDenied, std::move(detail));
    default:
        // NO_ERROR / PROTOCOL_ERROR / INTERNAL_ERROR / FLOW_CONTROL_ERROR / SETTINGS_TIMEOUT /
        // FRAME_SIZE_ERROR / COMPRESSION_ERROR / CONNECT_ERROR 规范都映射 INTERNAL;
        // STREAM_CLOSED 规范说「无映射、记日志」,这里同样落到 INTERNAL,原因写进 message。
        return Status(StatusCode::kInternal, std::move(detail));
    }
}

std::optional<StatusCode> ParseGrpcStatusValue(std::string_view value)
{
    if (value.empty() || value.size() > 2)
    {
        return std::nullopt;
    }
    uint32_t parsed = 0;
    const auto *begin = value.data();
    const auto *end = value.data() + value.size();
    const auto [ptr, ec] = std::from_chars(begin, end, parsed);
    if (ec != std::errc{} || ptr != end || parsed > kMaxStatusCodeValue)
    {
        return std::nullopt;
    }
    return static_cast<StatusCode>(parsed);
}

} // namespace muduo_grpc
