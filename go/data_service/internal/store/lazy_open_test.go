package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shared/authoritysql"
	"shared/placement"

	"github.com/stretchr/testify/require"
)

// 按需打开的单测(docs/design/data-service-startup-isolation.md §9),不连真库。

// openedForTest 是已经打开、常驻的 openOnce:get 直接回 value,不再调 open。
func openedForTest[V any](value V) *openOnce[V] {
	once := newOpenOnce[V]("test", time.Now)
	once.opened, once.value = true, value
	return once
}

// failedOpenForTest 是刚打开失败、处在冷却期的 openOnce:冷却期内 get 回 err,不调 open。
func failedOpenForTest[V any](err error) *openOnce[V] {
	once := newOpenOnce[V]("test", time.Now)
	once.failedAt, once.failure = time.Now(), err
	return once
}

func TestOpenOnceKeepsTheFirstSuccess(t *testing.T) {
	once := newOpenOnce[int]("test", time.Now)
	opens := 0
	for i := 0; i < 3; i++ {
		v, err := once.get(func() (int, error) { opens++; return 7, nil })
		require.NoError(t, err)
		require.Equal(t, 7, v)
	}
	require.Equal(t, 1, opens, "成功一次之后常驻,不再打开")
}

func TestOpenOnceBacksOffAfterAFailure(t *testing.T) {
	clock := time.Unix(1_800_000_000, 0)
	once := newOpenOnce[int]("test", func() time.Time { return clock })
	refused := errors.New("connection refused")
	result := refused
	opens := 0
	open := func() (int, error) {
		opens++
		if result != nil {
			return 0, result
		}
		return 7, nil
	}
	_, err := once.get(open)
	require.ErrorIs(t, err, refused)

	clock = clock.Add(lazyOpenBackoff - time.Millisecond)
	result = nil
	_, err = once.get(open)
	require.ErrorIs(t, err, refused, "冷却期内回上次的错误")
	require.Equal(t, 1, opens, "冷却期内不再打开")

	clock = clock.Add(time.Millisecond)
	v, err := once.get(open)
	require.NoError(t, err)
	require.Equal(t, 7, v)
	require.Equal(t, 2, opens)
}

func TestOpenOnceSharesOneOpenAcrossConcurrentCallers(t *testing.T) {
	once := newOpenOnce[int]("test", time.Now)
	var opens atomic.Int32
	release := make(chan struct{})
	open := func() (int, error) {
		opens.Add(1)
		<-release
		return 7, nil
	}
	const callers = 8
	var wg sync.WaitGroup
	values := make([]int, callers)
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			values[i], errs[i] = once.get(open)
		}(i)
	}
	require.Eventually(t, func() bool { return opens.Load() == 1 }, time.Second, time.Millisecond)
	close(release)
	wg.Wait()
	for i := 0; i < callers; i++ {
		require.NoError(t, errs[i])
		require.Equal(t, 7, values[i])
	}
	require.Equal(t, int32(1), opens.Load(), "并发调用共用同一次打开")
}

// gatedConnector 先问建连钩子,放行了才交给 inner 建连;和 authoritysql 的连接器挂钩子的位置相同(connector.Connect 最前面)。
type gatedConnector struct {
	gate      authoritysql.ConnectGate
	inner     driver.Connector
	onConnect func()
}

func (c gatedConnector) Connect(ctx context.Context) (driver.Conn, error) {
	settle, err := c.gate(ctx, "under-test")
	if err != nil {
		return nil, err
	}
	conn, err := c.inner.Connect(ctx)
	if settle != nil {
		settle(err)
	}
	if err == nil && c.onConnect != nil {
		c.onConnect()
	}
	return conn, err
}

func (c gatedConnector) Driver() driver.Driver { return c.inner.Driver() }

// lazyShardFake 是按需模式的替身片,照 openCheckedPool 的按需分支开池:down 里的库此刻连不上;
// 连得上时核对在一个一次性的替身小池上跑,通过之后真正的池才给连接。时钟由用例推进。
type lazyShardFake struct {
	conns func(schema string) driver.Connector
	// registry 是登记这些池的名单,和常驻进程一样;nil 时不登记。
	registry *LazyPools

	mu       sync.Mutex
	now      time.Time
	down     map[string]bool
	verifies map[string]int
	connects map[string]int
}

func newLazyShardFake(conns func(schema string) driver.Connector, down ...string) *lazyShardFake {
	f := &lazyShardFake{conns: conns, now: time.Unix(1_800_000_000, 0), down: map[string]bool{}, verifies: map[string]int{}, connects: map[string]int{}}
	for _, schema := range down {
		f.down[schema] = true
	}
	return f
}

func (f *lazyShardFake) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *lazyShardFake) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func (f *lazyShardFake) setDown(schema string, down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down[schema] = down
}

// counts 回这个库核对过几次、真正的池建过几条连接。
func (f *lazyShardFake) counts(schema string) (verifies, connects int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.verifies[schema], f.connects[schema]
}

func (f *lazyShardFake) opener() shardOpener {
	return func(one MySQLConfig, _ string, check poolCheck) (*sql.DB, error) {
		schema := one.DBName
		return lazyPool(schema, f.clock,
			func() error {
				f.mu.Lock()
				f.verifies[schema]++
				down := f.down[schema]
				f.mu.Unlock()
				if down {
					return fmt.Errorf("%w: dial tcp: connection refused", authoritysql.ErrUnreachable)
				}
				probe := sql.OpenDB(f.conns(schema))
				defer probe.Close()
				return check(probe)
			},
			func(gate authoritysql.ConnectGate) (*sql.DB, error) {
				return sql.OpenDB(gatedConnector{gate: gate, inner: f.conns(schema), onConnect: func() {
					f.mu.Lock()
					defer f.mu.Unlock()
					f.connects[schema]++
				}}), nil
			}, f.registry)
	}
}

// 启动时连不上的池记在名单里:就绪判定据它不就绪;后台重核(RetryClosed)在恢复、冷却过后把它打开,不用等第一次用到,之后名单里不再有它。
func TestLazyPoolsRetryClosedPoolsInTheBackground(t *testing.T) {
	const k = 2
	const bad = "mmorpg_global_01"
	f := newLazyShardFake(func(schema string) driver.Connector {
		return fakeShardConnector{logicalShards: k, slot: testShardSlot(schema)}
	}, bad)
	f.registry = &LazyPools{}
	shards, err := openShardPoolsWith(shardCfgForTest(k), noShapeCheck, f.opener())
	require.NoError(t, err)
	t.Cleanup(func() { _ = shards.Close() })
	require.Equal(t, []string{bad}, f.registry.Closed())

	f.registry.RetryClosed()
	verifies, _ := f.counts(bad)
	require.Equal(t, 1, verifies, "冷却期内后台重核不拨号")

	// 失败的时刻记在核对做完时,比那一拍晚。这里让核对做了 1 秒:下一拍离失败不到一个间隔,也要已经出了冷却、照样重核。
	f.advance(lazyOpenBackoff)
	f.registry.RetryClosed()
	f.advance(LazyPoolRetryInterval - time.Second)
	f.registry.RetryClosed()
	verifies, _ = f.counts(bad)
	require.Equal(t, 3, verifies, "每一拍都重核,不会两拍才核一次")

	f.setDown(bad, false)
	f.advance(LazyPoolRetryInterval)
	f.registry.RetryClosed()
	verifies, connects := f.counts(bad)
	require.Equal(t, 4, verifies)
	require.Zero(t, connects, "重核用一次性的小池,不占业务池的连接")
	require.Empty(t, f.registry.Closed(), "核对通过之后不再算关着")
	require.NoError(t, shards.pools[1].PingContext(context.Background()))
	verifies, _ = f.counts(bad)
	require.Equal(t, 4, verifies, "打开之后用到时不再核对")

	var none *LazyPools
	require.Empty(t, none.Closed())
	none.RetryClosed()
}

// 一片连不上:其余片照常装配、照常给连接;这一片的池关着,借连接回不可用。
// 恢复之后,冷却过了第一次用到时先核对,通过才放行;放行之后常驻,不再核对。
func TestOpenShardPoolsKeepsAnUnreachableShardClosedUntilItVerifies(t *testing.T) {
	const k = 2
	const bad = "mmorpg_global_01"
	f := newLazyShardFake(func(schema string) driver.Connector {
		return fakeShardConnector{logicalShards: k, slot: testShardSlot(schema)}
	}, bad)
	shards, err := openShardPoolsWith(shardCfgForTest(k), noShapeCheck, f.opener())
	require.NoError(t, err, "一片连不上不挡装配")
	t.Cleanup(func() { _ = shards.Close() })
	ctx := context.Background()

	require.NoError(t, shards.pools[0].PingContext(ctx), "健康的片照常给连接")
	err = shards.pools[1].PingContext(ctx)
	require.ErrorIs(t, err, authoritysql.ErrUnreachable, "关着的片借连接回不可用")
	require.ErrorContains(t, err, bad)
	verifies, connects := f.counts(bad)
	require.Equal(t, 1, verifies, "启动时试一次,冷却期内不再核对")
	require.Zero(t, connects, "核对通过之前一条连接都不给")

	f.setDown(bad, false)
	require.ErrorIs(t, shards.pools[1].PingContext(ctx), authoritysql.ErrUnreachable, "恢复了也要等冷却过去")
	f.advance(lazyOpenBackoff)
	require.NoError(t, shards.pools[1].PingContext(ctx), "核对通过才放行")
	verifies, connects = f.counts(bad)
	require.Equal(t, 2, verifies)
	require.Equal(t, 1, connects)

	// 不留空闲连接,逼每次借用都新建:放行之后新建连接不再核对。
	shards.pools[1].SetMaxIdleConns(0)
	require.NoError(t, shards.pools[1].PingContext(ctx))
	require.NoError(t, shards.pools[1].PingContext(ctx))
	verifies, connects = f.counts(bad)
	require.Equal(t, 2, verifies)
	require.Equal(t, 3, connects)
}

// 片号对不上(配错 K、连错片)永远不放行:启动时连得上就当场拒绝装配(进程拒启);
// 启动时连不上、恢复之后才发现对不上的,每次核对都不过,一条连接都不给。
func TestOpenShardPoolsNeverAdmitsAShardWithTheWrongSlot(t *testing.T) {
	const k = 2
	const bad = "mmorpg_global_01"
	// 这一片记着的是「4 片里的第 1 片」。
	conns := func(schema string) driver.Connector {
		if schema == bad {
			return fakeShardConnector{logicalShards: 4, slot: 1}
		}
		return fakeShardConnector{logicalShards: k, slot: 0}
	}

	t.Run("reachable at startup", func(t *testing.T) {
		_, err := openShardPoolsWith(shardCfgForTest(k), noShapeCheck, newLazyShardFake(conns).opener())
		require.ErrorContains(t, err, "slot 1 of 4")
		require.False(t, errors.Is(err, authoritysql.ErrUnreachable), "核对不过不能当成连不上")
	})

	t.Run("unreachable at startup, wrong after recovery", func(t *testing.T) {
		f := newLazyShardFake(conns, bad)
		shards, err := openShardPoolsWith(shardCfgForTest(k), noShapeCheck, f.opener())
		require.NoError(t, err)
		t.Cleanup(func() { _ = shards.Close() })
		f.setDown(bad, false)
		for i := 0; i < 3; i++ {
			f.advance(lazyOpenBackoff)
			require.ErrorContains(t, shards.pools[1].PingContext(context.Background()), "slot 1 of 4")
		}
		verifies, connects := f.counts(bad)
		require.Equal(t, 4, verifies, "每过一个冷却期重核一次")
		require.Zero(t, connects, "核对不过一条连接都不给")
	})
}

// 这一代没有写资格的库(写者注册时它连不上,docs/design/data-service-startup-isolation.md §7):
// 常驻进程里照常装配,这个库的池一条连接都不给,存档库照样打不开;工具和迁移照旧当场回错误。
func TestDatabaseWithoutAWriterGrantStaysClosed(t *testing.T) {
	ctx := context.Background()
	cfg := shardCfgForTest(2)
	cfg.PermissionFor = func(schema string) (authoritysql.Permission, error) {
		if schema == "mmorpg_global_00" {
			return authoritysql.Reader(), nil
		}
		return authoritysql.Permission{}, fmt.Errorf("%w: %s", ErrNoWriterGrant, schema)
	}
	_, err := cfg.ForSchema("mmorpg_global_01")
	require.ErrorIs(t, err, ErrNoWriterGrant, "工具和迁移照旧当场回错误")

	cfg.Lazy = true
	healthy, err := cfg.ForSchema("mmorpg_global_00")
	require.NoError(t, err)
	require.Nil(t, healthy.Closed)
	one, err := cfg.ForSchema("mmorpg_global_01")
	require.NoError(t, err, "常驻进程里缺写资格不算装配失败")
	require.ErrorIs(t, one.Closed, ErrNoWriterGrant)
	db, err := openCheckedPool(one, "", func(*sql.DB) error {
		t.Fatal("a closed pool is never checked")
		return nil
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	err = db.PingContext(ctx)
	require.ErrorIs(t, err, ErrNoWriterGrant)
	require.ErrorContains(t, err, "mmorpg_global_01")

	stores, err := newPlayerStorePools(cfg, authoritysql.BackendMySQL, []uint32{placement.FirstStorageID}, PlayerStorePoolOptions{})
	require.NoError(t, err)
	_, _, err = stores.DB(ctx, placement.FirstStorageID)
	require.ErrorIs(t, err, ErrNoWriterGrant, "没有写资格的存档库打不开")
}
