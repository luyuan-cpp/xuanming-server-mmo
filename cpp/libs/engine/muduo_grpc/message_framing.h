#pragma once

// gRPC 的 Length-Prefixed-Message:1 字节压缩标志 + 4 字节大端长度 + 消息体。
// DATA 帧边界与消息边界无关,接收方必须能跨帧拼接(PROTOCOL-HTTP2.md)。

#include <cstddef>
#include <cstdint>
#include <string>
#include <string_view>
#include <vector>

#include "status.h"

namespace muduo_grpc
{

inline constexpr size_t kMessagePrefixBytes = 5;

// 把一条未压缩消息(压缩标志 0)拼到 out 末尾。payload.size() 必须 ≤ UINT32_MAX(调用方先按上限拦截)。
void AppendLengthPrefixedMessage(std::string &out, std::string_view payload);

// 增量拆包。每个流一个实例,不跨流复用。
class MessageDeframer
{
public:
    explicit MessageDeframer(size_t maxMessageBytes) : maxMessageBytes(maxMessageBytes) {}

    // 喂入一段 DATA 负载;拆出的完整消息追加到 out。
    // 返回非 OK 表示流必须终止:声明长度超上限 → RESOURCE_EXHAUSTED;压缩标志为 1 → INTERNAL
    // (我们从不声明 grpc-accept-encoding,对端压缩即违约);标志既不是 0 也不是 1 → INTERNAL。
    // 一旦返回错误,之后的 Feed 一律返回同一个错误。
    Status Feed(const uint8_t *data, size_t len, std::vector<std::string> &out);

    // 有没有拆了一半的消息(流结束时有残留 = 对端截断,调用方按 INTERNAL 处理)。
    bool HasPartialMessage() const { return headerFilled > 0 || bodyExpected > 0; }

private:
    size_t maxMessageBytes;
    uint8_t header[kMessagePrefixBytes]{};
    size_t headerFilled{0};
    size_t bodyExpected{0};
    std::string body;
    Status error;
};

} // namespace muduo_grpc
