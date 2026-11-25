package loginpregatelogic

import (
	"testing"

	"login/internal/logic/pkg/loginqueue"
	"shared/cellroute"
)

func TestSelectCellAffinityCandidates(t *testing.T) {
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

	candidates := []loginqueue.GateCandidate{
		{NodeID: 1, ZoneID: 10},
		{NodeID: 2, ZoneID: 20},
	}

	// Player 1 -> Cell 10 -> filters to NodeID 1.
	got := selectCellAffinityCandidates(router, 1, candidates)
	if len(got) != 1 || got[0].NodeID != 1 {
		t.Fatalf("cell 10 affinity = %+v, want NodeID 1", got)
	}

	// Player 3000 -> Cell 20 -> filters to NodeID 2.
	got = selectCellAffinityCandidates(router, 3000, candidates)
	if len(got) != 1 || got[0].NodeID != 2 {
		t.Fatalf("cell 20 affinity = %+v, want NodeID 2", got)
	}

	// Fallback when no gate advertises matching cell ID.
	unmatched := []loginqueue.GateCandidate{{NodeID: 9, ZoneID: 99}}
	got = selectCellAffinityCandidates(router, 1, unmatched)
	if len(got) != 1 || got[0].NodeID != 9 {
		t.Fatalf("fallback = %+v, want original slice", got)
	}
}
