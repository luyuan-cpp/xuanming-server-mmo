package locker

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// replaySetNXHook 让下一条 SET(SETNX)照常执行、但把结果改成 false(一次性),模拟首发已经写入、回包丢了之后
// 客户端自动重发,重发看到「已存在」。
type replaySetNXHook struct {
	armed atomic.Bool
}

func (h *replaySetNXHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *replaySetNXHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		if boolCmd, ok := cmd.(*redis.BoolCmd); ok && cmd.Name() == "set" && h.armed.CompareAndSwap(true, false) {
			boolCmd.SetVal(false)
		}
		return err
	}
}

func (h *replaySetNXHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// 锁里是自己这一次写进去的值:TryLock 必须认作已拿到,否则锁空占到 TTL 过期
// (docs/design/infra-reconnect-overview.md §4 R8)。
func TestTryLock_RecognizesOwnWriteAfterReplayedSetNX(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	hook := &replaySetNXHook{}
	hook.armed.Store(true)
	client.AddHook(hook)

	lock, err := NewRedisLocker(client).TryLock(context.Background(), "lock:replay", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !lock.IsLocked() {
		t.Fatal("锁里是自己这一次的值,必须认作已拿到")
	}
	if got, err := mr.Get("lock:replay"); err != nil || got != lock.Value {
		t.Fatalf("锁值应为本次的 %q,实际 %q (%v)", lock.Value, got, err)
	}
}

// 锁确实是别人的:不能误认成自己的。
func TestTryLock_DoesNotAdoptSomeoneElsesLock(t *testing.T) {
	mr := miniredis.RunT(t)
	if err := mr.Set("lock:taken", "someone-else"); err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	lock, err := NewRedisLocker(client).TryLock(context.Background(), "lock:taken", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if lock.IsLocked() {
		t.Fatal("锁属于别人时不能认作已拿到")
	}
}
