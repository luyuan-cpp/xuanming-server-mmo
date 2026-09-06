#pragma once

// gate 安全闸门:客户端令牌密钥的运行模式门控 + GM 面调用方鉴权。
//
// 为什么单独一个头,而且**不依赖 muduo / protobuf / 引擎**任何东西:
//   1) 这里全是纯函数(输入 -> 结论),可以在没有整套 C++ 引擎的机器上直接
//      编译单测,见 cpp/nodes/gate/tests/gate_security_test.cpp;
//   2) 同一条结论在不同调用点该打 FATAL(启动拒绝)还是 WARN(降级留痕)
//      由调用点决定,所以本头只**返回结论,不打日志**;
//   3) HMAC 工具原来藏在 client_message_processor.cpp 的匿名 namespace 里,
//      GM 鉴权要复用它 —— 提到这里避免第二份实现。
//
// 依赖:标准库 + OpenSSL(gate 已经链了 ssl/crypto,不引入新依赖)+
//      引擎层 core/security/token_security.h(同样只依赖标准库 + OpenSSL)。

#include <openssl/crypto.h>
#include <openssl/evp.h>
#include <openssl/hmac.h>

#include "security/token_security.h"

#include <cctype>
#include <cerrno>
#include <cstdint>
#include <cstdlib>
#include <ctime>
#include <deque>
#include <iomanip>
#include <sstream>
#include <string>
#include <string_view>
#include <unordered_set>
#include <utility>

namespace gate_security
{

// ── HMAC / 运行模式 / 空密钥处置:唯一实现在引擎层 ─────────────────────────
//
// 原来定义在这里的 HmacSha256Hex / ConstantTimeEquals / RunMode / ParseRunMode /
// TokenSecretVerdict / ClassifyTokenSecret 已提到 core/security/token_security.h
// (battle 节点客户端直连票据要用同一套,见 turn-based-battle-server.md §18)。
// 这里只留 using 别名,gate 全部调用点与 tests/gate_security_test.cpp 的 API 不变。

using token_security::BytesToHex;
using token_security::ClassifyTokenSecret;
using token_security::ConstantTimeEquals;
using token_security::HmacSha256Hex;
using token_security::IsNonProdMode;
using token_security::ParseRunMode;
using token_security::RunMode;
using token_security::RunModeName;
using token_security::RunModeResolution;
using token_security::ToLowerAscii;
using token_security::TokenSecretVerdict;
using token_security::TokenSecretVerdictName;
using token_security::TrimAscii;

inline constexpr char kRunModeEnv[] = "GATE_RUN_MODE";

// 进程内解析一次并缓存:运行模式在进程生命周期内不会变,而且这个值会被
// 每条新连接读到,不能每次都 getenv。
inline const RunModeResolution &ResolveRunModeOnce()
{
	static const RunModeResolution resolution = token_security::ResolveRunModeFromEnv(kRunModeEnv);
	return resolution;
}

inline RunMode CurrentRunMode()
{
	return ResolveRunModeOnce().mode;
}

// ── GM 面鉴权 ───────────────────────────────────────────────────────────────
//
// GmGracefulShutdownRequest 只有 operator / reason 两个字段
// (proto/common/base/gm_admin.proto),而这条 RPC 走的是 muduo GameChannel:
// CallMethod 的 controller 和 done 都恒为 nullptr(game_channel.cpp:451),
// **没有任何 metadata 边信道**可用。本轮不动 proto(改了就得重生成 C++/Go/Java
// 三侧产物,无法在本工作包内验证),因此签名寄生在 operator 字段里:
//
//     operator = "<操作人>|<unix 秒>|<nonce>|<hmac-sha256 hex>"
//
// canonical 串(被签名的内容,字段间用 '\n' 分隔,不可省略任何一段):
//
//     <method> '\n' <目标 node_id> '\n' <操作人> '\n' <unix 秒原文> '\n'
//     <nonce> '\n' <reason>
//
// 几条刻意的约束:
//   * 时间戳用**原文**参与签名,不做"解析后再格式化"—— 否则签名方和验证方
//     对前导零 / 正负号的处理只要有一点分歧,签名就永远对不上;
//   * method 进 canonical 串,防止把某条 GM RPC 的签名搬去调用另一条;
//   * 目标 node_id 进 canonical 串。nonce 去重表是**进程内**的,不跨节点 ——
//     不绑 node_id 的话,抓到一条合法停机请求就能在时间窗内挨个重放给全区
//     每一个 gate,一次抓包换全服掉线。绑上之后一条签名只对一个 gate 有效;
//   * reason 进 canonical 串,防止中间人把 "maintenance" 改写成别的审计理由;
//   * 时间窗 + nonce 去重共同防重放,缺一不可(只有时间窗则窗口内可无限重放,
//     只有 nonce 则 nonce 表要无限大)。
//
// 密钥只从环境变量 GATE_GM_ADMIN_SECRET 读,不从 YAML 读 —— 与 Go 侧
// developmentPasswordProviderFromConfig 同一条纪律:共享密钥不进受管配置文件。
// **未配置密钥时一律拒绝**,不设"开发模式免签"的口子:GM 停机在开发环境用
// Ctrl+C / SIGTERM 就够了,没有任何需要免签的正当场景。

enum class GmAuthResult
{
	kOk,
	kSecretNotConfigured,  // GATE_GM_ADMIN_SECRET 没配 -> fail-closed
	kMalformedEnvelope,    // operator 字段不是四段式,或各段不合法
	kTimestampOutOfWindow, // 时间戳超出允许的时钟偏差窗口
	kSignatureMismatch,    // HMAC 对不上
	kReplayedNonce,        // 窗口内 nonce 重复(或 nonce 表已满)
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

// 严格四段式。操作人名里不允许出现分隔符 '|' —— 允许的话分段就有歧义,
// 攻击者可以把签名段挪进"操作人"里绕过校验。
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

// targetNodeId 传的是**十进制无前导零**的节点号文本(验证方固定用
// std::to_string(gNode->GetNodeId()) 生成),签名方必须用同样的写法。
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

// 窗口内的 nonce 去重表。按过期时间淘汰,并且**有容量上限**:表满时直接拒
// (而不是淘汰最旧的)—— 淘汰会重新打开重放窗口,拒绝只是让 GM 重试。
// 由于签名校验排在登记 nonce 之前,只有已经持有密钥的调用方能往表里塞东西,
// 未认证的攻击者撑不满这张表。
class GmNonceCache
{
public:
	static constexpr size_t kMaxEntries = 1024;

	// 返回 false 表示该 nonce 在窗口内已经用过(或表已满)。
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
	std::string operatorName; // 仅在 kOk 时有意义,供审计日志使用
};

// 校验顺序是有意为之:签名先于 nonce 登记。反过来的话,任何未认证的进程都能
// 用垃圾 nonce 把去重表灌满,把合法 GM 请求挤掉 —— 鉴权本身变成 DoS 面。
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

	if (TrimAscii(secret).empty())
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
	const std::string expected = HmacSha256Hex(secret, canonical);
	if (!ConstantTimeEquals(expected, envelope.signatureHex))
	{
		outcome.result = GmAuthResult::kSignatureMismatch;
		return outcome;
	}

	// 保留时长取整个窗口宽度的两倍:时间戳可以比本地时钟早 skew,也可以晚
	// skew,只有覆盖整段 [now-skew, now+skew] 才能保证同一 nonce 在它还能通过
	// 时间窗校验的整个期间都留在表里。
	if (!nonceCache.Register(envelope.nonce, nowUnix, skewSeconds * 2))
	{
		outcome.result = GmAuthResult::kReplayedNonce;
		return outcome;
	}

	outcome.result = GmAuthResult::kOk;
	outcome.operatorName = std::move(envelope.operatorName);
	return outcome;
}

// 允许的时钟偏差。默认 300s,可用 GATE_GM_AUTH_SKEW_SECONDS 调,但**钳制**在
// [30, 900] —— 配成 0 会让所有请求都因为往返延迟被拒,配成一天等于没有时间窗。
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
	// gate 的 RPC handler 全部跑在 muduo 事件循环线程上(GameChannel 在
	// EventLoop 线程里 CallMethod),所以这里不需要额外加锁。
	static GmNonceCache cache;
	return cache;
}

// 生产调用点用的便利封装:密钥读环境变量,时间读系统时钟,nonce 表用进程单例。
// targetNodeId 由调用点传本节点的 node_id 文本(std::to_string(GetNodeId()))。
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

} // namespace gate_security
