package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
	base "proto/common/base"
	"shared/authoritysql"
	"shared/dirshard"
	"shared/placement"
)

// 真库上的建角残留核账:K=4 夹具上照现行流程建好角色(先登记名字,再走目录的建角),然后按「各库恢复点错开」删掉对应的行,
// 跑 plan、apply、plan。开关同 account_directory_shard_tidb_test.go(ACCOUNT_DIRECTORY_TEST_DSN)。
//
// 工具用维护身份连库(夹具的 admin):维护身份不过写者闸门,修复的事务里自己锁闸门行,所以这里也用这个身份,不用夹具的普通写者。

func residualAuditorOnFixture(t *testing.T, f *directoryShardFixture) (*CreateResidualAuditor, *PlayerNameStore) {
	t.Helper()
	dsn, err := f.SQL.OperatorDSN(dirshard.DirectoryDBName(f.Base, 0, shardFixtureK))
	require.NoError(t, err)
	admin, err := mysql.ParseDSN(dsn)
	require.NoError(t, err)
	conf := MySQLConfig{Host: admin.Addr, User: admin.User, Password: admin.Passwd, DBName: f.Base, MaxOpenConn: 4, MaxIdleConn: 2,
		LogicalShards: shardFixtureK, Backend: authoritysql.BackendTiDB, Permission: f.SQL.OperatorPermission()}
	directory, err := NewAccountDirectoryStore(conf, authoritysql.BackendTiDB, 5, 4)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, directory.Close()) })
	require.NoError(t, directory.ConfigurePlayerStores([]uint32{f.Storage}, PlayerStorePoolOptions{MaxOpenConn: 2, MaxIdleConn: 1}))
	names, err := NewPlayerNameStore(conf)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, names.Close()) })
	auditor, err := NewCreateResidualAuditor(directory, names, time.Now)
	require.NoError(t, err)
	return auditor, names
}

// residualRunAllShards 逐片跑完一轮,结论按 player 排序。
func residualRunAllShards(t *testing.T, ctx context.Context, a *CreateResidualAuditor, repairs CreateResidualRepairs) []CreateResidualFinding {
	t.Helper()
	var findings []CreateResidualFinding
	for slot := 0; slot < a.Shards(); slot++ {
		report, err := a.Run(ctx, CreateResidualScan{Slot: slot, Batch: 2, Repair: repairs})
		require.NoError(t, err)
		require.True(t, report.Done)
		findings = append(findings, report.Findings...)
	}
	sort.SliceStable(findings, func(i, j int) bool { return findings[i].PlayerID < findings[j].PlayerID })
	return findings
}

// createNamedRoleForTest 照 login 的顺序建角:先登记名字(第 ① 步),再走目录建角(路由、ref、初始行、角色行、名字置为已绑定、ref 激活)。
// 目录接上名字登记表才会在提交之后绑定(生产由 svc 装配时接,这里在建角之前接,重复接无害)。
func createNamedRoleForTest(t *testing.T, ctx context.Context, writer *AccountDirectoryStore, names *PlayerNameStore, account string, player uint64, name string) {
	t.Helper()
	key, ok := residualNameKeyOf(name)
	require.True(t, ok)
	_, _, err := names.Reserve(ctx, player, key.display, key.norm, uint64(time.Now().UnixMilli()))
	require.NoError(t, err)
	writer.ConfigurePlayerNames(names)
	_, err = createInitialRoleForTest(writer, ctx, account, &base.AccountSimplePlayer{PlayerId: player, ZoneId: 1, Name: name})
	require.NoError(t, err)
}

func TestTiDBCreateResidualRepairsSkewedRecovery(t *testing.T) {
	f := newDirectoryShardFixture(t)
	writer := f.directory(t, 5)
	auditor, names := residualAuditorOnFixture(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	playerSchema, ok := placement.StoreDBName(f.Storage)
	require.True(t, ok)
	storeDB, err := f.SQL.OpenSchema(ctx, playerSchema)
	require.NoError(t, err)

	// 四名角色分在四片上,账号分在两片上。
	createNamedRoleForTest(t, ctx, writer, names, shardSeller, 12, "ResA")
	createNamedRoleForTest(t, ctx, writer, names, shardSeller, 13, "ResB")
	createNamedRoleForTest(t, ctx, writer, names, shardBuyer, 14, "ResC")
	createNamedRoleForTest(t, ctx, writer, names, shardBuyer, 11, "ResD")
	owned, err := writer.Lookup(ctx, 12)
	require.NoError(t, err)
	require.True(t, owned.GetInitialPlayerCommit(), "现行建角流程提交的角色带初始提交标记,摘孤儿凭它拒绝写墓碑")
	require.Empty(t, residualOutline(residualRunAllShards(t, ctx, auditor, CreateResidualRepairs{})), "完整建成的角色没有残留")

	// 各库恢复点错开:账号片早于写 ref(A),名字片早于登记名字(B,玩家片上的名字副本还在),存档库早于写初始行(C)。
	_, err = f.Raw[dirshard.SlotOfAccount(shardSeller, shardFixtureK)].ExecContext(ctx, "DELETE FROM account_role_ref WHERE account=? AND player_id=?", []byte(shardSeller), 12)
	require.NoError(t, err)
	_, err = f.Raw[dirshard.SlotOfName("resb", shardFixtureK)].ExecContext(ctx, "DELETE FROM player_name WHERE name_norm=?", "resb")
	require.NoError(t, err)
	_, err = storeDB.ExecContext(ctx, "DELETE FROM player_store WHERE player_id=?", 14)
	require.NoError(t, err)

	require.Equal(t, []string{
		"12 ref_missing planned",
		"13 name_missing planned",
		"14 initial_rows_missing planned",
	}, residualOutline(residualRunAllShards(t, ctx, auditor, CreateResidualRepairs{})))
	roles, err := writer.Read(ctx, shardSeller, false)
	require.NoError(t, err)
	require.Len(t, roles.Players, 1, "plan 不写:A 还不在列表里")

	require.Equal(t, []string{
		"12 ref_missing repaired",
		"13 name_missing repaired",
		"14 initial_rows_missing repaired",
	}, residualOutline(residualRunAllShards(t, ctx, auditor, residualRepairAll)))

	roles, err = writer.Read(ctx, shardSeller, false)
	require.NoError(t, err)
	require.Len(t, roles.Players, 2, "补回 ref 之后 A 回到列表")
	holder, err := residualNameHolderForTest(ctx, f, "resb")
	require.NoError(t, err)
	require.EqualValues(t, 13, holder)
	want, err := initialPlayerStoreRows(&base.AccountSimplePlayer{PlayerId: 14, ZoneId: 1, Name: "ResC"})
	require.NoError(t, err)
	rows, err := readPlayerStoreProofRows(ctx, storeDB, 14, false)
	require.NoError(t, err)
	require.True(t, sameInitialRows(rows, want), "重写的就是建角时那几行初始行")

	require.Empty(t, residualOutline(residualRunAllShards(t, ctx, auditor, residualRepairAll)), "修过之后重放什么都不剩")
}

// 名字在恢复里丢了,之后被别的角色登记:只报告,两边一个字节都不动;裁决按建角时刻(路由行 created_at_ms)。
func TestTiDBCreateResidualNameTakenLeavesBothSidesUntouched(t *testing.T) {
	f := newDirectoryShardFixture(t)
	writer := f.directory(t, 5)
	auditor, names := residualAuditorOnFixture(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	createNamedRoleForTest(t, ctx, writer, names, shardSeller, 21, "ResE")
	_, err := f.Raw[dirshard.SlotOfName("rese", shardFixtureK)].ExecContext(ctx, "DELETE FROM player_name WHERE name_norm=?", "rese")
	require.NoError(t, err)
	createNamedRoleForTest(t, ctx, writer, names, shardBuyer, 22, "ResE")
	// 夹具的路由行时刻是常数,这里改开:21 先建,22 后建。
	for player, createdMs := range map[uint64]uint64{21: 100, 22: 200} {
		_, err = f.Raw[dirshard.SlotOfPlayer(player, shardFixtureK)].ExecContext(ctx, "UPDATE player_route SET created_at_ms=? WHERE route_shard=? AND player_id=?",
			createdMs, placement.RouteRowShard(player), player)
		require.NoError(t, err)
	}

	findings := residualRunAllShards(t, ctx, auditor, residualRepairAll)
	require.Equal(t, []string{"21 name_taken manual name_held_by_other"}, residualOutline(findings))
	require.EqualValues(t, 22, findings[0].Holder)
	require.True(t, findings[0].HolderLive)
	require.EqualValues(t, 21, findings[0].Keeper, "21 建得早,按「建角早者留名」该它留名")

	holder, err := residualNameHolderForTest(ctx, f, "rese")
	require.NoError(t, err)
	require.EqualValues(t, 22, holder, "现登记者不动")
	var ownedNorm string
	require.NoError(t, f.Raw[dirshard.SlotOfPlayer(21, shardFixtureK)].QueryRowContext(ctx, "SELECT name_norm FROM player_name_owner WHERE player_id=?", 21).Scan(&ownedNorm))
	require.Equal(t, "rese", ownedNorm, "本角色的名字副本也不动")
}

func residualNameHolderForTest(ctx context.Context, f *directoryShardFixture, norm string) (uint64, error) {
	var holder uint64
	err := f.Raw[dirshard.SlotOfName(norm, shardFixtureK)].QueryRowContext(ctx, "SELECT player_id FROM player_name WHERE name_norm=?", norm).Scan(&holder)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return holder, err
}
