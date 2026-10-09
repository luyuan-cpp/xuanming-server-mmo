package main

// docs/design/player-storage-placement.md §12 合服工具缺口修复(第一段:A1–A11、A15)的纯单测,不连库。
// 与真库交互的部分(SQL 形状的执行计划、Lua CAS、端到端的围栏释放 / 保留)在 integration_test.go
// (build tag merge_integration)的 TestIT_Gap* 用例里。A11 的纯判定在 scene_hot_state_test.go,
// A15 的单行事务在 lock_order_test.go。
//
// merge_zone 的 go.mod 里没有 miniredis / sqlmock(也没有 vendor),按约定不为了测试引新依赖:
// Redis / MySQL 边界一律经接缝注入替身(playerIDSource 的 sliceSource、mappingPresence 函数)。

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// ── A1:守卫改集合比较 ────────────────────────────────────────

// fakeMapping 是 mappingPresence 的内存替身:集合里的 id 有 player:zone 映射(指向哪个区不管)。
func fakeMapping(mapped ...uint64) mappingPresence {
	set := make(map[uint64]bool, len(mapped))
	for _, id := range mapped {
		set[id] = true
	}
	return func(_ context.Context, ids []uint64) ([]bool, error) {
		out := make([]bool, len(ids))
		for i, id := range ids {
			out[i] = set[id]
		}
		return out, nil
	}
}

func guardOptions() options {
	return options{sourceZone: 901, mappingAddr: "127.0.0.1:6379", mappingDB: 0, expectedSrcPlayers: -1}
}

func TestGuardPlayerSet_UnmappedRowIsNotOffsetByMappedPlayerWithoutRow(t *testing.T) {
	// 源库 {1,2,3};映射到源区的是 {1,2,4}(4 是映射到源区、首次存盘还没落库的新号)。
	// 旧守卫只比「行数 3 <= 收集到的 3 个 id」,放行 —— 而 3 没有映射,会被漏在死区。
	rows := sliceSource{1, 2, 3}
	collected := []uint64{1, 2, 4}
	err := guardPlayerSet(context.Background(), rows, fakeMapping(1, 2, 4), collected, guardOptions(), false)
	if err == nil {
		t.Fatal("a row without any mapping must be refused even when a mapped player without a row balances the count")
	}
	for _, want := range []string{"1 of them have NO player:zone mapping", "[3]", "backfill-home-zone -zone 901"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not contain %q", err, want)
		}
	}
}

func TestGuardPlayerSet_RowMappedToAnotherZoneIsAColdCopyNotAGap(t *testing.T) {
	// 5 早先合出去了:它在源库的行是冷副本,映射指向别区 —— 有归属,不算缺口。
	rows := sliceSource{1, 2, 5}
	if err := guardPlayerSet(context.Background(), rows, fakeMapping(1, 2, 5), []uint64{1, 2}, guardOptions(), false); err != nil {
		t.Fatalf("a row whose mapping points at another zone must not block the merge: %v", err)
	}
}

func TestGuardPlayerSet_EmptyAndExpectedCountStillEnforced(t *testing.T) {
	o := guardOptions()
	if err := guardPlayerSet(context.Background(), sliceSource{}, fakeMapping(), nil, o, false); err == nil ||
		!strings.Contains(err.Error(), "silent no-op") {
		t.Fatalf("an empty player set must still be refused: %v", err)
	}
	o.expectedSrcPlayers = 3
	if err := guardPlayerSet(context.Background(), sliceSource{1, 2}, fakeMapping(1, 2), []uint64{1, 2}, o, false); err == nil {
		t.Fatal("a count drift since the T-1 rehearsal must still abort")
	}
	// 续跑不比人数(清单里的人数就是首跑时守过的那个)。
	if err := guardPlayerSet(context.Background(), sliceSource{1, 2}, fakeMapping(1, 2), []uint64{1, 2}, o, true); err != nil {
		t.Fatalf("a resumed run must not re-check -expected-src-players: %v", err)
	}
}

func TestFindUnmappedPlayers_PagesThroughEverythingAndCapsTheSample(t *testing.T) {
	// 多于一页(backfillBatchSize)且全部无映射:计数要全,样本要封顶。
	n := backfillBatchSize + 7
	rows := make(sliceSource, n)
	for i := range rows {
		rows[i] = uint64(i + 1)
	}
	scan, err := findUnmappedPlayers(context.Background(), rows, fakeMapping())
	if err != nil {
		t.Fatal(err)
	}
	if scan.Rows != n || scan.Unmapped != n {
		t.Fatalf("scan = %+v, want rows=unmapped=%d", scan, n)
	}
	if len(scan.Sample) != unmappedSampleSize || scan.Sample[0] != 1 {
		t.Fatalf("sample = %v, want the first %d ids", scan.Sample, unmappedSampleSize)
	}
}

func TestFindUnmappedPlayers_LookupErrorsAndShortAnswersFailClosed(t *testing.T) {
	boom := errors.New("mapping redis down")
	failing := func(context.Context, []uint64) ([]bool, error) { return nil, boom }
	if _, err := findUnmappedPlayers(context.Background(), sliceSource{1}, failing); !errors.Is(err, boom) {
		t.Fatalf("a lookup error must fail the guard, got %v", err)
	}
	short := func(context.Context, []uint64) ([]bool, error) { return []bool{true}, nil }
	if _, err := findUnmappedPlayers(context.Background(), sliceSource{1, 2}, short); err == nil {
		t.Fatal("an answer shorter than the batch must not be read as 'all mapped'")
	}
}

// ── A2:改映射按清单逐键 CAS ─────────────────────────────────

func TestClassifyMappingValue_MirrorsTheCASScript(t *testing.T) {
	// 与 remapMappingScript 的返回码逐一对应:dry-run 与 apply 必须按同一口径分类。
	for _, c := range []struct {
		cur     string
		present bool
		want    mappingOutcome
	}{
		{"", false, mappingMissing},
		{"901", true, mappingChanged},
		{"902", true, mappingAlready},
		{"903", true, mappingForeign},
		{"", true, mappingForeign}, // 空串值不是 src 也不是 dst
	} {
		if got := classifyMappingValue(c.cur, c.present, "901", "902"); got != c.want {
			t.Errorf("classify(%q, present=%v) = %d, want %d", c.cur, c.present, got, c.want)
		}
	}
	// 返回码是与 Lua 共享的契约,字面量不能漂移(脚本本身对真 Redis 的行为在 TestIT_Gap_RemapPlayerMapping)。
	if mappingMissing != 0 || mappingChanged != 1 || mappingAlready != 2 || mappingForeign != 3 {
		t.Fatal("mappingOutcome values drifted from remapMappingScript's return codes")
	}
}

func TestMappingRemapReport_AlreadyPlusChangedMustEqualTheManifest(t *testing.T) {
	var ok mappingRemapReport
	ok.add(1, mappingChanged)
	ok.add(2, mappingAlready) // 续跑:上次已改
	if err := ok.complete(2); err != nil {
		t.Fatalf("changed + already == manifest must pass: %v", err)
	}

	var bad mappingRemapReport
	bad.add(1, mappingChanged)
	bad.add(2, mappingMissing)
	bad.add(3, mappingForeign)
	err := bad.complete(3)
	if err == nil {
		t.Fatal("a manifest player whose key is missing or points at a third zone must block the step")
	}
	for _, want := range []string{"only 1 of the 3", "1 have no player:zone key", "1 map to a third zone", "[2 3]"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestUint64sNotIn(t *testing.T) {
	got := uint64sNotIn([]uint64{1, 2, 3, 4}, []uint64{2, 4, 9})
	if !reflect.DeepEqual(got, []uint64{1, 3}) {
		t.Fatalf("got %v, want [1 3]", got)
	}
	if got := uint64sNotIn(nil, []uint64{1}); got != nil {
		t.Fatalf("empty input produced %v", got)
	}
}

func TestPlayerZoneKey(t *testing.T) {
	if got := playerZoneKey(90002); got != "player:zone:90002" {
		t.Fatalf("got %q", got)
	}
}

// ── A3:dry-run 不写 -manifest-path ──────────────────────────

func TestDryRunManifestPath(t *testing.T) {
	if got := dryRunManifestPath("merge_901_to_902.json"); got != "merge_901_to_902.json.dryrun.json" {
		t.Fatalf("got %q", got)
	}
}

func TestLoadRunManifest_RefusesDryRunPreviews(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.json")
	m := newMergeManifest("run-1", 901, 902, "op", time.Now())
	m.PlayerIDs = []uint64{1}
	m.DryRun = true
	// (a) 预览被改名放到了 -manifest-path 上:按内容里的 dry_run 拒。
	if err := saveManifest(path, m); err != nil {
		t.Fatal(err)
	}
	if got, err := loadRunManifest(path); err == nil || got != nil {
		t.Fatalf("a dry-run preview must never be consumed, got (%v, %v)", got, err)
	}
	// (b) 路径带预览后缀:哪怕文件还不存在也拒(后缀保留给预览)。
	if _, err := loadRunManifest(dryRunManifestPath(filepath.Join(dir, "absent.json"))); err == nil {
		t.Fatal("a -manifest-path with the preview suffix must be refused")
	}
	// (c) 真清单照常读出;不存在仍是 (nil, nil)(首跑的正常状态)。
	m.DryRun = false
	if err := saveManifest(path, m); err != nil {
		t.Fatal(err)
	}
	if got, err := loadRunManifest(path); err != nil || got == nil || got.RunID != "run-1" {
		t.Fatalf("a real manifest must load: (%v, %v)", got, err)
	}
	if got, err := loadRunManifest(filepath.Join(dir, "nope.json")); err != nil || got != nil {
		t.Fatalf("a missing manifest is not an error: (%v, %v)", got, err)
	}
}

func TestManifestDryRunFieldIsOptionalOnDisk(t *testing.T) {
	// 升级前的清单没有 dry_run:照常读出、为 false(不升 manifestVersion)。
	path := filepath.Join(t.TempDir(), "old.json")
	raw, _ := json.Marshal(map[string]any{"version": manifestVersion, "run_id": "r-old", "source_zone": 901, "target_zone": 902})
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := loadRunManifest(path)
	if err != nil || m == nil || m.DryRun {
		t.Fatalf("an old manifest must load as a real one: (%+v, %v)", m, err)
	}
	// 真清单落盘时不写这个字段(omitempty),旧版工具读它也不受影响。
	out := filepath.Join(t.TempDir(), "new.json")
	if err := saveManifest(out, newMergeManifest("r", 901, 902, "op", time.Now())); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if strings.Contains(string(b), "dry_run") {
		t.Errorf("a real manifest must not carry dry_run: %s", b)
	}
}

// ── A4:撞号预检 / 续跑认逐列相同 ────────────────────────────

func TestDecideTableCopy(t *testing.T) {
	for _, c := range []struct {
		name                string
		policy              existingTargetRows
		src, dst, identical int
		want                tableCopyAction
		wantErr             bool
		wantErrContains     string
	}{
		{name: "nothing anywhere", policy: refuseExistingTargetRows, want: tableNothingToCopy},
		{name: "fresh, target empty", policy: refuseExistingTargetRows, src: 3, want: tableNeedsCopy},
		{name: "resume, target empty (table not reached last time)", policy: acceptIdenticalTargetRows, src: 3, want: tableNeedsCopy},
		{name: "fresh, target already holds rows", policy: refuseExistingTargetRows, src: 3, dst: 3, identical: 3,
			wantErr: true, wantErrContains: "fresh run"},
		{name: "resume, byte-identical copy", policy: acceptIdenticalTargetRows, src: 3, dst: 3, identical: 3, want: tableAlreadyCopied},
		{name: "resume, a target row changed after the copy", policy: acceptIdenticalTargetRows, src: 3, dst: 3, identical: 2,
			wantErr: true, wantErrContains: "only 2 of the 3"},
		{name: "resume, target-only row (source has no row for it)", policy: acceptIdenticalTargetRows, src: 2, dst: 3, identical: 2,
			wantErr: true, wantErrContains: "source has 2"},
		{name: "resume, target holds rows the source lost", policy: acceptIdenticalTargetRows, src: 0, dst: 1,
			wantErr: true},
		{name: "resume, partial copy (impossible for one-tx tables)", policy: acceptIdenticalTargetRows, src: 3, dst: 2, identical: 2,
			wantErr: true},
	} {
		got, err := decideTableCopy(c.policy, c.src, c.dst, c.identical)
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: want a refusal, got action %d", c.name, got)
			} else if c.wantErrContains != "" && !strings.Contains(err.Error(), c.wantErrContains) {
				t.Errorf("%s: error %q does not contain %q", c.name, err, c.wantErrContains)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: got (%d, %v), want (%d, nil)", c.name, got, err, c.want)
		}
	}
}

func TestIdenticalRowCondition(t *testing.T) {
	got := identicalRowCondition([]string{"player_id", "bag"})
	if want := "d.`player_id` <=> s.`player_id` AND d.`bag` <=> s.`bag`"; got != want {
		t.Fatalf("got %q, want %q (NULL-safe per column, both sides)", got, want)
	}
}

// ── A5:续跑时表集合变化即拒绝 ───────────────────────────────

func TestCheckResumeTables(t *testing.T) {
	recorded := []string{"player_centre_database", "player_database", "player_database_1"}
	if err := checkResumeTables(recorded, []string{"player_database", "player_database_1", "player_centre_database"}); err != nil {
		t.Fatalf("the same set in a different order must pass: %v", err)
	}
	if err := checkResumeTables(nil, []string{"player_database"}); err != nil {
		t.Fatalf("a manifest without recorded tables takes the discovered set: %v", err)
	}
	err := checkResumeTables(recorded, []string{"player_centre_database", "player_database", "player_database_2"})
	if err == nil {
		t.Fatal("a changed table set must refuse the resume")
	}
	for _, want := range []string{"only_in_manifest=[player_database_1]", "only_discovered_now=[player_database_2]"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// ── A6:-verify-merged 逐 id 核对 ────────────────────────────

func TestLoadVerifyManifest(t *testing.T) {
	if _, err := loadVerifyManifest("", 901, 902); err == nil || !strings.Contains(err.Error(), "-manifest-path") {
		t.Fatalf("no -manifest-path must be refused with a pointer to the flag: %v", err)
	}
	dir := t.TempDir()
	if _, err := loadVerifyManifest(filepath.Join(dir, "absent.json"), 901, 902); err == nil {
		t.Fatal("a missing manifest cannot back a per-player verification")
	}
	path := filepath.Join(dir, "m.json")
	if err := saveManifest(path, newMergeManifest("r", 901, 902, "op", time.Now())); err != nil {
		t.Fatal(err)
	}
	if _, err := loadVerifyManifest(path, 903, 902); err == nil {
		t.Fatal("a manifest for another zone pair must be refused")
	}
	if m, err := loadVerifyManifest(path, 901, 902); err != nil || m == nil {
		t.Fatalf("the right manifest must load: (%v, %v)", m, err)
	}
}

func TestVerifyManifestChecks_WithoutAManifestAreInfra(t *testing.T) {
	cfg := auditConfig{src: 901, dst: 902, manifestErr: errors.New("manifest x does not exist")}
	for _, fn := range []func(context.Context, auditConfig) ResourceAudit{verifyManifestMapping, verifyManifestRows} {
		r := fn(context.Background(), cfg)
		if r.Severity != "block" || !isInfraAudit(r) || !strings.Contains(r.Notes, "does not exist") {
			t.Errorf("%s without a manifest must be an INFRA block carrying the cause, got %+v", r.Name, r)
		}
	}
	// 连错误都没有(调用方忘了读):同样 INFRA,不能当作「没有玩家要核对」。
	r := verifyManifestMapping(context.Background(), auditConfig{src: 901, dst: 902})
	if !isInfraAudit(r) {
		t.Errorf("no manifest at all must be INFRA, got %+v", r)
	}
}

func TestVerifyManifestMapping_ManifestCountMustMatchTheRehearsal(t *testing.T) {
	m := newMergeManifest("r", 901, 902, "op", time.Now())
	m.PlayerIDs = []uint64{1, 2}
	// 句柄故意给 nil:人数对不上在任何查询之前就判出来,是真问题(block),不能被说成 INFRA。
	r := verifyManifestMapping(context.Background(), auditConfig{src: 901, dst: 902, manifest: m, expectedSrcPlayers: 3})
	if r.Severity != "block" || isInfraAudit(r) || !strings.Contains(r.Notes, "is this the manifest") {
		t.Fatalf("a manifest whose player count differs from the T-1 record must block, got %+v", r)
	}
	// 人数对得上、只是没有 mapping 句柄:查不成 = INFRA。
	r = verifyManifestMapping(context.Background(), auditConfig{src: 901, dst: 902, manifest: m, expectedSrcPlayers: 2})
	if !isInfraAudit(r) {
		t.Fatalf("no mapping handle must be INFRA, got %+v", r)
	}
}

func TestVerifyManifestRows_SkippedRowsStepIsNotVerified(t *testing.T) {
	m := newMergeManifest("r", 901, 902, "op", time.Now())
	m.PlayerIDs = []uint64{1, 2}
	r := verifyManifestRows(context.Background(), auditConfig{src: 901, dst: 902, manifest: m})
	if r.Severity != "warn" || isInfraAudit(r) || !strings.Contains(r.Notes, "NOT VERIFIED") {
		t.Fatalf("a manifest without step %s must be reported as NOT VERIFIED (warn), got %+v", stepPlayerRows, r)
	}
	// 步骤记了完成、却没有 MySQL 句柄:查不成 = INFRA。
	m.markStep(stepPlayerRows, "x")
	if r := verifyManifestRows(context.Background(), auditConfig{src: 901, dst: 902, manifest: m}); !isInfraAudit(r) {
		t.Fatalf("no MySQL handle must be INFRA, got %+v", r)
	}
}

func TestMainRowCheckFor_NoRecordLooksAtTheTargetZoneMainTable(t *testing.T) {
	// copy 模式拷过去的人没有落点记录:判据是目标区 zone 库(第一段 A6 的口径)。有记录的人看记录指向的库
	// (player-storage-placement.md §10:pin 模式钉住的、copy 模式因已有记录而没拷的),见 TestExpectedMainRowStore。
	if got := mainRowCheckFor(newMergeManifest("r", 901, 902, "op", time.Now()), 902).where; !strings.Contains(got, "zone_902_db") {
		t.Fatalf("row check describes %q, want it to name zone_902_db for players without a placement record", got)
	}
	if store, ok := expectedMainRowStore(placementRead{}, 902); !ok || store != 902 {
		t.Fatalf("no record → (%d,%v), want the target zone 902", store, ok)
	}
}

func TestRunAuditMode_VerifyIncludesThePerPlayerChecksAndDropsTheLowerBound(t *testing.T) {
	names := map[string]bool{}
	for _, r := range runAuditMode(context.Background(), auditConfig{verify: true, tradeSchema: defaultTradeSchema,
		guildSchema: defaultGuildSchema, src: 901, dst: 902}) {
		names[r.Name] = true
	}
	for _, want := range []string{"verify:mapping_src", "verify:manifest_mapping", "verify:manifest_rows"} {
		if !names[want] {
			t.Errorf("-verify-merged must include %s (got %v)", want, names)
		}
	}
	if names["verify:target_zone_rows"] {
		t.Error("the lower-bound verify:target_zone_rows (target natives count toward it) must be gone")
	}
}

// ── A8:步骤 4 以锁内重读为准 ────────────────────────────────

func TestChooseRankWrite(t *testing.T) {
	snapshot := []manifestRankMember{{Member: "11", Score: 90}}
	reread := []manifestRankMember{{Member: "11", Score: 100}, {Member: "12", Score: 50}}
	for _, written := range []bool{true, false} {
		toWrite, toRecord, gone := chooseRankWrite(reread, snapshot, written)
		if gone || !reflect.DeepEqual(toWrite, reread) || !reflect.DeepEqual(toRecord, reread) {
			t.Fatalf("written=%v: a non-empty re-read must win over the stale snapshot: (%v, %v, %v)", written, toWrite, toRecord, gone)
		}
	}
	// 源榜已不在、快照是上一次写入之前落盘的那一份(续跑):不写(不回退目标榜分数),撤销依据沿用快照。
	toWrite, toRecord, gone := chooseRankWrite(nil, snapshot, true)
	if !gone || len(toWrite) != 0 || !reflect.DeepEqual(toRecord, snapshot) {
		t.Fatalf("resume after the previous MULTI/EXEC: (%v, %v, %v), want nothing written and the snapshot kept", toWrite, toRecord, gone)
	}
	// 源榜已不在、快照只是清单阶段读的(guild 服合法清空了源榜):不写(不复活已解散的公会),撤销依据清空。
	toWrite, toRecord, gone = chooseRankWrite(nil, snapshot, false)
	if !gone || len(toWrite) != 0 || len(toRecord) != 0 {
		t.Fatalf("source rank emptied by the guild service: (%v, %v, %v), want nothing written and nothing recorded", toWrite, toRecord, gone)
	}
}

func TestManifestRankMembersUnwrittenIsOptionalOnDisk(t *testing.T) {
	// 旧清单没有这个字段 = false = 「快照可能写过」,撤销依据按旧行为保留。
	var legacy mergeManifest
	if err := json.Unmarshal([]byte(`{"version":1,"rank_members":[{"member":"11","score":1}]}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.RankMembersUnwritten {
		t.Fatal("a manifest without rank_members_unwritten must read as possibly written")
	}
	raw, err := json.Marshal(&mergeManifest{Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "rank_members_unwritten") {
		t.Fatalf("false must be omitted on disk: %s", raw)
	}
}

// ── A9:guild_member 审计不再吞错 ─────────────────────────────

func TestAuditGuildMembers_SkippedGuildIsAWarningNotAPass(t *testing.T) {
	r := auditGuildMembers(context.Background(), auditConfig{skipGuild: true, guildSchema: defaultGuildSchema})
	if r.Severity != "warn" || isInfraAudit(r) || !strings.Contains(r.Notes, "SKIPPED") {
		t.Fatalf("an explicit guild skip must be reported as SKIPPED (warn), got %+v", r)
	}
}

// 没部署帮会的环境(合服带 -skip-guild-mysql -skip-guild-rank):verify:guild_zone / verify:guild_rank 与
// guild_member 同一口径报 SKIPPED(warn),不再因为查不到表恒报 INFRA、让 -verify-merged 永远 exit 2。
func TestVerifyGuildChecks_SkippedGuildIsAWarningNotInfra(t *testing.T) {
	cfg := auditConfig{skipGuild: true, guildSchema: defaultGuildSchema, src: 901, dst: 902}
	for _, r := range []ResourceAudit{
		verifyGuildZoneDrained(context.Background(), cfg),
		verifyGuildRankZSets(context.Background(), cfg),
	} {
		if r.Severity != "warn" || isInfraAudit(r) || !strings.Contains(r.Notes, "SKIPPED") {
			t.Errorf("%s: an explicit guild skip must be SKIPPED (warn), got %+v", r.Name, r)
		}
	}
	// 没声明跳过、也没有句柄:照旧 INFRA。
	cfg.skipGuild = false
	for _, r := range []ResourceAudit{
		verifyGuildZoneDrained(context.Background(), cfg),
		verifyGuildRankZSets(context.Background(), cfg),
	} {
		if !isInfraAudit(r) {
			t.Errorf("%s: without the skip an unchecked guild check must stay INFRA, got %+v", r.Name, r)
		}
	}
}

func TestAuditGuildMembers_NoHandleIsInfraWhenNotSkipped(t *testing.T) {
	r := auditGuildMembers(context.Background(), auditConfig{guildSchema: defaultGuildSchema})
	if r.Severity != "block" || !isInfraAudit(r) {
		t.Fatalf("an unchecked guild_member must be INFRA unless the guild is declared absent, got %+v", r)
	}
}

// ── A10:merge:merged_into:{src} ─────────────────────────────

// A10:合走标记在清单落盘之前(第一次写之前)判定,不再等步骤 5 —— 那时步骤 1~4 都已提交。
func TestMergedIntoConflict(t *testing.T) {
	sameDst := &mergedIntoValue{Dst: 902, RunID: "r"}
	otherDst := &mergedIntoValue{Dst: 903, RunID: "r"}
	boom := errors.New("redis down")
	for _, c := range []struct {
		name            string
		srcCur          *mergedIntoValue
		srcErr          error
		dstCur          *mergedIntoValue
		dstErr          error
		wantErr, substr string
	}{
		{name: "no marker anywhere"},
		{name: "resume / re-merge into the same dst", srcCur: sameDst},
		{name: "source already merged elsewhere", srcCur: otherDst, wantErr: "y", substr: "already merged into zone 903"},
		{name: "target itself merged away", dstCur: otherDst, wantErr: "y", substr: "retired zone"},
		{name: "source marker unreadable", srcErr: boom, wantErr: "y", substr: "cannot prove zone 901"},
		{name: "target marker unreadable", dstErr: boom, wantErr: "y", substr: "cannot prove target zone 902"},
	} {
		err := mergedIntoConflict(901, 902, c.srcCur, "raw", c.srcErr, c.dstCur, "raw", c.dstErr)
		if (err != nil) != (c.wantErr != "") {
			t.Errorf("%s: err=%v", c.name, err)
			continue
		}
		if err != nil && !strings.Contains(err.Error(), c.substr) {
			t.Errorf("%s: err=%v, want it to mention %q", c.name, err, c.substr)
		}
	}
}

func TestMergedIntoKeyAndValueShape(t *testing.T) {
	if got := mergedIntoKey(901); got != "merge:merged_into:901" {
		t.Fatalf("got %q", got)
	}
	raw, err := json.Marshal(mergedIntoValue{Dst: 902, RunID: "901-902-x"})
	if err != nil {
		t.Fatal(err)
	}
	// 契约:值是 JSON {dst, run_id}(回填的拒绝文案原样打出它,撤销按它认领)。
	if string(raw) != `{"dst":902,"run_id":"901-902-x"}` {
		t.Fatalf("merged_into value = %s", raw)
	}
	var back mergedIntoValue
	if err := json.Unmarshal(raw, &back); err != nil || back.Dst != 902 || back.RunID != "901-902-x" {
		t.Fatalf("round trip = (%+v, %v)", back, err)
	}
	// 与围栏同在 merge: 命名空间,但不是围栏:读者 EXISTS merge:in_progress:* 不能把它当围栏。
	if strings.HasPrefix(mergedIntoKey(901), "merge:in_progress:") {
		t.Fatal("the merged_into marker must not look like a merge fence")
	}
}
