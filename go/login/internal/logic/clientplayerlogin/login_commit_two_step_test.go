package clientplayerloginlogic

// 登录提交两步之间的结局(docs/design/redis-player-store-cluster.md §10.2)。登录态按连接分布、设备集按账号分布,不同槽,
// 进不了同一段脚本:第 1 步连接没关才写登录态,第 2 步登记设备集。这里钉住两步之间每种结局都能自愈。
// 句柄用生产的构造器(守卫生效),键形或同槽出问题这些用例直接报错。本包用例改写 config.AppConfig,不能加 t.Parallel()。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"login/internal/config"
	"login/internal/logic/pkg/auth"
	"login/internal/logic/pkg/ctxkeys"
	"login/internal/logic/pkg/loginsession"
	"login/internal/logic/pkg/token"
	"login/internal/svc"
	pbbase "proto/common/base"
	login_proto "proto/login"
	"shared/generated/pb/table"
	"shared/playerredis"
	"shared/redisslot"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

const loginCommitAccount = "login-commit-account"

// beforeDeviceSetHook 在设备集那条管道(SADD + PEXPIRE)发出之前调用 before,before 报错时这条管道不发。
type beforeDeviceSetHook struct {
	before func() error
}

func (h *beforeDeviceSetHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *beforeDeviceSetHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook { return next }

func (h *beforeDeviceSetHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			if strings.EqualFold(cmd.Name(), "sadd") && h.before != nil {
				if err := h.before(); err != nil {
					return err
				}
				break
			}
		}
		return next(ctx, cmds)
	}
}

type loginCommitEnv struct {
	mr       *miniredis.Miniredis
	svcCtx   *svc.ServiceContext
	authType string
	hook     *beforeDeviceSetHook
}

func newLoginCommitEnv(t *testing.T) *loginCommitEnv {
	t.Helper()
	previous := config.AppConfig
	t.Cleanup(func() { config.AppConfig = previous })
	config.AppConfig.LegacyGateLoginEnabled = true
	config.AppConfig.Locker.AccountLockTTL = 10
	config.AppConfig.Account.MaxDevicesPerAccount = 3
	config.AppConfig.Node.SessionExpireMin = 30
	config.AppConfig.HomeZone.RefreshRoleListDisabled = true

	mr := miniredis.RunT(t)
	control := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = control.Close() })
	login, err := playerredis.New(playerredis.Config{Addrs: []string{mr.Addr()}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = login.Close() })
	hook := &beforeDeviceSetHook{}
	login.AddHook(hook)

	authType := fmt.Sprintf("login-commit-%d", accountPersistenceAuthSequence.Add(1))
	auth.Register(authType, accountPersistenceAuth{account: loginCommitAccount})
	return &loginCommitEnv{
		mr:       mr,
		authType: authType,
		hook:     hook,
		svcCtx: &svc.ServiceContext{
			RedisClient: control, LoginRedis: login, AccountDirectory: fixtureDirectory(t, control),
			TokenManager: token.NewManager(login, token.Config{AccessTokenTTL: time.Hour, RefreshTokenTTL: 24 * time.Hour}),
		},
	}
}

func loginCommitDetails(incarnation uint64) *pbbase.SessionDetails {
	return &pbbase.SessionDetails{SessionId: 7300 + uint32(incarnation), GateInstanceId: "gate-login-commit", SessionIncarnation: incarnation}
}

func (e *loginCommitEnv) login(t *testing.T, details *pbbase.SessionDetails) *login_proto.LoginResponse {
	t.Helper()
	ctx := ctxkeys.WithSessionDetails(context.Background(), details)
	resp, err := NewLoginLogic(ctx, e.svcCtx).Login(&login_proto.LoginRequest{AuthType: e.authType, AuthToken: "code"})
	require.NoError(t, err)
	return resp
}

func (e *loginCommitEnv) disconnect(t *testing.T, details *pbbase.SessionDetails) {
	t.Helper()
	ctx := ctxkeys.WithSessionDetails(context.Background(), details)
	_, err := NewDisconnectLogic(ctx, e.svcCtx).Disconnect(&login_proto.LoginNodeDisconnectRequest{SessionId: details.GetSessionId()})
	require.NoError(t, err)
}

func (e *loginCommitEnv) deviceMembers(t *testing.T) []string {
	t.Helper()
	deviceSet := redisslot.AccountDeviceSetKey(loginCommitAccount)
	if !e.mr.Exists(deviceSet) {
		return nil
	}
	members, err := e.mr.Members(deviceSet)
	require.NoError(t, err)
	return members
}

func loginCommitRef(t *testing.T, details *pbbase.SessionDetails) loginsession.Ref {
	t.Helper()
	ref, ok := loginsession.RefOf(details)
	require.True(t, ok)
	return ref
}

func TestLoginCommitWritesSessionThenDeviceSet(t *testing.T) {
	e := newLoginCommitEnv(t)
	details := loginCommitDetails(1)
	ref := loginCommitRef(t, details)

	resp := e.login(t, details)

	require.Zero(t, resp.GetErrorMessage().GetId())
	stored, err := e.mr.Get(ref.Key())
	require.NoError(t, err)
	require.Equal(t, loginCommitAccount, stored)
	require.Equal(t, 30*time.Minute, e.mr.TTL(ref.Key()))
	require.Equal(t, []string{ref.DeviceMember()}, e.deviceMembers(t))
	require.Equal(t, 30*time.Minute, e.mr.TTL(redisslot.AccountDeviceSetKey(loginCommitAccount)))
}

// 第 2 步失败:这次登录按 Redis 故障回失败,不签令牌。已写下的登录态没有人清理,随 TTL 过期。
func TestLoginDeviceSetFailureFailsTheLoginAndSessionExpires(t *testing.T) {
	e := newLoginCommitEnv(t)
	e.hook.before = func() error { return errors.New("injected device set failure") }
	details := loginCommitDetails(1)
	ref := loginCommitRef(t, details)

	resp := e.login(t, details)

	require.Equal(t, uint32(table.LoginError_kLoginRedisSetFailed), resp.GetErrorMessage().GetId())
	require.Empty(t, resp.GetAccessToken(), "第 2 步失败的登录不签令牌")
	require.Empty(t, resp.GetPlayers())
	require.Empty(t, e.deviceMembers(t))
	require.True(t, e.mr.Exists(ref.Key()), "第 1 步已经写下的登录态留到过期")
	e.mr.FastForward(30*time.Minute + time.Second)
	require.False(t, e.mr.Exists(ref.Key()), "登录态随 TTL 过期,不会永久留下")
}

// Disconnect 抢在第 2 步之前:Cleanup 那时还删不到设备集成员,第 2 步写下的就成了陈旧成员。
// 下一次同账号登录的 PruneStaleFromDeviceSet 发现它的登录态不存在,把它剔除。
func TestDisconnectBeforeDeviceSetLeavesStaleMemberAndNextLoginHeals(t *testing.T) {
	e := newLoginCommitEnv(t)
	first := loginCommitDetails(1)
	firstRef := loginCommitRef(t, first)
	raced := false
	e.hook.before = func() error {
		raced = true
		e.hook.before = nil
		e.disconnect(t, first)
		return nil
	}

	e.login(t, first)

	require.True(t, raced, "夹具必须让 Disconnect 插在两步之间")
	require.False(t, e.mr.Exists(firstRef.Key()), "Disconnect 已经清掉登录态")
	require.Equal(t, []string{firstRef.DeviceMember()}, e.deviceMembers(t), "第 2 步留下一个陈旧成员")

	second := loginCommitDetails(2)
	secondRef := loginCommitRef(t, second)
	resp := e.login(t, second)

	require.Zero(t, resp.GetErrorMessage().GetId())
	require.Equal(t, []string{secondRef.DeviceMember()}, e.deviceMembers(t), "下一次登录剔除陈旧成员")
}

// Disconnect 在第 1 步之前:栅栏已关,脚本什么都不写,第 2 步也不走。
func TestLoginAfterDisconnectWritesNothing(t *testing.T) {
	e := newLoginCommitEnv(t)
	details := loginCommitDetails(1)
	e.disconnect(t, details)

	resp := e.login(t, details)

	require.Equal(t, uint32(table.LoginError_kLoginSessionNotFound), resp.GetErrorMessage().GetId())
	require.False(t, e.mr.Exists(loginCommitRef(t, details).Key()))
	require.Empty(t, e.deviceMembers(t))
}

// 设备数上限只数活连接:陈旧成员先被剔除,不占名额。
func TestDeviceLimitIgnoresStaleMembers(t *testing.T) {
	e := newLoginCommitEnv(t)
	for i := uint64(1); i <= 3; i++ {
		require.Zero(t, e.login(t, loginCommitDetails(i)).GetErrorMessage().GetId())
	}
	require.NoError(t, e.svcCtx.LoginRedis.Del(context.Background(), loginCommitRef(t, loginCommitDetails(1)).Key()).Err())

	require.Zero(t, e.login(t, loginCommitDetails(4)).GetErrorMessage().GetId(), "一个成员已陈旧,第 4 条连接不超限")
	require.Len(t, e.deviceMembers(t), 3)

	require.Equal(t, uint32(table.LoginError_kTooManyDevices), e.login(t, loginCommitDetails(5)).GetErrorMessage().GetId(), "3 条活连接之外再来一条超限")
}
