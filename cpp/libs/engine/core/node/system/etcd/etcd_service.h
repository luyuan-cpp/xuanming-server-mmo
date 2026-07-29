#pragma once

#include <string>
#include <unordered_map>
#include <chrono>

#include "proto/etcd/etcd.pb.h"
#include "time/comp/timer_task_comp.h"

class EtcdService {
public:
    void Init();
    void InitHandlers();
    void StartWatchingPrefixes();
    void Shutdown();
    void RequestNodeLease();
    void RequestReRegistration();

    // 永久停掉注册流的所有重试(端口 / node_id / txn 超时兜底)。
    // 身份冲突收尾期间必须调用:这台节点已经不是 node_id 的合法持有者了,
    // 再去重抢端口 / 重占 node_id 只会干扰正在接手的那个进程。
    void StopRegistrationRetries();
    void RegisterService();
    void StartLeaseKeepAlive();

    // Called by Node::StartGrpcServer after the gRPC port is bound and accepting
    // connections. Publishes the discovery key (NodeInfo) that peers watch.
    // The publish was deliberately deferred from OnTxnSucceeded(allocKey) so
    // peers don't see the node alive via etcd watch and dial gRPC before the
    // port is open (cold-start "connection refused" race).
    // No-op for re-registration mode (gRPC stayed up, publish happened early).
    void PublishDiscoveryAfterGrpcReady();

    int64_t GetLeaseId() const { return leaseId; }

    // Returns true if we haven't received a keepalive ACK within the lease TTL,
    // meaning etcd has likely expired our lease and another node could claim our ID.
    bool IsLeasePresumablyExpired() const;

private:
    enum class RegistrationMode : uint8_t {
        kInitialBoot,
        kReRegisterExisting,
    };

    void HandlePutEvent(const std::string& key, const std::string& value);
    void HandleDeleteEvent(const std::string& key, const std::string& value);
    void OnLeaseGranted(const ::etcdserverpb::LeaseGrantResponse& reply);
    void OnKeepAliveResponse(const ::etcdserverpb::LeaseKeepAliveResponse& reply);
    void OnWatchResponse(const ::etcdserverpb::WatchResponse& response);
    void OnTxnSucceeded(const std::string& key);
    void OnTxnFailed(const std::string& key);
    void ActivateSnowFlakeAfterGuard();
    // Allocates the RPC port and retries with backoff while none is available.
    // Reuses acquirePortTimer so there is a single retry path for the port phase.
    void AcquirePortWithRetry();

    // 注册流的 CAS 响应超时兜底。挂在已有的 grpcHandlerTimer 上,不新建定时器。
    //
    // 为什么需要:生成的 etcd grpc client 只在 `status.ok()` 时才调 handler,
    // RPC 失败时**什么都不做** —— 于是注册流会永久停在某一阶段(没 node_id、
    // 不发布服务发现、也不重试)。到期后重查权威(重发同一个幂等 CAS 链),
    // 不是"假设成功继续往下走"。
    void CheckPendingTxnDeadline();
    void SetRegistrationMode(RegistrationMode mode, const char* reason);
    const char* RegistrationModeName(RegistrationMode mode) const;
    bool IsNodePortKey(const std::string& key) const;
    bool IsNodeIdKey(const std::string& key) const;
    // True if `key` is the global allocation key for this node's
    // (node_type, node_id). Distinguishing it from the per-zone NodeIdKey
    // lets OnTxnSucceeded skip Snowflake activation on the alloc-only step
    // (we activate Snowflake only after the per-zone publish succeeds).
    bool IsNodeAllocationKey(const std::string& key) const;

    void InitKVHandlers();
    void InitWatchHandlers();
    void InitLeaseHandlers();
    void InitTxnHandlers();
    void ScheduleWatchReconnect();
private:
    bool hasSentWatch = false;
    bool hasSentRange = false;
    bool leaseRequestInFlight_ = false;
    bool registrationStopped_ = false;
    RegistrationMode registrationMode_ = RegistrationMode::kInitialBoot;
    int64_t leaseId = 0;
    int64_t leaseTtlSeconds_ = 0;
    std::chrono::steady_clock::time_point lastKeepAliveAckTime_{};
    // 零值 = 当前没有在等 CAS 响应(或刚收到响应)。
    std::chrono::steady_clock::time_point txnDeadline_{};
    std::unordered_map<std::string, int64_t> revision;
    TimerTaskComp grpcHandlerTimer;
    TimerTaskComp acquireNodeTimer;
	TimerTaskComp acquirePortTimer;
	TimerTaskComp watchReconnectTimer;
};
