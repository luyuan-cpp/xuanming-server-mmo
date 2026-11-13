#include "player_battle.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "battle_lock_reply_shape.h"
#include "battle_lock_rules.h"
#include "engine/core/async/coroutine_callback.h"
#include "core/system/scene_task_scope.h"
#include <boost/cobalt/task.hpp>
// 本人近期战斗结果的列表规则(docs/design/battle-result-recovery.md §4.2)。
#include "battle_result_history.h"
// FIGHTING 冻结到期之后续冻还是解冻(纯判定)与 match 的 SQL 终局只读查询(turn-battle-gap-closure.md §11)。
#include "battle_freeze_hold_rules.h"
#include "battle_terminal_query.h"

#include <muduo/base/Logging.h>
#include <muduo/net/EventLoop.h>
#include <muduo/net/TimerId.h>
#include <muduo/contrib/hiredis/Hiredis.h>
#include <hiredis/hiredis.h>

#include <algorithm>
#include <array>
#include <atomic>
#include <chrono>
#include <exception>
#include <functional>
#include <limits>
#include <map>
#include <set>
#include <google/protobuf/message.h>
#include <string>
#include <utility>
#include <vector>

#include "engine/core/time/system/time.h"
#include "network/network_utils.h"
#include "network/node_utils.h"
#include "network/client_message_sender.h"
#include "thread_context/ecs_context.h"
#include "thread_context/node_context_manager.h"
#include "infra/storage/redis_cluster/redis_cluster_client.h"

#include "actor/attribute/constants/actor_state_attribute_calculator_constants.h"
#include "actor/attribute/system/actor_attribute_calculator.h"
#include "combat/buff/comp/buff_comp.h"
#include "combat/buff/constants/buff.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/item_hold.h" // 战斗道具预留(docs/design/battle-item-reservation.md)
#include "player/system/player_mail_outbox.h" // 两包都放不下时掉落改投邮件(docs/design/battle-overflow-mail.md)
#include "modules/bag/comp/player_bags_comp.h"
#include "table/proto/tip/bag_error_tip.pb.h" // kBagItemNotStacked:两包都放不下的 DEFER 原因(收件箱定向唤醒)
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/gain_block/gain_block_service.h"
#include "modules/currency/system/currency_system.h"
// tlsGuidSegmentRegistry:掉落入包前预检 item 号段余量(审计 cpp-economy#0)。
#include "modules/id_segment/guid_segment_registry.h"
#include "modules/mission/comp/mission_comp.h"
#include "player/comp/player_frozen_comp.h"
// PlayerTravelHandoffComp:交接一挂上 SavePlayer 就硬性跳过写盘,此刻应用等于落不了盘。
#include "player/comp/player_ownership_comp.h"
#include "player/system/player_lifecycle.h"  // IsPlayerFrozen / IsSceneChangeBusy
#include "player/system/role_admission_system.h"
#include "player/system/player_mission.h"
#include "player/system/player_pet.h"
#include "player/system/player_level_rules.h"  // CappedLevel:快照等级按生效等级
#include "modules/condition/condition_type.h"
#include "modules/mission/mission_events.h"
#include "player/system/player_revive.h"
#include "player/system/player_skill.h"
#include "player/system/player_team.h"
// PublishAssetWake:战斗冻结解除后唤醒资产通道(landing-recovery-sync-digest.md §5.2)。
#include "player/system/player_landing.h"
// OnBattleFreezeCleared:因战斗延后的收件箱处置立即重查(player-inbox-service.md §7.3 第 4 条,C1)。
#include "player/system/player_inbox_system.h"
#include "player/system/player_inbox_registry.h"
#include "proto/player_inbox/player_inbox.pb.h"  // PLAYER_INBOX_MSG_BATTLE_SETTLEMENT:按类型规格取载荷上限
// 冻结到期时判「这一局的结算是否已在本实体应用过」:收件箱战斗域的最近结局环(HasRecentAppliedBattleSettlement)。
#include "player/system/player_inbox_rules.h"
#include "proto/common/component/player_inbox_ledger_comp.pb.h"
// 近期战斗结果:确认已读前判「此刻能不能改这名玩家」的唯一判定;持久组件与客户端协议。
#include "player/system/player_mutability.h"
#include "proto/common/component/player_battle_result_comp.pb.h"
#include "proto/scene/player_battle_result.pb.h"

// 时间->回合换算与回合常量的权威定义在回合引擎库(scene 可以依赖 battle 常量,反向禁止)。
#include "services/battle/constants/turn_battle_constants.h"
// 战斗配表指纹(七张战斗表确定性序列化 sha256,随快照携带给 match/battle 比对)。
#include "services/battle/data/battle_table_fingerprint.h"

#include "table/code/buff_table.h"
#include "table/code/item_table.h"
#include "table/code/pet_table.h"
#include "table/code/attributepool_table.h"
#include "table/code/skill_table.h"
#include "table/proto/tip/common_error_tip.pb.h"

#include "proto/common/base/common.pb.h"
#include "proto/common/component/actor_attribute_state_comp.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"  // PlayerProfileComp(快照里的角色名)
#include "proto/common/component/player_login_comp.pb.h"
#include "proto/common/component/player_network_comp.pb.h"
#include "proto/common/component/player_pet_comp.pb.h"
#include "proto/common/component/player_skill_comp.pb.h"

// —— 以下生成产物当前尚未生成,proto 重生成后出现(路径按仓库生成器命名规律推断,
//    依据见任务返回 open_issues;battle 节点侧代码采用同一推断)——
#include "proto/battle/battle_data.pb.h"                              // proto/battle/battle_data.proto
#include "proto/common/component/battle_comp.pb.h"                    // InBattleComp / eInBattleState
#include "proto/common/event/battle_event.pb.h"                       // BattleConfirmedEvent
#include "proto/scene/scene.pb.h"                                     // PrepareBattle* / CancelBattlePrepare*(重生成后新增消息)
#include "proto/battle/player_battle.pb.h"                            // BattleEndS2C / BattleReconnectS2C
#include "rpc/service_metadata/player_battle_service_metadata.h"      // BattleClientPlayerNotify*MessageId
#include "spatial/system/movement.h"                                  // HaltMotion:进入战斗冻结即停步

// Windows SDK 的同名宏会改写 protobuf 反射方法名，此处隔离宏污染。
#ifdef GetMessage
#undef GetMessage
#endif

namespace
{
	// Redis key 契约(设计文档 §6):
	//   battle:lock:{player_id} = battle_id           —— 战斗串行化咨询锁(match 读,只 EXISTS)
	//   battle:ctx:{player_id}  = InBattleComp 序列化 —— 锁的伴生上下文(scene 私有):
	//       battle_node_id(仅日志/排障)/ deadline_ms / state / prepare_deadline_ms,供"实体没了但锁还在"
	//       的路径重建冻结(完整下线再登录、备战到期后迟到的确认)。生命周期与锁完全同步:
	//       同一条 Lua 里写入 / 续期 / 删除,同 TTL,只在锁存在且值匹配时才被信任。
	// 结算原件不进 Redis:结算只走 match SQL + player_inbox(player-data-tidb-scaling-plan.md §5.4)。
	constexpr const char *kBattleLockKeyFmt = battle_lock::kBattleLockKeyFmt;
	constexpr const char *kBattleCtxKeyFmt = battle_lock::kBattleCtxKeyFmt;
	// 写锁 + ctx(PrepareBattle 首发与重试共用)与只延长的条件续期,脚本与理由见 battle_lock_rules.h。
	constexpr const char *kWriteBattleLockScript = battle_lock::kWriteBattleLockScript;
	constexpr const char *kRenewBattleLockIfMatchScript = battle_lock::kRenewBattleLockIfMatchScript;

	// speed 缺配兜底:BaseAttributesComp.speed 为 0(存量数据未配置)时的默认出手速度。
	// 高于引擎的怪物默认速度(kMonsterDefaultSpeed=60),保证缺配玩家不至于永远后手。
	// 2026-09-14 速度单位 ×12(原 10),与引擎常量一起放大
	constexpr uint64_t kFallbackBattleSpeed = 120;

	// reaper 定时器(单定时器形态:整个进程只有一个,StartReaper / StopReaper 成对)
	muduo::net::TimerId gReaperTimerId;
	bool gReaperActive = false;
	muduo::net::EventLoop* gReaperLoop = nullptr;

	// 条件删锁命令的测试注入(见 PlayerBattleSystem::SetBattleLockReleaseSenderForTest);生产恒为空,走 tlsPlayerRedis。
	PlayerBattleSystem::BattleLockReleaseSender gBattleLockReleaseSender = nullptr;
	// 组队跟随唤醒(见 PlayerBattleSystem::SetFollowWakeFnForTest)。本文件里所有「补一次跟随检查」都经它。
	PlayerBattleSystem::FollowWakeFn gFollowWakeFn = &PlayerTeamSystem::OnBattleFreezeCleared;
	// 锁脚本观察点(见 PlayerBattleSystem::SetLockScriptObserverForTest)。生产为 nullptr。
	PlayerBattleSystem::LockScriptObserver gLockScriptObserver = nullptr;
	// 锁写入 / 续期重试的调度器注入点(见 PlayerBattleSystem::SetLockRetrySchedulerForTest)。
	// 生产为 nullptr = 当前线程的 EventLoop::runAfter(ScheduleLockWriteRetry)。
	PlayerBattleSystem::LockRetrySchedulerFn gLockRetryScheduler = nullptr;
	PlayerBattleSystem::LockScriptSender gLockScriptSender = nullptr;

	// FIGHTING 冻结到期时的 SQL 终局查询(见 PlayerBattleSystem::SetBattleTerminalQueryForTest)。
	// 生产 = MatchInternal.GetBattleTerminalDecision 只读查询(battle_terminal_query.h)。
	bool SendBattleTerminalQuery(const uint64_t battleId, std::function<void(battle_freeze_hold::TerminalAnswer)> done)
	{
		return battle_terminal_query::Send(battleId, PlayerBattleSystem::kTerminalQueryDeadlineMs, std::move(done));
	}
	PlayerBattleSystem::BattleTerminalQueryFn gBattleTerminalQuery = &SendBattleTerminalQuery;

	// 终局查询「在途多久算丢失」用的单调钟(毫秒,原点任意,只取差值)。墙钟只给跨进程的截止时刻(deadline、续冻上限)用;
	// 在途时长是进程内耗时(AGENTS §11.3),墙钟回拨会让在途查询永远不判丢失,前跳会把正常在途的查询误判丢失。
	// 生产实现与锁写入重试预算共用同一个 SteadyNowMs(定义在下面,steady_clock)。
	uint64_t SteadyNowMs();
	PlayerBattleSystem::TerminalQueryClockFn gTerminalQueryClock = &SteadyNowMs;

	// SQL 终局查询的在途登记,键 = (player_id, battle_id)。queryId 认出「这一次」查询(进程内递增,判丢失之后再发的
	// 新查询换一个号,迟到的旧回答摘不掉它的登记);sentSteadyMs 是发出时的单调钟读数,只用来判丢失。
	// 同一局同一时刻至多一条在途:reaper 节拍遇到在途的直接跳过;完成回调里先摘登记再处置。
	// 生产传输恰好回调一次(GrpcDone 契约)。在途超过 kTerminalQueryLostAfterMs 仍没回就当作丢失,摘掉、按「未知」判 ——
	// 丢一次回调不能把玩家永远冻在这一局(按「未知」判照样受续冻上限约束)。
	// 所有权与线程模型:thread_local,scene loop 线程独占;不持久化,只用于去重与防丢失,不参与终局判定本身。
	struct TerminalQueryInFlight
	{
		uint64_t queryId = 0;
		uint64_t sentSteadyMs = 0;
	};
	thread_local std::map<std::pair<uint64_t, uint64_t>, TerminalQueryInFlight> gTerminalQueriesInFlight;
	thread_local uint64_t gTerminalQuerySeq = 0;
	constexpr uint64_t kReaperIntervalMs = static_cast<uint64_t>(PlayerBattleSystem::kReaperIntervalSec * 1000.0);
	constexpr uint64_t kTerminalQueryLostAfterMs = kReaperIntervalMs + PlayerBattleSystem::kTerminalQueryDeadlineMs;
	static_assert(kTerminalQueryLostAfterMs > PlayerBattleSystem::kTerminalQueryDeadlineMs,
				  "判丢失必须晚于单次调用期限:期限之内传输一定会回调");
	// 任何路径(正常回答、查询丢失、没发出去)的解冻都不晚于越过续冻上限后的第一拍:越过上限的那一拍直接解冻,不查询、
	// 也不等在途的查询(ResolveExpiredFightingFreeze)。那一拍最晚在上限之后一个节拍;锁按 FreezeHoldDeadlineMs + kLockExtraTtlSec
	// 写,EX 秒级下取整最多早到 1 秒。一拍加这 1 秒必须短于锁余量,否则最后一次处置之前锁已经没了,match 可能先把玩家放进下一局。
	static_assert(kReaperIntervalMs + 1000ULL < static_cast<uint64_t>(PlayerBattleSystem::kLockExtraTtlSec) * 1000ULL,
				  "越过续冻上限后的第一拍必须早于锁过期");

	// 续冻事件的进程内计数(PlayerBattleSystem::FreezeHoldEventCount 读)。relaxed 原子:写只在 scene loop 线程,
	// 读者(单测、日后的 /metrics 采集回调)只要单调的累计值。告警仍走同名结构化日志。
	std::array<std::atomic<uint64_t>, static_cast<std::size_t>(battle_freeze_hold::FreezeHoldEvent::kCount)> gFreezeHoldEvents{};

	void CountFreezeHoldEvent(const battle_freeze_hold::FreezeHoldEvent event)
	{
		const auto index = static_cast<std::size_t>(event);
		if (index < gFreezeHoldEvents.size())
		{
			gFreezeHoldEvents[index].fetch_add(1, std::memory_order_relaxed);
		}
	}

	// 一次 SQL 终局查询走到哪一步(SendTrackedTerminalQuery 的结论)。底层类型按枚举项个数选;kCount 恒为最后一项。
	enum class TerminalQueryStep : uint8_t
	{
		kSent,     // 已发出:回答之后经 onAnswer 到达(loop 线程,恰好一次;注入实现可能在本次调用栈内同步回)
		kInFlight, // 同一 (player_id, battle_id) 的上一次查询还在途:这一拍什么都不做,等它的回答
		kLost,     // 上一次在途超过 kTerminalQueryLostAfterMs(单调钟):已摘登记、已计 kQueryLost,调用方按「未知」处置
		kNotSent,  // 没发出去(没有密钥 / 没有就绪的 match 节点 …):调用方按「未知」处置

		kCount
	};

	// 带在途登记的 SQL 终局查询,冻结到期(ResolveExpiredFightingFreeze)与道具预留复核(ReviewBattleItemHold)共用。
	// 同一 (player_id, battle_id) 同一时刻至多一条在途:两条路不会同时处置同一局(预留复核只看冻结已不在的局),
	// 万一交错(查询在途时冻结又按锁重建),回答只交给发出它的那一方,另一方下一拍自己再问。
	// onAnswer 只能捕获 id 与时刻,不捕获实体句柄(回答到达时实体可能已销毁 / 句柄被复用,AGENTS §11.7 同理)。
	// nowMs 是这一拍的墙钟,只进日志。
	TerminalQueryStep SendTrackedTerminalQuery(const uint64_t playerId, const uint64_t battleId, const uint64_t nowMs,
											   std::function<void(battle_freeze_hold::TerminalAnswer)> onAnswer)
	{
		const auto key = std::make_pair(playerId, battleId);
		// 在途多久按单调钟量(进程内耗时,AGENTS §11.3):墙钟回拨 / 跳变既不会让在途查询永远不判丢失,也不会误判丢失。
		const uint64_t steadyNowMs = gTerminalQueryClock();
		if (const auto found = gTerminalQueriesInFlight.find(key); found != gTerminalQueriesInFlight.end())
		{
			const uint64_t sentSteadyMs = found->second.sentSteadyMs;
			const uint64_t inFlightMs = steadyNowMs > sentSteadyMs ? steadyNowMs - sentSteadyMs : 0;
			if (inFlightMs <= kTerminalQueryLostAfterMs)
			{
				return TerminalQueryStep::kInFlight;
			}
			LOG_ERROR << "[PlayerBattle] metric=battle_terminal_query_lost player_id=" << playerId << " battle_id=" << battleId
					  << " in_flight_ms=" << inFlightMs << " now_ms=" << nowMs
					  << ",terminal query never completed;treated as unknown";
			CountFreezeHoldEvent(battle_freeze_hold::FreezeHoldEvent::kQueryLost);
			gTerminalQueriesInFlight.erase(found);
			return TerminalQueryStep::kLost;
		}
		const uint64_t queryId = ++gTerminalQuerySeq;
		gTerminalQueriesInFlight[key] = TerminalQueryInFlight{queryId, steadyNowMs};
		const bool sent = gBattleTerminalQuery(
			battleId, [playerId, battleId, queryId, onAnswer = std::move(onAnswer)](const battle_freeze_hold::TerminalAnswer answer) {
				// 只摘属于这一次查询的登记:判丢失之后再发的新查询换了查询号,迟到的旧回答不能把它摘掉。
				const auto found = gTerminalQueriesInFlight.find(std::make_pair(playerId, battleId));
				if (found != gTerminalQueriesInFlight.end() && found->second.queryId == queryId)
				{
					gTerminalQueriesInFlight.erase(found);
				}
				onAnswer(answer);
			});
		if (!sent)
		{
			const auto found = gTerminalQueriesInFlight.find(key);
			if (found != gTerminalQueriesInFlight.end() && found->second.queryId == queryId)
			{
				gTerminalQueriesInFlight.erase(found);
			}
			return TerminalQueryStep::kNotSent;
		}
		return TerminalQueryStep::kSent;
	}

	// 释放这一局的道具预留(docs/design/battle-item-reservation.md)。只在「这一局不会再从预留里扣」的时刻调用:
	// 结算已在本实体应用过、备战被 match 取消、SQL 终局是 ABORT、或这一局没有可裁决的 SQL 原件。
	// **续冻到上限解冻时不调**:那时结算仍可能晚到,预留要留到结算或作废,这正是它存在的理由。
	// 结算应用那一刻的释放不走这里(在 ApplySettlementToEntity 里与扣消耗同一帧,随结算那一次存档提交)。
	// 预留是存档数据:释放之后请一次关键进度存盘(1 秒内合并),不等定时存盘;丢了只是多占一会儿,下次加载再复核,方向安全。
	void ReleaseBattleItemHold(entt::entity player, const uint64_t playerId, const uint64_t battleId, const char* reason)
	{
		if (!BagService::ReleaseHold(player, battleId))
		{
			return;
		}
		LOG_INFO << "[PlayerBattle] metric=battle_item_hold_released player_id=" << playerId << " battle_id=" << battleId
				 << " reason=" << reason;
		PlayerLifecycleSystem::RequestKeyProgressSave(player, "battle_item_hold_released");
	}


	// 战斗锁与上下文都是玩家域键,走 tlsPlayerRedis。这里为真只说明至少一个节点连着,这名玩家所在的 slot 不一定可用,
	// 所以每个 command() 的返回值都要看:REDIS_ERR = 没发出去,回调不会来。
	bool RedisReady()
	{
		return tlsPlayerRedis.connected();
	}

	// 战斗锁写入 / 续期的三种操作名:进日志(op=)、传给锁脚本观察点。
	constexpr char kLockOpWrite[] = "write_lock";
	constexpr char kLockOpRenew[] = "renew_lock";
	constexpr char kLockOpRebuildRecheck[] = "rebuild_recheck";

	// 每次准备发送锁脚本时调一次观察点(算出 TTL 与 ctx 之后、判断 Redis 是否就绪之前)。
	void NotifyLockScriptObserver(const char* op, const uint64_t playerId, const uint64_t battleId, const uint64_t ttlSec,
								  const std::string& ctxPayload)
	{
		if (gLockScriptObserver != nullptr)
		{
			gLockScriptObserver(op, playerId, battleId, ttlSec, ctxPayload);
		}
	}

	// 战斗锁写入 / 续期的一次尝试(预算与退避见 battle_lock_rules.h 的 NextLockWriteRetryDelayMs)。
	struct LockWriteAttempt
	{
		uint32_t number = 1;      // 第几次发,从 1 起
		uint64_t firstSentMs = 0; // 首发时的单调时钟毫秒;0 = 还没发过,由发送函数填上
	};

	// 续期链的在途登记,键 = (player_id, battle_id)。
	//
	// 为什么只给续期(renew_lock)去重:放大源只有它 —— battle 侧对同一局的确认每 10 秒周期补发一次,ConfirmBattle 对
	// 已 FIGHTING 的同局确认都会顺带续期;Redis 中断时每名玩家每 10 秒就为同一件事叠一条重试链,日志与
	// metric=battle_lock_write_gave_up 计数随之虚高。在途链每次重发都按实体现状重算期限与 ctx(ConfirmBattleLockIfMatch
	// 的门禁),所以同键的首发直接让给它,不会因此用到旧值;链的最后一发若恰好已带着让路之前的值发出,下一条周期确认
	// 会再起一条链。
	// 其余两种不去重、不登记:
	//  - write_lock:每局只有 PrepareBattle 一次首发,没有放大源;
	//  - rebuild_recheck:每一次重建都必须有自己的首发 —— 只有首发回 0 才撤销重建,让给旧链会让新一次重建失去这层保护。
	//
	// 登记:续期**首发发出之前**(TryBeginRenewChain);同键已登记就让路,这一次什么都不发。
	// 摘除:链的每个终态恰好一次(EndRenewChain)——
	//  ① 拿到整数应答(1 或 0 都是结论);
	//  ② 重发前门禁放弃(实体在本节点,但这一局在它身上已不是 FIGHTING);
	//  ③ 放弃重试:次数 / 预算用尽,或当前线程没有 loop(ScheduleLockWriteRetry 返回 false);
	//  ④ 命令没发出去(或 Redis 未就绪)且排不上重试 —— 与 ③ 同一个出口。
	//  ⑤ Scene 作用域停止本地等待。
	// 排上重试不是终态,登记留给下一次发送。登记在首发之前、全程单线程,同一时刻同键至多一条链,按键摘不会摘掉别的链。
	// 所有权与线程模型:thread_local,scene loop 线程独占(发送函数、hiredis 回调、定时器都在 loop 线程上),
	// 不持久化、不跨线程。任务的 RAII 在终态或本地停止时摘登记;晚定时器和 Redis 回复不再恢复任务。
	// 只用于去重,不参与任何正确性判定;但漏摘会让同键之后的续期首发一直让路、续期再也发不出去,所以上面终态缺一不可。
	using RenewChainKey = std::pair<uint64_t, uint64_t>; // (player_id, battle_id)
	thread_local std::set<RenewChainKey> gRenewChainsInFlight;

	// 续期首发入口调用:同键已有在途续期链时让给它(返回 false,调用方什么都不发);否则登记并返回 true。
	bool TryBeginRenewChain(const uint64_t playerId, const uint64_t battleId)
	{
		if (!gRenewChainsInFlight.emplace(playerId, battleId).second)
		{
			LOG_DEBUG << "[PlayerBattle] Conditional renewal skipped: a renewal chain for the same battle is in flight, player_id="
					  << playerId << " battle_id=" << battleId
					  << "(the chain recomputes TTL and ctx from the entity on every resend)";
			return false;
		}
		return true;
	}

	// 续期链的终态(gRenewChainsInFlight 的 ①–④):每条链恰好调一次。
	void EndRenewChain(const uint64_t playerId, const uint64_t battleId)
	{
		gRenewChainsInFlight.erase(RenewChainKey(playerId, battleId));
	}

	uint64_t SteadyNowMs()
	{
		return static_cast<uint64_t>(std::chrono::duration_cast<std::chrono::milliseconds>(
										 std::chrono::steady_clock::now().time_since_epoch())
										 .count());
	}

	LockWriteAttempt StartLockWriteAttempt(LockWriteAttempt attempt)
	{
		if (attempt.firstSentMs == 0)
		{
			attempt.firstSentMs = SteadyNowMs();
		}
		return attempt;
	}

	// 没问到(空应答 / ERROR / Redis 未就绪 / 命令没发出去)之后按退避排下一次。
	// 返回 true = 已排上;false = 放弃重试(次数 / 预算用尽,或既没有注入调度器、当前线程也没有 loop),已打 ERROR,
	// 这条链到此为止 —— 登记了在途链的调用方(只有续期,gRenewChainsInFlight)据此摘登记,其余调用方忽略返回值即可。
	// resend 重跑整个「门禁 + 发送」:门禁在重发那一刻按 player_id 现取实体、按实体现状判。三个发送函数的闭包
	// 都只捕获 id 与字节,不捕获实体句柄或 this(实体可能已销毁、句柄可能被复用)。不保存 TimerId:节点停机时 loop 退出,
	// 没开火的重试随之丢弃 —— 锁带 TTL,不会留下永久状态;放弃时锁可能缺失或提前过期,结算不受影响(它只走收件箱)。
	// 定时器:生产为当前线程的 EventLoop::runAfter;单测经 PlayerBattleSystem::SetLockRetrySchedulerForTest 注入,
	// 注入时改调注入的函数,当前线程没有 loop 也照常排(单测线程没有 loop)。
	bool ScheduleLockWriteRetry(const char* op, const uint64_t playerId, const uint64_t battleId,
								const LockWriteAttempt attempt, std::function<void(LockWriteAttempt)> resend)
	{
		const uint64_t delayMs = battle_lock::NextLockWriteRetryDelayMs(
			attempt.number, SteadyNowMs() - attempt.firstSentMs, playerId ^ battleId);
		auto* loop = gLockRetryScheduler == nullptr ? muduo::net::EventLoop::getEventLoopOfCurrentThread() : nullptr;
		if (delayMs == 0 || (gLockRetryScheduler == nullptr && loop == nullptr))
		{
			LOG_ERROR << "[PlayerBattle] metric=battle_lock_write_gave_up op=" << op << " player_id=" << playerId
					  << " battle_id=" << battleId << " attempts=" << attempt.number
					  << ",battle lock write/renewal still unanswered;lock may be missing or expire early(match queue check and freeze restore are blind until it is rewritten;settlement via inbox is unaffected)";
			return false;
		}
		LOG_WARN << "[PlayerBattle] metric=battle_lock_write_retry op=" << op << " player_id=" << playerId
				 << " battle_id=" << battleId << " next_attempt=" << attempt.number + 1 << " delay_ms=" << delayMs;
		const LockWriteAttempt next{attempt.number + 1, attempt.firstSentMs};
		std::function<void()> fire = [resend = std::move(resend), next]() { resend(next); };
		if (gLockRetryScheduler != nullptr)
		{
			return gLockRetryScheduler(delayMs, std::move(fire));
		}
		else
		{
			loop->runAfter(static_cast<double>(delayMs) / 1000.0, std::move(fire));
		}
		return true;
	}

	// 备战时写进快照 routing 的大厅会话号(turn-based §22.5 第 3 条)。
	// match 的 NotifyBattleAssigned 与 battle 的 NotifyBattleStart 都按这份快照路由推送;
	// PREPARING 期间换了会话(断线重连 / REPLACE / 跨 gate 重定向),新会话收不到这两条,
	// ConfirmBattle 升级 FIGHTING 时比对它来补推一次重连提示。
	// 纯运行时组件:不入库,也**不能**并进 InBattleComp —— 后者经 SerializeBattleCtx 落 battle:ctx。
	// 生命周期:PrepareBattle 与 InBattleComp 同时挂上;ConfirmBattle 升级后即摘;
	// 其余情况随 RemoveInBattleComp 一并摘除。组件缺失而 InBattleComp 为 PREPARING,
	// 说明实体是登录重建出来的(RestoreBattleFreezeOnLogin),同样视为换过会话。
	struct BattlePrepareSessionComp
	{
		uint32_t gateSessionId = 0;
	};

	// 登录复查标记(2026-10-16):登录钩子读战斗锁重建冻结时「没问到」的玩家。
	// 没问到 = 空应答(连接断开 / 被释放:主切换掐连接、存活探测摘连接)、命令没发出去,或者当时 Redis 根本不可用。
	// 它不能当成「没有锁」:战斗在途却不重建冻结,战斗闸(资产通道、收件箱 requiresOutOfBattle)就放行了。
	// 标记之后由 reaper 节拍(RunLoginRechecks,30 秒)在 Redis 可用时重新读锁。重读之前先摘标记,这一轮又没问到就重新挂上,
	// 所以同一玩家同一时刻最多一条复查在途。
	// 纯运行时标记:不入库(实体重建时登录钩子本来就会重跑),随实体销毁。唯一写入口 MarkLoginRecheck,
	// 唯一消费口 RunLoginRechecks。不由 Redis 重连成功触发:负责人 2026-11-05 定「重连成功不做业务逻辑」(RedisSystem
	// 也不挂重连回调,infra-reconnect-overview.md R14),复查等一个 reaper 节拍足够。
	struct BattleLoginRecheckComp
	{
	};

	// reaper 按期限判作废(FIGHTING 到期)的最近一局(2026-10-19,第二、三轮评审 P3)。
	// 到期时 reaper 条件删锁;删锁没落地(Redis 抖)时锁还在,之后任何一条按锁重建的路径(登录挂的复查赶上冻结到期、
	// 迟到的确认事件)都会把玩家冻回这场已结束的战斗。
	// 按锁重建之前先比这里(battle_lock_rules.h 的 ClassifyLockRebuild):锁值等于它就不重建,补一次条件删锁。
	// 单槽够用:锁还在时 match 对锁 fail-closed,玩家排不进下一局;锁没了才可能有下一局,那时旧值已无锁可比。
	// 只记 FIGHTING 到期:PREPARING 到期刻意保留锁,等迟到的确认按锁重建 FIGHTING(reaper 的说明)。
	// 这一局的结算若之后才到,收件箱照常应用;对账日志 battle_inbox_shortfall_after_freeze_released 也读这里。
	// 纯运行时:不入库,随实体销毁。实体重建(重登、跨节点换场景 / 跨 zone 交接后在目的地新建)后槽位为空,登录重建对同一把锁的行为与改动前相同。
	// 唯一写入口 RememberExpiredFightingBattle;读口 ExpiredFightingBattleIdOf。
	struct BattleFreezeExpiredComp
	{
		uint64_t battleId = 0;
	};

	void RememberExpiredFightingBattle(entt::entity player, const uint64_t battleId)
	{
		if (!tlsEcs.actorRegistry.valid(player) || battleId == 0)
		{
			return;
		}
		// 此处延迟创建(按需状态的唯一写入口,docs/design/ecs-component-access-rules.md)。
		tlsEcs.actorRegistry.get_or_emplace<BattleFreezeExpiredComp>(player).battleId = battleId;
	}

	uint64_t ExpiredFightingBattleIdOf(entt::entity player)
	{
		if (!tlsEcs.actorRegistry.valid(player))
		{
			return 0;
		}
		const auto* expired = tlsEcs.actorRegistry.try_get<BattleFreezeExpiredComp>(player);
		return expired != nullptr ? expired->battleId : 0;
	}

	// 这一局的结算是否已在本实体应用过:收件箱战斗结算域的最近结局环里有 request_id(= battle_id)相同、结局 APPLIED 的条目
	// (player_inbox_rules::HasRecentAppliedBattleSettlement,收件箱状态机与效果同帧记入,随同一份快照提交)。
	// 冻结到期时据此判「本地已收尾」(turn-battle-gap-closure.md §11):环只有 32 条,但冻结期间玩家进不了别的局,
	// 这一局总在环里。账本组件缺失(加载不变量被破坏)按「没应用」处理,交给 SQL 终局查询决定。只读。
	//
	// 只认 APPLIED,REJECTED / SKIPPED 不算收尾(2026-11-06 评审核实,不是遗漏):战斗结算域里这两种都不是终局。
	//  - 收件箱 Ack 对这个域只收 APPLIED,报 REJECTED / SKIPPED 回 CONFLICT、行不变;VoidItem 对这个域一律拒绝
	//    (runbook §6.1「Battle 域条目卡住」);Match 只凭 APPLIED 收据放票。
	//  - scene 的执行器从不回 kRejected(坏载荷也只 DEFER 或按版本错位停用该域,ApplyInboxSettlement);条目带指令、
	//    或回放到非 APPLIED 的原结局,状态机直接停用该域(player_inbox_system.cpp「battle outcome requires original APPLIED decision」)。
	// 环里真出现这两种只能是旧版本或缺陷留下的异常,唯一出口是修好之后让 scene 真正把原件执行成功 —— 结算之后仍可能到达,
	// 所以照旧查终局、续冻,到上限告警交人工,不能当成「不会再有结算」提前解冻。
	bool IsBattleSettlementAppliedHere(entt::entity player, const uint64_t battleId)
	{
		if (!tlsEcs.actorRegistry.valid(player) || battleId == 0)
		{
			return false;
		}
		const auto* ledger = tlsEcs.actorRegistry.try_get<PlayerInboxLedgerComp>(player);
		return ledger != nullptr &&
			   player_inbox_rules::HasRecentAppliedBattleSettlement(
				   player_inbox_rules::FindCursor(*ledger,
												  static_cast<uint32_t>(player_inbox::PLAYER_INBOX_DOMAIN_SCENE_BATTLE_SETTLEMENT)),
				   battleId);
	}

	// 按锁重建两个入口(RestoreBattleFreezeOnLogin、RebuildBattleFreezeFromLock)共用的裁决接线:
	// 本实体的作废槽位 → ClassifyLockRebuild。单测经 PlayerBattleSystem::DecideLockRebuildFor 走同一个函数。
	battle_lock::LockRebuildDecision DecideLockRebuild(entt::entity player, const uint64_t lockBattleId)
	{
		return battle_lock::ClassifyLockRebuild(lockBattleId, ExpiredFightingBattleIdOf(player));
	}

	void MarkLoginRecheck(entt::entity player, const uint64_t playerId, const char* reason)
	{
		if (!tlsEcs.actorRegistry.valid(player))
		{
			return;
		}
		LOG_WARN << "[PlayerBattle] metric=battle_login_recheck_scheduled player_id=" << playerId << " reason=" << reason
				 << ",login battle lock read reached no conclusion;freeze restore deferred to the next reaper tick";
		// 此处延迟创建(按需状态的唯一写入口,docs/design/ecs-component-access-rules.md)。
		// 空标记类型取不出引用,不用 get_or_emplace。
		if (!tlsEcs.actorRegistry.any_of<BattleLoginRecheckComp>(player))
		{
			tlsEcs.actorRegistry.emplace<BattleLoginRecheckComp>(player);
		}
	}

	// 当前大厅会话号;无会话快照(已断线、实体仍在)时返回 0。
	uint32_t CurrentGateSessionId(entt::entity player)
	{
		const auto* session = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(player);
		return session != nullptr ? session->gate_session_id() : 0;
	}

	// 摘 InBattleComp 的唯一入口(team-system.md §F.2):确实摘掉组件时回调组队系统,
	// 补一次战斗中被跳过的跟随检查。所有解冻路径(结算确认、取消、备战作废、战斗作废、
	// 重建撤销)都必须走这里,漏一处就漏一次跟随。
	// 调用方若还要条件删锁,应先发删锁命令再调本函数:跟随链会读 battle:lock fail-closed,
	// 删锁与读锁是同一玩家的键,同 slot,路由层停等,跟随链读到的是删锁之后的状态。
	//
	// 同一处还发一次资产待办唤醒(landing-recovery-sync-digest.md §5.2):唤醒走控制域、删锁走玩家域,两条命令之间没有顺序,
	// 也不需要:消费端重投的资产指令到 scene 后只看 InBattleComp(此刻已摘),不读 battle:lock。资产通道在战斗中回 RETRY
	// (asset_op_system.cpp 的 kAssetInBattle 只看 InBattleComp 在不在,PREPARING 与 FIGHTING 都算),
	// 组件一摘就能应用,不必等最长 60 秒的退避。设计稿写的挂点是 ClearBattleFreeze 之后,但最常见的
	// 解冻 —— 结算确认(CompleteInboxSettlement)—— 并不经过 ClearBattleFreeze,挂在这个唯一入口上才不漏。
	// 唤醒只是提示:合并窗口、交接在途跳过、Redis 不可用都由 PublishAssetWake 自己处理,丢了退回原退避。
	bool RemoveInBattleComp(entt::entity player)
	{
		if (!tlsEcs.actorRegistry.valid(player))
		{
			return false;
		}
		// 备战会话记录只随 InBattleComp 存在:一并摘除(组件缺失时 remove 为空操作)
		tlsEcs.actorRegistry.remove<BattlePrepareSessionComp>(player);
		if (!tlsEcs.actorRegistry.any_of<InBattleComp>(player))
		{
			return false;
		}
		tlsEcs.actorRegistry.remove<InBattleComp>(player);
		gFollowWakeFn(player);
		PlayerLandingSystem::PublishAssetWake(player, PlayerLandingSystem::AssetWakeReason::kBattleFreezeCleared);
		PlayerInboxSystem::OnBattleFreezeCleared(player);
		return true;
	}

	// battle:lock 条件操作的 Lua 脚本:GET 与 DEL/EXPIRE/SET 在服务端原子执行,
	// 不存在"GET 看到旧值、DEL 删掉新值"的窗口。KEYS[1]=battle:lock,KEYS[2]=battle:ctx,
	// ARGV[1]=battle_id。伴生 ctx 永远跟锁同命:锁删 ctx 删,锁续 ctx 续。
	// 返回 1 = 命中并执行,0 = 锁不存在或值不匹配。
	constexpr char kDeleteLockIfMatchScript[] =
		"if redis.call('GET', KEYS[1]) == ARGV[1] then "
		"redis.call('DEL', KEYS[2]); return redis.call('DEL', KEYS[1]) else return 0 end";
	// 条件读 ctx:锁值 == battle_id 才返回 {ctx 或 nil, 锁剩余 TTL 秒};否则返回 nil。
	// 调用方据此区分"锁不在/已易主"(无重建资格)与"锁在但 ctx 缺失"(降级重建)。
	constexpr char kGetCtxIfLockMatchScript[] =
		"if redis.call('GET', KEYS[1]) == ARGV[1] then "
		"return {redis.call('GET', KEYS[2]), redis.call('TTL', KEYS[1])} else return false end";
	// 读锁 + ctx(登录重建用,不预设 battle_id):锁不存在返回 nil;否则返回 {锁值, ctx 或 nil, TTL}。
	constexpr char kGetLockAndCtxScript[] =
		"local v = redis.call('GET', KEYS[1]); if not v then return false end; "
		"return {v, redis.call('GET', KEYS[2]), redis.call('TTL', KEYS[1])}";

	// 锁 TTL(秒)= 冻结到期时刻的剩余 + 余量:锁必须活得比 InBattleComp 久。
	// 传入的是冻结到期时刻(PlayerBattleSystem::FreezeExpiresAtMs),不是裸的 deadline_ms;FIGHTING 用 FightingLockTtlSecFor。
	uint64_t LockTtlSecFor(const uint64_t deadlineMs, const uint64_t nowMs)
	{
		const uint64_t remainSec = deadlineMs > nowMs ? (deadlineMs - nowMs) / 1000 : 0;
		return remainSec + PlayerBattleSystem::kLockExtraTtlSec;
	}

	// FIGHTING 冻结的锁 TTL:按冻结最晚可能的到期时刻 FreezeHoldDeadlineMs(deadline + 终局提交宽限 + 收件箱投递余量
	// + 续冻上限)计,从确认开局那一刻起就是这个值,不分到期之前还是续冻之中(2026-11-06 负责人定)。
	// 锁必须活得比冻结久,而 FIGHTING 冻结可以续到续冻上限:锁若只到 FreezeExpiresAtMs + 余量,玩家在到期之前下线、
	// 超过那个时刻才重登,就读不到锁、重建不出冻结,到期也就不会查终局 —— 迟到的结算照样按持有量夹紧;match 也会提前放下一场。
	// 代价:结算一直不来的局,锁最多多留约一个续冻上限,这段时间 match 按 battle:lock 拒绝排队,有界;
	// 正常结算、取消、到期解冻都照旧按条件删锁,不受影响。续冻一拍续的锁、重登 / 迟到确认按锁重建时的复核都走这里。
	uint64_t FightingLockTtlSecFor(const uint64_t deadlineMs, const uint64_t nowMs)
	{
		return LockTtlSecFor(PlayerBattleSystem::FreezeHoldDeadlineMs(deadlineMs), nowMs);
	}

	// 条件删锁脚本的回包处理:生产 EVAL 回调与测试注入的发送器共用这一个函数。
	// 脚本返回 1 = 锁是这一次删掉的,0 = 锁不存在(已过期 / 已删)或已易主(玩家进了下一场),都不是错误。
	void OnBattleLockReleaseReply(const uint64_t playerId, const uint64_t battleId, const entt::entity refollow,
								  const redisReply* reply)
	{
		if (reply == nullptr || reply->type == REDIS_REPLY_ERROR)
		{
			LOG_ERROR << "[PlayerBattle] Conditional lock deletion EVAL failed, player_id=" << playerId
					  << " battle_id=" << battleId;
			return;
		}
		if (reply->type == REDIS_REPLY_INTEGER && reply->integer == 0)
		{
			LOG_INFO << "[PlayerBattle] Conditional lock deletion did not match(lock missing or value mismatch), player_id="
					 << playerId << " battle_id=" << battleId;
			return;
		}
		if (refollow == entt::null || reply->type != REDIS_REPLY_INTEGER)
		{
			return;
		}
		// 锁确实删掉了。回调期间实体可能已销毁 / 句柄被复用,或已备战下一局(那一局解冻时自会触发跟随)。
		if (!tlsEcs.actorRegistry.valid(refollow) || tlsEcs.GetPlayerId(refollow) != playerId ||
			tlsEcs.actorRegistry.any_of<InBattleComp>(refollow))
		{
			return;
		}
		gFollowWakeFn(refollow);
	}

	// 条件删锁(连带 ctx):锁值 == battleId 才 DEL(所有删锁路径统一走这里,防迟到的取消/结算/作废
	// 误删玩家下一场战斗的锁)。fire-and-forget:锁是咨询性的,失败最迟随 TTL 过期。
	//
	// refollowOnDelete(默认不填):锁**确实被这条命令删掉**、且那时实体仍是这个玩家、身上没有 InBattleComp 时,
	// 补一次组队跟随检查。给「解冻早已发生、锁现在才真正放掉」的调用方用(按锁重建守卫 kSkipExpired 的补删、
	// CompleteInboxSettlement 组件已不在的分支):解冻时 RemoveInBattleComp 触发的跟随链读到锁还在(或读失败)会
	// fail-closed 跳过,之后没有别的东西再触发跟随,队员会一直留在原场景。
	// 先删锁再摘组件的调用方(ClearBattleFreeze 等)不要填:摘组件那一刻已经触发了跟随,而且排在删锁之后(同 slot 停等)。
	void DeleteBattleLockIfMatch(const uint64_t playerId, const uint64_t battleId,
								 const entt::entity refollowOnDelete = entt::null)
	{
		// 测试注入的发送器排在 Redis 可用判断之前:单测不连 Redis,也要能观察删锁命令与回包处理。生产恒为空。
		if (gBattleLockReleaseSender != nullptr)
		{
			const bool sent = gBattleLockReleaseSender(playerId, battleId,
				[playerId, battleId, refollow = refollowOnDelete](redisReply* reply) {
					OnBattleLockReleaseReply(playerId, battleId, refollow, reply);
				});
			if (!sent)
			{
				LOG_WARN << "[PlayerBattle] Conditional lock deletion not sent, player_id=" << playerId
						 << " battle_id=" << battleId << ", lock will expire with its TTL";
			}
			return;
		}
		if (!RedisReady())
		{
			// 非致命:锁带 EX,最迟随 TTL 过期;记日志说明串行化窗口靠 TTL 兜底
			LOG_WARN << "[PlayerBattle] Conditional lock deletion skipped(Redis disconnected), player_id=" << playerId
					 << " battle_id=" << battleId << ", lock will expire with its TTL";
			return;
		}
		// entt::entity 用 init-capture(MSVC C3495,见 copilot-instructions)。
		const int ret = tlsPlayerRedis.command(
			[playerId, battleId, refollow = refollowOnDelete](hiredis::Hiredis*, redisReply* reply) {
				OnBattleLockReleaseReply(playerId, battleId, refollow, reply);
			},
			(std::string("EVAL %s 2 ") + kBattleLockKeyFmt + " " + kBattleCtxKeyFmt + " %llu").c_str(),
			kDeleteLockIfMatchScript, playerId, playerId, battleId);
		if (ret != REDIS_OK)
		{
			LOG_WARN << "[PlayerBattle] Conditional lock deletion not sent, player_id=" << playerId
					 << " battle_id=" << battleId << ", lock will expire with its TTL";
		}
	}

	// 整笔结算此刻能不能应用。**必须是 BagService / CurrencySystem 内部闸的超集**:
	//  - IsPlayerFrozen:与那两个入口同判据;
	//  - PlayerTravelHandoffComp / UnregisterPlayer:这两个窗口里存盘入口开头直接跳过写盘,应用了也落不了盘。
	// 不应用就回 DEFER,条目留在收件箱,由本节点续做或目的地节点重拉。
	bool IsSettlementApplicable(entt::entity player)
	{
		return !PlayerLifecycleSystem::IsPlayerFrozen(player) &&
			   !tlsEcs.actorRegistry.any_of<PlayerTravelHandoffComp, UnregisterPlayer>(player);
	}

	// 从 InBattleComp 组 ctx 序列化(ctx 就是组件本身的镜像,不另造并行结构)
	std::string SerializeBattleCtx(const InBattleComp& inBattle)
	{
		return inBattle.SerializeAsString();
	}

	// 这一局冻结当前该有的锁 TTL:按冻结最晚可能的到期时刻算。写锁、重试与重建复核共用。
	// PREPARING 按备战期限(FreezeExpiresAtMs);FIGHTING(以及没有状态、按 FIGHTING 处理的)走 FightingLockTtlSecFor,
	// 按续冻上限 FreezeHoldDeadlineMs 算。
	uint64_t LockTtlSecOf(const InBattleComp& inBattle, const uint64_t nowMs)
	{
		if (inBattle.state() != IN_BATTLE_STATE_PREPARING)
		{
			return FightingLockTtlSecFor(inBattle.deadline_ms(), nowMs);
		}
		return LockTtlSecFor(PlayerBattleSystem::FreezeExpiresAtMs(/*preparing=*/true, inBattle.deadline_ms(),
																   inBattle.prepare_deadline_ms()),
							 nowMs);
	}

	// 锁写入 / 续期重试的门禁:按 player_id 现取实体。entityPresent = 实体仍在本节点且身份仍是这名玩家;
	// 返回它身上属于这一局的 InBattleComp,没有(已摘 / 已是别的局)返回 nullptr。只读。
	const InBattleComp* CurrentFreezeOf(const uint64_t playerId, const uint64_t battleId, bool& entityPresent)
	{
		const auto player = tlsEcs.GetPlayer(playerId);
		entityPresent = tlsEcs.actorRegistry.valid(player) && tlsEcs.GetPlayerId(player) == playerId;
		if (!entityPresent)
		{
			return nullptr;
		}
		const auto* inBattle = tlsEcs.actorRegistry.try_get<InBattleComp>(player);
		return (inBattle != nullptr && inBattle->battle_id() == battleId) ? inBattle : nullptr;
	}

	// 仅适配这三条 Lua 的自有结果，不把空回复、错误或本地停止当成整数结论。
	struct LockReply
	{
		battle_lock::ReplyShape shape = battle_lock::ReplyShape::kNoReply;
		int64_t integer = 0;
		std::string text;
	};

	auto AwaitLockReply(muduo::net::EventLoop& loop, const char* op, uint64_t playerId,
		uint64_t battleId, uint64_t ttlSec, const std::string& payload)
	{
		return loop_async::AwaitCallback<LockReply>(loop,
			[op, playerId, battleId, ttlSec, payload](auto done) {
				auto receive = [done](redisReply* reply) {
					done(LockReply{battle_lock::ReplyShapeOf(reply), reply ? reply->integer : 0,
						std::string(battle_lock::ReplyText(reply))});
				};
				if (gLockScriptSender)
					return gLockScriptSender(op, playerId, battleId, ttlSec, payload, receive) == REDIS_OK;
				if (!RedisReady()) return false;
				return tlsPlayerRedis.command([receive](hiredis::Hiredis*, redisReply* reply) { receive(reply); },
					(std::string("EVAL %s 2 ") + kBattleLockKeyFmt + " " + kBattleCtxKeyFmt + " %llu %llu %b").c_str(),
					op == kLockOpWrite ? kWriteBattleLockScript : kRenewBattleLockIfMatchScript,
					playerId, playerId, battleId, ttlSec, payload.data(), payload.size()) == REDIS_OK;
			// 没发出去与空回复同样按「没问到」重试,但日志要分得开:前者是 Redis 未就绪 / 命令被本地拒绝,后者是连接上丢了回复。
			}, LockReply{battle_lock::ReplyShape::kNoReply, 0, "not sent (Redis not ready or command rejected locally)"});
	}

	void ResolveRebuiltFreezeRecheck(uint64_t playerId, uint64_t battleId, uint32_t attemptNumber,
		battle_lock::ReplyShape shape, int64_t integer);

	// 一次写入链的门禁、发送、回复、原预算退避在同一函数。续期账本只负责同局去重。
	// 写锁、续期、重建复核三条链都挂在 Scene 的持久作用域(TryStartDurable),不随瞬态任务一起停:
	// 维护 P1、普通停机、身份冲突之后,备战(gRPC 保留到 P3)和确认补发(Kafka)仍会到达。这时不写锁、不续期,
	// 玩家在别的节点重登就重建不出冻结,match 排队也看不到这一局,落进 player_battle.h「冻结何时算到期」一节说的资产复制。
	// 改造前的回调链在这些窗口里照常发送。只有拆除前的 StopAll 才结束本地等待,已经发出的 Lua 不撤回。
	boost::cobalt::task<void> RunLockWrites(muduo::net::EventLoop& loop, const char* op,
		uint64_t playerId, uint64_t battleId, uint64_t deadlineMs, uint64_t ttlSec, std::string payload)
	{
		const bool renew = op == kLockOpRenew;
		if (renew && !TryBeginRenewChain(playerId, battleId)) co_return;
		struct RenewalGuard
		{
			uint64_t playerId, battleId;
			bool active;
			~RenewalGuard() { if (active) EndRenewChain(playerId, battleId); }
		} renewal{playerId, battleId, renew};
		auto attempt = StartLockWriteAttempt({});
		for (;;)
		{
			// 组件只在这段同步门禁内借用，任何 await 都只保存 ID 和自有字节。
			if (op == kLockOpWrite || attempt.number > 1)
			{
				bool present = false;
				const auto* current = CurrentFreezeOf(playerId, battleId, present);
				if (renew)
				{
					if (present && (!current || current->state() != IN_BATTLE_STATE_FIGHTING))
					{
						LOG_INFO << "[PlayerBattle] Conditional renewal retry abandoned: battle no longer fighting on this entity, player_id="
								 << playerId << " battle_id=" << battleId;
						co_return;
					}
					if (present) { deadlineMs = current->deadline_ms(); payload = SerializeBattleCtx(*current); }
				}
				else
				{
					if (!current)
					{
						// 写锁首发由调用方刚核对过冻结,走到这里只会是重试。
						LOG_INFO << "[PlayerBattle] Battle lock retry abandoned: freeze no longer belongs to this battle, op=" << op
								 << " player_id=" << playerId << " battle_id=" << battleId << " attempt=" << attempt.number;
						co_return;
					}
					ttlSec = LockTtlSecOf(*current, TimeSystem::NowMillisecondsUTC());
					payload = SerializeBattleCtx(*current);
				}
			}
			if (renew) ttlSec = FightingLockTtlSecFor(deadlineMs, TimeSystem::NowMillisecondsUTC());
			NotifyLockScriptObserver(op, playerId, battleId, ttlSec, payload);
			auto reply = co_await AwaitLockReply(loop, op, playerId, battleId, ttlSec, payload);
			if (!reply || !SceneTaskScope::IsDurableAccepting()) co_return;
			if (battle_lock::IsLockWriteAnswered(reply->shape))
			{
				if (op == kLockOpRebuildRecheck)
					ResolveRebuiltFreezeRecheck(playerId, battleId, attempt.number, reply->shape, reply->integer);
				else if (renew && reply->integer == 0)
					LOG_WARN << "[PlayerBattle] Conditional renewal did not match, player_id=" << playerId << " battle_id=" << battleId;
				co_return;
			}
			LOG_WARN << "[PlayerBattle] Lock unanswered op=" << op << " player_id=" << playerId
				<< " battle_id=" << battleId << " reply=" << battle_lock::ReplyShapeName(reply->shape) << " err=" << reply->text;
			auto next = co_await loop_async::AwaitCallback<LockWriteAttempt>(loop,
				[op, playerId, battleId, attempt](auto done) {
					return ScheduleLockWriteRetry(op, playerId, battleId, attempt, std::move(done));
				}, LockWriteAttempt{0, attempt.firstSentMs});
			if (!next || !SceneTaskScope::IsDurableAccepting() || next->number == 0) co_return;
			attempt = *next;
		}
	}

	uint64_t WriteBattleLock(uint64_t playerId, uint64_t battleId)
	{
		if (!SceneTaskScope::IsDurableAccepting()) return 0;
		bool present = false;
		const auto* current = CurrentFreezeOf(playerId, battleId, present);
		if (!current) return 0;
		const auto ttl = LockTtlSecOf(*current, TimeSystem::NowMillisecondsUTC());
		auto& loop = *muduo::net::EventLoop::getEventLoopOfCurrentThread();
		(void)SceneTaskScope::TryStartDurable(RunLockWrites(loop, kLockOpWrite, playerId, battleId, 0, ttl, {}), "WriteBattleLock");
		return ttl;
	}

	void ConfirmBattleLockIfMatch(uint64_t playerId, uint64_t battleId, uint64_t deadlineMs, std::string payload)
	{
		if (!SceneTaskScope::IsDurableAccepting()) return;
		auto& loop = *muduo::net::EventLoop::getEventLoopOfCurrentThread();
		(void)SceneTaskScope::TryStartDurable(RunLockWrites(loop, kLockOpRenew, playerId, battleId, deadlineMs, 0, std::move(payload)), "RenewBattleLock");
	}

	// 重建复核(ConfirmRebuiltFreeze)应答的「判定 + 撤销」。只有整数应答是结论:
	//  - 非 0:锁仍是本战斗的,重建成立(脚本已同步 ctx),什么都不做;
	//  - 0 且是首发(attemptNumber == 1):锁在「读锁」与「这条复核」之间被删或易主,重建所依据的锁已不成立 ——
	//    撤掉刚挂的 InBattleComp,不让已结算 / 已取消的战斗把玩家冻到 reaper(metric=battle_freeze_rebuild_reverted);
	//  - 0 且是重试(attemptNumber != 1;0 是调用方缺陷,同样保守按重试处理):重试与读锁之间隔着数秒到数十秒,
	//    这期间锁也可能只是按 TTL 自然过期,与「被结算 / 取消删掉」分不清;撤销会把仍在打的玩家解冻,所以只记 WARN、
	//    保留冻结,交给 reaper 按期限解冻(结算只走收件箱,不依赖锁)。
	// 撤销与 WARN 都先按 player_id 现取实体复核:实体有效、身份仍是这名玩家、InBattleComp 仍是这一局才处置 ——
	// 回调期间实体可能已销毁 / 句柄被复用,或重建出的组件已被结算 / reaper 摘掉,那时什么都不做。
	// 非整数形状(没问到)不是结论,这里什么都不做:重试由 RunLockWrites 按首发时刻和原预算安排。
	// integer 只在 shape == kInteger 时有意义。PlayerBattleSystem::OnRebuiltFreezeRecheckReply 转调这里,
	// 生产回调与单测走同一个函数。
	void ResolveRebuiltFreezeRecheck(const uint64_t playerId, const uint64_t battleId, const uint32_t attemptNumber,
									 const battle_lock::ReplyShape shape, const int64_t integer)
	{
		if (!battle_lock::IsLockWriteAnswered(shape))
		{
			return; // 没问到:不是结论,重试归调用方
		}
		if (integer != 0)
		{
			return; // 锁仍匹配:重建成立,ctx 已同步
		}
		const auto player = tlsEcs.GetPlayer(playerId);
		if (!tlsEcs.actorRegistry.valid(player) || tlsEcs.GetPlayerId(player) != playerId)
		{
			return;
		}
		const auto* current = tlsEcs.actorRegistry.try_get<InBattleComp>(player);
		if (current == nullptr || current->battle_id() != battleId)
		{
			return;
		}
		if (attemptNumber != 1)
		{
			LOG_WARN << "[PlayerBattle] Reconstruction recheck retry found lock missing;freeze kept, player_id=" << playerId
					 << " battle_id=" << battleId << " attempt=" << attemptNumber
					 << "(lock may have expired naturally;reaper releases the freeze at the deadline)";
			return;
		}
		RemoveInBattleComp(player);
		LOG_WARN << "[PlayerBattle] metric=battle_freeze_rebuild_reverted player_id=" << playerId
				 << " battle_id=" << battleId << ",lock was deleted during reconstruction(settlement/cancellation completed);reconstruction canceled";
	}

	// 重建冻结后的复核(RebuildBattleFreezeFromLock / RestoreBattleFreezeOnLogin 挂上 InBattleComp 之后立刻调用)。
	// 重建依据的是「读锁的那条 EVAL」看到的锁;同一 slot 上排在它之后、这条复核之前的命令里,可能有一条同 battle_id 的
	// 条件删锁(结算确认 / 取消)。复核 = 再发一次条件续期 + 覆写 ctx;拿到整数应答后的处置(首发回 0 才撤销重建,
	// 重试回 0 保留冻结)在 ResolveRebuiltFreezeRecheck。
	// 不去重(只有续期去重,gRenewChainsInFlight 的说明):每一次重建都要有自己的首发,否则新一次重建失去「首发回 0 即撤销」的保护。
	// 没问到时有界重试。重发前按 player_id 现取实体,重建出的这一局组件还在才再问,
	// 期限与 ctx 按此刻的组件重算(期间迟到的确认可能已把 PREPARING 升级成 FIGHTING)。
	// 闭包只捕获 id 与字节,不捕获实体句柄(实体可能已销毁、句柄可能被复用)。
	// 重试放弃时重建保留,由 reaper 按期限解冻。
	void ConfirmRebuiltFreeze(uint64_t playerId, uint64_t battleId, uint64_t ttlSec, std::string payload)
	{
		if (!SceneTaskScope::IsDurableAccepting()) return;
		auto& loop = *muduo::net::EventLoop::getEventLoopOfCurrentThread();
		(void)SceneTaskScope::TryStartDurable(RunLockWrites(loop, kLockOpRebuildRecheck, playerId, battleId, 0, ttlSec, std::move(payload)), "RecheckBattleLock");
	}

	// 解析 kGetCtxIfLockMatchScript / kGetLockAndCtxScript 返回的数组元素:
	// 字符串元素 -> out(nil 元素留空并返回 false)
	bool ReplyElementToString(const redisReply* reply, const size_t index, std::string& out)
	{
		if (reply == nullptr || reply->type != REDIS_REPLY_ARRAY || index >= reply->elements)
		{
			return false;
		}
		const redisReply* element = reply->element[index];
		if (element == nullptr || element->type != REDIS_REPLY_STRING)
		{
			return false;
		}
		out.assign(element->str, element->len);
		return true;
	}

	int64_t ReplyElementToInteger(const redisReply* reply, const size_t index)
	{
		if (reply == nullptr || reply->type != REDIS_REPLY_ARRAY || index >= reply->elements)
		{
			return -1;
		}
		const redisReply* element = reply->element[index];
		return (element != nullptr && element->type == REDIS_REPLY_INTEGER) ? element->integer : -1;
	}

	// 用锁 + ctx 重建 InBattleComp 的取值规则(登录重建 / 迟到确认重建共用):
	//   battle_id 以调用方给的(锁值 / 事件)为准;ctx 的 battle_id 不一致时 ctx 整体不信;
	//   deadline_ms 缺失时按锁剩余 TTL 反推(PlayerBattleSystem::InferFightingDeadlineFromLockTtlMs,评审修正 2026-11-06):
	//   锁 TTL = 冻结最晚到期时刻 + 60s 余量,FIGHTING 的最晚到期又比 deadline 晚终局提交宽限 + 收件箱投递余量 + 续冻上限
	//   (FreezeHoldDeadlineMs)。看得出是 FIGHTING 锁时把这几项扣掉,首次查询落在原来的到期时刻(只会偏晚两秒以内);
	//   分不清是不是备战期的锁时照旧取「现在 + TTL」,宁可多冻。到期之后照样先查终局、受续冻上限约束;
	//   保证 reaper 一定有一个非 0 的到期时刻,不会把玩家永久冻住。
	InBattleComp BuildInBattleFromCtx(const uint64_t battleId, const std::string& ctxPayload,
									  const int64_t lockTtlSec, const uint64_t nowMs)
	{
		InBattleComp rebuilt;
		InBattleComp ctx;
		if (!ctxPayload.empty() && ctx.ParseFromString(ctxPayload) && ctx.battle_id() == battleId)
		{
			rebuilt = ctx;
		}
		rebuilt.set_battle_id(battleId);
		if (rebuilt.state() == IN_BATTLE_STATE_NONE)
		{
			// 没有 ctx 可信:锁存在即战斗在途,保守按战斗中处理(拒绝再入局,等结算/reaper)
			rebuilt.set_state(IN_BATTLE_STATE_FIGHTING);
		}
		if (rebuilt.deadline_ms() == 0)
		{
			rebuilt.set_deadline_ms(PlayerBattleSystem::InferFightingDeadlineFromLockTtlMs(nowMs, lockTtlSec));
		}
		if (rebuilt.state() == IN_BATTLE_STATE_PREPARING && rebuilt.prepare_deadline_ms() == 0)
		{
			rebuilt.set_prepare_deadline_ms(rebuilt.deadline_ms());
		}
		return rebuilt;
	}

	// 摘 InBattleComp + 条件删锁(取消/作废的统一收尾)
	void ClearBattleFreeze(entt::entity player, const uint64_t playerId, const uint64_t battleId)
	{
		// 先发条件删锁再摘组件:摘组件会触发组队跟随链,它读 battle:lock fail-closed
		DeleteBattleLockIfMatch(playerId, battleId);
		RemoveInBattleComp(player);
	}

	// 物品能否在回合制战斗里使用(只决定快照带不带)。结算不重判:消耗是开局快照与引擎早已定下的事实。
	bool IsBattleUsableItem(uint32_t configId)
	{
		const auto* itemRow = ItemTableManager::Instance().FindByIdSilent(configId).first;
		return itemRow != nullptr && itemRow->battle_usable() != 0;
	}

	// item 号段此刻能不能为掉落铸齐新实例的身份(审计 cpp-economy#0)。
	// 号段发不出号时,BagService 会整批拒绝入包。在任何副作用之前判:不够就整笔 DEFER,条目留在收件箱。
	// 这与资产通道 ApplyCredit 的号段预检同一纪律、同一口径。上界见 BagService::MaxNewInstancesFor:
	// 主背包放不下时一个号都不铸,转投临时格最多也只铸这么多,一份余量就覆盖两段式发放。
	// 判出不足就请号段客户端续一段(RequestTopUp,审计 R2 r2-battle#3):预取只在发号之后判阈值,这里整笔 DEFER、
	// 一个号都不发,「需要量 > 剩余 > 预取阈值、又没有下一段」时没人续段,这笔结算就永远 DEFER、票停在 state 3。
	// 新段落下之后,收件箱的 DEFER 退避重试即可通过(续段那一次按不小于需要量的 step 领,不超过 MaxStep)。不发号。
	// 需要量大于 IdSegments 的 MaxStep(kExceedsMaxStep):续段永远凑不齐,但战斗结算不能拒收 —— 奖励与消耗都在这一笔里,
	// 拒了就丢奖励或白喝药。所以与号段停用(kUnavailable)一样整笔 DEFER 并打 ERROR,原件留在收件箱,调大 MaxStep 之后自愈。
	// 生产 MaxStep 1,000,000,一局掉落远到不了;走到这里就是配置写错(评审修正 2026-11-06)。
	bool HasItemGuidsForDrops(const ItemCountMap& gained, const uint64_t playerId, const uint64_t battleId)
	{
		if (gained.empty())
		{
			return true;
		}
		auto& segment = tlsGuidSegmentRegistry.Get(GuidKind::kItem);
		const uint64_t needed = BagService::MaxNewInstancesFor(gained);
		const uint64_t available = segment.Available();
		if (available >= needed)
		{
			return true;
		}
		const auto topUp = segment.RequestTopUp(needed);
		if (topUp == GuidSegmentClient::TopUpOutcome::kFetching || topUp == GuidSegmentClient::TopUpOutcome::kEnough)
		{
			LOG_WARN << "[PlayerBattle] metric=battle_settlement_item_guid_shortage player_id=" << playerId
					 << " battle_id=" << battleId << " needed=" << needed << " available=" << available
					 << " top_up=" << TopUpOutcomeName(topUp)
					 << ",item guid segment cannot cover drops;whole settlement deferred until the requested segment lands";
		}
		else
		{
			// 号段停用 / 停机(kUnavailable),或者这一批比一段最多能给的号还多(kExceedsMaxStep):续段帮不上忙,
			// 要人看配置(IdSegments 的 Enabled / MaxStep)。结算不能拒收,照样整笔 DEFER。
			LOG_ERROR << "[PlayerBattle] metric=battle_settlement_item_guid_shortage player_id=" << playerId
					  << " battle_id=" << battleId << " needed=" << needed << " available=" << available
					  << " max_step=" << segment.options().maxStep << " top_up=" << TopUpOutcomeName(topUp)
					  << ",item guid segment cannot cover drops and a top-up cannot help;whole settlement deferred";
		}
		return false;
	}

	// 推 BattleEndS2C(结算入账通知):个人终局只由 scene 在结算随存档提交之后推,battle 节点不发。
	void PushBattleEndToPlayer(entt::entity player, const ::BattleSettlementData& settlement)
	{
		::BattleEndS2C message;
		message.set_battle_id(settlement.battle_id());
		message.set_outcome(settlement.outcome());
		*message.mutable_settlement() = settlement;
		// 掉落放不进两个包、整批改投了邮件(battle-overflow-mail.md):这件事记在这一局的近期结果里(与结算同一次存档提交),
		// 从那里取,结算屏据此提示去邮箱领取。结果已被淘汰出列表(之后又结算了 10 局以上)时不带这个提示。
		if (const auto* history = tlsEcs.actorRegistry.try_get<PlayerBattleResultComp>(player); history != nullptr)
		{
			if (const auto* result = battle_result::FindByBattle(*history, settlement.battle_id()); result != nullptr)
			{
				message.set_items_mailed(result->items_mailed());
			}
		}
		SendToPlayerClient(BattleClientPlayerNotifyBattleEndMessageId, message, player);
	}

	// 解析 gate 路由(session_id / gate_node_id / gate_instance_id)。
	// gate_instance_id 取不到时留空并由调用方 fail-closed(宪法 §7 不变量 2)。
	struct GateRoute
	{
		uint32_t sessionId = 0;
		NodeId gateNodeId = 0;
		std::string gateInstanceId;
	};

	bool ResolveGateRoute(entt::entity player, GateRoute& out)
	{
		const auto* sessionPB = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(player);
		if (sessionPB == nullptr || sessionPB->gate_session_id() == 0)
		{
			return false;
		}
		out.sessionId = sessionPB->gate_session_id();
		out.gateNodeId = GetGateNodeId(sessionPB->gate_session_id());
		// node_id 不是 entt 实体句柄(network_utils.h 禁令),必须走 ResolveLocalZoneGateEntity
		if (const auto gateEntityOpt = ResolveLocalZoneGateEntity(sessionPB->gate_session_id()))
		{
			auto& gateRegistry = tlsNodeContextManager.GetRegistry(eNodeType::GateNodeService);
			if (const auto* gateNodeInfo = gateRegistry.try_get<NodeInfo>(*gateEntityOpt))
			{
				out.gateInstanceId = gateNodeInfo->node_uuid();
			}
		}
		return true;
	}
} // namespace

bool PlayerBattleSystem::IsInBattle(entt::entity player)
{
	return tlsEcs.actorRegistry.valid(player) &&
		   tlsEcs.actorRegistry.any_of<InBattleComp>(player);
}

bool PlayerBattleSystem::BuildBattleSnapshot(entt::entity player, ::BattlePlayerSnapshot& snapshot, uint32_t& errorTipId)
{
	const uint64_t playerId = tlsEcs.GetPlayerId(player);

	// —— 基础属性(必需;含 speed 出手序)——
	const auto* baseAttributes = tlsEcs.actorRegistry.try_get<BaseAttributesComp>(player);
	if (baseAttributes == nullptr)
	{
		LOG_ERROR << "[PlayerBattle] Snapshot creation failed: missing BaseAttributesComp, player_id=" << playerId;
		errorTipId = kEntityIsNull;
		return false;
	}

	// 空背包、空技能和空宠物列表有各自的已加载组件；组件本身缺失不是“玩家没有这些数据”。
	// 派生属性也应在加载完成时算好。先拒绝不完整输入，不能用一级、空列表或零加成拼出合法快照。
	if (!tlsEcs.actorRegistry.all_of<LevelComp, PlayerUint32Comp, PlayerProfileComp,
		PlayerSkillListComp, PlayerPetComp, PlayerBagsComp, DerivedAttributesComp>(player))
	{
		LOG_ERROR << "[PlayerBattle] Snapshot creation failed: persisted or derived component missing, player_id=" << playerId;
		errorTipId = kServiceUnavailable;
		return false;
	}

	const auto& registry = tlsEcs.actorRegistry;
	// —— 路由信息(必需;battle 节点出站 Kafka 全靠它,不查 etcd)——
	GateRoute gateRoute;
	if (!ResolveGateRoute(player, gateRoute))
	{
		LOG_ERROR << "[PlayerBattle] Snapshot creation failed: missing session snapshot(player offline?), player_id=" << playerId;
		errorTipId = kSessionNotFound;
		return false;
	}

	snapshot.set_player_id(playerId);
	// 名字副本来自 PlayerProfileComp(真源 data_service player_name;login 首次入场补齐,scene 只读)。
	// 名字字段尚未补齐时允许留空。
	const auto& profile = registry.get<PlayerProfileComp>(player);
	snapshot.set_player_name(profile.name());
	snapshot.set_appearance_id(profile.appearance_id());
	snapshot.set_gender(profile.gender());
	snapshot.set_class_id(registry.get<PlayerUint32Comp>(player).class_());

	// 生效等级(超上限的存储等级按上限,0 按 1):引擎的伤害 / 回血 / 怪物参考等级都读快照等级
	snapshot.set_level(playerlevel::CappedLevel(registry.get<LevelComp>(player).level()));

	*snapshot.mutable_base_attributes() = *baseAttributes;
	if (baseAttributes->speed() == 0)
	{
		// speed 缺配(存量数据/表未填):给默认值保证出手序可结算,缺配现状见 open_issues
		snapshot.mutable_base_attributes()->set_speed(kFallbackBattleSpeed);
		LOG_WARN << "[PlayerBattle] speed=0, using default action speed " << kFallbackBattleSpeed
				 << ", player_id=" << playerId;
	}

	// 存量 max_health=0 时保守取当前 HP(不给引擎超治疗空间)。
	const auto& derived = registry.get<DerivedAttributesComp>(player);
	const uint64_t maxHealth = derived.max_health() > 0
								   ? derived.max_health()
								   : std::max<uint64_t>(baseAttributes->health(), 1);
	snapshot.set_max_health(maxHealth);
	// max_mana / 二级属性:来自属性加点系统的 DerivedAttributesComp(登录加载即重算);
	// 存量蓝上限为 0 时取当前 MP；物伤/法伤/防御沿用已经计算出的派生值。
	const uint64_t maxMana = derived.max_mana() > 0
									 ? derived.max_mana()
								 : std::max<uint64_t>(baseAttributes->mana(), 1);
	snapshot.set_max_mana(maxMana);
	snapshot.set_physical_attack(derived.physical_attack());
	snapshot.set_magic_attack(derived.magic_attack());
	snapshot.set_defense(derived.defense());
	// 战斗类属性的装备加成(必杀 / 连击 / 反击 / 反震 / 抗性,equipment-attributes.md §4.5):
	// 整块原样拷给引擎，不把缺失组件伪装成全 0 加成。
	snapshot.mutable_combat()->CopyFrom(derived.combat());

	// —— 出战宝宝(player-pet.md §5):没带宝宝时不填,引擎侧自然没有这个单位 ——
	if (::BattlePetSnapshot petSnapshot; PetSystem::BuildBattleSnapshot(player, petSnapshot))
	{
		*snapshot.add_pets() = petSnapshot;
	}

	// —— 技能列表:只带回合制真能施放的(G6)+ 职业技能生效等级(class-skills.md §6.1)——
	// 被动 / 开关 / 持续施法不进 skill_table_ids;不生效的条目(别的职业 / 已退役的占位技能 / 等级不够)一律不带。
	// 过滤规则只在 PlayerSkillSystem 一处,引擎 InitPlayers 再复核一道。
	PlayerSkillSystem::FillBattleSnapshot(player, snapshot);

	// —— 参战持续 buff(时长换算为回合)——
	if (const auto* buffList = tlsEcs.actorRegistry.try_get<BuffListComp>(player))
	{
		for (const auto& [buffId, entry] : *buffList)
		{
			const auto* buffRow =
				BuffTableManager::Instance().FindByIdSilent(entry.buffPb.buff_table_id()).first;
			if (buffRow == nullptr)
			{
				continue;
			}
			// 控制类不带进战斗(G5)。理由是时长口径:场景里只剩半秒的眩晕,这里
			// 按表全量时长换算会变成整整几个回合,玩家开局就被跳过好几手;
			// 而 PVP 里对手可以在应战前挂个控制再进局。控制的归属应当是战斗内产生的。
			const uint32_t buffType = buffRow->buff_type();
			if (buffType == kBuffTypeStun || buffType == kBuffTypeFreeze ||
				buffType == kBuffTypeSilence)
			{
				continue;
			}
			// 瞬时 buff(非无限且 duration<=0)在实时侧是"挂上即到期";原先这里把它
			// 当成无限持续带进去,战斗内永不消失。与引擎口径对齐:直接不带。
			if (buffRow->infinite_duration() == 0 && buffRow->duration() <= 0)
			{
				continue;
			}
			auto* buffEntry = snapshot.add_buffs();
			buffEntry->set_buff_id(buffId);
			buffEntry->set_buff_table_id(entry.buffPb.buff_table_id());
			buffEntry->set_layer(std::max<uint32_t>(entry.buffPb.layer(), 1));
			// caster 域转换(G5):BuffComp.caster 是 scene 的 entt 实体整数,而引擎的
			// actor_id 域是 player_id(或带 bit63 的局内号)。原样塞过去,一旦某个
			// entt 整数恰好等于某玩家的 player_id,引擎就会把这条 buff 算到那名玩家头上
			// (叠层隔离、BUFF_TICK 事件来源都按它判)。只有"施法者就是自己"这一种情形
			// 能可靠映射;其余一律置 0 = 无施法者。
			uint64_t casterActorId = 0;
			if (entry.buffPb.caster() != 0 &&
				entry.buffPb.caster() == static_cast<uint64_t>(entt::to_integral(player)))
			{
				casterActorId = playerId;
			}
			buffEntry->set_caster_id(casterActorId);
			// 剩余时长拿不到(TimerTaskComp 设计上不暴露 deadline,BuffComp 也没有到期字段),
			// 仍按表全量时长换算回合:rounds = max(1, ceil(ms / 6000))。
			// 这对增益是偏宽、对减益是偏严,但控制类已被上面剔除,剩下的都是周期效果类,
			// 影响有界。要精确剩余时长必须给 BuffComp 加到期时间字段(另开任务)。
			if (buffRow->infinite_duration() != 0)
			{
				buffEntry->set_remain_rounds(0); // 0 = 无限持续
			}
			else
			{
				buffEntry->set_remain_rounds(turnbattle::RoundsFromSeconds(buffRow->duration()));
			}
		}
	}

	// —— 战斗道具副本(G2)——
	// 只从主背包取、且只取 ItemTable.battle_usable 非 0 的物品。
	//   * 只取主背包:仓库/装备栏不参战;临时格(kTemporary)是主背包放不下整批掉落时的溢出缓冲,
	//     满了就拒、不挤掉旧物(2026-11-05 起;之前的先进先出已删除,bag-rule-policy-layering.md §6.6),玩家可以随时取回。
	//     结算扣消耗只看主背包,把它计进副本就是让战斗用一份结算扣不到的药。
	//   * 这是一份**数量副本**,不带 guid/格子:实例身份归 scene(战斗掉落 iid 归属决策),
	//     战斗服只拿可丢弃的派生副本(D1)。
	//   * 副本里的数量随即被本局的道具预留占住(PrepareBattle,docs/design/battle-item-reservation.md),
	//     已被别的预留占住的数量不进副本:那一局终局之前它们随时可能被它的结算扣走,两局各把同一瓶药算进副本,
	//     总有一边扣不出。备战这一刻本局还没有预留,这里减掉的全是旧局的。
	const auto& bags = registry.get<PlayerBagsComp>(player);
	std::map<uint32_t, uint64_t> battleItems;  // 有序:同一背包每次出的副本顺序稳定
	bags.bags[kInventory].ForEachItem([&battleItems](Guid, const ItemComp& item) {
		// 副本只计结算时扣得出的普通单位(ItemStore::IsCountableByConfig,与 RemoveItemsClamped 同口径):
		// 带更新版本字段的药结算扣不到,计进副本就是白用(混版本设计 §4.4 第 6 批)。
		if (!IsBattleUsableItem(item.config_id()) || !ItemStore::IsCountableByConfig(item))
		{
			return;
		}
		battleItems[item.config_id()] += item.size();
	});
	for (const auto& [configId, count] : battleItems)
	{
		const uint64_t held = BagService::HeldCount(player, bags.bags[kInventory], configId);
		if (count <= held)
		{
			continue;
		}
		auto* itemEntry = snapshot.add_items();
		itemEntry->set_item_table_id(configId);
		itemEntry->set_count(count - held);
	}

	// —— 路由 ——
	auto* routing = snapshot.mutable_routing();
	routing->set_session_id(gateRoute.sessionId);
	routing->set_gate_node_id(gateRoute.gateNodeId);
	routing->set_gate_instance_id(gateRoute.gateInstanceId);
	if (gateRoute.gateInstanceId.empty())
	{
		// 不 fail-closed 拒绝备战:battle 节点出站时会按不变量 2 拒发并报错,
		// 这里先告警便于定位 gate 发现异常
		LOG_WARN << "[PlayerBattle] gate_instance_id is empty(gate node not discovered locally), player_id="
				 << playerId << " session_id=" << gateRoute.sessionId;
	}
	const auto& selfNode = GetNodeInfo();
	routing->set_scene_node_id(selfNode.node_id());
	routing->set_scene_instance_id(selfNode.node_uuid());
	routing->set_zone_id(GetZoneId());

	// team_index:阵营分配是 match 的编排职责,scene 不感知,由 match 在 CreateBattle 前改写
	snapshot.set_team_index(0);

	// 配表指纹:本节点七张战斗表的指纹(表加载完成时已缓存),match 比对全员一致、
	// battle 开局时再与自身比对(cross-zone-matchmaking.md §10)
	snapshot.set_table_fingerprint(turnbattle::BattleTableFingerprint::Current());
	return true;
}

void PlayerBattleSystem::PrepareBattle(const ::PrepareBattleRequest& request, ::PrepareBattleResponse& response)
{
	const uint64_t playerId = request.player_id();
	const uint64_t battleId = request.battle_id();
    const uint64_t prepareDeadlineMs = request.prepare_deadline_ms() != 0 ? request.prepare_deadline_ms() : request.deadline_ms();

	if (playerId == 0 || battleId == 0 || request.deadline_ms() == 0 || prepareDeadlineMs <= TimeSystem::NowMillisecondsUTC())
	{
		LOG_ERROR << "[PlayerBattle] PrepareBattle invalid arguments: player_id=" << playerId
				  << " battle_id=" << battleId << " deadline_ms=" << request.deadline_ms();
		response.mutable_error_message()->set_id(kInvalidParameter);
		return;
	}

	// 原战斗结算可以完成，但已接受交易seal后不能在排空间隙再进入新战斗。
	if (RoleAdmissionSystem::IsSealed(playerId))
	{
		response.mutable_error_message()->set_id(kFeatureUnavailable);
		return;
	}
	const auto player = tlsEcs.GetPlayer(playerId);
	if (!tlsEcs.actorRegistry.valid(player))
	{
		// 玩家不在本节点(已下线或被改派):match 按补偿矩阵处理
		LOG_WARN << "[PlayerBattle] PrepareBattle rejected: player offline, player_id=" << playerId
				 << " battle_id=" << battleId;
		response.mutable_error_message()->set_id(kEntityIsNull);
		return;
	}

	// 换图在途:交接冻结(PlayerFrozenComp,实体只读)、交接意图、普通 EnterScene / 镜像建场景的应答还没回、
	// 作废交接的 handoff 标记未确认撤回 —— 判据与客户端 EnterScene 的发送侧闸是同一个 IsSceneChangeBusy。
	// 先到先得:换图先发出,备战就让路。放进来的话,随后落地的同节点路由会把战斗中的玩家换到别的场景
	// (破坏"战斗在途不切场景"),18 起交接时才被 StartTravelHandoff 的战斗复查挡下,客户端收到一条换图失败。
	// match 对任何备战拒绝走同一条补偿(肇事者出局、其余回队首),不分原因;与 Java 版
	// PlayerBattleService.prepare 的 switchInFlight → 1006 同口径(turn-based-battle-server.md §23)。
	if (PlayerLifecycleSystem::IsSceneChangeBusy(player))
	{
		const bool handoffInFlight =
			tlsEcs.actorRegistry.any_of<PlayerFrozenComp, PlayerTravelHandoffComp>(player);
		LOG_WARN << "[PlayerBattle] PrepareBattle rejected: scene change in flight, player_id=" << playerId
				 << " battle_id=" << battleId << " handoff_in_flight=" << handoffInFlight;
		response.mutable_error_message()->set_id(kFeatureUnavailable);
		return;
	}

	// 结算串行化(D4):上一场结算未落地前拒绝二次备战 —— 冻结拦截点之一
	if (const auto* existing = tlsEcs.actorRegistry.try_get<InBattleComp>(player))
	{
		LOG_WARN << "[PlayerBattle] PrepareBattle rejected: battle already in progress, player_id=" << playerId
				 << " existing_battle_id=" << existing->battle_id()
				 << " new_battle_id=" << battleId;
		response.mutable_error_message()->set_id(kFeatureUnavailable);
		return;
	}

	// 0 血玩家不得入局(纵深防御:结算/登录已做基础复活,这里兜底):
	// 引擎会在开局即判定该方战败,对手白拿一局,自己白排一次队。
	if (const auto* attrs = tlsEcs.actorRegistry.try_get<BaseAttributesComp>(player);
		attrs != nullptr && attrs->health() == 0)
	{
		LOG_WARN << "[PlayerBattle] PrepareBattle rejected: player has 0 health(dead and not revived), player_id=" << playerId
				 << " battle_id=" << battleId;
		response.mutable_error_message()->set_id(kFeatureUnavailable);
		return;
	}

	// 先组快照,失败不留任何冻结痕迹
	uint32_t errorTipId = kSuccess;
	if (!BuildBattleSnapshot(player, *response.mutable_snapshot(), errorTipId))
	{
		response.clear_snapshot();
		response.mutable_error_message()->set_id(errorTipId);
		return;
	}

	// 备战作废期限:match 在 gather 阶段崩溃时,已冻结成员只等这个短期限而不是整场战斗时限;
	// 旧版 match 不填(0)时沿用 deadline_ms,行为与一期一致
	// 快照与配表读取后，在冻结提交点再次校验；旧 worker 失租后迟到的 Prepare 不能复活。
    if(prepareDeadlineMs <= TimeSystem::NowMillisecondsUTC())
    {response.clear_snapshot();response.mutable_error_message()->set_id(kInvalidParameter);return;}

	// 战斗道具预留(docs/design/battle-item-reservation.md):快照里带进战斗的道具按配置号与数量占住,与冻结同一刻建立。
	// 结算只从这份预留里扣,扣完连同余量一起释放;备战取消、SQL 终局 ABORT、或没有可裁决原件时整份释放。
	// 冻结按期限(含续冻上限)解开之后,这些道具仍然转不走、用不掉,晚到的结算照样扣得出 —— 不再按持有量夹紧。
	// 建不起来就整笔拒绝、不留冻结:缺持久组件是加载不变量被破坏(kServiceUnavailable);同号已有预留是过期的重复备战,
	// 数量对不上是期间被拿走了(都回 kFeatureUnavailable,match 按补偿矩阵处理)。没有可参战道具的玩家不建预留。
	item_hold::HoldLines heldLines;
	for (const auto& item : response.snapshot().items())
	{
		heldLines[item.item_table_id()] += item.count();
	}
	if (!heldLines.empty())
	{
		const uint64_t nowMs = TimeSystem::NowMillisecondsUTC();
		const uint32_t holdError = BagService::PlaceHold(player, battleId, item_hold::ItemHoldKind::kBattle, heldLines, nowMs,
														 FreezeExpiresAtMs(/*preparing=*/false, request.deadline_ms(), 0));
		if (holdError != kSuccess)
		{
			LOG_ERROR << "[PlayerBattle] PrepareBattle rejected: battle item hold could not be placed, player_id=" << playerId
					  << " battle_id=" << battleId << " held_configs=" << heldLines.size() << " err=" << holdError;
			response.clear_snapshot();
			response.mutable_error_message()->set_id(holdError == kServiceUnavailable ? kServiceUnavailable : kFeatureUnavailable);
			return;
		}
	}

	// 上面已确认没有冻结且快照完整，此处才创建本次备战状态。
	auto& inBattle = tlsEcs.actorRegistry.emplace<InBattleComp>(player);
	inBattle.set_battle_id(battleId);
	inBattle.set_battle_node_id(request.battle_node_id());
	inBattle.set_deadline_ms(request.deadline_ms());
	inBattle.set_prepare_deadline_ms(prepareDeadlineMs);
	inBattle.set_state(IN_BATTLE_STATE_PREPARING);
	// 备战即停步:从这一刻起 MovementSystem 不再积分他的位移,客户端的移动上报也一律丢弃。
	// 速度留着的话,旁人按最后一条移动一直外推(spatial/system/actor_motion.h 的 kNotIntegratedByMovement)。
	MovementSystem::HaltMotion(player);
	// 记下写进快照 routing 的会话号(BuildBattleSnapshot 已保证非 0),ConfirmBattle 据此判断
	// 备战期间是否换过会话。emplace_or_replace:正常不会残留(随 RemoveInBattleComp 摘),防御性覆盖。
	tlsEcs.actorRegistry.emplace_or_replace<BattlePrepareSessionComp>(
		player, BattlePrepareSessionComp{response.snapshot().routing().session_id()});

	// 指纹与快照同值(match 用响应字段比对,battle 用快照字段比对)
	response.set_table_fingerprint(turnbattle::BattleTableFingerprint::Current());

	// 写 battle:lock:{player_id}=battle_id 与伴生 ctx(一条 Lua,EX = 备战期限 + 60s)。
	// 备战期锁只需活到备战作废期限;CreateBattle 确认后 ConfirmBattle 再按冻结到期时刻条件续期。
	// 权威判定仍是本实体的 InBattleComp,但锁不是可有可无:match 排队读它拦第二场,玩家完整下线再登录时新实体靠它重建冻结
	// (重建不出来,战斗闸就看不到这场战斗)。所以没问到时有界重试,重发前确认 InBattleComp 仍是这一局。
	const uint64_t ttlSec = WriteBattleLockOnPrepare(playerId, battleId);

	// 预留是存档数据,1 秒内落盘(不等定时存盘):节点在这之后崩溃,重登时冻结由锁重建,预留也已在盘上。
	if (!heldLines.empty())
	{
		PlayerLifecycleSystem::RequestKeyProgressSave(player, "battle_item_hold_placed");
	}

	LOG_INFO << "[PlayerBattle] Battle preparation freeze completed: player_id=" << playerId
			 << " battle_id=" << battleId
			 << " battle_node_id=" << request.battle_node_id()
			 << " deadline_ms=" << request.deadline_ms()
			 << " prepare_deadline_ms=" << prepareDeadlineMs
			 << " lock_ttl_sec=" << ttlSec
			 << " held_configs=" << heldLines.size()
			 << " table_fingerprint=" << response.table_fingerprint();
}

void PlayerBattleSystem::CancelBattlePrepare(const ::CancelBattlePrepareRequest& request)
{
	const uint64_t playerId = request.player_id();
	const uint64_t battleId = request.battle_id();

	const auto player = tlsEcs.GetPlayer(playerId);
	if (!tlsEcs.actorRegistry.valid(player))
	{
		// 实体不在(取消到达前玩家下线):没有 InBattleComp 可看状态,先按锁值条件读 ctx,
		// ctx 已是 FIGHTING(确认早于取消到达)同样拒绝 —— 与在线路径同一条规则
		if (!RedisReady())
		{
			LOG_WARN << "[PlayerBattle] CancelBattlePrepare: player offline and Redis disconnected;skipped, player_id="
					 << playerId << " battle_id=" << battleId << "(lock will expire with its TTL)";
			return;
		}
		const int ret = tlsPlayerRedis.command(
			[playerId, battleId](hiredis::Hiredis*, redisReply* reply) {
				if (reply == nullptr || reply->type == REDIS_REPLY_ERROR)
				{
					LOG_ERROR << "[PlayerBattle] CancelBattlePrepare lock/ctx read EVAL failed, player_id="
							  << playerId << " battle_id=" << battleId;
					return;
				}
				if (reply->type != REDIS_REPLY_ARRAY)
				{
					LOG_INFO << "[PlayerBattle] CancelBattlePrepare: player offline and lock missing/ownership changed;idempotently ignored, player_id="
							 << playerId << " battle_id=" << battleId;
					return;
				}
				std::string ctxPayload;
				InBattleComp ctx;
				if (ReplyElementToString(reply, 0, ctxPayload) && ctx.ParseFromString(ctxPayload) &&
					ctx.battle_id() == battleId && ctx.state() == IN_BATTLE_STATE_FIGHTING)
				{
					LOG_WARN << "[PlayerBattle] metric=battle_cancel_rejected_fighting player_id=" << playerId
							 << " battle_id=" << battleId
							 << ",player offline but ctx already FIGHTING(confirmation preceded cancellation);lock deletion rejected";
					return;
				}
				LOG_INFO << "[PlayerBattle] CancelBattlePrepare: player offline;delete lock conditionally by value, player_id="
						 << playerId << " battle_id=" << battleId;
				DeleteBattleLockIfMatch(playerId, battleId);
			},
			(std::string("EVAL %s 2 ") + kBattleLockKeyFmt + " " + kBattleCtxKeyFmt + " %llu").c_str(),
			kGetCtxIfLockMatchScript, playerId, playerId, battleId);
		if (ret != REDIS_OK)
		{
			LOG_WARN << "[PlayerBattle] CancelBattlePrepare: player offline and lock/ctx read not sent;skipped, player_id="
					 << playerId << " battle_id=" << battleId << "(lock will expire with its TTL)";
		}
		return;
	}

	const auto* inBattle = tlsEcs.actorRegistry.try_get<InBattleComp>(player);
	if (inBattle == nullptr)
	{
		// 已被结算/作废收尾,幂等 OK
		LOG_INFO << "[PlayerBattle] CancelBattlePrepare: no battle in progress;idempotently ignored, player_id="
				 << playerId << " battle_id=" << battleId;
		return;
	}
	if (inBattle->battle_id() != battleId)
	{
		// 迟到的取消(玩家已进入下一场):必须忽略,否则会解冻新战斗
		LOG_WARN << "[PlayerBattle] CancelBattlePrepare: battle_id mismatch;ignored, player_id=" << playerId
				 << " in_battle_id=" << inBattle->battle_id() << " cancel_battle_id=" << battleId;
		return;
	}
	if (inBattle->state() == IN_BATTLE_STATE_FIGHTING)
	{
		// 确认已到达 = battle 房间确实建成过。此时的取消是 match 侧 CreateBattle 超时后的
		// 过期回滚(DestroyBattle 可能失败、房间仍在打):解冻会让玩家同时进两局并丢掉本局结算。
		// 拒绝之,由结算 / 房间强制收尾 / reaper 按 deadline_ms 兜底。
		LOG_WARN << "[PlayerBattle] metric=battle_cancel_rejected_fighting player_id=" << playerId
				 << " battle_id=" << battleId << " deadline_ms=" << inBattle->deadline_ms()
				 << ",already FIGHTING;cancellation and unfreeze rejected(stale rollback)";
		return;
	}

	ClearBattleFreeze(player, playerId, battleId);
	// match 取消备战 = 这一局不会开打、也不会有结算:道具预留一并释放(docs/design/battle-item-reservation.md)。
	// 玩家不在本节点时(上面的离线分支)预留在他的存档里,下次加载后由 reaper 的预留复核按 SQL 终局收尾。
	ReleaseBattleItemHold(player, playerId, battleId, "prepare_cancelled");
	LOG_INFO << "[PlayerBattle] Battle preparation canceled and freeze released: player_id=" << playerId << " battle_id=" << battleId;
}

void PlayerBattleSystem::ConfirmBattle(const ::BattleConfirmedEvent& event)
{
	const uint64_t playerId = event.player_id();
	const uint64_t battleId = event.battle_id();
	if (playerId == 0 || battleId == 0)
	{
		LOG_ERROR << "[PlayerBattle] Invalid BattleConfirmedEvent: player_id=" << playerId
				  << " battle_id=" << battleId << " deadline_ms=" << event.deadline_ms();
		return;
	}
	const uint64_t nowMs = TimeSystem::NowMillisecondsUTC();

	const auto player = tlsEcs.GetPlayer(playerId);
	if (!tlsEcs.actorRegistry.valid(player))
	{
		// 玩家不在本节点(确认到达前下线/被改派):没有 InBattleComp 可升级,
		// 按锁值条件续期并把 FIGHTING/正式 deadline 落进 ctx —— 玩家重新登录时据此重建冻结,
		// 战斗闸才看得到这场战斗(否则新实体没有 InBattleComp,资产通道放行)。
		// event.deadline_ms 为 0 时无从计算 TTL,保持备战期 TTL 不动(最迟随 TTL 过期)
		//
		// 这一步本身**不重试**:Redis 未就绪、读 ctx 的 EVAL 没问到(空应答 / ERROR)、命令没发出去,都只打日志返回。
		// 兜底是 battle 侧的确认周期补发(开局后 180 秒内每 10 秒一次):下一条确认到达时整条路径重跑一遍。
		// 读到 ctx 之后的条件续期(ConfirmBattleLockIfMatch)才有有界重试。代价:Redis 中断超过补发窗口时
		// 锁停在备战期 TTL,离线玩家重登时可能读不到锁、不重建冻结(结算只走收件箱,不丢奖)。
		if (event.deadline_ms() == 0)
		{
			LOG_WARN << "[PlayerBattle] ConfirmBattle: player offline and deadline_ms=0;renewal skipped, player_id="
					 << playerId << " battle_id=" << battleId;
			return;
		}
		if (!RedisReady())
		{
			LOG_WARN << "[PlayerBattle] ConfirmBattle: player offline and Redis disconnected;renewal skipped, player_id="
					 << playerId << " battle_id=" << battleId << "(next periodic confirmation from battle retries it)";
			return;
		}
		const uint64_t deadlineMs = event.deadline_ms();
		const int ret = tlsPlayerRedis.command(
			[playerId, battleId, deadlineMs](hiredis::Hiredis*, redisReply* reply) {
				if (reply == nullptr || reply->type == REDIS_REPLY_ERROR)
				{
					LOG_ERROR << "[PlayerBattle] ConfirmBattle lock/ctx read EVAL failed, player_id=" << playerId
							  << " battle_id=" << battleId << "(next periodic confirmation from battle retries it)";
					return;
				}
				if (reply->type != REDIS_REPLY_ARRAY)
				{
					LOG_WARN << "[PlayerBattle] ConfirmBattle: player offline and lock missing/ownership changed;ignored, player_id="
							 << playerId << " battle_id=" << battleId;
					return;
				}
				// 先读 ctx 再覆写:battle_node_id 只有备战时写的 ctx 有,确认事件不带
				std::string ctxPayload;
				ReplyElementToString(reply, 0, ctxPayload);
				const uint64_t callbackNowMs = TimeSystem::NowMillisecondsUTC();
				InBattleComp ctx = BuildInBattleFromCtx(battleId, ctxPayload,
														ReplyElementToInteger(reply, 1), callbackNowMs);
				ctx.set_state(IN_BATTLE_STATE_FIGHTING);
				ctx.set_deadline_ms(deadlineMs);
				const uint64_t ttlSec = FightingLockTtlSecFor(deadlineMs, callbackNowMs);
				ConfirmBattleLockIfMatch(playerId, battleId, deadlineMs, SerializeBattleCtx(ctx));
				LOG_INFO << "[PlayerBattle] ConfirmBattle: player offline;renew conditionally by lock value and persist ctx, player_id="
						 << playerId << " battle_id=" << battleId << " deadline_ms=" << deadlineMs
						 << " battle_node_id=" << ctx.battle_node_id() << " lock_ttl_sec=" << ttlSec;
			},
			(std::string("EVAL %s 2 ") + kBattleLockKeyFmt + " " + kBattleCtxKeyFmt + " %llu").c_str(),
			kGetCtxIfLockMatchScript, playerId, playerId, battleId);
		if (ret != REDIS_OK)
		{
			LOG_WARN << "[PlayerBattle] ConfirmBattle: player offline and lock/ctx read not sent;renewal skipped, player_id="
					 << playerId << " battle_id=" << battleId << "(next periodic confirmation from battle retries it)";
		}
		return;
	}

	auto* inBattle = tlsEcs.actorRegistry.try_get<InBattleComp>(player);
	if (inBattle == nullptr)
	{
		// 在线但没有 InBattleComp:备战到期被 reaper 摘掉(只摘组件不删锁)后迟到的确认,
		// 或玩家完整下线再登录后(登录重建尚未完成/未命中)才到的确认。
		// 锁值 == battle_id 证明这期间玩家没有被放进第二场,可以安全重建为 FIGHTING;
		// 锁不在则确认已过期(取消/结算/锁 TTL 过期),忽略。
		RebuildBattleFreezeFromLock(player, playerId, battleId, event.deadline_ms(), "late_confirm");
		return;
	}
	if (inBattle->battle_id() != battleId)
	{
		// 玩家已进下一场:迟到的确认必须忽略,否则会把新战斗的期限改坏
		LOG_INFO << "[PlayerBattle] ConfirmBattle: battle_id mismatch;idempotently ignored, player_id=" << playerId
				 << " event_battle_id=" << battleId << " in_battle_id=" << inBattle->battle_id();
		return;
	}
	if (inBattle->state() == IN_BATTLE_STATE_FIGHTING)
	{
		// Kafka at-least-once 重投 / battle 侧周期补发(开局后 180 秒内每 10 秒一次):已 FIGHTING,不再升级,
		// 但顺带再做一次条件续期。首次确认的续期若没落地,锁会在「备战期限 + 60 秒」就自然过期,而战斗时限默认 300 秒;
		// 续期只延长、锁值匹配才动,重复执行无副作用,周期补发因此顺带修好它。
		// 期限与 ctx 取组件上的(首次确认已写定),不信事件 —— 与「已 FIGHTING 不改期限」一致。
		// 上一条确认的续期还在重试时这次让给它(只有续期去重):那条链每次重发都按组件现状重算,发的是同一份。
		ConfirmBattleLockIfMatch(playerId, battleId, inBattle->deadline_ms(), SerializeBattleCtx(*inBattle));
		LOG_DEBUG << "[PlayerBattle] ConfirmBattle: already FIGHTING;state unchanged,lock renewed conditionally, player_id="
				  << playerId << " battle_id=" << battleId;
		return;
	}
	if (inBattle->state() != IN_BATTLE_STATE_PREPARING)
	{
		LOG_INFO << "[PlayerBattle] ConfirmBattle: already in " << eInBattleState_Name(inBattle->state())
				 << ",idempotently ignored, player_id=" << playerId << " battle_id=" << battleId;
		return;
	}

	// PREPARING -> FIGHTING:作废期限切到正式 deadline(event 没填时沿用备战时的 deadline_ms)
	const uint64_t deadlineMs = event.deadline_ms() != 0 ? event.deadline_ms() : inBattle->deadline_ms();
	inBattle->set_state(IN_BATTLE_STATE_FIGHTING);
	inBattle->set_deadline_ms(deadlineMs);

	const uint64_t ttlSec = FightingLockTtlSecFor(deadlineMs, nowMs);
	ConfirmBattleLockIfMatch(playerId, battleId, deadlineMs, SerializeBattleCtx(*inBattle));

	LOG_INFO << "[PlayerBattle] Battle confirmed PREPARING->FIGHTING: player_id=" << playerId
			 << " battle_id=" << battleId
			 << " deadline_ms=" << deadlineMs
			 << " prepare_deadline_ms=" << inBattle->prepare_deadline_ms()
			 << " lock_ttl_sec=" << ttlSec;

	// 备战期间换过会话 -> 房间此刻已建成,补推一次重连提示(turn-based §22.5 第 3 条)。
	// match 的 NotifyBattleAssigned 与 battle 的 NotifyBattleStart 走的是备战快照里的旧会话路由,
	// 新会话收不到;OnPlayerEnterScene 在 PREPARING 时刻意不推(房间未建成,补签会被判 BattleGone),
	// 所以这里是换会话后唯一的恢复入口。判据:
	//   - 无备战会话记录:实体由 RestoreBattleFreezeOnLogin 以 PREPARING 重建(完整重登);
	//   - 记录的会话号 != 当前会话号:大厅断线重连 / REPLACE / 跨 gate 重定向。
	// 当前无会话(已断线、实体仍在)不推:之后的 RECONNECT 进场会因已 FIGHTING 在第 2 步推。
	// 客户端对「同局已有活直连 / 同局补签在途」直接返回,重复提示无害。
	const auto* prepareSession = tlsEcs.actorRegistry.try_get<BattlePrepareSessionComp>(player);
	const uint32_t currentSessionId = CurrentGateSessionId(player);
	const bool sessionChanged =
		prepareSession == nullptr || prepareSession->gateSessionId != currentSessionId;
	if (sessionChanged && currentSessionId != 0)
	{
		LOG_INFO << "[PlayerBattle] Session changed during preparation;send reconnect notification after confirmation: player_id=" << playerId
				 << " battle_id=" << battleId
				 << " prepare_session_id=" << (prepareSession != nullptr ? prepareSession->gateSessionId : 0u)
				 << " current_session_id=" << currentSessionId;
		NotifyBattleReconnectToClient(player);
	}
	// 升级完成即摘:之后的换会话由 OnPlayerEnterScene 按 FIGHTING 推提示
	tlsEcs.actorRegistry.remove<BattlePrepareSessionComp>(player);
}

void PlayerBattleSystem::RebuildBattleFreezeFromLock(entt::entity player, const uint64_t playerId,
													 const uint64_t battleId, const uint64_t deadlineMsHint,
													 const char* reason)
{
	if (!RedisReady())
	{
		LOG_WARN << "[PlayerBattle] Freeze reconstruction skipped(Redis disconnected), player_id=" << playerId
				 << " battle_id=" << battleId << " reason=" << reason;
		return;
	}
	const std::string reasonCopy = reason;
	const int ret = tlsPlayerRedis.command(
		[player, playerId, battleId, deadlineMsHint, reasonCopy](hiredis::Hiredis*, redisReply* reply) {
			if (reply == nullptr || reply->type == REDIS_REPLY_ERROR)
			{
				LOG_ERROR << "[PlayerBattle] Freeze reconstruction lock/ctx read EVAL failed, player_id=" << playerId
						  << " battle_id=" << battleId << " reason=" << reasonCopy;
				return;
			}
			if (reply->type != REDIS_REPLY_ARRAY)
			{
				// 锁不在或已易主:确认已过期(取消/结算/TTL 过期),没有重建资格
				LOG_INFO << "[PlayerBattle] Freeze reconstruction did not match: lock missing or value mismatch, player_id=" << playerId
						 << " battle_id=" << battleId << " reason=" << reasonCopy;
				return;
			}
			// 回调期间实体可能已销毁/复用,或已被新的 PrepareBattle 挂上组件
			if (!tlsEcs.actorRegistry.valid(player) || tlsEcs.GetPlayerId(player) != playerId)
			{
				LOG_INFO << "[PlayerBattle] Freeze reconstruction abandoned: entity no longer exists, player_id=" << playerId
						 << " battle_id=" << battleId << " reason=" << reasonCopy;
				return;
			}
			if (const auto* existing = tlsEcs.actorRegistry.try_get<InBattleComp>(player))
			{
				LOG_INFO << "[PlayerBattle] Freeze reconstruction abandoned: InBattleComp already exists, player_id=" << playerId
						 << " battle_id=" << battleId << " in_battle_id=" << existing->battle_id()
						 << " reason=" << reasonCopy;
				return;
			}
			// 本实体已判作废的局:删锁没落地才会再读到这把锁。不重建,补一次条件删锁。
			if (DecideLockRebuild(player, battleId) == battle_lock::LockRebuildDecision::kSkipExpired)
			{
				LOG_WARN << "[PlayerBattle] metric=battle_freeze_rebuild_skipped_expired player_id=" << playerId
						 << " battle_id=" << battleId << " reason=" << reasonCopy
						 << ",lock still points to a battle this node already expired;not rebuilding,retrying conditional lock deletion";
				// 解冻早已发生(reaper 作废时),锁现在才真正放掉:删掉了就补一次组队跟随(DeleteBattleLockIfMatch 的说明)。
				DeleteBattleLockIfMatch(playerId, battleId, player);
				return;
			}
			std::string ctxPayload;
			ReplyElementToString(reply, 0, ctxPayload);
			const uint64_t nowMs = TimeSystem::NowMillisecondsUTC();
			InBattleComp rebuilt =
				BuildInBattleFromCtx(battleId, ctxPayload, ReplyElementToInteger(reply, 1), nowMs);
			// 确认事件到达 = 房间已建成:无论 ctx 记的是什么状态,一律升级为 FIGHTING
			rebuilt.set_state(IN_BATTLE_STATE_FIGHTING);
			if (deadlineMsHint != 0)
			{
				rebuilt.set_deadline_ms(deadlineMsHint);
			}
			tlsEcs.actorRegistry.emplace<InBattleComp>(player, rebuilt);
			// 重建出冻结与备战同样停步:备战冻结被摘掉之后到确认事件到达之间,他可能已经又走起来了。
			MovementSystem::HaltMotion(player);

			// 续期到冻结最晚到期时刻(正式 deadline + 宽限 + 余量 + 续冻上限)+ 覆写 ctx,同时复核锁仍是本战斗的(否则撤销重建)
			const uint64_t ttlSec = FightingLockTtlSecFor(rebuilt.deadline_ms(), nowMs);
			ConfirmRebuiltFreeze(playerId, battleId, ttlSec, SerializeBattleCtx(rebuilt));

			LOG_WARN << "[PlayerBattle] metric=battle_freeze_rebuilt player_id=" << playerId
					 << " battle_id=" << battleId << " reason=" << reasonCopy
					 << " battle_node_id=" << rebuilt.battle_node_id()
					 << " deadline_ms=" << rebuilt.deadline_ms() << " lock_ttl_sec=" << ttlSec;

			// 重建出的一律是 FIGHTING(房间已建成),而这条路径上 PREPARING->FIGHTING 的升级提示
			// 从未推过(备战冻结已被 reaper 摘掉 / 实体是新建的),无从判断客户端是否知道这一局:
			// 在线就无条件推一次重连提示(turn-based §22.5 第 3 条)。客户端对「同局已有活直连 /
			// 同局补签在途」直接返回,只有直连正在建连时会多一次补签;迟到确认很少发生,可以接受。
			// 若随后 ConfirmRebuiltFreeze 撤销重建(锁已被结算删掉),补签失败 -> 客户端判 BattleGone,
			// 与这一局确实已结束一致。当前无会话时不推:之后的 RECONNECT 进场按 FIGHTING 推。
			if (CurrentGateSessionId(player) != 0)
			{
				NotifyBattleReconnectToClient(player);
			}
		},
		(std::string("EVAL %s 2 ") + kBattleLockKeyFmt + " " + kBattleCtxKeyFmt + " %llu").c_str(),
		kGetCtxIfLockMatchScript, playerId, playerId, battleId);
	if (ret != REDIS_OK)
	{
		LOG_WARN << "[PlayerBattle] Freeze reconstruction lock/ctx read not sent, player_id=" << playerId
				 << " battle_id=" << battleId << " reason=" << reason;
	}
}

void PlayerBattleSystem::RestoreBattleFreezeOnLogin(entt::entity player, const uint64_t playerId)
{
	if (!RedisReady())
	{
		// 读不了锁不等于没有锁:记入登录复查(BattleLoginRecheckComp 的说明)。以前直接 return,
		// 战斗在途时这次登录就不重建冻结,战斗闸看不到这场战斗。
		MarkLoginRecheck(player, playerId, "lock_read_redis_disconnected");
		return;
	}
	const int rc = tlsPlayerRedis.command(
		[player, playerId](hiredis::Hiredis*, redisReply* reply) {
			if (!tlsEcs.actorRegistry.valid(player) || tlsEcs.GetPlayerId(player) != playerId)
			{
				return;
			}
			if (tlsEcs.actorRegistry.any_of<InBattleComp>(player))
			{
				return; // 重连路径组件仍在 / 登录后已备战新局:不动
			}
			switch (battle_lock::ClassifyLoginLockRead(battle_lock::ReplyShapeOf(reply)))
			{
			case battle_lock::LoginLockRead::kRecheck:
				// 空应答:连接断了,锁在不在不知道。以前与 NIL 一起按「无锁」return,战斗闸就此失效。
				MarkLoginRecheck(player, playerId, "lock_read_no_reply");
				return;
			case battle_lock::LoginLockRead::kNoLock:
				// 锁不在 = 没有战斗在途。EVAL 的 ERROR 也按无锁处理:既有取舍,锁本身是咨询性的
				// (代价见 battle_lock_rules.h 的 ClassifyLoginLockRead)。
				return;
			case battle_lock::LoginLockRead::kLock:
				break;
			}
			std::string lockValue;
			if (!ReplyElementToString(reply, 0, lockValue))
			{
				return;
			}
			uint64_t battleId = 0;
			try
			{
				battleId = std::stoull(lockValue);
			}
			catch (const std::exception&)
			{
				LOG_ERROR << "[PlayerBattle] Login reconstruction: invalid lock value, player_id=" << playerId
						  << " lock_value=" << lockValue;
				return;
			}
			if (battleId == 0)
			{
				return;
			}
			// 本实体已判作废的局:删锁没落地才会再读到这把锁(登录挂的复查赶上冻结到期)。不重建,补一次条件删锁。
			// 锁指向的局若其实已结算而还没放锁(上一实体结算提交之后、放锁之前崩溃),这里照常重建:
			// 收件箱重放原结局时 CompleteInboxSettlement 按同一局解冻放锁,结算不再到来时 reaper 按 ctx 的期限收尾。
			if (DecideLockRebuild(player, battleId) == battle_lock::LockRebuildDecision::kSkipExpired)
			{
				LOG_WARN << "[PlayerBattle] metric=battle_freeze_rebuild_skipped_expired player_id=" << playerId
						 << " battle_id=" << battleId << " reason=login"
						 << ",lock still points to a battle this node already expired;not rebuilding,retrying conditional lock deletion";
				// 解冻早已发生(reaper 作废时),锁现在才真正放掉:删掉了就补一次组队跟随(DeleteBattleLockIfMatch 的说明)。
				DeleteBattleLockIfMatch(playerId, battleId, player);
				return;
			}
			std::string ctxPayload;
			ReplyElementToString(reply, 1, ctxPayload);
			const uint64_t nowMs = TimeSystem::NowMillisecondsUTC();
			const InBattleComp rebuilt =
				BuildInBattleFromCtx(battleId, ctxPayload, ReplyElementToInteger(reply, 2), nowMs);
			tlsEcs.actorRegistry.emplace<InBattleComp>(player, rebuilt);
			// 重建出冻结与备战同样停步:登录之后到这条读回来之间,客户端可能已经开始走了。
			MovementSystem::HaltMotion(player);

			// 复核锁仍是本战斗的(登录窗口内恰好确认的结算已删锁 -> 撤销重建);
			// TTL 按重建后冻结最晚的到期时刻算:PREPARING 看 prepare_deadline,
			// FIGHTING 看 deadline + 终局提交宽限 + 收件箱投递余量 + 续冻上限(FreezeHoldDeadlineMs)
			ConfirmRebuiltFreeze(playerId, battleId, LockTtlSecOf(rebuilt, nowMs), SerializeBattleCtx(rebuilt));

			LOG_WARN << "[PlayerBattle] metric=battle_freeze_rebuilt player_id=" << playerId
					 << " battle_id=" << battleId << " reason=login"
					 << " state=" << eInBattleState_Name(rebuilt.state())
					 << " battle_node_id=" << rebuilt.battle_node_id()
					 << " deadline_ms=" << rebuilt.deadline_ms()
					 << " prepare_deadline_ms=" << rebuilt.prepare_deadline_ms();

			// 战斗中(房间已建成)才提示客户端补签重连;PREPARING 由随后的确认/取消/reaper 处理 ——
			// 重建实体不挂 BattlePrepareSessionComp,确认到达升级 FIGHTING 时 ConfirmBattle 据此补推提示。
			// 不再要求 battle_node_id 已知(turn-based §22 D72):客户端凭 battle_id 经
			// MatchService.RequestBattleTicket 补签,用不到节点号;ctx 缺失的降级重建同样要提示。
			if (rebuilt.state() == IN_BATTLE_STATE_FIGHTING)
			{
				NotifyBattleReconnectToClient(player);
			}
		},
		(std::string("EVAL %s 2 ") + kBattleLockKeyFmt + " " + kBattleCtxKeyFmt).c_str(),
		kGetLockAndCtxScript, playerId, playerId);
	if (rc != REDIS_OK)
	{
		// 命令没发出去时回调永远不会来:与 Redis 不可用同样记入复查。
		MarkLoginRecheck(player, playerId, "lock_read_not_sent");
	}
}

namespace {
// 完整终局来源里的未知嵌套字段不能被旧执行器忽略后记成整笔APPLIED。
bool HasNestedUnknownFields(const google::protobuf::Message& message)
{
    const auto* reflection = message.GetReflection();
    if (!reflection->GetUnknownFields(message).empty()) return true;
    std::vector<const google::protobuf::FieldDescriptor*> fields;
    reflection->ListFields(message, &fields);
    for (const auto* field : fields) {
        if (field->cpp_type() != google::protobuf::FieldDescriptor::CPPTYPE_MESSAGE) continue;
        if (field->is_repeated()) {
            for (int i = 0; i < reflection->FieldSize(message, field); ++i)
                if (HasNestedUnknownFields(reflection->GetRepeatedMessage(message, field, i))) return true;
        } else if (HasNestedUnknownFields(reflection->GetMessage(message, field))) return true;
    }
    return false;
}

bool CollectExactSettlementItems(const ::BattleSettlementData& settlement,
                                ItemCountMap& consumed, ItemCountMap& gained)
{
    const auto collect = [](const auto& entries, ItemCountMap& target) {
        for (const auto& item : entries) {
            if (item.item_table_id() == 0 || item.count() == 0 ||
                item.count() > std::numeric_limits<uint32_t>::max()) return false;
            auto& total = target[item.item_table_id()];
            if (item.count() > std::numeric_limits<uint32_t>::max() - total) return false;
            total += static_cast<uint32_t>(item.count());
        }
        return true;
    };
    // 可战斗使用性已在原开局快照审定；终局不按热更后的标记重判这份已提交消耗事实。
    return collect(settlement.items_consumed(), consumed) && collect(settlement.items_gained(), gained);
}

// 本节点 Item 表查不到的最小配置号;都查得到返回 0(配置号 0 已被 CollectExactSettlementItems 判为坏载荷,到不了这里)。
// 取最小的只为日志确定。查不到意味着什么、为什么整笔 DEFER,见 ApplySettlementToEntity 里的调用处。
uint32_t SmallestItemConfigUnknownToThisNode(const ItemCountMap& items)
{
    uint32_t smallest = 0;
    for (const auto& entry : items)
    {
        const uint32_t configId = entry.first;
        if (ItemTableManager::Instance().Exists(configId)) continue;
        if (smallest == 0 || configId < smallest) smallest = configId;
    }
    return smallest;
}

// 掉落里此刻被获得封禁的项(全服或本人:GainBlockService::IsItemGainBlocked,与各入包入口、资产通道同一个判定)。
// 封禁是终局业务决定:这一项从整笔候选里剔除,这一局不发、解封后也不补发,其余效果照常应用;调用方在应用成功后
// 逐项记 WARN(metric=battle_settlement_item_blocked)。不剔除的话 BagService 会整批回 kAssetBlocked,整笔 DEFER
// 让这一局在封禁期间永远到不了 APPLIED、解封后又整笔补发。BagService 的「任一项被封整批拒绝」契约不变,
// 剔除只在本调用方做。只看掉落:消耗是扣减,不受获得封禁约束。按配置号升序处理,日志顺序确定。
std::vector<std::pair<uint32_t, uint32_t>> DropBlockedGains(entt::entity player, ItemCountMap& gained)
{
    std::vector<std::pair<uint32_t, uint32_t>> blocked;
    for (const auto& [configId, count] : SortedByConfigId(gained))
    {
        if (!GainBlockService::IsItemGainBlocked(player, configId)) continue;
        blocked.emplace_back(configId, count);
        gained.erase(configId);
    }
    return blocked;
}

using SettlementCurrencyGains = std::map<CurrencyType, uint64_t>;

// 结算币种的校验结论。不认识的币种单列一类:battle 节点的奖励档按同一张取值表(proto/common/base/currency_type.proto)
// 校验,本二进制不认识的值只可能来自更新版本的 battle 节点,是版本错位;它在会话内永远不会被认识,收件箱回 kVersionSkew
// 而不是 kDefer(本地续做只会空转,混版本持久字段保护设计 §12)。金额越界、重复币种在任何版本上都是坏载荷。
enum class SettlementCurrencyVerdict : uint8_t
{
    kValid,
    kUnknownCurrency,
    kMalformed,
};

// 新集合优先；只有集合为空才解释旧金币字段。全笔币种/金额/去重校验不产生副作用。
// 与版本无关的坏载荷优先于版本错位:整笔判完才看有没有不认识的币种。gains 只收本版本认识的币种。
SettlementCurrencyVerdict CollectSettlementCurrencyGains(const ::BattleSettlementData& settlement, SettlementCurrencyGains& gains)
{
    if (settlement.currency_gains_size() > 0) {
        std::set<uint32_t> seenTypes;
        bool unknownCurrency = false;
        for (const auto& gain : settlement.currency_gains()) {
            if (gain.amount() > static_cast<uint64_t>(std::numeric_limits<int64_t>::max()) ||
                !seenTypes.insert(gain.currency_type()).second) return SettlementCurrencyVerdict::kMalformed;
            if (!IsKnownCurrencyType(gain.currency_type())) {
                unknownCurrency = true;
                continue;
            }
            gains.emplace(static_cast<CurrencyType>(gain.currency_type()), gain.amount());
        }
        if (unknownCurrency) return SettlementCurrencyVerdict::kUnknownCurrency;
    } else if (settlement.gold_gain() > 0) {
        if (settlement.gold_gain() > static_cast<uint64_t>(std::numeric_limits<int64_t>::max()))
            return SettlementCurrencyVerdict::kMalformed;
        gains.emplace(kCurrencyGold, settlement.gold_gain());
    }
    return SettlementCurrencyVerdict::kValid;
}

// 这一币种的获得此刻是否被封禁:全服 GainBlock 或本人 GM 封禁,与 CurrencySystem::AddCurrency 回 kAssetBlocked 的
// 条件相同(货币入口的标准判法,见 gain_block_service.h)。预检(CanCreditWholeBattleCurrency)与入账(creditCurrencies)
// 共用这一个判定,两处对同一币种不会给出不同结论。
bool IsSettlementCurrencyGainBlocked(entt::entity player, CurrencyType type)
{
    return GainBlockService::IsGloballyBlocked(GainBlockService::GainType::kCurrency, static_cast<uint32_t>(type)) ||
           CurrencySystem::IsCurrencyBlocked(player, type);
}

// Inbox整批提交前按每个币种复核净入账额；缺组件、金额越界、未知债务与余额溢出继续保留原件，不部分提交。
// 被封禁的币种回 true,意思是「这一项将被跳过」,不是「可以入账」:封禁是终局业务决定,这一币种这一局整笔不入账、
// 也不抵欠款,解封后不补发(与资产通道 Credit 的 REJECTED、已删除的旧结算路径同一口径);跳过发生在 creditCurrencies。
// 若按「不能入账」整笔 DEFER,封禁期间这一局永远到不了 APPLIED(Match 的 ready 票一直占着),解封后又整笔补发,违背封禁本意。
// 封禁排在组件与金额校验之后、欠款与溢出复核之前:不入账的币种不必复核它的欠款与余额。
bool CanCreditWholeBattleCurrency(entt::entity player, CurrencyType type, uint64_t amount)
{
    if (amount == 0) return true;
    const auto* balances = tlsEcs.actorRegistry.try_get<CurrencyComp>(player);
    const auto* currency = tlsEcs.actorRegistry.try_get<PlayerCurrencyComp>(player);
    if (!balances || !currency || amount > static_cast<uint64_t>(std::numeric_limits<int64_t>::max())) return false;
    if (IsSettlementCurrencyGainBlocked(player, type)) return true;
    uint64_t net = amount;
    if (const auto it = currency->debts.find(static_cast<uint32_t>(type)); it != currency->debts.end()) {
        const auto& debt = it->second;
        // 只认更新版本加的欠款字段，已退役字段的残留字节不算版本错位。
        if (CurrencySystem::DebtCarriesUnknownFields(player, type)) return false;
        if (!debt.frozen && (debt.expiresAt == 0 || TimeSystem::NowSecondsUTC() < debt.expiresAt))
            net -= std::min(net, debt.Remaining());
    }
    const auto index = static_cast<int>(type);
    const uint64_t balance = index < balances->values_size() ? balances->values(index) : 0;
    return net <= std::numeric_limits<uint64_t>::max() - balance;
}

// 数量副本没有GUID；只从当前完整实例候选中扣已知普通单位，未知schema/实例不能当普通数量。
bool HasUnsupportedInboxConsumption(entt::entity player, const ::BattleSettlementData& settlement)
{
    std::set<uint32_t> configs;
    for (const auto& entry : settlement.items_consumed()) {
        const auto* row = ItemTableManager::Instance().FindByIdSilent(entry.item_table_id()).first;
        // 本节点表里查不到这一行不在这里判:Item 表行 id 只增不删(cfg_ids_permanent),查不到只可能是这一行还没热更到
        // 本节点,或结算来自更新版本的 battle 节点,热更到之后本地续做即可成功,所以由 ApplySettlementToEntity 整笔 DEFER,
        // 不停用整个域。行还在却带本版本不认识的字段,热更不会让它消失,才是版本错位。
        if (!row) continue;
        if (HasNestedUnknownFields(*row)) return true;
        configs.insert(entry.item_table_id());
    }
    const auto* bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player);
    if (!bags) return false; // 缺持久组件仍由整笔加载闸拒绝，不能解释为零库存。
    bool unsupported = false;
    bags->bags[kInventory].ForEachItem([&](Guid guid, const ItemComp& item) {
        if (!configs.contains(item.config_id())) return;
        if (guid == 0 || item.item_id() != guid || item.size() == 0 || !ItemStore::IsCountableByConfig(item)
            || HasNestedUnknownFields(item)) unsupported = true;
    });
    return unsupported;
}

// 结算里的宝宝效果哪些可以应用。false = 整笔 DEFER(缺持久组件、带未知字段、坏数据、宝宝种类在本节点 Pet 表里查不到);
// skipped = 按规则只跳过这一只、其余照常的原因计数(宝宝已不在身上、宝宝属性池缺配)。
bool CollectApplicableInboxPets(entt::entity player, const ::BattleSettlementData& settlement,
                               std::set<uint64_t>& applicable, std::map<std::string, uint32_t>& skipped)
{
    if (settlement.pets_size() == 0) return true;
    const auto* owned = tlsEcs.actorRegistry.try_get<PlayerPetComp>(player);
    if (!owned || HasNestedUnknownFields(*owned)) return false;
    std::set<uint64_t> ownedIds, effects;
    for (const auto& pet : owned->pets())
        if (pet.pet_id() == 0 || !ownedIds.insert(pet.pet_id()).second) return false;
    bool hasPool = false, unknownPool = false;
    for (const auto& pool : AttributePoolTableManager::Instance().FindAll().data())
        if (pool.owner_type() == PetSystem::kPoolOwnerPet) {
            hasPool = true; unknownPool = HasNestedUnknownFields(pool); break;
        }
    for (const auto& effect : settlement.pets()) {
        if (effect.pet_id() == 0 || !effects.insert(effect.pet_id()).second) return false;
        const auto pet = std::find_if(owned->pets().begin(), owned->pets().end(), [&effect](const auto& entry) {
            return entry.pet_id() == effect.pet_id();
        });
        if (pet == owned->pets().end()) { ++skipped["pet_no_longer_owned"]; continue; }
        const auto* row = PetTableManager::Instance().FindByIdSilent(pet->pet_table_id()).first;
        if (!row) {
            // 玩家还带着这只宝宝,它的种类在本节点 Pet 表里查不到:整笔 DEFER,不跳过这一只。Pet 表与 Item 表同一条规则
            // (行 id 只增不删,cfg_ids_permanent),查不到只可能是这一行还没热更到本节点,或宝宝来自更新版本;跳过就永久丢掉
            // 它的战后终值,热更到之后本地续做即可成功(与 ApplySettlementToEntity 里物品查不到表同一口径)。
            LOG_WARN << "[PlayerBattle] metric=persist_version_skew where=battle_pet player_id=" << tlsEcs.GetPlayerId(player)
                     << " battle_id=" << settlement.battle_id() << " pet_id=" << effect.pet_id()
                     << " pet_table_id=" << pet->pet_table_id()
                     << "; pet config is not in this node's pet table (row not hot-updated here yet, or pet from a newer"
                        " version), deferring the whole settlement without side effects";
            return false;
        }
        if (HasNestedUnknownFields(*row)) return false;
        if (!hasPool) { ++skipped["pet_pool_missing"]; continue; }
        if (unknownPool) return false;
        applicable.insert(effect.pet_id());
    }
    return true;
}

bool HasCompleteBattleEffectInputs(entt::entity player, const ::BattleSettlementData& settlement,
                                  const SettlementCurrencyGains& currencyGains)
{
    if (!tlsEcs.actorRegistry.all_of<BaseAttributesComp, DerivedAttributesComp, CurrencyComp, PlayerCurrencyComp>(player))
        return false;
    if (const auto* current = tlsEcs.actorRegistry.try_get<InBattleComp>(player);
        current != nullptr && current->battle_id() != settlement.battle_id()) return false;
    const bool kills = std::any_of(settlement.defeated_monsters().begin(), settlement.defeated_monsters().end(),
        [](const auto& d) { return d.monster_config_id() != 0 && d.count() > 0; });
    if (kills) {
        const auto* missions = tlsEcs.actorRegistry.try_get<MissionsContainerComp>(player);
        if (!missions || !missions->Get(MissionListComp::kPlayerMission)) return false;
    }
    std::set<uint64_t> applicable;
    std::map<std::string, uint32_t> skipped;
    if (!CollectApplicableInboxPets(player, settlement, applicable, skipped)) return false;
    return std::all_of(currencyGains.begin(), currencyGains.end(), [player](const auto& gain) {
        return CanCreditWholeBattleCurrency(player, gain.first, gain.second);
    });
}
} // namespace

namespace {
// ── 本人近期战斗结果(docs/design/battle-result-recovery.md §4、§5)──────────────────────────────
//
// 已写进近期结果、但这份存档还没确认提交的局(按需状态,只在本文件内):
//  - 创建:RecordBattleResult(ApplySettlementToEntity 写结果的同一帧)延迟创建,唯一创建入口;
//  - 摘除:PublishBattleResult(CompleteInboxSettlement,同 E/q 双 SQL 确认之后)摘掉对应 battle_id,空了就摘组件;
//    结果被淘汰出列表时也一并摘掉;
//  - 读者:查询与确认只读它,列在这里的局不对客户端公开 —— 与 BattleEndS2C 同一个公开时机。
// 不落库:实体重建后从盘上加载回来的结果都已提交。
struct BattleResultAwaitingCommitComp
{
    std::vector<uint64_t> battleIds;
};

// 本实体还没公开的局;没有组件 = 没有。none 由调用方提供(一个空 vector),返回的引用不会悬空。
const std::vector<uint64_t>& AwaitingCommitBattleIds(entt::entity player, const std::vector<uint64_t>& none)
{
    const auto* awaiting = tlsEcs.actorRegistry.try_get<BattleResultAwaitingCommitComp>(player);
    return awaiting != nullptr ? awaiting->battleIds : none;
}

std::vector<std::pair<uint32_t, uint32_t>> ItemLinesOf(const ItemCountMap& items)
{
    return std::vector<std::pair<uint32_t, uint32_t>>(items.begin(), items.end());
}

// 这一局本人实际入账的结果。货币取实际入账的金额(先抵欠款的部分也算在内;被封禁的币种已从 currencyGains 摘掉,不记);
// 掉落取剔除了被封禁项之后的清单(granted);消耗取 min(要求, 实际持有)(consumed)。经验系统没接入,不记经验。
PlayerBattleResult BuildBattleResult(const ::BattleSettlementData& settlement, const SettlementCurrencyGains& currencyGains,
                                     const ItemCountMap& granted, const ItemCountMap& consumed, const uint64_t settledAtMs)
{
    PlayerBattleResult entry;
    entry.set_battle_id(settlement.battle_id());
    entry.set_outcome(static_cast<uint32_t>(settlement.outcome()));
    entry.set_team_index(settlement.player_team_index());
    entry.set_is_dead(settlement.is_dead());
    entry.set_fled(settlement.fled());
    entry.set_total_rounds(settlement.total_rounds());
    entry.set_finished_at_ms(settlement.finished_at_ms());
    entry.set_match_mode(settlement.match_mode());
    entry.set_battle_config_id(settlement.battle_config_id());
    entry.set_settled_at_ms(settledAtMs);
    for (const auto& [type, amount] : currencyGains) // std::map:按币种升序
    {
        if (amount == 0) continue;
        auto* line = entry.add_currencies();
        line->set_currency_type(static_cast<uint32_t>(type));
        line->set_amount(amount);
    }
    const bool gainedTruncated = battle_result::FillItemLines(ItemLinesOf(granted), *entry.mutable_items_gained());
    const bool consumedTruncated = battle_result::FillItemLines(ItemLinesOf(consumed), *entry.mutable_items_consumed());
    entry.set_lines_truncated(gainedTruncated || consumedTruncated);
    return entry;
}

// 把这一局的本人结果写进近期结果。ApplySettlementToEntity 全部效果写完之后、同一帧调用,随同一次存档提交;
// 效果之前已确认 PlayerBattleResultComp 在场,这之后没有失败分支,所以这里只记日志、不回失败。
// itemsMailed:granted 这批掉落两个包都放不下、整批改投了邮件(battle-overflow-mail.md);照常列进 items_gained。
void RecordBattleResult(entt::entity player, const uint64_t playerId, const ::BattleSettlementData& settlement,
                        const SettlementCurrencyGains& currencyGains, const ItemCountMap& granted, const ItemCountMap& consumed,
                        const bool itemsMailed)
{
    const uint64_t battleId = settlement.battle_id();
    auto* history = tlsEcs.actorRegistry.try_get<PlayerBattleResultComp>(player);
    if (history == nullptr)
    {
        // 只可能是缺陷:效果之前核对过它在场,效果路径不摘持久组件。奖励已入账、不能回头;这一局重登后看不到结果。
        LOG_ERROR << "[PlayerBattle] metric=battle_result_record_missing player_id=" << playerId << " battle_id=" << battleId
                  << ";persisted PlayerBattleResultComp vanished after the preflight, result not recorded (rewards already applied)";
        return;
    }
    auto entry = BuildBattleResult(settlement, currencyGains, granted, consumed, TimeSystem::NowMillisecondsUTC());
    entry.set_items_mailed(itemsMailed);
    const bool truncated = entry.lines_truncated();
    const int currencyLines = entry.currencies_size();
    const int gainedLines = entry.items_gained_size();
    const int consumedLines = entry.items_consumed_size();
    std::vector<uint64_t> evicted;
    switch (battle_result::Record(*history, std::move(entry), evicted))
    {
    case battle_result::RecordOutcome::kRecorded:
        break;
    case battle_result::RecordOutcome::kDuplicateBattle:
        // 同一局的结算按收件箱账本只执行一次,走到这里只可能是上游缺陷:保留第一条,不写第二条。
        LOG_WARN << "[PlayerBattle] metric=battle_result_duplicate player_id=" << playerId << " battle_id=" << battleId
                 << ";this battle already has a result record, keeping the first one";
        return;
    case battle_result::RecordOutcome::kInvalidBattle:
    case battle_result::RecordOutcome::kCount:
        // battle_id 为 0 的结算在 ApplySettlementToEntity 入口就被拒了,这里只为穷举。
        return;
    }
    // 此处延迟创建:这一局的结果在存档确认提交之前不公开(PublishBattleResult 摘)。
    auto& awaiting = tlsEcs.actorRegistry.get_or_emplace<BattleResultAwaitingCommitComp>(player).battleIds;
    // 被淘汰出列表的局不再有结果可公开。
    awaiting.erase(std::remove_if(awaiting.begin(), awaiting.end(),
                                  [&evicted](const uint64_t id) { return battle_result::ContainsBattle(evicted, id); }),
                   awaiting.end());
    awaiting.push_back(battleId);
    if (truncated)
    {
        LOG_WARN << "[PlayerBattle] metric=battle_result_lines_truncated player_id=" << playerId << " battle_id=" << battleId
                 << " max_lines=" << battle_result::kMaxItemLinesPerList
                 << ";item lines above the per-list limit are left out of the result record (rewards unaffected)";
    }
    LOG_INFO << "[PlayerBattle] metric=battle_result_recorded player_id=" << playerId << " battle_id=" << battleId
             << " result_seq=" << history->last_result_seq() << " currencies=" << currencyLines
             << " items_gained=" << gainedLines << " items_consumed=" << consumedLines
             << " truncated=" << (truncated ? 1 : 0) << " evicted=" << evicted.size();
}

// 这一局的存档已确认提交(CompleteInboxSettlement 调用):结果从此对客户端公开。
// 没在等的局什么都不做:收件箱回放原结局、实体重建后加载回来的结果本就已公开。
void PublishBattleResult(entt::entity player, const uint64_t battleId)
{
    auto* awaiting = tlsEcs.actorRegistry.try_get<BattleResultAwaitingCommitComp>(player);
    if (awaiting == nullptr) return;
    auto& ids = awaiting->battleIds;
    const auto it = std::find(ids.begin(), ids.end(), battleId);
    if (it == ids.end()) return;
    ids.erase(it);
    if (ids.empty()) tlsEcs.actorRegistry.remove<BattleResultAwaitingCommitComp>(player);
    LOG_INFO << "[PlayerBattle] metric=battle_result_published player_id=" << tlsEcs.GetPlayerId(player)
             << " battle_id=" << battleId;
}

// ── 作废结算(docs/design/battle-instance-loss-early-abort.md §5.2)──────────────────────────────
// Match 为这一局写了唯一的 ABORT(执行进程失效、截止作废、帮会取消、人工处置等),在同一事务里给每名成员投一份
// outcome = BATTLE_OUTCOME_VOID、效果字段全空的结算(go/match/internal/authority/void_settlement.go)。

// 作废结算只许带身份、作废结局与只供展示的字段;出现名单之外的字段(现有的效果字段,或以后新加的)就不是作废结算,
// 是生产方缺陷,不当作废也不当结算。按「只许出现哪些」判而不是逐个列效果字段:以后给结算加新效果时,带着它的作废正文默认被拒。
// 名单与 Match 写作废结算的字段(voidSettlementFor)、收件箱服务的 voidSettlementFields 是同一份。
bool IsEffectFreeVoidSettlement(const ::BattleSettlementData& settlement)
{
    std::vector<const google::protobuf::FieldDescriptor*> present;
    settlement.GetReflection()->ListFields(settlement, &present);
    for (const auto* field : present)
    {
        switch (field->number())
        {
        case ::BattleSettlementData::kBattleIdFieldNumber:
        case ::BattleSettlementData::kPlayerIdFieldNumber:
        case ::BattleSettlementData::kOutcomeFieldNumber:
        case ::BattleSettlementData::kPlayerTeamIndexFieldNumber:
        case ::BattleSettlementData::kFinishedAtMsFieldNumber:
        case ::BattleSettlementData::kMatchModeFieldNumber:
        case ::BattleSettlementData::kBattleConfigIdFieldNumber:
            continue;
        default:
            return false;
        }
    }
    return true;
}

// 应用一份作废结算。绝不进 ApplySettlementToEntity:那里把 health / mana 当战后终值回写,作废正文里的 0 会把血量写成 0
// (阵亡复活还会回满,等于白送一次回复)。这里只做两件事,随收件箱账本同一次存档提交:
//   1. 释放这一局的道具预留:不会再有结算从预留里扣(battle-item-reservation.md);
//   2. 写一条 VOID 近期结果,存档确认之后才对客户端公开。
// 解冻、条件删锁、推 BattleEndS2C{VOID} 仍在 CompleteInboxSettlement(同 E/q 双 SQL 确认之后),与普通结算同一个公开点。
// 正文不合格(带效果、身份不符)按坏载荷处理:整笔 DEFER 并打 ERROR,原件留在收件箱交人工(与 kMalformed 货币同口径);
// 这名玩家的冻结仍由 reaper 按 SQL 终局(ABORT)兜底解开。
PlayerInboxExecResult ApplyVoidSettlement(entt::entity player, const ::BattleSettlementData& settlement)
{
    const uint64_t playerId = tlsEcs.GetPlayerId(player);
    const uint64_t battleId = settlement.battle_id();
    if (battleId == 0 || settlement.player_id() != playerId || !IsEffectFreeVoidSettlement(settlement))
    {
        LOG_ERROR << "[PlayerBattle] metric=battle_void_settlement_malformed player_id=" << playerId << " battle_id=" << battleId
                  << " settlement_player_id=" << settlement.player_id()
                  << ";void settlement carries effects or a foreign identity, deferred for manual handling";
        return {PlayerInboxExecKind::kDefer, 0};
    }
    // 近期结果是持久数据,只由加载创建;缺了是加载不变量被破坏。结果写不进去就整笔延后,与普通结算的前置核对同一口径。
    if (!tlsEcs.actorRegistry.all_of<PlayerBattleResultComp>(player))
    {
        LOG_ERROR << "[PlayerBattle] metric=battle_result_history_missing player_id=" << playerId << " battle_id=" << battleId
                  << " op=void_settlement;persisted PlayerBattleResultComp missing (load invariant broken), void settlement deferred";
        return {PlayerInboxExecKind::kDefer, 0};
    }
    ReleaseBattleItemHold(player, playerId, battleId, "void_settlement");
    RecordBattleResult(player, playerId, settlement, SettlementCurrencyGains{}, ItemCountMap{}, ItemCountMap{},
                       /*itemsMailed=*/false);
    LOG_INFO << "[PlayerBattle] metric=battle_void_settlement_applied player_id=" << playerId << " battle_id=" << battleId
             << ";match aborted this battle, no effects applied; freeze is released after the save commits";
    return {PlayerInboxExecKind::kApplied, 0};
}
} // namespace

PlayerInboxExecResult PlayerBattleSystem::ApplyInboxSettlement(entt::entity player, const std::string& payload)
{
    ::BattleSettlementData settlement;
    // 载荷上限只读类型规格(proto 的 max_payload_bytes 是唯一事实源,C-3);读不到(0)按上限未知拒执行,fail-closed。
    const uint32_t maxPayloadBytes = player_inbox_registry::MaxPayloadBytes(
        static_cast<uint32_t>(player_inbox::PLAYER_INBOX_MSG_BATTLE_SETTLEMENT));
    if (maxPayloadBytes == 0 || payload.size() > maxPayloadBytes || !settlement.ParseFromString(payload) ||
        HasNestedUnknownFields(settlement) ||
        !::eBattleOutcome_IsValid(settlement.outcome()) || settlement.outcome() == ::BATTLE_OUTCOME_ONGOING)
        return {PlayerInboxExecKind::kVersionSkew, 0};
    if (!tlsEcs.actorRegistry.valid(player)) return {PlayerInboxExecKind::kDefer, 0};
    // 作废走自己的分支,不进下面任何效果路径(ApplyVoidSettlement 的说明)。
    if (settlement.outcome() == ::BATTLE_OUTCOME_VOID) return ApplyVoidSettlement(player, settlement);
    if (HasUnsupportedInboxConsumption(player, settlement)) return {PlayerInboxExecKind::kVersionSkew, 0};
    SettlementCurrencyGains currencyGains;
    switch (CollectSettlementCurrencyGains(settlement, currencyGains)) {
    case SettlementCurrencyVerdict::kValid:
        break;
    case SettlementCurrencyVerdict::kUnknownCurrency:
        LOG_WARN << "[PlayerBattle] metric=persist_version_skew where=battle_currency player_id=" << tlsEcs.GetPlayerId(player)
                 << " battle_id=" << settlement.battle_id()
                 << "; settlement carries a currency unknown to this version, halting the battle settlement domain";
        return {PlayerInboxExecKind::kVersionSkew, 0};
    case SettlementCurrencyVerdict::kMalformed:
        // 坏载荷在任何版本上都不会成功;战斗结算域不收 REJECTED(Ack 只认 APPLIED),原件留着交人工(runbook「Battle 域条目卡住」)。
        return {PlayerInboxExecKind::kDefer, 0};
    }
    for (const auto& [type, amount] : currencyGains)
        if (amount > 0 && CurrencySystem::DebtCarriesUnknownFields(player, type))
            return {PlayerInboxExecKind::kVersionSkew, 0};
    // 两包都放不下而延后时把 kBagItemNotStacked 当作 DEFER 的原因带回(2026-11-06,bag-rule-policy-layering.md §6.6):
    // 收件箱记在本域上;人物背包或临时格腾出位置之后(BagService 的「腾位通知」:出包、扣消耗、整理合堆、取回临时格、
    // 穿到空槽,2026-11-12 起不只取回),PlayerInboxSystem::OnBagSpaceFreed 只提前这一类延后。判据是这一次调用里
    // 「连续放不下」的计数涨了 —— 只有 ConsumeInventoryAndGrant 两包都放不下、改投邮件也接不住(待寄队列满、请求号暂缺;
    // docs/design/battle-overflow-mail.md)那一支会加它。其余延后原因不带,照常退避。
    const uint32_t overflowBefore = BagService::ConsecutiveGrantOverflowRejections(player);
    if (!ApplySettlementToEntity(player, settlement)) {
        const bool bagsFull = BagService::ConsecutiveGrantOverflowRejections(player) > overflowBefore;
        return {PlayerInboxExecKind::kDefer, bagsFull ? static_cast<uint32_t>(kBagItemNotStacked) : 0u};
    }
    return {PlayerInboxExecKind::kApplied, 0};
}

bool PlayerBattleSystem::CompleteInboxSettlement(entt::entity player, const std::string& payload)
{
    ::BattleSettlementData settlement;
    if (!tlsEcs.actorRegistry.valid(player) || !settlement.ParseFromString(payload) ||
        HasNestedUnknownFields(settlement) || settlement.player_id() != tlsEcs.GetPlayerId(player) ||
        settlement.battle_id() == 0) return false;
    // SQL确认之后才公开终局/解冻/放锁；旧局不能修改已经开始的新局HP或摘新局组件。
    // 近期战斗结果同一个时机公开,而且放在下面的同局判断之外:玩家此刻若已在下一局里,终局通知不推,结果照样公开。
    // 先于 BattleEndS2C:客户端收到终局通知就可能立刻来查(docs/design/battle-result-recovery.md §5、§7)。
    PublishBattleResult(player, settlement.battle_id());
    const auto* current = tlsEcs.actorRegistry.try_get<InBattleComp>(player);
    if (current == nullptr || current->battle_id() == settlement.battle_id()) {
        // 先发条件删锁再摘组件:摘组件会触发组队跟随链,它读 battle:lock fail-closed,两条命令同 slot,路由层停等。
        // 组件已不在(reaper 已判作废 / 实体是新建的):解冻早已发生,锁现在才真正放掉,删掉了要补一次跟随。
        entt::entity refollow = entt::null;
        if (current == nullptr) refollow = player;
        DeleteBattleLockIfMatch(settlement.player_id(), settlement.battle_id(), refollow);
        if (current != nullptr) RemoveInBattleComp(player);
        PushBattleEndToPlayer(player, settlement);
    }
    return true;
}

uint32_t PlayerBattleSystem::GetRecentBattleResults(entt::entity player, ::GetRecentBattleResultsResponse& response)
{
    if (!tlsEcs.actorRegistry.valid(player)) return kEntityIsNull;
    const uint64_t playerId = tlsEcs.GetPlayerId(player);
    const auto* history = tlsEcs.actorRegistry.try_get<PlayerBattleResultComp>(player);
    if (history == nullptr)
    {
        // 持久组件只由加载创建;缺了是加载不变量被破坏,不补空列表冒充「没有结果」(ECS 组件访问规则)。
        LOG_ERROR << "[PlayerBattle] metric=battle_result_history_missing player_id=" << playerId << " op=query"
                  << ";persisted PlayerBattleResultComp missing (load invariant broken)";
        return kServiceUnavailable;
    }
    const std::vector<uint64_t> none;
    const auto& hidden = AwaitingCommitBattleIds(player, none);
    uint32_t unread = 0;
    for (const auto* entry : battle_result::VisibleNewestFirst(*history, hidden))
    {
        *response.add_results() = *entry;
        if (!entry->seen()) ++unread;
    }
    LOG_INFO << "[PlayerBattle] Recent battle results queried: player_id=" << playerId
             << " returned=" << response.results_size() << " unread=" << unread
             << " awaiting_commit=" << hidden.size();
    return kSuccess;
}

uint32_t PlayerBattleSystem::AckBattleResults(entt::entity player, const ::AckBattleResultsRequest& request)
{
    if (!tlsEcs.actorRegistry.valid(player)) return kEntityIsNull;
    const uint64_t playerId = tlsEcs.GetPlayerId(player);
    if (static_cast<std::size_t>(request.battle_ids_size()) > battle_result::kMaxEntries)
    {
        return kArraySizeTooLargeInMessage;
    }
    // 冻结 / 交接 / 退出中 / 没有归属围栏:本节点此刻不改这名玩家的存档(player_mutability.h 的唯一判定)。
    // 已读标记不是资产,拒掉的代价只是客户端下次落地再补发一次确认。
    if (const auto mutability = player_mutability::ClassifyPlayerMutability(player);
        mutability != player_mutability::PlayerMutability::kMutable)
    {
        LOG_INFO << "[PlayerBattle] Battle result acknowledgement rejected: player_id=" << playerId
                 << " reason=" << player_mutability::PlayerMutabilityName(mutability);
        return kFeatureUnavailable;
    }
    auto* history = tlsEcs.actorRegistry.try_get<PlayerBattleResultComp>(player);
    if (history == nullptr)
    {
        LOG_ERROR << "[PlayerBattle] metric=battle_result_history_missing player_id=" << playerId << " op=ack"
                  << ";persisted PlayerBattleResultComp missing (load invariant broken)";
        return kServiceUnavailable;
    }
    const std::vector<uint64_t> none;
    const uint32_t newlySeen =
        battle_result::MarkSeen(*history, request.battle_ids(), AwaitingCommitBattleIds(player, none));
    // 不触发存盘:已读标记随下一次存盘(周期存盘或下一笔结算)落地,丢了最多让客户端再弹一次。
    LOG_INFO << "[PlayerBattle] Battle results acknowledged: player_id=" << playerId
             << " requested=" << request.battle_ids_size() << " newly_seen=" << newlySeen;
    return kSuccess;
}

bool PlayerBattleSystem::ApplySettlementToEntity(entt::entity player, const ::BattleSettlementData& settlement)
{
    if (!tlsEcs.actorRegistry.valid(player)) return false;
    const uint64_t playerId = tlsEcs.GetPlayerId(player);
    const uint64_t battleId = settlement.battle_id();
    if (playerId == 0 || playerId != settlement.player_id() || battleId == 0)
    {
        LOG_ERROR << "[PlayerBattle] Settlement application failed: invalid entity or settlement ownership, player_id=" << playerId
                  << " battle_id=" << battleId;
        return false;
    }

    // 整笔可应用判据。必须在任何副作用之前,且必须覆盖**整笔**结算:一笔「只有 HP + 宝宝 + 击杀事实」的 PVE 结算
    // (打空怪、无掉落,完全正常)也不能在冻结期穿过闸门 —— 此刻存盘被硬性跳过,改动会随实体销毁一起没。
    if (!IsSettlementApplicable(player))
    {
        LOG_WARN << "[PlayerBattle] Player cannot currently be modified(cross-zone freeze / handoff in progress / exiting),"
                    "settlement deferred: player_id=" << playerId << " battle_id=" << battleId;
        return false;
    }

    // 宠物、物品和任务的写入口不能把加载故障当空状态跳过。必须在金币、HP/MP、任务产生任何副作用前检查,
    // 否则永久水位一过,漏掉的宠物状态或物品再也无法补回。
    const bool advancesMissions = std::any_of(settlement.defeated_monsters().begin(), settlement.defeated_monsters().end(),
        [](const auto& defeat) { return defeat.monster_config_id() != 0 && defeat.count() > 0; });
    // 近期战斗结果(PlayerBattleResultComp)同属持久数据,效果写完要在同一帧记进去,那时已没有失败分支:
    // 缺了说明加载不变量已被破坏,在这里整笔 DEFER,不补默认值(docs/design/battle-result-recovery.md §4.3)。
    if (!tlsEcs.actorRegistry.all_of<DerivedAttributesComp, PlayerBattleResultComp>(player) ||
        (settlement.pets_size() > 0 && !tlsEcs.actorRegistry.all_of<PlayerPetComp>(player)) ||
        (advancesMissions && !tlsEcs.actorRegistry.all_of<MissionsContainerComp>(player)) ||
        ((settlement.items_consumed_size() > 0 || settlement.items_gained_size() > 0) &&
         !tlsEcs.actorRegistry.all_of<PlayerBagsComp>(player)))
    {
        LOG_ERROR << "[PlayerBattle] Settlement application failed: required persisted or derived component missing, player_id="
                  << playerId << " battle_id=" << battleId;
        return false;
    }

    SettlementCurrencyGains currencyGains;
    if (CollectSettlementCurrencyGains(settlement, currencyGains) != SettlementCurrencyVerdict::kValid) {
        LOG_ERROR << "[PlayerBattle] Invalid currency settlement: player_id=" << playerId
                  << " battle_id=" << battleId;
        return false;
    }

    const auto inboxIdentity = RoleAdmissionSystem::ConfirmedIdentity(playerId);
    if (!inboxIdentity) return false;
    const auto originalIdentity = inboxIdentity->SerializeAsString();
    ItemCountMap inboxConsumed, inboxGained, actualConsumption;
    std::set<uint64_t> inboxPets;
    std::map<std::string, uint32_t> skippedPets;
    std::string originalPets;
    if (!HasCompleteBattleEffectInputs(player, settlement, currencyGains) ||
        !CollectExactSettlementItems(settlement, inboxConsumed, inboxGained)) return false;
    if (!CollectApplicableInboxPets(player, settlement, inboxPets, skippedPets)) return false;
    // 消耗或掉落里有本节点 Item 表查不到的配置号:整笔 DEFER、零副作用(混版本持久字段保护设计 §12 载荷行、§13 第 10 条)。
    // Item 表行 id 只增不删(data/AGENTS.md「行 id 只增不删」,导表器按 cfg_ids_permanent 守住),查不到只可能是这一行
    // 还没热更到本节点,或结算来自更新版本的 battle 节点。剔除这一项就是丢掉玩家这一局的掉落;热更到这一行之后本地续做
    // 即可成功,与号段暂缺、跨区冻结同一条路:收件箱照常退避重试,不停用本域。
    // 必须在剔除封禁项、按候选算实际可扣量之前:查不到的项既不能当「被封」剔掉,也不能当「持有 0」夹紧。
    const uint32_t unknownConsumed = SmallestItemConfigUnknownToThisNode(inboxConsumed);
    const uint32_t unknownGained = SmallestItemConfigUnknownToThisNode(inboxGained);
    if (unknownConsumed != 0 || unknownGained != 0)
    {
        LOG_WARN << "[PlayerBattle] metric=persist_version_skew where=battle_item player_id=" << playerId
                 << " battle_id=" << battleId << " side=" << (unknownConsumed != 0 ? "consumed" : "gained")
                 << " config_id=" << (unknownConsumed != 0 ? unknownConsumed : unknownGained)
                 << "; item config is not in this node's item table (row not hot-updated here yet, or settlement from a newer"
                    " battle node), deferring the whole settlement without side effects";
        return false;
    }
    // 被获得封禁的掉落只剔除这一项(DropBlockedGains 的说明)。排在号段预检与入包之前:被封的掉落不铸号、不进候选。
    const auto blockedDrops = DropBlockedGains(player, inboxGained);
    if (settlement.pets_size() > 0) originalPets = tlsEcs.actorRegistry.get<PlayerPetComp>(player).SerializeAsString();
    // 实际可扣量 = 可计数总量 - 别的局的道具预留;本局自己的预留可以动用(battle-item-reservation.md)。
    // 与 ConsumeInventoryAndGrant(consumeFromHoldId = battleId)里的夹紧是同一个判据(BagService::RemovableCount)。
    if (!inboxConsumed.empty()) {
        const auto& inventory = tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory];
        for (const auto& [config, count] : inboxConsumed)
            actualConsumption[config] = static_cast<uint32_t>(
                std::min<uint64_t>(count, BagService::RemovableCount(player, inventory, config, battleId)));
    }

    // 掉落要铸的新实例身份必须先有着落(审计 cpp-economy#0)。号段暂缺是会自己消失的条件,
    // 与跨区冻结同类:整笔 DEFER、零副作用,条目留在收件箱。
    // 按真正交给 ConsumeInventoryAndGrant 的掉落算(已剔除被封禁的项):剔除的项不铸号,
    // 按原件算会把本可发放的结算误判成号段不足。
    if (!HasItemGuidsForDrops(inboxGained, playerId, battleId))
    {
        return false;
    }

    auto* baseAttributes = tlsEcs.actorRegistry.try_get<BaseAttributesComp>(player);
    if (baseAttributes == nullptr)
    {
        LOG_ERROR << "[PlayerBattle] Settlement application failed: missing BaseAttributesComp, player_id=" << playerId
                  << " battle_id=" << battleId;
        return false;
    }

    // 被获得封禁而跳过的币种(CanCreditWholeBattleCurrency 的说明),应用成功之后逐项记日志并从 currencyGains 摘掉。
    std::vector<std::pair<CurrencyType, uint64_t>> blockedCurrencies;
    // scene EventLoop内同步执行且不让出；全部可拒绝条件已在首笔入账前复核。
    // 封禁按提交这一刻判定,与紧挨着的复核(credit 里的 HasCompleteBattleEffectInputs)看到同一份状态;
    // 先判再调 AddCurrency:被封的币种不进入账原语,也就不走它的错误分支(那里回 kAssetBlocked 并打调用栈)。
    const auto creditCurrencies = [&]() -> uint32_t {
        for (const auto& [type, amount] : currencyGains) {
            if (amount == 0) continue;
            if (IsSettlementCurrencyGainBlocked(player, type)) {
                blockedCurrencies.emplace_back(type, amount);
                continue;
            }
            const auto err = CurrencySystem::AddCurrency(player, type, static_cast<int64_t>(amount), TX_SYSTEM_GRANT, battleId);
            if (err != kSuccess) {
                LOG_ERROR << "[PlayerBattle] Currency credit failed after preflight;settlement deferred: player_id="
                          << playerId << " battle_id=" << battleId
                          << " currency_type=" << static_cast<uint32_t>(type) << " err=" << err;
                return err;
            }
        }
        return kSuccess;
    };
    // 整批包候选及所有初始化完成后，复核身份、宠物与每币净额；成功后不再有业务失败分支。
    const auto credit = [&]() -> uint32_t {
        const auto currentIdentity = RoleAdmissionSystem::ConfirmedIdentity(playerId);
        const auto* currentPets = tlsEcs.actorRegistry.try_get<PlayerPetComp>(player);
        if (!currentIdentity || currentIdentity->SerializeAsString() != originalIdentity ||
            (settlement.pets_size() > 0 && (!currentPets || currentPets->SerializeAsString() != originalPets)) ||
            HasUnsupportedInboxConsumption(player, settlement) ||
            !IsSettlementApplicable(player) || !HasCompleteBattleEffectInputs(player, settlement, currencyGains) ||
            !tlsEcs.actorRegistry.all_of<BaseAttributesComp, DerivedAttributesComp, PlayerBattleResultComp>(player))
            return kInvalidParameter;
        return creditCurrencies();
    };
    // 两包都放不下时这一局的掉落整批改投邮件(docs/design/battle-overflow-mail.md):在任何副作用之前备好待寄条目
    // (分封、取请求号、核待寄队列余量),与扣消耗、入账在同一段写进存档;存档确认提交之后才寄出(PlayerMailOutboxSystem)。
    // 接不住(待寄队列满、请求号暂缺)就与以前一样整笔延后。mailed 只在 accept 接住时非空,结算成功时它们已经写进存档。
    PlayerMailOutboxSystem::PreparedMails mailed;
    const BagService::GrantOverflowRoute toMail{
        [&](const ItemCountMap& items) {
            mail_outbox_rules::MailItems mailItems;
            for (const auto& [config, count] : items) mailItems[config] += count;
            return PlayerMailOutboxSystem::PrepareBattleOverflow(player, battleId, mailItems, mailed);
        },
        [&](const ItemCountMap&) {
            PlayerMailOutboxSystem::CommitPrepared(player, battleId, mailed, TimeSystem::NowMillisecondsUTC());
        }};
    const auto result = inboxConsumed.empty() && inboxGained.empty() ? credit() :
        BagService::ConsumeInventoryAndGrant(player, inboxConsumed, inboxGained, battleId,
            "{\"source\":\"battle-inbox\"}", credit, /*consumeFromHoldId=*/battleId, &toMail);
    if (result != kSuccess) return false;
    for (const auto& mail : mailed)
        LOG_INFO << "[PlayerBattle] metric=battle_items_mailed player_id=" << playerId << " battle_id=" << battleId
                 << " request_id=" << mail.requestId << " item_lines=" << mail.items.size()
                 << ";both bags were full, this battle's drops were queued for mail";
    // 这一局的道具预留到此为止:消耗已从预留里扣出,余量随之放开(battle-item-reservation.md)。与扣消耗同一帧、
    // 同一次存档提交(结算随存档确认之后才公开),两条分支(有没有物品)都走这里。没有预留(没带道具)时是空操作。
    if (BagService::ReleaseHold(player, battleId))
        LOG_INFO << "[PlayerBattle] metric=battle_item_hold_released player_id=" << playerId << " battle_id=" << battleId
                 << " reason=settlement_applied";
    // reaper 已按期限摘掉这一局的冻结(BattleFreezeExpiredComp 记着它)之后才到的结算:冻结期间资产通道被战斗闸挡着,
    // 摘掉之后就不再挡,这时的夹紧可能是本局用掉的消耗品先被别的通道扣走了。单独打一行供对账(2026-10-30)。
    const bool freezeReleasedBeforeSettlement = ExpiredFightingBattleIdOf(player) == battleId;
    for (const auto& [config, requested] : inboxConsumed) {
        const auto applied = actualConsumption.at(config);
        if (applied >= requested) continue;
        LOG_ERROR << "metric=battle_inbox_consumption_shortfall reason=held_less_than_committed"
                  << " battle_id=" << battleId << " config_id=" << config
                  << " requested=" << requested << " consumed=" << applied;
        if (freezeReleasedBeforeSettlement)
            LOG_ERROR << "metric=battle_inbox_shortfall_after_freeze_released path=inbox battle_id=" << battleId
                      << " config_id=" << config << " requested=" << requested << " consumed=" << applied
                      << " player_id=" << playerId
                      << ";battle freeze was released by the reaper before this settlement arrived,"
                      << " consumption clamped to current holdings";
    }
    for (const auto& [reason, count] : skippedPets)
        LOG_ERROR << "metric=battle_inbox_pet_effect_skipped reason=" << reason
                  << " battle_id=" << battleId << " count=" << count;
    // 被获得封禁而跳过的掉落与币种:封禁本意就是不发,不补偿、解封后也不补发,只留痕供客服与对账按 battle_id 查。
    for (const auto& [configId, count] : blockedDrops)
        LOG_WARN << "[PlayerBattle] metric=battle_settlement_item_blocked source=inbox battle_id=" << battleId
                 << " config_id=" << configId << " count=" << count << " player_id=" << playerId
                 << ";item gain blocked (global or this player), this drop was skipped and the rest of the settlement applied";
    for (const auto& [type, amount] : blockedCurrencies) {
        LOG_WARN << "[PlayerBattle] metric=battle_settlement_currency_blocked source=inbox battle_id=" << battleId
                 << " currency_type=" << static_cast<uint32_t>(type) << " amount=" << amount << " player_id=" << playerId
                 << ";currency gain blocked (global or this player), this currency was skipped and the rest of the settlement applied";
        // 近期战斗结果只记实际入账(RecordBattleResult 读 currencyGains):被跳过的币种从这里摘掉。
        currencyGains.erase(type);
    }
    baseAttributes = tlsEcs.actorRegistry.try_get<BaseAttributesComp>(player); // 初始化回调后不持旧storage地址。

    // HP/MP 终值回写(引擎输出已按快照上限饱和,这里再按派生上限夹一次,防御引擎侧越界)
    const auto& derived = tlsEcs.actorRegistry.get<DerivedAttributesComp>(player);
    uint64_t health = settlement.health();
    if (derived.max_health() > 0)
    {
        health = std::min<uint64_t>(health, derived.max_health());
    }
    baseAttributes->set_health(health);
    uint64_t mana = settlement.mana();
    if (derived.max_mana() > 0)
    {
        mana = std::min<uint64_t>(mana, derived.max_mana());
    }
    baseAttributes->set_mana(mana);
    // 阵亡基础复活(与登录加载同一规则,产品完整死亡/复活流程未定前的基线):
    // 不复活的话 0 血玩家会再次排队、被快照进新局、引擎开局即判负(2026-09-02 冒烟实测,
    // 离线期间的结算在登录后才应用时也走这里,登录时的复活判定早已跑完、拦不住)。
    if (settlement.is_dead() || health == 0)
    {
        // 回满到玩家真实上限(属性加点算出的 DerivedAttributesComp)。
        // 只回 ClassTable 的 1 级初值会把等级/加点成长吞掉(20 级 max_health 约 1100,初值 500)。
        ReviveBaseAttributesIfDead(*baseAttributes,
            derived.max_health(), derived.max_mana());
        LOG_INFO << "[PlayerBattle] Basic revival after death in settlement: player_id=" << playerId
                 << " battle_id=" << battleId
                 << " health=" << baseAttributes->health() << " mana=" << baseAttributes->mana();
    }
    ActorAttributeCalculatorSystem::MarkAttributeForUpdate(player, kHealth);
    ActorAttributeCalculatorSystem::MarkAttributeForUpdate(player, kEnergy);

    // 出战宝宝的终值回写(阵亡宝宝在 PetSystem 内回满,理由见那里的注释)。
    // 回写完必须推一次列表:玩家战后打开宝宝面板看到的应该是战后血量,
    // 不推的话面板会一直停在战前那份(设计文档 player-pet.md §4 承诺的推送时机)。
    if (settlement.pets_size() > 0)
    {
        for (const auto& petSettlement : settlement.pets())
        {
            if (!inboxPets.contains(petSettlement.pet_id())) continue;
            PetSystem::ApplyBattleSettlement(player, petSettlement);
        }
        if (!inboxPets.empty()) PetSystem::PushList(player);
    }

    // 经验系统尚未接入,一期仅记日志留痕;升级后果见 PlayerAttributeSystem::GmSetLevel,
    // 经验系统接入时抽出共用(见 open_issues)。
    if (settlement.exp_gain() > 0)
    {
        LOG_INFO << "[PlayerBattle] Experience settlement deferred(experience system not integrated): player_id=" << playerId
                 << " battle_id=" << battleId << " exp=" << settlement.exp_gain();
    }

    // 只有 battle 最终结算携带的真实击杀事实推进任务。每只怪一条事实，顺序任务不会用同一次击杀跨过后续相同目标。
    for (const auto& defeat : settlement.defeated_monsters()) {
        if (defeat.monster_config_id() == 0) continue;
        for (uint32_t index = 0; index < defeat.count(); ++index) {
            ConditionEvent progress;
            progress.player = player;
            progress.conditionType = static_cast<uint32_t>(eConditionType::kConditionKillMonster);
            progress.conditionIds.push_back(defeat.monster_config_id());
            progress.amount = 1;
            PlayerMissionSystem::OnCondition(player, progress);
        }
    }
    // 本人近期战斗结果:与上面全部效果同一帧写入,随同一次存档提交;存档确认提交之后由 CompleteInboxSettlement 公开
    // (docs/design/battle-result-recovery.md §5)。记实际入账:剔除被封禁项之后的掉落(两包都放不下而改投邮件的
    // 那一批也算:它已经是玩家的,经邮件领取)、未被封禁的币种、按实际持有夹紧的消耗。
    RecordBattleResult(player, playerId, settlement, currencyGains, inboxGained, actualConsumption,
                       /*itemsMailed=*/!mailed.empty());
    return true; // 永久Inbox域水位由调用方与本次效果同帧记入同一快照。
}

void PlayerBattleSystem::NotifyBattleReconnectToClient(entt::entity player)
{
	const auto* inBattle = tlsEcs.actorRegistry.try_get<InBattleComp>(player);
	if (inBattle == nullptr)
	{
		return;
	}

	// 只推重连提示(turn-based §22 D72):gate 不再中继战斗,没有绑定可重建。
	// 这是 scene 自己经大厅会话的下行(scene→gate TCP,不经 Kafka),不归 D39 管;
	// 客户端收到后凭 battle_id 经 MatchService.RequestBattleTicket 补签、重建直连、
	// 直连就绪后 GetBattleState 补拉全量状态。
	::BattleReconnectS2C message;
	message.set_battle_id(inBattle->battle_id());
	SendToPlayerClient(BattleClientPlayerNotifyBattleReconnectMessageId, message, player);

	LOG_INFO << "[PlayerBattle] Session changed during battle;reconnect notification sent: player_id=" << tlsEcs.GetPlayerId(player)
			 << " battle_id=" << inBattle->battle_id()
			 << " state=" << eInBattleState_Name(inBattle->state())
			 << " battle_node_id=" << inBattle->battle_node_id();
}

void PlayerBattleSystem::RunLoginRechecks()
{
	auto view = tlsEcs.actorRegistry.view<BattleLoginRecheckComp>();
	if (view.size() == 0)
	{
		return;
	}
	const bool redisReady = RedisReady();
	size_t deferred = 0;
	// 先收集再处理:RestoreBattleFreezeOnLogin 可能当场重新挂标记(命令没发出去),边遍历边改视图会失效迭代器。
	std::vector<entt::entity> due(view.begin(), view.end());
	for (const auto entity : due)
	{
		const uint64_t playerId = tlsEcs.GetPlayerId(entity);
		battle_lock::LoginRecheckEntryState state;
		state.exitingOrNoIdentity = playerId == 0 || tlsEcs.actorRegistry.any_of<UnregisterPlayer>(entity);
		state.travelHandoffOrFrozen = !IsSettlementApplicable(entity);
		state.redisReady = redisReady;
		// 各分支的理由见 battle_lock_rules.h 的 ClassifyLoginRecheckEntry(退出中 > 交接中 > Redis 不可用 > 重跑)。
		switch (battle_lock::ClassifyLoginRecheckEntry(state))
		{
		case battle_lock::LoginRecheckStep::kDrop:
			tlsEcs.actorRegistry.remove<BattleLoginRecheckComp>(entity);
			LOG_INFO << "[PlayerBattle] Login battle recheck dropped: player is exiting, player_id=" << playerId;
			continue;
		case battle_lock::LoginRecheckStep::kKeep:
			if (!state.travelHandoffOrFrozen)
			{
				++deferred; // 只为汇总日志计数 Redis 不可用的那部分
			}
			continue;
		case battle_lock::LoginRecheckStep::kRun:
			break;
		}
		// 先摘标记再重跑:这一轮又没问到会重新挂上,同一玩家同一时刻最多一条复查在途。
		tlsEcs.actorRegistry.remove<BattleLoginRecheckComp>(entity);
		LOG_WARN << "[PlayerBattle] metric=battle_login_recheck_run player_id=" << playerId
				 << ",rerunning login battle lock read and freeze restore";
		if (!tlsEcs.actorRegistry.any_of<InBattleComp>(entity))
		{
			RestoreBattleFreezeOnLogin(entity, playerId);
		}
	}
	if (deferred != 0)
	{
		// 只打一条汇总,不按玩家刷屏。
		LOG_WARN << "[PlayerBattle] Login battle rechecks deferred(Redis disconnected), pending=" << deferred;
	}
}

bool PlayerBattleSystem::IsLoginRecheckPending(entt::entity player)
{
	return tlsEcs.actorRegistry.valid(player) && tlsEcs.actorRegistry.any_of<BattleLoginRecheckComp>(player);
}

uint64_t PlayerBattleSystem::ExpiredFightingBattleId(entt::entity player)
{
	return ExpiredFightingBattleIdOf(player);
}

battle_lock::LockRebuildDecision PlayerBattleSystem::DecideLockRebuildFor(entt::entity player, const uint64_t lockBattleId)
{
	return DecideLockRebuild(player, lockBattleId);
}

void PlayerBattleSystem::ReleaseExpiredFightingFreeze(entt::entity player, const uint64_t playerId,
													  const uint64_t battleId)
{
	// 先记下作废的局,再删锁:删锁没落地时,之后任何一条按锁重建的路径都靠它认出这把锁(BattleFreezeExpiredComp 的说明)。
	RememberExpiredFightingBattle(player, battleId);
	// 按 battle_id 条件删锁:作废判定与锁值同源,不会误删别的战斗。
	// 走到这里只有这几种情形(ResolveExpiredFightingFreeze / OnExpiredFightingTerminalAnswer):SQL 终局是 ABORT、这一局没有
	// 可裁决的 SQL 原件(永远不会有结算)、结算已在本实体应用过、续冻到了上限,以及组件缺身份。
	// 续冻到上限的那一局,结算若之后才到,照常应用(结算不需要冻结在场),消耗按当时持有量夹紧。
	ClearBattleFreeze(player, playerId, battleId);
}

void PlayerBattleSystem::OnPlayerEnterScene(entt::entity player, uint32_t enterGsType)
{
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return;
	}
	const uint64_t playerId = tlsEcs.GetPlayerId(player);
	if (playerId == 0)
	{
		return;
	}

	// 1) 实体是新建的(InBattleComp 不落库)而 battle:lock 还在 = 战斗仍在途:按锁 + ctx 重建冻结
	//    (FIGHTING 时提示客户端补签重连),之后的收件箱结算按同一局解冻放锁。
	//    结算不在这里读:它只经收件箱,落地钩子 PlayerInboxSystem::OnPlayerLanded 会拉一次。
	//    「没问到」(空应答 / 命令没发出去 / Redis 不可用)不当结论:
	//    记入登录复查,reaper 节拍在 Redis 可用时重读(BattleLoginRecheckComp 的说明)。
	if (!tlsEcs.actorRegistry.any_of<InBattleComp>(player))
	{
		RestoreBattleFreezeOnLogin(player, playerId);
	}

	// 2) 战斗中换了会话:实体仍存活且 InBattleComp 为 FIGHTING -> 提示客户端补签重连(turn-based §22 D72)
	//    (完整下线再登录的提示走上面的 RestoreBattleFreezeOnLogin 回调)。
	//    RECONNECT 与 REPLACE 都算换会话:跨 gate 重定向(RedirectToGate)后旧会话通常仍是 Online,
	//    login 判成 REPLACE 而不是 RECONNECT;只认 RECONNECT 会让重定向后的玩家拿不到
	//    重连提示,战斗在客户端侧断掉(turn-based-battle-server.md §18.7 / D38)。
	//    LOGIN_FIRST 不进这里:首登实体是新建的,没有 InBattleComp,由第 1 步按锁 + ctx 重建。
	//    PREPARING 不推(§22.5 第 3 条):房间还没建成(观战记录可能也还没写),补签拿到
	//    kInvalidParameter,客户端会先判 BattleGone、弹「战斗已结束」再被拉回战斗。
	//    这一局的提示交给 ConfirmBattle:升级 FIGHTING 时比对 BattlePrepareSessionComp 记下的
	//    备战会话号,换过就补推;确认事件由 battle 侧周期补发兜底。
	if (enterGsType == LOGIN_RECONNECT || enterGsType == LOGIN_REPLACE)
	{
		if (const auto* inBattle = tlsEcs.actorRegistry.try_get<InBattleComp>(player);
			inBattle != nullptr && inBattle->state() == IN_BATTLE_STATE_FIGHTING)
		{
			NotifyBattleReconnectToClient(player);
		}
	}
}

void PlayerBattleSystem::ReapExpiredFreezes(const uint64_t nowMs)
{
	// 先收集再摘除:边遍历边 remove 会失效视图迭代器
	struct ExpiredEntry
	{
		entt::entity entity;
		uint64_t battleId;
		bool preparing;
		uint64_t deadlineMs;   // 原始期限:PREPARING 是生效的备战期限,FIGHTING 是 deadline_ms
		uint64_t expiresAtMs;  // FreezeExpiresAtMs:reaper 实际判到期的时刻
	};
	std::vector<ExpiredEntry> expired;
	for (auto&& [entity, inBattle] : tlsEcs.actorRegistry.view<InBattleComp>().each())
	{
		// PREPARING 看备战期限(CreateBattle 确认前 match 崩溃只等短期限),prepare_deadline_ms 为 0(旧版 match/存量组件)
		// 时退回 deadline_ms;FIGHTING 看 deadline + 终局提交宽限 + 收件箱投递余量(2026-10-30):deadline 一过就摘,
		// 迟到的合法结算到达之前资产通道的战斗闸就放开了(FreezeExpiresAtMs 的说明)。
		const bool preparing = inBattle.state() == IN_BATTLE_STATE_PREPARING;
		const uint64_t expiresAtMs = FreezeExpiresAtMs(preparing, inBattle.deadline_ms(), inBattle.prepare_deadline_ms());
		if (expiresAtMs != 0 && expiresAtMs < nowMs)
		{
			expired.push_back({entity, inBattle.battle_id(), preparing,
							   preparing ? expiresAtMs : inBattle.deadline_ms(), expiresAtMs});
		}
	}

	for (const auto& entry : expired)
	{
		const uint64_t playerId = tlsEcs.GetPlayerId(entry.entity);
		// 结构化 metric 日志:cpp scene 进程当前没有 Prometheus 端点,
		// 由日志侧提取计数,见 open_issues。
		// 两个 metric 区分:备战期作废(match gather 断链)vs 战斗期作废(battle 节点崩溃)
		if (entry.preparing)
		{
			// 备战到期:只摘 InBattleComp,不主动删锁 —— 锁 EX = prepare_deadline+60s 自然过期。
			// 这 60s 是 BattleConfirmedEvent 的投递余量(Kafka rebalance / 积压 / battle 侧补发):
			// 迟到的确认到达时锁值仍 == battle_id,ConfirmBattle 据此重建 FIGHTING 冻结;
			// 期间 match JoinQueue 对锁 fail-closed,玩家不会被放进第二场。
			// 确认真的没来,锁随 TTL 过期后玩家照常放行,与之前相比只多等最多 60s。
			LOG_WARN << "[PlayerBattle] metric=battle_prepare_expired player_id=" << playerId
					 << " battle_id=" << entry.battleId
					 << " prepare_deadline_ms=" << entry.deadlineMs
					 << " now_ms=" << nowMs
					 << ",preparation invalidated and component removed(CreateBattle unconfirmed;match disconnected or gather failed without cancellation);lock retained until TTL expiry for reconstruction by late confirmation";
			// 锁刻意保留:组队跟随链读到锁会 fail-closed 跳过,玩家下次进场再补
			RemoveInBattleComp(entry.entity);
			continue;
		}
		// FIGHTING 到期不再直接解冻(2026-11-06):先查这一局的 SQL 终局,COMPLETE 而本地还没应用 / 查不到就有界续冻。
		ResolveExpiredFightingFreeze(entry.entity, playerId, entry.battleId, nowMs);
	}

	// 冻结已经不在、道具预留还在的旧局(续冻到上限解冻、备战到期、重登时锁已过期、离线时被取消):按 SQL 终局收尾。
	// 排在冻结处置之后:这一拍刚按上限解开的局,预留复核在同一拍就接手。
	ReviewBattleItemHolds(nowMs);
}

void PlayerBattleSystem::ReviewBattleItemHolds(const uint64_t nowMs)
{
	// 先收集再处置:处置会改 PlayerItemHoldComp,不能边遍历边改。
	struct DueHold
	{
		entt::entity entity;
		uint64_t holdId;
		uint64_t reviewAfterMs;
	};
	std::vector<DueHold> due;
	for (auto&& [entity, holds] : tlsEcs.actorRegistry.view<PlayerItemHoldComp>().each())
	{
		if (holds.holds_size() == 0)
		{
			continue;
		}
		const auto* inBattle = tlsEcs.actorRegistry.try_get<InBattleComp>(entity);
		for (const auto& hold : holds.holds())
		{
			// 别的种类归别的预留方;这一局的冻结还在时归冻结那条路(到期先查终局,ABORT 时连同预留一起释放)。
			if (hold.kind() != static_cast<uint32_t>(item_hold::ItemHoldKind::kBattle) ||
				(inBattle != nullptr && inBattle->battle_id() == hold.hold_id()) || hold.review_after_ms() >= nowMs)
			{
				continue;
			}
			due.push_back({entity, hold.hold_id(), hold.review_after_ms()});
		}
	}
	for (const auto& entry : due)
	{
		ReviewBattleItemHold(entry.entity, tlsEcs.GetPlayerId(entry.entity), entry.holdId, entry.reviewAfterMs, nowMs);
	}
}

void PlayerBattleSystem::ReviewBattleItemHold(entt::entity player, const uint64_t playerId, const uint64_t holdId,
											   const uint64_t reviewAfterMs, const uint64_t nowMs)
{
	// 只复核登记在本节点的玩家实体;释放预留是存档改动:交接、退出中、没有归属围栏时不动
	// (交接完成后在目的地复核,退出后下次加载再复核)。
	if (playerId == 0 || tlsEcs.GetPlayer(playerId) != player || !player_mutability::IsPlayerMutable(player))
	{
		return;
	}
	// 冻结到期 + 续冻上限之后仍没收尾:每 10 分钟一行 ERROR。结算还在路上(发件箱积压、收件箱整笔延后)或 match 不可达时会走到这里;
	// 玩家这时能正常游戏,只是这一局带进战斗的道具转不走、用不掉。
	const uint64_t overdueAtMs = reviewAfterMs > UINT64_MAX - kFreezeHoldCapMs ? UINT64_MAX : reviewAfterMs + kFreezeHoldCapMs;
	if (nowMs > overdueAtMs && battle_freeze_hold::IsHoldOverdueLogDue(nowMs - overdueAtMs, kReaperIntervalMs))
	{
		LOG_ERROR << "[PlayerBattle] metric=battle_item_hold_overdue player_id=" << playerId << " battle_id=" << holdId
				  << " review_after_ms=" << reviewAfterMs << " now_ms=" << nowMs
				  << ",battle items are still held for a battle whose settlement has not applied here;"
					 "they stay unmovable until the settlement applies or the battle is aborted";
	}
	if (IsBattleSettlementAppliedHere(player, holdId))
	{
		// 结算应用时本该同帧释放;走到这里是异常(例如旧版本应用过结算、没有释放这份预留)。已应用就没有要从预留里扣的了。
		LOG_WARN << "[PlayerBattle] Battle item hold outlived its applied settlement, releasing: player_id=" << playerId
				 << " battle_id=" << holdId;
		ReleaseBattleItemHold(player, playerId, holdId, "applied_here");
		return;
	}
	// 没发出去、在途太久都按「未知」:预留留着,下一拍再问(没有上限,理由见 battle_freeze_hold::IsBattleHoldReleasable)。
	(void)SendTrackedTerminalQuery(playerId, holdId, nowMs, [playerId, holdId](const battle_freeze_hold::TerminalAnswer answer) {
		OnBattleItemHoldTerminalAnswer(playerId, holdId, answer);
	});
}

void PlayerBattleSystem::OnBattleItemHoldTerminalAnswer(const uint64_t playerId, const uint64_t holdId,
														 const battle_freeze_hold::TerminalAnswer answer)
{
	const auto player = tlsEcs.GetPlayer(playerId);
	if (!tlsEcs.actorRegistry.valid(player) || tlsEcs.GetPlayerId(player) != playerId)
	{
		return; // 玩家已不在本节点:预留在他的存档里,下次加载后再复核
	}
	// 回答在途期间这一局的冻结又挂上了(迟到的确认 / 按锁重建):归冻结那条路处置。
	if (const auto* inBattle = tlsEcs.actorRegistry.try_get<InBattleComp>(player);
		inBattle != nullptr && inBattle->battle_id() == holdId)
	{
		return;
	}
	if (!battle_freeze_hold::IsBattleHoldReleasable(answer))
	{
		LOG_DEBUG << "[PlayerBattle] Battle item hold kept: player_id=" << playerId << " battle_id=" << holdId
				  << " terminal=" << battle_freeze_hold::TerminalAnswerName(answer);
		return;
	}
	if (!player_mutability::IsPlayerMutable(player))
	{
		return; // 下一拍再看
	}
	ReleaseBattleItemHold(player, playerId, holdId, battle_freeze_hold::TerminalAnswerName(answer));
}

void PlayerBattleSystem::ResolveExpiredFightingFreeze(entt::entity player, const uint64_t playerId,
													   const uint64_t battleId, const uint64_t nowMs)
{
	// 没有身份可查(组件损坏):按改动前的规则解冻,不能把它冻到上限。
	if (playerId == 0 || battleId == 0)
	{
		LOG_ERROR << "[PlayerBattle] metric=battle_freeze_expired player_id=" << playerId << " battle_id=" << battleId
				  << " terminal=no_identity now_ms=" << nowMs << ",expired freeze has no player or battle id;released without a terminal query";
		ReleaseExpiredFightingFreeze(player, playerId, battleId);
		return;
	}
	// 这一局的结算已在本实体应用过(还在等提交确认,或已提交而删锁没落地、玩家重登后按锁重建了冻结):
	// 消耗已经扣过,解冻不会让药白喝;结算路径上的放锁(CompleteInboxSettlement)晚到也照常收尾。不必查。
	// 环里的 REJECTED / SKIPPED 不算收尾,照旧往下查(IsBattleSettlementAppliedHere 的说明)。
	if (IsBattleSettlementAppliedHere(player, battleId))
	{
		LOG_WARN << "[PlayerBattle] metric=battle_freeze_expired player_id=" << playerId << " battle_id=" << battleId
				 << " terminal=applied_here now_ms=" << nowMs
				 << ",settlement of this battle is already applied on this entity;freeze released without a terminal query";
		ReleaseExpiredFightingFreeze(player, playerId, battleId);
		// 结算应用时已释放过预留;这里再放一次只为兜住「结算应用了而预留还在」的异常,幂等。
		ReleaseBattleItemHold(player, playerId, battleId, "applied_here");
		return;
	}
	const auto key = std::make_pair(playerId, battleId);
	// 已越过续冻上限(墙钟:上限锚定在 battle 给的 deadline 上,是跨进程的截止时刻):越过之后的处置只有「解冻」一种,
	// 这一拍直接告警解冻,不再发查询、也不等在途的回答(评审修正 2026-11-06)。这样任何路径(含查询丢失)的解冻都不晚于
	// 越过上限后的第一拍,锁一定还在(kReaperIntervalMs 的 static_assert)。在途登记一并摘掉,迟到的回答见冻结已不在,什么都不做。
	const auto* inBattle = tlsEcs.actorRegistry.try_get<InBattleComp>(player);
	const uint64_t deadlineMs = inBattle != nullptr ? inBattle->deadline_ms() : 0;
	const uint64_t holdDeadlineMs = FreezeHoldDeadlineMs(deadlineMs);
	if (holdDeadlineMs != 0 && nowMs > holdDeadlineMs)
	{
		const bool queryInFlight = gTerminalQueriesInFlight.erase(key) > 0;
		LOG_ERROR << "[PlayerBattle] metric=battle_freeze_hold_cap_reached player_id=" << playerId << " battle_id=" << battleId
				  << " terminal=not_queried_past_cap query_in_flight=" << queryInFlight << " deadline_ms=" << deadlineMs
				  << " hold_deadline_ms=" << holdDeadlineMs << " now_ms=" << nowMs
				  << ",freeze held up to the cap but the settlement never settled here;releasing the freeze under the expiry rule"
					 "(battle items stay held, a settlement arriving later still consumes from the hold)";
		CountFreezeHoldEvent(battle_freeze_hold::FreezeHoldEvent::kCapReached);
		// 只解冻、不放道具预留(battle-item-reservation.md):结算仍可能晚到,到了从预留里扣;
		// 预留交给 reaper 的预留复核(ReviewBattleItemHolds)按 SQL 终局收尾。
		ReleaseExpiredFightingFreeze(player, playerId, battleId);
		return;
	}
	// 回调只捕获 id 与这一拍的墙钟时刻(不捕获实体句柄:回答到达时实体可能已销毁 / 句柄被复用)。
	switch (SendTrackedTerminalQuery(playerId, battleId, nowMs, [playerId, battleId, nowMs](const battle_freeze_hold::TerminalAnswer answer) {
				OnExpiredFightingTerminalAnswer(playerId, battleId, answer, nowMs);
			}))
	{
	case TerminalQueryStep::kSent:
	case TerminalQueryStep::kInFlight: // 上一拍发出的查询还在途:等它的回答
	case TerminalQueryStep::kCount:
		return;
	case TerminalQueryStep::kLost:
	case TerminalQueryStep::kNotSent:
		OnExpiredFightingTerminalAnswer(playerId, battleId, battle_freeze_hold::TerminalAnswer::kUnknown, nowMs);
		return;
	}
}

void PlayerBattleSystem::OnExpiredFightingTerminalAnswer(const uint64_t playerId, const uint64_t battleId,
														  const battle_freeze_hold::TerminalAnswer answer, const uint64_t nowMs)
{
	const auto player = tlsEcs.GetPlayer(playerId);
	if (!tlsEcs.actorRegistry.valid(player) || tlsEcs.GetPlayerId(player) != playerId)
	{
		// 玩家已不在本节点:下次登录按锁重建冻结,到期再查一次(续冻期间锁按续冻上限算 TTL)。
		LOG_INFO << "[PlayerBattle] Battle terminal answer dropped: player is no longer on this node, player_id=" << playerId
				 << " battle_id=" << battleId << " terminal=" << battle_freeze_hold::TerminalAnswerName(answer);
		return;
	}
	const auto* inBattle = tlsEcs.actorRegistry.try_get<InBattleComp>(player);
	// 与 reaper、LockTtlSecOf 同一口径:不是 PREPARING 就按 FIGHTING 处置(state 缺失的组件 reaper 也当 FIGHTING 送来)。
	// 只认 FIGHTING 的话,这种冻结每拍都查、回答每次都被丢掉,永远解不了冻,违反续冻有界(评审修正 2026-11-06)。
	if (inBattle == nullptr || inBattle->battle_id() != battleId || inBattle->state() == IN_BATTLE_STATE_PREPARING)
	{
		return; // 查询在途期间结算已确认提交并解冻,或玩家已在下一局:没有要处置的冻结
	}
	const uint64_t deadlineMs = inBattle->deadline_ms();
	const uint64_t expiresAtMs = FreezeExpiresAtMs(/*preparing=*/false, deadlineMs, 0);
	const uint64_t holdDeadlineMs = FreezeHoldDeadlineMs(deadlineMs);
	const uint64_t heldMs = nowMs > expiresAtMs ? nowMs - expiresAtMs : 0;
	const char* terminal = battle_freeze_hold::TerminalAnswerName(answer);
	switch (battle_freeze_hold::DecideExpiredFighting(answer, nowMs, holdDeadlineMs))
	{
	case battle_freeze_hold::ExpiredFightingStep::kRelease:
		LOG_WARN << "[PlayerBattle] metric=battle_freeze_expired player_id=" << playerId << " battle_id=" << battleId
				 << " terminal=" << terminal << " deadline_ms=" << deadlineMs << " expires_at_ms=" << expiresAtMs
				 << " held_ms=" << heldMs << " now_ms=" << nowMs
				 << ",no settlement will arrive for this battle(SQL terminal is ABORT, or there is no adjudicable SQL original);"
					"freeze released";
		ReleaseExpiredFightingFreeze(player, playerId, battleId);
		// 不会再有结算:道具预留一并释放(battle-item-reservation.md)。
		ReleaseBattleItemHold(player, playerId, battleId, terminal);
		return;
	case battle_freeze_hold::ExpiredFightingStep::kReleaseAtCap:
		LOG_ERROR << "[PlayerBattle] metric=battle_freeze_hold_cap_reached player_id=" << playerId << " battle_id=" << battleId
				  << " terminal=" << terminal << " deadline_ms=" << deadlineMs << " hold_deadline_ms=" << holdDeadlineMs
				  << " held_ms=" << heldMs << " now_ms=" << nowMs
				  << ",freeze held up to the cap but the settlement never applied here;releasing the freeze under the expiry rule"
					 "(battle items stay held, a settlement arriving later still consumes from the hold)";
		CountFreezeHoldEvent(battle_freeze_hold::FreezeHoldEvent::kCapReached);
		// 只解冻、不放道具预留(同 ResolveExpiredFightingFreeze 越过上限的那一支)。
		ReleaseExpiredFightingFreeze(player, playerId, battleId);
		return;
	case battle_freeze_hold::ExpiredFightingStep::kHold:
	case battle_freeze_hold::ExpiredFightingStep::kCount:
		break;
	}
	const char* why = ",SQL terminal unknown(query failed or unverifiable);freeze and lock kept until the next reaper tick";
	auto event = battle_freeze_hold::FreezeHoldEvent::kHeldUnknown;
	if (answer == battle_freeze_hold::TerminalAnswer::kComplete)
	{
		why = ",SQL terminal is COMPLETE but the settlement is not applied here yet;freeze and lock kept until the next reaper tick";
		event = battle_freeze_hold::FreezeHoldEvent::kHeldComplete;
	}
	else if (answer == battle_freeze_hold::TerminalAnswer::kPending)
	{
		why = ",SQL terminal not decided yet although the original is adjudicable(a COMPLETE may still land);"
			  "freeze and lock kept until the next reaper tick";
		event = battle_freeze_hold::FreezeHoldEvent::kHeldPending;
	}
	LOG_WARN << "[PlayerBattle] metric=battle_freeze_held player_id=" << playerId << " battle_id=" << battleId
			 << " terminal=" << terminal << " deadline_ms=" << deadlineMs << " hold_deadline_ms=" << holdDeadlineMs
			 << " held_ms=" << heldMs << " now_ms=" << nowMs << why;
	CountFreezeHoldEvent(event);
	// 锁跟着续:FightingLockTtlSecFor 按续冻上限算 TTL;只延长不缩短、锁值匹配才动(与确认续期同一条链)。
	// 开局确认时锁已写到同一个上限,这一次续期补的是确认续期没落地、或按锁重建时 TTL 偏短的情形。
	ConfirmBattleLockIfMatch(playerId, battleId, deadlineMs, SerializeBattleCtx(*inBattle));
}

bool PlayerBattleSystem::IsTerminalQueryInFlight(const uint64_t playerId, const uint64_t battleId)
{
	return gTerminalQueriesInFlight.contains(std::make_pair(playerId, battleId));
}

uint64_t PlayerBattleSystem::FreezeHoldEventCount(const battle_freeze_hold::FreezeHoldEvent event)
{
	const auto index = static_cast<std::size_t>(event);
	return index < gFreezeHoldEvents.size() ? gFreezeHoldEvents[index].load(std::memory_order_relaxed) : 0;
}

void PlayerBattleSystem::StartReaper(muduo::net::EventLoop* loop)
{
	if (loop == nullptr)
	{
		LOG_ERROR << "[PlayerBattle] StartReaper: loop is null";
		return;
	}
	if (gReaperActive)
	{
		// 幂等:先取消再重挂,避免重复定时器
		loop->cancel(gReaperTimerId);
		gReaperActive = false;
	}
	gReaperLoop = loop;

	gReaperTimerId = loop->runEvery(kReaperIntervalSec, [] {
		// 第一遍:按冻结到期时刻(FreezeExpiresAtMs)作废到期的冻结。
		ReapExpiredFreezes(TimeSystem::NowMillisecondsUTC());

		// 第二遍:登录钩子「没问到」的玩家重新读锁(BattleLoginRecheckComp 的说明)。
		// 排在第一遍之后:同一玩家的条件删锁与读数同 slot,路由层停等,删锁先于读数执行。
		RunLoginRechecks();
	});
	gReaperActive = true;

	LOG_INFO << "[PlayerBattle] Battle freeze reaper started, interval=" << kReaperIntervalSec << "s";
}

void PlayerBattleSystem::SetBattleLockReleaseSenderForTest(BattleLockReleaseSender sender)
{
	gBattleLockReleaseSender = sender; // nullptr = 生产实现(tlsPlayerRedis 发 EVAL)
}

void PlayerBattleSystem::SetFollowWakeFnForTest(FollowWakeFn fn)
{
	gFollowWakeFn = (fn == nullptr) ? &PlayerTeamSystem::OnBattleFreezeCleared : fn;
}

void PlayerBattleSystem::SetLockScriptObserverForTest(LockScriptObserver observer)
{
	gLockScriptObserver = observer; // nullptr = 生产(不观察)
}

void PlayerBattleSystem::SetLockScriptSenderForTest(LockScriptSender fn)
{
    gLockScriptSender = fn;
}

void PlayerBattleSystem::SetBattleTerminalQueryForTest(BattleTerminalQueryFn fn)
{
	// 换实现时清掉在途登记:属于旧实现的回调不保证还会来,留着会让同一局在下一个用例里被当成「还在途」。
	gTerminalQueriesInFlight.clear();
	gBattleTerminalQuery = (fn == nullptr) ? &SendBattleTerminalQuery : fn; // nullptr = 生产实现(MatchInternal 只读查询)
}

void PlayerBattleSystem::SetTerminalQueryClockForTest(TerminalQueryClockFn fn)
{
	gTerminalQueryClock = (fn == nullptr) ? &SteadyNowMs : fn; // nullptr = 生产实现(steady_clock)
}

void PlayerBattleSystem::SetLockRetrySchedulerForTest(LockRetrySchedulerFn fn)
{
	gLockRetryScheduler = fn; // nullptr = 生产实现(当前线程的 EventLoop::runAfter)
}

uint64_t PlayerBattleSystem::WriteBattleLockOnPrepare(const uint64_t playerId, const uint64_t battleId)
{
	// 匿名命名空间里的重发闭包调不到私有成员,逻辑放在那边;PrepareBattle 与单测都走这一个入口。
	return WriteBattleLock(playerId, battleId);
}

void PlayerBattleSystem::OnRebuiltFreezeRecheckReply(const uint64_t playerId, const uint64_t battleId,
													 const uint32_t attemptNumber,
													 const battle_lock::ReplyShape shape, const int64_t integer)
{
	// 与 ConfirmRebuiltFreeze 的 hiredis 回调走同一个函数(匿名命名空间里的回调调不到私有成员,逻辑放在那边)。
	ResolveRebuiltFreezeRecheck(playerId, battleId, attemptNumber, shape, integer);
}

void PlayerBattleSystem::StopReaper()
{
	if (!gReaperActive || gReaperLoop == nullptr)
	{
		return;
	}
	gReaperLoop->cancel(gReaperTimerId);
	gReaperActive = false;
	LOG_INFO << "[PlayerBattle] Battle freeze reaper stopped";
}
