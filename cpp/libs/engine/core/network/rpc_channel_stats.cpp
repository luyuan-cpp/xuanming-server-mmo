#include "rpc_channel_stats.h"

#include <memory>
#include <sstream>
#include <string_view>

#include "muduo/base/Logging.h"
#include "muduo/net/EventLoop.h"

#include "../metrics/prometheus_text.h"
#include "rpc_status.h"

namespace
{
constexpr double kRpcChannelStatsReportIntervalSeconds = 30.0;

uint64_t Load(const std::atomic<uint64_t>& value)
{
    return value.load(std::memory_order_relaxed);
}

void WriteCounter(prometheus_text::Writer& writer, std::string_view name, std::string_view help, uint64_t value)
{
    writer.BeginFamily(name, help, prometheus_text::MetricType::kCounter);
    writer.Sample({}, value);
}

void WriteGauge(prometheus_text::Writer& writer, std::string_view name, std::string_view help, uint64_t value)
{
    writer.BeginFamily(name, help, prometheus_text::MetricType::kGauge);
    writer.Sample({}, value);
}

void SampleOutcome(prometheus_text::Writer& writer, RpcStatusCode code, const std::atomic<uint64_t>& counter)
{
    writer.Sample({{"outcome", RpcStatusCodeName(code)}}, Load(counter));
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

void WriteRpcChannelMetrics(prometheus_text::Writer& writer, const RpcChannelStats& stats)
{
    // 新增结局时要在下面补一行 SampleOutcome,FormatRpcChannelStats 同步补字段。
    static_assert(static_cast<std::size_t>(RpcStatusCode::kCount) == 9);

    // 客户端。
    WriteCounter(writer, "cpp_node_rpc_client_calls_started_total",
                 "Node-to-node RPC calls this process started that wait for a reply.", Load(stats.started));
    writer.BeginFamily("cpp_node_rpc_client_calls_total",
                       "Finished node-to-node RPC calls by outcome: ok; not_sent (surely not sent); remote_rejected (the peer "
                       "refused it before running it, kill switch denials included); remote_failed (the peer handler ran and "
                       "failed); reply_dropped, timeout, disconnected, channel_closed, invalid_response (outcome unknown).",
                       prometheus_text::MetricType::kCounter);
    SampleOutcome(writer, RpcStatusCode::kOk, stats.ok);
    SampleOutcome(writer, RpcStatusCode::kNotSent, stats.notSent);
    SampleOutcome(writer, RpcStatusCode::kRemoteRejected, stats.remoteRejected);
    SampleOutcome(writer, RpcStatusCode::kRemoteFailed, stats.remoteFailed);
    SampleOutcome(writer, RpcStatusCode::kReplyDropped, stats.replyDropped);
    SampleOutcome(writer, RpcStatusCode::kTimeout, stats.timeout);
    SampleOutcome(writer, RpcStatusCode::kDisconnected, stats.disconnected);
    SampleOutcome(writer, RpcStatusCode::kChannelClosed, stats.channelClosed);
    SampleOutcome(writer, RpcStatusCode::kInvalidResponse, stats.invalidResponse);
    WriteCounter(writer, "cpp_node_rpc_client_late_replies_total",
                 "Replies that matched no in-flight call (it had already timed out or failed on disconnect, or the id is unknown).",
                 Load(stats.lateReply));
    WriteCounter(writer, "cpp_node_rpc_client_legacy_id0_replies_total",
                 "Replies carrying call id 0 (a peer binary without call ids, or a peer replying to a one-way request); "
                 "should stay 0.",
                 Load(stats.legacyId0Reply));
    writer.BeginFamily("cpp_node_rpc_client_oneway_total",
                       "One-way requests: sent, or not_sent (dropped locally: channel closed, not connected, message id not "
                       "dispatchable, request type mismatch, serialization failed, maintenance capacity exhausted).",
                       prometheus_text::MetricType::kCounter);
    writer.Sample({{"result", "sent"}}, Load(stats.onewaySent));
    writer.Sample({{"result", "not_sent"}}, Load(stats.onewayRejected));
    WriteCounter(writer, "cpp_node_rpc_client_calls_dropped_without_loop_total",
                 "In-flight calls destroyed with their channel after its event loop was gone, so their callbacks never ran "
                 "(a shutdown ordering gap); should stay 0.",
                 Load(stats.droppedWithoutLoop));
    WriteGauge(writer, "cpp_node_rpc_client_in_flight_max",
               "Highest number of in-flight calls seen on a single channel since process start.", Load(stats.maxInFlight));
    WriteGauge(writer, "cpp_node_rpc_timeout_sweep_lateness_max_milliseconds",
               "Largest delay since process start between a call's deadline and the sweep that timed it out "
               "(a sign that this node's event loop stalled).",
               Load(stats.maxSweepLatenessMs));

    // 服务端。
    WriteCounter(writer, "cpp_node_rpc_received_unknown_type_total",
                 "Frames received with an unknown RPC message type (retired types included); dropped without closing the link.",
                 Load(stats.unknownType));
    WriteCounter(writer, "cpp_node_rpc_server_requests_dispatched_total", "Requests handed to a handler.",
                 Load(stats.requestsDispatched));
    WriteCounter(writer, "cpp_node_rpc_server_requests_rejected_total",
                 "Requests refused before dispatch (invalid message id, no service or method, malformed request).",
                 Load(stats.requestsRejected));
    WriteCounter(writer, "cpp_node_rpc_server_handler_failures_total",
                 "Dispatched requests whose handler ran and failed explicitly (SetFailed or RpcDoneCallback::Fail).",
                 Load(stats.handlerFailed));
    WriteCounter(writer, "cpp_node_rpc_server_replies_adopted_total",
                 "Requests whose handler took the reply over to send it later (RpcDoneCallback::Adopt).", Load(stats.adopted));
    writer.BeginFamily("cpp_node_rpc_server_reply_anomalies_total",
                       "Replies that went wrong: run_twice (done ran more than once), channel_gone (the channel or its loop was "
                       "gone before the reply), abandoned (an adopted reply was released unfinished; the caller sees reply_dropped).",
                       prometheus_text::MetricType::kCounter);
    writer.Sample({{"kind", "run_twice"}}, Load(stats.replyRunTwice));
    writer.Sample({{"kind", "channel_gone"}}, Load(stats.replyChannelGone));
    writer.Sample({{"kind", "abandoned"}}, Load(stats.replyAbandoned));

    // 链路心跳(docs/design/muduo-rpc-channel.md §21)。
    WriteCounter(writer, "cpp_node_rpc_link_idle_closed_total",
                 "Node links closed because the peer sent nothing for the idle timeout (peer dead or link half-open).",
                 Load(stats.linkIdleClosed));
    WriteCounter(writer, "cpp_node_rpc_link_idle_checks_deferred_total",
                 "Idle checks that found the timeout reached but ran late (this loop had stalled), so the peer got one more check.",
                 Load(stats.linkIdleDeferred));
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
