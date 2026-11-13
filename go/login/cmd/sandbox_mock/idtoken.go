package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"time"
)

// Sign in with Apple 与 Google 登录的沙箱(2026-11-13 起,auth_type 为 apple / google)。
//
// 真实流程里客户端 SDK 直接从渠道拿身份令牌,服务端只去渠道取公钥验签(login 的 pkg/auth/idtoken.go)。mock 因此提供两类接口:
//
//	GET /auth/keys、GET /oauth2/v3/certs
//	    公钥(JWKS)。路径与 appleid.apple.com、www.googleapis.com 相同,login 的 AuthProviders.Apple / Google.Endpoint
//	    指向本 mock 即可;签发方(iss)固定是两家的生产值,不随 Endpoint 变。
//	GET /mock/apple/id_token?user=<种子>&aud=<ClientId>、GET /mock/google/id_token?user=<种子>&aud=<ClientId>
//	    代替客户端 SDK 签发令牌,回 {"id_token": "..."},拿去发 LoginRequest{auth_type:"apple" / "google", auth_token: 令牌}。
//	    aud 必须等于 login 配置的 ClientId,否则 login 按 aud 不符拒绝(用来走这个分支)。
//
// 签名密钥在进程启动时生成、只在内存里。kid 是公钥的 RFC 7638 指纹:mock 重启后 kid 变化,
// login 遇到不认识的 kid 会重拉公钥(离上次拉取满 1 分钟才拉),所以 mock 刚重启的 1 分钟内登录可能被拒。
// sub 由 user 种子派生,同一个种子永远得到同一个账号;user=EXPIRED 签一个已过期的令牌,用来走「令牌过期」分支。

const (
	sandboxAppleIssuer  = "https://appleid.apple.com"
	sandboxGoogleIssuer = "https://accounts.google.com"
	// 与两家的真实有效期一致:Apple 10 分钟,Google 1 小时。
	sandboxAppleTokenTTL  = 10 * time.Minute
	sandboxGoogleTokenTTL = time.Hour
)

// idTokenIssuer 是 mock 的签名方:一把 RSA 私钥,同时供 JWKS 接口与签发接口使用。
type idTokenIssuer struct {
	key *rsa.PrivateKey
	kid string
	now func() time.Time
}

// newIdTokenIssuer 生成签名密钥。rsa.GenerateKey 用 crypto/rand 且位数合法时不会失败,失败只可能是运行环境坏了,直接 panic。
func newIdTokenIssuer() *idTokenIssuer {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic("sandbox mock: generate RSA signing key: " + err.Error())
	}
	return &idTokenIssuer{key: key, kid: rsaThumbprint(&key.PublicKey), now: time.Now}
}

func base64URL(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func rsaExponent(pub *rsa.PublicKey) string { return base64URL(big.NewInt(int64(pub.E)).Bytes()) }

// rsaThumbprint 是 RFC 7638 的 JWK 指纹:按字典序排列必需成员、不留空白,取 SHA-256。
func rsaThumbprint(pub *rsa.PublicKey) string {
	canonical := `{"e":"` + rsaExponent(pub) + `","kty":"RSA","n":"` + base64URL(pub.N.Bytes()) + `"}`
	sum := sha256.Sum256([]byte(canonical))
	return base64URL(sum[:])
}

func (i *idTokenIssuer) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	writeJSON(w, map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "kid": i.kid, "use": "sig", "alg": "RS256",
		"n": base64URL(i.key.PublicKey.N.Bytes()), "e": rsaExponent(&i.key.PublicKey),
	}}})
}

// mintHandler 返回一个渠道的签发接口。salt 区分两家:同一个 user 种子在 Apple 与 Google 是两个不同的账号,与真实渠道一致。
func (i *idTokenIssuer) mintHandler(issuer string, ttl time.Duration, salt string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		user, aud := q.Get("user"), q.Get("aud")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if user == "" || aud == "" {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]any{"error": "query parameters user and aud are required"})
			return
		}
		iat := i.now()
		exp := iat.Add(ttl)
		if user == "EXPIRED" {
			iat, exp = iat.Add(-2*time.Hour), iat.Add(-time.Hour)
		}
		token, err := i.sign(map[string]any{
			"iss": issuer,
			"aud": aud,
			"sub": "sandbox_" + hashTo12(user, salt),
			"iat": iat.Unix(),
			"exp": exp.Unix(),
		})
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]any{"error": "sign failed"})
			return
		}
		writeJSON(w, map[string]any{"id_token": token})
	}
}

// sign 按 JWS compact 格式签一个 RS256 令牌。
func (i *idTokenIssuer) sign(claims map[string]any) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "RS256", "kid": i.kid, "typ": "JWT"})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := base64URL(header) + "." + base64URL(payload)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, i.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64URL(sig), nil
}
