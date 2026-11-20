package mall

import "testing"

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
