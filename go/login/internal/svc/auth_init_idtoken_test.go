package svc

import (
	"context"
	"errors"
	"testing"

	"login/internal/config"
	"login/internal/logic/pkg/auth"
)

// Apple / Google provider 的装配:ClientId 必须透传给校验器。没透传时校验器的 aud 为空,
// 每个请求都回「渠道未配置」(ErrProviderUnavailable);透传了,坏令牌在取公钥之前就按凭证无效拒绝,不走网络。
func TestIdTokenProvidersFromConfigCarryClientId(t *testing.T) {
	providers := map[string]*auth.IdTokenProvider{
		auth.AppleAuthType:  appleProviderFromConfig(&config.IdTokenAuthConf{ClientId: "com.example.mmorpg"}),
		auth.GoogleAuthType: googleProviderFromConfig(&config.IdTokenAuthConf{ClientId: "1-abc.apps.googleusercontent.com"}),
	}
	for name, p := range providers {
		_, err := p.Validate(context.Background(), "not-a-jwt")
		if err == nil {
			t.Fatalf("%s: 坏令牌应被拒绝", name)
		}
		if errors.Is(err, auth.ErrProviderUnavailable) {
			t.Fatalf("%s: ClientId 没有透传给校验器: %v", name, err)
		}
	}
}
