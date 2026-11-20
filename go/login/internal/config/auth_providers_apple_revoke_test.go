package config

import (
	"strings"
	"testing"
)

// 撤销 Sign in with Apple 授权的三项配置(docs/design/account-deletion.md §8.2):要么都写要么都不写;Team ID、Key ID 是 10 位
// 大写字母或数字;私钥只从环境变量读,变量缺失或为空拒启;PEM 带结尾换行照收(Secret 从文件建时就是这样);Google 段不收这三项。

func appleRevokeLookup(env map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok
	}
}

func TestAppleRevokeKeyAllOrNone(t *testing.T) {
	pem := "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"
	lookup := appleRevokeLookup(map[string]string{"APPLE_KEY": pem})

	secrets, warnings, err := resolveAuthProviders(AuthConfig{Apple: &IdTokenAuthConf{ClientId: testAppleClientId}}, false, lookup)
	if err != nil || secrets.ApplePrivateKeyPEM != "" {
		t.Fatalf("三项都不写应放行且不配撤销: %v %q", err, secrets.ApplePrivateKeyPEM)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "not revoked on account deletion") {
		t.Fatalf("没配撤销应告警: %v", warnings)
	}

	full := &IdTokenAuthConf{ClientId: testAppleClientId, TeamId: "ABCDE12345", KeyId: "FGHIJ67890", PrivateKeyEnv: "APPLE_KEY"}
	secrets, _, err = resolveAuthProviders(AuthConfig{Apple: full}, false, lookup)
	if err != nil || secrets.ApplePrivateKeyPEM != pem {
		t.Fatalf("三项都写应解析出私钥原文(带结尾换行): %v", err)
	}

	for name, c := range map[string]*IdTokenAuthConf{
		"缺 KeyId":     {ClientId: testAppleClientId, TeamId: "ABCDE12345", PrivateKeyEnv: "APPLE_KEY"},
		"缺 TeamId":    {ClientId: testAppleClientId, KeyId: "FGHIJ67890", PrivateKeyEnv: "APPLE_KEY"},
		"缺私钥变量名":      {ClientId: testAppleClientId, TeamId: "ABCDE12345", KeyId: "FGHIJ67890"},
		"TeamId 小写":   {ClientId: testAppleClientId, TeamId: "abcde12345", KeyId: "FGHIJ67890", PrivateKeyEnv: "APPLE_KEY"},
		"KeyId 长度不对":  {ClientId: testAppleClientId, TeamId: "ABCDE12345", KeyId: "FGHIJ678", PrivateKeyEnv: "APPLE_KEY"},
		"环境变量不在":      {ClientId: testAppleClientId, TeamId: "ABCDE12345", KeyId: "FGHIJ67890", PrivateKeyEnv: "MISSING"},
		"变量名是未展开的占位符": {ClientId: testAppleClientId, TeamId: "ABCDE12345", KeyId: "FGHIJ67890", PrivateKeyEnv: "${APPLE_KEY}"},
	} {
		if _, _, err := resolveAuthProviders(AuthConfig{Apple: c}, false, lookup); err == nil {
			t.Fatalf("%s: 应拒绝启动", name)
		}
	}
}

func TestGoogleSectionRejectsAppleRevokeFields(t *testing.T) {
	g := &IdTokenAuthConf{ClientId: testGoogleClientId, TeamId: "ABCDE12345"}
	if _, _, err := resolveAuthProviders(AuthConfig{Google: g}, false, appleRevokeLookup(nil)); err == nil {
		t.Fatal("Google 段写了 Apple 的撤销配置应拒绝")
	}
}
