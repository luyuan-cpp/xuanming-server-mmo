package channel

import "testing"

// 微信小游戏虚拟支付的两种签名,取自微信官方文档的示例(《小游戏虚拟支付》签名详解),并经 Python hmac 复算。
// 道具直购的 paySig / signature 与查单的 pay_sig / signature 都是这两个算法,只是拼的串不同。
func TestWechatMinigameSignaturesMatchOfficialVectors(t *testing.T) {
	const body = `{"offer_id": "12345678", "openid": "oUrsfxxxxxxxxxx", "ts": 1668136271, "zone_id": "1", "env": 0}`
	if got := hmacHex([]byte("12345"), "/wxa/game/getbalance&"+body); got != "11bac6388871d29c055c7d16fbe42e8d646855b666faf89b15c815218b1b23bd" {
		t.Fatalf("pay_sig = %s", got)
	}
	const raw = `{"kv_list":[{"key":"score","value":"100"},{"key":"gold","value":"3000"}]}`
	if got := hmacHex([]byte("9hAb/NEYUlkaMBEsmFgzig=="), raw); got != "282c0d87a489e3cf6cb58d93c747716563ed67fb13ba38c2433327097adbe02c" {
		t.Fatalf("signature = %s", got)
	}
}

// 金额不经浮点:6.10 元必须正好是 610 分,多于两位小数、带符号、空串一律拒绝。
func TestYuanConversionIsExact(t *testing.T) {
	for text, fen := range map[string]uint64{"0.01": 1, "6": 600, "6.1": 610, "6.10": 610, "648.00": 64800, "90000000000000.00": 9_000_000_000_000_000} {
		got, err := ParseYuan(text)
		if err != nil || got != fen {
			t.Fatalf("ParseYuan(%q) = %d %v, want %d", text, got, err, fen)
		}
	}
	for _, bad := range []string{"", ".5", "6.", "6.100", "-1", "+1", "1e3", "6,10", " 6", "90000000000000.01", "99999999999999999"} {
		if _, err := ParseYuan(bad); err == nil {
			t.Fatalf("ParseYuan accepted %q", bad)
		}
	}
	for fen, text := range map[uint64]string{1: "0.01", 610: "6.10", 64800: "648.00"} {
		if got := FormatYuan(fen); got != text {
			t.Fatalf("FormatYuan(%d) = %q", fen, got)
		}
	}
}

// Google 的交易号由 purchase token 派生:同一个 token 永远得到同一个号,且能原样报给商城。
func TestGoogleTransactionIDIsDeterministic(t *testing.T) {
	first, again := GoogleTransactionID("token-a"), GoogleTransactionID("token-a")
	if first != again || first == GoogleTransactionID("token-b") || len(first) == 0 || len(first) > 128 {
		t.Fatalf("GoogleTransactionID = %q / %q", first, again)
	}
}
