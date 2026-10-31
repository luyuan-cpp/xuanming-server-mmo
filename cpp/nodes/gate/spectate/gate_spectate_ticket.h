#pragma once

// 观战订阅凭证的校验(docs/design/battle-spectate-tiered-fanout.md §15.2 / §15.5,D98):match 签发,gate 本地验签。
//
// 纯函数:不碰会话表、连接和日志,只返回结论,处置(回错误信封、计数、启动拒绝)由调用点决定,
// 所以能直接单测(tests/gate_spectate_ticket_test.cpp)。
//
// 校验顺序:密钥 → 凭证外层 → 签名 → 载荷 → 玩家 → 场次 → 期限。签名在解析载荷之前:没通过验签的字节不值得解析,
// 也不该让它的内容影响后面的分支。凭证只决定「能不能订阅」,订阅建立后的寿命由战斗终局 / 期限 / 会话决定。

#include <cstddef>
#include <cstdint>
#include <string>
#include <string_view>

#include "proto/gate/gate_spectate_ticket.pb.h"

namespace gate_spectate
{
	enum class TicketVerdict : uint8_t
	{
		kOk,
		kSecretNotConfigured, // 本 gate 没配 spectate_token_secret:fail-closed,一律拒绝
		kMalformedTicket,     // SpectateTicket 外层解析失败
		kBadSignature,        // HMAC 对不上(篡改,或签发方用了别的密钥)
		kMalformedPayload,    // 验签通过但载荷解析失败,或缺 battle_id / player_id / battle_instance_id
		kPlayerMismatch,      // 凭证不属于本会话的玩家(凭证不能转让),或会话还没绑定玩家
		kBattleMismatch,      // 凭证不是这一场的
		kExpired,             // 按 gate 墙钟已过 expire_at_ms
	};

	// 低基数,可进日志与计数。
	const char *TicketVerdictName(TicketVerdict verdict);

	struct TicketCheck
	{
		TicketVerdict verdict{TicketVerdict::kSecretNotConfigured};
		SpectateTicketPayload payload; // 只在 kOk 时有效

		bool ok() const { return verdict == TicketVerdict::kOk; }
	};

	// sessionPlayerId:会话当前绑定的玩家;0 = 尚未绑定,一律拒绝(调用方负责把 kInvalidGuid 归一成 0)。
	// expire_at_ms == nowWallMs 视为已过期(与 gate 登录令牌、battle 票据同口径)。
	TicketCheck VerifySpectateTicket(std::string_view secret, const std::string &ticketBytes, uint64_t sessionPlayerId,
									 uint64_t requestBattleId, uint64_t nowWallMs);

	// ── 密钥强度(启动门禁,§15.7,口径同 battle 的 ClassifySecretStrength)────────────────────────

	// HMAC-SHA256 输出 32 字节;密钥短于它时暴力搜索空间塌到密钥长度上,凭证可伪造给任意玩家。
	inline constexpr std::size_t kMinSpectateSecretBytes = 32;

	enum class SecretStrengthVerdict : uint8_t
	{
		kOk,
		kTooShort,     // 去首尾空白后不足 kMinSpectateSecretBytes
		kSameAsGate,   // 与 gate_token_secret 相同:信任域没分开
		kSameAsBattle, // 与 battle_token_secret 相同:信任域没分开
	};

	// 只在密钥非空时才有意义,空密钥的处置由运行模式决定(token_security::ClassifyTokenSecret)。
	// 另外两个密钥为空时不判相同。
	SecretStrengthVerdict ClassifySecretStrength(std::string_view spectateSecret, std::string_view gateSecret,
												 std::string_view battleSecret);
} // namespace gate_spectate
