#include "player_battle.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "battle_settlement_application_cache.h"
#include "battle_settlement_ledger.h"

#include <muduo/base/Logging.h>
#include <muduo/net/EventLoop.h>
#include <muduo/net/TimerId.h>
#include <muduo/contrib/hiredis/Hiredis.h>
#include <hiredis/hiredis.h>

#include <algorithm>
#include <chrono>
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
#include "thread_context/redis_manager.h"

#include "actor/attribute/constants/actor_state_attribute_calculator_constants.h"
#include "actor/attribute/system/actor_attribute_calculator.h"
#include "combat/buff/comp/buff_comp.h"
#include "combat/buff/constants/buff.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/gain_block/gain_block_service.h"
#include "modules/currency/system/currency_system.h"
// tlsGuidSegmentRegistry:掉落入包前预检 item 号段余量(审计 cpp-economy#0)。
#include "modules/id_segment/guid_segment_registry.h"
#include "modules/mission/comp/mission_comp.h"
#include "player/comp/player_frozen_comp.h"
// PlayerTravelHandoffComp:交接一挂上 SavePlayerToRedis 就硬性跳过写盘,此刻应用等于落不了盘。
#include "player/comp/player_ownership_comp.h"
// PlayerLastPersistedSnapshotComp:上一次**确实写进 Redis** 的那份 PlayerAllData。
// 它仅是SQL确认的候选；销账还须同E/q的双SQL ACK，不另建可漂移的持久化水位。
#include "player/comp/last_persisted_snapshot_comp.h"
#include "player/system/player_lifecycle.h"  // IsCrossZoneFrozen / SavePlayerToRedis / IsSceneChangeBusy
#include "player/system/role_admission_system.h"
#include "player/system/player_mission.h"
#include "core/system/redis.h"  // tlsRedisSystem:判断该玩家是否还有未落地的存盘
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

// 时间->回合换算与回合常量的权威定义在回合引擎库(scene 可以依赖 battle 常量,反向禁止)。
#include "services/battle/constants/turn_battle_constants.h"
// 待结算记录的 Redis key / Lua 契约(R07)。battle 侧写、scene 侧读与销账,
// 两端必须用**同一份**常量 —— 手抄两遍就是等着某天改一处漏一处。
#include "services/battle/settlement/settlement_outbox.h"
#include "services/battle/settlement/settlement_reply_shape.h"
// 战斗配表指纹(七张战斗表确定性序列化 sha256,随快照携带给 match/battle 比对)。
#include "services/battle/data/battle_table_fingerprint.h"

#include "table/code/buff_table.h"
#include "table/code/item_table.h"
#include "table/code/pet_table.h"
#include "table/code/attributepool_table.h"
#include "table/code/skill_table.h"
#include "table/proto/tip/asset_error_tip.pb.h"  // kAssetBlocked:金币入账的终局拒绝(审计 cpp-economy#1)
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
#include "proto/common/event/battle_event.pb.h"                       // BattleSettlementEvent
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
	//   battle:settlement:pending:{player_id}    = event —— 待结算记录(不过期,landing-recovery §7)
	//   battle:settlement:pending:id:{player_id} = battle_id —— 伴生 id(与上一条同 SET/同 DEL)
	//   battle:settlement:pending:index          = ZSET(member=player_id,score=写入毫秒)—— 积压索引,
	//       只用于 battle 节点报数;写 pending 的脚本同时 ZADD,销账的脚本同时 ZREM。
	//   读写 pending 的 Lua 全部定义在 services/battle/settlement/settlement_outbox.h,这里只引用。
	//
	//   待结算记录的写者从"只有 scene 的离线分支"变成了"battle 结算时必写"(R07):
	//   battle 先落库再投递,scene 应用后**条件销账**(id 匹配才删),未销账的由 battle
	//   有界重投。因此 scene 的每一条"结算已定局"路径都必须销账 —— 漏一条的后果是
	//   battle 一直重投到次数用尽,而且玩家下次登录会被登录钩子**再发一次奖励**。
	// 锁键 battle 节点也要读(确认模式落库判「已被别的局取代」),所以与 pending 键一样以 settlement_outbox.h 为准。
	constexpr const char *kBattleLockKeyFmt = battle_settlement::kBattleLockKeyFmt;
	constexpr char kBattleCtxKeyFmt[] = "battle:ctx:%llu";
	constexpr const char *kPendingSettlementKeyFmt = battle_settlement::kPendingSettlementKeyFmt;
	constexpr const char *kPendingSettlementIdKeyFmt = battle_settlement::kPendingSettlementIdKeyFmt;
	constexpr const char *kPendingSettlementIndexKey = battle_settlement::kPendingSettlementIndexKey;

	// speed 缺配兜底:BaseAttributesComp.speed 为 0(存量数据未配置)时的默认出手速度。
	// 高于引擎的怪物默认速度(kMonsterDefaultSpeed=60),保证缺配玩家不至于永远后手。
	// 2026-09-14 速度单位 ×12(原 10),与引擎常量一起放大
	constexpr uint64_t kFallbackBattleSpeed = 120;

	// reaper 定时器(单定时器形态:整个进程只有一个,StartReaper / StopReaper 成对)
	muduo::net::TimerId gReaperTimerId;
	bool gReaperActive = false;
	muduo::net::EventLoop* gReaperLoop = nullptr;

    // 场景 loop 内跨实体生命周期的有限去重，覆盖正常下线重建与 Redis DEL 尚未完成窗口。
    // 不持久化，不承诺进程崩溃后的 exactly-once；已完成项最多保留 7 天(battle_settlement_application_cache.h)。
    // pending 已不再过期(landing-recovery §7),但两者无需同值:这层缓存不是权威,
    // 「这一局是否已应用」的权威是持久账本 BattleSettlementLedgerComp(ApplySettlementToEntity 先判它)。
    thread_local battle_settlement::SettlementApplicationCache gSettlementApplications;

	// 结算应用后的存盘入口(见 PlayerBattleSystem::SetPersistFnForTest)。
	PlayerBattleSystem::PersistFn gPersistFn = &PlayerLifecycleSystem::SavePlayerToRedis;

	bool RedisReady()
	{
		auto& redis = tlsRedis.GetZoneRedis();
		return redis && redis->connected();
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

	// 登录复查标记(2026-10-16):登录钩子第一步(读待结算记录,没有再读锁重建冻结)「没问到」的玩家。
	// 没问到 = 空应答(连接断开 / 被释放:主切换掐连接、存活探测摘连接)、读待结算记录时的 ERROR、
	// 命令没发出去,或者当时 Redis 根本不可用。它不能当成结论:
	//   - 当成「没有挂起结算」→ 这次登录漏补发奖励(记录不过期,但要等下一次登录;一直在线就一直拿不到);
	//   - 当成「没有锁」→ 战斗在途却不重建冻结,战斗闸(资产通道、收件箱 requiresOutOfBattle)放行。
	// 标记之后由 reaper 节拍(RunLoginRechecks,30 秒)在 Redis 可用时重跑整个第一步;复查完成前不按锁重建冻结
	// (重建依赖「确实没有挂起结算」这个前提)。重跑之前先摘标记,这一轮又没问到就重新挂上,所以同一玩家同一时刻
	// 最多一条复查在途。
	// 纯运行时标记:不入库(实体重建时登录钩子本来就会重跑),随实体销毁。唯一写入口 MarkLoginRecheck,
	// 唯一消费口 RunLoginRechecks。没有用 Redis 重连回调触发:那是 RedisSystem 独占的单槽,复查等一个 reaper 节拍足够。
	struct BattleLoginRecheckComp
	{
	};

	// reaper 按期限判作废(FIGHTING 到期)的最近一局(2026-10-19,第二、三轮评审 P3)。
	// 到期时 reaper 条件删锁;删锁没落地(Redis 抖)时锁还在,之后任何一条按锁重建的路径(冻结到期补读挂的复查、
	// 补应用另一局挂起结算之后的重建、登录挂的复查赶上冻结到期)都会把玩家冻回这场已结束的战斗。
	// 按锁重建之前先比这里(settlement_outbox.h 的 ClassifyLockRebuild):锁值等于它就不重建,补一次条件删锁。
	// 单槽够用:锁还在时 match 对锁 fail-closed,玩家排不进下一局;锁没了才可能有下一局,那时旧值已无锁可比。
	// 只记 FIGHTING 到期:PREPARING 到期刻意保留锁,等迟到的确认按锁重建 FIGHTING(reaper 的说明)。
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
	// 本实体的作废槽位 + 活账本 → ClassifyLockRebuild。单测经 PlayerBattleSystem::DecideLockRebuildFor 走同一个函数。
	battle_settlement::LockRebuildDecision DecideLockRebuild(entt::entity player, const uint64_t lockBattleId,
															 const BattleSettlementLedgerComp& ledger)
	{
		return battle_settlement::ClassifyLockRebuild(lockBattleId, ExpiredFightingBattleIdOf(player),
													  battle_settlement::HasApplied(ledger, lockBattleId));
	}

	void MarkLoginRecheck(entt::entity player, const uint64_t playerId, const char* reason)
	{
		if (!tlsEcs.actorRegistry.valid(player))
		{
			return;
		}
		LOG_WARN << "[PlayerBattle] metric=battle_login_recheck_scheduled player_id=" << playerId << " reason=" << reason
				 << ",login battle check could not reach a conclusion;pending settlement and freeze restore deferred to the next reaper tick";
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
	// 补一次战斗中被跳过的跟随检查。所有解冻路径(结算、取消、备战作废、战斗作废、
	// 重建撤销、重复结算销账)都必须走这里,漏一处就漏一次跟随。
	// 调用方若还要条件删锁,应先发删锁命令再调本函数:跟随链会读 battle:lock fail-closed,
	// 删锁先入 hiredis 管道可保证跟随链读到的是删锁之后的状态。
	//
	// 同一处还发一次资产待办唤醒(landing-recovery-sync-digest.md §5.2):资产通道在战斗中回 RETRY
	// (asset_op_system.cpp 的 kAssetInBattle 只看 InBattleComp 在不在,PREPARING 与 FIGHTING 都算),
	// 组件一摘就能应用,不必等最长 60 秒的退避。设计稿写的挂点是 ClearBattleFreeze 之后,但最常见的
	// 解冻 —— 结算应用(ReleaseFreezeKeepLock)—— 并不经过 ClearBattleFreeze,挂在这个唯一入口上才不漏。
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
		PlayerTeamSystem::OnBattleFreezeCleared(player);
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

	// 落盘确认后的销账 + 放锁(同时 ZREM 积压索引),一条 Lua 原子完成。
	// 键位、返回值与「为什么两件事必须在同一条脚本里」见 settlement_outbox.h 的 kAckSettlementScript。
	constexpr const char *kAckSettlementScript = battle_settlement::kAckSettlementScript;
	// 离线暂存:锁值 == battle_id 才写 pending 两键(不过期)并 ZADD 积压索引,一条 Lua 原子完成。
	// 键位、返回值与「为什么不能先 GET 锁、回调里再 SET」见 settlement_outbox.h 的
	// kStorePendingSettlementIfLockMatchScript。
	constexpr const char *kStorePendingIfLockMatchScript = battle_settlement::kStorePendingSettlementIfLockMatchScript;
	// 结算已应用(内存)后续锁:锁值 == battle_id 且剩余 TTL < ARGV[2] 秒时,把锁与 ctx 一起续到 ARGV[2]。
	// 只延长、不缩短;返回 1 = 锁属于本局,0 = 锁不在或已易主。
	constexpr char kHoldLockIfMatchScript[] =
		"if redis.call('GET', KEYS[1]) ~= ARGV[1] then return 0 end "
		"local t = redis.call('TTL', KEYS[1]) "
		"if t >= 0 and t < tonumber(ARGV[2]) then "
		"redis.call('EXPIRE', KEYS[1], ARGV[2]); redis.call('EXPIRE', KEYS[2], ARGV[2]) end "
		"return 1";

	// 锁 TTL(秒)= 期限剩余 + 余量:锁必须活得比 InBattleComp 久
	uint64_t LockTtlSecFor(const uint64_t deadlineMs, const uint64_t nowMs)
	{
		const uint64_t remainSec = deadlineMs > nowMs ? (deadlineMs - nowMs) / 1000 : 0;
		return remainSec + PlayerBattleSystem::kLockExtraTtlSec;
	}

	// 条件删锁(连带 ctx):锁值 == battleId 才 DEL(所有删锁路径统一走这里,防迟到的取消/结算/作废
	// 误删玩家下一场战斗的锁)。fire-and-forget:锁是咨询性的,失败最迟随 TTL 过期。
	//
	// refollowOnDelete(默认不填):锁**确实被这条命令删掉**、且那时实体仍是这个玩家、身上没有 InBattleComp 时,
	// 补一次组队跟随检查。给「解冻早已发生、锁现在才真正放掉」的调用方用(按锁重建守卫 kSkipExpired 的补删):
	// 解冻时 RemoveInBattleComp 触发的跟随链读到锁还在(或读失败)会 fail-closed 跳过,之后没有别的东西再触发跟随,
	// 队员会一直留在原场景。与 ClearPendingSettlementIfMatch「锁此刻才真正放掉,补一次跟随检查」同一约定。
	// 先删锁再摘组件的调用方(ClearBattleFreeze 等)不要填:摘组件那一刻已经触发了跟随,而且排在删锁之后(同连接 FIFO)。
	void DeleteBattleLockIfMatch(const uint64_t playerId, const uint64_t battleId,
								 const entt::entity refollowOnDelete = entt::null)
	{
		if (!RedisReady())
		{
			// 非致命:锁带 EX,最迟随 TTL 过期;记日志说明串行化窗口靠 TTL 兜底
			LOG_WARN << "[PlayerBattle] Conditional lock deletion skipped(Redis disconnected), player_id=" << playerId
					 << " battle_id=" << battleId << ", lock will expire with its TTL";
			return;
		}
		// entt::entity 用 init-capture(MSVC C3495,见 copilot-instructions)。
		tlsRedis.GetZoneRedis()->command(
			[playerId, battleId, refollow = refollowOnDelete](hiredis::Hiredis*, redisReply* reply) {
				if (reply == nullptr || reply->type == REDIS_REPLY_ERROR)
				{
					LOG_ERROR << "[PlayerBattle] Conditional lock deletion EVAL failed, player_id=" << playerId
							  << " battle_id=" << battleId;
					return;
				}
				if (reply->type == REDIS_REPLY_INTEGER && reply->integer == 0)
				{
					// 锁不存在(已过期/已删)或已易主(玩家进了下一场):都不是错误
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
				PlayerTeamSystem::OnBattleFreezeCleared(refollow);
			},
			(std::string("EVAL %s 2 ") + kBattleLockKeyFmt + " " + kBattleCtxKeyFmt + " %llu").c_str(),
			kDeleteLockIfMatchScript, playerId, playerId, battleId);
	}

	// 结算销账(R07):battle:settlement:pending:{id} 的值属于 battleId 才删两键,
	// 同一条 Lua 里 battle:lock 的值属于 battleId 也一并删掉(连 ctx)。
	// 销账就是 battle 侧发件箱的 ACK —— battle 每 10s 探测一次伴生 id 键,值不再等于
	// 自己的 battle_id 就停止重投。锁跟着一起放,是因为结算应用后锁被刻意留到这一刻
	// (见 ReleaseFreezeKeepLock):锁在,就证明这一局还没完。
	//
	// 必须**条件删**而不是无脑 DEL:玩家打完这一局立刻进下一局、下一局又结算时,
	// 一条迟到的旧结算若无条件删,就把**新一局**的待结算记录/锁抹掉了。
	// 删 pending 的同一段 Lua 里 ZREM 积压索引(landing-recovery §7);pending 已不属于本局时不动索引。
	// fire-and-forget:失败时 pending 留着(不再过期),代价是 battle 多重投几轮,账本条目保留给下一轮重试。
	// **只能经 AckSettlementPending 调用**,那里决定此刻能不能销账。
	void ClearPendingSettlementIfMatch(const uint64_t playerId, const uint64_t battleId)
	{
		const auto originalPlayer = tlsEcs.GetPlayer(playerId);
		const auto originalIdentity = RoleAdmissionSystem::ConfirmedIdentity(playerId);
		const auto originalIdentityBytes = originalIdentity ? originalIdentity->SerializeAsString() : std::string{};
		if (!RedisReady())
		{
			LOG_WARN << "[PlayerBattle] Settlement acknowledgment skipped(Redis disconnected), player_id=" << playerId
					 << " battle_id=" << battleId << ",battle will redeliver until retries are exhausted(settlement itself is idempotent)";
			return;
		}
		tlsRedis.GetZoneRedis()->command(
			[playerId, battleId, originalPlayer = originalPlayer, originalIdentityBytes](hiredis::Hiredis*, redisReply* reply) {
				if (reply == nullptr || reply->type != REDIS_REPLY_INTEGER || reply->integer < 0 || reply->integer > 3)
				{
					LOG_ERROR << "[PlayerBattle] Settlement acknowledgment EVAL failed, player_id=" << playerId
							  << " battle_id=" << battleId;
					return;  // 账本条目保留:下一轮重投/reaper 会再来一次
				}
				// 条件删已经落地(删掉了,或记录已不属于本局 —— 两种都意味着本局的 pending
				// 再也不会被任何人应用)。此刻、且只有此刻,才能摘掉账本条目:提前摘会让
				// 「销账其实没落地 + 重启」退回重复发奖(§9.3 的复制窗口)。
				const bool lockReleased = reply->type == REDIS_REPLY_INTEGER && (reply->integer & 2) != 0;
				const auto owner = tlsEcs.GetPlayer(playerId);
				if (!tlsEcs.actorRegistry.valid(owner)) return;
				const auto currentIdentity = RoleAdmissionSystem::ConfirmedIdentity(playerId);
				const auto* admission = tlsEcs.actorRegistry.try_get<PlayerRoleAdmissionComp>(owner);
				if (owner != originalPlayer || !tlsEcs.actorRegistry.valid(owner) || tlsEcs.GetPlayerId(owner) != playerId
					|| originalIdentityBytes.empty() || !currentIdentity || currentIdentity->SerializeAsString() != originalIdentityBytes
					|| admission == nullptr || admission->generation != currentIdentity->generation()
					|| admission->ownerRevision != currentIdentity->owner_revision()) return;
				if (auto* ledger = tlsEcs.actorRegistry.try_get<BattleSettlementLedgerComp>(owner);
					ledger != nullptr && battle_settlement::ForgetApplied(*ledger, battleId))
				{
					LOG_DEBUG << "[PlayerBattle] Settlement ledger entry removed(acknowledgment persisted), player_id=" << playerId
							  << " battle_id=" << battleId;
				}
				// 解冻时(RemoveInBattleComp)组队跟随链读到锁还在会 fail-closed 跳过;锁此刻才真正放掉,
				// 补一次跟随检查。锁已删在本条回复之前执行,跟随链随后的读看到的必然是删锁之后的状态。
				if (lockReleased && !tlsEcs.actorRegistry.any_of<InBattleComp>(owner))
				{
					PlayerTeamSystem::OnBattleFreezeCleared(owner);
				}
			},
			(std::string("EVAL %s 5 ") + kPendingSettlementKeyFmt + " " + kPendingSettlementIdKeyFmt + " " +
			 kBattleLockKeyFmt + " " + kBattleCtxKeyFmt + " " + kPendingSettlementIndexKey + " %llu %llu").c_str(),
			kAckSettlementScript, playerId, playerId, playerId, playerId, battleId, playerId);
	}

	// ── 结算幂等的持久基底(turn-battle-gap-closure.md §9.3)──────────────────
	//
	// 改造前:幂等只有进程内的 thread_local 应用缓存,而「应用 → 当场销账」之间隔着
	// 一次周期存盘(默认 300s)。进程死在这个窗口里,奖励就永久没了,transaction_log
	// 却记着发放成功。把销账挪到存盘之后又会开出反向的复制窗口。两个窗口只能靠
	// 「标记与资产同生共死」同时堵住 —— 标记就落在同一行 player_database 里。
	//
	// 于是有两条**必须分开**的判据(合成一条是第一版设计被评审判 broken 的主因):
	//   HasApplied(实体上的活账本)      -> 「别再发一次奖」
	//   IsSettlementDurable(落盘快照)   -> 「可以销账」
	// 活账本里有、落盘快照里没有,恰恰是最危险的一刻:此时销账 = 崩溃即永久丢失。

	// 活账本是玩家的**必需组件**,唯一创建点是加载(PlayerDatabaseMessageFieldsUnmarshal 无条件
	// emplace,内容随 player_database 回来),这里只取不建。缺失 = 加载不变量被破坏:记 ERROR 并返回
	// nullptr,调用方一律 fail-closed —— 不应用、不销账、不按锁重建冻结,pending 原样保留,交给
	// 发件箱重投 / 下一次登录(届时账本随盘上字节回来)裁决。
	// **绝不** get_or_emplace 一本空账本:空账本把已经应用过的局重新判成「没应用」,应用路径据此
	// 再发一次奖、销账路径据此当场销账;下一次存盘还会拿空账本覆盖盘上的条目。
	// 与资产通道账本(asset_op_system.cpp「绝不 get_or_emplace 一本空账本」)是同一条约定。
	BattleSettlementLedgerComp* FindSettlementLedger(entt::entity player, const uint64_t playerId,
													 const uint64_t battleId, const char* context)
	{
		auto* ledger = tlsEcs.actorRegistry.try_get<BattleSettlementLedgerComp>(player);
		if (ledger == nullptr)
		{
			LOG_ERROR << "[PlayerBattle] metric=battle_settlement_ledger_missing player_id=" << playerId
					  << " entity=" << entt::to_integral(player) << " battle_id=" << battleId
					  << " context=" << context
					  << ",settlement ledger component missing(load invariant broken);operation skipped, pending retained";
		}
		return ledger;
	}

	// 仅证明该局条目在Redis成功快照中；不可代替两张玩家SQL行的提交确认。
	bool SnapshotContainsSettlement(entt::entity player, const uint64_t battleId)
	{
		const auto* snapshot = tlsEcs.actorRegistry.try_get<PlayerLastPersistedSnapshotComp>(player);
		if (snapshot == nullptr || !snapshot->HasSnapshot()) return false;
		return battle_settlement::HasApplied(
			snapshot->snapshot->player_database_data().settlement_ledger(), battleId);
	}

	// 只在销账写入口延迟创建的按需状态，不持久化、不持连接；每实体同一时刻最多一个只读probe。
	struct PendingSettlementSqlProbe
	{
		uint64_t requestId{0};
		std::chrono::steady_clock::time_point started{};
	};
	thread_local uint64_t settlementSqlProbeSeq = 0;

	void RequestSqlConfirmedSettlementAck(entt::entity player, uint64_t playerId, uint64_t battleId)
	{
		const auto now = std::chrono::steady_clock::now();
		auto &probe = tlsEcs.actorRegistry.get_or_emplace<PendingSettlementSqlProbe>(player);
		if (probe.requestId != 0 && now - probe.started < std::chrono::seconds(10)) return;
		const auto *snapshot = tlsEcs.actorRegistry.try_get<PlayerLastPersistedSnapshotComp>(player);
		if (snapshot == nullptr || !snapshot->HasSnapshot() || settlementSqlProbeSeq == UINT64_MAX) return;
		const auto requestId = ++settlementSqlProbeSeq;
		probe.requestId = requestId; probe.started = now;
		const bool sent = RoleAdmissionSystem::ConfirmSnapshotSqlCommit(player, *snapshot->snapshot,
			[player = player, playerId, battleId, requestId](bool committed) {
				if (!tlsEcs.actorRegistry.valid(player) || tlsEcs.GetPlayer(playerId) != player
					|| tlsEcs.GetPlayerId(player) != playerId) return;
				auto *current = tlsEcs.actorRegistry.try_get<PendingSettlementSqlProbe>(player);
				if (current == nullptr || current->requestId != requestId) return;
				current->requestId = 0;
				if (committed && SnapshotContainsSettlement(player, battleId))
					ClearPendingSettlementIfMatch(playerId, battleId);
			});
		if (!sent) probe.requestId = 0;
	}

	// 整笔结算此刻能不能应用。**必须是 BagService / CurrencySystem 内部闸的超集**:
	//  - IsCrossZoneFrozen:与那两个入口同判据。漏掉它,金币会被拒(整笔返回 false 重投)
	//    但道具会被静默拒绝而结算照样记进账本 —— 道具永久丢;
	//  - PlayerTravelHandoffComp / UnregisterPlayer:这两个窗口里 SavePlayerToRedis 开头
	//    直接跳过写盘,应用了也落不了盘。不应用、不销账,交给发件箱重投或目的地节点补应用。
	//
	// 刻意**不**含资产通道的 HasFencedOwnership(归属围栏):那条是给「Go 依据 durable 记账」
	// 用的,本路径的重做凭据是发件箱 + pending,重投时会重新解析 player:{id}:location,
	// 无围栏实体上最坏结果是这一笔被重投一次,不会出现帮会那种「记了资金却被盖掉」。
	bool IsSettlementApplicable(entt::entity player)
	{
		return !PlayerLifecycleSystem::IsCrossZoneFrozen(player) &&
			   !tlsEcs.actorRegistry.any_of<PlayerTravelHandoffComp, UnregisterPlayer>(player);
	}

	void AckSettlementPending(uint64_t playerId, uint64_t battleId, bool supersededWithoutEntity = false);

	// 应用之后立刻压一次存盘:不等回调、不阻塞,只为把「已应用但没落盘」的窗口从一个
	// 周期存盘(默认 300s)压到一次 Redis 往返。这一步是 pending **每玩家单槽**时的关键 ——
	// 槽位被占太久,玩家打下一场时 battle 侧的无条件 SET 会把上一场的 payload 覆盖掉,
	// 那一局就再也没有持久副本了(评审 blocker 3)。
	void RequestSettlementPersist(entt::entity player, const uint64_t playerId, const uint64_t battleId)
	{
		// 不可改的实体不压存盘:冻结/交接在途时本节点无权再写盘,退出在途时退出存盘已经在飞
		// (它 marshal 的那一份本来就含账本,落地即 durable),这里再压一次只会与之竞争。
		// 跳过不影响正确性:durable 仍为假 -> 不销账 -> pending 保留 -> 重投或目的地补应用。
		if (!IsSettlementApplicable(player))
		{
			LOG_DEBUG << "[PlayerBattle] Settlement save skipped(entity cannot currently be modified), player_id=" << playerId
					  << " battle_id=" << battleId;
			return;
		}
		if (!gPersistFn(player))
		{
			// false 只表示没有排入新写:要么盘上已是这一份(快路径),要么存盘被拒(缺身份 / 持久组件、存盘序号耗尽,保存入口已打 ERROR)。
			// 交接 / 退出在途已被上面 IsSettlementApplicable 挡掉,走不到这里。哪种都不能当成「已落盘」,durable 必须由同快照双SQL ACK确认。
			LOG_DEBUG << "[PlayerBattle] Settlement save not queued(unchanged or refused), player_id="
					  << playerId << " battle_id=" << battleId;
			return;
		}
		// 快路径:在同一条连接上紧跟一条 PING。玩家存盘客户端与 tlsRedis.GetZoneRedis() 是同一个
		// hiredis 连接(core/system/redis.cpp:playerRedis 由它构造),回复按发送顺序处理;
		// 存盘成功回调(HandlePlayerAsyncSaved)同步更新 PlayerLastPersistedSnapshotComp。
		// 所以 PING 的回调里,刚才那笔存盘若已直接发出,就必然已落地、快照已是新的 ——
		// PING只触发再次核快照；仍须另读同E/q双SQL ACK才能销账放锁。
		// 存盘若被排在同玩家另一笔在途存盘之后(或进了失败重试),PING 会先回来,这次销账照常延后,
		// 由发件箱 10s 重投 / reaper 30s / 登录补销账接手,正确性不依赖快路径。
		if (!RedisReady()) return;
		tlsRedis.GetZoneRedis()->command(
			[playerId, battleId](hiredis::Hiredis*, redisReply*) {
				AckSettlementPending(playerId, battleId);
			},
			"PING");
	}

	// **销账的唯一入口**(= 给 battle 侧发件箱的 ACK,R07;落盘后连同战斗锁一起放)。
	//
	// 规则按优先级只有三条:
	//  1. 本节点有这名玩家的活实体,且账本里有这一局 = 已应用:
	//     Redis快照中有且同E/q双SQL ACK确认 -> 销账 + 放锁;否则保留，必要时请求存盘,
	//     交给快路径(PING)/ 发件箱 10s 重投 / reaper 30s / 登录补销账。
	//  2. 本节点有活实体、账本里没有 = 本实体没应用过这一局 -> 按判废/重复投递处理,立即销账
	//     (与改造前行为一致)。锁在结算应用后一直留到落盘,所以重投窗口内「锁不在」只可能是
	//     已作废或已销账,不再可能是「应用了还没落盘」。
	//  3. 本节点没有活实体:无从区分「已随退出存盘落地」与「随实体被废黜/进程崩溃一起丢了」。
	//     只有调用方能正面证明这一局已被取代(supersededWithoutEntity:锁已被另一局持有)时才销账;
	//     否则什么都不删,pending 留给登录钩子 —— 登录时账本随盘上字节回来,由规则 1/应用路径裁决。
	//
	// 为什么判废分支也必须走这里:发件箱重投会落进各种「看起来与应用无关」的分支
	// (无 InBattleComp、battle_id 不匹配……),改造前它们各自当场销账,把延后的努力全部作废。
	// 按 player_id 现取实体,调用方不必持实体句柄,也不会拿到过期句柄。
	void AckSettlementPending(const uint64_t playerId, const uint64_t battleId, const bool supersededWithoutEntity)
	{
		const auto player = tlsEcs.GetPlayer(playerId);
		const bool live = tlsEcs.actorRegistry.valid(player) && tlsEcs.GetPlayerId(player) == playerId;
		if (!live)
		{
			if (supersededWithoutEntity)
			{
				ClearPendingSettlementIfMatch(playerId, battleId);
				return;
			}
			LOG_INFO << "[PlayerBattle] No live player entity on this node;pending settlement retained for the login hook: player_id=" << playerId
					 << " battle_id=" << battleId;
			return;
		}
		// 活实体却没有账本:规则 1 / 2 都判不了。销账是不可逆的那一侧(销了就可能丢奖励),
		// 所以什么都不删,pending 留给重投 / 登录钩子(FindSettlementLedger 的说明)。
		const auto* ledger = FindSettlementLedger(player, playerId, battleId, "ack");
		if (ledger == nullptr)
		{
			return;
		}
		if (!battle_settlement::HasApplied(*ledger, battleId))
		{
			ClearPendingSettlementIfMatch(playerId, battleId);
			return;
		}
		const auto *candidate = tlsEcs.actorRegistry.try_get<PlayerLastPersistedSnapshotComp>(player);
		const auto *owner = tlsEcs.actorRegistry.try_get<PlayerOwnerEpochComp>(player);
		if (candidate != nullptr && owner != nullptr && owner->epoch != 0
			&& candidate->committed_owner_epoch == owner->epoch && candidate->committed_save_seq != 0
			&& SnapshotContainsSettlement(player, battleId))
		{
			RequestSqlConfirmedSettlementAck(player, playerId, battleId);
			return;
		}
		// 已有一笔未落地的存盘时不再压:存盘队列里排着的永远是最新的一份(redis_client.h 的约定),
		// 结算应用之后才压的那一份必然带着这条账本。不加这道闸,每场结算都会连存两次盘,
		// reaper 每轮还会按账本条目数各压一次(评审 wne803bj5 #7)。
		const auto& playerRedis = tlsRedisSystem.GetPlayerDataRedis();
		if (playerRedis == nullptr || !playerRedis->HasUnsettledSave(playerId))
		{
			RequestSettlementPersist(player, playerId, battleId);
		}
		LOG_INFO << "[PlayerBattle] Settlement applied but not persisted;acknowledgment deferred: player_id=" << playerId
				 << " battle_id=" << battleId << ",acknowledgment will complete after persistence via fast path/redelivery/reaper/login";
	}

	// 离线暂存待结算记录(两键不过期 + 进积压索引),只在锁仍属于本局时写。battle 已经写过同一份,
	// 这里是幂等覆盖,同时兜住"老版本 battle 还没落库就投递"的灰度窗口;老版本 battle 写入带的 7 天 TTL
	// 也在这里被不带 EX 的 SET 清掉。
	// 锁校验与写入必须在同一条 Lua 里(kStorePendingSettlementIfLockMatchScript 的说明)。
	void StorePendingSettlementIfLockMatch(const uint64_t playerId, const uint64_t battleId,
										   const std::string& payload)
	{
		if (!RedisReady())
		{
			LOG_ERROR << "[PlayerBattle] Offline settlement staging failed(Redis disconnected), player_id=" << playerId
					  << " battle_id=" << battleId << ",retained for redelivery";
			return;
		}
		tlsRedis.GetZoneRedis()->command(
			[playerId, battleId](hiredis::Hiredis*, redisReply* reply) {
				// 无回复 / Redis 错误不是锁不存在,不能销账丢掉可恢复的奖励。
				if (reply == nullptr || reply->type != REDIS_REPLY_INTEGER)
				{
					LOG_WARN << "[PlayerBattle] Offline settlement staging EVAL failed;retained for redelivery: player_id=" << playerId
							 << " battle_id=" << battleId;
					return;
				}
				if (reply->integer == 0)
				{
					// 玩家不在本节点时,「锁不在」不能当成「这一局已作废」的证据(评审 wne803bj5 #0/#1):
					//  - 锁的 TTL 是 deadline+60s,离线玩家的结算常常晚于它到达,锁自然过期而这一局是合法的;
					//  - 能判废 FIGHTING 的只有 reaper,它要实体在场,离线玩家的战斗没有判废方;
					//  - 也可能这一局刚在别处落盘销账(销账同一条 Lua 删 pending 与锁)。
					// 所以这里什么都不删、什么都不写,已有的 pending 留给登录钩子(它不看锁,按账本裁决)。
					// 代价:发件箱会重投到次数用尽再打一条 undelivered,这正是它设计上的兜底。
					LOG_WARN << "[PlayerBattle] Offline settlement: battle lock missing;not staged,existing pending settlement retained for the login hook, player_id="
							 << playerId << " battle_id=" << battleId;
					return;
				}
				if (reply->integer == 2)
				{
					// 锁被另一局正面持有 = 这一局已被取代:本局锁只会在落盘销账时才删,
					// 另一局能拿到锁,说明这一局要么已落盘销账,要么已被判废。可以销账。
					LOG_WARN << "[PlayerBattle] Offline settlement discarded: battle lock value mismatch, player_id=" << playerId
							 << " battle_id=" << battleId;
					AckSettlementPending(playerId, battleId, /*supersededWithoutEntity=*/true);
					return;
				}
				LOG_INFO << "[PlayerBattle] Settlement staged(player offline), player_id=" << playerId
						 << " battle_id=" << battleId;
			},
			(std::string("EVAL %s 4 ") + kBattleLockKeyFmt + " " + kPendingSettlementKeyFmt + " " +
			 kPendingSettlementIdKeyFmt + " " + kPendingSettlementIndexKey + " %llu %b %llu %llu").c_str(),
			kStorePendingIfLockMatchScript, playerId, playerId, playerId, battleId,
			payload.data(), payload.size(), TimeSystem::NowMillisecondsUTC(), playerId);
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
		tlsRedis.GetZoneRedis()->command(
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
	}

	// 重建冻结后的复核:重建依据的是"发 EVAL 那一刻"的锁,回调链上可能已排着一条同 battle_id 的
	// 条件删锁(结算按锁应用 / 取消)。再发一次条件续期 + 覆写 ctx:命中说明锁仍是本战斗的,重建成立;
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
		tlsRedis.GetZoneRedis()->command(
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
	//   deadline_ms 缺失时按锁剩余 TTL 反推(锁 TTL = deadline + 60s 余量,只会略晚不会更早),
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

	// 摘 InBattleComp + 条件删锁(结算已应用/取消/作废的统一收尾)
	void ClearBattleFreeze(entt::entity player, const uint64_t playerId, const uint64_t battleId)
	{
		// 先发条件删锁再摘组件:摘组件会触发组队跟随链,它读 battle:lock fail-closed
		DeleteBattleLockIfMatch(playerId, battleId);
		RemoveInBattleComp(player);
	}

	// 结算应用后续锁,保证锁至少活过 battle 发件箱的重投窗口(kSettlementLockHoldSec)。
	// 防的是「应用了、还没落盘、进程崩了」之后锁自然过期:重投若在那之后到达,会把「锁不在」
	// 读成「这一局已作废」,删掉唯一的 pending。fire-and-forget:失败时退回原 TTL(deadline+60s)。
	void HoldBattleLockUntilDurable(const uint64_t playerId, const uint64_t battleId)
	{
		if (!RedisReady()) return;
		tlsRedis.GetZoneRedis()->command(
			[playerId, battleId](hiredis::Hiredis*, redisReply* reply) {
				if (reply == nullptr || reply->type == REDIS_REPLY_ERROR)
				{
					LOG_ERROR << "[PlayerBattle] Post-settlement lock renewal EVAL failed, player_id=" << playerId
							  << " battle_id=" << battleId << ",lock will expire with its original TTL";
				}
			},
			(std::string("EVAL %s 2 ") + kBattleLockKeyFmt + " " + kBattleCtxKeyFmt + " %llu %u").c_str(),
			kHoldLockIfMatchScript, playerId, playerId, battleId,
			static_cast<unsigned>(PlayerBattleSystem::kSettlementLockHoldSec));
	}

	// 结算已应用(内存)后的解冻:只摘 InBattleComp,**锁留到落盘**。
	//
	// 锁是「这一局还没完」的唯一跨进程证据:match 靠它挡下一场排队,重投分支靠它区分
	// 「已作废」与「应用了但还没落盘」。改造前应用当下就删锁,两件事同时出错(评审 wne803bj5):
	//  - 崩溃后重投把「锁不在」读成作废,删掉唯一的 pending,奖励永久丢失;
	//  - 玩家能在 pending 还没销账时开下一场,battle 对下一局的无条件 SET 覆盖掉这一局的 pending,
	//    重登时登录钩子还会因为读到旧 pending 而漏掉下一场的冻结重建。
	// 锁由 AckSettlementPending 在落盘确认后与 pending 同一条 Lua 删掉,并补一次跟随检查。
	// 代价:落盘确认之前不能排下一场 —— 快路径(RequestSettlementPersist 的 PING)把它压到一次 Redis 往返。
	void ReleaseFreezeKeepLock(entt::entity player, const uint64_t playerId, const uint64_t battleId)
	{
		HoldBattleLockUntilDurable(playerId, battleId);
		RemoveInBattleComp(player);
	}

	// 推 BattleEndS2C(结算入账通知;battle 节点也会在战斗结束时推一份,
	// 客户端按 battle_id 幂等处理;离线补应用路径则只有这一份)
	// 物品能否在回合制战斗里使用。快照出包与结算扣除共用这一处判据 ——
	// 分成两处写,日后只改一边就会出现"能带进去却扣不掉"(或反过来)。
	bool IsBattleUsableItem(uint32_t configId)
	{
		const auto* itemRow = ItemTableManager::Instance().FindByIdSilent(configId).first;
		return itemRow != nullptr && itemRow->battle_usable() != 0;
	}

	// 结算掉落按 config 汇总:跳过空项,数量夹到 uint32。号段预检与入包共用这一份 ——
	// 两处各写一遍,改了一边就会出现「预检的不是实际要发的」。
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
	// 号段发不出号时,BagService 会整批拒绝入包;主背包与临时格两次都被拒,掉落就丢了。
	// 所以必须在任何不可重复的副作用(金币)之前判:不够就整笔延后,保留 pending 等重投。
	// 这与资产通道 ApplyCredit 的号段预检同一纪律。上界见 BagService::MaxNewInstancesFor:
	// 主背包放不下时一个号都不铸,转投临时格最多也只铸这么多,一份余量就覆盖两段式发放。
	// 号段双 buffer 加后台预取,续段通常是秒级的事,发件箱 10s 一轮的重投足以接上。
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
				 << ",item guid segment cannot cover drops;whole settlement deferred for redelivery";
		return false;
	}

	// 结算道具落地:先按实际持有夹紧扣消耗,再把掉落塞进背包。
	// 全程不返回失败 —— 见调用处注释(金币已入账,失败会导致重投时重复加钱)。
	// 号段不足导致的掉落失败已由 HasItemGuidsForDrops 在金币之前挡掉。走到这里仍会失败的,
	// 只剩两处都放不下、物品被全服禁发、配置错误这类终局原因,维持「记 ERROR、继续」。
	void ApplySettlementItems(entt::entity player, uint64_t playerId,
							  const ::BattleSettlementData& settlement)
	{
		auto* bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player);
		if (bags == nullptr)
		{
			// 玩家实体没经过背包还原(理论上登录链一定挂了,这里是防御)
			LOG_ERROR << "[PlayerBattle] Item settlement skipped: player lacks bag component, player_id=" << playerId
					  << " battle_id=" << settlement.battle_id();
			return;
		}
		auto& inventory = bags->bags[kInventory];
		const uint64_t battleId = settlement.battle_id();
		const std::string extra = "{\"source\":\"battle\",\"battle_id\":" +
								  std::to_string(battleId) + "}";

		// ① 消耗:引擎账本记的是"在战斗副本里用掉了几个",玩家真实持有可能更少
		//    (战斗中途被别处扣走)。契约是按实际持有夹紧、不足按 0 并记日志防刷。
		if (settlement.items_consumed_size() > 0)
		{
			ItemCountMap consumed;
			uint64_t requestedTotal = 0;
			for (const auto& item : settlement.items_consumed())
			{
				if (item.item_table_id() == 0 || item.count() == 0)
				{
					continue;
				}
				// 反向校验:快照只会把 battle_usable 的物品放进副本,所以账本里出现别的 id
				// 只有两种可能 —— 陈旧的 battle 节点,或有人伪造结算。放行等于让战斗服
				// 点名销毁玩家的任意物品(装备/任务道具),必须挡住并告警。
				if (!IsBattleUsableItem(item.item_table_id()))
				{
					LOG_ERROR << "[PlayerBattle] Settlement consumption rejected: contains non-battle items, player_id=" << playerId
							  << " battle_id=" << battleId << " item=" << item.item_table_id();
					continue;
				}
				// BattleItemEntry.count 是 uint64,ItemCountMap 的值是 uint32:必须夹一次,
				// 否则高位截断会把一个天文数字变成小数量(或反过来)
				const uint64_t clamped =
					std::min<uint64_t>(item.count(), std::numeric_limits<uint32_t>::max());
				consumed[item.item_table_id()] += static_cast<uint32_t>(clamped);
				requestedTotal += clamped;
			}
			if (!consumed.empty())
			{
				std::vector<DrainedInstance> drained;
				const auto result = BagService::RemoveItemsClamped(
					player, inventory, consumed, &drained, battleId, extra);
				uint64_t drainedTotal = 0;
				for (const auto& entry : drained)
				{
					drainedTotal += entry.amount;
				}
				if (result != kSuccess)
				{
					LOG_ERROR << "[PlayerBattle] Item consumption deduction rejected: player_id=" << playerId
							  << " battle_id=" << battleId << " err=" << result;
				}
				else if (drainedTotal < requestedTotal)
				{
					// 防刷信号:账面用了 N 个、实际只扣到 M 个。今天 scene 侧没有任何玩家可
					// 触发的背包扣减入口(背包 RPC 只有 GetBag/SortBag,Sort 已加 InBattle 闸),
					// 所以正常玩法下不该出现。日后新增扣减入口(交易/寄售/使用物品)必须各自
					// 加 InBattle 闸(D48:闸只放入口层),否则这条 WARN 会变成常态。
					LOG_WARN << "[PlayerBattle] Item consumption clamped to actual holdings: player_id=" << playerId
							 << " battle_id=" << battleId << " requested=" << requestedTotal
							 << " drained=" << drainedTotal;
				}

			}
		}

		// ② 掉落:主背包放不下就退到临时格(kTemporary 的设计语义就是掉落溢出缓冲,
		//    FIFO 淘汰最旧的)。两处都失败才算丢,记 ERROR 等人查。
		if (settlement.items_gained_size() > 0)
		{
			const ItemCountMap gained = CollectGainedItems(settlement);
			if (!gained.empty())
			{
				// bag_service.h 保证任何拒绝都不修改原包，失败时整批转投临时格。
				auto result = BagService::AddItems(player, inventory, gained,
					TX_ITEM_AWARD, battleId, extra);
				if (result != kSuccess)
				{
					LOG_WARN << "[PlayerBattle] 主背包拒绝，整批掉落转投临时格: player_id=" << playerId
						<< " battle_id=" << battleId << " err=" << result;
					result = BagService::AddItems(player, bags->bags[kTemporary], gained,
						TX_ITEM_AWARD, battleId, extra);
				}
				if (result != kSuccess)
				{
					LOG_ERROR << "[PlayerBattle] Drop grant failed(main bag and temporary slots both lack space): player_id="
							  << playerId << " battle_id=" << battleId << " err=" << result;
				}
			}
		}
	}

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
		tlsRedis.GetZoneRedis()->command(
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
		tlsRedis.GetZoneRedis()->command(
			[playerId](hiredis::Hiredis*, redisReply* reply) {
				if (reply == nullptr || reply->type == REDIS_REPLY_ERROR)
				{
					LOG_ERROR << "[PlayerBattle] SET battle:ctx failed, player_id=" << playerId
							  << "(login reconstruction will fall back to a conservative freeze without battle_node_id)";
				}
			},
			(std::string("SET ") + kBattleCtxKeyFmt + " %b EX %llu").c_str(),
			playerId, ctxPayload.data(), ctxPayload.size(), ttlSec);
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
		tlsRedis.GetZoneRedis()->command(
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
		// 结算才能在线命中(否则新实体没有 InBattleComp,结算被确定性丢弃)。
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
		tlsRedis.GetZoneRedis()->command(
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
				const uint64_t ttlSec = LockTtlSecFor(deadlineMs, callbackNowMs);
				ConfirmBattleLockIfMatch(playerId, battleId, ttlSec, SerializeBattleCtx(ctx));
				LOG_INFO << "[PlayerBattle] ConfirmBattle: player offline;renew conditionally by lock value and persist ctx, player_id="
						 << playerId << " battle_id=" << battleId << " deadline_ms=" << deadlineMs
						 << " battle_node_id=" << ctx.battle_node_id() << " lock_ttl_sec=" << ttlSec;
			},
			(std::string("EVAL %s 2 ") + kBattleLockKeyFmt + " " + kBattleCtxKeyFmt + " %llu").c_str(),
			kGetCtxIfLockMatchScript, playerId, playerId, battleId);
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

	const uint64_t ttlSec = LockTtlSecFor(deadlineMs, nowMs);
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
	tlsRedis.GetZoneRedis()->command(
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
			// 锁在结算应用后一直留到落盘销账(ReleaseFreezeKeepLock)。锁指向的这一局若已在活账本里,
			// 说明它已经结算完、只是还没落盘销账 —— 不能据此把玩家重新冻进一场已结束的战斗,
			// 改为推进销账(已落盘就连锁一起放掉)。
			// 没有账本就判不了这一局结没结算:不能把「缺账本」当「没应用」去冻人,也推进不了销账。
			// 本次重建跳过 —— 锁仍在 Redis 里串行化下一次排队,结算应用路径对缺账本同样 fail-closed。
			const auto* ledger = FindSettlementLedger(player, playerId, battleId, "freeze_rebuild");
			if (ledger == nullptr)
			{
				return;
			}
			// 已应用 > 已作废 > 重建(ClassifyLockRebuild 的说明)。
			switch (DecideLockRebuild(player, battleId, *ledger))
			{
			case battle_settlement::LockRebuildDecision::kAdvanceAck:
				LOG_INFO << "[PlayerBattle] Freeze reconstruction skipped: locked battle settled and awaiting persistence, player_id=" << playerId
						 << " battle_id=" << battleId << " reason=" << reasonCopy;
				AckSettlementPending(playerId, battleId);
				return;
			case battle_settlement::LockRebuildDecision::kSkipExpired:
				// reaper 已按期限判这一局作废,删锁没落地才会再读到这把锁:不重建,补一次条件删锁。
				LOG_WARN << "[PlayerBattle] metric=battle_freeze_rebuild_skipped_expired player_id=" << playerId
						 << " battle_id=" << battleId << " reason=" << reasonCopy
						 << ",lock still points to a battle this node already expired;not rebuilding,retrying conditional lock deletion";
				// 解冻早已发生(reaper 作废时),锁现在才真正放掉:删掉了就补一次组队跟随(DeleteBattleLockIfMatch 的说明)。
				DeleteBattleLockIfMatch(playerId, battleId, player);
				return;
			case battle_settlement::LockRebuildDecision::kRebuild:
				break;
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

			// 续期到正式 deadline + 覆写 ctx,同时复核锁仍是本战斗的(否则撤销重建)
			const uint64_t ttlSec = LockTtlSecFor(rebuilt.deadline_ms(), nowMs);
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
	const int rc = tlsRedis.GetZoneRedis()->command(
		[player, playerId](hiredis::Hiredis*, redisReply* reply) {
			if (!tlsEcs.actorRegistry.valid(player) || tlsEcs.GetPlayerId(player) != playerId)
			{
				return;
			}
			if (tlsEcs.actorRegistry.any_of<InBattleComp>(player))
			{
				return; // 重连路径组件仍在 / 登录后已备战新局:不动
			}
			switch (battle_settlement::ClassifyLoginLockRead(battle_settlement::ReplyShapeOf(reply)))
			{
			case battle_settlement::LoginLockRead::kRecheck:
				// 空应答:连接断了,锁在不在不知道。以前与 NIL 一起按「无锁」return,战斗闸就此失效。
				MarkLoginRecheck(player, playerId, "lock_read_no_reply");
				return;
			case battle_settlement::LoginLockRead::kNoLock:
				// 锁不在 = 没有战斗在途。EVAL 的 ERROR 也按无锁处理:既有取舍,锁本身是咨询性的
				// (代价见 settlement_outbox.h 的 ClassifyLoginLockRead)。
				return;
			case battle_settlement::LoginLockRead::kLock:
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
			// 锁在结算应用后一直留到落盘销账(ReleaseFreezeKeepLock)。锁指向的这一局若已在活账本里,
			// 说明它已经结算完、只是还没落盘销账 —— 不能据此把玩家重新冻进一场已结束的战斗,
			// 改为推进销账(已落盘就连锁一起放掉)。
			// 缺账本时跳过本次重建,理由同 RebuildBattleFreezeFromLock。
			const auto* ledger = FindSettlementLedger(player, playerId, battleId, "login_freeze_restore");
			if (ledger == nullptr)
			{
				return;
			}
			// 已应用 > 已作废 > 重建(ClassifyLockRebuild 的说明)。
			switch (DecideLockRebuild(player, battleId, *ledger))
			{
			case battle_settlement::LockRebuildDecision::kAdvanceAck:
				LOG_INFO << "[PlayerBattle] Freeze reconstruction skipped: locked battle settled and awaiting persistence, player_id=" << playerId
						 << " battle_id=" << battleId << " reason=login";
				AckSettlementPending(playerId, battleId);
				return;
			case battle_settlement::LockRebuildDecision::kSkipExpired:
				// reaper 已按期限判这一局作废,删锁没落地才会再读到这把锁:不重建,补一次条件删锁。
				// 走到这里的路径:冻结到期补读没读到挂的复查、补应用另一局挂起结算之后的重建、登录挂的复查赶上冻结到期。
				LOG_WARN << "[PlayerBattle] metric=battle_freeze_rebuild_skipped_expired player_id=" << playerId
						 << " battle_id=" << battleId << " reason=login"
						 << ",lock still points to a battle this node already expired;not rebuilding,retrying conditional lock deletion";
				// 解冻早已发生(reaper 作废时),锁现在才真正放掉:删掉了就补一次组队跟随(DeleteBattleLockIfMatch 的说明)。
				DeleteBattleLockIfMatch(playerId, battleId, player);
				return;
			case battle_settlement::LockRebuildDecision::kRebuild:
				break;
			}
			std::string ctxPayload;
			ReplyElementToString(reply, 1, ctxPayload);
			const uint64_t nowMs = TimeSystem::NowMillisecondsUTC();
			const InBattleComp rebuilt =
				BuildInBattleFromCtx(battleId, ctxPayload, ReplyElementToInteger(reply, 2), nowMs);
			tlsEcs.actorRegistry.emplace<InBattleComp>(player, rebuilt);

			// 复核锁仍是本战斗的(登录窗口内恰好到达的结算已删锁 -> 撤销重建);
			// TTL 按重建后的有效期限算:PREPARING 看 prepare_deadline,其余看 deadline
			const uint64_t effectiveDeadlineMs =
				(rebuilt.state() == IN_BATTLE_STATE_PREPARING && rebuilt.prepare_deadline_ms() != 0)
					? rebuilt.prepare_deadline_ms()
					: rebuilt.deadline_ms();
			ConfirmRebuiltFreeze(player, playerId, battleId, LockTtlSecFor(effectiveDeadlineMs, nowMs),
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

bool CanCreditWholeBattleGold(entt::entity player, uint64_t gold)
{
    if (gold == 0) return true;
    const auto* balances = tlsEcs.actorRegistry.try_get<CurrencyComp>(player);
    const auto* currency = tlsEcs.actorRegistry.try_get<PlayerCurrencyComp>(player);
    if (!balances || !currency || gold > static_cast<uint64_t>(std::numeric_limits<int64_t>::max()) ||
        GainBlockService::IsGainBlocked(GainBlockService::GainType::kCurrency, static_cast<uint32_t>(kCurrencyGold)) ||
        CurrencySystem::IsCurrencyBlocked(player, kCurrencyGold)) return false;
    uint64_t net = gold;
    if (const auto it = currency->debts.find(static_cast<uint32_t>(kCurrencyGold)); it != currency->debts.end()) {
        const auto& debt = it->second;
        // 与下方结算入口同一谓词:只认更新版本加的欠款字段,已退役字段的残留字节不算版本错位。
        if (CurrencySystem::DebtCarriesUnknownFields(player, kCurrencyGold)) return false;
        if (!debt.frozen && (debt.expiresAt == 0 || TimeSystem::NowSecondsUTC() < debt.expiresAt))
            net -= std::min(net, debt.Remaining());
    }
    const auto index = static_cast<int>(kCurrencyGold);
    const uint64_t balance = index < balances->values_size() ? balances->values(index) : 0;
    return net <= std::numeric_limits<uint64_t>::max() - balance;
}

// 数量副本没有GUID；只从当前完整实例候选中扣已知普通单位，未知schema/实例不能当普通数量。
bool HasUnsupportedInboxConsumption(entt::entity player, const ::BattleSettlementData& settlement)
{
    std::set<uint32_t> configs;
    for (const auto& entry : settlement.items_consumed()) {
        const auto* row = ItemTableManager::Instance().FindByIdSilent(entry.item_table_id()).first;
        if (!row || HasNestedUnknownFields(*row)) return true;
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

bool HasCompleteBattleEffectInputs(entt::entity player, const ::BattleSettlementData& settlement)
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
    return CanCreditWholeBattleGold(player, settlement.gold_gain());
}
} // namespace

PlayerInboxExecResult PlayerBattleSystem::ApplyInboxSettlement(entt::entity player, const std::string& payload)
{
    ::BattleSettlementData settlement;
    if (payload.size() > 16384 || !settlement.ParseFromString(payload) || HasNestedUnknownFields(settlement) ||
        !::eBattleOutcome_IsValid(settlement.outcome()) || settlement.outcome() == ::BATTLE_OUTCOME_ONGOING)
        return {PlayerInboxExecKind::kVersionSkew, 0};
    if (!tlsEcs.actorRegistry.valid(player)) return {PlayerInboxExecKind::kDefer, 0};
    if (HasUnsupportedInboxConsumption(player, settlement)) return {PlayerInboxExecKind::kVersionSkew, 0};
    if (settlement.gold_gain() > 0 && CurrencySystem::DebtCarriesUnknownFields(player, kCurrencyGold))
        return {PlayerInboxExecKind::kVersionSkew, 0};
    if (!ApplySettlementToEntity(player, settlement, nullptr, true)) return {PlayerInboxExecKind::kDefer, 0};
    return {PlayerInboxExecKind::kApplied, 0};
}

bool PlayerBattleSystem::CompleteInboxSettlement(entt::entity player, const std::string& payload)
{
    ::BattleSettlementData settlement;
    if (!tlsEcs.actorRegistry.valid(player) || !settlement.ParseFromString(payload) ||
        HasNestedUnknownFields(settlement) || settlement.player_id() != tlsEcs.GetPlayerId(player) ||
        settlement.battle_id() == 0) return false;
    // SQL确认之后才公开终局/解冻；旧局不能修改已经开始的新局HP或摘新局组件。
    const auto* current = tlsEcs.actorRegistry.try_get<InBattleComp>(player);
    if (current == nullptr || current->battle_id() == settlement.battle_id()) {
        DeleteBattleLockIfMatch(settlement.player_id(), settlement.battle_id());
        if (current != nullptr) RemoveInBattleComp(player);
        PushBattleEndToPlayer(player, settlement);
    }
    return true;
}

bool PlayerBattleSystem::ApplySettlementToEntity(entt::entity player, const ::BattleSettlementData& settlement,
                                                bool* alreadyAppliedOut, bool fromInbox)
{
    if (alreadyAppliedOut != nullptr) *alreadyAppliedOut = false;
    if (!tlsEcs.actorRegistry.valid(player)) return false;
    const uint64_t playerId = tlsEcs.GetPlayerId(player);
    const uint64_t battleId = settlement.battle_id();
    if (playerId == 0 || playerId != settlement.player_id() || battleId == 0)
    {
        LOG_ERROR << "[PlayerBattle] Settlement application failed: invalid entity or settlement ownership, player_id=" << playerId
                  << " battle_id=" << battleId;
        return false;
    }

    // 持久账本先判:它是**权威**的「这一局是否已经应用过」,唯一能跨进程重启与实体重建
    // 生效的一层(thread_local 应用缓存重启即空)。命中 = 奖已经发过,绝不能再发一次。
    // 但这**不**代表可以销账 —— 能不能销账由 IsSettlementDurable 按落盘字节单独判,
    // 调用方经 AckSettlementPending 决定。两条判据分开是本次改造的核心。
    // 账本缺失在任何副作用之前就拒绝(校验前置):既判不了这一局应用过没有,应用完也无处登记,
    // 只能整笔不应用、保留 pending 等重投(FindSettlementLedger 的说明)。
    const auto* ledger = fromInbox ? nullptr : FindSettlementLedger(player, playerId, battleId, "apply");
    if (!fromInbox && ledger == nullptr)
    {
        return false;
    }
    if (!fromInbox && battle_settlement::HasApplied(*ledger, battleId))
    {
        LOG_INFO << "[PlayerBattle] Settlement already in persistent ledger;duplicate application skipped: player_id=" << playerId
                 << " battle_id=" << battleId;
        if (alreadyAppliedOut != nullptr) *alreadyAppliedOut = true;
        return true;
    }

    // 整笔可应用判据。必须在任何不可重复的副作用之前(应用缓存契约),且必须覆盖**整笔**结算 ——
    // 改造前它只看 gold/items,于是一笔「只有 HP + 宝宝 + 击杀事实」的 PVE 结算(打空怪、无掉落,
    // 完全正常)会在冻结期穿过闸门:任务进度被推、宝宝被改,而此刻存盘被硬性跳过,改动随实体
    // 销毁一起没,账本却已记成已应用。
    if (!IsSettlementApplicable(player))
    {
        LOG_WARN << "[PlayerBattle] Player cannot currently be modified(cross-zone freeze / handoff in progress / exiting),"
                    "settlement retained for redelivery: player_id=" << playerId << " battle_id=" << battleId;
        return false;
    }

    // 宠物、物品和任务的写入口不能把加载故障当空状态跳过。必须在金币、HP/MP、任务与幂等账本
    // 产生任何副作用前检查，否则重投会被整笔账本挡住，漏掉的宠物状态或物品再也无法补回。
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

    const auto inboxIdentity = fromInbox ? RoleAdmissionSystem::ConfirmedIdentity(playerId) : std::nullopt;
    if (fromInbox && !inboxIdentity) return false;
    const auto originalIdentity = inboxIdentity ? inboxIdentity->SerializeAsString() : std::string{};
    ItemCountMap inboxConsumed, inboxGained, actualConsumption;
    std::set<uint64_t> inboxPets;
    std::map<std::string, uint32_t> skippedPets;
    std::string originalPets;
    if (fromInbox && (!HasCompleteBattleEffectInputs(player, settlement) ||
        !CollectExactSettlementItems(settlement, inboxConsumed, inboxGained))) return false;
    if (fromInbox) {
        if (!CollectApplicableInboxPets(player, settlement, inboxPets, skippedPets)) return false;
        if (settlement.pets_size() > 0) originalPets = tlsEcs.actorRegistry.get<PlayerPetComp>(player).SerializeAsString();
        if (!inboxConsumed.empty()) {
            const auto& inventory = tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory];
            for (const auto& [config, count] : inboxConsumed)
                actualConsumption[config] = static_cast<uint32_t>(std::min<uint64_t>(count, inventory.GetTotalItemCount(config)));
        }
    }

    // 掉落要铸的新实例身份必须先有着落(审计 cpp-economy#0)。号段暂缺是会自己消失的条件,
    // 与跨区冻结同类:整笔延后、零副作用,保留 pending 等重投。不能等到入包失败才发现 ——
    // 那时金币已入账,只能记 ERROR 吞掉掉落。
    if (!HasItemGuidsForDrops(CollectGainedItems(settlement), playerId, battleId))
    {
        return false;
    }

    // 走到这里,权威账本已证明本实体没有这一局。进程内缓存若还留着一条「已完成」,只可能是上一条
    // 实体生命周期留下的(那次应用没落盘,实体被重建 —— 重登、DiscardDeposedEntityOnReentry、存盘被
    // 归属围栏拒绝后废黜),资产也随之没了。以账本为准先清掉它再应用:原先是「本轮不应用、等下一次投递」,
    // 可登录钩子只投这一次、发件箱可能早已放弃,下一局还会覆盖单槽 pending,这一局就丢了
    // (第二轮评审 w0zw729x1 #2)。Forget 不动在途项,重入保护不受影响。
    if (!fromInbox && gSettlementApplications.Forget(playerId, battleId))
    {
        LOG_ERROR << "[PlayerBattle] metric=battle_settlement_cache_ledger_mismatch player_id=" << playerId
                  << " battle_id=" << battleId << ",cache reports applied but persistent ledger has no record;reapply according to ledger";
    }

    using ApplicationCache = battle_settlement::SettlementApplicationCache;
    const auto apply = [&]() -> bool {
		auto* baseAttributes = tlsEcs.actorRegistry.try_get<BaseAttributesComp>(player);
		if (baseAttributes == nullptr)
		{
			LOG_ERROR << "[PlayerBattle] Settlement application failed: missing BaseAttributesComp, player_id=" << playerId
					  << " battle_id=" << settlement.battle_id();
			return false;
		}

	    // 唯一会正常返回失败的入账入口必须在 HP/宝宝/任务副作用之前执行。
	    // 按 AddCurrency 的返回码契约分两类(currency_system.h):
	    //  - kAssetBlocked(全服 GainBlock / GM 单人封禁)是终局拒绝,重投永远同样被拒。整笔返回 false
	    //    会让这一局永远应用不上:药品不扣,掉落与任务进度丢失,下一局还会覆盖单槽 pending
	    //    (审计 cpp-economy#1)。所以只跳过金币,其余照常应用并登记账本,与资产通道把封禁记成
	    //    REJECTED 同一口径。拒绝发生在任何改动之前(AddCurrency 先判封禁再动账),跳过即零副作用。
	    //  - 其余失败(冻结 kAssetFrozen、缺组件 kInvalidParameter 等)不标记完成、不销账,保留 pending
	    //    等条件消失后重投。冻结其实已被前面的 IsSettlementApplicable 整笔挡掉。
	    bool goldBlocked = false;
        if (fromInbox) {
            // 整批包候选及所有初始化完成后，最后复核净金币（扣活跃债务后）不会溢出。
            // AddCurrency失败零副作用；成功后包提交不再有业务失败分支。
            const auto credit = [&]() -> uint32_t {
                const auto currentIdentity = RoleAdmissionSystem::ConfirmedIdentity(playerId);
                const auto* currentPets = tlsEcs.actorRegistry.try_get<PlayerPetComp>(player);
                if (!currentIdentity || currentIdentity->SerializeAsString() != originalIdentity ||
                    (settlement.pets_size() > 0 && (!currentPets || currentPets->SerializeAsString() != originalPets)) ||
                    HasUnsupportedInboxConsumption(player, settlement) ||
                    !IsSettlementApplicable(player) || !HasCompleteBattleEffectInputs(player, settlement) ||
                    !tlsEcs.actorRegistry.all_of<BaseAttributesComp, DerivedAttributesComp>(player)) return kInvalidParameter;
                return settlement.gold_gain() == 0 ? kSuccess : CurrencySystem::AddCurrency(
                    player, kCurrencyGold, static_cast<int64_t>(settlement.gold_gain()), TX_SYSTEM_GRANT, battleId);
            };
            const auto result = inboxConsumed.empty() && inboxGained.empty() ? credit() :
                BagService::ConsumeInventoryAndGrant(player, inboxConsumed, inboxGained, battleId,
                    "{\"source\":\"battle-inbox\"}", credit);
            if (result != kSuccess) return false;
            for (const auto& [config, requested] : inboxConsumed) {
                const auto applied = actualConsumption.at(config);
                if (applied < requested)
                    LOG_ERROR << "metric=battle_inbox_consumption_shortfall reason=held_less_than_committed"
                              << " battle_id=" << battleId << " config_id=" << config
                              << " requested=" << requested << " consumed=" << applied;
            }
            for (const auto& [reason, count] : skippedPets)
                LOG_ERROR << "metric=battle_inbox_pet_effect_skipped reason=" << reason
                          << " battle_id=" << battleId << " count=" << count;
            baseAttributes = tlsEcs.actorRegistry.try_get<BaseAttributesComp>(player); // 初始化回调后不持旧storage地址。
        }
	    else if (settlement.gold_gain() > 0)
	    {
	        if (settlement.gold_gain() > static_cast<uint64_t>(std::numeric_limits<int64_t>::max()))
	        {
	            LOG_ERROR << "[PlayerBattle] Currency settlement exceeds credit range: player_id=" << playerId
	                      << " battle_id=" << battleId;
	            return false;
	        }
	        const auto err = CurrencySystem::AddCurrency(
	            player, kCurrencyGold, static_cast<int64_t>(settlement.gold_gain()));
	        if (err == kAssetBlocked)
	        {
	            goldBlocked = true;
	            LOG_WARN << "[PlayerBattle] metric=battle_settlement_gold_blocked player_id=" << playerId
	                     << " battle_id=" << battleId << " gold=" << settlement.gold_gain()
	                     << ",gold gain blocked(GM or global gain block);gold skipped, rest of settlement applied";
	        }
	        else if (err != kSuccess)
	        {
	            LOG_WARN << "[PlayerBattle] Currency credit failed;settlement retained for redelivery: player_id=" << playerId
	                     << " battle_id=" << battleId << " err=" << err;
	            return false;
	        }
	    }
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
		// 离线挂起结算在登录后补应用时也走这里,登录时的复活判定早已跑完、拦不住)。
		if (settlement.is_dead() || health == 0)
		{
			// 回满到玩家真实上限(属性加点算出的 DerivedAttributesComp)。
			// 只回 ClassTable 的 1 级初值会把等级/加点成长吞掉(20 级 max_health 约 1100,初值 500)。
			ReviveBaseAttributesIfDead(*baseAttributes,
				derived.max_health(), derived.max_mana());
			LOG_INFO << "[PlayerBattle] Basic revival after death in settlement: player_id=" << playerId
					 << " battle_id=" << settlement.battle_id()
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
				if (fromInbox && !inboxPets.contains(petSettlement.pet_id())) continue;
				PetSystem::ApplyBattleSettlement(player, petSettlement);
			}
			if (!fromInbox || !inboxPets.empty()) PetSystem::PushList(player);
		}

		// 经验系统尚未接入,一期仅记日志留痕;升级后果见 PlayerAttributeSystem::GmSetLevel,
		// 经验系统接入时抽出共用(见 open_issues)。
		if (settlement.exp_gain() > 0)
		{
			LOG_INFO << "[PlayerBattle] Experience settlement deferred(experience system not integrated): player_id=" << playerId
					 << " battle_id=" << settlement.battle_id() << " exp=" << settlement.exp_gain();
		}

		// 道具:先扣消耗、再发掉落(G1/G3)。
		//
		// **这一段任何失败都不能让整笔结算失败**:走到这里金币已经入账,返回 false
		// 会让应用缓存把这次记录抹掉、pending 保留,重投时金币会再加一次
		// (battle_settlement_application_cache.h 的契约:返回 false 必须在不可重复的
		// 副作用之前)。所以道具的失败一律记日志 + 继续,由流水与告警去兜。
		if (!fromInbox && (settlement.items_consumed_size() > 0 || settlement.items_gained_size() > 0))
		{
			ApplySettlementItems(player, playerId, settlement);
		}

	    // 只有 battle 最终结算携带的真实击杀事实推进任务，由同一应用缓存保护金币和进度。
	    // 每只怪一条事实，顺序任务不会用同一次击杀跨过后续相同目标。
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
		// 登记「这一局已应用」。必须在所有副作用之后、返回 true 之前:提前登记会让中途
		// 失败的结算被永久判成已应用。条目与上面这些资产改动在同一份 player_database blob 里,
		// 下一次落盘要么一起落、要么一起没 —— 这正是幂等能跨进程重启生效的全部原因。
		// 这里重新取一次,不复用开头那个指针:中途派发的任务事件等若销毁了别的实体,entt 的
		// swap-and-pop 会挪动同类组件,旧指针不再可靠。开头已确认过账本在,此处缺失只可能是
		// 本实体在应用途中被拆掉 —— 副作用已发生、撤不回,仍返回 true 让应用缓存记成已完成
		// (返回 false 会让重投再发一次奖,违反应用缓存契约),只大声报错交人工按流水核对。
		if (fromInbox) return true; // 永久Inbox域水位由调用方与本次效果同帧记入同一快照。
		auto* recordLedger = tlsEcs.actorRegistry.try_get<BattleSettlementLedgerComp>(player);
		if (recordLedger == nullptr)
		{
			LOG_ERROR << "[PlayerBattle] metric=battle_settlement_ledger_missing Settlement side effects applied but ledger entry could not be recorded, player_id="
					  << playerId << " battle_id=" << battleId
					  << ",only the in-process cache blocks reapplication;persistent idempotency lost for this battle";
		}
		else if (battle_settlement::RecordApplied(*recordLedger, battleId, TimeSystem::NowMillisecondsUTC()))
		{
			LOG_ERROR << "[PlayerBattle] metric=battle_settlement_ledger_evicted player_id=" << playerId
					  << " battle_id=" << battleId << ",ledger full(consecutive "
					  << battle_settlement::kMaxAppliedRecords
					  << " battle acknowledgments unconfirmed,indicating prolonged Redis conditional deletion failure);oldest entry evicted;"
						 "redelivery of the evicted battle will grant rewards again";
		}
		LOG_INFO << "[PlayerBattle] Settlement applied: player_id=" << playerId
				 << " battle_id=" << settlement.battle_id()
				 << " outcome=" << settlement.outcome()
				 << " health=" << health << " mana=" << settlement.mana()
				 << " gold=" << settlement.gold_gain()
				 << " gold_blocked=" << goldBlocked
				 << " is_dead=" << settlement.is_dead()
				 << " rounds=" << settlement.total_rounds();
        return true;
    };
    if (fromInbox) return apply();
    const auto status = gSettlementApplications.Apply(playerId, battleId, ApplicationCache::Clock::now(), apply);
    if (status == ApplicationCache::Result::Applied)
    {
        // 立刻压一次存盘,把「已应用但没落盘」的窗口从一个周期存盘压到一次 Redis 往返。
        // 这一步在 pending 每玩家单槽的前提下是必需的:槽位被占太久,玩家打下一场时
        // battle 侧的无条件 SET 会覆盖掉上一场的 payload,那一局就再无持久副本。
        RequestSettlementPersist(player, playerId, battleId);
        return true;
    }
    if (status == ApplicationCache::Result::Duplicate)
    {
        // 防御分支:应用前已按账本清掉陈旧的已完成项,正常走不到。真走到了说明缓存与账本的约定被破坏,
        // 不应用、不销账,保留 pending 等重投,并告警。
        LOG_ERROR << "[PlayerBattle] Settlement cache unexpectedly reported duplicate(stale entry cleared before application), player_id=" << playerId
                  << " battle_id=" << battleId << ",retained for redelivery";
        return false;
    }
    // InFlight 不能提前 ACK；Failed/Full 保留记录供重投，避免永久吞掉合法结算。
    LOG_WARN << "[PlayerBattle] Settlement not applied this time;retained for redelivery: player_id=" << playerId
             << " battle_id=" << battleId << " status=" << static_cast<int>(status);
    return false;
}

void PlayerBattleSystem::ApplySettlement(const ::BattleSettlementEvent& event)
{
	const auto& settlement = event.settlement();
	const uint64_t playerId = settlement.player_id();
	const uint64_t battleId = settlement.battle_id();
	if (playerId == 0 || battleId == 0)
	{
		LOG_ERROR << "[PlayerBattle] Invalid settlement event: player_id=" << playerId << " battle_id=" << battleId;
		return;
	}

	const auto player = tlsEcs.GetPlayer(playerId);
	// 退出中(UnregisterPlayer:退出存盘在途 / 等收敛)的实体视同离线(cross-zone-scene-travel.md §12.6.3,
	// M1 第 2 条):在线应用会在退出存盘之后改实体,而应用后立刻销账 —— 退出收尾若没把这笔差额存下来
	// (旧实现的"落地即销毁"就是这样),奖励就两头落空。走离线暂存:按锁校验后 StorePendingSettlementIfLockMatch、
	// **不销账**,玩家下次进场由 OnPlayerEnterScene 补应用(同节点重连复用实体时 EnterScene 第 6 步同样会补)。
	if (!tlsEcs.actorRegistry.valid(player) || tlsEcs.actorRegistry.any_of<UnregisterPlayer>(player))
	{
		// 玩家已下线(lease 过期被清理)或正在退出:写离线挂起结算。
		// 锁值 == battle_id 才写,校验与写入在同一条 Lua 里;锁被另一局持有才销账丢弃;
		// 锁不在则什么都不动,留给登录钩子按账本裁决(StorePendingSettlementIfLockMatch 的说明)。
		if (!RedisReady())
		{
			LOG_ERROR << "[PlayerBattle] Offline settlement staging failed(Redis disconnected), player_id=" << playerId
					  << " battle_id=" << battleId << ",this settlement is lost(Kafka redelivery can recover it)";
			return;
		}
		std::string payload;
		if (!event.SerializeToString(&payload))
		{
			LOG_ERROR << "[PlayerBattle] Settlement event serialization failed, player_id=" << playerId
					  << " battle_id=" << battleId;
			return;
		}
		StorePendingSettlementIfLockMatch(playerId, battleId, payload);
		return;
	}

	// 在线路径:InBattleComp.battle_id 匹配才应用(不变量:摘除后再来的同 id 结算丢弃)
	const auto* inBattle = tlsEcs.actorRegistry.try_get<InBattleComp>(player);
	if (inBattle != nullptr && inBattle->battle_id() != battleId)
	{
		LOG_WARN << "[PlayerBattle] Settlement discarded: battle_id mismatch(duplicate delivery or invalidated), player_id=" << playerId
				 << " event_battle_id=" << battleId << " in_battle_id=" << inBattle->battle_id();
		// 条件销账:只删仍指向本局的记录。玩家已经在打下一场时,记录早被那一局覆盖,
		// 这里是无害的空操作;而真的是"重复投递"时,它让 battle 停止重投。
		AckSettlementPending(playerId, battleId);
		return;
	}
	if (inBattle == nullptr)
	{
		// 在线但没有 InBattleComp:玩家战斗中完整下线再登录(组件不落库)且登录重建未命中/未完成,
		// 或备战到期被 reaper 摘组件后确认与结算都迟到。锁值 == battle_id 证明这场战斗仍是玩家的
		// 当前战斗(期间没有进第二场),结算必须应用而不是丢弃;锁不在才是真正的重复投递/已作废。
		if (!RedisReady())
		{
			LOG_ERROR << "[PlayerBattle] Settlement discarded: no InBattleComp and Redis disconnected;cannot verify lock, player_id="
					  << playerId << " battle_id=" << battleId << "(Kafka redelivery can recover it)";
			return;
		}
		tlsRedis.GetZoneRedis()->command(
			[player, playerId, battleId, event](hiredis::Hiredis*, redisReply* reply) {
                // 无回复/Redis 错误不是锁不存在，不能销账丢掉可恢复的奖励。
                if (reply == nullptr ||
                    (reply->type != REDIS_REPLY_STRING && reply->type != REDIS_REPLY_NIL))
                {
                    LOG_WARN << "[PlayerBattle] Settlement lock lookup failed;retained for redelivery: player_id=" << playerId
                             << " battle_id=" << battleId;
                    return;
                }
				if (reply == nullptr || reply->type != REDIS_REPLY_STRING ||
					std::string(reply->str, reply->len) != std::to_string(battleId))
				{
					LOG_WARN << "[PlayerBattle] Settlement discarded: no InBattleComp and lock missing/value mismatch(duplicate delivery or invalidated), player_id="
							 << playerId << " battle_id=" << battleId;
					AckSettlementPending(playerId, battleId);
					return;
				}
				if (!tlsEcs.actorRegistry.valid(player) || tlsEcs.GetPlayerId(player) != playerId ||
					tlsEcs.actorRegistry.any_of<UnregisterPlayer>(player))
				{
					// 回调期间玩家又下线了 / 进入了退出流程(视同离线,理由见本函数开头):
					// 锁还在,交给 Kafka 重投走离线暂存路径
					LOG_WARN << "[PlayerBattle] Settlement application by lock abandoned: entity no longer exists, player_id=" << playerId
							 << " battle_id=" << battleId << "(Kafka redelivery can recover it)";
					return;
				}
				if (const auto* current = tlsEcs.actorRegistry.try_get<InBattleComp>(player);
					current != nullptr && current->battle_id() != battleId)
				{
					LOG_WARN << "[PlayerBattle] Settlement discarded: player entered another battle during callback, player_id=" << playerId
							 << " battle_id=" << battleId << " in_battle_id=" << current->battle_id();
					return;
				}
				LOG_WARN << "[PlayerBattle] metric=battle_settlement_applied_by_lock player_id=" << playerId
						 << " battle_id=" << battleId << ",no InBattleComp;apply settlement after matching lock value";
				bool alreadyApplied = false;
				if (!ApplySettlementToEntity(player, event.settlement(), &alreadyApplied)) return;
				// 组件可能在回调期间被登录重建/迟到确认挂回来(同 battle_id):一并摘除;
				// 锁留到落盘销账才删(ReleaseFreezeKeepLock 的说明)
				ReleaseFreezeKeepLock(player, playerId, battleId);
				// 销账 = 给 battle 侧发件箱的 ACK(R07),必须在应用之后
				AckSettlementPending(playerId, battleId);
				if (!alreadyApplied) PushBattleEndToPlayer(player, event.settlement());
			},
			(std::string("GET ") + kBattleLockKeyFmt).c_str(), playerId);
		return;
	}

	bool alreadyApplied = false;
	if (!ApplySettlementToEntity(player, settlement, &alreadyApplied)) return;
	// 摘冻结但**锁留到落盘销账**(ReleaseFreezeKeepLock 的说明);销账那条 Lua 连锁一起删
	ReleaseFreezeKeepLock(player, playerId, battleId);
	// 销账 = 给 battle 侧发件箱的 ACK(R07):battle 探测到伴生 id 键已不属于本局就停止重投。
	// 顺序不能反 —— 先销账后应用的话,应用中途进程崩溃就两头落空。
	//
	// 「应用完成 → 销账落地前崩溃」不再重复入账:这一局已记进与资产同一份 blob 的账本,
	// 崩溃则账本与资产一起没(重投/登录会重新应用),落盘则一起在(重投/登录被账本挡掉),
	// 见 turn-battle-gap-closure.md §9.3。用"锁还在才补应用"来收紧登录补应用仍然是错的:
	// 玩家离线超过锁 TTL(deadline+60s)后锁自然过期,而待结算记录不会过期(landing-recovery §7),
	// 按锁 gating 会把这类**合法**的离线结算判掉,那是真丢奖励。
	AckSettlementPending(playerId, battleId);
	if (!alreadyApplied) PushBattleEndToPlayer(player, settlement);
}

void PlayerBattleSystem::ApplyPendingSettlement(entt::entity player, const ::BattleSettlementEvent& event)
{
	const auto& settlement = event.settlement();
	const uint64_t playerId = settlement.player_id();

	bool alreadyApplied = false;
	if (!ApplySettlementToEntity(player, settlement, &alreadyApplied)) return;

	// 理论上离线结算与"实体上还挂着 InBattleComp"不共存(实体重建后组件不落库),
	// 防御性摘除:同 battle_id 才摘,避免误伤登录后刚备战的新战斗
	const auto* inBattle = tlsEcs.actorRegistry.try_get<InBattleComp>(player);
	const bool removeInBattle = inBattle != nullptr && inBattle->battle_id() == settlement.battle_id();

	// 销账 + 放锁都交给 AckSettlementPending:落盘了就在同一条 Lua 里条件删 pending 与锁
	// (锁删除后 match 才放行下一次排队);还没落盘就都留着,锁先续到覆盖重投窗口。
	// 两者都按 battle_id **条件删**:登录钩子理论上早于任何备战,但一旦顺序反过来,
	// 无条件 DEL 会把玩家刚开始的下一场的记录/锁抹掉。
	HoldBattleLockUntilDurable(playerId, settlement.battle_id());
	AckSettlementPending(playerId, settlement.battle_id());

	if (removeInBattle)
	{
		RemoveInBattleComp(player);
	}

	if (!alreadyApplied) PushBattleEndToPlayer(player, settlement);

	// 登录钩子只在「没有待结算记录」时才按锁重建冻结。可锁在落盘销账前一直留着,
	// 玩家也可能在上一局还没销账时已经开了下一场(锁自然过期等极端情况):这里补一次按锁重建。
	// 锁指向刚补应用的这一局时,RestoreBattleFreezeOnLogin 查账本命中会跳过重建;指向下一场
	// 才重建并提示客户端补签重连(评审 wne803bj5 #8)。只在补应用成功后做:失败时锁与 pending 都指向这一局,
	// 重建会把玩家冻进一场已经结束的战斗、让客户端去重连一个不存在的房间。
	// 同一条连接 FIFO:上面的销账/续锁命令排在这次读锁之前。
	if (!tlsEcs.actorRegistry.any_of<InBattleComp>(player))
	{
		RestoreBattleFreezeOnLogin(player, playerId);
	}
	LOG_INFO << "[PlayerBattle] Pending offline settlement applied: player_id=" << playerId
			 << " battle_id=" << settlement.battle_id();
}

void PlayerBattleSystem::RecoverPendingSettlementAfterFreezeExpiry(entt::entity player, const uint64_t playerId)
{
	// 「没读到」(Redis 不可用 / 空应答 / ERROR / 意外类型 / 命令没发出去)都记入登录复查(BattleLoginRecheckComp 的说明):
	// 这是到期后唯一一次主动补读(battle 发件箱已停止重投),以前只留痕、等下次进场,在线玩家就一直拿不到奖励。
	// 复查重跑的是登录钩子第一步:有记录照样走 ApplyPendingSettlement,没有记录再按锁重建。
	// 锁已由 reaper 在这次补读之前条件删掉;删锁没落地时锁还指向刚作废的这一局,按锁重建的入口认得它
	// (ReleaseExpiredFightingFreeze 先记下了 BattleFreezeExpiredComp):不重建,补一次条件删锁(2026-10-19)。
	if (!RedisReady())
	{
		LOG_WARN << "[PlayerBattle] Pending settlement recovery after freeze expiry skipped(Redis disconnected), player_id="
				 << playerId << "(pending settlement retained;login recheck scheduled)";
		MarkLoginRecheck(player, playerId, "freeze_expiry_pending_read_redis_disconnected");
		return;
	}
	// entt::entity 用 init-capture(MSVC C3495,见 copilot-instructions)。
	const int ret = tlsRedis.GetZoneRedis()->command(
		[player = player, playerId](hiredis::Hiredis*, redisReply* reply) {
			// 身份复核与 OnPlayerEnterScene 的回调同一写法:实体已销毁 / 句柄被复用就什么都不做,
			// pending 留给下次进场的登录钩子。
			if (!tlsEcs.actorRegistry.valid(player) || tlsEcs.GetPlayerId(player) != playerId)
			{
				return;
			}
			const auto shape = battle_settlement::ReplyShapeOf(reply);
			switch (battle_settlement::ClassifyLoginPendingRead(shape))
			{
			case battle_settlement::LoginPendingRead::kNoPending:
				// 没有待结算记录:只解冻,与改造前一致。
				return;
			case battle_settlement::LoginPendingRead::kRecheck:
				// 读失败不能当成"没有记录"吞掉:留下痕迹分清"没有记录"与"没读到",并记入登录复查。
				LOG_WARN << "[PlayerBattle] Pending settlement read failed during freeze-expiry recovery, player_id=" << playerId
						 << " reply=" << battle_settlement::ReplyShapeName(shape)
						 << " err=" << std::string(battle_settlement::ReplyText(reply))
						 << "(pending settlement retained;login recheck scheduled)";
				MarkLoginRecheck(player, playerId, "freeze_expiry_pending_read_unusable_reply");
				return;
			case battle_settlement::LoginPendingRead::kPending:
				break;
			}
			if (reply->len > static_cast<size_t>(std::numeric_limits<int>::max()))
			{
				LOG_ERROR << "[PlayerBattle] Pending settlement payload too large, player_id=" << playerId;
				return;
			}
			::BattleSettlementEvent event;
			if (!event.ParseFromArray(reply->str, static_cast<int>(reply->len)))
			{
				// 不在这里删坏记录:补读路径不扩大删除面,清理留给登录钩子(它会连同积压索引成员一起删)。
				LOG_ERROR << "[PlayerBattle] Pending settlement parse failed during freeze-expiry recovery;left for the login hook, player_id="
						  << playerId;
				return;
			}
			LOG_WARN << "[PlayerBattle] metric=battle_settlement_freeze_expiry_pending_hit player_id=" << playerId
					 << " battle_id=" << event.settlement().battle_id()
					 << ",battle freeze expired with a pending settlement;applying it now instead of waiting for next scene entry";
			ApplyPendingSettlement(player, event);
		},
		(std::string("GET ") + kPendingSettlementKeyFmt).c_str(), playerId);
	if (ret != REDIS_OK)
	{
		// 命令没发出去时回调永远不会被调:就地留痕并记入登录复查。
		LOG_WARN << "[PlayerBattle] Pending settlement recovery after freeze expiry not sent, player_id=" << playerId
				 << " ret=" << ret << "(pending settlement retained;login recheck scheduled)";
		MarkLoginRecheck(player, playerId, "freeze_expiry_pending_read_not_sent");
	}
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

void PlayerBattleSystem::CheckPendingSettlementOnEnter(entt::entity player, const uint64_t playerId)
{
	if (!RedisReady())
	{
		// 以前只打一条 WARN、等下一次登录:玩家一直在线就一直拿不到挂起结算,战斗在途时冻结也不重建。
		MarkLoginRecheck(player, playerId, "pending_read_redis_disconnected");
		return;
	}
	const int rc = tlsRedis.GetZoneRedis()->command(
		[player, playerId](hiredis::Hiredis*, redisReply* reply) {
			if (!tlsEcs.actorRegistry.valid(player) || tlsEcs.GetPlayerId(player) != playerId)
			{
				// 回调期间实体已销毁/复用:留着 pending,下次登录再应用
				return;
			}
			const auto shape = battle_settlement::ReplyShapeOf(reply);
			switch (battle_settlement::ClassifyLoginPendingRead(shape))
			{
			case battle_settlement::LoginPendingRead::kRecheck:
				// 空应答 / ERROR / 意外类型:没问到。以前与 NIL 走同一分支,当成「没有挂起结算」去按锁重建 ——
				// 这次登录就漏补发奖励。复查完成前不调 RestoreBattleFreezeOnLogin(重建依赖「确实没有挂起结算」)。
				MarkLoginRecheck(player, playerId,
								 shape == battle_settlement::ReplyShape::kNoReply ? "pending_read_no_reply"
																				  : "pending_read_unusable_reply");
				return;
			case battle_settlement::LoginPendingRead::kNoPending:
				// 无挂起结算 -> 查锁重建(重连路径组件仍在时函数内部直接跳过)
				if (!tlsEcs.actorRegistry.any_of<InBattleComp>(player))
				{
					RestoreBattleFreezeOnLogin(player, playerId);
				}
				return;
			case battle_settlement::LoginPendingRead::kPending:
				break;
			}
			if (reply->len > static_cast<size_t>(std::numeric_limits<int>::max()))
			{
				LOG_ERROR << "[PlayerBattle] Pending settlement payload too large, player_id=" << playerId;
				return;
			}
			::BattleSettlementEvent event;
			if (!event.ParseFromArray(reply->str, static_cast<int>(reply->len)))
			{
				LOG_ERROR << "[PlayerBattle] Pending settlement parse failed;invalid data removed, player_id=" << playerId;
				if (RedisReady())
				{
					// 脏数据解不出 battle_id,无从条件删;两键连同积压索引成员一起无条件删
					// (kDeleteCorruptPendingSettlementScript 的说明)。记录不再过期,不删就永远躺在那里。
					tlsRedis.GetZoneRedis()->command(
						[](hiredis::Hiredis*, redisReply*) {},
						(std::string("EVAL %s 3 ") + kPendingSettlementKeyFmt + " " +
						 kPendingSettlementIdKeyFmt + " " + kPendingSettlementIndexKey + " %llu").c_str(),
						battle_settlement::kDeleteCorruptPendingSettlementScript, playerId, playerId, playerId);
				}
				// 这里**不**按锁重建:锁多半正属于这条坏记录的那一局(已结束、从没应用、账本里没有),
				// 重建会把玩家冻进一场已结束的战斗并让客户端去重连不存在的房间(第二轮评审 w0zw729x1 #1)。
				// 锁留给 TTL 过期;真有另一局在途时,它的结算/迟到确认会按锁匹配路径重建。
				return;
			}
			ApplyPendingSettlement(player, event);
		},
		(std::string("GET ") + kPendingSettlementKeyFmt).c_str(), playerId);
	if (rc != REDIS_OK)
	{
		// 命令没发出去时回调永远不会来:与 Redis 不可用同样记入复查。
		MarkLoginRecheck(player, playerId, "pending_read_not_sent");
	}
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
	// 先收集再处理:CheckPendingSettlementOnEnter 可能当场重新挂标记(命令没发出去),边遍历边改视图会失效迭代器。
	std::vector<entt::entity> due(view.begin(), view.end());
	for (const auto entity : due)
	{
		const uint64_t playerId = tlsEcs.GetPlayerId(entity);
		battle_settlement::LoginRecheckEntryState state;
		state.exitingOrNoIdentity = playerId == 0 || tlsEcs.actorRegistry.any_of<UnregisterPlayer>(entity);
		state.travelHandoffOrFrozen = !IsSettlementApplicable(entity);
		state.redisReady = redisReady;
		// 各分支的理由见 settlement_outbox.h 的 ClassifyLoginRecheckEntry(退出中 > 交接中 > Redis 不可用 > 重跑)。
		switch (battle_settlement::ClassifyLoginRecheckEntry(state))
		{
		case battle_settlement::LoginRecheckStep::kDrop:
			tlsEcs.actorRegistry.remove<BattleLoginRecheckComp>(entity);
			LOG_INFO << "[PlayerBattle] Login battle recheck dropped: player is exiting, player_id=" << playerId;
			continue;
		case battle_settlement::LoginRecheckStep::kKeep:
			if (!state.travelHandoffOrFrozen)
			{
				++deferred; // 只为汇总日志计数 Redis 不可用的那部分
			}
			continue;
		case battle_settlement::LoginRecheckStep::kRun:
			break;
		}
		// 先摘标记再重跑:这一轮又没问到会重新挂上,同一玩家同一时刻最多一条复查在途。
		tlsEcs.actorRegistry.remove<BattleLoginRecheckComp>(entity);
		LOG_WARN << "[PlayerBattle] metric=battle_login_recheck_run player_id=" << playerId
				 << ",rerunning login pending-settlement check and freeze restore";
		CheckPendingSettlementOnEnter(entity, playerId);
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

bool PlayerBattleSystem::DecideLockRebuildFor(entt::entity player, const uint64_t lockBattleId,
											  battle_settlement::LockRebuildDecision& out)
{
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return false;
	}
	// 只读:与两个重建入口不同,这里不经 FindSettlementLedger(它会对缺账本打 ERROR),缺账本直接返回 false。
	const auto* ledger = tlsEcs.actorRegistry.try_get<BattleSettlementLedgerComp>(player);
	if (ledger == nullptr)
	{
		return false;
	}
	out = DecideLockRebuild(player, lockBattleId, *ledger);
	return true;
}

void PlayerBattleSystem::ReleaseExpiredFightingFreeze(entt::entity player, const uint64_t playerId,
													  const uint64_t battleId)
{
	// 先记下作废的局,再删锁:删锁没落地时,之后任何一条按锁重建的路径都靠它认出这把锁(BattleFreezeExpiredComp 的说明)。
	RememberExpiredFightingBattle(player, battleId);
	// 按 battle_id 条件删锁:作废判定与锁值同源,不会误删别的战斗
	ClearBattleFreeze(player, playerId, battleId);
	// 补读一次待结算记录(设计 §7):到期常常是因为结算投递没送到,而 battle 只重投 120 秒,
	// 在线玩家又等不到登录钩子。退出中的实体(UnregisterPlayer)视同离线,不补读 ——
	// ApplySettlementToEntity 本来也会拒绝,读了只是白跑一次;它的结算留给下次进场。
	// 读命令排在上面的条件删锁之后(同连接 FIFO):补应用之后的按锁重建看到的是删锁之后的状态。
	if (playerId != 0 && tlsEcs.actorRegistry.valid(player) &&
		!tlsEcs.actorRegistry.any_of<UnregisterPlayer>(player))
	{
		RecoverPendingSettlementAfterFreezeExpiry(player, playerId);
	}
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

	// 1) 离线挂起结算:所有登录类型都查(先应用挂起结算再放开排队,§3.2)。
	//    没有挂起结算时,再看 battle:lock 是否还在 —— 在则说明战斗仍在途而实体是新建的
	//    (InBattleComp 不落库),按锁 + ctx 重建冻结(FIGHTING 时提示客户端补签重连),
	//    后续结算才能在线命中。
	//    两步必须串在同一条回调链上:挂起结算应用后,销账/续锁命令要排在读锁之前(同连接 FIFO)。
	//    锁在结算应用后一直留到落盘销账(ReleaseFreezeKeepLock),所以按锁重建前还要查账本:
	//    锁指向已应用的局就不重建(RestoreBattleFreezeOnLogin / RebuildBattleFreezeFromLock 内)。
	//    任一步「没问到」(空应答 / 读记录时的 ERROR / 命令没发出去 / Redis 不可用)都不当结论:
	//    记入登录复查,reaper 节拍在 Redis 可用时重跑这一步(BattleLoginRecheckComp 的说明)。
	CheckPendingSettlementOnEnter(player, playerId);

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
		const uint64_t nowMs = TimeSystem::NowMillisecondsUTC();

		// 先收集再摘除:边遍历边 remove 会失效视图迭代器
		struct ExpiredEntry
		{
			entt::entity entity;
			uint64_t battleId;
			bool preparing;
			uint64_t deadlineMs;
		};
		std::vector<ExpiredEntry> expired;
		for (auto&& [entity, inBattle] : tlsEcs.actorRegistry.view<InBattleComp>().each())
		{
			// PREPARING 看备战期限(CreateBattle 确认前 match 崩溃只等短期限),
			// FIGHTING 看正式期限;prepare_deadline_ms 为 0(旧版 match/存量组件)时退回 deadline_ms
			const bool preparing = inBattle.state() == IN_BATTLE_STATE_PREPARING;
			const uint64_t effectiveDeadlineMs =
				(preparing && inBattle.prepare_deadline_ms() != 0) ? inBattle.prepare_deadline_ms()
																	: inBattle.deadline_ms();
			if (effectiveDeadlineMs != 0 && effectiveDeadlineMs < nowMs)
			{
				expired.push_back({entity, inBattle.battle_id(), preparing, effectiveDeadlineMs});
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
					 << " now_ms=" << nowMs << ",battle invalidated and freeze released(battle node crashed or settlement event lost)";
			ReleaseExpiredFightingFreeze(entry.entity, playerId, entry.battleId);
		}

		// 第二遍:排空「已应用但销账尚未确认」的账本条目。
		//
		// 为什么需要它:销账被改成必须等落盘之后,而推动重试的只有 battle 侧发件箱,
		// 它只重投 12 轮(120s)就放弃 —— 之后再没有任何人会来碰这条 pending。
		// 没有这一遍,条目会一直挂到玩家下次登录,pending 也一直白占(不再过期),还会被下一局的
		// 无条件 SET 覆盖。有了它,排空只依赖本节点自己的定时器,不依赖对端是否还在重投。
		//
		// 账本在加载时创建，view 覆盖全部在线玩家；稳态只是条目为空，由下方 applied_size 跳过。
		struct PendingAck
		{
			uint64_t playerId;
			uint64_t battleId;
		};
		std::vector<PendingAck> pendingAcks;
		for (auto&& [entity, ledger] : tlsEcs.actorRegistry.view<BattleSettlementLedgerComp>().each())
		{
			if (ledger.applied_size() == 0) continue;
			const uint64_t playerId = tlsEcs.GetPlayerId(entity);
			if (playerId == 0) continue;
			// 先收集再处理:AckSettlementPending 会按 player_id 重新取实体并可能触发存盘,
			// 边遍历边调用等于在遍历中改注册表。
			for (const auto& applied : ledger.applied())
			{
				pendingAcks.push_back({playerId, applied.battle_id()});
			}
		}
		for (const auto& ack : pendingAcks)
		{
			AckSettlementPending(ack.playerId, ack.battleId);
		}

		// 第三遍:登录钩子「没问到」的玩家重跑登录检查(BattleLoginRecheckComp 的说明)。
		// 排在前两遍之后:同一条连接 FIFO,补读与销账命令先于这里的读数到达 Redis。
		RunLoginRechecks();
	});
	gReaperActive = true;

	LOG_INFO << "[PlayerBattle] Battle freeze reaper started, interval=" << kReaperIntervalSec << "s";
}

void PlayerBattleSystem::SetPersistFnForTest(PersistFn fn)
{
	gPersistFn = (fn == nullptr) ? &PlayerLifecycleSystem::SavePlayerToRedis : fn;
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
