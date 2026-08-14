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
#include "gate_security.h"
#include "gate_version.h"
#include "node_config_manager.h"
#include "grpc_client/grpc_init_client.h"
#include "session/system/session.h"
#include "session/manager/session_manager.h"
#include "proto/scene_manager/scene_manager_service.pb.h"
#include "proto/contracts/kafka/gate_command.pb.h"
#include "proto/common/base/message.pb.h"
#include "rpc/service_metadata/rpc_event_registry.h"
#include "handler/event/gate_kafka_command_router.h"

#include <string>
#include <unordered_map>
#include <utility>

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

    return node::entry::RunSimpleNodeMainWithOwnedContext<GateHandler, GateRuntimeContext, GateNodeHooks>(
        GateNodeService,
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
        Node::CanConnectNodeTypeList{SceneNodeService, LoginNodeService, SceneManagerNodeService},
        [](Node &node, GateRuntimeContext &context)
        {
            // 先过安全门禁,再做任何别的初始化:配置有问题就不该把服务拉起来。
            // 此处 Node 构造已完成,LoadConfigs 读过 etc/base_deploy_config.yaml。
            ValidateGateTokenSecretOrDie();
            ValidateGateConnectionLimitOrDie();

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
                    if (info.conn)
                    {
                        info.conn->forceClose();
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
            SetIfEmptyHandler([&context](const ClientContext &ctx, const ::google::protobuf::Message &reply)
                              {
                auto sd = GetSessionDetailsByClientContext(ctx);
                if (!sd) return;
                auto it = tlsSessionManager.sessions().find(sd->session_id());
                if (it == tlsSessionManager.sessions().end()) return;

                // If reply is already MessageContent, send directly.
                if (reply.GetDescriptor() == MessageContent::descriptor()) {
                    context.rpcClientHandler.SendMessageToClient(it->second.conn, reply);
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
                context.rpcClientHandler.SendMessageToClient(it->second.conn, mc); });

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

                        context.dependencyGate.WaitAndRun(n, {LoginNodeService, SceneNodeService}, [&context](Node &n)
                                                                   {
                        // Report player_count to etcd every 10 seconds for load balancing
                        context.playerCountReportTimer.RunEvery(10.0, [&n] {
                            auto count = static_cast<uint32_t>(tlsSessionManager.sessions().size());
                            n.GetNodeInfo().set_player_count(count);
                            n.GetEtcdManager().UpdateNodeInfo();
                        }); }, "Gate"); });
        });
}
