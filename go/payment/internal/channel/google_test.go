package channel

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	googlePackage       = "com.mmorpg.game.android"
	googleClientEmail   = "payment@mmorpg-test.iam.gserviceaccount.com"
	googlePushAud       = "https://pay.example.test/notify/google"
	googlePushEmail     = "pubsub-push@mmorpg-test.iam.gserviceaccount.com"
	googleProductID     = "diamond_60"
	googlePurchaseToken = "gp-purchase-token-secret-0001"
	googleOrderID       = "GPA.3301-1234-5678-90123"
)

// googleJWK 把 RSA 公钥编成 Google OIDC JWKS 里的单个键。
func googleJWK(kid string, pub *rsa.PublicKey) map[string]string {
	var expBytes []byte
	for e := pub.E; e > 0; e >>= 8 {
		expBytes = append([]byte{byte(e & 0xff)}, expBytes...)
	}
	return map[string]string{
		"kid": kid, "kty": "RSA", "alg": "RS256",
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(expBytes),
	}
}

type googleServer struct {
	mu          sync.Mutex
	tokenCalls  int
	certsCalls  int
	certsStatus int
	certsBody   string
	apiHandler  func(w http.ResponseWriter, r *http.Request)
	serviceKey  *rsa.PrivateKey
	oidcKey     *rsa.PrivateKey
	server      *httptest.Server
	channel     *Google
}

func startGoogle(t *testing.T, apiHandler func(w http.ResponseWriter, r *http.Request)) *googleServer {
	t.Helper()
	serviceKey, oidcKey := testRSAKey(t, "merchant"), testRSAKey(t, "platform")
	gs := &googleServer{
		certsStatus: http.StatusOK,
		apiHandler:  apiHandler,
		serviceKey:  serviceKey,
		oidcKey:     oidcKey,
	}
	jwk, _ := json.Marshal(map[string]any{"keys": []any{googleJWK("kid-1", &oidcKey.PublicKey)}})
	gs.certsBody = string(jwk)

	gs.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			if err := r.ParseForm(); err != nil {
				t.Errorf("token parse form: %v", err)
			}
			if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
				t.Errorf("unexpected grant_type %q", r.Form.Get("grant_type"))
			}
			parts, err := splitJWS(r.Form.Get("assertion"))
			if err != nil || verifyRS256(&serviceKey.PublicKey, parts.signingInput, parts.signature) != nil {
				t.Errorf("invalid service account assertion")
			}
			var claims struct {
				Iss   string `json:"iss"`
				Scope string `json:"scope"`
				Aud   string `json:"aud"`
			}
			if json.Unmarshal(parts.payload, &claims) != nil || claims.Iss != googleClientEmail || claims.Scope != googleScope || claims.Aud != gs.server.URL+"/token" {
				t.Errorf("unexpected assertion claims: %s", parts.payload)
			}
			gs.mu.Lock()
			gs.tokenCalls++
			gs.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"access_token":"google-access-token-1","expires_in":3600}`)
		case r.URL.Path == "/certs":
			gs.mu.Lock()
			gs.certsCalls++
			status, body := gs.certsStatus, gs.certsBody
			gs.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		case strings.HasPrefix(r.URL.Path, "/androidpublisher/v3/applications/"+googlePackage):
			if r.Header.Get("Authorization") != "Bearer google-access-token-1" {
				t.Errorf("missing bearer access token on %s", r.URL.Path)
			}
			if gs.apiHandler == nil {
				t.Errorf("unexpected api call %s %s", r.Method, r.URL.RequestURI())
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			gs.apiHandler(w, r)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(gs.server.Close)

	ch, err := NewGoogle(GoogleConfig{
		PackageName: googlePackage, ClientEmail: googleClientEmail, PrivateKey: serviceKey,
		PushAudience: googlePushAud, PushServiceAccount: googlePushEmail,
		APIBase: gs.server.URL, TokenURL: gs.server.URL + "/token", CertsURL: gs.server.URL + "/certs",
		HTTP: gs.server.Client(), Now: fixedNow,
	})
	if err != nil {
		t.Fatalf("NewGoogle: %v", err)
	}
	gs.channel = ch
	return gs
}

func (gs *googleServer) pushToken(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	tok, err := jwtSign(map[string]string{"alg": "RS256", "kid": kid, "typ": "JWT"}, claims, signRS256(key))
	if err != nil {
		t.Fatalf("sign push OIDC JWT: %v", err)
	}
	return tok
}

func googlePushClaims() map[string]any {
	return map[string]any{
		"iss": "https://accounts.google.com", "aud": googlePushAud,
		"email": googlePushEmail, "email_verified": true,
		"exp": testNow.Add(10 * time.Minute).Unix(),
	}
}

func googlePubSubBody(t *testing.T, note map[string]any) []byte {
	t.Helper()
	inner, err := json.Marshal(note)
	if err != nil {
		t.Fatalf("marshal inner notification: %v", err)
	}
	env, err := json.Marshal(map[string]any{"message": map[string]string{"data": base64.StdEncoding.EncodeToString(inner)}})
	if err != nil {
		t.Fatalf("marshal pubsub envelope: %v", err)
	}
	return env
}

func googlePurchaseJSON(state, consumption string, quantity, refundable int, testPurchase bool) string {
	obj := map[string]any{
		"productLineItem": []map[string]any{{
			"productId": googleProductID,
			"productOfferDetails": map[string]any{
				"quantity": quantity, "refundableQuantity": refundable, "consumptionState": consumption,
			},
		}},
		"purchaseStateContext":        map[string]string{"purchaseState": state},
		"orderId":                     googleOrderID,
		"obfuscatedExternalAccountId": (&Google{}).AccountRef(1001),
		"obfuscatedExternalProfileId": strings.ToUpper(testOrderID),
		"purchaseCompletionTime":      testPaidAt.Format(time.RFC3339Nano),
	}
	if testPurchase {
		obj["testPurchaseContext"] = map[string]any{}
	}
	raw, _ := json.Marshal(obj)
	return string(raw)
}

func TestNewGoogleConfigAndFixedMethods(t *testing.T) {
	key := testRSAKey(t, "merchant")
	good := GoogleConfig{PackageName: googlePackage, ClientEmail: googleClientEmail, PrivateKey: key,
		PushAudience: googlePushAud, PushServiceAccount: googlePushEmail}
	ch, err := NewGoogle(good)
	if err != nil || NameOf(ch.ID()) != NameGoogle || ch.Name() != NameGoogle || !ch.NeedsProduct() || !ch.Supports("USD") || ch.Supports("") {
		t.Fatalf("NewGoogle(good): %v", err)
	}
	for name, edit := range map[string]func(*GoogleConfig){
		"no package":    func(c *GoogleConfig) { c.PackageName = "" },
		"no privateKey": func(c *GoogleConfig) { c.PrivateKey = nil },
		"no audience":   func(c *GoogleConfig) { c.PushAudience = "" },
		"no push email": func(c *GoogleConfig) { c.PushServiceAccount = "" },
	} {
		bad := good
		edit(&bad)
		if _, err := NewGoogle(bad); err == nil {
			t.Fatalf("NewGoogle accepted %s", name)
		}
	}

	ctx := context.Background()
	prep, err := ch.Prepare(ctx, Order{OrderID: testOrderID, PlayerID: 1001, ProductID: googleProductID}, Ref{})
	if err != nil || prep.Params.Google == nil || prep.Params.Google.ProductId != googleProductID ||
		prep.Params.Google.ObfuscatedProfileId != testOrderID || prep.Params.Google.ObfuscatedAccountId != ch.AccountRef(1001) {
		t.Fatalf("Prepare = %+v, %v", prep.Params.Google, err)
	}
	if _, err := ch.Query(ctx, QueryRequest{OutTradeNo: testOutTradeNo}); !errors.Is(err, ErrQueryUnsupported) {
		t.Fatalf("Query err = %v", err)
	}
	if err := ch.Close(ctx, testOutTradeNo); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if status, _ := ackOf(ch, AckOK); status != http.StatusNoContent {
		t.Fatalf("AckOK = %d", status)
	}
	for _, kind := range []AckKind{AckRetry, AckRejected} {
		if status, _ := ackOf(ch, kind); status != http.StatusInternalServerError {
			t.Fatalf("ack %d = %d", kind, status)
		}
	}
}

// Verify:PURCHASED / PENDING / CANCELLED / 部分已退 / 4xx 未找到的映射,访问令牌缓存复用,传输错不泄露 purchaseToken。
func TestGoogleVerifyMapsPurchaseStatesAndProtectsToken(t *testing.T) {
	ctx := context.Background()
	var body string
	status := http.StatusOK
	gs := startGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/purchases/productsv2/tokens/"+googlePurchaseToken) {
			t.Errorf("unexpected purchase request %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})

	// PURCHASED 未消耗
	body = googlePurchaseJSON("PURCHASED", "CONSUMPTION_STATE_YET_TO_BE_CONSUMED", 1, 1, false)
	res, err := gs.channel.Verify(ctx, Receipt{PurchaseToken: googlePurchaseToken, ProductID: googleProductID})
	if err != nil || res.State != TradePaid || res.Payment == nil {
		t.Fatalf("Verify PURCHASED = %+v, %v", res, err)
	}
	want := Payment{
		TransactionID: GoogleTransactionID(googlePurchaseToken), OrderID: testOrderID, PaidAt: testPaidAt,
		ProductID: googleProductID, StorePriced: true, ChannelToken: googlePurchaseToken, ChannelOrderID: googleOrderID,
		NeedsFinalize: true, Test: false, AccountRef: gs.channel.AccountRef(1001),
	}
	if !samePayment(*res.Payment, want) {
		t.Fatalf("payment = %+v, want %+v", *res.Payment, want)
	}

	// PURCHASED 已消耗 + 测试交易 + 可退数量减少 → TradeRefundedOutside
	body = googlePurchaseJSON("PURCHASED", "CONSUMPTION_STATE_CONSUMED", 1, 0, true)
	res, err = gs.channel.Verify(ctx, Receipt{PurchaseToken: googlePurchaseToken})
	if err != nil || res.State != TradeRefundedOutside || res.Payment == nil || res.Payment.NeedsFinalize || !res.Payment.Test {
		t.Fatalf("Verify partial refund = %+v, %v", res, err)
	}

	// PENDING → TradeNotPaid
	body = googlePurchaseJSON("PENDING", "CONSUMPTION_STATE_YET_TO_BE_CONSUMED", 1, 1, false)
	res, err = gs.channel.Verify(ctx, Receipt{PurchaseToken: googlePurchaseToken})
	if err != nil || res.State != TradeNotPaid || res.Payment != nil {
		t.Fatalf("Verify PENDING = %+v, %v", res, err)
	}

	// CANCELLED → TradeClosed(带 Payment)
	body = googlePurchaseJSON("CANCELLED", "CONSUMPTION_STATE_YET_TO_BE_CONSUMED", 1, 0, false)
	res, err = gs.channel.Verify(ctx, Receipt{PurchaseToken: googlePurchaseToken})
	if err != nil || res.State != TradeClosed || res.Payment == nil {
		t.Fatalf("Verify CANCELLED = %+v, %v", res, err)
	}

	// 商品号不符 → TradeClosed
	body = googlePurchaseJSON("PURCHASED", "CONSUMPTION_STATE_YET_TO_BE_CONSUMED", 1, 1, false)
	res, err = gs.channel.Verify(ctx, Receipt{PurchaseToken: googlePurchaseToken, ProductID: "other_sku"})
	if err != nil || res.State != TradeClosed {
		t.Fatalf("Verify mismatched product = %+v, %v", res, err)
	}

	// 404 / 410 → TradeClosed;500 → CallError
	status, body = http.StatusNotFound, `{}`
	if res, err := gs.channel.Verify(ctx, Receipt{PurchaseToken: googlePurchaseToken}); err != nil || res.State != TradeClosed {
		t.Fatalf("Verify 404 = %+v, %v", res, err)
	}
	status = http.StatusInternalServerError
	var call *CallError
	if _, err := gs.channel.Verify(ctx, Receipt{PurchaseToken: googlePurchaseToken}); !errors.As(err, &call) || call.Op != "purchase" {
		t.Fatalf("Verify 500 err = %v, want purchase CallError", err)
	}

	// 多次调用只换过一次 OAuth token
	gs.mu.Lock()
	tokenCalls := gs.tokenCalls
	gs.mu.Unlock()
	if tokenCalls != 1 {
		t.Fatalf("token endpoint called %d times, want 1", tokenCalls)
	}

	// 传输失败时不泄露 URL 里的 purchaseToken
	cause := errors.New("connection refused")
	broken, err := NewGoogle(GoogleConfig{
		PackageName: googlePackage, ClientEmail: googleClientEmail, PrivateKey: gs.serviceKey,
		PushAudience: googlePushAud, PushServiceAccount: googlePushEmail,
		APIBase: "http://google.test", TokenURL: gs.server.URL + "/token", CertsURL: gs.server.URL + "/certs",
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/token" {
				return jsonAnswer(`{"access_token":"tok","expires_in":3600}`), nil
			}
			return nil, cause
		})}, Now: fixedNow,
	})
	if err != nil {
		t.Fatalf("NewGoogle broken: %v", err)
	}
	_, err = broken.Verify(ctx, Receipt{PurchaseToken: googlePurchaseToken})
	if !errors.Is(err, cause) || strings.Contains(err.Error(), googlePurchaseToken) {
		t.Fatalf("transport error leaked purchase token or lost cause: %v", err)
	}
}

// Finalize 与 Refund:已消耗的重复 consume 视为成功;无 orderId 转人工;已 CANCELLED 直接沿用退款成功。
func TestGoogleFinalizeAndRefund(t *testing.T) {
	ctx := context.Background()
	calls := &callLog{}
	consumeStatus := http.StatusNoContent
	purchaseState, consumeState := "PURCHASED", "CONSUMPTION_STATE_CONSUMED"
	refundStatus := http.StatusNoContent
	gs := startGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		calls.add(r.Method + " " + r.URL.RequestURI())
		switch {
		case strings.HasSuffix(r.URL.Path, ":consume"):
			w.WriteHeader(consumeStatus)
		case strings.Contains(r.URL.Path, "/purchases/productsv2/tokens/"):
			_, _ = io.WriteString(w, googlePurchaseJSON(purchaseState, consumeState, 1, 1, false))
		case strings.Contains(r.URL.Path, ":refund"):
			w.WriteHeader(refundStatus)
		}
	})

	if err := gs.channel.Finalize(ctx, googleProductID, googlePurchaseToken); err != nil {
		t.Fatalf("Finalize 204: %v", err)
	}
	// 非 2xx 时回查发现已消耗也算完成
	consumeStatus = http.StatusBadRequest
	if err := gs.channel.Finalize(ctx, googleProductID, googlePurchaseToken); err != nil {
		t.Fatalf("Finalize already consumed: %v", err)
	}
	consumeState = "CONSUMPTION_STATE_YET_TO_BE_CONSUMED"
	var call *CallError
	if err := gs.channel.Finalize(ctx, googleProductID, googlePurchaseToken); !errors.As(err, &call) || call.Op != "consume" {
		t.Fatalf("Finalize failed err = %v", err)
	}

	// 无渠道订单号(测试购买)→ RefundFailed / NO_ORDER_ID
	ref, err := gs.channel.Refund(ctx, RefundRequest{OutRefundNo: "rf-1", ChannelToken: googlePurchaseToken})
	if err != nil || ref.State != RefundFailed || ref.Detail != "NO_ORDER_ID" {
		t.Fatalf("Refund without orderId = %+v, %v", ref, err)
	}
	// 正常退款带 revoke=false
	ref, err = gs.channel.Refund(ctx, RefundRequest{OutRefundNo: "rf-1", ChannelToken: googlePurchaseToken, ChannelOrderID: googleOrderID})
	if err != nil || ref.State != RefundSucceeded || ref.RefundID != "rf-1" || !ref.At.Equal(testNow) {
		t.Fatalf("Refund 204 = %+v, %v", ref, err)
	}
	if calls.count("POST /androidpublisher/v3/applications/"+googlePackage+"/orders/"+googleOrderID+":refund?revoke=false") != 1 {
		t.Fatalf("refund calls = %v", calls.list())
	}
	// 已 CANCELLED 不再调 :refund
	purchaseState = "CANCELLED"
	ref, err = gs.channel.Refund(ctx, RefundRequest{OutRefundNo: "rf-2", ChannelToken: googlePurchaseToken, ChannelOrderID: googleOrderID})
	if err != nil || ref.State != RefundSucceeded || ref.RefundID != "rf-2" {
		t.Fatalf("Refund already cancelled = %+v, %v", ref, err)
	}
}

// RTDN 通知:OIDC 验签、一次性商品线索、作废购买撤销、取不到公钥报 errGoogleCerts(不算验签失败)、密钥轮换重拉。
func TestGoogleParseNotificationOIDCAndVoidedPurchase(t *testing.T) {
	gs := startGoogle(t, nil)
	makeReq := func(tok string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/notify/google", nil)
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		return r
	}
	validTok := gs.pushToken(t, gs.oidcKey, "kid-1", googlePushClaims())

	// 1. 一次性购买通知 → Receipts
	oneTimeBody := googlePubSubBody(t, map[string]any{
		"packageName": googlePackage, "eventTimeMillis": timeUnixMilliString(testNow),
		"oneTimeProductNotification": map[string]any{"notificationType": 1, "purchaseToken": googlePurchaseToken, "sku": googleProductID},
	})
	note, err := gs.channel.ParseNotification(makeReq(validTok), oneTimeBody)
	if err != nil || len(note.Receipts) != 1 || note.Receipts[0].PurchaseToken != googlePurchaseToken || note.Receipts[0].ProductID != googleProductID {
		t.Fatalf("oneTimeProduct = %+v, %v", note, err)
	}

	// 2. 作废购买通知(productType=2 一次性商品)→ Revocations
	voidedBody := googlePubSubBody(t, map[string]any{
		"packageName": googlePackage, "eventTimeMillis": timeUnixMilliString(testPaidAt),
		"voidedPurchaseNotification": map[string]any{
			"purchaseToken": googlePurchaseToken, "orderId": googleOrderID, "productType": 2, "refundType": 1,
		},
	})
	note, err = gs.channel.ParseNotification(makeReq(validTok), voidedBody)
	if err != nil || len(note.Revocations) != 1 {
		t.Fatalf("voidedPurchase = %+v, %v", note, err)
	}
	if rev := note.Revocations[0]; rev.TransactionID != GoogleTransactionID(googlePurchaseToken) || !rev.Revoked ||
		!rev.ObservedAt.Equal(testPaidAt) || rev.Detail != "VOIDED_REFUND_TYPE_1" {
		t.Fatalf("revocation = %+v", rev)
	}

	// 3. 订阅作废(productType=1)或测试通知 → 验过签不处理
	subVoided := googlePubSubBody(t, map[string]any{
		"packageName":                googlePackage,
		"voidedPurchaseNotification": map[string]any{"purchaseToken": googlePurchaseToken, "productType": 1, "refundType": 1},
	})
	if note, err := gs.channel.ParseNotification(makeReq(validTok), subVoided); err != nil || len(note.Revocations) != 0 || len(note.Receipts) != 0 {
		t.Fatalf("subscription voided = %+v, %v", note, err)
	}

	// 3b. 距上次成功拉取未满冷却期(1 分钟)遇到未知 kid 直接判 ErrBadSignature，不再触发外网重拉（防 JWKS 刷新 DoS）
	unknownKidTok := gs.pushToken(t, gs.oidcKey, "kid-new", googlePushClaims())
	gs.mu.Lock()
	gs.certsStatus = http.StatusInternalServerError
	gs.mu.Unlock()
	if _, err := gs.channel.ParseNotification(makeReq(unknownKidTok), voidedBody); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("unknown kid within refetch cooldown: err = %v, want ErrBadSignature without refetch", err)
	}

	// 4. 超过冷却期后取不到公钥(JWKS 500)报 errGoogleCerts,不算 ErrBadSignature
	gs.channel.certsMu.Lock()
	gs.channel.certsFetchedAt = testNow.Add(-2 * time.Minute)
	gs.channel.certsMu.Unlock()
	_, err = gs.channel.ParseNotification(makeReq(unknownKidTok), voidedBody)
	if !errors.Is(err, errGoogleCerts) || errors.Is(err, ErrBadSignature) {
		t.Fatalf("certs 500: err = %v, want errGoogleCerts and not ErrBadSignature", err)
	}

	// 5. 恢复 JWKS 后仍没有这个 kid → ErrBadSignature
	gs.mu.Lock()
	gs.certsStatus = http.StatusOK
	gs.mu.Unlock()
	if _, err := gs.channel.ParseNotification(makeReq(unknownKidTok), voidedBody); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("unknown kid after refresh: err = %v, want ErrBadSignature", err)
	}

	// 6. 密钥轮换:超过冷却期后 JWKS 换上 kid-2,旧缓存未命中触发重拉并验签通过
	rotatedKey := testRSAKey(t, "merchant")
	rotatedJWK, _ := json.Marshal(map[string]any{"keys": []any{googleJWK("kid-2", &rotatedKey.PublicKey)}})
	gs.mu.Lock()
	gs.certsBody = string(rotatedJWK)
	gs.mu.Unlock()
	gs.channel.certsMu.Lock()
	gs.channel.certsFetchedAt = testNow.Add(-2 * time.Minute)
	gs.channel.certsMu.Unlock()
	rotatedTok := gs.pushToken(t, rotatedKey, "kid-2", googlePushClaims())
	if note, err := gs.channel.ParseNotification(makeReq(rotatedTok), oneTimeBody); err != nil || len(note.Receipts) != 1 {
		t.Fatalf("rotated kid = %+v, %v", note, err)
	}

	// 7. OIDC 校验失败(错误 aud / iss / email / email_verified=false / 过期 / 包名不符)一律 ErrBadSignature
	withClaims := func(edit func(map[string]any)) string {
		c := googlePushClaims()
		edit(c)
		return gs.pushToken(t, rotatedKey, "kid-2", c)
	}
	for name, req := range map[string]*http.Request{
		"no bearer":   makeReq(""),
		"wrong aud":   makeReq(withClaims(func(c map[string]any) { c["aud"] = "https://other.test" })),
		"wrong iss":   makeReq(withClaims(func(c map[string]any) { c["iss"] = "https://evil.test" })),
		"wrong email": makeReq(withClaims(func(c map[string]any) { c["email"] = "other@test" })),
		"unverified":  makeReq(withClaims(func(c map[string]any) { c["email_verified"] = false })),
		"expired":     makeReq(withClaims(func(c map[string]any) { c["exp"] = testNow.Unix() })),
	} {
		if _, err := gs.channel.ParseNotification(req, oneTimeBody); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("%s: err = %v, want ErrBadSignature", name, err)
		}
	}
	otherPkg := googlePubSubBody(t, map[string]any{"packageName": "com.other.pkg"})
	if _, err := gs.channel.ParseNotification(makeReq(rotatedTok), otherPkg); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("other package: err = %v, want ErrBadSignature", err)
	}
}

// PollHistory:作废列表翻页,起点早于 29 天时抬到 29 天前。
func TestGooglePollHistoryVoidedPurchases(t *testing.T) {
	ctx := context.Background()
	var firstStart string
	gs := startGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		if q.Get("token") == "" {
			firstStart = q.Get("startTime")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"voidedPurchases": []map[string]string{{"purchaseToken": "tok-1", "voidedTimeMillis": timeUnixMilliString(testPaidAt)}},
				"tokenPagination": map[string]string{"nextPageToken": "next-1"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"voidedPurchases": []map[string]string{{"purchaseToken": "tok-2", "voidedTimeMillis": timeUnixMilliString(testPaidAt)}},
		})
	})

	notes, next, err := gs.channel.PollHistory(ctx, testNow.Add(-60*24*time.Hour))
	if err != nil || len(notes) != 2 {
		t.Fatalf("PollHistory = %d notes, %v", len(notes), err)
	}
	wantFloor := testNow.Add(-29 * 24 * time.Hour)
	if firstStart != strings.TrimSpace(firstStart) || firstStart != string([]byte(firstStart)) {
		t.Fatalf("unexpected startTime %q", firstStart)
	}
	if got := firstStart; got != json.Number(got).String() || got != timeUnixMilliString(wantFloor) {
		t.Fatalf("startTime = %q, want 29d floor %q", got, timeUnixMilliString(wantFloor))
	}
	if !next.Equal(testNow.Add(-time.Minute)) {
		t.Fatalf("next = %v, want %v", next, testNow.Add(-time.Minute))
	}
	if notes[0].Revocations[0].TransactionID != GoogleTransactionID("tok-1") || notes[1].Revocations[0].TransactionID != GoogleTransactionID("tok-2") {
		t.Fatalf("unexpected voided transaction ids: %+v", notes)
	}
}

func timeUnixMilliString(t time.Time) string {
	raw, _ := json.Marshal(t.UnixMilli())
	return string(raw)
}
