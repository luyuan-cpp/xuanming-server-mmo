package safego

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/zeromicro/go-zero/core/logx/logtest"
)

func panicCount(point string) float64 {
	return testutil.ToFloat64(panicTotal.WithLabelValues(point))
}

// waitFor 轮询等待 cond 成立,超时则失败。
// 后台 goroutine 的可见性靠等待,不靠 sleep 猜时间。
func waitFor(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("等待超时(%v): %s", timeout, desc)
}

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		point      string
		fn         func()
		wantOK     bool
		wantPanics float64
		wantLog    bool
	}{
		{
			name:       "正常返回",
			point:      "test.run.normal",
			fn:         func() {},
			wantOK:     true,
			wantPanics: 0,
		},
		{
			name:       "panic 被兜住",
			point:      "test.run.panic",
			fn:         func() { panic("炸了") },
			wantOK:     false,
			wantPanics: 1,
			wantLog:    true,
		},
		{
			name:       "panic(nil 指针解引用)同样被兜住",
			point:      "test.run.nilderef",
			fn:         func() { var p *int; _ = *p },
			wantOK:     false,
			wantPanics: 1,
			wantLog:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := logtest.NewCollector(t)

			if got := Run(tt.point, tt.fn); got != tt.wantOK {
				t.Fatalf("Run() = %v, 期望 %v", got, tt.wantOK)
			}
			if got := panicCount(tt.point); got != tt.wantPanics {
				t.Fatalf("safego_panic_total{point=%q} = %v, 期望 %v",
					tt.point, got, tt.wantPanics)
			}

			out := logs.String()
			if tt.wantLog {
				if !strings.Contains(out, EventGoroutinePanic) {
					t.Errorf("日志里没有稳定事件名 %q: %s", EventGoroutinePanic, out)
				}
				if !strings.Contains(out, tt.point) {
					t.Errorf("日志里没有点位名 %q: %s", tt.point, out)
				}
				// 完整栈必须打出来,否则等于只知道"炸了"不知道"哪炸的"。
				if !strings.Contains(out, "goroutine ") || !strings.Contains(out, "safego") {
					t.Errorf("日志里没有 goroutine 栈: %s", out)
				}
			} else if strings.Contains(out, EventGoroutinePanic) {
				t.Errorf("不该打 panic 事件: %s", out)
			}
		})
	}
}

func TestGoRecoversPanic(t *testing.T) {
	logtest.NewCollector(t)

	const point = "test.go.panic"
	done := make(chan struct{})

	// fn 先 panic;若 Go 没兜住,整个测试进程会直接崩掉。
	Go(point, func() {
		defer close(done)
		panic("后台 goroutine 炸了")
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Go 起的 goroutine 没跑起来")
	}

	waitFor(t, 2*time.Second, "panic 计数到 1", func() bool {
		return panicCount(point) == 1
	})
}

func TestGoNormal(t *testing.T) {
	logtest.NewCollector(t)

	const point = "test.go.normal"
	var ran atomic.Bool
	Go(point, func() { ran.Store(true) })

	waitFor(t, 2*time.Second, "fn 被执行", ran.Load)
	if got := panicCount(point); got != 0 {
		t.Fatalf("safego_panic_total{point=%q} = %v, 期望 0", point, got)
	}
}

// TestLoopSurvivesRoundPanic 是本包最关键的一条:
// **单轮 panic 不能杀掉循环本身**。
//
// 前三轮全部 panic,循环必须照常跑到第 6 轮以上,
// 并且 panic 计数恰好等于炸掉的轮数。
func TestLoopSurvivesRoundPanic(t *testing.T) {
	logtest.NewCollector(t)

	const point = "test.loop.round_panic"
	const panicRounds = 3

	var rounds atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	Loop(ctx, point, 2*time.Millisecond, func(context.Context) {
		if rounds.Add(1) <= panicRounds {
			panic("这一轮炸了")
		}
	})

	waitFor(t, 5*time.Second, "循环在前几轮 panic 之后仍跑满 6 轮", func() bool {
		return rounds.Load() >= 6
	})

	cancel()

	if got := panicCount(point); got != panicRounds {
		t.Fatalf("safego_panic_total{point=%q} = %v, 期望 %d(只有炸掉的那几轮)",
			point, got, panicRounds)
	}
}

func TestLoopStopsOnContextCancel(t *testing.T) {
	logtest.NewCollector(t)

	const point = "test.loop.cancel"
	var rounds atomic.Int32

	ctx, cancel := context.WithCancel(context.Background())
	Loop(ctx, point, 2*time.Millisecond, func(context.Context) { rounds.Add(1) })

	waitFor(t, 5*time.Second, "循环至少跑一轮", func() bool { return rounds.Load() >= 1 })
	cancel()

	// 取消后给足时间收尾,再确认计数不再增长。
	time.Sleep(50 * time.Millisecond)
	settled := rounds.Load()
	time.Sleep(50 * time.Millisecond)
	if got := rounds.Load(); got != settled {
		t.Fatalf("ctx 取消后循环仍在跑: %d → %d", settled, got)
	}
}

// TestLoopRoundReceivesContext 确认 round 拿到的是同一个 ctx
// (循环体里做 RPC / Redis 调用要靠它传取消信号)。
func TestLoopRoundReceivesContext(t *testing.T) {
	logtest.NewCollector(t)

	type ctxKey struct{}
	base := context.WithValue(context.Background(), ctxKey{}, "值")
	ctx, cancel := context.WithCancel(base)
	defer cancel()

	var got atomic.Value
	Loop(ctx, "test.loop.ctx", 2*time.Millisecond, func(c context.Context) {
		if v := c.Value(ctxKey{}); v != nil {
			got.Store(v)
		}
	})

	waitFor(t, 5*time.Second, "round 拿到带值的 ctx", func() bool {
		return got.Load() == "值"
	})
}

func TestLoopRejectsNonPositiveInterval(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
	}{
		{"零间隔", 0},
		{"负间隔", -time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := logtest.NewCollector(t)

			var ran atomic.Bool
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// 不启动,也绝不能 panic(time.NewTicker(0) 会 panic)。
			Loop(ctx, "test.loop.bad_interval", tt.interval, func(context.Context) {
				ran.Store(true)
			})

			time.Sleep(30 * time.Millisecond)
			if ran.Load() {
				t.Fatal("interval 非法时不该启动循环")
			}
			if !strings.Contains(logs.String(), "test.loop.bad_interval") {
				t.Errorf("没有记录非法 interval: %s", logs.String())
			}
		})
	}
}

// TestRunConcurrent 确认 Run 可被并发调用(计数走 Prometheus,自身线程安全)。
func TestRunConcurrent(t *testing.T) {
	logtest.NewCollector(t)

	const point = "test.run.concurrent"
	const n = 20

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			Run(point, func() { panic("并发炸") })
		}()
	}
	wg.Wait()

	if got := panicCount(point); got != n {
		t.Fatalf("safego_panic_total{point=%q} = %v, 期望 %d", point, got, n)
	}
}
