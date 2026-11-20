// battle 客户端直连的连接准入(client/battle_connection_admission.h;docs/design/turn-based-battle-server.md D27)。
//
// 登记在 cpp/tests/rpc_controller_test/rpc_controller_test.vcxproj(Debug|x64,与其余 battle 用例同一处)。
// 与 gate 共用的速率闸、缺省速率推导、环境变量解析、未验签预算的判定、按来源前缀限速(前缀聚合、只限公网来源、前缀表)
// 与直方图分桶由 rpc_controller_test/accept_admission_test.cpp 与 gate 的 connection_admission_test.cpp 测;这里测 battle 自己的:
// 拒绝原因、判定顺序(含按来源限速与未验签预算)、按原因计数、关源、BATTLE_* 变量、按来源限速只在承压时且只对公网来源生效、
// 淘汰计数、指标页的样本行。时刻与随机数全部注入。
// 「准入回调在 accept 之后、TcpConnection 建出来之前执行,被拒的连接收到 RST」是 muduo 那一层的行为,由
// rpc_controller_test/acceptor_admission_test.cpp 用真实回环连接测;直连面的未认证池(按 connId 有序的集合)与 main.cpp
// 把两者接起来的那几行由 tools/scripts/tests/battle_connection_alerts_contract.tests.ps1 静态核对,冒烟验证。

#include <gtest/gtest.h>

#include <chrono>
#include <cstdint>
#include <optional>
#include <set>
#include <string>
#include <string_view>

#include "client/battle_connection_admission.h"
#include "metrics/prometheus_text.h"
#include "muduo/net/InetAddress.h"

namespace
{

using namespace battle_connection_admission;
using std::chrono::milliseconds;
using std::chrono::seconds;

// 离时钟零点足够远的起点:速率闸把零点当「桶是满的」,用例不该碰巧落在零点上。
const SteadyTime kT0 = SteadyTime{} + std::chrono::hours(1);

uint64_t Rejected(const Admission &admission, const RejectReason reason)
{
    return admission.GetCounters().rejected[static_cast<size_t>(reason)];
}

// 对端地址。缺省取文档用的公网段(203.0.113.0/24):按来源限速只对公网来源生效。用到时才构造,不放在静态初始化里。
muduo::net::InetAddress Peer(const char *ip = "203.0.113.7")
{
    return muduo::net::InetAddress(ip, 40000);
}

// 没有未认证直连时的判定(预算这一道不起作用;按来源限速缺省关闭):只看关源、总量与速率。
std::optional<RejectReason> AdmitPlain(Admission &admission, const size_t current, const size_t max, const SteadyTime now)
{
    return admission.Admit(Peer(), current, max, /*unauthenticated=*/0, std::nullopt, now).rejected;
}

// 按概率早拒的随机源替身。
uint32_t AlwaysDrop(uint32_t)
{
    return 0;
}
uint32_t NeverDrop(const uint32_t bound)
{
    return bound - 1;
}

// ---------------------------------------------------------------------------
// 纯逻辑
// ---------------------------------------------------------------------------

// 原因名是指标标签的取值:每个原因一个,非空、互不相同、只含小写字母与下划线。
TEST(BattleConnectionAdmissionPolicy, EveryRejectReasonHasADistinctLabelValue)
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
    // 与 gate_rejected_total 同义的原因用同一个取值,告警与看板可以按 reason 并列两个节点。
    EXPECT_EQ(RejectReasonName(RejectReason::kCapacity), "capacity");
    EXPECT_EQ(RejectReasonName(RejectReason::kAcceptRate), "accept_rate");
    EXPECT_EQ(RejectReasonName(RejectReason::kShuttingDown), "shutting_down");
    EXPECT_EQ(RejectReasonName(RejectReason::kNoTokenSecret), "no_token_secret");
    EXPECT_EQ(RejectReasonName(RejectReason::kPreAuthBudget), "preauth_budget");
    EXPECT_EQ(RejectReasonName(RejectReason::kPreAuthEarlyDrop), "preauth_early_drop");
    EXPECT_EQ(RejectReasonName(RejectReason::kSourceRate), "source_rate");
}

// BATTLE_* 两个变量:不设按生效上限推导(BattleMaxConnections 4096 → 100 条/秒、桶深 4096),写错时说明里点名的是 battle 的变量。
TEST(BattleConnectionAdmissionPolicy, RateLimitResolvesFromTheBattleVariables)
{
    std::string error;
    EXPECT_EQ(ResolveRateLimit(nullptr, nullptr, 4096, error), (RateLimit{100, 4096}));
    EXPECT_EQ(ResolveRateLimit("800", nullptr, 4096, error), (RateLimit{800, 4096}));
    EXPECT_EQ(ResolveRateLimit("800", "9000", 4096, error), (RateLimit{800, 9000}));
    EXPECT_TRUE(error.empty());

    EXPECT_FALSE(ResolveRateLimit("0", nullptr, 4096, error).has_value()) << "没有「0 = 关闭」";
    EXPECT_NE(error.find(kAcceptRateEnv), std::string::npos) << error;
    EXPECT_NE(error.find("BattleMaxConnections"), std::string::npos) << error;
    EXPECT_EQ(error.find("GATE_"), std::string::npos) << "不能报成 gate 的变量: " << error;
    error.clear();
    EXPECT_FALSE(ResolveRateLimit(nullptr, "-5", 4096, error).has_value());
    EXPECT_NE(error.find(kAcceptBurstEnv), std::string::npos) << error;

    EXPECT_EQ(std::string_view(kAcceptRateEnv), "BATTLE_ACCEPT_RATE_PER_SECOND");
    EXPECT_EQ(std::string_view(kAcceptBurstEnv), "BATTLE_ACCEPT_BURST");
}

// 未验签预算的三个 BATTLE_* 变量:不设时 4096 的上限跟着速率下限(起点 500、预算 1000);写错时点名的是 battle 的变量。
TEST(BattleConnectionAdmissionPolicy, PreAuthBudgetResolvesFromTheBattleVariables)
{
    std::string error;
    EXPECT_EQ(ResolvePreAuthBudget(nullptr, nullptr, nullptr, 4096, error), (PreAuthBudget{500, 1000}));
    EXPECT_EQ(ResolvePreAuthBudget("400", nullptr, "2000", 4096, error), (PreAuthBudget{200, 400, milliseconds(2000)}));
    EXPECT_TRUE(error.empty()) << error;

    EXPECT_FALSE(ResolvePreAuthBudget("400", "401", nullptr, 4096, error).has_value()) << "起点不能大于预算";
    EXPECT_NE(error.find(kPreAuthEarlyDropStartEnv), std::string::npos) << error;
    EXPECT_EQ(error.find("GATE_"), std::string::npos) << "不能报成 gate 的变量: " << error;
    error.clear();
    EXPECT_FALSE(ResolvePreAuthBudget("many", nullptr, nullptr, 4096, error).has_value());
    EXPECT_NE(error.find(kPreAuthBudgetEnv), std::string::npos) << error;

    EXPECT_EQ(std::string_view(kPreAuthBudgetEnv), "BATTLE_PREAUTH_BUDGET");
    EXPECT_EQ(std::string_view(kPreAuthEarlyDropStartEnv), "BATTLE_PREAUTH_EARLY_DROP_START");
    EXPECT_EQ(std::string_view(kPreAuthEvictableAgeEnv), "BATTLE_PREAUTH_EVICTABLE_AGE_MS");
}

// 按来源限速的两个 BATTLE_* 变量:缺省关闭;设了速率才开,桶深缺省是速率 × 握手期限;写错时点名的是 battle 的变量。
TEST(BattleConnectionAdmissionPolicy, PerSourceLimitingResolvesFromTheBattleVariablesAndIsOffByDefault)
{
    std::optional<RateLimit> limit = RateLimit{1, 1};
    std::string error;
    EXPECT_TRUE(ResolveSourceRateLimit(nullptr, nullptr, limit, error));
    EXPECT_FALSE(limit.has_value()) << "不设 = 关闭";
    EXPECT_TRUE(ResolveSourceRateLimit("10", nullptr, limit, error));
    EXPECT_EQ(limit, (RateLimit{10, 100}));
    EXPECT_TRUE(ResolveSourceRateLimit("10", "30", limit, error));
    EXPECT_EQ(limit, (RateLimit{10, 30}));
    EXPECT_TRUE(error.empty()) << error;

    EXPECT_FALSE(ResolveSourceRateLimit(nullptr, "30", limit, error)) << "只设桶深不设速率";
    EXPECT_NE(error.find(kSourceRateEnv), std::string::npos) << error;
    EXPECT_EQ(error.find("GATE_"), std::string::npos) << "不能报成 gate 的变量: " << error;
    error.clear();
    EXPECT_FALSE(ResolveSourceRateLimit("0", nullptr, limit, error)) << "没有「0 = 关闭」,关闭就是不设";

    EXPECT_EQ(std::string_view(kSourceRateEnv), "BATTLE_SOURCE_RATE_PER_SECOND");
    EXPECT_EQ(std::string_view(kSourceBurstEnv), "BATTLE_SOURCE_BURST");
    // BattleMaxConnections 4096 时的推荐值:全局缺省持续速率(下限 100)的 1/10,桶深 10 秒的量。
    EXPECT_EQ(accept_admission::DefaultSourceRateLimit(4096), (RateLimit{10, 100}));
}

// 判定顺序:关源 → 总量 → 按来源 → 未验签预算 → 速率;只有放行才从速率闸取名额。
TEST(BattleConnectionAdmissionPolicy, DecideChecksSourceThenCapacityThenPerSourceThenPreAuthThenRate)
{
    accept_admission::AcceptRateGate gate(RateLimit{10, 1});
    const AdmissionState open{.sourceClosed = false, .currentConnections = 9, .maxConnections = 10};
    AdmissionState closed = open;
    closed.sourceClosed = true;
    closed.sourceOverRate = true;
    AdmissionState full = open;
    full.currentConnections = 10;
    full.sourceOverRate = true;
    AdmissionState overFull = open;
    overFull.currentConnections = 11;
    // 未认证池到了预算,里面全是刚建的直连:没有可以让位的。
    AdmissionState youngPool = open;
    youngPool.unauthenticated = 20;
    youngPool.oldestUnauthenticatedAge = milliseconds(100);
    youngPool.preAuthBudget = PreAuthBudget{10, 20};
    AdmissionState earlyDropBand = youngPool;
    earlyDropBand.unauthenticated = 15;
    // 来源超速排在预算前面:即使池也到了预算,报的是 source_rate。
    AdmissionState sourceOverRate = youngPool;
    sourceOverRate.sourceOverRate = true;

    EXPECT_EQ(Decide(closed, gate, kT0, &AlwaysDrop).rejected, RejectReason::kShuttingDown);
    EXPECT_EQ(Decide(full, gate, kT0, &AlwaysDrop).rejected, RejectReason::kCapacity) << "总量排在按来源前面";
    EXPECT_EQ(Decide(overFull, gate, kT0, &AlwaysDrop).rejected, RejectReason::kCapacity);
    EXPECT_EQ(Decide(sourceOverRate, gate, kT0, &AlwaysDrop).rejected, RejectReason::kSourceRate);
    EXPECT_EQ(Decide(youngPool, gate, kT0, &AlwaysDrop).rejected, RejectReason::kPreAuthBudget);
    EXPECT_EQ(Decide(earlyDropBand, gate, kT0, &AlwaysDrop).rejected, RejectReason::kPreAuthEarlyDrop);
    // 上面几次拒绝都没有动速率闸:唯一的名额还在。
    EXPECT_EQ(Decide(open, gate, kT0, &AlwaysDrop), (Verdict{}));
    EXPECT_EQ(Decide(open, gate, kT0, &AlwaysDrop).rejected, RejectReason::kAcceptRate);
}

// 淘汰只在放行时才执行:速率闸也放行了才带「先淘汰」,速率闸拒了就不淘汰。
TEST(BattleConnectionAdmissionPolicy, EvictionIsRequestedOnlyWhenTheConnectionIsAdmitted)
{
    accept_admission::AcceptRateGate gate(RateLimit{10, 1});
    AdmissionState oldPool{.sourceClosed = false, .currentConnections = 9, .maxConnections = 10};
    oldPool.unauthenticated = 20;
    oldPool.oldestUnauthenticatedAge = seconds(5);
    oldPool.preAuthBudget = PreAuthBudget{10, 20};

    EXPECT_EQ(Decide(oldPool, gate, kT0, &AlwaysDrop), (Verdict{.evictOldestUnauthenticated = true}));
    const Verdict rateLimited = Decide(oldPool, gate, kT0, &AlwaysDrop);
    EXPECT_EQ(rateLimited.rejected, RejectReason::kAcceptRate);
    EXPECT_FALSE(rateLimited.evictOldestUnauthenticated) << "没放行就不淘汰";
}

// ---------------------------------------------------------------------------
// 准入状态与计数
// ---------------------------------------------------------------------------

TEST(BattleConnectionAdmission, AdmitsBelowTheCapAndRejectsAtIt)
{
    Admission admission(RateLimit{1000, 1000});
    EXPECT_FALSE(AdmitPlain(admission, 0, 2, kT0).has_value());
    EXPECT_FALSE(AdmitPlain(admission, 1, 2, kT0).has_value());
    EXPECT_EQ(AdmitPlain(admission, 2, 2, kT0), RejectReason::kCapacity);
    EXPECT_EQ(AdmitPlain(admission, 2, 2, kT0), RejectReason::kCapacity);
    EXPECT_EQ(Rejected(admission, RejectReason::kCapacity), 2u);
    EXPECT_EQ(Rejected(admission, RejectReason::kAcceptRate), 0u);
    // 走掉一条直连,位置立刻腾出来。
    EXPECT_FALSE(AdmitPlain(admission, 1, 2, kT0).has_value());
    // 接纳由建好会话的那一刻计(NoteAccepted),准入放行本身不计:放行之后连接回调还可能因空密钥兜底拒掉。
    EXPECT_EQ(admission.GetCounters().accepted, 0u);
}

TEST(BattleConnectionAdmission, RejectsAboveTheAcceptRateAndRecoversWithTime)
{
    Admission admission(RateLimit{1, 2});
    EXPECT_FALSE(AdmitPlain(admission, 0, 1000, kT0).has_value());
    EXPECT_FALSE(AdmitPlain(admission, 0, 1000, kT0).has_value());
    EXPECT_EQ(AdmitPlain(admission, 0, 1000, kT0), RejectReason::kAcceptRate);
    EXPECT_EQ(Rejected(admission, RejectReason::kAcceptRate), 1u);
    EXPECT_EQ(Rejected(admission, RejectReason::kCapacity), 0u);
    EXPECT_FALSE(AdmitPlain(admission, 0, 1000, kT0 + seconds(1)).has_value()) << "每秒一条:一秒后又有名额";
}

// 满员时被拒的直连不消耗速率名额:腾出位置之后名额还在。
TEST(BattleConnectionAdmission, CapacityRejectionsDoNotBurnRateTokens)
{
    Admission admission(RateLimit{1, 1});
    for (int i = 0; i < 100; ++i)
    {
        EXPECT_EQ(AdmitPlain(admission, 1, 1, kT0), RejectReason::kCapacity);
    }
    EXPECT_EQ(Rejected(admission, RejectReason::kCapacity), 100u);
    EXPECT_FALSE(AdmitPlain(admission, 0, 1, kT0).has_value()) << "唯一的名额没有被满员期间的拒绝用掉";
}

TEST(BattleConnectionAdmission, ClosedSourceRejectsEverything)
{
    Admission admission(RateLimit{1000, 1000});
    EXPECT_FALSE(AdmitPlain(admission, 0, 1000, kT0).has_value());
    EXPECT_FALSE(admission.SourceClosed());
    admission.CloseSource();
    EXPECT_TRUE(admission.SourceClosed());
    EXPECT_EQ(AdmitPlain(admission, 0, 1000, kT0), RejectReason::kShuttingDown);
    EXPECT_EQ(AdmitPlain(admission, 0, 1000, kT0 + seconds(60)), RejectReason::kShuttingDown);
    EXPECT_EQ(Rejected(admission, RejectReason::kShuttingDown), 2u);
}

// 连接建出来之后才被拒(连接回调里的第二道总量闸、空密钥兜底)只计数。
TEST(BattleConnectionAdmission, PostAcceptRejectionsAreCountedByReason)
{
    Admission admission(RateLimit{1000, 1000});
    admission.NoteRejectedAfterAccept(RejectReason::kCapacity);
    admission.NoteRejectedAfterAccept(RejectReason::kNoTokenSecret);
    admission.NoteRejectedAfterAccept(RejectReason::kNoTokenSecret);
    EXPECT_EQ(Rejected(admission, RejectReason::kCapacity), 1u);
    EXPECT_EQ(Rejected(admission, RejectReason::kNoTokenSecret), 2u);
}

// 启动时换上正式参数:新的速率闸参数生效、桶重新装满,未验签预算与按来源限速换成新的。
TEST(BattleConnectionAdmission, ConfigureReplacesTheRateLimitThePreAuthBudgetAndThePerSourceLimit)
{
    Admission admission(RateLimit{1, 1});
    EXPECT_EQ(admission.CurrentPreAuthBudget(), (PreAuthBudget{500, 1000})) << "没 Configure 时取速率下限推导的缺省";
    EXPECT_FALSE(admission.CurrentSourceRateLimit().has_value()) << "按来源限速缺省关闭";
    EXPECT_FALSE(AdmitPlain(admission, 0, 1000, kT0).has_value());
    EXPECT_EQ(AdmitPlain(admission, 0, 1000, kT0), RejectReason::kAcceptRate);

    admission.Configure(RateLimit{50, 3}, PreAuthBudget{20, 40}, RateLimit{5, 50});
    EXPECT_EQ(admission.CurrentRateLimit(), (RateLimit{50, 3}));
    EXPECT_EQ(admission.CurrentPreAuthBudget(), (PreAuthBudget{20, 40}));
    EXPECT_EQ(admission.CurrentSourceRateLimit(), (RateLimit{5, 50}));
    for (int i = 0; i < 3; ++i)
    {
        EXPECT_FALSE(AdmitPlain(admission, 0, 1000, kT0).has_value()) << "第 " << i << " 条";
    }
    EXPECT_EQ(AdmitPlain(admission, 0, 1000, kT0), RejectReason::kAcceptRate);

    admission.Configure(RateLimit{50, 3}, PreAuthBudget{20, 40});
    EXPECT_FALSE(admission.CurrentSourceRateLimit().has_value()) << "不传就是关闭";
}

// 未验签预算:承压时最老的够老就淘汰(调用方淘汰后计一次);全是刚建的就到预算全拒、早拒带里按概率拒。
TEST(BattleConnectionAdmission, UnauthenticatedPoolIsBudgetedLikeTheGate)
{
    Admission admission(RateLimit{1000, 1000});
    admission.Configure(RateLimit{1000, 1000}, PreAuthBudget{2, 4});

    const Verdict evict = admission.Admit(Peer(), 0, 1000, /*unauthenticated=*/2, seconds(3), kT0);
    EXPECT_EQ(evict, (Verdict{.evictOldestUnauthenticated = true}));
    admission.NotePreAuthEvicted();
    EXPECT_EQ(admission.GetCounters().preAuthEvicted, 1u);

    EXPECT_EQ(admission.Admit(Peer(), 0, 1000, 4, milliseconds(100), kT0).rejected, RejectReason::kPreAuthBudget);
    EXPECT_EQ(admission.Admit(Peer(), 0, 1000, 4, std::nullopt, kT0).rejected, RejectReason::kPreAuthBudget)
        << "loop 刚卡过时调用方不给年龄:不淘汰,到预算就拒";

    SetEarlyDropRandomForTesting(&AlwaysDrop);
    EXPECT_EQ(admission.Admit(Peer(), 0, 1000, 3, milliseconds(100), kT0).rejected, RejectReason::kPreAuthEarlyDrop);
    SetEarlyDropRandomForTesting(&NeverDrop);
    EXPECT_FALSE(admission.Admit(Peer(), 0, 1000, 3, milliseconds(100), kT0).rejected.has_value());
    SetEarlyDropRandomForTesting(nullptr);

    EXPECT_EQ(Rejected(admission, RejectReason::kPreAuthBudget), 2u);
    EXPECT_EQ(Rejected(admission, RejectReason::kPreAuthEarlyDrop), 1u);
}

// 按来源限速只在开启且承压时生效、只对公网来源生效:平时同一个 /24 再多直连也不拦;池到了早拒起点之后,
// 超出这个公网前缀的桶就拒,别的前缀照常;私网来源(边缘 nginx、集群内)承压时也不拦。
TEST(BattleConnectionAdmission, PerSourceLimitingAppliesOnlyUnderPressureAndOnlyToPublicSources)
{
    Admission admission(RateLimit{1000, 1000});
    // 预算起点 2、满点 1000:池里两条刚建的就算承压,但离预算很远,预算这一道不拒。
    admission.Configure(RateLimit{1'000'000, 1'000'000}, PreAuthBudget{2, 1000}, RateLimit{1, 2});
    for (int i = 0; i < 50; ++i)
    {
        EXPECT_FALSE(admission.Admit(Peer("203.0.113.3"), 0, 4096, 0, std::nullopt, kT0).rejected.has_value())
            << "不承压:不按来源限速";
    }
    EXPECT_EQ(Rejected(admission, RejectReason::kSourceRate), 0u);

    // 池到起点(全是刚建的,不淘汰):承压。年龄给 100ms,不够淘汰。
    const auto underPressure = [&](const char *ip, const SteadyTime now)
    { return admission.Admit(Peer(ip), 0, 4096, /*unauthenticated=*/2, milliseconds(100), now).rejected; };
    EXPECT_FALSE(underPressure("203.0.113.3", kT0).has_value());
    EXPECT_FALSE(underPressure("203.0.113.200", kT0).has_value());
    EXPECT_EQ(underPressure("203.0.113.99", kT0), RejectReason::kSourceRate) << "这个 /24 的桶深 2 用完";
    EXPECT_FALSE(underPressure("198.51.100.1", kT0).has_value()) << "别的前缀照常";
    for (int i = 0; i < 20; ++i)
    {
        EXPECT_FALSE(underPressure("10.244.1.17", kT0).has_value()) << "私网来源(边缘 nginx 的 Pod 地址)不按来源限速";
    }
    EXPECT_FALSE(underPressure("203.0.113.3", kT0 + seconds(1)).has_value()) << "每秒回一个";
    EXPECT_EQ(Rejected(admission, RejectReason::kSourceRate), 1u);
    EXPECT_EQ(admission.GetCounters().sourceUntracked, 0u);
}

// 全局速率闸拒过直连也算承压:之后一个握手期限内按来源限速,超速的前缀在速率闸之前就被拦下、不消耗全局名额。
TEST(BattleConnectionAdmission, AGlobalRateRejectionEngagesPerSourceLimitingForOneHandshakeDeadline)
{
    Admission admission(RateLimit{1, 1});
    admission.Configure(RateLimit{1, 1}, PreAuthBudget{500, 1000}, RateLimit{1, 1});
    const auto admit = [&](const SteadyTime now)
    { return admission.Admit(Peer("198.51.100.9"), 0, 4096, 0, std::nullopt, now).rejected; };
    EXPECT_FALSE(admit(kT0).has_value()) << "不承压,用掉速率闸唯一的名额";
    EXPECT_EQ(admit(kT0), RejectReason::kAcceptRate) << "速率闸拒了:从这一刻起承压";

    // 1 秒后速率闸回了一个名额;这个前缀第一次被按来源问,桶是满的,放行并记一笔。
    EXPECT_FALSE(admit(kT0 + seconds(1)).has_value());
    EXPECT_EQ(admit(kT0 + seconds(1)), RejectReason::kSourceRate) << "同一前缀超速,在速率闸之前被拦下";
    EXPECT_EQ(Rejected(admission, RejectReason::kAcceptRate), 1u) << "没有消耗全局名额,也没算成全局超速";

    // 距上一次全局拒绝过了一个握手期限:不再承压,不按来源限速。
    EXPECT_FALSE(admit(kT0 + seconds(11)).has_value());
    EXPECT_EQ(Rejected(admission, RejectReason::kSourceRate), 1u);
}

TEST(BattleConnectionAdmission, HandshakesAndHandshakeTimeoutsAreCounted)
{
    Admission admission(RateLimit{1000, 1000});
    admission.NoteAccepted();
    admission.NoteAccepted();
    admission.NoteHandshake(kT0, kT0 + milliseconds(40));
    admission.NoteHandshakeTimeout();
    EXPECT_EQ(admission.GetCounters().accepted, 2u);
    EXPECT_EQ(admission.GetCounters().handshakes, 1u);
    EXPECT_EQ(admission.GetCounters().handshakeTimeouts, 1u);
}

// 计数原样写进 /metrics:标签只有封闭的 reason 与 result,不带对端地址、房间号与玩家号(AGENTS.md §9)。
TEST(BattleConnectionAdmission, CountersAreExportedAsPrometheusMetrics)
{
    Admission admission(RateLimit{50, 60});
    // 三条接纳:一条 40ms 握手,一条 2.5s 握手,一条被握手期限关掉。
    for (int i = 0; i < 3; ++i)
    {
        ASSERT_FALSE(AdmitPlain(admission, static_cast<size_t>(i), 3, kT0).has_value());
        admission.NoteAccepted();
    }
    admission.NoteHandshake(kT0, kT0 + milliseconds(40));
    admission.NoteHandshake(kT0, kT0 + milliseconds(2500));
    admission.NoteHandshakeTimeout();
    // 满员之后两条被拒;关源之后再一条;连接回调里的空密钥兜底一条。
    EXPECT_EQ(AdmitPlain(admission, 3, 3, kT0), RejectReason::kCapacity);
    EXPECT_EQ(AdmitPlain(admission, 3, 3, kT0), RejectReason::kCapacity);
    admission.CloseSource();
    EXPECT_EQ(AdmitPlain(admission, 2, 3, kT0), RejectReason::kShuttingDown);
    admission.NoteRejectedAfterAccept(RejectReason::kNoTokenSecret);
    admission.NotePreAuthEvicted();

    prometheus_text::Writer writer;
    admission.CollectMetrics(writer, /*currentConnections=*/2, /*maxConnections=*/3, /*unauthenticatedConnections=*/1);
    const std::string &text = writer.Text();
    EXPECT_EQ(writer.DroppedSamples(), 0u) << text;

    for (const std::string &line : {
             std::string("battle_connections_current 2\n"),
             std::string("battle_connections_max 3\n"),
             std::string("battle_unauthenticated_connections 1\n"),
             std::string("battle_accepted_total 3\n"),
             std::string("battle_rejected_total{reason=\"capacity\"} 2\n"),
             std::string("battle_rejected_total{reason=\"accept_rate\"} 0\n"),
             std::string("battle_rejected_total{reason=\"shutting_down\"} 1\n"),
             std::string("battle_rejected_total{reason=\"no_token_secret\"} 1\n"),
             std::string("battle_rejected_total{reason=\"preauth_budget\"} 0\n"),
             std::string("battle_rejected_total{reason=\"preauth_early_drop\"} 0\n"),
             std::string("battle_rejected_total{reason=\"source_rate\"} 0\n"),
             std::string("battle_accept_rate_limit_per_second 50\n"),
             std::string("battle_accept_burst 60\n"),
             std::string("# TYPE battle_handshake_duration_ms histogram\n"),
             std::string("battle_handshake_duration_ms_bucket{le=\"25\"} 0\n"),
             std::string("battle_handshake_duration_ms_bucket{le=\"50\"} 1\n"),
             std::string("battle_handshake_duration_ms_bucket{le=\"2000\"} 1\n"),
             std::string("battle_handshake_duration_ms_bucket{le=\"3000\"} 2\n"),
             std::string("battle_handshake_duration_ms_bucket{le=\"+Inf\"} 2\n"),
             std::string("battle_handshake_duration_ms_sum 2540\n"),
             std::string("battle_handshake_duration_ms_count 2\n"),
             std::string("battle_preauth_reaped_total 1\n"),
             std::string("battle_preauth_evicted_total 1\n"),
             std::string("battle_preauth_budget 1000\n"),
             std::string("battle_preauth_early_drop_start 500\n"),
             std::string("battle_preauth_evictable_age_ms 3000\n"),
             std::string("battle_source_rate_limit_per_second 0\n"),
             std::string("battle_source_burst 0\n"),
             std::string("battle_source_rate_active 0\n"),
             std::string("battle_source_tracked 0\n"),
             std::string("battle_source_untracked_total 0\n"),
         })
    {
        EXPECT_NE(text.find(line), std::string::npos) << "missing sample line: " << line << "in:\n" << text;
    }
    EXPECT_EQ(text.find("peer"), std::string::npos);
    EXPECT_EQ(text.find("battle_id"), std::string::npos);
    EXPECT_EQ(text.find("player_id"), std::string::npos);
}

} // namespace
