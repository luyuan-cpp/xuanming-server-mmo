package placement

import (
	"errors"
	"testing"
)

// codecVectors / malformedVectors 是落点记录编解码的测试向量。tools/merge_zone/placement_codec_test.go
// 用同一组字面量钉住镜像实现:改这里必须同步改那边,否则两边对「合法值」的理解会静默分叉。
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

func TestParseValidVectors(t *testing.T) {
	for _, v := range codecVectors {
		got, err := Parse(v.raw)
		if err != nil {
			t.Fatalf("Parse(%q) error: %v", v.raw, err)
		}
		if got != v.want {
			t.Fatalf("Parse(%q) = %+v, want %+v", v.raw, got, v.want)
		}
		if enc := got.Encode(); enc != v.raw {
			t.Fatalf("Encode round trip of %q = %q", v.raw, enc)
		}
	}
}

func TestParseMalformedVectors(t *testing.T) {
	for _, raw := range malformedVectors {
		if _, err := Parse(raw); !errors.Is(err, ErrMalformed) {
			t.Fatalf("Parse(%q) error = %v, want ErrMalformed", raw, err)
		}
	}
}

func TestKeys(t *testing.T) {
	if got := Key(90002); got != "player:placement:90002" {
		t.Fatalf("Key = %q", got)
	}
	if got := HomeZoneKey(90002); got != "player:zone:90002" {
		t.Fatalf("HomeZoneKey = %q", got)
	}
	if got := MergeFenceKey(102); got != "merge:in_progress:102" {
		t.Fatalf("MergeFenceKey = %q", got)
	}
	if got := CapabilityKey(101); got != "db:capability:zone:101" {
		t.Fatalf("CapabilityKey = %q", got)
	}
}

func TestStoreDBNameRoundTrip(t *testing.T) {
	cases := []struct {
		id   uint32
		name string
		ok   bool
	}{
		{0, "", false},
		{1, "zone_1_db", true},
		{102, "zone_102_db", true},
		{MaxZoneStorageID, "zone_999999_db", true},
		{FirstNonZoneStorageID, "player_store_1000000_db", true},
	}
	for _, c := range cases {
		name, ok := StoreDBName(c.id)
		if name != c.name || ok != c.ok {
			t.Fatalf("StoreDBName(%d) = (%q,%v), want (%q,%v)", c.id, name, ok, c.name, c.ok)
		}
		if !ok {
			continue
		}
		back, ok := StorageIDFromDBName(name)
		if !ok || back != c.id {
			t.Fatalf("StorageIDFromDBName(%q) = (%d,%v), want (%d,true)", name, back, ok, c.id)
		}
	}
}

func TestStorageIDFromDBNameRejectsOutsideFamilies(t *testing.T) {
	for _, name := range []string{
		"", "zone__db", "zone_0_db", "zone_01_db", "zone_1000000_db", "player_store_5_db",
		"player_store__db", "mmorpg_guild", "zone_1_dbx", "xzone_1_db", "zone_-1_db", "zone_1 _db",
	} {
		if id, ok := StorageIDFromDBName(name); ok {
			t.Fatalf("StorageIDFromDBName(%q) = %d, want rejection", name, id)
		}
	}
}

func TestValidateRejectsStableWithRunID(t *testing.T) {
	if err := (Record{StorageID: 1, Version: 1, RunID: "x"}).Validate(); !errors.Is(err, ErrMalformed) {
		t.Fatalf("stable record with run_id must be malformed, got %v", err)
	}
}

func TestSameRouteIgnoresFrozen(t *testing.T) {
	stable := Record{StorageID: 102, Version: 3}
	frozen := Record{StorageID: 102, Version: 3, Frozen: true, RunID: "r1"}
	if !stable.SameRoute(frozen) {
		t.Fatal("freezing must not change the route")
	}
	if stable.SameRoute(Record{StorageID: 1000000, Version: 4}) {
		t.Fatal("a switched record must not be the same route")
	}
}

func TestParseHomeZone(t *testing.T) {
	if z, ok, err := ParseHomeZone(""); z != 0 || ok || err != nil {
		t.Fatalf("empty = (%d,%v,%v)", z, ok, err)
	}
	if z, ok, err := ParseHomeZone("101"); z != 101 || !ok || err != nil {
		t.Fatalf("101 = (%d,%v,%v)", z, ok, err)
	}
	for _, raw := range []string{"0", "-1", "abc", "4294967296"} {
		if _, _, err := ParseHomeZone(raw); !errors.Is(err, ErrMalformed) {
			t.Fatalf("ParseHomeZone(%q) error = %v, want ErrMalformed", raw, err)
		}
	}
}

func TestEffectiveStorage(t *testing.T) {
	rec := Record{StorageID: 102, Version: 1}
	if got := EffectiveStorage(rec, true, 101, true, 7); got != 102 {
		t.Fatalf("record must win, got %d", got)
	}
	if got := EffectiveStorage(Record{}, false, 101, true, 7); got != 101 {
		t.Fatalf("absent record must fall back to home_zone, got %d", got)
	}
	if got := EffectiveStorage(Record{}, false, 0, false, 7); got != 7 {
		t.Fatalf("no record and no home must fall back to the processing zone, got %d", got)
	}
}
