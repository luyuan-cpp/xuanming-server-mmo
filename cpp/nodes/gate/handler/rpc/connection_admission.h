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
#include <functional>
#include <optional>
#include <string>
#include <string_view>
#include <unordered_map>

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
	kSourceRate,       // 按来源前缀限速生效中(承压),这个前缀的新建连接超过了它的桶
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
	"source_rate",
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
// 规则、缺省值的推导与判定函数在 core 的 network/accept_admission.h(与 battle 直连同一份),这里引进来。
// gate 的池大小 n 就是 UnauthenticatedConnections()(RAII 占位,pre_auth_slot.h)。gate 特有的一条依据:
// login 在会话数到上限的 90% 时就不再往这台派人(gate-load-balancing-design.md),缺省预算(上限的 10%)正好是剩下的那一成,
// 未验签连接再多也挤不掉已验签会话的那 90%。
// 本端 loop 刚卡过(client_heartbeat.h LoopRecentlyStalled)时不淘汰:那时验签请求可能还在接收缓冲里没读。
using accept_admission::DecidePreAuth;
using accept_admission::DefaultPreAuthBudget;
using accept_admission::kDefaultMinEvictableAge;
using accept_admission::kEarlyDropStartPercent;
using accept_admission::PreAuthBudget;
using accept_admission::PreAuthVerdict;
using accept_admission::UniformBelow;

// 运维覆盖(不设 = 按 DefaultPreAuthBudget 由生效上限推导;取值范围见 accept_admission::PreAuthEnvNames):
//   GATE_PREAUTH_BUDGET            预算 B;只设它时早拒起点按新的 B 取一半;
//   GATE_PREAUTH_EARLY_DROP_START  早拒起点 S,不能大于预算;
//   GATE_PREAUTH_EVICTABLE_AGE_MS  可淘汰年龄(毫秒),短于 10 秒握手期限。
// 写错拒绝启动。正常洪峰碰到了早拒(GatePreauthBudgetRejecting 且握手 P99 也在变慢)时先调大前两个,不用改代码重新发版。
inline constexpr const char *kPreAuthBudgetEnv = "GATE_PREAUTH_BUDGET";
inline constexpr const char *kPreAuthEarlyDropStartEnv = "GATE_PREAUTH_EARLY_DROP_START";
inline constexpr const char *kPreAuthEvictableAgeEnv = "GATE_PREAUTH_EVICTABLE_AGE_MS";

// 由三个环境变量的原文与生效上限定出未验签预算。任一变量非法时返回 nullopt,error 里是给启动日志用的英文说明。
inline std::optional<PreAuthBudget> ResolvePreAuthBudget(const char *rawBudget, const char *rawEarlyDropStart,
														 const char *rawEvictableAgeMs, uint32_t effectiveMaxConnections,
														 std::string &error)
{
	return accept_admission::ResolvePreAuthBudget(
		{kPreAuthBudgetEnv, kPreAuthEarlyDropStartEnv, kPreAuthEvictableAgeEnv}, rawBudget, rawEarlyDropStart,
		rawEvictableAgeMs, effectiveMaxConnections, error);
}

// ---------------------------------------------------------------------------
// 按来源前缀的新建连接速率闸(设计稿 P2-2)
// ---------------------------------------------------------------------------
//
// 前面几道都是全局的:持续超限时,合法新连接与攻击连接抢同一个速率桶,谁来得多谁占得多。这一道按来源前缀
// (IPv4 /24、IPv6 /64)各给一个小令牌桶,让单个来源最多拿到全局速率的一成。照设计稿 §7 的约束:
//   - 限速率不限总量:CGNAT / 网吧 / 校园网 / 基站出口后面可能有几百上千个真玩家,不做每来源并发上限;
//   - 按 /24、/64 聚合:IPv6 按 /128 等于没做(单主机能在自己的 /64 里生成 2^64 个源地址);
//   - 缺省关闭:只有 gate 看得到玩家真实源地址时才有意义(G9:external 入口 + externalTrafficPolicy: Local)。
//     podip 形态、代理型 LB 后面看到的是节点或 LB 的地址,开了就会把所有人当成同一个来源限速;
//   - 开了也只在承压时生效(「告警后自动开启」):未验签池过了早拒起点,或全局速率闸在最近一个握手期限内拒过连接。
//     平时一条都不拦,不会误伤同一出口后面的大批玩家;
//   - 不封来源、不落持久状态(§7「拒绝 N 次就封 IP」是状态炸弹):每个前缀只是一个会自己回满的桶。
//
// 开启时的推荐参数(DefaultSourceRateLimit;没有压测数据,按推导先定):
//   持续速率 = 全局缺省持续速率的 1/10(上限 2 万时 20 条/秒;全局取下限 100 时为 10):至少要十个前缀一起来才吃得满全局速率闸;
//   桶深 = 持续速率 × 握手期限(200 / 100):单个前缀稳态下最多占住未验签预算的一成。
//   承压期间一个 /24 后面几百个真玩家同时重连,先放行一个桶深,之后按持续速率进,几十秒内接完。这是攻击期间的降级,平时不生效。
// 前缀表有上限(kMaxTrackedSources):满了先清掉桶已回满的前缀(每秒最多清一次),清不出位置时新前缀不登记、直接放行
// (fail-open,计 gate_source_untracked_total 并采样打日志)。只有极分散的来源(上万个前缀)才撑得满它,那时这一道帮不上忙,
// 全局的几道照常生效;拒掉新来源反而会把真玩家一起挡掉。
inline constexpr uint32_t kSourceShareDivisor = 10;
inline constexpr size_t kMaxTrackedSources = 65536;

constexpr RateLimit DefaultSourceRateLimit(uint32_t effectiveMaxConnections)
{
	const uint32_t deadlineSeconds = static_cast<uint32_t>(
		std::chrono::duration_cast<std::chrono::seconds>(client_liveness::kClientLiveness.handshakeTimeout).count());
	const uint32_t perSecond = (std::max)(uint32_t{1}, DefaultRateLimit(effectiveMaxConnections).perSecond / kSourceShareDivisor);
	return RateLimit{perSecond, perSecond * deadlineSeconds};
}

// 一个来源前缀:IPv4 取高 24 位,IPv6 取高 64 位。
struct SourcePrefix
{
	bool ipv6{false};
	uint64_t bits{0};

	bool operator==(const SourcePrefix &) const = default;
};

struct SourcePrefixHash
{
	size_t operator()(const SourcePrefix &prefix) const noexcept
	{
		return std::hash<uint64_t>{}(prefix.bits ^ (prefix.ipv6 ? 0x9E3779B97F4A7C15ULL : 0ULL));
	}
};

constexpr SourcePrefix SourcePrefixOfIpv4(uint32_t hostOrderAddress)
{
	return SourcePrefix{false, hostOrderAddress >> 8};
}

// address 是网络字节序的 16 个字节。IPv4 映射地址(::ffff:a.b.c.d)按 IPv4 算:同一台客户端经 IPv4 还是双栈套接字进来,
// 不该落进两个桶。
constexpr SourcePrefix SourcePrefixOfIpv6(const std::array<uint8_t, 16> &address)
{
	bool mapped = address[10] == 0xFF && address[11] == 0xFF;
	for (size_t i = 0; i < 10; ++i)
	{
		mapped = mapped && address[i] == 0;
	}
	if (mapped)
	{
		return SourcePrefixOfIpv4((uint32_t{address[12]} << 24) | (uint32_t{address[13]} << 16) |
								  (uint32_t{address[14]} << 8) | uint32_t{address[15]});
	}
	uint64_t bits = 0;
	for (size_t i = 0; i < 8; ++i)
	{
		bits = (bits << 8) | address[i];
	}
	return SourcePrefix{true, bits};
}

// 按前缀分桶的速率闸:每个前缀一个 AcceptRateGate(GCRA,起步是满桶)。判定阶段只问 WouldAdmit,不改状态;
// 所有闸都放行之后才 Take,被别的闸拒掉的连接不消耗这个前缀的名额。不加锁:只在 gate 的 accept 线程上用。
class SourceRateGate
{
public:
	explicit SourceRateGate(RateLimit limit, size_t maxTracked = kMaxTrackedSources)
		: limit_(limit), maxTracked_(maxTracked)
	{
	}

	bool WouldAdmit(const SourcePrefix &prefix, SteadyTime now) const
	{
		const auto it = gates_.find(prefix);
		return it == gates_.end() || it->second.CanTake(now);
	}

	// 放行之后记一笔。表满时先清掉桶已回满的前缀;仍然满就不登记、返回 false(这个前缀这次没被计入,调用方计一次 untracked)。
	bool Take(const SourcePrefix &prefix, SteadyTime now)
	{
		auto it = gates_.find(prefix);
		if (it == gates_.end())
		{
			if (gates_.size() >= maxTracked_)
			{
				PruneIfDue(now);
			}
			if (gates_.size() >= maxTracked_)
			{
				return false;
			}
			it = gates_.emplace(prefix, AcceptRateGate(limit_)).first;
		}
		it->second.TryTake(now);
		return true;
	}

	size_t Tracked() const { return gates_.size(); }
	const RateLimit &Limit() const { return limit_; }

private:
	// 桶已回满的前缀与没登记过等价,可以删。只在表满时清、每秒最多一次:撑满时不至于每条连接都扫一遍表。
	void PruneIfDue(SteadyTime now)
	{
		if (lastPruneAt_ && now - *lastPruneAt_ < std::chrono::seconds(1))
		{
			return;
		}
		lastPruneAt_ = now;
		std::erase_if(gates_, [now](const auto &entry) { return entry.second.Full(now); });
	}

	RateLimit limit_;
	size_t maxTracked_;
	std::unordered_map<SourcePrefix, AcceptRateGate, SourcePrefixHash> gates_;
	std::optional<SteadyTime> lastPruneAt_;
};

// 开关与参数(缺省关闭,理由见上):
//   GATE_SOURCE_RATE_PER_SECOND  设了就开启:单个前缀的持续速率,规范十进制 1..accept_admission::kMaxAcceptRatePerSecond;
//                                推荐值见 DefaultSourceRateLimit(启动日志会打出来);
//   GATE_SOURCE_BURST            单个前缀的桶深,规范十进制 1..accept_admission::kMaxAcceptBurst;不设时取持续速率 × 握手期限。
// 只设桶深不设速率、或写错都拒绝启动。
inline constexpr const char *kSourceRateEnv = "GATE_SOURCE_RATE_PER_SECOND";
inline constexpr const char *kSourceBurstEnv = "GATE_SOURCE_BURST";

// 解析两个变量。成功返回 true:limit 为空表示关闭,否则是开启后的参数;失败返回 false,error 里是给启动日志用的英文说明。
inline bool ResolveSourceRateLimit(const char *rawRate, const char *rawBurst, std::optional<RateLimit> &limit,
								   std::string &error)
{
	limit.reset();
	const auto rate = ParseOptionalPositive(rawRate, accept_admission::kMaxAcceptRatePerSecond);
	if (!rate)
	{
		error = std::string(kSourceRateEnv) + " must be an integer 1.." +
				std::to_string(accept_admission::kMaxAcceptRatePerSecond) + " (leave it unset to keep per-source limiting off)";
		return false;
	}
	const auto burst = ParseOptionalPositive(rawBurst, accept_admission::kMaxAcceptBurst);
	if (!burst)
	{
		error = std::string(kSourceBurstEnv) + " must be an integer 1.." + std::to_string(accept_admission::kMaxAcceptBurst) +
				" (leave it unset for rate x handshake deadline)";
		return false;
	}
	if (*rate == 0)
	{
		if (*burst != 0)
		{
			error = std::string(kSourceBurstEnv) + " is set but " + kSourceRateEnv +
					" is not; per-source limiting is enabled by setting the rate";
			return false;
		}
		return true;
	}
	const uint64_t deadlineSeconds = static_cast<uint64_t>(
		std::chrono::duration_cast<std::chrono::seconds>(client_liveness::kClientLiveness.handshakeTimeout).count());
	const uint64_t derivedBurst = (std::min)(uint64_t{accept_admission::kMaxAcceptBurst}, uint64_t{*rate} * deadlineSeconds);
	limit = RateLimit{*rate, *burst != 0 ? *burst : static_cast<uint32_t>(derivedBurst)};
	return true;
}

// 判定所需的当前状态。
struct AdmissionState
{
	bool sourceClosed{false};   // 已关源
	size_t currentSessions{0};  // 会话表大小。与连接一一对应:建连回调插入,断开回调删除,都在 loop 线程上同步完成
	uint32_t maxConnections{0}; // 生效上限(EffectiveMaxConnections)
	uint64_t unauthenticated{0}; // 持有未验签占位的连接数
	// 其中最老一条已建连多久。没有未验签连接、或此刻不允许淘汰(本端 loop 刚卡过)时为空。
	std::optional<SteadyTime::duration> oldestUnauthenticatedAge;
	PreAuthBudget preAuthBudget{DefaultPreAuthBudget(EffectiveMaxConnections(0))};
	bool sourceOverRate{false}; // 按来源限速生效中(开启且承压),而这条连接的来源前缀已超过它的桶
};

// 判定结果:拒绝(带原因),或放行;放行时可能要先淘汰最老的一条未验签连接给它让位。
struct Admission
{
	std::optional<RejectReason> rejected;    // 空 = 放行
	bool evictOldestUnauthenticated{false}; // 放行,并且要先淘汰最老的一条未验签连接

	bool operator==(const Admission &) const = default;
};

// 判定一条刚 accept 进来的连接,顺序:关源 → 总量 → 按来源限速 → 未验签预算 → 速率。放行时已从速率闸取走一个名额;
// 拒绝时不动速率闸:满员、来源超速、到预算、早拒的连接都不消耗名额,腾出位置之后名额还在。
// 来源排在预算前面:超速前缀的连接不会让别人的老连接被淘汰,也不消耗早拒的随机数。
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
	if (state.sourceOverRate)
	{
		return Admission{.rejected = RejectReason::kSourceRate};
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

// 启动时调一次(安全门禁之后、客户端回调装上之前):记下生效上限,装好速率闸(桶是满的)、未验签预算与按来源限速
// (sourceRateLimit 为空 = 关闭)。三组参数都由 main.cpp 从环境变量解析(写错拒启)。
void Configure(uint32_t configuredMaxConnections, RateLimit rateLimit, PreAuthBudget preAuthBudget,
			   std::optional<RateLimit> sourceRateLimit);
// 同上,未验签预算按上限推导(DefaultPreAuthBudget),按来源限速关闭。单测用。
void Configure(uint32_t configuredMaxConnections, RateLimit rateLimit);

// 关源:从这一刻起新连接一律在 accept 处拒绝(reason = shutting_down)。停机 / 维护关源时调一次,不可撤销
// (关源之后进程随即排空退出)。状态放在本模块里,准入回调因此不需要捕获任何对象。
void CloseSource();

// Acceptor 准入回调的函数体(main.cpp 经 TcpServer::setAdmissionCallback 装上)。放行返回 true;拒绝返回 false,
// 并按原因计数、采样打日志(被拒的量就是攻击流量的量级,不能逐条写盘)。
// 判定要求先淘汰时,放行之前收回最老那条未验签连接的占位,再经会话里的 weak 连接 forceClose 它:
// 会话不在这里删,留给随后的断开回调(没验签的会话没有角色,断开回调只删会话)。本端 loop 刚卡过时不淘汰。
// 按来源限速开启且承压时,从 peer 取来源前缀问它的桶;放行之后才给这个前缀记一笔。
bool AdmitConnection(const muduo::net::InetAddress &peer, SteadyTime now);

// 来源前缀(IPv4 /24、IPv6 /64)。不是 IPv4 / IPv6 的地址返回空,这样的连接不按来源限速。
std::optional<SourcePrefix> SourcePrefixOf(const muduo::net::InetAddress &peer);

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
	uint64_t sourceUntracked{0}; // 按来源限速生效时,前缀表满、没能登记就放行的连接(fail-open)

	bool operator==(const Counters &) const = default;
};
const Counters &GetCounters();
// 当前持有未验签占位的连接数(gate_unauthenticated_connections)。
uint64_t UnauthenticatedConnections();
// 生效的速率闸参数。
RateLimit CurrentRateLimit();
// 生效的未验签预算。
PreAuthBudget CurrentPreAuthBudget();
// 按来源限速的参数;关闭时为空。
std::optional<RateLimit> CurrentSourceRateLimit();

// 把连接类指标写进一次抓取(登记在进程的指标注册表里,由 /metrics 出口调用):
//   gate_connections_current / gate_connections_max      会话数与生效上限;
//   gate_unauthenticated_connections                     还没验签的连接数(未验签池占用,区分洪峰与攻击的判据);
//   gate_accepted_total                                  接纳的连接;rate() 就是新建连接速率;
//   gate_rejected_total{reason}                          被拒的连接,reason 是 RejectReason 的名字;
//   gate_accept_rate_limit_per_second / gate_accept_burst 生效的速率闸参数;
//   gate_handshake_duration_ms                           建连到验签成功的耗时直方图;
//   gate_preauth_reaped_total                            建连满握手期限仍未验签、被关掉的连接(client_heartbeat 的计数);
//   gate_preauth_evicted_total                           承压时被淘汰、给新连接让位的未验签连接;
//   gate_preauth_budget / gate_preauth_early_drop_start / gate_preauth_evictable_age_ms 生效的未验签预算、早拒起点与可淘汰年龄;
//   gate_source_rate_limit_per_second / gate_source_burst 按来源限速的参数(关闭时为 0);
//   gate_source_rate_active                              最近一次准入判定时按来源限速是否在生效(开启且承压为 1);
//   gate_source_tracked / gate_source_untracked_total    登记着的来源前缀数,与前缀表满、没登记就放行的连接。
// 标签只有 reason 一个封闭集合,不带对端地址、来源前缀、会话号与账号(AGENTS.md §9)。在 gate 主 loop 上执行,读本线程的计数。
void CollectMetrics(prometheus_text::Writer &writer);
// gate 启动时调一次,把 CollectMetrics 登记进进程的指标注册表;重复登记只记一行 ERROR。
void RegisterMetrics();

// 清空本线程的计数、直方图、关源标记、配置、淘汰索引与注入的随机源(回到未 Configure 的缺省),
// 不动仍被会话持有的未验签占位(它们之后不会再被淘汰选中)。只给单测用。
void ResetForTesting();
// 换掉按概率早拒用的随机源(传 nullptr 换回缺省的伪随机数)。只给单测用,ResetForTesting 也会换回缺省。
void SetEarlyDropRandomForTesting(UniformBelow uniformBelow);

} // namespace gate_connection_admission
