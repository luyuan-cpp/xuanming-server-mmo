package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"

	"login/internal/config"
	"login/internal/logic/pkg/auth"
)

// 小游戏登录的沙箱端到端依赖两件事:mock 的应答 provider 认得,linux 登录栈的配置过得了 login 的启动门禁。
// 这里用真 provider 打真路由表(newMux),任何一边改了线上格式,另一边在单测里就会红。

func newSandbox(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(newMux())
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestWeChatMiniGameProviderAgainstSandboxMock(t *testing.T) {
	base := newSandbox(t)
	mini := &auth.WeChatMiniGameProvider{AppId: "wx_sandbox_minigame", AppSecret: "s", Endpoint: base}
	app := &auth.WeChatProvider{AppId: "wx_sandbox_appid", AppSecret: "s", Endpoint: base}
	ctx := context.Background()

	first, err := mini.Validate(ctx, "U_alice")
	if err != nil {
		t.Fatalf("js_code 以 U 开头应登录成功: %v", err)
	}
	again, err := mini.Validate(ctx, "U_alice")
	if err != nil || again.Account != first.Account {
		t.Fatalf("同一个 js_code 应得到同一个账号: first=%q again=%+v err=%v", first.Account, again, err)
	}
	if !strings.HasPrefix(first.Account, "wx_unionid_") {
		t.Fatalf("account = %q, want wx_<unionid>", first.Account)
	}
	// mock 对 App 的 code 与小游戏的 js_code 用同一条规则派生 unionid:同一主体下两种形态是同一个账号。
	fromApp, err := app.Validate(ctx, "U_alice")
	if err != nil {
		t.Fatalf("app: %v", err)
	}
	if fromApp.Account != first.Account {
		t.Fatalf("App 与小游戏应解析出同一个账号: app=%q mini=%q", fromApp.Account, first.Account)
	}

	// 不以 U 开头 = 小游戏没绑定开放平台账号、没有 unionid:缺省拒绝,而且是配置侧故障。
	if _, err := mini.Validate(ctx, "alice"); !errors.Is(err, auth.ErrProviderUnavailable) {
		t.Fatalf("缺 unionid 应拒绝并标 ErrProviderUnavailable,实际 %v", err)
	}
	// 过期的 code 是玩家凭证问题,不算故障。
	if _, err := mini.Validate(ctx, "EXPIRED"); err == nil || errors.Is(err, auth.ErrProviderUnavailable) {
		t.Fatalf("过期 code 应按凭证无效拒绝,实际 %v", err)
	}
}

func TestDouyinMiniGameProviderAgainstSandboxMock(t *testing.T) {
	base := newSandbox(t)
	p := &auth.DouyinMiniGameProvider{AppId: "tt_sandbox_minigame", AppSecret: "s", Endpoint: base}
	ctx := context.Background()

	first, err := p.Validate(ctx, "bob")
	if err != nil {
		t.Fatalf("登录应成功: %v", err)
	}
	again, err := p.Validate(ctx, "bob")
	if err != nil || again.Account != first.Account {
		t.Fatalf("同一个 code 应得到同一个账号: first=%q again=%+v err=%v", first.Account, again, err)
	}
	if !strings.HasPrefix(first.Account, "dy_unionid_") {
		t.Fatalf("account = %q, want dy_<unionid>", first.Account)
	}
	other, err := p.Validate(ctx, "carol")
	if err != nil || other.Account == first.Account {
		t.Fatalf("不同的 code 应得到不同的账号: first=%q other=%+v err=%v", first.Account, other, err)
	}
	if _, err := p.Validate(ctx, "EXPIRED"); err == nil || errors.Is(err, auth.ErrProviderUnavailable) {
		t.Fatalf("过期 code 应按凭证无效拒绝,实际 %v", err)
	}
}

// 真实平台的 code2Session 应答带 session_key。provider 不解析它,所以只有直接看 mock 的应答才能钉住
// 「mock 没有悄悄变得比真实平台干净」:否则「session_key 不进日志」的端到端验证会变成空转。
func TestMiniGameHandlersReturnSessionKeyLikeTheRealChannels(t *testing.T) {
	cases := map[string]struct {
		handler http.HandlerFunc
		target  string
	}{
		"wechat": {handler: handleWeChatMiniGameCode2Session,
			target: "/sns/jscode2session?appid=wx1&secret=s&js_code=U_alice&grant_type=authorization_code"},
		"douyin": {handler: handleDouyinMiniGameCode2Session,
			target: "/mgplatform/api/apps/jscode2session?appid=tt1&secret=s&code=bob"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.handler(rec, httptest.NewRequest(http.MethodGet, tc.target, nil))
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			for _, field := range []string{"session_key", "openid", "unionid"} {
				if v, _ := body[field].(string); v == "" {
					t.Errorf("应答缺少 %s: %v", field, body)
				}
			}
		})
	}
}

// linux 登录栈的 login 把小游戏两段也指向本 mock。App 与小游戏的 AppId 写成一样时 login 拒绝启动
// (config/auth_providers.go),这里在单测里就拦住,不等到起容器。
func TestLinuxLoginStackMiniGameSectionsPointAtSandbox(t *testing.T) {
	raw, err := os.ReadFile("../../../../deploy/login-stack.linux/login.yaml")
	if err != nil {
		t.Fatalf("read linux login stack config: %v", err)
	}
	var probe struct {
		AuthProviders config.AuthConfig `json:"AuthProviders,optional"`
	}
	if err := conf.LoadFromYamlBytes(raw, &probe); err != nil {
		t.Fatalf("load linux login stack config: %v", err)
	}
	providers := probe.AuthProviders
	if providers.WeChat == nil || providers.WeChatMiniGame == nil || providers.DouyinMiniGame == nil {
		t.Fatal("linux login stack config no longer configures WeChat / WeChatMiniGame / DouyinMiniGame; update this test together with the stack")
	}
	if providers.WeChatMiniGame.AppId == providers.WeChat.AppId {
		t.Fatalf("WeChatMiniGame.AppId 与 WeChat.AppId 都是 %q:login 会拒绝启动", providers.WeChat.AppId)
	}
	for name, section := range map[string]*config.MiniGameAuthConf{
		"WeChatMiniGame": providers.WeChatMiniGame, "DouyinMiniGame": providers.DouyinMiniGame} {
		if section.Endpoint != providers.WeChat.Endpoint {
			t.Errorf("%s.Endpoint = %q, want the sandbox mock %q", name, section.Endpoint, providers.WeChat.Endpoint)
		}
		if section.AppId == "" || section.AppSecret == "" {
			t.Errorf("%s 缺 AppId 或内联 AppSecret(本栈 Mode=dev,用内联值)", name)
		}
	}
}
