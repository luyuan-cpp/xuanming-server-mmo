#pragma once

// battle 节点客户端直连的安全闸门:票据签名 / 校验 + 运行模式门控。
//
// 与 gate 的 gate_security.h 同一套纪律,同一份基元(core/security/token_security.h):
//   1) 纯函数,不依赖 muduo / protobuf / 引擎 —— 票据载荷以"已序列化字节"进出,
//      proto 的解析放在调用点(battle_client_edge.cpp),这样本头能在没有整套引擎
//      的机器上直接编单测(tests/battle_ticket_test.cpp);
//   2) 只返回结论不打日志,FATAL / WARN / 拒连由调用点决定。
//
// 票据契约(设计文档 turn-based-battle-server.md §18 D24/D25):
//   * 签发者 = battle 节点本身(CreateBattle / AddObserver 成功后),密钥
//     BaseDeployConfig.battle_token_secret,全部 battle 实例共享;
//   * 签名 = hex(HMAC-SHA256(secret, BattleTicketPayload 序列化字节)),
//     与 gate 令牌(GateTokenPayload)完全同口径,客户端两条连接可复用同一份握手代码;
//   * 验签之外还要过字段判定(ClassifyTicketFields):签给本节点(node_id + 实例 UUID)、
//     未过期、角色合法;然后由房间管理器核对"房间仍存在且该玩家确在名单上"。
//     票据寿命 = 房间作废期限,可重复用于重连;房间销毁即失效。

#include "security/token_security.h"

#include <cstdint>
#include <string>
#include <string_view>

namespace battle_security
{

using token_security::ClassifyTokenSecret;
using token_security::ConstantTimeEquals;
using token_security::HmacSha256Hex;
using token_security::IsNonProdMode;
using token_security::RunMode;
using token_security::RunModeName;
using token_security::RunModeResolution;
using token_security::TokenSecretVerdict;
using token_security::TokenSecretVerdictName;

// 运行模式环境变量。与 gate 分开命名:同一台开发机上 gate 跑 dev、battle 也跑 dev
// 是常态,但两者的密钥缺失后果不同,不该被一个变量一起放行。
inline constexpr char kRunModeEnv[] = "BATTLE_RUN_MODE";

// 进程内解析一次并缓存(每条新直连都会读它,不能每次 getenv)。
inline const RunModeResolution &ResolveRunModeOnce()
{
	static const RunModeResolution resolution = token_security::ResolveRunModeFromEnv(kRunModeEnv);
	return resolution;
}

inline RunMode CurrentRunMode()
{
	return ResolveRunModeOnce().mode;
}

// ── 密钥强度(启动门禁,§18.6)───────────────────────────────────────────────

// HMAC-SHA256 输出 32 字节;密钥短于它时暴力搜索空间塌到密钥长度上,32 是下限不是建议值
// (与 go/login/internal/config/secrets.go 的 MinSecretLen 同口径)。
inline constexpr size_t kMinTokenSecretBytes = 32;

enum class SecretStrengthVerdict
{
	kOk,
	kTooShort,   // 去首尾空白后不足 kMinTokenSecretBytes
	kSameAsGate, // 与 gate_token_secret 相同:信任域没分开(D24)
};

// 只在密钥非空(ClassifyTokenSecret == kEnforce)时才有意义;空密钥的处置由运行模式决定,
// 不在这里重复判。gateSecret 为空时不判相同(单机 dev 允许两边都没配)。
inline SecretStrengthVerdict ClassifySecretStrength(std::string_view battleSecret,
													std::string_view gateSecret)
{
	const std::string trimmed = token_security::TrimAscii(battleSecret);
	if (trimmed.size() < kMinTokenSecretBytes)
	{
		return SecretStrengthVerdict::kTooShort;
	}
	if (!gateSecret.empty() && trimmed == token_security::TrimAscii(gateSecret))
	{
		return SecretStrengthVerdict::kSameAsGate;
	}
	return SecretStrengthVerdict::kOk;
}

// ── 票据签名 / 验签(字节级) ────────────────────────────────────────────────

// 返回 hex 签名;空串 = OpenSSL 失败(调用点按"签不出票"处理,不下发空签名)。
inline std::string SignTicket(std::string_view secret, std::string_view payloadBytes)
{
	return HmacSha256Hex(secret, payloadBytes);
}

// 常数时间比较;expected 为空(OpenSSL 失败)一律不通过。
inline bool VerifyTicketSignature(std::string_view secret, std::string_view payloadBytes,
								  std::string_view signatureHex)
{
	const std::string expected = HmacSha256Hex(secret, payloadBytes);
	return !expected.empty() && ConstantTimeEquals(expected, signatureHex);
}

// ── 票据字段判定(与 proto 解耦) ───────────────────────────────────────────

// 与 proto 枚举 eBattleTicketRole 数值一致(NONE=0 / PARTICIPANT=1 / OBSERVER=2)。
// 这里不 include proto 头,靠数值对齐;调用点用 static_cast 转换。
enum class TicketRole : int
{
	kNone = 0,
	kParticipant = 1,
	kObserver = 2,
};

struct TicketFields
{
	uint64_t battleId = 0;
	uint64_t playerId = 0;
	uint32_t nodeId = 0;
	std::string_view instanceId;
	uint64_t expireAtMs = 0;
	int role = 0;
};

enum class TicketVerdict
{
	kOk,
	kEmptyIdentity,    // battle_id / player_id 为 0
	kNodeMismatch,     // 不是签给本节点的(node_id 不同)
	kInstanceMismatch, // node_id 相同但实例 UUID 不同(节点重启后 node_id 被复用)
	kExpired,          // 已过房间作废期限
	kRoleInvalid,      // 角色不是参战者 / 观众
};

inline const char *TicketVerdictName(TicketVerdict verdict)
{
	switch (verdict)
	{
	case TicketVerdict::kOk:
		return "ok";
	case TicketVerdict::kEmptyIdentity:
		return "empty_identity";
	case TicketVerdict::kNodeMismatch:
		return "node_mismatch";
	case TicketVerdict::kInstanceMismatch:
		return "instance_mismatch";
	case TicketVerdict::kExpired:
		return "expired";
	case TicketVerdict::kRoleInvalid:
	default:
		return "role_invalid";
	}
}

// 判定顺序即"最便宜的先拒":身份空 → 节点 → 实例 → 期限 → 角色。
// expire_at_ms == now 视为已过期(闭区间右端不放行,与 gate 的 `<= now` 同口径)。
inline TicketVerdict ClassifyTicketFields(const TicketFields &ticket,
										  uint32_t selfNodeId,
										  std::string_view selfInstanceId,
										  uint64_t nowMs)
{
	if (ticket.battleId == 0 || ticket.playerId == 0)
	{
		return TicketVerdict::kEmptyIdentity;
	}
	if (ticket.nodeId != selfNodeId)
	{
		return TicketVerdict::kNodeMismatch;
	}
	// 实例 UUID 两边都非空才有比较意义;签发侧一定填(SelfInstanceId),
	// 空值只可能来自伪造 / 旧版本票据,同样拒绝。
	if (ticket.instanceId.empty() || selfInstanceId.empty() || ticket.instanceId != selfInstanceId)
	{
		return TicketVerdict::kInstanceMismatch;
	}
	if (ticket.expireAtMs <= nowMs)
	{
		return TicketVerdict::kExpired;
	}
	if (ticket.role != static_cast<int>(TicketRole::kParticipant) &&
		ticket.role != static_cast<int>(TicketRole::kObserver))
	{
		return TicketVerdict::kRoleInvalid;
	}
	return TicketVerdict::kOk;
}

} // namespace battle_security
