#pragma once

// Scene-side stamping for the data-consistency stress probe.
//
// When the env var STRESS_TEST_PROBE=1 is set at process start, every call
// to SavePlayerToRedis stamps each sub-table message with a monotonic
// per-(player_id, msg_type) counter and a deterministic 16-byte signature
// over (player_id || msg_type || test_seq). The Go-side verifier
// (go/db/cmd/verifier) re-validates the signature and uses test_seq to
// assert "last write wins" through Kafka -> DB consumer -> MySQL+Redis.
//
// IMPORTANT: the signature algorithm here MUST match
// go/db/internal/stresstest/probe.go byte-for-byte. Both sides have a
// golden-vector test that locks the algorithm. If you change either side,
// update both AND both fixtures.

#include <string>

#include "proto/common/database/mysql_database_table.pb.h"

namespace stresstest_probe
{
    // IsEnabled reads STRESS_TEST_PROBE once and caches the result. Returns
    // true if and only if the env var was "1" at process start. Thread-safe.
    bool IsEnabled();

    // ComputeSig returns the canonical 16-byte test_sig for the given inputs.
    // Output buffer must hold at least 16 bytes. Mirrors
    // go/db/internal/stresstest.ComputeSig.
    void ComputeSig(uint64_t playerId,
                    const std::string &msgType,
                    uint64_t testSeq,
                    uint8_t outSig[16]);

    // NextSeq returns a monotonically increasing per-player counter.
    // Counters survive for the lifetime of the process; suitable for our
    // smoke / soak tests where players don't migrate between Scene
    // instances mid-test.
    uint64_t NextSeq(uint64_t playerId);

    // StampPlayerDatabase stamps the in-memory sub-table message with a
    // freshly-allocated test_seq and matching test_sig. No-op when
    // IsEnabled() is false, so it's cheap to call unconditionally.
    void StampPlayerDatabase(player_database &row);
    void StampPlayerDatabase1(player_database_1 &row);
}
