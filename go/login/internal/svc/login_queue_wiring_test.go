package svc

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"login/internal/config"
	"login/internal/logic/pkg/loginqueue"
)

// 本文件盯住 G1b 的接线:出队 leader 锁按本 login 的 Node.ZoneId 拆,
// 锁键 = Queue.DispatcherLockKey + ":{q:<ZoneId>}"(loginqueue.ZoneDispatcherLockKey);开了排队却给不出合法 zone 时起服即失败。
//
// 这几个用例改写包级 config.AppConfig,所以本包用例都不能加 t.Parallel()。

func loginQueueTestConf(lockKeyBase string) config.QueueConf {
	return config.QueueConf{
		Enabled:           true,
		DispatchInterval:  30 * time.Millisecond,
		AdmitTTL:          60 * time.Second,
		QueueEntryTTL:     time.Hour,
		SoftCapMultiplier: 1.5,
		DispatcherLockTTL: 2 * time.Second,
		DispatcherLockKey: lockKeyBase,
	}
}

// dispatcherPanicCount 汇总 safego_panic_total 里点位以 login.queue_dispatcher 开头的计数
// (选主循环 login.queue_dispatcher 与任期 login.queue_dispatcher_term)。还没有任何 panic 时
// 这条序列不存在(safego 惰性注册),返回 0。计数是进程级累加的,用例一律比较增量。
// safego 的计数器是 client_golang 原生注册的,不经 go-zero 的指标开关,不用 Enable。
func dispatcherPanicCount(t *testing.T) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	var total float64
	for _, mf := range families {
		if mf.GetName() != "safego_panic_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "point" && strings.HasPrefix(lp.GetValue(), "login.queue_dispatcher") {
					total += m.GetCounter().GetValue()
				}
			}
		}
	}
	return total
}

// GateWatcher 留 nil:队列为空时 dispatcher 不碰 gate 目录(见 loginqueue 的
// TestDispatchOnce_EmptyQueueSkipsGateDirectory),这一轮选主 + 空跑不会走到 CandidatesForZone。
// 「先判空再算容量」写反时,nil watcher 会在 dispatcher 的任期里 panic,被 safego 接住、不让用例失败。
// 所以用例末尾有两条机器判据:停机后锁键必须已删(正常停机放锁),以及 dispatcher 点位的
// safego_panic_total 不许增加。任期 panic 由 electAndRun 里的 safego.Run 同步接住、计数先于
// Stop 返回,所以计数断言是确定的;顺序本身仍由上面那条 loginqueue 用例负责。
func TestInitLoginQueue_DispatcherLocksOwnZone(t *testing.T) {
	previous := config.AppConfig
	t.Cleanup(func() { config.AppConfig = previous })
	rdb, mr := newSvcTestRedis(t)

	const base = "dispatcher:lock:login_queue"
	config.AppConfig = config.Config{}
	config.AppConfig.Node.ZoneId = 7
	config.AppConfig.Queue = loginQueueTestConf(base)

	s := &ServiceContext{ControlRedis: rdb}
	s.initLoginQueue()
	if s.QueueDispatcher == nil {
		t.Fatal("Queue.Enabled=true must build a dispatcher")
	}
	panicsBefore := dispatcherPanicCount(t)
	s.QueueDispatcher.Start()
	defer s.QueueDispatcher.Stop() // Stop 可重复调用;中途 Fatal 时由它收尾

	want := loginqueue.ZoneDispatcherLockKey(base, 7)
	deadline := time.Now().Add(3 * time.Second)
	for !mr.Exists(want) {
		if time.Now().After(deadline) {
			t.Fatalf("zone 7 leader lock %q was never acquired", want)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if mr.Exists(base) {
		t.Fatalf("legacy global lock key %q must not be written any more", base)
	}

	s.QueueDispatcher.Stop()
	if mr.Exists(want) {
		t.Fatalf("zone 7 leader lock %q still held after Stop: a normal shutdown must release it; "+
			"a key left behind means the dispatcher goroutine exited abnormally", want)
	}
	if got := dispatcherPanicCount(t) - panicsBefore; got != 0 {
		t.Fatalf("dispatcher panicked %v time(s) while idling on an empty queue (safego swallowed it)", got)
	}
}

func TestInitLoginQueue_ZeroZonePanicsWhenQueueEnabled(t *testing.T) {
	previous := config.AppConfig
	t.Cleanup(func() { config.AppConfig = previous })
	rdb, _ := newSvcTestRedis(t)

	config.AppConfig = config.Config{} // Node.ZoneId = 0
	config.AppConfig.Queue = loginQueueTestConf("dispatcher:lock:login_queue")

	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("Queue.Enabled=true with Node.ZoneId=0 must panic at startup (queue would only grow)")
		}
		// 只认 dispatcher 的 zone 校验:initLoginQueue 在走到 NewDispatcher 之前还有别的步骤,
		// 那里任何无关的 panic 都不能让本用例假绿。
		if !strings.Contains(fmt.Sprint(recovered), "zone id must be non-zero") {
			t.Fatalf("panic = %v, want the dispatcher zone validation", recovered)
		}
	}()
	(&ServiceContext{ControlRedis: rdb}).initLoginQueue()
}

// (2026-10-15)开了排队时 QueueEntryTTL 非正、AdmitTTL 不足 1ms 即起服失败(loginqueue.ValidateTTLs),
// 显式写 0s 不回落默认值。修复前红:initLoginQueue 原样把 0 / 负值交给 loginqueue.New,不 panic,
// 带着「每次入队删整条队列」或「每拍拒绝出队」的配置上线。
// (2026-10-18 补注)QueueEntryTTL 收紧为 ∈ [1s, loginqueue.queueTokenMaxAge=24h],越界同样起服失败,报错串仍含
// "ttl must be positive"。1s 下界与 Enqueue 的秒级 EXPIRE、续期毫秒值为正对齐;上界是令牌 24h 先到期,更大的空闲
// TTL 没有意义。装配层这两组守的是「起服即失败」:loginqueue 包内的 TestValidateTTLs_RejectsNonPositive 只钉住
// 函数本身,initLoginQueue 换成只判正数的本地校验时只有这里会红。修复前红:500ms 与 25h 两组不 panic。
func TestInitLoginQueue_NonPositiveTTLPanicsWhenQueueEnabled(t *testing.T) {
	cases := []struct {
		name               string
		admitTTL, entryTTL time.Duration
	}{
		{"AdmitTTL=0", 0, time.Hour},
		{"AdmitTTL=-1s", -time.Second, time.Hour},
		{"AdmitTTL=500us", 500 * time.Microsecond, time.Hour}, // 放行脚本 SET PX 取整到毫秒,亚毫秒成 PX 0
		{"QueueEntryTTL=0", 60 * time.Second, 0},
		{"QueueEntryTTL=-1s", 60 * time.Second, -time.Second},
		{"QueueEntryTTL=500ms", 60 * time.Second, 500 * time.Millisecond}, // 2026-10-18:低于 1s 下界
		{"QueueEntryTTL=25h", 60 * time.Second, 25 * time.Hour},           // 2026-10-18:高于令牌 24h 上界
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			previous := config.AppConfig
			t.Cleanup(func() { config.AppConfig = previous })
			rdb, _ := newSvcTestRedis(t)

			config.AppConfig = config.Config{}
			config.AppConfig.Node.ZoneId = 7 // 合法 zone:只让 TTL 校验有机会失败
			config.AppConfig.Queue = loginQueueTestConf("dispatcher:lock:login_queue")
			config.AppConfig.Queue.AdmitTTL = tc.admitTTL
			config.AppConfig.Queue.QueueEntryTTL = tc.entryTTL

			s := &ServiceContext{ControlRedis: rdb}
			defer func() {
				recovered := recover()
				if recovered == nil {
					t.Fatalf("Queue.Enabled=true with %s must panic at startup", tc.name)
				}
				// 只认 TTL 校验:别的 panic 不能让本用例假绿。
				if !strings.Contains(fmt.Sprint(recovered), "ttl must be positive") {
					t.Fatalf("panic = %v, want the TTL validation", recovered)
				}
				if s.LoginQueue != nil || s.QueueDispatcher != nil {
					t.Fatal("a rejected TTL must build neither the queue nor the dispatcher")
				}
			}()
			s.initLoginQueue()
		})
	}
}

// 默认关:不构造队列和 dispatcher,也就不做任何新校验,与修复前一致。
func TestInitLoginQueue_DisabledSkipsDispatcherValidation(t *testing.T) {
	previous := config.AppConfig
	t.Cleanup(func() { config.AppConfig = previous })
	rdb, _ := newSvcTestRedis(t)

	config.AppConfig = config.Config{}
	config.AppConfig.Queue = loginQueueTestConf("")
	config.AppConfig.Queue.Enabled = false

	s := &ServiceContext{ControlRedis: rdb}
	s.initLoginQueue()
	if s.LoginQueue != nil || s.QueueDispatcher != nil {
		t.Fatal("Queue.Enabled=false must build neither the queue nor the dispatcher")
	}
}
