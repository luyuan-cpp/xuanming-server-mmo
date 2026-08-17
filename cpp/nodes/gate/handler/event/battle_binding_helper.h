#pragma once

#include "proto/contracts/kafka/gate_event.pb.h"
#include "type_define/type_define.h"

// 回合制战斗的 gate 侧会话绑定(设计文档 turn-based-battle-server.md §5.5)。
//
// gate_event_handler.cpp 的 BindBattleEvent / UnbindBattleEvent 守护段只放一行
// 委托,逻辑集中在本手写文件,重生成时不受影响。
//
// 绑定分两部分:
//   * 节点实体绑定:写在 SessionInfo.SetEntityId(BattleNodeService, ...),
//     客户端战斗消息(HandleGrpcNodeMessage)按它转发;
//   * battle_id 记录:存在本文件的 gate 本地表里,UnbindBattleEvent 必须按
//     battle_id 匹配当前记录才能清除(防迟到的解绑清掉新战斗的绑定)。
namespace gate_battle_binding
{
	// Bind:按 battle_node_id 解析 gate 本地 battle 节点实体(battle 是全局池、
	// 非 zone-scoped,node_id 由 etcd 全局 CAS 保证同类型唯一,按 node_id 直查、
	// 不带 zone),写入会话绑定并记录 battle_id。
	void HandleBindBattle(const contracts::kafka::BindBattleEvent &event);

	// Unbind:battle_id 匹配当前记录才清除;不匹配/无记录时幂等忽略。
	void HandleUnbindBattle(const contracts::kafka::UnbindBattleEvent &event);

	// 会话断开、battle 节点被摘除时清理 battle_id 记录(节点实体绑定的清理
	// 由调用方自身路径负责:断开走 sessions.erase,摘除走 OnNodeRemoveEvent)。
	void ClearBattleRecord(SessionId sessionId);
}
