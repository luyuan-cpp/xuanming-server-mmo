package config

import (
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/service"
)

// 本文件钉住第三方登录配置的启动期门禁(auth_providers.go)。背景:login 的 conf.MustLoad 不展开 ${VAR},
// 旧文档教的 `AppSecret: "${WECHAT_APP_SECRET}"` 会把字面量当密钥,微信每次都回「AppSecret 无效」而起服零报错。

const testWeChatSecret = "9f2c7d41a8b3e6f05c1d2e3f4a5b6c7d" // 形如真实 AppSecret 的 32 位十六进制,非占位串

func envLookup(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func weChatConf() *WeChatAuthConf {
	return &WeChatAuthConf{AppId: "wx0123456789abcdef", AppSecretEnv: "LOGIN_WECHAT_APP_SECRET", RequireUnionId: true}
}

func qqConf() *QQAuthConf {
	return &QQAuthConf{AppId: "101234567", RequestUnionId: true, RequireUnionId: true}
}

type authProvidersProbe struct {
	AuthProviders AuthConfig `json:"AuthProviders,optional"`
}

// 块写了但没写 RequireUnionId / RequestUnionId:取 default=true(fail-closed),不是零值 false。
func TestAuthProvidersYamlDefaultsAreFailClosed(t *testing.T) {
	yaml := "AuthProviders:\n" +
		"  WeChat:\n    AppId: wx0123456789abcdef\n    AppSecretEnv: LOGIN_WECHAT_APP_SECRET\n" +
		"  QQ:\n    AppId: \"101234567\"\n"
	var p authProvidersProbe
	if err := conf.LoadFromYamlBytes([]byte(yaml), &p); err != nil {
		t.Fatalf("load: %v", err)
	}
	if p.AuthProviders.WeChat == nil || !p.AuthProviders.WeChat.RequireUnionId {
		t.Fatalf("WeChat.RequireUnionId 缺省必须为 true,got %+v", p.AuthProviders.WeChat)
	}
	if p.AuthProviders.QQ == nil || !p.AuthProviders.QQ.RequireUnionId || !p.AuthProviders.QQ.RequestUnionId {
		t.Fatalf("QQ.RequireUnionId / RequestUnionId 缺省必须为 true,got %+v", p.AuthProviders.QQ)
	}
}

func TestAuthProvidersYamlMissingBlockMeansDisabled(t *testing.T) {
	var p authProvidersProbe
	if err := conf.LoadFromYamlBytes([]byte("Name: login.rpc\n"), &p); err != nil {
		t.Fatalf("load: %v", err)
	}
	if p.AuthProviders.WeChat != nil || p.AuthProviders.QQ != nil {
		t.Fatalf("不写 AuthProviders 就不应启用任何第三方登录,got %+v", p.AuthProviders)
	}
}

func TestResolveAuthProvidersReadsSecretFromNamedEnv(t *testing.T) {
	for _, relaxed := range []bool{false, true} {
		got, _, err := resolveAuthProviders(AuthConfig{WeChat: weChatConf(), QQ: qqConf()}, relaxed,
			envLookup(map[string]string{"LOGIN_WECHAT_APP_SECRET": testWeChatSecret}))
		if err != nil {
			t.Fatalf("relaxed=%v: %v", relaxed, err)
		}
		if got.WeChatAppSecret != testWeChatSecret {
			t.Fatalf("relaxed=%v: WeChat secret not resolved from env", relaxed)
		}
		if got.QQAppKey != "" {
			t.Fatal("QQ 没配 AppKey 时应为空")
		}
	}
}

// 未展开的 ${...} 在任何模式下都拒绝:它永远不可能是真值。
func TestResolveAuthProvidersRejectsUnexpandedPlaceholders(t *testing.T) {
	cases := map[string]func() (AuthConfig, map[string]string){
		"inline secret": func() (AuthConfig, map[string]string) {
			w := weChatConf()
			w.AppSecretEnv, w.AppSecret = "", "${WECHAT_APP_SECRET}"
			return AuthConfig{WeChat: w}, nil
		},
		"app id": func() (AuthConfig, map[string]string) {
			w := weChatConf()
			w.AppId = "${WECHAT_APP_ID}"
			return AuthConfig{WeChat: w}, map[string]string{"LOGIN_WECHAT_APP_SECRET": testWeChatSecret}
		},
		"env value": func() (AuthConfig, map[string]string) {
			return AuthConfig{WeChat: weChatConf()}, map[string]string{"LOGIN_WECHAT_APP_SECRET": "${WECHAT_APP_SECRET}"}
		},
		"endpoint": func() (AuthConfig, map[string]string) {
			w := weChatConf()
			w.Endpoint = "https://${SANDBOX_HOST}"
			return AuthConfig{WeChat: w}, map[string]string{"LOGIN_WECHAT_APP_SECRET": testWeChatSecret}
		},
		"qq app id": func() (AuthConfig, map[string]string) {
			q := qqConf()
			q.AppId = "${QQ_APP_ID}"
			return AuthConfig{QQ: q}, nil
		},
		"qq inline key": func() (AuthConfig, map[string]string) {
			q := qqConf()
			q.AppKey = "${QQ_APP_KEY}"
			return AuthConfig{QQ: q}, nil
		},
	}
	for name, build := range cases {
		for _, relaxed := range []bool{false, true} {
			cfg, env := build()
			if _, _, err := resolveAuthProviders(cfg, relaxed, envLookup(env)); err == nil {
				t.Errorf("%s (relaxed=%v): 未展开的占位符必须拒绝启动", name, relaxed)
			}
		}
	}
}

func TestResolveAuthProvidersInlineSecretOnlyInRelaxedMode(t *testing.T) {
	w := weChatConf()
	w.AppSecretEnv, w.AppSecret = "", testWeChatSecret
	if _, _, err := resolveAuthProviders(AuthConfig{WeChat: w}, false, envLookup(nil)); err == nil {
		t.Fatal("生产模式必须拒绝内联 AppSecret")
	}
	got, warnings, err := resolveAuthProviders(AuthConfig{WeChat: w}, true, envLookup(nil))
	if err != nil {
		t.Fatalf("宽松模式应放行内联 AppSecret: %v", err)
	}
	if got.WeChatAppSecret != testWeChatSecret || len(warnings) == 0 {
		t.Fatalf("宽松模式应解析内联值并告警,warnings=%v", warnings)
	}

	q := qqConf()
	q.AppKey = testWeChatSecret
	if _, _, err := resolveAuthProviders(AuthConfig{QQ: q}, false, envLookup(nil)); err == nil {
		t.Fatal("生产模式同样拒绝内联 QQ AppKey")
	}
}

func TestResolveAuthProvidersMissingOrBadSecret(t *testing.T) {
	cases := map[string]struct {
		conf *WeChatAuthConf
		env  map[string]string
	}{
		"env named but missing":  {conf: weChatConf(), env: nil},
		"env empty":              {conf: weChatConf(), env: map[string]string{"LOGIN_WECHAT_APP_SECRET": "  "}},
		"trailing newline":       {conf: weChatConf(), env: map[string]string{"LOGIN_WECHAT_APP_SECRET": testWeChatSecret + "\n"}},
		"neither env nor inline": {conf: &WeChatAuthConf{AppId: "wx0123456789abcdef", RequireUnionId: true}},
		"empty app id": {conf: &WeChatAuthConf{AppSecretEnv: "LOGIN_WECHAT_APP_SECRET", RequireUnionId: true},
			env: map[string]string{"LOGIN_WECHAT_APP_SECRET": testWeChatSecret}},
	}
	for name, tc := range cases {
		// 显式写错的配置在宽松模式也拒绝,不静默回落。
		for _, relaxed := range []bool{false, true} {
			_, _, err := resolveAuthProviders(AuthConfig{WeChat: tc.conf}, relaxed, envLookup(tc.env))
			if err == nil {
				t.Errorf("%s (relaxed=%v): 应拒绝启动", name, relaxed)
				continue
			}
			if strings.Contains(err.Error(), testWeChatSecret) {
				t.Errorf("%s: 报错回显了密钥", name)
			}
		}
	}
}

func TestResolveAuthProvidersPlaceholderSecretRejectedInProduction(t *testing.T) {
	env := map[string]string{"LOGIN_WECHAT_APP_SECRET": "change-me-wechat-app-secret-000000"}
	if _, _, err := resolveAuthProviders(AuthConfig{WeChat: weChatConf()}, false, envLookup(env)); err == nil {
		t.Fatal("生产模式必须拒绝占位串 AppSecret")
	}
	if _, _, err := resolveAuthProviders(AuthConfig{WeChat: weChatConf()}, true, envLookup(env)); err != nil {
		t.Fatalf("宽松模式不因占位串拒绝(本地沙箱): %v", err)
	}
}

func TestResolveAuthProvidersEndpointPolicy(t *testing.T) {
	env := map[string]string{"LOGIN_WECHAT_APP_SECRET": testWeChatSecret}
	cases := []struct {
		endpoint  string
		relaxedOK bool
		strictOK  bool
	}{
		{endpoint: "", relaxedOK: true, strictOK: true},
		{endpoint: "https://wx-proxy.internal:8443/base", relaxedOK: true, strictOK: true},
		{endpoint: "http://127.0.0.1:18090", relaxedOK: true, strictOK: false},
		{endpoint: "http://sandbox-mock:18090", relaxedOK: true, strictOK: false},
		{endpoint: "ftp://example.com", relaxedOK: false, strictOK: false},
		{endpoint: "https://user:pass@example.com", relaxedOK: false, strictOK: false},
		{endpoint: "https://example.com/?token=x", relaxedOK: false, strictOK: false},
		{endpoint: "https://example.com/#frag", relaxedOK: false, strictOK: false},
		{endpoint: "https:///no-host", relaxedOK: false, strictOK: false},
		{endpoint: "127.0.0.1:18090", relaxedOK: false, strictOK: false},
	}
	for _, tc := range cases {
		for _, relaxed := range []bool{false, true} {
			want := tc.strictOK
			if relaxed {
				want = tc.relaxedOK
			}
			w := weChatConf()
			w.Endpoint = tc.endpoint
			q := qqConf()
			q.Endpoint = tc.endpoint
			for name, cfg := range map[string]AuthConfig{"wechat": {WeChat: w}, "qq": {QQ: q}} {
				_, _, err := resolveAuthProviders(cfg, relaxed, envLookup(env))
				if (err == nil) != want {
					t.Errorf("%s endpoint=%q relaxed=%v: err=%v, want ok=%v", name, tc.endpoint, relaxed, err, want)
				}
				if err != nil && tc.endpoint != "" && strings.Contains(err.Error(), tc.endpoint) {
					t.Errorf("%s endpoint=%q: 报错回显了地址(可能带凭据)", name, tc.endpoint)
				}
			}
		}
	}
}

func TestResolveAuthProvidersQQUnionIdConsistency(t *testing.T) {
	q := qqConf()
	q.RequestUnionId = false
	if _, _, err := resolveAuthProviders(AuthConfig{QQ: q}, false, envLookup(nil)); err == nil {
		t.Fatal("不请求 unionid 却要求 unionid:必须拒绝启动(否则每次登录都失败)")
	}
	q.RequireUnionId = false
	_, warnings, err := resolveAuthProviders(AuthConfig{QQ: q}, false, envLookup(nil))
	if err != nil {
		t.Fatalf("两者都显式关闭应放行: %v", err)
	}
	if len(warnings) == 0 {
		t.Fatal("允许回落 openid 必须告警(账号分裂风险)")
	}
}

func TestResolveAuthProvidersNotConfiguredIsNoop(t *testing.T) {
	got, warnings, err := resolveAuthProviders(AuthConfig{}, false, envLookup(nil))
	if err != nil || got != (AuthProviderSecrets{}) || len(warnings) != 0 {
		t.Fatalf("没配第三方登录应原样放行: got=%+v warnings=%v err=%v", got, warnings, err)
	}
}

// 端到端:ResolveSecrets 的生产门禁覆盖第三方凭据,并把解析结果交给 ActiveSecrets 的消费者。
func TestResolveSecretsIncludesAuthProviders(t *testing.T) {
	cfg := &Config{}
	cfg.Mode = service.ProMode
	cfg.Secrets.GateToken.Value = "a3f9c2e17b6d40f8a1c5e9b2d7f4038e"
	cfg.AuthProviders.WeChat = weChatConf()

	resolved, _, err := ResolveSecrets(cfg, envLookup(map[string]string{"LOGIN_WECHAT_APP_SECRET": testWeChatSecret}))
	if err != nil {
		t.Fatalf("ResolveSecrets: %v", err)
	}
	if resolved.AuthProviders.WeChatAppSecret != testWeChatSecret {
		t.Fatal("ResolvedSecrets.AuthProviders 未带出 WeChat AppSecret")
	}

	if _, _, err := ResolveSecrets(cfg, envLookup(nil)); err == nil {
		t.Fatal("生产模式缺 WeChat AppSecret 环境变量必须拒绝启动")
	}

	cfg.AuthProviders.WeChat.AppSecretEnv, cfg.AuthProviders.WeChat.AppSecret = "", "${WECHAT_APP_SECRET}"
	if _, _, err := ResolveSecrets(cfg, envLookup(nil)); err == nil {
		t.Fatal("旧文档的 ${VAR} 写法必须拒绝启动")
	}
}
