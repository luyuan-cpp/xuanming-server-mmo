package clientplayerloginlogic

// LegacyGateLoginEnabled=false 时 Login 全链路的回归测试(走真实 Login(),miniredis 承载 Redis)。
//
// 事故形态:旧判定只看「带不带会话」,把新流程第二步——gate 上的 Login{auth_type:"access_token"}——
// 也当成旧路径拒掉(回 kLoginUnknownError)。那一步是 login_session 与设备集的唯一写入点,
// 开关一关,CreatePlayer / EnterGame 第一步按会话取账号就读不到 login_session、回 kLoginSessionNotFound,
// 所有客户端都进不了游戏。
//
// 2026-10-18 起旧路径在认证之前拒绝:被拒的请求不调用认证器(第三方 code 不被消费)、不抢账号锁、
// 不建账号目录;凭据真假都回 kLoginUnknownError。对应断言见第三方 / 口令 / 账号锁三个用例。
//
// 前三个用例最初只用 Login() 与各包既有的公开接口写成,能原样放到 access_token 误拦修复前的代码上跑出红灯;
// 「认证之前拒绝」的断言另外用到同包的 legacyLoginCount 与 accountLoginLockKey。
// 本包用例改写 config.AppConfig 与进程级计数,不能加 t.Parallel()。

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"login/internal/config"
	"login/internal/constants"
	"login/internal/logic/pkg/auth"
	"login/internal/logic/pkg/ctxkeys"
	"login/internal/logic/pkg/loginsession"
	"login/internal/logic/pkg/token"
	"login/internal/svc"
	pbbase "proto/common/base"
	loginpb "proto/login"
	"shared/generated/pb/table"
	"shared/redisslot"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

const killSwitchSessionID = uint32(7101)

// auth 注册表是进程级的、重名即 panic、没有注销接口,所以 "access_token" 在本测试二进制里只注册一次,
// 由转发器按当前用例取 token 管理器(各用例各起一个 miniredis)。本包里别处若也要注册 "access_token",改用这里的转发器。
var (
	killSwitchTokens          atomic.Pointer[token.Manager]
	killSwitchAccessTokenOnce sync.Once
	killSwitchAuthSequence    atomic.Uint64
)

type killSwitchAccessTokenProvider struct{}

func (killSwitchAccessTokenProvider) Validate(ctx context.Context, accessToken string) (*auth.AuthResult, error) {
	tokens := killSwitchTokens.Load()
	if tokens == nil {
		return nil, errors.New("killswitch test: token manager not installed")
	}
	return (&auth.AccessTokenProvider{TokenManager: tokens}).Validate(ctx, accessToken)
}

// killSwitchThirdParty 模拟第三方首认证(微信 / QQ 用 code 换身份),记下被调用的次数。
type killSwitchThirdParty struct {
	account string
	calls   atomic.Int32
}

func (p *killSwitchThirdParty) Validate(context.Context, string) (*auth.AuthResult, error) {
	p.calls.Add(1)
	return &auth.AuthResult{Account: p.account}, nil
}

type killSwitchEnv struct {
	mr             *miniredis.Miniredis
	rdb            *redis.Client
	svcCtx         *svc.ServiceContext
	thirdPartyType string
	thirdParty     *killSwitchThirdParty
}

func newKillSwitchEnv(t *testing.T, legacyGateLoginEnabled bool, account string) *killSwitchEnv {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	previousConfig := config.AppConfig
	t.Cleanup(func() { config.AppConfig = previousConfig })
	config.AppConfig.LegacyGateLoginEnabled = legacyGateLoginEnabled
	config.AppConfig.Locker.AccountLockTTL = 10
	config.AppConfig.Account.MaxDevicesPerAccount = 3
	config.AppConfig.Node.SessionExpireMin = 30
	config.AppConfig.HomeZone.RefreshRoleListDisabled = true

	tokens := token.NewManager(rdb, token.Config{AccessTokenTTL: time.Hour, RefreshTokenTTL: 30 * 24 * time.Hour})
	killSwitchAccessTokenOnce.Do(func() { auth.Register("access_token", killSwitchAccessTokenProvider{}) })
	killSwitchTokens.Store(tokens)
	t.Cleanup(func() { killSwitchTokens.Store(nil) })

	// 第三方认证器每个用例一个独立名字:重名即 panic,也不碰生产认证器或其他用例的状态。
	thirdPartyType := fmt.Sprintf("killswitch-third-party-%d", killSwitchAuthSequence.Add(1))
	thirdParty := &killSwitchThirdParty{account: account}
	auth.Register(thirdPartyType, thirdParty)

	return &killSwitchEnv{
		mr:             mr,
		rdb:            rdb,
		svcCtx:         &svc.ServiceContext{RedisClient: rdb, LoginRedis: rdb, AccountDirectory: fixtureDirectory(t, rdb), TokenManager: tokens},
		thirdPartyType: thirdPartyType,
		thirdParty:     thirdParty,
	}
}

// killSwitchGateCtx 模拟 cpp gate HandleGrpcNodeMessage 转发来的请求上下文。
func killSwitchGateCtx() context.Context {
	return ctxkeys.WithSessionDetails(context.Background(), &pbbase.SessionDetails{
		SessionId: killSwitchSessionID, GateInstanceId: "gate-uuid-killswitch", SessionIncarnation: 9,
	})
}

func killSwitchSessionRef(t *testing.T, ctx context.Context) loginsession.Ref {
	t.Helper()
	details, found := ctxkeys.GetSessionDetails(ctx)
	require.True(t, found, "测试上下文必须包含本连接身份")
	ref, ok := loginsession.RefOf(details)
	require.True(t, ok, "测试上下文的连接身份必须完整")
	return ref
}

// requireNoSessionWritten 断言 Login 没有留下任何会话状态:登录态未写入,设备集也没有这条连接。
func requireNoSessionWritten(t *testing.T, env *killSwitchEnv, ctx context.Context, account string) {
	t.Helper()
	ref := killSwitchSessionRef(t, ctx)
	require.False(t, env.mr.Exists(ref.Key()), "被拒的请求不得写 login_session")
	member, err := env.rdb.SIsMember(ctx, redisslot.AccountDeviceSetKey(account), ref.DeviceMember()).Result()
	require.NoError(t, err)
	require.False(t, member, "被拒的请求不得登记设备集")
}

// requireRejectedBeforeAuth 断言被 kill switch 拒绝的请求停在认证之前:没调用第三方认证器,
// 没留下账号锁,也没建账号目录(GetOrInitUserAccount 会在账号没有目录时写一份空目录)。
func requireRejectedBeforeAuth(t *testing.T, env *killSwitchEnv, account string) {
	t.Helper()
	require.Zero(t, env.thirdParty.calls.Load(),
		"被拒的旧路径不得调用认证器:第三方 code 是一次性的,消费掉之后客户端改走 /api/login 还得重新拉起授权")
	require.False(t, env.mr.Exists(accountLoginLockKey(account)), "被拒的请求不得留下账号锁")
	require.False(t, env.mr.Exists(constants.GetAccountDataKey(account)), "被拒的请求不得建账号目录")
}

// 新流程(robot/main.go loginAndEnterViaHttpGateway 同序):/api/login 首认证拿 access_token →
// gate 上 Login{access_token} 绑定会话。开关关闭时第二步必须照常成功,并写下 CreatePlayer / EnterGame 要读的 login_session。
func TestLegacyGateLoginDisabled_GateAccessTokenLoginStillBindsSession(t *testing.T) {
	const account = "killswitch-new-flow"
	env := newKillSwitchEnv(t, false, account)

	// 第 1 步:Java Gateway POST /api/login 直调 login(不带会话),第三方凭据首认证。
	httpResp, err := NewLoginLogic(context.Background(), env.svcCtx).Login(&loginpb.LoginRequest{
		AuthType: env.thirdPartyType, AuthToken: "oauth-code",
	})
	require.NoError(t, err)
	require.Zero(t, httpResp.GetErrorMessage().GetId(), "开关只管 gate 上的凭据登录,HTTP 入口不受影响")
	require.NotEmpty(t, httpResp.GetAccessToken(), "HTTP 首认证要签 access_token,供 gate 上绑定会话")

	// 第 2 步:连上 gate 后在 TCP 上发 Login{access_token}。
	gateCtx := killSwitchGateCtx()
	gateResp, err := NewLoginLogic(gateCtx, env.svcCtx).Login(&loginpb.LoginRequest{
		AuthType: "access_token", AuthToken: httpResp.GetAccessToken(),
	})
	require.NoError(t, err)
	require.Zero(t, gateResp.GetErrorMessage().GetId(),
		"LegacyGateLoginEnabled=false 不得拒绝 gate 上的 access_token 绑定,否则谁都进不了游戏")
	require.Empty(t, gateResp.GetAccessToken(), "access_token 绑定不轮换 token")
	require.Equal(t, int32(1), env.thirdParty.calls.Load(), "gate 上的绑定只验自家 token,不再回源第三方")

	// CreatePlayer / EnterGame 的第一步就是按会话取账号:取得到才进得了游戏。
	ref := killSwitchSessionRef(t, gateCtx)
	bound, err := loginsession.GetAccount(gateCtx, env.rdb, ref)
	require.NoError(t, err, "login_session 必须由 gate 上的 access_token Login 写入")
	require.Equal(t, account, bound)
	member, err := env.rdb.SIsMember(gateCtx, redisslot.AccountDeviceSetKey(account), ref.DeviceMember()).Result()
	require.NoError(t, err)
	require.True(t, member, "设备集必须登记这条会话(设备数上限靠它)")
}

// gate 上直接用第三方凭据首认证是要关掉的旧路径:开关打开时照常服务(对照组,证明用例不是空转),
// 关闭时在认证之前回 kLoginUnknownError:不调用认证器、不抢账号锁、不建账号目录、不签 token、
// 不回角色、不留任何会话状态,但照样计进 legacy(运维靠它看开关关闭后还剩多少尾巴)。
func TestLegacyGateLoginDisabled_GateThirdPartyFirstAuthRejected(t *testing.T) {
	cases := []struct {
		name    string
		enabled bool
	}{
		{name: "flag on serves legacy gate login", enabled: true},
		{name: "flag off rejects legacy gate login", enabled: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const account = "killswitch-legacy-third-party"
			env := newKillSwitchEnv(t, tc.enabled, account)

			legacyBefore := legacyLoginCount.Load()
			gateCtx := killSwitchGateCtx()
			resp, err := NewLoginLogic(gateCtx, env.svcCtx).Login(&loginpb.LoginRequest{
				AuthType: env.thirdPartyType, AuthToken: "oauth-code",
			})
			require.NoError(t, err)
			require.Equal(t, legacyBefore+1, legacyLoginCount.Load(), "gate 上的凭据登录无论放行还是被拒都要计进 legacy")

			if tc.enabled {
				require.Zero(t, resp.GetErrorMessage().GetId())
				require.NotEmpty(t, resp.GetAccessToken())
				require.Equal(t, int32(1), env.thirdParty.calls.Load(), "对照组:放行的旧路径照常调用认证器")
				bound, err := loginsession.GetAccount(gateCtx, env.rdb, killSwitchSessionRef(t, gateCtx))
				require.NoError(t, err)
				require.Equal(t, account, bound)
				return
			}
			require.Equal(t, uint32(table.LoginError_kLoginUnknownError), resp.GetErrorMessage().GetId())
			require.Empty(t, resp.GetAccessToken(), "被拒的旧路径不得签 token")
			require.Empty(t, resp.GetRefreshToken())
			require.Empty(t, resp.GetPlayers())
			requireRejectedBeforeAuth(t, env, account)
			requireNoSessionWritten(t, env, gateCtx, account)
		})
	}
}

// 被拒的请求不抢账号锁:锁先被同账号的另一次登录占着。若拒绝点还在抢锁之后,这里会先回
// kLoginInProgress(且第三方 code 已被消费);拒绝点在认证之前,则回 kLoginUnknownError,锁原样不动。
func TestLegacyGateLoginDisabled_RejectedBeforeAccountLock(t *testing.T) {
	const account = "killswitch-legacy-locked"
	const holder = "held-by-another-login"
	env := newKillSwitchEnv(t, false, account)
	lockKey := accountLoginLockKey(account)
	require.NoError(t, env.mr.Set(lockKey, holder))
	env.mr.SetTTL(lockKey, 10*time.Second)

	resp, err := NewLoginLogic(killSwitchGateCtx(), env.svcCtx).Login(&loginpb.LoginRequest{
		AuthType: env.thirdPartyType, AuthToken: "oauth-code",
	})
	require.NoError(t, err)
	require.Equal(t, uint32(table.LoginError_kLoginUnknownError), resp.GetErrorMessage().GetId(),
		"kill switch 必须先于账号锁生效")
	require.Zero(t, env.thirdParty.calls.Load(), "被拒的旧路径不得调用认证器")
	held, err := env.mr.Get(lockKey)
	require.NoError(t, err)
	require.Equal(t, holder, held, "被拒的请求不得改动别人持有的账号锁")
	require.False(t, env.mr.Exists(constants.GetAccountDataKey(account)), "被拒的请求不得建账号目录")
}

// gate 上直接用口令登录(空 auth_type 是 "password" 的旧拼法)在开关关闭时同样在认证之前被拒:
// 回 kLoginUnknownError、不留会话状态、不建账号目录。
//
// 本包用例不注册口令认证器(TestLoginPasswordPathsReturnAuthErrorBeforeSideEffects 依赖进程级认证器为空),
// 与生产默认一致。拒绝点挪到认证之前以后,回的码不再取决于口令认证是否开启——以前认证先失败、回
// kLoginAccountNotFound,现在凭据真假都回 kLoginUnknownError。口令在 gate 上归旧路径这一点由 TestClassifyLoginPath 钉住。
func TestLegacyGateLoginDisabled_GatePasswordLoginRejected(t *testing.T) {
	for _, authType := range []string{"", "password"} {
		t.Run("auth_type="+authType, func(t *testing.T) {
			const account = "killswitch-legacy-password"
			env := newKillSwitchEnv(t, false, account)

			gateCtx := killSwitchGateCtx()
			resp, err := NewLoginLogic(gateCtx, env.svcCtx).Login(&loginpb.LoginRequest{
				AuthType: authType, Account: account, Password: "anything",
			})
			require.NoError(t, err)
			require.Equal(t, uint32(table.LoginError_kLoginUnknownError), resp.GetErrorMessage().GetId(),
				"gate 上的口令登录在开关关闭时必须在认证之前被拒")
			require.Empty(t, resp.GetAccessToken())
			require.Empty(t, resp.GetPlayers())
			require.False(t, env.mr.Exists(accountLoginLockKey(account)), "被拒的请求不得留下账号锁")
			require.False(t, env.mr.Exists(constants.GetAccountDataKey(account)), "被拒的请求不得建账号目录")
			requireNoSessionWritten(t, env, gateCtx, account)
		})
	}
}
