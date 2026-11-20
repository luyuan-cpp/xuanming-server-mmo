#pragma once

// 客户端面在 accept 处准入的共享纯逻辑。gate 的大厅连接与 battle 的战斗直连共用这一份
// (docs/design/gate-connection-admission-control.md P1-1 ~ P1-4;battle 见 turn-based-battle-server.md D27)。
//
// 只放两个客户端面规则相同、要一起改的部分:新建连接速率闸(GCRA)、缺省速率的推导、覆盖它的环境变量解析、
// 握手耗时直方图。各节点自己的部分留在各节点:拒绝原因(指标 <节点>_rejected_total{reason} 的标签集合,两边不同)、
// 判定函数(gate 与 battle 都按「关源 → 总量 → 新建速率」,gate 另有自己的判据)、会话表、指标名与日志。
// 装配:gate 在 cpp/nodes/gate/handler/rpc/connection_admission.{h,cpp},battle 在
// cpp/nodes/battle/client/battle_connection_admission.{h,cpp}。
//
// 只依赖 STL 与 client_liveness.h,不碰 muduo,不读时钟(时刻由调用方给)。单测在
// cpp/tests/rpc_controller_test/accept_admission_test.cpp。不加锁:两个客户端面的 accept 都只在一个 EventLoop 线程上。
//
// 本文件里的 std::max 一律写成 (std::max):gate.vcxproj 没定义 NOMINMAX,muduo 的 Windows 适配头会带进 <Windows.h>
// 的 min / max 宏(同 gate handler/event/scene_route_helper.h 的说明),加括号之后与包含顺序无关。

#include <algorithm>
#include <array>
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <optional>
#include <string>
#include <string_view>

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
		limit_ = RateLimit{(std::max<uint32_t>)(limit.perSecond, 1), (std::max<uint32_t>)(limit.burst, 1)};
		const SteadyTime::duration oneSecond = std::chrono::duration_cast<SteadyTime::duration>(std::chrono::seconds(1));
		const SteadyTime::duration perConnection = oneSecond / static_cast<SteadyTime::duration::rep>(limit_.perSecond);
		interval_ = (std::max)(perConnection, SteadyTime::duration(1));
		tolerance_ = interval_ * static_cast<SteadyTime::duration::rep>(limit_.burst - 1);
		tat_ = SteadyTime{};
	}

	// 取一个名额。取不到返回 false:这条连接不接。
	bool TryTake(SteadyTime now)
	{
		const SteadyTime tat = (std::max)(tat_, now);
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

} // namespace accept_admission
