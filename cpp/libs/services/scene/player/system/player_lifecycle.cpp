#include "player_lifecycle.h"

#include <cstdlib>

#include "proto/common/event/actor_event.pb.h"
#include "proto/common/component/player_async_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/component/player_login_comp.pb.h"
#include "proto/common/component/player_network_comp.pb.h"
#include "proto/common/event/player_event.pb.h"

#include "thread_context/redis_manager.h"
#include "time/system/time.h"
#include "type_alias/player_session_type_alias.h"
#include "core/utils/defer/defer.h"
#include "core/utils/proto/proto_dirty_compare.h"
#include "network/node_utils.h"
#include "battle/system/player_battle.h"
#include "proto/common/component/battle_comp.pb.h"
#include "proto/common/component/team_comp.pb.h"
#include "player/comp/last_persisted_snapshot_comp.h"
#include "player/comp/player_frozen_comp.h"
#include "player/comp/player_ownership_comp.h"
#include "player/system/dirty_save_stats.h"
#include "player/system/player_data_loader.h"
#include "engine/core/type_define/type_define.h"
#include "table/proto/tip/cross_server_error_tip.pb.h"
#include "player_tip.h"
#include "modules/scene/comp/scene_comp.h"
#include "modules/scene/comp/scene_node_comp.h"
#include <engine/infra/messaging/kafka/kafka_producer.h>
#include "core/system/redis.h"
#include "player_scene.h"
#include "player_team.h"
#include "hexagons_grid.h"  // Hex —— 退出场景时要和 SceneEntityComp 成对摘掉
#include "stress_test_probe.h"
#include "player/constants/player.h"
#include "proto/db/db_task.pb.h"
#include "modules/snapshot/snapshot_system.h"
#include "modules/transaction_log/anomaly_detector.h"
#include <proto/scene/scene_info.pb.h>
#include "player/comp/afk_comp.h"
#include "frame/manager/frame_time.h"
#include "proto/common/event/scene_event.pb.h"
#include "network/network_utils.h"
#include "network/player_message_utils.h"
#include "network/rpc_session.h"
#include "thread_context/node_context_manager.h"
#include "rpc/service_metadata/client_player_common_service_metadata.h"
#include "table/proto/tip/scene_error_tip.pb.h"
#include "proto/scene_manager/scene_manager_service.pb.h"
#include "grpc_client/scene_manager/scene_manager_service_grpc_client.h"
#include "muduo/net/EventLoop.h"
#include <algorithm>
#include <vector>

thread_local PendingEnterMap tlsPendingEnterMap;

PendingEnterMap& PlayerLifecycleSystem::GetPendingEnterMap()
{
	return tlsPendingEnterMap;
}

// 紧急疏散票据:实体销毁后就再也拿不到 gate / session 了,所以必须在存盘前抄一份。
struct EmergencyRelocateTicket
{
	Guid playerId{kInvalidGuid};
	SessionId sessionId{kInvalidSessionId};
	NodeId gateNodeId{0};
	std::string gateInstanceId;
};

thread_local bool tlsEmergencyRelocating = false;
thread_local std::unordered_map<Guid, EmergencyRelocateTicket> tlsEmergencyRelocateTickets;
// 票据已消费、handoff 标记正在写、EnterScene 还没发出去的改派数。票据在发起写标记时就删了,
// 不单独计这一段的话 IsEmergencyRelocateDrained 会在"改派请求其实还没发"时宣布收敛,
// 节点随即 quit loop,Redis 回调再也不会来,这批玩家的改派就丢了。
thread_local std::size_t tlsRelocateHandoffMarksInFlight = 0;

namespace
{
	// Sends a SendTipToClient message directly to the gate by session_id.
	// Used during the async-load window when the player entity does not yet
	// exist, so the entity-based PlayerTipSystem path is not available.
	//
	// playerId 是 gate 侧的身份栅栏(routing-identity-audit-20260908.md R13):
	// 这条路径没有玩家实体可取 Guid,但调用方两处都拿得到 player_id —— 必须传下来,
	// 否则本函数会成为整条推送链上唯一一个不带身份的口子。
	void SendTipToPendingSession(SessionId sessionId, Guid playerId, uint32_t tipId)
	{
		if (sessionId == 0)
		{
			return;
		}
		// 不能写 `entt::entity{GetGateNodeId(sessionId)}` —— node_id 是业务编号,
		// 而 gate 实体槽位是 registry.create() 按发现顺序分配的(node_connector.cpp:118 /
		// registration_manager.cpp),两者早在 uuid 主键重构后就不再相等。
		// network_utils.h:27-30 明文禁止这种写法。单 gate 部署时 node_id 从 1 起、
		// 实体槽位从 0 起,valid() 必假,这条提示 100% 发不出去;多 gate 时更糟,
		// 会命中另一个 gate 的槽位、把提示发给没有这个会话的节点。
		// 本文件 EnqueueRelocateTicket 与 SendMessageToClientViaGate 都已走
		// ResolveLocalZoneGateEntity,只有这个 namespace-local 函数漏改。
		const auto gateEntityOpt = ResolveLocalZoneGateEntity(sessionId);
		if (!gateEntityOpt)
		{
			LOG_WARN << "SendTipToPendingSession: gate not found for session " << sessionId;
			return;
		}
		auto &gateNodeRegistry = tlsNodeContextManager.GetRegistry(eNodeType::GateNodeService);
		auto *gateSessionPtr = gateNodeRegistry.try_get<RpcSession>(*gateEntityOpt);
		if (gateSessionPtr == nullptr)
		{
			LOG_WARN << "SendTipToPendingSession: RpcSession missing for session " << sessionId;
			return;
		}
		TipInfoMessage tip;
		tip.set_id(tipId);
		SendMessageToClientViaGate(SceneClientPlayerCommonSendTipToClientMessageId,
								   tip, *gateSessionPtr, sessionId, playerId);
	}

	// 会话所在 gate 的实例 uuid(EnterSceneRequest.gate_instance_id,scene_manager 用它做 Kafka
	// 目标校验)。找不到 gate 返回空串:与疏散票据的既有行为一致,由 scene_manager 侧决定怎么处理。
	std::string ResolveGateInstanceId(SessionId sessionId)
	{
		const auto gateEntityOpt = ResolveLocalZoneGateEntity(sessionId);
		if (!gateEntityOpt)
		{
			return {};
		}
		auto &gateRegistry = tlsNodeContextManager.GetRegistry(eNodeType::GateNodeService);
		const auto *gateNodeInfo = gateRegistry.try_get<NodeInfo>(*gateEntityOpt);
		return gateNodeInfo != nullptr ? gateNodeInfo->node_uuid() : std::string{};
	}

	// 传送交接的 EnterScene 应答预算。生成的 gRPC 客户端在 status 非 OK 时**不调**应答处理器,
	// 而 scene_manager 不可达 / 连接被重置就是这种情况 —— 没有这道看门狗,玩家会以冻结态
	// 永远挂在本节点。取值远大于 EnterScene 的正常耗时(压测 P99 亚秒),只兜真正的丢应答。
	constexpr double kTravelReplyBudgetSec = 30.0;

	// PlayerSceneChangeInFlightComp 的有效期。EnterScene 正常亚秒返回;应答丢失时(scene_manager
	// 不可达,生成的 gRPC 客户端不回调)不能把玩家永久挡在换图之外,过了这个时间就放下一条请求。
	// 代价:超过它才到的迟到应答可能被记到下一条请求头上(见 PlayerSceneChangeInFlightComp 的说明),
	// 后果只是一次换图失败或多余的交接,不会双主 —— 去留始终由 owner_epoch 比对裁决。
	constexpr uint64_t kSceneChangeInFlightTtlMs = 5000;
} // namespace

void PlayerLifecycleSystem::HandlePlayerAsyncLoadFailed(Guid playerId,
														MessageAsyncClient<Guid, PlayerAllData>::LoadFailureReason reason)
{
	using LoadFailureReason = MessageAsyncClient<Guid, PlayerAllData>::LoadFailureReason;

	// DataNotFound: NIL after exhausting retries. Treat as a brand-new player
	// whose DB rows do not exist yet (CreatePlayer only writes account meta;
	// PlayerAllData parent key is first written by Scene's own SavePlayerToRedis,
	// so a first-time login will always observe NIL here). Hand off to the
	// existing new-player branch in HandlePlayerAsyncLoaded by feeding it an
	// empty PlayerAllData -- it detects player_database_data().player_id()==0
	// and stamps the playerId from the async-load key.
	if (reason == LoadFailureReason::DataNotFound)
	{
		LOG_INFO << "HandlePlayerAsyncLoadFailed: no Redis data for player " << playerId
				 << " after retries, treating as brand-new player";
		PlayerAllData empty;
		HandlePlayerAsyncLoaded(playerId, empty);
		return;
	}

	// RedisError: connection lost, parse failure, or unexpected reply type.
	// We cannot proceed -- notify the client (if a session is still bound) so
	// it leaves the loading screen instead of hanging, then drop pending state.
	LOG_ERROR << "HandlePlayerAsyncLoadFailed: Redis load failed for player " << playerId;

	auto pendingIt = tlsPendingEnterMap.find(playerId);
	if (pendingIt != tlsPendingEnterMap.end())
	{
		const SessionId sessionId = pendingIt->second.enterInfo.session_id();
		if (sessionId != 0)
		{
			// Best-effort tip; safe even if the gate session is already gone.
			SendTipToPendingSession(sessionId, playerId, kEnterSceneFailed);
			SessionMap().erase(sessionId);
		}
		tlsPendingEnterMap.erase(pendingIt);
	}
}

void PlayerLifecycleSystem::HandlePlayerAsyncSaveFailed(Guid playerId, const std::string &redisKey, int retryCount)
{
	// 这条日志代表**真实的数据持久化风险**,不是可以忽略的抖动:
	// scene 的 PlayerAllData key 与 db 服务回写的分表 key 是两套命名空间
	// (见 docs/design/player-async-save-loss-windows.md §2),没有任何下游
	// 会把它修回来。玩家下次进场从这个 key 加载 = 回档到上一次成功存盘。
	LOG_ERROR << "HandlePlayerAsyncSaveFailed: DATA-DURABILITY RISK — player " << playerId
			  << " could not be persisted after " << retryCount << " retries, key=" << redisKey
			  << ". The latest payload remains queued with capped backoff; Redis still holds the previous save.";

	const auto playerEntity = tlsEcs.GetPlayer(playerId);
	if (!tlsEcs.actorRegistry.valid(playerEntity))
	{
		return;
	}

	// 刻意**不**碰 PlayerLastPersistedSnapshotComp:它的语义是"确实落过盘"。
	// 保持旧值 → 下一次 SavePlayerToRedis 的 proto-compare 必然判不等 → 会重写
	// 整份数据,这正是我们要的自愈。若在这里更新它,快路径会永久跳过存盘。

	// fail-closed:绝不在 Redis 尚未接收最新值时销毁唯一内存态。MessageAsyncClient
	// 会继续保留最新 payload 重试;若玩家正在退出,UnregisterPlayer 标记让
	// IsSaveInFlight 保持为 true,由节点已有的有界 drain 看门狗决定最终停机边界。
	// 这可能暂时保留实体,但不会把一次 Redis 抖动确定性地升级成玩家回档。
	if (tlsEcs.actorRegistry.any_of<UnregisterPlayer>(playerEntity))
	{
		LOG_ERROR << "HandlePlayerAsyncSaveFailed: retaining exiting player " << playerId
				  << " until the queued save succeeds; node drain remains bounded by its watchdog.";
	}
}

void PlayerLifecycleSystem::HandlePlayerAsyncLoaded(Guid playerId, const PlayerAllData &message)
{
	LOG_INFO << "HandlePlayerAsyncLoaded: Loading player " << playerId;

	// Consume the pending enter info. If absent, the load was orphaned.
	auto pendingIt = tlsPendingEnterMap.find(playerId);
	if (pendingIt == tlsPendingEnterMap.end())
	{
		LOG_WARN << "HandlePlayerAsyncLoaded: no pending enter info for player " << playerId << ", skipping";
		return;
	}
	PlayerEnterContext ctx = std::move(pendingIt->second);
	tlsPendingEnterMap.erase(pendingIt);

	// If the session was erased during async load (e.g. client disconnected
	// and ExitGame arrived before load completed), skip entity creation.
	if (ctx.enterInfo.session_id() != 0
		&& SessionMap().find(ctx.enterInfo.session_id()) == SessionMap().end())
	{
		LOG_INFO << "HandlePlayerAsyncLoaded: session " << ctx.enterInfo.session_id()
		         << " cancelled during async load for player " << playerId << ", skipping";
		return;
	}

	// If the loaded data has player_id=0, this is a brand-new player whose DB
	// rows don't exist yet (CreatePlayer only creates an account entry), or an
	// orphan after zone rollback.  In either case, set the player_id from the
	// async-load key and let InitPlayerFromAllData handle first-time registration
	// via the registration_timestamp check.
	const PlayerAllData *data = &message;
	PlayerAllData patchedMessage;
	if (message.player_database_data().player_id() == 0)
	{
		LOG_INFO << "HandlePlayerAsyncLoaded: No existing DB data for player " << playerId
				 << ", will initialize as new player";
		patchedMessage = message;
		patchedMessage.mutable_player_database_data()->set_player_id(playerId);
		patchedMessage.mutable_player_database_1_data()->set_player_id(playerId);
		data = &patchedMessage;
	}

	InitPlayerFromAllData(*data, ctx);
}

void PlayerLifecycleSystem::HandlePlayerAsyncSaved(Guid playerId, PlayerAllData &message)
{
	LOG_INFO << "HandlePlayerAsyncSaved: Saving complete for player: " << playerId;

	// TODO: When should session be deleted?

	auto playerEntity = tlsEcs.GetPlayer(playerId);

	// ── 归属交接(CZ-5,跨 zone 传送 / 同 zone 跨节点换图):必须排在所有既有分支之前 ──
	// 这次落地就是"源已落盘"那道门的凭证:从这里开始才允许写 handoff 标记、再请求
	// scene_manager 放行。顺序反了(先请求后落盘)目标 zone 会读到旧数据 —— 与紧急疏散
	// "先存盘后改派"是同一条纪律。
	//
	// 退出优先于传送:实体若同时带 UnregisterPlayer(客户端在传送发起后断线 / 主动退出),
	// 传送意图作废、走下面的正常退出收尾。否则这里起了交接,而退出那条链又永远等不到
	// 第二次回调,实体会以冻结态悬挂。
	bool travelHandoffPending = false;
	if (tlsEcs.actorRegistry.valid(playerEntity) &&
		tlsEcs.actorRegistry.any_of<PlayerTravelHandoffComp>(playerEntity))
	{
		if (tlsEcs.actorRegistry.any_of<UnregisterPlayer>(playerEntity))
		{
			LOG_WARN << "HandlePlayerAsyncSaved: player " << playerId
					 << " is exiting; dropping in-flight zone travel intent (exit wins)";
			tlsEcs.actorRegistry.remove<PlayerTravelHandoffComp>(playerEntity);
			tlsEcs.actorRegistry.remove<PlayerFrozenComp>(playerEntity);
		}
		else
		{
			travelHandoffPending = true;
		}
	}

	if (travelHandoffPending)
	{
		// 实体保留(冻结)直到 EnterScene 应答:放行 → 销毁;未成 → 解冻。
		// 快照仍在函数末尾更新,交接未成后快路径判定才正确。
		BeginTravelHandoff(playerId);
	}
	// 这里原先还有一条"只带 PlayerFrozenComp 就挂起、等 ACK 或 reaper 来解"的分支,属于已下线的
	// player_migrate 搬数据链(CZ-1)。现在 Frozen 只与 PlayerTravelHandoffComp 成对出现,上面已处理;
	// 留着那条分支,任何漏摘 Frozen 的实体退出时都会永久悬挂(ACK 与 reaper 都没了)。
	//
	// valid() 必须在前:对已销毁的实体调 any_of 是 entt 的未定义行为。原先靠被删分支的 valid()
	// 短路不到这一条,属于既有疏漏,顺手补上。
	else if (tlsEcs.actorRegistry.valid(playerEntity) &&
			 tlsEcs.actorRegistry.any_of<UnregisterPlayer>(playerEntity))
	{
		// Detect saves that outran the player_locator reconnect lease (30s).
		// See todo.md #280, layer 3. logout_initiated_ms is stamped in
		// HandleExitGameNode; default value 0 from older proto runs is treated
		// as "unknown" and silently skipped.
		const auto& unregisterTag = tlsEcs.actorRegistry.get<UnregisterPlayer>(playerEntity);
		const int64_t logoutMs = unregisterTag.logout_initiated_ms();
		if (logoutMs > 0)
		{
			constexpr int64_t kReconnectLeaseMs = 30 * 1000;
			const int64_t elapsedMs = TimeSystem::NowMillisecondsUTC() - logoutMs;
			if (elapsedMs > kReconnectLeaseMs)
			{
				LOG_WARN << "HandlePlayerAsyncSaved: save outran reconnect lease for player "
						 << playerId << " — elapsed_ms=" << elapsedMs
						 << " lease_ms=" << kReconnectLeaseMs
						 << " (cross-node re-login during this window may have read stale data)";
			}
		}

		// Defense in depth: if a reconnect rebound this entity to a live session
		// after the save was enqueued, the UnregisterPlayer tag should have been
		// cleared by EnterScene. If a tag still slipped through but the player
		// has an active session, do NOT destroy — drop the stale unregister intent.
		const auto *snapshot = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(playerEntity);
		if (snapshot != nullptr && snapshot->gate_session_id() != kInvalidSessionId)
		{
			const auto sessionIt = SessionMap().find(snapshot->gate_session_id());
			if (sessionIt != SessionMap().end() && sessionIt->second == playerId)
			{
				LOG_WARN << "HandlePlayerAsyncSaved: ignoring stale UnregisterPlayer for player "
						 << playerId << " — live session " << snapshot->gate_session_id()
						 << " indicates reconnect superseded the logout intent";
				tlsEcs.actorRegistry.remove<UnregisterPlayer>(playerEntity);
				return;
			}
		}

		LOG_INFO << "Player marked for unregistration: " << playerId;

		FinishExitAfterPersist(playerId);
	}

	// Update last-persisted snapshot (todo.md #204 / #226 slice B).
	// Successful save means the bytes in `message` are now what's in
	// Redis; record them so the next SavePlayerToRedis can do a
	// dirty-equality fast-path check. Skip if the entity was already
	// destroyed above (UnregisterPlayer + DestroyPlayer path) — there's
	// nothing to attach the component to.
	if (tlsEcs.actorRegistry.valid(playerEntity))
	{
		auto& snap = tlsEcs.actorRegistry.get_or_emplace<PlayerLastPersistedSnapshotComp>(playerEntity);
		snap.Replace(message);
	}

	// player_locator lease handles reconnect gating via Redis TTL.
}

// CONSIDER: handle reentry into a different scene node while load is still in progress
void PlayerLifecycleSystem::EnterScene(const entt::entity player, const PlayerEnterContext &ctx)
{
	const auto &enterInfo = ctx.enterInfo;
	const auto playerId = tlsEcs.actorRegistry.get<Guid>(player);
	LOG_DEBUG << "EnterScene: Player " << playerId << " entering scene node"
	         << " session=" << enterInfo.session_id()
	         << " scene_id=" << enterInfo.scene_id()
	         << " enter_gs_type=" << enterInfo.enter_gs_type()
	         << " home_zone=" << ctx.homeZoneId
	         << " owner_epoch=" << ctx.ownerEpoch;

	// 0. Cancel any pending unregistration. If this player previously called
	//    HandleExitGameNode (disconnect) but the async save hasn't completed yet,
	//    the entity still carries the UnregisterPlayer tag and the save callback
	//    will destroy it. Reconnect supersedes the logout intent — clear the tag
	//    so HandlePlayerAsyncSaved leaves the live entity alone.
	if (tlsEcs.actorRegistry.any_of<UnregisterPlayer>(player))
	{
		tlsEcs.actorRegistry.remove<UnregisterPlayer>(player);
		LOG_INFO << "EnterScene: cancelled pending unregistration for reconnected player " << playerId;
	}

	// 0.5 归属:放在场景查找之前 —— 归属讲的是"这份数据归谁、谁持有",与放没放进场景无关,
	//     实体只要在本节点存在、会被存盘,就必须带着 Go 这次路由决策给的值。
	//     0 一律不覆盖(旧版 gate / scene_manager 未填,或首登时 Init 刚挂的零值组件);
	//     epoch 取 max:见 PlayerOwnerEpochComp 的说明。
	//     非 per-tick 路径,get_or_emplace 合规(AGENTS §7.5)。
	if (ctx.homeZoneId != 0)
	{
		auto &homeZone = tlsEcs.actorRegistry.get_or_emplace<PlayerHomeZoneComp>(player);
		if (homeZone.homeZoneId != 0 && homeZone.homeZoneId != ctx.homeZoneId)
		{
			// home_zone 是 data_service 的稳定事实,同一玩家两次路由给出不同值只可能是
			// 上游配置 / 合服操作出了问题;以最新路由为准,但必须留下证据。
			LOG_WARN << "EnterScene: home_zone changed for player " << playerId
					 << " " << homeZone.homeZoneId << " -> " << ctx.homeZoneId;
		}
		homeZone.homeZoneId = ctx.homeZoneId;
	}
	if (ctx.ownerEpoch != 0)
	{
		auto &ownerEpoch = tlsEcs.actorRegistry.get_or_emplace<PlayerOwnerEpochComp>(player);
		if (ctx.ownerEpoch < ownerEpoch.epoch)
		{
			LOG_WARN << "EnterScene: ignoring stale owner_epoch " << ctx.ownerEpoch
					 << " for player " << playerId << " (cached " << ownerEpoch.epoch << ")";
		}
		ownerEpoch.epoch = std::max(ownerEpoch.epoch, ctx.ownerEpoch);
	}

	// 1. Bind session: map session_id -> player_id on this Scene node
	//    so SendMessageToPlayer can route by session, and
	//    SendMessageToClientViaGate can find the gate node.
	if (enterInfo.session_id() != 0)
	{
		// Clean up old session mapping if player already had a different session
		// (e.g. reconnect with new Gate connection). Prevents orphaned entries.
		auto &snapshot = tlsEcs.actorRegistry.get_or_emplace<PlayerSessionSnapshotComp>(player);
		const auto oldSessionId = snapshot.gate_session_id();
		if (oldSessionId != 0 && oldSessionId != enterInfo.session_id())
		{
			SessionMap().erase(oldSessionId);
			LOG_INFO << "EnterScene: cleaned up old session " << oldSessionId
			         << " for player " << playerId;
		}

		SessionMap().insert_or_assign(enterInfo.session_id(), playerId);
		snapshot.set_gate_session_id(enterInfo.session_id());
		snapshot.set_player_id(playerId);
	}

	// 2. Find the target scene entity by scene_id (allocated by SceneManager).
	entt::entity targetScene = entt::null;
	if (enterInfo.scene_id() != 0)
	{
		auto view = tlsEcs.sceneRegistry.view<SceneInfoComp>();
		for (auto entity : view)
		{
			const auto &info = view.get<SceneInfoComp>(entity);
			if (info.scene_id() == enterInfo.scene_id())
			{
				targetScene = entity;
				break;
			}
		}

		if (targetScene == entt::null)
		{
			LOG_ERROR << "EnterScene: scene_id=" << enterInfo.scene_id()
			          << " not found on this node for player " << playerId;
		}
	}

	// 放不进场景就必须 fail-closed,不能继续往下走。
	//
	// 旧实现只打一条 ERROR 就接着执行第 4/5 步:玩家被标记成"已登录"、
	// PlayerLoginEvent 照常触发(任务、每日奖励等业务系统开始结算),但他不在
	// 任何场景里 —— 没有 AOI、收不到广播,客户端也没收到 NotifyEnterScene,
	// 会永远停在加载界面。更糟的是 enter_gs_type 已被写入,玩家重试进场时
	// `alreadyLoggedIn` 为真,登录事件**再也不会补触发**,这一次的登录结算
	// 就永久丢了。
	//
	// 这里既不置登录态也不触发事件,只给客户端一个明确的失败提示,让它走
	// 正常重试路径(与 HandlePlayerAsyncLoadFailed 的 RedisError 分支同款处理)。
	if (targetScene == entt::null)
	{
		SendTipToPendingSession(enterInfo.session_id(), playerId, kEnterSceneFailed);
		LOG_ERROR << "EnterScene: aborting entry for player " << playerId
		          << " (scene_id=" << enterInfo.scene_id()
		          << " unavailable); login state NOT set so a retry can still fire PlayerLoginEvent.";
		return;
	}

	// 3. Enter the scene: bind player to scene entity and send client notification.
	PlayerSceneSystem::HandleEnterScene(player, targetScene);

	// 3.1 玩家已经被路由放进了场景:此前记下的"换图在途"(PlayerSceneChangeInFlightComp)到此兑现。
	//     不等 scene_manager 的 gRPC 应答来摘:路由(Kafka → gate → 本节点)与应答(gRPC)是两条
	//     通道,客户端收到 EnterSceneS2C 后立刻发下一次换图时,上一条的应答可能还没到,留着它会把
	//     这次合法请求误拒成"切换中"。随后到达的成功应答找不到在途组件,是静默 no-op。
	tlsEcs.actorRegistry.remove<PlayerSceneChangeInFlightComp>(player);

	// 3.5 组队:刷新 TeamId 并检查同节点跟随(team-system.md §F.2)。
	//     放在 HandleEnterScene 之外:它对"已在目标场景"幂等早退,30s 宽限期内同场景重连
	//     会走到那个早退,放在里面就会漏刷新。战斗在途的判定在跟随链内部直接读 battle:lock,
	//     不依赖第 6 步冻结重建与本链的回调先后。
	PlayerTeamSystem::OnEnteredScene(player);

	// 4. Set login state for downstream systems (reconnect, first-login logic, etc.).
	if (enterInfo.enter_gs_type() != 0)
	{
		auto &enterState = tlsEcs.actorRegistry.get_or_emplace<PlayerEnterGameStateComp>(player);
		const bool alreadyLoggedIn = (enterState.enter_gs_type() != 0);
		enterState.set_enter_gs_type(enterInfo.enter_gs_type());

		// 5. Fire login event only on first entry — skip on duplicate/reconnect
		//    to avoid business systems (quests, daily rewards, etc.) reacting twice.
		if (!alreadyLoggedIn)
		{
			PlayerLoginEvent loginEvent;
			loginEvent.set_actor_entity(entt::to_integral(player));
			loginEvent.set_enter_gs_type(enterInfo.enter_gs_type());
			tlsEcs.dispatcher.trigger(loginEvent);
		}
	}

	// 6. 回合制战斗登录后置钩子:先应用离线挂起结算(再放开排队),
	//    RECONNECT 且战斗在途时向 gate 重发 BindBattleEvent 并提示客户端补拉。
	//    放在场景绑定成功之后:会话快照与 gate 路由此时才可靠。
	PlayerBattleSystem::OnPlayerEnterScene(player, enterInfo.enter_gs_type());
}



void PlayerLifecycleSystem::HandleBindPlayerToGateOK(entt::entity player)
{
	// TODO: notify client that gate binding is ready; send initial scene state snapshot
}

// TODO: Validate session before removal
void PlayerLifecycleSystem::RemovePlayerSession(const Guid playerId)
{
	auto playerIt = tlsEcs.playerList.find(playerId);
	if (playerIt == tlsEcs.playerList.end())
	{
		LOG_ERROR << "RemovePlayerSession: player entity not found in session map for player: " << playerId;
		return;
	}
	RemovePlayerSession(playerIt->second);
}

void PlayerLifecycleSystem::RemovePlayerSession(entt::entity player)
{
	auto *const playerSessionSnapshotPB = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(player);
	if (playerSessionSnapshotPB == nullptr)
	{
		LOG_ERROR << "RemovePlayerSession: PlayerSessionSnapshotComp not found for player: " << entt::to_integral(player);
		return;
	}

	// 必须**先取值**再改字段,不能用 defer:defer 宏是 [&] 捕获、作用域结束才求值,
	// 旧写法 `defer(erase(snapshot->gate_session_id())); snapshot->set(kInvalid)` 的
	// 实际执行序是先把字段改成 kInvalidSessionId、defer 再拿着 kInvalidSessionId 去
	// erase —— 真正的旧 session 从来没被删掉过。后果:每次断线/顶号在 SessionMap
	// 残留一条 session→player 映射,长期运行的节点上无界增长;残留映射还会让
	// 已死 session 的消息一路查到玩家实体(幸有 PlayerSessionSnapshotComp 的
	// stale-session 守卫拦下投递,但那层守卫从来不是为兜这个漏设计的)。
	const auto sessionIdToErase = playerSessionSnapshotPB->gate_session_id();
	LOG_INFO << "Removing player session: sessionId = " << sessionIdToErase;

	playerSessionSnapshotPB->set_gate_session_id(kInvalidSessionId);
	SessionMap().erase(sessionIdToErase);
}

void PlayerLifecycleSystem::RemovePlayerSessionSilently(Guid playerId)
{
	auto playerIt = tlsEcs.playerList.find(playerId);
	if (playerIt == tlsEcs.playerList.end())
	{
		return;
	}
	RemovePlayerSession(playerIt->second);
}

void PlayerLifecycleSystem::DestroyPlayer(Guid playerId)
{
	LOG_INFO << "Destroying player: " << playerId;

	const auto playerEntity = tlsEcs.GetPlayer(playerId);

	// 异常检测的滑动窗口桶按 entt::entity 建键,而它自己没有任何回收挂钩:
	// 玩家销毁后桶永远留在 thread_local map 里(entt 复用槽位会递增 version,
	// 新实体的 key 与旧的不等,旧桶永不再命中也永不释放)。长期运行的场景节点
	// 上,每个下线玩家在每个碰过的币种/物品 config 上各留一个死桶 —— 违反
	// 「数据增长有界」。这里是玩家实体销毁的唯一出口,顺手清掉。
	AnomalyDetector::ClearPlayer(playerEntity);

	// 把玩家从所在场景的 ScenePlayers 里摘掉 —— 必须在 DestroyEntity 之前。
	//
	// 现有的销毁路径(正常登出 HandleExitGameNode、被废黜 DestroyDeposedPlayer)都会先走
	// DetachFromScene,此时 SceneEntityComp 已被摘除,下面的 try_get 拿不到、自然跳过(幂等)。
	// 这里是兜底:DestroyEntity 只动 actorRegistry,而 ScenePlayers 在 sceneRegistry,没有任何
	// on_destroy 钩子会替它清。哪条新路径忘了先摘场景就直接调 DestroyPlayer(已下线的
	// player_migrate 搬数据链就犯过),源场景的 ScenePlayers 里会留下一个悬垂 entity id。
	//
	// 后果与 DetachFromScene 注释里写的完全一样:entt 会复用实体 id,
	// 源场景残留的陈旧 id 过一阵子可能正好是另一个场景里某个活着的玩家,
	// 一旦源场景被 BeginSceneDrain 排空,就会给那个不相干的玩家错发改派票、
	// 把他从当前场景踢走。放在这个"唯一销毁出口"里做,一次覆盖全部销毁路径。
	if (const auto *sceneComp = tlsEcs.actorRegistry.try_get<SceneEntityComp>(playerEntity))
	{
		if (auto *scenePlayers = tlsEcs.sceneRegistry.try_get<ScenePlayers>(sceneComp->sceneEntity))
		{
			scenePlayers->erase(playerEntity);
		}
	}

	defer(tlsEcs.playerList.erase(playerId));
	DestroyEntity(tlsEcs.actorRegistry, playerEntity);
}

void PlayerLifecycleSystem::DetachFromScene(entt::entity player)
{
	auto *sceneComp = tlsEcs.actorRegistry.try_get<SceneEntityComp>(player);
	if (sceneComp == nullptr)
	{
		return;
	}

	BeforeLeaveScene leaveEvent;
	leaveEvent.set_entity(entt::to_integral(player));
	tlsEcs.dispatcher.trigger(leaveEvent);

	// 把玩家从所在场景的 ScenePlayers 里摘掉。
	//
	// 之前这里只删了玩家身上的 SceneEntityComp,场景那一侧的集合从来没清过;
	// 换场景那条路径(player_scene.cpp)手工 erase 了,退出这条路径没有。
	// ScenePlayers 是弱引用集合、以前没有真正的消费者,所以这个泄漏一直是静默的。
	//
	// 现在 BeginSceneDrain 会遍历它来决定这个场景还有谁要改派,泄漏就变成了
	// 会伤到玩家的 bug:entt 会复用实体 id,场景 A 里的一个陈旧 id 过一阵子
	// 可能正好是场景 B 里某个活着的玩家,排空 A 会把那个不相干的玩家从 B 踢走。
	if (auto *scenePlayers = tlsEcs.sceneRegistry.try_get<ScenePlayers>(sceneComp->sceneEntity))
	{
		scenePlayers->erase(player);
	}

	tlsEcs.actorRegistry.remove<SceneEntityComp>(player);
	// Hex 必须和 SceneEntityComp 成对回收。换场景那条路径(player_scene.cpp:194)
	// 显式删了 Hex 并注明"让 AOI 把新场景当成一次全新进场",退出这条路径漏了。
	// 留着 Hex 的后果:存盘在途(savePending)期间玩家重连、实体被复用时,
	// AoiSystem::UpdateGridState 会走"位置更新"分支而不是"首次进场"分支;
	// 若重连点与旧 hex 相同,hex_distance==0 直接 return,实体再也不会被插进
	// 任何格子 —— 谁都看不见他,他也看不见任何人,且没有任何路径能自愈。
	tlsEcs.actorRegistry.remove<Hex>(player);
}

void PlayerLifecycleSystem::HandleExitGameNode(entt::entity player)
{
	// valid() 必须在 try_get 之前:对已销毁的实体调 try_get 是 entt 的未定义行为,
	// 旧顺序是先 try_get 再判 valid,等于先踩了再检查。
	if (!tlsEcs.actorRegistry.valid(player))
	{
		LOG_ERROR << "HandleExitGameNode: Player entity is not valid";
		return;
	}

	const auto* g = tlsEcs.actorRegistry.try_get<Guid>(player);
	LOG_INFO << "HandleExitGameNode: Player " << (g ? *g : 0) << " is exiting the scene node";

	if (tlsEcs.actorRegistry.all_of<UnregisterPlayer>(player))
	{
		LOG_INFO << "Player " << (g ? *g : 0) << " is already marked for unregistration";
		return;
	}

	auto& unregisterTag = tlsEcs.actorRegistry.emplace<UnregisterPlayer>(player);
	unregisterTag.set_logout_initiated_ms(TimeSystem::NowMillisecondsUTC());

	// Remove entity from AOI grid immediately so the AOI system stops
	// sending messages to the (already-disconnected) gate session.
	DetachFromScene(player);

	// Capture a logout snapshot before persisting (safety net for rollback).
	SnapshotSystem::CaptureAndSend(player, SNAPSHOT_LOGOUT);

	const Guid exitingPlayerId = (g != nullptr) ? *g : kInvalidGuid;
	const bool savePending = PlayerLifecycleSystem::SavePlayerToRedis(player);

	if (!savePending)
	{
		// proto-compare 快路径判定"Redis 里已经是同一份数据",于是本次不写盘,
		// HandlePlayerAsyncSaved **永远不会**被调用。
		//
		// 旧实现到这里就 return 了,于是 UnregisterPlayer 标记的实体、SessionMap 条目、
		// tlsEcs.playerList 条目全部留在内存里再也不清 —— 玩家看起来"还在线",
		// 重连时还得靠别的兜底路径。AFK 踢下线是最容易命中的场景:玩家挂机不动,
		// 数据与上一次周期存盘逐字节相同,快路径必然跳过。
		//
		// 数据已经在盘上,直接跑与存盘回调相同的收尾。
		LOG_INFO << "HandleExitGameNode: player " << exitingPlayerId
				 << " already persisted (dirty-save fast path); finishing exit inline";
		FinishExitAfterPersist(exitingPlayerId);
		return;
	}

	// Re-login race protection (todo.md #280). The save is now in flight.
	// THREE layers cover the read-stale-data window between SavePlayerToRedis
	// being enqueued here and HandlePlayerAsyncSaved firing:
	//
	//   1. Same-node reconnect — EnterScene() clears the UnregisterPlayer tag
	//      so HandlePlayerAsyncSaved drops the stale unregister intent and
	//      leaves the live entity alone (see EnterScene step 0).
	//
	//   2. Cross-node reconnect — player_locator holds a 30s Redis lease
	//      (DefaultTTLSeconds in player_locator.yaml). Any re-login within
	//      that window is steered back to the original node, which falls
	//      back to layer 1.
	//
	//   3. Save-exceeds-lease anomaly — if Redis/Kafka backoff pushes the
	//      save past 30s the player CAN appear on a fresh node before
	//      HandlePlayerAsyncSaved fires. HandlePlayerAsyncSaved logs a
	//      "save outran reconnect lease" warning by comparing
	//      logout_initiated_ms; ops watch the warning and follow up if
	//      it's not just a one-off spike.
	//
	// IsSaveInFlight() exposes layers 1–2 for callers that want to query
	// rather than rely on the implicit ECS-marker convention.
}

void PlayerLifecycleSystem::FinishExitAfterPersist(Guid playerId)
{
	if (playerId == kInvalidGuid)
	{
		LOG_ERROR << "FinishExitAfterPersist: invalid player id";
		return;
	}

	const auto playerEntity = tlsEcs.GetPlayer(playerId);

	// 归属交接在途(PlayerTravelHandoffComp):退出优先。交接只在"状态已落盘 + 输入已冻结"
	// 之后发起,盘上就是最新状态,本地实体没有任何目的地还要等的东西 —— 直接按普通退出销毁。
	// scene_manager 那边的应答随后到达时实体已不在,HandleTravelEnterSceneReply 幂等忽略;
	// 它若已放行,location 已指向目标(跨 zone 时 node 为空),下次登录按 Offline-Return 规则处理。
	if (tlsEcs.actorRegistry.valid(playerEntity) &&
		tlsEcs.actorRegistry.any_of<PlayerTravelHandoffComp>(playerEntity))
	{
		LOG_INFO << "FinishExitAfterPersist: player " << playerId
				 << " exited during ownership handoff; dropping handoff intent (exit wins)";
		tlsEcs.actorRegistry.remove<PlayerTravelHandoffComp>(playerEntity);
		tlsEcs.actorRegistry.remove<PlayerFrozenComp>(playerEntity);
	}

	// Frozen 只应与 PlayerTravelHandoffComp 成对出现(上面已一起摘掉)。走到这里还带着 Frozen
	// 说明有路径漏摘 —— 记一条错误日志后解冻继续退出,绝不悬挂:这里原先是"等 ACK 或 reaper"的
	// 挂起分支,而 player_migrate 搬数据链已下线(CZ-1),没有任何人会再来解它。
	if (tlsEcs.actorRegistry.valid(playerEntity) &&
		tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(playerEntity))
	{
		LOG_ERROR << "FinishExitAfterPersist: orphan PlayerFrozenComp without handoff intent, player "
				  << playerId << "; unfreezing (exit wins)";
		tlsEcs.actorRegistry.remove<PlayerFrozenComp>(playerEntity);
	}

	// 顺序不能反:先改派再摘 session 的话,改派用到的 session 已经没了;
	// 先销毁实体再改派的话,票据里的 gate/session 也拿不到。
	// 改派只需要票据里抄下来的信息,所以放在最前面。
	DispatchEmergencyRelocate(playerId);

	RemovePlayerSession(playerId);
	LOG_INFO << "Player session removed";
	DestroyPlayer(playerId);
}

// 抄一份改派票据并把玩家推进退出流程。
//
// 票据必须在实体销毁前抄:改派要用的 gate / session 挂在实体上,
// HandleExitGameNode 的存盘回调会把实体销毁掉。
//
// 返回 true 表示登记了票据(玩家有活着的 gate 会话,稍后会被改派);
// false 表示玩家已经断线,只存盘不改派。
//
// 供两条路径共用:整节点疏散(身份冲突)与单场景排空(频道缩容)。
// 两者对玩家的处理完全一样 —— 存盘、改派到大世界、销毁本地实体 ——
// 区别只在于**范围**,以及节点自己要不要跟着退出。所以逻辑只写一份。
bool PlayerLifecycleSystem::EnqueueRelocateTicket(entt::entity playerEntity, const char *reasonTag)
{
	if (!tlsEcs.actorRegistry.valid(playerEntity))
	{
		return false;
	}
	const auto *guid = tlsEcs.actorRegistry.try_get<Guid>(playerEntity);
	if (guid == nullptr || *guid == kInvalidGuid)
	{
		return false;
	}

	bool ticketed = false;
	const auto *session = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(playerEntity);
	if (session != nullptr && session->gate_session_id() != kInvalidSessionId)
	{
		EmergencyRelocateTicket ticket;
		ticket.playerId = *guid;
		ticket.sessionId = session->gate_session_id();
		ticket.gateNodeId = GetGateNodeId(session->gate_session_id());
		ticket.gateInstanceId = ResolveGateInstanceId(session->gate_session_id());
		tlsEmergencyRelocateTickets.insert_or_assign(*guid, std::move(ticket));
		ticketed = true;
	}
	else
	{
		// 没有 gate 会话就没法改派(玩家已经断线),存盘仍然要做。
		LOG_INFO << "[" << reasonTag << "] player " << *guid
				 << " has no live gate session; persisting only";
	}

	// 已经在退出流程中的玩家这里是 no-op,票据仍然登记着,
	// 等它自己的存盘回调到达时一样会被消费。
	HandleExitGameNode(playerEntity);
	return ticketed;
}

void PlayerLifecycleSystem::BeginEmergencyRelocateAll()
{
	if (tlsEmergencyRelocating)
	{
		return;
	}
	tlsEmergencyRelocating = true;

	// 存盘链路会在回调里销毁实体,先把实体列表固化下来再遍历。
	auto view = tlsEcs.actorRegistry.view<Player>();
	std::vector<entt::entity> players(view.begin(), view.end());

	LOG_WARN << "[EmergencyRelocate] node identity lost; persisting and relocating "
			 << players.size() << " online player(s) to the main world";

	for (auto entity : players)
	{
		EnqueueRelocateTicket(entity, "EmergencyRelocate");
	}
}

std::size_t PlayerLifecycleSystem::BeginSceneDrain(entt::entity sceneEntity)
{
	auto *scenePlayers = tlsEcs.sceneRegistry.try_get<ScenePlayers>(sceneEntity);
	if (scenePlayers == nullptr || scenePlayers->empty())
	{
		return 0;
	}

	// 退出流程会把玩家从 ScenePlayers 里摘掉,边遍历边改会失效迭代器,
	// 所以先固化一份快照。
	std::vector<entt::entity> residents(scenePlayers->begin(), scenePlayers->end());

	LOG_WARN << "[SceneDrain] draining scene entity " << entt::to_integral(sceneEntity)
			 << ": relocating " << residents.size() << " player(s) to the main world";

	std::size_t relocating = 0;
	for (auto entity : residents)
	{
		if (EnqueueRelocateTicket(entity, "SceneDrain"))
		{
			++relocating;
		}
	}
	return relocating;
}

bool PlayerLifecycleSystem::IsEmergencyRelocateDrained()
{
	if (!tlsEmergencyRelocating)
	{
		return true;
	}
	// 票据清空 = 每个有会话的玩家都已存盘落地并进入改派;
	// 标记在途为 0 = 这些改派的 EnterScene 确实都发出去了(写 handoff 标记是异步的);
	// 实体清空 = 本地不再持有任何玩家状态。
	return tlsEmergencyRelocateTickets.empty() &&
		   tlsRelocateHandoffMarksInFlight == 0 &&
		   tlsEcs.actorRegistry.view<Player>().size() == 0;
}

void PlayerLifecycleSystem::DispatchEmergencyRelocate(Guid playerId)
{
	auto it = tlsEmergencyRelocateTickets.find(playerId);
	if (it == tlsEmergencyRelocateTickets.end())
	{
		return; // 不在疏散中,或者已经派发过
	}
	const EmergencyRelocateTicket ticket = it->second;
	tlsEmergencyRelocateTickets.erase(it);

	// 换手门(CZ-4):scene_manager 只凭 player:{id}:handoff 放行"已有位置记录的跨节点落点"。
	// 走到这里时存盘已经落地(FinishExitAfterPersist 的前置条件),正是可以写标记的那一刻;
	// 不写的话生产配置(AllowUnsafeCrossNodeHandoff=false)下疏散 / 排空的每一个玩家都会被
	// ErrHandoffPending 挡回来,而本地实体马上就要销毁,玩家只能自己重登。
	// 标记落地之后再发 EnterScene;实体此后立刻销毁,回调只用按值捕获的票据。
	uint64_t ownerEpoch = 0;
	if (const auto playerEntity = tlsEcs.GetPlayer(playerId); tlsEcs.actorRegistry.valid(playerEntity))
	{
		if (const auto *epochComp = tlsEcs.actorRegistry.try_get<PlayerOwnerEpochComp>(playerEntity))
		{
			ownerEpoch = epochComp->epoch;
		}
	}
	auto &redis = tlsRedis.GetZoneRedis();
	if (ownerEpoch == 0 || !redis || !redis->connected())
	{
		// epoch 未铸造(兼容窗口,scene_manager 侧也没有门可过)或 Redis 不可用:照旧直接发。
		// 后者在生产下会被换手门拒绝 —— 写不出落盘凭证就不该被放行,这是 fail-closed 的本意。
		if (ownerEpoch != 0)
		{
			LOG_ERROR << "[EmergencyRelocate] zone redis unavailable; handoff mark not written for player "
					  << playerId << ", scene_manager will refuse the re-home until the player re-enters";
		}
		SendEmergencyRelocateEnterScene(playerId, ticket);
		return;
	}

	const std::string key = player_ownership::HandoffRedisKey(playerId);
	const std::string value = player_ownership::HandoffRedisValue(ownerEpoch, TimeSystem::NowMillisecondsUTC());
	++tlsRelocateHandoffMarksInFlight;
	const int ret = redis->command(
		[playerId, ticket](hiredis::Hiredis *, redisReply *reply)
		{
			--tlsRelocateHandoffMarksInFlight;
			if (reply == nullptr || reply->type == REDIS_REPLY_ERROR)
			{
				LOG_ERROR << "[EmergencyRelocate] SET handoff mark failed for player " << playerId
						  << (reply != nullptr && reply->str != nullptr ? std::string(" err=") + reply->str : "")
						  << "; requesting re-home anyway (scene_manager decides)";
			}
			SendEmergencyRelocateEnterScene(playerId, ticket);
		},
		"SET %s %s EX %d", key.c_str(), value.c_str(), player_ownership::kHandoffMarkTtlSec);
	if (ret != REDIS_OK)
	{
		// 命令没发出去,回调不会来:自己把计数还回去。
		--tlsRelocateHandoffMarksInFlight;
		LOG_ERROR << "[EmergencyRelocate] redis command dispatch failed for player " << playerId;
		SendEmergencyRelocateEnterScene(playerId, ticket);
	}
}

void PlayerLifecycleSystem::SendEmergencyRelocateEnterScene(Guid playerId, const EmergencyRelocateTicket &ticket)
{
	const auto smEntity = GetSceneManagerEntity(playerId);
	if (smEntity == entt::null)
	{
		LOG_ERROR << "[EmergencyRelocate] no SceneManager node reachable; player " << playerId
				  << " keeps its gate session and has to re-enter through the normal login flow";
		return;
	}
	auto &smRegistry = tlsNodeContextManager.GetRegistry(eNodeType::SceneManagerNodeService);

	::scene_manager::EnterSceneRequest req;
	req.set_player_id(playerId);
	// scene_id / scene_conf_id 都留 0:让 SceneManager 按它自己的世界频道表挑一个
	// **存活节点**上的大世界频道。C++ 侧不复制一份地图/频道选择规则(权威只有一份),
	// 于是"副本节点挂了"与"大世界节点挂了"落到完全相同的一条路径。
	req.set_session_id(ticket.sessionId);
	req.set_gate_id(std::to_string(ticket.gateNodeId));
	req.set_gate_instance_id(ticket.gateInstanceId);
	req.set_gate_zone_id(GetZoneId());
	req.set_zone_id(GetZoneId());
	// 刻意不设 request_id。SceneManager 的 request_id 去重是 60s SETNX,一旦填一个
	// 按 (node, player) 稳定的键,同一个玩家在 60s 内被排空第二次(频道缩容会发生)
	// 就会被静默丢掉,玩家卡在原地。这里本来也不需要去重:票据在发送前就已经从
	// tlsEmergencyRelocateTickets 里删掉了,每张票最多发一次,也没有重试。
	// 其它 EnterScene 调用点(player_scene.cpp)同样不带 request_id。
	scene_manager::SendSceneManagerEnterScene(smRegistry, smEntity, req);

	LOG_INFO << "[EmergencyRelocate] requested main-world re-home for player " << playerId
			 << " (session=" << ticket.sessionId << ", gate=" << ticket.gateNodeId << ")";
}

entt::entity PlayerLifecycleSystem::InitPlayerFromAllData(const PlayerAllData &playerAllData, const PlayerEnterContext &ctx)
{
	auto playerId = playerAllData.player_database_data().player_id();

	if (playerId == 0)
	{
		LOG_ERROR << "[InitPlayerFromAllData] Rejecting player with id=0 (empty data)";
		return entt::null;
	}

	LOG_INFO << "[InitPlayerFromAllData] Init player: " << playerId;

	auto player = tlsEcs.actorRegistry.create();

	// Register in global player-entity map
	if (const auto [it, inserted] = tlsEcs.playerList.emplace(playerId, player); !inserted)
	{
		LOG_ERROR << "[InitPlayerFromAllData] Player already exists in GlobalPlayerList: " << playerId;
		return entt::null;
	}

	tlsEcs.actorRegistry.emplace<Player>(player);
	tlsEcs.actorRegistry.emplace<Guid>(player, playerId);
	tlsEcs.actorRegistry.emplace<LastActiveFrameComp>(player, tlsFrameTimeManager.frameTime.current_frame());

	// 归属组件建实体即挂(零值),真实值由随后的 EnterScene 按本次路由上下文赋(ctx 为 0 时保持 0)。
	// 先挂零值而不是等 EnterScene:存盘路径按"组件缺失 == 0"处理也行,但统一存在能让
	// try_get 分支少一种形态。
	tlsEcs.actorRegistry.emplace<PlayerHomeZoneComp>(player);
	tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player);

	PlayerAllDataMessageFieldsUnMarshal(player, playerAllData);

	// First-time registration: initialize defaults
	if (playerAllData.player_database_data().uint64_pb_component().registration_timestamp() <= 0)
	{
		tlsEcs.actorRegistry.get_or_emplace<PlayerUint64Comp>(player).set_registration_timestamp(TimeSystem::NowSecondsUTC());
		tlsEcs.actorRegistry.get_or_emplace<LevelComp>(player).set_level(1);

		RegisterPlayerEvent registerPlayer;
		registerPlayer.set_actor_entity(entt::to_integral(player));
		tlsEcs.dispatcher.trigger(registerPlayer);
	}

	tlsEcs.actorRegistry.emplace<ViewRadius>(player).set_radius(10);

	// player_locator (Go) owns the canonical session/location record.

	// Fire component initialization events
	InitializeActorCompsEvent initActorEvent;
	initActorEvent.set_actor_entity(entt::to_integral(player));
	tlsEcs.dispatcher.trigger(initActorEvent);

	InitializePlayerCompsEvent initPlayerEvent;
	initPlayerEvent.set_actor_entity(entt::to_integral(player));
	tlsEcs.dispatcher.trigger(initPlayerEvent);

	EnterScene(player, ctx);

	// Capture a login snapshot for rollback safety net.
	SnapshotSystem::CaptureAndSend(player, SNAPSHOT_LOGIN);

	return player;
}

bool PlayerLifecycleSystem::SavePlayerToRedis(entt::entity player)
{
	if (!tlsEcs.actorRegistry.valid(player))
	{
		LOG_ERROR << "[SavePlayerToRedis] Invalid player entity";
		return false;
	}

	auto playerId = tlsEcs.actorRegistry.get<Guid>(player);

	// 交接已发起(handoff 标记已写)后本节点不得再写:标记落地那一刻起 scene_manager 随时
	// 可能放行并推进 epoch,再写只会被 CAS 拒、把 stale_owner_write_rejected 从"双主信号"
	// 变成噪声。状态自冻结起没变过,盘上就是最新的。返回 false 与快路径同义:
	// 调用方(退出流程)自己收尾,FinishExitAfterPersist 里"退出优先"会作废传送。
	if (const auto *travel = tlsEcs.actorRegistry.try_get<PlayerTravelHandoffComp>(player);
		travel != nullptr && travel->requestedAtMs != 0)
	{
		LOG_INFO << "[SavePlayerToRedis] skip: zone travel handoff already requested for player "
				 << playerId << " (target_zone=" << travel->targetZoneId << ")";
		return false;
	}

	using SaveMessage = PlayerDataRedis::element_type::MessageValuePtr;
	SaveMessage message = std::make_shared<SaveMessage::element_type>();

	PlayerAllDataMessageFieldsMarshal(player, *message);

	// Set player_id on each sub-table (not set by generated marshal code).
	// MUST be done BEFORE Save(): MessageAsyncClient::Save() now serializes the
	// payload eagerly into Element::serialized_payload, so any post-Save mutation
	// would not make it into the Redis blob.
	message->mutable_player_database_data()->set_player_id(playerId);
	message->mutable_player_database_1_data()->set_player_id(playerId);

	// Count every save attempt that reaches the fast-path check (i.e. we
	// already paid the marshal cost). The pre-condition failures above
	// (invalid entity) are not interesting for the skip-rate denominator —
	// they'd skew the ratio without telling us anything about dirty-save
	// effectiveness.
	dirty_save_stats::IncTotal();

	// Dirty-save fast path (todo.md #204 / #226 slice B).
	// MUST run BEFORE stresstest_probe::Stamp* below (Review R2 fix,
	// 2026-05-17). The probe writes non-business fields (timestamps /
	// counters) into the message which would otherwise poison the
	// equality check — under STRESS_TEST_PROBE the stamped fields
	// change every call, so a post-stamp IsEqual always returns false
	// and the optimization is silently disabled. Compare clean
	// business data here, stamp probe only on the path that actually
	// persists.
	//
	// Skipping is safe because:
	//   - The previous save already committed identical bytes; reading
	//     them back yields the same state.
	//   - HandlePlayerAsyncSaved updates the snapshot ONLY after a
	//     successful save, so a snapshot-equality match means the
	//     last persisted state matches what we'd write now.
	//   - First save (no snapshot present) always falls through and
	//     writes — `ShouldPersist(current, nullptr)` returns true.
	//
	// Limitation: snapshot lives only on the live entity. A reconnect
	// that destroys + re-creates the entity loses the snapshot, so the
	// first save after EnterScene always writes. That's intentional —
	// we'd rather pay one redundant write than risk skipping a save
	// when the in-memory state diverged from Redis during a load path
	// we don't fully trust.
	if (auto* snap = tlsEcs.actorRegistry.try_get<PlayerLastPersistedSnapshotComp>(player);
		snap != nullptr && snap->HasSnapshot() &&
		dirty_save::IsEqual(*message, *snap->snapshot))
	{
		dirty_save_stats::IncSkipped();
		LOG_DEBUG << "[SavePlayerToRedis] no-op for player " << playerId
				  << " -- proto-compare clean, last_save_ms=" << snap->saved_at_ms;
		// false = 本次没有写盘,调用方不能再指望 HandlePlayerAsyncSaved 回调。
		return false;
	}

	// Stamp the data-consistency stress probe (no-op when STRESS_TEST_PROBE
	// is unset). MUST be before Save() — payload is serialized eagerly
	// inside Save(). Stamped after the dirty-save check (see R2 note above)
	// so probe fields don't pollute the equality comparison.
	stresstest_probe::StampPlayerDatabase(*message->mutable_player_database_data());
	stresstest_probe::StampPlayerDatabase1(*message->mutable_player_database_1_data());

	// ── 归属(CZ-2 / CZ-3):落库目的地按 home_zone 选,不按进程 zone ────────────────
	// 访客在别的 zone 玩,数据仍归 home_zone;topic 选错 = 玩家数据落进别人的库,回家即回档。
	// home_zone 未知时 fail-closed 用进程 zone(改动前的行为),但必须 WARN + 计数,
	// 不许静默(不变量 §6.2)。放在快路径之后:没写盘的调用不该计入。
	uint32_t homeZoneId = 0;
	if (const auto *homeZone = tlsEcs.actorRegistry.try_get<PlayerHomeZoneComp>(player))
	{
		homeZoneId = homeZone->homeZoneId;
	}
	if (homeZoneId == 0)
	{
		owner_epoch_stats::IncHomeZoneUnknown();
		homeZoneId = GetZoneId();
		// 逐次只打 DEBUG:滚动升级窗口(旧 gate / 旧 scene_manager 不带 home_zone_id)里每个玩家
		// 每次存盘都会走到这里,WARN 会刷屏。"不静默"由计数 + redis.cpp 里每 30s 一行的
		// [OwnerEpoch] home_zone_unknown=N 汇总(WARN)保证。
		LOG_DEBUG << "[SavePlayerToRedis] home_zone unknown for player " << playerId
				 << "; falling back to process zone " << homeZoneId
				 << " (metric=home_zone_unknown). Route chain must carry RoutePlayerEvent.home_zone_id.";
	}

	// ── owner_epoch(CZ-4 ①):Redis 写带 CAS,DBTask 带 epoch ────────────────────────
	// epoch != 0:Save 的 guard 重载在 Lua 里原子比对 player:{id}:owner_epoch == 期望值,不等
	//            即拒绝并回调 HandlePlayerSaveRejected(本节点已被废黜)。
	// epoch == 0:旧版 Go 未铸造的兼容窗口,走无守卫的旧 Save,计数以便升级完成后核对恒 0。
	uint64_t ownerEpoch = 0;
	if (const auto *epochComp = tlsEcs.actorRegistry.try_get<PlayerOwnerEpochComp>(player))
	{
		ownerEpoch = epochComp->epoch;
	}
	if (ownerEpoch != 0)
	{
		tlsRedisSystem.GetPlayerDataRedis()->Save(message, playerId,
												  player_ownership::OwnerEpochRedisKey(playerId),
												  std::to_string(ownerEpoch));
	}
	else
	{
		owner_epoch_stats::IncOwnerEpochUnknown();
		tlsRedisSystem.GetPlayerDataRedis()->Save(message, playerId);
	}

	// Send each sub-table as a separate DBTask (matching how login reads per-table)
	const std::string playerIdStr = std::to_string(playerId);
	const std::string dbTaskTopic = GetDbTaskTopic(homeZoneId);

	auto sendSubTableTask = [&](const google::protobuf::Message &subMsg)
	{
		const std::string tableName(subMsg.GetDescriptor()->full_name());

		std::string bodyBytes;
		if (!subMsg.SerializeToString(&bodyBytes))
		{
			LOG_ERROR << "[SavePlayerToRedis] Serialize failed: table=" << tableName
					  << " player=" << playerId;
			return;
		}

		taskpb::DBTask dbTask;
		dbTask.set_key(playerId);
		dbTask.set_op("write");
		dbTask.set_msg_type(tableName);
		dbTask.set_body(std::move(bodyBytes));
		dbTask.set_task_id(playerIdStr + ":" + tableName + ":" + std::to_string(TimeSystem::NowMillisecondsUTC()));
		// Redis 侧的 CAS 挡不住这条通道:被废黜节点的 DBTask 仍可能后到并覆盖 MySQL。
		// db 服务落库前比对(reentry-barrier §6.3);0 表示兼容窗口放行。
		dbTask.set_owner_epoch(ownerEpoch);

		std::string dbTaskBytes;
		if (!dbTask.SerializeToString(&dbTaskBytes))
		{
			LOG_ERROR << "[SavePlayerToRedis] DBTask serialize failed: table=" << tableName
					  << " player=" << playerId;
			return;
		}

		auto err = KafkaProducer::Instance().send(dbTaskTopic, dbTaskBytes, playerIdStr);
		if (err != RdKafka::ERR_NO_ERROR)
		{
			LOG_ERROR << "[SavePlayerToRedis] Kafka send failed: table=" << tableName
					  << " player=" << playerId << " topic=" << dbTaskTopic
					  << " err=" << RdKafka::err2str(err);
		}
	};

	sendSubTableTask(message->player_database_data());
	sendSubTableTask(message->player_database_1_data());

	LOG_INFO << "[SavePlayerToRedis] Player " << playerId << " saved to Redis, DB write tasks enqueued"
			 << " (topic=" << dbTaskTopic << ", owner_epoch=" << ownerEpoch << ")";
	return true;
}

void PlayerLifecycleSystem::HandlePlayerSaveRejected(Guid playerId, const std::string &redisKey, const std::string &rejectedEpoch)
{
	// 被拒的是"发出那次存盘时缓存的 epoch"。实体此刻缓存的值若已经不同,说明那只是
	// 一笔旧代际的在途写(存盘发出之后、应答回来之前,新的路由事件把更新的 epoch 送到了
	// 本实体)——本节点仍是合法持有者,不能自毁,用当前 epoch 重新存一次把状态落地。
	// 只有被拒的值就是实体当前缓存的值,才说明 scene_manager 已把玩家改派给别人。
	if (const auto playerEntity = tlsEcs.GetPlayer(playerId); tlsEcs.actorRegistry.valid(playerEntity))
	{
		const auto *epochComp = tlsEcs.actorRegistry.try_get<PlayerOwnerEpochComp>(playerEntity);
		const std::string currentEpoch = std::to_string(epochComp != nullptr ? epochComp->epoch : 0);
		if (currentEpoch != rejectedEpoch)
		{
			LOG_WARN << "HandlePlayerSaveRejected: stale in-flight save rejected for player " << playerId
					 << " key=" << redisKey << " rejected_epoch=" << rejectedEpoch
					 << " current_epoch=" << currentEpoch << " — still the owner, re-saving with current epoch";
			// SavePlayerToRedis 返回 false = 与上次成功落盘的快照相同、无需再写,同样是安全的终点。
			SavePlayerToRedis(playerEntity);
			return;
		}
	}

	owner_epoch_stats::IncStaleOwnerWriteRejected();
	// 这条日志是"曾经出现过双主"的直接证据(cross-zone-scene-travel.md §6.3 要求压测期恒 0):
	// 本节点还拿着旧 epoch 在写,而 scene_manager 已把玩家改派出去。数据没有被污染
	// (Lua 原子拒绝),但本节点这份内存态从此作废。
	LOG_ERROR << "HandlePlayerSaveRejected: owner_epoch CAS rejected save for player " << playerId
			  << " key=" << redisKey << " epoch=" << rejectedEpoch
			  << " — this node has been deposed; dropping local state, no retry, no relocate"
			  << " (metric=stale_owner_write_rejected)";

	DestroyDeposedPlayer(playerId, "stale_owner_write_rejected");
}

void PlayerLifecycleSystem::DestroyDeposedPlayer(Guid playerId, const char *reasonTag, bool routine)
{
	const auto playerEntity = tlsEcs.GetPlayer(playerId);
	if (!tlsEcs.actorRegistry.valid(playerEntity))
	{
		// 回调到达前实体已没了(退出流程 / 另一条废黜路径先到),幂等。
		LOG_INFO << "[" << reasonTag << "] player " << playerId << " already gone; nothing to tear down";
		return;
	}

	// 疏散票据作废:改派也是 EnterScene,会拿着旧 epoch 去撞门;而且这个玩家已经有新主了。
	// 票据删掉后 IsEmergencyRelocateDrained 照常收敛。
	tlsEmergencyRelocateTickets.erase(playerId);

	// 交接标记与冻结成对摘掉。实体马上就销毁,组件本来也会跟着消失;显式先摘是为了让下面
	// DetachFromScene 触发的事件(BeforeLeaveScene …)看到的是一个普通的离场实体,而不是一个
	// 会被各业务系统的 Frozen 闸跳过的实体。remove 对不存在的组件是 no-op。
	tlsEcs.actorRegistry.remove<PlayerTravelHandoffComp>(playerEntity);
	tlsEcs.actorRegistry.remove<PlayerFrozenComp>(playerEntity);

	// 与 HandleExitGameNode 同款,只是**没有存盘**:摘场景(AOI 停止广播)→ 摘会话 → 销毁。
	DetachFromScene(playerEntity);
	RemovePlayerSession(playerId);
	DestroyPlayer(playerId);

	if (routine)
	{
		// 交接成功的正常收尾:生产配置下每次跨节点换图 / 跨 zone 传送都会走到,打 WARN 会刷屏。
		LOG_INFO << "[" << reasonTag << "] local entity for player " << playerId
				 << " destroyed without persisting (ownership handed over)";
	}
	else
	{
		LOG_WARN << "[" << reasonTag << "] local entity for player " << playerId
				 << " destroyed without persisting (ownership moved away)";
	}
}

// ─────────────────────────────────────────────────────────────────────
// 归属交接:源端释放链(cross-zone-scene-travel.md CZ-5 / §4)
//
// 两个入口,同一条链:
//   跨 zone 传送        客户端 TravelToZone ──▶ RequestZoneTravel(CZ-6 校验)──▶ StartTravelHandoff
//   同 zone 跨节点换图  普通 EnterScene 被 scene_manager 以 18 暂拒
//                       ──▶ HandleTravelEnterSceneReply ──▶ StartTravelHandoff(目标 = 本 zone)
//
//   StartTravelHandoff:挂 PlayerTravelHandoffComp + PlayerFrozenComp ──▶ SavePlayerToRedis
//   存盘落地 ──▶ BeginTravelHandoff:SET player:{id}:handoff "{epoch}:{now}" EX 300
//            ──▶ RequestTravelEnterScene:scene_manager.EnterScene(ZoneId=目标, SceneId, SceneConfId)
//            ──▶ HandleTravelEnterSceneReply:Redirect(跨 zone 放行)      → DestroyDeposedPlayer
//                                             成功无 Redirect(同 zone 放行) → ResolveTravelOutcome:
//                                                 epoch 已变 → DestroyDeposedPlayer;没变 → 静默解冻
//                                             错误 / 超时                   → ResolveTravelOutcome:
//                                                 epoch 已变 → DestroyDeposedPlayer;没变 → 解冻回 tip
//
// 为什么同 zone 换图是"被拒之后才交接"而不是每次都先存盘:scene 节点事先不知道目标场景在不在
// 本节点(频道由 scene_manager 挑),同节点换图占绝大多数且不需要冻结 / 存盘 / 销毁。只在收到 18
// 时才进入这条链,平时的换图路径一行不变,单节点部署与 dev 旁路没有回归面;代价是跨节点换图
// 多一次注定被拒的 EnterScene 往返。没有服务端自动重试环:每次交接最多重发一次,再失败就解冻。
//
// 所有异步回调只捕获 playerId(+ 代际),回调里按 id 回查实体:回调到达时实体可能已被
// 退出流程销毁、也可能已换了一次交接(AGENTS §11.7 精神)。
// ─────────────────────────────────────────────────────────────────────

uint32_t PlayerLifecycleSystem::RequestZoneTravel(entt::entity player, uint32_t targetZoneId, uint32_t sceneConfigId)
{
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return kZoneTravelTargetBusy;
	}
	const auto *guid = tlsEcs.actorRegistry.try_get<Guid>(player);
	const Guid playerId = guid != nullptr ? *guid : kInvalidGuid;

	// 目标必须是"别的 zone"。等于本 zone 不是传送而是换图(走 EnterScene);放过去会让
	// StartTravelHandoff 把它当成同 zone 交接 —— 玩家没被 18 拒过就被冻结、存盘、可能被销毁。
	// 目标 zone 是否真的存在这里判不了(C++ 侧没有 zone 表):交给 scene_manager,不存在时它回错误,
	// 走 ResolveTravelOutcome 解冻并回 kZoneTravelTargetBusy。
	if (targetZoneId == 0 || targetZoneId == GetZoneId())
	{
		LOG_WARN << "[ZoneTravel] rejected: bad target zone, player_id=" << playerId
				 << " target_zone=" << targetZoneId << " self_zone=" << GetZoneId();
		return kZoneTravelTargetZoneNotFound;
	}

	// ── CZ-6:战斗在途 / 备战中不可传送 ──
	// InBattleComp 同时覆盖 PREPARING(备战)与 FIGHTING:结算落地摘除它之前,实体必须留在本节点
	// 接收结算事件。已下线的 player_migrate 搬数据链里也有这道拦截,随函数删除后由这里接住。
	if (PlayerBattleSystem::IsInBattle(player))
	{
		LOG_WARN << "[ZoneTravel] rejected: in battle, player_id=" << playerId << " target_zone=" << targetZoneId;
		return kZoneTravelInBattle;
	}

	// ── CZ-6:组队在途不可传送 ──
	// 组队默认只许同区(team-system.md D.3),带着队伍去别的 zone,队伍跟随 / 整队匹配的语义全部失效。
	// 拒绝而不是自动离队:离队是玩家的决定。TeamId 只在 team_id != 0 时挂在实体上,是 scene 从
	// Redis 投影刷新来的缓存,可能短暂过时(刚离队还没刷新)—— 过时只会多拒一次,重试即可。
	if (const auto *team = tlsEcs.actorRegistry.try_get<TeamId>(player); team != nullptr && team->team_id() != 0)
	{
		LOG_WARN << "[ZoneTravel] rejected: in team, player_id=" << playerId
				 << " team_id=" << team->team_id() << " target_zone=" << targetZoneId;
		return kZoneTravelInTeam;
	}

	// 已冻结 / 已有交接 / 普通换图的应答还没回来:幂等拒绝。scene 的客户端消息入口不拦冻结玩家,
	// 不在这里拒,重复请求会对已存在的组件 emplace(entt 断言)或重置交接代际。
	if (IsSceneChangeBusy(player))
	{
		const bool handoffInFlight =
			tlsEcs.actorRegistry.any_of<PlayerFrozenComp, PlayerTravelHandoffComp>(player);
		LOG_INFO << "[ZoneTravel] rejected: scene change busy, player_id=" << playerId
				 << " handoff_in_flight=" << handoffInFlight;
		// 两个码分属不同的 tip 枚举(cross_server_error / scene_error),三目两侧先转成同一类型。
		return handoffInFlight ? static_cast<uint32_t>(kSceneTransferInProgress)
							   : static_cast<uint32_t>(kEnterSceneChangingScene);
	}

	// 跨 zone 传送不指定场景实例(sceneId = 0):目标 zone 的实例由它自己的 scene_manager 挑。
	return StartTravelHandoff(player, targetZoneId, /*sceneId=*/0, sceneConfigId);
}

uint32_t PlayerLifecycleSystem::StartTravelHandoff(entt::entity player, uint32_t targetZoneId, uint64_t sceneId,
													uint32_t sceneConfigId)
{
	// ── 校验段:任何一条不满足都直接返回 tip,此前不得改任何状态 ──
	if (targetZoneId == 0)
	{
		return kZoneTravelTargetZoneNotFound;
	}
	const bool crossZone = (targetZoneId != GetZoneId());
	// 没有更具体原因时的兜底码:跨 zone 传送与同 zone 换图各用各的,客户端文案不串。
	const uint32_t genericFailTip = crossZone ? static_cast<uint32_t>(kZoneTravelTargetBusy)
											  : static_cast<uint32_t>(kEnterSceneFailed);

	if (!tlsEcs.actorRegistry.valid(player))
	{
		return genericFailTip;
	}
	const auto *guid = tlsEcs.actorRegistry.try_get<Guid>(player);
	if (guid == nullptr || *guid == kInvalidGuid)
	{
		return genericFailTip;
	}
	const Guid playerId = *guid;

	// 正在退出:退出优先(与 HandlePlayerAsyncSaved / FinishExitAfterPersist 同一条纪律)。
	if (tlsEcs.actorRegistry.any_of<UnregisterPlayer>(player))
	{
		return genericFailTip;
	}
	// 已有交接 / 已冻结:不重入。对已存在的组件 emplace 是 entt 断言;replace 则会重置 requestedAtMs
	// 代际,让在途的应答与看门狗全部对不上号。
	if (tlsEcs.actorRegistry.any_of<PlayerFrozenComp, PlayerTravelHandoffComp>(player))
	{
		return crossZone ? static_cast<uint32_t>(kSceneTransferInProgress)
						 : static_cast<uint32_t>(kEnterSceneChangingScene);
	}
	// 战斗在途必须在这里重查:同 zone 交接由 18 应答触发,离发请求已隔一个往返,玩家可能刚进备战。
	// 反方向已有保护:PrepareBattle 拒绝冻结中的玩家。
	if (tlsEcs.actorRegistry.any_of<InBattleComp>(player))
	{
		return crossZone ? static_cast<uint32_t>(kZoneTravelInBattle)
						 : static_cast<uint32_t>(kEnterSceneFailed);
	}
	// gate 会话必须活着:交接的 EnterScene 要用它路由 / 重定向;断线宽限期内的实体拿旧会话去请求,
	// 会改写离线玩家的 location(与 player_team.cpp HasLiveSession 同一判法)。
	const auto *session = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(player);
	if (session == nullptr || session->gate_session_id() == 0 || session->gate_session_id() == kInvalidSessionId)
	{
		return genericFailTip;
	}
	if (const auto sessionIt = SessionMap().find(session->gate_session_id());
		sessionIt == SessionMap().end() || sessionIt->second != playerId)
	{
		return genericFailTip;
	}

	// ── 从这里开始改状态 ──
	// 普通换图的在途记录到此为止:它的使命(记住"要去哪")已经转交给交接组件。
	tlsEcs.actorRegistry.remove<PlayerSceneChangeInFlightComp>(player);

	// 逐字段赋值,不用聚合初始化(见 PlayerTravelHandoffComp 的说明)。
	auto &travel = tlsEcs.actorRegistry.emplace<PlayerTravelHandoffComp>(player);
	travel.targetZoneId = targetZoneId;
	travel.sceneId = sceneId;
	travel.sceneConfigId = sceneConfigId;
	travel.requestedAtMs = 0; // 0 = 等这次存盘落地;BeginTravelHandoff 写标记时才占代际

	// 先冻结再存盘:冻结之后状态不再变,这次存盘(或快路径判定的"盘上已是同一份")才代表
	// 玩家交接那一刻的最终态。同一 key 只发布最新快照的完成回调(redis_client.h EnqueueSave /
	// OnSaved),所以冻结前还在途的周期存盘不会抢先触发 BeginTravelHandoff。
	// frozenAtMs 另存一份局部值:下面存盘阶段看门狗要用它当代际,不隔着 SavePlayerToRedis 去读组件引用。
	const int64_t frozenAtMs = static_cast<int64_t>(TimeSystem::NowMillisecondsUTC());
	auto &frozen = tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
	frozen.frozenAtMs = frozenAtMs;
	frozen.toZoneId = targetZoneId;

	LOG_INFO << "[ZoneTravel] handoff started for player " << playerId
			 << " target_zone=" << targetZoneId << (crossZone ? " (cross-zone)" : " (same-zone cross-node)")
			 << " scene_id=" << sceneId << " scene_conf_id=" << sceneConfigId;

	// 双路径约定(见头文件 BeginTravelHandoff 的说明):返回 true = 落地回调 HandlePlayerAsyncSaved
	// 会接手;返回 false = 快路径没写盘、回调永远不会来,必须自己调。
	// BeginTravelHandoff 内部的失败分支(无 epoch / Redis 断连)会同步解冻并回 tip,这里仍返回
	// "已受理":对调用方而言就是"受理后未成",与异步失败同一种形态。
	if (SavePlayerToRedis(player))
	{
		// 存盘在途。Redis 断连时这次写会留在重试队列里,落地回调可能很久都不来,而应答看门狗要到
		// EnterScene 发出之后才挂 —— 这一段没人兜底的话,玩家会以冻结态一直等下去。
		ArmTravelSaveWatchdog(playerId, frozenAtMs);
	}
	else
	{
		BeginTravelHandoff(playerId);
	}
	return kTravelAccepted;
}

void PlayerLifecycleSystem::ArmTravelSaveWatchdog(Guid playerId, int64_t frozenAtMs)
{
	auto *loop = muduo::net::EventLoop::getEventLoopOfCurrentThread();
	if (loop == nullptr)
	{
		// 单测 / 无 loop 的宿主:同 ArmTravelReplyWatchdog,只靠落地回调与"退出优先"收敛。
		return;
	}
	// 只捕获 id + 代际(frozenAtMs:这一段 requestedAtMs 还是 0,分不出是哪一次交接)。不保存 TimerId、不取消。
	loop->runAfter(kTravelReplyBudgetSec, [playerId, frozenAtMs]()
	{
		const auto entity = tlsEcs.GetPlayer(playerId);
		if (!tlsEcs.actorRegistry.valid(entity))
		{
			return;
		}
		const auto *travel = tlsEcs.actorRegistry.try_get<PlayerTravelHandoffComp>(entity);
		const auto *frozen = tlsEcs.actorRegistry.try_get<PlayerFrozenComp>(entity);
		if (travel == nullptr || frozen == nullptr || frozen->frozenAtMs != frozenAtMs || travel->requestedAtMs != 0)
		{
			return; // 已经发起(归应答看门狗管)/ 已结束 / 已是另一次交接
		}
		// requestedAtMs 仍为 0 = 标记没写、EnterScene 没发,不可能被放行,可以直接解冻(不需要
		// ResolveTravelOutcome)。那次存盘之后照常落地也无妨:届时实体上已没有交接意图,
		// HandlePlayerAsyncSaved 按普通存盘处理。
		AbortTravelHandoff(playerId, "handoff save did not land in time");
	});
}

bool PlayerLifecycleSystem::IsSceneChangeBusy(entt::entity player)
{
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return false;
	}
	if (tlsEcs.actorRegistry.any_of<PlayerFrozenComp, PlayerTravelHandoffComp>(player))
	{
		return true;
	}
	const auto *pending = tlsEcs.actorRegistry.try_get<PlayerSceneChangeInFlightComp>(player);
	if (pending == nullptr)
	{
		return false;
	}
	// 时钟回拨(now < sentAtMs)按已过期处理:宁可多放一条请求,也不能把玩家长期挡在换图之外。
	const uint64_t nowMs = TimeSystem::NowMillisecondsUTC();
	return nowMs >= pending->sentAtMs && nowMs - pending->sentAtMs < kSceneChangeInFlightTtlMs;
}

void PlayerLifecycleSystem::NoteSceneChangeRequested(entt::entity player, uint64_t sceneId, uint32_t sceneConfigId)
{
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return;
	}
	// 一次换图请求一次,不是 per-tick 路径,emplace_or_replace 合规(AGENTS §7.5)。
	// replace 语义是有意的:TTL 过期后放行的下一条请求要覆盖上一条的目标。
	auto &pending = tlsEcs.actorRegistry.emplace_or_replace<PlayerSceneChangeInFlightComp>(player);
	pending.sceneId = sceneId;
	pending.sceneConfigId = sceneConfigId;
	pending.sentAtMs = TimeSystem::NowMillisecondsUTC();
}

bool PlayerLifecycleSystem::IsHandoffRequested(entt::entity player)
{
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return false;
	}
	const auto *travel = tlsEcs.actorRegistry.try_get<PlayerTravelHandoffComp>(player);
	return travel != nullptr && travel->requestedAtMs != 0;
}

bool PlayerLifecycleSystem::DiscardStaleHandoffEntity(entt::entity player, uint64_t incomingOwnerEpoch)
{
	// 0 = 上游没填 epoch(旧版 gate / scene_manager),无从比较,保持原有的"复用实体"行为。
	if (incomingOwnerEpoch == 0 || !IsHandoffRequested(player))
	{
		return false;
	}
	uint64_t cachedEpoch = 0;
	if (const auto *epochComp = tlsEcs.actorRegistry.try_get<PlayerOwnerEpochComp>(player))
	{
		cachedEpoch = epochComp->epoch;
	}
	if (incomingOwnerEpoch <= cachedEpoch)
	{
		// 相等 = 同 zone 重发后落回了本节点(同物理节点不铸造 epoch),本实体就是合法持有者,照常复用,
		//        随后到达的成功应答会经 ResolveTravelOutcome 静默解冻;
		// 更小 = 乱序到达的旧路由,EnterScene 自己会按 max 忽略。
		return false;
	}
	const auto *guid = tlsEcs.actorRegistry.try_get<Guid>(player);
	if (guid == nullptr)
	{
		return false;
	}
	const Guid playerId = *guid;
	// epoch 比缓存的新 = 我发起的那次交接其实已被放行(应答丢了、看门狗还没到期),玩家在别处
	// 玩过之后又被派回本节点。盘上是他在别处的最新状态,本实体是交接那一刻的旧状态。
	LOG_WARN << "[ZoneTravel] re-entry with newer owner_epoch " << incomingOwnerEpoch << " (cached " << cachedEpoch
			 << ") hit a stale in-handoff entity for player " << playerId
			 << "; discarding it so the player is reloaded from storage";
	DestroyDeposedPlayer(playerId, "handoff_superseded_by_reentry");
	return true;
}

void PlayerLifecycleSystem::BeginTravelHandoff(Guid playerId)
{
	const auto playerEntity = tlsEcs.GetPlayer(playerId);
	if (!tlsEcs.actorRegistry.valid(playerEntity))
	{
		return;
	}
	auto *travel = tlsEcs.actorRegistry.try_get<PlayerTravelHandoffComp>(playerEntity);
	if (travel == nullptr)
	{
		return;
	}
	if (travel->requestedAtMs != 0)
	{
		// 已发起(如周期存盘与退出存盘的两次回调都到了这里):不重复写标记、不重复请求。
		return;
	}

	uint64_t ownerEpoch = 0;
	if (const auto *epochComp = tlsEcs.actorRegistry.try_get<PlayerOwnerEpochComp>(playerEntity))
	{
		ownerEpoch = epochComp->epoch;
	}
	if (ownerEpoch == 0)
	{
		// 没有 epoch 就过不了 CZ-4 ② 那道门(scene_manager 要求 handoff.epoch == 当前 owner_epoch),
		// 请求出去也只会被拒;而且 epoch 为 0 说明路由链还没升级完,这时跨 zone 交接本身就不安全。
		AbortTravelHandoff(playerId, "owner_epoch unknown (0); route chain not upgraded");
		return;
	}

	auto &redis = tlsRedis.GetZoneRedis();
	if (!redis || !redis->connected())
	{
		// 标记写不进去就等于没落盘凭证,fail-closed:不发 EnterScene,让玩家留在本节点重试。
		AbortTravelHandoff(playerId, "zone redis not connected");
		return;
	}

	const uint64_t nowMs = TimeSystem::NowMillisecondsUTC();
	const std::string key = player_ownership::HandoffRedisKey(playerId);
	const std::string value = player_ownership::HandoffRedisValue(ownerEpoch, nowMs);

	// 先占住代际再发命令:回调与看门狗都拿它判断"还是不是这一次传送"。
	travel->requestedAtMs = nowMs;

	const int ret = redis->command(
		[playerId, nowMs](hiredis::Hiredis *, redisReply *reply)
		{
			if (reply == nullptr || reply->type == REDIS_REPLY_ERROR)
			{
				LOG_ERROR << "[ZoneTravel] SET handoff mark failed for player " << playerId
						  << (reply != nullptr && reply->str != nullptr ? std::string(" err=") + reply->str : "");
				AbortTravelHandoff(playerId, "handoff mark write failed");
				return;
			}
			// 回调期间实体可能已被退出流程销毁或换了一代,RequestTravelEnterScene 自己按 id 回查。
			const auto entity = tlsEcs.GetPlayer(playerId);
			const auto *current = tlsEcs.actorRegistry.valid(entity)
									  ? tlsEcs.actorRegistry.try_get<PlayerTravelHandoffComp>(entity)
									  : nullptr;
			if (current == nullptr || current->requestedAtMs != nowMs)
			{
				LOG_INFO << "[ZoneTravel] handoff mark landed but travel intent is gone/replaced for player "
						 << playerId << "; not requesting EnterScene";
				return;
			}
			RequestTravelEnterScene(playerId);
		},
		"SET %s %s EX %d", key.c_str(), value.c_str(), player_ownership::kHandoffMarkTtlSec);
	if (ret != REDIS_OK)
	{
		AbortTravelHandoff(playerId, "redis command dispatch failed");
	}
}

void PlayerLifecycleSystem::RequestTravelEnterScene(Guid playerId)
{
	const auto playerEntity = tlsEcs.GetPlayer(playerId);
	if (!tlsEcs.actorRegistry.valid(playerEntity))
	{
		return;
	}
	const auto *travel = tlsEcs.actorRegistry.try_get<PlayerTravelHandoffComp>(playerEntity);
	if (travel == nullptr)
	{
		return;
	}

	// 传送中实体还活着,gate / session 直接从实体上取,不需要像疏散那样提前抄票据。
	const auto *session = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(playerEntity);
	if (session == nullptr || session->gate_session_id() == kInvalidSessionId)
	{
		AbortTravelHandoff(playerId, "no live gate session");
		return;
	}

	const auto smEntity = GetSceneManagerEntity(playerId);
	if (smEntity == entt::null)
	{
		AbortTravelHandoff(playerId, "no SceneManager node reachable");
		return;
	}
	auto &smRegistry = tlsNodeContextManager.GetRegistry(eNodeType::SceneManagerNodeService);

	::scene_manager::EnterSceneRequest req;
	req.set_player_id(playerId);
	// 跨 zone 传送 sceneId 恒为 0:目标 zone 的场景实例由它的 scene_manager 按 scene_conf_id /
	// 世界频道表挑,C++ 侧不复制一份选择规则(与 DispatchEmergencyRelocate 同一理由)。
	// 同 zone 换图照抄被 18 拒掉的那次请求:加入已有镜像 / 副本时 scene_id 非 0,丢了它玩家会被
	// 送进按 scene_conf_id 另挑的频道,而不是他要去的那个实例。
	req.set_scene_id(travel->sceneId);
	req.set_scene_conf_id(travel->sceneConfigId);
	req.set_session_id(session->gate_session_id());
	req.set_gate_id(std::to_string(GetGateNodeId(session->gate_session_id())));
	req.set_gate_instance_id(ResolveGateInstanceId(session->gate_session_id()));
	req.set_gate_zone_id(GetZoneId());
	// zone_id != gate_zone_id 就是 scene_manager 判定"跨 zone"的依据,它据此走 CZ-4 两道门
	// 并回 Redirect 票据(GateTokenPayload.player_id / target_zone_id)。
	// 同 zone 交接时两者相等,走普通落点:凭 handoff 标记过换手门,应答没有 Redirect。
	req.set_zone_id(travel->targetZoneId);
	// 刻意不设 request_id:理由同 DispatchEmergencyRelocate(60s SETNX 去重会吞掉玩家
	// 短时间内的第二次传送)。幂等由 requestedAtMs 代际 + scene_manager 的 handoff 比对保证。

	// 应答靠 EnterSceneResponse.player_id 回显对回玩家(见 HandleTravelEnterSceneReply)。
	scene_manager::SendSceneManagerEnterScene(smRegistry, smEntity, req);
	ArmTravelReplyWatchdog(playerId, travel->requestedAtMs);

	LOG_INFO << "[ZoneTravel] requested EnterScene for player " << playerId
			 << " target_zone=" << travel->targetZoneId
			 << " scene_id=" << travel->sceneId
			 << " scene_conf_id=" << travel->sceneConfigId
			 << " session=" << session->gate_session_id();
}

void PlayerLifecycleSystem::ArmTravelReplyWatchdog(Guid playerId, uint64_t requestedAtMs)
{
	auto *loop = muduo::net::EventLoop::getEventLoopOfCurrentThread();
	if (loop == nullptr)
	{
		// 单测 / 无 loop 的宿主:没有看门狗,只靠应答与"退出优先"收敛。生产节点必有 loop。
		LOG_WARN << "[ZoneTravel] no event loop on this thread; EnterScene reply watchdog not armed for player "
				 << playerId;
		return;
	}
	// 只捕获 id + 代际。不保存 TimerId、不取消:到期时代际不符即 no-op,比维护一张定时器表更简单。
	loop->runAfter(kTravelReplyBudgetSec, [playerId, requestedAtMs]()
	{
		const auto entity = tlsEcs.GetPlayer(playerId);
		if (!tlsEcs.actorRegistry.valid(entity))
		{
			return;
		}
		const auto *travel = tlsEcs.actorRegistry.try_get<PlayerTravelHandoffComp>(entity);
		if (travel == nullptr || travel->requestedAtMs != requestedAtMs)
		{
			return; // 已收到应答,或已是另一次传送
		}
		// 超时只说明应答没到,不说明没被放行:先核实,再决定解冻还是销毁。
		ResolveTravelOutcome(playerId, requestedAtMs, "EnterScene reply timed out");
	});
}

void PlayerLifecycleSystem::ResolveTravelOutcome(Guid playerId, uint64_t requestedAtMs, const char *reason,
												 bool replyWasSuccess)
{
	const auto playerEntity = tlsEcs.GetPlayer(playerId);
	if (!tlsEcs.actorRegistry.valid(playerEntity))
	{
		return;
	}
	const auto *travel = tlsEcs.actorRegistry.try_get<PlayerTravelHandoffComp>(playerEntity);
	if (travel == nullptr || travel->requestedAtMs != requestedAtMs)
	{
		return;
	}
	uint64_t cachedEpoch = 0;
	if (const auto *epochComp = tlsEcs.actorRegistry.try_get<PlayerOwnerEpochComp>(playerEntity))
	{
		cachedEpoch = epochComp->epoch;
	}

	auto &redis = tlsRedis.GetZoneRedis();
	if (!redis || !redis->connected())
	{
		// 判不清就不解冻。玩家保持冻结(他可以断线,退出优先),Redis 恢复后下一轮看门狗再判。
		LOG_ERROR << "[ZoneTravel] cannot verify travel outcome for player " << playerId << " (" << reason
				  << "): zone redis unavailable; keeping the player frozen and re-arming the watchdog";
		ArmTravelReplyWatchdog(playerId, requestedAtMs);
		return;
	}

	const std::string handoffKey = player_ownership::HandoffRedisKey(playerId);
	const std::string epochKey = player_ownership::OwnerEpochRedisKey(playerId);
	const std::string reasonText = reason;
	// 顺序就是语义:先撤回标记,再读 epoch。两条命令走同一条连接,Redis 按序执行。
	redis->command([](hiredis::Hiredis *, redisReply *) {}, "DEL %s", handoffKey.c_str());
	const int ret = redis->command(
		[playerId, requestedAtMs, cachedEpoch, reasonText, replyWasSuccess](hiredis::Hiredis *, redisReply *reply)
		{
			const auto entity = tlsEcs.GetPlayer(playerId);
			if (!tlsEcs.actorRegistry.valid(entity))
			{
				return;
			}
			const auto *current = tlsEcs.actorRegistry.try_get<PlayerTravelHandoffComp>(entity);
			if (current == nullptr || current->requestedAtMs != requestedAtMs)
			{
				return;
			}
			if (reply == nullptr || reply->type == REDIS_REPLY_ERROR)
			{
				LOG_ERROR << "[ZoneTravel] GET owner_epoch failed while verifying travel outcome for player "
						  << playerId << "; keeping the player frozen and re-arming the watchdog";
				ArmTravelReplyWatchdog(playerId, requestedAtMs);
				return;
			}
			// 缺键按 0(scene_manager 从未铸造);值由 INCR 产生,必为十进制整数。
			uint64_t redisEpoch = 0;
			if (reply->type == REDIS_REPLY_STRING && reply->str != nullptr)
			{
				redisEpoch = std::strtoull(reply->str, nullptr, 10);
			}
			if (redisEpoch == cachedEpoch)
			{
				// 归属没动。失败应答 / 超时 → 交接未成,解冻并回失败 tip;
				// 成功应答(同 zone)→ 重发后落回了本节点(同物理节点不铸造 epoch),换图由
				// PlayerEnterGameNode → EnterScene 就地完成,静默解冻,不发失败 tip。
				AbortTravelHandoff(playerId, reasonText.c_str(), /*notifyFailure=*/!replyWasSuccess);
				return;
			}
			if (replyWasSuccess)
			{
				// 同 zone 放行的正常收尾:目标节点已拿到新 epoch,本节点不再持有该玩家。
				LOG_INFO << "[ZoneTravel] same-zone handoff granted for player " << playerId
						 << ": owner_epoch " << cachedEpoch << " -> " << redisEpoch
						 << "; destroying source-side entity";
				DestroyDeposedPlayer(playerId, "scene_handoff_granted", /*routine=*/true);
				return;
			}
			LOG_WARN << "[ZoneTravel] travel for player " << playerId << " was granted although the reply was lost/failed ("
					 << reasonText << "): owner_epoch " << cachedEpoch << " -> " << redisEpoch
					 << "; destroying source-side entity";
			DestroyDeposedPlayer(playerId, "travel_granted_without_reply");
		},
		"GET %s", epochKey.c_str());
	if (ret != REDIS_OK)
	{
		LOG_ERROR << "[ZoneTravel] redis command dispatch failed while verifying travel outcome for player "
				  << playerId << "; keeping the player frozen and re-arming the watchdog";
		ArmTravelReplyWatchdog(playerId, requestedAtMs);
	}
}

void PlayerLifecycleSystem::HandleTravelEnterSceneReply(Guid playerId, const ::scene_manager::EnterSceneResponse &resp)
{
	const auto playerEntity = tlsEcs.GetPlayer(playerId);
	if (!tlsEcs.actorRegistry.valid(playerEntity))
	{
		LOG_INFO << "[ZoneTravel] EnterScene reply for player " << playerId
				 << " but entity is gone (exited during travel); ignoring";
		return;
	}
	const auto *travel = tlsEcs.actorRegistry.try_get<PlayerTravelHandoffComp>(playerEntity);

	// ── 第一段:没有交接在途 —— 这是一条普通 EnterScene 的应答 ──
	if (travel == nullptr)
	{
		const auto *pending = tlsEcs.actorRegistry.try_get<PlayerSceneChangeInFlightComp>(playerEntity);
		if (pending == nullptr)
		{
			// 疏散 / 队伍跟随等不挂在途组件的请求,或看门狗已先核实过的迟到应答。没有事可做。
			LOG_DEBUG << "[ZoneTravel] EnterScene reply for player " << playerId
					  << " without an in-flight handoff or scene change; ignoring"
					  << " error_code=" << resp.error_code() << " has_redirect=" << resp.has_redirect();
			return;
		}
		// 先抄后摘:StartTravelHandoff 也会摘这个组件,之后 pending 指针即失效。
		const PlayerSceneChangeInFlightComp target = *pending;
		tlsEcs.actorRegistry.remove<PlayerSceneChangeInFlightComp>(playerEntity);

		if (resp.error_code() == kSmErrHandoffPending)
		{
			// 目标场景在别的节点。18 的语义是"请先存盘并出示标记":按同一个目标起同 zone 交接,
			// 落盘、写标记之后由 RequestTravelEnterScene 重发。scene_manager 被拒时未改任何状态。
			LOG_INFO << "[ZoneTravel] EnterScene for player " << playerId
					 << " needs a cross-node handoff (scene_manager code=" << resp.error_code()
					 << "); starting same-zone handoff scene_id=" << target.sceneId
					 << " scene_conf_id=" << target.sceneConfigId;
			if (const uint32_t tip = StartTravelHandoff(playerEntity, GetZoneId(), target.sceneId, target.sceneConfigId);
				tip != kTravelAccepted)
			{
				// 起不了交接(刚进备战 / 刚断线 …):玩家留在原场景,告诉客户端这次换图没成。
				LOG_WARN << "[ZoneTravel] same-zone handoff not started for player " << playerId << " tip=" << tip;
				PlayerTipSystem::SendToPlayer(playerEntity, tip, {});
			}
			return;
		}
		if (resp.error_code() != 0)
		{
			// 普通换图失败。EnterSceneC2S 的同步应答早已返回"已受理",不补这条 tip 客户端会一直等
			// 一个永远不来的 EnterSceneS2C。
			PlayerTipSystem::SendToPlayer(playerEntity, kEnterSceneFailed, {});
		}
		return;
	}

	// ── 第二段:交接在途 ──
	if (travel->requestedAtMs == 0)
	{
		// 存盘还没落地、交接的 EnterScene 还没发:这只能是交接之前那条请求的迟到应答
		// (例如客户端连点,第一条的 18 已经起了交接,第二条的应答这时才到)。不动交接。
		LOG_INFO << "[ZoneTravel] late EnterScene reply for player " << playerId
				 << " while the handoff save is still in flight; ignoring"
				 << " error_code=" << resp.error_code();
		return;
	}
	const uint64_t requestedAtMs = travel->requestedAtMs;
	const bool sameZone = (travel->targetZoneId == GetZoneId());

	if (resp.error_code() != 0)
	{
		LOG_WARN << "[ZoneTravel] EnterScene rejected for player " << playerId
				 << " code=" << resp.error_code() << " msg=" << resp.error_message();
		ResolveTravelOutcome(playerId, requestedAtMs, "scene_manager rejected");
		return;
	}
	if (!resp.has_redirect())
	{
		if (sameZone)
		{
			// 同 zone 放行的正常形态:成功、没有票据。两种可能应答本身分不出来 ——
			//   a) 目标在别的节点:scene_manager 已铸造新 epoch、路由已发,本节点不再持有该玩家;
			//   b) 重发时 scene_manager 重新挑频道挑回了本节点:同物理节点不铸造,场景就地切换。
			// 交给 ResolveTravelOutcome 按 epoch 判。b) 也必须过它的 DEL(理由见头文件)。
			ResolveTravelOutcome(playerId, requestedAtMs, "same-zone placement", /*replyWasSuccess=*/true);
			return;
		}
		// 跨 zone 放行了却没有票据:协议异常。是否已推进 epoch 由 ResolveTravelOutcome 查清楚。
		LOG_ERROR << "[ZoneTravel] EnterScene reply for player " << playerId
				  << " has neither error nor redirect; verifying outcome";
		ResolveTravelOutcome(playerId, requestedAtMs, "reply without redirect");
		return;
	}

	// 跨 zone 放行:scene_manager 已 INCR owner_epoch 并把 location 指向目标 zone,客户端会经
	// Kafka RedirectToGateEvent → gate msg 124 → RedirectFlow 连到目标 zone。
	// 本节点从这一刻起不再持有该玩家,且手里的 epoch 已旧 —— 不能再存盘,只能销毁。
	LOG_INFO << "[ZoneTravel] handoff granted for player " << playerId
			 << " -> gate " << resp.redirect().target_gate_ip() << ":" << resp.redirect().target_gate_port()
			 << "; destroying source-side entity";
	DestroyDeposedPlayer(playerId, "travel_redirect", /*routine=*/true);
}

void PlayerLifecycleSystem::AbortTravelHandoff(Guid playerId, const char *reason, bool notifyFailure)
{
	const auto playerEntity = tlsEcs.GetPlayer(playerId);
	if (!tlsEcs.actorRegistry.valid(playerEntity))
	{
		return;
	}
	const auto *travel = tlsEcs.actorRegistry.try_get<PlayerTravelHandoffComp>(playerEntity);
	if (travel == nullptr)
	{
		return; // 已经没有交接意图(应答与看门狗只有一个能赢),幂等
	}
	// 先抄后摘:失败 tip 按"这是哪一种交接"选,摘掉组件就无从知道了。
	const bool crossZone = (travel->targetZoneId != GetZoneId());
	tlsEcs.actorRegistry.remove<PlayerTravelHandoffComp>(playerEntity);

	if (notifyFailure)
	{
		LOG_WARN << "[ZoneTravel] handoff aborted for player " << playerId << ": " << reason
				 << "; unfreezing and keeping player on this node";
	}
	else
	{
		LOG_INFO << "[ZoneTravel] handoff resolved in place for player " << playerId << ": " << reason
				 << "; ownership unchanged, unfreezing silently";
	}

	// 解冻:StartTravelHandoff 用 PlayerFrozenComp 冻结输入,交接没发生就还给玩家。
	tlsEcs.actorRegistry.remove<PlayerFrozenComp>(playerEntity);

	// best-effort 删 handoff 标记:标记的语义是"这一刻的状态已落盘、可以交接",玩家解冻后
	// 会继续产生新状态,留着它会让之后某次(比如断线重登)的跨节点 EnterScene 误以为
	// 盘上是最新的。删不掉也有 TTL 兜底(与 scene_manager 只比对不删的契约不冲突:
	// 这是源端撤回自己写的标记)。
	if (auto &redis = tlsRedis.GetZoneRedis(); redis && redis->connected())
	{
		const std::string key = player_ownership::HandoffRedisKey(playerId);
		redis->command([](hiredis::Hiredis *, redisReply *) {}, "DEL %s", key.c_str());
	}

	if (!notifyFailure)
	{
		return;
	}
	// 让客户端收起"传送中 / 切换中"遮罩。跨 zone 传送未成一律归"目标区繁忙"(含目标区不存在、
	// 无可用 gate、scene_manager 拒绝、应答超时):这些在 scene 侧分不清,也都是"稍后再试"。
	// 同 zone 换图未成沿用通用的进场失败码。
	// 玩家在交接期间退出的情形到不了这里:退出流程(FinishExitAfterPersist)会先作废交接并销毁实体。
	PlayerTipSystem::SendToPlayer(playerEntity,
								  crossZone ? static_cast<uint32_t>(kZoneTravelTargetBusy)
											: static_cast<uint32_t>(kEnterSceneFailed),
								  {});
}

bool PlayerLifecycleSystem::IsSaveInFlight(Guid playerId)
{
	// "In flight" == HandleExitGameNode has stamped the UnregisterPlayer marker
	// AND HandlePlayerAsyncSaved has not yet fired (which would either drop the
	// marker on a reconnect-superseded entity, or destroy the entity outright).
	//
	// Once the entity is destroyed, tlsEcs.GetPlayer(playerId) returns
	// entt::null and any_of<UnregisterPlayer> on null returns false — so the
	// "save complete" terminal state is naturally represented by "no entity".
	const auto playerEntity = tlsEcs.GetPlayer(playerId);
	if (!tlsEcs.actorRegistry.valid(playerEntity))
	{
		return false;
	}
	return tlsEcs.actorRegistry.any_of<UnregisterPlayer>(playerEntity);
}

// ─────────────────────────────────────────────────────────────────────
// 归属交接冻结态查询。
//
// PlayerFrozenComp 由 StartTravelHandoff 挂、由 AbortTravelHandoff / DestroyDeposedPlayer /
// "退出优先"分支摘。它最早属于 player_migrate 搬数据链(Kafka 搬 PlayerAllData + ACK + reaper),
// 那条链已按 cross-zone-scene-travel.md CZ-1 下线,只留下这个组件与业务侧的拦写闸。
// ─────────────────────────────────────────────────────────────────────

bool PlayerLifecycleSystem::IsCrossZoneFrozen(entt::entity player)
{
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return false;
	}
	return tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(player);
}
