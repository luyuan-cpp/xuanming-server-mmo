package shardowner

import (
	"fmt"
	"slices"
	"testing"

	"shared/releasetrack"
)

func memberNames(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("uuid-%02d", i)
	}
	return out
}

// 每个分片恰好一个负责者,而且是成员之一;所有成员各自的 Owned 合起来不重不漏地覆盖 64 片。
func TestOwnedPartitionsAllShards(t *testing.T) {
	members := memberNames(5)
	seen := make(map[uint32]string, ShardCount)
	for _, m := range members {
		owned := Owned(m, members)
		if !slices.IsSorted(owned) {
			t.Fatalf("Owned(%s) not ascending: %v", m, owned)
		}
		for _, shard := range owned {
			if prev, dup := seen[shard]; dup {
				t.Fatalf("shard %d owned by both %s and %s", shard, prev, m)
			}
			seen[shard] = m
		}
	}
	if len(seen) != ShardCount {
		t.Fatalf("shards covered = %d, want %d", len(seen), ShardCount)
	}
	for shard := uint32(0); shard < ShardCount; shard++ {
		if Owner(shard, members) != seen[shard] {
			t.Fatalf("Owner(%d) disagrees with Owned", shard)
		}
	}
}

// 结果只取决于成员集合,与顺序无关;不在成员表里的进程、空 self 不负责任何分片。
func TestOwnedIgnoresOrderAndNonMembers(t *testing.T) {
	members := memberNames(4)
	reversed := slices.Clone(members)
	slices.Reverse(reversed)
	for _, m := range members {
		if !slices.Equal(Owned(m, members), Owned(m, reversed)) {
			t.Fatalf("Owned(%s) depends on member order", m)
		}
	}
	if got := Owned("not-registered", members); len(got) != 0 {
		t.Fatalf("non-member owns %v", got)
	}
	if got := Owned("", members); len(got) != 0 {
		t.Fatalf("empty self owns %v", got)
	}
	if got := Owned("solo", []string{"solo"}); len(got) != ShardCount {
		t.Fatalf("single member owns %d shards, want %d", len(got), ShardCount)
	}
}

// 加一个副本时,只有改归新副本的分片换主,其余分片的负责者不变(HRW 的最小搬迁)。
func TestAddingMemberOnlyMovesShardsToIt(t *testing.T) {
	before := memberNames(4)
	after := append(slices.Clone(before), "uuid-new")
	moved := 0
	for shard := uint32(0); shard < ShardCount; shard++ {
		was, now := Owner(shard, before), Owner(shard, after)
		if was == now {
			continue
		}
		if now != "uuid-new" {
			t.Fatalf("shard %d moved from %s to %s, not to the new member", shard, was, now)
		}
		moved++
	}
	if moved == 0 || moved == ShardCount {
		t.Fatalf("moved %d shards: want some but not all", moved)
	}
}

func registration(uuid, track string) []byte {
	if track == "" {
		return []byte(fmt.Sprintf(`{"nodeId":1,"nodeType":30,"nodeUuid":%q,"zoneId":1}`, uuid))
	}
	return []byte(fmt.Sprintf(`{"nodeId":1,"nodeType":30,"nodeUuid":%q,"zoneId":1,"releaseTrack":%q}`, uuid, track))
}

const (
	keyA = "PlayerInboxNodeService.rpc/zone/1/node_type/30/node_id/1"
	keyB = "PlayerInboxNodeService.rpc/zone/1/node_type/30/node_id/2"
	keyC = "PlayerInboxNodeService.rpc/zone/1/node_type/30/node_id/3"
)

// 成员表只收 stable(含没写轨道的旧注册值);canary、认不出的轨道、没有 uuid、解析不了的值和占位键都不算。
func TestMemberOfAcceptsOnlyStableRegistrations(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		value []byte
		want  string
	}{
		{"stable", keyA, registration("a", "stable"), "a"},
		{"no track is stable", keyA, registration("a", ""), "a"},
		{"canary", keyA, registration("a", "canary"), ""},
		{"unknown track", keyA, registration("a", "beta"), ""},
		{"no uuid", keyA, []byte(`{"nodeId":1,"releaseTrack":"stable"}`), ""},
		{"not json", keyA, []byte("not-json"), ""},
		{"allocation key", "PlayerInboxNodeService.rpc/allocated/node_type/30/node_id/1", []byte("a"), ""},
	}
	for _, c := range cases {
		got, ok := memberOf([]byte(c.key), c.value)
		if (c.want != "") != ok || got != c.want {
			t.Errorf("%s: memberOf = (%q, %v), want %q", c.name, got, ok, c.want)
		}
	}
}

func newTestTracker(self string, track releasetrack.Track) *Tracker {
	return &Tracker{prefix: "PlayerInboxNodeService.rpc/", self: func() string { return self }, selfCanary: track.IsCanary(),
		members: map[string]string{}}
}

// 第一次全量读之前、或本进程还没注册时不就绪;就绪后按成员表给出本副本的分片,事件增删随之生效。
func TestTrackerOwnedShardsFollowsMembership(t *testing.T) {
	tr := newTestTracker("a", releasetrack.Stable)
	if _, ready := tr.OwnedShards(); ready {
		t.Fatal("ready before the first full listing")
	}
	tr.synced = true
	tr.applyPut([]byte(keyA), registration("a", "stable"))
	if got, ready := tr.OwnedShards(); !ready || len(got) != ShardCount {
		t.Fatalf("alone: owned %d shards (ready=%v), want all %d", len(got), ready, ShardCount)
	}

	tr.applyPut([]byte(keyB), registration("b", "stable"))
	tr.applyPut([]byte(keyC), registration("c", "canary")) // canary 不分走分片
	want := Owned("a", []string{"a", "b"})
	if got, _ := tr.OwnedShards(); !slices.Equal(got, want) {
		t.Fatalf("with b and a canary c: owned %v, want %v", got, want)
	}

	tr.applyDelete([]byte(keyB))
	if got, _ := tr.OwnedShards(); len(got) != ShardCount {
		t.Fatalf("after b left: owned %d shards, want all %d", len(got), ShardCount)
	}

	// 同一个 key 的值改成 canary(重新部署成金丝雀)= 摘掉。
	tr.applyPut([]byte(keyA), registration("a", "canary"))
	if got, ready := tr.OwnedShards(); !ready || len(got) != 0 {
		t.Fatalf("self no longer a stable member: owned %v (ready=%v), want none", got, ready)
	}

	unregistered := newTestTracker("", releasetrack.Stable)
	unregistered.synced = true
	if _, ready := unregistered.OwnedShards(); ready {
		t.Fatal("ready before this process registered")
	}
}

// canary 进程不负责任何分片,而且不用等成员表就绪。
func TestCanaryTrackerOwnsNothing(t *testing.T) {
	tr := newTestTracker("a", releasetrack.Canary)
	tr.synced = true
	tr.applyPut([]byte(keyA), registration("a", "stable"))
	if got, ready := tr.OwnedShards(); !ready || len(got) != 0 {
		t.Fatalf("canary owns %v (ready=%v), want none and ready", got, ready)
	}
}
