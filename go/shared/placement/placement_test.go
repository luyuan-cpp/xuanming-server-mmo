package placement

import (
	"testing"
)

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

// 地域存档库的编号(总体方案 §7.2):地域 0 的 0 号库就是 DefaultGlobalStorageID,库名走 StoreDBName 的非 zone 家族。
func TestRegionStorageIDLayout(t *testing.T) {
	cases := []struct {
		region, shard uint32
		want          uint32
		name          string
	}{
		{0, 0, DefaultGlobalStorageID, "player_store_1000000_db"},
		{1, 7, 1001007, "player_store_1001007_db"},
		{MaxRegionCode, RegionStorageStride - 1, 1063999, "player_store_1063999_db"},
	}
	for _, c := range cases {
		got, err := RegionStorageID(c.region, c.shard)
		if err != nil || got != c.want {
			t.Fatalf("RegionStorageID(%d,%d) = (%d,%v), want %d", c.region, c.shard, got, err, c.want)
		}
		if name, ok := StoreDBName(got); !ok || name != c.name {
			t.Fatalf("StoreDBName(%d) = (%q,%v), want %q", got, name, ok, c.name)
		}
		if IsZoneStorage(got) {
			t.Fatalf("region storage %d must not fall into the zone family", got)
		}
	}
	for _, c := range []struct{ region, shard uint32 }{{MaxRegionCode + 1, 0}, {0, RegionStorageStride}} {
		if id, err := RegionStorageID(c.region, c.shard); err == nil {
			t.Fatalf("RegionStorageID(%d,%d) = %d, want an error", c.region, c.shard, id)
		}
	}
}

// 建号选库按 player_id 取模,落在本地域的 K 个库里;K 为 0、超过上限、玩家号为 0 都拒绝,不落到意料之外的库。
func TestRegionStorageIDForPlayer(t *testing.T) {
	const region, shards = uint32(2), uint32(64)
	seen := map[uint32]bool{}
	for playerID := uint64(1); playerID <= 4096; playerID++ {
		id, err := RegionStorageIDForPlayer(region, shards, playerID)
		if err != nil {
			t.Fatalf("RegionStorageIDForPlayer(%d) failed: %v", playerID, err)
		}
		want, _ := RegionStorageID(region, uint32(playerID%uint64(shards)))
		if id != want {
			t.Fatalf("RegionStorageIDForPlayer(%d) = %d, want %d", playerID, id, want)
		}
		seen[id] = true
	}
	if len(seen) != int(shards) {
		t.Fatalf("4096 consecutive ids hit %d storages, want all %d", len(seen), shards)
	}
	if id, err := RegionStorageIDForPlayer(0, 1, 123456789); err != nil || id != DefaultGlobalStorageID {
		t.Fatalf("dev K=1 = (%d,%v), want %d", id, err, DefaultGlobalStorageID)
	}
	for name, c := range map[string]struct {
		region, shards uint32
		player         uint64
	}{
		"no shards":       {0, 0, 1},
		"too many shards": {0, RegionStorageStride + 1, 1},
		"region too big":  {MaxRegionCode + 1, 1, 1},
		"player 0":        {0, 1, 0},
	} {
		if id, err := RegionStorageIDForPlayer(c.region, c.shards, c.player); err == nil {
			t.Fatalf("%s: got %d, want an error", name, id)
		}
	}
}

func TestRegionStorageDBNames(t *testing.T) {
	names, err := RegionStorageDBNames(1, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"player_store_1001000_db", "player_store_1001001_db", "player_store_1001002_db"}
	if len(names) != len(want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("names = %v, want %v", names, want)
		}
		if id, ok := StorageIDFromDBName(names[i]); !ok || id != 1001000+uint32(i) {
			t.Fatalf("StorageIDFromDBName(%q) = (%d,%v)", names[i], id, ok)
		}
	}
	if _, err := RegionStorageDBNames(0, 0); err == nil {
		t.Fatal("K=0 must be rejected")
	}
}
