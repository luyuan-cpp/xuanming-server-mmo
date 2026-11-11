#pragma once

#include <cstddef>
#include <cstdint>

#include "core/tracing/trace_context.h"
#include "proto/common/base/rpc_message.pb.h"

// RPC0 信封上的 trace(docs/design/distributed-tracing.md §5.2):W3C trace-context 的二进制形态,
// 放在 GameRpcMessage 的 trace_id_hi / trace_id_lo / parent_span_id / trace_flags(字段 7-10)。
//
// 三个 64 位字段都按大端把字节拼成整数:%016llx%016llx 打出来就是日志里的 trace_id,抓包能直接对上日志。
// 四个字段全 0 表示没有 trace;proto3 不编码 0 值,所以没有 trace 的帧一个字节都不多。
// 只在 REQUEST 上写:RESPONSE / RPC_ERROR / HEARTBEAT 不带,回包由调用方恢复自己捕获的上下文。

namespace rpc_trace_envelope_detail {

inline uint64_t LoadBigEndian64(const uint8_t* bytes)
{
    uint64_t value = 0;
    for (size_t i = 0; i < 8; ++i) {
        value = (value << 8) | bytes[i];
    }
    return value;
}

inline void StoreBigEndian64(uint64_t value, uint8_t* bytes)
{
    for (size_t i = 0; i < 8; ++i) {
        bytes[7 - i] = static_cast<uint8_t>(value >> (8 * i));
    }
}

} // namespace rpc_trace_envelope_detail

// trace 有效时写进信封;无效时什么都不写,四个字段保持 0。
inline void WriteTraceToRpcEnvelope(const tracing::TraceContext& trace, GameRpcMessage& message)
{
    if (!trace.IsValid()) {
        return;
    }
    using rpc_trace_envelope_detail::LoadBigEndian64;
    message.set_trace_id_hi(LoadBigEndian64(trace.trace_id.bytes.data()));
    message.set_trace_id_lo(LoadBigEndian64(trace.trace_id.bytes.data() + 8));
    message.set_parent_span_id(LoadBigEndian64(trace.span_id.bytes.data()));
    message.set_trace_flags(trace.flags);
}

// 读出信封上的 trace。trace_id 全 0 或 parent_span_id 为 0 都当作没有:返回 false,trace 保持原样,
// 收方据此开新根。对端发来什么都不会让请求被拒(设计 §3.3)。flags 只取低 8 位。
inline bool ReadTraceFromRpcEnvelope(const GameRpcMessage& message, tracing::TraceContext& trace)
{
    using rpc_trace_envelope_detail::StoreBigEndian64;
    tracing::TraceContext parsed;
    StoreBigEndian64(message.trace_id_hi(), parsed.trace_id.bytes.data());
    StoreBigEndian64(message.trace_id_lo(), parsed.trace_id.bytes.data() + 8);
    StoreBigEndian64(message.parent_span_id(), parsed.span_id.bytes.data());
    if (!parsed.IsValid()) {
        return false;
    }
    parsed.flags = static_cast<uint8_t>(message.trace_flags() & 0xFFu);
    trace = parsed;
    return true;
}
