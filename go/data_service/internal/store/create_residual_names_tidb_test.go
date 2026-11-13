package store

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	base "proto/common/base"
	dspb "proto/data_service"
	"shared/dirshard"
)

// 真库上的名字登记表两种残留(create_residual.go 文件头):删角的名字还登记着(removed_role_name)、孤儿名字(orphan_name)。
// 开关同 account_directory_shard_tidb_test.go(ACCOUNT_DIRECTORY_TEST_DSN),只建/删自己的随机库;工具用维护身份(同 create_residual_tidb_test.go)。
//
// 片号由 shared/dirshard 金样钉住:玩家 12 在第 0 片,13 在第 1 片,14 在第 2 片,11 在第 3 片。

// residualRunAllNameShards 逐片扫一轮名字行,结论按 player 排序。
func residualRunAllNameShards(t *testing.T, ctx context.Context, a *CreateResidualAuditor, repairs CreateResidualRepairs) []CreateResidualFinding {
	t.Helper()
	var findings []CreateResidualFinding
	for slot := 0; slot < a.Shards(); slot++ {
		report, err := a.Run(ctx, CreateResidualScan{Slot: slot, Target: ResidualTargetNames, Batch: 2, Repair: repairs})
		require.NoError(t, err)
		require.True(t, report.Done)
		findings = append(findings, report.Findings...)
	}
	sort.SliceStable(findings, func(i, j int) bool { return findings[i].PlayerID < findings[j].PlayerID })
	return findings
}

// 用例「名字片恢复点早于删角释放」:删角写完墓碑、收尾放掉了名字,名字片却恢复到释放之前,名字行(已绑定)和名字副本又回到已删的角色名下,
// 小表那一行早删了。角色行扫描连墓碑一起扫,按墓碑里的原名报 removed_role_name;apply 先复核墓碑,再按 (player_id, 判重键) 条件删,
// 删完删名字缓存,别的角色随即能登记这个名字。重放什么都不剩。
func TestTiDBCreateResidualReleasesTheNameOfARemovedRole(t *testing.T) {
	f := newDirectoryShardFixture(t)
	writer, names := namedDirectoryForTest(t, f)
	auditor, _, cache := residualAuditorWithCacheOnFixture(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	key := reserveForTest(t, ctx, names, 12, "GoneTwice")
	_, err := createInitialRoleForTest(writer, ctx, shardSeller, &base.AccountSimplePlayer{PlayerId: 12, ZoneId: 1, Name: "GoneTwice"})
	require.NoError(t, err)
	require.NoError(t, writer.Remove(ctx, shardSeller, []*dspb.AccountRoleVersion{{PlayerId: 12, OwnerRevision: 1}}))
	_, _, found := nameRowForTest(t, ctx, f, key.norm)
	require.False(t, found, "删角收尾放掉了名字")

	// 名字片恢复到释放之前:名字行(已绑定)和名字副本又回来了。
	_, err = f.Raw[dirshard.SlotOfName(key.norm, shardFixtureK)].ExecContext(ctx,
		"INSERT INTO player_name (player_id, name, name_norm, created_ms, bound_ms) VALUES (?,?,?,?,?)", 12, key.display, key.norm, 1, 2)
	require.NoError(t, err)
	_, err = f.Raw[dirshard.SlotOfPlayer(12, shardFixtureK)].ExecContext(ctx,
		"INSERT INTO player_name_owner (player_id, name_norm, name) VALUES (?,?,?)", 12, key.norm, key.display)
	require.NoError(t, err)

	require.Equal(t, []string{"12 removed_role_name planned"}, residualOutline(residualRunAllShards(t, ctx, auditor, CreateResidualRepairs{})))
	require.Equal(t, []string{"12 removed_role_name repaired"}, residualOutline(residualRunAllShards(t, ctx, auditor, residualRepairAll)))
	_, _, found = nameRowForTest(t, ctx, f, key.norm)
	require.False(t, found, "名字行删了")
	require.Zero(t, ownerRowsForTest(t, ctx, f, 12), "名字副本删了")
	require.Equal(t, []uint64{12}, cache.deleted, "删完删名字缓存")
	outcome, _, err := names.Reserve(ctx, 13, "GoneTwice", key.norm, uint64(time.Now().UnixMilli()))
	require.NoError(t, err)
	require.Equal(t, ReserveInserted, outcome, "别的角色能登记这个名字")

	require.Empty(t, residualOutline(residualRunAllShards(t, ctx, auditor, residualRepairAll)), "重放什么都不剩")
}

// 用例「玩家片冻结期间回滚释放失败留下的孤儿名字,解冻之后由核账工具收尾」:建角回滚时释放之前的证明写不了墓碑,名字行留在预留态。
//   - 13:回滚在登记路由之前(没有路由行、没有角色行):证明不写墓碑(提交要求路由行,提交不了),只删预留态的名字行。
//   - 14:回滚在登记路由之后(有路由行、没有角色行、存档只有初始行、没有 ref):证明写墓碑,迟到的建角提交不了,再删名字行。
//   - 11:有路由行、没有角色行,但这名角色在某个账号上有 ACTIVE 的 ref(玩家片恢复点早于提交):不证明、不删,只报告。
//   - 15:刚登记的预留态(可能是在途的建角):不报。
//
// apply 之后删名字缓存,重放只剩 11。
func TestTiDBCreateResidualSettlesOrphanNameReservations(t *testing.T) {
	f := newDirectoryShardFixture(t)
	writer, names := namedDirectoryForTest(t, f)
	auditor, _, cache := residualAuditorWithCacheOnFixture(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	old := uint64(time.Now().Add(-2 * ResidualOrphanNameMinAge).UnixMilli())
	reserveAt := func(player uint64, name string, nowMs uint64) residualName {
		key, ok := residualNameKeyOf(name)
		require.True(t, ok)
		outcome, _, err := names.Reserve(ctx, player, key.display, key.norm, nowMs)
		require.NoError(t, err)
		require.Equal(t, ReserveInserted, outcome)
		return key
	}
	unrouted := reserveAt(13, "OrphanA", old)
	routed := reserveAt(14, "OrphanB", old)
	committed := reserveAt(11, "OrphanC", old)
	fresh := reserveAt(15, "OrphanD", uint64(time.Now().UnixMilli()))
	for _, player := range []uint64{14, 11} {
		require.NoError(t, installInitialRouteForTest(writer, ctx, &base.AccountSimplePlayer{PlayerId: player, ZoneId: 1}))
	}
	_, err := f.Raw[dirshard.SlotOfAccount(shardSeller, shardFixtureK)].ExecContext(ctx,
		"INSERT INTO account_role_ref(account,player_id,state,tx_id,listing_id,home_zone,nonce,created_ms) VALUES (?,?,?,0,0,0,5,1)",
		[]byte(shardSeller), 11, refStateActive)
	require.NoError(t, err)

	require.Equal(t, []string{
		"11 orphan_name manual ref_active",
		"13 orphan_name planned",
		"14 orphan_name planned",
	}, residualOutline(residualRunAllNameShards(t, ctx, auditor, CreateResidualRepairs{})), "plan 只报告;刚登记的不报")

	require.Equal(t, []string{
		"11 orphan_name manual ref_active",
		"13 orphan_name repaired",
		"14 orphan_name repaired",
	}, residualOutline(residualRunAllNameShards(t, ctx, auditor, residualRepairAll)))
	for _, key := range []residualName{unrouted, routed} {
		_, _, found := nameRowForTest(t, ctx, f, key.norm)
		require.False(t, found, "孤儿名字删了:%s", key.norm)
	}
	for _, key := range []residualName{committed, fresh} {
		_, _, found := nameRowForTest(t, ctx, f, key.norm)
		require.True(t, found, "有提交证据的、刚登记的都不动:%s", key.norm)
	}
	roleRows := func(player uint64) int {
		var n int
		require.NoError(t, f.Raw[dirshard.SlotOfPlayer(player, shardFixtureK)].QueryRowContext(ctx, "SELECT COUNT(*) FROM account_role WHERE player_id=?", player).Scan(&n))
		return n
	}
	require.Equal(t, 1, roleRows(14), "有路由行:先写墓碑,迟到的建角提交不了")
	require.Zero(t, roleRows(13), "没有路由行:提交不了,不写墓碑")
	require.Zero(t, roleRows(11), "有提交证据:不证明")
	require.ElementsMatch(t, []uint64{13, 14}, cache.deleted, "名字删掉之后删名字缓存")

	require.Equal(t, []string{"11 orphan_name manual ref_active"}, residualOutline(residualRunAllNameShards(t, ctx, auditor, residualRepairAll)), "重放只剩要人看的那一项")
}
