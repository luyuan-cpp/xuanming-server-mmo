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
    : target_(target), prefix_(std::move(prefix)), recheckInterval_(recheckInterval)
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
    if (transport_ || shutdown_)
    {
        LOG_ERROR << "[KillSwitch] etcd sync started twice or after shutdown; ignored";
        return;
    }
    transport_ = std::move(transport);
    if (!transport_)
    {
        LOG_ERROR << "[KillSwitch] No etcd transport; kill switch inactive (all requests allowed)";
        return;
    }
    LOG_INFO << "[KillSwitch] Watching prefix " << prefix_ << " over a dedicated etcd connection (transport="
             << transport_->Name() << ", recheck every ~" << recheckInterval_.count() << "s)";
    RequestFullRange();
    ScheduleRecheck();
}

void KillSwitchEtcdSync::Shutdown()
{
    if (shutdown_)
    {
        return;
    }
    shutdown_ = true;
    recheckTimer_.Cancel();
    cancelRetryTimer_.Cancel();
    if (transport_)
    {
        transport_->Shutdown();
        transport_.reset();
    }
    ResetWatchState();
}

void KillSwitchEtcdSync::RequestFullRange()
{
    if (shutdown_ || !transport_)
    {
        return;
    }
    etcdserverpb::RangeRequest request;
    request.set_key(prefix_);
    request.set_range_end(RangeEndOf(prefix_));
    // 线性一致读:etcd 失去多数派时读失败,快照随之按 StaleAfter 作废(读不到健康的 etcd 就退回全放行)。
    transport_->Range(request);
}

void KillSwitchEtcdSync::ScheduleRecheck()
{
    if (shutdown_)
    {
        return;
    }
    double delaySeconds = static_cast<double>(kInitialRetryInterval.count());
    if (everSynced_)
    {
        std::uniform_real_distribution<double> spread(1.0 - kRecheckJitterRatio, 1.0 + kRecheckJitterRatio);
        delaySeconds = static_cast<double>(recheckInterval_.count()) * spread(jitter_);
    }
    // 单测线程上没有 EventLoop 时 RunAfter 返回 false:定时器不武装,由测试直接驱动回调。
    recheckTimer_.RunAfter(delaySeconds, [this]
    {
        RequestFullRange();
        ScheduleRecheck();
    });
}

void KillSwitchEtcdSync::ScheduleCancelRetry()
{
    if (shutdown_)
    {
        return;
    }
    cancelRetryBackoffSeconds_ = discovery_resync::NextCancelRetrySeconds(cancelRetryBackoffSeconds_);
    cancelRetryTimer_.RunAfter(cancelRetryBackoffSeconds_, [this] { RequestFullRange(); });
}

void KillSwitchEtcdSync::OnRange(const etcdserverpb::RangeRequest *request, const etcdserverpb::RangeResponse &reply)
{
    if (shutdown_)
    {
        return;
    }
    if (request != nullptr && request->key() != prefix_)
    {
        LOG_WARN << "[KillSwitch] Unexpected etcd range reply for key " << request->key() << "; ignored";
        return;
    }
    const int64_t revision = reply.header().revision();
    const bool firstSync = !everSynced_;
    everSynced_ = true;
    if (reply.more())
    {
        // 没有设 limit,正常不会截断;真截断了按已返回的部分应用:少了规则只会多放行(fail-open)。
        LOG_WARN << "[KillSwitch] etcd range for " << prefix_ << " is truncated (more=true, count=" << reply.count()
                 << "); applying the returned keys only";
    }

    if (revision >= appliedThrough_)
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
        target_.ReplaceAll(std::move(rawRules));
        appliedThrough_ = revision;
        if (firstSync)
        {
            LOG_INFO << "[KillSwitch] Initial synchronization complete: " << keyCount << " keys, revision=" << revision
                     << ", rules applying to this process=" << target_.ApplicableRuleCount();
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
        target_.MarkSynced();
        LOG_DEBUG << "[KillSwitch] Range snapshot at revision " << revision << " is older than the applied state ("
                  << appliedThrough_ << "); freshness renewed only";
    }

    if (!watchCreateSent_)
    {
        CreateWatch();
    }
}

void KillSwitchEtcdSync::CreateWatch()
{
    if (shutdown_ || !transport_)
    {
        return;
    }
    etcdserverpb::WatchRequest request;
    auto &create = *request.mutable_create_request();
    create.set_key(prefix_);
    create.set_range_end(RangeEndOf(prefix_));
    // 从已应用状态之后的第一个 revision 起:快照与 watch 之间不漏事件,也不回放更旧的。
    create.set_start_revision(appliedThrough_ + 1);
    watchCreateSent_ = true;
    transport_->WatchWrite(request);
}

void KillSwitchEtcdSync::ResetWatchState()
{
    watchCreateSent_ = false;
    watchId_ = -1;
}

void KillSwitchEtcdSync::OnWatch(const etcdserverpb::WatchResponse &response)
{
    if (shutdown_)
    {
        return;
    }
    if (response.created())
    {
        if (!response.canceled())
        {
            watchId_ = response.watch_id();
            cancelRetryBackoffSeconds_ = 0;
            LOG_INFO << "[KillSwitch] etcd watch created: watch_id=" << watchId_ << ", from revision "
                     << appliedThrough_ + 1;
        }
    }
    else if (watchId_ < 0 || response.watch_id() != watchId_)
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
        if (revision <= appliedThrough_)
        {
            continue; // 已经被更新的全量快照覆盖
        }
        appliedThrough_ = revision;
        const std::string relative = RelativeKey(kv.key());
        if (relative.empty())
        {
            continue;
        }
        if (event.type() == mvccpb::Event_EventType::Event_EventType_PUT)
        {
            target_.Put(relative, kv.value());
        }
        else if (event.type() == mvccpb::Event_EventType::Event_EventType_DELETE)
        {
            target_.Erase(relative);
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
        cancelRetryTimer_.Cancel();
        RequestFullRange();
        return;
    }
    // 其他原因的取消:退避重拉(1 秒起、翻倍、封顶 30 秒),对端持续拒绝时不空转。
    ScheduleCancelRetry();
}

void KillSwitchEtcdSync::OnWatchStreamRestarted()
{
    if (shutdown_)
    {
        return;
    }
    // 新流上一个 watch 都没有:重新全量 Range,应答到了再从已应用状态之后建 watch。
    LOG_WARN << "[KillSwitch] etcd watch stream rebuilt; re-ranging before re-creating the watch";
    ResetWatchState();
    cancelRetryTimer_.Cancel();
    RequestFullRange();
}

std::string KillSwitchEtcdSync::RelativeKey(const std::string &key) const
{
    if (key.size() <= prefix_.size() || key.compare(0, prefix_.size(), prefix_) != 0)
    {
        return {};
    }
    return key.substr(prefix_.size());
}
