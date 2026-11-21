#include "sha256.h"

#include <cassert>

#include <openssl/evp.h>

// 使用 BoringSSL/OpenSSL 的 EVP_sha256 计算单次 SHA-256 摘要。

Sha256::Digest Sha256::HashBytes(std::string_view data)
{
    Digest out{};
    unsigned int outLen = 0;
    const void *input = data.empty() ? "" : static_cast<const void *>(data.data());
    const int ok = EVP_Digest(input, data.size(), out.data(), &outLen, EVP_sha256(), nullptr);
    assert(ok == 1 && outLen == out.size());
    if (ok != 1 || outLen != out.size())
    {
        out.fill(0);
    }
    return out;
}

std::string Sha256::HashToBytes(std::string_view data)
{
    const auto d = HashBytes(data);
    return std::string(reinterpret_cast<const char *>(d.data()), d.size());
}

std::string Sha256::HashToHex(std::string_view data)
{
    const auto d = HashBytes(data);
    static const char hex[] = "0123456789abcdef";
    std::string out;
    out.resize(64);
    for (size_t i = 0; i < d.size(); ++i)
    {
        out[i * 2 + 0] = hex[(d[i] >> 4) & 0xF];
        out[i * 2 + 1] = hex[d[i] & 0xF];
    }
    return out;
}
