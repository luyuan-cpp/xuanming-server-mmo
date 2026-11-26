package cellroute

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestBuildBalancedAndDeterministicRouting(t *testing.T) {
	cells := []CellSpec{
		{RegionID: 1, CellID: 101},
		{RegionID: 1, CellID: 102},
		{RegionID: 2, CellID: 201},
	}
	entries, regions, err := BuildBalancedEntries(cells)
	if err != nil {
		t.Fatal(err)
	}
	tbl, err := NewStaticTable(entries, regions)
	if err != nil {
		t.Fatal(err)
	}
	router, err := NewRouter(tbl)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := router.Route(0); !errors.Is(err, ErrInvalidPlayerID) {
		t.Fatalf("Route(0) err = %v, want ErrInvalidPlayerID", err)
	}

	counts := map[uint32]int{}
	for pid := uint64(1); pid <= LogicalCellCount; pid++ {
		loc, err := router.Route(pid)
		if err != nil {
			t.Fatalf("Route(%d) failed: %v", pid, err)
		}
		if loc.RegionID != regions[loc.CellID] {
			t.Fatalf("region mismatch for cell %d: got %d want %d", loc.CellID, loc.RegionID, regions[loc.CellID])
		}
		counts[loc.CellID]++
	}
	// 4096 / 3 = 1366, 1365, 1365
	if counts[101] != 1366 || counts[102] != 1365 || counts[201] != 1365 {
		t.Fatalf("unexpected slot distribution: %+v", counts)
	}
}

func TestStaticTableRejectsMismatchAndInvalidJSON(t *testing.T) {
	entries, regions, err := BuildBalancedEntries([]CellSpec{{RegionID: 1, CellID: 10}})
	if err != nil {
		t.Fatal(err)
	}
	// Tamper region mismatch.
	entries[100].RegionID = 99
	if _, err := NewStaticTable(entries, regions); err == nil {
		t.Fatal("expected region mismatch error")
	}

	// Wrong length.
	if _, err := NewStaticTable(entries[:10], regions); err == nil {
		t.Fatal("expected wrong length error")
	}

	// Invalid JSON payload.
	if _, _, err := ParseTableJSON([]byte("{broken")); err == nil {
		t.Fatal("expected json parse error")
	}

	// Duplicate cell ID rejected fail-closed.
	if _, _, err := BuildBalancedEntries([]CellSpec{{RegionID: 1, CellID: 10}, {RegionID: 1, CellID: 10}}); err == nil {
		t.Fatal("expected duplicate cell error")
	}
}

func TestAtomicTableConcurrentSwapAndJSONRoundTrip(t *testing.T) {
	e1, r1, _ := BuildBalancedEntries([]CellSpec{{RegionID: 1, CellID: 1}})
	t1, _ := NewStaticTable(e1, r1)
	e2, r2, _ := BuildBalancedEntries([]CellSpec{{RegionID: 2, CellID: 2}})
	t2, _ := NewStaticTable(e2, r2)

	raw, err := MarshalTableJSON(7, t2, r2)
	if err != nil {
		t.Fatal(err)
	}
	parsed, ver, err := ParseTableJSON(raw)
	if err != nil || ver != 7 {
		t.Fatalf("ParseTableJSON = ver %d, err %v", ver, err)
	}

	at, err := NewAtomicTable(t1)
	if err != nil {
		t.Fatal(err)
	}
	router, err := NewRouter(at)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(id uint64) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				loc, rerr := router.Route(id + uint64(i) + 1)
				if rerr != nil {
					t.Errorf("Route failed: %v", rerr)
					return
				}
				if loc.CellID != 1 && loc.CellID != 2 {
					t.Errorf("unexpected cell: %+v", loc)
					return
				}
			}
		}(uint64(g * 1000))
	}

	// Malformed payload must keep previous table intact.
	applyPayload("/test/cellroute", []byte("not-json"), at)
	locBefore, _ := router.Route(42)
	if locBefore.CellID != 1 {
		t.Fatalf("malformed update corrupted table: %+v", locBefore)
	}

	if err := at.Swap(parsed); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	locAfter, _ := router.Route(42)
	if locAfter.RegionID != 2 || locAfter.CellID != 2 {
		t.Fatalf("expected swapped table (2,2), got %+v", locAfter)
	}
}

func TestBuildRouter_Modes(t *testing.T) {
	ctx := context.Background()
	r, stop, err := BuildRouter(ctx, Conf{}, nil)
	if err != nil || r != nil || stop != nil {
		t.Fatalf("empty mode = (router=%v, hasStop=%v, err=%v), want all nil", r != nil, stop != nil, err)
	}
	r, stop, err = BuildRouter(ctx, Conf{
		Mode: "static",
		Cells: []CellSpec{
			{RegionID: 1, CellID: 10},
			{RegionID: 2, CellID: 20},
		},
	}, nil)
	if err != nil || r == nil || stop != nil {
		t.Fatalf("static mode = (router=%v, hasStop=%v, err=%v)", r != nil, stop != nil, err)
	}
	if loc, err := r.Route(12345); err != nil || (loc.CellID != 10 && loc.CellID != 20) {
		t.Fatalf("Route(12345) = %+v, err=%v", loc, err)
	}
	if _, _, err := BuildRouter(ctx, Conf{Mode: "bogus"}, nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("bogus mode err = %v, want ErrInvalidConfig", err)
	}
	if _, _, err := BuildRouter(ctx, Conf{Mode: "etcd", EtcdKey: "/k"}, nil); err == nil {
		t.Fatal("expected error for etcd mode with nil client")
	}
}
