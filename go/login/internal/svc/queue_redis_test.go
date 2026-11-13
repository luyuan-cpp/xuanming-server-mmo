package svc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"login/internal/config"
	"login/internal/logic/pkg/loginqueue"

	"shared/clusterredis"
)

// 本文件盯住 login 侧排队句柄的接缝(shared-redis-split-by-purpose.md §25.3 第 10 条;
// redis-cluster-all-domains-20261110.md §4.3、§5.1):
// 排队与放行锁走 Node.QueueRedis;没配置时与控制集群句柄是同一个客户端;gate 标记不跟着走;
// 放行流水线走专用句柄,带守卫的排队句柄上才放得出一整批。
// 与 login_queue_wiring_test.go 一样改写包级 config.AppConfig,不能加 t.Parallel()。

func TestNewQueueRedis_NilConfigSharesTheControlClient(t *testing.T) {
	control, mr := newSvcTestRedis(t)
	controlConf := clusterredis.Config{Addrs: []string{mr.Addr()}}
	got, conf := newQueueRedis(context.Background(), control, controlConf, nil)
	if got != redis.UniversalClient(control) {
		t.Fatal("Node.QueueRedis 没配置时必须原样返回控制集群句柄:同一个客户端,不多开连接池")
	}
	if len(conf.Addrs) != 1 || conf.Addrs[0] != mr.Addr() {
		t.Fatalf("没配置时排队所连实例的配置就是控制集群的配置,got %+v", conf)
	}
}

func TestNewQueueRedis_ConfiguredBuildsSeparateGuardedClient(t *testing.T) {
	control, _ := newSvcTestRedis(t)
	_, queueMr := newSvcTestRedis(t)
	ctx := context.Background()

	split := clusterredis.Config{Addrs: []string{queueMr.Addr()}, PoolSizePerNode: 3}
	got, conf := newQueueRedis(ctx, control, clusterredis.Config{Addrs: []string{"unused:1"}}, &split)
	t.Cleanup(func() { _ = got.Close() })

	if got == redis.UniversalClient(control) {
		t.Fatal("配置了 Node.QueueRedis 必须另建客户端")
	}
	single, ok := got.(*redis.Client)
	if !ok || single.Options().Addr != queueMr.Addr() || single.Options().PoolSize != 3 {
		t.Fatalf("排队句柄没按配置建: %T %+v", got, conf)
	}
	if len(conf.Addrs) != 1 || conf.Addrs[0] != queueMr.Addr() {
		t.Fatalf("返回的配置必须是排队实例的配置(放行流水线的专用句柄按它建),got %+v", conf)
	}
	// 另建的句柄同样带控制集群守卫:玩家域形状的键在发送前就被拒。
	if err := got.Get(ctx, "session:{42}").Err(); !errors.Is(err, clusterredis.ErrWrongDomainTag) {
		t.Fatalf("排队句柄必须带控制集群守卫,玩家域的键应被拒,got %v", err)
	}
}

// 生产的接线:排队句柄带守卫,放行流水线换成专用句柄。一拍放行 4 个人(探路 1 条 + 流水线 3 条)都要放得出去;
// 专用句柄漏接时流水线会被守卫整批拒绝(loginqueue 的 TestPopAdmit_GuardedClientAloneRejectsThePipelineAndKeepsEntriesQueued)。
func TestInitLoginQueue_AdmitPipelineUsesTheUnguardedClient(t *testing.T) {
	previous := config.AppConfig
	t.Cleanup(func() { config.AppConfig = previous })
	_, mr := newSvcTestRedis(t)
	ctx := context.Background()

	config.AppConfig = config.Config{}
	config.AppConfig.Node.ZoneId = 7
	config.AppConfig.Queue = loginQueueTestConf("dispatcher:lock:login_queue")

	conf := clusterredis.Config{Addrs: []string{mr.Addr()}, PoolSizePerNode: 4}
	control := mustNewControlRedis(ctx, "ControlRedis", conf)
	admit := mustNewQueueAdmitRedis(ctx, conf)
	t.Cleanup(func() { _ = control.Close(); _ = admit.Close() })
	if single, ok := admit.(*redis.Client); !ok || single.Options().PoolSize != 1 {
		t.Fatalf("放行流水线的专用句柄每个节点只要一条连接,got %T", admit)
	}

	s := &ServiceContext{ControlRedis: control, queueAdmitRedis: admit}
	s.initLoginQueue()
	if s.LoginQueue == nil {
		t.Fatal("Queue.Enabled=true 必须装好排队")
	}
	for i := 0; i < 4; i++ {
		if _, err := s.LoginQueue.Enqueue(ctx, 7, "acct", "dev"); err != nil {
			t.Fatalf("Enqueue #%d: %v", i, err)
		}
	}
	pick := func(zoneID uint32, _ string) (*loginqueue.AdmitSlot, error) {
		return &loginqueue.AdmitSlot{NodeID: 1, IP: "10.0.0.1", Port: 9000, ZoneID: zoneID}, nil
	}
	if n, err := s.LoginQueue.PopAdmit(ctx, 7, 4, 100, pick); err != nil || n != 4 {
		t.Fatalf("PopAdmit = (%d, %v), want (4, nil): the pipelined chunk must go through the unguarded client", n, err)
	}
}

// 配了 QueueRedis 时,放行锁与排队键只出现在 queue 实例上,控制集群句柄所连的实例上一个都没有。
func TestInitLoginQueue_QueueKeysLandOnQueueRedis(t *testing.T) {
	previous := config.AppConfig
	t.Cleanup(func() { config.AppConfig = previous })
	coreRdb, coreMr := newSvcTestRedis(t)
	queueRdb, queueMr := newSvcTestRedis(t)

	const base = "dispatcher:lock:login_queue"
	config.AppConfig = config.Config{}
	config.AppConfig.Node.ZoneId = 7
	config.AppConfig.Queue = loginQueueTestConf(base)

	s := &ServiceContext{ControlRedis: coreRdb, QueueRedis: queueRdb}
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
		t.Fatalf("放行锁 %q 不得写到控制集群句柄所连的实例", lock)
	}
	s.QueueDispatcher.Stop() // 停掉再入队:队列非空时 dispatcher 会去挑 gate,这里没有 GateWatcher

	if _, err := s.LoginQueue.Enqueue(context.Background(), 7, "acct-queue-redis", "dev-1"); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if !queueMr.Exists("queue:{q:7}") {
		t.Fatalf("排队 ZSET 必须在 queue 实例上,queue 实例现有键: %v", queueMr.Keys())
	}
	for _, k := range coreMr.Keys() {
		if strings.HasPrefix(k, "queue:") || strings.HasPrefix(k, "admit") || strings.HasPrefix(k, "dispatcher:") {
			t.Fatalf("控制集群句柄所连的实例上不得出现排队族键 %q", k)
		}
	}
}

// gate 排空标记走 ControlRedis,不跟着排队句柄走:用两份 Redis 上相反的标记验证实际读源,每个场景使用首次读取的独立缓存。
// 标记键里的 zone 是本 login 的 Node.ZoneId。
func TestInitLoginQueue_DrainFilterStaysOnControlRedis(t *testing.T) {
	previous := config.AppConfig
	t.Cleanup(func() { config.AppConfig = previous })
	config.AppConfig = config.Config{}
	config.AppConfig.Node.ZoneId = 7
	config.AppConfig.Queue = loginQueueTestConf("dispatcher:lock:login_queue")
	config.AppConfig.Queue.Enabled = false // 容量提供者不依赖排队开关。

	const gateID uint32 = 41
	for _, test := range []struct {
		name                       string
		controlMarked, queueMarked bool
		wantCount                  int
	}{
		{name: "control_mark_only", controlMarked: true, wantCount: 1},
		{name: "queue_mark_only", queueMarked: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			controlRdb, _ := newSvcTestRedis(t)
			queueRdb, _ := newSvcTestRedis(t)
			ctx := context.Background()
			if test.controlMarked {
				if err := loginqueue.MarkGateDraining(ctx, controlRdb, 7, gateID, 1000, 60); err != nil {
					t.Fatalf("mark gate draining in the control Redis: %v", err)
				}
			}
			if test.queueMarked {
				if err := loginqueue.MarkGateDraining(ctx, queueRdb, 7, gateID, 1000, 60); err != nil {
					t.Fatalf("mark gate draining in queue Redis: %v", err)
				}
			}
			s := &ServiceContext{ControlRedis: controlRdb, QueueRedis: queueRdb}
			s.initLoginQueue()
			provider, ok := s.queueCapProvider.(*gateWatcherCapacityProvider)
			if !ok || provider == nil || provider.drain == nil {
				t.Fatalf("queue capacity provider has no drain cache: %T", s.queueCapProvider)
			}
			draining := provider.drain.Draining(ctx, 7, []loginqueue.GateCandidate{{NodeID: gateID}})
			if len(draining) != test.wantCount || draining[gateID] != test.controlMarked {
				t.Fatalf("draining gates = %v, want count=%d control mark=%v; queue mark=%v must not affect selection", draining, test.wantCount, test.controlMarked, test.queueMarked)
			}
		})
	}
}

// 标记键里的 zone 取本 login 的 Node.ZoneId,不取候选自带的 ZoneID,也不取查询时传的分区键:
// 别的 zone 号下的同号 gate 标记不算数。
func TestInitLoginQueue_DrainMarksAreReadUnderTheLoginsOwnZone(t *testing.T) {
	previous := config.AppConfig
	t.Cleanup(func() { config.AppConfig = previous })
	config.AppConfig = config.Config{}
	config.AppConfig.Node.ZoneId = 7
	config.AppConfig.Queue = loginQueueTestConf("dispatcher:lock:login_queue")
	config.AppConfig.Queue.Enabled = false

	rdb, _ := newSvcTestRedis(t)
	ctx := context.Background()
	const ownGate, otherGate uint32 = 41, 42
	if err := loginqueue.MarkGateDraining(ctx, rdb, 7, ownGate, 1000, 60); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if err := loginqueue.MarkGateDraining(ctx, rdb, 8, otherGate, 1000, 60); err != nil {
		t.Fatalf("mark: %v", err)
	}
	s := &ServiceContext{ControlRedis: rdb}
	s.initLoginQueue()
	provider := s.queueCapProvider.(*gateWatcherCapacityProvider)
	// 分区键传 0(不按 zone 过滤的那一份候选)也照样按本 zone 读。
	draining := provider.drain.Draining(ctx, 0, []loginqueue.GateCandidate{{NodeID: ownGate, ZoneID: 9}, {NodeID: otherGate, ZoneID: 8}})
	if len(draining) != 1 || !draining[ownGate] {
		t.Fatalf("draining = %v, want only gate %d (marked under zone 7)", draining, ownGate)
	}
}
