package releasetrack

import "testing"

func TestPolicyDeterministicAndBounded(t *testing.T) {
	if _, err := NewPolicy(101, "seed"); err == nil {
		t.Fatal("percent > 100 must fail")
	}
	if _, err := NewPolicy(1, ""); err == nil {
		t.Fatal("enabled canary without seed must fail")
	}
	stable, err := NewPolicy(0, "")
	if err != nil || stable.Select(42) != Stable {
		t.Fatalf("zero canary policy: track=%v err=%v", stable.Select(42), err)
	}
	canary, err := NewPolicy(100, "release-2026-11")
	if err != nil || canary.Select(42) != Canary {
		t.Fatalf("full canary policy: track=%v err=%v", canary.Select(42), err)
	}
	if canary.Select(0) != Stable {
		t.Fatal("id=0 must always map to Stable")
	}

	p, err := NewPolicy(37, "release-2026-11")
	if err != nil {
		t.Fatal(err)
	}
	canaryHits := 0
	for id := uint64(1); id <= 1000; id++ {
		first := p.Select(id)
		if first != Stable && first != Canary {
			t.Fatalf("invalid track id=%d track=%v", id, first)
		}
		if p.Select(id) != first {
			t.Fatalf("non-deterministic track id=%d", id)
		}
		if first == Canary {
			canaryHits++
		}
	}
	if canaryHits == 0 || canaryHits == 1000 {
		t.Fatalf("unexpected canary cohort split: %d/1000", canaryHits)
	}
}
