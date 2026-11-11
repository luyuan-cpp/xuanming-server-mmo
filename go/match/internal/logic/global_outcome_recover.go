package logic

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"match/internal/authority"

	"github.com/zeromicro/go-zero/core/logx"
)

// globalOutcomeRecoverInterval 是到期终局恢复循环(match.global.outcome.recover)的节拍。
const globalOutcomeRecoverInterval = time.Second

// globalOutcomeRecoverBudget 是一拍到期终局恢复(车道页,每圈第一拍再加一页全量页,各至多 8 局)的时间预算。
// 2026-11-06 起每页先在锁外并行向各局原 battle 实例取证(authority.DeadlineProbeTimeout,原实例连不上时要等满),
// 两页各多一个取证期限;循环是串行的(ticker 丢掉跑不完的拍),预算变长只让下一拍晚到,不会叠出并发轮次。
const globalOutcomeRecoverBudget = 3*time.Second + 2*authority.DeadlineProbeTimeout

// expiredOutcomeRecoverer 是到期终局恢复循环要的能力(生产实现 *authority.Store),收窄成接口便于替身测试。
type expiredOutcomeRecoverer interface {
	RecoverExpiredOutcomeLane(ctx context.Context, lanes, lane uint64, cursor authority.ExpiredOutcomeCursor) (authority.ExpiredOutcomeCursor, error)
	RecoverExpiredOutcomes(ctx context.Context, afterRetryMs, afterBattle uint64) (uint64, uint64, error)
}

// globalOutcomeRecovery 是到期终局恢复的独立循环(2026-10-30 从撮合轮 runGlobalRound 挪出):成功开房的 state4、
// 回包丢失的 state3 等不在 Pending 里的成局,过了权威截止与终局提交宽限之后在这里争唯一一次 SQL ABORT。
//
//   - 状态都是本结构的字段,只由 match.global.outcome.recover 这一个 goroutine 读写,所以不加锁,也不挂在
//     ServiceContext 上(挂在那里时撮合轮与本循环会同时读写,就是数据竞争);
//   - 不按池归属分工:所有副本都跑,同一成局的并发恢复靠原 P → formation 行锁与条件只写一份终局;
//   - 分车道(2026-11-05):候选按 battle_id 分成 authority.ExpiredOutcomeLanes 条车道,每拍扫一条车道的一页,
//     每圈(车道数那么多拍)按随机排列走完全部车道、下一圈重新洗牌。多个副本的排列互相独立,同一拍大多落在不同车道上,
//     吞吐随副本数增长;每个副本每圈都把每条车道访问一遍,单条车道的访问间隔不超过两圈。
//   - 全量页:每圈第一拍再调一次 RecoverExpiredOutcomes(退休旧调度字段 + 不分车道扫一页)。这两条 SQL 都要走完在途的
//     state 4 行,成本与在途局数成正比、与积压无关,所以每圈只跑一次(2026-11-05 评审修正:原先每拍都退休、车道页不满
//     还补一页,积压为空时每副本每拍全量扫四遍,比分车道之前还贵一倍)。它与车道页互不依赖,车道页出错也照跑;
//   - 游标语义不变:每条车道与全量页各有一个游标,按返回值推进,出错也推进(单条损坏不能卡住整页),扫到末尾回零值。
type globalOutcomeRecovery struct {
	lanes   [authority.ExpiredOutcomeLanes]authority.ExpiredOutcomeCursor
	overall authority.ExpiredOutcomeCursor
	pending []int             // 本圈还没走的车道,按随机排列
	shuffle func(n int) []int // 生成一圈的车道顺序;nil 用 rand.Perm,测试注入固定顺序
}

// round 跑一拍到期终局恢复。未确认(含提交结果未知)时 HoldRelay 并打 ERROR,原件保留到下轮重放。
func (r *globalOutcomeRecovery) round(ctx context.Context, recoverer expiredOutcomeRecoverer, hold func(error)) {
	recoverCtx, cancel := context.WithTimeout(ctx, globalOutcomeRecoverBudget)
	defer cancel()
	lane, cycleStart := r.nextLane()
	next, err := recoverer.RecoverExpiredOutcomeLane(recoverCtx, authority.ExpiredOutcomeLanes, lane, r.lanes[lane])
	r.lanes[lane] = next
	if cycleStart {
		var fullErr error
		r.overall.AfterRetryMs, r.overall.AfterBattle, fullErr = recoverer.RecoverExpiredOutcomes(recoverCtx, r.overall.AfterRetryMs, r.overall.AfterBattle)
		err = errors.Join(err, fullErr)
	}
	if err != nil {
		hold(err)
		logx.Error("全球匹配到期终局恢复未确认，保留原件等待下轮")
	}
}

// nextLane 取本圈的下一条车道;一圈走完就重新洗牌,这一拍 cycleStart 为 true。
func (r *globalOutcomeRecovery) nextLane() (lane uint64, cycleStart bool) {
	if len(r.pending) == 0 {
		shuffle := r.shuffle
		if shuffle == nil {
			shuffle = rand.Perm
		}
		r.pending = shuffle(int(authority.ExpiredOutcomeLanes))
		cycleStart = true
	}
	lane = uint64(r.pending[0])
	r.pending = r.pending[1:]
	return lane, cycleStart
}
