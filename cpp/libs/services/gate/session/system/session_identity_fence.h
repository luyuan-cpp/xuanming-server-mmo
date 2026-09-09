#pragma once

// gate 写客户端 socket 之前的**身份栅栏**(docs/design/routing-identity-audit-20260908.md
// R13 / R14)。纯函数,零依赖,故意从 gate_service_handler.cpp 里拆出来 —— 和
// node_kafka_command_filter.h 同一条理由:要测它就不该把 SessionManager、
// muduo TcpConnection、整条 RPC 注册链拖进测试工程。
//
// 为什么需要栅栏:
//   scene 只凭 session_id 反查 gate(session_id 高位嵌 gate 的 routing node_id),
//   而 routing node_id 是**立刻复用**的:gate G(node_id=3)重启后继任者 G' 仍拿 3 号,
//   scene 用旧 session_id 反查会命中 G' —— 于是 A 玩家的结算/提示/广播被写进 G' 上
//   **另一个玩家**的 socket。session_id 的低 17 位序号在单个 gate 实例内累计 131071 条
//   连接后还会回绕,所以只带 gate 实例 uuid 的代次栅栏挡不住"同一代次内的 session 复用"。
//   两种情况都只有一个共同解:把**目标玩家**带在消息里,写 socket 前和会话当前绑定的
//   玩家比对一次。
//
// 三态而不是两态:灰度窗口里老发送方不带 player_id(0),必须放行,否则升级顺序一反
// 就是全服推送静默消失。但"放行"和"校验通过"必须能被区分,否则迁移完成与否无从观测 ——
// kDeliverUnfenced 就是给这一条用的,调用方只在**首次**打一行 INFO。
// 灰度一个版本之后本枚举删掉 kDeliverUnfenced,0 直接归 kDrop。

#include <cstdint>

namespace gate_session_fence
{
	enum class PushVerdict
	{
		// 目标玩家与会话当前绑定一致:正常投递。
		kDeliver,
		// 发送方没带 player_id(老版本):放行,但调用方应记一次 INFO 供迁移观测。
		kDeliverUnfenced,
		// 目标玩家与会话当前绑定不一致:**丢弃**。这正是 node_id 复用 / session 序号回绕
		// 之后本该发生的事 —— 宁可丢一条推送,也不能把它写进别人的 socket。
		kDrop,
	};

	// sessionBoundPlayerId:gate 本地会话当前绑定的玩家(SessionInfo::playerId)。
	//   BindSession 之前是 kInvalidGuid(UINT64_MAX);会话存在但尚未绑定玩家。
	// targetPlayerId:消息里携带的目标玩家(0 = 发送方没带)。
	inline PushVerdict ClassifyPush(uint64_t sessionBoundPlayerId, uint64_t targetPlayerId)
	{
		if (targetPlayerId == 0)
		{
			return PushVerdict::kDeliverUnfenced;
		}
		if (sessionBoundPlayerId == targetPlayerId)
		{
			return PushVerdict::kDeliver;
		}
		// 会话尚未绑定玩家(kInvalidGuid / 0)同样丢弃,不留"未绑定就放行"的口子 ——
		// 那正是 R13 的失败形态本身:gate 重启后继任者把 session 5 发给一条刚连上、
		// 还没登录的新连接,而 scene 仍在用旧 session 5 推 A 的结算。放行等于把战斗
		// 结算写进一个陌生人的 socket。
		//
		// 反过来"会话未绑定却收到指名推送"在正常时序下不可能发生:scene 只会给
		// 已进场景的玩家推送,而进场景必然晚于 gate 侧 BindSession 落地。
		return PushVerdict::kDrop;
	}
} // namespace gate_session_fence
