package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件钉住 Apple 令牌撤销(docs/design/account-deletion.md §8.2):
//   - client_secret 是 ES256 JWT,头部 kid,声明 iss / aud / sub / iat / exp(5 分钟),签名用 .p8 的公钥验得过;
//   - 先换票再撤销,撤销的是换到的 refresh token;
//   - invalid_grant 回 ErrAppleCodeInvalid(不算故障),其余非 2xx、坏应答、网络错误按渠道故障;
//   - 私钥不是 PKCS#8 的 P-256 拒绝构造。

func testAppleKey(t *testing.T) (*ecdsa.PrivateKey, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})) + "\n"
}

// appleStub 记下收到的表单,按 tokenStatus / tokenBody 回换票的应答。
type appleStub struct {
	mu          sync.Mutex
	forms       map[string][]map[string]string
	tokenStatus int
	tokenBody   string
	revokeCode  int
}

func (s *appleStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	form := map[string]string{}
	for k := range r.PostForm {
		form[k] = r.PostForm.Get(k)
	}
	s.mu.Lock()
	if s.forms == nil {
		s.forms = map[string][]map[string]string{}
	}
	s.forms[r.URL.Path] = append(s.forms[r.URL.Path], form)
	s.mu.Unlock()
	switch r.URL.Path {
	case appleTokenPath:
		w.WriteHeader(s.tokenStatus)
		_, _ = w.Write([]byte(s.tokenBody))
	case appleRevokePath:
		w.WriteHeader(s.revokeCode)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func newTestRevoker(t *testing.T, stub *appleStub) (*AppleRevoker, *ecdsa.PrivateKey) {
	t.Helper()
	key, keyPEM := testAppleKey(t)
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	r, err := NewAppleRevoker("com.example.game", "TEAM123456", "KEY1234567", keyPEM, srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("NewAppleRevoker: %v", err)
	}
	r.now = func() time.Time { return time.Unix(1_800_000_000, 0) }
	return r, key
}

func verifyClientSecret(t *testing.T, secret string, pub *ecdsa.PublicKey) map[string]any {
	t.Helper()
	parts := strings.Split(secret, ".")
	if len(parts) != 3 {
		t.Fatalf("client_secret 不是 JWS: %q", secret)
	}
	var header map[string]string
	if err := decodeJWTSegment(parts[0], &header); err != nil || header["alg"] != "ES256" || header["kid"] != "KEY1234567" {
		t.Fatalf("头部不对: %v %v", header, err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		t.Fatalf("签名应是 64 字节的 R‖S: %v", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("client_secret 签名验不过")
	}
	var claims map[string]any
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

func TestAppleRevokeExchangesCodeThenRevokesRefreshToken(t *testing.T) {
	stub := &appleStub{tokenStatus: 200, tokenBody: `{"access_token":"at","refresh_token":"rt-123","id_token":"x"}`, revokeCode: 200}
	r, key := newTestRevoker(t, stub)
	if err := r.RevokeWithCode(context.Background(), "code-abc"); err != nil {
		t.Fatalf("RevokeWithCode: %v", err)
	}
	token := stub.forms[appleTokenPath][0]
	if token["code"] != "code-abc" || token["grant_type"] != "authorization_code" || token["client_id"] != "com.example.game" {
		t.Fatalf("换票表单不对: %v", token)
	}
	claims := verifyClientSecret(t, token["client_secret"], &key.PublicKey)
	if claims["iss"] != "TEAM123456" || claims["aud"] != appleAudience || claims["sub"] != "com.example.game" {
		t.Fatalf("声明不对: %v", claims)
	}
	if exp, iat := claims["exp"].(float64), claims["iat"].(float64); exp-iat != appleSecretLifetime.Seconds() {
		t.Fatalf("client_secret 有效期 = %v", exp-iat)
	}
	revoke := stub.forms[appleRevokePath][0]
	if revoke["token"] != "rt-123" || revoke["token_type_hint"] != "refresh_token" {
		t.Fatalf("撤销表单不对: %v", revoke)
	}
}

func TestAppleRevokeErrorClassification(t *testing.T) {
	cases := []struct {
		name        string
		stub        *appleStub
		code        string
		wantInvalid bool
	}{
		{"invalid_grant", &appleStub{tokenStatus: 400, tokenBody: `{"error":"invalid_grant"}`, revokeCode: 200}, "c", true},
		{"空授权码", &appleStub{tokenStatus: 200, revokeCode: 200}, "", true},
		{"invalid_client 是我们的配置错", &appleStub{tokenStatus: 400, tokenBody: `{"error":"invalid_client"}`, revokeCode: 200}, "c", false},
		{"换票 500", &appleStub{tokenStatus: 500, revokeCode: 200}, "c", false},
		{"应答没有令牌", &appleStub{tokenStatus: 200, tokenBody: `{}`, revokeCode: 200}, "c", false},
		{"应答不是 JSON", &appleStub{tokenStatus: 200, tokenBody: `<html>`, revokeCode: 200}, "c", false},
		{"撤销 503", &appleStub{tokenStatus: 200, tokenBody: `{"refresh_token":"rt"}`, revokeCode: 503}, "c", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := newTestRevoker(t, tc.stub)
			err := r.RevokeWithCode(context.Background(), tc.code)
			if tc.wantInvalid {
				if !errors.Is(err, ErrAppleCodeInvalid) || errors.Is(err, ErrProviderUnavailable) {
					t.Fatalf("应回 ErrAppleCodeInvalid,实际 %v", err)
				}
				return
			}
			if !errors.Is(err, ErrProviderUnavailable) {
				t.Fatalf("应按渠道故障,实际 %v", err)
			}
			if strings.Contains(err.Error(), "rt-") || strings.Contains(err.Error(), "client_secret=") {
				t.Fatalf("错误文案带了令牌或密钥: %v", err)
			}
		})
	}
}

func TestNewAppleRevokerRejectsBadKeys(t *testing.T) {
	rsaPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: mustPKCS8(t, testRSAKey1())}))
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p384PEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: mustPKCS8(t, p384)}))
	_, goodPEM := testAppleKey(t)
	for name, tc := range map[string]struct{ pem, team string }{
		"不是 PEM":   {"not a key", "TEAM123456"},
		"RSA 私钥":   {rsaPEM, "TEAM123456"},
		"P-384":    {p384PEM, "TEAM123456"},
		"缺 TeamId": {goodPEM, ""},
	} {
		if _, err := NewAppleRevoker("com.example.game", tc.team, "KEY1234567", tc.pem, "", nil); err == nil {
			t.Fatalf("%s: 应拒绝", name)
		}
	}
}

func mustPKCS8(t *testing.T, key any) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}
