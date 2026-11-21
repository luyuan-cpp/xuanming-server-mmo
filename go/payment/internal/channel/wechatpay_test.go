package channel

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	wxAppID         = "wx0000000000000001"
	wxMchID         = "1900000001"
	wxSerialNo      = "TESTMERCHANTSERIAL0001"
	wxKeyID         = "PUB_KEY_ID_TEST0001"
	wxNotifyURL     = "https://pay.example.test/notify/wechat_pay"
	wxTransactionID = "4200001234202611201234567890"
	wxOutRefundNo   = "rf0123456789abcdef0123456789abcd"
	wxRefundID      = "50000000382019052709732678859"
)

// wxRefundedAt 是用例里微信报的退款成功时刻:北京时间 2026-11-20 19:59:45。
var wxRefundedAt = time.Date(2026, 11, 20, 11, 59, 45, 0, time.UTC)

// wechatFixture 是一个渠道,外加扮演微信要用的密钥。
type wechatFixture struct {
	channel  *WechatPay
	merchant *rsa.PrivateKey // 商户私钥:渠道拿它给请求与调起参数签名,用例拿公钥验
	platform *rsa.PrivateKey // 「微信支付」的私钥:用例拿它给应答与回调签名,渠道拿公钥验
	apiKey   []byte          // APIv3 密钥:回调密文用它加密
}

func newWechatFixture(t *testing.T, endpoint string, client *http.Client) wechatFixture {
	t.Helper()
	f := wechatFixture{merchant: testRSAKey(t, "merchant"), platform: testRSAKey(t, "platform"), apiKey: []byte(testSecret(t))}
	ch, err := NewWechatPay(WechatPayConfig{AppID: wxAppID, MchID: wxMchID, MerchantSerialNo: wxSerialNo, MerchantKey: f.merchant,
		APIv3Key: f.apiKey, PlatformKeyID: wxKeyID, PlatformKey: &f.platform.PublicKey, NotifyURL: wxNotifyURL,
		Description: "测试充值", Endpoint: endpoint, HTTP: client, Now: fixedNow})
	if err != nil {
		t.Fatalf("NewWechatPay: %v", err)
	}
	f.channel = ch
	return f
}

// wechatAnswerHeader 给一段正文配上微信的签名头。应答与回调是同一套:验签串是「时间戳\n随机串\n正文\n」,
// 这里独立拼,不借用被测代码。
func wechatAnswerHeader(t *testing.T, key *rsa.PrivateKey, body []byte, at time.Time) http.Header {
	t.Helper()
	const nonce = "593BEC0C930BF1AFEB40B4A08C8FB242"
	timestamp := strconv.FormatInt(at.Unix(), 10)
	signature, err := signSHA256(key, []byte(timestamp+"\n"+nonce+"\n"+string(body)+"\n"))
	if err != nil {
		t.Errorf("sign answer: %v", err)
	}
	header := http.Header{}
	header.Set("Wechatpay-Serial", wxKeyID)
	header.Set("Wechatpay-Timestamp", timestamp)
	header.Set("Wechatpay-Nonce", nonce)
	header.Set("Wechatpay-Signature", signature)
	header.Set("Wechatpay-Signature-Type", "WECHATPAY2-SHA256-RSA2048")
	return header
}

var wechatAuthorization = regexp.MustCompile(`^WECHATPAY2-SHA256-RSA2048 mchid="([^"]*)",nonce_str="([0-9a-f]{32})",signature="([^"]*)",timestamp="([0-9]+)",serial_no="([^"]*)"$`)

// checkWechatRequest 按 APIv3 的规则核对一次请求:Authorization 头的形状、商户号、证书序列号、时间戳,
// 并用商户公钥验签名串「方法\n地址\n时间戳\n随机串\n正文\n」(没有正文时最后一行是空行)。
func checkWechatRequest(t *testing.T, merchant *rsa.PublicKey, r *http.Request, body []byte) {
	t.Helper()
	parts := wechatAuthorization.FindStringSubmatch(r.Header.Get("Authorization"))
	if parts == nil {
		t.Errorf("Authorization header has an unexpected shape: %q", r.Header.Get("Authorization"))
		return
	}
	mchID, nonce, signature, timestamp, serial := parts[1], parts[2], parts[3], parts[4], parts[5]
	if mchID != wxMchID || serial != wxSerialNo || timestamp != strconv.FormatInt(testNow.Unix(), 10) {
		t.Errorf("Authorization carries mchid=%q serial_no=%q timestamp=%q", mchID, serial, timestamp)
	}
	message := r.Method + "\n" + r.URL.RequestURI() + "\n" + timestamp + "\n" + nonce + "\n" + string(body) + "\n"
	if err := verifySHA256(merchant, []byte(message), signature); err != nil {
		t.Errorf("request signature does not verify over %q: %v", message, err)
	}
	if r.Header.Get("Wechatpay-Serial") != wxKeyID || r.Header.Get("Accept") != "application/json" {
		t.Errorf("request headers: Wechatpay-Serial=%q Accept=%q", r.Header.Get("Wechatpay-Serial"), r.Header.Get("Accept"))
	}
	if len(body) > 0 && r.Header.Get("Content-Type") != "application/json" {
		t.Errorf("request with a body has Content-Type %q", r.Header.Get("Content-Type"))
	}
}

// wechatAnswer 是假微信对一次请求的应答。header 为 nil 时,2xx 且有正文的应答按微信的规则签名(错误应答不签,渠道也不验);
// 非 nil 时原样用它,用来演示验签失败。
type wechatAnswer struct {
	status int
	body   string
	header http.Header
}

// startWechat 起一个假微信并把渠道接上去。假微信先核对每个请求的签名与公共头,记下「方法 地址 正文」,再由 answer 决定应答。
func startWechat(t *testing.T, answer func(method, uri string) wechatAnswer) (wechatFixture, *callLog) {
	t.Helper()
	merchant, platform := testRSAKey(t, "merchant"), testRSAKey(t, "platform")
	calls := &callLog{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		checkWechatRequest(t, &merchant.PublicKey, r, body)
		calls.add(r.Method + " " + r.URL.RequestURI() + " " + string(body))
		reply := answer(r.Method, r.URL.RequestURI())
		header := reply.header
		if header == nil && reply.status/100 == 2 && reply.body != "" {
			header = wechatAnswerHeader(t, platform, []byte(reply.body), testNow)
		}
		for name, values := range header {
			w.Header()[name] = values
		}
		w.WriteHeader(reply.status)
		_, _ = io.WriteString(w, reply.body)
	}))
	t.Cleanup(server.Close)
	return newWechatFixture(t, server.URL, server.Client()), calls
}

// always 是不看请求、总给同一个应答的假微信。
func always(reply wechatAnswer) func(string, string) wechatAnswer {
	return func(string, string) wechatAnswer { return reply }
}

func wechatTransactionJSON(state string) string {
	return `{"appid":"` + wxAppID + `","mchid":"` + wxMchID + `","out_trade_no":"` + testOutTradeNo +
		`","transaction_id":"` + wxTransactionID + `","trade_type":"APP","trade_state":"` + state +
		`","success_time":"2026-11-20T19:59:30+08:00","amount":{"total":600,"payer_total":550,"currency":"CNY","payer_currency":"CNY"}}`
}

func wechatRefundJSON(status string) string {
	return `{"refund_id":"` + wxRefundID + `","out_refund_no":"` + wxOutRefundNo + `","transaction_id":"` + wxTransactionID +
		`","out_trade_no":"` + testOutTradeNo + `","status":"` + status +
		`","success_time":"2026-11-20T19:59:45+08:00","amount":{"total":600,"refund":600,"currency":"CNY"}}`
}

// wxPayment 是上面那笔交易应当映射成的付款:金额取订单金额 total,不是扣掉优惠后的 payer_total。
func wxPayment() Payment {
	return Payment{TransactionID: wxTransactionID, OutTradeNo: testOutTradeNo, AmountMinor: 600, Currency: "CNY", PaidAt: testPaidAt}
}

func wxOrder() Order {
	return Order{OrderID: testOrderID, OutTradeNo: testOutTradeNo, AmountMinor: 600, Currency: "CNY", ExpireAt: testNow.Add(30 * time.Minute)}
}

// 三种签名串的行结构:每一项占一行,每行以 \n 结尾,最后一行也是;没有正文的请求最后一行是空行。
func TestWechatSignMessagesFollowTheDocumentedLayout(t *testing.T) {
	if got := wechatRequestMessage("POST", "/v3/pay/transactions/app", "1554208460", "nonce", []byte(`{"a":1}`)); got != "POST\n/v3/pay/transactions/app\n1554208460\nnonce\n{\"a\":1}\n" {
		t.Fatalf("request message = %q", got)
	}
	if got := wechatRequestMessage("GET", "/v3/x?mchid=1", "1554208460", "nonce", nil); got != "GET\n/v3/x?mchid=1\n1554208460\nnonce\n\n" {
		t.Fatalf("bodyless request message = %q", got)
	}
	if got := wechatAnswerMessage("1554209980", "nonce", []byte(`{"b":2}`)); got != "1554209980\nnonce\n{\"b\":2}\n" {
		t.Fatalf("answer message = %q", got)
	}
	if got := wechatAppSignMessage("wx8888888888888888", "1414561699", "nonce", "prepay"); got != "wx8888888888888888\n1414561699\nnonce\nprepay\n" {
		t.Fatalf("app sign message = %q", got)
	}
}

func TestNewWechatPayRejectsUnusableConfig(t *testing.T) {
	merchant, platform := testRSAKey(t, "merchant"), testRSAKey(t, "platform")
	good := WechatPayConfig{AppID: wxAppID, MchID: wxMchID, MerchantSerialNo: wxSerialNo, MerchantKey: merchant, APIv3Key: []byte(testSecret(t)),
		PlatformKeyID: wxKeyID, PlatformKey: &platform.PublicKey, NotifyURL: wxNotifyURL, Description: "测试充值"}
	if ch, err := NewWechatPay(good); err != nil || NameOf(ch.ID()) != NameWechatPay || ch.Name() != NameWechatPay || !ch.Supports("CNY") || ch.Supports("USD") || ch.NeedsProduct() {
		t.Fatalf("NewWechatPay(good): %v", err)
	}
	for name, edit := range map[string]func(*WechatPayConfig){
		"no app id":                func(c *WechatPayConfig) { c.AppID = "" },
		"no merchant key":          func(c *WechatPayConfig) { c.MerchantKey = nil },
		"no platform key":          func(c *WechatPayConfig) { c.PlatformKey = nil },
		"an APIv3 key of 31 bytes": func(c *WechatPayConfig) { c.APIv3Key = c.APIv3Key[:31] },
		"a platform certificate serial instead of a public key id": func(c *WechatPayConfig) { c.PlatformKeyID = "5157F09EFDC096DE15EBE81A47057A72" },
		"a notify URL with a query":                                func(c *WechatPayConfig) { c.NotifyURL += "?a=1" },
		"a description of 128 characters":                          func(c *WechatPayConfig) { c.Description = strings.Repeat("充", 128) },
	} {
		bad := good
		edit(&bad)
		if _, err := NewWechatPay(bad); err == nil {
			t.Fatalf("NewWechatPay accepted %s", name)
		}
	}
}

// APP 下单:请求按 APIv3 签名、正文字段齐全;调起参数用商户私钥签「appId\n时间戳\n随机串\nprepay_id\n」;
// prepay_id 在有效期内复用,过期、时刻不可信的重新下单。
func TestWechatPrepareSignsTheOrderAndTheAppParams(t *testing.T) {
	const prepayID = "wx201410272009395522657a690389285100"
	f, calls := startWechat(t, always(wechatAnswer{status: http.StatusOK, body: `{"prepay_id":"` + prepayID + `"}`}))
	ctx := context.Background()

	prepared, err := f.channel.Prepare(ctx, wxOrder(), Ref{})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	wantCall := `POST /v3/pay/transactions/app {"appid":"` + wxAppID + `","mchid":"` + wxMchID + `","description":"测试充值","out_trade_no":"` + testOutTradeNo +
		`","time_expire":"2026-11-20T20:30:00+08:00","notify_url":"` + wxNotifyURL + `","amount":{"total":600,"currency":"CNY"}}`
	if got := calls.list(); len(got) != 1 || got[0] != wantCall {
		t.Fatalf("prepay calls = %q, want %q", got, wantCall)
	}
	if prepared.Ref.Value != prepayID || !prepared.Ref.At.Equal(testNow) || prepared.PayerID != "" {
		t.Fatalf("Prepare ref = %+v", prepared.Ref)
	}
	params := prepared.Params.Wechat
	if params == nil || params.AppId != wxAppID || params.PartnerId != wxMchID || params.PrepayId != prepayID || params.PackageValue != "Sign=WXPay" ||
		params.Timestamp != strconv.FormatInt(testNow.Unix(), 10) || len(params.NonceStr) != 32 {
		t.Fatalf("app params = %v", params)
	}
	message := wxAppID + "\n" + params.Timestamp + "\n" + params.NonceStr + "\n" + prepayID + "\n"
	if err := verifySHA256(&f.merchant.PublicKey, []byte(message), params.Sign); err != nil {
		t.Fatalf("app params signature does not verify over %q: %v", message, err)
	}

	again, err := f.channel.Prepare(ctx, wxOrder(), Ref{Value: prepayID, At: testNow.Add(-time.Hour)})
	if err != nil || again.Ref.Value != "" || again.Params.Wechat == nil || again.Params.Wechat.PrepayId != prepayID || len(calls.list()) != 1 {
		t.Fatalf("Prepare with a fresh prepay_id: ref=%+v err=%v calls=%d", again.Ref, err, len(calls.list()))
	}
	for name, cached := range map[string]Ref{
		"older than the reuse window": {Value: "stale", At: testNow.Add(-wechatPrepayReuse)},
		"from the future":             {Value: "stale", At: testNow.Add(time.Minute)},
		"without a time":              {Value: "stale"},
	} {
		renewed, err := f.channel.Prepare(ctx, wxOrder(), cached)
		if err != nil || renewed.Ref.Value != prepayID || renewed.Params.Wechat.PrepayId != prepayID {
			t.Fatalf("%s: ref=%+v err=%v", name, renewed.Ref, err)
		}
	}
	if got := len(calls.list()); got != 4 {
		t.Fatalf("prepay was called %d times, want 4: once at first, once per stale reference", got)
	}

	usd := wxOrder()
	usd.Currency = "USD"
	if _, err := f.channel.Prepare(ctx, usd, Ref{}); err == nil || len(calls.list()) != 4 {
		t.Fatalf("Prepare for a USD order: err=%v calls=%d, want an error without calling WeChat", err, len(calls.list()))
	}
}

// 下单失败分三种:渠道说已付(调用方改查单)、渠道明确拒绝(CallError 带错误码)、传输失败(不是 CallError)。
func TestWechatPrepareTellsRejectionsFromTransportFailures(t *testing.T) {
	ctx := context.Background()

	paid, _ := startWechat(t, always(wechatAnswer{status: http.StatusBadRequest, body: `{"code":"ORDERPAID","message":"paid"}`}))
	if _, err := paid.channel.Prepare(ctx, wxOrder(), Ref{}); !errors.Is(err, ErrOrderPaid) {
		t.Fatalf("ORDERPAID: err = %v, want ErrOrderPaid", err)
	}

	for name, c := range map[string]struct {
		body string
		code string
	}{
		"a documented code": {`{"code":"PARAM_ERROR","message":"bad"}`, "PARAM_ERROR"},
		// 错误码只认大写字母、数字、下划线:别的文本不进日志与表。
		"free text in the code field": {`{"code":"<b>please retry</b>","message":"bad"}`, ""},
		"not JSON":                    {`<html>bad gateway</html>`, ""},
	} {
		rejected, _ := startWechat(t, always(wechatAnswer{status: http.StatusBadRequest, body: c.body}))
		_, err := rejected.channel.Prepare(ctx, wxOrder(), Ref{})
		var call *CallError
		if !errors.As(err, &call) || call.Channel != NameWechatPay || call.Op != "prepay" || call.Status != http.StatusBadRequest || call.Code != c.code {
			t.Fatalf("%s: err = %v, want a prepay CallError with code %q", name, err, c.code)
		}
	}

	for name, body := range map[string]string{
		"no prepay_id":           `{}`,
		"an oversized prepay_id": `{"prepay_id":"` + strings.Repeat("x", 65) + `"}`,
	} {
		unusable, _ := startWechat(t, always(wechatAnswer{status: http.StatusOK, body: body}))
		if _, err := unusable.channel.Prepare(ctx, wxOrder(), Ref{}); err == nil {
			t.Fatalf("Prepare accepted an answer with %s", name)
		}
	}

	cause := errors.New("connection reset")
	broken := newWechatFixture(t, "http://wechat.test", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, cause })})
	_, err := broken.channel.Prepare(ctx, wxOrder(), Ref{})
	var call *CallError
	if !errors.Is(err, cause) || errors.As(err, &call) {
		t.Fatalf("transport failure: err = %v, want the transport error and not a CallError", err)
	}
}

// 带正文的成功应答一律验签:签名不对、公钥 ID 不认识、时间戳超出 5 分钟、缺签名头,都不采信。
func TestWechatAnswersMustCarryAValidSignature(t *testing.T) {
	platform := testRSAKey(t, "platform")
	body := wechatTransactionJSON("NOTPAY")
	good := wechatAnswerHeader(t, platform, []byte(body), testNow)
	edit := func(name, value string) http.Header {
		header := good.Clone()
		header.Set(name, value)
		return header
	}
	query := func(t *testing.T, header http.Header) (QueryResult, error) {
		f, _ := startWechat(t, always(wechatAnswer{status: http.StatusOK, body: body, header: header}))
		return f.channel.Query(context.Background(), QueryRequest{OutTradeNo: testOutTradeNo})
	}
	for name, header := range map[string]http.Header{
		"signed by another key":       wechatAnswerHeader(t, testRSAKey(t, "merchant"), []byte(body), testNow),
		"signature over another body": wechatAnswerHeader(t, platform, []byte(body+" "), testNow),
		"unknown key id":              edit("Wechatpay-Serial", "PUB_KEY_ID_OTHER"),
		"more than five minutes old":  wechatAnswerHeader(t, platform, []byte(body), testNow.Add(-wechatClockSkew-time.Second)),
		"more than five minutes away": wechatAnswerHeader(t, platform, []byte(body), testNow.Add(wechatClockSkew+time.Second)),
		"another signature type":      edit("Wechatpay-Signature-Type", "WECHATPAY2-SM2-WITH-SM3"),
		"no signature headers":        {},
	} {
		t.Run(name, func(t *testing.T) {
			if result, err := query(t, header); err == nil {
				t.Fatalf("Query trusted the answer: %+v", result)
			}
		})
	}
	for name, header := range map[string]http.Header{
		"signed now":                      good,
		"signed exactly five minutes ago": wechatAnswerHeader(t, platform, []byte(body), testNow.Add(-wechatClockSkew)),
	} {
		t.Run(name, func(t *testing.T) {
			if result, err := query(t, header); err != nil || result.State != TradeNotPaid {
				t.Fatalf("Query = %+v, %v", result, err)
			}
		})
	}
}

// 查单:各 trade_state 的映射;订单不存在算没付;应答不是这张订单、这个商户的不采信。
func TestWechatQueryMapsTradeStates(t *testing.T) {
	ctx := context.Background()
	wantCall := "GET /v3/pay/transactions/out-trade-no/" + testOutTradeNo + "?mchid=" + wxMchID + " "
	for name, c := range map[string]struct {
		answer  wechatAnswer
		state   TradeState
		payment bool
	}{
		"SUCCESS":       {wechatAnswer{status: http.StatusOK, body: wechatTransactionJSON("SUCCESS")}, TradePaid, true},
		"REFUND":        {wechatAnswer{status: http.StatusOK, body: wechatTransactionJSON("REFUND")}, TradeRefundedOutside, true},
		"NOTPAY":        {wechatAnswer{status: http.StatusOK, body: wechatTransactionJSON("NOTPAY")}, TradeNotPaid, false},
		"USERPAYING":    {wechatAnswer{status: http.StatusOK, body: wechatTransactionJSON("USERPAYING")}, TradeNotPaid, false},
		"CLOSED":        {wechatAnswer{status: http.StatusOK, body: wechatTransactionJSON("CLOSED")}, TradeClosed, false},
		"REVOKED":       {wechatAnswer{status: http.StatusOK, body: wechatTransactionJSON("REVOKED")}, TradeClosed, false},
		"PAYERROR":      {wechatAnswer{status: http.StatusOK, body: wechatTransactionJSON("PAYERROR")}, TradeClosed, false},
		"never ordered": {wechatAnswer{status: http.StatusNotFound, body: `{"code":"ORDER_NOT_EXIST","message":"x"}`}, TradeNotPaid, false},
	} {
		t.Run(name, func(t *testing.T) {
			f, calls := startWechat(t, always(c.answer))
			result, err := f.channel.Query(ctx, QueryRequest{OutTradeNo: testOutTradeNo})
			if err != nil || result.State != c.state || (result.Payment != nil) != c.payment {
				t.Fatalf("Query = %+v, %v", result, err)
			}
			if c.payment && !samePayment(*result.Payment, wxPayment()) {
				t.Fatalf("payment = %+v, want %+v", *result.Payment, wxPayment())
			}
			if got := calls.list(); len(got) != 1 || got[0] != wantCall {
				t.Fatalf("query calls = %q, want %q", got, wantCall)
			}
		})
	}

	paid := wechatTransactionJSON("SUCCESS")
	for name, body := range map[string]string{
		"an unknown trade_state":        wechatTransactionJSON("FROZEN"),
		"another order":                 strings.Replace(paid, testOutTradeNo, "ffffffffffffffffffffffffffffffff", 1),
		"another merchant":              strings.Replace(paid, wxMchID, "1900000002", 1),
		"another app":                   strings.Replace(paid, wxAppID, "wx0000000000000002", 1),
		"a payment without an id":       strings.Replace(paid, wxTransactionID, "", 1),
		"a payment of nothing":          strings.Replace(paid, `"total":600`, `"total":0`, 1),
		"a payment in another currency": strings.Replace(paid, `"currency":"CNY"`, `"currency":"USD"`, 1),
		"an unreadable success_time":    strings.Replace(paid, "2026-11-20T19:59:30+08:00", "2026-11-20 19:59:30", 1),
		"something that is not JSON":    `<html>ok</html>`,
	} {
		t.Run(name, func(t *testing.T) {
			f, _ := startWechat(t, always(wechatAnswer{status: http.StatusOK, body: body}))
			result, err := f.channel.Query(ctx, QueryRequest{OutTradeNo: testOutTradeNo})
			var call *CallError
			if err == nil || errors.As(err, &call) {
				t.Fatalf("Query = %+v, %v; want a plain error", result, err)
			}
		})
	}

	for name, answer := range map[string]wechatAnswer{
		"not found with another code": {status: http.StatusNotFound, body: `{"code":"RESOURCE_NOT_EXISTS","message":"x"}`},
		"server error":                {status: http.StatusInternalServerError, body: `{"code":"SYSTEM_ERROR","message":"x"}`},
		"rate limited":                {status: http.StatusTooManyRequests, body: `{"code":"FREQUENCY_LIMITED","message":"x"}`},
	} {
		t.Run(name, func(t *testing.T) {
			f, _ := startWechat(t, always(answer))
			_, err := f.channel.Query(ctx, QueryRequest{OutTradeNo: testOutTradeNo})
			var call *CallError
			if !errors.As(err, &call) || call.Op != "query" || call.Status != answer.status {
				t.Fatalf("Query: err = %v, want a query CallError with http %d", err, answer.status)
			}
		})
	}
}

// 关单:订单不存在、已经关闭、已付(ORDERPAID,调用方关单后紧接着会查单记账)都算无需重试;其余失败报 CallError。
func TestWechatCloseTreatsMissingAndClosedOrdersAsDone(t *testing.T) {
	wantCall := "POST /v3/pay/transactions/out-trade-no/" + testOutTradeNo + `/close {"mchid":"` + wxMchID + `"}`
	for name, c := range map[string]struct {
		answer wechatAnswer
		done   bool
	}{
		"closed now":     {wechatAnswer{status: http.StatusNoContent}, true},
		"never ordered":  {wechatAnswer{status: http.StatusNotFound, body: `{"code":"ORDER_NOT_EXIST","message":"x"}`}, true},
		"already closed": {wechatAnswer{status: http.StatusBadRequest, body: `{"code":"ORDER_CLOSED","message":"x"}`}, true},
		"already paid":   {wechatAnswer{status: http.StatusBadRequest, body: `{"code":"ORDERPAID","message":"x"}`}, true},
		"server error":   {wechatAnswer{status: http.StatusInternalServerError, body: `{"code":"SYSTEM_ERROR","message":"x"}`}, false},
	} {
		t.Run(name, func(t *testing.T) {
			f, calls := startWechat(t, always(c.answer))
			err := f.channel.Close(context.Background(), testOutTradeNo)
			var call *CallError
			switch {
			case c.done && err != nil:
				t.Fatalf("Close: %v", err)
			case !c.done && (!errors.As(err, &call) || call.Op != "close" || call.Status != c.answer.status):
				t.Fatalf("Close: err = %v, want a close CallError with http %d", err, c.answer.status)
			}
			if got := calls.list(); len(got) != 1 || got[0] != wantCall {
				t.Fatalf("close calls = %q, want %q", got, wantCall)
			}
		})
	}
}

// 退款:先按退款单号查,已有退款就沿用、不再发起;没有才发起。渠道明确失败转人工,限频与系统错误留给重试。
func TestWechatRefundReusesAnExistingRefundAndMapsOutcomes(t *testing.T) {
	notFound := wechatAnswer{status: http.StatusNotFound, body: `{"code":"RESOURCE_NOT_EXISTS","message":"x"}`}
	found := func(status string) wechatAnswer {
		return wechatAnswer{status: http.StatusOK, body: wechatRefundJSON(status)}
	}
	rejected := func(status int, code string) wechatAnswer {
		return wechatAnswer{status: status, body: `{"code":"` + code + `","message":"x"}`}
	}
	queryCall := "GET /v3/refund/domestic/refunds/" + wxOutRefundNo + " "
	createCall := `POST /v3/refund/domestic/refunds {"transaction_id":"` + wxTransactionID + `","out_refund_no":"` + wxOutRefundNo +
		`","reason":"refund","amount":{"refund":600,"total":600,"currency":"CNY"}}`
	for name, c := range map[string]struct {
		queried, created wechatAnswer // 查退款的应答、发起退款的应答(status 为 0 表示不该发起)
		currency         string
		want             RefundResult // State 为 0 表示期望报错
		callOp           string       // 期望的 CallError.Op;want.State 为 0 而它为空表示期望别的错误
	}{
		"already refunded":             {queried: found("SUCCESS"), want: RefundResult{State: RefundSucceeded, RefundID: wxRefundID, At: wxRefundedAt}},
		"still processing":             {queried: found("PROCESSING"), want: RefundResult{State: RefundProcessing, Detail: "PROCESSING"}},
		"stuck on the user's account":  {queried: found("ABNORMAL"), want: RefundResult{State: RefundFailed, Detail: "ABNORMAL"}},
		"closed":                       {queried: found("CLOSED"), want: RefundResult{State: RefundFailed, Detail: "CLOSED"}},
		"accepted":                     {queried: notFound, created: found("PROCESSING"), want: RefundResult{State: RefundProcessing, Detail: "PROCESSING"}},
		"done at once":                 {queried: notFound, created: found("SUCCESS"), want: RefundResult{State: RefundSucceeded, RefundID: wxRefundID, At: wxRefundedAt}},
		"merchant balance too low":     {queried: notFound, created: rejected(http.StatusForbidden, "NOT_ENOUGH"), want: RefundResult{State: RefundProcessing, Detail: "NOT_ENOUGH"}},
		"rejected":                     {queried: notFound, created: rejected(http.StatusBadRequest, "INVALID_REQUEST"), want: RefundResult{State: RefundFailed, Detail: "INVALID_REQUEST"}},
		"system error":                 {queried: notFound, created: rejected(http.StatusInternalServerError, "SYSTEM_ERROR"), callOp: "refund"},
		"rate limited":                 {queried: notFound, created: rejected(http.StatusTooManyRequests, "FREQUENCY_LIMITED"), callOp: "refund"},
		"signature rejected":           {queried: notFound, created: rejected(http.StatusUnauthorized, "SIGN_ERROR"), callOp: "refund"},
		"refund query fails":           {queried: rejected(http.StatusInternalServerError, "SYSTEM_ERROR"), callOp: "refund_query"},
		"a refund of another amount":   {queried: wechatAnswer{status: http.StatusOK, body: strings.Replace(wechatRefundJSON("SUCCESS"), `"refund":600`, `"refund":500`, 1)}},
		"an answer for another refund": {queried: wechatAnswer{status: http.StatusOK, body: strings.Replace(wechatRefundJSON("SUCCESS"), wxOutRefundNo, "rf-other", 1)}},
		"an unknown refund status":     {queried: found("FROZEN")},
		"a payment that is not CNY":    {queried: notFound, currency: "USD", want: RefundResult{State: RefundFailed, Detail: "CURRENCY"}},
	} {
		t.Run(name, func(t *testing.T) {
			f, calls := startWechat(t, func(method, _ string) wechatAnswer {
				if method == http.MethodGet {
					return c.queried
				}
				return c.created
			})
			currency := "CNY"
			if c.currency != "" {
				currency = c.currency
			}
			got, err := f.channel.Refund(context.Background(), RefundRequest{OutTradeNo: testOutTradeNo, TransactionID: wxTransactionID,
				OutRefundNo: wxOutRefundNo, AmountMinor: 600, Currency: currency, Reason: "refund"})
			var call *CallError
			switch {
			case c.want.State != 0:
				if err != nil || got.State != c.want.State || got.RefundID != c.want.RefundID || got.Detail != c.want.Detail || !got.At.Equal(c.want.At) {
					t.Fatalf("Refund = %+v, %v; want %+v", got, err, c.want)
				}
			case c.callOp != "":
				if !errors.As(err, &call) || call.Op != c.callOp {
					t.Fatalf("Refund: err = %v, want a %s CallError", err, c.callOp)
				}
			default:
				if err == nil || errors.As(err, &call) {
					t.Fatalf("Refund = %+v, %v; want a plain error", got, err)
				}
			}
			wantCalls := []string{queryCall}
			if c.created.status != 0 {
				wantCalls = append(wantCalls, createCall)
			}
			if list := calls.list(); strings.Join(list, "\n") != strings.Join(wantCalls, "\n") {
				t.Fatalf("refund calls = %q, want %q", list, wantCalls)
			}
		})
	}
}

// wechatSeal 按微信回调的规则加密资源:AEAD_AES_256_GCM,密钥是 APIv3 密钥,随机串 12 字节,密文后面跟认证标签。
func wechatSeal(t *testing.T, key []byte, nonce, associated, plain string) string {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	return base64.StdEncoding.EncodeToString(aead.Seal(nil, []byte(nonce), []byte(plain), []byte(associated)))
}

func wechatCallback(header http.Header) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/notify/wechat_pay", nil)
	request.Header = header
	return request
}

// notification 造一条微信回调:资源用 APIv3 密钥加密,整段正文用「微信支付」私钥签名。
func (f wechatFixture) notification(t *testing.T, eventType, resource string) (*http.Request, []byte) {
	t.Helper()
	const nonce = "0123456789ab"
	body, err := json.Marshal(map[string]any{
		"id": "EV-2018022511223320873", "create_time": "2026-11-20T20:00:00+08:00", "resource_type": "encrypt-resource",
		"event_type": eventType, "summary": "test",
		"resource": map[string]string{"original_type": "transaction", "algorithm": "AEAD_AES_256_GCM",
			"ciphertext": wechatSeal(t, f.apiKey, nonce, "transaction", resource), "associated_data": "transaction", "nonce": nonce},
	})
	if err != nil {
		t.Fatalf("marshal notification: %v", err)
	}
	return wechatCallback(wechatAnswerHeader(t, f.platform, body, testNow)), body
}

// 付款成功回调:验签、解密、核对是本商户本应用的,映射成付款;正文改一个字节、签名过期、密钥不对都拒绝。
func TestWechatPaymentNotificationIsVerifiedAndDecrypted(t *testing.T) {
	f := newWechatFixture(t, "", nil)
	paid := wechatTransactionJSON("SUCCESS")
	request, body := f.notification(t, "TRANSACTION.SUCCESS", paid)
	note, err := f.channel.ParseNotification(request, body)
	if err != nil || note.Payment == nil || len(note.Revocations) != 0 || len(note.Receipts) != 0 {
		t.Fatalf("notification = %+v, %v", note, err)
	}
	if !samePayment(*note.Payment, wxPayment()) {
		t.Fatalf("payment = %+v, want %+v", *note.Payment, wxPayment())
	}

	if _, err := f.channel.ParseNotification(request, tamper(t, body, "TRANSACTION.SUCCESS", "TRANSACTION.SUCCESs")); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("body changed after signing: err = %v, want ErrBadSignature", err)
	}
	resigned := func(body []byte, key *rsa.PrivateKey, at time.Time) (*http.Request, []byte) {
		return wechatCallback(wechatAnswerHeader(t, key, body, at)), body
	}
	otherKey := f
	otherKey.apiKey = []byte(testSecret(t))
	for name, build := range map[string]func() (*http.Request, []byte){
		"signed more than five minutes ago": func() (*http.Request, []byte) {
			return resigned(body, f.platform, testNow.Add(-wechatClockSkew-time.Second))
		},
		"signed by another key": func() (*http.Request, []byte) { return resigned(body, f.merchant, testNow) },
		"a signature probe": func() (*http.Request, []byte) {
			request, body := resigned(body, f.platform, testNow)
			request.Header.Set("Wechatpay-Signature", "WECHATPAY/SIGNTEST/"+request.Header.Get("Wechatpay-Signature"))
			return request, body
		},
		"a resource sealed with another APIv3 key": func() (*http.Request, []byte) {
			return otherKey.notification(t, "TRANSACTION.SUCCESS", paid)
		},
		"a resource with another cipher": func() (*http.Request, []byte) {
			return resigned(tamper(t, body, "AEAD_AES_256_GCM", "AEAD_AES_128_GCM"), f.platform, testNow)
		},
		"a transaction of another merchant": func() (*http.Request, []byte) {
			return f.notification(t, "TRANSACTION.SUCCESS", strings.Replace(paid, wxMchID, "1900000002", 1))
		},
		"a transaction of another app": func() (*http.Request, []byte) {
			return f.notification(t, "TRANSACTION.SUCCESS", strings.Replace(paid, wxAppID, "wx0000000000000002", 1))
		},
		"a transaction that did not succeed": func() (*http.Request, []byte) {
			return f.notification(t, "TRANSACTION.SUCCESS", wechatTransactionJSON("NOTPAY"))
		},
		"a transaction of nothing": func() (*http.Request, []byte) {
			return f.notification(t, "TRANSACTION.SUCCESS", strings.Replace(paid, `"total":600`, `"total":0`, 1))
		},
		"a body that is not JSON": func() (*http.Request, []byte) { return resigned([]byte("<xml/>"), f.platform, testNow) },
	} {
		request, body := build()
		if _, err := f.channel.ParseNotification(request, body); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("%s: err = %v, want ErrBadSignature", name, err)
		}
	}

	// 别的事件验过签照常应答成功,不产生事实。
	for _, event := range []string{"REFUND.ABNORMAL", "REFUND.CLOSED", "TRANSACTION.OTHER"} {
		request, body := f.notification(t, event, paid)
		if note, err := f.channel.ParseNotification(request, body); err != nil || note.Payment != nil || len(note.Revocations) != 0 {
			t.Fatalf("%s: notification = %+v, %v; want nothing to do", event, note, err)
		}
	}
}

// 退款成功回调翻成一次撤销:交易号是被退那笔付款的微信支付单号,时刻取退款成功时间。
func TestWechatRefundNotificationBecomesARevocation(t *testing.T) {
	f := newWechatFixture(t, "", nil)
	refunded := `{"mchid":"` + wxMchID + `","out_trade_no":"` + testOutTradeNo + `","transaction_id":"` + wxTransactionID + `","out_refund_no":"` + wxOutRefundNo +
		`","refund_id":"` + wxRefundID + `","refund_status":"SUCCESS","success_time":"2026-11-20T19:59:45+08:00","amount":{"total":600,"refund":600,"payer_total":600,"payer_refund":600}}`
	request, body := f.notification(t, "REFUND.SUCCESS", refunded)
	note, err := f.channel.ParseNotification(request, body)
	if err != nil || note.Payment != nil || len(note.Revocations) != 1 {
		t.Fatalf("notification = %+v, %v", note, err)
	}
	if got := note.Revocations[0]; got.TransactionID != wxTransactionID || !got.Revoked || !got.ObservedAt.Equal(wxRefundedAt) || got.Detail != "REFUND.SUCCESS" || got.Payment != nil {
		t.Fatalf("revocation = %+v", got)
	}
	if _, err := f.channel.ParseNotification(request, tamper(t, body, "REFUND.SUCCESS", "REFUND.SUCCESs")); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("body changed after signing: err = %v, want ErrBadSignature", err)
	}

	request, body = f.notification(t, "REFUND.SUCCESS", strings.Replace(refunded, `"refund_status":"SUCCESS"`, `"refund_status":"CLOSED"`, 1))
	if note, err := f.channel.ParseNotification(request, body); err != nil || len(note.Revocations) != 0 {
		t.Fatalf("refund that did not succeed: notification = %+v, %v; want nothing to do", note, err)
	}
	for name, resource := range map[string]string{
		"a refund of another merchant":      strings.Replace(refunded, wxMchID, "1900000002", 1),
		"a refund without the payment's id": strings.Replace(refunded, wxTransactionID, "", 1),
	} {
		request, body := f.notification(t, "REFUND.SUCCESS", resource)
		if _, err := f.channel.ParseNotification(request, body); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("%s: err = %v, want ErrBadSignature", name, err)
		}
	}
}

// 回调应答:成功 204 无正文;失败 4xx / 5xx 带 {"code":"FAIL"},微信按退避重发。
func TestWechatAckShapes(t *testing.T) {
	f := newWechatFixture(t, "", nil)
	if status, body := ackOf(f.channel, AckOK); status != http.StatusNoContent || body != "" {
		t.Fatalf("AckOK = http %d %q", status, body)
	}
	for kind, want := range map[AckKind]int{AckRetry: http.StatusInternalServerError, AckRejected: http.StatusUnauthorized} {
		if status, body := ackOf(f.channel, kind); status != want || !strings.Contains(body, `"code":"FAIL"`) {
			t.Fatalf("ack %d = http %d %q, want http %d with a FAIL body", kind, status, body, want)
		}
	}
}
