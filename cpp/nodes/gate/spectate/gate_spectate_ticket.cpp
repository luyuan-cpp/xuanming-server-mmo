#include "gate_spectate_ticket.h"

#include "security/token_security.h"

namespace gate_spectate
{
	const char *TicketVerdictName(TicketVerdict verdict)
	{
		switch (verdict)
		{
		case TicketVerdict::kOk:
			return "ok";
		case TicketVerdict::kSecretNotConfigured:
			return "secret_not_configured";
		case TicketVerdict::kMalformedTicket:
			return "malformed_ticket";
		case TicketVerdict::kBadSignature:
			return "bad_signature";
		case TicketVerdict::kMalformedPayload:
			return "malformed_payload";
		case TicketVerdict::kPlayerMismatch:
			return "player_mismatch";
		case TicketVerdict::kBattleMismatch:
			return "battle_mismatch";
		case TicketVerdict::kExpired:
			return "expired";
		}
		return "unknown";
	}

	TicketCheck VerifySpectateTicket(std::string_view secret, const std::string &ticketBytes, uint64_t sessionPlayerId,
									 uint64_t requestBattleId, uint64_t nowWallMs)
	{
		TicketCheck check;
		const auto reject = [&check](TicketVerdict verdict)
		{
			check.verdict = verdict;
			check.payload.Clear();
			return check;
		};

		// 纯空白等同没配,与 ClassifyTokenSecret 同口径。dev / test 放行空密钥也只是让进程能起来,订阅仍然全拒。
		if (token_security::TrimAscii(secret).empty())
		{
			return reject(TicketVerdict::kSecretNotConfigured);
		}

		SpectateTicket ticket;
		if (!ticket.ParseFromString(ticketBytes))
		{
			return reject(TicketVerdict::kMalformedTicket);
		}

		// HmacSha256Hex 失败返回空串,ConstantTimeEquals 对空串一律不等,所以 OpenSSL 失败同样落到拒绝。
		const std::string expectedSignature = token_security::HmacSha256Hex(secret, ticket.payload());
		if (!token_security::ConstantTimeEquals(expectedSignature, ticket.signature()))
		{
			return reject(TicketVerdict::kBadSignature);
		}

		if (!check.payload.ParseFromString(ticket.payload()))
		{
			return reject(TicketVerdict::kMalformedPayload);
		}
		// 实例为空时 gate 没法做防僵尸过滤(§15.3),玩家或场次为 0 则不可能属于任何会话。签发方不会发出这种凭证,这里兜 fail-closed。
		if (check.payload.battle_id() == 0 || check.payload.player_id() == 0 ||
			check.payload.battle_instance_id().empty())
		{
			return reject(TicketVerdict::kMalformedPayload);
		}
		if (sessionPlayerId == 0 || check.payload.player_id() != sessionPlayerId)
		{
			return reject(TicketVerdict::kPlayerMismatch);
		}
		if (check.payload.battle_id() != requestBattleId)
		{
			return reject(TicketVerdict::kBattleMismatch);
		}
		if (check.payload.expire_at_ms() <= nowWallMs)
		{
			return reject(TicketVerdict::kExpired);
		}

		check.verdict = TicketVerdict::kOk;
		return check;
	}

	SecretStrengthVerdict ClassifySecretStrength(std::string_view spectateSecret, std::string_view gateSecret,
												 std::string_view battleSecret)
	{
		const std::string trimmed = token_security::TrimAscii(spectateSecret);
		if (trimmed.size() < kMinSpectateSecretBytes)
		{
			return SecretStrengthVerdict::kTooShort;
		}
		if (!gateSecret.empty() && trimmed == token_security::TrimAscii(gateSecret))
		{
			return SecretStrengthVerdict::kSameAsGate;
		}
		if (!battleSecret.empty() && trimmed == token_security::TrimAscii(battleSecret))
		{
			return SecretStrengthVerdict::kSameAsBattle;
		}
		return SecretStrengthVerdict::kOk;
	}
} // namespace gate_spectate
