package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	testPassportIssuer   = "https://passport.example.com"
	testPassportClientID = "xuanming-game-client"
)

func TestPassportProviderValidatesTokenAndUsesWellKnownJWKS(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		wantPath string
	}{
		{"defaults to issuer", "", "/.well-known/jwks"},
		{"endpoint override with base path", "https://casdoor.internal:8000/auth/", "/auth/.well-known/jwks"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &jwksStub{paths: make(chan string, 16)}
			stub.publish(publicJWK("cert-built-in", testRSAKey1()))
			cli, srv := newTestClient(stub)
			t.Cleanup(srv.Close)

			p := NewPassportProvider(testPassportIssuer, testPassportClientID, tc.endpoint, cli)
			now := time.Now().Unix()
			claims := map[string]any{
				"iss":       testPassportIssuer,
				"aud":       []string{testPassportClientID},
				"sub":       "550e8400-e29b-41d4-a716-446655440000",
				"iat":       now,
				"exp":       now + int64(time.Hour/time.Second),
				"tokenType": "id-token",
			}
			res, err := p.Validate(context.Background(), signTestIdToken(t, testRSAKey1(), "cert-built-in", claims))
			if err != nil {
				t.Fatalf("Validate failed: %v", err)
			}
			if res.Account != "passport_550e8400-e29b-41d4-a716-446655440000" {
				t.Fatalf("account = %q", res.Account)
			}
			if got := <-stub.paths; got != tc.wantPath {
				t.Fatalf("JWKS path = %q, want %q", got, tc.wantPath)
			}
		})
	}
}

func TestPassportProviderRejectsBadConfigSubjectAndOversizedLifetime(t *testing.T) {
	if _, err := NewPassportProvider("", testPassportClientID, "", nil).Validate(context.Background(), "a.b.c"); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("missing issuer should fail as ErrProviderUnavailable, got %v", err)
	}
	if _, err := NewPassportProvider(testPassportIssuer, "", "", nil).Validate(context.Background(), "a.b.c"); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("missing clientID should fail as ErrProviderUnavailable, got %v", err)
	}

	stub := &jwksStub{paths: make(chan string, 16)}
	stub.publish(publicJWK("cert-built-in", testRSAKey1()))
	cli, srv := newTestClient(stub)
	t.Cleanup(srv.Close)
	p := NewPassportProvider(testPassportIssuer, testPassportClientID, "", cli)

	now := time.Now().Unix()
	invalidCases := map[string]map[string]any{
		"wrong issuer": {
			"iss": "https://other.example.com", "aud": testPassportClientID, "sub": "u1",
			"iat": now, "exp": now + 3600, "tokenType": "id-token",
		},
		"wrong audience": {
			"iss": testPassportIssuer, "aud": "other-client", "sub": "u1",
			"iat": now, "exp": now + 3600, "tokenType": "id-token",
		},
		"subject with whitespace": {
			"iss": testPassportIssuer, "aud": testPassportClientID, "sub": "bad sub",
			"iat": now, "exp": now + 3600, "tokenType": "id-token",
		},
		"subject exceeds account max bytes": {
			"iss": testPassportIssuer, "aud": testPassportClientID, "sub": strings.Repeat("a", 190),
			"iat": now, "exp": now + 3600, "tokenType": "id-token",
		},
		"exp equals iat": {
			"iss": testPassportIssuer, "aud": testPassportClientID, "sub": "u1",
			"iat": now, "exp": now, "tokenType": "id-token",
		},
		// Casdoor 一次换票给三枚同钥、同 iss / aud / sub 的 JWT,只有身份令牌能登录。访问令牌与刷新令牌任何玩家都拿得到,
		// 必须按凭证无效拒绝,不能算渠道故障;刷新令牌有效期 24h,要在「有效期超限 = 配置故障」之前就被类型挡下。
		"access token": {
			"iss": testPassportIssuer, "aud": testPassportClientID, "sub": "u1",
			"iat": now, "exp": now + 3600, "tokenType": "access-token",
		},
		"refresh token with long lifetime": {
			"iss": testPassportIssuer, "aud": testPassportClientID, "sub": "u1",
			"iat": now, "exp": now + int64(24*time.Hour/time.Second), "tokenType": "refresh-token",
		},
		"token type missing": {
			"iss": testPassportIssuer, "aud": testPassportClientID, "sub": "u1",
			"iat": now, "exp": now + 3600,
		},
	}
	for name, claims := range invalidCases {
		_, err := p.Validate(context.Background(), signTestIdToken(t, testRSAKey1(), "cert-built-in", claims))
		requireInvalidCredential(t, err, name)
	}

	// Casdoor 默认 expireInHours=168 必须被识别为渠道配置故障(ErrProviderUnavailable),不让 7 天防重放键塞满 Redis。
	oversized := map[string]any{
		"iss":       testPassportIssuer,
		"aud":       testPassportClientID,
		"sub":       "550e8400-e29b-41d4-a716-446655440000",
		"iat":       now,
		"exp":       now + int64(168*time.Hour/time.Second),
		"tokenType": "id-token",
	}
	_, err := p.Validate(context.Background(), signTestIdToken(t, testRSAKey1(), "cert-built-in", oversized))
	requireProviderUnavailable(t, err, "oversized token lifetime")
}

func TestPassportProviderConsumesEachTokenOnce(t *testing.T) {
	stub := &jwksStub{paths: make(chan string, 16)}
	stub.publish(publicJWK("cert-built-in", testRSAKey1()))
	cli, srv := newTestClient(stub)
	t.Cleanup(srv.Close)

	guard := &memoryReplayGuard{}
	p := NewPassportProvider(testPassportIssuer, testPassportClientID, "", cli).WithReplayGuard(guard)

	now := time.Now().Unix()
	claims := map[string]any{
		"iss":       testPassportIssuer,
		"aud":       testPassportClientID,
		"sub":       "550e8400-e29b-41d4-a716-446655440000",
		"iat":       now,
		"exp":       now + 600,
		"tokenType": "id-token",
	}
	tok := signTestIdToken(t, testRSAKey1(), "cert-built-in", claims)

	if _, err := p.Validate(context.Background(), tok); err != nil {
		t.Fatalf("first validate: %v", err)
	}
	_, err := p.Validate(context.Background(), tok)
	requireInvalidCredential(t, err, "replayed passport token")

	if guard.count() != 1 {
		t.Fatalf("replay guard count = %d, want 1", guard.count())
	}
	for key := range guard.seen {
		if !strings.HasPrefix(key, "login:idtoken:used:v1:passport:") {
			t.Fatalf("unexpected replay key prefix: %s", key)
		}
		if strings.Contains(key, tok) || strings.Contains(key, claims["sub"].(string)) {
			t.Fatalf("replay key leaked token or sub: %s", key)
		}
	}

	claims["sub"] = "another-uuid"
	guard.err = errors.New("redis unavailable")
	_, err = p.Validate(context.Background(), signTestIdToken(t, testRSAKey1(), "cert-built-in", claims))
	requireProviderUnavailable(t, err, "replay store unavailable")
}
