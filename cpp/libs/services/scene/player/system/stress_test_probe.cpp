#include "stress_test_probe.h"

#include <algorithm>
#include <chrono>
#include <mutex>
#include <unordered_map>

namespace stresstest_probe
{
    namespace
    {
        // FNV-1a 64-bit primitives. Constants match RFC and the Go mirror
        // in go/db/internal/stresstest/probe.go.
        constexpr uint64_t kFnv64Offset = 0xcbf29ce484222325ULL;
        constexpr uint64_t kFnv64Prime  = 0x00000100000001b3ULL;

        // Two distinct seeds give two independent 64-bit hashes that we
        // concatenate to form the 128-bit signature.
        constexpr uint64_t kSigSeedHi = 0x9e3779b97f4a7c15ULL;
        constexpr uint64_t kSigSeedLo = 0xbf58476d1ce4e5b9ULL;

        // Fnv1a64 is kept for symmetry with the Go reference
        // (go/db/internal/stresstest/probe.go) but the actual ComputeSig
        // below inlines the loop because it chains three discontiguous
        // buffers without allocating. [[maybe_unused]] silences MSVC C4551
        // / GCC -Wunused-function under our /WX build.
        [[maybe_unused]] uint64_t Fnv1a64(uint64_t seed, const uint8_t *data, size_t len)
        {
            uint64_t h = kFnv64Offset ^ seed;
            for (size_t i = 0; i < len; ++i)
            {
                h ^= static_cast<uint64_t>(data[i]);
                h *= kFnv64Prime;
            }
            return h;
        }

        // Big-endian uint64 -> 8 bytes (mirrors binary.BigEndian.PutUint64).
        void EncodeBE64(uint64_t v, uint8_t out[8])
        {
            out[0] = static_cast<uint8_t>(v >> 56);
            out[1] = static_cast<uint8_t>(v >> 48);
            out[2] = static_cast<uint8_t>(v >> 40);
            out[3] = static_cast<uint8_t>(v >> 32);
            out[4] = static_cast<uint8_t>(v >> 24);
            out[5] = static_cast<uint8_t>(v >> 16);
            out[6] = static_cast<uint8_t>(v >> 8);
            out[7] = static_cast<uint8_t>(v);
        }

        // EnabledOnce caches the env-var read so a hot save loop doesn't
        // re-stat /proc/self/environ on every call.
        const bool kEnabled = []() -> bool {
            const char *v = std::getenv("STRESS_TEST_PROBE");
            return v != nullptr && std::strcmp(v, "1") == 0;
        }();

        // Per-player counter table. Hot path: we lock once per save which
        // is fine for the stress-test workload (saves happen on
        // login/logout/scene-switch frequency, not per-tick).
        std::mutex                                gCountersMu;
        std::unordered_map<uint64_t, uint64_t>    gCounters;
    }

    bool IsEnabled()
    {
        return kEnabled;
    }

    void ComputeSig(uint64_t playerId,
                    const std::string &msgType,
                    uint64_t testSeq,
                    uint8_t outSig[16])
    {
        uint8_t pidBuf[8];
        uint8_t seqBuf[8];
        EncodeBE64(playerId, pidBuf);
        EncodeBE64(testSeq, seqBuf);

        // Two passes of FNV-1a-64, each chained over (pid || msgType || seq).
        uint64_t hi;
        {
            uint64_t h = kFnv64Offset ^ kSigSeedHi;
            for (int i = 0; i < 8; ++i) { h ^= pidBuf[i]; h *= kFnv64Prime; }
            for (size_t i = 0; i < msgType.size(); ++i)
            {
                h ^= static_cast<uint8_t>(msgType[i]);
                h *= kFnv64Prime;
            }
            for (int i = 0; i < 8; ++i) { h ^= seqBuf[i]; h *= kFnv64Prime; }
            hi = h;
        }

        uint64_t lo;
        {
            uint64_t h = kFnv64Offset ^ kSigSeedLo;
            for (int i = 0; i < 8; ++i) { h ^= pidBuf[i]; h *= kFnv64Prime; }
            for (size_t i = 0; i < msgType.size(); ++i)
            {
                h ^= static_cast<uint8_t>(msgType[i]);
                h *= kFnv64Prime;
            }
            for (int i = 0; i < 8; ++i) { h ^= seqBuf[i]; h *= kFnv64Prime; }
            lo = h;
        }

        EncodeBE64(hi, outSig);
        EncodeBE64(lo, outSig + 8);
    }

    uint64_t NextSeq(uint64_t playerId)
    {
        // The seq must be strictly monotonically increasing across Scene
        // process restarts, otherwise the verifier (mode=atleast) would
        // false-positive after a restart: the Go consumer's per-key applied
        // guard is gated on the Kafka offset, not on test_seq, so a fresh
        // "test_seq=1" stamp would be applied on top of a much higher
        // pre-restart MySQL value, regressing the row.
        //
        // We solve this without persistence by seeding the counter from
        // wall-clock microseconds since the unix epoch. After any restart
        // the new clock-derived floor is strictly greater than any seq the
        // previous process ever produced (assuming wall-clock doesn't jump
        // backwards by hours, which we accept as out-of-scope).
        const uint64_t now_us = static_cast<uint64_t>(
            std::chrono::duration_cast<std::chrono::microseconds>(
                std::chrono::system_clock::now().time_since_epoch())
                .count());

        std::lock_guard<std::mutex> lock(gCountersMu);
        uint64_t &slot = gCounters[playerId];
        // max(prior+1, now_us) preserves strict monotonicity within the
        // process when many calls land in the same microsecond, while still
        // jumping the floor on the first call after a restart.
        slot = std::max<uint64_t>(slot + 1, now_us);
        return slot;
    }

    namespace
    {
        // StampGeneric stamps any message that has a stress_test_probe
        // sub-message field whose name matches PlayerStressTestProbe. We
        // operate via the typed proto API to keep the call site readable
        // and let the compiler catch field rename / removal at build time.
        template <typename TableMsg>
        void StampImpl(TableMsg &row, const std::string &msgType)
        {
            if (!IsEnabled())
            {
                return;
            }
            const uint64_t playerId = row.player_id();
            const uint64_t seq      = NextSeq(playerId);

            uint8_t sig[16];
            ComputeSig(playerId, msgType, seq, sig);

            auto *probe = row.mutable_stress_test_probe();
            probe->set_test_seq(seq);
            probe->set_test_sig(reinterpret_cast<const char *>(sig), sizeof(sig));
        }
    }

    void StampPlayerDatabase(player_database &row)
    {
        // Must match the proto's full message name for verifier alignment.
        StampImpl(row, "player_database");
    }

    void StampPlayerDatabase1(player_database_1 &row)
    {
        StampImpl(row, "player_database_1");
    }
}
