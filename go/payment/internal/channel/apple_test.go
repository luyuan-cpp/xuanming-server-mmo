package channel

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	appleBundleID        = "com.mmorpg.game.test"
	appleAppID     int64 = 6400000001
	appleIssuer          = "11111111-2222-3333-4444-555555555555"
	appleKeyID           = "KEYID12345"
	appleProductID       = "diamond_60"
	appleTxnID           = "2000000123456789"
)

var (
	oidAppleLeaf         = asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 6, 11, 1}
	oidAppleIntermediate = asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 6, 2, 1}
)

// applePKI 是一套现场生成的三级 ECDSA P-256 证书链(根 → 中间 → 叶子),不落盘、不依赖真实 Apple 证书。
type applePKI struct {
	rootCert *x509.Certificate
	inter    *x509.Certificate
	leaf     *x509.Certificate
	leafKey  *ecdsa.PrivateKey
	x5c      []string
}

type appleCertOpts struct {
	omitLeafOID  bool
	omitInterOID bool
	interNotCA   bool
	leafExpired  bool
}

func newApplePKI(t *testing.T, opts appleCertOpts) applePKI {
	t.Helper()
	genKey := func() *ecdsa.PrivateKey {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatalf("generate EC key: %v", err)
		}
		return k
	}
	createCert := func(tmpl, parent *x509.Certificate, pub *ecdsa.PublicKey, signer *ecdsa.PrivateKey) *x509.Certificate {
		der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, signer)
		if err != nil {
			t.Fatalf("CreateCertificate: %v", err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatalf("ParseCertificate: %v", err)
		}
		return cert
	}

	rootKey, interKey, leafKey := genKey(), genKey(), genKey()
	notBefore, notAfter := testNow.Add(-24*time.Hour), testNow.Add(24*time.Hour)
	rootTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Test Apple Root"},
		NotBefore: notBefore, NotAfter: notAfter,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, BasicConstraintsValid: true, IsCA: true,
	}
	rootCert := createCert(rootTmpl, rootTmpl, &rootKey.PublicKey, rootKey)

	interTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "Test Apple Intermediate"},
		NotBefore: notBefore, NotAfter: notAfter,
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: !opts.interNotCA,
	}
	if !opts.omitInterOID {
		interTmpl.ExtraExtensions = []pkix.Extension{{Id: oidAppleIntermediate, Value: []byte{0x05, 0x00}}}
	}
	interCert := createCert(interTmpl, rootCert, &interKey.PublicKey, rootKey)

	leafNotAfter := notAfter
	if opts.leafExpired {
		leafNotAfter = testNow.Add(-time.Hour)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "Test Apple Leaf"},
		NotBefore: notBefore, NotAfter: leafNotAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, BasicConstraintsValid: true,
	}
	if !opts.omitLeafOID {
		leafTmpl.ExtraExtensions = []pkix.Extension{{Id: oidAppleLeaf, Value: []byte{0x05, 0x00}}}
	}
	leafCert := createCert(leafTmpl, interCert, &leafKey.PublicKey, interKey)

	return applePKI{
		rootCert: rootCert, inter: interCert, leaf: leafCert, leafKey: leafKey,
		x5c: []string{
			base64.StdEncoding.EncodeToString(leafCert.Raw),
			base64.StdEncoding.EncodeToString(interCert.Raw),
			base64.StdEncoding.EncodeToString(rootCert.Raw),
		},
	}
}

func (p applePKI) signJWS(t *testing.T, payload any) string {
	t.Helper()
	tok, err := jwtSign(map[string]any{"alg": "ES256", "x5c": p.x5c}, payload, signES256(p.leafKey))
	if err != nil {
		t.Fatalf("sign apple JWS: %v", err)
	}
	return tok
}

type appleFixture struct {
	channel *Apple
	apiKey  *ecdsa.PrivateKey
	pki     applePKI
}

func newAppleFixture(t *testing.T, allowSandbox bool, prodAPI, sandboxAPI string, client *http.Client) appleFixture {
	t.Helper()
	apiKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate API key: %v", err)
	}
	pki := newApplePKI(t, appleCertOpts{})
	ch, err := NewApple(AppleConfig{
		BundleID: appleBundleID, AppAppleID: appleAppID, IssuerID: appleIssuer, KeyID: appleKeyID,
		PrivateKey: apiKey, RootCertificates: []*x509.Certificate{pki.rootCert},
		AllowSandbox: allowSandbox, ProductionAPI: prodAPI, SandboxAPI: sandboxAPI,
		HTTP: client, Now: fixedNow,
	})
	if err != nil {
		t.Fatalf("NewApple: %v", err)
	}
	return appleFixture{channel: ch, apiKey: apiKey, pki: pki}
}

func sampleAppleTxn(env string) appleTransaction {
	return appleTransaction{
		TransactionID: appleTxnID, BundleID: appleBundleID, ProductID: appleProductID,
		PurchaseDate: testPaidAt.UnixMilli(), Quantity: 1, Type: "Consumable",
		AppAccountToken: strings.ToUpper(testOrderID), Environment: env,
		Storefront: "CHN", Price: 6000, Currency: "CNY", SignedDate: testNow.UnixMilli(),
	}
}

func checkAppleAPIAuth(t *testing.T, apiPub *ecdsa.PublicKey, authorization string) {
	t.Helper()
	raw, ok := strings.CutPrefix(authorization, "Bearer ")
	if !ok || raw == "" {
		t.Errorf("missing Bearer JWT in Authorization header")
		return
	}
	parts, err := splitJWS(raw)
	if err != nil {
		t.Errorf("split API JWT: %v", err)
		return
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
		Typ string `json:"typ"`
	}
	if json.Unmarshal(parts.header, &hdr) != nil || hdr.Alg != "ES256" || hdr.Kid != appleKeyID || hdr.Typ != "JWT" {
		t.Errorf("unexpected API JWT header: %s", parts.header)
	}
	if err := verifyES256(apiPub, parts.signingInput, parts.signature); err != nil {
		t.Errorf("API JWT signature invalid: %v", err)
	}
	var claims struct {
		Iss string `json:"iss"`
		Aud string `json:"aud"`
		Bid string `json:"bid"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	if json.Unmarshal(parts.payload, &claims) != nil || claims.Iss != appleIssuer || claims.Aud != "appstoreconnect-v1" ||
		claims.Bid != appleBundleID || claims.Iat != testNow.Unix() || claims.Exp != testNow.Unix()+300 {
		t.Errorf("unexpected API JWT claims: %s", parts.payload)
	}
}

func TestNewAppleRejectsUnusableConfigAndFixedMethods(t *testing.T) {
	apiKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	pki := newApplePKI(t, appleCertOpts{})
	good := AppleConfig{BundleID: appleBundleID, AppAppleID: appleAppID, IssuerID: appleIssuer, KeyID: appleKeyID,
		PrivateKey: apiKey, RootCertificates: []*x509.Certificate{pki.rootCert}}
	ch, err := NewApple(good)
	if err != nil || NameOf(ch.ID()) != NameApple || ch.Name() != NameApple || !ch.NeedsProduct() || !ch.Supports("USD") || ch.Supports("") {
		t.Fatalf("NewApple(good): %v", err)
	}
	for name, edit := range map[string]func(*AppleConfig){
		"no bundle id":    func(c *AppleConfig) { c.BundleID = "" },
		"no private key":  func(c *AppleConfig) { c.PrivateKey = nil },
		"no root certs":   func(c *AppleConfig) { c.RootCertificates = nil },
		"zero appAppleId": func(c *AppleConfig) { c.AppAppleID = 0 },
	} {
		bad := good
		edit(&bad)
		if _, err := NewApple(bad); err == nil {
			t.Fatalf("NewApple accepted %s", name)
		}
	}

	ctx := context.Background()
	prep, err := ch.Prepare(ctx, Order{OrderID: testOrderID, ProductID: appleProductID}, Ref{})
	if err != nil || prep.Params.Apple == nil || prep.Params.Apple.ProductId != appleProductID || prep.Params.Apple.AppAccountToken != testOrderID {
		t.Fatalf("Prepare = %+v, %v", prep.Params.Apple, err)
	}
	if _, err := ch.Prepare(ctx, Order{OrderID: testOrderID}, Ref{}); err == nil {
		t.Fatal("Prepare accepted an order without ProductID")
	}
	if _, err := ch.Query(ctx, QueryRequest{OutTradeNo: testOutTradeNo}); !errors.Is(err, ErrQueryUnsupported) {
		t.Fatalf("Query err = %v, want ErrQueryUnsupported", err)
	}
	if err := ch.Close(ctx, testOutTradeNo); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if ref, err := ch.Refund(ctx, RefundRequest{}); err != nil || ref.State != RefundFailed || ref.Detail != "NO_REFUND_API" {
		t.Fatalf("Refund = %+v, %v", ref, err)
	}
	if ch.AccountRef(42) != "" {
		t.Fatalf("AccountRef = %q, want empty", ch.AccountRef(42))
	}
	if status, _ := ackOf(ch, AckOK); status != http.StatusOK {
		t.Fatalf("AckOK status = %d", status)
	}
	for _, kind := range []AckKind{AckRetry, AckRejected} {
		if status, _ := ackOf(ch, kind); status != http.StatusInternalServerError {
			t.Fatalf("ack %d status = %d", kind, status)
		}
	}
}

// 交易核验:正式环境通过、沙箱回退、撤销日期映射为外部退款、异常字段标记 Anomaly、伪造或链不对的 JWS 拒绝。
func TestAppleVerifyProductionSandboxRevocationAndChainChecks(t *testing.T) {
	ctx := context.Background()
	var prodHandler, sandboxHandler http.HandlerFunc
	prodSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { prodHandler(w, r) }))
	sandboxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sandboxHandler(w, r) }))
	t.Cleanup(prodSrv.Close)
	t.Cleanup(sandboxSrv.Close)

	f := newAppleFixture(t, true, prodSrv.URL, sandboxSrv.URL, prodSrv.Client())
	writeTxn := func(w http.ResponseWriter, signed string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"signedTransactionInfo":"`+signed+`"}`)
	}

	// 1. 正式环境正常交易
	prodHandler = func(w http.ResponseWriter, r *http.Request) {
		checkAppleAPIAuth(t, &f.apiKey.PublicKey, r.Header.Get("Authorization"))
		if r.URL.Path != "/inApps/v1/transactions/"+appleTxnID {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		writeTxn(w, f.pki.signJWS(t, sampleAppleTxn("Production")))
	}
	sandboxHandler = func(w http.ResponseWriter, _ *http.Request) { t.Errorf("sandbox should not be called") }

	res, err := f.channel.Verify(ctx, Receipt{TransactionID: appleTxnID})
	if err != nil || res.State != TradePaid || res.Payment == nil {
		t.Fatalf("Verify prod = %+v, %v", res, err)
	}
	want := Payment{
		TransactionID: appleTxnID, OrderID: testOrderID, PaidAt: testPaidAt,
		ProductID: appleProductID, StorePriced: true, StoreAmountMinor: 600, StoreCurrency: "CNY", Test: false,
	}
	if !samePayment(*res.Payment, want) {
		t.Fatalf("payment = %+v, want %+v", *res.Payment, want)
	}

	// 2. 正式 4040010 → 回退沙箱
	prodHandler = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"errorCode":4040010}`)
	}
	sandboxHandler = func(w http.ResponseWriter, _ *http.Request) {
		writeTxn(w, f.pki.signJWS(t, sampleAppleTxn("Sandbox")))
	}
	res, err = f.channel.Verify(ctx, Receipt{TransactionID: appleTxnID})
	if err != nil || res.State != TradePaid || res.Payment == nil || !res.Payment.Test {
		t.Fatalf("Verify sandbox fallback = %+v, %v", res, err)
	}

	// 两个环境都 4040010 → TradeClosed
	sandboxHandler = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"errorCode":4040010}`)
	}
	res, err = f.channel.Verify(ctx, Receipt{TransactionID: appleTxnID})
	if err != nil || res.State != TradeClosed || res.Payment != nil {
		t.Fatalf("Verify not found = %+v, %v", res, err)
	}

	// AllowSandbox=false 收到沙箱交易 → TradeClosed
	strict, err := NewApple(AppleConfig{
		BundleID: appleBundleID, AppAppleID: appleAppID, IssuerID: appleIssuer, KeyID: appleKeyID,
		PrivateKey: f.apiKey, RootCertificates: []*x509.Certificate{f.pki.rootCert},
		AllowSandbox: false, ProductionAPI: prodSrv.URL, SandboxAPI: sandboxSrv.URL, HTTP: prodSrv.Client(), Now: fixedNow,
	})
	if err != nil {
		t.Fatalf("NewApple strict: %v", err)
	}
	prodHandler = func(w http.ResponseWriter, _ *http.Request) {
		writeTxn(w, f.pki.signJWS(t, sampleAppleTxn("Sandbox")))
	}
	res, err = strict.Verify(ctx, Receipt{TransactionID: appleTxnID})
	if err != nil || res.State != TradeClosed {
		t.Fatalf("strict Verify sandbox txn = %+v, %v", res, err)
	}

	// 3. 带撤销日期 → TradeRefundedOutside(含 Payment)
	revoked := sampleAppleTxn("Production")
	revoked.RevocationDate = testNow.UnixMilli()
	prodHandler = func(w http.ResponseWriter, _ *http.Request) { writeTxn(w, f.pki.signJWS(t, revoked)) }
	res, err = f.channel.Verify(ctx, Receipt{TransactionID: appleTxnID})
	if err != nil || res.State != TradeRefundedOutside || res.Payment == nil || res.Payment.TransactionID != appleTxnID {
		t.Fatalf("Verify revoked = %+v, %v", res, err)
	}

	// 4. 异常交易(非消耗型 / 数量不为 1 / 缺 appAccountToken)标 Anomaly
	for name, edit := range map[string]func(*appleTransaction){
		"non-consumable":   func(x *appleTransaction) { x.Type = "Non-Consumable" },
		"quantity 2":       func(x *appleTransaction) { x.Quantity = 2 },
		"no account token": func(x *appleTransaction) { x.AppAccountToken = "" },
	} {
		txn := sampleAppleTxn("Production")
		edit(&txn)
		prodHandler = func(w http.ResponseWriter, _ *http.Request) { writeTxn(w, f.pki.signJWS(t, txn)) }
		res, err = f.channel.Verify(ctx, Receipt{TransactionID: appleTxnID})
		if err != nil || res.Payment == nil || res.Payment.Anomaly == "" {
			t.Fatalf("%s: res=%+v err=%v, want Anomaly", name, res, err)
		}
	}

	// 5. 证书链、OID、有效期、bundleId、非数字单号全部拒绝
	untrusted := newApplePKI(t, appleCertOpts{})
	noLeafOID := newApplePKI(t, appleCertOpts{omitLeafOID: true})
	noInterOID := newApplePKI(t, appleCertOpts{omitInterOID: true})
	notCA := newApplePKI(t, appleCertOpts{interNotCA: true})
	expired := newApplePKI(t, appleCertOpts{leafExpired: true})

	for name, c := range map[string]struct {
		roots  []*x509.Certificate
		signed string
	}{
		"untrusted root":    {[]*x509.Certificate{f.pki.rootCert}, untrusted.signJWS(t, sampleAppleTxn("Production"))},
		"missing leaf OID":  {[]*x509.Certificate{noLeafOID.rootCert}, noLeafOID.signJWS(t, sampleAppleTxn("Production"))},
		"missing inter OID": {[]*x509.Certificate{noInterOID.rootCert}, noInterOID.signJWS(t, sampleAppleTxn("Production"))},
		"inter not CA":      {[]*x509.Certificate{notCA.rootCert}, notCA.signJWS(t, sampleAppleTxn("Production"))},
		"expired at sign":   {[]*x509.Certificate{expired.rootCert}, expired.signJWS(t, sampleAppleTxn("Production"))},
		"two certs in x5c": {[]*x509.Certificate{f.pki.rootCert}, func() string {
			tok, _ := jwtSign(map[string]any{"alg": "ES256", "x5c": f.pki.x5c[:2]}, sampleAppleTxn("Production"), signES256(f.pki.leafKey))
			return tok
		}()},
		"wrong bundleId": {[]*x509.Certificate{f.pki.rootCert}, func() string {
			txn := sampleAppleTxn("Production")
			txn.BundleID = "com.other.app"
			return f.pki.signJWS(t, txn)
		}()},
	} {
		t.Run(name, func(t *testing.T) {
			ch, err := NewApple(AppleConfig{
				BundleID: appleBundleID, AppAppleID: appleAppID, IssuerID: appleIssuer, KeyID: appleKeyID,
				PrivateKey: f.apiKey, RootCertificates: c.roots, ProductionAPI: prodSrv.URL, HTTP: prodSrv.Client(), Now: fixedNow,
			})
			if err != nil {
				t.Fatalf("NewApple: %v", err)
			}
			prodHandler = func(w http.ResponseWriter, _ *http.Request) { writeTxn(w, c.signed) }
			if _, err := ch.Verify(ctx, Receipt{TransactionID: appleTxnID}); err == nil {
				t.Fatalf("Verify accepted %s", name)
			}
		})
	}

	if _, err := f.channel.Verify(ctx, Receipt{TransactionID: "abc-not-digits"}); err == nil {
		t.Fatal("Verify accepted non-numeric transaction id")
	}
}

// 通知 V2:ONE_TIME_CHARGE / REFUND / REFUND_REVERSED 各自映射;不带 data 的通知验过签照常应答;bundle / appAppleId / 签名不对拒绝。
func TestAppleParseNotificationV2(t *testing.T) {
	f := newAppleFixture(t, true, "", "", nil)
	wrap := func(jws string) []byte {
		raw, _ := json.Marshal(map[string]string{"signedPayload": jws})
		return raw
	}
	makeNote := func(noteType, env, bundle string, appID int64, signedTxn string) []byte {
		payload := map[string]any{
			"notificationType": noteType,
			"signedDate":       testNow.UnixMilli(),
			"data": map[string]any{
				"appAppleId":            appID,
				"bundleId":              bundle,
				"environment":           env,
				"signedTransactionInfo": signedTxn,
			},
		}
		return wrap(f.pki.signJWS(t, payload))
	}

	paidTxn := f.pki.signJWS(t, sampleAppleTxn("Production"))
	note, err := f.channel.ParseNotification(nil, makeNote("ONE_TIME_CHARGE", "Production", appleBundleID, appleAppID, paidTxn))
	if err != nil || note.Payment == nil || note.Payment.TransactionID != appleTxnID || len(note.Revocations) != 0 {
		t.Fatalf("ONE_TIME_CHARGE = %+v, %v", note, err)
	}

	revoked := sampleAppleTxn("Production")
	revoked.RevocationDate = testNow.UnixMilli()
	revokedTxn := f.pki.signJWS(t, revoked)

	// ONE_TIME_CHARGE 携带已撤销交易不记成已付
	note, err = f.channel.ParseNotification(nil, makeNote("ONE_TIME_CHARGE", "Production", appleBundleID, appleAppID, revokedTxn))
	if err != nil || note.Payment != nil || len(note.Revocations) != 0 {
		t.Fatalf("revoked ONE_TIME_CHARGE = %+v, %v", note, err)
	}

	note, err = f.channel.ParseNotification(nil, makeNote("REFUND", "Production", appleBundleID, appleAppID, revokedTxn))
	if err != nil || note.Payment != nil || len(note.Revocations) != 1 || !note.Revocations[0].Revoked ||
		note.Revocations[0].TransactionID != appleTxnID || note.Revocations[0].Detail != "REFUND" || note.Revocations[0].Payment == nil {
		t.Fatalf("REFUND = %+v, %v", note, err)
	}

	note, err = f.channel.ParseNotification(nil, makeNote("REFUND_REVERSED", "Production", appleBundleID, appleAppID, paidTxn))
	if err != nil || len(note.Revocations) != 1 || note.Revocations[0].Revoked ||
		note.Revocations[0].Detail != "REFUND_REVERSED" || note.Revocations[0].Payment == nil {
		t.Fatalf("REFUND_REVERSED = %+v, %v", note, err)
	}

	// 不带 data 的通知(如 SUMMARY)验过签照常应答,不算验签失败
	noData := wrap(f.pki.signJWS(t, map[string]any{"notificationType": "RENEWAL_EXTENSION", "subtype": "SUMMARY", "signedDate": testNow.UnixMilli()}))
	if note, err := f.channel.ParseNotification(nil, noData); err != nil || note.Payment != nil || len(note.Revocations) != 0 {
		t.Fatalf("notification without data = %+v, %v; want nil error and empty notification", note, err)
	}

	// 其他类型(含 TEST / CONSUMPTION_REQUEST)验过签照常应答
	if note, err := f.channel.ParseNotification(nil, makeNote("TEST", "Production", appleBundleID, appleAppID, "")); err != nil || note.Payment != nil {
		t.Fatalf("TEST notification = %+v, %v", note, err)
	}

	// 拒绝情形:签名不对、bundle 不符、正式环境 appAppleId 不符、内层交易 JWS 伪造
	untrusted := newApplePKI(t, appleCertOpts{})
	for name, body := range map[string][]byte{
		"wrong bundle":      makeNote("ONE_TIME_CHARGE", "Production", "com.other.app", appleAppID, paidTxn),
		"wrong appAppleId":  makeNote("ONE_TIME_CHARGE", "Production", appleBundleID, appleAppID+1, paidTxn),
		"untrusted outer":   wrap(untrusted.signJWS(t, map[string]any{"notificationType": "TEST", "signedDate": testNow.UnixMilli()})),
		"untrusted inner":   makeNote("ONE_TIME_CHARGE", "Production", appleBundleID, appleAppID, untrusted.signJWS(t, sampleAppleTxn("Production"))),
		"missing signedTxn": makeNote("ONE_TIME_CHARGE", "Production", appleBundleID, appleAppID, ""),
		"bad JSON envelope": []byte(`{}`),
	} {
		if _, err := f.channel.ParseNotification(nil, body); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("%s: err = %v, want ErrBadSignature", name, err)
		}
	}
}

// 通知历史补拉:起点早于 179 天时抬到 179 天内;单窗超过 50 页时窗口减半再拉并推进游标到减半后的时刻。
func TestApplePollHistoryClampsTo179DaysAndHalvesOversizedWindows(t *testing.T) {
	ctx := context.Background()
	var handler http.HandlerFunc
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler(w, r) }))
	t.Cleanup(srv.Close)

	f := newAppleFixture(t, false, srv.URL, "", srv.Client())
	paidTxn := f.pki.signJWS(t, sampleAppleTxn("Production"))
	noteJWS := f.pki.signJWS(t, map[string]any{
		"notificationType": "ONE_TIME_CHARGE",
		"signedDate":       testNow.UnixMilli(),
		"data": map[string]any{
			"appAppleId": appleAppID, "bundleId": appleBundleID, "environment": "Production", "signedTransactionInfo": paidTxn,
		},
	})
	noDataJWS := f.pki.signJWS(t, map[string]any{"notificationType": "RENEWAL_EXTENSION", "signedDate": testNow.UnixMilli()})

	// 1. 起点 200 天前 → 被抬到 now - 179 天;两页翻完,忽略不带 data 的通知。
	var gotStart int64
	pageCalls := 0
	handler = func(w http.ResponseWriter, r *http.Request) {
		checkAppleAPIAuth(t, &f.apiKey.PublicKey, r.Header.Get("Authorization"))
		var req struct {
			StartDate int64 `json:"startDate"`
			EndDate   int64 `json:"endDate"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode history request: %v", err)
		}
		gotStart = req.StartDate
		pageCalls++
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("paginationToken") == "" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"notificationHistory": []map[string]string{{"signedPayload": noDataJWS}},
				"hasMore":             true,
				"paginationToken":     "page-2",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"notificationHistory": []map[string]string{{"signedPayload": noteJWS}},
			"hasMore":             false,
		})
	}
	notes, next, err := f.channel.PollHistory(ctx, testNow.Add(-200*24*time.Hour))
	if err != nil || len(notes) != 1 || notes[0].Payment == nil || notes[0].Payment.TransactionID != appleTxnID {
		t.Fatalf("PollHistory 200d = %d notes, %v", len(notes), err)
	}
	wantFloor := testNow.Add(-appleHistoryKept)
	if gotStart != wantFloor.UnixMilli() {
		t.Fatalf("startDate = %d, want 179d floor %d", gotStart, wantFloor.UnixMilli())
	}
	if !next.Equal(testNow.Add(-time.Minute)) || pageCalls != 2 {
		t.Fatalf("next=%v calls=%d", next, pageCalls)
	}

	// 2. 窗口过大(持续 hasMore 超过 50 页)→ 窗口减半后拉完,游标推进到减半后的时刻。
	since := testNow.Add(-61 * time.Minute) // 初始窗口 [since, now-1m) = 60 分钟;减半后 = 30 分钟
	var seenWindows []int64
	handler = func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			StartDate int64 `json:"startDate"`
			EndDate   int64 `json:"endDate"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		spanMs := req.EndDate - req.StartDate
		if r.URL.Query().Get("paginationToken") == "" {
			seenWindows = append(seenWindows, spanMs)
		}
		w.Header().Set("Content-Type", "application/json")
		if spanMs > int64(30*time.Minute/time.Millisecond) {
			tok := r.URL.Query().Get("paginationToken")
			nextNum := 1
			if tok != "" {
				n, _ := strconv.Atoi(tok)
				nextNum = n + 1
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"notificationHistory": []map[string]string{{"signedPayload": noteJWS}},
				"hasMore":             true,
				"paginationToken":     strconv.Itoa(nextNum),
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"notificationHistory": []map[string]string{{"signedPayload": noteJWS}},
			"hasMore":             false,
		})
	}
	notes, next, err = f.channel.PollHistory(ctx, since)
	if err != nil || len(notes) != 1 {
		t.Fatalf("PollHistory halved = %d notes, %v", len(notes), err)
	}
	wantNext := since.Add(30 * time.Minute)
	if !next.Equal(wantNext) {
		t.Fatalf("halved cursor = %v, want %v", next, wantNext)
	}
	if len(seenWindows) != 2 || seenWindows[0] != int64(60*time.Minute/time.Millisecond) || seenWindows[1] != int64(30*time.Minute/time.Millisecond) {
		t.Fatalf("seen window spans (ms) = %v", seenWindows)
	}
}

func TestAppleMilliunitsToMinorHandlesZeroTwoAndThreeDecimalCurrencies(t *testing.T) {
	for _, c := range []struct {
		currency string
		price    int64
		want     uint64
	}{
		{"JPY", 1000_000, 1000},
		{"krw", 5500_000, 5500},
		{"VND", 22000_000, 22000},
		{"USD", 4990, 499},
		{"CNY", 6000, 600},
		{"KWD", 1250, 1250},
		{"BHD", 500, 500},
		{"USD", 0, 0},
	} {
		if got := appleMilliunitsToMinor(c.price, c.currency); got != c.want {
			t.Errorf("appleMilliunitsToMinor(%d, %q) = %d, want %d", c.price, c.currency, got, c.want)
		}
	}
}
