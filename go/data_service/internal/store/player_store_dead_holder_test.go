package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"shared/accountdirectory"
)

// LeaveScene 判死释放的提交点判定(player_store_dead_holder.go),与交易判死的 raiseStoreEpoch 同一口径:三列一起比。
// 真库上的事务与读见 player_store_dead_holder_tidb_test.go。

func TestDecideDeadHolderRelease(t *testing.T) {
	const locationEpoch = uint64(30)
	seen := lockedPlayerStoreOwner{ownerEpoch: 30, holder: []byte("instance-a"), claimSeq: 4}
	cases := []struct {
		name        string
		current     lockedPlayerStoreOwner
		seen        lockedPlayerStoreOwner
		wantRelease bool
		wantErr     error
	}{
		{name: "还是读到的那一份:释放", current: seen, seen: seen, wantRelease: true},
		{name: "同实例同代次重登,只有 claim_seq 变", current: lockedPlayerStoreOwner{ownerEpoch: 30, holder: []byte("instance-a"), claimSeq: 5},
			seen: seen, wantErr: accountdirectory.ErrConflict},
		{name: "换了持有者,代次不比 location 新", current: lockedPlayerStoreOwner{ownerEpoch: 30, holder: []byte("instance-b"), claimSeq: 5},
			seen: seen, wantErr: accountdirectory.ErrConflict},
		{name: "代次变了但不比 location 新", current: lockedPlayerStoreOwner{ownerEpoch: 29, holder: []byte("instance-a"), claimSeq: 4},
			seen: seen, wantErr: accountdirectory.ErrConflict},
		{name: "新的持有者在更新的代次认领了:不动,也不算冲突", current: lockedPlayerStoreOwner{ownerEpoch: 31, holder: []byte("instance-b"), claimSeq: 5},
			seen: seen},
		{name: "上一次释放已经做完(重试)", current: lockedPlayerStoreOwner{ownerEpoch: 31, claimSeq: 4}, seen: seen},
		{name: "读到时就没有持有者(建角初行),认领在途也要挡", current: lockedPlayerStoreOwner{ownerEpoch: 1, holder: []byte{}},
			seen: lockedPlayerStoreOwner{ownerEpoch: 1}, wantRelease: true},
		{name: "读到时没有持有者,之后有人认领了", current: lockedPlayerStoreOwner{ownerEpoch: 30, holder: []byte("instance-a"), claimSeq: 1},
			seen: lockedPlayerStoreOwner{ownerEpoch: 1}, wantErr: accountdirectory.ErrConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			release, err := decideDeadHolderRelease(tc.current, locationEpoch, tc.seen)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.wantRelease, release)
		})
	}
}

// 读写权行:有路由按落点读三列;没有路由 / 写权行不算错(没有东西要释放);落点不在白名单、读失败都回错误,不能当成没有。
func TestReadPlayerStoreHolder(t *testing.T) {
	ctx := context.Background()
	s, steps := newFlowStore(4)
	const player = uint64(11)

	holder, err := s.ReadPlayerStoreHolder(ctx, player)
	require.NoError(t, err)
	require.Nil(t, holder, "没有路由")

	steps.routes[player] = RouteRow{PlayerID: player, HomeZone: 1, StorageID: flowStorage, PlacementVersion: 1, Rev: 1}
	holder, err = s.ReadPlayerStoreHolder(ctx, player)
	require.NoError(t, err)
	require.Nil(t, holder, "没有写权行")

	steps.storeOwners[player] = lockedPlayerStoreOwner{ownerEpoch: 30, holder: []byte("instance-a"), claimSeq: 4}
	holder, err = s.ReadPlayerStoreHolder(ctx, player)
	require.NoError(t, err)
	require.EqualValues(t, 30, holder.GetOwnerEpoch())
	require.Equal(t, []byte("instance-a"), holder.GetHolderInstance())
	require.EqualValues(t, 4, holder.GetClaimSeq())

	steps.fail["readStoreOwner"] = []error{errFlow}
	_, err = s.ReadPlayerStoreHolder(ctx, player)
	require.ErrorIs(t, err, errFlow)

	steps.allowed[flowStorage] = false
	_, err = s.ReadPlayerStoreHolder(ctx, player)
	require.ErrorIs(t, err, accountdirectory.ErrUnavailable)

	_, err = s.ReadPlayerStoreHolder(ctx, 0)
	require.ErrorIs(t, err, accountdirectory.ErrInvalid)
}
