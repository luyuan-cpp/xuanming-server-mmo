package passportsession

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"login/internal/logic/pkg/auth"
	"login/internal/logic/pkg/token"
	"shared/generated/pb/table"
)

// 钉住 Enforcer 的口径(docs/design/official-passport-registration.md §3.6):
//   - 登录:身份令牌早于改密、用户被封被删都拒绝;改密之前的旧登录按改密时刻吊销,吊销了才踢线;核对不了放行。
//   - 刷新:不是通行证登录签的不去问;早于改密不续期;吊销失败也不续期;核对不了照常续期。
//   - 被封被删吊销全部令牌并踢线。

const (
	testSub     = "550e8400-e29b-41d4-a716-446655440000"
	testAccount = "passport_" + testSub
)

type fakeTokens struct {
	revokedAll    []string
	revokedBefore []int64
	revokeCount   int64
	err           error
}

func (f *fakeTokens) RevokeAll(_ context.Context, account string) error {
	f.revokedAll = append(f.revokedAll, account)
	return f.err
}

func (f *fakeTokens) RevokeIssuedBefore(_ context.Context, _ string, cutoff int64) (int64, error) {
	f.revokedBefore = append(f.revokedBefore, cutoff)
	return f.revokeCount, f.err
}

type fakePlayers struct {
	kicked []string
	tips   []uint32
}

func (f *fakePlayers) KickAccountPlayers(_ context.Context, account string, reasonTip uint32) error {
	f.kicked = append(f.kicked, account)
	f.tips = append(f.tips, reasonTip)
	return nil
}

// newEnforcer 起一个只回 getUser 原文的 Casdoor get-user 桩;status 非 0 时直接回这个 HTTP 状态码。
func newEnforcer(t *testing.T, status int, getUser string, tokens *fakeTokens, players *fakePlayers) *Enforcer {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok","msg":"","data":` + getUser + `}`))
	}))
	t.Cleanup(srv.Close)
	admin, err := auth.NewPassportAdmin("https://passport.example.com", srv.URL, "xuanming-game-server", "test-admin-secret", srv.Client())
	if err != nil {
		t.Fatalf("NewPassportAdmin: %v", err)
	}
	return New(admin, tokens, players)
}

func userChangedPasswordAt(changedAt int64) string {
	return `{"owner":"xuanming","name":"alice","id":"` + testSub + `","lastChangePasswordTime":"` +
		time.Unix(changedAt, 0).UTC().Format(time.RFC3339) + `"}`
}

const changedAt = int64(1795000000)

var credentialChangedTip = uint32(table.LoginError_kLoginPassportCredentialChanged)

func TestCheckLoginRefusesIdentityTokensFromBeforeThePasswordChange(t *testing.T) {
	tokens, players := &fakeTokens{revokeCount: 2}, &fakePlayers{}
	e := newEnforcer(t, 0, userChangedPasswordAt(changedAt), tokens, players)

	if tip := e.CheckLogin(context.Background(), testAccount, changedAt-60); tip != credentialChangedTip {
		t.Fatalf("a token issued before the change must be refused with the credential-changed tip, got %d", tip)
	}
	if len(tokens.revokedBefore) != 1 || tokens.revokedBefore[0] != changedAt || len(tokens.revokedAll) != 0 {
		t.Fatalf("earlier sessions must be revoked up to the change time only: before=%v all=%v", tokens.revokedBefore, tokens.revokedAll)
	}
	if len(players.kicked) != 1 || players.kicked[0] != testAccount || players.tips[0] != credentialChangedTip {
		t.Fatalf("revoked sessions must be kicked with the credential-changed tip: %v %v", players.kicked, players.tips)
	}
}

func TestCheckLoginAfterThePasswordChangeRevokesOnlyEarlierSessions(t *testing.T) {
	tokens, players := &fakeTokens{revokeCount: 1}, &fakePlayers{}
	e := newEnforcer(t, 0, userChangedPasswordAt(changedAt), tokens, players)
	if tip := e.CheckLogin(context.Background(), testAccount, changedAt+60); tip != 0 {
		t.Fatalf("a login after the change must go through, got tip %d", tip)
	}
	if len(tokens.revokedBefore) != 1 || tokens.revokedBefore[0] != changedAt || len(players.kicked) != 1 {
		t.Fatalf("the thief's earlier sessions must be revoked and kicked: before=%v kicked=%v", tokens.revokedBefore, players.kicked)
	}

	// 旧登录早就清掉了(吊销 0 个):不踢线,免得把玩家改密之后在别的设备上的会话也踢掉。
	tokens2, players2 := &fakeTokens{}, &fakePlayers{}
	if tip := newEnforcer(t, 0, userChangedPasswordAt(changedAt), tokens2, players2).CheckLogin(context.Background(), testAccount, changedAt+60); tip != 0 ||
		len(players2.kicked) != 0 {
		t.Fatalf("nothing revoked must kick nobody: tip=%d kicked=%v", tip, players2.kicked)
	}

	// 从没改过密码:什么都不吊销。
	tokens3 := &fakeTokens{}
	never := `{"owner":"xuanming","name":"alice","id":"` + testSub + `"}`
	if tip := newEnforcer(t, 0, never, tokens3, &fakePlayers{}).CheckLogin(context.Background(), testAccount, changedAt); tip != 0 ||
		len(tokens3.revokedBefore)+len(tokens3.revokedAll) != 0 {
		t.Fatalf("no password change: tip=%d revoked=%v/%v", tip, tokens3.revokedBefore, tokens3.revokedAll)
	}
}

func TestBannedOrDeletedUsersLoseEverySession(t *testing.T) {
	for name, getUser := range map[string]string{
		"forbidden": `{"owner":"xuanming","name":"alice","id":"` + testSub + `","isForbidden":true}`,
		"deleted":   "null",
	} {
		tokens, players := &fakeTokens{}, &fakePlayers{}
		e := newEnforcer(t, 0, getUser, tokens, players)
		if tip := e.CheckLogin(context.Background(), testAccount, changedAt); tip != credentialChangedTip {
			t.Errorf("%s: login must be refused, got tip %d", name, tip)
		}
		if len(tokens.revokedAll) != 1 || len(players.kicked) != 1 {
			t.Errorf("%s: every session must be revoked and kicked: all=%v kicked=%v", name, tokens.revokedAll, players.kicked)
		}
	}
}

func TestPassportOutagesLetLoginsAndRefreshesThrough(t *testing.T) {
	tokens := &fakeTokens{}
	e := newEnforcer(t, http.StatusBadGateway, "", tokens, &fakePlayers{})
	if tip := e.CheckLogin(context.Background(), testAccount, changedAt); tip != 0 {
		t.Fatalf("passport down: login must go through, got tip %d", tip)
	}
	if e.CheckRefresh(context.Background(), &token.TokenData{Account: testAccount, AuthType: auth.PassportAuthType, IdentityIssuedAt: 1}) {
		t.Fatal("passport down: refresh must go through")
	}
	if len(tokens.revokedAll)+len(tokens.revokedBefore) != 0 {
		t.Fatal("nothing may be revoked when the passport cannot be asked")
	}
}

func TestCheckRefresh(t *testing.T) {
	stale := &token.TokenData{Account: testAccount, AuthType: auth.PassportAuthType, IdentityIssuedAt: changedAt - 60}
	fresh := &token.TokenData{Account: testAccount, AuthType: auth.PassportAuthType, IdentityIssuedAt: changedAt + 60}

	tokens, players := &fakeTokens{revokeCount: 1}, &fakePlayers{}
	e := newEnforcer(t, 0, userChangedPasswordAt(changedAt), tokens, players)
	if !e.CheckRefresh(context.Background(), stale) {
		t.Fatal("a session from before the change must not be renewed")
	}
	if len(tokens.revokedBefore) != 1 || tokens.revokedBefore[0] != changedAt || len(players.kicked) != 1 {
		t.Fatalf("refusing must revoke up to the change time and kick: before=%v kicked=%v", tokens.revokedBefore, players.kicked)
	}
	if e.CheckRefresh(context.Background(), fresh) {
		t.Fatal("a session from after the change must be renewed")
	}

	// 吊销失败也不续期(旧令牌还在,下一次再来)。
	failing := &fakeTokens{err: errors.New("redis down")}
	if !newEnforcer(t, 0, userChangedPasswordAt(changedAt), failing, &fakePlayers{}).CheckRefresh(context.Background(), stale) {
		t.Fatal("a failed revocation must still refuse the refresh")
	}

	// 不是通行证登录签的:不去问通行证(桩回 500 也照常续期,也不吊销)。
	untouched := &fakeTokens{}
	other := newEnforcer(t, http.StatusInternalServerError, "", untouched, &fakePlayers{})
	if other.CheckRefresh(context.Background(), &token.TokenData{Account: testAccount, AuthType: "password"}) || len(untouched.revokedAll)+len(untouched.revokedBefore) != 0 {
		t.Fatal("a password login named passport_* must not be checked against the passport")
	}
}

func TestNilEnforcerLetsEverythingThrough(t *testing.T) {
	var e *Enforcer
	if New(nil, &fakeTokens{}, nil) != nil {
		t.Fatal("no admin client means no enforcer")
	}
	if e.CheckLogin(context.Background(), testAccount, 1) != 0 || e.CheckRefresh(context.Background(), &token.TokenData{Account: testAccount, AuthType: auth.PassportAuthType}) {
		t.Fatal("a nil enforcer (no admin credentials, dev/test only) must let logins and refreshes through")
	}
}
