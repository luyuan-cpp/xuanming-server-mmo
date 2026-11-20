// 客户端面 accept 处准入的共享纯逻辑(cpp/libs/engine/core/network/accept_admission.h):gate 的大厅连接与 battle 的战斗直连
// 共用的新建连接速率闸(GCRA)、缺省速率推导、环境变量解析与握手耗时直方图。
//
// 这些用例 2026-11-19 从 cpp/nodes/gate/tests/connection_admission_test.cpp 挪来(逻辑挪进 core 时一起挪),断言没有改;
// 新增的是 ResolveRateLimit 按调用方给的变量名出错误说明、HandshakeHistogram 的累计两条。
// 各节点自己的部分(拒绝原因、判定顺序、计数与指标)在 gate 的 connection_admission_test.cpp 与
// battle 的 cpp/nodes/battle/tests/battle_connection_admission_test.cpp。
// 时刻全部注入,整数时长,边界值精确。

#include <gtest/gtest.h>

#include <chrono>
#include <string>

#include "network/accept_admission.h"
#include "network/client_liveness.h"

namespace
{

using namespace accept_admission;
using std::chrono::milliseconds;
using std::chrono::seconds;

// 离时钟零点足够远的起点:速率闸把零点当「桶是满的」,用例不该碰巧落在零点上。
const SteadyTime kT0 = SteadyTime{} + std::chrono::hours(1);

int TakeAll(AcceptRateGate &gate, const SteadyTime now, const int cap = 1'000'000)
{
	int taken = 0;
	while (taken < cap && gate.TryTake(now))
	{
		++taken;
	}
	return taken;
}

const RateLimitEnvNames kNames{"X_ACCEPT_RATE", "X_ACCEPT_BURST", "XMaxConnections"};

// 缺省速率:桶深 = 上限(从零接回一整台不被拒),持续速率 = 上限的 1%/秒,下限 100/秒。
TEST(AcceptAdmissionPolicy, DefaultRateIsOnePercentOfCapacityPerSecondWithAFullCapacityBurst)
{
	EXPECT_EQ(DefaultRateLimit(20000), (RateLimit{200, 20000}));
	EXPECT_EQ(DefaultRateLimit(131071), (RateLimit{1310, 131071}));
	// 上限配得很小时速率闸不该成为瓶颈:速率取下限,桶深至少一秒的量。
	EXPECT_EQ(DefaultRateLimit(10000), (RateLimit{100, 10000}));
	EXPECT_EQ(DefaultRateLimit(5), (RateLimit{100, 100}));
	// battle 直连的缺省上限(bin/etc/base_deploy_config.yaml BattleMaxConnections: 4096)与 dev / test 的硬上限 65535。
	EXPECT_EQ(DefaultRateLimit(4096), (RateLimit{100, 4096}));
	EXPECT_EQ(DefaultRateLimit(65535), (RateLimit{655, 65535}));
	// 握手期限 10s 内,一个从不验签的来源稳态下最多占住上限的 10%(设计稿 P1-3 的推导)。
	const RateLimit limit = DefaultRateLimit(20000);
	const auto handshakeSeconds =
		std::chrono::duration_cast<seconds>(client_liveness::kClientLiveness.handshakeTimeout).count();
	EXPECT_EQ(limit.perSecond * handshakeSeconds, 20000 / 10);
}

TEST(AcceptAdmissionPolicy, OptionalPositiveEnvAcceptsOnlyCanonicalDecimals)
{
	EXPECT_EQ(ParseOptionalPositive(nullptr, 1000), 0u) << "未设";
	EXPECT_EQ(ParseOptionalPositive("", 1000), 0u) << "空串按未设";
	EXPECT_EQ(ParseOptionalPositive("1", 1000), 1u);
	EXPECT_EQ(ParseOptionalPositive("1000", 1000), 1000u) << "上界本身合法";
	for (const char *bad : {"0", "01", "-1", "+1", " 1", "1 ", "abc", "1.5", "0x10", "1001", "99999999999"})
	{
		EXPECT_FALSE(ParseOptionalPositive(bad, 1000).has_value()) << "应拒绝: '" << bad << "'";
	}
}

TEST(AcceptAdmissionPolicy, RateLimitResolvesFromEnvOrFallsBackToTheDefault)
{
	std::string error;
	EXPECT_EQ(ResolveRateLimit(kNames, nullptr, nullptr, 20000, error), DefaultRateLimit(20000));
	EXPECT_TRUE(error.empty());

	// 只给速率:桶深仍取 max(上限, 速率)。
	EXPECT_EQ(ResolveRateLimit(kNames, "500", nullptr, 20000, error), (RateLimit{500, 20000}));
	EXPECT_EQ(ResolveRateLimit(kNames, "50000", "", 20000, error), (RateLimit{50000, 50000}));
	// 只给桶深:速率仍按缺省推导。
	EXPECT_EQ(ResolveRateLimit(kNames, nullptr, "300", 20000, error), (RateLimit{200, 300}));
	// 两个都给:原样采用,允许低于缺省下限(运维明确写了就照办)。
	EXPECT_EQ(ResolveRateLimit(kNames, "10", "20", 20000, error), (RateLimit{10, 20}));
	EXPECT_TRUE(error.empty());

	EXPECT_FALSE(ResolveRateLimit(kNames, "0", nullptr, 20000, error).has_value()) << "没有「0 = 关闭」";
	EXPECT_FALSE(ResolveRateLimit(kNames, "200", "many", 20000, error).has_value());
	EXPECT_FALSE(ResolveRateLimit(kNames, "1000001", nullptr, 20000, error).has_value()) << "超过上界";
	EXPECT_FALSE(ResolveRateLimit(kNames, nullptr, "1000001", 20000, error).has_value()) << "桶深超过上界";
}

// 错误说明点名的是调用方给的变量名与上限配置项名:gate 与 battle 各报各的,运维照着说明改对的那个变量。
TEST(AcceptAdmissionPolicy, ResolveErrorNamesTheCallersVariables)
{
	std::string error;
	ASSERT_FALSE(ResolveRateLimit(kNames, "0", nullptr, 20000, error).has_value());
	EXPECT_NE(error.find("X_ACCEPT_RATE must be an integer 1..1000000"), std::string::npos) << error;
	EXPECT_NE(error.find("XMaxConnections"), std::string::npos) << error;
	EXPECT_EQ(error.find("X_ACCEPT_BURST"), std::string::npos) << "只点名写错的那一个: " << error;

	error.clear();
	ASSERT_FALSE(ResolveRateLimit(kNames, nullptr, "many", 20000, error).has_value());
	EXPECT_NE(error.find("X_ACCEPT_BURST must be an integer 1..1000000"), std::string::npos) << error;
	EXPECT_NE(error.find("max(XMaxConnections, rate)"), std::string::npos) << error;
}

TEST(AcceptRateGate, StartsWithAFullBurst)
{
	AcceptRateGate gate(RateLimit{10, 20});
	EXPECT_EQ(TakeAll(gate, kT0), 20);
	EXPECT_FALSE(gate.TryTake(kT0)) << "同一时刻用完之后不能再接";
}

TEST(AcceptRateGate, ReleasesOneConnectionPerIntervalAfterTheBurst)
{
	AcceptRateGate gate(RateLimit{10, 20});
	TakeAll(gate, kT0);
	// 每秒 10 条:每 100ms 放行一条。
	EXPECT_TRUE(gate.TryTake(kT0 + milliseconds(100)));
	EXPECT_FALSE(gate.TryTake(kT0 + milliseconds(100)));
	EXPECT_FALSE(gate.TryTake(kT0 + milliseconds(150))) << "不到一个间隔不放行";
	EXPECT_TRUE(gate.TryTake(kT0 + milliseconds(200)));
}

TEST(AcceptRateGate, NeverAccumulatesBeyondTheBurst)
{
	AcceptRateGate gate(RateLimit{10, 20});
	TakeAll(gate, kT0);
	EXPECT_EQ(TakeAll(gate, kT0 + std::chrono::minutes(10)), 20) << "空闲再久也只攒满一个桶";
}

// 任意时间窗 T 内放行的条数 ≤ 桶深 + 速率 × T:这是速率闸给「不停建连又立刻关」定的上界。
TEST(AcceptRateGate, AdmitsAtMostBurstPlusRateTimesWindow)
{
	AcceptRateGate gate(RateLimit{200, 1000});
	int admitted = 0;
	// 10 秒里每毫秒来 5 条(5000 条/秒,远超速率)。
	for (int ms = 0; ms < 10'000; ++ms)
	{
		for (int i = 0; i < 5; ++i)
		{
			admitted += gate.TryTake(kT0 + milliseconds(ms)) ? 1 : 0;
		}
	}
	EXPECT_LE(admitted, 1000 + 200 * 10);
	EXPECT_GE(admitted, 1000 + 200 * 10 - 1) << "洪水期间也恰好按速率放行,不少放";
}

// 拒绝不改状态:被拒的洪水不会把之后的名额往后推。
TEST(AcceptRateGate, RejectionsDoNotConsumeOrDelayTokens)
{
	AcceptRateGate gate(RateLimit{10, 1});
	EXPECT_TRUE(gate.TryTake(kT0));
	for (int i = 0; i < 10'000; ++i)
	{
		EXPECT_FALSE(gate.TryTake(kT0 + milliseconds(50)));
	}
	EXPECT_TRUE(gate.TryTake(kT0 + milliseconds(100))) << "到点照常放行";
}

TEST(AcceptRateGate, ClockGoingBackwardsOnlyRejects)
{
	AcceptRateGate gate(RateLimit{10, 2});
	EXPECT_EQ(TakeAll(gate, kT0), 2);
	EXPECT_FALSE(gate.TryTake(kT0 - seconds(30))) << "时刻倒退不会多放行";
	EXPECT_TRUE(gate.TryTake(kT0 + milliseconds(100)));
}

TEST(AcceptRateGate, ResetAppliesTheNewLimitAndRefillsTheBucket)
{
	AcceptRateGate gate(RateLimit{10, 2});
	TakeAll(gate, kT0);
	gate.Reset(RateLimit{1000, 5});
	EXPECT_EQ(gate.Limit(), (RateLimit{1000, 5}));
	EXPECT_EQ(TakeAll(gate, kT0), 5);
	EXPECT_TRUE(gate.TryTake(kT0 + milliseconds(1))) << "每秒 1000 条:1ms 一条";
}

// 0 不会除零,也不会变成「永远拒绝」:按 1 处理。
// CanTake 只问不扣,判据与 TryTake 相同;Full 在空闲得够久、攒满一个突发之后为真(按来源分桶时据此清表)。
TEST(AcceptRateGate, CanTakeAsksWithoutSpendingAndFullMeansTheBurstIsBack)
{
	AcceptRateGate gate(RateLimit{10, 2});
	EXPECT_TRUE(gate.Full(kT0));
	EXPECT_TRUE(gate.CanTake(kT0));
	EXPECT_TRUE(gate.CanTake(kT0)) << "问两次也不扣";
	EXPECT_TRUE(gate.TryTake(kT0));
	EXPECT_FALSE(gate.Full(kT0));
	EXPECT_TRUE(gate.TryTake(kT0));
	EXPECT_FALSE(gate.CanTake(kT0));
	EXPECT_FALSE(gate.TryTake(kT0));
	EXPECT_TRUE(gate.CanTake(kT0 + milliseconds(100))) << "每秒 10 条:100ms 回一个";
	EXPECT_FALSE(gate.Full(kT0 + milliseconds(100)));
	EXPECT_TRUE(gate.Full(kT0 + milliseconds(200))) << "两个间隔之后桶回满";
}

// 未验签预算的覆盖变量:报错里点名的是调用方给的变量名;起点按新预算取一半;年龄必须短于握手期限。
TEST(AcceptAdmissionPolicy, PreAuthBudgetResolvesFromTheCallersEnvNames)
{
	const PreAuthEnvNames names{"X_PREAUTH_BUDGET", "X_PREAUTH_START", "X_PREAUTH_AGE_MS"};
	std::string error;
	EXPECT_EQ(ResolvePreAuthBudget(names, nullptr, nullptr, nullptr, 4096, error), DefaultPreAuthBudget(4096));
	EXPECT_EQ(DefaultPreAuthBudget(4096), (PreAuthBudget{500, 1000})) << "上限不到 1 万时跟着速率下限";
	EXPECT_EQ(ResolvePreAuthBudget(names, "600", nullptr, nullptr, 4096, error), (PreAuthBudget{300, 600}));
	EXPECT_TRUE(error.empty()) << error;
	EXPECT_FALSE(ResolvePreAuthBudget(names, "600", "601", nullptr, 4096, error).has_value());
	EXPECT_NE(error.find("X_PREAUTH_START"), std::string::npos) << error;
	error.clear();
	EXPECT_FALSE(ResolvePreAuthBudget(names, nullptr, nullptr, "abc", 4096, error).has_value());
	EXPECT_NE(error.find("X_PREAUTH_AGE_MS"), std::string::npos) << error;
}

TEST(AcceptRateGate, ZeroParametersAreTreatedAsOne)
{
	AcceptRateGate gate(RateLimit{0, 0});
	EXPECT_EQ(gate.Limit(), (RateLimit{1, 1}));
	EXPECT_TRUE(gate.TryTake(kT0));
	EXPECT_FALSE(gate.TryTake(kT0 + milliseconds(999)));
	EXPECT_TRUE(gate.TryTake(kT0 + seconds(1)));
}

// 最大的有限上界就是握手期限,这一条由头文件里的 static_assert 钉住;这里只核分桶。
TEST(AcceptAdmissionPolicy, HandshakeBucketsAreInclusiveUpperBounds)
{
	EXPECT_EQ(HandshakeBucketIndex(0), 0u);
	EXPECT_EQ(HandshakeBucketIndex(10), 0u) << "上界含本身(le)";
	EXPECT_EQ(HandshakeBucketIndex(11), 1u);
	EXPECT_EQ(HandshakeBucketIndex(1000), 6u);
	EXPECT_EQ(HandshakeBucketIndex(10000), kHandshakeUpperBoundsMs.size() - 1);
	EXPECT_EQ(HandshakeBucketIndex(10001), kHandshakeUpperBoundsMs.size()) << "超过最大上界落 +Inf";
}

// 直方图只累计各桶自己的次数(不累计到更高的桶)与耗时之和;Writer 在写出时再求累计值。
TEST(HandshakeHistogram, ObserveCountsOneBucketAndAddsTheElapsedMilliseconds)
{
	HandshakeHistogram histogram;
	histogram.Observe(kT0, kT0 + milliseconds(40));
	histogram.Observe(kT0, kT0 + milliseconds(2500));
	histogram.Observe(kT0, kT0 + seconds(11)); // 超过最大上界:落 +Inf
	EXPECT_EQ(histogram.buckets[HandshakeBucketIndex(40)], 1u);
	EXPECT_EQ(histogram.buckets[HandshakeBucketIndex(2500)], 1u);
	EXPECT_EQ(histogram.buckets.back(), 1u);
	EXPECT_EQ(histogram.sumMs, 40u + 2500u + 11000u);

	uint64_t total = 0;
	for (const uint64_t count : histogram.buckets)
	{
		total += count;
	}
	EXPECT_EQ(total, 3u) << "每次只记一个桶";
}

// 时刻由调用方给;倒退时按 0 记,不让一个负数变成巨大的无符号值。
TEST(HandshakeHistogram, ClockGoingBackwardsCountsAsZero)
{
	HandshakeHistogram histogram;
	histogram.Observe(kT0, kT0 - seconds(5));
	histogram.Observe(kT0, kT0);
	EXPECT_EQ(histogram.buckets[0], 2u);
	EXPECT_EQ(histogram.sumMs, 0u);
}

} // namespace
