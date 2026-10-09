package main

// placement_codec.go 的镜像测试。下面两组向量与 go/shared/placement/placement_test.go 的 codecVectors /
// malformedVectors **逐字相同**:改任何一边都必须同步改另一边,否则两边对「合法值」的理解会静默分叉
// (工具写下的记录 go/db 读不懂,或 go/db 认的记录工具当成畸形)。
// Record 别名只为让向量字面量能从 shared 那边原样拷过来、两边可以直接 diff;其余用例镜像 shared 的同名用例。

import (
	"errors"
	"testing"
)

type Record = placementRecord

var codecVectors = []struct {
	raw  string
	want Record
}{
	{"102:1", Record{StorageID: 102, Version: 1}},
	{"1:18446744073709551615", Record{StorageID: 1, Version: 18446744073709551615}},
	{"1000000:7", Record{StorageID: 1000000, Version: 7}},
	{"102:3:frozen:run-20260928_a", Record{StorageID: 102, Version: 3, Frozen: true, RunID: "run-20260928_a"}},
}

var malformedVectors = []string{
	"",
	"102",
	"102:",
	":1",
	"0:1",
	"102:0",
	"102:1:frozen",
	"102:1:frozen:",
	"102:1:moving:run",
	"102:1:frozen:bad run",
	"102:1:frozen:a:b",
	"-1:1",
	"102:-1",
	"4294967296:1",
	"abc:1",
	"102:1:",
}

func TestPlacementCodec_ParseValidVectors(t *testing.T) {
	for _, v := range codecVectors {
		got, err := parsePlacement(v.raw)
		if err != nil {
			t.Fatalf("parsePlacement(%q) error: %v", v.raw, err)
		}
		if got != v.want {
			t.Fatalf("parsePlacement(%q) = %+v, want %+v", v.raw, got, v.want)
		}
		if enc := got.Encode(); enc != v.raw {
			t.Fatalf("Encode round trip of %q = %q", v.raw, enc)
		}
	}
}

func TestPlacementCodec_ParseMalformedVectors(t *testing.T) {
	for _, raw := range malformedVectors {
		if _, err := parsePlacement(raw); !errors.Is(err, errPlacementMalformed) {
			t.Fatalf("parsePlacement(%q) error = %v, want errPlacementMalformed", raw, err)
		}
	}
}

func TestPlacementCodec_Keys(t *testing.T) {
	// 与 shared/placement 的 Key / HomeZoneKey / MergeFenceKey / CapabilityKey 同一组期望值。
	if got := placementKey(90002); got != "player:placement:90002" {
		t.Fatalf("placementKey = %q", got)
	}
	if got := playerZoneKey(90002); got != "player:zone:90002" {
		t.Fatalf("playerZoneKey = %q", got)
	}
	if got := mergeFenceKey(102); got != "merge:in_progress:102" {
		t.Fatalf("mergeFenceKey = %q", got)
	}
	if got := capabilityKey(101); got != "db:capability:zone:101" {
		t.Fatalf("capabilityKey = %q", got)
	}
	if capabilityRoutingV1 != "placement-routing-v1" {
		t.Fatalf("capability value drifted from shared/placement.CapabilityRoutingV1: %q", capabilityRoutingV1)
	}
}

func TestPlacementCodec_StoreDBNameRoundTrip(t *testing.T) {
	cases := []struct {
		id   uint32
		name string
		ok   bool
	}{
		{0, "", false},
		{1, "zone_1_db", true},
		{102, "zone_102_db", true},
		{maxZoneStorageID, "zone_999999_db", true},
		{firstNonZoneStorageID, "player_store_1000000_db", true},
	}
	for _, c := range cases {
		name, ok := storeDBName(c.id)
		if name != c.name || ok != c.ok {
			t.Fatalf("storeDBName(%d) = (%q,%v), want (%q,%v)", c.id, name, ok, c.name, c.ok)
		}
		if !ok {
			continue
		}
		back, ok := storageIDFromDBName(name)
		if !ok || back != c.id {
			t.Fatalf("storageIDFromDBName(%q) = (%d,%v), want (%d,true)", name, back, ok, c.id)
		}
	}
	// zone 家族与 zoneDBName(镜像 go/db config.ZoneDBName)同规则。
	if name, _ := storeDBName(102); name != zoneDBName(102) {
		t.Fatalf("storeDBName and zoneDBName diverged: %q vs %q", name, zoneDBName(102))
	}
	if defaultGlobalStorageID != 1000000 {
		t.Fatalf("defaultGlobalStorageID drifted: %d", defaultGlobalStorageID)
	}
}

func TestPlacementCodec_StorageIDFromDBNameRejectsOutsideFamilies(t *testing.T) {
	for _, name := range []string{
		"", "zone__db", "zone_0_db", "zone_01_db", "zone_1000000_db", "player_store_5_db",
		"player_store__db", "mmorpg_guild", "zone_1_dbx", "xzone_1_db", "zone_-1_db", "zone_1 _db",
	} {
		if id, ok := storageIDFromDBName(name); ok {
			t.Fatalf("storageIDFromDBName(%q) = %d, want rejection", name, id)
		}
	}
}

func TestPlacementCodec_ValidateRejectsStableWithRunID(t *testing.T) {
	if err := (placementRecord{StorageID: 1, Version: 1, RunID: "x"}).Validate(); !errors.Is(err, errPlacementMalformed) {
		t.Fatalf("stable record with run_id must be malformed, got %v", err)
	}
}

func TestPlacementCodec_SameRouteIgnoresFrozen(t *testing.T) {
	stable := placementRecord{StorageID: 102, Version: 3}
	frozen := placementRecord{StorageID: 102, Version: 3, Frozen: true, RunID: "r1"}
	if !stable.SameRoute(frozen) {
		t.Fatal("freezing must not change the route")
	}
	if stable.SameRoute(placementRecord{StorageID: 1000000, Version: 4}) {
		t.Fatal("a switched record must not be the same route")
	}
}

func TestPlacementCodec_ParseHomeZone(t *testing.T) {
	if z, ok, err := parseHomeZone(""); z != 0 || ok || err != nil {
		t.Fatalf("empty = (%d,%v,%v)", z, ok, err)
	}
	if z, ok, err := parseHomeZone("101"); z != 101 || !ok || err != nil {
		t.Fatalf("101 = (%d,%v,%v)", z, ok, err)
	}
	for _, raw := range []string{"0", "-1", "abc", "4294967296"} {
		if _, _, err := parseHomeZone(raw); !errors.Is(err, errPlacementMalformed) {
			t.Fatalf("parseHomeZone(%q) error = %v, want errPlacementMalformed", raw, err)
		}
	}
}

func TestPlacementCodec_EffectiveStorage(t *testing.T) {
	rec := placementRecord{StorageID: 102, Version: 1}
	if got := effectiveStorage(rec, true, 101, true, 7); got != 102 {
		t.Fatalf("record must win, got %d", got)
	}
	if got := effectiveStorage(placementRecord{}, false, 101, true, 7); got != 101 {
		t.Fatalf("absent record must fall back to home_zone, got %d", got)
	}
	if got := effectiveStorage(placementRecord{}, false, 0, false, 7); got != 7 {
		t.Fatalf("no record and no home must fall back to the processing zone, got %d", got)
	}
}
