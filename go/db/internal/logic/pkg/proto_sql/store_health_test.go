package proto_sql

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"db/internal/metrics"

	"github.com/go-sql-driver/mysql"
)

// fakePing 按顺序返回预置的探活结果,并记录调用次数。结果用完之后一直返回最后一个。
type fakePing struct {
	mu      sync.Mutex
	results []error
	calls   int
}

func (p *fakePing) ping(context.Context, *GameDB) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if len(p.results) == 0 {
		return nil
	}
	err := p.results[0]
	if len(p.results) > 1 {
		p.results = p.results[1:]
	}
	return err
}

func (p *fakePing) set(results ...error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.results = results
}

func (p *fakePing) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

var errDialRefused = &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}

// healthStoreID 是探活用例用的存档库:建注册表时先打开它,相当于运行中已经打开的库。
const healthStoreID = uint32(1000000)

func newHealthTestRegistry(t *testing.T, clock *fakeClock, ping *fakePing) *StoreRegistry {
	t.Helper()
	registry, err := NewStoreRegistry(StoreRegistryOptions{
		AllowFamilies: true,
		Open:          newFakeOpener().open,
		Now:           clock.Now,
		Cooldown:      5 * time.Second,
		Jitter:        func(d time.Duration) time.Duration { return d },
		Ping:          ping.ping,
	})
	if err != nil {
		t.Fatalf("NewStoreRegistry: %v", err)
	}
	if _, err := registry.Store(context.Background(), healthStoreID); err != nil {
		t.Fatalf("open the store under test: %v", err)
	}
	return registry
}

func assertCoolingError(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrStoreUnavailable) || !errors.Is(err, ErrStoreCoolingDown) {
		t.Fatalf("want an error wrapping ErrStoreUnavailable and ErrStoreCoolingDown, got %v", err)
	}
}

// 连接数满时服务端在应答:探活拿到 1040 也不进冷却,一次局部故障不能让整个实例停写(L-04 评审 F1)。
func TestReportConnLost_ProbeAnsweredByServerDoesNotCoolDown(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	ping := &fakePing{}
	ping.set(&mysql.MySQLError{Number: 1040, Message: "Too many connections"})
	registry := newHealthTestRegistry(t, clock, ping)

	registry.ReportConnLost(healthStoreID,mysql.ErrInvalidConn)

	if registry.CoolingDown(healthStoreID) {
		t.Fatal("a probe answered by the server must not start a cooldown")
	}
	if _, err := registry.Store(context.Background(), healthStoreID); err != nil {
		t.Fatalf("store must stay available: %v", err)
	}
}

// 探活也连不上才冷却;冷却期内直接挡回,不再探活。
func TestReportConnLost_UnreachableProbeStartsCooldown(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	ping := &fakePing{}
	ping.set(errDialRefused)
	registry := newHealthTestRegistry(t, clock, ping)

	registry.ReportConnLost(healthStoreID,mysql.ErrInvalidConn)
	if !registry.CoolingDown(healthStoreID) {
		t.Fatal("an unreachable probe must start a cooldown")
	}
	_, err := registry.Store(context.Background(), healthStoreID)
	assertCoolingError(t, err)

	registry.ReportConnLost(healthStoreID,mysql.ErrInvalidConn)
	clock.advance(4 * time.Second)
	_, err = registry.Store(context.Background(), healthStoreID)
	assertCoolingError(t, err)
	if got := ping.count(); got != 1 {
		t.Fatalf("no probe may run inside the cooldown, got %d probes", got)
	}
}

// 到期只放一次探活:通了就放行并清掉冷却,不通就续上一个冷却期。
func TestCooldownExpiry_SingleProbeDecides(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	ping := &fakePing{}
	ping.set(errDialRefused, errDialRefused, nil)
	registry := newHealthTestRegistry(t, clock, ping)

	registry.ReportConnLost(healthStoreID,mysql.ErrInvalidConn) // 第 1 次探活:不通,冷却到 +5s
	clock.advance(6 * time.Second)
	_, err := registry.Store(context.Background(), healthStoreID) // 第 2 次探活:仍不通,续到 +11s
	assertCoolingError(t, err)
	if !registry.CoolingDown(healthStoreID) {
		t.Fatal("a failed expiry probe must re-arm the cooldown")
	}

	clock.advance(6 * time.Second)
	store, err := registry.Store(context.Background(), healthStoreID) // 第 3 次探活:通了
	if err != nil || store == nil {
		t.Fatalf("a successful expiry probe must hand the store back, got store=%v err=%v", store, err)
	}
	if registry.CoolingDown(healthStoreID) {
		t.Fatal("a successful probe must clear the cooldown")
	}
	if got := ping.count(); got != 3 {
		t.Fatalf("want 3 probes in total, got %d", got)
	}
}

// 冷却期内任何一条语句成功都说明库是通的,立即解除。
func TestReportHealthy_ClearsCooldown(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	ping := &fakePing{}
	ping.set(errDialRefused)
	registry := newHealthTestRegistry(t, clock, ping)

	registry.ReportConnLost(healthStoreID,mysql.ErrInvalidConn)
	registry.ReportHealthy(healthStoreID)

	if registry.CoolingDown(healthStoreID) {
		t.Fatal("a successful statement must clear the cooldown")
	}
	if _, err := registry.Store(context.Background(), healthStoreID); err != nil {
		t.Fatalf("store must be available again: %v", err)
	}
}

// 还没打开过的库不探活:打开路径自己有失败冷却。
func TestReportConnLost_UnopenedStoreIsIgnored(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	ping := &fakePing{}
	registry := newHealthTestRegistry(t, clock, ping)

	registry.ReportConnLost(1000102, errDialRefused)

	if got := ping.count(); got != 0 {
		t.Fatalf("an unopened store must not be probed, got %d probes", got)
	}
	if registry.CoolingDown(1000102) {
		t.Fatal("an unopened store must not enter the runtime cooldown")
	}
}

// 冷却库个数在抓取时现算,只数没到期的:到期后再也没有流量的库不能让指标与告警一直挂着(复核意见 3)。
func TestCooldownCounts_OnlyCountsUnexpiredCooldowns(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	ping := &fakePing{}
	ping.set(errDialRefused)
	registry := newHealthTestRegistry(t, clock, ping)

	registry.ReportConnLost(healthStoreID,mysql.ErrInvalidConn)
	if got := registry.CooldownCounts()[metrics.PoolStore]; got != 1 {
		t.Fatalf("store pool cooling count = %d, want 1", got)
	}
	clock.advance(6 * time.Second)
	if got := registry.CooldownCounts()[metrics.PoolStore]; got != 0 {
		t.Fatalf("an expired cooldown must not be counted, got %d", got)
	}
}

// 到期时并发进来的放行请求只探活一次:前一个探活刚续上冷却,后来者不再探活(复核意见 2)。
func TestProbeAtExpiry_SkipsWhenAnotherProbeJustReArmed(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	ping := &fakePing{}
	ping.set(errDialRefused)
	registry := newHealthTestRegistry(t, clock, ping)

	registry.ReportConnLost(healthStoreID,mysql.ErrInvalidConn) // 第 1 次探活,冷却到 +5s
	clock.advance(6 * time.Second)
	expired := registry.cooling[healthStoreID]                    // 模拟一个调用方在锁外读到了已到期的记录
	_, err := registry.Store(context.Background(), healthStoreID) // 第 2 次探活:仍不通,续到 +11s
	assertCoolingError(t, err)

	// 那个调用方拿着旧记录进来,必须看到冷却已被续上而不再探活。
	_, err = registry.storeAfterCooldown(healthStoreID, storeNamed("player_store_1000000_db"), expired)
	assertCoolingError(t, err)
	if got := ping.count(); got != 2 {
		t.Fatalf("a late caller must not probe again, got %d probes", got)
	}
}
