package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	base "proto/common/base"
	dspb "proto/data_service"
	"shared/accountdirectory"
	"shared/rolefence"
)

// 账号注销在目录一侧的状态机(docs/design/account-deletion.md §4、§7),真 TiDB(夹具同 accountDirectoryTiDB,缺 DSN 跳过,
// ACCOUNT_DIRECTORY_REQUIRE_TIDB=1 时缺 DSN 算失败):
//   - 申请幂等、不刷新时刻;冷静期内建角按满拒绝;撤销回 0 后能建;
//   - 认领把到期的 1 改成 2 并写租约,租约内不重复认领,租约过期再认领;执行中撤销回 TOO_LATE;
//   - 角色没删完不收尾;删完收尾写回执、删账号行;同一账号串再登录是新行、带「以前注销完成」的时刻;
//   - 预检有挂起什么都不写;逾期计数。

func deletionState(t *testing.T, s *AccountDirectoryStore, ctx context.Context, account string) dspb.AccountDeletionState {
	t.Helper()
	state, err := s.AccountDeletionState(ctx, account)
	require.NoError(t, err)
	return state
}

func TestTiDBAccountDeletionLifecycle(t *testing.T) {
	s, db := accountDirectoryTiDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const account = "apple_deletion.lifecycle"

	out, err := s.RequestDeletion(ctx, account, DeletionSourceUser, 0)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_NOT_FOUND, out.Outcome, "从没登录过的账号不能注销")

	_, err = s.Read(ctx, account, true)
	require.NoError(t, err)
	_, err = createInitialRoleForTest(s, ctx, account, &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 9101})
	require.NoError(t, err)

	// 冷静期一小时:申请成功,重复申请原样回、不刷新时刻。
	out, err = s.RequestDeletion(ctx, account, DeletionSourceUser, time.Hour)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_OK, out.Outcome)
	require.Equal(t, dspb.AccountDeletionState_ACCOUNT_DELETION_STATE_PENDING, out.Lifecycle.DeletionState)
	first := out.Lifecycle.DeletionExecuteAfterMs
	require.NotZero(t, first)
	out, err = s.RequestDeletion(ctx, account, DeletionSourceUser, 2*time.Hour)
	require.NoError(t, err)
	require.Equal(t, first, out.Lifecycle.DeletionExecuteAfterMs, "重复申请不刷新时刻")

	// 冷静期内:名额按 0 算,建角回满;没到期不被认领。
	_, err = createInitialRoleForTest(s, ctx, account, &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 9102})
	require.ErrorIs(t, err, accountdirectory.ErrFull)
	claimed, err := s.ClaimDeletions(ctx, MaxDeletionClaim)
	require.NoError(t, err)
	require.NotContains(t, claimed, account)
	lifecycle, err := s.ReadWithLifecycle(ctx, account, false)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountDeletionState_ACCOUNT_DELETION_STATE_PENDING, lifecycle.Lifecycle.DeletionState)

	// 撤销之后一切照旧。
	out, err = s.CancelDeletion(ctx, account)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_OK, out.Outcome)
	require.Equal(t, dspb.AccountDeletionState_ACCOUNT_DELETION_STATE_NONE, out.Lifecycle.DeletionState)
	_, err = createInitialRoleForTest(s, ctx, account, &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 9102})
	require.NoError(t, err)

	// 冷静期 0:立刻到期,认领改成执行中;租约内不重复认领;执行中撤销太晚。
	out, err = s.RequestDeletion(ctx, account, DeletionSourceAdmin, 0)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountDeletionState_ACCOUNT_DELETION_STATE_PENDING, out.Lifecycle.DeletionState)
	claimed, err = s.ClaimDeletions(ctx, MaxDeletionClaim)
	require.NoError(t, err)
	require.Contains(t, claimed, account)
	require.Equal(t, dspb.AccountDeletionState_ACCOUNT_DELETION_STATE_DELETING, deletionState(t, s, ctx, account))
	claimed, err = s.ClaimDeletions(ctx, MaxDeletionClaim)
	require.NoError(t, err)
	require.NotContains(t, claimed, account, "租约内不重复认领")
	_, err = db.ExecContext(ctx, "UPDATE account_directory SET deletion_lease_until_ms=1 WHERE account=?", []byte(account))
	require.NoError(t, err)
	claimed, err = s.ClaimDeletions(ctx, MaxDeletionClaim)
	require.NoError(t, err)
	require.Contains(t, claimed, account, "租约过期由别的副本接着认领")
	out, err = s.CancelDeletion(ctx, account)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_TOO_LATE, out.Outcome)
	_, err = createInitialRoleForTest(s, ctx, account, &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 9103})
	require.ErrorIs(t, err, accountdirectory.ErrFull, "执行中同样不能建角")

	// 角色没删完不收尾。
	fin, err := s.FinishDeletion(ctx, account)
	require.NoError(t, err)
	require.False(t, fin.Done)
	require.EqualValues(t, 2, fin.RemainingRefs)
	for _, id := range []uint64{9101, 9102} {
		owned, err := s.Lookup(ctx, id)
		require.NoError(t, err)
		require.NoError(t, s.Remove(ctx, account, []*dspb.AccountRoleVersion{{PlayerId: id, OwnerRevision: owned.OwnerRevision}}))
	}

	// 删完收尾:回执、删行;重复收尾幂等。
	fin, err = s.FinishDeletion(ctx, account)
	require.NoError(t, err)
	require.True(t, fin.Done)
	fin, err = s.FinishDeletion(ctx, account)
	require.NoError(t, err)
	require.True(t, fin.Done)
	_, err = s.ReadWithLifecycle(ctx, account, false)
	require.ErrorIs(t, err, accountdirectory.ErrMissing)
	var source uint32
	require.NoError(t, db.QueryRowContext(ctx, "SELECT source FROM "+AccountDeletionReceiptTableName+" WHERE account_digest=?", accountDigest(account)).Scan(&source))
	require.Equal(t, DeletionSourceAdmin, source)
	var leaked int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+AccountDeletionReceiptTableName+" WHERE account_digest=?", []byte(account)).Scan(&leaked))
	require.Zero(t, leaked, "回执不存原账号")

	// 同一账号串再登录:新账号,角色为空,带上次注销完成的时刻。
	again, err := s.ReadWithLifecycle(ctx, account, true)
	require.NoError(t, err)
	require.Empty(t, again.Players.Players)
	require.NotZero(t, again.Lifecycle.PreviousDeletedMs)
	require.Equal(t, dspb.AccountDeletionState_ACCOUNT_DELETION_STATE_NONE, again.Lifecycle.DeletionState)
	_, err = createInitialRoleForTest(s, ctx, account, &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 9104})
	require.NoError(t, err, "新账号照常建角")
}

func TestTiDBAccountDeletionPrecheckBlocksWithoutWriting(t *testing.T) {
	s, _ := accountDirectoryTiDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const account = "google_deletion.blocked"
	_, err := s.Read(ctx, account, true)
	require.NoError(t, err)
	_, err = createInitialRoleForTest(s, ctx, account, &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 9201})
	require.NoError(t, err)

	fences := fencesOf(s)
	fences.setDryRun(rolefence.PrepareBlocked)
	fences.mu.Lock()
	fences.blockers = []string{"guild_leader"}
	fences.mu.Unlock()
	out, err := s.RequestDeletion(ctx, account, DeletionSourceUser, 0)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_BLOCKED, out.Outcome)
	require.Len(t, out.Blockers, 1)
	require.EqualValues(t, 9201, out.Blockers[0].PlayerId)
	require.Equal(t, "guild_leader", out.Blockers[0].Reason)
	require.Equal(t, dspb.AccountDeletionState_ACCOUNT_DELETION_STATE_NONE, deletionState(t, s, ctx, account), "预检没过什么都不写")
	// 试探必须带删角令牌:参与者不收没有令牌的 PrepareRoleFreeze,而且按令牌种类选删角的判据(不是过户的「先离帮」)。
	fences.mu.Lock()
	probed := fences.probed[len(fences.probed)-1]
	fences.mu.Unlock()
	require.Equal(t, rolefence.RemovalToken(9201), probed.FreezeToken)
	require.False(t, probed.FreezeFinal)
	owner, err := s.Lookup(ctx, 9201)
	require.NoError(t, err)
	require.Zero(t, owner.TradeListingId, "试探不写目录行")

	// 问不齐不算通过。
	fences.setDryRun(rolefence.PrepareRetry)
	out, err = s.RequestDeletion(ctx, account, DeletionSourceUser, 0)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_RETRY, out.Outcome)
	require.Equal(t, dspb.AccountDeletionState_ACCOUNT_DELETION_STATE_NONE, deletionState(t, s, ctx, account))

	// 运维代办:帮主不拦(没有玩家去转让,执行时收尾自动让位);在途业务照拦。
	fences.setDryRun(rolefence.PrepareBlocked)
	fences.mu.Lock()
	fences.blockers = []string{"guild_leader", "trade_commerce"}
	fences.mu.Unlock()
	out, err = s.RequestDeletion(ctx, account, DeletionSourceAdmin, 0)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_BLOCKED, out.Outcome)
	require.Len(t, out.Blockers, 1)
	require.Equal(t, "trade_commerce", out.Blockers[0].Reason)
	fences.mu.Lock()
	fences.blockers = []string{"guild_leader"}
	fences.mu.Unlock()
	out, err = s.RequestDeletion(ctx, account, DeletionSourceAdmin, 0)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_OK, out.Outcome)
	require.Equal(t, dspb.AccountDeletionState_ACCOUNT_DELETION_STATE_PENDING, deletionState(t, s, ctx, account))
}

func TestTiDBAccountDeletionOverdueCount(t *testing.T) {
	s, db := accountDirectoryTiDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const account = "wx_deletion.overdue"
	_, err := s.Read(ctx, account, true)
	require.NoError(t, err)
	out, err := s.RequestDeletion(ctx, account, DeletionSourceUser, 0)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_OK, out.Outcome)
	n, err := s.AccountDeletionsOverdue(ctx, DeletionOverdueAfter)
	require.NoError(t, err)
	require.Zero(t, n, "刚到期不算逾期")
	_, err = db.ExecContext(ctx, "UPDATE account_directory SET deletion_execute_after_ms=deletion_execute_after_ms-? WHERE account=?",
		(25 * time.Hour).Milliseconds(), []byte(account))
	require.NoError(t, err)
	n, err = s.AccountDeletionsOverdue(ctx, DeletionOverdueAfter)
	require.NoError(t, err)
	require.Equal(t, 1, n)
}
