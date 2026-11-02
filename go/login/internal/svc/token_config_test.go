package svc

import (
	"testing"
	"time"

	"login/internal/config"
)

// TokenConfig → token.Config 的映射:开关取反、校验接线、窗口 0 加吊销的启动提示。
func TestTokenConfigFrom(t *testing.T) {
	base := config.TokenConf{
		AccessTokenTTL:     2 * time.Hour,
		RefreshTokenTTL:    720 * time.Hour,
		RefreshReuseGrace:  30 * time.Second,
		RefreshReuseRevoke: true,
	}

	cfg, err := tokenConfigFrom(base)
	if err != nil {
		t.Fatalf("defaults rejected: %v", err)
	}
	if cfg.ReuseObserveOnly {
		t.Fatal("RefreshReuseRevoke=true must map to ReuseObserveOnly=false (revoke on reuse)")
	}
	if cfg.AccessTokenTTL != base.AccessTokenTTL || cfg.RefreshTokenTTL != base.RefreshTokenTTL ||
		cfg.RefreshReuseGrace != base.RefreshReuseGrace {
		t.Fatalf("TTL / grace not carried over: %+v", cfg)
	}
	if zeroGraceRevokes(cfg) {
		t.Fatal("a 30s grace must not trigger the zero-grace warning")
	}

	observe := base
	observe.RefreshReuseRevoke = false
	if cfg, err := tokenConfigFrom(observe); err != nil || !cfg.ReuseObserveOnly {
		t.Fatalf("RefreshReuseRevoke=false must map to observe-only, got %+v / %v", cfg, err)
	}

	zero := base
	zero.RefreshReuseGrace = 0
	if cfg, err := tokenConfigFrom(zero); err != nil || !zeroGraceRevokes(cfg) {
		t.Fatalf("grace 0 with revoke must be accepted and flagged, got %+v / %v", cfg, err)
	}

	for name, bad := range map[string]config.TokenConf{
		"sub-millisecond grace":        {AccessTokenTTL: time.Hour, RefreshTokenTTL: time.Hour, RefreshReuseGrace: 500 * time.Microsecond},
		"grace parsed as nanoseconds":  {AccessTokenTTL: time.Hour, RefreshTokenTTL: time.Hour, RefreshReuseGrace: 30},
		"grace above the upper bound":  {AccessTokenTTL: time.Hour, RefreshTokenTTL: time.Hour, RefreshReuseGrace: 61 * time.Second},
		"zero ttl (block left zeroed)": {},
		"refresh shorter than access":  {AccessTokenTTL: 2 * time.Hour, RefreshTokenTTL: time.Hour},
	} {
		if _, err := tokenConfigFrom(bad); err == nil {
			t.Errorf("%s: accepted, want a startup error", name)
		}
	}
}
