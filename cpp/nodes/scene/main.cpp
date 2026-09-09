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
#include "battle/system/player_battle.h"
#include "services/battle/data/battle_table_fingerprint.h"
#include "kafka/system/kafka.h"
#include "node_config_manager.h"
#include "proto/contracts/kafka/scene_command.pb.h"
#include "id_segment_bootstrap.h"
#include "modules/id_segment/guid_segment_registry.h"

#include <chrono>
#include <limits>
#include <unordered_set>
#include <vector>

using namespace muduo;
using namespace muduo::net;

namespace
{

    struct SceneRuntimeContext
    {
        TimerTaskComp worldTimer;
        DependencyGate dependencyGate;
        SceneNodeGrpcImpl grpcService;
        std::unordered_set<Guid> shutdownPlayerIds;
        std::size_t lastPendingPlayerSaves = std::numeric_limits<std::size_t>::max();
        std::size_t lastPendingKafkaMessages = std::numeric_limits<std::size_t>::max();
        std::size_t lastRemainingPlayers = std::numeric_limits<std::size_t>::max();
        bool shutdownDrainLogged = false;

        explicit SceneRuntimeContext(EventLoop& loop) : grpcService(loop) {}
    };

    struct SceneNodeHooks
    {
        struct TableLoadHandler
        {
            static void OnLoaded()
            {
                ConfigSystem::OnConfigLoadSuccessful();
                // 战斗配表指纹:表加载完成后算一次并缓存(PrepareBattle 每次备战直接读缓存),
                // 也让指纹出现在启动日志里便于跨 zone 比对排障
                LOG_INFO << "scene 战斗配表指纹: table_fingerprint="
                         << turnbattle::BattleTableFingerprint::Refresh();
            }
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
		{
        // DataServiceNodeService:永久 guid 号段经它的 AllocateIdSegment 领取(全局池 gRPC 节点,
        // 在 DataServiceNodeService.rpc 前缀下发现)。不在白名单里发现了也不会建连。
        Node node(&loop, SceneNodeService,
                  Node::CanConnectNodeTypeList{ SceneManagerNodeService, DataServiceNodeService },
                  &handler);

        node::entry::detail::ApplyPostConstructionHooks<SceneNodeHooks>(node);

        node.RegisterGrpcService(&context->grpcService);

        node::entry::detail::InstallSignalHandlers(loop);

        tlsRedisSystem.Initialize(&loop);
        World::InitializeSystemBeforeConnect();

        // 永久 guid(item / tx_id / snapshot_id)全部走号段(docs/design/node-id-overhaul-plan-20260908.md
        // §6 / §7.5):一种 GUID 一个 GuidSegmentClient 实例,按 BaseDeployConfig.id_segment 启用,
        // 经 DataService.AllocateIdSegment 从全局库 id_segment 表领 [lo, hi)。scene 不再参与任何
        // snowflake 槽位协议 —— 号段对节点数不敏感,10 万台 scene 也不会压垮协调器。
        // 这里只配置传输、定时器和"DataService 连上即 Warm"的钩子,不发请求。
        const std::size_t idSegmentKinds = ConfigureGuidSegmentClients(node);

        // SIGTERM / GM 共用这一个唯一的普通停机玩家存盘入口;
        // 先复制实体列表,再逐个走完整 HandleExitGameNode 流程。
        auto exitAllPlayers = [&context](Node &)
        {
            // 停机后不再续段(定时器取消、不再发 gRPC);手里的号照常发完。
            // 放在最前面与 dependencyGate / worldTimer 一起收,晚了会在拆传输层时再发请求。
            tlsGuidSegmentRegistry.ShutdownAll();
            // 先停 Agones lifecycle worker 并 join,再动玩家数据。
            // 顺序不能反:worker 还在跑的时候进程正在退出,health / ready 请求
            // 会打到一个正在拆的对象上。
            //
            // 刻意**不**在这里调 POST /shutdown —— SIGTERM 通常正是 Agones
            // 删 Pod 发出来的,再回敬一个 /shutdown 就是递归触发删除。
            // 只有进程自己决定自我终止时才调 RequestShutdown()。
            agones::SceneLifecycle::Instance().Stop();
			context->dependencyGate.probeTimer.Cancel();
			context->worldTimer.Cancel();
			CrossZoneReaper::StopTick();
			PlayerBattleSystem::StopReaper();
			tlsRedisSystem.BeginShutdown();

            auto view = tlsEcs.actorRegistry.view<Player>();
			std::vector<entt::entity> players;
			players.reserve(view.size());
            for (auto entity : view)
            {
				players.push_back(entity);
				if (const auto *playerId = tlsEcs.actorRegistry.try_get<Guid>(entity))
				{
					context->shutdownPlayerIds.insert(*playerId);
				}
            }

			LOG_INFO << "Shutdown save: initiating exit for " << players.size() << " online players.";
			for (const auto entity : players)
			{
				if (tlsEcs.actorRegistry.valid(entity))
				{
					PlayerLifecycleSystem::HandleExitGameNode(entity);
				}
			}
			LOG_INFO << "Shutdown save initiated; waiting for Redis ACK and Kafka producer flush.";
        };
        node.SetBeforeShutdown(exitAllPlayers);

		// 每次轮询都重新扫描在线实体。gRPC / Kafka 入口已经由 Node 封住,但停机
		// 瞬间已经排入 EventLoop 的请求仍可能晚一拍创建玩家;不能只信首次快照。
		node.SetShutdownDrainComplete([&context](Node &n)
									  {
			auto view = tlsEcs.actorRegistry.view<Player>();
			std::vector<entt::entity> playersNeedingExit;
			for (auto entity : view)
			{
				const auto *playerId = tlsEcs.actorRegistry.try_get<Guid>(entity);
				if (playerId == nullptr)
				{
					continue;
				}
				context->shutdownPlayerIds.insert(*playerId);
				if (!PlayerLifecycleSystem::IsSaveInFlight(*playerId))
				{
					playersNeedingExit.push_back(entity);
				}
			}

			for (const auto entity : playersNeedingExit)
			{
				if (tlsEcs.actorRegistry.valid(entity))
				{
					PlayerLifecycleSystem::HandleExitGameNode(entity);
				}
			}

			std::size_t pendingPlayerSaves = 0;
			for (const Guid playerId : context->shutdownPlayerIds)
			{
				if (PlayerLifecycleSystem::IsSaveInFlight(playerId))
				{
					++pendingPlayerSaves;
				}
			}

			const std::size_t remainingPlayers = tlsEcs.actorRegistry.view<Player>().size();
			const bool kafkaDrained = n.GetKafkaManager().FlushProducer(std::chrono::milliseconds(0));
			const std::size_t pendingKafkaMessages = n.GetKafkaManager().PendingProducerMessages();
			if (pendingPlayerSaves != context->lastPendingPlayerSaves ||
				pendingKafkaMessages != context->lastPendingKafkaMessages ||
				remainingPlayers != context->lastRemainingPlayers)
			{
				LOG_INFO << "Shutdown drain progress: redis_player_saves=" << pendingPlayerSaves
						 << " kafka_messages=" << pendingKafkaMessages
						 << " remaining_players=" << remainingPlayers;
				context->lastPendingPlayerSaves = pendingPlayerSaves;
				context->lastPendingKafkaMessages = pendingKafkaMessages;
				context->lastRemainingPlayers = remainingPlayers;
			}

			const bool drained = remainingPlayers == 0 && pendingPlayerSaves == 0 && kafkaDrained;
			if (drained && !context->shutdownDrainLogged)
			{
				context->shutdownDrainLogged = true;
				LOG_INFO << "Shutdown persistence barrier complete: Redis ACKs observed and Kafka producer flushed.";
			}
			else if (!drained)
			{
				// gRPC 完全退场前 predicate 可能短暂为 true,后续在途任务又
				// 追加玩家。重置后只有最终稳定状态才会保留 complete 日志。
				context->shutdownDrainLogged = false;
			}
			return drained; });

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

        node.SetAfterStart([&context, idSegmentKinds](Node& n) {
            // 路由 node_id 到这里已是终值(StartRpcServer 打完 banner 才调 afterStartFn_),
            // buff / skill 的临时 id 现在才能种 node 段。以前在 InitializeSystemBeforeConnect
            // 里种,那时 node_id 恒 0。
            World::OnRoutingNodeIdAllocated(n.GetNodeId());

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

            // 回合制战斗冻结 reaper:30s 低频扫描 InBattleComp.deadline_ms,
            // 过期即作废解冻(battle 节点崩溃 / match 断链的补偿路径,
            // turn-based-battle-server.md §3.2)。不进 20FPS World::Update。
            PlayerBattleSystem::StartReaper(n.GetLoop());

            // 号段首段是启动硬依赖:每个启用的种类领到第一段 [lo, hi) 之前不放玩家进来 ——
            // 拾取 / 交易流水 / 快照都要铸永久 guid,没段时一律 fail-closed(kInvalidGuid),
            // 那不是"号段模式在跑"。运行期靠双 buffer 弱依赖,启动期必须硬等,这是设计取舍(§6.5)。
            // Warm 主要由 ConfigureGuidSegmentClients 挂的"DataService 连上即 Warm"钩子触发;这里再
            // 幂等地催一次,覆盖 DataService 早于本回调连上的情况。连不上时客户端自己按 500ms→5s 退避。
            if (idSegmentKinds > 0)
            {
                tlsGuidSegmentRegistry.WarmAll();
            }
            else
            {
                LOG_WARN << "[idsegment] no GUID kind enabled in BaseDeployConfig.id_segment: "
                         << "item / tx / snapshot minting will fail closed on this node";
            }
            context->dependencyGate.AddCondition("id segments ready", []
                                                 { return tlsGuidSegmentRegistry.AllEnabledReady(); });
            context->dependencyGate.WaitAndRun(n, { SceneManagerNodeService },
                [&context](auto&) {
                    context->worldTimer.RunEvery(tlsFrameTimeManager.frameTime.delta_time(), World::Update);
                }, "Scene");
        });

        loop.loop();
		} // 异常 quit 时先让 Node 析构兜底完成业务 drain,Redis 此时仍保持可用。

		// Node 正常 finalizer 已先 free Hiredis;这里是幂等兜底,随后才销毁
		// 仍由 SceneRuntimeContext 持有的 MessageAsyncClient。
		tlsRedis.Shutdown();
		tlsRedisSystem.Shutdown();

		// loop 退出后的兜底 join。SetBeforeShutdown 已经调过一次,Stop() 幂等;
		// 但走 conflict-shutdown 之类的分支时不保证走过那条路径。
        agones::SceneLifecycle::Instance().Stop(); });
}
