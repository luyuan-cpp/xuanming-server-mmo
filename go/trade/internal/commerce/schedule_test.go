package commerce

import (
	"slices"
	"testing"

	tradepb "proto/trade"
)

// 缺口清单第 16 条:推进一步没有变化时的下次到期(docs/design/jubaozhai-market.md §15.8)。
func TestNextDueMsByStage(t *testing.T) {
	const now = uint64(1_000_000)
	hour := uint64(idleRecheck.Milliseconds())
	fallback := uint64(inFlightRecheck.Milliseconds())
	settled := &tradepb.TradeCommerceLeg{Role: "seller_debit", OpId: 7, Outcome: 2}
	inFlight := &tradepb.TradeCommerceLeg{Role: "buyer_debit_1", OpId: 8}
	cases := []struct {
		name string
		r    *tradepb.TradeCommerceRecord
		w    *tradepb.TradeCommerceWorkflow
		want uint64
	}{
		{"在架寄售排到截止", &tradepb.TradeCommerceRecord{Kind: 3, ExpiresMs: now + 600_000},
			&tradepb.TradeCommerceWorkflow{Stage: "open", Legs: []*tradepb.TradeCommerceLeg{settled}}, now + 600_000},
		{"截止很远时一小时复查", &tradepb.TradeCommerceRecord{Kind: 3, ExpiresMs: now + 7*hour},
			&tradepb.TradeCommerceWorkflow{Stage: "open"}, now + hour},
		{"在拍拍品排到结拍", &tradepb.TradeCommerceRecord{Kind: 5, ExpiresMs: now + 90_000},
			&tradepb.TradeCommerceWorkflow{Stage: "lot_open", HighestBidder: 21}, now + 90_000},
		{"等确认的角色交易排到截止", &tradepb.TradeCommerceRecord{Kind: 2, ExpiresMs: now + 30_000},
			&tradepb.TradeCommerceWorkflow{Stage: "confirm"}, now + 30_000},
		{"别的种类的 confirm 照旧每轮", &tradepb.TradeCommerceRecord{Kind: 3, ExpiresMs: now + 30_000},
			&tradepb.TradeCommerceWorkflow{Stage: "confirm"}, now},
		{"挂着的市场单一小时复查,不看截止", &tradepb.TradeCommerceRecord{Kind: 4, Side: 1, Quantity: 5, FilledQuantity: 2, ExpiresMs: now + 1},
			&tradepb.TradeCommerceWorkflow{Stage: "market_open"}, now + hour},
		{"全部成交的母单等子单收尾,子单收尾会唤醒,30 秒兜底", &tradepb.TradeCommerceRecord{Kind: 4, Side: 1, Quantity: 5, FilledQuantity: 5},
			&tradepb.TradeCommerceWorkflow{Stage: "market_open"}, now + fallback},
		{"撤单后等子单的母单 30 秒兜底", &tradepb.TradeCommerceRecord{Kind: 4, Side: 1, Quantity: 5, FilledQuantity: 5},
			&tradepb.TradeCommerceWorkflow{Stage: "market_wait"}, now + fallback},
		{"官方退款一小时复查", &tradepb.TradeCommerceRecord{Kind: 1, Status: 8},
			&tradepb.TradeCommerceWorkflow{Stage: "official_refund"}, now + hour},
		{"有腿在途:终结会唤醒,30 秒兜底,不看截止", &tradepb.TradeCommerceRecord{Kind: 3, ExpiresMs: now + 600_000},
			&tradepb.TradeCommerceWorkflow{Stage: "open", Legs: []*tradepb.TradeCommerceLeg{settled, inFlight}}, now + fallback},
		{"人民币寄售不改节奏", &tradepb.TradeCommerceRecord{Kind: 6, ExpiresMs: now + 600_000},
			&tradepb.TradeCommerceWorkflow{Stage: "open"}, now},
		{"人民币寄售有腿在途也不改节奏", &tradepb.TradeCommerceRecord{Kind: 6},
			&tradepb.TradeCommerceWorkflow{Stage: "rmb_escrow", Legs: []*tradepb.TradeCommerceLeg{inFlight}}, now},
		{"没列出的 stage 每轮复查", &tradepb.TradeCommerceRecord{Kind: 3},
			&tradepb.TradeCommerceWorkflow{Stage: "deliver"}, now},
		{"进程时钟已过截止也不早于现在", &tradepb.TradeCommerceRecord{Kind: 3, ExpiresMs: now - 5},
			&tradepb.TradeCommerceWorkflow{Stage: "open"}, now},
		{"没有截止时刻按一小时复查", &tradepb.TradeCommerceRecord{Kind: 5},
			&tradepb.TradeCommerceWorkflow{Stage: "lot_open"}, now + hour},
	}
	for _, c := range cases {
		if got := nextDueMs(c.r, c.w, now); got != c.want {
			t.Errorf("%s: 下次到期 = now%+d, 应为 now%+d", c.name, int64(got)-int64(now), int64(c.want)-int64(now))
		}
	}
}

// 缺口清单第 16 条:成交子单从推进中变成已成交、已取消时唤醒两张母单(买单与卖单);别的变化、别的种类都不唤醒。
// 2026-11-11 起卖单也唤醒(以前只唤醒买单,卖单要等兜底复查);子单转人工不再连带母单,也不唤醒它
// (jubaozhai-market.md §15.7.1 第 4 类)。
func TestFilledParentsToWake(t *testing.T) {
	fill := func(status uint32) *tradepb.TradeCommerceRecord {
		return &tradepb.TradeCommerceRecord{Kind: 4, Side: 0, ParentId: 900, CounterpartyId: 901, Status: status}
	}
	for _, settled := range []uint32{Settled, Cancelled} {
		if got := filledParentsToWake(fill(Delivering), fill(settled)); !slices.Equal(got, []uint64{900, 901}) {
			t.Errorf("子单收尾为 %d 时应唤醒买单与卖单,得到 %v", settled, got)
		}
	}
	if got := filledParentsToWake(fill(Delivering), fill(Manual)); got != nil {
		t.Errorf("子单转人工不算收尾,不唤醒母单: %v", got)
	}
	if got := filledParentsToWake(fill(Escrowing), fill(Delivering)); got != nil {
		t.Errorf("子单还在推进,不应唤醒母单: %v", got)
	}
	if got := filledParentsToWake(fill(Settled), fill(Settled)); got != nil {
		t.Errorf("已收尾的子单再推进一次不重复唤醒: %v", got)
	}
	parent := &tradepb.TradeCommerceRecord{Kind: 4, Side: 1, Status: Escrowing}
	done := &tradepb.TradeCommerceRecord{Kind: 4, Side: 1, Status: Settled}
	if got := filledParentsToWake(parent, done); got != nil {
		t.Errorf("母单自己结案不唤醒谁: %v", got)
	}
	consignment := &tradepb.TradeCommerceRecord{Kind: 3, ParentId: 900, Status: Settled}
	if got := filledParentsToWake(&tradepb.TradeCommerceRecord{Kind: 3, ParentId: 900, Status: Delivering}, consignment); got != nil {
		t.Errorf("不是市场子单不唤醒: %v", got)
	}
}

func TestShardFiltersFollowOwns(t *testing.T) {
	e := &Engine{}
	if e.commerceShards().SQL() != "1=1" || e.receiptShards().SQL() != "1=1" {
		t.Fatal("没装配分工时不过滤")
	}
	e.Owns = func(key string) bool { return key == workCommerce+":3" || key == workReceiptAck+":9" }
	if got := e.commerceShards().SQL(); got != "MOD(commerce_id,64) IN(?)" {
		t.Fatalf("合同片条件不对: %q", got)
	}
	if got := e.receiptShards().SQL(); got != "MOD(op_id,64) IN(?)" {
		t.Fatalf("收据片条件不对: %q", got)
	}
	if e.commerceShards().Args()[0] != 3 || e.receiptShards().Args()[0] != 9 {
		t.Fatal("两类工作各自按自己的键分工")
	}
}

// 拍品结案(成交或按撤单)的那一步回它引用的出价,供 advance 提交之后唤醒(delivery_order.go 的拍卖退回靠它及时退款);其余情况回 0。
func TestFinishedLotBid(t *testing.T) {
	w := &tradepb.TradeCommerceWorkflow{WinningBidId: 42}
	lot := func(status uint32) *tradepb.TradeCommerceRecord { return &tradepb.TradeCommerceRecord{Kind: 5, Status: status} }
	cases := []struct {
		name          string
		before, after *tradepb.TradeCommerceRecord
		want          uint64
	}{
		{"成交", lot(Delivering), lot(Settled), 42},
		{"整单退回后按撤单结案", lot(CancelPending), lot(Cancelled), 42},
		{"还在交付", lot(Delivering), lot(Delivering), 0},
		{"转人工不算结案", lot(Delivering), lot(Manual), 0},
		{"已经结案的再推一次", lot(Settled), lot(Settled), 0},
		{"不是拍品", &tradepb.TradeCommerceRecord{Kind: 3, Status: Delivering}, &tradepb.TradeCommerceRecord{Kind: 3, Status: Settled}, 0},
	}
	for _, tc := range cases {
		if got := finishedLotBid(tc.before, tc.after, w); got != tc.want {
			t.Errorf("%s: finishedLotBid = %d, want %d", tc.name, got, tc.want)
		}
	}
	if got := finishedLotBid(lot(CancelPending), lot(Cancelled), &tradepb.TradeCommerceWorkflow{}); got != 0 {
		t.Errorf("没有出价的拍品撤单: finishedLotBid = %d, want 0", got)
	}
}
