// 排空迁移遇上路由 lane 还有请求的会话(handler/rpc/drain_migration.cpp Classify;docs/design/client-request-ordering.md O9,2026-11-10)。
//
// 推 124 会让 lane 的排队项不再发出(OnSessionRedirected 逐条回 1003),排在慢请求后面的 LeaveGame 就白发了。所以 lane 里还有
// 在途或排队的请求时这一拍不迁,等它空了再迁。不进 gate.vcxproj,登记在本工程;drain_migration.cpp 已随 drain_migration_test.cpp
// 那组编进本工程,这里只测 Classify。会话是局部对象,不进会话表:Classify 只读传入的会话。

#include <gtest/gtest.h>

#include <chrono>
#include <utility>

#include "handler/rpc/drain_migration.h"
#include "session/comp/router_lane_item.h"
#include "session/comp/scene_lane_item.h"
#include "session/comp/session_info_comp.h"

namespace
{
	using gate_drain_migration::Candidate;
	using gate_drain_migration::Classify;
	using gate_drain_migration::SteadyTime;

	constexpr SteadyTime kNow = SteadyTime{} + std::chrono::hours(1);
	constexpr std::size_t kDepth = 32;

	gate_lane::RouterLaneItem Item(gate_lane::CallSeq seq)
	{
		gate_lane::RouterLaneItem item;
		item.seq = seq;
		return item;
	}

	// 选角界面:验过票、本连接 Login 成功、没绑角色。
	void MakeRoleSelect(SessionInfo &session)
	{
		session.verified = true;
		session.loginSucceeded = true;
		session.incarnation = 1;
	}

	// 已进游戏:再绑上角色、有场景路由。
	void MakeInGame(SessionInfo &session)
	{
		MakeRoleSelect(session);
		session.playerId = 100000000000005ull;
		session.sceneId = 7001;
	}

	TEST(DrainMigrationLaneBusy, InGameSessionWithQueuedOrInFlightRequestsIsSkipped)
	{
		SessionInfo session;
		MakeInGame(session);
		ASSERT_EQ(Classify(session, kNow), Candidate::kInGame);

		ASSERT_EQ(session.routerLane.Admit(Item(1), kDepth), gate_lane::AdmitResult::kQueued);
		EXPECT_EQ(Classify(session, kNow), Candidate::kSkip) << "排队项会在推 124 时作废";

		auto item = session.routerLane.TakeNextIfIdle();
		ASSERT_TRUE(item.has_value());
		ASSERT_TRUE(session.routerLane.MarkSent(item->seq));
		EXPECT_EQ(Classify(session, kNow), Candidate::kSkip) << "在途那条的回包还没回来";

		ASSERT_TRUE(session.routerLane.Complete(item->seq));
		EXPECT_EQ(Classify(session, kNow), Candidate::kInGame) << "lane 空了照常迁";
	}

	TEST(DrainMigrationLaneBusy, RoleSelectSessionWithQueuedRequestIsSkipped)
	{
		SessionInfo session;
		MakeRoleSelect(session);
		ASSERT_EQ(Classify(session, kNow), Candidate::kRoleSelect);

		ASSERT_EQ(session.routerLane.Admit(Item(1), kDepth), gate_lane::AdmitResult::kQueued);
		EXPECT_EQ(Classify(session, kNow), Candidate::kSkip);
	}

	// 场景 lane 同理(docs/design/client-scene-request-handoff-hold.md §4.6,2026-11-12):还有发往 scene 的请求在途、排队或
	// 等重投时这一拍不迁,免得客户端到新 gate 上发的请求抢在它们前面执行。
	TEST(DrainMigrationLaneBusy, InGameSessionWithBusySceneLaneIsSkipped)
	{
		SessionInfo session;
		MakeInGame(session);
		ASSERT_EQ(Classify(session, kNow), Candidate::kInGame);

		ASSERT_EQ(session.sceneLane.Admit(gate_scene_lane::SceneLaneItem{}, kDepth), gate_scene_lane::AdmitResult::kQueued);
		EXPECT_EQ(Classify(session, kNow), Candidate::kSkip) << "发往 scene 的请求还排着";

		auto item = session.sceneLane.TakeNextIfReady(kNow);
		ASSERT_TRUE(item.has_value());
		ASSERT_TRUE(session.sceneLane.MarkSent(std::move(*item), 1, kNow + std::chrono::seconds(13)));
		EXPECT_EQ(Classify(session, kNow), Candidate::kSkip) << "在途那条的回包还没回来";

		auto notExecuted = session.sceneLane.Complete(1);
		ASSERT_TRUE(notExecuted.has_value());
		session.sceneLane.HoldForRetry(std::move(*notExecuted), kNow + std::chrono::milliseconds(200));
		EXPECT_EQ(Classify(session, kNow), Candidate::kSkip) << "玩家在换场景节点,请求在等重投";

		ASSERT_TRUE(session.sceneLane.TakeNextIfReady(kNow + std::chrono::milliseconds(200)).has_value());
		EXPECT_EQ(Classify(session, kNow), Candidate::kInGame) << "场景 lane 空了照常迁";
	}
} // namespace
