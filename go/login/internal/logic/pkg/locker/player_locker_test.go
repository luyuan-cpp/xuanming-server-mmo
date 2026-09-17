package locker

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/redis/go-redis/v9"
)

// evalFault 挂在 miniredis 的 PreHook 上,只拦 EVAL(Renew / Release 都走它),
// 模拟"本进程到 Redis 单边不可达":请求报错,服务端 key 不受影响。
// PreHook 在该连接的服务端 goroutine 上执行、不持 miniredis 的服务端锁,在里面 sleep 只拖住本连接。
type evalFault struct {
	mu     sync.Mutex
	failIf func(call int64) bool
	// failDelay 让报错晚这么久才回给客户端,模拟续期卡在途中。
	failDelay time.Duration
	// passDelay 让放行的 EVAL 晚这么久才执行,模拟成功续期回包慢于报错。
	passDelay time.Duration
	calls     int64
	passed    int64
	// failedSincePass 是最近一次放行之后报错的 EVAL 数,放行即清零。
	failedSincePass int64
	// arrived 在每个 EVAL 到达服务端时非阻塞地收到一个信号。
	arrived chan struct{}
}

func newEvalFault() *evalFault {
	return &evalFault{arrived: make(chan struct{}, 16)}
}

func (f *evalFault) setFailIf(fn func(call int64) bool) {
	f.mu.Lock()
	f.failIf = fn
	f.mu.Unlock()
}

func (f *evalFault) setFailDelay(d time.Duration) {
	f.mu.Lock()
	f.failDelay = d
	f.mu.Unlock()
}

func (f *evalFault) setPassDelay(d time.Duration) {
	f.mu.Lock()
	f.passDelay = d
	f.mu.Unlock()
}

func (f *evalFault) hook(c *server.Peer, cmd string, _ ...string) bool {
	if cmd != "EVAL" {
		return false
	}
	select {
	case f.arrived <- struct{}{}:
	default:
	}

	f.mu.Lock()
	f.calls++
	fail := f.failIf != nil && f.failIf(f.calls)
	failDelay, passDelay := f.failDelay, f.passDelay
	if fail {
		f.failedSincePass++
	} else {
		f.passed++
		f.failedSincePass = 0
	}
	f.mu.Unlock()

	if !fail {
		if passDelay > 0 {
			time.Sleep(passDelay)
		}
		return false
	}
	if failDelay > 0 {
		time.Sleep(failDelay)
	}
	// 前缀不能是 LOADING / READONLY 等,否则 go-redis 会自行重试。
	c.WriteError("ERR simulated redis partition")
	return true
}

func (f *evalFault) passedCount() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.passed
}

func (f *evalFault) failedSincePassCount() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failedSincePass
}

type lostEvent struct {
	err error
	// failedSincePass 是 onLost 触发时最近一次放行之后报错的 EVAL 数。
	failedSincePass int64
}

type lostRecorder struct {
	fault *evalFault
	count atomic.Int32
	ch    chan lostEvent
}

func newLostRecorder(fault *evalFault) *lostRecorder {
	return &lostRecorder{fault: fault, ch: make(chan lostEvent, 4)}
}

func (r *lostRecorder) onLost(err error) {
	r.count.Add(1)
	select {
	case r.ch <- lostEvent{err: err, failedSincePass: r.fault.failedSincePassCount()}:
	default:
	}
}

// 客户端开 ContextTimeoutEnabled:socket 读写认续期 ctx 截止时间,maxCall = 续期超时
// min(2s, ttl/8),门槛可按 ttl 精确推算(ttl 3s → maxCall 375ms → 门槛 2s)。
func newHeartbeatFixture(t *testing.T, ttl time.Duration) (*miniredis.Miniredis, *evalFault, *Lock) {
	t.Helper()
	return newHeartbeatFixtureWithOptions(t, ttl, &redis.Options{ContextTimeoutEnabled: true})
}

// newHeartbeatFixtureWithOptions 用给定客户端选项建夹具,Addr 由夹具填成 miniredis 地址。
func newHeartbeatFixtureWithOptions(t *testing.T, ttl time.Duration, opts *redis.Options) (*miniredis.Miniredis, *evalFault, *Lock) {
	t.Helper()
	mr := miniredis.RunT(t)
	opts.Addr = mr.Addr()
	rdb := redis.NewClient(opts)
	t.Cleanup(func() { _ = rdb.Close() })

	fault := newEvalFault()
	mr.Server().SetPreHook(fault.hook)

	lock, err := NewRedisLocker(rdb).TryLock(context.Background(), "player_locker:heartbeat_test", ttl)
	if err != nil || !lock.IsLocked() {
		t.Fatalf("拿锁失败: locked=%v err=%v", lock != nil && lock.IsLocked(), err)
	}
	return mr, fault, lock
}

// eventually 每 10ms 轮询一次条件,超时即失败。
func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
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

// mustStop 连调两次 stop,既验证不死锁也验证可重复调用。
func mustStop(t *testing.T, stop func()) {
	t.Helper()
	mustStopWithin(t, stop, 3*time.Second)
}

// mustStopWithin 同 mustStop,等待上限由调用方给出(stop 要等在途续期返回时用)。
func mustStopWithin(t *testing.T, stop func(), timeout time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		stop()
		stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("stop() %s 内未返回,心跳 goroutine 疑似死锁", timeout)
	}
}

// 续期先成功、再持续报错:必须恰在失联后第 2 个出错拍自我降级 —— 第 1 拍就降级是一次抖动
// 误让位;第 3 拍才降级 ≈ TTL,与服务端 key 过期赛跑。按报错次数断言,不按墙钟间隔。
// 客户端不开 ContextTimeoutEnabled、读 500ms + 写 50ms:maxCall = CallBudget(375ms, 550ms) = 925ms < 1s
// 且 2s+925ms < 3s,门槛 2s。放行的 EVAL 晚 300ms 才执行(比读超时低 200ms,仍能成功;成功回包慢于报错):
// 成功续期在 S 发出,出错拍定在 S+1s、S+2s,lastOK=S 时第 2 个出错拍 since ≥ 2s 降级,计数 2。
// 回归点:lastOK 改记回包时刻 S+300ms 时第 2 个出错拍 since = 2.0s−0.3s = 1.7s < 2s,拖到第 3 拍,计数 3。
func TestStartHeartbeat_SelfFencesBeforeExpiryOnPersistentRenewErrors(t *testing.T) {
	const ttl = 3 * time.Second
	const interval = ttl / 3
	_, fault, lock := newHeartbeatFixtureWithOptions(t, ttl, &redis.Options{
		ReadTimeout:  500 * time.Millisecond,
		WriteTimeout: 50 * time.Millisecond,
	})

	var failing atomic.Bool
	fault.setFailIf(func(int64) bool { return failing.Load() })
	fault.setPassDelay(300 * time.Millisecond)

	rec := newLostRecorder(fault)
	stop := lock.StartHeartbeat(interval, ttl, rec.onLost)
	t.Cleanup(stop)

	eventually(t, 5*time.Second, "至少一次续期成功", func() bool { return fault.passedCount() >= 1 })
	failing.Store(true)

	var ev lostEvent
	select {
	case ev = <-rec.ch:
	case <-time.After(10 * time.Second):
		t.Fatal("续期持续报错 10s 仍未自我降级")
	}

	if ev.failedSincePass < 2 {
		t.Fatalf("第 %d 个出错拍就降级了,一次抖动不该降级", ev.failedSincePass)
	}
	if ev.failedSincePass > 2 {
		t.Fatalf("第 %d 个出错拍才降级,应在第 2 个出错拍降级、赶在 key 过期(TTL %s)之前", ev.failedSincePass, ttl)
	}
	if ev.err == nil || !strings.Contains(ev.err.Error(), "self-fencing") {
		t.Fatalf("onLost 错误应说明自我降级,实际 %v", ev.err)
	}

	mustStop(t, stop)
	if n := rec.count.Load(); n != 1 {
		t.Fatalf("onLost 应恰好调用 1 次,实际 %d", n)
	}
}

// 奇数次续期报错、偶数次成功:每次报错前一拍都刚续期成功,不得降级。
// 回归点:成功续期若不刷新 lastOK,第 3 次续期(距拿锁 ≈TTL)报错时就会误降级。
func TestStartHeartbeat_ToleratesTransientRenewErrors(t *testing.T) {
	const ttl = 3 * time.Second
	const interval = ttl / 3
	mr, fault, lock := newHeartbeatFixture(t, ttl)

	fault.setFailIf(func(call int64) bool { return call%2 == 1 })

	rec := newLostRecorder(fault)
	stop := lock.StartHeartbeat(interval, ttl, rec.onLost)
	t.Cleanup(stop)

	// 第 4 次续期能发出,就证明第 1、3 次报错后心跳都没有退出。
	eventually(t, 8*time.Second, "报错-成功交替两轮", func() bool { return fault.passedCount() >= 2 })

	if n := rec.count.Load(); n != 0 {
		ev := <-rec.ch
		t.Fatalf("瞬时报错后又续期成功,不应降级;onLost 被调用 %d 次: %v", n, ev.err)
	}
	if got, err := mr.Get(lock.Key); err != nil || got != lock.Value {
		t.Fatalf("锁应仍属本锁: got=%q err=%v", got, err)
	}

	mustStop(t, stop)
	if n := rec.count.Load(); n != 0 {
		t.Fatalf("stop 后不应触发 onLost,实际 %d 次", n)
	}
}

// 续期成功但属主已换人(读到 0):保持原行为,立刻 onLost,且不动他人的锁。
func TestStartHeartbeat_OwnershipLostFiresOnLost(t *testing.T) {
	const ttl = 1500 * time.Millisecond
	mr, fault, lock := newHeartbeatFixture(t, ttl)

	rec := newLostRecorder(fault)
	stop := lock.StartHeartbeat(50*time.Millisecond, ttl, rec.onLost)
	t.Cleanup(stop)

	if err := mr.Set(lock.Key, "someone-else"); err != nil {
		t.Fatalf("改写锁值失败: %v", err)
	}

	select {
	case ev := <-rec.ch:
		if ev.err == nil || !strings.Contains(ev.err.Error(), "lost lock ownership") {
			t.Fatalf("onLost 错误应说明属主已换,实际 %v", ev.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("属主已换 3s 仍未 onLost")
	}

	mustStop(t, stop)
	if n := rec.count.Load(); n != 1 {
		t.Fatalf("onLost 应恰好调用 1 次,实际 %d", n)
	}
	if got, _ := mr.Get(lock.Key); got != "someone-else" {
		t.Fatalf("不应改动他人的锁,实际 %q", got)
	}
}

// 降级计时从 SETNX 发出时刻起算,不从 StartHeartbeat 被调用时起算:拿锁后隔 2.2s 才起心跳、
// 续期从头就报错。首拍本该在拿锁后 1s,已过,立即发出,since ≥ 2.2s ≥ 门槛 2s,第 1 个出错拍
// 就必须降级,计数 1。回归点:以 StartHeartbeat 时的 time.Now() 为初值(节拍随之后移)时
// 首拍 since 只算 ≈1s,拖到第 2 拍,计数 2。
func TestStartHeartbeat_FenceClockStartsAtLockAcquisition(t *testing.T) {
	const ttl = 3 * time.Second
	const interval = ttl / 3
	_, fault, lock := newHeartbeatFixture(t, ttl)
	fault.setFailIf(func(int64) bool { return true })

	time.Sleep(2200 * time.Millisecond)
	rec := newLostRecorder(fault)
	stop := lock.StartHeartbeat(interval, ttl, rec.onLost)
	t.Cleanup(stop)

	select {
	case ev := <-rec.ch:
		if ev.failedSincePass != 1 {
			t.Fatalf("应在第 1 个出错拍降级,实际报错 %d 次后才降级", ev.failedSincePass)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("续期持续报错 5s 仍未自我降级")
	}

	mustStop(t, stop)
	if n := rec.count.Load(); n != 1 {
		t.Fatalf("onLost 应恰好调用 1 次,实际 %d", n)
	}
}

// 续期在途时调用 stop:这次续期报错回来也不得再 onLost —— 调用方收尾(释放锁、退出临界区)
// 之后晚到的 onLost 会误取消后续流程。客户端不开 ContextTimeoutEnabled、读写超时各 2s:
// maxCall = CallBudget(375ms, 4s) ≥ interval,门槛 0,任一续期报错都会降级,不依赖距拿锁多久。
// 首拍在拿锁后 1s 发出,服务端 1.5s 后才回 ERR(低于读超时,在途窗口由它决定,不靠 375ms 的 ctx 窗口);
// EVAL 一到达服务端就调 stop,续期返回后若不复查 stop 就会触发 onLost。
func TestStartHeartbeat_NoOnLostAfterStopDuringInflightRenew(t *testing.T) {
	const ttl = 3 * time.Second
	const interval = ttl / 3
	_, fault, lock := newHeartbeatFixtureWithOptions(t, ttl, &redis.Options{
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
	})
	fault.setFailIf(func(int64) bool { return true })
	fault.setFailDelay(1500 * time.Millisecond)

	rec := newLostRecorder(fault)
	stop := lock.StartHeartbeat(interval, ttl, rec.onLost)
	t.Cleanup(stop)

	select {
	case <-fault.arrived:
	case <-time.After(3 * time.Second):
		t.Fatal("心跳 3s 内未发出续期")
	}
	mustStopWithin(t, stop, 5*time.Second)

	if n := rec.count.Load(); n != 0 {
		ev := <-rec.ch
		t.Fatalf("stop 在续期在途时调用,不应再 onLost;实际调用 %d 次: %v", n, ev.err)
	}
}

// 拿锁后隔 900ms 才起心跳,续期节拍仍须从 SETNX 发出时刻排起:首拍在拿锁后 1.0s 发出。EVAL 全部报错
// 且 300ms 才回包(低于续期 ctx 超时 375ms):首拍 1.3s 回包,since ≈ 1.3s < 门槛 2s 不降级;第 2 拍定在
// 2.0s,since ≥ 2s 降级,计数 2。回归点:按 StartHeartbeat 时刻起 ticker 时首拍在 1.9s 发出、2.2s 回包,
// since ≥ 2s,第 1 拍就降级,计数 1;这种错相位下首个出错拍若快速失败逃过门槛,第 2 个出错拍卡满调用上限
// 就会拖过 key 过期(见 leader.FenceAfter)。
func TestStartHeartbeat_RenewScheduleAlignsWithLockAcquisition(t *testing.T) {
	const ttl = 3 * time.Second
	const interval = ttl / 3
	_, fault, lock := newHeartbeatFixture(t, ttl)
	fault.setFailIf(func(int64) bool { return true })
	fault.setFailDelay(300 * time.Millisecond)

	time.Sleep(900 * time.Millisecond)
	rec := newLostRecorder(fault)
	stop := lock.StartHeartbeat(interval, ttl, rec.onLost)
	t.Cleanup(stop)

	select {
	case ev := <-rec.ch:
		if ev.failedSincePass != 2 {
			t.Fatalf("应在第 2 个出错拍降级,实际报错 %d 次后降级;续期节拍没有对齐拿锁时刻", ev.failedSincePass)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("续期持续报错 5s 仍未自我降级")
	}

	mustStop(t, stop)
	if n := rec.count.Load(); n != 1 {
		t.Fatalf("onLost 应恰好调用 1 次,实际 %d", n)
	}
}

// 客户端不开 ContextTimeoutEnabled、读 500ms + 写 300ms:socket 读写不认续期 ctx 截止时间,maxCall 须按
// CallBudget(375ms, 800ms) = 1.175s 算(≥ interval 1s),FenceAfter 满足不了约束,任一续期报错即降级。
// EVAL 报错 300ms 才回包(低于读超时,ERR 能回到客户端):第 1 个出错拍即 onLost,计数 1。
// 读写上界单独(800ms)小于 interval,所以两种回归都会被抓住:取 max(375ms, 800ms) 或忽略
// GoRedisMaxCallDuration 时 maxCall < 1s、门槛 2s,首拍 since ≈ 1.3s 逃过,第 2 拍才降级,计数 2。
func TestStartHeartbeat_FenceUsesClientWorstCaseCall(t *testing.T) {
	const ttl = 3 * time.Second
	const interval = ttl / 3
	_, fault, lock := newHeartbeatFixtureWithOptions(t, ttl, &redis.Options{
		ReadTimeout:  500 * time.Millisecond,
		WriteTimeout: 300 * time.Millisecond,
	})
	fault.setFailIf(func(int64) bool { return true })
	fault.setFailDelay(300 * time.Millisecond)

	rec := newLostRecorder(fault)
	stop := lock.StartHeartbeat(interval, ttl, rec.onLost)
	t.Cleanup(stop)

	select {
	case ev := <-rec.ch:
		if ev.failedSincePass != 1 {
			t.Fatalf("客户端调用上限不小于心跳间隔时应在第 1 个出错拍降级,实际报错 %d 次后降级", ev.failedSincePass)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("续期持续报错 5s 仍未自我降级")
	}

	mustStop(t, stop)
	if n := rec.count.Load(); n != 1 {
		t.Fatalf("onLost 应恰好调用 1 次,实际 %d", n)
	}
}
