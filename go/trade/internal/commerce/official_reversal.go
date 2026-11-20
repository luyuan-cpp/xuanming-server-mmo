package commerce

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	tradepb "proto/trade"
	"trade/internal/data"
)

// 官方订单的发放跟着付款的最新结论走(docs/design/payment-recharge.md §22.6)。
//
// 商城给每张官方订单绑定付款的渠道翻转编一个只增的修订号:0 是原始付款,奇数 = 已被渠道撤销(用户在渠道侧退款、作废),
// 大于 0 的偶数 = 撤销又被撤回。修订号记在合同上(workflow.payment_revision / payment_revoked),planOfficial 按腿的结局
// 算出买家现在是否持有这份发放,与该不该持有对不上就补一条腿:
//   - 持有却已撤销:扣回腿 official_reverse_<修订号>(交易扣出流,TX_PAYMENT_REVERSAL)。扣不到(已经用掉)转人工,不透支;
//   - 未持有却该持有:从没发过走原发放腿 official_credit,发过又扣回的走补发腿 official_regrant_<修订号>。
// 腿名带修订号,所以同一个修订号至多一条腿,重放不会重复扣或重复发。

const (
	// stageOfficialDeliver 是官方合同还在收敛发放的阶段;stageOfficialReversed 是付款已撤销且买家不持有发放的终局。
	stageOfficialDeliver  = "official_deliver"
	stageOfficialReversed = "official_reversed"

	officialCreditRole    = "official_credit"
	officialReversePrefix = "official_reverse_"
	officialRegrantPrefix = "official_regrant_"

	manualReasonPaymentReversal = "payment_reversal_failed" // 付款被渠道修订后,扣回或补发那条腿没有到账
)

// isOfficialReverseLeg 判一条腿是不是付款撤销后的扣回腿(含人工补发的 "#k" 后缀)。
func isOfficialReverseLeg(role string) bool {
	return strings.HasPrefix(role, officialReversePrefix)
}

// officialHeld 按腿的结局算买家现在是否持有这份发放:到账的发放腿之后持有,到账的扣回腿之后不持有。
// 被人工补发取代的腿不算(由取代它的腿负责)。settled 为假表示还有腿没有结局,这一步不能下结论。
func officialHeld(w *tradepb.TradeCommerceWorkflow) (held, settled bool) {
	for _, l := range w.Legs {
		if l.SupersededBy != "" {
			continue
		}
		if !done(l) {
			return false, false
		}
		if applied(l) {
			held = l.Stream == 4
		}
	}
	return held, true
}

// planOfficial 把官方合同的发放收敛到付款的最新结论,只在 official_deliver 阶段调用。
func planOfficial(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow, now uint64) {
	held, settled := officialHeld(w)
	if !settled {
		return
	}
	entitled := !w.PaymentRevoked
	if held == entitled {
		if entitled {
			w.Stage, r.Status = "done", Settled
		} else {
			w.Stage, r.Status = stageOfficialReversed, Cancelled
		}
		return
	}
	revision := strconv.FormatUint(uint64(w.PaymentRevision), 10)
	role, stream := officialCreditRole, uint32(4)
	switch {
	case !entitled:
		role, stream = officialReversePrefix+revision, 3
	case leg(w, officialCreditRole) != nil && w.PaymentRevision > 0:
		role = officialRegrantPrefix + revision
	}
	if leg(w, role) == nil {
		addLeg(w, role, r.BuyerPlayerId, stream, w.OfficialBundle)
		return
	}
	// 这个修订号的腿已有结局(在途的上面已返回)却没让发放到位:没到账。
	if role == officialCreditRole && w.PaymentRevision == 0 {
		// 付款没被修订过的首次发放被拒:回报商城转退款(原有行为)。
		w.Stage, r.Status = "official_refund", 8
		return
	}
	// 修订过的付款不能再走退款(钱已被渠道退走,或订单状态已归扣回流程管):转人工。
	markManual(r, w, manualReasonPaymentReversal, fmt.Sprintf("腿 %s 没有到账,付款修订号 %d", role, w.PaymentRevision), now)
}

// applyPaymentRevision 把商城报来的修订号记到官方合同上(只增不减),并让合同回到收敛阶段、立即到期。
// 已转人工的合同只记修订号,不自动重开:它在等人处理。返回锁内读到(或刚存下)的合同。
//
// 只改合同自己的行,不动资产、不读角色归属,所以只锁合同行;之后的推进(advance)照常先拿角色锁再复核。
func (e *Engine) applyPaymentRevision(ctx context.Context, id uint64, revision uint32) (*tradepb.TradeCommerceRecord, error) {
	var out *tradepb.TradeCommerceRecord
	err := e.Repo.Tx(ctx, func(tx *sql.Tx) error {
		r, err := e.Repo.Lock(ctx, tx, id)
		if err != nil {
			return err
		}
		w, err := data.Workflow(r)
		if err != nil {
			return corrupt(err)
		}
		out = r
		if r.Kind != 1 || w.PaymentRevision >= revision {
			return nil
		}
		w.PaymentRevision, w.PaymentRevoked = revision, revision%2 == 1
		if r.Status != Manual {
			w.Stage, r.Status = stageOfficialDeliver, Delivering
		}
		return e.Repo.Save(ctx, tx, r, w, e.now())
	})
	return out, err
}

// officialDeliveryState 是回给商城交付待办的结论;空串表示还没有结论。
// 付款撤销后不持有发放的合同:发过又扣回的仍报 DELIVERED(确实交付过),从没发过的报 VOIDED。
func officialDeliveryState(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) string {
	switch r.Status {
	case Settled:
		return "DELIVERED"
	case 8:
		return "REFUND_PENDING"
	case Manual:
		return "MANUAL"
	case Cancelled:
		if applied(currentLeg(w, officialCreditRole)) {
			return "DELIVERED"
		}
		return "VOIDED"
	}
	return ""
}

// officialReversalState 是回给商城扣回待办的结论;空串表示合同还没收敛到这个修订号。
// 修订号一变合同就回到收敛阶段(applyPaymentRevision),所以这里看到的终局一定是按当前修订号得出的。
func officialReversalState(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) string {
	switch {
	case r.Status == Manual:
		return "MANUAL"
	case r.Status == Settled && !w.PaymentRevoked:
		return "DELIVERED"
	case r.Status == Cancelled && w.PaymentRevoked:
		return "REVERSED"
	}
	return ""
}
