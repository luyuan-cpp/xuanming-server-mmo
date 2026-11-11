// auth_providers.go —— 第三方登录(微信 / QQ)配置的启动期门禁,由 ResolveSecrets 调用。
//
// 已核实的两个坑:
//
//  1. **${VAR} 不会被展开**:login 用 conf.MustLoad 读配置,没带 conf.UseEnv(),go-zero 原样保留
//     `AppSecret: "${WECHAT_APP_SECRET}"` 这串字面量,provider 会拿它去调微信、每次都失败。
//     这里刻意不改成全局 UseEnv:它会对整份 yaml 做 os.ExpandEnv,Redis 口令、正则等任何含 `$`
//     的值都会被静默改写。改为与 PasswordAuth.DSNEnv 同一套纪律 —— 配置里只写环境变量名
//     (AppSecretEnv),任何字段里出现未展开的 `${` 一律拒绝启动。
//  2. **真实凭据进 ConfigMap**:生产模式只认 Env,内联明文只在 Mode=dev/test 放行(本地沙箱)。
//
// 宽松 / 严格档与 secrets.go 同一口径(IsRelaxedMode)。
package config

import (
	"fmt"
	"net/url"
	"strings"
)

// AuthProviderSecrets 是解析后的第三方登录凭据,只存在进程内存里,不进日志。
// 空串表示该 provider 没配(或 QQ 没写当前用不到的 AppKey)。
type AuthProviderSecrets struct {
	WeChatAppSecret string
	QQAppKey        string
}

// containsUnexpandedPlaceholder 判断值里是否有没被展开的 ${...}。
// 真实的 AppId / 密钥 / URL 都不会含 "${",出现即说明有人照旧文档写了占位符。
func containsUnexpandedPlaceholder(v string) bool {
	return strings.Contains(v, "${")
}

// resolveAuthProviders 校验 AuthProviders 段并解析其中的凭据。
//
// 返回 error 表示配置不可用,调用方(ResolveSecrets → main)拒绝启动;warnings 由调用方打日志。
// 报错只点字段名与环境变量名,不回显任何密钥或 URL(URL 里可能带用户信息)。
func resolveAuthProviders(cfg AuthConfig, relaxed bool, lookup func(string) (string, bool)) (AuthProviderSecrets, []string, error) {
	var out AuthProviderSecrets
	var warnings []string

	if w := cfg.WeChat; w != nil {
		if err := checkAuthAppId("WeChat", w.AppId); err != nil {
			return AuthProviderSecrets{}, warnings, err
		}
		if err := checkOAuthEndpoint("WeChat", w.Endpoint, relaxed); err != nil {
			return AuthProviderSecrets{}, warnings, err
		}
		secret, ws, err := resolveAuthCredential("WeChat", "AppSecret", w.AppSecretEnv, w.AppSecret, true, relaxed, lookup)
		if err != nil {
			return AuthProviderSecrets{}, warnings, err
		}
		warnings = append(warnings, ws...)
		out.WeChatAppSecret = secret
		if !w.RequireUnionId {
			warnings = append(warnings, "AuthProviders.WeChat.RequireUnionId=false: accounts are created by openid when unionid is unavailable; "+
				"integrating the Open Platform later (multiple apps sharing one unionid) will split the same player into two accounts that cannot be merged afterwards")
		}
	}

	if q := cfg.QQ; q != nil {
		if err := checkAuthAppId("QQ", q.AppId); err != nil {
			return AuthProviderSecrets{}, warnings, err
		}
		if err := checkOAuthEndpoint("QQ", q.Endpoint, relaxed); err != nil {
			return AuthProviderSecrets{}, warnings, err
		}
		if !q.RequestUnionId && q.RequireUnionId {
			return AuthProviderSecrets{}, warnings, fmt.Errorf(
				"AuthProviders.QQ.RequestUnionId=false 时永远拿不到 unionid,RequireUnionId 必须同时显式写 false")
		}
		key, ws, err := resolveAuthCredential("QQ", "AppKey", q.AppKeyEnv, q.AppKey, false, relaxed, lookup)
		if err != nil {
			return AuthProviderSecrets{}, warnings, err
		}
		warnings = append(warnings, ws...)
		out.QQAppKey = key
		if !q.RequireUnionId {
			warnings = append(warnings, "AuthProviders.QQ.RequireUnionId=false: accounts are created by openid when unionid is unavailable; "+
				"adopting unionid later will split the same player into two accounts that cannot be merged afterwards")
		}
	}

	return out, warnings, nil
}

// checkAuthAppId:AppId 是公开值,可以写在配置里,但不能为空、不能带首尾空白、不能是未展开的占位符。
func checkAuthAppId(provider, appId string) error {
	if containsUnexpandedPlaceholder(appId) {
		return fmt.Errorf("AuthProviders.%s.AppId 含未展开的 ${...}:login 不展开环境变量,请直接写应用号", provider)
	}
	if strings.TrimSpace(appId) == "" {
		return fmt.Errorf("AuthProviders.%s.AppId 不能为空", provider)
	}
	if appId != strings.TrimSpace(appId) {
		return fmt.Errorf("AuthProviders.%s.AppId 含首尾空白", provider)
	}
	return nil
}

// checkOAuthEndpoint 校验沙箱覆盖地址。空串 = 生产地址,直接放行。
// 生产模式只接受 https:Endpoint 上的请求带着 AppSecret / access_token,明文 http 等于把凭据交给链路上的任何人。
func checkOAuthEndpoint(provider, raw string, relaxed bool) error {
	if raw == "" {
		return nil
	}
	if containsUnexpandedPlaceholder(raw) {
		return fmt.Errorf("AuthProviders.%s.Endpoint 含未展开的 ${...}", provider)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("AuthProviders.%s.Endpoint 不是合法 URL", provider)
	}
	switch scheme := strings.ToLower(u.Scheme); {
	case scheme == "https":
	case scheme == "http" && relaxed:
	default:
		return fmt.Errorf("AuthProviders.%s.Endpoint 生产模式只接受 https://(http:// 只在 Mode=dev/test 放行)", provider)
	}
	if u.Opaque != "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return fmt.Errorf("AuthProviders.%s.Endpoint 须是 scheme://主机[:端口][/路径],不带用户信息、查询串与片段", provider)
	}
	return nil
}

// resolveAuthCredential 解析一个第三方凭据:Env 优先,内联明文只在宽松模式放行。
// required=false 时两者都没写返回空串(QQ 的 AppKey 当前用不到)。
//
// 显式写了 Env 但环境变量缺失 / 为空 —— 任何模式都直接报错,不回落到内联值,与 SecretConf.resolve 同一口径。
func resolveAuthCredential(provider, field, envName, inline string, required, relaxed bool,
	lookup func(string) (string, bool)) (string, []string, error) {
	envField := field + "Env"
	if containsUnexpandedPlaceholder(envName) || containsUnexpandedPlaceholder(inline) {
		return "", nil, fmt.Errorf("AuthProviders.%s.%s 含未展开的 ${...}:login 不展开环境变量,请在 %s 里写环境变量名",
			provider, field, envField)
	}

	if name := strings.TrimSpace(envName); name != "" {
		v, ok := lookup(name)
		if !ok || strings.TrimSpace(v) == "" {
			return "", nil, fmt.Errorf("AuthProviders.%s.%s=%q,但该环境变量缺失或为空", provider, envField, name)
		}
		if err := checkCredentialValue(provider, field, v, relaxed); err != nil {
			return "", nil, err
		}
		var warnings []string
		if inline != "" {
			warnings = append(warnings, fmt.Sprintf("AuthProviders.%s sets both %s and %s; %s takes precedence, remove the inline value",
				provider, envField, field, envField))
		}
		return v, warnings, nil
	}

	if inline == "" {
		if required {
			return "", nil, fmt.Errorf("AuthProviders.%s 需要 %s(存放 %s 的环境变量名)", provider, envField, field)
		}
		return "", nil, nil
	}
	if !relaxed {
		return "", nil, fmt.Errorf("生产模式不接受内联的 AuthProviders.%s.%s:真实凭据只能经 %s 指向的环境变量注入",
			provider, field, envField)
	}
	if err := checkCredentialValue(provider, field, inline, relaxed); err != nil {
		return "", nil, err
	}
	return inline, []string{fmt.Sprintf("AuthProviders.%s.%s uses inline plaintext, local development only; use %s in production", provider, field, envField)}, nil
}

// checkCredentialValue 拦住「看起来就不对」的凭据值。
func checkCredentialValue(provider, field, v string, relaxed bool) error {
	if containsUnexpandedPlaceholder(v) {
		return fmt.Errorf("AuthProviders.%s.%s 的值含未展开的 ${...}", provider, field)
	}
	// 常见于 kubectl create secret --from-file 带进的行尾换行:原样发给渠道只会得到「密钥无效」,
	// 而且看日志查不出来。这里不替运维 trim(那会让同一份 Secret 在不同服务里含义不同),直接拒绝。
	if v != strings.TrimSpace(v) {
		return fmt.Errorf("AuthProviders.%s.%s 的值含首尾空白(常见于 Secret 带进的换行)", provider, field)
	}
	if !relaxed && IsPlaceholderSecret(v) {
		return fmt.Errorf("AuthProviders.%s.%s 是占位串:它已经进过 git / 部署脚本 / 文档,等于公开值", provider, field)
	}
	return nil
}
