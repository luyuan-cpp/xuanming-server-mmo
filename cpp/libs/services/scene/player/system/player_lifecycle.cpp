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
#include "player/system/handoff_mark_withdraw.h"
#include "player/system/player_data_loader.h"
#include "engine/core/type_define/type_define.h"
#include "table/proto/tip/cross_server_error_tip.pb.h"
#include "node/system/grpc_call_deadline.h"      // 在途换图 TTL 从 SceneManager 的 gRPC deadline 派生
#include "player_tip.h"
#include "modules/scene/comp/scene_comp.h"
#include "modules/scene/comp/scene_node_comp.h"
#include <engine/infra/messaging/kafka/kafka_producer.h>
#include "core/system/redis.h"
#include "player_scene.h"
#include "player_team.h"
#include "hexagons_grid.h"  // Hex —— 退出场景时要和 SceneEntityComp 成对摘掉
#include "generated/attribute/actorbaseattributess2c_attribute_sync.h" // StopMotionForExit:速度脏位
#include "proto/common/component/actor_comp.pb.h"                     // Velocity / Acceleration
#include "proto/scene/player_state_attribute_sync.pb.h"                // ActorBaseAttributesS2C 字段号
#include "stress_test_probe.h"
#include "player/constants/player.h"
#include <node_config_manager.h> // SavePlayerToRedis:DBTask topic 世代号(BaseDeployConfig.db_task_topic_generation)
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
#include "table/code/world_table.h" // RequestZoneTravel:目标地图必须是 World 表登记的世界图
#include "proto/scene_manager/scene_manager_service.pb.h"
#include "proto/scene_manager/storage.pb.h" // storage::PlayerLocation:ResolveTravelOutcome 读 location 判要不要踢线
#include "grpc_client/scene_manager/scene_manager_service_grpc_client.h"
#include "muduo/net/EventLoop.h"
#include <algorithm>
#include <chrono>
#include <functional>
#include <iterator>
#include <limits>
#include <string>
#include <type_traits>
#include <unordered_map>
#include <vector>

bool player_ownership::ParsePlayerLocationElement(const redisReply *element, storage::PlayerLocation &out)
{
	if (element == nullptr || element->type != REDIS_REPLY_STRING)
	{
		return false;
	}
	if (element->len > static_cast<size_t>(std::numeric_limits<int>::max()))
	{
		return false;
	}
	return out.ParseFromArray(element->str, static_cast<int>(element->len));
}

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
// 已发出、应答未到的断线释放标记条件写数(A1′、A2′ 放弃补写,以及改派踢线前的凭证补写,exit_release_mark.h)。计入停机
// drain 谓词(main.cpp)与 IsEmergencyRelocateDrained(R6):节点在它归零之前 quit loop 的话回调再也不会来,计数与日志都丢。
thread_local std::size_t tlsExitReleaseMarksInFlight = 0;
// 疏散 / 排空改派的待确认表(契约见 relocate_confirm.h):每张被消费的票据一条,键 = player_id。
// tlsRelocateHandoffMarksInFlight 只覆盖"标记在写、EnterScene 还没发"这一段;发出之后改派有没有生效,由这张表盯到
// 有结论为止(已在别处 / 已落回本节点 / 已踢线 / 已放弃)。
// 线程模型:只在 scene 逻辑线程上读写 —— 登记(FinishExitAfterPersist → DispatchEmergencyRelocate)、写标记的 Redis
// 回调、EnterScene 的应答 / 传输失败(主循环上的定时器轮询 CompletionQueue 后分发)、进场路由(PlayerEnterGameNode)、
// 核实的 Redis 回调、RedisSystem 的重连回调与 1s 定时器,都跑在同一个 EventLoop 上,不需要锁。认领靠的 EnterScene
// 关联号也是线程内发号(tlsEnterSceneCorrelationSeq),两者作用域一致:号只在发出它的线程上有意义。
// 不随 tlsEcs.Clear() 清空;进程退出丢表无害(只是少踢,玩家自己重登)。
thread_local relocate_confirm::Table tlsRelocateConfirms;

namespace
{
	// 按会话把一条消息直接推给会话所在的 gate。用在没有玩家实体可用的两种场合:载入在途(SendTipToPendingSession),
	// 以及实体已销毁、手里只剩改派票据抄下的会话(SendTipAndKickToSession)。有实体时走 SendMessageToClientViaGate
	// 的实体重载,不要走这里。
	//   expectedGateInstanceId  发票时 gate 的实例 uuid。非空时与 gate 当前的 NodeInfo.node_uuid 比对,不等 = gate 进程
	//                           已换过一个、会话随旧进程消失,不推(kGateReplaced)。空 = 不比对(载入在途的提示;
	//                           发票时就没找到 gate、抄不到实例号的票据)。
	//   site                    失败原因的日志前缀。四种推不出去的原因各打一条 WARN,由本函数自己打。
	// playerId 是 gate 侧的身份栅栏(routing-identity-audit-20260908.md R13):会话号已被别的玩家占用时由 gate 在写
	// socket 之前拦下。这里没有玩家实体可取 Guid,调用方都拿得到 player_id —— 必须传下来,否则这条路径会成为整条
	// 推送链上唯一一个不带身份的口子。
	// 返回 kSent 只表示"已交给 RpcSession":底层发送没有返回值,不代表客户端收到。
	relocate_confirm::GatePushResult PushToSessionViaGate(SessionId sessionId, Guid playerId,
														  const std::string &expectedGateInstanceId, uint32_t messageId,
														  const google::protobuf::Message &message, const char *site)
	{
		// 不能写 `entt::entity{GetGateNodeId(sessionId)}` —— node_id 是业务编号,
		// 而 gate 实体槽位是 registry.create() 按发现顺序分配的(node_connector.cpp:118 /
		// registration_manager.cpp),两者早在 uuid 主键重构后就不再相等。
		// network_utils.h:27-30 明文禁止这种写法。单 gate 部署时 node_id 从 1 起、
		// 实体槽位从 0 起,valid() 必假,这条提示 100% 发不出去;多 gate 时更糟,
		// 会命中另一个 gate 的槽位、把提示发给没有这个会话的节点。
		// 本文件 EnqueueRelocateTicket 与 SendMessageToClientViaGate 都走 ResolveLocalZoneGateEntity,
		// 按会话直推的函数同样一律走它。
		const auto gateEntityOpt = ResolveLocalZoneGateEntity(sessionId);
		if (!gateEntityOpt)
		{
			LOG_WARN << site << ": gate not found for session " << sessionId;
			return relocate_confirm::GatePushResult::kGateGone;
		}
		auto &gateNodeRegistry = tlsNodeContextManager.GetRegistry(eNodeType::GateNodeService);
		auto *gateSessionPtr = gateNodeRegistry.try_get<RpcSession>(*gateEntityOpt);
		if (gateSessionPtr == nullptr)
		{
			LOG_WARN << site << ": RpcSession missing for session " << sessionId;
			return relocate_confirm::GatePushResult::kGateGone;
		}
		// 连接已断时 RpcSession 自己会丢弃并打 ERROR;在这里先判,调用方才分得清"交出去了"与"根本没发"。
		if (!gateSessionPtr->IsConnected())
		{
			LOG_WARN << site << ": gate not connected for session " << sessionId;
			return relocate_confirm::GatePushResult::kGateGone;
		}
		if (!expectedGateInstanceId.empty())
		{
			// 这里不调 ResolveGateInstanceId(定义在后面,而且会重复解析一次 gate 实体)。拿不到 NodeInfo 时证明不了
			// 还是同一个 gate 进程,按已换处理(fail-closed:宁可不推,不把踢线推给另一个进程上的同号会话)。
			const auto *gateNodeInfo = gateNodeRegistry.try_get<NodeInfo>(*gateEntityOpt);
			if (gateNodeInfo == nullptr || gateNodeInfo->node_uuid() != expectedGateInstanceId)
			{
				LOG_WARN << site << ": gate instance replaced for session " << sessionId;
				return relocate_confirm::GatePushResult::kGateReplaced;
			}
		}
		SendMessageToClientViaGate(messageId, message, *gateSessionPtr, sessionId, playerId);
		return relocate_confirm::GatePushResult::kSent;
	}

	// Sends a SendTipToClient message directly to the gate by session_id.
	// Used during the async-load window when the player entity does not yet
	// exist, so the entity-based PlayerTipSystem path is not available.
	// 尽力而为:推不出去(gate 不在 / 连接已断)只留 PushToSessionViaGate 的那条 WARN,调用方不看结果。
	void SendTipToPendingSession(SessionId sessionId, Guid playerId, uint32_t tipId)
	{
		if (sessionId == 0)
		{
			return;
		}
		TipInfoMessage tip;
		tip.set_id(tipId);
		PushToSessionViaGate(sessionId, playerId, /*expectedGateInstanceId=*/std::string(),
							 SceneClientPlayerCommonSendTipToClientMessageId, tip, "SendTipToPendingSession");
	}

	// 疏散 / 排空改派核实出"没生效"之后的客户端出口:实体早已销毁,只能按票据里抄下的会话直推 —— 先回失败 tip,
	// tip 交出去了才紧跟踢线 KickPlayer(34,reason.id = 同一个 tip),客户端断线回选服重登。
	// 与 SendTipAndKickToClient 是两条路、同一份契约(先 tip 后 34、reason 同码):那个按实体踢,对象是还活着的冻结实体;
	// 这个按会话踢,对象是"实体已不在、会话还挂着"的票据。待确认条目存在期间本节点没有该玩家实体(或条目在等载入),
	// 两条路不会对同一玩家同时生效。
	// 票据的会话号可以是 0(发票条件只排除了 kInvalidSessionId):没有有效会话就什么都不发,按 gate 不在计。
	// 这个判定放在这里而不是 PushToSessionViaGate 里:后者还服务 SendTipToPendingSession,那边对无效会话号的既有
	// 日志原文不能变。
	// 返回值是踢线那一条的结果:tip 没交出去时直接返回它的失败原因,不再发 34。
	relocate_confirm::GatePushResult SendTipAndKickToSession(SessionId sessionId, Guid playerId,
															 const std::string &expectedGateInstanceId, uint32_t tipId)
	{
		static constexpr const char *kSite = "[RelocateConfirm] push";
		if (!player_exit::IsBoundSession(sessionId))
		{
			LOG_WARN << kSite << ": ticket has no bound session for player " << playerId << " (session=" << sessionId
					 << ")";
			return relocate_confirm::GatePushResult::kGateGone;
		}
		TipInfoMessage tip;
		tip.set_id(tipId);
		const auto tipResult = PushToSessionViaGate(sessionId, playerId, expectedGateInstanceId,
													SceneClientPlayerCommonSendTipToClientMessageId, tip, kSite);
		if (tipResult != relocate_confirm::GatePushResult::kSent)
		{
			return tipResult;
		}
		GameKickPlayerRequest kick;
		kick.mutable_reason()->set_id(tipId);
		return PushToSessionViaGate(sessionId, playerId, expectedGateInstanceId,
									SceneClientPlayerCommonKickPlayerMessageId, kick, kSite);
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

	// 交接的两道看门狗预算(存盘 30s / EnterScene 应答 30s)、冻结硬上限(70s)与晚发窗口(35s)统一定义在
	// travel_freeze_cap.h(kSaveBudget / kReplyBudget / kFreezeCap / kDispatchWindow),应答预算的下限约束也写在那里:
	// 几个常量之间有 static_assert 守着的时序关系,拆开放会让改一个忘改另一个。

	// 交接组件上的单调冻结起点必须与 travel_freeze_cap 用同一只时钟:组件头为了不依赖系统头直接写了
	// std::chrono::steady_clock,两边若走散,ElapsedSinceFreeze 的减法会编译不过或静默换算错。
	static_assert(std::is_same_v<decltype(PlayerTravelHandoffComp::frozenAtSteady), travel_freeze_cap::Clock::time_point>,
				  "PlayerTravelHandoffComp::frozenAtSteady 必须是 travel_freeze_cap::Clock::time_point");

	// PlayerSceneChangeInFlightComp 的有效期 = SceneManager 的 gRPC deadline + 1s,运行期取值
	// (docs/design/grpc-client-deadline-failure-callback.md §4.2「上游比下游宽」)。
	// 生成的 gRPC 客户端保证每次调用在 deadline 内以应答或失败收场,两者都会摘掉在途组件
	// (DispatchEnterSceneReply / DispatchEnterSceneTransportFailure);TTL 只兜"完成通知永远不来"
	// (例如 SceneManager 节点在调用途中被摘除),不能把玩家永久挡在换图之外。
	// 必须比 deadline 宽:否则应答 / 失败通知到达之前槽位已过期、被下一次登记覆盖,号就对不上了。
	// 超过它才到的迟到应答按 correlation_id 对号:号与下一条请求记下的不同,直接丢弃。旧版 scene_manager
	// 不回显号时退回按 player_id 对应答,这一残余仍在,后果只是一次换图失败或多余的交接,不会双主 ——
	// 去留始终由 owner_epoch 比对裁决。
	constexpr uint64_t kSceneChangeInFlightMarginMs = 1000;

	uint64_t SceneChangeInFlightTtlMs()
	{
		return static_cast<uint64_t>(grpc_call_deadline::Get(eNodeType::SceneManagerNodeService).count()) +
			   kSceneChangeInFlightMarginMs;
	}

	// [TravelHandoff] 汇总行的周期,与 RedisSystem 的 [DirtySave] / [OwnerEpoch] 同为 30s。
	constexpr double kTravelHandoffStatsIntervalSec = 30.0;

	// 一次交接走到终态(放行 / 就地解决 / 未成 / 标记已发出而销毁)时调用:终态计数 +1,并累计这次交接的冻结时长。
	// 必须在摘 PlayerFrozenComp **之前**调 —— 冻结起点只记在那个组件上。
	// 时钟回拨(now < frozenAtMs)按 0 计,不让一个负数把累计值冲成天文数字。
	void ObserveTravelHandoffEnded(entt::entity player, std::atomic<uint64_t> &outcomeCounter)
	{
		travel_handoff_stats::Inc(outcomeCounter);
		if (!tlsEcs.actorRegistry.valid(player))
		{
			return;
		}
		const auto *frozen = tlsEcs.actorRegistry.try_get<PlayerFrozenComp>(player);
		if (frozen == nullptr)
		{
			return;
		}
		const int64_t nowMs = static_cast<int64_t>(TimeSystem::NowMillisecondsUTC());
		travel_handoff_stats::ObserveFrozenMs(nowMs > frozen->frozenAtMs
												  ? static_cast<uint64_t>(nowMs - frozen->frozenAtMs)
												  : uint64_t{0});
	}

	// 受理后未成、又没法在原地恢复(epoch 已被推进、玩家已不在本节点)时的客户端出口:先回失败 tip,
	// 紧跟踢线 KickPlayer(34,reason.id = 同一个 tip),客户端断线回选服重登。
	// 两条消息走同一条 scene→gate 连接(同一个 RpcSession),实际按发送顺序到达;player_message_utils.h
	// 声明 scene→玩家的推送不保证顺序,颠倒时客户端只是少显示一条 tip,踢线本身不受影响。
	// 必须在 DestroyDeposedPlayer **之前**调:它会摘会话(RemovePlayerSession),之后两条都发不出去。
	void SendTipAndKickToClient(entt::entity player, uint32_t tipId)
	{
		PlayerTipSystem::SendToPlayer(player, tipId, {});
		GameKickPlayerRequest kick;
		kick.mutable_reason()->set_id(tipId);
		SendMessageToClientViaGate(SceneClientPlayerCommonKickPlayerMessageId, kick, player);
	}

	// 在当前线程的 EventLoop 上挂一个 30s 周期定时器,把 travel_handoff_stats 打成一行 [TravelHandoff]。
	// 挂的时机有两种,哪个先到算哪个:第一次发起交接(StartTravelHandoff),或第一次给 reply_uncorrelated /
	// reply_unmatched 计数(DispatchEnterSceneReply)。前缀与 key=value 布局是给 stress_summarize.ps1 之类的
	// 脚本解析用的,改格式要同步解析方(目前还没有解析方)。
	//
	// 为什么挂在这里而不是 RedisSystem 那个 30s 快照定时器里:这些计数只属于本文件的交接链,
	// 既没交接过、也没收到过异常 EnterScene 应答的进程(dev 旁路、单测宿主)不需要这个定时器,也不该多一行日志。
	// 从没交接过的节点(例如只跑普通换图的单节点部署)一旦出现那两项计数也要挂上:reply_uncorrelated 非 0
	// 说明 scene_manager 没升级(没回显 correlation_id)或有发送点绕过了统一出口,reply_unmatched 非 0 说明有
	// 外来 / 迟到应答被丢弃 —— 这两件事与本节点是否发起过交接无关,不打出来运维就看不见。
	// 回调不捕获任何对象、只读进程级原子计数,loop 析构时定时器随之销毁,不需要保存 TimerId 去取消
	// (AGENTS §11.7 管的是"绑了对象的回调",这里没有对象)。
	// 有变化才打:值都是累计值,长时间没有交接时不重复刷同一行。
	void EnsureTravelHandoffStatsTimer()
	{
		static thread_local bool armed = false;
		if (armed)
		{
			return;
		}
		auto *loop = muduo::net::EventLoop::getEventLoopOfCurrentThread();
		if (loop == nullptr)
		{
			return; // 单测 / 无 loop 的宿主:不打汇总行,计数本身照常累加
		}
		armed = true;
		loop->runEvery(kTravelHandoffStatsIntervalSec, [lastLogged = travel_handoff_stats::Snapshot{}]() mutable
		{
			const auto stats = travel_handoff_stats::Read();
			if (stats == lastLogged)
			{
				return;
			}
			lastLogged = stats;
			LOG_INFO << "[TravelHandoff] started=" << stats.started
					 << " started_cross_zone=" << stats.startedCrossZone
					 << " granted=" << stats.granted
					 << " granted_without_reply=" << stats.grantedWithoutReply
					 << " resolved_in_place=" << stats.resolvedInPlace
					 << " aborted=" << stats.aborted
					 << " exit_wins=" << stats.exitWins
					 << " save_watchdog_fired=" << stats.saveWatchdogFired
					 << " reply_watchdog_fired=" << stats.replyWatchdogFired
					 << " verify_rearmed=" << stats.verifyRearmed
					 << " frozen_ms_total=" << stats.frozenMsTotal
					 << " frozen_ms_max=" << stats.frozenMsMax
					 << " withdraw_deferred=" << stats.withdrawDeferred
					 << " withdraw_expired=" << stats.withdrawExpired
					 << " granted_client_reset=" << stats.grantedClientReset
					 << " reply_uncorrelated=" << stats.replyUncorrelated
					 << " reply_unmatched=" << stats.replyUnmatched
					 << " freeze_cap_reached=" << stats.freezeCapReached
					 << " dispatch_window_closed=" << stats.dispatchWindowClosed
					 << " mark_sent_destroyed=" << stats.markSentDestroyed
					 << " mark_sent_client_reset=" << stats.markSentClientReset
					 << " destroy_deferred_unsettled_save=" << stats.destroyDeferredUnsettledSave
					 << " freeze_unstamped=" << stats.freezeUnstamped
					 << " watchdog_early_fire=" << stats.watchdogEarlyFire
					 << " handoff_fastpath_forced=" << stats.handoffFastpathForced
					 << " rolled_back_adopted=" << stats.rolledBackAdopted
					 << " returned_after_grant=" << stats.returnedAfterGrant
					 << " rollback_receipt_anomaly=" << stats.rollbackReceiptAnomaly;
		});
	}

	// 在 muduo 定时器上挂一个"按单调截止时刻 dueAt 执行 fn"的一次性任务。当前线程没有 EventLoop(单测宿主)时
	// 什么都不挂、返回 false,调用方自己决定怎么记录。
	//
	// 为什么要复核:muduo 的定时器按 Timestamp::now 到期,Windows 上那是 gettimeofday → system_clock(墙钟),
	// 与 steady_clock 是两只独立的时钟。墙钟向前跳(对时 / 人工改时间)会让 runAfter 提前醒来,看门狗提前裁决 ——
	// 应答看门狗落进 scene_manager "铸造 → 路由 / 回滚"窗口就会读到一个即将被回滚的 epoch(见 kReplyBudget 的下限约束)。
	// 所以醒来后按 steady_clock 再看一眼:离 dueAt 还差超过 travel_freeze_cap::kEarlyFireTolerance(1s)就按剩余时间
	// 重挂并计 watchdog_early_fire;差 1s 以内(含两只时钟的频率差与微秒截断造成的毫秒级伪提前)直接执行,
	// 正常运行不会抬高这个计数。每次重挂的剩余时间严格变小,次数有界。
	// 墙钟**回拨**会把定时器推迟,这里管不了(引擎级残余,见 travel_freeze_cap.h 文件头)。
	// 回调只按值捕获截止时刻与 fn,不绑任何对象(AGENTS §11.7);fn 自己也只能按值捕获 id 与代际。
	bool RunAtMonotonic(travel_freeze_cap::Clock::time_point dueAt, std::function<void()> fn)
	{
		auto *loop = muduo::net::EventLoop::getEventLoopOfCurrentThread();
		if (loop == nullptr)
		{
			return false;
		}
		const auto remaining = travel_freeze_cap::RemainingUntil(dueAt, travel_freeze_cap::Clock::now());
		loop->runAfter(std::chrono::duration<double>(remaining).count(), [dueAt, fn = std::move(fn)]()
		{
			const auto now = travel_freeze_cap::Clock::now();
			if (travel_freeze_cap::ShouldRearmEarlyFire(dueAt, now))
			{
				travel_handoff_stats::Inc(travel_handoff_stats::Get().watchdogEarlyFire);
				LOG_WARN << "[ZoneTravel] handoff watchdog fired "
						 << std::chrono::duration_cast<std::chrono::milliseconds>(
								travel_freeze_cap::RemainingUntil(dueAt, now)).count()
						 << "ms before its monotonic deadline (wall clock stepped forward?); re-arming"
						 << " (metric=watchdog_early_fire)";
				// 回调跑在 loop 线程上,当前线程必有 loop,重挂不会失败。
				RunAtMonotonic(dueAt, fn);
				return;
			}
			fn();
		});
		return true;
	}

	// 冻结硬上限的 1s 扫描(PlayerLifecycleSystem::EnforceTravelFreezeCaps)。写法同 EnsureTravelHandoffStatsTimer:
	// 第一次发起交接时在当前线程的 EventLoop 上挂一次(thread_local armed 防重复),没有 loop(单测宿主)直接返回;
	// 回调不捕获任何对象、只按 id 回查实体,loop 析构时定时器随之销毁,不需要保存 TimerId 去取消。
	// 为什么自挂、不借 RedisSystem 的 1s 节拍:那条节拍上的"不分家"(cross-zone-scene-travel.md §12.6.10 第 16 条)
	// 针对的是 Redis 命令的重试(撤回、A2′)—— 它们要与重连回调成对驱动、排在加载重试之前。本扫描是生命周期截止时刻:
	// 不发 Redis 命令、不需要重连回调,也不该随 RedisSystem::BeginShutdown / Shutdown 一起被取消(Redis 不可用恰恰
	// 是上限要兜的情形)。平时交接实体为 0 到几个,空扫描只是一次空 view。
	void EnsureTravelFreezeCapTimer()
	{
		static thread_local bool armed = false;
		if (armed)
		{
			return;
		}
		auto *loop = muduo::net::EventLoop::getEventLoopOfCurrentThread();
		if (loop == nullptr)
		{
			return; // 单测 / 无 loop 的宿主:单测直接调 EnforceTravelFreezeCaps 并注入时间
		}
		armed = true;
		loop->runEvery(std::chrono::duration<double>(travel_freeze_cap::kSweepInterval).count(), []()
		{
			PlayerLifecycleSystem::EnforceTravelFreezeCaps(travel_freeze_cap::Clock::now());
		});
	}

	// ── EnterScene 关联号(EnterSceneRequest.correlation_id,契约见 player_lifecycle.h 的 enter_scene_reply)──
	//
	// 线程内计数器。应答只回到发出它的那个进程、那条线程的完成队列(生成的 gRPC 客户端在同一线程上发送与回调),
	// 所以线程内唯一就够了;进程重启后从 1 重来无害 —— 旧进程发出的请求,其应答回不到新进程。
	// 号只在进程内有意义,排障时要与 player_id 一起看。
	thread_local uint64_t tlsEnterSceneCorrelationSeq = 0;

	// 本线程是否已经为"scene_manager 没回显 correlation_id"打过 WARN。滚动升级窗口里每条应答都会这样,
	// 只打一次,趋势看 reply_uncorrelated。
	thread_local bool tlsWarnedUncorrelatedReply = false;

	// 取下一个关联号:前置自增,从 1 开始,恒非 0(uint64 在进程寿命内不会回绕)。
	uint64_t NextEnterSceneCorrelationId()
	{
		return ++tlsEnterSceneCorrelationSeq;
	}

	// scene 进程内 EnterScene 的**唯一**发送出口:全仓(cpp/generated 之外)只有这里直接调生成的
	// EnterScene 发送函数(cpp/generated/grpc_client/scene_manager)。由它保证每条请求都带非 0 关联号,DispatchEnterSceneReply 才能把
	// "应答号为 0"读成"scene_manager 是旧版、没回显"。有等待者的发送走 PlayerLifecycleSystem::RequestSceneChange
	// (或本文件的交接 RequestTravelEnterScene);疏散 / 排空的等待者是待确认表(SendEmergencyRelocateEnterScene 先
	// Table::MarkSent 把号记到条目上、再调本函数;表满没被跟踪的那一次没有等待者)。绕过它直调生成函数的发送点,
	// 在新版 scene_manager 下应答号为 0,会退回按 player_id 对应答,并让 reply_uncorrelated 非 0。
	// 前置条件:correlationId ≠ 0(调用方用 NextEnterSceneCorrelationId 取号、先记到等待者上再调本函数)、
	// smEntity 非空(调用方已查 GetSceneManagerEntity)。
	void SendCorrelatedEnterScene(entt::entity smEntity, ::scene_manager::EnterSceneRequest &req, uint64_t correlationId)
	{
		if (correlationId == 0)
		{
			// 违反前置条件是调用方的 bug。宁可换一个新号也不发 0:发 0 等于自称"旧版 SM 没回显",
			// 应答会被按 player_id 配给别的等待者。
			correlationId = NextEnterSceneCorrelationId();
			LOG_ERROR << "[EnterSceneReply] SendCorrelatedEnterScene called with correlation_id 0 for player "
					  << req.player_id() << "; sending with corr=" << correlationId << " instead";
		}
		req.set_correlation_id(correlationId);
		auto &smRegistry = tlsNodeContextManager.GetRegistry(eNodeType::SceneManagerNodeService);
		scene_manager::SendSceneManagerEnterScene(smRegistry, smEntity, req);
	}

	// [ExitPersist]、[ExitRelease] 与 [RelocateConfirm] 三行汇总(计数含义见 player_lifecycle.h 的 exit_persist_stats /
	// exit_release_stats / relocate_confirm_stats;改派待确认表的每一项计数都发生在某次退出之后,所以借退出链的这个
	// 定时器就够了)。与上面的 [TravelHandoff] 同款:第一次有玩家退出(或第一次发起 A2′)时在当前线程的
	// EventLoop 上挂 30s 定时器,回调不捕获任何对象、只读进程级原子计数(AGENTS §11.7 管的是绑了对象的回调,
	// 这里没有对象),各自有变化才打。挂在本文件而不是 RedisSystem 的快照定时器里:计数只属于本文件的退出链,
	// 改动也只落在退出链自己的文件里。
	constexpr double kExitPersistStatsIntervalSec = 30.0;

	// [ExitRelease] 行:key=value 平铺,key 取自 exit_release_mark 的两张名表与固定项。
	std::string FormatExitReleaseStats(const exit_release_stats::Snapshot &stats)
	{
		std::string line = "[ExitRelease]";
		const auto append = [&line](const char *key, uint64_t value)
		{
			line += ' ';
			line += key;
			line += '=';
			line += std::to_string(value);
		};
		for (std::size_t i = 0; i < stats.decisions.size(); ++i)
		{
			append(exit_release_mark::kExitReleaseDecisionNames[i], stats.decisions[i]);
		}
		append("written", stats.written);
		append("epoch_moved", stats.epochMoved);
		append("failed", stats.failed);
		for (std::size_t i = 0; i < stats.inheritResults.size(); ++i)
		{
			append(exit_release_mark::kInheritClearResultNames[i], stats.inheritResults[i]);
		}
		append("inherit_failed", stats.inheritFailed);
		append("inherit_refused", stats.inheritRefused);
		append("inherit_rewritten", stats.inheritRewritten);
		append("inherit_rewrite_skipped", stats.inheritRewriteSkipped);
		append("inherit_rewrite_failed", stats.inheritRewriteFailed);
		append("inherit_rewrite_dropped_node_holds", stats.inheritRewriteDroppedNodeHolds);
		append("inherit_rewrite_handed_over", stats.inheritRewriteHandedOver);
		return line;
	}

	// [RelocateConfirm] 行:key=value 平铺。数组项的 key 取自 relocate_confirm 的名表(push / credential 两组加前缀,
	// 免得与 outcomes 里的同名项混淆),其余是固定项。顺序与原文是给 runbook 按原文 grep 的,改了要同步 runbook。
	std::string FormatRelocateConfirmStats(const relocate_confirm_stats::Snapshot &stats)
	{
		std::string line = "[RelocateConfirm]";
		const auto append = [&line](const std::string &key, uint64_t value)
		{
			line += ' ';
			line += key;
			line += '=';
			line += std::to_string(value);
		};
		for (std::size_t i = 0; i < stats.ticketDecisions.size(); ++i)
		{
			append(relocate_confirm::kTicketDecisionNames[i], stats.ticketDecisions[i]);
		}
		append("untracked_overflow", stats.untrackedOverflow);
		append("cancelled_on_reentry", stats.cancelledOnReentry);
		append("ticket_dropped_deposed", stats.ticketsDroppedDeposed);
		append("reply_verified", stats.replyVerified);
		append("reply_ignored", stats.replyIgnored);
		append("transport_failed", stats.transportFailed);
		append("not_sent", stats.notSent);
		append("mark_write_timeout", stats.markWriteTimeout);
		append("reply_timeout", stats.replyTimeout);
		append("verify_deferred", stats.verifyDeferred);
		append("settle_waits", stats.settleWaits);
		for (std::size_t i = 0; i < stats.outcomes.size(); ++i)
		{
			append(relocate_confirm::kOutcomeNames[i], stats.outcomes[i]);
		}
		for (std::size_t i = 0; i < stats.push.size(); ++i)
		{
			append(std::string("push_") + relocate_confirm::kGatePushResultNames[i], stats.push[i]);
		}
		for (std::size_t i = 0; i < stats.credentialActions.size(); ++i)
		{
			append(std::string("credential_") + relocate_confirm::kCredentialActionNames[i], stats.credentialActions[i]);
		}
		append("credential_written", stats.credentialWritten);
		append("credential_epoch_moved", stats.credentialEpochMoved);
		append("credential_failed", stats.credentialFailed);
		return line;
	}

	void EnsureExitStatsTimer()
	{
		static thread_local bool armed = false;
		if (armed)
		{
			return;
		}
		auto *loop = muduo::net::EventLoop::getEventLoopOfCurrentThread();
		if (loop == nullptr)
		{
			return; // 单测 / 无 loop 的宿主:不打汇总行,计数本身照常累加
		}
		armed = true;
		loop->runEvery(kExitPersistStatsIntervalSec,
					   [lastLogged = exit_persist_stats::Snapshot{}, lastRelease = exit_release_stats::Snapshot{},
						lastRelocate = relocate_confirm_stats::Snapshot{}]() mutable
		{
			const auto release = exit_release_stats::Read();
			if (!(release == lastRelease))
			{
				lastRelease = release;
				LOG_INFO << FormatExitReleaseStats(release);
			}
			// [RelocateConfirm] 必须排在下面 [ExitPersist] 的"无变化就 return"之前:三行各看各的变化。
			const auto relocate = relocate_confirm_stats::Read();
			if (!(relocate == lastRelocate))
			{
				lastRelocate = relocate;
				LOG_INFO << FormatRelocateConfirmStats(relocate);
			}
			const auto stats = exit_persist_stats::Read();
			if (stats == lastLogged)
			{
				return;
			}
			lastLogged = stats;
			LOG_INFO << "[ExitPersist] exit_superseded=" << stats.superseded
					 << " exit_intent_missing=" << stats.intentMissing
					 << " exit_resave=" << stats.resave
					 << " exit_resave_capped=" << stats.resaveExhausted
					 << " exit_exhausted_rekick=" << stats.exhaustedRekick
					 << " exit_fastpath_deferred=" << stats.fastpathDeferred
					 << " exit_client_msg_rejected=" << stats.clientMsgRejected
					 << " exit_deposed_on_reentry=" << stats.deposedOnReentry;
		});
	}

	// handoff 标记的待撤回表(契约见 handoff_mark_withdraw.h)。只在 scene 逻辑线程上读写:
	// 登记(WithdrawHandoffMark)、Redis 回调销账、RedisSystem 的重连回调与 1s 定时器重试,
	// 都跑在同一个 EventLoop 上,不需要锁。
	thread_local handoff_mark_withdraw::Queue tlsHandoffWithdrawQueue;

	// 发一次条件撤回。条目此刻必须已在 tlsHandoffWithdrawQueue 里:本函数只负责"发",销账只在拿到
	// 整数应答的回调里做,其余任何情形(没连上 / 命令没发出去 / 空应答 / ERROR 应答)条目都原样留在
	// 表里等下一轮重试。
	//
	// 不能只判 connected():hiredis 在连接断开时用空 reply 逐个回调挂起命令,那段时间 REDIS_CONNECTED
	// 标志还没清、connected() 仍为真,而 redisvAsyncCommand 已因 DISCONNECTING / FREEING 返回 ERR ——
	// 退出流程恰恰可能就跑在那样一个回调里。所以 command() 的返回值必须一并检查。
	//
	// withdraw_deferred 是趋势类计数,不是"未撤回的条数":同一条目可能先发送失败、重试后应答又失败,
	// 各计一次。回调只按值捕获 id、标记原文与"是否首发",不绑对象、不持连接(AGENTS §11.7)。
	//
	// 日志分级:Redis 不通时每个条目每 5s 重试一次、直到 305s 截止,逐次打 ERROR 会把故障期日志淹掉
	// (最坏 kMaxPending × 61 条)。ERROR 只留给首发失败(此刻起该玩家被拦)与放弃 / 淘汰(此刻起只剩
	// TTL 兜底);后续重试的失败降为 DEBUG,趋势看 withdraw_deferred,每轮重试另有一条 WARN 汇总。
	// 脚本串作为一个 %s 参数传入是安全的:hiredis 的格式化只按**格式串**里的空格切参数,%s 代入的
	// 内容整体算一个参数(redis_client.h 的存盘 Lua 也是同一写法);key / 标记原文只含数字与冒号。
	void SendHandoffWithdraw(const handoff_mark_withdraw::Entry &entry, const char *site)
	{
		const Guid playerId = entry.playerId;
		const bool firstAttempt = entry.attempts <= 1; // CollectDue 取走时已加一:1 = 首发
		const char *deferredReason = nullptr;
		auto &redis = tlsRedis.GetZoneRedis();
		if (!redis || !redis->connected())
		{
			deferredReason = "redis_unavailable";
		}
		else
		{
			const std::string key = player_ownership::HandoffRedisKey(playerId);
			const int ret = redis->command(
				[playerId, firstAttempt, markValue = entry.markValue](hiredis::Hiredis *, redisReply *reply)
				{
					if (reply != nullptr && reply->type == REDIS_REPLY_INTEGER)
					{
						// 条件删除确实在 Redis 上执行过了:1 = 删掉了自己写的那一份;0 = 标记已不是这一份
						// (没写成 / 已过期 / 已被同一玩家的新标记覆盖),两种都说明旧标记不在了,可以销账。
						tlsHandoffWithdrawQueue.Confirm(playerId, markValue);
						if (reply->integer == 1)
						{
							LOG_INFO << "[ZoneTravel][WithdrawMark] withdrawn player=" << playerId
									 << " mark=" << markValue;
						}
						else
						{
							LOG_DEBUG << "[ZoneTravel][WithdrawMark] nothing to withdraw player=" << playerId
									  << " mark=" << markValue << " (mark absent or replaced)";
						}
						return;
					}
					// 空应答 = 连接在应答前断了,结果未知;ERROR 应答 = Redis 明确没执行(脚本被拒 / 只读副本 /
					// 正在加载数据集)。都不销账,等下一轮重试。
					const char *reason = reply == nullptr ? "reply_lost" : "reply_error";
					const std::string err =
						reply != nullptr && reply->str != nullptr ? std::string(" err=") + reply->str : std::string();
					if (firstAttempt)
					{
						LOG_ERROR << "[ZoneTravel][WithdrawMark] deferred player=" << playerId << " mark=" << markValue
								  << " reason=" << reason << err;
					}
					else
					{
						LOG_DEBUG << "[ZoneTravel][WithdrawMark] retry deferred player=" << playerId
								  << " mark=" << markValue << " reason=" << reason << err;
					}
					travel_handoff_stats::Inc(travel_handoff_stats::Get().withdrawDeferred);
				},
				"EVAL %s 1 %s %s", handoff_mark_withdraw::kLuaDelIfEqual, key.c_str(), entry.markValue.c_str());
			if (ret != REDIS_OK)
			{
				deferredReason = "dispatch_failed"; // 命令没发出去,回调不会来
			}
		}
		if (deferredReason == nullptr)
		{
			return;
		}
		if (firstAttempt)
		{
			LOG_ERROR << "[ZoneTravel][WithdrawMark] deferred player=" << playerId << " mark=" << entry.markValue
					  << " site=" << site << " reason=" << deferredReason
					  << " pending=" << tlsHandoffWithdrawQueue.size()
					  << "; scene change stays refused for this player until the withdrawal is confirmed";
		}
		else
		{
			LOG_DEBUG << "[ZoneTravel][WithdrawMark] retry deferred player=" << playerId
					  << " mark=" << entry.markValue << " site=" << site << " reason=" << deferredReason
					  << " attempts=" << entry.attempts;
		}
		travel_handoff_stats::Inc(travel_handoff_stats::Get().withdrawDeferred);
	}

	// 丢弃过了截止时刻的条目,并把到期该重试的逐条发出去。force = true 忽略重试间隔(重连时用)。
	// 放弃是安全的:截止时刻 = 登记时刻 + 标记 TTL + 余量,而登记不早于写标记,此刻 Redis 侧的标记
	// 必然已经过期;但它说明 Redis 连续 300s 以上不可用,必须留下 ERROR 与计数。
	void FlushDueHandoffWithdrawals(bool force, const char *site)
	{
		if (tlsHandoffWithdrawQueue.size() == 0)
		{
			return;
		}
		std::size_t expired = 0;
		const auto due = tlsHandoffWithdrawQueue.CollectDue(handoff_mark_withdraw::Clock::now(), force, expired);
		if (expired > 0)
		{
			LOG_ERROR << "[ZoneTravel][WithdrawMark] gave up on " << expired
					  << " mark(s): deadline (mark TTL) passed without a confirmed withdrawal";
			travel_handoff_stats::Get().withdrawExpired.fetch_add(expired, std::memory_order_relaxed);
		}
		std::size_t retried = 0;
		for (const auto &entry : due)
		{
			if (entry.attempts > 1)
			{
				++retried;
			}
			SendHandoffWithdraw(entry, site);
		}
		if (retried > 0)
		{
			// 逐条的重试失败只打 DEBUG(见 SendHandoffWithdraw),这里每轮汇总一条,让故障期仍看得见积压。
			LOG_WARN << "[ZoneTravel][WithdrawMark] retried " << retried << " unconfirmed withdrawal(s) site=" << site
					 << " pending=" << tlsHandoffWithdrawQueue.size();
		}
	}

	// 存盘要写的那份 PlayerAllData:marshal 全部字段,再补两张子表的 player_id(生成的 marshal 不填)。
	// SavePlayerToRedisImpl 与退出分支的收敛比对共用,保证"比的"与"写的"是同一个形状。
	// 不打压测探针:探针只在真正写盘的路径上打(见 SavePlayerToRedisImpl)。
	void MarshalPlayerForSave(entt::entity player, Guid playerId, PlayerAllData &out)
	{
		PlayerAllDataMessageFieldsMarshal(player, out);
		out.mutable_player_database_data()->set_player_id(playerId);
		out.mutable_player_database_1_data()->set_player_id(playerId);
	}

	// 该玩家的 PlayerAllData 在 MessageAsyncClient 里还有没有未落地的存盘(在途或排队),
	// 复用帮会 B4c 在 redis_client.h 落的只读查询 HasUnsettledSave。
	// 客户端未初始化(单测宿主;生产在任何玩家进场之前就 Initialize 了)时没有任何存盘队列,如实返回 false。
	// 注意 redis_client.h 的使用约束:在 save_callback_(即 HandlePlayerAsyncSaved)里调用恒为 false ——
	// 同 key 有更新的排队值时旧值落地不回调。退出分支仍然查它,是为了不依赖那条实现细节。
	bool HasUnsettledPlayerSave(Guid playerId)
	{
		const auto &playerRedis = tlsRedisSystem.GetPlayerDataRedis();
		return playerRedis != nullptr && playerRedis->HasUnsettledSave(playerId);
	}

	// 这次交接已冻结了多久(单调时钟)。冻结上限扫描与两个晚发闸都只经这里取冻结时长。
	// frozenAtSteady 没打点(缺省值 = 时钟纪元,某条挂交接组件的路径漏写了它):就地补记为 now、计 freeze_unstamped、
	// 打 ERROR,返回 0 —— 不当作到期。偏安全的一侧:漏打点只会让处置最多晚 kFreezeCap,不会下一拍就被销毁 / 拦下。
	// now 早于起点(调用方传入的时刻比打点早,只可能出现在单测注入时间时)按 0 计,不返回负值。
	travel_freeze_cap::Clock::duration ElapsedSinceFreeze(PlayerTravelHandoffComp &travel, Guid playerId,
														  travel_freeze_cap::Clock::time_point now)
	{
		if (travel.frozenAtSteady == travel_freeze_cap::Clock::time_point{})
		{
			travel.frozenAtSteady = now;
			travel_handoff_stats::Inc(travel_handoff_stats::Get().freezeUnstamped);
			LOG_ERROR << "[ZoneTravel][FreezeCap] handoff of player " << playerId
					  << " has no monotonic freeze stamp; stamping it now (metric=freeze_unstamped)";
			return travel_freeze_cap::Clock::duration::zero();
		}
		return now >= travel.frozenAtSteady ? now - travel.frozenAtSteady : travel_freeze_cap::Clock::duration::zero();
	}

	// 该玩家的 gate 会话是否还活着:会话快照在、gate_session_id 有效、且 SessionMap 仍把它映射到本玩家
	// (与 player_team.cpp HasLiveSession 同一判法)。StartTravelHandoff 的发起校验与 ConcludeHandoffAfterMarkSent
	// 的"要不要踢线"共用这一把尺子:断线宽限期内的实体拿旧会话去请求会改写离线玩家的 location,
	// 往已被取代 / 已解绑的会话发 34 则可能踢掉别人。
	bool HasLiveGateSession(entt::entity player, Guid playerId)
	{
		const auto *session = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(player);
		if (session == nullptr || session->gate_session_id() == 0 || session->gate_session_id() == kInvalidSessionId)
		{
			return false;
		}
		const auto sessionIt = SessionMap().find(session->gate_session_id());
		return sessionIt != SessionMap().end() && sessionIt->second == playerId;
	}
} // namespace

bool player_exit::IsPersistedPayloadCurrent(const PlayerAllData &landed, const PlayerAllData &current)
{
	PlayerAllData landedStripped = landed;
	PlayerAllData currentStripped = current;
	for (PlayerAllData *payload : {&landedStripped, &currentStripped})
	{
		payload->mutable_player_database_data()->clear_stress_test_probe();
		payload->mutable_player_database_1_data()->clear_stress_test_probe();
	}
	return dirty_save::IsEqual(landedStripped, currentStripped);
}

namespace
{
	// ── 断线释放标记(A1′)与载入时清标记(A2′)的粘合代码 ─────────────────────────────
	// 判定、Lua 与结果枚举见 exit_release_mark.h;设计见 cross-zone-scene-travel.md §12.6.3 第二步 / 第三步。
	// 全部只在 scene 逻辑线程上跑(FinishExitAfterPersist、PlayerEnterGameNode、HandlePlayerAsyncLoaded、Redis 回调、
	// RedisSystem 的重连回调与 1s 定时器同一个 EventLoop),不需要锁。
	// Redis 回调一律只按值捕获 id / 代际 / 标记原文,不绑对象、不持连接(AGENTS §11.7)。

	// 本节点身份探针(M3),由节点入口经 PlayerLifecycleSystem::SetNodeIdentityProbe 注入。
	thread_local std::function<bool()> tlsNodeIdentityProbe;

	// A2′ 载入生命周期号的发号器(节点内单调,0 保留给"未分配")。
	thread_local uint64_t tlsInheritClearLifecycleSeq = 0;

	// 已放弃、但还有 A2′ 应答未到的载入生命周期:lifecycle → 玩家与待到应答数。晚到的应答若删掉了
	// (或可能删掉了)epoch == N 的标记,由它补写一次(M7)。条目在补写发出或应答到齐时删除;hiredis 断线时
	// 对挂起命令逐个回空应答,条目不会无界增长。建出实体的生命周期不登记:那时本节点就是持有者,不补写。
	struct AbandonedInheritClear
	{
		Guid playerId{kInvalidGuid};
		uint32_t outstandingReplies{0};
	};
	thread_local std::unordered_map<uint64_t, AbandonedInheritClear> tlsAbandonedInheritClears;

	bool ReadSwitchOnce(const char *envName, bool defaultValue)
	{
		const char *raw = std::getenv(envName);
		const bool value = exit_release_mark::ParseSwitch(raw, defaultValue);
		LOG_INFO << "[ExitRelease] " << envName << "=" << (raw != nullptr ? raw : "<unset>") << " -> "
				 << (value ? "on" : "off");
		return value;
	}

	// A1′ 总开关(M14),默认开;进程内第一次用到时读一次环境变量。回滚步骤见 exit_release_mark::kEnvExitReleaseMark。
	bool IsExitReleaseMarkEnabled()
	{
		static const bool enabled = ReadSwitchOnce(exit_release_mark::kEnvExitReleaseMark, true);
		return enabled;
	}

	// dev 旁路开关(M6),默认关;必须与 scene_manager 的 AllowUnsafeCrossNodeHandoff 同步打开。
	bool IsDevUnsafeCrossNodeHandoff()
	{
		static const bool enabled = ReadSwitchOnce(exit_release_mark::kEnvDevUnsafeCrossNode, false);
		return enabled;
	}

	// 本节点身份当前是否确认有效(M3):疏散中一律无效;其余看节点入口注入的探针(etcd 租约未推定过期)。
	// 没注入 = 不确认(fail-closed)。
	bool IsNodeIdentityConfirmed()
	{
		if (tlsEmergencyRelocating)
		{
			return false;
		}
		return tlsNodeIdentityProbe && tlsNodeIdentityProbe();
	}

	exit_release_mark::ReplyShape ToReplyShape(const redisReply *reply)
	{
		if (reply == nullptr)
		{
			return exit_release_mark::ReplyShape::kNone;
		}
		switch (reply->type)
		{
		case REDIS_REPLY_INTEGER:
			return exit_release_mark::ReplyShape::kInteger;
		case REDIS_REPLY_ERROR:
			return exit_release_mark::ReplyShape::kError;
		default:
			return exit_release_mark::ReplyShape::kOther;
		}
	}

	int64_t ReplyInteger(const redisReply *reply)
	{
		return reply != nullptr && reply->type == REDIS_REPLY_INTEGER ? static_cast<int64_t>(reply->integer) : 0;
	}

	// 失败应答的排障后缀:空应答 → reason=reply_lost;ERROR → reason=reply_error err=…;其余为空串。
	std::string ReplyFailureSuffix(const redisReply *reply)
	{
		if (reply == nullptr)
		{
			return " reason=reply_lost";
		}
		if (reply->type == REDIS_REPLY_INTEGER)
		{
			return std::string();
		}
		std::string suffix = " reason=reply_error";
		if (reply->type == REDIS_REPLY_ERROR && reply->str != nullptr)
		{
			suffix += " err=";
			suffix += reply->str;
		}
		return suffix;
	}

	// 条件写标记的三个来源:A1′ 退出收尾,A2′ 载入放弃补写,改派踢线前的凭证兜底补写(待确认表核实出"没变"、
	// 而 Redis 里已没有同代标记时,见 relocate_confirm::DecideCredential)。只决定计到哪组计数、日志里写哪个 site。
	enum class MarkWriteSite : uint8_t
	{
		kExitRelease,
		kInheritRewrite,
		kRelocateRefresh,

		kCount
	};
	constexpr const char *kMarkWriteSiteNames[] = {
		"exit_release",
		"inherit_rewrite",
		"relocate_refresh",
	};
	static_assert(std::size(kMarkWriteSiteNames) == static_cast<std::size_t>(MarkWriteSite::kCount),
				  "kMarkWriteSiteNames 必须与 MarkWriteSite 一一对应");

	constexpr const char *MarkWriteSiteName(MarkWriteSite site)
	{
		const auto index = static_cast<std::size_t>(site);
		return index < std::size(kMarkWriteSiteNames) ? kMarkWriteSiteNames[index] : "?";
	}

	// 按 site 把一次条件写的结果计到各自那一组上。必须按 site 分三路,不能再用"是不是 A2′ 补写"的二分:
	// 改派的凭证补写会被算进 A1′ 的 written / epoch_moved / failed,[ExitRelease] 行的 attempted 就对不上账了。
	void CountMarkWrite(MarkWriteSite site, exit_release_mark::MarkWriteResult result)
	{
		using Result = exit_release_mark::MarkWriteResult;
		auto &stats = exit_release_stats::Get();
		auto &relocateStats = relocate_confirm_stats::Get();
		// 每个 site 的三个去向:写成 / owner_epoch 已变没写 / 失败。
		std::atomic<uint64_t> *written = nullptr;
		std::atomic<uint64_t> *epochMoved = nullptr;
		std::atomic<uint64_t> *failed = nullptr;
		switch (site)
		{
		case MarkWriteSite::kExitRelease:
			written = &stats.written;
			epochMoved = &stats.epochMoved;
			failed = &stats.failed;
			break;
		case MarkWriteSite::kInheritRewrite:
			written = &stats.inheritRewritten;
			epochMoved = &stats.inheritRewriteSkipped;
			failed = &stats.inheritRewriteFailed;
			break;
		case MarkWriteSite::kRelocateRefresh:
			written = &relocateStats.credentialWritten;
			epochMoved = &relocateStats.credentialEpochMoved;
			failed = &relocateStats.credentialFailed;
			break;
		case MarkWriteSite::kCount:
			break;
		}
		if (written == nullptr)
		{
			// 越界的 site 只可能是调用方传错:不计到任何一组上(计错组比不计更误导),留一条 ERROR。
			LOG_ERROR << "[ExitRelease] CountMarkWrite called with an unknown site "
					  << static_cast<uint32_t>(site) << "; result not counted";
			return;
		}
		switch (result)
		{
		case Result::kWritten:
			written->fetch_add(1, std::memory_order_relaxed);
			break;
		case Result::kEpochMoved:
			epochMoved->fetch_add(1, std::memory_order_relaxed);
			break;
		default:
			failed->fetch_add(1, std::memory_order_relaxed);
			break;
		}
	}

	// 条件写 player:{id}:handoff "E:now_ms":owner_epoch 仍等于 E 才写(exit_release_mark::kLuaWriteIfOwnerEpoch)。
	// A1′、A2′ 放弃补写与改派踢线前的凭证补写(MarkWriteSite::kRelocateRefresh)共用。走 tlsRedis.GetZoneRedis() ——
	// 与存盘、A2′、标记撤回、改派的核实读同一条连接(FIFO):退出存盘的落地回调已经到了,这条写必然排在它之后执行。
	// **一律不重试**(R7):写不成的后果只是这一次重登可能回 18、下一次重试放行;重试会把"这一刻已落盘"的
	// 凭证推迟到一个不再成立的时刻。在途计数:发出前 +1,回调或派发失败时 -1(R6)。
	void SendConditionalMarkWrite(Guid playerId, uint64_t epoch, MarkWriteSite site)
	{
		const std::string markValue = player_ownership::HandoffRedisValue(epoch, TimeSystem::NowMillisecondsUTC());
		auto &redis = tlsRedis.GetZoneRedis();
		if (!redis || !redis->connected())
		{
			CountMarkWrite(site, exit_release_mark::MarkWriteResult::kFailed);
			LOG_WARN << "[ExitRelease] mark write failed player=" << playerId << " mark=" << markValue
					 << " site=" << MarkWriteSiteName(site) << " reason=redis_unavailable (not retried)";
			return;
		}
		const std::string ownerEpochKey = player_ownership::OwnerEpochRedisKey(playerId);
		const std::string handoffKey = player_ownership::HandoffRedisKey(playerId);
		const std::string epochText = std::to_string(epoch);
		++tlsExitReleaseMarksInFlight;
		// 脚本串作为一个 %s 参数传入是安全的:hiredis 只按**格式串**里的空格切参数(同 SendHandoffWithdraw)。
		const int ret = redis->command(
			[playerId, markValue, site](hiredis::Hiredis *, redisReply *reply)
			{
				if (tlsExitReleaseMarksInFlight > 0)
				{
					--tlsExitReleaseMarksInFlight;
				}
				const auto result =
					exit_release_mark::ClassifyMarkWriteReply(ToReplyShape(reply), ReplyInteger(reply));
				CountMarkWrite(site, result);
				switch (result)
				{
				case exit_release_mark::MarkWriteResult::kWritten:
					LOG_INFO << "[ExitRelease] mark written player=" << playerId << " mark=" << markValue
							 << " site=" << MarkWriteSiteName(site);
					break;
				case exit_release_mark::MarkWriteResult::kEpochMoved:
					LOG_INFO << "[ExitRelease] mark not written player=" << playerId << " mark=" << markValue
							 << " site=" << MarkWriteSiteName(site) << ": owner_epoch moved on";
					break;
				default:
					LOG_WARN << "[ExitRelease] mark write failed player=" << playerId << " mark=" << markValue
							 << " site=" << MarkWriteSiteName(site) << ReplyFailureSuffix(reply) << " (not retried)";
					break;
				}
			},
			"EVAL %s 2 %s %s %s %s %d", exit_release_mark::kLuaWriteIfOwnerEpoch, ownerEpochKey.c_str(),
			handoffKey.c_str(), epochText.c_str(), markValue.c_str(), player_ownership::kHandoffMarkTtlSec);
		if (ret != REDIS_OK)
		{
			// 命令没发出去,回调不会来:自己把计数还回去。
			if (tlsExitReleaseMarksInFlight > 0)
			{
				--tlsExitReleaseMarksInFlight;
			}
			CountMarkWrite(site, exit_release_mark::MarkWriteResult::kFailed);
			LOG_WARN << "[ExitRelease] mark write failed player=" << playerId << " mark=" << markValue
					 << " site=" << MarkWriteSiteName(site) << " reason=dispatch_failed (not retried)";
		}
	}

	// A1′:退出收尾时按 exit_release_mark::DecideExitReleaseMark 判定写不写断线释放标记,并按结果计数。
	// 只由 FinishExitAfterPersist 调用,位置在 DispatchEmergencyRelocate 之后、RemovePlayerSession 之前
	// (实体与意图组件此刻还在)。
	//   relocateTicketConsumed    本次改派消费了疏散票据:这次的标记归改派负责(条件写,可能没写成;没生效要踢线时
	//                             由待确认表在踢线前兜底补写),A1′ 不覆盖。票据作废(没发改派)时为 false,A1′ 照常判定
	//   exitWonOverIssuedHandoff  FinishExitAfterPersist 自己的"退出优先"分支里交接标记已写(M11)
	void WriteExitReleaseMarkIfEligible(Guid playerId, entt::entity playerEntity, bool relocateTicketConsumed,
										bool exitWonOverIssuedHandoff)
	{
		exit_release_mark::ExitReleaseFacts facts;
		facts.featureEnabled = IsExitReleaseMarkEnabled();
		facts.entityValid = tlsEcs.actorRegistry.valid(playerEntity);
		const PlayerExitIntentComp *intent =
			facts.entityValid ? tlsEcs.actorRegistry.try_get<PlayerExitIntentComp>(playerEntity) : nullptr;
		facts.hasIntent = intent != nullptr;
		if (intent != nullptr)
		{
			facts.cause = intent->cause;
			facts.releaseMarkSuppressed = intent->releaseMarkSuppressed;
			facts.suppressedBy = intent->suppressedBy;
		}
		facts.relocateTicketConsumed = relocateTicketConsumed;
		// 与改派的"更早请求可能还会被放行"共用同一个判据(player_exit_intent.h),两处不许各写一份。
		facts.handoffMarkInflight = player_exit::HandoffEnterSceneMayBeInFlight(exitWonOverIssuedHandoff, intent);
		if (facts.entityValid)
		{
			if (const auto *epochComp = tlsEcs.actorRegistry.try_get<PlayerOwnerEpochComp>(playerEntity))
			{
				facts.ownerEpoch = epochComp->epoch;
			}
		}
		facts.identityValid = IsNodeIdentityConfirmed();

		const auto decision = exit_release_mark::DecideExitReleaseMark(facts);
		exit_release_stats::Inc(exit_release_stats::Get().decisions[static_cast<std::size_t>(decision)]);
		if (decision == exit_release_mark::ExitReleaseDecision::kWrite)
		{
			SendConditionalMarkWrite(playerId, facts.ownerEpoch, MarkWriteSite::kExitRelease);
			return;
		}
		// 常态的不写(原因不在可写集合 / 开关关闭 / epoch 未铸造 / 实体已无效)打 DEBUG:ReleasePlayer 每次跨节点
		// 换图都会走到 skip_cause;其余(压制 / 改派 / 交接在途 / 身份不确认 / 意图缺失)打 INFO,排障要看得见。
		switch (decision)
		{
		case exit_release_mark::ExitReleaseDecision::kSkipCause:
		case exit_release_mark::ExitReleaseDecision::kSkipDisabled:
		case exit_release_mark::ExitReleaseDecision::kSkipEpochUnknown:
		case exit_release_mark::ExitReleaseDecision::kSkipEntityInvalid:
			LOG_DEBUG << "[ExitRelease] not writing release mark player=" << playerId
					  << " reason=" << exit_release_mark::ExitReleaseDecisionName(decision)
					  << " cause=" << player_exit::ExitCauseName(facts.cause);
			break;
		default:
			LOG_INFO << "[ExitRelease] not writing release mark player=" << playerId
					 << " reason=" << exit_release_mark::ExitReleaseDecisionName(decision)
					 << " cause=" << player_exit::ExitCauseName(facts.cause) << " owner_epoch=" << facts.ownerEpoch;
			break;
		}
	}

	// 载入被放弃(会话在载入期间取消 / 载入 RedisError / 闸门拒绝)时处理本生命周期删掉过的标记(M7):
	// 已知删掉过(或可能删掉过)epoch == N 的那一份 → 按 A1′ 同款条件补写一次(owner_epoch == N 才写 "N:now");
	// 否则若还有 A2′ 应答未到,登记进 tlsAbandonedInheritClears,由晚到的应答决定要不要补写。只发一次。
	// 安全性同 A1′:本生命周期从未建过实体,盘上仍是上一任持有者的终态。
	void AbandonInheritClear(Guid playerId, const InheritClearState &state, const char *reason)
	{
		if (state.rewriteEpoch != 0)
		{
			LOG_INFO << "[ExitRelease][InheritClear] load abandoned (" << reason << ") after the inherited mark of"
					 << " epoch " << state.rewriteEpoch << " was deleted (or may have been); re-writing it player="
					 << playerId;
			SendConditionalMarkWrite(playerId, state.rewriteEpoch, MarkWriteSite::kInheritRewrite);
			return;
		}
		if (state.lifecycle != 0 && state.outstandingReplies > 0)
		{
			AbandonedInheritClear abandoned;
			abandoned.playerId = playerId;
			abandoned.outstandingReplies = state.outstandingReplies;
			tlsAbandonedInheritClears.insert_or_assign(state.lifecycle, abandoned);
		}
	}

	// 拒建实体(M2 / M10)。写法照 HandlePlayerAsyncLoadFailed 的 RedisError 分支:回 kEnterSceneFailed、擦掉预登记
	// 会话与待入场条目(之后到达的载入结果找不到条目,按"孤儿载入"跳过);随后按 M7 处理删掉过的标记。
	void RefuseInheritedEntry(Guid playerId, const char *reason)
	{
		auto pendingIt = tlsPendingEnterMap.find(playerId);
		if (pendingIt == tlsPendingEnterMap.end())
		{
			return;
		}
		const SessionId sessionId = pendingIt->second.enterInfo.session_id();
		const uint64_t ownerEpoch = pendingIt->second.ownerEpoch;
		InheritClearState state = std::move(pendingIt->second.inheritClear);
		tlsPendingEnterMap.erase(pendingIt);

		exit_release_stats::Inc(exit_release_stats::Get().inheritRefused);
		LOG_ERROR << "[ExitRelease][InheritClear] refusing entry player=" << playerId << " owner_epoch=" << ownerEpoch
				  << " session=" << sessionId << " reason=" << reason << " attempts=" << state.attemptsSent
				  << " (metric=inherit_refused)";
		if (sessionId != 0)
		{
			// Best-effort tip; safe even if the gate session is already gone.
			SendTipToPendingSession(sessionId, playerId, kEnterSceneFailed);
			SessionMap().erase(sessionId);
		}
		AbandonInheritClear(playerId, state, reason);
	}

	void SendInheritClear(Guid playerId);

	// A2′ 失败(空应答 / ERROR / Redis 不可用 / 派发失败)后:没到上限就排下一次重发(期间保留已载入的数据),
	// 到了上限(次数或截止时刻,单调时钟)就拒建实体。
	void ScheduleInheritClearRetry(Guid playerId, const char *reason)
	{
		auto pendingIt = tlsPendingEnterMap.find(playerId);
		if (pendingIt == tlsPendingEnterMap.end())
		{
			return;
		}
		auto &state = pendingIt->second.inheritClear;
		state.phase = exit_release_mark::InheritClearPhase::kRetryWait;
		const auto now = exit_release_mark::Clock::now();
		if (exit_release_mark::IsInheritClearExhausted(state.attemptsSent, now - state.firstSentAt))
		{
			RefuseInheritedEntry(playerId, "inherited-mark clear kept failing");
			return;
		}
		state.nextRetryAt = now + exit_release_mark::InheritClearRetryDelay(state.attemptsSent);
		LOG_WARN << "[ExitRelease][InheritClear] clear failed player=" << playerId << " epoch=" << state.targetEpoch
				 << " reason=" << reason << " attempt=" << state.attemptsSent << "/"
				 << exit_release_mark::kInheritClearMaxAttempts << "; will retry, loaded data (if any) is kept";
	}

	void HandleInheritClearReply(Guid playerId, uint64_t lifecycle, uint64_t epoch, uint32_t seq,
								 exit_release_mark::InheritClearResult result, const std::string &detail)
	{
		auto &stats = exit_release_stats::Get();
		exit_release_stats::Inc(stats.inheritResults[static_cast<std::size_t>(result)]);
		if (exit_release_mark::IsInheritClearFailure(result))
		{
			exit_release_stats::Inc(stats.inheritFailed);
		}
		// 成功类逐条 INFO(M15:验证步骤要看得见),核对不过与失败 WARN(拒建实体另有 ERROR)。
		if (exit_release_mark::IsInheritClearConfirmed(result))
		{
			LOG_INFO << "[ExitRelease][InheritClear] player=" << playerId << " epoch=" << epoch
					 << " result=" << exit_release_mark::InheritClearResultName(result);
		}
		else
		{
			LOG_WARN << "[ExitRelease][InheritClear] player=" << playerId << " epoch=" << epoch
					 << " result=" << exit_release_mark::InheritClearResultName(result) << detail;
		}

		auto pendingIt = tlsPendingEnterMap.find(playerId);
		if (pendingIt == tlsPendingEnterMap.end() || pendingIt->second.inheritClear.lifecycle != lifecycle)
		{
			// 这次载入已经结束:放弃了(在放弃表里)或者建出了实体(不在表里,本节点就是持有者,什么都不做)。
			const auto abandonedIt = tlsAbandonedInheritClears.find(lifecycle);
			if (abandonedIt == tlsAbandonedInheritClears.end())
			{
				return;
			}
			// epoch == 0(路由不带 owner_epoch 的那一种清理)不知道删掉的是哪一代,无从补写:只记账(见 exit_release_mark.h)。
			if (epoch != 0 && exit_release_mark::MayHaveDeletedCurrent(result))
			{
				const Guid abandonedPlayer = abandonedIt->second.playerId;
				tlsAbandonedInheritClears.erase(abandonedIt);
				// 补写前先看本节点是否已由更新的一轮接手(判定与理由见 exit_release_mark::DecideAbandonedRewrite)。
				// 移交时:更新一轮建出实体即作废;它也放弃时由 AbandonInheritClear 补写,那时补写必然排在任何
				// 更后一轮的 A2′ 之前(同一条连接 FIFO)。它自己的 A2′ 删掉当前代际时会覆盖 rewriteEpoch,
				// 两者都是 owner_epoch 条件写,取哪一个都只在归属未变时生效。
				const auto newerIt = tlsPendingEnterMap.find(abandonedPlayer);
				const auto rewriteDecision = exit_release_mark::DecideAbandonedRewrite(
					tlsEcs.actorRegistry.valid(tlsEcs.GetPlayer(abandonedPlayer)), newerIt != tlsPendingEnterMap.end());
				if (rewriteDecision == exit_release_mark::AbandonedRewriteDecision::kDropNodeHolds)
				{
					exit_release_stats::Inc(exit_release_stats::Get().inheritRewriteDroppedNodeHolds);
					LOG_INFO << "[ExitRelease][InheritClear] late reply for an abandoned load deleted (or may have"
							 << " deleted) the mark of epoch " << epoch << ", but this node already holds the"
							 << " player; not re-writing it player=" << abandonedPlayer;
					return;
				}
				if (rewriteDecision == exit_release_mark::AbandonedRewriteDecision::kHandToNewerLoad)
				{
					auto &newerState = newerIt->second.inheritClear;
					if (newerState.rewriteEpoch == 0)
					{
						newerState.rewriteEpoch = epoch;
					}
					exit_release_stats::Inc(exit_release_stats::Get().inheritRewriteHandedOver);
					LOG_INFO << "[ExitRelease][InheritClear] late reply for an abandoned load deleted (or may have"
							 << " deleted) the mark of epoch " << epoch << "; a newer load (lifecycle "
							 << newerState.lifecycle << ") now owns the re-write player=" << abandonedPlayer;
					return;
				}
				LOG_INFO << "[ExitRelease][InheritClear] late reply for an abandoned load deleted (or may have deleted)"
						 << " the mark of epoch " << epoch << "; re-writing it player=" << abandonedPlayer;
				SendConditionalMarkWrite(abandonedPlayer, epoch, MarkWriteSite::kInheritRewrite);
				return;
			}
			if (abandonedIt->second.outstandingReplies <= 1)
			{
				tlsAbandonedInheritClears.erase(abandonedIt);
			}
			else
			{
				--abandonedIt->second.outstandingReplies;
			}
			return;
		}

		auto &state = pendingIt->second.inheritClear;
		if (state.outstandingReplies > 0)
		{
			--state.outstandingReplies;
		}
		if (epoch != 0 && exit_release_mark::MayHaveDeletedCurrent(result))
		{
			state.rewriteEpoch = epoch; // epoch == 0 的清理不知道删掉的是哪一代,不补写(只影响活性)
		}
		if (seq != state.attemptSeq || epoch != state.targetEpoch)
		{
			return; // 更早的一次(或针对别的 epoch):只记账,不参与闸门判定(M2:只认本 ctx.ownerEpoch 的那一次)
		}
		if (exit_release_mark::IsInheritClearConfirmed(result))
		{
			state.phase = exit_release_mark::InheritClearPhase::kConfirmed;
			if (state.stashedLoad != nullptr)
			{
				// 载入早已完成、在等这条应答:按原路径重走一遍(会话取消判定 → 闸门 → 建实体)。
				// 调用之后待入场条目会被擦掉,state 引用随之失效,不再使用。
				const std::shared_ptr<const PlayerAllData> stashed = std::move(state.stashedLoad);
				state.stashedLoad.reset();
				PlayerLifecycleSystem::HandlePlayerAsyncLoaded(playerId, *stashed);
			}
			return;
		}
		if (result == exit_release_mark::InheritClearResult::kEpochMismatch)
		{
			// 核对不过:这次路由已过时(M2)。立即拒绝、不补发;Lua 一个标记都没删。
			RefuseInheritedEntry(playerId, "owner_epoch no longer matches this route (clear returned -1)");
			return;
		}
		ScheduleInheritClearRetry(playerId, exit_release_mark::InheritClearResultName(result));
	}

	// 针对待入场条目的 targetEpoch 发一次 A2′。条目此刻必须在 tlsPendingEnterMap 里。
	void SendInheritClear(Guid playerId)
	{
		auto pendingIt = tlsPendingEnterMap.find(playerId);
		if (pendingIt == tlsPendingEnterMap.end())
		{
			return;
		}
		auto &state = pendingIt->second.inheritClear;
		const uint64_t epoch = state.targetEpoch;
		++state.attemptSeq;
		++state.attemptsSent;
		if (state.attemptsSent == 1)
		{
			state.firstSentAt = exit_release_mark::Clock::now();
		}
		state.phase = exit_release_mark::InheritClearPhase::kInFlight;
		const uint64_t lifecycle = state.lifecycle;
		const uint32_t seq = state.attemptSeq;

		auto &redis = tlsRedis.GetZoneRedis();
		if (!redis || !redis->connected())
		{
			exit_release_stats::Inc(exit_release_stats::Get().inheritFailed);
			ScheduleInheritClearRetry(playerId, "redis_unavailable");
			return;
		}
		const std::string ownerEpochKey = player_ownership::OwnerEpochRedisKey(playerId);
		const std::string handoffKey = player_ownership::HandoffRedisKey(playerId);
		const std::string epochText = std::to_string(epoch);
		++state.outstandingReplies;
		const auto onReply = [playerId, lifecycle, epoch, seq](hiredis::Hiredis *, redisReply *reply)
		{
			HandleInheritClearReply(
				playerId, lifecycle, epoch, seq,
				exit_release_mark::ClassifyInheritClearReply(ToReplyShape(reply), ReplyInteger(reply)),
				ReplyFailureSuffix(reply));
		};
		// epoch == 0:路由不带 owner_epoch(兼容窗口),无从核对,改删 ≤ 当前 owner_epoch 的标记(见 exit_release_mark.h)。
		const int ret = epoch == 0
							? redis->command(onReply, "EVAL %s 2 %s %s", exit_release_mark::kLuaInheritClearUnknownEpoch,
											 ownerEpochKey.c_str(), handoffKey.c_str())
							: redis->command(onReply, "EVAL %s 2 %s %s %s", exit_release_mark::kLuaInheritClear,
											 ownerEpochKey.c_str(), handoffKey.c_str(), epochText.c_str());
		if (ret != REDIS_OK)
		{
			// 命令没发出去,回调不会来。command() 期间没有改过待入场表,state 引用仍有效。
			--state.outstandingReplies;
			exit_release_stats::Inc(exit_release_stats::Get().inheritFailed);
			ScheduleInheritClearRetry(playerId, "dispatch_failed");
		}
	}
} // namespace

namespace
{
	// ── 疏散 / 排空改派的待确认表:粘合代码 ──────────────────────────────────────────────
	// 判定、阶段迁移与预算全在 relocate_confirm.h(契约写在它的文件头,改这里之前先读一遍);设计见
	// cross-zone-scene-travel.md「CPP-3 疏散 / 排空改派的待确认表」。这一段只做粘合:读写 tlsRelocateConfirms、
	// 发核实命令、按会话推 gate、计数、打日志。
	// 全部只在 scene 逻辑线程上跑(线程模型见 tlsRelocateConfirms 的定义),不需要锁。Redis 回调只按值捕获
	// playerId、条目号与核实代际,不绑对象、不持连接(AGENTS §11.7);表内指针只在一次调用栈里用,任何可能结清条目的
	// 调用(含发 Redis 命令)之后一律重新 Find。不往任何实体上挂组件;读到的 owner_epoch 只当条件补写的参数与日志,
	// 不写进任何 PlayerOwnerEpochComp(player_ownership_comp.h:读 Redis 采纳 epoch 的入口只有归属取证那一个)。
	// 时间:定时扫描用调用方注入的时刻,其余入口在各自开头取一次单调时钟,一路往下传。

	using RelocateClock = relocate_confirm::Clock;

	int64_t RelocateMillis(RelocateClock::duration duration)
	{
		return std::chrono::duration_cast<std::chrono::milliseconds>(duration).count();
	}

	// 条目从登记到 now 过了多久(毫秒),只进日志。now 早于登记时刻(单测注入时间)按 0 计。
	int64_t RelocateAgeMs(const relocate_confirm::Entry &entry, RelocateClock::time_point now)
	{
		return now > entry.createdAt ? RelocateMillis(now - entry.createdAt) : 0;
	}

	// 日志里 evidence= 的取值。条目的 evidence 字段要到第一次转入核实(Table::BeginVerify)才写,此前是初值 kNotSent。
	// 从没进过核实就结清 / 被列进积压清单的条目(例如等应答时被进场路由直接带进落地),照打初值会读成"没发出去",
	// 而它其实已经发出。真正的 kNotSent 只在关联号为 0 时出现(没发出去,或写标记的回调超时 —— 那之后不会再发),
	// 所以"kNotSent 且关联号非 0"= 还没有任何证据,打成 none。
	const char *RelocateEvidenceText(const relocate_confirm::Entry &entry)
	{
		if (entry.evidence == relocate_confirm::Evidence::kNotSent && entry.correlationId != 0)
		{
			return "none";
		}
		return relocate_confirm::EvidenceName(entry.evidence);
	}

	// 一条**已经从表里取走**的条目按"不踢"收口:计 outcome、打 resolved 行。
	// verdictText = 日志里 verdict= 的取值:这次结清是由一次核实读数裁决出来的,传 relocate_confirm::VerdictName(读数);
	// 没有读数的一律传 relocate_confirm::kVerdictUnread。reply_code 与 unsettled 两项是给 gave_up_unverified 排障用的:
	// 分得出是"还有请求结局未定"还是"拒绝码不在可以盲踢的白名单里"。
	void ConcludeRelocateWithoutKick(const relocate_confirm::Entry &entry, relocate_confirm::Outcome outcome,
									 const char *verdictText, RelocateClock::time_point now)
	{
		using relocate_confirm::Outcome;
		relocate_confirm_stats::Inc(relocate_confirm_stats::Get().outcomes[static_cast<std::size_t>(outcome)]);
		const std::string line = "[RelocateConfirm] resolved player=" + std::to_string(entry.playerId) +
								 " seq=" + std::to_string(entry.seq) +
								 " outcome=" + relocate_confirm::OutcomeName(outcome) +
								 " evidence=" + RelocateEvidenceText(entry) +
								 " verdict=" + verdictText +
								 " reply_code=" + std::to_string(entry.replyErrorCode) +
								 " unsettled=" + (relocate_confirm::RequestOutcomeUnsettled(entry) ? "1" : "0") +
								 " age_ms=" + std::to_string(RelocateAgeMs(entry, now));
		switch (outcome)
		{
		case Outcome::kGranted:
		case Outcome::kMovedElsewhere:
		case Outcome::kLandedHere:
			LOG_INFO << line;
			return;
		case Outcome::kSuperseded:
		case Outcome::kIndeterminate:
			// 不踢,但值得留证据:会话换了 / 读到了却判不清。
			LOG_WARN << line;
			return;
		case Outcome::kGaveUpUnverified:
		case Outcome::kKickVerified:   // 两个踢线结果不该走到这里(它们由 KickRelocatedSession 打 kick 行);
		case Outcome::kKickUnverified: // 真走到了也按最高级别留痕。
		case Outcome::kCount:
			break;
		}
		// gave_up_unverified:读不到、又不能排除已被放行 —— 这条会话可能还挂在没有实体的玩家上,本节点不敢踢。
		LOG_ERROR << line;
	}

	// 取走条目并按"不踢"收口。条目已不在(另一条路径先结清了)时 no-op。
	void ResolveRelocate(Guid playerId, relocate_confirm::Outcome outcome, const char *verdictText,
						 RelocateClock::time_point now)
	{
		const std::optional<relocate_confirm::Entry> taken = tlsRelocateConfirms.Take(playerId);
		if (!taken.has_value())
		{
			return;
		}
		ConcludeRelocateWithoutKick(*taken, outcome, verdictText, now);
	}

	// 踢线:按票据里抄下的会话直推 tip(kEnterSceneFailed)+ KickPlayer(34),让客户端断线回选服重登。
	// 核实过的踢(kKickVerified)与读不到时的踢(kKickUnverified)都经这里。
	//   credentialAction / credentialEpoch  踢线前补不补写重登凭证,由调用方按同一次核实读数判好
	//                                       (relocate_confirm::DecideCredential);没有读数时传 kNone / 0;
	//   verdictText                         同 ConcludeRelocateWithoutKick。
	// tip 码用 kEnterSceneFailed:客户端按它识别"换图确定没成"并断线重登。不用 kEnterSceneServerBusy —— 那个码被客户端
	// 刻意排除在"确定没成"之外(见 DispatchEnterSceneTransportFailure)。
	void KickRelocatedSession(Guid playerId, relocate_confirm::Outcome outcome,
							  relocate_confirm::CredentialAction credentialAction, uint64_t credentialEpoch,
							  const char *verdictText, RelocateClock::time_point now)
	{
		using relocate_confirm::Outcome;
		// 1. 先取走:下面的补写与推送都不会重入到同一条条目上,重复调用也拿不到第二次。
		const std::optional<relocate_confirm::Entry> taken = tlsRelocateConfirms.Take(playerId);
		if (!taken.has_value())
		{
			return;
		}
		const relocate_confirm::Entry &entry = *taken;
		auto &stats = relocate_confirm_stats::Get();

		// 2. 踢线前的本地复核:本节点此刻有他的有效实体就不踢、也不补写。"条目存在 ⇒ 本节点没有该玩家实体"只是契约,
		//    这里在代码里判一次:载入卡过落地预算才建出实体、或核实应答回来时实体已建出,踢下去打掉的是一条正在本节点
		//    上的合法会话,补写出来的标记还会在新持有期间留成一张免存盘的放行证(回档)。
		//    载入还在途(只在待入场表里)时照常踢,但不补写 —— 那一条由 DecideCredential 的 kSkipLocalHolder 挡。
		if (const auto localEntity = tlsEcs.GetPlayer(playerId); tlsEcs.actorRegistry.valid(localEntity))
		{
			const auto *snapshot = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(localEntity);
			const bool sameSession = snapshot != nullptr && snapshot->gate_session_id() == entry.sessionId;
			const Outcome withheld = sameSession ? Outcome::kLandedHere : Outcome::kSuperseded;
			relocate_confirm_stats::Inc(stats.outcomes[static_cast<std::size_t>(withheld)]);
			if (sameSession)
			{
				LOG_INFO << "[RelocateConfirm] kick withheld player=" << playerId << " seq=" << entry.seq
						 << ": entity is back on this node, outcome=" << relocate_confirm::OutcomeName(withheld);
			}
			else
			{
				LOG_WARN << "[RelocateConfirm] kick withheld player=" << playerId << " seq=" << entry.seq
						 << ": entity is back on this node, outcome=" << relocate_confirm::OutcomeName(withheld);
			}
			return;
		}

		// 3. 凭证:每次踢线按判定结果计一次;只有 kAttempt 真的去写。补写是 owner_epoch 条件写,一律不重试,
		//    计入 tlsExitReleaseMarksInFlight(因而进两个 drain 谓词),结果由 CountMarkWrite 记到 credential_* 上。
		//    先发凭证写、再推踢线:两条走的是不同的连接(zone Redis / gate 的 RpcSession),彼此没有顺序保证,
		//    这样排只是让客户端被踢后立刻重登时标记大概率已经落地。
		relocate_confirm_stats::Inc(stats.credentialActions[static_cast<std::size_t>(credentialAction)]);
		if (credentialAction == relocate_confirm::CredentialAction::kAttempt)
		{
			SendConditionalMarkWrite(playerId, credentialEpoch, MarkWriteSite::kRelocateRefresh);
		}

		// 4. 推送。gate 已不在 / 已换实例时推不出去,客户端仍然卡着:如实记成 push_gate_gone / push_gate_replaced。
		const relocate_confirm::GatePushResult push = SendTipAndKickToSession(
			entry.sessionId, playerId, entry.gateInstanceId, static_cast<uint32_t>(kEnterSceneFailed));
		relocate_confirm_stats::Inc(stats.outcomes[static_cast<std::size_t>(outcome)]);
		relocate_confirm_stats::Inc(stats.push[static_cast<std::size_t>(push)]);
		const std::string line = "[RelocateConfirm] kick player=" + std::to_string(playerId) +
								 " seq=" + std::to_string(entry.seq) +
								 " outcome=" + relocate_confirm::OutcomeName(outcome) +
								 " push=" + relocate_confirm::GatePushResultName(push) +
								 " credential=" + relocate_confirm::CredentialActionName(credentialAction) +
								 " credential_epoch=" + std::to_string(credentialEpoch) +
								 " session=" + std::to_string(entry.sessionId) +
								 " evidence=" + relocate_confirm::EvidenceName(entry.evidence) +
								 " verdict=" + verdictText +
								 " reply_code=" + std::to_string(entry.replyErrorCode) +
								 " age_ms=" + std::to_string(RelocateAgeMs(entry, now));
		if (outcome == Outcome::kKickVerified && push == relocate_confirm::GatePushResult::kSent)
		{
			// 核实过、也推出去了:改派没成是异常信号,但处置是完整的。
			LOG_WARN << line;
		}
		else
		{
			// 没核实就踢,或者踢线没推出去(客户端仍卡着):都要人看。
			LOG_ERROR << line;
		}
	}

	// 这次核实没读到:1s 后重试(Table::MarkVerifyDeferred),计 verify_deferred。
	// firstAttempt = 本轮核实里第一次没读到 → WARN;之后的重试失败降为 DEBUG(Redis 不通时每条每秒一次,逐次打 WARN
	// 会把故障期日志淹掉;趋势看 verify_deferred)。detail 是可选的排障后缀(ERROR 应答的原文)。
	void DeferRelocateVerify(relocate_confirm::Entry &entry, const char *reason, bool firstAttempt,
							 RelocateClock::time_point now, const std::string &detail = std::string())
	{
		relocate_confirm::Table::MarkVerifyDeferred(entry, now);
		relocate_confirm_stats::Inc(relocate_confirm_stats::Get().verifyDeferred);
		if (firstAttempt)
		{
			LOG_WARN << "[RelocateConfirm] verify deferred player=" << entry.playerId << " seq=" << entry.seq
					 << " reason=" << reason << detail;
		}
		else
		{
			LOG_DEBUG << "[RelocateConfirm] verify deferred player=" << entry.playerId << " seq=" << entry.seq
					  << " reason=" << reason << detail;
		}
	}

	// 核实读(SendRelocateVerify 发出的 EVAL)的应答。全部参数都是回调按值捕获的:条目号、发出时的核实代际、
	// 发出时是不是一次重试(只决定"没读到"的日志级别)。
	void HandleRelocateVerifyReply(Guid playerId, uint64_t seq, uint32_t gen, bool wasRetry, const redisReply *reply)
	{
		using relocate_confirm::Outcome;
		using relocate_confirm::VerifyAction;
		const RelocateClock::time_point now = RelocateClock::now();

		// 入口判据写死:条目号对得上,并且条目仍在核实阶段、仍有命令在途、代际就是这一条(AcceptsVerifyReply)。
		// 不通过的一律丢弃 —— 空应答 / ERROR / 正常应答一视同仁,不计数、不改条目。条目被转去落地(进场路由到了)或
		// 重新开始一轮核实之后,在途旧应答读到的"仍指向源场景"若照常裁决,会踢掉正在本节点载入的合法会话。
		relocate_confirm::Entry *entry = tlsRelocateConfirms.Find(playerId, seq);
		if (entry == nullptr || !relocate_confirm::AcceptsVerifyReply(*entry, gen))
		{
			LOG_DEBUG << "[RelocateConfirm] stale verify reply dropped player=" << playerId << " seq=" << seq
					  << " gen=" << gen << " phase="
					  << (entry != nullptr ? relocate_confirm::PhaseName(entry->phase) : "gone")
					  << " in_flight=" << (entry != nullptr && entry->verifyInFlight ? 1 : 0)
					  << " entry_gen=" << (entry != nullptr ? entry->verifyGen : 0u);
			return;
		}
		entry->verifyInFlight = false;

		// 应答必须是三元素数组、每个元素 STRING 或 NIL(MGET 对缺键 / 类型不对的键给 NIL)。空应答(连接断了)、
		// ERROR(带 shebang 的脚本在只读副本 / MISCONF / OOM 下整体被拒)、其它形状都算这次没读到。
		const char *deferReason = nullptr;
		std::string deferDetail;
		if (reply == nullptr)
		{
			deferReason = "reply_lost";
		}
		else if (reply->type == REDIS_REPLY_ERROR)
		{
			deferReason = "reply_error";
			if (reply->str != nullptr)
			{
				deferDetail = std::string(" err=") + reply->str;
			}
		}
		else if (reply->type != REDIS_REPLY_ARRAY || reply->elements != 3)
		{
			deferReason = "malformed";
		}
		else
		{
			for (std::size_t i = 0; i < 3; ++i)
			{
				const redisReply *element = reply->element[i];
				if (element == nullptr ||
					(element->type != REDIS_REPLY_STRING && element->type != REDIS_REPLY_NIL))
				{
					deferReason = "malformed";
				}
			}
		}
		if (deferReason != nullptr)
		{
			DeferRelocateVerify(*entry, deferReason, /*firstAttempt=*/!wasRetry, now, deferDetail);
			return;
		}
		const redisReply *epochElement = reply->element[0];
		const redisReply *locationElement = reply->element[1];
		const redisReply *handoffElement = reply->element[2];

		// owner_epoch:键不存在(NIL)或不是完整的十进制串一律按 0 —— 0 在凭证判定里就是"不写"。
		uint64_t redisOwnerEpoch = 0;
		if (epochElement->type == REDIS_REPLY_STRING && epochElement->str != nullptr)
		{
			redisOwnerEpoch =
				relocate_confirm::ParseOwnerEpoch(std::string_view(epochElement->str, epochElement->len));
		}

		// location:present 必须按元素类型单独取。ParsePlayerLocationElement 对"键不存在"与"解析失败"都返回 false,
		// 把解析失败当成不存在会让一条写坏的 location 判成 kAbsent(可踢),正确结果是判不清(不踢)。
		// 裁决只看 zone / node / scene:不读 rollback_receipt(它只证明凭这份标记的某条请求被回滚,证明不了本次改派已处理完),
		// 也不看 location 里记的 owner_epoch。
		storage::PlayerLocation location;
		relocate_confirm::PlacementFacts placement;
		placement.expectation = entry->expectation;
		placement.present = locationElement->type == REDIS_REPLY_STRING;
		placement.parsed = placement.present && player_ownership::ParsePlayerLocationElement(locationElement, location);
		placement.locationZoneId = placement.parsed ? location.zone_id() : 0;
		placement.selfZoneId = GetZoneId();
		// PlayerLocation.node_id 是十进制字符串(与归属取证、队伍跟随同一判法)。node_id 只在 zone 内唯一,
		// zone 由 ClassifyPlacement 另比;任一 zone 为 0 时它判"判不清",不把 0 当成本 zone。
		placement.nodeIsSelf = placement.parsed && location.node_id() == std::to_string(GetNodeInfo().node_id());
		placement.locationSceneId = placement.parsed ? location.scene_id() : 0;
		placement.sourceSceneId = entry->sourceSceneId;
		placement.evacuating = tlsEmergencyRelocating;
		const relocate_confirm::Verdict verdict = relocate_confirm::ClassifyPlacement(placement);
		const char *verdictText = relocate_confirm::VerdictName(verdict);

		// 读到"没变 / 已不存在"时现在许不许踢:过了 settleAt,或者没有请求结局未定且证据不是成功应答 / 结果未知。
		const bool requestUnsettled = relocate_confirm::RequestOutcomeUnsettled(*entry);
		const bool kickAllowedNow =
			now >= entry->settleAt || relocate_confirm::MayKickBeforeSettle(entry->evidence, requestUnsettled);
		switch (relocate_confirm::DecideAfterVerify(verdict, kickAllowedNow))
		{
		case VerifyAction::kResolveMoved:
			// 已在别处:有成功应答的算本次改派放行了(granted),否则是别的请求把他放走的。都不踢。
			ResolveRelocate(playerId,
							entry->evidence == relocate_confirm::Evidence::kReplySucceeded ? Outcome::kGranted
																							: Outcome::kMovedElsewhere,
							verdictText, now);
			return;
		case VerifyAction::kResolveIndeterminate:
			ResolveRelocate(playerId, Outcome::kIndeterminate, verdictText, now);
			return;
		case VerifyAction::kAwaitLanding:
			// scene_manager 把他放到了本节点的另一个场景:进场路由还在路上,转去等它。
			relocate_confirm::Table::BeginLanding(*entry, now, /*reentered=*/false, /*loadPending=*/false);
			LOG_INFO << "[RelocateConfirm] landing player=" << playerId << " seq=" << seq << " reentered=0";
			return;
		case VerifyAction::kWaitSettle:
			relocate_confirm::Table::WaitForSettle(*entry);
			relocate_confirm_stats::Inc(relocate_confirm_stats::Get().settleWaits);
			LOG_INFO << "[RelocateConfirm] waiting for settle player=" << playerId << " seq=" << seq
					 << " evidence=" << relocate_confirm::EvidenceName(entry->evidence) << " verdict=" << verdictText
					 << " settle_in_ms=" << RelocateMillis(entry->settleAt - now);
			return;
		case VerifyAction::kKick:
		{
			// 凭证判定用的全是这一刻的事实:同一次读到的 owner_epoch 与 handoff,加上本节点此刻的状态。
			relocate_confirm::CredentialFacts credential;
			credential.verdict = verdict;
			credential.redisOwnerEpoch = redisOwnerEpoch;
			credential.markCarriesEpoch =
				handoffElement->type == REDIS_REPLY_STRING && handoffElement->str != nullptr &&
				relocate_confirm::MarkCarriesEpoch(std::string_view(handoffElement->str, handoffElement->len),
												   redisOwnerEpoch);
			credential.localHolderPossible = tlsEcs.actorRegistry.valid(tlsEcs.GetPlayer(playerId)) ||
											 tlsPendingEnterMap.count(playerId) != 0;
			credential.identityConfirmed = IsNodeIdentityConfirmed();
			credential.devUnsafeCrossNode = IsDevUnsafeCrossNodeHandoff();
			// entry 指针到此为止不再使用:KickRelocatedSession 会把条目取走。
			KickRelocatedSession(playerId, Outcome::kKickVerified, relocate_confirm::DecideCredential(credential),
								 redisOwnerEpoch, verdictText, now);
			return;
		}
		case VerifyAction::kCount:
			break;
		}
		// DecideAfterVerify 不会返回 kCount;真走到这里就让本轮核实自己到期,按"无法核实"处置(不在这里猜)。
		LOG_ERROR << "[RelocateConfirm] verify reply for player " << playerId << " seq=" << seq
				  << " produced no action (verdict=" << verdictText << "); leaving the entry to its verify deadline";
	}

	// 发一次核实读。条目必须在 kVerifying 且没有命令在途,否则 no-op。
	// 命令是只读脚本 relocate_confirm::kLuaReadPlacement(一次读回 owner_epoch / location / handoff,为什么带 shebang、
	// 为什么不删任何键见它的注释)。**必须走 tlsRedis.GetZoneRedis()**:muduo hiredis::Hiredis::command 是
	// redisvAsyncCommand 的薄封装,无队列、无重放,与存盘、标记写、载入前的标记清理同一条 FIFO 连接。改走
	// redis_client.h 那类带重试队列的接口,读数之后的凭证补写可能被重放到某次载入的标记清理之后,等于给活持有者
	// 发一张放行证。
	// settle 之前不许读的情形(结果未知 / 还有请求结局未定,relocate_confirm::MustReadAfterSettle)在这里拦:
	// 不发命令,把下一次核实排到 settleAt。
	// now 由调用方传入,函数内不另取时钟。
	void SendRelocateVerify(Guid playerId, uint64_t seq, RelocateClock::time_point now)
	{
		relocate_confirm::Entry *entry = tlsRelocateConfirms.Find(playerId, seq);
		if (entry == nullptr || entry->phase != relocate_confirm::Phase::kVerifying || entry->verifyInFlight)
		{
			return;
		}
		const relocate_confirm::Evidence evidence = entry->evidence;
		const relocate_confirm::Expectation expectation = entry->expectation;
		if (now < entry->settleAt &&
			relocate_confirm::MustReadAfterSettle(evidence, relocate_confirm::RequestOutcomeUnsettled(*entry)))
		{
			relocate_confirm::Table::WaitForSettle(*entry);
			relocate_confirm_stats::Inc(relocate_confirm_stats::Get().settleWaits);
			LOG_INFO << "[RelocateConfirm] waiting for settle player=" << playerId << " seq=" << seq
					 << " evidence=" << relocate_confirm::EvidenceName(evidence)
					 << " verdict=" << relocate_confirm::kVerdictUnread
					 << " settle_in_ms=" << RelocateMillis(entry->settleAt - now);
			return;
		}

		// 上一次没读到、这次是重试:只决定"又没读到"时的日志级别。MarkVerifySent 会清掉这个位,先抄下来。
		const bool wasRetry = entry->retryPending;
		auto &redis = tlsRedis.GetZoneRedis();
		if (!redis || !redis->connected())
		{
			DeferRelocateVerify(*entry, "redis_unavailable", /*firstAttempt=*/!wasRetry, now);
			return;
		}
		const uint32_t gen = relocate_confirm::Table::MarkVerifySent(*entry);
		const std::string ownerEpochKey = player_ownership::OwnerEpochRedisKey(playerId);
		const std::string locationKey = player_ownership::LocationRedisKey(playerId);
		const std::string handoffKey = player_ownership::HandoffRedisKey(playerId);
		// 不能只判 connected():连接正在断开时它仍为真,而命令已经发不出去(理由见 SendHandoffWithdraw 上方的注释),
		// command() 的返回值必须一并检查。脚本串作为一个 %s 参数传入是安全的(同 SendHandoffWithdraw)。
		const int ret = redis->command(
			[playerId, seq, gen, wasRetry](hiredis::Hiredis *, redisReply *reply)
			{
				HandleRelocateVerifyReply(playerId, seq, gen, wasRetry, reply);
			},
			"EVAL %s 3 %s %s %s", relocate_confirm::kLuaReadPlacement, ownerEpochKey.c_str(), locationKey.c_str(),
			handoffKey.c_str());
		if (ret != REDIS_OK)
		{
			// 命令没发出去,回调不会来。command() 之后不沿用之前的指针,重新对号;仍是刚才那一条才记推迟。
			relocate_confirm::Entry *unsent = tlsRelocateConfirms.Find(playerId, seq);
			if (unsent != nullptr && relocate_confirm::AcceptsVerifyReply(*unsent, gen))
			{
				DeferRelocateVerify(*unsent, "dispatch_failed", /*firstAttempt=*/!wasRetry, now);
			}
			return;
		}
		// 重试的那几次降为 DEBUG(同 DeferRelocateVerify):Redis 连得上但脚本被拒时(只读副本 / MISCONF / 切主后的旧主)
		// 每条条目每秒重发一次,逐次打 INFO 会把故障期日志淹掉。
		if (wasRetry)
		{
			LOG_DEBUG << "[RelocateConfirm] verify player=" << playerId << " seq=" << seq << " gen=" << gen
					  << " evidence=" << relocate_confirm::EvidenceName(evidence)
					  << " expectation=" << relocate_confirm::ExpectationName(expectation);
		}
		else
		{
			LOG_INFO << "[RelocateConfirm] verify player=" << playerId << " seq=" << seq << " gen=" << gen
					 << " evidence=" << relocate_confirm::EvidenceName(evidence)
					 << " expectation=" << relocate_confirm::ExpectationName(expectation);
		}
	}

	// 有了证据:转入(新一轮)核实并立刻试着读一次。应答认领、传输失败认领、改派没发出去、落地两类共用。
	// 条目已不在时 no-op。读不读、何时读由 SendRelocateVerify 决定(settle 之前可能只是排到 settleAt)。
	void StartRelocateVerify(Guid playerId, uint64_t seq, relocate_confirm::Evidence evidence,
							 relocate_confirm::Expectation expectation, RelocateClock::time_point now)
	{
		relocate_confirm::Entry *entry = tlsRelocateConfirms.Find(playerId, seq);
		if (entry == nullptr)
		{
			return;
		}
		relocate_confirm::Table::BeginVerify(*entry, now, evidence, expectation);
		SendRelocateVerify(playerId, seq, now);
	}

	// kLanding 条目的每拍检查:实体建出来没有、载入是不是被放弃了、等没等过头(relocate_confirm::DecideLanding)。
	// "实体已建出且会话一致"就按 landed_here 结清 —— 它不保证玩家进了场景(目标场景在路由到达前被销毁时,
	// EnterScene 只回一条失败 tip 就返回,实体与会话都已在),这是进场链自己的既有缺口,不在这里收。
	void CheckRelocateLanding(Guid playerId, uint64_t seq, RelocateClock::time_point now)
	{
		using relocate_confirm::LandingAction;
		const relocate_confirm::Entry *entry = tlsRelocateConfirms.Find(playerId, seq);
		if (entry == nullptr || entry->phase != relocate_confirm::Phase::kLanding)
		{
			return;
		}
		const auto playerEntity = tlsEcs.GetPlayer(playerId);
		const bool entityValid = tlsEcs.actorRegistry.valid(playerEntity);
		bool sessionMatches = false;
		if (entityValid)
		{
			const auto *snapshot = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(playerEntity);
			sessionMatches = snapshot != nullptr && snapshot->gate_session_id() == entry->sessionId;
		}
		const bool loadPending = tlsPendingEnterMap.count(playerId) != 0;
		// entry 指针在 switch 之后不再使用:每一支要么结清条目,要么重新 Find。
		switch (relocate_confirm::DecideLanding(entityValid, sessionMatches, entry->reentered, loadPending,
												now >= entry->deadline))
		{
		case LandingAction::kWait:
			return;
		case LandingAction::kResolveLanded:
			ResolveRelocate(playerId, relocate_confirm::Outcome::kLandedHere, relocate_confirm::kVerdictUnread, now);
			return;
		case LandingAction::kResolveSuperseded:
			ResolveRelocate(playerId, relocate_confirm::Outcome::kSuperseded, relocate_confirm::kVerdictUnread, now);
			return;
		case LandingAction::kVerifyAbandoned:
			// 进场路由到过、载入却已不在途(继承标记核对不过 / 载入失败 / 会话在载入中取消):那几条出口只回 tip、不踢线,
			// 踢不踢由这里核实后决定 —— 读到仍指向本节点才踢。
			StartRelocateVerify(playerId, seq, relocate_confirm::Evidence::kLandingAbandoned,
								relocate_confirm::Expectation::kAtThisNode, now);
			return;
		case LandingAction::kVerifyTimeout:
			StartRelocateVerify(playerId, seq, relocate_confirm::Evidence::kLandingTimeout,
								relocate_confirm::Expectation::kAtThisNode, now);
			return;
		case LandingAction::kCount:
			return;
		}
	}

	// DispatchEnterSceneReply 在"实体已不在"分支里问一句:这条应答是不是本次改派的。返回 true = 已认领(调用方直接
	// 返回),false = 不是待确认表的(调用方照原样记一条 INFO)。
	// 认领按关联号精确匹配;回显号为 0(旧版 scene_manager)才退回按 player_id(relocate_confirm::DecideReplyClaim)。
	// 认领之后**只触发核实,从不直接定案**:成功不直接结清(可能是把他放回了正在排空的同一个场景、路由还在路上),
	// 拒绝不直接踢(推路由失败那一类拒绝在回滚没成时等于已放行到别处)。
	// 应答回显的 owner_epoch_after_rollback 只进日志,绝不采纳。
	bool ClaimRelocateEnterSceneReply(Guid playerId, uint64_t replyTag, const ::scene_manager::EnterSceneResponse &resp)
	{
		using relocate_confirm::ClaimAction;
		relocate_confirm::Entry *entry = tlsRelocateConfirms.FindPlayer(playerId);
		const ClaimAction claim = relocate_confirm::DecideReplyClaim(
			entry != nullptr, entry != nullptr ? entry->correlationId : 0,
			entry != nullptr ? entry->phase : relocate_confirm::Phase::kMarkWriting, replyTag);
		if (entry == nullptr || claim == ClaimAction::kNotMine || claim == ClaimAction::kCount)
		{
			return false;
		}
		const uint64_t seq = entry->seq;
		// 号精确匹配的认领才记"本次请求带应答体的完成通知已到"与它的拒绝码(只认第一次,见 Table::MarkCompletion)。
		// 回显号为 0 的退路认领归属不精确(那条应答可能是更早请求的),不记:条目继续按"还有请求结局未定"处理,
		// 只会多等到 settle、读不到时不盲踢。
		if (replyTag != 0)
		{
			relocate_confirm::Table::MarkCompletion(*entry, resp.error_code());
		}
		if (claim == ClaimAction::kIgnore)
		{
			// 条目已不在等应答:还没发出(只可能是退路认领)/ 已有证据在核实 / 已落回本节点。结论由已排好的路径给。
			relocate_confirm_stats::Inc(relocate_confirm_stats::Get().replyIgnored);
			LOG_INFO << "[RelocateConfirm] reply ignored player=" << playerId << " seq=" << seq << " corr=" << replyTag
					 << " phase=" << relocate_confirm::PhaseName(entry->phase) << " error_code=" << resp.error_code();
			return true;
		}
		relocate_confirm_stats::Inc(relocate_confirm_stats::Get().replyVerified);
		const relocate_confirm::Evidence evidence = relocate_confirm::EvidenceForReply(resp.error_code());
		if (resp.has_redirect())
		{
			// 改派的目标 zone 就是本 zone,不该被重定向。只留痕,仍按应答码当证据(error_code 为 0 即成功),由核实定案。
			LOG_WARN << "[RelocateConfirm] reply for player=" << playerId << " seq=" << seq << " corr=" << replyTag
					 << " carries a cross-zone redirect, which a same-zone relocate should never get; verifying anyway";
		}
		LOG_INFO << "[RelocateConfirm] reply player=" << playerId << " seq=" << seq << " corr=" << replyTag
				 << " error_code=" << resp.error_code()
				 << " epoch_after_rollback=" << resp.owner_epoch_after_rollback()
				 << " evidence=" << relocate_confirm::EvidenceName(evidence);
		// 必须排在 MarkCompletion 之后:SendRelocateVerify 要读 completionSeen 决定是不是等到 settle 再读。
		StartRelocateVerify(playerId, seq, evidence, relocate_confirm::Expectation::kAtSource, RelocateClock::now());
		return true;
	}

	// DispatchEnterSceneTransportFailure 在"实体已不在"分支里问一句:这次传输失败是不是本次改派的。返回值同上。
	// 只按号精确匹配,没有 player_id 退路(relocate_confirm::DecideTransportFailureClaim)。
	// 传输失败 = **结果未知**:deadline 到期之后 scene_manager 的 handler 仍在跑,可能照样铸造、推路由、回滚。
	// 所以认领后只是换一种证据(kTransportFailed)转核实,而且要等到 settle 才读 —— 此刻读到窗口中间的"已在别处"
	// 会被结清为不踢,随后的回滚把 location 写回源场景,会话就永远挂着。
	bool ClaimRelocateTransportFailure(Guid playerId, uint64_t requestTag, const std::string &reason)
	{
		using relocate_confirm::ClaimAction;
		relocate_confirm::Entry *entry = tlsRelocateConfirms.FindPlayer(playerId);
		const ClaimAction claim = relocate_confirm::DecideTransportFailureClaim(
			entry != nullptr, entry != nullptr ? entry->correlationId : 0,
			entry != nullptr ? entry->phase : relocate_confirm::Phase::kMarkWriting, requestTag);
		if (entry == nullptr || claim == ClaimAction::kNotMine || claim == ClaimAction::kCount)
		{
			return false;
		}
		const uint64_t seq = entry->seq;
		relocate_confirm_stats::Inc(relocate_confirm_stats::Get().transportFailed);
		// **不**记 MarkCompletion:传输失败是"结果未知",不是结局。记成"已完成"的话,条目随后若被一条外来的同会话
		// 路由带进落地、载入又被放弃,重新核实时证据已换成 kLandingAbandoned,RequestOutcomeUnsettled 又为假,
		// "settle 之前不读、不提前踢、读不到不盲踢"三道保护就全没了 —— 而 scene_manager 的 handler 此刻可能正要把他
		// 放到别处。completionSeen 保持为假:落地路径上的核实同样等到 settle,读不到时放弃而不是盲踢。
		if (claim == ClaimAction::kIgnore)
		{
			// 条目已不在等完成通知(例如进场路由已先把他带回本节点):只记一笔,结论由已排好的路径给。
			LOG_INFO << "[RelocateConfirm] transport failure ignored player=" << playerId << " seq=" << seq
					 << " corr=" << requestTag << " phase=" << relocate_confirm::PhaseName(entry->phase) << " ("
					 << reason << ")";
			return true;
		}
		LOG_WARN << "[RelocateConfirm] transport failure player=" << playerId << " seq=" << seq << " corr=" << requestTag
				 << " (" << reason << "); outcome unknown, verifying after settle";
		StartRelocateVerify(playerId, seq, relocate_confirm::Evidence::kTransportFailed,
							relocate_confirm::Expectation::kAtSource, RelocateClock::now());
		return true;
	}
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
		// 载入放弃:A2′ 若已删掉(或可能删掉)epoch == N 的标记,补写回去(M7)。
		const InheritClearState inheritClear = std::move(pendingIt->second.inheritClear);
		tlsPendingEnterMap.erase(pendingIt);
		AbandonInheritClear(playerId, inheritClear, "load failed with a Redis error");
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

	// If the session was erased during async load (e.g. client disconnected
	// and ExitGame arrived before load completed), skip entity creation.
	const SessionId pendingSessionId = pendingIt->second.enterInfo.session_id();
	if (pendingSessionId != 0 && SessionMap().find(pendingSessionId) == SessionMap().end())
	{
		LOG_INFO << "HandlePlayerAsyncLoaded: session " << pendingSessionId
		         << " cancelled during async load for player " << playerId << ", skipping";
		// 载入放弃:A2′ 若已删掉(或可能删掉)epoch == N 的标记,补写回去(M7)。
		const InheritClearState inheritClear = std::move(pendingIt->second.inheritClear);
		tlsPendingEnterMap.erase(pendingIt);
		AbandonInheritClear(playerId, inheritClear, "session cancelled during load");
		return;
	}

	// A2′ 闸门(§12.6.3 第三步,M10 三态):只认"针对本 ctx.ownerEpoch 的那一次清理返回了非 -1"。
	// 不对 enter_gs_type == 0 fail-open:首登与重登走同一道闸。
	{
		auto &inheritClear = pendingIt->second.inheritClear;
		switch (exit_release_mark::DecideInheritGate(pendingIt->second.ownerEpoch, inheritClear.targetEpoch,
													 inheritClear.phase))
		{
		case exit_release_mark::InheritGateDecision::kProceed:
			break;
		case exit_release_mark::InheritGateDecision::kWait:
			// 应答未到 / 等重发:暂存载入结果,由应答(或重发后的应答)回来重走本函数;截止时刻到了由
			// RetryInheritedMarkClears 拒绝。待入场条目保留,期间的重连照常覆盖上下文、沿用清理状态。
			inheritClear.stashedLoad = std::make_shared<const PlayerAllData>(message);
			LOG_INFO << "HandlePlayerAsyncLoaded: player " << playerId << " loaded; waiting for the inherited-mark"
					 << " clear of owner_epoch " << pendingIt->second.ownerEpoch << " before creating the entity";
			return;
		case exit_release_mark::InheritGateDecision::kRefuse:
		default:
			RefuseInheritedEntry(playerId, "no inherited-mark clear confirmed for this owner_epoch");
			return;
		}
	}

	PlayerEnterContext ctx = std::move(pendingIt->second);
	tlsPendingEnterMap.erase(pendingIt);

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
			// 先抄后摘(与 FinishExitAfterPersist 同一写法):交接若已发起(HandleExitGameNode 的 M4 分支在
			// requestedAtMs != 0 时不内联收尾、等的就是这次落地),"{markEpoch}:{requestedAtMs}" 标记已写,
			// 组件一摘 FinishExitAfterPersist 就再也看不到它,必须在这里撤回;否则标记留到 300s TTL,
			// 玩家同节点重连后的下一次跨节点换图会凭它免存盘过换手门(回档)。理由详见 FinishExitAfterPersist,
			// 那里也说明了为什么撤回删不到 scene_manager 回滚转写出来的 "E+2:t" 却无害、为什么不许改成无条件 DEL。
			const auto &travelIntent = tlsEcs.actorRegistry.get<PlayerTravelHandoffComp>(playerEntity);
			const uint64_t handoffMarkEpoch = travelIntent.markEpoch;
			const uint64_t handoffRequestedAtMs = travelIntent.requestedAtMs;
			const bool handoffMarkWritten = handoffMarkEpoch != 0 && handoffRequestedAtMs != 0;
			LOG_WARN << "HandlePlayerAsyncSaved: player " << playerId
					 << " is exiting; dropping in-flight zone travel intent (exit wins)"
					 << " handoff_mark_written=" << handoffMarkWritten;
			travel_handoff_stats::Inc(travel_handoff_stats::Get().exitWins);
			tlsEcs.actorRegistry.remove<PlayerTravelHandoffComp>(playerEntity);
			tlsEcs.actorRegistry.remove<PlayerFrozenComp>(playerEntity);
			if (handoffMarkWritten)
			{
				WithdrawHandoffMark(playerId, handoffMarkEpoch, handoffRequestedAtMs, "exit wins");
				// 交接组件已摘,FinishExitAfterPersist 看不到它了:记在意图组件上,让 A1′ 不写断线释放标记(M11)。
				if (auto *exitIntent = tlsEcs.actorRegistry.try_get<PlayerExitIntentComp>(playerEntity))
				{
					exitIntent->travelHandoffMarkIssued = true;
				}
			}
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
			// 每次退出只打一条(意图组件上的 leaseOverrunWarned):Z1 修复后一次退出可能落地多次,
			// 不去重的话帮会值班 LogQL 的计数会从"事件数"变成"落地数"(§12.6.9 承诺保留的是原文与口径)。
			// 意图组件缺失(成对约定被破坏,下面 fail-closed)时无处去重,照旧打。
			auto *warnIntent = tlsEcs.actorRegistry.try_get<PlayerExitIntentComp>(playerEntity);
			const bool alreadyWarned = warnIntent != nullptr && warnIntent->leaseOverrunWarned;
			if (elapsedMs > kReconnectLeaseMs && !alreadyWarned)
			{
				if (warnIntent != nullptr)
				{
					warnIntent->leaseOverrunWarned = true;
				}
				LOG_WARN << "HandlePlayerAsyncSaved: save outran reconnect lease for player "
						 << playerId << " — elapsed_ms=" << elapsedMs
						 << " lease_ms=" << kReconnectLeaseMs
						 << " (cross-node re-login during this window may have read stale data)";
			}
		}

		// ── Z1 修复(cross-zone-scene-travel.md §12.6.3 第一步)──────────────────────────────
		// 旧判据"快照里的会话仍在 SessionMap 里、映射到本玩家 → 重连已取代退出"是错的:HandleExitGameNode
		// 从不解绑会话,查到的就是退出会话本身,于是每一次真写盘的正常断线都被判成"被取代",实体成了
		// 僵尸(2026-09-06 实跑日志 "ignoring stale UnregisterPlayer … live session <退出会话>")。
		// 判定顺序:1 被取代 → 2 意图缺失(fail-closed)→ 3 收敛才销毁 → 4 不收敛则重存。
		auto *exitIntent = tlsEcs.actorRegistry.try_get<PlayerExitIntentComp>(playerEntity);

		// 1. 被取代:当前会话 ≠ 退出那一刻的会话,且它在 SessionMap 里映射到本玩家。正常重连在 EnterScene
		//    第 0 步就摘了标记、走不到这里,这是纵深防御。保留实体、摘两件退出标记;快照照常在函数末尾
		//    更新(这份字节确实落了盘,旧实现在这里 return、连快照都不更新)。
		if (exitIntent != nullptr)
		{
			SessionId currentSession = kInvalidSessionId;
			if (const auto *snapshot = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(playerEntity))
			{
				currentSession = snapshot->gate_session_id();
			}
			const auto sessionIt = SessionMap().find(currentSession);
			const bool mappedToPlayer = sessionIt != SessionMap().end() && sessionIt->second == playerId;
			if (player_exit::IsSupersedingSession(currentSession, exitIntent->sessionAtExit, mappedToPlayer))
			{
				LOG_WARN << "HandlePlayerAsyncSaved: exit of player " << playerId
						 << " superseded by a newer session " << currentSession
						 << " (session at exit " << exitIntent->sessionAtExit
						 << ", cause=" << player_exit::ExitCauseName(exitIntent->cause)
						 << "); keeping the entity (metric=exit_superseded)";
				exit_persist_stats::Inc(exit_persist_stats::Get().superseded);
				tlsEcs.actorRegistry.remove<UnregisterPlayer, PlayerExitIntentComp>(playerEntity);
				exitIntent = nullptr;
			}
		}
		// 2. 意图缺失:UnregisterPlayer 只在 HandleExitGameNode 里与意图组件成对挂,缺失说明成对约定被破坏。
		//    分不清是不是被取代、也不知道内存与盘上差多少,销毁可能踢掉活玩家或丢掉差额(§12.6.4 M9)。
		//    fail-closed 沿用旧行为:保留实体、摘 UnregisterPlayer(停机 drain 会对它重新发起退出)。
		else
		{
			LOG_ERROR << "HandlePlayerAsyncSaved: exiting player " << playerId
					  << " has UnregisterPlayer but no PlayerExitIntentComp; keeping the entity and dropping the"
					  << " unregister tag instead of destroying it (fail-closed, metric=exit_intent_missing)";
			exit_persist_stats::Inc(exit_persist_stats::Get().intentMissing);
			tlsEcs.actorRegistry.remove<UnregisterPlayer>(playerEntity);
		}

		if (exitIntent != nullptr)
		{
			// 3. 收敛判定(M1 "比对后才销毁"):落地的 payload 未必等于此刻的内存 —— 存盘在途期间的改动
			//    (停机期间照收的客户端操作、战斗结算,Redis ERROR 时的新旧颠倒)今天靠僵尸的周期存盘补回,
			//    改成"落地即销毁"会让它们永久丢失。所以只在"刚落地的就是当前内存"时才销毁。
			PlayerAllData current;
			MarshalPlayerForSave(playerEntity, playerId, current);
			const bool payloadCurrent = player_exit::IsPersistedPayloadCurrent(message, current);
			// 在落地回调里这个值恒为 false(redis_client.h HasUnsettledSave 的使用约束:有排队值时 OnSaved 直接发新值、
			// 不回调),收敛实际只看 payloadCurrent。仍然传入,只为与回调之外的收尾口(HandleExitGameNode 快路径 /
			// rekick)共用 DecideAfterPersist 这一把尺子;日志里的 unsettled_save= 在这里恒为 0。
			const bool unsettledSave = HasUnsettledPlayerSave(playerId);
			switch (player_exit::DecideAfterPersist(payloadCurrent, unsettledSave, exitIntent->resaveRounds,
													player_exit::kMaxExitResaveRounds))
			{
			case player_exit::AfterPersistDecision::kFinish:
				LOG_INFO << "Player marked for unregistration: " << playerId
						 << " (cause=" << player_exit::ExitCauseName(exitIntent->cause)
						 << ", resave_rounds=" << static_cast<uint32_t>(exitIntent->resaveRounds) << ")";
				FinishExitAfterPersist(playerId);
				return; // 实体已销毁,快照无处可挂
			case player_exit::AfterPersistDecision::kResave:
			{
				// 4. 不收敛:先把快照更新为刚落地的这份(它确实已在盘上),再存一次当前内存;保留
				//    UnregisterPlayer 与意图组件,等下一次落地再判。
				++exitIntent->resaveRounds;
				exit_persist_stats::Inc(exit_persist_stats::Get().resave);
				LOG_INFO << "HandlePlayerAsyncSaved: exiting player " << playerId
						 << " changed while its exit save was in flight (payload_current=" << payloadCurrent
						 << " unsettled_save=" << unsettledSave << "); re-saving before destroying, round "
						 << static_cast<uint32_t>(exitIntent->resaveRounds) << "/"
						 << static_cast<uint32_t>(player_exit::kMaxExitResaveRounds) << " (metric=exit_resave)";
				tlsEcs.actorRegistry.get_or_emplace<PlayerLastPersistedSnapshotComp>(playerEntity).Replace(message);
				if (SavePlayerToRedis(playerEntity))
				{
					return; // 等这次存盘的落地回调
				}
				// 没写盘:快路径判"盘上已是同一份",或交接已发起不得再写。还有未落地的存盘就等它的落地回调;
				// 否则盘上此刻就是当前内存,直接收尾。(本分支在落地回调里,按 redis_client.h 的约束下面的查询
				// 恒为 false;留着是防御:将来若有回调之外的调用方走到这里,仍然 fail-closed。)
				if (HasUnsettledPlayerSave(playerId))
				{
					LOG_INFO << "HandlePlayerAsyncSaved: exiting player " << playerId
							 << " has an unsettled save; finishing exit when it lands";
					return;
				}
				LOG_INFO << "Player marked for unregistration: " << playerId
						 << " (converged on re-save, cause=" << player_exit::ExitCauseName(exitIntent->cause) << ")";
				FinishExitAfterPersist(playerId);
				return;
			}
			case player_exit::AfterPersistDecision::kRetainExhausted:
			default:
				// 重存到上限仍不收敛:还有别的东西在持续改这个实体。fail-closed 保留实体与两件退出标记,
				// 不销毁、不丢差额;IsSaveInFlight 保持为真。出口:之后的任何一次落地(例如周期存盘)只要收敛了
				// 照样收尾;再来一次退出请求(停机 exitAllPlayers / s2s LeaveScene / 排空)会补发一次存盘
				// (HandleExitGameNode 的"已在退出中"分支);都不收敛时停机由 Node 的有界 drain 看门狗裁决;
				// 保留期间玩家若在别处玩过又被派回本节点,由 DiscardDeposedEntityOnReentry 丢弃重载、不复用。
				// 快照照常在函数末尾更新。
				LOG_ERROR << "HandlePlayerAsyncSaved: exiting player " << playerId << " did not converge after "
						  << static_cast<uint32_t>(exitIntent->resaveRounds)
						  << " re-save round(s) (payload_current=" << payloadCurrent
						  << " unsettled_save=" << unsettledSave
						  << "); keeping the entity, the node drain watchdog decides (metric=exit_resave_capped)";
				exit_persist_stats::Inc(exit_persist_stats::Get().resaveExhausted);
				break;
			}
		}
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
	//    PlayerExitIntentComp 与 UnregisterPlayer 成对摘(player_exit_intent.h 的成对约定),见 CancelExitOnReconnect。
	CancelExitOnReconnect(player);

	// 0.5 归属:放在场景查找之前 —— 归属讲的是"这份数据归谁、谁持有",与放没放进场景无关,
	//     实体只要在本节点存在、会被存盘,就必须带着 Go 这次路由决策给的值。
	//     0 一律不覆盖(旧版 gate / scene_manager 未填,或首登时 Init 刚挂的零值组件);
	//     epoch 取 max:见 PlayerOwnerEpochComp 的说明。
	//     非 per-tick 路径,get_or_emplace 合规(AGENTS §7.5)。
	//
	//     先记下路由到达之前缓存的 epoch:下面按 max 覆盖之后,就分不出"这条路由带来的 epoch 与我
	//     手里的是同一代"与"路由带来了更新的一代"了,而 3.2 步只认前者。
	uint64_t epochBeforeRoute = 0;
	if (const auto *cachedEpoch = tlsEcs.actorRegistry.try_get<PlayerOwnerEpochComp>(player))
	{
		epochBeforeRoute = cachedEpoch->epoch;
	}
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

	// 3.2 同 zone 交接"重发后落回本节点"的就地收尾:同样不等 gRPC 应答。
	//     交接重发的 EnterScene 若被 scene_manager 重新挑频道挑回了本节点(同物理节点不铸造 epoch),
	//     路由走到这里时实体还带着 PlayerFrozenComp:玩家人已在新场景,输入却全被冻结闸丢弃。应答
	//     正常到达时只差几毫秒;应答丢失(scene_manager 在路由 ACK 之后重启 / 断连:传输失败对交接只记日志、
	//     不当证据,见 DispatchEnterSceneTransportFailure)时要冻到应答看门狗,看门狗还会按"失败"回一条假的
	//     kEnterSceneFailed。
	//     判据:交接已发起、目标是本 zone 且没指定场景实例、路由带来的 epoch 非 0 且与路由到达前缓存的
	//     是同一代。
	//       * 指定了实例(sceneId != 0)的交接落不回本节点:实例所在节点是固定的,它在本节点的话
	//         第一条请求就不会被 18 拒。此时到达的同代路由不是这次重发的结果,不动交接;
	//       * 更新的一代到不了这里:scene_handler.cpp 已先走 DiscardStaleHandoffEntity 销毁重载;
	//       * 更旧的一代是乱序到达的旧路由,不是这次重发的结果,不动交接;
	//       * 跨 zone 传送不在此列:路由落到本节点说明不了那次传送的去留,仍由应答 / 看门狗裁决。
	//     不直接 AbortTravelHandoff:同一代的路由也可能是交接之前那次同节点换图迟到的路由,或顶号重连
	//     的同落点路由,此刻重发的那条请求也许正在 scene_manager 里铸造 epoch —— 未经核实就解冻,
	//     等于同一名玩家在两处同时活着。走 ResolveTravelOutcome:原子删掉本族标记再读 epoch 与 location,按判定表
	//     裁决 —— 没变就静默解冻(证据 kSucceeded 取的正是"归属没动 = 换图已就地完成,不回失败 tip"这层含义);
	//     变了且回执是本次标记原文就采纳解冻(B5);回执对不上交叉校验就按"标记已发出"收口(B6);其余按已放行
	//     销毁(同 zone,不踢线)。之后到达的应答 / 看门狗因交接意图已摘而成为 no-op。
	if (const auto *travel = tlsEcs.actorRegistry.try_get<PlayerTravelHandoffComp>(player);
		travel != nullptr && travel->requestedAtMs != 0 && travel->targetZoneId == GetZoneId() &&
		travel->sceneId == 0 && ctx.ownerEpoch != 0 && ctx.ownerEpoch == epochBeforeRoute)
	{
		LOG_INFO << "EnterScene: player " << playerId
				 << " was placed on this node while its same-zone handoff is in flight (owner_epoch "
				 << ctx.ownerEpoch << " unchanged); verifying and settling the handoff in place";
		ResolveTravelOutcome(playerId, travel->requestedAtMs, "placed on this node by route",
							 travel_outcome::Evidence::kSucceeded);
	}

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
	//    换会话(RECONNECT / REPLACE)且战斗在途时推重连提示,客户端据此补签重建直连后补拉
	//    (turn-based §22 D72)。
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

void PlayerLifecycleSystem::StopMotionForExit(entt::entity player)
{
	// 只改已有组件、不 emplace:没有运动学组件的实体(测试宿主、非移动实体)什么都不做。
	// 两者都清:MovementSystem 积分 Velocity(不排除 UnregisterPlayer),MovementAccelerationSystem 积分
	// (Velocity + Acceleration)。与各 tick 系统排除 PlayerFrozenComp 同一个道理 —— 要落盘的那一刻起
	// Transform 不能再被逐帧推进;这里不去改 movement.cpp 的排除目录,而是在退出链自己的入口把矢量清掉。
	if (auto *velocity = tlsEcs.actorRegistry.try_get<Velocity>(player))
	{
		if (velocity->x() != 0.0 || velocity->y() != 0.0 || velocity->z() != 0.0)
		{
			velocity->set_x(0.0);
			velocity->set_y(0.0);
			velocity->set_z(0.0);
			// 与 MovementSystem 撞墙清速同一写法:重连后同步给客户端的是 0 速度。
			SetActorBaseAttributesS2CAttrDirtyBit(player, ActorBaseAttributesS2C::kVelocityFieldNumber);
		}
	}
	if (auto *acceleration = tlsEcs.actorRegistry.try_get<Acceleration>(player))
	{
		acceleration->set_x(0.0);
		acceleration->set_y(0.0);
		acceleration->set_z(0.0);
	}
}

void PlayerLifecycleSystem::CancelExitOnReconnect(entt::entity player)
{
	if (tlsEcs.actorRegistry.any_of<UnregisterPlayer>(player))
	{
		const auto *guid = tlsEcs.actorRegistry.try_get<Guid>(player);
		tlsEcs.actorRegistry.remove<UnregisterPlayer>(player);
		LOG_INFO << "EnterScene: cancelled pending unregistration for reconnected player "
				 << (guid != nullptr ? *guid : kInvalidGuid);
	}
	// remove 对不存在的组件是 no-op。意图组件无条件摘:成对约定被破坏(只剩意图)时也在这里收干净。
	tlsEcs.actorRegistry.remove<PlayerExitIntentComp>(player);
}

void PlayerLifecycleSystem::HandleExitGameNode(entt::entity player, ExitCause cause)
{
	// valid() 必须在 try_get 之前:对已销毁的实体调 try_get 是 entt 的未定义行为,
	// 旧顺序是先 try_get 再判 valid,等于先踩了再检查。
	if (!tlsEcs.actorRegistry.valid(player))
	{
		LOG_ERROR << "HandleExitGameNode: Player entity is not valid";
		return;
	}

	EnsureExitStatsTimer();

	const auto* g = tlsEcs.actorRegistry.try_get<Guid>(player);
	LOG_INFO << "HandleExitGameNode: Player " << (g ? *g : 0) << " is exiting the scene node"
			 << " (cause=" << player_exit::ExitCauseName(cause) << ")";

	if (tlsEcs.actorRegistry.all_of<UnregisterPlayer>(player))
	{
		// 已在退出中:不重挂,只合并原因(原因保持第一次的,按规则粘性置位 releaseMarkSuppressed)。
		// devBypassSuppressesTransfer 取 scene 侧 dev 开关(SCENE_DEV_UNSAFE_CROSS_NODE_HANDOFF,默认关 = 生产口径,
		// 须与 scene_manager 的 AllowUnsafeCrossNodeHandoff 同步打开,M6)。
		if (auto *intent = tlsEcs.actorRegistry.try_get<PlayerExitIntentComp>(player))
		{
			player_exit::MergeExitCause(*intent, cause, IsDevUnsafeCrossNodeHandoff());
			LOG_INFO << "Player " << (g ? *g : 0) << " is already marked for unregistration"
					 << " (cause=" << player_exit::ExitCauseName(intent->cause)
					 << ", incoming=" << player_exit::ExitCauseName(cause)
					 << ", release_mark_suppressed=" << intent->releaseMarkSuppressed << ")";

			// 重存到上限仍不收敛、被保留的实体:再来一次退出请求时补发一次存盘(轮次清零,重新计)。
			// 停机时 RedisSystem 已取消周期存盘、drain 只对不在退出中的实体重发退出,exitAllPlayers 这一次
			// 就是它最后的落盘机会;平时的 s2s LeaveScene / 排空 / 疏散同样给它一个出口。有在途 / 排队的存盘
			// 时不插手,等它落地由退出分支照常判定。
			if (g != nullptr &&
				player_exit::ShouldRekickExhaustedExit(intent->resaveRounds, player_exit::kMaxExitResaveRounds,
													   HasUnsettledPlayerSave(*g)))
			{
				const Guid retainedPlayerId = *g;
				intent->resaveRounds = 0;
				exit_persist_stats::Inc(exit_persist_stats::Get().exhaustedRekick);
				LOG_WARN << "HandleExitGameNode: player " << retainedPlayerId
						 << " was retained after exhausting its exit re-saves; saving once more"
						 << " (incoming cause=" << player_exit::ExitCauseName(cause) << ", metric=exit_exhausted_rekick)";
				StopMotionForExit(player); // 纵深防御:退出发起时已清过,这里再清一次不改变语义
				if (!SavePlayerToRedis(player) && !HasUnsettledPlayerSave(retainedPlayerId))
				{
					// 没写盘(快路径判"盘上已是同一份",或交接已发起不得再写)且没有未落地的存盘:
					// 盘上就是当前内存,直接收尾。intent / g 此后悬空,不再使用。
					LOG_INFO << "Player marked for unregistration: " << retainedPlayerId
							 << " (converged on exhausted re-kick)";
					FinishExitAfterPersist(retainedPlayerId);
				}
			}
		}
		else
		{
			// 成对约定被破坏(不该发生)。不在这里补挂:补挂的意图抄不到退出那一刻的会话,只会让落地回调
			// 误判;留给落地回调按"意图缺失"fail-closed 处理(计 exit_intent_missing)。
			LOG_ERROR << "Player " << (g ? *g : 0)
					  << " is already marked for unregistration but has no PlayerExitIntentComp";
		}
		return;
	}

	auto& unregisterTag = tlsEcs.actorRegistry.emplace<UnregisterPlayer>(player);
	unregisterTag.set_logout_initiated_ms(TimeSystem::NowMillisecondsUTC());

	// 与 UnregisterPlayer 成对挂(见 player_exit_intent.h)。sessionAtExit 必须此刻抄:落地回调要凭它
	// 区分"退出会话本身还在 SessionMap 里"(不算取代,Z1)与"已被一个更新的会话取代"。
	{
		PlayerExitIntentComp intent;
		if (const auto *sessionSnapshot = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(player))
		{
			intent.sessionAtExit = sessionSnapshot->gate_session_id();
		}
		intent.cause = cause;
		// 会话是不是已经死了(粘性;之后再来的客户端断线由 MergeExitCause 置位)。改派票据据此作废。
		intent.clientDisconnected = cause == ExitCause::kClientDisconnect;
		// 源场景必须此刻抄:下面的 DetachFromScene 会摘掉 SceneEntityComp,而疏散 / 排空的改派要等退出收敛之后才发,
		// 那时已无从知道玩家是从哪个场景被排走的。改派的确认核实拿它与 Redis 里的 location 比:仍指向这个场景 = 改派
		// 没生效。对"整节点疏散时本来就已在退出中"的玩家同样有效(那时只合并原因,这里抄下的值还在)。
		// sceneRegistry 上先 valid 再 try_get:对已销毁的场景实体调 try_get 是 entt 的未定义行为。
		if (const auto *sceneComp = tlsEcs.actorRegistry.try_get<SceneEntityComp>(player);
			sceneComp != nullptr && tlsEcs.sceneRegistry.valid(sceneComp->sceneEntity))
		{
			if (const auto *sceneInfo = tlsEcs.sceneRegistry.try_get<SceneInfoComp>(sceneComp->sceneEntity))
			{
				intent.sceneIdAtExit = sceneInfo->scene_id();
			}
		}
		tlsEcs.actorRegistry.emplace_or_replace<PlayerExitIntentComp>(player, intent);
	}

	// 退出即冻结运动学,必须在第一次存盘之前:否则 MovementSystem 继续推进 Transform,落地内容永远追不上
	// 内存,退出分支重存到上限后保留实体(Z1 以"超限保留"的形态回来)。
	StopMotionForExit(player);

	// Remove entity from AOI grid immediately so the AOI system stops
	// sending messages to the (already-disconnected) gate session.
	DetachFromScene(player);

	// Capture a logout snapshot before persisting (safety net for rollback).
	SnapshotSystem::CaptureAndSend(player, SNAPSHOT_LOGOUT);

	const Guid exitingPlayerId = (g != nullptr) ? *g : kInvalidGuid;
	bool savePending = PlayerLifecycleSystem::SavePlayerToRedis(player);

	// 快路径只比了"上次落地的快照",没看同 key 还有没有在途 / 排队中的存盘(§12.6.4 M4 / Z2):
	// 例如更早一次内容不同的周期存盘正在退避重试,此刻销毁实体,那笔写随后落地会把盘改回去,
	// 而内存里的最新态已经没了。有未落地的存盘就改走一次真实存盘(与排队值合并,只留最新),
	// 落地后由 HandlePlayerAsyncSaved 的退出分支按收敛规则收尾。
	if (!savePending && exitingPlayerId != kInvalidGuid && HasUnsettledPlayerSave(exitingPlayerId))
	{
		exit_persist_stats::Inc(exit_persist_stats::Get().fastpathDeferred);
		LOG_INFO << "HandleExitGameNode: player " << exitingPlayerId
				 << " matches its last persisted snapshot but still has an unsettled save; forcing a real save"
				 << " before finishing exit (metric=exit_fastpath_deferred)";
		savePending = SavePlayerToRedisImpl(player, /*allowSkipWhenPersisted=*/false);
		if (!savePending)
		{
			// 仍没写(交接已发起、不得再写):不能同步收尾(那笔在途存盘会在实体销毁后落地、把盘改回中间态),
			// 等它的落地回调走退出分支;交接标记若已写,由 HandlePlayerAsyncSaved 的"退出优先"分支撤回。
			LOG_WARN << "HandleExitGameNode: player " << exitingPlayerId
					 << " could not force a save; finishing exit when the unsettled save lands";
			return;
		}
	}

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
	// 本分支里交接标记已写(交接 EnterScene 可能在途):A1′ 不写断线释放标记(M11)。
	bool exitWonOverIssuedHandoff = false;

	// 归属交接在途(PlayerTravelHandoffComp):退出优先。交接只在"状态已落盘 + 输入已冻结"
	// 之后发起,盘上就是最新状态,本地实体没有任何目的地还要等的东西 —— 直接按普通退出销毁。
	// scene_manager 那边的应答随后到达时实体已不在,DispatchEnterSceneReply 幂等忽略;
	// 它若已放行,location 已指向目标(跨 zone 时 node 为空),下次登录按 Offline-Return 规则处理。
	if (tlsEcs.actorRegistry.valid(playerEntity) &&
		tlsEcs.actorRegistry.any_of<PlayerTravelHandoffComp>(playerEntity))
	{
		// 先抄后摘:handoff 标记写没写过、原文是什么只有组件知道(markEpoch != 0 = BeginTravelHandoff
		// 的 SET 已发出,原文 = "{markEpoch}:{requestedAtMs}")。
		const auto &travelIntent = tlsEcs.actorRegistry.get<PlayerTravelHandoffComp>(playerEntity);
		const uint64_t handoffMarkEpoch = travelIntent.markEpoch;
		const uint64_t handoffRequestedAtMs = travelIntent.requestedAtMs;
		const bool handoffMarkWritten = handoffMarkEpoch != 0 && handoffRequestedAtMs != 0;
		LOG_INFO << "FinishExitAfterPersist: player " << playerId
				 << " exited during ownership handoff; dropping handoff intent (exit wins)"
				 << " handoff_mark_written=" << handoffMarkWritten;
		travel_handoff_stats::Inc(travel_handoff_stats::Get().exitWins);
		tlsEcs.actorRegistry.remove<PlayerTravelHandoffComp>(playerEntity);
		tlsEcs.actorRegistry.remove<PlayerFrozenComp>(playerEntity);

		// 标记已写就必须撤回(与 AbortTravelHandoff 同走 WithdrawHandoffMark)。scene_manager 这次若没有
		// 推进 epoch(请求没发出去 / 回了非放行错误 / 重挑频道挑回本节点不铸造;路由失败回滚不在此列,见下),标记
		// "{E}:{ms}" 在 300s TTL 内仍等于当前 owner_epoch。玩家在 player_locator 30s 租约内重连回本节点
		// (同落点不铸造,epoch 仍是 E)继续产生新状态,此后任一次跨节点 EnterScene 都会凭这份旧标记
		// 免存盘过换手门:目标节点读到旧档(回档),本节点的释放存盘被 CAS 拒(假的双主告警)。
		// 撤回的最坏后果:scene_manager 恰好卡在预检与铸造 Lua 之间 → Lua 回"标记已撤回"(18)、
		// 未改任何状态,而玩家本来就在退出;它若已经铸造完,这次撤回无害。
		// 撤回是按标记原文的条件删除,不再依赖"排在 DispatchEmergencyRelocate 之前":疏散改派自己写的
		// 新标记 ms 取自墙钟、正常情况下与旧标记不同,迟到 / 重发的撤回删不到它(同一毫秒 / 墙钟回拨的
		// 同值碰撞不排除,后果只是改派被 18 拒回、玩家重登,不回档)。Redis 不通或应答丢失时不再只靠 TTL
		// 兜底:条目留在待撤回表里重试,期间该玩家重连回本节点也发不出 EnterScene(IsSceneChangeBusy)。
		// HandlePlayerAsyncSaved 里同名的"退出优先"分支也做同样的撤回:交接发起之后 SavePlayerToRedis 虽然
		// 直接跳过,但 HandleExitGameNode 的 M4 分支会在那时改为等一笔更早的未落地存盘,它落地时
		// requestedAtMs 可能已非 0(标记已写)。
		// GO-2(§12.8):scene_manager 推路由失败后的单调回滚会把 "E:t" 转写成 "E+2:t"。按原文撤回删不到它(条件删
		// 返回 0 即销账),这是**无害且正确**的:交接发起后本实体不再写盘、Redis 包含冻结内存,转写标记说的"t 时刻的
		// 状态已落盘、持有者不再写"依然为真 —— 之后本节点重载时 A2′(N = E+2)会删掉它,跨节点重登凭它过门零损失。
		// **禁止**为了"删干净"改成无条件 DEL:那会删掉现任持有者此刻的活标记(见 travel_outcome::kLuaJudgeTravelOutcome
		// 为什么只删本族)。
		if (handoffMarkWritten)
		{
			WithdrawHandoffMark(playerId, handoffMarkEpoch, handoffRequestedAtMs, "exit wins");
			exitWonOverIssuedHandoff = true;
		}
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

	// 改派登记要记一笔"本节点替他发出的更早 EnterScene 还可能被 scene_manager 处理"(实体销毁之后就无从得知了):
	//   * 退出优先作废的那次交接,标记已写 → 它的 EnterScene 可能还在排队(与 A1′ 共用同一个判据,见 player_exit_intent.h);
	//   * 实体上还挂着在途的普通换图,发出不满一个 settle 窗口。
	// 换手门只比标记的代际:那条更早的请求可以凭改派刚写的同代标记过门。所以为真时,改派被拒也不能马上踢,
	// 核实要等到 settle 再读(relocate_confirm::RequestOutcomeUnsettled)。
	// 已知漏报:普通换图的传输失败会当场摘掉在途组件(DispatchEnterSceneTransportFailure),此后一个 settle 窗口内被
	// 排空的玩家读不到它。后果只伤活性(被瞬时拒绝后立即踢,那条换图随后才被放行,重登即恢复)。
	const bool exitEntityValid = tlsEcs.actorRegistry.valid(playerEntity);
	const PlayerExitIntentComp *exitIntent =
		exitEntityValid ? tlsEcs.actorRegistry.try_get<PlayerExitIntentComp>(playerEntity) : nullptr;
	bool earlierEnterSceneMayReply = player_exit::HandoffEnterSceneMayBeInFlight(exitWonOverIssuedHandoff, exitIntent);
	if (exitEntityValid)
	{
		if (const auto *inFlight = tlsEcs.actorRegistry.try_get<PlayerSceneChangeInFlightComp>(playerEntity))
		{
			// settle 窗口从 scene_manager 的 gRPC deadline 派生,与待确认表登记时用的是同一个取值。
			const auto settleWindow =
				relocate_confirm::BudgetsFor(grpc_call_deadline::Get(eNodeType::SceneManagerNodeService)).settleWindow;
			const auto settleWindowMs = std::chrono::duration_cast<std::chrono::milliseconds>(settleWindow).count();
			earlierEnterSceneMayReply =
				earlierEnterSceneMayReply ||
				relocate_confirm::SceneChangeReplyMayArrive(/*hasInFlight=*/true, TimeSystem::NowMillisecondsUTC(),
															inFlight->sentAtMs, static_cast<uint64_t>(settleWindowMs));
		}
	}

	// 顺序不能反:先改派再摘 session 的话,改派用到的 session 已经没了;
	// 先销毁实体再改派的话,票据里的 gate/session 也拿不到。
	// 改派只需要票据里抄下来的信息,所以放在最前面。
	const bool relocateTicketConsumed = DispatchEmergencyRelocate(playerId, earlierEnterSceneMayReply);

	// A1′ 断线释放标记(§12.6.3 第二步):改派之后(改派消费了票据就由它写标记,A1′ 不覆盖)、摘会话之前
	// (实体、意图组件、owner_epoch 此刻都还在)。条件与计数见 WriteExitReleaseMarkIfEligible。
	WriteExitReleaseMarkIfEligible(playerId, playerEntity, relocateTicketConsumed, exitWonOverIssuedHandoff);

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
bool PlayerLifecycleSystem::EnqueueRelocateTicket(entt::entity playerEntity, const char *reasonTag, ExitCause cause)
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

	// 已经在退出流程中的玩家这里只合并退出原因(见 HandleExitGameNode),票据仍然登记着,
	// 等它自己的存盘回调到达时一样会被消费。
	HandleExitGameNode(playerEntity, cause);
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
		EnqueueRelocateTicket(entity, "EmergencyRelocate", ExitCause::kIdentityConflict);
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
		if (EnqueueRelocateTicket(entity, "SceneDrain", ExitCause::kSceneDrain))
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
	// 断线释放标记在途为 0(R6):疏散期间 A1′ 按"身份不确认"一律不写,这一项正常恒 0,纳入谓词只为不依赖那条判定。
	// 待确认表清空 = 每一次改派都有了结论(已在别处 / 已落回本节点 / 已踢线 / 已放弃)。不等它的话,节点在 quit loop 时
	// 会把即将到达的拒绝应答丢掉,被拒的玩家就挂在没有实体的会话上 —— 这正是待确认表要收的口。等待受 Node 的冲突
	// drain 看门狗约束;它到期后 Node 走 Shutdown,停机谓词同样等这张表、再给一道看门狗(头文件本函数的注释),
	// 两道都到期还剩的条目才只放弃、不踢。疏散中**这张表里的条目**只读 Redis、只推 gate(凭证补写被身份闸挡住),
	// 不给将死的节点新增任何写。(表之外有一处写:疏散中被同会话进场留下的票据,在停机阶段的退出收尾里照常
	// 做一次改派标记条件写,见 ReconcileRelocateOnReentry。)
	return tlsEmergencyRelocateTickets.empty() &&
		   tlsRelocateHandoffMarksInFlight == 0 &&
		   tlsExitReleaseMarksInFlight == 0 &&
		   tlsRelocateConfirms.size() == 0 &&
		   tlsEcs.actorRegistry.view<Player>().size() == 0;
}

bool PlayerLifecycleSystem::DispatchEmergencyRelocate(Guid playerId, bool earlierEnterSceneMayReply)
{
	auto it = tlsEmergencyRelocateTickets.find(playerId);
	if (it == tlsEmergencyRelocateTickets.end())
	{
		return false; // 不在疏散中,或者已经派发过
	}
	const EmergencyRelocateTicket ticket = it->second;
	tlsEmergencyRelocateTickets.erase(it);

	// 实体此刻还在(FinishExitAfterPersist 在本函数返回之后才销毁它):意图组件、当前会话、缓存的 owner_epoch 都要
	// 现在读,之后只剩按值抄下来的东西。实体已无效(不该发生)时三样都按"没有"处理。
	const auto playerEntity = tlsEcs.GetPlayer(playerId);
	const bool entityValid = tlsEcs.actorRegistry.valid(playerEntity);
	const PlayerExitIntentComp *exitIntent =
		entityValid ? tlsEcs.actorRegistry.try_get<PlayerExitIntentComp>(playerEntity) : nullptr;
	SessionId currentSession = kInvalidSessionId;
	uint64_t ownerEpoch = 0;
	if (entityValid)
	{
		if (const auto *snapshot = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(playerEntity))
		{
			currentSession = snapshot->gate_session_id();
		}
		if (const auto *epochComp = tlsEcs.actorRegistry.try_get<PlayerOwnerEpochComp>(playerEntity))
		{
			ownerEpoch = epochComp->epoch;
		}
	}

	// 票据作废:本次退出期间客户端已断线(会话已死),或实体当前的会话已不是发票时那一条。给这样的会话改派,
	// scene_manager 放行后路由会在 gate 被丢掉,location 停在一个从未载入该玩家的节点上,租约内重登被挑到别处时
	// 拿不出那一代的标记。不发改派、返回 false:由 A1′ 按第一次退出原因决定写不写断线释放标记(排空时写、疏散时不写)。
	const relocate_confirm::TicketDecision ticketDecision = relocate_confirm::DecideTicket(
		exitIntent != nullptr && exitIntent->clientDisconnected, player_exit::IsBoundSession(currentSession),
		currentSession == ticket.sessionId);
	relocate_confirm_stats::Inc(
		relocate_confirm_stats::Get().ticketDecisions[static_cast<std::size_t>(ticketDecision)]);
	if (ticketDecision != relocate_confirm::TicketDecision::kDispatch)
	{
		LOG_INFO << "[RelocateConfirm] ticket voided player=" << playerId
				 << " decision=" << relocate_confirm::TicketDecisionName(ticketDecision)
				 << " ticket_session=" << ticket.sessionId << " current_session=" << currentSession;
		return false;
	}

	// 换手门(CZ-4):scene_manager 只凭 player:{id}:handoff 放行"已有位置记录的跨节点落点"。
	// 走到这里时存盘已经落地(FinishExitAfterPersist 的前置条件),正是可以写标记的那一刻;
	// 不写的话生产配置(AllowUnsafeCrossNodeHandoff=false)下疏散 / 排空的每一个玩家都会被
	// ErrHandoffPending 挡回来,而本地实体马上就要销毁,玩家只能自己重登。
	// 标记落地之后再发 EnterScene;实体此后立刻销毁,回调只用按值捕获的票据与条目号。
	auto &redis = tlsRedis.GetZoneRedis();
	const bool writeMark = ownerEpoch != 0 && redis && redis->connected();
	// 本次**尝试**写的标记原文;不写时为空。只记到条目上进日志。
	const std::string value =
		writeMark ? player_ownership::HandoffRedisValue(ownerEpoch, TimeSystem::NowMillisecondsUTC()) : std::string();

	// 登记进待确认表:必须在销毁实体之前、与票据消费同一个调用栈里做(契约见 relocate_confirm.h)。从这一刻起这次
	// 改派有人盯到有结论为止。逐字段显式赋值:Registration 的缺省值取的是保守一侧,漏赋会让条目白等到 settle。
	relocate_confirm::Registration registration;
	registration.playerId = playerId;
	registration.sessionId = ticket.sessionId;
	registration.gateNodeId = ticket.gateNodeId;
	registration.gateInstanceId = ticket.gateInstanceId;
	registration.ownerEpoch = ownerEpoch; // 可能落后于 Redis(回滚把 epoch 推进了而本节点还没取证),只进日志
	registration.sourceSceneId = exitIntent != nullptr ? exitIntent->sceneIdAtExit : 0;
	registration.markValue = value;
	registration.earlierEnterSceneMayReply = earlierEnterSceneMayReply;
	const relocate_confirm::Clock::time_point registeredAt = relocate_confirm::Clock::now();
	// "更早请求"还有第三个来源:同一玩家**上一次改派**自己的请求。上一条条目还没收口(只可能在等载入:他刚被一条
	// 路由带回本节点、1s 节拍还没来得及结清)、它的请求结局未定、又还没过它的 settle —— 那条请求仍可能被
	// scene_manager 处理,而且可以凭这次改派刚写的同代标记过门。照样记成"更早请求可能被放行":这次改派被瞬时
	// 拒绝时不立即踢,等到 settle 再读。只覆盖得到旧条目尚未结清的那不到 1s;旧条目已按 landed_here 结清之后
	// 再被排空的,仍是已知漏报(只伤活性,见设计文档 CPP-3 一节的残余)。
	// 这个判据偏保守,会有"假阳性",不要当成漏洞去修:上一条的成功应答若在实体建出之后才到,它走的是
	// DispatchEnterSceneReply 的实体有效分支、不经认领,completionSeen 一直为假 —— 其实那条请求已经完成。
	// 判错的一侧只是这次改派被瞬时拒绝时多等到 settle 才踢、读不到时不盲踢。
	if (const relocate_confirm::Entry *previous = tlsRelocateConfirms.FindPlayer(playerId);
		previous != nullptr && registeredAt < previous->settleAt && relocate_confirm::RequestOutcomeUnsettled(*previous))
	{
		registration.earlierEnterSceneMayReply = true;
	}
	// settle 窗口与等完成通知的预算从 scene_manager 的 gRPC deadline 派生(grpc_call_deadline.h 的要求)。
	const relocate_confirm::Budgets budgets =
		relocate_confirm::BudgetsFor(grpc_call_deadline::Get(eNodeType::SceneManagerNodeService));
	std::optional<relocate_confirm::Entry> superseded;
	const uint64_t seq = tlsRelocateConfirms.Add(registration, registeredAt, budgets, superseded);
	if (superseded.has_value())
	{
		// 同一玩家上一次改派的条目还没收口就又被排空:只可能是他刚落回本节点、1s 节拍还没来得及把 kLanding 条目结清。
		// 旧条目按 superseded 结清(不踢:玩家本人就在本节点上,正要被再次改派)。别的阶段出现在这里说明"条目存在期间
		// 本节点没有该玩家实体"的契约被破坏了,留一条 ERROR。
		if (superseded->phase != relocate_confirm::Phase::kLanding)
		{
			LOG_ERROR << "[RelocateConfirm] entry of player " << playerId << " seq=" << superseded->seq
					  << " was replaced while in phase " << relocate_confirm::PhaseName(superseded->phase)
					  << " (expected landing): the player had an entity on this node while its relocate was unresolved";
		}
		ConcludeRelocateWithoutKick(*superseded, relocate_confirm::Outcome::kSuperseded,
									relocate_confirm::kVerdictUnread, registeredAt);
	}
	if (seq == 0)
	{
		// 表满:不淘汰在途条目(淘汰 = 放弃对某个玩家的确认),这一次按旧行为发出、不跟踪。
		relocate_confirm_stats::Inc(relocate_confirm_stats::Get().untrackedOverflow);
		LOG_ERROR << "[RelocateConfirm] untracked player=" << playerId << " reason=table_full pending="
				  << tlsRelocateConfirms.size() << " (metric=untracked_overflow)";
	}
	else
	{
		LOG_INFO << "[RelocateConfirm] tracking player=" << playerId << " seq=" << seq << " session=" << ticket.sessionId
				 << " gate=" << ticket.gateNodeId << " owner_epoch=" << ownerEpoch
				 << " source_scene=" << registration.sourceSceneId
				 << " mark=" << (value.empty() ? std::string("none") : value)
				 << " earlier_reply_possible=" << (registration.earlierEnterSceneMayReply ? 1 : 0);
	}

	if (!writeMark)
	{
		// epoch 未铸造(兼容窗口,scene_manager 侧也没有门可过)或 Redis 不可用:照旧直接发。
		// 后者在生产下会被换手门拒绝 —— 写不出落盘凭证就不该被放行,这是 fail-closed 的本意。
		// 被拒之后由待确认表核实并踢线,不再让玩家挂着。
		if (ownerEpoch != 0)
		{
			LOG_ERROR << "[EmergencyRelocate] zone redis unavailable; handoff mark not written for player "
					  << playerId << ", scene_manager will refuse the re-home until the player re-enters";
		}
		TrackRelocateDispatch(playerId, seq, ticket, relocate_confirm::MarkWrite::kNotAttempted);
		return true;
	}
	if (relocate_confirm::Entry *entry = tlsRelocateConfirms.Find(playerId, seq); entry != nullptr)
	{
		entry->markWrite = relocate_confirm::MarkWrite::kPending; // seq 为 0(未跟踪)时 Find 找不到,跳过
	}

	// 条件写(GO-2 §12.8):owner_epoch 仍等于本实体缓存的 E 才写 "E:now"(exit_release_mark::kLuaWriteIfOwnerEpoch,
	// 与 A1′ 同一段 Lua)。旧写法无条件 SET:scene_manager 推路由失败、已把归属单调回滚到本节点并把标记转写成
	// "E+2:t"、而本节点还没来得及取证时,恰逢疏散 / 排空就会用过期的 E 盖掉转写标记,改派与之后的重登都回 18。
	// 条件写在 epoch 已变时不写,转写标记留着,改派凭它过门;对已被废黜的节点同样不写,比无条件写严格更安全。
	// 无论写没写成都照常发改派,由 scene_manager 的换手门裁决(与改动前一致)。
	// 残余:疏散在回滚**之前**撤回了原标记时回滚回 marker_gone,改派会回 18(三重巧合,cross-zone-scene-travel.md §12.8)。
	// 写的结局(写成 / owner_epoch 已变没写 / 失败)只记到条目上进日志,**不进任何判定**:没写时 scene_manager 凭的是
	// 别人的标记,从"这份原文写没写成"推不出改派有没有被放行、被回滚。改派没生效要踢线时,凭证由待确认表按那一次
	// 核实读到的 owner_epoch 兜底补写(relocate_confirm::DecideCredential)。
	const std::string ownerEpochKey = player_ownership::OwnerEpochRedisKey(playerId);
	const std::string key = player_ownership::HandoffRedisKey(playerId);
	const std::string epochText = std::to_string(ownerEpoch);
	++tlsRelocateHandoffMarksInFlight;
	// 脚本串作为一个 %s 参数传入是安全的:hiredis 只按**格式串**里的空格切参数(同 SendConditionalMarkWrite)。
	// 回调只按值捕获 id、条目号、票据与标记原文(AGENTS §11.7);回调到达时凭 (playerId, seq) 对号,条目已结清 /
	// 已落回本节点 / 已超时转核实的不再发(TrackRelocateDispatch)。
	const int ret = redis->command(
		[playerId, seq, ticket, value](hiredis::Hiredis *, redisReply *reply)
		{
			--tlsRelocateHandoffMarksInFlight;
			const exit_release_mark::MarkWriteResult writeResult =
				exit_release_mark::ClassifyMarkWriteReply(ToReplyShape(reply), ReplyInteger(reply));
			relocate_confirm::MarkWrite markWrite = relocate_confirm::MarkWrite::kFailed;
			switch (writeResult)
			{
			case exit_release_mark::MarkWriteResult::kWritten:
				markWrite = relocate_confirm::MarkWrite::kWritten;
				break;
			case exit_release_mark::MarkWriteResult::kEpochMoved:
				markWrite = relocate_confirm::MarkWrite::kEpochMoved;
				LOG_INFO << "[EmergencyRelocate] handoff mark not written for player " << playerId << " mark=" << value
						 << ": owner_epoch moved on (e.g. rolled back past this node's cached epoch); keeping the"
						 << " existing mark, requesting re-home anyway (scene_manager decides)";
				break;
			default:
				LOG_ERROR << "[EmergencyRelocate] conditional handoff mark write failed for player " << playerId
						  << " mark=" << value << ReplyFailureSuffix(reply)
						  << "; requesting re-home anyway (scene_manager decides)";
				break;
			}
			TrackRelocateDispatch(playerId, seq, ticket, markWrite);
		},
		"EVAL %s 2 %s %s %s %s %d", exit_release_mark::kLuaWriteIfOwnerEpoch, ownerEpochKey.c_str(), key.c_str(),
		epochText.c_str(), value.c_str(), player_ownership::kHandoffMarkTtlSec);
	if (ret != REDIS_OK)
	{
		// 命令没发出去,回调不会来:自己把计数还回去。
		--tlsRelocateHandoffMarksInFlight;
		LOG_ERROR << "[EmergencyRelocate] redis command dispatch failed for player " << playerId;
		TrackRelocateDispatch(playerId, seq, ticket, relocate_confirm::MarkWrite::kFailed);
	}
	return true;
}

void PlayerLifecycleSystem::TrackRelocateDispatch(Guid playerId, uint64_t seq, const EmergencyRelocateTicket &ticket,
												  relocate_confirm::MarkWrite markWrite)
{
	if (seq == 0)
	{
		// 没被跟踪(登记时表满):按旧行为直接发,发没发出去、有没有生效都不再过问。
		SendEmergencyRelocateEnterScene(playerId, ticket, /*trackSeq=*/0);
		return;
	}
	relocate_confirm::Entry *entry = tlsRelocateConfirms.Find(playerId, seq);
	if (entry != nullptr)
	{
		entry->markWrite = markWrite; // 只进日志
	}
	if (entry == nullptr || entry->phase != relocate_confirm::Phase::kMarkWriting)
	{
		// 写标记的回调到达时这次改派已经有了去向:已结清,进场路由已先把他带回本节点(条目在等载入),或回调来得
		// 太晚、条目已按"没发出去"转核实。再发一条 EnterScene 只会给一条已经处置过的会话重新改派。
		LOG_INFO << "[RelocateConfirm] not sending player=" << playerId << " seq=" << seq << " phase="
				 << (entry != nullptr ? relocate_confirm::PhaseName(entry->phase) : "gone")
				 << ": entry already moved on";
		return;
	}
	const uint64_t correlationId = SendEmergencyRelocateEnterScene(playerId, ticket, seq);
	if (correlationId == 0)
	{
		// 没发出去(scene_manager 注册表为空):改派不可能生效,核实一下 location 没被别的请求动过就踢。
		relocate_confirm_stats::Inc(relocate_confirm_stats::Get().notSent);
		StartRelocateVerify(playerId, seq, relocate_confirm::Evidence::kNotSent,
							relocate_confirm::Expectation::kAtSource, relocate_confirm::Clock::now());
		return;
	}
	// 发送之后不沿用之前的指针,重新对号再读条目上的两个预算(SendEmergencyRelocateEnterScene 里 MarkSent 时定的)。
	entry = tlsRelocateConfirms.Find(playerId, seq);
	if (entry == nullptr)
	{
		return;
	}
	LOG_INFO << "[RelocateConfirm] sent player=" << playerId << " seq=" << seq << " corr=" << correlationId
			 << " mark_write=" << relocate_confirm::MarkWriteName(markWrite)
			 << " settle_ms=" << RelocateMillis(entry->settleAt - entry->sentAt)
			 << " reply_wait_ms=" << RelocateMillis(entry->deadline - entry->sentAt);
}

uint64_t PlayerLifecycleSystem::SendEmergencyRelocateEnterScene(Guid playerId, const EmergencyRelocateTicket &ticket,
																uint64_t trackSeq)
{
	// GetSceneManagerEntity 只在 scene_manager 注册表为空时返回 null;"全是坏通道"时照样挑出一个,请求随后以传输失败
	// 收场(由待确认表按结果未知处理)。所以本函数判得出的"没发出去"只有注册表为空这一种。
	// 它的最末一级还会挑出"挂了 NodeInfo、通道还没建"的实体,而生成的发送函数对实体上的通道组件没有保护 ——
	// 这是所有 EnterScene 发送点共有的既有风险(正常路径下通道先于 NodeInfo 挂上),不是改派特有的,这里不处理。
	const auto smEntity = GetSceneManagerEntity(playerId);
	if (smEntity == entt::null)
	{
		if (trackSeq != 0)
		{
			LOG_ERROR << "[EmergencyRelocate] no SceneManager node reachable; player " << playerId
					  << " was not re-homed, will verify and kick";
		}
		else
		{
			LOG_ERROR << "[EmergencyRelocate] no SceneManager node reachable; player " << playerId
					  << " keeps its gate session and has to re-enter through the normal login flow";
		}
		return 0;
	}
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
	// 关联号(统一出口要求每条都带非 0 号):这次改派的等待者是待确认表里的条目,不是实体 —— 发出时本地实体随即销毁。
	// **先把号记到条目上再发**(SendCorrelatedEnterScene 的前置条件,与 RequestSceneChange / RequestTravelEnterScene
	// 同一纪律):应答 / 传输失败回来时走到 DispatchEnterSceneReply / DispatchEnterSceneTransportFailure 的"实体已不在"
	// 分支,由 ClaimRelocateEnterSceneReply / ClaimRelocateTransportFailure 按这个号认领。实体若已在本节点重建,
	// 应答照旧进 Classify,对不上任何等待者的号(kNoWaiter / kUnmatched)—— 那时条目已在等载入,本来就不看应答。
	// 未跟踪(trackSeq == 0,表满)的这一次没有等待者,号只进日志。
	const uint64_t correlationId = NextEnterSceneCorrelationId();
	if (trackSeq != 0)
	{
		// 调用方(TrackRelocateDispatch)已确认条目在 kMarkWriting;这里再对一次号,只在仍然如此时迁移。
		// 应答与 settle 的钟都从这一刻起算,预算从 scene_manager 的 gRPC deadline 派生。
		if (relocate_confirm::Entry *entry = tlsRelocateConfirms.Find(playerId, trackSeq);
			entry != nullptr && entry->phase == relocate_confirm::Phase::kMarkWriting)
		{
			relocate_confirm::Table::MarkSent(
				*entry, relocate_confirm::Clock::now(), correlationId,
				relocate_confirm::BudgetsFor(grpc_call_deadline::Get(eNodeType::SceneManagerNodeService)));
		}
	}
	SendCorrelatedEnterScene(smEntity, req, correlationId);

	LOG_INFO << "[EmergencyRelocate] requested main-world re-home for player " << playerId
			 << " (session=" << ticket.sessionId << ", gate=" << ticket.gateNodeId << ", corr=" << correlationId << ")";
	return correlationId;
}

void PlayerLifecycleSystem::ReconcileRelocateOnReentry(Guid playerId, SessionId sessionId)
{
	// 1. 还没派发的票据作废。任何把玩家路由回本节点的进场,要么复用实体并取消退出(EnterScene 第 0 步),要么废黜实体
	//    后重新载入;两种情况下那次退出都不会再正常收尾。留着票据,它会在下一次、会话已换的退出里被误消费。
	//    整节点疏散中、带着票据那同一条有效会话的进场例外,票据留着(relocate_confirm::CancelsTicketOnReentry):
	//    进场若取消了他的退出,他就留在这个将死的节点上,而 BeginEmergencyRelocateAll 只发一轮票(疏散开始后不再
	//    补发)。留着的票据会在节点最后的 exitAllPlayers 收尾时被消费,把同一条会话改派出去 —— 那是将死节点在
	//    停机阶段多做的一次"改派标记条件写 + EnterScene"(owner_epoch 条件写把关,与疏散开始时那一轮同样的写);
	//    删掉的话这名玩家既不改派也不踢。另一条会话或会话 0 的进场照旧作废。
	//    进场若走的是"废黜实体后重载"(PlayerEnterGameNode 的 1.5 / 1.6),留下的票据随后由 DestroyDeposedPlayer
	//    删掉,计的是 ticket_dropped_deposed 而不是 cancelled_on_reentry。
	const auto ticketIt = tlsEmergencyRelocateTickets.find(playerId);
	if (ticketIt != tlsEmergencyRelocateTickets.end() &&
		relocate_confirm::CancelsTicketOnReentry(tlsEmergencyRelocating, player_exit::IsBoundSession(sessionId),
												 sessionId == ticketIt->second.sessionId))
	{
		tlsEmergencyRelocateTickets.erase(ticketIt);
		relocate_confirm_stats::Inc(relocate_confirm_stats::Get().cancelledOnReentry);
		LOG_INFO << "[RelocateConfirm] cancelled on reentry player=" << playerId << " session=" << sessionId
				 << " (metric=cancelled_on_reentry)";
	}

	// 2. 本节点替他发的改派有了去向。
	relocate_confirm::Entry *entry = tlsRelocateConfirms.FindPlayer(playerId);
	if (entry == nullptr)
	{
		return;
	}
	const relocate_confirm::Clock::time_point now = relocate_confirm::Clock::now();
	if (!player_exit::IsBoundSession(sessionId) || sessionId != entry->sessionId)
	{
		// 进场带来的不是票据那条会话(或根本没带会话):玩家已经重登,票据里的旧会话已死。不踢。
		ResolveRelocate(playerId, relocate_confirm::Outcome::kSuperseded, relocate_confirm::kVerdictUnread, now);
		return;
	}
	// 同一条会话被路由回了本节点:转入"落地等载入",由 SweepRelocateConfirms 每拍看实体建出来没有;载入被放弃
	// (继承标记核对不过 / 载入失败 / 会话在载入中取消)时核实后踢。条目若还在写标记 / 等应答 / 核实,那些阶段的
	// 回调与应答此后都因阶段已变而不再生效(在途的核实应答由 AcceptsVerifyReply 丢弃)。
	// loadPending 取的是"上一次载入还在不在":本函数排在登记这一次的待入场条目之前。同一条路由被 gate 补发时
	// 上一次载入还在途,BeginLanding 据此不重置截止时刻;上一次已结束则是真正的第二次进场,截止时刻重新起算。
	const bool loadPending = tlsPendingEnterMap.count(playerId) != 0;
	const bool wasLanding = entry->phase == relocate_confirm::Phase::kLanding;
	const bool wasReentered = entry->reentered;
	const relocate_confirm::Clock::time_point previousDeadline = entry->deadline;
	relocate_confirm::Table::BeginLanding(*entry, now, /*reentered=*/true, loadPending);
	if (!wasLanding || !wasReentered || entry->deadline != previousDeadline)
	{
		LOG_INFO << "[RelocateConfirm] landing player=" << playerId << " seq=" << entry->seq << " reentered=1";
	}
	else
	{
		LOG_DEBUG << "[RelocateConfirm] duplicate route for a landing entry, player=" << playerId
				  << " seq=" << entry->seq << "; deadline unchanged";
	}
}

void PlayerLifecycleSystem::SweepRelocateConfirms(relocate_confirm::Clock::time_point now, bool reconnected)
{
	if (tlsRelocateConfirms.size() == 0)
	{
		return;
	}
	auto &stats = relocate_confirm_stats::Get();
	// CollectDue 只归类、只给出 (playerId, seq);逐个处置时会结清条目,所以每一个都重新对号,不持有表内指针。
	const relocate_confirm::Table::Due due = tlsRelocateConfirms.CollectDue(now, reconnected);
	if (due.markWriteTimedOut > 0)
	{
		stats.markWriteTimeout.fetch_add(due.markWriteTimedOut, std::memory_order_relaxed);
	}
	if (due.replyTimedOut > 0)
	{
		stats.replyTimeout.fetch_add(due.replyTimedOut, std::memory_order_relaxed);
	}
	for (const auto &[playerId, seq] : due.toVerify)
	{
		SendRelocateVerify(playerId, seq, now);
	}
	for (const auto &[playerId, seq] : due.verifyExpired)
	{
		// 一轮核实到期仍读不到。本节点读不到 ≠ scene_manager 写不了(脚本在被降级的旧主上被拒、本节点与 Redis 之间
		// 的分区),所以只有"本节点替他发的请求里没有一条可能写过落点"才踢,其余放弃、不踢(relocate_confirm::
		// ShouldKickUnverified;拒绝码过 SmRejectedBeforeAnyPlacementWrite 的白名单)。没有读数,也就不补写凭证。
		const relocate_confirm::Entry *entry = tlsRelocateConfirms.Find(playerId, seq);
		if (entry == nullptr || entry->phase != relocate_confirm::Phase::kVerifying)
		{
			continue;
		}
		if (relocate_confirm::ShouldKickUnverified(entry->evidence, relocate_confirm::RequestOutcomeUnsettled(*entry),
												   SmRejectedBeforeAnyPlacementWrite(entry->replyErrorCode)))
		{
			KickRelocatedSession(playerId, relocate_confirm::Outcome::kKickUnverified,
								 relocate_confirm::CredentialAction::kNone, /*credentialEpoch=*/0,
								 relocate_confirm::kVerdictUnread, now);
		}
		else
		{
			ResolveRelocate(playerId, relocate_confirm::Outcome::kGaveUpUnverified, relocate_confirm::kVerdictUnread,
							now);
		}
	}
	for (const auto &[playerId, seq] : due.landingToCheck)
	{
		CheckRelocateLanding(playerId, seq, now);
	}
}

std::size_t PlayerLifecycleSystem::RelocateConfirmsPending()
{
	return tlsRelocateConfirms.size();
}

void PlayerLifecycleSystem::LogRelocateConfirmBacklog(const char *site)
{
	if (tlsRelocateConfirms.size() == 0)
	{
		return;
	}
	const auto counts = tlsRelocateConfirms.CountByPhase();
	std::string summary = "[RelocateConfirm] backlog site=";
	summary += site;
	summary += " total=";
	summary += std::to_string(tlsRelocateConfirms.size());
	for (std::size_t i = 0; i < counts.size(); ++i)
	{
		summary += ' ';
		summary += relocate_confirm::kPhaseNames[i];
		summary += '=';
		summary += std::to_string(counts[i]);
	}
	LOG_WARN << summary;
	// 逐条列出:drain 看门狗到期后这些条目只放弃、不踢,事后要查得出是哪些玩家可能还挂着。player_id 只进日志,
	// 不做指标 label。不在任何定时路径上(调用方保证每次 drain 只调一次)。
	const relocate_confirm::Clock::time_point now = relocate_confirm::Clock::now();
	for (const relocate_confirm::Entry &entry : tlsRelocateConfirms.Snapshot())
	{
		LOG_INFO << "[RelocateConfirm] backlog entry player=" << entry.playerId << " seq=" << entry.seq
				 << " phase=" << relocate_confirm::PhaseName(entry.phase)
				 << " evidence=" << RelocateEvidenceText(entry) << " session=" << entry.sessionId
				 << " age_ms=" << RelocateAgeMs(entry, now);
	}
}

std::optional<relocate_confirm::Entry> PlayerLifecycleSystem::PeekRelocateConfirm(Guid playerId)
{
	const relocate_confirm::Entry *entry = tlsRelocateConfirms.FindPlayer(playerId);
	if (entry == nullptr)
	{
		return std::nullopt;
	}
	return *entry;
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
	return SavePlayerToRedisImpl(player, /*allowSkipWhenPersisted=*/true);
}

bool PlayerLifecycleSystem::SavePlayerToRedisImpl(entt::entity player, bool allowSkipWhenPersisted)
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

	// Marshal + set player_id on each sub-table (not set by generated marshal code),
	// shared with the exit-convergence compare in HandlePlayerAsyncSaved.
	// MUST be done BEFORE Save(): MessageAsyncClient::Save() now serializes the
	// payload eagerly into Element::serialized_payload, so any post-Save mutation
	// would not make it into the Redis blob.
	MarshalPlayerForSave(player, playerId, *message);

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
	//
	// allowSkipWhenPersisted = false:不走快路径,一定写盘。只有三处这么传:退出流程的 M4 分支
	// (exit_fastpath_deferred)、StartTravelHandoff 的快路径补写(handoff_fastpath_forced),以及跨 zone 交接
	// 被回滚到本节点后的采纳(JudgeTravelOutcomeReply B5,rolled_back_adopted)。
	if (auto* snap = tlsEcs.actorRegistry.try_get<PlayerLastPersistedSnapshotComp>(player);
		allowSkipWhenPersisted && snap != nullptr && snap->HasSnapshot() &&
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
	// 世代号来自部署配置(DbTaskTopicGeneration,缺键 / 0 = 第一代,名字不带后缀),必须与 go/db、
	// go/login 的 Kafka.TopicGeneration 相等:换代后仍按旧名写 = 存盘进了已排空、没人消费的旧 topic。
	// 选哪个 zone 的 topic 仍只看 home_zone(上面的回落逻辑),世代号不改变路由。
	const std::string dbTaskTopic = GetDbTaskTopic(
		homeZoneId, tlsNodeConfigManager.GetBaseDeployConfig().db_task_topic_generation());

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
			// SavePlayerToRedis 返回 false = 与上次成功落盘的快照相同、无需再写(或交接已发起不得再写),
			// 对在线实体是安全的终点。
			if (SavePlayerToRedis(playerEntity))
			{
				return;
			}
			// 退出中的实体却不是:被拒的那笔若就是它的退出存盘,不写盘就没有落地回调,退出永远收不了尾
			// (UnregisterPlayer 一直挂着、IsSaveInFlight 恒真、客户端消息一直被拒)。与 HandleExitGameNode
			// 快路径同一规则:还有未落地的存盘就等它落地走退出分支;没有就说明盘上已是当前内存,直接收尾。
			if (tlsEcs.actorRegistry.any_of<UnregisterPlayer>(playerEntity) && !HasUnsettledPlayerSave(playerId))
			{
				LOG_INFO << "Player marked for unregistration: " << playerId
						 << " (exit save re-issued after a stale-epoch rejection was already persisted)";
				FinishExitAfterPersist(playerId);
			}
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
	// 这是票据在 DispatchEmergencyRelocate 之外的第二个删除点:不发改派、不进待确认表,也就没有人替这条会话核实与
	// 踢线。多数入口下玩家已在别处或正被本节点重载,作废是对的;但排空 / 疏散发起的退出还没收敛就被"标记已发出"
	// 收口销毁时(那条路按"退出中 = 客户端已断开"不踢),客户端可能还连着。这里不踢(被废黜的玩家多半已在别处,
	// 按旧会话踢会误伤),只留计数与日志让它看得见。
	if (tlsEmergencyRelocateTickets.erase(playerId) != 0)
	{
		relocate_confirm_stats::Inc(relocate_confirm_stats::Get().ticketsDroppedDeposed);
		LOG_WARN << "[RelocateConfirm] ticket dropped player=" << playerId << " site=" << reasonTag
				 << ": entity destroyed as deposed before its exit converged; not relocating, not kicking"
				 << " (metric=ticket_dropped_deposed)";
	}

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
//   同 zone 跨节点换图  普通 EnterScene(RequestSceneChange,带关联号)被 scene_manager 以 18 暂拒
//                       ──▶ DispatchEnterSceneReply(号匹配在途换图)──▶ HandleSceneChangeEnterSceneReply
//                       ──▶ StartTravelHandoff(目标 = 本 zone)
//
//   StartTravelHandoff:挂 PlayerTravelHandoffComp + PlayerFrozenComp ──▶ SavePlayerToRedis
//   存盘落地 ──▶ BeginTravelHandoff:SET player:{id}:handoff "{epoch}:{now}" EX 300
//            ──▶ RequestTravelEnterScene:取号记到交接组件 → scene_manager.EnterScene(ZoneId=目标, SceneId, SceneConfId)
//            ──▶ DispatchEnterSceneReply(号匹配本代交接;旧版 SM 不回显时按 player_id)
//            ──▶ HandleTravelEnterSceneReply:Redirect(跨 zone 放行)      → DestroyDeposedPlayer
//                                             成功无 Redirect(同 zone 放行) → ResolveTravelOutcome
//                                             错误 / 超时                   → ResolveTravelOutcome
//   ResolveTravelOutcome(GO-2 判定表,§12.8):一次 EVAL 原子地删掉本次交接这一族标记、再读 owner_epoch + location,
//     然后只有两种正向证据能解冻,其余一律不存盘销毁:
//       epoch 没变                                   → 解冻(成功应答静默,其余回 tip)
//       回执 == 本次标记原文且 epoch 恰好前进两格、location 指回本节点(scene_manager 推路由失败、单调回滚到本节点)
//                                                    → 采纳 E+2、解冻回 tip、强制存盘一次
//       回执对上但交叉校验不成立                     → ConcludeHandoffAfterMarkSent(tip + 34 + 不存盘销毁)
//       其余(已放行 / 放行后又回来 / location 缺失)  → DestroyDeposedPlayer
//         (跨 zone 且 location 是本次交接的等待落点时,销毁之前先回失败 tip + 踢线 34:客户端没拿到重定向,让它重登)
//     应答回显的 owner_epoch_after_rollback 只进日志,绝不参与判定。
//   同 zone 重发后落回本节点时,进场路由可能先于应答到达(应答也可能丢):EnterScene 3.2 步不等应答,
//   直接走 ResolveTravelOutcome 静默解冻。
//   交接途中玩家退出:退出优先(FinishExitAfterPersist),标记已写则按原文一并撤回(删不到回滚转写的 "E+2:t",无害)。
//   冻结有硬上限(travel_freeze_cap.h,单调时钟):
//     冻结满 70s 仍无结论 ──▶ EnforceTravelFreezeCaps(1s 扫描):handoff 标记没发出 → AbortTravelHandoff(解冻 + tip);
//                              已发出 → ConcludeHandoffAfterMarkSent(tip + 踢线 34 + 不存盘销毁)
//     35s 晚发闸:set_mark 阶段(BeginTravelHandoff,SET 未发)→ Abort;enter_scene 阶段(RequestTravelEnterScene,
//                SET 已 OK)→ Conclude。SET 已 OK 之后发现没有 gate 会话 / 没有 scene_manager 同样走 Conclude。
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

	// 目标地图:0 = 由目标 zone 的 scene_manager 挑默认大世界;非 0 必须是 World 表登记过的世界图
	// (World.scene_id 是 BaseScene id,与 scene_manager 的 worldConfIds / 世界频道同一口径)。
	// scene_config_id 是客户端可控字段,必须在冻结之前查:一旦放行,源实体随即销毁,目标 zone 才发现
	// 这张图落不进去(副本 / 镜像的 conf、乱填的 id)时玩家已经没有"原地"可回。后面还有两道:
	// scene_manager 第一条腿只读检查目标 zone 有没有这张图的频道(World 表全服一份,某个 zone 没开
	// 这张图只有它知道),第二条腿解析失败回落默认大世界。
	// World 表为空 = 没载表(单测宿主),判不了就不拦,交给后面两道。
	// 复用既有的 kEnterSceneSceneNotFound,不为它新开 tip 码。
	if (sceneConfigId != 0 && WorldTableManager::Instance().Count() != 0 &&
		WorldTableManager::Instance().CountBySceneIdIndex(sceneConfigId) == 0)
	{
		LOG_WARN << "[ZoneTravel] rejected: scene_config_id is not a world map, player_id=" << playerId
				 << " target_zone=" << targetZoneId << " scene_config_id=" << sceneConfigId;
		return kEnterSceneSceneNotFound;
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
	if (!HasLiveGateSession(player, playerId))
	{
		return genericFailTip;
	}
	// scene_manager 必须可达。handoff 标记的 SET 一旦发出就不许解冻(骨架),SET 之后才发现没有 SM 只能按
	// "标记已发出"销毁并踢线(ConcludeHandoffAfterMarkSent)。所以在改任何状态之前先挡住常见情形(请求时 SM
	// 就不在),仍然只回失败 tip;剩下的只有"SM 恰好在 SET 往返那几毫秒里消失"的极窄窗口。
	if (GetSceneManagerEntity(playerId) == entt::null)
	{
		LOG_WARN << "[ZoneTravel] handoff not started for player " << playerId << ": no SceneManager node reachable";
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
	// 冻结硬上限 / 晚发窗口从这一刻起算(单调时钟,与下面挂 PlayerFrozenComp 同一刻),之后不改。
	travel.frozenAtSteady = travel_freeze_cap::Clock::now();
	travel.destroyDeferralReported = false;

	// 先冻结再存盘:冻结之后状态不再变,这次存盘(或快路径判定的"盘上已是同一份")才代表
	// 玩家交接那一刻的最终态。同一 key 只发布最新快照的完成回调(redis_client.h EnqueueSave /
	// OnSaved),所以冻结前还在途的周期存盘不会抢先触发 BeginTravelHandoff。
	// frozenAtMs 另存一份局部值:下面存盘阶段看门狗要用它当代际,不隔着 SavePlayerToRedis 去读组件引用。
	const int64_t frozenAtMs = static_cast<int64_t>(TimeSystem::NowMillisecondsUTC());
	auto &frozen = tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
	frozen.frozenAtMs = frozenAtMs;
	frozen.toZoneId = targetZoneId;

	// 成败计数从这里起算(此前的校验拒绝没有改状态,不算一次交接)。各终态在
	// AbortTravelHandoff / 放行销毁 / "退出优先"分支里各记一次,见 travel_handoff_stats。
	travel_handoff_stats::Inc(travel_handoff_stats::Get().started);
	if (crossZone)
	{
		travel_handoff_stats::Inc(travel_handoff_stats::Get().startedCrossZone);
	}
	EnsureTravelHandoffStatsTimer();
	EnsureTravelFreezeCapTimer();

	LOG_INFO << "[ZoneTravel] handoff started for player " << playerId
			 << " target_zone=" << targetZoneId << (crossZone ? " (cross-zone)" : " (same-zone cross-node)")
			 << " scene_id=" << sceneId << " scene_conf_id=" << sceneConfigId;

	// 双路径约定(见头文件 BeginTravelHandoff 的说明):返回 true = 落地回调 HandlePlayerAsyncSaved
	// 会接手;返回 false = 快路径没写盘、回调永远不会来,必须自己调。
	// BeginTravelHandoff 内部的失败分支(无 epoch / Redis 断连)会同步解冻并回 tip,这里仍返回
	// "已受理":对调用方而言就是"受理后未成",与异步失败同一种形态。
	bool savePending = SavePlayerToRedis(player);
	// 快路径只比了"上次落地的快照",没看同 key 还有没有在途 / 排队中的存盘 —— 与退出链 M4(HandleExitGameNode)
	// 同一个问题、同一写法。例如一笔更早的、内容不同的周期存盘 X 正在退避重试(redis_client 在 Redis 不可用时无限
	// 重试,从不放弃):这时直接写标记,X 之后仍可能以当前 epoch 落地 —— 放行前落地,目标节点读到的就是 X;交接未成
	// 而实体被不存盘销毁时,X 就成了永久状态。改走一次真实存盘:新值排在 X 之后(同 key 排队值永远比在途值新,完成回调
	// 只对最新值发布),只有它落地才回调 HandlePlayerAsyncSaved → BeginTravelHandoff。这保证了"SET 发出 ⇒ Redis ⊇
	// 冻结内存"(骨架 I1),放行路径也跟着受益。此刻 requestedAtMs 仍为 0,不会被 SavePlayerToRedisImpl 的
	// "交接已发起不得再写"跳过。
	if (!savePending && HasUnsettledPlayerSave(playerId))
	{
		travel_handoff_stats::Inc(travel_handoff_stats::Get().handoffFastpathForced);
		LOG_INFO << "[ZoneTravel] handoff of player " << playerId
				 << " matches its last persisted snapshot but still has an unsettled save; forcing a real save"
				 << " before writing the handoff mark (metric=handoff_fastpath_forced)";
		savePending = SavePlayerToRedisImpl(player, /*allowSkipWhenPersisted=*/false);
		if (!savePending)
		{
			// 不可达(实体有效、requestedAtMs 刚置 0 时强制存盘必然写盘)。fail-closed:有未落地的存盘就不许写标记;
			// SET 还没发,解冻是安全的(不变量 I0)。
			LOG_ERROR << "[ZoneTravel] handoff of player " << playerId
					  << " could not force a save while an older save is unsettled; not writing the handoff mark";
			AbortTravelHandoff(playerId, "could not force a save before writing the handoff mark");
			return kTravelAccepted;
		}
	}
	if (savePending)
	{
		// 存盘在途。Redis 断连时这次写会留在重试队列里,落地回调可能很久都不来,而应答看门狗要到
		// EnterScene 发出之后才挂 —— 存盘看门狗是这一段的主兜底;它没能按时触发时(例如墙钟回拨推迟了 muduo
		// 定时器),存盘晚于 35s 才落地的由晚发闸(set_mark 阶段)拦下不写标记,一直不落地的由 70s 冻结上限的
		// "标记未发出 → 解冻"分支收住。
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
	// 只捕获 id + 代际(frozenAtMs:这一段 requestedAtMs 还是 0,分不出是哪一次交接)。不保存 TimerId、不取消。
	// 截止时刻按单调时钟定,muduo(按墙钟)提前唤醒时由 RunAtMonotonic 按剩余时间重挂。
	// 单测 / 无 loop 的宿主挂不上(RunAtMonotonic 返回 false):同 ArmTravelReplyWatchdog,只靠落地回调与
	// "退出优先"收敛;生产节点另有冻结上限兜底。
	RunAtMonotonic(travel_freeze_cap::Clock::now() + travel_freeze_cap::kSaveBudget, [playerId, frozenAtMs]()
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
		travel_handoff_stats::Inc(travel_handoff_stats::Get().saveWatchdogFired);
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
	// 该玩家上一次作废的交接留下的 handoff 标记还没确认撤回(Redis 不通 / 应答丢失,见
	// handoff_mark_withdraw.h):标记可能带着当前 owner_epoch 还活着,此时发出的跨节点 EnterScene 会
	// 凭它免存盘过换手门 → 回档。fail-closed:确认撤回(或标记 TTL 到期)之前一律不替他发。
	// 表按 player_id 记,玩家退出后重连回本节点换了实体也照样拦得住。不是 per-tick 路径,平时表为空。
	if (const auto *guid = tlsEcs.actorRegistry.try_get<Guid>(player);
		guid != nullptr && tlsHandoffWithdrawQueue.HasPlayer(*guid))
	{
		// INFO 而非 WARN:Redis 不通的最长 305s 里客户端连点 / 队伍跟随会反复走到这里,而调用方各自
		// 还有一条拒绝日志;故障本身已由 WithdrawMark 的首发 ERROR 与每轮 WARN 汇总报出。
		LOG_INFO << "[ZoneTravel][WithdrawMark] scene change refused for player " << *guid
				 << ": withdrawal of a stale handoff mark not yet confirmed";
		return true;
	}
	const auto *pending = tlsEcs.actorRegistry.try_get<PlayerSceneChangeInFlightComp>(player);
	if (pending == nullptr)
	{
		return false;
	}
	// 时钟回拨(now < sentAtMs)按已过期处理:宁可多放一条请求,也不能把玩家长期挡在换图之外。
	const uint64_t nowMs = TimeSystem::NowMillisecondsUTC();
	return nowMs >= pending->sentAtMs && nowMs - pending->sentAtMs < SceneChangeInFlightTtlMs();
}

uint64_t PlayerLifecycleSystem::RequestSceneChange(entt::entity player, entt::entity smEntity,
												   ::scene_manager::EnterSceneRequest &req, bool playerRequested)
{
	// 先取号、先登记,再发:应答是异步的,不会在发送返回之前回来,但"等待者记下的号"必须在发出之前就位。
	const uint64_t correlationId = NextEnterSceneCorrelationId();
	if (tlsEcs.actorRegistry.valid(player))
	{
		// 一次换图请求一次,不是 per-tick 路径,emplace_or_replace 合规(AGENTS §7.5)。
		// replace 语义是有意的:TTL 过期后放行的下一条请求要覆盖上一条的目标与号 —— 上一条的迟到应答因此
		// 对不上号而被丢弃,不会再摘掉这一条的记录。
		// 目标直接取自 req:18 起交接时用的就是真正发出去的那个目标,不会与记下的分叉。
		// scene_conf_id 窄化成 uint32,与 StartTravelHandoff / PlayerTravelHandoffComp.sceneConfigId 同一类型。
		auto &pending = tlsEcs.actorRegistry.emplace_or_replace<PlayerSceneChangeInFlightComp>(player);
		pending.sceneId = req.scene_id();
		pending.sceneConfigId = static_cast<uint32_t>(req.scene_conf_id());
		pending.sentAtMs = TimeSystem::NowMillisecondsUTC();
		pending.playerRequested = playerRequested;
		pending.correlationId = correlationId;
	}
	SendCorrelatedEnterScene(smEntity, req, correlationId);
	return correlationId;
}

void PlayerLifecycleSystem::DispatchEnterSceneReply(const ::scene_manager::EnterSceneResponse &resp)
{
	using enter_scene_reply::Route;

	const Guid playerId = resp.player_id();
	const uint64_t replyTag = resp.correlation_id();
	if (playerId == 0)
	{
		// 更老的 scene_manager 连 player_id 都不回显:对不回玩家,交接只能靠应答看门狗收敛。
		LOG_DEBUG << "[EnterSceneReply] reply without player_id (scene_manager too old to echo it); ignoring"
				  << " corr=" << replyTag << " error_code=" << resp.error_code();
		return;
	}
	if (replyTag == 0)
	{
		// 本进程的 EnterScene 全经 SendCorrelatedEnterScene 发出、号恒非 0,所以号为 0 = scene_manager 没回显
		// (旧版)或有发送点绕过了统一出口。退回按 player_id 对应答(Classify 的 legacy 分支),不 fail-closed
		// (理由见头文件 enter_scene_reply)。计数让运维看得见这个状态;WARN 每线程只打一次,免得滚动窗口里刷屏。
		travel_handoff_stats::Inc(travel_handoff_stats::Get().replyUncorrelated);
		EnsureTravelHandoffStatsTimer();
		if (!tlsWarnedUncorrelatedReply)
		{
			tlsWarnedUncorrelatedReply = true;
			LOG_WARN << "[EnterSceneReply] scene_manager 未回显 correlation_id,退回按 player_id 对应答(首见 player="
					 << playerId << ");此后同类应答只计 reply_uncorrelated,不再逐条告警";
		}
	}

	const auto playerEntity = tlsEcs.GetPlayer(playerId);
	if (!tlsEcs.actorRegistry.valid(playerEntity))
	{
		// 疏散 / 排空改派的应答只会落在这里(改派发出时实体随即销毁):先交待确认表按号认领,认领后只触发核实。
		// 挂在"实体已不在"之后而不是实体查找之前:待确认条目存在期间本节点要么没有该玩家实体,要么条目已在等载入
		// (那时应答本来就该忽略);实体有效时应答照旧进下面的 Classify,不会被按 player_id 吞掉。
		if (ClaimRelocateEnterSceneReply(playerId, replyTag, resp))
		{
			return;
		}
		// 不认领的:玩家在交接途中退出,或没被跟踪(表满)的改派。
		LOG_INFO << "[ZoneTravel] EnterScene reply for player " << playerId
				 << " but entity is gone (exited during travel); ignoring corr=" << replyTag
				 << " error_code=" << resp.error_code();
		return;
	}

	const auto *travel = tlsEcs.actorRegistry.try_get<PlayerTravelHandoffComp>(playerEntity);
	const auto *pending = tlsEcs.actorRegistry.try_get<PlayerSceneChangeInFlightComp>(playerEntity);
	const bool handoffInFlight = travel != nullptr;
	const uint64_t handoffTag = handoffInFlight ? travel->enterSceneCorrelationId : 0;
	const bool sceneChangeInFlight = pending != nullptr;
	const uint64_t sceneChangeTag = sceneChangeInFlight ? pending->correlationId : 0;
	const Route route =
		enter_scene_reply::Classify(replyTag, handoffInFlight, handoffTag, sceneChangeInFlight, sceneChangeTag);

	switch (route)
	{
	case Route::kTravelHandoff:
	case Route::kLegacyTravelHandoff:
		HandleTravelEnterSceneReply(playerEntity, playerId, resp, /*correlated=*/route == Route::kTravelHandoff);
		return;

	case Route::kSceneChange:
	case Route::kLegacySceneChange:
	{
		// 先抄后摘:按值交给处理函数。remove 之后 pending 指针即失效,StartTravelHandoff 也会摘这个组件。
		const PlayerSceneChangeInFlightComp target = *pending;
		tlsEcs.actorRegistry.remove<PlayerSceneChangeInFlightComp>(playerEntity);
		HandleSceneChangeEnterSceneReply(playerEntity, playerId, target, resp);
		return;
	}

	case Route::kSceneChangeDuringHandoff:
		// 按构造不可达:StartTravelHandoff 起交接时摘掉在途换图组件,交接期间 IsSceneChangeBusy 挡住新的普通换图。
		// 出现即说明有路径破坏了这条不变量。丢弃而不是走普通换图分支:那会在交接中给客户端补一条失败 tip,
		// 18 还会试图再起一次交接。
		travel_handoff_stats::Inc(travel_handoff_stats::Get().replyUnmatched);
		EnsureTravelHandoffStatsTimer();
		LOG_ERROR << "[EnterSceneReply] invariant broken: scene-change reply while a handoff is in flight, player="
				  << playerId << " corr=" << replyTag << " handoff_tag=" << handoffTag
				  << " scene_change_tag=" << sceneChangeTag << " error_code=" << resp.error_code()
				  << " has_redirect=" << resp.has_redirect() << "; dropping";
		return;

	case Route::kUnmatched:
		// 不是任何等待者的应答(上一代交接 / 被顶替的请求 / 过了 TTL 才到的,或交接的 EnterScene 还没发)。
		// 外来的 redirect 也只记录、不销毁:scene_manager 若真凭本代标记放行了它,本代自己的请求会被拒,
		// 由 ResolveTravelOutcome 按 epoch 收敛。
		if (enter_scene_reply::IsSuspiciousUnmatched(route, handoffInFlight, resp.error_code() != 0,
													 resp.has_redirect()))
		{
			travel_handoff_stats::Inc(travel_handoff_stats::Get().replyUnmatched);
			EnsureTravelHandoffStatsTimer();
			LOG_INFO << "[EnterSceneReply] dropped unmatched reply player=" << playerId << " corr=" << replyTag
					 << " route=" << enter_scene_reply::RouteName(route) << " handoff_tag=" << handoffTag
					 << " scene_change_tag=" << sceneChangeTag << " error_code=" << resp.error_code()
					 << " has_redirect=" << resp.has_redirect();
		}
		else
		{
			// 路由先到(EnterScene 3.1 已摘在途组件)之后才到、又撞上新一条换图的纯成功迟到应答:常态,不计数。
			LOG_DEBUG << "[EnterSceneReply] late success reply for an older request, player=" << playerId
					  << " corr=" << replyTag << " scene_change_tag=" << sceneChangeTag << "; ignoring";
		}
		return;

	case Route::kNoWaiter:
	case Route::kCount:
		break;
	}
	// kNoWaiter:未跟踪(表满)的改派、退出后重建的实体、路由已先落地(EnterScene 3.1 已摘)的成功应答,或看门狗已先
	// 核实过的迟到应答。没有事可做。(kCount 不会由 Classify 返回,一并落到这里。)
	LOG_DEBUG << "[ZoneTravel] EnterScene reply for player " << playerId
			  << " without an in-flight handoff or scene change; ignoring corr=" << replyTag
			  << " route=" << enter_scene_reply::RouteName(route) << " error_code=" << resp.error_code()
			  << " has_redirect=" << resp.has_redirect();
}

void PlayerLifecycleSystem::DispatchEnterSceneTransportFailure(const ::scene_manager::EnterSceneRequest &req,
															   const std::string &reason)
{
	using enter_scene_reply::Route;

	const Guid playerId = req.player_id();
	const uint64_t requestTag = req.correlation_id();
	if (requestTag == 0)
	{
		// 本进程的 EnterScene 全经 SendCorrelatedEnterScene 发出、号恒非 0。号为 0 只能是有发送点绕过了统一出口:
		// 不退回按 player_id 对 —— 那会把"吃掉别的请求"的串号带回来。交给各等待者自己的兜底(TTL / 看门狗)。
		LOG_ERROR << "[EnterSceneReply] EnterScene transport failure without correlation_id for player " << playerId
				  << " (a send bypassed SendCorrelatedEnterScene); ignoring (" << reason << ")";
		return;
	}

	const auto playerEntity = tlsEcs.GetPlayer(playerId);
	if (!tlsEcs.actorRegistry.valid(playerEntity))
	{
		// 先交待确认表按号认领(疏散 / 排空发的 EnterScene,发完实体就销毁了):认领后当作结果未知,等到 settle 才核实。
		// 不认领的是途中退出的玩家,或没被跟踪(表满)的改派。
		if (ClaimRelocateTransportFailure(playerId, requestTag, reason))
		{
			return;
		}
		LOG_INFO << "[EnterSceneReply] EnterScene transport failure for player " << playerId
				 << " but entity is gone; ignoring corr=" << requestTag << " (" << reason << ")";
		return;
	}

	const auto *travel = tlsEcs.actorRegistry.try_get<PlayerTravelHandoffComp>(playerEntity);
	const auto *pending = tlsEcs.actorRegistry.try_get<PlayerSceneChangeInFlightComp>(playerEntity);
	const bool handoffInFlight = travel != nullptr;
	const uint64_t handoffTag = handoffInFlight ? travel->enterSceneCorrelationId : 0;
	const bool sceneChangeInFlight = pending != nullptr;
	const uint64_t sceneChangeTag = sceneChangeInFlight ? pending->correlationId : 0;
	const Route route =
		enter_scene_reply::Classify(requestTag, handoffInFlight, handoffTag, sceneChangeInFlight, sceneChangeTag);

	switch (route)
	{
	case Route::kTravelHandoff:
	case Route::kLegacyTravelHandoff:
		// 结果未知,不是"被拒":scene_manager 可能已铸造 epoch、正走 Kafka 路由 / 回滚窗口。不当失败证据去解冻
		// (已被放行时解冻并带新 epoch 存盘 = 回档),也不提前核实(读到窗口中间的 epoch,见 travel_freeze_cap.h
		// kReplyBudget 的下限约束)。去留照旧由应答看门狗 / 冻结上限按 owner_epoch 裁决。
		LOG_WARN << "[ZoneTravel] handoff EnterScene transport failure for player " << playerId
				 << " corr=" << requestTag << " (" << reason
				 << "); outcome unknown, leaving the verdict to the reply watchdog / freeze cap";
		return;

	case Route::kSceneChange:
	case Route::kLegacySceneChange:
	{
		// 先抄后摘:与 DispatchEnterSceneReply 同一纪律。槽位释放后玩家可以重试。
		const PlayerSceneChangeInFlightComp target = *pending;
		tlsEcs.actorRegistry.remove<PlayerSceneChangeInFlightComp>(playerEntity);
		if (!target.playerRequested)
		{
			// 队伍跟随:玩家没在等,只记日志(team-system.md DV-6;组队线约定)。
			LOG_INFO << "[ZoneTravel] team-follow EnterScene transport failure for player " << playerId
					 << " scene_id=" << target.sceneId << " corr=" << requestTag << " (" << reason
					 << "); player stays in the current scene";
			return;
		}
		// 回专用码 kEnterSceneServerBusy(「服务器繁忙,请稍后再试」)而不是 kEnterSceneFailed:传输失败时
		// scene_manager 可能已执行、路由事件随后到达,断言"换图失败"会出现先报失败后被搬走;不回任何提示则
		// 客户端一直等一个不会来的 EnterSceneS2C(EnterSceneC2S 的同步应答早已返回"已受理")。客户端收到任意
		// tip 即结束同区换图的等待并按码显示文案(mmorpg-client GameClient.DescribeTravelTip);本码**不**属于
		// 客户端 IsTravelFailureTip 认的"确定没成",所以不会被当成传送失败证据。取舍见设计文档 §5 #2。
		LOG_WARN << "[ZoneTravel] EnterScene transport failure for player " << playerId
				 << " scene_id=" << target.sceneId << " scene_conf_id=" << target.sceneConfigId
				 << " corr=" << requestTag << " (" << reason << "); notifying the client";
		PlayerTipSystem::SendToPlayer(playerEntity, kEnterSceneServerBusy, {});
		return;
	}

	case Route::kSceneChangeDuringHandoff:
		// 按构造不可达(见 DispatchEnterSceneReply 同一分支)。丢弃:走普通换图分支会在交接中给客户端补一条 tip。
		LOG_ERROR << "[EnterSceneReply] invariant broken: scene-change transport failure while a handoff is in flight,"
				  << " player=" << playerId << " corr=" << requestTag << " handoff_tag=" << handoffTag
				  << " scene_change_tag=" << sceneChangeTag << " (" << reason << "); dropping";
		return;

	case Route::kUnmatched:
		// 被顶替的请求(过了 TTL 又发了一条)或上一代交接的请求:不是当前等待者的,不动它。
		LOG_INFO << "[EnterSceneReply] transport failure of a superseded EnterScene for player " << playerId
				 << " corr=" << requestTag << " handoff_tag=" << handoffTag << " scene_change_tag=" << sceneChangeTag
				 << " (" << reason << "); ignoring";
		return;

	case Route::kNoWaiter:
	case Route::kCount:
		break;
	}
	// kNoWaiter:路由已先落地(EnterScene 3.1 已摘在途组件)之后才到的失败,或没有等待者的发送。
	LOG_INFO << "[EnterSceneReply] EnterScene transport failure for player " << playerId
			 << " without an in-flight handoff or scene change; ignoring corr=" << requestTag << " route="
			 << enter_scene_reply::RouteName(route) << " (" << reason << ")";
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
		// 相等 = 同 zone 重发后落回了本节点(同物理节点不铸造 epoch),本实体就是合法持有者,照常复用;
		//        EnterScene 3.2 步放进场景后立刻走 ResolveTravelOutcome 核实并静默解冻,不等应答
		//        (应答可能丢),随后到达的成功应答是 no-op;
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
	// 更新的一代也可能不是"别处玩过":scene_manager 推路由失败后把归属单调回滚到本节点(epoch E → E+2,
	// location 按原字节指回本节点、带本次回执),本节点还没取证采纳,同落点重连的路由就带着 E+2 先到了(判定表 B11)。
	// 这时同样销毁重载:交接发起后本实体不再写盘,Redis 包含冻结内存(骨架 I1),重载零损失;随后到达的取证回调
	// 按代际找不到这一次交接(重载出的新实体没有交接组件)而 no-op。
	LOG_WARN << "[ZoneTravel] re-entry with newer owner_epoch " << incomingOwnerEpoch << " (cached " << cachedEpoch
			 << ") hit a stale in-handoff entity for player " << playerId
			 << "; discarding it so the player is reloaded from storage";
	// 这也是一次"放行了但应答没到"的终态,只是先于看门狗由再入路由发现。
	travel_handoff_stats::Inc(travel_handoff_stats::Get().grantedWithoutReply);
	ObserveTravelHandoffEnded(player, travel_handoff_stats::Get().granted);
	DestroyDeposedPlayer(playerId, "handoff_superseded_by_reentry");
	return true;
}

bool PlayerLifecycleSystem::DiscardDeposedEntityOnReentry(entt::entity player, uint64_t incomingOwnerEpoch)
{
	// 不限定"退出中"(复审 ownership-minor):不带 UnregisterPlayer 的活僵尸同样会被复用成真身 —— 意图缺失分支
	// 摘掉 UnregisterPlayer 后保留的实体(M9),或 gate 崩溃没代发 ExitGame 的实体。它们内存不再变化,周期存盘
	// 一直走快路径不写盘,永远不会被 CAS 拒掉而自行销毁。交接已发起的实体由 DiscardStaleHandoffEntity 先处理。
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return false;
	}
	uint64_t cachedEpoch = 0;
	if (const auto *epochComp = tlsEcs.actorRegistry.try_get<PlayerOwnerEpochComp>(player))
	{
		cachedEpoch = epochComp->epoch;
	}
	if (!player_exit::IsDeposedOnReentry(cachedEpoch, incomingOwnerEpoch))
	{
		// 恰好 +1(落回本节点这一次自己铸的)/ 不新 / 上游未填或缓存为 0(未知):照常复用,
		// 退出中的由 EnterScene 第 0 步取消退出(判据推导见 player_exit::IsDeposedOnReentry)。
		return false;
	}
	const auto *guid = tlsEcs.actorRegistry.try_get<Guid>(player);
	if (guid == nullptr)
	{
		return false;
	}
	const Guid playerId = *guid;
	const bool exiting = tlsEcs.actorRegistry.any_of<UnregisterPlayer>(player);
	// 退出中:这个退出曾被保留到租约之后(重存超限 / 存盘失败重试中);活实体:本节点上留着没退出的僵尸。
	// 两者都是期间 location 被删、玩家在别的节点玩过又被派回本节点。盘上是别处的最新状态,本实体是旧状态:
	// 复用它 = 旧内存成真身,下一次带新 epoch 的存盘覆盖别处的进度(§12.6.1(d) 回档)。它自己带旧 epoch 的
	// 在途 / 之后的写本来就会被 CAS 拒,丢弃不会丢掉任何本可以落盘的改动。
	LOG_WARN << "HandleReentry: player " << playerId << " re-entered with owner_epoch " << incomingOwnerEpoch
			 << " while a stale " << (exiting ? "exiting" : "live") << " entity (cached " << cachedEpoch
			 << ") was still retained; discarding it so the player is reloaded from storage"
			 << " (metric=exit_deposed_on_reentry)";
	exit_persist_stats::Inc(exit_persist_stats::Get().deposedOnReentry);
	DestroyDeposedPlayer(playerId, exiting ? "exit_superseded_by_reentry" : "live_superseded_by_reentry");
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

	// 晚发闸(set_mark 阶段):冻结已超过 travel_freeze_cap::kDispatchWindow(35s)就不再写标记。此刻 SET 还没发出,
	// 不可能已被放行(不变量 I0),解冻安全。这一步保证之后发出的 SET / EnterScene 都来得及在冻结上限(70s)之前被
	// 应答看门狗 + 一次核实收敛。存盘看门狗(30s ≤ 35s)正常会先到并解冻,这里是纵深防御:只在它没能按时触发
	// (墙钟回拨把 muduo 定时器推迟了等)时才会生效。排在 epoch 检查之前:窗口已关时不论别的条件如何都不写。
	{
		const auto now = travel_freeze_cap::Clock::now();
		const auto frozenFor = ElapsedSinceFreeze(*travel, playerId, now);
		if (!travel_freeze_cap::IsDispatchWindowOpen(frozenFor))
		{
			travel_handoff_stats::Inc(travel_handoff_stats::Get().dispatchWindowClosed);
			LOG_WARN << "[ZoneTravel][DispatchWindow] player=" << playerId << " stage=set_mark frozen_ms="
					 << std::chrono::duration_cast<std::chrono::milliseconds>(frozenFor).count()
					 << ": handoff save landed too late to verify ownership before the freeze cap; not writing the mark";
			AbortTravelHandoff(playerId, "handoff dispatch window closed before the mark was sent");
			return;
		}
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
			if (reply != nullptr && reply->type == REDIS_REPLY_ERROR)
			{
				// 服务端明确报错 = SET 确定没生效。只处置同一代交接(代际 = nowMs),理由见头文件。
				HandleTravelMarkWriteRejected(playerId, nowMs, reply->str != nullptr ? reply->str : "");
				return;
			}
			if (reply == nullptr)
			{
				// 结果**未知**,不是「确定没写」:hiredis 在连接断开时会用空 reply 回调所有挂起
				// 命令(muduo_windows/.../Hiredis.cc),而这条 SET 可能已经被 Redis 执行了。
				// 此时不就地 AbortTravelHandoff:连接刚断,它的撤回(WithdrawHandoffMark)当场发不出去,
				// 只能挂进待撤回表等重连,玩家却已经解冻;保持冻结到核实完更稳妥。
				// (历史背景,现已不成立:WithdrawHandoffMark 落地之前,Abort 的撤回会被 connected() 守卫
				// 静默跳过,标记带着当前 epoch 残活到 TTL(300s);那 300s 内玩家任何一次跨节点 / 跨 zone
				// EnterScene 都会被 scene_manager 判成「标记 epoch == 当前 owner_epoch ⇒ 源已落盘」直接放行,
				// 目标节点读到旧档 —— 玩家回档,且 epoch 没变过、CAS 不响,全程零报错。现在残留标记由
				// 待撤回表 + IsSceneChangeBusy 兜住,见 handoff_mark_withdraw.h。)
				// 交给 ResolveTravelOutcome:Redis 不可用时保持冻结 + 计数 + 重挂应答看门狗;
				// 连接恢复后它的原子取证脚本先删掉本次交接这一族标记、再读 owner_epoch 与 location —— 正好把
				// 可能已落地的标记撤回,再按判定表决定解冻还是销毁。代际用 nowMs(与 travel->requestedAtMs 一致)。
				LOG_ERROR << "[ZoneTravel] SET handoff mark result unknown for player " << playerId
						  << " (connection dropped before reply); keeping the player frozen until it is verified";
				// 证据 kMarkWriteUnknown:EnterScene 根本没发出去,epoch 就算变了也不是本次交接推进的,永不踢线。
				ResolveTravelOutcome(playerId, nowMs, "handoff mark write result unknown",
									 travel_outcome::Evidence::kMarkWriteUnknown);
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
		// 命令没进 hiredis 的输出缓冲,标记确定没写:markEpoch 保持 0,Abort 不需要撤回。
		AbortTravelHandoff(playerId, "redis command dispatch failed");
		return;
	}
	// SET 已发出:记下写标记用的 epoch,交接作废时靠 "{markEpoch}:{requestedAtMs}" 还原标记原文做条件撤回。
	// 回调是异步的,不会在 command() 返回之前跑;command() 到这里之间也没有动过 registry,travel 指针仍有效。
	travel->markEpoch = ownerEpoch;
}

void PlayerLifecycleSystem::HandleTravelMarkWriteRejected(Guid playerId, uint64_t requestedAtMs, const char *err)
{
	const char *errText = err != nullptr ? err : "";
	const auto playerEntity = tlsEcs.GetPlayer(playerId);
	auto *travel = tlsEcs.actorRegistry.valid(playerEntity)
					   ? tlsEcs.actorRegistry.try_get<PlayerTravelHandoffComp>(playerEntity)
					   : nullptr;
	if (travel == nullptr || travel->requestedAtMs != requestedAtMs)
	{
		// 不是这一代交接的应答:实体已不在 / 交接已收尾,或者已经是新一代交接(旧一代被冻结上限销毁后同节点重登、
		// 又发起了一次,新一代的 SET 可能已经发出)。什么都不动:Abort 会把新一代错误解冻(违反 I2 / I3),还会按新一代
		// 的 markEpoch 登记撤回、删掉它刚写下的标记。
		LOG_WARN << "[ZoneTravel] SET handoff mark failed for player " << playerId << " requested_at_ms=" << requestedAtMs
				 << " err=" << errText << ", but that handoff generation is no longer in flight (current="
				 << (travel != nullptr ? travel->requestedAtMs : uint64_t{0}) << "); ignoring the late reply";
		return;
	}
	LOG_ERROR << "[ZoneTravel] SET handoff mark failed for player " << playerId << " requested_at_ms=" << requestedAtMs
			  << " err=" << errText;
	// 服务端明确报错 = SET 确定没生效,盘上没有这份标记,就地解冻是安全的(不变量 I0)。
	// 标记确定不存在,就不该让 Abort 去登记撤回:SET 被拒的典型原因(READONLY / LOADING)下撤回的 EVAL 同样会被拒、
	// 销不了账,玩家会被一个不存在的标记挡在换图之外直到截止时刻。
	travel->markEpoch = 0;
	// travel 指针到此为止不再使用:Abort 会摘组件。
	AbortTravelHandoff(playerId, "handoff mark write failed");
}

void PlayerLifecycleSystem::RequestTravelEnterScene(Guid playerId)
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

	// 走到这里 SET 已经拿到 OK:handoff 标记确定已写,scene_manager 随时可能凭它放行(不经本节点的落点也算 ——
	// 玩家断线后重登落到别的节点,IsSceneChangeBusy 拦不住,见 handoff_mark_withdraw.h 契约第二条)。按骨架,
	// 下面三种不发 EnterScene 的情形都**不许解冻**(AbortTravelHandoff 会违反不变量 I2 / I3),一律走
	// ConcludeHandoffAfterMarkSent:tip(会话活着时)+ 踢线 34 + 不存盘销毁。每个 return 之后都不再碰 travel 指针。
	const auto now = travel_freeze_cap::Clock::now();

	// 晚发闸(enter_scene 阶段):SET 的应答晚于 kDispatchWindow(35s)才到,说明 zone Redis 已严重异常,再发
	// EnterScene 其核实大概率赶不上冻结上限。本节点没发出过 EnterScene,不可能是本次交接放行的,会话仍绑在这里 ——
	// 立即收口,客户端约 35s 就能拿到结论而不是等到 70s;不踢的话就是一条哑连接,所以同 zone 也要踢。
	if (const auto frozenFor = ElapsedSinceFreeze(*travel, playerId, now);
		!travel_freeze_cap::IsDispatchWindowOpen(frozenFor))
	{
		travel_handoff_stats::Inc(travel_handoff_stats::Get().dispatchWindowClosed);
		LOG_WARN << "[ZoneTravel][DispatchWindow] player=" << playerId << " stage=enter_scene frozen_ms="
				 << std::chrono::duration_cast<std::chrono::milliseconds>(frozenFor).count()
				 << ": handoff mark reply arrived too late to verify ownership before the freeze cap;"
				 << " not sending EnterScene";
		ConcludeHandoffAfterMarkSent(playerId, travel_freeze_cap::MarkSentSite::kDispatchWindow, now);
		return;
	}

	// 传送中实体还活着,gate / session 直接从实体上取,不需要像疏散那样提前抄票据。
	const auto *session = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(playerEntity);
	if (session == nullptr || session->gate_session_id() == kInvalidSessionId)
	{
		// 没有会话:只销毁不踢(Conclude 自己按 HasLiveGateSession 判,这里判不到活会话)。
		LOG_WARN << "[ZoneTravel] no live gate session for player " << playerId
				 << " after the handoff mark was written; not sending EnterScene";
		ConcludeHandoffAfterMarkSent(playerId, travel_freeze_cap::MarkSentSite::kNoGateSession, now);
		return;
	}

	const auto smEntity = GetSceneManagerEntity(playerId);
	if (smEntity == entt::null)
	{
		// StartTravelHandoff 已在冻结之前预检过 SM:走到这里只剩"SM 恰好在 SET 往返那几毫秒里消失"的窄窗口。
		LOG_WARN << "[ZoneTravel] no SceneManager node reachable for player " << playerId
				 << " after the handoff mark was written; not sending EnterScene";
		ConcludeHandoffAfterMarkSent(playerId, travel_freeze_cap::MarkSentSite::kNoSceneManager, now);
		return;
	}

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

	// 应答按 correlation_id 对号(DispatchEnterSceneReply):先把号记到交接组件上再发,只有回显这个号的应答
	// 才算本代交接的证据。发送之前号为 0 = 交接的 EnterScene 还没发,这段时间到达的任何应答都不可能是它的。
	// 每代交接只走到这里一次(BeginTravelHandoff 的 SET 回调按 requestedAtMs 代际守着),看门狗只核实、不重发。
	const uint64_t correlationId = NextEnterSceneCorrelationId();
	travel->enterSceneCorrelationId = correlationId;
	SendCorrelatedEnterScene(smEntity, req, correlationId);
	// travel 指针在这里仍有效:发送只把请求交给 gRPC,不碰 registry。
	ArmTravelReplyWatchdog(playerId, travel->requestedAtMs, travel_outcome::Evidence::kNoReply,
						   "EnterScene reply timed out");

	LOG_INFO << "[ZoneTravel] requested EnterScene for player " << playerId
			 << " target_zone=" << travel->targetZoneId
			 << " scene_id=" << travel->sceneId
			 << " scene_conf_id=" << travel->sceneConfigId
			 << " session=" << session->gate_session_id()
			 << " corr=" << correlationId;
}

void PlayerLifecycleSystem::ArmTravelReplyWatchdog(Guid playerId, uint64_t requestedAtMs,
												   travel_outcome::Evidence evidence, std::string reason)
{
	// 只按值捕获 id + 代际 + 证据 + 原因文本,不绑任何对象(§11.7)。不保存 TimerId、不取消:到期时代际
	// 不符即 no-op,比维护一张定时器表更简单。截止时刻按单调时钟定,muduo(按墙钟)提前唤醒时由 RunAtMonotonic
	// 按剩余时间重挂(重挂时 lambda 随 std::function 复制,reason 一并复制)。
	const bool armed = RunAtMonotonic(
		travel_freeze_cap::Clock::now() + travel_freeze_cap::kReplyBudget,
		[playerId, requestedAtMs, evidence, reason = std::move(reason)]()
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
		// Redis 不可用时 ResolveTravelOutcome 会重挂本看门狗,每次到期都计一次(重挂另计 verify_rearmed)。
		// 证据用挂载时给的:首次挂载是 kNoReply;重挂时是当初那次裁决的证据,不在这里翻成超时。
		// 首次挂的这个不取消、总是先到期:若应答已到而没能裁决,ResolveTravelOutcome 会用组件上记下的
		// 应答证据覆盖这里的 kNoReply(travel_outcome::EffectiveEvidence)。
		travel_handoff_stats::Inc(travel_handoff_stats::Get().replyWatchdogFired);
		ResolveTravelOutcome(playerId, requestedAtMs, reason.c_str(), evidence);
	});
	if (!armed)
	{
		// 单测 / 无 loop 的宿主:没有看门狗(也没有 1s 上限扫描,单测直接调 EnforceTravelFreezeCaps),只靠应答与
		// "退出优先"收敛。生产节点必有 loop。
		LOG_WARN << "[ZoneTravel] no event loop on this thread; EnterScene reply watchdog not armed for player "
				 << playerId;
	}
}

void PlayerLifecycleSystem::ResolveTravelOutcome(Guid playerId, uint64_t requestedAtMs, const char *reason,
												 travel_outcome::Evidence incomingEvidence)
{
	const auto playerEntity = tlsEcs.GetPlayer(playerId);
	if (!tlsEcs.actorRegistry.valid(playerEntity))
	{
		return;
	}
	auto *travel = tlsEcs.actorRegistry.try_get<PlayerTravelHandoffComp>(playerEntity);
	if (travel == nullptr || travel->requestedAtMs != requestedAtMs)
	{
		return;
	}
	// 判定表第一刀(travel_freeze_cap::IsHandoffMarkSent):本函数只裁决"handoff 标记的 SET 已发出"的交接。调用方都在
	// SET 发出之后才进来(SET 回调 / 应答 / 应答看门狗 / EnterScene 3.2 都先判 requestedAtMs != 0),传进 0 是调用方的 bug。
	// 不走下面的取证:SET 没发出时读到别人推进的 epoch 会把一个本可原地恢复(冻结态那次存盘还可能没落地)的玩家销毁。
	// 也不在这里解冻:不动交接,交给存盘看门狗 / 冻结上限的"标记未发出"分支按判定表 A1 解冻。
	if (!travel_freeze_cap::IsHandoffMarkSent(requestedAtMs))
	{
		LOG_ERROR << "[ZoneTravel] ResolveTravelOutcome called for player " << playerId << " (" << reason
				  << ") before the handoff mark was sent (requested_at_ms=0); leaving the handoff to the save watchdog"
				  << " / freeze cap";
		return;
	}
	// 应答 / 路由落点带来的证据记到组件上:这次若因 Redis 不可用没能裁决,首次挂的 kNoReply 看门狗
	// (不取消)会先到期再进来,那时要用这里记下的证据,而不是它带来的 kNoReply(见头文件 travel_outcome)。
	if (incomingEvidence != travel_outcome::Evidence::kNoReply)
	{
		travel->hasRecordedEvidence = true;
		travel->recordedEvidence = static_cast<uint8_t>(incomingEvidence);
	}
	const travel_outcome::Evidence evidence =
		travel_outcome::EffectiveEvidence(travel->hasRecordedEvidence, travel->recordedEvidence, incomingEvidence);
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
		travel_handoff_stats::Inc(travel_handoff_stats::Get().verifyRearmed);
		ArmTravelReplyWatchdog(playerId, requestedAtMs, evidence, reason);
		return;
	}

	const std::string handoffKey = player_ownership::HandoffRedisKey(playerId);
	const std::string epochKey = player_ownership::OwnerEpochRedisKey(playerId);
	const std::string locationKey = player_ownership::LocationRedisKey(playerId);
	const std::string requestedAtText = std::to_string(requestedAtMs);
	const std::string reasonText = reason;
	// 一次 EVAL 完成取证:只删本次交接这一族标记 → 读 owner_epoch 与 location(脚本与"为什么"见
	// travel_outcome::kLuaJudgeTravelOutcome)。删除与读取在同一段原子脚本里,取代了改动前"同一条不重放连接上先 DEL
	// 后 MGET"的顺序保证(不变量 I0 机制 2);以后若拆回两条命令,必须走不重放的同一连接。
	// 回调只按值捕获 id + 代际 + 缓存 epoch + 原因文本 + 证据,回调里按 id 回查实体(§11.7,不绑对象)。
	// 脚本串作为一个 %s 参数传入是安全的:hiredis 只按**格式串**里的空格切参数(同 SendHandoffWithdraw);
	// 脚本里的 %d 是 Lua 的模式字符,不经 hiredis 格式化。
	const int ret = redis->command(
		[playerId, requestedAtMs, cachedEpoch, reasonText, dispatchedEvidence = evidence](hiredis::Hiredis *,
																							redisReply *reply)
		{
			JudgeTravelOutcomeReply(playerId, requestedAtMs, cachedEpoch, reasonText, dispatchedEvidence, reply);
		},
		"EVAL %s 3 %s %s %s %s", travel_outcome::kLuaJudgeTravelOutcome, handoffKey.c_str(), epochKey.c_str(),
		locationKey.c_str(), requestedAtText.c_str());
	if (ret != REDIS_OK)
	{
		LOG_ERROR << "[ZoneTravel] redis command dispatch failed while verifying travel outcome for player "
				  << playerId << "; keeping the player frozen and re-arming the watchdog";
		travel_handoff_stats::Inc(travel_handoff_stats::Get().verifyRearmed);
		ArmTravelReplyWatchdog(playerId, requestedAtMs, evidence, reasonText);
	}
}

void PlayerLifecycleSystem::JudgeTravelOutcomeReply(Guid playerId, uint64_t requestedAtMs, uint64_t cachedEpoch,
													const std::string &reasonText,
													travel_outcome::Evidence dispatchedEvidence, const redisReply *reply)
{
	using travel_outcome::Ownership;

	const auto entity = tlsEcs.GetPlayer(playerId);
	if (!tlsEcs.actorRegistry.valid(entity))
	{
		return;
	}
	auto *current = tlsEcs.actorRegistry.try_get<PlayerTravelHandoffComp>(entity);
	if (current == nullptr || current->requestedAtMs != requestedAtMs)
	{
		return;
	}
	// 按回调这一刻的组件再取一次:看门狗这次核实发出之后、回来之前到达的应答已把证据记到组件上,
	// 以应答证据为准。
	const travel_outcome::Evidence evidence =
		travel_outcome::EffectiveEvidence(current->hasRecordedEvidence, current->recordedEvidence, dispatchedEvidence);
	// 判定表 B2:脚本应答必须是两元素数组,owner_epoch 那一项只能是字符串(INCR 产生的十进制)或 NIL(从未铸造 / 类型
	// 不对,MGET 都给 NIL)。空应答(连接断开)/ ERROR(只读副本、MISCONF、OOM、ACL 拒 EVAL:#!lua 让它们整体被拒,
	// 删除一并没执行)/ 其余形状都判不清归属:保持冻结、证据原样重挂看门狗,不解冻,终局交给冻结硬上限。
	const bool replyShapeOk = reply != nullptr && reply->type == REDIS_REPLY_ARRAY && reply->elements == 2 &&
							  reply->element[0] != nullptr &&
							  (reply->element[0]->type == REDIS_REPLY_STRING || reply->element[0]->type == REDIS_REPLY_NIL);
	if (!replyShapeOk)
	{
		LOG_ERROR << "[ZoneTravel] travel outcome judge script failed or returned a malformed reply for player "
				  << playerId << " (" << reasonText << ", evidence=" << travel_outcome::EvidenceName(evidence)
				  << (reply != nullptr && reply->type == REDIS_REPLY_ERROR && reply->str != nullptr
						  ? std::string(", err=") + reply->str
						  : std::string())
				  << "); keeping the player frozen and re-arming the watchdog";
		travel_handoff_stats::Inc(travel_handoff_stats::Get().verifyRearmed);
		ArmTravelReplyWatchdog(playerId, requestedAtMs, evidence, reasonText);
		return;
	}
	// 缺键按 0(scene_manager 从未铸造 / 键被淘汰);值由 INCR 产生,必为十进制整数。
	const redisReply *epochElement = reply->element[0];
	uint64_t redisEpoch = 0;
	if (epochElement->type == REDIS_REPLY_STRING && epochElement->str != nullptr)
	{
		redisEpoch = std::strtoull(epochElement->str, nullptr, 10);
	}

	// location 只解析这一次:判定(ClassifyOwnership)与下面"已放行"分支的踢线判据共用。
	storage::PlayerLocation location;
	const bool locationParsed = player_ownership::ParsePlayerLocationElement(reply->element[1], location);
	travel_outcome::OwnershipFacts facts;
	facts.cachedEpoch = cachedEpoch;
	facts.markEpoch = current->markEpoch;
	facts.redisEpoch = redisEpoch;
	facts.locationParsed = locationParsed;
	if (locationParsed)
	{
		// 与 player_team.cpp 队伍跟随同一判法:PlayerLocation.node_id 是十进制字符串(scene_manager FormatUint 写入)。
		// 本 zone 为 0(节点身份还没就位)时一律不算"指回本节点",落向不采纳的一侧。
		facts.locationOnSelf = GetZoneId() != 0 && location.zone_id() == GetZoneId() &&
							   location.node_id() == std::to_string(GetNodeInfo().node_id());
		facts.locationOwnerEpoch = location.owner_epoch();
		// 回执只认与本次标记原文逐字节相同的那一份:陈旧回执(更早的交接,requestedAtMs 不同)与回滚链留下的转写形态
		// ("E+2k:t" 被第三方凭它铸造后又回滚)都对不上,一律不采纳。
		facts.receiptIsMine = !location.rollback_receipt().empty() && current->markEpoch != 0 &&
							  location.rollback_receipt() ==
								  player_ownership::HandoffRedisValue(current->markEpoch, requestedAtMs);
	}
	const Ownership ownership = travel_outcome::ClassifyOwnership(facts);

	switch (ownership)
	{
	case Ownership::kUnchanged:
		// B4 归属没动。失败应答 / 超时 / 写标记结果未知 / 协议异常 → 交接未成,解冻并回失败 tip;
		// 成功(同 zone)→ 重发后落回了本节点(同物理节点不铸造 epoch),换图由 PlayerEnterGameNode → EnterScene
		// 就地完成,静默解冻,不发失败 tip。
		// 先把 markEpoch 清 0:脚本已在同一原子步骤里删掉本族标记,不再登记撤回 —— 否则撤回的应答回来之前 Redis 一断,
		// 待撤回表会让 IsSceneChangeBusy 把这个玩家挡在换图之外最长约 305s。
		current->markEpoch = 0;
		// current 指针到此为止不再使用:Abort 会摘组件。
		AbortTravelHandoff(playerId, reasonText.c_str(),
						   /*notifyFailure=*/evidence != travel_outcome::Evidence::kSucceeded);
		return;

	case Ownership::kRolledBackToSelf:
	{
		// B5 scene_manager 推路由失败,已把本次交接的铸造(E → E+1)单调回滚到本节点:location 按原字节指回本节点、
		// owner_epoch 再前进一格到 E+2,回执 = 本次标记原文(与 INCR 同一段 Lua 原子写入)。本族标记(含转写出来的
		// "E+2:t")已被本次脚本原子删掉,此后再没有凭它的铸造;E+2 之后能换属主的只剩本节点自己以后写的标记,以及
		// 不凭标记的路径(location 为空 / 等待落点 / 死节点 / zone 下线 / dev 旁路),都不适用于指向活节点的 location。
		// 所以本节点仍是唯一属主,采纳 E+2 并解冻。这是"节点不得自己读 Redis 取 epoch"(CZ-3)的唯一例外,三条前提缺一
		// 不可:原子脚本先删本族标记;回执逐字节等于本次标记原文;location 指回本节点本 zone(ClassifyOwnership 已核)。
		auto *epochComp = tlsEcs.actorRegistry.try_get<PlayerOwnerEpochComp>(entity);
		if (epochComp == nullptr)
		{
			// 按构造不可能(cachedEpoch 与 markEpoch 都来自它,判得出 B5 就说明它在)。拿不到就无处采纳,
			// 按回执异常收口(B6),不解冻。
			LOG_ERROR << "[ZoneTravel][RollbackAdopt] player " << playerId << " was rolled back to this node (receipt="
					  << location.rollback_receipt() << ") but has no PlayerOwnerEpochComp to adopt owner_epoch "
					  << redisEpoch << " into; concluding as a receipt anomaly (metric=rollback_receipt_anomaly)";
			// 推迟销毁(有未落地存盘)时不计数,由看门狗 / 冻结上限重判(同冻结上限那一支的写法)。
			if (ConcludeHandoffAfterMarkSent(playerId, travel_freeze_cap::MarkSentSite::kReceiptAnomaly,
											 travel_freeze_cap::Clock::now()))
			{
				travel_handoff_stats::Inc(travel_handoff_stats::Get().rollbackReceiptAnomaly);
			}
			return;
		}
		LOG_WARN << "[ZoneTravel][RollbackAdopt] player " << playerId
				 << " handoff was rolled back to this node (receipt=" << location.rollback_receipt() << ", "
				 << reasonText << ", evidence=" << travel_outcome::EvidenceName(evidence) << "): owner_epoch "
				 << cachedEpoch << " -> " << redisEpoch << "; adopting and unfreezing (metric=rolled_back_adopted)";
		epochComp->epoch = std::max(epochComp->epoch, redisEpoch);
		current->markEpoch = 0; // 本族标记已被脚本删掉,不登记撤回(理由同 B4)
		travel_handoff_stats::Inc(travel_handoff_stats::Get().rolledBackAdopted);
		// current 指针到此为止不再使用:Abort 会摘交接组件与冻结组件(实体保留)。
		AbortTravelHandoff(playerId, reasonText.c_str(),
						   /*notifyFailure=*/evidence != travel_outcome::Evidence::kSucceeded);
		// 强制写一次(跳过 dirty-save 快路径):让 DBTask(E+2) 立刻越过幽灵持有者可能落下的 DBTask(E+1),并恢复
		// "快照 == Redis"的前提(见头文件 SavePlayerToRedisImpl)。交接组件已摘,"交接已发起不得再写"不再拦它。
		// 实体正在退出(退出链 M4 在等一笔更早的存盘)时同样写:它成为最新一份,退出分支按收敛规则收尾,
		// A1′ 按 E+2 条件写。
		if (!SavePlayerToRedisImpl(entity, /*allowSkipWhenPersisted=*/false))
		{
			// 按构造不可达(实体有效、交接组件已摘时强制存盘必然写盘)。只留证据:内存与盘上同为冻结态那一份
			// (骨架 I1),不写也不丢数据,只是少了上面两条好处,下一次周期存盘会补上。
			LOG_ERROR << "[ZoneTravel][RollbackAdopt] player " << playerId
					  << " adopted owner_epoch " << redisEpoch << " but the forced save was not issued";
		}
		return;
	}

	case Ownership::kReceiptAnomaly:
		// B6 回执是本次标记原文,但"回滚之后再无新落点"的交叉校验不成立:owner_epoch 键被淘汰后补种、新旧 scene_manager
		// 混跑、数据写坏 …… 判不清,不采纳、不解冻,按"标记已发出"收口(tip + 踢线 34 + 不存盘销毁)。可以踢线:回执是本次
		// 原文说明回滚之后没有任何新落点,能用本会话发 EnterScene 的只有冻结中的本节点,会话不可能被合法地改绑到别处。
		LOG_ERROR << "[ZoneTravel][RollbackAdopt] player " << playerId << " rollback receipt matches this handoff ("
				  << location.rollback_receipt() << ") but the cross-check failed: cached_epoch=" << cachedEpoch
				  << " mark_epoch=" << facts.markEpoch << " redis_epoch=" << redisEpoch
				  << " location_epoch=" << facts.locationOwnerEpoch << " location_on_self=" << facts.locationOnSelf
				  << " (" << reasonText << ", evidence=" << travel_outcome::EvidenceName(evidence)
				  << "); not adopting (metric=rollback_receipt_anomaly)";
		// current 指针到此为止不再使用:Conclude 会销毁实体(有未落地存盘时推迟,由看门狗 / 冻结上限扫描重判)。
		// 推迟时不计数:看门狗重判会再次走到这里,只在真正收口的那一次计数(同冻结上限那一支的写法)。
		if (ConcludeHandoffAfterMarkSent(playerId, travel_freeze_cap::MarkSentSite::kReceiptAnomaly,
										 travel_freeze_cap::Clock::now()))
		{
			travel_handoff_stats::Inc(travel_handoff_stats::Get().rollbackReceiptAnomaly);
		}
		return;

	case Ownership::kReturnedToSelf:
		// B7 epoch 变了、没有本次回执,location 却指回本节点本 zone:已放行后又回到本节点(另一次交接把他派了回来)、
		// 回滚链(回执是转写形态)、或 owner_epoch 键被淘汰读成 0。都不是"本次交接被回滚到我",不解冻 —— 沿用下面的
		// "已放行"分支不存盘销毁;路由若随后到达本节点,按"无实体"从盘上加载(零损失)。不踢:回到本节点的会话可能
		// 正合法地绑在本节点上(判据 c 对指向活节点的 location 必然不成立,下面自然不会踢)。
		LOG_WARN << "[ZoneTravel] player " << playerId << " location points back to this node but owner_epoch moved "
				 << cachedEpoch << " -> " << redisEpoch << " without this handoff's rollback receipt (receipt="
				 << location.rollback_receipt() << ", " << reasonText << ", evidence="
				 << travel_outcome::EvidenceName(evidence) << "); treating it as granted, not unfreezing"
				 << " (metric=returned_after_grant)";
		travel_handoff_stats::Inc(travel_handoff_stats::Get().returnedAfterGrant);
		break;

	case Ownership::kMovedElsewhere:
	case Ownership::kLocationUnknown:
	case Ownership::kCount:
		// B8 / B9:已放行(location 在别处,或已删 / 写坏)。kCount 不会由 ClassifyOwnership 返回,一并按已放行处理
		// (销毁一侧,fail-closed)。
		break;
	}

	// ── "已放行"分支(B7 / B8 / B9)──
	if (evidence == travel_outcome::Evidence::kSucceeded)
	{
		// 同 zone 放行的正常收尾:目标节点已拿到新 epoch,本节点不再持有该玩家。
		LOG_INFO << "[ZoneTravel] same-zone handoff granted for player " << playerId << ": owner_epoch " << cachedEpoch
				 << " -> " << redisEpoch << " (" << travel_outcome::OwnershipName(ownership)
				 << "); destroying source-side entity";
		ObserveTravelHandoffEnded(entity, travel_handoff_stats::Get().granted);
		DestroyDeposedPlayer(playerId, "scene_handoff_granted", /*routine=*/true);
		return;
	}
	LOG_WARN << "[ZoneTravel] travel for player " << playerId << " was granted although the reply was lost/failed ("
			 << reasonText << ", evidence=" << travel_outcome::EvidenceName(evidence) << "): owner_epoch "
			 << cachedEpoch << " -> " << redisEpoch << " (" << travel_outcome::OwnershipName(ownership)
			 << "); destroying source-side entity";
	travel_handoff_stats::Inc(travel_handoff_stats::Get().grantedWithoutReply);
	ObserveTravelHandoffEnded(entity, travel_handoff_stats::Get().granted);

	// 受理后未成且无法在原地恢复:玩家已不在本节点,客户端却没拿到任何结果(跨 zone 的会话仍绑在
	// 本节点)。满足全部条件才在销毁之前回失败 tip + 踢线 34,让它断线回选服重登;判据与理由见
	// 头文件 ResolveTravelOutcome / travel_outcome::ShouldResetClientOnGrant。
	const uint32_t targetZoneId = current->targetZoneId;
	const bool crossZone = targetZoneId != GetZoneId();
	if (travel_outcome::ShouldResetClientOnGrant(crossZone, evidence))
	{
		const char *skipReason = nullptr;
		if (tlsEcs.actorRegistry.any_of<UnregisterPlayer>(entity))
		{
			skipReason = "player is exiting (UnregisterPlayer); the exit flow owns the session";
		}
		else if (!locationParsed)
		{
			skipReason = "location missing or unparsable";
		}
		else if (!travel_outcome::IsAwaitingPlacementOfHandoff(location.node_id().empty(), location.zone_id(),
															   location.owner_epoch(), targetZoneId, redisEpoch))
		{
			skipReason = "location is not this handoff's awaiting placement (epoch advanced by another request)";
		}

		if (skipReason == nullptr)
		{
			LOG_WARN << "[ZoneTravel][ClientReset] player " << playerId << " (" << reasonText
					 << ", evidence=" << travel_outcome::EvidenceName(evidence) << "): owner_epoch "
					 << cachedEpoch << " -> " << redisEpoch << ", location awaits placement in zone "
					 << targetZoneId << "; sending tip " << static_cast<uint32_t>(kZoneTravelTargetBusy)
					 << " + KickPlayer so the client re-logs in";
			travel_handoff_stats::Inc(travel_handoff_stats::Get().grantedClientReset);
			SendTipAndKickToClient(entity, static_cast<uint32_t>(kZoneTravelTargetBusy));
		}
		else
		{
			LOG_WARN << "[ZoneTravel][ClientReset] not resetting client of player " << playerId << " ("
					 << reasonText << ", evidence=" << travel_outcome::EvidenceName(evidence)
					 << "): " << skipReason << "; target_zone=" << targetZoneId
					 << " owner_epoch=" << redisEpoch << "; destroying only";
		}
	}
	// current 指针到此为止不再使用:DestroyDeposedPlayer 会摘组件、销毁实体。
	DestroyDeposedPlayer(playerId, "travel_granted_without_reply");
}

void PlayerLifecycleSystem::HandleSceneChangeEnterSceneReply(entt::entity playerEntity, Guid playerId,
															 PlayerSceneChangeInFlightComp target,
															 const ::scene_manager::EnterSceneResponse &resp)
{
	if (!target.playerRequested)
	{
		// 服务器替他发的队伍跟随:玩家没在等结果。被拒(含理论上不该出现的 18:跟随只发本节点上的
		// 场景)只记日志、队员留在原场景,不起交接、不回 tip —— 跟随不跨节点拉人(team-system.md DV-6)。
		if (resp.error_code() != 0)
		{
			LOG_INFO << "[ZoneTravel] team-follow EnterScene for player " << playerId
					 << " was rejected by scene_manager code=" << resp.error_code()
					 << " scene_id=" << target.sceneId << " corr=" << resp.correlation_id()
					 << "; player stays in the current scene";
		}
		return;
	}

	if (resp.error_code() == kSmErrHandoffPending)
	{
		// 目标场景在别的节点。18 的语义是"请先存盘并出示标记":按同一个目标起同 zone 交接,
		// 落盘、写标记之后由 RequestTravelEnterScene 重发。scene_manager 被拒时未改任何状态。
		LOG_INFO << "[ZoneTravel] EnterScene for player " << playerId
				 << " needs a cross-node handoff (scene_manager code=" << resp.error_code()
				 << "); starting same-zone handoff scene_id=" << target.sceneId
				 << " scene_conf_id=" << target.sceneConfigId << " corr=" << resp.correlation_id();
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
}

void PlayerLifecycleSystem::HandleTravelEnterSceneReply(entt::entity playerEntity, Guid playerId,
														const ::scene_manager::EnterSceneResponse &resp, bool correlated)
{
	const auto *travel = tlsEcs.actorRegistry.try_get<PlayerTravelHandoffComp>(playerEntity);
	if (travel == nullptr || travel->requestedAtMs == 0)
	{
		// 防御性检查,不可达:Dispatch 只在交接在途、且交接的 EnterScene 已发(号已记,非 0)时才分到这里,
		// 而号只在 BeginTravelHandoff 的 SET 回调里(requestedAtMs 置位之后)才写。走到这里说明不变量被破坏:
		// 不拿这条应答当证据,不动交接,交给看门狗收敛。
		LOG_ERROR << "[ZoneTravel] invariant broken: travel reply routed without an issued handoff EnterScene, player="
				  << playerId << " has_travel=" << (travel != nullptr) << " corr=" << resp.correlation_id()
				  << " correlated=" << correlated << " error_code=" << resp.error_code() << "; ignoring";
		return;
	}
	const uint64_t requestedAtMs = travel->requestedAtMs;
	const bool sameZone = (travel->targetZoneId == GetZoneId());

	if (resp.error_code() != 0)
	{
		// owner_epoch_after_rollback 只进日志,**绝不**参与判定(GO-2 §12.8 候选判据 d):应答可能丢;回滚会同时把
		// 交接标记转写到新 epoch,第三方可凭它铸出更新的值,而号码匹配的这条应答照样带着旧回显。去留只由
		// ResolveTravelOutcome 的原子取证裁决 —— 它先删掉本族标记,再认 Redis 里的回滚回执。
		LOG_WARN << "[ZoneTravel] EnterScene rejected for player " << playerId
				 << " code=" << resp.error_code() << " msg=" << resp.error_message()
				 << " owner_epoch_after_rollback=" << resp.owner_epoch_after_rollback()
				 << " corr=" << resp.correlation_id() << " correlated=" << correlated;
		ResolveTravelOutcome(playerId, requestedAtMs, "scene_manager rejected", travel_outcome::Evidence::kFailed);
		return;
	}
	if (!resp.has_redirect())
	{
		if (sameZone)
		{
			// 同 zone 放行的正常形态:成功、没有票据。两种可能应答本身分不出来 ——
			//   a) 目标在别的节点:scene_manager 已铸造新 epoch、路由已发,本节点不再持有该玩家;
			//   b) 重发时 scene_manager 重新挑频道挑回了本节点:同物理节点不铸造,场景就地切换。
			// 交给 ResolveTravelOutcome 按 epoch 判。b) 也必须过它的原子取证删除(理由见头文件)。
			LOG_INFO << "[ZoneTravel] same-zone handoff reply for player " << playerId
					 << " corr=" << resp.correlation_id() << " correlated=" << correlated << "; verifying outcome";
			ResolveTravelOutcome(playerId, requestedAtMs, "same-zone placement", travel_outcome::Evidence::kSucceeded);
			return;
		}
		// 跨 zone 放行了却没有票据:协议异常。是否已推进 epoch 由 ResolveTravelOutcome 查清楚;
		// 证据 kAnomalous:已被放行时只销毁、不踢线(这条应答说明不了客户端没拿到结果)。
		LOG_ERROR << "[ZoneTravel] EnterScene reply for player " << playerId
				  << " has neither error nor redirect; verifying outcome corr=" << resp.correlation_id()
				  << " correlated=" << correlated;
		ResolveTravelOutcome(playerId, requestedAtMs, "reply without redirect", travel_outcome::Evidence::kAnomalous);
		return;
	}

	// 跨 zone 放行:scene_manager 已 INCR owner_epoch 并把 location 指向目标 zone,客户端会经
	// Kafka RedirectToGateEvent → gate msg 124 → RedirectFlow 连到目标 zone。
	// 本节点从这一刻起不再持有该玩家,且手里的 epoch 已旧 —— 不能再存盘,只能销毁。
	// 这条分支不经核实,所以只接受本代交接的应答(Dispatch 按号分发;旧版 SM 下退回按 player_id,同今天)。
	// GO-2 的回滚只发生在推重定向 / 路由失败的路径上(那时回的是 ErrKafkaRoute),成功且带 Redirect 的应答说明
	// 第一条腿确已放行(判定表 B10)。
	LOG_INFO << "[ZoneTravel] handoff granted for player " << playerId
			 << " -> gate " << resp.redirect().target_gate_ip() << ":" << resp.redirect().target_gate_port()
			 << "; destroying source-side entity"
			 << " corr=" << resp.correlation_id() << " correlated=" << correlated;
	ObserveTravelHandoffEnded(playerEntity, travel_handoff_stats::Get().granted);
	DestroyDeposedPlayer(playerId, "travel_redirect", /*routine=*/true);
}

void PlayerLifecycleSystem::WithdrawHandoffMark(Guid playerId, uint64_t markEpoch, uint64_t requestedAtMs,
												   const char *site)
{
	// 先登记、后发命令:哪怕命令当场发不出去,IsSceneChangeBusy 这道闸也已经立起来了。
	const auto now = handoff_mark_withdraw::Clock::now();
	std::optional<handoff_mark_withdraw::Entry> evicted;
	tlsHandoffWithdrawQueue.Add(playerId, player_ownership::HandoffRedisValue(markEpoch, requestedAtMs), now,
								std::chrono::seconds(player_ownership::kHandoffMarkTtlSec) +
									handoff_mark_withdraw::kDeadlineMargin,
								evicted);
	if (evicted.has_value())
	{
		// 表满只可能出现在 Redis 长时间不通、且期间有 kMaxPending 个交接作废。被淘汰的那一条此后只剩
		// TTL 兜底(对该玩家是 fail-open),与放弃同等对待:记 ERROR、计入 withdraw_expired。
		// 日志带上被淘汰的玩家与标记原文,事后才查得出是谁处在回档窗口里;player_id 只进日志、不做指标 label。
		LOG_ERROR << "[ZoneTravel][WithdrawMark] pending table full (" << handoff_mark_withdraw::kMaxPending
				  << "); evicted player=" << evicted->playerId << " mark=" << evicted->markValue
				  << " (entry closest to its deadline), that mark now only expires by TTL";
		travel_handoff_stats::Inc(travel_handoff_stats::Get().withdrawExpired);
	}
	// 新条目登记时 nextAttemptAt = now,这里立刻把它(连同其它到期条目)发出去。
	FlushDueHandoffWithdrawals(/*force=*/false, site);
}

void PlayerLifecycleSystem::RetryPendingHandoffWithdrawals(bool reconnected)
{
	FlushDueHandoffWithdrawals(/*force=*/reconnected, reconnected ? "reconnect" : "retry");
}

void PlayerLifecycleSystem::BeginInheritedMarkClear(Guid playerId)
{
	auto pendingIt = tlsPendingEnterMap.find(playerId);
	if (pendingIt == tlsPendingEnterMap.end())
	{
		return;
	}
	EnsureExitStatsTimer();
	PlayerEnterContext &ctx = pendingIt->second;
	InheritClearState &state = ctx.inheritClear;
	if (state.lifecycle == 0)
	{
		state.lifecycle = ++tlsInheritClearLifecycleSeq;
	}
	// ctx.ownerEpoch == 0(旧版 gate / scene_manager 的路由,兼容窗口)同样要清,只是换成不核对归属、删 ≤ 当前
	// owner_epoch 的那一段(SendInheritClear 按 targetEpoch == 0 选脚本)。跳过的话上一任在本节点写下的 A1′
	// 标记会在新持有期间一直有效,下一次跨节点落点凭它免存盘过门(复审 ownership-major)。
	if (state.targetEpoch == ctx.ownerEpoch && state.phase != exit_release_mark::InheritClearPhase::kNone)
	{
		return; // 本生命周期已针对这个 epoch 发过(在途 / 等重发 / 已确认),重连覆盖上下文时不重复发
	}
	// 新的 epoch(首次,或同一次载入期间重连带来了不同的路由):重新开始计次与截止时刻。旧 epoch 那次若还在途,
	// 它的应答只记账(rewriteEpoch),不参与闸门判定。
	state.targetEpoch = ctx.ownerEpoch;
	state.phase = exit_release_mark::InheritClearPhase::kNone;
	state.attemptsSent = 0;
	state.firstSentAt = {};
	state.nextRetryAt = {};
	if (ctx.ownerEpoch == 0)
	{
		LOG_INFO << "[ExitRelease][InheritClear] route for player " << playerId
				 << " carries no owner_epoch; clearing marks up to the current owner_epoch instead";
	}
	SendInheritClear(playerId);
}

void PlayerLifecycleSystem::RetryInheritedMarkClears(bool reconnected)
{
	if (tlsPendingEnterMap.empty())
	{
		return;
	}
	// 先收集、后处理:拒绝会擦待入场条目,重发失败也可能拒绝,遍历中不能改表。
	const auto now = exit_release_mark::Clock::now();
	std::vector<Guid> toRefuse;
	std::vector<Guid> toResend;
	for (const auto &[playerId, ctx] : tlsPendingEnterMap)
	{
		const InheritClearState &state = ctx.inheritClear;
		const bool waiting = state.phase == exit_release_mark::InheritClearPhase::kRetryWait;
		const bool inFlight = state.phase == exit_release_mark::InheritClearPhase::kInFlight;
		if (!waiting && !inFlight)
		{
			continue;
		}
		// 截止时刻对在途同样生效:黑洞连接上的 EVAL 不会回空应答,不能让玩家无限停在载入界面。
		if (now - state.firstSentAt >= exit_release_mark::kInheritClearDeadline)
		{
			toRefuse.push_back(playerId);
			continue;
		}
		if (waiting && (reconnected || now >= state.nextRetryAt))
		{
			toResend.push_back(playerId);
		}
	}
	for (const Guid playerId : toRefuse)
	{
		RefuseInheritedEntry(playerId, "inherited-mark clear deadline passed");
	}
	for (const Guid playerId : toResend)
	{
		SendInheritClear(playerId);
	}
}

std::size_t PlayerLifecycleSystem::ExitReleaseMarksInFlight()
{
	return tlsExitReleaseMarksInFlight;
}

void PlayerLifecycleSystem::SetNodeIdentityProbe(std::function<bool()> probe)
{
	tlsNodeIdentityProbe = std::move(probe);
}

void PlayerLifecycleSystem::EnforceTravelFreezeCaps(travel_freeze_cap::Clock::time_point now)
{
	// 先收集、后处置:处置会摘组件 / 销毁实体,遍历 view 时改 registry 是未定义行为。
	// 不排除退出中的实体:骨架没有给退出开例外。原先想排除它的理由(退出链 M4 可能正在等一笔更早的未落地存盘)
	// 由 ConcludeHandoffAfterMarkSent 的"有未落地存盘就推迟销毁"覆盖。
	// 只读写已有组件的字段(ElapsedSinceFreeze 可能补记打点),不 emplace、不 get_or_emplace。
	std::vector<Guid> due;
	auto view = tlsEcs.actorRegistry.view<PlayerTravelHandoffComp>();
	for (const auto entity : view)
	{
		const auto *guid = tlsEcs.actorRegistry.try_get<Guid>(entity);
		if (guid == nullptr)
		{
			// 交接组件只由 StartTravelHandoff 挂,那里要求实体有 Guid;没有 Guid 的实体既回查不到也处置不了。
			continue;
		}
		auto &travel = view.get<PlayerTravelHandoffComp>(entity);
		if (ElapsedSinceFreeze(travel, *guid, now) < travel_freeze_cap::kFreezeCap)
		{
			continue;
		}
		due.push_back(*guid);
	}
	for (const Guid playerId : due)
	{
		ExpireTravelFreeze(playerId, now);
	}
}

void PlayerLifecycleSystem::ExpireTravelFreeze(Guid playerId, travel_freeze_cap::Clock::time_point now)
{
	// 按 id 重查、重算、重判:收集之后前一个玩家的处置可能已经改了 registry,不信任收集阶段的结论。
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
	const auto frozenFor = ElapsedSinceFreeze(*travel, playerId, now);
	const auto action =
		travel_freeze_cap::DecideFreezeCap(frozenFor, travel_freeze_cap::IsHandoffMarkSent(travel->requestedAtMs));
	switch (action)
	{
	case travel_freeze_cap::FreezeCapAction::kNone:
		return;
	case travel_freeze_cap::FreezeCapAction::kUnfreeze:
		// 标记从没发出:不可能已被放行(I0),解冻 + 失败 tip。30s 存盘看门狗正常早就处理了,这一支应恒不出现;
		// 出现说明存盘看门狗没能按时触发(墙钟回拨推迟了 muduo 定时器等)。
		travel_handoff_stats::Inc(travel_handoff_stats::Get().freezeCapReached);
		LOG_WARN << "[ZoneTravel][FreezeCap] player=" << playerId << " action="
				 << travel_freeze_cap::FreezeCapActionName(action) << " target_zone=" << travel->targetZoneId
				 << " frozen_ms=" << std::chrono::duration_cast<std::chrono::milliseconds>(frozenFor).count()
				 << ": handoff mark was never sent; unfreezing with a failure tip";
		// travel 指针到此为止不再使用:Abort 会摘组件。
		AbortTravelHandoff(playerId, "handoff freeze cap reached before the handoff mark was sent");
		return;
	case travel_freeze_cap::FreezeCapAction::kDestroy:
		// 标记已发出:归属在本节点核实不了,永不解冻。推迟销毁(有未落地存盘)时不计数,下一拍再判。
		if (ConcludeHandoffAfterMarkSent(playerId, travel_freeze_cap::MarkSentSite::kFreezeCap, now))
		{
			travel_handoff_stats::Inc(travel_handoff_stats::Get().freezeCapReached);
		}
		return;
	case travel_freeze_cap::FreezeCapAction::kCount:
		break;
	}
	LOG_ERROR << "[ZoneTravel][FreezeCap] player=" << playerId << " unexpected freeze cap action "
			  << static_cast<uint32_t>(action) << "; leaving the handoff to the next sweep";
}

bool PlayerLifecycleSystem::ConcludeHandoffAfterMarkSent(Guid playerId, travel_freeze_cap::MarkSentSite site,
														 travel_freeze_cap::Clock::time_point now)
{
	const auto playerEntity = tlsEcs.GetPlayer(playerId);
	if (!tlsEcs.actorRegistry.valid(playerEntity))
	{
		return true; // 已被别的路径收尾(退出 / 放行 / 废黜),幂等
	}
	auto *travel = tlsEcs.actorRegistry.try_get<PlayerTravelHandoffComp>(playerEntity);
	if (travel == nullptr)
	{
		return true;
	}
	const char *siteName = travel_freeze_cap::MarkSentSiteName(site);

	// 前置条件被破坏(调用方以为 SET 已发出,其实没有):SET 未发出一侧解冻是安全的(I0),不能反过来销毁一个
	// 本可以原地恢复的玩家。
	if (!travel_freeze_cap::IsHandoffMarkSent(travel->requestedAtMs))
	{
		LOG_ERROR << "[ZoneTravel][MarkSentDestroy] player=" << playerId << " site=" << siteName
				  << ": called although the handoff mark was never sent (requested_at_ms=0); unfreezing instead";
		AbortTravelHandoff(playerId, "mark-sent conclusion reached without a sent handoff mark");
		return true;
	}

	// 有未落地的存盘:此刻不存盘销毁,那笔在途 / 排队的写随后落地会把盘改回中间态,成为永久状态(与退出链 M4
	// 同一个顾虑)。数据一致性优先于活性:推迟,返回 false,由下一拍上限扫描重判。StartTravelHandoff 的快路径补写
	// 落地之后这里不可达(SET 发出后本节点不再产生新的存盘,发出前已确认没有未落地的存盘),它是冻结硬上限唯一允许的
	// 例外 —— redis_client 在 Redis 不可用时无限重试,推迟可能没有上界,所以每次交接只告警一次。
	if (HasUnsettledPlayerSave(playerId))
	{
		if (!travel->destroyDeferralReported)
		{
			travel->destroyDeferralReported = true;
			travel_handoff_stats::Inc(travel_handoff_stats::Get().destroyDeferredUnsettledSave);
			LOG_ERROR << "[ZoneTravel][MarkSentDestroy] player=" << playerId << " site=" << siteName
					  << ": unsettled save still pending; deferring destroy until it lands"
					  << " (metric=destroy_deferred_unsettled_save)";
		}
		return false;
	}

	// 先抄后毁:DestroyDeposedPlayer 会摘组件、销毁实体。
	const uint32_t targetZoneId = travel->targetZoneId;
	const bool crossZone = targetZoneId != GetZoneId();
	const std::string markValue = player_ownership::HandoffRedisValue(travel->markEpoch, travel->requestedAtMs);
	const uint64_t enterSceneCorrelationId = travel->enterSceneCorrelationId;
	const char *evidenceName =
		travel->hasRecordedEvidence
			? travel_outcome::EvidenceName(static_cast<travel_outcome::Evidence>(travel->recordedEvidence))
			: "none";
	const auto frozenMs =
		std::chrono::duration_cast<std::chrono::milliseconds>(ElapsedSinceFreeze(*travel, playerId, now)).count();
	const bool exiting = tlsEcs.actorRegistry.any_of<UnregisterPlayer>(playerEntity);
	// 退出中的实体客户端已断开,会话由退出流程收尾;会话已被取代 / 解绑时发 34 可能踢掉别人。
	const bool kick = !exiting && HasLiveGateSession(playerEntity, playerId);

	// 上限这一支是"两道看门狗与晚发闸都没能收敛"的信号、回执异常这一支应恒不出现(键被淘汰 / 新旧 SM 混跑 / 数据写坏),
	// 两者打 ERROR;其余三支是已知的窄窗口,打 WARN。
	// "(ownership UNKNOWN, not confirmed moved)" 是给 runbook 判读的:随后 DestroyDeposedPlayer 那行沿用下来的
	// "(ownership moved away)" 在这里不成立。字段顺序与原文是 runbook 的 grep 依据,改动要同步 runbook。
	const std::string detail =
		std::string("[ZoneTravel][MarkSentDestroy] player=") + std::to_string(playerId) + " site=" + siteName +
		" target_zone=" + std::to_string(targetZoneId) + (crossZone ? "(cross-zone)" : "(same-zone)") +
		" mark=" + markValue + " enter_scene_sent=" + (enterSceneCorrelationId != 0 ? "1" : "0") +
		" corr=" + std::to_string(enterSceneCorrelationId) + " evidence=" + evidenceName +
		" frozen_ms=" + std::to_string(frozenMs) + " exiting=" + (exiting ? "1" : "0") + " kick=" + (kick ? "1" : "0") +
		": handoff mark already sent and ownership cannot be verified here; destroying without persisting"
		" (ownership UNKNOWN, not confirmed moved)";
	if (site == travel_freeze_cap::MarkSentSite::kFreezeCap || site == travel_freeze_cap::MarkSentSite::kReceiptAnomaly)
	{
		LOG_ERROR << detail;
	}
	else
	{
		LOG_WARN << detail;
	}

	// 必须先踢再销毁:DestroyDeposedPlayer 会摘会话(RemovePlayerSession),之后 tip 与 34 都发不出去。
	// 34 的 reason.id 就是这个 tip 码,客户端用 DescribeKickReason 显示原因后回选服重登。
	if (kick)
	{
		travel_handoff_stats::Inc(travel_handoff_stats::Get().markSentClientReset);
		SendTipAndKickToClient(playerEntity, crossZone ? static_cast<uint32_t>(kZoneTravelTargetBusy)
													   : static_cast<uint32_t>(kEnterSceneFailed));
	}
	// 终态计数排在摘 PlayerFrozenComp 之前(要读冻结起点)。travel 指针到此为止不再使用。
	ObserveTravelHandoffEnded(playerEntity, travel_handoff_stats::Get().markSentDestroyed);
	// 不发任何 Redis 命令、不撤回标记:理由见头文件。销毁之后标记起断线释放标记(A1′)的作用。
	DestroyDeposedPlayer(playerId, siteName);
	return true;
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
	// 先抄后摘:失败 tip 按"这是哪一种交接"选、标记原文靠 markEpoch + requestedAtMs 还原,
	// 摘掉组件就无从知道了。
	const bool crossZone = (travel->targetZoneId != GetZoneId());
	const uint64_t handoffMarkEpoch = travel->markEpoch;
	const uint64_t handoffRequestedAtMs = travel->requestedAtMs;
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

	// 终态计数:回失败 tip 的是"未成",静默解冻的是"就地解决"。排在摘 Frozen 之前(要读冻结起点)。
	ObserveTravelHandoffEnded(playerEntity, notifyFailure ? travel_handoff_stats::Get().aborted
														  : travel_handoff_stats::Get().resolvedInPlace);

	// 解冻:StartTravelHandoff 用 PlayerFrozenComp 冻结输入,交接没发生就还给玩家。
	tlsEcs.actorRegistry.remove<PlayerFrozenComp>(playerEntity);

	// 撤回 handoff 标记:标记的语义是"这一刻的状态已落盘、可以交接",玩家解冻后会继续产生新状态,
	// 留着它会让之后某次跨节点 EnterScene 误以为盘上是最新的(回档)。与 scene_manager 只比对不删的
	// 契约不冲突:这是源端撤回自己写的标记。撤回没确认之前 IsSceneChangeBusy 对该玩家返回 true ——
	// 解冻照常,只是暂时不替他发 EnterScene,不再是"删不掉就靠 TTL 兜底"。
	// markEpoch == 0 = 盘上没有这份标记:写标记的 SET 从没发出去过(存盘看门狗 / epoch 未知 / Redis 未连接 / 命令没
	// 发出去)、SET 收到 ERROR 应答(HandleTravelMarkWriteRejected),或 ResolveTravelOutcome 的取证脚本已在同一原子
	// 步骤里删掉了本族标记(判定表 B4 / B5)。不登记,免得一个根本不存在的标记把玩家挡在换图之外。
	if (handoffMarkEpoch != 0 && handoffRequestedAtMs != 0)
	{
		WithdrawHandoffMark(playerId, handoffMarkEpoch, handoffRequestedAtMs, "abort");
	}

	if (!notifyFailure)
	{
		return;
	}
	// 让客户端收起"传送中 / 切换中"遮罩。跨 zone 传送未成一律归"目标区繁忙"(含目标区不存在、
	// 无可用 gate、scene_manager 拒绝、应答超时):这些在 scene 侧分不清,也都是"稍后再试"。
	// 同 zone 换图未成沿用通用的进场失败码。
	// 退出中的实体只可能经存盘看门狗或冻结上限的"标记未发出"分支走到这里(其余情形由退出流程先作废交接并销毁
	// 实体):tip 发往已断开的会话,无害。
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
