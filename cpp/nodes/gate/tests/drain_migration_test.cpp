// gate 排空迁移调度的单测(handler/rpc/drain_migration.h,docs/design/gate-drain-session-migration.md §4.3)。
//
// 不进 gate.vcxproj,登记在 cpp/tests/routing_identity_test/routing_identity_test.vcxproj,被测源码直接编进测试。
// 两个出口(要票、推 124)注入替身:要票替身记下请求、把回调攒起来,由用例回放成各种应答;推送替身照真实出口的收尾
// 标记会话。时刻全部由用例给定,被测代码不读时钟。滞留关闭要真的关一条连接,那一组用真实的回环 TCP
// (夹具在 loopback_connection.h)。计数是进程累计的 thread_local,SetUp 里 ResetForTesting 清零。
// 会话表是 unordered_map,遍历顺序不确定:用例只断言「要了几张票、各要过一次」,不断言先后。
//
// 真实的推送出口 PushRedirect 另有 gate_redirect_delivery_test.cpp。没覆盖的一层:要票出口
// (client_session_handler.cpp 的 SendGateMigrationRequest)与 main.cpp 的定时器,它们拖着节点注册表与 gRPC 通道,
// 进不了这个工程;靠核对调用点与 robot 冒烟(robot 的 gate_migrate_smoke 模式)。

#include <gtest/gtest.h>

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <functional>
#include <memory>
#include <set>
#include <string>
#include <utility>
#include <vector>

#include "handler/rpc/drain_migration.h"
#include "loopback_connection.h"
#include "muduo/net/EventLoop.h"
#include "session/manager/session_manager.h"

namespace
{
	using gate_drain_migration::Candidate;
	using gate_drain_migration::Classify;
	using gate_drain_migration::GetCounters;
	using gate_drain_migration::kEnterGameSettle;
	using gate_drain_migration::kRedirectLinger;
	using gate_drain_migration::kRetryBackoff;
	using gate_drain_migration::LeaseActive;
	using gate_drain_migration::PushOutcome;
	using gate_drain_migration::ReplyCallback;
	using gate_drain_migration::RequestReply;
	using gate_drain_migration::SteadyTime;
	using gate_drain_migration::Tick;
	using std::chrono::milliseconds;
	using std::chrono::seconds;

	constexpr Guid kPlayerA = 100000000000001ull;
	constexpr Guid kPlayerB = 100000000000002ull;
	constexpr Guid kPlayerC = 100000000000003ull;
	constexpr uint64_t kScene = 7001;
	constexpr int64_t kDrainMarkedAt = 1793000000;
	constexpr uint32_t kDefaultLeaseMs = 60000;

	// 单调时钟上的任意起点。被测代码只比较时刻先后。
	constexpr SteadyTime kT0 = SteadyTime{} + std::chrono::hours(1);

	// ---- 两个出口的替身 ----

	// 要票出口:accept = false 模拟「没发出」(没有路由服 / login 实例、缺通道)。发出的请求按顺序记下,回调攒着等用例回放。
	struct FakeLogin
	{
		bool accept{true};
		std::vector<SessionId> requested;
		std::vector<ReplyCallback> pending;
	};

	FakeLogin &Login()
	{
		static FakeLogin login;
		return login;
	}

	bool FakeSend(SessionId sessionId, const SessionInfo &, ReplyCallback done)
	{
		FakeLogin &login = Login();
		if (!login.accept)
		{
			return false;
		}
		login.requested.push_back(sessionId);
		login.pending.push_back(std::move(done));
		return true;
	}

	struct PushRecord
	{
		SessionId sessionId{0};
		Guid ticketPlayerId{0};
		RedirectToGateNotify notify;
	};

	// 推 124 出口:outcome 不是 kPushed 时只记录、不改会话。推成功时照真实出口(gate_redirect_delivery.cpp)的收尾:
	// lane 置重定向、登记滞留关闭。
	struct FakeClient
	{
		PushOutcome outcome{PushOutcome::kPushed};
		std::vector<PushRecord> pushes;
	};

	FakeClient &Client()
	{
		static FakeClient client;
		return client;
	}

	PushOutcome FakePush(SessionId sessionId, Guid ticketPlayerId, const RedirectToGateNotify &notify, SteadyTime now)
	{
		FakeClient &client = Client();
		client.pushes.push_back(PushRecord{sessionId, ticketPlayerId, notify});
		if (client.outcome != PushOutcome::kPushed)
		{
			return client.outcome;
		}
		SessionInfo &session = tlsSessionManager.sessions().at(sessionId);
		(void)session.routerLane.MarkRedirected();
		gate_drain_migration::NoteRedirected(sessionId, session, now);
		return PushOutcome::kPushed;
	}

	// ---- 会话与消息 ----

	// 已进游戏:验过票、登录成功、绑了角色、有场景路由。
	SessionInfo &AddInGame(SessionId sessionId, Guid playerId, uint64_t incarnation)
	{
		SessionInfo &session = tlsSessionManager.sessions()[sessionId];
		session.verified = true;
		session.loginSucceeded = true;
		session.playerId = playerId;
		session.sceneId = kScene;
		session.incarnation = incarnation;
		return session;
	}

	// 停在选角界面:验过票、登录成功、没绑角色。
	SessionInfo &AddRoleSelect(SessionId sessionId, uint64_t incarnation)
	{
		SessionInfo &session = tlsSessionManager.sessions()[sessionId];
		session.verified = true;
		session.loginSucceeded = true;
		session.incarnation = incarnation;
		return session;
	}

	contracts::kafka::GateDrainMigrationEvent Lease(uint32_t ratePerSecond, uint32_t leaseMs = kDefaultLeaseMs)
	{
		contracts::kafka::GateDrainMigrationEvent event;
		event.set_drain_marked_at(kDrainMarkedAt);
		event.set_rate_per_second(ratePerSecond);
		event.set_lease_ms(leaseMs);
		return event;
	}

	RequestReply Reply(loginpb::GateMigrationResult result)
	{
		RequestReply reply;
		reply.delivered = true;
		reply.response.set_result(result);
		if (result == loginpb::GATE_MIGRATION_RESULT_OK)
		{
			reply.response.set_target_gate_ip("10.0.0.7");
			reply.response.set_target_gate_port(7101);
			reply.response.set_token_payload("payload");
			reply.response.set_token_signature("signature");
			reply.response.set_token_deadline(1793000300);
		}
		return reply;
	}

	// 回放第 index 个攒着的应答,receivedAt 是应答到达时刻。
	void Answer(std::size_t index, const RequestReply &reply, SteadyTime receivedAt)
	{
		ASSERT_LT(index, Login().pending.size());
		Login().pending[index](reply, receivedAt);
	}

	void ResetState()
	{
		tlsSessionManager.sessions().clear();
		gate_drain_migration::ResetForTesting();
		Login() = FakeLogin{};
		Client() = FakeClient{};
	}

	class DrainMigration : public testing::Test
	{
	protected:
		void SetUp() override
		{
			ResetState();
			gate_drain_migration::SetRequestSender(&FakeSend);
			gate_drain_migration::SetRedirectPusher(&FakePush);
		}

		void TearDown() override
		{
			gate_drain_migration::SetRequestSender(nullptr);
			gate_drain_migration::SetRedirectPusher(nullptr);
			ResetState();
		}
	};

	// 一种应答让许可停下:之后不再要票,也不推票。
	void ExpectReplyStopsLease(loginpb::GateMigrationResult result)
	{
		AddInGame(1, kPlayerA, 1);
		AddInGame(2, kPlayerB, 2);
		gate_drain_migration::OnLease(Lease(1), kT0);
		Tick(kT0);
		ASSERT_EQ(Login().requested.size(), 1u);

		Answer(0, Reply(result), kT0 + milliseconds(20));

		EXPECT_FALSE(LeaseActive(kT0 + milliseconds(20)));
		Tick(kT0 + seconds(1));
		EXPECT_EQ(Login().requested.size(), 1u);
		EXPECT_TRUE(Client().pushes.empty());
		EXPECT_EQ(GetCounters().leasesStopped, 1u);
	}
} // namespace

// ---- 选人判定 ----

TEST_F(DrainMigration, InGameRequiresVerifiedLoggedInBoundRoleWithASceneRoute)
{
	SessionInfo &session = AddInGame(1, kPlayerA, 1);
	EXPECT_EQ(Classify(session, kT0), Candidate::kInGame);

	session.verified = false;
	EXPECT_EQ(Classify(session, kT0), Candidate::kSkip);
	session.verified = true;

	session.loginSucceeded = false;
	EXPECT_EQ(Classify(session, kT0), Candidate::kSkip);
	session.loginSucceeded = true;

	// 绑定已到、场景路由还没到:进场途中。
	session.sceneId = 0;
	EXPECT_EQ(Classify(session, kT0), Candidate::kSkip);
	session.sceneId = kScene;

	// 路由到了、还没交给 scene(切场景或补发欠账中):新连接上的重连会和这次进场交错。
	session.pendingSceneEntry = PendingSceneEntry{};
	EXPECT_EQ(Classify(session, kT0), Candidate::kSkip);
	session.pendingSceneEntry.reset();

	EXPECT_EQ(Classify(session, kT0), Candidate::kInGame);
}

// 推过 124 的会话(跨 zone 传送或上一次迁移)不再迁:lane 已置重定向,或已记下推送时刻,两者任一即跳过。
TEST_F(DrainMigration, RedirectedSessionsAreSkipped)
{
	SessionInfo &laneRedirected = AddInGame(1, kPlayerA, 1);
	(void)laneRedirected.routerLane.MarkRedirected();
	EXPECT_EQ(Classify(laneRedirected, kT0), Candidate::kSkip);

	SessionInfo &pushed = AddInGame(2, kPlayerB, 2);
	pushed.redirectedAt = kT0;
	EXPECT_EQ(Classify(pushed, kT0), Candidate::kSkip);
}

// 选角界面的连接在转发 EnterGame 之后 kEnterGameSettle 内不迁:login 可能还在给它绑角色。
TEST_F(DrainMigration, RoleSelectWaitsForEnterGameToSettle)
{
	SessionInfo &session = AddRoleSelect(1, 1);
	EXPECT_EQ(Classify(session, kT0), Candidate::kRoleSelect);

	gate_drain_migration::NoteEnterGameForwarded(session, kT0);
	EXPECT_EQ(Classify(session, kT0 + kEnterGameSettle - seconds(1)), Candidate::kSkip);
	EXPECT_EQ(Classify(session, kT0 + kEnterGameSettle), Candidate::kRoleSelect);
}

// LeaveGame 之后会话还留着旧角色号,但不算绑定(login_admission.h HasBoundRole):按选角界面迁,票据不绑玩家。
TEST_F(DrainMigration, SessionAfterLeaveGameCountsAsRoleSelect)
{
	SessionInfo &session = AddInGame(1, kPlayerA, 1);
	session.sceneRouteResetPending = true;
	session.sceneId = 0;
	EXPECT_EQ(Classify(session, kT0), Candidate::kRoleSelect);
}

// ---- 许可与限速 ----

TEST_F(DrainMigration, NothingIsRequestedWithoutALease)
{
	AddInGame(1, kPlayerA, 1);
	Tick(kT0);
	EXPECT_FALSE(LeaseActive(kT0));
	EXPECT_TRUE(Login().requested.empty());
}

// 每拍至多 rate 个,在途至多 2 × rate;在途的会话不重复要票。
TEST_F(DrainMigration, RatePerTickAndInFlightCapBoundRequests)
{
	for (SessionId sessionId = 1; sessionId <= 5; ++sessionId)
	{
		AddInGame(sessionId, kPlayerA + sessionId, sessionId);
	}
	gate_drain_migration::OnLease(Lease(2), kT0);

	Tick(kT0);
	EXPECT_EQ(Login().requested.size(), 2u);
	Tick(kT0 + seconds(1));
	EXPECT_EQ(Login().requested.size(), 4u);
	Tick(kT0 + seconds(2)); // 在途已到 2 × rate
	EXPECT_EQ(Login().requested.size(), 4u);

	Answer(0, Reply(loginpb::GATE_MIGRATION_RESULT_OK), kT0 + seconds(2));
	Tick(kT0 + seconds(3)); // 回了一个,腾出一个在途名额
	EXPECT_EQ(Login().requested.size(), 5u);

	const std::set<SessionId> distinct(Login().requested.begin(), Login().requested.end());
	EXPECT_EQ(distinct.size(), 5u);
	EXPECT_EQ(GetCounters().requested, 5u);
}

// login 每拍续期;不续就在截止时停下。
TEST_F(DrainMigration, LeaseExpiresUnlessRenewed)
{
	gate_drain_migration::OnLease(Lease(1, 3000), kT0);
	gate_drain_migration::OnLease(Lease(1, 3000), kT0 + seconds(2)); // 续到 kT0 + 5s
	Tick(kT0 + seconds(4));
	EXPECT_TRUE(LeaseActive(kT0 + seconds(4)));

	Tick(kT0 + seconds(5));
	EXPECT_FALSE(LeaseActive(kT0 + seconds(5)));
	EXPECT_EQ(GetCounters().leases, 2u);
	EXPECT_EQ(GetCounters().leasesStopped, 1u);

	// 停下之后不再要票。
	AddInGame(1, kPlayerA, 1);
	Tick(kT0 + seconds(6));
	EXPECT_TRUE(Login().requested.empty());
}

// rate 或 lease 为 0 的许可立即停(迁移开关关了、或 login 显式叫停)。
TEST_F(DrainMigration, ZeroRateOrZeroDurationStopsImmediately)
{
	gate_drain_migration::OnLease(Lease(5), kT0);
	EXPECT_TRUE(LeaseActive(kT0));
	gate_drain_migration::OnLease(Lease(0), kT0 + seconds(1));
	EXPECT_FALSE(LeaseActive(kT0 + seconds(1)));

	gate_drain_migration::OnLease(Lease(5), kT0 + seconds(2));
	EXPECT_TRUE(LeaseActive(kT0 + seconds(2)));
	gate_drain_migration::OnLease(Lease(5, 0), kT0 + seconds(3));
	EXPECT_FALSE(LeaseActive(kT0 + seconds(3)));

	EXPECT_EQ(GetCounters().leases, 4u);
	EXPECT_EQ(GetCounters().leasesStopped, 2u);
}

// ---- 应答处置 ----

// 游戏中的票据绑定请求时的角色;应答字段原样进 124。推过的会话不再要票。
TEST_F(DrainMigration, OkReplyPushesATicketBoundToTheRequestedPlayer)
{
	AddInGame(1, kPlayerA, 1);
	gate_drain_migration::OnLease(Lease(10), kT0);
	Tick(kT0);
	ASSERT_EQ(Login().requested.size(), 1u);

	Answer(0, Reply(loginpb::GATE_MIGRATION_RESULT_OK), kT0 + milliseconds(20));

	ASSERT_EQ(Client().pushes.size(), 1u);
	const PushRecord &push = Client().pushes.front();
	EXPECT_EQ(push.sessionId, 1u);
	EXPECT_EQ(push.ticketPlayerId, kPlayerA);
	EXPECT_EQ(push.notify.target_ip(), "10.0.0.7");
	EXPECT_EQ(push.notify.target_port(), 7101u);
	EXPECT_EQ(push.notify.token_payload(), "payload");
	EXPECT_EQ(push.notify.token_signature(), "signature");
	EXPECT_EQ(push.notify.token_deadline(), 1793000300);
	EXPECT_EQ(GetCounters().offered, 1u);

	Tick(kT0 + seconds(1));
	EXPECT_EQ(Login().requested.size(), 1u);
}

// 选角界面的票据不绑玩家:新连接上 EnterGame 进哪个角色由玩家重新选。
TEST_F(DrainMigration, RoleSelectTicketIsNotBoundToAPlayer)
{
	AddRoleSelect(1, 1);
	gate_drain_migration::OnLease(Lease(10), kT0);
	Tick(kT0);
	Answer(0, Reply(loginpb::GATE_MIGRATION_RESULT_OK), kT0);

	ASSERT_EQ(Client().pushes.size(), 1u);
	EXPECT_EQ(Client().pushes.front().ticketPlayerId, 0u);
}

// login 不认为本 gate 在排空(标记已删、或本实例不在它的目录里):迟到的许可不能把人迁走。
TEST_F(DrainMigration, NotDrainingReplyStopsTheLease)
{
	ExpectReplyStopsLease(loginpb::GATE_MIGRATION_RESULT_NOT_DRAINING);
	EXPECT_EQ(GetCounters().notDraining, 1u);
}

TEST_F(DrainMigration, DisabledReplyStopsTheLease)
{
	ExpectReplyStopsLease(loginpb::GATE_MIGRATION_RESULT_DISABLED);
	EXPECT_EQ(GetCounters().disabled, 1u);
}

// 同 zone 没有合格目标:许可不停,这条会话从应答到达时起退避。
TEST_F(DrainMigration, NoTargetReplyBacksOffThatSession)
{
	AddInGame(1, kPlayerA, 1);
	gate_drain_migration::OnLease(Lease(1), kT0);
	Tick(kT0);
	const SteadyTime answeredAt = kT0 + milliseconds(50);
	Answer(0, Reply(loginpb::GATE_MIGRATION_RESULT_NO_TARGET), answeredAt);

	EXPECT_TRUE(LeaseActive(answeredAt));
	Tick(answeredAt + kRetryBackoff - seconds(1));
	EXPECT_EQ(Login().requested.size(), 1u);
	Tick(answeredAt + kRetryBackoff);
	EXPECT_EQ(Login().requested.size(), 2u);
	EXPECT_EQ(GetCounters().noTarget, 1u);
}

// 调用失败或应答解不出(结果未知):不推,按失败退避。
TEST_F(DrainMigration, UndeliveredReplyBacksOff)
{
	AddInGame(1, kPlayerA, 1);
	gate_drain_migration::OnLease(Lease(1), kT0);
	Tick(kT0);
	Answer(0, RequestReply{}, kT0);

	EXPECT_TRUE(Client().pushes.empty());
	EXPECT_EQ(GetCounters().failed, 1u);
	Tick(kT0 + kRetryBackoff - seconds(1));
	EXPECT_EQ(Login().requested.size(), 1u);
	Tick(kT0 + kRetryBackoff);
	EXPECT_EQ(Login().requested.size(), 2u);
}

// 出口没发出:回调不会来,在途状态当场收回,这条会话退避。
TEST_F(DrainMigration, RequestNotSentIsRevertedAndBackedOff)
{
	SessionInfo &session = AddInGame(1, kPlayerA, 1);
	Login().accept = false;
	gate_drain_migration::OnLease(Lease(10), kT0);
	Tick(kT0);

	EXPECT_EQ(GetCounters().notSent, 1u);
	EXPECT_EQ(GetCounters().requested, 0u);
	EXPECT_FALSE(session.drainMigrationInFlight);

	Login().accept = true;
	Tick(kT0 + seconds(1));
	EXPECT_TRUE(Login().requested.empty());
	Tick(kT0 + kRetryBackoff);
	EXPECT_EQ(Login().requested.size(), 1u);
}

// 应答到达时 session_id 已被新连接复用(代次不同):票不属于眼前这条连接,丢弃;在途名额照常收回。
TEST_F(DrainMigration, ReplyForAReplacedSessionIsDiscarded)
{
	AddInGame(1, kPlayerA, 1);
	gate_drain_migration::OnLease(Lease(10), kT0);
	Tick(kT0);

	tlsSessionManager.sessions().erase(1);
	AddInGame(1, kPlayerB, 2);
	Answer(0, Reply(loginpb::GATE_MIGRATION_RESULT_OK), kT0);

	EXPECT_TRUE(Client().pushes.empty());
	EXPECT_EQ(GetCounters().staleReply, 1u);
	Tick(kT0 + seconds(1));
	EXPECT_EQ(Login().requested.size(), 2u);
}

// 应答在路上时会话状态变了(开始切场景、换了角色):不推这张票,下一拍按新状态重新判。
TEST_F(DrainMigration, TicketIsDroppedWhenTheSessionChangedInFlight)
{
	SessionInfo &enteringScene = AddInGame(1, kPlayerA, 1);
	SessionInfo &switchedRole = AddInGame(2, kPlayerB, 2);
	gate_drain_migration::OnLease(Lease(10), kT0);
	Tick(kT0);
	ASSERT_EQ(Login().requested.size(), 2u);

	enteringScene.pendingSceneEntry = PendingSceneEntry{};
	switchedRole.playerId = kPlayerC;
	Answer(0, Reply(loginpb::GATE_MIGRATION_RESULT_OK), kT0);
	Answer(1, Reply(loginpb::GATE_MIGRATION_RESULT_OK), kT0);

	EXPECT_TRUE(Client().pushes.empty());
	EXPECT_EQ(GetCounters().staleReply, 2u);
	EXPECT_EQ(GetCounters().offered, 0u);
}

// 推送没到客户端(连接已释放、身份不符):票作废,这条会话退避,不在下一拍立刻再要。
TEST_F(DrainMigration, PushThatDoesNotReachTheClientBacksOff)
{
	AddInGame(1, kPlayerA, 1);
	Client().outcome = PushOutcome::kConnectionGone;
	gate_drain_migration::OnLease(Lease(10), kT0);
	Tick(kT0);
	Answer(0, Reply(loginpb::GATE_MIGRATION_RESULT_OK), kT0);

	EXPECT_EQ(Client().pushes.size(), 1u);
	EXPECT_EQ(GetCounters().pushDropped, 1u);
	EXPECT_EQ(GetCounters().offered, 0u);
	Tick(kT0 + seconds(1));
	EXPECT_EQ(Login().requested.size(), 1u);
	Tick(kT0 + kRetryBackoff);
	EXPECT_EQ(Login().requested.size(), 2u);
}

// 同一会话只记第一次推送:滞留关闭从第一次推 124 起算。
TEST_F(DrainMigration, NoteRedirectedKeepsTheFirstPushTime)
{
	SessionInfo &session = AddInGame(1, kPlayerA, 1);
	gate_drain_migration::NoteRedirected(1, session, kT0);
	gate_drain_migration::NoteRedirected(1, session, kT0 + seconds(10));
	ASSERT_TRUE(session.redirectedAt.has_value());
	EXPECT_EQ(*session.redirectedAt, kT0);
}

// 登记过滞留关闭、到期时会话已不在:直接丢弃登记。
TEST_F(DrainMigration, LingerEntryOfAClosedSessionIsDropped)
{
	SessionInfo &session = AddInGame(1, kPlayerA, 1);
	gate_drain_migration::NoteRedirected(1, session, kT0);
	tlsSessionManager.sessions().erase(1);

	Tick(kT0 + kRedirectLinger);
	EXPECT_EQ(GetCounters().lingerClosed, 0u);
}

// ---- 滞留关闭:要一条真实连接 ----

namespace
{
	// 回环连接与循环驱动取自 gate 单测共用的夹具(loopback_connection.h)。
	using gate_test::DriveLoopUntil;
	using gate_test::LoopbackConnection;

	class DrainMigrationLinger : public testing::Test
	{
	protected:
		void SetUp() override
		{
			loop_ = std::make_unique<muduo::net::EventLoop>();
			ResetState();
		}

		void TearDown() override
		{
			ResetState();
			connections_.clear(); // 拆连接要驱动 loop,必须先于 loop_ 析构
			loop_.reset();
		}

		// 建一条回环连接,挂成一个已推过 124 的游戏中会话(推送时刻 kT0)。
		LoopbackConnection &AddRedirectedSession(SessionId sessionId, uint64_t incarnation)
		{
			auto connection = std::make_unique<LoopbackConnection>(*loop_);
			EXPECT_TRUE(connection->Open("drain_linger_" + std::to_string(sessionId)));
			SessionInfo &session = AddInGame(sessionId, kPlayerA, incarnation);
			session.conn = connection->ServerSide();
			gate_drain_migration::NoteRedirected(sessionId, session, kT0);
			connections_.push_back(std::move(connection));
			return *connections_.back();
		}

		muduo::net::EventLoop &Loop() { return *loop_; }

	private:
		std::unique_ptr<muduo::net::EventLoop> loop_;
		std::vector<std::unique_ptr<LoopbackConnection>> connections_;
	};
} // namespace

// 推过 124 满 kRedirectLinger 仍连着:关掉,之后照常走断开回调(断线保留)。
TEST_F(DrainMigrationLinger, RedirectedConnectionIsClosedAfterTheLinger)
{
	LoopbackConnection &connection = AddRedirectedSession(1, 1);

	Tick(kT0 + kRedirectLinger - seconds(1));
	EXPECT_EQ(GetCounters().lingerClosed, 0u);
	EXPECT_TRUE(connection.ServerSideConnected());

	Tick(kT0 + kRedirectLinger);
	EXPECT_EQ(GetCounters().lingerClosed, 1u);
	EXPECT_TRUE(DriveLoopUntil(Loop(), [&connection] { return connection.BothSidesDown(); }));
}

// 到期时 session_id 已被新连接复用(代次不同):不关新连接。
TEST_F(DrainMigrationLinger, ReusedSessionIdIsNotClosed)
{
	LoopbackConnection &connection = AddRedirectedSession(1, 1);
	tlsSessionManager.sessions().at(1).incarnation = 2;

	Tick(kT0 + kRedirectLinger);
	EXPECT_EQ(GetCounters().lingerClosed, 0u);
	EXPECT_TRUE(connection.ServerSideConnected());
}
