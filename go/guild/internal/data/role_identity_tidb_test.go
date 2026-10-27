package data

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net"
	"os"
	"shared/authoritysql/sqltest"
	"strconv"
	"testing"
	"time"

	base "proto/common/base"
	dbpb "proto/common/database"
	dspb "proto/data_service"
	pb "proto/guild"
	matchpb "proto/match"
	teampb "proto/team"
	"shared/roleidentity"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-sql-driver/mysql"
	"github.com/luyuancpp/proto2mysql"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// 只建删本例随机库，所有表来自正式PB；本例验证guild提交接缝，DS真实Begin并发由目录跨域例覆盖。
func guildRoleTiDB(t *testing.T) (context.Context, *sql.DB, *GuildRepo, *ActivityRepo) {
	t.Helper()
	dsn := os.Getenv("GUILD_ROLE_TEST_DSN")
	if dsn == "" {
		if os.Getenv("GUILD_ROLE_REQUIRE_TIDB") == "1" {
			t.Fatal("缺少GUILD_ROLE_TEST_DSN")
		}
		t.Skip("未配置独占guild角色权威TiDB")
	}
	cfg, err := mysql.ParseDSN(dsn)
	require.NoError(t, err)
	host, port, err := net.SplitHostPort(cfg.Addr)
	require.NoError(t, err)
	require.True(t, host == "localhost" || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback())
	portNumber, err := strconv.Atoi(port)
	require.NoError(t, err)
	require.True(t, portNumber > 0 && portNumber <= 65535, "只接受显式本地数值端口；本任务ownedTiDB为24000")
	random := make([]byte, 8)
	_, err = rand.Read(random)
	require.NoError(t, err)
	schema := "guild_role_it_" + hex.EncodeToString(random)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	opts, err := sqltest.OptionsFromEnv(dsn, schema+"_writer", schema)
	require.NoError(t, err)
	fixture, err := sqltest.New(ctx, t, opts)
	require.NoError(t, err)
	db, err := fixture.OpenSchema(ctx, schema)
	require.NoError(t, err)
	model := proto2mysql.NewDB()
	require.NoError(t, model.OpenDB(db, schema))
	tables := append(Tables(), []proto.Message{&dbpb.AccountRole{}, &matchpb.MatchAuthorityTicketRecord{}, &teampb.TeamLaunchRecord{}, &teampb.TeamLaunchRoleRecord{}}...)
	for _, row := range tables {
		model.RegisterTable(row)
		require.NoError(t, model.CreateOrUpdateTable(row))
	}
	createGuildRouteFixture(t, ctx, db)
	require.NoError(t, fixture.GrantProtoTables(ctx, schema, tables))
	require.NoError(t, fixture.GrantTables(ctx, schema, "route_meta", "player_route", "zone_merge_fence", "zone_merged_into", "route_reconcile"))
	db, err = fixture.Open(ctx, schema, 8)
	require.NoError(t, err)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	repo := NewGuildRepo(rdb, db, time.Minute)
	require.NoError(t, repo.ConfigureRoleAuthority(schema))
	require.NoError(t, repo.ConfigureMatchAuthority(schema))
	activities, err := NewActivityRepo(repo)
	require.NoError(t, err)
	return ctx, db, repo, activities
}
func TestTiDBGuildCommitRechecksActorAndTrialOriginalsAndRetainsFrozenOwed(t *testing.T) {
	ctx, db, repo, activities := guildRoleTiDB(t)
	seedManagedGuild(t, ctx, db, 44, 1, 1, 30, 11, map[uint64]uint32{22: 1})
	for _, p := range []uint64{11, 22} {
		_, err := db.ExecContext(ctx, "INSERT INTO account_role(player_id,account,role_proto,owner_revision,updated_ms,trade_listing_id) VALUES(?,'seller',?,1,1,0)", p, []byte{})
		require.NoError(t, err)
	}
	originalCtx, err := roleidentity.Capture(ctx, &dspb.AccountRoleOwnership{Account: "seller", OwnerRevision: 1, Role: &base.AccountSimplePlayer{PlayerId: 11}})
	require.NoError(t, err)
	require.NoError(t, repo.UpdateAnnouncementAuthorized(originalCtx, 44, 11, "original"))
	_, err = db.ExecContext(ctx, "UPDATE account_role SET account='buyer',owner_revision=2 WHERE player_id=11")
	require.NoError(t, err)
	require.ErrorIs(t, repo.UpdateAnnouncementAuthorized(originalCtx, 44, 11, "must-not-commit"), ErrWriteConflict)
	var text string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT announcement FROM guild WHERE guild_id=44").Scan(&text))
	require.Equal(t, "original", text)
	_, err = db.ExecContext(ctx, "UPDATE account_role SET account='seller',owner_revision=1 WHERE player_id=11")
	require.NoError(t, err)
	battle := actTrialKey(actTrialRow(), 900, 44, 11, testNowMs)
	battle.LaunchRequestID = 51
	roster := []*pb.GuildTrialRoleIdentity{{PlayerId: 11, Account: "seller", OwnerRevision: 1}, {PlayerId: 22, Account: "seller", OwnerRevision: 1}}
	consentedSQLLobby(t, originalCtx, repo, 51, 44, battle.ActivityID, 11, 22)
	prepared, err := activities.PrepareTrialLaunch(originalCtx, 51, battle, 1, roster, testNowMs)
	require.NoError(t, err)
	require.NotNil(t, prepared)
	_, err = db.ExecContext(ctx, "UPDATE account_role SET account='buyer',owner_revision=2 WHERE player_id=22")
	require.NoError(t, err)
	require.ErrorIs(t, activities.RegisterTrialBattle(ctx, battle, testNowMs), ErrWriteConflict)
	_, err = db.ExecContext(ctx, "UPDATE account_role SET account='seller',owner_revision=1 WHERE player_id=22")
	require.NoError(t, err)
	require.NoError(t, activities.RegisterTrialBattle(ctx, battle, testNowMs))
	expired, err := activities.ExpireTrialBattle(ctx, 900, 44, testNowMs+1)
	require.NoError(t, err)
	require.True(t, expired)
	var active int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_trial_launch_role WHERE state=1").Scan(&active))
	require.Equal(t, 2, active, "EXPIRED仍可能收到迟到结果，不能当安全撤销")
	_, err = activities.SettleTrialBattleTx(ctx, TrialSettleInput{Battle: battle, Activity: actTrialRow(), Candidates: []uint64{11, 22}, NowMs: testNowMs + 2})
	require.NoError(t, err)
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_trial_launch_role WHERE state=1").Scan(&active))
	require.Zero(t, active)
	battle.BattleID = 902
	battle.LaunchRequestID = 52
	consentedSQLLobby(t, originalCtx, repo, 52, 44, battle.ActivityID, 11, 22)
	prepared, err = activities.PrepareTrialLaunch(originalCtx, 52, battle, 1, roster, testNowMs+3)
	require.NoError(t, err)
	require.NoError(t, activities.FinishCancelledTrialLaunch(ctx, prepared, &matchpb.CancelActivityBattleResponse{RequestCancelled: true, RequestDigest: "fixture-verifier-covered-separately"}, testNowMs+4))
	row, _, err := activities.TrialLaunch(ctx, 52)
	require.NoError(t, err)
	require.EqualValues(t, 3, row.State)
	require.NotEmpty(t, row.CancelReceipt)
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_trial_launch_role WHERE state=1").Scan(&active))
	require.Zero(t, active)
	_, err = db.ExecContext(ctx, "INSERT INTO guild_trial_reward_owed("+owedRewardColumns+") VALUES(22,901,44,3,20231001,?,0,?)", []byte{1}, testNowMs)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "UPDATE account_role SET trade_listing_id=99 WHERE player_id=22")
	require.NoError(t, err)
	_, err = activities.ConvertOwedReward(ctx, OwedReward{PlayerID: 22, BattleID: 901, GuildID: 44, ActivityID: 3, PeriodKey: 20231001, Payload: []byte{1}}, 999, testNowMs+3)
	require.ErrorIs(t, err, ErrWriteConflict)
	var owed, ops int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_trial_reward_owed WHERE player_id=22").Scan(&owed))
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_asset_op WHERE player_id=22").Scan(&ops))
	require.Equal(t, 1, owed)
	require.Zero(t, ops)
}
