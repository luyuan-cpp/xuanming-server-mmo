package main

// 纯函数单测。Redis / MySQL 相关的行为在 integration_test.go(build tag
// merge_integration)里对着真实的 127.0.0.1 实例跑。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// ── Kafka lag 解析 ────────────────────────────────────────────

const sampleConsumerGroupOutput = `
GROUP                  TOPIC             PARTITION  CURRENT-OFFSET  LOG-END-OFFSET  LAG  CONSUMER-ID  HOST  CLIENT-ID
db_rpc_consumer_group  db_task_zone_901  0          120             120             0    c-1          /1.2.3.4  x
db_rpc_consumer_group  db_task_zone_901  1          77              77              0    c-1          /1.2.3.4  x
db_rpc_consumer_group  db_task_zone_902  0          10              15              5    c-2          /1.2.3.5  y
`

func TestParseConsumerGroupLag_SumsOnlyRequestedTopic(t *testing.T) {
	lag, err := parseConsumerGroupLag(sampleConsumerGroupOutput, "db_task_zone_901")
	if err != nil {
		t.Fatal(err)
	}
	if lag != 0 {
		t.Fatalf("lag=%d want 0", lag)
	}
	lag, err = parseConsumerGroupLag(sampleConsumerGroupOutput, "db_task_zone_902")
	if err != nil {
		t.Fatal(err)
	}
	if lag != 5 {
		t.Fatalf("lag=%d want 5", lag)
	}
}

func TestParseConsumerGroupLag_UnknownTopicIsAnError(t *testing.T) {
	// 「这个 group 没消费过这个 topic」绝不能被当成 lag=0 放行。
	if _, err := parseConsumerGroupLag(sampleConsumerGroupOutput, "db_task_zone_999"); err == nil {
		t.Fatal("expected an error for a topic the group never consumed")
	}
}

func TestParseConsumerGroupLag_DashLagIsAnError(t *testing.T) {
	out := `GROUP TOPIC PARTITION CURRENT-OFFSET LOG-END-OFFSET LAG
g db_task_zone_901 0 - 120 -`
	if _, err := parseConsumerGroupLag(out, "db_task_zone_901"); err == nil {
		t.Fatal("expected an error when LAG is '-' (no committed offset)")
	}
}

func TestParseConsumerGroupLag_ColumnOrderIsReadFromHeader(t *testing.T) {
	// 列顺序被打乱(不同 Kafka 版本会插列):必须按表头名定位,不按下标。
	out := `LAG TOPIC GROUP PARTITION
3 db_task_zone_901 g 0
4 db_task_zone_901 g 1`
	lag, err := parseConsumerGroupLag(out, "db_task_zone_901")
	if err != nil {
		t.Fatal(err)
	}
	if lag != 7 {
		t.Fatalf("lag=%d want 7", lag)
	}
}

func TestDbTaskTopicAndQueueKeys(t *testing.T) {
	if got := dbTaskTopic(901, 1); got != "db_task_zone_901" {
		t.Fatalf("gen1 topic=%q", got)
	}
	if got := dbTaskTopic(901, 3); got != "db_task_zone_901_g3" {
		t.Fatalf("gen3 topic=%q", got)
	}
	if got := dbRetryQueueKey("db_task_zone_901"); got != "kafka:retry:queue:db_task_zone_901" {
		t.Fatalf("retry key=%q", got)
	}
	if got := dbRetryProcessingKey("t"); got != "kafka:retry:processing:t" {
		t.Fatalf("processing key=%q", got)
	}
	if got := dbDeadQueueKey("t"); got != "kafka:dead:queue:t" {
		t.Fatalf("dead key=%q", got)
	}
}

// ── 缓存键 ───────────────────────────────────────────────────

func TestPlayerCacheKeys_CoversParentSubAndDiscoveredTables(t *testing.T) {
	keys := playerCacheKeys(42, []string{"player_database", "player_database_1", "player_centre_database"})
	want := map[string]bool{
		"PlayerAllData:42":          true,
		"BagAllData:42":             true,
		"QuestAllData:42":           true,
		"MailAllData:42":            true,
		"player_database:42":        true,
		"player_database_1:42":      true,
		"player_centre_database:42": true,
	}
	if len(keys) != len(want) {
		t.Fatalf("got %d keys %v, want %d", len(keys), keys, len(want))
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

func TestPlayerCacheKeys_DeduplicatesTableAndMsgTypeOverlap(t *testing.T) {
	// 表名与 MsgType 同名是常态;重复的键会让 pipeline 白跑一次 DEL。
	keys := playerCacheKeys(7, []string{"PlayerAllData", "player_database"})
	seen := map[string]int{}
	for _, k := range keys {
		seen[k]++
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("key %q appears %d times", k, n)
		}
	}
}

// ── 批次 / 列表工具 ──────────────────────────────────────────

func TestChunkUint64(t *testing.T) {
	got := chunkUint64([]uint64{1, 2, 3, 4, 5}, 2)
	want := [][]uint64{{1, 2}, {3, 4}, {5}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if n := len(chunkUint64(nil, 10)); n != 0 {
		t.Fatalf("nil input produced %d chunks", n)
	}
	// size<=0 不能死循环。
	if n := len(chunkUint64([]uint64{1, 2}, 0)); n != 2 {
		t.Fatalf("size 0 produced %d chunks", n)
	}
}

func TestInListLiteral(t *testing.T) {
	if got := inListLiteral([]uint64{1, 20, 300}); got != "1,20,300" {
		t.Fatalf("got %q", got)
	}
	if got := inListLiteral(nil); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestSortedUint64_SortsAndDedups(t *testing.T) {
	got := sortedUint64([]uint64{5, 1, 5, 3, 1})
	want := []uint64{1, 3, 5}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if sortedUint64(nil) != nil {
		t.Fatal("nil should stay nil")
	}
}

// ── 清单 ─────────────────────────────────────────────────────

func TestManifestRoundTripAndStepFlags(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.json")
	now := time.Date(2026, 9, 8, 3, 0, 0, 0, time.UTC)
	m := newMergeManifest("run-1", 901, 902, "host/pid=1", now)
	m.PlayerIDs = []uint64{3, 1, 2}
	m.GuildIDs = []uint64{9}
	m.RankMembers = []manifestRankMember{{Member: "9", Score: 12.5}}
	m.Tables = []string{"player_database"}
	m.markStep(stepPlayerRows, "copied=3")

	if err := saveManifest(path, m); err != nil {
		t.Fatal(err)
	}
	got, err := loadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.stepDone(stepPlayerRows) {
		t.Error("player_rows should be marked done")
	}
	if got.stepDone(stepGuildRank) {
		t.Error("guild_rank should not be done")
	}
	if got.RunID != "run-1" || got.SourceZone != 901 || got.TargetZone != 902 {
		t.Errorf("identity lost: %+v", got)
	}
	if !reflect.DeepEqual(got.RankMembers, m.RankMembers) {
		t.Errorf("rank members lost: %v", got.RankMembers)
	}
	// 落盘必须是原子的 —— .tmp 不能留下。
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error(".tmp file left behind")
	}
}

func TestLoadManifest_MissingFileIsNotAnError(t *testing.T) {
	got, err := loadManifest(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || got != nil {
		t.Fatalf("got (%v, %v), want (nil, nil)", got, err)
	}
}

func TestLoadManifest_RejectsUnknownVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.json")
	raw, _ := json.Marshal(map[string]any{"version": manifestVersion + 1})
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadManifest(path); err == nil {
		t.Fatal("expected a version mismatch error rather than a guess")
	}
}

func TestValidateManifestForRun_RejectsWrongZones(t *testing.T) {
	m := newMergeManifest("r", 901, 902, "op", time.Now())
	if err := validateManifestForRun(m, 901, 902); err != nil {
		t.Fatalf("matching zones rejected: %v", err)
	}
	if err := validateManifestForRun(m, 903, 902); err == nil {
		t.Fatal("expected a mismatch error for a different source zone")
	}
	if err := validateManifestForRun(nil, 1, 2); err != nil {
		t.Fatalf("nil manifest should be fine: %v", err)
	}
}

func TestDefaultManifestPathShape(t *testing.T) {
	got := defaultManifestPath(901, 902, time.Date(2026, 9, 8, 3, 4, 5, 0, time.UTC))
	if got != "merge_901_to_902_20260908T030405Z.json" {
		t.Fatalf("got %q", got)
	}
}

// ── 围栏 ─────────────────────────────────────────────────────

func TestMergeFenceKeyAndValueShape(t *testing.T) {
	if got := mergeFenceKey(901); got != "merge:in_progress:901" {
		t.Fatalf("got %q", got)
	}
	v := mergeInProgressValue{
		Tool: "tools/merge_zone", RunID: "r1", SourceZone: 901, TargetZone: 902,
		StartedAt: "2026-09-08T03:00:00Z", StartedAtUnixMs: 1, ExpiresAtUnixMs: 2,
		Operator: "h/pid=1", ManifestPath: "m.json",
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	// 契约:读者只做 EXISTS,但 value 必须是可读 JSON 且带 run_id / started_at。
	for _, want := range []string{`"tool":"tools/merge_zone"`, `"run_id":"r1"`, `"started_at"`, `"source_zone":901`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("fence value missing %s: %s", want, raw)
		}
	}
	var back mergeInProgressValue
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back != v {
		t.Errorf("round trip changed the value")
	}
}

func TestNewRunIDIsUnique(t *testing.T) {
	now := time.Now()
	a := newRunID(1, 2, now)
	b := newRunID(1, 2, now)
	if a == b {
		t.Fatal("two runs started in the same second must still get different ids")
	}
}

// ── guild 键 ─────────────────────────────────────────────────

func TestGuildKeyShapesMatchGuildRepo(t *testing.T) {
	if got := guildCacheKey(7); got != "guild:v2:7" {
		t.Fatalf("got %q", got)
	}
	if got := guildCacheGenerationKey(7); got != "guild:v2:cache_generation:7" {
		t.Fatalf("got %q", got)
	}
	if got := guildZoneRankKey(902); got != "guild_rank:zone:902" {
		t.Fatalf("got %q", got)
	}
	if guildRankLockKey != "guild_rank:maintenance_lock" {
		t.Fatalf("lock key drifted from guild_repo.go: %q", guildRankLockKey)
	}
}

// ── 审计 INFRA 标记 ──────────────────────────────────────────

func TestInfraAuditIsBlockAndDetectable(t *testing.T) {
	r := infraAudit("x", "redis ping failed: %v", "timeout")
	if r.Severity != "block" {
		t.Errorf("infra findings must be block severity, got %q", r.Severity)
	}
	if !isInfraAudit(r) {
		t.Error("infra finding not detected — exit code would be 1 instead of 2")
	}
	if isInfraAudit(ResourceAudit{Name: "y", Severity: "block", Notes: "real problem"}) {
		t.Error("a genuine block finding must not be misread as infra")
	}
	if !strings.Contains(r.Notes, "timeout") {
		t.Errorf("infra note lost the cause: %q", r.Notes)
	}
}

// ── 数据锁键(item 10 的排除对象)────────────────────────────

func TestPlayerDataLockKeyMatchesRouter(t *testing.T) {
	// go/data_service/internal/routing/router.go::PlayerDataLockKey
	if got := playerDataLockKey(42); got != "player:{42}:__lock" {
		t.Fatalf("got %q", got)
	}
	// 它必须落在 scan pattern 内 —— 正因为如此才需要显式排除。
	if !strings.HasPrefix(playerDataLockKey(42), "player:{42}:") {
		t.Fatal("lock key no longer matches the scan pattern; the skip is now dead code")
	}
}

// ── 写意图 ───────────────────────────────────────────────────

func TestRequireWriteIntent(t *testing.T) {
	if err := requireWriteIntent(options{}); err == nil {
		t.Error("neither flag should be refused")
	}
	if err := requireWriteIntent(options{dryRun: true, apply: true}); err == nil {
		t.Error("both flags should be refused (ops would think it wrote)")
	}
	if err := requireWriteIntent(options{dryRun: true}); err != nil {
		t.Errorf("-dry-run alone: %v", err)
	}
	if err := requireWriteIntent(options{apply: true}); err != nil {
		t.Errorf("-apply alone: %v", err)
	}
}

// ── 默认 DB 号(item 3 的回归护栏)──────────────────────────

func TestRedisDBDefaultsMatchServiceYAML(t *testing.T) {
	// 这四个常量若漂移,合服会安静地扫错库。数值来源见 main.go 顶部注释。
	cases := []struct {
		name string
		got  int
		want int
	}{
		// go-zero RedisConf 没有 DB 字段 → mapping Redis 恒为 DB 0(见 main.go 注释)。
		{"mapping (data_service MappingRedis, go-zero 无 DB 字段)", defaultMappingRedisDB, 0},
		{"guild (guild.yaml RedisClient.DB)", defaultGuildRedisDB, 2},
		{"friend (friend.yaml RedisClient.DB)", defaultFriendRedisDB, 3},
		{"shared/login (login.yaml Node.RedisClient.DB)", defaultSharedRedisDB, 0},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %d want %d", c.name, c.got, c.want)
		}
	}
}
