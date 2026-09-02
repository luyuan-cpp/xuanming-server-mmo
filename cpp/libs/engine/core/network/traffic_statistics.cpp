#include "traffic_statistics.h"

#include <algorithm>
#include <vector>

#include "muduo/base/Logging.h"
#include "muduo/net/EventLoop.h"
#include "rpc/service_metadata/rpc_event_registry.h"

TrafficStatsCollector &TrafficStatsCollector::Instance()
{
    static TrafficStatsCollector instance;
    return instance;
}

TrafficStatsCollector::TrafficStatsCollector()
{
    if (const char *env = std::getenv("NODE_TRAFFIC_STATS_INTERVAL_SECONDS"))
    {
        int val = std::atoi(env);
        if (val > 0 && val <= 3600)
        {
            reportIntervalSeconds_ = static_cast<uint32_t>(val);
        }
    }

    if (const char *env = std::getenv("NODE_TRAFFIC_STATS_ENABLED"))
    {
        if (std::string(env) == "1")
        {
            uint32_t autoMinutes = 0;
            if (const char *durEnv = std::getenv("NODE_TRAFFIC_STATS_AUTO_DISABLE_MINUTES"))
            {
                autoMinutes = static_cast<uint32_t>(std::atoi(durEnv));
            }
            Enable(autoMinutes);
        }
    }
}

void TrafficStatsCollector::RecordSend(uint32_t messageId, uint32_t byteSize)
{
    if (!enabled_.load(std::memory_order_relaxed))
        return;
    if (messageId >= kMaxRpcMethodCount)
    {
        LOG_WARN << "[TrafficStats] Send messageId out of range, skip: " << messageId
                 << " (max: " << kMaxRpcMethodCount << ")";
        return;
    }

    auto &c = counters_[messageId];
    c.sendCount.fetch_add(1, std::memory_order_relaxed);
    c.sendBytes.fetch_add(byteSize, std::memory_order_relaxed);
    UpdateMaxSize(c.maxMessageSize, byteSize);
}

void TrafficStatsCollector::RecordRecv(uint32_t messageId, uint32_t byteSize)
{
    if (!enabled_.load(std::memory_order_relaxed))
        return;
    if (messageId >= kMaxRpcMethodCount)
    {
        LOG_WARN << "[TrafficStats] Recv messageId out of range, skip: " << messageId
                 << " (max: " << kMaxRpcMethodCount << ")";
        return;
    }

    auto &c = counters_[messageId];
    c.recvCount.fetch_add(1, std::memory_order_relaxed);
    c.recvBytes.fetch_add(byteSize, std::memory_order_relaxed);
    UpdateMaxSize(c.maxMessageSize, byteSize);
}

void TrafficStatsCollector::UpdateMaxSize(std::atomic<uint32_t> &current, uint32_t newSize)
{
    uint32_t prev = current.load(std::memory_order_relaxed);
    while (newSize > prev)
    {
        if (current.compare_exchange_weak(prev, newSize, std::memory_order_relaxed))
        {
            break;
        }
    }
}

void TrafficStatsCollector::Enable(uint32_t autoDisableMinutes)
{
    for (auto &c : counters_)
    {
        c.Reset();
    }
    windowStart_ = std::chrono::steady_clock::now();

    if (autoDisableMinutes > 0)
    {
        autoDisableTime_ = windowStart_ + std::chrono::minutes(autoDisableMinutes);
    }
    else
    {
        autoDisableTime_ = std::chrono::steady_clock::time_point{};
    }

    enabled_.store(true, std::memory_order_release);
    LOG_INFO << "[TrafficStats] Enabled. report_interval=" << reportIntervalSeconds_ << "s"
             << (autoDisableMinutes > 0 ? " auto_disable_in=" + std::to_string(autoDisableMinutes) + "min" : "");
}

void TrafficStatsCollector::Disable()
{
    enabled_.store(false, std::memory_order_release);
    ReportAndReset();
    LOG_INFO << "[TrafficStats] Disabled.";
}

void TrafficStatsCollector::ReportAndReset()
{
    auto now = std::chrono::steady_clock::now();
    double windowSeconds = std::chrono::duration<double>(now - windowStart_).count();
    if (windowSeconds <= 0)
        windowSeconds = 1.0;

    // Check auto-disable
    if (autoDisableTime_.time_since_epoch().count() > 0 && now >= autoDisableTime_)
    {
        enabled_.store(false, std::memory_order_release);
        LOG_INFO << "[TrafficStats] Auto-disabled after timeout.";
    }

    // Snapshot and reset
    std::vector<MessageTrafficSnapshot> snapshots;
    snapshots.reserve(kMaxRpcMethodCount);

    for (uint32_t i = 0; i < kMaxRpcMethodCount; ++i)
    {
        auto &c = counters_[i];
        MessageTrafficSnapshot s;
        s.messageId = i;
        s.sendCount = c.sendCount.exchange(0, std::memory_order_relaxed);
        s.recvCount = c.recvCount.exchange(0, std::memory_order_relaxed);
        s.sendBytes = c.sendBytes.exchange(0, std::memory_order_relaxed);
        s.recvBytes = c.recvBytes.exchange(0, std::memory_order_relaxed);
        s.maxMessageSize = c.maxMessageSize.exchange(0, std::memory_order_relaxed);

        if (s.TotalCount() > 0)
        {
            snapshots.push_back(s);
        }
    }

    windowStart_ = now;

    if (snapshots.empty())
    {
        LOG_INFO << "[TrafficStats] window=" << static_cast<uint32_t>(windowSeconds) << "s no traffic";
        return;
    }

    LogWindowSummary(windowSeconds, snapshots);
}

static std::string FormatBytes(uint64_t bytes)
{
    if (bytes >= 1024ULL * 1024 * 1024)
    {
        return std::to_string(bytes / (1024ULL * 1024 * 1024)) + "." +
               std::to_string((bytes / (1024ULL * 1024 * 100)) % 10) + "GB";
    }
    if (bytes >= 1024 * 1024)
    {
        return std::to_string(bytes / (1024 * 1024)) + "." +
               std::to_string((bytes / (1024 * 100)) % 10) + "MB";
    }
    if (bytes >= 1024)
    {
        return std::to_string(bytes / 1024) + "." +
               std::to_string((bytes / 100) % 10) + "KB";
    }
    return std::to_string(bytes) + "B";
}

static const char *GetMethodName(uint32_t messageId)
{
    if (messageId < kMaxRpcMethodCount && gRpcMethodRegistry[messageId].methodName)
    {
        return gRpcMethodRegistry[messageId].methodName;
    }
    return "unknown";
}

static const char *GetServiceName(uint32_t messageId)
{
    if (messageId < kMaxRpcMethodCount && gRpcMethodRegistry[messageId].serviceName)
    {
        return gRpcMethodRegistry[messageId].serviceName;
    }
    return "unknown";
}

void TrafficStatsCollector::LogWindowSummary(double windowSeconds, const std::vector<MessageTrafficSnapshot> &snapshots)
{
    uint64_t totalSendCount = 0, totalRecvCount = 0;
    uint64_t totalSendBytes = 0, totalRecvBytes = 0;

    for (const auto &s : snapshots)
    {
        totalSendCount += s.sendCount;
        totalRecvCount += s.recvCount;
        totalSendBytes += s.sendBytes;
        totalRecvBytes += s.recvBytes;
    }

    LOG_INFO << "[TrafficStats] window=" << static_cast<uint32_t>(windowSeconds) << "s"
             << " send=" << totalSendCount << "msg/" << FormatBytes(totalSendBytes)
             << " recv=" << totalRecvCount << "msg/" << FormatBytes(totalRecvBytes)
             << " send_rate=" << static_cast<uint64_t>(totalSendCount / windowSeconds) << "msg/s/" << FormatBytes(static_cast<uint64_t>(totalSendBytes / windowSeconds)) << "/s"
             << " recv_rate=" << static_cast<uint64_t>(totalRecvCount / windowSeconds) << "msg/s/" << FormatBytes(static_cast<uint64_t>(totalRecvBytes / windowSeconds)) << "/s";

    // Sort by total bytes descending, show top 20
    auto sorted = snapshots;
    std::sort(sorted.begin(), sorted.end(), [](const auto &a, const auto &b)
              { return a.TotalBytes() > b.TotalBytes(); });

    constexpr size_t kTopN = 20;
    size_t count = std::min(sorted.size(), kTopN);

    for (size_t i = 0; i < count; ++i)
    {
        const auto &s = sorted[i];
        LOG_INFO << "[TrafficStats]   #" << s.messageId
                 << " " << GetServiceName(s.messageId) << "." << GetMethodName(s.messageId)
                 << " send=" << s.sendCount << "msg/" << FormatBytes(s.sendBytes)
                 << " recv=" << s.recvCount << "msg/" << FormatBytes(s.recvBytes)
                 << " rate=" << static_cast<uint64_t>(s.TotalCount() / windowSeconds) << "msg/s"
                 << " " << FormatBytes(static_cast<uint64_t>(s.TotalBytes() / windowSeconds)) << "/s"
                 << " max=" << FormatBytes(s.maxMessageSize);
    }

    if (sorted.size() > kTopN)
    {
        LOG_INFO << "[TrafficStats]   ... and " << (sorted.size() - kTopN) << " more message types";
    }
}

void RegisterTrafficStatsReporter(muduo::net::EventLoop& loop)
{
    auto& collector = TrafficStatsCollector::Instance();
    if (!collector.IsEnabled())
    {
        LOG_INFO << "[TrafficStats] Not enabled (set NODE_TRAFFIC_STATS_ENABLED=1 to enable).";
        return;
    }

    double interval = static_cast<double>(collector.GetReportIntervalSeconds());
    loop.runEvery(interval, []
    {
        auto& c = TrafficStatsCollector::Instance();
        if (c.IsEnabled())
        {
            c.ReportAndReset();
        }
    });

    LOG_INFO << "[TrafficStats] Periodic reporter registered, interval=" << collector.GetReportIntervalSeconds() << "s";
}
