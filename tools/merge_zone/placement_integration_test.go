//go:build merge_integration

package main

// docs/design/player-storage-placement.md 第二段(落点)的集成测试:对着真实 MySQL / Redis 与真二进制,
// 跳过条件、一次性库与 Redis DB 9/10/11 的隔离全部沿用 integration_test.go 的 TestMain。
//
//	go test -tags merge_integration -run "TestIT_Placement_|TestIT_Relocate_|TestIT_StorageAudit" -v ./...
//
// 覆盖:CAS 脚本语义、能力标记;合服 pin 端到端(验证 / 续跑 / 撤销)、copy 跳过有记录者、能力标记缺失拒绝、
// 冻结玩家让合服与撤销拒绝(与搬库互斥);-mode pin-placement;搬库(进非 zone 落点库再搬回、冷副本覆盖、
// 条件不满足跳过、等锁超时解冻、比对失败解冻、崩溃续跑、abort、能力标记缺失拒绝、R3 语句走主键);storage-audit。
// 纯判定的单测在 placement_codec_test.go / placement_ops_test.go / relocate_test.go。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// itMarkCapable 替 go/db 写能力标记(它启动成功之后写的那一把,mapping Redis)。
func itMarkCapable(t *testing.T, zones ...uint32) {
	t.Helper()
	m := itRedis(t, itMappingRD)
	for _, z := range zones {
		if err := m.Set(context.Background(), capabilityKey(z), capabilityRoutingV1, 0).Err(); err != nil {
			t.Fatal(err)
		}
	}
}

// itPlacement 读 player:placement:{id};键不存在返回 ""。
func itPlacement(t *testing.T, id uint64) string {
	t.Helper()
	v, _ := itRedis(t, itMappingRD).Get(context.Background(), placementKey(id)).Result()
	return v
}

func itSetPlacement(t *testing.T, id uint64, value string) {
	t.Helper()
	if err := itRedis(t, itMappingRD).Set(context.Background(), placementKey(id), value, 0).Err(); err != nil {
		t.Fatal(err)
	}
}

func itHome(t *testing.T, id uint64) string {
	t.Helper()
	v, _ := itRedis(t, itMappingRD).Get(context.Background(), playerZoneKey(id)).Result()
	return v
}

func itSetHome(t *testing.T, id uint64, zone uint32) {
	t.Helper()
	if err := itRedis(t, itMappingRD).Set(context.Background(), playerZoneKey(id), fmt.Sprint(zone), 0).Err(); err != nil {
		t.Fatal(err)
	}
}

// itTransform 读 schema.player_database 里一名玩家的 transform 列;没有行返回 found=false。
func itTransform(t *testing.T, db *sql.DB, schema string, id uint64) (string, bool) {
	t.Helper()
	var v []byte
	err := db.QueryRow("SELECT transform FROM "+schema+".player_database WHERE player_id = ?", id).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(v), true
}

// itPlayerTables 是一次性库里的玩家表(与 discoverPlayerTables 在 zone_901_db 上发现的一致,升序)。
var itPlayerTables = []string{"player_centre_database", "player_database", "player_database_1"}

// ── CAS 脚本与能力标记 ───────────────────────────────────────────

func TestIT_Placement_CASScriptSemantics(t *testing.T) {
	itReset(t)
	ctx := context.Background()
	m := itRedis(t, itMappingRD)
	itSetHome(t, 9001, itSrcZone)
	itSetHome(t, 9002, itDstZone)
	itSetHome(t, 9003, itSrcZone)
	if err := m.Set(ctx, mergeFenceKey(itSrcZone), `{"run_id":"someone"}`, 0).Err(); err != nil {
		t.Fatal(err)
	}

	// 9001:无记录、home 对 → 写成;9002:home 不是 901 → 不写;9003:围栏在 → 不写。
	res, err := runPlacementCAS(ctx, m, []placementCAS{
		{PlayerID: 9001, ExpectAbsent: true, ExpectHome: "901", NewValue: "901:1"},
		{PlayerID: 9002, ExpectAbsent: true, ExpectHome: "901", NewValue: "901:1"},
		{PlayerID: 9003, ExpectAbsent: true, ExpectHome: "901", FenceZone: itSrcZone, NewValue: "901:1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Outcome != placementCASWritten || res[1].Outcome != placementCASHomeChanged || res[1].CurrentHome != "902" ||
		res[2].Outcome != placementCASFenced {
		t.Fatalf("outcomes = %+v", res)
	}
	if itPlacement(t, 9001) != "901:1" || itPlacement(t, 9002) != "" || itPlacement(t, 9003) != "" {
		t.Fatalf("records = %q %q %q", itPlacement(t, 9001), itPlacement(t, 9002), itPlacement(t, 9003))
	}

	// 同一个 pipeline 里对同一把键两次按原值 CAS:第一次写成冻结值,第二次看到的已不是原值,不写。
	res, err = runPlacementCAS(ctx, m, []placementCAS{
		{PlayerID: 9001, ExpectRecord: "901:1", NewValue: frozenValue(901, 1, "reloc-it")},
		{PlayerID: 9001, ExpectRecord: "901:1", NewValue: "902:2"},
		{PlayerID: 9001, ExpectAbsent: true, NewValue: "901:1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Outcome != placementCASWritten || res[1].Outcome != placementCASRecordChanged ||
		res[1].CurrentRecord != frozenValue(901, 1, "reloc-it") || res[2].Outcome != placementCASRecordChanged {
		t.Fatalf("outcomes = %+v", res)
	}
	// 坏值在发出去之前就被挡住,一个字节都不写。
	if _, err := runPlacementCAS(ctx, m, []placementCAS{{PlayerID: 9002, ExpectAbsent: true, NewValue: "901:0"}}); err == nil {
		t.Fatal("a malformed new value must be refused before it reaches Redis")
	}
	if itPlacement(t, 9002) != "" {
		t.Fatal("a refused CAS wrote a record")
	}

	reads, err := readPlacements(ctx, m, []uint64{9001, 9002, 9004})
	if err != nil {
		t.Fatal(err)
	}
	if !reads[0].Record.Frozen || reads[0].Record.RunID != "reloc-it" || reads[1].RecordPresent || reads[1].Home != 902 ||
		reads[2].RecordPresent || reads[2].HomePresent {
		t.Fatalf("reads = %+v", reads)
	}
}

func TestIT_Placement_CapabilityCheck(t *testing.T) {
	itReset(t)
	ctx := context.Background()
	m := itRedis(t, itMappingRD)
	err := checkPlacementCapabilities(ctx, m, []uint32{901, 902})
	if err == nil || !strings.Contains(err.Error(), "missing in [901 902]") {
		t.Fatalf("no markers: %v", err)
	}
	itMarkCapable(t, 901)
	if err := checkPlacementCapabilities(ctx, m, []uint32{901, 902}); err == nil || !strings.Contains(err.Error(), "missing in [902]") {
		t.Fatalf("one marker: %v", err)
	}
	m.Set(ctx, capabilityKey(902), "placement-routing-v0", 0)
	if err := checkPlacementCapabilities(ctx, m, []uint32{901, 902}); err == nil || !strings.Contains(err.Error(), "placement-routing-v0") {
		t.Fatalf("an unknown marker value must not count: %v", err)
	}
	itMarkCapable(t, 902)
	if err := checkPlacementCapabilities(ctx, m, []uint32{901, 902}); err != nil {
		t.Fatalf("both marked: %v", err)
	}
}

// ── 合服 pin 模式 ────────────────────────────────────────────────

// TestIT_Placement_PinMergeVerifyUnmerge 是 pin 模式的端到端(§10.1):合服只改归属,一行都不拷;
// 无记录者钉 "{src}:1",已有记录者不动;共享缓存不删;-verify-merged 按有效落点核对;续跑幂等;撤销只改回 home。
func TestIT_Placement_PinMergeVerifyUnmerge(t *testing.T) {
	itReset(t)
	ctx := context.Background()
	db := itOpen(t)
	mapRdb := itRedis(t, itMappingRD)
	shared := itRedis(t, itSharedRD)
	ids := []uint64{9001, 9002, 9003}
	itSeedPlayers(t, ids, true)
	// 9003 合服之前就有落点记录(早先搬过):不动它。
	itSetPlacement(t, 9003, "901:5")
	// 目标区原住民。
	if _, err := db.Exec("INSERT INTO "+zoneDBName(itDstZone)+".player_database (player_id, transform) VALUES (?,?)",
		9050, []byte("native")); err != nil {
		t.Fatal(err)
	}
	itSetHome(t, 9050, itDstZone)
	shared.Set(ctx, "PlayerAllData:9001", "live-cache", 0)
	itMarkCapable(t, itSrcZone, itDstZone)

	manifest := filepath.Join(t.TempDir(), "merge.json")
	args := []string{"-source-zone", "901", "-target-zone", "902", "-manifest-path", manifest, "-apply", "-expected-src-players", "3",
		"-db-capability-zones", "901,902"}
	out, code := itRun(t, args...)
	if code != 0 {
		t.Fatalf("pin merge exit=%d\n%s", code, out)
	}
	if !strings.Contains(out, "candidates=3 pinned=2 already_pinned=0 kept_existing=1") {
		t.Errorf("the pin step did not report its counts:\n%s", out)
	}
	if n := itCount(t, db, zoneDBName(itDstZone)+".player_database"); n != 1 {
		t.Errorf("pin mode copied rows: target player_database has %d rows, want 1 (the native)", n)
	}
	for _, id := range ids {
		if h := itHome(t, id); h != "902" {
			t.Errorf("player %d home = %q, want 902", id, h)
		}
	}
	if itPlacement(t, 9001) != "901:1" || itPlacement(t, 9002) != "901:1" || itPlacement(t, 9003) != "901:5" {
		t.Errorf("placements = %q %q %q, want 901:1 901:1 901:5", itPlacement(t, 9001), itPlacement(t, 9002), itPlacement(t, 9003))
	}
	if itPlacement(t, 9050) != "" {
		t.Error("a target-zone native got a placement record")
	}
	if n, _ := shared.Exists(ctx, "PlayerAllData:9001").Result(); n != 1 {
		t.Error("pin mode must not invalidate the shared player cache (nothing moved)")
	}
	itAssertNoFence(t, "a finished pin merge")
	if n, _ := mapRdb.Exists(ctx, mergedIntoKey(itSrcZone)).Result(); n != 1 {
		t.Error("merge:merged_into:901 missing after the pin merge")
	}
	man, err := loadManifest(manifest)
	if err != nil || man == nil {
		t.Fatalf("manifest: %v", err)
	}
	if man.PlayerRowsMode != playerRowsModePin || !man.stepDone(stepPinPlacement) || man.stepDone(stepPlayerRows) ||
		len(man.Tables) != 0 || !man.PlacementScanned || fmt.Sprint(man.PlacementExisting) != "[{9003 901:5}]" {
		t.Errorf("manifest = mode %q steps %+v tables %v scanned %v existing %v", man.PlayerRowsMode, man.Steps, man.Tables,
			man.PlacementScanned, man.PlacementExisting)
	}

	verify := func() (string, int) {
		return itRun(t, "-mode", "audit", "-source-zone", "901", "-target-zone", "902", "-verify-merged",
			"-manifest-path", manifest, "-expected-src-players", "3")
	}
	if out, code := verify(); code != 0 || !strings.Contains(out, "0 block(s)") {
		t.Fatalf("pin verification must pass (rows are found through the placement records) exit=%d\n%s", code, out)
	}
	// 漏钉一个:有效落点回落 home = 902,目标库没有他的行 —— 验证必须拦住。
	mapRdb.Del(ctx, placementKey(9002))
	if out, code := verify(); code != 1 || !strings.Contains(out, "9002") {
		t.Fatalf("a player whose pin is missing must block (exit=%d)\n%s", code, out)
	}
	itSetPlacement(t, 9002, "901:1")

	// 续跑幂等:读清单,什么都不再写。
	out, code = itRun(t, args...)
	if code != 0 || !strings.Contains(out, "RESUME") {
		t.Fatalf("re-run exit=%d\n%s", code, out)
	}
	if n := itCount(t, db, zoneDBName(itDstZone)+".player_database"); n != 1 {
		t.Errorf("the re-run copied rows: %d", n)
	}

	// 撤销:只改回 home,落点记录不动,不删行、不删缓存。pin 清单的撤销同样要求 -db-capability-zones(没有缺省值)。
	if out, code := itRun(t, "-mode", "unmerge", "-manifest-path", manifest, "-apply"); code == 0 ||
		!strings.Contains(out, "-db-capability-zones") {
		t.Fatalf("unmerging a pin merge without -db-capability-zones must be refused (exit=%d)\n%s", code, out)
	}
	out, code = itRun(t, "-mode", "unmerge", "-manifest-path", manifest, "-apply", "-db-capability-zones", "901,902")
	if code != 0 {
		t.Fatalf("unmerge exit=%d\n%s", code, out)
	}
	for _, id := range ids {
		if h := itHome(t, id); h != "901" {
			t.Errorf("player %d home after unmerge = %q, want 901", id, h)
		}
	}
	if itPlacement(t, 9001) != "901:1" || itPlacement(t, 9003) != "901:5" {
		t.Errorf("unmerge touched placements: %q %q", itPlacement(t, 9001), itPlacement(t, 9003))
	}
	if itHome(t, 9050) != "902" || itCount(t, db, zoneDBName(itDstZone)+".player_database") != 1 {
		t.Error("unmerge disturbed the target zone")
	}
	if n := itCount(t, db, zoneDBName(itSrcZone)+".player_database"); n != 3 {
		t.Errorf("source rows after unmerge = %d, want 3 (they never moved)", n)
	}
	if n, _ := shared.Exists(ctx, "PlayerAllData:9001").Result(); n != 1 {
		t.Error("a pin-mode unmerge must not invalidate the shared cache")
	}
}

func TestIT_Placement_PinMergeRefusedWithoutCapability(t *testing.T) {
	itReset(t)
	itSeedPlayers(t, []uint64{9011, 9012}, true)
	manifest := filepath.Join(t.TempDir(), "merge.json")
	bare := []string{"-source-zone", "901", "-target-zone", "902", "-manifest-path", manifest, "-apply"}

	// -db-capability-zones 没有缺省值:不给就在入口拒绝。
	if out, code := itRun(t, bare...); code == 0 || !strings.Contains(out, "-db-capability-zones") ||
		!strings.Contains(out, "no default") {
		t.Fatalf("a pin merge without -db-capability-zones must be refused (exit=%d)\n%s", code, out)
	}
	itAssertNoFence(t, "a missing -db-capability-zones")

	// 列出 src / dst 而它们没有标记(T-0 已下线的情形):照常检查、被拒,文案说明 T-0 口径与 capability-check。
	args := append(append([]string{}, bare...), "-db-capability-zones", "901,902")
	out, code := itRun(t, args...)
	if code == 0 || !strings.Contains(out, "placement-routing-v1") || !strings.Contains(out, "-player-rows-mode copy") ||
		!strings.Contains(out, "T-0") || !strings.Contains(out, "capability-check") {
		t.Fatalf("a pin merge without capability markers must be refused with the way out (exit=%d)\n%s", code, out)
	}
	itAssertNoFence(t, "a capability refusal")
	if _, err := os.Stat(manifest); !os.IsNotExist(err) {
		t.Errorf("a refusal before the first write left a manifest (stat err=%v)", err)
	}
	if itHome(t, 9011) != "901" || itPlacement(t, 9011) != "" {
		t.Error("a refused merge wrote something")
	}
	// 只有源区升级了:目标区的 go/db 仍是旧版,照样拒绝,并点名缺哪个 zone。
	itMarkCapable(t, itSrcZone)
	if out, code := itRun(t, args...); code == 0 || !strings.Contains(out, "missing in [902]") {
		t.Fatalf("a missing target-zone marker must be refused (exit=%d)\n%s", code, out)
	}
}

// TestIT_Placement_CopyModeSkipsPlayersWithARecord(§10.2):copy 模式只拷没有落点记录的人;有记录者的行在其落点库,
// 不拷 —— 但处理他们存盘的 dst go/db 必须按记录选库,所以这时同样要能力标记。验证按记录找到他们的行,撤销不删他们。
func TestIT_Placement_CopyModeSkipsPlayersWithARecord(t *testing.T) {
	itReset(t)
	db := itOpen(t)
	itSeedPlayers(t, []uint64{9101, 9102, 9103}, true)
	itSetPlacement(t, 9103, "901:2")
	manifest := filepath.Join(t.TempDir(), "merge.json")
	args := []string{"-source-zone", "901", "-target-zone", "902", "-manifest-path", manifest, "-apply",
		"-player-rows-mode", "copy", "-expected-src-players", "3", "-db-capability-zones", "901,902"}

	out, code := itRun(t, args...)
	if code == 0 || !strings.Contains(out, "already have a placement record") || !strings.Contains(out, "placement-routing-v1") {
		t.Fatalf("copy mode with a record-holder and no capability markers must be refused (exit=%d)\n%s", code, out)
	}
	itAssertNoFence(t, "a copy-mode capability refusal under the fence")
	if _, err := os.Stat(manifest); !os.IsNotExist(err) {
		t.Errorf("the refusal left a manifest (stat err=%v)", err)
	}

	itMarkCapable(t, itSrcZone, itDstZone)
	out, code = itRun(t, args...)
	if code != 0 {
		t.Fatalf("copy merge exit=%d\n%s", code, out)
	}
	for id, want := range map[uint64]bool{9101: true, 9102: true, 9103: false} {
		if _, found := itTransform(t, db, zoneDBName(itDstZone), id); found != want {
			t.Errorf("player %d row in the target zone = %v, want %v", id, found, want)
		}
	}
	man, err := loadManifest(manifest)
	if err != nil || man == nil {
		t.Fatalf("manifest: %v", err)
	}
	if man.PlayerRowsMode != playerRowsModeCopy || fmt.Sprint(man.PlacementExisting) != "[{9103 901:2}]" ||
		!strings.Contains(man.Steps[stepPlayerRows].Detail, "placed_skipped=1") {
		t.Errorf("manifest = mode %q existing %v rows step %+v", man.PlayerRowsMode, man.PlacementExisting, man.Steps[stepPlayerRows])
	}
	if itHome(t, 9103) != "902" || itPlacement(t, 9103) != "901:2" {
		t.Errorf("9103 home %q placement %q, want 902 / 901:2", itHome(t, 9103), itPlacement(t, 9103))
	}
	out, code = itRun(t, "-mode", "audit", "-source-zone", "901", "-target-zone", "902", "-verify-merged",
		"-manifest-path", manifest, "-expected-src-players", "3")
	if code != 0 {
		t.Fatalf("verification must find 9103's row through its record (exit=%d)\n%s", code, out)
	}

	out, code = itRun(t, "-mode", "unmerge", "-manifest-path", manifest, "-apply")
	if code != 0 {
		t.Fatalf("unmerge exit=%d\n%s", code, out)
	}
	if n := itCount(t, db, zoneDBName(itDstZone)+".player_database"); n != 0 {
		t.Errorf("target rows after unmerge = %d, want 0 (the two copies deleted)", n)
	}
	if _, found := itTransform(t, db, zoneDBName(itSrcZone), 9103); !found || itPlacement(t, 9103) != "901:2" {
		t.Error("the unmerge touched the record-holder's row or record")
	}
}

// TestIT_Placement_MergeAndUnmergeRefuseFrozenPlayers(§9 末条):清单里有冻结记录(搬库正在挪)的玩家时,合服
// (两种模式)与撤销都在围栏之下、任何写之前拒绝,正常释放围栏。
func TestIT_Placement_MergeAndUnmergeRefuseFrozenPlayers(t *testing.T) {
	itReset(t)
	itSeedPlayers(t, []uint64{9201, 9202}, true)
	itSetPlacement(t, 9202, frozenValue(901, 1, "reloc-it-x"))
	itMarkCapable(t, itSrcZone, itDstZone)
	for _, mode := range []string{"pin", "copy"} {
		manifest := filepath.Join(t.TempDir(), "merge.json")
		out, code := itRun(t, "-source-zone", "901", "-target-zone", "902", "-manifest-path", manifest, "-apply",
			"-player-rows-mode", mode, "-db-capability-zones", "901,902")
		if code == 0 || !strings.Contains(out, "FROZEN") || !strings.Contains(out, "reloc-it-x") || !strings.Contains(out, "relocate-abort") {
			t.Fatalf("%s: a frozen manifest player must refuse the merge (exit=%d)\n%s", mode, code, out)
		}
		itAssertNoFence(t, mode+" merge refused for a frozen player")
		if _, err := os.Stat(manifest); !os.IsNotExist(err) {
			t.Errorf("%s: the refusal left a manifest", mode)
		}
		if itHome(t, 9201) != "901" || itPlacement(t, 9201) != "" {
			t.Errorf("%s: the refused merge wrote something", mode)
		}
	}

	// 撤销:合服之后的状态(home 已是 902),清单里有人被冻结 —— 同样拒绝,映射原封不动。
	itSetHome(t, 9201, itDstZone)
	itSetHome(t, 9202, itDstZone)
	manifest := filepath.Join(t.TempDir(), "done.json")
	m := newMergeManifest("run-frozen-unmerge", itSrcZone, itDstZone, "it", time.Now())
	m.PlayerIDs = []uint64{9201, 9202}
	if err := saveManifest(manifest, m); err != nil {
		t.Fatal(err)
	}
	out, code := itRun(t, "-mode", "unmerge", "-manifest-path", manifest, "-apply")
	if code == 0 || !strings.Contains(out, "unmerge refused") || !strings.Contains(out, "FROZEN") {
		t.Fatalf("an unmerge over a frozen player must be refused (exit=%d)\n%s", code, out)
	}
	if itHome(t, 9201) != "902" {
		t.Error("the refused unmerge restored a mapping")
	}
	itAssertNoFence(t, "an unmerge refused for a frozen player")
}

// TestIT_Placement_UnmergeRequiresSourceCapabilityForOffSourceRecords(§10.4 第 1 条的镜像):合服后被
// pin-placement 钉在 dst 的清单玩家,撤销改回 home 之后由 src 的 go/db 处理存盘,它必须按记录选库;
// 没有能力标记时在任何写之前拒绝(未进入半撤销状态,围栏照常释放),补上标记后撤销照常进行、落点记录不动。
func TestIT_Placement_UnmergeRequiresSourceCapabilityForOffSourceRecords(t *testing.T) {
	itReset(t)
	itSetHome(t, 9231, itDstZone)
	itSetHome(t, 9232, itDstZone)
	itSetPlacement(t, 9232, "902:1") // 合服后 -mode pin-placement -zone 902 钉的
	manifest := filepath.Join(t.TempDir(), "done.json")
	m := newMergeManifest("run-offsrc-unmerge", itSrcZone, itDstZone, "it", time.Now())
	m.PlayerIDs = []uint64{9231, 9232}
	if err := saveManifest(manifest, m); err != nil {
		t.Fatal(err)
	}

	out, code := itRun(t, "-mode", "unmerge", "-manifest-path", manifest, "-apply")
	if code == 0 || !strings.Contains(out, "does not point at zone 901") || !strings.Contains(out, "placement-routing-v1") {
		t.Fatalf("an unmerge over an off-source record without the src marker must be refused (exit=%d)\n%s", code, out)
	}
	itAssertNoFence(t, "an unmerge refused for a missing src capability marker")
	if itHome(t, 9231) != "902" || itHome(t, 9232) != "902" {
		t.Error("the refused unmerge restored a mapping")
	}

	// 只有目标区的标记不够:撤销之后处理存盘的是源区。
	itMarkCapable(t, itDstZone)
	if out, code := itRun(t, "-mode", "unmerge", "-manifest-path", manifest, "-apply"); code == 0 ||
		!strings.Contains(out, "missing in [901]") {
		t.Fatalf("a missing source-zone marker must be refused (exit=%d)\n%s", code, out)
	}

	itMarkCapable(t, itSrcZone)
	if out, code := itRun(t, "-mode", "unmerge", "-manifest-path", manifest, "-apply"); code != 0 {
		t.Fatalf("with the src marker the unmerge must go through (exit=%d)\n%s", code, out)
	}
	if itHome(t, 9231) != "901" || itHome(t, 9232) != "901" {
		t.Errorf("mappings not restored: %s / %s", itHome(t, 9231), itHome(t, 9232))
	}
	if got := itPlacement(t, 9232); got != "902:1" {
		t.Errorf("the unmerge must leave the placement record alone, got %q", got)
	}
}

func TestIT_Placement_ResumeMustKeepTheManifestMode(t *testing.T) {
	itReset(t)
	itSeedPlayers(t, []uint64{9211}, true)
	itMarkCapable(t, itSrcZone, itDstZone)
	manifest := filepath.Join(t.TempDir(), "merge.json")
	m := newMergeManifest("run-legacy", itSrcZone, itDstZone, "it", time.Now()) // 没有 player_rows_mode = copy
	m.PlayerIDs = []uint64{9211}
	if err := saveManifest(manifest, m); err != nil {
		t.Fatal(err)
	}
	out, code := itRun(t, "-source-zone", "901", "-target-zone", "902", "-manifest-path", manifest, "-apply",
		"-db-capability-zones", "901,902")
	if code == 0 || !strings.Contains(out, "pass -player-rows-mode copy") {
		t.Fatalf("resuming a copy-mode manifest with the pin default must be refused (exit=%d)\n%s", code, out)
	}
	// 续跑的清单校验在围栏之下做:上一次 -apply 已落清单(可能已写过),拒绝时围栏保留。
	itAssertFenceKept(t, out, "a mode refusal of a resume")
}

// ── -mode pin-placement ─────────────────────────────────────────

func TestIT_Placement_PinPlacementMode(t *testing.T) {
	itReset(t)
	ctx := context.Background()
	mapRdb := itRedis(t, itMappingRD)
	itSetHome(t, 9301, itSrcZone)
	itSetHome(t, 9302, itSrcZone)
	itSetPlacement(t, 9302, "1000000:3")
	itSetHome(t, 9303, itDstZone)

	out, code := itRun(t, "-mode", "pin-placement", "-zone", "901", "-dry-run")
	if code != 0 || !strings.Contains(out, "candidates=2 pinned=1 already_pinned=0 kept_existing=1 home_moved=0") {
		t.Fatalf("dry-run exit=%d\n%s", code, out)
	}
	if itPlacement(t, 9301) != "" {
		t.Fatal("a dry-run pinned a player")
	}
	out, code = itRun(t, "-mode", "pin-placement", "-zone", "901", "-apply")
	if code != 0 {
		t.Fatalf("apply exit=%d\n%s", code, out)
	}
	if itPlacement(t, 9301) != "901:1" || itPlacement(t, 9302) != "1000000:3" || itPlacement(t, 9303) != "" {
		t.Fatalf("placements = %q %q %q", itPlacement(t, 9301), itPlacement(t, 9302), itPlacement(t, 9303))
	}
	if out, code = itRun(t, "-mode", "pin-placement", "-zone", "901", "-apply"); code != 0 || !strings.Contains(out, "already_pinned=1") {
		t.Fatalf("the re-run must be idempotent (exit=%d)\n%s", code, out)
	}

	// 正在合服 / 已被合走:拒绝,一个都不钉。
	itSetHome(t, 9304, itSrcZone)
	mapRdb.Set(ctx, mergeFenceKey(itSrcZone), `{"run_id":"m"}`, 0)
	if out, code := itRun(t, "-mode", "pin-placement", "-zone", "901", "-apply"); code == 0 || !strings.Contains(out, "being merged") {
		t.Fatalf("a fenced zone must be refused (exit=%d)\n%s", code, out)
	}
	mapRdb.Del(ctx, mergeFenceKey(itSrcZone))
	if err := markMergedInto(ctx, mapRdb, itSrcZone, itDstZone, "run-gone", false); err != nil {
		t.Fatal(err)
	}
	if out, code := itRun(t, "-mode", "pin-placement", "-zone", "901", "-apply"); code == 0 || !strings.Contains(out, "merged away") {
		t.Fatalf("a merged-away zone must be refused (exit=%d)\n%s", code, out)
	}
	if itPlacement(t, 9304) != "" {
		t.Error("a refused pin-placement wrote a record")
	}
}

// ── 搬库 ─────────────────────────────────────────────────────────

// itRelocateArgs 是 -mode relocate 的公共参数。
func itRelocateArgs(manifest string, s, t uint32, extra ...string) []string {
	return append([]string{"-mode", "relocate", "-relocate-source-storage", fmt.Sprint(s), "-relocate-target-storage", fmt.Sprint(t),
		"-db-capability-zones", "901,902", "-manifest-path", manifest}, extra...)
}

// TestIT_Relocate_IntoAPlayerStoreAndBack:整条 R0~R5 经真二进制跑一遍 —— dry-run 不写;apply 把两名玩家搬进非 zone
// 落点库(T 里的旧行是冷副本,先删后插并计数);源库的行留作冷副本;storage-audit 认出源库已无人指向;再整库搬回。
func TestIT_Relocate_IntoAPlayerStoreAndBack(t *testing.T) {
	itReset(t)
	db := itOpen(t)
	itSeedPlayers(t, []uint64{9401, 9402}, true)
	itSetPlacement(t, 9402, "901:3")
	// T 里 9401 的一份旧行(早先搬出去又搬回来留下的冷副本)。
	if _, err := db.Exec("INSERT INTO "+itStoreDB+".player_database (player_id, transform) VALUES (?,?)", 9401, []byte("stale")); err != nil {
		t.Fatal(err)
	}
	itMarkCapable(t, itSrcZone, itDstZone)
	dir := t.TempDir()
	manifest := filepath.Join(dir, "reloc.json")
	args := itRelocateArgs(manifest, itSrcZone, itStoreID, "-relocate-player-ids", "9401,9402")

	out, code := itRun(t, append(args, "-dry-run")...)
	if code != 0 || !strings.Contains(out, "2 players would be frozen and relocated") {
		t.Fatalf("dry-run exit=%d\n%s", code, out)
	}
	if itPlacement(t, 9401) != "" || itPlacement(t, 9402) != "901:3" {
		t.Fatal("a dry-run changed a placement")
	}
	if _, err := os.Stat(manifest); !os.IsNotExist(err) {
		t.Fatalf("a dry-run wrote the manifest (stat err=%v)", err)
	}

	out, code = itRun(t, append(args, "-apply")...)
	if code != 0 {
		t.Fatalf("relocate exit=%d\n%s", code, out)
	}
	if itPlacement(t, 9401) != fmt.Sprintf("%d:2", itStoreID) || itPlacement(t, 9402) != fmt.Sprintf("%d:4", itStoreID) {
		t.Fatalf("placements = %q %q, want v+1 on the store (no record counts as v=1)", itPlacement(t, 9401), itPlacement(t, 9402))
	}
	for _, id := range []uint64{9401, 9402} {
		if v, found := itTransform(t, db, itStoreDB, id); !found || v != fmt.Sprintf("transform-%d", id) {
			t.Errorf("player %d row in the store = %q (found=%v), want the source bytes", id, v, found)
		}
		if itHome(t, id) != "901" {
			t.Errorf("a relocation must not touch home_zone: %d → %q", id, itHome(t, id))
		}
	}
	for _, tbl := range itPlayerTables {
		if n := itCount(t, db, itStoreDB+"."+tbl); n != 2 {
			t.Errorf("%s.%s has %d rows, want 2", itStoreDB, tbl, n)
		}
		if n := itCount(t, db, zoneDBName(itSrcZone)+"."+tbl); n != 2 {
			t.Errorf("the source rows must stay as cold copies: %s has %d", tbl, n)
		}
	}
	m, err := loadRelocateManifest(manifest)
	if err != nil || m == nil {
		t.Fatalf("relocate manifest: %v", err)
	}
	if got := relocStates(m); got != "9401:switched 9402:switched" {
		t.Errorf("states = %s", got)
	}
	if p := relocPlayer(m, 9401); p.ColdCopyRows != 1 || p.Final != fmt.Sprintf("%d:2", itStoreID) {
		t.Errorf("9401 = %+v, want the stale target row counted as a cold copy", p)
	}

	// 源库已无人指向:只剩冷副本。
	out, code = itRun(t, "-mode", "storage-audit", "-storage", "901")
	if code != 0 || !strings.Contains(out, "effective=0") || !strings.Contains(out, "cold_copies=2") ||
		!strings.Contains(out, "nothing routes to storage 901") {
		t.Fatalf("storage-audit after the move (exit=%d)\n%s", code, out)
	}

	// 整库搬回(不给 id):按有效落点枚举。
	back := filepath.Join(dir, "back.json")
	out, code = itRun(t, append(itRelocateArgs(back, itStoreID, itSrcZone), "-apply")...)
	if code != 0 {
		t.Fatalf("relocate back exit=%d\n%s", code, out)
	}
	if itPlacement(t, 9401) != "901:3" || itPlacement(t, 9402) != "901:5" {
		t.Fatalf("placements after moving back = %q %q, want 901:3 901:5", itPlacement(t, 9401), itPlacement(t, 9402))
	}
}

// TestIT_Relocate_SkipsPlayersThatDoNotQualify(R1):有效落点不是 S、home 所在 zone 正在合服(与合服互斥)、
// 已被别的搬库冻结的人一律跳过,记录不动;符合条件的照常切换;没全切成 exit 1。
func TestIT_Relocate_SkipsPlayersThatDoNotQualify(t *testing.T) {
	itReset(t)
	ctx := context.Background()
	db := itOpen(t)
	itSeedPlayers(t, []uint64{9501, 9502, 9503, 9504}, true)
	itSetPlacement(t, 9501, "902:1")
	itSetHome(t, 9502, itDstZone)
	itSetPlacement(t, 9502, "901:1")
	itRedis(t, itMappingRD).Set(ctx, mergeFenceKey(itDstZone), `{"run_id":"merge-in-progress"}`, 0)
	itSetPlacement(t, 9503, frozenValue(901, 1, "reloc-other"))
	itMarkCapable(t, itSrcZone, itDstZone)
	manifest := filepath.Join(t.TempDir(), "reloc.json")

	out, code := itRun(t, append(itRelocateArgs(manifest, itSrcZone, itDstZone, "-relocate-player-ids", "9501,9502,9503,9504"), "-apply")...)
	if code != 1 || !strings.Contains(out, "NOT all players were relocated") {
		t.Fatalf("exit=%d, want 1 with a summary\n%s", code, out)
	}
	if itPlacement(t, 9504) != "902:2" {
		t.Errorf("the qualifying player was not switched: %q", itPlacement(t, 9504))
	}
	if _, found := itTransform(t, db, zoneDBName(itDstZone), 9504); !found {
		t.Error("the qualifying player's row was not copied")
	}
	if itPlacement(t, 9501) != "902:1" || itPlacement(t, 9502) != "901:1" || itPlacement(t, 9503) != frozenValue(901, 1, "reloc-other") {
		t.Errorf("skipped players' records changed: %q %q %q", itPlacement(t, 9501), itPlacement(t, 9502), itPlacement(t, 9503))
	}
	m, err := loadRelocateManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[uint64]string{9501: "effective storage is 902", 9502: "being merged", 9503: "already frozen by run reloc-other"} {
		if p := relocPlayer(m, id); p.State != relocSkipped || !strings.Contains(p.Reason, want) {
			t.Errorf("player %d = %+v, want skipped with %q", id, p, want)
		}
	}
}

// itRelocateEngine 用真实的接缝实现(mapping / shared Redis、zone_901_db → zone_902_db)装配搬库状态机,进程内驱动。
func itRelocateEngine(t *testing.T, m *relocateManifest, lockWait time.Duration, wrap func(*sqlRelocateRows) relocateRows) *relocateEngine {
	t.Helper()
	ctx := context.Background()
	db := itOpenReadCommitted(t)
	src, dst := zoneDBName(itSrcZone), zoneDBName(itDstZone)
	cols := map[string][]string{}
	for _, tbl := range itPlayerTables {
		c, err := alignedPlayerColumns(ctx, db, src, dst, tbl)
		if err != nil {
			t.Fatal(err)
		}
		cols[tbl] = c
	}
	sqlRows, err := newSQLRelocateRows(ctx, db, src, dst, itPlayerTables, cols)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sqlRows.Close)
	var rows relocateRows = sqlRows
	if wrap != nil {
		rows = wrap(sqlRows)
	}
	e := newRelocateEngine(m, func(*relocateManifest) error { return nil },
		redisRelocatePlacements{rdb: itRedis(t, itMappingRD)}, redisOrderingLocks{rdb: itRedis(t, itSharedRD)}, rows,
		map[uint32]bool{itSrcZone: true, itDstZone: true}, 100, lockWait)
	e.lockPoll = 50 * time.Millisecond
	return e
}

// TestIT_Relocate_LockWaitTimeoutUnfreezes(R2):go/db 的排序锁一直在(一条冻结之前就选好库的写还没做完),
// 期限到了本批解冻、一行都不拷;锁消失之后另起一次照常切换。
func TestIT_Relocate_LockWaitTimeoutUnfreezes(t *testing.T) {
	itReset(t)
	ctx := context.Background()
	db := itOpen(t)
	itSeedPlayers(t, []uint64{9601}, true)
	shared := itRedis(t, itSharedRD)
	lock := orderingLockKey(dbTaskTopic(itSrcZone, 1), 9601, "player_database")
	if err := shared.Set(ctx, lock, "go-db-token", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}

	m := newRelocateManifest("reloc-it-lock", itSrcZone, itDstZone, 1, itPlayerTables, []uint64{9601}, "it", time.Now())
	if err := itRelocateEngine(t, m, 300*time.Millisecond, nil).run(ctx); err != nil {
		t.Fatal(err)
	}
	if p := relocPlayer(m, 9601); p.State != relocUnfrozen || !strings.Contains(p.Reason, "R2") {
		t.Fatalf("player = %+v, want unfrozen by the lock-wait timeout", p)
	}
	if itPlacement(t, 9601) != "901:1" {
		t.Errorf("record after the timeout = %q, want the stable 901:1", itPlacement(t, 9601))
	}
	if _, found := itTransform(t, db, zoneDBName(itDstZone), 9601); found {
		t.Error("a row was copied although the in-flight write never finished")
	}

	shared.Del(ctx, lock)
	m2 := newRelocateManifest("reloc-it-lock2", itSrcZone, itDstZone, 1, itPlayerTables, []uint64{9601}, "it", time.Now())
	if err := itRelocateEngine(t, m2, 300*time.Millisecond, nil).run(ctx); err != nil {
		t.Fatal(err)
	}
	if relocPlayer(m2, 9601).State != relocSwitched || itPlacement(t, 9601) != "902:2" {
		t.Fatalf("after the lock cleared: state %s record %q", relocPlayer(m2, 9601).State, itPlacement(t, 9601))
	}
}

// itTamperingRows 在真实的 R3 之后改掉 T 里的行,模拟「拷贝与比对之间有人写了 T」。
type itTamperingRows struct {
	*sqlRelocateRows
	db *sql.DB
}

func (r itTamperingRows) copyPlayer(ctx context.Context, id uint64) (int, error) {
	n, err := r.sqlRelocateRows.copyPlayer(ctx, id)
	if err == nil {
		_, err = r.db.ExecContext(ctx, "UPDATE "+zoneDBName(itDstZone)+".player_database SET transform = ? WHERE player_id = ?",
			[]byte("tampered"), id)
	}
	return n, err
}

func TestIT_Relocate_CompareMismatchUnfreezes(t *testing.T) {
	itReset(t)
	ctx := context.Background()
	itSeedPlayers(t, []uint64{9651}, true)
	db := itOpen(t)
	m := newRelocateManifest("reloc-it-r4", itSrcZone, itDstZone, 1, itPlayerTables, []uint64{9651}, "it", time.Now())
	e := itRelocateEngine(t, m, time.Second, func(r *sqlRelocateRows) relocateRows { return itTamperingRows{sqlRelocateRows: r, db: db} })
	if err := e.run(ctx); err != nil {
		t.Fatal(err)
	}
	if p := relocPlayer(m, 9651); p.State != relocUnfrozen || !strings.Contains(p.Reason, "R4") {
		t.Fatalf("player = %+v, want unfrozen by the byte-by-byte compare", p)
	}
	if itPlacement(t, 9651) != "901:1" {
		t.Errorf("record = %q, want back on the source", itPlacement(t, 9651))
	}
}

// TestIT_Relocate_ResumeAfterACrashAndAbort:崩溃之后续跑按清单认领本次的冻结(清单来不及记的也认);
// relocate-abort 把本次仍冻结的人解冻回 S,清单标 aborted,之后不许再续跑。
func TestIT_Relocate_ResumeAfterACrashAndAbort(t *testing.T) {
	itReset(t)
	db := itOpen(t)
	itSeedPlayers(t, []uint64{9701, 9702, 9711, 9712}, true)
	itMarkCapable(t, itSrcZone, itDstZone)
	dir := t.TempDir()

	// 崩溃现场:9701 清单记了冻结;9702 冻结写进了 Redis,清单还是 pending。
	crash := filepath.Join(dir, "crash.json")
	m := newRelocateManifest("reloc-it-crash", itSrcZone, itDstZone, 1, itPlayerTables, []uint64{9701, 9702}, "it", time.Now())
	m.Players[0].State, m.Players[0].Version, m.Players[0].HomeZone = relocFrozen, 1, itSrcZone
	if err := saveRelocateManifest(crash, m); err != nil {
		t.Fatal(err)
	}
	itSetPlacement(t, 9701, frozenValue(901, 1, "reloc-it-crash"))
	itSetPlacement(t, 9702, frozenValue(901, 1, "reloc-it-crash"))
	out, code := itRun(t, append(itRelocateArgs(crash, itSrcZone, itDstZone), "-apply")...)
	if code != 0 || !strings.Contains(out, "RESUME") {
		t.Fatalf("resume exit=%d\n%s", code, out)
	}
	for _, id := range []uint64{9701, 9702} {
		if itPlacement(t, id) != "902:2" {
			t.Errorf("player %d = %q after the resume, want 902:2", id, itPlacement(t, id))
		}
		if _, found := itTransform(t, db, zoneDBName(itDstZone), id); !found {
			t.Errorf("player %d row missing in the target", id)
		}
	}

	// 放弃另一次:9711 冻结中,9712 还没冻结。
	abort := filepath.Join(dir, "abort.json")
	a := newRelocateManifest("reloc-it-abort", itSrcZone, itDstZone, 1, itPlayerTables, []uint64{9711, 9712}, "it", time.Now())
	a.Players[0].State, a.Players[0].Version, a.Players[0].HomeZone = relocFrozen, 1, itSrcZone
	if err := saveRelocateManifest(abort, a); err != nil {
		t.Fatal(err)
	}
	itSetPlacement(t, 9711, frozenValue(901, 1, "reloc-it-abort"))
	out, code = itRun(t, "-mode", "relocate-abort", "-manifest-path", abort, "-apply")
	if code != 0 {
		t.Fatalf("abort exit=%d\n%s", code, out)
	}
	if itPlacement(t, 9711) != "901:1" || itPlacement(t, 9712) != "" {
		t.Fatalf("after the abort 9711=%q 9712=%q, want 901:1 and no record", itPlacement(t, 9711), itPlacement(t, 9712))
	}
	back, err := loadRelocateManifest(abort)
	if err != nil || !back.Aborted || relocStates(back) != "9711:unfrozen 9712:skipped" {
		t.Fatalf("aborted manifest = %+v err=%v", back, err)
	}
	if out, code := itRun(t, append(itRelocateArgs(abort, itSrcZone, itDstZone), "-apply")...); code == 0 || !strings.Contains(out, "aborted") {
		t.Fatalf("an aborted run must not resume (exit=%d)\n%s", code, out)
	}
}

func TestIT_Relocate_RefusesWithoutCapabilityOrExplicitZones(t *testing.T) {
	itReset(t)
	itSeedPlayers(t, []uint64{9751}, true)
	manifest := filepath.Join(t.TempDir(), "reloc.json")
	out, code := itRun(t, append(itRelocateArgs(manifest, itSrcZone, itDstZone, "-relocate-player-ids", "9751"), "-apply")...)
	if code == 0 || !strings.Contains(out, "placement-routing-v1") {
		t.Fatalf("a relocation without capability markers must be refused (exit=%d)\n%s", code, out)
	}
	if _, err := os.Stat(manifest); !os.IsNotExist(err) {
		t.Errorf("the refusal left a manifest (stat err=%v)", err)
	}
	if itPlacement(t, 9751) != "" {
		t.Error("the refused relocation froze a player")
	}
	// -db-capability-zones 没有缺省值,搬库也不接受 none:必须显式列出所有在跑的 zone。
	out, code = itRun(t, "-mode", "relocate", "-relocate-source-storage", "901", "-relocate-target-storage", "902",
		"-manifest-path", manifest, "-apply")
	if code == 0 || !strings.Contains(out, "-db-capability-zones") || !strings.Contains(out, "every zone") {
		t.Fatalf("relocate without an explicit -db-capability-zones must be refused (exit=%d)\n%s", code, out)
	}
	out, code = itRun(t, "-mode", "relocate", "-relocate-source-storage", "901", "-relocate-target-storage", "902",
		"-manifest-path", manifest, "-apply", "-db-capability-zones", "none")
	if code == 0 || !strings.Contains(out, "none is not accepted") {
		t.Fatalf("relocate must refuse -db-capability-zones none (exit=%d)\n%s", code, out)
	}
}

// TestIT_Relocate_R3StatementsArePrimaryKeyPointOps 钉住 R3 两条写语句的执行计划(死锁审计口径):删 T 的旧行与
// INSERT … SELECT 的读侧都是完整主键等值(key=PRIMARY、key_len=8),不经过任何二级索引。
func TestIT_Relocate_R3StatementsArePrimaryKeyPointOps(t *testing.T) {
	itReset(t)
	ctx := context.Background()
	db := itOpenReadCommitted(t)
	src, dst := zoneDBName(itSrcZone)+".player_database", zoneDBName(itDstZone)+".player_database"
	for _, q := range []string{src, dst} {
		if _, err := db.Exec("INSERT INTO "+q+" (player_id, transform) VALUES (?,?)", 9851, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	plan := itExplain(t, db, itInlineUintArgs(t, relocateDeleteSQL(dst), 9851))
	if plan["key"] != "PRIMARY" || plan["key_len"] != "8" {
		t.Fatalf("R3 delete plan key=%q key_len=%q, want PRIMARY/8", plan["key"], plan["key_len"])
	}
	cols, err := alignedPlayerColumns(ctx, db, zoneDBName(itSrcZone), zoneDBName(itDstZone), "player_database")
	if err != nil {
		t.Fatal(err)
	}
	rows := itExplainRows(t, db, itInlineUintArgs(t, relocateCopySQL(dst, src, cols), 9851))
	var read map[string]string
	for _, r := range rows {
		if r["select_type"] != "INSERT" {
			read = r
		}
	}
	if read == nil || read["key"] != "PRIMARY" || read["key_len"] != "8" {
		t.Fatalf("R3 copy read-side plan = %v, want key=PRIMARY key_len=8 (all rows: %v)", read, rows)
	}
}

// itExplainRows 跑 EXPLAIN FORMAT=TRADITIONAL,返回全部行(INSERT … SELECT 有写入侧与读取侧两行)。
func itExplainRows(t *testing.T, db *sql.DB, stmt string) []map[string]string {
	t.Helper()
	rows, err := db.Query("EXPLAIN FORMAT=TRADITIONAL " + stmt)
	if err != nil {
		t.Fatalf("EXPLAIN %q: %v", stmt, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		plan := make(map[string]string, len(cols))
		for i, c := range cols {
			plan[c] = vals[i].String
		}
		out = append(out, plan)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// ── -mode storage-audit ─────────────────────────────────────────

func TestIT_StorageAudit_CountsLiveColdAndUnmappedRows(t *testing.T) {
	itReset(t)
	ctx := context.Background()
	db := itOpen(t)
	for _, id := range []uint64{9901, 9902, 9903, 9904} {
		if _, err := db.Exec("INSERT INTO "+zoneDBName(itSrcZone)+".player_database (player_id) VALUES (?)", id); err != nil {
			t.Fatal(err)
		}
	}
	// 9901:无记录、home 901 → 真源;9902:记录指向 902 → 冷副本;9903:既无记录也无 home → 无主行;
	// 9904:记录指向 901、home 902 → 真源;9905:冻结在 901(没有行也算有效落点);9906:畸形记录。
	itSetHome(t, 9901, itSrcZone)
	itSetPlacement(t, 9902, "902:1")
	itSetHome(t, 9902, itSrcZone)
	itSetPlacement(t, 9904, "901:2")
	itSetHome(t, 9904, itDstZone)
	itSetPlacement(t, 9905, frozenValue(901, 1, "reloc-x"))
	itSetPlacement(t, 9906, "junk")

	rep, err := auditStorage(ctx, db, itRedis(t, itMappingRD), itSrcZone)
	if err != nil {
		t.Fatal(err)
	}
	if rep.ByRecord != 2 || rep.FrozenOnRecord != 1 || rep.ByHome != 1 || rep.Rows != 4 || rep.LiveRows != 2 ||
		rep.ColdCopies != 1 || rep.UnmappedRows != 1 || rep.MalformedRecords != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if fmt.Sprint(rep.ColdSample, rep.UnmappedSample) != "[9902] [9903]" {
		t.Errorf("samples = %v %v", rep.ColdSample, rep.UnmappedSample)
	}
	out, code := itRun(t, "-mode", "storage-audit", "-storage", "901")
	if code != 0 || !strings.Contains(out, "NOT SAFE to retire storage 901") {
		t.Fatalf("storage-audit binary (exit=%d)\n%s", code, out)
	}
	// 库不存在 = 没查成,exit 2。
	if out, code := itRun(t, "-mode", "storage-audit", "-storage", "1000999"); code != 2 {
		t.Fatalf("a missing store must be an infrastructure failure (exit=%d)\n%s", code, out)
	}
}
