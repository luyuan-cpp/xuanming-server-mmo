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
	CountOverdueDeadlineAborts(context.Context) (uint64, uint64, error)
}

// globalAuditGauges 是巡检读数的 gauge 出口(2026-10-30,评审 B-4 / B-6):到期滞留成局的存量,
// 与终局发件箱三类(result / first / waiting)的积压条数和最老待投年龄;2026-11-11 加「超期未作废」的存量与最早超期秒数。
// label 只有 kind,不带 battle_id / player_id。
type globalAuditGauges struct {
	strandedExpired func(count uint64)
	outboxBacklog   func(kind string, rows uint64, oldestAgeSeconds float64)
	overdueAborts   func(count uint64, oldestOverdueSeconds float64)
}

// productionAuditGauges 是生产用的 gauge 出口(metrics/outcome.go)。
func productionAuditGauges() globalAuditGauges {
	return globalAuditGauges{
		strandedExpired: metrics.SetGlobalExpiredStranded,
		outboxBacklog:   metrics.SetOutcomeOutboxBacklog,
		overdueAborts:   metrics.SetGlobalDeadlineAbortOverdue,
	}
}

// auditStrandedExpiredFormations 是每分钟的全球终局巡检(matcher.go 按这个名字挂到 relay 循环上):
//   - 过了权威截止、却进不了到期终局恢复的成局（2026-10-22 评审补，social-match-battle#0）;
//   - 终局发件箱的积压(2026-10-30,评审 B-6);
//   - 超期未作废的可裁决成局(2026-11-11)。
func auditStrandedExpiredFormations(ctx context.Context, s *svc.ServiceContext) {
	auditGlobalOutcomes(ctx, s.Authority, productionAuditGauges())
}

func auditGlobalOutcomes(ctx context.Context, a globalOutcomeAuditor, gauges globalAuditGauges) {
	auditStrandedExpired(ctx, a, gauges)
	auditOutcomeOutboxBacklog(ctx, a, gauges)
	auditOverdueDeadlineAborts(ctx, a, gauges)
}

// auditOverdueDeadlineAborts 报「超期未作废」(2026-11-11,authority.Store.CountOverdueDeadlineAborts):自动路径可裁决、
// 截止之后已超过最晚落定时刻仍没有终局的在途成局。match 可用、恢复跟得上时这个数恒为 0;大于 0 说明到期恢复一拍预算清不完积压
// (加副本,吞吐随副本数线性增长),或 match / 它的库长期不可用,scene 可能已按续冻上限解冻(晚到的结算由战斗道具预留兜住)。
// 形状合格却过不了原件核对的局(original_mismatch)永远不会自动落定,也计在里面,用 formationfix 处置。
// 存量写 gauge(0 也写;读失败保留上一次读数,不伪造 0);大于 0 时打一行 ERROR,不带 battle_id(明细按手册的 SQL 查)。
func auditOverdueDeadlineAborts(ctx context.Context, a globalOutcomeAuditor, gauges globalAuditGauges) {
	check, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	overdue, oldestMs, err := a.CountOverdueDeadlineAborts(check)
	if err != nil {
		if ctx.Err() == nil {
			logx.Errorf("global overdue deadline-abort audit failed: %v", err)
		}
		return
	}
	gauges.overdueAborts(overdue, float64(oldestMs)/1000)
	if overdue == 0 {
		return
	}
	logx.Errorf("global formations are overdue for their deadline abort: count=%d oldest_overdue_ms=%d; expired-outcome recovery is not keeping up "+
		"(add match replicas, check the match database), or the formations cannot be adjudicated automatically (see go/match/cmd/formationfix)", overdue, oldestMs)
}

// auditStrandedExpired 报告过了权威截止加宽限、却进不了到期终局恢复的成局(口径与 formationfix 列表里 shape_invalid 一类相同,
// 含 state 4 与终局协议不是 1 的局,2026-11-06)。AcquireWork 的门禁（awaitsExpiredOutcome）让它们领不到租约，
// RecoverExpiredOutcomes 又因候选形状不合格不选它们，票据静默停在 state 2/3/4。这里不尝试修复：形状不合格说明
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
	logx.Errorf("global formations past deadline cannot reach expired-outcome recovery: count=%d,tickets stay occupied;"+
		"outcome shape check failed(list and handle them with go/match/cmd/formationfix, see docs/ops/match-formation-manual-abort-runbook.md)", stranded)
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
