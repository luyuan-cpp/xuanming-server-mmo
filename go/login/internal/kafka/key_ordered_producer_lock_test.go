package kafka

import (
	"context"
	"testing"
	"time"

	db_proto "proto/db"

	"github.com/IBM/sarama"
	"github.com/IBM/sarama/mocks"
)

// newLockTestProducer 直接拼一个不连 broker 的生产者,只换掉底层 SyncProducer。
// 分区由 sp 自己的配置决定(mocks.NewSyncProducer 的 config.Producer.Partitioner),
// 与生产路径一样不在消息上填 Partition。
func newLockTestProducer(t *testing.T, sp sarama.SyncProducer, partitions int32) *KeyOrderedKafkaProducer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p := &KeyOrderedKafkaProducer{
		producer:     sp,
		topic:        "db_task_lock_test",
		partitionCnt: int(partitions),
		ctx:          ctx,
		cancel:       cancel,
	}
	p.payloadPool.New = func() interface{} { return make([]byte, 0, 1024) }
	return p
}

// 回归(G0,d5cbb8a8e):失败分支曾在持有 p.mu 时二次加锁 —— Kafka 选主期间 SendMessage 返回
// ErrLeaderNotAvailable,旧代码在失败分支里把分区标为不可用时再加一次 p.mu(sync.Mutex 不可重入),
// 发送 goroutine 卡死,之后本进程所有发送都排在锁后面。
// 本项删掉了整套不可用分区代码,这条用例防止以后有人在失败分支里再引入二次加锁。
func TestSendTaskLeaderNotAvailableDoesNotDeadlock(t *testing.T) {
	sp := mocks.NewSyncProducer(t, nil)
	sp.ExpectSendMessageAndFail(sarama.ErrLeaderNotAvailable)
	sp.ExpectSendMessageAndSucceed()
	t.Cleanup(func() { _ = sp.Close() })

	p := newLockTestProducer(t, sp, 2)

	type result struct{ first, second error }
	done := make(chan result, 1)
	go func() {
		var r result
		r.first = p.SendTask(context.Background(), &db_proto.DBTask{TaskId: "lock-1", Key: 42, Op: db_proto.DBOp_DB_OP_READ}, "42")
		r.second = p.SendTask(context.Background(), &db_proto.DBTask{TaskId: "lock-2", Key: 42, Op: db_proto.DBOp_DB_OP_READ}, "42")
		done <- r
	}()

	select {
	case r := <-done:
		if r.first == nil {
			t.Fatal("first send must surface ErrLeaderNotAvailable")
		}
		if r.second != nil {
			t.Fatalf("second send must not be blocked or poisoned by the first failure: %v", r.second)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SendTask deadlocked on p.mu after a send failure")
	}

	// SendTask 返回之后 p.mu 必须已经释放。轮询有 5s 上限(单调时钟),探测 goroutine 不会泄漏。
	lockFree := make(chan bool, 1)
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if p.mu.TryLock() {
				p.mu.Unlock()
				lockFree <- true
				return
			}
			time.Sleep(time.Millisecond)
		}
		lockFree <- false
	}()
	if !<-lockFree {
		t.Fatal("p.mu still held after SendTask returned")
	}
}
