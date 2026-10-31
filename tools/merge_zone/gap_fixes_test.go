package main

// 合服工具的纯单测,不连库。与真库交互的部分(SQL 形状的执行计划、端到端)在 integration_test.go
// (build tag merge_integration)。单行事务在 lock_order_test.go,别名合服的流程在 alias_merge_test.go。
//
// merge_zone 的 go.mod 里没有 miniredis / sqlmock(也没有 vendor),按约定不为了测试引新依赖:
// Redis / SQL 边界一律经接缝注入替身。

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

// ── A3:dry-run 不写 -manifest-path ──────────────────────────

func TestDryRunManifestPath(t *testing.T) {
	if got := dryRunManifestPath("merge_901_to_902.json"); got != "merge_901_to_902.json.dryrun.json" {
		t.Fatalf("got %q", got)
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

// ── A5:续跑时表集合变化即拒绝 ───────────────────────────────

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
	// 契约:值是 JSON {dst, run_id}(撤销按它认领)。
	if string(raw) != `{"dst":902,"run_id":"901-902-x"}` {
		t.Fatalf("merged_into value = %s", raw)
	}
	var back mergedIntoValue
	if err := json.Unmarshal(raw, &back); err != nil || back.Dst != 902 || back.RunID != "901-902-x" {
		t.Fatalf("round trip = (%+v, %v)", back, err)
	}
}

func TestLoadRunManifest_RefusesDryRunPreviews(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.json")
	m := newMergeManifest("run-1", 901, 902, "op", time.Now())
	m.GuildIDs = []uint64{1}
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

func TestVerifyMergedInto_WithoutAManifestIsInfra(t *testing.T) {
	r := verifyMergedInto(context.Background(), auditConfig{src: 901, dst: 902, manifestErr: errors.New("manifest x does not exist")})
	if r.Severity != "block" || !isInfraAudit(r) || !strings.Contains(r.Notes, "does not exist") {
		t.Errorf("without a manifest it must be an INFRA block carrying the cause, got %+v", r)
	}
	// 连错误都没有(调用方忘了读):同样 INFRA,不能当作通过。
	r = verifyMergedInto(context.Background(), auditConfig{src: 901, dst: 902})
	if !isInfraAudit(r) {
		t.Errorf("no manifest at all must be INFRA, got %+v", r)
	}
}
