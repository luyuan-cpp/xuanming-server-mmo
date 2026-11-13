package token

import (
	"context"
	"errors"
	"testing"

	"shared/redisslot"
)

// ValidateAccess 的错误分两类:令牌本身不能用(包 ErrAccessRejected,调用方让客户端重新登录),
// 与存储故障(不包,调用方回故障、让客户端稍后重试)。本文件钉住这条分界。

func TestValidateAccessRejectionsWrapErrAccessRejected(t *testing.T) {
	m, rdb, mr := newGuardedManager(t)
	ctx := context.Background()
	pair := issueSeed(t, m)

	if _, err := m.ValidateAccess(ctx, pair.AccessToken); err != nil {
		t.Fatalf("前提:刚签发的 access token 应可用: %v", err)
	}

	cases := map[string]func(t *testing.T) string{
		"malformed": func(t *testing.T) string { return "not-a-token" },
		"refresh used as access": func(t *testing.T) string {
			return pair.RefreshToken
		},
		"unknown or expired": func(t *testing.T) string {
			other := issueSeed(t, m)
			tag, secret := mustParseToken(t, other.AccessToken)
			mr.Del(redisslot.AccessTokenKey(tag, secret))
			return other.AccessToken
		},
		"corrupt record": func(t *testing.T) string {
			other := issueSeed(t, m)
			tag, secret := mustParseToken(t, other.AccessToken)
			if err := rdb.Set(ctx, redisslot.AccessTokenKey(tag, secret), "{not json", 0).Err(); err != nil {
				t.Fatal(err)
			}
			return other.AccessToken
		},
		"family revoked": func(t *testing.T) string {
			other := issueSeed(t, m)
			parsed := parsedOf(t, other.AccessToken)
			mr.Del(redisslot.RefreshFamilyKey(parsed.tag, parsed.familyID))
			return other.AccessToken
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := m.ValidateAccess(ctx, build(t))
			if !errors.Is(err, ErrAccessRejected) {
				t.Fatalf("ValidateAccess err = %v, want ErrAccessRejected", err)
			}
		})
	}
}

// Redis 不可用时令牌可能是好的:错误不能包 ErrAccessRejected,否则调用方会把在线玩家踢回登录页。
func TestValidateAccessStoreFailureIsNotARejection(t *testing.T) {
	m, _, mr := newGuardedManager(t)
	pair := issueSeed(t, m)
	mr.Close()

	_, err := m.ValidateAccess(context.Background(), pair.AccessToken)
	if err == nil {
		t.Fatal("Redis 不可用时 ValidateAccess 不应成功")
	}
	if errors.Is(err, ErrAccessRejected) {
		t.Fatalf("存储故障不应标 ErrAccessRejected: %v", err)
	}
}
