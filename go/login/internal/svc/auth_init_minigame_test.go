package svc

import (
	"fmt"
	"strings"
	"testing"

	"login/internal/config"
	"login/internal/logic/pkg/auth"
)

// 小游戏 provider 的装配:密钥只来自 ResolveSecrets 且各取各的字段,unionid 策略按配置取反。
// 与 auth_init_oauth_test.go 同一纪律:只测纯函数与「注册前就 panic」的路径,不往进程级注册表里写。

const (
	resolvedWeChatMiniGameSecret = "resolved-wechat-minigame-secret"
	resolvedDouyinMiniGameSecret = "resolved-douyin-minigame-secret"
)

func miniGameSecrets() config.AuthProviderSecrets {
	return config.AuthProviderSecrets{
		WeChatAppSecret:         "resolved-wechat-app-secret",
		WeChatMiniGameAppSecret: resolvedWeChatMiniGameSecret,
		DouyinMiniGameAppSecret: resolvedDouyinMiniGameSecret,
	}
}

func TestWeChatMiniGameProviderFromConfigUsesItsOwnResolvedSecret(t *testing.T) {
	conf := &config.MiniGameAuthConf{AppId: "wxfedcba9876543210", AppSecret: "inline-must-not-be-used",
		RequireUnionId: true, Endpoint: "https://wx.example.com"}
	p, err := weChatMiniGameProviderFromConfig(conf, miniGameSecrets())
	if err != nil {
		t.Fatalf("weChatMiniGameProviderFromConfig: %v", err)
	}
	if p.AppSecret != resolvedWeChatMiniGameSecret {
		t.Fatal("微信小游戏必须用 ResolveSecrets 解析出的小游戏密钥,不能读 yaml 内联值,也不能拿 App 或抖音的密钥")
	}
	if p.AllowOpenIdFallback {
		t.Fatal("RequireUnionId=true 时不得允许回落 openid")
	}
	if p.AppId != conf.AppId || p.Endpoint != conf.Endpoint {
		t.Fatalf("AppId / Endpoint 未透传: %+v", p)
	}

	conf.RequireUnionId = false
	p, err = weChatMiniGameProviderFromConfig(conf, miniGameSecrets())
	if err != nil || !p.AllowOpenIdFallback {
		t.Fatalf("RequireUnionId=false 应允许回落 openid: provider=%+v err=%v", p, err)
	}
}

func TestDouyinMiniGameProviderFromConfigUsesItsOwnResolvedSecret(t *testing.T) {
	conf := &config.MiniGameAuthConf{AppId: "tt0123456789abcdef", AppSecret: "inline-must-not-be-used",
		RequireUnionId: true, Endpoint: "https://dy.example.com"}
	p, err := douyinMiniGameProviderFromConfig(conf, miniGameSecrets())
	if err != nil {
		t.Fatalf("douyinMiniGameProviderFromConfig: %v", err)
	}
	if p.AppSecret != resolvedDouyinMiniGameSecret {
		t.Fatal("抖音小游戏必须用 ResolveSecrets 解析出的抖音密钥,不能读 yaml 内联值,也不能拿微信的密钥")
	}
	if p.AllowOpenIdFallback {
		t.Fatal("RequireUnionId=true 时不得允许回落 openid")
	}
	if p.AppId != conf.AppId || p.Endpoint != conf.Endpoint {
		t.Fatalf("AppId / Endpoint 未透传: %+v", p)
	}

	conf.RequireUnionId = false
	p, err = douyinMiniGameProviderFromConfig(conf, miniGameSecrets())
	if err != nil || !p.AllowOpenIdFallback {
		t.Fatalf("RequireUnionId=false 应允许回落 openid: provider=%+v err=%v", p, err)
	}
}

// 只解析出别的渠道的密钥时,小游戏 provider 不能借用它:拒绝,不带空密钥或别人的密钥上线。
func TestMiniGameProvidersFromConfigRejectUnresolvedSecret(t *testing.T) {
	conf := &config.MiniGameAuthConf{AppId: "wxfedcba9876543210", AppSecret: "inline", RequireUnionId: true}
	onlyApp := config.AuthProviderSecrets{WeChatAppSecret: "resolved-wechat-app-secret"}
	if _, err := weChatMiniGameProviderFromConfig(conf, onlyApp); err == nil {
		t.Fatal("微信小游戏密钥没解析出来时必须拒绝")
	}
	if _, err := douyinMiniGameProviderFromConfig(conf, onlyApp); err == nil {
		t.Fatal("抖音小游戏密钥没解析出来时必须拒绝")
	}
}

// 正常启动路径一定先 ResolveSecrets;漏了这一步时 InitAuthProviders 必须在注册前拒绝启动。
func TestInitAuthProvidersRejectsMiniGamesWithoutResolvedSecrets(t *testing.T) {
	sections := map[string]func(*config.Config){
		auth.WeChatMiniGameAuthType: func(c *config.Config) {
			c.AuthProviders.WeChatMiniGame = &config.MiniGameAuthConf{AppId: "wxfedcba9876543210",
				AppSecret: "inline-only", RequireUnionId: true}
		},
		auth.DouyinMiniGameAuthType: func(c *config.Config) {
			c.AuthProviders.DouyinMiniGame = &config.MiniGameAuthConf{AppId: "tt0123456789abcdef",
				AppSecret: "inline-only", RequireUnionId: true}
		},
	}
	for authType, configure := range sections {
		t.Run(authType, func(t *testing.T) {
			previousConfig, previousSecrets := config.AppConfig, config.ActiveSecrets
			t.Cleanup(func() { config.AppConfig, config.ActiveSecrets = previousConfig, previousSecrets })

			config.AppConfig = config.Config{}
			configure(&config.AppConfig)
			config.ActiveSecrets = nil

			defer func() {
				recovered := recover()
				if recovered == nil {
					t.Fatalf("InitAuthProviders 在密钥未解析时注册了 %s", authType)
				}
				if !strings.Contains(fmt.Sprint(recovered), "ResolveSecrets") {
					t.Fatalf("panic = %v, want unresolved-secret validation", recovered)
				}
				if auth.Get(authType) != nil {
					t.Fatal("拒绝启动前不得写入 auth 注册表")
				}
			}()
			InitAuthProviders()
		})
	}
}
