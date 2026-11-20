// 客户端面 accept 处准入的共享纯逻辑(cpp/libs/engine/core/network/accept_admission.h):gate 的大厅连接与 battle 的战斗直连
// 共用的新建连接速率闸(GCRA)、缺省速率推导、环境变量解析、握手耗时直方图、未验签预算与按来源前缀限速。
//
// 速率闸与直方图的用例 2026-11-19 从 cpp/nodes/gate/tests/connection_admission_test.cpp 挪来,按来源前缀的三条 2026-11-20 挪来
// (逻辑挪进 core 时一起挪),断言没有改;新增的是按调用方变量名出错误说明、直方图累计、只限公网来源与承压判定。
// 各节点自己的部分(拒绝原因、判定顺序、计数与指标)在 gate 的 connection_admission_test.cpp 与
// battle 的 cpp/nodes/battle/tests/battle_connection_admission_test.cpp。
// 时刻全部注入,整数时长,边界值精确。

#include <gtest/gtest.h>

#include <array>
#include <chrono>
#include <cstdint>
#include <optional>
#include <string>

#include "muduo/net/InetAddress.h"
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

// ---------------------------------------------------------------------------
// 按来源前缀限速
// ---------------------------------------------------------------------------

// 来源前缀:IPv4 按 /24、IPv6 按 /64 聚合;IPv4 映射地址按 IPv4 算;IPv6 前缀与数值相同的 IPv4 前缀不是同一个来源。
TEST(AcceptAdmissionPolicy, SourcePrefixesAggregateIpv4Slash24AndIpv6Slash64)
{
	EXPECT_EQ(SourcePrefixOfIpv4(0xC0A80101), SourcePrefixOfIpv4(0xC0A801FE)) << "192.168.1.1 与 192.168.1.254 同一个 /24";
	EXPECT_NE(SourcePrefixOfIpv4(0xC0A80101), SourcePrefixOfIpv4(0xC0A80201));

	const std::array<uint8_t, 16> host{0x20, 0x01, 0x0d, 0xb8, 0x00, 0x00, 0x00, 0x01, 1, 2, 3, 4, 5, 6, 7, 8};
	std::array<uint8_t, 16> sameSlash64 = host;
	sameSlash64[8] = 0xFF;
	sameSlash64[15] = 0x09;
	std::array<uint8_t, 16> otherSlash64 = host;
	otherSlash64[7] = 0x02;
	EXPECT_EQ(SourcePrefixOfIpv6(host), SourcePrefixOfIpv6(sameSlash64)) << "接口标识不同、/64 相同";
	EXPECT_NE(SourcePrefixOfIpv6(host), SourcePrefixOfIpv6(otherSlash64));

	const std::array<uint8_t, 16> mapped{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xFF, 0xFF, 192, 168, 1, 7};
	EXPECT_EQ(SourcePrefixOfIpv6(mapped), SourcePrefixOfIpv4(0xC0A80101));
	EXPECT_NE((SourcePrefix{true, 0xC0A801}), (SourcePrefix{false, 0xC0A801}));
	EXPECT_NE(SourcePrefixHash{}(SourcePrefix{true, 0xC0A801}), SourcePrefixHash{}(SourcePrefix{false, 0xC0A801}));

	// 从 muduo 的地址取前缀:与按字节算的同一个。
	EXPECT_EQ(SourcePrefixOf(muduo::net::InetAddress("192.168.1.9", 40000)), SourcePrefixOfIpv4(0xC0A80109));
	EXPECT_EQ(SourcePrefixOf(muduo::net::InetAddress("2001:db8:0:1::5", 40000, true)), SourcePrefixOfIpv6(host));
}

// 只有公网来源按来源限速:私网、回环、链路本地、运营商级 NAT 共享段、0/8,以及 IPv6 的 ::/64、唯一本地、链路本地都不限;
// 边界两侧各取一个地址。
TEST(AcceptAdmissionPolicy, OnlyPublicSourcesAreRateLimitedPerSource)
{
	const auto v4 = [](const uint8_t a, const uint8_t b, const uint8_t c, const uint8_t d)
	{ return SourcePrefixOfIpv4((uint32_t{a} << 24) | (uint32_t{b} << 16) | (uint32_t{c} << 8) | uint32_t{d}); };
	for (const SourcePrefix nonPublic : {v4(10, 0, 0, 1), v4(10, 255, 255, 1), v4(172, 16, 0, 1), v4(172, 31, 255, 1),
										 v4(192, 168, 1, 1), v4(100, 64, 0, 1), v4(100, 127, 255, 1), v4(127, 0, 0, 1),
										 v4(169, 254, 1, 1), v4(0, 1, 2, 3)})
	{
		EXPECT_FALSE(IsPublicSourcePrefix(nonPublic)) << "bits=" << nonPublic.bits;
	}
	for (const SourcePrefix isPublic : {v4(8, 8, 8, 8), v4(9, 255, 255, 1), v4(11, 0, 0, 1), v4(172, 15, 255, 1),
										v4(172, 32, 0, 1), v4(192, 167, 1, 1), v4(192, 169, 1, 1), v4(100, 63, 255, 1),
										v4(100, 128, 0, 1), v4(169, 253, 1, 1), v4(203, 0, 113, 7)})
	{
		EXPECT_TRUE(IsPublicSourcePrefix(isPublic)) << "bits=" << isPublic.bits;
	}

	const auto v6 = [](const uint16_t first, const uint16_t second)
	{
		std::array<uint8_t, 16> bytes{};
		bytes[0] = static_cast<uint8_t>(first >> 8);
		bytes[1] = static_cast<uint8_t>(first & 0xFF);
		bytes[2] = static_cast<uint8_t>(second >> 8);
		bytes[3] = static_cast<uint8_t>(second & 0xFF);
		bytes[15] = 1;
		return SourcePrefixOfIpv6(bytes);
	};
	EXPECT_FALSE(IsPublicSourcePrefix(v6(0x0000, 0x0000))) << "::1";
	EXPECT_FALSE(IsPublicSourcePrefix(v6(0xFC00, 0x0000))) << "fc00::/7";
	EXPECT_FALSE(IsPublicSourcePrefix(v6(0xFDFF, 0x1234))) << "fc00::/7 的另一半";
	EXPECT_FALSE(IsPublicSourcePrefix(v6(0xFE80, 0x0000))) << "fe80::/10";
	EXPECT_FALSE(IsPublicSourcePrefix(v6(0xFEBF, 0x0000))) << "fe80::/10 的末端";
	EXPECT_TRUE(IsPublicSourcePrefix(v6(0x2001, 0x0db8)));
	EXPECT_TRUE(IsPublicSourcePrefix(v6(0xFBFF, 0x0000))) << "fc00::/7 之前";
	EXPECT_TRUE(IsPublicSourcePrefix(v6(0xFEC0, 0x0000))) << "fe80::/10 之后";

	// 从 muduo 的地址取:公网给前缀,私网与 IPv4 映射的私网给空。
	EXPECT_EQ(RateLimitedSourceOf(muduo::net::InetAddress("203.0.113.9", 40000)), SourcePrefixOfIpv4(0xCB007109));
	EXPECT_FALSE(RateLimitedSourceOf(muduo::net::InetAddress("10.244.1.17", 40000)).has_value()) << "边缘 nginx 的 Pod 地址";
	EXPECT_FALSE(RateLimitedSourceOf(muduo::net::InetAddress("::ffff:10.0.0.1", 40000, true)).has_value());
	EXPECT_FALSE(RateLimitedSourceOf(muduo::net::InetAddress("fd00::5", 40000, true)).has_value());
}

// 每个前缀一个桶:只问不扣;一个前缀用完不影响别的前缀;按持续速率回满。
TEST(SourceRateGate, EachPrefixHasItsOwnBucketAndAskingDoesNotSpend)
{
	SourceRateGate gate(RateLimit{1, 2});
	const SourcePrefix a = SourcePrefixOfIpv4(0x0A000001);
	const SourcePrefix b = SourcePrefixOfIpv4(0x0A000101);
	EXPECT_TRUE(gate.WouldAdmit(a, kT0));
	EXPECT_TRUE(gate.WouldAdmit(a, kT0)) << "只问不扣";
	EXPECT_EQ(gate.Tracked(), 0u);
	EXPECT_TRUE(gate.Take(a, kT0));
	EXPECT_TRUE(gate.Take(a, kT0));
	EXPECT_FALSE(gate.WouldAdmit(a, kT0)) << "桶深 2 用完";
	EXPECT_TRUE(gate.WouldAdmit(b, kT0)) << "别的前缀不受影响";
	EXPECT_FALSE(gate.WouldAdmit(a, kT0 + milliseconds(999)));
	EXPECT_TRUE(gate.WouldAdmit(a, kT0 + seconds(1))) << "每秒回一个";
	EXPECT_EQ(gate.Tracked(), 1u);
}

// 前缀表满:新前缀照样放行但不登记(fail-open);表满时先清掉桶已回满的前缀,每秒最多清一次。
TEST(SourceRateGate, AFullTableAdmitsNewPrefixesUntrackedAndPrunesRefilledOnes)
{
	SourceRateGate gate(RateLimit{1, 1}, 2);
	const auto prefix = [](const uint32_t index) { return SourcePrefixOfIpv4(index << 8); };
	EXPECT_TRUE(gate.Take(prefix(1), kT0));
	EXPECT_TRUE(gate.Take(prefix(2), kT0));
	EXPECT_TRUE(gate.WouldAdmit(prefix(3), kT0)) << "没登记过的前缀放行";
	EXPECT_FALSE(gate.Take(prefix(3), kT0)) << "表满、两个桶都还没回满:不登记";
	EXPECT_EQ(gate.Tracked(), 2u);
	// 1 秒后两个桶都回满了:清掉它们,给新前缀腾位置。
	EXPECT_TRUE(gate.Take(prefix(3), kT0 + seconds(1)));
	EXPECT_EQ(gate.Tracked(), 1u);
}

// 承压:池到了早拒起点,或全局速率闸在最近一个握手期限内拒过连接;两样都没有就不承压。
TEST(AcceptAdmissionPolicy, PerSourceLimitingEngagesOnlyUnderPressure)
{
	const PreAuthBudget budget{500, 1000};
	EXPECT_FALSE(SourceRateUnderPressure(499, budget, std::nullopt, kT0));
	EXPECT_TRUE(SourceRateUnderPressure(500, budget, std::nullopt, kT0)) << "池到早拒起点";
	EXPECT_TRUE(SourceRateUnderPressure(0, budget, kT0, kT0 + seconds(9))) << "握手期限内拒过";
	EXPECT_FALSE(SourceRateUnderPressure(0, budget, kT0, kT0 + seconds(10))) << "满一个握手期限之后不再算";
}

// 两个开关变量:不设 = 关闭;设了速率才开,桶深缺省是速率 × 握手期限;只设桶深、写 0、越界都拒;错误说明点名调用方的变量名。
TEST(AcceptAdmissionPolicy, SourceRateResolvesFromTheCallersEnvNames)
{
	const SourceRateEnvNames names{"X_SOURCE_RATE", "X_SOURCE_BURST"};
	std::optional<RateLimit> limit = RateLimit{1, 1};
	std::string error;
	EXPECT_TRUE(ResolveSourceRateLimit(names, nullptr, nullptr, limit, error));
	EXPECT_FALSE(limit.has_value()) << "不设 = 关闭";
	EXPECT_TRUE(ResolveSourceRateLimit(names, "20", nullptr, limit, error));
	EXPECT_EQ(limit, (RateLimit{20, 200}));
	EXPECT_TRUE(ResolveSourceRateLimit(names, "20", "50", limit, error));
	EXPECT_EQ(limit, (RateLimit{20, 50}));
	EXPECT_TRUE(error.empty()) << error;

	EXPECT_FALSE(ResolveSourceRateLimit(names, nullptr, "50", limit, error)) << "只设桶深不设速率";
	EXPECT_NE(error.find("X_SOURCE_BURST is set but X_SOURCE_RATE is not"), std::string::npos) << error;
	error.clear();
	EXPECT_FALSE(ResolveSourceRateLimit(names, "0", nullptr, limit, error)) << "没有「0 = 关闭」,关闭就是不设";
	EXPECT_NE(error.find("X_SOURCE_RATE must be an integer"), std::string::npos) << error;
	error.clear();
	EXPECT_FALSE(ResolveSourceRateLimit(names, "20", "1000001", limit, error)) << "桶深超过上界";
	EXPECT_NE(error.find("X_SOURCE_BURST must be an integer"), std::string::npos) << error;

	EXPECT_EQ(DefaultSourceRateLimit(20000), (RateLimit{20, 200}));
	EXPECT_EQ(DefaultSourceRateLimit(50), (RateLimit{10, 100})) << "全局取下限 100 时";
}

} // namespace
