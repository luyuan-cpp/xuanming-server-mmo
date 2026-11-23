package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

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
