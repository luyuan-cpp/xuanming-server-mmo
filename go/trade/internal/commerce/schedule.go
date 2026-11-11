package commerce

// schedule.go —— 合同推进的分工与下次到期(缺口清单第 16 条,docs/design/jubaozhai-market.md §15.8)。
//
// 分工:每个副本只推进自己负责的片(合同按 commerce_id、收据 ACK 按 op_id,各 64 片),归属由 Engine.Owns 判,
// 条件写进 Due / AckDue 的 SQL。分工不承担正确性:推进、推迟、隔离仍以合同行锁和版本条件为准。
//
// 下次到期:updated_ms 兼作调度键(CommerceRepo.Due)。以前 advance 不论有没有推进都 Save(updated_ms=现在),
// 在架寄售、在拍拍品、等确认的角色交易、挂着的市场单每轮被重存一次:版本每秒加一,玩家按旧版本提交的购买、取消
// 撞乐观冲突;它们每轮都排回队尾,活跃合同越多,新成交排得越后。现在一步推进没有任何变化时不重存,
// 只按 nextDueMs 排下次到期(CommerceRepo.RescheduleTx,不改版本)。

import (
	"time"

	tradepb "proto/trade"
	"shared/workshard"
)

// 按片分工的两类工作。归属键 = 类别 + ":" + 片号(workshard.ShardKey),两类各自独立分工。
const (
	workCommerce   = "trade-commerce"    // 合同推进,片号 = commerce_id % 64
	workReceiptAck = "trade-receipt-ack" // 收据 ACK,片号 = op_id % 64
)

// commerceShards 是本副本负责的合同片的 SQL 条件;Engine.Owns 为 nil 时不过滤。
func (e *Engine) commerceShards() workshard.Filter {
	return workshard.SQLFilter(e.Owns, workCommerce, "commerce_id")
}

// receiptShards 是本副本负责的收据片的 SQL 条件;Engine.Owns 为 nil 时不过滤。
func (e *Engine) receiptShards() workshard.Filter {
	return workshard.SQLFilter(e.Owns, workReceiptAck, "op_id")
}

// idleRecheck 是只等时间或玩家操作的合同最长多久复查一次,兜住没想到的依赖。与 bidHeldMaxWait 是同一个兜底。
const idleRecheck = time.Hour

// inFlightRecheck 是等别的行上的事件的合同多久兜底复查一次:有腿在途(等资产指令终结),或市场母单等成交子单收尾。
// 这两种事件发生时都会唤醒合同(资产指令终结后 data.AssetOpRepo.wakeContractOf,子单收尾后 Engine.advance 唤醒母单),
// 正常情况下合同立刻到期;唤醒丢了(进程在提交与唤醒之间退出、唤醒语句失败)才等到这个兜底。
// 离线玩家的腿可能几小时不终结,以前这些合同每秒空转一次,现在每 30 秒一次。
const inFlightRecheck = 30 * time.Second

// nextDueMs 返回推进一步之后合同没有任何变化时的下次到期(进程时钟毫秒,与 Due 的 nowMs、Save 写的 updated_ms 同一个时钟)。
//
//   - 人民币寄售(kind 6)走到 advance 的是资产段,还依赖角色目录与商城,照旧每轮复查(现在);
//   - 等别的行上的事件的(waitsForOtherRows):inFlightRecheck 之后兜底复查,事件发生时会被唤醒;
//   - 只等时间或玩家操作的(waitsForTimeOrPlayers):排到截止时刻,最远 idleRecheck。玩家操作、竞价刷新最高价、
//     撮合成交、人工恢复都会 Save 这一行、立即到期,所以排得晚挡不住它们;
//   - 其余合同:仍是现在,每轮复查,与改动前的节奏相同(只是不改版本、不重写 workflow)。
//
// 结果不早于 nowMs。进程时钟先于数据库时钟走到截止时刻时,这一行最多每轮空转一次,直到 plan 用数据库时刻判它到期。
func nextDueMs(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow, nowMs uint64) uint64 {
	if r.Kind == 6 {
		return nowMs
	}
	if waitsForOtherRows(r, w) {
		return nowMs + uint64(inFlightRecheck.Milliseconds())
	}
	if !waitsForTimeOrPlayers(r, w) {
		return nowMs
	}
	due := nowMs + uint64(idleRecheck.Milliseconds())
	if expiryAdvances(r, w) && r.ExpiresMs != 0 && r.ExpiresMs < due {
		due = r.ExpiresMs
	}
	return max(due, nowMs)
}

// waitsForOtherRows 报告合同是否在等别的行上的事件:有腿在途(结局写在资产待办行),或市场母单在等成交子单收尾
// (marketBarrier 读子单的状态:撤单后的 market_wait,或已全部成交的 market_open)。
func waitsForOtherRows(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) bool {
	if legsInFlight(w) {
		return true
	}
	if r.Kind != 4 || r.Side == 0 {
		return false
	}
	return w.Stage == "market_wait" || (w.Stage == "market_open" && r.FilledQuantity == r.Quantity)
}

// waitsForTimeOrPlayers 报告合同此刻是否只等时间或玩家操作:stage 只会因截止时刻或这一行自己的 Save 往前走。
// 调用方先排除了 waitsForOtherRows。撮合由新单那一方发起(step 在推进之后撮合,同一本盘口由 guard 行锁串行),
// 挂着的单不用每轮重撮。
func waitsForTimeOrPlayers(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) bool {
	switch w.Stage {
	case "open", "lot_open", "official_refund":
		return true
	case "confirm":
		return r.Kind == 2
	case "market_open":
		return r.Side != 0 && r.FilledQuantity < r.Quantity
	}
	return false
}

// filledParentToWake 判这一步推进是不是让一张市场成交子单收尾了:子单(kind 4、side 0、有母单)从推进中变成
// 已成交或已取消时,回它的母单号,由 advance 在提交之后唤醒母单(母单在 marketBarrier 里等子单收尾,
// 见 waitsForOtherRows)。其余情况回 0。子单转人工不唤醒:母单接着等它被人工处理完,那之后引擎把子单推进到
// 结案的那一步才唤醒(jubaozhai-market.md §15.7.1 第 4 类)。
func filledParentToWake(before, after *tradepb.TradeCommerceRecord) uint64 {
	if after.Kind != 4 || after.Side != 0 || after.ParentId == 0 {
		return 0
	}
	if fillSettledStatus(before.Status) || !fillSettledStatus(after.Status) {
		return 0
	}
	return after.ParentId
}

// fillSettledStatus 是 marketBarrier 认的子单收尾状态:已取消(7)与已成交(11)。转人工(10)不算收尾,
// 母单等它被人工处理完,不跟着转人工(2026-11-11 起,jubaozhai-market.md §15.7.1 第 4 类)。
func fillSettledStatus(status uint32) bool {
	return status == Cancelled || status == Settled
}

// expiryAdvances 报告截止时刻会不会推进这个 stage,对应 plan 里按 expires_ms 下架寄售、结拍、取消角色交易的三处。
func expiryAdvances(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) bool {
	switch w.Stage {
	case "open", "lot_open":
		return true
	case "confirm":
		return r.Kind == 2
	}
	return false
}

// legsInFlight 报告是否有已发出、还没记下结局的腿。结局由重投循环写进资产待办行,合同这一行看不到;
// 终结一侧提交后会唤醒合同(data.AssetOpRepo.wakeContractOf)。
func legsInFlight(w *tradepb.TradeCommerceWorkflow) bool {
	for _, l := range w.Legs {
		if l.OpId != 0 && l.Outcome == 0 {
			return true
		}
	}
	return false
}
