#pragma once

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <memory>
#include <string>
#include <string_view>
#include <utility>
#include <vector>
#include <openssl/crypto.h>
#include <openssl/evp.h>
#include <openssl/rand.h>
#include "token_security.h"

// gate → Go 服务的会话身份签名(docs/design/gate-online-identity.md §3、§10.1)。
//
// 与 go/shared/callerauth 的 v2 契约逐字一致:canonical 串相同,签名算法是 Ed25519(RFC 8032)。
// gate 是唯一的签名方,持私钥;login 与在线域服务只持公钥,签不出名 —— 任何一个验证方被攻破,
// 都冒充不了 gate 去调别的服务。2026-11-11 之前全体共用一把 HMAC 密钥(v1,已删除)。
namespace caller_auth
{
// gate 的签名私钥:32 字节 Ed25519 种子的十六进制(64 个字符)。只注入 gate,走 Secret,不进日志。
inline constexpr char kSigningKeyEnv[] = "MMORPG_INTERNAL_AUTH_SIGNING_KEY";
// 验证方的公钥列表:逗号分隔,每项是 32 字节公钥的十六进制。gate 也读它:自己的公钥不在列表里就拒绝启动,
// 把「签名私钥与验证方公钥不配对」挡在启动时,而不是等上线后全量验签失败。与 Go 的 callerauth.PublicKeysEnv 同名。
inline constexpr char kPublicKeysEnv[] = "MMORPG_INTERNAL_AUTH_PUBLIC_KEYS";
// canonical 第一行,钉死串格式与算法;与 Go 的 canonicalVersion 是同一条字面量。
inline constexpr char kCanonicalVersion[] = "mmorpg-gate-identity/v2";

inline constexpr std::size_t kSeedBytes = 32;
inline constexpr std::size_t kPublicKeyBytes = 32;
inline constexpr std::size_t kSignatureBytes = 64;

struct SigningKeyDeleter
{
    void operator()(EVP_PKEY* key) const { EVP_PKEY_free(key); }
};
using SigningKey = std::unique_ptr<EVP_PKEY, SigningKeyDeleter>;

inline int HexDigitValue(char c)
{
    if (c >= '0' && c <= '9') return c - '0';
    if (c >= 'a' && c <= 'f') return c - 'a' + 10;
    if (c >= 'A' && c <= 'F') return c - 'A' + 10;
    return -1;
}

// 恰好 bytes 字节的十六进制,大小写都认,不许任何其他字符(含空白)。与 Go 的 decodeFixedHex 同一规则。
inline bool DecodeFixedHex(std::string_view hex, std::size_t bytes, std::string& out)
{
    if (hex.size() != bytes * 2)
    {
        return false;
    }
    out.assign(bytes, '\0');
    for (std::size_t i = 0; i < bytes; ++i)
    {
        const int high = HexDigitValue(hex[2 * i]);
        const int low = HexDigitValue(hex[2 * i + 1]);
        if (high < 0 || low < 0)
        {
            return false;
        }
        out[i] = static_cast<char>((high << 4) | low);
    }
    return true;
}

// 由种子十六进制构造私钥;不合格返回空指针。种子的明文副本用完即抹掉。
inline SigningKey ParseSeedHex(std::string_view seedHex)
{
    std::string seed;
    if (!DecodeFixedHex(seedHex, kSeedBytes, seed))
    {
        return nullptr;
    }
    SigningKey key(EVP_PKEY_new_raw_private_key(EVP_PKEY_ED25519, nullptr,
        reinterpret_cast<const unsigned char*>(seed.data()), seed.size()));
    OPENSSL_cleanse(seed.data(), seed.size());
    return key;
}

// 私钥配对的 32 字节公钥;失败返回空串。
inline std::string PublicKeyBytes(const EVP_PKEY* key)
{
    if (key == nullptr)
    {
        return {};
    }
    std::string raw(kPublicKeyBytes, '\0');
    std::size_t length = raw.size();
    if (EVP_PKEY_get_raw_public_key(key, reinterpret_cast<unsigned char*>(raw.data()), &length) != 1 ||
        length != kPublicKeyBytes)
    {
        return {};
    }
    return raw;
}

// 公钥列表是否合格且含有这一把:逗号分隔、至少一项、每项恰好 64 个十六进制字符、不许空项与重复
// (与 Go 的 ParsePublicKeys 同一规则,验证方拒启的列表 gate 也拒启)。
inline bool PublicKeyListed(std::string_view publicKeys, std::string_view publicKey)
{
    if (publicKeys.empty() || publicKey.size() != kPublicKeyBytes)
    {
        return false;
    }
    std::vector<std::string> keys;
    bool found = false;
    std::size_t start = 0;
    while (true)
    {
        const std::size_t comma = publicKeys.find(',', start);
        const auto item = publicKeys.substr(start, comma == std::string_view::npos ? std::string_view::npos : comma - start);
        std::string raw;
        if (!DecodeFixedHex(item, kPublicKeyBytes, raw))
        {
            return false;
        }
        for (const auto& seen : keys)
        {
            if (seen == raw)
            {
                return false;
            }
        }
        found = found || raw == publicKey;
        keys.push_back(std::move(raw));
        if (comma == std::string_view::npos)
        {
            return found;
        }
        start = comma + 1;
    }
}

// canonical 串:版本行、调用方、目标方法、主体摘要、毫秒时间戳、nonce,LF 分隔、末尾无换行;
// 与 go/shared/callerauth.Canonical 逐字节一致。subject 是原始 SessionDetails 字节。失败返回空串。
inline std::string Canonical(std::string_view fullMethod, std::string_view subject, int64_t timestampMs,
                             std::string_view nonce)
{
    unsigned char digest[EVP_MAX_MD_SIZE];
    unsigned int length = 0;
    if (EVP_Digest(subject.data(), subject.size(), digest, &length, EVP_sha256(), nullptr) != 1)
    {
        return {};
    }
    std::string canonical(kCanonicalVersion);
    canonical += "\ngate\n";
    canonical += fullMethod;
    canonical += '\n';
    canonical += token_security::BytesToHex(digest, length);
    canonical += '\n';
    canonical += std::to_string(timestampMs);
    canonical += '\n';
    canonical += nonce;
    return canonical;
}

// 对任意字节出 Ed25519 签名的小写十六进制(128 个字符);失败返回空串。
// 每次调用新建 EVP_MD_CTX,多个 IO 线程可以共用同一把只读私钥。
inline std::string SignHex(EVP_PKEY* key, std::string_view data)
{
    if (key == nullptr)
    {
        return {};
    }
    std::unique_ptr<EVP_MD_CTX, decltype(&EVP_MD_CTX_free)> context(EVP_MD_CTX_new(), &EVP_MD_CTX_free);
    unsigned char signature[kSignatureBytes];
    std::size_t length = sizeof(signature);
    if (!context || EVP_DigestSignInit(context.get(), nullptr, nullptr, nullptr, key) != 1 ||
        EVP_DigestSign(context.get(), signature, &length, reinterpret_cast<const unsigned char*>(data.data()),
            data.size()) != 1 ||
        length != sizeof(signature))
    {
        return {};
    }
    return token_security::BytesToHex(signature, static_cast<unsigned int>(length));
}

// gate 进程唯一的签名私钥。只在 main 起任何线程之前由 InitProcessSigner 写一次,之后各线程只读。
inline SigningKey& ProcessSigningKey()
{
    static SigningKey key;
    return key;
}

// gate 启动时调用一次:读私钥种子与公钥列表,核对配对后装进进程级签名器。
// 返回空串表示成功,否则是英文的拒启原因(只点变量名与问题,不回显值)。
inline std::string InitProcessSigner(const char* seedHex, const char* publicKeys)
{
    if (seedHex == nullptr || *seedHex == '\0')
    {
        return std::string(kSigningKeyEnv) + " is not set";
    }
    auto key = ParseSeedHex(seedHex);
    if (!key)
    {
        return std::string(kSigningKeyEnv) + " must be exactly 64 hex characters (a 32-byte Ed25519 seed)";
    }
    const auto publicKey = PublicKeyBytes(key.get());
    if (publicKey.empty())
    {
        return std::string("failed to derive the public key from ") + kSigningKeyEnv;
    }
    if (publicKeys == nullptr || *publicKeys == '\0')
    {
        return std::string(kPublicKeysEnv) + " is not set";
    }
    if (!PublicKeyListed(publicKeys, publicKey))
    {
        return std::string(kPublicKeysEnv) + " is malformed or does not list the public key of " + kSigningKeyEnv;
    }
    ProcessSigningKey() = std::move(key);
    return {};
}

// 给一次出站调用挂签名头。每次现取系统时间与密码学随机 nonce;私钥未初始化或任何一步失败时不写半份 metadata。
inline bool Append(std::vector<std::pair<std::string, std::string>>& metadata, const char* fullMethod,
                   std::string_view subject)
{
    EVP_PKEY* key = ProcessSigningKey().get();
    if (key == nullptr || fullMethod == nullptr || fullMethod[0] != '/')
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
    const auto canonical = Canonical(fullMethod, subject, timestamp, nonce);
    if (canonical.empty())
    {
        return false;
    }
    const auto signature = SignHex(key, canonical);
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
