package channel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

const (
	dyAppID          = "tt0000000000000001"
	dyChannelOrderNo = "dy_channel_order_20261120001"
	dyNonce          = "8932"
)

var dyTimestamp = strconv.FormatInt(testPaidAt.Unix(), 10)

func newDouyinFixture(t *testing.T) (*DouyinMinigame, string) {
	t.Helper()
	token := testSecret(t)
	ch, err := NewDouyinMinigame(DouyinMinigameConfig{AppID: dyAppID, PushToken: token})
	if err != nil {
		t.Fatalf("NewDouyinMinigame: %v", err)
	}
	return ch, token
}

func dyEnvelope(t *testing.T, token, ts, nonce, msg, typ string) []byte {
	t.Helper()
	sig := sortedSHA1(token, ts, nonce, msg)
	env := map[string]string{"timestamp": ts, "nonce": nonce, "msg": msg, "signature": sig}
	if typ != "" {
		env["type"] = typ
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal douyin envelope: %v", err)
	}
	return raw
}

func TestNewDouyinMinigameConfigAndPrepare(t *testing.T) {
	if _, err := NewDouyinMinigame(DouyinMinigameConfig{AppID: dyAppID}); err == nil {
		t.Fatal("NewDouyinMinigame accepted empty PushToken")
	}
	if _, err := NewDouyinMinigame(DouyinMinigameConfig{PushToken: "tok"}); err == nil {
		t.Fatal("NewDouyinMinigame accepted empty AppID")
	}

	ch, _ := newDouyinFixture(t)
	if NameOf(ch.ID()) != NameDouyinMinigame || ch.Name() != NameDouyinMinigame || !ch.Supports("CNY") || ch.Supports("USD") || ch.NeedsProduct() {
		t.Fatalf("identity: id=%v name=%q", ch.ID(), ch.Name())
	}

	ctx := context.Background()
	prep, err := ch.Prepare(ctx, Order{OutTradeNo: testOutTradeNo, AmountMinor: 600, Currency: "CNY", Title: "60钻石礼包"}, Ref{})
	if err != nil || prep.Params.Douyin == nil {
		t.Fatalf("Prepare: %v", err)
	}
	p := prep.Params.Douyin
	if p.Mode != "game" || p.Env != 0 || p.CurrencyType != "CNY" || p.Platform != "android" || p.BuyQuantity != 1 ||
		p.ZoneId != "1" || p.CustomId != testOutTradeNo || p.GoodType != 2 || p.OrderAmount != 600 || p.GoodName != "60钻石礼包" {
		t.Fatalf("douyin params = %+v", p)
	}

	// 抖音文档规定 goodName 不超过 10 个字符:超长截断,空串回落默认名。
	longPrep, err := ch.Prepare(ctx, Order{OutTradeNo: testOutTradeNo, AmountMinor: 600, Currency: "CNY", Title: "一二三四五六七八九十十一"}, Ref{})
	if err != nil || longPrep.Params.Douyin.GoodName != "一二三四五六七八九十" {
		t.Fatalf("long title GoodName = %q, %v", longPrep.Params.Douyin.GoodName, err)
	}
	emptyPrep, err := ch.Prepare(ctx, Order{OutTradeNo: testOutTradeNo, AmountMinor: 600, Currency: "CNY"}, Ref{})
	if err != nil || emptyPrep.Params.Douyin.GoodName != "游戏道具" {
		t.Fatalf("empty title GoodName = %q, %v", emptyPrep.Params.Douyin.GoodName, err)
	}
	if _, err := ch.Prepare(ctx, Order{OutTradeNo: testOutTradeNo, AmountMinor: 600, Currency: "USD"}, Ref{}); err == nil {
		t.Fatal("Prepare accepted USD")
	}

	if _, err := ch.Query(ctx, QueryRequest{OutTradeNo: testOutTradeNo}); !errors.Is(err, ErrQueryUnsupported) {
		t.Fatalf("Query err = %v", err)
	}
	if err := ch.Close(ctx, testOutTradeNo); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if ref, err := ch.Refund(ctx, RefundRequest{}); err != nil || ref.State != RefundFailed || ref.Detail != "NO_REFUND_API" {
		t.Fatalf("Refund = %+v, %v", ref, err)
	}
}

// 开发者后台配置回调地址时的 GET 握手:按 token / timestamp / nonce / msg 排序做 SHA-1,通过原样回 echostr。
func TestDouyinHandshakeEchoesOnlyForValidSignature(t *testing.T) {
	ch, token := newDouyinFixture(t)
	run := func(method, sig, msg, echostr string) (bool, int, string) {
		q := url.Values{"timestamp": {dyTimestamp}, "nonce": {dyNonce}, "signature": {sig}}
		if msg != "" {
			q.Set("msg", msg)
		}
		if echostr != "" {
			q.Set("echostr", echostr)
		}
		rec := httptest.NewRecorder()
		handled := ch.Handshake(rec, httptest.NewRequest(method, "/notify/douyin_minigame?"+q.Encode(), nil))
		return handled, rec.Code, rec.Body.String()
	}

	sigWithMsg := sortedSHA1(token, dyTimestamp, dyNonce, "verify")
	for _, valid := range []string{sigWithMsg, strings.ToUpper(sigWithMsg)} {
		if handled, status, body := run(http.MethodGet, valid, "verify", "echo-dy-1"); !handled || status != http.StatusOK || body != "echo-dy-1" {
			t.Fatalf("valid handshake = %v %d %q", handled, status, body)
		}
	}
	sigNoMsg := sortedSHA1(token, dyTimestamp, dyNonce, "")
	if handled, status, body := run(http.MethodGet, sigNoMsg, "", "echo-dy-2"); !handled || status != http.StatusOK || body != "echo-dy-2" {
		t.Fatalf("valid handshake without msg = %v %d %q", handled, status, body)
	}
	if handled, status, body := run(http.MethodGet, strings.Repeat("0", 40), "verify", "echo-dy-1"); !handled || status != http.StatusUnauthorized || body != "" {
		t.Fatalf("bad signature handshake = %v %d %q", handled, status, body)
	}
	if handled, status, _ := run(http.MethodGet, sigWithMsg, "verify", ""); !handled || status != http.StatusUnauthorized {
		t.Fatalf("empty echostr handshake = %v %d", handled, status)
	}
	if handled, _, _ := run(http.MethodPost, sigWithMsg, "verify", "echo-dy-1"); handled {
		t.Fatal("POST was treated as handshake")
	}
}

// POST 支付回调:验签、解析 OrderSuccessPayInfo、支持秒与毫秒时间戳、无关类型跳过、篡改与坏字段拒绝。
func TestDouyinParseNotificationAndAck(t *testing.T) {
	ch, token := newDouyinFixture(t)
	msg := `{"appid":"` + dyAppID + `","cp_orderno":"` + testOutTradeNo + `","order_no_channel":"` + dyChannelOrderNo + `","amount_cent":600,"currency":"CNY"}`

	// 1. 正常支付回调(秒级时间戳,带 type=payment)
	note, err := ch.ParseNotification(nil, dyEnvelope(t, token, dyTimestamp, dyNonce, msg, "payment"))
	if err != nil || note.Payment == nil || len(note.Revocations) != 0 {
		t.Fatalf("ParseNotification = %+v, %v", note, err)
	}
	want := Payment{
		TransactionID: testOutTradeNo, OutTradeNo: testOutTradeNo, AmountMinor: 600,
		Currency: "CNY", PaidAt: testPaidAt, ChannelOrderID: dyChannelOrderNo,
	}
	if !samePayment(*note.Payment, want) {
		t.Fatalf("payment = %+v, want %+v", *note.Payment, want)
	}

	// 2. 毫秒级时间戳、省略 type 与 currency(默认 CNY)
	msTS := strconv.FormatInt(testPaidAt.UnixMilli(), 10)
	msgNoCurrency := `{"appid":"` + dyAppID + `","cp_orderno":"` + testOutTradeNo + `","order_no_channel":"` + dyChannelOrderNo + `","amount_cent":600}`
	note, err = ch.ParseNotification(nil, dyEnvelope(t, token, msTS, dyNonce, msgNoCurrency, ""))
	if err != nil || note.Payment == nil || !samePayment(*note.Payment, want) {
		t.Fatalf("ms timestamp & default currency = %+v, %v", note, err)
	}

	// 3. 非支付类型验过签照常应答,不产生付款
	if note, err := ch.ParseNotification(nil, dyEnvelope(t, token, dyTimestamp, dyNonce, msg, "other")); err != nil || note.Payment != nil {
		t.Fatalf("non-payment type = %+v, %v", note, err)
	}

	// 4. 拒绝情形
	for name, body := range map[string][]byte{
		"wrong token":     dyEnvelope(t, testSecret(t), dyTimestamp, dyNonce, msg, "payment"),
		"tampered msg":    tamper(t, dyEnvelope(t, token, dyTimestamp, dyNonce, msg, "payment"), "600", "601"),
		"empty timestamp": dyEnvelope(t, token, "", dyNonce, msg, "payment"),
		"empty nonce":     dyEnvelope(t, token, dyTimestamp, "", msg, "payment"),
		"empty msg":       dyEnvelope(t, token, dyTimestamp, dyNonce, "", "payment"),
		"wrong appid":     dyEnvelope(t, token, dyTimestamp, dyNonce, strings.Replace(msg, dyAppID, "tt-other", 1), "payment"),
		"no cp_orderno":   dyEnvelope(t, token, dyTimestamp, dyNonce, strings.Replace(msg, testOutTradeNo, "", 1), "payment"),
		"zero amount":     dyEnvelope(t, token, dyTimestamp, dyNonce, strings.Replace(msg, "600", "0", 1), "payment"),
		"negative amount": dyEnvelope(t, token, dyTimestamp, dyNonce, strings.Replace(msg, "600", "-10", 1), "payment"),
		"bad json":        []byte(`not-json`),
	} {
		if _, err := ch.ParseNotification(nil, body); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("%s: err = %v, want ErrBadSignature", name, err)
		}
	}

	// 5. WriteAck
	if status, body := ackOf(ch, AckOK); status != http.StatusOK || body != `{"err_no":0,"err_tips":"success"}` {
		t.Fatalf("AckOK = %d %q", status, body)
	}
	for _, kind := range []AckKind{AckRetry, AckRejected} {
		if status, body := ackOf(ch, kind); status != http.StatusInternalServerError || body != `{"err_no":1,"err_tips":"failed"}` {
			t.Fatalf("ack %d = %d %q", kind, status, body)
		}
	}
}
