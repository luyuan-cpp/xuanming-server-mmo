package svc

import (
	"context"
	"errors"
	"testing"

	"login/internal/config"
	"login/internal/logic/pkg/auth"
)

// Passport provider 装配测试:Issuer 与 ClientId 必须透传给校验器,坏令牌在取公钥之前按凭证无效拒绝。
func TestPassportProviderFromConfigCarriesIssuerAndClientId(t *testing.T) {
	p := passportProviderFromConfig(&config.PassportAuthConf{
		Issuer:   "https://passport.example.com",
		ClientId: "xuanming-game-client",
	})
	_, err := p.Validate(context.Background(), "not-a-jwt")
	if err == nil {
		t.Fatal("passport: malformed token should be rejected")
	}
	if errors.Is(err, auth.ErrProviderUnavailable) {
		t.Fatalf("passport: Issuer/ClientId not forwarded to verifier: %v", err)
	}
}
