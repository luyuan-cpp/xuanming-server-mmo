package redisslot

import (
	"crypto/sha256"
	"encoding/hex"
	"hash/fnv"
	"sort"
	"strings"
	"testing"
)

// 金样向量由独立实现(Python,按 Redis Cluster 规范)算出,C++ redis_slot_test.cpp 钉同一组值。
// 改动任何一个值都意味着跨语言契约变了:两侧必须同批改,且上线后禁止改(会让存量键换槽)。

func TestCRC16GoldenVector(t *testing.T) {
	if got := CRC16("123456789"); got != 0x31C3 {
		t.Fatalf("CRC16(123456789) = %#x, want 0x31c3", got)
	}
}

func TestHashSlotGoldenVectors(t *testing.T) {
	cases := map[string]uint16{
		"123456789":               12739,
		"{user1000}.following":    3443,
		"foo{}{bar}":              8363, // 空 tag:整键参与哈希
		"foo{{bar}}zap":           4015, // tag 是 "{bar"
		"foo{bar}{zap}":           5061, // 只取第一个 tag
		"PlayerAllData:{42}":      8000,
		"player:{42}:owner_epoch": 8000,
		"42":                      8000,
		"1":                       9842,
		"18446744073709551615":    5255,
	}
	for key, want := range cases {
		if got := HashSlot(key); got != want {
			t.Errorf("HashSlot(%q) = %d, want %d", key, got, want)
		}
	}
}

func TestPlayerSlotAndTag(t *testing.T) {
	if got := PlayerSlot(42); got != 8000 {
		t.Fatalf("PlayerSlot(42) = %d, want 8000", got)
	}
	if got := PlayerSlot(18446744073709551615); got != 5255 {
		t.Fatalf("PlayerSlot(max) = %d, want 5255", got)
	}
	if got := PlayerTag(42); got != "{42}" {
		t.Fatalf("PlayerTag(42) = %q", got)
	}
	// 玩家键与同槽索引键必须同槽:这是补投索引与存盘屏障能放进同一段 Lua 的前提。
	for _, pid := range []uint64{1, 42, 1000003, 18446744073709551615} {
		playerKey := "player:" + PlayerTag(pid) + ":owner_epoch"
		indexKey := "player:save_pending:" + PlayerIndexTag(pid)
		if HashSlot(playerKey) != HashSlot(indexKey) {
			t.Errorf("pid %d: player key slot %d != index key slot %d", pid, HashSlot(playerKey), HashSlot(indexKey))
		}
	}
}

func TestSlotTagGoldenVectors(t *testing.T) {
	cases := map[uint16]string{0: "afps", 1: "emj", 2: "ahwf", 100: "aotc", 12739: "aarc", 16383: "hia"}
	for slot, want := range cases {
		if got := SlotTag(slot); got != want {
			t.Errorf("SlotTag(%d) = %q, want %q", slot, got, want)
		}
	}
}

func TestSlotTagTableCoversEverySlot(t *testing.T) {
	var b strings.Builder
	for s := 0; s < SlotCount; s++ {
		tag := SlotTag(uint16(s))
		if tag == "" || len(tag) > slotTagMaxLen || strings.Trim(tag, slotTagAlphabet) != "" {
			t.Fatalf("SlotTag(%d) = %q: 形状不合法", s, tag)
		}
		if got := HashSlot("{" + tag + "}"); got != uint16(s) {
			t.Fatalf("SlotTag(%d) = %q 哈希到 %d", s, tag, got)
		}
		if s > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(tag)
	}
	sum := sha256.Sum256([]byte(b.String()))
	const want = "b564523fd7c87f294eab3efd4b09e171775388ea14790138c1d4bb988df61d4c"
	if got := hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("slot-tag 表 SHA-256 = %s, want %s", got, want)
	}
	// C++ 侧测试工程不链 SHA-256,用 FNV-1a 64 钉同一张表;两个摘要必须同时成立。
	h := fnv.New64a()
	_, _ = h.Write([]byte(b.String()))
	if got := h.Sum64(); got != 0xf4a7bdc8d20a28e5 || b.Len() != 72467 {
		t.Fatalf("slot-tag 表 FNV-1a64 = %#x len=%d, want 0xf4a7bdc8d20a28e5 len=72467", got, b.Len())
	}
}

func TestHasHashTag(t *testing.T) {
	yes := []string{"PlayerAllData:{42}", "player:save_pending:{afps}", "a{b}c"}
	no := []string{"PlayerAllData:42", "foo{}bar", "foo{bar", "plain"}
	for _, k := range yes {
		if !HasHashTag(k) {
			t.Errorf("HasHashTag(%q) = false", k)
		}
	}
	for _, k := range no {
		if HasHashTag(k) {
			t.Errorf("HasHashTag(%q) = true", k)
		}
	}
}

func TestSlotTagOutOfRangePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("SlotTag(16384) 未 panic")
		}
	}()
	SlotTag(SlotCount)
}

func TestReservedPlayerKeySuffixes(t *testing.T) {
	got := ReservedPlayerKeySuffixes()
	sort.Strings(got)
	want := []string{
		"asset_sql_commit", "channel_switch_cooldown", "handoff", "leave_release_wait", "leave_tomb", "location",
		"location_revision", "location_revision_floor", "login_generation", "owner_epoch", "save_barrier", "sync_digest",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("保留后缀表 = %v, want %v(新增 player:{<id>}: 键必须登记,删除须同步 data_service)", got, want)
	}
	if !IsReservedPlayerKeySuffix("save_barrier") || IsReservedPlayerKeySuffix("hp") {
		t.Fatal("IsReservedPlayerKeySuffix 判定错误")
	}
}
