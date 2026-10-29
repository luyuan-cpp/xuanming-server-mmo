package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 清单记录映射来源(D1 收尾,handoff-d1-remaining §2.3 第 2 条、placement-line-handoff §3 第 7 条):
// 合服续跑、搬库续跑与 relocate-abort 必须与清单同一个 -route-source;旧清单没有这个字段 = redis。

func TestCheckManifestRouteSource(t *testing.T) {
	for _, tc := range []struct {
		name, recorded, current string
		wantErr                 string // 空 = 期望通过
	}{
		{"legacy manifest resumed with redis", "", routeSourceRedis, ""},
		{"legacy manifest resumed with db", "", routeSourceDB, "Redis-only tool"},
		{"db resumed with db", routeSourceDB, routeSourceDB, ""},
		{"redis resumed with redis", routeSourceRedis, routeSourceRedis, ""},
		{"db resumed with redis", routeSourceDB, routeSourceRedis, "-route-source db"},
		{"redis resumed with db", routeSourceRedis, routeSourceDB, "-route-source redis"},
		{"unknown recorded value", "mysql", routeSourceDB, "unknown route_source"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkManifestRouteSource("merge", tc.recorded, tc.current)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("checkManifestRouteSource(%q, %q) = %v, want nil", tc.recorded, tc.current, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("checkManifestRouteSource(%q, %q) = %v, want an error containing %q", tc.recorded, tc.current, err, tc.wantErr)
			}
		})
	}
	if got := recordedRouteSource(""); got != routeSourceRedis {
		t.Fatalf("recordedRouteSource(\"\") = %q, want redis (manifests written before -route-source existed)", got)
	}
}

// TestCheckUnmergeRouteSource(B1-3):撤销只拒绝「清单是 db、本次是 redis」与不认识的记录值;redis / 旧清单配 db 撤销放行
// (合服在 P1 之前、撤销在 P4 之后是认可的路径)。
func TestCheckUnmergeRouteSource(t *testing.T) {
	for _, tc := range []struct {
		name, recorded, current string
		wantErr                 string // 空 = 期望通过
	}{
		{"db merge undone with db", routeSourceDB, routeSourceDB, ""},
		{"redis merge undone with redis", routeSourceRedis, routeSourceRedis, ""},
		{"legacy merge undone with redis", "", routeSourceRedis, ""},
		{"redis merge undone with db", routeSourceRedis, routeSourceDB, ""},
		{"legacy merge undone with db", "", routeSourceDB, ""},
		{"db merge undone with redis", routeSourceDB, routeSourceRedis, "-route-source db"},
		{"unknown recorded value", "mysql", routeSourceDB, "unknown route_source"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkUnmergeRouteSource(tc.recorded, tc.current)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("checkUnmergeRouteSource(%q, %q) = %v, want nil", tc.recorded, tc.current, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("checkUnmergeRouteSource(%q, %q) = %v, want an error containing %q", tc.recorded, tc.current, err, tc.wantErr)
			}
		})
	}
}

// 字段真的落盘、真的读回:续跑核对靠的是磁盘上的清单,不是内存里的结构。
func TestManifestRouteSourceRoundTrip(t *testing.T) {
	dir := t.TempDir()

	mergePath := filepath.Join(dir, "merge.json")
	m := newMergeManifest("r-route", 901, 902, "op", time.Now())
	m.RouteSource = routeSourceDB
	if err := saveManifest(mergePath, m); err != nil {
		t.Fatal(err)
	}
	got, err := loadRunManifest(mergePath)
	if err != nil || got == nil || got.RouteSource != routeSourceDB {
		t.Fatalf("merge manifest route_source after round trip = %+v, %v; want db", got, err)
	}

	// 旧清单(没有 route_source 字段)读回来是空串,按 redis 处理。
	legacyPath := filepath.Join(dir, "legacy.json")
	legacy := `{"version":1,"run_id":"r-old","source_zone":901,"target_zone":902,"player_ids":[],"guild_ids":[],` +
		`"rank_members":[],"tables":[],"trade_listing_ids":[],"steps":{}}`
	if err := os.WriteFile(legacyPath, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	old, err := loadRunManifest(legacyPath)
	if err != nil || old == nil || old.RouteSource != "" {
		t.Fatalf("legacy merge manifest = %+v, %v; want route_source empty", old, err)
	}
	if err := checkManifestRouteSource("merge", old.RouteSource, routeSourceDB); err == nil {
		t.Fatal("a legacy (Redis-only) merge manifest must not be resumed with -route-source db")
	}

	relocPath := filepath.Join(dir, "relocate.json")
	rm := newRelocateManifest("reloc-route", 901, 902, 1, []string{"player_database"}, []uint64{1}, "op", time.Now())
	rm.RouteSource = routeSourceDB
	if err := saveRelocateManifest(relocPath, rm); err != nil {
		t.Fatal(err)
	}
	gotReloc, err := loadRelocateManifest(relocPath)
	if err != nil || gotReloc == nil || gotReloc.RouteSource != routeSourceDB {
		t.Fatalf("relocate manifest route_source after round trip = %+v, %v; want db", gotReloc, err)
	}
	if err := checkManifestRouteSource("relocate", gotReloc.RouteSource, routeSourceRedis); err == nil {
		t.Fatal("a relocation written with -route-source db must not be resumed / aborted with -route-source redis")
	}
}
