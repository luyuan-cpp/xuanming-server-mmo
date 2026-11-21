package mall

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 商城签名的跨语言黄金样例:与 go/trade mall_signature_test.go、robot commerce_mock_payer_test.go、
// java/mall_game HmacSignatureGoldenTest 是同一组输入、同一个期望值。改签名算法必须几边同批改,不能只改这里的字面量。
func TestSignatureMatchesCrossLanguageGolden(t *testing.T) {
	const body = `{"orderId":"00112233-4455-6677-8899-aabbccddeeff","channel":"mock-dev","transactionId":"mockdev-00112233445566778899aabbccddeeff","amountMinor":600,"currency":"CNY"}`
	got := Signature("commerce-golden-payment-secret-0123456789", "payment", "payment-adapter", "1700000000123", "golden-nonce-0000000001",
		"POST", "/internal/payments", []byte(body))
	if got != "886584609f3fb1fa50a5ddb30f02837fb7e75d11fa0075f344447872c7eaf051" {
		t.Fatalf("Signature = %s", got)
	}
}

func TestNewRejectsUnsafeConfiguration(t *testing.T) {
	const secret = "commerce-golden-payment-secret-0123456789"
	if _, err := New("http://127.0.0.1:18120", "payment-adapter", secret, 0); err != nil {
		t.Fatalf("valid configuration rejected: %v", err)
	}
	for name, args := range map[string][3]string{
		"short secret":   {"http://127.0.0.1:18120", "payment-adapter", "short"},
		"no host":        {"http://", "payment-adapter", secret},
		"with query":     {"http://127.0.0.1:18120/?a=1", "payment-adapter", secret},
		"with userinfo":  {"http://user@127.0.0.1:18120", "payment-adapter", secret},
		"empty subject":  {"http://127.0.0.1:18120", "", secret},
		"unknown scheme": {"ftp://127.0.0.1", "payment-adapter", secret},
	} {
		if _, err := New(args[0], args[1], args[2], 0); err == nil {
			t.Fatalf("New accepted %s", name)
		}
	}
}

func TestPendingRefundsPagination(t *testing.T) {
	var seenCursors []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/refunds/poll" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Cursor string `json:"cursor"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		seenCursors = append(seenCursors, req.Cursor)
		w.Header().Set("Content-Type", "application/json")
		if req.Cursor == "" {
			_ = json.NewEncoder(w).Encode(PendingRefundPage{
				Entries:    []PendingRefund{{OrderID: "00000000-0000-0000-0000-000000000001", Channel: "wechat_pay", TransactionID: "tx-1", AmountMinor: 100, Currency: "CNY", Reason: "DUPLICATE_PAYMENT"}},
				NextCursor: "r1:00000000-0000-0000-0000-000000000001:wechat_pay:tx-1",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(PendingRefundPage{
			Entries: []PendingRefund{{OrderID: "00000000-0000-0000-0000-000000000002", Channel: "alipay", TransactionID: "tx-2", AmountMinor: 200, Currency: "CNY", Reason: "CANCELLED_LATE"}},
		})
	}))
	defer server.Close()

	client, err := New(server.URL, "payment-adapter", "commerce-golden-payment-secret-0123456789", 0)
	if err != nil {
		t.Fatal(err)
	}
	first, err := client.PendingRefunds(context.Background(), "")
	if err != nil || len(first.Entries) != 1 || first.NextCursor == "" {
		t.Fatalf("first page = %+v, %v", first, err)
	}
	second, err := client.PendingRefunds(context.Background(), first.NextCursor)
	if err != nil || len(second.Entries) != 1 || second.NextCursor != "" {
		t.Fatalf("second page = %+v, %v", second, err)
	}
	if len(seenCursors) != 2 || seenCursors[0] != "" || seenCursors[1] != first.NextCursor {
		t.Fatalf("seenCursors = %v", seenCursors)
	}
}
