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
	"shared/accountdirectory"
	"shared/dirshard"
	"shared/placement"
)

// 替身存档库的编号。必须是存档库编号(>= placement.FirstStorageID):区号值域的编号 PlayerStorePools 不认(zone 存档库已退役,A17c)。
const (
	fakeStoreA = placement.FirstStorageID + 1
	fakeStoreB = placement.FirstStorageID + 2
	fakeStoreC = placement.FirstStorageID + 3
	fakeStoreD = placement.FirstStorageID + 4
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

// fakeStorePoolMaxOpen 是替身存档库池的连接上限:每个库的读名额是它的一半,即 1 个。
const fakeStorePoolMaxOpen = 2

// fakeStorePools 给每个落点配好连接池和读名额,不走打开。openFailed 里的落点打开失败,处在冷却期。
func fakeStorePools(t *testing.T, connectors map[uint32]fakeStoreConnector, openFailed map[uint32]error) *PlayerStorePools {
	t.Helper()
	pools := &PlayerStorePools{allowed: map[uint32]bool{}, now: time.Now, flight: syncx.NewSingleFlight(), reads: map[uint32]readSlots{},
		pools: map[uint32]*sql.DB{}, failures: map[uint32]playerStoreOpenFailure{}}
	for storage, connector := range connectors {
		db := sql.OpenDB(connector)
		t.Cleanup(func() { _ = db.Close() })
		pools.allowed[storage] = true
		pools.reads[storage] = newReadSlots(fakeStorePoolMaxOpen)
		pools.pools[storage] = db
	}
	for storage, err := range openFailed {
		pools.allowed[storage] = true
		pools.reads[storage] = newReadSlots(fakeStorePoolMaxOpen)
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
		fakeStoreA: {cols: cols, rows: [][]driver.Value{displayRowForTest(t, 11, playerStoreCharacterKind, level(7)), displayRowForTest(t, 11, playerStoreProfileKind, name("好"))}},
		fakeStoreB: {queryErr: errors.New("storage B down")},
		// 读到一半断了:不能留半份资料,有等级没名字比没有更糟。
		fakeStoreC: {cols: cols, rows: [][]driver.Value{displayRowForTest(t, 31, playerStoreCharacterKind, level(9))}, rowsErr: errors.New("connection lost")},
	}, map[uint32]error{fakeStoreD: errors.New("storage D cannot be opened")})}

	out := s.readDisplaysByStorage(context.Background(), map[uint32][]uint64{fakeStoreA: {11}, fakeStoreB: {21, 22}, fakeStoreC: {31}, fakeStoreD: {41}})
	require.Len(t, out, 1, "只剩读得到的落点")
	require.EqualValues(t, 7, out[11].GetLevel())
	require.Equal(t, "好", out[11].GetName())
}

func TestReadDisplaysAllStoragesDownIsAnEmptyAnswer(t *testing.T) {
	s := &AccountDirectoryStore{stores: fakeStorePools(t, map[uint32]fakeStoreConnector{fakeStoreA: {queryErr: errors.New("down")}}, nil)}
	require.Empty(t, s.readDisplaysByStorage(context.Background(), map[uint32][]uint64{fakeStoreA: {11}}))
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

// 账本和展示资料读和别的工作共用连接池(目录分片上有登录,存档库上有建角、判死),最多占一半。
func TestReadSlotLimitLeavesHalfThePool(t *testing.T) {
	for maxOpenConn, want := range map[int]int{1: 1, 2: 1, 8: 4, 128: 64} {
		require.Equal(t, want, readSlotLimit(maxOpenConn), "max_open_conn=%d", maxOpenConn)
		require.Equal(t, want, cap(newReadSlots(maxOpenConn)), "max_open_conn=%d", maxOpenConn)
	}
}

// 名额用完时等到调用方的截止时间就失败,不会一直等。
func TestReadSlotsHonorTheCallerDeadline(t *testing.T) {
	slots := newReadSlots(2)
	release, err := slots.acquire(context.Background())
	require.NoError(t, err)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = slots.acquire(canceled)
	require.ErrorIs(t, err, context.Canceled)

	release()
	release, err = slots.acquire(context.Background())
	require.NoError(t, err)
	release()
}

// 名额跟着存档库的池走:一个库的读名额用完,不影响读另一个库。
// 全进程只有一份名额时(2026-11-12 之前),K8s 每库 2 条连接只发得出 1 个名额,整个实例的读排成一队。
func TestStoreReadSlotsArePerStore(t *testing.T) {
	pools := fakeStorePools(t, map[uint32]fakeStoreConnector{fakeStoreA: {}, fakeStoreB: {}}, nil)
	_, holdStoreA, err := pools.ReadDB(context.Background(), fakeStoreA)
	require.NoError(t, err)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = pools.ReadDB(canceled, fakeStoreA)
	require.ErrorIs(t, err, context.Canceled, "库 A 的名额用完了")

	_, releaseStoreB, err := pools.ReadDB(context.Background(), fakeStoreB)
	require.NoError(t, err, "库 B 的读不用等库 A")
	releaseStoreB()

	holdStoreA()
	_, releaseStoreA, err := pools.ReadDB(context.Background(), fakeStoreA)
	require.NoError(t, err, "释放之后库 A 又有名额")
	releaseStoreA()
}

// 建角、判死释放、交易冻结证据走 DB,读名额用完也照常拿到连接池:名额就是为了不让读挤掉它们。
func TestStoreWritesDoNotTakeReadSlots(t *testing.T) {
	pools := fakeStorePools(t, map[uint32]fakeStoreConnector{fakeStoreA: {}}, nil)
	_, hold, err := pools.ReadDB(context.Background(), fakeStoreA)
	require.NoError(t, err)
	defer hold()

	db, _, err := pools.DB(context.Background(), fakeStoreA)
	require.NoError(t, err)
	require.NotNil(t, db)
}

// 打不开或不在白名单的库不占名额,也不会因为没有名额表项而卡住。
func TestStoreReadOfAnUnavailableStoreTakesNoSlot(t *testing.T) {
	pools := fakeStorePools(t, nil, map[uint32]error{fakeStoreD: errFlow})
	_, _, err := pools.ReadDB(context.Background(), fakeStoreD)
	require.ErrorIs(t, err, errFlow)
	require.Empty(t, pools.reads[fakeStoreD], "打开失败时没有占名额")

	_, _, err = pools.ReadDB(context.Background(), fakeStoreA)
	require.ErrorIs(t, err, accountdirectory.ErrUnavailable, "不在白名单")
}

// 名额跟着目录分片的池走:一片的读名额用完,不影响读另一片上玩家的路由。
func TestDirectoryReadSlotsArePerShard(t *testing.T) {
	const shards = 2
	s := &AccountDirectoryStore{directoryReads: []readSlots{newReadSlots(2), newReadSlots(2)}}
	// 找两名在第 0 片、一名在第 1 片的玩家。
	var onShard0 []uint64
	var onShard1 uint64
	for id := uint64(1); len(onShard0) < 2 || onShard1 == 0; id++ {
		if dirshard.SlotOfPlayer(id, shards) == 0 {
			onShard0 = append(onShard0, id)
		} else if onShard1 == 0 {
			onShard1 = id
		}
	}

	holdShard0, err := s.acquireDirectoryRead(context.Background(), onShard0[0])
	require.NoError(t, err)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = s.acquireDirectoryRead(canceled, onShard0[1])
	require.ErrorIs(t, err, context.Canceled, "同一片的另一名玩家要等")

	releaseShard1, err := s.acquireDirectoryRead(context.Background(), onShard1)
	require.NoError(t, err, "另一片不用等")
	releaseShard1()
	holdShard0()
}

// 账本读先占目录分片的名额读路由,放掉之后再占存档库的名额读行,不同时占两个;读完两个名额都还回去。
func TestLedgerReadReturnsBothSlots(t *testing.T) {
	s, steps := newFlowStore(2)
	steps.routed(11)
	s.stores = fakeStorePools(t, map[uint32]fakeStoreConnector{flowStorage: {
		cols: []string{"kind", "owner_epoch", "save_seq", "released", "payload"},
		rows: [][]driver.Value{{int64(playerStoreOwnerKind), int64(7), int64(9), int64(1), []byte{}}},
	}}, nil)
	// 每个池只有 1 个名额:漏还一个,第二次读就会等到截止时间。
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		row, err := s.ReadPlayerStoreAsset(ctx, 11)
		cancel()
		require.NoError(t, err)
		require.NotNil(t, row)
	}
	require.Empty(t, s.directoryReads[0])
	require.Empty(t, s.stores.reads[flowStorage])
}

// 存档库的名额等不到时,账本读按调用方的截止时间失败,不能当成「没有存档」。
func TestLedgerReadFailsWhenTheStoreSlotNeverFrees(t *testing.T) {
	s, steps := newFlowStore(2)
	steps.routed(11)
	s.stores = fakeStorePools(t, map[uint32]fakeStoreConnector{flowStorage: {}}, nil)
	_, hold, err := s.stores.ReadDB(context.Background(), flowStorage)
	require.NoError(t, err)
	defer hold()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	row, err := s.ReadPlayerStoreAsset(ctx, 11)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, row)
	require.Empty(t, s.directoryReads[0], "目录分片的名额在读完路由后就还了")
}

// 展示读同理:等不到名额的落点不记成「落点不可用」的结果交回去,整次调用按调用方的截止时间失败由 ReadPlayerDisplays 负责;
// 这里核对单个落点等不到名额时回的是 ctx 的错误。
func TestDisplayReadOfABusyStoreReportsTheCallerDeadline(t *testing.T) {
	s := &AccountDirectoryStore{stores: fakeStorePools(t, map[uint32]fakeStoreConnector{fakeStoreA: {}}, nil)}
	_, hold, err := s.stores.ReadDB(context.Background(), fakeStoreA)
	require.NoError(t, err)
	defer hold()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = s.readDisplaysOfStorage(ctx, fakeStoreA, []uint64{11})
	require.ErrorIs(t, err, context.DeadlineExceeded)
}
