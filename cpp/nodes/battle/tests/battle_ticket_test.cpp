// battle 直连票据安全闸门单测(设计文档 turn-based-battle-server.md §18 D24/D25/D27)。
//
// 与 cpp/nodes/gate/tests/gate_security_test.cpp 同一种组织方式:**不进 battle.vcxproj
// 的 ClCompile 列表**,不会被 MSBuild 编进 battle 可执行文件,也不会被 vcxproj2cmake.py
// 生成进 CMakeLists.txt —— 独立跑。battle_security.h / core/security/token_security.h
// 刻意不依赖 muduo / protobuf / 引擎,就是为了让这份测试能在没有整套 C++ 引擎的机器上
// 直接编出来。
//
// 跑法(仓库根目录,Linux 或带 g++ 的容器;gtest 用仓库里已经 vendored 的那份):
//
//   GT=third_party/grpc/third_party/googletest/googletest
//   g++ -std=c++23 -I cpp/nodes/battle -I cpp/libs/engine/core -I "$GT/include" -I "$GT"
//       cpp/nodes/battle/tests/battle_ticket_test.cpp
//       "$GT/src/gtest-all.cc" "$GT/src/gtest_main.cc"
//       -lssl -lcrypto -lpthread -o /tmp/battle_ticket_test
//   /tmp/battle_ticket_test
//
// (上面四行是同一条命令,换行只为可读;实际执行时接成一行。)

#include <gtest/gtest.h>

#include <cstdlib>
#include <string>

#include "battle_security.h"

namespace
{

using battle_security::ClassifyTicketFields;
using battle_security::ClassifyTokenSecret;
using battle_security::RunMode;
using battle_security::SignTicket;
using battle_security::TicketFields;
using battle_security::TicketRole;
using battle_security::TicketVerdict;
using battle_security::TokenSecretVerdict;
using battle_security::VerifyTicketSignature;

constexpr char kSecret[] = "unit-test-battle-token-secret-0123456789abcdef";
// "序列化后的 BattleTicketPayload":本头是字节级的,任何字节串都行
const std::string kPayload = std::string("\x08\xd2\x09\x10\x01", 5);

TEST(BattleTicketSignature, SignThenVerifyRoundTrips)
{
	const std::string sig = SignTicket(kSecret, kPayload);
	ASSERT_EQ(sig.size(), 64u); // SHA256 = 32 字节 = 64 hex
	EXPECT_TRUE(VerifyTicketSignature(kSecret, kPayload, sig));
}

TEST(BattleTicketSignature, IsDeterministicForSameInputs)
{
	EXPECT_EQ(SignTicket(kSecret, kPayload), SignTicket(kSecret, kPayload));
}

TEST(BattleTicketSignature, RejectsTamperedPayload)
{
	const std::string sig = SignTicket(kSecret, kPayload);
	std::string tampered = kPayload;
	tampered[0] ^= 0x01;
	EXPECT_FALSE(VerifyTicketSignature(kSecret, tampered, sig));
}

TEST(BattleTicketSignature, RejectsWrongSecret)
{
	const std::string sig = SignTicket(kSecret, kPayload);
	EXPECT_FALSE(VerifyTicketSignature("another-secret-that-is-long-enough-000000", kPayload, sig));
}

TEST(BattleTicketSignature, RejectsEmptyAndTruncatedSignature)
{
	const std::string sig = SignTicket(kSecret, kPayload);
	EXPECT_FALSE(VerifyTicketSignature(kSecret, kPayload, ""));
	EXPECT_FALSE(VerifyTicketSignature(kSecret, kPayload, sig.substr(0, 63)));
	EXPECT_FALSE(VerifyTicketSignature(kSecret, kPayload, sig + "0"));
}

TEST(BattleTicketSignature, GateTokenSecretDoesNotVerifyBattleTicket)
{
	// 分域:用 gate 的密钥签出来的东西不能过 battle 的验签(任一泄露不影响另一侧)
	const std::string gateSigned = SignTicket("gate-secret-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx", kPayload);
	EXPECT_FALSE(VerifyTicketSignature(kSecret, kPayload, gateSigned));
}

// ---- 字段判定 ----

TicketFields ValidTicket()
{
	TicketFields t;
	t.battleId = 65047318352658432ull;
	t.playerId = 9001;
	t.nodeId = 7;
	t.instanceId = "battle-uuid-1";
	t.expireAtMs = 1'000'000;
	t.role = static_cast<int>(TicketRole::kParticipant);
	return t;
}

constexpr uint32_t kSelfNode = 7;
constexpr char kSelfInstance[] = "battle-uuid-1";
constexpr uint64_t kNow = 999'999;

TEST(BattleTicketFields, AcceptsWellFormedParticipantAndObserver)
{
	TicketFields t = ValidTicket();
	EXPECT_EQ(ClassifyTicketFields(t, kSelfNode, kSelfInstance, kNow), TicketVerdict::kOk);
	t.role = static_cast<int>(TicketRole::kObserver);
	EXPECT_EQ(ClassifyTicketFields(t, kSelfNode, kSelfInstance, kNow), TicketVerdict::kOk);
}

TEST(BattleTicketFields, RejectsEmptyIdentity)
{
	TicketFields t = ValidTicket();
	t.battleId = 0;
	EXPECT_EQ(ClassifyTicketFields(t, kSelfNode, kSelfInstance, kNow), TicketVerdict::kEmptyIdentity);
	t = ValidTicket();
	t.playerId = 0;
	EXPECT_EQ(ClassifyTicketFields(t, kSelfNode, kSelfInstance, kNow), TicketVerdict::kEmptyIdentity);
}

TEST(BattleTicketFields, RejectsTicketSignedForAnotherNode)
{
	TicketFields t = ValidTicket();
	t.nodeId = 8;
	EXPECT_EQ(ClassifyTicketFields(t, kSelfNode, kSelfInstance, kNow), TicketVerdict::kNodeMismatch);
}

TEST(BattleTicketFields, RejectsSameNodeIdButDifferentInstance)
{
	// 节点重启后 node_id 被复用:旧票的 node_id 对得上,实例 UUID 对不上,必须拒
	TicketFields t = ValidTicket();
	t.instanceId = "battle-uuid-0-restarted";
	EXPECT_EQ(ClassifyTicketFields(t, kSelfNode, kSelfInstance, kNow), TicketVerdict::kInstanceMismatch);
}

TEST(BattleTicketFields, RejectsEmptyInstanceOnEitherSide)
{
	TicketFields t = ValidTicket();
	t.instanceId = "";
	EXPECT_EQ(ClassifyTicketFields(t, kSelfNode, kSelfInstance, kNow), TicketVerdict::kInstanceMismatch);
	t = ValidTicket();
	EXPECT_EQ(ClassifyTicketFields(t, kSelfNode, "", kNow), TicketVerdict::kInstanceMismatch);
}

TEST(BattleTicketFields, ExpiryIsExclusiveAtDeadline)
{
	TicketFields t = ValidTicket();
	EXPECT_EQ(ClassifyTicketFields(t, kSelfNode, kSelfInstance, t.expireAtMs - 1), TicketVerdict::kOk);
	EXPECT_EQ(ClassifyTicketFields(t, kSelfNode, kSelfInstance, t.expireAtMs), TicketVerdict::kExpired);
	EXPECT_EQ(ClassifyTicketFields(t, kSelfNode, kSelfInstance, t.expireAtMs + 1), TicketVerdict::kExpired);
}

TEST(BattleTicketFields, RejectsUnknownRole)
{
	TicketFields t = ValidTicket();
	t.role = 0;
	EXPECT_EQ(ClassifyTicketFields(t, kSelfNode, kSelfInstance, kNow), TicketVerdict::kRoleInvalid);
	t.role = 42;
	EXPECT_EQ(ClassifyTicketFields(t, kSelfNode, kSelfInstance, kNow), TicketVerdict::kRoleInvalid);
}

TEST(BattleTicketFields, CheapestRejectionWinsWhenSeveralFieldsAreBad)
{
	// 判定顺序契约:身份 → 节点 → 实例 → 期限 → 角色
	TicketFields t = ValidTicket();
	t.nodeId = 99;
	t.expireAtMs = 0;
	t.role = 0;
	EXPECT_EQ(ClassifyTicketFields(t, kSelfNode, kSelfInstance, kNow), TicketVerdict::kNodeMismatch);
}

// ---- 空密钥处置(与 gate 同一套判据,只是环境变量不同) ----

TEST(BattleTokenSecretPolicy, EmptySecretIsRefusedInProdAndBypassedInDevTest)
{
	EXPECT_EQ(ClassifyTokenSecret("", RunMode::kProd), TokenSecretVerdict::kRefuse);
	EXPECT_EQ(ClassifyTokenSecret("   ", RunMode::kProd), TokenSecretVerdict::kRefuse);
	EXPECT_EQ(ClassifyTokenSecret("", RunMode::kDev), TokenSecretVerdict::kDevBypass);
	EXPECT_EQ(ClassifyTokenSecret("", RunMode::kTest), TokenSecretVerdict::kDevBypass);
	EXPECT_EQ(ClassifyTokenSecret(kSecret, RunMode::kProd), TokenSecretVerdict::kEnforce);
	EXPECT_EQ(ClassifyTokenSecret(kSecret, RunMode::kDev), TokenSecretVerdict::kEnforce);
}

TEST(BattleTokenSecretPolicy, RunModeEnvIsBattleSpecific)
{
	// gate 与 battle 的运行模式变量必须分开:同机 gate=dev 不能顺带放行 battle
	EXPECT_STREQ(battle_security::kRunModeEnv, "BATTLE_RUN_MODE");
}

// ---- 密钥强度(§18.6:≥32 字节且 ≠ GateTokenSecret) ----

TEST(BattleTokenSecretStrength, RejectsShortSecretsIncludingWhitespacePadding)
{
	using battle_security::ClassifySecretStrength;
	using battle_security::SecretStrengthVerdict;
	EXPECT_EQ(ClassifySecretStrength("x", ""), SecretStrengthVerdict::kTooShort);
	// 31 字节差一个字节也不行
	EXPECT_EQ(ClassifySecretStrength(std::string(31, 'a'), ""), SecretStrengthVerdict::kTooShort);
	// 空白不算长度:凑出来的 32 字节里有 8 个空格 → 实际 24
	EXPECT_EQ(ClassifySecretStrength("    " + std::string(24, 'a') + "    ", ""),
			  SecretStrengthVerdict::kTooShort);
	EXPECT_EQ(ClassifySecretStrength(std::string(32, 'a'), ""), SecretStrengthVerdict::kOk);
	EXPECT_EQ(ClassifySecretStrength(kSecret, ""), SecretStrengthVerdict::kOk);
}

TEST(BattleTokenSecretStrength, RejectsReuseOfGateSecretButAllowsUnsetGate)
{
	using battle_security::ClassifySecretStrength;
	using battle_security::SecretStrengthVerdict;
	const std::string gate = "unit-test-gate-token-secret-0123456789abcdef";
	EXPECT_EQ(ClassifySecretStrength(gate, gate), SecretStrengthVerdict::kSameAsGate);
	// 首尾空白差异不构成"不同密钥"
	EXPECT_EQ(ClassifySecretStrength("  " + gate + "  ", gate), SecretStrengthVerdict::kSameAsGate);
	EXPECT_EQ(ClassifySecretStrength(kSecret, gate), SecretStrengthVerdict::kOk);
	// gate 侧没配密钥(本地 dev)不判相同:这一项只守"分域",不守 gate 自己的强度
	EXPECT_EQ(ClassifySecretStrength(kSecret, ""), SecretStrengthVerdict::kOk);
}

} // namespace
