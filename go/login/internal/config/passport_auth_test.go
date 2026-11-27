package config

import (
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/service"
)

const (
	testPassportIssuer         = "https://passport.example.com"
	testPassportClientId       = "xuanming-game-client"
	testPassportAdminClientId  = "xuanming-game-server"
	testPassportAdminSecretEnv = "MMORPG_LOGIN_PASSPORT_ADMIN_CLIENT_SECRET"
	testPassportAdminSecret    = "5d1c0e7a9b3f4a628c7d2e1f0a9b8c7d"
)

// passportConf 是一份生产模式也过得了门禁的官方通行证段(管理凭据走环境变量)。
func passportConf() *PassportAuthConf {
	return &PassportAuthConf{
		Issuer:               testPassportIssuer,
		ClientId:             testPassportClientId,
		AdminClientId:        testPassportAdminClientId,
		AdminClientSecretEnv: testPassportAdminSecretEnv,
	}
}

// passportEnv 是带着管理凭据密钥的环境。
func passportEnv() func(string) (string, bool) {
	return envLookup(map[string]string{testPassportAdminSecretEnv: testPassportAdminSecret})
}

func TestPassportAuthYamlSection(t *testing.T) {
	yaml := "AuthProviders:\n" +
		"  Passport:\n" +
		"    Issuer: " + testPassportIssuer + "\n" +
		"    ClientId: " + testPassportClientId + "\n" +
		"    Endpoint: https://casdoor.internal:8000\n" +
		"    AdminClientId: " + testPassportAdminClientId + "\n" +
		"    AdminClientSecretEnv: " + testPassportAdminSecretEnv + "\n"
	var p authProvidersProbe
	if err := conf.LoadFromYamlBytes([]byte(yaml), &p); err != nil {
		t.Fatalf("load: %v", err)
	}
	want := passportConf()
	want.Endpoint = "https://casdoor.internal:8000"
	if p.AuthProviders.Passport == nil || *p.AuthProviders.Passport != *want {
		t.Fatalf("Passport section parsed unexpectedly: %+v", p.AuthProviders.Passport)
	}

	var none authProvidersProbe
	if err := conf.LoadFromYamlBytes([]byte("AuthProviders:\n  QQ:\n    AppId: \"101234567\"\n"), &none); err != nil {
		t.Fatalf("load: %v", err)
	}
	if none.AuthProviders.Passport != nil {
		t.Fatalf("Passport should be nil when omitted, got %+v", none.AuthProviders.Passport)
	}

	for name, badYaml := range map[string]string{
		"missing issuer":    "AuthProviders:\n  Passport:\n    ClientId: " + testPassportClientId + "\n",
		"missing client id": "AuthProviders:\n  Passport:\n    Issuer: " + testPassportIssuer + "\n",
	} {
		var probe authProvidersProbe
		if err := conf.LoadFromYamlBytes([]byte(badYaml), &probe); err == nil {
			t.Errorf("%s: expected YAML load failure", name)
		}
	}
}

func TestResolveAuthProvidersPassportRejectsBadSections(t *testing.T) {
	cases := map[string]func(*PassportAuthConf){
		"empty issuer":          func(c *PassportAuthConf) { c.Issuer = "" },
		"blank issuer":          func(c *PassportAuthConf) { c.Issuer = "   " },
		"issuer whitespace":     func(c *PassportAuthConf) { c.Issuer = c.Issuer + "\n" },
		"issuer placeholder":    func(c *PassportAuthConf) { c.Issuer = "https://${PASSPORT_HOST}" },
		"issuer trailing slash": func(c *PassportAuthConf) { c.Issuer = testPassportIssuer + "/" },
		"issuer userinfo":       func(c *PassportAuthConf) { c.Issuer = "https://user:pass@passport.example.com" },
		"issuer query":          func(c *PassportAuthConf) { c.Issuer = testPassportIssuer + "?a=1" },
		"issuer fragment":       func(c *PassportAuthConf) { c.Issuer = testPassportIssuer + "#frag" },
		"issuer non-http":       func(c *PassportAuthConf) { c.Issuer = "ftp://passport.example.com" },
		"empty client id":       func(c *PassportAuthConf) { c.ClientId = "" },
		"blank client id":       func(c *PassportAuthConf) { c.ClientId = "   " },
		"client id whitespace":  func(c *PassportAuthConf) { c.ClientId = c.ClientId + " " },
		"client id placeholder": func(c *PassportAuthConf) { c.ClientId = "${CLIENT_ID}" },
		"endpoint placeholder":  func(c *PassportAuthConf) { c.Endpoint = "https://${CASDOOR_HOST}" },
		"endpoint userinfo":     func(c *PassportAuthConf) { c.Endpoint = "https://user:pass@casdoor.internal:8000" },
		"endpoint query":        func(c *PassportAuthConf) { c.Endpoint = "https://casdoor.internal:8000/?x=1" },
		// 管理凭据:应用号与密钥缺一不可,且不能拿客户端的公开应用充当。
		"admin client id without secret":  func(c *PassportAuthConf) { c.AdminClientSecretEnv = "" },
		"admin secret without client id":  func(c *PassportAuthConf) { c.AdminClientId = "" },
		"admin client id whitespace":      func(c *PassportAuthConf) { c.AdminClientId = c.AdminClientId + " " },
		"admin client id placeholder":     func(c *PassportAuthConf) { c.AdminClientId = "${ADMIN_CLIENT_ID}" },
		"admin client is the game client": func(c *PassportAuthConf) { c.AdminClientId = c.ClientId },
		"admin secret env not in the env": func(c *PassportAuthConf) { c.AdminClientSecretEnv = "MMORPG_LOGIN_PASSPORT_NOT_SET" },
		"admin secret env is a ${...}":    func(c *PassportAuthConf) { c.AdminClientSecretEnv = "${ADMIN_SECRET}" },
		"admin secret inline placeholder": func(c *PassportAuthConf) { c.AdminClientSecretEnv = ""; c.AdminClientSecret = "${ADMIN_SECRET}" },
	}
	for name, mutate := range cases {
		for _, relaxed := range []bool{false, true} {
			c := passportConf()
			mutate(c)
			_, _, err := resolveAuthProviders(AuthConfig{Passport: c}, relaxed, passportEnv())
			if err == nil {
				t.Errorf("%s (relaxed=%v): expected rejection", name, relaxed)
				continue
			}
			if !strings.Contains(err.Error(), "Passport") {
				t.Errorf("%s: error should name Passport section: %v", name, err)
			}
			if strings.Contains(err.Error(), "user:pass") || strings.Contains(err.Error(), testPassportAdminSecret) {
				t.Errorf("%s: error leaked credentials", name)
			}
		}
	}
}

func TestResolveAuthProvidersPassportSchemePolicy(t *testing.T) {
	httpIssuer := passportConf()
	httpIssuer.Issuer = "http://127.0.0.1:8000"
	if _, _, err := resolveAuthProviders(AuthConfig{Passport: httpIssuer}, false, passportEnv()); err == nil {
		t.Error("production mode must reject http:// Issuer")
	}
	if _, _, err := resolveAuthProviders(AuthConfig{Passport: httpIssuer}, true, passportEnv()); err != nil {
		t.Errorf("dev/test mode should allow http:// Issuer: %v", err)
	}

	httpEndpoint := passportConf()
	httpEndpoint.Endpoint = "http://casdoor:8000"
	if _, _, err := resolveAuthProviders(AuthConfig{Passport: httpEndpoint}, false, passportEnv()); err == nil {
		t.Error("production mode must reject http:// Endpoint")
	}
	if _, _, err := resolveAuthProviders(AuthConfig{Passport: httpEndpoint}, true, passportEnv()); err != nil {
		t.Errorf("dev/test mode should allow http:// Endpoint: %v", err)
	}
}

// 管理凭据(注销时删通行证用户)的门禁:生产必须配且只认环境变量;dev/test 可以不配,但要告警;内联明文只在 dev/test 放行。
func TestResolveAuthProvidersPassportAdminClientPolicy(t *testing.T) {
	secrets, warnings, err := resolveAuthProviders(AuthConfig{Passport: passportConf()}, false, passportEnv())
	if err != nil || secrets.PassportAdminClientSecret != testPassportAdminSecret || len(warnings) != 0 {
		t.Fatalf("production with env secret: secret resolved=%v warnings=%v err=%v",
			secrets.PassportAdminClientSecret == testPassportAdminSecret, warnings, err)
	}

	loginOnly := &PassportAuthConf{Issuer: testPassportIssuer, ClientId: testPassportClientId}
	if _, _, err := resolveAuthProviders(AuthConfig{Passport: loginOnly}, false, passportEnv()); err == nil ||
		!strings.Contains(err.Error(), "AdminClientSecretEnv") {
		t.Fatalf("production without the admin client must refuse to start and name the missing fields, got %v", err)
	}
	secrets, warnings, err = resolveAuthProviders(AuthConfig{Passport: loginOnly}, true, passportEnv())
	if err != nil || secrets.PassportAdminClientSecret != "" || len(warnings) != 1 || !strings.Contains(warnings[0], "AdminClientId") {
		t.Fatalf("dev/test without the admin client should start with one warning: warnings=%v err=%v", warnings, err)
	}

	inline := passportConf()
	inline.AdminClientSecretEnv = ""
	inline.AdminClientSecret = "sandbox_secret_keep_it_local"
	if _, _, err := resolveAuthProviders(AuthConfig{Passport: inline}, false, passportEnv()); err == nil {
		t.Error("production mode must reject an inline AdminClientSecret")
	}
	secrets, _, err = resolveAuthProviders(AuthConfig{Passport: inline}, true, passportEnv())
	if err != nil || secrets.PassportAdminClientSecret != inline.AdminClientSecret {
		t.Errorf("dev/test mode should accept an inline AdminClientSecret: %v", err)
	}
}

func TestResolveSecretsAcceptsPassportInProduction(t *testing.T) {
	cfg := &Config{}
	cfg.Mode = service.ProMode
	cfg.Secrets.GateToken.Value = "a3f9c2e17b6d40f8a1c5e9b2d7f4038e"
	cfg.AuthProviders.Passport = passportConf()
	cfg.AuthProviders.Passport.Endpoint = "https://casdoor.internal:8000"
	got, warnings, err := ResolveSecrets(cfg, passportEnv())
	if err != nil {
		t.Fatalf("ResolveSecrets rejected valid production Passport config: %v", err)
	}
	if got.AuthProviders != (AuthProviderSecrets{PassportAdminClientSecret: testPassportAdminSecret}) {
		t.Fatal("Passport should resolve the admin client secret and nothing else")
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
}
