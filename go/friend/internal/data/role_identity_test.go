package data

import (
	"context"
	"testing"

	base "proto/common/base"
	dspb "proto/data_service"
	"shared/authoritysql"
	"shared/roleidentity"

	"github.com/stretchr/testify/require"
)

func TestOriginalRoleNeverFallsBackToUnconfiguredSystemRepo(t *testing.T) {
	repo := NewFriendRepo(nil, nil, FriendRepoOptions{})
	ctx, err := roleidentity.Capture(context.Background(), &dspb.AccountRoleOwnership{Account: "original", OwnerRevision: 7, Role: &base.AccountSimplePlayer{PlayerId: 22}})
	require.NoError(t, err)
	_, err = repo.CreateRequest(ctx, 22, 11, AddFriendLimits{})
	require.ErrorIs(t, err, ErrOriginalRole)
	_, err = repo.GetFriendList(ctx, 22)
	require.ErrorIs(t, err, ErrOriginalRole)
	require.ErrorIs(t, repo.Unblock(ctx, 22, 11), ErrOriginalRole)
	require.Error(t, repo.ConfigureRoleAuthority("bad.schema", authoritysql.BackendTiDB))
	repo.roleAuthority = "test_global"
	_, err = repo.GetFriendList(context.Background(), 22)
	require.ErrorIs(t, err, ErrOriginalRole)
	_, err = repo.GetFriendList(ctx, 11)
	require.ErrorIs(t, err, ErrOriginalRole)
}
