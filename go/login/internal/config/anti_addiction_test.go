package config

import (
	"strings"
	"testing"
)

const testNppaSecret = "2836e95fcd10e04b0069bb1ee659955b"

func validAntiAddiction() AntiAddictionConf {
	return AntiAddictionConf{
		Enabled:                  true,
		PlatformManagedAuthTypes: []string{"wechat_minigame", "douyin_minigame"},
		ExtraPlayDays:            []string{"2026-10-01"},
		DailySubmitLimit:         5,
		Nppa:                     NppaConf{AppId: "app", BizId: "biz", SecretKeyEnv: "LOGIN_NPPA_SECRET_KEY"},
	}
}

func nppaEnv(value string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		if name == "LOGIN_NPPA_SECRET_KEY" {
			return value, true
		}
		return "", false
	}
}

func TestResolveAntiAddictionDisabledChecksNothing(t *testing.T) {
	secret, err := resolveAntiAddiction(AntiAddictionConf{}, nppaEnv(""))
	if err != nil || secret != "" {
		t.Fatalf("disabled = %q, %v; want empty and no error", secret, err)
	}
}

func TestResolveAntiAddictionReturnsTheSecret(t *testing.T) {
	secret, err := resolveAntiAddiction(validAntiAddiction(), nppaEnv(testNppaSecret))
	if err != nil || secret != testNppaSecret {
		t.Fatalf("valid = %q, %v", secret, err)
	}
}

func TestResolveAntiAddictionRejectsIncompleteConfig(t *testing.T) {
	cases := map[string]struct {
		mutate func(*AntiAddictionConf)
		secret string
	}{
		"no app id":        {func(c *AntiAddictionConf) { c.Nppa.AppId = "" }, testNppaSecret},
		"no biz id":        {func(c *AntiAddictionConf) { c.Nppa.BizId = " " }, testNppaSecret},
		"no secret env":    {func(c *AntiAddictionConf) { c.Nppa.SecretKeyEnv = "" }, testNppaSecret},
		"secret missing":   {func(*AntiAddictionConf) {}, ""},
		"secret not hex":   {func(*AntiAddictionConf) {}, strings.Repeat("z", 32)},
		"secret too short": {func(*AntiAddictionConf) {}, testNppaSecret[:16]},
		"bad url":          {func(c *AntiAddictionConf) { c.Nppa.CheckUrl = "ftp://example" }, testNppaSecret},
		"bad play day":     {func(c *AntiAddictionConf) { c.ExtraPlayDays = []string{"2026-1-1"} }, testNppaSecret},
		"no submit limit":  {func(c *AntiAddictionConf) { c.DailySubmitLimit = 0 }, testNppaSecret},
	}
	for name, c := range cases {
		cfg := validAntiAddiction()
		c.mutate(&cfg)
		_, err := resolveAntiAddiction(cfg, nppaEnv(c.secret))
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if c.secret != "" && strings.Contains(err.Error(), c.secret) {
			t.Errorf("%s: error leaks the secret value: %v", name, err)
		}
	}
}
