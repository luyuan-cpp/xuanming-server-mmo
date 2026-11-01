package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/luyuancpp/proto2mysql"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	base "proto/common/base"
	databasepb "proto/common/database"
	dspb "proto/data_service"
	"shared/accountdirectory"
	"shared/authoritysql"
	"shared/authoritysql/sqltest"
	"shared/dirshard"
	"shared/placement"
	"shared/rolefence"
)

// K=4 的真库夹具:每片一个随机库,外加流水库和一个存档库,闸门各一份。
// 开关和 account_directory_tidb_test.go 相同(ACCOUNT_DIRECTORY_TEST_DSN),只建/删自己的随机库。
//
// 下面的片号由 shared/dirshard 的金样钉住:账号 seller 在第 0 片,buyer 在第 3 片;
// 玩家 12 在第 0 片,13 在第 1 片,14 在第 2 片,11 在第 3 片;名字 yunzhongjun 在第 1 片。
const (
	shardFixtureK  = 4
	shardSeller    = "seller"
	shardBuyer     = "buyer"
	shardSellerAt  = 0
	shardBuyerAt   = 3
	shardRoleAtOne = uint64(13)
	shardRoleAtTwo = uint64(14)
)

type directoryShardFixture struct {
	SQL     *sqltest.Fixture
	Conf    MySQLConfig
	Base    string
	Storage uint32
	// Raw 是每片的 schema 连接,按下标取,用来断言行落在哪一片。
	Raw    []*sql.DB
	Ledger *sql.DB
}

func requireAccountDirectoryDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("ACCOUNT_DIRECTORY_TEST_DSN")
	if dsn == "" {
		if os.Getenv("ACCOUNT_DIRECTORY_REQUIRE_TIDB") == "1" {
			t.Fatal("缺少 ACCOUNT_DIRECTORY_TEST_DSN")
		}
		t.Skip("未配置专用账号目录 TiDB")
	}
	return dsn
}

func randomSuffixForTest(t *testing.T) []byte {
	t.Helper()
	var nonce [8]byte
	_, err := rand.Read(nonce[:])
	require.NoError(t, err)
	return nonce[:]
}

// tableNamesForTest 列出一组 message 的表名。
func tableNamesForTest(tables []proto.Message) []string {
	names := make([]string, 0, len(tables))
	for _, table := range tables {
		name, _ := proto2mysql.TableNameFromDescriptor(table.ProtoReflect().Descriptor())
		names = append(names, name)
	}
	return names
}

// businessTablesForTest 去掉闸门表:闸门表归 operator,普通写者不授权。
func businessTablesForTest(tables []proto.Message) []proto.Message {
	var business []proto.Message
	for _, model := range tables {
		name, _ := proto2mysql.TableNameFromDescriptor(model.ProtoReflect().Descriptor())
		switch name {
		case "route_meta", "writer_registry", "writer_slot", "writer_lifecycle_receipt", "sql_writer_drain_run", "sql_writer_freeze_ticket", "sql_writer_maintenance_receipt":
			continue
		}
		business = append(business, model)
	}
	return business
}

func newDirectoryShardFixture(t *testing.T) *directoryShardFixture {
	t.Helper()
	dsn := requireAccountDirectoryDSN(t)
	nonce := randomSuffixForTest(t)
	name := "account_directory_k4_" + hex.EncodeToString(nonce)
	storageID := binary.BigEndian.Uint32(nonce[:4]) | 0x80000000
	playerSchema, ok := placement.StoreDBName(storageID)
	require.True(t, ok)
	shards := dirshard.DirectoryDBNames(name, shardFixtureK)
	ledger := dirshard.LedgerDBName(name)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	opts, err := sqltest.OptionsFromEnv(dsn, shards[0], append(append([]string{}, shards...), ledger, playerSchema)...)
	require.NoError(t, err)
	fixture, err := sqltest.New(ctx, t, opts)
	require.NoError(t, err)

	out := &directoryShardFixture{SQL: fixture, Base: name, Storage: storageID, Raw: make([]*sql.DB, shardFixtureK)}
	for slot, schema := range shards {
		require.NoError(t, fixture.PrepareOwnedSchema(ctx, schema, func(ctx context.Context, db *sql.DB) error {
			return migrateDirectoryShard(ctx, db, schema, slot, shardFixtureK, MigrateOptions{})
		}))
		require.NoError(t, fixture.GrantProtoTables(ctx, schema, businessTablesForTest(directoryTableMessages(slot))))
		raw, err := fixture.OpenSchema(ctx, schema)
		require.NoError(t, err)
		out.Raw[slot] = raw
	}
	require.NoError(t, fixture.PrepareOwnedSchema(ctx, ledger, func(ctx context.Context, db *sql.DB) error {
		return migrateLedger(ctx, db, ledger, MigrateOptions{})
	}))
	require.NoError(t, fixture.GrantProtoTables(ctx, ledger, businessTablesForTest(ledgerTableMessages())))
	out.Ledger, err = fixture.OpenSchema(ctx, ledger)
	require.NoError(t, err)

	install, err := fixture.OpenSchema(ctx, playerSchema)
	require.NoError(t, err)
	model := proto2mysql.NewDB()
	require.NoError(t, model.OpenDB(install, playerSchema))
	playerModels := []proto.Message{&databasepb.PlayerStore{}}
	for _, row := range playerModels {
		model.RegisterTable(row)
		require.NoError(t, model.CreateOrUpdateTable(row))
	}
	require.NoError(t, fixture.GrantProtoTables(ctx, playerSchema, playerModels))

	normal, err := mysql.ParseDSN(fixture.DSN(shards[0]))
	require.NoError(t, err)
	out.Conf = MySQLConfig{Host: normal.Addr, User: normal.User, Password: normal.Passwd, DBName: name, MaxOpenConn: 4, MaxIdleConn: 2, LogicalShards: shardFixtureK, Permission: fixture.Permission, PermissionFor: fixture.PermissionFor}
	return out
}

func (f *directoryShardFixture) directory(t *testing.T, maxRoles int) *AccountDirectoryStore {
	t.Helper()
	directory, err := NewAccountDirectoryStore(f.Conf, authoritysql.BackendTiDB, maxRoles, 4)
	require.NoError(t, err)
	require.NoError(t, directory.ConfigurePlayerStores([]uint32{f.Storage}, PlayerStorePoolOptions{MaxOpenConn: 4, MaxIdleConn: 2}))
	initialStorageForTest.Store(directory, f.Storage)
	directory.ConfigureFenceCoordinator(&fakeFences{outcome: rolefence.PrepareClean})
	t.Cleanup(func() {
		initialStorageForTest.Delete(directory)
		require.NoError(t, directory.Close())
	})
	return directory
}

// rowsOnShards 数每一片上这条查询的结果。
func (f *directoryShardFixture) rowsOnShards(t *testing.T, query string, args ...any) []int {
	t.Helper()
	out := make([]int, shardFixtureK)
	for slot, db := range f.Raw {
		require.NoError(t, db.QueryRow(query, args...).Scan(&out[slot]))
	}
	return out
}

func tableExistsForTest(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name=?", table).Scan(&count))
	return count == 1
}

// 夹具依赖的片号和 dirshard 算出来的一致,对不上就先修这里。
func TestDirectoryShardFixtureConstantsMatchDirshard(t *testing.T) {
	require.Equal(t, shardSellerAt, dirshard.SlotOfAccount(shardSeller, shardFixtureK))
	require.Equal(t, shardBuyerAt, dirshard.SlotOfAccount(shardBuyer, shardFixtureK))
	require.Equal(t, 1, dirshard.SlotOfPlayer(shardRoleAtOne, shardFixtureK))
	require.Equal(t, 2, dirshard.SlotOfPlayer(shardRoleAtTwo, shardFixtureK))
	require.Equal(t, 1, dirshard.SlotOfName("yunzhongjun", shardFixtureK))
}

// 账号目录和 ref 只在账号所在的片,角色行、路由只在玩家所在的片。读列表跨片。
func TestTiDBShardedCreateAndReadAcrossShards(t *testing.T) {
	f := newDirectoryShardFixture(t)
	s := f.directory(t, 5)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	for _, player := range []uint64{shardRoleAtOne, shardRoleAtTwo} {
		_, err := createInitialRoleForTest(s, ctx, shardSeller, &base.AccountSimplePlayer{PlayerId: player, ZoneId: 1})
		require.NoError(t, err)
	}
	require.Equal(t, []int{1, 0, 0, 0}, f.rowsOnShards(t, "SELECT COUNT(*) FROM account_directory"))
	require.Equal(t, []int{2, 0, 0, 0}, f.rowsOnShards(t, "SELECT COUNT(*) FROM account_role_ref WHERE state=?", refStateActive))
	require.Equal(t, []int{0, 1, 1, 0}, f.rowsOnShards(t, "SELECT COUNT(*) FROM account_role"))
	require.Equal(t, []int{0, 1, 1, 0}, f.rowsOnShards(t, "SELECT COUNT(*) FROM player_route"))

	roles, err := s.Read(ctx, shardSeller, false)
	require.NoError(t, err)
	require.Len(t, roles.Players, 2)
	require.Equal(t, shardRoleAtOne, roles.Players[0].PlayerId)
	require.Equal(t, shardRoleAtTwo, roles.Players[1].PlayerId)

	owned, err := s.Lookup(ctx, shardRoleAtTwo)
	require.NoError(t, err)
	require.Equal(t, shardSeller, owned.Account)
}

// 一个账号的角色上限按 ref 计,判定只在账号所在的片,并发也不超。
func TestTiDBShardedCapHoldsUnderConcurrency(t *testing.T) {
	f := newDirectoryShardFixture(t)
	s := f.directory(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	results := make(chan error, 8)
	for player := uint64(1); player <= 8; player++ {
		go func(player uint64) {
			_, err := createInitialRoleForTest(s, ctx, shardSeller, &base.AccountSimplePlayer{PlayerId: player, ZoneId: 1})
			results <- err
		}(player)
	}
	created, full := 0, 0
	for range 8 {
		err := <-results
		if err == nil {
			created++
			continue
		}
		require.ErrorIs(t, err, accountdirectory.ErrFull)
		full++
	}
	require.Equal(t, 2, created)
	require.Equal(t, 6, full)
	roles, err := s.Read(ctx, shardSeller, false)
	require.NoError(t, err)
	require.Len(t, roles.Players, 2)
}

// 完整的跨片过户:角色行、收据在玩家所在的片,买家 ref 在买家的片,卖家 ref 从卖家的片删掉。证据从存档库读。
func TestTiDBShardedTransferAcrossShards(t *testing.T) {
	f := newDirectoryShardFixture(t)
	s := f.directory(t, 5)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := createInitialRoleForTest(s, ctx, shardSeller, &base.AccountSimplePlayer{PlayerId: shardRoleAtOne, ZoneId: 1})
	require.NoError(t, err)
	verifier, err := NewSQLRoleFreezeVerifier(s, []uint32{f.Storage})
	require.NoError(t, err)
	trades := NewAccountRoleTradeStore(s, verifier)
	_, err = trades.Begin(ctx, &dspb.BeginAccountRoleTradeRequest{ListingId: 101, PlayerId: shardRoleAtOne, SellerAccount: shardSeller, ExpectedOwnerRevision: 1})
	require.NoError(t, err)
	frozen, err := trades.Confirm(ctx, &dspb.ConfirmAccountRoleTradeFreezeRequest{ListingId: 101, PlayerId: shardRoleAtOne, SellerAccount: shardSeller, ExpectedOwnerRevision: 1})
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleFreezeEvidenceKind_ACCOUNT_ROLE_FREEZE_EVIDENCE_KIND_INITIAL_COMMIT, frozen.FreezeEvidence.Kind)

	in := &dspb.TransferAccountRoleRequest{TxId: 1001, ListingId: 101, PlayerId: shardRoleAtOne, SellerAccount: shardSeller, BuyerAccount: shardBuyer, ExpectedOwnerRevision: 1}
	out, err := trades.Transfer(ctx, in)
	require.NoError(t, err)
	require.EqualValues(t, 2, out.BuyerOwnerRevision)

	require.Equal(t, []int{0, 1, 0, 0}, f.rowsOnShards(t, "SELECT COUNT(*) FROM account_role_transfer_receipt"))
	require.Equal(t, []int{0, 1, 0, 0}, f.rowsOnShards(t, "SELECT COUNT(*) FROM account_role_trade"))
	require.Equal(t, []int{0, 0, 0, 1}, f.rowsOnShards(t, "SELECT COUNT(*) FROM account_role_ref WHERE account=? AND state=?", []byte(shardBuyer), refStateActive))
	require.Equal(t, []int{0, 0, 0, 0}, f.rowsOnShards(t, "SELECT COUNT(*) FROM account_role_ref WHERE account=?", []byte(shardSeller)))

	buyer, err := s.Read(ctx, shardBuyer, false)
	require.NoError(t, err)
	require.Len(t, buyer.Players, 1)
	seller, err := s.Read(ctx, shardSeller, false)
	require.NoError(t, err)
	require.Empty(t, seller.Players)

	replayed, err := trades.Transfer(ctx, in)
	require.NoError(t, err)
	require.True(t, proto.Equal(out, replayed))
}

// 建角失败留下的 PENDING ref:读路径过滤,超过宽限后后台写墓碑清掉,迟到的建角不能复活。
func TestTiDBShardedStaleCreateRefIsSweptAndTombstoned(t *testing.T) {
	f := newDirectoryShardFixture(t)
	s := f.directory(t, 5)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	role := &base.AccountSimplePlayer{PlayerId: shardRoleAtOne, ZoneId: 1}
	require.NoError(t, installInitialRouteForTest(s, ctx, role))
	_, err := s.steps.reserveRef(ctx, shardSeller, roleRef{PlayerID: role.PlayerId, HomeZone: role.ZoneId, Nonce: newRefNonce()}, 5)
	require.NoError(t, err)
	roles, err := s.Read(ctx, shardSeller, false)
	require.NoError(t, err)
	require.Empty(t, roles.Players)

	// 还在宽限内:不动。
	result, err := s.SweepStaleRefs(ctx, 0, 64)
	require.NoError(t, err)
	require.Zero(t, result.Dropped)
	require.Equal(t, []int{1, 0, 0, 0}, f.rowsOnShards(t, "SELECT COUNT(*) FROM account_role_ref"))

	// 把 ref 的时间拨回宽限之前。
	_, err = f.Raw[shardSellerAt].ExecContext(ctx, "UPDATE account_role_ref SET created_ms=created_ms-? WHERE player_id=?", refPendingGrace.Milliseconds()+1000, role.PlayerId)
	require.NoError(t, err)
	result, err = s.SweepStaleRefs(ctx, refPendingGrace, 64)
	require.NoError(t, err)
	require.Equal(t, 1, result.Dropped)
	require.Equal(t, []int{0, 0, 0, 0}, f.rowsOnShards(t, "SELECT COUNT(*) FROM account_role_ref"))
	require.Equal(t, []int{0, 1, 0, 0}, f.rowsOnShards(t, "SELECT COUNT(*) FROM account_role WHERE LENGTH(account)=0"))

	_, err = s.Create(ctx, shardSeller, role)
	require.ErrorIs(t, err, accountdirectory.ErrConflict, "墓碑挡住迟到的建角")
}

// reserveRef 的事务读完 ref 之后,后台的 dropRef 抢先删掉它(它不持目录行锁)。写是 upsert,ref 还在。
// 原来的 UPDATE 这时影响 0 行还回成功,过户就在没有 ref 的情况下提交了。
func TestTiDBWritePendingRefSurvivesConcurrentDrop(t *testing.T) {
	f := newDirectoryShardFixture(t)
	s := f.directory(t, 5)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := s.steps.reserveRef(ctx, shardBuyer, roleRef{PlayerID: shardRoleAtOne, HomeZone: 1, Nonce: newRefNonce()}, 5)
	require.NoError(t, err)

	want := roleRef{PlayerID: shardRoleAtOne, TxID: 1001, ListingID: 101, Nonce: newRefNonce()}
	err = shardTx(ctx, s.shards.forAccount(shardBuyer), func(tx *sql.Tx) error {
		require.NoError(t, lockDirectory(ctx, tx, shardBuyer, true))
		refs, err := readRefs(ctx, tx, shardBuyer)
		require.NoError(t, err)
		require.Len(t, refs, 1)
		// 走裸连接:这个事务持着一个闸门槽位,经闸门的连接可能排在它后面。
		_, err = f.Raw[shardBuyerAt].ExecContext(ctx, "DELETE FROM account_role_ref WHERE account=? AND player_id=?", []byte(shardBuyer), shardRoleAtOne)
		require.NoError(t, err)
		return writePendingRef(ctx, tx, shardBuyer, want)
	})
	require.NoError(t, err)

	ref, err := s.steps.readRef(ctx, shardBuyer, shardRoleAtOne)
	require.NoError(t, err)
	require.NotNil(t, ref, "ref 不能丢")
	require.Equal(t, refStatePending, ref.State)
	require.EqualValues(t, 1001, ref.TxID)
	require.EqualValues(t, 101, ref.ListingID)
	require.Equal(t, want.Nonce, ref.Nonce)
}

// 已有 ref 时不占新名额,改写成这笔过户并换 nonce。建角的重放不动它。
func TestTiDBReserveRefOverExistingRef(t *testing.T) {
	f := newDirectoryShardFixture(t)
	s := f.directory(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	first := roleRef{PlayerID: shardRoleAtOne, HomeZone: 1, Nonce: newRefNonce()}
	full, err := s.steps.reserveRef(ctx, shardBuyer, first, 1)
	require.NoError(t, err)
	require.Nil(t, full)

	full, err = s.steps.reserveRef(ctx, shardBuyer, roleRef{PlayerID: shardRoleAtOne, HomeZone: 1, Nonce: newRefNonce()}, 1)
	require.NoError(t, err)
	require.Nil(t, full)
	ref, err := s.steps.readRef(ctx, shardBuyer, shardRoleAtOne)
	require.NoError(t, err)
	require.Equal(t, first.Nonce, ref.Nonce, "建角的重放不动它")

	transfer := roleRef{PlayerID: shardRoleAtOne, TxID: 1001, ListingID: 101, Nonce: newRefNonce()}
	full, err = s.steps.reserveRef(ctx, shardBuyer, transfer, 1)
	require.NoError(t, err)
	require.Nil(t, full, "已有这条 ref,满位也不占新名额")
	ref, err = s.steps.readRef(ctx, shardBuyer, shardRoleAtOne)
	require.NoError(t, err)
	require.Equal(t, transfer.Nonce, ref.Nonce)
	require.EqualValues(t, 1001, ref.TxID)
	require.Zero(t, ref.HomeZone, "home_zone 只有建角的 PENDING 才有")

	full, err = s.steps.reserveRef(ctx, shardBuyer, roleRef{PlayerID: shardRoleAtTwo, TxID: 1002, ListingID: 102, Nonce: newRefNonce()}, 1)
	require.NoError(t, err)
	require.Len(t, full, 1, "别的角色要占新名额")
}

// activateRef 只在 ref 属于本笔过户、或者没有在途过户时才改。覆盖了别的过户的话,后台会按「ACTIVE 且真相不是这个账号」把它删掉。
func TestTiDBActivateRefLeavesAnotherTransferAlone(t *testing.T) {
	f := newDirectoryShardFixture(t)
	s := f.directory(t, 5)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	read := func() *roleRef {
		t.Helper()
		ref, err := s.steps.readRef(ctx, shardBuyer, shardRoleAtOne)
		require.NoError(t, err)
		return ref
	}
	pending := roleRef{PlayerID: shardRoleAtOne, TxID: 2002, ListingID: 202, Nonce: newRefNonce()}
	_, err := s.steps.reserveRef(ctx, shardBuyer, pending, 5)
	require.NoError(t, err)

	for _, other := range []uint64{2001, 0} {
		require.NoError(t, s.steps.activateRef(ctx, shardBuyer, shardRoleAtOne, other), "没改到不算错")
		ref := read()
		require.Equal(t, refStatePending, ref.State, "tx_id=%d", other)
		require.EqualValues(t, 2002, ref.TxID)
		require.EqualValues(t, 202, ref.ListingID)
		require.Equal(t, pending.Nonce, ref.Nonce)
	}

	require.NoError(t, s.steps.activateRef(ctx, shardBuyer, shardRoleAtOne, 2002))
	ref := read()
	require.Equal(t, refStateActive, ref.State)
	require.Zero(t, ref.TxID)
	require.Zero(t, ref.ListingID)
	require.NotEqual(t, pending.Nonce, ref.Nonce, "换 nonce,挡住在途的清理")

	// 没有在途过户:谁来都能写。
	active := ref.Nonce
	require.NoError(t, s.steps.activateRef(ctx, shardBuyer, shardRoleAtOne, 2003))
	require.Equal(t, refStateActive, read().State)
	require.NotEqual(t, active, read().Nonce)

	// 行没了就补。
	_, err = f.Raw[shardBuyerAt].ExecContext(ctx, "DELETE FROM account_role_ref WHERE account=? AND player_id=?", []byte(shardBuyer), shardRoleAtOne)
	require.NoError(t, err)
	require.NoError(t, s.steps.activateRef(ctx, shardBuyer, shardRoleAtOne, 2004))
	require.Equal(t, refStateActive, read().State)
}

// 名字行在名字所在的片,owner 行在玩家所在的片;批量读按玩家分片拆组。
func TestTiDBShardedPlayerNames(t *testing.T) {
	f := newDirectoryShardFixture(t)
	cfg := f.Conf
	names, err := NewPlayerNameStore(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, names.Close()) })
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	now := uint64(time.Now().UnixMilli())

	const norm = "yunzhongjun"
	outcome, _, err := names.Reserve(ctx, shardRoleAtTwo, "YunZhongJun", norm, now)
	require.NoError(t, err)
	require.Equal(t, ReserveInserted, outcome)
	require.Equal(t, []int{0, 1, 0, 0}, f.rowsOnShards(t, "SELECT COUNT(*) FROM player_name"))
	require.Equal(t, []int{0, 0, 1, 0}, f.rowsOnShards(t, "SELECT COUNT(*) FROM player_name_owner"))

	// 撞名问名字所在的片,和玩家在哪一片无关。
	outcome, owner, err := names.Reserve(ctx, 12, "YUNZHONGJUN", norm, now)
	require.NoError(t, err)
	require.Equal(t, ReserveTaken, outcome)
	require.Equal(t, shardRoleAtTwo, owner)
	require.Equal(t, []int{0, 0, 1, 0}, f.rowsOnShards(t, "SELECT COUNT(*) FROM player_name_owner"), "撞名不写 owner 行")

	outcome, _, err = names.Reserve(ctx, 12, "Other", "other", now)
	require.NoError(t, err)
	require.Equal(t, ReserveInserted, outcome)

	got, err := names.BatchGet(ctx, []uint64{shardRoleAtTwo, 12, shardRoleAtOne})
	require.NoError(t, err)
	require.Equal(t, map[uint64]string{shardRoleAtTwo: "YunZhongJun", 12: "Other"}, got)

	// 第 2 步失败之后的重放:owner 行补上。
	_, err = f.Raw[2].ExecContext(ctx, "DELETE FROM player_name_owner WHERE player_id=?", shardRoleAtTwo)
	require.NoError(t, err)
	outcome, _, err = names.Reserve(ctx, shardRoleAtTwo, "YunZhongJun", norm, now)
	require.NoError(t, err)
	require.Equal(t, ReserveAlreadyOwned, outcome)

	rel, err := names.Release(ctx, shardRoleAtTwo, norm, 0)
	require.NoError(t, err)
	require.Equal(t, ReleaseDeleted, rel)
	require.Equal(t, []int{0, 0, 0, 0}, f.rowsOnShards(t, "SELECT COUNT(*) FROM player_name_owner WHERE player_id=?", shardRoleAtTwo))
	got, err = names.BatchGet(ctx, []uint64{shardRoleAtTwo})
	require.NoError(t, err)
	require.Empty(t, got)
}

// 路由按玩家分片,批量读跨片。zone_merged_into 只在第 0 片。
func TestTiDBShardedRouteStore(t *testing.T) {
	f := newDirectoryShardFixture(t)
	routes, err := NewRouteStore(f.Conf, RouteStoreOptions{Writer: NewRouteWriterID("test", "host")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, routes.Close()) })
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	has, err := routes.HasAnyRoute(ctx)
	require.NoError(t, err)
	require.False(t, has)
	for _, player := range []uint64{11, 12, 13, 14} {
		_, err = routes.RegisterHome(ctx, RouteRegisterRequest{PlayerID: player, HomeZone: 7, StorageID: f.Storage})
		require.NoError(t, err)
	}
	require.Equal(t, []int{1, 1, 1, 1}, f.rowsOnShards(t, "SELECT COUNT(*) FROM player_route"))
	has, err = routes.HasAnyRoute(ctx)
	require.NoError(t, err)
	require.True(t, has)
	rows, err := routes.GetRoutes(ctx, []uint64{11, 12, 13, 14, 15})
	require.NoError(t, err)
	require.Len(t, rows, 4)
	require.EqualValues(t, f.Storage, rows[13].StorageID)

	merged, err := routes.ListMergedInto(ctx)
	require.NoError(t, err)
	require.Empty(t, merged)
	require.True(t, tableExistsForTest(t, f.Raw[0], ZoneMergedIntoTableName))
	for _, db := range f.Raw[1:] {
		require.False(t, tableExistsForTest(t, db, ZoneMergedIntoTableName))
	}
}

// 流水和快照只在流水库,目录里没有它们。流水库有自己的闸门。
func TestTiDBLedgerIsItsOwnDatabase(t *testing.T) {
	f := newDirectoryShardFixture(t)
	for _, table := range []string{"transaction_log", PlayerSnapshotTableName, "rollback_audit_log", RouteMetaTableName} {
		require.Truef(t, tableExistsForTest(t, f.Ledger, table), "流水库缺 %s", table)
	}
	for _, table := range []string{AccountRoleTableName, PlayerNameTableName, PlayerRouteTableName} {
		require.Falsef(t, tableExistsForTest(t, f.Ledger, table), "%s 不该在流水库", table)
	}
	for _, db := range f.Raw {
		for _, table := range []string{"transaction_log", PlayerSnapshotTableName, "rollback_audit_log"} {
			require.Falsef(t, tableExistsForTest(t, db, table), "%s 不该在目录", table)
		}
	}
	ledgerCfg, err := f.Conf.ForSchema(dirshard.LedgerDBName(f.Base))
	require.NoError(t, err)
	snapshots, err := NewSnapshotStore(ledgerCfg)
	require.NoError(t, err)
	require.NoError(t, snapshots.Close())
}

// 配错 K 拒绝启动:每片的 directory_shard 记着建表时的 K。
func TestTiDBWrongLogicalShardsRefusesToStart(t *testing.T) {
	f := newDirectoryShardFixture(t)
	wrong := f.Conf
	wrong.LogicalShards = 2
	st, err := NewPlayerNameStore(wrong)
	if err == nil {
		_ = st.Close()
	}
	require.ErrorContains(t, err, "Directory.LogicalShards")
}

// -migrate 逐库建表:先核完全部库都是空的再动手,只要有一个库已有表就一张都不建。
func TestTiDBMigrateSchemaBuildsEveryShardAndLedger(t *testing.T) {
	dsn := requireAccountDirectoryDSN(t)
	cfg, err := mysql.ParseDSN(dsn)
	require.NoError(t, err)
	admin, err := sql.Open("mysql", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var principal string
	require.NoError(t, admin.QueryRowContext(ctx, "SELECT CURRENT_USER()").Scan(&principal))

	createAll := func(base string, k int) []string {
		names := append(dirshard.DirectoryDBNames(base, k), dirshard.LedgerDBName(base))
		for _, name := range names {
			_, err := admin.ExecContext(ctx, "CREATE DATABASE `"+name+"`")
			require.NoError(t, err)
			t.Cleanup(func() {
				drop, done := context.WithTimeout(context.Background(), time.Minute)
				defer done()
				_, err := admin.ExecContext(drop, "DROP DATABASE IF EXISTS `"+name+"`")
				require.NoError(t, err)
			})
		}
		return names
	}
	conf := func(base string, k int) MySQLConfig {
		return MySQLConfig{Host: cfg.Addr, User: cfg.User, Password: cfg.Passwd, DBName: base, MaxOpenConn: 4, MaxIdleConn: 2, LogicalShards: k, Permission: authoritysql.Schema(principal)}
	}
	tableCount := func(schema string) int {
		var count int
		require.NoError(t, admin.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=?", schema).Scan(&count))
		return count
	}
	hasTable := func(schema, table string) bool {
		var count int
		require.NoError(t, admin.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=? AND table_name=?", schema, table).Scan(&count))
		return count == 1
	}

	base := "account_directory_mig_" + hex.EncodeToString(randomSuffixForTest(t))
	createAll(base, 2)
	require.NoError(t, MigrateSchema(ctx, conf(base, 2), MigrateOptions{}))
	for slot, schema := range dirshard.DirectoryDBNames(base, 2) {
		var k, got int
		require.NoError(t, admin.QueryRowContext(ctx, fmt.Sprintf("SELECT logical_shards,slot FROM `%s`.directory_shard WHERE id=1", schema)).Scan(&k, &got))
		require.Equal(t, []int{2, slot}, []int{k, got})
		for _, table := range []string{AccountRoleTableName, AccountRoleRefTableName, PlayerNameTableName, PlayerNameOwnerTableName, PlayerRouteTableName, RouteMetaTableName, authoritysql.MoveSentinelTable} {
			require.Truef(t, hasTable(schema, table), "%s 缺 %s", schema, table)
		}
		require.Equal(t, slot == 0, hasTable(schema, ZoneMergedIntoTableName), schema)
		require.False(t, hasTable(schema, PlayerSnapshotTableName), schema)
		var seeded int
		require.NoError(t, admin.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM `%s`.route_meta WHERE id=1", schema)).Scan(&seeded))
		require.Equal(t, 1, seeded, "每片一份闸门")
	}
	ledger := dirshard.LedgerDBName(base)
	for _, table := range []string{"transaction_log", PlayerSnapshotTableName, "rollback_audit_log", RouteMetaTableName, authoritysql.MoveSentinelTable} {
		require.Truef(t, hasTable(ledger, table), "%s 缺 %s", ledger, table)
	}
	require.False(t, hasTable(ledger, AccountRoleTableName))

	// 每个 zone、每次部署都跑:完整的库跳过,一个字节都不动,尤其不换 generation(写者名册按它渲染)。
	shards := dirshard.DirectoryDBNames(base, 2)
	generationOf := func(schema string) string {
		var generation string
		require.NoError(t, admin.QueryRowContext(ctx, "SELECT generation FROM `"+schema+"`.route_meta WHERE id=1").Scan(&generation))
		return generation
	}
	first := generationOf(shards[0])
	require.NoError(t, MigrateSchema(ctx, conf(base, 2), MigrateOptions{}), "重复跑")
	require.Equal(t, first, generationOf(shards[0]), "重复跑不换 generation")

	// K 变了:这个库不是这一片,拒绝。
	require.ErrorContains(t, MigrateSchema(ctx, conf(base, 4), MigrateOptions{}), "K cannot change")

	// 中途崩过:建好的库跳过,没动过的库接着建。
	_, err = admin.ExecContext(ctx, "DROP DATABASE `"+shards[1]+"`")
	require.NoError(t, err)
	_, err = admin.ExecContext(ctx, "CREATE DATABASE `"+shards[1]+"`")
	require.NoError(t, err)
	require.NoError(t, MigrateSchema(ctx, conf(base, 2), MigrateOptions{}), "接着建")
	require.True(t, hasTable(shards[1], AccountRoleTableName))
	require.Equal(t, first, generationOf(shards[0]), "已经建好的库不动")

	// 表齐了但闸门行没种(崩在建完表之后):不修补,拒绝。
	_, err = admin.ExecContext(ctx, "DELETE FROM `"+shards[1]+"`.route_meta WHERE id=1")
	require.NoError(t, err)
	require.ErrorContains(t, MigrateSchema(ctx, conf(base, 2), MigrateOptions{}), "route_meta has no row")

	// 只有最后一个库已有表:半成品,前面的库一张都不建。
	partial := "account_directory_mig_" + hex.EncodeToString(randomSuffixForTest(t))
	names := createAll(partial, 2)
	_, err = admin.ExecContext(ctx, "CREATE TABLE `"+dirshard.LedgerDBName(partial)+"`.transaction_log (id BIGINT PRIMARY KEY)")
	require.NoError(t, err)
	require.ErrorContains(t, MigrateSchema(ctx, conf(partial, 2), MigrateOptions{}), "half built")
	require.Zero(t, tableCount(names[0]))
	require.Zero(t, tableCount(names[1]))

	require.Error(t, MigrateSchema(ctx, conf(partial, 3), MigrateOptions{}), "K 不是 64 的约数")
}
