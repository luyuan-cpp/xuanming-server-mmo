// gate 客户端连接准入(handler/rpc/connection_admission.h,docs/design/gate-connection-admission-control.md P1-1 ~ P1-4)。
//
// 不进 gate.vcxproj,登记在 cpp/tests/routing_identity_test/routing_identity_test.vcxproj。分两组:
//   - 纯逻辑:拒绝原因、session id 空间的硬上限、GATE_* 环境变量、判定顺序。时刻全部注入,整数时长,边界值精确。
//     与 battle 直连共用的速率闸(GCRA)、缺省速率推导、环境变量解析与握手直方图在 core 的 network/accept_admission.h,
//     由 cpp/tests/rpc_controller_test/accept_admission_test.cpp 测;
//   - 装配:AdmitConnection 按原因计数,未验签占位随验签 / 会话销毁归还,指标页的样本行。会话表用进程里的 tlsSessionManager
//     (来自 gate.lib),每个用例前后清空;计数是 thread_local,SetUp 里 ResetForTesting。
// 「准入回调在 accept 之后、TcpConnection 建出来之前执行,被拒的连接收到 RST」是 muduo 那一层的行为,
// 由 cpp/tests/rpc_controller_test/acceptor_admission_test.cpp 用真实回环连接测;main.cpp 把两者接起来的那几行靠冒烟。

#include <gtest/gtest.h>

#include <chrono>
#include <cstdint>
#include <memory>
#include <set>
#include <string>
#include <string_view>
#include <utility>

#include "handler/rpc/client_heartbeat.h"
#include "handler/rpc/connection_admission.h"
#include "metrics/prometheus_text.h"
#include "muduo/net/InetAddress.h"
#include "session/manager/session_manager.h"

namespace
{

using namespace gate_connection_admission;
using std::chrono::milliseconds;
using std::chrono::seconds;

// 离时钟零点足够远的起点:速率闸把零点当「桶是满的」,用例不该碰巧落在零点上。
const SteadyTime kT0 = SteadyTime{} + std::chrono::hours(1);

uint64_t Rejected(const RejectReason reason)
{
	return GetCounters().rejected[static_cast<size_t>(reason)];
}

// ---------------------------------------------------------------------------
// 纯逻辑
// ---------------------------------------------------------------------------

// 原因名是指标标签的取值:每个原因一个,非空、互不相同、只含小写字母与下划线。
TEST(ConnectionAdmissionPolicy, EveryRejectReasonHasADistinctLabelValue)
{
	std::set<std::string_view> seen;
	for (size_t i = 0; i < kRejectReasonCount; ++i)
	{
		const std::string_view name = RejectReasonName(static_cast<RejectReason>(i));
		ASSERT_FALSE(name.empty()) << "reason " << i;
		for (const char c : name)
		{
			EXPECT_TRUE((c >= 'a' && c <= 'z') || c == '_') << name;
		}
		EXPECT_TRUE(seen.insert(name).second) << "duplicate label value: " << name;
	}
	EXPECT_EQ(RejectReasonName(RejectReason::kCapacity), "capacity");
	EXPECT_EQ(RejectReasonName(RejectReason::kAcceptRate), "accept_rate");
	EXPECT_EQ(RejectReasonName(RejectReason::kShuttingDown), "shutting_down");
}

// dev / test 的 0 只关掉配置阈值,session id 空间的硬上限仍在。
TEST(ConnectionAdmissionPolicy, ZeroConfiguredLimitFallsBackToTheSessionIdSpace)
{
	EXPECT_EQ(EffectiveMaxConnections(0), SessionIdGenerator::kSeqMask);
	EXPECT_EQ(EffectiveMaxConnections(1), 1u);
	EXPECT_EQ(EffectiveMaxConnections(20000), 20000u);
}

// gate 的转发口带的是 GATE_* 两个变量名;解析规则本身在 accept_admission_test.cpp。
TEST(ConnectionAdmissionPolicy, RateLimitResolvesFromEnvOrFallsBackToTheDefault)
{
	std::string error;
	EXPECT_EQ(ResolveRateLimit(nullptr, nullptr, 20000, error), DefaultRateLimit(20000));
	EXPECT_TRUE(error.empty());

	// 只给速率:桶深仍取 max(上限, 速率)。
	EXPECT_EQ(ResolveRateLimit("500", nullptr, 20000, error), (RateLimit{500, 20000}));
	EXPECT_EQ(ResolveRateLimit("50000", "", 20000, error), (RateLimit{50000, 50000}));
	// 只给桶深:速率仍按缺省推导。
	EXPECT_EQ(ResolveRateLimit(nullptr, "300", 20000, error), (RateLimit{200, 300}));
	// 两个都给:原样采用,允许低于缺省下限(运维明确写了就照办)。
	EXPECT_EQ(ResolveRateLimit("10", "20", 20000, error), (RateLimit{10, 20}));
	EXPECT_TRUE(error.empty());

	EXPECT_FALSE(ResolveRateLimit("0", nullptr, 20000, error).has_value()) << "没有「0 = 关闭」";
	EXPECT_NE(error.find(kAcceptRateEnv), std::string::npos) << error;
	error.clear();
	EXPECT_FALSE(ResolveRateLimit("200", "many", 20000, error).has_value());
	EXPECT_NE(error.find(kAcceptBurstEnv), std::string::npos) << error;
	error.clear();
	EXPECT_FALSE(ResolveRateLimit("1000001", nullptr, 20000, error).has_value()) << "超过上界";
	EXPECT_FALSE(error.empty());
	EXPECT_NE(error.find("GateMaxConnections"), std::string::npos) << "说明里点名的是 gate 的上限配置项: " << error;
}

// 判定顺序:关源 → 总量 → 速率;只有放行才从速率闸取名额。
TEST(ConnectionAdmissionPolicy, DecideChecksSourceThenCapacityThenRate)
{
	AcceptRateGate gate(RateLimit{10, 1});

	EXPECT_EQ(Decide(AdmissionState{.sourceClosed = true, .currentSessions = 0, .maxConnections = 10}, gate, kT0),
			  RejectReason::kShuttingDown);
	EXPECT_EQ(Decide(AdmissionState{.sourceClosed = false, .currentSessions = 10, .maxConnections = 10}, gate, kT0),
			  RejectReason::kCapacity);
	EXPECT_EQ(Decide(AdmissionState{.sourceClosed = false, .currentSessions = 11, .maxConnections = 10}, gate, kT0),
			  RejectReason::kCapacity);
	// 上面三次拒绝都没有动速率闸:唯一的名额还在。
	EXPECT_FALSE(
		Decide(AdmissionState{.sourceClosed = false, .currentSessions = 9, .maxConnections = 10}, gate, kT0).has_value());
	EXPECT_EQ(Decide(AdmissionState{.sourceClosed = false, .currentSessions = 9, .maxConnections = 10}, gate, kT0),
			  RejectReason::kAcceptRate);
}

// ---------------------------------------------------------------------------
// 装配
// ---------------------------------------------------------------------------

class ConnectionAdmission : public ::testing::Test
{
protected:
	void SetUp() override
	{
		tlsSessionManager.sessions().clear();
		ResetForTesting();
		baselineUnauthenticated_ = UnauthenticatedConnections();
	}
	void TearDown() override
	{
		tlsSessionManager.sessions().clear();
		ResetForTesting();
	}

	// 本用例新增的未验签连接数(别的用例不会留下占位,基线正常是 0;取差值只为不依赖这一点)。
	uint64_t Unauthenticated() const { return UnauthenticatedConnections() - baselineUnauthenticated_; }

	static SessionInfo &AddSession(const SessionId id) { return tlsSessionManager.sessions()[id]; }

	// 准入回调只把对端地址写进采样日志,取值与判定无关。
	const muduo::net::InetAddress peer_{static_cast<uint16_t>(40000)};

private:
	uint64_t baselineUnauthenticated_{0};
};

TEST_F(ConnectionAdmission, AdmitsBelowTheCapAndRejectsAtIt)
{
	Configure(2, RateLimit{1000, 1000});
	EXPECT_TRUE(AdmitConnection(peer_, kT0));
	AddSession(1);
	EXPECT_TRUE(AdmitConnection(peer_, kT0));
	AddSession(2);

	EXPECT_FALSE(AdmitConnection(peer_, kT0));
	EXPECT_FALSE(AdmitConnection(peer_, kT0));
	EXPECT_EQ(Rejected(RejectReason::kCapacity), 2u);
	EXPECT_EQ(Rejected(RejectReason::kAcceptRate), 0u);

	// 走掉一条连接,位置立刻腾出来。
	tlsSessionManager.sessions().erase(1);
	EXPECT_TRUE(AdmitConnection(peer_, kT0));
}

// dev / test 把上限配成 0:准入闸按 session id 空间放行,不会把 0 当成「一条都不接」。
TEST_F(ConnectionAdmission, ZeroConfiguredLimitStillAdmits)
{
	Configure(0, DefaultRateLimit(EffectiveMaxConnections(0)));
	EXPECT_TRUE(AdmitConnection(peer_, kT0));
	EXPECT_EQ(Rejected(RejectReason::kCapacity), 0u);
}

TEST_F(ConnectionAdmission, RejectsAboveTheAcceptRateAndRecoversWithTime)
{
	Configure(1000, RateLimit{1, 2});
	EXPECT_TRUE(AdmitConnection(peer_, kT0));
	EXPECT_TRUE(AdmitConnection(peer_, kT0));
	EXPECT_FALSE(AdmitConnection(peer_, kT0));
	EXPECT_EQ(Rejected(RejectReason::kAcceptRate), 1u);
	EXPECT_EQ(Rejected(RejectReason::kCapacity), 0u);

	EXPECT_TRUE(AdmitConnection(peer_, kT0 + seconds(1)));
	EXPECT_EQ(CurrentRateLimit(), (RateLimit{1, 2}));
}

// 满员时被拒的连接不消耗速率名额:腾出位置之后名额还在。
TEST_F(ConnectionAdmission, CapacityRejectionsDoNotBurnRateTokens)
{
	Configure(1, RateLimit{1, 1});
	AddSession(1);
	for (int i = 0; i < 100; ++i)
	{
		EXPECT_FALSE(AdmitConnection(peer_, kT0));
	}
	EXPECT_EQ(Rejected(RejectReason::kCapacity), 100u);
	tlsSessionManager.sessions().erase(1);
	EXPECT_TRUE(AdmitConnection(peer_, kT0)) << "唯一的名额没有被满员期间的拒绝用掉";
}

TEST_F(ConnectionAdmission, ClosedSourceRejectsEverything)
{
	Configure(1000, RateLimit{1000, 1000});
	EXPECT_TRUE(AdmitConnection(peer_, kT0));
	CloseSource();
	EXPECT_FALSE(AdmitConnection(peer_, kT0));
	EXPECT_FALSE(AdmitConnection(peer_, kT0 + seconds(60)));
	EXPECT_EQ(Rejected(RejectReason::kShuttingDown), 2u);
}

// 连接建出来之后才被拒(纵深防御分支)只计数。
TEST_F(ConnectionAdmission, PostAcceptRejectionsAreCountedByReason)
{
	NoteRejectedAfterAccept(RejectReason::kNotReady);
	NoteRejectedAfterAccept(RejectReason::kNoTokenSecret);
	NoteRejectedAfterAccept(RejectReason::kNoTokenSecret);
	EXPECT_EQ(Rejected(RejectReason::kNotReady), 1u);
	EXPECT_EQ(Rejected(RejectReason::kNoTokenSecret), 2u);
}

TEST_F(ConnectionAdmission, UnverifiedSessionHoldsASlotUntilItVerifies)
{
	SessionInfo &session = AddSession(1);
	OnSessionOpened(session, kT0);
	EXPECT_EQ(GetCounters().accepted, 1u);
	EXPECT_EQ(Unauthenticated(), 1u);
	ASSERT_TRUE(session.preAuthSlot);

	OnSessionVerified(session, kT0 + milliseconds(40));
	EXPECT_EQ(Unauthenticated(), 0u);
	EXPECT_FALSE(session.preAuthSlot);
	EXPECT_EQ(GetCounters().handshakes, 1u);

	// 已验证会话重发验签:没有占位,不重复计。
	OnSessionVerified(session, kT0 + seconds(5));
	EXPECT_EQ(Unauthenticated(), 0u);
	EXPECT_EQ(GetCounters().handshakes, 1u);
}

// dev 免验签的会话建连即已验证:计接纳,不占未验签位,也不进握手直方图。
TEST_F(ConnectionAdmission, SessionVerifiedAtConnectHoldsNoSlot)
{
	SessionInfo &session = AddSession(1);
	session.verified = true;
	OnSessionOpened(session, kT0);
	EXPECT_EQ(GetCounters().accepted, 1u);
	EXPECT_EQ(Unauthenticated(), 0u);
	OnSessionVerified(session, kT0);
	EXPECT_EQ(GetCounters().handshakes, 0u);
}

// 没验签就断开:会话从表里删掉,占位随之归还,不需要任何出口手写减一。
TEST_F(ConnectionAdmission, ErasingAnUnverifiedSessionReturnsItsSlot)
{
	OnSessionOpened(AddSession(1), kT0);
	OnSessionOpened(AddSession(2), kT0);
	OnSessionOpened(AddSession(3), kT0);
	EXPECT_EQ(Unauthenticated(), 3u);

	tlsSessionManager.sessions().erase(2);
	EXPECT_EQ(Unauthenticated(), 2u);
	tlsSessionManager.sessions().clear();
	EXPECT_EQ(Unauthenticated(), 0u);
	EXPECT_EQ(GetCounters().handshakes, 0u) << "没验签的连接不进握手直方图";
}

// 占位放在 shared_ptr 里(会话按值拷贝时共享同一个占位):最后一份引用销毁才归还,不会减两次。
TEST_F(ConnectionAdmission, SharedSlotIsReturnedOnceWhenTheLastOwnerGoes)
{
	SessionInfo &session = AddSession(1);
	OnSessionOpened(session, kT0);
	{
		const std::shared_ptr<PreAuthSlot> other = session.preAuthSlot;
		EXPECT_EQ(Unauthenticated(), 1u);
		tlsSessionManager.sessions().erase(1);
		EXPECT_EQ(Unauthenticated(), 1u) << "另一份引用还活着";
	}
	EXPECT_EQ(Unauthenticated(), 0u);
}

// ResetForTesting 清计数与配置,但不动仍被会话持有的占位。
TEST_F(ConnectionAdmission, ResetKeepsLiveSlotsCounted)
{
	OnSessionOpened(AddSession(1), kT0);
	ResetForTesting();
	EXPECT_EQ(GetCounters().accepted, 0u);
	EXPECT_EQ(Unauthenticated(), 1u);
	tlsSessionManager.sessions().clear();
	EXPECT_EQ(Unauthenticated(), 0u);
}

// 计数原样写进 /metrics:标签只有封闭的 reason,不带对端地址、会话号与玩家号(AGENTS.md §9)。
TEST_F(ConnectionAdmission, CountersAreExportedAsPrometheusMetrics)
{
	const uint64_t reapedBefore = gate_client_heartbeat::GetCounters().handshakeTimeouts;
	Configure(3, RateLimit{50, 60});

	// 三条接纳:一条 40ms 验签,一条 2.5s 验签,一条还没验签。
	ASSERT_TRUE(AdmitConnection(peer_, kT0));
	SessionInfo &fast = AddSession(1);
	OnSessionOpened(fast, kT0);
	ASSERT_TRUE(AdmitConnection(peer_, kT0));
	SessionInfo &slow = AddSession(2);
	OnSessionOpened(slow, kT0);
	ASSERT_TRUE(AdmitConnection(peer_, kT0));
	OnSessionOpened(AddSession(3), kT0);
	OnSessionVerified(fast, kT0 + milliseconds(40));
	OnSessionVerified(slow, kT0 + milliseconds(2500));
	// 满员之后两条被拒;关源之后再一条。
	EXPECT_FALSE(AdmitConnection(peer_, kT0));
	EXPECT_FALSE(AdmitConnection(peer_, kT0));
	CloseSource();
	EXPECT_FALSE(AdmitConnection(peer_, kT0));

	prometheus_text::Writer writer;
	CollectMetrics(writer);
	const std::string &text = writer.Text();
	EXPECT_EQ(writer.DroppedSamples(), 0u) << text;

	const std::string reaped = "gate_preauth_reaped_total " + std::to_string(reapedBefore) + "\n";
	for (const std::string &line : {
			 std::string("gate_connections_current 3\n"),
			 std::string("gate_connections_max 3\n"),
			 std::string("gate_unauthenticated_connections 1\n"),
			 std::string("gate_accepted_total 3\n"),
			 std::string("gate_rejected_total{reason=\"capacity\"} 2\n"),
			 std::string("gate_rejected_total{reason=\"accept_rate\"} 0\n"),
			 std::string("gate_rejected_total{reason=\"shutting_down\"} 1\n"),
			 std::string("gate_rejected_total{reason=\"not_ready\"} 0\n"),
			 std::string("gate_rejected_total{reason=\"no_token_secret\"} 0\n"),
			 std::string("gate_accept_rate_limit_per_second 50\n"),
			 std::string("gate_accept_burst 60\n"),
			 std::string("# TYPE gate_handshake_duration_ms histogram\n"),
			 std::string("gate_handshake_duration_ms_bucket{le=\"25\"} 0\n"),
			 std::string("gate_handshake_duration_ms_bucket{le=\"50\"} 1\n"),
			 std::string("gate_handshake_duration_ms_bucket{le=\"2000\"} 1\n"),
			 std::string("gate_handshake_duration_ms_bucket{le=\"3000\"} 2\n"),
			 std::string("gate_handshake_duration_ms_bucket{le=\"+Inf\"} 2\n"),
			 std::string("gate_handshake_duration_ms_sum 2540\n"),
			 std::string("gate_handshake_duration_ms_count 2\n"),
			 reaped,
		 })
	{
		EXPECT_NE(text.find(line), std::string::npos) << "missing sample line: " << line << "in:\n" << text;
	}
	EXPECT_EQ(text.find("peer"), std::string::npos);
	EXPECT_EQ(text.find("session_id"), std::string::npos);
	EXPECT_EQ(text.find("player_id"), std::string::npos);
}

} // namespace
