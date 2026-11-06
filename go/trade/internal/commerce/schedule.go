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

// nextDueMs 返回推进一步之后合同没有任何变化时的下次到期(进程时钟毫秒,与 Due 的 nowMs、Save 写的 updated_ms 同一个时钟)。
//
//   - 只等时间或玩家操作的合同(waitsForTimeOrPlayers):排到截止时刻,最远 idleRecheck。玩家操作、竞价刷新最高价、
//     撮合成交、人工恢复都会 Save 这一行、立即到期,所以排得晚挡不住它们;
//   - 其余合同:仍是现在,每轮复查,与改动前的节奏相同(只是不改版本、不重写 workflow)。
//
// 结果不早于 nowMs。进程时钟先于数据库时钟走到截止时刻时,这一行最多每轮空转一次,直到 plan 用数据库时刻判它到期。
func nextDueMs(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow, nowMs uint64) uint64 {
	if !waitsForTimeOrPlayers(r, w) {
		return nowMs
	}
	due := nowMs + uint64(idleRecheck.Milliseconds())
	if expiryAdvances(r, w) && r.ExpiresMs != 0 && r.ExpiresMs < due {
		due = r.ExpiresMs
	}
	return max(due, nowMs)
}

// waitsForTimeOrPlayers 报告合同此刻是否只等时间或玩家操作:没有腿在途,而且 stage 只会因截止时刻或这一行自己的 Save
// 往前走。撮合由新单那一方发起(step 在推进之后撮合,同一本盘口由 guard 行锁串行),挂着的单不用每轮重撮。
// 人民币寄售(kind 6)走到 advance 的是资产段,还依赖角色目录与商城,不在这里判,照旧每轮复查。
func waitsForTimeOrPlayers(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) bool {
	if r.Kind == 6 || legsInFlight(w) {
		return false
	}
	switch w.Stage {
	case "open", "lot_open", "official_refund":
		return true
	case "confirm":
		return r.Kind == 2
	case "market_open":
		// 已全部成交的母单在等成交子单收尾(marketBarrier 读子单的状态),子单在别的行上,只能每轮复查。
		return r.Side != 0 && r.FilledQuantity < r.Quantity
	}
	return false
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

// legsInFlight 报告是否有已发出、还没记下结局的腿。结局由重投循环写进资产待办行,合同这一行看不到,只能每轮复查。
func legsInFlight(w *tradepb.TradeCommerceWorkflow) bool {
	for _, l := range w.Legs {
		if l.OpId != 0 && l.Outcome == 0 {
			return true
		}
	}
	return false
}
