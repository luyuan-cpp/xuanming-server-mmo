package config

import (
	"fmt"
	"net/url"
	"strings"
)

// PassportAuthConf 是官方通行证(Casdoor OIDC,auth_type="passport")的配置。
// 与 Apple / Google 一样,登录只验 Casdoor 公钥签名的 RS256 id_token,不需要客户端密钥;
// 不同之处在于自托管通行证每个部署有自己的签发方域名(Issuer),因此必须显式配置;
// 另外玩家的个人信息存在我们自己的通行证里,账号注销要去删,所以多一组管理接口凭据(AdminClient*)。
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

	// AdminClientId 与 AdminClientSecretEnv 是 login 调 Casdoor 管理接口用的机密应用凭据:账号注销时删掉通行证里的用户
	// (邮箱、手机号、口令哈希都存在通行证那边,pkg/auth/passport_admin.go)。这个应用与玩家在 Casdoor 的同一个组织里,
	// 不能是客户端用的那个公开应用(ClientId)。
	// 生产必填:不配就删不掉通行证里的个人信息,注销不算数,login 拒绝启动。只有 Mode=dev/test 可以不配(本地只验登录)。
	AdminClientId string `json:"AdminClientId,optional"`
	// AdminClientSecretEnv 是存放该应用 ClientSecret 的环境变量名(不是密钥本身)。
	AdminClientSecretEnv string `json:"AdminClientSecretEnv,optional"`
	// AdminClientSecret 是内联明文,只在 Mode=dev/test 放行(指向 sandbox-mock 的本地栈);生产只认 AdminClientSecretEnv。
	AdminClientSecret string `json:"AdminClientSecret,optional"`
}

// resolvePassportProvider 在启动期校验官方通行证段,并解析管理接口的密钥(由 resolveAuthProviders 调用)。
// 返回的密钥为空表示没配管理凭据(只在 dev/test 允许,同时带一条告警)。报错只点字段名,不回显 URL 与密钥。
func resolvePassportProvider(p *PassportAuthConf, relaxed bool, lookup func(string) (string, bool)) (string, []string, error) {
	if err := checkPassportIssuer(p.Issuer, relaxed); err != nil {
		return "", nil, err
	}
	if err := checkPassportClientId("ClientId", p.ClientId); err != nil {
		return "", nil, err
	}
	if err := checkOAuthEndpoint("Passport", p.Endpoint, relaxed); err != nil {
		return "", nil, err
	}

	hasSecretSource := strings.TrimSpace(p.AdminClientSecretEnv) != "" || p.AdminClientSecret != ""
	if !hasSecretSource {
		if p.AdminClientId != "" {
			return "", nil, fmt.Errorf("AuthProviders.Passport.AdminClientId is set without AdminClientSecretEnv: both are needed to delete passport users")
		}
		if !relaxed {
			return "", nil, fmt.Errorf("AuthProviders.Passport requires AdminClientId and AdminClientSecretEnv outside dev/test mode: " +
				"account deletion must also delete the passport user (email, phone, password hash)")
		}
		return "", []string{"AuthProviders.Passport has no AdminClientId/AdminClientSecretEnv: account deletion leaves the passport user " +
			"(email, phone, password hash) in the passport; allowed in dev/test only"}, nil
	}
	if err := checkPassportClientId("AdminClientId", p.AdminClientId); err != nil {
		return "", nil, err
	}
	if p.AdminClientId == p.ClientId {
		return "", nil, fmt.Errorf("AuthProviders.Passport.AdminClientId must be a separate confidential application, not the public game client (ClientId)")
	}
	return resolveAuthCredential("Passport", "AdminClientSecret", p.AdminClientSecretEnv, p.AdminClientSecret, true, relaxed, lookup)
}

// checkPassportClientId 校验一个 Casdoor 应用的 ClientId(公开值):非空、无首尾空白、不是未展开的占位符。
func checkPassportClientId(field, value string) error {
	if containsUnexpandedPlaceholder(value) {
		return fmt.Errorf("AuthProviders.Passport.%s contains an unexpanded ${...}: login does not expand environment variables, write the client ID itself", field)
	}
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("AuthProviders.Passport.%s must not be empty", field)
	}
	if value != strings.TrimSpace(value) {
		return fmt.Errorf("AuthProviders.Passport.%s has leading or trailing whitespace", field)
	}
	return nil
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
