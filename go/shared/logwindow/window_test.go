package logwindow

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestWindowFirstWindowAndRecovery(t *testing.T) {
	var w Window

	// First error is always emitted.
	if ok, streak := w.Admit(1000, 5000); !ok || streak != 1 {
		t.Fatalf("first Admit = (%v, %d), want (true, 1)", ok, streak)
	}
	w.AddExtra(3)

	// Within window: suppressed, streak increments.
	if ok, streak := w.Admit(2000, 5000); ok || streak != 2 {
		t.Fatalf("within window Admit = (%v, %d), want (false, 2)", ok, streak)
	}
	w.AddExtra(2)

	// Window elapsed: admitted with accumulated streak.
	if ok, streak := w.Admit(6000, 5000); !ok || streak != 3 {
		t.Fatalf("after window Admit = (%v, %d), want (true, 3)", ok, streak)
	}

	// Recovery resets both counters and reports totals once.
	failed, extra := w.Recovered()
	if failed != 3 || extra != 5 {
		t.Fatalf("Recovered = (%d, %d), want (3, 5)", failed, extra)
	}
	if f2, e2 := w.Recovered(); f2 != 0 || e2 != 0 {
		t.Fatalf("second Recovered = (%d, %d), want (0, 0)", f2, e2)
	}

	// Next failure after recovery emits immediately again.
	if ok, streak := w.Admit(6500, 5000); !ok || streak != 1 {
		t.Fatalf("post-recovery Admit = (%v, %d), want (true, 1)", ok, streak)
	}
}

func TestWindowConcurrentFirstAdmitSingleWinner(t *testing.T) {
	var w Window
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := w.Admit(1000, 60_000); ok {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := admitted.Load(); got != 1 {
		t.Fatalf("concurrent admitted = %d, want 1", got)
	}
	failed, _ := w.Recovered()
	if failed != 32 {
		t.Fatalf("recovered failed = %d, want 32", failed)
	}
}
