package clientplayerloginlogic

// login 的三个 Redis 句柄各管各的键(docs/design/redis-player-store-cluster.md §5.1、§10.2):控制域、玩家域、登录域放到三个
// miniredis 上,从 HTTP 登录走到 EnterGame 入场链的会话落盘,核对每个键落在哪。玩家域与登录域用生产的构造器,
// 任何一条命令带着不带 tag 或跨槽的键,守卫直接报错,这条链走不通。本包用例改写 config.AppConfig,不能加 t.Parallel()。

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"login/internal/config"
	"login/internal/logic/pkg/auth"
	"login/internal/logic/pkg/ctxkeys"
	"login/internal/logic/pkg/sessionmanager"
	"login/internal/logic/pkg/token"
	"login/internal/svc"
	pbbase "proto/common/base"
	login_proto "proto/login"
	"shared/generated/pb/table"
	"shared/playerredis"
	"shared/redisslot"

	"github.com/alicebob/miniredis/v2"
	"github.com/panjf2000/ants/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestRedisDomainsKeepTheirOwnKeys(t *testing.T) {
	const account = "domain-wiring-account"
	previous := config.AppConfig
	t.Cleanup(func() { config.AppConfig = previous })
	config.AppConfig.LegacyGateLoginEnabled = true
	config.AppConfig.Locker.AccountLockTTL = 10
	config.AppConfig.Locker.PlayerLockTTL = 120
	config.AppConfig.Account.MaxDevicesPerAccount = 3
	config.AppConfig.Node.SessionExpireMin = 30
	config.AppConfig.HomeZone.RefreshRoleListDisabled = true

	controlRedis, playerRedis, loginRedis := miniredis.RunT(t), miniredis.RunT(t), miniredis.RunT(t)
	control := redis.NewClient(&redis.Options{Addr: controlRedis.Addr()})
	t.Cleanup(func() { _ = control.Close() })
	newDomain := func(mr *miniredis.Miniredis) redis.UniversalClient {
		client, err := playerredis.New(playerredis.Config{Addrs: []string{mr.Addr()}})
		require.NoError(t, err)
		t.Cleanup(func() { _ = client.Close() })
		return client
	}
	player, login := newDomain(playerRedis), newDomain(loginRedis)

	pool, err := ants.NewPool(1)
	require.NoError(t, err)
	t.Cleanup(pool.Release)
	locator := &enterWiringLocator{}
	directory := fixtureDirectory(t, control)
	directory.seed(account, &pbbase.AccountSimplePlayer{PlayerId: identityPlayerID, ClassId: 3, Name: "云中君"})
	svcCtx := &svc.ServiceContext{
		RedisClient: control, PlayerRedis: player, LoginRedis: login, AccountDirectory: directory,
		TokenManager:        token.NewManager(login, token.Config{AccessTokenTTL: time.Hour, RefreshTokenTTL: 24 * time.Hour}),
		PlayerLocatorClient: locator,
		PreloadPool:         pool,
	}
	authType := fmt.Sprintf("domain-wiring-%d", accountPersistenceAuthSequence.Add(1))
	auth.Register(authType, accountPersistenceAuth{account: account})

	// HTTP 入口签令牌。gate 上这里用凭据登录,只为驱动登录提交,access_token 绑定走的是同一条写路径。
	httpResp, err := NewLoginLogic(context.Background(), svcCtx).Login(&login_proto.LoginRequest{AuthType: authType, AuthToken: "code"})
	require.NoError(t, err)
	require.Zero(t, httpResp.GetErrorMessage().GetId())

	details := &pbbase.SessionDetails{SessionId: 7400, GateInstanceId: "gate-domain-wiring", SessionIncarnation: 1}
	gateCtx := ctxkeys.WithSessionDetails(context.Background(), details)
	gateResp, err := NewLoginLogic(gateCtx, svcCtx).Login(&login_proto.LoginRequest{AuthType: authType, AuthToken: "code"})
	require.NoError(t, err)
	require.Zero(t, gateResp.GetErrorMessage().GetId())

	enterResp, err := NewEnterGameLogic(gateCtx, svcCtx).EnterGame(&login_proto.EnterGameRequest{PlayerId: identityPlayerID})
	require.NoError(t, err)
	require.Nil(t, enterResp.GetErrorMessage(), "入场链应当已受理")
	require.NoError(t, pool.ReleaseTimeout(5*time.Second))
	require.EqualValues(t, 1, locator.setSessionCalls.Load(), "链要走到会话落盘,登录代次与玩家锁才都走过")

	// 控制域:账号锁全部释放后什么都不剩。
	require.Empty(t, controlRedis.Keys(), "登录态、令牌、玩家键都不在控制域")

	// 玩家域:只有登录代次留下(玩家锁已释放,幂等键只在进场成功后写)。
	require.Equal(t, []string{sessionmanager.LoginGenerationKey(identityPlayerID)}, playerRedis.Keys())

	// 登录域:连接键与账号键都在,每个键都带 tag,账号键全部同槽。
	connection := redisslot.LoginConnectionID(details.GetGateInstanceId(), details.GetSessionIncarnation())
	accountSlot := redisslot.HashSlot("{" + redisslot.AccountTag(account) + "}")
	seen := map[string]bool{}
	for _, key := range loginRedis.Keys() {
		require.True(t, redisslot.HasHashTag(key), "登录域的键都带 hash tag: %s", key)
		family, _, _ := strings.Cut(key, "{")
		seen[family] = true
		switch family {
		case "login_session:", "login:sessfence:":
			require.Equal(t, redisslot.CRC16(connection)&(redisslot.SlotCount-1), redisslot.HashSlot(key), key)
		case "login_account_sessions:", "account_refresh:", "access_token:", "refresh_token:", "refresh_family:":
			require.Equal(t, accountSlot, redisslot.HashSlot(key), key)
		default:
			t.Fatalf("登录域出现预期之外的键: %s", key)
		}
	}
	for _, family := range []string{"login_session:", "login:sessfence:", "login_account_sessions:", "account_refresh:", "access_token:", "refresh_token:", "refresh_family:"} {
		require.True(t, seen[family], "登录域缺 %s 这族键: %v", family, loginRedis.Keys())
	}
}

// 账号锁在登录域:ServiceContext 只给登录域句柄,登录域里被占着就回 kLoginInProgress;换到别的句柄这里会空指针。
func TestAccountLockLivesOnTheLoginDomain(t *testing.T) {
	const account = "domain-lock-account"
	previous := config.AppConfig
	t.Cleanup(func() { config.AppConfig = previous })
	config.AppConfig.Locker.AccountLockTTL = 10
	config.AppConfig.HomeZone.RefreshRoleListDisabled = true

	loginRedis := miniredis.RunT(t)
	login := redis.NewClient(&redis.Options{Addr: loginRedis.Addr()})
	t.Cleanup(func() { _ = login.Close() })
	authType := fmt.Sprintf("domain-lock-%d", accountPersistenceAuthSequence.Add(1))
	auth.Register(authType, accountPersistenceAuth{account: account})
	require.NoError(t, loginRedis.Set(accountLoginLockKey(account), "held-by-another-login"))

	resp, err := NewLoginLogic(context.Background(), &svc.ServiceContext{LoginRedis: login}).Login(&login_proto.LoginRequest{AuthType: authType, AuthToken: "code"})

	require.NoError(t, err)
	require.Equal(t, uint32(table.LoginError_kLoginInProgress), resp.GetErrorMessage().GetId())
}
