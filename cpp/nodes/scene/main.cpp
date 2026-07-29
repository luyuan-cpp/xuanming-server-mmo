#include "muduo/net/EventLoop.h"
#include "muduo/base/Logging.h"

#include "node/system/node/node_entry.h"
#include "agones/agones_scene_lifecycle.h"
#include "handler/rpc/scene_handler.h"
#include "handler/grpc/scene_node_service.h"
#include "core/config/config.h"
#include "world/world.h"
#include "core/system/redis.h"
#include "frame/manager/frame_time.h"
#include "player/system/player_lifecycle.h"
#include "player/system/cross_zone_reaper.h"
#include "kafka/system/kafka.h"
#include "proto/contracts/kafka/scene_command.pb.h"

using namespace muduo;
using namespace muduo::net;

namespace
{

    struct SceneRuntimeContext
    {
        TimerTaskComp worldTimer;
        DependencyGate dependencyGate;
        SceneNodeGrpcImpl grpcService;

        explicit SceneRuntimeContext(EventLoop& loop) : grpcService(loop) {}
    };

    struct SceneNodeHooks
    {
        struct TableLoadHandler
        {
            static void OnLoaded() { ConfigSystem::OnConfigLoadSuccessful(); }
        };
        using KafkaCommandType = contracts::kafka::SceneCommand;
    };

} // namespace

int main(int argc, char *argv[])
{
    return node::entry::RunNodeMain([](EventLoop &loop)
                                    {
        node::entry::detail::ApplyPreConstructionHooks<SceneNodeHooks>();

        auto context = std::make_unique<SceneRuntimeContext>(loop);

        SceneHandler handler;
        Node node(&loop, SceneNodeService,
                  Node::CanConnectNodeTypeList{ SceneManagerNodeService },
                  &handler);

        node::entry::detail::ApplyPostConstructionHooks<SceneNodeHooks>(node);

        node.RegisterGrpcService(&context->grpcService);

        node::entry::detail::InstallSignalHandlers(loop);

        tlsRedisSystem.Initialize(&loop);
        World::InitializeSystemBeforeConnect();

        // SIGTERM / conflict safety net: save all players via the full exit flow.
        // The preferred shutdown path is GmGracefulShutdown RPC, which also calls
        // HandleExitGameNode for each player before triggering Shutdown().
        auto exitAllPlayers = [](Node &)
        {
            // 先停 Agones lifecycle worker 并 join,再动玩家数据。
            // 顺序不能反:worker 还在跑的时候进程正在退出,health / ready 请求
            // 会打到一个正在拆的对象上。
            //
            // 刻意**不**在这里调 POST /shutdown —— SIGTERM 通常正是 Agones
            // 删 Pod 发出来的,再回敬一个 /shutdown 就是递归触发删除。
            // 只有进程自己决定自我终止时才调 RequestShutdown()。
            agones::SceneLifecycle::Instance().Stop();

            auto view = tlsEcs.actorRegistry.view<Player>();
            LOG_INFO << "Emergency save: exiting " << view.size() << " online players before shutdown...";
            for (auto entity : view)
            {
                PlayerLifecycleSystem::HandleExitGameNode(entity);
            }
            LOG_INFO << "All players exited.";
        };
        node.SetBeforeShutdown(exitAllPlayers);

        // 身份冲突(etcd 租约过期 / node_id 被别人抢走)和普通 SIGTERM 不是一回事:
        // 这台节点马上就不再是 node_id 的合法持有者,但玩家的 gate 会话还连着。
        // 所以除了存盘,还要把玩家改派到别的 scene node 的大世界频道 ——
        // 否则他们的会话指着一具尸体,只能等自己发现掉线再重登。
        //
        // 顺序由 PlayerLifecycleSystem 保证:抄会话 -> 存盘 -> 存盘落地后才发改派请求。
        // 反过来做的话,新节点会从 Redis 读到存盘前的旧数据(回档)。
        node.SetOnConflictShutdown([](Node &, NodeIdConflictReason)
                                   {
            // 先停 Agones lifecycle worker(理由同 exitAllPlayers 里的注释),
            // 再动玩家数据。Stop() 幂等。
            agones::SceneLifecycle::Instance().Stop();
            PlayerLifecycleSystem::BeginEmergencyRelocateAll(); });

        // 有界 drain:存盘全部落地、改派全部派发完才退出。
        // 到期兜底(Redis / SceneManager 卡住时)由 Node 的看门狗负责,不会无限等。
        node.SetConflictDrainComplete([](Node &)
                                      { return PlayerLifecycleSystem::IsEmergencyRelocateDrained(); });

        node.SetAfterStart([&context](Node& n) {
            // Agones 生命周期。放在 SetAfterStart 里是有意的:此时 gRPC server
            // 已监听、依赖已初始化、etcd 注册已完成,进程确实可以接活了,
            // 这时候才有资格 POST /ready。提前 Ready 会让 Agones 把还没准备好的
            // 进程标成可分配。
            //
            // 非 Agones 环境(本地开发 / 普通 Deployment):ReadAgonesEnv() 读不到
            // AGONES_SDK_HTTP_PORT,或 Windows 构建拿不到 curl 传输层,
            // 都会退化成 Disabled —— 不起线程、不发 HTTP、所有 gate 直接放行。
            {
                const auto agonesEnv = agones::ReadAgonesEnv();
                std::unique_ptr<agones::HttpTransport> transport;
                if (agonesEnv.enabled)
                {
                    transport = agones::MakeDefaultHttpTransport();
                }
                if (transport == nullptr)
                {
                    LOG_INFO << "Agones lifecycle disabled (enabled=" << agonesEnv.enabled
                             << ", transport=" << (agonesEnv.enabled ? "unavailable" : "n/a") << ")";
                    agones::SceneLifecycle::Instance().StartDisabled();
                }
                else
                {
                    agones::SceneLifecycle::Instance().Start(
                        std::move(transport), agonesEnv.BaseUrl(), agones::LifecycleOptions{});
                }
            }

            // Subscribe cross-zone migration topics.
            //
            // `player_migrate`     — destination side: receive players migrating
            //                        INTO this zone's scene nodes from elsewhere
            //                        (HandlePlayerMigration → InitPlayerFromAllData
            //                        → publish ACK).
            // `player_migrate_ack` — source side: receive ACKs confirming the
            //                        destination loaded the player so we can
            //                        clear PlayerFrozenComp + DestroyPlayer
            //                        (HandlePlayerMigrationAck).
            //
            // groupId is per-node-id so each scene node has its own consumer
            // group and consumes every relevant message — partition-key=playerId
            // ensures same-player ordering. If you put multiple nodes in the
            // same consumer group they'll round-robin partitions and miss ACKs
            // intended for their own outgoing migrations.
            //
            // See docs/design/cross-zone-readiness-audit.md §3 (Kafka self-
            // orchestrated design) and §10.3 option A (this wiring) for context.
            // Without this subscription the audit doc's "件 2/件 3" code paths
            // exist but never fire.
            const std::string crossZoneGroupId =
                "scene-cross-zone-" + std::to_string(n.GetNodeId());
            if (!n.RegisterKafkaMessageHandler(
                    {"player_migrate", "player_migrate_ack"},
                    crossZoneGroupId,
                    &KafkaSystem::KafkaMessageHandler))
            {
                LOG_ERROR << "Failed to subscribe cross-zone Kafka topics; "
                          << "cross-zone migration will not work on this node "
                          << "(group_id=" << crossZoneGroupId << ").";
            }

            // Start the cross-zone reaper. Without this the Frozen state
            // pattern (player_lifecycle.cpp) can leak — a player whose
            // `player_migrate_ack` is dropped or whose destination crashes
            // would stay frozen forever. The reaper scans the Redis
            // `player_migration:*` records every 10s, re-publishes
            // migrations whose ACK is overdue, and unfreezes / notifies
            // the client when retries are exhausted. It also performs a
            // restart-recovery sweep on the way in (StartTick calls
            // ScanAndRecover before arming the periodic timer) so source-
            // side crashes don't leak Redis records either.
            //
            // See docs/design/cross-zone-readiness-audit.md §3.2 件 3 + §7.
            CrossZoneReaper::StartTick(n.GetLoop());

            context->dependencyGate.WaitAndRun(n, { SceneManagerNodeService },
                [&context](auto&) {
                    context->worldTimer.RunEvery(tlsFrameTimeManager.frameTime.delta_time(), World::Update);
                }, "Scene");
        });

        loop.loop();

        // loop 退出后的兜底 join。SetBeforeShutdown 已经调过一次,Stop() 幂等;
        // 但走 conflict-shutdown 之类的分支时不保证走过那条路径。
        agones::SceneLifecycle::Instance().Stop(); });
}