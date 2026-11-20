package svc

import (
	"context"
	"testing"

	"github.com/redis/go-redis/v9"

	"login/internal/config"
	"login/internal/logic/pkg/loginqueue"

	"shared/clusterredis"
)

// 本文件盯住 login 侧排队句柄的接缝(shared-redis-split-by-purpose.md §25.3 第 10 条;
// redis-cluster-all-domains-20261110.md §4.3、§5.1):
// 排队、放行锁与 gate 标记都走控制集群句柄(ControlRedis;单独的排队实例 Node.QueueRedis 第 6b-2 批删除);
// 放行流水线走专用句柄,带守卫的排队句柄上才放得出一整批;gate 标记按本 login 的 zone 读。
// 与 login_queue_wiring_test.go 一样改写包级 config.AppConfig,不能加 t.Parallel()。

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
		if _, err := s.LoginQueue.Enqueue(ctx, 7, "", "dev"); err != nil {
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
