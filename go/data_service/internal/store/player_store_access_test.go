package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/syncx"
	"google.golang.org/protobuf/proto"

	componentpb "proto/common/component"
	databasepb "proto/common/database"
)

// fakeStoreConnector 回答存档库的读。一个落点配一个,配什么回什么。
type fakeStoreConnector struct {
	cols     []string
	rows     [][]driver.Value
	queryErr error
	rowsErr  error
}

func (c fakeStoreConnector) Connect(context.Context) (driver.Conn, error) {
	return fakeStoreConn{c}, nil
}
func (c fakeStoreConnector) Driver() driver.Driver { return fakeNameDriver{} }

type fakeStoreConn struct{ fakeStoreConnector }

func (c fakeStoreConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("不支持 Prepare")
}
func (c fakeStoreConn) Close() error              { return nil }
func (c fakeStoreConn) Begin() (driver.Tx, error) { return nil, errors.New("不支持事务") }
func (c fakeStoreConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	if c.queryErr != nil {
		return nil, c.queryErr
	}
	return &fakeNameRows{cols: c.cols, rows: append([][]driver.Value(nil), c.rows...), rowsErr: c.rowsErr}, nil
}

// fakeStorePools 给每个落点配好连接池,不走打开。openFailed 里的落点打开失败,处在冷却期。
func fakeStorePools(t *testing.T, connectors map[uint32]fakeStoreConnector, openFailed map[uint32]error) *PlayerStorePools {
	t.Helper()
	pools := &PlayerStorePools{allowed: map[uint32]bool{}, now: time.Now, flight: syncx.NewSingleFlight(), pools: map[uint32]*sql.DB{}, failures: map[uint32]playerStoreOpenFailure{}}
	for storage, connector := range connectors {
		db := sql.OpenDB(connector)
		t.Cleanup(func() { _ = db.Close() })
		pools.allowed[storage] = true
		pools.pools[storage] = db
	}
	for storage, err := range openFailed {
		pools.allowed[storage] = true
		pools.failures[storage] = playerStoreOpenFailure{at: pools.now(), err: err}
	}
	return pools
}

func displayRowForTest(t *testing.T, player uint64, kind int32, row *databasepb.PlayerDatabase) []driver.Value {
	t.Helper()
	payload, err := proto.Marshal(row)
	require.NoError(t, err)
	return []driver.Value{int64(player), int64(kind), payload}
}

// 规格 §6:一个落点读不了只跳过那些玩家,好友列表这类调用方只缺这个落点的人。
func TestReadDisplaysSkipsABadStorage(t *testing.T) {
	cols := []string{"player_id", "kind", "payload"}
	level := func(n uint32) *databasepb.PlayerDatabase {
		return &databasepb.PlayerDatabase{LevelComponent: &componentpb.LevelComp{Level: n}}
	}
	name := func(s string) *databasepb.PlayerDatabase {
		return &databasepb.PlayerDatabase{ProfileComponent: &componentpb.PlayerProfileComp{Name: s}}
	}
	s := &AccountDirectoryStore{stores: fakeStorePools(t, map[uint32]fakeStoreConnector{
		1: {cols: cols, rows: [][]driver.Value{displayRowForTest(t, 11, playerStoreCharacterKind, level(7)), displayRowForTest(t, 11, playerStoreProfileKind, name("好"))}},
		2: {queryErr: errors.New("storage 2 down")},
		// 读到一半断了:不能留半份资料,有等级没名字比没有更糟。
		3: {cols: cols, rows: [][]driver.Value{displayRowForTest(t, 31, playerStoreCharacterKind, level(9))}, rowsErr: errors.New("connection lost")},
	}, map[uint32]error{4: errors.New("storage 4 cannot be opened")})}

	out := s.readDisplaysByStorage(context.Background(), map[uint32][]uint64{1: {11}, 2: {21, 22}, 3: {31}, 4: {41}})
	require.Len(t, out, 1, "只剩读得到的落点")
	require.EqualValues(t, 7, out[11].GetLevel())
	require.Equal(t, "好", out[11].GetName())
}

func TestReadDisplaysAllStoragesDownIsAnEmptyAnswer(t *testing.T) {
	s := &AccountDirectoryStore{stores: fakeStorePools(t, map[uint32]fakeStoreConnector{1: {queryErr: errors.New("down")}}, nil)}
	require.Empty(t, s.readDisplaysByStorage(context.Background(), map[uint32][]uint64{1: {11}}))
	require.Empty(t, s.readDisplaysByStorage(context.Background(), map[uint32][]uint64{}))
}

// 账本读不能跳过:调用方拿它判账本有没有见过一笔资产,读不到当成没有的话会重复发放。
func TestReadPlayerStoreAssetReportsABadStorage(t *testing.T) {
	assetCols := []string{"kind", "owner_epoch", "save_seq", "released", "payload"}
	for name, connector := range map[string]fakeStoreConnector{
		"查询失败":   {queryErr: errFlow},
		"读到一半断了": {cols: assetCols, rows: [][]driver.Value{{int64(playerStoreOwnerKind), int64(7), int64(9), int64(0), []byte{}}}, rowsErr: errFlow},
	} {
		t.Run(name, func(t *testing.T) {
			s, steps := newFlowStore(2)
			steps.routed(11)
			s.stores = fakeStorePools(t, map[uint32]fakeStoreConnector{flowStorage: connector}, nil)
			row, err := s.ReadPlayerStoreAsset(context.Background(), 11)
			require.ErrorIs(t, err, errFlow)
			require.Nil(t, row, "读不到不能当成没有")
		})
	}
	t.Run("打不开", func(t *testing.T) {
		s, steps := newFlowStore(2)
		steps.routed(11)
		s.stores = fakeStorePools(t, nil, map[uint32]error{flowStorage: errFlow})
		row, err := s.ReadPlayerStoreAsset(context.Background(), 11)
		require.ErrorIs(t, err, errFlow)
		require.Nil(t, row)
	})
}

func TestReadPlayerStoreAssetReadsOwnerAndAsset(t *testing.T) {
	s, steps := newFlowStore(2)
	steps.routed(11)
	s.stores = fakeStorePools(t, map[uint32]fakeStoreConnector{flowStorage: {
		cols: []string{"kind", "owner_epoch", "save_seq", "released", "payload"},
		rows: [][]driver.Value{{int64(playerStoreOwnerKind), int64(7), int64(9), int64(1), []byte{}}},
	}}, nil)
	row, err := s.ReadPlayerStoreAsset(context.Background(), 11)
	require.NoError(t, err)
	require.EqualValues(t, 7, row.OwnerEpoch)
	require.EqualValues(t, 9, row.SaveSeq)
	require.True(t, row.Released)
	require.EqualValues(t, 11, row.Player.GetPlayerId())
}

// 物品获取封禁表在资料行(kProfile):读的是资料行那一份,同一次结果里别的行(这里摆一行钱包行)不混进来。
func TestReadPlayerStoreProfileReadsOwnerAndProfile(t *testing.T) {
	payloadOf := func(ids ...uint32) []byte {
		payload, err := proto.Marshal(&databasepb.PlayerDatabase{ItemGainBlock: &componentpb.PlayerItemGainBlockComp{ItemConfigIds: ids}})
		require.NoError(t, err)
		return payload
	}
	s, steps := newFlowStore(2)
	steps.routed(11)
	s.stores = fakeStorePools(t, map[uint32]fakeStoreConnector{flowStorage: {
		cols: []string{"kind", "owner_epoch", "save_seq", "released", "payload"},
		rows: [][]driver.Value{
			{int64(playerStoreOwnerKind), int64(7), int64(9), int64(0), []byte{}},
			{int64(playerStoreWalletKind), int64(0), int64(0), int64(0), payloadOf(99)},
			{int64(playerStoreProfileKind), int64(0), int64(0), int64(0), payloadOf(30045, 30046)},
		},
	}}, nil)
	row, err := s.ReadPlayerStoreProfile(context.Background(), 11)
	require.NoError(t, err)
	require.EqualValues(t, 11, row.Player.GetPlayerId())
	require.Equal(t, []uint32{30045, 30046}, row.Player.GetItemGainBlock().GetItemConfigIds())
}

// 没有写权行(角色没建完)是「没有存档」,不是「存档在、没封过」;资料行不在是后者。
func TestReadPlayerStoreProfileOwnerDecidesPresence(t *testing.T) {
	cols := []string{"kind", "owner_epoch", "save_seq", "released", "payload"}
	t.Run("只有资料行", func(t *testing.T) {
		s, steps := newFlowStore(2)
		steps.routed(11)
		s.stores = fakeStorePools(t, map[uint32]fakeStoreConnector{flowStorage: {
			cols: cols, rows: [][]driver.Value{{int64(playerStoreProfileKind), int64(0), int64(0), int64(0), []byte{}}},
		}}, nil)
		row, err := s.ReadPlayerStoreProfile(context.Background(), 11)
		require.NoError(t, err)
		require.Nil(t, row)
	})
	t.Run("只有写权行", func(t *testing.T) {
		s, steps := newFlowStore(2)
		steps.routed(11)
		s.stores = fakeStorePools(t, map[uint32]fakeStoreConnector{flowStorage: {
			cols: cols, rows: [][]driver.Value{{int64(playerStoreOwnerKind), int64(7), int64(9), int64(1), []byte{}}},
		}}, nil)
		row, err := s.ReadPlayerStoreProfile(context.Background(), 11)
		require.NoError(t, err)
		require.NotNil(t, row)
		require.EqualValues(t, 11, row.Player.GetPlayerId())
		require.Empty(t, row.Player.GetItemGainBlock().GetItemConfigIds())
	})
	t.Run("没有路由", func(t *testing.T) {
		s, _ := newFlowStore(2)
		row, err := s.ReadPlayerStoreProfile(context.Background(), 11)
		require.NoError(t, err)
		require.Nil(t, row)
	})
}

// 读不成不能当成没有:运维会把「没有」读成「没封」。
func TestReadPlayerStoreProfileReportsABadStorage(t *testing.T) {
	cols := []string{"kind", "owner_epoch", "save_seq", "released", "payload"}
	for name, connector := range map[string]fakeStoreConnector{
		"查询失败":   {queryErr: errFlow},
		"读到一半断了": {cols: cols, rows: [][]driver.Value{{int64(playerStoreOwnerKind), int64(7), int64(9), int64(0), []byte{}}}, rowsErr: errFlow},
	} {
		t.Run(name, func(t *testing.T) {
			s, steps := newFlowStore(2)
			steps.routed(11)
			s.stores = fakeStorePools(t, map[uint32]fakeStoreConnector{flowStorage: connector}, nil)
			row, err := s.ReadPlayerStoreProfile(context.Background(), 11)
			require.ErrorIs(t, err, errFlow)
			require.Nil(t, row)
		})
	}
	t.Run("资料行解析失败", func(t *testing.T) {
		s, steps := newFlowStore(2)
		steps.routed(11)
		s.stores = fakeStorePools(t, map[uint32]fakeStoreConnector{flowStorage: {cols: cols, rows: [][]driver.Value{
			{int64(playerStoreOwnerKind), int64(7), int64(9), int64(0), []byte{}},
			{int64(playerStoreProfileKind), int64(0), int64(0), int64(0), []byte{0xff, 0xff, 0xff}},
		}}}, nil)
		row, err := s.ReadPlayerStoreProfile(context.Background(), 11)
		require.Error(t, err)
		require.Nil(t, row)
	})
}

// 账本和展示资料读和登录共用账号目录池,不能把连接占满。
func TestPlayerStoreReadLimitLeavesConnectionsForLogin(t *testing.T) {
	for maxOpenConn, want := range map[int]int{1: 1, 2: 1, 8: 4, 128: 64} {
		require.Equal(t, want, playerStoreReadLimit(maxOpenConn), "max_open_conn=%d", maxOpenConn)
	}
}

// 名额用完时等到调用方的截止时间就失败,不会一直等。
func TestAcquirePlayerStoreReadHonorsTheCallerDeadline(t *testing.T) {
	s := &AccountDirectoryStore{playerStoreReads: make(chan struct{}, 1)}
	release, err := s.acquirePlayerStoreRead(context.Background())
	require.NoError(t, err)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = s.acquirePlayerStoreRead(canceled)
	require.ErrorIs(t, err, context.Canceled)

	release()
	release, err = s.acquirePlayerStoreRead(context.Background())
	require.NoError(t, err)
	release()
}
