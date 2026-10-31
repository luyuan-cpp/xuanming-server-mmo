package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"shared/placement"
)

// memStore 是 mappingStore 的内存替身:Locked 里的改动 body 成功才生效,失败回滚,和真事务同形。
type memStore struct {
	rows      map[uint32]mappingRow
	frozen    bool
	lockErr   error // Locked 进事务就失败(连不上库、锁等待超时)
	insertErr error
	locked    [][]uint32 // 每次 Locked 收到的 zones
	inserts   []mappingRow
	deletes   int
}

func newMemStore(merges map[uint32]uint32) *memStore {
	s := &memStore{rows: map[uint32]mappingRow{}}
	for src, dst := range merges {
		s.rows[src] = mappingRow{Src: src, Dst: dst, RunID: "seed", MergedAtMs: 1}
	}
	return s
}

func (s *memStore) sorted() []mappingRow {
	var out []mappingRow
	for _, src := range slices.Sorted(maps.Keys(s.rows)) {
		out = append(out, s.rows[src])
	}
	return out
}

func (s *memStore) Snapshot(context.Context) (bool, []mappingRow, error) {
	if s.lockErr != nil {
		return false, nil, s.lockErr
	}
	return !s.frozen, s.sorted(), nil
}

func (s *memStore) Locked(_ context.Context, zones []uint32, body func(mappingTx) error) error {
	s.locked = append(s.locked, zones)
	if s.lockErr != nil {
		return s.lockErr
	}
	if err := refuseFrozen(!s.frozen); err != nil {
		return err
	}
	saved := maps.Clone(s.rows)
	if err := body(memTx{s}); err != nil {
		s.rows = saved
		return err
	}
	return nil
}

type memTx struct{ s *memStore }

func (t memTx) Rows(context.Context) ([]mappingRow, error) { return t.s.sorted(), nil }

func (t memTx) Insert(_ context.Context, src, dst uint32, runID string) error {
	if t.s.insertErr != nil {
		return t.s.insertErr
	}
	row := mappingRow{Src: src, Dst: dst, RunID: runID, MergedAtMs: 2}
	t.s.rows[src] = row
	t.s.inserts = append(t.s.inserts, row)
	return nil
}

func (t memTx) Delete(_ context.Context, src, dst uint32) (bool, error) {
	row, ok := t.s.rows[src]
	if !ok || row.Dst != dst {
		return false, nil
	}
	delete(t.s.rows, src)
	t.s.deletes++
	return true, nil
}

var testNow = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

func runModeWith(store mappingStore, mode string, src, dst uint32, dryRun bool) (string, error) {
	var out bytes.Buffer
	err := execute(context.Background(), store, options{mode: mode, sourceZone: src, targetZone: dst, dryRun: dryRun}, testNow, &out)
	return out.String(), err
}

// chain 造一条 first → first+1 → … 的映射,共 hops 跳。
func chain(first uint32, hops int) map[uint32]uint32 {
	merges := map[uint32]uint32{}
	for i := 0; i < hops; i++ {
		merges[first+uint32(i)] = first + uint32(i) + 1
	}
	return merges
}

func TestDecideMerge(t *testing.T) {
	cases := []struct {
		name        string
		merges      map[uint32]uint32
		src, dst    uint32
		wantAlready bool
		wantRefuse  string // 空 = 放行
	}{
		{name: "空表放行", src: 5, dst: 6},
		{name: "别的映射不影响", merges: map[uint32]uint32{1: 2}, src: 5, dst: 6},
		{name: "连续合服:源区可以有人合进来", merges: map[uint32]uint32{4: 5}, src: 5, dst: 6},
		{name: "同 dst 已存在是幂等", merges: map[uint32]uint32{5: 6}, src: 5, dst: 6, wantAlready: true},
		{name: "同 dst 已存在,目标区后来又被合走,仍是幂等", merges: map[uint32]uint32{5: 6, 6: 7}, src: 5, dst: 6, wantAlready: true},
		{name: "源区已合进别的区", merges: map[uint32]uint32{5: 7}, src: 5, dst: 6, wantRefuse: "already merged into zone 7"},
		{name: "目标区已被合走", merges: map[uint32]uint32{6: 7}, src: 5, dst: 6, wantRefuse: "merge into its final zone 7"},
		{name: "目标区已被合走,点名链尾", merges: map[uint32]uint32{6: 7, 7: 8}, src: 5, dst: 6, wantRefuse: "final zone 8"},
		{name: "反向合会成环", merges: map[uint32]uint32{5: 6}, src: 6, dst: 5, wantRefuse: "would form a cycle"},
		{name: "隔一跳反向合也会成环", merges: map[uint32]uint32{4: 5, 5: 6}, src: 6, dst: 4, wantRefuse: "would form a cycle"},
		{name: "表里已有环", merges: map[uint32]uint32{1: 2, 2: 1}, src: 5, dst: 6, wantRefuse: "does not resolve"},
		{name: "目标区在坏链上", merges: map[uint32]uint32{6: 7, 7: 6}, src: 5, dst: 6, wantRefuse: "does not resolve"},
		{name: "恰好到跳数上限放行", merges: chain(1, placement.MaxMergedZoneHops-1), src: uint32(placement.MaxMergedZoneHops), dst: 100},
		{name: "超过跳数上限", merges: chain(1, placement.MaxMergedZoneHops), src: uint32(placement.MaxMergedZoneHops) + 1, dst: 100, wantRefuse: "does not resolve"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := maps.Clone(tc.merges)
			already, err := decideMerge(tc.merges, tc.src, tc.dst)
			if tc.wantRefuse != "" {
				if !errors.Is(err, errRefused) || !strings.Contains(err.Error(), tc.wantRefuse) {
					t.Fatalf("err = %v, want a refusal containing %q", err, tc.wantRefuse)
				}
			} else if err != nil {
				t.Fatalf("err = %v", err)
			}
			if already != tc.wantAlready {
				t.Fatalf("already = %v, want %v", already, tc.wantAlready)
			}
			if !maps.Equal(before, tc.merges) {
				t.Fatalf("decideMerge changed its input: %v, was %v", tc.merges, before)
			}
		})
	}
}

func TestRunMerge_WritesOneRowWithAuditRunID(t *testing.T) {
	store := newMemStore(nil)
	out, err := runModeWith(store, modeMerge, 5, 6, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.inserts) != 1 {
		t.Fatalf("inserts = %v", store.inserts)
	}
	got := store.inserts[0]
	if got.Src != 5 || got.Dst != 6 || got.RunID != "5-6-20261030T120000Z" {
		t.Fatalf("row = %+v", got)
	}
	if len(got.RunID) > 64 {
		t.Fatalf("run_id %q exceeds the column width", got.RunID)
	}
	if fmt.Sprint(store.locked) != "[[5 6]]" {
		t.Fatalf("locked zones = %v, want source and target", store.locked)
	}
	if !strings.Contains(out, "merged: zone 5 -> zone 6") {
		t.Fatalf("out = %q", out)
	}
}

func TestRunMerge_RepeatIsIdempotent(t *testing.T) {
	store := newMemStore(nil)
	if _, err := runModeWith(store, modeMerge, 5, 6, false); err != nil {
		t.Fatal(err)
	}
	out, err := runModeWith(store, modeMerge, 5, 6, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.inserts) != 1 || !strings.Contains(out, "nothing written") {
		t.Fatalf("second run: inserts=%v out=%q", store.inserts, out)
	}
}

func TestRunMerge_RefusalWritesNothing(t *testing.T) {
	cases := map[string]*memStore{
		"目标区已被合走":  newMemStore(map[uint32]uint32{6: 7}),
		"源区已合进别的区": newMemStore(map[uint32]uint32{5: 7}),
		"目录被冻结":    {rows: map[uint32]mappingRow{}, frozen: true},
	}
	for name, store := range cases {
		t.Run(name, func(t *testing.T) {
			before := maps.Clone(store.rows)
			_, err := runModeWith(store, modeMerge, 5, 6, false)
			if !errors.Is(err, errRefused) {
				t.Fatalf("err = %v, want a refusal", err)
			}
			if !maps.Equal(before, store.rows) || len(store.inserts) != 0 {
				t.Fatalf("a refusal wrote something: %v", store.rows)
			}
		})
	}
}

func TestRunMerge_InfrastructureFailureIsNotARefusal(t *testing.T) {
	boom := errors.New("lock wait timeout")
	store := newMemStore(nil)
	store.lockErr = boom
	if _, err := runModeWith(store, modeMerge, 5, 6, false); !errors.Is(err, boom) || errors.Is(err, errRefused) {
		t.Fatalf("err = %v, want the failure itself", err)
	}
	store = newMemStore(nil)
	store.insertErr = boom
	if _, err := runModeWith(store, modeMerge, 5, 6, false); !errors.Is(err, boom) || len(store.rows) != 0 {
		t.Fatalf("err = %v rows = %v, want the failure and a rollback", err, store.rows)
	}
}

func TestDryRun_WritesNothingAndTakesNoLock(t *testing.T) {
	t.Run("merge 预报写入", func(t *testing.T) {
		store := newMemStore(nil)
		out, err := runModeWith(store, modeMerge, 5, 6, true)
		if err != nil || !strings.Contains(out, "would record zone 5 -> zone 6 (run_id 5-6-20261030T120000Z)") {
			t.Fatalf("out=%q err=%v", out, err)
		}
		if len(store.rows) != 0 || len(store.locked) != 0 {
			t.Fatalf("dry-run touched the store: rows=%v locked=%v", store.rows, store.locked)
		}
	})
	t.Run("merge 预报幂等", func(t *testing.T) {
		out, err := runModeWith(newMemStore(map[uint32]uint32{5: 6}), modeMerge, 5, 6, true)
		if err != nil || !strings.Contains(out, "writes nothing") {
			t.Fatalf("out=%q err=%v", out, err)
		}
	})
	t.Run("merge 预报拒绝", func(t *testing.T) {
		if _, err := runModeWith(newMemStore(map[uint32]uint32{6: 7}), modeMerge, 5, 6, true); !errors.Is(err, errRefused) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("冻结同样预报拒绝", func(t *testing.T) {
		store := newMemStore(nil)
		store.frozen = true
		if _, err := runModeWith(store, modeMerge, 5, 6, true); !errors.Is(err, errRefused) {
			t.Fatalf("merge: err = %v", err)
		}
		if _, err := runModeWith(store, modeUnmerge, 5, 6, true); !errors.Is(err, errRefused) {
			t.Fatalf("unmerge: err = %v", err)
		}
	})
	t.Run("unmerge 预报", func(t *testing.T) {
		store := newMemStore(map[uint32]uint32{5: 6})
		out, err := runModeWith(store, modeUnmerge, 5, 6, true)
		if err != nil || !strings.Contains(out, "would delete zone 5 -> zone 6") {
			t.Fatalf("out=%q err=%v", out, err)
		}
		if len(store.rows) != 1 || len(store.locked) != 0 {
			t.Fatalf("dry-run touched the store: rows=%v locked=%v", store.rows, store.locked)
		}
		out, err = runModeWith(store, modeUnmerge, 9, 6, true)
		if err != nil || !strings.Contains(out, "zone 9 is not merged") {
			t.Fatalf("out=%q err=%v", out, err)
		}
	})
}

func TestRunUnmerge(t *testing.T) {
	t.Run("删掉这一行", func(t *testing.T) {
		store := newMemStore(map[uint32]uint32{5: 6})
		out, err := runModeWith(store, modeUnmerge, 5, 6, false)
		if err != nil || store.deletes != 1 || len(store.rows) != 0 || !strings.Contains(out, "unmerged: zone 5 -> zone 6") {
			t.Fatalf("out=%q err=%v store=%+v", out, err, store)
		}
	})
	t.Run("不存在是幂等成功", func(t *testing.T) {
		store := newMemStore(nil)
		out, err := runModeWith(store, modeUnmerge, 5, 6, false)
		if err != nil || store.deletes != 0 || !strings.Contains(out, "zone 5 is not merged") {
			t.Fatalf("out=%q err=%v", out, err)
		}
	})
	t.Run("映射到别的区时不删", func(t *testing.T) {
		store := newMemStore(map[uint32]uint32{5: 7})
		out, err := runModeWith(store, modeUnmerge, 5, 6, false)
		if err != nil || len(store.rows) != 1 || !strings.Contains(out, "zone 5 maps to zone 7, not zone 6") {
			t.Fatalf("out=%q err=%v rows=%v", out, err, store.rows)
		}
	})
	t.Run("不要求逆序撤销:链上任何一跳都能删", func(t *testing.T) {
		store := newMemStore(map[uint32]uint32{4: 5, 5: 6})
		if _, err := runModeWith(store, modeUnmerge, 4, 5, false); err != nil {
			t.Fatal(err)
		}
		if _, ok := store.rows[5]; !ok || len(store.rows) != 1 {
			t.Fatalf("rows = %v", store.rows)
		}
		store = newMemStore(map[uint32]uint32{4: 5, 5: 6})
		if _, err := runModeWith(store, modeUnmerge, 5, 6, false); err != nil {
			t.Fatal(err)
		}
		if _, ok := store.rows[4]; !ok || len(store.rows) != 1 {
			t.Fatalf("rows = %v", store.rows)
		}
	})
	t.Run("目录被冻结拒绝", func(t *testing.T) {
		store := newMemStore(map[uint32]uint32{5: 6})
		store.frozen = true
		if _, err := runModeWith(store, modeUnmerge, 5, 6, false); !errors.Is(err, errRefused) || len(store.rows) != 1 {
			t.Fatalf("err=%v rows=%v", err, store.rows)
		}
	})
}

func TestRunAudit_ListsMappingAndFinalZones(t *testing.T) {
	store := newMemStore(map[uint32]uint32{4: 5, 5: 6, 8: 9})
	out, err := runModeWith(store, modeAudit, 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"route_meta.writable: true",
		"merged zones: 3",
		"final zone 6 includes zones [4 5 6]",
		"final zone 9 includes zones [8 9]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("audit output misses %q:\n%s", want, out)
		}
	}
	// 4 的最终区是 6:链要解析到底,不是它的直接目标。
	found := false
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "4" {
			found = fields[1] == "5" && fields[2] == "6"
		}
	}
	if !found {
		t.Fatalf("zone 4 row should show dst 5 and final 6:\n%s", out)
	}
	if len(store.locked) != 0 {
		t.Fatalf("audit took locks: %v", store.locked)
	}
}

func TestRunAudit_BrokenMappingIsAFinding(t *testing.T) {
	store := newMemStore(map[uint32]uint32{1: 2, 2: 1})
	out, err := runModeWith(store, modeAudit, 0, 0, false)
	if !errors.Is(err, errRefused) || !strings.Contains(err.Error(), "does not resolve") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out, "unresolved") {
		t.Fatalf("the rows must still be listed:\n%s", out)
	}
}

func TestRunAudit_EmptyTable(t *testing.T) {
	out, err := runModeWith(newMemStore(nil), modeAudit, 0, 0, false)
	if err != nil || !strings.Contains(out, "merged zones: 0") {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestExitCodes(t *testing.T) {
	if got := exitCodeOf(nil); got != exitOK {
		t.Errorf("nil = %d", got)
	}
	if got := exitCodeOf(refusedf("no")); got != exitRefused {
		t.Errorf("refused = %d", got)
	}
	if got := exitCodeOf(errors.New("connection lost")); got != exitFailed {
		t.Errorf("failure = %d", got)
	}
}

const fixtureDSN = "op:s3cret-word@tcp(127.0.0.1:4000)/mmorpg_global"

func TestParseOptions(t *testing.T) {
	dsn := []string{"-route-db-dsn", fixtureDSN}
	ok := []struct {
		name string
		args []string
		want options
	}{
		{"merge", []string{"-mode", "merge", "-source-zone", "5", "-target-zone", "6"}, options{mode: modeMerge, sourceZone: 5, targetZone: 6}},
		{"merge dry-run", []string{"-mode", "merge", "-source-zone", "5", "-target-zone", "6", "-dry-run"}, options{mode: modeMerge, sourceZone: 5, targetZone: 6, dryRun: true}},
		{"unmerge", []string{"-mode", "unmerge", "-source-zone", "5", "-target-zone", "6"}, options{mode: modeUnmerge, sourceZone: 5, targetZone: 6}},
		{"audit", []string{"-mode", "audit"}, options{mode: modeAudit}},
		{"zone 上限", []string{"-mode", "merge", "-source-zone", "999998", "-target-zone", "999999"}, options{mode: modeMerge, sourceZone: 999998, targetZone: 999999}},
	}
	for _, tc := range ok {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseOptions(append(tc.args, dsn...), io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			tc.want.routeDSN = fixtureDSN
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}

	bad := map[string][]string{
		"没有 mode":        {"-route-db-dsn", fixtureDSN},
		"未知 mode":        {"-mode", "pin-placement", "-route-db-dsn", fixtureDSN},
		"没有 DSN":         {"-mode", "audit"},
		"缺源区":            {"-mode", "merge", "-target-zone", "6", "-route-db-dsn", fixtureDSN},
		"缺目标区":           {"-mode", "unmerge", "-source-zone", "5", "-route-db-dsn", fixtureDSN},
		"同一个区":           {"-mode", "merge", "-source-zone", "5", "-target-zone", "5", "-route-db-dsn", fixtureDSN},
		"zone 越界":        {"-mode", "merge", "-source-zone", "5", "-target-zone", "1000000", "-route-db-dsn", fixtureDSN},
		"审计不收 zone":      {"-mode", "audit", "-source-zone", "5", "-route-db-dsn", fixtureDSN},
		"审计不收 dry-run":   {"-mode", "audit", "-dry-run", "-route-db-dsn", fixtureDSN},
		"已删旗标 -apply":    {"-mode", "merge", "-source-zone", "5", "-target-zone", "6", "-apply", "-route-db-dsn", fixtureDSN},
		"已删旗标 -redis":    {"-mode", "merge", "-source-zone", "5", "-target-zone", "6", "-redis-addr", "h:1", "-route-db-dsn", fixtureDSN},
		"已删旗标 -manifest": {"-mode", "merge", "-source-zone", "5", "-target-zone", "6", "-manifest-path", "x", "-route-db-dsn", fixtureDSN},
		"多余的位置参数":        {"-mode", "audit", "-route-db-dsn", fixtureDSN, "extra"},
	}
	for name, args := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := parseOptions(args, io.Discard); err == nil {
				t.Fatal("must refuse")
			}
		})
	}
}

// 错误和用法提示都不能带出 DSN 里的口令,包括把 DSN 写错位置的情况。
func TestRun_NeverEchoesTheDSN(t *testing.T) {
	const secret = "s3cret-word"
	getenv := func(string) string { return "" }
	cases := map[string][]string{
		"DSN 写成位置参数": {"-mode", "audit", "-route-db-dsn", "x", fixtureDSN},
		"DSN 解析不了":   {"-mode", "audit", "-route-db-dsn", "op:" + secret + "@tcp(127.0.0.1:1)"},
		"DSN 没有库名":   {"-mode", "audit", "-route-db-dsn", "op:" + secret + "@tcp(127.0.0.1:1)/"},
		"写操作没有操作员身份": {"-mode", "merge", "-source-zone", "5", "-target-zone", "6", "-route-db-dsn", fixtureDSN},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := run(args, getenv, &out, &errOut); code == exitOK {
				t.Fatalf("exit %d, want a failure", code)
			}
			if strings.Contains(out.String()+errOut.String(), secret) {
				t.Fatalf("the password leaked:\n%s\n%s", out.String(), errOut.String())
			}
		})
	}
}

func TestOpenStoreFor_RealWriteNeedsOperatorIdentity(t *testing.T) {
	o := options{mode: modeMerge, sourceZone: 5, targetZone: 6, routeDSN: fixtureDSN}
	_, err := openStoreFor(context.Background(), o, func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "MMORPG_SQL_OPERATOR_PRINCIPAL") {
		t.Fatalf("err = %v", err)
	}
	if !o.writes() {
		t.Fatal("a real merge writes")
	}
	o.dryRun = true
	if o.writes() {
		t.Fatal("dry-run does not write")
	}
	if (options{mode: modeAudit}).writes() {
		t.Fatal("audit does not write")
	}
}

func TestOpenStoreFor_RejectsUnknownBackend(t *testing.T) {
	o := options{mode: modeAudit, routeDSN: fixtureDSN}
	_, err := openStoreFor(context.Background(), o, func(name string) string {
		if name == "MMORPG_SQL_BACKEND" {
			return "postgres"
		}
		return ""
	})
	if err == nil || !strings.Contains(err.Error(), "MMORPG_SQL_BACKEND") {
		t.Fatalf("err = %v", err)
	}
}
