package kafka

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHealth_MarkRunningClearsPreviousFailure 锁死 G4:一次已经恢复的故障不能永久污染健康标志。
//
// supervise 在冷却后重建 reader 并从断点续消费,消费循环重新跑起来时 markRunning 必须把
// failed / lastErr 一起清掉。不清的话,进程生命周期内只要有过一次 DB 抖动,
// Healthy() 就恒为 false —— "现在真的停了"这个唯一有价值的信号被永久淹没。
func TestHealth_MarkRunningClearsPreviousFailure(t *testing.T) {
	h := newHealth("test")

	h.markFailed(errDBDown)
	require.False(t, h.Healthy())
	require.True(t, h.StoppedOnError())
	require.ErrorIs(t, h.LastError(), errDBDown)

	h.markRunning()
	assert.True(t, h.Healthy(), "a resumed consumer must report healthy again")
	assert.False(t, h.StoppedOnError())
	assert.NoError(t, h.LastError())
}

// TestSupervise_ResumesAfterDBFailureAndHealthRecovers 走完整条 supervisor 路径:
// 第一条消费者因 DB 用尽重试而停下(健康标志翻红),冷却后重建的第二条正常消费,
// 健康标志必须回到绿色。
func TestSupervise_ResumesAfterDBFailureAndHealthRecovers(t *testing.T) {
	health := newHealth(ConsumerTxLog)
	var builds atomic.Int32
	failedOnce := make(chan struct{}, 1)

	build := func() (runner, MessageReader) {
		n := builds.Add(1)
		reader := newFakeReader(nil)
		// 第一轮:sink 永远失败 → Run 返回 ErrConsumerStoppedOnDB 并把 health 翻红。
		// 第二轮:sink 正常 → Run 一直跑到 ctx 取消。
		sink := &fakeTxSink{alwaysFail: n == 1}
		c := NewTxLogConsumer(TxLogConsumerConfig{
			BatchSize: 1, FlushInterval: time.Second,
			DBMaxAttempts: 1, DBRetryBackoff: time.Millisecond,
		}, reader, sink)
		c.health = health
		if n == 1 {
			reader.push(txMsg(t, 0, 1, validTx(1)))
			go func() {
				// 等它真的翻红再放行断言,避免用 sleep 猜时序。
				for health.LastError() == nil {
					time.Sleep(time.Millisecond)
				}
				failedOnce <- struct{}{}
			}()
		}
		return c, reader
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		supervise(ctx, ConsumerTxLog, 10*time.Millisecond, build)
		close(done)
	}()

	select {
	case <-failedOnce:
	case <-time.After(5 * time.Second):
		t.Fatal("the first consumer never recorded the database failure")
	}

	require.Eventually(t, func() bool { return builds.Load() >= 2 }, 5*time.Second, 5*time.Millisecond,
		"supervisor must rebuild the reader after the cooldown")
	require.Eventually(t, health.Healthy, 5*time.Second, 5*time.Millisecond,
		"health must go green again once the consumer resumed (G4)")
	assert.False(t, health.StoppedOnError())
	assert.NoError(t, health.LastError())

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not exit after cancel")
	}
}

// TestConsumersWait 锁死关停顺序用的等待点:main 必须能在关连接池之前有界地等消费者退出,
// 否则 txlog 的 flushOnShutdown 只会拿到一个已经关掉的池(等于从来没写过)。
func TestConsumersWait(t *testing.T) {
	var nilConsumers *Consumers
	assert.True(t, nilConsumers.Wait(time.Millisecond), "never-started consumers must not block shutdown")

	c := &Consumers{TxLog: newHealth(ConsumerTxLog), Snapshot: newHealth(ConsumerSnapshot)}
	c.wg.Add(1)
	assert.False(t, c.Wait(20*time.Millisecond), "a consumer still running must make Wait time out, not hang forever")

	go c.wg.Done()
	assert.True(t, c.Wait(5*time.Second))
}
