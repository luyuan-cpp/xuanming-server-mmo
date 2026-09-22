package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// InsertSnapshotIfGuidAbsent 的就地死锁重试(审计 #11)的纯逻辑部分。真库上"死锁确实撞上、并被就地吸收"
// 的确定性交错见 snapshot_store_integration_test.go(-tags=integration)。

func mysqlErrNo(n uint16) error {
	return &mysql.MySQLError{Number: n, Message: "injected by unit test"}
}

func noBackoff() time.Duration { return 0 }

func TestRetryOnDeadlock_RerunsDeadlockUntilSuccess(t *testing.T) {
	calls := 0
	reruns, err := retryOnDeadlock(context.Background(), 3, noBackoff, func() error {
		calls++
		if calls < 3 {
			return mysqlErrNo(mysqlErrDeadlock)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("两次 1213 之后第三次成功,应返回 nil,got %v", err)
	}
	if calls != 3 || reruns != 2 {
		t.Fatalf("应执行 3 次、重跑 2 次,got calls=%d reruns=%d", calls, reruns)
	}
}

func TestRetryOnDeadlock_BoundedByAttempts(t *testing.T) {
	calls := 0
	reruns, err := retryOnDeadlock(context.Background(), 3, noBackoff, func() error {
		calls++
		return mysqlErrNo(mysqlErrDeadlock)
	})
	if !isDeadlockMySQL(err) {
		t.Fatalf("预算用尽时必须把最后一次的 1213 原样交给调用方(消费者据此做通用重试),got %v", err)
	}
	if calls != 3 || reruns != 2 {
		t.Fatalf("上限 3 次:应执行 3 次、重跑 2 次,got calls=%d reruns=%d", calls, reruns)
	}
}

// 1205 刻意不就地重试:它已经白等了一整个 innodb_lock_wait_timeout,就地再等会把消费者的最坏等待放大数倍。
func TestRetryOnDeadlock_DoesNotRerunLockWaitTimeoutOrOtherErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"1205 锁等待超时", mysqlErrNo(mysqlErrLockWaitTimeout)},
		{"1062 重复键", mysqlErrNo(mysqlErrDupEntry)},
		{"非 MySQL 错误", errors.New("boom")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			reruns, err := retryOnDeadlock(context.Background(), 3, noBackoff, func() error {
				calls++
				return tc.err
			})
			if !errors.Is(err, tc.err) {
				t.Fatalf("应原样返回 %v,got %v", tc.err, err)
			}
			if calls != 1 || reruns != 0 {
				t.Fatalf("非 1213 不许重跑:got calls=%d reruns=%d", calls, reruns)
			}
		})
	}
}

func TestRetryOnDeadlock_BackoffIsCancelable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	calls := 0
	// 退避给一小时:若等待不可取消,本用例会挂到 go test 超时,而不是靠墙钟断言。
	reruns, err := retryOnDeadlock(ctx, 3, func() time.Duration { return time.Hour }, func() error {
		calls++
		return mysqlErrNo(mysqlErrDeadlock)
	})
	if calls != 1 || reruns != 0 {
		t.Fatalf("ctx 已取消时不应再重跑:got calls=%d reruns=%d", calls, reruns)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("错误应带上 ctx 的取消原因,got %v", err)
	}
	if !isDeadlockMySQL(err) {
		t.Fatalf("错误应同时保留最后一次的 1213,调用方才知道为什么在等,got %v", err)
	}
}

func TestSnapshotGuidDeadlockBackoffWithinBounds(t *testing.T) {
	for i := 0; i < 1000; i++ {
		d := snapshotGuidDeadlockBackoff()
		if d < snapshotGuidDeadlockBackoffMin || d > snapshotGuidDeadlockBackoffMax {
			t.Fatalf("第 %d 次退避 %v 超出 [%v, %v]", i, d, snapshotGuidDeadlockBackoffMin, snapshotGuidDeadlockBackoffMax)
		}
	}
}

// 两层重试叠加的预算:就地重试必须至少能重跑一次(否则 1213 照旧冒泡成消费者 1s 重试 + ERROR),
// 而最坏多睡的时长必须远小于消费者的 1s 通用退避(见 snapshotGuidDeadlockBackoffMax 的注释)。
func TestSnapshotGuidInPlaceRetryBudget(t *testing.T) {
	if snapshotGuidInsertAttempts < 2 {
		t.Fatalf("snapshotGuidInsertAttempts=%d:至少要 2 次才能就地吸收一次 1213", snapshotGuidInsertAttempts)
	}
	worst := time.Duration(snapshotGuidInsertAttempts-1) * snapshotGuidDeadlockBackoffMax
	if worst > 100*time.Millisecond {
		t.Fatalf("就地重试最坏多睡 %v,超过 100ms 预算:会与消费者 1s×DBMaxAttempts 的通用重试叠加出超预算等待", worst)
	}
}

// TestLockErrorClassifiersShareOneSource:1213 / 1205 在本包只有一个权威定义(id_segment_store.go 的常量),
// 两个分类器各自的口径也钉在这里 —— isRetryableMySQL 认 1213 + 1205,isDeadlockMySQL 只认 1213。
func TestLockErrorClassifiersShareOneSource(t *testing.T) {
	if mysqlErrDeadlock != 1213 || mysqlErrLockWaitTimeout != 1205 {
		t.Fatalf("错误号常量被改了:deadlock=%d lock_wait_timeout=%d", mysqlErrDeadlock, mysqlErrLockWaitTimeout)
	}
	for _, tc := range []struct {
		name                string
		err                 error
		retryable, deadlock bool
	}{
		{"1213", mysqlErrNo(mysqlErrDeadlock), true, true},
		{"1205", mysqlErrNo(mysqlErrLockWaitTimeout), true, false},
		{"1062", mysqlErrNo(mysqlErrDupEntry), false, false},
		{"非 MySQL 错误", errors.New("boom"), false, false},
	} {
		if got := isRetryableMySQL(tc.err); got != tc.retryable {
			t.Errorf("isRetryableMySQL(%s)=%v,want %v", tc.name, got, tc.retryable)
		}
		if got := isDeadlockMySQL(tc.err); got != tc.deadlock {
			t.Errorf("isDeadlockMySQL(%s)=%v,want %v", tc.name, got, tc.deadlock)
		}
	}
}

// TestJitteredBackoffDegenerateInterval:区间退化(hi <= lo)时返回 lo,不 panic。
func TestJitteredBackoffDegenerateInterval(t *testing.T) {
	for _, hi := range []time.Duration{10 * time.Millisecond, 5 * time.Millisecond, 0} {
		if d := jitteredBackoff(10*time.Millisecond, hi); d != 10*time.Millisecond {
			t.Fatalf("jitteredBackoff(10ms, %v)=%v,want 10ms", hi, d)
		}
	}
}
