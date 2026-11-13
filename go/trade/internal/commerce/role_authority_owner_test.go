package commerce

// 入口归属预检(RoleAuthority.VerifyOriginalOwner,docs/design/gate-online-identity.md §9.3)不连库的用例:
// 护栏副本用内存替身,目录用替身,所以随普通单测一起跑。真实 SQL 与合同事务里的加锁复核仍由 role_fence_tidb_test.go 覆盖。

import (
	"context"
	"errors"
	"testing"

	"shared/rolefence"
	"shared/rolefence/fencetest"

	"github.com/stretchr/testify/require"
)

func ownerPrecheckRoles(t *testing.T) (*RoleAuthority, *fencetest.MemoryReplica, *fencetest.Directory) {
	t.Helper()
	replica, directory := fencetest.NewMemoryReplica(t), fencetest.NewDirectory()
	roles, err := NewRoleAuthority(replica.DB(), directory)
	require.NoError(t, err)
	return roles, replica, directory
}

// 稳态:副本相符时每个请求只读一次本库,不读目录。浏览这类高频只读请求靠这一条。
func TestVerifyOriginalOwnerReadsOnlyTheLocalReplicaOnceItMatches(t *testing.T) {
	roles, replica, directory := ownerPrecheckRoles(t)
	directory.Set(11, "seller", 1, 0)
	seller := fenceActor(t, 11, "seller", 1)

	require.NoError(t, roles.VerifyOriginalOwner(seller))
	require.Equal(t, []uint64{11}, directory.Reads, "副本缺行:读一次目录写回副本")
	readsAfterHeal := len(replica.Reads())

	require.NoError(t, roles.VerifyOriginalOwner(seller))
	require.Equal(t, []uint64{11}, directory.Reads, "副本相符不读目录")
	require.Len(t, replica.Reads(), readsAfterHeal+1, "副本相符只读一次本库")
}

// 卖家把自己这名角色挂了人民币寄售:角色是冻结的,但仍归原身份,查询与重放要照常放行(合同写入由 lockRoles 另判)。
// 冻结的副本可能停在过户之前,所以先读一次目录:目录说已经过户,旧主就被拒。
func TestVerifyOriginalOwnerAllowsRoleFrozenByItsOwnListingButNotAfterTransfer(t *testing.T) {
	roles, replica, directory := ownerPrecheckRoles(t)
	seller, buyer := fenceActor(t, 11, "seller", 1), fenceActor(t, 11, "buyer", 2)
	replica.Put(rolefence.Fence{PlayerID: 11, Account: "seller", OwnerRevision: 1, FreezeToken: 77, FreezeFinal: true, Version: 1})
	directory.Set(11, "seller", 1, 77)

	require.NoError(t, roles.VerifyOriginalOwner(seller), "冻结期间角色仍归原身份")
	require.Equal(t, []uint64{11}, directory.Reads)

	// 过户提交、推送丢了:副本还停在「卖家 + 冻结」。
	directory.Set(11, "buyer", 2, 0)
	err := roles.VerifyOriginalOwner(seller)
	require.ErrorIs(t, err, ErrConflict)
	require.True(t, OwnerRejected(err))
	require.NoError(t, roles.VerifyOriginalOwner(buyer))

	reads := len(directory.Reads)
	require.True(t, OwnerRejected(roles.VerifyOriginalOwner(seller)))
	require.Len(t, directory.Reads, reads, "副本修订号已经更大:旧主直接拒绝,不读目录")
}

// 删角、目录里没有这名角色、ctx 里没有原身份:都是确定不归。
func TestVerifyOriginalOwnerRejectsDeletedUnknownAndAnonymousCallers(t *testing.T) {
	roles, replica, _ := ownerPrecheckRoles(t)
	replica.Put(rolefence.Fence{PlayerID: 11, OwnerRevision: 2})
	require.True(t, OwnerRejected(roles.VerifyOriginalOwner(fenceActor(t, 11, "seller", 1))), "删角墓碑")

	err := roles.VerifyOriginalOwner(fenceActor(t, 404, "ghost", 1))
	require.ErrorIs(t, err, ErrConflict, "目录里没有这名角色映射成本服务的拒绝错误")
	require.True(t, OwnerRejected(err))

	require.True(t, OwnerRejected(roles.VerifyOriginalOwner(context.Background())), "没有原身份")
	require.True(t, OwnerRejected((*RoleAuthority)(nil).VerifyOriginalOwner(fenceActor(t, 11, "seller", 1))), "没装配护栏不能算通过")
}

// 目录读不到、库故障:这一次判不了,不能当成角色变了(入口回 Aborted,客户端重试)。
func TestVerifyOriginalOwnerIsInconclusiveWhenDirectoryOrStorageFails(t *testing.T) {
	roles, replica, directory := ownerPrecheckRoles(t)
	seller := fenceActor(t, 11, "seller", 1)

	directory.Err = errors.Join(rolefence.ErrUnavailable, errors.New("data_service down"))
	err := roles.VerifyOriginalOwner(seller)
	require.ErrorIs(t, err, rolefence.ErrUnavailable)
	require.False(t, OwnerRejected(err))

	boom := errors.New("sql failed")
	replica.FailWith(boom)
	err = roles.VerifyOriginalOwner(seller)
	require.ErrorIs(t, err, boom)
	require.False(t, OwnerRejected(err))
}
