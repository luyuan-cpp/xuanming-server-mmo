#pragma once

#include <chrono>
#include <cstdint>
#include <memory>
#include <random>
#include <string>

#include "kill_switch/kill_switch.h"
#include "node/system/etcd/etcd_transport.h"
#include "time/comp/timer_task_comp.h"

// 热关停规则的 etcd 同步(docs/design/rpc-kill-switch.md §9):一条独立的 etcd 连接,只读前缀 /mmorpg/killswitch/。
//
//   启动:全量 Range → 从快照 revision + 1 建 watch;
//   watch 事件:逐条 Put / Erase 进 KillSwitch;
//   每 recheckInterval(缺省 20 秒,StaleAfter 的三分之一,±20% 抖动)全量 Range 复核:读到即续新鲜度并对账,
//     读不到就让快照按 StaleAfter 自然作废(R12 教训:watch 通道开着不代表还能读到 etcd);
//     第一次读到之前每 3 秒重试一次(传输的 unary 失败不回调,只能按节奏重发);
//   流重建 / watch 被压缩:立即回到全量 Range;其他原因的取消:按 discovery_resync 的退避重拉。
//
// 不回退:Range 与 watch 走不同的流,应答先后不定。本地状态记到「已准确反映到哪个 revision」(appliedThrough_);
// 更旧的 Range 快照只续新鲜度不替换,mod_revision 不比它新的 watch 事件跳过。
//
// 与节点的注册 / 发现完全分开(各自一条传输):这里出任何问题都只会让热关停退回全放行,不影响注册与发现,反之亦然。
// 每次 Range 只读热关停前缀(规则条数级别),全服读量随进程数线性增长,不是发现那种平方项。
//
// 线程与生命周期:只在节点 loop 线程使用,传输回调也只在该线程。必须在 loop 仍在运行时 Shutdown
// (传输的要求,同 EtcdService::Shutdown);Shutdown 之后不再回调、不再发请求。定时器是本对象的成员,
// 闭包捕获 this 属于 AGENTS.md §11.7 的同寿命例外,所以本类禁止拷贝与移动。
class KillSwitchEtcdSync final : public EtcdResponseSink
{
public:
    // 缺省复核间隔:StaleAfter 的三分之一。与 etcd 失联满 StaleAfter 才作废,在那之前至少还有两次全量读的机会。
    static constexpr std::chrono::seconds kDefaultRecheckInterval{kill_switch::kDefaultStaleAfter / 3};
    // 第一次读到 etcd 之前的重试间隔(与 Go killswitch 的 ResyncBackoff 同值)。
    static constexpr std::chrono::seconds kInitialRetryInterval{3};

    explicit KillSwitchEtcdSync(kill_switch::KillSwitch &target,
                                std::string prefix = std::string(kill_switch::kDefaultPrefix),
                                std::chrono::seconds recheckInterval = kDefaultRecheckInterval);
    ~KillSwitchEtcdSync() override;
    KillSwitchEtcdSync(const KillSwitchEtcdSync &) = delete;
    KillSwitchEtcdSync &operator=(const KillSwitchEtcdSync &) = delete;
    KillSwitchEtcdSync(KillSwitchEtcdSync &&) = delete;
    KillSwitchEtcdSync &operator=(KillSwitchEtcdSync &&) = delete;

    // 建传输(etcd_transport::Create,与 EtcdService 同一个实现、同一个地址)并发第一次全量 Range。只调一次。
    // 要在节点构造期调用(loop 尚未运行):传输在构造时解析地址。
    void Start(muduo::net::EventLoop &loop, const std::string &endpoint);
    // 单测入口:用给定的传输(替身)代替 etcd_transport::Create,其余同 Start。
    void StartWithTransport(std::unique_ptr<EtcdTransport> transport);
    // 停定时器、拆传输。幂等。
    void Shutdown();

    // ── EtcdResponseSink(只在节点 loop 线程回调)──
    void OnRange(const etcdserverpb::RangeRequest *request, const etcdserverpb::RangeResponse &reply) override;
    void OnPut(const etcdserverpb::PutResponse &) override {}
    void OnDeleteRange(const etcdserverpb::DeleteRangeResponse &) override {}
    void OnWatch(const etcdserverpb::WatchResponse &reply) override;
    void OnKeepAlive(const etcdserverpb::LeaseKeepAliveResponse &) override {}
    void OnWatchStreamRestarted() override;

    // 单测观察用。
    int64_t AppliedThroughRevision() const { return appliedThrough_; }
    bool WatchEstablished() const { return watchId_ >= 0; }

private:
    void RequestFullRange();
    void CreateWatch();
    void ResetWatchState();
    void ScheduleRecheck();
    void ScheduleCancelRetry();
    // 完整 key → 去掉前缀的相对键;不属于前缀返回空串。
    std::string RelativeKey(const std::string &key) const;

    kill_switch::KillSwitch &target_;
    const std::string prefix_;
    const std::chrono::seconds recheckInterval_;

    std::unique_ptr<EtcdTransport> transport_;
    TimerTaskComp recheckTimer_;
    TimerTaskComp cancelRetryTimer_;
    std::mt19937 jitter_{std::random_device{}()};

    // 本地规则已准确反映到的 etcd revision:全量快照的 revision,或之后应用过的最新 watch 事件的 mod_revision。
    int64_t appliedThrough_{0};
    bool everSynced_{false};
    bool watchCreateSent_{false};
    int64_t watchId_{-1};
    double cancelRetryBackoffSeconds_{0};
    bool shutdown_{false};
};
