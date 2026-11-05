#pragma once

#include <chrono>
#include <cstdint>
#include <string>
#include <string_view>
#include <utility>
#include <vector>
#include <openssl/rand.h>
#include "token_security.h"

namespace caller_auth
{
inline constexpr char kSecretEnv[] = "MMORPG_INTERNAL_AUTH_SECRET";

// 与 go/shared/callerauth 的 v1 契约逐字一致。subject 是原始 SessionDetails 字节。
inline std::string Sign(std::string_view secret, std::string_view fullMethod,
                        std::string_view subject, int64_t timestampMs, std::string_view nonce)
{
    if (secret.size() < 32 || fullMethod.empty() || fullMethod.front() != '/' || nonce.size() < 16)
    {
        return {};
    }
    unsigned char digest[EVP_MAX_MD_SIZE];
    unsigned int length = 0;
    if (EVP_Digest(subject.data(), subject.size(), digest, &length, EVP_sha256(), nullptr) != 1)
    {
        return {};
    }
    const std::string canonical = "mmorpg-login-caller-auth/v1\ngate\n" + std::string(fullMethod) + "\n" +
        token_security::BytesToHex(digest, length) + "\n" + std::to_string(timestampMs) + "\n" + std::string(nonce);
    return token_security::HmacSha256Hex(secret, canonical);
}

// 环境由启动入口注入；每次出站取系统时间与密码学随机 nonce。失败时不写半份 metadata。
inline bool Append(std::vector<std::pair<std::string, std::string>>& metadata, const char* fullMethod,
                   std::string_view subject, const char* secret)
{
    if (secret == nullptr || fullMethod == nullptr)
    {
        return false;
    }
    unsigned char random[16];
    if (RAND_bytes(random, sizeof(random)) != 1)
    {
        return false;
    }
    const auto nonce = token_security::BytesToHex(random, sizeof(random));
    const auto timestamp = std::chrono::duration_cast<std::chrono::milliseconds>(
        std::chrono::system_clock::now().time_since_epoch()).count();
    const auto signature = Sign(secret, fullMethod, subject, timestamp, nonce);
    if (signature.empty())
    {
        return false;
    }
    metadata.emplace_back("x-caller-id", "gate");
    metadata.emplace_back("x-caller-ts", std::to_string(timestamp));
    metadata.emplace_back("x-caller-nonce", nonce);
    metadata.emplace_back("x-caller-sig", signature);
    return true;
}
} // namespace caller_auth
