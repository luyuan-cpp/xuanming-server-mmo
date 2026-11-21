#pragma once
#include <array>
#include <cstdint>
#include <string>
#include <string_view>

// Sha256 — 基于 BoringSSL/OpenSSL EVP_sha256 的单次 SHA-256 摘要封装。
//
// 用于战斗配表指纹(battle_table_fingerprint.cpp)、导航文件校验(navigation.cpp)与维护原件摘要(node.cpp)。
// 对外接口只暴露单次计算(HashBytes / HashToBytes / HashToHex),底层委托给 BoringSSL/OpenSSL,
// 避免在业务仓库中自行维护 FIPS 180-4 轮函数实现。

class Sha256
{
public:
    // 32-byte digest type — exposed for callers that store the raw bytes
    // (e.g. a Redis key payload). Most call sites want HexDigest below.
    using Digest = std::array<uint8_t, 32>;

    // One-shot hash of a contiguous buffer. The interface is intentionally
    // narrow: we don't need streaming because the cross-zone payload is
    // built once and serialized once. Adding update/finalize would invite
    // misuse without value.
    static Digest HashBytes(std::string_view data);

    // Convenience: SHA-256 hash returned as a 32-byte std::string. Suits
    // protobuf `bytes` fields without extra copies.
    static std::string HashToBytes(std::string_view data);

    // Convenience: hex-encoded (lowercase, 64 chars) digest. Useful when
    // logging or comparing in JSON; the wire format prefers HashToBytes.
    static std::string HashToHex(std::string_view data);
};
