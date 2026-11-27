package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"

	"shared/redisslot"

	"github.com/zeromicro/go-zero/core/logx"
)

// 官方通行证(Casdoor OIDC)登录渠道(docs/design/official-passport-registration.md)。
//
// 对 login 与全球账号库而言,官方通行证与 Apple / Google 同构:
//   - 客户端完成 OAuth2 + PKCE 注册/登录后把 Casdoor 签发的 RS256 id_token 交给 /api/login(auth_type="passport");
//   - 服务端复用 idTokenVerifier 按 Casdoor 的 /.well-known/jwks 验签并核 iss / aud / exp / iat / nbf / sub;
//   - 额外守住三道自托管 IdP 特有的防线:
//       1. 只收身份令牌(tokenType = "id-token")。Casdoor 一次换票同时给出身份令牌、访问令牌、刷新令牌,三枚都是同一把钥匙签的
//          JWT,iss / aud / sub 相同(v4.27.1 object/token_jwt.go),只有 tokenType 不同。不核这一项,身份令牌被一次性消费之后,
//          同一次换票拿到的访问令牌还能再登录一次;刷新令牌有效期更长。别的类型按凭证无效拒绝,不算渠道故障:
//          任何玩家都拿得到自己的访问令牌和刷新令牌,不能让他凭这个刷出故障告警。
//       2. 令牌总有效期 (exp - iat) 不得超过 passportMaxTokenLifetime(2h):Casdoor 默认 168h,不拦截会把
//          LoginRedis 一次性防重放键保留 7 天并放大令牌泄露窗口;超限按 ErrProviderUnavailable(配置故障)拒收。
//       3. 账号标识 passport_<sub> 不含空白且总长度 <= 191 字节(对齐 account_directory 主键上限)。

const (
	// PassportAuthType 是客户端在 LoginRequest.auth_type 里送的值,也是指标 label 取值,上线后不改名。
	PassportAuthType = "passport"

	// passportIDTokenType 是 Casdoor 写在身份令牌里的 tokenType;访问令牌是 "access-token",刷新令牌是 "refresh-token"。
	passportIDTokenType = "id-token"

	passportAccountPrefix    = "passport_"
	passportJWKSPath         = "/.well-known/jwks"
	passportMaxAccountBytes  = 191
	passportMaxTokenLifetime = 2 * time.Hour
)

// PassportProvider 校验自托管官方通行证(Casdoor)签发的 OIDC 身份令牌,account = "passport_" + sub。
type PassportProvider struct {
	issuer   string
	verifier *idTokenVerifier
	// replay 登记已用令牌;nil = 不防重放(仅单测使用,svc 装配时注入登录域 Redis)。
	replay IdTokenReplayGuard
}

// NewPassportProvider 构造官方通行证认证器。issuer 必须与令牌 iss 完全一致(不带末尾斜杠);
// endpoint 可选,为空时直接向 issuer + "/.well-known/jwks" 拉取公钥。client 为 nil 用默认 5s 客户端。
func NewPassportProvider(issuer, clientID, endpoint string, client *http.Client) *PassportProvider {
	base := idTokenBaseURL(endpoint, issuer)
	return &PassportProvider{
		issuer:   issuer,
		verifier: newIdTokenVerifier(PassportAuthType, []string{issuer}, clientID, base+passportJWKSPath, client),
	}
}

// WithReplayGuard 装上一次性消费登记并返回 p 本身,只在开始服务之前调用。
func (p *PassportProvider) WithReplayGuard(g IdTokenReplayGuard) *PassportProvider {
	p.replay = g
	return p
}

func (p *PassportProvider) Validate(ctx context.Context, identityToken string) (*AuthResult, error) {
	if p.issuer == "" || p.verifier.audience == "" {
		return nil, fmt.Errorf("%s: provider not configured (missing Issuer or ClientId): %w", PassportAuthType, ErrProviderUnavailable)
	}
	claims, err := p.verifier.verifyClaims(ctx, identityToken)
	if err != nil {
		return nil, err
	}
	if err := validatePassportClaims(claims); err != nil {
		return nil, err
	}
	if err := p.consume(ctx, identityToken, p.verifier.usedFor(claims)); err != nil {
		return nil, err
	}
	account := passportAccountPrefix + claims.Sub
	iat, _ := claims.Iat.Int64() // verifyClaims 已核过
	logx.Infof("%s auth ok: account=%s", PassportAuthType, maskAccount(account, len(passportAccountPrefix)))
	return &AuthResult{Account: account, IdentityIssuedAt: iat}, nil
}

// PassportSubject 从账号名取回通行证的 sub;不是通行证账号回 false。账号注销与刷新前的核对按它找到通行证里的用户(passport_admin.go)。
func PassportSubject(account string) (string, bool) {
	sub, ok := strings.CutPrefix(account, passportAccountPrefix)
	return sub, ok && sub != ""
}

func validatePassportClaims(claims *idTokenClaims) error {
	// 先核类型再核有效期:刷新令牌的有效期本来就长,不能让它走到下面的「配置故障」。
	if claims.TokenType != passportIDTokenType {
		return fmt.Errorf("%s: token is not an ID token", PassportAuthType)
	}
	if strings.IndexFunc(claims.Sub, unicode.IsSpace) >= 0 {
		return fmt.Errorf("%s: token subject contains whitespace", PassportAuthType)
	}
	if len(passportAccountPrefix)+len(claims.Sub) > passportMaxAccountBytes {
		return fmt.Errorf("%s: token subject exceeds max account length", PassportAuthType)
	}
	iat, _ := claims.Iat.Int64() // verifyClaims 已核过
	exp, _ := claims.Exp.Int64()
	if exp <= iat {
		return fmt.Errorf("%s: token exp is not after iat", PassportAuthType)
	}
	lifetime := time.Duration(exp-iat) * time.Second
	if lifetime > passportMaxTokenLifetime {
		return providerFault(fmt.Errorf("%s: token lifetime %s exceeds max %s; lower passport application expireInHours",
			PassportAuthType, lifetime, passportMaxTokenLifetime))
	}
	return nil
}

func (p *PassportProvider) consume(ctx context.Context, identityToken string, ttl time.Duration) error {
	if p.replay == nil {
		return nil
	}
	digest := sha256.Sum256([]byte(identityToken))
	fresh, err := p.replay.FirstUse(ctx, redisslot.IdTokenUsedKey(PassportAuthType, hex.EncodeToString(digest[:])), ttl)
	if err != nil {
		return providerFault(fmt.Errorf("%s: record identity token use: %w", PassportAuthType, err))
	}
	if !fresh {
		return fmt.Errorf("%s: identity token already used", PassportAuthType)
	}
	return nil
}
