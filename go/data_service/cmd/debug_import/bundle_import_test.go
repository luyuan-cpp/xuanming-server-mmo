package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"data_service/cmd/debugutil"
	databasepb "proto/common/database"

	"shared/generated/persistschema"
	"shared/ownerepoch"
	"shared/savebarrier"
	"shared/scenenode"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"
)

const bundlePlayer = 288230376151711745 // 2^58+1:float64 表示不了,任何经 float64 的路径都会改号

func rawValue(t *testing.T, msg proto.Message) *debugutil.Value {
	t.Helper()
	raw, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	value := debugutil.RenderProto(raw, func() proto.Message { return msg.ProtoReflect().Type().New().Interface() }, nil)
	return &value
}

// testBundle 构造一份导出包:线上 owner_epoch=7,屏障 (6,50),SQL 两行围栏 (6,50),父快照 last_save_seq=60。
func testBundle(t *testing.T, withParent bool) *debugutil.Bundle {
	t.Helper()
	v := savebarrier.Version(6, 50)
	b := &debugutil.Bundle{
		Format: debugutil.BundleFormat, FormatVersion: debugutil.BundleFormatVersion, PlayerID: bundlePlayer,
		Redis: []debugutil.RedisEntry{
			{Key: ownerepoch.OwnerEpochKey(bundlePlayer), Type: "string", Value: &debugutil.Value{Kind: "text", Text: "7"}},
			{Key: savebarrier.Key(bundlePlayer), Type: "hash", Hash: map[string]debugutil.Value{
				"pending": {Kind: "text", Text: v}, "player_database": {Kind: "text", Text: v}, "player_database_1": {Kind: "text", Text: v},
			}},
		},
		MySQL: debugutil.BundleMySQL{Database: "zone_3_db", Tables: []debugutil.TableRow{
			{Table: "player_database", Found: true, Row: rawValue(t, &databasepb.PlayerDatabase{PlayerId: bundlePlayer, FenceOwnerEpoch: 6, FenceSaveSeq: 50})},
			{Table: "player_database_1", Found: true, Row: rawValue(t, &databasepb.PlayerDatabase_1{PlayerId: bundlePlayer, FenceOwnerEpoch: 6, FenceSaveSeq: 50})},
		}},
	}
	if withParent {
		parent := &databasepb.PlayerAllData{
			PlayerDatabaseData:   &databasepb.PlayerDatabase{PlayerId: bundlePlayer},
			PlayerDatabase_1Data: &databasepb.PlayerDatabase_1{PlayerId: bundlePlayer},
			LastSaveSeq:          60,
		}
		b.Redis = append(b.Redis, debugutil.RedisEntry{Key: debugutil.ParentKey(bundlePlayer), Type: "string", Value: rawValue(t, parent)})
	}
	return b
}

func TestBuildImportPlanPrefersParentAndCollectsVersions(t *testing.T) {
	plan, err := buildImportPlan(testBundle(t, true), sourceAuto)
	if err != nil {
		t.Fatal(err)
	}
	if plan.source != sourceRedisParent {
		t.Fatalf("source = %s", plan.source)
	}
	if plan.baseEpoch != 7 || plan.seq != 60 {
		t.Fatalf("baseEpoch=%d seq=%d, want 7/60", plan.baseEpoch, plan.seq)
	}
	if len(plan.rows) != 2 || plan.rows[0].table != "player_database" || plan.rows[1].table != "player_database_1" {
		t.Fatalf("rows = %+v", plan.rows)
	}
	if playerIDOf(plan.rows[0].msg) != bundlePlayer {
		t.Fatal("player_id lost precision")
	}
}

func TestBuildImportPlanSQLSource(t *testing.T) {
	plan, err := buildImportPlan(testBundle(t, false), sourceAuto)
	if err != nil {
		t.Fatal(err)
	}
	if plan.source != sourceSQL || plan.seq != 50 || plan.baseEpoch != 7 {
		t.Fatalf("plan = %+v", plan)
	}
	if _, err := buildImportPlan(testBundle(t, false), sourceRedisParent); err == nil {
		t.Fatal("redis-parent without parent must fail")
	}

	missing := testBundle(t, true)
	missing.MySQL.Tables = missing.MySQL.Tables[:1]
	if _, err := buildImportPlan(missing, sourceSQL); err == nil {
		t.Fatal("sql source with one table missing must fail")
	}
	if _, err := buildImportPlan(missing, sourceAuto); err != nil {
		t.Fatalf("auto with parent should not need SQL rows: %v", err)
	}
}

func TestBuildImportPlanRejectsForeignRow(t *testing.T) {
	b := testBundle(t, false)
	b.MySQL.Tables[0].Row = rawValue(t, &databasepb.PlayerDatabase{PlayerId: 1})
	if _, err := buildImportPlan(b, sourceSQL); err == nil || !strings.Contains(err.Error(), "player_id") {
		t.Fatalf("foreign row accepted: %v", err)
	}
}

// 行的持久 schema 世代高于本工具时拒绝导入:本工具写不出新世代的列,还会原样拷走声称完整的戳(混版本设计 §5.4)。
func TestBuildImportPlanRejectsNewerPersistSchemaGen(t *testing.T) {
	newer := uint32(persistschema.Gen) + 1

	sqlBundle := testBundle(t, false)
	sqlBundle.MySQL.Tables[1].Row = rawValue(t, &databasepb.PlayerDatabase_1{PlayerId: bundlePlayer, FenceOwnerEpoch: 6, FenceSaveSeq: 50, PersistSchemaGen: newer})
	if _, err := buildImportPlan(sqlBundle, sourceSQL); err == nil || !strings.Contains(err.Error(), "持久 schema 世代") {
		t.Fatalf("SQL 行世代超前必须拒绝: %v", err)
	}

	parentBundle := testBundle(t, false)
	parent := &databasepb.PlayerAllData{
		PlayerDatabaseData:   &databasepb.PlayerDatabase{PlayerId: bundlePlayer, PersistSchemaGen: newer},
		PlayerDatabase_1Data: &databasepb.PlayerDatabase_1{PlayerId: bundlePlayer},
	}
	parentBundle.Redis = append(parentBundle.Redis, debugutil.RedisEntry{Key: debugutil.ParentKey(bundlePlayer), Type: "string", Value: rawValue(t, parent)})
	if _, err := buildImportPlan(parentBundle, sourceRedisParent); err == nil || !strings.Contains(err.Error(), "持久 schema 世代") {
		t.Fatalf("父快照世代超前必须拒绝: %v", err)
	}

	current := testBundle(t, false)
	current.MySQL.Tables[0].Row = rawValue(t, &databasepb.PlayerDatabase{PlayerId: bundlePlayer, FenceOwnerEpoch: 6, FenceSaveSeq: 50, PersistSchemaGen: persistschema.Gen})
	if _, err := buildImportPlan(current, sourceSQL); err != nil {
		t.Fatalf("本工具世代的行应可导入: %v", err)
	}
}

func TestParseBundleRoundTripKeepsLargeIDs(t *testing.T) {
	raw, err := json.Marshal(testBundle(t, true))
	if err != nil {
		t.Fatal(err)
	}
	if !isBundleFile(raw) {
		t.Fatal("bundle not detected")
	}
	b, err := parseBundle(raw)
	if err != nil {
		t.Fatal(err)
	}
	if b.PlayerID != bundlePlayer {
		t.Fatalf("player_id = %d", b.PlayerID)
	}

	b.FormatVersion = 99
	raw, _ = json.Marshal(b)
	if _, err := parseBundle(raw); err == nil {
		t.Fatal("unknown format_version accepted")
	}
	if isBundleFile([]byte(`{"mode":"playerdb"}`)) {
		t.Fatal("legacy export detected as bundle")
	}
}

func TestLocalPlayerAdmit(t *testing.T) {
	if err := (&localPlayerState{}).admit(false, 1); err != nil {
		t.Fatalf("clean target rejected: %v", err)
	}
	if err := (&localPlayerState{onlineKeys: []string{"x"}}).admit(true, 1); err == nil {
		t.Fatal("online local player must be rejected even with -replace")
	}
	existing := &localPlayerState{sqlTables: []string{"player_database"}}
	if err := existing.admit(false, 1); err == nil {
		t.Fatal("existing save overwritten without -replace")
	}
	if err := existing.admit(true, 1); err != nil {
		t.Fatalf("-replace rejected: %v", err)
	}
	if err := (&localPlayerState{mapped: true, mappedZone: 2}).admit(true, 1); err == nil {
		t.Fatal("mapping to another zone must be rejected")
	}
	for raw, wantOK := range map[string]bool{"1:3": true, "1000000:1": false, "1:2:frozen:run1": false, "": false, "x": false} {
		err := (&localPlayerState{placementPresent: true, placementRaw: raw}).admit(true, 1)
		if (err == nil) != wantOK {
			t.Fatalf("placement %q: err=%v, want ok=%v", raw, err, wantOK)
		}
	}
}

func TestBuildImportPlanRejectsBrokenParentAndStaleCheckout(t *testing.T) {
	b := testBundle(t, false)
	broken := &databasepb.PlayerAllData{PlayerDatabaseData: &databasepb.PlayerDatabase{PlayerId: bundlePlayer}, LastSaveSeq: 60}
	b.Redis = append(b.Redis, debugutil.RedisEntry{Key: debugutil.ParentKey(bundlePlayer), Type: "string", Value: rawValue(t, broken)})
	if _, err := buildImportPlan(b, sourceRedisParent); err == nil {
		t.Fatal("parent missing player_database_1_data accepted for redis-parent")
	}
	plan, err := buildImportPlan(b, sourceAuto)
	if err != nil || plan.source != sourceSQL || len(plan.notes) == 0 {
		t.Fatalf("auto must fall back to SQL with a note: %+v %v", plan, err)
	}

	stale := testBundle(t, false)
	stale.MySQL.Tables[0].LiveExtraColumns = []string{"inbox_ledger"}
	if _, err := buildImportPlan(stale, sourceSQL); err == nil {
		t.Fatal("rows exported by an older checkout accepted")
	}
}

func TestStageThenCommitLocalRedis(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	const id = 11
	mr.Set(debugutil.ParentKey(id), "stale-parent")
	mr.Set(debugutil.SubTableCacheKey(&databasepb.PlayerDatabase{}, id), "stale-cache")
	mr.Set(ownerepoch.OwnerEpochKey(id), "3")
	old := savebarrier.Version(3, 1)
	mr.HSet(savebarrier.Key(id), "pending", old, "player_database", old, "player_database_1", old, "outbox_version", old)
	mr.ZAdd(debugutil.SavePendingIndexKey(1, id), 0, "11")

	version := savebarrier.Version(8, 60)
	if err := stageLocalRedis(context.Background(), client, id, 1, "3", 8, version); err != nil {
		t.Fatal(err)
	}
	// 第 1 段之后:旧父快照没了、pending 已前进而 ACK 为空 —— login 冷加载与快路径都会拒绝,不会加载旧本地数据。
	if mr.Exists(debugutil.ParentKey(id)) || mr.Exists(debugutil.SubTableCacheKey(&databasepb.PlayerDatabase{}, id)) {
		t.Fatal("stale parent / cache not deleted")
	}
	if mr.HGet(savebarrier.Key(id), "pending") != version || mr.HGet(savebarrier.Key(id), "player_database") != "" {
		t.Fatal("stage must advance pending without ACKs")
	}
	if mr.HGet(savebarrier.Key(id), "outbox_version") != "" {
		t.Fatal("stale outbox survived; C++ save Lua would reject the barrier")
	}
	if got, _ := mr.Get(ownerepoch.OwnerEpochKey(id)); got != "8" {
		t.Fatalf("owner_epoch = %q", got)
	}
	if members, _ := mr.ZMembers(debugutil.SavePendingIndexKey(1, id)); len(members) != 0 {
		t.Fatalf("save_pending index still has %v", members)
	}

	if err := commitLocalRedis(context.Background(), client, id, version); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"pending", "player_database", "player_database_1"} {
		if got := mr.HGet(savebarrier.Key(id), field); got != version {
			t.Fatalf("barrier %s = %q", field, got)
		}
	}
	if mr.Exists(debugutil.PlayerToAccountKey(id)) {
		t.Fatal("屏障提交不能伪造账号归属，目录必须另行提交SQL")
	}
	if mr.Exists(scenenode.LocationKey(id)) {
		t.Fatal("import must not create a location")
	}
}

func TestStageLocalRedisRechecksPreflight(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()
	const id = 12
	version := savebarrier.Version(2, 1)

	mr.Set(ownerepoch.OwnerEpochKey(id), "4")
	if err := stageLocalRedis(context.Background(), client, id, 1, "", 2, version); err == nil {
		t.Fatal("owner_epoch appeared after preflight but stage did not refuse")
	}
	if mr.Exists(savebarrier.Key(id)) {
		t.Fatal("refused stage must not write")
	}

	mr.Set(scenenode.LocationKey(id), "x")
	if err := stageLocalRedis(context.Background(), client, id, 1, "4", 5, version); err == nil {
		t.Fatal("player came online after preflight but stage did not refuse")
	}

	mr.Del(scenenode.LocationKey(id))
	mr.Set("player_locker:12", "inflight-login")
	if err := stageLocalRedis(context.Background(), client, id, 1, "4", 5, version); err == nil {
		t.Fatal("已受理的登录链尚未写locator，导入仍必须拒绝")
	}
	if mr.Exists(savebarrier.Key(id)) {
		t.Fatal("登录链冲突不能推进屏障")
	}
	if owner, err := mr.Get(ownerepoch.OwnerEpochKey(id)); err != nil || owner != "4" {
		t.Fatalf("拒绝导入不应前进owner: owner=%q err=%v", owner, err)
	}
	mr.Del("player_locker:12")
	if err := stageLocalRedis(context.Background(), client, id, 1, "4", 5, version); err != nil {
		t.Fatalf("matching preflight refused: %v", err)
	}
	if err := commitLocalRedis(context.Background(), client, id, savebarrier.Version(9, 9)); err == nil {
		t.Fatal("commit must refuse when pending is not the staged version")
	}
	if mr.Exists(debugutil.PlayerToAccountKey(id)) {
		t.Fatal("refused commit must not write the reverse index")
	}
}
