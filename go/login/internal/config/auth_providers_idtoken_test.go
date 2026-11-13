package config

import (
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/service"
)

// 本文件钉住身份令牌渠道(AuthProviders.Apple / Google)的启动期门禁:没有密钥要解析,
// 只校验 ClientId(公开值,规则同 AppId)与取公钥的 Endpoint(生产只接受 https,理由见 checkIdTokenProvider)。

const (
	testAppleClientId  = "com.example.mmorpg"
	testGoogleClientId = "123456789012-abcdefghijklmnopqrstuvwxyz012345.apps.googleusercontent.com"
)

// idTokenSections 把「只配一个身份令牌段」的两种配置列出来,给两段都该如此的用例复用。
func idTokenSections() map[string]func(mutate func(*IdTokenAuthConf)) AuthConfig {
	return map[string]func(func(*IdTokenAuthConf)) AuthConfig{
		"Apple": func(mutate func(*IdTokenAuthConf)) AuthConfig {
			c := &IdTokenAuthConf{ClientId: testAppleClientId}
			mutate(c)
			return AuthConfig{Apple: c}
		},
		"Google": func(mutate func(*IdTokenAuthConf)) AuthConfig {
			c := &IdTokenAuthConf{ClientId: testGoogleClientId}
			mutate(c)
			return AuthConfig{Google: c}
		},
	}
}

func TestIdTokenProvidersYamlSections(t *testing.T) {
	yaml := "AuthProviders:\n" +
		"  Apple:\n    ClientId: " + testAppleClientId + "\n" +
		"  Google:\n    ClientId: " + testGoogleClientId + "\n    Endpoint: https://mock.example.com\n"
	var p authProvidersProbe
	if err := conf.LoadFromYamlBytes([]byte(yaml), &p); err != nil {
		t.Fatalf("load: %v", err)
	}
	if p.AuthProviders.Apple == nil || p.AuthProviders.Apple.ClientId != testAppleClientId || p.AuthProviders.Apple.Endpoint != "" {
		t.Fatalf("Apple 段解析不对: %+v", p.AuthProviders.Apple)
	}
	if p.AuthProviders.Google == nil || p.AuthProviders.Google.Endpoint != "https://mock.example.com" {
		t.Fatalf("Google 段解析不对: %+v", p.AuthProviders.Google)
	}

	var none authProvidersProbe
	if err := conf.LoadFromYamlBytes([]byte("AuthProviders:\n  QQ:\n    AppId: \"101234567\"\n"), &none); err != nil {
		t.Fatalf("load: %v", err)
	}
	if none.AuthProviders.Apple != nil || none.AuthProviders.Google != nil {
		t.Fatalf("没写 Apple / Google 段就不应启用,got %+v", none.AuthProviders)
	}

	// 段写了但没写 ClientId:go-zero 按必填拒绝,不会得到 aud 为空、所有令牌都被拒的渠道。
	var missing authProvidersProbe
	if err := conf.LoadFromYamlBytes([]byte("AuthProviders:\n  Apple:\n    Endpoint: https://mock.example.com\n"), &missing); err == nil {
		t.Fatal("Apple 段缺 ClientId 应加载失败")
	}
}

func TestResolveAuthProvidersAcceptsIdTokenProvidersWithoutSecrets(t *testing.T) {
	cfg := AuthConfig{Apple: &IdTokenAuthConf{ClientId: testAppleClientId}, Google: &IdTokenAuthConf{ClientId: testGoogleClientId}}
	for _, relaxed := range []bool{false, true} {
		got, warnings, err := resolveAuthProviders(cfg, relaxed, envLookup(nil))
		if err != nil {
			t.Fatalf("relaxed=%v: %v", relaxed, err)
		}
		if got != (AuthProviderSecrets{}) {
			t.Fatalf("relaxed=%v: 身份令牌渠道不应解析出任何密钥: %+v", relaxed, got)
		}
		if len(warnings) != 0 {
			t.Fatalf("relaxed=%v: 合规配置不应有告警: %v", relaxed, warnings)
		}
	}
}

// 写错的配置在宽松模式也拒绝,报错点名配置段。
func TestResolveAuthProvidersIdTokenRejectsBadSections(t *testing.T) {
	cases := map[string]func(*IdTokenAuthConf){
		"empty client id":        func(c *IdTokenAuthConf) { c.ClientId = "" },
		"blank client id":        func(c *IdTokenAuthConf) { c.ClientId = "   " },
		"client id whitespace":   func(c *IdTokenAuthConf) { c.ClientId = c.ClientId + "\n" },
		"client id placeholder":  func(c *IdTokenAuthConf) { c.ClientId = "${CLIENT_ID}" },
		"endpoint placeholder":   func(c *IdTokenAuthConf) { c.Endpoint = "https://${SANDBOX_HOST}" },
		"endpoint with userinfo": func(c *IdTokenAuthConf) { c.Endpoint = "https://user:pass@example.com" },
		"endpoint with query":    func(c *IdTokenAuthConf) { c.Endpoint = "https://example.com/?x=1" },
		"endpoint not http(s)":   func(c *IdTokenAuthConf) { c.Endpoint = "ftp://example.com" },
	}
	for caseName, mutate := range cases {
		for section, build := range idTokenSections() {
			for _, relaxed := range []bool{false, true} {
				_, _, err := resolveAuthProviders(build(mutate), relaxed, envLookup(nil))
				if err == nil {
					t.Errorf("%s/%s (relaxed=%v): 应拒绝启动", section, caseName, relaxed)
					continue
				}
				if !strings.Contains(err.Error(), section) {
					t.Errorf("%s/%s: 报错没有点名配置段: %v", section, caseName, err)
				}
				if strings.Contains(err.Error(), "user:pass") {
					t.Errorf("%s/%s: 报错回显了带凭据的地址", section, caseName)
				}
			}
		}
	}
}

// 取公钥的地址:生产只接受 https(公钥被换掉就能伪造任意玩家的令牌),http 只在 dev / test 放行(本地沙箱)。
func TestResolveAuthProvidersIdTokenEndpointPolicy(t *testing.T) {
	for section, build := range idTokenSections() {
		httpEndpoint := build(func(c *IdTokenAuthConf) { c.Endpoint = "http://sandbox-mock:18090" })
		if _, _, err := resolveAuthProviders(httpEndpoint, false, envLookup(nil)); err == nil {
			t.Errorf("%s: 生产模式必须拒绝 http:// 的 Endpoint", section)
		}
		if _, _, err := resolveAuthProviders(httpEndpoint, true, envLookup(nil)); err != nil {
			t.Errorf("%s: 宽松模式应放行 http:// 的沙箱 Endpoint: %v", section, err)
		}
		httpsEndpoint := build(func(c *IdTokenAuthConf) { c.Endpoint = "https://mock.example.com/base" })
		if _, _, err := resolveAuthProviders(httpsEndpoint, false, envLookup(nil)); err != nil {
			t.Errorf("%s: 生产模式应放行 https:// 的 Endpoint: %v", section, err)
		}
	}
}

// Google 的 ClientId 必须是 OAuth 客户端 ID;Apple 的 Bundle ID / Services ID 没有固定后缀,不受这条限制。
func TestResolveAuthProvidersGoogleClientIdMustBeOAuthClientId(t *testing.T) {
	for _, bad := range []string{"123456789012", "com.example.mmorpg", "GOCSPX-secret-looking-value",
		".apps.googleusercontent.com.evil", ".apps.googleusercontent.com"} {
		cfg := AuthConfig{Google: &IdTokenAuthConf{ClientId: bad}}
		for _, relaxed := range []bool{false, true} {
			if _, _, err := resolveAuthProviders(cfg, relaxed, envLookup(nil)); err == nil {
				t.Errorf("Google.ClientId=%q (relaxed=%v): 应拒绝启动", bad, relaxed)
			}
		}
	}
	if _, _, err := resolveAuthProviders(AuthConfig{Apple: &IdTokenAuthConf{ClientId: "com.example.mmorpg"}}, false, envLookup(nil)); err != nil {
		t.Fatalf("Apple 的 Bundle ID 应放行: %v", err)
	}
}

// 端到端:生产模式的 ResolveSecrets 放行合规的 Apple / Google 段,不要求任何环境变量。
func TestResolveSecretsAcceptsIdTokenProvidersInProduction(t *testing.T) {
	cfg := &Config{}
	cfg.Mode = service.ProMode
	cfg.Secrets.GateToken.Value = "a3f9c2e17b6d40f8a1c5e9b2d7f4038e"
	cfg.AuthProviders.Apple = &IdTokenAuthConf{ClientId: testAppleClientId}
	cfg.AuthProviders.Google = &IdTokenAuthConf{ClientId: testGoogleClientId}

	if _, _, err := ResolveSecrets(cfg, envLookup(nil)); err != nil {
		t.Fatalf("ResolveSecrets: %v", err)
	}

	cfg.AuthProviders.Apple.Endpoint = "http://sandbox-mock:18090"
	if _, _, err := ResolveSecrets(cfg, envLookup(nil)); err == nil {
		t.Fatal("生产模式的 ResolveSecrets 必须拒绝 http:// 的取公钥地址")
	}
}
