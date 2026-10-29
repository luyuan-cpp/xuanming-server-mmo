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
	"shared/accountdirectory"
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
	for _, table := range []string{"player_database", "player_database_1"} {
		var id, epoch, sequence, generation uint64
		require.NoError(t, db.QueryRow("SELECT player_id,fence_owner_epoch,fence_save_seq,persist_schema_gen FROM `"+schema+"`.`"+table+"` WHERE player_id=?", player).Scan(&id, &epoch, &sequence, &generation))
		require.Equal(t, []uint64{player, 1, 1, uint64(persistschema.Gen)}, []uint64{id, epoch, sequence, generation})
	}
}

func TestTiDBCreateRoleCommitsDirectoryAndBothInitialRowsOnce(t *testing.T) {
	s, db := accountDirectoryTiDB(t)
	_, schema := initialPlayerSchemaForTest(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	role := &base.AccountSimplePlayer{PlayerId: 3101, ZoneId: 1, Name: "初始提交"}
	require.NoError(t, installInitialRouteForTest(s, ctx, role))
	out, err := s.CreateWithInitialCommit(ctx, "owner", role)
	require.NoError(t, err)
	require.Len(t, out.Players.Players, 1)
	require.NotNil(t, out.InitialPlayerData)
	require.EqualValues(t, 1, out.InitialPlayerData.LastSaveSeq)
	require.EqualValues(t, 1, out.InitialPlayerData.PlayerDatabaseData.FenceOwnerEpoch)
	require.EqualValues(t, 1, out.InitialPlayerData.PlayerDatabase_1Data.FenceSaveSeq)
	require.Zero(t, out.InitialPlayerData.PlayerDatabaseData.GetUint64PbComponent().GetRegistrationTimestamp())
	requireInitialRowsForTest(t, db, schema, role.PlayerId)
	var marker uint64
	require.NoError(t, db.QueryRow("SELECT initial_player_commit FROM account_role WHERE player_id=?", role.PlayerId).Scan(&marker))
	require.EqualValues(t, 1, marker)

	// 原目录幂等不能重新制造缓存写资格，也不能补回后来丢失的 SQL 行。
	_, err = db.Exec("DELETE FROM `"+schema+"`.player_database_1 WHERE player_id=?", role.PlayerId)
	require.NoError(t, err)
	out, err = s.CreateWithInitialCommit(ctx, "owner", role)
	require.NoError(t, err)
	require.Nil(t, out.InitialPlayerData)
	var count int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM `"+schema+"`.player_database_1 WHERE player_id=?", role.PlayerId).Scan(&count))
	require.Zero(t, count)
}

func TestTiDBCreateRoleSecondInitialRowCollisionRollsBackWholeCommit(t *testing.T) {
	s, db := accountDirectoryTiDB(t)
	_, schema := initialPlayerSchemaForTest(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	role := &base.AccountSimplePlayer{PlayerId: 3102, ZoneId: 1}
	require.NoError(t, installInitialRouteForTest(s, ctx, role))
	_, err := db.Exec("INSERT INTO `"+schema+"`.player_database_1(player_id,fence_owner_epoch,fence_save_seq,persist_schema_gen) VALUES (?,7,9,?)", role.PlayerId, persistschema.Gen)
	require.NoError(t, err)
	out, err := s.CreateWithInitialCommit(ctx, "owner", role)
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	require.Nil(t, out)
	for _, query := range []string{
		"SELECT COUNT(*) FROM account_directory WHERE account='owner'",
		"SELECT COUNT(*) FROM account_role WHERE player_id=3102",
		"SELECT COUNT(*) FROM `" + schema + "`.player_database WHERE player_id=3102",
	} {
		var count int
		require.NoError(t, db.QueryRow(query).Scan(&count))
		require.Zero(t, count, query)
	}
	var epoch, sequence uint64
	require.NoError(t, db.QueryRow("SELECT fence_owner_epoch,fence_save_seq FROM `"+schema+"`.player_database_1 WHERE player_id=?", role.PlayerId).Scan(&epoch, &sequence))
	require.Equal(t, []uint64{7, 9}, []uint64{epoch, sequence}, "严格 INSERT 不得重写旧行")
}

func TestTiDBCreateRoleConcurrentInitialReceiptIsIssuedOnce(t *testing.T) {
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
	issued := 0
	for r := range results {
		require.NoError(t, r.err)
		require.Len(t, r.out.Players.Players, 1)
		if r.out.InitialPlayerData != nil {
			issued++
		}
	}
	require.Equal(t, 1, issued)
	requireInitialRowsForTest(t, db, schema, role.PlayerId)
}

func TestTiDBCreateRoleMissingSecondInitialTableRollsBackWholeCommit(t *testing.T) {
	s, db := accountDirectoryTiDB(t)
	_, schema := initialPlayerSchemaForTest(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	role := &base.AccountSimplePlayer{PlayerId: 3110, ZoneId: 1}
	require.NoError(t, installInitialRouteForTest(s, ctx, role))
	_, err := db.Exec("DROP TABLE `" + schema + "`.player_database_1")
	require.NoError(t, err)
	out, err := s.CreateWithInitialCommit(ctx, "owner", role)
	require.Error(t, err)
	require.Nil(t, out)
	for _, query := range []string{
		"SELECT COUNT(*) FROM account_directory WHERE account='owner'",
		"SELECT COUNT(*) FROM account_role WHERE player_id=3110",
		"SELECT COUNT(*) FROM `" + schema + "`.player_database WHERE player_id=3110",
	} {
		var count int
		require.NoError(t, db.QueryRow(query).Scan(&count))
		require.Zero(t, count, query)
	}
}

func TestTiDBInitialCommitIsRequiredWithoutTiDBProbeAndLegacyIsExplicit(t *testing.T) {
	baseStore, db := accountDirectoryTiDB(t)
	storage, schema := initialPlayerSchemaForTest(t, baseStore)
	cfg, err := mysql.ParseDSN(os.Getenv("ACCOUNT_DIRECTORY_TEST_DSN"))
	require.NoError(t, err)
	conf := MySQLConfig{Host: cfg.Addr, User: cfg.User, Password: cfg.Passwd, DBName: baseStore.schema}
	// dev/test 可关闭 TiDB 探针与交易 SQLProof，不能关闭正式 Create 的持久提交。
	global, err := NewAccountDirectoryStore(conf, false, 2, 4)
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
	out, err := global.CreateWithInitialCommit(ctx, "global", role)
	require.NoError(t, err)
	require.NotNil(t, out.InitialPlayerData)
	requireInitialRowsForTest(t, db, schema, role.PlayerId)

	legacy, err := NewLegacyAccountDirectoryStore(conf, false, 2, 4)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, legacy.Close()) })
	out, err = legacy.CreateWithInitialCommit(ctx, "legacy", &base.AccountSimplePlayer{PlayerId: 3105})
	require.NoError(t, err)
	require.Nil(t, out.InitialPlayerData, "Legacy 隔离路径不得发初始持久资格")
	var marker uint64
	require.NoError(t, db.QueryRow("SELECT initial_player_commit FROM account_role WHERE player_id=3105").Scan(&marker))
	require.Zero(t, marker)
	// 即便显式 Legacy 构造器，正式配置此能力之后也不能继续跳过初始提交。
	require.NoError(t, legacy.ConfigureInitialPlayerCommit([]uint32{storage}))
	_, err = legacy.CreateWithInitialCommit(ctx, "legacy", &base.AccountSimplePlayer{PlayerId: 3106, ZoneId: 1})
	require.Error(t, err)
}

func TestTiDBInitialCommitProofNeedsOriginPayloadAndNoHistoricalFloor(t *testing.T) {
	for _, mode := range []string{"initial", "unmarked", "payload", "historical-floor"} {
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
			case "payload":
				_, err = db.Exec("UPDATE `"+schema+"`.player_database SET `profile_component`=? WHERE player_id=3107", []byte{8, 1})
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

// Import/Restore 只管理目录，不把旧号数字或缺行误认成初始提交。
func TestTiDBImportAndRestoreNeverSeedInitialPlayerRows(t *testing.T) {
	s, db := accountDirectoryTiDB(t)
	_, schema := initialPlayerSchemaForTest(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	role := &base.AccountSimplePlayer{PlayerId: 3109, ZoneId: 1}
	_, err := s.ImportRole(ctx, "imported", role, false, roleImportTestID(31))
	require.NoError(t, err)
	require.NoError(t, s.Remove(ctx, "imported", []*dspb.AccountRoleVersion{{PlayerId: role.PlayerId, OwnerRevision: 1}}))
	_, err = s.ImportRole(ctx, "imported", role, true, roleImportTestID(32))
	require.NoError(t, err)
	var marker uint64
	require.NoError(t, db.QueryRow("SELECT initial_player_commit FROM account_role WHERE player_id=?", role.PlayerId).Scan(&marker))
	require.Zero(t, marker)
	for _, table := range []string{"player_database", "player_database_1"} {
		var count int
		require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM `"+schema+"`.`"+table+"` WHERE player_id=?", role.PlayerId).Scan(&count))
		require.Zero(t, count)
	}
	// 正式 Create 也不能复活或补发这个已导入角色的资格。
	require.NoError(t, installInitialRouteForTest(s, ctx, role))
	out, err := s.CreateWithInitialCommit(ctx, "imported", role)
	require.NoError(t, err)
	require.Nil(t, out.InitialPlayerData)
}
