package proto_sql

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"db/internal/dbguard"
	"db/internal/dbtest"

	dbpb "proto/common/database"

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
		HomeStorageID: 1,
		Home:          storeNamed("zone_1_db"),
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

func TestAdmitStore(t *testing.T) {
	allow := dbguard.Allowlist{Names: []string{"zone_1_db", "mmorpg_custom_db"}, Source: "test"}
	cases := []struct {
		name     string
		families bool
		db       string
		wantVia  string
		wantOK   bool
	}{
		{name: "allow list wins even with families off", families: false, db: "zone_1_db", wantVia: StoreAdmittedByAllowlist, wantOK: true},
		{name: "non-family name only via allow list", families: true, db: "mmorpg_custom_db", wantVia: StoreAdmittedByAllowlist, wantOK: true},
		{name: "zone family", families: true, db: "zone_102_db", wantVia: StoreAdmittedByFamily, wantOK: true},
		{name: "player_store family", families: true, db: "player_store_1000000_db", wantVia: StoreAdmittedByFamily, wantOK: true},
		{name: "families off rejects a family name outside the allow list", families: false, db: "zone_102_db"},
		{name: "outside both families", families: true, db: "mmorpg_guild"},
		{name: "zone id outside the zone range is not a zone-family name", families: true, db: "zone_1000000_db"},
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

func TestNewStoreRegistryRejectsInconsistentSeeds(t *testing.T) {
	opener := newFakeOpener()
	cases := []struct {
		name string
		opts StoreRegistryOptions
	}{
		{name: "zone outside the zone storage range",
			opts: StoreRegistryOptions{HomeStorageID: 1000000, Home: storeNamed("zone_1000000_db"), Open: opener.open}},
		{name: "zone 0", opts: StoreRegistryOptions{HomeStorageID: 0, Home: storeNamed("zone_0_db"), Open: opener.open}},
		{name: "home not opened", opts: StoreRegistryOptions{HomeStorageID: 1, Open: opener.open}},
		{name: "home name diverges from the derived name",
			opts: StoreRegistryOptions{HomeStorageID: 1, Home: storeNamed("zone_2_db"), Open: opener.open}},
		{name: "no opener", opts: StoreRegistryOptions{HomeStorageID: 1, Home: storeNamed("zone_1_db")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewStoreRegistry(tc.opts); err == nil {
				t.Fatal("inconsistent seed must be rejected at startup")
			}
		})
	}
}

func TestStoreRegistryServesHomeWithoutOpening(t *testing.T) {
	opener := newFakeOpener()
	registry := newTestRegistry(t, opener, dbguard.Allowlist{}, true, nil)
	store, err := registry.Store(context.Background(), 1)
	if err != nil {
		t.Fatalf("home store: %v", err)
	}
	if store.SqlModel.DBName != "zone_1_db" {
		t.Fatalf("home store = %q", store.SqlModel.DBName)
	}
	if n := opener.count("zone_1_db"); n != 0 {
		t.Fatalf("the zone's own database was opened by InitDB; the registry must not open it again (opened %d times)", n)
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
			results[i], errs[i] = registry.Store(context.Background(), 102)
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
	if n := opener.count("zone_102_db"); n != 1 {
		t.Fatalf("concurrent first use must open the store exactly once, opened %d times", n)
	}
}

func TestStoreRegistryAdmission(t *testing.T) {
	t.Run("families off and not in the allow list: rejected without dialing", func(t *testing.T) {
		opener := newFakeOpener()
		registry := newTestRegistry(t, opener, dbguard.Allowlist{Names: []string{"zone_1_db"}, Source: "test"}, false, nil)
		_, err := registry.Store(context.Background(), 102)
		if !errors.Is(err, ErrStoreUnavailable) {
			t.Fatalf("err = %v, want ErrStoreUnavailable", err)
		}
		if n := opener.count("zone_102_db"); n != 0 {
			t.Fatalf("a rejected store must never be dialed, opened %d times", n)
		}
	})
	t.Run("families off but in the allow list: opened", func(t *testing.T) {
		opener := newFakeOpener()
		registry := newTestRegistry(t, opener, dbguard.Allowlist{Names: []string{"zone_1_db", "zone_102_db"}, Source: "test"}, false, nil)
		if _, err := registry.Store(context.Background(), 102); err != nil {
			t.Fatalf("allow-listed store: %v", err)
		}
	})
	t.Run("storage id 0 names no database", func(t *testing.T) {
		registry := newTestRegistry(t, newFakeOpener(), dbguard.Allowlist{}, true, nil)
		if _, err := registry.Store(context.Background(), 0); !errors.Is(err, ErrStoreUnavailable) {
			t.Fatalf("err = %v, want ErrStoreUnavailable", err)
		}
	})
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

func TestStoreRegistryRejectsAnOpenerThatLandsOnAnotherDatabase(t *testing.T) {
	registry, err := NewStoreRegistry(StoreRegistryOptions{
		HomeStorageID: 1,
		Home:          storeNamed("zone_1_db"),
		AllowFamilies: true,
		Open: func(context.Context, string) (*GameDB, error) {
			return storeNamed("zone_7_db"), nil
		},
	})
	if err != nil {
		t.Fatalf("NewStoreRegistry: %v", err)
	}
	if _, err := registry.Store(context.Background(), 102); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("err = %v, want ErrStoreUnavailable", err)
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
	db, rec := dbtest.Open(t, placementStoreHandler("zone_102_db",
		declaredColumnRows(msg, "bigint unsigned", ""), [][]driver.Value{{table}}))

	store, err := preparePlacementStore(context.Background(), db, "zone_102_db", []proto.Message{msg})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if store.DB != db || store.SqlModel == nil || store.SqlModel.DBName != "zone_102_db" {
		t.Fatalf("store must own its pool and a model bound to zone_102_db, got %+v", store)
	}
	if store.SqlModel.GetCreateTableSQL(msg) == "" {
		t.Fatal("tables must be registered on the store's own model")
	}
	assertNoDDL(t, rec)
}

func TestPreparePlacementStoreRejectsAPoolOnAnotherDatabase(t *testing.T) {
	msg := &dbpb.PlayerSnapshot{}
	db, rec := dbtest.Open(t, placementStoreHandler("zone_999_db", nil, nil))

	_, err := preparePlacementStore(context.Background(), db, "zone_102_db", []proto.Message{msg})
	if !errors.Is(err, dbguard.ErrDatabaseNotAllowed) {
		t.Fatalf("err = %v, want ErrDatabaseNotAllowed", err)
	}
	assertNoDDL(t, rec)
}

// 缺列与启动期同一道闸;报错必须提示给迁移命令追加 -storage-id,否则照抄补救命令迁的是本 zone 库。
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
