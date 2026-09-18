#pragma once
#include <array>
#include <cstdint>
#include <string>
#include <string_view>

// Sha256 — minimal, dependency-free SHA-256 implementation.
//
// Why this lives here:
//   The project does not link OpenSSL into cpp/nodes/scene (verified
//   2026-05-23 via `grep openssl scene.vcxproj` → no match). Cross-zone
//   migration's payload-dedup check (cross-zone-readiness-audit.md §7
//   失败 D) needs a stable, fast hash of the migration payload bytes
//   so the destination can distinguish exact-duplicate Kafka redelivery
//   from a reaper retry that carries a mutated payload.
//
//   Pulling OpenSSL into the scene node just for one hash call would
//   add a multi-MB dependency to the runtime image and complicate the
//   K8s payload pipeline. A self-contained ~150-line implementation
//   here is the right size — single-call use, no streaming required,
//   all heavy work amortizes against the Kafka send.
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
