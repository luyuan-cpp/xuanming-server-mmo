#include "player_battle.h"

#include <muduo/base/Logging.h>
#include <muduo/net/EventLoop.h>
#include <muduo/net/TimerId.h>
#include <muduo/contrib/hiredis/Hiredis.h>
#include <hiredis/hiredis.h>

#include <algorithm>
#include <exception>
#include <limits>
#include <string>
#include <utility>
#include <vector>

#include "engine/core/time/system/time.h"
#include "engine/infra/messaging/kafka/kafka_producer.h"
#include "node/system/node/node_command_route.h"
#include "network/network_utils.h"
#include "network/node_utils.h"
#include "network/player_message_utils.h"
#include "thread_context/ecs_context.h"
#include "thread_context/node_context_manager.h"
#include "thread_context/redis_manager.h"

#include "actor/attribute/constants/actor_state_attribute_calculator_constants.h"
#include "actor/attribute/system/actor_attribute_calculator.h"
#include "combat/buff/comp/buff_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "player/comp/player_frozen_comp.h"
#include "player/system/player_pet.h"
#include "player/system/player_revive.h"

// 时间->回合换算与回合常量的权威定义在回合引擎库(scene 可以依赖 battle 常量,反向禁止)。
#include "services/battle/constants/turn_battle_constants.h"
// 待结算记录的 Redis key / Lua 契约(R07)。battle 侧写、scene 侧读与销账,
// 两端必须用**同一份**常量 —— 手抄两遍就是等着某天改一处漏一处。
#include "services/battle/settlement/settlement_outbox.h"
// 战斗配表指纹(六张战斗表确定性序列化 sha256,随快照携带给 match/battle 比对)。
#include "services/battle/data/battle_table_fingerprint.h"

#include "table/code/buff_table.h"
#include "table/proto/tip/common_error_tip.pb.h"

#include "proto/common/base/common.pb.h"
#include "proto/common/component/actor_attribute_state_comp.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/player_login_comp.pb.h"
#include "proto/common/component/player_network_comp.pb.h"
#include "proto/common/component/player_skill_comp.pb.h"
#include "proto/contracts/kafka/gate_command.pb.h"

// —— 以下生成产物当前尚未生成,proto 重生成后出现(路径按仓库生成器命名规律推断,
//    依据见任务返回 open_issues;battle 节点侧代码采用同一推断)——
#include "proto/battle/battle_data.pb.h"                              // proto/battle/battle_data.proto
#include "proto/common/component/battle_comp.pb.h"                    // InBattleComp / eInBattleState
#include "proto/common/event/battle_event.pb.h"                       // BattleSettlementEvent
#include "proto/scene/scene.pb.h"                                     // PrepareBattle* / CancelBattlePrepare*(重生成后新增消息)
#include "proto/contracts/kafka/gate_event.pb.h"                      // BindBattleEvent(重生成后新增)
#include "proto/battle/player_battle.pb.h"                            // BattleEndS2C / BattleReconnectS2C
#include "rpc/service_metadata/contracts_kafka_gate_event_event_id.h" // ContractsKafkaBindBattleEventEventId(重生成后新增)
#include "rpc/service_metadata/player_battle_service_metadata.h"      // BattleClientPlayerNotify*MessageId

namespace
{
	// Redis key 契约(设计文档 §6):
	//   battle:lock:{player_id} = battle_id           —— 战斗串行化咨询锁(match 读,只 EXISTS)
	//   battle:ctx:{player_id}  = InBattleComp 序列化 —— 锁的伴生上下文(scene 私有):
	//       battle_node_id / deadline_ms / state / prepare_deadline_ms,供"实体没了但锁还在"
	//       的路径重建冻结(完整下线再登录、备战到期后迟到的确认)。生命周期与锁完全同步:
	//       同时 SET / 同 TTL / 同一条 Lua 里 EXPIRE / DEL,只在锁存在且值匹配时才被信任。
	//   battle:settlement:pending:{player_id}    = event —— 待结算记录(TTL 7 天)
	//   battle:settlement:pending:id:{player_id} = battle_id —— 伴生 id(与上一条同 SET/同 TTL/同 DEL)
	//
	//   待结算记录的写者从"只有 scene 的离线分支"变成了"battle 结算时必写"(R07):
	//   battle 先落库再投递,scene 应用后**条件销账**(id 匹配才删),未销账的由 battle
	//   有界重投。因此 scene 的每一条"结算已定局"路径都必须销账 —— 漏一条的后果是
	//   battle 一直重投到次数用尽,而且玩家下次登录会被登录钩子**再发一次奖励**。
	constexpr char kBattleLockKeyFmt[] = "battle:lock:%llu";
	constexpr char kBattleCtxKeyFmt[] = "battle:ctx:%llu";
	constexpr const char *kPendingSettlementKeyFmt = battle_settlement::kPendingSettlementKeyFmt;
	constexpr const char *kPendingSettlementIdKeyFmt = battle_settlement::kPendingSettlementIdKeyFmt;

	// speed 缺配兜底:BaseAttributesComp.speed 为 0(存量数据未配置)时的默认出手速度。
	// 高于引擎的怪物默认速度(kMonsterDefaultSpeed=5),保证缺配玩家不至于永远后手。
	constexpr uint64_t kFallbackBattleSpeed = 10;

	// reaper 定时器(与 CrossZoneReaper 相同的单定时器形态)
	muduo::net::TimerId gReaperTimerId;
	bool gReaperActive = false;
	muduo::net::EventLoop* gReaperLoop = nullptr;

	bool RedisReady()
	{
		auto& redis = tlsRedis.GetZoneRedis();
		return redis && redis->connected();
	}

	uint64_t GuidForLog(entt::entity player)
	{
		const auto* g = tlsEcs.actorRegistry.try_get<Guid>(player);
		return g ? *g : 0;
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

	// 锁 TTL(秒)= 期限剩余 + 余量:锁必须活得比 InBattleComp 久
	uint64_t LockTtlSecFor(const uint64_t deadlineMs, const uint64_t nowMs)
	{
		const uint64_t remainSec = deadlineMs > nowMs ? (deadlineMs - nowMs) / 1000 : 0;
		return remainSec + PlayerBattleSystem::kLockExtraTtlSec;
	}

	// 条件删锁(连带 ctx):锁值 == battleId 才 DEL(所有删锁路径统一走这里,防迟到的取消/结算/作废
	// 误删玩家下一场战斗的锁)。fire-and-forget:锁是咨询性的,失败最迟随 TTL 过期。
	void DeleteBattleLockIfMatch(const uint64_t playerId, const uint64_t battleId)
	{
		if (!RedisReady())
		{
			// 非致命:锁带 EX,最迟随 TTL 过期;记日志说明串行化窗口靠 TTL 兜底
			LOG_WARN << "[PlayerBattle] 条件删锁跳过(Redis 未连接), player_id=" << playerId
					 << " battle_id=" << battleId << ", 锁将随 TTL 自然过期";
			return;
		}
		tlsRedis.GetZoneRedis()->command(
			[playerId, battleId](hiredis::Hiredis*, redisReply* reply) {
				if (reply == nullptr || reply->type == REDIS_REPLY_ERROR)
				{
					LOG_ERROR << "[PlayerBattle] 条件删锁 EVAL 失败, player_id=" << playerId
							  << " battle_id=" << battleId;
					return;
				}
				if (reply->type == REDIS_REPLY_INTEGER && reply->integer == 0)
				{
					// 锁不存在(已过期/已删)或已易主(玩家进了下一场):都不是错误
					LOG_INFO << "[PlayerBattle] 条件删锁未命中(锁不存在或值不匹配), player_id="
							 << playerId << " battle_id=" << battleId;
				}
			},
			(std::string("EVAL %s 2 ") + kBattleLockKeyFmt + " " + kBattleCtxKeyFmt + " %llu").c_str(),
			kDeleteLockIfMatchScript, playerId, playerId, battleId);
	}

	// 结算销账(R07):battle:settlement:pending:{id} 的值属于 battleId 才删两键。
	// 这就是 battle 侧发件箱的 ACK —— battle 每 10s 探测一次伴生 id 键,值不再等于
	// 自己的 battle_id 就停止重投。
	//
	// 必须**条件删**而不是无脑 DEL:玩家打完这一局立刻进下一局、下一局又结算时,
	// 一条迟到的旧结算若无条件删,就把**新一局**的待结算记录抹掉了(与 battle:lock
	// 的条件删同一条纪律)。fire-and-forget:失败最迟随 TTL 过期,代价是 battle 多重投几轮。
	void ClearPendingSettlementIfMatch(const uint64_t playerId, const uint64_t battleId)
	{
		if (!RedisReady())
		{
			LOG_WARN << "[PlayerBattle] 结算销账跳过(Redis 未连接), player_id=" << playerId
					 << " battle_id=" << battleId << ",battle 侧会重投到次数用尽(结算本身幂等)";
			return;
		}
		tlsRedis.GetZoneRedis()->command(
			[playerId, battleId](hiredis::Hiredis*, redisReply* reply) {
				if (reply == nullptr || reply->type == REDIS_REPLY_ERROR)
				{
					LOG_ERROR << "[PlayerBattle] 结算销账 EVAL 失败, player_id=" << playerId
							  << " battle_id=" << battleId;
				}
			},
			(std::string("EVAL %s 2 ") + kPendingSettlementKeyFmt + " " +
			 kPendingSettlementIdKeyFmt + " %llu").c_str(),
			battle_settlement::kDeletePendingSettlementIfMatchScript, playerId, playerId, battleId);
	}

	// 写待结算记录(两键同 TTL)。离线分支仍然写:battle 已经写过同一份,这里是
	// 幂等覆盖 + 续 TTL,同时兜住"老版本 battle 还没落库就投递"的灰度窗口。
	void StorePendingSettlement(const uint64_t playerId, const uint64_t battleId,
								const std::string& payload)
	{
		if (!RedisReady())
		{
			LOG_ERROR << "[PlayerBattle] 待结算记录写入失败(Redis 断连), player_id=" << playerId
					  << " battle_id=" << battleId;
			return;
		}
		tlsRedis.GetZoneRedis()->command(
			[playerId, battleId](hiredis::Hiredis*, redisReply* reply) {
				if (reply == nullptr || reply->type == REDIS_REPLY_ERROR)
				{
					LOG_ERROR << "[PlayerBattle] SET 待结算记录失败, player_id=" << playerId
							  << " battle_id=" << battleId;
				}
			},
			(std::string("EVAL %s 2 ") + kPendingSettlementKeyFmt + " " +
			 kPendingSettlementIdKeyFmt + " %b %llu %u").c_str(),
			battle_settlement::kSetPendingSettlementScript, playerId, playerId,
			payload.data(), payload.size(), battleId,
			PlayerBattleSystem::kPendingSettlementTtlSec);
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
			LOG_WARN << "[PlayerBattle] 条件续期跳过(Redis 未连接), player_id=" << playerId
					 << " battle_id=" << battleId << "(锁可能早于战斗结束过期,match 咨询性检查短暂失明)";
			return;
		}
		tlsRedis.GetZoneRedis()->command(
			[playerId, battleId, ttlSec](hiredis::Hiredis*, redisReply* reply) {
				if (reply == nullptr || reply->type == REDIS_REPLY_ERROR)
				{
					LOG_ERROR << "[PlayerBattle] 条件续期 EVAL 失败, player_id=" << playerId
							  << " battle_id=" << battleId;
					return;
				}
				if (reply->type == REDIS_REPLY_INTEGER && reply->integer == 0)
				{
					LOG_WARN << "[PlayerBattle] 条件续期未命中(锁不存在或值不匹配), player_id="
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
			LOG_WARN << "[PlayerBattle] 重建复核跳过(Redis 未连接), player_id=" << playerId
					 << " battle_id=" << battleId << "(冻结按 deadline 由 reaper 兜底)";
			return;
		}
		tlsRedis.GetZoneRedis()->command(
			[player, playerId, battleId, ttlSec](hiredis::Hiredis*, redisReply* reply) {
				if (reply == nullptr || reply->type == REDIS_REPLY_ERROR)
				{
					LOG_ERROR << "[PlayerBattle] 重建复核 EVAL 失败, player_id=" << playerId
							  << " battle_id=" << battleId;
					return;
				}
				if (reply->type == REDIS_REPLY_INTEGER && reply->integer != 0)
				{
					return; // 锁仍匹配:重建成立,ctx 已同步
				}
				if (!tlsEcs.actorRegistry.valid(player) || GuidForLog(player) != playerId)
				{
					return;
				}
				if (const auto* current = tlsEcs.actorRegistry.try_get<InBattleComp>(player);
					current != nullptr && current->battle_id() == battleId)
				{
					tlsEcs.actorRegistry.remove<InBattleComp>(player);
					LOG_WARN << "[PlayerBattle] metric=battle_freeze_rebuild_reverted player_id=" << playerId
							 << " battle_id=" << battleId << ",重建期间锁已被删(结算/取消已收尾),撤销重建";
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
		tlsEcs.actorRegistry.remove<InBattleComp>(player);
		DeleteBattleLockIfMatch(playerId, battleId);
	}

	// 推 BattleEndS2C(结算入账通知;battle 节点也会在战斗结束时推一份,
	// 客户端按 battle_id 幂等处理;离线补应用路径则只有这一份)
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
	const uint64_t playerId = GuidForLog(player);

	// —— 基础属性(必需;含 speed 出手序)——
	const auto* baseAttributes = tlsEcs.actorRegistry.try_get<BaseAttributesComp>(player);
	if (baseAttributes == nullptr)
	{
		LOG_ERROR << "[PlayerBattle] 组快照失败: 缺 BaseAttributesComp, player_id=" << playerId;
		errorTipId = kEntityIsNull;
		return false;
	}

	// —— 路由信息(必需;battle 节点出站 Kafka 全靠它,不查 etcd)——
	GateRoute gateRoute;
	if (!ResolveGateRoute(player, gateRoute))
	{
		LOG_ERROR << "[PlayerBattle] 组快照失败: 缺会话快照(玩家不在线?), player_id=" << playerId;
		errorTipId = kSessionNotFound;
		return false;
	}

	snapshot.set_player_id(playerId);
	// 昵称:scene 玩家实体当前没有昵称组件(display_name 只在 user 表),一期留空,见 open_issues
	snapshot.set_player_name("");

	const auto* levelComp = tlsEcs.actorRegistry.try_get<LevelComp>(player);
	snapshot.set_level(levelComp != nullptr && levelComp->level() > 0 ? levelComp->level() : 1);

	*snapshot.mutable_base_attributes() = *baseAttributes;
	if (baseAttributes->speed() == 0)
	{
		// speed 缺配(存量数据/表未填):给默认值保证出手序可结算,缺配现状见 open_issues
		snapshot.mutable_base_attributes()->set_speed(kFallbackBattleSpeed);
		LOG_WARN << "[PlayerBattle] speed=0, 使用默认出手速度 " << kFallbackBattleSpeed
				 << ", player_id=" << playerId;
	}

	// max_health:派生属性缺失时保守取当前 HP(不给引擎超治疗空间)
	const auto* derived = tlsEcs.actorRegistry.try_get<DerivedAttributesComp>(player);
	const uint64_t maxHealth = (derived != nullptr && derived->max_health() > 0)
								   ? derived->max_health()
								   : std::max<uint64_t>(baseAttributes->health(), 1);
	snapshot.set_max_health(maxHealth);
	// max_mana / 二级属性:来自属性加点系统的 DerivedAttributesComp(登录加载即重算);
	// 缺失时蓝上限取当前 MP(引擎内不回蓝越界),物伤/法伤/防御为 0 = 引擎老公式
	const uint64_t maxMana = (derived != nullptr && derived->max_mana() > 0)
								 ? derived->max_mana()
								 : std::max<uint64_t>(baseAttributes->mana(), 1);
	snapshot.set_max_mana(maxMana);
	if (derived != nullptr)
	{
		snapshot.set_physical_attack(derived->physical_attack());
		snapshot.set_magic_attack(derived->magic_attack());
		snapshot.set_defense(derived->defense());
	}

	// —— 出战宝宝(player-pet.md §5):没带宝宝时不填,引擎侧自然没有这个单位 ——
	if (::BattlePetSnapshot petSnapshot; PetSystem::BuildBattleSnapshot(player, petSnapshot))
	{
		*snapshot.add_pets() = petSnapshot;
	}

	// —— 技能列表 ——
	if (const auto* skillList = tlsEcs.actorRegistry.try_get<PlayerSkillListComp>(player))
	{
		for (const auto& skill : skillList->skill_list())
		{
			if (skill.skill_table_id() != 0)
			{
				snapshot.add_skill_table_ids(skill.skill_table_id());
			}
		}
	}

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
			auto* buffEntry = snapshot.add_buffs();
			buffEntry->set_buff_id(buffId);
			buffEntry->set_buff_table_id(entry.buffPb.buff_table_id());
			buffEntry->set_layer(std::max<uint32_t>(entry.buffPb.layer(), 1));
			buffEntry->set_caster_id(entry.buffPb.caster());
			// 剩余时长拿不到(TimerTaskComp 设计上不暴露 deadline,BuffComp 也没有到期字段),
			// 一期按表全量时长换算回合:rounds = max(1, ceil(ms / 6000));现状见 open_issues。
			if (buffRow->infinite_duration() != 0 || buffRow->duration() <= 0)
			{
				buffEntry->set_remain_rounds(0); // 0 = 无限持续
			}
			else
			{
				buffEntry->set_remain_rounds(turnbattle::RoundsFromSeconds(buffRow->duration()));
			}
		}
	}

	// —— 战斗道具副本:背包系统尚未挂载玩家实体(见 player/system/bag_marshal.h 现状说明),
	//    一期传空;接入后在此按"可战斗消耗品"过滤生成副本。——

	// —— 路由 ——
	auto* routing = snapshot.mutable_routing();
	routing->set_session_id(gateRoute.sessionId);
	routing->set_gate_node_id(gateRoute.gateNodeId);
	routing->set_gate_instance_id(gateRoute.gateInstanceId);
	if (gateRoute.gateInstanceId.empty())
	{
		// 不 fail-closed 拒绝备战:battle 节点出站时会按不变量 2 拒发并报错,
		// 这里先告警便于定位 gate 发现异常
		LOG_WARN << "[PlayerBattle] gate_instance_id 为空(本地未发现该 gate 节点), player_id="
				 << playerId << " session_id=" << gateRoute.sessionId;
	}
	const auto& selfNode = GetNodeInfo();
	routing->set_scene_node_id(selfNode.node_id());
	routing->set_scene_instance_id(selfNode.node_uuid());
	routing->set_zone_id(GetZoneId());

	// team_index:阵营分配是 match 的编排职责,scene 不感知,由 match 在 CreateBattle 前改写
	snapshot.set_team_index(0);

	// 配表指纹:本节点六张战斗表的指纹(表加载完成时已缓存),match 比对全员一致、
	// battle 开局时再与自身比对(cross-zone-matchmaking.md §10)
	snapshot.set_table_fingerprint(turnbattle::BattleTableFingerprint::Current());
	return true;
}

void PlayerBattleSystem::PrepareBattle(const ::PrepareBattleRequest& request, ::PrepareBattleResponse& response)
{
	const uint64_t playerId = request.player_id();
	const uint64_t battleId = request.battle_id();

	if (playerId == 0 || battleId == 0 || request.deadline_ms() == 0)
	{
		LOG_ERROR << "[PlayerBattle] PrepareBattle 参数非法: player_id=" << playerId
				  << " battle_id=" << battleId << " deadline_ms=" << request.deadline_ms();
		response.mutable_error_message()->set_id(kInvalidParameter);
		return;
	}

	const auto player = tlsEcs.GetPlayer(playerId);
	if (!tlsEcs.actorRegistry.valid(player))
	{
		// 玩家不在本节点(已下线或被改派):match 按补偿矩阵处理
		LOG_WARN << "[PlayerBattle] PrepareBattle 拒绝: 玩家不在线, player_id=" << playerId
				 << " battle_id=" << battleId;
		response.mutable_error_message()->set_id(kEntityIsNull);
		return;
	}

	// 跨 zone 迁移在途:实体只读,不接受新战斗
	if (tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(player))
	{
		LOG_WARN << "[PlayerBattle] PrepareBattle 拒绝: 玩家处于跨 zone 冻结, player_id=" << playerId
				 << " battle_id=" << battleId;
		response.mutable_error_message()->set_id(kFeatureUnavailable);
		return;
	}

	// 结算串行化(D4):上一场结算未落地前拒绝二次备战 —— 冻结拦截点之一
	if (const auto* existing = tlsEcs.actorRegistry.try_get<InBattleComp>(player))
	{
		LOG_WARN << "[PlayerBattle] PrepareBattle 拒绝: 已有战斗在途, player_id=" << playerId
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
		LOG_WARN << "[PlayerBattle] PrepareBattle 拒绝: 玩家 0 血(阵亡未复活), player_id=" << playerId
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
	const uint64_t prepareDeadlineMs =
		request.prepare_deadline_ms() != 0 ? request.prepare_deadline_ms() : request.deadline_ms();

	// 挂 InBattleComp(上面已确认不存在,直接 emplace;此处非 per-tick 路径)
	auto& inBattle = tlsEcs.actorRegistry.emplace<InBattleComp>(player);
	inBattle.set_battle_id(battleId);
	inBattle.set_battle_node_id(request.battle_node_id());
	inBattle.set_deadline_ms(request.deadline_ms());
	inBattle.set_prepare_deadline_ms(prepareDeadlineMs);
	inBattle.set_state(IN_BATTLE_STATE_PREPARING);

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
					LOG_ERROR << "[PlayerBattle] SET battle:lock 失败, player_id=" << playerId;
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
					LOG_ERROR << "[PlayerBattle] SET battle:ctx 失败, player_id=" << playerId
							  << "(登录重建将退化为无 battle_node_id 的保守冻结)";
				}
			},
			(std::string("SET ") + kBattleCtxKeyFmt + " %b EX %llu").c_str(),
			playerId, ctxPayload.data(), ctxPayload.size(), ttlSec);
	}
	else
	{
		LOG_WARN << "[PlayerBattle] SET battle:lock 跳过(Redis 未连接), player_id=" << playerId
				 << " battle_id=" << battleId << "(match 咨询性检查短暂失明,权威仍在 InBattleComp)";
	}

	LOG_INFO << "[PlayerBattle] 备战冻结完成: player_id=" << playerId
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
			LOG_WARN << "[PlayerBattle] CancelBattlePrepare: 玩家不在线且 Redis 未连接,跳过, player_id="
					 << playerId << " battle_id=" << battleId << "(锁随 TTL 过期)";
			return;
		}
		tlsRedis.GetZoneRedis()->command(
			[playerId, battleId](hiredis::Hiredis*, redisReply* reply) {
				if (reply == nullptr || reply->type == REDIS_REPLY_ERROR)
				{
					LOG_ERROR << "[PlayerBattle] CancelBattlePrepare 读锁/ctx EVAL 失败, player_id="
							  << playerId << " battle_id=" << battleId;
					return;
				}
				if (reply->type != REDIS_REPLY_ARRAY)
				{
					LOG_INFO << "[PlayerBattle] CancelBattlePrepare: 玩家不在线且锁不在/已易主,幂等忽略, player_id="
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
							 << ",玩家不在线但 ctx 已 FIGHTING(确认早于取消),拒绝清锁";
					return;
				}
				LOG_INFO << "[PlayerBattle] CancelBattlePrepare: 玩家不在线,按锁值条件清锁, player_id="
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
		LOG_INFO << "[PlayerBattle] CancelBattlePrepare: 无战斗在途,幂等忽略, player_id="
				 << playerId << " battle_id=" << battleId;
		return;
	}
	if (inBattle->battle_id() != battleId)
	{
		// 迟到的取消(玩家已进入下一场):必须忽略,否则会解冻新战斗
		LOG_WARN << "[PlayerBattle] CancelBattlePrepare: battle_id 不匹配,忽略, player_id=" << playerId
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
				 << ",已 FIGHTING 拒绝取消解冻(过期回滚)";
		return;
	}

	ClearBattleFreeze(player, playerId, battleId);
	LOG_INFO << "[PlayerBattle] 备战取消解冻: player_id=" << playerId << " battle_id=" << battleId;
}

void PlayerBattleSystem::ConfirmBattle(const ::BattleConfirmedEvent& event)
{
	const uint64_t playerId = event.player_id();
	const uint64_t battleId = event.battle_id();
	if (playerId == 0 || battleId == 0)
	{
		LOG_ERROR << "[PlayerBattle] BattleConfirmedEvent 非法: player_id=" << playerId
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
			LOG_WARN << "[PlayerBattle] ConfirmBattle: 玩家不在线且 deadline_ms=0,跳过续期, player_id="
					 << playerId << " battle_id=" << battleId;
			return;
		}
		if (!RedisReady())
		{
			LOG_WARN << "[PlayerBattle] ConfirmBattle: 玩家不在线且 Redis 未连接,跳过续期, player_id="
					 << playerId << " battle_id=" << battleId;
			return;
		}
		const uint64_t deadlineMs = event.deadline_ms();
		tlsRedis.GetZoneRedis()->command(
			[playerId, battleId, deadlineMs](hiredis::Hiredis*, redisReply* reply) {
				if (reply == nullptr || reply->type == REDIS_REPLY_ERROR)
				{
					LOG_ERROR << "[PlayerBattle] ConfirmBattle 读锁/ctx EVAL 失败, player_id=" << playerId
							  << " battle_id=" << battleId;
					return;
				}
				if (reply->type != REDIS_REPLY_ARRAY)
				{
					LOG_WARN << "[PlayerBattle] ConfirmBattle: 玩家不在线且锁不在/已易主,忽略, player_id="
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
				LOG_INFO << "[PlayerBattle] ConfirmBattle: 玩家不在线,按锁值条件续期并落 ctx, player_id="
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
		RebuildBattleFreezeFromLock(player, playerId, battleId, event.deadline_ms(),
									/*rebindGate=*/false, "late_confirm");
		return;
	}
	if (inBattle->battle_id() != battleId)
	{
		// 玩家已进下一场:迟到的确认必须忽略,否则会把新战斗的期限改坏
		LOG_INFO << "[PlayerBattle] ConfirmBattle: battle_id 不匹配,幂等忽略, player_id=" << playerId
				 << " event_battle_id=" << battleId << " in_battle_id=" << inBattle->battle_id();
		return;
	}
	if (inBattle->state() != IN_BATTLE_STATE_PREPARING)
	{
		// Kafka at-least-once 重投 / battle 侧周期补发:已 FIGHTING,幂等忽略
		LOG_INFO << "[PlayerBattle] ConfirmBattle: 已处于 " << eInBattleState_Name(inBattle->state())
				 << ",幂等忽略, player_id=" << playerId << " battle_id=" << battleId;
		return;
	}

	// PREPARING -> FIGHTING:作废期限切到正式 deadline(event 没填时沿用备战时的 deadline_ms)
	const uint64_t deadlineMs = event.deadline_ms() != 0 ? event.deadline_ms() : inBattle->deadline_ms();
	inBattle->set_state(IN_BATTLE_STATE_FIGHTING);
	inBattle->set_deadline_ms(deadlineMs);

	const uint64_t ttlSec = LockTtlSecFor(deadlineMs, nowMs);
	ConfirmBattleLockIfMatch(playerId, battleId, ttlSec, SerializeBattleCtx(*inBattle));

	LOG_INFO << "[PlayerBattle] 战斗确认 PREPARING->FIGHTING: player_id=" << playerId
			 << " battle_id=" << battleId
			 << " deadline_ms=" << deadlineMs
			 << " prepare_deadline_ms=" << inBattle->prepare_deadline_ms()
			 << " lock_ttl_sec=" << ttlSec;
}

void PlayerBattleSystem::RebuildBattleFreezeFromLock(entt::entity player, const uint64_t playerId,
													 const uint64_t battleId, const uint64_t deadlineMsHint,
													 const bool rebindGate, const char* reason)
{
	if (!RedisReady())
	{
		LOG_WARN << "[PlayerBattle] 冻结重建跳过(Redis 未连接), player_id=" << playerId
				 << " battle_id=" << battleId << " reason=" << reason;
		return;
	}
	const std::string reasonCopy = reason;
	tlsRedis.GetZoneRedis()->command(
		[player, playerId, battleId, deadlineMsHint, rebindGate, reasonCopy](hiredis::Hiredis*, redisReply* reply) {
			if (reply == nullptr || reply->type == REDIS_REPLY_ERROR)
			{
				LOG_ERROR << "[PlayerBattle] 冻结重建读锁/ctx EVAL 失败, player_id=" << playerId
						  << " battle_id=" << battleId << " reason=" << reasonCopy;
				return;
			}
			if (reply->type != REDIS_REPLY_ARRAY)
			{
				// 锁不在或已易主:确认已过期(取消/结算/TTL 过期),没有重建资格
				LOG_INFO << "[PlayerBattle] 冻结重建未命中: 锁不在或值不匹配, player_id=" << playerId
						 << " battle_id=" << battleId << " reason=" << reasonCopy;
				return;
			}
			// 回调期间实体可能已销毁/复用,或已被新的 PrepareBattle 挂上组件
			if (!tlsEcs.actorRegistry.valid(player) || GuidForLog(player) != playerId)
			{
				LOG_INFO << "[PlayerBattle] 冻结重建放弃: 实体已不在, player_id=" << playerId
						 << " battle_id=" << battleId << " reason=" << reasonCopy;
				return;
			}
			if (const auto* existing = tlsEcs.actorRegistry.try_get<InBattleComp>(player))
			{
				LOG_INFO << "[PlayerBattle] 冻结重建放弃: 已有 InBattleComp, player_id=" << playerId
						 << " battle_id=" << battleId << " in_battle_id=" << existing->battle_id()
						 << " reason=" << reasonCopy;
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

			// 续期到正式 deadline + 覆写 ctx,同时复核锁仍是本战斗的(否则撤销重建)
			const uint64_t ttlSec = LockTtlSecFor(rebuilt.deadline_ms(), nowMs);
			ConfirmRebuiltFreeze(player, playerId, battleId, ttlSec, SerializeBattleCtx(rebuilt));

			LOG_WARN << "[PlayerBattle] metric=battle_freeze_rebuilt player_id=" << playerId
					 << " battle_id=" << battleId << " reason=" << reasonCopy
					 << " battle_node_id=" << rebuilt.battle_node_id()
					 << " deadline_ms=" << rebuilt.deadline_ms() << " lock_ttl_sec=" << ttlSec;

			if (rebindGate)
			{
				RebindBattleOnReconnect(player);
			}
		},
		(std::string("EVAL %s 2 ") + kBattleLockKeyFmt + " " + kBattleCtxKeyFmt + " %llu").c_str(),
		kGetCtxIfLockMatchScript, playerId, playerId, battleId);
}

void PlayerBattleSystem::RestoreBattleFreezeOnLogin(entt::entity player, const uint64_t playerId)
{
	if (!RedisReady())
	{
		return;
	}
	tlsRedis.GetZoneRedis()->command(
		[player, playerId](hiredis::Hiredis*, redisReply* reply) {
			if (reply == nullptr || reply->type != REDIS_REPLY_ARRAY)
			{
				return; // 锁不在 = 没有战斗在途(EVAL 错误也按无锁处理,锁本身是咨询性的)
			}
			if (!tlsEcs.actorRegistry.valid(player) || GuidForLog(player) != playerId)
			{
				return;
			}
			if (tlsEcs.actorRegistry.any_of<InBattleComp>(player))
			{
				return; // 重连路径组件仍在 / 登录后已备战新局:不动
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
				LOG_ERROR << "[PlayerBattle] 登录重建: 锁值非法, player_id=" << playerId
						  << " lock_value=" << lockValue;
				return;
			}
			if (battleId == 0)
			{
				return;
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

			// 战斗中(房间已建成)才重绑 gate 并提示客户端补拉;PREPARING 由随后的确认/取消/reaper 处理。
			// battle_node_id 为 0(ctx 缺失的降级重建)时绑定无目标,只保留冻结不重绑。
			if (rebuilt.state() == IN_BATTLE_STATE_FIGHTING)
			{
				if (rebuilt.battle_node_id() != 0)
				{
					RebindBattleOnReconnect(player);
				}
				else
				{
					LOG_WARN << "[PlayerBattle] 登录重建: battle_node_id 未知,跳过 gate 重绑, player_id="
							 << playerId << " battle_id=" << battleId;
				}
			}
		},
		(std::string("EVAL %s 2 ") + kBattleLockKeyFmt + " " + kBattleCtxKeyFmt).c_str(),
		kGetLockAndCtxScript, playerId, playerId);
}

void PlayerBattleSystem::ApplySettlementToEntity(entt::entity player, const ::BattleSettlementData& settlement)
{
	const uint64_t playerId = GuidForLog(player);

	auto* baseAttributes = tlsEcs.actorRegistry.try_get<BaseAttributesComp>(player);
	if (baseAttributes == nullptr)
	{
		LOG_ERROR << "[PlayerBattle] 应用结算失败: 缺 BaseAttributesComp, player_id=" << playerId
				  << " battle_id=" << settlement.battle_id();
		return;
	}

	// HP/MP 终值回写(引擎输出已按快照上限饱和,这里再按派生上限夹一次,防御引擎侧越界)
	uint64_t health = settlement.health();
	if (const auto* derived = tlsEcs.actorRegistry.try_get<DerivedAttributesComp>(player);
		derived != nullptr && derived->max_health() > 0)
	{
		health = std::min<uint64_t>(health, derived->max_health());
	}
	baseAttributes->set_health(health);
	uint64_t mana = settlement.mana();
	if (const auto* derived = tlsEcs.actorRegistry.try_get<DerivedAttributesComp>(player);
		derived != nullptr && derived->max_mana() > 0)
	{
		mana = std::min<uint64_t>(mana, derived->max_mana());
	}
	baseAttributes->set_mana(mana);
	// 阵亡基础复活(与登录加载同一规则,产品完整死亡/复活流程未定前的基线):
	// 不复活的话 0 血玩家会再次排队、被快照进新局、引擎开局即判负(2026-09-02 冒烟实测,
	// 离线挂起结算在登录后补应用时也走这里,登录时的复活判定早已跑完、拦不住)。
	if (settlement.is_dead() || health == 0)
	{
		// 回满到玩家真实上限(属性加点算出的 DerivedAttributesComp);取不到才退回职业初值 ——
		// 只回 ClassTable 的 1 级初值会把等级/加点成长吞掉(20 级 max_health 约 1100,初值 500)。
		const auto* derivedForRevive = tlsEcs.actorRegistry.try_get<DerivedAttributesComp>(player);
		ReviveBaseAttributesIfDead(*baseAttributes,
			derivedForRevive != nullptr ? derivedForRevive->max_health() : 0,
			derivedForRevive != nullptr ? derivedForRevive->max_mana() : 0);
		LOG_INFO << "[PlayerBattle] 结算阵亡基础复活: player_id=" << playerId
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
			PetSystem::ApplyBattleSettlement(player, petSettlement);
		}
		PetSystem::PushList(player);
	}

	// 金钱:走统一入账入口(补缴/封禁钩子都在里面,禁止直写 CurrencyComp)
	if (settlement.gold_gain() > 0)
	{
		const auto err = CurrencySystem::AddCurrency(
			player, kCurrencyGold, static_cast<int64_t>(settlement.gold_gain()));
		if (err != kSuccess)
		{
			LOG_WARN << "[PlayerBattle] 金钱入账失败: player_id=" << playerId
					 << " battle_id=" << settlement.battle_id()
					 << " gold=" << settlement.gold_gain() << " err=" << err;
		}
	}

	// 经验:全仓当前没有经验值组件/升级结算系统(只有 PlayerUpgradeEvent 事件壳),
	// 一期仅记日志留痕,接入后在此入账(见 open_issues)
	if (settlement.exp_gain() > 0)
	{
		LOG_INFO << "[PlayerBattle] 经验结算暂缓(经验系统未接入): player_id=" << playerId
				 << " battle_id=" << settlement.battle_id() << " exp=" << settlement.exp_gain();
	}

	// 道具:背包系统尚未挂载玩家实体,消耗扣除(防刷:按实际持有校验,不足按 0)与
	// 掉落发放都无从落地,一期仅记日志(见 open_issues)
	if (settlement.items_consumed_size() > 0 || settlement.items_gained_size() > 0)
	{
		LOG_INFO << "[PlayerBattle] 道具结算暂缓(背包系统未挂载): player_id=" << playerId
				 << " battle_id=" << settlement.battle_id()
				 << " consumed=" << settlement.items_consumed_size()
				 << " gained=" << settlement.items_gained_size();
	}

	LOG_INFO << "[PlayerBattle] 结算已应用: player_id=" << playerId
			 << " battle_id=" << settlement.battle_id()
			 << " outcome=" << settlement.outcome()
			 << " health=" << health << " mana=" << settlement.mana()
			 << " gold=" << settlement.gold_gain()
			 << " is_dead=" << settlement.is_dead()
			 << " rounds=" << settlement.total_rounds();
}

void PlayerBattleSystem::ApplySettlement(const ::BattleSettlementEvent& event)
{
	const auto& settlement = event.settlement();
	const uint64_t playerId = settlement.player_id();
	const uint64_t battleId = settlement.battle_id();
	if (playerId == 0 || battleId == 0)
	{
		LOG_ERROR << "[PlayerBattle] 结算事件非法: player_id=" << playerId << " battle_id=" << battleId;
		return;
	}

	const auto player = tlsEcs.GetPlayer(playerId);
	if (!tlsEcs.actorRegistry.valid(player))
	{
		// 玩家已下线(lease 过期被清理):写离线挂起结算。
		// 写之前按锁值校验 battle_id —— 锁已易主/已被 reaper 判废的迟到结算直接丢弃,
		// 与在线路径"按 InBattleComp.battle_id 匹配"的语义对齐。
		if (!RedisReady())
		{
			LOG_ERROR << "[PlayerBattle] 离线结算暂存失败(Redis 未连接), player_id=" << playerId
					  << " battle_id=" << battleId << ",本条结算丢失(Kafka 重投可补)";
			return;
		}
		std::string payload;
		if (!event.SerializeToString(&payload))
		{
			LOG_ERROR << "[PlayerBattle] 结算事件序列化失败, player_id=" << playerId
					  << " battle_id=" << battleId;
			return;
		}
		tlsRedis.GetZoneRedis()->command(
			[playerId, battleId, payload = std::move(payload)](hiredis::Hiredis*, redisReply* reply) {
				if (reply == nullptr || reply->type != REDIS_REPLY_STRING)
				{
					LOG_WARN << "[PlayerBattle] 离线结算丢弃: 战斗锁不存在(已作废?), player_id="
							 << playerId << " battle_id=" << battleId;
					// 判定为"这一局不再需要结算"就必须销账(条件删,只删仍属于本局的记录):
					// 不销账的话 battle 会一直重投到次数用尽,而且记录会留到玩家下次登录
					// 被登录钩子应用 —— 那等于把刚判废的结算又发了出去。
					ClearPendingSettlementIfMatch(playerId, battleId);
					return;
				}
				if (std::string(reply->str, reply->len) != std::to_string(battleId))
				{
					LOG_WARN << "[PlayerBattle] 离线结算丢弃: 战斗锁值不匹配, player_id=" << playerId
							 << " battle_id=" << battleId;
					ClearPendingSettlementIfMatch(playerId, battleId);
					return;
				}
				StorePendingSettlement(playerId, battleId, payload);
				LOG_INFO << "[PlayerBattle] 结算已暂存(玩家离线), player_id=" << playerId
						 << " battle_id=" << battleId;
			},
			(std::string("GET ") + kBattleLockKeyFmt).c_str(), playerId);
		return;
	}

	// 在线路径:InBattleComp.battle_id 匹配才应用(不变量:摘除后再来的同 id 结算丢弃)
	const auto* inBattle = tlsEcs.actorRegistry.try_get<InBattleComp>(player);
	if (inBattle != nullptr && inBattle->battle_id() != battleId)
	{
		LOG_WARN << "[PlayerBattle] 结算丢弃: battle_id 不匹配(重复投递或已作废), player_id=" << playerId
				 << " event_battle_id=" << battleId << " in_battle_id=" << inBattle->battle_id();
		// 条件销账:只删仍指向本局的记录。玩家已经在打下一场时,记录早被那一局覆盖,
		// 这里是无害的空操作;而真的是"重复投递"时,它让 battle 停止重投。
		ClearPendingSettlementIfMatch(playerId, battleId);
		return;
	}
	if (inBattle == nullptr)
	{
		// 在线但没有 InBattleComp:玩家战斗中完整下线再登录(组件不落库)且登录重建未命中/未完成,
		// 或备战到期被 reaper 摘组件后确认与结算都迟到。锁值 == battle_id 证明这场战斗仍是玩家的
		// 当前战斗(期间没有进第二场),结算必须应用而不是丢弃;锁不在才是真正的重复投递/已作废。
		if (!RedisReady())
		{
			LOG_ERROR << "[PlayerBattle] 结算丢弃: 无 InBattleComp 且 Redis 未连接无法核对锁, player_id="
					  << playerId << " battle_id=" << battleId << "(Kafka 重投可补)";
			return;
		}
		tlsRedis.GetZoneRedis()->command(
			[player, playerId, battleId, event](hiredis::Hiredis*, redisReply* reply) {
				if (reply == nullptr || reply->type != REDIS_REPLY_STRING ||
					std::string(reply->str, reply->len) != std::to_string(battleId))
				{
					LOG_WARN << "[PlayerBattle] 结算丢弃: 无 InBattleComp 且锁不在/值不匹配(重复投递或已作废), player_id="
							 << playerId << " battle_id=" << battleId;
					ClearPendingSettlementIfMatch(playerId, battleId);
					return;
				}
				if (!tlsEcs.actorRegistry.valid(player) || GuidForLog(player) != playerId)
				{
					// 回调期间玩家又下线了:锁还在,交给 Kafka 重投走离线暂存路径
					LOG_WARN << "[PlayerBattle] 结算按锁应用放弃: 实体已不在, player_id=" << playerId
							 << " battle_id=" << battleId << "(Kafka 重投可补)";
					return;
				}
				if (const auto* current = tlsEcs.actorRegistry.try_get<InBattleComp>(player);
					current != nullptr && current->battle_id() != battleId)
				{
					LOG_WARN << "[PlayerBattle] 结算丢弃: 回调期间玩家已进下一场, player_id=" << playerId
							 << " battle_id=" << battleId << " in_battle_id=" << current->battle_id();
					return;
				}
				LOG_WARN << "[PlayerBattle] metric=battle_settlement_applied_by_lock player_id=" << playerId
						 << " battle_id=" << battleId << ",无 InBattleComp,按锁值匹配应用结算";
				ApplySettlementToEntity(player, event.settlement());
				// 组件可能在回调期间被登录重建/迟到确认挂回来(同 battle_id):一并摘除
				ClearBattleFreeze(player, playerId, battleId);
				// 销账 = 给 battle 侧发件箱的 ACK(R07),必须在应用之后
				ClearPendingSettlementIfMatch(playerId, battleId);
				PushBattleEndToPlayer(player, event.settlement());
			},
			(std::string("GET ") + kBattleLockKeyFmt).c_str(), playerId);
		return;
	}

	ApplySettlementToEntity(player, settlement);
	ClearBattleFreeze(player, playerId, battleId);
	// 销账 = 给 battle 侧发件箱的 ACK(R07):battle 探测到伴生 id 键已不属于本局就停止重投。
	// 顺序不能反 —— 先销账后应用的话,应用中途进程崩溃就两头落空。
	//
	// 残留窗口(已知、刻意接受):"应用完成 → 本进程在销账落地前崩溃"会留下一条待结算
	// 记录,玩家下次登录被补应用一次(重复入账)。这个窗口是微秒级、且需要进程恰好死在
	// 这一行,与既有的 ClearBattleFreeze / ApplyPendingSettlement 的 DEL 同为
	// fire-and-forget 语义,不是本次引入的新形状。用"锁还在才补应用"来收紧它是错的:
	// 玩家离线超过锁 TTL(deadline+60s)后锁自然过期,而待结算记录还有 7 天,
	// 按锁 gating 会把这类**合法**的离线结算判掉,那是真丢奖励。
	ClearPendingSettlementIfMatch(playerId, battleId);
	PushBattleEndToPlayer(player, settlement);
}

void PlayerBattleSystem::ApplyPendingSettlement(entt::entity player, const ::BattleSettlementEvent& event)
{
	const auto& settlement = event.settlement();
	const uint64_t playerId = settlement.player_id();

	ApplySettlementToEntity(player, settlement);

	// 理论上离线结算与"实体上还挂着 InBattleComp"不共存(实体重建后组件不落库),
	// 防御性摘除:同 battle_id 才摘,避免误伤登录后刚备战的新战斗
	if (const auto* inBattle = tlsEcs.actorRegistry.try_get<InBattleComp>(player);
		inBattle != nullptr && inBattle->battle_id() == settlement.battle_id())
	{
		tlsEcs.actorRegistry.remove<InBattleComp>(player);
	}

	// 清理待结算记录 + 锁(锁删除后 match 才放行下一次排队 —— 先应用再放开)。
	// 两者都按 battle_id **条件删**:登录钩子理论上早于任何备战,但一旦顺序反过来,
	// 无条件 DEL 会把玩家刚开始的下一场的记录/锁抹掉。条件删同时也是给 battle 侧
	// 发件箱的 ACK(R07)。
	ClearPendingSettlementIfMatch(playerId, settlement.battle_id());
	DeleteBattleLockIfMatch(playerId, settlement.battle_id());

	PushBattleEndToPlayer(player, settlement);
	LOG_INFO << "[PlayerBattle] 离线挂起结算已补应用: player_id=" << playerId
			 << " battle_id=" << settlement.battle_id();
}

void PlayerBattleSystem::RebindBattleOnReconnect(entt::entity player)
{
	const auto* inBattle = tlsEcs.actorRegistry.try_get<InBattleComp>(player);
	if (inBattle == nullptr)
	{
		return;
	}
	const uint64_t playerId = GuidForLog(player);

	GateRoute gateRoute;
	if (!ResolveGateRoute(player, gateRoute))
	{
		LOG_WARN << "[PlayerBattle] 重连重绑失败: 缺会话快照, player_id=" << playerId
				 << " battle_id=" << inBattle->battle_id();
		return;
	}
	// 不变量 2 fail-closed:空 instance id 会关闭 gate 侧防僵尸过滤,宁可不发
	if (gateRoute.gateInstanceId.empty())
	{
		LOG_ERROR << "[PlayerBattle] 重连重绑被拒: gate_instance_id 为空, player_id=" << playerId
				  << " battle_id=" << inBattle->battle_id()
				  << " gate_node_id=" << gateRoute.gateNodeId;
		return;
	}

	// BindBattleEvent 经 Kafka gate-cmd_g<N> 的 gate_node_id % P 号分区
	// (GateCommand;key=player_id;分区由目标 gate 定死,同一 gate 的命令天然有序)
	contracts::kafka::BindBattleEvent bindEvent;
	bindEvent.set_session_id(gateRoute.sessionId);
	bindEvent.set_battle_node_id(inBattle->battle_node_id());
	bindEvent.set_battle_id(inBattle->battle_id());
	bindEvent.set_player_id(playerId);

	contracts::kafka::GateCommand command;
	command.set_event_id(ContractsKafkaBindBattleEventEventId);
	// 共享分区后的第一级(数字)过滤,理由见 node_kafka_command_filter.h。
	command.set_target_gate_id(gateRoute.gateNodeId);
	command.set_target_instance_id(gateRoute.gateInstanceId);
	command.set_payload(bindEvent.SerializeAsString());

	const auto route = node::kafka::ResolveCommandRoute(GateNodeService, gateRoute.gateNodeId);
	const auto err = KafkaProducer::Instance().send(route.topic, command.SerializeAsString(),
													std::to_string(playerId), route.partition);
	if (err != RdKafka::ERR_NO_ERROR)
	{
		LOG_ERROR << "[PlayerBattle] BindBattleEvent 发送失败: topic=" << route.topic
				  << " partition=" << route.partition
				  << " player_id=" << playerId << " battle_id=" << inBattle->battle_id()
				  << " err=" << RdKafka::err2str(err);
		return;
	}

	// 推重连提示,客户端随后用 GetBattleState 向 battle 节点补拉全量状态
	::BattleReconnectS2C message;
	message.set_battle_id(inBattle->battle_id());
	SendMessageToClientViaGate(BattleClientPlayerNotifyBattleReconnectMessageId, message, player);

	LOG_INFO << "[PlayerBattle] 战斗中重连已重绑: player_id=" << playerId
			 << " battle_id=" << inBattle->battle_id()
			 << " battle_node_id=" << inBattle->battle_node_id()
			 << " gate_node_id=" << gateRoute.gateNodeId;
}

void PlayerBattleSystem::OnPlayerEnterScene(entt::entity player, uint32_t enterGsType)
{
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return;
	}
	const uint64_t playerId = GuidForLog(player);
	if (playerId == 0)
	{
		return;
	}

	// 1) 离线挂起结算:所有登录类型都查(先应用挂起结算再放开排队,§3.2)。
	//    没有挂起结算时,再看 battle:lock 是否还在 —— 在则说明战斗仍在途而实体是新建的
	//    (InBattleComp 不落库),按锁 + ctx 重建冻结并重绑 gate,后续结算才能在线命中。
	//    两步必须串在同一条回调链上:挂起结算应用后会条件删锁,若并行发出读锁,
	//    先发出的 GET 会看到旧锁而把已结算的战斗重建回来。
	if (RedisReady())
	{
		tlsRedis.GetZoneRedis()->command(
			[player, playerId](hiredis::Hiredis*, redisReply* reply) {
				if (!tlsEcs.actorRegistry.valid(player) || GuidForLog(player) != playerId)
				{
					// 回调期间实体已销毁/复用:留着 pending,下次登录再应用
					return;
				}
				if (reply == nullptr || reply->type != REDIS_REPLY_STRING)
				{
					// 无挂起结算 -> 查锁重建(重连路径组件仍在时函数内部直接跳过)
					if (!tlsEcs.actorRegistry.any_of<InBattleComp>(player))
					{
						RestoreBattleFreezeOnLogin(player, playerId);
					}
					return;
				}
				if (reply->len > static_cast<size_t>(std::numeric_limits<int>::max()))
				{
					LOG_ERROR << "[PlayerBattle] 挂起结算 payload 超长, player_id=" << playerId;
					return;
				}
				::BattleSettlementEvent event;
				if (!event.ParseFromArray(reply->str, static_cast<int>(reply->len)))
				{
					LOG_ERROR << "[PlayerBattle] 挂起结算解析失败,清理脏数据, player_id=" << playerId;
					if (RedisReady())
					{
						// 脏数据解不出 battle_id,无从条件删;两键一起无条件删。
						// 伴生 id 键必须一起删,否则它会永远指向一个再也读不出内容的记录,
						// battle 侧发件箱据此判定"未销账"而重投到次数用尽。
						tlsRedis.GetZoneRedis()->command(
							[](hiredis::Hiredis*, redisReply*) {},
							(std::string("DEL ") + kPendingSettlementKeyFmt + " " +
							 kPendingSettlementIdKeyFmt).c_str(), playerId, playerId);
					}
					return;
				}
				ApplyPendingSettlement(player, event);
			},
			(std::string("GET ") + kPendingSettlementKeyFmt).c_str(), playerId);
	}
	else
	{
		LOG_WARN << "[PlayerBattle] 挂起结算检查跳过(Redis 未连接), player_id=" << playerId
				 << "(battle:lock 未解,排队仍被挡,下次登录再补)";
	}

	// 2) 战斗中重连:实体仍存活且挂着 InBattleComp -> 重发 gate 绑定 + 提示客户端补拉
	//    (完整下线再登录的重绑走上面的 RestoreBattleFreezeOnLogin 回调)
	if (enterGsType == LOGIN_RECONNECT && tlsEcs.actorRegistry.any_of<InBattleComp>(player))
	{
		RebindBattleOnReconnect(player);
	}
}

void PlayerBattleSystem::StartReaper(muduo::net::EventLoop* loop)
{
	if (loop == nullptr)
	{
		LOG_ERROR << "[PlayerBattle] StartReaper: loop 为空";
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
			const uint64_t playerId = GuidForLog(entry.entity);
			// 结构化 metric 日志:cpp scene 进程当前没有 Prometheus 端点(与
			// CrossZoneReaper 同款口径),由日志侧提取计数,见 open_issues。
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
						 << ",备战作废摘组件(CreateBattle 未确认,match 断链或 gather 失败未取消);锁保留至 TTL 过期供迟到确认重建";
				tlsEcs.actorRegistry.remove<InBattleComp>(entry.entity);
				continue;
			}
			LOG_WARN << "[PlayerBattle] metric=battle_freeze_expired player_id=" << playerId
					 << " battle_id=" << entry.battleId
					 << " deadline_ms=" << entry.deadlineMs
					 << " now_ms=" << nowMs << ",战斗作废解冻(battle 节点崩溃或结算事件丢失)";
			// 按 battle_id 条件删锁:作废判定与锁值同源,不会误删别的战斗
			ClearBattleFreeze(entry.entity, playerId, entry.battleId);
		}
	});
	gReaperActive = true;

	LOG_INFO << "[PlayerBattle] 战斗冻结 reaper 已启动, interval=" << kReaperIntervalSec << "s";
}

void PlayerBattleSystem::StopReaper()
{
	if (!gReaperActive || gReaperLoop == nullptr)
	{
		return;
	}
	gReaperLoop->cancel(gReaperTimerId);
	gReaperActive = false;
	LOG_INFO << "[PlayerBattle] 战斗冻结 reaper 已停止";
}
