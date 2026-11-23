package config

import (
	"fmt"
	"net/url"
	"strings"
)

// PassportAuthConf 是官方通行证(Casdoor OIDC,auth_type="passport")的配置。
// 与 Apple / Google 一样,登录只验 Casdoor 公钥签名的 RS256 id_token,不需要客户端密钥;
// 不同之处在于自托管通行证每个部署有自己的签发方域名(Issuer),因此必须显式配置。
// 架构与安全约束见 docs/design/official-passport-registration.md。
type PassportAuthConf struct {
	// Issuer 是令牌 iss 声明必须精确匹配的签发方地址(与 Casdoor 的 origin 配置一致,不带末尾斜杠)。
	// 生产模式只接受 https://,http:// 只在 Mode=dev/test 放行。
	Issuer string `json:"Issuer"`
	// ClientId 是 Casdoor Application 的公开 OAuth ClientId(令牌 aud 必须包含此值)。
	ClientId string `json:"ClientId"`
	// Endpoint 可选,覆盖拉取 /.well-known/jwks 公钥的基址;留空则直接向 Issuer 拉取。
	// 用于公网签发方(https://passport.example.com)与集群内网拉公钥分离,或指向本地 sandbox-mock。
	Endpoint string `json:"Endpoint,optional"`
}

// checkPassportProvider 在启动期校验官方通行证段(由 resolveAuthProviders 调用)。
// 报错只点字段名,不回显 URL 内容。
func checkPassportProvider(p *PassportAuthConf, relaxed bool) error {
	if err := checkPassportIssuer(p.Issuer, relaxed); err != nil {
		return err
	}
	if containsUnexpandedPlaceholder(p.ClientId) {
		return fmt.Errorf("AuthProviders.Passport.ClientId contains an unexpanded ${...}: login does not expand environment variables, write the client ID itself")
	}
	if strings.TrimSpace(p.ClientId) == "" {
		return fmt.Errorf("AuthProviders.Passport.ClientId must not be empty")
	}
	if p.ClientId != strings.TrimSpace(p.ClientId) {
		return fmt.Errorf("AuthProviders.Passport.ClientId has leading or trailing whitespace")
	}
	return checkOAuthEndpoint("Passport", p.Endpoint, relaxed)
}

func checkPassportIssuer(raw string, relaxed bool) error {
	if containsUnexpandedPlaceholder(raw) {
		return fmt.Errorf("AuthProviders.Passport.Issuer contains an unexpanded ${...}: login does not expand environment variables, write the issuer URL itself")
	}
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("AuthProviders.Passport.Issuer must not be empty")
	}
	if raw != strings.TrimSpace(raw) {
		return fmt.Errorf("AuthProviders.Passport.Issuer has leading or trailing whitespace")
	}
	if strings.HasSuffix(raw, "/") {
		return fmt.Errorf("AuthProviders.Passport.Issuer must not have a trailing slash (match Casdoor canonical origin)")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("AuthProviders.Passport.Issuer is not a valid URL")
	}
	switch scheme := strings.ToLower(u.Scheme); {
	case scheme == "https":
	case scheme == "http" && relaxed:
	default:
		return fmt.Errorf("AuthProviders.Passport.Issuer requires https:// outside dev/test mode")
	}
	if u.Opaque != "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return fmt.Errorf("AuthProviders.Passport.Issuer must be scheme://host[:port][/path] without userinfo, query, or fragment")
	}
	return nil
}
