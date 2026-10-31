// Windows only, and it must stay first: this header pulls <WS2tcpip.h> ahead of
// <Windows.h> so the winsock2 declarations win over winsock.h's, and it supplies
// the POSIX shims muduo's Windows port needs. It ships in the vendored Windows
// muduo tree (third_party/muduo), which the Linux image does not copy; its whole
// body is #ifdef WIN32, so on Linux it would contribute nothing but a missing file.
#ifdef WIN32
#include "muduo/base/CrossPlatformAdapterFunction.h"
#endif
#include "muduo/base/Logging.h"
#include "muduo/net/EventLoopThreadPool.h"

#include "node/system/node/node_entry.h"
#include "node/system/etcd/etcd_transport.h"
#include "handler/rpc/gate_service_handler.h"
#include "handler/rpc/client_session_handler.h"
#include "handler/rpc/client_request_lane.h"
#include "handler/rpc/router_muduo_forward.h"
#include "muduo_grpc/channel.h"
#include "node/system/node/muduo_grpc_channel_slot.h"
#include "thread_context/node_context_manager.h"
#include "gate_codec.h"
#include "gate_router_mode.h"
#include "gate_security.h"
#include "gate_version.h"
#include "node_config_manager.h"
#include "session/manager/session_manager.h"
#include "network/network_utils.h"
#include "proto/common/base/session.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "proto/scene_manager/scene_manager_service.pb.h"
#include "proto/contracts/kafka/gate_command.pb.h"
#include "proto/common/base/message.pb.h"
#include "rpc/service_metadata/rpc_event_registry.h"
#include "handler/event/gate_kafka_command_router.h"
#include "handler/event/scene_entry_dispatch.h"
#include "proto/contracts/kafka/gate_event.pb.h"
#include "spectate/gate_spectate_client.h"
#include "spectate/gate_spectate_ticket.h"

#include <atomic>
#include <chrono>
#include <cstdint>
#include <cstdlib>
#include <memory>
#include <string>
#include <unordered_map>
#include <utility>
#include <vector>

namespace
{

    // 启动门禁:空 gate_token_secret 在生产模式下**拒绝启动**。
    //
    // 形态照抄 Go 侧 login/internal/svc/auth_init.go 的
    // validateDevelopmentPasswordMode:弱入口必须由显式运行模式授权,配置里
    // 恰好少了一项不算授权。只不过 Go 那边用 panic,这里用 LOG_FATAL —— 在
    // muduo 里它同样是"打完这行就 abort",是本仓一贯的"拒绝启动"惯用法
    // (见 node.cpp 的 RegisterKafkaHandlers 失败分支)。
    //
    // 调用时机必须晚于 Node 构造(Initialize -> LoadConfigs 才把 YAML 读进
    // gNodeConfigManager),所以放在 configure 回调的第一句,而不是 main() 开头。
    void ValidateGateTokenSecretOrDie()
    {
        const auto &resolution = gate_security::ResolveRunModeOnce();
        if (!resolution.recognized)
        {
            // 把 GATE_RUN_MODE 拼错成 "develop" / "yes" 之类会静默按生产跑。
            // 生产是安全侧,不该因此拒绝启动,但必须让人看见。
            LOG_WARN << "Unrecognized " << gate_security::kRunModeEnv << "='" << resolution.raw
                     << "', falling back to run_mode=prod. Valid values: prod|dev|test.";
        }

        const auto mode = resolution.mode;
        const auto &secret = gNodeConfigManager.GetBaseDeployConfig().gate_token_secret();
        switch (gate_security::ClassifyTokenSecret(secret, mode))
        {
        case gate_security::TokenSecretVerdict::kEnforce:
            LOG_INFO << "Client token verification ENFORCED, run_mode="
                     << gate_security::RunModeName(mode);
            break;
        case gate_security::TokenSecretVerdict::kDevBypass:
            LOG_WARN << "SECURITY WARNING: gate_token_secret is EMPTY and "
                     << gate_security::kRunModeEnv << "=" << gate_security::RunModeName(mode)
                     << " — client token verification is DISABLED and every connection will be"
                     << " auto-verified. NEVER run this configuration in production.";
            break;
        case gate_security::TokenSecretVerdict::kRefuse:
            // LOG_FATAL 会 abort。这正是要的效果:带着空密钥跑起来的 gate 等于
            // 一个不设防的入口,宁可 CrashLoopBackOff 让人立刻发现。
            LOG_FATAL << "Refusing to start: gate_token_secret is empty while run_mode=prod."
                      << " Set GateTokenSecret in etc/base_deploy_config.yaml, or set "
                      << gate_security::kRunModeEnv << "=dev|test for a local run.";
            break;
        }
    }

    // 启动门禁:生产 gate 不能把连接上限留成 proto3 默认值 0。
    //
    // 0 只给显式 dev/test 的本地调试使用。连接层即使遇到 0 也会保留
    // session-id 空间的硬上限,但若生产配置漏键、ConfigMap 挂载遮蔽或误写成 0
    // 后仍允许启动,期望的运维容量阈值会静默失效,退化到 131071 的兜底值。
    void ValidateGateConnectionLimitOrDie()
    {
        const auto mode = gate_security::CurrentRunMode();
        const auto maxConnections =
            gNodeConfigManager.GetBaseDeployConfig().gate_max_connections();
        // 为所有 node_id 保留 UINT32_MAX 这个拒连哨兵,所以可并发使用的
        // session id 至多是低 17 位的非哨兵取值数 kSeqMask(131071)。
        // 超过后 Generate() 会回绕,而活跃集合已占满时碰撞检查会永久自旋,
        // 把唯一 EventLoop 线程卡死。
        constexpr uint32_t kMaxSafeConnections = SessionIdGenerator::kSeqMask;

        if (maxConnections == 0)
        {
            if (gate_security::IsNonProdMode(mode))
            {
                LOG_WARN << "SECURITY WARNING: gate_max_connections=0 and "
                         << gate_security::kRunModeEnv << "="
                         << gate_security::RunModeName(mode)
                         << " -- the configured operational limit is DISABLED for this"
                         << " local run; the hard session-id cap " << kMaxSafeConnections
                         << " remains enforced.";
                return;
            }

            LOG_FATAL << "Refusing to start: gate_max_connections is 0 while run_mode=prod."
                      << " Set GateMaxConnections to a positive value in"
                      << " etc/base_deploy_config.yaml; unlimited connections are allowed"
                      << " only with " << gate_security::kRunModeEnv << "=dev|test.";
            return;
        }

        if (maxConnections > kMaxSafeConnections)
        {
            LOG_FATAL << "Refusing to start: gate_max_connections=" << maxConnections
                      << " exceeds the safe session-id capacity " << kMaxSafeConnections
                      << ". Lower GateMaxConnections; otherwise the session-id generator"
                      << " can wrap while every id is still active and stall the EventLoop.";
            return;
        }

        LOG_INFO << "Client connection limit ENFORCED, max_connections=" << maxConnections;
    }

    // 启动门禁:观战凭证密钥(docs/design/battle-spectate-tiered-fanout.md §15.7,口径同 battle 令牌)。
    // prod 下空密钥、过短、与 gate / battle 令牌密钥相同都拒绝启动:密钥短了凭证可以伪造给任意玩家,相同则信任域没分开。
    // dev / test 放行并 WARN。这里的放行只是让进程起得来,不是跳过校验:没有密钥时所有 SubscribeSpectate 一律拒绝
    // (gate_spectate_ticket.h),本机也不注册观战帧消费者,观众只能走直连。
    void ValidateSpectateTokenSecretOrDie()
    {
        const auto mode = gate_security::CurrentRunMode();
        const auto &deploy = gNodeConfigManager.GetBaseDeployConfig();
        const auto &secret = deploy.spectate_token_secret();
        switch (gate_security::ClassifyTokenSecret(secret, mode))
        {
        case gate_security::TokenSecretVerdict::kEnforce:
            break;
        case gate_security::TokenSecretVerdict::kDevBypass:
            LOG_WARN << "spectate_token_secret is EMPTY and " << gate_security::kRunModeEnv << "="
                     << gate_security::RunModeName(mode)
                     << " -- every SubscribeSpectate will be rejected and spectate frame consumers are not registered;"
                     << " spectators can only use the direct path.";
            return;
        case gate_security::TokenSecretVerdict::kRefuse:
            LOG_FATAL << "Refusing to start: spectate_token_secret is empty while run_mode=prod."
                      << " Set SpectateTokenSecret in etc/base_deploy_config.yaml, or set "
                      << gate_security::kRunModeEnv << "=dev|test for a local run.";
            return;
        }

        const char *problem = nullptr;
        switch (gate_spectate::ClassifySecretStrength(secret, deploy.gate_token_secret(), deploy.battle_token_secret()))
        {
        case gate_spectate::SecretStrengthVerdict::kOk:
            LOG_INFO << "Spectate ticket verification ENFORCED, run_mode=" << gate_security::RunModeName(mode);
            return;
        case gate_spectate::SecretStrengthVerdict::kTooShort:
            problem = "is shorter than the required minimum";
            break;
        case gate_spectate::SecretStrengthVerdict::kSameAsGate:
            problem = "equals gate_token_secret (trust domains must be separate)";
            break;
        case gate_spectate::SecretStrengthVerdict::kSameAsBattle:
            problem = "equals battle_token_secret (trust domains must be separate)";
            break;
        }
        if (gate_security::IsNonProdMode(mode))
        {
            LOG_WARN << "spectate_token_secret " << problem << "; tolerated only because run_mode="
                     << gate_security::RunModeName(mode) << ", minimum_bytes=" << gate_spectate::kMinSpectateSecretBytes;
            return;
        }
        LOG_FATAL << "Refusing to start: spectate_token_secret " << problem
                  << " while run_mode=prod, minimum_bytes=" << gate_spectate::kMinSpectateSecretBytes;
    }

    struct GateRuntimeContext
    {
        ProtobufDispatcher protobufDispatcher;
        ProtobufCodec codec;
        GateClientSessionHandler clientSessionHandler;
        DependencyGate dependencyGate;
        TimerTaskComp playerCountReportTimer;
        // CPP-2 进场转发欠账的到期补发(handler/event/scene_entry_dispatch.cpp)。回调不捕获任何对象;
        // TimerTaskComp 自带存活令牌(AGENTS.md §11.7)。定时器只负责唤醒,截止判断用 steady_clock。
        TimerTaskComp sceneEntryRetryTimer;
        // 路由 lane 在途调用的本地截止扫描(handler/rpc/client_request_lane.cpp)。回调不捕获任何对象;
        // 必须主动扫:队头卡住时客户端在等回包,不会再发新请求触发惰性清理(client-request-ordering.md §4.5)。
        TimerTaskComp routerLaneExpiryTimer;
        // 观战订阅中心(spectate/gate_spectate_hub.h)。定时器与 Kafka 回调都不捕获对象,经 gate_spectate_client.h 的入口
        // 按弱引用找到它;在 Node 之后析构,这时 Kafka 消费已经停了。
        std::shared_ptr<gate_spectate::SpectateHub> spectateHub;
        TimerTaskComp spectateSweepTimer;
        bool sourceClosed = false;

        GateRuntimeContext()
            : protobufDispatcher([](const TcpConnectionPtr &conn, const MessagePtr &msg, Timestamp)
                                 {
            // typeName 可由公网客户端控制,不能逐帧 ERROR;拒绝后强关,codec 会
            // 丢弃同一批 pipeline 的剩余帧。
            static uint64_t unknownMessageCount = 0;
            if ((unknownMessageCount++ & 0x3FF) == 0)
            {
                LOG_ERROR << "Unknown protobuf messages rejected (sampled), latest_type="
                          << std::string(msg->GetTypeName())
                          << " rejected_total=" << unknownMessageCount;
            }
            conn->forceClose(); }),
              codec([this](const TcpConnectionPtr &conn, const MessagePtr &msg, Timestamp ts)
                    { protobufDispatcher.onProtobufMessage(conn, msg, ts); }),
              clientSessionHandler(codec, protobufDispatcher)
        {
        }
    };

    struct GateNodeHooks
    {
        using KafkaCommandType = contracts::kafka::GateCommand;
        static void DispatchKafkaCommand(const std::string& topic, const KafkaCommandType& command)
        {
            DispatchGateKafkaCommand(topic, command);
        }
    };

    // 维护控制模式下启动的 boot(判据与 node.cpp InitMaintenanceControl 相同:变量存在且非空)。
    bool IsMaintenanceControlledBoot()
    {
        const char *directory = std::getenv("MMORPG_MAINTENANCE_CONTROL_DIR");
        return directory != nullptr && directory[0] != '\0';
    }

    // 观战帧的两个消费者(docs/design/battle-spectate-tiered-fanout.md §15.3)。消息体就是 SpectateFrameEvent,不套 GateCommand。
    // 回调已在主 loop 上。没有密钥时不注册:订阅全拒,收帧没有去处(prod 下空密钥进程已拒启,走到这里只会是 dev / test)。
    bool RegisterSpectateFrameConsumers(Node &gateNode)
    {
        const auto &secret = gNodeConfigManager.GetBaseDeployConfig().spectate_token_secret();
        if (token_security::TrimAscii(secret).empty())
        {
            return true;
        }

        using contracts::kafka::SpectateFrameEvent;
        const auto onFrame = [](const std::string &, const SpectateFrameEvent &event)
        { gate_spectate::OnKafkaFrame(event); };

        // L1 中继:与 gate-cmd 同一分区契约(node_id % 命令 topic 分区数),target_gate_id / target_instance_id 过滤。
        auto relayOptions = node::kafka::BuildDefaultKafkaOptions(gateNode.GetNodeType());
        relayOptions.commandTopicBase = node::kafka::kSpectateRelayTopicBase;
        // 观战帧是持续流:这台 gate 重启拿回同一 node_id 后,battle 的中继表还指着前任实例(没有摘除通道,只有这台 gate 上来新观众
        // 重新挂接才覆盖),这些场每回合一帧都按实例不符丢弃。1000 场、3 秒一回合约 330 行 / 秒,不采样就全写在主 loop 上。
        // 广播 options 由这里复制,两条 topic 共用一个计数。
        relayOptions.targetFilter.staleInstanceDropCounter = std::make_shared<std::atomic<uint64_t>>(0);
        if (!node::kafka::RegisterKafkaCommandHandler<SpectateFrameEvent>(gateNode, relayOptions, onFrame))
        {
            return false;
        }

        // 维护控制模式的 manual consumer 证据要求「一个消费者只 assign 本节点的一个分区」(node.cpp RegisterKafkaMessageHandler、
        // kafka_consumer.cpp ReadManualLocked),全分区消费者放不进去,硬注册会把整个 gate 打成 LOG_FATAL。
        // 维护 boot 是单个具名节点的受控过程;这台 gate 上的 L2 观众收不到帧,靠空闲巡检收尾,不碰资产,代价可接受。
        if (IsMaintenanceControlledBoot())
        {
            LOG_WARN << "Spectate broadcast consumer is not registered on a maintenance-controlled boot:"
                     << " L2 spectators on this gate end with SPECTATE_END_SOURCE_LOST.";
            return true;
        }

        // L2 广播:assign 全部分区,两个目标字段留空(现有过滤规则放行),防僵尸靠 SpectateHub 比对 battle_instance_id。
        auto broadcastOptions = relayOptions;
        broadcastOptions.commandTopicBase = node::kafka::kSpectateBroadcastTopicBase;
        broadcastOptions.assignAllPartitions = node::kafka::kSpectateBroadcastTopicPartitions;
        return node::kafka::RegisterKafkaCommandHandler<SpectateFrameEvent>(gateNode, broadcastOptions, onFrame);
    }

    bool RegisterGateKafkaConsumers(Node &gateNode)
    {
        if (!node::kafka::RegisterKafkaCommandHandler<GateNodeHooks::KafkaCommandType>(
                gateNode, &GateNodeHooks::DispatchKafkaCommand))
        {
            return false;
        }
        return RegisterSpectateFrameConsumers(gateNode);
    }

    // 所有节点类型上的 gRPC 通道都没有未完成的调用(关机排空用:断线通知发出并拿到结果之后,才允许清掉通道)。
    // 路由模式下通知走路由服通道,直连模式走 login 通道;两者都在这里,不按模式区分。
    bool AllGrpcChannelsIdle()
    {
        for (auto &registry : tlsNodeContextManager.GetAllRegistries())
        {
            for (const auto &[entity, channel] : registry.view<MuduoGrpcChannelPtr>().each())
            {
                if (channel && channel->outstandingCalls() != 0)
                {
                    return false;
                }
            }
        }
        return true;
    }

} // namespace

int main(int argc, char *argv[])
{
    // 启动首行。事故复盘第一个问题永远是"线上跑的到底是哪一版",这一行直写
    // stdout(不经 muduo,不受 LogLevel 影响),即使进程在 etcd 阶段就崩掉,
    // 版本三元组也已经落进容器日志。node_id 要等 etcd CAS、zone_id 要等
    // LoadConfigs,这时都还没有,所以先打 pending;两者到位后 SetAfterStart 里
    // 再补一条同前缀的完整六元组行。
    gate_version::PrintStartupLine("GATE", nullptr, nullptr);

    // Gate's outbound connections, by transport:
    //   * SceneNodeService — muduo TCP RPC (in-zone Gate↔Scene messaging).
    //   * LoginNodeService / SceneManagerNodeService — gRPC, dispatched via
    //     PickRandomNode in client_session_handler (msg_id=48 login,
    //     scene-manager redirects, etc.). They MUST appear in the whitelist
    //     so ConnectAllNodes / AddServiceNode wire them into the local
    //     entt::registry; otherwise PickRandomNode finds an empty registry
    //     and rejects every request with "Node not found, message id: 48".
    // Cross-zone is handled by ServiceDiscoveryManager's zone filter
    // (see NodeUtils::IsZoneScopedNodeType): only same-zone Login/Scene
    // are inserted, while SceneManager (cross-zone by design) is global.
    // Other gates are reached via Kafka (`gate-{id}` topic). An empty
    // whitelist would make Gate-1 connect to Gate-2's client-facing TCP
    // listener; their codecs (ProtobufCodec vs RpcCodec) are incompatible
    // and the receiver logs `ProtobufCodec::defaultErrorCallback -
    // InvalidNameLen` on every reconnect (~2 Hz).
    //
    //   * BattleNodeService **两种模式都不进白名单**(turn-based §22 D66):战斗上行与
    //     战斗帧只走客户端直连 battle,gate 不中继、不持 battle stub、不维护战斗会话绑定;
    //     经 gate 发来的 BattleClientPlayer 号在 DispatchClientRpcMessage 统一回
    //     kServiceUnavailable。battle 发往大厅的开局公告仍经 Kafka gate-{id} 的
    //     PushToPlayerEvent 下发,与白名单无关。
    //   * MatchNodeService — 匹配服务(Go gRPC,无状态随机路由):客户端
    //     JoinQueue/ChallengePlayer/WatchBattle 等按 NODE_MATCH 路由,缺席则
    //     全部报 "Node not found ... message id: 157"(2026-09-01 冒烟补)。
    //
    //   * ClientRpcRouterNodeService — 客户端 RPC 路由服(Go gRPC,全局池,不分 zone;
    //     docs/design/client-rpc-router.md D29–D34)。GATE_CLIENT_RPC_ROUTER=1 时它是
    //     gate **唯一**的 gRPC 目标:白名单收成 {Scene(TCP), ClientRpcRouter},gate 对
    //     login / scene_manager / match **零 stub、零 channel**,gRPC 连接数 =
    //     路由服副本数,与业务服务数量无关 —— 以后加 chat / friend / guild 不再碰 gate。
    //     旧模式(默认,未设或非 1/true/on)完全不连路由服,除战斗(两种模式都不经 gate,
    //     见上)外行为与改前一致;所以路由服可以先于 gate 切换单独上线,回退 = 去掉环境
    //     变量再滚动(设计文档 §6 灰度)。
    const bool routerMode = gate_router_mode::IsRouterModeEnabled();
    const Node::CanConnectNodeTypeList connectTo = routerMode
        ? Node::CanConnectNodeTypeList{SceneNodeService, eNodeType::ClientRpcRouterNodeService}
        : Node::CanConnectNodeTypeList{SceneNodeService, LoginNodeService, SceneManagerNodeService, MatchNodeService};

    // 检查旧的 NODE_ETCD_TRANSPORT 环境变量(etcd 只剩 muduo_h2 传输;设成 grpc 会打 WARN),须在 Node 构造前调用。
    etcd_transport::InstallFromEnv();
    return node::entry::RunSimpleNodeMainWithOwnedContext<GateHandler, GateRuntimeContext, GateNodeHooks>(
        GateNodeService,
        connectTo,
        [connectTo, routerMode](Node &node, GateRuntimeContext &context)
        {
            // 先过安全门禁,再做任何别的初始化:配置有问题就不该把服务拉起来。
            // 此处 Node 构造已完成,LoadConfigs 读过 etc/base_deploy_config.yaml。
            ValidateGateTokenSecretOrDie();
            ValidateGateConnectionLimitOrDie();
            ValidateSpectateTokenSecretOrDie();

            // 观战订阅中心。hub 先建好:客户端消息、会话关闭与 Kafka 回调都经 gate_spectate_client.h 的入口找它。
            context.spectateHub = gate_spectate::InstallHub(
                *node.GetLoop(), gNodeConfigManager.GetBaseDeployConfig().spectate_token_secret());
            // 覆盖 node_entry 按 GateNodeHooks 装的默认钩子:gate-cmd 的注册不变,同一时机追加两个观战帧消费者。
            node.SetKafkaHandlers([](Node &n)
                                  { return RegisterGateKafkaConsumers(n); });

            // 启动日志:出口模式与出站白名单一起打。事故复盘要能一眼看出"这台 gate
            // 连的是路由服还是业务服务",不能靠翻容器的环境变量。
            {
                const std::vector<uint32_t> whitelistForLog(connectTo.begin(), connectTo.end());
                LOG_INFO << "gate client message outbound mode=" << gate_router_mode::RouterModeName(routerMode)
                         << "(" << gate_router_mode::kRouterModeEnv << "=1|true|on selects router;other values select direct)"
                         << ", outbound allowlist=" << Node::FormatNodeTypeNames(whitelistForLog)
                         << (routerMode
                                 ? ", router mode:gate has no stubs for Go business services;gRPC connection count equals router replica count;all battle messages rejected(direct connections only)"
                                   ";per-session router lane enabled(client gRPC requests ordered per session, depth limit " + std::to_string(gate_lane::kMaxLaneDepth) + ", max queue wait " + std::to_string(gate_lane::kMaxLaneWait.count()) + "ms)"
                                 : ", direct mode:non-battle behavior matches behavior before router introduction;all battle messages rejected(direct connections only)"
                                   ";client requests are NOT ordered per session(router lane is router-mode only, client-request-ordering.md O7; goes away with the direct path in D61)");
            }

            // CPP-2 链路就绪印章:TCP 连上 0.5s 后才发握手,scene 在收到握手时才为本 gate 挂 RpcSession,之前发出的
            // 进场通知会被丢掉,所以「可以向这个 scene 交付进场」要以握手成功为准。conn 是发出握手的那条出站连接
            // (在途调用在 DOWN 时已全部失败,成功回调里它必然就是回包所走的连接),按连接对象盖章,重连后旧章自然失效。
            // 以前写在按 message_id 注册的 OnSceneNodeHandshakeReply 里(docs/design/muduo-rpc-channel.md §8.3)。
            node.GetNodeRegistrationManager().SetHandshakeSucceededHook(
                [](uint32_t nodeType, const muduo::net::TcpConnectionPtr &conn, const NodeHandshakeResponse &response)
                {
                    if (nodeType != SceneNodeService)
                    {
                        return;
                    }
                    gate_scene_entry::OnSceneLinkHandshaken(conn, response.peer_node().node_uuid(),
                                                            std::chrono::steady_clock::now());
                });

            // GATE_ROUTER_TRANSPORT 已失效(docs/design/grpc-muduo-transport.md §14.1):官方 gRPC 客户端随第 3 批删除,
            // 所有出站 gRPC 都走 muduo_h2:节点通道(路由服、直连模式的业务服务)由 NodeConnector::ConnectToGrpcNode 统一建,
            // etcd 走 MuduoEtcdTransport。
            // 只为兼容旧部署留这一处检查:不设或 muduo_h2 静默;其余值(含旧默认 grpc)打 WARN —— 设成 grpc 的部署以为
            // 自己还在官方实现上,必须让人看见它已被忽略。与路由模式开关 GATE_CLIENT_RPC_ROUTER(业务拓扑)是两回事。
            if (const char *rawTransport = std::getenv("GATE_ROUTER_TRANSPORT"))
            {
                const std::string transport = token_security::ToLowerAscii(token_security::TrimAscii(rawTransport));
                if (!transport.empty() && transport != "muduo_h2")
                {
                    LOG_WARN << "GATE_ROUTER_TRANSPORT='" << rawTransport
                             << "' is ignored: the official gRPC client was removed; all outbound gRPC uses muduo_h2.";
                }
            }

            // Disconnect all client sessions before shutdown (SIGTERM or conflict).
            auto disconnectAllClients = [&context](Node &)
            {
                context.sourceClosed = true;
                context.dependencyGate.probeTimer.Cancel();
                context.playerCountReportTimer.Cancel();
                auto &sessions = tlsSessionManager.sessions();
                LOG_INFO << "Disconnecting " << sessions.size() << " client sessions before shutdown...";
                for (auto &[sessionId, info] : sessions)
                {
                    if (const auto conn = info.conn.lock())
                    {
                        conn->forceClose();
                    }
                }
                LOG_INFO << "All client sessions disconnected.";
            };
            // 关机阶段 0(node.cpp FinalizeShutdownInLoop)会同步清掉全部 gRPC 通道。gate 没有 gRPC server,
            // beforeShutdown 之后同一调用栈就会走到 Finalize,而被 forceClose 的会话要到下一轮才进断线处理 ——
            // 那时通道已经没了,Login.Disconnect 通知发不出去,login 侧会话停在 ONLINE。所以先等 beforeShutdown
            // 关掉的会话都走完断线处理、gRPC 通道上没有在途调用,再放行 Finalize;节点的 15s 排空预算兜底(gate 调用的
            // deadline 最长 12s,在途调用必然先结束)。只等 beforeShutdown 当时的那批会话被移除;但「通道无在途调用」
            // 不区分调用属于谁,关机期间新连进来的会话的请求同样会推迟放行,上限仍是 15s 预算。
            // 第 3 批(docs/design/grpc-muduo-transport.md §14)以前这道等待只在 GATE_ROUTER_TRANSPORT=muduo_h2 时装,
            // 官方客户端路径保持旧行为(Finalize 时封住 CQ 写入口,随后的通知同样发不出去)。传输只剩一种之后不再按
            // 模式区分:直连模式(默认,通知走 login 通道)与路由模式(走路由服通道)都等。
            {
                auto closingSessions = std::make_shared<std::vector<SessionId>>();
                auto closeSource = [disconnectAllClients, closingSessions](Node &n)
                                       {
                    for (const auto &entry : tlsSessionManager.sessions())
                    {
                        closingSessions->push_back(entry.first);
                    }
                    disconnectAllClients(n); };
                node.SetBeforeShutdown(closeSource);
                node.SetMaintenanceInboundRelay(); // 内部推送/路由是relay；客户端TCP根由closeSource关闭。
                node.SetMaintenanceSourceClose(closeSource);
                node.SetMaintenanceRelayStop([&context](Node &) { context.sceneEntryRetryTimer.Cancel(); context.routerLaneExpiryTimer.Cancel(); context.spectateSweepTimer.Cancel(); });
                node.SetBeforeTeardown([&context](Node &) { context.sceneEntryRetryTimer.Cancel(); context.routerLaneExpiryTimer.Cancel(); context.spectateSweepTimer.Cancel(); });
                node.SetShutdownDrainComplete([closingSessions](Node &)
                                              {
                    const auto &sessions = tlsSessionManager.sessions();
                    for (const SessionId sessionId : *closingSessions)
                    {
                        if (sessions.contains(sessionId))
                        {
                            return false;
                        }
                    }
                    return AllGrpcChannelsIdle(); });
            }
            node.SetOnConflictShutdown([disconnectAllClients](Node &n, NodeIdConflictReason)
                                       { disconnectAllClients(n); });

            InitGateCodec(context.codec);

            // Post-startup: attach client TCP callbacks + initialize session ID generator.
            // Override connection/message callbacks BEFORE WaitAndRun so that any
            // client connecting while dependencies are still pending gets the correct
            // ProtobufCodec (type-name format) instead of the default RPC0 codec,
            // which would cause kUnknownMessageType errors.
            node.SetAfterStart([&context](Node &n)
                               {
                        // session_id 的 node 段必须在装 connection 回调之前种好。
                        //
                        // 原来这一行在 dependencyGate.WaitAndRun 的 ready 回调里,而
                        // setConnectionCallback 在它之前就装上了 —— 于是在等待
                        // Login/Scene 被发现的这段窗口里连进来的客户端,拿到的
                        // session_id 里 node 段是 0(TransientNodeCompositeIdGenerator
                        // 的 node_id_ 默认 0)。scene 侧 GetGateNodeId(session_id) 得到 0,
                        // ResolveLocalZoneGateEntity 永远解析不出归属 gate,这个玩家
                        // 整局都收不到任何服务端下行,而且没有任何自愈路径。
                        //
                        // node_id 在这里已经是终值:Node 打完启动 banner(banner 里就
                        // 打印了 GetNodeId())紧接着才调 afterStartFn_,见 node.cpp:928。
                        tlsSessionManager.session_id_gen().set_node_id(n.GetNodeId());

                        // 版本行第二次:node_id / zone_id 到这里都是终值了(理由同上),
                        // 补一条带完整六元组的 [gate_version]。stdout + 日志各打一份 ——
                        // stdout 那份不受 LogLevel 影响,日志那份进归档文件。
                        {
                            const std::string nodeIdText = std::to_string(n.GetNodeId());
                            const std::string zoneIdText =
                                std::to_string(gNodeConfigManager.GetGameConfig().zone_id());
                            gate_version::PrintStartupLine("GATE", nodeIdText.c_str(), zoneIdText.c_str());
                            LOG_INFO << gate_version::StartupLine("GATE", nodeIdText.c_str(), zoneIdText.c_str());
                        }

                        n.GetTcpServer().setConnectionCallback(
                            [&context](const TcpConnectionPtr& conn) {
                                if (context.sourceClosed && conn->connected()) { conn->forceClose(); return; }
                                context.clientSessionHandler.OnConnection(conn);
                            });
                        n.GetTcpServer().setMessageCallback(
                            [&context](const TcpConnectionPtr& conn, muduo::net::Buffer* buf, Timestamp ts) {
                                if (context.sourceClosed) { buf->retrieveAll(); conn->forceClose(); return; }
                                context.codec.onMessage(conn, buf, ts);
                            });

                        // 进场转发补发的扫描紧跟客户端回调装上、不放进依赖门的回调里:客户端连进来之后就可能
                        // 有会话与路由,依赖迟迟不就绪时欠账也不能因为没人扫而错过截止收口。
                        // 正常情况下欠账索引为空,每轮只做一次 empty()。
                        context.sceneEntryRetryTimer.RunEvery(gate_scene_entry::kSweepIntervalSeconds, [] {
                            gate_scene_entry::RetryDueSceneEntries(std::chrono::steady_clock::now());
                        });
                        // 路由 lane 的在途过期扫描同理:客户端连进来之后就可能有在途调用。在途索引为空时每轮只做一次堆顶比较。
                        context.routerLaneExpiryTimer.RunEvery(gate_lane::kSweepIntervalSeconds, [] {
                            gate_lane::ExpireDue(std::chrono::steady_clock::now());
                        });
                        // 观战空闲巡检与指标行:回调不捕获对象,hub 按弱引用找,没有了就是空操作(AGENTS.md §11.7)。
                        context.spectateSweepTimer.RunEvery(gate_spectate::kSweepIntervalSeconds, [] {
                            gate_spectate::SweepIdleNow();
                        });

                        // 依赖门:路由模式下等的是路由服而不是 login —— gate 这时根本不连
                        // login,等它永远等不到;Scene(TCP 中继)两种模式都要等。
                        const std::vector<uint32_t> requiredDependencies = gate_router_mode::IsRouterModeEnabled()
                            ? std::vector<uint32_t>{eNodeType::ClientRpcRouterNodeService, SceneNodeService}
                            : std::vector<uint32_t>{LoginNodeService, SceneNodeService};
                        context.dependencyGate.WaitAndRun(n, requiredDependencies, [&context](Node &n)
                                                                   {
                        // Report player_count to etcd every 10 seconds for load balancing
                        context.playerCountReportTimer.RunEvery(10.0, [&n] {
                            auto count = static_cast<uint32_t>(tlsSessionManager.sessions().size());
                            n.MutableNodeInfo().set_player_count(count);
                            n.GetEtcdManager().UpdateNodeInfo();
                        }); }, "Gate"); });
        });
}
