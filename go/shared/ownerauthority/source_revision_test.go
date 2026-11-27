package ownerauthority

import (
	"errors"
	"testing"
)

func TestComposeAndSplitSourceRevision(t *testing.T) {
	r1, err := ComposeSourceRevision(1, 1)
	if err != nil {
		t.Fatalf("ComposeSourceRevision(1,1): %v", err)
	}
	r2, err := ComposeSourceRevision(1, 2)
	if err != nil {
		t.Fatalf("ComposeSourceRevision(1,2): %v", err)
	}
	rNextTerm, err := ComposeSourceRevision(2, 1)
	if err != nil {
		t.Fatalf("ComposeSourceRevision(2,1): %v", err)
	}
	if !(SourceRevisionLegacy < r1 && r1 < r2 && r2 < rNextTerm) {
		t.Fatalf("expected strict total order 0 < %d < %d < %d", r1, r2, rNextTerm)
	}
	term, seq := SplitSourceRevision(r2)
	if term != 1 || seq != 2 {
		t.Fatalf("SplitSourceRevision(%d) = (%d, %d), want (1, 2)", r2, term, seq)
	}

	maxRev, err := ComposeSourceRevision(SourceRevisionMaxTerm, SourceRevisionMaxSeq)
	if err != nil {
		t.Fatalf("max boundary failed: %v", err)
	}
	if tm, sq := SplitSourceRevision(maxRev); tm != SourceRevisionMaxTerm || sq != SourceRevisionMaxSeq {
		t.Fatalf("max split = (%d,%d)", tm, sq)
	}

	invalid := [][2]uint64{
		{0, 1},
		{1, 0},
		{SourceRevisionMaxTerm + 1, 1},
		{1, SourceRevisionMaxSeq + 1},
	}
	for _, tc := range invalid {
		if _, err := ComposeSourceRevision(tc[0], tc[1]); !errors.Is(err, ErrInvalidSourceRevision) {
			t.Fatalf("ComposeSourceRevision(%d,%d) err = %v, want ErrInvalidSourceRevision", tc[0], tc[1], err)
		}
	}
}

func TestSourceRevisionMintMonotonicAndExhaustion(t *testing.T) {
	if _, err := NewSourceRevisionMint(0); !errors.Is(err, ErrInvalidSourceRevision) {
		t.Fatalf("term=0 err = %v, want ErrInvalidSourceRevision", err)
	}
	mint, err := NewSourceRevisionMint(7)
	if err != nil {
		t.Fatalf("NewSourceRevisionMint(7): %v", err)
	}
	prev := uint64(0)
	for i := range 5 {
		rev, err := mint.Next()
		if err != nil {
			t.Fatalf("Next #%d: %v", i, err)
		}
		if rev <= prev {
			t.Fatalf("non-monotonic rev %d <= %d", rev, prev)
		}
		prev = rev
	}
	// Simulate near-exhaustion to prove fail-closed without wrap-around.
	mint.seq.Store(SourceRevisionMaxSeq - 1)
	last, err := mint.Next()
	if err != nil {
		t.Fatalf("final valid Next: %v", err)
	}
	if _, sq := SplitSourceRevision(last); sq != SourceRevisionMaxSeq {
		t.Fatalf("last seq = %d, want %d", sq, SourceRevisionMaxSeq)
	}
	if _, err := mint.Next(); !errors.Is(err, ErrInvalidSourceRevision) {
		t.Fatalf("exhausted Next err = %v, want ErrInvalidSourceRevision", err)
	}
}
