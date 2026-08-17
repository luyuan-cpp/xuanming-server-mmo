package leader

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeStore 是内存版锁存储:单 key、带过期,行为对齐 Redis 的
// SET NX EX / 属主校验 Lua。不引入 miniredis,shared 模块零新增依赖。
type fakeStore struct {
	mu       sync.Mutex
	val      string
	expireAt time.Time
	// failEval 为 true 时 SetNX / EvalInt 全部报错,模拟"本副本到 Redis
	// 单边不可达"(续期只报错、读不到 0,但服务端 key 照常过期;
	// 竞选同样失败,所以降级后不会又抢回来)。
	failEval bool
}

func (s *fakeStore) SetNX(_ context.Context, _ string, value string, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failEval {
		return false, context.DeadlineExceeded
	}
	if s.val != "" && time.Now().Before(s.expireAt) {
		return false, nil
	}
	s.val = value
	s.expireAt = time.Now().Add(ttl)
	return true, nil
}

func (s *fakeStore) setFailEval(fail bool) {
	s.mu.Lock()
	s.failEval = fail
	s.mu.Unlock()
}

func (s *fakeStore) EvalInt(_ context.Context, script string, _ []string, args ...string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failEval {
		return 0, context.DeadlineExceeded
	}
	expired := s.val == "" || !time.Now().Before(s.expireAt)
	switch script {
	case renewScript:
		if expired || s.val != args[0] {
			return 0, nil
		}
		ms, _ := time.ParseDuration(args[1] + "ms")
		s.expireAt = time.Now().Add(ms)
		return 1, nil
	case releaseScript:
		if expired || s.val != args[0] {
			return 0, nil
		}
		s.val = ""
		return 1, nil
	}
	return 0, nil
}

// steal 模拟锁被别的进程抢走(例如 Redis 抖动导致过期后易主)。
func (s *fakeStore) steal(newVal string, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.val = newVal
	s.expireAt = time.Now().Add(ttl)
}

func (s *fakeStore) holder() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.val == "" || !time.Now().Before(s.expireAt) {
		return ""
	}
	return s.val
}

// eventually 每 10ms 轮询一次条件,最多等 3s。
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

const testTTL = 300 * time.Millisecond

func TestElectorBecomesLeaderAndReleasesOnCtxCancel(t *testing.T) {
	store := &fakeStore{}
	e := New(store, "test:lock", Options{TTL: testTTL, ID: "a"})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx, nil); close(done) }()

	eventually(t, "当选领导者", e.IsLeader)
	if h := store.holder(); !strings.HasPrefix(h, "a:") {
		t.Fatalf("锁属主应为 a:*,实际 %q", h)
	}

	cancel()
	<-done
	if e.IsLeader() {
		t.Fatal("ctx 结束后不应仍是领导者")
	}
	if h := store.holder(); h != "" {
		t.Fatalf("退出时应释放锁,实际属主 %q", h)
	}
}

func TestMutualExclusionAndTakeover(t *testing.T) {
	store := &fakeStore{}
	a := New(store, "test:lock", Options{TTL: testTTL, ID: "a"})
	b := New(store, "test:lock", Options{TTL: testTTL, ID: "b"})

	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	doneA := make(chan struct{})
	go func() { a.Run(ctxA, nil); close(doneA) }()
	go b.Run(ctxB, nil)

	eventually(t, "恰好一个领导者", func() bool { return a.IsLeader() != b.IsLeader() })

	// 锁存储层面任何时刻只有一个属主(IsLeader 的瞬时值在调度停顿下可能
	// 有短暂降级滞后,不拿它做逐时刻断言,以免测试机高负载时假失败)。
	for i := 0; i < 20; i++ {
		h := store.holder()
		if h != "" && !strings.HasPrefix(h, "a:") && !strings.HasPrefix(h, "b:") {
			t.Fatalf("锁属主异常: %q", h)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// 关掉现任领导者,另一个应在 ~TTL 内接管。
	if a.IsLeader() {
		cancelA()
		<-doneA
		eventually(t, "b 接管领导权", b.IsLeader)
	} else {
		cancelB()
		eventually(t, "a 接管领导权", a.IsLeader)
		cancelA()
		<-doneA
	}
}

func TestLostLockCancelsWhileLeaderCtx(t *testing.T) {
	store := &fakeStore{}
	e := New(store, "test:lock", Options{TTL: testTTL, ID: "a"})

	leadCtxCh := make(chan context.Context, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx, func(lctx context.Context) {
		leadCtxCh <- lctx
		<-lctx.Done()
	})

	var leadCtx context.Context
	select {
	case leadCtx = <-leadCtxCh:
	case <-time.After(3 * time.Second):
		t.Fatal("等待当选超时")
	}

	// 锁被别的进程抢走(TTL 给长,阻止本实例马上抢回)。
	store.steal("intruder", time.Hour)

	eventually(t, "丢锁后领导者 ctx 被取消", func() bool {
		select {
		case <-leadCtx.Done():
			return true
		default:
			return false
		}
	})
	eventually(t, "丢锁后 IsLeader 变 false", func() bool { return !e.IsLeader() })

	// 锁仍被入侵者持有,本实例只能保持跟随者。
	time.Sleep(2 * (testTTL / 3))
	if e.IsLeader() {
		t.Fatal("锁被他人持有期间不应重新当选")
	}
	if h := store.holder(); h != "intruder" {
		t.Fatalf("入侵者的锁不应被本实例删除,实际属主 %q", h)
	}
}

func TestSelfFencingOnPersistentRenewErrors(t *testing.T) {
	store := &fakeStore{}
	e := New(store, "test:lock", Options{TTL: testTTL, ID: "a"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx, nil)

	eventually(t, "当选领导者", e.IsLeader)

	// 模拟到 Redis 单边不可达:续期只报错、永远读不到 0。
	// 自我降级必须在 ~2/3 TTL 内触发,不能一直当自己还是领导者。
	store.setFailEval(true)
	eventually(t, "持续续期失败后自我降级", func() bool { return !e.IsLeader() })
}

func TestOnStateChangeSequence(t *testing.T) {
	store := &fakeStore{}
	var mu sync.Mutex
	var seq []bool
	// TTL 给大:本用例只验证回调序列,不验证过期 —— 用大 TTL 排除
	// 调度停顿导致 fake 锁过期、序列多出 [true false true ...] 的假失败。
	e := New(store, "test:lock", Options{
		TTL: time.Minute,
		ID:  "a",
		OnStateChange: func(isLeader bool) {
			mu.Lock()
			seq = append(seq, isLeader)
			mu.Unlock()
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx, nil); close(done) }()

	eventually(t, "当选回调", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seq) >= 1 && seq[0]
	})
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(seq) != 2 || !seq[0] || seq[1] {
		t.Fatalf("回调序列应为 [true false],实际 %v", seq)
	}
}
