package channel

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	paymentpb "proto/payment"
)

// 微信小游戏虚拟支付 2.0,道具直购(wx.requestMidasPaymentGameItem,mode=goods)。规则核对自 developers.weixin.qq.com:
//   - signData 是 JSON 串:mode / offerId / buyQuantity / env / currencyType / platform / zoneId / productId / goodsPrice(分)/ outTradeNo / attach;
//     客户端必须逐字节原样传(paySig 按它算)。outTradeNo ≤32 字符、不以下划线开头、只能用一次。
//   - paySig = hex(HMAC-SHA256(AppKey, "requestMidasPaymentGameItem&" + signData));
//     signature = hex(HMAC-SHA256(session_key, signData)),session_key 由 jscode2session 用当次 wx.login 的 code 换来。
//   - 服务端接口 pay_sig = hex(HMAC-SHA256(AppKey, uri + "&" + 正文)),signature = hex(HMAC-SHA256(session_key, 正文)),
//     查单 POST /wxa/game/queryorderinfo(biz_id=2 道具直购;pay_state 2 = 已支付;errcode 90019 = 单号不存在)。
//   - 发货推送 minigame_game_pay_goods_deliver_notify、退款推送 minigame_pay_refund_succ_notify:
//     消息推送的 URL 签名(sha1(sort(Token, timestamp, nonce)),安全模式是 msg_signature 并带 Encrypt),
//     再验 PayEventSig = hex(HMAC-SHA256(AppKey, Event + "&" + Payload));IsMock 为真的是控制台的模拟推送,只回成功。
//     应答 {"ErrCode":0,"ErrMsg":"Success"},3 秒内;未成功按退避重推约 24 小时。
//   - 没有开发者退款接口:要退的钱转人工(在 MP 后台处理),后台退款同样经退款推送翻转事实。
//
// 报给商城的交易号是 OutTradeNo(微信保证一个商户单号只能付一次);金额取发货推送的 OrigPrice(原价),ActualPrice 只是用户实付。

const wechatMiniAPIBase = "https://api.weixin.qq.com"

type WechatMinigameConfig struct {
	AppID          string
	AppSecret      string // 小游戏 AppSecret:换 session_key 与 access_token
	OfferID        string // 虚拟支付 2.0 的支付应用 ID
	AppKey         string // 与 Env 对应的 AppKey(正式 / 沙箱两把)
	Env            uint32 // 0 = 正式,1 = 沙箱
	ZoneID         string // 分区 ID,缺省 "1"
	PushToken      string // 消息推送的 Token
	EncodingAESKey string // 消息推送安全模式的 EncodingAESKey(43 字符);明文模式留空
	APIBase        string // 缺省 https://api.weixin.qq.com;只有用例改
	HTTP           *http.Client
	Now            func() time.Time
}

type WechatMinigame struct {
	cfg     WechatMinigameConfig
	sealKey []byte // 会话密钥落库前的加密密钥,由 AppSecret 派生
	aesKey  []byte // 消息推送解密密钥;明文模式为空

	tokenMu     sync.Mutex
	accessToken string
	tokenExpiry time.Time
}

func NewWechatMinigame(cfg WechatMinigameConfig) (*WechatMinigame, error) {
	switch {
	case cfg.AppID == "" || cfg.AppSecret == "" || cfg.OfferID == "" || cfg.AppKey == "":
		return nil, errors.New("payment channel: wechat_minigame needs AppID, AppSecret, OfferID and AppKey")
	case cfg.PushToken == "":
		return nil, errors.New("payment channel: wechat_minigame needs the message push token")
	case cfg.Env > 1:
		return nil, errors.New("payment channel: wechat_minigame Env must be 0 (production) or 1 (sandbox)")
	}
	if cfg.ZoneID == "" {
		cfg.ZoneID = "1"
	}
	if cfg.APIBase == "" {
		cfg.APIBase = wechatMiniAPIBase
	}
	if cfg.HTTP == nil {
		cfg.HTTP = newHTTPClient()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	w := &WechatMinigame{cfg: cfg}
	seal := sha256.Sum256([]byte("payment:wechat-minigame:session-key-seal:v1\n" + cfg.AppSecret))
	w.sealKey = seal[:]
	if cfg.EncodingAESKey != "" {
		key, err := base64.StdEncoding.DecodeString(cfg.EncodingAESKey + "=")
		if err != nil || len(key) != 32 {
			return nil, errors.New("payment channel: wechat_minigame EncodingAESKey must be the 43-character key from the console")
		}
		w.aesKey = key
	}
	return w, nil
}

func (w *WechatMinigame) ID() paymentpb.PayChannel {
	return paymentpb.PayChannel_PAY_CHANNEL_WECHAT_MINIGAME
}

func (w *WechatMinigame) Name() string { return NameWechatMinigame }

func (w *WechatMinigame) Supports(currency string) bool { return currency == "CNY" }

// NeedsProduct:道具直购要 MP 后台登记过的道具 ID(goodsPrice 会与后台价格比对)。
func (w *WechatMinigame) NeedsProduct() bool { return true }

func (w *WechatMinigame) Close(context.Context, string) error { return nil }

func (w *WechatMinigame) Refund(context.Context, RefundRequest) (RefundResult, error) {
	return refundUnsupported, nil
}

// code2Session 用当次 wx.login 的 code 换 openid 与 session_key。session_key 只在本进程内存与加密后的意图行里出现,不进日志。
func (w *WechatMinigame) code2Session(ctx context.Context, code string) (string, string, error) {
	query := url.Values{"appid": {w.cfg.AppID}, "secret": {w.cfg.AppSecret}, "js_code": {code}, "grant_type": {"authorization_code"}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, w.cfg.APIBase+"/sns/jscode2session?"+query.Encode(), nil)
	if err != nil {
		return "", "", err
	}
	response, err := w.cfg.HTTP.Do(request)
	if err != nil {
		return "", "", err
	}
	defer response.Body.Close()
	raw, err := readBody(response)
	if err != nil {
		return "", "", err
	}
	var out struct {
		OpenID     string `json:"openid"`
		SessionKey string `json:"session_key"`
		ErrCode    int    `json:"errcode"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", "", errors.New("payment channel: wechat_minigame code2session answer is not readable")
	}
	switch {
	case out.ErrCode == 40029 || out.ErrCode == 40226 || out.ErrCode == 40163:
		return "", "", ErrLoginRequired // code 无效、被拦截或已用过
	case out.ErrCode != 0 || out.OpenID == "" || out.SessionKey == "":
		return "", "", &CallError{Channel: NameWechatMinigame, Op: "code2session", Status: response.StatusCode, Code: strconv.Itoa(out.ErrCode)}
	}
	return out.OpenID, out.SessionKey, nil
}

// wechatMiniSignData 的字段顺序就是 signData 串里的顺序;客户端原样传,顺序本身不影响验签,但要稳定可审。
type wechatMiniSignData struct {
	Mode         string `json:"mode"`
	OfferID      string `json:"offerId"`
	BuyQuantity  int    `json:"buyQuantity"`
	Env          uint32 `json:"env"`
	CurrencyType string `json:"currencyType"`
	Platform     string `json:"platform"`
	ZoneID       string `json:"zoneId"`
	ProductID    string `json:"productId"`
	GoodsPrice   uint64 `json:"goodsPrice"`
	OutTradeNo   string `json:"outTradeNo"`
	Attach       string `json:"attach"`
}

// Prepare 每次调起都用新的 code 换会话密钥:wx.login 会更新 session_key,旧的签不过。
func (w *WechatMinigame) Prepare(ctx context.Context, order Order, _ Ref) (Prepared, error) {
	if order.LoginCode == "" {
		return Prepared{}, ErrLoginRequired
	}
	if order.ProductID == "" || order.Currency != "CNY" {
		return Prepared{}, errors.New("payment channel: wechat_minigame needs a product id and a CNY order")
	}
	openID, sessionKey, err := w.code2Session(ctx, order.LoginCode)
	if err != nil {
		return Prepared{}, err
	}
	platform := "android"
	if order.DeviceOS == "ios" {
		platform = "ios"
	}
	encoded, err := json.Marshal(wechatMiniSignData{Mode: "goods", OfferID: w.cfg.OfferID, BuyQuantity: 1, Env: w.cfg.Env, CurrencyType: "CNY",
		Platform: platform, ZoneID: w.cfg.ZoneID, ProductID: order.ProductID, GoodsPrice: order.AmountMinor, OutTradeNo: order.OutTradeNo})
	if err != nil {
		return Prepared{}, err
	}
	signData := string(encoded)
	sealed, err := w.seal(sessionKey)
	if err != nil {
		return Prepared{}, err
	}
	return Prepared{
		Ref:     Ref{Value: sealed, At: w.cfg.Now()},
		PayerID: openID,
		Params: ClientParams{WechatMinigame: &paymentpb.WechatMinigamePayParams{
			SignData:  signData,
			PaySig:    hmacHex([]byte(w.cfg.AppKey), "requestMidasPaymentGameItem&"+signData),
			Signature: hmacHex([]byte(sessionKey), signData),
		}},
	}, nil
}

// seal / unseal:会话密钥是用户态凭证,落库前用 AES-256-GCM 加密(密钥由 AppSecret 派生),意图结束时随渠道引用一起清空。
func (w *WechatMinigame) seal(plain string) (string, error) {
	block, err := aes.NewCipher(w.sealKey)
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(aead.Seal(nonce, nonce, []byte(plain), nil)), nil
}

func (w *WechatMinigame) unseal(sealed string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(w.sealKey)
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < aead.NonceSize() {
		return "", errors.New("payment channel: sealed session key is too short")
	}
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], nil)
	if err != nil {
		return "", errors.New("payment channel: sealed session key cannot be opened")
	}
	return string(plain), nil
}

// token 是接口调用凭据(稳定版:多副本同时取拿到的是同一个,不会互相挤掉),过期前 5 分钟换新。
func (w *WechatMinigame) token(ctx context.Context) (string, error) {
	w.tokenMu.Lock()
	defer w.tokenMu.Unlock()
	now := w.cfg.Now()
	if w.accessToken != "" && now.Before(w.tokenExpiry.Add(-5*time.Minute)) {
		return w.accessToken, nil
	}
	body, err := json.Marshal(map[string]string{"grant_type": "client_credential", "appid": w.cfg.AppID, "secret": w.cfg.AppSecret})
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.APIBase+"/cgi-bin/stable_token", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := w.cfg.HTTP.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	raw, err := readBody(response)
	if err != nil {
		return "", err
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		ErrCode     int    `json:"errcode"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.ErrCode != 0 || out.AccessToken == "" || out.ExpiresIn <= 0 {
		return "", &CallError{Channel: NameWechatMinigame, Op: "stable_token", Status: response.StatusCode, Code: strconv.Itoa(out.ErrCode)}
	}
	w.accessToken, w.tokenExpiry = out.AccessToken, now.Add(time.Duration(out.ExpiresIn)*time.Second)
	return w.accessToken, nil
}

// Query 查道具直购订单。要建意图时存下的 openid 与会话密钥;会话密钥过期(90016)时查不了,只能等发货推送。
func (w *WechatMinigame) Query(ctx context.Context, req QueryRequest) (QueryResult, error) {
	if req.PayerID == "" || req.Ref.Value == "" {
		return QueryResult{}, ErrQueryUnsupported
	}
	sessionKey, err := w.unseal(req.Ref.Value)
	if err != nil {
		return QueryResult{}, ErrQueryUnsupported
	}
	body, err := json.Marshal(map[string]any{"openid": req.PayerID, "offer_id": w.cfg.OfferID, "ts": w.cfg.Now().Unix(),
		"zone_id": w.cfg.ZoneID, "env": w.cfg.Env, "out_trade_no": req.OutTradeNo, "biz_id": 2})
	if err != nil {
		return QueryResult{}, err
	}
	token, err := w.token(ctx)
	if err != nil {
		return QueryResult{}, err
	}
	const uri = "/wxa/game/queryorderinfo"
	query := url.Values{"access_token": {token}, "signature": {hmacHex([]byte(sessionKey), string(body))}, "sig_method": {"hmac_sha256"},
		"pay_sig": {hmacHex([]byte(w.cfg.AppKey), uri+"&"+string(body))}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.APIBase+uri+"?"+query.Encode(), bytes.NewReader(body))
	if err != nil {
		return QueryResult{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := w.cfg.HTTP.Do(request)
	if err != nil {
		return QueryResult{}, err
	}
	defer response.Body.Close()
	raw, err := readBody(response)
	if err != nil {
		return QueryResult{}, err
	}
	var out struct {
		ErrCode       int    `json:"errcode"`
		PayState      int    `json:"pay_state"`
		PayFinishTime int64  `json:"pay_finish_time"`
		OutTradeNo    string `json:"out_trade_no"`
		TransactionID string `json:"transaction_id"`
		ProductID     string `json:"product_id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return QueryResult{}, errors.New("payment channel: wechat_minigame query answer is not readable")
	}
	switch {
	case out.ErrCode == 90019:
		return QueryResult{State: TradeNotPaid}, nil
	case out.ErrCode != 0:
		return QueryResult{}, &CallError{Channel: NameWechatMinigame, Op: "query", Status: response.StatusCode, Code: strconv.Itoa(out.ErrCode)}
	case out.PayState != 2:
		return QueryResult{State: TradeNotPaid}, nil
	case out.OutTradeNo != req.OutTradeNo || req.AmountMinor == 0:
		return QueryResult{}, errors.New("payment channel: wechat_minigame query answer belongs to another order")
	}
	paidAt := w.cfg.Now()
	if out.PayFinishTime > 0 {
		paidAt = time.Unix(out.PayFinishTime, 0)
	}
	return QueryResult{State: TradePaid, Payment: &Payment{TransactionID: req.OutTradeNo, OutTradeNo: req.OutTradeNo, AmountMinor: req.AmountMinor,
		Currency: "CNY", PaidAt: paidAt, ProductID: out.ProductID, ChannelOrderID: out.TransactionID, Test: w.cfg.Env == 1}}, nil
}

// sortedSHA1 是微信消息推送的 URL 签名:参数按字典序排序后拼接,取 SHA-1 的十六进制。
func sortedSHA1(values ...string) string {
	sorted := append([]string(nil), values...)
	sort.Strings(sorted)
	sum := sha1.Sum([]byte(strings.Join(sorted, "")))
	return hex.EncodeToString(sum[:])
}

func equalHex(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(strings.ToLower(a)), []byte(strings.ToLower(b))) == 1
}

// Handshake 是在控制台配置推送地址时的 GET 校验:签名对了原样回 echostr。
func (w *WechatMinigame) Handshake(rw http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	q := r.URL.Query()
	if !equalHex(q.Get("signature"), sortedSHA1(w.cfg.PushToken, q.Get("timestamp"), q.Get("nonce"))) {
		rw.WriteHeader(http.StatusUnauthorized)
		return true
	}
	rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
	rw.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(rw, q.Get("echostr"))
	return true
}

// decrypt 是消息推送安全模式的解密(AES-256-CBC,IV 取密钥前 16 字节,PKCS#7 填充按 32 字节块):
// 明文 = 16 字节随机串 + 4 字节网络序长度 + 消息 + AppID。AppID 不是本小游戏的拒绝。
func (w *WechatMinigame) decrypt(encrypted string) ([]byte, error) {
	sealed, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil || len(sealed) == 0 || len(sealed)%aes.BlockSize != 0 {
		return nil, errors.New("payment channel: wechat_minigame encrypted push is malformed")
	}
	block, err := aes.NewCipher(w.aesKey)
	if err != nil {
		return nil, err
	}
	plain := make([]byte, len(sealed))
	cipher.NewCBCDecrypter(block, w.aesKey[:aes.BlockSize]).CryptBlocks(plain, sealed)
	pad := int(plain[len(plain)-1])
	if pad < 1 || pad > 32 || pad > len(plain) {
		return nil, errors.New("payment channel: wechat_minigame encrypted push has bad padding")
	}
	plain = plain[:len(plain)-pad]
	if len(plain) < 20 {
		return nil, errors.New("payment channel: wechat_minigame encrypted push is too short")
	}
	size := int(binary.BigEndian.Uint32(plain[16:20]))
	if size < 0 || 20+size > len(plain) {
		return nil, errors.New("payment channel: wechat_minigame encrypted push has a bad length")
	}
	if string(plain[20+size:]) != w.cfg.AppID {
		return nil, errors.New("payment channel: wechat_minigame encrypted push is for another app")
	}
	return plain[20 : 20+size], nil
}

type wechatMiniPush struct {
	Event    string `json:"Event"`
	MiniGame struct {
		Payload     string `json:"Payload"`
		PayEventSig string `json:"PayEventSig"`
		IsMock      bool   `json:"IsMock"`
	} `json:"MiniGame"`
}

func (w *WechatMinigame) ParseNotification(r *http.Request, body []byte) (Notification, error) {
	q := r.URL.Query()
	timestamp, nonce := q.Get("timestamp"), q.Get("nonce")
	if q.Get("encrypt_type") == "aes" {
		var envelope struct {
			Encrypt string `json:"Encrypt"`
		}
		if w.aesKey == nil || json.Unmarshal(body, &envelope) != nil || envelope.Encrypt == "" ||
			!equalHex(q.Get("msg_signature"), sortedSHA1(w.cfg.PushToken, timestamp, nonce, envelope.Encrypt)) {
			return Notification{}, ErrBadSignature
		}
		plain, err := w.decrypt(envelope.Encrypt)
		if err != nil {
			return Notification{}, ErrBadSignature
		}
		body = plain
	} else if !equalHex(q.Get("signature"), sortedSHA1(w.cfg.PushToken, timestamp, nonce)) {
		return Notification{}, ErrBadSignature
	}
	var push wechatMiniPush
	if err := json.Unmarshal(body, &push); err != nil || push.Event == "" {
		return Notification{}, ErrBadSignature
	}
	if push.Event != "minigame_game_pay_goods_deliver_notify" && push.Event != "minigame_pay_refund_succ_notify" {
		return Notification{}, nil
	}
	if !equalHex(push.MiniGame.PayEventSig, hmacHex([]byte(w.cfg.AppKey), push.Event+"&"+push.MiniGame.Payload)) {
		return Notification{}, ErrBadSignature
	}
	if push.MiniGame.IsMock {
		return Notification{}, nil // 控制台的模拟推送:只回成功,不做业务
	}
	if push.Event == "minigame_pay_refund_succ_notify" {
		var refund struct {
			Env          uint32 `json:"Env"`
			OutTradeNo   string `json:"OutTradeNo"`
			RefundSource int    `json:"RefundSource"`
		}
		if err := json.Unmarshal([]byte(push.MiniGame.Payload), &refund); err != nil || refund.OutTradeNo == "" || refund.Env != w.cfg.Env {
			return Notification{}, ErrBadSignature
		}
		return Notification{Revocations: []Revocation{{TransactionID: refund.OutTradeNo, Revoked: true, ObservedAt: w.cfg.Now(),
			Detail: "REFUND_SOURCE_" + strconv.Itoa(refund.RefundSource)}}}, nil
	}
	var goods struct {
		Env        uint32 `json:"Env"`
		OutTradeNo string `json:"OutTradeNo"`
		GoodsInfo  struct {
			ProductID string `json:"ProductId"`
			Quantity  uint64 `json:"Quantity"`
			OrigPrice uint64 `json:"OrigPrice"`
		} `json:"GoodsInfo"`
		WeChatPayInfo struct {
			TransactionID string `json:"TransactionId"`
		} `json:"WeChatPayInfo"`
	}
	if err := json.Unmarshal([]byte(push.MiniGame.Payload), &goods); err != nil || goods.OutTradeNo == "" || goods.Env != w.cfg.Env || goods.GoodsInfo.OrigPrice == 0 {
		return Notification{}, ErrBadSignature
	}
	quantity := goods.GoodsInfo.Quantity
	if quantity == 0 {
		quantity = 1
	}
	payment := &Payment{TransactionID: goods.OutTradeNo, OutTradeNo: goods.OutTradeNo, AmountMinor: goods.GoodsInfo.OrigPrice * quantity, Currency: "CNY",
		PaidAt: w.cfg.Now(), ProductID: goods.GoodsInfo.ProductID, ChannelOrderID: goods.WeChatPayInfo.TransactionID, Test: goods.Env == 1}
	if quantity != 1 {
		payment.Anomaly = "wechat_minigame goods quantity is " + strconv.FormatUint(quantity, 10)
	}
	return Notification{Payment: payment}, nil
}

// WriteAck:成功 {"ErrCode":0,"ErrMsg":"Success"},失败 {"ErrCode":99999,...};安全模式下应答也是明文。
func (w *WechatMinigame) WriteAck(rw http.ResponseWriter, kind AckKind) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	if kind == AckOK {
		_, _ = rw.Write([]byte(`{"ErrCode":0,"ErrMsg":"Success"}`))
		return
	}
	_, _ = rw.Write([]byte(`{"ErrCode":99999,"ErrMsg":"internal error"}`))
}
