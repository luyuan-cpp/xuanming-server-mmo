package commerce

// tick_isolation.go —— Engine.Tick 的单行隔离(2026-10-20,第一轮审计 §2.5 go-asset-services#0 / #1)。
//
// 以前 Tick 只要一条合同出错就整轮返回;失败行的 updated_ms 不变,下一轮仍排在 Due 队头,全服交易被一行卡死
// (最容易触发的是离线买家累计 16 条未决 credit 后的 ErrTooManyPending)。持有中的竞价(bid_held)也不改 updated_ms,
// 在拍拍品数达到 Due 上限后同样霸占队头。本文件把一行的失败收口在这一行:
//   - 暂时失败:推迟这一行的下次到期(CommerceRepo.Defer,只在版本未变时生效,不旧盖新);
//   - 确定性坏数据:隔离为 Manual 并移出 Due(quarantine),等人工处理;
//   - 等待中的持有竞价:推迟到拍品截止(settleHeldBid)。
//
// 2026-10-22 按复核意见修正:单行子预算改为从父 ctx 截止时刻推出(rowBudgetFor),卡住的行先撞上自己的子预算、
// 父 ctx 还来得及推迟它,并给收据 ACK 留出预算;乐观冲突(conflict)不推迟、角色冲突(role_conflict)分钟级复查;
// 隔离时截止合同名下仍在途的托管扣款;撮合失败不再走 deferCommerce。
// 设计说明见 docs/design/jubaozhai-market.md §15.3,人工处理见 docs/ops/trade-commerce-quarantine-runbook.md。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	tradepb "proto/trade"
	"shared/assetop"
	"trade/internal/data"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/zeromicro/go-zero/core/logx"
)

const (
	// tickDueLimit / tickAckLimit:每轮最多推进的合同数与最多尝试的收据 ACK 数(沿用改动前的 100)。
	tickDueLimit = 100
	tickAckLimit = 100
	// 单行子预算从 Tick 父 ctx 的截止时刻推出来(rowBudgetFor;svc 每轮只给合同推进 3s,见 svc.AssetChannel.Start)。
	// 以前固定 5s,比整轮预算还长:两者同时到期,卡住的那一行连推迟都来不及做,下一秒又排在 Due 队头吃满整轮(2026-10-22)。
	//   - tickAckReserve:Due 循环给本轮收据 ACK 留的时间。活跃合同每轮都会被 advance 重新 Save,Due 几乎总能取满,
	//     不留的话预算全被合同推进吃掉,scene 端托管原件的 ACK 一直轮不到;
	//   - tickDeferBudget:一行失败后推迟 / 隔离它的预算(deferCommerce 自带这段超时)。子预算先扣掉它,
	//     所以卡住的行一定先撞上自己的子预算,父 ctx 还剩这一段把它推迟;
	//   - tickRowBudget:单行子预算的绝对上限(父预算很长时才起作用);
	//   - tickRowMinBudget:剩余不够它就不再开新行。没轮到的行仍到期,下一轮或别的副本接着推进。
	// 正常的一行(EnsureSeqRows、8 次 op_id 取号、一次合同事务)只用几十毫秒,这几段预算只约束卡住的行。
	tickAckReserve   = 500 * time.Millisecond
	tickDeferBudget  = 300 * time.Millisecond
	tickRowBudget    = 5 * time.Second
	tickRowMinBudget = 200 * time.Millisecond
	// tickRetryBackoff:暂时失败(存储超时、锁等待 / 死锁重试用尽、号段暂不可用、依赖 RPC 超时等)的推迟中值,±20% 抖动。
	tickRetryBackoff = 10 * time.Second
	// tickPendingBackoff:ErrTooManyPending 的推迟中值,±20% 抖动。它要等那位玩家的未决资产指令终结
	// (多半要他重新上线),比一般的暂时失败慢得多,没必要每 10 秒白跑一遍。
	tickPendingBackoff = 60 * time.Second
	// tickRoleConflictBackoff:role_conflict(参与角色已换主、被人民币寄售冻结或目录行不合格)的推迟中值,±20% 抖动。
	// 这是长期状态:换主不可逆(owner_revision 只增不减),冻结按公示 / 寄售期以天计。每 10 秒重试只会刷 ERROR,
	// 分钟级复查足够;冻结解除后合同最多晚这么久恢复推进。
	tickRoleConflictBackoff = 5 * time.Minute
	// bidHeldRecheck / bidHeldMaxWait:持有竞价等拍品结算时的推迟下限与上限。拍品截止前它不可能结算,
	// 所以推迟到拍品截止,但至少 bidHeldRecheck、至多 bidHeldMaxWait(拍品卡在 Manual 时仍有界地复查)。
	bidHeldRecheck = 30 * time.Second
	bidHeldMaxWait = time.Hour
)

// errCorruptContract 标记"合同自身的持久数据坏了"的确定性失败:重试只会每轮撞同一个错。
var errCorruptContract = errors.New("交易合同数据损坏")

// corrupt 把 err 标成确定性坏数据。errors.Is(…, errCorruptContract) 与 errors.Is(…, 原错误) 同时成立。
func corrupt(err error) error { return fmt.Errorf("%w: %w", errCorruptContract, err) }

// errRoleConflict 标记"交易参与角色已换主、被人民币寄售冻结或目录行不合格":长期状态,不是存储抖动,
// 按 role_conflict 分钟级复查(2026-10-22)。包装里同时带着 ErrConflict,客户端拿到的 tip 码不变(commerceCode 用 errors.As)。
var errRoleConflict = errors.New("交易参与角色已换主或被冻结")

// errStaleObservation 标记乐观并发冲突:锁内复核发现 Due / 预读之后,合同或对手方已被别的写者推进
// (多副本同时推进同一批合同时的正常现象)。重新读一次就能消解,所以不推迟,下一轮立即重试(2026-10-22)。
// 同样带着 ErrConflict。
var errStaleObservation = errors.New("交易合同观察已过期")

func roleConflict() error     { return fmt.Errorf("%w: %w", errRoleConflict, ErrConflict) }
func staleObservation() error { return fmt.Errorf("%w: %w", errStaleObservation, ErrConflict) }

// asRoleConflict 把角色目录给出的 ErrConflict(角色被冻结、目录行不合格)标成 role_conflict;存储 / RPC 错误原样返回。
func asRoleConflict(err error) error {
	if errors.Is(err, ErrConflict) && !errors.Is(err, errRoleConflict) {
		return fmt.Errorf("%w: %w", errRoleConflict, err)
	}
	return err
}

// 失败分类,即 tickRowFailuresTotal / marketMatchFailuresTotal 的 class label(低基数常量,不带任何玩家或合同 id)。
const (
	failureRetry          = "retry"
	failureTooManyPending = "too_many_pending"
	failureCorrupt        = "corrupt"
	failureRoleConflict   = "role_conflict"
	failureConflict       = "conflict"
)

var failureClasses = []string{failureRetry, failureTooManyPending, failureCorrupt, failureRoleConflict, failureConflict}

// failureClass 按优先级分类:坏数据优先(否则会按暂时失败无限重试),其次积压背压、角色冲突、乐观冲突,其余一律算暂时失败。
func failureClass(err error) string {
	switch {
	case errors.Is(err, errCorruptContract):
		return failureCorrupt
	case errors.Is(err, assetop.ErrTooManyPending):
		return failureTooManyPending
	case errors.Is(err, errRoleConflict):
		return failureRoleConflict
	case errors.Is(err, errStaleObservation):
		return failureConflict
	default:
		return failureRetry
	}
}

// rowBudgetFor 算 Due 里下一行能用的子预算,deadline 取自 Tick 的父 ctx,now 由调用方传入(单测要确定性)。
//
// 子预算必须严格早于父截止时刻结束,并给后面的事留出余量(2026-10-22):
//   - 先扣掉本轮收据 ACK 的 tickAckReserve 与这一行失败后推迟 / 隔离的 tickDeferBudget;
//   - 剩下的时间一行最多用一半:一行卡住时,同一轮后面的行至少还有另一半;正常的行只用几十毫秒,不受影响;
//   - 再不超过 tickRowBudget。不足 tickRowMinBudget 时回 false:本轮不再开新行,剩下的时间留给 ACK。
//
// 父 ctx 没有截止时刻(单测或直接调用)时用 tickRowBudget。按生产的 3s 计:第一行至多 1.1s,
// 卡住并推迟之后父 ctx 还剩至少 1.6s,下一行至多 0.4s,收据 ACK 始终留着 0.5s。
func rowBudgetFor(ctx context.Context, now time.Time) (time.Duration, bool) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return tickRowBudget, true
	}
	budget := min((deadline.Sub(now)-tickAckReserve-tickDeferBudget)/2, tickRowBudget)
	if budget < tickRowMinBudget {
		return 0, false
	}
	return budget, true
}

// 指标注册在 prometheus.DefaultRegisterer 上,与 data 包的 trade_assetop_* 计数器同一个 registry,
// 由 svc.StartMetrics 的 promhttp.Handler() 一起暴露。包级变量只初始化一次,单测里重复构造 Engine 也不会重复注册。
var (
	tickRowFailuresTotal = promauto.With(prometheus.DefaultRegisterer).NewCounterVec(prometheus.CounterOpts{
		Subsystem: "trade",
		Name:      "commerce_tick_row_failures_total",
		Help:      "交易合同推进单行失败次数(按 class:retry / too_many_pending / corrupt / role_conflict / conflict);失败行被推迟、隔离或下一轮立即重试,不再挡住同轮其余合同。",
	}, []string{"class"})
	quarantinedTotal = promauto.With(prometheus.DefaultRegisterer).NewCounter(prometheus.CounterOpts{
		Subsystem: "trade",
		Name:      "commerce_quarantined_total",
		Help:      "因确定性坏数据被隔离为 Manual 的交易合同数(含被移出盘口的坏对手单);>0 需要人工处理(托管资产仍在合同里)。",
	})
	marketMatchFailuresTotal = promauto.With(prometheus.DefaultRegisterer).NewCounterVec(prometheus.CounterOpts{
		Subsystem: "trade",
		Name:      "commerce_market_match_failures_total",
		Help:      "市场订单推进成功后撮合失败的次数(按 class);taker 已排到队尾,下一轮照常重撮,不推迟也不隔离。",
	}, []string{"class"})
	tickTruncatedTotal = promauto.With(prometheus.DefaultRegisterer).NewCounter(prometheus.CounterOpts{
		Subsystem: "trade",
		Name:      "commerce_tick_truncated_total",
		Help:      "本轮预算用完时 Due 里还有合同没开始推进的轮数;持续增长说明合同推进吞吐跟不上,或有行反复卡满子预算。",
	})
)

func init() {
	// 先把所有 class 都建出来:从未发生与指标不存在在告警规则里不该长得一样。
	for _, class := range failureClasses {
		tickRowFailuresTotal.WithLabelValues(class)
		marketMatchFailuresTotal.WithLabelValues(class)
	}
}

// step 推进 Due 里的一行。返回的错误由 Tick 交给 deferCommerce 分流;合同自身的持久数据坏了用 corrupt 包装。
func (e *Engine) step(ctx context.Context, r *tradepb.TradeCommerceRecord) error {
	if r.Kind == 6 {
		if e.Rmb == nil {
			return errors.New("人民币寄售恢复器未装配")
		}
		return e.Rmb.step(ctx, r)
	}
	w, err := data.Workflow(r)
	if err != nil {
		return corrupt(err)
	}
	switch w.Stage {
	case "bid_ready":
		return e.acceptBid(ctx, r.CommerceId)
	case "bid_held":
		return e.settleHeldBid(ctx, r)
	}
	if err := e.advance(ctx, r); err != nil {
		return fmt.Errorf("推进交易合同: %w", err)
	}
	if r.Kind == 4 && r.Side != 0 {
		// advance 已提交:这一行的版本已变、updated_ms 已写成现在(排到了队尾)。拿 Due 读到的旧版本去推迟或隔离
		// 必然落空,所以撮合失败不交给 deferCommerce,只单独计数、记日志,下一轮照常重撮(2026-10-22)。
		// 对手单自己坏了的情况 matchMarket 会把对手单移出盘口,不算这里的失败。
		if err := e.matchMarket(ctx, r.CommerceId); err != nil {
			recordMatchFailure(r.CommerceId, err)
		}
	}
	return nil
}

// recordMatchFailure 记下一次 taker 撮合失败。乐观冲突(对手方在预读之后变了)是多副本的正常现象,只记 INFO。
func recordMatchFailure(takerID uint64, err error) {
	class := failureClass(err)
	marketMatchFailuresTotal.WithLabelValues(class).Inc()
	if class == failureConflict {
		logx.Infof("[Commerce] Market match raced with another writer, rematching next round taker_id=%d: %v", takerID, err)
		return
	}
	logx.Errorf("[Commerce] Market match failed, rematching next round taker_id=%d class=%s: %v", takerID, class, err)
}

// settleHeldBid:拍品结算后把持有中的竞价结案;拍品未结算时把这一行推迟到拍品截止。
//
// 以前未结算时原样跳过、updated_ms 不变,每个有出价的在拍拍品都有一行 bid_held 永远排在 Due 队头;
// 在拍拍品数达到 tickDueLimit 后 Due 只回这些行,拍品自己也推进不了,整个交易停摆。
func (e *Engine) settleHeldBid(ctx context.Context, r *tradepb.TradeCommerceRecord) error {
	parent, err := e.Repo.Get(ctx, r.ParentId)
	if errors.Is(err, sql.ErrNoRows) {
		return corrupt(fmt.Errorf("持有竞价的拍品 %d 不存在", r.ParentId))
	}
	if err != nil {
		return err
	}
	if parent.Status != Settled {
		now := e.now()
		until := min(max(parent.ExpiresMs, now+uint64(bidHeldRecheck.Milliseconds())), now+uint64(bidHeldMaxWait.Milliseconds()))
		// 落空(版本已变)说明这一行刚被出价改成 bid_refund 之类,新状态立即到期,不需要推迟。
		_, err := e.Repo.Defer(ctx, r.CommerceId, r.Version, until)
		return err
	}
	return e.Repo.Tx(ctx, func(tx *sql.Tx) error {
		current, err := e.Repo.Lock(ctx, tx, r.CommerceId)
		if err != nil {
			return err
		}
		cw, err := data.Workflow(current)
		if err != nil {
			return corrupt(err)
		}
		if cw.Stage != "bid_held" {
			return nil
		}
		current.Status = Settled
		cw.Stage = "done"
		if err := e.Repo.Ops.MarkCommerceOpsConsumedTx(ctx, tx, current.CommerceId); err != nil {
			return err
		}
		return e.Repo.Save(ctx, tx, current, cw, e.now())
	})
}

// deferCommerce 处理一行推进失败,按 failureClass 分流:
//   - conflict(乐观冲突):不推迟,下一轮按新状态立即重试 —— 推迟 10 秒会把临近截止的有效出价拖过截止;
//   - corrupt:先尝试隔离;隔离落空(版本已变)或失败时退回普通推迟;
//   - 其余:按 tickFailureBackoff 推迟下次到期(role_conflict 分钟级)。
//
// 推迟 / 隔离用父 ctx 派生的 tickDeferBudget(这一行的子预算已经用完;rowBudgetFor 保证父 ctx 还留着这一段)。
// 推迟本身失败只记日志:这一行下一轮还会被取到,不丢任何东西。典型是别的事务正持有这一行的合同行锁 ——
// 那时任何写都拿不到锁,这一行每轮只花它的子预算加这一段,同一轮其余合同照常推进。
//
// 推迟与隔离都以 Due 读到的 r.Version 为条件。这一行若在 Due 之后被别的副本或玩家操作推进过,两者都会落空,
// 由新状态决定它何时到期 —— 绝不拿旧观察覆盖新行。
func (e *Engine) deferCommerce(parent context.Context, r *tradepb.TradeCommerceRecord, cause error) {
	ctx, cancel := context.WithTimeout(parent, tickDeferBudget)
	defer cancel()
	class := failureClass(cause)
	tickRowFailuresTotal.WithLabelValues(class).Inc()
	if class == failureConflict {
		logx.Infof("[Commerce] Contract step raced with another writer, retrying next round commerce_id=%d kind=%d version=%d: %v",
			r.CommerceId, r.Kind, r.Version, cause)
		return
	}
	if class == failureCorrupt {
		stage, quarantined, err := e.quarantine(ctx, r)
		if err == nil && quarantined {
			quarantinedTotal.Inc()
			logx.Errorf("[Commerce] Contract quarantined as manual (deterministic data error) commerce_id=%d kind=%d status=%d version=%d stage=%q: %v",
				r.CommerceId, r.Kind, r.Status, r.Version, stage, cause)
			return
		}
		if err != nil {
			logx.Errorf("[Commerce] Failed to quarantine contract commerce_id=%d version=%d: %v (step error: %v)",
				r.CommerceId, r.Version, err, cause)
		}
		// 隔离落空(版本已变)或失败:退回普通推迟,下一轮按新版本重判。
	}
	untilMs := tickFailureBackoff(e.now(), cause, rand.Float64)
	deferred, err := e.Repo.Defer(ctx, r.CommerceId, r.Version, untilMs)
	if err != nil {
		logx.Errorf("[Commerce] Failed to defer contract commerce_id=%d version=%d after step error (%v): %v",
			r.CommerceId, r.Version, cause, err)
		return
	}
	switch class {
	case failureTooManyPending:
		// 积压背压是预期内的状态(离线玩家的未决资产指令),指标已计数,日志不升到 ERROR。
		logx.Infof("[Commerce] Contract step deferred by pending-op backpressure commerce_id=%d kind=%d deferred=%t until_ms=%d: %v",
			r.CommerceId, r.Kind, deferred, untilMs, cause)
	case failureRoleConflict:
		// 长期状态:合同与对方的托管会一直扣着,需要人工判断(docs/ops/trade-commerce-quarantine-runbook.md)。
		logx.Errorf("[Commerce] Contract blocked by a party role (owner changed, frozen by a listing, or directory row invalid) commerce_id=%d kind=%d status=%d deferred=%t until_ms=%d: %v",
			r.CommerceId, r.Kind, r.Status, deferred, untilMs, cause)
	default:
		logx.Errorf("[Commerce] Contract step failed commerce_id=%d kind=%d status=%d class=%s deferred=%t until_ms=%d: %v",
			r.CommerceId, r.Kind, r.Status, class, deferred, untilMs, cause)
	}
}

// tickFailureBackoff 算失败行的下次到期时刻:nowMs + 中值 ×(0.8 + 0.4×rnd()),复用 assetop.NextAttemptMs 的抖动口径
// (attempts=0、max=base:不做指数增长 —— 合同表没有失败次数列,推迟有界即可)。rnd 由调用方注入,单测要确定性。
// conflict 类不推迟(deferCommerce 不会为它调用这里);隔离落空的 corrupt 按一般暂时失败推迟。
func tickFailureBackoff(nowMs uint64, cause error, rnd func() float64) uint64 {
	base := tickRetryBackoff
	switch failureClass(cause) {
	case failureTooManyPending:
		base = tickPendingBackoff
	case failureRoleConflict:
		base = tickRoleConflictBackoff
	}
	return assetop.NextAttemptMs(nowMs, 0, base, base, rnd)
}

// quarantine 把确定性坏数据的合同隔离为 Manual(移出 Due),返回原 stage 供日志取证。只在行仍是 r.Version 时生效。
func (e *Engine) quarantine(ctx context.Context, r *tradepb.TradeCommerceRecord) (string, bool, error) {
	stage, quarantined := "", false
	err := e.Repo.Tx(ctx, func(tx *sql.Tx) error {
		stage, quarantined = "", false
		cur, err := e.Repo.Lock(ctx, tx, r.CommerceId)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if cur.Version != r.Version || cur.Status == Manual {
			return nil
		}
		if stage, err = e.quarantineLockedTx(ctx, tx, cur); err != nil {
			return err
		}
		quarantined = true
		return nil
	})
	return stage, quarantined, err
}

// quarantineLockedTx 在调用方已锁住合同行 cur 的事务里把它隔离为 Manual,返回原 stage 供日志取证。
// 调用方:quarantine(合同自己那一行)与 matchMarket(移出盘口的坏对手单)。
//
//   - 先把合同名下仍待办的托管扣款截止提前到现在(ExpireCommerceEscrowDebitsTx,与玩家取消同一写法、同一锁序:
//     合同行 → 按 op_id 升序点锁 op 行)。截止之后 assetop.Loop 用同一个 seq 改发 Abort 定下结局:scene 已经扣到的
//     照常回 APPLIED 留下证据,没扣到的不会再扣 —— 隔离之后不再把任何人的资产扣进人工单(2026-10-22)。
//     隔离之前已经排定的发放腿(stream 4)照常投递:它们来自隔离之前合法的状态,截住反而会让资产悬空。
//   - workflow 解得开:stage 改成 "manual"(与 APPLIED_PARTIAL / 撮合子单坏账的既有隔离同形),
//     这样 Mutate / PlaceBid / 撮合的 stage 判断都不会再把它拉回推进;
//   - 解不开:只改状态列(CommerceRepo.SaveStatus),坏字节原样保留作人工取证。
//
// 已托管的资产仍记在合同里,隔离本身不发出、不退回任何资产;人工按合同与 op 行证据处理
// (docs/ops/trade-commerce-quarantine-runbook.md)。
func (e *Engine) quarantineLockedTx(ctx context.Context, tx *sql.Tx, cur *tradepb.TradeCommerceRecord) (string, error) {
	if err := e.Repo.Ops.ExpireCommerceEscrowDebitsTx(ctx, tx, cur.CommerceId, e.now()); err != nil {
		return "", err
	}
	w, decodeErr := data.Workflow(cur)
	if decodeErr != nil {
		return "<undecodable>", e.Repo.SaveStatus(ctx, tx, cur, Manual, e.now())
	}
	stage := w.Stage
	cur.Status = Manual
	w.Stage = "manual"
	return stage, e.Repo.Save(ctx, tx, cur, w, e.now())
}

// ackReceipts 轮转发一页收据 ACK。按 op_id 翻页(CommerceRepo.AckDue 的 afterOpID):离线卖家的 ACK 一直 done=false,
// 只从表头取会让它们永远占住名额,后面的 ACK 永远轮不到。每尝试一条就把游标推到它;本页不满说明到了表尾,下一轮从头。
// RPC / SQL 依赖错误保留待办、推进游标并继续本页,页末汇总给 Tick 调用者统一记录;
// 正常离线(done=false、err=nil)不算故障。预算用完时游标停在最后一条已尝试的 op 上。
// payload 解不开的行(AckDue 回 Bundle=nil)跳过并告警、游标照常越过它:托管原件仍在 escrow_snapshot 里,
// 只是这条 ACK 发不出去,要人工按 op_id 查;不能让它挡住同页其余卖家的 ACK。
func (e *Engine) ackReceipts(ctx context.Context) error {
	if e.Applier == nil || e.Ledger == nil {
		return nil
	}
	ops, err := e.Repo.AckDue(ctx, e.ackAfterOpID, tickAckLimit)
	if err != nil {
		return err
	}
	var failures error
	for _, op := range ops {
		if err := ctx.Err(); err != nil {
			return errors.Join(failures, err)
		}
		if op.Bundle == nil {
			logx.Errorf("[Commerce] Receipt ack skipped: asset op payload undecodable op_id=%d stream=%d", op.OpID, int32(op.Stream))
			e.ackAfterOpID = op.OpID
			continue
		}
		child, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
		ok, err := assetop.AcknowledgeReceipt(child, e.Applier, e.Ledger, op.Request())
		cancel()
		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("交易收据确认依赖失败 op_id=%d: %w", op.OpID, err))
		} else if ok {
			if err := e.Repo.Acked(ctx, op.OpID); err != nil {
				failures = errors.Join(failures, fmt.Errorf("交易收据待办清理失败 op_id=%d: %w", op.OpID, err))
			}
		}
		e.ackAfterOpID = op.OpID
	}
	if len(ops) < tickAckLimit {
		e.ackAfterOpID = 0
	}
	return failures
}

// logOffbandLegOutcome 记下"合同腿的终局不是 scene 落盘给的"(人工终结或自动放弃)。只是观察日志:
// 所在事务可能随后回滚重试,所以不计指标(放弃与人工终结各自已在 assetop 计数:assetop_abandoned_total、
// assetop_manual_resolve_total)。
func logOffbandLegOutcome(commerceID uint64, l *tradepb.TradeCommerceLeg, out data.LegOutcome) {
	source := out.Source()
	if source == "scene" {
		return
	}
	logx.Infof("[Commerce] Leg outcome observed from %s resolution commerce_id=%d role=%s op_id=%d status=%d",
		source, commerceID, l.Role, l.OpId, int32(out.Status))
}
