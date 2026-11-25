package config

import (
	"testing"

	"shared/cellroute"
)

func TestConfig_CellRouteField(t *testing.T) {
	var cfg Config
	if err := cfg.CellRoute.Validate(); err != nil {
		t.Fatalf("default empty CellRoute should be valid: %v", err)
	}
	cfg.CellRoute = cellroute.Conf{
		Mode:  "static",
		Cells: []cellroute.CellSpec{{RegionID: 1, CellID: 1}},
	}
	if err := cfg.CellRoute.Validate(); err != nil {
		t.Fatalf("static CellRoute should be valid: %v", err)
	}
	cfg.CellRoute = cellroute.Conf{Mode: "invalid"}
	if err := cfg.CellRoute.Validate(); err == nil {
		t.Fatal("expected validation error for invalid CellRoute mode")
	}
}
