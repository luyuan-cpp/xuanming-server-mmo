package main

// 搬库(relocate.go / relocate_run.go / relocate_manifest.go,docs/design/player-storage-placement.md §9)的纯单测。
// 状态机经三个接缝注入内存替身,不连库:fakePlacements 按 placementCASScript 的语义逐条执行 CAS,fakeLocks 模拟
// go/db 排序锁,fakeRows 模拟 R3 / R4。真 Redis 上的脚本、真 MySQL 上的 R3 事务与端到端在 placement_integration_test.go。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	relocTestS   = uint32(901)
	relocTestT   = uint32(1000000)
	relocTestRun = "reloc-test-run"
)

// fakePlacements 是 relocatePlacements 的内存替身。cas 与 placementCASScript 同一判定顺序:
// 记录 → home → 围栏 → 写。
type fakePlacements struct {
	records map[uint64]string
	homes   map[uint64]string
	fences  map[uint32]bool
	// beforeCAS 可选:每次 cas 执行之前调用,模拟并发写者。
	beforeCAS func(ops []placementCAS)
	// casErrAfter > 0:累计执行这么多条 CAS 之后返回错误(模拟 pipeline 中途失败)。
	casErrAfter int
	casDone     int
}

func newFakePlacements() *fakePlacements {
	return &fakePlacements{records: map[uint64]string{}, homes: map[uint64]string{}, fences: map[uint32]bool{}}
}

func (f *fakePlacements) read(_ context.Context, ids []uint64) ([]placementRead, error) {
	out := make([]placementRead, 0, len(ids))
	for _, id := range ids {
		var rec, home any
		if v, ok := f.records[id]; ok {
			rec = v
		}
		if v, ok := f.homes[id]; ok {
			home = v
		}
		rd, err := decodePlacementRead(rec, home)
		if err != nil {
			return nil, err
		}
		out = append(out, rd)
	}
	return out, nil
}

func (f *fakePlacements) cas(_ context.Context, ops []placementCAS) ([]placementCASResult, error) {
	if f.beforeCAS != nil {
		f.beforeCAS(ops)
	}
	var out []placementCASResult
	for _, op := range ops {
		if err := op.validate(); err != nil {
			return out, err
		}
		if f.casErrAfter > 0 && f.casDone >= f.casErrAfter {
			return out, errors.New("injected CAS pipeline failure")
		}
		f.casDone++
		cur, curOK := f.records[op.PlayerID]
		home, homeOK := f.homes[op.PlayerID]
		res := placementCASResult{CurrentRecord: cur, CurrentHome: home}
		switch {
		case op.ExpectAbsent && curOK, !op.ExpectAbsent && (!curOK || cur != op.ExpectRecord):
			res.Outcome = placementCASRecordChanged
		case op.ExpectHome != "" && (!homeOK || home != op.ExpectHome):
			res.Outcome = placementCASHomeChanged
		case op.FenceZone != 0 && f.fences[op.FenceZone]:
			res.Outcome = placementCASFenced
		default:
			f.records[op.PlayerID] = op.NewValue
			res.Outcome = placementCASWritten
		}
		out = append(out, res)
	}
	return out, nil
}

// fakeLocks:held[key] = 还会被观察到「存在」的次数,-1 = 一直存在。
type fakeLocks struct {
	held   map[string]int
	probes int
	err    error
}

func (l *fakeLocks) exists(_ context.Context, keys []string) ([]bool, error) {
	l.probes++
	if l.err != nil {
		return nil, l.err
	}
	out := make([]bool, len(keys))
	for i, k := range keys {
		switch n := l.held[k]; {
		case n < 0:
			out[i] = true
		case n > 0:
			out[i] = true
			l.held[k] = n - 1
		}
	}
	return out, nil
}

// fakeRows 模拟 R3 / R4。
type fakeRows struct {
	copyErr    map[uint64]error
	cold       map[uint64]int
	differ     map[uint64]bool
	compareErr error
	afterCopy  func(id uint64)
	copied     []uint64
}

func newFakeRows() *fakeRows {
	return &fakeRows{copyErr: map[uint64]error{}, cold: map[uint64]int{}, differ: map[uint64]bool{}}
}

func (r *fakeRows) copyPlayer(_ context.Context, id uint64) (int, error) {
	if err := r.copyErr[id]; err != nil {
		return 0, err
	}
	r.copied = append(r.copied, id)
	if r.afterCopy != nil {
		r.afterCopy(id)
	}
	return r.cold[id], nil
}

func (r *fakeRows) mismatched(_ context.Context, ids []uint64) ([]uint64, error) {
	if r.compareErr != nil {
		return nil, r.compareErr
	}
	var out []uint64
	for _, id := range ids {
		if r.differ[id] {
			out = append(out, id)
		}
	}
	return out, nil
}

// fakeClock 让 R2 的等待不依赖墙钟:sleep 直接把时钟拨过去。
type fakeClock struct {
	t      time.Time
	sleeps int
}

func (c *fakeClock) now() time.Time { return c.t }
func (c *fakeClock) sleep(_ context.Context, d time.Duration) error {
	c.sleeps++
	c.t = c.t.Add(d)
	return nil
}

var relocTestTables = []string{"player_database", "player_database_1"}

func relocTestManifest(ids ...uint64) *relocateManifest {
	return newRelocateManifest(relocTestRun, relocTestS, relocTestT, 1, relocTestTables, ids, "test",
		time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC))
}

// relocTestEngine 装配替身;saves 记下每次落盘时的逐人状态与当时已拷贝的人数。
func relocTestEngine(m *relocateManifest, p *fakePlacements, l *fakeLocks, r *fakeRows) (*relocateEngine, *fakeClock, *[]string) {
	clock := &fakeClock{t: time.Date(2026, 9, 28, 1, 0, 0, 0, time.UTC)}
	saves := &[]string{}
	save := func(m *relocateManifest) error {
		*saves = append(*saves, fmt.Sprintf("copied=%d %s", len(r.copied), relocStates(m)))
		return nil
	}
	e := newRelocateEngine(m, save, p, l, r, map[uint32]bool{901: true, 902: true}, 100, time.Second)
	e.lockPoll = 100 * time.Millisecond
	e.sleep, e.now = clock.sleep, clock.now
	return e, clock, saves
}

// 编译期确认替身满足接缝(接口一改,这里先报错,而不是在某个用例里才发现)。
var (
	_ relocatePlacements = (*fakePlacements)(nil)
	_ relocateLocks      = (*fakeLocks)(nil)
	_ relocateRows       = (*fakeRows)(nil)
)

// relocStates 是「id:状态」列表,断言用。
func relocStates(m *relocateManifest) string {
	parts := make([]string, len(m.Players))
	for i, p := range m.Players {
		parts[i] = fmt.Sprintf("%d:%s", p.PlayerID, p.State)
	}
	return strings.Join(parts, " ")
}

func relocPlayer(m *relocateManifest, id uint64) relocatePlayer {
	for _, p := range m.Players {
		if p.PlayerID == id {
			return p
		}
	}
	return relocatePlayer{}
}

// ── R1~R5 ────────────────────────────────────────────────────────

func TestRelocateEngine_SwitchesEligiblePlayersAndSkipsTheRest(t *testing.T) {
	p := newFakePlacements()
	// 1:记录在 S,冻结后切到 T:4。2:无记录、home == S,按 v=1 冻结、切到 T:2。
	p.records[1], p.homes[1] = "901:3", "901"
	p.homes[2] = "901"
	// 3:有效落点是 902。4:别的搬库正在挪他。5:无记录,有效落点是 home 902。
	p.records[3], p.homes[3] = "902:1", "901"
	p.records[4], p.homes[4] = "901:2:frozen:reloc-other", "901"
	p.homes[5] = "902"
	// 6:home 所在 zone 正在合服(围栏在 CAS 那一刻挡住)。7:没有 home,拼不出 topic。8:home 所在 zone 没核实过能力标记。
	p.records[6], p.homes[6], p.fences[902] = "901:1", "902", true
	p.records[7] = "901:1"
	p.records[8], p.homes[8] = "901:1", "903"
	rows := newFakeRows()
	rows.cold[2] = 3
	m := relocTestManifest(1, 2, 3, 4, 5, 6, 7, 8)
	e, _, saves := relocTestEngine(m, p, &fakeLocks{held: map[string]int{}}, rows)

	if err := e.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "1:switched 2:switched 3:skipped 4:skipped 5:skipped 6:skipped 7:skipped 8:skipped"
	if got := relocStates(m); got != want {
		t.Fatalf("states = %s\nwant     %s", got, want)
	}
	if p.records[1] != "1000000:4" || p.records[2] != "1000000:2" {
		t.Errorf("switched records = %q / %q, want 1000000:4 / 1000000:2 (v+1; no record counts as v=1)", p.records[1], p.records[2])
	}
	if p.records[3] != "902:1" || p.records[4] != "901:2:frozen:reloc-other" || p.records[6] != "901:1" {
		t.Errorf("skipped players' records were touched: %v", p.records)
	}
	if _, ok := p.records[5]; ok {
		t.Error("a skipped player without a record must not get one")
	}
	if !reflect.DeepEqual(rows.copied, []uint64{1, 2}) {
		t.Errorf("copied %v, want only the frozen players [1 2]", rows.copied)
	}
	if got := relocPlayer(m, 2); got.ColdCopyRows != 3 || got.Final != "1000000:2" || got.Version != 1 || got.HomeZone != 901 {
		t.Errorf("player 2 = %+v, want cold_copy_rows=3 final=1000000:2 version=1 home=901", got)
	}
	for id, want := range map[uint64]string{3: "effective storage is 902", 4: "already frozen by run reloc-other",
		5: "home zone 902", 6: "being merged", 7: "no player:zone", 8: "not capability-checked"} {
		if r := relocPlayer(m, id).Reason; !strings.Contains(r, want) {
			t.Errorf("player %d reason %q does not contain %q", id, r, want)
		}
	}
	// 清单先于写:第一次落盘在 R1 之后、任何拷贝之前,且已记下冻结者。
	if len(*saves) < 2 || !strings.HasPrefix((*saves)[0], "copied=0 ") || !strings.Contains((*saves)[0], "1:frozen 2:frozen") {
		t.Errorf("saves = %v, want the freeze persisted before the first copy", *saves)
	}
}

func TestRelocateEngine_LockWaitTimeoutUnfreezesTheWholeBatch(t *testing.T) {
	p := newFakePlacements()
	p.records[1], p.homes[1] = "901:3", "901"
	p.homes[2] = "901"
	locks := &fakeLocks{held: map[string]int{orderingLockKey(dbTaskTopic(901, 1), 1, "player_database"): -1}}
	rows := newFakeRows()
	m := relocTestManifest(1, 2)
	e, clock, _ := relocTestEngine(m, p, locks, rows)

	if err := e.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := relocStates(m); got != "1:unfrozen 2:unfrozen" {
		t.Fatalf("states = %s, want the whole batch unfrozen", got)
	}
	// 解冻回稳定态,版本不变(P-2);无记录者留下 "{S}:1",有效落点与冻结前相同。
	if p.records[1] != "901:3" || p.records[2] != "901:1" {
		t.Errorf("records after unfreeze = %q / %q, want 901:3 / 901:1", p.records[1], p.records[2])
	}
	if len(rows.copied) != 0 {
		t.Errorf("nothing may be copied after a lock-wait timeout, copied %v", rows.copied)
	}
	if r := relocPlayer(m, 2).Reason; !strings.Contains(r, "R2") || !strings.Contains(r, "still held") {
		t.Errorf("reason = %q", r)
	}
	if clock.sleeps < 9 || clock.sleeps > 11 {
		t.Errorf("slept %d times, want ~10 polls of 100ms within the 1s wait", clock.sleeps)
	}
}

func TestRelocateEngine_WaitsForLocksThatClear(t *testing.T) {
	p := newFakePlacements()
	p.records[1], p.homes[1] = "901:3", "901"
	// 冻结之前选好库的那条写还要被观察到 3 次;之后消失 —— 等到它、再拷。
	key := orderingLockKey(dbTaskTopic(901, 1), 1, "player_database_1")
	locks := &fakeLocks{held: map[string]int{key: 3}}
	rows := newFakeRows()
	m := relocTestManifest(1)
	e, clock, _ := relocTestEngine(m, p, locks, rows)
	if err := e.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if relocPlayer(m, 1).State != relocSwitched || clock.sleeps != 3 {
		t.Fatalf("state=%s sleeps=%d, want switched after 3 polls", relocPlayer(m, 1).State, clock.sleeps)
	}
}

func TestRelocateEngine_CopyFailureUnfreezesOnlyThatPlayer(t *testing.T) {
	p := newFakePlacements()
	p.records[1], p.homes[1] = "901:3", "901"
	p.homes[2] = "901"
	rows := newFakeRows()
	rows.copyErr[1] = errors.New("1213 deadlock")
	m := relocTestManifest(1, 2)
	e, _, _ := relocTestEngine(m, p, &fakeLocks{held: map[string]int{}}, rows)
	if err := e.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := relocStates(m); got != "1:unfrozen 2:switched" {
		t.Fatalf("states = %s", got)
	}
	if p.records[1] != "901:3" || !strings.Contains(relocPlayer(m, 1).Reason, "R3 copy failed") {
		t.Errorf("player 1: record %q reason %q", p.records[1], relocPlayer(m, 1).Reason)
	}
}

func TestRelocateEngine_CompareMismatchOrFailureUnfreezes(t *testing.T) {
	p := newFakePlacements()
	p.records[1], p.homes[1] = "901:3", "901"
	p.homes[2] = "901"
	rows := newFakeRows()
	rows.differ[2] = true
	m := relocTestManifest(1, 2)
	e, _, _ := relocTestEngine(m, p, &fakeLocks{held: map[string]int{}}, rows)
	if err := e.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := relocStates(m); got != "1:switched 2:unfrozen" || p.records[2] != "901:1" {
		t.Fatalf("mismatch: states = %s, record 2 = %q", got, p.records[2])
	}
	if r := relocPlayer(m, 2).Reason; !strings.Contains(r, "R4") {
		t.Errorf("reason = %q", r)
	}

	// 比对查询本身失败:证明不了 T 是完整的一份,一个都不切。
	p = newFakePlacements()
	p.records[1], p.homes[1] = "901:3", "901"
	p.homes[2] = "901"
	rows = newFakeRows()
	rows.compareErr = errors.New("connection reset")
	m = relocTestManifest(1, 2)
	e, _, _ = relocTestEngine(m, p, &fakeLocks{held: map[string]int{}}, rows)
	if err := e.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := relocStates(m); got != "1:unfrozen 2:unfrozen" {
		t.Fatalf("compare failure: states = %s, want everyone unfrozen", got)
	}
}

func TestRelocateEngine_SwitchCASFailureIsReportedAndLeftAsIs(t *testing.T) {
	p := newFakePlacements()
	p.records[1], p.homes[1] = "901:3", "901"
	p.homes[2] = "901"
	rows := newFakeRows()
	rows.afterCopy = func(id uint64) {
		if id == 1 {
			// 有人在冻结期间动了记录(不该发生,工具不能替人决定)。
			p.records[1] = "902:9"
		}
	}
	m := relocTestManifest(1, 2)
	e, _, _ := relocTestEngine(m, p, &fakeLocks{held: map[string]int{}}, rows)
	if err := e.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := relocStates(m); got != "1:cas_failed 2:switched" {
		t.Fatalf("states = %s", got)
	}
	if p.records[1] != "902:9" || !strings.Contains(relocPlayer(m, 1).Reason, `"902:9"`) {
		t.Errorf("a failed switch must leave the record as found and say what it found: %q / %q", p.records[1], relocPlayer(m, 1).Reason)
	}
}

func TestRelocateEngine_ResumeAdoptsThisRunsFreezeAndSwitch(t *testing.T) {
	p := newFakePlacements()
	for id := uint64(1); id <= 4; id++ {
		p.homes[id] = "901"
	}
	// 1:R5 之后、落盘之前崩溃,已切换。2:R1 之后、落盘之前崩溃,已冻结。
	// 3:解冻之后、落盘之前崩溃,回到稳定态 —— 重新判定、重新冻结。4:终态,续跑不再碰。
	p.records[1] = "1000000:4"
	p.records[2] = frozenValue(901, 1, relocTestRun)
	p.records[3] = "901:1"
	p.records[4] = "1000000:9"
	m := relocTestManifest(1, 2, 3, 4)
	m.Players[0].State, m.Players[0].Version = relocFrozen, 3
	m.Players[2].State, m.Players[2].Version = relocFrozen, 1
	m.Players[3].State, m.Players[3].Final = relocSwitched, "1000000:9"
	rows := newFakeRows()
	e, _, _ := relocTestEngine(m, p, &fakeLocks{held: map[string]int{}}, rows)
	if err := e.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := relocStates(m); got != "1:switched 2:switched 3:switched 4:switched" {
		t.Fatalf("states = %s", got)
	}
	if !reflect.DeepEqual(rows.copied, []uint64{2, 3}) {
		t.Errorf("copied %v, want [2 3] (player 1 was already switched)", rows.copied)
	}
	if p.records[1] != "1000000:4" || p.records[2] != "1000000:2" || p.records[3] != "1000000:2" || p.records[4] != "1000000:9" {
		t.Errorf("records = %v", p.records)
	}
	if relocPlayer(m, 1).Final != "1000000:4" {
		t.Errorf("an adopted switch must record its final placement: %+v", relocPlayer(m, 1))
	}
}

func TestRelocateEngine_AdoptedFreezeWithoutAHomeIsUnfrozen(t *testing.T) {
	p := newFakePlacements()
	// 本次冻结的,但 player:zone 已经没了:拼不出 R2 的 topic。
	p.records[1] = frozenValue(901, 2, relocTestRun)
	m := relocTestManifest(1)
	e, _, _ := relocTestEngine(m, p, &fakeLocks{held: map[string]int{}}, newFakeRows())
	if err := e.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if relocPlayer(m, 1).State != relocUnfrozen || p.records[1] != "901:2" {
		t.Fatalf("state=%s record=%q, want unfrozen back to 901:2", relocPlayer(m, 1).State, p.records[1])
	}
}

func TestRelocateEngine_CASPipelineFailureStopsThenResumeFinishes(t *testing.T) {
	p := newFakePlacements()
	p.records[1], p.homes[1] = "901:3", "901"
	p.homes[2] = "901"
	// R1 的第一条 CAS 生效,第二条所在的 pipeline 报错。
	p.casErrAfter = 1
	rows := newFakeRows()
	m := relocTestManifest(1, 2)
	e, _, _ := relocTestEngine(m, p, &fakeLocks{held: map[string]int{}}, rows)
	if err := e.run(context.Background()); err == nil {
		t.Fatal("a Redis failure during R1 must stop the run")
	}
	if got := relocStates(m); got != "1:frozen 2:pending" {
		t.Fatalf("after the failure states = %s, want the confirmed freeze kept and the unknown one pending", got)
	}
	if len(rows.copied) != 0 {
		t.Fatalf("nothing may be copied after a failed R1, copied %v", rows.copied)
	}
	p.casErrAfter = 0
	if err := e.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := relocStates(m); got != "1:switched 2:switched" {
		t.Fatalf("resume: states = %s", got)
	}
}

func TestRelocateEngine_Abort(t *testing.T) {
	setup := func() (*relocateManifest, *fakePlacements) {
		p := newFakePlacements()
		// 1:冻结中 → 解冻回 901:3。2:已切换(清单来不及记)→ 认回 switched。3:从没被本次冻结 → skipped。
		// 4:冻结了、清单还是 pending → 按记录自身的版本解冻。5:终态,不碰。
		p.records[1] = frozenValue(901, 3, relocTestRun)
		p.records[2] = "1000000:2"
		p.records[3] = "901:1"
		p.records[4] = frozenValue(901, 1, relocTestRun)
		p.records[5] = "1000000:7"
		m := relocTestManifest(1, 2, 3, 4, 5)
		m.Players[0].State, m.Players[0].Version = relocFrozen, 3
		m.Players[1].State, m.Players[1].Version = relocFrozen, 1
		m.Players[4].State = relocSwitched
		return m, p
	}

	m, p := setup()
	e, _, _ := relocTestEngine(m, p, nil, nil)
	rep, err := e.abort(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Unfrozen != 2 || rep.Switched != 1 || rep.Untouched != 1 || rep.Failed != 0 {
		t.Fatalf("dry-run report = %s", rep)
	}
	if p.records[1] != frozenValue(901, 3, relocTestRun) || relocPlayer(m, 1).State != relocFrozen {
		t.Fatal("a dry-run abort must not write")
	}

	m, p = setup()
	e, _, _ = relocTestEngine(m, p, nil, nil)
	rep, err = e.abort(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Unfrozen != 2 || rep.Switched != 1 || rep.Untouched != 1 {
		t.Fatalf("report = %s", rep)
	}
	if got := relocStates(m); got != "1:unfrozen 2:switched 3:skipped 4:unfrozen 5:switched" {
		t.Fatalf("states = %s", got)
	}
	if p.records[1] != "901:3" || p.records[4] != "901:1" || p.records[2] != "1000000:2" || p.records[3] != "901:1" {
		t.Errorf("records = %v", p.records)
	}
}

// ── R1 判定 ──────────────────────────────────────────────────────

func TestDecideFreeze(t *testing.T) {
	caps := map[uint32]bool{901: true, 902: true}
	for _, c := range []struct {
		name    string
		p       relocatePlayer
		rd      placementRead
		kind    freezeKind
		version uint64
		reason  string
	}{
		{"stable record on S", relocatePlayer{}, placementReadOf(t, "901:3", "902"), freezeGo, 3, ""},
		{"no record, home == S", relocatePlayer{}, placementReadOf(t, nil, "901"), freezeGo, 1, ""},
		{"record elsewhere", relocatePlayer{}, placementReadOf(t, "902:1", "901"), freezeSkip, 0, "effective storage is 902"},
		{"no record, home elsewhere", relocatePlayer{}, placementReadOf(t, nil, "902"), freezeSkip, 0, "home zone 902"},
		{"frozen by another run", relocatePlayer{}, placementReadOf(t, "901:3:frozen:other", "901"), freezeSkip, 0, "already frozen by run other"},
		{"malformed record", relocatePlayer{}, placementReadOf(t, "901:x", "901"), freezeSkip, 0, "malformed placement record"},
		{"malformed home", relocatePlayer{}, placementReadOf(t, "901:1", "abc"), freezeSkip, 0, "player:zone is malformed"},
		{"no home", relocatePlayer{}, placementReadOf(t, "901:1", nil), freezeSkip, 0, "no player:zone"},
		{"home not capability-checked", relocatePlayer{}, placementReadOf(t, "901:1", "903"), freezeSkip, 0, "not capability-checked"},
		{"adopt this run's freeze", relocatePlayer{}, placementReadOf(t, "901:4:frozen:"+relocTestRun, "901"), freezeAdoptFrozen, 4, ""},
		{"adopt this run's switch", relocatePlayer{Version: 4}, placementReadOf(t, "1000000:5", "901"), freezeAdoptSwitched, 0, ""},
		// 版本对不上的目标记录不是本次切的:当成「有效落点不是 S」跳过。
		{"a foreign switch is not adopted", relocatePlayer{Version: 4}, placementReadOf(t, "1000000:9", "901"), freezeSkip, 0, "effective storage is 1000000"},
	} {
		d := decideFreeze(c.p, c.rd, relocTestS, relocTestT, relocTestRun, caps)
		if d.kind != c.kind || d.version != c.version || !strings.Contains(d.reason, c.reason) {
			t.Errorf("%s: got kind=%d version=%d reason=%q, want kind=%d version=%d reason~%q",
				c.name, d.kind, d.version, d.reason, c.kind, c.version, c.reason)
		}
	}
	// 冻结 CAS 的前提:记录原值(或缺席)与 home 原值都是刚读到的,围栏按 home 查,新值是冻结值。
	d := decideFreeze(relocatePlayer{PlayerID: 42}, placementReadOf(t, "901:3", "902"), relocTestS, relocTestT, relocTestRun, caps)
	want := placementCAS{PlayerID: 42, ExpectRecord: "901:3", ExpectHome: "902", FenceZone: 902, NewValue: "901:3:frozen:" + relocTestRun}
	if d.op != want {
		t.Errorf("freeze op = %+v, want %+v", d.op, want)
	}
	d = decideFreeze(relocatePlayer{PlayerID: 43}, placementReadOf(t, nil, "901"), relocTestS, relocTestT, relocTestRun, caps)
	if !d.op.ExpectAbsent || d.op.NewValue != "901:1:frozen:"+relocTestRun || d.op.FenceZone != 901 {
		t.Errorf("no-record freeze op = %+v", d.op)
	}
	// S 是非 zone 库时,无记录的人不可能以它为有效落点。
	if d := decideFreeze(relocatePlayer{}, placementReadOf(t, nil, "901"), relocTestT, 902, relocTestRun, caps); d.kind != freezeSkip {
		t.Errorf("no record can never be effective on a non-zone store: %+v", d)
	}
}

// ── 键与 SQL 形状 ────────────────────────────────────────────────

func TestOrderingLockKeyMirrorsGoDB(t *testing.T) {
	// go/db:locker.NewRedisLocker 的前缀 "distributed:lock:" + orderingLockKey "kafka:ordering:{topic}:{key}:{msgType}"。
	if got := orderingLockKey("db_task_zone_101", 42, "player_database"); got != "distributed:lock:kafka:ordering:db_task_zone_101:42:player_database" {
		t.Fatalf("got %q", got)
	}
	keys := relocateLockKeys([]relocatePlayer{{PlayerID: 7, HomeZone: 101}, {PlayerID: 8, HomeZone: 102}},
		[]string{"player_database", "player_database_1"}, 3)
	want := []string{
		"distributed:lock:kafka:ordering:db_task_zone_101_g3:7:player_database",
		"distributed:lock:kafka:ordering:db_task_zone_101_g3:7:player_database_1",
		"distributed:lock:kafka:ordering:db_task_zone_102_g3:8:player_database",
		"distributed:lock:kafka:ordering:db_task_zone_102_g3:8:player_database_1",
	}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys = %v\nwant %v (topic by each player's home zone and the topic generation)", keys, want)
	}
}

func TestRelocateSQLShapes(t *testing.T) {
	// 形状即锁序:WHERE 只有完整主键等值(一名玩家、一行),不经过任何二级索引。
	if got := relocateDeleteSQL("player_store_1000000_db.player_database"); got !=
		"DELETE FROM player_store_1000000_db.player_database WHERE player_id = ?" {
		t.Errorf("delete = %q", got)
	}
	got := relocateCopySQL("player_store_1000000_db.player_database", "zone_901_db.player_database", []string{"player_id", "bag"})
	if want := "INSERT INTO player_store_1000000_db.player_database (`player_id`,`bag`) SELECT `player_id`,`bag` " +
		"FROM zone_901_db.player_database WHERE player_id = ?"; got != want {
		t.Errorf("copy = %q\nwant %q", got, want)
	}
	if strings.Contains(got, " IN (") || strings.Count(got, "?") != 1 {
		t.Errorf("the copy must be a single primary-key equality: %q", got)
	}
	if got := byteIdenticalRowCondition([]string{"player_id", "bag"}); got !=
		"CAST(d.`player_id` AS BINARY) <=> CAST(s.`player_id` AS BINARY) AND CAST(d.`bag` AS BINARY) <=> CAST(s.`bag` AS BINARY)" {
		t.Errorf("byte-identical condition = %q", got)
	}
}

func TestRowsDiffer(t *testing.T) {
	ids := []uint64{1, 2, 3, 4, 5}
	// 1:两边都没有 → 相同;2:只有 S 有 → 不同;3:只有 T 有 → 不同;4:两边都有且相同;5:两边都有但不同。
	got := rowsDiffer(ids, []uint64{1, 3}, []uint64{1, 2}, map[uint64]bool{4: true})
	if !reflect.DeepEqual(got, []uint64{2, 3, 5}) {
		t.Fatalf("differ = %v, want [2 3 5]", got)
	}
}

// ── 参数 / 清单 ──────────────────────────────────────────────────

func TestParsePlayerIDList(t *testing.T) {
	got, err := parsePlayerIDList(" 3, 1,3,,2 ")
	if err != nil || !reflect.DeepEqual(got, []uint64{1, 2, 3}) {
		t.Fatalf("got %v %v", got, err)
	}
	if got, err := parsePlayerIDList(""); err != nil || got != nil {
		t.Fatalf("empty = %v %v", got, err)
	}
	for _, bad := range []string{"0", "x", "-1", "18446744073709551616"} {
		if _, err := parsePlayerIDList(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

func TestValidateRelocateStorages(t *testing.T) {
	if err := validateRelocateStorages(901, 1000000); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][2]uint32{{0, 1}, {1, 0}, {901, 901}} {
		if err := validateRelocateStorages(c[0], c[1]); err == nil {
			t.Errorf("%v must be refused", c)
		}
	}
}

func TestNewRelocateRunIDFitsTheFrozenValue(t *testing.T) {
	id := newRelocateRunID(901, 1000000, time.Now())
	if !validRunID(id) {
		t.Fatalf("run_id %q cannot appear in a frozen placement value", id)
	}
	if _, err := parsePlacement(frozenValue(901, 1, id)); err != nil {
		t.Fatalf("the frozen value built from %q does not parse: %v", id, err)
	}
	if id == newRelocateRunID(901, 1000000, time.Now()) {
		t.Fatal("two runs must get different run ids")
	}
}

func TestRelocateManifestRoundTripAndRefusals(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reloc.json")
	m := relocTestManifest(3, 1, 2)
	if got := relocStates(m); got != "1:pending 2:pending 3:pending" {
		t.Fatalf("new manifest players must be sorted and pending: %s", got)
	}
	m.Players[0].State, m.Players[0].Version, m.Players[0].Final = relocSwitched, 1, "1000000:2"
	if err := saveRelocateManifest(path, m); err != nil {
		t.Fatal(err)
	}
	back, err := loadRelocateManifest(path)
	if err != nil || back == nil || !reflect.DeepEqual(back.Players, m.Players) || back.RunID != relocTestRun {
		t.Fatalf("round trip = %+v %v", back, err)
	}
	if got, err := loadRelocateManifest(filepath.Join(dir, "absent.json")); got != nil || err != nil {
		t.Fatalf("a missing manifest is the first run: %v %v", got, err)
	}

	write := func(name string, mutate func(*relocateManifest)) string {
		cp := *relocTestManifest(1)
		mutate(&cp)
		p := filepath.Join(dir, name)
		if err := saveRelocateManifest(p, &cp); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for name, mutate := range map[string]func(*relocateManifest){
		"dry.json":     func(m *relocateManifest) { m.DryRun = true },
		"version.json": func(m *relocateManifest) { m.RelocateVersion = 2 },
		"kind.json":    func(m *relocateManifest) { m.Kind = "" },
		"runid.json":   func(m *relocateManifest) { m.RunID = "bad run" },
		"state.json":   func(m *relocateManifest) { m.Players[0].State = "moving" },
	} {
		if _, err := loadRelocateManifest(write(name, mutate)); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
	if _, err := loadRelocateManifest(dryRunManifestPath(path)); err == nil {
		t.Error("the dry-run preview suffix must be refused even before the file exists")
	}
	// 合服清单不是搬库清单。
	merge := filepath.Join(dir, "merge.json")
	if err := saveManifest(merge, newMergeManifest("r", 901, 902, "op", time.Now())); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRelocateManifest(merge); err == nil || !strings.Contains(err.Error(), "not a relocate manifest") {
		t.Errorf("a merge manifest must be refused: %v", err)
	}
	if raw, _ := os.ReadFile(path); !strings.Contains(string(raw), `"kind": "relocate"`) {
		t.Errorf("a relocate manifest must carry its kind: %s", raw)
	}
}

func TestCheckRelocateResume(t *testing.T) {
	m := relocTestManifest(1, 2)
	if err := checkRelocateResume(m, relocTestS, relocTestT, relocTestTables, 1, nil); err != nil {
		t.Fatalf("a matching resume must pass: %v", err)
	}
	if err := checkRelocateResume(m, relocTestS, relocTestT, relocTestTables, 1, []uint64{2, 1}); err != nil {
		t.Fatalf("the same player set in the flag must pass: %v", err)
	}
	for name, err := range map[string]error{
		"storages":   checkRelocateResume(m, relocTestS, 902, relocTestTables, 1, nil),
		"generation": checkRelocateResume(m, relocTestS, relocTestT, relocTestTables, 2, nil),
		"tables":     checkRelocateResume(m, relocTestS, relocTestT, []string{"player_database"}, 1, nil),
		"players":    checkRelocateResume(m, relocTestS, relocTestT, relocTestTables, 1, []uint64{1, 3}),
	} {
		if err == nil {
			t.Errorf("%s mismatch must be refused", name)
		}
	}
	m.Aborted = true
	if err := checkRelocateResume(m, relocTestS, relocTestT, relocTestTables, 1, nil); err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Errorf("an aborted run must not resume: %v", err)
	}
}

func TestRelocateManifestSummary(t *testing.T) {
	m := relocTestManifest(1, 2, 3)
	m.Players[0].State = relocSwitched
	m.Players[1].State, m.Players[1].Reason = relocSkipped, "why"
	if s := m.relocSummary(); s != "players=3 switched=1 unfrozen=0 skipped=1 cas_failed=0 frozen=0 pending=1" {
		t.Errorf("summary = %q", s)
	}
	if got := m.relocSample(relocSkipped); !reflect.DeepEqual(got, []string{"2: why"}) {
		t.Errorf("sample = %v", got)
	}
	for _, s := range []string{relocSwitched, relocSkipped, relocUnfrozen, relocCASFailed} {
		if !relocFinal(s) {
			t.Errorf("%s must be final", s)
		}
	}
	if relocFinal(relocPending) || relocFinal(relocFrozen) {
		t.Error("pending / frozen are not final")
	}
}
