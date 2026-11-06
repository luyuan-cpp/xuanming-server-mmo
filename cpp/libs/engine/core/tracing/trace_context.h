#pragma once

#include <array>
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <cstring>
#include <random>
#include <string>
#include <string_view>

// W3C Trace Context 原语(docs/design/distributed-tracing.md §3.1、§5.7)。
//
// 提供:128 位 trace_id 与 64 位 span_id 的生成;`traceparent` 头 `00-{32 位 trace_id}-{16 位 span_id}-{2 位 flags}`
// 的解析与序列化;新根与子 span 的构造。只有头文件,不依赖 protobuf 和日志;线程局部的「当前上下文」
// 在 thread_context/trace_context_tls.h。
//
// 为什么用 W3C 格式而不是自定义格式:Go(go-zero)、Java(Micrometer)和任何 OpenTelemetry 采集器都直接认,
// 三种语言之间不需要转换;128 位 trace_id 在任何规模下都不必考虑碰撞。

namespace tracing {

inline constexpr size_t kTraceIdBytes = 16;
inline constexpr size_t kSpanIdBytes = 8;
inline constexpr size_t kTraceIdHexChars = kTraceIdBytes * 2;
inline constexpr size_t kSpanIdHexChars = kSpanIdBytes * 2;
// "00-" + trace_id + "-" + span_id + "-" + flags
inline constexpr size_t kTraceparentChars = 3 + kTraceIdHexChars + 1 + kSpanIdHexChars + 1 + 2;

// 把 count 个字节写成 2*count 个小写十六进制字符,不写 '\0'。查半字节表而不是逐字节 snprintf:
// 日志后缀与 traceparent 序列化都在热路径上。
inline void WriteLowerHex(const uint8_t* bytes, size_t count, char* out)
{
    constexpr char kDigits[] = "0123456789abcdef";
    for (size_t i = 0; i < count; ++i) {
        out[i * 2] = kDigits[bytes[i] >> 4];
        out[i * 2 + 1] = kDigits[bytes[i] & 0x0F];
    }
}

// 16 字节 = 线上 32 个小写十六进制字符(W3C trace-id)。全零是非法值。
struct TraceId {
    std::array<uint8_t, kTraceIdBytes> bytes{};

    bool IsValid() const
    {
        for (const auto b : bytes) {
            if (b != 0) return true;
        }
        return false;
    }

    std::string ToHex() const
    {
        std::string hex(kTraceIdHexChars, '\0');
        WriteLowerHex(bytes.data(), bytes.size(), hex.data());
        return hex;
    }
};

// 8 字节 = 线上 16 个小写十六进制字符(W3C parent-id)。全零是非法值。
struct SpanId {
    std::array<uint8_t, kSpanIdBytes> bytes{};

    bool IsValid() const
    {
        for (const auto b : bytes) {
            if (b != 0) return true;
        }
        return false;
    }

    std::string ToHex() const
    {
        std::string hex(kSpanIdHexChars, '\0');
        WriteLowerHex(bytes.data(), bytes.size(), hex.data());
        return hex;
    }
};

// 线程局部 PRNG,每个线程用时钟与 TLS 地址播种一次。trace-id 只要求全局唯一、不要求不可预测
// (W3C 安全考量一节),所以不用加密随机数;每次入站请求都调用,不能有系统调用。
inline TraceId NewTraceId()
{
    thread_local std::mt19937_64 rng(
        static_cast<uint64_t>(
            std::chrono::high_resolution_clock::now().time_since_epoch().count()
        ) ^ reinterpret_cast<uintptr_t>(&rng));

    TraceId id;
    const uint64_t hi = rng();
    const uint64_t lo = rng();
    std::memcpy(&id.bytes[0], &hi, 8);
    std::memcpy(&id.bytes[8], &lo, 8);
    return id;
}

inline SpanId NewSpanId()
{
    thread_local std::mt19937_64 rng(
        static_cast<uint64_t>(
            std::chrono::high_resolution_clock::now().time_since_epoch().count()
        ) ^ reinterpret_cast<uintptr_t>(&rng) ^ 0x9e3779b97f4a7c15ULL);

    SpanId id;
    const uint64_t v = rng();
    std::memcpy(&id.bytes[0], &v, 8);
    return id;
}

// flags 只用最低位 sampled。新根默认采样(01):本期不导出 span,flags 只是标记;
// 真正的取舍放在采集器的尾部采样(设计 §3.4)。
struct TraceContext {
    TraceId trace_id;
    SpanId span_id;
    uint8_t flags{0x01};

    // 只写 sampled 位,其余 flag 位按 W3C level 1 写 0。
    std::string ToTraceparent() const
    {
        std::string header(kTraceparentChars, '-');
        header[0] = '0';
        header[1] = '0';
        WriteLowerHex(trace_id.bytes.data(), kTraceIdBytes, header.data() + 3);
        WriteLowerHex(span_id.bytes.data(), kSpanIdBytes, header.data() + 4 + kTraceIdHexChars);
        header[kTraceparentChars - 2] = '0';
        header[kTraceparentChars - 1] = (flags & 0x01) ? '1' : '0';
        return header;
    }

    bool IsValid() const { return trace_id.IsValid() && span_id.IsValid(); }
};

// 小写十六进制字符 → 0..15;其他字符(包括大写)→ -1。W3C 规定含大写的 traceparent 必须忽略。
inline int LowerHexDigitValue(char c)
{
    if (c >= '0' && c <= '9') return c - '0';
    if (c >= 'a' && c <= 'f') return c - 'a' + 10;
    return -1;
}

inline bool ParseLowerHex(std::string_view src, uint8_t* dst, size_t count)
{
    if (src.size() != count * 2) return false;
    for (size_t i = 0; i < count; ++i) {
        const int hi = LowerHexDigitValue(src[i * 2]);
        const int lo = LowerHexDigitValue(src[i * 2 + 1]);
        if (hi < 0 || lo < 0) return false;
        dst[i] = static_cast<uint8_t>((hi << 4) | lo);
    }
    return true;
}

// 解析 `traceparent`。只认版本 00、正好 55 个字符、小写十六进制、trace_id 与 span_id 都非零;
// 成功才写 ctx,失败时 ctx 保持原样。调用方把失败当作「没有上下文」去开新根,
// 绝不能据此拒绝或丢弃请求(设计 §3.3)。
inline bool ParseTraceparent(std::string_view header, TraceContext& ctx)
{
    constexpr size_t kSpanStart = 4 + kTraceIdHexChars;
    constexpr size_t kFlagsStart = kSpanStart + kSpanIdHexChars + 1;
    if (header.size() != kTraceparentChars) return false;
    if (header[0] != '0' || header[1] != '0') return false;
    if (header[2] != '-' || header[kSpanStart - 1] != '-' || header[kFlagsStart - 1] != '-') return false;

    TraceContext parsed;
    uint8_t flags = 0;
    if (!ParseLowerHex(header.substr(3, kTraceIdHexChars), parsed.trace_id.bytes.data(), kTraceIdBytes)) return false;
    if (!ParseLowerHex(header.substr(kSpanStart, kSpanIdHexChars), parsed.span_id.bytes.data(), kSpanIdBytes)) return false;
    if (!ParseLowerHex(header.substr(kFlagsStart, 2), &flags, 1)) return false;
    if (!parsed.IsValid()) return false;

    parsed.flags = flags;
    ctx = parsed;
    return true;
}

// 新根:本进程是这条 trace 的起点(入口没有收到合法上下文)。
inline TraceContext NewRoot()
{
    TraceContext c;
    c.trace_id = NewTraceId();
    c.span_id = NewSpanId();
    c.flags = 0x01;
    return c;
}

// 同一 trace 下的子 span:沿用 trace_id 与 flags,换一个新的 span_id。
inline TraceContext ChildSpan(const TraceContext& parent)
{
    TraceContext c;
    c.trace_id = parent.trace_id;
    c.span_id = NewSpanId();
    c.flags = parent.flags;
    return c;
}

} // namespace tracing
