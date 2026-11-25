package svc

import (
	"context"
	"testing"

	"scene_manager/internal/config"
)

func TestInitOwnerAuthority_ConfigValidation(t *testing.T) {
	// Disabled config returns nil without error.
	store, db, err := initOwnerAuthority(context.Background(), config.OwnerAuthorityConfig{}, nil)
	if err != nil || store != nil || db != nil {
		t.Fatalf("disabled config = (%v, %v, %v), want all nil", store, db, err)
	}

	// Invalid backend rejected fail-fast.
	_, _, err = initOwnerAuthority(context.Background(), config.OwnerAuthorityConfig{
		DSN:     "user:pass@tcp(127.0.0.1:3306)/owner",
		Backend: "postgres",
	}, nil)
	if err == nil {
		t.Fatal("expected invalid backend error")
	}

	// Simultaneous DSN and DSNEnv rejected by single-source secretenv guard.
	_, _, err = initOwnerAuthority(context.Background(), config.OwnerAuthorityConfig{
		DSN:     "user:pass@tcp(127.0.0.1:3306)/owner",
		DSNEnv:  "OWNER_DSN",
		Backend: "mysql",
	}, func(string) (string, bool) { return "env_dsn", true })
	if err == nil {
		t.Fatal("expected dual DSN+DSNEnv conflict error")
	}

	// Invalid mode rejected fail-fast.
	_, _, err = initOwnerAuthority(context.Background(), config.OwnerAuthorityConfig{
		Mode:    "bogus",
		DSN:     "user:pass@tcp(127.0.0.1:3306)/owner",
		Backend: "mysql",
	}, nil)
	if err == nil {
		t.Fatal("expected invalid mode error")
	}

	// Missing DSNEnv value rejected.
	_, _, err = initOwnerAuthority(context.Background(), config.OwnerAuthorityConfig{
		DSNEnv:  "OWNER_DSN",
		Backend: "mysql",
	}, func(string) (string, bool) { return "", false })
	if err == nil {
		t.Fatal("expected missing env error")
	}
}
