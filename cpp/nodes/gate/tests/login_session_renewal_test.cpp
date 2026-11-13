// gate 登录态续租调度的单测(handler/rpc/login_session_renewal.h,docs/design/gate-online-identity.md §10.4)。
//
// 不进 gate.vcxproj,登记在 cpp/tests/routing_identity_test/routing_identity_test.vcxproj,被测源码直接编进测试。
// 发请求的出口注入替身:记下请求、把回调攒起来,由用例回放成各种应答。时刻全部由用例给定,被测代码不读时钟。
// 计数是进程累计的 thread_local,SetUp 里 ResetForTesting 清零。
//
// 没覆盖的一层:真实出口(client_session_handler.cpp 的 SendLoginSessionRenewRequest)、两处登录成功的调用点
// (client_request_lane.cpp、rpc_reply_bridge.cpp)与 main.cpp 的定时器。它们拖着节点注册表与 gRPC 通道,进不了这个工程;
// 靠核对调用点与 robot 冒烟。login 一侧的续期语义在 go/login 的 sessionfence / loginsession / clientplayerlogin 单测里。

#include <gtest/gtest.h>

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <utility>
#include <vector>

#include "handler/rpc/login_session_renewal.h"
#include "session/manager/session_manager.h"

namespace
{
	using gate_login_renewal::GetCounters;
	using gate_login_renewal::InFlight;
	using gate_login_renewal::kMaxInFlight;
	using gate_login_renewal::kMaxInterval;
	using gate_login_renewal::kMaxRequestsPerTick;
	using gate_login_renewal::kMinInterval;
	using gate_login_renewal::kRetryBackoff;
	using gate_login_renewal::NextInterval;
	using gate_login_renewal::NoteLoginSucceeded;
	using gate_login_renewal::PendingRenewals;
	using gate_login_renewal::ReplyCallback;
	using gate_login_renewal::RequestReply;
	using gate_login_renewal::SteadyTime;
	using gate_login_renewal::Tick;
	using std::chrono::milliseconds;
	using std::chrono::seconds;

	// 单调时钟上的任意起点。被测代码只比较时刻先后。
	constexpr SteadyTime kT0 = SteadyTime{} + std::chrono::hours(1);
	// login 按 30 分钟的登录态寿命给出的间隔。
	constexpr uint32_t kRenewAfterSeconds = 600;

	// 出口替身:accept = false 模拟「没发出」(没有路由服 / login 实例、缺通道)。发出的请求按顺序记下,回调攒着等用例回放。
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

	// 一条已建立的连接(还没登录成功)。
	SessionInfo &AddSession(SessionId sessionId, uint64_t incarnation)
	{
		SessionInfo &session = tlsSessionManager.sessions()[sessionId];
		session.verified = true;
		session.incarnation = incarnation;
		return session;
	}

	// 登录成功的连接:与两处真实调用点同一个动作(置 loginSucceeded,再通知续租模块)。
	SessionInfo &AddLoggedIn(SessionId sessionId, uint64_t incarnation, SteadyTime now)
	{
		SessionInfo &session = AddSession(sessionId, incarnation);
		session.loginSucceeded = true;
		NoteLoginSucceeded(sessionId, session, now);
		return session;
	}

	RequestReply Renewed(uint32_t renewAfterSeconds = kRenewAfterSeconds)
	{
		RequestReply reply;
		reply.delivered = true;
		reply.response.set_renewed(true);
		reply.response.set_renew_after_seconds(renewAfterSeconds);
		return reply;
	}

	// login 回「没有可续的记录」。
	RequestReply Gone()
	{
		RequestReply reply;
		reply.delivered = true;
		return reply;
	}

	// 调用失败或应答不可解析。
	RequestReply Failed()
	{
		return RequestReply{};
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
		gate_login_renewal::ResetForTesting();
		Login() = FakeLogin{};
	}

	class LoginSessionRenewal : public testing::Test
	{
	protected:
		void SetUp() override
		{
			ResetState();
			gate_login_renewal::SetRequestSender(&FakeSend);
		}

		void TearDown() override
		{
			gate_login_renewal::SetRequestSender(nullptr);
			ResetState();
		}
	};
} // namespace

// ---- 什么时候开始续租 ----

TEST_F(LoginSessionRenewal, FirstRenewalGoesOutOnTheTickAfterLoginSucceeds)
{
	// 第一次续租立刻到期:它同时向 login 取间隔,gate 不需要知道登录态的寿命。
	SessionInfo &session = AddLoggedIn(1, 11, kT0);
	EXPECT_EQ(session.loginRenewal, LoginRenewalState::kQueued);
	EXPECT_TRUE(Login().requested.empty()) << "登录成功的调用点只排队,不在回包路径上发请求";

	Tick(kT0);

	ASSERT_EQ(Login().requested.size(), 1u);
	EXPECT_EQ(Login().requested[0], 1u);
	EXPECT_EQ(session.loginRenewal, LoginRenewalState::kInFlight);
	EXPECT_EQ(InFlight(), 1u);
	EXPECT_EQ(GetCounters().scheduled, 1u);
	EXPECT_EQ(GetCounters().requested, 1u);
}

TEST_F(LoginSessionRenewal, ConnectionsThatNeverLoggedInAreNeverRenewed)
{
	AddSession(1, 11);

	Tick(kT0);
	Tick(kT0 + std::chrono::hours(2));

	EXPECT_TRUE(Login().requested.empty());
	EXPECT_EQ(PendingRenewals(), 0u);
}

// ---- 应答的处置 ----

TEST_F(LoginSessionRenewal, RenewedReplySchedulesTheNextRenewalAtTheIntervalLoginGave)
{
	SessionInfo &session = AddLoggedIn(1, 11, kT0);
	Tick(kT0);
	const SteadyTime repliedAt = kT0 + milliseconds(20);

	Answer(0, Renewed(), repliedAt);

	EXPECT_EQ(session.loginRenewal, LoginRenewalState::kQueued);
	EXPECT_EQ(InFlight(), 0u);
	EXPECT_EQ(GetCounters().renewed, 1u);
	const seconds interval = NextInterval(kRenewAfterSeconds, 1);
	Tick(repliedAt + interval - seconds(1));
	EXPECT_EQ(Login().requested.size(), 1u) << "没到间隔不续";
	Tick(repliedAt + interval);
	EXPECT_EQ(Login().requested.size(), 2u) << "到了间隔再续一次";
}

TEST_F(LoginSessionRenewal, AConnectionThatStaysOnlineKeepsBeingRenewed)
{
	// 回归场景:一直在线的连接。每个周期都续,过了登录态最初的寿命(三个间隔)仍在续。
	AddLoggedIn(1, 11, kT0);
	SteadyTime now = kT0;
	for (std::size_t round = 0; round < 6; ++round)
	{
		Tick(now);
		ASSERT_EQ(Login().requested.size(), round + 1);
		Answer(round, Renewed(), now);
		now += NextInterval(kRenewAfterSeconds, 1);
	}
	EXPECT_EQ(GetCounters().renewed, 6u);
	EXPECT_EQ(GetCounters().scheduled, 1u) << "同一次登录只排一条续租链";
}

TEST_F(LoginSessionRenewal, GoneReplyStopsRenewingUntilTheConnectionLogsInAgain)
{
	// 离开游戏之后 login 删了登录态、解除了绑定:没有可续的,停下。同一条连接再次登录成功后重新开始。
	SessionInfo &session = AddLoggedIn(1, 11, kT0);
	Tick(kT0);

	Answer(0, Gone(), kT0 + milliseconds(20));

	EXPECT_EQ(session.loginRenewal, LoginRenewalState::kIdle);
	EXPECT_EQ(GetCounters().gone, 1u);
	Tick(kT0 + std::chrono::hours(2));
	EXPECT_EQ(Login().requested.size(), 1u) << "停下之后不再续";
	EXPECT_EQ(PendingRenewals(), 0u);

	NoteLoginSucceeded(1, session, kT0 + std::chrono::hours(3));
	Tick(kT0 + std::chrono::hours(3));

	EXPECT_EQ(Login().requested.size(), 2u) << "再次登录成功后重新开始续租";
	EXPECT_EQ(GetCounters().scheduled, 2u);
}

TEST_F(LoginSessionRenewal, GoneReplyAfterAReloginInFlightAsksAgainInsteadOfStopping)
{
	// 续租在途时:LeaveGame → 重新 Login 成功。在途那次可能是在新的登录之前处理的,回「没有可续的记录」,
	// 而它和 Login 回包到达 gate 的先后没有保证。停下的话,这条刚登录的连接就再也不续了。
	SessionInfo &session = AddLoggedIn(1, 11, kT0);
	Tick(kT0);
	NoteLoginSucceeded(1, session, kT0 + milliseconds(10));
	EXPECT_EQ(session.loginRenewal, LoginRenewalState::kInFlightRelogged);

	const SteadyTime repliedAt = kT0 + milliseconds(20);
	Answer(0, Gone(), repliedAt);

	EXPECT_EQ(session.loginRenewal, LoginRenewalState::kQueued);
	EXPECT_EQ(GetCounters().rearmed, 1u);
	EXPECT_EQ(GetCounters().gone, 0u);
	Tick(repliedAt);
	ASSERT_EQ(Login().requested.size(), 2u) << "马上再问一次";

	// 第二次问到的是新登录之后的状态。若仍然没有记录(这次登录之后又离开了),才真的停下。
	Answer(1, Gone(), repliedAt + milliseconds(20));
	EXPECT_EQ(session.loginRenewal, LoginRenewalState::kIdle);
	EXPECT_EQ(GetCounters().gone, 1u);
}

TEST_F(LoginSessionRenewal, LoginSuccessWhileQueuedDoesNotAddASecondRenewalChain)
{
	// 同一条连接上重复登录成功(切换账号)时,已经排着的那一次问到的就是新状态,不能再排一条,否则续租频率翻倍。
	SessionInfo &session = AddLoggedIn(1, 11, kT0);
	NoteLoginSucceeded(1, session, kT0 + milliseconds(5));
	NoteLoginSucceeded(1, session, kT0 + milliseconds(6));

	Tick(kT0 + seconds(1));

	EXPECT_EQ(Login().requested.size(), 1u);
	EXPECT_EQ(PendingRenewals(), 0u);
	EXPECT_EQ(GetCounters().scheduled, 1u);
}

TEST_F(LoginSessionRenewal, FailedCallRetriesAfterTheBackoff)
{
	SessionInfo &session = AddLoggedIn(1, 11, kT0);
	Tick(kT0);
	const SteadyTime repliedAt = kT0 + milliseconds(20);

	Answer(0, Failed(), repliedAt);

	EXPECT_EQ(session.loginRenewal, LoginRenewalState::kQueued) << "结果未知:不停下,退避之后重试";
	EXPECT_EQ(GetCounters().failed, 1u);
	Tick(repliedAt + kRetryBackoff - seconds(1));
	EXPECT_EQ(Login().requested.size(), 1u);
	Tick(repliedAt + kRetryBackoff);
	EXPECT_EQ(Login().requested.size(), 2u);
}

TEST_F(LoginSessionRenewal, UnsentRequestRetriesAfterTheBackoff)
{
	SessionInfo &session = AddLoggedIn(1, 11, kT0);
	Login().accept = false;

	Tick(kT0);

	EXPECT_EQ(session.loginRenewal, LoginRenewalState::kQueued);
	EXPECT_EQ(InFlight(), 0u) << "没发出的请求不占在途";
	EXPECT_EQ(GetCounters().notSent, 1u);
	EXPECT_EQ(GetCounters().requested, 0u);

	Login().accept = true;
	Tick(kT0 + kRetryBackoff - seconds(1));
	EXPECT_TRUE(Login().requested.empty());
	Tick(kT0 + kRetryBackoff);
	EXPECT_EQ(Login().requested.size(), 1u);
}

// ---- 连接断开与会话号复用 ----

TEST_F(LoginSessionRenewal, QueuedRenewalForAClosedConnectionIsDropped)
{
	AddLoggedIn(1, 11, kT0);
	tlsSessionManager.sessions().erase(1);

	Tick(kT0);

	EXPECT_TRUE(Login().requested.empty());
	EXPECT_EQ(GetCounters().dropped, 1u);
	EXPECT_EQ(PendingRenewals(), 0u);
}

TEST_F(LoginSessionRenewal, ReplyForAClosedConnectionIsIgnored)
{
	AddLoggedIn(1, 11, kT0);
	Tick(kT0);
	tlsSessionManager.sessions().erase(1);

	Answer(0, Renewed(), kT0 + milliseconds(20));

	EXPECT_EQ(GetCounters().staleReply, 1u);
	EXPECT_EQ(InFlight(), 0u) << "在途计数照样收回";
	EXPECT_EQ(PendingRenewals(), 0u) << "不给已断开的连接排下一次";
}

TEST_F(LoginSessionRenewal, ReusedSessionIdDoesNotInheritTheOldConnectionsRenewal)
{
	// 会话号被新连接复用(代次不同):旧连接排着的续租与迟到的应答都不作用到新连接上,新连接没登录就不续。
	AddLoggedIn(1, 11, kT0);
	Tick(kT0);
	tlsSessionManager.sessions().erase(1);
	SessionInfo &reused = AddSession(1, 12);

	Answer(0, Renewed(), kT0 + milliseconds(20));
	Tick(kT0 + std::chrono::hours(1));

	EXPECT_EQ(reused.loginRenewal, LoginRenewalState::kIdle);
	EXPECT_EQ(Login().requested.size(), 1u);
	EXPECT_EQ(GetCounters().staleReply, 1u);
}

// ---- 限速 ----

TEST_F(LoginSessionRenewal, EachTickSendsAtMostTheBudgetAndCatchesUpOnLaterTicks)
{
	// login 故障恢复后所有连接同时到期:每拍至多 kMaxRequestsPerTick 个,其余留到后面几拍,不在一拍里打出去。
	const uint32_t total = kMaxRequestsPerTick + 50;
	for (uint32_t i = 1; i <= total; ++i)
	{
		AddLoggedIn(i, 1000 + i, kT0);
	}

	Tick(kT0);
	EXPECT_EQ(Login().requested.size(), kMaxRequestsPerTick);
	EXPECT_EQ(PendingRenewals(), 50u);

	Tick(kT0 + seconds(1));
	EXPECT_EQ(Login().requested.size(), total);
	EXPECT_EQ(PendingRenewals(), 0u);
}

TEST_F(LoginSessionRenewal, StopsSendingWhileTooManyRequestsAreInFlight)
{
	// 应答迟迟不回(login 慢、路由服慢)时不再加压,等在途的回来。
	const uint32_t total = static_cast<uint32_t>(kMaxInFlight) + 10;
	for (uint32_t i = 1; i <= total; ++i)
	{
		AddLoggedIn(i, 1000 + i, kT0);
	}
	Tick(kT0);
	Tick(kT0 + seconds(1));
	ASSERT_EQ(InFlight(), kMaxInFlight);

	Tick(kT0 + seconds(2));
	EXPECT_EQ(Login().requested.size(), kMaxInFlight) << "在途到上限之后这一拍不再发";

	Answer(0, Renewed(), kT0 + seconds(2));
	Tick(kT0 + seconds(3));
	EXPECT_EQ(Login().requested.size(), kMaxInFlight + 1) << "回来一个,放一个";
}

TEST_F(LoginSessionRenewal, NothingIsSentWithoutASender)
{
	gate_login_renewal::SetRequestSender(nullptr);
	SessionInfo &session = AddLoggedIn(1, 11, kT0);

	Tick(kT0);

	EXPECT_EQ(session.loginRenewal, LoginRenewalState::kQueued) << "出口没装上时登记留着,装上之后照常发";
	EXPECT_EQ(PendingRenewals(), 1u);
}

// ---- 间隔 ----

TEST(LoginSessionRenewalInterval, IsClampedAndSpreadBySessionId)
{
	// 正常值:不超过 login 给的间隔,至多提前五分之一;同一会话号每次相同,不同会话号摊开。
	for (SessionId sessionId = 0; sessionId < 500; ++sessionId)
	{
		const seconds interval = NextInterval(kRenewAfterSeconds, sessionId);
		EXPECT_LE(interval, seconds(600)) << "session_id=" << sessionId;
		EXPECT_GE(interval, seconds(480)) << "session_id=" << sessionId;
		EXPECT_EQ(interval, NextInterval(kRenewAfterSeconds, sessionId));
	}
	EXPECT_NE(NextInterval(kRenewAfterSeconds, 1), NextInterval(kRenewAfterSeconds, 2));

	// 异常应答:0 或极小值夹到下限(不能让一条连接每拍都续),极大值夹到上限(不能大到等于不续)。
	EXPECT_LE(NextInterval(0, 0), kMinInterval);
	EXPECT_GE(NextInterval(0, 7), kMinInterval - seconds(1));
	EXPECT_LE(NextInterval(UINT32_MAX, 0), kMaxInterval);
	EXPECT_GE(NextInterval(UINT32_MAX, 12345), kMaxInterval - kMaxInterval / 5);
}
