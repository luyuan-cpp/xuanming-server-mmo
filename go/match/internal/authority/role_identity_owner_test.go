package authority

// 入口归属预检(Store.VerifyOriginalOwner,docs/design/gate-online-identity.md §9.3)不连库的用例:
// 护栏副本用内存替身,目录用替身,所以随普通单测一起跑。真实 SQL 与提交点的加锁复核仍由 role_identity_native_test.go 的 TiDB 用例覆盖。

import (
	"context"
	"errors"
	"testing"

	"shared/rolefence"
	"shared/rolefence/fencetest"

	"github.com/stretchr/testify/require"
)

func ownerPrecheckStore(t *testing.T) (*Store, *fencetest.MemoryReplica, *fencetest.Directory) {
	t.Helper()
	replica, directory := fencetest.NewMemoryReplica(t), fencetest.NewDirectory()
	s := &Store{DB: replica.DB()}
	s.fence = rolefence.NewGuard(replica.DB(), directory)
	return s, replica, directory
}

// 稳态:副本相符时每个请求只读一次本库,不读目录。副本缺行只在第一次读一次目录。
func TestVerifyOriginalOwnerReadsOnlyTheLocalReplicaOnceItMatches(t *testing.T) {
	s, replica, directory := ownerPrecheckStore(t)
	ctx := context.Background()
	directory.Set(11, "seller", 7, 0)
	seller := nativeRoleActor(t, ctx, 11, "seller", 7)

	require.NoError(t, s.VerifyOriginalOwner(seller))
	require.Equal(t, []uint64{11}, directory.Reads, "副本缺行:读一次目录写回副本")
	readsAfterHeal := len(replica.Reads())

	require.NoError(t, s.VerifyOriginalOwner(seller))
	require.Equal(t, []uint64{11}, directory.Reads, "副本相符不读目录")
	require.Len(t, replica.Reads(), readsAfterHeal+1, "副本相符只读一次本库")
}

// 过户、删角、目录里没有这名角色、ctx 里没有原身份:都是确定不归,入口回 PermissionDenied。
func TestVerifyOriginalOwnerRejectsRolesThatNoLongerBelongToTheSession(t *testing.T) {
	s, replica, directory := ownerPrecheckStore(t)
	ctx := context.Background()
	seller := nativeRoleActor(t, ctx, 11, "seller", 7)

	replica.Put(rolefence.Fence{PlayerID: 11, Account: "buyer", OwnerRevision: 8})
	err := s.VerifyOriginalOwner(seller)
	require.ErrorIs(t, err, ErrOriginalPlayer)
	require.True(t, OwnerRejected(err))
	require.Empty(t, directory.Reads, "旧主的会话直接拒绝,不读目录")
	require.NoError(t, s.VerifyOriginalOwner(nativeRoleActor(t, ctx, 11, "buyer", 8)), "新主通过")
	require.True(t, OwnerRejected(s.VerifyOriginalOwner(nativeRoleActor(t, ctx, 11, "other", 8))), "修订号相同账号不同")

	replica.Put(rolefence.Fence{PlayerID: 11, OwnerRevision: 9})
	require.True(t, OwnerRejected(s.VerifyOriginalOwner(nativeRoleActor(t, ctx, 11, "buyer", 8))), "删角墓碑")

	err = s.VerifyOriginalOwner(nativeRoleActor(t, ctx, 404, "ghost", 1))
	require.ErrorIs(t, err, ErrOriginalPlayer, "目录里没有这名角色映射成本服务的拒绝错误")
	require.True(t, OwnerRejected(err))

	require.True(t, OwnerRejected(s.VerifyOriginalOwner(ctx)), "没有原身份")
}

// 冻结不拦读,但冻结的副本可能停在过户之前:先读一次目录再判。目录说还是他就放行,目录说已经过户就拒绝。
func TestVerifyOriginalOwnerAllowsFrozenRoleAfterConfirmingWithTheDirectory(t *testing.T) {
	s, replica, directory := ownerPrecheckStore(t)
	ctx := context.Background()
	seller := nativeRoleActor(t, ctx, 11, "seller", 7)
	replica.Put(rolefence.Fence{PlayerID: 11, Account: "seller", OwnerRevision: 7, FreezeToken: 901, FreezeFinal: true, Version: 1})
	directory.Set(11, "seller", 7, 901)

	require.NoError(t, s.VerifyOriginalOwner(seller), "冻结期间角色仍归原身份")
	require.Equal(t, []uint64{11}, directory.Reads)

	// 过户提交、推送丢了:副本还停在「旧主 + 冻结」。
	directory.Set(11, "buyer", 8, 0)
	require.ErrorIs(t, s.VerifyOriginalOwner(seller), ErrOriginalPlayer)
	row, ok := replica.Row(11)
	require.True(t, ok)
	require.Equal(t, "buyer", row.Account, "副本按目录更新")
}

// 目录读不到、库故障:这一次判不了,不能当成角色变了(入口回 Aborted,客户端重试)。
func TestVerifyOriginalOwnerIsInconclusiveWhenDirectoryOrStorageFails(t *testing.T) {
	s, replica, directory := ownerPrecheckStore(t)
	seller := nativeRoleActor(t, context.Background(), 11, "seller", 7)

	directory.Err = errors.Join(rolefence.ErrUnavailable, errors.New("data_service down"))
	err := s.VerifyOriginalOwner(seller)
	require.ErrorIs(t, err, rolefence.ErrUnavailable)
	require.False(t, OwnerRejected(err))

	boom := errors.New("sql failed")
	replica.FailWith(boom)
	err = s.VerifyOriginalOwner(seller)
	require.ErrorIs(t, err, boom)
	require.False(t, OwnerRejected(err))
}

// 没装配护栏不能算通过:独立夹具里的 Store 没有 fence,预检一律拒绝。
func TestVerifyOriginalOwnerWithoutFenceRejects(t *testing.T) {
	seller := nativeRoleActor(t, context.Background(), 11, "seller", 7)
	require.ErrorIs(t, (&Store{}).VerifyOriginalOwner(seller), ErrOriginalPlayer)
	require.ErrorIs(t, (*Store)(nil).VerifyOriginalOwner(seller), ErrOriginalPlayer)
}
