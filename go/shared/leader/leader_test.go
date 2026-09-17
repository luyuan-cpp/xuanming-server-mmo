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
// 刻意不实现 CallBounder:心跳的 maxCall 即续期 ctx 超时,门槛可按 TTL 精确推算。
type fakeStore struct {
	mu       sync.Mutex
	val      string
	expireAt time.Time
	// failEval 为 true 时 SetNX / EvalInt 全部报错,模拟"本副本到 Redis
	// 单边不可达"(续期只报错、读不到 0,但服务端 key 照常过期;
	// 竞选同样失败,所以降级后不会又抢回来)。
	failEval bool
	// renewDelay 让**成功**的续期晚这么久才返回(出错仍立即返回),
	// 模拟"出错回包比成功快"(如连接被拒)。
	renewDelay time.Duration
	// failDelay 让 failEval 下**报错**的续期晚这么久才返回,模拟失联时续期卡在途中。
	// fakeStore 不认 ctx,取值不得超过续期 ctx 超时,否则违背心跳按它算出的 maxCall。
	failDelay time.Duration
	// lastRenewOKAt 是最后一次成功续期到达存储的时刻。
	lastRenewOKAt time.Time
	// renewFailsSinceOK 是最近一次成功续期之后报错的续期次数,成功即清零;释放脚本不计。
	renewFailsSinceOK int
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

func (s *fakeStore) setRenewDelay(d time.Duration) {
	s.mu.Lock()
	s.renewDelay = d
	s.mu.Unlock()
}

func (s *fakeStore) setFailDelay(d time.Duration) {
	s.mu.Lock()
	s.failDelay = d
	s.mu.Unlock()
}

func (s *fakeStore) lastRenewOK() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastRenewOKAt
}

func (s *fakeStore) renewFailuresSinceOK() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.renewFailsSinceOK
}

func (s *fakeStore) EvalInt(_ context.Context, script string, _ []string, args ...string) (int64, error) {
	arrivedAt := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failEval {
		if script == renewScript {
			s.renewFailsSinceOK++
			if delay := s.failDelay; delay > 0 {
				s.mu.Unlock()
				time.Sleep(delay)
				s.mu.Lock()
			}
		}
		return 0, context.DeadlineExceeded
	}
	expired := s.val == "" || !arrivedAt.Before(s.expireAt)
	switch script {
	case renewScript:
		if expired || s.val != args[0] {
			return 0, nil
		}
		ms, _ := time.ParseDuration(args[1] + "ms")
		s.expireAt = arrivedAt.Add(ms)
		s.lastRenewOKAt = arrivedAt
		s.renewFailsSinceOK = 0
		if delay := s.renewDelay; delay > 0 {
			// 等回包期间不持锁,不拖慢测试侧的 setFailEval 等调用。
			s.mu.Unlock()
			time.Sleep(delay)
			s.mu.Lock()
		}
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
	eventuallyWithin(t, 3*time.Second, what, cond)
}

// eventuallyWithin 每 10ms 轮询一次条件,最多等 timeout。
func eventuallyWithin(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
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

// 成功续期回包慢、出错回包快(如连接被拒)时,失联后必须恰在第 2 个出错拍降级:第 1 拍就降级是
// 一次抖动误让位;第 3 拍才降级 ≈ TTL,与 key 服务端过期赛跑。按出错次数断言,不按墙钟间隔。
// TTL 3s、interval 1s、maxCall = 续期 ctx 超时 375ms → 门槛 2s。成功续期在 S 发出、S+400ms 回包,
// 之后出错拍定在 S+1s、S+2s:lastOK=S 时第 2 个出错拍 since ≥ 2s 降级,计数 2。
// 回归点:只把 lastOK 改记回包时刻 S+400ms 时,第 2 个出错拍 since = 2.0s−0.4s = 1.6s < 2s,
// 拖到第 3 拍,计数 3。400ms 超过续期 ctx 超时,但 fakeStore 不认 ctx,且只拖慢成功回包,
// 不影响出错拍的 maxCall。
func TestSelfFencingBeforeExpiryWhenErrorsReturnFasterThanRenews(t *testing.T) {
	const ttl = 3 * time.Second
	store := &fakeStore{}
	store.setRenewDelay(400 * time.Millisecond)

	var mu sync.Mutex
	failsAtDemotion := -1
	e := New(store, "test:lock", Options{
		TTL: ttl,
		ID:  "a",
		OnStateChange: func(isLeader bool) {
			if isLeader {
				return
			}
			// 降级回调在心跳 goroutine 退出之后、释放锁之前执行,此刻计数已稳定。
			n := store.renewFailuresSinceOK()
			mu.Lock()
			if failsAtDemotion < 0 {
				failsAtDemotion = n
			}
			mu.Unlock()
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx, nil)

	eventually(t, "当选领导者", e.IsLeader)
	eventuallyWithin(t, 5*time.Second, "至少一次续期成功", func() bool { return !store.lastRenewOK().IsZero() })
	store.setFailEval(true)
	eventuallyWithin(t, 10*time.Second, "持续续期失败后自我降级", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return failsAtDemotion >= 0
	})

	mu.Lock()
	got := failsAtDemotion
	mu.Unlock()
	if got < 2 {
		t.Fatalf("第 %d 个出错拍就降级了,一次抖动不该降级", got)
	}
	if got > 2 {
		t.Fatalf("第 %d 个出错拍才降级,应在第 2 个出错拍降级、赶在 key 过期(TTL %s)之前", got, ttl)
	}
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

// boundedStore 在 fakeStore 之上报告客户端单次调用上限,走心跳的 CallBounder 分支。
type boundedStore struct {
	*fakeStore
	maxCall time.Duration
}

var _ CallBounder = boundedStore{}

func (s boundedStore) MaxCallDuration() time.Duration { return s.maxCall }

// failsAtFirstDemotion 跑一个 TTL 3s(interval 1s)的选举器:当选回调里让续期从此全部报错,再同步
// 阻塞 electedDelay 推迟心跳起动;返回首次降级时"最近成功续期之后报错的续期次数"。
// 当选前 failEval 为 false,降级后 SetNX 同样报错,不会再次当选。
func failsAtFirstDemotion(t *testing.T, store Store, fs *fakeStore, electedDelay time.Duration) int {
	t.Helper()
	var mu sync.Mutex
	fails := -1
	e := New(store, "test:lock", Options{
		TTL: 3 * time.Second,
		ID:  "a",
		OnStateChange: func(isLeader bool) {
			if isLeader {
				fs.setFailEval(true)
				time.Sleep(electedDelay)
				return
			}
			// 降级回调在心跳 goroutine 退出之后执行,此刻计数已稳定。
			n := fs.renewFailuresSinceOK()
			mu.Lock()
			if fails < 0 {
				fails = n
			}
			mu.Unlock()
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx, nil)

	eventuallyWithin(t, 10*time.Second, "续期持续报错后自我降级", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return fails >= 0
	})
	mu.Lock()
	defer mu.Unlock()
	return fails
}

// 心跳起动晚于 SETNX(同步当选回调阻塞 900ms)时,续期节拍仍须从 SETNX 发出时刻排起:首拍在拿锁后
// 1.0s 发出,报错 300ms 才回包,since ≈ 1.3s < 门槛 2s 不降级;第 2 拍定在 2.0s,since ≥ 2s 降级,计数 2。
// 回归点:按心跳起动时刻起 ticker 时首拍在 1.9s 发出、2.2s 回包,since ≥ 2s,计数 1;这种错相位下首个
// 出错拍若快速失败逃过门槛,第 2 个出错拍卡满调用上限就会拖过 key 过期。maxCall = 续期 ctx 超时 375ms,
// 两侧各有约 200ms 以上余量。
func TestHeartbeatScheduleAlignsWithCampaignNotHeartbeatStart(t *testing.T) {
	store := &fakeStore{}
	store.setFailDelay(300 * time.Millisecond)
	if got := failsAtFirstDemotion(t, store, store, 900*time.Millisecond); got != 2 {
		t.Fatalf("应在第 2 个出错拍降级,实际第 %d 个;续期节拍没有对齐 SETNX 发出时刻", got)
	}
}

// Store 报告 socket 读写上界 800ms 时门槛须按"续期 ctx 超时 + 读写上界"算:maxCall = CallBudget(375ms, 800ms)
// = 1.175s ≥ interval 1s,FenceAfter 满足不了约束,任一续期报错即降级,计数 1。上界单独(800ms)小于 interval,
// 所以两种回归都会被抓住:取 max(375ms, 800ms) 或忽略 CallBounder 时 maxCall < 1s、门槛 2s,首拍报错 300ms
// 回包、since ≈ 1.3s 逃过门槛,第 2 拍才降级,计数 2。
func TestHeartbeatFenceUsesStoreCallBound(t *testing.T) {
	fs := &fakeStore{}
	fs.setFailDelay(300 * time.Millisecond)
	store := boundedStore{fakeStore: fs, maxCall: 800 * time.Millisecond}
	if got := failsAtFirstDemotion(t, store, fs, 0); got != 1 {
		t.Fatalf("Store 报告的调用上限不小于心跳间隔时应在第 1 个出错拍降级,实际第 %d 个", got)
	}
}

// 降级计时从 SETNX 发出时刻起算,不从心跳起动时刻起算:当选回调让续期从此全部立即报错(SETNX 已成功),
// 再阻塞 2.2s 推迟心跳起动。首拍本该在拿锁后 1s,已过,立即发出,since ≥ 2.2s ≥ 门槛 2s,第 1 个出错拍
// 就降级,计数 1。回归点:acquiredAt 取心跳起动时刻(或当选回调之后)时首拍 since 只算 ≈1s,拖到第 2 拍,
// 计数 2 —— 而服务端 key 从 SETNX 起算,早已过去 2.2s。
func TestHeartbeatFenceClockStartsAtCampaign(t *testing.T) {
	store := &fakeStore{}
	if got := failsAtFirstDemotion(t, store, store, 2200*time.Millisecond); got != 1 {
		t.Fatalf("应在第 1 个出错拍降级,实际第 %d 个;降级计时没有从 SETNX 发出时刻起算", got)
	}
}
