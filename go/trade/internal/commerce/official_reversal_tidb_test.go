package commerce

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	rollbackpb "proto/common/rollback"
)

// 付款被渠道撤销又撤回(docs/design/payment-recharge.md §22.6),商城 ↔ trade 的整条协议在真库上走一遍:
// 已交付的订单收到修订号 1 → 一条扣回腿(交易扣出流,TX_PAYMENT_REVERSAL)→ 扣到后回执 REVERSED;
// 修订号 2 → 一条补发腿 → 到账后回执 DELIVERED。腿没有结局之前不回执。
// 夹具同 TestTiDBMallBridgeWaitsForCommittedLegAndRecoversLostReceipt:资产终局由 settleLegs 直接写,不冒充 scene 的端到端。
func TestTiDBMallReversalClawsBackThenRegrants(t *testing.T) {
	e := tiDBEngine(t)
	ctx := context.Background()
	in := mallIntent{OrderID: "00112233-4455-6677-8899-aabbccddeeff", BuyerID: "22", BuyerAccount: "account-22", BuyerOwnerRevision: "1", SKU: 1, Quantity: 1, Config: 10, Count: 2, Title: "玄铁"}

	// 假商城:交付待办在收到回执后消失;扣回待办给出当前修订号,这个修订号收到回执后消失。
	type reversalReceipt struct {
		Revision uint32 `json:"revision"`
		State    string `json:"state"`
	}
	var mu sync.Mutex
	delivered := false
	revision, receipted := uint32(0), uint32(0)
	var reversalReceipts []reversalReceipt
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		last := ""
		switch r.URL.Path {
		case "/internal/deliveries/poll":
			page := mallPollPage{Entries: []mallPollEntry{}}
			if !delivered {
				page.Entries = append(page.Entries, mallPollEntry{Intent: &in, Cursor: &last, PaymentRevision: revision})
			}
			_ = json.NewEncoder(w).Encode(page)
		case "/internal/deliveries/receipt":
			delivered = true
		case "/internal/reversals/poll":
			entries := []mallReversalEntry{}
			if revision > receipted {
				entries = append(entries, mallReversalEntry{Intent: &in, Revision: revision, Entitled: revision%2 == 0, Cursor: &last})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"entries": entries})
		case "/internal/reversals/receipt":
			var receipt reversalReceipt
			if err := json.NewDecoder(r.Body).Decode(&receipt); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			reversalReceipts = append(reversalReceipts, receipt)
			receipted = receipt.Revision
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	m, err := NewMall(server.URL, "gateway-only-test-secret-00000000001", "delivery-only-test-secret-0000000002", e)
	require.NoError(t, err)

	setRevision := func(v uint32) {
		mu.Lock()
		defer mu.Unlock()
		revision = v
	}
	receipts := func() []reversalReceipt {
		mu.Lock()
		defer mu.Unlock()
		return append([]reversalReceipt(nil), reversalReceipts...)
	}
	countOps := func(where string) int {
		var n int
		require.NoError(t, e.Repo.DB.QueryRow("SELECT COUNT(*) FROM trade_asset_op WHERE "+where).Scan(&n))
		return n
	}
	// 商城一轮(建合同 / 记修订号 / 回执)之后合同推进一轮(出腿 / 收结局)。
	step := func() {
		require.NoError(t, m.Tick(ctx))
		tick(t, e)
	}
	reversalOps := fmt.Sprintf("stream=3 AND tx_type=%d", uint32(rollbackpb.TransactionType_TX_PAYMENT_REVERSAL))

	// 1. 正常交付。
	step()
	settleLegs(t, e, 2)
	step()
	require.NoError(t, m.Tick(ctx))
	require.Equal(t, 1, countOps("stream=4"), "交付只发一条发放腿")
	require.Empty(t, receipts(), "没有修订就没有扣回回执")

	// 2. 渠道撤销:扣回。
	setRevision(1)
	step()
	require.Equal(t, 1, countOps(reversalOps), "撤销生成一条扣回腿,带付款撤销的流水类型")
	require.NoError(t, m.Tick(ctx))
	require.Empty(t, receipts(), "扣回腿在途时不回执")
	settleLegs(t, e, 2)
	step()
	require.NoError(t, m.Tick(ctx))
	require.Equal(t, []reversalReceipt{{Revision: 1, State: "REVERSED"}}, receipts())
	step()
	require.Equal(t, 1, countOps(reversalOps), "同一个修订号重放不再出腿")

	// 3. 撤销被撤回:补发。
	setRevision(2)
	step()
	require.Equal(t, 2, countOps("stream=4"), "撤回生成一条补发腿")
	require.Len(t, receipts(), 1, "补发腿在途时不回执")
	settleLegs(t, e, 2)
	step()
	require.NoError(t, m.Tick(ctx))
	require.Equal(t, []reversalReceipt{{Revision: 1, State: "REVERSED"}, {Revision: 2, State: "DELIVERED"}}, receipts())
	require.Equal(t, 1, countOps("stream=3"), "全程只扣回一次")
}
