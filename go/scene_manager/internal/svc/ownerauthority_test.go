package svc

import (
	"context"
	"strings"
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

	// enforce 在准入屏障做实之前启动即拒(数据覆写审计 r2 第 4 轮 r2-oa#1):没有人续实例租约、实例身份是常量,
	// 屏障恒为当前时刻。拒在连库之前,所以这里给的 DSN 连不连得上都一样。
	_, _, err = initOwnerAuthority(context.Background(), config.OwnerAuthorityConfig{
		Mode:    "enforce",
		DSN:     "user:pass@tcp(127.0.0.1:3306)/owner",
		Backend: "mysql",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "Mode=enforce is refused") {
		t.Fatalf("enforce mode err = %v, want refusal until the admit barrier is implemented", err)
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
