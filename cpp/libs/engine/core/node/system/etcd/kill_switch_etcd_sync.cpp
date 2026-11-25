#include "node/system/etcd/kill_switch_etcd_sync.h"

#include <map>
#include <utility>

#include <muduo/base/Logging.h>

#include "node/system/discovery/discovery_resync_policy.h"

namespace
{
// 定期复核在基准间隔上乘 [1 - r, 1 + r) 的随机系数,避免全服进程同一时刻读 etcd(与发现的兜底重拉同一做法)。
constexpr double kRecheckJitterRatio = 0.2;

// 前缀范围的上界:最后一个字节加一(同 EtcdHelper::RangeQuery)。
std::string RangeEndOf(const std::string &prefix)
{
    std::string end = prefix;
    end.back() += 1;
    return end;
}
} // namespace

KillSwitchEtcdSync::KillSwitchEtcdSync(kill_switch::KillSwitch &target, std::string prefix,
                                       std::chrono::seconds recheckInterval)
    : target(target), prefix(std::move(prefix)), recheckInterval(recheckInterval)
{
}

KillSwitchEtcdSync::~KillSwitchEtcdSync()
{
    Shutdown();
}

void KillSwitchEtcdSync::Start(muduo::net::EventLoop &loop, const std::string &endpoint)
{
    StartWithTransport(etcd_transport::Create(loop, endpoint, *this));
}

void KillSwitchEtcdSync::StartWithTransport(std::unique_ptr<EtcdTransport> transport)
{
    if (this->transport || shutdown)
    {
        LOG_ERROR << "[KillSwitch] etcd sync started twice or after shutdown; ignored";
        return;
    }
    this->transport = std::move(transport);
    if (!this->transport)
    {
        LOG_ERROR << "[KillSwitch] No etcd transport; kill switch inactive (all requests allowed)";
        return;
    }
    LOG_INFO << "[KillSwitch] Watching prefix " << prefix << " over a dedicated etcd connection (transport="
             << this->transport->Name() << ", recheck every ~" << recheckInterval.count() << "s)";
    RequestFullRange();
    ScheduleRecheck();
}

void KillSwitchEtcdSync::Shutdown()
{
    if (shutdown)
    {
        return;
    }
    shutdown = true;
    recheckTimer.Cancel();
    cancelRetryTimer.Cancel();
    if (transport)
    {
        transport->Shutdown();
        transport.reset();
    }
    ResetWatchState();
}

void KillSwitchEtcdSync::RequestFullRange()
{
    if (shutdown || !transport)
    {
        return;
    }
    etcdserverpb::RangeRequest request;
    request.set_key(prefix);
    request.set_range_end(RangeEndOf(prefix));
    // 线性一致读:etcd 失去多数派时读失败,快照随之按 StaleAfter 作废(读不到健康的 etcd 就退回全放行)。
    transport->Range(request);
}

void KillSwitchEtcdSync::ScheduleRecheck()
{
    if (shutdown)
    {
        return;
    }
    double delaySeconds = static_cast<double>(kInitialRetryInterval.count());
    if (everSynced)
    {
        std::uniform_real_distribution<double> spread(1.0 - kRecheckJitterRatio, 1.0 + kRecheckJitterRatio);
        delaySeconds = static_cast<double>(recheckInterval.count()) * spread(jitter);
    }
    // 单测线程上没有 EventLoop 时 RunAfter 返回 false:定时器不武装,由测试直接驱动回调。
    recheckTimer.RunAfter(delaySeconds, [this]
    {
        RequestFullRange();
        ScheduleRecheck();
    });
}

void KillSwitchEtcdSync::ScheduleCancelRetry()
{
    if (shutdown)
    {
        return;
    }
    cancelRetryBackoffSeconds = discovery_resync::NextCancelRetrySeconds(cancelRetryBackoffSeconds);
    cancelRetryTimer.RunAfter(cancelRetryBackoffSeconds, [this] { RequestFullRange(); });
}

void KillSwitchEtcdSync::OnRange(const etcdserverpb::RangeRequest *request, const etcdserverpb::RangeResponse &reply)
{
    if (shutdown)
    {
        return;
    }
    if (request != nullptr && request->key() != prefix)
    {
        LOG_WARN << "[KillSwitch] Unexpected etcd range reply for key " << request->key() << "; ignored";
        return;
    }
    const int64_t revision = reply.header().revision();
    const bool firstSync = !everSynced;
    everSynced = true;
    if (reply.more())
    {
        // 没有设 limit,正常不会截断;真截断了按已返回的部分应用:少了规则只会多放行(fail-open)。
        LOG_WARN << "[KillSwitch] etcd range for " << prefix << " is truncated (more=true, count=" << reply.count()
                 << "); applying the returned keys only";
    }

    if (revision >= appliedThrough)
    {
        std::map<std::string, std::string> rawRules;
        for (const auto &kv : reply.kvs())
        {
            std::string relative = RelativeKey(kv.key());
            if (!relative.empty())
            {
                rawRules.emplace(std::move(relative), kv.value());
            }
        }
        const size_t keyCount = rawRules.size();
        target.ReplaceAll(std::move(rawRules));
        appliedThrough = revision;
        if (firstSync)
        {
            LOG_INFO << "[KillSwitch] Initial synchronization complete: " << keyCount << " keys, revision=" << revision
                     << ", rules applying to this process=" << target.ApplicableRuleCount();
        }
        else
        {
            LOG_DEBUG << "[KillSwitch] Periodic resynchronization: " << keyCount << " keys, revision=" << revision;
        }
    }
    else
    {
        // Range 与 watch 走不同的流:这份快照比 watch 已经应用到的状态旧。不替换(否则会把更新的变更回退掉),
        // 只说明 etcd 还读得到。
        target.MarkSynced();
        LOG_DEBUG << "[KillSwitch] Range snapshot at revision " << revision << " is older than the applied state ("
                  << appliedThrough << "); freshness renewed only";
    }

    if (!watchCreateSent)
    {
        CreateWatch();
    }
}

void KillSwitchEtcdSync::CreateWatch()
{
    if (shutdown || !transport)
    {
        return;
    }
    etcdserverpb::WatchRequest request;
    auto &create = *request.mutable_create_request();
    create.set_key(prefix);
    create.set_range_end(RangeEndOf(prefix));
    // 从已应用状态之后的第一个 revision 起:快照与 watch 之间不漏事件,也不回放更旧的。
    create.set_start_revision(appliedThrough + 1);
    watchCreateSent = true;
    transport->WatchWrite(request);
}

void KillSwitchEtcdSync::ResetWatchState()
{
    watchCreateSent = false;
    watchId = -1;
}

void KillSwitchEtcdSync::OnWatch(const etcdserverpb::WatchResponse &response)
{
    if (shutdown)
    {
        return;
    }
    if (response.created())
    {
        if (!response.canceled())
        {
            watchId = response.watch_id();
            cancelRetryBackoffSeconds = 0;
            LOG_INFO << "[KillSwitch] etcd watch created: watch_id=" << watchId << ", from revision "
                     << appliedThrough + 1;
        }
    }
    else if (watchId < 0 || response.watch_id() != watchId)
    {
        // 专用连接上只有这一个 watch;对不上的是旧流或已取消 watch 的残留。
        LOG_DEBUG << "[KillSwitch] etcd watch reply for unknown watch_id=" << response.watch_id() << "; ignored";
        return;
    }

    for (const auto &event : response.events())
    {
        const auto &kv = event.kv();
        // 删除事件的 kv.mod_revision 就是删除发生的 revision;缺省时退回应答头的 revision(同 EtcdService)。
        const int64_t revision = kv.mod_revision() != 0 ? kv.mod_revision() : response.header().revision();
        if (revision <= appliedThrough)
        {
            continue; // 已经被更新的全量快照覆盖
        }
        appliedThrough = revision;
        const std::string relative = RelativeKey(kv.key());
        if (relative.empty())
        {
            continue;
        }
        if (event.type() == mvccpb::Event_EventType::Event_EventType_PUT)
        {
            target.Put(relative, kv.value());
        }
        else if (event.type() == mvccpb::Event_EventType::Event_EventType_DELETE)
        {
            target.Erase(relative);
        }
    }

    if (!response.canceled())
    {
        return;
    }
    const int64_t compactRevision = response.compact_revision();
    LOG_WARN << "[KillSwitch] etcd watch canceled: watch_id=" << response.watch_id()
             << " compact_revision=" << compactRevision << " reason=" << response.cancel_reason();
    ResetWatchState();
    if (compactRevision != 0)
    {
        // 起点已被压缩:立刻全量重拉,从新快照之后建 watch(新起点必然在压缩点之后)。
        cancelRetryTimer.Cancel();
        RequestFullRange();
        return;
    }
    // 其他原因的取消:退避重拉(1 秒起、翻倍、封顶 30 秒),对端持续拒绝时不空转。
    ScheduleCancelRetry();
}

void KillSwitchEtcdSync::OnWatchStreamRestarted()
{
    if (shutdown)
    {
        return;
    }
    // 新流上一个 watch 都没有:重新全量 Range,应答到了再从已应用状态之后建 watch。
    LOG_WARN << "[KillSwitch] etcd watch stream rebuilt; re-ranging before re-creating the watch";
    ResetWatchState();
    cancelRetryTimer.Cancel();
    RequestFullRange();
}

std::string KillSwitchEtcdSync::RelativeKey(const std::string &key) const
{
    if (key.size() <= prefix.size() || key.compare(0, prefix.size(), prefix) != 0)
    {
        return {};
    }
    return key.substr(prefix.size());
}
