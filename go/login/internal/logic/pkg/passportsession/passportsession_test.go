package passportsession

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"login/internal/logic/pkg/auth"
	"login/internal/logic/pkg/token"
	"shared/generated/pb/table"
	"shared/safego"
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
func newEnforcer(t *testing.T, status int, getUser string, tokens *fakeTokens, players Players) *Enforcer {
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
	e := New(admin, tokens, players)
	e.goKick = func(_ string, fn func()) { fn() } // 踢线同步跑,断言踢没踢
	return e
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

// 通行证回调(HandleEvent,§3.7):只拿回调找人,按通行证此刻的状态处理;管理组织的人不核对;格式不对回 ErrBadEvent;
// 查不了回错误(让通行证重投)。
func TestHandleEventChecksTheNamedUsersAgainstThePassport(t *testing.T) {
	user := userChangedPasswordAt(changedAt)

	// 改密:记录的目标用户与扩展用户都是玩家本人(同一个人只核对一次)。
	tokens, players := &fakeTokens{revokeCount: 1}, &fakePlayers{}
	e := newEnforcer(t, 0, user, tokens, players)
	setPassword := `{"organization":"xuanming","user":"alice","action":"set-password","object":"",` +
		`"extendedUser":{"owner":"xuanming","name":"alice","id":"` + testSub + `"}}`
	if n, err := e.HandleEvent(context.Background(), []byte(setPassword)); err != nil || n != 1 {
		t.Fatalf("set-password event: (%d, %v), want one user checked", n, err)
	}
	if len(tokens.revokedBefore) != 1 || tokens.revokedBefore[0] != changedAt || len(players.kicked) != 1 || players.kicked[0] != testAccount {
		t.Fatalf("the change must revoke earlier sessions and kick: before=%v kicked=%v", tokens.revokedBefore, players.kicked)
	}

	// 管理员在后台改资料:操作人在管理组织里,不核对;目标用户在请求体里。
	tokens2 := &fakeTokens{}
	byAdmin := `{"organization":"built-in","user":"admin","action":"update-user",` +
		`"object":"{\"owner\":\"xuanming\",\"name\":\"alice\",\"id\":\"` + testSub + `\"}",` +
		`"extendedUser":{"owner":"built-in","name":"admin","id":"admin-id"}}`
	if n, err := newEnforcer(t, 0, user, tokens2, &fakePlayers{}).HandleEvent(context.Background(), []byte(byAdmin)); err != nil || n != 1 ||
		len(tokens2.revokedBefore) != 1 {
		t.Fatalf("admin update-user: (%d, %v) revoked=%v, want the target user checked", n, err, tokens2.revokedBefore)
	}

	// 只点到管理组织的人:什么都不查、不吊销。
	tokens3 := &fakeTokens{}
	adminOnly := `{"organization":"built-in","user":"admin","action":"set-password","object":""}`
	if n, err := newEnforcer(t, http.StatusInternalServerError, "", tokens3, &fakePlayers{}).HandleEvent(context.Background(), []byte(adminOnly)); err != nil || n != 0 ||
		len(tokens3.revokedAll)+len(tokens3.revokedBefore) != 0 {
		t.Fatalf("built-in users only: (%d, %v)", n, err)
	}

	// 格式不对:回 ErrBadEvent(网关回 400,不重投)。
	for name, payload := range map[string]string{"empty": "", "not json": "x", "too large": string(make([]byte, maxEventBytes+1))} {
		if _, err := e.HandleEvent(context.Background(), []byte(payload)); !errors.Is(err, ErrBadEvent) {
			t.Errorf("%s: want ErrBadEvent, got %v", name, err)
		}
	}

	// 通行证查不了:回错误,让通行证重投。
	if _, err := newEnforcer(t, http.StatusBadGateway, "", &fakeTokens{}, &fakePlayers{}).HandleEvent(context.Background(), []byte(setPassword)); err == nil ||
		errors.Is(err, ErrBadEvent) {
		t.Fatalf("passport down: want a retryable error, got %v", err)
	}
}

// 应用凭据发起的操作(本项目注销时用服务端应用的凭据删通行证用户):记录里的操作人是 Casdoor 的应用伪用户
// 「app/<应用名>」,不能拿去查(v4.27.1 遇到三段的 id 直接 panic,整条回调一直被重投);被删的玩家按名字已查不到,
// 不吊销、不报错,通行证不重投。
func TestHandleEventSkipsAppPseudoUsers(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.Query().Get("id"))
		mu.Unlock()
		_, _ = w.Write([]byte(`{"status":"ok","msg":"","data":null}`))
	}))
	t.Cleanup(srv.Close)
	admin, err := auth.NewPassportAdmin("https://passport.example.com", srv.URL, "xuanming-game-server", "test-admin-secret", srv.Client())
	if err != nil {
		t.Fatalf("NewPassportAdmin: %v", err)
	}
	tokens, players := &fakeTokens{}, &fakePlayers{}
	e := New(admin, tokens, players)
	e.goKick = func(_ string, fn func()) { fn() }

	deleteByApp := `{"organization":"xuanming","user":"app/app-xuanming-server","action":"delete-user",` +
		`"object":"{\"owner\":\"xuanming\",\"name\":\"alice\"}"}`
	if n, err := e.HandleEvent(context.Background(), []byte(deleteByApp)); err != nil || n != 0 {
		t.Fatalf("delete-user by the server app: (%d, %v), want nothing checked and no error", n, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 1 || asked[0] != "xuanming/alice" {
		t.Fatalf("only the deleted player may be looked up, asked=%v", asked)
	}
	if len(tokens.revokedAll)+len(tokens.revokedBefore)+len(players.kicked) != 0 {
		t.Fatalf("a user gone by name cannot be resolved: all=%v before=%v kicked=%v", tokens.revokedAll, tokens.revokedBefore, players.kicked)
	}
}

// blockingPlayers 的踢线一直卡到 release 关掉,踢完关 done。
type blockingPlayers struct {
	release chan struct{}
	done    chan struct{}
}

func (b *blockingPlayers) KickAccountPlayers(context.Context, string, uint32) error {
	<-b.release
	close(b.done)
	return nil
}

// 踢线在后台跑:登录不等它(同步踢线会把登录拖过 login 的 RPC 时限,而身份令牌已被一次性消费),踢线照样跑完。
func TestKickRunsInTheBackground(t *testing.T) {
	players := &blockingPlayers{release: make(chan struct{}), done: make(chan struct{})}
	e := newEnforcer(t, 0, userChangedPasswordAt(changedAt), &fakeTokens{revokeCount: 1}, players)
	e.goKick = safego.Go // 生产的后台执行

	returned := make(chan uint32, 1)
	go func() { returned <- e.CheckLogin(context.Background(), testAccount, changedAt+60) }()
	select {
	case tip := <-returned:
		if tip != 0 {
			t.Fatalf("a login after the change must pass, got tip %d", tip)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CheckLogin must not wait for the kick")
	}
	close(players.release)
	select {
	case <-players.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the background kick must still run")
	}
}

func TestHandleEventRevokesEverythingForBannedUsers(t *testing.T) {
	tokens, players := &fakeTokens{}, &fakePlayers{}
	banned := `{"owner":"xuanming","name":"alice","id":"` + testSub + `","isForbidden":true}`
	event := `{"organization":"xuanming","user":"alice","object":"","extendedUser":{"owner":"xuanming","name":"alice","id":"` + testSub + `"}}`
	if n, err := newEnforcer(t, 0, banned, tokens, players).HandleEvent(context.Background(), []byte(event)); err != nil || n != 1 {
		t.Fatalf("ban event: (%d, %v)", n, err)
	}
	if len(tokens.revokedAll) != 1 || tokens.revokedAll[0] != testAccount || len(players.kicked) != 1 {
		t.Fatalf("a ban must revoke every session and kick: all=%v kicked=%v", tokens.revokedAll, players.kicked)
	}
}
