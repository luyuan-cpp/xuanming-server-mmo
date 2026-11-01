package dirshard

import (
	"hash/crc32"
	"reflect"
	"testing"
)

func TestValidateAcceptsOnlyDivisorsOf64(t *testing.T) {
	for k := -1; k <= 128; k++ {
		want := false
		switch k {
		case 1, 2, 4, 8, 16, 32, 64:
			want = true
		}
		if got := Validate(k) == nil; got != want {
			t.Fatalf("Validate(%d) ok = %t, want %t", k, got, want)
		}
	}
}

func TestNormalizeTreatsOnlyZeroAsUnset(t *testing.T) {
	if Normalize(0) != 1 || Normalize(4) != 4 || Normalize(-1) != -1 {
		t.Fatal("Normalize 只把 0 换成 1")
	}
}

// 金样:这些值上线后不能变,变了就是把已有的行路由到别的库。crc32 用 IEEE 多项式,先对 64 取模再对 K 取模。
// 期望值由独立实现(Python zlib.crc32)算出,不是从本包抄的。
func TestSlotGoldenValues(t *testing.T) {
	for _, tc := range []struct {
		key   string
		crc   uint32
		slot  int
		slots map[int]int // K → 片
	}{
		{key: "a", crc: 3904355907, slot: 3, slots: map[int]int{1: 0, 2: 1, 4: 3, 8: 3, 16: 3, 32: 3, 64: 3}},
		{key: "Account", crc: 2995482424, slot: 56, slots: map[int]int{1: 0, 2: 0, 4: 0, 8: 0, 16: 8, 32: 24, 64: 56}},
		{key: "seller", crc: 4212839420, slot: 60, slots: map[int]int{1: 0, 2: 0, 4: 0, 8: 4, 16: 12, 32: 28, 64: 60}},
		{key: "buyer", crc: 2224054195, slot: 51, slots: map[int]int{1: 0, 2: 1, 4: 3, 8: 3, 16: 3, 32: 19, 64: 51}},
		{key: "yunzhongjun", crc: 733180585, slot: 41, slots: map[int]int{1: 0, 2: 1, 4: 1, 8: 1, 16: 9, 32: 9, 64: 41}},
		{key: "账号", crc: 2158321825, slot: 33, slots: map[int]int{1: 0, 2: 1, 4: 1, 8: 1, 16: 1, 32: 1, 64: 33}},
		{key: "云中君", crc: 613186218, slot: 42, slots: map[int]int{1: 0, 2: 0, 4: 2, 8: 2, 16: 10, 32: 10, 64: 42}},
	} {
		if got := crc32.ChecksumIEEE([]byte(tc.key)); got != tc.crc {
			t.Fatalf("crc32(%q) = %d, want %d", tc.key, got, tc.crc)
		}
		if got := SlotOfAccount(tc.key, 64); got != tc.slot {
			t.Fatalf("SlotOfAccount(%q, 64) = %d, want %d", tc.key, got, tc.slot)
		}
		for k, want := range tc.slots {
			if got := SlotOfAccount(tc.key, k); got != want {
				t.Fatalf("SlotOfAccount(%q, %d) = %d, want %d", tc.key, k, got, want)
			}
			if got := SlotOfName(tc.key, k); got != want {
				t.Fatalf("SlotOfName(%q, %d) = %d, want %d", tc.key, k, got, want)
			}
		}
	}
}

func TestSlotOfPlayerGoldenValues(t *testing.T) {
	for _, tc := range []struct {
		id    uint64
		slots map[int]int
	}{
		{id: 1, slots: map[int]int{1: 0, 2: 1, 4: 1, 8: 1, 16: 1, 32: 1, 64: 1}},
		{id: 63, slots: map[int]int{1: 0, 2: 1, 4: 3, 8: 7, 16: 15, 32: 31, 64: 63}},
		{id: 64, slots: map[int]int{1: 0, 2: 0, 4: 0, 8: 0, 16: 0, 32: 0, 64: 0}},
		{id: 4242, slots: map[int]int{1: 0, 2: 0, 4: 2, 8: 2, 16: 2, 32: 18, 64: 18}},
		{id: 1<<63 - 1, slots: map[int]int{1: 0, 2: 1, 4: 3, 8: 7, 16: 15, 32: 31, 64: 63}},
		{id: 9000000000000000001, slots: map[int]int{1: 0, 2: 1, 4: 1, 8: 1, 16: 1, 32: 1, 64: 1}},
	} {
		for k, want := range tc.slots {
			if got := SlotOfPlayer(tc.id, k); got != want {
				t.Fatalf("SlotOfPlayer(%d, %d) = %d, want %d", tc.id, k, got, want)
			}
		}
	}
}

// 账号按原字节算:不 trim、不 casefold,和目录的比较口径相同。
func TestSlotOfAccountIsByteExact(t *testing.T) {
	if SlotOfAccount("Account", 64) == SlotOfAccount("account", 64) {
		t.Fatal("大小写不同的账号落到了同一个槽,这条金样要和 crc32 一起核")
	}
}

func TestDirectoryDBName(t *testing.T) {
	if got := DirectoryDBName("mmorpg_global", 0, 1); got != "mmorpg_global" {
		t.Fatalf("K=1 就是基名,got %q", got)
	}
	if got := DirectoryDBName("mmorpg_global", 0, 4); got != "mmorpg_global_00" {
		t.Fatalf("got %q", got)
	}
	if got := DirectoryDBName("mmorpg_global", 63, 64); got != "mmorpg_global_63" {
		t.Fatalf("got %q", got)
	}
	want := []string{"g_00", "g_01", "g_02", "g_03"}
	if got := DirectoryDBNames("g", 4); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if got := DirectoryDBNames("g", 1); !reflect.DeepEqual(got, []string{"g"}) {
		t.Fatalf("got %v", got)
	}
	if got := LedgerDBName("mmorpg_global"); got != "mmorpg_global_ledger" {
		t.Fatalf("got %q", got)
	}
}
