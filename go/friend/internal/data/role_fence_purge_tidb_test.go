package data

import (
	"context"
	"database/sql"
	"testing"
	"time"

	rfpb "proto/role_fence"
	"shared/rolefence"
	"shared/rolefence/fencetest"
	"shared/serviceauth"

	"github.com/stretchr/testify/require"
)

const friendFenceCoordinatorSecret = "role-fence-friend-test-only-coordinator-secret"

// friendPurge 走真的 PurgeRemovedRole:鉴权、按版本合并墓碑、锁副本、清理,同一个事务。
func friendPurge(t *testing.T, ctx context.Context, server *rolefence.ParticipantServer, state *rfpb.RoleFenceState) {
	t.Helper()
	in := &rfpb.PurgeRemovedRoleRequest{State: state}
	require.NoError(t, serviceauth.Sign(rfpb.RoleFenceParticipant_PurgeRemovedRole_FullMethodName, in, rolefence.CoordinatorCaller, friendFenceCoordinatorSecret, uint64(time.Now().UnixMilli())))
	out, err := server.PurgeRemovedRole(ctx, in)
	require.NoError(t, err)
	require.False(t, out.GetRetry(), "清理应一次成功")
}

// 删角收尾(docs/design/account-deletion.md §6.2):他在任一侧的好友边、申请、拉黑和他的容量行都删掉,对端的 friend_count
// 跟着减到真实边数;别人之间的关系不动。第二次调用没有可删的行,照样成功、什么都不变。
func TestTiDBFriendPurgeRemovedRoleDeletesBothSidesAndKeepsCountsExact(t *testing.T) {
	repo, _, ctx, _ := roleIdentityTiDBPair(t)
	db := repo.db
	const removed, friend, other, stranger, blocked, blocker, unrelated uint64 = 31, 32, 33, 34, 35, 36, 37
	// 三人两两互为好友,各 2 个好友。
	for _, edge := range [][2]uint64{{removed, friend}, {removed, other}, {friend, removed}, {friend, other}, {other, removed}, {other, friend}} {
		_, err := db.ExecContext(ctx, "INSERT INTO friend (player_id, friend_player_id, since_ms) VALUES (?, ?, 1)", edge[0], edge[1])
		require.NoError(t, err)
	}
	for _, player := range []uint64{removed, friend, other} {
		_, err := db.ExecContext(ctx, "INSERT INTO friend_capacity (player_id, friend_count, created_ms) VALUES (?, 2, 1)", player)
		require.NoError(t, err)
	}
	seedPending(t, ctx, db, stranger, removed)
	seedPending(t, ctx, db, stranger, other)
	_, err := db.ExecContext(ctx, "INSERT INTO friend_request (from_player_id, to_player_id, request_time_ms, status, updated_ms, request_id) VALUES (?, ?, 1, ?, 1, ?)",
		removed, unrelated, requestStatusRejected, testRequestIDs.Add(1))
	require.NoError(t, err)
	seedBlock(t, ctx, db, removed, blocked)
	seedBlock(t, ctx, db, blocker, removed)
	seedBlock(t, ctx, db, friend, unrelated)
	assertFriendInvariants(t, ctx, db)
	fencetest.Seed(t, ctx, db, removed, "removed", 1)

	server, err := rolefence.NewParticipantServer(db, rolefence.NoPending, RolePurge, friendFenceCoordinatorSecret)
	require.NoError(t, err)
	tombstone := &rfpb.RoleFenceState{PlayerId: removed, OwnerRevision: 2, Version: 2, HomeZone: 1}
	for round := 0; round < 2; round++ {
		friendPurge(t, ctx, server, tombstone)
		requireFriendPurged(t, ctx, db, removed)
		require.EqualValues(t, 2, mustCount(t, ctx, db, "SELECT COUNT(*) FROM friend"), "只剩 friend↔other 两条边")
		require.EqualValues(t, 1, mustCount(t, ctx, db, "SELECT friend_count FROM friend_capacity WHERE player_id=?", friend))
		require.EqualValues(t, 1, mustCount(t, ctx, db, "SELECT friend_count FROM friend_capacity WHERE player_id=?", other))
		require.EqualValues(t, 1, mustCount(t, ctx, db, "SELECT COUNT(*) FROM friend_request WHERE from_player_id=? AND to_player_id=?", stranger, other))
		require.EqualValues(t, 1, mustCount(t, ctx, db, "SELECT COUNT(*) FROM friend_block WHERE player_id=? AND blocked_player_id=?", friend, unrelated))
		assertFriendInvariants(t, ctx, db)
		fence, err := rolefence.Read(ctx, db, removed)
		require.NoError(t, err)
		require.Empty(t, fence.Account, "副本已合并成墓碑")
	}
}

func requireFriendPurged(t *testing.T, ctx context.Context, db *sql.DB, player uint64) {
	t.Helper()
	for _, query := range []string{
		"SELECT COUNT(*) FROM friend WHERE player_id=? OR friend_player_id=?",
		"SELECT COUNT(*) FROM friend_request WHERE from_player_id=? OR to_player_id=?",
		"SELECT COUNT(*) FROM friend_block WHERE player_id=? OR blocked_player_id=?",
		"SELECT COUNT(*) FROM friend_capacity WHERE player_id=? OR player_id=?",
	} {
		require.Zero(t, mustCount(t, ctx, db, query, player, player), query)
	}
}
