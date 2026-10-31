package clientplayerloginlogic

// 跨账号登录态继承的回归测试(docs/design/data-overwrite-rollback-audit-20261018.md §2.3 login-session-route#1;
// client-request-ordering.md §7.2 第 2 条)。
//
// 场景:gate 被 kill -9 后以同一 node_id 重启,session 序号从 0 开始,新连接拿到与旧进程某条已登录连接相同的
// session_id;旧连接的 login_session 在 30min TTL 内仍在。修复前,新连接跳过 Login 直接发 CreatePlayer / EnterGame,
// login 按 session_id 读到旧连接的账号,于是能在别人的账号下建角、进别人的角色。
// 修复后登录态按 (gate_instance_id, session_incarnation) 建键,撞号的新连接读不到任何账号。

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"login/internal/config"
	"login/internal/constants"
	"login/internal/logic/pkg/auth"
	"login/internal/logic/pkg/ctxkeys"
	"login/internal/logic/pkg/loginsession"
	"login/internal/logic/pkg/sessionfence"
	"login/internal/logic/pkg/token"
	"login/internal/svc"
	pbbase "proto/common/base"
	dbpb "proto/common/database"
	login_proto "proto/login"
	"shared/generated/pb/table"
	"shared/redisslot"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

const (
	identitySessionID     = uint32(3<<17 | 5) // node_id = 3、序号 5:重启前后的两个 gate 进程都会发出这个号
	identityVictimAccount = "identity_victim"
	identityVictimPlayer  = uint64(990001)
	identityOwnAccount    = "identity_attacker"
)

// 重启前 gate 进程上、已登录 victim 的连接。
func identityOldGateDetails() *pbbase.SessionDetails {
	return &pbbase.SessionDetails{SessionId: identitySessionID, GateInstanceId: "gate-before-restart", SessionIncarnation: 9}
}

// 重启后同一 node_id 的新 gate 进程上、与旧连接同号的新连接。
func identityRestartedGateDetails() *pbbase.SessionDetails {
	return &pbbase.SessionDetails{SessionId: identitySessionID, GateInstanceId: "gate-after-restart", SessionIncarnation: 1}
}

type identityHarness struct {
	rdb    *redis.Client
	mr     *miniredis.Miniredis
	svcCtx *svc.ServiceContext
}

// newIdentityHarness 种下重启前那条连接的全部残留:victim 的登录态与设备集成员、victim 的账号 blob 与角色反查索引。
func newIdentityHarness(t *testing.T) *identityHarness {
	t.Helper()
	previous := config.AppConfig
	t.Cleanup(func() { config.AppConfig = previous })
	config.AppConfig.LegacyGateLoginEnabled = true
	config.AppConfig.Node.SessionExpireMin = 30
	config.AppConfig.Locker.AccountLockTTL = 10
	config.AppConfig.Locker.PlayerLockTTL = 120
	config.AppConfig.Account.MaxDevicesPerAccount = 3
	config.AppConfig.Account.MaxPlayersPerAccount = 5
	config.AppConfig.Account.CacheExpire = time.Hour
	config.AppConfig.HomeZone.RefreshRoleListDisabled = true

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()

	victim := identityRef(t, identityOldGateDetails())
	require.NoError(t, sessionfence.CommitLogin(ctx, rdb, victim.Fence(), identityVictimAccount, loginsession.TTL()))
	require.NoError(t, loginsession.RegisterDevice(ctx, rdb, identityVictimAccount, victim, loginsession.TTL()))

	blob, err := proto.Marshal(&dbpb.UserAccounts{SimplePlayers: &pbbase.AccountSimplePlayerList{
		Players: []*pbbase.AccountSimplePlayer{{PlayerId: identityVictimPlayer, Name: "受害者"}},
	}})
	require.NoError(t, err)
	require.NoError(t, rdb.Set(ctx, constants.GetAccountDataKey(identityVictimAccount), blob, 0).Err())
	require.NoError(t, rdb.Set(ctx, constants.PlayerToAccountKey(identityVictimPlayer), identityVictimAccount, 0).Err())

	return &identityHarness{rdb: rdb, mr: mr, svcCtx: &svc.ServiceContext{
		RedisClient: rdb, PlayerRedis: rdb, LoginRedis: rdb, AccountDirectory: fixtureDirectory(t, rdb),
		TokenManager: token.NewManager(rdb, token.Config{AccessTokenTTL: time.Hour, RefreshTokenTTL: 30 * 24 * time.Hour}),
	}}
}

func identityRef(t *testing.T, details *pbbase.SessionDetails) loginsession.Ref {
	t.Helper()
	ref, ok := loginsession.RefOf(details)
	require.True(t, ok, "夹具的连接身份必须完整")
	return ref
}

func (h *identityHarness) victimBlob(t *testing.T) []byte {
	t.Helper()
	raw, err := h.rdb.Get(context.Background(), constants.GetAccountDataKey(identityVictimAccount)).Bytes()
	require.NoError(t, err)
	return raw
}

func TestCreatePlayerAfterGateRestartWithoutLoginIsRejected(t *testing.T) {
	h := newIdentityHarness(t)
	before := h.victimBlob(t)
	ctx := ctxkeys.WithSessionDetails(context.Background(), identityRestartedGateDetails())

	resp, err := NewCreatePlayerLogic(ctx, h.svcCtx).CreatePlayer(&login_proto.CreatePlayerRequest{})

	require.NoError(t, err)
	require.Equal(t, uint32(table.LoginError_kLoginSessionNotFound), resp.GetErrorMessage().GetId(),
		"撞号的新连接没有 Login 过,不能在 victim 的账号下建角")
	require.Equal(t, before, h.victimBlob(t), "victim 的账号 blob 不得被改动")
	require.False(t, h.mr.Exists(redisslot.AccountCreateLockKey(identityVictimAccount)), "拒绝发生在拿建角锁之前")
}

func TestEnterGameAfterGateRestartWithoutLoginIsRejected(t *testing.T) {
	h := newIdentityHarness(t)
	ctx := ctxkeys.WithSessionDetails(context.Background(), identityRestartedGateDetails())

	resp, err := NewEnterGameLogic(ctx, h.svcCtx).EnterGame(&login_proto.EnterGameRequest{PlayerId: identityVictimPlayer})

	require.NoError(t, err)
	require.Equal(t, uint32(table.LoginError_kLoginSessionNotFound), resp.GetErrorMessage().GetId(),
		"撞号的新连接没有 Login 过,不能进 victim 的角色")
	require.False(t, h.mr.Exists(fmt.Sprintf("player_locker:{%d}", identityVictimPlayer)), "拒绝发生在拿玩家锁之前")
	newFence, _ := sessionfence.KeyOf(identityRestartedGateDetails())
	require.False(t, h.mr.Exists(redisslot.SessionFenceKey(newFence.String())), "拒绝发生在建会话栅栏记录之前,不留残留")
}

func TestLoginOnRestartedGateBindsOwnAccountOnly(t *testing.T) {
	h := newIdentityHarness(t)
	ctx := ctxkeys.WithSessionDetails(context.Background(), identityRestartedGateDetails())
	authType := fmt.Sprintf("identity-login-%d", accountPersistenceAuthSequence.Add(1))
	auth.Register(authType, accountPersistenceAuth{account: identityOwnAccount})

	loginResp, err := NewLoginLogic(ctx, h.svcCtx).Login(&login_proto.LoginRequest{AuthType: authType})
	require.NoError(t, err)
	require.Zero(t, loginResp.GetErrorMessage().GetId(), "正常 Login 不受影响")

	account, err := loginsession.GetAccount(ctx, h.rdb, identityRef(t, identityRestartedGateDetails()))
	require.NoError(t, err)
	require.Equal(t, identityOwnAccount, account, "本连接的登录态是自己的账号")
	victimAccount, err := loginsession.GetAccount(ctx, h.rdb, identityRef(t, identityOldGateDetails()))
	require.NoError(t, err)
	require.Equal(t, identityVictimAccount, victimAccount, "另一条连接的登录态不受影响")

	// 登录之后会话检查放行,但读到的是自己的账号:victim 的角色不在其中、反查索引也不指向自己 → 不能进。
	enterResp, err := NewEnterGameLogic(ctx, h.svcCtx).EnterGame(&login_proto.EnterGameRequest{PlayerId: identityVictimPlayer})
	require.NoError(t, err)
	require.Equal(t, uint32(table.LoginError_kLoginEnterGameGuid), enterResp.GetErrorMessage().GetId())
}

// identityCountingAuth 记下认证器被调用的次数:第三方 code 是一次性的,被拒的连接不能消费它。
type identityCountingAuth struct {
	account string
	calls   atomic.Int32
}

func (a *identityCountingAuth) Validate(context.Context, string) (*auth.AuthResult, error) {
	a.calls.Add(1)
	return &auth.AuthResult{Account: a.account}, nil
}

// 没有完整连接身份(实例号 + 会话代次)的连接一律拒绝,回 kLoginSessionIdNotFound,什么都不写。
// 按 session_id 建键的旧路径已删除:它正是撞号继承登录态的来源。
func TestConnectionWithoutIdentityIsRejected(t *testing.T) {
	for name, details := range map[string]*pbbase.SessionDetails{
		"no incarnation":   {SessionId: identitySessionID + 1, GateInstanceId: "gate-x"},
		"no gate instance": {SessionId: identitySessionID + 1, SessionIncarnation: 3},
		"session id only":  {SessionId: identitySessionID + 1},
	} {
		t.Run(name, func(t *testing.T) {
			h := newIdentityHarness(t)
			before := h.mr.Keys()
			ctx := ctxkeys.WithSessionDetails(context.Background(), details)
			provider := &identityCountingAuth{account: identityOwnAccount}
			authType := fmt.Sprintf("identity-rejected-%d", accountPersistenceAuthSequence.Add(1))
			auth.Register(authType, provider)
			wantTip := uint32(table.LoginError_kLoginSessionIdNotFound)

			loginResp, err := NewLoginLogic(ctx, h.svcCtx).Login(&login_proto.LoginRequest{AuthType: authType})
			require.NoError(t, err)
			require.Equal(t, wantTip, loginResp.GetErrorMessage().GetId())
			require.Zero(t, provider.calls.Load(), "拒绝发生在认证之前,第三方 code 不被消费")
			require.Empty(t, loginResp.GetAccessToken())

			createResp, err := NewCreatePlayerLogic(ctx, h.svcCtx).CreatePlayer(&login_proto.CreatePlayerRequest{})
			require.NoError(t, err)
			require.Equal(t, wantTip, createResp.GetErrorMessage().GetId())

			enterResp, err := NewEnterGameLogic(ctx, h.svcCtx).EnterGame(&login_proto.EnterGameRequest{PlayerId: identityVictimPlayer})
			require.NoError(t, err)
			require.Equal(t, wantTip, enterResp.GetErrorMessage().GetId())

			// 断线与离开没有回包可带 tip,也没有状态要收尾。
			_, err = NewDisconnectLogic(ctx, h.svcCtx).Disconnect(&login_proto.LoginNodeDisconnectRequest{SessionId: identitySessionID + 1})
			require.NoError(t, err)
			_, err = NewLeaveGameLogic(ctx, h.svcCtx).LeaveGame(&login_proto.LeaveGameRequest{})
			require.NoError(t, err)

			require.Equal(t, before, h.mr.Keys(), "被拒的连接不得留下任何键")
		})
	}
}

func TestDisconnectOnRestartedGateLeavesOtherConnectionsAlone(t *testing.T) {
	// 撞号的新连接断开:只清本连接的登录态,不动重启前那条连接的登录态。
	h := newIdentityHarness(t)
	ctx := ctxkeys.WithSessionDetails(context.Background(), identityRestartedGateDetails())

	_, err := NewDisconnectLogic(ctx, h.svcCtx).Disconnect(&login_proto.LoginNodeDisconnectRequest{SessionId: identitySessionID})
	require.NoError(t, err)

	require.True(t, h.mr.Exists(identityRef(t, identityOldGateDetails()).Key()), "另一条连接的登录态不受影响")
}
