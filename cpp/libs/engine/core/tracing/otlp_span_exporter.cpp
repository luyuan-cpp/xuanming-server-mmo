#include "core/tracing/otlp_span_exporter.h"

#include <algorithm>
#include <condition_variable>
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
#include "muduo/net/EventLoopThread.h"
#include "muduo/net/InetAddress.h"
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
// 对象析构前显式 forceClose,不依赖 ~TcpClient 的 use_count 判断。只在导出线程上使用与析构(析构排进 queueInLoop)。
class HttpConnection : public std::enable_shared_from_this<HttpConnection> {
public:
    struct Callbacks {
        std::function<void(uint64_t id)> onUp;
        std::function<void(uint64_t id)> onDown;
        std::function<void(uint64_t id, muduo::net::Buffer* buffer)> onBytes;
        std::function<void(uint64_t id, int savedErrno)> onConnectFailed;
    };

    HttpConnection(muduo::net::EventLoop* loop, const muduo::net::InetAddress& address, uint64_t id, Callbacks callbacks)
        : loop_(loop),
          client_(loop, address, "otlp-span-export#" + std::to_string(id)),
          id_(id),
          callbacks_(std::move(callbacks))
    {
    }

    ~HttpConnection()
    {
        const auto conn = client_.connection();
        if (conn) {
            conn->forceClose();
        }
    }

    HttpConnection(const HttpConnection&) = delete;
    HttpConnection& operator=(const HttpConnection&) = delete;

    void Connect()
    {
        if (connectStarted_) {
            return;
        }
        connectStarted_ = true;
        std::weak_ptr<HttpConnection> weakSelf = weak_from_this();
        client_.setConnectionCallback([weakSelf](const muduo::net::TcpConnectionPtr& conn) {
            if (const auto self = weakSelf.lock()) {
                if (conn->connected()) {
                    self->callbacks_.onUp(self->id_);
                } else {
                    self->callbacks_.onDown(self->id_);
                }
            }
        });
        client_.setMessageCallback(
            [weakSelf](const muduo::net::TcpConnectionPtr&, muduo::net::Buffer* buffer, muduo::Timestamp) {
                if (const auto self = weakSelf.lock()) {
                    self->callbacks_.onBytes(self->id_, buffer);
                    return;
                }
                buffer->retrieveAll();
            });
        // muduo LOCAL DELTA 的连接失败回调:先停掉 Connector 的内部重试,上报排到下一轮(回调可能正处在 Connector 自己的
        // 事件栈上,也可能在 connect() 里同步发生),做法同 muduo_grpc::Http2Transport::HandleConnectFailed。
        client_.setConnectFailedCallback([weakSelf](int savedErrno) {
            const auto self = weakSelf.lock();
            if (!self) {
                return;
            }
            self->client_.stop();
            self->loop_->queueInLoop([weakSelf, savedErrno] {
                if (const auto again = weakSelf.lock()) {
                    again->callbacks_.onConnectFailed(again->id_, savedErrno);
                }
            });
        });
        client_.connect();
    }

    bool Send(const std::string& bytes)
    {
        const auto conn = client_.connection();
        if (!conn || !conn->connected()) {
            return false;
        }
        conn->send(bytes.data(), static_cast<int>(bytes.size()));
        return true;
    }

    // 关掉连接(还没连上就停掉 Connector)。关闭结果经 onDown 回来;调用方随后把本对象排进 queueInLoop 释放。
    void Close()
    {
        const auto conn = client_.connection();
        if (conn) {
            conn->forceClose();
            return;
        }
        if (connectStarted_) {
            client_.stop();
        }
    }

    bool Connected() const
    {
        const auto conn = client_.connection();
        return conn && conn->connected();
    }

private:
    muduo::net::EventLoop* loop_;
    muduo::net::TcpClient client_;
    uint64_t id_;
    Callbacks callbacks_;
    bool connectStarted_{false};
};

} // namespace

// 停机收尾的完成信号:导出线程在收尾的最后一跳里置位,调用 Shutdown 的线程等它。
struct OtlpSpanExporter::Latch {
    std::mutex mutex;
    std::condition_variable cv;
    bool done{false};

    void Signal()
    {
        {
            std::lock_guard<std::mutex> lock(mutex);
            done = true;
        }
        cv.notify_all();
    }

    bool WaitFor(std::chrono::milliseconds timeout)
    {
        std::unique_lock<std::mutex> lock(mutex);
        return cv.wait_for(lock, timeout, [this] { return done; });
    }
};

class OtlpSpanExporter::Impl : public std::enable_shared_from_this<OtlpSpanExporter::Impl> {
public:
    Impl(muduo::net::EventLoop* loop, SpanExportQueue& queue, SpanExportStats& stats, OtlpSpanExporterOptions options,
         std::shared_ptr<OtlpSpanExporter::Latch> latch)
        : loop_(loop), queue_(queue), stats_(stats), options_(std::move(options)), latch_(std::move(latch)),
          url_(options_.endpoint.ToUrl())
    {
        options_.maxExportBatchSize = std::max<size_t>(options_.maxExportBatchSize, 1);
    }

    void StartInLoop()
    {
        std::weak_ptr<Impl> weakSelf = weak_from_this();
        // 周期定时器只绑 weak:Impl 先没了就是空操作。
        periodicTimer_ = loop_->runEvery(ToSeconds(options_.scheduleDelay), [weakSelf] {
            if (const auto self = weakSelf.lock()) {
                self->TryExport();
            }
        });
        periodicTimerArmed_ = true;
        // 攒够一批时提前发。回调在 Submit 的线程上、持队列锁执行,只做一次 queueInLoop;摘回调(停机)之后 loop_ 不会再被碰到。
        muduo::net::EventLoop* loop = loop_;
        queue_.SetBatchReadyCallback([loop, weakSelf] {
            loop->queueInLoop([weakSelf] {
                if (const auto self = weakSelf.lock()) {
                    self->TryExport();
                }
            });
        });
    }

    void BeginShutdownInLoop()
    {
        if (shuttingDown_) {
            return;
        }
        shuttingDown_ = true;
        shutdownDeadline_ = SteadyClock::now() + options_.shutdownTimeout;
        queue_.SetBatchReadyCallback(nullptr);
        CancelPeriodicTimer();
        std::weak_ptr<Impl> weakSelf = weak_from_this();
        shutdownTimer_ = loop_->runAfter(ToSeconds(options_.shutdownTimeout), [weakSelf] {
            if (const auto self = weakSelf.lock()) {
                self->shutdownTimerArmed_ = false;
                self->OnShutdownDeadline();
            }
        });
        shutdownTimerArmed_ = true;
        ContinueShutdown();
    }

private:
    enum class Phase {
        kIdle,
        kConnecting,
        kAwaitingResponse,
    };

    void TryExport()
    {
        if (finished_ || phase_ != Phase::kIdle) {
            return;   // 同一时刻最多一个在途请求;它结束时会自己复核还要不要接着发
        }
        if (!shuttingDown_ && SteadyClock::now() < backoffUntil_) {
            DropOneBatchWhileBackingOff();
            return;
        }
        if (queue_.Size() == 0) {
            if (shuttingDown_) {
                FinishShutdown();
            }
            return;   // 空闲的周期:不解析、不建连
        }
        const bool reuse = connection_ && connection_->Connected();
        std::optional<muduo::net::InetAddress> address;
        if (!reuse) {
            ReleaseConnection();
            // 停机冲刷不做 DNS 解析(阻塞可能拖过停机时限):主机是 IPv4 字面量才在停机时新建连接,否则剩下的按停机丢弃计数。
            address = ResolveEndpoint(/*allowDns=*/!shuttingDown_);
            if (!address) {
                if (shuttingDown_) {
                    FinishShutdown();
                    return;
                }
                inFlightSpans_ = queue_.PopBatch(batch_, options_.maxExportBatchSize);
                batch_.clear();
                FailInFlight("cannot resolve " + options_.endpoint.host, /*backoff=*/true);
                return;
            }
        }
        batch_.clear();
        inFlightSpans_ = queue_.PopBatch(batch_, options_.maxExportBatchSize);
        request_ = BuildOtlpHttpRequest(options_.endpoint, EncodeExportTraceServiceRequest(options_.resource, batch_));
        batch_.clear();
        ArmExportDeadline();

        if (reuse) {
            SendInFlight();
            return;
        }
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
        connection_ = std::make_shared<HttpConnection>(loop_, *address, nextConnectionId_++, std::move(callbacks));
        connectionId_ = nextConnectionId_ - 1;
        phase_ = Phase::kConnecting;
        connection_->Connect();
    }

    void SendInFlight()
    {
        response_.clear();
        phase_ = Phase::kAwaitingResponse;
        if (!connection_->Send(request_)) {
            FailInFlight("connection lost before send", /*backoff=*/true);
        }
    }

    void OnUp(uint64_t id)
    {
        if (id != connectionId_ || !connection_) {
            return;
        }
        if (phase_ == Phase::kConnecting) {
            SendInFlight();
        }
    }

    // 下面几个回调都先处置连接、再结算在途的一批:结算可能接着发下一批,那时不能再碰这条旧连接。
    void OnDown(uint64_t id)
    {
        if (id != connectionId_ || !connection_) {
            return;
        }
        const Phase phase = phase_;
        // 正文到连接关闭为止的响应:关闭就是完整。
        const HttpResponseParse parsed =
            phase == Phase::kAwaitingResponse ? ParseHttpResponse(response_) : HttpResponseParse{};
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
        if (id != connectionId_ || !connection_) {
            return;
        }
        ReleaseConnection();
        if (phase_ == Phase::kConnecting) {
            lastErrno_ = savedErrno;
            FailInFlight("connect failed", /*backoff=*/true);
        }
    }

    void OnBytes(uint64_t id, muduo::net::Buffer* buffer)
    {
        if (id != connectionId_ || !connection_ || phase_ != Phase::kAwaitingResponse) {
            // 没在等响应时收到的字节:旧连接的残余或对端多发,连接不能再用。
            buffer->retrieveAll();
            if (id == connectionId_ && connection_) {
                CloseConnection();
            }
            return;
        }
        response_.append(buffer->peek(), buffer->readableBytes());
        buffer->retrieveAll();
        const HttpResponseParse parsed = ParseHttpResponse(response_);
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
        if (!parsed.keepAlive || parsed.consumed != response_.size()) {
            CloseConnection();
        }
        CompleteInFlight(parsed.statusCode);
    }

    void ArmExportDeadline()
    {
        CancelExportDeadline();
        auto timeout = options_.exportTimeout;
        if (shuttingDown_) {
            const auto remaining = std::chrono::duration_cast<std::chrono::milliseconds>(shutdownDeadline_ - SteadyClock::now());
            timeout = std::max(std::min(timeout, remaining), std::chrono::milliseconds(1));
        }
        const uint64_t generation = ++exportGeneration_;
        std::weak_ptr<Impl> weakSelf = weak_from_this();
        exportDeadline_ = loop_->runAfter(ToSeconds(timeout), [weakSelf, generation] {
            if (const auto self = weakSelf.lock()) {
                self->OnExportDeadline(generation);
            }
        });
        exportDeadlineArmed_ = true;
    }

    void CancelExportDeadline()
    {
        if (exportDeadlineArmed_) {
            loop_->cancel(exportDeadline_);
            exportDeadlineArmed_ = false;
        }
    }

    void OnExportDeadline(uint64_t generation)
    {
        if (generation != exportGeneration_) {
            return;
        }
        exportDeadlineArmed_ = false;
        if (phase_ == Phase::kIdle) {
            return;
        }
        CloseConnection();
        FailInFlight("export timed out", /*backoff=*/true);
    }

    void CompleteInFlight(int statusCode)
    {
        CancelExportDeadline();
        phase_ = Phase::kIdle;
        const uint64_t spans = inFlightSpans_;
        inFlightSpans_ = 0;
        request_.clear();
        if (statusCode >= 200 && statusCode < 300) {
            stats_.exportedSpans.fetch_add(spans, std::memory_order_relaxed);
            stats_.exportedBatches.fetch_add(1, std::memory_order_relaxed);
            consecutiveTransportFailures_ = 0;
            backoffUntil_ = SteadyClock::time_point{};
            if (failing_) {
                LOG_INFO << "[Tracing] OTLP span export to " << url_ << " recovered after " << failedBatchesWhileFailing_
                         << " failed batch(es)";
                failing_ = false;
                failedBatchesWhileFailing_ = 0;
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
        phase_ = Phase::kIdle;
        const uint64_t spans = inFlightSpans_;
        inFlightSpans_ = 0;
        request_.clear();
        std::string detail = reason;
        if (lastErrno_ != 0) {
            detail += " (errno " + std::to_string(lastErrno_) + ")";
            lastErrno_ = 0;
        }
        if (spans > 0) {
            RecordFailedBatch(spans, detail);
        }
        if (backoff && !shuttingDown_) {
            ++consecutiveTransportFailures_;
            auto delay = options_.initialBackoff;
            for (uint32_t i = 1; i < consecutiveTransportFailures_ && delay < options_.maxBackoff; ++i) {
                delay *= 2;
            }
            delay = std::min(delay, options_.maxBackoff);
            backoffUntil_ = SteadyClock::now() + delay;
        }
        AfterInFlightSettled();
    }

    // 记一批失败:状态从正常转成失败时打一行 WARN,之后只计数,恢复时打一行 INFO(不按批刷日志)。
    void RecordFailedBatch(uint64_t spans, const std::string& reason)
    {
        stats_.failedSpans.fetch_add(spans, std::memory_order_relaxed);
        stats_.failedBatches.fetch_add(1, std::memory_order_relaxed);
        ++failedBatchesWhileFailing_;
        if (!failing_) {
            failing_ = true;
            LOG_WARN << "[Tracing] OTLP span export to " << url_ << " failed: " << reason << "; dropped " << spans
                     << " span(s). Batches are dropped (not retried) until the collector accepts again";
        }
    }

    void DropOneBatchWhileBackingOff()
    {
        std::vector<SpanRecord> dropped;
        const size_t count = queue_.PopBatch(dropped, options_.maxExportBatchSize);
        if (count > 0) {
            stats_.failedSpans.fetch_add(count, std::memory_order_relaxed);
        }
    }

    void AfterInFlightSettled()
    {
        // 接下来的动作一律排到下一轮:这里可能正处在连接的回调栈上,不在这里同步建新连接或再发一批。
        std::weak_ptr<Impl> weakSelf = weak_from_this();
        if (shuttingDown_) {
            loop_->queueInLoop([weakSelf] {
                if (const auto self = weakSelf.lock()) {
                    self->ContinueShutdown();
                }
            });
            return;
        }
        // 还够一批就接着发,不等下一个周期(BatchSpanProcessor 的语义)。
        if (queue_.Size() >= options_.maxExportBatchSize) {
            loop_->queueInLoop([weakSelf] {
                if (const auto self = weakSelf.lock()) {
                    self->TryExport();
                }
            });
        }
    }

    void ContinueShutdown()
    {
        if (finished_ || phase_ != Phase::kIdle) {
            return;
        }
        if (SteadyClock::now() >= shutdownDeadline_) {
            FinishShutdown();
            return;
        }
        TryExport();   // 队列空了会走到 FinishShutdown
    }

    void OnShutdownDeadline()
    {
        if (finished_) {
            return;
        }
        if (phase_ != Phase::kIdle) {
            CancelExportDeadline();
            const uint64_t spans = inFlightSpans_;
            inFlightSpans_ = 0;
            phase_ = Phase::kIdle;
            if (spans > 0) {
                stats_.failedSpans.fetch_add(spans, std::memory_order_relaxed);
                stats_.failedBatches.fetch_add(1, std::memory_order_relaxed);
            }
        }
        FinishShutdown();
    }

    void FinishShutdown()
    {
        if (finished_) {
            return;
        }
        finished_ = true;
        CancelExportDeadline();
        CancelPeriodicTimer();
        if (shutdownTimerArmed_) {
            loop_->cancel(shutdownTimer_);
            shutdownTimerArmed_ = false;
        }
        const size_t discarded = queue_.DiscardAll();
        if (discarded > 0) {
            LOG_WARN << "[Tracing] span exporter stopped with " << discarded << " span(s) not exported";
        }
        CloseConnection();
        // 连接对象排在队尾析构(不在它自己的回调栈上);~TcpClient 再排的 forceCloseInLoop / connectDestroyed /
        // Connector 的 resetChannel 由下面三跳 queueInLoop 排空之后才放行(AGENTS.md §11.7)。
        std::shared_ptr<OtlpSpanExporter::Latch> latch = latch_;
        muduo::net::EventLoop* loop = loop_;
        loop_->queueInLoop([loop, latch] {
            loop->queueInLoop([loop, latch] {
                loop->queueInLoop([latch] { latch->Signal(); });
            });
        });
    }

    std::optional<muduo::net::InetAddress> ResolveEndpoint(bool allowDns)
    {
        const OtlpHttpEndpoint& endpoint = options_.endpoint;
        // muduo 的 InetAddress 遇到非法 IP 只打一行日志并留下 0.0.0.0,所以先按规范 IPv4 校验(muduo_grpc/address.h)。
        if (muduo_grpc::IsCanonicalIpv4(endpoint.host)) {
            return muduo::net::InetAddress(endpoint.host, endpoint.port);
        }
        if (!allowDns) {
            return std::nullopt;
        }
        muduo::net::InetAddress address(endpoint.port);
        if (!muduo::net::InetAddress::resolve(endpoint.host, &address)) {
            return std::nullopt;
        }
        return address;
    }

    void CloseConnection()
    {
        if (connection_) {
            connection_->Close();
            ReleaseConnection();
        }
    }

    // 不在连接自己的回调栈上析构:移进排队任务。之后这条连接的回调按 connectionId_ 认出是旧的,一律忽略。
    void ReleaseConnection()
    {
        if (!connection_) {
            return;
        }
        connectionId_ = 0;
        loop_->queueInLoop([released = std::move(connection_)]() mutable { released.reset(); });
        connection_.reset();
    }

    void CancelPeriodicTimer()
    {
        if (periodicTimerArmed_) {
            loop_->cancel(periodicTimer_);
            periodicTimerArmed_ = false;
        }
    }

    muduo::net::EventLoop* loop_;
    SpanExportQueue& queue_;
    SpanExportStats& stats_;
    OtlpSpanExporterOptions options_;
    std::shared_ptr<OtlpSpanExporter::Latch> latch_;
    const std::string url_;

    Phase phase_{Phase::kIdle};
    std::vector<SpanRecord> batch_;
    std::string request_;
    std::string response_;
    uint64_t inFlightSpans_{0};
    std::shared_ptr<HttpConnection> connection_;
    uint64_t connectionId_{0};
    uint64_t nextConnectionId_{1};
    int lastErrno_{0};

    muduo::net::TimerId periodicTimer_;
    bool periodicTimerArmed_{false};
    muduo::net::TimerId exportDeadline_;
    bool exportDeadlineArmed_{false};
    uint64_t exportGeneration_{0};

    uint32_t consecutiveTransportFailures_{0};
    SteadyClock::time_point backoffUntil_{};
    bool failing_{false};
    uint64_t failedBatchesWhileFailing_{0};

    bool shuttingDown_{false};
    bool finished_{false};
    SteadyClock::time_point shutdownDeadline_{};
    muduo::net::TimerId shutdownTimer_;
    bool shutdownTimerArmed_{false};
};

OtlpSpanExporter::OtlpSpanExporter(SpanExportQueue& queue, SpanExportStats& stats, OtlpSpanExporterOptions options)
    : queue_(queue), stats_(stats), options_(std::move(options))
{
}

OtlpSpanExporter::~OtlpSpanExporter()
{
    Shutdown();
}

void OtlpSpanExporter::Start()
{
    if (started_) {
        return;
    }
    started_ = true;
    thread_ = std::make_unique<muduo::net::EventLoopThread>(muduo::net::EventLoopThread::ThreadInitCallback(),
                                                           "span-export");
    loop_ = thread_->startLoop();
    latch_ = std::make_shared<Latch>();
    // Impl 在本线程上构造(只存参数,不碰 loop),之后它的全部状态只在导出线程上读写。
    impl_ = std::make_shared<Impl>(loop_, queue_, stats_, options_, latch_);
    loop_->runInLoop([impl = impl_] { impl->StartInLoop(); });
}

void OtlpSpanExporter::Shutdown()
{
    if (!started_ || stopped_) {
        return;
    }
    stopped_ = true;
    loop_->runInLoop([impl = impl_] { impl->BeginShutdownInLoop(); });
    // 收尾本身以 shutdownTimeout 为限(导出线程上的定时器),多等 2 秒给收尾的几跳 queueInLoop 与线程调度。
    if (!latch_->WaitFor(options_.shutdownTimeout + std::chrono::seconds(2))) {
        // 导出线程卡住了(例如解析主机名阻塞):不能在本线程上拆它的 loop、连接与定时器。有意泄漏,进程正在退出。
        LOG_ERROR << "[Tracing] span exporter thread did not finish its shutdown in time; leaving it running until process exit";
        (void)thread_.release();
        (void)new std::shared_ptr<Impl>(std::move(impl_));
        return;
    }
    // ~EventLoopThread:quit + join。Impl 在导出线程上已经释放了连接、取消了定时器,之后在本线程上析构是安全的。
    thread_.reset();
    impl_.reset();
    loop_ = nullptr;
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
