#pragma once

#include <memory>
#include <muduo/base/AsyncLogging.h>
#include <muduo/net/TcpServer.h>
#include "network/rpc_client.h"
#include "network/rpc_server.h"
#include "node/system/node/node_util.h"
#include "time/comp/timer_task_comp.h"
#include "type_define/type_define.h"
#include <boost/uuid/uuid.hpp>
#include <boost/uuid/uuid_generators.hpp>
#include <atomic>
#include <chrono>
#include <condition_variable>
#include <functional>
#include <mutex>
#include <optional>
#include <network/node_utils.h>
#include "infra/messaging/kafka/kafka_manager.h"
#include "node/system/etcd/etcd_service.h"
#include "node/system/etcd/etcd_manager.h"
#include "node/system/registration/registration_manager.h"
#include "node/system/discovery/service_discovery_manager.h"

// 只前向声明:本头经大量翻译单元包含,gRPC 服务端(muduo_grpc,docs/design/grpc-muduo-transport.md §15)
// 的完整定义只在 node.cpp 里需要。
namespace muduo_grpc
{
class Server;
class Service;
} // namespace muduo_grpc

// Prometheus /metrics 出口(core/metrics/metrics_http_exporter.h),完整定义只在 node.cpp 里需要。
class MetricsHttpExporter;
struct NodeMaintenanceControl;

// Tracks a pending node removal that can be cancelled if the node re-registers.
struct PendingNodeRemoval
{
    TimerTaskComp timer;
    NodeInfo nodeInfo;
};

enum class NodeIdConflictReason
{
    // 2026-09-08 起前两个不再触发冲突关停:失租只是拿新 lease 重注册(EtcdService::
    // RequestReRegistration),身份由分配键的 CAS 决定。枚举值保留给日志 / 外部 hook 的
    // 签名兼容,当前唯一的触发源是 kReRegistrationFailed。
    kLeaseExpiredByEtcd,    // keepalive returned TTL=0(已不再触发)
    kLeaseDeadlineExceeded, // local health monitor: no ACK within TTL(已不再触发)
    kReRegistrationFailed,  // re-register CAS failed / Watch 发现 node_id 被别的 uuid 持有
};

class Node : muduo::noncopyable
{
public:
    using RpcServerPtr = std::unique_ptr<muduo::net::RpcServer>;
    using ServiceList = std::vector<::google::protobuf::Service *>;
    using CanConnectNodeTypeList = std::set<uint32_t>;

    using PartitionClassIds = std::vector<int32_t>;

    using AfterStartFn = std::function<void(Node &)>;
    using KafkaHandlersFn = std::function<bool(Node &)>;
    using BeforeShutdownFn = std::function<void(Node &)>;
    // 返回 true 表示普通停机前的业务收尾已经落地,可以开始拆网络和运行时。
    // 不设置时视为 before-shutdown hook 是同步的。
    using ShutdownDrainCompleteFn = std::function<bool(Node &)>;
    // 拆除前的最后一个业务时刻,见 SetBeforeTeardown。
    using BeforeTeardownFn = std::function<void(Node &)>;
    using OnConflictShutdownFn = std::function<void(Node &, NodeIdConflictReason)>;
    // 返回 true 表示"身份冲突后的收尾(存盘 / 迁移玩家)已经全部落地,可以退出了"。
    // 不设置时视为收尾是同步的,hook 返回即退出(gate 就是这种)。
    using ConflictDrainCompleteFn = std::function<bool(Node &)>;

    explicit Node(muduo::net::EventLoop *loop, const std::string &logFilePath);

    // Full construction: derives log dir from nodeType (e.g. GateNodeService -> "logs/gate").
    // Calls Initialize() and registers thread observability.
    Node(muduo::net::EventLoop *loop,
         uint32_t nodeType,
         CanConnectNodeTypeList connectTo,
         ::google::protobuf::Service *replyService = nullptr);

    virtual ~Node();

    // Basic information accessors
    int64_t GetLeaseId() const;
    muduo::net::EventLoop *GetLoop() { return &eventLoop; }
    NodeId GetNodeId() const { return GetNodeInfo().node_id(); }
    uint32_t GetNodeType() const { return GetNodeInfo().node_type(); }
    // 身份由本节点独占持有;读写均限所属 loop 线程,不能跨线程借用引用。
    const NodeInfo &GetNodeInfo() const { return localNodeInfo_; }
    // 仅供启动、注册链路与 gate 人数上报写入;不会向 ECS 补默认组件。
    NodeInfo &MutableNodeInfo();
    KafkaManager &GetKafkaManager() { return kafkaManager; }
    virtual google::protobuf::Service *GetNodeReplyService() { return replyService_ ? &replyService_->get() : nullptr; }
    muduo::AsyncLogging &Log() { return logSystem; }
    CanConnectNodeTypeList &GetTargetNodeTypeWhitelist() { return targetNodeTypeWhitelist; }
    NodeHandshakeManager &GetNodeRegistrationManager() { return nodeRegistrationManager; }
    ServiceDiscoveryManager &GetServiceDiscoveryManager() { return serviceDiscoveryManager; }
    EtcdManager &GetEtcdManager() { return etcdManager; }

    // Only valid after StartRpcServer().
    muduo::net::TcpServer &GetTcpServer() { return rpcServer->GetTcpServer(); }

    // Go-style: register hooks before start instead of subclassing.
    Node &SetAfterStart(AfterStartFn fn)
    {
        afterStartFn_ = std::move(fn);
        return *this;
    }
    Node &SetKafkaHandlers(KafkaHandlersFn fn)
    {
        kafkaHandlersFn_ = std::move(fn);
        return *this;
    }
    Node &SetBeforeShutdown(BeforeShutdownFn fn)
    {
        beforeShutdownFn_ = std::move(fn);
        return *this;
    }
    Node &SetShutdownDrainComplete(ShutdownDrainCompleteFn fn)
    {
        shutdownDrainCompleteFn_ = std::move(fn);
        return *this;
    }
    // 拆除前钩子:普通停机与身份冲突停机(含 loop 异常退出后 ~Node 的兜底排空)都会经 FinalizeTeardownInLoop 调用它,
    // 每个 Node **至多一次**(走到拆除才调),在 loop 线程上。
    // 时刻:Kafka producer 的最后一次有界 flush(kafkaManager.Shutdown)刚做完、它派发的投递回执已进调用方的账本;
    // Redis(tlsRedis)、ECS、节点注册表都还没拆,异步日志也还在写文件。适合「停掉还会推进业务的定时器 + 把停机时
    // 仍未落定的事打进日志」这类收账动作:Node 析构之后 AsyncOutput 在 Linux 上什么都不写,那时打的日志会静默丢失。
    // 钩子里不得再发起需要等待的异步工作(之后只剩拆除),也不得同步阻塞。
    Node &SetBeforeTeardown(BeforeTeardownFn fn)
    {
        beforeTeardownFn_ = std::move(fn);
        return *this;
    }
    Node &SetOnConflictShutdown(OnConflictShutdownFn fn)
    {
        onConflictShutdownFn_ = std::move(fn);
        return *this;
    }
    Node &SetConflictDrainComplete(ConflictDrainCompleteFn fn)
    {
        conflictDrainCompleteFn_ = std::move(fn);
        return *this;
    }
    bool HasDiscoveredServiceNode(uint32_t nodeType) const
    {
        return serviceDiscoveryManager.ServiceNodes().HasNodesOfType(nodeType);
    }

    std::vector<uint32_t> CollectMissingDiscoveredServiceNodes(const std::vector<uint32_t> &requiredNodeTypes) const
    {
        std::vector<uint32_t> missingNodeTypes;
        missingNodeTypes.reserve(requiredNodeTypes.size());

        for (uint32_t nodeType : requiredNodeTypes)
        {
            if (!HasDiscoveredServiceNode(nodeType))
            {
                missingNodeTypes.push_back(nodeType);
            }
        }

        return missingNodeTypes;
    }

    static std::string FormatNodeTypeNames(const std::vector<uint32_t> &nodeTypes)
    {
        std::string output;
        for (size_t i = 0; i < nodeTypes.size(); ++i)
        {
            if (i > 0)
                output += ",";
            output += eNodeType_Name(static_cast<eNodeType>(nodeTypes[i]));
        }
        return output;
    }

    // Node registration and service handling
    void HandleServiceNodeStop(const std::string &key, const std::string &nodeJson);
    void CancelPendingNodeRemoval(const std::string &nodeUuid);

    virtual void StartRpcServer();
    using KafkaMessageHandler = std::function<void(const std::string &, const std::string &)>;
    bool RegisterKafkaMessageHandler(const std::vector<std::string> &topics,
                                     const std::string &groupId,
                                     KafkaMessageHandler handler,
                                     const std::vector<int32_t> &partitions = {},
                                     const KafkaPartitionAssignPolicy &assignPolicy = {});

    // gRPC 服务端:在 StartRpcServer() 之前注册(etcd 端口分配看 HasGrpcServices() 决定是否派生 gRPC 端口)。
    // 服务由调用者拥有,必须活过 Node:生成的方法表闭包捕获服务的 this,muduo_grpc::Server 在 Node 析构前
    // (OnGrpcServerDrained / 收尾的防御销毁)已经销毁。服务端整个跑在本节点 EventLoop 上,不创建线程。
    void RegisterGrpcService(muduo_grpc::Service &service);
    bool HasGrpcServices() const { return !grpcServices_.empty(); }

    // 本节点的 etcd 身份(node_id 租约)已经失效时调用。
    //
    // 关键约束:调用点**不得**紧接着 LOG_FATAL / abort。玩家存盘是异步的
    // (提交请求还在发往 go/db 的路上),abort 会把它们全部丢掉。
    // 这里的顺序是:fence 发号 -> 跑业务收尾 hook -> 有界等待收尾完成 -> 正常退出。
    virtual void OnNodeIdConflictShutdown(NodeIdConflictReason reason);

    // 非阻塞地把停机请求投递到 EventLoop。适合 RPC handler / 信号转发器:
    // handler 跑在 loop 线程上,不能同步等待整个 drain —— gRPC 排空与收尾都要靠 loop 推进,
    // 在 loop 上等就永远等不到。
    void RequestShutdown();

    // 优雅停机。EventLoop 外调用会等待有界时间;异步入口优先用
    // RequestShutdown()。
    void Shutdown();

    // 仅报告已观察到的失败；false 不代表完整 P1/P3 资格，不能代替各传输 owner 的正式收据。
    bool HasKnownShutdownFailure() const { return shutdownFailure_.load(std::memory_order_acquire); }

    // P1 仅封根；P3 才停止中继。装配方复用真实业务钩子，不能用进程存活或 pid 代替排空。
    // 由真实启动owner声明整个内部RPC/gRPC为relay；不按method名称推断。
    Node& SetMaintenanceInboundRelay() { maintenanceInboundRelay_=true; return *this; }
    bool RequiresMaintenanceTargetIdentity() const { return maintenancePrepareBoot_; }
    Node& SetMaintenanceSourceClose(BeforeShutdownFn fn) { maintenanceSourceClose_ = std::move(fn); return *this; }
    Node& SetMaintenanceRelayStop(BeforeTeardownFn fn) { maintenanceRelayStop_ = std::move(fn); return *this; }

    // Utility and state queries
    bool IsCurrentNode(const NodeInfo &candidateNode) const;
    bool IsServiceStarted() { return rpcServer != nullptr; }

    // CLIENT_ENDPOINT_REQUIRED(D79)在启动时解析后的缓存值,构造完成后不再变化。
    // 只给"把本节点地址下发给客户端"的出口做纵深防御:为 true 时经
    // client_endpoint::ClientFacing(GetNodeInfo(), true) 拿不到地址就必须拒签,不许回落 endpoint。
    // 线程:本函数读的是普通成员(构造期写一次、之后只读),任何线程都可调用。
    // 配套的 NodeInfo 由 Node 持有,只在 loop 线程写;跨线程读取不会与写入同步。
    // 所以 ClientFacing 的 node 实参必须取自 loop 线程上的
    // gNode->GetNodeInfo()(例如 battle 在 loop 线程上执行的 BuildAssignment / 预签)。
    bool ClientEndpointRequired() const { return clientEndpointRequired_; }

protected:
    // Initialization
    void InitRpcServer();
    // 解析并写入 NodeInfo.client_endpoint(D76/D79)。只在 InitRpcServer 设完 endpoint 之后、
    // etcd 分配与发布之前调用一次;配置非法或地址来源失败直接 LOG_FATAL,绝不带病发布。
    void InitClientEndpoint();
    void Initialize();
    void InitLogSystem();
    void LoadConfigs();
    void LoadAllConfigData();
    void SetupTimeZone();
    void InitKafka();
    void StartKafkaPolling();
    virtual bool RegisterKafkaHandlers() { return kafkaHandlersFn_ ? kafkaHandlersFn_(*this) : true; }
    void InitEtcdService();
    // 按 MMORPG_METRICS_PORT 开 Prometheus 出口(未设 = 不开;非法值拒启)。只在 Initialize 里调一次。
    void StartMetricsExporter();

    // gRPC server lifecycle (called automatically if services registered).
    void StartGrpcServer();
    // 停机第 4 步(docs/design/grpc-muduo-transport.md §15.10):BeginShutdown 进入 loop 内排空
    // (停听、两段 GOAWAY、已收完的调用照常派发,约 3s 硬上限);没有服务端时直接记为已排空。
    void ShutdownGrpcServer();
    // 排空完成回调。由 Server 经 queueInLoop 投递(不在 Server 自己的栈上,也不在 I/O 事件处理期间),
    // 所以可以在这里同步销毁 Server。幂等。
    void OnGrpcServerDrained();
    // 关机收尾分两段(docs/design/muduo-rpc-channel.md §6.5):阶段 0 关闭全部 RPC 通道、让在途调用以失败完成,
    // 再把拆除(FinalizeTeardownInLoop)排到这些失败回调之后。
    void FinalizeShutdownInLoop();
    void FinalizeTeardownInLoop();

    void ReleaseNodeId();
    void RegisterHandlers();
    void StopWatchingServiceNodes();
    static void AsyncOutput(const char *msg, int len);
    void StartNodeRegistrationHealthMonitor();
    void ExecuteNodeRemoval(const NodeInfo &stoppedNode);
    void StartConflictDrainWatchdog();
    void StartShutdownDrainWatchdog();
    void MaybeFinalizeShutdownInLoop();

    void ShutdownInLoop();
    void InitMaintenanceControl();
    void PollMaintenanceControl();
    void CloseMaintenanceSource();
    void CloseTransportOwnersInLoop();
    void ReleaseLocalOwnersInLoop();

    // Member variables
    // 本节点身份的唯一所有者,最先构造、最后析构;ECS Clear 不影响身份。
    NodeInfo localNodeInfo_;
    // 入口拥有 EventLoop，必须活过 Node 析构中的全部排空与网络资源释放。
    muduo::net::EventLoop &eventLoop;
    muduo::AsyncLogging logSystem;
    RpcServerPtr rpcServer;
    // /metrics 出口:在 loop 线程创建,FinalizeTeardownInLoop 里随出站连接一起释放(TcpServer 必须在 loop 活着时析构)。
    std::unique_ptr<MetricsHttpExporter> metricsExporter_;
    TimerTaskComp serviceHealthMonitorTimer;
    // 注意:node_id / 端口的重试定时器住在 EtcdService(它才是驱动注册流的那个类)。
    // Node 这里曾经有一份同名成员,但从来没被 RunAfter 过,只在 Shutdown 里被 Cancel ——
    // 纯粹是影子成员,读代码的人会以为取消它就停住了重试,其实什么也没停。已删除。
    TimerTaskComp conflictDrainTimer;
    TimerTaskComp shutdownDrainTimer;
    TimerTaskComp maintenanceControlTimer_;
    std::unique_ptr<NodeMaintenanceControl> maintenanceControl_;
    BeforeShutdownFn maintenanceSourceClose_;
    BeforeTeardownFn maintenanceRelayStop_;
    // Kafka consumption is driven by KafkaConsumer::startBackgroundPolling
    // (a dedicated thread that queueInLoop's callbacks back into eventLoop).
    // The producer is non-blocking and flushes via KafkaManager::Shutdown.
    // No EventLoop timer is required for either path.
    CanConnectNodeTypeList targetNodeTypeWhitelist;
    std::unordered_map<std::string, int64_t> revision;
    bool hasSentRange{false};
    bool hasSentWatch{false};
    boost::uuids::random_generator uuidGenerator;
    KafkaManager kafkaManager;
    EtcdManager etcdManager;
    NodeHandshakeManager nodeRegistrationManager;
    ServiceDiscoveryManager serviceDiscoveryManager;
    std::atomic<bool> shutdownStarted{false};
    std::mutex shutdownCompletionMutex_;
    std::condition_variable shutdownCompletionCv_;
    bool shutdownComplete_{false};
    bool shutdownGrpcDrainComplete_{false};
    std::atomic<bool> shutdownFailure_{false};
    bool shutdownFinalizationStarted_{false};
    bool kafkaPollingStarted{false};
    // 见 ClientEndpointRequired();由 InitClientEndpoint 在构造期写一次。
    bool clientEndpointRequired_{false};

    // Grace-period pending removals: node_uuid -> pending removal state.
    // When etcd fires DELETE, removal is deferred; a subsequent PUT cancels it.
    std::unordered_map<std::string, std::unique_ptr<PendingNodeRemoval>> pendingNodeRemovals_;

    // 调用者拥有服务，必须活过 Node 及其注册通道；空值表示不额外注册该服务。
    std::optional<std::reference_wrapper<::google::protobuf::Service>> replyService_;
    AfterStartFn afterStartFn_;
    KafkaHandlersFn kafkaHandlersFn_;
    BeforeShutdownFn beforeShutdownFn_;
    ShutdownDrainCompleteFn shutdownDrainCompleteFn_;
    BeforeTeardownFn beforeTeardownFn_;
    // FinalizeTeardownInLoop 会因 gRPC 本地延后完成未排空而重入;钩子只在第一次进入时调用。
    bool beforeTeardownRan_{false};
    bool sourceBusinessClosed_{false};
    bool maintenanceInboundRelay_{false};
    bool maintenancePrepareBoot_{false}, maintenancePreparedStart_{false}, maintenanceSourceOpened_{false};
    void StartPreparedSourceInLoop();
    bool transportCloseStarted_{false};
    bool rpcNativeJoined_{false};
    bool localOwnerReleaseStarted_{false};
    OnConflictShutdownFn onConflictShutdownFn_;
    ConflictDrainCompleteFn conflictDrainCompleteFn_;
    bool conflictShutdownStarted_{false};
    std::chrono::steady_clock::time_point conflictDrainDeadline_{};
    std::chrono::steady_clock::time_point shutdownDrainDeadline_{};

    // gRPC server (optional, started only when services are registered via RegisterGrpcService).
    // 服务由调用者拥有(见 RegisterGrpcService),这里只存引用。
    std::vector<std::reference_wrapper<muduo_grpc::Service>> grpcServices_;
    // muduo_grpc::Server 是连接的唯一强持有者(TcpServer 的等价物,AGENTS §11.7),自己的定时器 / 闭包只绑 weak;
    // Node 是它唯一的所有者。全部在本节点 EventLoop 上运行,不创建线程(第 4 批起没有 poller / 关机线程)。
    // 只在排队任务里或 loop 外销毁:OnGrpcServerDrained 里 reset;收尾时若仍在,经 queueInLoop 防御销毁。
    std::shared_ptr<muduo_grpc::Server> grpcServer_;
    // 排空起点(BeginShutdown 时刻),只用于 "gRPC server drained. drain_ms=" 日志。
    std::chrono::steady_clock::time_point grpcDrainStart_{};
};

// DependencyGate — polls for required service-discovery dependencies before
// firing a one-shot "ready" callback.  Eliminates boilerplate in node main files.
//
// Usage (inside SetAfterStart):
//   DependencyGate gate;
//   gate.WaitAndRun(n, { SceneManagerNodeService }, [&](auto&) {
//       worldTimer.RunEvery(dt, World::Update);
//   });
//
struct DependencyGate
{
    // 探测回调捕获 this;定时器可转移不代表宿主地址可以改变。
    DependencyGate() = default;
    DependencyGate(const DependencyGate&) = delete;
    DependencyGate& operator=(const DependencyGate&) = delete;
    DependencyGate(DependencyGate&&) = delete;
    DependencyGate& operator=(DependencyGate&&) = delete;

    struct ExtraCondition
    {
        std::string name;
        std::function<bool()> ready;
    };

    TimerTaskComp probeTimer;
    bool ready{false};
    uint32_t waitLogTick{0};
    // 除服务发现之外的就绪条件(例如 scene 的 "id segments ready"),WaitAndRun 之前 AddCondition。
    std::vector<ExtraCondition> extraConditions;

    void AddCondition(std::string name, std::function<bool()> readyFn)
    {
        extraConditions.push_back(ExtraCondition{std::move(name), std::move(readyFn)});
    }

    // nodeName: e.g. "Scene", "Gate" — used only in log messages.
    // requiredNodeTypes: service types to wait for.
    // onReady: invoked exactly once when all dependencies are discovered.
    template <typename TNode, typename ReadyFn>
    void WaitAndRun(TNode &node,
                    const std::vector<uint32_t> &requiredNodeTypes,
                    ReadyFn onReady,
                    const std::string &nodeName = "",
                    double probeIntervalSec = 1.0,
                    uint32_t logEveryNTicks = 5)
    {
        probeTimer.RunEvery(probeIntervalSec,
                            [this, &node, requiredNodeTypes, onReady = std::move(onReady), nodeName, logEveryNTicks]
                            {
                                if (ready)
                                    return;

                                auto missing = node.CollectMissingDiscoveredServiceNodes(requiredNodeTypes);
                                std::string missingConditions;
                                for (const auto &condition : extraConditions)
                                {
                                    if (!condition.ready())
                                    {
                                        if (!missingConditions.empty())
                                            missingConditions += ",";
                                        missingConditions += condition.name;
                                    }
                                }
                                if (!missing.empty() || !missingConditions.empty())
                                {
                                    if (++waitLogTick % logEveryNTicks == 0)
                                    {
                                        LOG_INFO << nodeName << " startup waiting dependencies: missing="
                                                 << Node::FormatNodeTypeNames(missing)
                                                 << " conditions=" << (missingConditions.empty() ? "(none)" : missingConditions);
                                    }
                                    return;
                                }

                                ready = true;
                                LOG_INFO << nodeName << " dependency ready: all required nodes discovered. "
                                         << "required=" << Node::FormatNodeTypeNames(requiredNodeTypes)
                                         << " conditions=" << extraConditions.size();
                                onReady(node);
                            });
    }
};

extern Node *gNode;
