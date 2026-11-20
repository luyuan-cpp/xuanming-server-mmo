#pragma once

// gate 客户端连接准入(docs/design/gate-connection-admission-control.md §6 的 P0-2 后一半与 P1-1 ~ P1-4)。
//
// 判定发生在 muduo Acceptor 的准入回调里:accept 成功之后、TcpConnection 建出来之前。被拒的连接当场以 RST 关闭,
// gate 不为它建任何对象,内核也不留 TIME_WAIT。判据都不需要知道对端是谁,按顺序:
//   1. 关源:停机 / 维护关源之后不再接新连接;
//   2. 总量:会话数到上限(GateMaxConnections;dev / test 的 0 = 只剩 session id 空间的硬上限);
//   3. 未验签预算:还没验签的连接单独一个池,承压时最老的先让位,腾不出位置时按概率早拒、到预算全拒(见 PreAuthBudget);
//   4. 速率:新建连接的全局令牌桶,挡住「不停建连又立刻关」这种总量闸永远碰不到的流量。
// 需要知道对端是谁的判据(每来源限制)不在这里,见设计稿 P2-2。
//
// 本文件分两层:
//   - 纯逻辑(拒绝原因、环境变量名、未验签预算、判定函数):不碰 muduo、会话表与时钟,
//     单测直接测(cpp/nodes/gate/tests/connection_admission_test.cpp)。速率闸、缺省速率的推导、环境变量解析与
//     握手耗时直方图是 battle 直连也在用的同一份,在 core 的 network/accept_admission.h
//     (单测在 cpp/tests/rpc_controller_test/accept_admission_test.cpp),下面用 using 引进本命名空间;
//   - 装配(Configure 往下):gate loop 线程上的 thread_local 状态,与 client_heartbeat / drain_migration 同一做法,
//     回调不捕获任何对象(AGENTS.md §11.7)。只能在 gate 的 EventLoop 线程上调用。

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
#include "network/client_liveness.h"
#include "network/network_utils.h"
#include "session/comp/session_info_comp.h"

namespace muduo::net
{
class InetAddress;
}

namespace gate_connection_admission
{

// 与 battle 直连共用的纯逻辑(core network/accept_admission.h)。引进来之后 gate 的代码与单测照旧不带前缀。
using accept_admission::AcceptRateGate;
using accept_admission::DefaultRateLimit;
using accept_admission::HandshakeBucketIndex;
using accept_admission::HandshakeHistogram;
using accept_admission::kHandshakeBucketCount;
using accept_admission::kHandshakeUpperBoundsMs;
using accept_admission::ParseOptionalPositive;
using accept_admission::RateLimit;
using accept_admission::SteadyTime;

// ---------------------------------------------------------------------------
// 纯逻辑
// ---------------------------------------------------------------------------

// 一条连接被拒绝的原因。指标 gate_rejected_total{reason} 的标签取值,封闭集合(AGENTS.md §9:标签只放低基数值)。
enum class RejectReason : uint8_t
{
	kCapacity,      // 会话数到上限
	kAcceptRate,    // 新建连接速率超过令牌桶
	kShuttingDown,  // 已关源(停机 / 维护),不再接新连接
	kNotReady,      // session id 的 node 段还没种好(纵深防御,正常启动走不到)
	kNoTokenSecret, // prod 下 gate_token_secret 为空(纵深防御,启动门禁已拒启)
	kPreAuthBudget,    // 未验签连接到预算,且没有建连够久、可以让位的
	kPreAuthEarlyDrop, // 未验签连接过了早拒起点、没有可以让位的,按概率拒了
	kCount,
};
inline constexpr size_t kRejectReasonCount = static_cast<size_t>(RejectReason::kCount);
inline constexpr auto kRejectReasonNames = std::to_array<std::string_view>({
	"capacity",
	"accept_rate",
	"shutting_down",
	"not_ready",
	"no_token_secret",
	"preauth_budget",
	"preauth_early_drop",
});
static_assert(kRejectReasonNames.size() == kRejectReasonCount, "one name per RejectReason");

constexpr std::string_view RejectReasonName(RejectReason reason)
{
	return kRejectReasonNames[static_cast<size_t>(reason)];
}

// 生效的连接上限。dev / test 的 0 只关掉「运维配置的阈值」,不能关掉 session id 空间的硬上限:
// 131071 个可用 id 全占满之后发号器会回绕,碰撞检查永远找不到空号(main.cpp ValidateGateConnectionLimitOrDie)。
constexpr uint32_t EffectiveMaxConnections(uint32_t configuredMaxConnections)
{
	return configuredMaxConnections > 0 ? configuredMaxConnections : SessionIdGenerator::kSeqMask;
}

// 运维覆盖(压测要打超过缺省速率的建连、或实测后要调阈值时用;不设 = 按 DefaultRateLimit 推导,推导见
// accept_admission.h):
//   GATE_ACCEPT_RATE_PER_SECOND  持续速率,规范十进制 1..accept_admission::kMaxAcceptRatePerSecond;
//   GATE_ACCEPT_BURST            桶深,规范十进制 1..accept_admission::kMaxAcceptBurst;不设时取 max(连接上限, 持续速率)。
// 没有「0 = 关闭」:速率闸不留一个拼错就整个失效的开关,要放开就写一个足够大的数。battle 直连用自己的两个变量名。
inline constexpr const char *kAcceptRateEnv = "GATE_ACCEPT_RATE_PER_SECOND";
inline constexpr const char *kAcceptBurstEnv = "GATE_ACCEPT_BURST";

// 由两个环境变量的原文与生效上限定出速率闸参数。任一变量非法时返回 nullopt,error 里是给启动日志用的英文说明。
inline std::optional<RateLimit> ResolveRateLimit(const char *rawRate, const char *rawBurst,
												 uint32_t effectiveMaxConnections, std::string &error)
{
	return accept_admission::ResolveRateLimit({kAcceptRateEnv, kAcceptBurstEnv, "GateMaxConnections"}, rawRate, rawBurst,
											  effectiveMaxConnections, error);
}

// ---------------------------------------------------------------------------
// 未验签连接的独立预算(设计稿 P0-2 的后一半)
// ---------------------------------------------------------------------------
//
// 建连之后、验签成功之前的连接单独算一个池,大小 n 就是 UnauthenticatedConnections()(RAII 占位,pre_auth_slot.h)。
// 池上有两个点:早拒起点 S 与满点 B(预算)。新连接来时:
//   1. n < S:不承压,放行;
//   2. n ≥ S,且最老的一条未验签连接已建连满 kMinEvictableAge:淘汰它,放行新连接(LRU:攻击者占着不验签的老连接先死);
//   3. 没有够老的可以让位、n ≥ B:拒绝(preauth_budget);
//   4. 没有够老的可以让位、S ≤ n < B:按概率拒绝,概率从 S 处的 0 线性升到 B 处的 100%(preauth_early_drop)。
// 先淘汰、后早拒:能腾出位置就不拒新连接。只有池里全是刚建的连接(一次突发)时才拒,而且拒绝率随占用平滑上升,
// 不是到 B 时一下子从 0 跳到 100%。
//
// 取值(没有压测数据,按下面的推导先定;实测后只改这里的常数):
//   B = 缺省持续速率 × 握手期限(上限 2 万时 200 条/秒 × 10 秒 = 2000,即上限的 10%;上限不到 1 万时跟着速率下限,为 1000):
//     - 设计稿给的区间是上限的 5% ~ 10%,取上沿;
//     - 这正是速率闸放得进来的稳态流量在一个握手期限里能攒下的最多未验签连接:稳态下池到不了 B,预算只在速率闸的
//       突发额度被花掉的那几秒里起作用;
//     - login 在会话数到上限的 90% 时就不再往这台派人(gate-load-balancing-design.md),B 正好是剩下的 10%:
//       未验签连接再多也挤不掉已验签会话的那 90%;
//     - 用缺省速率而不是覆盖后的速率:压测时调高速率闸不该顺带放大未验签预算。
//   S = B 的一半(上限 2 万时 1000,即上限的 5%):
//     - 合法洪峰的未验签池很小:客户端建连后立刻发验签,accept 时验签请求多半已在接收缓冲里,下一轮 loop 就处理完。
//       最坏的冷启动全量重连按 2 万条在 5 秒内连进来(4000 条/秒)、95% 的连接 50ms 验签、5% 首个数据段丢包重传要 1.5 秒估,
//       池 ≈ 4000 × (0.95 × 0.05 + 0.05 × 1.5) ≈ 490 条,是上限的 2.5%,起点留了一倍余量;
//     - sshd 缺省 MaxStartups 10:30:100 的起点只有满点的 10%,那是给很少有人同时登录的 sshd 定的;
//       gate 要扛重连风暴,起点必须高于合法洪峰。
//     - 上限配得很小(本机、验收把上限调到几十)时起点 500 高于上限,这一道不起作用,该起作用的是总量闸
//       (与速率闸设下限同一个用意)。
//   早拒的曲线:sshd 的公式 p = rate + (100 - rate) × (n - S) / (B - S) 取 rate = 0,也就是 Envoy overload manager
//     scaled trigger 的形状(S 处 0、B 处 100%,中间线性)。不取 sshd 缺省的 30% 起跳:起点已经高于合法洪峰的估计,
//     一次刚越过起点的合法洪峰不该立刻被拒掉三成。
//   kMinEvictableAge = 3 秒:
//     - 与 GateHandshakeSlow 告警(握手 P99 > 3 秒)同值:正常客户端 3 秒内验签,建连 3 秒还没验签的落在最慢的 1% 里;
//     - 容得下首个数据段连丢两次:三次握手已给出一个 RTT 样本,重传超时约 3 × RTT(Linux 下限 200ms,逐次翻倍),
//       RTT 在 300ms 以内时两次重传都落在 3 秒内;
//     - 远小于 10 秒的握手期限:承压时未验签连接活 3 秒左右就让位,不再占满 10 秒。这对应 Envoy overload manager 的
//       reduce_timeouts(承压时把空闲超时缩到下限 min_timeout),只是按「谁最老」一条一条执行;
//     - 速率闸放进来的稳态流量下池停在 S,每条连接活 S / 持续速率 = 5 秒才轮到它让位,比 3 秒长:
//       淘汰换掉的只是 5 秒还没验签的连接。
//   与速率闸的先后:预算排在速率前面判,理由与「总量排在速率前面」相同 —— 被预算拒掉的连接不消耗速率名额;
//   淘汰只在速率闸也放行之后才执行(没放行就不淘汰)。
struct PreAuthBudget
{
	uint32_t earlyDropStart; // S:未验签连接数到这里开始承压(先淘汰够老的,没有够老的就按概率拒)
	uint32_t full;           // B:预算;到这里且没有够老的可以让位就一律拒

	bool operator==(const PreAuthBudget &) const = default;
};
inline constexpr uint32_t kEarlyDropStartPercent = 50; // S 占 B 的百分比
inline constexpr std::chrono::milliseconds kMinEvictableAge{3000};
static_assert(kMinEvictableAge < client_liveness::kClientLiveness.handshakeTimeout,
			  "eviction must act before the handshake deadline reaps the connection anyway");

constexpr PreAuthBudget DefaultPreAuthBudget(uint32_t effectiveMaxConnections)
{
	const uint64_t deadlineSeconds = static_cast<uint64_t>(
		std::chrono::duration_cast<std::chrono::seconds>(client_liveness::kClientLiveness.handshakeTimeout).count());
	const uint64_t perSecond = DefaultRateLimit(effectiveMaxConnections).perSecond;
	const uint64_t full = (std::min)(uint64_t{UINT32_MAX}, perSecond * deadlineSeconds);
	return PreAuthBudget{static_cast<uint32_t>(full * kEarlyDropStartPercent / 100), static_cast<uint32_t>(full)};
}

// 未验签预算这一道的判定结果。
enum class PreAuthVerdict : uint8_t
{
	kAdmit,             // 不承压;或承压、按概率放行了
	kAdmitEvictOldest,  // 承压:淘汰最老的一条未验签连接,给这条让位
	kRejectEarlyDrop,   // 承压、没有够老的可以让位,按概率拒了
	kRejectBudget,      // 到预算、没有够老的可以让位
};

// 返回 [0, bound) 里均匀的一个整数(bound ≥ 1)。按概率早拒用:装配层用线程局部的伪随机数,单测注入定值。
using UniformBelow = uint32_t (*)(uint32_t bound);

// 未验签预算这一道。unauthenticated 是持有未验签占位的连接数,oldestAge 是其中最老一条已建连多久(没有未验签连接为空)。
// 只在早拒带里、超过起点时取一次随机数。
inline PreAuthVerdict DecidePreAuth(const PreAuthBudget &budget, uint64_t unauthenticated,
									std::optional<SteadyTime::duration> oldestAge, UniformBelow uniformBelow)
{
	if (unauthenticated < budget.earlyDropStart)
	{
		return PreAuthVerdict::kAdmit;
	}
	if (oldestAge && *oldestAge >= kMinEvictableAge)
	{
		return PreAuthVerdict::kAdmitEvictOldest;
	}
	if (unauthenticated >= budget.full)
	{
		return PreAuthVerdict::kRejectBudget;
	}
	// 走到这里 S ≤ n < B,所以 B - S ≥ 1。取 d ∈ [0, B - S),d < n - S 就拒:拒绝的概率恰好是 (n - S) / (B - S)。
	const uint64_t over = unauthenticated - budget.earlyDropStart;
	if (over > 0 && uniformBelow(budget.full - budget.earlyDropStart) < over)
	{
		return PreAuthVerdict::kRejectEarlyDrop;
	}
	return PreAuthVerdict::kAdmit;
}

// 判定所需的当前状态。
struct AdmissionState
{
	bool sourceClosed{false};   // 已关源
	size_t currentSessions{0};  // 会话表大小。与连接一一对应:建连回调插入,断开回调删除,都在 loop 线程上同步完成
	uint32_t maxConnections{0}; // 生效上限(EffectiveMaxConnections)
	uint64_t unauthenticated{0};                                  // 持有未验签占位的连接数
	std::optional<SteadyTime::duration> oldestUnauthenticatedAge; // 其中最老一条已建连多久;没有未验签连接为空
	PreAuthBudget preAuthBudget{DefaultPreAuthBudget(EffectiveMaxConnections(0))};
};

// 判定结果:拒绝(带原因),或放行;放行时可能要先淘汰最老的一条未验签连接给它让位。
struct Admission
{
	std::optional<RejectReason> rejected;    // 空 = 放行
	bool evictOldestUnauthenticated{false}; // 放行,并且要先淘汰最老的一条未验签连接

	bool operator==(const Admission &) const = default;
};

// 判定一条刚 accept 进来的连接,顺序:关源 → 总量 → 未验签预算 → 速率。放行时已从速率闸取走一个名额;
// 拒绝时不动速率闸:满员、到预算、早拒的连接都不消耗名额,腾出位置之后名额还在。
inline Admission Decide(const AdmissionState &state, AcceptRateGate &rateGate, SteadyTime now, UniformBelow uniformBelow)
{
	if (state.sourceClosed)
	{
		return Admission{.rejected = RejectReason::kShuttingDown};
	}
	if (state.currentSessions >= state.maxConnections)
	{
		return Admission{.rejected = RejectReason::kCapacity};
	}
	const PreAuthVerdict preAuth =
		DecidePreAuth(state.preAuthBudget, state.unauthenticated, state.oldestUnauthenticatedAge, uniformBelow);
	if (preAuth == PreAuthVerdict::kRejectBudget)
	{
		return Admission{.rejected = RejectReason::kPreAuthBudget};
	}
	if (preAuth == PreAuthVerdict::kRejectEarlyDrop)
	{
		return Admission{.rejected = RejectReason::kPreAuthEarlyDrop};
	}
	if (!rateGate.TryTake(now))
	{
		return Admission{.rejected = RejectReason::kAcceptRate};
	}
	return Admission{.evictOldestUnauthenticated = preAuth == PreAuthVerdict::kAdmitEvictOldest};
}

// ---------------------------------------------------------------------------
// 装配(gate loop 线程)
// ---------------------------------------------------------------------------

// 启动时调一次(安全门禁之后、客户端回调装上之前):记下生效上限,装好速率闸(桶是满的),按上限定未验签预算。
void Configure(uint32_t configuredMaxConnections, RateLimit rateLimit);

// 关源:从这一刻起新连接一律在 accept 处拒绝(reason = shutting_down)。停机 / 维护关源时调一次,不可撤销
// (关源之后进程随即排空退出)。状态放在本模块里,准入回调因此不需要捕获任何对象。
void CloseSource();

// Acceptor 准入回调的函数体(main.cpp 经 TcpServer::setAdmissionCallback 装上)。放行返回 true;拒绝返回 false,
// 并按原因计数、采样打日志(被拒的量就是攻击流量的量级,不能逐条写盘)。
// 判定要求先淘汰时,放行之前收回最老那条未验签连接的占位,再经会话里的 weak 连接 forceClose 它:
// 会话不在这里删,留给随后的断开回调(没验签的会话没有角色,断开回调只删会话)。
bool AdmitConnection(const muduo::net::InetAddress &peer, SteadyTime now);

// 连接已经建出来之后才发现不能要(HandleConnectionEstablished 里的纵深防御分支):只计数,连接由调用方 forceClose。
void NoteRejectedAfterAccept(RejectReason reason);

// 会话刚插入会话表:计一次接纳;还没验签的会话领一个未验签占位(SessionInfo::preAuthSlot),并按建连先后进淘汰索引。
void OnSessionOpened(SessionId sessionId, SessionInfo &session, SteadyTime now);

// 验签成功:归还占位,握手耗时进直方图。没有占位(已验证会话重发验签、dev 免验签)时什么也不做,可重复调用。
void OnSessionVerified(SessionInfo &session, SteadyTime now);

// 进程级累计计数(gate loop 线程,单调不减)。供指标与单测读。
struct Counters
{
	uint64_t accepted{0};                                // 接纳并建了会话的连接
	std::array<uint64_t, kRejectReasonCount> rejected{}; // 被拒的连接,按原因
	uint64_t handshakes{0};                              // 验签成功、握手耗时进了直方图的连接
	uint64_t preAuthEvicted{0};                          // 承压时被淘汰、给新连接让位的未验签连接

	bool operator==(const Counters &) const = default;
};
const Counters &GetCounters();
// 当前持有未验签占位的连接数(gate_unauthenticated_connections)。
uint64_t UnauthenticatedConnections();
// 生效的速率闸参数。
RateLimit CurrentRateLimit();
// 生效的未验签预算(Configure 按上限推导,见 DefaultPreAuthBudget)。
PreAuthBudget CurrentPreAuthBudget();

// 把连接类指标写进一次抓取(登记在进程的指标注册表里,由 /metrics 出口调用):
//   gate_connections_current / gate_connections_max      会话数与生效上限;
//   gate_unauthenticated_connections                     还没验签的连接数(未验签池占用,区分洪峰与攻击的判据);
//   gate_accepted_total                                  接纳的连接;rate() 就是新建连接速率;
//   gate_rejected_total{reason}                          被拒的连接,reason 是 RejectReason 的名字;
//   gate_accept_rate_limit_per_second / gate_accept_burst 生效的速率闸参数;
//   gate_handshake_duration_ms                           建连到验签成功的耗时直方图;
//   gate_preauth_reaped_total                            建连满握手期限仍未验签、被关掉的连接(client_heartbeat 的计数);
//   gate_preauth_evicted_total                           承压时被淘汰、给新连接让位的未验签连接;
//   gate_preauth_budget / gate_preauth_early_drop_start  生效的未验签预算与早拒起点。
// 标签只有 reason 一个封闭集合,不带对端地址、会话号与账号(AGENTS.md §9)。在 gate 主 loop 上执行,读本线程的计数。
void CollectMetrics(prometheus_text::Writer &writer);
// gate 启动时调一次,把 CollectMetrics 登记进进程的指标注册表;重复登记只记一行 ERROR。
void RegisterMetrics();

// 清空本线程的计数、直方图、关源标记、配置、淘汰索引与注入的随机源(回到未 Configure 的缺省),
// 不动仍被会话持有的未验签占位(它们之后不会再被淘汰选中)。只给单测用。
void ResetForTesting();
// 换掉按概率早拒用的随机源(传 nullptr 换回缺省的伪随机数)。只给单测用,ResetForTesting 也会换回缺省。
void SetEarlyDropRandomForTesting(UniformBelow uniformBelow);

} // namespace gate_connection_admission
