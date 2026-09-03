#pragma once

// 回合制战斗 scene 侧集成(设计文档 docs/design/turn-based-battle-server.md §5.3)。
//
// PlayerBattleSystem 是 scene 节点上回合制战斗的唯一业务入口:
//   - PrepareBattle / CancelBattlePrepare:match 服务 gather 管线的冻结/解冻(D4 结算串行化);
//   - ConfirmBattle:battle 节点 CreateBattle 成功后的确认事件,PREPARING -> FIGHTING
//     并把作废期限从 prepare_deadline_ms 切到正式 deadline_ms(cross-zone-matchmaking.md §10);
//   - ApplySettlement:battle 节点结算事件的唯一应用者(宪法 §7 新增不变量 4);
//   - OnPlayerEnterScene:登录/重连后置钩子(先应用离线挂起结算,再处理战斗中重连的重绑);
//   - reaper:低频扫描 InBattleComp,PREPARING 看 prepare_deadline_ms、FIGHTING 看 deadline_ms,
//     过期即作废解冻(§3.2 补偿矩阵)。
//
// 冻结语义:InBattleComp 存在 = 玩家有一场战斗在途,拒绝再次备战/切场景/跨 zone 迁移;
// 摘除条件 = 结算已应用 / match 取消(仅 PREPARING)/ reaper 超时作废,是玩家再次进战斗的唯一放行条件。
//
// battle:lock 语义:值 = battle_id,所有删除/续期一律"值匹配才操作"(Lua 原子 GET+DEL /
// GET+EXPIRE),迟到的取消/结算/作废绝不会误伤玩家后续的新战斗锁。
// 伴生 battle:ctx:{player_id} = InBattleComp 序列化(scene 私有,与锁同 SET / 同 TTL / 同 DEL):
// 实体没了但锁还在的两条路径靠它重建冻结 —— ①玩家战斗中完整下线再登录(组件不落库);
// ②备战到期 reaper 只摘组件不删锁,迟到的 BattleConfirmedEvent 按锁值重建 FIGHTING。
// 结算在线路径无 InBattleComp 时同样按锁值匹配应用而不是丢弃。
//
// 线程模型:所有方法都必须在 scene 节点 loop 线程调用(gRPC 入口由生成骨架 runInLoop 投递)。

#include <cstdint>

#include "entt/src/entt/entity/entity.hpp"
#include "engine/core/type_define/type_define.h"

namespace muduo
{
	namespace net
	{
		class EventLoop;
	}
}

// 消息定义在 proto/scene/scene.proto(无 package → 全局命名空间),
// muduo Scene 服务与 SceneNodeGrpc 控制面共享同一份类型。
class PrepareBattleRequest;
class PrepareBattleResponse;
class CancelBattlePrepareRequest;
class BattleSettlementEvent;
class BattleConfirmedEvent;
class BattleSettlementData;
class BattlePlayerSnapshot;

class PlayerBattleSystem
{
public:
	// match -> scene:冻结玩家并返回战斗快照。
	// 校验:在线(实体在本节点且有 gate 会话)/ 无 InBattleComp / 未处于跨 zone 冻结。
	// 成功:挂 InBattleComp{state=PREPARING, deadline_ms, prepare_deadline_ms(0 时取 deadline_ms)}
	// + SET battle:lock:{player_id}=battle_id(EX 按 prepare_deadline_ms+60s,咨询性锁,
	// 权威判定仍是 InBattleComp)+ 返回 BattlePlayerSnapshot(含 table_fingerprint)
	// + response.table_fingerprint。
	// 失败:response.error_message 填拒绝原因,不留任何冻结痕迹。
	static void PrepareBattle(const ::PrepareBattleRequest& request, ::PrepareBattleResponse& response);

	// match -> scene:gather 失败补偿。battle_id 与 InBattleComp 匹配且 state==PREPARING 才
	// 摘组件 + 条件删锁;不匹配(迟到取消)忽略;已 FIGHTING(确认已到 = 房间建成过)拒绝并记
	// metric=battle_cancel_rejected_fighting —— 这是 match CreateBattle 超时后的过期回滚,解冻会让
	// 玩家同时进两局。玩家实体不在时按锁值读 ctx,ctx 未 FIGHTING 才条件删锁(幂等)。
	static void CancelBattlePrepare(const ::CancelBattlePrepareRequest& request);

	// battle --Kafka--> scene:CreateBattle 成功确认(battle 侧开局后一段时间内周期补发,幂等)。
	// 在线且 InBattleComp.battle_id 匹配且 state==PREPARING -> state=FIGHTING、
	// deadline_ms=event.deadline_ms、battle:lock 条件续期 + ctx 覆写(值==battle_id 才 EXPIRE 到
	// deadline-now+60s);已 FIGHTING / battle_id 不匹配 -> 幂等忽略;
	// 在线但无 InBattleComp(备战到期被摘 / 登录重建未命中)-> 锁值==battle_id 则重建 FIGHTING 冻结;
	// 玩家不在本节点 -> 锁值匹配才续期,并把 FIGHTING/deadline 落进 ctx 供登录重建。
	static void ConfirmBattle(const ::BattleConfirmedEvent& event);

	// battle --Kafka--> scene:应用战斗结算。
	// InBattleComp.battle_id 匹配才应用(HP/MP 终值 + 金钱;经验/道具见 open_issues),
	// 然后摘组件 + DEL 锁 + 推 BattleEndS2C;InBattleComp 存在但 id 不匹配丢弃并告警;
	// 在线但无 InBattleComp -> 锁值==battle_id 才应用(下线再登录 / 备战到期后的迟到结算),
	// 否则按 at-least-once 重复投递丢弃;
	// 玩家实体不在 -> 校验锁值后写 battle:settlement:pending:{player_id}(TTL 7 天)。
	static void ApplySettlement(const ::BattleSettlementEvent& event);

	// 登录/重连后置钩子(player_lifecycle.cpp EnterScene 末尾调用):
	//   1) 查 Redis 挂起结算,有则应用并清理(先应用挂起结算再放开排队,§3.2);
	//      没有挂起结算且无 InBattleComp -> 读 battle:lock + ctx,锁在则重建 InBattleComp,
	//      FIGHTING 且 battle_node_id 已知时向 gate 重发 BindBattleEvent + 推 BattleReconnectS2C;
	//   2) RECONNECT 且仍有 InBattleComp -> 向 gate 重发 BindBattleEvent + 推 BattleReconnectS2C。
	static void OnPlayerEnterScene(entt::entity player, uint32_t enterGsType);

	// 冻结拦截查询:InBattleComp 存在即在战(切场景/跨 zone 等入口用)。
	static bool IsInBattle(entt::entity player);

	// reaper:30s 低频定时器,扫描 InBattleComp:PREPARING 按 prepare_deadline_ms 过期只摘组件、
	// 锁保留到 TTL(prepare_deadline+60s)自然过期给迟到确认留重建余量;FIGHTING 按 deadline_ms
	// 过期摘组件 + 条件删锁。结构化日志 metric=battle_prepare_expired / battle_freeze_expired。
	// 不进 20FPS 帧循环;注册/停止时机与 CrossZoneReaper 一致(main.cpp)。
	static void StartReaper(muduo::net::EventLoop* loop);
	static void StopReaper();

	// reaper 扫描间隔(秒)。战斗作废的时间精度要求是"分钟级",30s 足够。
	static constexpr double kReaperIntervalSec = 30.0;
	// battle:lock 的 EX 在 deadline 之上的余量(秒):锁必须活得比 InBattleComp 久,
	// 避免"组件还在、锁先没了"让 match 放进第二场。
	static constexpr uint32_t kLockExtraTtlSec = 60;
	// 离线挂起结算 TTL:7 天(设计文档 §6)。
	static constexpr uint32_t kPendingSettlementTtlSec = 7 * 24 * 3600;

private:
	// 组 BattlePlayerSnapshot(属性/等级/技能/参战 buff/道具副本/路由信息/配表指纹)。
	// 失败时把拒绝原因写进 errorTipId 并返回 false(缺基础属性组件/缺会话快照)。
	static bool BuildBattleSnapshot(entt::entity player, ::BattlePlayerSnapshot& snapshot, uint32_t& errorTipId);

	// 把结算终值落到玩家实体(HP/MP 写 BaseAttributesComp + 属性脏位;金钱走 CurrencySystem)。
	static void ApplySettlementToEntity(entt::entity player, const ::BattleSettlementData& settlement);

	// RECONNECT 重绑:经 Kafka gate-{gate_id} 发 BindBattleEvent(GateCommand,
	// target_instance_id 必填,宪法 §7 不变量 2),并推 BattleReconnectS2C。
	static void RebindBattleOnReconnect(entt::entity player);

	// 应用挂起结算并清理 pending key + 锁(登录钩子的 Redis 回调侧)。
	static void ApplyPendingSettlement(entt::entity player, const ::BattleSettlementEvent& event);

	// 按锁值匹配重建 FIGHTING 冻结(迟到确认 / 在线无组件路径):锁值==battleId 才读 ctx 重建
	// InBattleComp、条件续期并覆写 ctx;deadlineMsHint 非 0 时覆盖 ctx 的 deadline;
	// rebindGate 为真时重建后向 gate 重发 BindBattleEvent。reason 只进日志。
	static void RebuildBattleFreezeFromLock(entt::entity player, uint64_t playerId, uint64_t battleId,
											uint64_t deadlineMsHint, bool rebindGate, const char* reason);

	// 登录重建:读 battle:lock + ctx(不预设 battle_id),锁在则按 ctx 重建 InBattleComp;
	// FIGHTING 且 battle_node_id 已知时重绑 gate。只在无挂起结算且无 InBattleComp 时调用。
	static void RestoreBattleFreezeOnLogin(entt::entity player, uint64_t playerId);
};
