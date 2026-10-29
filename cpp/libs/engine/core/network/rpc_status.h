#pragma once

// 一次节点间 RPC0 调用在调用方看到的完成状态(docs/design/muduo-rpc-channel.md §4.3)。
//
// 只在本进程内使用,不上线:线上的拒绝原因是 GameErrorCode(rpc_message.proto)。
// 两类失败必须分清:
//   * DefinitelyNotExecuted():本地没发出去,或对端在派发前就拒绝了 —— 可以安全重试;
//   * OutcomeUnknown():超时 / 断连 / 通道关闭 / 回包坏了 / handler 接管后丢了回复 —— 对端可能已经执行。
//     与 gRPC 的口径一致(grpc-muduo-transport.md §16.2):只释放本地在途状态、提示客户端、
//     重试幂等操作,不得当作「对端拒绝」去改权威状态。
// 命名避开 Windows 的 ERROR / NO_ERROR 宏(muduo 原版 RpcChannel.cc 在 proto 改名后踩过)。

#include <array>
#include <cstddef>
#include <cstdint>

#include "proto/common/base/rpc_message.pb.h"

enum class RpcStatusCode : uint8_t
{
    kOk,
    kNotSent,          // 本地确定未发出:未连接 / 通道已关闭 / 在途已满 / 消息号不可派发 / 类型与注册表不符 / 序列化失败
    kRemoteRejected,   // 对端 RPC_ERROR:NO_SERVICE / NO_METHOD / INVALID_REQUEST / WRONG_PROTO —— 派发前拒绝,确定未执行
    kRemoteFailed,     // 对端 RPC_ERROR:HANDLER_FAILED —— handler 已运行并显式失败
    kReplyDropped,     // 对端 RPC_ERROR:REPLY_DROPPED —— handler 接管后没完成就释放了,结果未知
    kTimeout,          // 截止时刻已到仍未回包,结果未知
    kDisconnected,     // 在途期间连接断开,结果未知
    kChannelClosed,    // 通道销毁 / 节点关机,结果未知
    kInvalidResponse,  // 回包解析失败或 message_id 与登记不符,结果未知
    kCount
};

// const char*:muduo 的 LogStream 不认 std::string_view。
inline constexpr std::array<const char*, static_cast<std::size_t>(RpcStatusCode::kCount)> kRpcStatusCodeNames{
    "ok",
    "not_sent",
    "remote_rejected",
    "remote_failed",
    "reply_dropped",
    "timeout",
    "disconnected",
    "channel_closed",
    "invalid_response",
};
static_assert(kRpcStatusCodeNames.size() == static_cast<std::size_t>(RpcStatusCode::kCount));

inline const char* RpcStatusCodeName(RpcStatusCode code)
{
    const auto index = static_cast<std::size_t>(code);
    return index < kRpcStatusCodeNames.size() ? kRpcStatusCodeNames[index] : "unknown";
}

class RpcStatus
{
public:
    constexpr RpcStatus() = default;
    constexpr explicit RpcStatus(RpcStatusCode code, GameErrorCode remoteError = RPC_NO_ERROR)
        : code_(code), remoteError_(remoteError)
    {
    }

    static constexpr RpcStatus Ok() { return RpcStatus{RpcStatusCode::kOk}; }

    // 对端回了 RPC_ERROR:按线上的拒绝原因归类。
    static constexpr RpcStatus FromRemoteError(GameErrorCode error)
    {
        switch (error)
        {
        case HANDLER_FAILED:
            return RpcStatus{RpcStatusCode::kRemoteFailed, error};
        case REPLY_DROPPED:
            return RpcStatus{RpcStatusCode::kReplyDropped, error};
        default:
            // NO_SERVICE / NO_METHOD / INVALID_REQUEST / WRONG_PROTO,以及将来新增而本端不认识的码:
            // 一律按「派发前拒绝」处理。新增码若含义是「已执行」,必须在上面单列。
            return RpcStatus{RpcStatusCode::kRemoteRejected, error};
        }
    }

    constexpr bool ok() const { return code_ == RpcStatusCode::kOk; }
    constexpr RpcStatusCode code() const { return code_; }
    // 只有 kRemoteRejected / kRemoteFailed / kReplyDropped 时有意义。
    constexpr GameErrorCode remoteError() const { return remoteError_; }

    constexpr bool DefinitelyNotExecuted() const
    {
        return code_ == RpcStatusCode::kNotSent || code_ == RpcStatusCode::kRemoteRejected;
    }

    constexpr bool OutcomeUnknown() const
    {
        return code_ == RpcStatusCode::kTimeout || code_ == RpcStatusCode::kDisconnected ||
               code_ == RpcStatusCode::kChannelClosed || code_ == RpcStatusCode::kInvalidResponse ||
               code_ == RpcStatusCode::kReplyDropped;
    }

    const char* name() const { return RpcStatusCodeName(code_); }

private:
    RpcStatusCode code_{RpcStatusCode::kOk};
    GameErrorCode remoteError_{RPC_NO_ERROR};
};
