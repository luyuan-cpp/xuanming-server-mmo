package assetop

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// 缺口清单第 25 条第四批(docs/design/assetop-redelivery-sharding-20261110.md §14.3):重投循环的活性 gauge
// assetop_loop_last_run_unixtime。口径在 shared/looplive(那里有它自己的用例),这里只测 Loop.Run 一侧的接线:
// 起来时写、每一轮 Tick 返回后写(读到期行失败的一轮也写)、panic 的一轮不写但循环不死、有意停止时删序列。

// countingDueStore 数 ListDue 被调了几次,并按 fail 决定这一次怎么坏:nil = 正常回空。
type countingDueStore struct {
	*fakeStore
	calls atomic.Int64
	fail  func() error // 返回非 nil 就把它当读失败;要模拟 panic 就在里面 panic
}

func (s *countingDueStore) ListDue(ctx context.Context, nowMs uint64, limit int) ([]uint64, error) {
	s.calls.Add(1)
	if s.fail != nil {
		if err := s.fail(); err != nil {
			return nil, err
		}
	}
	return s.fakeStore.ListDue(ctx, nowMs, limit)
}

// startLivenessLoop 起一个 5ms 一拍的重投循环,返回它的指标与停止函数(取消并等 Run 退出)。
func startLivenessLoop(t *testing.T, store Store) (*Metrics, func()) {
	t.Helper()
	cfg := DefaultLoopConfig()
	cfg.Interval = 5 * time.Millisecond
	m := NewMetrics(prometheus.NewRegistry(), "test")
	loop, err := NewLoop(cfg, store, appliedApplier(), m, fixedNow)
	if err != nil {
		t.Fatalf("NewLoop 失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		loop.Run(ctx)
	}()
	return m, func() {
		cancel()
		<-done
	}
}

// waitFor 在 2 秒内等 cond 成立;等不到就判红。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("2 秒内没等到:%s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// Run 一起来就写一次(不等第一拍);跑着时每拍往前写;ctx 结束、有意停止时删掉序列,停掉的循环不算卡住。
func TestRunMarksLivenessAtStartAndStopsItOnExit(t *testing.T) {
	store := &countingDueStore{fakeStore: newFakeStore()}
	m, stop := startLivenessLoop(t, store)
	waitFor(t, "循环起来之后应有活性序列", func() bool { _, ok := m.loopLive.Value(); return ok })

	// 把时刻拨回很早:之后跑完的一拍必须把它重新写成现在。
	m.loopLive.Mark(time.Unix(1000, 0))
	waitFor(t, "跑完一拍之后时刻应往前写", func() bool { got, _ := m.loopLive.Value(); return got > 1000 })

	stop()
	if _, ok := m.loopLive.Value(); ok {
		t.Fatal("Run 退出之后应删掉活性序列")
	}
}

// 读到期行失败的一拍也算跑完,照样写时刻:失败另有 assetop_store_errors_total{op="list"},一次库故障不该同时报成循环卡死。
func TestRunMarksLivenessOnFailedRounds(t *testing.T) {
	store := &countingDueStore{fakeStore: newFakeStore(), fail: func() error { return errors.New("database unavailable") }}
	m, stop := startLivenessLoop(t, store)
	defer stop()
	waitFor(t, "循环起来之后应有活性序列", func() bool { _, ok := m.loopLive.Value(); return ok })

	m.loopLive.Mark(time.Unix(1000, 0))
	waitFor(t, "读失败的一拍也应把时刻往前写", func() bool { got, _ := m.loopLive.Value(); return got > 1000 })
}

// 每一拍都 panic:循环不死(一拍一拍接着跑),但时刻不再前进 —— 停摆告警靠的就是这个。
func TestRunKeepsRunningButStopsMarkingWhenRoundsPanic(t *testing.T) {
	store := &countingDueStore{fakeStore: newFakeStore(), fail: func() error { panic("test: poisoned row") }}
	m, stop := startLivenessLoop(t, store)
	defer stop()
	waitFor(t, "循环起来之后应有活性序列", func() bool { _, ok := m.loopLive.Value(); return ok })

	m.loopLive.Mark(time.Unix(1000, 0))
	before := store.calls.Load()
	waitFor(t, "panic 之后循环应接着跑后面的拍", func() bool { return store.calls.Load() >= before+3 })
	if got, ok := m.loopLive.Value(); !ok || got != 1000 {
		t.Fatalf("panic 的拍不写时刻,值应停在 1000,实际 %v(ok=%t)", got, ok)
	}
}

// 不接指标(metrics 为 nil)的循环照常跑。
func TestRunWithoutMetrics(t *testing.T) {
	store := &countingDueStore{fakeStore: newFakeStore()}
	cfg := DefaultLoopConfig()
	cfg.Interval = 5 * time.Millisecond
	loop, err := NewLoop(cfg, store, appliedApplier(), nil, fixedNow)
	if err != nil {
		t.Fatalf("NewLoop 失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		loop.Run(ctx)
	}()
	waitFor(t, "没有指标的循环也应一拍一拍地跑", func() bool { return store.calls.Load() >= 2 })
	cancel()
	<-done
}
