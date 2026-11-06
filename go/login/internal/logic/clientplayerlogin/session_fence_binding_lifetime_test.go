package clientplayerloginlogic

// 会话栅栏的绑定寿命接进 EnterGame / Disconnect 之后的回归(docs/design/data-overwrite-rollback-audit-20261018.md
// §4 login 第 2 条,docs/design/client-request-ordering.md §6.2「TTL」)。栅栏本身的寿命规则在 pkg/sessionfence 里测;
// 这里测的是两处后果:
//   - 绑定之后过了在途操作的 ttl(原先 6 分钟)、登录态还在,同一连接 EnterGame 另一个角色必须在入口被拒,
//     原先记录随 ttl 过期,第二个角色照常受理,一条连接绑上两个角色;
//   - 随后断线,gate 会话头带 player_id = 0(BindSession 还没到 gate)时也要从栅栏读到绑定的角色并挂断线租约,
//     原先读不到,玩家永久停在 ONLINE。

import (
	"context"
	"testing"
	"time"

	"login/internal/config"
	"login/internal/logic/pkg/loginsession"
	"login/internal/logic/pkg/sessionfence"
	"login/internal/svc"

	login_proto "proto/login"
	"shared/generated/pb/table"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

const fenceLifetimeAccount = "acct-fence-lifetime"

func TestEnterGame_SecondRoleRejectedAfterFenceOperationTTL(t *testing.T) {
	previous := config.AppConfig
	t.Cleanup(func() { config.AppConfig = previous })
	config.AppConfig.Node.SessionExpireMin = 30
	config.AppConfig.Locker.PlayerLockTTL = 120

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	locator := &fenceLocator{}
	svcCtx := &svc.ServiceContext{
		RedisClient: rdb, PlayerRedis: rdb, LoginRedis: rdb,
		AccountDirectory: fixtureDirectory(t, rdb), PlayerLocatorClient: locator,
	}
	ctx := fenceWiringCtx(0)

	// 这条连接 Login 过(登录态 30 分钟),第一次 EnterGame 的后台链已在栅栏上绑定 fenceWiringPlayerID。
	require.NoError(t, sessionfence.CommitLogin(ctx, rdb, fenceWiringKey, fenceLifetimeAccount, loginsession.TTL()))
	_, gen, err := sessionfence.CheckEnterGame(ctx, rdb, fenceWiringKey, fenceWiringPlayerID, sessionFenceTTL)
	require.NoError(t, err)
	outcome, err := sessionfence.BindPlayer(ctx, rdb, fenceWiringKey, fenceWiringPlayerID, gen)
	require.NoError(t, err)
	require.Equal(t, sessionfence.OutcomeOK, outcome)

	// 过了在途操作的 ttl,没有 LeaveGame / Disconnect;登录态仍在。
	mr.FastForward(sessionFenceTTL + time.Minute)

	resp, err := NewEnterGameLogic(ctx, svcCtx).EnterGame(&login_proto.EnterGameRequest{PlayerId: fenceWiringPlayerID + 1})
	require.NoError(t, err)
	require.Equal(t, uint32(table.LoginError_kLoginInProgress), resp.GetErrorMessage().GetId(),
		"同一连接已绑定角色,换绑第二个角色必须在入口被拒")

	_, err = NewDisconnectLogic(ctx, svcCtx).Disconnect(&login_proto.LoginNodeDisconnectRequest{SessionId: fenceWiringSessionID})
	require.NoError(t, err)
	require.Equal(t, []uint64{fenceWiringPlayerID}, locator.disconnectingPlayers(),
		"会话头不带玩家时,断线要从栅栏读到绑定的角色挂断线租约")
}
