package data

import (
	"context"
	"database/sql"
	"math"
	"sync"
	"testing"
	"time"

	base "proto/common/base"
	dspb "proto/data_service"
	rfpb "proto/role_fence"
	"shared/rolefence"
	"shared/roleidentity"

	"github.com/stretchr/testify/require"
)

// friendFenceDirectory 是目录 GetRoleFenceState 的替身。副本表和自愈走真的。
type friendFenceDirectory struct {
	mu      sync.Mutex
	states  map[uint64]*rfpb.RoleFenceState
	version uint64
	reads   int
}

func (d *friendFenceDirectory) GetRoleFenceState(_ context.Context, player uint64) (*rfpb.RoleFenceState, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reads++
	state, ok := d.states[player]
	if !ok {
		return nil, rolefence.ErrRoleNotFound
	}
	return state, nil
}

// set 改权威状态,版本加 1。不碰副本:相当于推送丢了。
func (d *friendFenceDirectory) set(player uint64, account string, revision, token uint64) *rfpb.RoleFenceState {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.version++
	state := &rfpb.RoleFenceState{PlayerId: player, Account: account, OwnerRevision: revision, FreezeToken: token, Version: d.version, HomeZone: 1}
	d.states[player] = state
	return state
}

// push 改权威状态并写进副本,相当于协调者的 Prepare 或 Push 到了。
func (d *friendFenceDirectory) push(t *testing.T, ctx context.Context, db *sql.DB, player uint64, account string, revision, token uint64) {
	t.Helper()
	_, err := rolefence.Apply(ctx, db, d.set(player, account, revision, token))
	require.NoError(t, err)
}

func (d *friendFenceDirectory) readCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reads
}

func roleIdentityTiDBPair(t *testing.T) (*FriendRepo, *FriendRepo, context.Context, *friendFenceDirectory) {
	t.Helper()
	first, second, ctx := friendTiDBReplicaPair(t)
	// 这组用例共用一个库:每例重建副本表,不继承上一例的状态。
	_, err := first.db.ExecContext(ctx, "DROP TABLE IF EXISTS "+rolefence.TableName)
	require.NoError(t, err)
	require.NoError(t, rolefence.EnsureSchema(ctx, first.db))
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, dropErr := first.db.ExecContext(cleanup, "DROP TABLE IF EXISTS "+rolefence.TableName)
		require.NoError(t, dropErr)
	})
	directory := &friendFenceDirectory{states: map[uint64]*rfpb.RoleFenceState{}}
	directory.set(11, "peer", 3, 0)
	directory.set(22, "original", 7, 0)
	first.ConfigureRoleFence(rolefence.NewGuard(first.db, directory))
	second.ConfigureRoleFence(rolefence.NewGuard(second.db, directory))
	return first, second, ctx, directory
}

func roleIdentityContext(t *testing.T, parent context.Context, player uint64, account string, revision uint64) context.Context {
	t.Helper()
	ctx, err := roleidentity.Capture(parent, &dspb.AccountRoleOwnership{Account: account, OwnerRevision: revision, Role: &base.AccountSimplePlayer{PlayerId: player}})
	require.NoError(t, err)
	return ctx
}

func TestTiDBFriendOriginalRole_AllClientReadsAndWritesRejectTransferABAFrozen(t *testing.T) {
	repo, _, ctx, directory := roleIdentityTiDBPair(t)
	original := roleIdentityContext(t, ctx, 22, "original", 7)
	for _, change := range []struct {
		account         string
		revision, token uint64
	}{{"next", 8, 0}, {"original", 9, 0}, {"original", 7, 81}} {
		directory.push(t, ctx, repo.db, 22, change.account, change.revision, change.token)
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
	first, second, ctx, directory := roleIdentityTiDBPair(t)
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
	for _, change := range []struct {
		account  string
		revision uint64
	}{{"next", 8}, {"original", 9}} {
		directory.push(t, ctx, second.db, 22, change.account, change.revision, 0)
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
	directory.push(t, ctx, second.db, 22, "original", 9, 81)
	_, err = first.RemoveFriend(peer, 11, 22)
	require.ErrorIs(t, err, ErrOriginalRole)
	require.EqualValues(t, 2, mustCount(t, ctx, first.db, "SELECT COUNT(*) FROM friend"))
	directory.push(t, ctx, second.db, 22, "original", 9, 0)
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
	repo, _, ctx, directory := roleIdentityTiDBPair(t)
	directory.set(math.MaxUint64, "maximum", math.MaxUint64, 0)
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

// 目录里没有对端:不写副本,按原逻辑跳过。好友申请的目标本来就可能不存在。
func TestTiDBFriendAbsentPeerIsSkippedAndWritesNoReplica(t *testing.T) {
	repo, _, ctx, _ := roleIdentityTiDBPair(t)
	original := roleIdentityContext(t, ctx, 22, "original", 7)
	request, err := repo.CreateRequest(original, 22, 33, AddFriendLimits{})
	require.NoError(t, err)
	require.True(t, request.Changed)
	require.EqualValues(t, 1, mustCount(t, ctx, repo.db, "SELECT COUNT(*) FROM friend_request"))
	require.Zero(t, mustCount(t, ctx, repo.db, "SELECT COUNT(*) FROM role_fence WHERE player_id=33"))
	require.EqualValues(t, 1, mustCount(t, ctx, repo.db, "SELECT COUNT(*) FROM role_fence WHERE player_id=22"))
}

// actor 自己不在目录里不是跳过:会话原件对不上任何角色,一律拒绝。
func TestTiDBFriendActorMissingFromDirectoryIsRejected(t *testing.T) {
	repo, _, ctx, _ := roleIdentityTiDBPair(t)
	ghost := roleIdentityContext(t, ctx, 44, "ghost", 1)
	_, err := repo.GetFriendList(ghost, 44)
	require.ErrorIs(t, err, ErrOriginalRole)
	_, err = repo.CreateRequest(ghost, 44, 11, AddFriendLimits{})
	require.ErrorIs(t, err, ErrOriginalRole)
	require.Zero(t, mustCount(t, ctx, repo.db, "SELECT COUNT(*) FROM role_fence WHERE player_id=44"))
}

// 冻结写进了副本,过户提交,推送丢了。旧主读到权威状态后被拒,新主自愈后通过。
func TestTiDBFriendTransferWithLostPushHealsNewOwnerRejectsOldOwner(t *testing.T) {
	repo, _, ctx, directory := roleIdentityTiDBPair(t)
	directory.push(t, ctx, repo.db, 22, "original", 7, 81)
	directory.set(22, "buyer", 8, 0)
	before := directory.readCount()

	buyer := roleIdentityContext(t, ctx, 22, "buyer", 8)
	_, err := repo.GetFriendList(buyer, 22)
	require.NoError(t, err)
	require.Equal(t, before+1, directory.readCount(), "新主自愈一次")

	original := roleIdentityContext(t, ctx, 22, "original", 7)
	_, err = repo.GetFriendList(original, 22)
	require.ErrorIs(t, err, ErrOriginalRole)
	require.Equal(t, before+1, directory.readCount(), "副本修订号更大时旧主直接被拒,不打 RPC")
}

// 读路径持副本行锁到读完:这是和冻结互斥的串行点,读完才让 Prepare 过去。
func TestTiDBFriendRoleReaderHoldsReplicaLockUntilClosed(t *testing.T) {
	repo, _, ctx, directory := roleIdentityTiDBPair(t)
	original := roleIdentityContext(t, ctx, 22, "original", 7)
	_, closeRead, err := repo.roleReader(original, 22)
	require.NoError(t, err)
	frozen := make(chan error, 1)
	state := directory.set(22, "original", 7, 81)
	go func() {
		_, applyErr := rolefence.Apply(ctx, repo.db, state)
		frozen <- applyErr
	}()
	select {
	case err = <-frozen:
		t.Fatalf("读路径持锁期间冻结不该写进去: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	closeRead()
	select {
	case err = <-frozen:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("读完之后冻结应该写进去")
	}
}
