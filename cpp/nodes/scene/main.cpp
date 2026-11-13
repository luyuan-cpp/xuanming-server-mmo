#include "muduo/net/EventLoop.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "muduo/base/Logging.h"

#include "node/system/node/node_entry.h"
#include "node/system/etcd/etcd_transport.h"
#include "network/client_message_sender.h"
#include "infra/agones/agones_gameserver_lifecycle.h"
#include "infra/storage/redis_cluster/redis_cluster_client.h"
#include "handler/rpc/scene_handler.h"
#include "handler/grpc/scene_node_service.h"
#include "core/config/config.h"
#include "world/world.h"
#include "world/scene_ownership_reconciler.h"
#include "spatial/system/aoi_load.h"
#include "core/system/redis.h"
#include "core/system/scene_task_scope.h"
#include "frame/manager/frame_time.h"
#include "player/system/player_lifecycle.h"
#include "player/system/player_team_follow_scope.h"
#include "player/system/player_ownership_metrics.h"
#include "player/system/player_save_metrics.h"
#include "player/system/player_equip.h"
#include "player/system/player_inbox_system.h"
#include "player/system/player_mail_outbox.h"
#include "player/system/player_mystery_merchant.h"
#include "battle/system/player_battle.h"
#include "services/battle/data/battle_table_fingerprint.h"
#include "proto/contracts/kafka/scene_command.pb.h"
#include "handler/event/scene_kafka_command_router.h"
#include "id_segment_bootstrap.h"
#include "scene_planned_drain.h"
#include "world/scene_planned_drain_rules.h"
#include "modules/id_segment/guid_segment_registry.h"

#include <chrono>
#include <limits>
#include <unordered_set>
#include <vector>

using namespace muduo;
using namespace muduo::net;

namespace
{
    // Agones 排空标签键,与 battle 同一个(main.cpp kAgonesDrainLabelKey、k8s 部署脚本)。
    constexpr char kAgonesDrainLabelKey[] = "mmorpg.io/drain";

    struct SceneRuntimeContext
    {
        TimerTaskComp worldTimer;
        // 玩家持久收件箱的 1 秒 tick(PlayerInboxSystem::Tick)。刻意不挂在 DependencyGate 之后:
        // 收件箱不可用不得阻断进场(IB-7),它自己按退避处理"没有 READY 节点"。
        TimerTaskComp playerInboxTimer;
        // 待寄邮件的 1 秒 tick(PlayerMailOutboxSystem::Tick,docs/design/battle-overflow-mail.md)。同样不挂在
        // DependencyGate 之后:邮件不可用只让条目按退避留着。与收件箱 tick 同一组停机路径取消。
        TimerTaskComp mailOutboxTimer;
        DependencyGate dependencyGate;
        SceneNodeGrpcImpl grpcService;
        PlayerTeamFollowScope teamFollow;
        SceneTaskScope tasks;
        // 场景归属对账(缺口清单第 33 条):在 SetAfterStart 里 Start,三条停机路径与 Node 析构之后各 Stop 一次。
        SceneOwnershipReconciler ownershipReconciler;
        std::unordered_set<Guid> shutdownPlayerIds;
        std::size_t lastPendingPlayerSaves = std::numeric_limits<std::size_t>::max();
        std::size_t lastPendingKafkaMessages = std::numeric_limits<std::size_t>::max();
        std::size_t lastRemainingPlayers = std::numeric_limits<std::size_t>::max();
        std::size_t lastExitReleaseMarks = std::numeric_limits<std::size_t>::max();
        std::size_t lastRelocateConfirms = std::numeric_limits<std::size_t>::max();
        bool shutdownDrainLogged = false;
        // 本次停机里"drain 只剩改派待确认表"的逐条清单已经打过(每次停机只打一次)。
        bool relocateBacklogLogged = false;
        // 计划排空编排(scene_planned_drain.h)。定时器回调捕获 Node,三条停机路径与拆除钩子都会先 Stop()。
        ScenePlannedDrain plannedDrain;

        explicit SceneRuntimeContext(EventLoop& loop) : grpcService(loop), teamFollow(loop), tasks(loop) {}
        // 启动途中抛异常也必须在局部 EventLoop 销毁前收掉 TLS 中的任务所有者。
        ~SceneRuntimeContext() { StopGuidSegmentTransport(); }
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
                LOG_INFO << "scene combat table fingerprint: table_fingerprint="
                         << turnbattle::BattleTableFingerprint::Refresh();
                // 装备掷属性的挂点 + 装备表校验(docs/design/equipment-attributes.md §4.3)。
                // 放在配表加载完成回调里:此时表已就绪,而 Node 还在构造、没有监听任何端口,
                // 一定早于第一笔入包流量(玩家加载 / 掉落 / 邮件 / GM 发物)。漏装的后果是新装备
                // 永远没有随机属性,且没有任何报错。
                PlayerEquipSystem::InstallItemInitializer();
                // 神秘商人用到的三张表(规则 / 商品 / 站位)核一遍,问题打进启动日志;不阻断启动。
                PlayerMysteryMerchantSystem::ValidateTables();
            }
        };
        using KafkaCommandType = contracts::kafka::SceneCommand;
        static void DispatchKafkaCommand(const std::string& topic, const KafkaCommandType& command)
        {
            DispatchSceneKafkaCommand(topic, command);
        }
    };

} // namespace

int main(int argc, char *argv[])
{
    // 检查旧的 NODE_ETCD_TRANSPORT 环境变量(etcd 只剩 muduo_h2 传输;设成 grpc 会打 WARN),须在 Node 构造前调用。
    etcd_transport::InstallFromEnv();
    return node::entry::RunNodeMain([](EventLoop &loop)
                                    {
        node::entry::detail::ApplyPreConstructionHooks<SceneNodeHooks>();

        auto context = std::make_unique<SceneRuntimeContext>(loop);

        SceneHandler handler;
		{
        // DataServiceNodeService:永久 guid 号段经它的 AllocateIdSegment 领取(全局池 gRPC 节点,
        // 在 DataServiceNodeService.rpc 前缀下发现)。不在白名单里发现了也不会建连。
        // PlayerInboxNodeService:玩家持久收件箱的 FetchItems / AckItems(全局 gRPC 服务,
        // 在 PlayerInboxNodeService.rpc 前缀下发现;docs/design/player-inbox-service.md §1.4)。
        // PlayerStoreNodeService:玩家存盘的 LoadAndClaim / SavePlayer(go/db,按 zone 发现;
        // docs/design/player-save-direct-commit.md §5.2)。
        // TeamNodeService:队伍成员权威读取由 Match 同端口提供，只在需要刷新队伍时使用。
        // MatchNodeService:FIGHTING 冻结到期时只读查这一局的 SQL 终局(MatchInternal.GetBattleTerminalDecision,
        // 全局池,不限当前 zone;docs/design/turn-battle-gap-closure.md §11)。只在 reaper 判出冻结到期时调用。
        // MailNodeService:两包都放不下的战斗掉落改投邮件,以 battle 身份调 MailAdmin.SendPersonalMail
        // (全服一份、多副本,不限当前 zone;docs/design/battle-overflow-mail.md)。弱依赖:不进 DependencyGate,
        // 发现不到只让待寄条目按退避留着,不阻断进场。
        // 发现集合(docs/design/connection-fanout-subsetting-20261106.md §3.4)= 建连白名单 + Gate + 自身 Scene。
        // Gate:gate 主动连进来,入站登记(registration_manager.cpp)查快照里的 gate 登记,广播用 ResolveLocalZoneGateEntity;
        // Scene:本 zone 的同类,注册自检、劫持检测与选号要看。scene 不读 Battle、ClientRpcRouter、Login、Friend、Guild,不再看它们。
        Node node(&loop, SceneNodeService,
                  Node::CanConnectNodeTypeList{ SceneManagerNodeService, DataServiceNodeService,
                                                PlayerInboxNodeService, PlayerStoreNodeService, TeamNodeService,
                                                MatchNodeService, MailNodeService },
                  Node::DiscoveryNodeTypeList{ SceneManagerNodeService, DataServiceNodeService,
                                               PlayerInboxNodeService, PlayerStoreNodeService, TeamNodeService,
                                               MatchNodeService, MailNodeService, GateNodeService, SceneNodeService },
                  &handler);

        node::entry::detail::ApplyPostConstructionHooks<SceneNodeHooks>(node);

        // 断线释放标记(A1′)的"本节点身份当前有效"探针(cross-zone-scene-travel.md §12.6.4 M3):
        // EtcdService::IsIdentityConfirmedFresh —— lease 已授予、不在重注册中(分配键 CAS 尚未裁定)、
        // 注册流未停、最近一次 keepalive ACK 在 TTL/2 内。scene 库不直接依赖 Node,由这里注入;
        // 捕获的 node 活到本作用域结束(含析构里的兜底 drain 循环),作用域之后立即清掉。
        // 本地 ACK 在 TTL/2 内时 etcd 侧租约至少还剩 TTL/2,正常不可能已被别的进程接手;残余(§12.6.8)只剩
        // 租约被 etcd 提前撤销(人工 revoke / etcd 数据恢复)、本地尚未收到 TTL<=0 的窗口。同一窗口里旧进程
        // 迟到的同代存盘覆盖不了新进程:同号新进程按同一 E 登记交接资格会被拒(kRegisterFenceLua,同 E 不同实例),
        // scene_manager 判定持有实例已被顶替后先铸新 epoch 再派(GO-3,cross-zone-scene-travel.md §13.10),
        // 旧进程之后的存盘被 owner CAS 拒绝;A1′ 迟到的标记同样过不了资格校验。
        PlayerLifecycleSystem::SetNodeIdentityProbe([&node]()
            { return node.GetServiceDiscoveryManager().etcdService.IsIdentityConfirmedFresh(); });

        // 归属交接 / owner_epoch 计数挂到 Prometheus 出口(设了 MMORPG_METRICS_PORT 才有出口,见 metrics_http_exporter.h)。
        RegisterPlayerOwnershipMetrics();
        // 存盘分段耗时与存盘尝试计数(player_save_metrics.h),同一个出口。
        RegisterPlayerSaveMetrics();
        // 场景归属对账的计数(scene_ownership_reconciler.h),同一个出口。
        RegisterSceneOwnershipMetrics();
        // 每次广播发给了几台 gate 的直方图(network/broadcast_fanout_stats.h),同一个出口。
        RegisterBroadcastFanoutMetrics();
        // 服务器压力监控的读数:模拟步利用率、AOI 压力、因容量收缩裁掉的关注条目(spatial/system/aoi_load.h)。
        RegisterAoiLoadMetrics();

        // context 先于 node 构造、晚于 node 析构:方法表闭包捕获 grpcService 的 this,服务必须活过 Node。
        node.RegisterGrpcService(context->grpcService);

        node::entry::detail::InstallSignalHandlers(loop);

        // 玩家持久收件箱:注册表自检并打印生效配置(GameConfig 已在 Node 构造时加载)。
        PlayerInboxSystem::Initialize();

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
            // 计划排空(如果走过)到此结束:剩下的人按停机原因存盘下线。先停它的定时器,它的回调捕获着 Node。
            context->plannedDrain.Stop();
            context->tasks.Stop();
            context->grpcService.CloseCreationAdmission();
            // 停机后不再续段(定时器取消、不再发 gRPC);手里的号照常发完。
            // 放在最前面与 dependencyGate / worldTimer 一起收,晚了会在拆传输层时再发请求。
            CloseGuidSegmentTransport();
            tlsGuidSegmentRegistry.ShutdownAll();
            // 先停 Agones lifecycle worker 并 join,再动玩家数据。
            // 顺序不能反:worker 还在跑的时候进程正在退出,health / ready 请求
            // 会打到一个正在拆的对象上。
            //
            // SIGTERM 触发的停机刻意**不**调 POST /shutdown —— SIGTERM 通常正是 Agones
            // 删 Pod 发出来的,再回敬一个 /shutdown 就是递归触发删除。
            // 只有进程自己决定自我终止时才调 RequestShutdown():Agones 排空标签触发的计划排空就是
            // (docs/design/scene-node-planned-drain.md §4.1),请 Agones 删掉本 GameServer、由 Fleet 按当前模板补一台。
            // Stop() 之前收下的 /shutdown 一定发出(GameServerLifecycle::RequestShutdown 的注释)。
            if (context->plannedDrain.TriggeredByAgonesLabel())
            {
                agones::GameServerLifecycle::Instance().RequestShutdown();
            }
            agones::GameServerLifecycle::Instance().Stop();
			context->dependencyGate.probeTimer.Cancel();
			context->worldTimer.Cancel();
			// 收件箱 tick 一并停:停机期间不再拉取 / 确认。已应用、未确认的结局随退出存盘落地,
			// 下次落地时由账本补确认(设计 §7.7)。
			// P1 保留持久收件箱 relay；普通停机与 P3 才取消此计时器。
			PlayerBattleSystem::StopReaper();
			PlayerMysteryMerchantSystem::Stop();
			// 停机期间不再按归属排空:玩家正按停机原因存盘退出。也必须早于 Node 析构里的兜底 drain 循环,
			// 那之后探针捕获的 node 不再可用。
			context->ownershipReconciler.Stop();
			tlsRedisSystem.BeginShutdown();

            auto view = tlsEcs.actorRegistry.view<Player>();
			std::vector<entt::entity> players;
			players.reserve(view.size());
            for (auto entity : view)
            {
				players.push_back(entity);
				if (const auto *playerId = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(entity))
				{
					context->shutdownPlayerIds.insert(playerId->value);
				}
            }

			LOG_INFO << "Shutdown save: initiating exit for " << players.size() << " online players.";
			for (const auto entity : players)
			{
				if (tlsEcs.actorRegistry.valid(entity))
				{
					PlayerLifecycleSystem::HandleExitGameNode(entity, ExitCause::kNodeShutdown);
				}
			}
			LOG_INFO << "Shutdown save initiated; waiting for Redis ACK and Kafka producer flush.";
        };
        // 拆除前收尾:业务排空之后、释放 Redis 与传输之前执行,日志 / ECS 都还活着,把没落定的存盘留一行证据(带玩家号)。
        // 两条停机路径各挂一次:普通停机由 SetBeforeTeardown 调;维护路径(P3)关闭中继时 Node 只调 SetMaintenanceRelayStop
        // 的钩子,随即置 beforeTeardownRan_、释放本地所有者,不再调 SetBeforeTeardown 的钩子。两处必须是同一段收尾,
        // 否则维护路径释放 Hiredis 时持久任务还在等回包,号段传输和归属对账也没停(2026-11-11 订正,此前 P3 只停两个定时器)。
        const auto beforeTeardown = [&context](Node &)
        {
            context->plannedDrain.Stop();
            context->tasks.StopAll();
            context->grpcService.CloseCreationAdmission();
            StopGuidSegmentTransport();
            context->playerInboxTimer.Cancel();
            context->mailOutboxTimer.Cancel();
            context->ownershipReconciler.Stop();
            PlayerLifecycleSystem::LogUnsettledPlayerStoreSaves();
        };
        node.SetMaintenanceInboundRelay(); // gate/Go已接收链的下游，保留到P3；根是本机世界/玩家输入。
        node.SetMaintenanceSourceClose(exitAllPlayers);
        node.SetMaintenanceRelayStop(beforeTeardown);
        node.SetBeforeShutdown([&context, exitAllPlayers](Node &n) {
            context->playerInboxTimer.Cancel();
            context->mailOutboxTimer.Cancel();
            exitAllPlayers(n);
        });

		// 每次轮询都重新扫描在线实体。P1保留Kafka/RPC下游推进，P3才停止并join；gRPC在OnGrpcServerDrained前仍在排空,
		// 已收完的调用照常派发,停机瞬间已经排入 EventLoop 的请求也会执行,都可能晚一拍创建玩家;不能只信首次快照。
		node.SetShutdownDrainComplete([&context](Node &n)
									  {
			auto view = tlsEcs.actorRegistry.view<Player>();
			std::vector<entt::entity> playersNeedingExit;
			for (auto entity : view)
			{
				const auto *playerId = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(entity);
				if (playerId == nullptr)
				{
					continue;
				}
				context->shutdownPlayerIds.insert(playerId->value);
				if (!PlayerLifecycleSystem::IsPlayerExitPending(playerId->value))
				{
					playersNeedingExit.push_back(entity);
				}
			}

			for (const auto entity : playersNeedingExit)
			{
				if (tlsEcs.actorRegistry.valid(entity))
				{
					PlayerLifecycleSystem::HandleExitGameNode(entity, ExitCause::kNodeShutdown);
				}
			}

			std::size_t pendingPlayerSaves = 0;
			for (const Guid playerId : context->shutdownPlayerIds)
			{
				if (PlayerLifecycleSystem::IsPlayerExitPending(playerId))
				{
					++pendingPlayerSaves;
				}
			}

			const std::size_t remainingPlayers = tlsEcs.actorRegistry.view<Player>().size();
			const bool kafkaDrained = n.GetKafkaManager().FlushProducer(std::chrono::milliseconds(0));
			const std::size_t pendingKafkaMessages = n.GetKafkaManager().PendingProducerMessages();
			// 断线释放标记(A1′)的条件写在途数(R6):实体销毁之后才发、回调才归零,不等它的话 loop 一退
			// 回调就再也不会来。受 Node drain 看门狗约束(Redis 或 go/db 卡住时不会无限等)。
			const std::size_t exitReleaseMarks = PlayerLifecycleSystem::ExitReleaseMarksInFlight();
			// 疏散 / 排空改派的待确认表(relocate_confirm.h)里还没有结论的改派数。停机前的单场景排空会留下条目:
			// scene_manager 的拒绝应答、核实读的应答都还在路上,不等它们的话 loop 一退,被拒的玩家就挂在没有实体的
			// 会话上。等待同样受 Node 的 drain 看门狗(15s)约束;确认阶段只读 Redis、只推 gate,唯一的写(踢线前的
			// 凭证补写)要过身份闸、且已计入上面的 exitReleaseMarks。看门狗到期时还剩的条目只放弃、不踢。
			// 身份冲突疏散也会走到这里:冲突 drain 的看门狗到期后 Node 调 Shutdown,被它放下的条目在本谓词下再等
			// 一道 15s(1s 扫描仍在跑,settle 落在这个窗口里的条目会被读到并踢线),所以失去身份的节点最长约 30s 才退出。
			const std::size_t relocateConfirms = PlayerLifecycleSystem::RelocateConfirmsPending();
			if (pendingPlayerSaves != context->lastPendingPlayerSaves ||
				pendingKafkaMessages != context->lastPendingKafkaMessages ||
				remainingPlayers != context->lastRemainingPlayers ||
				exitReleaseMarks != context->lastExitReleaseMarks ||
				relocateConfirms != context->lastRelocateConfirms)
			{
				LOG_INFO << "Shutdown drain progress: redis_player_saves=" << pendingPlayerSaves
						 << " kafka_messages=" << pendingKafkaMessages
						 << " remaining_players=" << remainingPlayers
						 << " exit_release_marks=" << exitReleaseMarks
						 << " relocate_confirms=" << relocateConfirms;
				context->lastPendingPlayerSaves = pendingPlayerSaves;
				context->lastPendingKafkaMessages = pendingKafkaMessages;
				context->lastRemainingPlayers = remainingPlayers;
				context->lastExitReleaseMarks = exitReleaseMarks;
				context->lastRelocateConfirms = relocateConfirms;
			}

			const bool drained = remainingPlayers == 0 && pendingPlayerSaves == 0 && exitReleaseMarks == 0 &&
								 relocateConfirms == 0 && kafkaDrained;
			// drain 第一次只剩待确认表时把条目逐条打出来(一条 WARN 汇总 + 每条一行 INFO),每次停机只打一次:
			// 看门狗到期后这些条目被放弃,事后要查得出是哪些玩家的会话可能还挂着。本谓词被周期轮询,所以用标志挡住重复。
			if (!drained && relocateConfirms > 0 && remainingPlayers == 0 && pendingPlayerSaves == 0 &&
				exitReleaseMarks == 0 && kafkaDrained && !context->relocateBacklogLogged)
			{
				context->relocateBacklogLogged = true;
				PlayerLifecycleSystem::LogRelocateConfirmBacklog("shutdown_drain");
			}
			if (drained && !context->shutdownDrainLogged)
			{
				context->shutdownDrainLogged = true;
				LOG_INFO << "Shutdown persistence barrier complete: Redis ACKs observed and Kafka producer flushed.";
			}
			else if (!drained)
			{
				// gRPC 排空完成(OnGrpcServerDrained)之前 predicate 可能短暂为 true,之后派发的
				// 调用又追加玩家。重置后只有最终稳定状态才会保留 complete 日志。
				context->shutdownDrainLogged = false;
			}
			return drained; });

        // 普通停机的拆除前钩子,与维护路径的中继关闭共用 beforeTeardown(见其定义处)。
        node.SetBeforeTeardown(beforeTeardown);

        // 身份冲突(etcd 租约过期 / node_id 被别人抢走)和普通 SIGTERM 不是一回事:
        // 这台节点马上就不再是 node_id 的合法持有者,但玩家的 gate 会话还连着。
        // 所以除了存盘,还要把玩家改派到别的 scene node 的大世界频道 ——
        // 否则他们的会话指着一具尸体,只能等自己发现掉线再重登。
        //
        // 顺序由 PlayerLifecycleSystem 保证:抄会话 -> 存盘 -> 存盘落地后才发改派请求。
        // 反过来做的话,新节点认领时会从 TiDB 读到存盘前的旧数据(回档)。
        node.SetOnConflictShutdown([&context](Node &, NodeIdConflictReason)
                                   {
            // 计划排空中撞上身份冲突:整节点疏散接管(它不限速),排空的定时器不再推进。
            context->plannedDrain.Stop();
            context->tasks.Stop();
            context->grpcService.CloseCreationAdmission();
            // 整节点疏散接管全部玩家,不再按单个场景的归属排空。
            context->ownershipReconciler.Stop();
            // 先停 Agones lifecycle worker(理由同 exitAllPlayers 里的注释),
            // 再动玩家数据。Stop() 幂等。
            agones::GameServerLifecycle::Instance().Stop();
            PlayerLifecycleSystem::BeginEmergencyRelocateAll(); });

        // 有界 drain:存盘全部落地、改派全部派发完才退出。
        // 到期兜底(go/db / SceneManager 卡住时)由 Node 的看门狗负责,不会无限等。
        node.SetConflictDrainComplete([](Node &)
                                      { return PlayerLifecycleSystem::IsEmergencyRelocateDrained(); });

        // 计划排空(docs/design/scene-node-planned-drain.md):SIGTERM(K8s 滚动、缩容、删 Pod)与 Agones 排空标签先把玩家
        // 疏散到别的节点 —— 关建场景准入、发布 admission = CLOSED,scene_manager 把本节点的世界频道迁走后发 DestroyScene,
        // 住户按限速存盘、带着原频道号改派;副本与没赶上迁移的到自疏散时刻由本节点自己疏散。完成或到预算才进普通停机,
        // 剩下的人由 exitAllPlayers 存盘下线。预算由部署按 Pod 宽限期注入(MMORPG_PLANNED_DRAIN_BUDGET_SECONDS)。
        node.SetPlannedDrain(
            [&context](Node &n)
            { context->plannedDrain.Begin(n, [&context]
                                          { context->grpcService.CloseCreationAdmission(); }); },
            [&context](Node &n)
            { return context->plannedDrain.IsComplete(n); },
            scene_planned_drain::kDefaultBudget);

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
            //
            // scene 不开 EventLoop 心跳绑定(D84);开排空标签(D83,与 battle 同键 mmorpg.io/drain):标签出现时 lifecycle
            // 拒绝新的分配许可,ScenePlannedDrain 每秒看一眼它的判定,看到就进计划排空(docs/design/scene-node-planned-drain.md
            // §4.1)。标签由运维 / 部署脚本给旧构建的 Allocated GameServer 打,或由 Fleet 缩容时的 allocationOverflow 打。
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
                    agones::GameServerLifecycle::Instance().StartDisabled();
                }
                else
                {
                    agones::LifecycleOptions options;
                    options.drainLabelKey = kAgonesDrainLabelKey;
                    agones::GameServerLifecycle::Instance().Start(
                        std::move(transport), agonesEnv.BaseUrl(), std::move(options));
                    context->plannedDrain.WatchAgonesDrainLabel(n);
                }
            }

            // 这里原先订阅 player_migrate / player_migrate_ack 两个 Kafka topic 并启动 CrossZoneReaper,
            // 属于"跨 zone 搬数据"那条链(Kafka 搬 PlayerAllData + ACK + reaper 重发)。该链已按
            // docs/design/cross-zone-scene-travel.md CZ-1 下线:全仓从无触发点,且与"数据不搬家、
            // 按 home_zone 落库"的决策冲突。跨 zone 现在走「重定向 + 目标 zone 直接从盘上加载」,
            // 源端释放链在 PlayerLifecycleSystem(StartTravelHandoff …),冻结的收口由它自己的存盘 / 应答
            // 看门狗(各 30s)与 70s 冻结硬上限(travel_freeze_cap.h,EnforceTravelFreezeCaps)负责,不需要 reaper。
            // broker 上残留的 consumer group `scene-cross-zone-{nodeId}`、两个 topic,以及 Redis 里的
            // player_migration:* 记录(TTL 120s)都无害,不为它们写清理代码。

            // 回合制战斗冻结 reaper:30s 低频扫描 InBattleComp.deadline_ms,
            // 过期即作废解冻(battle 节点崩溃 / match 断链的补偿路径,
            // turn-based-battle-server.md §3.2)。不进 20FPS World::Update。
            PlayerBattleSystem::StartReaper(n.GetLoop());

            // 玩家持久收件箱的 tick(docs/design/player-inbox-service.md §7.4):每秒处理到期的检查、在线兜底、
            // 等落盘与补确认。不进 DependencyGate(IB-7):收件箱没连上时 tick 只会按退避记一次延后。
            context->playerInboxTimer.RunEvery(PlayerInboxSystem::kTickIntervalSec, PlayerInboxSystem::Tick);
            // 待寄邮件的 tick(docs/design/battle-overflow-mail.md):每秒把已确认落盘的待寄条目交给邮件服务,
            // 每名玩家至多一封在途。只寄已提交的存档快照里已有的条目,所以不必等别的启动依赖。
            context->mailOutboxTimer.RunEvery(PlayerMailOutboxSystem::kTickIntervalSec, PlayerMailOutboxSystem::Tick);
            // 没配发件密钥(没有部署 mail 的调试栈)时不接改投:两包都放不下的结算照旧整笔延后。在这里说一次,
            // 之后每一次不接只打 DEBUG。K8s 上密钥是必需的 secretKeyRef,缺了 Pod 起不来,走不到这一行。
            if (PlayerMailOutboxSystem::IsSenderConfigured())
            {
                LOG_INFO << "[MailOutbox] Battle overflow mail is enabled: drops that fit neither bag are mailed to the player";
            }
            else
            {
                LOG_WARN << "[MailOutbox] Battle overflow mail is disabled: MAIL_PRODUCER_SECRET_BATTLE is not set (or shorter"
                            " than 32 bytes); settlements whose drops fit neither bag are deferred until the player frees space";
            }
            // 神秘商人:每 5 秒从 zone Redis 读一次「这一轮商人在哪、卖什么」(scene_manager 写入),
            // 读到新的一轮就向本节点玩家发出现公告(docs/design/mystery-merchant.md §5.1)。不进 20FPS World::Update。
            PlayerMysteryMerchantSystem::Start(n.GetLoop());
            // 场景归属对账(缺口清单第 33 条,docs/design/scene-node-ownership-reconcile.md):每 10s 经场景控制域读本地
            // 各场景的 scene:{id}:node 与本节点号的持有者,不归本进程的实体按住户预算排空后销毁 —— 与 etcd 单边失联、被判死之后,
            // scene_manager 已把它们改派、拆掉,或交给了接走这个号的新进程。核对不看 etcd 身份(失联期间照样收敛);身份探针
            // 与 A1′ 同一个函数,只用来解读持有者记录。捕获的 node 由三条停机路径的 Stop 在它析构之前解开。node_id 与
            // node_uuid 到这里已是终值。不进 DependencyGate:它只读控制域 Redis,不依赖 SceneManager 连接。
            context->ownershipReconciler.Start([&n]()
                { return n.GetServiceDiscoveryManager().etcdService.IsIdentityConfirmedFresh(); });

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

		// Node 已析构:身份探针捕获的引用从此悬空,立即清掉(之后不再跑 loop,A1′ 也不会再被调用)。
		PlayerLifecycleSystem::SetNodeIdentityProbe({});
		// 同理丢掉对账器的探针(正常路径已在停机钩子里 Stop 过,这里是幂等兜底)。
		context->ownershipReconciler.Stop();

		// Node 正常 finalizer 已先 free Hiredis;这里是幂等兜底。
		context->tasks.StopAll();
		context->grpcService.CloseCreationAdmission();
		StopGuidSegmentTransport();
		tlsRedis.Shutdown();
		tlsSceneControlRedis.Shutdown();
		tlsPlayerRedis.Shutdown();
		tlsRedisSystem.Shutdown();

		// loop 退出后的兜底 join。SetBeforeShutdown 已经调过一次,Stop() 幂等;
		// 但走 conflict-shutdown 之类的分支时不保证走过那条路径。
        agones::GameServerLifecycle::Instance().Stop(); });
}
