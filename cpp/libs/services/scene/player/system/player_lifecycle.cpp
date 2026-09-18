#include "player_lifecycle.h"
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
#include "player/comp/last_persisted_snapshot_comp.h"
#include "player/comp/player_frozen_comp.h"
#include "player/comp/player_ownership_comp.h"
#include "player/system/cross_zone_reaper.h"
#include "player/system/dirty_save_stats.h"
#include "player/system/player_data_loader.h"
#include "engine/core/type_define/type_define.h"
#include "core/utils/encode/sha256.h"
#include "proto/common/event/player_migration_event.pb.h"
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
	HandleCrossZoneTransfer(playerEntity);

	// ── 跨 zone 传送交接(CZ-5):必须排在所有既有分支之前 ──────────────────────
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
		// 实体保留(冻结)直到 EnterScene 应答:Redirect → 销毁;错误 → 解冻。
		// 快照仍在函数末尾更新,传送失败后快路径判定才正确。
		BeginTravelHandoff(playerId);
	}
	// Cross-zone in flight — DO NOT destroy here.
	// HandleCrossZoneTransfer set PlayerFrozenComp on the entity above and
	// published `player_migrate` to Kafka. The entity must stay alive
	// (write-disabled via the Frozen check in business systems) until either:
	//   • the destination's `player_migrate_ack` arrives (ACK handler then
	//     removes PlayerFrozenComp and calls DestroyPlayer);
	//   • the reaper declares the migration failed (it then removes
	//     PlayerFrozenComp and unfreezes the entity so the player keeps
	//     playing on the source side).
	//
	// Before this change, the code fell straight through to DestroyPlayer
	// after Kafka send. If the broker dropped the message or the destination
	// node crashed, the player vanished on BOTH sides. See
	// cross-zone-readiness-audit.md §1 失败 B and §3.2 件 2.
	//
	// IMPORTANT: this gate must come BEFORE the UnregisterPlayer branch
	// below — HandleExitGameNode emplaces UnregisterPlayer on the entity
	// before SavePlayerToRedis runs, so a cross-zone path also carries that
	// tag. We want PlayerFrozenComp to win.
	else if (tlsEcs.actorRegistry.valid(playerEntity) &&
			 tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(playerEntity))
	{
		LOG_INFO << "HandlePlayerAsyncSaved: player " << playerId
				 << " is frozen for cross-zone migration; deferring destroy until ACK or reaper.";
		// Still update last-persisted snapshot below so the dirty-save fast
		// path stays correct if the migration fails and the player resumes.
	}
	else if (tlsEcs.actorRegistry.any_of<UnregisterPlayer>(playerEntity))
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
	// HandleExitGameNode(正常登出)那条路径已经在摘 SceneEntityComp 时一并
	// 摘了 ScenePlayers,但**跨 zone 迁移**这条路径没有:HandleCrossZoneTransfer
	// 只挂 PlayerFrozenComp、保留实体和 SceneEntityComp,随后 ACK 成功
	// (HandlePlayerMigrationAck)或目的端重建(HandlePlayerMigration 的
	// payload 变更分支)直接调 DestroyPlayer —— 于是源场景的 ScenePlayers
	// 里留下一个悬垂 entity id。DestroyEntity 只动 actorRegistry,而
	// ScenePlayers 在 sceneRegistry,没有任何 on_destroy 钩子会替它清。
	//
	// 后果与 HandleExitGameNode 注释里写的完全一样:entt 会复用实体 id,
	// 源场景残留的陈旧 id 过一阵子可能正好是另一个场景里某个活着的玩家,
	// 一旦源场景被 BeginSceneDrain 排空,就会给那个不相干的玩家错发改派票、
	// 把他从当前场景踢走。
	//
	// 放在这个"唯一销毁出口"里做,一次覆盖全部销毁路径:正常登出路径此时
	// SceneEntityComp 已被摘除,下面的 try_get 拿不到、自然跳过(idempotent);
	// 两条跨 zone 路径实体还带着 SceneEntityComp,正好在这里补上。
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

	// 跨 zone **传送**在途(PlayerTravelHandoffComp)与下面的 player_migrate 迁移不同:
	// 退出优先。交接只在"状态已落盘 + 输入已冻结"之后发起,盘上就是最新状态,
	// 本地实体没有任何目的地还要等的东西 —— 直接按普通退出销毁。
	// scene_manager 那边的应答随后到达时实体已不在,HandleTravelEnterSceneReply 幂等忽略;
	// 它若已放行,location 已指向目标 zone(node 为空),下次登录按 Offline-Return 规则处理。
	// 不在这里让路,下面的 PlayerFrozenComp 分支会把它当成等 ACK 的迁移永久挂起。
	if (tlsEcs.actorRegistry.valid(playerEntity) &&
		tlsEcs.actorRegistry.any_of<PlayerTravelHandoffComp>(playerEntity))
	{
		LOG_INFO << "FinishExitAfterPersist: player " << playerId
				 << " exited during zone travel; dropping travel intent (exit wins)";
		tlsEcs.actorRegistry.remove<PlayerTravelHandoffComp>(playerEntity);
		tlsEcs.actorRegistry.remove<PlayerFrozenComp>(playerEntity);
	}

	// 跨 zone 迁移在途的实体不能在这里销毁 —— 它要活到目的地 ACK 到达
	// (或者 reaper 判定迁移失败把它解冻)为止,否则玩家两边都没了。
	// 这条判定原本只写在 HandlePlayerAsyncSaved 里,而"存盘快路径跳过"那条
	// 收尾路径绕过了它;两条路径既然共用本函数,判定就必须放在这里,否则会漂移。
	if (tlsEcs.actorRegistry.valid(playerEntity) &&
		tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(playerEntity))
	{
		LOG_INFO << "FinishExitAfterPersist: player " << playerId
				 << " is frozen for cross-zone migration; deferring destroy until ACK or reaper.";
		return;
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
	// 票据清空 = 每个有会话的玩家都已存盘落地并派发过改派;
	// 实体清空 = 本地不再持有任何玩家状态。
	return tlsEmergencyRelocateTickets.empty() &&
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

void PlayerLifecycleSystem::HandleCrossZoneTransfer(entt::entity playerEntity)
{
	auto changeInfo = tlsEcs.actorRegistry.try_get<ChangeSceneInfoComp>(playerEntity);
	if (!changeInfo)
	{
		return;
	}

	if (!changeInfo->is_cross_zone())
	{
		return;
	}

	// 回合制战斗冻结拦截:战斗在途禁止跨 zone 迁移(设计文档 §5.3 冻结清单)。
	// InBattleComp 摘除前实体必须留在本节点接收结算事件;迁移意图直接作废,
	// 玩家结算落地(或 reaper 判废)后重新发起即可。
	if (tlsEcs.actorRegistry.any_of<InBattleComp>(playerEntity))
	{
		LOG_WARN << "[PlayerBattle] 跨 zone 迁移被拒: 玩家战斗在途, player_id="
				 << tlsEcs.actorRegistry.get<Guid>(playerEntity)
				 << " to_zone=" << changeInfo->to_zone_id();
		tlsEcs.actorRegistry.remove<ChangeSceneInfoComp>(playerEntity);
		return;
	}

	auto playerId = tlsEcs.actorRegistry.get<Guid>(playerEntity);

	PlayerAllData playerAllDataMessage;
	PlayerAllDataMessageFieldsMarshal(playerEntity, playerAllDataMessage);
	playerAllDataMessage.mutable_player_database_data()->set_player_id(playerId);
	playerAllDataMessage.mutable_player_database_1_data()->set_player_id(playerId);

	// ⚠️ KNOWN GAP (cross-zone-readiness-audit.md §1.3): PlayerAllData currently
	// only carries the 7 ECS components present in player_database. bag, quest,
	// and mail data are NOT in the proto and will silently vanish on every
	// cross-zone transition until the BagAllData / QuestAllData / MailAllData
	// sub-messages are added (audit doc §3.2 件 1, task #23).

	const auto toZoneId = changeInfo->to_zone_id();

	PlayerMigrationEvent request;
	request.set_player_id(playerId);
	request.set_from_zone(GetZoneId());
	request.set_to_zone(toZoneId);
	request.mutable_scene_info()->CopyFrom(*changeInfo);
	const std::string serializedPlayerData = playerAllDataMessage.SerializeAsString();
	// Stamp a SHA-256 of the payload bytes so the destination can
	// distinguish exact-duplicate Kafka redelivery (same hash → skip
	// re-Init, just re-ACK) from a reaper retry that carries a fresh
	// payload (different hash → process normally; latest-wins).
	// See cross-zone-readiness-audit.md §7 失败 D and the proto's
	// payload_sha256 doc comment.
	request.set_payload_sha256(Sha256::HashToBytes(serializedPlayerData));
	request.set_serialized_player_data(serializedPlayerData);

	// partition 参数**必须留空**(PARTITION_UA,按 key=playerId 哈希)。
	// 旧代码把 toZoneId 直接当 Kafka partition 号传:topic 自动创建时只有
	// 1 个 partition,produce(partition=zoneId≥1) 直接 ERR__UNKNOWN_PARTITION,
	// 消息根本发不出去 —— 跨 zone 迁移 100% 失败,玩家冻结到 reaper 判弃。
	// 就算运维手工建了多 partition,zone 路由也不该编码在 partition 上:
	// 订阅侧是 per-node 消费组、全 partition 消费,真正的目标过滤在
	// HandlePlayerMigration 的 to_zone/场景归属检查里。
	KafkaProducer::Instance().send("player_migrate", request.SerializeAsString(), std::to_string(playerId));

	LOG_INFO << "[CrossZone] Sent player transfer to zone " << toZoneId << ": " << playerId;

	PlayerTipSystem::SendToPlayer(playerEntity, kSceneTransferInProgress, {});

	// Freeze the source-side entity instead of immediately destroying it.
	// Before this change, `DestroyPlayer` ran in HandlePlayerAsyncSaved right
	// after the Kafka send — meaning a Kafka broker failure or destination-node
	// crash would lose the player on BOTH sides (source destroyed, destination
	// never received). See cross-zone-readiness-audit.md §1 失败 B.
	//
	// PlayerFrozenComp keeps the entity alive but write-disabled until either:
	//   • The destination publishes a `player_migrate_ack` (success path) —
	//     the ACK handler removes the component and calls DestroyPlayer.
	//   • The reaper declares the migration failed after retries (failure
	//     path) — the reaper removes the component and unfreezes the entity
	//     so the player keeps playing on the source side.
	//
	// All business systems (AOI / combat / currency / bag / etc.) must
	// gate on `actorRegistry.any_of<PlayerFrozenComp>(player)` to skip
	// writes. The audit doc §3.2 件 2 enumerates the systems that need
	// the gate (still being threaded through — task #26).
	auto& frozen = tlsEcs.actorRegistry.emplace_or_replace<PlayerFrozenComp>(playerEntity);
	frozen.frozenAtMs = TimeSystem::NowMillisecondsUTC();
	frozen.toZoneId = toZoneId;
	frozen.migrateAttempts = 1;

	// Persist a Redis migration record so the reaper can recover this
	// migration if the Kafka publish above is lost / destination crashes /
	// THIS scene node restarts before ACK arrives. attempt=1 because this
	// is the original publish; the reaper bumps attempt on republish.
	// See docs/design/cross-zone-readiness-audit.md §3.2 件 3.
	CrossZoneReaper::RecordMigrationStart(
		playerId, GetZoneId(), toZoneId, /*toNodeId=*/0u, /*attempt=*/1u);

	tlsEcs.actorRegistry.remove<ChangeSceneInfoComp>(playerEntity);
}

void PlayerLifecycleSystem::HandlePlayerMigration(const PlayerMigrationEvent &msg)
{
	// ── 目标过滤:必须最先做 ─────────────────────────────────────────────
	//
	// 订阅拓扑是 per-node-id 消费组(scene-cross-zone-{nodeId},见 scene/main.cpp),
	// 即 **每个 scene 节点都会收到 topic 里的每一条消息** —— 不分 zone、不分节点。
	// 这里若不过滤,一次跨 zone 迁移会让集群里**所有** scene 节点各建一份该玩家的
	// 实体、各自 SavePlayerToRedis、各自 ACK:玩家在 N 个节点同时"在线",
	// 每个幽灵节点的周期存盘还会持续用陈旧数据覆盖真实节点写入的 PlayerAllData
	// —— 表现为玩家进度反复回档。这是 CLAUDE.md 不变量 2(共享 topic 消息必须
	// 带目标标识并在消费侧过滤)在 player_migrate 上的落地。
	//
	// 第一级:zone 过滤。别的 zone 的迁移与本节点无关,静默跳过(DEBUG——
	// 这是共享 topic 扇出的正常现象,不是异常)。
	if (msg.to_zone() != GetZoneId())
	{
		LOG_DEBUG << "[CrossZone] HandlePlayerMigration: ignoring migration for zone "
				  << msg.to_zone() << " (we are zone " << GetZoneId() << "), player "
				  << msg.player_id();
		return;
	}

	// 第二级:节点归属。同 zone 内有多台 scene 节点时,理想判据是"目标场景在
	// 本节点"。但当前 PlayerMigrationEvent 里**没有可用的目标场景标识**:
	// scene_info.guid 全仓无赋值点(grep set_guid 仅 view.cpp 的另一消息),
	// 且它是 uint32,装不下 64 位 snowflake scene_id —— 这是跨 zone 能力
	// 未完成清单的一部分(cross-zone-readiness-audit.md;生产侧本就由
	// scene_manager 的 AllowUnsafeCrossNodeHandoff=false 在上游 fail-closed)。
	//
	// 因此这里的策略:guid 有值(未来发布侧修好后)→ 严格按场景归属认领;
	// guid==0(现状)→ 放行但 WARN。zone 内单 scene 节点的开发环境行为不变;
	// 多节点开发环境会有抢建风险,WARN 就是给那种局面留的证据。
	{
		const uint64_t targetSceneId = msg.scene_info().guid();
		if (targetSceneId != 0)
		{
			bool sceneIsLocal = false;
			for (const auto entity : tlsEcs.sceneRegistry.view<SceneInfoComp>())
			{
				if (tlsEcs.sceneRegistry.get<SceneInfoComp>(entity).scene_id() == targetSceneId)
				{
					sceneIsLocal = true;
					break;
				}
			}
			if (!sceneIsLocal)
			{
				LOG_DEBUG << "[CrossZone] HandlePlayerMigration: target scene " << targetSceneId
						  << " not on this node, ignoring (player " << msg.player_id() << ").";
				return;
			}
		}
		else
		{
			LOG_WARN << "[CrossZone] HandlePlayerMigration: no target-scene id in event for player "
					 << msg.player_id() << " — claiming by zone only. With multiple scene nodes "
					 << "in this zone every node will claim this player (ghost entities); "
					 << "the migration protocol needs a 64-bit target scene id before that topology.";
		}
	}

	// Idempotency guard — see cross-zone-failure-test-runbook.md §失败 D and
	// task #32. Kafka rebalance can redeliver player_migrate after we already
	// processed it (or the source's reaper republishes during a slow ACK
	// window). If the player entity is already present on this node, the
	// previous Init must have succeeded — we only need to re-emit the ACK so
	// the source's reaper / DestroyPlayer path completes. Re-running Init
	// would create a second entt entity (with the same player_id but a
	// different entity handle) and the player would "double-spawn" — items
	// flooded into bag, duplicate AOI entries, etc.
	//
	// Two-tier dedup:
	//   Tier 1 (structural): is the player entity already in our registry?
	//     Catches the common case (Kafka rebalance redelivery within the
	//     same node lifetime).
	//   Tier 2 (payload hash): does msg.payload_sha256 match what we
	//     stamped onto the per-player Redis dedup key on first receipt?
	//     Catches the failure-then-reaper-retry race where the source
	//     republishes a fresh payload (different state) — Tier 1 alone
	//     would say "already there, just ACK" but the new state is more
	//     recent and should be applied. Different hash → fresh migration.
	//   The payload hash is empty on legacy publishers (pre-this-field);
	//   in that case we degrade gracefully to Tier-1-only behavior.
	const auto existingPlayer = tlsEcs.GetPlayer(msg.player_id());
	if (existingPlayer != entt::null && tlsEcs.actorRegistry.valid(existingPlayer))
	{
		// Compare the incoming hash with the one we stamped on first receipt.
		// If they differ, this is a reaper retry with mutated payload — we
		// MUST tear down the stale entity and rebuild from the fresh data.
		// Without this, the player ends up on this node with state from the
		// initial publish even after the source recomputed and resent.
		const std::string &incomingHash = msg.payload_sha256();
		if (!incomingHash.empty())
		{
			// Compute what the existing-entity payload would hash to and
			// compare. We can't easily fish out the original bytes (they
			// went through InitPlayerFromAllData and live decomposed in
			// the registry), so we marshal back out and hash that.
			//
			// This is the conservative path: if our re-marshal disagrees
			// with the source's incoming hash, somebody mutated state
			// between Init and the redelivery. Treat as fresh migration.
			PlayerAllData rehash;
			PlayerAllDataMessageFieldsMarshal(existingPlayer, rehash);
			rehash.mutable_player_database_data()->set_player_id(msg.player_id());
			rehash.mutable_player_database_1_data()->set_player_id(msg.player_id());
			const std::string currentHash = Sha256::HashToBytes(rehash.SerializeAsString());
			if (currentHash != incomingHash)
			{
				LOG_WARN << "[CrossZone] HandlePlayerMigration: player " << msg.player_id()
						 << " already present but payload_sha256 differs (incoming hex="
						 << Sha256::HashToHex(incomingHash)
						 << ", local hex=" << Sha256::HashToHex(currentHash)
						 << "); treating as fresh migration (reaper retry with mutated state).";
				DestroyPlayer(msg.player_id());
				// Fall through to the normal Init path below.
			}
			else
			{
				LOG_INFO << "[CrossZone] HandlePlayerMigration: player " << msg.player_id()
						 << " already present with matching payload hash — exact-duplicate "
						 << "delivery; skipping Init, re-emitting ACK.";
				PlayerMigrationAckEvent ackEvent;
				ackEvent.set_player_id(msg.player_id());
				ackEvent.set_from_zone(msg.from_zone());
				ackEvent.set_to_zone(msg.to_zone());
				ackEvent.set_ack_at_ms(TimeSystem::NowMillisecondsUTC());

				std::string ackBytes;
				if (ackEvent.SerializeToString(&ackBytes))
				{
					KafkaProducer::Instance().send(
						"player_migrate_ack", ackBytes, std::to_string(msg.player_id()));
				}
				return;
			}
		}
		else
		{
			// Legacy publisher (no payload_sha256). Original Tier-1 behavior:
			// any duplicate-delivery is treated as exact-duplicate. This is
			// the documented degradation path; once all publishers carry the
			// hash this branch becomes dead code we can remove.
			LOG_INFO << "[CrossZone] HandlePlayerMigration: player " << msg.player_id()
					 << " from zone " << msg.from_zone() << " already present on this node "
					 << "(legacy duplicate, no payload_sha256); skipping Init, re-emitting ACK.";
			PlayerMigrationAckEvent ackEvent;
			ackEvent.set_player_id(msg.player_id());
			ackEvent.set_from_zone(msg.from_zone());
			ackEvent.set_to_zone(msg.to_zone());
			ackEvent.set_ack_at_ms(TimeSystem::NowMillisecondsUTC());

			std::string ackBytes;
			if (ackEvent.SerializeToString(&ackBytes))
			{
				KafkaProducer::Instance().send(
					"player_migrate_ack", ackBytes, std::to_string(msg.player_id()));
			}
			return;
		}
	}

	PlayerAllData playerAllDataMessage;
	if (!playerAllDataMessage.ParseFromString(msg.serialized_player_data()))
	{
		LOG_ERROR << "Parse failed for player migration data";
		return;
	}

	// 老路径没有路由事件,home_zone / owner_epoch 都拿不到(保持 0):存盘会 fail-closed
	// 落进程 zone 并计数 —— 与改动前"固定落进程 zone"行为一致,只是不再静默。
	// 该路径按 cross-zone-scene-travel.md CZ-1 在阶段 3 下线,这里不补 data_service 查询。
	PlayerEnterContext ctx;

	auto player = InitPlayerFromAllData(playerAllDataMessage, ctx);
	if (!tlsEcs.actorRegistry.valid(player))
	{
		// InitPlayerFromAllData rejected the payload (player_id=0 or other
		// validation failure). Do NOT publish an ACK — the source needs to
		// see the migration as failed and let the reaper retry / declare
		// it lost.
		LOG_ERROR << "[CrossZone] HandlePlayerMigration rejected player_id="
				  << msg.player_id() << " from zone " << msg.from_zone()
				  << "; no ACK will be sent.";
		return;
	}

	SavePlayerToRedis(player);

	// Publish `player_migrate_ack` so the source-side scene node can clear
	// its PlayerFrozenComp and DestroyPlayer. Without this ACK the source
	// entity sits frozen forever (or until the reaper times it out and
	// unfreezes it, leading to double-presence).
	//
	// Payload is a `PlayerMigrationAckEvent` proto (see
	// proto/common/event/player_migration_event.proto). We use protobuf
	// rather than JSON for consistency with every other Kafka payload in
	// this codebase — JSON parsing is an order of magnitude slower and
	// costs us schema versioning / type safety.
	//
	// Kafka partition_key is the same playerId used by `player_migrate`, so
	// ACK arrives on the same partition's downstream — preserving ordering
	// with any subsequent migrations of the same player.
	PlayerMigrationAckEvent ackEvent;
	ackEvent.set_player_id(msg.player_id());
	ackEvent.set_from_zone(msg.from_zone());
	ackEvent.set_to_zone(msg.to_zone());
	ackEvent.set_ack_at_ms(TimeSystem::NowMillisecondsUTC());

	std::string ackBytes;
	if (!ackEvent.SerializeToString(&ackBytes))
	{
		LOG_ERROR << "[CrossZone] Failed to serialize PlayerMigrationAckEvent for player "
				  << msg.player_id() << " — source-side reaper will eventually retry the migration.";
		return;
	}

	auto ackErr = KafkaProducer::Instance().send(
		"player_migrate_ack", ackBytes, std::to_string(msg.player_id()));
	if (ackErr != RdKafka::ERR_NO_ERROR)
	{
		LOG_ERROR << "[CrossZone] Failed to publish ACK for player " << msg.player_id()
				  << " from_zone=" << msg.from_zone() << " err=" << RdKafka::err2str(ackErr)
				  << " — source-side reaper will eventually retry the migration.";
	}
	else
	{
		LOG_INFO << "[CrossZone] Published ACK for player " << msg.player_id()
				 << " (from_zone=" << msg.from_zone() << ", to_zone=" << msg.to_zone() << ").";
	}
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
	// 先挂零值而不是等 EnterScene:HandlePlayerMigration 老路径建实体后直接 SavePlayerToRedis,
	// 存盘路径按"组件缺失 == 0"处理也行,但统一存在能让 try_get 分支少一种形态。
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
		LOG_WARN << "[SavePlayerToRedis] home_zone unknown for player " << playerId
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

void PlayerLifecycleSystem::HandlePlayerSaveRejected(Guid playerId, const std::string &redisKey)
{
	owner_epoch_stats::IncStaleOwnerWriteRejected();
	// 这条日志是"曾经出现过双主"的直接证据(cross-zone-scene-travel.md §6.3 要求压测期恒 0):
	// 本节点还拿着旧 epoch 在写,而 scene_manager 已把玩家改派出去。数据没有被污染
	// (Lua 原子拒绝),但本节点这份内存态从此作废。
	LOG_ERROR << "HandlePlayerSaveRejected: owner_epoch CAS rejected save for player " << playerId
			  << " key=" << redisKey
			  << " — this node has been deposed; dropping local state, no retry, no relocate"
			  << " (metric=stale_owner_write_rejected)";

	DestroyDeposedPlayer(playerId, "stale_owner_write_rejected");
}

void PlayerLifecycleSystem::DestroyDeposedPlayer(Guid playerId, const char *reasonTag)
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

	// 两种在途标记一并摘掉,否则 FinishExitAfterPersist / HandlePlayerAsyncSaved 会把它当成
	// 等 ACK 的迁移而延迟销毁。remove 对不存在的组件是 no-op。
	tlsEcs.actorRegistry.remove<PlayerTravelHandoffComp>(playerEntity);
	tlsEcs.actorRegistry.remove<PlayerFrozenComp>(playerEntity);

	// 与 HandleExitGameNode 同款,只是**没有存盘**:摘场景(AOI 停止广播)→ 摘会话 → 销毁。
	DetachFromScene(playerEntity);
	RemovePlayerSession(playerId);
	DestroyPlayer(playerId);

	LOG_WARN << "[" << reasonTag << "] local entity for player " << playerId
			 << " destroyed without persisting (ownership moved away)";
}

// ─────────────────────────────────────────────────────────────────────
// 跨 zone 传送:源端释放链(cross-zone-scene-travel.md CZ-5 / §4)
//
//   存盘落地 ──▶ BeginTravelHandoff:SET player:{id}:handoff "{epoch}:{now}" EX 300
//            ──▶ RequestTravelEnterScene:scene_manager.EnterScene(ZoneId=目标, SceneId=0)
//            ──▶ HandleTravelEnterSceneReply:Redirect → DestroyDeposedPlayer
//                                             错误 / 超时 → AbortTravelHandoff
//
// 所有异步回调只捕获 playerId(+ 代际),回调里按 id 回查实体:回调到达时实体可能已被
// 退出流程销毁、也可能已换了一次传送(AGENTS §11.7 精神)。
// ─────────────────────────────────────────────────────────────────────

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
	// scene_id 留 0:目标 zone 的场景实例由 scene_manager 按 scene_conf_id / 世界频道表挑,
	// C++ 侧不复制一份选择规则(与 DispatchEmergencyRelocate 同一理由)。
	req.set_scene_conf_id(travel->sceneConfigId);
	req.set_session_id(session->gate_session_id());
	req.set_gate_id(std::to_string(GetGateNodeId(session->gate_session_id())));
	req.set_gate_instance_id(ResolveGateInstanceId(session->gate_session_id()));
	req.set_gate_zone_id(GetZoneId());
	// zone_id != gate_zone_id 就是 scene_manager 判定"跨 zone"的依据,它据此走 CZ-4 两道门
	// 并回 Redirect 票据(GateTokenPayload.player_id / target_zone_id)。
	req.set_zone_id(travel->targetZoneId);
	// 刻意不设 request_id:理由同 DispatchEmergencyRelocate(60s SETNX 去重会吞掉玩家
	// 短时间内的第二次传送)。幂等由 requestedAtMs 代际 + scene_manager 的 handoff 比对保证。

	// 应答里没有 player_id,靠 metadata 回显定位(见 kTravelPlayerIdMetaKey 说明)。
	scene_manager::SendSceneManagerEnterScene(smRegistry, smEntity, req,
											  {kTravelPlayerIdMetaKey}, {std::to_string(playerId)});
	ArmTravelReplyWatchdog(playerId, travel->requestedAtMs);

	LOG_INFO << "[ZoneTravel] requested EnterScene for player " << playerId
			 << " target_zone=" << travel->targetZoneId
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
		AbortTravelHandoff(playerId, "EnterScene reply timed out");
	});
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
	if (!tlsEcs.actorRegistry.any_of<PlayerTravelHandoffComp>(playerEntity))
	{
		// 看门狗先判了超时并解冻,或这是上一次传送的迟到应答。玩家已在本节点继续玩,
		// 不能拿一条无主的应答去销毁他。若 scene_manager 其实已放行,本节点 epoch 已旧,
		// 下一次存盘会被 CAS 拒并走废黜路径 —— 结果仍然安全,只是多一条 stale 计数。
		LOG_WARN << "[ZoneTravel] EnterScene reply for player " << playerId
				 << " but no travel intent on entity (timed out / stale reply); ignoring"
				 << " error_code=" << resp.error_code() << " has_redirect=" << resp.has_redirect();
		return;
	}

	if (resp.error_code() != 0)
	{
		LOG_WARN << "[ZoneTravel] EnterScene rejected for player " << playerId
				 << " code=" << resp.error_code() << " msg=" << resp.error_message();
		AbortTravelHandoff(playerId, "scene_manager rejected");
		return;
	}
	if (!resp.has_redirect())
	{
		// 放行了却没有票据:协议异常。玩家留在本节点是唯一不丢人的选择;
		// 若 scene_manager 其实已推进 epoch,下一次存盘的 CAS 会把本节点正确废黜。
		LOG_ERROR << "[ZoneTravel] EnterScene reply for player " << playerId
				  << " has neither error nor redirect; treating as failure";
		AbortTravelHandoff(playerId, "reply without redirect");
		return;
	}

	// 放行:scene_manager 已 INCR owner_epoch 并把 location 指向目标 zone,客户端会经
	// Kafka RedirectToGateEvent → gate msg 124 → RedirectFlow 连到目标 zone。
	// 本节点从这一刻起不再持有该玩家,且手里的 epoch 已旧 —— 不能再存盘,只能销毁。
	LOG_INFO << "[ZoneTravel] handoff granted for player " << playerId
			 << " -> gate " << resp.redirect().target_gate_ip() << ":" << resp.redirect().target_gate_port()
			 << "; destroying source-side entity";
	DestroyDeposedPlayer(playerId, "travel_redirect");
}

void PlayerLifecycleSystem::AbortTravelHandoff(Guid playerId, const char *reason)
{
	const auto playerEntity = tlsEcs.GetPlayer(playerId);
	if (!tlsEcs.actorRegistry.valid(playerEntity))
	{
		return;
	}
	if (tlsEcs.actorRegistry.remove<PlayerTravelHandoffComp>(playerEntity) == 0)
	{
		return; // 已经没有传送意图(应答与看门狗只有一个能赢),幂等
	}
	LOG_WARN << "[ZoneTravel] travel aborted for player " << playerId << ": " << reason
			 << "; unfreezing and keeping player on this node";

	// 解冻:阶段 2 的 TravelToZone 用 PlayerFrozenComp 冻结输入,失败就还给玩家。
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

	// 阶段 2 会有专用 tip;现在复用通用的进场失败码让客户端收起"传送中"遮罩。
	// 玩家在传送期间退出的情形到不了这里:退出流程(FinishExitAfterPersist)会先作废传送并销毁实体。
	PlayerTipSystem::SendToPlayer(playerEntity, kEnterSceneFailed, {});
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
// Cross-zone migration ACK / Frozen state helpers
//
// See docs/design/cross-zone-readiness-audit.md §3.2 件 2-3 for the
// design rationale. Frozen state replaces the old "Kafka send → immediate
// DestroyPlayer" pattern that silently lost players on broker / dest
// failure.
// ─────────────────────────────────────────────────────────────────────

bool PlayerLifecycleSystem::IsCrossZoneFrozen(entt::entity player)
{
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return false;
	}
	return tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(player);
}

void PlayerLifecycleSystem::HandlePlayerMigrationAck(Guid playerId, uint32_t toZoneId)
{
	const auto playerEntity = tlsEcs.GetPlayer(playerId);
	if (!tlsEcs.actorRegistry.valid(playerEntity))
	{
		// Already destroyed (reaper / restart recovery beat us, or this is a
		// duplicate ACK from Kafka rebalance). Idempotent no-op.
		LOG_INFO << "[CrossZone] ACK for player " << playerId
				 << " (zone=" << toZoneId
				 << ") arrived but entity is gone — assuming already-handled, ignoring.";
		return;
	}

	const auto* frozen = tlsEcs.actorRegistry.try_get<PlayerFrozenComp>(playerEntity);
	if (frozen == nullptr)
	{
		// Entity exists but isn't frozen — something is off. Could be:
		//   • Player reconnected after a failed migration and is live again,
		//     and a stale ACK from the failed attempt arrived late.
		//   • Bug in the migration state machine.
		// Log and bail — refusing to destroy a live player on an unverified ACK.
		LOG_WARN << "[CrossZone] ACK for player " << playerId
				 << " (zone=" << toZoneId
				 << ") arrived but entity is NOT frozen — ignoring (possible stale ACK).";
		return;
	}

	if (frozen->toZoneId != 0 && frozen->toZoneId != toZoneId)
	{
		// ACK from the wrong zone — almost certainly a duplicate from a
		// previous failed migration attempt that the reaper already gave up on.
		// Don't destroy.
		LOG_WARN << "[CrossZone] ACK zone mismatch for player " << playerId
				 << " — frozen.toZoneId=" << frozen->toZoneId
				 << " ack.toZoneId=" << toZoneId << " — ignoring.";
		return;
	}

	LOG_INFO << "[CrossZone] ACK confirmed for player " << playerId
			 << " (zone=" << toZoneId << "); destroying source-side entity.";

	// Order matters:
	//   1. Remove Frozen so any business system that checks IsCrossZoneFrozen
	//      between here and DestroyPlayer doesn't see a stale freeze.
	//   2. Remove gate session — the destination already accepted the player,
	//      the client should have switched binding via gate routing.
	//   3. Destroy entity.
	//   4. Tell the reaper this migration is done so it stops watching the
	//      Redis player_migration:{playerId} key. (DEL is idempotent — if the
	//      reaper TTL beat us to it, this is a harmless no-op.)
	tlsEcs.actorRegistry.remove<PlayerFrozenComp>(playerEntity);
	RemovePlayerSession(playerId);
	DestroyPlayer(playerId);
	CrossZoneReaper::RecordMigrationDone(playerId);
}
