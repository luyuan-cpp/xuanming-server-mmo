package logic

import (
	"context"
	"errors"
	"time"

	"match/internal/authority"
	"match/internal/metrics"
	"match/internal/svc"
)

// 剧本审核流水 match_battle_script_review 的保留期清理(docs/design/battle-script.md §6.4),写法同观战意图的保留期清理
// (globalSpectatorIntentPurge):每个副本各跑各的,只做幂等删除,同时跑无害;StartRelayLoop 管 safego 点位、每轮 recover、
// 活性(match_loop_last_run_unixtime)与停机排空。一轮清几页见 retention_purge.go。
const (
	battleScriptReviewPurgeInterval = time.Minute
	// battleScriptReviewPurgeBudget 是一步(清一页:一次读加一次删;或读最老一行)的时间预算,同观战意图清理。
	battleScriptReviewPurgeBudget = 3 * time.Second
	// battleScriptReviewPurgeBatch 是每页最多删的行数。
	battleScriptReviewPurgeBatch = 1000
)

// battleScriptReviewPurger 是保留期清理要的能力(生产实现 *authority.Store),收窄成接口便于替身测试。
type battleScriptReviewPurger interface {
	PurgeExpiredBattleScriptReviews(ctx context.Context, retention time.Duration, after authority.BattleScriptReviewCursor, limit int) (authority.BattleScriptReviewCursor, int64, error)
	OldestBattleScriptReviewMs(ctx context.Context) (uint64, error)
}

// battleScriptReviewPurge 是清理循环的状态。游标只由这一个循环的 goroutine 读写,不加锁;进程重启从头扫一遍即可。
type battleScriptReviewPurge struct {
	retention time.Duration
	after     authority.BattleScriptReviewCursor
}

// startBattleScriptReviewPurge 起审核流水的保留期清理循环,保留期取 BattleScript.ReviewRetentionDays。
func startBattleScriptReviewPurge(ctx context.Context, s *svc.ServiceContext) {
	purge := &battleScriptReviewPurge{retention: s.Config.BattleScript.ReviewRetention()}
	metrics.SetRetentionMaxAge(metrics.RetentionPurgeBattleScriptReview, purge.retention)
	s.StartRelayLoop(ctx, "match.battle_script_review.purge", battleScriptReviewPurgeInterval, func(ctx context.Context) { purge.round(ctx, s.Authority) })
}

// round 跑一轮,结局交给 observeRetentionRound(失败计数与日志;搬库冻结不算失败)。
// 到期的行没删掉由告警 MatchRetentionBehind 报(deploy/k8s/match-alerts.yaml)。
func (p *battleScriptReviewPurge) round(ctx context.Context, purger battleScriptReviewPurger) {
	observeRetentionRound(metrics.RetentionPurgeBattleScriptReview, p.purgeAndMeasure(ctx, purger))
}

// purgeAndMeasure 连续清页,再读表里现存最老一行的时刻报给逾期告警。清理失败的那一页不动游标。清理失败也照读最老一行:
// 删不动而读得动时(例如 DELETE 超时),指标照样反映表里的行在变老。
func (p *battleScriptReviewPurge) purgeAndMeasure(ctx context.Context, purger battleScriptReviewPurger) error {
	purgeErr := drainRetentionPages(func() (bool, error) {
		pageCtx, cancel := context.WithTimeout(ctx, battleScriptReviewPurgeBudget)
		defer cancel()
		next, deleted, err := purger.PurgeExpiredBattleScriptReviews(pageCtx, p.retention, p.after, battleScriptReviewPurgeBatch)
		if err != nil {
			return false, err
		}
		p.after = next
		metrics.ObserveBattleScriptReviewPurged(deleted)
		return next != (authority.BattleScriptReviewCursor{}), nil
	})
	readCtx, cancel := context.WithTimeout(ctx, battleScriptReviewPurgeBudget)
	defer cancel()
	oldestMs, readErr := purger.OldestBattleScriptReviewMs(readCtx)
	if readErr == nil {
		metrics.SetRetentionOldest(metrics.RetentionPurgeBattleScriptReview, oldestMs)
	}
	return errors.Join(purgeErr, readErr)
}
