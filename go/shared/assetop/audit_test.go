package assetop

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	assetpb "proto/common/asset"
)

// 日对账(audit.go,设计 §6.2):分类口径是纯函数,AuditOnce 只读计数并刷新两组按类 gauge。

const (
	auditDebit  = assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT
	auditCredit = assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT
)

func TestClassifyPending(t *testing.T) {
	const abandonBefore = 1_000_000
	notHere := assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_NOT_HERE
	unknown := assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN

	cases := []struct {
		name          string
		facts         PendingFacts
		abandonBefore uint64
		want          PendingClass
	}{
		// 新插入、还没投过的行 last_outcome 也是 0(UNKNOWN),不能算告警。
		{"新行", PendingFacts{CreatedMs: 2_000_000}, abandonBefore, PendingOnlineRetry},
		{"投过之后 UNKNOWN", PendingFacts{CreatedMs: 2_000_000, Attempts: 1, LastOutcome: unknown}, abandonBefore, PendingAlert},
		// 告警类不能因为又老又离线就被归进别的类而不响。
		{"又老又 UNKNOWN 仍是告警", PendingFacts{CreatedMs: 1, Attempts: 9, LastOutcome: unknown}, abandonBefore, PendingAlert},
		{"超过放弃门槛", PendingFacts{CreatedMs: abandonBefore - 1, Attempts: 9, LastOutcome: notHere}, abandonBefore, PendingAbandonCandidate},
		{"恰在放弃门槛上不算候选", PendingFacts{CreatedMs: abandonBefore, Attempts: 9, LastOutcome: notHere}, abandonBefore, PendingOffline},
		{"门槛为 0 不分出候选", PendingFacts{CreatedMs: 1, Attempts: 9, LastOutcome: notHere}, 0, PendingOffline},
		{"本地合成的 NOT_HERE", PendingFacts{CreatedMs: 2_000_000, Attempts: 6, LastOutcome: notHere}, abandonBefore, PendingOffline},
		// 粘性的 27007 不改变「本地合成」这个判断(carryPartialReason)。
		{"本地 NOT_HERE 带粘性 partial 码", PendingFacts{CreatedMs: 2_000_000, Attempts: 6, LastOutcome: notHere, LastReason: ReasonPartialApplied}, abandonBefore, PendingOffline},
		// scene 自己回的 NOT_HERE 一律带 kAssetPlayerNotHere:玩家在换节点,正常重试中。
		{"scene 回的 NOT_HERE", PendingFacts{CreatedMs: 2_000_000, Attempts: 6, LastOutcome: notHere, LastReason: ReasonPlayerNotHere}, abandonBefore, PendingOnlineRetry},
		{"RETRY", PendingFacts{CreatedMs: 2_000_000, Attempts: 3, LastOutcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_RETRY, LastReason: ReasonInBattle}, abandonBefore, PendingOnlineRetry},
		{"未落盘的 APPLIED", PendingFacts{CreatedMs: 2_000_000, Attempts: 1, LastOutcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED}, abandonBefore, PendingOnlineRetry},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClassifyPending(c.facts, c.abandonBefore); got != c.want {
				t.Fatalf("ClassifyPending = %s,期望 %s", got, c.want)
			}
		})
	}
}

func TestPendingClassNames(t *testing.T) {
	want := map[PendingClass]string{
		PendingOnlineRetry:      "online_retry",
		PendingOffline:          "offline",
		PendingAlert:            "alert",
		PendingAbandonCandidate: "abandon_candidate",
		PendingClass(0):         "invalid",
		pendingClassCount:       "invalid",
	}
	for c, name := range want {
		if got := c.String(); got != name {
			t.Fatalf("PendingClass(%d) 名字应为 %q,实际 %q", c, name, got)
		}
	}
}

// 逐行累加:同组合并行数、年龄取最老;created_ms 晚于 now 的行年龄按 0 计;输出按 (流, 类别) 排序。
func TestPendingTally(t *testing.T) {
	const now = 10_000_000
	tally := NewPendingTally(now, 0)
	notHere := assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_NOT_HERE

	tally.Add(PendingFacts{Stream: auditCredit, CreatedMs: now - 5_000})
	tally.Add(PendingFacts{Stream: auditDebit, CreatedMs: now - 1_000, Attempts: 6, LastOutcome: notHere})
	tally.Add(PendingFacts{Stream: auditDebit, CreatedMs: now - 9_000, Attempts: 6, LastOutcome: notHere})
	tally.Add(PendingFacts{Stream: auditDebit, CreatedMs: now - 2_000})
	tally.Add(PendingFacts{Stream: auditDebit, CreatedMs: now + 60_000}) // 时钟回拨

	got := tally.Counts()
	want := []ClassCount{
		{Stream: auditDebit, Class: PendingOnlineRetry, Rows: 2, OldestAgeMs: 2_000},
		{Stream: auditDebit, Class: PendingOffline, Rows: 2, OldestAgeMs: 9_000},
		{Stream: auditCredit, Class: PendingOnlineRetry, Rows: 1, OldestAgeMs: 5_000},
	}
	if len(got) != len(want) {
		t.Fatalf("应有 %d 组,实际 %+v", len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 组应为 %+v,实际 %+v", i, want[i], got[i])
		}
	}
}

// auditStore 在 fakeStore 之上实现可选的 PendingClassCounter。
type auditStore struct {
	*fakeStore

	counts           []ClassCount
	err              error
	calls            int
	gotNowMs         uint64
	gotAbandonBefore uint64
	afterCount       func() // 只在实际采样返回边界触发卸任，不提前取消新一轮准入。
}

func (s *auditStore) CountPendingByClass(_ context.Context, nowMs, abandonBeforeMs uint64) ([]ClassCount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.gotNowMs, s.gotAbandonBefore = nowMs, abandonBeforeMs
	if s.err != nil {
		return nil, s.err
	}
	if s.afterCount != nil {
		s.afterCount()
	}
	return append([]ClassCount(nil), s.counts...), nil
}

func newAuditLoop(t *testing.T, store *auditStore) (*Loop, *Metrics) {
	t.Helper()
	return newTestLoop(t, store, appliedApplier(), func(c *LoopConfig) {
		c.Streams = []assetpb.AssetOpStream{auditDebit, auditCredit}
	})
}

func pendingRows(m *Metrics, stream, class string) float64 {
	return testutil.ToFloat64(m.pendingRows.WithLabelValues(stream, class))
}

func pendingClassAge(m *Metrics, stream, class string) float64 {
	return testutil.ToFloat64(m.pendingClassOldestAge.WithLabelValues(stream, class))
}

// 一轮对账:本服务的每条流 × 四类都置值(没回的置 0),同组合并,不在 Streams 里的流照样报,
// 非法类别丢弃;放弃截止时刻由循环按 AbandonAfter 算好传给 Store。
func TestAuditOnceRefreshesClassGauges(t *testing.T) {
	store := &auditStore{fakeStore: newFakeStore(), counts: []ClassCount{
		{Stream: auditDebit, Class: PendingOnlineRetry, Rows: 3, OldestAgeMs: 120_000},
		{Stream: auditDebit, Class: PendingOffline, Rows: 5, OldestAgeMs: 86_400_000},
		{Stream: auditDebit, Class: PendingOnlineRetry, Rows: 1, OldestAgeMs: 300_000}, // 同组再回一次:行数相加、年龄取大
		{Stream: assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT, Class: PendingAlert, Rows: 2, OldestAgeMs: 1_000},
		{Stream: auditDebit, Class: PendingClass(0), Rows: 9, OldestAgeMs: 9},
	}}
	loop, m := newAuditLoop(t, store)

	if err := loop.AuditOnce(context.Background(), testNowMs); err != nil {
		t.Fatalf("AuditOnce 出错: %v", err)
	}
	if store.calls != 1 || store.gotNowMs != testNowMs || store.gotAbandonBefore != abandonCutoffForTest {
		t.Fatalf("计数入参应为 (now=%d, abandonBefore=%d),实际 calls=%d (%d, %d)",
			testNowMs, abandonCutoffForTest, store.calls, store.gotNowMs, store.gotAbandonBefore)
	}
	// 先数序列再逐个读值:WithLabelValues 会顺手建出不存在的序列。
	// 2 条本服务的流 × 4 类 + 1 条配置外的流;非法类别不报。
	if got := testutil.CollectAndCount(m.pendingRows); got != 9 {
		t.Fatalf("assetop_pending_rows 应有 9 条序列,实际 %d", got)
	}
	if got := testutil.CollectAndCount(m.pendingClassOldestAge); got != 9 {
		t.Fatalf("assetop_pending_class_oldest_age_seconds 应有 9 条序列,实际 %d", got)
	}

	checks := []struct {
		stream, class string
		rows, age     float64
	}{
		{"guild_debit", "online_retry", 4, 300},
		{"guild_debit", "offline", 5, 86_400},
		{"guild_debit", "alert", 0, 0},
		{"guild_debit", "abandon_candidate", 0, 0},
		{"guild_credit", "online_retry", 0, 0},
		{"guild_credit", "offline", 0, 0},
		{"trade_debit", "alert", 2, 1},
	}
	for _, c := range checks {
		if got := pendingRows(m, c.stream, c.class); got != c.rows {
			t.Errorf("%s/%s 行数应为 %v,实际 %v", c.stream, c.class, c.rows, got)
		}
		if got := pendingClassAge(m, c.stream, c.class); got != c.age {
			t.Errorf("%s/%s 最老年龄应为 %vs,实际 %v", c.stream, c.class, c.age, got)
		}
	}
	// 原有按 stream 的最老年龄 gauge 不归对账管,一点不动。
	if got := testutil.CollectAndCount(m.pendingOldestAge); got != 0 {
		t.Fatalf("对账不得写 assetop_pending_oldest_age_seconds,实际 %d 条序列", got)
	}
}

// 某一类清空之后 gauge 必须回到 0,否则告警会一直挂着;计数失败时保留上一轮的值。
func TestAuditOnceClearsAndKeepsOnError(t *testing.T) {
	store := &auditStore{fakeStore: newFakeStore(), counts: []ClassCount{
		{Stream: auditDebit, Class: PendingAlert, Rows: 2, OldestAgeMs: 4_000},
	}}
	loop, m := newAuditLoop(t, store)

	if err := loop.AuditOnce(context.Background(), testNowMs); err != nil {
		t.Fatalf("第一轮出错: %v", err)
	}
	store.err = errors.New("连接断了")
	if err := loop.AuditOnce(context.Background(), testNowMs); err == nil {
		t.Fatal("计数失败必须回错误")
	}
	if got := testutil.ToFloat64(m.storeErrorsTotal.WithLabelValues("audit")); got != 1 {
		t.Fatalf("audit 出错指标应为 1,实际 %v", got)
	}
	if got := pendingRows(m, "guild_debit", "alert"); got != 2 {
		t.Fatalf("计数失败时应保留上一轮的值 2,实际 %v", got)
	}

	store.err = nil
	store.counts = nil
	if err := loop.AuditOnce(context.Background(), testNowMs); err != nil {
		t.Fatalf("第三轮出错: %v", err)
	}
	if got := pendingRows(m, "guild_debit", "alert"); got != 0 {
		t.Fatalf("告警类清空后行数应回到 0,实际 %v", got)
	}
	if got := pendingClassAge(m, "guild_debit", "alert"); got != 0 {
		t.Fatalf("告警类清空后年龄应回到 0,实际 %v", got)
	}
}

// 不在 Streams 里的组合(配置漏写)同样不能停在旧值上:下一轮没再回来就置 0,序列保留(不留空窗)。
// 卸任重置之后再当选,上一任报过的组合不会被重新建出来。
func TestAuditOnceZeroesVanishedExtraCombos(t *testing.T) {
	tradeDebit := assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT
	store := &auditStore{fakeStore: newFakeStore(), counts: []ClassCount{
		{Stream: tradeDebit, Class: PendingAlert, Rows: 2, OldestAgeMs: 4_000},
	}}
	loop, m := newAuditLoop(t, store)

	if err := loop.AuditOnce(context.Background(), testNowMs); err != nil {
		t.Fatalf("第一轮出错: %v", err)
	}
	if got := pendingRows(m, "trade_debit", "alert"); got != 2 {
		t.Fatalf("配置外的组合应照样报出来,实际 %v", got)
	}

	store.counts = nil
	if err := loop.AuditOnce(context.Background(), testNowMs); err != nil {
		t.Fatalf("第二轮出错: %v", err)
	}
	// 先数序列再逐个读值:WithLabelValues 会顺手建出不存在的序列。2 条流 × 4 类 + 置 0 的那一组。
	if got := testutil.CollectAndCount(m.pendingRows); got != 9 {
		t.Fatalf("消失的组合应置 0 而不是删掉,assetop_pending_rows 应有 9 条序列,实际 %d", got)
	}
	if got := pendingRows(m, "trade_debit", "alert"); got != 0 {
		t.Fatalf("配置外的组合消失后行数应回到 0,实际 %v", got)
	}
	if got := pendingClassAge(m, "trade_debit", "alert"); got != 0 {
		t.Fatalf("配置外的组合消失后年龄应回到 0,实际 %v", got)
	}

	loop.resetAuditGauges()
	if err := loop.AuditOnce(context.Background(), testNowMs); err != nil {
		t.Fatalf("重新当选后一轮出错: %v", err)
	}
	if got := testutil.CollectAndCount(m.pendingRows); got != 8 {
		t.Fatalf("卸任重置后不该再建出上一任的配置外组合,应只有 8 条序列,实际 %d", got)
	}
}

// 放弃关闭时不分出 abandon_candidate:截止时刻传 0。
func TestAuditOnceWithoutAbandon(t *testing.T) {
	store := &auditStore{fakeStore: newFakeStore()}
	loop, _ := newTestLoop(t, store, appliedApplier(), func(c *LoopConfig) { c.AbandonAfter = 0 })

	if err := loop.AuditOnce(context.Background(), testNowMs); err != nil {
		t.Fatalf("AuditOnce 出错: %v", err)
	}
	if store.gotAbandonBefore != 0 {
		t.Fatalf("放弃关闭时截止时刻应传 0,实际 %d", store.gotAbandonBefore)
	}
}

// Store 没实现 PendingClassCounter:不是错误,只是不跑。
func TestAuditOnceWithoutCounterIsNoop(t *testing.T) {
	loop, m := newTestLoop(t, newFakeStore(), appliedApplier(), nil)
	if err := loop.AuditOnce(context.Background(), testNowMs); err != nil {
		t.Fatalf("未实现对账计数时应回 nil,实际 %v", err)
	}
	if got := testutil.CollectAndCount(m.pendingRows); got != 0 {
		t.Fatalf("不跑对账就不该有按类序列,实际 %d", got)
	}
}

// RunMaintenance 一启动就先跑一轮对账(换主之后 gauge 不该空 24 小时),
// 退出(卸任)时清空按类 gauge(否则旧执行者的值会和新执行者的一起被 max() 取到)。
func TestRunMaintenanceAuditsFirstAndResetsOnExit(t *testing.T) {
	store := &auditStore{fakeStore: newFakeStore(), counts: []ClassCount{
		{Stream: auditDebit, Class: PendingOffline, Rows: 1, OldestAgeMs: 1_000},
	}}
	loop, m := newAuditLoop(t, store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store.afterCount = cancel // 首轮已准入并取得样本后卸任，随后退出并清空 gauge。
	loop.RunMaintenance(ctx)

	if store.calls != 1 {
		t.Fatalf("启动时应先跑一轮对账,实际 %d 轮", store.calls)
	}
	if got := testutil.CollectAndCount(m.pendingRows); got != 0 {
		t.Fatalf("退出时应清空 assetop_pending_rows,实际还剩 %d 条", got)
	}
	if got := testutil.CollectAndCount(m.pendingClassOldestAge); got != 0 {
		t.Fatalf("退出时应清空 assetop_pending_class_oldest_age_seconds,实际还剩 %d 条", got)
	}
}

// 已卸任的调用没有新轮准入，不能靠测试用旧 ctx 强制执行一次。
func TestRunMaintenanceRejectsAlreadyCanceledParent(t *testing.T) {
	store := &auditStore{fakeStore: newFakeStore()}
	loop, m := newAuditLoop(t, store)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	loop.RunMaintenance(ctx)
	if store.calls != 0 {
		t.Fatalf("已取消的新维护轮不得访问 store，实际 %d 次", store.calls)
	}
	if got := testutil.CollectAndCount(m.pendingRows); got != 0 {
		t.Fatalf("未准入维护轮不得留下 gauge，实际 %d 条", got)
	}
}
