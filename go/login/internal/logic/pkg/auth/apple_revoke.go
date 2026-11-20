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
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Sign in with Apple 的令牌撤销(docs/design/account-deletion.md §8,负责人 2026-11-19 定 B)。
//
// 申请注销本来就要求重新认证;Apple 重新授权时客户端同时拿到新的 authorizationCode,随申请送上。这里用它换票(/auth/token),
// 拿到这次授权的 refresh token 后立刻撤销(/auth/revoke),什么都不保存。授权码一次性、约 5 分钟有效。
// client_secret 是用 Sign in with Apple 私钥(.p8,EC P-256)现签的 ES256 JWT,5 分钟有效,只在这次请求里用。
//
// 错误分类同第三方登录 §12.2:授权码无效 / 已用 / 过期(invalid_grant)回 ErrAppleCodeInvalid,客户端要重新拉起 Apple;
// 网络、超时、非 2xx、应答不可解析、我们自己的配置被拒(invalid_client)按渠道故障(ErrProviderUnavailable)。
// 错误文案与日志不带授权码、令牌和 client_secret。

const (
	appleAudience       = "https://appleid.apple.com"
	appleTokenPath      = "/auth/token"
	appleRevokePath     = "/auth/revoke"
	appleSecretLifetime = 5 * time.Minute
	maxAppleCodeBytes   = 4096
)

// ErrAppleCodeInvalid:授权码无效、已经用过或过期。不是渠道故障,玩家重新授权即可。
var ErrAppleCodeInvalid = errors.New("apple: authorization code invalid or already used")

// AppleRevoker 撤销一次 Sign in with Apple 授权。零值不可用,用 NewAppleRevoker 构造;可并发使用。
type AppleRevoker struct {
	clientID string // client_id,即 AuthProviders.Apple.ClientId(Bundle ID)
	teamID   string
	keyID    string
	key      *ecdsa.PrivateKey
	baseURL  string
	http     *http.Client // nil 用默认 5s 客户端
	now      func() time.Time
}

// NewAppleRevoker 解析 .p8 私钥(PKCS#8 PEM、EC P-256)。endpoint 覆盖 Apple 的域名(沙箱 mock),空 = 生产地址。
// 私钥格式不对返回错误,调用方拒绝启动:配了撤销却撤不了,注销时才发现就晚了。
func NewAppleRevoker(clientID, teamID, keyID, privateKeyPEM, endpoint string, client *http.Client) (*AppleRevoker, error) {
	block, _ := pem.Decode([]byte(privateKeyPEM))
	if block == nil {
		return nil, errors.New("apple: Sign in with Apple private key is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("apple: Sign in with Apple private key is not a PKCS#8 key")
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, errors.New("apple: Sign in with Apple private key must be an EC P-256 key")
	}
	if clientID == "" || teamID == "" || keyID == "" {
		return nil, errors.New("apple: ClientId, TeamId and KeyId are required to revoke tokens")
	}
	return &AppleRevoker{clientID: clientID, teamID: teamID, keyID: keyID, key: key,
		baseURL: idTokenBaseURL(endpoint, appleDefaultEndpoint), http: client, now: time.Now}, nil
}

// AppleGrant 是换票拿到的一枚可撤销令牌,只在内存里、只用于紧接着的 Revoke,不落盘、不进日志。
type AppleGrant struct {
	token string
	hint  string // refresh_token 或 access_token
}

// ExchangeCode 用授权码换票。授权码无效 / 已用 / 过期回 ErrAppleCodeInvalid;申请注销在改任何状态之前先做这一步,
// 授权码不对就让玩家重新授权,注销状态不动。
func (r *AppleRevoker) ExchangeCode(ctx context.Context, code string) (AppleGrant, error) {
	if code == "" || len(code) > maxAppleCodeBytes {
		return AppleGrant{}, ErrAppleCodeInvalid
	}
	secret, err := r.clientSecret()
	if err != nil {
		return AppleGrant{}, providerFault(fmt.Errorf("apple: sign client secret: %w", err))
	}
	status, body, err := r.postForm(ctx, appleTokenPath, url.Values{
		"client_id": {r.clientID}, "client_secret": {secret}, "code": {code}, "grant_type": {"authorization_code"},
	})
	if err != nil {
		return AppleGrant{}, err
	}
	if status != http.StatusOK {
		if appleErrorCode(body) == "invalid_grant" {
			return AppleGrant{}, ErrAppleCodeInvalid
		}
		return AppleGrant{}, fmt.Errorf("apple: token endpoint status %d (%s): %w", status, appleErrorCode(body), ErrProviderUnavailable)
	}
	var tokens struct {
		RefreshToken string `json:"refresh_token"`
		AccessToken  string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tokens); err != nil {
		return AppleGrant{}, fmt.Errorf("apple: bad token response json: %w", ErrProviderUnavailable)
	}
	if tokens.RefreshToken != "" {
		return AppleGrant{token: tokens.RefreshToken, hint: "refresh_token"}, nil
	}
	if tokens.AccessToken != "" {
		return AppleGrant{token: tokens.AccessToken, hint: "access_token"}, nil
	}
	return AppleGrant{}, fmt.Errorf("apple: token response carries no token: %w", ErrProviderUnavailable)
}

// Revoke 撤销换到的那次授权。注销已经提交之后才调;失败只影响 Apple 侧的授权,玩家可以到 Apple 设置里手动移除。
func (r *AppleRevoker) Revoke(ctx context.Context, grant AppleGrant) error {
	if grant.token == "" {
		return fmt.Errorf("apple: nothing to revoke: %w", ErrProviderUnavailable)
	}
	secret, err := r.clientSecret()
	if err != nil {
		return providerFault(fmt.Errorf("apple: sign client secret: %w", err))
	}
	status, body, err := r.postForm(ctx, appleRevokePath, url.Values{
		"client_id": {r.clientID}, "client_secret": {secret}, "token": {grant.token}, "token_type_hint": {grant.hint},
	})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("apple: revoke endpoint status %d (%s): %w", status, appleErrorCode(body), ErrProviderUnavailable)
	}
	return nil
}

// RevokeWithCode 换票并撤销,两步连着做(运维与测试用;申请注销在两步之间插入注销提交)。
func (r *AppleRevoker) RevokeWithCode(ctx context.Context, code string) error {
	grant, err := r.ExchangeCode(ctx, code)
	if err != nil {
		return err
	}
	return r.Revoke(ctx, grant)
}

// clientSecret 现签 ES256 JWT(签名是 R‖S 各 32 字节,RFC 7518 §3.4)。
func (r *AppleRevoker) clientSecret() (string, error) {
	now := r.now()
	header, err := json.Marshal(map[string]string{"alg": "ES256", "kid": r.keyID})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]any{
		"iss": r.teamID, "iat": now.Unix(), "exp": now.Add(appleSecretLifetime).Unix(), "aud": appleAudience, "sub": r.clientID,
	})
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	sigR, sigS, err := ecdsa.Sign(rand.Reader, r.key, digest[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	sigR.FillBytes(sig[:32])
	sigS.FillBytes(sig[32:])
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// postForm 发表单,2xx 与 4xx 都读应答体(4xx 要看 error 字段分辨授权码无效),应答限 64 KiB。网络错误按渠道故障,不带 URL。
func (r *AppleRevoker) postForm(ctx context.Context, path string, form url.Values) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, providerFault(oauthRequestError("apple", "build request", err))
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClientOrDefault(r.http).Do(req)
	if err != nil {
		return 0, nil, providerFault(oauthRequestError("apple", "http error", err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthResponseBytes+1))
	if err != nil {
		return 0, nil, providerFault(oauthRequestError("apple", "read body", err))
	}
	if len(body) > maxOAuthResponseBytes {
		return 0, nil, fmt.Errorf("apple: response exceeds %d bytes: %w", maxOAuthResponseBytes, ErrProviderUnavailable)
	}
	return resp.StatusCode, body, nil
}

// appleErrorCode 取应答体里的 error 字段(低基数,如 invalid_grant、invalid_client),只进错误文案;解析不了回 unknown。
func appleErrorCode(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil || e.Error == "" || len(e.Error) > 64 {
		return "unknown"
	}
	return e.Error
}
