#pragma once

// GM 面安全基元：GM 客户端消息模式闸 + 节点间 GM 管理 RPC 的 HMAC 鉴权与防重放。
//
// 原来住在 cpp/nodes/gate/gate_security.h，随后 scene 侧（scene_admin_handler /
// player_gm_guard / gm_player_command_security）也需要共用同一套信封格式、
// canonical 串、时间窗与 nonce 去重实现。提到引擎核心层做唯一权威来源，
// 消除跨节点相对路径包含；gate_security.h 保留 using 别名，对外 API 不变。
//
// 约束（与 token_security.h 一致）：
//   1) 纯函数 + 轻量数据结构，不依赖 muduo / protobuf / 引擎其它模块，
//      可脱离整套 C++ 引擎独立编译单测（如 gate_security_test.cpp）；
//   2) 只返回结论不打日志，拒绝后的日志级别与响应口径由调用点决定；
//   3) 仅依赖标准库 + engine/core/security/token_security.h（内部含 OpenSSL）。

#include "security/token_security.h"

#include <cctype>
#include <cerrno>
#include <cstdint>
#include <cstdlib>
#include <ctime>
#include <deque>
#include <string>
#include <string_view>
#include <unordered_set>
#include <utility>

namespace gm_security
{

// ── GM 客户端消息闸（P0-a）────────────────────────────────────────────────────

enum class GmClientMessageVerdict
{
	kAllow,  // 非生产模式：放行（本地联调 / robot 冒烟）
	kRefuse, // 生产模式：拒绝
};

inline const char *GmClientMessageVerdictName(GmClientMessageVerdict verdict)
{
	return verdict == GmClientMessageVerdict::kAllow ? "allow" : "refuse";
}

// 纯函数：只看运行模式给结论，不认识 message_id、不打日志。
inline GmClientMessageVerdict ClassifyGmClientMessage(token_security::RunMode mode)
{
	return token_security::IsNonProdMode(mode) ? GmClientMessageVerdict::kAllow : GmClientMessageVerdict::kRefuse;
}

// ── GM 面鉴权 ───────────────────────────────────────────────────────────────
//
// 信封格式（寄生在 operator / auth 字段）：
//     "<操作人>|<unix 秒>|<nonce>|<hmac-sha256 hex>"
//
// canonical 串（字段间用 '\n' 分隔，不可省略任何一段）：
//     <method> '\n' <目标 node_id> '\n' <操作人> '\n' <unix 秒原文> '\n'
//     <nonce> '\n' <reason>
//
// 约束：
//   * 时间戳用原文参与签名，避免前导零 / 格式差异导致双端失配；
//   * method 与目标 node_id 进 canonical 串，阻断跨方法、跨节点重放；
//   * 密钥未配置时一律拒绝（fail-closed），不设开发模式免签口子；
//   * 先验签名、后登记 nonce，防止未认证流量灌满去重表造成 DoS。

enum class GmAuthResult
{
	kOk,
	kSecretNotConfigured,  // 密钥未配置 -> fail-closed
	kMalformedEnvelope,    // 信封不是四段式，或各段不合法
	kTimestampOutOfWindow, // 时间戳超出允许的时钟偏差窗口
	kSignatureMismatch,    // HMAC 对不上
	kReplayedNonce,        // 窗口内 nonce 重复（或 nonce 表已满）
};

inline const char *GmAuthResultName(GmAuthResult result)
{
	switch (result)
	{
	case GmAuthResult::kOk:
		return "ok";
	case GmAuthResult::kSecretNotConfigured:
		return "secret_not_configured";
	case GmAuthResult::kMalformedEnvelope:
		return "malformed_envelope";
	case GmAuthResult::kTimestampOutOfWindow:
		return "timestamp_out_of_window";
	case GmAuthResult::kSignatureMismatch:
		return "signature_mismatch";
	case GmAuthResult::kReplayedNonce:
	default:
		return "replayed_nonce";
	}
}

inline constexpr char kGmAdminSecretEnv[] = "GATE_GM_ADMIN_SECRET";
inline constexpr char kGmAuthSkewEnv[] = "GATE_GM_AUTH_SKEW_SECONDS";
inline constexpr char kGmEnvelopeSeparator = '|';
inline constexpr size_t kGmSignatureHexLength = 64; // SHA256 = 32 字节 = 64 hex
inline constexpr size_t kGmMaxOperatorLength = 64;
inline constexpr size_t kGmMaxNonceLength = 128;
inline constexpr int64_t kGmDefaultSkewSeconds = 300;
inline constexpr int64_t kGmMinSkewSeconds = 30;
inline constexpr int64_t kGmMaxSkewSeconds = 900;

struct GmEnvelope
{
	std::string operatorName;
	std::string timestampRaw;
	std::string nonce;
	std::string signatureHex;
};

inline bool IsAsciiDigits(std::string_view text)
{
	if (text.empty())
	{
		return false;
	}
	for (const char ch : text)
	{
		if (std::isdigit(static_cast<unsigned char>(ch)) == 0)
		{
			return false;
		}
	}
	return true;
}

inline bool IsLowerHex(std::string_view text)
{
	if (text.empty())
	{
		return false;
	}
	for (const char ch : text)
	{
		const bool isDigit = ch >= '0' && ch <= '9';
		const bool isHexLetter = ch >= 'a' && ch <= 'f';
		if (!isDigit && !isHexLetter)
		{
			return false;
		}
	}
	return true;
}

// 严格四段式。操作人名里不允许出现分隔符 '|'，避免分段歧义绕过校验。
inline bool ParseGmEnvelope(std::string_view field, GmEnvelope &out)
{
	std::string parts[4];
	size_t partIndex = 0;
	size_t cursor = 0;
	while (partIndex < 4)
	{
		const size_t separator = field.find(kGmEnvelopeSeparator, cursor);
		if (partIndex == 3)
		{
			if (separator != std::string_view::npos)
			{
				return false; // 多于四段
			}
			parts[partIndex] = std::string(field.substr(cursor));
			break;
		}
		if (separator == std::string_view::npos)
		{
			return false; // 少于四段
		}
		parts[partIndex] = std::string(field.substr(cursor, separator - cursor));
		cursor = separator + 1;
		++partIndex;
	}

	if (parts[0].empty() || parts[0].size() > kGmMaxOperatorLength)
	{
		return false;
	}
	if (!IsAsciiDigits(parts[1]) || parts[1].size() > 20)
	{
		return false;
	}
	if (parts[2].empty() || parts[2].size() > kGmMaxNonceLength)
	{
		return false;
	}
	if (parts[3].size() != kGmSignatureHexLength || !IsLowerHex(parts[3]))
	{
		return false;
	}

	out.operatorName = std::move(parts[0]);
	out.timestampRaw = std::move(parts[1]);
	out.nonce = std::move(parts[2]);
	out.signatureHex = std::move(parts[3]);
	return true;
}

// targetNodeId 传的是十进制无前导零的节点号文本（验证方固定用 std::to_string(GetNodeId())）。
inline std::string BuildGmCanonicalString(std::string_view method,
										  std::string_view targetNodeId,
										  std::string_view operatorName,
										  std::string_view timestampRaw,
										  std::string_view nonce,
										  std::string_view reason)
{
	std::string canonical;
	canonical.reserve(method.size() + targetNodeId.size() + operatorName.size() +
					  timestampRaw.size() + nonce.size() + reason.size() + 5);
	canonical.append(method).push_back('\n');
	canonical.append(targetNodeId).push_back('\n');
	canonical.append(operatorName).push_back('\n');
	canonical.append(timestampRaw).push_back('\n');
	canonical.append(nonce).push_back('\n');
	canonical.append(reason);
	return canonical;
}

// 窗口内的 nonce 去重表：按过期时间淘汰，表满时直接拒（不提前淘汰未过期项，防止重开重放窗口）。
class GmNonceCache
{
public:
	static constexpr size_t kMaxEntries = 1024;

	// 返回 false 表示该 nonce 在窗口内已经用过（或表已满）。
	bool Register(std::string_view nonce, int64_t nowUnix, int64_t retainSeconds)
	{
		PruneExpired(nowUnix);
		const std::string key(nonce);
		if (seen_.find(key) != seen_.end())
		{
			return false;
		}
		if (entries_.size() >= kMaxEntries)
		{
			return false;
		}
		entries_.emplace_back(nowUnix + retainSeconds, key);
		seen_.insert(key);
		return true;
	}

	size_t size() const { return entries_.size(); }

private:
	void PruneExpired(int64_t nowUnix)
	{
		while (!entries_.empty() && entries_.front().first <= nowUnix)
		{
			seen_.erase(entries_.front().second);
			entries_.pop_front();
		}
	}

	std::deque<std::pair<int64_t, std::string>> entries_;
	std::unordered_set<std::string> seen_;
};

struct GmAuthOutcome
{
	GmAuthResult result = GmAuthResult::kSecretNotConfigured;
	std::string operatorName; // 仅在 kOk 时有意义，供审计日志使用
};

// 先验签名、后登记 nonce：未认证调用方无法往去重表塞数据。
inline GmAuthOutcome VerifyGmRequest(std::string_view method,
									 std::string_view targetNodeId,
									 std::string_view operatorField,
									 std::string_view reason,
									 std::string_view secret,
									 int64_t nowUnix,
									 int64_t skewSeconds,
									 GmNonceCache &nonceCache)
{
	GmAuthOutcome outcome;

	if (token_security::TrimAscii(secret).empty())
	{
		outcome.result = GmAuthResult::kSecretNotConfigured;
		return outcome;
	}

	GmEnvelope envelope;
	if (!ParseGmEnvelope(operatorField, envelope))
	{
		outcome.result = GmAuthResult::kMalformedEnvelope;
		return outcome;
	}

	errno = 0;
	char *parseEnd = nullptr;
	const long long requestTime = std::strtoll(envelope.timestampRaw.c_str(), &parseEnd, 10);
	if (errno != 0 || parseEnd == nullptr || *parseEnd != '\0')
	{
		outcome.result = GmAuthResult::kMalformedEnvelope;
		return outcome;
	}

	const int64_t delta = static_cast<int64_t>(requestTime) - nowUnix;
	if (delta > skewSeconds || delta < -skewSeconds)
	{
		outcome.result = GmAuthResult::kTimestampOutOfWindow;
		return outcome;
	}

	const std::string canonical = BuildGmCanonicalString(
		method, targetNodeId, envelope.operatorName, envelope.timestampRaw, envelope.nonce, reason);
	const std::string expected = token_security::HmacSha256Hex(secret, canonical);
	if (!token_security::ConstantTimeEquals(expected, envelope.signatureHex))
	{
		outcome.result = GmAuthResult::kSignatureMismatch;
		return outcome;
	}

	// 保留时长取整个窗口宽度的两倍，覆盖整段 [now-skew, now+skew]。
	if (!nonceCache.Register(envelope.nonce, nowUnix, skewSeconds * 2))
	{
		outcome.result = GmAuthResult::kReplayedNonce;
		return outcome;
	}

	outcome.result = GmAuthResult::kOk;
	outcome.operatorName = std::move(envelope.operatorName);
	return outcome;
}

// 允许的时钟偏差。默认 300s，可用 GATE_GM_AUTH_SKEW_SECONDS 调整，钳制在 [30, 900]。
inline int64_t ResolveGmSkewSeconds()
{
	const char *raw = std::getenv(kGmAuthSkewEnv);
	if (raw == nullptr || raw[0] == '\0')
	{
		return kGmDefaultSkewSeconds;
	}
	errno = 0;
	char *parseEnd = nullptr;
	const long long parsed = std::strtoll(raw, &parseEnd, 10);
	if (errno != 0 || parseEnd == nullptr || *parseEnd != '\0')
	{
		return kGmDefaultSkewSeconds;
	}
	if (parsed < kGmMinSkewSeconds)
	{
		return kGmMinSkewSeconds;
	}
	if (parsed > kGmMaxSkewSeconds)
	{
		return kGmMaxSkewSeconds;
	}
	return static_cast<int64_t>(parsed);
}

inline GmNonceCache &ProcessGmNonceCache()
{
	// 节点 RPC handler 全部运行在单线程 muduo EventLoop 上，无需额外加锁。
	static GmNonceCache cache;
	return cache;
}

inline GmAuthOutcome VerifyGmRequestFromEnv(std::string_view method,
											std::string_view targetNodeId,
											std::string_view operatorField,
											std::string_view reason)
{
	const char *secret = std::getenv(kGmAdminSecretEnv);
	const auto nowUnix = static_cast<int64_t>(std::time(nullptr));
	return VerifyGmRequest(method,
						   targetNodeId,
						   operatorField,
						   reason,
						   secret != nullptr ? std::string_view(secret) : std::string_view{},
						   nowUnix,
						   ResolveGmSkewSeconds(),
						   ProcessGmNonceCache());
}

} // namespace gm_security
