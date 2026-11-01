package authoritysql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-sql-driver/mysql"
)

const (
	placementAddrA = "10.0.0.1:4000"
	placementAddrB = "10.0.0.2:4000"
	placementQuery = "SELECT 1"
)

// 所有用例都不连真库:放置解析器和拨号都是替身,连接池是真的 database/sql。
type fakeResolver struct {
	mu      sync.Mutex
	addr    string
	version uint64
	err     error
}

func (r *fakeResolver) ResolvePlacement(string) (string, uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.addr, r.version, r.err
}

func (r *fakeResolver) set(addr string, version uint64, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addr, r.version, r.err = addr, version, err
}

// fakeDial 记下每次拨号的地址,每次给一条新连接。
type fakeDial struct {
	addrs []string
	conns []*backendFakeConn
}

func (d *fakeDial) newInner(cfg *mysql.Config) (driver.Connector, error) {
	d.addrs = append(d.addrs, cfg.Addr)
	server := healthyMySQL()
	conn := &backendFakeConn{answer: func(query string) ([]string, []driver.Value, error) {
		if query == placementQuery {
			return fakeColumns(1), []driver.Value{[]byte("1")}, nil
		}
		return server.answer(query)
	}}
	d.conns = append(d.conns, conn)
	return &backendFakeConnector{conn: conn}, nil
}

func placedDB(t *testing.T, resolver PlacementResolver, dial *fakeDial) *sql.DB {
	t.Helper()
	cfg := mysql.NewConfig()
	cfg.DBName = "fixture"
	cfg.Addr = "dsn-address-is-not-used:1"
	db := sql.OpenDB(connector{backend: BackendMySQL, keepLockWait: true, permission: Reader(), placed: &placedDialer{
		template: cfg, logicalDB: cfg.DBName, resolver: resolver, newInner: dial.newInner,
	}})
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func queryOne(t *testing.T, db *sql.DB) {
	t.Helper()
	var one string
	if err := db.QueryRowContext(context.Background(), placementQuery).Scan(&one); err != nil {
		t.Fatal(err)
	}
}

func TestPlacedPoolDialsCurrentClusterAndKeepsConnection(t *testing.T) {
	resolver := &fakeResolver{addr: placementAddrA}
	dial := &fakeDial{}
	db := placedDB(t, resolver, dial)
	queryOne(t, db)
	queryOne(t, db)
	if len(dial.addrs) != 1 || dial.addrs[0] != placementAddrA {
		t.Fatalf("版本不变时连接要复用,且按映射的地址拨号,不用 DSN 的: %v", dial.addrs)
	}
}

// 空闲时翻转:取连接时 ResetSession 回 ErrBadConn,连接被关,重试落到新集群。业务拿到的还是同一个 *sql.DB。
func TestPlacedPoolDropsIdleConnectionAfterFlip(t *testing.T) {
	resolver := &fakeResolver{addr: placementAddrA}
	dial := &fakeDial{}
	db := placedDB(t, resolver, dial)
	queryOne(t, db)
	resolver.set(placementAddrB, 1, nil)
	queryOne(t, db)
	if len(dial.addrs) != 2 || dial.addrs[1] != placementAddrB {
		t.Fatalf("翻转之后要拨新集群: %v", dial.addrs)
	}
	if !dial.conns[0].closed || dial.conns[1].closed {
		t.Fatalf("旧连接要关,新连接留着: old=%v new=%v", dial.conns[0].closed, dial.conns[1].closed)
	}
	queryOne(t, db)
	if len(dial.addrs) != 2 {
		t.Fatalf("新连接记的是新版本,不该再换: %v", dial.addrs)
	}
}

// 持有连接期间翻转:这条连接上的语句继续走旧集群(写路径由旧集群的闸门拒绝),归还时不进连接池。
func TestPlacedPoolDiscardsConnectionReturnedAfterFlip(t *testing.T) {
	resolver := &fakeResolver{addr: placementAddrA}
	dial := &fakeDial{}
	db := placedDB(t, resolver, dial)
	ctx := context.Background()
	held, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	resolver.set(placementAddrB, 1, nil)
	var one string
	if err := held.QueryRowContext(ctx, placementQuery).Scan(&one); err != nil {
		t.Fatal(err)
	}
	if len(dial.addrs) != 1 {
		t.Fatalf("持有中的连接不换集群: %v", dial.addrs)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if !dial.conns[0].closed {
		t.Fatal("过期的连接不能回到连接池")
	}
	queryOne(t, db)
	if len(dial.addrs) != 2 || dial.addrs[1] != placementAddrB {
		t.Fatalf("下一条连接落到新集群: %v", dial.addrs)
	}
}

// 映射突然读不懂:建连失败,已有连接按过期丢弃,不留在旧集群上。
func TestPlacedPoolResolverErrorFailsClosed(t *testing.T) {
	resolver := &fakeResolver{addr: placementAddrA}
	dial := &fakeDial{}
	db := placedDB(t, resolver, dial)
	queryOne(t, db)
	resolver.set("", 0, errors.New("mapping is broken"))
	var one string
	err := db.QueryRowContext(context.Background(), placementQuery).Scan(&one)
	if err == nil || !strings.Contains(err.Error(), "落点未知") {
		t.Fatalf("落点未知时不能建连: %v", err)
	}
	if !dial.conns[0].closed {
		t.Fatal("落点未知时旧连接要丢弃")
	}
	if len(dial.addrs) != 1 {
		t.Fatalf("落点未知时不该拨号: %v", dial.addrs)
	}
}

// 没配放置解析器时行为不变:连接固定连 inner,连接不带过期判定。
func TestNoResolverKeepsFixedConnection(t *testing.T) {
	conn := &backendFakeConn{answer: func(query string) ([]string, []driver.Value, error) {
		if query == placementQuery {
			return fakeColumns(1), []driver.Value{[]byte("1")}, nil
		}
		return healthyMySQL().answer(query)
	}}
	inner := &backendFakeConnector{conn: conn}
	guarded, err := PinCurrentSession(inner, BackendMySQL, Reader())
	if err != nil {
		t.Fatal(err)
	}
	if guarded.(connector).placed != nil {
		t.Fatal("PinCurrentSession 不按映射拨号")
	}
	db := sql.OpenDB(guarded)
	defer db.Close()
	queryOne(t, db)
	queryOne(t, db)
	if inner.connects != 1 || conn.closed {
		t.Fatalf("连接要复用: connects=%d closed=%v", inner.connects, conn.closed)
	}
	raw, err := guarded.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if raw.(*guardedConn).stale != nil {
		t.Fatal("没有解析器的连接不该带过期判定")
	}
}

func TestPinPlacedSessionValidates(t *testing.T) {
	cfg := mysql.NewConfig()
	cfg.DBName = "fixture"
	resolver := &fakeResolver{addr: placementAddrA}
	if _, err := PinPlacedSession(nil, resolver, BackendMySQL, Reader()); err == nil {
		t.Fatal("缺模板必须拒绝")
	}
	if _, err := PinPlacedSession(cfg, nil, BackendMySQL, Reader()); err == nil {
		t.Fatal("缺解析器必须拒绝")
	}
	if _, err := PinPlacedSession(mysql.NewConfig(), resolver, BackendMySQL, Reader()); err == nil {
		t.Fatal("模板没有库名必须拒绝")
	}
	if _, err := PinPlacedSession(cfg, resolver, Backend("oracle"), Reader()); err == nil {
		t.Fatal("非法档位必须拒绝")
	}
	if _, err := PinPlacedSession(cfg, resolver, BackendMySQL, Permission{}); err == nil {
		t.Fatal("零权限必须拒绝")
	}
	guarded, err := PinPlacedSession(cfg, resolver, BackendMySQL, Reader())
	if err != nil {
		t.Fatal(err)
	}
	if placed := guarded.(connector).placed; placed == nil || placed.logicalDB != "fixture" {
		t.Fatalf("逻辑库取模板的库名: %+v", placed)
	}
}

// 冻结 → 翻转的完整口径:冻结后写事务回 ErrWriterFrozen(连接还在旧集群、版本没变);
// 映射翻转之后,调用方重试拿到新集群的连接,写成功。旧连接被关掉,*sql.DB 句柄不变。
func TestFrozenOldClusterThenRetryLandsOnNewCluster(t *testing.T) {
	oldWire, newWire := &writerFakeWire{}, &writerFakeWire{}
	oldGate := &writerFakeGate{wire: oldWire}
	var version atomic.Uint64
	db := sql.OpenDB(flippingConnector{connect: func(ctx context.Context) (driver.Conn, error) {
		dialed := version.Load()
		wire, gate := oldWire, oldGate
		if dialed != 0 {
			wire, gate = newWire, &writerFakeGate{wire: newWire}
		}
		guarded, err := newGuardedConnector(&writerFakeConnector{wire: wire}, gate, writerFakeSession{wire: wire}, mysqlStatementPolicy{}, writerUnitPermission())
		if err != nil {
			return nil, err
		}
		guarded.stale = func() bool { return version.Load() != dialed }
		return guarded.Connect(ctx)
	}})
	defer db.Close()
	ctx := context.Background()
	update := func() error {
		_, err := db.ExecContext(ctx, "UPDATE t SET n=n+1")
		return err
	}
	if err := update(); err != nil {
		t.Fatal(err)
	}
	oldGate.beforeErr = ErrWriterFrozen
	observed, frozen := ObserveWriterFrozen(ctx)
	if _, err := db.ExecContext(observed, "UPDATE t SET n=n+1"); !errors.Is(err, ErrWriterFrozen) || !frozen() {
		t.Fatalf("冻结窗口里要回可重试的 ErrWriterFrozen 并置观察位: %v", err)
	}
	if oldWire.closes != 0 {
		t.Fatal("版本没变时连接留着,解冻回滚之后接着用")
	}
	version.Store(1)
	if err := update(); err != nil {
		t.Fatalf("翻转之后重试要落到新集群: %v", err)
	}
	if oldWire.closes != 1 || newWire.execs != 1 || newWire.closes != 0 {
		t.Fatalf("旧连接要关,写要落到新集群: oldCloses=%d newExecs=%d newCloses=%d", oldWire.closes, newWire.execs, newWire.closes)
	}
	if oldWire.execs != 1 {
		t.Fatalf("冻结之后旧集群不能再收到写: %d", oldWire.execs)
	}
}

type flippingConnector struct {
	connect func(context.Context) (driver.Conn, error)
}

func (c flippingConnector) Connect(ctx context.Context) (driver.Conn, error) { return c.connect(ctx) }
func (flippingConnector) Driver() driver.Driver                              { return opaqueDriver{} }

func TestWithPlacementSetsResolver(t *testing.T) {
	resolver := &fakeResolver{}
	var options openOptions
	WithPlacement(resolver)(&options)
	if options.placement != resolver {
		t.Fatal("选项没有生效")
	}
}
