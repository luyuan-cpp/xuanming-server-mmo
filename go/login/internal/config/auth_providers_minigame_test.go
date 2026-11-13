package config

import (
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/service"
)

// 本文件钉住小游戏渠道(AuthProviders.WeChatMiniGame / DouyinMiniGame)的启动期门禁。规则与 WeChat 段相同
// (auth_providers_test.go),这里只验证两段各自接上了这套门禁,以及两段的密钥不会串到别的字段上。

const (
	wechatMiniGameSecretEnv = "LOGIN_WECHAT_MINIGAME_APP_SECRET"
	douyinMiniGameSecretEnv = "LOGIN_DOUYIN_MINIGAME_APP_SECRET"
	// 形如真实 AppSecret 的 32 位十六进制,非占位串;三个值互不相同,串了字段能看出来。
	testWeChatMiniGameSecret = "5b1e0c7fa9d24836b0c4e7d19f3a6c52"
	testDouyinMiniGameSecret = "c07d3a91e5f84b26a1d0937e6b5c8f14"
)

func weChatMiniGameConf() *MiniGameAuthConf {
	return &MiniGameAuthConf{AppId: "wxfedcba9876543210", AppSecretEnv: wechatMiniGameSecretEnv, RequireUnionId: true}
}

func douyinMiniGameConf() *MiniGameAuthConf {
	return &MiniGameAuthConf{AppId: "tt0123456789abcdef", AppSecretEnv: douyinMiniGameSecretEnv, RequireUnionId: true}
}

func miniGameEnv() map[string]string {
	return map[string]string{
		"LOGIN_WECHAT_APP_SECRET": testWeChatSecret,
		wechatMiniGameSecretEnv:   testWeChatMiniGameSecret,
		douyinMiniGameSecretEnv:   testDouyinMiniGameSecret,
	}
}

// miniGameSections 把「只配一个小游戏段」的两种配置列出来,给两段都该如此的用例复用。
// build 拿到的是一份新的段配置,可以随意改。
func miniGameSections() map[string]func(mutate func(*MiniGameAuthConf)) AuthConfig {
	return map[string]func(func(*MiniGameAuthConf)) AuthConfig{
		"WeChatMiniGame": func(mutate func(*MiniGameAuthConf)) AuthConfig {
			m := weChatMiniGameConf()
			mutate(m)
			return AuthConfig{WeChatMiniGame: m}
		},
		"DouyinMiniGame": func(mutate func(*MiniGameAuthConf)) AuthConfig {
			m := douyinMiniGameConf()
			mutate(m)
			return AuthConfig{DouyinMiniGame: m}
		},
	}
}

// 段写了但没写 RequireUnionId:取 default=true(fail-closed),不是零值 false;没写的段是 nil(不启用)。
func TestMiniGameYamlDefaultsAreFailClosed(t *testing.T) {
	yaml := "AuthProviders:\n" +
		"  WeChatMiniGame:\n    AppId: wxfedcba9876543210\n    AppSecretEnv: " + wechatMiniGameSecretEnv + "\n" +
		"  DouyinMiniGame:\n    AppId: tt0123456789abcdef\n    AppSecretEnv: " + douyinMiniGameSecretEnv + "\n"
	var p authProvidersProbe
	if err := conf.LoadFromYamlBytes([]byte(yaml), &p); err != nil {
		t.Fatalf("load: %v", err)
	}
	if p.AuthProviders.WeChatMiniGame == nil || !p.AuthProviders.WeChatMiniGame.RequireUnionId {
		t.Fatalf("WeChatMiniGame.RequireUnionId 缺省必须为 true,got %+v", p.AuthProviders.WeChatMiniGame)
	}
	if p.AuthProviders.DouyinMiniGame == nil || !p.AuthProviders.DouyinMiniGame.RequireUnionId {
		t.Fatalf("DouyinMiniGame.RequireUnionId 缺省必须为 true,got %+v", p.AuthProviders.DouyinMiniGame)
	}
	if p.AuthProviders.WeChat != nil || p.AuthProviders.QQ != nil {
		t.Fatalf("只写小游戏段不应启用 App 形态的渠道,got %+v", p.AuthProviders)
	}

	var none authProvidersProbe
	if err := conf.LoadFromYamlBytes([]byte("AuthProviders:\n  QQ:\n    AppId: \"101234567\"\n"), &none); err != nil {
		t.Fatalf("load: %v", err)
	}
	if none.AuthProviders.WeChatMiniGame != nil || none.AuthProviders.DouyinMiniGame != nil {
		t.Fatalf("没写小游戏段就不应启用小游戏登录,got %+v", none.AuthProviders)
	}
}

// 三个渠道同时配置:每个密钥进自己的字段,不串。
func TestResolveAuthProvidersMiniGameSecretsGoToTheirOwnFields(t *testing.T) {
	cfg := AuthConfig{WeChat: weChatConf(), WeChatMiniGame: weChatMiniGameConf(), DouyinMiniGame: douyinMiniGameConf()}
	for _, relaxed := range []bool{false, true} {
		got, warnings, err := resolveAuthProviders(cfg, relaxed, envLookup(miniGameEnv()))
		if err != nil {
			t.Fatalf("relaxed=%v: %v", relaxed, err)
		}
		want := AuthProviderSecrets{WeChatAppSecret: testWeChatSecret,
			WeChatMiniGameAppSecret: testWeChatMiniGameSecret, DouyinMiniGameAppSecret: testDouyinMiniGameSecret}
		if got != want {
			t.Fatalf("relaxed=%v: 密钥解析结果与配置不对应", relaxed)
		}
		if len(warnings) != 0 {
			t.Fatalf("relaxed=%v: 合规配置不应有告警: %v", relaxed, warnings)
		}
	}
}

func TestResolveAuthProvidersMiniGameInlineSecretOnlyInRelaxedMode(t *testing.T) {
	for name, build := range miniGameSections() {
		cfg := build(func(m *MiniGameAuthConf) { m.AppSecretEnv, m.AppSecret = "", testWeChatMiniGameSecret })
		if _, _, err := resolveAuthProviders(cfg, false, envLookup(nil)); err == nil {
			t.Errorf("%s: 生产模式必须拒绝内联 AppSecret", name)
		}
		got, warnings, err := resolveAuthProviders(cfg, true, envLookup(nil))
		if err != nil {
			t.Errorf("%s: 宽松模式应放行内联 AppSecret: %v", name, err)
			continue
		}
		if got.WeChatMiniGameAppSecret+got.DouyinMiniGameAppSecret != testWeChatMiniGameSecret || len(warnings) == 0 {
			t.Errorf("%s: 宽松模式应解析内联值并告警,warnings=%v", name, warnings)
		}
	}
}

// 显式写错的配置在宽松模式也拒绝,报错不回显密钥与地址。
func TestResolveAuthProvidersMiniGameRejectsBadSections(t *testing.T) {
	cases := map[string]struct {
		mutate func(*MiniGameAuthConf)
		env    map[string]string
	}{
		"env named but missing": {mutate: func(*MiniGameAuthConf) {}, env: map[string]string{}},
		"env value blank": {mutate: func(*MiniGameAuthConf) {},
			env: map[string]string{wechatMiniGameSecretEnv: "  ", douyinMiniGameSecretEnv: "  "}},
		"env value with trailing newline": {mutate: func(*MiniGameAuthConf) {},
			env: map[string]string{wechatMiniGameSecretEnv: testWeChatMiniGameSecret + "\n",
				douyinMiniGameSecretEnv: testDouyinMiniGameSecret + "\n"}},
		"env value is unexpanded placeholder": {mutate: func(*MiniGameAuthConf) {},
			env: map[string]string{wechatMiniGameSecretEnv: "${SECRET}", douyinMiniGameSecretEnv: "${SECRET}"}},
		"neither env nor inline": {mutate: func(m *MiniGameAuthConf) { m.AppSecretEnv = "" }, env: miniGameEnv()},
		"inline placeholder":     {mutate: func(m *MiniGameAuthConf) { m.AppSecretEnv, m.AppSecret = "", "${SECRET}" }, env: miniGameEnv()},
		"empty app id":           {mutate: func(m *MiniGameAuthConf) { m.AppId = "" }, env: miniGameEnv()},
		"app id placeholder":     {mutate: func(m *MiniGameAuthConf) { m.AppId = "${APP_ID}" }, env: miniGameEnv()},
		"app id with whitespace": {mutate: func(m *MiniGameAuthConf) { m.AppId = " wxfedcba9876543210" }, env: miniGameEnv()},
		"endpoint placeholder":   {mutate: func(m *MiniGameAuthConf) { m.Endpoint = "https://${SANDBOX_HOST}" }, env: miniGameEnv()},
		"endpoint with userinfo": {mutate: func(m *MiniGameAuthConf) { m.Endpoint = "https://user:pass@example.com" }, env: miniGameEnv()},
	}
	for caseName, tc := range cases {
		for section, build := range miniGameSections() {
			for _, relaxed := range []bool{false, true} {
				_, _, err := resolveAuthProviders(build(tc.mutate), relaxed, envLookup(tc.env))
				if err == nil {
					t.Errorf("%s/%s (relaxed=%v): 应拒绝启动", section, caseName, relaxed)
					continue
				}
				for _, secret := range []string{testWeChatMiniGameSecret, testDouyinMiniGameSecret, "user:pass"} {
					if strings.Contains(err.Error(), secret) {
						t.Errorf("%s/%s: 报错回显了密钥或带凭据的地址", section, caseName)
					}
				}
				if !strings.Contains(err.Error(), section) {
					t.Errorf("%s/%s: 报错没有点名配置段: %v", section, caseName, err)
				}
			}
		}
	}
}

func TestResolveAuthProvidersMiniGameEndpointAndPlaceholderPolicy(t *testing.T) {
	for section, build := range miniGameSections() {
		httpEndpoint := build(func(m *MiniGameAuthConf) { m.Endpoint = "http://sandbox-mock:18090" })
		if _, _, err := resolveAuthProviders(httpEndpoint, false, envLookup(miniGameEnv())); err == nil {
			t.Errorf("%s: 生产模式必须拒绝 http:// 的 Endpoint(请求里带 AppSecret)", section)
		}
		if _, _, err := resolveAuthProviders(httpEndpoint, true, envLookup(miniGameEnv())); err != nil {
			t.Errorf("%s: 宽松模式应放行 http:// 的沙箱 Endpoint: %v", section, err)
		}
		httpsEndpoint := build(func(m *MiniGameAuthConf) { m.Endpoint = "https://mock.example.com/base" })
		if _, _, err := resolveAuthProviders(httpsEndpoint, false, envLookup(miniGameEnv())); err != nil {
			t.Errorf("%s: 生产模式应放行 https:// 的 Endpoint: %v", section, err)
		}

		placeholderEnv := map[string]string{
			wechatMiniGameSecretEnv: "change-me-wechat-minigame-secret-00",
			douyinMiniGameSecretEnv: "change-me-douyin-minigame-secret-00",
		}
		plain := build(func(*MiniGameAuthConf) {})
		if _, _, err := resolveAuthProviders(plain, false, envLookup(placeholderEnv)); err == nil {
			t.Errorf("%s: 生产模式必须拒绝占位串 AppSecret", section)
		}
		if _, _, err := resolveAuthProviders(plain, true, envLookup(placeholderEnv)); err != nil {
			t.Errorf("%s: 宽松模式不因占位串拒绝(本地沙箱): %v", section, err)
		}
	}
}

func TestResolveAuthProvidersMiniGameOpenIdFallbackWarnsByName(t *testing.T) {
	for section, build := range miniGameSections() {
		cfg := build(func(m *MiniGameAuthConf) { m.RequireUnionId = false })
		_, warnings, err := resolveAuthProviders(cfg, false, envLookup(miniGameEnv()))
		if err != nil {
			t.Errorf("%s: 显式允许回落 openid 应放行: %v", section, err)
			continue
		}
		named := false
		for _, w := range warnings {
			if strings.Contains(w, "AuthProviders."+section+".RequireUnionId=false") {
				named = true
			}
		}
		if !named {
			t.Errorf("%s: 允许回落 openid 必须告警并点名配置段(账号分裂风险),warnings=%v", section, warnings)
		}
	}
}

// App 与小游戏是两个应用,应用号不会相同:写成一样说明有一段抄错了,起服就拒绝。
func TestResolveAuthProvidersRejectsSameAppIdForWeChatAppAndMiniGame(t *testing.T) {
	app, mini := weChatConf(), weChatMiniGameConf()
	mini.AppId = app.AppId
	for _, relaxed := range []bool{false, true} {
		_, _, err := resolveAuthProviders(AuthConfig{WeChat: app, WeChatMiniGame: mini}, relaxed, envLookup(miniGameEnv()))
		if err == nil {
			t.Fatalf("relaxed=%v: WeChat 与 WeChatMiniGame 的 AppId 相同必须拒绝启动", relaxed)
		}
	}
	// 只配小游戏、或者两段的 AppId 不同,都不受这条限制。
	if _, _, err := resolveAuthProviders(AuthConfig{WeChatMiniGame: mini}, false, envLookup(miniGameEnv())); err != nil {
		t.Fatalf("只配小游戏段不应被 AppId 查重拒绝: %v", err)
	}
	mini.AppId = "wxfedcba9876543210"
	if _, _, err := resolveAuthProviders(AuthConfig{WeChat: app, WeChatMiniGame: mini}, false, envLookup(miniGameEnv())); err != nil {
		t.Fatalf("AppId 不同应放行: %v", err)
	}
}

// 端到端:ResolveSecrets 的生产门禁覆盖小游戏凭据,并把解析结果交给 ActiveSecrets 的消费者。
func TestResolveSecretsIncludesMiniGameProviders(t *testing.T) {
	cfg := &Config{}
	cfg.Mode = service.ProMode
	cfg.Secrets.GateToken.Value = "a3f9c2e17b6d40f8a1c5e9b2d7f4038e"
	cfg.AuthProviders.WeChatMiniGame = weChatMiniGameConf()
	cfg.AuthProviders.DouyinMiniGame = douyinMiniGameConf()

	resolved, _, err := ResolveSecrets(cfg, envLookup(miniGameEnv()))
	if err != nil {
		t.Fatalf("ResolveSecrets: %v", err)
	}
	if resolved.AuthProviders.WeChatMiniGameAppSecret != testWeChatMiniGameSecret ||
		resolved.AuthProviders.DouyinMiniGameAppSecret != testDouyinMiniGameSecret {
		t.Fatal("ResolvedSecrets.AuthProviders 未带出小游戏的 AppSecret")
	}

	if _, _, err := ResolveSecrets(cfg, envLookup(map[string]string{wechatMiniGameSecretEnv: testWeChatMiniGameSecret})); err == nil {
		t.Fatal("生产模式缺抖音小游戏的 AppSecret 环境变量必须拒绝启动")
	}
}
