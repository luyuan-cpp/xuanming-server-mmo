package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	storepb "proto/player_store"
	"shared/accountdirectory"
)

// LeaveScene 判死释放两步(player_store_dead_holder.go)的真库用例:读写权行 → (scene_manager 复核持有实例)→ 比较并写。

func TestTiDBReleaseDeadPlayerStoreHolderIfUnchanged(t *testing.T) {
	s, db := accountDirectoryTiDB(t)
	_, schema := initialPlayerSchemaForTest(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	role := directRoleForTest(t, s, ctx, 3207)
	setOwner := func(epoch uint64, holder string, claimSeq uint64) {
		t.Helper()
		_, err := db.Exec("UPDATE `"+schema+"`.`player_store` SET owner_epoch=?,released=0,holder_instance=?,claim_seq=? WHERE player_id=? AND kind=0",
			epoch, []byte(holder), claimSeq, role.PlayerId)
		require.NoError(t, err)
	}
	readOwner := func() storeRowForTest {
		t.Helper()
		owner, ok := readStoreRowForTest(t, db, schema, role.PlayerId, storepb.PlayerStoreKind_PLAYER_STORE_KIND_OWNER)
		require.True(t, ok)
		return owner
	}

	// 建角初行:没有持有者。
	seen, err := s.ReadPlayerStoreHolder(ctx, role.PlayerId)
	require.NoError(t, err)
	require.EqualValues(t, 1, seen.GetOwnerEpoch())
	require.Empty(t, seen.GetHolderInstance())

	setOwner(30, "instance-a", 4)
	seen, err = s.ReadPlayerStoreHolder(ctx, role.PlayerId)
	require.NoError(t, err)
	require.EqualValues(t, 30, seen.GetOwnerEpoch())
	require.Equal(t, []byte("instance-a"), seen.GetHolderInstance())
	require.EqualValues(t, 4, seen.GetClaimSeq())

	// 读之后、释放之前,持有进程以同一 UUID 重注册,玩家同节点重登认领:(E, H) 写回原值,只有 claim_seq 变。
	// 挂住的释放不许踢掉这名在线玩家。
	setOwner(30, "instance-a", 5)
	err = s.ReleaseDeadPlayerStoreHolderIfUnchanged(ctx, role.PlayerId, 30, seen)
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	owner := readOwner()
	require.EqualValues(t, 30, owner.epoch)
	require.EqualValues(t, 0, owner.released)
	require.Equal(t, []byte("instance-a"), owner.holder)

	// 重读之后写权行没再变:释放。
	seen, err = s.ReadPlayerStoreHolder(ctx, role.PlayerId)
	require.NoError(t, err)
	require.NoError(t, s.ReleaseDeadPlayerStoreHolderIfUnchanged(ctx, role.PlayerId, 30, seen))
	owner = readOwner()
	require.EqualValues(t, 31, owner.epoch, "抬到 location 这一代 + 1:旧持有者恢复之后的写因 E<Er 被挡住")
	require.EqualValues(t, 1, owner.released)
	require.Empty(t, owner.holder)

	// 幂等:同一份 seen 重试,库里已比 location 这一代新,不动,也不算冲突。
	require.NoError(t, s.ReleaseDeadPlayerStoreHolderIfUnchanged(ctx, role.PlayerId, 30, seen))
	require.EqualValues(t, 31, readOwner().epoch)

	// 新的持有者在更新的代次认领了:不动它。
	setOwner(40, "instance-b", 6)
	require.NoError(t, s.ReleaseDeadPlayerStoreHolderIfUnchanged(ctx, role.PlayerId, 30, seen))
	owner = readOwner()
	require.EqualValues(t, 40, owner.epoch)
	require.EqualValues(t, 0, owner.released)
	require.Equal(t, []byte("instance-b"), owner.holder)

	// 没有这名玩家:没有东西要读、要释放。
	missing, err := s.ReadPlayerStoreHolder(ctx, 987654321)
	require.NoError(t, err)
	require.Nil(t, missing)
	require.NoError(t, s.ReleaseDeadPlayerStoreHolderIfUnchanged(ctx, 987654321, 30, seen))
}
