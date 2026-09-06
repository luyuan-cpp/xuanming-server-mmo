#pragma once

// 令牌安全基元:HMAC-SHA256 签名 / 常数时间比较 / 运行模式门控 / 空密钥处置。
//
// 这些原来全部住在 cpp/nodes/gate/gate_security.h 里,是 gate 客户端令牌校验的
// 私有工具。battle 节点做客户端直连(设计文档 turn-based-battle-server.md §18)
// 需要一模一样的一套:同样的 HMAC 口径、同样的"空密钥必须由显式运行模式授权"
// 纪律。两处各抄一份迟早漂移(AGENTS.md §11.2 DRY:会共同变化的重复才消除),
// 所以提到引擎层做唯一权威来源;gate_security.h 只保留 using 别名与 gate 专属
// 的 GM 鉴权部分,对外 API 不变。
//
// 与 gate_security.h 同样的约束:
//   1) 纯函数,不依赖 muduo / protobuf / 引擎,能在没有整套 C++ 引擎的机器上
//      直接编单测(gate/tests/gate_security_test.cpp、battle/tests/battle_ticket_test.cpp);
//   2) 只返回结论不打日志,FATAL / WARN / 拒连由调用点决定;
//   3) 依赖:标准库 + OpenSSL(gate / battle 都已链 ssl/crypto)。

#include <openssl/crypto.h>
#include <openssl/evp.h>
#include <openssl/hmac.h>

#include <cctype>
#include <cstdlib>
#include <iomanip>
#include <sstream>
#include <string>
#include <string_view>

namespace token_security
{

// ── HMAC / 定长比较 ─────────────────────────────────────────────────────────

inline std::string BytesToHex(const unsigned char *data, unsigned int size)
{
	std::ostringstream stream;
	stream << std::hex << std::setfill('0');
	for (unsigned int index = 0; index < size; ++index)
	{
		stream << std::setw(2) << static_cast<unsigned int>(data[index]);
	}
	return stream.str();
}

// hex(HMAC-SHA256(secret, payload))。返回空串 = OpenSSL 调用失败(调用点按拒绝处理)。
inline std::string HmacSha256Hex(std::string_view secret, std::string_view payload)
{
	unsigned char digest[EVP_MAX_MD_SIZE];
	unsigned int digestLength = 0;
	const auto *result = HMAC(EVP_sha256(),
							  secret.data(),
							  static_cast<int>(secret.size()),
							  reinterpret_cast<const unsigned char *>(payload.data()),
							  payload.size(),
							  digest,
							  &digestLength);
	if (result == nullptr)
	{
		return {};
	}
	return BytesToHex(digest, digestLength);
}

// 常数时间比较。std::string 的 == 在首个不匹配字节处提前返回,攻击者可用
// 计时差逐字节猜签名。长度不等可以直接拒(长度不是秘密);两边都空也拒
// (空签名对空签名"相等"不能算通过)。
inline bool ConstantTimeEquals(std::string_view lhs, std::string_view rhs)
{
	if (lhs.size() != rhs.size() || lhs.empty())
	{
		return false;
	}
	return CRYPTO_memcmp(lhs.data(), rhs.data(), lhs.size()) == 0;
}

// ── 运行模式 ────────────────────────────────────────────────────────────────
//
// 唯一的显式判据是每个节点自己的环境变量(gate: GATE_RUN_MODE,battle:
// BATTLE_RUN_MODE)。默认值必须是安全的那一侧:未设置 / 设了不认识的值,一律
// 按 prod 处理。

enum class RunMode
{
	kProd,
	kDev,
	kTest,
};

inline const char *RunModeName(RunMode mode)
{
	switch (mode)
	{
	case RunMode::kDev:
		return "dev";
	case RunMode::kTest:
		return "test";
	case RunMode::kProd:
	default:
		return "prod";
	}
}

// 非生产 = 允许出现降级路径(空密钥放行)的环境。
inline bool IsNonProdMode(RunMode mode)
{
	return mode == RunMode::kDev || mode == RunMode::kTest;
}

inline std::string ToLowerAscii(std::string_view text)
{
	std::string lowered;
	lowered.reserve(text.size());
	for (const char ch : text)
	{
		lowered.push_back(static_cast<char>(std::tolower(static_cast<unsigned char>(ch))));
	}
	return lowered;
}

inline std::string TrimAscii(std::string_view text)
{
	size_t begin = 0;
	size_t end = text.size();
	while (begin < end && std::isspace(static_cast<unsigned char>(text[begin])) != 0)
	{
		++begin;
	}
	while (end > begin && std::isspace(static_cast<unsigned char>(text[end - 1])) != 0)
	{
		--end;
	}
	return std::string(text.substr(begin, end - begin));
}

// recognized 输出"这个字符串是不是我们认识的模式名"。不认识时 mode 仍然是
// kProd(安全侧),但调用点应当就此打一条 WARN —— 把运行模式拼错成
// "Dev "、"develop" 之类而静默按生产跑,是运维最容易踩的坑。
inline RunMode ParseRunMode(std::string_view raw, bool *recognized)
{
	const std::string value = ToLowerAscii(TrimAscii(raw));
	const auto mark = [recognized](bool ok)
	{
		if (recognized != nullptr)
		{
			*recognized = ok;
		}
	};

	if (value.empty())
	{
		// 未配置不算"拼错",只是走默认(生产)。
		mark(true);
		return RunMode::kProd;
	}
	if (value == "prod" || value == "production" || value == "release" || value == "live")
	{
		mark(true);
		return RunMode::kProd;
	}
	if (value == "dev" || value == "development" || value == "local")
	{
		mark(true);
		return RunMode::kDev;
	}
	if (value == "test" || value == "testing")
	{
		mark(true);
		return RunMode::kTest;
	}

	mark(false);
	return RunMode::kProd;
}

struct RunModeResolution
{
	RunMode mode = RunMode::kProd;
	bool recognized = true;
	std::string raw;
};

// 读一次环境变量并解析。调用点自己负责缓存(运行模式在进程生命周期内不会变,
// 而这个值会被每条新连接读到,不能每次都 getenv)。
inline RunModeResolution ResolveRunModeFromEnv(const char *envName)
{
	RunModeResolution result;
	const char *raw = envName != nullptr ? std::getenv(envName) : nullptr;
	result.raw = raw != nullptr ? raw : "";
	result.mode = ParseRunMode(result.raw, &result.recognized);
	return result;
}

// ── 空密钥的处置 ────────────────────────────────────────────────────────────

enum class TokenSecretVerdict
{
	kEnforce,   // 配了密钥:正常做 HMAC 令牌校验
	kDevBypass, // 没配密钥 + 非生产:放行,调用点必须打醒目 WARN
	kRefuse,    // 没配密钥 + 生产:fail-closed(拒绝启动 / 拒绝连接)
};

inline const char *TokenSecretVerdictName(TokenSecretVerdict verdict)
{
	switch (verdict)
	{
	case TokenSecretVerdict::kEnforce:
		return "enforce";
	case TokenSecretVerdict::kDevBypass:
		return "dev_bypass";
	case TokenSecretVerdict::kRefuse:
	default:
		return "refuse";
	}
}

// 纯空白的密钥等同于没配 —— YAML 里写成 `XxxTokenSecret: " "` 不该被当成配好了。
inline TokenSecretVerdict ClassifyTokenSecret(std::string_view secret, RunMode mode)
{
	if (!TrimAscii(secret).empty())
	{
		return TokenSecretVerdict::kEnforce;
	}
	return IsNonProdMode(mode) ? TokenSecretVerdict::kDevBypass : TokenSecretVerdict::kRefuse;
}

} // namespace token_security
