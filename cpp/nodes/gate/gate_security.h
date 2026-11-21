#pragma once

// gate 安全闸门:客户端令牌密钥的运行模式门控 + GM 面调用方鉴权。
//
// 为什么单独一个头,而且**不依赖 muduo / protobuf / 引擎**任何东西:
//   1) 这里全是纯函数(输入 -> 结论),可以在没有整套 C++ 引擎的机器上直接
//      编译单测,见 cpp/nodes/gate/tests/gate_security_test.cpp;
//   2) 同一条结论在不同调用点该打 FATAL(启动拒绝)还是 WARN(降级留痕)
//      由调用点决定,所以本头只**返回结论,不打日志**;
//   3) 共享基元已全部提到引擎层 core/security/(token_security.h 与 gm_security.h),
//      本头只保留 gate 专属运行模式缓存与 using 别名,对外 API 完全不变。

#include "security/gm_security.h"
#include "security/token_security.h"

namespace gate_security
{

// ── HMAC / 运行模式 / 空密钥处置:唯一实现在引擎层 ─────────────────────────

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

// ── GM 客户端消息闸与 GM 面鉴权:唯一实现在引擎层 core/security/gm_security.h ─

using gm_security::BuildGmCanonicalString;
using gm_security::ClassifyGmClientMessage;
using gm_security::GmAuthOutcome;
using gm_security::GmAuthResult;
using gm_security::GmAuthResultName;
using gm_security::GmClientMessageVerdict;
using gm_security::GmClientMessageVerdictName;
using gm_security::GmEnvelope;
using gm_security::GmNonceCache;
using gm_security::IsAsciiDigits;
using gm_security::IsLowerHex;
using gm_security::kGmAdminSecretEnv;
using gm_security::kGmAuthSkewEnv;
using gm_security::kGmDefaultSkewSeconds;
using gm_security::kGmEnvelopeSeparator;
using gm_security::kGmMaxNonceLength;
using gm_security::kGmMaxOperatorLength;
using gm_security::kGmMaxSkewSeconds;
using gm_security::kGmMinSkewSeconds;
using gm_security::kGmSignatureHexLength;
using gm_security::ParseGmEnvelope;
using gm_security::ProcessGmNonceCache;
using gm_security::ResolveGmSkewSeconds;
using gm_security::VerifyGmRequest;
using gm_security::VerifyGmRequestFromEnv;

// 生产调用点的便利封装:模式读进程内缓存的 GATE_RUN_MODE。
inline bool GmClientMessagesAllowed()
{
	return ClassifyGmClientMessage(CurrentRunMode()) == GmClientMessageVerdict::kAllow;
}

} // namespace gate_security
