#include "player_battle.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "battle_lock_reply_shape.h"
#include "battle_lock_rules.h"

#include <muduo/base/Logging.h>
#include <muduo/net/EventLoop.h>
#include <muduo/net/TimerId.h>
#include <muduo/contrib/hiredis/Hiredis.h>
#include <hiredis/hiredis.h>

#include <algorithm>
#include <exception>
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
#include "modules/bag/comp/player_bags_comp.h"
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
#include "player/system/player_lifecycle.h"  // IsCrossZoneFrozen / IsSceneChangeBusy
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
	//       同时 SET / 同 TTL / 同一条 Lua 里 EXPIRE / DEL,只在锁存在且值匹配时才被信任。
	// 结算原件不进 Redis:结算只走 match SQL + player_inbox(player-data-tidb-scaling-plan.md §5.4)。
	constexpr const char *kBattleLockKeyFmt = battle_lock::kBattleLockKeyFmt;
	constexpr const char *kBattleCtxKeyFmt = battle_lock::kBattleCtxKeyFmt;

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


	// 战斗锁与上下文都是玩家域键,走 tlsPlayerRedis。这里为真只说明至少一个节点连着,这名玩家所在的 slot 不一定可用,
	// 所以每个 command() 的返回值都要看:REDIS_ERR = 没发出去,回调不会来。
	bool RedisReady()
	{
		return tlsPlayerRedis.connected();
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
	// 唯一消费口 RunLoginRechecks。没有用 Redis 重连回调触发:那是 RedisSystem 独占的单槽,复查等一个 reaper 节拍足够。
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
	// 条件续期 + 覆写 ctx(ARGV[2]=ttl 秒,ARGV[3]=ctx 序列化):ConfirmBattle 一次原子完成
	// "锁切正式期限 + ctx 记录 FIGHTING/deadline",登录重建读到的 ctx 与锁永远一致。
	constexpr char kConfirmLockIfMatchScript[] =
		"if redis.call('GET', KEYS[1]) == ARGV[1] then "
		"redis.call('SET', KEYS[2], ARGV[3], 'EX', ARGV[2]); "
		"return redis.call('EXPIRE', KEYS[1], ARGV[2]) else return 0 end";
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

	// FIGHTING 冻结的锁 TTL:按冻结到期时刻(deadline + 终局提交宽限 + 收件箱投递余量,FreezeExpiresAtMs)计,
	// 不再按裸 deadline。reaper 要等到那个时刻才摘冻结,锁若按裸 deadline 先过期,宽限期内重登就重建不出冻结,
	// match 也会提前放下一场(2026-10-30)。
	uint64_t FightingLockTtlSecFor(const uint64_t deadlineMs, const uint64_t nowMs)
	{
		return LockTtlSecFor(PlayerBattleSystem::FreezeExpiresAtMs(/*preparing=*/false, deadlineMs, 0), nowMs);
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
	//  - IsCrossZoneFrozen:与那两个入口同判据;
	//  - PlayerTravelHandoffComp / UnregisterPlayer:这两个窗口里存盘入口开头直接跳过写盘,应用了也落不了盘。
	// 不应用就回 DEFER,条目留在收件箱,由本节点续做或目的地节点重拉。
	bool IsSettlementApplicable(entt::entity player)
	{
		return !PlayerLifecycleSystem::IsCrossZoneFrozen(player) &&
			   !tlsEcs.actorRegistry.any_of<PlayerTravelHandoffComp, UnregisterPlayer>(player);
	}

	// 从 InBattleComp 组 ctx 序列化(ctx 就是组件本身的镜像,不另造并行结构)
	std::string SerializeBattleCtx(const InBattleComp& inBattle)
	{
		return inBattle.SerializeAsString();
	}

	// 条件续期 + 覆写 ctx:锁值 == battleId 才 EXPIRE 到 ttlSec 并 SET ctx(同 TTL)。
	// ConfirmBattle 把备战短期限切成正式期限、并把 FIGHTING 状态落进 ctx 供登录重建。
	void ConfirmBattleLockIfMatch(const uint64_t playerId, const uint64_t battleId, const uint64_t ttlSec,
								  const std::string& ctxPayload)
	{
		if (!RedisReady())
		{
			LOG_WARN << "[PlayerBattle] Conditional renewal skipped(Redis disconnected), player_id=" << playerId
					 << " battle_id=" << battleId << "(lock may expire before battle ends;match advisory checks are temporarily blind)";
			return;
		}
		const int ret = tlsPlayerRedis.command(
			[playerId, battleId, ttlSec](hiredis::Hiredis*, redisReply* reply) {
				if (reply == nullptr || reply->type == REDIS_REPLY_ERROR)
				{
					LOG_ERROR << "[PlayerBattle] Conditional renewal EVAL failed, player_id=" << playerId
							  << " battle_id=" << battleId;
					return;
				}
				if (reply->type == REDIS_REPLY_INTEGER && reply->integer == 0)
				{
					LOG_WARN << "[PlayerBattle] Conditional renewal did not match(lock missing or value mismatch), player_id="
							 << playerId << " battle_id=" << battleId << " ttl_sec=" << ttlSec;
				}
			},
			(std::string("EVAL %s 2 ") + kBattleLockKeyFmt + " " + kBattleCtxKeyFmt + " %llu %llu %b").c_str(),
			kConfirmLockIfMatchScript, playerId, playerId, battleId, ttlSec,
			ctxPayload.data(), ctxPayload.size());
		if (ret != REDIS_OK)
		{
			LOG_WARN << "[PlayerBattle] Conditional renewal not sent, player_id=" << playerId
					 << " battle_id=" << battleId << "(lock may expire before battle ends;match advisory checks are temporarily blind)";
		}
	}

	// 重建冻结后的复核:重建依据的是"发 EVAL 那一刻"的锁,回调链上可能已排着一条同 battle_id 的
	// 条件删锁(结算确认 / 取消)。再发一次条件续期 + 覆写 ctx:命中说明锁仍是本战斗的,重建成立;
	// 未命中说明锁已在两次往返之间被删,立刻撤掉刚挂的 InBattleComp,不让已结算的战斗把玩家冻到 reaper。
	void ConfirmRebuiltFreeze(entt::entity player, const uint64_t playerId, const uint64_t battleId,
							  const uint64_t ttlSec, const std::string& ctxPayload)
	{
		if (!RedisReady())
		{
			LOG_WARN << "[PlayerBattle] Reconstruction recheck skipped(Redis disconnected), player_id=" << playerId
					 << " battle_id=" << battleId << "(reaper will release the freeze at the deadline)";
			return;
		}
		const int ret = tlsPlayerRedis.command(
			[player, playerId, battleId, ttlSec](hiredis::Hiredis*, redisReply* reply) {
				if (reply == nullptr || reply->type == REDIS_REPLY_ERROR)
				{
					LOG_ERROR << "[PlayerBattle] Reconstruction recheck EVAL failed, player_id=" << playerId
							  << " battle_id=" << battleId;
					return;
				}
				if (reply->type == REDIS_REPLY_INTEGER && reply->integer != 0)
				{
					return; // 锁仍匹配:重建成立,ctx 已同步
				}
				if (!tlsEcs.actorRegistry.valid(player) || tlsEcs.GetPlayerId(player) != playerId)
				{
					return;
				}
				if (const auto* current = tlsEcs.actorRegistry.try_get<InBattleComp>(player);
					current != nullptr && current->battle_id() == battleId)
				{
					RemoveInBattleComp(player);
					LOG_WARN << "[PlayerBattle] metric=battle_freeze_rebuild_reverted player_id=" << playerId
							 << " battle_id=" << battleId << ",lock was deleted during reconstruction(settlement/cancellation completed);reconstruction canceled";
				}
			},
			(std::string("EVAL %s 2 ") + kBattleLockKeyFmt + " " + kBattleCtxKeyFmt + " %llu %llu %b").c_str(),
			kConfirmLockIfMatchScript, playerId, playerId, battleId, ttlSec,
			ctxPayload.data(), ctxPayload.size());
		if (ret != REDIS_OK)
		{
			LOG_WARN << "[PlayerBattle] Reconstruction recheck not sent, player_id=" << playerId
					 << " battle_id=" << battleId << "(reaper will release the freeze at the deadline)";
		}
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
	//   deadline_ms 缺失时按锁剩余 TTL 反推(锁 TTL = 冻结到期时刻 + 60s 余量,FIGHTING 的冻结到期又比 deadline
	//   晚终局提交宽限 + 收件箱投递余量,见 FreezeExpiresAtMs;反推只会偏晚、不会更早,宁可多冻一会儿),
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
			const uint64_t ttlMs = lockTtlSec > 0 ? static_cast<uint64_t>(lockTtlSec) * 1000 : 0;
			rebuilt.set_deadline_ms(nowMs + ttlMs);
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

	// 结算掉落按 config 汇总:跳过空项,数量夹到 uint32。只给号段预检用。
	ItemCountMap CollectGainedItems(const ::BattleSettlementData& settlement)
	{
		ItemCountMap gained;
		for (const auto& item : settlement.items_gained())
		{
			if (item.item_table_id() == 0 || item.count() == 0)
			{
				continue;
			}
			// BattleItemEntry.count 是 uint64,ItemCountMap 的值是 uint32:先夹到上限,
			// 否则高位截断会把一个天文数字变成小数量。同一 config 多行累加时也按上限饱和。
			const uint64_t clamped =
				std::min<uint64_t>(item.count(), std::numeric_limits<uint32_t>::max());
			auto& total = gained[item.item_table_id()];
			total = static_cast<uint32_t>(
				std::min<uint64_t>(uint64_t{total} + clamped, std::numeric_limits<uint32_t>::max()));
		}
		return gained;
	}

	// item 号段此刻能不能为掉落铸齐新实例的身份(审计 cpp-economy#0)。
	// 号段发不出号时,BagService 会整批拒绝入包。在任何副作用之前判:不够就整笔 DEFER,条目留在收件箱。
	// 这与资产通道 ApplyCredit 的号段预检同一纪律。上界见 BagService::MaxNewInstancesFor:
	// 主背包放不下时一个号都不铸,转投临时格最多也只铸这么多,一份余量就覆盖两段式发放。
	// 号段双 buffer 加后台预取,续段通常是秒级的事,收件箱的 DEFER 退避足以接上。
	// 只读号段余量,不发号。
	bool HasItemGuidsForDrops(const ItemCountMap& gained, const uint64_t playerId, const uint64_t battleId)
	{
		if (gained.empty())
		{
			return true;
		}
		const uint64_t needed = BagService::MaxNewInstancesFor(gained);
		const uint64_t available = tlsGuidSegmentRegistry.Get(GuidKind::kItem).Available();
		if (available >= needed)
		{
			return true;
		}
		LOG_WARN << "[PlayerBattle] metric=battle_settlement_item_guid_shortage player_id=" << playerId
				 << " battle_id=" << battleId << " needed=" << needed << " available=" << available
				 << ",item guid segment cannot cover drops;whole settlement deferred";
		return false;
	}

	// 推 BattleEndS2C(结算入账通知):个人终局只由 scene 在结算随存档提交之后推,battle 节点不发。
	void PushBattleEndToPlayer(entt::entity player, const ::BattleSettlementData& settlement)
	{
		::BattleEndS2C message;
		message.set_battle_id(settlement.battle_id());
		message.set_outcome(settlement.outcome());
		*message.mutable_settlement() = settlement;
		SendMessageToClientViaGate(BattleClientPlayerNotifyBattleEndMessageId, message, player);
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
	//   * 只取主背包:仓库/装备栏不参战;临时格(kTemporary)是掉落溢出缓冲,会 FIFO
	//     淘汰旧物 —— 把它计进副本,战斗中一次掉落就可能把"账面上还有的药"挤掉,
	//     于是结算扣除按实际持有夹紧时凭空少扣,白赚一次回血。
	//   * 这是一份**数量副本**,不带 guid/格子:实例身份归 scene(战斗掉落 iid 归属决策),
	//     战斗服只拿可丢弃的派生副本(D1)。
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
		auto* itemEntry = snapshot.add_items();
		itemEntry->set_item_table_id(configId);
		itemEntry->set_count(count);
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

	// 上面已确认没有冻结且快照完整，此处才创建本次备战状态。
	auto& inBattle = tlsEcs.actorRegistry.emplace<InBattleComp>(player);
	inBattle.set_battle_id(battleId);
	inBattle.set_battle_node_id(request.battle_node_id());
	inBattle.set_deadline_ms(request.deadline_ms());
	inBattle.set_prepare_deadline_ms(prepareDeadlineMs);
	inBattle.set_state(IN_BATTLE_STATE_PREPARING);
	// 记下写进快照 routing 的会话号(BuildBattleSnapshot 已保证非 0),ConfirmBattle 据此判断
	// 备战期间是否换过会话。emplace_or_replace:正常不会残留(随 RemoveInBattleComp 摘),防御性覆盖。
	tlsEcs.actorRegistry.emplace_or_replace<BattlePrepareSessionComp>(
		player, BattlePrepareSessionComp{response.snapshot().routing().session_id()});

	// 指纹与快照同值(match 用响应字段比对,battle 用快照字段比对)
	response.set_table_fingerprint(turnbattle::BattleTableFingerprint::Current());

	// SET battle:lock:{player_id}=battle_id EX(prepare_deadline+60s)。
	// 备战期锁只需活到备战作废期限;CreateBattle 确认后 ConfirmBattle 再按正式 deadline 条件续期。
	// 咨询性锁(match JoinQueue 读),fire-and-forget:失败只影响 match 的提前拒绝,
	// 权威判定仍是本节点的 InBattleComp,不影响正确性。
	const uint64_t nowMs = TimeSystem::NowMillisecondsUTC();
	const uint64_t ttlSec = LockTtlSecFor(prepareDeadlineMs, nowMs);
	if (RedisReady())
	{
		const int lockRet = tlsPlayerRedis.command(
			[playerId](hiredis::Hiredis*, redisReply* reply) {
				if (reply == nullptr || reply->type == REDIS_REPLY_ERROR)
				{
					LOG_ERROR << "[PlayerBattle] SET battle:lock failed, player_id=" << playerId;
				}
			},
			(std::string("SET ") + kBattleLockKeyFmt + " %llu EX %llu").c_str(),
			playerId, battleId, ttlSec);
		// 伴生 ctx(同 TTL):玩家完整下线再登录 / 备战到期后迟到的确认,靠它重建 InBattleComp
		// (battle_node_id 只有这里知道,BattleConfirmedEvent 不带)
		const std::string ctxPayload = SerializeBattleCtx(inBattle);
		const int ctxRet = tlsPlayerRedis.command(
			[playerId](hiredis::Hiredis*, redisReply* reply) {
				if (reply == nullptr || reply->type == REDIS_REPLY_ERROR)
				{
					LOG_ERROR << "[PlayerBattle] SET battle:ctx failed, player_id=" << playerId
							  << "(login reconstruction will fall back to a conservative freeze without battle_node_id)";
				}
			},
			(std::string("SET ") + kBattleCtxKeyFmt + " %b EX %llu").c_str(),
			playerId, ctxPayload.data(), ctxPayload.size(), ttlSec);
		if (lockRet != REDIS_OK || ctxRet != REDIS_OK)
		{
			LOG_WARN << "[PlayerBattle] SET battle:lock/ctx not sent, player_id=" << playerId
					 << " battle_id=" << battleId << "(match advisory checks are temporarily blind;InBattleComp remains authoritative)";
		}
	}
	else
	{
		LOG_WARN << "[PlayerBattle] SET battle:lock skipped(Redis disconnected), player_id=" << playerId
				 << " battle_id=" << battleId << "(match advisory checks are temporarily blind;InBattleComp remains authoritative)";
	}

	LOG_INFO << "[PlayerBattle] Battle preparation freeze completed: player_id=" << playerId
			 << " battle_id=" << battleId
			 << " battle_node_id=" << request.battle_node_id()
			 << " deadline_ms=" << request.deadline_ms()
			 << " prepare_deadline_ms=" << prepareDeadlineMs
			 << " lock_ttl_sec=" << ttlSec
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
		if (event.deadline_ms() == 0)
		{
			LOG_WARN << "[PlayerBattle] ConfirmBattle: player offline and deadline_ms=0;renewal skipped, player_id="
					 << playerId << " battle_id=" << battleId;
			return;
		}
		if (!RedisReady())
		{
			LOG_WARN << "[PlayerBattle] ConfirmBattle: player offline and Redis disconnected;renewal skipped, player_id="
					 << playerId << " battle_id=" << battleId;
			return;
		}
		const uint64_t deadlineMs = event.deadline_ms();
		const int ret = tlsPlayerRedis.command(
			[playerId, battleId, deadlineMs](hiredis::Hiredis*, redisReply* reply) {
				if (reply == nullptr || reply->type == REDIS_REPLY_ERROR)
				{
					LOG_ERROR << "[PlayerBattle] ConfirmBattle lock/ctx read EVAL failed, player_id=" << playerId
							  << " battle_id=" << battleId;
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
				ConfirmBattleLockIfMatch(playerId, battleId, ttlSec, SerializeBattleCtx(ctx));
				LOG_INFO << "[PlayerBattle] ConfirmBattle: player offline;renew conditionally by lock value and persist ctx, player_id="
						 << playerId << " battle_id=" << battleId << " deadline_ms=" << deadlineMs
						 << " battle_node_id=" << ctx.battle_node_id() << " lock_ttl_sec=" << ttlSec;
			},
			(std::string("EVAL %s 2 ") + kBattleLockKeyFmt + " " + kBattleCtxKeyFmt + " %llu").c_str(),
			kGetCtxIfLockMatchScript, playerId, playerId, battleId);
		if (ret != REDIS_OK)
		{
			LOG_WARN << "[PlayerBattle] ConfirmBattle: player offline and lock/ctx read not sent;renewal skipped, player_id="
					 << playerId << " battle_id=" << battleId;
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
	if (inBattle->state() != IN_BATTLE_STATE_PREPARING)
	{
		// Kafka at-least-once 重投 / battle 侧周期补发:已 FIGHTING,幂等忽略
		LOG_INFO << "[PlayerBattle] ConfirmBattle: already in " << eInBattleState_Name(inBattle->state())
				 << ",idempotently ignored, player_id=" << playerId << " battle_id=" << battleId;
		return;
	}

	// PREPARING -> FIGHTING:作废期限切到正式 deadline(event 没填时沿用备战时的 deadline_ms)
	const uint64_t deadlineMs = event.deadline_ms() != 0 ? event.deadline_ms() : inBattle->deadline_ms();
	inBattle->set_state(IN_BATTLE_STATE_FIGHTING);
	inBattle->set_deadline_ms(deadlineMs);

	const uint64_t ttlSec = FightingLockTtlSecFor(deadlineMs, nowMs);
	ConfirmBattleLockIfMatch(playerId, battleId, ttlSec, SerializeBattleCtx(*inBattle));

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

			// 续期到冻结到期时刻(正式 deadline + 宽限 + 余量)+ 覆写 ctx,同时复核锁仍是本战斗的(否则撤销重建)
			const uint64_t ttlSec = FightingLockTtlSecFor(rebuilt.deadline_ms(), nowMs);
			ConfirmRebuiltFreeze(player, playerId, battleId, ttlSec, SerializeBattleCtx(rebuilt));

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

			// 复核锁仍是本战斗的(登录窗口内恰好确认的结算已删锁 -> 撤销重建);
			// TTL 按重建后冻结的到期时刻算(与 reaper 同一个判据):PREPARING 看 prepare_deadline,
			// FIGHTING 看 deadline + 终局提交宽限 + 收件箱投递余量(FreezeExpiresAtMs)
			const uint64_t expiresAtMs = FreezeExpiresAtMs(rebuilt.state() == IN_BATTLE_STATE_PREPARING,
														   rebuilt.deadline_ms(), rebuilt.prepare_deadline_ms());
			ConfirmRebuiltFreeze(player, playerId, battleId, LockTtlSecFor(expiresAtMs, nowMs),
								 SerializeBattleCtx(rebuilt));

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

// 一项因配置行已不存在而被剔除的结算物品(只供成功应用之后打日志)。
struct SkippedSettlementItem
{
    std::string side;
    uint32_t configId = 0;
    uint32_t count = 0;
};

// 掉落 / 消耗项的配置行已被热更删掉(C-2,2026-10-30)。这份终局是开局快照与引擎早已定下的事实,
// 配置行之后被删既不是版本错位,也不会自愈:整笔 DEFER 会让这一局的金币、任务、HP 与其余物品永远卡在收件箱里,
// 判版本错位则连同之后每一局一起停用整个战斗结算域。所以只把这些项从整笔候选里剔除,其余效果照常应用,
// 调用方在应用成功后逐项记 ERROR(metric=battle_inbox_item_effect_skipped),交人工按流水补偿。
// 只认「行不存在」:行还在、但行或玩家实例带本版本不认识的字段 / 实例不可计数,仍按版本错位处理
// (HasUnsupportedInboxConsumption)。按配置号升序处理,日志顺序确定。
void DropItemsWithoutConfigRow(ItemCountMap& items, const char* side, std::vector<SkippedSettlementItem>& skipped)
{
    for (const auto& [configId, count] : SortedByConfigId(items))
    {
        if (ItemTableManager::Instance().Exists(configId)) continue;
        skipped.push_back(SkippedSettlementItem{side, configId, count});
        items.erase(configId);
    }
}

using SettlementCurrencyGains = std::map<CurrencyType, uint64_t>;

// 新集合优先；只有集合为空才解释旧金币字段。全笔币种/金额/去重校验不产生副作用。
bool CollectSettlementCurrencyGains(const ::BattleSettlementData& settlement, SettlementCurrencyGains& gains)
{
    if (settlement.currency_gains_size() > 0) {
        for (const auto& gain : settlement.currency_gains()) {
            if (gain.currency_type() >= static_cast<uint32_t>(kCurrencyMax) ||
                gain.amount() > static_cast<uint64_t>(std::numeric_limits<int64_t>::max()) ||
                !gains.emplace(static_cast<CurrencyType>(gain.currency_type()), gain.amount()).second) return false;
        }
    } else if (settlement.gold_gain() > 0) {
        if (settlement.gold_gain() > static_cast<uint64_t>(std::numeric_limits<int64_t>::max())) return false;
        gains.emplace(kCurrencyGold, settlement.gold_gain());
    }
    return true;
}

// Inbox整批提交前按每个币种复核净入账额；未知债务和封禁继续保留原件，不部分提交。
bool CanCreditWholeBattleCurrency(entt::entity player, CurrencyType type, uint64_t amount)
{
    if (amount == 0) return true;
    const auto* balances = tlsEcs.actorRegistry.try_get<CurrencyComp>(player);
    const auto* currency = tlsEcs.actorRegistry.try_get<PlayerCurrencyComp>(player);
    if (!balances || !currency || amount > static_cast<uint64_t>(std::numeric_limits<int64_t>::max()) ||
        GainBlockService::IsGainBlocked(GainBlockService::GainType::kCurrency, static_cast<uint32_t>(type)) ||
        CurrencySystem::IsCurrencyBlocked(player, type)) return false;
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
        // 配置行已被热更删掉不是版本错位(C-2):这一项由 ApplySettlementToEntity 剔除并记 ERROR,这里跳过;
        // 行还在却带本版本不认识的字段,才是版本错位。
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
        if (!row) { ++skipped["pet_table_missing"]; continue; }
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
    if (HasUnsupportedInboxConsumption(player, settlement)) return {PlayerInboxExecKind::kVersionSkew, 0};
    SettlementCurrencyGains currencyGains;
    if (!CollectSettlementCurrencyGains(settlement, currencyGains)) return {PlayerInboxExecKind::kDefer, 0};
    for (const auto& [type, amount] : currencyGains)
        if (amount > 0 && CurrencySystem::DebtCarriesUnknownFields(player, type))
            return {PlayerInboxExecKind::kVersionSkew, 0};
    if (!ApplySettlementToEntity(player, settlement)) return {PlayerInboxExecKind::kDefer, 0};
    return {PlayerInboxExecKind::kApplied, 0};
}

bool PlayerBattleSystem::CompleteInboxSettlement(entt::entity player, const std::string& payload)
{
    ::BattleSettlementData settlement;
    if (!tlsEcs.actorRegistry.valid(player) || !settlement.ParseFromString(payload) ||
        HasNestedUnknownFields(settlement) || settlement.player_id() != tlsEcs.GetPlayerId(player) ||
        settlement.battle_id() == 0) return false;
    // SQL确认之后才公开终局/解冻/放锁；旧局不能修改已经开始的新局HP或摘新局组件。
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
    if (!tlsEcs.actorRegistry.all_of<DerivedAttributesComp>(player) ||
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
    if (!CollectSettlementCurrencyGains(settlement, currencyGains)) {
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
    std::vector<SkippedSettlementItem> skippedItems;
    std::string originalPets;
    if (!HasCompleteBattleEffectInputs(player, settlement, currencyGains) ||
        !CollectExactSettlementItems(settlement, inboxConsumed, inboxGained)) return false;
    if (!CollectApplicableInboxPets(player, settlement, inboxPets, skippedPets)) return false;
    // 配置行已被热更删掉的掉落 / 消耗只剔除这一项,不拖住整笔(C-2,DropItemsWithoutConfigRow 的说明)。
    // 必须在下面按候选算实际可扣量之前做:actualConsumption 与 ConsumeInventoryAndGrant 只看剔除后的两张表。
    DropItemsWithoutConfigRow(inboxConsumed, "consumed", skippedItems);
    DropItemsWithoutConfigRow(inboxGained, "gained", skippedItems);
    if (settlement.pets_size() > 0) originalPets = tlsEcs.actorRegistry.get<PlayerPetComp>(player).SerializeAsString();
    if (!inboxConsumed.empty()) {
        const auto& inventory = tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory];
        for (const auto& [config, count] : inboxConsumed)
            actualConsumption[config] = static_cast<uint32_t>(std::min<uint64_t>(count, inventory.GetTotalItemCount(config)));
    }

    // 掉落要铸的新实例身份必须先有着落(审计 cpp-economy#0)。号段暂缺是会自己消失的条件,
    // 与跨区冻结同类:整笔 DEFER、零副作用,条目留在收件箱。
    if (!HasItemGuidsForDrops(CollectGainedItems(settlement), playerId, battleId))
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

    // scene EventLoop内同步执行且不让出；全部可拒绝条件已在首笔入账前复核。
    const auto creditCurrencies = [&]() -> uint32_t {
        for (const auto& [type, amount] : currencyGains) {
            if (amount == 0) continue;
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
            !tlsEcs.actorRegistry.all_of<BaseAttributesComp, DerivedAttributesComp>(player)) return kInvalidParameter;
        return creditCurrencies();
    };
    const auto result = inboxConsumed.empty() && inboxGained.empty() ? credit() :
        BagService::ConsumeInventoryAndGrant(player, inboxConsumed, inboxGained, battleId,
            "{\"source\":\"battle-inbox\"}", credit);
    if (result != kSuccess) return false;
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
    for (const auto& skipped : skippedItems)
        LOG_ERROR << "metric=battle_inbox_item_effect_skipped reason=item_table_missing side=" << skipped.side
                  << " count=" << skipped.count << " config_id=" << skipped.configId
                  << " battle_id=" << battleId << " player_id=" << playerId
                  << ";item config row no longer exists, this item effect was dropped and the rest of the settlement applied";
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
	SendMessageToClientViaGate(BattleClientPlayerNotifyBattleReconnectMessageId, message, player);

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
	// 这一局若其实 COMPLETE 了(battle 崩溃之外,收件箱迟迟没投到),结算到达时照常应用:结算不需要冻结在场。
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
		LOG_WARN << "[PlayerBattle] metric=battle_freeze_expired player_id=" << playerId
				 << " battle_id=" << entry.battleId
				 << " deadline_ms=" << entry.deadlineMs
				 << " expires_at_ms=" << entry.expiresAtMs
				 << " now_ms=" << nowMs
				 << ",battle invalidated and freeze released after the terminal grace and inbox delivery margin(battle node crashed or settlement lost)";
		ReleaseExpiredFightingFreeze(entry.entity, playerId, entry.battleId);
	}
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
