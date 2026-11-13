package svc

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"match/internal/metrics"
	"shared/drainproof"
)

// 缺口清单第 25 条第五批(docs/design/assetop-redelivery-sharding-20261110.md §14.8):终局发件箱投递循环每一轮各自 recover,
// 活性写在一轮正常返回之后。口径在 shared/looplive(那里有它自己的用例),这里只测循环一侧。不连库。

// waitRounds 在 2 秒内等投递轮数到 want;等不到就停掉循环并判红。
func waitRounds(t *testing.T, rounds *atomic.Int32, want int32, stop func()) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for rounds.Load() < want {
		if time.Now().After(deadline) {
			stop()
			t.Fatalf("2 秒内应跑到第 %d 轮,实际只跑了 %d 轮", want, rounds.Load())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// relayHeld 报告中继是否被打过 HOLD(结局未知)。会关掉它的入口,只在用例收尾时调。
func relayHeld(t *testing.T, relay *drainproof.Session) bool {
	t.Helper()
	relay.CloseAdmission()
	err := relay.Wait(context.Background())
	if err != nil && !errors.Is(err, drainproof.ErrHold) {
		t.Fatalf("夹具前提:关掉入口后 Wait 只该回 nil 或 ErrHold,实际 %v", err)
	}
	return err != nil
}

// startOutboxLoop 起一个 2ms 一轮的投递循环,返回停止函数(取消并等它退出)。
func startOutboxLoop(relay *drainproof.Session, publish func(context.Context) error) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runOutcomeOutboxLoop(ctx, relay, 2*time.Millisecond, publish)
	}()
	return func() {
		cancel()
		<-done
	}
}

// 一轮 panic 之后循环接着跑后面的轮次,不往外抛(进程不死);那一轮打了 HOLD;循环停止后删掉活性序列。
// 原来 panic 会一路抛出去把进程打死:按片分工之后,一条坏的终局行会随着片换主把每个 match 副本挨个打死。
func TestOutcomeOutboxLoopSurvivesAPanickingRound(t *testing.T) {
	metrics.OutcomeOutboxLive.Stop()
	relay := drainproof.New()
	var rounds atomic.Int32
	stop := startOutboxLoop(relay, func(context.Context) error {
		if rounds.Add(1) == 1 {
			panic("test: poisoned outcome row")
		}
		return nil
	})
	waitRounds(t, &rounds, 3, stop)
	if _, ok := metrics.OutcomeOutboxLive.Value(); !ok {
		t.Error("循环还在跑,活性序列应当在")
	}
	stop()
	if _, ok := metrics.OutcomeOutboxLive.Value(); ok {
		t.Error("循环停止之后应删掉活性序列")
	}
	if !relayHeld(t, relay) {
		t.Error("panic 的那一轮结局未知,排空证明应停在 HOLD")
	}
}

// 每一轮都 panic:循环不死,但活性时刻不再前进 —— 停摆告警靠的就是这个。
func TestOutcomeOutboxLoopStopsMarkingWhenEveryRoundPanics(t *testing.T) {
	metrics.OutcomeOutboxLive.Stop()
	relay := drainproof.New()
	var rounds atomic.Int32
	stop := startOutboxLoop(relay, func(context.Context) error {
		rounds.Add(1)
		panic("test: poisoned outcome row")
	})
	defer stop()
	waitRounds(t, &rounds, 1, stop)
	// 起来时写过一次;之后把时刻拨回很早,再跑几轮:panic 的轮次不得把它往前写。
	metrics.OutcomeOutboxLive.Mark(time.Unix(1000, 0))
	waitRounds(t, &rounds, rounds.Load()+3, stop)
	if got, ok := metrics.OutcomeOutboxLive.Value(); !ok || got != 1000 {
		t.Fatalf("panic 的轮次不写活性时刻,值应停在 1000,实际 %v(ok=%t)", got, ok)
	}
}

// 一轮投递出错:按结局未知打 HOLD,循环接着跑,这一轮照样算跑完(活性时刻往前写)—— 出错的行留在发件箱里下一轮重投。
// 每一轮拿到的 ctx 带自己的预算。
func TestOutcomeOutboxLoopHoldsOnDeliveryErrorAndKeepsRunning(t *testing.T) {
	metrics.OutcomeOutboxLive.Stop()
	relay := drainproof.New()
	var rounds atomic.Int32
	var withBudget atomic.Bool
	stop := startOutboxLoop(relay, func(call context.Context) error {
		if _, ok := call.Deadline(); ok {
			withBudget.Store(true)
		}
		rounds.Add(1)
		return errors.New("inbox unavailable")
	})
	waitRounds(t, &rounds, 1, stop)
	metrics.OutcomeOutboxLive.Mark(time.Unix(1000, 0))
	waitRounds(t, &rounds, rounds.Load()+3, stop)
	got, ok := metrics.OutcomeOutboxLive.Value()
	stop()
	if !ok || got <= 1000 {
		t.Errorf("出错的一轮也算跑完,应把活性时刻往前写,实际 %v(ok=%t)", got, ok)
	}
	if !withBudget.Load() {
		t.Error("每一轮拿到的 ctx 应带这一轮的预算")
	}
	if !relayHeld(t, relay) {
		t.Error("投递出错按结局未知,排空证明应停在 HOLD")
	}
}

// 中继入口已关(停机排空):循环不再开新的一轮,自己退出并删掉活性序列,不等 ctx。
func TestOutcomeOutboxLoopStopsWhenRelayAdmissionCloses(t *testing.T) {
	metrics.OutcomeOutboxLive.Stop()
	relay := drainproof.New()
	relay.CloseAdmission()
	var rounds atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		runOutcomeOutboxLoop(context.Background(), relay, time.Hour, func(context.Context) error {
			rounds.Add(1)
			return nil
		})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("入口关了之后循环应自己退出")
	}
	if rounds.Load() != 0 {
		t.Errorf("入口关了之后不该再投递,实际投了 %d 轮", rounds.Load())
	}
	if _, ok := metrics.OutcomeOutboxLive.Value(); ok {
		t.Error("循环有意停止之后应删掉活性序列")
	}
}
