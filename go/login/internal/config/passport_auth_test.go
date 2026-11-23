package config

import (
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/service"
)

const (
	testPassportIssuer   = "https://passport.example.com"
	testPassportClientId = "xuanming-game-client"
)

func TestPassportAuthYamlSection(t *testing.T) {
	yaml := "AuthProviders:\n" +
		"  Passport:\n" +
		"    Issuer: " + testPassportIssuer + "\n" +
		"    ClientId: " + testPassportClientId + "\n" +
		"    Endpoint: https://casdoor.internal:8000\n"
	var p authProvidersProbe
	if err := conf.LoadFromYamlBytes([]byte(yaml), &p); err != nil {
		t.Fatalf("load: %v", err)
	}
	if p.AuthProviders.Passport == nil ||
		p.AuthProviders.Passport.Issuer != testPassportIssuer ||
		p.AuthProviders.Passport.ClientId != testPassportClientId ||
		p.AuthProviders.Passport.Endpoint != "https://casdoor.internal:8000" {
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
	}
	for name, mutate := range cases {
		for _, relaxed := range []bool{false, true} {
			c := &PassportAuthConf{Issuer: testPassportIssuer, ClientId: testPassportClientId}
			mutate(c)
			_, _, err := resolveAuthProviders(AuthConfig{Passport: c}, relaxed, envLookup(nil))
			if err == nil {
				t.Errorf("%s (relaxed=%v): expected rejection", name, relaxed)
				continue
			}
			if !strings.Contains(err.Error(), "Passport") {
				t.Errorf("%s: error should name Passport section: %v", name, err)
			}
			if strings.Contains(err.Error(), "user:pass") {
				t.Errorf("%s: error leaked credentials from URL", name)
			}
		}
	}
}

func TestResolveAuthProvidersPassportSchemePolicy(t *testing.T) {
	httpIssuer := AuthConfig{Passport: &PassportAuthConf{Issuer: "http://127.0.0.1:8000", ClientId: testPassportClientId}}
	if _, _, err := resolveAuthProviders(httpIssuer, false, envLookup(nil)); err == nil {
		t.Error("production mode must reject http:// Issuer")
	}
	if _, _, err := resolveAuthProviders(httpIssuer, true, envLookup(nil)); err != nil {
		t.Errorf("dev/test mode should allow http:// Issuer: %v", err)
	}

	httpEndpoint := AuthConfig{Passport: &PassportAuthConf{
		Issuer:   testPassportIssuer,
		ClientId: testPassportClientId,
		Endpoint: "http://casdoor:8000",
	}}
	if _, _, err := resolveAuthProviders(httpEndpoint, false, envLookup(nil)); err == nil {
		t.Error("production mode must reject http:// Endpoint")
	}
	if _, _, err := resolveAuthProviders(httpEndpoint, true, envLookup(nil)); err != nil {
		t.Errorf("dev/test mode should allow http:// Endpoint: %v", err)
	}
}

func TestResolveSecretsAcceptsPassportInProductionWithoutEnvSecrets(t *testing.T) {
	cfg := &Config{}
	cfg.Mode = service.ProMode
	cfg.Secrets.GateToken.Value = "a3f9c2e17b6d40f8a1c5e9b2d7f4038e"
	cfg.AuthProviders.Passport = &PassportAuthConf{
		Issuer:   testPassportIssuer,
		ClientId: testPassportClientId,
		Endpoint: "https://casdoor.internal:8000",
	}
	got, warnings, err := ResolveSecrets(cfg, envLookup(nil))
	if err != nil {
		t.Fatalf("ResolveSecrets rejected valid production Passport config: %v", err)
	}
	if got.AuthProviders != (AuthProviderSecrets{}) {
		t.Fatalf("Passport should not resolve any secrets: %+v", got.AuthProviders)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
}
