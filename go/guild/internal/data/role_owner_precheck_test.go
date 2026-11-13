package data

// 入口归属预检(GuildRepo.CheckOriginalRole,docs/design/gate-online-identity.md §9.3)不连库的用例:
// 护栏副本用内存替身,目录用替身,所以随普通单测一起跑。真实 SQL 与写事务里的加锁复核仍由 role_fence_tidb_test.go 覆盖。

import (
	"context"
	"errors"
	"testing"

	"shared/rolefence"
	"shared/rolefence/fencetest"

	"github.com/stretchr/testify/require"
)

func ownerPrecheckRepo(t *testing.T) (*GuildRepo, *fencetest.MemoryReplica, *fencetest.Directory) {
	t.Helper()
	replica, directory := fencetest.NewMemoryReplica(t), fencetest.NewDirectory()
	repo := &GuildRepo{db: replica.DB()}
	repo.fence = rolefence.NewGuard(replica.DB(), directory)
	return repo, replica, directory
}

// 稳态:副本相符时每个请求只读一次本库,不读目录。
func TestCheckOriginalRoleReadsOnlyTheLocalReplicaOnceItMatches(t *testing.T) {
	repo, replica, directory := ownerPrecheckRepo(t)
	directory.Set(11, "seller", 1, 0)
	seller := lobbyActor(t, context.Background(), 11, "seller", 1)

	require.NoError(t, repo.CheckOriginalRole(seller))
	require.Equal(t, []uint64{11}, directory.Reads, "副本缺行:读一次目录写回副本")
	readsAfterHeal := len(replica.Reads())

	require.NoError(t, repo.CheckOriginalRole(seller))
	require.Equal(t, []uint64{11}, directory.Reads, "副本相符不读目录")
	require.Len(t, replica.Reads(), readsAfterHeal+1, "副本相符只读一次本库")
}

// 预检只判归属,与 match、trade 同一个判据:冻结的角色读得到自己的帮会数据(写由提交点的 rolefence.Check 拒绝)。
// 冻结的副本可能停在过户之前,所以先读一次目录:目录说已经过户,旧主就被拒。
func TestCheckOriginalRoleAllowsFrozenRoleButNotAfterTransfer(t *testing.T) {
	repo, replica, directory := ownerPrecheckRepo(t)
	ctx := context.Background()
	seller, buyer := lobbyActor(t, ctx, 11, "seller", 1), lobbyActor(t, ctx, 11, "buyer", 2)
	replica.Put(rolefence.Fence{PlayerID: 11, Account: "seller", OwnerRevision: 1, FreezeToken: 901, FreezeFinal: true, Version: 1})
	directory.Set(11, "seller", 1, 901)

	require.NoError(t, repo.CheckOriginalRole(seller), "冻结期间角色仍归原身份")
	require.Equal(t, []uint64{11}, directory.Reads)

	directory.Set(11, "buyer", 2, 0)
	require.ErrorIs(t, repo.CheckOriginalRole(seller), ErrWriteConflict, "过户之后旧主被拒")
	require.NoError(t, repo.CheckOriginalRole(buyer))
}

// 删角回本服务的拒绝错误;目录里没有这名角色回 RoleNotFound,由装配层的 guildOwnerRejected 一并判成拒绝。
func TestCheckOriginalRoleRejectsDeletedAndUnknownRoles(t *testing.T) {
	repo, replica, _ := ownerPrecheckRepo(t)
	ctx := context.Background()
	replica.Put(rolefence.Fence{PlayerID: 11, OwnerRevision: 2})
	require.ErrorIs(t, repo.CheckOriginalRole(lobbyActor(t, ctx, 11, "seller", 1)), ErrWriteConflict, "删角墓碑")

	err := repo.CheckOriginalRole(lobbyActor(t, ctx, 404, "ghost", 1))
	require.ErrorIs(t, err, rolefence.ErrRoleNotFound)
}

// 目录读不到、库故障:这一次判不了,不能当成角色变了(入口回 Aborted,客户端重试)。
func TestCheckOriginalRoleIsInconclusiveWhenDirectoryOrStorageFails(t *testing.T) {
	repo, replica, directory := ownerPrecheckRepo(t)
	seller := lobbyActor(t, context.Background(), 11, "seller", 1)

	directory.Err = errors.Join(rolefence.ErrUnavailable, errors.New("data_service down"))
	err := repo.CheckOriginalRole(seller)
	require.ErrorIs(t, err, rolefence.ErrUnavailable)
	require.NotErrorIs(t, err, ErrWriteConflict)
	require.NotErrorIs(t, err, rolefence.ErrRoleNotFound)

	boom := errors.New("sql failed")
	replica.FailWith(boom)
	err = repo.CheckOriginalRole(seller)
	require.ErrorIs(t, err, boom)
	require.NotErrorIs(t, err, ErrWriteConflict)
}
