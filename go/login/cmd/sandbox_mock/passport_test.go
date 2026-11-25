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
	cfg := &config.Config{}
	cfg.Mode = "dev"
	cfg.Secrets.GateToken.Value = "a3f9c2e17b6d40f8a1c5e9b2d7f4038e"
	cfg.AuthProviders = config.AuthConfig{Passport: p}
	if _, _, err := config.ResolveSecrets(cfg, func(string) (string, bool) { return "", false }); err != nil {
		t.Fatalf("linux stack Passport section failed startup gate: %v", err)
	}
}
