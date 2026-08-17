#include "player_battle.h"

#include <muduo/base/Logging.h>
#include <muduo/net/EventLoop.h>
#include <muduo/net/TimerId.h>
#include <muduo/contrib/hiredis/Hiredis.h>
#include <hiredis/hiredis.h>

#include <algorithm>
#include <limits>
#include <string>
#include <utility>
#include <vector>

#include "engine/core/time/system/time.h"
#include "engine/infra/messaging/kafka/kafka_producer.h"
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

// 时间->回合换算与回合常量的权威定义在回合引擎库(scene 可以依赖 battle 常量,反向禁止)。
#include "services/battle/constants/turn_battle_constants.h"

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
	//   battle:lock:{player_id} = battle_id           —— 战斗串行化咨询锁(match 读)
	//   battle:settlement:pending:{player_id} = event —— 离线结算暂存(TTL 7 天)
	constexpr char kBattleLockKeyFmt[] = "battle:lock:%llu";
	constexpr char kPendingSettlementKeyFmt[] = "battle:settlement:pending:%llu";

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

	// DEL battle:lock:{player_id}(无条件;调用方已确认 InBattleComp 匹配)
	void DeleteBattleLock(const uint64_t playerId)
	{
		if (!RedisReady())
		{
			// 非致命:锁带 EX,最迟随 TTL 过期;记日志说明串行化窗口靠 TTL 兜底
			LOG_WARN << "[PlayerBattle] 删除战斗锁跳过(Redis 未连接), player_id=" << playerId
					 << ", 锁将随 TTL 自然过期";
			return;
		}
		tlsRedis.GetZoneRedis()->command(
			[playerId](hiredis::Hiredis*, redisReply* reply) {
				if (reply == nullptr || reply->type == REDIS_REPLY_ERROR)
				{
					LOG_ERROR << "[PlayerBattle] DEL battle:lock 失败, player_id=" << playerId;
				}
			},
			(std::string("DEL ") + kBattleLockKeyFmt).c_str(), playerId);
	}

	// 条件删锁:GET 值 == battleId 才 DEL(玩家实体不在本节点时用,防误删新战斗的锁)。
	// GET 与 DEL 之间存在竞态窗口,但锁的写者只有本 zone 的 scene 节点、且 match 对同一
	// 玩家的编排是串行的(D4),窗口内不会出现"另一场战斗抢先 SET"的并发写。
	void DeleteBattleLockIfMatch(const uint64_t playerId, const uint64_t battleId)
	{
		if (!RedisReady())
		{
			LOG_WARN << "[PlayerBattle] 条件删锁跳过(Redis 未连接), player_id=" << playerId;
			return;
		}
		tlsRedis.GetZoneRedis()->command(
			[playerId, battleId](hiredis::Hiredis*, redisReply* reply) {
				if (reply == nullptr || reply->type != REDIS_REPLY_STRING)
				{
					return; // 锁不存在/已过期,无事可做
				}
				const std::string lockValue(reply->str, reply->len);
				if (lockValue != std::to_string(battleId))
				{
					LOG_WARN << "[PlayerBattle] 战斗锁值不匹配,跳过删除, player_id=" << playerId
							 << " battle_id=" << battleId << " lock=" << lockValue;
					return;
				}
				DeleteBattleLock(playerId);
			},
			(std::string("GET ") + kBattleLockKeyFmt).c_str(), playerId);
	}

	// 摘 InBattleComp + DEL 锁(结算已应用/取消/作废的统一收尾)
	void ClearBattleFreeze(entt::entity player, const uint64_t playerId)
	{
		tlsEcs.actorRegistry.remove<InBattleComp>(player);
		DeleteBattleLock(playerId);
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
	// max_mana:全仓当前没有蓝上限属性,一期取当前 MP 当上限(引擎内不回蓝越界),见 open_issues
	snapshot.set_max_mana(std::max<uint64_t>(baseAttributes->mana(), 1));

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

	// 先组快照,失败不留任何冻结痕迹
	uint32_t errorTipId = kSuccess;
	if (!BuildBattleSnapshot(player, *response.mutable_snapshot(), errorTipId))
	{
		response.clear_snapshot();
		response.mutable_error_message()->set_id(errorTipId);
		return;
	}

	// 挂 InBattleComp(上面已确认不存在,直接 emplace;此处非 per-tick 路径)
	auto& inBattle = tlsEcs.actorRegistry.emplace<InBattleComp>(player);
	inBattle.set_battle_id(battleId);
	inBattle.set_battle_node_id(request.battle_node_id());
	inBattle.set_deadline_ms(request.deadline_ms());
	inBattle.set_state(IN_BATTLE_STATE_PREPARING);

	// SET battle:lock:{player_id}=battle_id EX(deadline+60s)。
	// 咨询性锁(match JoinQueue 读),fire-and-forget:失败只影响 match 的提前拒绝,
	// 权威判定仍是本节点的 InBattleComp,不影响正确性。
	const uint64_t nowMs = TimeSystem::NowMillisecondsUTC();
	const uint64_t remainSec = request.deadline_ms() > nowMs ? (request.deadline_ms() - nowMs) / 1000 : 0;
	const uint64_t ttlSec = remainSec + kLockExtraTtlSec;
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
			 << " lock_ttl_sec=" << ttlSec;
}

void PlayerBattleSystem::CancelBattlePrepare(const ::CancelBattlePrepareRequest& request)
{
	const uint64_t playerId = request.player_id();
	const uint64_t battleId = request.battle_id();

	const auto player = tlsEcs.GetPlayer(playerId);
	if (!tlsEcs.actorRegistry.valid(player))
	{
		// 实体不在(取消到达前玩家下线):按锁值匹配条件删锁,幂等
		LOG_INFO << "[PlayerBattle] CancelBattlePrepare: 玩家不在线,按锁值条件清锁, player_id="
				 << playerId << " battle_id=" << battleId;
		DeleteBattleLockIfMatch(playerId, battleId);
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

	ClearBattleFreeze(player, playerId);
	LOG_INFO << "[PlayerBattle] 备战取消解冻: player_id=" << playerId << " battle_id=" << battleId;
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
	baseAttributes->set_mana(settlement.mana());
	ActorAttributeCalculatorSystem::MarkAttributeForUpdate(player, kHealth);
	ActorAttributeCalculatorSystem::MarkAttributeForUpdate(player, kEnergy);

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
					return;
				}
				if (std::string(reply->str, reply->len) != std::to_string(battleId))
				{
					LOG_WARN << "[PlayerBattle] 离线结算丢弃: 战斗锁值不匹配, player_id=" << playerId
							 << " battle_id=" << battleId;
					return;
				}
				if (!RedisReady())
				{
					LOG_ERROR << "[PlayerBattle] 离线结算暂存失败(Redis 断连), player_id=" << playerId;
					return;
				}
				tlsRedis.GetZoneRedis()->command(
					[playerId](hiredis::Hiredis*, redisReply* setReply) {
						if (setReply == nullptr || setReply->type == REDIS_REPLY_ERROR)
						{
							LOG_ERROR << "[PlayerBattle] SET 离线结算失败, player_id=" << playerId;
						}
					},
					(std::string("SET ") + kPendingSettlementKeyFmt + " %b EX %u").c_str(),
					playerId, payload.data(), payload.size(),
					PlayerBattleSystem::kPendingSettlementTtlSec);
				LOG_INFO << "[PlayerBattle] 结算已暂存(玩家离线), player_id=" << playerId
						 << " battle_id=" << battleId;
			},
			(std::string("GET ") + kBattleLockKeyFmt).c_str(), playerId);
		return;
	}

	// 在线路径:InBattleComp.battle_id 匹配才应用(不变量:摘除后再来的同 id 结算丢弃)
	const auto* inBattle = tlsEcs.actorRegistry.try_get<InBattleComp>(player);
	if (inBattle == nullptr || inBattle->battle_id() != battleId)
	{
		LOG_WARN << "[PlayerBattle] 结算丢弃: battle_id 不匹配(重复投递或已作废), player_id=" << playerId
				 << " event_battle_id=" << battleId
				 << " in_battle_id=" << (inBattle != nullptr ? inBattle->battle_id() : 0);
		return;
	}

	ApplySettlementToEntity(player, settlement);
	ClearBattleFreeze(player, playerId);
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

	// 清理 pending key + 锁(锁删除后 match 才放行下一次排队 —— 先应用再放开)
	if (RedisReady())
	{
		tlsRedis.GetZoneRedis()->command(
			[](hiredis::Hiredis*, redisReply*) {},
			(std::string("DEL ") + kPendingSettlementKeyFmt).c_str(), playerId);
	}
	DeleteBattleLock(playerId);

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

	// BindBattleEvent 经 Kafka gate-{gate_id}(GateCommand;key=player_id 保序)
	contracts::kafka::BindBattleEvent bindEvent;
	bindEvent.set_session_id(gateRoute.sessionId);
	bindEvent.set_battle_node_id(inBattle->battle_node_id());
	bindEvent.set_battle_id(inBattle->battle_id());
	bindEvent.set_player_id(playerId);

	contracts::kafka::GateCommand command;
	command.set_event_id(ContractsKafkaBindBattleEventEventId);
	command.set_target_instance_id(gateRoute.gateInstanceId);
	command.set_payload(bindEvent.SerializeAsString());

	const std::string topic = "gate-" + std::to_string(gateRoute.gateNodeId);
	const auto err = KafkaProducer::Instance().send(topic, command.SerializeAsString(),
													std::to_string(playerId));
	if (err != RdKafka::ERR_NO_ERROR)
	{
		LOG_ERROR << "[PlayerBattle] BindBattleEvent 发送失败: topic=" << topic
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

	// 1) 离线挂起结算:所有登录类型都查(先应用挂起结算再放开排队,§3.2)
	if (RedisReady())
	{
		tlsRedis.GetZoneRedis()->command(
			[player, playerId](hiredis::Hiredis*, redisReply* reply) {
				if (reply == nullptr || reply->type != REDIS_REPLY_STRING)
				{
					return; // 无挂起结算
				}
				if (!tlsEcs.actorRegistry.valid(player) || GuidForLog(player) != playerId)
				{
					// 回调期间实体已销毁/复用:留着 pending,下次登录再应用
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
						tlsRedis.GetZoneRedis()->command(
							[](hiredis::Hiredis*, redisReply*) {},
							(std::string("DEL ") + kPendingSettlementKeyFmt).c_str(), playerId);
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
		std::vector<std::pair<entt::entity, uint64_t>> expired;
		for (auto&& [entity, inBattle] : tlsEcs.actorRegistry.view<InBattleComp>().each())
		{
			if (inBattle.deadline_ms() != 0 && inBattle.deadline_ms() < nowMs)
			{
				expired.emplace_back(entity, inBattle.battle_id());
			}
		}

		for (const auto& [entity, battleId] : expired)
		{
			const uint64_t playerId = GuidForLog(entity);
			// 结构化 metric 日志:cpp scene 进程当前没有 Prometheus 端点(与
			// CrossZoneReaper 同款口径),由日志侧提取计数,见 open_issues
			LOG_WARN << "[PlayerBattle] metric=battle_freeze_expired player_id=" << playerId
					 << " battle_id=" << battleId
					 << " now_ms=" << nowMs << ",战斗作废解冻(battle 节点崩溃或 match 断链)";
			ClearBattleFreeze(entity, playerId);
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
