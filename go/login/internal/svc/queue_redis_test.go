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

// gate 排空标记留在核心：用两份 Redis 上相反的标记验证实际读源，每个场景使用首次读取的独立缓存。
func TestInitLoginQueue_DrainFilterStaysOnCoreRedis(t *testing.T) {
	previous := config.AppConfig
	t.Cleanup(func() { config.AppConfig = previous })
	config.AppConfig = config.Config{}
	config.AppConfig.Node.ZoneId = 7
	config.AppConfig.Queue = loginQueueTestConf("dispatcher:lock:login_queue")
	config.AppConfig.Queue.Enabled = false // 容量提供者不依赖排队开关。

	const gateID uint32 = 41
	for _, test := range []struct {
		name                    string
		coreMarked, queueMarked bool
		wantCount               int
	}{
		{name: "core_mark_only", coreMarked: true, wantCount: 1},
		{name: "queue_mark_only", queueMarked: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			coreRdb, _ := newSvcTestRedis(t)
			queueRdb, _ := newSvcTestRedis(t)
			ctx := context.Background()
			if test.coreMarked {
				if err := loginqueue.MarkGateDraining(ctx, coreRdb, gateID, 1000, 60); err != nil {
					t.Fatalf("mark gate draining in core Redis: %v", err)
				}
			}
			if test.queueMarked {
				if err := loginqueue.MarkGateDraining(ctx, queueRdb, gateID, 1000, 60); err != nil {
					t.Fatalf("mark gate draining in queue Redis: %v", err)
				}
			}
			s := &ServiceContext{RedisClient: coreRdb, QueueRedis: queueRdb}
			s.initLoginQueue()
			provider, ok := s.queueCapProvider.(*gateWatcherCapacityProvider)
			if !ok || provider == nil || provider.drain == nil {
				t.Fatalf("queue capacity provider has no drain cache: %T", s.queueCapProvider)
			}
			draining := provider.drain.Draining(ctx, 7, []loginqueue.GateCandidate{{NodeID: gateID}})
			if len(draining) != test.wantCount || draining[gateID] != test.coreMarked {
				t.Fatalf("draining gates = %v, want count=%d core mark=%v; queue mark=%v must not affect selection", draining, test.wantCount, test.coreMarked, test.queueMarked)
			}
		})
	}
}
