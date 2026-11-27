package token

import (
	"context"
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
