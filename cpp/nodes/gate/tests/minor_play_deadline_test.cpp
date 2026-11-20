// gate 未成年人到点断开的单测(handler/rpc/minor_play_deadline.h,docs/design/real-name-anti-addiction.md §5.4)。
//
// 不进 gate.vcxproj,登记在 cpp/tests/routing_identity_test/routing_identity_test.vcxproj,被测源码直接编进测试。
// 关连接的出口注入替身,只记下被关的会话;时刻全部由用例给定,被测代码不读时钟。
//
// 没覆盖的一层:真实出口(client_session_handler.cpp 的 CloseMinorAtPlayDeadline:推踢线、shutdown、延迟强关)、
// 两处登录成功的调用点(client_request_lane.cpp、rpc_reply_bridge.cpp)与 main.cpp 的定时器,靠核对调用点与冒烟。
// 回包里截止时刻的解析在本文件末尾两条用例里测(login_admission.h SuccessfulLoginResponse)。

#include <gtest/gtest.h>

#include <cstdint>
#include <vector>

#include "handler/rpc/login_admission.h"
#include "handler/rpc/minor_play_deadline.h"
#include "proto/common/base/message.pb.h"
#include "proto/login/login.pb.h"
#include "session/manager/session_manager.h"
#include "table/proto/tip/login_error_tip.pb.h"

namespace
{
	using gate_minor_deadline::GetCounters;
	using gate_minor_deadline::kCloseLeadMs;
	using gate_minor_deadline::NoteLoginSucceeded;
	using gate_minor_deadline::Pending;
	using gate_minor_deadline::Tick;

	// 某天 21:00:00 的 Unix 毫秒(数值本身无所谓,被测代码只比较先后)。
	constexpr uint64_t kDeadlineMs = 1'763'989'200'000ull;
	constexpr uint64_t kCloseAtMs = kDeadlineMs - kCloseLeadMs;

	// 出口替身:记下被关的会话的代次(代次在 gate 进程内不复用,比会话号更能认出是哪条连接)。
	std::vector<uint64_t> &Closed()
	{
		static std::vector<uint64_t> closed;
		return closed;
	}

	void FakeClose(SessionInfo &session)
	{
		Closed().push_back(session.incarnation);
	}

	SessionInfo &AddSession(SessionId sessionId, uint64_t incarnation)
	{
		SessionInfo &session = tlsSessionManager.sessions()[sessionId];
		session.verified = true;
		session.incarnation = incarnation;
		return session;
	}

	class MinorPlayDeadlineTest : public ::testing::Test
	{
	protected:
		void SetUp() override
		{
			tlsSessionManager.sessions().clear();
			gate_minor_deadline::ResetForTesting();
			gate_minor_deadline::SetCloser(&FakeClose);
			Closed().clear();
		}

		void TearDown() override
		{
			tlsSessionManager.sessions().clear();
			gate_minor_deadline::ResetForTesting();
		}
	};

	// 提前 kCloseLeadMs 关,而且只关一次:21:00:00 之前连接已经断开。
	TEST_F(MinorPlayDeadlineTest, ClosesOnceShortlyBeforeTheDeadline)
	{
		SessionInfo &session = AddSession(7, 101);
		NoteLoginSucceeded(7, session, kDeadlineMs);
		EXPECT_EQ(GetCounters().armed, 1u);

		Tick(kCloseAtMs - 1);
		EXPECT_TRUE(Closed().empty()) << "not due yet";

		Tick(kCloseAtMs);
		ASSERT_EQ(Closed().size(), 1u);
		EXPECT_EQ(Closed()[0], 101u);
		EXPECT_EQ(GetCounters().closed, 1u);

		Tick(kDeadlineMs + 60'000);
		EXPECT_EQ(Closed().size(), 1u) << "a connection is closed once";
		EXPECT_EQ(Pending(), 0u);
	}

	// 成年人的登录回包不带截止时刻:什么都不登记。
	TEST_F(MinorPlayDeadlineTest, AdultsAreNeverClosed)
	{
		SessionInfo &session = AddSession(7, 101);
		NoteLoginSucceeded(7, session, 0);
		EXPECT_EQ(Pending(), 0u);
		Tick(kDeadlineMs + 60'000);
		EXPECT_TRUE(Closed().empty());
	}

	// 一拍里到期多少条就关多少条,不受每拍预算限制(原先挂在续租上,每秒只处理 200 条)。
	TEST_F(MinorPlayDeadlineTest, ClosesEveryDueConnectionInOneTick)
	{
		constexpr uint32_t kMinors = 1000;
		for (uint32_t i = 0; i < kMinors; ++i)
		{
			NoteLoginSucceeded(i + 1, AddSession(i + 1, 1000 + i), kDeadlineMs);
		}
		Tick(kCloseAtMs);
		EXPECT_EQ(Closed().size(), kMinors);
	}

	// 连接已经断开,或会话号被新连接复用:到期登记对不上,丢弃,不误关新连接。
	TEST_F(MinorPlayDeadlineTest, StaleEntriesDoNotCloseAReusedSessionId)
	{
		NoteLoginSucceeded(7, AddSession(7, 101), kDeadlineMs);
		tlsSessionManager.sessions().erase(7);
		NoteLoginSucceeded(8, AddSession(8, 102), kDeadlineMs);
		tlsSessionManager.sessions().erase(8);
		AddSession(8, 203); // 同一个会话号,另一条连接,没有截止时刻

		Tick(kCloseAtMs);
		EXPECT_TRUE(Closed().empty());
		EXPECT_EQ(Pending(), 0u);
	}

	// 同一条连接改用成年人的账号登录:以最近一次成功的登录为准,截止时刻清掉。
	TEST_F(MinorPlayDeadlineTest, ALaterAdultLoginClearsTheDeadline)
	{
		SessionInfo &session = AddSession(7, 101);
		NoteLoginSucceeded(7, session, kDeadlineMs);
		NoteLoginSucceeded(7, session, 0);
		Tick(kCloseAtMs);
		EXPECT_TRUE(Closed().empty());
	}

	// 同一条连接重复登录(同一个未成年人):留下两条登记,只关一次。
	TEST_F(MinorPlayDeadlineTest, RepeatedLoginsCloseOnce)
	{
		SessionInfo &session = AddSession(7, 101);
		NoteLoginSucceeded(7, session, kDeadlineMs);
		NoteLoginSucceeded(7, session, kDeadlineMs);
		Tick(kCloseAtMs);
		EXPECT_EQ(Closed().size(), 1u);
	}

	// 赶在截止前几秒才登录进来:下一拍就关。
	TEST_F(MinorPlayDeadlineTest, LoginInsideTheLeadIsClosedOnTheNextTick)
	{
		NoteLoginSucceeded(7, AddSession(7, 101), kDeadlineMs);
		Tick(kDeadlineMs - 1000);
		EXPECT_EQ(Closed().size(), 1u);
	}

	loginpb::LoginResponse MinorLoginResponse()
	{
		loginpb::LoginResponse response;
		response.mutable_anti_addiction()->set_minor(true);
		response.mutable_anti_addiction()->set_play_deadline_ms(kDeadlineMs);
		return response;
	}

	// 两种转发模式的成功回包都能读出截止时刻:路由模式是信封里的 body,直连模式就是 LoginResponse。
	TEST(MinorPlayDeadlineReplyTest, SuccessfulLoginReplyCarriesTheDeadlineInBothModes)
	{
		const auto direct = gate_login_admission::SuccessfulLoginResponse(MinorLoginResponse());
		ASSERT_TRUE(direct.has_value());
		EXPECT_EQ(direct->anti_addiction().play_deadline_ms(), kDeadlineMs);

		MessageContent routed;
		routed.set_serialized_message(MinorLoginResponse().SerializeAsString());
		const auto viaRouter = gate_login_admission::SuccessfulLoginResponse(routed);
		ASSERT_TRUE(viaRouter.has_value());
		EXPECT_EQ(viaRouter->anti_addiction().play_deadline_ms(), kDeadlineMs);

		const auto adult = gate_login_admission::SuccessfulLoginResponse(loginpb::LoginResponse{});
		ASSERT_TRUE(adult.has_value());
		EXPECT_EQ(adult->anti_addiction().play_deadline_ms(), 0u);
	}

	// 被拒的登录(含防沉迷拒绝)不算成功:不置 loginSucceeded,也不登记截止时刻。
	TEST(MinorPlayDeadlineReplyTest, RejectedLoginReplyIsNotSuccessful)
	{
		loginpb::LoginResponse rejected = MinorLoginResponse();
		rejected.mutable_error_message()->set_id(kLoginMinorNotInPlayTime);
		EXPECT_FALSE(gate_login_admission::SuccessfulLoginResponse(rejected).has_value());

		MessageContent routed;
		routed.set_serialized_message(rejected.SerializeAsString());
		EXPECT_FALSE(gate_login_admission::SuccessfulLoginResponse(routed).has_value());

		MessageContent refusedByRouter;
		refusedByRouter.mutable_error_message()->set_id(kLoginSessionNotFound);
		EXPECT_FALSE(gate_login_admission::SuccessfulLoginResponse(refusedByRouter).has_value());
	}
} // namespace
