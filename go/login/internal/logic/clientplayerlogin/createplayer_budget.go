package clientplayerloginlogic

import (
	"context"
	"time"

	"login/internal/logic/pkg/createbudget"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/metric"
)

// CreatePlayer 持锁后的几跳(发号 / 名字登记 / player_route 登记 / SQL 账号目录提交)按 handler 截止分摊
// (login/internal/logic/pkg/createbudget;docs/design/client-request-ordering.md §5.3、§6.1)。
// 每一跳的 ctx 都从请求 ctx 派生:截止 = min(此刻 + 本跳上限, handler 截止 − 给后面各跳留的余量),
// 所以任何一跳拿到的截止都不晚于 handler 截止;剩下的不够本跳下限就不发这一跳,走与截止之后相同的放弃路径。

// createPlayerNow 是截止分摊取「此刻」的入口。只为单测注入可控的时钟;生产路径不改它(理由同 afterFunc)。
var createPlayerNow = time.Now

// createPlayerBudgetSkipTotal 数因为剩下的 handler 预算不够某一跳的下限而没有发出的跳。稳态为 0;
// 非 0 说明前面的步骤(Redis、SQL 账号目录读、发号、名字登记)在变慢,定位具体请求看同一处的 INFO 日志。
var createPlayerBudgetSkipTotal = metric.NewCounterVec(&metric.CounterVecOpts{
	Namespace: createPlayerMetricNamespace,
	Subsystem: createPlayerMetricSubsystem,
	Name:      "budget_skip_total",
	Help: "CreatePlayer hops not sent because the remaining handler budget was below the hop minimum. " +
		"Hop: mint | name | register | commit.",
	Labels: []string{"hop"},
})

// createBudgetPlan 返回本次建角的截止分摊方案。player_route 登记的上限取 homezone.Resolver 规范化之后的
// RegisterTimeout:与 login 启动门禁(login.go)核对的是同一个配置项、同一个缺省值。
func (l *CreatePlayerLogic) createBudgetPlan() createbudget.Plan {
	var register time.Duration
	if hz := l.svcCtx.HomeZone; hz != nil {
		register = hz.RegisterTimeout
	}
	return createbudget.New(register)
}

// enterHop 从请求 ctx 派生下一跳的 ctx。请求 ctx 已结束、或剩下的预算不够这一跳的下限时返回 ok = false
// 并记一条日志,调用方不发这一跳、按各自的补偿路径放弃(此刻这一跳一个 RPC 都没发,是「确定没写」)。
// ok = true 时调用方用完即调 cancel。
func (l *CreatePlayerLogic) enterHop(plan createbudget.Plan, hop createbudget.Hop, account string, playerID uint64) (context.Context, context.CancelFunc, bool) {
	ctx, cancel, err := plan.Derive(l.ctx, hop, createPlayerNow())
	if err == nil {
		return ctx, cancel, true
	}
	if l.ctx.Err() == nil {
		createPlayerBudgetSkipTotal.Inc(hop.String())
	}
	logx.Infof("[create-player] not sending the %s hop, no further writes (account=%s player_id=%d): %v",
		hop, account, playerID, err)
	return nil, nil, false
}
