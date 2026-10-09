// battle 建房准入闸单测(集群外入口 D82 的进程级补充):启动完成之前、停机开始之后一律拒绝建房,
// 且停机是终态 —— 停机先于启动完成时,迟到的 Open 不能把节点重新打开。
//
// 与 battle_push_policy_test.cpp 同一种组织方式:**不进 battle.vcxproj 的 ClCompile 列表**,
// 不会被 MSBuild 编进 battle 可执行文件,也不在 CMakeLists.txt 里 —— 独立跑。
// battle_admission_gate.h 刻意不依赖 muduo / protobuf / 引擎,就是为了让这份测试能在没有
// 整套 C++ 引擎的机器上直接编出来。
//
// 跑法(仓库根目录,Linux 或带 g++ 的容器;gtest 用仓库里已经 vendored 的那份):
//
//   GT=third_party/grpc/third_party/googletest/googletest
//   g++ -std=c++23 -I cpp/nodes/battle -I "$GT/include" -I "$GT"
//       cpp/nodes/battle/tests/battle_admission_gate_test.cpp
//       "$GT/src/gtest-all.cc" "$GT/src/gtest_main.cc"
//       -lpthread -o /tmp/battle_admission_gate_test
//   /tmp/battle_admission_gate_test
//
// (上面四行是同一条命令,换行只为可读;实际执行时接成一行。)

#include <gtest/gtest.h>

#include <string>

#include "battle_admission_gate.h"

namespace
{

using battle_admission::AdmissionGate;
using battle_admission::AdmissionPhase;
using battle_admission::ToString;

TEST(BattleAdmissionGate, StartsClosedForNewRoomsUntilOpened)
{
	// 启动窗口:etcd 已发布、lifecycle 还没启动,此时的 CreateBattle 必须被拒
	AdmissionGate gate;
	EXPECT_EQ(gate.Phase(), AdmissionPhase::kNotStarted);
}

TEST(BattleAdmissionGate, OpenAdmitsNewRooms)
{
	AdmissionGate gate;
	EXPECT_TRUE(gate.Open());
	EXPECT_EQ(gate.Phase(), AdmissionPhase::kOpen);
}

TEST(BattleAdmissionGate, RepeatedOpenIsNoOp)
{
	AdmissionGate gate;
	ASSERT_TRUE(gate.Open());
	EXPECT_FALSE(gate.Open());
	EXPECT_EQ(gate.Phase(), AdmissionPhase::kOpen);
}

TEST(BattleAdmissionGate, CloseRejectsNewRooms)
{
	// 停机窗口:SetBeforeShutdown 关闸之后,排在它后面的建房一律被拒
	AdmissionGate gate;
	ASSERT_TRUE(gate.Open());
	gate.Close();
	EXPECT_EQ(gate.Phase(), AdmissionPhase::kClosed);
}

TEST(BattleAdmissionGate, ClosedIsTerminalEvenBeforeOpen)
{
	// 停机先于启动完成(例如启动期 node_id 冲突触发停机):迟到的 Open 不得重新打开
	AdmissionGate gate;
	gate.Close();
	EXPECT_FALSE(gate.Open());
	EXPECT_EQ(gate.Phase(), AdmissionPhase::kClosed);
}

TEST(BattleAdmissionGate, CloseIsIdempotent)
{
	AdmissionGate gate;
	gate.Close();
	gate.Close();
	EXPECT_EQ(gate.Phase(), AdmissionPhase::kClosed);
}

TEST(BattleAdmissionGate, PhaseNamesAreStableForLogs)
{
	EXPECT_EQ(std::string(ToString(AdmissionPhase::kNotStarted)), "not_started");
	EXPECT_EQ(std::string(ToString(AdmissionPhase::kOpen)), "open");
	EXPECT_EQ(std::string(ToString(AdmissionPhase::kClosed)), "closed");
}

} // namespace
