package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"shared/redisslot"

	"github.com/redis/go-redis/v9"
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
// 渠道签的令牌本身在有效期内(Apple 10 分钟、Google 1 小时)可以反复用。login 把它做成一次性的(docs/design/account-deletion.md §3.4):
// 验签通过后在登录域 Redis 登记令牌摘要,TTL 到令牌过期,同一枚第二次来就按凭证无效拒绝。截获的令牌在合法客户端用过之后就用不了;
// 不做挑战式 nonce:/api/login 之前没有会话可绑,截获者连 nonce 一起拿到,效果与一次性消费相同(同一节的取舍)。
// 令牌只走 HTTPS,网关与服务端都不记录原文。
//
// Apple 的令牌撤销(用户删除账号时调 Apple 的 revoke 接口)属于账号注销功能(apple_revoke.go),不在登录这里做。

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
	// replay 登记用过的令牌;nil = 不防重放,只给单测和不接 Redis 的装配(svc 在接上登录域之后一律注入)。
	replay IdTokenReplayGuard
}

// IdTokenReplayGuard 是身份令牌的一次性消费登记。
type IdTokenReplayGuard interface {
	// FirstUse 原子地登记 key,ttl 之后自动忘掉;已经登记过回 false。
	FirstUse(ctx context.Context, key string, ttl time.Duration) (bool, error)
}

// RedisIdTokenReplayGuard 用登录域 Redis 的 SET NX PX 实现 IdTokenReplayGuard。
type RedisIdTokenReplayGuard struct{ rdb redis.UniversalClient }

func NewRedisIdTokenReplayGuard(rdb redis.UniversalClient) *RedisIdTokenReplayGuard {
	return &RedisIdTokenReplayGuard{rdb: rdb}
}

func (g *RedisIdTokenReplayGuard) FirstUse(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	return g.rdb.SetNX(ctx, key, "1", ttl).Result()
}

// WithReplayGuard 装上一次性消费登记,返回 p 本身。只在开始服务之前调。
func (p *IdTokenProvider) WithReplayGuard(g IdTokenReplayGuard) *IdTokenProvider {
	p.replay = g
	return p
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
	claims, err := p.verifier.verifyClaims(ctx, identityToken)
	if err != nil {
		return nil, err
	}
	if err := p.consume(ctx, identityToken, p.verifier.usedFor(claims)); err != nil {
		return nil, err
	}
	account := p.prefix + claims.Sub
	logx.Infof("%s auth ok: account=%s", p.authType, maskAccount(account, len(p.prefix)))
	return &AuthResult{Account: account}, nil
}

// consume 把一枚已验签的令牌登记为用过。再来一次按凭证无效(不标故障);登记读写失败按渠道故障,fail-closed:
// 登记不了就判断不了是不是重放。必须在验签之后调:没验过签的串不能占用登记,否则随手编的串就能把合法令牌「提前用掉」。
func (p *IdTokenProvider) consume(ctx context.Context, identityToken string, ttl time.Duration) error {
	if p.replay == nil {
		return nil
	}
	digest := sha256.Sum256([]byte(identityToken))
	fresh, err := p.replay.FirstUse(ctx, redisslot.IdTokenUsedKey(p.authType, hex.EncodeToString(digest[:])), ttl)
	if err != nil {
		return providerFault(fmt.Errorf("%s: record identity token use: %w", p.authType, err))
	}
	if !fresh {
		return fmt.Errorf("%s: identity token already used", p.authType)
	}
	return nil
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
