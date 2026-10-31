package clientplayerloginlogic

// Login 的失败语义回归(走真实 Login(),miniredis 承载 Redis)。
//
// 修复前三个问题:
//   - 认证失败一律回 2000(非故障码):微信 / QQ 不可用、AppSecret 失效与「code 无效」混在一起,in-band 告警看不到渠道故障;
//   - 账号目录读失败直接 return err:gRPC 变 UNKNOWN,Java 网关映射成 401,玩家被要求重走 OAuth;
//   - HTTP 路径 token 签发失败仍回成功、不带 token:第三方 code 已被消费,客户端拿空 token 卡在 gate 绑定那一步。
//
// 本包用例改写 config.AppConfig,不能加 t.Parallel()。

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"login/internal/config"
	"login/internal/constants"
	"login/internal/logic/pkg/auth"
	"login/internal/logic/pkg/token"
	"login/internal/svc"
	loginpb "proto/login"
	"shared/generated/pb/table"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// auth 注册表是进程级的、重名即 panic、没有注销接口:每个用例注册一个独立名字。
var semanticsAuthSequence atomic.Uint64

type semanticsProvider struct {
	account string
	err     error
}

func (p semanticsProvider) Validate(context.Context, string) (*auth.AuthResult, error) {
	if p.err != nil {
		return nil, p.err
	}
	return &auth.AuthResult{Account: p.account}, nil
}

type semanticsEnv struct {
	mr       *miniredis.Miniredis
	svcCtx   *svc.ServiceContext
	authType string
}

// 只在令牌存储管线处注入故障,避免破坏 Redis 握手与事务协议而让用例卡在断言之前。
type semanticsTokenStoreFailure struct{}

func (semanticsTokenStoreFailure) DialHook(next redis.DialHook) redis.DialHook { return next }

func (semanticsTokenStoreFailure) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return next
}

func (semanticsTokenStoreFailure) ProcessPipelineHook(redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(context.Context, []redis.Cmder) error {
		return errors.New("injected token store failure")
	}
}

// newSemanticsEnv 起一个只有 HTTP 新路径会用到的 ServiceContext。tokenRedisFails=true 时 token 管理器
// 用独立 Redis 客户端在存储管线入口注入错误,模拟「账号目录可读、token 写不进去」。
func newSemanticsEnv(t *testing.T, provider semanticsProvider, tokenRedisFails bool) *semanticsEnv {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	previousConfig := config.AppConfig
	t.Cleanup(func() { config.AppConfig = previousConfig })
	config.AppConfig.Locker.AccountLockTTL = 10
	config.AppConfig.HomeZone.RefreshRoleListDisabled = true

	tokenRdb := rdb
	if tokenRedisFails {
		tokenRdb = redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
		tokenRdb.AddHook(semanticsTokenStoreFailure{})
		t.Cleanup(func() { _ = tokenRdb.Close() })
	}
	tokens := token.NewManager(tokenRdb, token.Config{AccessTokenTTL: time.Hour, RefreshTokenTTL: 24 * time.Hour})

	authType := fmt.Sprintf("semantics-provider-%d", semanticsAuthSequence.Add(1))
	auth.Register(authType, provider)
	return &semanticsEnv{
		mr:       mr,
		svcCtx:   &svc.ServiceContext{RedisClient: rdb, LoginRedis: rdb, AccountDirectory: fixtureDirectory(t, rdb), TokenManager: tokens},
		authType: authType,
	}
}

// login 走 HTTP 新路径(上下文不带会话 = Java Gateway POST /api/login 直调)。
func (e *semanticsEnv) login(t *testing.T) *loginpb.LoginResponse {
	t.Helper()
	resp, err := NewLoginLogic(context.Background(), e.svcCtx).Login(&loginpb.LoginRequest{AuthType: e.authType, AuthToken: "code"})
	require.NoError(t, err, "失败必须走 in-band 码,不能变成 gRPC 错误(网关会把 UNKNOWN 映射成 401)")
	require.NotNil(t, resp)
	return resp
}

func requireLoginFailureTip(t *testing.T, resp *loginpb.LoginResponse, want table.LoginError) {
	t.Helper()
	require.NotNil(t, resp.ErrorMessage, "应带错误码")
	require.Equal(t, uint32(want), resp.ErrorMessage.Id)
	require.Empty(t, resp.AccessToken, "失败响应不得带 token")
	require.Empty(t, resp.RefreshToken, "失败响应不得带 token")
}

func TestLoginProviderUnavailableReturnsFaultTip(t *testing.T) {
	env := newSemanticsEnv(t, semanticsProvider{err: fmt.Errorf("wechat: http status 502: %w", auth.ErrProviderUnavailable)}, false)
	resp := env.login(t)
	requireLoginFailureTip(t, resp, table.LoginError_kLoginAuthProviderUnavailable)
	require.False(t, env.mr.Exists(constants.GetAccountDataKey("wx_any")), "认证失败不得初始化账号目录")
}

func TestLoginInvalidThirdPartyCredentialStillReturnsAccountNotFound(t *testing.T) {
	env := newSemanticsEnv(t, semanticsProvider{err: errors.New("wechat: errcode=40029")}, false)
	requireLoginFailureTip(t, env.login(t), table.LoginError_kLoginAccountNotFound)
}

func TestLoginAccountDirectorySQLFailureIsInBandFault(t *testing.T) {
	const account = "wx_redis_failure"
	env := newSemanticsEnv(t, semanticsProvider{account: account}, false)
	// 键类型不对:脚本里的 GET 报 WRONGTYPE,等价于账号目录读不出来的 Redis 故障,而锁键不受影响。
	env.svcCtx.AccountDirectory.(*testDirectory).readErr = errors.New("SQL目录不可用")
	requireLoginFailureTip(t, env.login(t), table.LoginError_kLoginDataSerializeFailed)
}

func TestLoginCorruptAccountDirectoryIsInBandFault(t *testing.T) {
	const account = "wx_corrupt_directory"
	env := newSemanticsEnv(t, semanticsProvider{account: account}, false)
	env.svcCtx.AccountDirectory.(*testDirectory).readErr = errAccountDirectoryCorrupt
	requireLoginFailureTip(t, env.login(t), table.LoginError_kLoginDataParseFailed)
}

func TestLoginHTTPTokenIssueFailureReturnsFaultTip(t *testing.T) {
	const account = "wx_token_failure"
	env := newSemanticsEnv(t, semanticsProvider{account: account}, true)
	resp := env.login(t)
	requireLoginFailureTip(t, resp, table.LoginError_kLoginTokenIssueFailed)
	require.Empty(t, resp.Players, "签发失败不得返回角色列表冒充成功")
}

func TestLoginHTTPSuccessStillIssuesTokens(t *testing.T) {
	const account = "wx_success"
	env := newSemanticsEnv(t, semanticsProvider{account: account}, false)
	resp := env.login(t)
	require.Nil(t, resp.ErrorMessage)
	require.NotEmpty(t, resp.AccessToken)
	require.NotEmpty(t, resp.RefreshToken)
	_, err := env.svcCtx.AccountDirectory.Read(context.Background(), account, false)
	require.NoError(t, err, "成功登录应初始化SQL目录")
}

func TestAuthFailureTip(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want table.LoginError
	}{
		{name: "provider unavailable", err: fmt.Errorf("qq: bad json: %w", auth.ErrProviderUnavailable), want: table.LoginError_kLoginAuthProviderUnavailable},
		{name: "wrapped twice", err: fmt.Errorf("outer: %w", fmt.Errorf("wechat: http error: %w: %w", context.DeadlineExceeded, auth.ErrProviderUnavailable)), want: table.LoginError_kLoginAuthProviderUnavailable},
		{name: "invalid credentials", err: auth.ErrInvalidCredentials, want: table.LoginError_kLoginAccountNotFound},
		{name: "password disabled", err: auth.ErrPasswordAuthDisabled, want: table.LoginError_kLoginAccountNotFound},
		{name: "unknown auth type", err: errors.New("unknown auth type: foo"), want: table.LoginError_kLoginAccountNotFound},
	}
	for _, tc := range cases {
		if got := authFailureTip(tc.err); got != uint32(tc.want) {
			t.Errorf("%s: authFailureTip = %d, want %d", tc.name, got, tc.want)
		}
	}
}
