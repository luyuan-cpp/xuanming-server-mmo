package token

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// 把 RefreshTokenTTL 调小之后账号已满:新家族的 score 反而最小。封顶不能把刚签发的家族淘汰掉,否则交给客户端的就是作废的令牌。
func TestIssue_CapNeverEvictsTheJustIssuedFamily(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	ctx := context.Background()

	long := NewManager(rdb, Config{AccessTokenTTL: time.Hour, RefreshTokenTTL: 720 * time.Hour})
	for i := 0; i < maxRefreshTokensPerAccount; i++ {
		if _, err := long.Issue(ctx, "cap-account", "password", ""); err != nil {
			t.Fatal(err)
		}
	}

	short := NewManager(rdb, Config{AccessTokenTTL: time.Hour, RefreshTokenTTL: 2 * time.Hour})
	pair, err := short.Issue(ctx, "cap-account", "password", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := short.ValidateAccess(ctx, pair.AccessToken); err != nil {
		t.Fatalf("刚签发的 access 不可用: %v", err)
	}
	next, err := short.Refresh(ctx, pair.RefreshToken)
	if err != nil {
		t.Fatalf("刚签发的 refresh 不可用: %v", err)
	}
	if _, err := short.ValidateAccess(ctx, next.AccessToken); err != nil {
		t.Fatalf("轮换出来的 access 不可用: %v", err)
	}
}
