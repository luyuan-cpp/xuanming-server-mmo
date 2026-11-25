#include "core/tracing/otlp_span_exporter.h"

#include <algorithm>
#include <functional>
#include <mutex>
#include <optional>
#include <string>
#include <utility>
#include <vector>

#include "metrics/metrics_registry.h"
#include "muduo/base/Logging.h"
#include "muduo/net/Buffer.h"
#include "muduo/net/EventLoop.h"
#include "muduo/net/TcpClient.h"
#include "muduo_grpc/address.h"

namespace tracing {

namespace {

using SteadyClock = std::chrono::steady_clock;

double ToSeconds(std::chrono::milliseconds duration)
{
    return static_cast<double>(duration.count()) / 1000.0;
}

// 一条到采集器的 TCP 连接(一个对象只对应一次连接,重连就换新对象,同 muduo_grpc::Http2Transport)。
// 不 enableRetry:重连与退避由导出器决定。muduo 的回调只绑 weak_ptr<自己>(AGENTS.md §11.7),
// 对象析构前显式 forceClose,不依赖 ~TcpClient 的 use_count 判断。只在 loop 线程上使用与析构(析构排进 queueInLoop)。
class HttpConnection : public std::enable_shared_from_this<HttpConnection> {
public:
    struct Callbacks {
        std::function<void(uint64_t id)> onUp;
        std::function<void(uint64_t id)> onDown;
        std::function<void(uint64_t id, muduo::net::Buffer* buffer)> onBytes;
        std::function<void(uint64_t id, int savedErrno)> onConnectFailed;
    };

    HttpConnection(muduo::net::EventLoop& loop, const muduo::net::InetAddress& address, uint64_t id, Callbacks callbacks)
        : loop(loop),
          client(&loop, address, "otlp-span-export#" + std::to_string(id)),
          id(id),
          callbacks(std::move(callbacks))
    {
    }

    ~HttpConnection()
    {
        const auto conn = client.connection();
        if (conn) {
            conn->forceClose();
        }
    }

    HttpConnection(const HttpConnection&) = delete;
    HttpConnection& operator=(const HttpConnection&) = delete;

    void Connect()
    {
        if (connectStarted) {
            return;
        }
        connectStarted = true;
        std::weak_ptr<HttpConnection> weakSelf = weak_from_this();
        client.setConnectionCallback([weakSelf](const muduo::net::TcpConnectionPtr& conn) {
            if (const auto self = weakSelf.lock()) {
                if (conn->connected()) {
                    self->callbacks.onUp(self->id);
                } else {
                    self->callbacks.onDown(self->id);
                }
            }
        });
        client.setMessageCallback(
            [weakSelf](const muduo::net::TcpConnectionPtr&, muduo::net::Buffer* buffer, muduo::Timestamp) {
                if (const auto self = weakSelf.lock()) {
                    self->callbacks.onBytes(self->id, buffer);
                    return;
                }
                buffer->retrieveAll();
            });
        // muduo LOCAL DELTA 的连接失败回调:先停掉 Connector 的内部重试,上报排到下一轮(回调可能正处在 Connector 自己的
        // 事件栈上,也可能在 connect() 里同步发生),做法同 muduo_grpc::Http2Transport::HandleConnectFailed。
        client.setConnectFailedCallback([weakSelf](int savedErrno) {
            const auto self = weakSelf.lock();
            if (!self) {
                return;
            }
            self->client.stop();
            self->loop.queueInLoop([weakSelf, savedErrno] {
                if (const auto again = weakSelf.lock()) {
                    again->callbacks.onConnectFailed(again->id, savedErrno);
                }
            });
        });
        client.connect();
    }

    bool Send(const std::string& bytes)
    {
        const auto conn = client.connection();
        if (!conn || !conn->connected()) {
            return false;
        }
        conn->send(bytes.data(), static_cast<int>(bytes.size()));
        return true;
    }

    // 关掉连接(还没连上就停掉 Connector)。关闭结果经 onDown 回来;调用方随后把本对象排进 queueInLoop 释放。
    void Close()
    {
        const auto conn = client.connection();
        if (conn) {
            conn->forceClose();
            return;
        }
        if (connectStarted) {
            client.stop();
        }
    }

    bool Connected() const
    {
        const auto conn = client.connection();
        return conn && conn->connected();
    }

private:
    muduo::net::EventLoop& loop; // 借用节点的 loop,连接对象在它析构之前释放
    muduo::net::TcpClient client;
    uint64_t id;
    Callbacks callbacks;
    bool connectStarted{false};
};

} // namespace

std::optional<muduo::net::InetAddress> ResolveOtlpEndpoint(const OtlpHttpEndpoint& endpoint)
{
    // muduo 的 InetAddress 遇到非法 IP 只打一行日志并留下 0.0.0.0,所以先按规范 IPv4 校验(muduo_grpc/address.h)。
    if (muduo_grpc::IsCanonicalIpv4(endpoint.host)) {
        return muduo::net::InetAddress(endpoint.host, endpoint.port);
    }
    muduo::net::InetAddress address(endpoint.port);
    if (!muduo::net::InetAddress::resolve(endpoint.host, &address)) {
        return std::nullopt;
    }
    return address;
}

class OtlpSpanExporter::Impl : public std::enable_shared_from_this<OtlpSpanExporter::Impl> {
public:
    Impl(muduo::net::EventLoop& loop, SpanExportQueue& queue, SpanExportStats& stats, OtlpSpanExporterOptions options)
        : loop(loop), queue(queue), stats(stats), options(std::move(options)), url(this->options.endpoint.ToUrl())
    {
        this->options.maxExportBatchSize = std::max<size_t>(this->options.maxExportBatchSize, 1);
    }

    void Start()
    {
        this->loop.assertInLoopThread();
        if (started) {
            return;
        }
        started = true;
        std::weak_ptr<Impl> weakSelf = weak_from_this();
        // 周期定时器只绑 weak:Impl 先没了就是空操作。
        periodicTimer = this->loop.runEvery(ToSeconds(options.scheduleDelay), [weakSelf] {
            if (const auto self = weakSelf.lock()) {
                self->TryExport();
            }
        });
        periodicTimerArmed = true;
        // 攒够一批时提前发。回调在 Submit 的线程上、持队列锁执行,只做一次 queueInLoop(Kafka 的 poll 线程就地派发时也安全);
        // 停机摘掉回调之后 loop 不会再被碰到,摘回调与 Submit 持同一把锁。
        muduo::net::EventLoop* loop = &this->loop;
        queue.SetBatchReadyCallback([loop, weakSelf] {
            loop->queueInLoop([weakSelf] {
                if (const auto self = weakSelf.lock()) {
                    self->TryExport();
                }
            });
        });
    }

    void BeginShutdown()
    {
        loop.assertInLoopThread();
        if (shuttingDown || finished) {
            return;
        }
        shuttingDown = true;
        shutdownDeadline = SteadyClock::now() + options.shutdownTimeout;
        queue.SetBatchReadyCallback(nullptr);
        CancelPeriodicTimer();
        std::weak_ptr<Impl> weakSelf = weak_from_this();
        shutdownTimer = loop.runAfter(ToSeconds(options.shutdownTimeout), [weakSelf] {
            if (const auto self = weakSelf.lock()) {
                self->shutdownTimerArmed = false;
                self->OnShutdownDeadline();
            }
        });
        shutdownTimerArmed = true;
        ContinueShutdown();
    }

    bool Finished() const { return finished; }

    // 析构前收口(没走完停机冲刷就析构时):不再发,剩下的按停机丢弃计数。
    void StopNow()
    {
        if (!finished) {
            queue.SetBatchReadyCallback(nullptr);
            if (phase != Phase::kIdle && inFlightSpans > 0) {
                stats.failedSpans.fetch_add(inFlightSpans, std::memory_order_relaxed);
                stats.failedBatches.fetch_add(1, std::memory_order_relaxed);
                inFlightSpans = 0;
            }
            phase = Phase::kIdle;
            FinishShutdown();
        }
    }

private:
    enum class Phase {
        kIdle,
        kConnecting,
        kAwaitingResponse,
    };

    void TryExport()
    {
        if (finished || phase != Phase::kIdle) {
            return;   // 同一时刻最多一个在途请求;它结束时会自己复核还要不要接着发
        }
        if (!shuttingDown && SteadyClock::now() < backoffUntil) {
            DropOneBatchWhileBackingOff();
            return;
        }
        if (queue.Size() == 0) {
            if (shuttingDown) {
                FinishShutdown();
            }
            return;   // 空闲的周期:不建连
        }
        batch.clear();
        inFlightSpans = queue.PopBatch(batch, options.maxExportBatchSize);
        const std::string body = EncodeExportTraceServiceRequest(options.resource, batch);
        batch.clear();
        if (body.empty()) {
            FailInFlight("cannot serialize OTLP request", /*backoff=*/false);
            return;
        }
        request = BuildOtlpHttpRequest(options.endpoint, body);
        ArmExportDeadline();

        if (connection && connection->Connected()) {
            SendInFlight();
            return;
        }
        ReleaseConnection();
        std::weak_ptr<Impl> weakSelf = weak_from_this();
        HttpConnection::Callbacks callbacks;
        callbacks.onUp = [weakSelf](uint64_t id) {
            if (const auto self = weakSelf.lock()) self->OnUp(id);
        };
        callbacks.onDown = [weakSelf](uint64_t id) {
            if (const auto self = weakSelf.lock()) self->OnDown(id);
        };
        callbacks.onBytes = [weakSelf](uint64_t id, muduo::net::Buffer* buffer) {
            if (const auto self = weakSelf.lock()) {
                self->OnBytes(id, buffer);
            } else {
                buffer->retrieveAll();
            }
        };
        callbacks.onConnectFailed = [weakSelf](uint64_t id, int savedErrno) {
            if (const auto self = weakSelf.lock()) self->OnConnectFailed(id, savedErrno);
        };
        connectionId = nextConnectionId++;
        connection = std::make_shared<HttpConnection>(loop, options.address, connectionId, std::move(callbacks));
        phase = Phase::kConnecting;
        connection->Connect();
    }

    void SendInFlight()
    {
        response.clear();
        phase = Phase::kAwaitingResponse;
        if (!connection->Send(request)) {
            FailInFlight("connection lost before send", /*backoff=*/true);
        }
    }

    void OnUp(uint64_t id)
    {
        if (id != connectionId || !connection) {
            return;
        }
        if (phase == Phase::kConnecting) {
            SendInFlight();
        }
    }

    // 下面几个回调都先处置连接、再结算在途的一批:结算可能接着发下一批,那时不能再碰这条旧连接。
    void OnDown(uint64_t id)
    {
        if (id != connectionId || !connection) {
            return;
        }
        const Phase phase = this->phase;
        // 正文到连接关闭为止的响应:关闭就是完整。
        const HttpResponseParse parsed =
            phase == Phase::kAwaitingResponse ? ParseHttpResponse(response) : HttpResponseParse{};
        ReleaseConnection();
        if (phase == Phase::kAwaitingResponse) {
            if (parsed.headersComplete && parsed.bodyUntilClose) {
                CompleteInFlight(parsed.statusCode);
            } else {
                FailInFlight("connection closed before the response", /*backoff=*/true);
            }
        } else if (phase == Phase::kConnecting) {
            FailInFlight("connection closed while connecting", /*backoff=*/true);
        }
    }

    void OnConnectFailed(uint64_t id, int savedErrno)
    {
        if (id != connectionId || !connection) {
            return;
        }
        ReleaseConnection();
        if (phase == Phase::kConnecting) {
            lastErrno = savedErrno;
            FailInFlight("connect failed", /*backoff=*/true);
        }
    }

    void OnBytes(uint64_t id, muduo::net::Buffer* buffer)
    {
        if (id != connectionId || !connection || phase != Phase::kAwaitingResponse) {
            // 没在等响应时收到的字节:旧连接的残余或对端多发,连接不能再用。
            buffer->retrieveAll();
            if (id == connectionId && connection) {
                CloseConnection();
            }
            return;
        }
        response.append(buffer->peek(), buffer->readableBytes());
        buffer->retrieveAll();
        const HttpResponseParse parsed = ParseHttpResponse(response);
        switch (parsed.state) {
        case HttpResponseParse::State::kNeedMore:
            return;
        case HttpResponseParse::State::kError:
            CloseConnection();
            FailInFlight("malformed HTTP response", /*backoff=*/false);
            return;
        case HttpResponseParse::State::kComplete:
            break;
        }
        // 响应之后还多出字节、或对端要求关闭:这条连接不再复用。
        if (!parsed.keepAlive || parsed.consumed != response.size()) {
            CloseConnection();
        }
        CompleteInFlight(parsed.statusCode);
    }

    void ArmExportDeadline()
    {
        CancelExportDeadline();
        auto timeout = options.exportTimeout;
        if (shuttingDown) {
            const auto remaining = std::chrono::duration_cast<std::chrono::milliseconds>(shutdownDeadline - SteadyClock::now());
            timeout = std::max(std::min(timeout, remaining), std::chrono::milliseconds(1));
        }
        const uint64_t generation = ++exportGeneration;
        std::weak_ptr<Impl> weakSelf = weak_from_this();
        exportDeadline = loop.runAfter(ToSeconds(timeout), [weakSelf, generation] {
            if (const auto self = weakSelf.lock()) {
                self->OnExportDeadline(generation);
            }
        });
        exportDeadlineArmed = true;
    }

    void CancelExportDeadline()
    {
        if (exportDeadlineArmed) {
            loop.cancel(exportDeadline);
            exportDeadlineArmed = false;
        }
    }

    void OnExportDeadline(uint64_t generation)
    {
        if (generation != exportGeneration) {
            return;
        }
        exportDeadlineArmed = false;
        if (phase == Phase::kIdle) {
            return;
        }
        CloseConnection();
        FailInFlight("export timed out", /*backoff=*/true);
    }

    void CompleteInFlight(int statusCode)
    {
        CancelExportDeadline();
        phase = Phase::kIdle;
        const uint64_t spans = inFlightSpans;
        inFlightSpans = 0;
        request.clear();
        if (statusCode >= 200 && statusCode < 300) {
            stats.exportedSpans.fetch_add(spans, std::memory_order_relaxed);
            stats.exportedBatches.fetch_add(1, std::memory_order_relaxed);
            consecutiveTransportFailures = 0;
            backoffUntil = SteadyClock::time_point{};
            if (failing) {
                LOG_INFO << "[Tracing] OTLP span export to " << url << " recovered after " << failedBatchesWhileFailing
                         << " failed batch(es)";
                failing = false;
                failedBatchesWhileFailing = 0;
            }
        } else {
            // 采集器拒收(4xx / 5xx):这一批丢掉,不退避(对端是通的)。
            RecordFailedBatch(spans, "rejected with HTTP status " + std::to_string(statusCode));
        }
        AfterInFlightSettled();
    }

    void FailInFlight(const std::string& reason, bool backoff)
    {
        CancelExportDeadline();
        phase = Phase::kIdle;
        const uint64_t spans = inFlightSpans;
        inFlightSpans = 0;
        request.clear();
        std::string detail = reason;
        if (lastErrno != 0) {
            detail += " (errno " + std::to_string(lastErrno) + ")";
            lastErrno = 0;
        }
        if (spans > 0) {
            RecordFailedBatch(spans, detail);
        }
        if (backoff && !shuttingDown) {
            ++consecutiveTransportFailures;
            auto delay = options.initialBackoff;
            for (uint32_t i = 1; i < consecutiveTransportFailures && delay < options.maxBackoff; ++i) {
                delay *= 2;
            }
            delay = std::min(delay, options.maxBackoff);
            backoffUntil = SteadyClock::now() + delay;
        }
        AfterInFlightSettled();
    }

    // 记一批失败:状态从正常转成失败时打一行 WARN,之后只计数,恢复时打一行 INFO(不按批刷日志)。
    void RecordFailedBatch(uint64_t spans, const std::string& reason)
    {
        stats.failedSpans.fetch_add(spans, std::memory_order_relaxed);
        stats.failedBatches.fetch_add(1, std::memory_order_relaxed);
        ++failedBatchesWhileFailing;
        if (!failing) {
            failing = true;
            LOG_WARN << "[Tracing] OTLP span export to " << url << " failed: " << reason << "; dropped " << spans
                     << " span(s). Batches are dropped (not retried) until the collector accepts again";
        }
    }

    void DropOneBatchWhileBackingOff()
    {
        std::vector<SpanRecord> dropped;
        const size_t count = queue.PopBatch(dropped, options.maxExportBatchSize);
        if (count > 0) {
            stats.failedSpans.fetch_add(count, std::memory_order_relaxed);
        }
    }

    void AfterInFlightSettled()
    {
        // 接下来的动作一律排到下一轮:这里可能正处在连接的回调栈上,不在这里同步建新连接或再发一批。
        std::weak_ptr<Impl> weakSelf = weak_from_this();
        if (shuttingDown) {
            loop.queueInLoop([weakSelf] {
                if (const auto self = weakSelf.lock()) {
                    self->ContinueShutdown();
                }
            });
            return;
        }
        // 还够一批就接着发,不等下一个周期(BatchSpanProcessor 的语义)。
        if (queue.Size() >= options.maxExportBatchSize) {
            loop.queueInLoop([weakSelf] {
                if (const auto self = weakSelf.lock()) {
                    self->TryExport();
                }
            });
        }
    }

    void ContinueShutdown()
    {
        if (finished || phase != Phase::kIdle) {
            return;
        }
        if (SteadyClock::now() >= shutdownDeadline) {
            FinishShutdown();
            return;
        }
        TryExport();   // 队列空了会走到 FinishShutdown
    }

    void OnShutdownDeadline()
    {
        if (finished) {
            return;
        }
        if (phase != Phase::kIdle) {
            CancelExportDeadline();
            const uint64_t spans = inFlightSpans;
            inFlightSpans = 0;
            phase = Phase::kIdle;
            if (spans > 0) {
                stats.failedSpans.fetch_add(spans, std::memory_order_relaxed);
                stats.failedBatches.fetch_add(1, std::memory_order_relaxed);
            }
        }
        FinishShutdown();
    }

    void FinishShutdown()
    {
        if (finished) {
            return;
        }
        finished = true;
        CancelExportDeadline();
        CancelPeriodicTimer();
        if (shutdownTimerArmed) {
            loop.cancel(shutdownTimer);
            shutdownTimerArmed = false;
        }
        const size_t discarded = queue.DiscardAll();
        if (discarded > 0) {
            LOG_WARN << "[Tracing] span exporter stopped with " << discarded << " span(s) not exported";
        }
        // 连接对象排在队尾析构(不在它自己的回调栈上);~TcpClient 再排的 forceCloseInLoop / connectDestroyed 由
        // 节点拆除末尾的两跳 queueInLoop 排空(AGENTS.md §11.7)。
        CloseConnection();
    }

    void CloseConnection()
    {
        if (connection) {
            connection->Close();
            ReleaseConnection();
        }
    }

    // 不在连接自己的回调栈上析构:移进排队任务。之后这条连接的回调按 connectionId_ 认出是旧的,一律忽略。
    void ReleaseConnection()
    {
        if (!connection) {
            return;
        }
        connectionId = 0;
        loop.queueInLoop([released = std::move(connection)]() mutable { released.reset(); });
        connection.reset();
    }

    void CancelPeriodicTimer()
    {
        if (periodicTimerArmed) {
            loop.cancel(periodicTimer);
            periodicTimerArmed = false;
        }
    }

    muduo::net::EventLoop& loop; // 借用节点的 loop;Impl 只在它的线程上使用,先于它析构
    SpanExportQueue& queue;
    SpanExportStats& stats;
    OtlpSpanExporterOptions options;
    const std::string url;

    bool started{false};
    Phase phase{Phase::kIdle};
    std::vector<SpanRecord> batch;
    std::string request;
    std::string response;
    uint64_t inFlightSpans{0};
    std::shared_ptr<HttpConnection> connection;
    uint64_t connectionId{0};
    uint64_t nextConnectionId{1};
    int lastErrno{0};

    muduo::net::TimerId periodicTimer;
    bool periodicTimerArmed{false};
    muduo::net::TimerId exportDeadline;
    bool exportDeadlineArmed{false};
    uint64_t exportGeneration{0};

    uint32_t consecutiveTransportFailures{0};
    SteadyClock::time_point backoffUntil{};
    bool failing{false};
    uint64_t failedBatchesWhileFailing{0};

    bool shuttingDown{false};
    bool finished{false};
    SteadyClock::time_point shutdownDeadline{};
    muduo::net::TimerId shutdownTimer;
    bool shutdownTimerArmed{false};
};

OtlpSpanExporter::OtlpSpanExporter(muduo::net::EventLoop& loop, SpanExportQueue& queue, SpanExportStats& stats,
                                   OtlpSpanExporterOptions options)
    : impl(std::make_shared<Impl>(loop, queue, stats, std::move(options)))
{
}

OtlpSpanExporter::~OtlpSpanExporter()
{
    impl->StopNow();
}

void OtlpSpanExporter::Start()
{
    impl->Start();
}

void OtlpSpanExporter::BeginShutdown()
{
    impl->BeginShutdown();
}

bool OtlpSpanExporter::Finished() const
{
    return impl->Finished();
}

SpanExportStats& ProcessSpanExportStats()
{
    // 与 ProcessSpanExportQueue 同理有意不析构:/metrics 的采集回调读它,不能先于最后一次抓取被静态析构拆掉。
    static SpanExportStats* const stats = new SpanExportStats();
    return *stats;
}

namespace {

void CollectSpanExportMetrics(prometheus_text::Writer& writer)
{
    const SpanExportQueue& queue = ProcessSpanExportQueue();
    const SpanExportStats& stats = ProcessSpanExportStats();
    if (writer.BeginFamily("spans_exported_total", "Spans accepted by the OTLP collector.",
                           prometheus_text::MetricType::kCounter)) {
        writer.Sample({}, stats.exportedSpans.load(std::memory_order_relaxed));
    }
    if (writer.BeginFamily("spans_dropped_total",
                           "Spans that never reached the collector: queue_full = the bounded queue was full, "
                           "export_failed = the batch failed or was dropped while the collector was unreachable, "
                           "shutdown = left in the queue when the exporter stopped.",
                           prometheus_text::MetricType::kCounter)) {
        writer.Sample({{"reason", "queue_full"}}, queue.DroppedQueueFull());
        writer.Sample({{"reason", "export_failed"}}, stats.failedSpans.load(std::memory_order_relaxed));
        writer.Sample({{"reason", "shutdown"}}, queue.DroppedOnShutdown());
    }
    if (writer.BeginFamily("span_export_batches_total", "OTLP export requests by result.",
                           prometheus_text::MetricType::kCounter)) {
        writer.Sample({{"result", "ok"}}, stats.exportedBatches.load(std::memory_order_relaxed));
        writer.Sample({{"result", "failed"}}, stats.failedBatches.load(std::memory_order_relaxed));
    }
    if (writer.BeginFamily("span_export_queue_size", "Spans waiting in the export queue.",
                           prometheus_text::MetricType::kGauge)) {
        writer.Sample({}, queue.Size());
    }
}

} // namespace

void RegisterSpanExportMetrics()
{
    static std::once_flag registered;
    std::call_once(registered, [] {
        if (!GlobalMetricsRegistry().Register("span_export", &CollectSpanExportMetrics)) {
            LOG_ERROR << "[Metrics] collector span_export is already registered";
        }
    });
}

} // namespace tracing
