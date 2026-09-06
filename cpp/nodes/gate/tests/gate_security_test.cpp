// gate 安全闸门单测(工作包 a/b 的回归测试)。
//
// 这个测试**不进 gate.vcxproj 的 ClCompile 列表**,因此既不会被 MSBuild 编进
// gate 可执行文件,也不会被 vcxproj2cmake.py 生成进 CMakeLists.txt 的
// SOURCE_FILES —— 它是独立跑的。gate_security.h / gate_version.h 刻意不依赖
// muduo / protobuf / 引擎,就是为了让这份测试能在没有整套 C++ 引擎的机器上
// 直接编出来。
//
// 跑法(仓库根目录,Linux 或带 g++ 的容器;gtest 用仓库里已经 vendored 的那份):
//
//   GT=third_party/grpc/third_party/googletest/googletest
//   g++ -std=c++23 -I cpp/nodes/gate -I cpp/libs/engine/core -I "$GT/include" -I "$GT"
//       cpp/nodes/gate/tests/gate_security_test.cpp
//       "$GT/src/gtest-all.cc" "$GT/src/gtest_main.cc"
//       -lssl -lcrypto -lpthread -o /tmp/gate_security_test
//   /tmp/gate_security_test
//
// (上面四行是同一条命令,换行只为可读;实际执行时接成一行。)

#include <gtest/gtest.h>

#include <cstdlib>
#include <string>

#include "gate_router_mode.h"
#include "gate_security.h"
#include "gate_version.h"

namespace
{

// 跨平台设/删环境变量。Windows 上 _putenv_s(name, "") 等价于删除(getenv 得 nullptr)。
void SetEnvForTest(const char *name, const char *value)
{
#ifdef _WIN32
	_putenv_s(name, value);
#else
	if (value[0] == '\0')
	{
		unsetenv(name);
	}
	else
	{
		setenv(name, value, 1);
	}
#endif
}

// 测试里统一用这把密钥,和签名辅助函数保持一致。
constexpr char kSecret[] = "unit-test-gm-secret";
constexpr char kMethod[] = "Gate.GmGracefulShutdown";
constexpr char kNodeId[] = "17";

// 按 canonical 规则造一个合法的 operator 信封。
std::string MakeEnvelope(const std::string &operatorName,
						 const std::string &timestampRaw,
						 const std::string &nonce,
						 const std::string &reason,
						 const std::string &secret = kSecret,
						 const std::string &method = kMethod,
						 const std::string &nodeId = kNodeId)
{
	const std::string canonical = gate_security::BuildGmCanonicalString(
		method, nodeId, operatorName, timestampRaw, nonce, reason);
	return operatorName + "|" + timestampRaw + "|" + nonce + "|" +
		   gate_security::HmacSha256Hex(secret, canonical);
}

} // namespace

// ── 运行模式:默认必须落在安全的那一侧 ─────────────────────────────────────

TEST(GateRunMode, UnsetDefaultsToProduction)
{
	bool recognized = false;
	EXPECT_EQ(gate_security::RunMode::kProd, gate_security::ParseRunMode("", &recognized));
	EXPECT_TRUE(recognized);
}

TEST(GateRunMode, UnknownValueDefaultsToProductionAndIsFlagged)
{
	bool recognized = true;
	// 把 GATE_RUN_MODE 拼错("develop" 不是我们认识的值)绝不能悄悄变成开发模式。
	EXPECT_EQ(gate_security::RunMode::kProd, gate_security::ParseRunMode("develop", &recognized));
	EXPECT_FALSE(recognized);

	recognized = true;
	EXPECT_EQ(gate_security::RunMode::kProd, gate_security::ParseRunMode("yes", &recognized));
	EXPECT_FALSE(recognized);
}

TEST(GateRunMode, DevAndTestAreRecognizedCaseInsensitivelyAndTrimmed)
{
	bool recognized = false;
	EXPECT_EQ(gate_security::RunMode::kDev, gate_security::ParseRunMode("  Dev \t", &recognized));
	EXPECT_TRUE(recognized);
	EXPECT_EQ(gate_security::RunMode::kDev, gate_security::ParseRunMode("DEVELOPMENT", nullptr));
	EXPECT_EQ(gate_security::RunMode::kTest, gate_security::ParseRunMode("Test", nullptr));
	EXPECT_EQ(gate_security::RunMode::kProd, gate_security::ParseRunMode("PRODUCTION", nullptr));
}

// ── 路由模式开关(GATE_CLIENT_RPC_ROUTER):默认必须落在旧模式 ─────────────
//
// client-rpc-router.md D34:未设 / 非 1|true|on 时 gate 行为与改前完全一致。
// 这是灰度开关,拼错宁可留在直连,绝不能悄悄切到路由服。

TEST(GateRouterMode, ExplicitOnValuesEnable)
{
	EXPECT_TRUE(gate_router_mode::ParseRouterModeFlag("1"));
	EXPECT_TRUE(gate_router_mode::ParseRouterModeFlag("true"));
	EXPECT_TRUE(gate_router_mode::ParseRouterModeFlag("on"));
}

TEST(GateRouterMode, OnValuesAreCaseInsensitiveAndTrimmed)
{
	EXPECT_TRUE(gate_router_mode::ParseRouterModeFlag("TRUE"));
	EXPECT_TRUE(gate_router_mode::ParseRouterModeFlag("True"));
	EXPECT_TRUE(gate_router_mode::ParseRouterModeFlag("ON"));
	EXPECT_TRUE(gate_router_mode::ParseRouterModeFlag("On"));
	EXPECT_TRUE(gate_router_mode::ParseRouterModeFlag("  1  "));
	EXPECT_TRUE(gate_router_mode::ParseRouterModeFlag("\ttrue\n"));
	EXPECT_TRUE(gate_router_mode::ParseRouterModeFlag(" on\r\n"));
}

TEST(GateRouterMode, EmptyAndUnsetDefaultToDirectMode)
{
	EXPECT_FALSE(gate_router_mode::ParseRouterModeFlag(""));
	EXPECT_FALSE(gate_router_mode::ParseRouterModeFlag("   \t "));
	// 变量名为空指针 / 变量未设置:都视为关。
	EXPECT_FALSE(gate_router_mode::ResolveRouterModeFromEnv(nullptr));
	SetEnvForTest("GATE_ROUTER_MODE_UNIT_TEST_UNSET", "");
	EXPECT_FALSE(gate_router_mode::ResolveRouterModeFromEnv("GATE_ROUTER_MODE_UNIT_TEST_UNSET"));
}

TEST(GateRouterMode, ExplicitOffAndUnknownValuesStayDirect)
{
	EXPECT_FALSE(gate_router_mode::ParseRouterModeFlag("0"));
	EXPECT_FALSE(gate_router_mode::ParseRouterModeFlag("false"));
	EXPECT_FALSE(gate_router_mode::ParseRouterModeFlag("off"));
	EXPECT_FALSE(gate_router_mode::ParseRouterModeFlag("FALSE"));
	// 没进白名单的肯定词也是关:开关只认三个显式值,不猜意图。
	EXPECT_FALSE(gate_router_mode::ParseRouterModeFlag("yes"));
	EXPECT_FALSE(gate_router_mode::ParseRouterModeFlag("enable"));
	EXPECT_FALSE(gate_router_mode::ParseRouterModeFlag("enabled"));
	EXPECT_FALSE(gate_router_mode::ParseRouterModeFlag("router"));
	// 形似但不等于:不能靠前缀匹配。
	EXPECT_FALSE(gate_router_mode::ParseRouterModeFlag("2"));
	EXPECT_FALSE(gate_router_mode::ParseRouterModeFlag("10"));
	EXPECT_FALSE(gate_router_mode::ParseRouterModeFlag("true1"));
	EXPECT_FALSE(gate_router_mode::ParseRouterModeFlag("on-"));
	EXPECT_FALSE(gate_router_mode::ParseRouterModeFlag("1 1"));
}

TEST(GateRouterMode, EnvValueIsReadAndParsed)
{
	// 用测试专属变量名,不与 IsRouterModeEnabled() 的进程级缓存耦合。
	constexpr char kEnv[] = "GATE_ROUTER_MODE_UNIT_TEST";
	SetEnvForTest(kEnv, "1");
	EXPECT_TRUE(gate_router_mode::ResolveRouterModeFromEnv(kEnv));
	SetEnvForTest(kEnv, " On ");
	EXPECT_TRUE(gate_router_mode::ResolveRouterModeFromEnv(kEnv));
	SetEnvForTest(kEnv, "off");
	EXPECT_FALSE(gate_router_mode::ResolveRouterModeFromEnv(kEnv));
	SetEnvForTest(kEnv, "");
	EXPECT_FALSE(gate_router_mode::ResolveRouterModeFromEnv(kEnv));
}

TEST(GateRouterMode, ModeNameIsStableForLogs)
{
	// 启动日志与运维手册都按这两个词查:不能改。
	EXPECT_STREQ("router", gate_router_mode::RouterModeName(true));
	EXPECT_STREQ("direct", gate_router_mode::RouterModeName(false));
}

// ── 空 gate_token_secret:这是工作包 a 的核心回归 ───────────────────────────

TEST(GateTokenSecret, EmptySecretInProductionIsRefused)
{
	// 旧行为:密钥为空 -> session.verified = true,放行所有连接。
	// 新行为:生产模式下空密钥 fail-closed。
	EXPECT_EQ(gate_security::TokenSecretVerdict::kRefuse,
			  gate_security::ClassifyTokenSecret("", gate_security::RunMode::kProd));
}

TEST(GateTokenSecret, WhitespaceOnlySecretCountsAsEmpty)
{
	// YAML 里写成 `GateTokenSecret: "   "` 不该被当成配好了。
	EXPECT_EQ(gate_security::TokenSecretVerdict::kRefuse,
			  gate_security::ClassifyTokenSecret("   \t ", gate_security::RunMode::kProd));
	EXPECT_EQ(gate_security::TokenSecretVerdict::kDevBypass,
			  gate_security::ClassifyTokenSecret("   ", gate_security::RunMode::kDev));
}

TEST(GateTokenSecret, EmptySecretIsBypassedOnlyInNonProdModes)
{
	EXPECT_EQ(gate_security::TokenSecretVerdict::kDevBypass,
			  gate_security::ClassifyTokenSecret("", gate_security::RunMode::kDev));
	EXPECT_EQ(gate_security::TokenSecretVerdict::kDevBypass,
			  gate_security::ClassifyTokenSecret("", gate_security::RunMode::kTest));
}

TEST(GateTokenSecret, ConfiguredSecretAlwaysEnforces)
{
	EXPECT_EQ(gate_security::TokenSecretVerdict::kEnforce,
			  gate_security::ClassifyTokenSecret("s3cret", gate_security::RunMode::kProd));
	// 即使在 dev 模式,配了密钥就必须真校验,不能因为"是开发环境"就跳过。
	EXPECT_EQ(gate_security::TokenSecretVerdict::kEnforce,
			  gate_security::ClassifyTokenSecret("s3cret", gate_security::RunMode::kDev));
}

// ── GM 面鉴权:工作包 b 的核心回归 ─────────────────────────────────────────

TEST(GateGmAuth, ValidSignaturePasses)
{
	gate_security::GmNonceCache cache;
	const std::string envelope = MakeEnvelope("gm_alice", "1000000", "nonce-1", "maintenance");

	const auto outcome = gate_security::VerifyGmRequest(
		kMethod, kNodeId, envelope, "maintenance", kSecret, 1000000, 300, cache);

	EXPECT_EQ(gate_security::GmAuthResult::kOk, outcome.result);
	EXPECT_EQ("gm_alice", outcome.operatorName);
}

TEST(GateGmAuth, PlainOperatorNameIsRejected)
{
	// 这是"修复前"的调用形态:请求体里只有一个自报的明文 operator。
	// 任何能连到端口的进程发这一条就能停服 —— 现在必须被拒。
	gate_security::GmNonceCache cache;
	const auto outcome = gate_security::VerifyGmRequest(
		kMethod, kNodeId, "gm_alice", "maintenance", kSecret, 1000000, 300, cache);
	EXPECT_EQ(gate_security::GmAuthResult::kMalformedEnvelope, outcome.result);
}

TEST(GateGmAuth, MissingSecretIsFailClosedEvenWithValidLookingEnvelope)
{
	gate_security::GmNonceCache cache;
	const std::string envelope = MakeEnvelope("gm_alice", "1000000", "nonce-1", "maintenance");
	const auto outcome = gate_security::VerifyGmRequest(
		kMethod, kNodeId, envelope, "maintenance", "", 1000000, 300, cache);
	EXPECT_EQ(gate_security::GmAuthResult::kSecretNotConfigured, outcome.result);
}

TEST(GateGmAuth, TamperedReasonBreaksSignature)
{
	// reason 进 canonical 串:中间人改写审计理由会导致验签失败。
	gate_security::GmNonceCache cache;
	const std::string envelope = MakeEnvelope("gm_alice", "1000000", "nonce-1", "maintenance");
	const auto outcome = gate_security::VerifyGmRequest(
		kMethod, kNodeId, envelope, "hotfix", kSecret, 1000000, 300, cache);
	EXPECT_EQ(gate_security::GmAuthResult::kSignatureMismatch, outcome.result);
}

TEST(GateGmAuth, SignatureFromAnotherMethodIsRejected)
{
	// method 进 canonical 串:拿 Scene 那条 GM RPC 的签名来调 Gate 这条,必须失败。
	gate_security::GmNonceCache cache;
	const std::string envelope =
		MakeEnvelope("gm_alice", "1000000", "nonce-1", "maintenance", kSecret, "Scene.GmGracefulShutdown");
	const auto outcome = gate_security::VerifyGmRequest(
		kMethod, kNodeId, envelope, "maintenance", kSecret, 1000000, 300, cache);
	EXPECT_EQ(gate_security::GmAuthResult::kSignatureMismatch, outcome.result);
}

TEST(GateGmAuth, SignatureForAnotherGateNodeIsRejected)
{
	// node_id 进 canonical 串:nonce 去重表是进程内的,不绑 node_id 就意味着
	// 抓到一条给 gate-17 的合法停机请求,可以在时间窗内挨个重放给 gate-18、
	// gate-19……一次抓包换全服掉线。
	gate_security::GmNonceCache cache;
	const std::string envelope =
		MakeEnvelope("gm_alice", "1000000", "nonce-1", "maintenance", kSecret, kMethod, "18");
	const auto outcome = gate_security::VerifyGmRequest(
		kMethod, kNodeId, envelope, "maintenance", kSecret, 1000000, 300, cache);
	EXPECT_EQ(gate_security::GmAuthResult::kSignatureMismatch, outcome.result);
}

TEST(GateGmAuth, WrongSecretIsRejected)
{
	gate_security::GmNonceCache cache;
	const std::string envelope =
		MakeEnvelope("gm_alice", "1000000", "nonce-1", "maintenance", "attacker-secret");
	const auto outcome = gate_security::VerifyGmRequest(
		kMethod, kNodeId, envelope, "maintenance", kSecret, 1000000, 300, cache);
	EXPECT_EQ(gate_security::GmAuthResult::kSignatureMismatch, outcome.result);
}

TEST(GateGmAuth, ExpiredAndFutureTimestampsAreRejected)
{
	gate_security::GmNonceCache cache;
	const std::string oldEnvelope = MakeEnvelope("gm_alice", "1000000", "nonce-old", "maintenance");
	EXPECT_EQ(gate_security::GmAuthResult::kTimestampOutOfWindow,
			  gate_security::VerifyGmRequest(kMethod, kNodeId, oldEnvelope, "maintenance", kSecret,
											 1000000 + 301, 300, cache)
				  .result);

	const std::string futureEnvelope = MakeEnvelope("gm_alice", "1000000", "nonce-future", "maintenance");
	EXPECT_EQ(gate_security::GmAuthResult::kTimestampOutOfWindow,
			  gate_security::VerifyGmRequest(kMethod, kNodeId, futureEnvelope, "maintenance", kSecret,
											 1000000 - 301, 300, cache)
				  .result);
}

TEST(GateGmAuth, ReplayOfTheSameEnvelopeIsRejected)
{
	gate_security::GmNonceCache cache;
	const std::string envelope = MakeEnvelope("gm_alice", "1000000", "nonce-replay", "maintenance");

	EXPECT_EQ(gate_security::GmAuthResult::kOk,
			  gate_security::VerifyGmRequest(kMethod, kNodeId, envelope, "maintenance", kSecret,
											 1000000, 300, cache)
				  .result);
	// 一字不改地重放同一条(签名当然还是对的)——必须被 nonce 表挡住。
	EXPECT_EQ(gate_security::GmAuthResult::kReplayedNonce,
			  gate_security::VerifyGmRequest(kMethod, kNodeId, envelope, "maintenance", kSecret,
											 1000000 + 10, 300, cache)
				  .result);
}

TEST(GateGmAuth, BadSignatureDoesNotConsumeNonceSlot)
{
	// 校验顺序回归:签名不对时不能登记 nonce,否则未认证的攻击者可以先用某个
	// nonce 发一条坏签名,把这个 nonce "烧掉",让合法 GM 请求随后被判重放。
	gate_security::GmNonceCache cache;
	const std::string forged =
		MakeEnvelope("gm_alice", "1000000", "nonce-shared", "maintenance", "attacker-secret");
	EXPECT_EQ(gate_security::GmAuthResult::kSignatureMismatch,
			  gate_security::VerifyGmRequest(kMethod, kNodeId, forged, "maintenance", kSecret,
											 1000000, 300, cache)
				  .result);
	EXPECT_EQ(0u, cache.size());

	const std::string legit = MakeEnvelope("gm_alice", "1000000", "nonce-shared", "maintenance");
	EXPECT_EQ(gate_security::GmAuthResult::kOk,
			  gate_security::VerifyGmRequest(kMethod, kNodeId, legit, "maintenance", kSecret,
											 1000000, 300, cache)
				  .result);
}

TEST(GateGmAuth, NonceExpiresOutsideTheRetentionWindow)
{
	gate_security::GmNonceCache cache;
	EXPECT_TRUE(cache.Register("n1", 1000, 600));
	EXPECT_FALSE(cache.Register("n1", 1100, 600));
	// 过期之后同名 nonce 可以再用 —— 但那时它的时间戳早已过不了时间窗校验。
	EXPECT_TRUE(cache.Register("n1", 1601, 600));
}

TEST(GateGmAuth, MalformedEnvelopesAreRejected)
{
	gate_security::GmNonceCache cache;
	const auto reject = [&cache](const std::string &operatorField)
	{
		return gate_security::VerifyGmRequest(kMethod, kNodeId, operatorField, "maintenance", kSecret,
											  1000000, 300, cache)
			.result;
	};

	EXPECT_EQ(gate_security::GmAuthResult::kMalformedEnvelope, reject(""));
	EXPECT_EQ(gate_security::GmAuthResult::kMalformedEnvelope, reject("a|1000000|n"));      // 少一段
	EXPECT_EQ(gate_security::GmAuthResult::kMalformedEnvelope, reject("a|1000000|n|x|y"));  // 多一段
	EXPECT_EQ(gate_security::GmAuthResult::kMalformedEnvelope, reject("|1000000|n|" + std::string(64, 'a')));
	EXPECT_EQ(gate_security::GmAuthResult::kMalformedEnvelope, reject("a|notanumber|n|" + std::string(64, 'a')));
	EXPECT_EQ(gate_security::GmAuthResult::kMalformedEnvelope, reject("a|1000000||" + std::string(64, 'a')));
	EXPECT_EQ(gate_security::GmAuthResult::kMalformedEnvelope, reject("a|1000000|n|tooshort"));
	EXPECT_EQ(gate_security::GmAuthResult::kMalformedEnvelope, reject("a|1000000|n|" + std::string(64, 'Z')));
}

TEST(GateGmAuth, SkewIsClampedToASaneRange)
{
	// 配 0 会让所有请求因为往返延迟被拒;配一天等于没有时间窗。两头都钳住。
#ifdef _WIN32
	_putenv_s(gate_security::kGmAuthSkewEnv, "0");
#else
	setenv(gate_security::kGmAuthSkewEnv, "0", 1);
#endif
	EXPECT_EQ(gate_security::kGmMinSkewSeconds, gate_security::ResolveGmSkewSeconds());

#ifdef _WIN32
	_putenv_s(gate_security::kGmAuthSkewEnv, "86400");
#else
	setenv(gate_security::kGmAuthSkewEnv, "86400", 1);
#endif
	EXPECT_EQ(gate_security::kGmMaxSkewSeconds, gate_security::ResolveGmSkewSeconds());

#ifdef _WIN32
	_putenv_s(gate_security::kGmAuthSkewEnv, "");
#else
	unsetenv(gate_security::kGmAuthSkewEnv);
#endif
	EXPECT_EQ(gate_security::kGmDefaultSkewSeconds, gate_security::ResolveGmSkewSeconds());
}

// ── 启动版本行 ─────────────────────────────────────────────────────────────

TEST(GateVersion, StartupLineCarriesAllSixFields)
{
	const std::string line = gate_version::StartupLine("GATE", "12", "3");
	EXPECT_NE(std::string::npos, line.find("[gate_version]"));
	EXPECT_NE(std::string::npos, line.find("version="));
	EXPECT_NE(std::string::npos, line.find("commit="));
	EXPECT_NE(std::string::npos, line.find("build_time="));
	EXPECT_NE(std::string::npos, line.find("node_type=GATE"));
	EXPECT_NE(std::string::npos, line.find("node_id=12"));
	EXPECT_NE(std::string::npos, line.find("zone_id=3"));
}

TEST(GateVersion, UnallocatedIdentityPrintsPending)
{
	const std::string line = gate_version::StartupLine("GATE", nullptr, nullptr);
	EXPECT_NE(std::string::npos, line.find("node_id=pending"));
	EXPECT_NE(std::string::npos, line.find("zone_id=pending"));
}
