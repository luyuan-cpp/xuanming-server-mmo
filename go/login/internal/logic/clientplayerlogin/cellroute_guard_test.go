package clientplayerloginlogic

import (
	"testing"

	"shared/cellroute"
)

func TestCheckCellRouteOwnership(t *testing.T) {
	entries, regions, err := cellroute.BuildBalancedEntries([]cellroute.CellSpec{
		{RegionID: 1, CellID: 10},
		{RegionID: 2, CellID: 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	tbl, err := cellroute.NewStaticTable(entries, regions)
	if err != nil {
		t.Fatal(err)
	}
	router, err := cellroute.NewRouter(tbl)
	if err != nil {
		t.Fatal(err)
	}

	// Nil router always allows.
	if err := checkCellRouteOwnership(nil, cellroute.Conf{SelfRegion: 1, SelfCell: 10, EnforceOwnership: true}, 42); err != nil {
		t.Fatalf("nil router rejected: %v", err)
	}

	// Player 1 maps to logical_cell 1 -> Cell (1, 10).
	if err := checkCellRouteOwnership(router, cellroute.Conf{SelfRegion: 1, SelfCell: 10, EnforceOwnership: true}, 1); err != nil {
		t.Fatalf("local cell player rejected: %v", err)
	}

	// Player 3000 maps to logical_cell 3000 -> Cell (2, 20): shadow observes, enforce rejects.
	if err := checkCellRouteOwnership(router, cellroute.Conf{SelfRegion: 1, SelfCell: 10, EnforceOwnership: false}, 3000); err != nil {
		t.Fatalf("shadow mode must not block foreign cell: %v", err)
	}
	if err := checkCellRouteOwnership(router, cellroute.Conf{SelfRegion: 1, SelfCell: 10, EnforceOwnership: true}, 3000); err == nil {
		t.Fatal("enforce mode must reject foreign cell")
	}
}
