package auth

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 本文件钉住身份令牌(Sign in with Apple、Google 登录)的校验:
//   - 只接受 RS256,先验签名再看声明;iss / aud / exp / iat / nbf / sub 逐项核对;
//   - 令牌本身的问题按凭证无效(不标 ErrProviderUnavailable),取不到公钥按渠道故障(标);
//   - 公钥缓存:新鲜期内不拉,过期后认识的 kid 先用旧公钥、后台换新,不认识的 kid 等一次重拉且每分钟最多一次,
//     重拉失败时 24 小时内的旧公钥照用;并发的拉取收成一次,等待的请求各看自己的 ctx。

const (
	testIdTokenClientID = "com.example.game"
	testIdTokenIssuer   = "https://issuer.example.com"
)

// 生成 RSA 密钥要几十到几百毫秒,整个包只生成一次。
var (
	testRSAKey1     = sync.OnceValue(func() *rsa.PrivateKey { return mustRSAKey(2048) })
	testRSAKey2     = sync.OnceValue(func() *rsa.PrivateKey { return mustRSAKey(2048) })
	testRSAKeyShort = sync.OnceValue(func() *rsa.PrivateKey { return mustRSAKey(1024) })
)

func mustRSAKey(bits int) *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		panic(err)
	}
	return k
}

// testClock 是可拨动的时钟;拉取在后台 goroutine 里也读它,所以加锁。
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock { return &testClock{now: time.Unix(1_800_000_000, 0)} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// jwksStub 是渠道的公钥接口。publish 换发布的公钥,fail 让它回 500,hold 让请求卡住直到 release。
type jwksStub struct {
	mu      sync.Mutex
	keys    []jwk
	status  int
	body    string
	hold    chan struct{}
	fetches atomic.Int32
	paths   chan string
}

func (s *jwksStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.fetches.Add(1)
	select {
	case s.paths <- r.URL.Path:
	default:
	}
	s.mu.Lock()
	hold := s.hold
	s.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-r.Context().Done():
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status != 0 {
		w.WriteHeader(s.status)
		return
	}
	if s.body != "" {
		_, _ = w.Write([]byte(s.body))
		return
	}
	_ = json.NewEncoder(w).Encode(jwkSet{Keys: s.keys})
}

func (s *jwksStub) publish(keys ...jwk) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys, s.status, s.body = keys, 0, ""
}

func (s *jwksStub) fail(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
}

func (s *jwksStub) holdRequests() (release func()) {
	ch := make(chan struct{})
	s.mu.Lock()
	s.hold = ch
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			s.hold = nil
			s.mu.Unlock()
			close(ch)
		})
	}
}

func publicJWK(kid string, key *rsa.PrivateKey) jwk {
	return jwk{
		Kty: "RSA", Kid: kid, Use: "sig", Alg: "RS256",
		N: base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
		E: base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes()),
	}
}

// newTestVerifier 搭一个公钥接口和对着它的校验器,初始发布 kid1。
func newTestVerifier(t *testing.T) (*idTokenVerifier, *jwksStub, *testClock) {
	t.Helper()
	stub := &jwksStub{paths: make(chan string, 16)}
	stub.publish(publicJWK("kid1", testRSAKey1()))
	cli, srv := newTestClient(stub)
	t.Cleanup(srv.Close)
	clock := newTestClock()
	v := newIdTokenVerifier("apple", []string{testIdTokenIssuer}, testIdTokenClientID, "https://keys.example.com/jwks", cli)
	v.now = clock.Now
	return v, stub, clock
}

// waitFetchIdle 等后台拉取结束,用例才能确定性地看拉取次数与缓存。
func waitFetchIdle(t *testing.T, v *idTokenVerifier) {
	t.Helper()
	v.mu.Lock()
	f := v.inflight
	v.mu.Unlock()
	if f == nil {
		return
	}
	select {
	case <-f.done:
	case <-time.After(5 * time.Second):
		t.Fatal("JWKS fetch did not finish")
	}
}

func validClaims(clock *testClock) map[string]any {
	now := clock.Now().Unix()
	return map[string]any{
		"iss": testIdTokenIssuer,
		"aud": testIdTokenClientID,
		"sub": "001234.abcdef0123456789.0123",
		"iat": now,
		"exp": now + 600,
	}
}

func signTestIdToken(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	signingInput := encodeTestSegment(t, map[string]any{"alg": "RS256", "kid": kid, "typ": "JWT"}) + "." +
		encodeTestSegment(t, claims)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func encodeTestSegment(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// requireInvalidCredential:令牌被拒,且按凭证无效处理(不是渠道故障)。
func requireInvalidCredential(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: 应拒绝", what)
	}
	if errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("%s: 令牌本身的问题不应标 ErrProviderUnavailable: %v", what, err)
	}
}

func requireProviderUnavailable(t *testing.T, err error, what string) {
	t.Helper()
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("%s: 应按渠道故障(ErrProviderUnavailable),实际 %v", what, err)
	}
}

func TestIdTokenVerifyAcceptsValidTokenAndReturnsSubject(t *testing.T) {
	v, stub, clock := newTestVerifier(t)
	claims := validClaims(clock)
	sub, err := v.verify(context.Background(), signTestIdToken(t, testRSAKey1(), "kid1", claims))
	if err != nil {
		t.Fatalf("合法令牌应通过: %v", err)
	}
	if sub != claims["sub"] {
		t.Fatalf("sub = %q, want %q", sub, claims["sub"])
	}
	// 第二次走缓存,不再拉公钥。
	if _, err := v.verify(context.Background(), signTestIdToken(t, testRSAKey1(), "kid1", claims)); err != nil {
		t.Fatalf("second verify: %v", err)
	}
	if got := stub.fetches.Load(); got != 1 {
		t.Fatalf("新鲜期内只应拉一次公钥,实际 %d 次", got)
	}
}

func TestIdTokenAudienceMayBeAnArray(t *testing.T) {
	v, _, clock := newTestVerifier(t)
	claims := validClaims(clock)
	claims["aud"] = []string{"other.app", testIdTokenClientID}
	if _, err := v.verify(context.Background(), signTestIdToken(t, testRSAKey1(), "kid1", claims)); err != nil {
		t.Fatalf("aud 数组里含本应用应通过: %v", err)
	}
	claims["aud"] = []string{"other.app"}
	_, err := v.verify(context.Background(), signTestIdToken(t, testRSAKey1(), "kid1", claims))
	requireInvalidCredential(t, err, "aud 数组里没有本应用")
}

func TestIdTokenRejectsBadClaims(t *testing.T) {
	cases := map[string]func(c map[string]any, now int64){
		"wrong issuer":          func(c map[string]any, _ int64) { c["iss"] = "https://accounts.google.com" },
		"missing issuer":        func(c map[string]any, _ int64) { delete(c, "iss") },
		"wrong audience":        func(c map[string]any, _ int64) { c["aud"] = "com.other.app" },
		"missing audience":      func(c map[string]any, _ int64) { delete(c, "aud") },
		"audience not a string": func(c map[string]any, _ int64) { c["aud"] = 42 },
		"missing subject":       func(c map[string]any, _ int64) { delete(c, "sub") },
		"empty subject":         func(c map[string]any, _ int64) { c["sub"] = "" },
		"expired":               func(c map[string]any, now int64) { c["exp"] = now - 61 },
		"missing exp":           func(c map[string]any, _ int64) { delete(c, "exp") },
		"exp not a number":      func(c map[string]any, now int64) { c["exp"] = "tomorrow" },
		"issued in the future":  func(c map[string]any, now int64) { c["iat"] = now + 120 },
		"missing iat":           func(c map[string]any, _ int64) { delete(c, "iat") },
		"not valid yet":         func(c map[string]any, now int64) { c["nbf"] = now + 120 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			v, _, clock := newTestVerifier(t)
			claims := validClaims(clock)
			mutate(claims, clock.Now().Unix())
			_, err := v.verify(context.Background(), signTestIdToken(t, testRSAKey1(), "kid1", claims))
			requireInvalidCredential(t, err, name)
		})
	}
}

// 时钟偏差在 1 分钟内放行:刚过期 30 秒、签发时间快 30 秒都还算有效。
func TestIdTokenToleratesSmallClockSkew(t *testing.T) {
	v, _, clock := newTestVerifier(t)
	claims := validClaims(clock)
	now := clock.Now().Unix()
	claims["exp"], claims["iat"], claims["nbf"] = now-30, now+30, now+30
	if _, err := v.verify(context.Background(), signTestIdToken(t, testRSAKey1(), "kid1", claims)); err != nil {
		t.Fatalf("1 分钟内的时钟偏差应放行: %v", err)
	}
}

func TestIdTokenRejectsTamperedOrForeignSignature(t *testing.T) {
	v, _, clock := newTestVerifier(t)
	token := signTestIdToken(t, testRSAKey1(), "kid1", validClaims(clock))
	parts := strings.Split(token, ".")

	forged := validClaims(clock)
	forged["sub"] = "someone-else"
	tampered := parts[0] + "." + encodeTestSegment(t, forged) + "." + parts[2]
	_, err := v.verify(context.Background(), tampered)
	requireInvalidCredential(t, err, "改了声明、沿用原签名")

	// 用别的私钥签、头部却写 kid1:验签失败。
	_, err = v.verify(context.Background(), signTestIdToken(t, testRSAKey2(), "kid1", validClaims(clock)))
	requireInvalidCredential(t, err, "别的私钥签的令牌")
}

// 算法混淆:不信令牌头部声明的算法。none、HS256(拿公钥当 HMAC 密钥)、RS512 都在取公钥之前就拒绝。
func TestIdTokenRejectsAlgorithmConfusion(t *testing.T) {
	v, stub, clock := newTestVerifier(t)
	claimsSeg := encodeTestSegment(t, validClaims(clock))
	pubN := testRSAKey1().PublicKey.N.Bytes()

	for _, alg := range []string{"none", "HS256", "RS512", "PS256", "ES256", ""} {
		headerSeg := encodeTestSegment(t, map[string]any{"alg": alg, "kid": "kid1"})
		mac := hmac.New(sha256.New, pubN)
		mac.Write([]byte(headerSeg + "." + claimsSeg))
		token := headerSeg + "." + claimsSeg + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
		_, err := v.verify(context.Background(), token)
		requireInvalidCredential(t, err, "alg="+alg)
	}
	if got := stub.fetches.Load(); got != 0 {
		t.Fatalf("算法不对应在取公钥之前拒绝,实际拉了 %d 次公钥", got)
	}
}

func TestIdTokenRejectsMalformedTokens(t *testing.T) {
	v, stub, clock := newTestVerifier(t)
	good := signTestIdToken(t, testRSAKey1(), "kid1", validClaims(clock))
	parts := strings.Split(good, ".")
	noKid := encodeTestSegment(t, map[string]any{"alg": "RS256"})

	cases := map[string]string{
		"empty":                "",
		"two segments":         parts[0] + "." + parts[1],
		"four segments":        good + ".x",
		"header not base64url": "%%%." + parts[1] + "." + parts[2],
		"header not json":      base64.RawURLEncoding.EncodeToString([]byte("nope")) + "." + parts[1] + "." + parts[2],
		"no kid":               noKid + "." + parts[1] + "." + parts[2],
		"signature padded":     parts[0] + "." + parts[1] + "." + parts[2] + "==",
		"too long":             good + strings.Repeat("A", maxIdTokenBytes),
	}
	for name, token := range cases {
		_, err := v.verify(context.Background(), token)
		requireInvalidCredential(t, err, name)
		if err != nil && strings.Contains(err.Error(), parts[2]) {
			t.Fatalf("%s: 错误文案不应带令牌原文", name)
		}
	}
	if got := stub.fetches.Load(); got != 0 {
		t.Fatalf("格式不对应在取公钥之前拒绝,实际拉了 %d 次公钥", got)
	}
}

// 不认识的 kid:每分钟最多重拉一次;渠道发布新公钥之后,下一次允许的重拉把它拉进来。
func TestIdTokenUnknownKidRefetchIsRateLimited(t *testing.T) {
	v, stub, clock := newTestVerifier(t)
	if _, err := v.verify(context.Background(), signTestIdToken(t, testRSAKey1(), "kid1", validClaims(clock))); err != nil {
		t.Fatalf("warm up: %v", err)
	}

	unknown := func() error {
		_, err := v.verify(context.Background(), signTestIdToken(t, testRSAKey2(), "kid2", validClaims(clock)))
		return err
	}
	requireInvalidCredential(t, unknown(), "刚拉过、kid 不在其中")
	if got := stub.fetches.Load(); got != 1 {
		t.Fatalf("离上次拉取不足 1 分钟不应重拉,实际 %d 次", got)
	}

	clock.Advance(jwksMinRefetch)
	requireInvalidCredential(t, unknown(), "重拉之后 kid 仍不在其中")
	if got := stub.fetches.Load(); got != 2 {
		t.Fatalf("满 1 分钟后应重拉一次,实际 %d 次", got)
	}

	// 渠道轮换:发布 kid2。1 分钟内还拒绝,到点重拉后通过。
	stub.publish(publicJWK("kid1", testRSAKey1()), publicJWK("kid2", testRSAKey2()))
	requireInvalidCredential(t, unknown(), "发布后 1 分钟内")
	clock.Advance(jwksMinRefetch)
	if err := unknown(); err != nil {
		t.Fatalf("渠道发布新公钥后应能验过: %v", err)
	}
	if got := stub.fetches.Load(); got != 3 {
		t.Fatalf("拉取次数 = %d, want 3", got)
	}
}

// 过了新鲜期:认识的 kid 直接用旧公钥并在后台换新;换新时渠道已撤掉的公钥随之失效。
func TestIdTokenStaleKeyServedWhileRefreshingInBackground(t *testing.T) {
	v, stub, clock := newTestVerifier(t)
	if _, err := v.verify(context.Background(), signTestIdToken(t, testRSAKey1(), "kid1", validClaims(clock))); err != nil {
		t.Fatalf("warm up: %v", err)
	}

	stub.publish(publicJWK("kid2", testRSAKey2())) // 渠道撤掉 kid1
	clock.Advance(jwksCacheTTL + time.Second)
	release := stub.holdRequests()
	defer release()
	if _, err := v.verify(context.Background(), signTestIdToken(t, testRSAKey1(), "kid1", validClaims(clock))); err != nil {
		t.Fatalf("过了新鲜期、认识的 kid 不应等拉取: %v", err)
	}
	release()
	waitFetchIdle(t, v)
	if got := stub.fetches.Load(); got != 2 {
		t.Fatalf("应在后台换新一次,实际拉了 %d 次", got)
	}

	_, err := v.verify(context.Background(), signTestIdToken(t, testRSAKey1(), "kid1", validClaims(clock)))
	requireInvalidCredential(t, err, "换新后渠道已撤掉的 kid")
	if _, err := v.verify(context.Background(), signTestIdToken(t, testRSAKey2(), "kid2", validClaims(clock))); err != nil {
		t.Fatalf("换新后的 kid2 应通过: %v", err)
	}
}

// 渠道公钥接口故障:24 小时内的旧公钥照用,超过就按渠道故障回;从没拉成功过也按渠道故障回。
func TestIdTokenKeysSurviveProviderOutageForMaxStale(t *testing.T) {
	v, stub, clock := newTestVerifier(t)
	token := func() string { return signTestIdToken(t, testRSAKey1(), "kid1", validClaims(clock)) }
	if _, err := v.verify(context.Background(), token()); err != nil {
		t.Fatalf("warm up: %v", err)
	}

	stub.fail(http.StatusInternalServerError)
	clock.Advance(2 * jwksCacheTTL)
	if _, err := v.verify(context.Background(), token()); err != nil {
		t.Fatalf("渠道故障时 24 小时内的旧公钥应照用: %v", err)
	}
	waitFetchIdle(t, v)

	clock.Advance(jwksMaxStale)
	_, err := v.verify(context.Background(), token())
	requireProviderUnavailable(t, err, "旧公钥超过 24 小时且重拉失败")

	// 离上次失败不足 1 分钟:不再拉,直接按渠道故障回。
	before := stub.fetches.Load()
	_, err = v.verify(context.Background(), token())
	requireProviderUnavailable(t, err, "刚失败过")
	if stub.fetches.Load() != before {
		t.Fatal("离上次拉取不足 1 分钟不应重拉")
	}
}

func TestIdTokenJWKSFailuresAreProviderUnavailable(t *testing.T) {
	cases := map[string]func(s *jwksStub){
		"http 500":       func(s *jwksStub) { s.fail(http.StatusInternalServerError) },
		"http 404":       func(s *jwksStub) { s.fail(http.StatusNotFound) },
		"not json":       func(s *jwksStub) { s.mu.Lock(); s.body = "<html>"; s.mu.Unlock() },
		"no keys":        func(s *jwksStub) { s.publish() },
		"only short key": func(s *jwksStub) { s.publish(publicJWK("kid1", testRSAKeyShort())) },
		"only enc key": func(s *jwksStub) {
			k := publicJWK("kid1", testRSAKey1())
			k.Use = "enc"
			s.publish(k)
		},
		"only EC key": func(s *jwksStub) {
			k := publicJWK("kid1", testRSAKey1())
			k.Kty = "EC"
			s.publish(k)
		},
		"oversized": func(s *jwksStub) {
			s.mu.Lock()
			s.body = `{"keys":[],"pad":"` + strings.Repeat("x", maxOAuthResponseBytes) + `"}`
			s.mu.Unlock()
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			v, stub, clock := newTestVerifier(t)
			setup(stub)
			_, err := v.verify(context.Background(), signTestIdToken(t, testRSAKey1(), "kid1", validClaims(clock)))
			requireProviderUnavailable(t, err, name)
		})
	}
}

// 并发的请求共用一次拉取。
func TestIdTokenConcurrentMissesShareOneFetch(t *testing.T) {
	v, stub, clock := newTestVerifier(t)
	release := stub.holdRequests()
	defer release()

	const n = 8
	token := signTestIdToken(t, testRSAKey1(), "kid1", validClaims(clock))
	errs := make(chan error, n)
	for range n {
		go func() {
			_, err := v.verify(context.Background(), token)
			errs <- err
		}()
	}
	// 等第一个请求把拉取发出去,再放行。
	select {
	case <-stub.paths:
	case <-time.After(5 * time.Second):
		t.Fatal("JWKS fetch was never sent")
	}
	release()
	for range n {
		if err := <-errs; err != nil {
			t.Fatalf("verify: %v", err)
		}
	}
	if got := stub.fetches.Load(); got != 1 {
		t.Fatalf("并发请求应共用一次拉取,实际 %d 次", got)
	}
}

// 等公钥的请求按自己的期限离开(按渠道故障回,保留超时语义);拉取不跟着它取消,后来的请求照样拿到结果。
func TestIdTokenWaiterHonorsItsOwnDeadline(t *testing.T) {
	v, stub, clock := newTestVerifier(t)
	release := stub.holdRequests()
	defer release()
	token := signTestIdToken(t, testRSAKey1(), "kid1", validClaims(clock))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := v.verify(ctx, token)
	requireProviderUnavailable(t, err, "等公钥时请求超时")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("应保留超时语义,实际 %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := v.verify(context.Background(), token)
		done <- err
	}()
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("拉取不应随先前请求的超时取消: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("verify did not return")
	}
	if got := stub.fetches.Load(); got != 1 {
		t.Fatalf("应只有一次拉取,实际 %d 次", got)
	}
}

func TestRSAPublicKeyFromJWKRejectsWeakParameters(t *testing.T) {
	good := publicJWK("kid1", testRSAKey1())
	if _, err := rsaPublicKeyFromJWK(good.N, good.E); err != nil {
		t.Fatalf("2048 位、e=65537 应接受: %v", err)
	}
	short := publicJWK("kid1", testRSAKeyShort())
	cases := map[string][2]string{
		"1024-bit modulus": {short.N, short.E},
		"exponent 1":       {good.N, base64.RawURLEncoding.EncodeToString([]byte{1})},
		"even exponent":    {good.N, base64.RawURLEncoding.EncodeToString([]byte{0x01, 0x00, 0x00})},
		"bad base64":       {"%%%", good.E},
	}
	for name, c := range cases {
		if _, err := rsaPublicKeyFromJWK(c[0], c[1]); err == nil {
			t.Fatalf("%s: 应拒绝", name)
		}
	}
}

// 渠道层:默认地址与公钥路径、两家的签发方、账号前缀、没配 ClientId 按渠道故障。
func TestIdTokenProvidersUseFixedIssuerAndJWKSPath(t *testing.T) {
	cases := []struct {
		name    string
		build   func(cli *http.Client) *IdTokenProvider
		issuers []string
		path    string
		prefix  string
	}{
		{"apple", func(cli *http.Client) *IdTokenProvider { return NewAppleProvider(testIdTokenClientID, "", cli) },
			[]string{appleIssuer}, appleJWKSPath, "apple_"},
		{"google", func(cli *http.Client) *IdTokenProvider {
			return NewGoogleProvider(testIdTokenClientID, "https://mock.example.com/base/", cli)
		}, googleIssuers, "/base" + googleJWKSPath, "google_"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &jwksStub{paths: make(chan string, 16)}
			stub.publish(publicJWK("kid1", testRSAKey1()))
			cli, srv := newTestClient(stub)
			t.Cleanup(srv.Close)
			p := tc.build(cli)

			for _, iss := range tc.issuers {
				claims := validClaims(newTestClock())
				claims["iat"], claims["exp"] = time.Now().Unix(), time.Now().Add(10*time.Minute).Unix()
				claims["iss"] = iss
				res, err := p.Validate(context.Background(), signTestIdToken(t, testRSAKey1(), "kid1", claims))
				if err != nil {
					t.Fatalf("iss=%s: %v", iss, err)
				}
				if res.Account != tc.prefix+claims["sub"].(string) {
					t.Fatalf("account = %q", res.Account)
				}
			}
			if got := <-stub.paths; got != tc.path {
				t.Fatalf("公钥路径 = %q, want %q", got, tc.path)
			}

			// 另一家的签发方不认。
			claims := validClaims(newTestClock())
			claims["iat"], claims["exp"] = time.Now().Unix(), time.Now().Add(10*time.Minute).Unix()
			claims["iss"] = "https://accounts.example.org"
			_, err := p.Validate(context.Background(), signTestIdToken(t, testRSAKey1(), "kid1", claims))
			requireInvalidCredential(t, err, "别家的签发方")
		})
	}

	_, err := NewAppleProvider("", "", nil).Validate(context.Background(), "x.y.z")
	requireProviderUnavailable(t, err, "没配 ClientId")
}
