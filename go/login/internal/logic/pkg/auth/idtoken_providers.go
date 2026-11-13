package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/zeromicro/go-zero/core/logx"
)

// Sign in with Apple 与 Google 登录(海外 iOS / Android)。
//
// 客户端用平台 SDK 拿到身份令牌(OIDC ID token),原样发 LoginRequest{auth_type:"apple" / "google", auth_token: 令牌};
// 服务端按渠道公布的公钥验签并核声明(idtoken.go),账号名用令牌的 sub:
//   - Apple 的 sub 对同一开发者团队下的所有 App 稳定;
//   - Google 的 sub 是 Google 账号的全局标识。
// 所以与微信 / QQ 的 unionid 一样,换设备、换同团队的另一个 App 都是同一个账号,不存在 openid 那种按应用分裂的问题。
//
// 令牌不是一次性的,有效期内(Apple 10 分钟、Google 1 小时)被截获可以重放;它只走 HTTPS,网关与服务端都不记录原文。
// 渠道的 nonce 机制要服务端先发挑战,当前登录流程没有这一步,不校验 nonce。
//
// Apple 的令牌撤销(用户删除账号时调 Apple 的 revoke 接口)要 Sign in with Apple 的私钥与授权码流程,属于账号注销功能,不在登录这里做。

const (
	// AppleAuthType / GoogleAuthType 是客户端在 LoginRequest.auth_type 里送的值,也是 login_auth_path_total 的标签取值,上线后不改名。
	AppleAuthType  = "apple"
	GoogleAuthType = "google"

	appleIssuer          = "https://appleid.apple.com"
	appleDefaultEndpoint = "https://appleid.apple.com"
	appleJWKSPath        = "/auth/keys"

	googleDefaultEndpoint = "https://www.googleapis.com"
	googleJWKSPath        = "/oauth2/v3/certs"
)

// googleIssuers:Google 的 ID token 两种 iss 写法都合法(Google 官方的服务端校验说明)。
var googleIssuers = []string{"https://accounts.google.com", "accounts.google.com"}

// IdTokenProvider 是用身份令牌登录的渠道。account = prefix + sub。
type IdTokenProvider struct {
	authType string
	prefix   string
	verifier *idTokenVerifier
}

// NewAppleProvider:clientID 是令牌 aud 必须包含的值(iOS App 的 Bundle ID;网页 / 安卓用 Services ID)。
// endpoint 覆盖取公钥的域名(沙箱 mock),空 = Apple 生产地址;签发方 iss 不随它变。client 为 nil 用默认 5s 客户端。
func NewAppleProvider(clientID, endpoint string, client *http.Client) *IdTokenProvider {
	return &IdTokenProvider{
		authType: AppleAuthType,
		prefix:   "apple_",
		verifier: newIdTokenVerifier(AppleAuthType, []string{appleIssuer}, clientID,
			idTokenBaseURL(endpoint, appleDefaultEndpoint)+appleJWKSPath, client),
	}
}

// NewGoogleProvider:clientID 是客户端请求 ID token 时用的 OAuth 客户端 ID(安卓 / iOS 都用同一个服务端客户端 ID 请求,aud 才一致)。
func NewGoogleProvider(clientID, endpoint string, client *http.Client) *IdTokenProvider {
	return &IdTokenProvider{
		authType: GoogleAuthType,
		prefix:   "google_",
		verifier: newIdTokenVerifier(GoogleAuthType, googleIssuers, clientID,
			idTokenBaseURL(endpoint, googleDefaultEndpoint)+googleJWKSPath, client),
	}
}

func (p *IdTokenProvider) Validate(ctx context.Context, identityToken string) (*AuthResult, error) {
	if p.verifier.audience == "" {
		return nil, fmt.Errorf("%s: provider not configured (missing ClientId): %w", p.authType, ErrProviderUnavailable)
	}
	sub, err := p.verifier.verify(ctx, identityToken)
	if err != nil {
		return nil, err
	}
	account := p.prefix + sub
	logx.Infof("%s auth ok: account=%s", p.authType, maskAccount(account, len(p.prefix)))
	return &AuthResult{Account: account}, nil
}

// maskAccount 只在日志里留前缀与 sub 的首尾几个字符,与 maskOAuthId 同一口径。
func maskAccount(account string, prefixLen int) string {
	return account[:prefixLen] + maskOAuthId(account[prefixLen:])
}

func idTokenBaseURL(override, defaultURL string) string {
	if override == "" {
		return defaultURL
	}
	return strings.TrimRight(override, "/")
}
