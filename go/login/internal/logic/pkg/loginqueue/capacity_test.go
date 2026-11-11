package loginqueue

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	gzprom "github.com/zeromicro/go-zero/core/prometheus"
)

// 区容量按 gate 承载封顶(2026-11-10,capacity.go 的 capByGates):
// 配置的区容量与 gate 台数写在两处,容量比 gate 到水位的承载大时,排队照样按容量放人,玩家连上 gate 才被
// GateMaxConnections 无声拒掉。以下用例钉住封顶之后 FreeSlots 与放行循环的可观察结果。

// zoneGaugeValue 从默认 registry 读 name{zone} 的当前值;序列不存在时 ok 为 false。要先 gzprom.Enable() 才写。
func zoneGaugeValue(t *testing.T, name string, zoneID uint32) (float64, bool) {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	zone := strconv.FormatUint(uint64(zoneID), 10)
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := m.GetLabel()
			if len(labels) == 1 && labels[0].GetName() == "zone" && labels[0].GetValue() == zone {
				return m.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

// deploy/k8s/zones.json 的形状:每区 2 台 gate、每台 GateMaxConnections 20000,开服档区容量 45000。
// 到水位(90%)两台一共只装得下 36000,区容量按它封顶;快速通道的占位预算(capacity − online)随之变小。
func TestFreeSlots_ConfiguredCapacityCappedAtGateWatermark(t *testing.T) {
	q, _, _, cleanup := newTestQueue(t)
	defer cleanup()
	prov := &fakeProvider{
		cands: map[uint32][]GateCandidate{1: {
			{NodeID: 1, IP: "10.0.0.1", Port: 9000, PlayerCount: 15000, MaxConnections: 20000, ZoneID: 1},
			{NodeID: 2, IP: "10.0.0.2", Port: 9000, PlayerCount: 15000, MaxConnections: 20000, ZoneID: 1},
		}},
		caps: map[uint32]uint32{1: 45000},
	}

	free, capacity, online, admitted, err := FreeSlots(context.Background(), prov, q, 1, 1.5)
	if err != nil {
		t.Fatalf("FreeSlots: %v", err)
	}
	if free != 6000 || capacity != 36000 || online != 30000 || admitted != 0 {
		t.Errorf("FreeSlots = (free=%d cap=%d online=%d admitted=%d), want (6000, 36000, 30000, 0)",
			free, capacity, online, admitted)
	}
}

// 两台都到水位时一个空位都不剩,排队开始拦人。封顶之前容量是 45000,这里还会放出 9000 张票,
// 拿票的人连上 gate 才被拒。
func TestFreeSlots_NoSlotsOnceEveryGateAtWatermark(t *testing.T) {
	q, _, _, cleanup := newTestQueue(t)
	defer cleanup()
	prov := &fakeProvider{
		cands: map[uint32][]GateCandidate{1: {
			{NodeID: 1, IP: "10.0.0.1", Port: 9000, PlayerCount: 18000, MaxConnections: 20000, ZoneID: 1},
			{NodeID: 2, IP: "10.0.0.2", Port: 9000, PlayerCount: 18000, MaxConnections: 20000, ZoneID: 1},
		}},
		caps: map[uint32]uint32{1: 45000},
	}

	free, capacity, _, _, err := FreeSlots(context.Background(), prov, q, 1, 1.5)
	if err != nil {
		t.Fatalf("FreeSlots: %v", err)
	}
	if free != 0 || capacity != 36000 {
		t.Errorf("FreeSlots = (free=%d cap=%d), want (0, 36000): every gate is at its watermark", free, capacity)
	}
}

// 容量只数在接客的 gate:排空中、停写的 gate 不贡献容量,它们上面的人照样算在线(稍后要挪到在接客的 gate 上)。
func TestFreeSlots_GateCapacityCountsOnlyServingGates(t *testing.T) {
	q, _, _, cleanup := newTestQueue(t)
	defer cleanup()
	prov := &fakeProvider{
		cands: map[uint32][]GateCandidate{1: {
			{NodeID: 1, IP: "10.0.0.1", Port: 9000, PlayerCount: 50, MaxConnections: 100, ZoneID: 1},
			{NodeID: 2, IP: "10.0.0.2", Port: 9000, PlayerCount: 10, MaxConnections: 1000, ZoneID: 1, Draining: true},
			{NodeID: 3, IP: "10.0.0.3", Port: 9000, PlayerCount: 0, MaxConnections: 1000, ZoneID: 1, Stale: true},
		}},
		caps: map[uint32]uint32{1: 1000},
	}

	free, capacity, online, _, err := FreeSlots(context.Background(), prov, q, 1, 1.5)
	if err != nil {
		t.Fatalf("FreeSlots: %v", err)
	}
	// 容量 = 第 1 台到水位的 90 人;在线 = 三台合计 60 人。
	if free != 30 || capacity != 90 || online != 60 {
		t.Errorf("FreeSlots = (free=%d cap=%d online=%d), want (30, 90, 60)", free, capacity, online)
	}
}

// 在接客的 gate 里有没声明上限的(MaxConnections 0:dev 下不设上限、旧版 gate)时不封顶,与封顶之前相同。
func TestFreeSlots_UndeclaredGateLimitKeepsConfiguredCapacity(t *testing.T) {
	q, _, _, cleanup := newTestQueue(t)
	defer cleanup()
	prov := &fakeProvider{
		cands: map[uint32][]GateCandidate{1: {
			{NodeID: 1, IP: "10.0.0.1", Port: 9000, PlayerCount: 10, MaxConnections: 100, ZoneID: 1},
			{NodeID: 2, IP: "10.0.0.2", Port: 9000, PlayerCount: 10, ZoneID: 1},
		}},
		caps: map[uint32]uint32{1: 1000},
	}

	free, capacity, _, _, err := FreeSlots(context.Background(), prov, q, 1, 1.5)
	if err != nil {
		t.Fatalf("FreeSlots: %v", err)
	}
	if free != 980 || capacity != 1000 {
		t.Errorf("FreeSlots = (free=%d cap=%d), want (980, 1000)", free, capacity)
	}
}

// 配置的容量本来就在 gate 承载以内时原样用。
func TestFreeSlots_CapacityWithinGateWatermarkUnchanged(t *testing.T) {
	q, _, _, cleanup := newTestQueue(t)
	defer cleanup()
	prov := &fakeProvider{
		cands: map[uint32][]GateCandidate{1: {
			{NodeID: 1, IP: "10.0.0.1", Port: 9000, PlayerCount: 100, MaxConnections: 20000, ZoneID: 1},
		}},
		caps: map[uint32]uint32{1: 5000},
	}

	free, capacity, _, _, err := FreeSlots(context.Background(), prov, q, 1, 1.5)
	if err != nil {
		t.Fatalf("FreeSlots: %v", err)
	}
	if free != 4900 || capacity != 5000 {
		t.Errorf("FreeSlots = (free=%d cap=%d), want (4900, 5000)", free, capacity)
	}
}

// 没配容量时软上限同样被封顶:软上限随在线人数涨(80 × 1.5 = 120),不能涨过 gate 装得下的人数(90)。
func TestFreeSlots_SoftCapAlsoCappedAtGateWatermark(t *testing.T) {
	q, _, _, cleanup := newTestQueue(t)
	defer cleanup()
	prov := &fakeProvider{
		cands: map[uint32][]GateCandidate{1: {
			{NodeID: 1, IP: "10.0.0.1", Port: 9000, PlayerCount: 80, MaxConnections: 100, ZoneID: 1},
		}},
		caps: map[uint32]uint32{},
	}

	free, capacity, _, _, err := FreeSlots(context.Background(), prov, q, 1, 1.5)
	if err != nil {
		t.Fatalf("FreeSlots: %v", err)
	}
	if free != 10 || capacity != 90 {
		t.Errorf("FreeSlots = (free=%d cap=%d), want (10, 90)", free, capacity)
	}
}

// login_queue_capacity 是封顶之后实际用的容量,login_queue_configured_capacity 是配置值(0 = 没配、按软上限)。
// 前者小于后者就是告警 LoginQueueCapacityLimitedByGates 的条件;没配容量时后者为 0,软上限被封顶不告警。
func TestFreeSlots_PublishesEffectiveAndConfiguredCapacity(t *testing.T) {
	gzprom.Enable() // 指标要开关打开才写,同 gate_eligibility_test.go
	q, _, _, cleanup := newTestQueue(t)
	defer cleanup()
	// zone 号与其它用例错开:指标是进程级的。
	const limited, unconfigured = 7101, 7102
	prov := &fakeProvider{
		cands: map[uint32][]GateCandidate{
			limited: {
				{NodeID: 1, IP: "10.0.0.1", Port: 9000, PlayerCount: 100, MaxConnections: 20000, ZoneID: limited},
				{NodeID: 2, IP: "10.0.0.2", Port: 9000, PlayerCount: 100, MaxConnections: 20000, ZoneID: limited},
			},
			unconfigured: {{NodeID: 3, IP: "10.0.0.3", Port: 9000, PlayerCount: 10, ZoneID: unconfigured}},
		},
		caps: map[uint32]uint32{limited: 45000},
	}
	for _, zone := range []uint32{limited, unconfigured} {
		if _, _, _, _, err := FreeSlots(context.Background(), prov, q, zone, 1.5); err != nil {
			t.Fatalf("FreeSlots(zone=%d): %v", zone, err)
		}
	}

	cases := []struct {
		name string
		zone uint32
		want float64
	}{
		{"login_queue_capacity", limited, 36000},
		{"login_queue_configured_capacity", limited, 45000},
		{"login_queue_capacity", unconfigured, 15}, // 软上限 10 × 1.5,gate 没声明上限、不封顶
		{"login_queue_configured_capacity", unconfigured, 0},
	}
	for _, tc := range cases {
		got, ok := zoneGaugeValue(t, tc.name, tc.zone)
		if !ok || got != tc.want {
			t.Errorf("%s{zone=%d} = (%v, present=%v), want %v", tc.name, tc.zone, got, ok, tc.want)
		}
	}
}

// 放行循环与快速通道走同一处算容量。配置的区容量 10、唯一一台 gate 上限 5(到水位 5 人):一拍只放 5 个,
// 第二拍不再放(放出去的 5 个在途占位还在)。封顶之前第一拍放 8 个,后 3 个拿着票连 gate 才被拒。
func TestDispatchOnce_AdmitsNoMoreThanGatesHold(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	secret := []byte("gate-capacity")
	q := New(rdb, time.Hour, 60*time.Second, secret)
	ctx := context.Background()

	prov := &integrationProvider{
		caps:  map[uint32]uint32{1: 10},
		cands: map[uint32][]GateCandidate{1: {{NodeID: 1, IP: "10.0.0.1", Port: 9000, MaxConnections: 5, ZoneID: 1}}},
	}
	// 不 Start:直接调 dispatchOnce,没有 goroutine,结果确定。
	d := mustNewDispatcher(t, q, prov, rdb, secret, "dispatcher:lock:gatecap", 1)
	for range 8 {
		if _, err := q.Enqueue(ctx, 1, "u", "d"); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	mustQueueLen := func(want uint32, when string) {
		t.Helper()
		n, err := q.QueueLen(ctx, 1)
		if err != nil || n != want {
			t.Fatalf("queue len (%s) = %d err=%v, want %d", when, n, err, want)
		}
	}

	d.dispatchOnce(ctx)
	mustQueueLen(3, "first tick admits only what the gate holds at its watermark")
	d.dispatchOnce(ctx)
	mustQueueLen(3, "second tick admits nothing while the five in-flight placeholders hold the gate's capacity")
}
