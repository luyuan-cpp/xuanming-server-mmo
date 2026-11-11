package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	base "proto/common/base"
	"shared/accountdirectory"
	"shared/authoritysql"
	"shared/dirshard"

	"github.com/stretchr/testify/require"
)

// fakeShardConnector 只回答 directory_shard 那条读。slot 是这个库记着的片号。
type fakeShardConnector struct {
	logicalShards, slot int64
	missing             bool
	err                 error
}

func (c fakeShardConnector) Connect(context.Context) (driver.Conn, error) {
	return fakeShardConn{c}, nil
}
func (c fakeShardConnector) Driver() driver.Driver { return fakeNameDriver{} }

type fakeShardConn struct{ fakeShardConnector }

func (c fakeShardConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("不支持 Prepare")
}
func (c fakeShardConn) Close() error              { return nil }
func (c fakeShardConn) Begin() (driver.Tx, error) { return nil, errors.New("不支持事务") }
func (c fakeShardConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if !strings.Contains(query, "directory_shard") {
		return nil, fmt.Errorf("未预期的查询 %q", query)
	}
	if c.err != nil {
		return nil, c.err
	}
	rows := &fakeNameRows{cols: []string{"logical_shards", "slot"}}
	if !c.missing {
		rows.rows = [][]driver.Value{{c.logicalShards, c.slot}}
	}
	return rows, nil
}

func shardCfgForTest(k int) MySQLConfig {
	return MySQLConfig{DBName: "mmorpg_global", LogicalShards: k, Permission: authoritysql.Reader()}
}

func TestOpenShardPoolsOpensEveryShardByName(t *testing.T) {
	const k = 4
	var mu sync.Mutex
	opened := map[string]bool{}
	shards, err := openShardPools(shardCfgForTest(k), func(one MySQLConfig) (*sql.DB, error) {
		mu.Lock()
		opened[one.DBName] = true
		mu.Unlock()
		var slot int64
		_, _ = fmt.Sscanf(one.DBName, "mmorpg_global_%02d", &slot)
		return sql.OpenDB(fakeShardConnector{logicalShards: k, slot: slot}), nil
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = shards.Close() })
	require.Equal(t, k, shards.k)
	require.Equal(t, dirshard.DirectoryDBNames("mmorpg_global", k), shards.names)
	require.Len(t, opened, k)
	for _, name := range shards.names {
		require.True(t, opened[name])
	}
}

// K=1 时库名就是基名,0 按 1。
func TestOpenShardPoolsSingleShardUsesBaseName(t *testing.T) {
	for _, k := range []int{0, 1} {
		var got string
		shards, err := openShardPools(shardCfgForTest(k), func(one MySQLConfig) (*sql.DB, error) {
			got = one.DBName
			return sql.OpenDB(fakeShardConnector{logicalShards: 1}), nil
		})
		require.NoError(t, err)
		require.Equal(t, 1, shards.k)
		require.Equal(t, "mmorpg_global", got)
		require.NoError(t, shards.Close())
	}
}

// K 不是 64 的约数:一片都不开。
func TestOpenShardPoolsRejectsBadK(t *testing.T) {
	for _, k := range []int{-1, 3, 128} {
		_, err := openShardPools(shardCfgForTest(k), func(MySQLConfig) (*sql.DB, error) {
			t.Fatal("K 非法不该开池")
			return nil, nil
		})
		require.Error(t, err)
	}
}

// 每个库各用自己的写资格:权限按库取,取不到就拒绝。
func TestOpenShardPoolsUsesPermissionOfEachSchema(t *testing.T) {
	cfg := shardCfgForTest(2)
	cfg.PermissionFor = func(schema string) (authoritysql.Permission, error) {
		if schema == "mmorpg_global_01" {
			return authoritysql.Permission{}, errors.New("no grant")
		}
		return authoritysql.Reader(), nil
	}
	_, err := openShardPools(cfg, func(one MySQLConfig) (*sql.DB, error) {
		return sql.OpenDB(fakeShardConnector{logicalShards: 2}), nil
	})
	require.ErrorContains(t, err, "no grant")
}

// 配错 K 会把玩家路由到别的片,读到「角色不存在」而且没有报错,所以任一片对不上都拒绝,已开的全关掉。
func TestOpenShardPoolsVerifiesDirectoryShard(t *testing.T) {
	const k = 2
	for name, tc := range map[string]struct {
		conn fakeShardConnector
		want string
	}{
		"K不符":   {fakeShardConnector{logicalShards: 4, slot: 1}, "slot 1 of 4"},
		"片号不符":  {fakeShardConnector{logicalShards: 2, slot: 0}, "slot 0 of 2"},
		"没有这一行": {fakeShardConnector{missing: true}, "-migrate"},
		"读失败":   {fakeShardConnector{err: errFlow}, errFlow.Error()},
	} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var pools []*sql.DB
			_, err := openShardPools(shardCfgForTest(k), func(one MySQLConfig) (*sql.DB, error) {
				conn := fakeShardConnector{logicalShards: k, slot: 0}
				if one.DBName == "mmorpg_global_01" {
					conn = tc.conn
				}
				db := sql.OpenDB(conn)
				mu.Lock()
				pools = append(pools, db)
				mu.Unlock()
				return db, nil
			})
			require.ErrorContains(t, err, tc.want)
			for _, db := range pools {
				require.Error(t, db.Ping(), "失败时已开的池都要关")
			}
		})
	}
}

func TestOpenShardPoolsOpenFailureNamesTheShard(t *testing.T) {
	_, err := openShardPools(shardCfgForTest(2), func(one MySQLConfig) (*sql.DB, error) {
		if one.DBName == "mmorpg_global_01" {
			return nil, errFlow
		}
		return sql.OpenDB(fakeShardConnector{logicalShards: 2, slot: 0}), nil
	})
	require.ErrorIs(t, err, errFlow)
	require.ErrorContains(t, err, "mmorpg_global_01")
}

// fakeTxConnector 只支持事务,BEGIN 和 COMMIT 回配好的错误。
type fakeTxConnector struct{ beginErr, commitErr error }

func (c fakeTxConnector) Connect(context.Context) (driver.Conn, error) { return fakeTxConn{c}, nil }
func (c fakeTxConnector) Driver() driver.Driver                        { return fakeNameDriver{} }

type fakeTxConn struct{ fakeTxConnector }

func (c fakeTxConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("不支持 Prepare") }
func (c fakeTxConn) Close() error                        { return nil }
func (c fakeTxConn) Begin() (driver.Tx, error)           { return nil, errors.New("只走 BeginTx") }
func (c fakeTxConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	if c.beginErr != nil {
		return nil, c.beginErr
	}
	return fakeTx{commitErr: c.commitErr}, nil
}

type fakeTx struct{ commitErr error }

func (t fakeTx) Commit() error   { return t.commitErr }
func (t fakeTx) Rollback() error { return nil }

// 事务没开始和提交结局未知要分得开:建角靠它决定回 ErrNotCommitted 还是结局未知。
func TestShardTxMarksOnlyWhatNeverBegan(t *testing.T) {
	ctx := context.Background()
	noop := func(*sql.Tx) error { return nil }
	run := func(connector fakeTxConnector, action func(*sql.Tx) error) error {
		db := sql.OpenDB(connector)
		t.Cleanup(func() { _ = db.Close() })
		return shardTx(ctx, db, action)
	}

	frozen := run(fakeTxConnector{beginErr: authoritysql.ErrWriterFrozen}, noop)
	require.True(t, txNeverBegan(frozen), "BEGIN 被闸门拒绝")
	require.ErrorIs(t, frozen, authoritysql.ErrWriterFrozen, "错误链不变,拦截器和 RPC 层照常认得出")

	require.True(t, txNeverBegan(run(fakeTxConnector{beginErr: errFlow}, noop)), "BEGIN 连不上")

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	db := sql.OpenDB(fakeTxConnector{})
	t.Cleanup(func() { _ = db.Close() })
	require.True(t, txNeverBegan(shardTx(canceled, db, noop)), "ctx 已结束,BEGIN 都没发")

	require.NoError(t, run(fakeTxConnector{}, noop))
	require.False(t, txNeverBegan(run(fakeTxConnector{}, func(*sql.Tx) error { return errFlow })), "事务里的失败不标")
	commitFailed := run(fakeTxConnector{commitErr: errFlow}, noop)
	require.ErrorIs(t, commitFailed, errFlow)
	require.False(t, txNeverBegan(commitFailed), "COMMIT 失败是结局未知,绝不能标")
}

// 建角撞上冻结的闸门:BEGIN 就被拒,确定没提交。login 收到才释放名字,玩家重试不会撞上自己占住的名字。
func TestCommitRoleBeginRefusedIsNotCommitted(t *testing.T) {
	db := sql.OpenDB(fakeTxConnector{beginErr: authoritysql.ErrWriterFrozen})
	t.Cleanup(func() { _ = db.Close() })
	steps := &sqlDirectorySteps{shards: singleShardPools(db, "mmorpg_global")}
	err := steps.commitRole(context.Background(), "owner", &base.AccountSimplePlayer{PlayerId: 11, ZoneId: 1}, []byte{}, 7)
	require.ErrorIs(t, err, accountdirectory.ErrNotCommitted)
	require.ErrorIs(t, err, authoritysql.ErrWriterFrozen)
}

// fakeShardPoolsForTest 造 K 个不拨号的池。
func fakeShardPoolsForTest(t *testing.T, k int) *shardPools {
	t.Helper()
	shards := &shardPools{k: k, names: dirshard.DirectoryDBNames("base", k), pools: make([]*sql.DB, k)}
	for slot := range shards.pools {
		shards.pools[slot] = sql.OpenDB(fakeShardConnector{logicalShards: int64(k), slot: int64(slot)})
		t.Cleanup(func() { _ = shards.pools[slot].Close() })
	}
	return shards
}

func TestShardPoolsRouteKeysToTheirSlot(t *testing.T) {
	shards := fakeShardPoolsForTest(t, 4)
	require.Same(t, shards.pools[dirshard.SlotOfPlayer(4242, 4)], shards.forPlayer(4242))
	require.Same(t, shards.pools[dirshard.SlotOfAccount("seller", 4)], shards.forAccount("seller"))
	require.Same(t, shards.pools[dirshard.SlotOfName("云中君", 4)], shards.forName("云中君"))
	require.Same(t, shards.pools[0], shards.first())
}

func TestEachPlayerGroupSplitsByShardAndKeepsOrder(t *testing.T) {
	shards := fakeShardPoolsForTest(t, 4)
	slotOf := make(map[*sql.DB]int, 4)
	for slot, db := range shards.pools {
		slotOf[db] = slot
	}
	var mu sync.Mutex
	got := map[int][]uint64{}
	err := shards.eachPlayerGroup([]uint64{1, 4, 5, 8, 2}, func(db *sql.DB, players []uint64) error {
		mu.Lock()
		defer mu.Unlock()
		got[slotOf[db]] = players
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, map[int][]uint64{0: {4, 8}, 1: {1, 5}, 2: {2}}, got, "没有玩家的片不碰,组内顺序不变")
}

// 任一组失败整批报错:半份结果会被调用方当成「这些人没有」。别的组照样跑完,不留半截 goroutine。
func TestEachPlayerGroupFailsTheWholeBatch(t *testing.T) {
	shards := fakeShardPoolsForTest(t, 4)
	var ran atomic.Int32
	err := shards.eachPlayerGroup([]uint64{0, 1, 2, 3}, func(db *sql.DB, _ []uint64) error {
		ran.Add(1)
		if db == shards.pools[2] {
			return errFlow
		}
		return nil
	})
	require.ErrorIs(t, err, errFlow)
	require.EqualValues(t, 4, ran.Load())
}

func TestRunParallelBoundsConcurrency(t *testing.T) {
	var current, peak atomic.Int32
	err := runParallel(64, shardFanOut, func(int) error {
		now := current.Add(1)
		for {
			seen := peak.Load()
			if now <= seen || peak.CompareAndSwap(seen, now) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		current.Add(-1)
		return nil
	})
	require.NoError(t, err)
	require.LessOrEqual(t, peak.Load(), int32(shardFanOut))
}

func TestRunParallelJoinsErrorsAndHandlesEmpty(t *testing.T) {
	require.NoError(t, runParallel(0, shardFanOut, func(int) error {
		t.Fatal("n=0 不该调用")
		return nil
	}))
	boom := errors.New("boom")
	err := runParallel(3, shardFanOut, func(i int) error {
		if i == 0 {
			return boom
		}
		return errFlow
	})
	require.ErrorIs(t, err, boom)
	require.ErrorIs(t, err, errFlow)
	// n=1 不起协程:调用方的栈上直接跑。
	require.ErrorIs(t, runParallel(1, shardFanOut, func(int) error { return boom }), boom)
}

func TestShardPoolsStatsSumAcrossShards(t *testing.T) {
	shards := fakeShardPoolsForTest(t, 2)
	shards.pools[0].SetMaxOpenConns(3)
	shards.pools[1].SetMaxOpenConns(5)
	require.Equal(t, 8, shards.stats().MaxOpenConnections)
}

func TestShardPoolsCloseSkipsUnopened(t *testing.T) {
	shards := &shardPools{k: 2, names: []string{"a", "b"}, pools: make([]*sql.DB, 2)}
	require.NoError(t, shards.Close())
}

// 目录和流水分开:表只在该在的库里,不多不少。每个逻辑库都有自己的闸门表。
func TestLogicalDatabaseTablePlacement(t *testing.T) {
	names := func(slot int) map[string]bool {
		out := map[string]bool{}
		for _, name := range tableNamesForTest(directoryTableMessages(slot)) {
			out[name] = true
		}
		return out
	}
	first, other := names(0), names(1)
	require.True(t, first[ZoneMergedIntoTableName], "zone_merged_into 留第 0 片")
	require.False(t, other[ZoneMergedIntoTableName])
	require.True(t, first[ZoneMergeHistoryTableName], "zone_merge_history 与映射同库,留第 0 片")
	require.False(t, other[ZoneMergeHistoryTableName])
	for _, table := range []string{PlayerRouteTableName, RouteMetaTableName, PlayerNameTableName, PlayerNameOwnerTableName, AccountRoleTableName, AccountRoleRefTableName, AccountDirectoryTableName, AccountRoleTradeTableName, AccountRoleTransferTableName, AccountRoleAdmissionTableName, AccountRoleTradeHolderTableName, AccountRoleRemovalTableName, DirectoryShardTableName, "writer_slot"} {
		require.Truef(t, first[table], "第 0 片缺 %s", table)
		require.Truef(t, other[table], "别的片缺 %s", table)
	}
	ledger := map[string]bool{}
	for _, name := range tableNamesForTest(ledgerTableMessages()) {
		ledger[name] = true
	}
	for _, table := range []string{"transaction_log", PlayerSnapshotTableName, "rollback_audit_log", RouteMetaTableName, "writer_slot"} {
		require.Truef(t, ledger[table], "流水库缺 %s", table)
	}
	for _, table := range []string{"transaction_log", PlayerSnapshotTableName, "rollback_audit_log"} {
		require.Falsef(t, first[table], "%s 不该在目录", table)
	}
	for _, table := range []string{AccountRoleTableName, PlayerNameTableName, PlayerRouteTableName, DirectoryShardTableName} {
		require.Falsef(t, ledger[table], "%s 不该在流水库", table)
	}

	// TableMessages 是并集,表名和契约测试按它遍历。
	all := tableNamesForTest(TableMessages())
	sort.Strings(all)
	for i := 1; i < len(all); i++ {
		require.NotEqual(t, all[i-1], all[i], "TableMessages 不能有重复的表")
	}
	for table := range first {
		require.Contains(t, all, table)
	}
	for table := range ledger {
		require.Contains(t, all, table)
	}
}
