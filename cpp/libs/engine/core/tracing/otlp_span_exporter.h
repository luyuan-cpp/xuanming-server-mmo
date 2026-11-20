#pragma once

#include <atomic>
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <memory>
#include <optional>

#include "core/tracing/otlp_http.h"
#include "core/tracing/otlp_trace_encoder.h"
#include "core/tracing/span_export_queue.h"
#include "muduo/net/InetAddress.h"

namespace muduo::net
{
class EventLoop;
}

// span 导出器(docs/design/distributed-tracing.md §10.3):从 SpanExportQueue 取 span,攒批编成 OTLP 的
// ExportTraceServiceRequest,经 OTLP/HTTP(protobuf)发给本机 / 本节点的 Alloy。攒批语义照 OTel SDK 规范的 BatchSpanProcessor:
//   * 每 scheduleDelay(5 秒)发一批,队列攒够一批(512)时提前发;同一时刻最多一个在途请求;
//   * 一次导出(连接 + 发送 + 等响应)超过 exportTimeout(30 秒)就算失败;
//   * 失败(连不上、超时、非 2xx)就丢掉这一批并计数,不重试、不回填队列。连不上时按 1 秒起、翻倍、封顶 60 秒退避:
//     退避期间每次该发的时候丢一批最旧的,队列不会一直顶满,也不会每 5 秒刷一遍 muduo 的连接日志;
//   * 停机时在 shutdownTimeout(2 秒)内把剩下的发出去,发不完的丢掉并计数。
// 不引入 OpenTelemetry C++ SDK(它的 OTLP/gRPC 导出器会拉起 gRPC 运行时线程,与「运行期不起 gRPC 线程」冲突)。
//
// 跑在节点的主 EventLoop 上,不另起线程(负责人 2026-11-19:C++ 节点除主线程不再开线程)。所有网络都是 muduo 的非阻塞 I/O;
// 唯一会阻塞的主机名解析放在启动时做一次(ResolveOtlpEndpoint),运行期只用解析好的地址:本机是 127.0.0.1,
// K8s 上是 Service 的 ClusterIP,Service 存在期间不变。在主线程上多出来的只有每批的 protobuf 编码,量随 span 数线性增长。
//
// 生命周期:构造、Start、BeginShutdown、Finished、析构都只在这个 loop 的线程上调用。连接是 HTTP/1.1 长连接,对端关了就在
// 下一批重连;连接对象只在排队任务里析构,muduo 回调只绑 weak_ptr(AGENTS.md §11.7)。停机不阻塞:BeginShutdown 开始有界冲刷,
// 调用方按 Finished() 轮询(Node 在 FinalizeTeardownInLoop 的让轮里等它),之后再析构。

namespace tracing {

struct OtlpSpanExporterOptions {
    OtlpHttpEndpoint endpoint;           // 请求的 Host 头与路径
    muduo::net::InetAddress address;     // 连哪(ResolveOtlpEndpoint 的结果)
    OtlpResource resource;
    size_t maxExportBatchSize{kDefaultSpanExportBatchSize};
    std::chrono::milliseconds scheduleDelay{5000};
    std::chrono::milliseconds exportTimeout{30000};
    std::chrono::milliseconds shutdownTimeout{2000};
    std::chrono::milliseconds initialBackoff{1000};
    std::chrono::milliseconds maxBackoff{60000};
};

// 把 endpoint.host 解析成地址:规范的点分十进制 IPv4 直接用;主机名走 InetAddress::resolve(阻塞)。
// 只在 loop 开转之前调用(Node::StartSpanExporter);解析不了返回 nullopt。
std::optional<muduo::net::InetAddress> ResolveOtlpEndpoint(const OtlpHttpEndpoint& endpoint);

// 导出结果的计数。生产用进程唯一的一份(ProcessSpanExportStats),/metrics 读它;单测各用各的。
struct SpanExportStats {
    std::atomic<uint64_t> exportedSpans{0};
    std::atomic<uint64_t> failedSpans{0};     // 发了但失败(连不上、超时、非 2xx、响应不合法)或退避期间丢掉的
    std::atomic<uint64_t> exportedBatches{0};
    std::atomic<uint64_t> failedBatches{0};
};

SpanExportStats& ProcessSpanExportStats();

// 把进程队列与进程计数登记进 GlobalMetricsRegistry(采集器名 span_export),重复调用只登记一次:
//   spans_exported_total、spans_dropped_total{reason="queue_full"|"export_failed"|"shutdown"}、
//   span_export_batches_total{result="ok"|"failed"}、span_export_queue_size。
void RegisterSpanExportMetrics();

class OtlpSpanExporter {
public:
    OtlpSpanExporter(muduo::net::EventLoop& loop, SpanExportQueue& queue, SpanExportStats& stats,
                     OtlpSpanExporterOptions options);
    // 还没收尾就析构:剩下的按停机丢弃计数,连接排进队列关掉,不再冲刷。
    ~OtlpSpanExporter();

    OtlpSpanExporter(const OtlpSpanExporter&) = delete;
    OtlpSpanExporter& operator=(const OtlpSpanExporter&) = delete;

    // 开始按周期导出。只调一次。不装进程的 span 去处:那由调用方决定(Node 在 Start 之后
    // InstallSpanSink(&ProcessSpanExportQueue()))。
    void Start();

    // 开始停机冲刷:不再按周期发,在 shutdownTimeout 内把队列里剩下的发出去。幂等、不阻塞。调用前应先摘掉进程的 span 去处,
    // 否则停下之后还在入队的 span 只会留在队列里(不再导出,也不计数)。
    void BeginShutdown();

    // 停机冲刷已结束(发完、超时或没有可发的)。之后析构不会再丢 span。
    bool Finished() const;

    class Impl;

private:
    std::shared_ptr<Impl> impl_;
};

} // namespace tracing
