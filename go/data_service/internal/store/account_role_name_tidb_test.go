package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	base "proto/common/base"
	dspb "proto/data_service"
	"shared/accountdirectory"
	"shared/dirshard"
	"shared/placement"
)

// 真库上的「预留 → 绑定」状态机(docs/design/guild-phase2/03-names.md §3.4a):K=4 夹具,名字片和玩家片不是同一片。
// 开关同 account_directory_shard_tidb_test.go(ACCOUNT_DIRECTORY_TEST_DSN),只建/删自己的随机库。
//
// 片号由 shared/dirshard 金样钉住:玩家 12 在第 0 片,13 在第 1 片,14 在第 2 片,11 在第 3 片。

// nameStoreForTest 是夹具上的名字登记表(普通写者,和 svc 装配同一份连接参数)。
func nameStoreForTest(t *testing.T, f *directoryShardFixture) *PlayerNameStore {
	t.Helper()
	names, err := NewPlayerNameStore(f.Conf)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, names.Close()) })
	return names
}

// namedDirectoryForTest 是接上名字登记表的目录(生产装配同一个接法)和名字登记表本身。
func namedDirectoryForTest(t *testing.T, f *directoryShardFixture) (*AccountDirectoryStore, *PlayerNameStore) {
	t.Helper()
	names := nameStoreForTest(t, f)
	directory := f.directory(t, 5)
	directory.ConfigurePlayerNames(names)
	return directory, names
}

// reserveForTest 照 login 第 ① 步登记名字,回判重键。
func reserveForTest(t *testing.T, ctx context.Context, names *PlayerNameStore, player uint64, name string) residualName {
	t.Helper()
	key, ok := residualNameKeyOf(name)
	require.True(t, ok)
	outcome, _, err := names.Reserve(ctx, player, key.display, key.norm, uint64(time.Now().UnixMilli()))
	require.NoError(t, err)
	require.Contains(t, []ReserveOutcome{ReserveInserted, ReserveAlreadyOwned}, outcome)
	return key
}

// nameRowForTest 直接读名字片上的名字行:登记在谁名下、绑定时刻。
func nameRowForTest(t *testing.T, ctx context.Context, f *directoryShardFixture, norm string) (holder, boundMs uint64, found bool) {
	t.Helper()
	err := f.Raw[dirshard.SlotOfName(norm, shardFixtureK)].QueryRowContext(ctx, "SELECT player_id,bound_ms FROM player_name WHERE name_norm=?", norm).Scan(&holder, &boundMs)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, false
	}
	require.NoError(t, err)
	return holder, boundMs, true
}

func ownerRowsForTest(t *testing.T, ctx context.Context, f *directoryShardFixture, player uint64) int {
	t.Helper()
	var n int
	require.NoError(t, f.Raw[dirshard.SlotOfPlayer(player, shardFixtureK)].QueryRowContext(ctx, "SELECT COUNT(*) FROM player_name_owner WHERE player_id=?", player).Scan(&n))
	return n
}

func removalRowsForTest(t *testing.T, ctx context.Context, f *directoryShardFixture, player uint64) int {
	t.Helper()
	var n int
	require.NoError(t, f.Raw[dirshard.SlotOfPlayer(player, shardFixtureK)].QueryRowContext(ctx, "SELECT COUNT(*) FROM account_role_removal WHERE player_id=?", player).Scan(&n))
	return n
}

// 建角提交之后名字行置为已绑定;之后建角回滚和运维清孤儿两条释放路径都被拒,名字行与名字副本一个字节都不动。
// 用例「已绑定的名字不可被释放」。
func TestTiDBCommittedRoleBindsItsNameAndReleaseIsRefused(t *testing.T) {
	f := newDirectoryShardFixture(t)
	d, names := namedDirectoryForTest(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	key := reserveForTest(t, ctx, names, 12, "BindA")
	holder, boundMs, found := nameRowForTest(t, ctx, f, key.norm)
	require.True(t, found)
	require.EqualValues(t, 12, holder)
	require.Zero(t, boundMs, "登记时是预留态")

	_, err := createInitialRoleForTest(d, ctx, shardSeller, &base.AccountSimplePlayer{PlayerId: 12, ZoneId: 1, Name: "BindA"})
	require.NoError(t, err)
	holder, boundMs, found = nameRowForTest(t, ctx, f, key.norm)
	require.True(t, found)
	require.EqualValues(t, 12, holder)
	require.NotZero(t, boundMs, "提交之后置为已绑定")

	seal, err := d.SealUncommittedRole(ctx, 12)
	require.NoError(t, err)
	require.Equal(t, RoleSealCommitted, seal, "证明那一步就拒:角色已提交")
	for _, minCreatedMs := range []uint64{1, 0} {
		outcome, err := names.Release(ctx, 12, key.norm, minCreatedMs)
		require.NoError(t, err)
		require.Equal(t, ReleaseBound, outcome, "min_created_ms=%d:已绑定的名字带不带运维 token 都删不掉", minCreatedMs)
	}
	holder, _, found = nameRowForTest(t, ctx, f, key.norm)
	require.True(t, found)
	require.EqualValues(t, 12, holder)
	require.Equal(t, 1, ownerRowsForTest(t, ctx, f, 12), "名字副本也不动")

	outcome, owner, err := names.Reserve(ctx, 13, "BINDA", key.norm, uint64(time.Now().UnixMilli()))
	require.NoError(t, err)
	require.Equal(t, ReserveTaken, outcome, "别的角色登记不了同一个名字")
	require.EqualValues(t, 12, owner)
}

// 名字行还是预留态(提交之后的绑定没做完),角色却已提交:证明那一步拒绝释放,名字行留着。
// 用例「预留态在角色已提交后被拒绝释放」。
func TestTiDBReservedNameOfCommittedRoleIsNotReleased(t *testing.T) {
	f := newDirectoryShardFixture(t)
	names := nameStoreForTest(t, f)
	unbound := f.directory(t, 5) // 没接名字登记表:提交之后不绑定,摆出「绑定没做完」
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	key := reserveForTest(t, ctx, names, 13, "Pending")
	_, err := createInitialRoleForTest(unbound, ctx, shardSeller, &base.AccountSimplePlayer{PlayerId: 13, ZoneId: 1, Name: "Pending"})
	require.NoError(t, err)
	_, boundMs, found := nameRowForTest(t, ctx, f, key.norm)
	require.True(t, found)
	require.Zero(t, boundMs)

	seal, err := unbound.SealUncommittedRole(ctx, 13)
	require.NoError(t, err)
	require.Equal(t, RoleSealCommitted, seal)
	owned, err := unbound.Lookup(ctx, 13)
	require.NoError(t, err, "证明一个字节都不写:角色还在")
	require.Equal(t, shardSeller, owned.GetAccount())
	holder, _, found := nameRowForTest(t, ctx, f, key.norm)
	require.True(t, found)
	require.EqualValues(t, 13, holder)
}

// 没有角色行、有路由行:证明写墓碑,迟到的建角在提交点撞上墓碑被拒,预留态的名字随后能释放。
// 没有路由行:提交点要求路由行,此刻提交不了,证明不写墓碑。
func TestTiDBSealBlocksALateCreateBeforeTheNameIsReleased(t *testing.T) {
	f := newDirectoryShardFixture(t)
	d, names := namedDirectoryForTest(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	role := &base.AccountSimplePlayer{PlayerId: 14, ZoneId: 1, Name: "LateOne"}
	key := reserveForTest(t, ctx, names, 14, role.Name)
	require.NoError(t, installInitialRouteForTest(d, ctx, role))

	seal, err := d.SealUncommittedRole(ctx, 14)
	require.NoError(t, err)
	require.Equal(t, RoleSealSealed, seal)
	_, err = d.Create(ctx, shardSeller, role)
	require.ErrorIs(t, err, accountdirectory.ErrConflict, "迟到的建角撞上墓碑,不能提交")
	_, err = d.Lookup(ctx, 14)
	require.ErrorIs(t, err, accountdirectory.ErrMissing)

	outcome, err := names.Release(ctx, 14, key.norm, 1)
	require.NoError(t, err)
	require.Equal(t, ReleaseDeleted, outcome)
	_, _, found := nameRowForTest(t, ctx, f, key.norm)
	require.False(t, found)

	seal, err = d.SealUncommittedRole(ctx, 14)
	require.NoError(t, err)
	require.Equal(t, RoleSealRemoved, seal, "第二次释放读到第一次写的墓碑,不再写")

	seal, err = d.SealUncommittedRole(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, RoleSealUnrouted, seal)
	var rows int
	require.NoError(t, f.Raw[dirshard.SlotOfPlayer(11, shardFixtureK)].QueryRowContext(ctx, "SELECT COUNT(*) FROM account_role WHERE player_id=?", 11).Scan(&rows))
	require.Zero(t, rows, "没有路由行时不写墓碑")
}

// 建角提交之后名字绑定没做完:同一建角的重放(预读那条路)补上;核账工具的 name_unbound 同样补得上。
// 用例「建角提交后名字绑定失败可由重放或核账补上」。
func TestTiDBUnboundNameIsBoundByCreateReplayOrResidualRepair(t *testing.T) {
	f := newDirectoryShardFixture(t)
	d, names := namedDirectoryForTest(t, f)
	unbound := f.directory(t, 5)
	auditor, _ := residualAuditorOnFixture(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	replayed := &base.AccountSimplePlayer{PlayerId: 12, ZoneId: 1, Name: "ReplayMe"}
	replayKey := reserveForTest(t, ctx, names, 12, replayed.Name)
	_, err := createInitialRoleForTest(unbound, ctx, shardSeller, replayed)
	require.NoError(t, err)
	repaired := &base.AccountSimplePlayer{PlayerId: 13, ZoneId: 1, Name: "AuditMe"}
	repairKey := reserveForTest(t, ctx, names, 13, repaired.Name)
	_, err = createInitialRoleForTest(unbound, ctx, shardSeller, repaired)
	require.NoError(t, err)

	// 重放:角色行已在,走预读那条路,补绑名字,不再碰存档。
	_, err = d.Create(ctx, shardSeller, replayed)
	require.NoError(t, err)
	_, boundMs, _ := nameRowForTest(t, ctx, f, replayKey.norm)
	require.NotZero(t, boundMs, "重放补上了绑定")

	// 核账:plan 报 name_unbound,apply 补上,再 plan 干净。
	require.Equal(t, []string{"13 name_unbound planned"}, residualOutline(residualRunAllShards(t, ctx, auditor, CreateResidualRepairs{})))
	require.Equal(t, []string{"13 name_unbound repaired"}, residualOutline(residualRunAllShards(t, ctx, auditor, CreateResidualRepairs{Names: true})))
	_, boundMs, _ = nameRowForTest(t, ctx, f, repairKey.norm)
	require.NotZero(t, boundMs)
	require.Empty(t, residualOutline(residualRunAllShards(t, ctx, auditor, residualRepairAll)))
}

// failingNames 是释放总失败的名字登记表,摆「名字片不可达」。
type failingNames struct{ roleNameRegistry }

func (failingNames) ReleaseRemoved(context.Context, uint64, string) (ReleaseOutcome, error) {
	return 0, errors.New("name shard unreachable")
}

// 删角写完墓碑之后释放名字,名字副本一起删,别的角色随即能登记;释放失败时小表那一行留着,后台 ResumeRemovals 接着放。
// 用例「删角后名字可释放」。
func TestTiDBRemovalReleasesTheNameAndTheResumerRetries(t *testing.T) {
	f := newDirectoryShardFixture(t)
	d, names := namedDirectoryForTest(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	key := reserveForTest(t, ctx, names, 12, "GoneSoon")
	_, err := createInitialRoleForTest(d, ctx, shardSeller, &base.AccountSimplePlayer{PlayerId: 12, ZoneId: 1, Name: "GoneSoon"})
	require.NoError(t, err)
	require.NoError(t, d.Remove(ctx, shardSeller, []*dspb.AccountRoleVersion{{PlayerId: 12, OwnerRevision: 1}}))
	_, _, found := nameRowForTest(t, ctx, f, key.norm)
	require.False(t, found, "墓碑之后名字释放了")
	require.Zero(t, ownerRowsForTest(t, ctx, f, 12))
	require.Zero(t, removalRowsForTest(t, ctx, f, 12), "收尾做完,小表那一行删了")
	outcome, _, err := names.Reserve(ctx, 13, "GoneSoon", key.norm, uint64(time.Now().UnixMilli()))
	require.NoError(t, err)
	require.Equal(t, ReserveInserted, outcome, "删角之后别的角色能登记这个名字")
	require.NoError(t, d.Remove(ctx, shardSeller, []*dspb.AccountRoleVersion{{PlayerId: 12, OwnerRevision: 1}}), "相同删除幂等")
	holder, _, found := nameRowForTest(t, ctx, f, key.norm)
	require.True(t, found)
	require.EqualValues(t, 13, holder, "重放只按墓碑里那名角色删,不碰别人后来登记的同名")

	// 名字片不可达:删角照样成功(墓碑已提交),名字留在已删的角色名下,小表那一行留着。
	key = reserveForTest(t, ctx, names, 14, "Stuck")
	_, err = createInitialRoleForTest(d, ctx, shardSeller, &base.AccountSimplePlayer{PlayerId: 14, ZoneId: 1, Name: "Stuck"})
	require.NoError(t, err)
	d.names = failingNames{}
	require.NoError(t, d.Remove(ctx, shardSeller, []*dspb.AccountRoleVersion{{PlayerId: 14, OwnerRevision: 1}}))
	_, err = d.Lookup(ctx, 14)
	require.ErrorIs(t, err, accountdirectory.ErrMissing)
	holder, boundMs, found := nameRowForTest(t, ctx, f, key.norm)
	require.True(t, found)
	require.EqualValues(t, 14, holder)
	require.NotZero(t, boundMs)
	require.Equal(t, 1, removalRowsForTest(t, ctx, f, 14))
	outcome, _, err = names.Reserve(ctx, 11, "Stuck", key.norm, uint64(time.Now().UnixMilli()))
	require.NoError(t, err)
	require.Equal(t, ReserveTaken, outcome, "没释放之前谁都登记不了:不会一名两角")

	d.names = names
	_, err = f.Raw[dirshard.SlotOfPlayer(14, shardFixtureK)].ExecContext(ctx, "UPDATE account_role_removal SET started_ms=started_ms-600000 WHERE player_id=?", 14)
	require.NoError(t, err)
	result, err := d.ResumeRemovals(ctx, 5*time.Minute, 10)
	require.NoError(t, err)
	require.Equal(t, RemovalResumeResult{Finished: 1}, result)
	_, _, found = nameRowForTest(t, ctx, f, key.norm)
	require.False(t, found, "后台收尾放掉了名字")
	require.Zero(t, removalRowsForTest(t, ctx, f, 14))
}

// 用例「判陈旧不给提交过的角色写墓碑」的真库一半:摆出玩家片恢复点早于建角提交(角色行没了,ref 退回 PENDING、超过宽限)。
// 名字已绑定(12)、存档存过盘(13)的都不写墓碑,ref 留着等核账;名字还是预留态、存档只有初始行原样的(14)照常写墓碑清掉。
func TestTiDBStaleCreateRefOfACommittedRoleIsNotTombstoned(t *testing.T) {
	f := newDirectoryShardFixture(t)
	d, names := namedDirectoryForTest(t, f)
	unbound := f.directory(t, 5) // 没接名字登记表:提交之后不绑定
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	playerSchema, ok := placement.StoreDBName(f.Storage)
	require.True(t, ok)
	storeDB, err := f.SQL.OpenSchema(ctx, playerSchema)
	require.NoError(t, err)

	reserveForTest(t, ctx, names, 12, "KeptName")
	_, err = createInitialRoleForTest(d, ctx, shardSeller, &base.AccountSimplePlayer{PlayerId: 12, ZoneId: 1, Name: "KeptName"})
	require.NoError(t, err)
	for player, name := range map[uint64]string{13: "SavedOnce", 14: "NeverIn"} {
		reserveForTest(t, ctx, names, player, name)
		_, err = createInitialRoleForTest(unbound, ctx, shardSeller, &base.AccountSimplePlayer{PlayerId: player, ZoneId: 1, Name: name})
		require.NoError(t, err)
	}
	_, err = storeDB.ExecContext(ctx, "UPDATE player_store SET save_seq=save_seq+1 WHERE player_id=?", 13)
	require.NoError(t, err)

	for _, player := range []uint64{12, 13, 14} {
		_, err = f.Raw[dirshard.SlotOfPlayer(player, shardFixtureK)].ExecContext(ctx, "DELETE FROM account_role WHERE player_id=?", player)
		require.NoError(t, err)
		_, err = f.Raw[dirshard.SlotOfAccount(shardSeller, shardFixtureK)].ExecContext(ctx,
			"UPDATE account_role_ref SET state=?,created_ms=created_ms-? WHERE account=? AND player_id=?",
			refStatePending, refPendingGrace.Milliseconds()+1000, []byte(shardSeller), player)
		require.NoError(t, err)
	}

	result, err := d.SweepStaleRefs(ctx, refPendingGrace, 64)
	require.NoError(t, err)
	require.Equal(t, SweepResult{Scanned: 3, Dropped: 1, Held: 2}, result)
	for player, want := range map[uint64]int{12: 0, 13: 0, 14: 1} {
		var rows int
		require.NoError(t, f.Raw[dirshard.SlotOfPlayer(player, shardFixtureK)].QueryRowContext(ctx, "SELECT COUNT(*) FROM account_role WHERE player_id=?", player).Scan(&rows))
		require.Equalf(t, want, rows, "player=%d:提交过的不写墓碑,没有任何痕迹的照常写", player)
	}
	var refs int
	require.NoError(t, f.Raw[dirshard.SlotOfAccount(shardSeller, shardFixtureK)].QueryRowContext(ctx,
		"SELECT COUNT(*) FROM account_role_ref WHERE account=?", []byte(shardSeller)).Scan(&refs))
	require.Equal(t, 2, refs, "留下的两条 ref 还在,等核账")
}
