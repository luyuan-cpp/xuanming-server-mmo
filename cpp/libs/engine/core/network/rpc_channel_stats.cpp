#include "rpc_channel_stats.h"

#include <memory>
#include <sstream>

#include "muduo/base/Logging.h"
#include "muduo/net/EventLoop.h"

namespace
{
constexpr double kRpcChannelStatsReportIntervalSeconds = 30.0;

uint64_t Load(const std::atomic<uint64_t>& value)
{
    return value.load(std::memory_order_relaxed);
}
} // namespace

RpcChannelStats& GetRpcChannelStats()
{
    static RpcChannelStats stats;
    return stats;
}

std::string FormatRpcChannelStats(const RpcChannelStats& stats)
{
    std::ostringstream out;
    out << "started=" << Load(stats.started)
        << " ok=" << Load(stats.ok)
        << " not_sent=" << Load(stats.notSent)
        << " remote_rejected=" << Load(stats.remoteRejected)
        << " remote_failed=" << Load(stats.remoteFailed)
        << " reply_dropped=" << Load(stats.replyDropped)
        << " timeout=" << Load(stats.timeout)
        << " disconnected=" << Load(stats.disconnected)
        << " channel_closed=" << Load(stats.channelClosed)
        << " invalid_response=" << Load(stats.invalidResponse)
        << " late_reply=" << Load(stats.lateReply)
        << " legacy_id0_reply=" << Load(stats.legacyId0Reply)
        << " unknown_type=" << Load(stats.unknownType)
        << " oneway_sent=" << Load(stats.onewaySent)
        << " oneway_rejected=" << Load(stats.onewayRejected)
        << " dropped_without_loop=" << Load(stats.droppedWithoutLoop)
        << " max_in_flight=" << Load(stats.maxInFlight)
        << " max_sweep_lateness_ms=" << Load(stats.maxSweepLatenessMs)
        << " requests_dispatched=" << Load(stats.requestsDispatched)
        << " requests_rejected=" << Load(stats.requestsRejected)
        << " handler_failed=" << Load(stats.handlerFailed)
        << " adopted=" << Load(stats.adopted)
        << " reply_run_twice=" << Load(stats.replyRunTwice)
        << " reply_channel_gone=" << Load(stats.replyChannelGone)
        << " reply_abandoned=" << Load(stats.replyAbandoned)
        << " link_idle_closed=" << Load(stats.linkIdleClosed)
        << " link_idle_deferred=" << Load(stats.linkIdleDeferred);
    return out.str();
}

void RegisterRpcChannelStatsReporter(muduo::net::EventLoop& loop)
{
    // 上一次打印的内容只在 loop 线程上读写;shared_ptr 让闭包可拷贝(muduo 的 TimerCallback 是 std::function)。
    auto lastPrinted = std::make_shared<std::string>();
    loop.runEvery(kRpcChannelStatsReportIntervalSeconds, [lastPrinted]
    {
        std::string current = FormatRpcChannelStats(GetRpcChannelStats());
        if (current == *lastPrinted)
        {
            return;
        }
        LOG_INFO << "[RpcChannel] summary " << current;
        *lastPrinted = std::move(current);
    });
}
