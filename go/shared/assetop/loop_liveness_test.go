package assetop

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"shared/safego"
)

// 缺口清单第 25 条第四批(docs/design/assetop-redelivery-sharding-20261110.md §14.3):重投循环的活性 gauge
// assetop_loop_last_run_unixtime。循环起来时写一次,每跑完一轮写一次(这一轮成败都写,panic 的那一轮不写),退出时删掉序列。

// steppingClock 每被问一次时刻就往前走一秒,用例据此分得出值是哪一次写的。
type steppingClock struct{ unix atomic.Int64 }

func (c *steppingClock) now() time.Time { return time.Unix(c.unix.Add(1), 0) }

// panickingDueStore 读到期行时直接 panic,模拟一轮跑到一半崩掉。
type panickingDueStore struct{ *fakeStore }

func (panickingDueStore) ListDue(context.Context, uint64, int) ([]uint64, error) { panic("test: list due") }

func newLivenessLoop(t *testing.T, store Store, clock func() time.Time) (*Loop, *prometheus.Registry) {
	t.Helper()
	cfg := DefaultLoopConfig()
	cfg.Interval = 5 * time.Millisecond
	reg := prometheus.NewRegistry()
	loop, err := NewLoop(cfg, store, appliedApplier(), NewMetrics(reg, "test"), clock)
	if err != nil {
		t.Fatalf("NewLoop 失败: %v", err)
	}
	return loop, reg
}

// loopLastRun 从 reg 读 assetop_loop_last_run_unixtime;没有序列(或读不出)时 ok=false。不经 WithLabelValues 读:那会顺手造出序列。
func loopLastRun(reg *prometheus.Registry) (value float64, ok bool) {
	families, err := reg.Gather()
	if err != nil {
		return 0, false
	}
	for _, mf := range families {
		if mf.GetName() == "assetop_loop_last_run_unixtime" && len(mf.GetMetric()) == 1 {
			return mf.GetMetric()[0].GetGauge().GetValue(), true
		}
	}
	return 0, false
}

// 一轮跑完就写这一刻,读到期行失败的那一轮也写(失败另有 assetop_store_errors_total);这一轮 panic 时不写,值停在上一轮。
func TestRunTickMarksEachFinishedRound(t *testing.T) {
	clock := &steppingClock{}
	clock.unix.Store(1000)
	store := newFakeStore()
	loop, reg := newLivenessLoop(t, store, clock.now)
	if _, ok := loopLastRun(reg); ok {
		t.Fatal("循环没跑过时不该有活性序列")
	}

	loop.runTick(context.Background())
	first, ok := loopLastRun(reg)
	if !ok || first <= 1000 {
		t.Fatalf("跑完一轮应写这一刻,实际 ok=%t 值=%v", ok, first)
	}

	store.mu.Lock()
	store.listErr = errors.New("database unavailable")
	store.mu.Unlock()
	loop.runTick(context.Background())
	if second, ok := loopLastRun(reg); !ok || second <= first {
		t.Fatalf("读到期行失败的一轮也算跑完,应往前写:上一轮 %v,这一轮 %v(ok=%t)", first, second, ok)
	}

	broken, brokenReg := newLivenessLoop(t, panickingDueStore{newFakeStore()}, clock.now)
	broken.metrics.markLoopRun(time.Unix(500, 0))
	if safego.Run("assetop.reconcile_tick", func() { broken.runTick(context.Background()) }) {
		t.Fatal("夹具前提:这一轮应 panic")
	}
	if value, ok := loopLastRun(brokenReg); !ok || value != 500 {
		t.Fatalf("panic 的那一轮不写活性时刻,值应停在 500,实际 %v(ok=%t)", value, ok)
	}
}

// Run 一起来就写一次(不等第一拍),跑着时每拍往前写,退出时删掉序列:停掉的循环不算卡住。
func TestRunMarksLivenessAtStartAndClearsItOnExit(t *testing.T) {
	clock := &steppingClock{}
	clock.unix.Store(5000)
	loop, reg := newLivenessLoop(t, newFakeStore(), clock.now)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		loop.Run(ctx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if value, ok := loopLastRun(reg); ok && value > 5001 {
			break // 起来时写的是 5001,之后至少跑完了一拍
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("Run 起来之后 2 秒内应写过活性时刻并至少跑完一拍")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if _, ok := loopLastRun(reg); ok {
		t.Fatal("Run 退出之后应删掉活性序列")
	}
}

// 不接指标(metrics 为 nil)的循环照常跑,写活性是空操作。
func TestRunTickWithoutMetrics(t *testing.T) {
	cfg := DefaultLoopConfig()
	loop, err := NewLoop(cfg, newFakeStore(), appliedApplier(), nil, fixedNow)
	if err != nil {
		t.Fatalf("NewLoop 失败: %v", err)
	}
	loop.runTick(context.Background())
	loop.metrics.clearLoopRun()
}
