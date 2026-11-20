package logic

import (
	"context"
	"errors"
	"testing"
	"time"

	"match/internal/authority"
	"match/internal/metrics"

	"github.com/stretchr/testify/require"
)

// fakeReviewPurger 记下每一页收到的保留期与翻页位置;page 给出这一页的结论,oldest 是读最老一行的结论。
type fakeReviewPurger struct {
	retentions []time.Duration
	afters     []authority.BattleScriptReviewCursor
	page       func(after authority.BattleScriptReviewCursor) (authority.BattleScriptReviewCursor, error)
	oldestMs   uint64
	oldestErr  error
}

func (f *fakeReviewPurger) PurgeExpiredBattleScriptReviews(_ context.Context, retention time.Duration, after authority.BattleScriptReviewCursor, _ int) (authority.BattleScriptReviewCursor, int64, error) {
	f.retentions = append(f.retentions, retention)
	f.afters = append(f.afters, after)
	next, err := f.page(after)
	return next, 0, err
}

func (f *fakeReviewPurger) OldestBattleScriptReviewMs(context.Context) (uint64, error) {
	return f.oldestMs, f.oldestErr
}

// 审核流水清理一轮连续清页:满页接着清,扫到末尾这一轮结束;失败的那一页不动游标、记一次失败,下一轮从它重试;
// 每一页都按配置的保留期删;清理失败的那一轮照样报最老一行。
func TestBattleScriptReviewPurgeDrainsPagesAndKeepsCursorOnFailure(t *testing.T) {
	const retention = 30 * 24 * time.Hour
	first := authority.BattleScriptReviewCursor{ReviewedMs: 1, ScriptID: 2, Revision: 3}
	failing := false
	purger := &fakeReviewPurger{oldestMs: 1_700_000_000_000, page: func(after authority.BattleScriptReviewCursor) (authority.BattleScriptReviewCursor, error) {
		switch {
		case after == (authority.BattleScriptReviewCursor{}):
			return first, nil // 满页
		case failing:
			return after, errors.New("TiDB unavailable")
		default:
			return authority.BattleScriptReviewCursor{}, nil // 不满一页:扫到末尾
		}
	}}
	purge := &battleScriptReviewPurge{retention: retention}
	failures := metrics.RetentionPurgeFailureValue(metrics.RetentionPurgeBattleScriptReview)

	purge.round(context.Background(), purger)
	require.Equal(t, []authority.BattleScriptReviewCursor{{}, first}, purger.afters, "一轮里满页接着清下一页")
	require.Equal(t, authority.BattleScriptReviewCursor{}, purge.after, "扫到末尾回到开头")
	require.Equal(t, float64(1_700_000_000), metrics.RetentionOldestValue(metrics.RetentionPurgeBattleScriptReview))

	failing = true
	purger.oldestMs = 1_700_000_060_000
	purge.round(context.Background(), purger)
	require.Equal(t, first, purge.after, "失败的那一页不动游标")
	require.Equal(t, failures+1, metrics.RetentionPurgeFailureValue(metrics.RetentionPurgeBattleScriptReview))
	require.Equal(t, float64(1_700_000_060), metrics.RetentionOldestValue(metrics.RetentionPurgeBattleScriptReview), "删不动时照样报最老一行")

	failing = false
	purge.round(context.Background(), purger)
	require.Equal(t, []authority.BattleScriptReviewCursor{{}, first, {}, first, first}, purger.afters, "下一轮从失败的那一页重试")
	require.Equal(t, failures+1, metrics.RetentionPurgeFailureValue(metrics.RetentionPurgeBattleScriptReview), "成功的一轮不计失败")
	require.Equal(t, []time.Duration{retention, retention, retention, retention, retention}, purger.retentions)
}

// 读最老一行失败:这一轮记一次失败,指标停在上一次读到的时刻(告警规则用 time() 减,它照样变老),清理的游标照常前进。
func TestBattleScriptReviewPurgeCountsFailedOldestRead(t *testing.T) {
	purger := &fakeReviewPurger{oldestMs: 1_700_000_000_000, page: func(authority.BattleScriptReviewCursor) (authority.BattleScriptReviewCursor, error) {
		return authority.BattleScriptReviewCursor{}, nil
	}}
	purge := &battleScriptReviewPurge{retention: time.Hour}
	purge.round(context.Background(), purger)
	failures := metrics.RetentionPurgeFailureValue(metrics.RetentionPurgeBattleScriptReview)

	purger.oldestMs, purger.oldestErr = 0, errors.New("TiDB unavailable")
	purge.round(context.Background(), purger)

	require.Equal(t, failures+1, metrics.RetentionPurgeFailureValue(metrics.RetentionPurgeBattleScriptReview))
	require.Equal(t, float64(1_700_000_000), metrics.RetentionOldestValue(metrics.RetentionPurgeBattleScriptReview))
}

// 一直是满页时一轮清到页数上限为止,不占着循环;扫完或出错立刻结束。
func TestDrainRetentionPagesStopsAtPageCapEndOrError(t *testing.T) {
	pages := 0
	require.NoError(t, drainRetentionPages(func() (bool, error) { pages++; return true, nil }))
	require.Equal(t, retentionPurgePagesPerRound, pages)

	pages = 0
	require.NoError(t, drainRetentionPages(func() (bool, error) { pages++; return pages < 3, nil }))
	require.Equal(t, 3, pages, "扫完(more=false)就结束")

	pages = 0
	failed := errors.New("page failed")
	require.ErrorIs(t, drainRetentionPages(func() (bool, error) { pages++; return true, failed }), failed)
	require.Equal(t, 1, pages, "出错不再清下一页")
}
