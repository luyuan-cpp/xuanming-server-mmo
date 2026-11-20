// gate 客户端连接准入(handler/rpc/connection_admission.h,docs/design/gate-connection-admission-control.md P0-2、P1-1 ~ P1-4)。
//
// 不进 gate.vcxproj,登记在 cpp/tests/routing_identity_test/routing_identity_test.vcxproj。分两组:
//   - 纯逻辑:拒绝原因、session id 空间的硬上限、GATE_* 环境变量、未验签预算(缺省值的推导、承压时先淘汰后早拒、
//     早拒概率的线性曲线)、判定顺序。时刻与随机数全部注入,整数时长,边界值精确。
//     与 battle 直连共用的速率闸(GCRA)、缺省速率推导、环境变量解析与握手直方图在 core 的 network/accept_admission.h,
//     由 cpp/tests/rpc_controller_test/accept_admission_test.cpp 测;
//   - 装配:AdmitConnection 按原因计数,未验签占位随验签 / 会话销毁归还,承压时按建连先后淘汰最老的未验签会话
//     (跳过已验签与会话号被复用的),指标页的样本行。会话表用进程里的 tlsSessionManager(来自 gate.lib),
//     每个用例前后清空;计数是 thread_local,SetUp 里 ResetForTesting。淘汰真的关掉了连接、又没有同步删会话,
//     用一对真实的回环连接测(loopback_connection.h)。
// 「准入回调在 accept 之后、TcpConnection 建出来之前执行,被拒的连接收到 RST」是 muduo 那一层的行为,
// 由 cpp/tests/rpc_controller_test/acceptor_admission_test.cpp 用真实回环连接测;main.cpp 把两者接起来的那几行靠冒烟。

#include <gtest/gtest.h>

#include <chrono>
#include <cstdint>
#include <memory>
#include <optional>
#include <set>
#include <string>
#include <string_view>
#include <utility>

#include "handler/rpc/client_heartbeat.h"
#include "handler/rpc/connection_admission.h"
#include "loopback_connection.h"
#include "metrics/prometheus_text.h"
#include "muduo/net/EventLoop.h"
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

// 按概率早拒的随机源替身(UniformBelow 是函数指针,替身用全局变量带参数)。
uint32_t gDraw = 0;       // FixedDraw 返回的值
uint32_t gDrawCalls = 0;  // 随机源被取了几次
uint32_t FixedDraw(const uint32_t bound)
{
	++gDrawCalls;
	EXPECT_LT(gDraw, bound) << "替身返回的值必须落在 [0, bound) 里";
	return gDraw;
}
uint32_t NeverDrop(const uint32_t bound)
{
	++gDrawCalls;
	return bound - 1;
}
uint32_t AlwaysDrop(uint32_t)
{
	++gDrawCalls;
	return 0;
}

// 未验签池里最老一条连接的年龄。
std::optional<SteadyTime::duration> Age(const SteadyTime::duration age)
{
	return age;
}

// 缺省的可淘汰年龄(accept_admission.h)。
constexpr auto kMinEvictableAge = kDefaultMinEvictableAge;

// 本文件里会话的代次从这里起。存活扫描的检查点堆是线程局部的,别的用例留下的检查点按 (session_id, 代次) 找会话,
// 代次错开就碰不到这里的会话。
constexpr uint64_t kIncarnationBase = 0x7E57'0000'0000ULL;

// 让存活扫描在 now 打一拍、且这一拍不迟到:LoopRecentlyStalled(now) 为假,淘汰只看年龄。扫描的节拍也是线程局部的,
// 别的用例留下的上一拍可能隔得很远,所以先在一个节拍之前补一拍。
void MarkLoopHealthy(const SteadyTime now)
{
	gate_client_heartbeat::SweepLiveness(now - seconds(1));
	gate_client_heartbeat::SweepLiveness(now);
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
	EXPECT_EQ(RejectReasonName(RejectReason::kPreAuthBudget), "preauth_budget");
	EXPECT_EQ(RejectReasonName(RejectReason::kPreAuthEarlyDrop), "preauth_early_drop");
	EXPECT_EQ(RejectReasonName(RejectReason::kSourceRate), "source_rate");
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

// 未验签预算 = 缺省持续速率 × 握手期限:上限 2 万时是上限的 10%,早拒起点是它的一半;上限不到 1 万时跟着速率下限。
TEST(ConnectionAdmissionPolicy, PreAuthBudgetIsWhatTheDefaultRateAdmitsInOneHandshakeDeadline)
{
	EXPECT_EQ(DefaultPreAuthBudget(20000), (PreAuthBudget{1000, 2000}));
	EXPECT_EQ(DefaultPreAuthBudget(131071), (PreAuthBudget{6550, 13100}));
	EXPECT_EQ(DefaultPreAuthBudget(10000), (PreAuthBudget{500, 1000}));
	// 上限调得很小(本机、验收把上限调到几十)时起点高于上限,这一道不起作用,由总量闸管。
	EXPECT_EQ(DefaultPreAuthBudget(50), (PreAuthBudget{500, 1000}));

	const RateLimit rate = DefaultRateLimit(20000);
	const auto deadline = client_liveness::kClientLiveness.handshakeTimeout;
	EXPECT_EQ(DefaultPreAuthBudget(20000).full, rate.perSecond * std::chrono::duration_cast<seconds>(deadline).count());
	// 速率闸放进来的稳态流量下池停在起点,每条连接要活「起点 / 持续速率」才轮到它让位;这段时间不短于可淘汰的年龄,
	// 所以淘汰换掉的只是那么久还没验签的连接。可淘汰的年龄又短于握手期限,否则承压时淘汰永远轮不到。
	EXPECT_GE(seconds(DefaultPreAuthBudget(20000).earlyDropStart / rate.perSecond), kMinEvictableAge);
	EXPECT_LT(kMinEvictableAge, deadline);
}

// 不承压(池不到起点)时一律放行:最老的再老也不淘汰,也不取随机数。
TEST(ConnectionAdmissionPolicy, PreAuthAdmitsBelowTheEarlyDropStartWithoutDrawing)
{
	const PreAuthBudget budget{10, 20};
	gDrawCalls = 0;
	EXPECT_EQ(DecidePreAuth(budget, 0, std::nullopt, &AlwaysDrop), PreAuthVerdict::kAdmit);
	EXPECT_EQ(DecidePreAuth(budget, 9, Age(seconds(9)), &AlwaysDrop), PreAuthVerdict::kAdmit);
	// 恰好在起点上:早拒概率是 0,同样不取随机数。
	EXPECT_EQ(DecidePreAuth(budget, 10, Age(milliseconds(1)), &AlwaysDrop), PreAuthVerdict::kAdmit);
	EXPECT_EQ(gDrawCalls, 0u);
}

// 承压时先淘汰:最老的一条建连满 kMinEvictableAge 就给新连接让位,到了预算也一样;不满才轮到早拒与预算。
TEST(ConnectionAdmissionPolicy, UnderPressureTheOldestYieldsOnceItIsOldEnough)
{
	const PreAuthBudget budget{10, 20};
	gDrawCalls = 0;
	EXPECT_EQ(DecidePreAuth(budget, 10, Age(kMinEvictableAge), &AlwaysDrop), PreAuthVerdict::kAdmitEvictOldest);
	EXPECT_EQ(DecidePreAuth(budget, 15, Age(seconds(9)), &AlwaysDrop), PreAuthVerdict::kAdmitEvictOldest);
	EXPECT_EQ(DecidePreAuth(budget, 25, Age(kMinEvictableAge), &AlwaysDrop), PreAuthVerdict::kAdmitEvictOldest)
		<< "到了预算也先淘汰,不拒";
	EXPECT_EQ(gDrawCalls, 0u) << "能淘汰就不取随机数";

	const auto young = Age(kMinEvictableAge - milliseconds(1));
	EXPECT_EQ(DecidePreAuth(budget, 20, young, &NeverDrop), PreAuthVerdict::kRejectBudget);
	EXPECT_EQ(DecidePreAuth(budget, 25, young, &NeverDrop), PreAuthVerdict::kRejectBudget);
	EXPECT_EQ(DecidePreAuth(budget, 20, std::nullopt, &NeverDrop), PreAuthVerdict::kRejectBudget)
		<< "找不到最老的一条(例如占位被会话的副本拿着)时按没有可淘汰的算";
	EXPECT_EQ(DecidePreAuth(budget, 15, young, &AlwaysDrop), PreAuthVerdict::kRejectEarlyDrop);
	EXPECT_EQ(DecidePreAuth(budget, 15, young, &NeverDrop), PreAuthVerdict::kAdmit);
}

// 早拒概率 p(n) = (n - S) / (B - S):随机源取遍 [0, B - S) 的每个值,被拒的恰好是 n - S 个。
TEST(ConnectionAdmissionPolicy, EarlyDropProbabilityRisesLinearlyFromTheStartToTheBudget)
{
	const PreAuthBudget budget{10, 20};
	const auto young = Age(milliseconds(100));
	for (uint64_t n = budget.earlyDropStart; n < budget.full; ++n)
	{
		uint32_t rejected = 0;
		for (gDraw = 0; gDraw < budget.full - budget.earlyDropStart; ++gDraw)
		{
			rejected += DecidePreAuth(budget, n, young, &FixedDraw) == PreAuthVerdict::kRejectEarlyDrop ? 1 : 0;
		}
		EXPECT_EQ(rejected, n - budget.earlyDropStart) << "n=" << n;
	}
}

// 起点不低于预算(配置退化)时没有早拒带:到起点就是到预算,不会拿 0 去取随机数。
TEST(ConnectionAdmissionPolicy, DegenerateBudgetWithoutAnEarlyDropBandStillRejectsAtTheBudget)
{
	gDrawCalls = 0;
	EXPECT_EQ(DecidePreAuth(PreAuthBudget{5, 5}, 5, Age(milliseconds(1)), &FixedDraw), PreAuthVerdict::kRejectBudget);
	EXPECT_EQ(DecidePreAuth(PreAuthBudget{6, 5}, 6, std::nullopt, &FixedDraw), PreAuthVerdict::kRejectBudget);
	EXPECT_EQ(DecidePreAuth(PreAuthBudget{6, 5}, 5, std::nullopt, &FixedDraw), PreAuthVerdict::kAdmit);
	EXPECT_EQ(gDrawCalls, 0u);
}

// 判定顺序:关源 → 总量 → 按来源限速 → 未验签预算 → 速率;只有放行才从速率闸取名额。
TEST(ConnectionAdmissionPolicy, DecideChecksSourceThenCapacityThenPerSourceThenPreAuthThenRate)
{
	AcceptRateGate gate(RateLimit{10, 1});
	const AdmissionState open{.sourceClosed = false, .currentSessions = 9, .maxConnections = 10};
	AdmissionState closed = open;
	closed.sourceClosed = true;
	AdmissionState full = open;
	full.currentSessions = 10;
	full.sourceOverRate = true; // 满员排在来源之前
	AdmissionState overFull = open;
	overFull.currentSessions = 11;
	// 未验签池到了预算,里面全是刚建的连接:没有可以让位的。
	AdmissionState youngPool = open;
	youngPool.unauthenticated = 20;
	youngPool.oldestUnauthenticatedAge = milliseconds(100);
	youngPool.preAuthBudget = PreAuthBudget{10, 20};
	AdmissionState earlyDropBand = youngPool;
	earlyDropBand.unauthenticated = 15;
	AdmissionState sourceOverRate = youngPool;
	sourceOverRate.sourceOverRate = true;

	EXPECT_EQ(Decide(closed, gate, kT0, &AlwaysDrop).rejected, RejectReason::kShuttingDown);
	EXPECT_EQ(Decide(full, gate, kT0, &AlwaysDrop).rejected, RejectReason::kCapacity);
	EXPECT_EQ(Decide(overFull, gate, kT0, &AlwaysDrop).rejected, RejectReason::kCapacity);
	EXPECT_EQ(Decide(sourceOverRate, gate, kT0, &AlwaysDrop).rejected, RejectReason::kSourceRate) << "来源超速排在预算之前";
	EXPECT_EQ(Decide(youngPool, gate, kT0, &AlwaysDrop).rejected, RejectReason::kPreAuthBudget);
	EXPECT_EQ(Decide(earlyDropBand, gate, kT0, &AlwaysDrop).rejected, RejectReason::kPreAuthEarlyDrop);
	// 上面几次拒绝都没有动速率闸:唯一的名额还在。
	EXPECT_EQ(Decide(open, gate, kT0, &AlwaysDrop), (Admission{}));
	EXPECT_EQ(Decide(open, gate, kT0, &AlwaysDrop).rejected, RejectReason::kAcceptRate);
}

// 淘汰只在放行时才执行:速率闸也放行了才带「先淘汰」,速率闸拒了就不淘汰。
TEST(ConnectionAdmissionPolicy, EvictionIsRequestedOnlyWhenTheConnectionIsAdmitted)
{
	AcceptRateGate gate(RateLimit{10, 1});
	AdmissionState oldPool{.sourceClosed = false, .currentSessions = 9, .maxConnections = 10};
	oldPool.unauthenticated = 20;
	oldPool.oldestUnauthenticatedAge = seconds(5);
	oldPool.preAuthBudget = PreAuthBudget{10, 20};

	EXPECT_EQ(Decide(oldPool, gate, kT0, &AlwaysDrop), (Admission{.evictOldestUnauthenticated = true}));
	const Admission rateLimited = Decide(oldPool, gate, kT0, &AlwaysDrop);
	EXPECT_EQ(rateLimited.rejected, RejectReason::kAcceptRate);
	EXPECT_FALSE(rateLimited.evictOldestUnauthenticated) << "没放行就不淘汰";
}

// gate 的三个预算覆盖变量:不设按上限推导;只设预算时起点按新预算取一半;起点不能大于预算;可淘汰年龄短于握手期限。
TEST(ConnectionAdmissionPolicy, PreAuthBudgetResolvesFromTheGateEnvNames)
{
	std::string error;
	EXPECT_EQ(ResolvePreAuthBudget(nullptr, nullptr, nullptr, 20000, error), DefaultPreAuthBudget(20000));
	EXPECT_EQ(ResolvePreAuthBudget("4000", nullptr, nullptr, 20000, error), (PreAuthBudget{2000, 4000}));
	EXPECT_EQ(ResolvePreAuthBudget(nullptr, "1500", "5000", 20000, error), (PreAuthBudget{1500, 2000, milliseconds(5000)}));
	EXPECT_EQ(ResolvePreAuthBudget(nullptr, "2000", "9999", 20000, error), (PreAuthBudget{2000, 2000, milliseconds(9999)}))
		<< "起点等于预算、年龄差 1ms 到握手期限都合法";
	EXPECT_TRUE(error.empty()) << error;

	EXPECT_FALSE(ResolvePreAuthBudget(nullptr, "2001", nullptr, 20000, error).has_value()) << "起点不能大于预算";
	EXPECT_NE(error.find(kPreAuthEarlyDropStartEnv), std::string::npos) << error;
	error.clear();
	EXPECT_FALSE(ResolvePreAuthBudget(nullptr, nullptr, "10000", 20000, error).has_value()) << "可淘汰年龄必须短于握手期限";
	EXPECT_NE(error.find(kPreAuthEvictableAgeEnv), std::string::npos) << error;
	error.clear();
	EXPECT_FALSE(ResolvePreAuthBudget("0", nullptr, nullptr, 20000, error).has_value()) << "没有「0 = 关闭」";
	EXPECT_NE(error.find(kPreAuthBudgetEnv), std::string::npos) << error;
}

// 按来源限速缺省关闭,设了速率才开;桶深缺省是速率 × 握手期限;推荐值是全局缺省持续速率的 1/10。
// 前缀聚合、只限公网来源与前缀表的用例在 core 的 accept_admission_test.cpp;这里核 gate 的转发口带 GATE_* 变量名。
TEST(ConnectionAdmissionPolicy, PerSourceLimitingIsOffUnlessItsRateIsSet)
{
	std::optional<RateLimit> limit = RateLimit{1, 1};
	std::string error;
	EXPECT_TRUE(ResolveSourceRateLimit(nullptr, nullptr, limit, error));
	EXPECT_FALSE(limit.has_value()) << "不设 = 关闭";
	EXPECT_TRUE(ResolveSourceRateLimit("20", nullptr, limit, error));
	EXPECT_EQ(limit, (RateLimit{20, 200}));
	EXPECT_TRUE(ResolveSourceRateLimit("20", "50", limit, error));
	EXPECT_EQ(limit, (RateLimit{20, 50}));
	EXPECT_TRUE(error.empty()) << error;

	EXPECT_FALSE(ResolveSourceRateLimit(nullptr, "50", limit, error)) << "只设桶深不设速率";
	EXPECT_NE(error.find(kSourceRateEnv), std::string::npos) << error;
	error.clear();
	EXPECT_FALSE(ResolveSourceRateLimit("0", nullptr, limit, error)) << "没有「0 = 关闭」,关闭就是不设";
	EXPECT_NE(error.find(kSourceRateEnv), std::string::npos) << error;

	EXPECT_EQ(DefaultSourceRateLimit(20000), (RateLimit{20, 200}));
	EXPECT_EQ(DefaultSourceRateLimit(50), (RateLimit{10, 100})) << "全局取下限 100 时";
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

	// 同 HandleConnectionEstablished:插入会话、领未验签占位、进淘汰索引。会话代次取 kIncarnationBase + 会话号(各不相同),
	// 生产上它来自 gate_lane::NextIncarnation();conn 给了就挂上(真实回环连接的服务端那一头)。
	static SessionInfo &OpenUnverified(const SessionId id, const SteadyTime openedAt,
									   const muduo::net::TcpConnectionPtr &conn = nullptr)
	{
		SessionInfo &session = AddSession(id);
		session.incarnation = kIncarnationBase + id;
		session.conn = conn;
		OnSessionOpened(id, session, openedAt);
		return session;
	}

	// 会话号 [firstId, lastId] 各开一条未验签会话,都在 openedAt 建连。
	static void OpenUnverifiedRange(const SessionId firstId, const SessionId lastId, const SteadyTime openedAt)
	{
		for (SessionId id = firstId; id <= lastId; ++id)
		{
			OpenUnverified(id, openedAt);
		}
	}

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
	OnSessionOpened(1, session, kT0);
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
	OnSessionOpened(1, session, kT0);
	EXPECT_EQ(GetCounters().accepted, 1u);
	EXPECT_EQ(Unauthenticated(), 0u);
	OnSessionVerified(session, kT0);
	EXPECT_EQ(GetCounters().handshakes, 0u);
}

// 没验签就断开:会话从表里删掉,占位随之归还,不需要任何出口手写减一。
TEST_F(ConnectionAdmission, ErasingAnUnverifiedSessionReturnsItsSlot)
{
	OnSessionOpened(1, AddSession(1), kT0);
	OnSessionOpened(2, AddSession(2), kT0);
	OnSessionOpened(3, AddSession(3), kT0);
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
	OnSessionOpened(1, session, kT0);
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
	OnSessionOpened(1, AddSession(1), kT0);
	ResetForTesting();
	EXPECT_EQ(GetCounters().accepted, 0u);
	EXPECT_EQ(Unauthenticated(), 1u);
	tlsSessionManager.sessions().clear();
	EXPECT_EQ(Unauthenticated(), 0u);
}

// Configure 按上限推导未验签预算。
TEST_F(ConnectionAdmission, ConfigureDerivesThePreAuthBudgetFromTheCap)
{
	Configure(20000, DefaultRateLimit(20000));
	EXPECT_EQ(CurrentPreAuthBudget(), DefaultPreAuthBudget(20000));
	Configure(0, DefaultRateLimit(EffectiveMaxConnections(0)));
	EXPECT_EQ(CurrentPreAuthBudget(), DefaultPreAuthBudget(EffectiveMaxConnections(0)));
}

// 池到起点之后,最老的一条未验签连接建连满 3 秒就给新连接让位:收回它的占位、计一次淘汰;会话留给断开回调删。
TEST_F(ConnectionAdmission, UnderPressureTheOldestUnverifiedConnectionMakesRoom)
{
	Configure(10000, RateLimit{1'000'000, 1'000'000});
	const PreAuthBudget budget = CurrentPreAuthBudget();
	ASSERT_EQ(budget, (PreAuthBudget{500, 1000}));
	SessionInfo &oldest = OpenUnverified(1, kT0);
	OpenUnverifiedRange(2, budget.earlyDropStart, kT0 + seconds(1));
	ASSERT_EQ(Unauthenticated(), budget.earlyDropStart);

	// 最老的一条还差 1ms 满 3 秒:不淘汰;池恰好在起点,早拒概率 0,放行。
	MarkLoopHealthy(kT0 + kMinEvictableAge - milliseconds(1));
	EXPECT_TRUE(AdmitConnection(peer_, kT0 + kMinEvictableAge - milliseconds(1)));
	EXPECT_EQ(GetCounters().preAuthEvicted, 0u);
	ASSERT_TRUE(oldest.preAuthSlot);

	MarkLoopHealthy(kT0 + kMinEvictableAge);
	EXPECT_TRUE(AdmitConnection(peer_, kT0 + kMinEvictableAge));
	EXPECT_EQ(GetCounters().preAuthEvicted, 1u);
	EXPECT_FALSE(oldest.preAuthSlot) << "被淘汰的连接这一刻起不再占未验签池";
	EXPECT_EQ(Unauthenticated(), budget.earlyDropStart - 1);
	EXPECT_TRUE(tlsSessionManager.sessions().contains(1)) << "不在准入回调里删会话,留给断开回调";

	// 池回到起点以下:不再淘汰,下一条最老的(晚 1 秒建连)原样留着。
	EXPECT_TRUE(AdmitConnection(peer_, kT0 + seconds(5)));
	EXPECT_EQ(GetCounters().preAuthEvicted, 1u);
	EXPECT_TRUE(tlsSessionManager.sessions().at(2).preAuthSlot);
	EXPECT_EQ(Rejected(RejectReason::kPreAuthBudget) + Rejected(RejectReason::kPreAuthEarlyDrop), 0u);
}

// 淘汰按 (session_id, 会话代次) 核对:验签过的、断开后会话号被新连接复用的,都不会被当成最老的那一条。
TEST_F(ConnectionAdmission, EvictionPassesOverVerifiedAndReplacedSessions)
{
	Configure(10000, RateLimit{1'000'000, 1'000'000});
	const PreAuthBudget budget = CurrentPreAuthBudget();
	SessionInfo &verifiedLater = OpenUnverified(1, kT0); // 最早建连,后来验签了
	OpenUnverified(2, kT0);                              // 断开了,会话号随后被新连接复用
	SessionInfo &oldestUnverified = OpenUnverified(3, kT0 + seconds(1));
	OpenUnverifiedRange(4, budget.earlyDropStart + 1, kT0 + seconds(2));
	verifiedLater.verified = true; // 同 DispatchTokenVerify:先置已验证,再在成功回包里归还占位
	OnSessionVerified(verifiedLater, kT0 + milliseconds(10));
	tlsSessionManager.sessions().erase(2);
	SessionInfo &reused = AddSession(2);
	reused.incarnation = kIncarnationBase + 1'000'000; // 新连接,新代次
	OnSessionOpened(2, reused, kT0 + seconds(2));
	ASSERT_EQ(Unauthenticated(), budget.earlyDropStart);

	MarkLoopHealthy(kT0 + seconds(4));
	EXPECT_TRUE(AdmitConnection(peer_, kT0 + seconds(4)));
	EXPECT_EQ(GetCounters().preAuthEvicted, 1u);
	EXPECT_FALSE(oldestUnverified.preAuthSlot) << "被淘汰的是仍未验签里最老的那一条";
	EXPECT_TRUE(verifiedLater.verified);
	EXPECT_TRUE(reused.preAuthSlot) << "会话号被复用的新连接不会被旧条目误杀";
}

// 池里全是刚建的连接:到预算一律拒,不消耗速率名额;等最老的满 3 秒再淘汰放行,用的正是留下来的名额。
TEST_F(ConnectionAdmission, AFullPoolOfYoungConnectionsIsRejectedWithoutBurningRateTokens)
{
	Configure(10000, RateLimit{1, 1}); // 速率闸只有一个名额
	const PreAuthBudget budget = CurrentPreAuthBudget();
	OpenUnverifiedRange(1, budget.full, kT0);
	for (int i = 0; i < 100; ++i)
	{
		EXPECT_FALSE(AdmitConnection(peer_, kT0 + seconds(1)));
	}
	EXPECT_EQ(Rejected(RejectReason::kPreAuthBudget), 100u);
	EXPECT_EQ(Rejected(RejectReason::kAcceptRate), 0u);

	MarkLoopHealthy(kT0 + kMinEvictableAge);
	EXPECT_TRUE(AdmitConnection(peer_, kT0 + kMinEvictableAge)) << "唯一的名额没有被预算拒绝用掉";
	EXPECT_EQ(GetCounters().preAuthEvicted, 1u);
	// 名额用完:还有够老的,但速率闸不放行,也就不淘汰。
	EXPECT_FALSE(AdmitConnection(peer_, kT0 + kMinEvictableAge));
	EXPECT_EQ(Rejected(RejectReason::kAcceptRate), 1u);
	EXPECT_EQ(GetCounters().preAuthEvicted, 1u);
	EXPECT_EQ(Unauthenticated(), budget.full - 1);
}

// 早拒用注入的随机源:n = 750(起点 500、预算 1000)时取到 249 拒、取到 250 放,概率恰好 250 / 500。
TEST_F(ConnectionAdmission, EarlyDropUsesTheInjectedRandomSource)
{
	Configure(10000, RateLimit{1'000'000, 1'000'000});
	OpenUnverifiedRange(1, 750, kT0);
	SetEarlyDropRandomForTesting(&FixedDraw);
	gDraw = 249;
	EXPECT_FALSE(AdmitConnection(peer_, kT0 + seconds(1)));
	EXPECT_EQ(Rejected(RejectReason::kPreAuthEarlyDrop), 1u);
	gDraw = 250;
	EXPECT_TRUE(AdmitConnection(peer_, kT0 + seconds(1)));
	EXPECT_EQ(Rejected(RejectReason::kPreAuthEarlyDrop), 1u);
	EXPECT_EQ(GetCounters().preAuthEvicted, 0u) << "早拒带里没有够老的,不淘汰";

	// ResetForTesting 换回缺省的伪随机数:取不到替身。
	ResetForTesting();
	Configure(10000, RateLimit{1'000'000, 1'000'000});
	gDrawCalls = 0;
	(void)AdmitConnection(peer_, kT0 + seconds(1));
	EXPECT_EQ(gDrawCalls, 0u);
}

// 本端 loop 刚卡过时不淘汰:那时「建连 3 秒还没验签」不可信,验签请求可能还在接收缓冲里没读。没有可淘汰的,就落到预算。
TEST_F(ConnectionAdmission, NoEvictionRightAfterTheLoopStalled)
{
	Configure(10000, RateLimit{1'000'000, 1'000'000});
	const PreAuthBudget budget = CurrentPreAuthBudget();
	OpenUnverifiedRange(1, budget.full, kT0);

	// 上一拍在 kT0 + 1s,之后 loop 卡了 4 秒:存活扫描的节拍还没轮到,准入回调先跑。
	MarkLoopHealthy(kT0 + seconds(1));
	EXPECT_FALSE(AdmitConnection(peer_, kT0 + seconds(5)));
	EXPECT_EQ(GetCounters().preAuthEvicted, 0u);
	EXPECT_EQ(Rejected(RejectReason::kPreAuthBudget), 1u);

	// 卡顿之后的第一拍本身迟到:这一拍之后仍不淘汰,积压的验签请求可能还没读完。
	gate_client_heartbeat::SweepLiveness(kT0 + seconds(5));
	EXPECT_FALSE(AdmitConnection(peer_, kT0 + seconds(5)));
	EXPECT_EQ(GetCounters().preAuthEvicted, 0u);

	// 再过一拍、准时:恢复淘汰。
	gate_client_heartbeat::SweepLiveness(kT0 + seconds(6));
	EXPECT_TRUE(AdmitConnection(peer_, kT0 + seconds(6)));
	EXPECT_EQ(GetCounters().preAuthEvicted, 1u);
}

// 按来源限速只在开启且承压时生效:平时同一个 /24 再多连接也不拦;池过了早拒起点之后,超出这个前缀的桶就拒,别的前缀照常。
TEST_F(ConnectionAdmission, PerSourceLimitingAppliesOnlyUnderPressure)
{
	Configure(10000, RateLimit{1'000'000, 1'000'000}, DefaultPreAuthBudget(10000), RateLimit{1, 2});
	ASSERT_EQ(CurrentSourceRateLimit(), (RateLimit{1, 2}));
	// 文档用的公网段(203.0.113.0/24、198.51.100.0/24):只有公网来源按来源限速。
	const muduo::net::InetAddress first("203.0.113.3", 40000);
	for (int i = 0; i < 50; ++i)
	{
		EXPECT_TRUE(AdmitConnection(first, kT0)) << "不承压:不按来源限速";
	}
	EXPECT_EQ(Rejected(RejectReason::kSourceRate), 0u);

	// 池到起点(全是刚建的连接,不淘汰):承压。
	OpenUnverifiedRange(1, CurrentPreAuthBudget().earlyDropStart, kT0);
	MarkLoopHealthy(kT0);
	EXPECT_TRUE(AdmitConnection(first, kT0));
	EXPECT_TRUE(AdmitConnection(muduo::net::InetAddress("203.0.113.200", 40000), kT0));
	EXPECT_FALSE(AdmitConnection(muduo::net::InetAddress("203.0.113.99", 40000), kT0)) << "这个 /24 的桶深 2 用完";
	EXPECT_EQ(Rejected(RejectReason::kSourceRate), 1u);
	EXPECT_TRUE(AdmitConnection(muduo::net::InetAddress("198.51.100.1", 40000), kT0)) << "别的前缀照常";
	EXPECT_TRUE(AdmitConnection(first, kT0 + seconds(1))) << "每秒回一个";
	EXPECT_EQ(GetCounters().sourceUntracked, 0u);
}

// 承压时私网来源也不按来源限速:经代理 / LB / 边缘 nginx 进来的连接在 accept 时只看得到前面那一跳的内网地址,
// 按它限速等于把后面所有玩家当成一个来源(core accept_admission.h IsPublicSourcePrefix)。
TEST_F(ConnectionAdmission, PerSourceLimitingSkipsNonPublicSources)
{
	Configure(10000, RateLimit{1'000'000, 1'000'000}, DefaultPreAuthBudget(10000), RateLimit{1, 1});
	OpenUnverifiedRange(1, CurrentPreAuthBudget().earlyDropStart, kT0);
	MarkLoopHealthy(kT0);
	const muduo::net::InetAddress edgeProxy("10.244.1.17", 40000);
	for (int i = 0; i < 20; ++i)
	{
		EXPECT_TRUE(AdmitConnection(edgeProxy, kT0)) << "承压,但私网来源不按来源限速";
	}
	EXPECT_EQ(Rejected(RejectReason::kSourceRate), 0u);
	EXPECT_TRUE(AdmitConnection(muduo::net::InetAddress("203.0.113.3", 40000), kT0));
	EXPECT_FALSE(AdmitConnection(muduo::net::InetAddress("203.0.113.4", 40000), kT0)) << "公网来源照常限";
	EXPECT_EQ(Rejected(RejectReason::kSourceRate), 1u);
}

// 全局速率闸拒过连接也算承压:之后一个握手期限内按来源限速,超速的前缀在速率闸之前就被拦下、不消耗全局名额。
TEST_F(ConnectionAdmission, AGlobalRateRejectionEngagesPerSourceLimitingForOneHandshakeDeadline)
{
	Configure(10000, RateLimit{1, 1}, DefaultPreAuthBudget(10000), RateLimit{1, 1});
	const muduo::net::InetAddress source("198.51.100.9", 40000);
	EXPECT_TRUE(AdmitConnection(source, kT0)) << "不承压,用掉速率闸唯一的名额";
	EXPECT_FALSE(AdmitConnection(source, kT0));
	EXPECT_EQ(Rejected(RejectReason::kAcceptRate), 1u) << "速率闸拒了:从这一刻起承压";

	// 1 秒后速率闸回了一个名额;这个前缀第一次被按来源问,桶是满的,放行并记一笔。
	EXPECT_TRUE(AdmitConnection(source, kT0 + seconds(1)));
	EXPECT_FALSE(AdmitConnection(source, kT0 + seconds(1)));
	EXPECT_EQ(Rejected(RejectReason::kSourceRate), 1u) << "同一前缀超速,在速率闸之前被拦下";
	EXPECT_EQ(Rejected(RejectReason::kAcceptRate), 1u) << "没有消耗全局名额,也没算成全局超速";

	// 距上一次全局拒绝过了一个握手期限:不再承压,不按来源限速。
	EXPECT_TRUE(AdmitConnection(source, kT0 + seconds(11)));
	EXPECT_EQ(Rejected(RejectReason::kSourceRate), 1u);
}

// 计数原样写进 /metrics:标签只有封闭的 reason,不带对端地址、会话号与玩家号(AGENTS.md §9)。
TEST_F(ConnectionAdmission, CountersAreExportedAsPrometheusMetrics)
{
	const uint64_t reapedBefore = gate_client_heartbeat::GetCounters().handshakeTimeouts;
	Configure(3, RateLimit{50, 60});

	// 三条接纳:一条 40ms 验签,一条 2.5s 验签,一条还没验签。
	ASSERT_TRUE(AdmitConnection(peer_, kT0));
	SessionInfo &fast = AddSession(1);
	OnSessionOpened(1, fast, kT0);
	ASSERT_TRUE(AdmitConnection(peer_, kT0));
	SessionInfo &slow = AddSession(2);
	OnSessionOpened(2, slow, kT0);
	ASSERT_TRUE(AdmitConnection(peer_, kT0));
	OnSessionOpened(3, AddSession(3), kT0);
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
			 std::string("gate_rejected_total{reason=\"preauth_budget\"} 0\n"),
			 std::string("gate_rejected_total{reason=\"preauth_early_drop\"} 0\n"),
			 std::string("gate_rejected_total{reason=\"source_rate\"} 0\n"),
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
			 // 上限 3 时预算跟着速率下限(100 条/秒 × 10 秒),起点高于上限,这一道不起作用。
			 std::string("gate_preauth_evicted_total 0\n"),
			 std::string("gate_preauth_budget 1000\n"),
			 std::string("gate_preauth_early_drop_start 500\n"),
			 std::string("gate_preauth_evictable_age_ms 3000\n"),
			 // 按来源限速缺省关闭:参数为 0,不生效,没有登记的前缀。
			 std::string("gate_source_rate_limit_per_second 0\n"),
			 std::string("gate_source_burst 0\n"),
			 std::string("gate_source_rate_active 0\n"),
			 std::string("gate_source_tracked 0\n"),
			 std::string("gate_source_untracked_total 0\n"),
		 })
	{
		EXPECT_NE(text.find(line), std::string::npos) << "missing sample line: " << line << "in:\n" << text;
	}
	EXPECT_EQ(text.find("peer"), std::string::npos);
	EXPECT_EQ(text.find("session_id"), std::string::npos);
	EXPECT_EQ(text.find("player_id"), std::string::npos);
}

// 淘汰经会话里的 weak 连接 forceClose:被淘汰的那条真实连接关掉,另一条晚建连的未验签连接不受影响;
// 准入回调返回时会话还在表里(由断开回调删,这里的回环服务端没装 gate 的断开回调,所以会话一直留着)。
class ConnectionAdmissionEviction : public ConnectionAdmission
{
protected:
	void SetUp() override
	{
		ConnectionAdmission::SetUp();
		loop_ = std::make_unique<muduo::net::EventLoop>();
	}
	void TearDown() override
	{
		tlsSessionManager.sessions().clear();
		victim_.reset(); // 拆连接要驱动 loop,必须先于 loop_ 析构
		bystander_.reset();
		loop_.reset();
		ConnectionAdmission::TearDown();
	}

	std::unique_ptr<muduo::net::EventLoop> loop_;
	std::unique_ptr<gate_test::LoopbackConnection> victim_;
	std::unique_ptr<gate_test::LoopbackConnection> bystander_;
};

TEST_F(ConnectionAdmissionEviction, EvictionClosesTheVictimsConnectionAndLeavesItsSessionToTheDisconnectCallback)
{
	Configure(10000, RateLimit{1'000'000, 1'000'000});
	victim_ = std::make_unique<gate_test::LoopbackConnection>(*loop_);
	ASSERT_TRUE(victim_->Open("preauth_victim"));
	bystander_ = std::make_unique<gate_test::LoopbackConnection>(*loop_);
	ASSERT_TRUE(bystander_->Open("preauth_bystander"));

	// 最老的未验签连接是 victim;bystander 晚 1 秒建连、同样未验签;其余凑到起点的会话不带连接。
	OpenUnverified(1, kT0, victim_->ServerSide());
	OpenUnverified(2, kT0 + seconds(1), bystander_->ServerSide());
	OpenUnverifiedRange(3, CurrentPreAuthBudget().earlyDropStart, kT0 + seconds(1));

	MarkLoopHealthy(kT0 + kMinEvictableAge);
	EXPECT_TRUE(AdmitConnection(peer_, kT0 + kMinEvictableAge));
	EXPECT_EQ(GetCounters().preAuthEvicted, 1u);
	EXPECT_FALSE(victim_->ServerSideConnected()) << "forceClose 已把连接置成关闭中";
	EXPECT_TRUE(tlsSessionManager.sessions().contains(1)) << "准入回调里不删会话";
	EXPECT_TRUE(bystander_->ServerSideConnected());

	ASSERT_TRUE(gate_test::DriveLoopUntil(*loop_, [this] { return victim_->BothSidesDown(); }));
	EXPECT_TRUE(bystander_->ServerSideConnected()) << "只关最老的那一条";
}

} // namespace
