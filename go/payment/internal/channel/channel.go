// Package channel 是各支付渠道的协议适配:下单签名、查单、关单、退款、回调验签、商店回执核实。
//
// 只做协议,不碰存储、不碰商城:「钱到没到、有没有被撤销」由本包按渠道规则核实后交给 core,
// 「记不记账、发不发货」是 core 与商城的事。签名、验签、解密只用标准库,协议细节全部在本包可审。
//
// 渠道分两类(docs/design/payment-recharge.md §22.3):
//   - 我们定价、服务端签名下单的:微信支付、支付宝、微信小游戏、抖音小游戏、本地联调。核心靠回调与按商户单号查单。
//   - 商店定价、客户端在商店扣款的:App Store、Google Play。核心靠客户端提交的交易号或令牌,服务端向商店核实(ReceiptChannel)。
package channel

import (
	"bytes"
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	paymentpb "proto/payment"
)

// 渠道在商城回执里的名字(CommerceApi.Payment.channel),也是配置里写的名字。已上线的名字不改:商城按 (渠道名, 交易号) 记账。
const (
	NameDevPay         = "devpay"
	NameWechatPay      = "wechat_pay"
	NameAlipay         = "alipay"
	NameWechatMinigame = "wechat_minigame"
	NameDouyinMinigame = "douyin_minigame"
	NameApple          = "apple"
	NameGoogle         = "google"
)

// NameOf 是渠道枚举对应的名字;未知渠道回空串。
func NameOf(id paymentpb.PayChannel) string {
	switch id {
	case paymentpb.PayChannel_PAY_CHANNEL_DEVPAY:
		return NameDevPay
	case paymentpb.PayChannel_PAY_CHANNEL_WECHAT_PAY:
		return NameWechatPay
	case paymentpb.PayChannel_PAY_CHANNEL_ALIPAY:
		return NameAlipay
	case paymentpb.PayChannel_PAY_CHANNEL_WECHAT_MINIGAME:
		return NameWechatMinigame
	case paymentpb.PayChannel_PAY_CHANNEL_DOUYIN_MINIGAME:
		return NameDouyinMinigame
	case paymentpb.PayChannel_PAY_CHANNEL_APPLE:
		return NameApple
	case paymentpb.PayChannel_PAY_CHANNEL_GOOGLE:
		return NameGoogle
	default:
		return ""
	}
}

// ErrBadSignature:回调验签不过、信封认不出、或者不是发给本商户的。不落任何状态,按渠道约定回失败。
var ErrBadSignature = errors.New("payment channel: notification rejected (signature, envelope or merchant mismatch)")

// ErrOrderPaid:下单时渠道说这笔订单已经付过(微信 ORDERPAID)。调用方应改为查单记账,而不是回给玩家失败。
var ErrOrderPaid = errors.New("payment channel: the order is already paid")

// ErrQueryUnsupported:这个渠道不能按商户单号查单(商店渠道、抖音),到账只靠回调或客户端提交。
var ErrQueryUnsupported = errors.New("payment channel: the channel cannot be queried by out_trade_no")

// ErrLoginRequired:微信小游戏要用当次登录凭证换会话密钥签名,请求没带或凭证已失效。
var ErrLoginRequired = errors.New("payment channel: a fresh mini-game login code is required")

// CallError 是渠道明确回了失败:非 2xx,或 2xx 里带业务错误码。Code 是渠道的错误码,进日志与表。
// 传输失败、超时不是 CallError,原样返回。两种都可以重试;明确不能重试的情形由各方法自己转成结果(如退款 RefundFailed)。
type CallError struct {
	Channel string
	Op      string
	Status  int
	Code    string
}

func (e *CallError) Error() string {
	return fmt.Sprintf("payment channel: %s %s failed (http %d, code %s)", e.Channel, e.Op, e.Status, e.Code)
}

// chinaZone 是渠道约定的时区:支付宝的时间参数、微信 time_expire 都按北京时间写。
var chinaZone = time.FixedZone("CST", 8*3600)

// Order 是要渠道代收的一笔钱。金额、币种来自商城订单的冻结值;商品号由 core 按配置从 SKU 映射。
type Order struct {
	OrderID     string
	OutTradeNo  string
	PlayerID    uint64
	AmountMinor uint64 // 最小货币单位;人民币为分
	Currency    string
	ExpireAt    time.Time // 我们定价的渠道过了这个时刻不再收款
	ProductID   string    // 商店或小游戏后台登记的商品号;不按商品定价的渠道为空
	Title       string    // 商品名,抖音收银台展示
	LoginCode   string    // 小游戏登录凭证
	DeviceOS    string    // android / ios
}

// Ref 是渠道侧的引用(微信 APP 的 prepay_id、微信小游戏的会话密钥),由核心层存进支付意图,同一订单再次调起或查单时原样交回。
type Ref struct {
	Value string
	At    time.Time
}

// ClientParams 是交给客户端调起渠道 SDK 的参数,至多一个有值。
type ClientParams struct {
	Wechat         *paymentpb.WechatAppPayParams
	Alipay         *paymentpb.AlipayAppPayParams
	DevPay         *paymentpb.DevPayParams
	WechatMinigame *paymentpb.WechatMinigamePayParams
	Douyin         *paymentpb.DouyinMinigamePayParams
	Apple          *paymentpb.AppleIapParams
	Google         *paymentpb.GooglePlayParams
}

// Prepared 是一次调起的结果。Ref.Value 非空表示新拿到了引用,调用方要存下来;PayerID 同理。
type Prepared struct {
	Ref     Ref
	PayerID string
	Params  ClientParams
}

// QueryRequest 是一次按商户单号的查单。PayerID、Ref 是建意图时存下的(微信小游戏查单要 openid 与会话密钥)。
type QueryRequest struct {
	OutTradeNo  string
	PayerID     string
	Ref         Ref
	AmountMinor uint64 // 意图上的应付金额:查单应答不带金额的渠道(微信小游戏)按它记
	Currency    string
}

// Payment 是渠道核实过的一笔付款。
type Payment struct {
	TransactionID string // 报给商城的交易号,在本渠道内唯一
	OutTradeNo    string // 我们定价的渠道:回报的商户单号
	OrderID       string // 商店渠道:交易里绑定的订单号(appAccountToken / obfuscatedProfileId);与 OutTradeNo 二选一
	AmountMinor   uint64 // 我们定价的渠道:这笔交易的订单金额(不是扣掉优惠后的用户实付)。商店渠道为 0,由 core 填订单冻结值
	Currency      string
	PaidAt        time.Time
	ProductID     string // 商店与小游戏渠道的商品号;core 核对它与订单的 SKU 一致
	StorePriced   bool   // 商店定价:报给商城的金额取订单冻结值,商店实收另记 StoreAmountMinor
	// StoreAmountMinor / StoreCurrency 是商店实收价(App Store 是 price/10 换成商店币种的分);0 表示渠道不给价格。
	StoreAmountMinor uint64
	StoreCurrency    string
	ChannelToken     string // 收尾与退款要用的渠道令牌(Google purchaseToken)
	ChannelOrderID   string // 渠道侧订单号,对账与退款用
	NeedsFinalize    bool   // 记账之后要做渠道收尾(Google consume)
	Test             bool   // 渠道标明的测试交易(沙箱、许可测试账号)
	// AccountRef 是交易里携带的账号绑定(Google obfuscatedExternalAccountId);core 核对它等于订单买家的派生值。
	AccountRef string
	// Anomaly 非空:渠道确认收了钱,但这笔不能按正常付款发货(例如数量不是 1)。core 记成人工处理,值是原因。
	Anomaly string
}

// TradeState 是查单得到的渠道侧订单状态。
type TradeState int

const (
	TradeNotPaid TradeState = iota + 1 // 还没付;渠道侧还没有这笔订单(没调起过、支付宝还没打开收银台)也算
	TradePaid                          // 已付,QueryResult.Payment 有值
	TradeClosed                        // 渠道侧已关闭,不会再收款
	// TradeRefundedOutside:渠道显示付过,但已经转了退款,而本服务还没记过这笔(退款不是经商城退款待办发起的)。
	// 不能发货,QueryResult.Payment 有值,由 core 记成人工处理。
	TradeRefundedOutside
)

type QueryResult struct {
	State   TradeState
	Payment *Payment
}

// RefundRequest 是退一笔付款的全额。OutRefundNo 由被退那笔付款确定性派生,是渠道退款的幂等键。
type RefundRequest struct {
	OutTradeNo     string
	TransactionID  string
	OutRefundNo    string
	AmountMinor    uint64
	Currency       string
	Reason         string
	ChannelToken   string
	ChannelOrderID string
}

type RefundState int

const (
	RefundProcessing RefundState = iota + 1 // 渠道已受理或暂时退不出(如商户余额不足),稍后再查
	RefundSucceeded                         // 钱已退回
	RefundFailed                            // 渠道明确失败或不支持开发者退款,不再自动重试,人工处理
)

type RefundResult struct {
	State    RefundState
	RefundID string    // 报给商城的退款流水号,在本渠道内唯一
	At       time.Time // 退款完成时刻;渠道没给就是零值
	Detail   string    // 渠道给的原因码,进日志与表,不含个人信息
}

// refundUnsupported 是没有开发者退款接口的渠道统一回的结论:转人工,由运营在渠道后台处理或引导用户向渠道申请。
var refundUnsupported = RefundResult{State: RefundFailed, Detail: "NO_REFUND_API"}

// Revocation 是渠道翻转了一笔付款的事实:用户在渠道侧退款或作废(Revoked=true),或撤销又被撤回(Revoked=false,只有 App Store)。
// ObservedAt 是渠道侧的事件时刻:比已记下的观测更早的不再翻转事实。Payment 在渠道给了完整交易时有值:
// 本服务还没记过这笔就收到撤销,要按「撤销后才记账」记成人工,不能发货。
type Revocation struct {
	TransactionID string
	Revoked       bool
	ObservedAt    time.Time
	Detail        string
	Payment       *Payment
}

// Notification 是一条验过签的回调。Payment 为 nil 且没有撤销:不是要处理的事件,照常应答成功即可。
type Notification struct {
	Payment     *Payment
	Revocations []Revocation
	// Receipts 是要按商店回执重新核实的交易(Google 的购买通知只是线索,要回查接口拿全量状态)。
	Receipts []Receipt
}

// AckKind 是回调应答的三种情形,各渠道写成自己约定的格式。
type AckKind int

const (
	AckOK       AckKind = iota + 1 // 已处理(含已处理过的重复通知)
	AckRetry                       // 本服务这次没处理成,要渠道稍后重发
	AckRejected                    // 验签失败或信封非法;渠道照样会重发,重发也不会过
)

// Channel 是一个支付渠道。实现必须并发安全:多个请求、后台任务同时调用。
type Channel interface {
	ID() paymentpb.PayChannel
	// Name 是渠道在商城回执与配置里的名字。
	Name() string
	// Supports 报告这个渠道收不收这种币种。
	Supports(currency string) bool
	// NeedsProduct 报告调起时要不要商品号(商店渠道、微信小游戏道具直购)。要而配置里没有映射的 SKU 不能用这个渠道付。
	NeedsProduct() bool
	// Prepare 生成调起参数。cached 是上一次存下的引用(可能为空或已过期),渠道自己决定能不能复用。
	Prepare(ctx context.Context, order Order, cached Ref) (Prepared, error)
	// Query 按商户单号查渠道侧订单状态;不支持的渠道回 ErrQueryUnsupported。
	Query(ctx context.Context, req QueryRequest) (QueryResult, error)
	// Close 关闭渠道侧未付的订单。订单不存在、已关闭、渠道没有关单接口都算成功;调用方关单后总要再查一次。
	Close(ctx context.Context, outTradeNo string) error
	// Refund 退一笔付款。先按 OutRefundNo 查,已有退款就沿用结果,不重复发起。没有开发者退款接口的渠道回 RefundFailed。
	Refund(ctx context.Context, req RefundRequest) (RefundResult, error)
	// ParseNotification 验签并解析一条回调。r 只用于读请求头与查询串,body 是已经读出的原始正文。
	ParseNotification(r *http.Request, body []byte) (Notification, error)
	// WriteAck 按渠道约定写回调应答。
	WriteAck(w http.ResponseWriter, kind AckKind)
}

// Receipt 是客户端提交的商店交易:App Store 的交易号,或 Google Play 的购买令牌与商品号。
type Receipt struct {
	TransactionID string
	PurchaseToken string
	ProductID     string
}

// ReceiptChannel 是商店渠道:付款发生在客户端与商店之间,服务端凭交易号或令牌向商店核实。
// 核实结论:TradePaid(Payment 有值)、TradeNotPaid(待处理,例如家长审批中)、TradeClosed(已取消或不该记账)、
// TradeRefundedOutside(核实时已被撤销,Payment 有值)。
type ReceiptChannel interface {
	Verify(ctx context.Context, receipt Receipt) (QueryResult, error)
	// AccountRef 是这个渠道里订单买家的账号绑定派生值,与 Payment.AccountRef 比对;渠道不绑账号回空串。
	AccountRef(player uint64) string
}

// Finalizer 是记账之后还要在渠道侧收尾的渠道(Google consume)。收尾重复做是安全的。
type Finalizer interface {
	Finalize(ctx context.Context, productID, token string) error
}

// HistoryPoller 是能按时间窗补拉事件的渠道(Google 作废交易列表、App Store 通知历史),用来补回调漏掉的付款与撤销。
// since 是上次拉到的时刻;返回这一次拉到的事件与可以推进到的时刻(下一次从它开始)。
type HistoryPoller interface {
	PollHistory(ctx context.Context, since time.Time) ([]Notification, time.Time, error)
}

// Handshaker 是配置回调地址时要先过 GET 握手的渠道(微信消息推送的 echostr)。handled 为真表示已写好应答。
type Handshaker interface {
	Handshake(w http.ResponseWriter, r *http.Request) (handled bool)
}

// ---------------------------------------------------------------------------
// 金额
// ---------------------------------------------------------------------------

// maxYuanFen 是本包接受的金额上限(分),与商城 CommerceStore.MAX_AMOUNT_MINOR 同一量级,防溢出。
const maxYuanFen = 9_000_000_000_000_000

// FormatYuan 把分写成支付宝要的「元,两位小数」。
func FormatYuan(fen uint64) string {
	return strconv.FormatUint(fen/100, 10) + "." + fmt.Sprintf("%02d", fen%100)
}

// ParseYuan 把「元」的十进制文本(至多两位小数)换成分。不经浮点:6.10 元必须正好是 610 分。
func ParseYuan(text string) (uint64, error) {
	whole, frac, hasDot := strings.Cut(text, ".")
	if whole == "" || len(whole) > 16 || !allDigits(whole) || (hasDot && (len(frac) == 0 || len(frac) > 2 || !allDigits(frac))) {
		return 0, fmt.Errorf("payment channel: amount %q is not a yuan amount with at most two decimals", text)
	}
	yuan, err := strconv.ParseUint(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("payment channel: amount %q is out of range", text)
	}
	for len(frac) < 2 {
		frac += "0"
	}
	cents, _ := strconv.ParseUint(frac, 10, 64)
	if yuan > (maxYuanFen-cents)/100 {
		return 0, fmt.Errorf("payment channel: amount %q is out of range", text)
	}
	return yuan*100 + cents, nil
}

func allDigits(text string) bool {
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return false
		}
	}
	return text != ""
}

// ---------------------------------------------------------------------------
// 密钥与签名
// ---------------------------------------------------------------------------

// ParsePrivateKey 读 RSA 私钥:PEM(PKCS#8「PRIVATE KEY」或 PKCS#1「RSA PRIVATE KEY」),或去掉首尾行的裸 base64(先按 PKCS#8 再按 PKCS#1)。
// 微信商户证书私钥 apiclient_key.pem 是 PKCS#8;支付宝密钥工具给的应用私钥两种都有。
func ParsePrivateKey(text string) (*rsa.PrivateKey, error) {
	der, err := keyDER(text, "PRIVATE KEY", "RSA PRIVATE KEY")
	if err != nil {
		return nil, err
	}
	if key, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("payment channel: private key is not RSA")
		}
		return rsaKey, nil
	}
	key, err := x509.ParsePKCS1PrivateKey(der)
	if err != nil {
		return nil, errors.New("payment channel: private key is neither PKCS#8 nor PKCS#1 RSA")
	}
	return key, nil
}

// ParsePublicKey 读 RSA 公钥:PEM(「PUBLIC KEY」或「RSA PUBLIC KEY」),或裸 base64 的 SubjectPublicKeyInfo。
// 微信支付公钥是 PEM;支付宝公钥通常是一行裸 base64。
func ParsePublicKey(text string) (*rsa.PublicKey, error) {
	der, err := keyDER(text, "PUBLIC KEY", "RSA PUBLIC KEY")
	if err != nil {
		return nil, err
	}
	if key, err := x509.ParsePKIXPublicKey(der); err == nil {
		rsaKey, ok := key.(*rsa.PublicKey)
		if !ok {
			return nil, errors.New("payment channel: public key is not RSA")
		}
		return rsaKey, nil
	}
	key, err := x509.ParsePKCS1PublicKey(der)
	if err != nil {
		return nil, errors.New("payment channel: public key is neither SubjectPublicKeyInfo nor PKCS#1 RSA")
	}
	return key, nil
}

// keyDER 取出密钥的 DER:有 PEM 头就按 PEM 读(类型只认给定的几种),否则把整段当 base64(容忍换行与空白)。
// 错误不回显密钥内容。
func keyDER(text string, pemTypes ...string) ([]byte, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, errors.New("payment channel: key is empty")
	}
	if strings.HasPrefix(trimmed, "-----BEGIN ") {
		block, _ := pem.Decode([]byte(trimmed))
		if block == nil {
			return nil, errors.New("payment channel: key PEM cannot be decoded")
		}
		for _, want := range pemTypes {
			if block.Type == want {
				return block.Bytes, nil
			}
		}
		return nil, fmt.Errorf("payment channel: unexpected key PEM type %q", block.Type)
	}
	compact := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, trimmed)
	der, err := base64.StdEncoding.DecodeString(compact)
	if err != nil {
		return nil, errors.New("payment channel: key is neither PEM nor base64")
	}
	return der, nil
}

// signSHA256 是 SHA256withRSA(PKCS#1 v1.5),微信 APIv3、支付宝 RSA2、Google 服务账号 JWT 都用它。
func signSHA256(key *rsa.PrivateKey, message []byte) (string, error) {
	digest := sha256.Sum256(message)
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(signature), nil
}

func verifySHA256(key *rsa.PublicKey, message []byte, signatureBase64 string) error {
	signature, err := base64.StdEncoding.DecodeString(signatureBase64)
	if err != nil {
		return errors.New("payment channel: signature is not base64")
	}
	digest := sha256.Sum256(message)
	return rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature)
}

// hmacHex 是 hex(HMAC-SHA256(key, message)),小写:微信小游戏的 paySig / signature / PayEventSig 都用它。
func hmacHex(key []byte, message string) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}

// randomNonce 是 32 个十六进制字符的随机串:微信的 nonce_str 要求 ≤32 字符。
func randomNonce() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// newHTTPClient 是渠道调用的 HTTP 客户端:不跟随重定向(网关不该重定向,跟了就可能把签过名的请求送到别处);
// 整体时限由调用方的 ctx 给,这里只给建连与读应答头的上限。
func newHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSHandshakeTimeout = 3 * time.Second
	transport.ResponseHeaderTimeout = 6 * time.Second
	transport.MaxIdleConnsPerHost = 16
	return &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// maxResponseBytes 是渠道应答体的上限。查单、退款、作废列表的应答都远小于它。
const maxResponseBytes = 1 << 20

// readBody 读应答正文,超过上限报错。
func readBody(response *http.Response) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxResponseBytes {
		return nil, errors.New("payment channel: answer is too large")
	}
	return raw, nil
}

// ---------------------------------------------------------------------------
// 本地联调渠道
// ---------------------------------------------------------------------------

// DevPaySignatureHeader 是联调回调的签名头:hex(HMAC-SHA256(联调密钥, 原始正文))。
const DevPaySignatureHeader = "X-Devpay-Signature"

// DevPay 是本地联调渠道:没有外部系统,联调工具带签名向回调入口报付款,金额由工具给(可以故意报错金额,走商城的异常付款路径);
// 带 revoke 的通知演练渠道侧撤销。只在服务 Mode=dev/test 时装配(config.Validate)。查单永远是「未付」,关单、退款立即成功。
type DevPay struct {
	secret []byte
	now    func() time.Time
}

func NewDevPay(secret string, now func() time.Time) (*DevPay, error) {
	if len(secret) < 32 {
		return nil, errors.New("payment channel: devpay secret must be at least 32 bytes")
	}
	if now == nil {
		now = time.Now
	}
	return &DevPay{secret: []byte(secret), now: now}, nil
}

func (d *DevPay) ID() paymentpb.PayChannel { return paymentpb.PayChannel_PAY_CHANNEL_DEVPAY }

func (d *DevPay) Name() string { return NameDevPay }

func (d *DevPay) Supports(currency string) bool { return currency != "" }

func (d *DevPay) NeedsProduct() bool { return false }

func (d *DevPay) Close(context.Context, string) error { return nil }

func (d *DevPay) Prepare(_ context.Context, order Order, _ Ref) (Prepared, error) {
	return Prepared{Params: ClientParams{DevPay: &paymentpb.DevPayParams{OutTradeNo: order.OutTradeNo}}}, nil
}

func (d *DevPay) Query(context.Context, QueryRequest) (QueryResult, error) {
	return QueryResult{State: TradeNotPaid}, nil
}

func (d *DevPay) Refund(_ context.Context, req RefundRequest) (RefundResult, error) {
	return RefundResult{State: RefundSucceeded, RefundID: "devpay-" + req.OutRefundNo, At: d.now()}, nil
}

// devPayNotice 是联调回调的正文。seq 非 0 时同一订单报出另一笔付款(交易号不同),用来演练重复付款;
// revoke 为真时报的是这笔付款被渠道撤销(用户在渠道侧退款),演练扣回。
type devPayNotice struct {
	OutTradeNo  string `json:"out_trade_no"`
	AmountMinor uint64 `json:"amount_minor"`
	Currency    string `json:"currency"`
	Seq         uint32 `json:"seq"`
	Revoke      bool   `json:"revoke"`
}

func (d *DevPay) ParseNotification(r *http.Request, body []byte) (Notification, error) {
	got, err := hex.DecodeString(r.Header.Get(DevPaySignatureHeader))
	if err != nil || !hmac.Equal(got, DevPaySign(d.secret, body)) {
		return Notification{}, ErrBadSignature
	}
	var notice devPayNotice
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&notice); err != nil || notice.OutTradeNo == "" || (!notice.Revoke && (notice.AmountMinor == 0 || notice.Currency == "")) {
		return Notification{}, ErrBadSignature
	}
	transactionID := "devpay-" + notice.OutTradeNo
	if notice.Seq > 0 {
		transactionID += "-" + strconv.FormatUint(uint64(notice.Seq), 10)
	}
	if notice.Revoke {
		return Notification{Revocations: []Revocation{{TransactionID: transactionID, Revoked: true, ObservedAt: d.now(), Detail: "devpay"}}}, nil
	}
	return Notification{Payment: &Payment{TransactionID: transactionID, OutTradeNo: notice.OutTradeNo,
		AmountMinor: notice.AmountMinor, Currency: notice.Currency, PaidAt: d.now()}}, nil
}

// DevPaySign 是联调回调的签名,联调工具与用例共用。
func DevPaySign(secret, body []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	return mac.Sum(nil)
}

func (d *DevPay) WriteAck(w http.ResponseWriter, kind AckKind) {
	switch kind {
	case AckOK:
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	case AckRejected:
		w.WriteHeader(http.StatusUnauthorized)
	default:
		w.WriteHeader(http.StatusServiceUnavailable)
	}
}
