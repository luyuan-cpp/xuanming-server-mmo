#pragma once

// 未成年人到点之后 battle 不再提供战斗直连(docs/design/real-name-anti-addiction.md §5.4)。
//
// 未成年人只能在可玩日的 20:00–21:00 玩,可玩时段之外不能以任何形式提供游戏服务。大厅连接由 gate 到点关掉
// (cpp/nodes/gate/handler/rpc/minor_play_deadline.h);战斗流量走客户端直连 battle 的第二条连接,不经过 gate,
// 由 battle 自己执行。截止时刻(Unix 毫秒,0 = 不限)由 match 从玩家会话记录 PlayerSession.minor_play_deadline_ms 取,
// 随 CreateBattle(每名参战者的快照)与 AddObserver 带来,房间按人记下(BattleRoomManager):
//   签票   票据期限取房间作废期限与该玩家直连服务截止时刻里较早的一个(TicketExpireAtMs);
//   到点   关掉他的直连,此后本场不再接受他握手(Reached);
//   战斗   照常进行:连接关掉之后与掉线同一处置,参战者的回合按超时自动出手。
//
// 与 battle_push_policy.h 同一条纪律:纯函数,不依赖 muduo / protobuf / 引擎,不打日志、不做 I/O,
// 能在没有整套引擎的机器上直接编单测(tests/battle_minor_play_deadline_test.cpp)。

#include <algorithm>
#include <cstdint>

namespace battle_minor_deadline
{

// 提前这么久停止直连服务。与 gate 关大厅连接的提前量同值(gate_minor_deadline::kCloseLeadMs):两条连接在同一时点
// 断开,都早于截止时刻。两边不必严格相等,但都必须大于 0;改这里时一起看 gate 那一个。
inline constexpr uint64_t kCloseLeadMs = 5000;

// 直连服务截止时刻 = 截止时刻 − 提前量。只对有截止时刻(非 0)的人有意义;截止时刻不足提前量时取 0(已经截止)。
constexpr uint64_t DirectCloseAtMs(const uint64_t playDeadlineMs)
{
	return playDeadlineMs > kCloseLeadMs ? playDeadlineMs - kCloseLeadMs : 0;
}

// 到点了没有:有截止时刻,且 nowMs 不早于直连服务截止时刻。0 = 不限,永远不到点。
// 与票据过期的判定(battle_security::ClassifyTicketFields 的 expire_at_ms <= now)在同一时刻翻转。
constexpr bool Reached(const uint64_t playDeadlineMs, const uint64_t nowMs)
{
	return playDeadlineMs != 0 && nowMs >= DirectCloseAtMs(playDeadlineMs);
}

// 票据期限:没有截止时刻的人就是房间作废期限;有的再按直连服务截止时刻封顶,取较早的一个。
constexpr uint64_t TicketExpireAtMs(const uint64_t roomDeadlineMs, const uint64_t playDeadlineMs)
{
	return playDeadlineMs == 0 ? roomDeadlineMs : std::min(roomDeadlineMs, DirectCloseAtMs(playDeadlineMs));
}

} // namespace battle_minor_deadline
