package auth

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// OIDC 身份令牌(Sign in with Apple 的 identity token、Google 登录的 ID token)的校验。
//
// 两家的令牌都是 RS256 签名的 JWT,公钥以 JWKS 发布。校验顺序固定:先按头部的 kid 取公钥验签名,签名对了才解析声明,
// 再核 iss、aud、exp、iat、sub。头部的 alg 只接受 RS256:不信令牌自己声明的算法(拒绝 none、HS256 等算法混淆)。
//
// 公钥缓存在本进程(每个 login 副本各一份,不需要跨副本协调):
//   - 拉到之后 jwksCacheTTL 内直接用;过了这个时间,认识的 kid 先照用旧公钥并在后台重拉(不让登录等这次拉取);
//   - 遇到不认识的 kid(渠道轮换了密钥)等一次重拉,但离上次拉取不足 jwksMinRefetch 就不拉、按无效令牌拒绝,
//     免得随手编 kid 的请求把渠道接口刷爆;
//   - 重拉失败时,jwksMaxStale 之内的旧公钥照用(渠道接口抖动不影响已在用的密钥),超过就按渠道故障回。
//
// 同一时刻最多一个拉取在跑(inflight),并发的请求等它结束;拉取用自己的超时(jwksFetchTimeout),不挂在某个请求的 ctx 上,
// 所以先来的请求超时离开不会让后面等着的请求一起失败。等待时各自看自己的 ctx,锁不跨网络 I/O。
//
// 错误分类沿用 oauthGet 的口径:取公钥失败(网络、非 2xx、超长、解析不了、一把能用的 RSA 公钥都没有、等公钥时请求超时)
// 标 ErrProviderUnavailable;令牌本身的问题(格式、算法、签名、声明、不认识的 kid)不标,按凭证无效处理。错误文案不带令牌原文。

const (
	jwksCacheTTL   = time.Hour
	jwksMinRefetch = time.Minute
	jwksMaxStale   = 24 * time.Hour
	// jwksFetchTimeout 是一次拉取的总时限,与渠道接口的默认 HTTP 超时相同。
	jwksFetchTimeout = defaultHTTPTimeout
	// idTokenLeeway 是核 exp / iat / nbf 时容忍的时钟偏差。
	idTokenLeeway = time.Minute
	// idTokenMinRSABits:低于 2048 位的公钥不用(NIST SP 800-57 的下限)。
	idTokenMinRSABits = 2048
	// maxIdTokenBytes 是令牌原文的上限。两家的令牌都在 2 KiB 以内,超长的不解析。
	maxIdTokenBytes = 16 << 10
)

// errIdTokenUnknownKey:令牌头部的 kid 不在渠道当前发布的公钥里(拉过之后仍不在,或离上次拉取太近不再拉)。按凭证无效处理。
var errIdTokenUnknownKey = errors.New("signing key not found")

// idTokenVerifier 校验一个渠道的身份令牌。零值不可用,用 newIdTokenVerifier 构造;可并发使用。
type idTokenVerifier struct {
	provider string   // 渠道名,只进错误文案
	issuers  []string // 接受的 iss,逐字比较
	audience string   // aud 必须包含的值
	jwksURL  string
	http     *http.Client // nil 用默认 5s 客户端
	now      func() time.Time

	// mu 保护下面的缓存状态;只在内存里读写时持有,不跨网络 I/O。
	mu         sync.Mutex
	keys       map[string]*rsa.PublicKey
	freshUntil time.Time  // 缓存在此之前可直接用
	staleUntil time.Time  // 重拉失败时,旧公钥在此之前仍可用
	lastFetch  time.Time  // 上次开始拉取的时间(成功或失败)
	inflight   *jwksFetch // 正在跑的拉取;nil = 没有
}

// jwksFetch 是一次正在跑的拉取。done 关闭之前 err 已写好(写在 mu 下)。
type jwksFetch struct {
	done chan struct{}
	err  error
}

func newIdTokenVerifier(provider string, issuers []string, audience, jwksURL string, client *http.Client) *idTokenVerifier {
	return &idTokenVerifier{
		provider: provider,
		issuers:  issuers,
		audience: audience,
		jwksURL:  jwksURL,
		http:     client,
		now:      time.Now,
	}
}

type idTokenHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

// idTokenAudience 兼容 aud 的两种合法形状:单个字符串,或字符串数组(RFC 7519 §4.1.3)。
type idTokenAudience []string

func (a *idTokenAudience) UnmarshalJSON(raw []byte) error {
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		*a = idTokenAudience{single}
		return nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return fmt.Errorf("aud is neither a string nor an array of strings")
	}
	*a = many
	return nil
}

type idTokenClaims struct {
	Iss string          `json:"iss"`
	Sub string          `json:"sub"`
	Aud idTokenAudience `json:"aud"`
	Exp json.Number     `json:"exp"`
	Iat json.Number     `json:"iat"`
	Nbf json.Number     `json:"nbf"`
}

// verify 校验令牌,返回 sub(渠道内稳定的用户标识)。
func (v *idTokenVerifier) verify(ctx context.Context, raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("%s: empty identity token", v.provider)
	}
	if len(raw) > maxIdTokenBytes {
		return "", fmt.Errorf("%s: identity token too long", v.provider)
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("%s: identity token is not a JWS compact serialization", v.provider)
	}

	var header idTokenHeader
	if err := decodeJWTSegment(parts[0], &header); err != nil {
		return "", fmt.Errorf("%s: bad token header", v.provider)
	}
	if header.Alg != "RS256" {
		return "", fmt.Errorf("%s: unsupported token alg %q", v.provider, header.Alg)
	}
	if header.Kid == "" {
		return "", fmt.Errorf("%s: token header has no kid", v.provider)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", fmt.Errorf("%s: bad token signature encoding", v.provider)
	}

	key, err := v.key(ctx, header.Kid)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature); err != nil {
		return "", fmt.Errorf("%s: token signature invalid", v.provider)
	}

	var claims idTokenClaims
	if err := decodeJWTSegment(parts[1], &claims); err != nil {
		return "", fmt.Errorf("%s: bad token claims", v.provider)
	}
	if err := v.checkClaims(&claims); err != nil {
		return "", err
	}
	return claims.Sub, nil
}

func (v *idTokenVerifier) checkClaims(c *idTokenClaims) error {
	if !slices.Contains(v.issuers, c.Iss) {
		return fmt.Errorf("%s: unexpected token issuer", v.provider)
	}
	if !slices.Contains([]string(c.Aud), v.audience) {
		return fmt.Errorf("%s: token audience does not include this app", v.provider)
	}
	if c.Sub == "" {
		return fmt.Errorf("%s: token has no subject", v.provider)
	}
	now := v.now()
	exp, err := c.Exp.Int64()
	if err != nil {
		return fmt.Errorf("%s: token has no valid exp", v.provider)
	}
	if !now.Before(time.Unix(exp, 0).Add(idTokenLeeway)) {
		return fmt.Errorf("%s: token expired", v.provider)
	}
	iat, err := c.Iat.Int64()
	if err != nil {
		return fmt.Errorf("%s: token has no valid iat", v.provider)
	}
	if time.Unix(iat, 0).After(now.Add(idTokenLeeway)) {
		return fmt.Errorf("%s: token issued in the future", v.provider)
	}
	if c.Nbf != "" {
		nbf, err := c.Nbf.Int64()
		if err != nil || time.Unix(nbf, 0).After(now.Add(idTokenLeeway)) {
			return fmt.Errorf("%s: token not valid yet", v.provider)
		}
	}
	return nil
}

// key 取 kid 对应的公钥,必要时拉一次 JWKS(规则见文件头)。
func (v *idTokenVerifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	now := v.now()
	if k, ok := v.keys[kid]; ok && now.Before(v.staleUntil) {
		if !now.Before(v.freshUntil) && v.inflight == nil && v.mayFetchLocked(now) {
			v.startFetchLocked(now) // 缓存过了新鲜期:照用认识的公钥,后台换新
		}
		v.mu.Unlock()
		return k, nil
	}
	f := v.inflight
	if f == nil {
		if !v.mayFetchLocked(now) {
			err := v.missLocked(now, nil)
			v.mu.Unlock()
			return nil, err
		}
		f = v.startFetchLocked(now)
	}
	v.mu.Unlock()

	select {
	case <-f.done:
	case <-ctx.Done():
		return nil, providerFault(fmt.Errorf("%s: wait for signing keys: %w", v.provider, ctx.Err()))
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if k, ok := v.keys[kid]; ok && v.now().Before(v.staleUntil) {
		return k, nil
	}
	return nil, v.missLocked(v.now(), f.err)
}

// mayFetchLocked:离上次开始拉取已满 jwksMinRefetch(或从没拉过)。调用方持有 mu。
func (v *idTokenVerifier) mayFetchLocked(now time.Time) bool {
	return v.lastFetch.IsZero() || now.Sub(v.lastFetch) >= jwksMinRefetch
}

// missLocked 是缓存里没有可用公钥时的结论。fetchErr 是刚结束的那次拉取的错误(没等拉取时为 nil)。
//   - 刚拉成功(或缓存仍在新鲜期)而 kid 不在里面:渠道没发布这把公钥,按凭证无效;
//   - 否则是拿不到公钥(拉取失败、或上次失败离现在太近不再拉):按渠道故障。
func (v *idTokenVerifier) missLocked(now time.Time, fetchErr error) error {
	if fetchErr != nil {
		return fetchErr
	}
	if now.Before(v.freshUntil) {
		return fmt.Errorf("%s: %w", v.provider, errIdTokenUnknownKey)
	}
	return fmt.Errorf("%s: signing keys unavailable: %w", v.provider, ErrProviderUnavailable)
}

// startFetchLocked 在后台开始一次拉取并登记为 inflight。调用方持有 mu,且 inflight 为 nil。
// 拉取结束时(持 mu)更新缓存、写 err、清 inflight,最后关 done。
func (v *idTokenVerifier) startFetchLocked(now time.Time) *jwksFetch {
	f := &jwksFetch{done: make(chan struct{})}
	v.inflight = f
	v.lastFetch = now
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), jwksFetchTimeout)
		keys, err := v.fetchKeys(ctx)
		cancel()

		v.mu.Lock()
		defer v.mu.Unlock()
		if err == nil {
			fetchedAt := v.now()
			v.keys = keys
			v.freshUntil = fetchedAt.Add(jwksCacheTTL)
			v.staleUntil = fetchedAt.Add(jwksMaxStale)
		}
		f.err = err
		v.inflight = nil
		close(f.done)
	}()
	return f
}

type jwkSet struct {
	Keys []jwk `json:"keys"`
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// fetchKeys 拉 JWKS,只留能用来验 RS256 签名的 RSA 公钥。一把都没有按渠道故障处理(不让所有令牌都被当成「不认识的 kid」)。
func (v *idTokenVerifier) fetchKeys(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	body, err := oauthGet(ctx, v.http, v.provider, v.jwksURL)
	if err != nil {
		return nil, err
	}
	var set jwkSet
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, fmt.Errorf("%s: bad JWKS json: %w", v.provider, ErrProviderUnavailable)
	}
	keys := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		if k.Kty != "RSA" || k.Kid == "" || (k.Use != "" && k.Use != "sig") || (k.Alg != "" && k.Alg != "RS256") {
			continue
		}
		pub, err := rsaPublicKeyFromJWK(k.N, k.E)
		if err != nil {
			continue
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%s: JWKS has no usable RS256 key: %w", v.provider, ErrProviderUnavailable)
	}
	return keys, nil
}

func rsaPublicKeyFromJWK(nB64, eB64 string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nB64)
	if err != nil {
		return nil, err
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eB64)
	if err != nil {
		return nil, err
	}
	n := new(big.Int).SetBytes(nBytes)
	e := new(big.Int).SetBytes(eBytes)
	if n.BitLen() < idTokenMinRSABits {
		return nil, fmt.Errorf("RSA key shorter than %d bits", idTokenMinRSABits)
	}
	if !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31-1 || e.Bit(0) == 0 {
		return nil, fmt.Errorf("RSA exponent out of range")
	}
	return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
}

// decodeJWTSegment 解一段 base64url(无填充)的 JSON。数字按 json.Number 读,exp / iat 不经过浮点。
func decodeJWTSegment(segment string, out any) error {
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return dec.Decode(out)
}
