package token

import (
	"context"
	"errors"
	"testing"
)

// 身份令牌签发时刻(TokenData.IdentityIssuedAt)跟着家族走:签发时写进载荷,轮换时原样带下去;
// ReadRefreshPayload 只读,令牌不在或格式不对回 (nil, nil)。官方通行证刷新前的改密核对靠这两条(clientplayerlogin/refreshtokenlogic.go)。
func TestIdentityIssuedAtSurvivesRotation(t *testing.T) {
	m, _, _ := newTestManager(t)
	ctx := context.Background()
	const issuedAt = int64(1795000000)
	pair, err := m.Issue(ctx, testAccount, testAuthType, testDevice, issuedAt)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	first, err := m.ReadRefreshPayload(ctx, pair.RefreshToken)
	if err != nil || first == nil || first.IdentityIssuedAt != issuedAt || first.Account != testAccount || first.AuthType != testAuthType {
		t.Fatalf("ReadRefreshPayload after Issue = (%+v, %v)", first, err)
	}

	next := mustRefresh(t, m, pair.RefreshToken)
	rotated, err := m.ReadRefreshPayload(ctx, next.RefreshToken)
	if err != nil || rotated == nil || rotated.IdentityIssuedAt != issuedAt {
		t.Fatalf("rotation must carry IdentityIssuedAt unchanged: (%+v, %v)", rotated, err)
	}
	access, err := m.ValidateAccess(ctx, next.AccessToken)
	if err != nil || access.IdentityIssuedAt != issuedAt {
		t.Fatalf("rotated access token payload = (%+v, %v)", access, err)
	}

	for name, raw := range map[string]string{
		"rotated away": pair.RefreshToken,
		"malformed":    "not-a-token",
		"access token": next.AccessToken,
	} {
		if data, err := m.ReadRefreshPayload(ctx, raw); data != nil || err != nil {
			t.Errorf("%s: ReadRefreshPayload = (%+v, %v), want (nil, nil)", name, data, err)
		}
	}
}

// RevokeIssuedBefore 只撤签发时刻不晚于截止时刻的家族(没有签发时刻的按 0 算):撤掉的家族 access 立即失效、refresh 续不了,
// 截止之后登录的家族照常可用;再撤一次什么都没有。官方通行证改密之后靠它只作废改密之前的登录。
func TestRevokeIssuedBeforeKeepsLaterLogins(t *testing.T) {
	m, _, _ := newTestManager(t)
	ctx := context.Background()
	issueAt := func(issuedAt int64) *TokenPair {
		t.Helper()
		pair, err := m.Issue(ctx, testAccount, testAuthType, testDevice, issuedAt)
		if err != nil {
			t.Fatalf("Issue(%d): %v", issuedAt, err)
		}
		return pair
	}
	const changedAt = int64(2000)
	revokedPairs := map[string]*TokenPair{
		"logged in before the change":     issueAt(changedAt - 1000),
		"logged in in the same second":    issueAt(changedAt),
		"issued before the field existed": issueAt(0),
	}
	kept := issueAt(changedAt + 1)

	n, err := m.RevokeIssuedBefore(ctx, testAccount, changedAt)
	if err != nil || n != int64(len(revokedPairs)) {
		t.Fatalf("RevokeIssuedBefore = (%d, %v), want %d", n, err, len(revokedPairs))
	}
	for name, pair := range revokedPairs {
		if _, err := m.ValidateAccess(ctx, pair.AccessToken); !errors.Is(err, ErrAccessRejected) {
			t.Errorf("%s: access after revocation: %v, want ErrAccessRejected", name, err)
		}
		requireRejected(t, m, pair.RefreshToken)
	}
	if _, err := m.ValidateAccess(ctx, kept.AccessToken); err != nil {
		t.Fatalf("a login after the change must keep working: %v", err)
	}
	mustRefresh(t, m, kept.RefreshToken)

	if n, err := m.RevokeIssuedBefore(ctx, testAccount, changedAt); err != nil || n != 0 {
		t.Fatalf("second RevokeIssuedBefore = (%d, %v), want nothing left to revoke", n, err)
	}
	if n, err := m.RevokeIssuedBefore(ctx, "account-without-tokens", changedAt); err != nil || n != 0 {
		t.Fatalf("RevokeIssuedBefore on an account without tokens = (%d, %v)", n, err)
	}
}
