package releasetrack

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
)

// Policy assigns a deterministic stable/canary cohort for a numeric subject ID
// (ported from xuanming-server/pkg/releasetrack).
// The returned Track is only the assignment intent; when canary lacks capacity
// and falls back to stable, callers must persist the actual track returned by
// orchestration rather than treating Policy.Select as final truth.
type Policy struct {
	percent uint32
	seed    string
}

// NewPolicy constructs a cohort policy with canary percentage in [0, 100].
// A non-empty seed is required whenever percent > 0 so rollouts are reproducible
// and can be re-salted per release.
func NewPolicy(percent uint32, seed string) (Policy, error) {
	if percent > 100 {
		return Policy{}, fmt.Errorf("releasetrack: canary percent %d out of range [0,100]", percent)
	}
	if percent > 0 && seed == "" {
		return Policy{}, errors.New("releasetrack: canary seed required when canary percent > 0")
	}
	return Policy{percent: percent, seed: seed}, nil
}

// Select deterministically buckets id into Stable or Canary using SHA-256(seed + ":" + id).
// Zero IDs always map to Stable to fail safe when caller identity is missing.
func (p Policy) Select(id uint64) Track {
	if p.percent == 0 || id == 0 {
		return Stable
	}
	if p.percent == 100 {
		return Canary
	}
	sum := sha256.Sum256([]byte(p.seed + ":" + strconv.FormatUint(id, 10)))
	if binary.BigEndian.Uint64(sum[:8])%100 < uint64(p.percent) {
		return Canary
	}
	return Stable
}
