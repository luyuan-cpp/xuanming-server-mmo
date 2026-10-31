// 观战订阅凭证验签的单测(spectate/gate_spectate_ticket.h,docs/design/battle-spectate-tiered-fanout.md §15.5)。
//
// 不进 gate.vcxproj,登记在 cpp/tests/routing_identity_test/routing_identity_test.vcxproj。
// 签名按 match 的口径独立算一遍:hex(HMAC-SHA256(secret, payload 字节)),小写。

#include <gtest/gtest.h>

#include <cstdint>
#include <string>

#include "proto/gate/gate_spectate_ticket.pb.h"
#include "security/token_security.h"
#include "spectate/gate_spectate_ticket.h"

namespace
{
	using gate_spectate::SecretStrengthVerdict;
	using gate_spectate::TicketVerdict;
	using gate_spectate::VerifySpectateTicket;

	const std::string kSecret = "unit-spectate-token-secret-0123456789abcdef";
	constexpr uint64_t kBattleId = 7001;
	constexpr uint64_t kPlayerId = 42;
	constexpr uint64_t kDeadlineMs = 9'000'000;
	constexpr uint64_t kNowMs = 1'000'000;
	constexpr uint64_t kExpireMs = kNowMs + 60'000;
	const std::string kInstance = "battle-instance-a";

	SpectateTicketPayload MakePayload()
	{
		SpectateTicketPayload payload;
		payload.set_battle_id(kBattleId);
		payload.set_player_id(kPlayerId);
		payload.set_battle_instance_id(kInstance);
		payload.set_deadline_ms(kDeadlineMs);
		payload.set_expire_at_ms(kExpireMs);
		return payload;
	}

	std::string Sign(const std::string &secret, const std::string &payloadBytes)
	{
		SpectateTicket ticket;
		ticket.set_payload(payloadBytes);
		ticket.set_signature(token_security::HmacSha256Hex(secret, payloadBytes));
		return ticket.SerializeAsString();
	}

	std::string SignedTicket(const SpectateTicketPayload &payload = MakePayload())
	{
		return Sign(kSecret, payload.SerializeAsString());
	}

	TicketVerdict Verify(const std::string &ticketBytes)
	{
		return VerifySpectateTicket(kSecret, ticketBytes, kPlayerId, kBattleId, kNowMs).verdict;
	}
} // namespace

TEST(SpectateTicket, ValidTicketReturnsItsPayload)
{
	const auto check = VerifySpectateTicket(kSecret, SignedTicket(), kPlayerId, kBattleId, kNowMs);
	ASSERT_TRUE(check.ok()) << gate_spectate::TicketVerdictName(check.verdict);
	EXPECT_EQ(check.payload.battle_id(), kBattleId);
	EXPECT_EQ(check.payload.player_id(), kPlayerId);
	EXPECT_EQ(check.payload.battle_instance_id(), kInstance);
	EXPECT_EQ(check.payload.deadline_ms(), kDeadlineMs);
	EXPECT_EQ(check.payload.expire_at_ms(), kExpireMs);
}

// 跨语言固定向量:payload 字节按 protobuf 线上格式手写(不经任何语言的序列化器),期望 hex 由 Python hmac 独立算出。
// Go(go/match/internal/logic/spectate_ticket 测试)与 Java(SpectateTicketsTest)用同一组常量:match 签、gate 验,
// 字节差一位观战就全部被拒,而且只有整栈跑起来才看得出。
TEST(SpectateTicket, CrossLanguageVectorVerifies)
{
	const std::string secret = "spectate-cross-language-vector-secret-0123456789";
	const std::string payloadBytes("i (", 11);
	const std::string expectedSignature = "81e49d087e684b9df52c0c7bde47b88afce761af48f37b8dda6bdc3f01c2b0d8";
	EXPECT_EQ(token_security::HmacSha256Hex(secret, payloadBytes), expectedSignature);

	SpectateTicket ticket;
	ticket.set_payload(payloadBytes);
	ticket.set_signature(expectedSignature);
	const auto check = VerifySpectateTicket(secret, ticket.SerializeAsString(), /*sessionPlayerId=*/2, /*battleId=*/1, /*nowMs=*/3);
	EXPECT_EQ(check.verdict, TicketVerdict::kOk);
	EXPECT_EQ(check.payload.battle_instance_id(), "i");
	EXPECT_EQ(check.payload.deadline_ms(), 3u);
	EXPECT_EQ(check.payload.expire_at_ms(), 4u);
}

TEST(SpectateTicket, TamperedPayloadIsRejected)
{
	SpectateTicket ticket;
	ASSERT_TRUE(ticket.ParseFromString(SignedTicket()));
	SpectateTicketPayload otherPlayer = MakePayload();
	otherPlayer.set_player_id(kPlayerId + 1);
	ticket.set_payload(otherPlayer.SerializeAsString()); // 换了载荷,签名还是原来的
	EXPECT_EQ(Verify(ticket.SerializeAsString()), TicketVerdict::kBadSignature);
}

TEST(SpectateTicket, TamperedSignatureIsRejected)
{
	SpectateTicket ticket;
	ASSERT_TRUE(ticket.ParseFromString(SignedTicket()));
	std::string signature = ticket.signature();
	signature[0] = signature[0] == 'a' ? 'b' : 'a';
	ticket.set_signature(signature);
	EXPECT_EQ(Verify(ticket.SerializeAsString()), TicketVerdict::kBadSignature);
}

TEST(SpectateTicket, TicketSignedWithAnotherSecretIsRejected)
{
	const std::string otherSecret = "another-spectate-token-secret-0123456789abcdef";
	EXPECT_EQ(Verify(Sign(otherSecret, MakePayload().SerializeAsString())), TicketVerdict::kBadSignature);
}

TEST(SpectateTicket, EmptyOrGarbageTicketIsRejected)
{
	EXPECT_FALSE(VerifySpectateTicket(kSecret, std::string{}, kPlayerId, kBattleId, kNowMs).ok());
	EXPECT_EQ(Verify(std::string("\xff\xff\xff", 3)), TicketVerdict::kMalformedTicket);
}

TEST(SpectateTicket, ExpiryIsCheckedAgainstTheGateWallClock)
{
	const auto at = [](uint64_t nowMs)
	{ return VerifySpectateTicket(kSecret, SignedTicket(), kPlayerId, kBattleId, nowMs).verdict; };
	EXPECT_EQ(at(kExpireMs - 1), TicketVerdict::kOk);
	EXPECT_EQ(at(kExpireMs), TicketVerdict::kExpired); // 边界不放行,与登录令牌、battle 票据同口径
	EXPECT_EQ(at(kExpireMs + 1), TicketVerdict::kExpired);
}

TEST(SpectateTicket, TicketCannotBeTransferredToAnotherPlayer)
{
	EXPECT_EQ(VerifySpectateTicket(kSecret, SignedTicket(), kPlayerId + 1, kBattleId, kNowMs).verdict,
			  TicketVerdict::kPlayerMismatch);
}

TEST(SpectateTicket, SessionWithoutBoundPlayerIsRejected)
{
	EXPECT_EQ(VerifySpectateTicket(kSecret, SignedTicket(), 0, kBattleId, kNowMs).verdict,
			  TicketVerdict::kPlayerMismatch);
}

TEST(SpectateTicket, TicketIsBoundToItsBattle)
{
	EXPECT_EQ(VerifySpectateTicket(kSecret, SignedTicket(), kPlayerId, kBattleId + 1, kNowMs).verdict,
			  TicketVerdict::kBattleMismatch);
}

TEST(SpectateTicket, EmptySecretRejectsEverythingEvenAMatchingEmptyKeySignature)
{
	// 用空密钥签出来的凭证 HMAC 上是对得上的:空密钥绝不能当成「不校验」或「校验通过」。
	const std::string signedWithEmptyKey = Sign(std::string{}, MakePayload().SerializeAsString());
	EXPECT_EQ(VerifySpectateTicket(std::string{}, signedWithEmptyKey, kPlayerId, kBattleId, kNowMs).verdict,
			  TicketVerdict::kSecretNotConfigured);
	EXPECT_EQ(VerifySpectateTicket("   ", SignedTicket(), kPlayerId, kBattleId, kNowMs).verdict,
			  TicketVerdict::kSecretNotConfigured);
}

TEST(SpectateTicket, SignedPayloadMissingIdentityFieldsIsRejected)
{
	SpectateTicketPayload noInstance = MakePayload();
	noInstance.clear_battle_instance_id(); // 没有实例就没法做防僵尸过滤
	EXPECT_EQ(Verify(SignedTicket(noInstance)), TicketVerdict::kMalformedPayload);

	SpectateTicketPayload noPlayer = MakePayload();
	noPlayer.clear_player_id();
	EXPECT_EQ(Verify(SignedTicket(noPlayer)), TicketVerdict::kMalformedPayload);

	SpectateTicketPayload noBattle = MakePayload();
	noBattle.clear_battle_id();
	EXPECT_EQ(Verify(SignedTicket(noBattle)), TicketVerdict::kMalformedPayload);

	// 签名是对的,但载荷根本解析不了。
	EXPECT_EQ(Verify(Sign(kSecret, std::string("\xff\xff\xff", 3))), TicketVerdict::kMalformedPayload);
}

TEST(SpectateTicketSecret, StrengthFollowsTheBattleTokenRules)
{
	const std::string strong(gate_spectate::kMinSpectateSecretBytes, 's');
	const std::string gateSecret(gate_spectate::kMinSpectateSecretBytes, 'g');
	const std::string battleSecret(gate_spectate::kMinSpectateSecretBytes, 'b');

	EXPECT_EQ(gate_spectate::ClassifySecretStrength(strong, gateSecret, battleSecret), SecretStrengthVerdict::kOk);
	EXPECT_EQ(gate_spectate::ClassifySecretStrength(strong.substr(1), gateSecret, battleSecret),
			  SecretStrengthVerdict::kTooShort);
	EXPECT_EQ(gate_spectate::ClassifySecretStrength(gateSecret, gateSecret, battleSecret),
			  SecretStrengthVerdict::kSameAsGate);
	EXPECT_EQ(gate_spectate::ClassifySecretStrength(battleSecret, gateSecret, battleSecret),
			  SecretStrengthVerdict::kSameAsBattle);
	// 只差首尾空白也算相同:YAML 里多写一个空格不该骗过分域检查。
	EXPECT_EQ(gate_spectate::ClassifySecretStrength(" " + gateSecret + " ", gateSecret, battleSecret),
			  SecretStrengthVerdict::kSameAsGate);
	// 另外两个密钥没配时不判相同(单机 dev)。
	EXPECT_EQ(gate_spectate::ClassifySecretStrength(strong, std::string{}, std::string{}), SecretStrengthVerdict::kOk);
}
