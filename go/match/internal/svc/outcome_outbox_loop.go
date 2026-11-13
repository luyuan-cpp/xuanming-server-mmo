package svc

import (
	"context"
	"time"

	"match/internal/metrics"
	"shared/authoritysql"
	"shared/drainproof"

	"github.com/zeromicro/go-zero/core/logx"
)

const (
	// outcomeOutboxPoint 是终局发件箱每一轮的 safego 点位名(safego_panic_total 的 label,必须是常量)。
	outcomeOutboxPoint = "match.outcome_outbox"
	// outcomeOutboxRoundBudget 是一轮投递的总预算。已经开始的一轮不随循环的 ctx 取消,用这份预算跑完:
	// 停机不会把一次发布、一次入箱截在半路、留下结局未知。活性告警 MatchOutcomeOutboxLoopStalled 的阈值按它取若干倍。
	outcomeOutboxRoundBudget = 40 * time.Second
)

// runOutcomeOutboxLoop 是终局发件箱的投递循环:每 interval 一轮,直到 ctx 结束或中继入口关闭(停机排空)。
// publish 是一轮投递本身(authority.Store.PublishBattleOutcomes),拿到的 ctx 带这一轮的预算。
//
// 发件箱按片分给各 stable 副本(整局结果按局号、个人结算按玩家号,authority/work_owner.go;缺口清单第 25 条),所以:
//
//   - 活性(metrics.OutcomeOutboxLive;docs/design/assetop-redelivery-sharding-20261110.md §14.8,口径在 shared/looplive
//     的包注释):起来时写一次;每一轮正常返回(成败都算)就写这一刻,panic 的一轮不写;有意停止时删掉序列。一个副本的循环停住,
//     它名下的片没人投,别的副本照常推进,告警 MatchOutcomeOutboxLoopStalled 按副本报。
//   - 每一轮各自 recover:一轮 panic 先打 HOLD(结局未知),只丢这一轮,循环接着跑。原来 panic 会一路抛出去把进程打死 ——
//     一条坏的终局行会先打死负责它那一片的副本,片换主之后再打死下一个,整个 match 跟着倒。
//
// 一轮出错(闸门冻结除外:确定没写)按结局未知打 HOLD,行留在发件箱里下一轮重投。
func runOutcomeOutboxLoop(ctx context.Context, relay *drainproof.Session, interval time.Duration, publish func(ctx context.Context) error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	live := metrics.OutcomeOutboxLive
	live.Mark(time.Now())
	for {
		if ctx.Err() != nil {
			live.Stop()
			return
		}
		finish, err := relay.Enter()
		if err != nil {
			// 中继入口已关(停机排空):不再开新的一轮,有意停止。
			live.Stop()
			return
		}
		live.Round(outcomeOutboxPoint, func() {
			defer finish()
			defer func() {
				if recovered := recover(); recovered != nil {
					relay.Hold()
					panic(recovered)
				}
			}()
			call, cancel := context.WithTimeout(context.WithoutCancel(ctx), outcomeOutboxRoundBudget)
			defer cancel()
			err := publish(call)
			if err == nil {
				return
			}
			if !authoritysql.IsWriterFrozen(err) {
				relay.Hold()
			}
			if ctx.Err() == nil {
				logx.Errorf("outcome original delivery awaiting recovery: %v", err)
			}
		})
		select {
		case <-ctx.Done():
			live.Stop()
			return
		case <-ticker.C:
		}
	}
}
