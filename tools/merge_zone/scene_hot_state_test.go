package main

// merge_zone 的 go.mod 里没有 miniredis(也没有 vendor),按约定不为了测试
// 引新依赖;这里只测纯规划 / 解码函数,Redis 执行层靠 dry-run 在真实环境验证。

import (
	"context"
	"testing"
)

// encodePlayerLocation 是测试侧的 protobuf 编码器,与 storage.proto 字段号一致。
func encodePlayerLocation(loc playerLocation, extraUnknown bool) []byte {
	var b []byte
	putVarint := func(v uint64) {
		for v >= 0x80 {
			b = append(b, byte(v)|0x80)
			v >>= 7
		}
		b = append(b, byte(v))
	}
	if loc.SceneID != 0 {
		putVarint(1<<3 | 0)
		putVarint(loc.SceneID)
	}
	if loc.NodeID != "" {
		putVarint(2<<3 | 2)
		putVarint(uint64(len(loc.NodeID)))
		b = append(b, loc.NodeID...)
	}
	if loc.UpdateTime != 0 {
		putVarint(3<<3 | 0)
		putVarint(loc.UpdateTime)
	}
	if loc.ZoneID != 0 {
		putVarint(4<<3 | 0)
		putVarint(uint64(loc.ZoneID))
	}
	if extraUnknown {
		// 未来追加的字段 9(fixed64)与字段 10(bytes):解码器必须跳过而不是报错。
		putVarint(9<<3 | 1)
		b = append(b, 1, 2, 3, 4, 5, 6, 7, 8)
		putVarint(10<<3 | 2)
		putVarint(3)
		b = append(b, 'x', 'y', 'z')
	}
	return b
}

func TestDecodePlayerLocation_RoundTrip(t *testing.T) {
	want := playerLocation{SceneID: 1234567890123, NodeID: "10", UpdateTime: 1757000000, ZoneID: 102}
	got, err := decodePlayerLocation(encodePlayerLocation(want, false))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestDecodePlayerLocation_SkipsUnknownFields(t *testing.T) {
	want := playerLocation{SceneID: 7, NodeID: "n", ZoneID: 3}
	got, err := decodePlayerLocation(encodePlayerLocation(want, true))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestDecodePlayerLocation_Truncated(t *testing.T) {
	raw := encodePlayerLocation(playerLocation{SceneID: 7, NodeID: "node-10", ZoneID: 3}, false)
	if _, err := decodePlayerLocation(raw[:len(raw)-3]); err == nil {
		t.Fatal("expected error on truncated buffer")
	}
	if _, err := decodePlayerLocation([]byte("not-a-player-location")); err == nil {
		t.Fatal("expected error on garbage")
	}
}

func TestDecideLocationZone(t *testing.T) {
	cases := []struct {
		name      string
		loc       playerLocation
		sceneZone string
		want      locationDecision
	}{
		{"zone matches", playerLocation{ZoneID: 102, SceneID: 1}, "", locationDelete},
		{"zone differs", playerLocation{ZoneID: 101, SceneID: 1}, "", locationKeep},
		{"legacy zone0 resolved to source", playerLocation{ZoneID: 0, SceneID: 1}, "102", locationDelete},
		{"legacy zone0 resolved elsewhere", playerLocation{ZoneID: 0, SceneID: 1}, "101", locationKeep},
		{"legacy zone0 scene mapping gone", playerLocation{ZoneID: 0, SceneID: 1}, "", locationUndecided},
		{"legacy zone0 no scene", playerLocation{ZoneID: 0, SceneID: 0}, "", locationUndecided},
		{"legacy zone0 garbage mapping", playerLocation{ZoneID: 0, SceneID: 1}, "abc", locationUndecided},
	}
	for _, c := range cases {
		if got := decideLocationZone(c.loc, 102, c.sceneZone); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestSceneHotStateKeys(t *testing.T) {
	keys := sceneHotStateKeys(42)
	if len(keys) != hotStateSceneKeysPerScene {
		t.Fatalf("got %d keys, want %d", len(keys), hotStateSceneKeysPerScene)
	}
	want := map[string]bool{
		"scene:42:zone": true, "scene:42:node": true, "scene:42:mirror": true,
		"scene:42:source": true, "scene:42:mirrors": true, "instance:42:player_count": true,
	}
	for _, k := range keys {
		if !want[k] {
			t.Errorf("unexpected key %q", k)
		}
		delete(want, k)
	}
	if len(want) != 0 {
		t.Errorf("missing keys %v", want)
	}
}

func TestParseSceneIDFromZoneKey(t *testing.T) {
	if id, ok := parseSceneIDFromZoneKey("scene:987654321:zone"); !ok || id != 987654321 {
		t.Fatalf("got %d %v", id, ok)
	}
	for _, bad := range []string{"scene:0:zone", "scene:abc:zone", "scene:1:node", "player:1:location", "scene::zone"} {
		if _, ok := parseSceneIDFromZoneKey(bad); ok {
			t.Errorf("%q should not parse", bad)
		}
	}
}

func TestParsePlayerIDFromLocationKey(t *testing.T) {
	if id, ok := parsePlayerIDFromLocationKey("player:5102:location"); !ok || id != 5102 {
		t.Fatalf("got %d %v", id, ok)
	}
	for _, bad := range []string{"player:{5102}:transform", "player:zone:5102", "player:x:location", "player:0:location"} {
		if _, ok := parsePlayerIDFromLocationKey(bad); ok {
			t.Errorf("%q should not parse", bad)
		}
	}
}

func TestSourceZonePatternsAndFixedKeys(t *testing.T) {
	pats := sourceZoneScanPatterns(102)
	wantPats := []string{
		"world_channels:zone:102:*",
		"world_channels:draining:zone:102:*",
		"world_channels:cooldown:zone:102:*",
		"node:zone:102:*",
	}
	if len(pats) != len(wantPats) {
		t.Fatalf("got %v", pats)
	}
	for i := range pats {
		if pats[i] != wantPats[i] {
			t.Errorf("pattern %d: got %q want %q", i, pats[i], wantPats[i])
		}
	}
	fixed := sourceZoneFixedKeys(102)
	if len(fixed) != 2 || fixed[0] != "world_channels:desired:zone:102" || fixed[1] != "scene_nodes:zone:102:load" {
		t.Fatalf("got %v", fixed)
	}
	// 目标 zone 的键绝不能被任何模式命中(前缀含 zone 号 + 冒号,101 与 1010 也不互撞)。
	for _, p := range pats {
		if p == "node:zone:1020:*" || p == "world_channels:zone:1020:*" {
			t.Errorf("pattern %q leaks into other zones", p)
		}
	}
}

func TestBackfillPlanBatch(t *testing.T) {
	var rep backfillReport
	ids := []uint64{1, 2, 3, 4}
	existing := []string{"", "7", "9", ""}
	got := backfillPlanBatch(ids, existing, "7", &rep)
	if len(got) != 2 || got[0] != 1 || got[1] != 4 {
		t.Fatalf("toCreate=%v", got)
	}
	if rep.WouldCreate != 2 || rep.AlreadyMapped != 1 || rep.Mismatched != 1 {
		t.Fatalf("report %+v", rep)
	}
}

func TestSliceSourceKeysetPaging(t *testing.T) {
	src := sliceSource{1, 2, 3, 5, 8, 13}
	ctx := context.Background()
	var after uint64
	var seen []uint64
	for {
		batch, err := src.NextBatch(ctx, after, 4)
		if err != nil {
			t.Fatal(err)
		}
		if len(batch) == 0 {
			break
		}
		seen = append(seen, batch...)
		after = batch[len(batch)-1]
	}
	if len(seen) != len(src) {
		t.Fatalf("seen %v", seen)
	}
	for i := range src {
		if seen[i] != src[i] {
			t.Fatalf("seen %v want %v", seen, src)
		}
	}
}

func TestZoneDBName(t *testing.T) {
	if zoneDBName(102) != "zone_102_db" {
		t.Fatal(zoneDBName(102))
	}
}
