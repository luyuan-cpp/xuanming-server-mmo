#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <cstring>
#include <string_view>
#include <type_traits>

#include "core/tracing/trace_context.h"

// 已结束的 span(docs/design/distributed-tracing.md §10.3 阶段三):业务线程在接缝作用域结束时填好它,交给导出去处
// (SpanSink),导出线程攒批后按 OTLP/HTTP 发给本机 Alloy。纯数据,只有头文件,不依赖 protobuf、muduo 与日志。
//
// 为什么是定长结构:业务线程入队只做一次 memcpy,不在堆上分配;名字与错误说明超长时按字节截断。
// 名字只放方法名、消息名或 topic(OTel 的 span 名要求低基数),不放玩家 id 这类高基数值。

namespace tracing {

// 与 OTLP Span.SpanKind 取值相同(opentelemetry-proto trace.proto),编码时原样转换。
enum class SpanKind : uint8_t {
    kInternal = 1,
    kServer = 2,
    kClient = 3,
    kProducer = 4,
    kConsumer = 5,
};

// 与 OTLP Status.StatusCode 取值相同。成功保持 kUnset:OTel 规范要求插桩库不主动置 OK。
enum class SpanStatus : uint8_t {
    kUnset = 0,
    kOk = 1,
    kError = 2,
};

// 产生 span 的通道,编码成 OTel 语义约定的属性(rpc.system / messaging.system,见 otlp_trace_encoder.cpp)。
enum class SpanSystem : uint8_t {
    kNone = 0,
    kRpc0 = 1,         // C++ 节点之间的 muduo RPC
    kGrpc = 2,         // muduo_grpc(C++ ↔ Go)
    kKafka = 3,        // librdkafka
    kGateClient = 4,   // gate 收到的客户端消息、建连与断线
    kCount,
};

inline constexpr size_t kSpanNameCapacity = 96;
inline constexpr size_t kSpanStatusMessageCapacity = 64;

// span 名的两段写法:tail 非空时是 "head/tail"(OTel RPC 约定的 {rpc.service}/{rpc.method}),否则就是 head。
// 两段都只在构造 SpanRecord 时读一次,不要求比 span 活得久。
struct SpanName {
    std::string_view head;
    std::string_view tail{};
};

struct SpanRecord {
    TraceId trace_id;
    SpanId span_id;
    SpanId parent_span_id;          // 全零 = 根 span
    uint64_t start_unix_nano{0};
    uint64_t end_unix_nano{0};
    uint32_t grpc_status_code{0};   // has_grpc_status 为真时有效
    uint8_t trace_flags{0};         // W3C flags,只用最低位 sampled
    SpanKind kind{SpanKind::kInternal};
    SpanStatus status{SpanStatus::kUnset};
    SpanSystem system{SpanSystem::kNone};
    bool parent_is_remote{false};   // 父 span 在别的进程(server / consumer 接续远端上下文时为真)
    bool has_grpc_status{false};
    uint8_t name_size{0};
    uint8_t status_message_size{0};
    std::array<char, kSpanNameCapacity> name{};
    std::array<char, kSpanStatusMessageCapacity> status_message{};

    std::string_view Name() const { return std::string_view(name.data(), name_size); }
    std::string_view StatusMessage() const { return std::string_view(status_message.data(), status_message_size); }

    void SetName(SpanName parts)
    {
        name_size = 0;
        AppendBounded(name.data(), kSpanNameCapacity, name_size, parts.head);
        if (!parts.tail.empty()) {
            AppendBounded(name.data(), kSpanNameCapacity, name_size, std::string_view("/", 1));
            AppendBounded(name.data(), kSpanNameCapacity, name_size, parts.tail);
        }
    }

    void SetStatusMessage(std::string_view text)
    {
        status_message_size = 0;
        AppendBounded(status_message.data(), kSpanStatusMessageCapacity, status_message_size, text);
    }

private:
    // 追加到定长缓冲,放不下的部分丢掉。size 一直不超过 capacity(两个容量都小于 256,uint8_t 装得下)。
    static void AppendBounded(char* buffer, size_t capacity, uint8_t& size, std::string_view text)
    {
        const size_t room = capacity - size;
        const size_t count = text.size() < room ? text.size() : room;
        if (count > 0) {
            std::memcpy(buffer + size, text.data(), count);
            size = static_cast<uint8_t>(size + count);
        }
    }
};

static_assert(kSpanNameCapacity < 256 && kSpanStatusMessageCapacity < 256, "sizes are stored in uint8_t");
static_assert(std::is_trivially_copyable_v<SpanRecord>, "SpanRecord is copied into the export queue with memcpy semantics");

// 已结束 span 的去处。Submit 可能在任何线程上调用(EventLoop 线程、Kafka poll 线程),实现必须线程安全、不阻塞、
// 不抛异常;放不下就丢并计数(docs/design/distributed-tracing.md §10.3:绝不阻塞业务线程)。
// 进程只装一个(InstallSpanSink,thread_context/trace_context_tls.h);装上的对象必须活到进程退出,
// 因为摘下之后仍可能有线程拿着旧指针正在 Submit。
class SpanSink {
public:
    virtual void Submit(const SpanRecord& span) noexcept = 0;

protected:
    ~SpanSink() = default;
};

} // namespace tracing
