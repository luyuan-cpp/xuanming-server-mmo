#pragma once

// 客户端面在 accept 处准入的共享纯逻辑。gate 的大厅连接与 battle 的战斗直连共用这一份
// (docs/design/gate-connection-admission-control.md P1-1 ~ P1-4;battle 见 turn-based-battle-server.md D27)。
//
// 只放两个客户端面规则相同、要一起改的部分:新建连接速率闸(GCRA)、缺省速率的推导、覆盖它的环境变量解析、
// 握手耗时直方图、未验签预算、按来源前缀限速。各节点自己的部分留在各节点:拒绝原因(指标 <节点>_rejected_total{reason}
// 的标签集合,两边不同)、判定函数(两边都按「关源 → 总量 → 按来源 → 未验签预算 → 新建速率」,gate 另有自己的兜底原因)、
// 会话表、指标名与日志。装配:gate 在 cpp/nodes/gate/handler/rpc/connection_admission.{h,cpp},battle 在
// cpp/nodes/battle/client/battle_connection_admission.{h,cpp}。
//
// 只依赖 STL、client_liveness.h 与 muduo 的 InetAddress(只有 SourcePrefixOf 用),不读时钟(时刻由调用方给)。单测在
// cpp/tests/rpc_controller_test/accept_admission_test.cpp。不加锁:两个客户端面的 accept 都只在一个 EventLoop 线程上。
//
// 本文件里的 std::max 一律写成 (std::max):gate.vcxproj 没定义 NOMINMAX,muduo 的 Windows 适配头会带进 <Windows.h>
// 的 min / max 宏(同 gate handler/event/scene_route_helper.h 的说明),加括号之后与包含顺序无关。

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

#include "muduo/net/Endian.h"
#include "muduo/net/InetAddress.h"
#include "network/client_liveness.h"

namespace accept_admission
{

using SteadyTime = std::chrono::steady_clock::time_point;

// 新建连接速率闸的参数。
struct RateLimit
{
	uint32_t perSecond; // 持续速率:每秒放行多少条新连接
	uint32_t burst;     // 桶深:攒满之后可以一口气放行多少条

	bool operator==(const RateLimit &) const = default;
};

// 缺省速率的两个常数,取值依据(设计稿 P1-3;没有压测数据之前按这套推导定,压测后只改这里或用环境变量覆盖):
//
//   桶深 = 连接上限。一个客户端面合法的最坏洪峰在量级上就是连接上限:gate 是重启后从零接回一整台的人
//   (设计稿 §8 的冷启动 + 全量重连),battle 是一批房间同时开局、参战者与观众一起来连直连。
//   桶深取上限,总量闸会放行的合法洪峰就永远不会被速率闸拒绝;速率闸只限制上限之外的周转。
//
//   持续速率 = 连接上限 / kCapacityRefillSeconds(每秒 1% 的容量),下限 kMinAcceptRatePerSecond:
//     - 握手期限是 10s(client_liveness.h,两个客户端面同值):一个从不验签的来源稳态下最多占住 速率 × 10s = 上限的 10%,
//       正好是设计稿 P0-2 给未验签连接定的预算上沿(5%~10%);
//     - 合法的稳态周转远低于它:gate 2 万人、人均在线半小时,进出约 11 条/秒,加上断线重连也只有几十条/秒;
//       battle 4096 条直连、一局几分钟,进出约 15~30 条/秒,落在下限 100 条/秒之内;
//     - 任意时间窗 T 内接纳的连接数 ≤ 上限 + 速率 × T,「不停建连又立刻关」的开销因此有界。
//   下限是给上限配得很小的环境(本机、单测把上限调到个位数验证总量闸,battle 的四千级上限)留的:
//   那时该起作用的是总量闸,不是速率闸。
inline constexpr uint32_t kCapacityRefillSeconds = 100;
inline constexpr uint32_t kMinAcceptRatePerSecond = 100;
// 环境变量覆盖时的取值范围。上界只为挡住误写:远高于单个 IO 线程接得动的速率,也保证速率闸里的时长运算不溢出。
inline constexpr uint32_t kMaxAcceptRatePerSecond = 1'000'000;
inline constexpr uint32_t kMaxAcceptBurst = 1'000'000;

constexpr RateLimit DefaultRateLimit(uint32_t effectiveMaxConnections)
{
	const uint32_t perSecond = (std::max)(kMinAcceptRatePerSecond, effectiveMaxConnections / kCapacityRefillSeconds);
	return RateLimit{perSecond, (std::max)(effectiveMaxConnections, perSecond)};
}

// 解析一个可选的正整数环境变量:nullptr / 空串 → 0(未设);规范十进制 1..upperBound → 该值;
// 其余(非数字、前导零、0、越界、带空白或符号)→ nullopt,调用方按启动配置错误拒启。
constexpr std::optional<uint32_t> ParseOptionalPositive(const char *raw, uint32_t upperBound)
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
	if (value == 0 || value > upperBound)
	{
		return std::nullopt;
	}
	return static_cast<uint32_t>(value);
}

// 一个客户端面的两个覆盖变量名,以及它的连接上限配置项名。只用来拼启动错误说明,三者都由节点给出。
// 没有「0 = 关闭」:速率闸不留一个拼错就整个失效的开关,要放开就写一个足够大的数。
struct RateLimitEnvNames
{
	std::string_view rate;           // 持续速率,规范十进制 1..kMaxAcceptRatePerSecond(例:GATE_ACCEPT_RATE_PER_SECOND)
	std::string_view burst;          // 桶深,规范十进制 1..kMaxAcceptBurst;不设时取 max(连接上限, 持续速率)(例:GATE_ACCEPT_BURST)
	std::string_view maxConnections; // 连接上限的配置项名(例:GateMaxConnections)
};

// 由两个环境变量的原文与生效上限定出速率闸参数。任一变量非法时返回 nullopt,error 里是给启动日志用的英文说明。
inline std::optional<RateLimit> ResolveRateLimit(const RateLimitEnvNames &names, const char *rawRate,
												 const char *rawBurst, uint32_t effectiveMaxConnections,
												 std::string &error)
{
	const auto rate = ParseOptionalPositive(rawRate, kMaxAcceptRatePerSecond);
	if (!rate)
	{
		error = std::string(names.rate) + " must be an integer 1.." + std::to_string(kMaxAcceptRatePerSecond) +
				" (leave it unset for the default derived from " + std::string(names.maxConnections) + ")";
		return std::nullopt;
	}
	const auto burst = ParseOptionalPositive(rawBurst, kMaxAcceptBurst);
	if (!burst)
	{
		error = std::string(names.burst) + " must be an integer 1.." + std::to_string(kMaxAcceptBurst) +
				" (leave it unset for max(" + std::string(names.maxConnections) + ", rate))";
		return std::nullopt;
	}
	RateLimit limit = DefaultRateLimit(effectiveMaxConnections);
	if (*rate != 0)
	{
		limit.perSecond = *rate;
	}
	limit.burst = *burst != 0 ? *burst : (std::max)(effectiveMaxConnections, limit.perSecond);
	return limit;
}

// 新建连接速率闸:GCRA(通用信元速率算法,与令牌桶等价;gate handler/event/world_chat_push_budget.h 是同一算法,
// 那边的参数是编译期常量)。只记「理论到达时刻」tat:每放行一条往后挪一个标准间隔;tat 领先当前时刻超过
// (burst - 1) 个间隔就拒绝。全用整数时长,没有浮点累计误差;拒绝不改状态,所以被拒的洪水不会把之后的合法连接也饿死。
// 起步时桶是满的。时刻由调用方给(单调时钟,单测注入);读数倒退(调用方传错)时只会多拒绝,不会多放行。
// 不加锁:accept 只在一个 EventLoop 线程上。
class AcceptRateGate
{
public:
	explicit AcceptRateGate(RateLimit limit) { Reset(limit); }

	// 换参数,并把桶重新装满。perSecond / burst 为 0 时按 1 处理(调用方都经 ResolveRateLimit,不会给 0)。
	void Reset(RateLimit limit)
	{
		this->limit = RateLimit{(std::max<uint32_t>)(limit.perSecond, 1), (std::max<uint32_t>)(limit.burst, 1)};
		const SteadyTime::duration oneSecond = std::chrono::duration_cast<SteadyTime::duration>(std::chrono::seconds(1));
		const SteadyTime::duration perConnection = oneSecond / static_cast<SteadyTime::duration::rep>(this->limit.perSecond);
		interval = (std::max)(perConnection, SteadyTime::duration(1));
		tolerance = interval * static_cast<SteadyTime::duration::rep>(this->limit.burst - 1);
		tat = SteadyTime{};
	}

	// 取一个名额。取不到返回 false:这条连接不接。
	bool TryTake(SteadyTime now)
	{
		if (!CanTake(now))
		{
			return false;
		}
		tat = (std::max)(tat, now) + interval;
		return true;
	}

	// 现在取不取得到名额,不改状态。按来源分桶时(gate 的 SourceRateGate)判定阶段只问,所有闸都放行之后才 TryTake,
	// 被别的闸拒掉的连接因此不消耗这个桶的名额。
	bool CanTake(SteadyTime now) const { return (std::max)(tat, now) - now <= tolerance; }

	// 桶是满的(空闲得够久,攒满了一个突发)。这时丢掉这个闸与重新建一个等价,按来源分桶时据此清表。
	bool Full(SteadyTime now) const { return tat <= now; }

	const RateLimit &Limit() const { return limit; }

private:
	RateLimit limit{};
	SteadyTime::duration interval{};
	SteadyTime::duration tolerance{};
	// 理论到达时刻。时钟零点 = 桶是满的:第一条一定放行,并且立刻有一个完整的突发可用。
	SteadyTime tat{};
};

// 握手耗时直方图(建连到验签成功)的桶上界,毫秒。最大的有限上界就是握手期限:期限内完成的握手都落在有限桶里,
// 超过的连接已被握手期限关掉。1s 以上分得细,是为了用 P99.9 复核握手期限该取多少(设计稿 P1-4)。
// prometheus_text::Writer 只写整数,所以单位取毫秒而不是秒。
inline constexpr auto kHandshakeUpperBoundsMs =
	std::to_array<uint64_t>({10, 25, 50, 100, 250, 500, 1000, 2000, 3000, 5000, 7500, 10000});
inline constexpr size_t kHandshakeBucketCount = kHandshakeUpperBoundsMs.size() + 1;
static_assert(std::chrono::milliseconds(kHandshakeUpperBoundsMs.back()) == client_liveness::kClientLiveness.handshakeTimeout,
			  "the largest finite handshake bucket must equal the handshake deadline; change both together");

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

// 握手耗时的累计,形状就是 prometheus_text::Writer::Histogram 要的入参(上界用 kHandshakeUpperBoundsMs)。
// 只记验签成功的连接;没验签就断开或被握手期限关掉的不进来,它们由各节点的收割计数与未验签连接数反映。
struct HandshakeHistogram
{
	std::array<uint64_t, kHandshakeBucketCount> buckets{}; // 各桶自己的计数(不累计),最后一个是超过最大上界的部分
	uint64_t sumMs{0};                                     // 耗时之和,毫秒

	// 记一次握手:openedAt 是建连时刻,verifiedAt 是验签成功时刻,都由调用方给(单调时钟)。
	// 倒退时按 0 记,不让一个负数变成巨大的无符号值。
	void Observe(SteadyTime openedAt, SteadyTime verifiedAt)
	{
		const uint64_t elapsedMs =
			verifiedAt <= openedAt
				? 0
				: static_cast<uint64_t>(
					  std::chrono::duration_cast<std::chrono::milliseconds>(verifiedAt - openedAt).count());
		++buckets[HandshakeBucketIndex(elapsedMs)];
		sumMs += elapsedMs;
	}
};

// ---------------------------------------------------------------------------
// 未验签连接的独立预算(gate 准入设计稿 P0-2;gate 大厅连接与 battle 直连同一份规则)
// ---------------------------------------------------------------------------
//
// 建连之后、验签(battle:票据握手或出示探测授权)成功之前的连接单独算一个池,大小 n 由各节点给。
// 池上有两个点:早拒起点 S 与满点 B(预算)。新连接来时:
//   1. n < S:不承压,放行;
//   2. n ≥ S,且最老的一条未验签连接已建连满 minEvictableAge:淘汰它,放行新连接(LRU:攻击者占着不验签的老连接先死);
//   3. 没有够老的可以让位、n ≥ B:拒绝(preauth_budget);
//   4. 没有够老的可以让位、S ≤ n < B:按概率拒绝,概率从 S 处的 0 线性升到 B 处的 100%(preauth_early_drop)。
// 先淘汰、后早拒:能腾出位置就不拒新连接。只有池里全是刚建的连接(一次突发)时才拒,而且拒绝率随占用平滑上升,
// 不是到 B 时一下子从 0 跳到 100%。本端 loop 刚卡过时调用方不给最老一条的年龄(client_liveness.h LoopStallWatch):
// 那时「建连多久还没验签」不可信,验签请求可能还在接收缓冲里没读。
//
// 缺省取值(没有压测数据,按下面的推导先定;实测后改常数,或用节点的覆盖变量):
//   B = 缺省持续速率 × 握手期限:上限 2 万时 200 条/秒 × 10 秒 = 2000(上限的 10%);上限不到 1 万时跟着速率下限,为 1000。
//     - gate 准入设计稿给的区间是上限的 5% ~ 10%,取上沿;
//     - 这正是速率闸放得进来的稳态流量在一个握手期限里能攒下的最多未验签连接:稳态下池到不了 B,
//       预算只在速率闸的突发额度被花掉的那几秒里起作用;
//     - 用缺省速率而不是覆盖后的速率:压测时调高速率闸不该顺带放大未验签预算。
//   S = B 的一半:
//     - 合法洪峰的未验签池很小:客户端建连后立刻发验签,accept 时验签请求多半已在接收缓冲里,下一轮 loop 就处理完。
//       gate 最坏的冷启动全量重连按 2 万条在 5 秒内连进来、95% 的连接 50ms 验签、5% 首个数据段丢包重传要 1.5 秒估,
//       池 ≈ 4000 × (0.95 × 0.05 + 0.05 × 1.5) ≈ 490 条,是上限的 2.5%,起点留了一倍余量;
//     - sshd 缺省 MaxStartups 10:30:100 的起点只有满点的 10%,那是给很少有人同时登录的 sshd 定的;
//       客户端面要扛重连风暴,起点必须高于合法洪峰;
//     - 上限配得很小(本机、验收把上限调到几十)时起点 500 高于上限,这一道不起作用,该起作用的是总量闸
//       (与速率闸设下限同一个用意)。
//   早拒的曲线:sshd 的公式 p = rate + (100 - rate) × (n - S) / (B - S) 取 rate = 0,也就是 Envoy overload manager
//     scaled trigger 的形状(S 处 0、B 处 100%,中间线性)。不取 sshd 缺省的 30% 起跳:起点已经高于合法洪峰的估计,
//     一次刚越过起点的合法洪峰不该立刻被拒掉三成。
//   minEvictableAge = 3 秒:
//     - 与 GateHandshakeSlow / BattleHandshakeSlow 告警(握手 P99 > 3 秒)同值:建连 3 秒还没验签的落在最慢的 1% 里;
//     - 容得下首个数据段连丢两次:三次握手已给出一个 RTT 样本,重传超时约 3 × RTT(Linux 下限 200ms,逐次翻倍),
//       RTT 在 300ms 以内时两次重传都落在 3 秒内;
//     - 远小于 10 秒的握手期限:承压时未验签连接活 3 秒左右就让位,不再占满 10 秒。这对应 Envoy overload manager 的
//       reduce_timeouts(承压时把空闲超时缩到下限 min_timeout),只是按「谁最老」一条一条执行;
//     - 速率闸放进来的稳态流量下池停在 S,每条连接活 S / 持续速率 = 5 秒才轮到它让位,比 3 秒长:
//       淘汰换掉的只是 5 秒还没验签的连接。
//   与速率闸的先后:各节点都把预算排在速率前面判,被预算拒掉的连接不消耗速率名额;淘汰只在速率闸也放行之后才执行。
inline constexpr uint32_t kEarlyDropStartPercent = 50; // S 占 B 的百分比
inline constexpr std::chrono::milliseconds kDefaultMinEvictableAge{3000};
static_assert(kDefaultMinEvictableAge < client_liveness::kClientLiveness.handshakeTimeout,
			  "eviction must act before the handshake deadline reaps the connection anyway");

struct PreAuthBudget
{
	uint32_t earlyDropStart; // S:未验签连接数到这里开始承压(先淘汰够老的,没有够老的就按概率拒)
	uint32_t full;           // B:预算;到这里且没有够老的可以让位就一律拒
	// 建连满这么久还没验签,承压时才可以被淘汰。必须短于握手期限(ResolvePreAuthBudget 核过)。
	std::chrono::milliseconds minEvictableAge{kDefaultMinEvictableAge};

	bool operator==(const PreAuthBudget &) const = default;
};

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
	kAdmit,            // 不承压;或承压、按概率放行了
	kAdmitEvictOldest, // 承压:淘汰最老的一条未验签连接,给这条让位
	kRejectEarlyDrop,  // 承压、没有够老的可以让位,按概率拒了
	kRejectBudget,     // 到预算、没有够老的可以让位
};

// 返回 [0, bound) 里均匀的一个整数(bound ≥ 1)。按概率早拒用:装配层用线程局部的伪随机数,单测注入定值。
using UniformBelow = uint32_t (*)(uint32_t bound);

// 未验签预算这一道。unauthenticated 是池里的连接数;oldestAge 是其中最老一条已建连多久,没有未验签连接、
// 或调用方此刻不允许淘汰(本端 loop 刚卡过)时为空。只在早拒带里、超过起点时取一次随机数。
inline PreAuthVerdict DecidePreAuth(const PreAuthBudget &budget, uint64_t unauthenticated,
									std::optional<SteadyTime::duration> oldestAge, UniformBelow uniformBelow)
{
	if (unauthenticated < budget.earlyDropStart)
	{
		return PreAuthVerdict::kAdmit;
	}
	if (oldestAge && *oldestAge >= budget.minEvictableAge)
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

// 一个客户端面覆盖未验签预算的三个变量名(都由节点给,只用来拼启动错误说明与日志)。不设 = DefaultPreAuthBudget 推导。
//   budget:         预算 B,规范十进制 1..kMaxPreAuthBudget;只设它时起点按新的 B 取一半;
//   earlyDropStart: 早拒起点 S,规范十进制 1..B;
//   evictableAgeMs: 可淘汰年龄,毫秒,规范十进制 1..(握手期限 - 1)。
// 没有「0 = 关闭」:要让预算几乎不起作用就把起点与预算写成上限那么大。
struct PreAuthEnvNames
{
	std::string_view budget;         // 例:GATE_PREAUTH_BUDGET
	std::string_view earlyDropStart; // 例:GATE_PREAUTH_EARLY_DROP_START
	std::string_view evictableAgeMs; // 例:GATE_PREAUTH_EVICTABLE_AGE_MS
};
// 覆盖时预算与起点的上界:只为挡住误写(远大于任何一台的连接上限)。
inline constexpr uint32_t kMaxPreAuthBudget = 1'000'000;

// 由三个环境变量的原文与生效上限定出未验签预算。任一变量非法、或起点大于预算时返回 nullopt,error 里是给启动日志用的英文说明。
inline std::optional<PreAuthBudget> ResolvePreAuthBudget(const PreAuthEnvNames &names, const char *rawBudget,
														 const char *rawEarlyDropStart, const char *rawEvictableAgeMs,
														 uint32_t effectiveMaxConnections, std::string &error)
{
	const auto budget = ParseOptionalPositive(rawBudget, kMaxPreAuthBudget);
	if (!budget)
	{
		error = std::string(names.budget) + " must be an integer 1.." + std::to_string(kMaxPreAuthBudget) +
				" (leave it unset for the default derived from the connection cap)";
		return std::nullopt;
	}
	const auto start = ParseOptionalPositive(rawEarlyDropStart, kMaxPreAuthBudget);
	if (!start)
	{
		error = std::string(names.earlyDropStart) + " must be an integer 1.." + std::to_string(kMaxPreAuthBudget) +
				" (leave it unset for half of the budget)";
		return std::nullopt;
	}
	const auto maxAgeMs = static_cast<uint32_t>(client_liveness::kClientLiveness.handshakeTimeout.count() - 1);
	const auto ageMs = ParseOptionalPositive(rawEvictableAgeMs, maxAgeMs);
	if (!ageMs)
	{
		error = std::string(names.evictableAgeMs) + " must be an integer 1.." + std::to_string(maxAgeMs) +
				" milliseconds, shorter than the handshake deadline (leave it unset for " +
				std::to_string(kDefaultMinEvictableAge.count()) + ")";
		return std::nullopt;
	}
	PreAuthBudget result = DefaultPreAuthBudget(effectiveMaxConnections);
	if (*budget != 0)
	{
		result.full = *budget;
		result.earlyDropStart = static_cast<uint32_t>(uint64_t{*budget} * kEarlyDropStartPercent / 100);
	}
	if (*start != 0)
	{
		if (*start > result.full)
		{
			error = std::string(names.earlyDropStart) + "=" + std::to_string(*start) + " must not exceed the budget " +
					std::to_string(result.full) + " (" + std::string(names.budget) + " or its default)";
			return std::nullopt;
		}
		result.earlyDropStart = *start;
	}
	if (*ageMs != 0)
	{
		result.minEvictableAge = std::chrono::milliseconds(*ageMs);
	}
	return result;
}

// ---------------------------------------------------------------------------
// 按来源前缀的新建连接速率闸(gate 准入设计稿 P2-2;gate 大厅连接与 battle 直连同一份规则)
// ---------------------------------------------------------------------------
//
// 前面几道都是全局的:持续超限时,合法新连接与攻击连接抢同一个速率桶,谁来得多谁占得多。这一道按来源前缀
// (IPv4 /24、IPv6 /64)各给一个小令牌桶,让单个来源最多拿到全局速率的一成。照设计稿 §7 的约束:
//   - 限速率不限总量:CGNAT / 网吧 / 校园网 / 基站出口后面可能有几百上千个真玩家,不做每来源并发上限;
//   - 按 /24、/64 聚合:IPv6 按 /128 等于没做(单主机能在自己的 /64 里生成 2^64 个源地址);
//   - 缺省关闭:只有节点看得到玩家真实源地址时才有意义。gate 是 external 入口 + externalTrafficPolicy: Local(G9),
//     battle 是 hostPort / Agones 端口映射(节点上的 DNAT,保源地址,D89);
//   - 只限公网来源(IsPublicSourcePrefix):accept 时看到私网、回环、链路本地或运营商级 NAT 共享段的地址,说明前面是代理、
//     负载均衡或集群内的调用方。小游戏 WebSocket 经边缘 nginx 进来就是边缘 Pod 的地址,accept 这一层还没读到任何字节,
//     认不出后面的真实玩家;按这个地址限速等于把它后面的所有玩家当成一个来源一起限。这类连接的按来源限速归边缘自己做;
//   - 开了也只在承压时生效(「告警后自动开启」,SourceRateUnderPressure):未验签池过了早拒起点,或全局速率闸在最近一个
//     握手期限内拒过连接。平时一条都不拦,不会误伤同一出口后面的大批玩家;
//   - 不封来源、不落持久状态(§7「拒绝 N 次就封 IP」是状态炸弹):每个前缀只是一个会自己回满的桶。
//
// 开启时的推荐参数(DefaultSourceRateLimit;没有压测数据,按推导先定):
//   持续速率 = 全局缺省持续速率的 1/10(上限 2 万时 20 条/秒;全局取下限 100 时为 10):至少要十个前缀一起来才吃得满全局速率闸;
//   桶深 = 持续速率 × 握手期限(200 / 100):单个前缀稳态下最多占住未验签预算的一成。
//   承压期间一个 /24 后面几百个真玩家同时重连,先放行一个桶深,之后按持续速率进,几十秒内接完。这是攻击期间的降级,平时不生效。
// 前缀表有上限(kMaxTrackedSources):满了先清掉桶已回满的前缀(每秒最多清一次),清不出位置时新前缀不登记、直接放行
// (fail-open,节点计 <节点>_source_untracked_total 并采样打日志)。只有极分散的来源(上万个前缀)才撑得满它,那时这一道帮不上忙,
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

// 这个前缀是不是公网来源。不是的(RFC 6890 里不全局可达、却会出现在 TCP 源地址上的几段)都是代理、负载均衡或集群内的地址,
// 不按来源限速,理由见本节开头。IPv4 映射地址已在 SourcePrefixOfIpv6 里折成 IPv4,按 IPv4 的规则判。
constexpr bool IsPublicSourcePrefix(const SourcePrefix &prefix)
{
	if (prefix.ipv6)
	{
		return prefix.bits != 0                 // ::/64:未指定、回环(::1)与 IPv4 兼容地址
			   && (prefix.bits >> 57) != 0x7E   // fc00::/7 唯一本地地址
			   && (prefix.bits >> 54) != 0x3FA; // fe80::/10 链路本地
	}
	const uint64_t firstOctet = prefix.bits >> 16;    // bits 是地址的高 24 位
	return firstOctet != 0 && firstOctet != 10 && firstOctet != 127 // 0/8、10/8、127/8
		   && (prefix.bits >> 14) != 0x191                          // 100.64/10 运营商级 NAT 共享段
		   && (prefix.bits >> 8) != 0xA9FE                          // 169.254/16 链路本地
		   && (prefix.bits >> 12) != 0xAC1                          // 172.16/12
		   && (prefix.bits >> 8) != 0xC0A8;                         // 192.168/16
}

// 对端地址的来源前缀;认不出的地址族返回空。
inline std::optional<SourcePrefix> SourcePrefixOf(const muduo::net::InetAddress &peer)
{
	const struct sockaddr *address = peer.getSockAddr();
	if (peer.family() == AF_INET)
	{
		const auto *v4 = reinterpret_cast<const struct sockaddr_in *>(address);
		return SourcePrefixOfIpv4(muduo::net::sockets::networkToHost32(v4->sin_addr.s_addr));
	}
	if (peer.family() == AF_INET6)
	{
		const auto *v6 = reinterpret_cast<const struct sockaddr_in6 *>(address);
		std::array<uint8_t, 16> bytes{};
		for (size_t i = 0; i < bytes.size(); ++i)
		{
			bytes[i] = static_cast<uint8_t>(v6->sin6_addr.s6_addr[i]);
		}
		return SourcePrefixOfIpv6(bytes);
	}
	return std::nullopt;
}

// accept 处按来源限速用的来源:公网地址返回它的前缀;非公网地址与认不出的地址族返回空,这条连接不按来源限速。
inline std::optional<SourcePrefix> RateLimitedSourceOf(const muduo::net::InetAddress &peer)
{
	const auto prefix = SourcePrefixOf(peer);
	return prefix && IsPublicSourcePrefix(*prefix) ? prefix : std::nullopt;
}

// 按前缀分桶的速率闸:每个前缀一个 AcceptRateGate(GCRA,起步是满桶)。判定阶段只问 WouldAdmit,不改状态;
// 所有闸都放行之后才 Take,被别的闸拒掉的连接不消耗这个前缀的名额。不加锁:只在节点的 accept 线程上用。
class SourceRateGate
{
public:
	explicit SourceRateGate(RateLimit limit, size_t maxTracked = kMaxTrackedSources)
		: limit(limit), maxTracked(maxTracked)
	{
	}

	bool WouldAdmit(const SourcePrefix &prefix, SteadyTime now) const
	{
		const auto it = gates.find(prefix);
		return it == gates.end() || it->second.CanTake(now);
	}

	// 放行之后记一笔。表满时先清掉桶已回满的前缀;仍然满就不登记、返回 false(这个前缀这次没被计入,调用方计一次 untracked)。
	bool Take(const SourcePrefix &prefix, SteadyTime now)
	{
		auto it = gates.find(prefix);
		if (it == gates.end())
		{
			if (gates.size() >= maxTracked)
			{
				PruneIfDue(now);
			}
			if (gates.size() >= maxTracked)
			{
				return false;
			}
			it = gates.emplace(prefix, AcceptRateGate(limit)).first;
		}
		it->second.TryTake(now);
		return true;
	}

	size_t Tracked() const { return gates.size(); }
	const RateLimit &Limit() const { return limit; }

private:
	// 桶已回满的前缀与没登记过等价,可以删。只在表满时清、每秒最多一次:撑满时不至于每条连接都扫一遍表。
	void PruneIfDue(SteadyTime now)
	{
		if (lastPruneAt && now - *lastPruneAt < std::chrono::seconds(1))
		{
			return;
		}
		lastPruneAt = now;
		std::erase_if(gates, [now](const auto &entry) { return entry.second.Full(now); });
	}

	RateLimit limit;
	size_t maxTracked;
	std::unordered_map<SourcePrefix, AcceptRateGate, SourcePrefixHash> gates;
	std::optional<SteadyTime> lastPruneAt;
};

// 承压:未验签池到了早拒起点,或全局速率闸在最近一个握手期限内拒过连接(lastAcceptRateRejectAt 由节点在速率闸拒绝时记)。
// 按来源限速只在承压时生效。
inline bool SourceRateUnderPressure(uint64_t unauthenticated, const PreAuthBudget &budget,
									const std::optional<SteadyTime> &lastAcceptRateRejectAt, SteadyTime now)
{
	if (unauthenticated >= budget.earlyDropStart)
	{
		return true;
	}
	return lastAcceptRateRejectAt && now - *lastAcceptRateRejectAt < client_liveness::kClientLiveness.handshakeTimeout;
}

// 一个客户端面开关按来源限速的两个变量名(都由节点给,只用来拼启动错误说明)。缺省关闭:
//   rate:  设了就开启,单个前缀的持续速率,规范十进制 1..kMaxAcceptRatePerSecond;推荐值见 DefaultSourceRateLimit;
//   burst: 单个前缀的桶深,规范十进制 1..kMaxAcceptBurst;不设时取持续速率 × 握手期限。
// 只设桶深不设速率、或写错都拒绝启动。没有「0 = 关闭」:关闭就是不设。
struct SourceRateEnvNames
{
	std::string_view rate;  // 例:GATE_SOURCE_RATE_PER_SECOND
	std::string_view burst; // 例:GATE_SOURCE_BURST
};

// 解析两个变量。成功返回 true:limit 为空表示关闭,否则是开启后的参数;失败返回 false,error 里是给启动日志用的英文说明。
inline bool ResolveSourceRateLimit(const SourceRateEnvNames &names, const char *rawRate, const char *rawBurst,
								   std::optional<RateLimit> &limit, std::string &error)
{
	limit.reset();
	const auto rate = ParseOptionalPositive(rawRate, kMaxAcceptRatePerSecond);
	if (!rate)
	{
		error = std::string(names.rate) + " must be an integer 1.." + std::to_string(kMaxAcceptRatePerSecond) +
				" (leave it unset to keep per-source limiting off)";
		return false;
	}
	const auto burst = ParseOptionalPositive(rawBurst, kMaxAcceptBurst);
	if (!burst)
	{
		error = std::string(names.burst) + " must be an integer 1.." + std::to_string(kMaxAcceptBurst) +
				" (leave it unset for rate x handshake deadline)";
		return false;
	}
	if (*rate == 0)
	{
		if (*burst != 0)
		{
			error = std::string(names.burst) + " is set but " + std::string(names.rate) +
					" is not; per-source limiting is enabled by setting the rate";
			return false;
		}
		return true;
	}
	const uint64_t deadlineSeconds = static_cast<uint64_t>(
		std::chrono::duration_cast<std::chrono::seconds>(client_liveness::kClientLiveness.handshakeTimeout).count());
	const uint64_t derivedBurst = (std::min)(uint64_t{kMaxAcceptBurst}, uint64_t{*rate} * deadlineSeconds);
	limit = RateLimit{*rate, *burst != 0 ? *burst : static_cast<uint32_t>(derivedBurst)};
	return true;
}

} // namespace accept_admission
