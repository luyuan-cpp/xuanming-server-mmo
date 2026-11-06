package commerce

// manual_db_test.go —— 人工恢复(manual.go)的真库回归:合同行锁、版本条件、留痕,以及引擎接手发号。
// 门控同 tick_isolation_db_test.go(COMMERCE_TEST_MYSQL_DSN + COMMERCE_REQUIRE_MYSQL=1,或 TiDB 那一组);
// 都没设时 Skip,Skip 不代表通过。

import (
	"context"
	"errors"
	"testing"
	"time"

	"shared/assetop"

	"github.com/stretchr/testify/require"
)

// 上架后被隔离的寄售整单退回:计划不写库;执行后版本加一、留痕、新腿不带 op_id;同一条命令重跑不再生效;
// 引擎下一轮给退回腿发号写 outbox,到账后合同按取消结案,只退一次。
func TestRealDBManualRefundOfQuarantinedConsignment(t *testing.T) {
	e := realDBEngine(t)
	ctx := context.Background()
	resolve := func(opID uint64) {
		t.Helper()
		ok, err := assetop.ResolveManually(ctx, e.Repo.Ops, assetop.ManualResolution{
			OpID: opID, Final: assetop.StatusApplied, Operator: "ops-regression", Reason: "按 scene 流水 correlation_id 判定",
		}, nil, time.Now)
		require.NoError(t, err)
		require.True(t, ok)
	}

	offer := consignment(t, e, 61, "manual-refund-01")
	tick(t, e)
	_, w := state(t, e, offer.CommerceId)
	resolve(leg(w, "seller_debit").OpId)
	tick(t, e)
	r, w := state(t, e, offer.CommerceId)
	require.Equal(t, "open", w.Stage)

	stage, quarantined, err := e.quarantine(ctx, r, errors.New("回归用例注入的坏数据"))
	require.NoError(t, err)
	require.True(t, quarantined)
	require.Equal(t, "open", stage)
	r, w = state(t, e, offer.CommerceId)
	require.Equal(t, Manual, r.Status)
	require.Equal(t, "open", w.Quarantine.GetStage())
	require.Equal(t, manualReasonCorrupt, w.Quarantine.GetReason())

	req := ManualRequest{CommerceID: offer.CommerceId, ExpectVersion: r.Version, Action: ManualRefund, Operator: "ops", Reason: "ticket-1"}
	plan, err := ResolveManual(ctx, e.Repo, req, e.now(), false)
	require.NoError(t, err)
	require.Equal(t, []string{"seller_debit/refund"}, plan.NewLegs)
	unchanged, _ := state(t, e, offer.CommerceId)
	require.Equal(t, r.Version, unchanged.Version, "只算计划不写库")

	result, err := ResolveManual(ctx, e.Repo, req, e.now(), true)
	require.NoError(t, err)
	require.Equal(t, r.Version, result.VersionBefore)
	r2, w2 := state(t, e, offer.CommerceId)
	require.Equal(t, r.Version+1, r2.Version)
	require.Equal(t, CancelPending, r2.Status)
	require.Equal(t, "return", w2.Stage)
	refund := leg(w2, "seller_debit/refund")
	require.NotNil(t, refund)
	require.Zero(t, refund.OpId, "工具不发号,由引擎发")
	require.Len(t, w2.ManualActions, 1)
	require.Equal(t, "ops", w2.ManualActions[0].Operator)
	require.Equal(t, r.Version, w2.ManualActions[0].VersionBefore)

	_, err = ResolveManual(ctx, e.Repo, req, e.now(), true)
	require.ErrorIs(t, err, ErrManualVersionChanged, "同一条命令重跑不能再加一次腿")

	tick(t, e)
	_, w3 := state(t, e, offer.CommerceId)
	refund = leg(w3, "seller_debit/refund")
	require.NotZero(t, refund.OpId)
	require.Equal(t, 1, opCount(t, e, 61, 4), "只退一次")

	resolve(refund.OpId)
	tick(t, e)
	r4, w4 := state(t, e, offer.CommerceId)
	require.Equal(t, Cancelled, r4.Status)
	require.Equal(t, "done", w4.Stage)
	require.Equal(t, 1, opCount(t, e, 61, 4))
}

// 没转人工的合同不能处理;不存在的合同回 ErrManualNotFound。
func TestRealDBManualRefusesLiveAndMissingContracts(t *testing.T) {
	e := realDBEngine(t)
	ctx := context.Background()
	offer := consignment(t, e, 62, "manual-live-01")
	tick(t, e)
	r, _ := state(t, e, offer.CommerceId)

	_, err := ResolveManual(ctx, e.Repo, ManualRequest{CommerceID: offer.CommerceId, ExpectVersion: r.Version, Action: ManualResume, Operator: "ops", Reason: "t"}, e.now(), true)
	require.ErrorIs(t, err, ErrManualNotAllowed)
	after, _ := state(t, e, offer.CommerceId)
	require.Equal(t, r.Version, after.Version)

	_, err = ResolveManual(ctx, e.Repo, ManualRequest{CommerceID: 987654321, ExpectVersion: 1, Action: ManualResume, Operator: "ops", Reason: "t"}, e.now(), true)
	require.ErrorIs(t, err, ErrManualNotFound)
}
