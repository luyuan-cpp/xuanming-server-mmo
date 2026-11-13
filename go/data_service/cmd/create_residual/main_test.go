package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"data_service/internal/store"

	"shared/authoritysql"
	"shared/dbplacement"

	"github.com/stretchr/testify/require"
)

// fakeScanKey 是一次扫描的对象:哪一片、扫角色行还是名字行。
type fakeScanKey struct {
	slot   int
	target store.CreateResidualTarget
}

type fakeAuditor struct {
	shards  int
	reports map[fakeScanKey]store.CreateResidualReport
	errs    map[fakeScanKey]error
	scans   []store.CreateResidualScan
}

func (f *fakeAuditor) Shards() int { return f.shards }

func (f *fakeAuditor) Run(_ context.Context, scan store.CreateResidualScan) (store.CreateResidualReport, error) {
	f.scans = append(f.scans, scan)
	key := fakeScanKey{slot: scan.Slot, target: scan.Target}
	report, ok := f.reports[key]
	if !ok {
		report = store.CreateResidualReport{Done: true}
	}
	report.Slot, report.Target = scan.Slot, scan.Target
	return report, f.errs[key]
}

func openFake(a *fakeAuditor, opened *bool) opener {
	return func(context.Context, options) (auditor, func(), error) {
		*opened = true
		return a, func() {}, nil
	}
}

func optionsFixture(mode, repair string) options {
	return options{mode: mode, repair: repair, target: "all", shard: -1, batch: store.CreateResidualDefaultBatch, maxRoles: 5, timeout: time.Minute,
		nameCacheAddr: "127.0.0.1:6379", nameCacheType: "node"}
}

func rolesOf(slot int) fakeScanKey { return fakeScanKey{slot: slot, target: store.ResidualTargetRoles} }
func namesOf(slot int) fakeScanKey { return fakeScanKey{slot: slot, target: store.ResidualTargetNames} }

func TestRejectsBadArgumentsBeforeOpeningTheStore(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*options)
	}{
		{"未知模式", func(o *options) { o.mode = "force" }},
		{"plan 带 repair", func(o *options) { o.repair = "ref" }},
		{"apply 不选种类", func(o *options) { o.mode = "apply" }},
		{"apply 未知种类", func(o *options) { o.mode, o.repair = "apply", "ref,everything" }},
		{"未知扫描对象", func(o *options) { o.target = "everything" }},
		{"after 不指定片", func(o *options) { o.target, o.after = "roles", 7 }},
		{"after 配名字行", func(o *options) { o.shard, o.target, o.after = 0, "names", 7 }},
		{"after 配两样都扫", func(o *options) { o.shard, o.after = 0, 7 }},
		{"after-name 不指定片", func(o *options) { o.target, o.afterName = "names", "alpha" }},
		{"after-name 配角色行", func(o *options) { o.shard, o.target, o.afterName = 0, "roles", "alpha" }},
		{"片号非法", func(o *options) { o.shard = -2 }},
		{"limit 为负", func(o *options) { o.limit = -1 }},
		{"batch 为 0", func(o *options) { o.batch = 0 }},
		{"batch 超上限", func(o *options) { o.batch = store.CreateResidualMaxBatch + 1 }},
		{"没有时限", func(o *options) { o.timeout = 0 }},
		{"角色上限为 0", func(o *options) { o.maxRoles = 0 }},
		{"修名字不给名字缓存", func(o *options) { o.mode, o.repair, o.nameCacheAddr = "apply", "name", "" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := optionsFixture("plan", "")
			c.mutate(&o)
			var out, errOut bytes.Buffer
			opened := false
			code := run(context.Background(), o, &out, &errOut, openFake(&fakeAuditor{shards: 1}, &opened))
			require.Equal(t, 2, code)
			require.False(t, opened, "参数错误不能碰库")
			require.NotEmpty(t, errOut.String())
		})
	}
}

// 用例「名字修复之后马上查得到」的入口一半:修名字必须给名字缓存的地址(修完要删缓存);只修 ref、初始行或只报告不要它。
func TestNameRepairNeedsTheNameCache(t *testing.T) {
	o := optionsFixture("apply", "ref,name")
	o.nameCacheAddr = ""
	var out, errOut bytes.Buffer
	opened := false
	require.Equal(t, 2, run(context.Background(), o, &out, &errOut, openFake(&fakeAuditor{shards: 1}, &opened)))
	require.Contains(t, errOut.String(), "-name-cache-addr")

	for _, o := range []options{optionsFixture("apply", "ref,initial_rows"), optionsFixture("plan", "")} {
		o.nameCacheAddr = ""
		errOut.Reset()
		require.Equal(t, 0, run(context.Background(), o, &out, &errOut, openFake(&fakeAuditor{shards: 1}, &opened)), errOut.String())
	}
}

// plan 扫全部片、每片先扫角色行再扫名字行,一项都不修;没有残留时退出码 0。
func TestPlanScansEveryShardWithoutRepairs(t *testing.T) {
	a := &fakeAuditor{shards: 3, reports: map[fakeScanKey]store.CreateResidualReport{
		rolesOf(0): {Scanned: 2, NextAfter: 40, Done: true}, rolesOf(1): {Scanned: 1, NextAfter: 41, Done: true},
		namesOf(0): {Scanned: 5, NextAfterName: "zeta", Done: true},
	}}
	var out, errOut bytes.Buffer
	opened := false
	code := run(context.Background(), optionsFixture("plan", ""), &out, &errOut, openFake(a, &opened))
	require.Equal(t, 0, code, errOut.String())
	require.Len(t, a.scans, 6)
	for i, scan := range a.scans {
		require.Equal(t, i/2, scan.Slot)
		require.Equal(t, []store.CreateResidualTarget{store.ResidualTargetRoles, store.ResidualTargetNames}[i%2], scan.Target, "每片先角色行、再名字行")
		require.Equal(t, store.CreateResidualRepairs{}, scan.Repair, "plan 不修")
	}
	require.Contains(t, out.String(), "shard shard=0 target=names scanned=5 findings=0 next_after_name=\"zeta\" done=true\n")
	require.Contains(t, out.String(), "summary mode=plan target=all shards=3 scanned=8 findings=0 attention=0 done=true")
}

// apply 只修 -repair 列出的种类;-target 只扫一样时带它的游标接着扫。
func TestApplyRepairsOnlyTheChosenKinds(t *testing.T) {
	a := &fakeAuditor{shards: 2}
	o := optionsFixture("apply", "ref, initial_rows")
	o.shard, o.target, o.after, o.limit, o.batch = 1, "roles", 99, 10, 5
	var out, errOut bytes.Buffer
	opened := false
	require.Equal(t, 0, run(context.Background(), o, &out, &errOut, openFake(a, &opened)), errOut.String())
	require.Equal(t, []store.CreateResidualScan{{Slot: 1, Target: store.ResidualTargetRoles, After: 99, Limit: 10, Batch: 5,
		Repair: store.CreateResidualRepairs{Refs: true, InitialRows: true}}}, a.scans)

	a = &fakeAuditor{shards: 2}
	o = optionsFixture("apply", "name")
	o.shard, o.target, o.afterName = 0, "names", "mike"
	require.Equal(t, 0, run(context.Background(), o, &out, &errOut, openFake(a, &opened)), errOut.String())
	require.Equal(t, []store.CreateResidualScan{{Slot: 0, Target: store.ResidualTargetNames, AfterName: "mike", Batch: store.CreateResidualDefaultBatch,
		Repair: store.CreateResidualRepairs{Names: true}}}, a.scans)
}

// 每一项一行;还要人看的项让退出码非 0。修好的不算,但补回 ref 之后超过角色上限(over_role_cap)算:账号超出上限要人定。
func TestFindingsThatNeedAttentionExitNonZero(t *testing.T) {
	overCap := store.CreateResidualFinding{Slot: 0, PlayerID: 12, Kind: store.ResidualRefMissing, Result: store.ResidualRepaired, Reason: "over_role_cap"}
	repaired := store.CreateResidualFinding{Slot: 0, PlayerID: 13, Kind: store.ResidualRefMissing, Result: store.ResidualRepaired}
	taken := store.CreateResidualFinding{Slot: 0, PlayerID: 21, Kind: store.ResidualNameTaken, Result: store.ResidualManual, Reason: "name_held_by_other", Holder: 22, HolderLive: true, Keeper: 21}
	report := store.CreateResidualReport{Scanned: 3, NextAfter: 30, Done: true, Findings: []store.CreateResidualFinding{overCap, repaired, taken}}
	a := &fakeAuditor{shards: 1, reports: map[fakeScanKey]store.CreateResidualReport{rolesOf(0): report}}
	var out, errOut bytes.Buffer
	opened := false
	require.Equal(t, 1, run(context.Background(), optionsFixture("apply", "ref,name"), &out, &errOut, openFake(a, &opened)))
	require.Contains(t, out.String(), "finding shard=0 player=12 kind=ref_missing result=repaired reason=over_role_cap attention=true\n")
	require.Contains(t, out.String(), "finding shard=0 player=13 kind=ref_missing result=repaired\n")
	require.Contains(t, out.String(), "finding shard=0 player=21 kind=name_taken result=manual reason=name_held_by_other holder=22 holder_live=true keeper=21 attention=true\n")
	require.Contains(t, out.String(), "attention=2")

	report.Findings = []store.CreateResidualFinding{overCap}
	a.reports[rolesOf(0)] = report
	out.Reset()
	require.Equal(t, 1, run(context.Background(), optionsFixture("apply", "ref"), &out, &errOut, openFake(a, &opened)), "只剩超过角色上限的那一项也要人看")
	require.Contains(t, out.String(), "attention=1")

	report.Findings = []store.CreateResidualFinding{repaired}
	a.reports[rolesOf(0)] = report
	out.Reset()
	require.Equal(t, 0, run(context.Background(), optionsFixture("apply", "ref"), &out, &errOut, openFake(a, &opened)), "修好的不算要人看")
}

// 一片扫到一半出错:停下,给出接着扫的位置(带上扫描对象);底层错误可能带连接细节,不输出。
func TestScanErrorStopsWithAResumePointAndNoDetails(t *testing.T) {
	a := &fakeAuditor{
		shards:  3,
		reports: map[fakeScanKey]store.CreateResidualReport{rolesOf(1): {Scanned: 4, NextAfter: 77}},
		errs:    map[fakeScanKey]error{rolesOf(1): errors.New("dial tcp 10.0.0.1:4000: password=private-password-must-not-leak")},
	}
	var out, errOut bytes.Buffer
	opened := false
	require.Equal(t, 1, run(context.Background(), optionsFixture("plan", ""), &out, &errOut, openFake(a, &opened)))
	require.Len(t, a.scans, 3, "第 0 片两样都扫完,第 1 片角色行出错之后不再扫")
	require.Contains(t, errOut.String(), "-shard 1 -target roles -after 77")
	require.NotContains(t, errOut.String()+out.String(), "private-password")

	a = &fakeAuditor{
		shards:  2,
		reports: map[fakeScanKey]store.CreateResidualReport{namesOf(0): {Scanned: 4, NextAfterName: "golf"}},
		errs:    map[fakeScanKey]error{namesOf(0): errors.New("connection reset")},
	}
	errOut.Reset()
	require.Equal(t, 1, run(context.Background(), optionsFixture("plan", ""), &out, &errOut, openFake(a, &opened)))
	require.Contains(t, errOut.String(), `-shard 0 -target names -after-name "golf"`)
}

func TestShardOutOfRangeIsRejected(t *testing.T) {
	a := &fakeAuditor{shards: 4}
	o := optionsFixture("plan", "")
	o.shard = 4
	var out, errOut bytes.Buffer
	opened := false
	require.Equal(t, 2, run(context.Background(), o, &out, &errOut, openFake(a, &opened)))
	require.Empty(t, a.scans)
}

// 没有映射就不知道库在哪:拒绝,不连连接串里的地址;也不去连名字缓存。
func TestRequiresDbPlacement(t *testing.T) {
	t.Setenv(authoritysql.BackendEnv, "mysql")
	t.Setenv("MMORPG_SQL_OPERATOR_PRINCIPAL", "maintenance@%")
	t.Setenv("CREATE_RESIDUAL_TEST_DSN", "u:p@tcp(127.0.0.1:1)/mmorpg_global")
	t.Setenv(dbplacement.EnvConfig, "")
	o := optionsFixture("plan", "")
	o.dsnEnv, o.schema, o.stores, o.logicalShards = "CREATE_RESIDUAL_TEST_DSN", "mmorpg_global", "1", 1
	var out, errOut bytes.Buffer
	code := run(context.Background(), o, &out, &errOut, openAuditor)
	require.Equal(t, 2, code)
	require.Contains(t, errOut.String(), dbplacement.EnvConfig)
	require.False(t, strings.Contains(errOut.String(), "u:p@"), "不回显连接串")
	require.Empty(t, out.String())
}
