package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"

	"login/internal/config"
	"login/internal/logic/pkg/auth"
)

// Apple / Google 登录的沙箱端到端:真 provider 去 mock 取公钥,验 mock 签发的令牌。
// mock 的令牌格式、公钥路径、签发方任何一边和 login 对不上,这里就会红。

func mintSandboxIdToken(t *testing.T, base, channel, user, aud string) string {
	t.Helper()
	q := url.Values{"user": {user}, "aud": {aud}}
	resp, err := http.Get(base + "/mock/" + channel + "/id_token?" + q.Encode())
	if err != nil {
		t.Fatalf("mint %s token: %v", channel, err)
	}
	defer resp.Body.Close()
	var body struct {
		IdToken string `json:"id_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || resp.StatusCode != http.StatusOK || body.IdToken == "" {
		t.Fatalf("mint %s token: status=%d err=%v", channel, resp.StatusCode, err)
	}
	return body.IdToken
}

func TestIdTokenProvidersAgainstSandboxMock(t *testing.T) {
	base := newSandbox(t)
	cases := []struct {
		channel  string
		clientID string
		provider *auth.IdTokenProvider
		prefix   string
	}{
		{"apple", "com.example.sandbox", auth.NewAppleProvider("com.example.sandbox", base, nil), "apple_"},
		{"google", "sandbox-client.apps.googleusercontent.com",
			auth.NewGoogleProvider("sandbox-client.apps.googleusercontent.com", base, nil), "google_"},
	}
	ctx := context.Background()
	accounts := map[string]string{}
	for _, tc := range cases {
		t.Run(tc.channel, func(t *testing.T) {
			first, err := tc.provider.Validate(ctx, mintSandboxIdToken(t, base, tc.channel, "alice", tc.clientID))
			if err != nil {
				t.Fatalf("mock 签发的令牌应验过: %v", err)
			}
			if !strings.HasPrefix(first.Account, tc.prefix) || len(first.Account) == len(tc.prefix) {
				t.Fatalf("account = %q, want prefix %q", first.Account, tc.prefix)
			}
			again, err := tc.provider.Validate(ctx, mintSandboxIdToken(t, base, tc.channel, "alice", tc.clientID))
			if err != nil || again.Account != first.Account {
				t.Fatalf("同一个种子应是同一个账号: first=%q again=%+v err=%v", first.Account, again, err)
			}
			bob, err := tc.provider.Validate(ctx, mintSandboxIdToken(t, base, tc.channel, "bob", tc.clientID))
			if err != nil || bob.Account == first.Account {
				t.Fatalf("不同种子应是不同账号: alice=%q bob=%+v err=%v", first.Account, bob, err)
			}
			accounts[tc.channel] = first.Account

			for what, token := range map[string]string{
				"过期令牌":      mintSandboxIdToken(t, base, tc.channel, "EXPIRED", tc.clientID),
				"签给别的应用的令牌": mintSandboxIdToken(t, base, tc.channel, "alice", "com.other.app"),
			} {
				_, err := tc.provider.Validate(ctx, token)
				if err == nil {
					t.Fatalf("%s 应被拒绝", what)
				}
				if errors.Is(err, auth.ErrProviderUnavailable) {
					t.Fatalf("%s 是凭证无效,不应标 ErrProviderUnavailable: %v", what, err)
				}
			}
		})
	}
	if a, g := accounts["apple"], accounts["google"]; a != "" && g != "" &&
		strings.TrimPrefix(a, "apple_") == strings.TrimPrefix(g, "google_") {
		t.Fatal("同一个种子在 Apple 与 Google 应是两个不同的 sub,与真实渠道一致")
	}
}

// 另一家的令牌拿来登这一家:签发方不符,拒绝。
func TestSandboxIdTokenIssuerIsPerChannel(t *testing.T) {
	base := newSandbox(t)
	apple := auth.NewAppleProvider("com.example.sandbox", base, nil)
	_, err := apple.Validate(context.Background(), mintSandboxIdToken(t, base, "google", "alice", "com.example.sandbox"))
	if err == nil || errors.Is(err, auth.ErrProviderUnavailable) {
		t.Fatalf("Google 签发方的令牌登 Apple 应按凭证无效拒绝: %v", err)
	}
}

func TestSandboxIdTokenMintRequiresUserAndAudience(t *testing.T) {
	base := newSandbox(t)
	for _, query := range []string{"", "?user=alice", "?aud=com.example.sandbox"} {
		resp, err := http.Get(base + "/mock/apple/id_token" + query)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("query %q: status = %d, want 400", query, resp.StatusCode)
		}
	}
}

// linux 登录栈把 Apple / Google 两段也指向本 mock,ClientId 过得了 login 的启动门禁(Google 要 OAuth 客户端 ID 的后缀)。
func TestLinuxLoginStackIdTokenSectionsPointAtSandbox(t *testing.T) {
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
	if providers.Apple == nil || providers.Google == nil || providers.WeChat == nil {
		t.Fatal("linux login stack config no longer configures Apple / Google; update this test together with the stack")
	}
	for name, section := range map[string]*config.IdTokenAuthConf{"Apple": providers.Apple, "Google": providers.Google} {
		if section.Endpoint != providers.WeChat.Endpoint {
			t.Errorf("%s.Endpoint = %q, want the sandbox mock %q", name, section.Endpoint, providers.WeChat.Endpoint)
		}
	}
	// 只放 Apple / Google 两段,单独过一遍启动门禁(本栈 Mode=dev,宽松档)。
	cfg := &config.Config{}
	cfg.Mode = "dev"
	cfg.Secrets.GateToken.Value = "a3f9c2e17b6d40f8a1c5e9b2d7f4038e"
	cfg.AuthProviders = config.AuthConfig{Apple: providers.Apple, Google: providers.Google}
	if _, _, err := config.ResolveSecrets(cfg, func(string) (string, bool) { return "", false }); err != nil {
		t.Fatalf("linux 栈的 Apple / Google 段过不了 login 的启动门禁: %v", err)
	}
}
