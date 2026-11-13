#pragma once

#include <atomic>
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <memory>

#include "core/tracing/otlp_http.h"
#include "core/tracing/otlp_trace_encoder.h"
#include "core/tracing/span_export_queue.h"

namespace muduo::net
{
class EventLoop;
class EventLoopThread;
}

// span 导出线程(docs/design/distributed-tracing.md §10.3):从 SpanExportQueue 取 span,攒批编成 OTLP 的
// ExportTraceServiceRequest,经 OTLP/HTTP(protobuf)发给本机 / 本节点的 Alloy。照 OTel SDK 规范的 BatchSpanProcessor:
//   * 每进程一个专用线程(muduo EventLoopThread),线程数固定;业务线程只往队列里放,永不等导出;
//   * 每 scheduleDelay(5 秒)发一批,队列攒够一批(512)时提前发;同一时刻最多一个在途请求;
//   * 一次导出(连接 + 发送 + 等响应)超过 exportTimeout(30 秒)就算失败;
//   * 失败(连不上、超时、非 2xx)就丢掉这一批并计数,不重试、不回填队列。连不上时按 1 秒起、翻倍、封顶 60 秒退避:
//     退避期间每次该发的时候丢一批最旧的,队列不会一直顶满,也不会每 5 秒刷一遍 muduo 的连接日志;
//   * 停机时在 shutdownTimeout(2 秒)内把剩下的发出去,发不完的丢掉并计数。
// 不引入 OpenTelemetry C++ SDK(它的 OTLP/gRPC 导出器会拉起 gRPC 运行时线程,与「运行期不起 gRPC 线程」冲突)。
//
// 连接是 HTTP/1.1 长连接,对端关了就在下一批重连。地址是主机名时每次建连在导出线程上解析一次(阻塞,只卡导出线程);
// 停机冲刷不做解析,没连着就直接丢。
//
// 生命周期:Start / Shutdown / 析构都在同一个非导出线程上调用(Node 的 loop 线程)。Shutdown 在导出线程上收尾:
// 连接与定时器都在那里释放,两跳 queueInLoop 之后才放行(AGENTS.md §11.7);等不到收尾(导出线程卡住)时
// 有意泄漏线程与内部对象,不在别的线程上拆它们。

namespace tracing {

struct OtlpSpanExporterOptions {
    OtlpHttpEndpoint endpoint;
    OtlpResource resource;
    size_t maxExportBatchSize{kDefaultSpanExportBatchSize};
    std::chrono::milliseconds scheduleDelay{5000};
    std::chrono::milliseconds exportTimeout{30000};
    std::chrono::milliseconds shutdownTimeout{2000};
    std::chrono::milliseconds initialBackoff{1000};
    std::chrono::milliseconds maxBackoff{60000};
};

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
    OtlpSpanExporter(SpanExportQueue& queue, SpanExportStats& stats, OtlpSpanExporterOptions options);
    ~OtlpSpanExporter();

    OtlpSpanExporter(const OtlpSpanExporter&) = delete;
    OtlpSpanExporter& operator=(const OtlpSpanExporter&) = delete;

    // 起导出线程并开始按周期导出。只调一次。不装进程的 span 去处:那由调用方决定(Node 在 Start 之后
    // InstallSpanSink(&ProcessSpanExportQueue()))。
    void Start();

    // 在 shutdownTimeout 内把队列里剩下的发出去,然后停线程。幂等。调用前应先摘掉进程的 span 去处,
    // 否则停下之后还在入队的 span 只会留在队列里(不再导出,也不计数)。
    void Shutdown();

    class Impl;
    struct Latch;

private:
    SpanExportQueue& queue_;
    SpanExportStats& stats_;
    OtlpSpanExporterOptions options_;
    std::unique_ptr<muduo::net::EventLoopThread> thread_;
    muduo::net::EventLoop* loop_{nullptr};   // 导出线程的 loop,随 thread_ 存活
    std::shared_ptr<Impl> impl_;
    std::shared_ptr<Latch> latch_;           // 收尾完成信号:导出线程在最后一跳置位,Shutdown 等它
    bool started_{false};
    bool stopped_{false};
};

} // namespace tracing
