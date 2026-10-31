package logic

import (
	"context"
	"time"

	"match/internal/svc"
)

// StartMatcherLoop 启动匹配的全部后台循环。多副本安全:撮合按池归属分工(poolowner),提交点在 SQL 事务里复核,
// 恢复靠工作租约与行锁;循环本身不持任何 Redis 锁。
func StartMatcherLoop(ctx context.Context, svcCtx *svc.ServiceContext) {
	svcCtx.GlobalGatherSlots = make(chan struct{}, svcCtx.Config.GlobalMatch.GatherSlotCount())
	readyCursor := &globalReadyCursor{}
	svcCtx.StartRelayLoop(ctx, "match.global.ready.scan", 500*time.Millisecond, func(ctx context.Context) { readyCursor.round(ctx, svcCtx) })
	// 到期终局恢复自成一个循环(2026-10-30,global_outcome_recover.go):翻页游标是这个循环的局部状态,只有它读写;
	// 不按池归属分工,所有副本都跑,幂等靠 SQL 行锁与条件。装配在 GlobalGatherSlots 之后,与成局发起的初始化顺序无关。
	outcomeRecovery := &globalOutcomeRecovery{}
	svcCtx.StartRelayLoop(ctx, "match.global.outcome.recover", globalOutcomeRecoverInterval, func(ctx context.Context) {
		outcomeRecovery.round(ctx, svcCtx.Authority, svcCtx.HoldRelay)
	})
	// 已提交成局的 Pending 恢复与新成局分开两个循环:恢复变慢不拖住撮合节拍。
	svcCtx.StartSourceLoop(ctx, "match.global", 500*time.Millisecond, func(ctx context.Context) { runGlobalRound(ctx, svcCtx) })
	StartGlobalPoolMatcher(ctx, svcCtx)
	// 到期成局巡检只报数(2026-10-22 评审补,global_expired_audit.go),与上面的恢复循环分开:不占它的 500ms 节拍。
	svcCtx.StartRelayLoop(ctx, "match.global.expired.audit", globalExpiredAuditInterval, func(ctx context.Context) { auditStrandedExpiredFormations(ctx, svcCtx) })
	// 观战索引定期清理与意图保留期清理(global_spectator.go)。
	startGlobalSpectateMaintenance(ctx, svcCtx, globalSpectateCleanupInterval, globalSpectatorIntentPurgeInterval)
}
