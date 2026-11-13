package data

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 两个独立 SQL 池和两个独立 Redis 模拟服务副本边界；所有权威行仍在同一 TiDB
// 测试库。本组不模拟网络分区，也不把宿主双副本结果冒充真实双区域验收。
func friendTiDBReplicaPair(t *testing.T) (*FriendRepo, *FriendRepo, context.Context) {
	t.Helper()
	dsn := friendTiDBTestDSN(t)
	first, ctx := openFriendTiDBTestDB(t, dsn)
	second := openFriendTiDBTestPool(t, dsn)
	require.NoError(t, second.PingContext(ctx))
	var firstID, secondID uint64
	require.NoError(t, first.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&firstID))
	require.NoError(t, second.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&secondID))
	require.NotEqual(t, firstID, secondID, "双副本必须使用不同物理连接，不能由进程内锁替代数据库并发")
	return newFriendTiDBTestRepo(t, first), newFriendTiDBTestRepo(t, second), ctx
}

func TestTiDBFriendReplicas_AcceptReplayAndVisibility(t *testing.T) {
	first, second, ctx := friendTiDBReplicaPair(t)
	const from, to uint64 = 1101, 1102
	forward, err := first.CreateRequest(ctx, from, to, AddFriendLimits{})
	require.NoError(t, err)
	reverse, err := second.CreateRequest(ctx, to, from, AddFriendLimits{})
	require.NoError(t, err)
	var changes atomic.Int32
	errs := runConcurrent(t, 16, func(i int) error {
		repo, requestID, accepter := first, forward.RequestID, to
		if i%2 != 0 {
			repo, requestID, accepter = second, reverse.RequestID, from
		}
		change, err := repo.AcceptRequest(ctx, requestID, accepter, 10)
		if err == nil && change.Changed {
			changes.Add(1)
		}
		return err
	})
	for _, err := range errs {
		require.NoError(t, err)
	}
	assert.EqualValues(t, 1, changes.Load(), "反向申请与同代次重放跨副本合计只能建立一次关系")
	assert.EqualValues(t, 2, mustCount(t, ctx, first.db, "SELECT COUNT(*) FROM friend_request WHERE status=2"))
	for _, repo := range []*FriendRepo{first, second} {
		friends, err := repo.GetFriendList(ctx, to)
		require.NoError(t, err)
		require.Len(t, friends, 1, "另一副本提交后的好友关系必须立即从 SQL 可见")
		assert.Equal(t, from, friends[0].FriendPlayerID)
		pending, err := repo.GetPendingRequests(ctx, to)
		require.NoError(t, err)
		assert.Empty(t, pending)
	}

	// 第二副本的失效访问不到第一副本私有缓存；权威读仍须看见删除后的 SQL。
	stale := `[{"friend_player_id":1101,"since_ms":1}]`
	require.NoError(t, first.rdb.Set(ctx, friendListKey(to), stale, 0).Err())
	removed, err := second.RemoveFriend(ctx, from, to)
	require.NoError(t, err)
	require.True(t, removed)
	cached, err := first.rdb.Get(ctx, friendListKey(to)).Result()
	require.NoError(t, err)
	require.Equal(t, stale, cached, "夹具必须真实保留另一副本未失效的旧列表")
	friends, err := first.GetFriendList(ctx, to)
	require.NoError(t, err)
	assert.Empty(t, friends, "跨副本失效不可靠时也不能成功返回旧权限关系")
	replay, err := first.AcceptRequest(ctx, forward.RequestID, to, 10)
	require.NoError(t, err)
	assert.False(t, replay.Changed)
	assert.Zero(t, mustCount(t, ctx, first.db, "SELECT COUNT(*) FROM friend"), "旧成功重放不能恢复删除的好友")

	next, err := second.CreateRequest(ctx, from, to, AddFriendLimits{})
	require.NoError(t, err)
	require.NotEqual(t, forward.RequestID, next.RequestID)
	_, err = first.AcceptRequest(ctx, forward.RequestID, to, 10)
	assert.ErrorIs(t, err, ErrRequestNotFound, "另一副本重发后，旧 ID 不得消费新申请")
	accepted, err := first.AcceptRequest(ctx, next.RequestID, to, 10)
	require.NoError(t, err)
	assert.True(t, accepted.Changed)
	assertFriendInvariants(t, ctx, first.db)
}

func TestTiDBFriendReplicas_AcceptBlockAndHardLimit(t *testing.T) {
	first, second, ctx := friendTiDBReplicaPair(t)
	const target uint64 = 2200
	requests := make([]uint64, 16)
	for i := range requests {
		change, err := first.CreateRequest(ctx, uint64(2100+i), target, AddFriendLimits{})
		require.NoError(t, err)
		requests[i] = change.RequestID
	}
	errs := runConcurrent(t, len(requests), func(i int) error {
		repo := first
		if i%2 != 0 {
			repo = second
		}
		_, err := repo.AcceptRequest(ctx, requests[i], target, 2)
		return err
	})
	accepted := 0
	for _, err := range errs {
		if err == nil {
			accepted++
		} else {
			assert.ErrorIs(t, err, ErrAcceptorFriendsFull)
		}
	}
	assert.Equal(t, 2, accepted, "硬上限由共享容量行保护，不能依赖单副本互斥")
	assert.EqualValues(t, 2, mustCount(t, ctx, second.db, "SELECT friend_count FROM friend_capacity WHERE player_id=?", target))

	const from, to uint64 = 2301, 2302
	request, err := first.CreateRequest(ctx, from, to, AddFriendLimits{})
	require.NoError(t, err)
	errs = runConcurrent(t, 2, func(i int) error {
		if i == 0 {
			_, err := first.AcceptRequest(ctx, request.RequestID, to, 10)
			if errors.Is(err, ErrRequestNotFound) || errors.Is(err, ErrBlocked) {
				return nil // 拉黑先提交时的正常业务拒绝。
			}
			return err
		}
		_, err := second.Block(ctx, to, from, 10)
		return err
	})
	for _, err := range errs {
		require.NoError(t, err)
	}
	assert.EqualValues(t, 1, mustCount(t, ctx, first.db,
		"SELECT COUNT(*) FROM friend_block WHERE player_id=? AND blocked_player_id=?", to, from))
	assert.Zero(t, mustCount(t, ctx, first.db, "SELECT COUNT(*) FROM friend WHERE player_id=? OR player_id=?", from, to),
		"无论谁先提交，都不能保留既好友又拉黑的状态")
	_, err = first.AcceptRequest(ctx, request.RequestID, to, 10)
	if err != nil {
		assert.ErrorIs(t, err, ErrRequestNotFound)
	}
	assert.Zero(t, mustCount(t, ctx, second.db, "SELECT COUNT(*) FROM friend WHERE player_id=? OR player_id=?", from, to),
		"接受重放不能绕过另一副本已经提交的拉黑")
	assertFriendInvariants(t, ctx, first.db)
}

// MySQL锁回归不能证明TiDB事务契约；此组单独连真实TiDB、每物理连接pessimistic+RC，
// 使用NONCLUSTERED主键夹具验证代次与并发容量。没有DSN明确跳过，不冒充验收通过。
func TestTiDBRequestGeneration_TransactionsAndHardLimit(t *testing.T) {
	db, ctx := openFriendTiDBTestDB(t, friendTiDBTestDSN(t))
	repo := newFriendTiDBTestRepo(t, db)
	require.NoError(t, CheckRequestIDLookupIndex(ctx, db))
	first, err := repo.CreateRequest(ctx, 11, 12, AddFriendLimits{})
	require.NoError(t, err)
	repeated, err := repo.CreateRequest(ctx, 11, 12, AddFriendLimits{})
	require.NoError(t, err)
	assert.Equal(t, first.RequestID, repeated.RequestID)
	assert.False(t, repeated.Changed)
	_, err = repo.RejectRequest(ctx, first.RequestID, 12)
	require.NoError(t, err)
	second, err := repo.CreateRequest(ctx, 11, 12, AddFriendLimits{})
	require.NoError(t, err)
	require.NotEqual(t, first.RequestID, second.RequestID)
	_, err = repo.AcceptRequest(ctx, first.RequestID, 12, 10)
	assert.ErrorIs(t, err, ErrRequestNotFound)
	accepted, err := repo.AcceptRequest(ctx, second.RequestID, 12, 10)
	require.NoError(t, err)
	assert.True(t, accepted.Changed)
	accepted, err = repo.AcceptRequest(ctx, second.RequestID, 12, 10)
	require.NoError(t, err)
	assert.False(t, accepted.Changed)

	const target uint64 = 200
	ids := make([]uint64, 16)
	for i := range ids {
		change, err := repo.CreateRequest(ctx, uint64(100+i), target, AddFriendLimits{})
		require.NoError(t, err)
		ids[i] = change.RequestID
	}
	errs := runConcurrent(t, len(ids), func(i int) error {
		change, err := repo.AcceptRequest(ctx, ids[i], target, 2)
		if err == nil && !change.Changed {
			return fmt.Errorf("首次处理被误判为重放")
		}
		return err
	})
	consumed := 0
	for _, err := range errs {
		if err == nil {
			consumed++
			continue
		}
		assert.ErrorIs(t, err, ErrAcceptorFriendsFull)
	}
	assert.Equal(t, 2, consumed, "守卫应在TiDB当前读中完整复核硬上限")
	assert.EqualValues(t, 2, mustCount(t, ctx, db, "SELECT COUNT(*) FROM friend WHERE player_id=?", target))
	assertFriendInvariants(t, ctx, db)

	_, err = db.ExecContext(ctx, `INSERT INTO friend_request
		(from_player_id,to_player_id,request_time_ms,status,updated_ms) VALUES(501,502,123,1,456)`)
	require.NoError(t, err)
	entries, err := repo.GetPendingRequests(ctx, 502)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.NotZero(t, entries[0].RequestID)
	assert.EqualValues(t, 123, entries[0].RequestTimeMs)
	_, err = repo.AcceptRequest(ctx, entries[0].RequestID, 503, 10)
	assert.ErrorIs(t, err, ErrRequestNotFound)
	_, err = repo.AcceptRequest(ctx, entries[0].RequestID, 502, 10)
	require.NoError(t, err)
}
