package channel

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	paymentpb "proto/payment"
)

// 微信支付 APIv3,直连商户,APP 支付。协议事实见 docs/design/payment-recharge.md §22.7(核对自 pay.weixin.qq.com/doc/v3/merchant)。
//
// 只支持「微信支付公钥」验签模式(PUB_KEY_ID_ 开头的公钥 ID,微信对新商户的推荐做法):平台证书模式要定时下载、轮换证书,
// 老商户可以在商户平台切到公钥模式。公钥轮换时要先在配置里换上新公钥 ID 与公钥:验签不过的回调回失败,微信会重发,
// 主动查单照常补账。

const wechatDefaultEndpoint = "https://api.mch.weixin.qq.com"

// wechatPrepayReuse:prepay_id 两小时有效,留 10 分钟余量,过了就重新下单。
const wechatPrepayReuse = 110 * time.Minute

// wechatClockSkew:应答与回调时间戳允许的偏差(微信文档建议 5 分钟)。
const wechatClockSkew = 5 * time.Minute

var wechatPublicKeyID = regexp.MustCompile(`^PUB_KEY_ID_[0-9A-Za-z_]{1,64}$`)

// 微信的错误码只取大写字母、数字、下划线,进日志与表之前先过一遍,不把应答里的任意文本写出去。
var wechatErrorCodePattern = regexp.MustCompile(`^[A-Z0-9_]{1,64}$`)

// 商城回执对退款流水号的要求(CommerceApi.Refund.transactionId)。
var refundIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type WechatPayConfig struct {
	AppID            string // 开放平台移动应用的 AppID,须已与商户号绑定
	MchID            string
	MerchantSerialNo string          // 商户 API 证书序列号
	MerchantKey      *rsa.PrivateKey // 商户 API 证书私钥
	APIv3Key         []byte          // 32 字节,解密回调
	PlatformKeyID    string          // 微信支付公钥 ID(PUB_KEY_ID_...)
	PlatformKey      *rsa.PublicKey  // 微信支付公钥,验应答与回调
	NotifyURL        string          // 付款成功回调地址,https,不带查询串
	Description      string          // 商品描述,≤127 个字符,用户在微信账单里看到
	Endpoint         string          // 缺省 https://api.mch.weixin.qq.com;只有用例与本地模拟才改
	HTTP             *http.Client
	Now              func() time.Time
}

type WechatPay struct{ cfg WechatPayConfig }

func NewWechatPay(cfg WechatPayConfig) (*WechatPay, error) {
	switch {
	case cfg.AppID == "" || cfg.MchID == "" || cfg.MerchantSerialNo == "":
		return nil, errors.New("payment channel: wechat_pay needs AppID, MchID and MerchantSerialNo")
	case cfg.MerchantKey == nil || cfg.PlatformKey == nil:
		return nil, errors.New("payment channel: wechat_pay needs the merchant private key and the WeChat Pay public key")
	case len(cfg.APIv3Key) != 32:
		return nil, errors.New("payment channel: wechat_pay APIv3 key must be exactly 32 bytes")
	case !wechatPublicKeyID.MatchString(cfg.PlatformKeyID):
		return nil, errors.New("payment channel: wechat_pay PlatformPublicKeyID must be a PUB_KEY_ID_... value (only the WeChat Pay public key mode is supported)")
	case cfg.NotifyURL == "" || strings.ContainsAny(cfg.NotifyURL, "?#"):
		return nil, errors.New("payment channel: wechat_pay notify URL is empty or carries a query string")
	case cfg.Description == "" || utf8.RuneCountInString(cfg.Description) > 127:
		return nil, errors.New("payment channel: wechat_pay description must be 1-127 characters")
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = wechatDefaultEndpoint
	}
	cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")
	if cfg.HTTP == nil {
		cfg.HTTP = newHTTPClient()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &WechatPay{cfg: cfg}, nil
}

func (w *WechatPay) ID() paymentpb.PayChannel { return paymentpb.PayChannel_PAY_CHANNEL_WECHAT_PAY }

func (w *WechatPay) Name() string { return NameWechatPay }

func (w *WechatPay) Supports(currency string) bool { return currency == "CNY" }

func (w *WechatPay) NeedsProduct() bool { return false }

func (w *WechatPay) Prepare(ctx context.Context, order Order, cached Ref) (Prepared, error) {
	if !w.Supports(order.Currency) {
		return Prepared{}, fmt.Errorf("payment channel: wechat_pay does not take currency %s", order.Currency)
	}
	now := w.cfg.Now()
	prepayID := cached.Value
	var fresh Ref
	if prepayID == "" || cached.At.IsZero() || now.Before(cached.At) || now.Sub(cached.At) >= wechatPrepayReuse {
		id, err := w.prepay(ctx, order)
		if err != nil {
			return Prepared{}, err
		}
		prepayID, fresh = id, Ref{Value: id, At: now}
	}
	params, err := w.appParams(prepayID, now)
	if err != nil {
		return Prepared{}, err
	}
	return Prepared{Ref: fresh, Params: ClientParams{Wechat: params}}, nil
}

type wechatAmount struct {
	Total    uint64 `json:"total"`
	Currency string `json:"currency"`
}

type wechatPrepayRequest struct {
	AppID       string       `json:"appid"`
	MchID       string       `json:"mchid"`
	Description string       `json:"description"`
	OutTradeNo  string       `json:"out_trade_no"`
	TimeExpire  string       `json:"time_expire"`
	NotifyURL   string       `json:"notify_url"`
	Amount      wechatAmount `json:"amount"`
}

// prepay 调 APP 下单换 prepay_id。同一商户单号、同一组参数重复下单是安全的:参数全部由订单确定性给出(金额、截止时刻、描述、回调地址)。
func (w *WechatPay) prepay(ctx context.Context, order Order) (string, error) {
	body, err := json.Marshal(wechatPrepayRequest{
		AppID: w.cfg.AppID, MchID: w.cfg.MchID, Description: w.cfg.Description, OutTradeNo: order.OutTradeNo,
		TimeExpire: order.ExpireAt.In(chinaZone).Format(time.RFC3339), NotifyURL: w.cfg.NotifyURL,
		Amount: wechatAmount{Total: order.AmountMinor, Currency: "CNY"},
	})
	if err != nil {
		return "", err
	}
	status, raw, err := w.do(ctx, http.MethodPost, "/v3/pay/transactions/app", body)
	if err != nil {
		return "", err
	}
	if status == http.StatusOK {
		var out struct {
			PrepayID string `json:"prepay_id"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || out.PrepayID == "" || len(out.PrepayID) > 64 {
			return "", errors.New("payment channel: wechat_pay prepay answer carries no usable prepay_id")
		}
		return out.PrepayID, nil
	}
	code := wechatErrorCode(raw)
	if code == "ORDERPAID" {
		return "", ErrOrderPaid
	}
	return "", &CallError{Channel: NameWechatPay, Op: "prepay", Status: status, Code: code}
}

// appParams 是 APP 调起支付的参数(PayReq)。package 固定为 Sign=WXPay,不进签名串。
func (w *WechatPay) appParams(prepayID string, now time.Time) (*paymentpb.WechatAppPayParams, error) {
	nonce, err := randomNonce()
	if err != nil {
		return nil, err
	}
	timestamp := strconv.FormatInt(now.Unix(), 10)
	sign, err := signSHA256(w.cfg.MerchantKey, []byte(wechatAppSignMessage(w.cfg.AppID, timestamp, nonce, prepayID)))
	if err != nil {
		return nil, err
	}
	return &paymentpb.WechatAppPayParams{AppId: w.cfg.AppID, PartnerId: w.cfg.MchID, PrepayId: prepayID,
		PackageValue: "Sign=WXPay", NonceStr: nonce, Timestamp: timestamp, Sign: sign}, nil
}

// wechatAppSignMessage:appId、timeStamp(Unix 秒)、nonceStr、prepayId 各占一行,每行以 \n 结尾,最后一行也是。
func wechatAppSignMessage(appID, timestamp, nonce, prepayID string) string {
	return appID + "\n" + timestamp + "\n" + nonce + "\n" + prepayID + "\n"
}

// wechatRequestMessage 是请求签名串:方法、去掉域名的 URL(含查询串)、时间戳(Unix 秒)、随机串、正文,各占一行,
// 每行以 \n 结尾;没有正文时最后一行是空行。
func wechatRequestMessage(method, pathAndQuery, timestamp, nonce string, body []byte) string {
	return method + "\n" + pathAndQuery + "\n" + timestamp + "\n" + nonce + "\n" + string(body) + "\n"
}

// wechatAnswerMessage 是应答与回调的验签串:时间戳、随机串、正文,各占一行,每行以 \n 结尾。
func wechatAnswerMessage(timestamp, nonce string, body []byte) string {
	return timestamp + "\n" + nonce + "\n" + string(body) + "\n"
}

func (w *WechatPay) do(ctx context.Context, method, pathAndQuery string, body []byte) (int, []byte, error) {
	nonce, err := randomNonce()
	if err != nil {
		return 0, nil, err
	}
	timestamp := strconv.FormatInt(w.cfg.Now().Unix(), 10)
	signature, err := signSHA256(w.cfg.MerchantKey, []byte(wechatRequestMessage(method, pathAndQuery, timestamp, nonce, body)))
	if err != nil {
		return 0, nil, err
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, w.cfg.Endpoint+pathAndQuery, reader)
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Authorization", fmt.Sprintf(`WECHATPAY2-SHA256-RSA2048 mchid="%s",nonce_str="%s",signature="%s",timestamp="%s",serial_no="%s"`,
		w.cfg.MchID, nonce, signature, timestamp, w.cfg.MerchantSerialNo))
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("User-Agent", "mmorpg-payment/1")
	request.Header.Set("Wechatpay-Serial", w.cfg.PlatformKeyID)
	response, err := w.cfg.HTTP.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return 0, nil, err
	}
	if len(raw) > maxResponseBytes {
		return 0, nil, errors.New("payment channel: wechat_pay answer is too large")
	}
	// 带正文的成功应答一律验签:查单结果决定发不发货,不只凭 TLS。错误应答里没有业务事实,与官方 SDK 一样不验。
	if response.StatusCode/100 == 2 && len(raw) > 0 {
		if err := w.verify(response.Header, raw); err != nil {
			return 0, nil, err
		}
	}
	return response.StatusCode, raw, nil
}

// verify 核对应答或回调的签名:公钥 ID 必须是配置里的那把,时间戳在允许偏差内。
// 「签名探测」(签名值以 WECHATPAY/SIGNTEST/ 开头)按普通回调验,验不过回失败,正是微信期望的行为。
func (w *WechatPay) verify(header http.Header, body []byte) error {
	if header.Get("Wechatpay-Serial") != w.cfg.PlatformKeyID {
		return errors.New("payment channel: wechat_pay message is signed with an unknown key id")
	}
	if kind := header.Get("Wechatpay-Signature-Type"); kind != "" && kind != "WECHATPAY2-SHA256-RSA2048" {
		return errors.New("payment channel: wechat_pay message uses an unsupported signature type")
	}
	timestamp, nonce, signature := header.Get("Wechatpay-Timestamp"), header.Get("Wechatpay-Nonce"), header.Get("Wechatpay-Signature")
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || nonce == "" || signature == "" {
		return errors.New("payment channel: wechat_pay message lacks signature headers")
	}
	if skew := w.cfg.Now().Sub(time.Unix(seconds, 0)); skew > wechatClockSkew || skew < -wechatClockSkew {
		return errors.New("payment channel: wechat_pay message timestamp is outside the allowed skew")
	}
	if err := verifySHA256(w.cfg.PlatformKey, []byte(wechatAnswerMessage(timestamp, nonce, body)), signature); err != nil {
		return errors.New("payment channel: wechat_pay message signature rejected")
	}
	return nil
}

func wechatErrorCode(raw []byte) string {
	var out struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(raw, &out) != nil || !wechatErrorCodePattern.MatchString(out.Code) {
		return ""
	}
	return out.Code
}

type wechatTransaction struct {
	AppID         string `json:"appid"`
	MchID         string `json:"mchid"`
	OutTradeNo    string `json:"out_trade_no"`
	TransactionID string `json:"transaction_id"`
	TradeState    string `json:"trade_state"`
	SuccessTime   string `json:"success_time"`
	Amount        struct {
		Total    uint64 `json:"total"`
		Currency string `json:"currency"`
	} `json:"amount"`
}

func (w *WechatPay) payment(txn wechatTransaction) (*Payment, error) {
	if txn.TransactionID == "" || txn.Amount.Total == 0 || txn.Amount.Currency != "CNY" {
		return nil, errors.New("payment channel: wechat_pay transaction lacks transaction_id, amount or currency")
	}
	paidAt := w.cfg.Now()
	if txn.SuccessTime != "" {
		parsed, err := time.Parse(time.RFC3339, txn.SuccessTime)
		if err != nil {
			return nil, errors.New("payment channel: wechat_pay success_time is not RFC3339")
		}
		paidAt = parsed
	}
	return &Payment{TransactionID: txn.TransactionID, OutTradeNo: txn.OutTradeNo, AmountMinor: txn.Amount.Total, Currency: "CNY", PaidAt: paidAt}, nil
}

// Query 按商户单号查单。
func (w *WechatPay) Query(ctx context.Context, req QueryRequest) (QueryResult, error) {
	outTradeNo := req.OutTradeNo
	status, raw, err := w.do(ctx, http.MethodGet, "/v3/pay/transactions/out-trade-no/"+url.PathEscape(outTradeNo)+"?mchid="+url.QueryEscape(w.cfg.MchID), nil)
	if err != nil {
		return QueryResult{}, err
	}
	switch {
	case status == http.StatusOK:
	case status == http.StatusNotFound && wechatErrorCode(raw) == "ORDER_NOT_EXIST":
		return QueryResult{State: TradeNotPaid}, nil // 从没下单成功过:没付
	default:
		return QueryResult{}, &CallError{Channel: NameWechatPay, Op: "query", Status: status, Code: wechatErrorCode(raw)}
	}
	var txn wechatTransaction
	if err := json.Unmarshal(raw, &txn); err != nil {
		return QueryResult{}, errors.New("payment channel: wechat_pay query answer is not a transaction")
	}
	if txn.AppID != w.cfg.AppID || txn.MchID != w.cfg.MchID || txn.OutTradeNo != outTradeNo {
		return QueryResult{}, errors.New("payment channel: wechat_pay query answer belongs to another app, merchant or order")
	}
	switch txn.TradeState {
	case "SUCCESS":
		p, err := w.payment(txn)
		return QueryResult{State: TradePaid, Payment: p}, err
	case "REFUND":
		p, err := w.payment(txn)
		return QueryResult{State: TradeRefundedOutside, Payment: p}, err
	case "NOTPAY", "USERPAYING":
		return QueryResult{State: TradeNotPaid}, nil
	case "CLOSED", "REVOKED", "PAYERROR":
		return QueryResult{State: TradeClosed}, nil
	default:
		return QueryResult{}, errors.New("payment channel: wechat_pay query answer has an unknown trade_state")
	}
}

// Close 关闭未付订单。订单不存在也算关好了。已付时微信回错,调用方关单后总会再查一次。
func (w *WechatPay) Close(ctx context.Context, outTradeNo string) error {
	body, err := json.Marshal(map[string]string{"mchid": w.cfg.MchID})
	if err != nil {
		return err
	}
	status, raw, err := w.do(ctx, http.MethodPost, "/v3/pay/transactions/out-trade-no/"+url.PathEscape(outTradeNo)+"/close", body)
	if err != nil {
		return err
	}
	if status == http.StatusNoContent || status == http.StatusOK || wechatErrorCode(raw) == "ORDER_NOT_EXIST" {
		return nil
	}
	return &CallError{Channel: NameWechatPay, Op: "close", Status: status, Code: wechatErrorCode(raw)}
}

type wechatRefundRequest struct {
	TransactionID string `json:"transaction_id"`
	OutRefundNo   string `json:"out_refund_no"`
	Reason        string `json:"reason,omitempty"`
	Amount        struct {
		Refund   uint64 `json:"refund"`
		Total    uint64 `json:"total"`
		Currency string `json:"currency"`
	} `json:"amount"`
}

type wechatRefund struct {
	RefundID    string `json:"refund_id"`
	OutRefundNo string `json:"out_refund_no"`
	Status      string `json:"status"`
	SuccessTime string `json:"success_time"`
	Amount      struct {
		Refund uint64 `json:"refund"`
	} `json:"amount"`
}

// Refund 全额退一笔付款。先按商户退款单号查:已有退款就沿用,不再发起;同一退款单号微信也只退一次。
func (w *WechatPay) Refund(ctx context.Context, req RefundRequest) (RefundResult, error) {
	status, raw, err := w.do(ctx, http.MethodGet, "/v3/refund/domestic/refunds/"+url.PathEscape(req.OutRefundNo), nil)
	if err != nil {
		return RefundResult{}, err
	}
	switch {
	case status == http.StatusOK:
		return w.refundResult(raw, req)
	case status == http.StatusNotFound && wechatErrorCode(raw) == "RESOURCE_NOT_EXISTS":
	default:
		return RefundResult{}, &CallError{Channel: NameWechatPay, Op: "refund_query", Status: status, Code: wechatErrorCode(raw)}
	}
	if req.Currency != "CNY" {
		return RefundResult{State: RefundFailed, Detail: "CURRENCY"}, nil
	}
	var body wechatRefundRequest
	body.TransactionID, body.OutRefundNo, body.Reason = req.TransactionID, req.OutRefundNo, req.Reason
	body.Amount.Refund, body.Amount.Total, body.Amount.Currency = req.AmountMinor, req.AmountMinor, "CNY"
	encoded, err := json.Marshal(body)
	if err != nil {
		return RefundResult{}, err
	}
	status, raw, err = w.do(ctx, http.MethodPost, "/v3/refund/domestic/refunds", encoded)
	if err != nil {
		return RefundResult{}, err
	}
	if status == http.StatusOK {
		return w.refundResult(raw, req)
	}
	code := wechatErrorCode(raw)
	switch {
	case status == http.StatusTooManyRequests || status >= 500 || status == http.StatusUnauthorized:
		// 限频、微信系统错误、签名错误(配置问题,修好就能退):用同一退款单号重试。
		return RefundResult{}, &CallError{Channel: NameWechatPay, Op: "refund", Status: status, Code: code}
	case code == "NOT_ENOUGH":
		return RefundResult{State: RefundProcessing, Detail: code}, nil // 商户余额不足:补足后同一退款单号重试
	default:
		return RefundResult{State: RefundFailed, Detail: code}, nil
	}
}

func (w *WechatPay) refundResult(raw []byte, req RefundRequest) (RefundResult, error) {
	var refund wechatRefund
	if err := json.Unmarshal(raw, &refund); err != nil || refund.OutRefundNo != req.OutRefundNo {
		return RefundResult{}, errors.New("payment channel: wechat_pay refund answer belongs to another refund")
	}
	switch refund.Status {
	case "SUCCESS":
		if !refundIDPattern.MatchString(refund.RefundID) || refund.Amount.Refund != req.AmountMinor {
			return RefundResult{}, errors.New("payment channel: wechat_pay refund answer has an unusable refund_id or a different amount")
		}
		at := time.Time{}
		if refund.SuccessTime != "" {
			parsed, err := time.Parse(time.RFC3339, refund.SuccessTime)
			if err != nil {
				return RefundResult{}, errors.New("payment channel: wechat_pay refund success_time is not RFC3339")
			}
			at = parsed
		}
		return RefundResult{State: RefundSucceeded, RefundID: refund.RefundID, At: at}, nil
	case "PROCESSING":
		return RefundResult{State: RefundProcessing, Detail: "PROCESSING"}, nil
	case "ABNORMAL", "CLOSED":
		// ABNORMAL:退到用户卡上失败(卡已注销等),要在商户平台转退;CLOSED:退款关闭。都要人工。
		return RefundResult{State: RefundFailed, Detail: refund.Status}, nil
	default:
		return RefundResult{}, errors.New("payment channel: wechat_pay refund answer has an unknown status")
	}
}

// ParseNotification 验签、解密付款成功回调。别的事件验过签照常应答成功,不产生事实。
func (w *WechatPay) ParseNotification(r *http.Request, body []byte) (Notification, error) {
	if err := w.verify(r.Header, body); err != nil {
		return Notification{}, ErrBadSignature
	}
	var envelope struct {
		EventType string `json:"event_type"`
		Resource  struct {
			Algorithm      string `json:"algorithm"`
			Ciphertext     string `json:"ciphertext"`
			AssociatedData string `json:"associated_data"`
			Nonce          string `json:"nonce"`
		} `json:"resource"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return Notification{}, ErrBadSignature
	}
	if envelope.EventType != "TRANSACTION.SUCCESS" && envelope.EventType != "REFUND.SUCCESS" {
		return Notification{}, nil // REFUND.ABNORMAL / REFUND.CLOSED 等:钱没有退回,不翻转事实
	}
	if envelope.Resource.Algorithm != "AEAD_AES_256_GCM" {
		return Notification{}, ErrBadSignature
	}
	plain, err := wechatDecrypt(w.cfg.APIv3Key, envelope.Resource.Nonce, envelope.Resource.AssociatedData, envelope.Resource.Ciphertext)
	if err != nil {
		return Notification{}, ErrBadSignature
	}
	if envelope.EventType == "REFUND.SUCCESS" {
		return w.refundNotification(plain)
	}
	var txn wechatTransaction
	if err := json.Unmarshal(plain, &txn); err != nil || txn.AppID != w.cfg.AppID || txn.MchID != w.cfg.MchID || txn.TradeState != "SUCCESS" {
		return Notification{}, ErrBadSignature
	}
	p, err := w.payment(txn)
	if err != nil {
		return Notification{}, ErrBadSignature
	}
	return Notification{Payment: p}, nil
}

// refundNotification 把退款成功通知变成一次撤销。本服务自己发起的退款(商城退款待办)也会收到这条通知:
// core 看这笔付款的退款进度认出它是自己的,不翻转事实;认不出的就是在商户平台等本服务之外退的款,要扣回。
// 部分退款也按整笔撤销处理:一笔付款对应一整份发放,没有按比例扣回的口径。
func (w *WechatPay) refundNotification(plain []byte) (Notification, error) {
	var refund struct {
		MchID         string `json:"mchid"`
		TransactionID string `json:"transaction_id"`
		OutRefundNo   string `json:"out_refund_no"`
		RefundStatus  string `json:"refund_status"`
		SuccessTime   string `json:"success_time"`
	}
	if err := json.Unmarshal(plain, &refund); err != nil || refund.MchID != w.cfg.MchID || refund.TransactionID == "" {
		return Notification{}, ErrBadSignature
	}
	if refund.RefundStatus != "SUCCESS" {
		return Notification{}, nil
	}
	at := w.cfg.Now()
	if refund.SuccessTime != "" {
		if parsed, err := time.Parse(time.RFC3339, refund.SuccessTime); err == nil {
			at = parsed
		}
	}
	return Notification{Revocations: []Revocation{{TransactionID: refund.TransactionID, Revoked: true, ObservedAt: at, Detail: "REFUND.SUCCESS"}}}, nil
}

// wechatDecrypt:AEAD_AES_256_GCM,密钥是 APIv3 密钥,密文是 base64(末尾 16 字节是认证标签),随机串 12 字节。
func wechatDecrypt(key []byte, nonce, associatedData, ciphertext string) ([]byte, error) {
	sealed, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(nonce) != aead.NonceSize() {
		return nil, errors.New("payment channel: wechat_pay resource nonce has the wrong length")
	}
	return aead.Open(nil, []byte(nonce), sealed, []byte(associatedData))
}

// WriteAck:成功回 204 无正文;失败回 4xx/5xx 带 {"code":"FAIL",...},微信按退避重发(约 24 小时内 15 次)。
func (w *WechatPay) WriteAck(rw http.ResponseWriter, kind AckKind) {
	if kind == AckOK {
		rw.WriteHeader(http.StatusNoContent)
		return
	}
	status := http.StatusInternalServerError
	if kind == AckRejected {
		status = http.StatusUnauthorized
	}
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	_, _ = rw.Write([]byte(`{"code":"FAIL","message":"failed"}`))
}
