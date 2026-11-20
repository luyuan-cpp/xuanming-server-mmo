package commerce

import (
	"testing"

	assetpb "proto/common/asset"
	tradepb "proto/trade"
)

// 官方合同的发放跟着付款修订号收敛(payment-recharge.md §22.6):逐步推演 plan,腿的结局由测试直接写。
func TestPlanOfficialFollowsPaymentRevisions(t *testing.T) {
	const appliedOutcome, rejectedOutcome = 2, 3
	newContract := func() (*tradepb.TradeCommerceRecord, *tradepb.TradeCommerceWorkflow) {
		bundle := &assetpb.AssetBundle{Currencies: []*assetpb.CurrencyAmount{{CurrencyType: 1, Amount: 60}}}
		return &tradepb.TradeCommerceRecord{Kind: 1, Status: Delivering, BuyerPlayerId: 7},
			&tradepb.TradeCommerceWorkflow{Stage: stageOfficialDeliver, OfficialBundle: bundle}
	}
	// revise 与 Engine.applyPaymentRevision 写合同的方式相同。
	revise := func(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow, revision uint32) {
		w.PaymentRevision, w.PaymentRevoked = revision, revision%2 == 1
		if r.Status != Manual {
			w.Stage, r.Status = stageOfficialDeliver, Delivering
		}
	}
	// settle 给最后一条腿写结局再推进一步。
	settle := func(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow, outcome uint32) {
		w.Legs[len(w.Legs)-1].Outcome = outcome
		planOfficial(r, w, 1)
	}
	expect := func(t *testing.T, r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow, stage string, status uint32, lastLeg string, stream uint32) {
		t.Helper()
		if w.Stage != stage || r.Status != status {
			t.Fatalf("stage=%q status=%d, want %q %d", w.Stage, r.Status, stage, status)
		}
		if lastLeg == "" {
			return
		}
		last := w.Legs[len(w.Legs)-1]
		if last.Role != lastLeg || last.Stream != stream || last.PlayerId != 7 {
			t.Fatalf("last leg %q stream %d player %d, want %q stream %d", last.Role, last.Stream, last.PlayerId, lastLeg, stream)
		}
	}

	t.Run("发放、撤销扣回、撤回补发", func(t *testing.T) {
		r, w := newContract()
		planOfficial(r, w, 1)
		expect(t, r, w, stageOfficialDeliver, Delivering, officialCreditRole, 4)
		planOfficial(r, w, 1)
		if len(w.Legs) != 1 {
			t.Fatalf("腿在途时不能再加腿,现有 %d 条", len(w.Legs))
		}
		settle(r, w, appliedOutcome)
		expect(t, r, w, "done", Settled, "", 0)

		revise(r, w, 1)
		planOfficial(r, w, 1)
		expect(t, r, w, stageOfficialDeliver, Delivering, "official_reverse_1", 3)
		if state := officialReversalState(r, w); state != "" {
			t.Fatalf("扣回在途时不能回执,得到 %q", state)
		}
		settle(r, w, appliedOutcome)
		expect(t, r, w, stageOfficialReversed, Cancelled, "", 0)
		if delivery, reversal := officialDeliveryState(r, w), officialReversalState(r, w); delivery != "DELIVERED" || reversal != "REVERSED" {
			t.Fatalf("delivery=%q reversal=%q", delivery, reversal)
		}

		revise(r, w, 2)
		planOfficial(r, w, 1)
		expect(t, r, w, stageOfficialDeliver, Delivering, "official_regrant_2", 4)
		settle(r, w, appliedOutcome)
		expect(t, r, w, "done", Settled, "", 0)
		if reversal := officialReversalState(r, w); reversal != "DELIVERED" {
			t.Fatalf("reversal=%q", reversal)
		}
		if len(w.Legs) != 3 {
			t.Fatalf("每个修订号至多一条腿,现有 %d 条", len(w.Legs))
		}
	})

	t.Run("扣不回转人工", func(t *testing.T) {
		r, w := newContract()
		planOfficial(r, w, 1)
		settle(r, w, appliedOutcome)
		revise(r, w, 1)
		planOfficial(r, w, 1)
		settle(r, w, rejectedOutcome)
		expect(t, r, w, "manual", Manual, "", 0)
		if w.Quarantine.GetReason() != manualReasonPaymentReversal || officialReversalState(r, w) != "MANUAL" {
			t.Fatalf("quarantine=%v reversal=%q", w.Quarantine, officialReversalState(r, w))
		}
		revise(r, w, 2)
		if r.Status != Manual || w.PaymentRevision != 2 {
			t.Fatalf("已转人工的合同只记修订号: status=%d revision=%d", r.Status, w.PaymentRevision)
		}
	})

	t.Run("没发过就被撤销:不发放", func(t *testing.T) {
		r, w := newContract()
		revise(r, w, 1)
		planOfficial(r, w, 1)
		expect(t, r, w, stageOfficialReversed, Cancelled, "", 0)
		if len(w.Legs) != 0 || officialDeliveryState(r, w) != "VOIDED" || officialReversalState(r, w) != "REVERSED" {
			t.Fatalf("legs=%d delivery=%q reversal=%q", len(w.Legs), officialDeliveryState(r, w), officialReversalState(r, w))
		}
	})

	t.Run("没修订过的首次发放被拒:转退款", func(t *testing.T) {
		r, w := newContract()
		planOfficial(r, w, 1)
		settle(r, w, rejectedOutcome)
		expect(t, r, w, "official_refund", 8, "", 0)
		if officialDeliveryState(r, w) != "REFUND_PENDING" {
			t.Fatalf("delivery=%q", officialDeliveryState(r, w))
		}
	})
}

func TestOfficialBundleIsItemsOrCurrency(t *testing.T) {
	if b, ok := officialBundle(mallIntent{Config: 10, Count: 2}); !ok || len(b.Items) != 1 || len(b.Currencies) != 0 {
		t.Fatalf("item grant: %v %v", b, ok)
	}
	if b, ok := officialBundle(mallIntent{CurrencyType: 1, CurrencyAmount: 60}); !ok || len(b.Currencies) != 1 || b.Currencies[0].Amount != 60 {
		t.Fatalf("currency grant: %v %v", b, ok)
	}
	if b, ok := officialBundle(mallIntent{CurrencyAmount: 5}); !ok || b.Currencies[0].CurrencyType != 0 {
		t.Fatalf("currency type 0 is a real currency: %v %v", b, ok)
	}
	for _, bad := range []mallIntent{{}, {Config: 10}, {Config: 10, Count: 4097}, {Config: 10, Count: 1, CurrencyAmount: 1}, {CurrencyType: 999, CurrencyAmount: 1}, {CurrencyType: 1}} {
		if _, ok := officialBundle(bad); ok {
			t.Fatalf("accepted %+v", bad)
		}
	}
}
