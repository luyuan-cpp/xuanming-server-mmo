package proto_sql

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"db/internal/dbguard"
	"db/internal/dbtest"
	"db/internal/metrics"

	dbpb "proto/common/database"
	"shared/authoritysql"

	"github.com/luyuancpp/proto2mysql"
	"google.golang.org/protobuf/proto"
)

// storeNamed 造一个只带库名、没有连接的库,注册表单测只看它的身份。
func storeNamed(name string) *GameDB {
	model := proto2mysql.NewDB()
	model.DBName = name
	return &GameDB{SqlModel: model}
}

// fakeOpener 记录每个库名被真正打开的次数;err 非 nil 时打开失败,release 非 nil 时阻塞到它被关闭。
type fakeOpener struct {
	mu      sync.Mutex
	calls   map[string]int
	err     error
	release chan struct{}
}

func newFakeOpener() *fakeOpener {
	return &fakeOpener{calls: map[string]int{}}
}

func (o *fakeOpener) open(_ context.Context, name string) (*GameDB, error) {
	o.mu.Lock()
	o.calls[name]++
	err, release := o.err, o.release
	o.mu.Unlock()
	if release != nil {
		<-release
	}
	if err != nil {
		return nil, err
	}
	return storeNamed(name), nil
}

func (o *fakeOpener) setErr(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.err = err
}

func (o *fakeOpener) count(name string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.calls[name]
}

// fakeClock 是可手动推进的时钟,冷却期用例不依赖真实墙钟。
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestRegistry(t *testing.T, opener *fakeOpener, allow dbguard.Allowlist, families bool, clock *fakeClock) *StoreRegistry {
	t.Helper()
	opts := StoreRegistryOptions{
		Allow:         allow,
		AllowFamilies: families,
		Open:          opener.open,
		RetryBackoff:  10 * time.Second,
	}
	if clock != nil {
		opts.Now = clock.Now
	}
	registry, err := NewStoreRegistry(opts)
	if err != nil {
		t.Fatalf("NewStoreRegistry: %v", err)
	}
	return registry
}

// 只剩存档库一个家族(docs/design/player-data-tidb-scaling-plan.md §7.2 A17c):已退役的 zone 库名不再被家族放行。
func TestAdmitStore(t *testing.T) {
	allow := dbguard.Allowlist{Names: []string{"player_store_1001000_db", "mmorpg_custom_db"}, Source: "test"}
	cases := []struct {
		name     string
		families bool
		db       string
		wantVia  string
		wantOK   bool
	}{
		{name: "allow list wins even with families off", families: false, db: "player_store_1001000_db", wantVia: StoreAdmittedByAllowlist, wantOK: true},
		{name: "non-family name only via allow list", families: true, db: "mmorpg_custom_db", wantVia: StoreAdmittedByAllowlist, wantOK: true},
		{name: "player_store family", families: true, db: "player_store_1000000_db", wantVia: StoreAdmittedByFamily, wantOK: true},
		{name: "families off rejects a family name outside the allow list", families: false, db: "player_store_1000007_db"},
		{name: "outside the family", families: true, db: "mmorpg_guild"},
		{name: "a retired zone database is not a family name", families: true, db: "zone_102_db"},
		{name: "an id below the store range is not a family name", families: true, db: "player_store_999999_db"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			via, ok := AdmitStore(allow, tc.families, tc.db)
			if via != tc.wantVia || ok != tc.wantOK {
				t.Fatalf("AdmitStore(%q, families=%v) = (%q,%v), want (%q,%v)", tc.db, tc.families, via, ok, tc.wantVia, tc.wantOK)
			}
		})
	}
}

func TestNewStoreRegistryNeedsAnOpener(t *testing.T) {
	if _, err := NewStoreRegistry(StoreRegistryOptions{AllowFamilies: true}); err == nil {
		t.Fatal("a registry without an opener must be rejected at startup")
	}
}

// go/db 没有本区库(A17c):注册表建出来是空的,第一次被指向的库才打开。
func TestStoreRegistryStartsEmpty(t *testing.T) {
	opener := newFakeOpener()
	registry := newTestRegistry(t, opener, dbguard.Allowlist{}, true, nil)
	if samples := registry.PoolSamples(); len(samples) != 0 {
		t.Fatalf("a new registry must hold no store, got %v", samples)
	}
	if _, err := registry.Store(context.Background(), 1000000); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if n := opener.count("player_store_1000000_db"); n != 1 {
		t.Fatalf("the first use must open the store once, opened %d times", n)
	}
}

func TestStoreRegistryOpensOnDemandOnceAndKeepsIt(t *testing.T) {
	opener := newFakeOpener()
	registry := newTestRegistry(t, opener, dbguard.Allowlist{}, true, nil)
	for i := 0; i < 3; i++ {
		store, err := registry.Store(context.Background(), 1000000)
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if store.SqlModel.DBName != "player_store_1000000_db" {
			t.Fatalf("attempt %d: store = %q", i, store.SqlModel.DBName)
		}
	}
	if n := opener.count("player_store_1000000_db"); n != 1 {
		t.Fatalf("an opened store must stay resident, opened %d times", n)
	}
}

// 同一个库第一次被并发指向时只打开一次(singleflight),所有请求拿到同一个库。
func TestStoreRegistryConcurrentFirstUseOpensOnce(t *testing.T) {
	opener := newFakeOpener()
	opener.release = make(chan struct{})
	registry := newTestRegistry(t, opener, dbguard.Allowlist{}, true, nil)

	const callers = 8
	var started, done sync.WaitGroup
	results := make([]*GameDB, callers)
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		started.Add(1)
		done.Add(1)
		go func(i int) {
			defer done.Done()
			started.Done()
			results[i], errs[i] = registry.Store(context.Background(), 1000102)
		}(i)
	}
	started.Wait()
	close(opener.release)
	done.Wait()

	for i := 0; i < callers; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if results[i] != results[0] {
			t.Fatalf("caller %d got a different store instance", i)
		}
	}
	if n := opener.count("player_store_1000102_db"); n != 1 {
		t.Fatalf("concurrent first use must open the store exactly once, opened %d times", n)
	}
}

func TestStoreRegistryAdmission(t *testing.T) {
	t.Run("families off and not in the allow list: rejected without dialing", func(t *testing.T) {
		opener := newFakeOpener()
		registry := newTestRegistry(t, opener, dbguard.Allowlist{Names: []string{"player_store_1000000_db"}, Source: "test"}, false, nil)
		_, err := registry.Store(context.Background(), 1000102)
		if !errors.Is(err, ErrStoreUnavailable) {
			t.Fatalf("err = %v, want ErrStoreUnavailable", err)
		}
		if n := opener.count("player_store_1000102_db"); n != 0 {
			t.Fatalf("a rejected store must never be dialed, opened %d times", n)
		}
	})
	t.Run("families off but in the allow list: opened", func(t *testing.T) {
		opener := newFakeOpener()
		registry := newTestRegistry(t, opener, dbguard.Allowlist{Names: []string{"player_store_1000000_db", "player_store_1000102_db"}, Source: "test"}, false, nil)
		if _, err := registry.Store(context.Background(), 1000102); err != nil {
			t.Fatalf("allow-listed store: %v", err)
		}
	})
	t.Run("storage id 0 names no database", func(t *testing.T) {
		registry := newTestRegistry(t, newFakeOpener(), dbguard.Allowlist{}, true, nil)
		if _, err := registry.Store(context.Background(), 0); !errors.Is(err, ErrStoreUnavailable) {
			t.Fatalf("err = %v, want ErrStoreUnavailable", err)
		}
	})
	t.Run("a zone id names no database", func(t *testing.T) {
		opener := newFakeOpener()
		registry := newTestRegistry(t, opener, dbguard.Allowlist{}, true, nil)
		if _, err := registry.Store(context.Background(), 102); !errors.Is(err, ErrStoreUnavailable) {
			t.Fatalf("err = %v, want ErrStoreUnavailable", err)
		}
		if n := opener.count("zone_102_db"); n != 0 {
			t.Fatalf("a retired zone database must never be dialed, opened %d times", n)
		}
	})
}

// 库名清单只许列地域存档库:zone 库名在清单里就拒启,报错点名(A17c)。
func TestNewStoreRegistryFromConfigRejectsZoneDatabasesInTheAllowList(t *testing.T) {
	t.Setenv(dbguard.AllowedDatabasesEnv, "player_store_1000000_db,zone_1_db")
	_, err := NewStoreRegistryFromConfig()
	if err == nil || !strings.Contains(err.Error(), `"zone_1_db"`) || !strings.Contains(err.Error(), "zone databases are retired") {
		t.Fatalf("a zone database in the allow list must be refused by name, got %v", err)
	}
}

// 写资格同理:boot 授予的每个库都必须是地域存档库,否则拒启,boot 不装配。
func TestInitDBWithWritersRejectsNonStoreQualifications(t *testing.T) {
	if boot != nil {
		t.Fatal("test precondition: no boot assembled in this process")
	}
	for name, writers := range map[string]map[string]authoritysql.WriterConfig{
		"no qualification":  {},
		"a zone database":   {"player_store_1000000_db": {}, "zone_1_db": {}},
		"a foreign schema":  {"mmorpg_guild": {}},
		"below store range": {"player_store_999999_db": {}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := InitDBWithWriters(writers); err == nil {
				t.Fatalf("InitDBWithWriters(%v) must be rejected", writers)
			}
			if boot != nil {
				t.Fatal("a rejected boot must not be assembled")
			}
		})
	}
}

// 打开失败:返回 ErrStoreUnavailable;冷却期内不再拨号,直接复用上次的错误;冷却期过后再试,成功后常驻。
func TestStoreRegistryOpenFailureBacksOffThenRetries(t *testing.T) {
	opener := newFakeOpener()
	boom := errors.New("dial tcp: connection refused")
	opener.setErr(boom)
	clock := &fakeClock{now: time.Unix(1_000_000, 0)}
	registry := newTestRegistry(t, opener, dbguard.Allowlist{}, true, clock)
	ctx := context.Background()

	_, err := registry.Store(ctx, 1000000)
	if !errors.Is(err, ErrStoreUnavailable) || !errors.Is(err, boom) {
		t.Fatalf("err = %v, want ErrStoreUnavailable wrapping the open error", err)
	}
	clock.advance(5 * time.Second)
	if _, err := registry.Store(ctx, 1000000); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("within the backoff err = %v, want the cached failure", err)
	}
	if n := opener.count("player_store_1000000_db"); n != 1 {
		t.Fatalf("within the backoff the store must not be dialed again, opened %d times", n)
	}

	opener.setErr(nil)
	clock.advance(6 * time.Second)
	store, err := registry.Store(ctx, 1000000)
	if err != nil {
		t.Fatalf("after the backoff: %v", err)
	}
	if store.SqlModel.DBName != "player_store_1000000_db" {
		t.Fatalf("store = %q", store.SqlModel.DBName)
	}
	if n := opener.count("player_store_1000000_db"); n != 2 {
		t.Fatalf("after the backoff exactly one more attempt is expected, opened %d times", n)
	}
	clock.advance(time.Hour)
	if _, err := registry.Store(ctx, 1000000); err != nil {
		t.Fatalf("an opened store stays resident: %v", err)
	}
	if n := opener.count("player_store_1000000_db"); n != 2 {
		t.Fatalf("a resident store must not be reopened, opened %d times", n)
	}
}

// 闸门冻结是搬库窗口,秒级,库没有坏:不进冷却,窗口一过下一条任务就能打开。错误链要分得清冻结。
func TestStoreRegistryFrozenGateDoesNotBackOff(t *testing.T) {
	opener := newFakeOpener()
	opener.setErr(fmt.Errorf("read the schema: %w", authoritysql.ErrWriterFrozen))
	clock := &fakeClock{now: time.Unix(1_000_000, 0)}
	registry := newTestRegistry(t, opener, dbguard.Allowlist{}, true, clock)
	ctx := context.Background()
	for attempt := 1; attempt <= 2; attempt++ {
		_, err := registry.Store(ctx, 1000000)
		if !errors.Is(err, ErrStoreUnavailable) || !errors.Is(err, authoritysql.ErrWriterFrozen) {
			t.Fatalf("attempt %d: err = %v", attempt, err)
		}
		if n := opener.count("player_store_1000000_db"); n != attempt {
			t.Fatalf("a frozen gate must not back off: opened %d times after %d attempts", n, attempt)
		}
	}
	opener.setErr(nil)
	store, err := registry.Store(ctx, 1000000)
	if err != nil {
		t.Fatalf("after the window: %v", err)
	}
	if store.SqlModel.DBName != "player_store_1000000_db" {
		t.Fatalf("store = %q", store.SqlModel.DBName)
	}
}

func TestStoreRegistryRejectsAnOpenerThatLandsOnAnotherDatabase(t *testing.T) {
	registry, err := NewStoreRegistry(StoreRegistryOptions{
		AllowFamilies: true,
		Open: func(context.Context, string) (*GameDB, error) {
			return storeNamed("player_store_1000007_db"), nil
		},
	})
	if err != nil {
		t.Fatalf("NewStoreRegistry: %v", err)
	}
	if _, err := registry.Store(context.Background(), 1000102); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("err = %v, want ErrStoreUnavailable", err)
	}
}

// ---------------------------------------------------------------------------
// 连接池:指标快照(G2b)
// ---------------------------------------------------------------------------

// storeWithPool 造一个带真连接池句柄的库(dbtest 假驱动;Stats() 不下发语句,nil handler 不会被调用)。
func storeWithPool(t *testing.T, name string, maxOpen int) *GameDB {
	t.Helper()
	handle, _ := dbtest.Open(t, nil)
	handle.SetMaxOpenConns(maxOpen)
	store := storeNamed(name)
	store.DB = handle
	return store
}

// PoolSamples 给每个已打开的存档库报一个池,类别只有 store;没有连接的库跳过。
func TestStoreRegistryPoolSamplesReportsEveryStorePool(t *testing.T) {
	opened := map[string]*GameDB{
		"player_store_1000000_db": storeWithPool(t, "player_store_1000000_db", 8),
		"player_store_1000001_db": storeWithPool(t, "player_store_1000001_db", 16),
		"player_store_1000002_db": storeNamed("player_store_1000002_db"), // 无连接:PoolSamples 跳过
	}
	registry, err := NewStoreRegistry(StoreRegistryOptions{
		AllowFamilies: true,
		Open: func(_ context.Context, name string) (*GameDB, error) {
			store, ok := opened[name]
			if !ok {
				return nil, errors.New("unexpected open of " + name)
			}
			return store, nil
		},
	})
	if err != nil {
		t.Fatalf("NewStoreRegistry: %v", err)
	}
	for _, id := range []uint32{1000000, 1000001, 1000002} {
		if _, err := registry.Store(context.Background(), id); err != nil {
			t.Fatalf("open storage %d: %v", id, err)
		}
	}

	samples := registry.PoolSamples()
	var maxOpen []int
	for _, s := range samples {
		if s.Pool != metrics.PoolStore {
			t.Fatalf("pool class = %q, want %q (the only class left)", s.Pool, metrics.PoolStore)
		}
		maxOpen = append(maxOpen, s.Stats.MaxOpenConnections)
	}
	sort.Ints(maxOpen)
	if len(maxOpen) != 2 || maxOpen[0] != 8 || maxOpen[1] != 16 {
		t.Fatalf("pool max open = %v, want [8 16] (the connection-less store must be skipped)", maxOpen)
	}
}

func TestApplyStorePoolSetsMaxOpen(t *testing.T) {
	handle, _ := dbtest.Open(t, nil)
	applyStorePool(handle, StorePoolOptions{MaxOpenConn: 7, MaxIdleConn: 2, ConnMaxIdleTime: time.Minute})
	if got := handle.Stats().MaxOpenConnections; got != 7 {
		t.Fatalf("MaxOpenConnections = %d, want 7", got)
	}
}

// ---------------------------------------------------------------------------
// preparePlacementStore:按需打开的只读核对(dbtest 假驱动)
// ---------------------------------------------------------------------------

// placementStoreHandler 在 schemaGateHandler 之上回答 SELECT DATABASE() 与 @@lower_case_table_names。
// 后者只有 proto2mysql main(83fed85 起)的 OpenDB 会查(它不再 USE,改为校验 DSN 的库);
// f3b308f 版的 OpenDB 只下发一条 USE。两种实现本用例都要能跑。
func placementStoreHandler(connected string, columns, primaries [][]driver.Value) dbtest.Handler {
	gate := schemaGateHandler(columns, primaries, 1, nil)
	return func(ctx context.Context, query string, args []driver.NamedValue) (*dbtest.Rows, error) {
		switch {
		case strings.Contains(query, "SELECT DATABASE()"):
			return &dbtest.Rows{Columns: []string{"DATABASE()"}, Values: [][]driver.Value{{connected}}}, nil
		case strings.Contains(query, "@@lower_case_table_names"):
			return &dbtest.Rows{Columns: []string{"@@lower_case_table_names"}, Values: [][]driver.Value{{int64(0)}}}, nil
		}
		return gate(ctx, query, args)
	}
}

// assertNoDDL 钉住「按需打开从不建库、不跑 DDL」:只允许 SELECT 与最后选库的 USE。
func assertNoDDL(t *testing.T, rec *dbtest.Recorder) {
	t.Helper()
	for _, s := range rec.Statements() {
		upper := strings.ToUpper(strings.TrimSpace(s))
		if !strings.HasPrefix(upper, "SELECT") && !strings.HasPrefix(upper, "USE ") {
			t.Fatalf("on-demand open must stay read-only, but issued: %s", s)
		}
	}
}

func TestPreparePlacementStoreOpensReadOnly(t *testing.T) {
	msg := &dbpb.PlayerSnapshot{}
	table := proto2mysql.GetTableName(msg)
	db, rec := dbtest.Open(t, placementStoreHandler("player_store_1000102_db",
		declaredColumnRows(msg, "bigint unsigned", ""), [][]driver.Value{{table}}))

	store, err := preparePlacementStore(context.Background(), db, "player_store_1000102_db", []proto.Message{msg})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if store.DB != db || store.SqlModel == nil || store.SqlModel.DBName != "player_store_1000102_db" {
		t.Fatalf("store must own its pool and a model bound to player_store_1000102_db, got %+v", store)
	}
	if store.SqlModel.GetCreateTableSQL(msg) == "" {
		t.Fatal("tables must be registered on the store's own model")
	}
	assertNoDDL(t, rec)
}

func TestPreparePlacementStoreRejectsAPoolOnAnotherDatabase(t *testing.T) {
	msg := &dbpb.PlayerSnapshot{}
	db, rec := dbtest.Open(t, placementStoreHandler("player_store_1000999_db", nil, nil))

	_, err := preparePlacementStore(context.Background(), db, "player_store_1000102_db", []proto.Message{msg})
	if !errors.Is(err, dbguard.ErrDatabaseNotAllowed) {
		t.Fatalf("err = %v, want ErrDatabaseNotAllowed", err)
	}
	assertNoDDL(t, rec)
}

// 缺列时拒绝打开;报错必须点名这个库的 -storage-id,照抄补救命令迁的就是这个库。
func TestPreparePlacementStoreRejectsMissingColumnWithStorageHint(t *testing.T) {
	msg := &dbpb.PlayerSnapshot{}
	table := proto2mysql.GetTableName(msg)
	db, rec := dbtest.Open(t, placementStoreHandler("player_store_1000000_db",
		declaredColumnRows(msg, "bigint unsigned", "data"), [][]driver.Value{{table}}))

	_, err := preparePlacementStore(context.Background(), db, "player_store_1000000_db", []proto.Message{msg})
	if err == nil {
		t.Fatal("a store missing a declared column must not be opened")
	}
	for _, want := range []string{table + ".data", "-storage-id 1000000"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error must mention %q, got: %v", want, err)
		}
	}
	assertNoDDL(t, rec)
}
