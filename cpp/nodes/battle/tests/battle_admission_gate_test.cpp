// battle 建房准入闸单测(集群外入口 D82 的进程级补充):启动完成之前、停机开始之后一律拒绝建房,
// 且停机是终态 —— 停机先于启动完成时,迟到的 Open 不能把节点重新打开。
// 同一文件还测可分配状态发布的决策(battle_admission_advertise.h,R6):期望状态的判定、只在回读不一致时写、
// 看不到自己的键或注册链忙时不写、回读迟迟不跟上时按 kResendAfter 补写。
//
// 与 battle_push_policy_test.cpp 同一种组织方式:**不进 battle.vcxproj 的 ClCompile 列表**,
// 不会被 MSBuild 编进 battle 可执行文件,也不在 CMakeLists.txt 里 —— 独立跑。
// battle_admission_gate.h 刻意不依赖 muduo / protobuf / 引擎,就是为了让这份测试能在没有
// 整套 C++ 引擎的机器上直接编出来。
//
// 跑法(仓库根目录,Linux 或带 g++ 的容器;gtest 用仓库里已经 vendored 的那份):
//
//   GT=third_party/googletest/googletest
//   g++ -std=c++23 -I cpp/nodes/battle -I "$GT/include" -I "$GT"
//       cpp/nodes/battle/tests/battle_admission_gate_test.cpp
//       "$GT/src/gtest-all.cc" "$GT/src/gtest_main.cc"
//       -lpthread -o /tmp/battle_admission_gate_test
//   /tmp/battle_admission_gate_test
//
// (上面四行是同一条命令,换行只为可读;实际执行时接成一行。)

#include <gtest/gtest.h>

#include <chrono>
#include <optional>
#include <string>

#include "battle_admission_advertise.h"
#include "battle_admission_gate.h"

namespace
{

using battle_admission::AcceptsNewRooms;
using battle_admission::AdmissionGate;
using battle_admission::AdmissionPhase;
using battle_admission::Advertised;
using battle_admission::AdvertiseAction;
using battle_admission::AdvertisePolicy;
using battle_admission::DesiredAdvertised;
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

// ---- 可分配状态发布(battle_admission_advertise.h,R6) ----

using Clock = std::chrono::steady_clock;

// 任取的固定起点;决策只看时间差。
const Clock::time_point kT0 = Clock::time_point{} + std::chrono::hours(1);

TEST(BattleAdmissionAdvertise, DesiredIsOpenOnlyWhenGateOpenAndLifecycleGrants)
{
	EXPECT_EQ(DesiredAdvertised(AdmissionPhase::kOpen, true), Advertised::kOpen);
	// 闸开着但 Agones 不放行许可(排空、Starting、停机中):不接新房间
	EXPECT_EQ(DesiredAdvertised(AdmissionPhase::kOpen, false), Advertised::kClosed);
	// 启动未完成、停机开始之后:不论 lifecycle 都不接
	EXPECT_EQ(DesiredAdvertised(AdmissionPhase::kNotStarted, true), Advertised::kClosed);
	EXPECT_EQ(DesiredAdvertised(AdmissionPhase::kClosed, true), Advertised::kClosed);
	EXPECT_TRUE(AcceptsNewRooms(AdmissionPhase::kOpen, true));
	EXPECT_FALSE(AcceptsNewRooms(AdmissionPhase::kClosed, true));
}

TEST(BattleAdmissionAdvertise, NoWriteUntilOwnKeyIsVisible)
{
	// 首发还没回 / 已失租:此刻写会抢在注册 CAS 之前建出服务键,只能等框架整份发布内存里的期望值
	AdvertisePolicy policy;
	EXPECT_EQ(policy.Decide(Advertised::kOpen, std::nullopt, true, kT0), AdvertiseAction::kNone);
}

TEST(BattleAdmissionAdvertise, NoWriteWhileRegistrationBusy)
{
	AdvertisePolicy policy;
	EXPECT_EQ(policy.Decide(Advertised::kOpen, Advertised::kClosed, false, kT0), AdvertiseAction::kNone);
	// 注册链空闲之后照常写,不因为之前被挡过而推迟
	EXPECT_EQ(policy.Decide(Advertised::kOpen, Advertised::kClosed, true, kT0 + std::chrono::seconds(1)),
			  AdvertiseAction::kWrite);
}

TEST(BattleAdmissionAdvertise, ConvergedStateWritesNothing)
{
	// 稳态零写入:回读与期望一致就不写,不论过了多久
	AdvertisePolicy policy;
	for (int second = 0; second < 600; ++second)
	{
		EXPECT_EQ(policy.Decide(Advertised::kOpen, Advertised::kOpen, true, kT0 + std::chrono::seconds(second)),
				  AdvertiseAction::kNone);
	}
}

TEST(BattleAdmissionAdvertise, WritesOnceOnChangeThenWaitsForReadback)
{
	AdvertisePolicy policy;
	ASSERT_EQ(policy.Decide(Advertised::kOpen, Advertised::kClosed, true, kT0), AdvertiseAction::kWrite);
	// 回读还没跟上:kResendAfter 内不重复写
	EXPECT_EQ(policy.Decide(Advertised::kOpen, Advertised::kClosed, true, kT0 + std::chrono::seconds(1)),
			  AdvertiseAction::kNone);
	EXPECT_EQ(policy.Decide(Advertised::kOpen, Advertised::kClosed, true,
							kT0 + AdvertisePolicy::kResendAfter - std::chrono::milliseconds(1)),
			  AdvertiseAction::kNone);
	// 回读到了:之后不再写
	EXPECT_EQ(policy.Decide(Advertised::kOpen, Advertised::kOpen, true, kT0 + std::chrono::seconds(10)),
			  AdvertiseAction::kNone);
}

TEST(BattleAdmissionAdvertise, ResendsWhenReadbackStaysStale)
{
	// Put 静默丢失:同一个值写出 kResendAfter 之后回读仍是旧值,补写;补写之后同样再等 kResendAfter
	AdvertisePolicy policy;
	ASSERT_EQ(policy.Decide(Advertised::kOpen, Advertised::kClosed, true, kT0), AdvertiseAction::kWrite);
	const auto resendAt = kT0 + AdvertisePolicy::kResendAfter;
	EXPECT_EQ(policy.Decide(Advertised::kOpen, Advertised::kClosed, true, resendAt), AdvertiseAction::kResend);
	EXPECT_EQ(policy.Decide(Advertised::kOpen, Advertised::kClosed, true, resendAt + std::chrono::seconds(1)),
			  AdvertiseAction::kNone);
	EXPECT_EQ(policy.Decide(Advertised::kOpen, Advertised::kClosed, true, resendAt + AdvertisePolicy::kResendAfter),
			  AdvertiseAction::kResend);
}

TEST(BattleAdmissionAdvertise, NewDesiredValueIsWrittenWithoutWaiting)
{
	// 刚写完 OPEN 就开始排空:CLOSED 是新值,不等 kResendAfter
	AdvertisePolicy policy;
	ASSERT_EQ(policy.Decide(Advertised::kOpen, Advertised::kClosed, true, kT0), AdvertiseAction::kWrite);
	EXPECT_EQ(policy.Decide(Advertised::kClosed, Advertised::kOpen, true, kT0 + std::chrono::seconds(1)),
			  AdvertiseAction::kWrite);
}

TEST(BattleAdmissionAdvertise, FlipBackBeforeReadbackConvergesOnLaterReadback)
{
	// 写了 OPEN、回读还没跟上时又回到 CLOSED:回读仍是 CLOSED,与期望一致,不写;
	// 迟到的 OPEN 回读出现后按不一致重写 CLOSED(按电平对账,最终收敛)
	AdvertisePolicy policy;
	ASSERT_EQ(policy.Decide(Advertised::kOpen, Advertised::kClosed, true, kT0), AdvertiseAction::kWrite);
	EXPECT_EQ(policy.Decide(Advertised::kClosed, Advertised::kClosed, true, kT0 + std::chrono::seconds(1)),
			  AdvertiseAction::kNone);
	EXPECT_EQ(policy.Decide(Advertised::kClosed, Advertised::kOpen, true, kT0 + std::chrono::seconds(2)),
			  AdvertiseAction::kWrite);
}

TEST(BattleAdmissionAdvertise, UnspecifiedReadbackIsRewritten)
{
	// 回读是不认识本字段的旧版本写的值:与期望不一致,照常写
	AdvertisePolicy policy;
	EXPECT_EQ(policy.Decide(Advertised::kClosed, Advertised::kUnspecified, true, kT0), AdvertiseAction::kWrite);
}

TEST(BattleAdmissionAdvertise, NamesAreStableForLogs)
{
	EXPECT_EQ(std::string(ToString(Advertised::kUnspecified)), "unspecified");
	EXPECT_EQ(std::string(ToString(Advertised::kOpen)), "open");
	EXPECT_EQ(std::string(ToString(Advertised::kClosed)), "closed");
}

} // namespace
