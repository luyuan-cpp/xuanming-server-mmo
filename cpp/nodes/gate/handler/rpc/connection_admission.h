#pragma once

// gate 客户端连接准入(docs/design/gate-connection-admission-control.md §6 的 P1-1 ~ P1-4)。
//
// 判定发生在 muduo Acceptor 的准入回调里:accept 成功之后、TcpConnection 建出来之前。被拒的连接当场以 RST 关闭,
// gate 不为它建任何对象,内核也不留 TIME_WAIT。判据都不需要知道对端是谁,按顺序:
//   1. 关源:停机 / 维护关源之后不再接新连接;
//   2. 总量:会话数到上限(GateMaxConnections;dev / test 的 0 = 只剩 session id 空间的硬上限);
//   3. 速率:新建连接的全局令牌桶,挡住「不停建连又立刻关」这种总量闸永远碰不到的流量。
// 需要知道对端是谁的判据(每来源限制、未验签连接的独立预算)不在这里,见设计稿 P0-2、P2-2。
//
// 本文件分两层:
//   - 纯逻辑(拒绝原因、速率闸、缺省速率的推导、环境变量解析、判定函数):不碰 muduo、会话表与时钟,
//     单测直接测(cpp/nodes/gate/tests/connection_admission_test.cpp);
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
#include "network/network_utils.h"
#include "session/comp/session_info_comp.h"

namespace muduo::net
{
class InetAddress;
}

namespace gate_connection_admission
{

using SteadyTime = std::chrono::steady_clock::time_point;

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
	kCount,
};
inline constexpr size_t kRejectReasonCount = static_cast<size_t>(RejectReason::kCount);
inline constexpr auto kRejectReasonNames = std::to_array<std::string_view>({
	"capacity",
	"accept_rate",
	"shutting_down",
	"not_ready",
	"no_token_secret",
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

// 新建连接速率闸的参数。
struct RateLimit
{
	uint32_t perSecond; // 持续速率:每秒放行多少条新连接
	uint32_t burst;     // 桶深:攒满之后可以一口气放行多少条

	bool operator==(const RateLimit &) const = default;
};

// 缺省速率的两个常数,取值依据(设计稿 P1-3;没有压测数据之前按这套推导定,压测后只改这里或用环境变量覆盖):
//
//   桶深 = 连接上限。一台 gate 合法的最坏洪峰是「重启后从零接回一整台的人」(设计稿 §8 的冷启动 + 全量重连),
//   量级就是连接上限。桶深取上限,总量闸会放行的合法洪峰就永远不会被速率闸拒绝;速率闸只限制上限之外的周转。
//
//   持续速率 = 连接上限 / kCapacityRefillSeconds(每秒 1% 的容量),下限 kMinAcceptRatePerSecond:
//     - 握手期限是 10s(client_liveness.h):一个从不验签的来源稳态下最多占住 速率 × 10s = 上限的 10%,
//       正好是设计稿 P0-2 给未验签连接定的预算上沿(5%~10%);
//     - 合法的稳态周转远低于它:2 万人、人均在线半小时,进出约 11 条/秒,加上移动网络的断线重连也只有几十条/秒,
//       每秒 200 条留了一个数量级的余量;
//     - 任意时间窗 T 内接纳的连接数 ≤ 上限 + 速率 × T,「不停建连又立刻关」的开销因此有界。
//   下限是给上限配得很小的环境(本机、单测把上限调到个位数验证总量闸)留的:那时该起作用的是总量闸,不是速率闸。
inline constexpr uint32_t kCapacityRefillSeconds = 100;
inline constexpr uint32_t kMinAcceptRatePerSecond = 100;
// 环境变量覆盖时的取值范围。上界只为挡住误写:远高于单个 IO 线程接得动的速率,也保证速率闸里的时长运算不溢出。
inline constexpr uint32_t kMaxAcceptRatePerSecond = 1'000'000;
inline constexpr uint32_t kMaxAcceptBurst = 1'000'000;

constexpr RateLimit DefaultRateLimit(uint32_t effectiveMaxConnections)
{
	const uint32_t perSecond = std::max(kMinAcceptRatePerSecond, effectiveMaxConnections / kCapacityRefillSeconds);
	return RateLimit{perSecond, std::max(effectiveMaxConnections, perSecond)};
}

// 运维覆盖(压测要打超过缺省速率的建连、或实测后要调阈值时用;不设 = 按 DefaultRateLimit 推导):
//   GATE_ACCEPT_RATE_PER_SECOND  持续速率,规范十进制 1..kMaxAcceptRatePerSecond;
//   GATE_ACCEPT_BURST            桶深,规范十进制 1..kMaxAcceptBurst;不设时取 max(连接上限, 持续速率)。
// 没有「0 = 关闭」:速率闸不留一个拼错就整个失效的开关,要放开就写一个足够大的数。
inline constexpr const char *kAcceptRateEnv = "GATE_ACCEPT_RATE_PER_SECOND";
inline constexpr const char *kAcceptBurstEnv = "GATE_ACCEPT_BURST";

// 解析一个可选的正整数环境变量:nullptr / 空串 → 0(未设);规范十进制 1..max → 该值;
// 其余(非数字、前导零、0、越界、带空白或符号)→ nullopt,调用方按启动配置错误拒启。
constexpr std::optional<uint32_t> ParseOptionalPositive(const char *raw, uint32_t max)
{
	if (raw == nullptr || raw[0] == '\0')
	{
		return uint32_t{0};
	}
	const std::string_view text(raw);
	if (text.size() > 10 || text.front() == '0')
	{
		return std::nullopt;
	}
	uint64_t value = 0;
	for (const char c : text)
	{
		if (c < '0' || c > '9')
		{
			return std::nullopt;
		}
		value = value * 10 + static_cast<uint64_t>(c - '0');
	}
	if (value == 0 || value > max)
	{
		return std::nullopt;
	}
	return static_cast<uint32_t>(value);
}

// 由两个环境变量的原文与生效上限定出速率闸参数。任一变量非法时返回 nullopt,error 里是给启动日志用的英文说明。
inline std::optional<RateLimit> ResolveRateLimit(const char *rawRate, const char *rawBurst,
												 uint32_t effectiveMaxConnections, std::string &error)
{
	const auto rate = ParseOptionalPositive(rawRate, kMaxAcceptRatePerSecond);
	if (!rate)
	{
		error = std::string(kAcceptRateEnv) + " must be an integer 1.." + std::to_string(kMaxAcceptRatePerSecond) +
				" (leave it unset for the default derived from GateMaxConnections)";
		return std::nullopt;
	}
	const auto burst = ParseOptionalPositive(rawBurst, kMaxAcceptBurst);
	if (!burst)
	{
		error = std::string(kAcceptBurstEnv) + " must be an integer 1.." + std::to_string(kMaxAcceptBurst) +
				" (leave it unset for max(GateMaxConnections, rate))";
		return std::nullopt;
	}
	RateLimit limit = DefaultRateLimit(effectiveMaxConnections);
	if (*rate != 0)
	{
		limit.perSecond = *rate;
	}
	limit.burst = *burst != 0 ? *burst : std::max(effectiveMaxConnections, limit.perSecond);
	return limit;
}

// 新建连接速率闸:GCRA(通用信元速率算法,与令牌桶等价;handler/event/world_chat_push_budget.h 是同一算法,
// 那边的参数是编译期常量)。只记「理论到达时刻」tat:每放行一条往后挪一个标准间隔;tat 领先当前时刻超过
// (burst - 1) 个间隔就拒绝。全用整数时长,没有浮点累计误差;拒绝不改状态,所以被拒的洪水不会把之后的合法连接也饿死。
// 起步时桶是满的。时刻由调用方给(单调时钟,单测注入);读数倒退(调用方传错)时只会多拒绝,不会多放行。
// 不加锁:gate 的 accept 只在一个 EventLoop 线程上。
class AcceptRateGate
{
public:
	AcceptRateGate() { Reset(DefaultRateLimit(EffectiveMaxConnections(0))); }
	explicit AcceptRateGate(RateLimit limit) { Reset(limit); }

	// 换参数,并把桶重新装满。perSecond / burst 为 0 时按 1 处理(调用方都经 ResolveRateLimit,不会给 0)。
	void Reset(RateLimit limit)
	{
		limit_ = RateLimit{std::max<uint32_t>(limit.perSecond, 1), std::max<uint32_t>(limit.burst, 1)};
		const SteadyTime::duration oneSecond = std::chrono::duration_cast<SteadyTime::duration>(std::chrono::seconds(1));
		const SteadyTime::duration perConnection = oneSecond / static_cast<SteadyTime::duration::rep>(limit_.perSecond);
		interval_ = std::max(perConnection, SteadyTime::duration(1));
		tolerance_ = interval_ * static_cast<SteadyTime::duration::rep>(limit_.burst - 1);
		tat_ = SteadyTime{};
	}

	// 取一个名额。取不到返回 false:这条连接不接。
	bool TryTake(SteadyTime now)
	{
		const SteadyTime tat = std::max(tat_, now);
		if (tat - now > tolerance_)
		{
			return false;
		}
		tat_ = tat + interval_;
		return true;
	}

	const RateLimit &Limit() const { return limit_; }

private:
	RateLimit limit_{};
	SteadyTime::duration interval_{};
	SteadyTime::duration tolerance_{};
	// 理论到达时刻。时钟零点 = 桶是满的:第一条一定放行,并且立刻有一个完整的突发可用。
	SteadyTime tat_{};
};

// 判定所需的当前状态。
struct AdmissionState
{
	bool sourceClosed{false};   // 已关源
	size_t currentSessions{0};  // 会话表大小。与连接一一对应:建连回调插入,断开回调删除,都在 loop 线程上同步完成
	uint32_t maxConnections{0}; // 生效上限(EffectiveMaxConnections)
};

// 判定一条刚 accept 进来的连接。放行返回 nullopt,并已从速率闸取走一个名额;拒绝返回原因,不动速率闸。
// 总量排在速率前面:满员时被拒的连接不消耗名额,腾出位置之后名额还在。
inline std::optional<RejectReason> Decide(const AdmissionState &state, AcceptRateGate &rateGate, SteadyTime now)
{
	if (state.sourceClosed)
	{
		return RejectReason::kShuttingDown;
	}
	if (state.currentSessions >= state.maxConnections)
	{
		return RejectReason::kCapacity;
	}
	if (!rateGate.TryTake(now))
	{
		return RejectReason::kAcceptRate;
	}
	return std::nullopt;
}

// 握手耗时直方图(建连到验签成功)的桶上界,毫秒。握手期限是 10s,超过的连接已被存活扫描关掉,不会落进 +Inf 之外的桶;
// 1s 以上分得细,是为了用 P99.9 复核握手期限该取多少(设计稿 P1-4)。Writer 只写整数,所以单位取毫秒而不是秒。
inline constexpr auto kHandshakeUpperBoundsMs =
	std::to_array<uint64_t>({10, 25, 50, 100, 250, 500, 1000, 2000, 3000, 5000, 7500, 10000});
inline constexpr size_t kHandshakeBucketCount = kHandshakeUpperBoundsMs.size() + 1;

// 一次握手耗时落在哪个桶:第一个「上界 ≥ 耗时」的桶;都不满足落最后一个(+Inf)。
constexpr size_t HandshakeBucketIndex(uint64_t elapsedMs)
{
	size_t index = 0;
	while (index < kHandshakeUpperBoundsMs.size() && elapsedMs > kHandshakeUpperBoundsMs[index])
	{
		++index;
	}
	return index;
}

// ---------------------------------------------------------------------------
// 装配(gate loop 线程)
// ---------------------------------------------------------------------------

// 启动时调一次(安全门禁之后、客户端回调装上之前):记下生效上限,装好速率闸(桶是满的)。
void Configure(uint32_t configuredMaxConnections, RateLimit rateLimit);

// 关源:从这一刻起新连接一律在 accept 处拒绝(reason = shutting_down)。停机 / 维护关源时调一次,不可撤销
// (关源之后进程随即排空退出)。状态放在本模块里,准入回调因此不需要捕获任何对象。
void CloseSource();

// Acceptor 准入回调的函数体(main.cpp 经 TcpServer::setAdmissionCallback 装上)。放行返回 true;拒绝返回 false,
// 并按原因计数、采样打日志(被拒的量就是攻击流量的量级,不能逐条写盘)。
bool AdmitConnection(const muduo::net::InetAddress &peer, SteadyTime now);

// 连接已经建出来之后才发现不能要(HandleConnectionEstablished 里的纵深防御分支):只计数,连接由调用方 forceClose。
void NoteRejectedAfterAccept(RejectReason reason);

// 会话刚插入会话表:计一次接纳;还没验签的会话领一个未验签占位(SessionInfo::preAuthSlot)。
void OnSessionOpened(SessionInfo &session, SteadyTime now);

// 验签成功:归还占位,握手耗时进直方图。没有占位(已验证会话重发验签、dev 免验签)时什么也不做,可重复调用。
void OnSessionVerified(SessionInfo &session, SteadyTime now);

// 进程级累计计数(gate loop 线程,单调不减)。供指标与单测读。
struct Counters
{
	uint64_t accepted{0};                                // 接纳并建了会话的连接
	std::array<uint64_t, kRejectReasonCount> rejected{}; // 被拒的连接,按原因
	uint64_t handshakes{0};                              // 验签成功、握手耗时进了直方图的连接

	bool operator==(const Counters &) const = default;
};
const Counters &GetCounters();
// 当前持有未验签占位的连接数(gate_unauthenticated_connections)。
uint64_t UnauthenticatedConnections();
// 生效的速率闸参数。
RateLimit CurrentRateLimit();

// 把连接类指标写进一次抓取(登记在进程的指标注册表里,由 /metrics 出口调用):
//   gate_connections_current / gate_connections_max      会话数与生效上限;
//   gate_unauthenticated_connections                     还没验签的连接数(未验签池占用,区分洪峰与攻击的判据);
//   gate_accepted_total                                  接纳的连接;rate() 就是新建连接速率;
//   gate_rejected_total{reason}                          被拒的连接,reason 是 RejectReason 的名字;
//   gate_accept_rate_limit_per_second / gate_accept_burst 生效的速率闸参数;
//   gate_handshake_duration_ms                           建连到验签成功的耗时直方图;
//   gate_preauth_reaped_total                            建连满握手期限仍未验签、被关掉的连接(client_heartbeat 的计数)。
// 标签只有 reason 一个封闭集合,不带对端地址、会话号与账号(AGENTS.md §9)。在 gate 主 loop 上执行,读本线程的计数。
void CollectMetrics(prometheus_text::Writer &writer);
// gate 启动时调一次,把 CollectMetrics 登记进进程的指标注册表;重复登记只记一行 ERROR。
void RegisterMetrics();

// 清空本线程的计数、直方图、关源标记与配置(回到未 Configure 的缺省),不动仍被会话持有的未验签占位。只给单测用。
void ResetForTesting();

} // namespace gate_connection_admission
