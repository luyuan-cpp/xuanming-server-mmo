#pragma once

// gRPC 调用的完成状态(docs/design/grpc-muduo-transport.md §4.3)。
//
// 状态码的**数值**与 gRPC 规范、grpc::StatusCode、grpc-go codes 完全一致:日志里按整数打印,
// 新旧两套实现的日志要能逐条对照;线上 trailers 的 grpc-status 也是这个十进制数。

#include <cstdint>
#include <optional>
#include <string>
#include <string_view>
#include <utility>

namespace muduo_grpc
{

enum class StatusCode : uint32_t
{
    kOk = 0,
    kCancelled = 1,
    kUnknown = 2,
    kInvalidArgument = 3,
    kDeadlineExceeded = 4,
    kNotFound = 5,
    kAlreadyExists = 6,
    kPermissionDenied = 7,
    kResourceExhausted = 8,
    kFailedPrecondition = 9,
    kAborted = 10,
    kOutOfRange = 11,
    kUnimplemented = 12,
    kInternal = 13,
    kUnavailable = 14,
    kDataLoss = 15,
    kUnauthenticated = 16,
};

// 规范里最大的状态码;超过它的 grpc-status 按 UNKNOWN 处理。
inline constexpr uint32_t kMaxStatusCodeValue = 16;

const char *StatusCodeName(StatusCode code);

class Status
{
public:
    Status() = default;
    Status(StatusCode code, std::string message) : code(code), detailMessage(std::move(message)) {}

    static Status Ok() { return Status(); }

    bool ok() const { return code == StatusCode::kOk; }
    StatusCode Code() const { return code; }
    // 日志与对照用:与 grpc::Status::error_code() 的整数值相同。
    uint32_t code_value() const { return static_cast<uint32_t>(code); }
    const std::string &message() const { return detailMessage; }

    // "UNAVAILABLE(14): connection lost" 形式,只用于日志。
    std::string ToString() const;

private:
    StatusCode code{StatusCode::kOk};
    std::string detailMessage;
};

// HTTP 状态码 → gRPC 状态码(gRPC doc/http-grpc-status-mapping.md)。
// 只在响应不是合法 gRPC 响应(:status != 200 或 content-type 不对)时使用。
StatusCode StatusCodeFromHttpStatus(int httpStatus);

// 对端 RST_STREAM 的 HTTP/2 错误码 → gRPC 状态(PROTOCOL-HTTP2.md「Errors」一节)。
// REFUSED_STREAM 的含义是「对端没处理这个流」,是否可重试由调用方另行判断,这里只给状态。
Status StatusFromRstStreamError(uint32_t http2ErrorCode);

// 解析 trailers 里的 grpc-status:必须是纯十进制、且在 0..16 之内;否则返回 nullopt(调用方按 UNKNOWN 处理)。
std::optional<StatusCode> ParseGrpcStatusValue(std::string_view value);

} // namespace muduo_grpc
