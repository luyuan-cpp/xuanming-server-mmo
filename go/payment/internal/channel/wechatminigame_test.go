package channel

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

const (
	miniAppID         = "wxminigame00000001"
	miniOfferID       = "1450000001"
	miniOpenID        = "oUrsf0000000000000000000001"
	miniLoginCode     = "0a1B2c3D4e5F6g7H8i9J"
	miniAccessToken   = "ACCESS-TOKEN-FOR-TESTS"
	miniProductID     = "gold_60"
	miniTransactionID = "4200001234202611201234567891"
	miniPushTimestamp = "1763640000"
	miniPushNonce     = "nonce-1"
	miniDeliverEvent  = "minigame_game_pay_goods_deliver_notify"
	miniRefundEvent   = "minigame_pay_refund_succ_notify"

	// 假 jscode2session 的成功应答;SESSION_KEY 由假微信换成这次现场生成的会话密钥。
	miniSessionOK = `{"openid":"` + miniOpenID + `","session_key":"SESSION_KEY","unionid":"u-1"}`
	miniTokenOK   = `{"access_token":"` + miniAccessToken + `","expires_in":7200}`
)

// miniFixture 是一个渠道,外加扮演微信要用的密钥(都是现场生成的随机串)。
type miniFixture struct {
	channel    *WechatMinigame
	appSecret  string
	appKey     string
	pushToken  string
	sessionKey string // 假 jscode2session 发的会话密钥
	aesKey     []byte // 消息推送安全模式的密钥;明文模式为 nil
}

func newMiniSecrets(t *testing.T) miniFixture {
	t.Helper()
	return miniFixture{appSecret: testSecret(t), appKey: testSecret(t), pushToken: testSecret(t), sessionKey: testSecret(t)}
}

// connect 用这些密钥建一个渠道。secure 为真时配上 EncodingAESKey(消息推送安全模式)。
func (f miniFixture) connect(t *testing.T, env uint32, secure bool, apiBase string, client *http.Client) miniFixture {
	t.Helper()
	cfg := WechatMinigameConfig{AppID: miniAppID, AppSecret: f.appSecret, OfferID: miniOfferID, AppKey: f.appKey, Env: env,
		PushToken: f.pushToken, APIBase: apiBase, HTTP: client, Now: fixedNow}
	if secure {
		f.aesKey = make([]byte, 32)
		if _, err := rand.Read(f.aesKey); err != nil {
			t.Fatalf("random AES key: %v", err)
		}
		cfg.EncodingAESKey = strings.TrimRight(base64.StdEncoding.EncodeToString(f.aesKey), "=")
	}
	ch, err := NewWechatMinigame(cfg)
	if err != nil {
		t.Fatalf("NewWechatMinigame: %v", err)
	}
	f.channel = ch
	return f
}

// miniAPI 是假微信开放接口三个地址的应答(JSON 文本);空串表示这个地址不该被调到。
type miniAPI struct {
	session string // GET /sns/jscode2session
	token   string // POST /cgi-bin/stable_token
	order   string // POST /wxa/game/queryorderinfo
}

// startMini 起一个假微信开放接口并把渠道接上去。每个地址先核对请求里的密钥与签名,再回 api 里的应答;
// 调用记成地址,查单另记一条「order 正文」。
func startMini(t *testing.T, env uint32, api miniAPI) (miniFixture, *callLog) {
	t.Helper()
	f := newMiniSecrets(t)
	appSecret, appKey, sessionKey := f.appSecret, f.appKey, f.sessionKey
	calls := &callLog{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		query := r.URL.Query()
		calls.add(r.URL.Path)
		answer := ""
		switch r.URL.Path {
		case "/sns/jscode2session":
			// 核对不过时不打印查询串:里面有 AppSecret。
			if r.Method != http.MethodGet || query.Get("appid") != miniAppID || query.Get("secret") != appSecret ||
				query.Get("js_code") != miniLoginCode || query.Get("grant_type") != "authorization_code" {
				t.Errorf("jscode2session: unexpected method or query parameters")
			}
			answer = strings.ReplaceAll(api.session, "SESSION_KEY", sessionKey)
		case "/cgi-bin/stable_token":
			var in struct {
				GrantType string `json:"grant_type"`
				AppID     string `json:"appid"`
				Secret    string `json:"secret"`
			}
			if json.Unmarshal(body, &in) != nil || r.Method != http.MethodPost || in.GrantType != "client_credential" || in.AppID != miniAppID || in.Secret != appSecret {
				t.Errorf("stable_token: unexpected method or body")
			}
			answer = api.token
		case "/wxa/game/queryorderinfo":
			if r.Method != http.MethodPost || query.Get("access_token") != miniAccessToken || query.Get("sig_method") != "hmac_sha256" ||
				query.Get("pay_sig") != hmacHex([]byte(appKey), "/wxa/game/queryorderinfo&"+string(body)) ||
				query.Get("signature") != hmacHex([]byte(sessionKey), string(body)) {
				t.Errorf("queryorderinfo: the token or the signatures do not match the request body")
			}
			calls.add("order " + string(body))
			answer = api.order
		}
		if answer == "" {
			t.Errorf("unexpected call to %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(server.Close)
	return f.connect(t, env, false, server.URL, server.Client()), calls
}

func miniOrder() Order {
	return Order{OrderID: testOrderID, OutTradeNo: testOutTradeNo, AmountMinor: 600, Currency: "CNY", ProductID: miniProductID,
		LoginCode: miniLoginCode, DeviceOS: "android"}
}

// queryRequest 是建意图之后的一次查单:带上付款人 openid 与加密存下的会话密钥。
func (f miniFixture) queryRequest(t *testing.T) QueryRequest {
	t.Helper()
	sealed, err := f.channel.seal(f.sessionKey)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	return QueryRequest{OutTradeNo: testOutTradeNo, PayerID: miniOpenID, Ref: Ref{Value: sealed, At: testNow}, AmountMinor: 600, Currency: "CNY"}
}

func TestNewWechatMinigameRejectsUnusableConfig(t *testing.T) {
	good := WechatMinigameConfig{AppID: miniAppID, AppSecret: testSecret(t), OfferID: miniOfferID, AppKey: testSecret(t), PushToken: testSecret(t)}
	if ch, err := NewWechatMinigame(good); err != nil || NameOf(ch.ID()) != NameWechatMinigame || ch.Name() != NameWechatMinigame || !ch.Supports("CNY") || ch.Supports("USD") || !ch.NeedsProduct() {
		t.Fatalf("NewWechatMinigame(good): %v", err)
	}
	for name, edit := range map[string]func(*WechatMinigameConfig){
		"no app secret":                     func(c *WechatMinigameConfig) { c.AppSecret = "" },
		"no app key":                        func(c *WechatMinigameConfig) { c.AppKey = "" },
		"no push token":                     func(c *WechatMinigameConfig) { c.PushToken = "" },
		"an environment other than 0 or 1":  func(c *WechatMinigameConfig) { c.Env = 2 },
		"an EncodingAESKey of 42 characters": func(c *WechatMinigameConfig) { c.EncodingAESKey = strings.Repeat("A", 42) },
	} {
		bad := good
		edit(&bad)
		if _, err := NewWechatMinigame(bad); err == nil {
			t.Fatalf("NewWechatMinigame accepted %s", name)
		}
	}
}

// 道具直购的调起参数:signData 的字段与顺序固定;paySig 用 AppKey 对「requestMidasPaymentGameItem&signData」做 HMAC,
// signature 用当次登录换来的会话密钥对 signData 做 HMAC。会话密钥加密后当渠道引用存下,付款人是 openid。
func TestWechatMinigamePrepareSignsSignData(t *testing.T) {
	f, calls := startMini(t, 0, miniAPI{session: miniSessionOK})
	ctx := context.Background()
	prepared, err := f.channel.Prepare(ctx, miniOrder(), Ref{})
	if err != nil || prepared.Params.WechatMinigame == nil {
		t.Fatalf("Prepare: %v", err)
	}
	params := prepared.Params.WechatMinigame
	wantSignData := `{"mode":"goods","offerId":"` + miniOfferID + `","buyQuantity":1,"env":0,"currencyType":"CNY","platform":"android","zoneId":"1","productId":"` + miniProductID +
		`","goodsPrice":600,"outTradeNo":"` + testOutTradeNo + `","attach":""}`
	if params.SignData != wantSignData {
		t.Fatalf("signData = %s, want %s", params.SignData, wantSignData)
	}
	if params.PaySig != hmacHex([]byte(f.appKey), "requestMidasPaymentGameItem&"+wantSignData) {
		t.Fatal("paySig is not HMAC-SHA256(AppKey, requestMidasPaymentGameItem&signData)")
	}
	if params.Signature != hmacHex([]byte(f.sessionKey), wantSignData) {
		t.Fatal("signature is not HMAC-SHA256(session_key, signData)")
	}
	if key, err := f.channel.unseal(prepared.Ref.Value); err != nil || key != f.sessionKey || strings.Contains(prepared.Ref.Value, f.sessionKey) ||
		!prepared.Ref.At.Equal(testNow) || prepared.PayerID != miniOpenID {
		t.Fatalf("Prepare ref: payer=%q at=%v unseal error=%v", prepared.PayerID, prepared.Ref.At, err)
	}

	ios := miniOrder()
	ios.DeviceOS = "ios"
	prepared, err = f.channel.Prepare(ctx, ios, Ref{})
	if err != nil || !strings.Contains(prepared.Params.WechatMinigame.SignData, `"platform":"ios"`) {
		t.Fatalf("Prepare on iOS: %v", err)
	}
	if got := calls.list(); len(got) != 2 || got[0] != "/sns/jscode2session" || got[1] != "/sns/jscode2session" {
		t.Fatalf("calls = %q, want one jscode2session per Prepare", got)
	}

	// 没有登录凭证、没有道具号、不是人民币:不去换会话密钥。
	noCode, noProduct, usd := miniOrder(), miniOrder(), miniOrder()
	noCode.LoginCode, noProduct.ProductID, usd.Currency = "", "", "USD"
	if _, err := f.channel.Prepare(ctx, noCode, Ref{}); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("Prepare without a login code: err = %v, want ErrLoginRequired", err)
	}
	if _, err := f.channel.Prepare(ctx, noProduct, Ref{}); err == nil {
		t.Fatal("Prepare accepted an order without a product id")
	}
	if _, err := f.channel.Prepare(ctx, usd, Ref{}); err == nil {
		t.Fatal("Prepare accepted a USD order")
	}
	if got := len(calls.list()); got != 2 {
		t.Fatalf("refused orders still called WeChat: %d calls", got)
	}
}

// 登录凭证无效、被拦截、已用过:要客户端重新登录;别的错误码是渠道明确失败。
func TestWechatMinigameLoginCodeProblemsAskForAFreshLogin(t *testing.T) {
	for code, relogin := range map[int]bool{40029: true, 40163: true, 40226: true, 45011: false, -1: false} {
		f, _ := startMini(t, 0, miniAPI{session: `{"errcode":` + strconv.Itoa(code) + `,"errmsg":"x"}`})
		_, err := f.channel.Prepare(context.Background(), miniOrder(), Ref{})
		var call *CallError
		switch {
		case relogin && !errors.Is(err, ErrLoginRequired):
			t.Fatalf("errcode %d: err = %v, want ErrLoginRequired", code, err)
		case !relogin && (!errors.As(err, &call) || call.Channel != NameWechatMinigame || call.Op != "code2session" || call.Code != strconv.Itoa(code)):
			t.Fatalf("errcode %d: err = %v, want a code2session CallError", code, err)
		}
	}
	f, _ := startMini(t, 0, miniAPI{session: `{"openid":"` + miniOpenID + `"}`})
	_, err := f.channel.Prepare(context.Background(), miniOrder(), Ref{})
	var call *CallError
	if !errors.As(err, &call) || call.Op != "code2session" {
		t.Fatalf("answer without a session key: err = %v, want a code2session CallError", err)
	}
}

// 传输失败的错误文本里不能有 AppSecret、登录凭证、access_token:它们都在请求地址里,而错误会被写进日志。
func TestWechatMinigameTransportErrorsKeepSecretsOutOfTheErrorText(t *testing.T) {
	cause := errors.New("connection reset")
	secrets := newMiniSecrets(t)
	failing := secrets.connect(t, 0, false, "http://weixin.test", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, cause })})
	_, err := failing.channel.Prepare(context.Background(), miniOrder(), Ref{})
	if !errors.Is(err, cause) {
		t.Fatalf("Prepare: err = %v, want the transport error", err)
	}
	for name, secret := range map[string]string{"AppSecret": secrets.appSecret, "login code": miniLoginCode} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("the Prepare error text leaks the %s", name)
		}
	}

	tokenOnly := secrets.connect(t, 0, false, "http://weixin.test", &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/cgi-bin/stable_token" {
			return jsonAnswer(miniTokenOK), nil
		}
		return nil, cause
	})})
	_, err = tokenOnly.channel.Query(context.Background(), tokenOnly.queryRequest(t))
	if !errors.Is(err, cause) {
		t.Fatalf("Query: err = %v, want the transport error", err)
	}
	if strings.Contains(err.Error(), miniAccessToken) || strings.Contains(err.Error(), "pay_sig") {
		t.Fatal("the Query error text leaks the access token or the request query string")
	}
}

// 查单:请求正文与两个签名;pay_state 2 才算已付,金额按意图上的应付金额记;单号不存在算没付;access_token 在有效期内复用。
func TestWechatMinigameQueryMapsOrderStates(t *testing.T) {
	ctx := context.Background()
	paid := `{"errcode":0,"errmsg":"ok","product_id":"` + miniProductID + `","pay_state":2,"deliver_state":1,"pay_finish_time":` + strconv.FormatInt(testPaidAt.Unix(), 10) +
		`,"out_trade_no":"` + testOutTradeNo + `","mch_order_no":"mch-1","transaction_id":"` + miniTransactionID + `"}`
	wantOrderCall := `order {"biz_id":2,"env":0,"offer_id":"` + miniOfferID + `","openid":"` + miniOpenID + `","out_trade_no":"` + testOutTradeNo +
		`","ts":` + strconv.FormatInt(testNow.Unix(), 10) + `,"zone_id":"1"}`

	f, calls := startMini(t, 0, miniAPI{token: miniTokenOK, order: paid})
	request := f.queryRequest(t)
	result, err := f.channel.Query(ctx, request)
	if err != nil || result.State != TradePaid || result.Payment == nil {
		t.Fatalf("Query = %+v, %v", result, err)
	}
	want := Payment{TransactionID: testOutTradeNo, OutTradeNo: testOutTradeNo, AmountMinor: 600, Currency: "CNY", PaidAt: testPaidAt,
		ProductID: miniProductID, ChannelOrderID: miniTransactionID}
	if !samePayment(*result.Payment, want) {
		t.Fatalf("payment = %+v, want %+v", *result.Payment, want)
	}
	if _, err := f.channel.Query(ctx, request); err != nil {
		t.Fatalf("second Query: %v", err)
	}
	if calls.count("/cgi-bin/stable_token") != 1 || calls.count("/wxa/game/queryorderinfo") != 2 || calls.count(wantOrderCall) != 2 {
		t.Fatalf("calls = %q, want one stable_token and two queries with %q", calls.list(), wantOrderCall)
	}

	// 意图上没有应付金额时不能记账(查单应答不带金额)。
	free := request
	free.AmountMinor = 0
	if result, err := f.channel.Query(ctx, free); err == nil {
		t.Fatalf("Query without an intent amount = %+v, want an error", result)
	}
	// 没有付款人或会话密钥、会话密钥解不开:查不了,只能等发货推送。
	other := newMiniSecrets(t).connect(t, 0, false, "", nil)
	for name, req := range map[string]QueryRequest{
		"no payer":                        {OutTradeNo: testOutTradeNo, Ref: request.Ref, AmountMinor: 600},
		"no session key":                  {OutTradeNo: testOutTradeNo, PayerID: miniOpenID, AmountMinor: 600},
		"a reference that is not sealed":  {OutTradeNo: testOutTradeNo, PayerID: miniOpenID, Ref: Ref{Value: "not-sealed"}, AmountMinor: 600},
		"a reference sealed by another app": {OutTradeNo: testOutTradeNo, PayerID: miniOpenID, Ref: other.queryRequest(t).Ref, AmountMinor: 600},
	} {
		if _, err := f.channel.Query(ctx, req); !errors.Is(err, ErrQueryUnsupported) {
			t.Fatalf("%s: err = %v, want ErrQueryUnsupported", name, err)
		}
	}

	for name, c := range map[string]struct {
		order string
		state TradeState // 0 表示期望报错
		code  string     // 期望的 CallError.Code;state 为 0 而它为空表示期望别的错误
	}{
		"not paid yet":            {order: `{"errcode":0,"errmsg":"ok","pay_state":1,"out_trade_no":"` + testOutTradeNo + `"}`, state: TradeNotPaid},
		"an unknown order number": {order: `{"errcode":90019,"errmsg":"x"}`, state: TradeNotPaid},
		"an expired session key":  {order: `{"errcode":90016,"errmsg":"x"}`, code: "90016"},
		"a rejected pay_sig":      {order: `{"errcode":90011,"errmsg":"x"}`, code: "90011"},
		"another order":           {order: strings.Replace(paid, testOutTradeNo, "ffffffffffffffffffffffffffffffff", 1)},
		"not JSON":                {order: `<html>`},
	} {
		t.Run(name, func(t *testing.T) {
			f, _ := startMini(t, 0, miniAPI{token: miniTokenOK, order: c.order})
			result, err := f.channel.Query(ctx, f.queryRequest(t))
			var call *CallError
			switch {
			case c.state != 0:
				if err != nil || result.State != c.state || result.Payment != nil {
					t.Fatalf("Query = %+v, %v", result, err)
				}
			case c.code != "":
				if !errors.As(err, &call) || call.Op != "query" || call.Code != c.code {
					t.Fatalf("Query: err = %v, want a query CallError with code %s", err, c.code)
				}
			default:
				if err == nil || errors.As(err, &call) {
					t.Fatalf("Query = %+v, %v; want a plain error", result, err)
				}
			}
		})
	}

	sandbox, _ := startMini(t, 1, miniAPI{token: miniTokenOK, order: paid})
	if result, err := sandbox.channel.Query(ctx, sandbox.queryRequest(t)); err != nil || result.Payment == nil || !result.Payment.Test {
		t.Fatalf("sandbox Query = %+v, %v; want a payment marked as a test", result, err)
	}
	rejected, _ := startMini(t, 0, miniAPI{token: `{"errcode":40013,"errmsg":"invalid appid"}`})
	_, err = rejected.channel.Query(ctx, rejected.queryRequest(t))
	var call *CallError
	if !errors.As(err, &call) || call.Op != "stable_token" || call.Code != "40013" {
		t.Fatalf("rejected stable_token: err = %v, want a stable_token CallError", err)
	}
}

// push 造一条消息推送正文(JSON 格式):事件、Payload,以及用 AppKey 对「事件&Payload」算的 PayEventSig。
func (f miniFixture) push(event, payload string, mock bool) []byte {
	body, _ := json.Marshal(map[string]any{
		"ToUserName": "gh_000000000001", "FromUserName": "oUrsf-official-account", "CreateTime": testNow.Unix(), "MsgType": "event", "Event": event,
		"MiniGame": map[string]any{"Payload": payload, "PayEventSig": hmacHex([]byte(f.appKey), event+"&"+payload), "IsMock": mock},
	})
	return body
}

// plainRequest 是明文模式的推送请求:地址上的 signature 只覆盖 Token、timestamp、nonce;正文靠 PayEventSig。
func (f miniFixture) plainRequest() *http.Request {
	query := url.Values{"signature": {sortedSHA1(f.pushToken, miniPushTimestamp, miniPushNonce)}, "timestamp": {miniPushTimestamp}, "nonce": {miniPushNonce}}
	return httptest.NewRequest(http.MethodPost, "/notify/wechat_minigame?"+query.Encode(), nil)
}

// miniEncrypt 按消息推送安全模式加密:明文 = 16 字节随机串 + 4 字节网络序长度 + 消息 + AppID,
// PKCS#7 按 32 字节补齐,AES-256-CBC,IV 取密钥前 16 字节。
func miniEncrypt(t *testing.T, aesKey []byte, appID string, message []byte) string {
	t.Helper()
	plain := make([]byte, 16, 16+4+len(message)+len(appID)+32)
	if _, err := rand.Read(plain); err != nil {
		t.Fatalf("random prefix: %v", err)
	}
	plain = binary.BigEndian.AppendUint32(plain, uint32(len(message)))
	plain = append(plain, message...)
	plain = append(plain, appID...)
	pad := 32 - len(plain)%32
	plain = append(plain, bytes.Repeat([]byte{byte(pad)}, pad)...)
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	sealed := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, aesKey[:aes.BlockSize]).CryptBlocks(sealed, plain)
	return base64.StdEncoding.EncodeToString(sealed)
}

// secure 把一条推送按安全模式包起来:正文只剩 Encrypt,地址上的 msg_signature 多覆盖一项密文。appID 是包进密文尾部的小游戏 AppID。
func (f miniFixture) secure(t *testing.T, appID string, message []byte) (*http.Request, []byte) {
	t.Helper()
	encrypted := miniEncrypt(t, f.aesKey, appID, message)
	query := url.Values{"encrypt_type": {"aes"}, "timestamp": {miniPushTimestamp}, "nonce": {miniPushNonce},
		"signature":     {sortedSHA1(f.pushToken, miniPushTimestamp, miniPushNonce)},
		"msg_signature": {sortedSHA1(f.pushToken, miniPushTimestamp, miniPushNonce, encrypted)}}
	body, err := json.Marshal(map[string]string{"ToUserName": "gh_000000000001", "Encrypt": encrypted})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return httptest.NewRequest(http.MethodPost, "/notify/wechat_minigame?"+query.Encode(), nil), body
}

// miniGoods 是发货推送的 Payload。原价 OrigPrice 600 分,用户实付 ActualPrice 550 分。
func miniGoods(env, quantity int) string {
	return `{"OpenId":"` + miniOpenID + `","Env":` + strconv.Itoa(env) + `,"OutTradeNo":"` + testOutTradeNo + `","GoodsInfo":{"ProductId":"` + miniProductID +
		`","Quantity":` + strconv.Itoa(quantity) + `,"ZoneId":"1","OrigPrice":600,"ActualPrice":550,"Attach":"","OrderSource":1},"WeChatPayInfo":{"MchOrderNo":"mch-1","TransactionId":"` +
		miniTransactionID + `"}}`
}

// miniPayment 是上面那条发货推送应当映射成的付款:交易号就是商户单号,金额取原价。
func miniPayment() Payment {
	return Payment{TransactionID: testOutTradeNo, OutTradeNo: testOutTradeNo, AmountMinor: 600, Currency: "CNY", PaidAt: testNow,
		ProductID: miniProductID, ChannelOrderID: miniTransactionID}
}

// 微信消息推送与抖音回调的地址签名:各项按字典序排序后拼接,取 SHA-1。"a"+"b"+"c" 的 SHA-1 是公开的测试向量。
func TestSortedSHA1MatchesTheKnownVector(t *testing.T) {
	if got := sortedSHA1("c", "a", "b"); got != "a9993e364706816aba3e25717850c26c9cd0d89d" {
		t.Fatalf("sortedSHA1(c, a, b) = %s, want SHA-1(\"abc\")", got)
	}
	if !equalHex("A9993E364706816ABA3E25717850C26C9CD0D89D", sortedSHA1("b", "c", "a")) || equalHex("a9993e", sortedSHA1("a", "b", "c")) {
		t.Fatal("equalHex must ignore case and reject a prefix")
	}
}

// 发货推送(明文模式):地址签名与 PayEventSig 都过了才映射成付款;改 Payload 一个字节、别的环境的推送都拒绝;
// 控制台的模拟推送与无关事件只回成功。
func TestWechatMinigameDeliveryPushBecomesAPayment(t *testing.T) {
	f := newMiniSecrets(t).connect(t, 0, false, "", nil)
	body := f.push(miniDeliverEvent, miniGoods(0, 1), false)
	note, err := f.channel.ParseNotification(f.plainRequest(), body)
	if err != nil || note.Payment == nil || len(note.Revocations) != 0 {
		t.Fatalf("notification = %+v, %v", note, err)
	}
	if !samePayment(*note.Payment, miniPayment()) {
		t.Fatalf("payment = %+v, want %+v", *note.Payment, miniPayment())
	}

	otherKey := f
	otherKey.appKey = testSecret(t)
	for name, c := range map[string]struct {
		request *http.Request
		body    []byte
	}{
		"the payload changed after signing": {f.plainRequest(), tamper(t, body, `\"OrigPrice\":600`, `\"OrigPrice\":601`)},
		"a URL signed with another token":   {newMiniSecrets(t).plainRequest(), body},
		"a URL without a signature":         {httptest.NewRequest(http.MethodPost, "/notify/wechat_minigame", nil), body},
		"an event signed with another key":  {f.plainRequest(), otherKey.push(miniDeliverEvent, miniGoods(0, 1), false)},
		"a push of the other environment":   {f.plainRequest(), f.push(miniDeliverEvent, miniGoods(1, 1), false)},
		"a payload without an order number": {f.plainRequest(), f.push(miniDeliverEvent, `{"Env":0,"GoodsInfo":{"OrigPrice":600}}`, false)},
		"a payload without a price":         {f.plainRequest(), f.push(miniDeliverEvent, strings.Replace(miniGoods(0, 1), `"OrigPrice":600`, `"OrigPrice":0`, 1), false)},
		"a body that is not JSON":           {f.plainRequest(), []byte("<xml><Event>x</Event></xml>")},
		"a body without an event":           {f.plainRequest(), []byte(`{}`)},
	} {
		if _, err := f.channel.ParseNotification(c.request, c.body); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("%s: err = %v, want ErrBadSignature", name, err)
		}
	}

	for name, body := range map[string][]byte{
		"a mock push from the console": f.push(miniDeliverEvent, miniGoods(0, 1), true),
		"an unrelated event":           []byte(`{"ToUserName":"gh_000000000001","MsgType":"event","Event":"wxa_media_check"}`),
	} {
		if note, err := f.channel.ParseNotification(f.plainRequest(), body); err != nil || note.Payment != nil || len(note.Revocations) != 0 {
			t.Fatalf("%s: notification = %+v, %v; want nothing to do", name, note, err)
		}
	}

	// 数量不是 1 的收了钱但不能按正常付款发货:记原因,由核心层转人工。推送不带数量时按 1 件。
	note, err = f.channel.ParseNotification(f.plainRequest(), f.push(miniDeliverEvent, miniGoods(0, 2), false))
	if err != nil || note.Payment == nil || note.Payment.AmountMinor != 1200 || note.Payment.Anomaly == "" {
		t.Fatalf("two pieces: notification = %+v, %v", note, err)
	}
	note, err = f.channel.ParseNotification(f.plainRequest(), f.push(miniDeliverEvent, miniGoods(0, 0), false))
	if err != nil || note.Payment == nil || !samePayment(*note.Payment, miniPayment()) {
		t.Fatalf("no quantity: notification = %+v, %v", note, err)
	}

	sandbox := newMiniSecrets(t).connect(t, 1, false, "", nil)
	note, err = sandbox.channel.ParseNotification(sandbox.plainRequest(), sandbox.push(miniDeliverEvent, miniGoods(1, 1), false))
	if err != nil || note.Payment == nil || !note.Payment.Test {
		t.Fatalf("sandbox push: notification = %+v, %v; want a payment marked as a test", note, err)
	}
}

// 退款推送翻成一次撤销:交易号是商户单号(与发货推送报的付款同一个号)。
func TestWechatMinigameRefundPushBecomesARevocation(t *testing.T) {
	f := newMiniSecrets(t).connect(t, 0, false, "", nil)
	payload := `{"OpenId":"` + miniOpenID + `","Env":0,"OutTradeNo":"` + testOutTradeNo + `","RefundSource":1}`
	body := f.push(miniRefundEvent, payload, false)
	note, err := f.channel.ParseNotification(f.plainRequest(), body)
	if err != nil || note.Payment != nil || len(note.Revocations) != 1 {
		t.Fatalf("notification = %+v, %v", note, err)
	}
	if got := note.Revocations[0]; got.TransactionID != testOutTradeNo || !got.Revoked || !got.ObservedAt.Equal(testNow) || got.Detail != "REFUND_SOURCE_1" || got.Payment != nil {
		t.Fatalf("revocation = %+v", got)
	}
	for name, body := range map[string][]byte{
		"the payload changed after signing": tamper(t, body, `\"RefundSource\":1`, `\"RefundSource\":2`),
		"a refund without an order number":  f.push(miniRefundEvent, `{"Env":0,"RefundSource":1}`, false),
		"a refund of the other environment": f.push(miniRefundEvent, strings.Replace(payload, `"Env":0`, `"Env":1`, 1), false),
	} {
		if _, err := f.channel.ParseNotification(f.plainRequest(), body); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("%s: err = %v, want ErrBadSignature", name, err)
		}
	}
}

// 安全模式:msg_signature 覆盖密文,解密后尾部的 AppID 必须是本小游戏;没配 EncodingAESKey 的渠道不收加密推送。
func TestWechatMinigameSecurePushIsDecrypted(t *testing.T) {
	f := newMiniSecrets(t).connect(t, 0, true, "", nil)
	message := f.push(miniDeliverEvent, miniGoods(0, 1), false)
	request, body := f.secure(t, miniAppID, message)
	note, err := f.channel.ParseNotification(request, body)
	if err != nil || note.Payment == nil || !samePayment(*note.Payment, miniPayment()) {
		t.Fatalf("notification = %+v, %v", note, err)
	}

	_, otherBody := f.secure(t, miniAppID, f.push(miniDeliverEvent, miniGoods(0, 2), false))
	if _, err := f.channel.ParseNotification(request, otherBody); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("ciphertext swapped under the same msg_signature: err = %v, want ErrBadSignature", err)
	}
	request, body = f.secure(t, "wxminigame00000002", message)
	if _, err := f.channel.ParseNotification(request, body); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("ciphertext for another app: err = %v, want ErrBadSignature", err)
	}
	request, _ = f.secure(t, miniAppID, message)
	if _, err := f.channel.ParseNotification(request, []byte(`{"ToUserName":"gh_000000000001"}`)); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("envelope without Encrypt: err = %v, want ErrBadSignature", err)
	}
	plainOnly := f.connect(t, 0, false, "", nil)
	request, body = f.secure(t, miniAppID, message)
	if _, err := plainOnly.channel.ParseNotification(request, body); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("encrypted push to a channel without EncodingAESKey: err = %v, want ErrBadSignature", err)
	}
}

// 配置推送地址时的 GET 握手:签名对了原样回 echostr,不对回 401;POST 不是握手。
func TestWechatMinigameHandshakeEchoesOnlyForAValidSignature(t *testing.T) {
	f := newMiniSecrets(t).connect(t, 0, false, "", nil)
	handshake := func(method, signature string) (bool, int, string) {
		query := url.Values{"signature": {signature}, "timestamp": {miniPushTimestamp}, "nonce": {miniPushNonce}, "echostr": {"echo-123"}}
		recorder := httptest.NewRecorder()
		handled := f.channel.Handshake(recorder, httptest.NewRequest(method, "/notify/wechat_minigame?"+query.Encode(), nil))
		return handled, recorder.Code, recorder.Body.String()
	}
	signature := sortedSHA1(f.pushToken, miniPushTimestamp, miniPushNonce)
	for _, valid := range []string{signature, strings.ToUpper(signature)} {
		if handled, status, body := handshake(http.MethodGet, valid); !handled || status != http.StatusOK || body != "echo-123" {
			t.Fatalf("handshake with a valid signature = %v, http %d, %q", handled, status, body)
		}
	}
	if handled, status, body := handshake(http.MethodGet, strings.Repeat("0", 40)); !handled || status != http.StatusUnauthorized || body != "" {
		t.Fatalf("handshake with a wrong signature = %v, http %d, %q", handled, status, body)
	}
	if handled, _, _ := handshake(http.MethodPost, signature); handled {
		t.Fatal("a POST was treated as the handshake")
	}
}

// 回调应答:成功 {"ErrCode":0,"ErrMsg":"Success"},失败 ErrCode 非 0;都是 200 的明文 JSON。
func TestWechatMinigameAckShapes(t *testing.T) {
	f := newMiniSecrets(t).connect(t, 0, false, "", nil)
	if status, body := ackOf(f.channel, AckOK); status != http.StatusOK || body != `{"ErrCode":0,"ErrMsg":"Success"}` {
		t.Fatalf("AckOK = http %d %s", status, body)
	}
	for _, kind := range []AckKind{AckRetry, AckRejected} {
		if status, body := ackOf(f.channel, kind); status != http.StatusOK || !strings.Contains(body, `"ErrCode":99999`) {
			t.Fatalf("ack %d = http %d %s, want a non-zero ErrCode", kind, status, body)
		}
	}
	if refund, err := f.channel.Refund(context.Background(), RefundRequest{}); err != nil || refund.State != RefundFailed || refund.Detail != "NO_REFUND_API" {
		t.Fatalf("Refund = %+v, %v; want a hand-over to manual handling", refund, err)
	}
	if err := f.channel.Close(context.Background(), testOutTradeNo); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
