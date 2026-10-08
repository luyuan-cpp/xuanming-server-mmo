// battle 下行出口路由判定单测(turn-based §22 D68,落 D39:战斗帧只走直连,
// 只有大厅公告在无直连时回落 Kafka→gate)。
//
// 与 battle_ticket_test.cpp 同一种组织方式:**不进 battle.vcxproj 的 ClCompile 列表**,
// 不会被 MSBuild 编进 battle 可执行文件,也不在 CMakeLists.txt 里 —— 独立跑。
// battle_push_policy.h 刻意不依赖 muduo / protobuf / 引擎,就是为了让这份测试能在没有
// 整套 C++ 引擎的机器上直接编出来。
//
// 跑法(仓库根目录,Linux 或带 g++ 的容器;gtest 用仓库里已经 vendored 的那份):
//
//   GT=third_party/grpc/third_party/googletest/googletest
//   g++ -std=c++23 -I cpp/nodes/battle -I "$GT/include" -I "$GT"
//       cpp/nodes/battle/tests/battle_push_policy_test.cpp
//       "$GT/src/gtest-all.cc" "$GT/src/gtest_main.cc"
//       -lpthread -o /tmp/battle_push_policy_test
//   /tmp/battle_push_policy_test
//
// (上面四行是同一条命令,换行只为可读;实际执行时接成一行。)

#include <gtest/gtest.h>

#include <initializer_list>

#include "battle_push_policy.h"

namespace
{

using battle_push_policy::Decide;
using battle_push_policy::PushCategory;
using battle_push_policy::PushRoute;

TEST(BattlePushPolicy, BattleFrameGoesDirectWhenDirectIsLive)
{
	EXPECT_EQ(Decide(PushCategory::kBattleFrame, true), PushRoute::kDirect);
}

TEST(BattlePushPolicy, BattleFrameIsDroppedWithoutLiveDirect)
{
	// D39 核心:战斗帧没有活直连时绝不回落 gate(回落 = 维持第二条战斗通路)
	EXPECT_EQ(Decide(PushCategory::kBattleFrame, false), PushRoute::kDrop);
}

TEST(BattlePushPolicy, LobbyAnnouncementGoesDirectWhenDirectIsLive)
{
	// 客户端已在直连上(重连 / 观众同会话幂等重推):大厅公告也不必再绕 gate
	EXPECT_EQ(Decide(PushCategory::kLobbyAnnouncement, true), PushRoute::kDirect);
}

TEST(BattlePushPolicy, LobbyAnnouncementFallsBackToGateWithoutLiveDirect)
{
	// Assigned / Start 发生在直连建立之前,不回落客户端就永远拿不到票据
	EXPECT_EQ(Decide(PushCategory::kLobbyAnnouncement, false), PushRoute::kViaGate);
}

TEST(BattlePushPolicy, OnlyLobbyAnnouncementEverRoutesViaGate)
{
	// 回落是白名单:两种类别 × 两种直连状态里,只有"大厅公告 + 无直连"一格走 gate
	int viaGate = 0;
	for (const PushCategory category : {PushCategory::kLobbyAnnouncement, PushCategory::kBattleFrame})
	{
		for (const bool hasLiveDirect : {false, true})
		{
			if (Decide(category, hasLiveDirect) == PushRoute::kViaGate)
			{
				++viaGate;
				EXPECT_EQ(category, PushCategory::kLobbyAnnouncement);
				EXPECT_FALSE(hasLiveDirect);
			}
		}
	}
	EXPECT_EQ(viaGate, 1);
}

TEST(BattlePushPolicy, IsUsableInConstantExpressions)
{
	// constexpr 判定:调用点零开销,也保证它不依赖任何运行期状态
	static_assert(Decide(PushCategory::kBattleFrame, false) == PushRoute::kDrop);
	static_assert(Decide(PushCategory::kLobbyAnnouncement, false) == PushRoute::kViaGate);
	SUCCEED();
}

} // namespace
