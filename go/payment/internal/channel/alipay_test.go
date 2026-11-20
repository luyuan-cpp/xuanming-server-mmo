package channel

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	aliAppID       = "2021000000000001"
	aliSellerID    = "2088000000000001"
	aliTradeNo     = "2026112022001400000000000001"
	aliNotifyURL   = "https://pay.example.test/notify/alipay"
	aliOutRefundNo = "rf0123456789abcdef0123456789abcd"
)

// alipayFixture 是一个渠道,外加扮演支付宝要用的密钥。
type alipayFixture struct {
	channel *Alipay
	app     *rsa.PrivateKey // 应用私钥:渠道拿它给请求与订单串签名,用例拿公钥验
	alipay  *rsa.PrivateKey // 「支付宝」的私钥:用例拿它给应答与通知签名,渠道拿公钥验
}

func newAlipayFixture(t *testing.T, gateway string, client *http.Client) alipayFixture {
	t.Helper()
	f := alipayFixture{app: testRSAKey(t, "merchant"), alipay: testRSAKey(t, "platform")}
	ch, err := NewAlipay(AlipayConfig{AppID: aliAppID, AppKey: f.app, AlipayKey: &f.alipay.PublicKey, SellerID: aliSellerID,
		Gateway: gateway, NotifyURL: aliNotifyURL, Subject: "测试充值", HTTP: client, Now: fixedNow})
	if err != nil {
		t.Fatalf("NewAlipay: %v", err)
	}
	f.channel = ch
	return f
}

// alipayTestContent 是用例自己按支付宝的规则拼的待签名串:去掉 exclude 里的键与空值,按键名升序,k=v 用 & 连接,值不编码。
func alipayTestContent(params map[string]string, exclude ...string) string {
	skip := map[string]bool{}
	for _, key := range exclude {
		skip[key] = true
	}
	keys := make([]string, 0, len(params))
	for key, value := range params {
		if !skip[key] && value != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	pairs := make([]string, len(keys))
	for i, key := range keys {
		pairs[i] = key + "=" + params[key]
	}
	return strings.Join(pairs, "&")
}

// alipayForm 把通知参数连同签名编成表单正文。
func alipayForm(params map[string]string, signature string) []byte {
	form := url.Values{"sign": {signature}}
	for name, value := range params {
		form.Set(name, value)
	}
	return []byte(form.Encode())
}

// notice 造一条支付宝异步通知:待签名串去掉 sign 与 sign_type,用「支付宝」私钥签。
func (f alipayFixture) notice(t *testing.T, params map[string]string) []byte {
	t.Helper()
	signature, err := signSHA256(f.alipay, []byte(alipayTestContent(params, "sign", "sign_type")))
	if err != nil {
		t.Fatalf("sign notice: %v", err)
	}
	return alipayForm(params, signature)
}

// alipayPaidParams 是一条付款成功通知的参数。subject 里故意带 &、= 与空格:验签用的是解码后的原文。
func alipayPaidParams() map[string]string {
	return map[string]string{
		"notify_time": "2026-11-20 20:00:00", "notify_type": "trade_status_sync", "notify_id": "ac05099524730693a8b330c5ecf72da978",
		"app_id": aliAppID, "charset": "utf-8", "version": "1.0", "sign_type": "RSA2",
		"trade_no": aliTradeNo, "out_trade_no": testOutTradeNo, "seller_id": aliSellerID, "trade_status": "TRADE_SUCCESS",
		"total_amount": "6.10", "receipt_amount": "6.10", "buyer_pay_amount": "5.60", "subject": "测试充值 & more=1",
		"gmt_create": "2026-11-20 19:59:00", "gmt_payment": "2026-11-20 19:59:30",
	}
}

// aliPayment 是上面那条通知应当映射成的付款:金额取订单金额 total_amount(6.10 元 = 610 分),不是买家实付。
func aliPayment() Payment {
	return Payment{TransactionID: aliTradeNo, OutTradeNo: testOutTradeNo, AmountMinor: 610, Currency: "CNY", PaidAt: testPaidAt}
}

// 待签名串:去掉指定的键与空值,按键名升序,k=v 用 & 连接,值用原文;订单串在签名之后才对值做 URL 编码。
func TestAlipaySignContentSortsAndExcludes(t *testing.T) {
	params := map[string]string{"method": "alipay.trade.query", "app_id": "2021", "sign": "SIG", "sign_type": "RSA2", "empty": "", "biz_content": `{"a":"1 2&3=/"}`}
	if got, want := alipaySignContent(params, "sign"), `app_id=2021&biz_content={"a":"1 2&3=/"}&method=alipay.trade.query&sign_type=RSA2`; got != want {
		t.Fatalf("request sign content = %q, want %q", got, want)
	}
	if got, want := alipaySignContent(params, "sign", "sign_type"), `app_id=2021&biz_content={"a":"1 2&3=/"}&method=alipay.trade.query`; got != want {
		t.Fatalf("notification sign content = %q, want %q", got, want)
	}
	if got, want := alipayEncode(params), `app_id=2021&biz_content=%7B%22a%22%3A%221+2%263%3D%2F%22%7D&method=alipay.trade.query&sign=SIG&sign_type=RSA2`; got != want {
		t.Fatalf("order string = %q, want %q", got, want)
	}
}

func TestNewAlipayRejectsUnusableConfig(t *testing.T) {
	app, alipay := testRSAKey(t, "merchant"), testRSAKey(t, "platform")
	good := AlipayConfig{AppID: aliAppID, AppKey: app, AlipayKey: &alipay.PublicKey, SellerID: aliSellerID, NotifyURL: aliNotifyURL, Subject: "测试充值"}
	if ch, err := NewAlipay(good); err != nil || NameOf(ch.ID()) != NameAlipay || ch.Name() != NameAlipay || !ch.Supports("CNY") || ch.Supports("USD") || ch.NeedsProduct() {
		t.Fatalf("NewAlipay(good): %v", err)
	}
	for name, edit := range map[string]func(*AlipayConfig){
		"an app id that is not numeric":   func(c *AlipayConfig) { c.AppID = "app-1" },
		"no app key":                      func(c *AlipayConfig) { c.AppKey = nil },
		"no Alipay key":                   func(c *AlipayConfig) { c.AlipayKey = nil },
		"a notify URL with a query":       func(c *AlipayConfig) { c.NotifyURL += "?a=1" },
		"a subject with a slash":          func(c *AlipayConfig) { c.Subject = "a/b" },
		"a seller id that is not numeric": func(c *AlipayConfig) { c.SellerID = "seller@example.test" },
	} {
		bad := good
		edit(&bad)
		if _, err := NewAlipay(bad); err == nil {
			t.Fatalf("NewAlipay accepted %s", name)
		}
	}
}

// APP 支付的订单串在本地签出:公共参数齐全、时间按北京时间、金额是两位小数的元;签名覆盖未编码的值。
func TestAlipayPrepareBuildsAVerifiableOrderString(t *testing.T) {
	f := newAlipayFixture(t, "", nil)
	order := Order{OrderID: testOrderID, OutTradeNo: testOutTradeNo, AmountMinor: 610, Currency: "CNY", ExpireAt: testNow.Add(30 * time.Minute)}
	prepared, err := f.channel.Prepare(context.Background(), order, Ref{})
	if err != nil || prepared.Params.Alipay == nil || prepared.Ref.Value != "" || prepared.PayerID != "" {
		t.Fatalf("Prepare: ref=%+v err=%v", prepared.Ref, err)
	}
	orderString := prepared.Params.Alipay.OrderString
	values, err := url.ParseQuery(orderString)
	if err != nil {
		t.Fatalf("order string is not a query string: %v", err)
	}
	params := map[string]string{}
	var names []string
	for _, pair := range strings.Split(orderString, "&") {
		name, _, _ := strings.Cut(pair, "=")
		names = append(names, name)
		params[name] = values.Get(name)
	}
	if !sort.StringsAreSorted(names) || len(names) != len(values) {
		t.Fatalf("order string keys are not unique and ascending: %v", names)
	}
	wantBiz := `{"out_trade_no":"` + testOutTradeNo + `","total_amount":"6.10","subject":"测试充值","product_code":"QUICK_MSECURITY_PAY","time_expire":"2026-11-20 20:30:00"}`
	want := map[string]string{"app_id": aliAppID, "method": "alipay.trade.app.pay", "format": "JSON", "charset": "utf-8", "sign_type": "RSA2",
		"timestamp": "2026-11-20 20:00:00", "version": "1.0", "notify_url": aliNotifyURL, "biz_content": wantBiz, "sign": params["sign"]}
	if len(params) != len(want) {
		t.Fatalf("order string parameters = %v", names)
	}
	for name, value := range want {
		if params[name] != value {
			t.Fatalf("order string %s = %q, want %q", name, params[name], value)
		}
	}
	if err := verifySHA256(&f.app.PublicKey, []byte(alipayTestContent(params, "sign")), params["sign"]); err != nil {
		t.Fatalf("order string signature does not verify: %v", err)
	}

	order.Currency = "USD"
	if _, err := f.channel.Prepare(context.Background(), order, Ref{}); err == nil {
		t.Fatal("Prepare accepted a USD order")
	}
}

// 异步通知:验签(去掉 sign 与 sign_type)、核对是本应用本商户的;TRADE_SUCCESS / TRADE_FINISHED 映射成付款;
// 改一个字节、别人的 app_id、重复的键都拒绝。
func TestAlipayNotificationBecomesAPayment(t *testing.T) {
	f := newAlipayFixture(t, "", nil)
	body := f.notice(t, alipayPaidParams())
	note, err := f.channel.ParseNotification(nil, body)
	if err != nil || note.Payment == nil || len(note.Revocations) != 0 {
		t.Fatalf("notification = %+v, %v", note, err)
	}
	if !samePayment(*note.Payment, aliPayment()) {
		t.Fatalf("payment = %+v, want %+v", *note.Payment, aliPayment())
	}

	with := func(edit func(map[string]string)) []byte {
		params := alipayPaidParams()
		edit(params)
		return f.notice(t, params)
	}
	finished := with(func(p map[string]string) { p["trade_status"] = "TRADE_FINISHED" })
	if note, err := f.channel.ParseNotification(nil, finished); err != nil || note.Payment == nil || !samePayment(*note.Payment, aliPayment()) {
		t.Fatalf("TRADE_FINISHED: notification = %+v, %v", note, err)
	}
	for name, body := range map[string][]byte{
		"WAIT_BUYER_PAY":         with(func(p map[string]string) { p["trade_status"] = "WAIT_BUYER_PAY" }),
		"closed without payment": with(func(p map[string]string) { p["trade_status"] = "TRADE_CLOSED"; delete(p, "gmt_payment") }),
	} {
		if note, err := f.channel.ParseNotification(nil, body); err != nil || note.Payment != nil || len(note.Revocations) != 0 {
			t.Fatalf("%s: notification = %+v, %v; want nothing to do", name, note, err)
		}
	}

	wrongRule := alipayPaidParams()
	overSignType, err := signSHA256(f.alipay, []byte(alipayTestContent(wrongRule, "sign")))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	for name, body := range map[string][]byte{
		"the amount changed after signing":      tamper(t, body, "total_amount=6.10", "total_amount=7.10"),
		"a key sent twice":                      append(append([]byte(nil), body...), "&total_amount=0.01"...),
		"a signature that also covers sign_type": alipayForm(wrongRule, overSignType),
		"signed by another key":                 alipayFixture{alipay: f.app}.notice(t, alipayPaidParams()),
		"no signature":                          alipayForm(alipayPaidParams(), ""),
		"not a form":                            []byte("%zz"),
		"another app":                           with(func(p map[string]string) { p["app_id"] = "2021000000000002" }),
		"another seller":                        with(func(p map[string]string) { p["seller_id"] = "2088000000000002" }),
		"the old RSA sign type":                 with(func(p map[string]string) { p["sign_type"] = "RSA" }),
		"no trade_no":                           with(func(p map[string]string) { delete(p, "trade_no") }),
		"no out_trade_no":                       with(func(p map[string]string) { delete(p, "out_trade_no") }),
		"an amount with three decimals":         with(func(p map[string]string) { p["total_amount"] = "6.100" }),
		"an amount of nothing":                  with(func(p map[string]string) { p["total_amount"] = "0.00" }),
		"an unreadable gmt_payment":             with(func(p map[string]string) { p["gmt_payment"] = "2026/11/20 19:59:30" }),
	} {
		if _, err := f.channel.ParseNotification(nil, body); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("%s: err = %v, want ErrBadSignature", name, err)
		}
	}
}

// 退款引起的通知带 refund_fee:部分退款仍是 TRADE_SUCCESS(付款与撤销都报),全额退款是 TRADE_CLOSED(只报撤销)。
func TestAlipayRefundNotificationBecomesARevocation(t *testing.T) {
	f := newAlipayFixture(t, "", nil)
	refundedAt := testPaidAt.Add(15*time.Second + 320*time.Millisecond) // 北京时间 19:59:45.320

	partial := alipayPaidParams()
	partial["refund_fee"], partial["gmt_refund"] = "2.00", "2026-11-20 19:59:45.320"
	note, err := f.channel.ParseNotification(nil, f.notice(t, partial))
	if err != nil || note.Payment == nil || !samePayment(*note.Payment, aliPayment()) || len(note.Revocations) != 1 {
		t.Fatalf("partial refund: notification = %+v, %v", note, err)
	}
	if got := note.Revocations[0]; got.TransactionID != aliTradeNo || !got.Revoked || !got.ObservedAt.Equal(refundedAt) || got.Detail != "REFUND" || got.Payment != nil {
		t.Fatalf("partial refund: revocation = %+v, want observed at %v", got, refundedAt)
	}

	full := alipayPaidParams()
	full["trade_status"], full["refund_fee"], full["gmt_refund"] = "TRADE_CLOSED", "6.10", "2026-11-20 19:59:45.320"
	note, err = f.channel.ParseNotification(nil, f.notice(t, full))
	if err != nil || note.Payment != nil || len(note.Revocations) != 1 || note.Revocations[0].TransactionID != aliTradeNo || !note.Revocations[0].ObservedAt.Equal(refundedAt) {
		t.Fatalf("full refund: notification = %+v, %v", note, err)
	}

	// 退款时刻读不出来就用当前时刻;refund_fee 是 0 的不算退款。
	undated := alipayPaidParams()
	undated["trade_status"], undated["refund_fee"] = "TRADE_CLOSED", "6.10"
	note, err = f.channel.ParseNotification(nil, f.notice(t, undated))
	if err != nil || len(note.Revocations) != 1 || !note.Revocations[0].ObservedAt.Equal(testNow) {
		t.Fatalf("refund without gmt_refund: notification = %+v, %v", note, err)
	}
	zero := alipayPaidParams()
	zero["refund_fee"] = "0.00"
	note, err = f.channel.ParseNotification(nil, f.notice(t, zero))
	if err != nil || note.Payment == nil || len(note.Revocations) != 0 {
		t.Fatalf("refund_fee of zero: notification = %+v, %v", note, err)
	}
}

// alipayReply 是假网关对一次调用的应答:raw 非空时整段原样回(用来演示坏应答);否则回 {"<应答节点>":node,"sign":对 node 原文的签名}。
type alipayReply struct {
	status int // 0 当 200
	raw    string
	node   string
}

// startAlipay 起一个假网关并把渠道接上去。假网关核对公共参数与请求签名,记下「接口名 业务参数」,再由 answer 按接口名给出应答。
func startAlipay(t *testing.T, answer func(method string) alipayReply) (alipayFixture, *callLog) {
	t.Helper()
	app, alipay := testRSAKey(t, "merchant"), testRSAKey(t, "platform")
	calls := &callLog{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("gateway: unreadable form: %v", err)
		}
		params := map[string]string{}
		for name := range r.PostForm {
			params[name] = r.PostForm.Get(name)
		}
		if r.Method != http.MethodPost || params["app_id"] != aliAppID || params["format"] != "JSON" || params["charset"] != "utf-8" ||
			params["sign_type"] != "RSA2" || params["version"] != "1.0" || params["timestamp"] != "2026-11-20 20:00:00" {
			t.Errorf("gateway: unexpected common parameters in %s request: %v", r.Method, params)
		}
		if err := verifySHA256(&app.PublicKey, []byte(alipayTestContent(params, "sign")), params["sign"]); err != nil {
			t.Errorf("gateway: request signature does not verify: %v", err)
		}
		calls.add(params["method"] + " " + params["biz_content"])
		reply := answer(params["method"])
		if reply.status != 0 {
			w.WriteHeader(reply.status)
		}
		if reply.raw != "" {
			_, _ = io.WriteString(w, reply.raw)
			return
		}
		signature, err := signSHA256(alipay, []byte(reply.node))
		if err != nil {
			t.Errorf("gateway: sign answer: %v", err)
		}
		_, _ = io.WriteString(w, `{"`+strings.ReplaceAll(params["method"], ".", "_")+`_response":`+reply.node+`,"sign":"`+signature+`"}`)
	}))
	t.Cleanup(server.Close)
	return newAlipayFixture(t, server.URL, server.Client()), calls
}

func alipayAlways(reply alipayReply) func(string) alipayReply {
	return func(string) alipayReply { return reply }
}

// alipayTradeNode 是查单应答节点;status 为空串时不带 trade_status,paidAt 为空串时不带打款时刻。
func alipayTradeNode(status, paidAt string) string {
	node := map[string]string{"code": "10000", "msg": "Success", "trade_no": aliTradeNo, "out_trade_no": testOutTradeNo,
		"buyer_logon_id": "159****5620", "total_amount": "6.10", "buyer_pay_amount": "5.60"}
	if status != "" {
		node["trade_status"] = status
	}
	if paidAt != "" {
		node["send_pay_date"] = paidAt
	}
	encoded, _ := json.Marshal(node)
	return string(encoded)
}

// 查单:各 trade_status 的映射;交易不存在算没付;关闭但打过款的是在本服务之外退的款。
func TestAlipayQueryMapsTradeStatus(t *testing.T) {
	ctx := context.Background()
	const paidAt = "2026-11-20 19:59:30"
	wantCall := `alipay.trade.query {"out_trade_no":"` + testOutTradeNo + `"}`
	for name, c := range map[string]struct {
		node    string
		state   TradeState
		payment bool
	}{
		"WAIT_BUYER_PAY":           {alipayTradeNode("WAIT_BUYER_PAY", ""), TradeNotPaid, false},
		"TRADE_SUCCESS":            {alipayTradeNode("TRADE_SUCCESS", paidAt), TradePaid, true},
		"TRADE_FINISHED":           {alipayTradeNode("TRADE_FINISHED", paidAt), TradePaid, true},
		"closed without a payment": {alipayTradeNode("TRADE_CLOSED", ""), TradeClosed, false},
		"closed after a payment":   {alipayTradeNode("TRADE_CLOSED", paidAt), TradeRefundedOutside, true},
		"never opened":             {`{"code":"40004","msg":"Business Failed","sub_code":"ACQ.TRADE_NOT_EXIST","sub_msg":"x"}`, TradeNotPaid, false},
	} {
		t.Run(name, func(t *testing.T) {
			f, calls := startAlipay(t, alipayAlways(alipayReply{node: c.node}))
			result, err := f.channel.Query(ctx, QueryRequest{OutTradeNo: testOutTradeNo})
			if err != nil || result.State != c.state || (result.Payment != nil) != c.payment {
				t.Fatalf("Query = %+v, %v", result, err)
			}
			if c.payment && !samePayment(*result.Payment, aliPayment()) {
				t.Fatalf("payment = %+v, want %+v", *result.Payment, aliPayment())
			}
			if got := calls.list(); len(got) != 1 || got[0] != wantCall {
				t.Fatalf("gateway calls = %q, want %q", got, wantCall)
			}
		})
	}

	alipay := testRSAKey(t, "platform")
	escaped := strings.Replace(alipayTradeNode("WAIT_BUYER_PAY", ""), `"buyer_logon_id":"159****5620"`, `"buyer_logon_id":"a\/b"`, 1)
	overUnescaped, err := signSHA256(alipay, []byte(strings.ReplaceAll(escaped, `\/`, `/`)))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	// 支付宝对 / 转义前的原文签名时,官方 SDK 把 \/ 换成 / 再验一次。
	slashes, _ := startAlipay(t, alipayAlways(alipayReply{raw: `{"alipay_trade_query_response":` + escaped + `,"sign":"` + overUnescaped + `"}`}))
	if result, err := slashes.channel.Query(ctx, QueryRequest{OutTradeNo: testOutTradeNo}); err != nil || result.State != TradeNotPaid {
		t.Fatalf("answer signed over the unescaped text: Query = %+v, %v", result, err)
	}

	paid := alipayTradeNode("TRADE_SUCCESS", paidAt)
	signed, err := signSHA256(alipay, []byte(paid))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	for name, c := range map[string]struct {
		reply  alipayReply
		callOp string // 期望的 CallError.Op;空串表示期望别的错误
	}{
		"an unknown trade_status":  {reply: alipayReply{node: alipayTradeNode("TRADE_FROZEN", paidAt)}},
		"another order":            {reply: alipayReply{node: strings.Replace(paid, testOutTradeNo, "ffffffffffffffffffffffffffffffff", 1)}},
		"a payment without an id":  {reply: alipayReply{node: strings.Replace(paid, aliTradeNo, "", 1)}},
		"a payment of nothing":     {reply: alipayReply{node: strings.Replace(paid, `"total_amount":"6.10"`, `"total_amount":"0.00"`, 1)}},
		"an answer without a sign": {reply: alipayReply{raw: `{"alipay_trade_query_response":` + paid + `}`}},
		"an answer whose sign covers other text": {reply: alipayReply{raw: `{"alipay_trade_query_response":` + strings.Replace(paid, "6.10", "9.10", 1) + `,"sign":"` + signed + `"}`}},
		"an answer that repeats its node": {reply: alipayReply{raw: `{"alipay_trade_query_response":` + strings.Replace(paid, "6.10", "9.10", 1) +
			`,"alipay_trade_query_response":` + paid + `,"sign":"` + signed + `"}`}},
		"an answer without its node": {reply: alipayReply{raw: `{"sign":"` + signed + `"}`}},
		"something that is not JSON": {reply: alipayReply{raw: `<html>ok</html>`}},
		"a gateway error node":       {reply: alipayReply{raw: `{"error_response":{"code":"40002","msg":"Invalid Arguments","sub_code":"isv.invalid-app-id"}}`}, callOp: "alipay.trade.query"},
		"a business failure":         {reply: alipayReply{node: `{"code":"20000","msg":"Service Currently Unavailable","sub_code":"aop.unknow-error"}`}, callOp: "query"},
		"a gateway that is down":     {reply: alipayReply{status: http.StatusBadGateway, raw: `<html>bad gateway</html>`}, callOp: "alipay.trade.query"},
	} {
		t.Run(name, func(t *testing.T) {
			f, _ := startAlipay(t, alipayAlways(c.reply))
			result, err := f.channel.Query(ctx, QueryRequest{OutTradeNo: testOutTradeNo})
			var call *CallError
			switch {
			case c.callOp != "":
				if !errors.As(err, &call) || call.Channel != NameAlipay || call.Op != c.callOp {
					t.Fatalf("Query: err = %v, want a %s CallError", err, c.callOp)
				}
			case err == nil || errors.As(err, &call):
				t.Fatalf("Query = %+v, %v; want a plain error", result, err)
			}
		})
	}

	cause := errors.New("connection reset")
	broken := newAlipayFixture(t, "http://alipay.test/gateway.do", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, cause })})
	_, err = broken.channel.Query(ctx, QueryRequest{OutTradeNo: testOutTradeNo})
	var call *CallError
	if !errors.Is(err, cause) || errors.As(err, &call) {
		t.Fatalf("transport failure: err = %v, want the transport error and not a CallError", err)
	}
}

// 关单:交易不存在、状态不允许关(已关或已付)都不算错,调用方关单后总会再查一次。
func TestAlipayCloseTreatsMissingAndSettledTradesAsDone(t *testing.T) {
	wantCall := `alipay.trade.close {"out_trade_no":"` + testOutTradeNo + `"}`
	for name, c := range map[string]struct {
		node string
		done bool
	}{
		"closed now":       {`{"code":"10000","msg":"Success","trade_no":"` + aliTradeNo + `"}`, true},
		"never opened":     {`{"code":"40004","msg":"Business Failed","sub_code":"ACQ.TRADE_NOT_EXIST"}`, true},
		"already settled":  {`{"code":"40004","msg":"Business Failed","sub_code":"ACQ.TRADE_STATUS_ERROR"}`, true},
		"a system failure": {`{"code":"20000","msg":"Service Currently Unavailable","sub_code":"aop.unknow-error"}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			f, calls := startAlipay(t, alipayAlways(alipayReply{node: c.node}))
			err := f.channel.Close(context.Background(), testOutTradeNo)
			var call *CallError
			switch {
			case c.done && err != nil:
				t.Fatalf("Close: %v", err)
			case !c.done && (!errors.As(err, &call) || call.Op != "close" || call.Code != "20000/aop.unknow-error"):
				t.Fatalf("Close: err = %v, want a close CallError", err)
			}
			if got := calls.list(); len(got) != 1 || got[0] != wantCall {
				t.Fatalf("gateway calls = %q, want %q", got, wantCall)
			}
		})
	}
}

// 退款:先按退款单号查,已退成功就沿用;没有记录才发起。资金变了才算退成,余额不足稍后再试,别的业务失败转人工。
func TestAlipayRefundReusesAnExistingRefundAndMapsOutcomes(t *testing.T) {
	const nothingYet = `{"code":"10000","msg":"Success"}`
	refundedAt := testPaidAt.Add(15 * time.Second) // 北京时间 19:59:45
	queryCall := `alipay.trade.fastpay.refund.query {"out_request_no":"` + aliOutRefundNo + `","out_trade_no":"` + testOutTradeNo + `","query_options":["gmt_refund_pay"]}`
	refundCall := `alipay.trade.refund {"out_request_no":"` + aliOutRefundNo + `","out_trade_no":"` + testOutTradeNo + `","refund_amount":"6.10","refund_reason":"refund"}`
	for name, c := range map[string]struct {
		queried, refunded string       // 查退款的应答节点、发起退款的应答节点(空串表示不该发起)
		currency          string       // 缺省 CNY
		want              RefundResult // State 为 0 表示期望报错
		callOp            string       // 期望的 CallError.Op;want.State 为 0 而它为空表示期望别的错误
	}{
		"already refunded": {
			queried: `{"code":"10000","msg":"Success","out_request_no":"` + aliOutRefundNo + `","refund_status":"REFUND_SUCCESS","refund_amount":"6.10","gmt_refund_pay":"2026-11-20 19:59:45"}`,
			want:    RefundResult{State: RefundSucceeded, RefundID: aliOutRefundNo, At: refundedAt}},
		"an earlier refund of another amount": {
			queried: `{"code":"10000","msg":"Success","refund_status":"REFUND_SUCCESS","refund_amount":"6.00"}`},
		"the refund query fails": {
			queried: `{"code":"20000","msg":"Service Currently Unavailable","sub_code":"aop.unknow-error"}`, callOp: "refund_query"},
		"refunded now": {
			queried: nothingYet, refunded: `{"code":"10000","msg":"Success","fund_change":"Y","gmt_refund_pay":"2026-11-20 19:59:45","refund_fee":"6.10"}`,
			want: RefundResult{State: RefundSucceeded, RefundID: aliOutRefundNo, At: refundedAt}},
		"accepted without a fund change": {
			queried: nothingYet, refunded: `{"code":"10000","msg":"Success","fund_change":"N"}`,
			want: RefundResult{State: RefundProcessing, Detail: "FUND_CHANGE_N"}},
		"merchant balance too low": {
			queried: nothingYet, refunded: `{"code":"40004","msg":"Business Failed","sub_code":"ACQ.SELLER_BALANCE_NOT_ENOUGH"}`,
			want: RefundResult{State: RefundProcessing, Detail: "ACQ.SELLER_BALANCE_NOT_ENOUGH"}},
		"rejected": {
			queried: nothingYet, refunded: `{"code":"40004","msg":"Business Failed","sub_code":"ACQ.TRADE_HAS_FINISHED"}`,
			want: RefundResult{State: RefundFailed, Detail: "40004/ACQ.TRADE_HAS_FINISHED"}},
		"a system failure": {
			queried: nothingYet, refunded: `{"code":"20000","msg":"Service Currently Unavailable","sub_code":"aop.unknow-error"}`, callOp: "refund"},
		"a payment that is not CNY": {
			queried: nothingYet, currency: "USD", want: RefundResult{State: RefundFailed, Detail: "CURRENCY"}},
	} {
		t.Run(name, func(t *testing.T) {
			f, calls := startAlipay(t, func(method string) alipayReply {
				if method == "alipay.trade.fastpay.refund.query" {
					return alipayReply{node: c.queried}
				}
				return alipayReply{node: c.refunded}
			})
			currency := "CNY"
			if c.currency != "" {
				currency = c.currency
			}
			got, err := f.channel.Refund(context.Background(), RefundRequest{OutTradeNo: testOutTradeNo, TransactionID: aliTradeNo,
				OutRefundNo: aliOutRefundNo, AmountMinor: 610, Currency: currency, Reason: "refund"})
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
			if c.refunded != "" {
				wantCalls = append(wantCalls, refundCall)
			}
			if list := calls.list(); strings.Join(list, "\n") != strings.Join(wantCalls, "\n") {
				t.Fatalf("gateway calls = %q, want %q", list, wantCalls)
			}
		})
	}
}

// 回调应答:处理成功回纯文本 success,其余一律 fail。
func TestAlipayAckShapes(t *testing.T) {
	f := newAlipayFixture(t, "", nil)
	for kind, want := range map[AckKind]string{AckOK: "success", AckRetry: "fail", AckRejected: "fail"} {
		if status, body := ackOf(f.channel, kind); status != http.StatusOK || body != want {
			t.Fatalf("ack %d = http %d %q, want http 200 %q", kind, status, body, want)
		}
	}
}
