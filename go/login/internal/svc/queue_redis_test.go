package svc

import (
	"context"
	"strings"
	"testing"
	"time"

	"login/internal/config"
	"login/internal/logic/pkg/loginqueue"
)

// 本文件盯住 G4a 第 1a 批 login 侧的接缝(shared-redis-split-by-purpose.md §25.3 第 10 条):
// 排队与放行锁走 Node.QueueRedis;没配置时与核心是同一个 client;gate 排空标记不跟着走。
// 与 login_queue_wiring_test.go 一样改写包级 config.AppConfig,不能加 t.Parallel()。

func TestNewQueueRedis_NilConfigSharesPointer(t *testing.T) {
	core, _ := newSvcTestRedis(t)
	if got := newQueueRedis(context.Background(), core, nil); got != core {
		t.Fatal("Node.QueueRedis 没配置时必须原样返回核心 client:同一个指针,不多开连接池")
	}
}

func TestNewQueueRedis_ConfiguredBuildsSeparateClient(t *testing.T) {
	core, _ := newSvcTestRedis(t)
	_, queueMr := newSvcTestRedis(t)

	got := newQueueRedis(context.Background(), core, &config.RedisConf{Host: queueMr.Addr(), PoolSize: 3})
	t.Cleanup(func() { _ = got.Close() })

	if got == core {
		t.Fatal("配置了 Node.QueueRedis 必须另建 client")
	}
	if got.Options().Addr != queueMr.Addr() || got.Options().PoolSize != 3 {
		t.Fatalf("queue client 没按配置建: addr=%q pool=%d", got.Options().Addr, got.Options().PoolSize)
	}
}

// 配了 QueueRedis 时,放行锁与排队键只出现在 queue 实例上,核心上一个都没有。
func TestInitLoginQueue_QueueKeysLandOnQueueRedis(t *testing.T) {
	previous := config.AppConfig
	t.Cleanup(func() { config.AppConfig = previous })
	coreRdb, coreMr := newSvcTestRedis(t)
	queueRdb, queueMr := newSvcTestRedis(t)

	const base = "dispatcher:lock:login_queue"
	config.AppConfig = config.Config{}
	config.AppConfig.Node.ZoneId = 7
	config.AppConfig.Queue = loginQueueTestConf(base)

	s := &ServiceContext{RedisClient: coreRdb, QueueRedis: queueRdb}
	s.initLoginQueue()
	if s.QueueDispatcher == nil || s.LoginQueue == nil {
		t.Fatal("Queue.Enabled=true 必须装好排队与 dispatcher")
	}

	// 放行锁:队列为空时 dispatcher 只选主不碰 gate 目录(同 TestInitLoginQueue_DispatcherLocksOwnZone)。
	s.QueueDispatcher.Start()
	defer s.QueueDispatcher.Stop()
	lock := loginqueue.ZoneDispatcherLockKey(base, 7)
	deadline := time.Now().Add(3 * time.Second)
	for !queueMr.Exists(lock) {
		if time.Now().After(deadline) {
			t.Fatalf("放行锁 %q 没有写到 queue 实例", lock)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if coreMr.Exists(lock) {
		t.Fatalf("放行锁 %q 不得写到核心实例", lock)
	}
	s.QueueDispatcher.Stop() // 停掉再入队:队列非空时 dispatcher 会去挑 gate,这里没有 GateWatcher

	if _, err := s.LoginQueue.Enqueue(context.Background(), 7, "acct-queue-redis", "dev-1"); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if !queueMr.Exists("queue:zone:7") {
		t.Fatalf("排队 ZSET 必须在 queue 实例上,queue 实例现有键: %v", queueMr.Keys())
	}
	for _, k := range coreMr.Keys() {
		if strings.HasPrefix(k, "queue:") || strings.HasPrefix(k, "admit") || strings.HasPrefix(k, "dispatcher:") {
			t.Fatalf("核心实例上不得出现排队族键 %q", k)
		}
	}
}

// gate 排空标记留在核心:k8s_gate_drain.ps1 与 scene_manager 都读核心上的那一份,
// login 的 AssignGate 过滤(gateWatcherCapacityProvider.rdb)必须同样读核心。
func TestInitLoginQueue_DrainFilterStaysOnCoreRedis(t *testing.T) {
	previous := config.AppConfig
	t.Cleanup(func() { config.AppConfig = previous })
	coreRdb, _ := newSvcTestRedis(t)
	queueRdb, _ := newSvcTestRedis(t)

	config.AppConfig = config.Config{}
	config.AppConfig.Node.ZoneId = 7
	config.AppConfig.Queue = loginQueueTestConf("dispatcher:lock:login_queue")
	config.AppConfig.Queue.Enabled = false // 只看容量提供者的装配,它不论排队开关都会装

	s := &ServiceContext{RedisClient: coreRdb, QueueRedis: queueRdb}
	s.initLoginQueue()
	provider, ok := s.queueCapProvider.(*gateWatcherCapacityProvider)
	if !ok {
		t.Fatalf("queueCapProvider 类型变了: %T", s.queueCapProvider)
	}
	if provider.rdb != coreRdb {
		t.Fatal("排空过滤必须读核心实例:gate 排空标记不随 queue 拆分")
	}
}
