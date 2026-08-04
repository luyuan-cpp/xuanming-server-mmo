package kafka

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// These tests guard the on-wire retry-queue payload format produced by
// wrapRetryPayload / consumed by unwrapRetryPayload. The format is the
// single source of truth for the per-key applied-seq guard (Bug 3 / TC5b)
// AND the origin-partition routing (retries must go back to the worker
// that owns the key's serialization), so a silent change here would
// re-open those regressions without any of the higher-level kafka
// consumer tests noticing.

func TestRetryPayload_RoundTrip(t *testing.T) {
	cases := []struct {
		name      string
		seq       uint64
		partition int32
		body      []byte
	}{
		{"empty body", 1, 0, nil},
		{"small body", 42, 3, []byte("hello")},
		{"max seq", ^uint64(0), 15, bytes.Repeat([]byte{0xAB}, 256)},
		{"binary body that looks like proto", 7, 9, []byte{0x08, 0x01, 0x12, 0x03, 'a', 'b', 'c'}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := wrapRetryPayload(tc.seq, tc.partition, tc.body)
			if got := wrapped[0]; got != retryPayloadMagicV2 {
				t.Fatalf("magic byte: want 0x%02x got 0x%02x", retryPayloadMagicV2, got)
			}
			if got := binary.BigEndian.Uint64(wrapped[1:9]); got != tc.seq {
				t.Fatalf("seq round-trip: want %d got %d", tc.seq, got)
			}
			if got := int32(binary.BigEndian.Uint32(wrapped[9:13])); got != tc.partition {
				t.Fatalf("partition round-trip: want %d got %d", tc.partition, got)
			}
			if !bytes.Equal(wrapped[13:], tc.body) {
				t.Fatalf("body round-trip mismatch")
			}

			gotSeq, gotPartition, hasPartition, gotBody := unwrapRetryPayload(wrapped)
			if gotSeq != tc.seq {
				t.Fatalf("unwrap seq: want %d got %d", tc.seq, gotSeq)
			}
			if !hasPartition {
				t.Fatalf("v2 payload must report hasPartition=true")
			}
			if gotPartition != tc.partition {
				t.Fatalf("unwrap partition: want %d got %d", tc.partition, gotPartition)
			}
			if !bytes.Equal(gotBody, tc.body) {
				t.Fatalf("unwrap body mismatch")
			}
		})
	}
}

// TestRetryPayload_V1PayloadReportsUnknownPartition guards the rolling-upgrade
// decoder for [magic1][seq][body]. The body remains inspectable, but
// hasPartition=false forces the consumer to durable quarantine; it must never
// invent provenance with Key%N.
func TestRetryPayload_V1PayloadReportsUnknownPartition(t *testing.T) {
	body := []byte{0x08, 0x01, 0x12, 0x03, 'a', 'b', 'c'}
	v1 := make([]byte, 1+8+len(body))
	v1[0] = retryPayloadMagic
	binary.BigEndian.PutUint64(v1[1:9], 42)
	copy(v1[9:], body)

	seq, _, hasPartition, gotBody := unwrapRetryPayload(v1)
	if seq != 42 {
		t.Fatalf("v1 payload seq: want 42 got %d", seq)
	}
	if hasPartition {
		t.Fatalf("v1 payload must report hasPartition=false")
	}
	if !bytes.Equal(gotBody, body) {
		t.Fatalf("v1 payload body mismatch")
	}
}

// TestRetryPayload_LegacyPayloadDecodesForQuarantine guards the backward-
// compat decoder: an older bare DBTask remains recoverable for operator
// inspection, but seq=0/hasPartition=false makes execution fail-closed into
// the dead queue.
//
// The unambiguity argument: proto3 wire format always starts with a
// non-zero field tag, and the smallest possible tag byte is 0x08 (field
// 1, varint). 0x01/0x02 cannot appear as the first byte of a valid proto3
// payload, so unwrapRetryPayload's discriminator is unconditionally safe.
func TestRetryPayload_LegacyPayloadDecodesForQuarantine(t *testing.T) {
	legacy := []byte{0x08, 0x01, 0x12, 0x03, 'a', 'b', 'c'}
	seq, _, hasPartition, body := unwrapRetryPayload(legacy)
	if seq != 0 {
		t.Fatalf("legacy payload should yield seq=0, got %d", seq)
	}
	if hasPartition {
		t.Fatalf("legacy payload must report hasPartition=false")
	}
	if !bytes.Equal(body, legacy) {
		t.Fatalf("legacy payload body must pass through unchanged")
	}
}

// TestRetryPayload_TruncatedFallsBackToLegacy guards a corner case:
// a payload short enough to lack the [magic][seq](+[partition]) prefix
// MUST be surfaced as a legacy payload (seq=0, body unchanged) rather
// than panic on a slice-out-of-range read.
func TestRetryPayload_TruncatedFallsBackToLegacy(t *testing.T) {
	for _, magic := range []byte{retryPayloadMagic, retryPayloadMagicV2} {
		for _, length := range []int{0, 1, 5, 8} {
			buf := bytes.Repeat([]byte{magic}, length)
			seq, _, _, body := unwrapRetryPayload(buf)
			if seq != 0 {
				t.Fatalf("magic=0x%02x len=%d: short payload must yield seq=0, got %d", magic, length, seq)
			}
			if !bytes.Equal(body, buf) {
				t.Fatalf("magic=0x%02x len=%d: short payload body must pass through unchanged", magic, length)
			}
		}
	}
	// v2 magic with only the v1-sized prefix (9..12 bytes) must not be
	// misparsed as v2; it has no valid task bytes either way, but it must
	// not panic.
	for _, length := range []int{9, 12} {
		buf := bytes.Repeat([]byte{retryPayloadMagicV2}, length)
		_, _, hasPartition, _ := unwrapRetryPayload(buf)
		if hasPartition {
			t.Fatalf("len=%d: truncated v2 payload must not report hasPartition", length)
		}
	}
}

// TestRetryPayload_MagicByteIsUnambiguous documents the invariant that
// neither magic byte can collide with any legitimate first byte of a
// proto3-serialized DBTask. If this test ever fires, either the magic
// byte must change or the format must gain a length-prefix instead.
func TestRetryPayload_MagicByteIsUnambiguous(t *testing.T) {
	for _, magic := range []byte{retryPayloadMagic, retryPayloadMagicV2} {
		if magic >= 0x08 {
			t.Fatalf("magic=0x%02x can collide with proto3 wire-format first byte (≥ 0x08)", magic)
		}
	}
}
