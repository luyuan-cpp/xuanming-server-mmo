package main

import (
	"strconv"
	"strings"
	"testing"

	"db/internal/playerstore"
)

// 预切语句:聚簇复合主键按值切要给全主键的每一列;player_store 把 [0, 2^63) 等分成 16 段,发件箱按片号 1..15 切。
func TestPresplitStatements(t *testing.T) {
	tables := presplitTables()
	if len(tables) != 2 || tables[0].name != "player_store" || tables[1].name != playerstore.AuditOutboxTable {
		t.Fatalf("tables = %+v", tables)
	}

	outbox := tables[1].stmt
	if !strings.HasPrefix(outbox, "SPLIT TABLE `player_audit_outbox` BY (1, 0, 0, 0, 0), (2, 0, 0, 0, 0), ") ||
		!strings.HasSuffix(outbox, ", (15, 0, 0, 0, 0)") || strings.Count(outbox, "(") != playerstore.AuditOutboxShards-1 {
		t.Fatalf("outbox split = %q", outbox)
	}

	store := tables[0].stmt
	const head = "SPLIT TABLE `player_store` BY "
	if !strings.HasPrefix(store, head) {
		t.Fatalf("player_store split = %q", store)
	}
	bounds := strings.Split(strings.TrimPrefix(store, head), ", (")
	if len(bounds) != tidbPresplitRegions-1 {
		t.Fatalf("player_store bounds = %d, want %d", len(bounds), tidbPresplitRegions-1)
	}
	var previous uint64
	for i, bound := range bounds {
		bound = strings.TrimPrefix(bound, "(")
		value, kind, ok := strings.Cut(strings.TrimSuffix(bound, ")"), ", ")
		if !ok || kind != "0" {
			t.Fatalf("bound %d = %q, want (player_id, 0)", i, bound)
		}
		id, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		if want := uint64(i+1) * (playerIDSpace / tidbPresplitRegions); id != want || id <= previous || id >= playerIDSpace {
			t.Fatalf("bound %d = %d, want %d (evenly spaced, increasing, below 2^63)", i, id, want)
		}
		previous = id
	}
}
