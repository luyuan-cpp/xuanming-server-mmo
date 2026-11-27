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
	"time"

	"github.com/zeromicro/go-zero/core/conf"

	"login/internal/config"
	"login/internal/logic/pkg/auth"
)

func mintSandboxPassportToken(t *testing.T, base, user, aud, iss string) string {
	t.Helper()
	q := url.Values{"user": {user}, "aud": {aud}}
	if iss != "" {
		q.Set("iss", iss)
	}
	resp, err := http.Get(base + "/mock/passport/id_token?" + q.Encode())
	if err != nil {
		t.Fatalf("mint passport token: %v", err)
	}
	defer resp.Body.Close()
	var body struct {
		IdToken string `json:"id_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || resp.StatusCode != http.StatusOK || body.IdToken == "" {
		t.Fatalf("mint passport token: status=%d err=%v", resp.StatusCode, err)
	}
	return body.IdToken
}

func TestPassportProviderAgainstSandboxMock(t *testing.T) {
	base := newSandbox(t)
	issuer := "https://passport.sandbox.local"
	clientID := "xuanming-game-client"
	p := auth.NewPassportProvider(issuer, clientID, base, nil)
	ctx := context.Background()

	first, err := p.Validate(ctx, mintSandboxPassportToken(t, base, "alice", clientID, issuer))
	if err != nil {
		t.Fatalf("valid passport token rejected: %v", err)
	}
	if !strings.HasPrefix(first.Account, "passport_") || len(first.Account) == len("passport_") {
		t.Fatalf("unexpected account: %q", first.Account)
	}

	again, err := p.Validate(ctx, mintSandboxPassportToken(t, base, "alice", clientID, issuer))
	if err != nil || again.Account != first.Account {
		t.Fatalf("same seed should map to same account: first=%q again=%+v err=%v", first.Account, again, err)
	}

	bob, err := p.Validate(ctx, mintSandboxPassportToken(t, base, "bob", clientID, issuer))
	if err != nil || bob.Account == first.Account {
		t.Fatalf("different seed should map to different account: alice=%q bob=%+v err=%v", first.Account, bob, err)
	}

	for name, tok := range map[string]string{
		"expired":     mintSandboxPassportToken(t, base, "EXPIRED", clientID, issuer),
		"wrong aud":   mintSandboxPassportToken(t, base, "alice", "other-client", issuer),
		"wrong iss":   mintSandboxPassportToken(t, base, "alice", clientID, "https://evil.example.com"),
		"apple token": mintSandboxIdToken(t, base, "apple", "alice", clientID),
		// 同一次换票拿到的访问令牌与刷新令牌:同钥、同 iss / aud,只有 tokenType 不同,login 只收身份令牌。
		// 刷新令牌有效期 24h,也必须按凭证无效拒绝,不能落进「有效期超限 = 配置故障」。
		"access token":  mintSandboxPassportToken(t, base, "ACCESS_TOKEN", clientID, issuer),
		"refresh token": mintSandboxPassportToken(t, base, "REFRESH_TOKEN", clientID, issuer),
	} {
		_, err := p.Validate(ctx, tok)
		if err == nil || errors.Is(err, auth.ErrProviderUnavailable) {
			t.Fatalf("%s: expected invalid credential rejection, got %v", name, err)
		}
	}

	// OVERSIZED_TTL(168h)必须被识为配置故障 ErrProviderUnavailable。
	_, err = p.Validate(ctx, mintSandboxPassportToken(t, base, "OVERSIZED_TTL", clientID, issuer))
	if !errors.Is(err, auth.ErrProviderUnavailable) {
		t.Fatalf("OVERSIZED_TTL should return ErrProviderUnavailable, got %v", err)
	}
}

func deletedSandboxPassportUsers(t *testing.T, base string) []string {
	t.Helper()
	resp, err := http.Get(base + "/mock/passport/deleted_users")
	if err != nil {
		t.Fatalf("list deleted passport users: %v", err)
	}
	defer resp.Body.Close()
	var body struct {
		Deleted []string `json:"deleted"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("list deleted passport users: %v", err)
	}
	return body.Deleted
}

// 账号注销删通行证用户:login 的 PassportAdmin 对着 mock 的管理接口走一遍,删掉、重做、凭据不对三种情况。
func TestPassportAdminDeleteUserAgainstSandboxMock(t *testing.T) {
	base := newSandbox(t)
	issuer := "https://passport.sandbox.local"
	ctx := context.Background()
	const sub = "passport-sandbox-0123456789ab"

	deleter, err := auth.NewPassportAdmin(issuer, base, sandboxPassportAdminClientID, sandboxPassportAdminClientSecret, nil)
	if err != nil {
		t.Fatalf("NewPassportAdmin: %v", err)
	}
	if err := deleter.DeleteUser(ctx, sub); err != nil {
		t.Fatalf("delete passport user: %v", err)
	}
	if err := deleter.DeleteUser(ctx, sub); err != nil {
		t.Fatalf("deleting a user that is already gone must succeed (the executor retries): %v", err)
	}
	if got := deletedSandboxPassportUsers(t, base); len(got) != 1 || got[0] != sub {
		t.Fatalf("deleted users = %v, want [%s]", got, sub)
	}

	wrongSecret, err := auth.NewPassportAdmin(issuer, base, sandboxPassportAdminClientID, "not-the-secret", nil)
	if err != nil {
		t.Fatalf("NewPassportAdmin: %v", err)
	}
	if err := wrongSecret.DeleteUser(ctx, "passport-sandbox-ba9876543210"); !errors.Is(err, auth.ErrProviderUnavailable) {
		t.Fatalf("wrong admin credentials must fail as ErrProviderUnavailable, got %v", err)
	}
	if got := deletedSandboxPassportUsers(t, base); len(got) != 1 {
		t.Fatalf("a refused request must not delete anyone: %v", got)
	}
}

// 刷新前的核对(official-passport-registration.md §3.6):mock 的 change_password / forbid_user 之后,
// login 的 PassportAdmin 按同一个种子的 sub 判出不再续期;之前照常续期。
func TestPassportAdminCheckSessionAgainstSandboxMock(t *testing.T) {
	base := newSandbox(t)
	admin, err := auth.NewPassportAdmin("https://passport.sandbox.local", base, sandboxPassportAdminClientID, sandboxPassportAdminClientSecret, nil)
	if err != nil {
		t.Fatalf("NewPassportAdmin: %v", err)
	}
	ctx := context.Background()
	loginAt := time.Now().Add(-time.Minute).Unix()
	for seed, want := range map[string]struct{ event, reason string }{
		"alice": {"change_password", auth.PassportSessionPasswordChanged},
		"bob":   {"forbid_user", auth.PassportSessionUserForbidden},
	} {
		sub := passportSandboxSub(seed)
		if reason, err := admin.CheckSession(ctx, sub, loginAt); err != nil || reason != "" {
			t.Fatalf("%s before %s: CheckSession = (%q, %v), want valid", seed, want.event, reason, err)
		}
		resp, err := http.Post(base+"/mock/passport/"+want.event+"?user="+seed, "", nil)
		if err != nil {
			t.Fatalf("%s %s: %v", seed, want.event, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s: status %d", seed, want.event, resp.StatusCode)
		}
		if reason, err := admin.CheckSession(ctx, sub, loginAt); err != nil || reason != want.reason {
			t.Fatalf("%s after %s: CheckSession = (%q, %v), want %q", seed, want.event, reason, err, want.reason)
		}
	}
}

// linux 登录栈把 Passport 段也指向本 mock,Issuer 与 Endpoint 一致且过得了 login 的启动门禁。
func TestLinuxLoginStackPassportSectionPointsAtSandbox(t *testing.T) {
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
	p := probe.AuthProviders.Passport
	if p == nil || probe.AuthProviders.WeChat == nil {
		t.Fatal("linux login stack config must configure AuthProviders.Passport alongside sandbox WeChat")
	}
	if p.Endpoint != probe.AuthProviders.WeChat.Endpoint {
		t.Fatalf("Passport.Endpoint = %q, want sandbox mock %q", p.Endpoint, probe.AuthProviders.WeChat.Endpoint)
	}
	// 管理凭据必须是 mock 认的那一组,否则沙箱里注销时删通行证用户每次都被拒。
	if p.AdminClientId != sandboxPassportAdminClientID || p.AdminClientSecret != sandboxPassportAdminClientSecret {
		t.Fatalf("Passport.AdminClientId / AdminClientSecret must match the sandbox mock's admin client")
	}
	cfg := &config.Config{}
	cfg.Mode = "dev"
	cfg.Secrets.GateToken.Value = "a3f9c2e17b6d40f8a1c5e9b2d7f4038e"
	cfg.AuthProviders = config.AuthConfig{Passport: p}
	if _, _, err := config.ResolveSecrets(cfg, func(string) (string, bool) { return "", false }); err != nil {
		t.Fatalf("linux stack Passport section failed startup gate: %v", err)
	}
}
