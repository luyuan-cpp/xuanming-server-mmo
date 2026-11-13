package authority

import (
	"context"
	"errors"
	"testing"
	"time"

	"match/internal/metrics"
	"shared/drainproof"

	"github.com/prometheus/client_golang/prometheus"
)

// recoveredPanics 读默认注册表里 safego_panic_total{point} 的当前值;这个点位还没 panic 过(没有序列)时是 0。
func recoveredPanics(point string) float64 {
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		return 0
	}
	for _, family := range families {
		if family.GetName() != "safego_panic_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "point" && label.GetValue() == point {
					return metric.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// 投榜循环每一轮各自 recover:一轮 panic 不往外抛(进程不死),那一轮打了 HOLD,循环照常等下一拍、ctx 结束才退出。
// 原来 panic 一路抛出 RunRankPublisher 把进程打死:投榜按 (模式, 来源片) 分给各副本,一条解释不了的行会随着片换主
// 把副本挨个打死(docs/design/assetop-redelivery-sharding-20261110.md §14.10)。
//
// Store 传 nil:第一轮读投榜片时空指针 panic,模拟一轮跑到一半炸掉;循环一起来就跑第一轮,不用等 2 秒的节拍。
// 端点不会真连(gRPC 客户端懒连接)。改之前这个用例会让整个测试进程因未兜住的 panic 退出。
func TestRunRankPublisherSurvivesAPanickingRound(t *testing.T) {
	relay := drainproof.New()
	before := recoveredPanics(rankPublishPoint)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunRankPublisher(ctx, nil, "127.0.0.1:1", "test-secret", relay)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for recoveredPanics(rankPublishPoint) < before+1 {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("第一轮的 panic 应在 2 秒内被兜住并计进 safego_panic_total")
		}
		time.Sleep(2 * time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("panic 的一轮之后循环应接着等下一拍,不该退出")
	default:
	}
	// 活性:一进来就写过一次,panic 的一轮不写也不删,循环跑着就有序列;ctx 结束、有意退出时删掉。
	live := metrics.LoopLive.For(rankPublishPoint)
	if _, ok := live.Value(); !ok {
		t.Error("循环跑着时应有活性序列")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 结束后循环应退出")
	}
	if _, ok := live.Value(); ok {
		t.Error("ctx 结束、循环有意退出之后应删掉活性序列")
	}

	relay.CloseAdmission()
	if err := relay.Wait(context.Background()); !errors.Is(err, drainproof.ErrHold) {
		t.Fatalf("panic 的那一轮结局未知,排空证明应停在 HOLD,实际 %v", err)
	}
}
