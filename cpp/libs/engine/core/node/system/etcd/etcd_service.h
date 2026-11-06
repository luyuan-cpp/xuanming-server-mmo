#pragma once

#include <string>
#include <unordered_map>
#include <chrono>
#include <deque>
#include <memory>
#include <optional>
#include <vector>

#include "proto/etcd/etcd.pb.h"
#include "async/loop_task_scope.h"
#include "time/comp/timer_task_comp.h"
#include "node/system/discovery/discovery_key_index.h"
#include "node/system/etcd/etcd_transport.h"

// etcd 注册 / 发现 / 续租的状态机。应答经 EtcdResponseSink 从传输层回来(docs/design/grpc-muduo-transport.md §13.4);
// 传输只有 muduo_h2 一种(2026-10-15 第 3 批删除官方实现):流断由传输重建,本类按前缀 Range → 对账 → watch(§13.5)。
class EtcdService final : public EtcdResponseSink {
public:
    // 重试回调捕获 this;已武装的定时器可移动,宿主地址仍必须固定。
    EtcdService() = default;
    ~EtcdService() override;
    EtcdService(const EtcdService&) = delete;
    EtcdService& operator=(const EtcdService&) = delete;
    EtcdService(EtcdService&&) = delete;
    EtcdService& operator=(EtcdService&&) = delete;

    void Init();
    void Shutdown();

    // EtcdHelper 的唯一出口。Init 之前 / Shutdown 之后返回 nullptr(调用方丢弃请求)。
    EtcdTransport* Transport() { return transport_.get(); }

    // 拿一个新 lease 并把端口 / 分配 / 服务三把 key 改挂上去,node_id 不变。
    // 触发源:健康监控发现本节点快照消失、keepalive 返回 TTL<=0、本地 ACK 超时。
    // 失租**不碰发号**(永久 guid 走号段,与 lease 无关):身份由 CAS 决定 —— 只有分配键的 value 已经是别人的
    // uuid(注册 CAS 明确拒绝 → OnNodeIdConflictShutdown)才是真的被抢。
    void RequestReRegistration(const char* reason);

    // 永久停掉注册流的所有重试(端口 / node_id / txn 超时兜底)。
    // 身份冲突收尾期间必须调用:这台节点已经不是 node_id 的合法持有者了,
    // 再去重抢端口 / 重占 node_id 只会干扰正在接手的那个进程。
    void StopRegistrationRetries();
    [[nodiscard]] bool RegistrationInFlight() const { return registrationRunning_; }
    // Node::StartGrpcServer 的就绪通知。初次引导或维护预备态还未开放监听时，注册任务等在发布之前。
    void PublishDiscoveryAfterGrpcReady();

    int64_t GetLeaseId() const { return leaseId; }

    // Returns true if we haven't received a keepalive ACK within the lease TTL,
    // meaning etcd has likely expired our lease and another node could claim our ID.
    bool IsLeasePresumablyExpired() const;

    // 本节点路由身份"当前确认有效"的只读判定,给要写跨进程放行凭证的调用方用
    // (scene 的断线释放标记 A1′,docs/design/cross-zone-scene-travel.md §12.6.4 M3)。
    // 比 IsLeasePresumablyExpired 严格,四条全部满足才为 true:
    //   ① lease 已授予(没授予 = 身份还没确认,不能当成"没过期");
    //   ② 不在重注册中:keepalive 回 TTL<=0 之后、分配键 CAS 裁定之前身份悬而未决,而重新授予 lease 时
    //      OnLeaseGranted 会把 ACK 时间置为 now —— 只看 ACK 会在这段窗口里误判有效;
    //   ③ 注册流没有被永久停掉(身份冲突收尾中);
    //   ④ 最近一次 keepalive ACK 距今 ≤ TTL/2(毫秒精度,给 etcd 侧过期留出余量)。
    // 线程模型同本类其余成员:只在本节点 loop 线程调用。
    bool IsIdentityConfirmedFresh() const;

    // EtcdResponseSink(只在节点 loop 线程回调)
    void OnRange(const etcdserverpb::RangeRequest* request, const etcdserverpb::RangeResponse& reply) override;
    void OnPut(const etcdserverpb::PutResponse& reply) override;
    void OnDeleteRange(const etcdserverpb::DeleteRangeResponse& reply) override;
    void OnWatch(const etcdserverpb::WatchResponse& reply) override;
    void OnKeepAlive(const etcdserverpb::LeaseKeepAliveResponse& reply) override;
    void OnWatchStreamRestarted() override;
    void OnKeepAliveStreamRestarted() override;

private:
    friend struct EtcdRegistrationTestAccess;
    enum class RegistrationMode : uint8_t {
        kInitialBoot,
        kReRegisterExisting,
    };
    enum class RegistrationStage : uint8_t { kPort, kAllocation, kPublish };

    // 注册任务的节点边界。集成测试可替换节点和传输，不需要监听端口或启动 Kafka。
    struct RegistrationIo {
        using TxnDone = EtcdRegistrationCompletion<etcdserverpb::TxnResponse>;
        std::function<void(EtcdRegistrationCompletion<etcdserverpb::LeaseGrantResponse>)> requestLease;
        std::function<void()> startKeepAlive;
        std::function<bool()> selectPort;
        std::function<void()> selectNodeId;
        std::function<void()> recordLostNodeId;
        std::function<std::string(RegistrationStage)> key;
        std::function<void(RegistrationStage, bool, bool, TxnDone)> claim;
        std::function<void()> startRpc;
        std::function<void()> fenceIdentity;
    };

    // 每个发现前缀的 watch 状态(§13.5)。
    struct PrefixWatch {
        bool createSent = false;   // 当前流上已发 create(等 created 应答或已建立)
        int64_t watchId = -1;      // etcd 分配的 watch_id;-1 = 还没收到 created
    };

    void HandlePutEvent(const std::string& key, const std::string& value);
    void HandleDeleteEvent(const std::string& key, const std::string& value);
    void OnKeepAliveResponse(const ::etcdserverpb::LeaseKeepAliveResponse& reply);

    // 新传输:一个前缀的 Range 快照 → 应用 PUT → 对账删除 → 该前缀在当前流上还没 watch 就从 R+1 建。
    void OnPrefixRange(const std::string& prefix, const etcdserverpb::RangeResponse& reply);
    // 新传输的 watch 应答:created 按发送顺序对回前缀;canceled(含 compact)只重建那一个前缀。
    void OnStreamWatchResponse(const etcdserverpb::WatchResponse& response);
    void ApplyWatchEvents(const etcdserverpb::WatchResponse& response);
    // 一批(一个 Range 快照 / 一个 watch 应答)里待摘的发现节点。整批应用完才摘,且只摘不再挂在任何发现键上的 uuid。
    struct PendingRemoval {
        std::string key;
        std::string value;   // 删除前的 NodeInfo JSON,交给 HandleServiceNodeStop
        std::string uuid;    // 空 = 解析不了,无法判断是否还挂在别的键上
        std::string reason;  // 日志用
    };
    // 一条发现键 PUT(来自 Range 快照或 watch):过时的丢弃;同一个键换了持有者(uuid 不同)时把旧持有者记进 removals。
    void ApplyDiscoveryPut(const std::string& key, const std::string& value, int64_t modRevision,
                           std::vector<PendingRemoval>& removals);
    void FlushPendingRemovals(std::vector<PendingRemoval>& removals);
    void ResetPrefixWatch(const std::string& prefix);
    // watch 因压缩以外的原因被取消:按前缀退避重拉(discovery_resync_policy.h)。所有待重拉的前缀共用一个定时器。
    void ScheduleCanceledPrefixRetry(const std::string& prefix);
    static bool IsAllocationKey(const std::string& key);

    void RequestNodeLease();
    void ConfigureRegistration(muduo::net::EventLoop& loop, RegistrationIo io);
    boost::cobalt::task<void> RegisterAsync(muduo::net::EventLoop& loop);
    void SetRegistrationMode(RegistrationMode mode, const char* reason);
    const char* RegistrationModeName(RegistrationMode mode) const;

private:
    bool hasSentWatch = false;
    bool registrationRunning_ = false;
    bool registrationStopped_ = false;
    bool restartRegistration_ = false;
    bool rpcPrepared_ = false;
    bool grpcReady_ = false;
    std::function<void(bool)> grpcReadyCompletion_;
    RegistrationMode registrationMode_ = RegistrationMode::kInitialBoot;
    int64_t leaseId = 0;
    int64_t leaseTtlSeconds_ = 0;
    std::chrono::steady_clock::time_point lastKeepAliveAckTime_{};
    // 新传输专用(§13.5):前缀 → watch 状态;已发 create、等 created 应答的前缀(etcd 按发送顺序回 created);
    // watch_id → 前缀;发现键索引(DELETE 不依赖 prev_kv、对账、丢过时条目)。
    std::unordered_map<std::string, PrefixWatch> prefixWatches_;
    std::deque<std::string> pendingWatchCreates_;
    std::unordered_map<int64_t, std::string> watchIdToPrefix_;
    DiscoveryKeyIndex discoveryKeys_;
    // 每个前缀已经应用过的最新 Range 快照的 revision:更旧的应答整条丢弃,watch 从它之后建(跨流重建保留)。
    std::unordered_map<std::string, int64_t> appliedSnapshotRevision_;
    // 取消后待重拉的前缀(去重、按到达顺序),与每个前缀当前的退避秒数;建成 watch 时清掉该前缀的退避。
    std::vector<std::string> canceledPrefixesToRetry_;
    std::unordered_map<std::string, double> cancelRetryBackoffSeconds_;
	TimerTaskComp cancelRetryTimer_;
    // 先停止注册等待，再析构传输；传输回调不持有注册任务。
    std::unique_ptr<EtcdTransport> transport_;
    std::optional<std::reference_wrapper<muduo::net::EventLoop>> registrationLoop_;
    RegistrationIo registrationIo_;
    std::optional<LoopTaskScope> registrationTasks_;
};
