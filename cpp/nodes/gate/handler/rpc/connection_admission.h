#pragma once

// gate 客户端连接准入(docs/design/gate-connection-admission-control.md §6 的 P0-2 后一半、P1-1 ~ P1-4 与 P2-4)。
//
// 判定发生在 muduo Acceptor 的准入回调里:accept 成功之后、TcpConnection 建出来之前。被拒的连接当场以 RST 关闭,
// gate 不为它建任何对象,内核也不留 TIME_WAIT。判据都不需要知道对端是谁,按顺序:
//   1. 关源:停机 / 维护关源之后不再接新连接;
//   2. 总量:连接数(会话 + 未验签)到上限(GateMaxConnections;dev / test 的 0 = 只剩 session id 空间的硬上限);
//   3. 未验签预算:还没验签的连接单独一个池,承压时最老的先让位,腾不出位置时按概率早拒、到预算全拒(见 PreAuthBudget);
//   4. 速率:新建连接的全局令牌桶,挡住「不停建连又立刻关」这种总量闸永远碰不到的流量。
// 需要知道对端是谁的判据(每来源限制)不在这里,见设计稿 P2-2。
//
// 放进来的连接先只有一条未验签记录(票据先行,设计稿 P2-4):验签成功才发 session_id、建 SessionInfo。
// 两步的入口在本文件(OpenPreAuthConnection / OpenVerifiedSession),记录存在 SessionManager::preAuthConnections()。
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
#include "muduo/net/TcpConnection.h"
#include "network/accept_admission.h"
#include "network/client_liveness.h"
#include "network/network_utils.h"

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
// gate 的池大小 n 就是 UnauthenticatedConnections(),即未验签表的大小(设计稿 P2-4)。gate 特有的一条依据:
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
// 规则、推荐值、前缀表与「只限公网来源」在 core 的 network/accept_admission.h(与 battle 直连同一份),这里引进来。
// gate 只在看得到玩家真实源地址的形态里有用:external 入口 + externalTrafficPolicy: Local(G9)。podip 形态、代理型 LB
// 与小游戏 WebSocket 的边缘 nginx 后面看到的都是私网地址,按规则本来就不限。
using accept_admission::DefaultSourceRateLimit;
using accept_admission::IsPublicSourcePrefix;
using accept_admission::kMaxTrackedSources;
using accept_admission::kSourceShareDivisor;
using accept_admission::RateLimitedSourceOf;
using accept_admission::SourcePrefix;
using accept_admission::SourcePrefixHash;
using accept_admission::SourcePrefixOf;
using accept_admission::SourcePrefixOfIpv4;
using accept_admission::SourcePrefixOfIpv6;
using accept_admission::SourceRateGate;

// 开关与参数(缺省关闭,取值范围见 accept_admission::SourceRateEnvNames):
//   GATE_SOURCE_RATE_PER_SECOND  设了就开启:单个前缀的持续速率;推荐值见 DefaultSourceRateLimit(启动日志会打出来);
//   GATE_SOURCE_BURST            单个前缀的桶深;不设时取持续速率 × 握手期限。
// 只设桶深不设速率、或写错都拒绝启动。
inline constexpr const char *kSourceRateEnv = "GATE_SOURCE_RATE_PER_SECOND";
inline constexpr const char *kSourceBurstEnv = "GATE_SOURCE_BURST";

// 解析两个变量。成功返回 true:limit 为空表示关闭,否则是开启后的参数;失败返回 false,error 里是给启动日志用的英文说明。
inline bool ResolveSourceRateLimit(const char *rawRate, const char *rawBurst, std::optional<RateLimit> &limit,
								   std::string &error)
{
	return accept_admission::ResolveSourceRateLimit({kSourceRateEnv, kSourceBurstEnv}, rawRate, rawBurst, limit, error);
}

// 判定所需的当前状态。
struct AdmissionState
{
	bool sourceClosed{false};      // 已关源
	size_t currentConnections{0};  // 连接数 = 会话数 + 未验签连接数(SessionManager::ClientConnectionCount),都在 loop 线程上同步增删
	uint32_t maxConnections{0};    // 生效上限(EffectiveMaxConnections)
	uint64_t unauthenticated{0};   // 未验签连接数(未验签表的大小)
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
	if (state.currentConnections >= state.maxConnections)
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
// 判定要求先淘汰时,放行之前摘掉最老那条未验签记录(这一刻起它不再占池),再经记录里的 weak 连接 forceClose 它;
// 随后的断开回调找不到记录,什么都不做。本端 loop 刚卡过时不淘汰。
// 按来源限速开启且承压时,从 peer 取来源前缀(只认公网地址,RateLimitedSourceOf)问它的桶;放行之后才给这个前缀记一笔。
bool AdmitConnection(const muduo::net::InetAddress &peer, SteadyTime now);

// 连接已经建出来之后才发现不能要(HandleConnectionEstablished 里的纵深防御分支):只计数,连接由调用方 forceClose。
void NoteRejectedAfterAccept(RejectReason reason);

// ---------------------------------------------------------------------------
// 票据先行(设计稿 P2-4):会话表与 session_id 发号器挡在票据之后
// ---------------------------------------------------------------------------
//
// 建连到验签成功之前,一条连接只有一条未验签记录(session/comp/pre_auth_connection.h:weak 连接、建连时刻,键是代次),
// 连接上下文是 PreAuthConnectionKey;不发 session_id、不建 SessionInfo。未验签预算的池大小与承压淘汰的最老一条、
// 握手期限(gate_client_heartbeat 从表头扫)、验签前单帧上限的判据、两个连接数指标都按这张表。

// 建连(HandleConnectionEstablished 过了纵深防御之后):登记一条未验签记录,代次现分配(gate_lane::NextIncarnation),
// 连接上下文换成它的键,计一次接纳。dev 免验签不走这里,建连即 OpenVerifiedSession。
void OpenPreAuthConnection(const muduo::net::TcpConnectionPtr &conn, SteadyTime now);

// 发号、建会话。两处调用:DispatchTokenVerify 的票据校验全部通过之后;dev 免验签建连时(ticketKey 为空)。
//   1. 发 session_id:跳过保留哨兵 kInvalidSessionId 与仍在用的号(发号器回绕之后的碰撞检查)。总量闸保证会话数小于上限、
//      上限不超过号空间,所以空号一定找得到;
//   2. ticketKey 不为空时按票据签名占位(SessionManager::ClaimTicket,一张票同一时刻只开一条连接)。这张票已被另一条活着的连接
//      占着就到此为止、返回空:号只是跳过一个,没建任何状态,未验签记录原样留着,调用方按 token_in_use 拒绝并关连接,
//      断开回调再摘记录;
//   3. 建 SessionInfo(verified、weak 连接、ticketKey、代次:沿用未验签记录的键,dev 免验签现分配),连接上下文换成 session_id;
//      有未验签记录的摘掉记录、握手耗时进直方图,没有的(dev 免验签)计一次接纳。
// 返回新会话的 session_id。心跳参数、票据带来的其余字段由调用方随后填(OnTokenVerified 等)。
std::optional<SessionId> OpenVerifiedSession(const muduo::net::TcpConnectionPtr &conn, const std::string &ticketKey,
											 SteadyTime now);

// 断开回调的第一步(HandleConnectionDisconnection):这条连接还没验签,就摘掉它的未验签记录(被淘汰、握手超时关掉的已经摘过)
// 并返回 true,调用方随即返回:没有会话、没有角色、没向 login 发过登录,不通知 login / scene。已有会话的连接返回 false。
bool ForgetPreAuthConnection(const muduo::net::TcpConnectionPtr &conn);

// 这条连接是否已验签:上下文是 session_id、且会话在表里(会话只在验签成功后建)。还没验签的连接上下文是未验签记录的键,
// 准入时被拒的是 kInvalidSessionId,都返回 false。给编解码器的验签前单帧上限用(ProtobufCodec::setPreAuthMessageLimit,
// accept_admission::kPreAuthMaxMessageBytes,准入设计 G10)。只读、不打日志:调用频率由公网流量决定。
bool IsVerifiedConnection(const muduo::net::TcpConnectionPtr &conn);

// 进程级累计计数(gate loop 线程,单调不减)。供指标与单测读。
struct Counters
{
	uint64_t accepted{0}; // 接纳的连接:登记了未验签记录,或 dev 免验签建连即建了会话
	std::array<uint64_t, kRejectReasonCount> rejected{}; // 被拒的连接,按原因
	uint64_t handshakes{0};                              // 验签成功、握手耗时进了直方图的连接
	uint64_t preAuthEvicted{0};                          // 承压时被淘汰、给新连接让位的未验签连接
	uint64_t sourceUntracked{0}; // 按来源限速生效时,前缀表满、没能登记就放行的连接(fail-open)

	bool operator==(const Counters &) const = default;
};
const Counters &GetCounters();
// 当前未验签连接数(gate_unauthenticated_connections),即未验签表的大小。
uint64_t UnauthenticatedConnections();
// 生效的速率闸参数。
RateLimit CurrentRateLimit();
// 生效的未验签预算。
PreAuthBudget CurrentPreAuthBudget();
// 按来源限速的参数;关闭时为空。
std::optional<RateLimit> CurrentSourceRateLimit();

// 把连接类指标写进一次抓取(登记在进程的指标注册表里,由 /metrics 出口调用):
//   gate_connections_current / gate_connections_max      连接数(会话 + 未验签,总量上限按它判)与生效上限;
//   gate_unauthenticated_connections                     还没验签的连接数(未验签表的大小,区分洪峰与攻击的判据);
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

// 清空本线程的计数、直方图、关源标记、配置与注入的随机源(回到未 Configure 的缺省)。会话表与未验签表在 tlsSessionManager 里,
// 不归这里清,用例自己清。只给单测用。
void ResetForTesting();
// 换掉按概率早拒用的随机源(传 nullptr 换回缺省的伪随机数)。只给单测用,ResetForTesting 也会换回缺省。
void SetEarlyDropRandomForTesting(UniformBelow uniformBelow);

} // namespace gate_connection_admission
