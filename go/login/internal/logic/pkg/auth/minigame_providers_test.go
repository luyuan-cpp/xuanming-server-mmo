package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/logx/logtest"
)

// 本文件钉住小游戏登录(微信 wx.login / 抖音 tt.login)的行为:
//   - 调的是小游戏的 code2Session,参数名与 App 形态不同(微信 js_code,抖音 code + Content-Type);
//   - 账号名用 unionid,缺失默认 fail-closed,与 App 形态同一口径;微信 App 与小游戏同 unionid 即同账号;
//   - 渠道 / 本服务配置侧的失败标 ErrProviderUnavailable,玩家凭证无效不标;
//   - session_key、AppSecret、code 不进日志和错误文案。

const (
	wechatMiniGamePath = "/sns/jscode2session"
	douyinMiniGamePath = "/mgplatform/api/apps/jscode2session"
)

// miniGameProviders 用同一个 HTTP 客户端构造两个渠道的 provider,给「两个渠道都该如此」的用例复用。
func miniGameProviders(cli *http.Client, allowOpenIdFallback bool) map[string]Provider {
	return map[string]Provider{
		WeChatMiniGameAuthType: &WeChatMiniGameProvider{AppId: "wxmini", AppSecret: "secret", HTTP: cli,
			AllowOpenIdFallback: allowOpenIdFallback},
		DouyinMiniGameAuthType: &DouyinMiniGameProvider{AppId: "ttmini", AppSecret: "secret", HTTP: cli,
			AllowOpenIdFallback: allowOpenIdFallback},
	}
}

// serveMiniGames 让两个渠道的 code2Session 路径都回同一个 handler。
func serveMiniGames(t *testing.T, handler http.HandlerFunc) *http.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(wechatMiniGamePath, handler)
	mux.HandleFunc(douyinMiniGamePath, handler)
	cli, srv := newTestClient(mux)
	t.Cleanup(srv.Close)
	return cli
}

func TestWeChatMiniGameProviderCallsCode2SessionAndUsesUnionId(t *testing.T) {
	cli := serveOAuth(t, wechatMiniGamePath, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("appid") != "wxmini" || q.Get("secret") != "secret" || q.Get("js_code") != "CODE1" ||
			q.Get("grant_type") != "authorization_code" {
			t.Errorf("code2Session 参数不对: appid=%q js_code=%q grant_type=%q",
				q.Get("appid"), q.Get("js_code"), q.Get("grant_type"))
		}
		if q.Has("code") {
			t.Error("小游戏的凭证参数名是 js_code,不是 App 形态的 code")
		}
		_, _ = w.Write([]byte(`{"openid":"OPEN1","session_key":"SK","unionid":"UNION1"}`))
	})
	p := &WeChatMiniGameProvider{AppId: "wxmini", AppSecret: "secret", HTTP: cli}
	res, err := p.Validate(context.Background(), "CODE1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Account != "wx_UNION1" {
		t.Fatalf("account = %q, want wx_UNION1", res.Account)
	}
}

func TestDouyinMiniGameProviderCallsCode2SessionAndUsesUnionId(t *testing.T) {
	cli := serveOAuth(t, douyinMiniGamePath, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("appid") != "ttmini" || q.Get("secret") != "secret" || q.Get("code") != "CODE1" {
			t.Errorf("code2Session 参数不对: appid=%q code=%q", q.Get("appid"), q.Get("code"))
		}
		if q.Has("anonymous_code") || q.Has("js_code") || q.Has("grant_type") {
			t.Errorf("不该出现的参数: %v", q)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json(抖音文档要求)", got)
		}
		_, _ = w.Write([]byte(`{"error":0,"session_key":"SK","openid":"OPEN1","anonymous_openid":"","unionid":"UNION1"}`))
	})
	p := &DouyinMiniGameProvider{AppId: "ttmini", AppSecret: "secret", HTTP: cli}
	res, err := p.Validate(context.Background(), "CODE1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Account != "dy_UNION1" {
		t.Fatalf("account = %q, want dy_UNION1", res.Account)
	}
}

// App 与小游戏挂在同一个开放平台主体下时 unionid 相同:两个 provider 必须解析出同一个账号名,
// 否则同一个人从 App 和小游戏进来会是两套角色。
func TestWeChatMiniGameAndAppResolveSameAccountForSameUnionId(t *testing.T) {
	app := &WeChatProvider{AppId: "wxapp", AppSecret: "s1", HTTP: serveOAuth(t, "/sns/oauth2/access_token",
		writeBody(`{"access_token":"AT","openid":"APP_OPEN","unionid":"UNION1"}`))}
	mini := &WeChatMiniGameProvider{AppId: "wxmini", AppSecret: "s2", HTTP: serveOAuth(t, wechatMiniGamePath,
		writeBody(`{"openid":"MINI_OPEN","session_key":"SK","unionid":"UNION1"}`))}

	fromApp, err := app.Validate(context.Background(), "app-code")
	if err != nil {
		t.Fatalf("app: %v", err)
	}
	fromMini, err := mini.Validate(context.Background(), "mini-code")
	if err != nil {
		t.Fatalf("mini game: %v", err)
	}
	if fromApp.Account != fromMini.Account {
		t.Fatalf("同一个 unionid 得到两个账号: app=%q mini=%q", fromApp.Account, fromMini.Account)
	}
}

func TestMiniGameProvidersMissingUnionIdFailClosedByDefault(t *testing.T) {
	cli := serveMiniGames(t, writeBody(`{"error":0,"openid":"OPEN1","session_key":"SK"}`))
	for name, p := range miniGameProviders(cli, false) {
		t.Run(name, func(t *testing.T) {
			res, err := p.Validate(context.Background(), "CODE1")
			if err == nil {
				t.Fatalf("缺 unionid 时应拒绝,实际得到账号 %q", res.Account)
			}
			if !errors.Is(err, ErrProviderUnavailable) {
				t.Fatalf("缺 unionid 是应用配置问题,应标 ErrProviderUnavailable,实际 %v", err)
			}
		})
	}
}

func TestMiniGameProvidersFallBackToOpenIdOnlyWhenAllowed(t *testing.T) {
	cli := serveMiniGames(t, writeBody(`{"error":0,"openid":"OPEN1","session_key":"SK"}`))
	want := map[string]string{WeChatMiniGameAuthType: "wx_OPEN1", DouyinMiniGameAuthType: "dy_OPEN1"}
	for name, p := range miniGameProviders(cli, true) {
		t.Run(name, func(t *testing.T) {
			res, err := p.Validate(context.Background(), "CODE1")
			if err != nil {
				t.Fatalf("显式允许回落 openid 时应放行: %v", err)
			}
			if res.Account != want[name] {
				t.Fatalf("account = %q, want %q", res.Account, want[name])
			}
		})
	}
}

func TestWeChatMiniGameErrCodeClassification(t *testing.T) {
	cases := []struct {
		errcode      string
		providerSide bool
	}{
		{errcode: "40029", providerSide: false}, // js_code 无效
		{errcode: "40163", providerSide: false}, // code 已被使用
		{errcode: "40226", providerSide: false}, // 高风险用户被微信拦截:是那个玩家的问题
		{errcode: "45011", providerSide: false}, // code2Session 上是按用户限频,不算本服务故障
		{errcode: "-1", providerSide: true},     // 微信系统繁忙
		{errcode: "40013", providerSide: true},  // AppId 无效
		{errcode: "40125", providerSide: true},  // AppSecret 无效
		{errcode: "45009", providerSide: true},  // 应用级调用量超限
	}
	for _, tc := range cases {
		t.Run(tc.errcode, func(t *testing.T) {
			cli := serveOAuth(t, wechatMiniGamePath, writeBody(`{"errcode":`+tc.errcode+`,"errmsg":"x"}`))
			p := &WeChatMiniGameProvider{AppId: "wxmini", AppSecret: "secret", HTTP: cli}
			_, err := p.Validate(context.Background(), "CODE1")
			if err == nil {
				t.Fatal("errcode 非 0 必须拒绝")
			}
			if got := errors.Is(err, ErrProviderUnavailable); got != tc.providerSide {
				t.Fatalf("errcode=%s ErrProviderUnavailable=%v,want %v(err=%v)", tc.errcode, got, tc.providerSide, err)
			}
		})
	}
}

// 抖音没有可核对的错误码表:error / errcode 非 0 一律按凭证无效,不标故障(与 QQ 同一处理)。
func TestDouyinMiniGameErrorsAreNotProviderFaults(t *testing.T) {
	for name, body := range map[string]string{
		"error with errcode": `{"errcode":40018,"errmsg":"bad code","error":1,"message":"bad code"}`,
		"error only":         `{"error":1}`,
		"errcode only":       `{"errcode":40015,"errmsg":"bad appid"}`,
	} {
		t.Run(name, func(t *testing.T) {
			cli := serveOAuth(t, douyinMiniGamePath, writeBody(body))
			p := &DouyinMiniGameProvider{AppId: "ttmini", AppSecret: "secret", HTTP: cli}
			res, err := p.Validate(context.Background(), "CODE1")
			if err == nil {
				t.Fatalf("错误应答必须拒绝,实际得到账号 %q", res.Account)
			}
			if errors.Is(err, ErrProviderUnavailable) {
				t.Fatalf("抖音的错误应答不应标 ErrProviderUnavailable:%v", err)
			}
		})
	}
}

// 非 2xx、超长应答、坏 JSON、成功应答却无 openid:都不是玩家凭证的问题。
func TestMiniGameProvidersTransportAndShapeFailuresAreProviderFaults(t *testing.T) {
	oversized := `{"openid":"` + strings.Repeat("a", maxOAuthResponseBytes) + `"}`
	cases := map[string]http.HandlerFunc{
		"http 502": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":0,"openid":"OPEN1","unionid":"U"}`))
		},
		"http 429":        func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) },
		"oversized body":  writeBody(oversized),
		"bad json":        writeBody(`<html>gateway error</html>`),
		"no openid in ok": writeBody(`{"error":0,"unionid":"U","session_key":"SK"}`),
	}
	for name, handler := range cases {
		cli := serveMiniGames(t, handler)
		for providerName, p := range miniGameProviders(cli, false) {
			t.Run(providerName+"/"+name, func(t *testing.T) {
				_, err := p.Validate(context.Background(), "CODE1")
				if !errors.Is(err, ErrProviderUnavailable) {
					t.Fatalf("应标 ErrProviderUnavailable,实际 %v", err)
				}
			})
		}
	}
}

// 空 code、没配 AppId / AppSecret:不发请求。前者是玩家凭证问题,后者是本服务配置问题。
func TestMiniGameProvidersRejectBeforeCallingChannel(t *testing.T) {
	called := false
	cli := &http.Client{Transport: credentialProbeTransport(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("must not be called")
	})}

	for name, p := range miniGameProviders(cli, false) {
		if _, err := p.Validate(context.Background(), ""); err == nil || errors.Is(err, ErrProviderUnavailable) {
			t.Errorf("%s: 空 code 应按凭证无效拒绝,实际 %v", name, err)
		}
	}
	unconfigured := map[string]Provider{
		WeChatMiniGameAuthType: &WeChatMiniGameProvider{AppId: "wxmini", HTTP: cli},
		DouyinMiniGameAuthType: &DouyinMiniGameProvider{AppSecret: "secret", HTTP: cli},
	}
	for name, p := range unconfigured {
		if _, err := p.Validate(context.Background(), "CODE1"); !errors.Is(err, ErrProviderUnavailable) {
			t.Errorf("%s: 缺 AppId / AppSecret 应标 ErrProviderUnavailable,实际 %v", name, err)
		}
	}
	if called {
		t.Fatal("拒绝之前不应访问渠道")
	}
}

// session_key 是小游戏端的签名密钥:成功日志与失败文案里都不许出现。
func TestMiniGameProvidersNeverLogSessionKey(t *testing.T) {
	const sessionKey = "SYNTHETIC_SESSION_KEY_0123456789"
	const openid = "oPENID_full_value_0123456789"
	bodies := map[string]string{
		"success":          `{"error":0,"openid":"` + openid + `","unionid":"UNION1","session_key":"` + sessionKey + `"}`,
		"missing unionid":  `{"error":0,"openid":"` + openid + `","session_key":"` + sessionKey + `"}`,
		"channel rejected": `{"error":1,"errcode":40029,"errmsg":"` + sessionKey + `","session_key":"` + sessionKey + `"}`,
	}
	for scenario, body := range bodies {
		cli := serveMiniGames(t, writeBody(body))
		for name, p := range miniGameProviders(cli, false) {
			t.Run(name+"/"+scenario, func(t *testing.T) {
				logs := logtest.NewCollector(t)
				_, err := p.Validate(context.Background(), "CODE1")
				if scenario == "success" && err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if err != nil {
					logx.Errorf("Auth failed: %v", err)
				}
				out := logs.String()
				if strings.Contains(out, sessionKey) {
					t.Fatal("session_key 进了日志")
				}
				if strings.Contains(out, openid) {
					t.Fatal("日志整段输出了 openid")
				}
				if scenario == "success" && !strings.Contains(out, maskOAuthId(openid)) {
					t.Fatalf("成功日志缺少脱敏后的 openid:%s", out)
				}
			})
		}
	}
}

// 请求 URL 里有 AppSecret 和 code,渠道应答也可能回显它们:错误文案与调用方日志都不能带出来。
func TestMiniGameProviderErrorsDoNotExposeCredentials(t *testing.T) {
	const sentinel = "SYNTHETIC_MINIGAME_CREDENTIAL"
	for _, providerName := range []string{WeChatMiniGameAuthType, DouyinMiniGameAuthType} {
		for _, scenario := range []string{"http", "build", "body", "json", "channel", "cancel", "deadline"} {
			t.Run(providerName+"/"+scenario, func(t *testing.T) {
				logs := logtest.NewCollector(t)
				client := &http.Client{Transport: credentialProbeTransport(func(*http.Request) (*http.Response, error) {
					switch scenario {
					case "http":
						return nil, errors.New("transport echoed " + sentinel)
					case "cancel":
						return nil, context.Canceled
					case "deadline":
						return nil, context.DeadlineExceeded
					}
					var body io.ReadCloser = io.NopCloser(strings.NewReader("invalid " + sentinel))
					switch scenario {
					case "body":
						body = &credentialProbeBody{sentinel: sentinel}
					case "channel":
						body = io.NopCloser(strings.NewReader(
							fmt.Sprintf(`{"error":1,"errcode":40029,"errmsg":%q,"message":%q}`, sentinel, sentinel)))
					}
					return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header)}, nil
				})}
				endpoint := ""
				if scenario == "build" {
					endpoint = "://" + sentinel
				}
				var provider Provider
				if providerName == WeChatMiniGameAuthType {
					provider = &WeChatMiniGameProvider{AppId: "probe", AppSecret: sentinel, HTTP: client, Endpoint: endpoint}
				} else {
					provider = &DouyinMiniGameProvider{AppId: "probe", AppSecret: sentinel, HTTP: client, Endpoint: endpoint}
				}
				_, err := provider.Validate(context.Background(), sentinel)
				if err == nil {
					t.Fatal("故障响应应拒绝验证")
				}
				if strings.Contains(err.Error(), sentinel) {
					t.Fatal("返回的错误泄露了凭证")
				}
				logx.Errorf("Auth failed: %v", err)
				if strings.Contains(logs.String(), sentinel) {
					t.Fatal("调用方日志泄露了凭证")
				}
				if !strings.Contains(logs.String(), providerName) {
					t.Fatal("错误里缺少渠道名,排障对不上号")
				}
				if scenario == "cancel" && !errors.Is(err, context.Canceled) {
					t.Fatal("取消语义丢失")
				}
				if scenario == "deadline" && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("超时语义丢失")
				}
			})
		}
	}
}

// oauthGet 不带请求头;带头的变体只多出调用方给的那几个头,不改动调用方手里的那一份。
func TestOAuthGetWithHeaderSendsOnlyGivenHeaders(t *testing.T) {
	var gotContentType []string
	cli := serveOAuth(t, "/probe", func(w http.ResponseWriter, r *http.Request) {
		gotContentType = append(gotContentType, r.Header.Get("Content-Type"))
		_, _ = w.Write([]byte(`{}`))
	})
	header := http.Header{"Content-Type": {"application/json"}}
	if _, err := oauthGetWithHeader(context.Background(), cli, "probe", "http://probe.invalid/probe", header); err != nil {
		t.Fatalf("with header: %v", err)
	}
	if _, err := oauthGet(context.Background(), cli, "probe", "http://probe.invalid/probe"); err != nil {
		t.Fatalf("without header: %v", err)
	}
	if len(gotContentType) != 2 || gotContentType[0] != "application/json" || gotContentType[1] != "" {
		t.Fatalf("Content-Type 依次为 %q,want [application/json, 空]", gotContentType)
	}
	if len(header) != 1 || header.Get("Content-Type") != "application/json" {
		t.Fatalf("调用方的 header 被改动: %v", header)
	}
}
