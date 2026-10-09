#pragma once

// battle 下行(S2C)出口的路由判定(turn-based §22 D68,落 D39)。
//
// 直连是战斗唯一通路:战斗帧只走直连,没有活直连就丢弃(客户端直连就绪后 GetBattleState
// 补拉);只有发生在直连建立之前、客户端据此才能建直连的大厅公告(NotifyBattleAssigned /
// NotifyBattleStart)在没有活直连时回落 Kafka→gate。
//
// 与 battle_security.h 同一条纪律:纯函数,不依赖 muduo / protobuf / 引擎,只返回结论
// 不打日志、不做 I/O —— 发送与丢弃日志由调用点(BattleRoomManager::PushBattleFrame /
// PushLobbyAnnouncement)负责。这样本头能在没有整套引擎的机器上直接编单测
// (tests/battle_push_policy_test.cpp)。

#include <cstdint>

namespace battle_push_policy
{

// 推送类别:决定"没有活直连时"怎么办。调用点按出口函数固定类别,不按消息号查表 ——
// 哪条消息属于哪一类写在调用点上,一眼可见。
enum class PushCategory : uint8_t
{
	// 直连建立之前必须送达的大厅公告:NotifyBattleAssigned / NotifyBattleStart。
	kLobbyAnnouncement,
	// 战斗帧:TurnResult / BattleEnd / SpectateState / SpectateTurnResult / SpectateEnd,
	// 以及直连建立后的一切战斗推送。
	kBattleFrame,
};

// 路由结论。
enum class PushRoute : uint8_t
{
	kDirect,  // 经该玩家的活直连直发
	kViaGate, // 经 Kafka→gate 的 PushToPlayerEvent 回落到大厅连接
	kDrop,    // 不发(调用方负责打采样日志)
};

// hasLiveDirect = 该玩家在房间里有一条已验证、仍 connected 的直连。
// 有活直连一律直发(大厅公告也一样:客户端已经在直连上了,不必再绕 gate);
// 没有活直连时,大厅公告回落 gate,其余(战斗帧,以及任何未来新增而没在这里显式放行的
// 类别)一律丢弃 —— 回落是白名单,不是默认。
constexpr PushRoute Decide(const PushCategory category, const bool hasLiveDirect)
{
	if (hasLiveDirect)
	{
		return PushRoute::kDirect;
	}
	return category == PushCategory::kLobbyAnnouncement ? PushRoute::kViaGate : PushRoute::kDrop;
}

} // namespace battle_push_policy
