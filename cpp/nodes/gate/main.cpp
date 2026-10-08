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
#include "handler/rpc/gate_service_handler.h"
#include "handler/rpc/client_message_processor.h"
#include "gate_codec.h"
#include "gate_router_mode.h"
#include "gate_security.h"
#include "gate_version.h"
#include "node_config_manager.h"
#include "grpc_client/grpc_init_client.h"
#include "session/system/session.h"
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

#include <chrono>
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

    struct GateRuntimeContext
    {
        ProtobufDispatcher protobufDispatcher;
        ProtobufCodec codec;
        RpcClientSessionHandler rpcClientHandler;
        DependencyGate dependencyGate;
        TimerTaskComp playerCountReportTimer;
        // CPP-2 进场转发欠账的到期补发(handler/event/scene_entry_dispatch.cpp)。回调不捕获任何对象;
        // TimerTaskComp 自带存活令牌(AGENTS.md §11.7)。定时器只负责唤醒,截止判断用 steady_clock。
        TimerTaskComp sceneEntryRetryTimer;

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
              rpcClientHandler(codec, protobufDispatcher)
        {
        }
    };

    struct GateNodeHooks
    {
        using KafkaCommandType = contracts::kafka::GateCommand;
    };

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
    //     PickRandomNode in client_message_processor (msg_id=48 login,
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

    return node::entry::RunSimpleNodeMainWithOwnedContext<GateHandler, GateRuntimeContext, GateNodeHooks>(
        GateNodeService,
        connectTo,
        [connectTo, routerMode](Node &node, GateRuntimeContext &context)
        {
            // 先过安全门禁,再做任何别的初始化:配置有问题就不该把服务拉起来。
            // 此处 Node 构造已完成,LoadConfigs 读过 etc/base_deploy_config.yaml。
            ValidateGateTokenSecretOrDie();
            ValidateGateConnectionLimitOrDie();

            // 启动日志:出口模式与出站白名单一起打。事故复盘要能一眼看出"这台 gate
            // 连的是路由服还是业务服务",不能靠翻容器的环境变量。
            {
                const std::vector<uint32_t> whitelistForLog(connectTo.begin(), connectTo.end());
                LOG_INFO << "gate 客户端消息出口模式=" << gate_router_mode::RouterModeName(routerMode)
                         << "(" << gate_router_mode::kRouterModeEnv << "=1|true|on 为 router,其它为 direct)"
                         << ", 出站白名单=" << Node::FormatNodeTypeNames(whitelistForLog)
                         << (routerMode
                                 ? ", 路由模式:gate 对 Go 业务服务零 stub,gRPC 连接数=路由服副本数,战斗消息一律拒绝(只走直连)"
                                 : ", 直连模式:除战斗外行为与引入路由服之前一致,战斗消息一律拒绝(只走直连)");
            }

            // Override the default Kafka dispatch with GateCommand-specific routing
            // that handles empty-payload events via fallback field mapping.
            node.SetKafkaHandlers([](Node &n)
                                  { return node::kafka::RegisterKafkaCommandHandler<contracts::kafka::GateCommand>(
                                        n, node::kafka::BuildDefaultKafkaOptions(n.GetNodeType()),
                                        DispatchGateKafkaCommand); });

            // Disconnect all client sessions before shutdown (SIGTERM or conflict).
            auto disconnectAllClients = [](Node &)
            {
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
            node.SetBeforeShutdown(disconnectAllClients);
            node.SetOnConflictShutdown([disconnectAllClients](Node &n, NodeIdConflictReason)
                                       { disconnectAllClients(n); });

            InitGateCodec(context.codec);

            // Build reverse map: response proto full_name -> message_id
            // Used to wrap gRPC replies in MessageContent for the client.
            static std::unordered_map<std::string, uint32_t> sResponseTypeToMsgId;
            for (uint32_t i = 0; i < kMaxRpcMethodCount; ++i)
            {
                const auto &meta = gRpcMethodRegistry[i];
                if (meta.responseProto)
                {
                    sResponseTypeToMsgId[std::string(meta.responseProto->GetDescriptor()->full_name())] = i;
                }
            }

            // gRPC response -> client TCP bridge (wrap in MessageContent)
            //
            // 路由服 ClientRpcRouter.Forward 的响应类型就是 MessageContent
            // (client-rpc-router.md D30):走下面第一个分支原样下发,路由模式不需要
            // 在这里加任何代码。上面的反查表因此会多一条
            // "MessageContent -> ClientRpcRouterForwardMessageId(176)",但那张表只在
            // reply **不是** MessageContent 时才查,这一条永远不会被命中;全表也只有
            // Forward 一条以 MessageContent 应答,不会挤掉别的回包类型的映射。
            SetIfEmptyHandler([&context](const ClientContext &ctx, const ::google::protobuf::Message &reply)
                              {
                auto sd = GetSessionDetailsByClientContext(ctx);
                if (!sd) return;
                auto it = tlsSessionManager.sessions().find(sd->session_id());
                if (it == tlsSessionManager.sessions().end()) return;
                const auto clientConn = it->second.conn.lock();
                if (!clientConn) return;   // 会话记录还在但连接已释放(仅关机窗口可能):当断开处理

                // If reply is already MessageContent, send directly.
                if (reply.GetDescriptor() == MessageContent::descriptor()) {
                    context.rpcClientHandler.SendMessageToClient(clientConn, reply);
                    return;
                }

                // Otherwise, wrap in MessageContent envelope for the client.
                std::string typeName(reply.GetDescriptor()->full_name());
                auto typeIt = sResponseTypeToMsgId.find(typeName);
                if (typeIt == sResponseTypeToMsgId.end()) {
                    LOG_ERROR << "No message_id mapping for gRPC response type: " << typeName.c_str();
                    return;
                }
                MessageContent mc;
                mc.set_serialized_message(reply.SerializeAsString());
                mc.set_message_id(typeIt->second);
                context.rpcClientHandler.SendMessageToClient(clientConn, mc); });

            // gRPC 调用失败 -> 客户端(docs/design/grpc-client-deadline-failure-callback.md §5 #8/#9)。
            //
            // 失败时服务端没有回写 initial metadata,上面按 GetSessionDetailsByClientContext 找会话的路走不通;
            // 会话从本次**发出**的 x-session-detail-bin 找回(生成的客户端保存了 Send 传入的原值,未经 Base64)。
            // 回 kServiceUnavailable 而不是具体业务码:传输失败 = 结果未知,下游可能已经执行(有副作用的 C2S
            // 靠 Go 侧幂等键兜重试)。会话已不在(断线通知 Login.Disconnect、客户端已断开)就只留日志。
            // 不带会话 metadata 的调用(gate 自己的 etcd 请求)与生成代码的默认行为一致:记 ERROR。
            // 下游整体不可用时失败频率 = 在线玩家的 gRPC 消息频率,日志按 1024 条采样(同 SendViaRouter)。
            SetIfEmptyFailedHandler([](const GrpcCallFailure &failure, const ::google::protobuf::Message & /*request*/)
                                    {
                const std::string *sessionBin = failure.FindSentMetadata(kSessionBinMetaKey);
                if (sessionBin == nullptr) {
                    LOG_ERROR << "gRPC " << failure.method << " failed: code=" << static_cast<int>(failure.status.error_code())
                              << " msg=" << failure.status.error_message();
                    return;
                }
                SessionDetails sessionDetails;
                if (!sessionDetails.ParseFromString(*sessionBin)) {
                    LOG_ERROR << "gRPC " << failure.method << " failed and its session metadata does not parse; client not notified";
                    return;
                }
                static uint64_t failedCount = 0;
                if ((failedCount++ & 0x3FF) == 0) {
                    LOG_WARN << "gRPC client call failed (sampled): method=" << failure.method
                             << " code=" << static_cast<int>(failure.status.error_code())
                             << " msg=" << failure.status.error_message()
                             << " latest_session_id=" << sessionDetails.session_id()
                             << " failed_total=" << failedCount;
                }
                const auto it = tlsSessionManager.sessions().find(sessionDetails.session_id());
                if (it == tlsSessionManager.sessions().end()) return;
                const auto clientConn = it->second.conn.lock();
                if (!clientConn) return;
                RpcClientSessionHandler::SendTipToClient(clientConn, kServiceUnavailable); });

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
                                context.rpcClientHandler.OnConnection(conn);
                            });
                        n.GetTcpServer().setMessageCallback(
                            [&context](const TcpConnectionPtr& conn, muduo::net::Buffer* buf, Timestamp ts) {
                                context.codec.onMessage(conn, buf, ts);
                            });

                        // 进场转发补发的扫描紧跟客户端回调装上、不放进依赖门的回调里:客户端连进来之后就可能
                        // 有会话与路由,依赖迟迟不就绪时欠账也不能因为没人扫而错过截止收口。
                        // 正常情况下欠账索引为空,每轮只做一次 empty()。
                        context.sceneEntryRetryTimer.RunEvery(gate_scene_entry::kSweepIntervalSeconds, [] {
                            gate_scene_entry::RetryDueSceneEntries(std::chrono::steady_clock::now());
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
                            n.GetNodeInfo().set_player_count(count);
                            n.GetEtcdManager().UpdateNodeInfo();
                        }); }, "Gate"); });
        });
}
