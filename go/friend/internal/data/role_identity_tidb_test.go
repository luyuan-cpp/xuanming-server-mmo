package data

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	base "proto/common/base"
	dspb "proto/data_service"
	"shared/authoritysql"
	"shared/roleidentity"

	"github.com/stretchr/testify/require"
)

func roleIdentityTiDBPair(t *testing.T) (*FriendRepo, *FriendRepo, context.Context, string) {
	t.Helper()
	first, second, ctx := friendTiDBReplicaPair(t)
	schema := fmt.Sprintf("friend_identity_%x", time.Now().UnixNano())
	_, err := first.db.ExecContext(ctx, "CREATE DATABASE `"+schema+"`")
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, dropErr := first.db.ExecContext(cleanup, "DROP DATABASE `"+schema+"`")
		require.NoError(t, dropErr)
	})
	_, err = first.db.ExecContext(ctx, "CREATE TABLE `"+schema+"`.account_role(player_id BIGINT UNSIGNED PRIMARY KEY,account VARBINARY(191) NOT NULL,owner_revision BIGINT UNSIGNED NOT NULL,trade_listing_id BIGINT UNSIGNED NOT NULL DEFAULT 0)")
	require.NoError(t, err)
	_, err = first.db.ExecContext(ctx, "INSERT INTO `"+schema+"`.account_role(player_id,account,owner_revision) VALUES(11,'peer',3),(22,'original',7)")
	require.NoError(t, err)
	require.NoError(t, first.ConfigureRoleAuthority(schema, authoritysql.BackendTiDB))
	require.NoError(t, second.ConfigureRoleAuthority(schema, authoritysql.BackendTiDB))
	return first, second, ctx, schema
}

func roleIdentityContext(t *testing.T, parent context.Context, player uint64, account string, revision uint64) context.Context {
	t.Helper()
	ctx, err := roleidentity.Capture(parent, &dspb.AccountRoleOwnership{Account: account, OwnerRevision: revision, Role: &base.AccountSimplePlayer{PlayerId: player}})
	require.NoError(t, err)
	return ctx
}

func TestTiDBFriendOriginalRole_AllClientReadsAndWritesRejectTransferABAFrozen(t *testing.T) {
	repo, _, ctx, schema := roleIdentityTiDBPair(t)
	original := roleIdentityContext(t, ctx, 22, "original", 7)
	for _, change := range []string{"account='next',owner_revision=8", "account='original',owner_revision=9", "account='original',owner_revision=7,trade_listing_id=81"} {
		_, err := repo.db.ExecContext(ctx, "UPDATE `"+schema+"`.account_role SET "+change+" WHERE player_id=22")
		require.NoError(t, err)
		for name, operation := range map[string]func() error{
			"add":       func() error { _, err := repo.CreateRequest(original, 22, 11, AddFriendLimits{}); return err },
			"accept":    func() error { _, err := repo.AcceptRequest(original, 123, 22, 10); return err },
			"reject":    func() error { _, err := repo.RejectRequest(original, 123, 22); return err },
			"remove":    func() error { _, err := repo.RemoveFriend(original, 22, 11); return err },
			"block":     func() error { _, err := repo.Block(original, 22, 11, 10); return err },
			"unblock":   func() error { return repo.Unblock(original, 22, 11) },
			"friends":   func() error { _, err := repo.GetFriendList(original, 22); return err },
			"pending":   func() error { _, err := repo.GetPendingRequests(original, 22); return err },
			"blocks":    func() error { _, err := repo.ListBlocks(original, 22); return err },
			"mutual":    func() error { _, err := repo.RecommendByMutual(original, 22, nil, 10); return err },
			"random":    func() error { _, err := repo.RecommendRandom(original, 22, nil, 10); return err },
			"directory": func() error { return repo.CheckOriginalRole(original) },
		} {
			require.ErrorIs(t, operation(), ErrOriginalRole, name)
		}
		for _, table := range []string{"friend", "friend_request", "friend_capacity", "friend_block"} {
			require.Zero(t, mustCount(t, ctx, repo.db, "SELECT COUNT(*) FROM "+table), table)
		}
	}
}

func TestTiDBFriendOriginalRole_ReplicasPreserveRelationsAndFrozenTarget(t *testing.T) {
	first, second, ctx, schema := roleIdentityTiDBPair(t)
	original := roleIdentityContext(t, ctx, 22, "original", 7)
	peer := roleIdentityContext(t, ctx, 11, "peer", 3)
	request, err := first.CreateRequest(peer, 11, 22, AddFriendLimits{})
	require.NoError(t, err)
	accepted, err := second.AcceptRequest(original, request.RequestID, 22, 10)
	require.NoError(t, err)
	require.True(t, accepted.Changed)
	friends, err := first.GetFriendList(original, 22)
	require.NoError(t, err)
	require.Len(t, friends, 1)
	require.EqualValues(t, 11, friends[0].FriendPlayerID)
	for _, change := range []string{"account='next',owner_revision=8", "account='original',owner_revision=9"} {
		_, err = second.db.ExecContext(ctx, "UPDATE `"+schema+"`.account_role SET "+change+" WHERE player_id=22")
		require.NoError(t, err)
		_, err = first.RemoveFriend(original, 22, 11)
		require.ErrorIs(t, err, ErrOriginalRole)
		_, err = second.GetFriendList(original, 22)
		require.ErrorIs(t, err, ErrOriginalRole)
		require.EqualValues(t, 2, mustCount(t, ctx, first.db, "SELECT COUNT(*) FROM friend"))
	}
	current := roleIdentityContext(t, ctx, 22, "original", 9)
	friends, err = second.GetFriendList(current, 22)
	require.NoError(t, err)
	require.Len(t, friends, 1)
	_, err = second.db.ExecContext(ctx, "UPDATE `"+schema+"`.account_role SET trade_listing_id=81 WHERE player_id=22")
	require.NoError(t, err)
	_, err = first.RemoveFriend(peer, 11, 22)
	require.ErrorIs(t, err, ErrOriginalRole)
	require.EqualValues(t, 2, mustCount(t, ctx, first.db, "SELECT COUNT(*) FROM friend"))
	_, err = second.db.ExecContext(ctx, "UPDATE `"+schema+"`.account_role SET trade_listing_id=0 WHERE player_id=22")
	require.NoError(t, err)
	_, err = second.Block(current, 22, 11, 10)
	require.NoError(t, err)
	require.ErrorIs(t, first.Unblock(original, 22, 11), ErrOriginalRole)
	blocks, err := first.ListBlocks(current, 22)
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	require.NoError(t, second.Unblock(current, 22, 11))
	assertFriendInvariants(t, ctx, first.db)
}

func TestTiDBFriendOriginalRole_Uint64OriginalsAndMissingBinding(t *testing.T) {
	repo, _, ctx, schema := roleIdentityTiDBPair(t)
	_, err := repo.db.ExecContext(ctx, "INSERT INTO `"+schema+"`.account_role(player_id,account,owner_revision) VALUES(?, 'maximum', ?)", uint64(math.MaxUint64), uint64(math.MaxUint64))
	require.NoError(t, err)
	maximum := roleIdentityContext(t, ctx, math.MaxUint64, "maximum", math.MaxUint64)
	request, err := repo.CreateRequest(maximum, math.MaxUint64, 11, AddFriendLimits{})
	require.NoError(t, err)
	require.NotZero(t, request.RequestID)
	peer := roleIdentityContext(t, ctx, 11, "peer", 3)
	pending, err := repo.GetPendingRequests(peer, 11)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.EqualValues(t, uint64(math.MaxUint64), pending[0].FromPlayerID)
	_, err = repo.GetFriendList(ctx, 22)
	require.ErrorIs(t, err, ErrOriginalRole)
	_, err = repo.CreateRequest(maximum, 22, 11, AddFriendLimits{})
	require.ErrorIs(t, err, ErrOriginalRole)
}
