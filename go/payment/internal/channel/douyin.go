package channel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	paymentpb "proto/payment"
)

// 抖音小游戏支付(tt.requestGamePayment,道具直购 goodType=2,只支持安卓)。规则核对自抖音开放平台
// (developer.open-douyin.com)小游戏虚拟支付与道具直购文档:
//   - 调起参数:goodType=2 时 orderAmount 单位分、customId 为开发者唯一单号、goodName 长度限制 ≤10 个字符。
//   - 回调地址配置握手(GET):Query 带 timestamp / nonce / msg / signature / echostr,按 token / timestamp / nonce / msg
//     字典序排序拼接做 SHA-1 小写十六进制,验过签原样返回 echostr(HTTP 200)。
//   - 支付结果回调(POST):JSON {timestamp, nonce, msg, signature, type};signature 算法同上;
//     msg 是 JSON 串(OrderSuccessPayInfo),含 appid / cp_orderno(即 customId)/ order_no_channel / amount_cent / currency。
//   - 回 HTTP 200 + {"err_no":0,"err_tips":"success"} 即确认,否则按阶梯重试约 4 小时 45 分钟。
//   - 没有沙箱,没有开发者退款接口;到账以回调为准,不做主动查单。

type DouyinMinigameConfig struct {
	AppID     string
	PushToken string // 支付回调的 Token(开发者后台「支付配置」)
	ZoneID    string // 缺省 "1"
	GoodName  string // 收银台展示的商品名,缺省用订单标题
}

type DouyinMinigame struct{ cfg DouyinMinigameConfig }

func NewDouyinMinigame(cfg DouyinMinigameConfig) (*DouyinMinigame, error) {
	if cfg.AppID == "" || cfg.PushToken == "" {
		return nil, errors.New("payment channel: douyin_minigame needs AppID and the payment callback token")
	}
	if cfg.ZoneID == "" {
		cfg.ZoneID = "1"
	}
	return &DouyinMinigame{cfg: cfg}, nil
}

func (d *DouyinMinigame) ID() paymentpb.PayChannel {
	return paymentpb.PayChannel_PAY_CHANNEL_DOUYIN_MINIGAME
}

func (d *DouyinMinigame) Name() string { return NameDouyinMinigame }

func (d *DouyinMinigame) Supports(currency string) bool { return currency == "CNY" }

func (d *DouyinMinigame) NeedsProduct() bool { return false }

func (d *DouyinMinigame) Prepare(_ context.Context, order Order, _ Ref) (Prepared, error) {
	if order.Currency != "CNY" {
		return Prepared{}, errors.New("payment channel: douyin_minigame only takes CNY")
	}
	name := d.cfg.GoodName
	if order.Title != "" {
		name = order.Title
	}
	if name == "" {
		name = "游戏道具"
	} else if utf8.RuneCountInString(name) > 10 {
		name = string([]rune(name)[:10])
	}
	return Prepared{Params: ClientParams{Douyin: &paymentpb.DouyinMinigamePayParams{
		Mode: "game", Env: 0, CurrencyType: "CNY", Platform: "android", BuyQuantity: 1, ZoneId: d.cfg.ZoneID,
		CustomId: order.OutTradeNo, ExtraInfo: "", GoodType: 2, OrderAmount: order.AmountMinor, GoodName: name,
	}}}, nil
}

func (d *DouyinMinigame) Query(context.Context, QueryRequest) (QueryResult, error) {
	return QueryResult{}, ErrQueryUnsupported
}

func (d *DouyinMinigame) Close(context.Context, string) error { return nil }

func (d *DouyinMinigame) Refund(context.Context, RefundRequest) (RefundResult, error) {
	return refundUnsupported, nil
}

// Handshake 是在抖音开放平台「虚拟支付 -> 支付设置」保存回调地址时的 GET 可访问性校验:
// 按 token / timestamp / nonce / msg 字典序排序拼接做 SHA-1,验过签原样回 echostr。
func (d *DouyinMinigame) Handshake(rw http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	q := r.URL.Query()
	echostr := q.Get("echostr")
	if echostr == "" || !equalHex(q.Get("signature"), sortedSHA1(d.cfg.PushToken, q.Get("timestamp"), q.Get("nonce"), q.Get("msg"))) {
		rw.WriteHeader(http.StatusUnauthorized)
		return true
	}
	rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
	rw.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(rw, echostr)
	return true
}

func (d *DouyinMinigame) ParseNotification(_ *http.Request, body []byte) (Notification, error) {
	var envelope struct {
		Timestamp string `json:"timestamp"`
		Nonce     string `json:"nonce"`
		Msg       string `json:"msg"`
		Signature string `json:"signature"`
		Type      string `json:"type"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Timestamp == "" || envelope.Nonce == "" || envelope.Msg == "" ||
		!equalHex(envelope.Signature, sortedSHA1(d.cfg.PushToken, envelope.Timestamp, envelope.Nonce, envelope.Msg)) {
		return Notification{}, ErrBadSignature
	}
	if envelope.Type != "" && envelope.Type != "payment" {
		return Notification{}, nil
	}
	var msg struct {
		AppID          string `json:"appid"`
		CpOrderNo      string `json:"cp_orderno"`
		OrderNoChannel string `json:"order_no_channel"`
		AmountCent     uint64 `json:"amount_cent"`
		Currency       string `json:"currency"`
	}
	if err := json.Unmarshal([]byte(envelope.Msg), &msg); err != nil || msg.AppID != d.cfg.AppID || msg.CpOrderNo == "" || msg.AmountCent == 0 {
		return Notification{}, ErrBadSignature
	}
	currency := msg.Currency
	if currency == "" {
		currency = "CNY"
	}
	paidAt := timeFromUnixText(envelope.Timestamp)
	return Notification{Payment: &Payment{TransactionID: msg.CpOrderNo, OutTradeNo: msg.CpOrderNo, AmountMinor: msg.AmountCent,
		Currency: currency, PaidAt: paidAt, ChannelOrderID: msg.OrderNoChannel}}, nil
}

// WriteAck:回 200 即确认;失败回 500,抖音会重试。
func (d *DouyinMinigame) WriteAck(w http.ResponseWriter, kind AckKind) {
	w.Header().Set("Content-Type", "application/json")
	if kind == AckOK {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"err_no":0,"err_tips":"success"}`))
		return
	}
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write([]byte(`{"err_no":1,"err_tips":"failed"}`))
}

// timeFromUnixText 把秒或毫秒的十进制时间戳变成时间;认不出返回零值(core 用当前时刻)。
func timeFromUnixText(text string) time.Time {
	value, err := strconv.ParseInt(text, 10, 64)
	switch {
	case err != nil || value <= 0:
		return time.Time{}
	case value > 1e12:
		return time.UnixMilli(value)
	default:
		return time.Unix(value, 0)
	}
}
