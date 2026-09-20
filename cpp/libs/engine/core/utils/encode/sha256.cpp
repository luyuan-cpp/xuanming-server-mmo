#include "sha256.h"

#include <array>
#include <cstdint>
#include <cstring>

// SHA-256 — FIPS 180-4. Reference implementation, public domain shape.
//
// This file is intentionally small and obvious. Don't optimize without
// benchmarks: the K-table + round function is cache-warm during a single
// HashBytes() call, and the only current input (the battle table
// fingerprint buffer, hashed at table load / refresh and then cached) is
// off any per-tick path.
// (Originally written for cross-zone migration payloads; that chain is gone.)

namespace
{
    // Round constants — first 32 bits of fractional parts of cube roots
    // of the first 64 primes (FIPS 180-4 §4.2.2).
    constexpr uint32_t K[64] = {
        0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
        0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
        0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
        0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
        0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
        0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
        0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
        0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
    };

    // Initial hash values (FIPS 180-4 §5.3.3).
    constexpr uint32_t H0[8] = {
        0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a,
        0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19,
    };

    inline uint32_t Rotr(uint32_t x, unsigned n) { return (x >> n) | (x << (32 - n)); }
    inline uint32_t BigSig0(uint32_t x) { return Rotr(x, 2) ^ Rotr(x, 13) ^ Rotr(x, 22); }
    inline uint32_t BigSig1(uint32_t x) { return Rotr(x, 6) ^ Rotr(x, 11) ^ Rotr(x, 25); }
    inline uint32_t SmallSig0(uint32_t x) { return Rotr(x, 7) ^ Rotr(x, 18) ^ (x >> 3); }
    inline uint32_t SmallSig1(uint32_t x) { return Rotr(x, 17) ^ Rotr(x, 19) ^ (x >> 10); }
    inline uint32_t Ch(uint32_t x, uint32_t y, uint32_t z) { return (x & y) ^ (~x & z); }
    inline uint32_t Maj(uint32_t x, uint32_t y, uint32_t z) { return (x & y) ^ (x & z) ^ (y & z); }

    // Process one 64-byte block. State H[8] is read-modify-write.
    void TransformBlock(uint32_t H[8], const uint8_t block[64])
    {
        uint32_t W[64];
        // Big-endian word load — same byte order as the SHA-256 spec.
        for (int t = 0; t < 16; ++t)
        {
            W[t] = (uint32_t(block[t * 4 + 0]) << 24) |
                   (uint32_t(block[t * 4 + 1]) << 16) |
                   (uint32_t(block[t * 4 + 2]) << 8)  |
                   (uint32_t(block[t * 4 + 3]) << 0);
        }
        for (int t = 16; t < 64; ++t)
        {
            W[t] = SmallSig1(W[t - 2]) + W[t - 7] + SmallSig0(W[t - 15]) + W[t - 16];
        }

        uint32_t a = H[0], b = H[1], c = H[2], d = H[3];
        uint32_t e = H[4], f = H[5], g = H[6], h = H[7];

        for (int t = 0; t < 64; ++t)
        {
            const uint32_t T1 = h + BigSig1(e) + Ch(e, f, g) + K[t] + W[t];
            const uint32_t T2 = BigSig0(a) + Maj(a, b, c);
            h = g; g = f; f = e;
            e = d + T1;
            d = c; c = b; b = a;
            a = T1 + T2;
        }

        H[0] += a; H[1] += b; H[2] += c; H[3] += d;
        H[4] += e; H[5] += f; H[6] += g; H[7] += h;
    }
}

Sha256::Digest Sha256::HashBytes(std::string_view data)
{
    uint32_t H[8];
    std::memcpy(H, H0, sizeof(H));

    // Process complete 64-byte blocks from the input. SHA-256 is byte-
    // oriented so we never touch sub-byte alignment.
    const uint8_t *p = reinterpret_cast<const uint8_t *>(data.data());
    size_t remaining = data.size();
    while (remaining >= 64)
    {
        TransformBlock(H, p);
        p += 64;
        remaining -= 64;
    }

    // Pad: 0x80, then zeros, then 64-bit big-endian total bit-length.
    // The last block(s) may overflow 64 bytes if the tail + length
    // doesn't fit, requiring a second pass.
    uint8_t buf[128];
    std::memset(buf, 0, sizeof(buf));
    std::memcpy(buf, p, remaining);
    buf[remaining] = 0x80;

    const uint64_t bitLen = static_cast<uint64_t>(data.size()) * 8;
    const size_t padTo = (remaining < 56) ? 64 : 128;
    // Pack length big-endian into the last 8 bytes of padTo.
    for (int i = 0; i < 8; ++i)
    {
        buf[padTo - 1 - i] = static_cast<uint8_t>(bitLen >> (8 * i));
    }

    TransformBlock(H, buf);
    if (padTo == 128)
    {
        TransformBlock(H, buf + 64);
    }

    // Big-endian state pack — 32-byte output.
    Digest out{};
    for (int i = 0; i < 8; ++i)
    {
        out[i * 4 + 0] = static_cast<uint8_t>(H[i] >> 24);
        out[i * 4 + 1] = static_cast<uint8_t>(H[i] >> 16);
        out[i * 4 + 2] = static_cast<uint8_t>(H[i] >> 8);
        out[i * 4 + 3] = static_cast<uint8_t>(H[i] >> 0);
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
