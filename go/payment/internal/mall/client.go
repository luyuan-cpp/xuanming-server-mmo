// Package mall 是支付服务以 payment 角色调用商城(java/mall_game)的客户端。
//
// 商城是订单的唯一权威:订单的买家、金额、币种、截止时刻只读它;付款与退款的终局由它记账。
// 签名是商城 v1(HmacAuthentication.sign):HMAC-SHA256(密钥, "v1\nrole\nsubject\nts\nnonce\nMETHOD\n原始URI\nhex(sha256(正文))")。
// 与 go/trade(mall.go commerceSignature)、robot、Java 三边逐字一致,client_test.go 用同一组跨语言黄金样例钉住。
package mall

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// 商城对这些情形回的 HTTP 状态。其余非 200(401 签名或时钟、403 渠道未放行、404、429、5xx、网络错误)都可以重试。
var (
	// ErrInvalid:400,请求参数非法。是本服务的错,重试也不会过。
	ErrInvalid = errors.New("mall: request rejected as invalid (400)")
	// ErrConflict:409,报来的这笔与商城已记下的冲突(交易号已属别的订单、改了金额币种、退款流水已用于别的付款)。商城约定可当终局。
	ErrConflict = errors.New("mall: request conflicts with what the mall has recorded (409)")
	// ErrNotFound:404,订单不存在。
	ErrNotFound = errors.New("mall: order not found (404)")
)

// StatusError 是商城回的其它非 200 状态,可以重试。
type StatusError struct{ Status int }

func (e *StatusError) Error() string { return fmt.Sprintf("mall: request failed with HTTP %d", e.Status) }

// Order 是 /internal/payments/order-view 的应答(CommerceApi.Order)。
type Order struct {
	OrderID           string `json:"orderId"`
	BuyerID           string `json:"buyerId"`
	SkuID             uint64 `json:"skuId"`
	Quantity          uint32 `json:"quantity"`
	AmountMinor       uint64 `json:"amountMinor"`
	Currency          string `json:"currency"`
	State             string `json:"state"`
	PaymentDeadlineMs uint64 `json:"paymentDeadlineMs"`
}

// OrderStateAwaitingPayment 是商城订单「待付款」的状态名(CommerceStore.view)。
const OrderStateAwaitingPayment = "AWAITING_PAYMENT"

// Payment 是 POST /internal/payments 的请求(CommerceApi.Payment):渠道核实过的一笔付款,金额币种是渠道实收值。
type Payment struct {
	OrderID       string `json:"orderId"`
	Channel       string `json:"channel"`
	TransactionID string `json:"transactionId"`
	AmountMinor   uint64 `json:"amountMinor"`
	Currency      string `json:"currency"`
}

// Outcome 是付款回执的结论(CommerceApi.PaymentOutcome)。RefundReason 为空:这笔是订单本身的付款;
// 非空:商城已为这笔钱建好退款待办(重复付款、金额或币种不符、取消后迟到……),值是原因。
type Outcome struct {
	OrderState   string
	RefundReason string
}

// PendingRefund 是 /internal/refunds/poll 的一项(CommerceApi.PendingRefund):要退的一笔付款。
type PendingRefund struct {
	OrderID       string `json:"orderId"`
	Channel       string `json:"channel"`
	TransactionID string `json:"transactionId"`
	AmountMinor   uint64 `json:"amountMinor"`
	Currency      string `json:"currency"`
	Reason        string `json:"reason"`
}

// Refund 是 /internal/refunds/receipt 的请求(CommerceApi.Refund)。PaymentTransactionID 是被退的那笔付款,TransactionID 是渠道的退款流水号。
type Refund struct {
	OrderID              string `json:"orderId"`
	Channel              string `json:"channel"`
	PaymentTransactionID string `json:"paymentTransactionId"`
	TransactionID        string `json:"transactionId"`
	AmountMinor          uint64 `json:"amountMinor"`
	Currency             string `json:"currency"`
}

type Client struct {
	base    string
	subject string
	secret  string
	http    *http.Client
	now     func() time.Time
}

// New 校验商城地址与密钥。地址只能是 scheme://主机[:端口](部署前缀不支持:签名要签商城看到的原始 URI)。
// 密钥至少 32 字节,与商城 CommerceProperties 的要求相同。
func New(base, subject, secret string, timeout time.Duration) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("mall: base URL must be scheme://host[:port] without path, query or user info")
	}
	if len(secret) < 32 {
		return nil, errors.New("mall: payment secret must be at least 32 bytes")
	}
	if subject == "" {
		return nil, errors.New("mall: payment subject is empty")
	}
	if timeout <= 0 {
		timeout = 2500 * time.Millisecond
	}
	return &Client{base: strings.TrimRight(base, "/"), subject: subject, secret: secret, now: time.Now,
		http: &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// Signature 是商城 v1 签名。path 是商城看到的原始 URI,body 是实际发出的字节(GET / 无正文时为空)。
func Signature(secret, role, subject, timestamp, nonce, method, path string, body []byte) string {
	hash := sha256.Sum256(body)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(strings.Join([]string{"v1", role, subject, timestamp, nonce, method, path, hex.EncodeToString(hash[:])}, "\n")))
	return hex.EncodeToString(mac.Sum(nil))
}

func (c *Client) call(ctx context.Context, path string, input, output any) error {
	var body []byte
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = encoded
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	var nonceRaw [16]byte
	if _, err := rand.Read(nonceRaw[:]); err != nil {
		return err
	}
	// 每次调用都换新 nonce:商城 nonce 只能用一次,重试同一业务请求也要新 nonce(商城回 409「签名请求已使用」)。
	nonce := hex.EncodeToString(nonceRaw[:])
	stamp := strconv.FormatInt(c.now().UnixMilli(), 10)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Commerce-Subject", c.subject)
	request.Header.Set("X-Commerce-Timestamp", stamp)
	request.Header.Set("X-Commerce-Nonce", nonce)
	request.Header.Set("X-Commerce-Signature", Signature(c.secret, "payment", c.subject, stamp, nonce, http.MethodPost, path, body))
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 256*1024+1))
	if err != nil {
		return err
	}
	if len(raw) > 256*1024 {
		return errors.New("mall: answer is too large")
	}
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusBadRequest:
		return ErrInvalid
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusConflict:
		return ErrConflict
	default:
		return &StatusError{Status: response.StatusCode}
	}
	if output == nil {
		return nil
	}
	if err := json.Unmarshal(raw, output); err != nil {
		return fmt.Errorf("mall: answer is not readable: %w", err)
	}
	return nil
}

// OrderView 读订单的冻结条款(payment 角色只读接口)。
func (c *Client) OrderView(ctx context.Context, orderID string) (Order, error) {
	var out Order
	if err := c.call(ctx, "/internal/payments/order-view", map[string]string{"orderId": orderID}, &out); err != nil {
		return Order{}, err
	}
	if out.OrderID != orderID {
		return Order{}, errors.New("mall: order view answers another order")
	}
	return out, nil
}

// RecordPayment 把一笔渠道付款记进商城。200 即已落库(重放同一笔也回 200、结论相同);409 是终局冲突;503 等可重试。
func (c *Client) RecordPayment(ctx context.Context, payment Payment) (Outcome, error) {
	var out struct {
		Order *struct {
			OrderID string `json:"orderId"`
			State   string `json:"state"`
		} `json:"order"`
		RefundReason *string `json:"refundReason"`
	}
	if err := c.call(ctx, "/internal/payments", payment, &out); err != nil {
		return Outcome{}, err
	}
	if out.Order == nil || out.Order.OrderID != payment.OrderID || out.Order.State == "" {
		return Outcome{}, errors.New("mall: payment answer lacks the order it was reported for")
	}
	outcome := Outcome{OrderState: out.Order.State}
	if out.RefundReason != nil {
		if *out.RefundReason == "" {
			return Outcome{}, errors.New("mall: payment answer has an empty refund reason")
		}
		outcome.RefundReason = *out.RefundReason
	}
	return outcome, nil
}

// PendingRefunds 列出商城待退款的付款(至多 100 条,商城未做游标)。
func (c *Client) PendingRefunds(ctx context.Context) ([]PendingRefund, error) {
	var out []PendingRefund
	if err := c.call(ctx, "/internal/refunds/poll", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ReportRefund 回执商城:这笔付款已在渠道退款。同一笔付款同一条退款流水重放回 200。
func (c *Client) ReportRefund(ctx context.Context, refund Refund) error {
	return c.call(ctx, "/internal/refunds/receipt", refund, nil)
}
