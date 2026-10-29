package logic

import (
	"context"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"match/internal/metrics"
	"match/internal/svc"
)

// globalExpiredAuditInterval 是到期成局巡检的节拍。只报数、不改状态，一分钟一次足够让值班在查询窗口内看到；
// 每个 match 副本各自报，计数随副本数放大，规则按 > 0 判即可。
const globalExpiredAuditInterval = time.Minute

// auditStrandedExpiredFormations 报告过了权威截止、却进不了到期终局恢复的成局（2026-10-22 评审补，
// social-match-battle#0）。AcquireWork 的门禁（awaitsExpiredOutcome）让它们领不到租约，RecoverExpiredOutcomes
// 又因候选形状不合格不选它们，票据静默停在 state 2/3。这里不尝试修复：形状不合格说明原件或能力证据有问题，
// 自动 ABORT 会在证据不足时作废可能已经打完的一局。处置见 cross-zone-matchmaking.md §13.3。
// 计数只进 global_flow_total{stage=recover,outcome=expired_unrecoverable}（不带 battle_id），明细由值班按文档里的 SQL 查。
func auditStrandedExpiredFormations(ctx context.Context, s *svc.ServiceContext) {
	check, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	stranded, err := s.Authority.CountStrandedExpiredFormations(check)
	if err != nil {
		if ctx.Err() == nil {
			logx.Errorf("global expired formation audit failed: %v", err)
		}
		return
	}
	if stranded == 0 {
		return
	}
	metrics.ObserveGlobalFlow("recover", "expired_unrecoverable")
	logx.Errorf("global formations past deadline cannot reach expired-outcome recovery: count=%d,tickets stay in state 2/3;"+
		"outcome shape check failed(see cross-zone-matchmaking.md 13.3 for the query and handling)", stranded)
}
