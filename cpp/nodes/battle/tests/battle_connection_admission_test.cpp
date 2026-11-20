// battle 客户端直连的连接准入(client/battle_connection_admission.h;docs/design/turn-based-battle-server.md D27)。
//
// 登记在 cpp/tests/rpc_controller_test/rpc_controller_test.vcxproj(Debug|x64,与其余 battle 用例同一处)。
// 与 gate 共用的速率闸、缺省速率推导、环境变量解析与直方图分桶由 rpc_controller_test/accept_admission_test.cpp 测;
// 这里测 battle 自己的:拒绝原因、判定顺序、按原因计数、关源、BATTLE_* 两个变量、指标页的样本行。时刻全部注入。
// 「准入回调在 accept 之后、TcpConnection 建出来之前执行,被拒的连接收到 RST」是 muduo 那一层的行为,由
// rpc_controller_test/acceptor_admission_test.cpp 用真实回环连接测;直连面与 main.cpp 把两者接起来的那几行由
// tools/scripts/tests/battle_connection_alerts_contract.tests.ps1 静态核对,冒烟验证。

#include <gtest/gtest.h>

#include <chrono>
#include <cstdint>
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

// 判定顺序:关源 → 总量 → 速率;只有放行才从速率闸取名额。
TEST(BattleConnectionAdmissionPolicy, DecideChecksSourceThenCapacityThenRate)
{
    accept_admission::AcceptRateGate gate(RateLimit{10, 1});

    EXPECT_EQ(Decide(AdmissionState{.sourceClosed = true, .currentConnections = 0, .maxConnections = 10}, gate, kT0),
              RejectReason::kShuttingDown);
    EXPECT_EQ(Decide(AdmissionState{.sourceClosed = false, .currentConnections = 10, .maxConnections = 10}, gate, kT0),
              RejectReason::kCapacity);
    EXPECT_EQ(Decide(AdmissionState{.sourceClosed = false, .currentConnections = 11, .maxConnections = 10}, gate, kT0),
              RejectReason::kCapacity);
    // 上面三次拒绝都没有动速率闸:唯一的名额还在。
    EXPECT_FALSE(
        Decide(AdmissionState{.sourceClosed = false, .currentConnections = 9, .maxConnections = 10}, gate, kT0).has_value());
    EXPECT_EQ(Decide(AdmissionState{.sourceClosed = false, .currentConnections = 9, .maxConnections = 10}, gate, kT0),
              RejectReason::kAcceptRate);
}

// ---------------------------------------------------------------------------
// 准入状态与计数
// ---------------------------------------------------------------------------

TEST(BattleConnectionAdmission, AdmitsBelowTheCapAndRejectsAtIt)
{
    Admission admission(RateLimit{1000, 1000});
    EXPECT_FALSE(admission.Admit(0, 2, kT0).has_value());
    EXPECT_FALSE(admission.Admit(1, 2, kT0).has_value());
    EXPECT_EQ(admission.Admit(2, 2, kT0), RejectReason::kCapacity);
    EXPECT_EQ(admission.Admit(2, 2, kT0), RejectReason::kCapacity);
    EXPECT_EQ(Rejected(admission, RejectReason::kCapacity), 2u);
    EXPECT_EQ(Rejected(admission, RejectReason::kAcceptRate), 0u);
    // 走掉一条直连,位置立刻腾出来。
    EXPECT_FALSE(admission.Admit(1, 2, kT0).has_value());
    // 接纳由建好会话的那一刻计(NoteAccepted),准入放行本身不计:放行之后连接回调还可能因空密钥兜底拒掉。
    EXPECT_EQ(admission.GetCounters().accepted, 0u);
}

TEST(BattleConnectionAdmission, RejectsAboveTheAcceptRateAndRecoversWithTime)
{
    Admission admission(RateLimit{1, 2});
    EXPECT_FALSE(admission.Admit(0, 1000, kT0).has_value());
    EXPECT_FALSE(admission.Admit(0, 1000, kT0).has_value());
    EXPECT_EQ(admission.Admit(0, 1000, kT0), RejectReason::kAcceptRate);
    EXPECT_EQ(Rejected(admission, RejectReason::kAcceptRate), 1u);
    EXPECT_EQ(Rejected(admission, RejectReason::kCapacity), 0u);
    EXPECT_FALSE(admission.Admit(0, 1000, kT0 + seconds(1)).has_value()) << "每秒一条:一秒后又有名额";
}

// 满员时被拒的直连不消耗速率名额:腾出位置之后名额还在。
TEST(BattleConnectionAdmission, CapacityRejectionsDoNotBurnRateTokens)
{
    Admission admission(RateLimit{1, 1});
    for (int i = 0; i < 100; ++i)
    {
        EXPECT_EQ(admission.Admit(1, 1, kT0), RejectReason::kCapacity);
    }
    EXPECT_EQ(Rejected(admission, RejectReason::kCapacity), 100u);
    EXPECT_FALSE(admission.Admit(0, 1, kT0).has_value()) << "唯一的名额没有被满员期间的拒绝用掉";
}

TEST(BattleConnectionAdmission, ClosedSourceRejectsEverything)
{
    Admission admission(RateLimit{1000, 1000});
    EXPECT_FALSE(admission.Admit(0, 1000, kT0).has_value());
    EXPECT_FALSE(admission.SourceClosed());
    admission.CloseSource();
    EXPECT_TRUE(admission.SourceClosed());
    EXPECT_EQ(admission.Admit(0, 1000, kT0), RejectReason::kShuttingDown);
    EXPECT_EQ(admission.Admit(0, 1000, kT0 + seconds(60)), RejectReason::kShuttingDown);
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

// 启动时换上正式参数:新参数生效,桶重新装满。
TEST(BattleConnectionAdmission, ConfigureReplacesTheRateLimitAndRefillsTheBucket)
{
    Admission admission(RateLimit{1, 1});
    EXPECT_FALSE(admission.Admit(0, 1000, kT0).has_value());
    EXPECT_EQ(admission.Admit(0, 1000, kT0), RejectReason::kAcceptRate);

    admission.Configure(RateLimit{50, 3});
    EXPECT_EQ(admission.CurrentRateLimit(), (RateLimit{50, 3}));
    for (int i = 0; i < 3; ++i)
    {
        EXPECT_FALSE(admission.Admit(0, 1000, kT0).has_value()) << "第 " << i << " 条";
    }
    EXPECT_EQ(admission.Admit(0, 1000, kT0), RejectReason::kAcceptRate);
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
        ASSERT_FALSE(admission.Admit(static_cast<size_t>(i), 3, kT0).has_value());
        admission.NoteAccepted();
    }
    admission.NoteHandshake(kT0, kT0 + milliseconds(40));
    admission.NoteHandshake(kT0, kT0 + milliseconds(2500));
    admission.NoteHandshakeTimeout();
    // 满员之后两条被拒;关源之后再一条;连接回调里的空密钥兜底一条。
    EXPECT_EQ(admission.Admit(3, 3, kT0), RejectReason::kCapacity);
    EXPECT_EQ(admission.Admit(3, 3, kT0), RejectReason::kCapacity);
    admission.CloseSource();
    EXPECT_EQ(admission.Admit(2, 3, kT0), RejectReason::kShuttingDown);
    admission.NoteRejectedAfterAccept(RejectReason::kNoTokenSecret);

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
         })
    {
        EXPECT_NE(text.find(line), std::string::npos) << "missing sample line: " << line << "in:\n" << text;
    }
    EXPECT_EQ(text.find("peer"), std::string::npos);
    EXPECT_EQ(text.find("battle_id"), std::string::npos);
    EXPECT_EQ(text.find("player_id"), std::string::npos);
}

} // namespace
