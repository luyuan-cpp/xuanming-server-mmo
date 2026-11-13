package channel

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	paymentpb "proto/payment"
)

// 支付宝开放平台,公钥模式(RSA2),APP 支付。协议事实见 docs/design/payment-recharge.md §22.7
// (开放平台文档本机访问不到,规则核对自官方 Java SDK alipay-sdk-java-all 的 AlipaySignature / AbstractAlipayClient / 各 Model)。
//
// 证书模式(app_cert_sn / alipay_root_cert_sn)没做:APP 支付用公钥模式即可,证书模式要另管证书链。

const alipayDefaultGateway = "https://openapi.alipay.com/gateway.do"

// alipayTimeLayout:支付宝的时间参数(timestamp、time_expire、gmt_payment……)都按北京时间写成这个格式。
const alipayTimeLayout = "2006-01-02 15:04:05"

type AlipayConfig struct {
	AppID     string
	AppKey    *rsa.PrivateKey // 应用私钥
	AlipayKey *rsa.PublicKey  // 支付宝公钥,验应答与回调
	SellerID  string          // 收款账号的支付宝用户号(2088 开头);填了就核对回调里的 seller_id
	Gateway   string          // 缺省正式网关;沙箱网关只在 Mode=dev/test 允许(config)
	NotifyURL string
	Subject   string // 订单标题,≤256 个字符,不含 / = & 等特殊字符
	HTTP      *http.Client
	Now       func() time.Time
}

type Alipay struct{ cfg AlipayConfig }

func NewAlipay(cfg AlipayConfig) (*Alipay, error) {
	switch {
	case cfg.AppID == "" || !allDigits(cfg.AppID):
		return nil, errors.New("payment channel: alipay AppID must be the numeric app id")
	case cfg.AppKey == nil || cfg.AlipayKey == nil:
		return nil, errors.New("payment channel: alipay needs the app private key and the Alipay public key")
	case cfg.NotifyURL == "" || strings.ContainsAny(cfg.NotifyURL, "?#"):
		return nil, errors.New("payment channel: alipay notify URL is empty or carries a query string")
	case cfg.Subject == "" || utf8.RuneCountInString(cfg.Subject) > 256 || strings.ContainsAny(cfg.Subject, "/=&"):
		return nil, errors.New("payment channel: alipay subject must be 1-256 characters without / = &")
	case cfg.SellerID != "" && !allDigits(cfg.SellerID):
		return nil, errors.New("payment channel: alipay SellerID must be the numeric user id")
	}
	if cfg.Gateway == "" {
		cfg.Gateway = alipayDefaultGateway
	}
	if cfg.HTTP == nil {
		cfg.HTTP = newHTTPClient()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Alipay{cfg: cfg}, nil
}

func (a *Alipay) ID() paymentpb.PayChannel { return paymentpb.PayChannel_PAY_CHANNEL_ALIPAY }

func (a *Alipay) Name() string { return NameAlipay }

func (a *Alipay) Supports(currency string) bool { return currency == "CNY" }

// alipaySignContent 是签名与验签的原文:去掉 exclude 里的键与值为空的项,按键名升序,拼成 k=v&k=v。值用原文,不编码。
// 请求签名时 exclude 只有 sign;付款回调验签时去掉 sign 与 sign_type(AlipaySignature.getSignCheckContentV1)。
func alipaySignContent(params map[string]string, exclude ...string) string {
	keys := make([]string, 0, len(params))
	for key, value := range params {
		if key == "" || value == "" || contains(exclude, key) {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, key := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(params[key])
	}
	return b.String()
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (a *Alipay) common(method string, biz []byte) map[string]string {
	return map[string]string{
		"app_id": a.cfg.AppID, "method": method, "format": "JSON", "charset": "utf-8", "sign_type": "RSA2",
		"timestamp": a.cfg.Now().In(chinaZone).Format(alipayTimeLayout), "version": "1.0", "biz_content": string(biz),
	}
}

func (a *Alipay) sign(params map[string]string) error {
	signature, err := signSHA256(a.cfg.AppKey, []byte(alipaySignContent(params, "sign")))
	if err != nil {
		return err
	}
	params["sign"] = signature
	return nil
}

// alipayEncode 把签好的参数拼成订单串:按键名升序,值做 URL 编码(签名之后才编码;签名原文用的是未编码的值)。
func alipayEncode(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for key, value := range params {
		if key != "" && value != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+url.QueryEscape(params[key]))
	}
	return strings.Join(parts, "&")
}

type alipayAppPayBiz struct {
	OutTradeNo  string `json:"out_trade_no"`
	TotalAmount string `json:"total_amount"`
	Subject     string `json:"subject"`
	ProductCode string `json:"product_code"`
	TimeExpire  string `json:"time_expire"`
}

// Prepare 在本地签出 APP 支付的订单串,不调支付宝:交易在用户打开收银台时才在支付宝侧创建。每次都重新签,不需要缓存引用。
func (a *Alipay) Prepare(_ context.Context, order Order, _ Ref) (Prepared, error) {
	if !a.Supports(order.Currency) {
		return Prepared{}, errors.New("payment channel: alipay only takes CNY")
	}
	biz, err := json.Marshal(alipayAppPayBiz{OutTradeNo: order.OutTradeNo, TotalAmount: FormatYuan(order.AmountMinor), Subject: a.cfg.Subject,
		ProductCode: "QUICK_MSECURITY_PAY", TimeExpire: order.ExpireAt.In(chinaZone).Format(alipayTimeLayout)})
	if err != nil {
		return Prepared{}, err
	}
	params := a.common("alipay.trade.app.pay", biz)
	params["notify_url"] = a.cfg.NotifyURL
	if err := a.sign(params); err != nil {
		return Prepared{}, err
	}
	return Prepared{Params: ClientParams{Alipay: &paymentpb.AlipayAppPayParams{OrderString: alipayEncode(params)}}}, nil
}

type alipayStatus struct {
	Code    string `json:"code"`
	SubCode string `json:"sub_code"`
}

func (s alipayStatus) code() string { return s.Code + "/" + s.SubCode }

// call 调一个开放平台接口,返回已验签的应答节点原文(<method 把点换成下划线>_response 的值)。
func (a *Alipay) call(ctx context.Context, method string, biz any) ([]byte, error) {
	encoded, err := json.Marshal(biz)
	if err != nil {
		return nil, err
	}
	params := a.common(method, encoded)
	if err := a.sign(params); err != nil {
		return nil, err
	}
	form := url.Values{}
	for key, value := range params {
		form.Set(key, value)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.Gateway, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")
	response, err := a.cfg.HTTP.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxResponseBytes {
		return nil, errors.New("payment channel: alipay answer is too large")
	}
	if response.StatusCode != http.StatusOK {
		return nil, &CallError{Channel: NameAlipay, Op: method, Status: response.StatusCode}
	}
	return a.verifyAnswer(method, raw)
}

// verifyAnswer 取出应答节点的原文并验签。签名覆盖的是节点值的原始字节(从 { 到配对的 }),不重新序列化、不排序;
// json.RawMessage 拿到的正是这段原始字节。原文含 \/ 而验不过时,按官方 SDK 的做法换成 / 再验一次。
// 应答必须带签名:业务失败的应答支付宝同样签名,没签名的一律当不可信。
func (a *Alipay) verifyAnswer(method string, raw []byte) ([]byte, error) {
	node := strings.ReplaceAll(method, ".", "_") + "_response"
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, errors.New("payment channel: alipay answer is not JSON")
	}
	content, ok := envelope[node]
	if !ok {
		var status alipayStatus
		if errorNode, found := envelope["error_response"]; found && json.Unmarshal(errorNode, &status) == nil {
			return nil, &CallError{Channel: NameAlipay, Op: method, Status: http.StatusOK, Code: status.code()}
		}
		return nil, errors.New("payment channel: alipay answer has no response node")
	}
	if bytes.Count(raw, []byte(`"`+node+`"`)) != 1 {
		return nil, errors.New("payment channel: alipay answer repeats its response node")
	}
	var signature string
	if err := json.Unmarshal(envelope["sign"], &signature); err != nil || signature == "" {
		return nil, errors.New("payment channel: alipay answer is not signed")
	}
	if verifySHA256(a.cfg.AlipayKey, content, signature) == nil {
		return content, nil
	}
	if bytes.Contains(content, []byte(`\/`)) && verifySHA256(a.cfg.AlipayKey, bytes.ReplaceAll(content, []byte(`\/`), []byte(`/`)), signature) == nil {
		return content, nil
	}
	return nil, errors.New("payment channel: alipay answer signature rejected")
}

// payment 由支付宝给的字段构造一笔付款。金额是 total_amount(订单金额,元,两位小数)。
func (a *Alipay) payment(tradeNo, outTradeNo, totalAmount, paidAt string) (*Payment, error) {
	if tradeNo == "" || outTradeNo == "" {
		return nil, errors.New("payment channel: alipay trade lacks trade_no or out_trade_no")
	}
	amount, err := ParseYuan(totalAmount)
	if err != nil || amount == 0 {
		return nil, errors.New("payment channel: alipay trade amount is not usable")
	}
	at := a.cfg.Now()
	if paidAt != "" {
		parsed, err := time.ParseInLocation(alipayTimeLayout, paidAt, chinaZone)
		if err != nil {
			return nil, errors.New("payment channel: alipay payment time is not in the expected format")
		}
		at = parsed
	}
	return &Payment{TransactionID: tradeNo, OutTradeNo: outTradeNo, AmountMinor: amount, Currency: "CNY", PaidAt: at}, nil
}

type alipayQueryAnswer struct {
	alipayStatus
	TradeNo     string `json:"trade_no"`
	OutTradeNo  string `json:"out_trade_no"`
	TradeStatus string `json:"trade_status"`
	TotalAmount string `json:"total_amount"`
	SendPayDate string `json:"send_pay_date"`
}

// Query 按商户单号查交易(alipay.trade.query)。交易还不存在(用户没打开过收银台)算没付。
func (a *Alipay) Query(ctx context.Context, outTradeNo string) (QueryResult, error) {
	content, err := a.call(ctx, "alipay.trade.query", map[string]string{"out_trade_no": outTradeNo})
	if err != nil {
		return QueryResult{}, err
	}
	var answer alipayQueryAnswer
	if err := json.Unmarshal(content, &answer); err != nil {
		return QueryResult{}, errors.New("payment channel: alipay query answer is not readable")
	}
	switch {
	case answer.Code == "10000":
	case answer.Code == "40004" && answer.SubCode == "ACQ.TRADE_NOT_EXIST":
		return QueryResult{State: TradeNotPaid}, nil
	default:
		return QueryResult{}, &CallError{Channel: NameAlipay, Op: "query", Status: http.StatusOK, Code: answer.code()}
	}
	if answer.OutTradeNo != outTradeNo {
		return QueryResult{}, errors.New("payment channel: alipay query answer belongs to another order")
	}
	switch answer.TradeStatus {
	case "WAIT_BUYER_PAY":
		return QueryResult{State: TradeNotPaid}, nil
	case "TRADE_SUCCESS", "TRADE_FINISHED":
		p, err := a.payment(answer.TradeNo, answer.OutTradeNo, answer.TotalAmount, answer.SendPayDate)
		return QueryResult{State: TradePaid, Payment: p}, err
	case "TRADE_CLOSED":
		// TRADE_CLOSED 既可能是未付超时关闭,也可能是付过之后全额退了款。有打款时刻说明付过:那是在本服务之外退的款。
		if answer.SendPayDate == "" {
			return QueryResult{State: TradeClosed}, nil
		}
		p, err := a.payment(answer.TradeNo, answer.OutTradeNo, answer.TotalAmount, answer.SendPayDate)
		return QueryResult{State: TradeRefundedOutside, Payment: p}, err
	default:
		return QueryResult{}, errors.New("payment channel: alipay query answer has an unknown trade_status")
	}
}

// Close 关闭未付交易(alipay.trade.close)。交易不存在、状态不允许关(已关或已付)都不算错:调用方关单后总会再查一次。
func (a *Alipay) Close(ctx context.Context, outTradeNo string) error {
	content, err := a.call(ctx, "alipay.trade.close", map[string]string{"out_trade_no": outTradeNo})
	if err != nil {
		return err
	}
	var answer alipayStatus
	if err := json.Unmarshal(content, &answer); err != nil {
		return errors.New("payment channel: alipay close answer is not readable")
	}
	if answer.Code == "10000" || (answer.Code == "40004" && (answer.SubCode == "ACQ.TRADE_NOT_EXIST" || answer.SubCode == "ACQ.TRADE_STATUS_ERROR")) {
		return nil
	}
	return &CallError{Channel: NameAlipay, Op: "close", Status: http.StatusOK, Code: answer.code()}
}

type alipayRefundQueryAnswer struct {
	alipayStatus
	OutRequestNo string `json:"out_request_no"`
	RefundStatus string `json:"refund_status"`
	RefundAmount string `json:"refund_amount"`
	GmtRefundPay string `json:"gmt_refund_pay"`
}

type alipayRefundAnswer struct {
	alipayStatus
	FundChange   string `json:"fund_change"`
	GmtRefundPay string `json:"gmt_refund_pay"`
}

// Refund 全额退一笔付款。out_request_no 用本服务派生的退款单号:同一交易下它唯一,重试必须不变。
// 先按它查(alipay.trade.fastpay.refund.query):已退成功就沿用;没有记录再发起 alipay.trade.refund。
// 支付宝的退款没有单独的流水号,报给商城的退款流水号就是这个退款单号。
func (a *Alipay) Refund(ctx context.Context, req RefundRequest) (RefundResult, error) {
	content, err := a.call(ctx, "alipay.trade.fastpay.refund.query", map[string]any{
		"out_trade_no": req.OutTradeNo, "out_request_no": req.OutRefundNo, "query_options": []string{"gmt_refund_pay"},
	})
	if err != nil {
		return RefundResult{}, err
	}
	var queried alipayRefundQueryAnswer
	if err := json.Unmarshal(content, &queried); err != nil {
		return RefundResult{}, errors.New("payment channel: alipay refund query answer is not readable")
	}
	if queried.Code != "10000" {
		return RefundResult{}, &CallError{Channel: NameAlipay, Op: "refund_query", Status: http.StatusOK, Code: queried.code()}
	}
	if queried.RefundStatus == "REFUND_SUCCESS" {
		refunded, err := ParseYuan(queried.RefundAmount)
		if err != nil || refunded != req.AmountMinor {
			return RefundResult{}, errors.New("payment channel: alipay refund query reports a different amount")
		}
		return RefundResult{State: RefundSucceeded, RefundID: req.OutRefundNo, At: a.refundTime(queried.GmtRefundPay)}, nil
	}
	// 没有 refund_status:这个退款单号还没退成(没收到,或上次失败了),发起退款。
	if req.Currency != "CNY" {
		return RefundResult{State: RefundFailed, Detail: "CURRENCY"}, nil
	}
	content, err = a.call(ctx, "alipay.trade.refund", map[string]string{
		"out_trade_no": req.OutTradeNo, "refund_amount": FormatYuan(req.AmountMinor), "out_request_no": req.OutRefundNo, "refund_reason": req.Reason,
	})
	if err != nil {
		return RefundResult{}, err
	}
	var answer alipayRefundAnswer
	if err := json.Unmarshal(content, &answer); err != nil {
		return RefundResult{}, errors.New("payment channel: alipay refund answer is not readable")
	}
	switch {
	case answer.Code == "10000" && answer.FundChange == "Y":
		return RefundResult{State: RefundSucceeded, RefundID: req.OutRefundNo, At: a.refundTime(answer.GmtRefundPay)}, nil
	case answer.Code == "10000":
		// 资金没有变化:重复请求或仍在处理。下一轮按退款单号再查,查到 REFUND_SUCCESS 才算退成。
		return RefundResult{State: RefundProcessing, Detail: "FUND_CHANGE_N"}, nil
	case answer.Code == "20000" || answer.SubCode == "ACQ.SYSTEM_ERROR":
		return RefundResult{}, &CallError{Channel: NameAlipay, Op: "refund", Status: http.StatusOK, Code: answer.code()}
	case answer.SubCode == "ACQ.SELLER_BALANCE_NOT_ENOUGH":
		return RefundResult{State: RefundProcessing, Detail: answer.SubCode}, nil // 商户余额不足:补足后同一退款单号重试
	default:
		return RefundResult{State: RefundFailed, Detail: answer.code()}, nil
	}
}

func (a *Alipay) refundTime(text string) time.Time {
	if text == "" {
		return time.Time{}
	}
	parsed, err := time.ParseInLocation(alipayTimeLayout, text, chinaZone)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

// ParseNotification 验签并解析异步通知(POST 表单)。
// 验签原文:去掉 sign、sign_type,值用解码后的原文,按键名升序拼接;再核对 app_id 与(配了的话)seller_id 是本商户的。
// 只有 TRADE_SUCCESS / TRADE_FINISHED 产生付款事实;等待付款、关闭、退款引起的通知验过签照常应答 success。
func (a *Alipay) ParseNotification(_ *http.Request, body []byte) (Notification, error) {
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return Notification{}, ErrBadSignature
	}
	params := make(map[string]string, len(values))
	for key, value := range values {
		if len(value) != 1 {
			return Notification{}, ErrBadSignature
		}
		params[key] = value[0]
	}
	if params["sign_type"] != "RSA2" || params["sign"] == "" {
		return Notification{}, ErrBadSignature
	}
	if err := verifySHA256(a.cfg.AlipayKey, []byte(alipaySignContent(params, "sign", "sign_type")), params["sign"]); err != nil {
		return Notification{}, ErrBadSignature
	}
	if params["app_id"] != a.cfg.AppID || (a.cfg.SellerID != "" && params["seller_id"] != a.cfg.SellerID) {
		return Notification{}, ErrBadSignature
	}
	switch params["trade_status"] {
	case "TRADE_SUCCESS", "TRADE_FINISHED":
	default:
		return Notification{}, nil
	}
	p, err := a.payment(params["trade_no"], params["out_trade_no"], params["total_amount"], params["gmt_payment"])
	if err != nil {
		return Notification{}, ErrBadSignature
	}
	return Notification{Payment: p}, nil
}

// WriteAck:处理成功回纯文本 success,其余一律 fail;支付宝收不到 success 会在 25 小时内按退避重发。
func (a *Alipay) WriteAck(w http.ResponseWriter, kind AckKind) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if kind == AckOK {
		_, _ = w.Write([]byte("success"))
		return
	}
	_, _ = w.Write([]byte("fail"))
}
