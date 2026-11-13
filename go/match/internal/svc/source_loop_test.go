package svc

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"match/internal/metrics"
	"shared/drainproof"
)

// StartSourceLoop(撮合的两个循环用它)每一轮各自 recover:一轮 panic 之后循环接着跑后面的轮次,那一轮打了 HOLD。
// 原来 panic 一路抛到最外层的 safego.Go 才被兜住,循环就此退出,进程却还活着、还在成员表里:它名下的撮合段从此没人撮合
// (docs/design/assetop-redelivery-sharding-20261110.md §14.10)。
func TestStartSourceLoopSurvivesAPanickingRound(t *testing.T) {
	sc := &ServiceContext{RootAdmission: drainproof.New(), RelayAdmission: drainproof.New()}
	var rounds atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	stop := func() {
		cancel()
		sc.sourceWorkers.Wait()
	}
	sc.StartSourceLoop(ctx, "match.test.source_loop", 2*time.Millisecond, func(context.Context) {
		if rounds.Add(1) == 1 {
			panic("test: poisoned ticket row")
		}
	})
	waitRounds(t, &rounds, 3, stop)
	stop()
	if !relayHeld(t, sc.RootAdmission) {
		t.Error("panic 的那一轮结局未知,排空证明应停在 HOLD")
	}
}

// 没有 panic 的循环不打 HOLD(每一轮各自 recover 不改变正常路径)。
func TestStartSourceLoopDoesNotHoldOnHealthyRounds(t *testing.T) {
	sc := &ServiceContext{RootAdmission: drainproof.New(), RelayAdmission: drainproof.New()}
	var rounds atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	stop := func() {
		cancel()
		sc.sourceWorkers.Wait()
	}
	sc.StartSourceLoop(ctx, "match.test.source_loop", 2*time.Millisecond, func(context.Context) { rounds.Add(1) })
	waitRounds(t, &rounds, 3, stop)
	stop()
	if relayHeld(t, sc.RootAdmission) {
		t.Error("正常跑完的轮次不该打 HOLD")
	}
}

// 活性(metrics.LoopLive 里 point 那一条,缺口清单第 25 条第六批,§14.10):循环起来就有序列;正常返回的一轮把时刻往前写;
// 之后每一轮都 panic 时循环接着跑、时刻不再前进(停摆告警靠的就是这个);ctx 结束、有意退出时删掉序列。
// 用自己的点位名:LoopLive 是包级的一族,别的用例的循环不会碰到这一条。
func TestStartSourceLoopLiveness(t *testing.T) {
	const point = "match.test.source_loop.liveness"
	live := metrics.LoopLive.For(point)
	sc := &ServiceContext{RootAdmission: drainproof.New(), RelayAdmission: drainproof.New()}
	var rounds atomic.Int32
	var poisoned atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	stop := func() {
		cancel()
		sc.sourceWorkers.Wait()
	}
	sc.StartSourceLoop(ctx, point, 2*time.Millisecond, func(context.Context) {
		rounds.Add(1)
		if poisoned.Load() {
			panic("test: poisoned ticket row")
		}
	})
	waitRounds(t, &rounds, 1, stop)
	if _, ok := live.Value(); !ok {
		stop()
		t.Fatal("循环起来之后应有活性序列")
	}

	// 轮次是串行的:计数到 n+2 时第 n+1 轮已经整轮跑完(连同写时刻)。
	live.Mark(time.Unix(1000, 0))
	waitRounds(t, &rounds, rounds.Load()+2, stop)
	if got, ok := live.Value(); !ok || got <= 1000 {
		stop()
		t.Fatalf("正常返回的一轮应把时刻往前写,实际 %v(ok=%t)", got, ok)
	}

	// 从这里起每一轮都 panic。先等两轮,让切换之前已经开始的那一轮跑完、写完时刻。
	poisoned.Store(true)
	waitRounds(t, &rounds, rounds.Load()+2, stop)
	live.Mark(time.Unix(1000, 0))
	waitRounds(t, &rounds, rounds.Load()+3, stop)
	if got, ok := live.Value(); !ok || got != 1000 {
		stop()
		t.Fatalf("panic 的轮次不写时刻,值应停在 1000,实际 %v(ok=%t)", got, ok)
	}

	stop()
	if _, ok := live.Value(); ok {
		t.Error("ctx 结束、循环有意退出之后应删掉活性序列")
	}
}

// StartRelayLoop(成局推进、到期恢复、整队维护、观战维护等用它)同一口径:一轮 panic 打中继 HOLD、循环接着跑,
// 之后正常返回的轮次照常写活性;ctx 结束、有意退出时删掉序列。
func TestStartRelayLoopLivenessAndPanic(t *testing.T) {
	const point = "match.test.relay_loop.liveness"
	live := metrics.LoopLive.For(point)
	sc := &ServiceContext{RootAdmission: drainproof.New(), RelayAdmission: drainproof.New()}
	var rounds atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	stop := func() {
		cancel()
		wait, release := context.WithTimeout(context.Background(), 2*time.Second)
		defer release()
		if err := sc.RelayAdmission.WaitIdle(wait); err != nil {
			t.Fatalf("ctx 结束后循环应在 2 秒内退出: %v", err)
		}
	}
	started := time.Now().Unix()
	sc.StartRelayLoop(ctx, point, 2*time.Millisecond, func(context.Context) {
		if rounds.Add(1) == 1 {
			panic("test: poisoned team row")
		}
	})
	waitRounds(t, &rounds, 3, stop)
	if got, ok := live.Value(); !ok || got < float64(started) {
		stop()
		t.Fatalf("panic 之后正常返回的轮次应把时刻写成现在,实际 %v(ok=%t)", got, ok)
	}

	stop()
	if _, ok := live.Value(); ok {
		t.Error("ctx 结束、循环有意退出之后应删掉活性序列")
	}
	if !relayHeld(t, sc.RelayAdmission) {
		t.Error("panic 的那一轮结局未知,中继应停在 HOLD")
	}
}
