package svc

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/service"

	"login/internal/config"
)

func TestValidateDevelopmentPasswordMode(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		wantErr bool
	}{
		{name: "dev", mode: service.DevMode},
		{name: "test", mode: service.TestMode},
		{name: "prod", mode: service.ProMode, wantErr: true},
		{name: "empty defaults fail closed", mode: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateDevelopmentPasswordMode(tt.mode)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateDevelopmentPasswordMode(%q) error = %v, wantErr = %v", tt.mode, err, tt.wantErr)
			}
		})
	}
}

func TestInitAuthProvidersRejectsDevPasswordAuthInProduction(t *testing.T) {
	previous := config.AppConfig
	t.Cleanup(func() { config.AppConfig = previous })

	config.AppConfig = config.Config{}
	config.AppConfig.Mode = service.ProMode
	config.AppConfig.DevPasswordAuth = config.DevPasswordAuthConf{
		Enabled:                true,
		SharedSecretEnv:        "LOGIN_TEST_DEV_PASSWORD_SECRET",
		AllowedAccountPrefixes: []string{"robot_"},
	}
	t.Setenv("LOGIN_TEST_DEV_PASSWORD_SECRET", "must-not-be-registered")

	defer func() {
		if recovered := recover(); recovered == nil {
			t.Fatal("InitAuthProviders did not reject DevPasswordAuth in production mode")
		} else if !strings.Contains(recovered.(string), "requires go-zero Mode") {
			t.Fatalf("panic = %v, want production mode validation", recovered)
		}
	}()
	InitAuthProviders()
}

func TestInitAuthProvidersRejectsProductionAndDevelopmentPasswordTogether(t *testing.T) {
	previous := config.AppConfig
	t.Cleanup(func() { config.AppConfig = previous })

	config.AppConfig = config.Config{
		PasswordAuth:    config.PasswordAuthConf{Enabled: true},
		DevPasswordAuth: config.DevPasswordAuthConf{Enabled: true},
	}
	defer func() {
		if recovered := recover(); recovered == nil {
			t.Fatal("InitAuthProviders accepted both password providers")
		} else if !strings.Contains(fmt.Sprint(recovered), "cannot both be enabled") {
			t.Fatalf("panic = %v, want mutually-exclusive validation", recovered)
		}
	}()
	InitAuthProviders()
}

func TestDevelopmentPasswordProviderFromConfigMissingEnvFailsStartupValidation(t *testing.T) {
	conf := config.DevPasswordAuthConf{
		Enabled:                true,
		SharedSecretEnv:        "LOGIN_TEST_MISSING_SECRET",
		AllowedAccountPrefixes: []string{"robot_"},
	}

	_, err := developmentPasswordProviderFromConfig(conf, func(string) (string, bool) {
		return "", false
	})
	if err == nil || !strings.Contains(err.Error(), "missing or empty") {
		t.Fatalf("error = %v, want missing environment variable validation error", err)
	}
}

func TestDevelopmentPasswordProviderFromConfigReadsEnvValue(t *testing.T) {
	const envName = "LOGIN_TEST_DEV_PASSWORD_SECRET"
	t.Setenv(envName, "from-environment")
	conf := config.DevPasswordAuthConf{
		Enabled:                true,
		SharedSecretEnv:        envName,
		AllowedAccountPrefixes: []string{"robot_"},
	}

	provider, err := developmentPasswordProviderFromConfig(conf, os.LookupEnv)
	if err != nil {
		t.Fatalf("developmentPasswordProviderFromConfig: %v", err)
	}
	result, err := provider.ValidatePassword(context.Background(), "robot_9", "from-environment")
	if err != nil {
		t.Fatalf("ValidatePassword: %v", err)
	}
	if result.Account != "robot_9" {
		t.Fatalf("account = %q, want robot_9", result.Account)
	}
}

func TestPasswordAuthDSNFromConfigRequiresNamedEnvironmentVariable(t *testing.T) {
	if _, err := passwordAuthDSNFromConfig(config.PasswordAuthConf{}, os.LookupEnv); err == nil {
		t.Fatal("empty DSNEnv must fail startup validation")
	}
	conf := config.PasswordAuthConf{Enabled: true, DSNEnv: "LOGIN_TEST_PASSWORD_DSN_MISSING"}
	if _, err := passwordAuthDSNFromConfig(conf, func(string) (string, bool) { return "", false }); err == nil {
		t.Fatal("missing DSN environment variable must fail startup validation")
	}
}

func TestPasswordAuthDSNFromConfigReadsEnvironmentWithoutLoggingIt(t *testing.T) {
	const envName = "LOGIN_TEST_PASSWORD_DSN"
	t.Setenv(envName, "user:secret@tcp(127.0.0.1:3306)/game")
	dsn, err := passwordAuthDSNFromConfig(config.PasswordAuthConf{Enabled: true, DSNEnv: envName}, os.LookupEnv)
	if err != nil {
		t.Fatalf("passwordAuthDSNFromConfig: %v", err)
	}
	if dsn != "user:secret@tcp(127.0.0.1:3306)/game" {
		t.Fatal("DSN environment value was not returned verbatim")
	}
}
