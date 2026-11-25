package svc

import (
	"context"
	"testing"

	"shared/cellroute"
)

func TestInitCellRouter_Modes(t *testing.T) {
	// Empty mode disables router cleanly.
	r, stop, err := initCellRouter(context.Background(), cellroute.Conf{}, nil)
	if err != nil || r != nil || stop != nil {
		t.Fatalf("empty mode = (router=%v, hasStop=%v, err=%v), want all nil", r != nil, stop != nil, err)
	}

	// Static mode builds deterministic 4096-slot router.
	r, stop, err = initCellRouter(context.Background(), cellroute.Conf{
		Mode: "static",
		Cells: []cellroute.CellSpec{
			{RegionID: 1, CellID: 10},
			{RegionID: 2, CellID: 20},
		},
	}, nil)
	if err != nil || r == nil || stop != nil {
		t.Fatalf("static mode = (router=%v, hasStop=%v, err=%v)", r != nil, stop != nil, err)
	}
	loc, err := r.Route(123456)
	if err != nil || (loc.CellID != 10 && loc.CellID != 20) {
		t.Fatalf("Route(123456) = %+v, err=%v", loc, err)
	}

	// Invalid mode rejected fail-fast.
	if _, _, err := initCellRouter(context.Background(), cellroute.Conf{Mode: "bogus"}, nil); err == nil {
		t.Fatal("expected error for unsupported mode")
	}
}
