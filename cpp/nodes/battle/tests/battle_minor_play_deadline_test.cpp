// 未成年人到点后 battle 不再提供战斗直连:时点与票据期限的判定单测
// (docs/design/real-name-anti-addiction.md §5.4,判定在 battle_minor_play_deadline.h)。
//
// 与 battle_ticket_test.cpp 同一种组织方式:不进 battle.vcxproj 的 ClCompile 列表,登记在
// cpp/tests/rpc_controller_test/rpc_controller_test.vcxproj 里随那个测试工程跑;两个头都不依赖
// muduo / protobuf / 引擎,也可以独立编。房间一侧的行为(按人记截止时刻、到点关直连、之后拒握手)在
// battle_observer_contract_test.cpp 的 MinorPlayDeadline* 用例里。
//
// 独立跑法(仓库根目录,Linux 或带 g++ 的容器;gtest 用仓库里已经 vendored 的那份):
//
//   GT=third_party/googletest/googletest
//   g++ -std=c++23 -I cpp/nodes/battle -I cpp/libs/engine/core -I "$GT/include" -I "$GT"
//       cpp/nodes/battle/tests/battle_minor_play_deadline_test.cpp
//       "$GT/src/gtest-all.cc" "$GT/src/gtest_main.cc"
//       -lssl -lcrypto -lpthread -o /tmp/battle_minor_play_deadline_test
//   /tmp/battle_minor_play_deadline_test
//
// (上面四行是同一条命令,换行只为可读;实际执行时接成一行。)

#include <gtest/gtest.h>

#include <cstdint>

#include "battle_minor_play_deadline.h"
#include "battle_security.h"

namespace
{

using battle_minor_deadline::DirectCloseAtMs;
using battle_minor_deadline::kCloseLeadMs;
using battle_minor_deadline::Reached;
using battle_minor_deadline::TicketExpireAtMs;

// 2026-11-27(周五)21:00 北京时间的 Unix 毫秒,只是个像样的数。
constexpr uint64_t kPlayDeadlineMs = 1'795'784'400'000ull;
constexpr uint64_t kCloseAtMs = kPlayDeadlineMs - kCloseLeadMs;

TEST(BattleMinorPlayDeadline, ClosesTheDirectConnectionAheadOfTheDeadline)
{
	// 提前量必须大于 0:21:00:00 之前直连已经断开(与 gate 关大厅连接同一时点)
	EXPECT_GT(kCloseLeadMs, 0u);
	EXPECT_EQ(DirectCloseAtMs(kPlayDeadlineMs), kCloseAtMs);
	EXPECT_LT(DirectCloseAtMs(kPlayDeadlineMs), kPlayDeadlineMs);
}

TEST(BattleMinorPlayDeadline, DeadlineShorterThanTheLeadIsAlreadyOver)
{
	// 截止时刻不足提前量(不会是真实的墙钟时刻):不回绕成一个巨大的数,按已经截止处理
	EXPECT_EQ(DirectCloseAtMs(kCloseLeadMs), 0u);
	EXPECT_EQ(DirectCloseAtMs(1), 0u);
	EXPECT_TRUE(Reached(1, 0));
}

TEST(BattleMinorPlayDeadline, ZeroMeansNoLimit)
{
	// 0 = 不限(成年人、平台管的小游戏渠道、没开防沉迷的部署):永远不到点,票据期限就是房间作废期限
	EXPECT_FALSE(Reached(0, 0));
	EXPECT_FALSE(Reached(0, UINT64_MAX));
	EXPECT_EQ(TicketExpireAtMs(kPlayDeadlineMs + 1, 0), kPlayDeadlineMs + 1);
}

TEST(BattleMinorPlayDeadline, ReachedFlipsExactlyAtTheCloseInstant)
{
	EXPECT_FALSE(Reached(kPlayDeadlineMs, kCloseAtMs - 1));
	EXPECT_TRUE(Reached(kPlayDeadlineMs, kCloseAtMs));
	EXPECT_TRUE(Reached(kPlayDeadlineMs, kPlayDeadlineMs));
	EXPECT_TRUE(Reached(kPlayDeadlineMs, UINT64_MAX));
}

TEST(BattleMinorPlayDeadline, TicketExpiresAtTheEarlierOfRoomDeadlineAndCloseInstant)
{
	// 截止时刻先到:票据按直连服务截止时刻封顶
	EXPECT_EQ(TicketExpireAtMs(kPlayDeadlineMs + 300'000, kPlayDeadlineMs), kCloseAtMs);
	// 房间先作废:沿用房间作废期限,不因截止时刻变长
	EXPECT_EQ(TicketExpireAtMs(kCloseAtMs - 60'000, kPlayDeadlineMs), kCloseAtMs - 60'000);
	// 两者相同
	EXPECT_EQ(TicketExpireAtMs(kCloseAtMs, kPlayDeadlineMs), kCloseAtMs);
}

TEST(BattleMinorPlayDeadline, CappedTicketIsRefusedAsExpiredFromTheCloseInstantOn)
{
	// 封顶后的票据与直连面的过期判定(ClassifyTicketFields:expire_at_ms <= now 即过期)对在一起:
	// 到点前一毫秒还能握手,到点那一刻起同一张票被判过期,和 Reached 在同一时刻翻转。
	battle_security::TicketFields ticket;
	ticket.battleId = 65047318352658432ull;
	ticket.playerId = 9001;
	ticket.nodeId = 7;
	ticket.instanceId = "battle-uuid-1";
	ticket.role = static_cast<int>(battle_security::TicketRole::kParticipant);
	ticket.expireAtMs = TicketExpireAtMs(kPlayDeadlineMs + 300'000, kPlayDeadlineMs);

	EXPECT_EQ(battle_security::ClassifyTicketFields(ticket, 7, "battle-uuid-1", kCloseAtMs - 1),
			  battle_security::TicketVerdict::kOk);
	EXPECT_FALSE(Reached(kPlayDeadlineMs, kCloseAtMs - 1));
	EXPECT_EQ(battle_security::ClassifyTicketFields(ticket, 7, "battle-uuid-1", kCloseAtMs),
			  battle_security::TicketVerdict::kExpired);
	EXPECT_TRUE(Reached(kPlayDeadlineMs, kCloseAtMs));
}

TEST(BattleMinorPlayDeadline, IsUsableInConstantExpressions)
{
	// constexpr 判定:不依赖任何运行期状态
	static_assert(Reached(kPlayDeadlineMs, kCloseAtMs));
	static_assert(!Reached(0, kPlayDeadlineMs));
	static_assert(TicketExpireAtMs(kPlayDeadlineMs + 1, kPlayDeadlineMs) == kCloseAtMs);
	SUCCEED();
}

} // namespace
