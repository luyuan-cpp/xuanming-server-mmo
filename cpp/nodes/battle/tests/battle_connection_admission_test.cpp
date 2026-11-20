// battle 客户端直连的连接准入(client/battle_connection_admission.h;docs/design/turn-based-battle-server.md D27)。
//
// 登记在 cpp/tests/rpc_controller_test/rpc_controller_test.vcxproj(Debug|x64,与其余 battle 用例同一处)。
// 与 gate 共用的速率闸、缺省速率推导、环境变量解析、未验签预算的判定与直方图分桶由 rpc_controller_test/accept_admission_test.cpp
// 与 gate 的 connection_admission_test.cpp 测;这里测 battle 自己的:拒绝原因、判定顺序(含未验签预算)、按原因计数、关源、
// BATTLE_* 变量、扫会话表时挑最老的未认证直连、淘汰计数、指标页的样本行。时刻与随机数全部注入。
// 「准入回调在 accept 之后、TcpConnection 建出来之前执行,被拒的连接收到 RST」是 muduo 那一层的行为,由
// rpc_controller_test/acceptor_admission_test.cpp 用真实回环连接测;直连面与 main.cpp 把两者接起来的那几行由
// tools/scripts/tests/battle_connection_alerts_contract.tests.ps1 静态核对,冒烟验证。

#include <gtest/gtest.h>

#include <chrono>
#include <cstdint>
#include <optional>
#include <set>
#include <string>
#include <string_view>

#include "client/battle_connection_admission.h"
#include "metrics/prometheus_text.h"

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

// 没有未认证直连时的判定(预算这一道不起作用):只看关源、总量与速率。
std::optional<RejectReason> AdmitPlain(Admission &admission, const size_t current, const size_t max, const SteadyTime now)
{
    return admission.Admit(current, max, /*unauthenticated=*/0, std::nullopt, now).rejected;
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

// 判定顺序:关源 → 总量 → 未验签预算 → 速率;只有放行才从速率闸取名额。
TEST(BattleConnectionAdmissionPolicy, DecideChecksSourceThenCapacityThenPreAuthThenRate)
{
    accept_admission::AcceptRateGate gate(RateLimit{10, 1});
    const AdmissionState open{.sourceClosed = false, .currentConnections = 9, .maxConnections = 10};
    AdmissionState closed = open;
    closed.sourceClosed = true;
    AdmissionState full = open;
    full.currentConnections = 10;
    AdmissionState overFull = open;
    overFull.currentConnections = 11;
    // 未认证池到了预算,里面全是刚建的直连:没有可以让位的。
    AdmissionState youngPool = open;
    youngPool.unauthenticated = 20;
    youngPool.oldestUnauthenticatedAge = milliseconds(100);
    youngPool.preAuthBudget = PreAuthBudget{10, 20};
    AdmissionState earlyDropBand = youngPool;
    earlyDropBand.unauthenticated = 15;

    EXPECT_EQ(Decide(closed, gate, kT0, &AlwaysDrop).rejected, RejectReason::kShuttingDown);
    EXPECT_EQ(Decide(full, gate, kT0, &AlwaysDrop).rejected, RejectReason::kCapacity);
    EXPECT_EQ(Decide(overFull, gate, kT0, &AlwaysDrop).rejected, RejectReason::kCapacity);
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

// 扫会话表的累计:数清条数,挑出建连最早的那一条(顺序任意)。
TEST(BattleConnectionAdmissionPolicy, UnauthenticatedScanCountsAndFindsTheOldest)
{
    UnauthenticatedScan empty;
    EXPECT_EQ(empty.count, 0u);
    EXPECT_FALSE(empty.oldestConnId.has_value());

    UnauthenticatedScan scan;
    scan.Add(7, kT0 + seconds(3));
    scan.Add(3, kT0 + seconds(1));
    scan.Add(9, kT0 + seconds(2));
    EXPECT_EQ(scan.count, 3u);
    ASSERT_TRUE(scan.oldestConnId.has_value());
    EXPECT_EQ(*scan.oldestConnId, 3u);
    EXPECT_EQ(scan.oldestOpenedAt, kT0 + seconds(1));
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

// 启动时换上正式参数:新的速率闸参数生效、桶重新装满,未验签预算换成新的。
TEST(BattleConnectionAdmission, ConfigureReplacesTheRateLimitAndThePreAuthBudget)
{
    Admission admission(RateLimit{1, 1});
    EXPECT_EQ(admission.CurrentPreAuthBudget(), (PreAuthBudget{500, 1000})) << "没 Configure 时取速率下限推导的缺省";
    EXPECT_FALSE(AdmitPlain(admission, 0, 1000, kT0).has_value());
    EXPECT_EQ(AdmitPlain(admission, 0, 1000, kT0), RejectReason::kAcceptRate);

    admission.Configure(RateLimit{50, 3}, PreAuthBudget{20, 40});
    EXPECT_EQ(admission.CurrentRateLimit(), (RateLimit{50, 3}));
    EXPECT_EQ(admission.CurrentPreAuthBudget(), (PreAuthBudget{20, 40}));
    for (int i = 0; i < 3; ++i)
    {
        EXPECT_FALSE(AdmitPlain(admission, 0, 1000, kT0).has_value()) << "第 " << i << " 条";
    }
    EXPECT_EQ(AdmitPlain(admission, 0, 1000, kT0), RejectReason::kAcceptRate);
}

// 未验签预算:承压时最老的够老就淘汰(调用方淘汰后计一次);全是刚建的就到预算全拒、早拒带里按概率拒。
TEST(BattleConnectionAdmission, UnauthenticatedPoolIsBudgetedLikeTheGate)
{
    Admission admission(RateLimit{1000, 1000});
    admission.Configure(RateLimit{1000, 1000}, PreAuthBudget{2, 4});

    const Verdict evict = admission.Admit(0, 1000, /*unauthenticated=*/2, seconds(3), kT0);
    EXPECT_EQ(evict, (Verdict{.evictOldestUnauthenticated = true}));
    admission.NotePreAuthEvicted();
    EXPECT_EQ(admission.GetCounters().preAuthEvicted, 1u);

    EXPECT_EQ(admission.Admit(0, 1000, 4, milliseconds(100), kT0).rejected, RejectReason::kPreAuthBudget);
    EXPECT_EQ(admission.Admit(0, 1000, 4, std::nullopt, kT0).rejected, RejectReason::kPreAuthBudget)
        << "loop 刚卡过时调用方不给年龄:不淘汰,到预算就拒";

    SetEarlyDropRandomForTesting(&AlwaysDrop);
    EXPECT_EQ(admission.Admit(0, 1000, 3, milliseconds(100), kT0).rejected, RejectReason::kPreAuthEarlyDrop);
    SetEarlyDropRandomForTesting(&NeverDrop);
    EXPECT_FALSE(admission.Admit(0, 1000, 3, milliseconds(100), kT0).rejected.has_value());
    SetEarlyDropRandomForTesting(nullptr);

    EXPECT_EQ(Rejected(admission, RejectReason::kPreAuthBudget), 2u);
    EXPECT_EQ(Rejected(admission, RejectReason::kPreAuthEarlyDrop), 1u);
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

// 计数原样写进 /metrics:标签只有封闭的 reason,不带对端地址、房间号与玩家号(AGENTS.md §9)。
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
         })
    {
        EXPECT_NE(text.find(line), std::string::npos) << "missing sample line: " << line << "in:\n" << text;
    }
    EXPECT_EQ(text.find("peer"), std::string::npos);
    EXPECT_EQ(text.find("battle_id"), std::string::npos);
    EXPECT_EQ(text.find("player_id"), std::string::npos);
}

} // namespace
