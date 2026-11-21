package store

import (
	"context"
	"testing"
	"time"

	base "proto/common/base"
	dspb "proto/data_service"
	"shared/dirshard"
	"shared/playername"

	"github.com/stretchr/testify/require"
)

func TestTiDBRenameRoleCommitHoldReplayAndResume(t *testing.T) {
	f := newDirectoryShardFixture(t)
	d, names := namedDirectoryForTest(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// 创建角色 12(旧名 RenameOldA)与角色 13(占位检查保护期)。
	oldKey := reserveForTest(t, ctx, names, 12, "RenameOldA")
	_, err := createInitialRoleForTest(d, ctx, shardSeller, &base.AccountSimplePlayer{PlayerId: 12, ZoneId: 1, Name: oldKey.display})
	require.NoError(t, err)
	otherKey := reserveForTest(t, ctx, names, 13, "RenameOtherB")
	_, err = createInitialRoleForTest(d, ctx, shardBuyer, &base.AccountSimplePlayer{PlayerId: 13, ZoneId: 1, Name: otherKey.display})
	require.NoError(t, err)

	// 1. 同名与非法名会记录 seq 并拒绝。
	sameOut, err := d.RenameRole(ctx, RenameRequest{
		Player: 12, Seq: 1, Display: oldKey.display, Norm: oldKey.norm,
		Cooldown: time.Hour, Hold: time.Hour,
	})
	require.NoError(t, err)
	require.Equal(t, dspb.RenameRoleResult_RENAME_ROLE_SAME_NAME, sameOut.Result)

	invalidOut, err := d.RenameRole(ctx, RenameRequest{
		Player: 12, Seq: 2, Invalid: true, Cooldown: time.Hour, Hold: time.Hour,
	})
	require.NoError(t, err)
	require.Equal(t, dspb.RenameRoleResult_RENAME_ROLE_INVALID, invalidOut.Result)

	// 2. 合法改名提交成功:新名绑定、旧名进入保护期、名字副本更新。
	newDisplay, newNorm, verdict := playername.Normalize("RenameNewC", playername.StructuralRules)
	require.Equal(t, playername.VerdictOK, verdict)
	okOut, err := d.RenameRole(ctx, RenameRequest{
		Player: 12, Seq: 3, Display: newDisplay, Norm: newNorm,
		Cooldown: time.Hour, Hold: time.Hour,
	})
	require.NoError(t, err)
	require.Equal(t, dspb.RenameRoleResult_RENAME_ROLE_OK, okOut.Result)
	require.Equal(t, newDisplay, okOut.Name)
	require.NotZero(t, okOut.RenamedMs)

	// 3. 幂等重放相同 seq 仍回 OK;更小 seq 回 STALE_SEQ 并附带 current_seq。
	replayOut, err := d.RenameRole(ctx, RenameRequest{
		Player: 12, Seq: 3, Display: newDisplay, Norm: newNorm,
		Cooldown: time.Hour, Hold: time.Hour,
	})
	require.NoError(t, err)
	require.Equal(t, dspb.RenameRoleResult_RENAME_ROLE_OK, replayOut.Result)
	require.Equal(t, okOut.Name, replayOut.Name)

	staleOut, err := d.RenameRole(ctx, RenameRequest{
		Player: 12, Seq: 2, Display: newDisplay, Norm: newNorm,
		Cooldown: time.Hour, Hold: time.Hour,
	})
	require.NoError(t, err)
	require.Equal(t, dspb.RenameRoleResult_RENAME_ROLE_STALE_SEQ, staleOut.Result)
	require.EqualValues(t, 3, staleOut.CurrentSeq)

	// 4. 冷却期内再改名被拒,返回 NextAllowedMs。
	nextDisplay, nextNorm, verdict := playername.Normalize("RenameNewD", playername.StructuralRules)
	require.Equal(t, playername.VerdictOK, verdict)
	cdOut, err := d.RenameRole(ctx, RenameRequest{
		Player: 12, Seq: 4, Display: nextDisplay, Norm: nextNorm,
		Cooldown: time.Hour, Hold: time.Hour,
	})
	require.NoError(t, err)
	require.Equal(t, dspb.RenameRoleResult_RENAME_ROLE_COOLDOWN, cdOut.Result)
	require.Greater(t, cdOut.NextAllowedMs, okOut.RenamedMs)

	// 5. 旧名处于保护期内,其他角色(13)不能抢注。
	heldOut, err := d.RenameRole(ctx, RenameRequest{
		Player: 13, Seq: 1, Display: oldKey.display, Norm: oldKey.norm,
		Cooldown: 0, Hold: time.Hour,
	})
	require.NoError(t, err)
	require.Equal(t, dspb.RenameRoleResult_RENAME_ROLE_TAKEN, heldOut.Result)

	// 6. 保护期过期后,角色 13 可以接管旧名。
	_, err = f.Raw[dirshard.SlotOfName(oldKey.norm, shardFixtureK)].ExecContext(
		ctx, "UPDATE player_name SET held_until_ms = 1 WHERE name_norm = ?", oldKey.norm,
	)
	require.NoError(t, err)
	takeoverOut, err := d.RenameRole(ctx, RenameRequest{
		Player: 13, Seq: 2, Display: oldKey.display, Norm: oldKey.norm,
		Cooldown: 0, Hold: time.Hour,
	})
	require.NoError(t, err)
	require.Equal(t, dspb.RenameRoleResult_RENAME_ROLE_OK, takeoverOut.Result)

	// 7. 模拟提交后崩溃未收尾:ResumeRenames 补完收尾并置为 settled。
	_, err = f.Raw[dirshard.SlotOfPlayer(12, shardFixtureK)].ExecContext(
		ctx, "UPDATE account_role_rename SET state = ?, updated_ms = 1 WHERE player_id = ?", renameCommitted, 12,
	)
	require.NoError(t, err)
	resumed, err := d.ResumeRenames(ctx, 0, time.Minute, 10)
	require.NoError(t, err)
	require.Equal(t, 1, resumed.Settled)
	require.Zero(t, resumed.Failed)
}
