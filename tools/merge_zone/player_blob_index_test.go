package main

import (
	"sort"
	"strings"
	"testing"
)

// TestPlayerFieldIndexKeyMatchesDataService:拷 blob 前删的目标端字段索引必须与 data_service 的键逐字节相同
// (go/data_service/internal/logic/data_logic.go::fieldIndexKey),并落在同一个 {id} 前缀下。本 module 引不到
// data_service,两边各自钉同一个字面量:这里与 data_service 的 TestFieldIndexKeyLiteralPinnedForMergeZone 成对,改一边必须同批改另一边。
// 键名一漂移,DEL 删的是另一个键,目标端残留索引的戳与拷过去的 __version 巧合相等时,拷过去的字段从全量读里消失。
func TestPlayerFieldIndexKeyMatchesDataService(t *testing.T) {
	if got := playerFieldIndexKey(42); got != "player:{42}:__fields" {
		t.Fatalf("got %q", got)
	}
	if !strings.HasPrefix(playerFieldIndexKey(42), "player:{42}:") {
		t.Fatal("field index key left the player's hash-tag prefix")
	}
}

// TestReservedPlayerKeySuffixesMatchSharedRedisslot:镜像表与 go/shared/redisslot 的 TestReservedPlayerKeySuffixes
// 钉同一份排序后的字面量。漏一个后缀,合服拷 blob 时就会把那把玩家域键(string)当字段拷到目标端。
func TestReservedPlayerKeySuffixesMatchSharedRedisslot(t *testing.T) {
	got := make([]string, 0, len(reservedPlayerKeySuffixes))
	for s := range reservedPlayerKeySuffixes {
		got = append(got, s)
	}
	sort.Strings(got)
	want := []string{
		"asset_sql_commit", "channel_switch_cooldown", "handoff", "leave_release_wait", "leave_tomb", "location",
		"location_revision", "location_revision_floor", "login_generation", "owner_epoch", "save_barrier", "sync_digest",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("reserved suffixes = %v, want %v", got, want)
	}
	if !isReservedPlayerKey(42, "player:{42}:owner_epoch") || isReservedPlayerKey(42, "player:{42}:bag") ||
		isReservedPlayerKey(42, "player:{43}:owner_epoch") || isReservedPlayerKey(42, "player:{42}:__version") {
		t.Fatal("isReservedPlayerKey misclassified")
	}
}
