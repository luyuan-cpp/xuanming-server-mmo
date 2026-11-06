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

type fakeAuditor struct {
	shards  int
	reports map[int]store.CreateResidualReport
	errs    map[int]error
	scans   []store.CreateResidualScan
}

func (f *fakeAuditor) Shards() int { return f.shards }

func (f *fakeAuditor) Run(_ context.Context, scan store.CreateResidualScan) (store.CreateResidualReport, error) {
	f.scans = append(f.scans, scan)
	report := f.reports[scan.Slot]
	report.Slot = scan.Slot
	return report, f.errs[scan.Slot]
}

func openFake(a *fakeAuditor, opened *bool) opener {
	return func(context.Context, options) (auditor, func(), error) {
		*opened = true
		return a, func() {}, nil
	}
}

func optionsFixture(mode, repair string) options {
	return options{mode: mode, repair: repair, shard: -1, batch: store.CreateResidualDefaultBatch, maxRoles: 5, timeout: time.Minute}
}

func TestRejectsBadArgumentsBeforeOpeningTheStore(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*options)
	}{
		{"未知模式", func(o *options) { o.mode = "force" }},
		{"plan 带 repair", func(o *options) { o.repair = "ref" }},
		{"apply 不选种类", func(o *options) { o.mode = "apply" }},
		{"apply 未知种类", func(o *options) { o.mode, o.repair = "apply", "ref,everything" }},
		{"after 不指定片", func(o *options) { o.after = 7 }},
		{"片号非法", func(o *options) { o.shard = -2 }},
		{"limit 为负", func(o *options) { o.limit = -1 }},
		{"batch 为 0", func(o *options) { o.batch = 0 }},
		{"batch 超上限", func(o *options) { o.batch = store.CreateResidualMaxBatch + 1 }},
		{"没有时限", func(o *options) { o.timeout = 0 }},
		{"角色上限为 0", func(o *options) { o.maxRoles = 0 }},
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

// plan 扫全部片、一项都不修;没有残留时退出码 0。
func TestPlanScansEveryShardWithoutRepairs(t *testing.T) {
	a := &fakeAuditor{shards: 3, reports: map[int]store.CreateResidualReport{
		0: {Scanned: 2, NextAfter: 40, Done: true}, 1: {Scanned: 1, NextAfter: 41, Done: true}, 2: {Done: true},
	}}
	var out, errOut bytes.Buffer
	opened := false
	code := run(context.Background(), optionsFixture("plan", ""), &out, &errOut, openFake(a, &opened))
	require.Equal(t, 0, code, errOut.String())
	require.Len(t, a.scans, 3)
	for slot, scan := range a.scans {
		require.Equal(t, slot, scan.Slot)
		require.Equal(t, store.CreateResidualRepairs{}, scan.Repair, "plan 不修")
	}
	require.Contains(t, out.String(), "summary mode=plan shards=3 scanned=3 findings=0 attention=0 done=true")
}

// apply 只修 -repair 列出的种类。
func TestApplyRepairsOnlyTheChosenKinds(t *testing.T) {
	a := &fakeAuditor{shards: 2, reports: map[int]store.CreateResidualReport{0: {Done: true}, 1: {Done: true}}}
	o := optionsFixture("apply", "ref, initial_rows")
	o.shard, o.after, o.limit, o.batch = 1, 99, 10, 5
	var out, errOut bytes.Buffer
	opened := false
	require.Equal(t, 0, run(context.Background(), o, &out, &errOut, openFake(a, &opened)), errOut.String())
	require.Equal(t, []store.CreateResidualScan{{Slot: 1, After: 99, Limit: 10, Batch: 5,
		Repair: store.CreateResidualRepairs{Refs: true, InitialRows: true}}}, a.scans)
}

// 每一项一行;还要人看的项让退出码非 0,修好的不算。
func TestFindingsThatNeedAttentionExitNonZero(t *testing.T) {
	report := store.CreateResidualReport{Scanned: 3, NextAfter: 30, Done: true, Findings: []store.CreateResidualFinding{
		{Slot: 0, PlayerID: 12, Kind: store.ResidualRefMissing, Result: store.ResidualRepaired, Reason: "over_role_cap"},
		{Slot: 0, PlayerID: 21, Kind: store.ResidualNameTaken, Result: store.ResidualManual, Reason: "name_held_by_other", Holder: 22, HolderLive: true, Keeper: 21},
	}}
	a := &fakeAuditor{shards: 1, reports: map[int]store.CreateResidualReport{0: report}}
	var out, errOut bytes.Buffer
	opened := false
	require.Equal(t, 1, run(context.Background(), optionsFixture("apply", "ref,name"), &out, &errOut, openFake(a, &opened)))
	require.Contains(t, out.String(), "finding shard=0 player=12 kind=ref_missing result=repaired reason=over_role_cap\n")
	require.Contains(t, out.String(), "finding shard=0 player=21 kind=name_taken result=manual reason=name_held_by_other holder=22 holder_live=true keeper=21\n")
	require.Contains(t, out.String(), "attention=1")

	report.Findings = report.Findings[:1]
	a.reports[0] = report
	out.Reset()
	require.Equal(t, 0, run(context.Background(), optionsFixture("apply", "ref"), &out, &errOut, openFake(a, &opened)), "修好的不算要人看")
}

// 一片扫到一半出错:停下,给出接着扫的位置;底层错误可能带连接细节,不输出。
func TestScanErrorStopsWithAResumePointAndNoDetails(t *testing.T) {
	a := &fakeAuditor{
		shards:  3,
		reports: map[int]store.CreateResidualReport{0: {Done: true}, 1: {Scanned: 4, NextAfter: 77}},
		errs:    map[int]error{1: errors.New("dial tcp 10.0.0.1:4000: password=private-password-must-not-leak")},
	}
	var out, errOut bytes.Buffer
	opened := false
	require.Equal(t, 1, run(context.Background(), optionsFixture("plan", ""), &out, &errOut, openFake(a, &opened)))
	require.Len(t, a.scans, 2, "出错之后不再扫后面的片")
	require.Contains(t, errOut.String(), "-shard 1 -after 77")
	require.NotContains(t, errOut.String()+out.String(), "private-password")
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

// 没有映射就不知道库在哪:拒绝,不连连接串里的地址。
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
