#pragma once
#include <array>
#include <cstdint>
#include <string>
#include <string_view>

// Sha256 — minimal, dependency-free SHA-256 implementation.
//
// Why this lives here:
//   现在唯一的调用方是战斗配表指纹(services/battle/data/battle_table_fingerprint.cpp:
//   scene / battle 两端对同一批配表字节算指纹比对),需要的只是一个稳定、无依赖、一次算完的
//   SHA-256。
//
//   历史:它最初(2026-05-23)是为跨 zone 搬数据链的 payload 去重而写 —— 让目的端区分
//   "Kafka 原样重投"与"reaper 带着变了的 payload 重发"。那条链(player_migrate / ACK /
//   CrossZoneReaper)已按 docs/design/cross-zone-scene-travel.md CZ-1 整条删除,该用途不复
//   存在。当时"scene 节点不链接 OpenSSL,为一次哈希引入它不划算"的前提也只代表当时:
//   scene.vcxproj / CMakeLists 现在已有 OpenSSL(token_security.h 的 HMAC 用它)。
//   保留这份 ~150 行的自包含实现,是因为指纹契约已经建立在它上面,不是因为没得选。
//
// Implementation note:
//   Standard FIPS 180-4 SHA-256, public-domain reference structure.
//   Big-endian state on output (32 bytes). Tested against the canonical
//   test vectors ("abc" → ba7816bf...) in cpp/tests if a test gets added
//   for it; the reference implementation is small enough to be reviewed
//   by hand.

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
