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
#include "grpc_client/grpc_init_client.h"
#include "session/system/session.h"
#include "session/manager/session_manager.h"
#include "proto/scene_manager/scene_manager_service.pb.h"
#include "proto/contracts/kafka/gate_command.pb.h"
#include "proto/common/base/message.pb.h"
#include "rpc/service_metadata/rpc_event_registry.h"
#include "handler/event/gate_kafka_command_router.h"

#include <unordered_map>
#include <utility>

namespace
{

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
            LOG_ERROR << "Unknown message: " << std::string(msg->GetTypeName());
            conn->shutdown(); }),
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
        //
        //   * BattleNodeService — 回合制战斗节点(gRPC,全局池,不分 zone):
        //     gate 按 BindBattleEvent 的会话绑定把客户端战斗消息转发过去。
        //     必须进白名单,否则 AddServiceNode/ConnectAllNodes 不会为 battle
        //     建实体和 gRPC stub,绑定解析(FindNodeEntityByNodeId)永远落空。
        Node::CanConnectNodeTypeList{SceneNodeService, LoginNodeService, SceneManagerNodeService, BattleNodeService},
        [](Node &node, GateRuntimeContext &context)
        {
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