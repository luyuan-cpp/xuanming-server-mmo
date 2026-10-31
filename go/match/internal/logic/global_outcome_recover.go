package logic

import (
	"context"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
)

// globalOutcomeRecoverInterval 是到期终局恢复循环(match.global.outcome.recover)的节拍。
const globalOutcomeRecoverInterval = time.Second

// globalOutcomeRecoverBudget 是一页到期终局恢复(至多 8 局)的时间预算,与挪出撮合轮之前相同。
const globalOutcomeRecoverBudget = 3 * time.Second

// expiredOutcomeRecoverer 是到期终局恢复循环要的能力(生产实现 *authority.Store),收窄成接口便于替身测试。
type expiredOutcomeRecoverer interface {
	RecoverExpiredOutcomes(ctx context.Context, afterRetryMs, afterBattle uint64) (uint64, uint64, error)
}

// globalOutcomeRecovery 是到期终局恢复的独立循环(2026-10-30 从撮合轮 runGlobalRound 挪出):成功开房的 state4、
// 回包丢失的 state3 等不在 Pending 里的成局,过了权威截止与终局提交宽限之后在这里争唯一一次 SQL ABORT。
//
//   - 翻页游标是本结构的字段,只由 match.global.outcome.recover 这一个 goroutine 读写,所以不加锁,也不挂在
//     ServiceContext 上(挂在那里时撮合轮与本循环会同时读写,就是数据竞争);
//   - 不按池归属分工:所有副本都跑,同一成局的并发恢复靠原 P → formation 行锁与条件只写一份终局;
//   - 游标语义不变:每页按 RecoverExpiredOutcomes 的返回值推进,出错也推进(单条损坏不能卡住整页),扫到末尾回 0。
type globalOutcomeRecovery struct {
	afterRetryMs, afterBattle uint64
}

// round 跑一页到期终局恢复。未确认(含提交结果未知)时 HoldRelay 并打 ERROR,原件保留到下轮重放。
func (r *globalOutcomeRecovery) round(ctx context.Context, recoverer expiredOutcomeRecoverer, hold func(error)) {
	recoverCtx, cancel := context.WithTimeout(ctx, globalOutcomeRecoverBudget)
	defer cancel()
	var err error
	r.afterRetryMs, r.afterBattle, err = recoverer.RecoverExpiredOutcomes(recoverCtx, r.afterRetryMs, r.afterBattle)
	if err != nil {
		hold(err)
		logx.Error("全球匹配到期终局恢复未确认，保留原件等待下轮")
	}
}
