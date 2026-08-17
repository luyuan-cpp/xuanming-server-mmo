#pragma once

// 回合制战斗 scene 侧集成(设计文档 docs/design/turn-based-battle-server.md §5.3)。
//
// PlayerBattleSystem 是 scene 节点上回合制战斗的唯一业务入口:
//   - PrepareBattle / CancelBattlePrepare:match 服务 gather 管线的冻结/解冻(D4 结算串行化);
//   - ApplySettlement:battle 节点结算事件的唯一应用者(宪法 §7 新增不变量 4);
//   - OnPlayerEnterScene:登录/重连后置钩子(先应用离线挂起结算,再处理战斗中重连的重绑);
//   - reaper:低频扫描 InBattleComp.deadline_ms,过期即作废解冻(§3.2 补偿矩阵)。
//
// 冻结语义:InBattleComp 存在 = 玩家有一场战斗在途,拒绝再次备战/切场景/跨 zone 迁移;
// 摘除条件 = 结算已应用 / match 取消 / reaper 超时作废,是玩家再次进战斗的唯一放行条件。
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
class BattleSettlementData;
class BattlePlayerSnapshot;

class PlayerBattleSystem
{
public:
	// match -> scene:冻结玩家并返回战斗快照。
	// 校验:在线(实体在本节点且有 gate 会话)/ 无 InBattleComp / 未处于跨 zone 冻结。
	// 成功:挂 InBattleComp{state=PREPARING} + SET battle:lock:{player_id}(EX=deadline+60s,
	// 咨询性锁,权威判定仍是 InBattleComp)+ 返回 BattlePlayerSnapshot。
	// 失败:response.error_message 填拒绝原因,不留任何冻结痕迹。
	static void PrepareBattle(const ::PrepareBattleRequest& request, ::PrepareBattleResponse& response);

	// match -> scene:gather 失败补偿。battle_id 与 InBattleComp 匹配才摘组件 + DEL 锁;
	// 不匹配(迟到取消)忽略。玩家实体不在时按锁值匹配条件删锁(幂等)。
	static void CancelBattlePrepare(const ::CancelBattlePrepareRequest& request);

	// battle --Kafka--> scene:应用战斗结算。
	// InBattleComp.battle_id 匹配才应用(HP/MP 终值 + 金钱;经验/道具见 open_issues),
	// 然后摘组件 + DEL 锁 + 推 BattleEndS2C;不匹配丢弃并告警(at-least-once 去重);
	// 玩家实体不在 -> 校验锁值后写 battle:settlement:pending:{player_id}(TTL 7 天)。
	static void ApplySettlement(const ::BattleSettlementEvent& event);

	// 登录/重连后置钩子(player_lifecycle.cpp EnterScene 末尾调用):
	//   1) 查 Redis 挂起结算,有则应用并清理(先应用挂起结算再放开排队,§3.2);
	//   2) RECONNECT 且仍有 InBattleComp -> 向 gate 重发 BindBattleEvent + 推 BattleReconnectS2C。
	static void OnPlayerEnterScene(entt::entity player, uint32_t enterGsType);

	// 冻结拦截查询:InBattleComp 存在即在战(切场景/跨 zone 等入口用)。
	static bool IsInBattle(entt::entity player);

	// reaper:30s 低频定时器,扫描 InBattleComp.deadline_ms,过期即摘组件 + DEL 锁 + 告警。
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
	// 组 BattlePlayerSnapshot(属性/等级/技能/参战 buff/道具副本/路由信息)。
	// 失败时把拒绝原因写进 errorTipId 并返回 false(缺基础属性组件/缺会话快照)。
	static bool BuildBattleSnapshot(entt::entity player, ::BattlePlayerSnapshot& snapshot, uint32_t& errorTipId);

	// 把结算终值落到玩家实体(HP/MP 写 BaseAttributesComp + 属性脏位;金钱走 CurrencySystem)。
	static void ApplySettlementToEntity(entt::entity player, const ::BattleSettlementData& settlement);

	// RECONNECT 重绑:经 Kafka gate-{gate_id} 发 BindBattleEvent(GateCommand,
	// target_instance_id 必填,宪法 §7 不变量 2),并推 BattleReconnectS2C。
	static void RebindBattleOnReconnect(entt::entity player);

	// 应用挂起结算并清理 pending key + 锁(登录钩子的 Redis 回调侧)。
	static void ApplyPendingSettlement(entt::entity player, const ::BattleSettlementEvent& event);
};
