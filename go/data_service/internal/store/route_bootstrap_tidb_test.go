package store

import (
	"context"
	"testing"
	"time"

	"github.com/luyuancpp/proto2mysql"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	base "proto/common/base"
	dbpb "proto/common/database"
	"shared/placement"
)

func TestTiDBEmptyRouteBootstrapDoesNotTreatExistingPlayerHistoryAsFresh(t *testing.T) {
	d, db, fixture := accountDirectoryTiDBWithWriter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// 复用随机自有storage及正规PB建表；额外两表与GoDB TablesFromJSON的正式迁移布局相同。
	id, name, rows := roleProofStorage(t, db, 11, fixture)
	model := proto2mysql.NewDB()
	require.NoError(t, model.OpenDB(rows, name))
	for _, row := range []proto.Message{&dbpb.PlayerCentreDatabase{}, &dbpb.UserAccounts{}} {
		model.RegisterTable(row)
		require.NoError(t, model.CreateOrUpdateTable(row))
	}
	_, err := db.ExecContext(ctx, "DELETE FROM player_route WHERE route_shard=? AND player_id=?", placement.RouteRowShard(11), 11)
	require.NoError(t, err)
	route := &RouteStore{db: db}
	any, err := route.HasAnyRoute(ctx)
	require.NoError(t, err)
	require.False(t, any)
	history, err := route.HasHistoricalPlayers(ctx, []uint32{id})
	require.NoError(t, err)
	require.True(t, history, "缓存/路由空不能抹掉真实玩家历史")
	_, err = rows.ExecContext(ctx, "DELETE FROM player_store WHERE player_id=11")
	require.NoError(t, err)
	history, err = route.HasHistoricalPlayers(ctx, []uint32{id})
	require.NoError(t, err)
	require.False(t, history)
	require.NoError(t, model.Insert(&dbpb.UserAccounts{Account: "password-only", Password: "fixture-password-hash"}))
	history, err = route.HasHistoricalPlayers(ctx, []uint32{id})
	require.NoError(t, err)
	require.False(t, history, "仅有密码账户且角色目录为空不能冒称玩家历史")
	require.NoError(t, model.Insert(&dbpb.PlayerCentreDatabase{PlayerId: 33}))
	history, err = route.HasHistoricalPlayers(ctx, []uint32{id})
	require.NoError(t, err)
	require.True(t, history, "只有正规centre历史也不能按新世界Arm")
	_, err = rows.ExecContext(ctx, "DELETE FROM player_centre_database WHERE player_id=33")
	require.NoError(t, err)
	require.NoError(t, model.Insert(&dbpb.UserAccounts{Account: "legacy-role", SimplePlayers: &base.AccountSimplePlayerList{Players: []*base.AccountSimplePlayer{{PlayerId: 44}}}}))
	history, err = route.HasHistoricalPlayers(ctx, []uint32{id})
	require.NoError(t, err)
	require.True(t, history, "旧SQL角色目录不能因新目录/player_store 为空而漏掉")
	_, err = rows.ExecContext(ctx, "DELETE FROM user_accounts WHERE account='legacy-role'")
	require.NoError(t, err)
	_, err = rows.ExecContext(ctx, "UPDATE user_accounts SET simple_players=? WHERE account='password-only'", []byte{0xff})
	require.NoError(t, err)
	history, err = route.HasHistoricalPlayers(ctx, []uint32{id})
	require.NoError(t, err)
	require.True(t, history, "非空未知旧目录不是无角色证明")
	_, err = rows.ExecContext(ctx, "UPDATE user_accounts SET simple_players=NULL WHERE account='password-only'")
	require.NoError(t, err)
	_, err = createInitialRoleForTest(d, ctx, "original-owner", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 22})
	require.NoError(t, err)
	history, err = route.HasHistoricalPlayers(ctx, []uint32{id})
	require.NoError(t, err)
	require.True(t, history, "目录有原角色也不能按新环境Arm")
	_, err = db.ExecContext(ctx, "DELETE FROM account_role WHERE player_id=22")
	require.NoError(t, err)
	_, err = rows.ExecContext(ctx, "DROP TABLE player_centre_database")
	require.NoError(t, err)
	_, err = route.HasHistoricalPlayers(ctx, []uint32{id})
	require.Error(t, err, "缺正规centre表不是空历史证明")
	require.NoError(t, model.CreateOrUpdateTable(&dbpb.PlayerCentreDatabase{}))
	_, err = rows.ExecContext(ctx, "DROP TABLE user_accounts")
	require.NoError(t, err)
	_, err = route.HasHistoricalPlayers(ctx, []uint32{id})
	require.Error(t, err, "缺正规旧目录表不是空历史证明")
	require.NoError(t, model.CreateOrUpdateTable(&dbpb.UserAccounts{}))
	_, err = rows.ExecContext(ctx, "DROP TABLE player_store")
	require.NoError(t, err)
	_, err = route.HasHistoricalPlayers(ctx, []uint32{id})
	require.Error(t, err, "缺正规schema不是空历史证明")
}
