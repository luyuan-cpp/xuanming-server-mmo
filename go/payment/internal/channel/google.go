package channel

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	paymentpb "proto/payment"
)

// Google Play 一次性消耗型商品。规则核对自 developers.google.com / developer.android.com 的接口参考:
//   - 服务账号换令牌:RS256 JWT(iss=client_email、scope=androidpublisher、aud=https://oauth2.googleapis.com/token、exp ≤ 1 小时),
//     POST 表单 grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer&assertion=JWT。
//   - 查购买:GET .../applications/{pkg}/purchases/productsv2/tokens/{token}(productLineItem / purchaseStateContext / testPurchaseContext ...)。
//   - 消耗:POST .../purchases/products/{productId}/tokens/{token}:consume;三天内不确认会被自动退款,消耗同时算确认。
//   - 退款:POST .../orders/{orderId}:refund?revoke=...;作废列表:GET .../purchases/voidedpurchases(startTime 不早于 30 天前)。
//   - 实时通知走 Pub/Sub 推送:Authorization: Bearer OIDC 令牌(RS256,iss accounts.google.com,aud 是订阅上配的受众,
//     email 是推送服务账号且 email_verified),正文 message.data 是 base64 的 DeveloperNotification。通知只是线索,状态以接口为准。
//
// 订单绑定:obfuscatedExternalProfileId = 商城订单号;obfuscatedExternalAccountId = 买家玩家号的 SHA-256(Google 要求不含个人信息)。
// 报给商城的交易号是 purchaseToken 的 SHA-256:orderId 带点号,不合商城的字符集,测试购买还可能没有 orderId。

const (
	googleAPIBase  = "https://androidpublisher.googleapis.com"
	googleTokenURL = "https://oauth2.googleapis.com/token"
	googleCertsURL = "https://www.googleapis.com/oauth2/v3/certs"
	googleScope    = "https://www.googleapis.com/auth/androidpublisher"
)

type GoogleConfig struct {
	PackageName                 string
	ClientEmail                 string          // 服务账号邮箱(服务账号 JSON 的 client_email)
	PrivateKey                  *rsa.PrivateKey // 服务账号私钥(服务账号 JSON 的 private_key)
	PushAudience                string          // Pub/Sub 推送订阅上配置的受众
	PushServiceAccount          string          // Pub/Sub 推送用的服务账号邮箱
	APIBase, TokenURL, CertsURL string          // 缺省正式地址;只有用例改
	HTTP                        *http.Client
	Now                         func() time.Time
}

type Google struct {
	cfg GoogleConfig

	tokenMu     sync.Mutex
	accessToken string
	tokenExpiry time.Time

	certsMu     sync.Mutex
	certs       map[string]*rsa.PublicKey
	certsExpiry time.Time
}

func NewGoogle(cfg GoogleConfig) (*Google, error) {
	switch {
	case cfg.PackageName == "" || cfg.ClientEmail == "" || cfg.PrivateKey == nil:
		return nil, errors.New("payment channel: google needs PackageName and the service account (client_email, private_key)")
	case cfg.PushAudience == "" || cfg.PushServiceAccount == "":
		return nil, errors.New("payment channel: google needs the Pub/Sub push audience and service account to verify notifications")
	}
	if cfg.APIBase == "" {
		cfg.APIBase = googleAPIBase
	}
	if cfg.TokenURL == "" {
		cfg.TokenURL = googleTokenURL
	}
	if cfg.CertsURL == "" {
		cfg.CertsURL = googleCertsURL
	}
	if cfg.HTTP == nil {
		cfg.HTTP = newHTTPClient()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Google{cfg: cfg}, nil
}

func (g *Google) ID() paymentpb.PayChannel { return paymentpb.PayChannel_PAY_CHANNEL_GOOGLE }

func (g *Google) Name() string { return NameGoogle }

// Supports:价格由 Google Play 按地区定,订单币种不限;报给商城的金额取订单冻结值。
func (g *Google) Supports(currency string) bool { return currency != "" }

func (g *Google) NeedsProduct() bool { return true }

// GoogleTransactionID 是一笔购买报给商城的交易号。
func GoogleTransactionID(purchaseToken string) string {
	sum := sha256.Sum256([]byte(purchaseToken))
	return hex.EncodeToString(sum[:])
}

// AccountRef 是买家玩家号的单向派生值(64 个十六进制字符,正好是 obfuscatedExternalAccountId 的上限)。
func (g *Google) AccountRef(player uint64) string {
	sum := sha256.Sum256([]byte("account:" + strconv.FormatUint(player, 10)))
	return hex.EncodeToString(sum[:])
}

func (g *Google) Prepare(_ context.Context, order Order, _ Ref) (Prepared, error) {
	if order.ProductID == "" {
		return Prepared{}, errors.New("payment channel: google needs a product id for the order")
	}
	return Prepared{Params: ClientParams{Google: &paymentpb.GooglePlayParams{ProductId: order.ProductID,
		ObfuscatedAccountId: g.AccountRef(order.PlayerID), ObfuscatedProfileId: order.OrderID}}}, nil
}

func (g *Google) Query(context.Context, QueryRequest) (QueryResult, error) {
	return QueryResult{}, ErrQueryUnsupported
}

func (g *Google) Close(context.Context, string) error { return nil }

// token 是 androidpublisher 的访问令牌,过期前 5 分钟换新。
func (g *Google) token(ctx context.Context) (string, error) {
	g.tokenMu.Lock()
	defer g.tokenMu.Unlock()
	now := g.cfg.Now()
	if g.accessToken != "" && now.Before(g.tokenExpiry.Add(-5*time.Minute)) {
		return g.accessToken, nil
	}
	assertion, err := jwtSign(map[string]string{"alg": "RS256", "typ": "JWT"},
		map[string]any{"iss": g.cfg.ClientEmail, "scope": googleScope, "aud": g.cfg.TokenURL, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()},
		signRS256(g.cfg.PrivateKey))
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {assertion}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, g.cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := g.cfg.HTTP.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	raw, err := readBody(response)
	if err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK {
		return "", &CallError{Channel: NameGoogle, Op: "token", Status: response.StatusCode}
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.AccessToken == "" || out.ExpiresIn <= 0 {
		return "", errors.New("payment channel: google token answer is not usable")
	}
	g.accessToken, g.tokenExpiry = out.AccessToken, now.Add(time.Duration(out.ExpiresIn)*time.Second)
	return g.accessToken, nil
}

func (g *Google) api(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	token, err := g.token(ctx)
	if err != nil {
		return 0, nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, g.cfg.APIBase+"/androidpublisher/v3/applications/"+url.PathEscape(g.cfg.PackageName)+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := g.cfg.HTTP.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	raw, err := readBody(response)
	return response.StatusCode, raw, err
}

// googlePurchase 是 ProductPurchaseV2 里本服务用到的字段。
type googlePurchase struct {
	ProductLineItem []struct {
		ProductID           string `json:"productId"`
		ProductOfferDetails struct {
			Quantity           int64  `json:"quantity"`
			RefundableQuantity int64  `json:"refundableQuantity"`
			ConsumptionState   string `json:"consumptionState"`
		} `json:"productOfferDetails"`
	} `json:"productLineItem"`
	PurchaseStateContext struct {
		PurchaseState string `json:"purchaseState"`
	} `json:"purchaseStateContext"`
	TestPurchaseContext         *json.RawMessage `json:"testPurchaseContext"`
	OrderID                     string           `json:"orderId"`
	ObfuscatedExternalAccountID string           `json:"obfuscatedExternalAccountId"`
	ObfuscatedExternalProfileID string           `json:"obfuscatedExternalProfileId"`
	PurchaseCompletionTime      string           `json:"purchaseCompletionTime"`
}

func (g *Google) purchase(ctx context.Context, purchaseToken string) (*googlePurchase, int, error) {
	if purchaseToken == "" || len(purchaseToken) > 1024 {
		return nil, 0, errors.New("payment channel: google purchase token is empty or too long")
	}
	status, raw, err := g.api(ctx, http.MethodGet, "/purchases/productsv2/tokens/"+url.PathEscape(purchaseToken), nil)
	if err != nil {
		return nil, 0, err
	}
	if status != http.StatusOK {
		return nil, status, &CallError{Channel: NameGoogle, Op: "purchase", Status: status}
	}
	var out googlePurchase
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, status, errors.New("payment channel: google purchase answer is not readable")
	}
	return &out, status, nil
}

// Verify 按购买令牌向 Google Play 核实。令牌查不到(4xx)按不该记账处理,不消耗,三天后 Google 自动退款。
func (g *Google) Verify(ctx context.Context, receipt Receipt) (QueryResult, error) {
	bought, status, err := g.purchase(ctx, receipt.PurchaseToken)
	if err != nil {
		if status == http.StatusNotFound || status == http.StatusBadRequest || status == http.StatusGone {
			return QueryResult{State: TradeClosed}, nil
		}
		return QueryResult{}, err
	}
	if len(bought.ProductLineItem) != 1 {
		return QueryResult{}, errors.New("payment channel: google purchase does not carry exactly one product")
	}
	line := bought.ProductLineItem[0]
	if receipt.ProductID != "" && line.ProductID != receipt.ProductID {
		return QueryResult{State: TradeClosed}, nil
	}
	payment := &Payment{TransactionID: GoogleTransactionID(receipt.PurchaseToken), OrderID: strings.ToLower(bought.ObfuscatedExternalProfileID),
		PaidAt: g.cfg.Now(), ProductID: line.ProductID, StorePriced: true, ChannelToken: receipt.PurchaseToken, ChannelOrderID: bought.OrderID,
		NeedsFinalize: line.ProductOfferDetails.ConsumptionState != "CONSUMPTION_STATE_CONSUMED", Test: bought.TestPurchaseContext != nil,
		AccountRef: bought.ObfuscatedExternalAccountID}
	if bought.PurchaseCompletionTime != "" {
		if at, err := time.Parse(time.RFC3339Nano, bought.PurchaseCompletionTime); err == nil {
			payment.PaidAt = at
		}
	}
	if quantity := line.ProductOfferDetails.Quantity; quantity > 1 {
		payment.Anomaly = "google purchase quantity is " + strconv.FormatInt(quantity, 10)
	}
	if payment.OrderID == "" {
		payment.Anomaly = "google purchase carries no obfuscatedExternalProfileId"
	}
	switch bought.PurchaseStateContext.PurchaseState {
	case "PURCHASED":
		details := line.ProductOfferDetails
		if details.Quantity > 0 && details.RefundableQuantity < details.Quantity {
			return QueryResult{State: TradeRefundedOutside, Payment: payment}, nil // 已经有一部分被退了
		}
		return QueryResult{State: TradePaid, Payment: payment}, nil
	case "PENDING":
		return QueryResult{State: TradeNotPaid}, nil // 待付款(如现金支付),付完会再来通知
	case "CANCELLED":
		return QueryResult{State: TradeClosed, Payment: payment}, nil
	default:
		return QueryResult{}, errors.New("payment channel: google purchase has an unknown purchaseState")
	}
}

// Finalize 消耗一笔已记账的购买。已经消耗过的再消耗会回错:回查一次,确实已消耗就算完成。
func (g *Google) Finalize(ctx context.Context, productID, purchaseToken string) error {
	status, _, err := g.api(ctx, http.MethodPost, "/purchases/products/"+url.PathEscape(productID)+"/tokens/"+url.PathEscape(purchaseToken)+":consume", nil)
	if err != nil {
		return err
	}
	if status/100 == 2 {
		return nil
	}
	bought, _, err := g.purchase(ctx, purchaseToken)
	if err == nil && len(bought.ProductLineItem) == 1 && bought.ProductLineItem[0].ProductOfferDetails.ConsumptionState == "CONSUMPTION_STATE_CONSUMED" {
		return nil
	}
	return &CallError{Channel: NameGoogle, Op: "consume", Status: status}
}

// Refund 按渠道订单号退款(revoke=false:消耗型商品的扣回由本服务经商城负责,不交给 Google 收回权益)。
// 退款接口不按我们的退款单号去重:发起前先查购买状态,已经整笔退掉就沿用,不再发起。
func (g *Google) Refund(ctx context.Context, req RefundRequest) (RefundResult, error) {
	if req.ChannelOrderID == "" {
		return RefundResult{State: RefundFailed, Detail: "NO_ORDER_ID"}, nil // 测试购买没有订单号,只能在 Play 管理中心处理
	}
	if bought, _, err := g.purchase(ctx, req.ChannelToken); err == nil && bought.PurchaseStateContext.PurchaseState == "CANCELLED" {
		return RefundResult{State: RefundSucceeded, RefundID: req.OutRefundNo, At: g.cfg.Now()}, nil
	}
	status, _, err := g.api(ctx, http.MethodPost, "/orders/"+url.PathEscape(req.ChannelOrderID)+":refund?revoke=false", nil)
	if err != nil {
		return RefundResult{}, err
	}
	switch {
	case status/100 == 2:
		return RefundResult{State: RefundSucceeded, RefundID: req.OutRefundNo, At: g.cfg.Now()}, nil
	case status == http.StatusTooManyRequests || status >= 500 || status == http.StatusUnauthorized:
		return RefundResult{}, &CallError{Channel: NameGoogle, Op: "refund", Status: status}
	default:
		return RefundResult{State: RefundFailed, Detail: "HTTP_" + strconv.Itoa(status)}, nil
	}
}

// ---------------------------------------------------------------------------
// 实时通知(Pub/Sub 推送)
// ---------------------------------------------------------------------------

// verifyPushToken 验推送请求带的 OIDC 令牌:RS256、Google 签发、受众与推送服务账号都对、未过期。
func (g *Google) verifyPushToken(ctx context.Context, authorization string) error {
	token, ok := strings.CutPrefix(authorization, "Bearer ")
	if !ok || token == "" {
		return errors.New("payment channel: google push carries no bearer token")
	}
	parts, err := splitJWS(token)
	if err != nil {
		return err
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(parts.header, &header); err != nil || header.Alg != "RS256" || header.Kid == "" {
		return errors.New("payment channel: google push token is not RS256 with a key id")
	}
	key, err := g.certKey(ctx, header.Kid)
	if err != nil {
		return err
	}
	if err := verifyRS256(key, parts.signingInput, parts.signature); err != nil {
		return err
	}
	var claims struct {
		Iss           string `json:"iss"`
		Aud           string `json:"aud"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Exp           int64  `json:"exp"`
	}
	if err := json.Unmarshal(parts.payload, &claims); err != nil {
		return errors.New("payment channel: google push token claims are not readable")
	}
	switch {
	case claims.Iss != "https://accounts.google.com" && claims.Iss != "accounts.google.com":
		return errors.New("payment channel: google push token has a foreign issuer")
	case claims.Aud != g.cfg.PushAudience:
		return errors.New("payment channel: google push token audience differs")
	case claims.Email != g.cfg.PushServiceAccount || !claims.EmailVerified:
		return errors.New("payment channel: google push token is not from the push service account")
	case g.cfg.Now().Unix() >= claims.Exp:
		return errors.New("payment channel: google push token has expired")
	}
	return nil
}

// certKey 取 Google 的 OIDC 签名公钥。缓存一小时;遇到不认识的 kid 立即重拉一次(Google 会轮换密钥)。
func (g *Google) certKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	g.certsMu.Lock()
	defer g.certsMu.Unlock()
	if key, ok := g.certs[kid]; ok && g.cfg.Now().Before(g.certsExpiry) {
		return key, nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, g.cfg.CertsURL, nil)
	if err != nil {
		return nil, err
	}
	response, err := g.cfg.HTTP.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	raw, err := readBody(response)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, &CallError{Channel: NameGoogle, Op: "certs", Status: response.StatusCode}
	}
	var set struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			Alg string `json:"alg"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil, errors.New("payment channel: google certs are not readable")
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, errN := base64.RawURLEncoding.DecodeString(k.N)
		e, errE := base64.RawURLEncoding.DecodeString(k.E)
		if errN != nil || errE != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		exponent := 0
		for _, b := range e {
			exponent = exponent<<8 | int(b)
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exponent}
	}
	g.certs, g.certsExpiry = keys, g.cfg.Now().Add(time.Hour)
	key, ok := keys[kid]
	if !ok {
		return nil, errors.New("payment channel: google push token is signed by an unknown key")
	}
	return key, nil
}

// googleNotification 是 DeveloperNotification 里本服务用到的部分。
type googleNotification struct {
	PackageName     string `json:"packageName"`
	EventTimeMillis string `json:"eventTimeMillis"`
	OneTimeProduct  *struct {
		NotificationType int    `json:"notificationType"`
		PurchaseToken    string `json:"purchaseToken"`
		Sku              string `json:"sku"`
	} `json:"oneTimeProductNotification"`
	VoidedPurchase *struct {
		PurchaseToken string `json:"purchaseToken"`
		OrderID       string `json:"orderId"`
		ProductType   int    `json:"productType"`
		RefundType    int    `json:"refundType"`
	} `json:"voidedPurchaseNotification"`
}

func (g *Google) ParseNotification(r *http.Request, body []byte) (Notification, error) {
	if err := g.verifyPushToken(r.Context(), r.Header.Get("Authorization")); err != nil {
		return Notification{}, ErrBadSignature
	}
	var envelope struct {
		Message struct {
			Data string `json:"data"`
		} `json:"message"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return Notification{}, ErrBadSignature
	}
	decoded, err := base64.StdEncoding.DecodeString(envelope.Message.Data)
	if err != nil {
		return Notification{}, ErrBadSignature
	}
	var note googleNotification
	if err := json.Unmarshal(decoded, &note); err != nil || note.PackageName != g.cfg.PackageName {
		return Notification{}, ErrBadSignature
	}
	at := g.cfg.Now()
	if millis, err := strconv.ParseInt(note.EventTimeMillis, 10, 64); err == nil && millis > 0 {
		at = time.UnixMilli(millis)
	}
	switch {
	case note.OneTimeProduct != nil && note.OneTimeProduct.PurchaseToken != "":
		// 1 = 购买完成,2 = 待付款的购买被取消:都回查接口拿全量状态。
		return Notification{Receipts: []Receipt{{PurchaseToken: note.OneTimeProduct.PurchaseToken, ProductID: note.OneTimeProduct.Sku}}}, nil
	case note.VoidedPurchase != nil && note.VoidedPurchase.PurchaseToken != "" && note.VoidedPurchase.ProductType == 2:
		return Notification{Revocations: []Revocation{{TransactionID: GoogleTransactionID(note.VoidedPurchase.PurchaseToken), Revoked: true,
			ObservedAt: at, Detail: "VOIDED_REFUND_TYPE_" + strconv.Itoa(note.VoidedPurchase.RefundType)}}}, nil
	default:
		return Notification{}, nil // 测试通知、订阅通知、退款审核通知:验过签,不处理
	}
}

// PollHistory 拉作废交易列表补漏(Google 的作废列表最早只到 30 天前)。
func (g *Google) PollHistory(ctx context.Context, since time.Time) ([]Notification, time.Time, error) {
	now := g.cfg.Now()
	floor := now.Add(-29 * 24 * time.Hour)
	if since.Before(floor) {
		since = floor
	}
	until := now.Add(-time.Minute)
	if !until.After(since) {
		return nil, since, nil
	}
	var out []Notification
	pageToken := ""
	for page := 0; page < 50; page++ {
		query := url.Values{"startTime": {strconv.FormatInt(since.UnixMilli(), 10)}, "endTime": {strconv.FormatInt(until.UnixMilli(), 10)},
			"maxResults": {"1000"}, "type": {"0"}}
		if pageToken != "" {
			query = url.Values{"token": {pageToken}, "maxResults": {"1000"}}
		}
		status, raw, err := g.api(ctx, http.MethodGet, "/purchases/voidedpurchases?"+query.Encode(), nil)
		if err != nil {
			return out, since, err
		}
		if status != http.StatusOK {
			return out, since, &CallError{Channel: NameGoogle, Op: "voided_purchases", Status: status}
		}
		var list struct {
			VoidedPurchases []struct {
				PurchaseToken    string `json:"purchaseToken"`
				VoidedTimeMillis string `json:"voidedTimeMillis"`
			} `json:"voidedPurchases"`
			TokenPagination struct {
				NextPageToken string `json:"nextPageToken"`
			} `json:"tokenPagination"`
		}
		if err := json.Unmarshal(raw, &list); err != nil {
			return out, since, errors.New("payment channel: google voided purchases answer is not readable")
		}
		for _, voided := range list.VoidedPurchases {
			at := until
			if millis, err := strconv.ParseInt(voided.VoidedTimeMillis, 10, 64); err == nil && millis > 0 {
				at = time.UnixMilli(millis)
			}
			out = append(out, Notification{Revocations: []Revocation{{TransactionID: GoogleTransactionID(voided.PurchaseToken), Revoked: true, ObservedAt: at, Detail: "VOIDED"}}})
		}
		if list.TokenPagination.NextPageToken == "" {
			return out, until, nil
		}
		pageToken = list.TokenPagination.NextPageToken
	}
	return out, since, errors.New("payment channel: google voided purchases have too many pages; will retry")
}

// WriteAck:Pub/Sub 推送收到 2xx 才算确认,其余按退避重投。
func (g *Google) WriteAck(w http.ResponseWriter, kind AckKind) {
	if kind == AckOK {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusInternalServerError)
}
