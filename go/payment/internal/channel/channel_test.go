package channel

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 各渠道用例共用的东西
// ---------------------------------------------------------------------------

// testNow 是用例里的「现在」:各渠道的 Now 都注入它,不读真实时钟。北京时间 2026-11-20 20:00:00。
var testNow = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// testPaidAt 是用例里渠道报的付款时刻:北京时间 2026-11-20 19:59:30。
var testPaidAt = time.Date(2026, 11, 20, 11, 59, 30, 0, time.UTC)

const (
	testOrderID    = "01234567-89ab-cdef-0123-456789abcdef" // 商城订单号
	testOutTradeNo = "0123456789abcdef0123456789abcdef"     // 它对应的商户单号
)

func fixedNow() time.Time { return testNow }

var (
	testKeysMu sync.Mutex
	testKeys   = map[string]*rsa.PrivateKey{}
)

// testRSAKey 现场生成一把 2048 位 RSA 密钥并按名字缓存。整个包只用两把:merchant(我方:商户、应用、服务账号)
// 与 platform(对方:微信支付、支付宝、Google)。仓库里不落任何密钥。
func testRSAKey(t *testing.T, name string) *rsa.PrivateKey {
	t.Helper()
	testKeysMu.Lock()
	defer testKeysMu.Unlock()
	if key, ok := testKeys[name]; ok {
		return key
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key %s: %v", name, err)
	}
	testKeys[name] = key
	return key
}

// testSecret 现场生成 32 个十六进制字符的随机串,当用例里的对称密钥与令牌。
func testSecret(t *testing.T) string {
	t.Helper()
	secret, err := randomNonce()
	if err != nil {
		t.Fatalf("random secret: %v", err)
	}
	return secret
}

// roundTripFunc 让用例直接给出 HTTP 传输的行为,不开端口。
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// jsonAnswer 是 roundTripFunc 用的一个 200 应答。
func jsonAnswer(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}

// callLog 记下假渠道收到的请求:处理协程写,用例协程读。
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *callLog) add(call string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, call)
}

func (l *callLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

// count 数出以 prefix 开头的请求。
func (l *callLog) count(prefix string) int {
	n := 0
	for _, call := range l.list() {
		if strings.HasPrefix(call, prefix) {
			n++
		}
	}
	return n
}

// ackOf 取一次 WriteAck 写出的状态码与正文。
func ackOf(ch Channel, kind AckKind) (int, string) {
	recorder := httptest.NewRecorder()
	ch.WriteAck(recorder, kind)
	return recorder.Code, recorder.Body.String()
}

// tamper 把 body 里的 old 换成 replacement(必须真的换到),返回改过的副本。
func tamper(t *testing.T, body []byte, old, replacement string) []byte {
	t.Helper()
	if !bytes.Contains(body, []byte(old)) {
		t.Fatalf("tamper: %q is not in the body", old)
	}
	return bytes.Replace(body, []byte(old), []byte(replacement), 1)
}

// samePayment 逐字段比较两笔付款;时刻按同一瞬间比(时区的写法可以不同)。
func samePayment(got, want Payment) bool {
	sameInstant := got.PaidAt.Equal(want.PaidAt)
	got.PaidAt, want.PaidAt = time.Time{}, time.Time{}
	return sameInstant && got == want
}

// ---------------------------------------------------------------------------
// 本地联调渠道
// ---------------------------------------------------------------------------

func devPayRequest(signature string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/notify/devpay", nil)
	if signature != "" {
		request.Header.Set(DevPaySignatureHeader, signature)
	}
	return request
}

// 联调回调:HMAC 对了才解析;付款、同一订单的第二笔(seq)、撤销(revoke)各自映射成事实;改一个字节就拒绝。
func TestDevPayNotificationRoundTrip(t *testing.T) {
	secret := testSecret(t)
	devpay, err := NewDevPay(secret, fixedNow)
	if err != nil {
		t.Fatalf("NewDevPay: %v", err)
	}
	sign := func(body string) string { return hex.EncodeToString(DevPaySign([]byte(secret), []byte(body))) }

	paid := `{"out_trade_no":"` + testOutTradeNo + `","amount_minor":600,"currency":"CNY"}`
	note, err := devpay.ParseNotification(devPayRequest(sign(paid)), []byte(paid))
	if err != nil || note.Payment == nil || len(note.Revocations) != 0 || len(note.Receipts) != 0 {
		t.Fatalf("payment notification = %+v, %v", note, err)
	}
	want := Payment{TransactionID: "devpay-" + testOutTradeNo, OutTradeNo: testOutTradeNo, AmountMinor: 600, Currency: "CNY", PaidAt: testNow}
	if !samePayment(*note.Payment, want) {
		t.Fatalf("payment = %+v, want %+v", *note.Payment, want)
	}

	second := `{"out_trade_no":"` + testOutTradeNo + `","amount_minor":600,"currency":"CNY","seq":2}`
	note, err = devpay.ParseNotification(devPayRequest(sign(second)), []byte(second))
	if err != nil || note.Payment == nil || note.Payment.TransactionID != "devpay-"+testOutTradeNo+"-2" {
		t.Fatalf("second payment = %+v, %v", note, err)
	}

	revoke := `{"out_trade_no":"` + testOutTradeNo + `","revoke":true}`
	note, err = devpay.ParseNotification(devPayRequest(sign(revoke)), []byte(revoke))
	if err != nil || note.Payment != nil || len(note.Revocations) != 1 {
		t.Fatalf("revocation = %+v, %v", note, err)
	}
	if got := note.Revocations[0]; got.TransactionID != "devpay-"+testOutTradeNo || !got.Revoked || !got.ObservedAt.Equal(testNow) || got.Detail != "devpay" || got.Payment != nil {
		t.Fatalf("revocation = %+v", got)
	}

	for name, c := range map[string]struct{ signature, body string }{
		"body changed after signing": {sign(paid), strings.Replace(paid, "600", "601", 1)},
		"no signature header":        {"", paid},
		"signature is not hex":       {"not-hex", paid},
		"signed with another secret": {hex.EncodeToString(DevPaySign([]byte(testSecret(t)), []byte(paid))), paid},
	} {
		if _, err := devpay.ParseNotification(devPayRequest(c.signature), []byte(c.body)); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("%s: err = %v, want ErrBadSignature", name, err)
		}
	}
	// 签名对、内容不成一条通知的,同样按信封非法拒绝。
	for name, body := range map[string]string{
		"unknown field":   `{"out_trade_no":"` + testOutTradeNo + `","amount_minor":600,"currency":"CNY","extra":1}`,
		"no out_trade_no": `{"amount_minor":600,"currency":"CNY"}`,
		"zero amount":     `{"out_trade_no":"` + testOutTradeNo + `","amount_minor":0,"currency":"CNY"}`,
		"no currency":     `{"out_trade_no":"` + testOutTradeNo + `","amount_minor":600}`,
		"not JSON":        `out_trade_no=` + testOutTradeNo,
	} {
		if _, err := devpay.ParseNotification(devPayRequest(sign(body)), []byte(body)); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("%s: err = %v, want ErrBadSignature", name, err)
		}
	}
}

// 联调渠道没有外部系统:查单永远未付,关单、退款立即成功,应答三种情形各有状态码。
func TestDevPayFixedAnswers(t *testing.T) {
	if _, err := NewDevPay("too-short", nil); err == nil {
		t.Fatal("NewDevPay accepted a secret shorter than 32 bytes")
	}
	devpay, err := NewDevPay(testSecret(t), fixedNow)
	if err != nil {
		t.Fatalf("NewDevPay: %v", err)
	}
	if NameOf(devpay.ID()) != NameDevPay || devpay.Name() != NameDevPay || devpay.NeedsProduct() || !devpay.Supports("USD") || devpay.Supports("") {
		t.Fatalf("identity: id=%v name=%q", devpay.ID(), devpay.Name())
	}
	ctx := context.Background()
	prepared, err := devpay.Prepare(ctx, Order{OutTradeNo: testOutTradeNo}, Ref{})
	if err != nil || prepared.Params.DevPay == nil || prepared.Params.DevPay.OutTradeNo != testOutTradeNo || prepared.Ref.Value != "" {
		t.Fatalf("Prepare = %+v, %v", prepared.Ref, err)
	}
	if result, err := devpay.Query(ctx, QueryRequest{OutTradeNo: testOutTradeNo}); err != nil || result.State != TradeNotPaid || result.Payment != nil {
		t.Fatalf("Query = %+v, %v", result, err)
	}
	if err := devpay.Close(ctx, testOutTradeNo); err != nil {
		t.Fatalf("Close: %v", err)
	}
	refund, err := devpay.Refund(ctx, RefundRequest{OutRefundNo: "rf-1"})
	if err != nil || refund.State != RefundSucceeded || refund.RefundID != "devpay-rf-1" || !refund.At.Equal(testNow) {
		t.Fatalf("Refund = %+v, %v", refund, err)
	}
	for kind, status := range map[AckKind]int{AckOK: http.StatusOK, AckRejected: http.StatusUnauthorized, AckRetry: http.StatusServiceUnavailable} {
		if got, _ := ackOf(devpay, kind); got != status {
			t.Fatalf("ack %d = http %d, want %d", kind, got, status)
		}
	}
}

// ---------------------------------------------------------------------------
// 密钥读取与公共辅助
// ---------------------------------------------------------------------------

// 密钥读取:PEM(PKCS#8 / PKCS#1)与去掉首尾行的裸 base64 都认;类型不对、读不懂的报错。
func TestKeyParsingAcceptsPEMAndBareBase64(t *testing.T) {
	key := testRSAKey(t, "merchant")
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	pkcs1 := x509.MarshalPKCS1PrivateKey(key)
	bare := base64.StdEncoding.EncodeToString(pkcs8)
	for name, text := range map[string]string{
		"PKCS#8 PEM":           string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})),
		"PKCS#1 PEM":           "\n" + string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: pkcs1})) + "\n",
		"bare PKCS#8":          bare,
		"bare PKCS#1":          base64.StdEncoding.EncodeToString(pkcs1),
		"bare with line wraps": bare[:64] + "\r\n" + bare[64:128] + "\n " + bare[128:],
	} {
		parsed, err := ParsePrivateKey(text)
		if err != nil || !parsed.Equal(key) {
			t.Fatalf("ParsePrivateKey(%s): %v", name, err)
		}
	}

	pkix, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey: %v", err)
	}
	for name, text := range map[string]string{
		"PKIX PEM":   string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pkix})),
		"PKCS#1 PEM": string(pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: x509.MarshalPKCS1PublicKey(&key.PublicKey)})),
		"bare PKIX":  base64.StdEncoding.EncodeToString(pkix),
	} {
		parsed, err := ParsePublicKey(text)
		if err != nil || !parsed.Equal(&key.PublicKey) {
			t.Fatalf("ParsePublicKey(%s): %v", name, err)
		}
	}

	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate EC key: %v", err)
	}
	ecPKCS8, err := x509.MarshalPKCS8PrivateKey(ecKey)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey(EC): %v", err)
	}
	for name, text := range map[string]string{
		"empty":           "  \n",
		"not base64":      "this is not a key",
		"wrong PEM type":  string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pkcs8})),
		"broken PEM":      "-----BEGIN PRIVATE KEY-----\nAAAA",
		"EC key":          string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecPKCS8})),
		"base64 of noise": base64.StdEncoding.EncodeToString([]byte("noise")),
	} {
		if _, err := ParsePrivateKey(text); err == nil {
			t.Fatalf("ParsePrivateKey accepted %s", name)
		}
		if _, err := ParsePublicKey(text); err == nil {
			t.Fatalf("ParsePublicKey accepted %s", name)
		}
	}
}

// SHA256withRSA:本包签的本包验得过;消息、公钥、编码任何一个不对都验不过。
func TestRSASignatureRoundTrip(t *testing.T) {
	key, other := testRSAKey(t, "merchant"), testRSAKey(t, "platform")
	signature, err := signSHA256(key, []byte("message"))
	if err != nil {
		t.Fatalf("signSHA256: %v", err)
	}
	if err := verifySHA256(&key.PublicKey, []byte("message"), signature); err != nil {
		t.Fatalf("verifySHA256 rejected its own signature: %v", err)
	}
	if verifySHA256(&key.PublicKey, []byte("messagf"), signature) == nil {
		t.Fatal("verifySHA256 accepted a changed message")
	}
	if verifySHA256(&other.PublicKey, []byte("message"), signature) == nil {
		t.Fatal("verifySHA256 accepted the signature under another key")
	}
	if verifySHA256(&key.PublicKey, []byte("message"), "@@@") == nil {
		t.Fatal("verifySHA256 accepted a signature that is not base64")
	}
}

// 传输失败的错误不带请求地址(地址里有 AppSecret、access_token、购买令牌),只留方法与主机;
// 底层原因还在,调用方照常能用 errors.Is 判超时。
func TestSendKeepsRequestURLOutOfTransportErrors(t *testing.T) {
	const address = "http://channel.test/tokens/purchase-token-1?secret=app-secret-1&js_code=login-code-1"
	for name, cause := range map[string]error{"refused": errors.New("connection refused"), "timeout": context.DeadlineExceeded} {
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, cause })}
		request, err := http.NewRequest(http.MethodGet, address, nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		_, err = send(client, request)
		if err == nil || !errors.Is(err, cause) {
			t.Fatalf("%s: err = %v, want it to wrap %v", name, err, cause)
		}
		for _, leaked := range []string{"purchase-token-1", "app-secret-1", "login-code-1"} {
			if strings.Contains(err.Error(), leaked) {
				t.Fatalf("%s: error text leaks %q: %v", name, leaked, err)
			}
		}
		if !strings.Contains(err.Error(), "GET channel.test") {
			t.Fatalf("%s: error text lost the method and host: %v", name, err)
		}
	}

	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return jsonAnswer(`{}`), nil })}
	request, err := http.NewRequest(http.MethodGet, address, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	response, err := send(client, request)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("send = %v, %v", response, err)
	}
	_ = response.Body.Close()
}
