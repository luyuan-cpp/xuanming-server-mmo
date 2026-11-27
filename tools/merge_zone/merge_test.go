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
	"sync"
	"testing"
	"time"

	"shared/mergedzonecopy"
	"shared/placement"
)

// memStore 是 mappingStore 的内存替身:Locked 里的改动(映射与历史)body 成功才生效,失败一起回滚,和真事务同形。
type memStore struct {
	rows           map[uint32]mappingRow
	history        []historyRow
	frozen         bool
	lockErr        error // Locked 进事务就失败(连不上库、锁等待超时)
	insertErr      error
	historyErr     error      // 追加历史失败(表不在、没有 INSERT 权限):映射与历史一起回滚
	historyReadErr error      // 审计读历史失败
	committedErr   error      // Committed 读不回来(提交之后第 0 片断开)
	locked         [][]uint32 // 每次 Locked 收到的 zones
	inserts        []mappingRow
	deletes        int
}

// memGeneration 是内存替身的库世代(第 0 片 route_meta.generation)。
const memGeneration = "0123456789abcdef0123456789abcdef"

// newMemStore 种的映射行都当成本工具写的:每行带一条对得上的 merge 历史(run_id "seed")。
func newMemStore(merges map[uint32]uint32) *memStore {
	s := &memStore{rows: map[uint32]mappingRow{}}
	for _, src := range slices.Sorted(maps.Keys(merges)) {
		dst := merges[src]
		s.rows[src] = mappingRow{Src: src, Dst: dst, RunID: "seed", MergedAtMs: 1}
		s.history = append(s.history, historyRow{Src: src, Seq: 1, Action: historyActionMerge, Dst: dst, RunID: "seed", Principal: "seed@%", AtMs: 1})
	}
	return s
}

// appendHistory 与 SQL 实现同形:序号是这个源区已有的最大值 +1。
func (s *memStore) appendHistory(src uint32, action string, dst uint32, runID string) error {
	if s.historyErr != nil {
		return s.historyErr
	}
	var last uint64
	for _, h := range s.history {
		if h.Src == src && h.Seq > last {
			last = h.Seq
		}
	}
	s.history = append(s.history, historyRow{Src: src, Seq: last + 1, Action: action, Dst: dst, RunID: runID, Principal: "operator@%", AtMs: 2})
	return nil
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

func (s *memStore) History(context.Context) ([]historyRow, error) {
	if s.lockErr != nil {
		return nil, s.lockErr
	}
	if s.historyReadErr != nil {
		return nil, s.historyReadErr
	}
	return slices.Clone(s.history), nil
}

func (s *memStore) Locked(_ context.Context, zones []uint32, body func(mappingTx) error) error {
	s.locked = append(s.locked, zones)
	if s.lockErr != nil {
		return s.lockErr
	}
	if err := refuseFrozen(!s.frozen); err != nil {
		return err
	}
	saved, savedHistory := maps.Clone(s.rows), slices.Clone(s.history)
	if err := body(memTx{s}); err != nil {
		s.rows, s.history = saved, savedHistory
		return err
	}
	return nil
}

// Committed 与 SQL 实现同一口径:版本是历史的条数,映射是现在的映射行。
func (s *memStore) Committed(context.Context) (mergedzonecopy.Mapping, error) {
	if s.lockErr != nil {
		return mergedzonecopy.Mapping{}, s.lockErr
	}
	if s.committedErr != nil {
		return mergedzonecopy.Mapping{}, s.committedErr
	}
	merges := make(map[uint32]uint32, len(s.rows))
	for src, row := range s.rows {
		merges[src] = row.Dst
	}
	return mergedzonecopy.Mapping{Generation: memGeneration, Version: uint64(len(s.history)), Merges: merges}, nil
}

// memCopies 是 etcd 副本的内存替身:revision 0 = 没有键,每写一次加 1;Put 按 revision 比较,和 etcd 的 ModRevision 同一语义。
// 真库的并发用例里几个 worker 共用一份,所以加锁。
type memCopies struct {
	mu       sync.Mutex
	value    mergedzonecopy.Mapping
	revision int64
	getErr   error
	putErr   error
	// racer 在每次 Put 比较 revision 之前调用,模拟别的写者(data_service)抢先写了一次。
	racer func(*memCopies)
	puts  int
}

func newMemCopies() *memCopies { return &memCopies{} }

func (c *memCopies) Key() string { return mergedzonecopy.Key("mem_global_00") }

func (c *memCopies) Get(context.Context) (mergedzonecopy.Mapping, int64, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.getErr != nil {
		return mergedzonecopy.Mapping{}, 0, false, c.getErr
	}
	if c.revision == 0 {
		return mergedzonecopy.Mapping{}, 0, false, nil
	}
	value := c.value
	value.Merges = maps.Clone(value.Merges)
	return value, c.revision, true, nil
}

func (c *memCopies) Put(_ context.Context, revision int64, mapping mergedzonecopy.Mapping) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.puts++
	if c.racer != nil {
		c.racer(c)
	}
	if c.putErr != nil {
		return false, c.putErr
	}
	if revision != c.revision {
		return false, nil
	}
	mapping.Merges = maps.Clone(mapping.Merges)
	c.value, c.revision = mapping, c.revision+1
	return true, nil
}

type memTx struct{ s *memStore }

func (t memTx) Rows(context.Context) ([]mappingRow, error) { return t.s.sorted(), nil }

func (t memTx) Insert(_ context.Context, src, dst uint32, runID string) error {
	if t.s.insertErr != nil {
		return t.s.insertErr
	}
	row := mappingRow{Src: src, Dst: dst, RunID: runID, MergedAtMs: 2}
	t.s.rows[src] = row
	if err := t.s.appendHistory(src, historyActionMerge, dst, runID); err != nil {
		return err
	}
	t.s.inserts = append(t.s.inserts, row)
	return nil
}

func (t memTx) Delete(_ context.Context, src, dst uint32) (bool, error) {
	row, ok := t.s.rows[src]
	if !ok || row.Dst != dst {
		return false, nil
	}
	delete(t.s.rows, src)
	if err := t.s.appendHistory(src, historyActionUnmerge, dst, row.RunID); err != nil {
		return false, err
	}
	t.s.deletes++
	return true, nil
}

var testNow = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

// runModeWith 每次给一份新的空副本:不关心副本的用例用它。
func runModeWith(store mappingStore, mode string, src, dst uint32, dryRun bool) (string, error) {
	return runModeWithCopies(store, newMemCopies(), mode, src, dst, dryRun)
}

func runModeWithCopies(store mappingStore, copies mergedzonecopy.Copies, mode string, src, dst uint32, dryRun bool) (string, error) {
	var out bytes.Buffer
	err := execute(context.Background(), store, copies, options{mode: mode, sourceZone: src, targetZone: dst, dryRun: dryRun}, testNow, &out)
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
			before, beforeHistory := maps.Clone(store.rows), slices.Clone(store.history)
			_, err := runModeWith(store, modeMerge, 5, 6, false)
			if !errors.Is(err, errRefused) {
				t.Fatalf("err = %v, want a refusal", err)
			}
			if !maps.Equal(before, store.rows) || len(store.inserts) != 0 || !slices.Equal(beforeHistory, store.history) {
				t.Fatalf("a refusal wrote something: rows=%v history=%v", store.rows, store.history)
			}
		})
	}
}

// 合服在同一个事务里追加一条 merge 历史,run_id 与映射行相同;幂等重跑不写历史。
func TestRunMerge_AppendsOneMergeEvent(t *testing.T) {
	store := newMemStore(nil)
	if _, err := runModeWith(store, modeMerge, 5, 6, false); err != nil {
		t.Fatal(err)
	}
	if _, err := runModeWith(store, modeMerge, 5, 6, false); err != nil {
		t.Fatal(err)
	}
	want := []historyRow{{Src: 5, Seq: 1, Action: historyActionMerge, Dst: 6, RunID: "5-6-20261030T120000Z", Principal: "operator@%", AtMs: 2}}
	if !slices.Equal(store.history, want) {
		t.Fatalf("history = %+v, want %+v", store.history, want)
	}
	if store.rows[5].RunID != store.history[0].RunID {
		t.Fatalf("the mapping row and its history disagree on run_id: %q vs %q", store.rows[5].RunID, store.history[0].RunID)
	}
}

// 撤销追加一条 unmerge 历史,带被删那一行的 run_id;没东西可删时不写历史;同一源区的序号接着往下排。
func TestRunUnmerge_AppendsUnmergeEventWithTheDeletedRunID(t *testing.T) {
	store := newMemStore(map[uint32]uint32{5: 6})
	if _, err := runModeWith(store, modeUnmerge, 5, 6, false); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][2]uint32{{5, 6}, {5, 9}} { // 再撤一次、撤一个从没合过的目标:都什么都不写
		if _, err := runModeWith(store, modeUnmerge, args[0], args[1], false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runModeWith(store, modeMerge, 5, 7, false); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, h := range store.history {
		got = append(got, fmt.Sprintf("%d#%d %s %d %s", h.Src, h.Seq, h.Action, h.Dst, h.RunID))
	}
	want := []string{"5#1 merge 6 seed", "5#2 unmerge 6 seed", "5#3 merge 7 5-7-20261030T120000Z"}
	if !slices.Equal(got, want) {
		t.Fatalf("history = %v, want %v", got, want)
	}
}

// 历史写不进去(表不在、没有 INSERT 权限)时报原错误、不算拒绝,映射跟着回滚:不能出现没有历史的映射变更。
func TestHistoryFailureRollsBackTheMapping(t *testing.T) {
	boom := errors.New("Error 1146: Table 'zone_merge_history' doesn't exist")
	t.Run("合服", func(t *testing.T) {
		store := newMemStore(nil)
		store.historyErr = boom
		if _, err := runModeWith(store, modeMerge, 5, 6, false); !errors.Is(err, boom) || errors.Is(err, errRefused) {
			t.Fatalf("err = %v, want the failure itself", err)
		}
		if len(store.rows) != 0 || len(store.history) != 0 || len(store.inserts) != 0 {
			t.Fatalf("rows=%v history=%v inserts=%v, want nothing", store.rows, store.history, store.inserts)
		}
	})
	t.Run("撤销", func(t *testing.T) {
		store := newMemStore(map[uint32]uint32{5: 6})
		store.historyErr = boom
		if _, err := runModeWith(store, modeUnmerge, 5, 6, false); !errors.Is(err, boom) || errors.Is(err, errRefused) {
			t.Fatalf("err = %v, want the failure itself", err)
		}
		if store.rows[5].Dst != 6 || len(store.history) != 1 || store.deletes != 0 {
			t.Fatalf("rows=%v history=%v deletes=%d, want the seed untouched", store.rows, store.history, store.deletes)
		}
	})
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
	if err != nil || !strings.Contains(out, "merged zones: 0") || !strings.Contains(out, "merge history: 0 event(s)") {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

// 撤销之后映射行没了,审计仍列得出那次合服和那次撤销:谁、什么时候、哪个区。
func TestRunAudit_ListsHistoryAfterUnmerge(t *testing.T) {
	store := newMemStore(nil)
	for _, step := range []struct {
		mode     string
		src, dst uint32
	}{{modeMerge, 5, 6}, {modeUnmerge, 5, 6}} {
		if _, err := runModeWith(store, step.mode, step.src, step.dst, false); err != nil {
			t.Fatal(err)
		}
	}
	out, err := runModeWith(store, modeAudit, 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"merged zones: 0", "merge history: 2 event(s)", "sql_principal"} {
		if !strings.Contains(out, want) {
			t.Errorf("audit output misses %q:\n%s", want, out)
		}
	}
	var events []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 7 && (fields[1] == historyActionMerge || fields[1] == historyActionUnmerge) {
			events = append(events, strings.Join(fields[1:], " "))
		}
	}
	want := []string{"merge 5 1 6 5-6-20261030T120000Z operator@%", "unmerge 5 2 6 5-6-20261030T120000Z operator@%"}
	if !slices.Equal(events, want) {
		t.Fatalf("history lines = %v, want %v\n%s", events, want, out)
	}
}

// 映射行在历史里找不到对应的 merge 记录(手改的,或建历史表之前写的):列出来并按拒绝退出,解析结果照样打印。
func TestRunAudit_UntrackedMappingIsAFinding(t *testing.T) {
	store := newMemStore(map[uint32]uint32{4: 5})
	store.rows[8] = mappingRow{Src: 8, Dst: 9, RunID: "by-hand", MergedAtMs: 1}
	out, err := runModeWith(store, modeAudit, 0, 0, false)
	if !errors.Is(err, errRefused) || !strings.Contains(err.Error(), "no merge event in zone_merge_history") {
		t.Fatalf("err = %v, want a refusal naming the history gap", err)
	}
	if !strings.Contains(out, "untracked mapping: zone 8 -> zone 9 (run_id by-hand)") || strings.Contains(out, "untracked mapping: zone 4") {
		t.Fatalf("only zone 8 is untracked:\n%s", out)
	}
	if !strings.Contains(out, "final zone 9 includes zones [8 9]") {
		t.Fatalf("the resolution must still be printed:\n%s", out)
	}
}

// 读不到历史时审计报原错误(退出码 2),不能当成「没有历史」去判来历不明。
func TestRunAudit_HistoryReadFailureIsNotAFinding(t *testing.T) {
	boom := errors.New("Error 1142: SELECT command denied")
	store := newMemStore(map[uint32]uint32{4: 5})
	store.historyReadErr = boom
	if _, err := runModeWith(store, modeAudit, 0, 0, false); !errors.Is(err, boom) || errors.Is(err, errRefused) {
		t.Fatalf("err = %v, want the read failure itself", err)
	}
}

func TestUntrackedMappings(t *testing.T) {
	rows := []mappingRow{{Src: 1, Dst: 2, RunID: "a"}, {Src: 3, Dst: 4, RunID: "b"}, {Src: 5, Dst: 6, RunID: "c"}}
	history := []historyRow{
		{Src: 1, Seq: 1, Action: historyActionMerge, Dst: 2, RunID: "a"},
		// 3 → 4 有过 merge,读历史之前刚被撤销:映射行是撤销之前读到的,不算来历不明。
		{Src: 3, Seq: 1, Action: historyActionMerge, Dst: 4, RunID: "b"},
		{Src: 3, Seq: 2, Action: historyActionUnmerge, Dst: 4, RunID: "b"},
		// 5 只有一条 run_id 对不上的 merge,或只有 unmerge:都不算。
		{Src: 5, Seq: 1, Action: historyActionMerge, Dst: 6, RunID: "other"},
		{Src: 5, Seq: 2, Action: historyActionUnmerge, Dst: 6, RunID: "c"},
	}
	got := untrackedMappings(rows, history)
	if len(got) != 1 || got[0].Src != 5 {
		t.Fatalf("untracked = %+v, want only zone 5", got)
	}
	if got := untrackedMappings(rows, nil); len(got) != 3 {
		t.Fatalf("no history: untracked = %+v, want all three", got)
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

const fixtureEtcd = "127.0.0.1:2379"

func TestParseOptions(t *testing.T) {
	dsn := []string{"-route-db-dsn", fixtureDSN}
	etcd := []string{"-etcd-hosts", fixtureEtcd}
	ok := []struct {
		name string
		args []string
		want options
	}{
		{"merge", append([]string{"-mode", "merge", "-source-zone", "5", "-target-zone", "6"}, etcd...), options{mode: modeMerge, sourceZone: 5, targetZone: 6, etcdHosts: fixtureEtcd}},
		{"merge dry-run 不要 etcd", []string{"-mode", "merge", "-source-zone", "5", "-target-zone", "6", "-dry-run"}, options{mode: modeMerge, sourceZone: 5, targetZone: 6, dryRun: true}},
		{"unmerge", append([]string{"-mode", "unmerge", "-source-zone", "5", "-target-zone", "6"}, etcd...), options{mode: modeUnmerge, sourceZone: 5, targetZone: 6, etcdHosts: fixtureEtcd}},
		{"audit", []string{"-mode", "audit"}, options{mode: modeAudit}},
		{"audit 比对副本", []string{"-mode", "audit", "-etcd-hosts", "10.0.0.1:2379, 10.0.0.2:2379"}, options{mode: modeAudit, etcdHosts: "10.0.0.1:2379, 10.0.0.2:2379"}},
		{"zone 上限", append([]string{"-mode", "merge", "-source-zone", "999998", "-target-zone", "999999"}, etcd...), options{mode: modeMerge, sourceZone: 999998, targetZone: 999999, etcdHosts: fixtureEtcd}},
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
		"真合服没有 etcd":     {"-mode", "merge", "-source-zone", "5", "-target-zone", "6", "-route-db-dsn", fixtureDSN},
		"真撤销没有 etcd":     {"-mode", "unmerge", "-source-zone", "5", "-target-zone", "6", "-route-db-dsn", fixtureDSN},
		"etcd 地址没有端口":    {"-mode", "audit", "-etcd-hosts", "etcd.infra", "-route-db-dsn", fixtureDSN},
		"etcd 地址有空项":     {"-mode", "audit", "-etcd-hosts", "127.0.0.1:2379,", "-route-db-dsn", fixtureDSN},
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
		"写操作没有操作员身份": {"-mode", "merge", "-source-zone", "5", "-target-zone", "6", "-route-db-dsn", fixtureDSN, "-etcd-hosts", fixtureEtcd},
		"写操作没有 etcd": {"-mode", "merge", "-source-zone", "5", "-target-zone", "6", "-route-db-dsn", fixtureDSN},
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

// 真写提交之后把第 0 片现在的映射发布进 etcd 副本:版本 = 历史条数;幂等重跑不再写;撤销同样发布;dry-run 不发布。
func TestRealWritesPublishTheCommittedMapping(t *testing.T) {
	store := newMemStore(nil)
	copies := newMemCopies()
	out, err := runModeWithCopies(store, copies, modeMerge, 5, 6, false)
	if err != nil || !strings.Contains(out, "published version 1") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if copies.value.Generation != memGeneration || copies.value.Version != 1 || !maps.Equal(copies.value.Merges, map[uint32]uint32{5: 6}) {
		t.Fatalf("copy = %+v", copies.value)
	}
	out, err = runModeWithCopies(store, copies, modeMerge, 5, 6, false)
	if err != nil || copies.puts != 1 || !strings.Contains(out, "already holds version 1") {
		t.Fatalf("an idempotent re-run publishes nothing new: out=%q err=%v puts=%d", out, err, copies.puts)
	}
	out, err = runModeWithCopies(store, copies, modeUnmerge, 5, 6, false)
	if err != nil || copies.value.Version != 2 || len(copies.value.Merges) != 0 || !strings.Contains(out, "published version 2") {
		t.Fatalf("an unmerge publishes too: out=%q err=%v copy=%+v", out, err, copies.value)
	}
	if _, err := runModeWithCopies(store, copies, modeMerge, 5, 6, true); err != nil || copies.puts != 2 {
		t.Fatalf("a dry-run publishes nothing: err=%v puts=%d", err, copies.puts)
	}
}

// 提交之后发布失败:映射已提交,按「没做成或结局未知」报(退出码 2),不是拒绝;重跑同一条命令补上发布。
func TestPublishFailureAfterCommitIsNotARefusalAndARerunRepairs(t *testing.T) {
	store := newMemStore(nil)
	copies := newMemCopies()
	copies.putErr = errors.New("etcd request timed out")
	out, err := runModeWithCopies(store, copies, modeMerge, 5, 6, false)
	if exitCodeOf(err) != exitFailed || !strings.Contains(err.Error(), "the mapping is committed") {
		t.Fatalf("err = %v, want exit 2 saying the mapping is committed", err)
	}
	if _, ok := store.rows[5]; !ok || !strings.Contains(out, "merged: zone 5 -> zone 6") {
		t.Fatalf("the mapping is committed and reported before publishing: out=%q rows=%v", out, store.rows)
	}
	copies.putErr = nil
	out, err = runModeWithCopies(store, copies, modeMerge, 5, 6, false)
	if err != nil || copies.value.Version != 1 || !strings.Contains(out, "already merged") || !strings.Contains(out, "published version 1") {
		t.Fatalf("the re-run publishes: out=%q err=%v copy=%+v", out, err, copies.value)
	}

	// 提交之后读不回第 0 片、读不到副本,同样是退出码 2。
	store.committedErr = errors.New("connection reset")
	if _, err := runModeWithCopies(store, newMemCopies(), modeMerge, 7, 6, false); exitCodeOf(err) != exitFailed {
		t.Fatalf("err = %v", err)
	}
	// 读回时撞上的拒绝(route_meta 没了)同样是退出码 2:映射已经提交,不能报成「校验拒绝、什么都没做」。
	store.committedErr = refusedf("route_meta has no row id=1")
	if _, err := runModeWithCopies(store, newMemCopies(), modeMerge, 9, 6, false); exitCodeOf(err) != exitFailed {
		t.Fatalf("a refusal read back after the commit must still be exit 2: err = %v", err)
	}
	store.committedErr = nil
	unreadable := newMemCopies()
	unreadable.getErr = errors.New("etcd unavailable")
	if _, err := runModeWithCopies(store, unreadable, modeMerge, 8, 6, false); exitCodeOf(err) != exitFailed {
		t.Fatalf("err = %v", err)
	}
}

// 副本领先于第 0 片(第 0 片回档过)、同版本内容不同、世代不同:映射照样已提交,但不发布,按退出码 2 报出原因。
func TestPublishRefusesACopyThatDisagreesWithShardZero(t *testing.T) {
	for _, tc := range []struct {
		name      string
		published mergedzonecopy.Mapping
		want      string
	}{
		{"副本领先", mergedzonecopy.Mapping{Generation: memGeneration, Version: 9, Merges: map[uint32]uint32{1: 2}}, "behind version 9"},
		{"同版本内容不同", mergedzonecopy.Mapping{Generation: memGeneration, Version: 1, Merges: map[uint32]uint32{5: 7}}, "different contents"},
		{"世代不同", mergedzonecopy.Mapping{Generation: "another", Merges: map[uint32]uint32{}}, "another directory database"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemStore(nil)
			copies := &memCopies{value: tc.published, revision: 3}
			_, err := runModeWithCopies(store, copies, modeMerge, 5, 6, false)
			if exitCodeOf(err) != exitFailed || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v", err)
			}
			if copies.puts != 0 || copies.revision != 3 {
				t.Fatalf("nothing is published: puts=%d revision=%d", copies.puts, copies.revision)
			}
		})
	}
}

// 发布撞上别的写者:重读,对方已经发布了同一个版本就不再写。
func TestPublishRetriesWhenAnotherWriterWins(t *testing.T) {
	store := newMemStore(nil)
	copies := newMemCopies()
	copies.racer = func(c *memCopies) {
		c.value = mergedzonecopy.Mapping{Generation: memGeneration, Version: 1, Merges: map[uint32]uint32{5: 6}}
		c.revision, c.racer = c.revision+1, nil
	}
	out, err := runModeWithCopies(store, copies, modeMerge, 5, 6, false)
	if err != nil || copies.puts != 1 || !strings.Contains(out, "already holds version 1") {
		t.Fatalf("out=%q err=%v puts=%d", out, err, copies.puts)
	}
}

// 真写没有 etcd 副本时什么都不写就拒绝(参数层已经拦下,这是 execute 自己的兜底);dry-run 与审计不要副本。
func TestRealWritesNeedTheEtcdCopy(t *testing.T) {
	store := newMemStore(nil)
	var out bytes.Buffer
	err := execute(context.Background(), store, nil, options{mode: modeMerge, sourceZone: 5, targetZone: 6}, testNow, &out)
	if !errors.Is(err, errRefused) || len(store.locked) != 0 || len(store.rows) != 0 {
		t.Fatalf("err=%v locked=%v rows=%v", err, store.locked, store.rows)
	}
	if err := execute(context.Background(), store, nil, options{mode: modeMerge, sourceZone: 5, targetZone: 6, dryRun: true}, testNow, &out); err != nil {
		t.Fatal(err)
	}
	if err := execute(context.Background(), store, nil, options{mode: modeAudit}, testNow, &out); err != nil || !strings.Contains(out.String(), "etcd copy: not checked") {
		t.Fatalf("err=%v out=%q", err, out.String())
	}
}

// 审计比对副本:缺席、落后、一致照常退出;领先按拒绝退出;读不到副本是退出码 2;审计从不写副本。
func TestRunAudit_ComparesTheEtcdCopy(t *testing.T) {
	store := newMemStore(map[uint32]uint32{5: 6}) // 一条历史,版本 1
	for _, tc := range []struct {
		name     string
		copies   *memCopies
		wantCode int
		want     string
	}{
		{"缺席", newMemCopies(), exitOK, "absent"},
		{"落后", &memCopies{value: mergedzonecopy.Mapping{Generation: memGeneration, Merges: map[uint32]uint32{}}, revision: 2}, exitOK, "behind directory shard 0"},
		{"一致", &memCopies{value: mergedzonecopy.Mapping{Generation: memGeneration, Version: 1, Merges: map[uint32]uint32{5: 6}}, revision: 2}, exitOK, "version 1, 1 merged zone(s)"},
		{"领先", &memCopies{value: mergedzonecopy.Mapping{Generation: memGeneration, Version: 3, Merges: map[uint32]uint32{5: 6}}, revision: 2}, exitRefused, "behind version 3"},
		{"读不到", &memCopies{getErr: errors.New("etcd unavailable")}, exitFailed, "etcd unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runModeWithCopies(store, tc.copies, modeAudit, 0, 0, false)
			if exitCodeOf(err) != tc.wantCode {
				t.Fatalf("exit = %d (err %v), want %d\n%s", exitCodeOf(err), err, tc.wantCode, out)
			}
			if !strings.Contains(out+fmt.Sprint(err), tc.want) {
				t.Fatalf("output misses %q:\n%s\nerr: %v", tc.want, out, err)
			}
			if tc.copies.puts != 0 {
				t.Fatal("the audit never writes the copy")
			}
		})
	}
}
