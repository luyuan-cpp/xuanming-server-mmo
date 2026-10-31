package logic

import (
	"context"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"match/internal/authority"
	"match/internal/metrics"
	"match/internal/svc"
)

// globalExpiredAuditInterval 是到期成局巡检的节拍。只报数、不改状态，一分钟一次足够让值班在查询窗口内看到；
// 每个 match 副本各自报，计数随副本数放大，规则按 > 0 判即可。
const globalExpiredAuditInterval = time.Minute

// globalOutcomeAuditor 是每分钟巡检要的只读能力(生产实现 *authority.Store),收窄成接口便于替身测试。
type globalOutcomeAuditor interface {
	CountStrandedExpiredFormations(context.Context) (uint64, error)
	ReadOutcomeOutboxBacklog(context.Context) (authority.OutcomeOutboxBacklog, error)
}

// globalAuditGauges 是巡检读数的 gauge 出口(2026-10-30,评审 B-4 / B-6):到期滞留成局的存量,
// 与终局发件箱三类(result / first / waiting)的积压条数和最老待投年龄。label 只有 kind,不带 battle_id / player_id。
type globalAuditGauges struct {
	strandedExpired func(count uint64)
	outboxBacklog   func(kind string, rows uint64, oldestAgeSeconds float64)
}

// productionAuditGauges 是生产用的 gauge 出口(metrics/outcome.go)。
func productionAuditGauges() globalAuditGauges {
	return globalAuditGauges{
		strandedExpired: metrics.SetGlobalExpiredStranded,
		outboxBacklog:   metrics.SetOutcomeOutboxBacklog,
	}
}

// auditStrandedExpiredFormations 是每分钟的全球终局巡检(matcher.go 按这个名字挂到 relay 循环上):
//   - 过了权威截止、却进不了到期终局恢复的成局（2026-10-22 评审补，social-match-battle#0）;
//   - 终局发件箱的积压(2026-10-30,评审 B-6)。
func auditStrandedExpiredFormations(ctx context.Context, s *svc.ServiceContext) {
	auditGlobalOutcomes(ctx, s.Authority, productionAuditGauges())
}

func auditGlobalOutcomes(ctx context.Context, a globalOutcomeAuditor, gauges globalAuditGauges) {
	auditStrandedExpired(ctx, a, gauges)
	auditOutcomeOutboxBacklog(ctx, a, gauges)
}

// auditStrandedExpired 报告过了权威截止、却进不了到期终局恢复的成局。AcquireWork 的门禁（awaitsExpiredOutcome）让它们
// 领不到租约，RecoverExpiredOutcomes 又因候选形状不合格不选它们，票据静默停在 state 2/3。这里不尝试修复：形状不合格说明
// 原件或能力证据有问题，自动 ABORT 会在证据不足时作废可能已经打完的一局。处置见 cross-zone-matchmaking.md §13.3。
// 事件计数进 global_flow_total{stage=recover,outcome=expired_unrecoverable}（不带 battle_id），明细由值班按文档里的 SQL 查;
// 存量写 gauge(0 也写:处置之后读数要能回落;读失败则保留上一次读数,不伪造 0)。
// 形状合格、却过不了 Create 原件核对的另一类由到期恢复每次访问时计 global_flow_total{stage=recover,outcome=original_mismatch}
// (authority.ErrUnrecoverableFormation),SQL 判不出,不在这个存量里。
func auditStrandedExpired(ctx context.Context, a globalOutcomeAuditor, gauges globalAuditGauges) {
	check, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	stranded, err := a.CountStrandedExpiredFormations(check)
	if err != nil {
		if ctx.Err() == nil {
			logx.Errorf("global expired formation audit failed: %v", err)
		}
		return
	}
	gauges.strandedExpired(stranded)
	if stranded == 0 {
		return
	}
	metrics.ObserveGlobalFlow("recover", "expired_unrecoverable")
	logx.Errorf("global formations past deadline cannot reach expired-outcome recovery: count=%d,tickets stay in state 2/3;"+
		"outcome shape check failed(see cross-zone-matchmaking.md 13.3 for the query and handling)", stranded)
}

// auditOutcomeOutboxBacklog 用 COUNT / MIN 刷新终局发件箱三类积压的 gauge。读失败保留上一次读数并打 ERROR。
func auditOutcomeOutboxBacklog(ctx context.Context, a globalOutcomeAuditor, gauges globalAuditGauges) {
	read, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	backlog, err := a.ReadOutcomeOutboxBacklog(read)
	if err != nil {
		if ctx.Err() == nil {
			logx.Errorf("global outcome outbox backlog read failed: %v", err)
		}
		return
	}
	for _, kind := range []struct {
		name    string
		backlog authority.OutboxBacklog
	}{
		{authority.OutboxKindResult, backlog.Result},
		{authority.OutboxKindFirst, backlog.First},
		{authority.OutboxKindWaiting, backlog.Waiting},
	} {
		gauges.outboxBacklog(kind.name, kind.backlog.Rows, kind.backlog.OldestAgeSeconds)
	}
}
