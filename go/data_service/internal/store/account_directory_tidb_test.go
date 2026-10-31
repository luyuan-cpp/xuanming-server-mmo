package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/luyuancpp/proto2mysql"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	base "proto/common/base"
	databasepb "proto/common/database"
	dspb "proto/data_service"
	"shared/accountdirectory"
	"shared/authoritysql"
	"shared/authoritysql/sqltest"
	"shared/placement"
)

// 显式专用 TiDB DSN；每例只建／删自己的随机库，不借用其它任务的现有库或端口假设。
type directorySQLFixture struct {
	SQL              *sqltest.Fixture
	InitialStorageID uint32
	ProofStorageID   uint32
}

func accountDirectoryTiDB(t *testing.T) (*AccountDirectoryStore, *sql.DB) {
	t.Helper()
	directory, schema, _ := accountDirectoryTiDBWithWriter(t)
	return directory, schema
}

func accountDirectoryTiDBWithWriter(t *testing.T) (*AccountDirectoryStore, *sql.DB, *directorySQLFixture) {
	t.Helper()
	dsn := os.Getenv("ACCOUNT_DIRECTORY_TEST_DSN")
	if dsn == "" {
		if os.Getenv("ACCOUNT_DIRECTORY_REQUIRE_TIDB") == "1" {
			t.Fatal("缺少 ACCOUNT_DIRECTORY_TEST_DSN")
		}
		t.Skip("未配置专用账号目录 TiDB")
	}
	_, err := mysql.ParseDSN(dsn)
	require.NoError(t, err)
	var nonce [8]byte
	_, err = rand.Read(nonce[:])
	require.NoError(t, err)
	name := "account_directory_it_" + hex.EncodeToString(nonce[:])
	storageID := binary.BigEndian.Uint32(nonce[:4]) | 0x80000000
	proofStorageID := storageID ^ 1
	playerSchema, ok := placement.StoreDBName(storageID)
	require.True(t, ok)
	proofSchema, ok := placement.StoreDBName(proofStorageID)
	require.True(t, ok)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// 同boot可能访问的两个存储库在注册之前固定，proof测试不能运行中扩展保护集合。
	opts, err := sqltest.OptionsFromEnv(dsn, name, playerSchema, proofSchema)
	require.NoError(t, err)
	fixture, err := sqltest.New(ctx, t, opts)
	require.NoError(t, err)
	require.NoError(t, fixture.PrepareOwnedSchema(ctx, name, func(ctx context.Context, db *sql.DB) error {
		opts := MigrateOptions{}
		release, err := acquireMigrateLock(ctx, db, name, opts.LockWait)
		if err != nil {
			return err
		}
		defer release()
		return migrateSchemaOn(ctx, db, name, opts)
	}))
	var business []proto.Message
	for _, model := range TableMessages() {
		table, ok := proto2mysql.TableNameFromDescriptor(model.ProtoReflect().Descriptor())
		require.True(t, ok)
		switch table {
		case "route_meta", "writer_registry", "writer_slot", "writer_lifecycle_receipt", "sql_writer_drain_run", "sql_writer_freeze_ticket", "sql_writer_maintenance_receipt":
			continue
		}
		business = append(business, model)
	}
	require.NoError(t, fixture.GrantProtoTables(ctx, name, business))
	playerModels := []proto.Message{&databasepb.PlayerStore{}}
	for _, schema := range []string{playerSchema, proofSchema} {
		install, err := fixture.OpenSchema(ctx, schema)
		require.NoError(t, err)
		model := proto2mysql.NewDB()
		require.NoError(t, model.OpenDB(install, schema))
		for _, row := range playerModels {
			model.RegisterTable(row)
			require.NoError(t, model.CreateOrUpdateTable(row))
		}
		require.NoError(t, fixture.GrantProtoTables(ctx, schema, playerModels))
	}
	normal, err := mysql.ParseDSN(fixture.DSN(name))
	require.NoError(t, err)
	conf := MySQLConfig{Host: normal.Addr, User: normal.User, Password: normal.Passwd, DBName: name, MaxOpenConn: 8, MaxIdleConn: 2, Permission: fixture.Permission}
	directory, err := NewAccountDirectoryStore(conf, authoritysql.BackendTiDB, 2, 8)
	require.NoError(t, err)
	// 证据夹具会把路由切到 proof 库,两个库都要在白名单里。
	require.NoError(t, directory.ConfigureInitialPlayerCommit([]uint32{storageID, proofStorageID}))
	// 只隔离原目录契约；跨域资产互锁仍由对应正式用例装配。
	directory.contracts = &roleAssetContractGuard{disabled: true}
	t.Cleanup(func() { require.NoError(t, directory.Close()) })
	return directory, fixture.Schema, &directorySQLFixture{SQL: fixture, InitialStorageID: storageID, ProofStorageID: proofStorageID}
}

func TestTiDBAccountDirectoryCapReplayRepairAndRemoval(t *testing.T) {
	s, db := accountDirectoryTiDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := s.Read(ctx, "Account", true)
	require.NoError(t, err)
	type result struct {
		id  uint64
		err error
	}
	results := make(chan result, 8)
	var workers sync.WaitGroup
	for i := uint64(1); i <= 8; i++ {
		workers.Add(1)
		go func(id uint64) {
			defer workers.Done()
			_, err := createInitialRoleForTest(s, ctx, "Account", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: id})
			results <- result{id, err}
		}(i)
	}
	workers.Wait()
	close(results)
	created := []uint64{}
	full := 0
	for r := range results {
		if r.err == nil {
			created = append(created, r.id)
		} else {
			require.ErrorIs(t, r.err, accountdirectory.ErrFull)
			full++
		}
	}
	require.Len(t, created, 2)
	require.Equal(t, 6, full)
	roles, err := s.Read(ctx, "Account", false)
	require.NoError(t, err)
	require.Len(t, roles.Players, 2)
	id := created[0]
	same := &base.AccountSimplePlayer{ZoneId: 1, PlayerId: id}
	_, err = createInitialRoleForTest(s, ctx, "Account", same)
	require.NoError(t, err, "满位不拒绝已提交的原角色重放")
	_, err = createInitialRoleForTest(s, ctx, "account", same)
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	owned, err := s.Lookup(ctx, id)
	require.NoError(t, err)
	require.EqualValues(t, 1, owned.OwnerRevision)
	// SQL角色原件带未来字段；通过真实Repair接口补齐后仍须逐字节保留。
	unknown := protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 7)
	same.ProtoReflect().SetUnknown(unknown)
	raw, err := proto.Marshal(same)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "UPDATE account_role SET role_proto=? WHERE player_id=?", raw, id)
	require.NoError(t, err)
	_, err = s.Repair(ctx, "Account", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: id, ClassId: 3, AppearanceId: "04_mountain_guardian_boy"}, 1)
	require.NoError(t, err)
	owned, err = s.Lookup(ctx, id)
	require.NoError(t, err)
	require.Equal(t, unknown, owned.Role.ProtoReflect().GetUnknown())
	require.EqualValues(t, 3, owned.Role.ClassId)
	_, err = s.Repair(ctx, "Account", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: id, ClassId: 4}, 1)
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	_, err = db.ExecContext(ctx, "UPDATE account_role SET owner_revision=2 WHERE player_id=?", id)
	require.NoError(t, err)
	_, err = s.Repair(ctx, "Account", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: id, Name: "晚到旧链"}, 1)
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	require.ErrorIs(t, s.Remove(ctx, "Account", []*dspb.AccountRoleVersion{{PlayerId: id, OwnerRevision: 1}}), accountdirectory.ErrConflict)
	_, err = s.Lookup(ctx, id)
	require.NoError(t, err, "旧修订删除不得动新归属")
	require.NoError(t, s.Remove(ctx, "Account", []*dspb.AccountRoleVersion{{PlayerId: id, OwnerRevision: 2}}))
	require.NoError(t, s.Remove(ctx, "Account", []*dspb.AccountRoleVersion{{PlayerId: id, OwnerRevision: 2}}), "相同删除幂等")
	_, err = s.Lookup(ctx, id)
	require.ErrorIs(t, err, accountdirectory.ErrMissing)
	_, err = createInitialRoleForTest(s, ctx, "Account", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: id})
	require.ErrorIs(t, err, accountdirectory.ErrConflict, "重放旧建角不能复活已删除目录")
	_, err = s.Repair(ctx, "Account", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: id, Name: "删除前旧链"}, 1)
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	canceled, stop := context.WithCancel(ctx)
	stop()
	_, err = createInitialRoleForTest(s, canceled, "Account", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 99})
	require.True(t, errors.Is(err, context.Canceled))
	_, err = s.Lookup(ctx, 99)
	require.ErrorIs(t, err, accountdirectory.ErrMissing)
}

// 登录读目录:create=false 未命中不建行;create=true 未命中才建行;目录行已在时只读返回,
// 别的事务持着这一行的锁(比如同账号正在建角)也不排队,读到的是已提交的列表。
func TestTiDBAccountDirectoryReadTakesNoLockOnHit(t *testing.T) {
	s, db := accountDirectoryTiDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	directoryRows := func(account string) int {
		t.Helper()
		var rows int
		require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM account_directory WHERE account=?", []byte(account)).Scan(&rows))
		return rows
	}
	_, err := s.Read(ctx, "reader", false)
	require.ErrorIs(t, err, accountdirectory.ErrMissing)
	require.Zero(t, directoryRows("reader"), "create=false 不建行")
	roles, err := s.Read(ctx, "reader", true)
	require.NoError(t, err)
	require.Empty(t, roles.Players)
	require.Equal(t, 1, directoryRows("reader"))

	role := &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 3201}
	_, err = createInitialRoleForTest(s, ctx, "reader", role)
	require.NoError(t, err)

	holder, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer holder.Rollback()
	var stored []byte
	require.NoError(t, holder.QueryRowContext(ctx, "SELECT account FROM account_directory WHERE account='reader' FOR UPDATE").Scan(&stored))
	for _, create := range []bool{false, true} {
		bounded, stop := context.WithTimeout(ctx, 5*time.Second)
		roles, err = s.Read(bounded, "reader", create)
		stop()
		require.NoError(t, err, "目录行被锁时读不排队(create=%v)", create)
		require.Len(t, roles.Players, 1)
	}
	require.NoError(t, holder.Rollback())
}

// 并发首次登录只建一行目录;之后提交的角色,提交之后开始的 Read 一定读得到。
func TestTiDBAccountDirectoryConcurrentFirstLoginCreatesOneRow(t *testing.T) {
	s, db := accountDirectoryTiDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	results := make(chan error, 8)
	start := make(chan struct{})
	for range 8 {
		go func() {
			<-start
			_, err := s.Read(ctx, "first-login", true)
			results <- err
		}()
	}
	close(start)
	for range 8 {
		require.NoError(t, <-results)
	}
	var rows int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM account_directory WHERE account='first-login'").Scan(&rows))
	require.Equal(t, 1, rows)
	_, err := createInitialRoleForTest(s, ctx, "first-login", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 3202})
	require.NoError(t, err)
	roles, err := s.Read(ctx, "first-login", false)
	require.NoError(t, err)
	require.Len(t, roles.Players, 1)
	_, err = s.Read(ctx, string(make([]byte, 192)), true)
	require.ErrorIs(t, err, accountdirectory.ErrInvalid, "非法账号在碰库之前就拒")
}

func TestTiDBAccountDirectoryPreservesExactAccountIdentity(t *testing.T) {
	s, _ := accountDirectoryTiDB(t)
	ctx := context.Background()
	for i, a := range []string{"Case", "case", " Case"} {
		_, err := createInitialRoleForTest(s, ctx, a, &base.AccountSimplePlayer{ZoneId: 1, PlayerId: uint64(100 + i)})
		require.NoError(t, err)
	}
	for i, a := range []string{"Case", "case", " Case"} {
		roles, err := s.Read(ctx, a, false)
		require.NoError(t, err)
		require.Len(t, roles.Players, 1)
		require.EqualValues(t, 100+i, roles.Players[0].PlayerId)
	}
	for i := 0; i < 4; i++ {
		conn, err := s.db.Conn(ctx)
		require.NoError(t, err)
		var isolation, mode, replica string
		require.NoError(t, conn.QueryRowContext(ctx, "SELECT @@transaction_isolation,@@tidb_txn_mode,@@tidb_replica_read").Scan(&isolation, &mode, &replica))
		require.Equal(t, "READ-COMMITTED", isolation)
		require.Equal(t, "pessimistic", mode)
		require.Equal(t, "leader", replica)
		defer conn.Close()
	}
}

// 夹具只登记本次角色的正式原路由，随后仍调用公开 Create 完成真实建角事务。
// 不预建所有玩家路由、不修改任何已存在路由；生产事务的路由/合服保护仍实际执行。
func createInitialRoleForTest(s *AccountDirectoryStore, ctx context.Context, account string, role *base.AccountSimplePlayer) (*base.AccountSimplePlayerList, error) {
	if err := installInitialRouteForTest(s, ctx, role); err != nil {
		return nil, err
	}
	return s.Create(ctx, account, role)
}

func installInitialRouteForTest(s *AccountDirectoryStore, ctx context.Context, role *base.AccountSimplePlayer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(s.initialStores) != 1 {
		return accountdirectory.ErrUnavailable
	}
	var storage uint32
	for id := range s.initialStores {
		storage = id
	}
	_, err := s.db.ExecContext(ctx, "INSERT INTO player_route(route_shard,player_id,home_zone,storage_id,placement_version,rev,updated_by,created_at_ms,updated_at_ms) VALUES (?,?,?, ?,1,1,'initial-commit-test',1,1) ON DUPLICATE KEY UPDATE player_id=player_id", placement.RouteRowShard(role.GetPlayerId()), role.GetPlayerId(), role.GetZoneId(), storage)
	return err
}
