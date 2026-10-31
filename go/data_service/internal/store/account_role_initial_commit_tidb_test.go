package store

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	base "proto/common/base"
	dspb "proto/data_service"
	storepb "proto/player_store"
	"shared/accountdirectory"
	"shared/authoritysql"
	"shared/generated/persistschema"
	"shared/placement"
)

func initialPlayerSchemaForTest(t *testing.T, s *AccountDirectoryStore) (uint32, string) {
	t.Helper()
	require.Len(t, s.initialStores, 1)
	for id := range s.initialStores {
		name, ok := placement.StoreDBName(id)
		require.True(t, ok)
		return id, name
	}
	t.Fatal("缺夹具落点")
	return 0, ""
}

func requireInitialRowsForTest(t *testing.T, db *sql.DB, schema string, player uint64) {
	t.Helper()
	for _, kind := range []storepb.PlayerStoreKind{
		storepb.PlayerStoreKind_PLAYER_STORE_KIND_OWNER,
		storepb.PlayerStoreKind_PLAYER_STORE_KIND_HOT,
		storepb.PlayerStoreKind_PLAYER_STORE_KIND_PROFILE,
	} {
		row, ok := readStoreRowForTest(t, db, schema, player, kind)
		require.True(t, ok, kind.String())
		require.Equal(t, []uint64{1, 1, uint64(persistschema.Gen)}, []uint64{row.epoch, row.seq, uint64(row.gen)}, kind.String())
	}
}

func TestTiDBCreateRoleCommitsDirectoryAndInitialRowsOnce(t *testing.T) {
	s, db := accountDirectoryTiDB(t)
	_, schema := initialPlayerSchemaForTest(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	role := &base.AccountSimplePlayer{PlayerId: 3101, ZoneId: 1, Name: "初始提交"}
	require.NoError(t, installInitialRouteForTest(s, ctx, role))
	out, err := s.CreateWithInitialCommit(ctx, "owner", role)
	require.NoError(t, err)
	require.Len(t, out.Players.Players, 1)
	requireInitialRowsForTest(t, db, schema, role.PlayerId)
	var marker uint64
	require.NoError(t, db.QueryRow("SELECT initial_player_commit FROM account_role WHERE player_id=?", role.PlayerId).Scan(&marker))
	require.EqualValues(t, 1, marker)

	// 原目录幂等不能补回后来丢失的 SQL 行。
	_, err = db.Exec("DELETE FROM `"+schema+"`.player_store WHERE player_id=? AND kind=?", role.PlayerId, int32(storepb.PlayerStoreKind_PLAYER_STORE_KIND_PROFILE))
	require.NoError(t, err)
	_, err = s.CreateWithInitialCommit(ctx, "owner", role)
	require.NoError(t, err)
	_, ok := readStoreRowForTest(t, db, schema, role.PlayerId, storepb.PlayerStoreKind_PLAYER_STORE_KIND_PROFILE)
	require.False(t, ok)
}

// 建角不再对 route_meta 加锁:有人持着这一行(维护事务、建池探针),建角照常提交,全服不排队。
// 冻结之后的新建角仍被拒:writable=0 时写者闸门和这里的普通读都不放行。
func TestTiDBCreateRoleDoesNotQueueOnRouteMetaAndFrozenStillRejects(t *testing.T) {
	s, db := accountDirectoryTiDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	role := &base.AccountSimplePlayer{PlayerId: 3111, ZoneId: 1}
	require.NoError(t, installInitialRouteForTest(s, ctx, role))

	holder, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer holder.Rollback()
	var writable uint64
	require.NoError(t, holder.QueryRowContext(ctx, "SELECT writable FROM route_meta WHERE id=1 FOR UPDATE").Scan(&writable))
	require.EqualValues(t, 1, writable)
	bounded, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	_, err = s.CreateWithInitialCommit(bounded, "route-meta-held", role)
	require.NoError(t, err, "route_meta 被锁时建角不排队")
	require.NoError(t, holder.Rollback())

	frozen := &base.AccountSimplePlayer{PlayerId: 3112, ZoneId: 1}
	require.NoError(t, installInitialRouteForTest(s, ctx, frozen))
	_, err = db.ExecContext(ctx, "UPDATE route_meta SET writable=0 WHERE id=1")
	require.NoError(t, err)
	t.Cleanup(func() {
		restore, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_, err := db.ExecContext(restore, "UPDATE route_meta SET writable=1 WHERE id=1")
		require.NoError(t, err)
	})
	_, err = s.CreateWithInitialCommit(ctx, "route-meta-frozen", frozen)
	require.Error(t, err, "冻结之后不能建角")
	var rows int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM account_role WHERE player_id=3112").Scan(&rows))
	require.Zero(t, rows)
}

func TestTiDBCreateRoleInitialRowCollisionRollsBackWholeCommit(t *testing.T) {
	s, db := accountDirectoryTiDB(t)
	_, schema := initialPlayerSchemaForTest(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	role := &base.AccountSimplePlayer{PlayerId: 3102, ZoneId: 1}
	require.NoError(t, installInitialRouteForTest(s, ctx, role))
	_, err := db.Exec("INSERT INTO `"+schema+"`.player_store (player_id,kind,owner_epoch,save_seq,schema_gen,holder_instance,released,payload) VALUES (?,?,7,9,?,?,0,?)",
		role.PlayerId, int32(storepb.PlayerStoreKind_PLAYER_STORE_KIND_HOT), persistschema.Gen, []byte{}, []byte{})
	require.NoError(t, err)
	out, err := s.CreateWithInitialCommit(ctx, "owner", role)
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	require.Nil(t, out)
	for _, query := range []string{
		"SELECT COUNT(*) FROM account_directory WHERE account='owner'",
		"SELECT COUNT(*) FROM account_role WHERE player_id=3102",
		"SELECT COUNT(*) FROM `" + schema + "`.player_store WHERE player_id=3102 AND kind=0",
	} {
		var count int
		require.NoError(t, db.QueryRow(query).Scan(&count))
		require.Zero(t, count, query)
	}
	row, ok := readStoreRowForTest(t, db, schema, role.PlayerId, storepb.PlayerStoreKind_PLAYER_STORE_KIND_HOT)
	require.True(t, ok)
	require.Equal(t, []uint64{7, 9}, []uint64{row.epoch, row.seq}, "严格 INSERT 不得重写旧行")
}

func TestTiDBCreateRoleConcurrentInitialRowsAreWrittenOnce(t *testing.T) {
	s, db := accountDirectoryTiDB(t)
	_, schema := initialPlayerSchemaForTest(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	role := &base.AccountSimplePlayer{PlayerId: 3103, ZoneId: 1}
	require.NoError(t, installInitialRouteForTest(s, ctx, role))
	type result struct {
		out *dspb.AccountDirectoryResponse
		err error
	}
	results := make(chan result, 8)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for n := 0; n < 8; n++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			out, err := s.CreateWithInitialCommit(ctx, "owner", proto.Clone(role).(*base.AccountSimplePlayer))
			results <- result{out, err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	for r := range results {
		require.NoError(t, r.err)
		require.Len(t, r.out.Players.Players, 1)
	}
	requireInitialRowsForTest(t, db, schema, role.PlayerId)
	var marker uint64
	require.NoError(t, db.QueryRow("SELECT initial_player_commit FROM account_role WHERE player_id=?", role.PlayerId).Scan(&marker))
	require.EqualValues(t, 1, marker)
}

func TestTiDBCreateRoleMissingInitialTableRollsBackWholeCommit(t *testing.T) {
	s, db := accountDirectoryTiDB(t)
	_, schema := initialPlayerSchemaForTest(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	role := &base.AccountSimplePlayer{PlayerId: 3110, ZoneId: 1}
	require.NoError(t, installInitialRouteForTest(s, ctx, role))
	_, err := db.Exec("DROP TABLE `" + schema + "`.player_store")
	require.NoError(t, err)
	out, err := s.CreateWithInitialCommit(ctx, "owner", role)
	require.Error(t, err)
	require.Nil(t, out)
	for _, query := range []string{
		"SELECT COUNT(*) FROM account_directory WHERE account='owner'",
		"SELECT COUNT(*) FROM account_role WHERE player_id=3110",
	} {
		var count int
		require.NoError(t, db.QueryRow(query).Scan(&count))
		require.Zero(t, count, query)
	}
}

func TestTiDBInitialCommitIsRequiredAndLegacyIsExplicit(t *testing.T) {
	baseStore, db := accountDirectoryTiDB(t)
	storage, schema := initialPlayerSchemaForTest(t, baseStore)
	cfg, err := mysql.ParseDSN(os.Getenv("ACCOUNT_DIRECTORY_TEST_DSN"))
	require.NoError(t, err)
	conf := MySQLConfig{Host: cfg.Addr, User: cfg.User, Password: cfg.Passwd, DBName: baseStore.schema}
	// 后端档位与交易 SQLProof 都不决定正式 Create 的持久提交。本用例连 TiDB,所以走 tidb 档:mysql 档会拒绝 TiDB。
	global, err := NewAccountDirectoryStore(conf, authoritysql.BackendTiDB, 2, 4)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, global.Close()) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	role := &base.AccountSimplePlayer{PlayerId: 3104, ZoneId: 1}
	_, err = global.CreateWithInitialCommit(ctx, "global", role)
	require.ErrorIs(t, err, accountdirectory.ErrUnavailable, "缺持久提交白名单必须拒绝")
	require.NoError(t, global.ConfigureInitialPlayerCommit([]uint32{storage}))
	_, err = global.CreateWithInitialCommit(ctx, "global", role)
	require.Error(t, err, "缺权威路由必须拒绝")
	require.NoError(t, installInitialRouteForTest(global, ctx, role))
	_, err = global.CreateWithInitialCommit(ctx, "global", role)
	require.NoError(t, err)
	requireInitialRowsForTest(t, db, schema, role.PlayerId)

}

func TestTiDBInitialCommitProofNeedsInitialRowsAndNoHistoricalFloor(t *testing.T) {
	for _, mode := range []string{"initial", "unmarked", "saved-row", "historical-floor"} {
		t.Run(mode, func(t *testing.T) {
			s, db := accountDirectoryTiDB(t)
			storage, schema := initialPlayerSchemaForTest(t, s)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, err := createInitialRoleForTest(s, ctx, "seller", &base.AccountSimplePlayer{PlayerId: 3107, ZoneId: 1})
			require.NoError(t, err)
			switch mode {
			case "unmarked":
				_, err = db.Exec("UPDATE account_role SET initial_player_commit=0 WHERE player_id=3107")
			case "saved-row":
				_, err = db.Exec("UPDATE `"+schema+"`.player_store SET save_seq=2 WHERE player_id=3107 AND kind=?", int32(storepb.PlayerStoreKind_PLAYER_STORE_KIND_HOT))
			case "historical-floor":
				admissions := NewAccountRoleAdmissionStore(s)
				_, err = admissions.Register(ctx, admissionTestRequest(3107, 1, 1))
				require.NoError(t, err)
				_, err = admissions.CloseAdmissions(ctx, &dspb.CloseRoleAdmissionsRequest{PlayerId: 3107, SceneInstanceId: admissionTestInstance, OwnerRevision: 1, ThroughGeneration: 1, RequiredOwnerEpoch: 17, RequiredSaveSeq: 3})
			}
			require.NoError(t, err)
			verifier, err := NewSQLRoleFreezeVerifier([]uint32{storage})
			require.NoError(t, err)
			trades := newTestAccountRoleTradeStore(s, verifier)
			_, err = trades.Begin(ctx, &dspb.BeginAccountRoleTradeRequest{ListingId: 3108, PlayerId: 3107, SellerAccount: "seller", ExpectedOwnerRevision: 1})
			require.NoError(t, err)
			out, err := trades.Confirm(ctx, &dspb.ConfirmAccountRoleTradeFreezeRequest{ListingId: 3108, PlayerId: 3107, SellerAccount: "seller", ExpectedOwnerRevision: 1})
			if mode == "initial" {
				require.NoError(t, err)
				require.Empty(t, out.Holders)
				require.Equal(t, dspb.AccountRoleFreezeEvidenceKind_ACCOUNT_ROLE_FREEZE_EVIDENCE_KIND_INITIAL_COMMIT, out.FreezeEvidence.Kind)
			} else {
				require.ErrorIs(t, err, accountdirectory.ErrConflict)
			}
		})
	}
}
