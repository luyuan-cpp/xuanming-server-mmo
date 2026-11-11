#pragma once

#include <concepts>
#include <cstdint>

// 日志上下文里的玩家从哪来(docs/design/distributed-tracing.md §5.5):消息顶层 player_id 字段的值。
// gRPC 服务端(core/network/grpc_unary_call.h)与控制面命令消费(node_kafka_command_handler.h)共用这一份口径,
// 与 Go 的 tracing.PlayerFieldInterceptor(请求顶层 GetPlayerId)一致。只有头文件,只依赖标准库。

namespace tracing {

// 消息顶层有能转成 uint64 的 player_id() 时返回它,否则返回 0(不装玩家)。
// 不要求消息都带这个字段;repeated(访问器要下标)、字符串、子消息形态的同名字段一律不取。
template <typename Message>
uint64_t TopLevelPlayerId(const Message& message)
{
    if constexpr (requires { { message.player_id() } -> std::convertible_to<uint64_t>; }) {
        return static_cast<uint64_t>(message.player_id());
    } else {
        return 0;
    }
}

} // namespace tracing
