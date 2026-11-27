package data

// 帮战入账分流的不连库用例(数据覆写审计 r2 第 4 轮 r2-gw#5 / #6;设计见
// docs/planning/guild-play-v2-implementation-plan.md 末节「帮战结算的入账分流」)。
// 真库用例:未决已满、到期收尾、保留期清理在 war_repo_test.go(GUILD_TEST_MYSQL_DSN);
// 护栏冻结与删除在 war_credit_owed_tidb_test.go(GUILD_ROLE_TEST_DSN)。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	assetpb "proto/common/asset"
	rollbackpb "proto/common/rollback"
	pb "proto/guild"
	"shared/assetop"
	"shared/rolefence"
)

// TestWarCreditOwedColumnsCoverEveryProtoField:待入账表的手写列清单与 GuildWarCreditOwedRecord 的字段序逐一一致,
// 插入语句的占位符数等于字段数。加字段却忘了同步,新列会永远读成零值。
func TestWarCreditOwedColumnsCoverEveryProtoField(t *testing.T) {
	fields := (&pb.GuildWarCreditOwedRecord{}).ProtoReflect().Descriptor().Fields()
	want := make([]string, 0, fields.Len())
	for i := 0; i < fields.Len(); i++ {
		want = append(want, string(fields.Get(i).Name()))
	}
	require.Equal(t, want, econSplitColumns(warCreditOwedColumns))
	assert.Equal(t, fields.Len(), strings.Count(sqlInsertWarCreditOwed, "?"))
}

// TestClassifyCreditFence:护栏按「目标」口径判,一名目标收不了不让整笔失败(r2-gw#6)。
// 缺行与「冻结而还没自愈过」先回 stale 让 Guard.RunTx 自愈;自愈后仍冻结才改记待入账;墓碑直接放弃,不必自愈。
func TestClassifyCreditFence(t *testing.T) {
	open := rolefence.Fence{PlayerID: 11, Account: "seller", OwnerRevision: 1, Version: 1, HomeZone: 1}
	frozen := open
	frozen.FreezeToken = 901
	removed := rolefence.Fence{PlayerID: 11, OwnerRevision: 2, Version: 3, HomeZone: 1}
	boom := errors.New("lock wait")
	cases := []struct {
		name      string
		fence     rolefence.Fence
		lockErr   error
		healed    bool
		want      creditFenceVerdict
		wantStale bool
		wantErr   error
	}{
		{"在役:照常入账", open, nil, false, creditFenceOpen, false, nil},
		{"副本缺行:先自愈", rolefence.Fence{PlayerID: 11}, rolefence.ErrMissing, false, creditFenceOpen, true, nil},
		{"自愈后仍缺行:再回 stale,RunTx 当真实拒绝", rolefence.Fence{PlayerID: 11}, rolefence.ErrMissing, true, creditFenceOpen, true, nil},
		{"冻结且没自愈过:先自愈(可能只是解冻推送丢了)", frozen, nil, false, creditFenceOpen, true, nil},
		{"自愈后仍冻结:改记待入账", frozen, nil, true, creditFenceFrozen, false, nil},
		{"墓碑:放弃,不必自愈", removed, nil, false, creditFenceRemoved, false, nil},
		{"锁副本出错:原样上抛", rolefence.Fence{PlayerID: 11}, boom, false, creditFenceOpen, false, boom},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, stale, err := classifyCreditFence(tc.fence, tc.lockErr, tc.healed)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantStale, stale)
			if !tc.wantStale {
				assert.Equal(t, tc.want, got)
			}
		})
	}
}

// 没装配护栏(独立仓储夹具)时不碰事务,全部按放行;结论列表按 player_id 升序。
func TestLockCreditFencesWithoutGuardAndVerdictLists(t *testing.T) {
	healed := false
	verdicts, err := (&GuildRepo{}).lockCreditFences(context.Background(), nil, []uint64{3, 1}, &healed)
	require.NoError(t, err)
	assert.Nil(t, verdicts)
	assert.Equal(t, creditFenceOpen, verdicts[3], "查不到的目标按放行")
	assert.False(t, healed)

	v := map[uint64]creditFenceVerdict{9: creditFenceFrozen, 3: creditFenceFrozen, 5: creditFenceRemoved, 7: creditFenceOpen}
	assert.Equal(t, []uint64{3, 9}, playersWithVerdict(v, creditFenceFrozen))
	assert.Equal(t, []uint64{5}, playersWithVerdict(v, creditFenceRemoved))
	assert.Nil(t, playersWithVerdict(nil, creditFenceFrozen))
}

// 待入账行转成指令时原样带过去:指令号就是结算前预发的那个(不另发号,同一笔至多一条指令),种类、交易类型、ref、周期键、
// 奖励包都来自行里;不持租约、立刻可领,没有截止。
func TestWarCreditRecordCarriesTheOwedCreditVerbatim(t *testing.T) {
	c := WarCreditOwed{
		PlayerID: 11, OpID: 9001, GuildID: 44,
		Kind: pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_BOUNTY_PAYOUT, TxType: rollbackpb.TransactionType_TX_GUILD_BOUNTY_PAYOUT,
		RefID: 2, PeriodKey: 20261126, Payload: []byte{1, 2, 3}, SourceID: 77001, CreatedMs: 5,
	}
	rec := warCreditRecord(c, assetop.Alloc{Epoch: 123, Seq: 17}, warOpLease{nextAttemptMs: 1000}, 1000)
	assert.Equal(t, uint64(9001), rec.GetOpId())
	assert.Equal(t, uint64(11), rec.GetPlayerId())
	assert.Equal(t, uint64(44), rec.GetGuildId())
	assert.Equal(t, uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT), rec.GetStream())
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING, rec.GetStatus())
	assert.Equal(t, pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_BOUNTY_PAYOUT, rec.GetKind())
	assert.Equal(t, uint32(rollbackpb.TransactionType_TX_GUILD_BOUNTY_PAYOUT), rec.GetTxType())
	assert.Equal(t, uint64(17), rec.GetSeq())
	assert.Equal(t, uint64(123), rec.GetStreamEpoch())
	assert.Equal(t, uint32(2), rec.GetRefId())
	assert.Equal(t, uint32(1), rec.GetRefCount())
	assert.Equal(t, uint32(20261126), rec.GetPeriodKey())
	assert.Equal(t, []byte{1, 2, 3}, rec.GetPayload())
	assert.Equal(t, uint64(1000), rec.GetNextAttemptMs())
	assert.Equal(t, uint64(1000), rec.GetCreatedMs())
	assert.Zero(t, rec.GetDeadlineMs())
	assert.Zero(t, rec.GetLeaseUntilMs())
	assert.Zero(t, rec.GetLeaseToken())
}
