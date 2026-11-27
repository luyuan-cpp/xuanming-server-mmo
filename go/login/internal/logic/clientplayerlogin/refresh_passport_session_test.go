package clientplayerloginlogic

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"login/internal/logic/pkg/auth"
	"login/internal/logic/pkg/token"
	"login/internal/svc"
	login_proto "proto/login"
	"shared/generated/pb/table"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// 刷新前核对官方通行证(refreshtokenlogic.go passportSessionRevoked,official-passport-registration.md §3.6):
// 登录之后改过密码就吊销整个账号、拒绝续期;改密在登录之前照常续期;通行证故障照常续期;不是通行证登录不去问通行证。

const testRefreshPassportSub = "550e8400-e29b-41d4-a716-446655440000"

// passportGetUserStub 模拟 Casdoor 的 get-user:status 为 0 时回 body,否则直接回这个 HTTP 状态码。
type passportGetUserStub struct {
	status int
	body   string
	calls  atomic.Int32
}

func (s *passportGetUserStub) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	s.calls.Add(1)
	if s.status != 0 {
		w.WriteHeader(s.status)
		return
	}
	_, _ = w.Write([]byte(s.body))
}

func passportUserChangedPasswordAt(changedAt time.Time) string {
	return `{"status":"ok","msg":"","data":{"owner":"xuanming","name":"alice","id":"` + testRefreshPassportSub +
		`","lastChangePasswordTime":"` + changedAt.Format(time.RFC3339) + `"}}`
}

// newPassportRefreshFixture:stub 为 nil 时不配管理接口(dev/test 没配管理凭据的情形)。
func newPassportRefreshFixture(t *testing.T, stub *passportGetUserStub) *svc.ServiceContext {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	svcCtx := &svc.ServiceContext{TokenManager: token.NewManager(rdb, token.Config{AccessTokenTTL: time.Hour, RefreshTokenTTL: 24 * time.Hour})}
	if stub != nil {
		srv := httptest.NewServer(stub)
		t.Cleanup(srv.Close)
		admin, err := auth.NewPassportAdmin("https://passport.example.com", srv.URL, "xuanming-game-server", "test-admin-secret", srv.Client())
		if err != nil {
			t.Fatalf("NewPassportAdmin: %v", err)
		}
		svcCtx.PassportAdmin = admin
	}
	return svcCtx
}

func issueForRefreshTest(t *testing.T, svcCtx *svc.ServiceContext, account, authType string, identityIssuedAt int64) *token.TokenPair {
	t.Helper()
	pair, err := svcCtx.TokenManager.Issue(context.Background(), account, authType, "", identityIssuedAt)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return pair
}

func refreshForTest(t *testing.T, svcCtx *svc.ServiceContext, refreshToken string) *login_proto.RefreshTokenResponse {
	t.Helper()
	resp, err := NewRefreshTokenLogic(context.Background(), svcCtx).RefreshToken(&login_proto.RefreshTokenRequest{RefreshToken: refreshToken})
	if err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	return resp
}

func requireRenewed(t *testing.T, name string, resp *login_proto.RefreshTokenResponse) {
	t.Helper()
	if resp.GetErrorMessage() != nil || resp.GetRefreshToken() == "" || resp.GetAccessToken() == "" {
		t.Fatalf("%s: want a renewed pair, got error=%v", name, resp.GetErrorMessage())
	}
}

func TestRefreshRevokesPassportSessionsLoggedInBeforePasswordChange(t *testing.T) {
	loginAt := time.Now().Add(-time.Hour).Truncate(time.Second)
	stub := &passportGetUserStub{body: passportUserChangedPasswordAt(loginAt.Add(10 * time.Minute))}
	svcCtx := newPassportRefreshFixture(t, stub)
	account := "passport_" + testRefreshPassportSub
	thief := issueForRefreshTest(t, svcCtx, account, auth.PassportAuthType, loginAt.Unix())
	otherDevice := issueForRefreshTest(t, svcCtx, account, auth.PassportAuthType, loginAt.Unix())

	resp := refreshForTest(t, svcCtx, thief.RefreshToken)
	if resp.GetErrorMessage().GetId() != uint32(table.LoginError_kLoginAccountNotFound) || resp.GetRefreshToken() != "" {
		t.Fatalf("a login from before the password change must not be renewed: error=%v refresh=%q", resp.GetErrorMessage(), resp.GetRefreshToken())
	}
	// 整个账号都吊销:另一台设备的 access 立即失效,它的 refresh 也续不了。
	if _, err := svcCtx.TokenManager.ValidateAccess(context.Background(), otherDevice.AccessToken); !errors.Is(err, token.ErrAccessRejected) {
		t.Fatalf("other device access token after revocation: %v, want ErrAccessRejected", err)
	}
	if got := refreshForTest(t, svcCtx, otherDevice.RefreshToken); got.GetRefreshToken() != "" {
		t.Fatal("other device refresh token must be revoked too")
	}
}

func TestRefreshRenewsPassportSessionsLoggedInAfterPasswordChange(t *testing.T) {
	changedAt := time.Now().Add(-time.Hour).Truncate(time.Second)
	stub := &passportGetUserStub{body: passportUserChangedPasswordAt(changedAt)}
	svcCtx := newPassportRefreshFixture(t, stub)
	pair := issueForRefreshTest(t, svcCtx, "passport_"+testRefreshPassportSub, auth.PassportAuthType, changedAt.Add(time.Minute).Unix())

	first := refreshForTest(t, svcCtx, pair.RefreshToken)
	requireRenewed(t, "first refresh", first)
	// 轮换出来的令牌带着最初的签发时刻,再续一次照样核对、照样通过。
	requireRenewed(t, "second refresh", refreshForTest(t, svcCtx, first.GetRefreshToken()))
	if got := stub.calls.Load(); got != 2 {
		t.Fatalf("passport get-user calls = %d, want one per refresh", got)
	}
}

func TestRefreshRenewsWhenPassportIsUnreachable(t *testing.T) {
	stub := &passportGetUserStub{status: http.StatusBadGateway}
	svcCtx := newPassportRefreshFixture(t, stub)
	pair := issueForRefreshTest(t, svcCtx, "passport_"+testRefreshPassportSub, auth.PassportAuthType, 1)
	requireRenewed(t, "passport down", refreshForTest(t, svcCtx, pair.RefreshToken))
	if stub.calls.Load() != 1 {
		t.Fatal("the passport should have been asked once")
	}
}

func TestRefreshDoesNotAskPassportForOtherLogins(t *testing.T) {
	stub := &passportGetUserStub{status: http.StatusInternalServerError}
	svcCtx := newPassportRefreshFixture(t, stub)
	// 口令账号的名字碰巧以 passport_ 开头:按签发时的 auth_type 判,不去问通行证。
	pair := issueForRefreshTest(t, svcCtx, "passport_"+testRefreshPassportSub, "password", 0)
	requireRenewed(t, "password login", refreshForTest(t, svcCtx, pair.RefreshToken))
	if stub.calls.Load() != 0 {
		t.Fatal("a non-passport login must not ask the passport")
	}

	// 没配管理凭据(只有 dev/test 允许):通行证登录照常续期。
	bare := newPassportRefreshFixture(t, nil)
	passportPair := issueForRefreshTest(t, bare, "passport_"+testRefreshPassportSub, auth.PassportAuthType, 0)
	requireRenewed(t, "no admin client", refreshForTest(t, bare, passportPair.RefreshToken))
}
