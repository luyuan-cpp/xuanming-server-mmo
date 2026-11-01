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
