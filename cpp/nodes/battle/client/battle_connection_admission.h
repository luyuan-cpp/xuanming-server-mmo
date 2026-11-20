#pragma once

// battle 客户端直连面的连接准入(docs/design/turn-based-battle-server.md D27;与 gate 同一套做法,
// docs/design/gate-connection-admission-control.md P1-1 ~ P1-4)。
//
// 判定发生在 muduo Acceptor 的准入回调里:accept 成功之后、TcpConnection 建出来之前。被拒的连接当场以 RST 关闭,
// battle 不为它建任何对象,内核也不留 TIME_WAIT。判据都不需要知道对端是谁,按顺序:
//   1. 关源:停机 / 维护关源之后不再接新连接;
//   2. 总量:直连会话数到上限(battle_max_connections;dev / test 的 0 = BattleClientEdge::kHardMaxConnections);
//   3. 速率:新建连接的全局令牌桶,挡住「不停建连又立刻关」这种总量闸永远碰不到的流量。
// 总量排在速率前面:满员时被拒的连接不消耗速率名额。顺序与 gate 的 Decide 相同;gate 另有自己的判据,判定函数各写各的。
//
// 速率闸、缺省速率的推导、环境变量解析与握手耗时直方图和 gate 是同一份(core network/accept_admission.h);
// 拒绝原因、计数与指标名是 battle 自己的。本文件只管判定、计数与写指标,不认识连接与会话表:当前直连数、生效上限、
// 未握手的直连数都由 BattleClientEdge 在调用时给,时刻也由调用方给(单调时钟,单测注入)。
// 单测:cpp/nodes/battle/tests/battle_connection_admission_test.cpp。
// 线程模型:只在 battle 主 loop 线程上用(与 BattleClientEdge 同线程),不加锁。

#include <array>
#include <cstddef>
#include <cstdint>
#include <optional>
#include <string>
#include <string_view>

#include "metrics/prometheus_text.h"
#include "network/accept_admission.h"

namespace battle_connection_admission
{

using accept_admission::RateLimit;
using accept_admission::SteadyTime;

// 一条直连被拒绝的原因。指标 battle_rejected_total{reason} 的标签取值,封闭集合(AGENTS.md §9:标签只放低基数值);
// 采样日志里的 reason 也用这几个名字。与 gate 的同名原因同义,告警与看板可以按 reason 并列两个节点。
enum class RejectReason : uint8_t
{
    kCapacity,      // 直连会话数到上限
    kAcceptRate,    // 新建连接速率超过令牌桶
    kShuttingDown,  // 已关源(停机 / 维护),不再接新连接
    kNoTokenSecret, // prod 下 battle_token_secret 为空(纵深防御,启动门禁已拒启)
    kCount,
};
inline constexpr size_t kRejectReasonCount = static_cast<size_t>(RejectReason::kCount);
inline constexpr auto kRejectReasonNames = std::to_array<std::string_view>({
    "capacity",
    "accept_rate",
    "shutting_down",
    "no_token_secret",
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
// 全服匹配的网络探测也连这个端口(每个机房只探一台 battle,见设计稿 D27 的说明),探测量大时要先调高持续速率。
inline constexpr const char *kAcceptRateEnv = "BATTLE_ACCEPT_RATE_PER_SECOND";
inline constexpr const char *kAcceptBurstEnv = "BATTLE_ACCEPT_BURST";

// 由两个环境变量的原文与生效上限定出速率闸参数。任一变量非法时返回 nullopt,error 里是给启动日志用的英文说明。
inline std::optional<RateLimit> ResolveRateLimit(const char *rawRate, const char *rawBurst,
                                                 uint32_t effectiveMaxConnections, std::string &error)
{
    return accept_admission::ResolveRateLimit({kAcceptRateEnv, kAcceptBurstEnv, "BattleMaxConnections"}, rawRate,
                                              rawBurst, effectiveMaxConnections, error);
}

// 判定所需的当前状态。
struct AdmissionState
{
    bool sourceClosed{false};     // 已关源
    size_t currentConnections{0}; // 直连会话数。与连接一一对应:建连回调插入,断开回调删除,都在 loop 线程上同步完成
    size_t maxConnections{0};     // 生效上限(BattleClientEdge::EffectiveMaxConnections)
};

// 判定一条刚 accept 进来的直连。放行返回 nullopt,并已从速率闸取走一个名额;拒绝返回原因,不动速率闸。
inline std::optional<RejectReason> Decide(const AdmissionState &state, accept_admission::AcceptRateGate &rateGate,
                                          SteadyTime now)
{
    if (state.sourceClosed)
    {
        return RejectReason::kShuttingDown;
    }
    if (state.currentConnections >= state.maxConnections)
    {
        return RejectReason::kCapacity;
    }
    if (!rateGate.TryTake(now))
    {
        return RejectReason::kAcceptRate;
    }
    return std::nullopt;
}

// 进程级累计计数(单调不减)。
struct Counters
{
    uint64_t accepted{0};                                // 接纳并建了会话的直连
    std::array<uint64_t, kRejectReasonCount> rejected{}; // 被拒的直连,按原因
    uint64_t handshakes{0};                              // 票据握手成功、耗时进了直方图的直连
    uint64_t handshakeTimeouts{0};                       // 建连满握手期限仍未握手、被关掉的直连

    bool operator==(const Counters &) const = default;
};

// 一个直连面的准入状态与计数。BattleClientEdge 持有一份,只在 battle 主 loop 线程上用。
class Admission
{
public:
    explicit Admission(RateLimit rateLimit) : rateGate_(rateLimit) {}

    // 换速率闸参数并把桶装满。启动时调一次(main.cpp ConfigureClientAdmissionOrDie)。
    void Configure(RateLimit rateLimit) { rateGate_.Reset(rateLimit); }

    // 关源:从这一刻起新直连一律在 accept 处拒绝(reason = shutting_down)。停机 / 维护关源时调,不可撤销
    // (关源之后进程随即排空退出;计划排空不关源,打着的房间还要接重连)。
    void CloseSource() { sourceClosed_ = true; }
    bool SourceClosed() const { return sourceClosed_; }

    // 准入回调的判定:放行返回 nullopt(已取走一个速率名额);拒绝返回原因,并已按原因计数。日志由调用方采样打。
    std::optional<RejectReason> Admit(size_t currentConnections, size_t maxConnections, SteadyTime now);

    // 连接建出来之后才发现不能要(连接回调里的第二道总量闸、空密钥兜底):只计数,连接由调用方关。
    void NoteRejectedAfterAccept(RejectReason reason);
    // 会话建好了:计一次接纳。
    void NoteAccepted();
    // 票据握手成功:耗时进直方图。openedAt 是建连时刻,verifiedAt 是握手成功时刻。
    void NoteHandshake(SteadyTime openedAt, SteadyTime verifiedAt);
    // 握手期限到了仍未握手、连接被关:计一次收割。
    void NoteHandshakeTimeout();

    const Counters &GetCounters() const { return counters_; }
    const RateLimit &CurrentRateLimit() const { return rateGate_.Limit(); }

    // 把连接类指标写进一次抓取(BattleClientEdge 登记的采集回调在 battle 主 loop 上调用):
    //   battle_connections_current / battle_connections_max  直连会话数与生效上限;
    //   battle_unauthenticated_connections                   既没完成票据握手、也没出示有效网络探测授权的直连;
    //   battle_accepted_total                                接纳的直连;rate() 就是新建连接速率;
    //   battle_rejected_total{reason}                        被拒的直连,reason 是 RejectReason 的名字;
    //   battle_accept_rate_limit_per_second / battle_accept_burst 生效的速率闸参数;
    //   battle_handshake_duration_ms                         建连到票据握手成功的耗时直方图;
    //   battle_preauth_reaped_total                          建连满握手期限仍未握手、被关掉的直连。
    // 标签只有 reason 一个封闭集合,不带对端地址、房间号与玩家号(AGENTS.md §9)。
    void CollectMetrics(prometheus_text::Writer &writer, size_t currentConnections, size_t maxConnections,
                        size_t unauthenticatedConnections) const;

private:
    bool sourceClosed_{false};
    accept_admission::AcceptRateGate rateGate_;
    Counters counters_;
    accept_admission::HandshakeHistogram handshake_;
};

} // namespace battle_connection_admission
