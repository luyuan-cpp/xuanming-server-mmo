// gate 客户端连接存活判定的纯逻辑单测(services/gate/session/comp/client_liveness.h,docs/design/gate-client-heartbeat.md)。
//
// 不进 gate.vcxproj,登记在 cpp/tests/routing_identity_test/routing_identity_test.vcxproj,与同工程其余用例共用 main。
// 被测头文件只依赖 STL:时刻全部是合成的,不读时钟、不等待;会话表用一个 std::map 假冒,两种关连接的回调只记录。

#include <gtest/gtest.h>

#include <chrono>
#include <cstdint>
#include <map>
#include <optional>
#include <utility>
#include <vector>

#include "session/comp/client_liveness.h"

namespace
{
	using gate_liveness::AdmitHeartbeat;
	using gate_liveness::ClientLiveness;
	using gate_liveness::ClientLivenessPolicy;
	using gate_liveness::HeartbeatAction;
	using gate_liveness::IdleVerdict;
	using gate_liveness::IsValidLivenessPolicy;
	using gate_liveness::JudgeIdle;
	using gate_liveness::kClientLiveness;
	using gate_liveness::LivenessSweeper;
	using gate_liveness::SteadyTime;
	using std::chrono::milliseconds;
	using std::chrono::nanoseconds;
	using std::chrono::seconds;

	constexpr milliseconds kHandshake = kClientLiveness.handshakeTimeout;
	constexpr milliseconds kInterval = kClientLiveness.interval;
	constexpr milliseconds kTimeout = kClientLiveness.idleTimeout;
	constexpr milliseconds kPeriod = kClientLiveness.checkPeriod;

	// 合成时刻的基点离 steady_clock 纪元足够远,减法不会落到纪元之前。
	SteadyTime At(const SteadyTime::duration offset)
	{
		return SteadyTime{} + std::chrono::hours(1) + offset;
	}

	HeartbeatAction Admit(ClientLiveness &liveness, const SteadyTime now)
	{
		return AdmitHeartbeat(liveness, /*verified=*/true, now, kClientLiveness);
	}

	struct FakeSession
	{
		uint64_t incarnation{0};
		bool verified{false};
		ClientLiveness liveness;
	};

	// 假会话表与两种关连接的记录。
	struct FakeSessions
	{
		std::map<uint32_t, FakeSession> sessions;
		std::vector<uint32_t> handshakeTimedOut;
		std::vector<std::pair<uint32_t, LivenessSweeper::Duration>> idleClosed;

		FakeSession &Add(const uint32_t sessionId, const uint64_t incarnation, const bool verified)
		{
			auto &session = sessions[sessionId];
			session = FakeSession{.incarnation = incarnation, .verified = verified};
			return session;
		}

		LivenessSweeper::SweepResult Sweep(LivenessSweeper &sweeper, const SteadyTime now)
		{
			return sweeper.Sweep(
				now,
				[this](const uint32_t sessionId, const uint64_t incarnation) -> std::optional<LivenessSweeper::SessionView>
				{
					const auto it = sessions.find(sessionId);
					if (it == sessions.end() || it->second.incarnation != incarnation)
					{
						return std::nullopt;
					}
					return LivenessSweeper::SessionView{.verified = it->second.verified, .liveness = it->second.liveness};
				},
				[this](const uint32_t sessionId, uint64_t) { handshakeTimedOut.push_back(sessionId); },
				[this](const uint32_t sessionId, uint64_t, const LivenessSweeper::Duration silentFor)
				{ idleClosed.emplace_back(sessionId, silentFor); });
		}

		// 按 1 秒节拍从 from 扫到 to(两端都含),模拟准时的定时器。返回期间两种关连接的总条数。
		uint64_t SweepEachSecond(LivenessSweeper &sweeper, const SteadyTime from, const SteadyTime to)
		{
			uint64_t closed = 0;
			for (SteadyTime t = from; t <= to; t += kPeriod)
			{
				const auto result = Sweep(sweeper, t);
				EXPECT_FALSE(result.late) << "a one-second cadence must never count as late";
				closed += result.handshakeTimeouts + result.idleClosed;
			}
			return closed;
		}
	};

	// 与 gate 装配层同一顺序:验签成功的回包发出时开始跟踪,并登记第一个空闲检查点(OnTokenVerified)。
	void VerifyAndTrack(LivenessSweeper &sweeper, FakeSession &session, const uint32_t sessionId, const SteadyTime now)
	{
		session.verified = true;
		ASSERT_TRUE(session.liveness.StartTracking(now));
		sweeper.Track(sessionId, session.incarnation, now);
	}
} // namespace

// ---- 参数 ----

TEST(ClientLivenessPolicy, ShippedValuesMatchTheDesign)
{
	EXPECT_EQ(kClientLiveness.handshakeTimeout, seconds(10));
	EXPECT_EQ(kClientLiveness.interval, seconds(10));
	EXPECT_EQ(kClientLiveness.idleTimeout, seconds(30));
	EXPECT_EQ(kClientLiveness.checkPeriod, seconds(1));
	EXPECT_EQ(kClientLiveness.maxHeartbeatsPerInterval, 3u);
	EXPECT_TRUE(IsValidLivenessPolicy(kClientLiveness));
}

TEST(ClientLivenessPolicy, RejectsPoliciesThatCouldKillHealthyConnections)
{
	const ClientLivenessPolicy valid = kClientLiveness;

	ClientLivenessPolicy policy = valid;
	policy.interval = milliseconds(0);
	EXPECT_FALSE(IsValidLivenessPolicy(policy));

	policy = valid;
	policy.handshakeTimeout = milliseconds(0);
	EXPECT_FALSE(IsValidLivenessPolicy(policy));

	policy = valid;
	policy.handshakeTimeout = policy.checkPeriod;
	EXPECT_FALSE(IsValidLivenessPolicy(policy)) << "the handshake deadline must outlast one check period";

	policy = valid;
	policy.checkPeriod = milliseconds(0);
	EXPECT_FALSE(IsValidLivenessPolicy(policy));

	policy = valid;
	policy.checkPeriod = policy.interval + milliseconds(1);
	EXPECT_FALSE(IsValidLivenessPolicy(policy)) << "check period longer than the interval";

	policy = valid;
	policy.idleTimeout = 2 * policy.interval;
	EXPECT_FALSE(IsValidLivenessPolicy(policy)) << "must tolerate at least one lost heartbeat";
	policy.idleTimeout = 2 * policy.interval + milliseconds(1);
	EXPECT_TRUE(IsValidLivenessPolicy(policy));

	policy = valid;
	policy.maxHeartbeatsPerInterval = 1;
	EXPECT_FALSE(IsValidLivenessPolicy(policy)) << "a backlog of two heartbeats must still be answered";

	policy = valid;
	policy.idleTimeout = milliseconds(static_cast<int64_t>(UINT32_MAX) + 1);
	EXPECT_FALSE(IsValidLivenessPolicy(policy)) << "advertised milliseconds must fit uint32";
}

// ---- 跟踪与心跳准入 ----

TEST(ClientLivenessTracking, StartTrackingOnlyOnceAndOnlyTrackedConnectionsRecordInboundTime)
{
	ClientLiveness liveness;
	liveness.NoteInbound(At(seconds(1)));
	EXPECT_EQ(liveness.lastInboundAt, SteadyTime{}) << "untracked connections do not record time";

	EXPECT_TRUE(liveness.StartTracking(At(seconds(2))));
	EXPECT_TRUE(liveness.tracked);
	EXPECT_EQ(liveness.lastInboundAt, At(seconds(2)));
	EXPECT_FALSE(liveness.StartTracking(At(seconds(3)))) << "already tracked";
	EXPECT_EQ(liveness.lastInboundAt, At(seconds(2))) << "a repeated start must not move the silence clock";

	liveness.NoteInbound(At(seconds(7)));
	EXPECT_EQ(liveness.lastInboundAt, At(seconds(7)));
}

TEST(ClientLivenessAdmit, UnverifiedHeartbeatIsRejectedWithoutTouchingState)
{
	ClientLiveness liveness;
	EXPECT_EQ(AdmitHeartbeat(liveness, /*verified=*/false, At(seconds(0)), kClientLiveness),
			  HeartbeatAction::kRejectUnverified);
	EXPECT_FALSE(liveness.tracked);
	EXPECT_EQ(liveness.heartbeatsInWindow, 0u);
}

// 正常流程里验签成功时已开始跟踪,心跳只回答。
TEST(ClientLivenessAdmit, HeartbeatOnATrackedConnectionIsJustAnswered)
{
	ClientLiveness liveness;
	ASSERT_TRUE(liveness.StartTracking(At(seconds(0))));
	EXPECT_EQ(Admit(liveness, At(seconds(1))), HeartbeatAction::kAnswer);
	EXPECT_EQ(Admit(liveness, At(seconds(11))), HeartbeatAction::kAnswer);
}

// 没发验签请求就来了心跳(dev 免验签):第一条开始跟踪,只这一次。
TEST(ClientLivenessAdmit, FirstHeartbeatStartsTrackingWhenTheVerifyReplyDidNot)
{
	ClientLiveness liveness;
	EXPECT_EQ(Admit(liveness, At(seconds(5))), HeartbeatAction::kAnswerAndStartTracking);
	EXPECT_TRUE(liveness.tracked);
	EXPECT_EQ(liveness.lastInboundAt, At(seconds(5)));
	EXPECT_EQ(Admit(liveness, At(seconds(5) + kInterval)), HeartbeatAction::kAnswer);
}

TEST(ClientLivenessAdmit, FourthHeartbeatInOneWindowIsExcessUntilTheWindowEnds)
{
	ClientLiveness liveness;
	const SteadyTime windowStart = At(seconds(0));
	EXPECT_NE(Admit(liveness, windowStart), HeartbeatAction::kExcess);
	EXPECT_EQ(Admit(liveness, windowStart + milliseconds(1)), HeartbeatAction::kAnswer);
	EXPECT_EQ(Admit(liveness, windowStart + milliseconds(2)), HeartbeatAction::kAnswer);
	EXPECT_EQ(Admit(liveness, windowStart + milliseconds(3)), HeartbeatAction::kExcess);
	EXPECT_EQ(Admit(liveness, windowStart + kInterval - nanoseconds(1)), HeartbeatAction::kExcess);
	// 满 interval 换新窗口,计数从头算。
	EXPECT_EQ(Admit(liveness, windowStart + kInterval), HeartbeatAction::kAnswer);
	EXPECT_EQ(Admit(liveness, windowStart + kInterval + milliseconds(1)), HeartbeatAction::kAnswer);
	EXPECT_EQ(Admit(liveness, windowStart + kInterval + milliseconds(2)), HeartbeatAction::kAnswer);
	EXPECT_EQ(Admit(liveness, windowStart + kInterval + milliseconds(3)), HeartbeatAction::kExcess);
}

// 正常节奏与两倍速一条都不超;断网恢复后积压的 3 条同时到,全部应答。
TEST(ClientLivenessAdmit, NormalAndDoubleCadenceAndABacklogOfThreeAreNeverExcess)
{
	for (const milliseconds spacing : {kInterval, kInterval / 2})
	{
		ClientLiveness liveness;
		for (int i = 0; i < 60; ++i)
		{
			EXPECT_NE(Admit(liveness, At(i * spacing)), HeartbeatAction::kExcess)
				<< "spacing_ms=" << spacing.count() << " heartbeat=" << i;
		}
	}

	ClientLiveness liveness;
	ASSERT_NE(Admit(liveness, At(seconds(0))), HeartbeatAction::kExcess);
	const SteadyTime backlogArrives = At(seconds(25));
	EXPECT_EQ(Admit(liveness, backlogArrives), HeartbeatAction::kAnswer);
	EXPECT_EQ(Admit(liveness, backlogArrives), HeartbeatAction::kAnswer);
	EXPECT_EQ(Admit(liveness, backlogArrives), HeartbeatAction::kAnswer);
}

// ---- 判死 ----

TEST(ClientLivenessJudge, ExactlyTheTimeoutIsDeadAndOneNanosecondLessIsAlive)
{
	const SteadyTime last = At(seconds(0));
	EXPECT_EQ(JudgeIdle(last, last + kTimeout - nanoseconds(1), false, kTimeout), IdleVerdict::kAlive);
	EXPECT_EQ(JudgeIdle(last, last + kTimeout, false, kTimeout), IdleVerdict::kDead);
	EXPECT_EQ(JudgeIdle(last, last + 10 * kTimeout, false, kTimeout), IdleVerdict::kDead);
}

TEST(ClientLivenessJudge, LateCheckDefersInsteadOfKilling)
{
	const SteadyTime last = At(seconds(0));
	EXPECT_EQ(JudgeIdle(last, last + kTimeout, true, kTimeout), IdleVerdict::kDeferred);
	EXPECT_EQ(JudgeIdle(last, last + kTimeout - nanoseconds(1), true, kTimeout), IdleVerdict::kAlive);
}

// ---- 握手期限 ----

TEST(ClientLivenessSweeper, FirstSweepIsNeverLate)
{
	LivenessSweeper sweeper(kClientLiveness);
	FakeSessions fake;
	const auto result = fake.Sweep(sweeper, At(seconds(500)));
	EXPECT_FALSE(result.late);
	EXPECT_EQ(result.checkGap, LivenessSweeper::Duration::zero());
}

// 判据是距建连的时长:期间收到过字节也不续期,只有验签能让它作废。
TEST(ClientLivenessSweeper, UnverifiedConnectionIsClosedExactlyAtTheHandshakeDeadline)
{
	LivenessSweeper sweeper(kClientLiveness);
	FakeSessions fake;
	const SteadyTime connectedAt = At(seconds(0));
	fake.Add(4, 40, /*verified=*/false);
	sweeper.ExpectHandshake(4, 40, connectedAt);

	EXPECT_EQ(fake.SweepEachSecond(sweeper, connectedAt + kPeriod, connectedAt + kHandshake - kPeriod), 0u);
	EXPECT_TRUE(fake.handshakeTimedOut.empty());

	const auto result = fake.Sweep(sweeper, connectedAt + kHandshake);
	EXPECT_EQ(result.handshakeTimeouts, 1u);
	EXPECT_EQ(fake.handshakeTimedOut, (std::vector<uint32_t>{4}));
	EXPECT_EQ(sweeper.PendingChecks(), 0u) << "a timed-out handshake is not re-scheduled";

	EXPECT_EQ(fake.SweepEachSecond(sweeper, connectedAt + kHandshake + kPeriod, connectedAt + 3 * kHandshake), 0u);
	EXPECT_EQ(fake.handshakeTimedOut.size(), 1u);
}

TEST(ClientLivenessSweeper, VerifyingBeforeTheDeadlineVoidsItAndStartsTheIdleCheck)
{
	LivenessSweeper sweeper(kClientLiveness);
	FakeSessions fake;
	const SteadyTime connectedAt = At(seconds(0));
	FakeSession &session = fake.Add(4, 40, /*verified=*/false);
	sweeper.ExpectHandshake(4, 40, connectedAt);
	const SteadyTime verifiedAt = connectedAt + seconds(2);
	VerifyAndTrack(sweeper, session, 4, verifiedAt);

	// 握手检查点在建连 + 10s 到期时已验签:丢弃,不关。
	EXPECT_EQ(fake.SweepEachSecond(sweeper, connectedAt + kPeriod, connectedAt + kHandshake), 0u);
	EXPECT_TRUE(fake.handshakeTimedOut.empty());
	EXPECT_EQ(sweeper.PendingChecks(), 1u) << "only the idle checkpoint is left";

	// 验签之后一帧都没来(没发心跳):从验签时刻起满 30s 关。
	EXPECT_EQ(fake.SweepEachSecond(sweeper, connectedAt + kHandshake + kPeriod, verifiedAt + kTimeout - kPeriod), 0u);
	EXPECT_EQ(fake.SweepEachSecond(sweeper, verifiedAt + kTimeout, verifiedAt + kTimeout), 1u);
	ASSERT_EQ(fake.idleClosed.size(), 1u);
	EXPECT_EQ(fake.idleClosed[0].second, LivenessSweeper::Duration(kTimeout));
}

TEST(ClientLivenessSweeper, LateSweepDefersTheHandshakeDeadline)
{
	LivenessSweeper sweeper(kClientLiveness);
	FakeSessions fake;
	const SteadyTime connectedAt = At(seconds(0));
	FakeSession &session = fake.Add(4, 40, /*verified=*/false);
	sweeper.ExpectHandshake(4, 40, connectedAt);
	EXPECT_EQ(fake.SweepEachSecond(sweeper, connectedAt + kPeriod, connectedAt + kHandshake - kPeriod), 0u);

	// 本端卡了 4s:积压的验签请求可能还在接收缓冲里,这一轮不关。
	const SteadyTime stalledUntil = connectedAt + kHandshake + seconds(3);
	const auto late = fake.Sweep(sweeper, stalledUntil);
	EXPECT_TRUE(late.late);
	EXPECT_EQ(late.deferred, 1u);
	EXPECT_EQ(late.handshakeTimeouts, 0u);

	// 卡顿恢复后读到了验签请求:期限作废。
	session.verified = true;
	EXPECT_EQ(fake.Sweep(sweeper, stalledUntil + kPeriod).handshakeTimeouts, 0u);
	EXPECT_TRUE(fake.handshakeTimedOut.empty());
}

// ---- 空闲超时 ----

TEST(ClientLivenessSweeper, SilentTrackedConnectionIsClosedOnceAtTheTimeout)
{
	LivenessSweeper sweeper(kClientLiveness);
	FakeSessions fake;
	const SteadyTime start = At(seconds(0));
	VerifyAndTrack(sweeper, fake.Add(7, 70, /*verified=*/true), 7, start);
	EXPECT_EQ(sweeper.PendingChecks(), 1u);

	EXPECT_EQ(fake.SweepEachSecond(sweeper, start + kPeriod, start + kTimeout - kPeriod), 0u);
	EXPECT_TRUE(fake.idleClosed.empty());
	EXPECT_EQ(sweeper.PendingChecks(), 1u) << "the checkpoint is not due before the timeout";

	EXPECT_EQ(fake.SweepEachSecond(sweeper, start + kTimeout, start + kTimeout), 1u);
	ASSERT_EQ(fake.idleClosed.size(), 1u);
	EXPECT_EQ(fake.idleClosed[0].first, 7u);
	EXPECT_EQ(fake.idleClosed[0].second, LivenessSweeper::Duration(kTimeout));
	EXPECT_EQ(sweeper.PendingChecks(), 0u) << "a closed connection is not re-scheduled";

	// 断开回调要到下一轮才删会话;这期间的扫描不会再关它一次。
	EXPECT_EQ(fake.SweepEachSecond(sweeper, start + kTimeout + kPeriod, start + 3 * kTimeout), 0u);
	EXPECT_EQ(fake.idleClosed.size(), 1u);
}

TEST(ClientLivenessSweeper, AnyInboundFramePostponesTheVerdict)
{
	LivenessSweeper sweeper(kClientLiveness);
	FakeSessions fake;
	const SteadyTime start = At(seconds(0));
	FakeSession &session = fake.Add(7, 70, /*verified=*/true);
	VerifyAndTrack(sweeper, session, 7, start);

	EXPECT_EQ(fake.SweepEachSecond(sweeper, start + kPeriod, start + seconds(20)), 0u);
	session.liveness.NoteInbound(start + seconds(20) + milliseconds(500)); // 例如一条 ClientRequest
	// 原检查点在 start + 30s 到期:活着,按「上一帧 + 30s」重新登记。
	EXPECT_EQ(fake.SweepEachSecond(sweeper, start + seconds(21), start + seconds(50)), 0u);
	EXPECT_EQ(sweeper.PendingChecks(), 1u);
	// 上一帧之后满 30s 的第一个准时节拍才判死。
	EXPECT_EQ(fake.SweepEachSecond(sweeper, start + seconds(51), start + seconds(51)), 1u);
	ASSERT_EQ(fake.idleClosed.size(), 1u);
	EXPECT_EQ(fake.idleClosed[0].second, LivenessSweeper::Duration(seconds(30) + milliseconds(500)));
}

TEST(ClientLivenessSweeper, HeartbeatsEveryIntervalKeepTheConnectionAlive)
{
	LivenessSweeper sweeper(kClientLiveness);
	FakeSessions fake;
	const SteadyTime start = At(seconds(0));
	FakeSession &session = fake.Add(7, 70, /*verified=*/true);
	VerifyAndTrack(sweeper, session, 7, start);

	for (SteadyTime t = start + kPeriod; t <= start + 10 * kTimeout; t += kPeriod)
	{
		if ((t - start) % kInterval == SteadyTime::duration::zero())
		{
			session.liveness.NoteInbound(t);
			EXPECT_EQ(Admit(session.liveness, t), HeartbeatAction::kAnswer);
		}
		EXPECT_EQ(fake.Sweep(sweeper, t).idleClosed, 0u);
	}
	EXPECT_TRUE(fake.idleClosed.empty());
	EXPECT_EQ(sweeper.PendingChecks(), 1u) << "exactly one idle checkpoint per tracked connection";
}

// 会话已关闭、session_id 被新连接复用(代次不符)、空闲检查点对应的会话没在跟踪:都丢弃,不回调。
TEST(ClientLivenessSweeper, ClosedReusedOrUntrackedSessionsAreDroppedNotClosed)
{
	LivenessSweeper sweeper(kClientLiveness);
	FakeSessions fake;
	const SteadyTime start = At(seconds(0));

	VerifyAndTrack(sweeper, fake.Add(1, 10, /*verified=*/true), 1, start);
	fake.sessions.erase(1); // 断开回调删了会话

	VerifyAndTrack(sweeper, fake.Add(2, 20, /*verified=*/true), 2, start);
	fake.Add(2, 21, /*verified=*/false); // 同号给了一条新连接

	sweeper.Track(3, 30, start); // 检查点对应的会话没在跟踪
	fake.Add(3, 30, /*verified=*/true);

	EXPECT_EQ(fake.SweepEachSecond(sweeper, start + kPeriod, start + kTimeout - kPeriod), 0u);
	const auto result = fake.Sweep(sweeper, start + kTimeout);
	EXPECT_EQ(result.stale, 3u);
	EXPECT_EQ(result.idleClosed, 0u);
	EXPECT_EQ(result.handshakeTimeouts, 0u);
	EXPECT_TRUE(fake.idleClosed.empty());
	EXPECT_EQ(sweeper.PendingChecks(), 0u);
}

// 同号的新连接只按它自己的时刻判定,与旧连接留下的检查点无关。
TEST(ClientLivenessSweeper, ReusedSessionIdIsJudgedOnItsOwnSchedule)
{
	LivenessSweeper sweeper(kClientLiveness);
	FakeSessions fake;
	const SteadyTime start = At(seconds(0));
	VerifyAndTrack(sweeper, fake.Add(5, 50, /*verified=*/true), 5, start);
	const SteadyTime reconnectAt = start + seconds(10);
	VerifyAndTrack(sweeper, fake.Add(5, 51, /*verified=*/false), 5, reconnectAt);

	EXPECT_EQ(fake.SweepEachSecond(sweeper, start + kPeriod, reconnectAt + kTimeout - kPeriod), 0u);
	EXPECT_EQ(fake.SweepEachSecond(sweeper, reconnectAt + kTimeout, reconnectAt + kTimeout), 1u);
	ASSERT_EQ(fake.idleClosed.size(), 1u);
	EXPECT_EQ(fake.idleClosed[0].first, 5u);
	EXPECT_EQ(fake.idleClosed[0].second, LivenessSweeper::Duration(kTimeout));
}

// 本端 loop 卡过(距上次扫描超过两个节拍):这一轮只推迟,下一次准时的扫描才判。
TEST(ClientLivenessSweeper, LateSweepDefersAndTheNextOnTimeSweepCloses)
{
	LivenessSweeper sweeper(kClientLiveness);
	FakeSessions fake;
	const SteadyTime start = At(seconds(0));
	VerifyAndTrack(sweeper, fake.Add(7, 70, /*verified=*/true), 7, start);
	EXPECT_EQ(fake.SweepEachSecond(sweeper, start + kPeriod, start + kTimeout - kPeriod), 0u);

	const SteadyTime stalledUntil = start + kTimeout + seconds(3);
	const auto late = fake.Sweep(sweeper, stalledUntil);
	EXPECT_TRUE(late.late);
	EXPECT_EQ(late.checkGap, LivenessSweeper::Duration(seconds(4)));
	EXPECT_EQ(late.deferred, 1u);
	EXPECT_EQ(late.idleClosed, 0u);
	EXPECT_TRUE(fake.idleClosed.empty());

	const auto onTime = fake.Sweep(sweeper, stalledUntil + kPeriod);
	EXPECT_FALSE(onTime.late);
	EXPECT_EQ(onTime.idleClosed, 1u);
	ASSERT_EQ(fake.idleClosed.size(), 1u);
}

// 卡顿期间积压在接收缓冲里的帧,在推迟之后、下一次扫描之前读到:不算空闲。
TEST(ClientLivenessSweeper, FrameReadAfterAStallSavesTheConnection)
{
	LivenessSweeper sweeper(kClientLiveness);
	FakeSessions fake;
	const SteadyTime start = At(seconds(0));
	FakeSession &session = fake.Add(7, 70, /*verified=*/true);
	VerifyAndTrack(sweeper, session, 7, start);
	EXPECT_EQ(fake.SweepEachSecond(sweeper, start + kPeriod, start + kTimeout - kPeriod), 0u);

	const SteadyTime stalledUntil = start + kTimeout + seconds(3);
	EXPECT_EQ(fake.Sweep(sweeper, stalledUntil).deferred, 1u);
	session.liveness.NoteInbound(stalledUntil + milliseconds(10));

	EXPECT_EQ(fake.Sweep(sweeper, stalledUntil + kPeriod).idleClosed, 0u);
	EXPECT_TRUE(fake.idleClosed.empty());
	EXPECT_EQ(sweeper.PendingChecks(), 1u);
}

// 检查点只在到期时取出;别的连接的检查点不受影响。
TEST(ClientLivenessSweeper, OnlyDueCheckpointsAreExamined)
{
	LivenessSweeper sweeper(kClientLiveness);
	FakeSessions fake;
	const SteadyTime start = At(seconds(0));
	VerifyAndTrack(sweeper, fake.Add(1, 10, /*verified=*/true), 1, start);
	VerifyAndTrack(sweeper, fake.Add(2, 20, /*verified=*/true), 2, start + seconds(5));

	EXPECT_EQ(fake.SweepEachSecond(sweeper, start + kPeriod, start + kTimeout), 1u);
	ASSERT_EQ(fake.idleClosed.size(), 1u);
	EXPECT_EQ(fake.idleClosed[0].first, 1u);
	EXPECT_EQ(sweeper.PendingChecks(), 1u);

	EXPECT_EQ(fake.SweepEachSecond(sweeper, start + kTimeout + kPeriod, start + seconds(5) + kTimeout), 1u);
	ASSERT_EQ(fake.idleClosed.size(), 2u);
	EXPECT_EQ(fake.idleClosed[1].first, 2u);
	EXPECT_EQ(sweeper.PendingChecks(), 0u);
}
