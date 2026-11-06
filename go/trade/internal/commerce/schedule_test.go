package commerce

import (
	"testing"

	tradepb "proto/trade"
)

// 缺口清单第 16 条:推进一步没有变化时的下次到期(docs/design/jubaozhai-market.md §15.8)。
func TestNextDueMsByStage(t *testing.T) {
	const now = uint64(1_000_000)
	hour := uint64(idleRecheck.Milliseconds())
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
		{"全部成交的母单等子单收尾,每轮复查", &tradepb.TradeCommerceRecord{Kind: 4, Side: 1, Quantity: 5, FilledQuantity: 5},
			&tradepb.TradeCommerceWorkflow{Stage: "market_open"}, now},
		{"等子单的母单每轮复查", &tradepb.TradeCommerceRecord{Kind: 4, Side: 1, Quantity: 5, FilledQuantity: 5},
			&tradepb.TradeCommerceWorkflow{Stage: "market_wait"}, now},
		{"官方退款一小时复查", &tradepb.TradeCommerceRecord{Kind: 1, Status: 8},
			&tradepb.TradeCommerceWorkflow{Stage: "official_refund"}, now + hour},
		{"有腿在途一律每轮复查", &tradepb.TradeCommerceRecord{Kind: 3, ExpiresMs: now + 600_000},
			&tradepb.TradeCommerceWorkflow{Stage: "open", Legs: []*tradepb.TradeCommerceLeg{settled, inFlight}}, now},
		{"人民币寄售不改节奏", &tradepb.TradeCommerceRecord{Kind: 6, ExpiresMs: now + 600_000},
			&tradepb.TradeCommerceWorkflow{Stage: "open"}, now},
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
