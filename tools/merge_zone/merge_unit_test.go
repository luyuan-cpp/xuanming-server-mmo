package main

// 纯函数单测。Redis / MySQL 相关的行为在 integration_test.go(build tag
// merge_integration)里对着真实的 127.0.0.1 实例跑。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	pb "proto/common/database"
	"shared/drainproof"
)

func TestMergeRecoveryAuditRequiresCompleteActualToolFiles(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "actual-source")
	raw := []byte("真实读取的审核测试源")
	if err := os.WriteFile(source, raw, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	plan := &pb.SqlWriterMaintenanceReceipt_Plan{RunId: bytes.Repeat([]byte{1}, 16)}
	publication := &pb.SqlWriterMaintenanceReceipt_PublicationEvidence{}
	for i := byte(2); i < 4; i++ {
		seed := &pb.SqlWriterMaintenanceReceipt_IssuerClosure{IssuerId: bytes.Repeat([]byte{i}, 16), InstanceId: bytes.Repeat([]byte{i + 2}, 16),
			LogicalRole: "MAINTENANCE", PermanentRevivalFenceId: bytes.Repeat([]byte{i + 4}, 16), SourceEvidence: []*pb.SqlWriterMaintenanceReceipt_SourcePin{{AbsolutePath: source, Sha256: sum[:]}}}
		plan.MaintenanceIssuers = append(plan.MaintenanceIssuers, seed)
		session := drainproof.New()
		finish, err := session.Enter()
		if err != nil {
			t.Fatal(err)
		}
		finish()
		session.CloseAdmission()
		closed, err := session.Closure(context.Background(), seed)
		if err != nil {
			t.Fatal(err)
		}
		publication.LogicalRoles = append(publication.LogicalRoles, closed)
		raw, err := protojson.Marshal(closed)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, fmt.Sprintf("tool-%d.json", i))
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		publication.SourceEvidence = append(publication.SourceEvidence, &pb.SqlWriterMaintenanceReceipt_SourcePin{AbsolutePath: path, Sha256: sum[:]})
	}
	inventory, err := (proto.MarshalOptions{Deterministic: true}).Marshal(&pb.SqlWriterMaintenanceReceipt_PublicationEvidence{LogicalRoles: plan.MaintenanceIssuers})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(inventory)
	publication.IssuerInventoryDigest = digest[:]
	planRaw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	frozen := &pb.SqlWriterMaintenanceReceipt{RunId: plan.RunId, PlanProto: planRaw, Kind: pb.SqlWriterMaintenanceReceipt_FREEZE, Phase: pb.SqlWriterMaintenanceReceipt_FROZEN, RunPhase: pb.SqlWriterDrainRun_SQL_FROZEN, CommittedMs: 1}
	if err := verifyClosedMaintenanceForAudit(plan, frozen, publication); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*pb.SqlWriterMaintenanceReceipt_PublicationEvidence){
		func(p *pb.SqlWriterMaintenanceReceipt_PublicationEvidence) { p.LogicalRoles = p.LogicalRoles[:1] },
		func(p *pb.SqlWriterMaintenanceReceipt_PublicationEvidence) {
			p.LogicalRoles[1].InstanceId = bytes.Repeat([]byte{9}, 16)
		},
		func(p *pb.SqlWriterMaintenanceReceipt_PublicationEvidence) {
			p.LogicalRoles[1].UnresolvedPublications = 1
		},
		func(p *pb.SqlWriterMaintenanceReceipt_PublicationEvidence) { p.SourceEvidence = p.SourceEvidence[:1] },
		func(p *pb.SqlWriterMaintenanceReceipt_PublicationEvidence) { p.SourceEvidence[1] = p.SourceEvidence[0] },
	} {
		bad := proto.Clone(publication).(*pb.SqlWriterMaintenanceReceipt_PublicationEvidence)
		mutate(bad)
		if err := verifyClosedMaintenanceForAudit(plan, frozen, bad); err == nil {
			t.Fatal("全集/boot/UNKNOWN/实际文件缺口不得开始业务审核")
		}
	}
	if err := os.WriteFile(source, []byte("源漂移"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyClosedMaintenanceForAudit(plan, frozen, publication); err == nil {
		t.Fatal("实际源pin漂移必须拒绝")
	}
}

func TestMergeRecoveryAuditRejectsUnknownSkippedAndMissingRows(t *testing.T) {
	rows := []ResourceAudit{}
	for _, name := range mergeRecoveryRowNames() {
		rows = append(rows, ResourceAudit{Name: name, Severity: "info", UniqueScope: "global"})
	}
	if err := validateMergeRecoveryRows(rows); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func([]ResourceAudit) []ResourceAudit{
		func(r []ResourceAudit) []ResourceAudit { return r[:len(r)-1] },
		func(r []ResourceAudit) []ResourceAudit { r[1] = r[0]; return r },
		func(r []ResourceAudit) []ResourceAudit { r[1].Severity = "warn"; return r },
		func(r []ResourceAudit) []ResourceAudit { r[1].Notes = "SKIPPED"; return r },
		func(r []ResourceAudit) []ResourceAudit { r[1].Notes = "UNKNOWN"; return r },
		func(r []ResourceAudit) []ResourceAudit { r[1].Notes = "NOT VERIFIED"; return r },
		func(r []ResourceAudit) []ResourceAudit { r[1].SourceCount = -1; return r },
		func(r []ResourceAudit) []ResourceAudit { r[1].ConflictCount = 1; return r },
	} {
		bad := mutate(append([]ResourceAudit(nil), rows...))
		if err := validateMergeRecoveryRows(bad); err == nil {
			t.Fatal("不完整审核不能解冻")
		}
	}
}

func TestMaintenanceWriteNeedsFixedToolOwner(t *testing.T) {
	if err := checkMaintenanceTool(context.Background()); !errors.Is(err, drainproof.ErrHold) {
		t.Fatalf("NORMAL冻结或空上下文不能冒TOOL: %v", err)
	}
	ctx, owner := testMaintenanceContext()
	if err := checkMaintenanceTool(ctx); err != nil {
		t.Fatal(err)
	}
	owner.session.CloseAdmission()
	calls := 0
	owner.check = func(context.Context) error { calls++; return nil }
	if err := checkMaintenanceTool(ctx); !errors.Is(err, drainproof.ErrClosed) || calls != 0 {
		t.Fatalf("永久闭合后必须在实际读取/写入前拒绝: %v calls=%d", err, calls)
	}
}

func TestMaintenanceStrongReadFailurePermanentlyHolds(t *testing.T) {
	ctx, owner := testMaintenanceContext()
	readErr := errors.New("SQL_FROZEN强读未知")
	owner.check = func(context.Context) error { return readErr }
	if err := checkMaintenanceTool(ctx); !errors.Is(err, readErr) {
		t.Fatal(err)
	}
	owner.check = func(context.Context) error { return nil }
	if err := checkMaintenanceTool(ctx); err != nil {
		t.Fatal(err)
	}
	owner.session.CloseAdmission()
	if err := owner.session.Wait(ctx); !errors.Is(err, drainproof.ErrHold) {
		t.Fatalf("后续强读成功不能洗掉先前未知: %v", err)
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

// ── 玩家行拷贝的列对齐(03-names.md §3.16 / §3.19)──────────────

func TestAuditPlayerNameConflicts_IsGlobalInfoRow(t *testing.T) {
	// 名字全服唯一(data_service player_name),合服没有重名可撞:这一行只能是 info。
	// 它若变成 block,每次合服审计都会 exit 1,运维只能学会跳过门禁。
	r := auditPlayerNameConflicts(context.Background(), auditConfig{})
	if r.Name != "player.name (global)" || r.UniqueScope != "global" {
		t.Errorf("name/scope = %q/%q, want player.name (global)/global", r.Name, r.UniqueScope)
	}
	if r.Severity != "info" {
		t.Errorf("severity = %q, want info", r.Severity)
	}
	if r.ConflictCount != 0 {
		t.Errorf("conflicts = %d, want 0 (global uniqueness leaves nothing to collide)", r.ConflictCount)
	}
	if isInfraAudit(r) {
		t.Error("the naming note must not be misread as an INFRA finding")
	}
}

// ── 清单 ─────────────────────────────────────────────────────

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

// ── run_id ───────────────────────────────────────────────────

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

// ── 帮会库名 ─────────────────────────────────────────────────

func TestGuildNamesMatchGuildService(t *testing.T) {
	// merge_zone 是独立 module,不能 import go/guild:库名镜像 go/guild/internal/data.DatabaseName
	// (guild 服的 config.Validate 强制 DSN 库名等于它),表名镜像 proto/guild/guild_db.proto 的
	// OptionTableName。漂移 = 改写落到不存在的表上,或更糟:落到另一个库的同名表上。
	if defaultGuildSchema != "mmorpg_guild" {
		t.Fatalf("defaultGuildSchema drifted from go/guild data.DatabaseName: %q", defaultGuildSchema)
	}
	if guildTable != "guild" || guildMemberTable != "guild_member" {
		t.Fatalf("guild table names drifted from guild_db.proto: %q / %q", guildTable, guildMemberTable)
	}
	// 重名探测比的是唯一键那一列(uk_guild = name_norm),不是展示名。
	if guildNameNormColumn != "name_norm" {
		t.Fatalf("guild unique-key column drifted from guild_db.proto: %q", guildNameNormColumn)
	}
	if got := guildQualified(defaultGuildSchema, guildTable); got != "mmorpg_guild.guild" {
		t.Fatalf("got %q", got)
	}
}

func TestValidateGuildSchemaName(t *testing.T) {
	for _, ok := range []string{"mmorpg_guild", "merge_zone_it_guild", "Guild1", strings.Repeat("a", 64)} {
		if err := validateGuildSchemaName(ok); err != nil {
			t.Errorf("%q should be accepted: %v", ok, err)
		}
	}
	// 库名直接拼进 SQL:任何带点、引号、空白、分号、连字符的都必须拒绝。
	for _, bad := range []string{
		"", "mmorpg_guild.guild", "x;DROP DATABASE y", "a b", "`mmorpg_guild`",
		"mmorpg-guild", "mmorpg_guild\n", strings.Repeat("a", 65),
	} {
		if err := validateGuildSchemaName(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
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
	// 这三个常量若漂移,合服会安静地扫错库。数值来源见 main.go 顶部注释。
	// (friend 的 DB 3 一条已随 friend:online 退役删除,2026-09-18 friend 移植 F3。)
	cases := []struct {
		name string
		got  int
		want int
	}{
		// go-zero RedisConf 没有 DB 字段 → mapping Redis 恒为 DB 0(见 main.go 注释)。
		{"mapping (data_service MappingRedis, go-zero 无 DB 字段)", defaultMappingRedisDB, 0},
		{"guild (guild.yaml RedisClient.DB;2026-10-23 由 2 改为 0)", defaultGuildRedisDB, 0},
		{"shared/login (login.yaml Node.RedisClient.DB)", defaultSharedRedisDB, 0},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %d want %d", c.name, c.got, c.want)
		}
	}
}

func TestReadOnlyPreflightDoesNotNeedWriteGrantButStillRequiresFacts(t *testing.T) {
	err := runPreflight(context.Background(), preflightDeps{}, preflightParams{readOnly: true, zone: 1, scope: "source"})
	if err == nil || errors.Is(err, errNormalFreezeProof) || errors.Is(err, drainproof.ErrHold) || !strings.Contains(err.Error(), "no scene_manager Redis handle") {
		t.Fatalf("只读预演应进入真实事实检查、缺句柄仍拒绝: %v", err)
	}
}
func TestWritePreflightStillRejectsMissingNormalFreeze(t *testing.T) {
	err := runPreflight(context.Background(), preflightDeps{}, preflightParams{zone: 1, scope: "source"})
	if !errors.Is(err, errNormalFreezeProof) {
		t.Fatalf("默认真实写不能借只读路径免除冻结: %v", err)
	}
	ctx, owner := testMaintenanceContext()
	err = runPreflight(ctx, preflightDeps{}, preflightParams{zone: 1, scope: "source"})
	if !errors.Is(err, errNormalFreezeProof) {
		t.Fatalf("只有TOOL没有正常冻结仍须拒绝: %v", err)
	}
	owner.session.CloseAdmission()
	if !errors.Is(owner.session.Wait(context.Background()), drainproof.ErrHold) {
		t.Fatal("写路径拒绝未留下HOLD")
	}
}

func TestManifestRoundTripAndStepFlags(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.json")
	now := time.Date(2026, 9, 8, 3, 0, 0, 0, time.UTC)
	m := newMergeManifest("run-1", 901, 902, "host/pid=1", now)
	m.GuildIDs = []uint64{9}
	m.TradeListingIDs = []uint64{7}
	m.RankMembers = []manifestRankMember{{Member: "9", Score: 12.5}}
	m.markStep(stepMergedInto, "zone_merged_into(901)=902")

	if err := saveManifest(path, m); err != nil {
		t.Fatal(err)
	}
	got, err := loadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.stepDone(stepMergedInto) {
		t.Error("merged_into should be marked done")
	}
	if got.stepDone(stepGuildRank) {
		t.Error("guild_rank should not be done")
	}
	if got.RunID != "run-1" || got.SourceZone != 901 || got.TargetZone != 902 {
		t.Errorf("identity lost: %+v", got)
	}
	if !reflect.DeepEqual(got.GuildIDs, m.GuildIDs) || !reflect.DeepEqual(got.TradeListingIDs, m.TradeListingIDs) || !reflect.DeepEqual(got.RankMembers, m.RankMembers) {
		t.Errorf("objects lost: %+v", got)
	}
	// 落盘必须是原子的 —— .tmp 不能留下。
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error(".tmp file left behind")
	}
}

func TestZoneDBName(t *testing.T) {
	if zoneDBName(102) != "zone_102_db" {
		t.Fatal(zoneDBName(102))
	}
}
