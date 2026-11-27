#pragma once

// battle 客户端直连面的连接准入(docs/design/turn-based-battle-server.md D27;与 gate 同一套做法,
// docs/design/gate-connection-admission-control.md P1-1 ~ P1-4)。
//
// 判定发生在 muduo Acceptor 的准入回调里:accept 成功之后、TcpConnection 建出来之前。被拒的连接当场以 RST 关闭,
// battle 不为它建任何对象,内核也不留 TIME_WAIT。判据都不需要知道对端是谁,按顺序:
//   1. 关源:停机 / 维护关源之后不再接新连接;
//   2. 总量:直连数(会话 + 未认证)到上限(battle_max_connections;dev / test 的 0 = BattleClientEdge::kHardMaxConnections);
//   3. 按来源限速(缺省关闭,BATTLE_SOURCE_RATE_PER_SECOND 打开;只在承压时、只对公网源地址生效):单个来源前缀超过它的桶就拒;
//   4. 未验签预算:既没完成票据握手、也没出示探测授权的直连单独一个池,承压时最老的先让位,腾不出位置时按概率早拒、
//      到预算全拒(规则与 gate 同一份,accept_admission.h 的 PreAuthBudget / DecidePreAuth);
//   5. 速率:新建连接的全局令牌桶,挡住「不停建连又立刻关」这种总量闸永远碰不到的流量。
// 前几道都排在速率前面:被它们拒掉的连接不消耗速率名额。顺序与 gate 的 Decide 相同。
//
// 速率闸、缺省速率的推导、环境变量解析、未验签预算、按来源前缀限速与握手耗时直方图和 gate 是同一份
// (core network/accept_admission.h);拒绝原因、计数与指标名是 battle 自己的。本文件只管判定、计数与写指标,不认识会话表:
// 当前直连数、生效上限、未认证直连数与其中最老一条的年龄都由 BattleClientEdge 在调用时给,时刻也由调用方给(单调时钟,单测注入)。
// 单测:cpp/nodes/battle/tests/battle_connection_admission_test.cpp。
// 线程模型:只在 battle 主 loop 线程上用(与 BattleClientEdge 同线程),不加锁。

#include <algorithm>
#include <array>
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <optional>
#include <string>
#include <string_view>

#include "metrics/prometheus_text.h"
#include "network/accept_admission.h"

namespace battle_connection_admission
{

using accept_admission::DecidePreAuth;
using accept_admission::DefaultPreAuthBudget;
using accept_admission::PreAuthBudget;
using accept_admission::PreAuthVerdict;
using accept_admission::RateLimit;
using accept_admission::SourcePrefix;
using accept_admission::SteadyTime;
using accept_admission::UniformBelow;

// 一条直连被拒绝的原因。指标 battle_rejected_total{reason} 的标签取值,封闭集合(AGENTS.md §9:标签只放低基数值);
// 采样日志里的 reason 也用这几个名字。与 gate 的同名原因同义,告警与看板可以按 reason 并列两个节点。
enum class RejectReason : uint8_t
{
    kCapacity,      // 直连数(会话 + 未认证)到上限
    kAcceptRate,    // 新建连接速率超过令牌桶
    kShuttingDown,  // 已关源(停机 / 维护),不再接新连接
    kNoTokenSecret, // prod 下 battle_token_secret 为空(纵深防御,启动门禁已拒启)
    kPreAuthBudget,    // 未认证直连到预算,且没有建连够久、可以让位的
    kPreAuthEarlyDrop, // 未认证直连过了早拒起点、没有可以让位的,按概率拒了
    kSourceRate,       // 按来源前缀限速生效中(承压),这个公网前缀的新建直连超过了它的桶
    kCount,
};
inline constexpr size_t kRejectReasonCount = static_cast<size_t>(RejectReason::kCount);
inline constexpr auto kRejectReasonNames = std::to_array<std::string_view>({
    "capacity",
    "accept_rate",
    "shutting_down",
    "no_token_secret",
    "preauth_budget",
    "preauth_early_drop",
    "source_rate",
});
static_assert(kRejectReasonNames.size() == kRejectReasonCount, "one name per RejectReason");

constexpr std::string_view RejectReasonName(RejectReason reason)
{
    return kRejectReasonNames[static_cast<size_t>(reason)];
}

// 运维覆盖(不设 = 按 accept_admission::DefaultRateLimit 由生效上限推导;BattleMaxConnections 4096 时是 100 条/秒、桶深 4096):
//   BATTLE_ACCEPT_RATE_PER_SECOND  持续速率,规范十进制 1..accept_admission::kMaxAcceptRatePerSecond;
//   BATTLE_ACCEPT_BURST            桶深,规范十进制 1..accept_admission::kMaxAcceptBurst;不设时取 max(连接上限, 持续速率)。
// 写错拒绝启动,没有「0 = 关闭」。与 gate 的两个变量分开,同机起 gate 与 battle 时各调各的。
// 全服匹配的网络探测也连这个端口(match 按玩家均摊在本机房可探测的 battle 上,机房里只有一台时全落在它上面,见设计稿 D27),
// 探测量大时要先调高持续速率。
inline constexpr const char *kAcceptRateEnv = "BATTLE_ACCEPT_RATE_PER_SECOND";
inline constexpr const char *kAcceptBurstEnv = "BATTLE_ACCEPT_BURST";

// 由两个环境变量的原文与生效上限定出速率闸参数。任一变量非法时返回 nullopt,error 里是给启动日志用的英文说明。
inline std::optional<RateLimit> ResolveRateLimit(const char *rawRate, const char *rawBurst,
                                                 uint32_t effectiveMaxConnections, std::string &error)
{
    return accept_admission::ResolveRateLimit({kAcceptRateEnv, kAcceptBurstEnv, "BattleMaxConnections"}, rawRate,
                                              rawBurst, effectiveMaxConnections, error);
}

// 未验签预算的运维覆盖(不设 = accept_admission::DefaultPreAuthBudget 由生效上限推导;BattleMaxConnections 4096 时
// 早拒起点 500、预算 1000,即跟着速率下限;取值范围见 accept_admission::PreAuthEnvNames):
//   BATTLE_PREAUTH_BUDGET / BATTLE_PREAUTH_EARLY_DROP_START / BATTLE_PREAUTH_EVICTABLE_AGE_MS。写错拒绝启动。
// 出示过探测授权的连接不在池里,探测量再大也不占预算。
inline constexpr const char *kPreAuthBudgetEnv = "BATTLE_PREAUTH_BUDGET";
inline constexpr const char *kPreAuthEarlyDropStartEnv = "BATTLE_PREAUTH_EARLY_DROP_START";
inline constexpr const char *kPreAuthEvictableAgeEnv = "BATTLE_PREAUTH_EVICTABLE_AGE_MS";

// 由三个环境变量的原文与生效上限定出未验签预算。任一变量非法时返回 nullopt,error 里是给启动日志用的英文说明。
inline std::optional<PreAuthBudget> ResolvePreAuthBudget(const char *rawBudget, const char *rawEarlyDropStart,
                                                         const char *rawEvictableAgeMs, uint32_t effectiveMaxConnections,
                                                         std::string &error)
{
    return accept_admission::ResolvePreAuthBudget(
        {kPreAuthBudgetEnv, kPreAuthEarlyDropStartEnv, kPreAuthEvictableAgeEnv}, rawBudget, rawEarlyDropStart,
        rawEvictableAgeMs, effectiveMaxConnections, error);
}

// 按来源限速的开关与参数(缺省关闭,取值范围见 accept_admission::SourceRateEnvNames;推荐值见 DefaultSourceRateLimit,
// BattleMaxConnections 4096 时是每个前缀 10 条/秒、桶深 100,启动日志会打出来):
//   BATTLE_SOURCE_RATE_PER_SECOND  设了就开启:单个前缀的持续速率;
//   BATTLE_SOURCE_BURST            单个前缀的桶深;不设时取持续速率 × 握手期限。
// 只设桶深不设速率、或写错都拒绝启动。battle 的 hostPort / Agones 端口映射保源地址(D89),所以直连看得到玩家的公网地址;
// 小游戏经边缘 nginx 进来的 WebSocket 直连只看得到边缘的内网地址,按规则不限(那一段的按来源限速在边缘,infra-up 的
// -WsEdgeSourceRatePerSecond)。
inline constexpr const char *kSourceRateEnv = "BATTLE_SOURCE_RATE_PER_SECOND";
inline constexpr const char *kSourceBurstEnv = "BATTLE_SOURCE_BURST";

// 解析两个变量。成功返回 true:limit 为空表示关闭,否则是开启后的参数;失败返回 false,error 里是给启动日志用的英文说明。
inline bool ResolveSourceRateLimit(const char *rawRate, const char *rawBurst, std::optional<RateLimit> &limit,
                                   std::string &error)
{
    return accept_admission::ResolveSourceRateLimit({kSourceRateEnv, kSourceBurstEnv}, rawRate, rawBurst, limit, error);
}

// 判定所需的当前状态。
struct AdmissionState
{
    bool sourceClosed{false};     // 已关源
    size_t currentConnections{0}; // 直连数 = 会话数 + 未认证直连数(票据先行:握手之前没有会话)。都在 loop 线程上同步增删
    size_t maxConnections{0};     // 生效上限(BattleClientEdge::EffectiveMaxConnections)
    uint64_t unauthenticated{0};  // 未认证直连数(没完成票据握手、也没出示探测授权)
    // 其中最老一条已建连多久。没有未认证直连、或此刻不允许淘汰(本端 loop 刚卡过)时为空。
    std::optional<SteadyTime::duration> oldestUnauthenticatedAge;
    PreAuthBudget preAuthBudget{DefaultPreAuthBudget(0)};
    bool sourceOverRate{false}; // 按来源限速生效中(开启且承压),而这条直连的公网来源前缀已超过它的桶
};

// 判定结果:拒绝(带原因),或放行;放行时可能要先淘汰最老的一条未认证直连给它让位。
struct Verdict
{
    std::optional<RejectReason> rejected;    // 空 = 放行
    bool evictOldestUnauthenticated{false}; // 放行,并且要先淘汰最老的一条未认证直连
    // 放行了,但按来源限速的前缀表满、这个来源没登记上(fail-open):调用方采样打日志。Decide 不填,Admission::Admit 填。
    bool sourceUntracked{false};

    bool operator==(const Verdict &) const = default;
};

// 判定一条刚 accept 进来的直连,顺序:关源 → 总量 → 按来源限速 → 未验签预算 → 速率。放行时已从速率闸取走一个名额;
// 拒绝时不动速率闸。来源排在预算前面:超速前缀的直连不会让别人的老连接被淘汰,也不消耗早拒的随机数(与 gate 相同)。
inline Verdict Decide(const AdmissionState &state, accept_admission::AcceptRateGate &rateGate, SteadyTime now,
                      UniformBelow uniformBelow)
{
    if (state.sourceClosed)
    {
        return Verdict{.rejected = RejectReason::kShuttingDown};
    }
    if (state.currentConnections >= state.maxConnections)
    {
        return Verdict{.rejected = RejectReason::kCapacity};
    }
    if (state.sourceOverRate)
    {
        return Verdict{.rejected = RejectReason::kSourceRate};
    }
    const PreAuthVerdict preAuth =
        DecidePreAuth(state.preAuthBudget, state.unauthenticated, state.oldestUnauthenticatedAge, uniformBelow);
    if (preAuth == PreAuthVerdict::kRejectBudget)
    {
        return Verdict{.rejected = RejectReason::kPreAuthBudget};
    }
    if (preAuth == PreAuthVerdict::kRejectEarlyDrop)
    {
        return Verdict{.rejected = RejectReason::kPreAuthEarlyDrop};
    }
    if (!rateGate.TryTake(now))
    {
        return Verdict{.rejected = RejectReason::kAcceptRate};
    }
    return Verdict{.evictOldestUnauthenticated = preAuth == PreAuthVerdict::kAdmitEvictOldest};
}

// 半关闭未认证连接的宽限期:rejectAndClose / OnUnknownMessage 均在 shutdown() 后设 100ms 延迟强关,
// 超过 200ms 仍留在未认证池内说明延迟收尾已逾期,承压时允许直接强关腾位。
inline constexpr std::chrono::milliseconds kHalfCloseEvictGrace{200};

// 判定一条非 connected(半关闭)未认证连接是否已逾期、可作为承压淘汰目标。
inline bool IsHalfClosedPreAuthEvictable(const SteadyTime::duration ageSinceOpen,
                                         const std::optional<SteadyTime::duration> ageSinceHalfClose,
                                         const PreAuthBudget &budget)
{
    if (ageSinceHalfClose)
    {
        return *ageSinceHalfClose >= kHalfCloseEvictGrace;
    }
    const auto threshold = (std::max)(budget.minEvictableAge, kHalfCloseEvictGrace);
    return ageSinceOpen >= threshold;
}

// 计算传给 DecidePreAuth 的有效建连年龄:对仍连着的连接返回 ageSinceOpen;对已逾期的半关闭连接
// 至少抬到 budget.minEvictableAge,避免未满 minEvictableAge 时误报 kPreAuthBudget 拒绝新连接。
inline SteadyTime::duration EffectivePreAuthEvictAge(const bool connected,
                                                     const SteadyTime::duration ageSinceOpen,
                                                     const std::optional<SteadyTime::duration> ageSinceHalfClose,
                                                     const PreAuthBudget &budget)
{
    if (!connected && IsHalfClosedPreAuthEvictable(ageSinceOpen, ageSinceHalfClose, budget))
    {
        const auto minAge = std::chrono::duration_cast<SteadyTime::duration>(budget.minEvictableAge);
        return (std::max)(ageSinceOpen, minAge);
    }
    return ageSinceOpen;
}

// 换掉按概率早拒用的随机源(传 nullptr 换回缺省的伪随机数)。只给单测用,用完要换回。
void SetEarlyDropRandomForTesting(UniformBelow uniformBelow);

// 进程级累计计数(单调不减)。
struct Counters
{
    uint64_t accepted{0};                                // 接纳并建了会话的直连
    std::array<uint64_t, kRejectReasonCount> rejected{}; // 被拒的直连,按原因
    uint64_t handshakes{0};                              // 票据握手成功、耗时进了直方图的直连
    uint64_t handshakeTimeouts{0};                       // 建连满握手期限仍未握手、被关掉的直连
    uint64_t preAuthEvicted{0};                          // 承压时被淘汰、给新连接让位的未认证直连
    uint64_t sourceUntracked{0};                         // 前缀表满、没做按来源记账就放行的直连

    bool operator==(const Counters &) const = default;
};

// 一个直连面的准入状态与计数。BattleClientEdge 持有一份,只在 battle 主 loop 线程上用。
class Admission
{
public:
    explicit Admission(RateLimit rateLimit) : rateGate(rateLimit) {}

    // 换速率闸参数并把桶装满,换未验签预算与按来源限速(sourceRateLimit 为空 = 关闭)。启动时调一次
    // (main.cpp ConfigureClientAdmissionOrDie)。
    void Configure(RateLimit rateLimit, PreAuthBudget preAuthBudget, std::optional<RateLimit> sourceRateLimit = std::nullopt)
    {
        rateGate.Reset(rateLimit);
        this->preAuthBudget = preAuthBudget;
        if (sourceRateLimit)
        {
            sourceRate.emplace(*sourceRateLimit);
        }
        else
        {
            sourceRate.reset();
        }
    }

    // 关源:从这一刻起新直连一律在 accept 处拒绝(reason = shutting_down)。停机 / 维护关源时调,不可撤销
    // (关源之后进程随即排空退出;计划排空不关源,打着的房间还要接重连)。
    void CloseSource() { sourceClosed = true; }
    bool SourceClosed() const { return sourceClosed; }

    // 准入回调的判定:放行时 rejected 为空(已取走一个速率名额),evictOldestUnauthenticated 为真时调用方要先淘汰
    // 最老的一条未认证直连(淘汰后调 NotePreAuthEvicted),sourceUntracked 为真时调用方采样打日志;拒绝时带原因,
    // 并已按原因计数。日志由调用方采样打。unauthenticated 与 oldestUnauthenticatedAge 的含义见 AdmissionState。
    // 按来源限速开启且承压时(SourceRateUnderPressure)从 peer 取公网来源前缀问它的桶,所有闸都放行之后才给它记一笔。
    Verdict Admit(const muduo::net::InetAddress &peer, size_t currentConnections, size_t maxConnections,
                  uint64_t unauthenticated, std::optional<SteadyTime::duration> oldestUnauthenticatedAge, SteadyTime now);

    // 连接建出来之后才发现不能要(连接回调里的第二道总量闸、空密钥兜底):只计数,连接由调用方关。
    void NoteRejectedAfterAccept(RejectReason reason);
    // 接纳了一条直连(登记了未认证记录):计一次接纳。
    void NoteAccepted();
    // 票据握手成功:耗时进直方图。openedAt 是建连时刻,verifiedAt 是握手成功时刻。
    void NoteHandshake(SteadyTime openedAt, SteadyTime verifiedAt);
    // 握手期限到了仍未握手、连接被关:计一次收割。
    void NoteHandshakeTimeout();
    // 承压时淘汰了一条未认证直连给新连接让位:计一次淘汰。
    void NotePreAuthEvicted();

    const Counters &GetCounters() const { return counters; }
    const RateLimit &CurrentRateLimit() const { return rateGate.Limit(); }
    const PreAuthBudget &CurrentPreAuthBudget() const { return preAuthBudget; }
    // 生效的按来源限速参数;关闭时为空。
    std::optional<RateLimit> CurrentSourceRateLimit() const
    {
        return sourceRate ? std::optional<RateLimit>(sourceRate->Limit()) : std::nullopt;
    }

    // 把连接类指标写进一次抓取(BattleClientEdge 登记的采集回调在 battle 主 loop 上调用):
    //   battle_connections_current / battle_connections_max  直连数(会话 + 未认证,总量上限按它判)与生效上限;
    //   battle_unauthenticated_connections                   既没完成票据握手、也没出示有效网络探测授权的直连;
    //   battle_accepted_total                                接纳的直连;rate() 就是新建连接速率;
    //   battle_rejected_total{reason}                        被拒的直连,reason 是 RejectReason 的名字;
    //   battle_accept_rate_limit_per_second / battle_accept_burst 生效的速率闸参数;
    //   battle_handshake_duration_ms                         建连到票据握手成功的耗时直方图;
    //   battle_preauth_reaped_total                          建连满握手期限仍未握手、被关掉的直连;
    //   battle_preauth_evicted_total                         承压时被淘汰、给新连接让位的未认证直连;
    //   battle_preauth_budget / battle_preauth_early_drop_start / battle_preauth_evictable_age_ms 生效的未验签预算参数;
    //   battle_source_rate_limit_per_second / battle_source_burst 生效的按来源限速参数(关闭时为 0);
    //   battle_source_rate_active                            最近一次判定时按来源限速是否在生效(开启且承压);
    //   battle_source_tracked / battle_source_untracked_total 前缀表里的前缀数,表满时没记账就放行的直连;
    //   battle_websocket_handshakes_total{result}            小游戏直连的 WebSocket 升级请求,result 是 accepted / rejected;
    //   battle_websocket_protocol_closed_total               帧不合协议、由本端关掉的 WebSocket 直连。
    // 标签只有 reason 与 result 两个封闭集合,不带对端地址、房间号与玩家号(AGENTS.md §9)。
    void CollectMetrics(prometheus_text::Writer &writer, size_t currentConnections, size_t maxConnections,
                        size_t unauthenticatedConnections) const;

private:
    bool sourceClosed{false};
    accept_admission::AcceptRateGate rateGate;
    // 没 Configure 时(只有不 Install 的单测)取速率下限推导的缺省:起点 500、预算 1000。main.cpp 启动时换成正式参数。
    PreAuthBudget preAuthBudget{DefaultPreAuthBudget(0)};
    // 按来源限速:空 = 关闭(缺省)。
    std::optional<accept_admission::SourceRateGate> sourceRate;
    // 全局速率闸最近一次拒绝的时刻:之后一个握手期限内算承压(SourceRateUnderPressure)。
    std::optional<SteadyTime> lastAcceptRateRejectAt;
    // 最近一次判定时按来源限速是否在生效,给指标用。
    bool sourceRateActive{false};
    Counters counters;
    accept_admission::HandshakeHistogram handshake;
};

} // namespace battle_connection_admission
