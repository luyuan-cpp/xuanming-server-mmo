package clientplayerloginlogic

// LeaveGame 在会话栅栏 Leave 报错时 fail-closed(F4,docs/design/client-request-ordering.md §6.2 第三种竞态):
// 回错误(路由服转成带请求 id 的 1003,客户端重发),不清登录态、不删玩家会话、不回成功;重发时照常收尾。
//
// 修复前:Leave 报错只记日志、把绑定的玩家当 0,继续清理并回成功 —— gen 没递增(在途的 EnterGame 后台链复查看不到
// 这次离开,会在离开之后把会话与位置写回去),绑定也没解除(入口续满一周寿命,残留绑定一直续命)。
// 下面第一例在修复前 LeaveGame 返回 nil 错误、并对会话头的玩家发出 MarkOffline;第二例在 Leave 脚本修复前,
// 重发时 Leave 返回 0,会话头又是 0,绑定的玩家没人收尾。

import (
	"context"
	"errors"
	"testing"

	"login/internal/logic/pkg/ctxkeys"
	"login/internal/logic/pkg/loginsession"
	"login/internal/logic/pkg/sessionfence"
	login_proto "proto/login"
	plpb "proto/player_locator"
	"shared/redisslot"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var errInjectedRedisTimeout = errors.New("injected: redis i/o timeout")

// fenceScriptFault 让下一条碰栅栏键的脚本命令(EVALSHA / EVAL)报错,只生效一次:
//   - executed = false:命令不发给 Redis,报错(确定没执行);
//   - executed = true:先照常执行再报错,模拟「脚本已执行、回包丢了」的读超时。NOSCRIPT 原样返回,
//     go-redis 随后改发 EVAL,仍由本钩子接住。
type fenceScriptFault struct {
	fenceKey string
	armed    bool
	executed bool
}

func (f *fenceScriptFault) DialHook(next redis.DialHook) redis.DialHook { return next }

func (f *fenceScriptFault) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if !f.armed || !f.matches(cmd) {
			return next(ctx, cmd)
		}
		if !f.executed {
			f.armed = false
			return errInjectedRedisTimeout
		}
		if err := next(ctx, cmd); err != nil {
			return err
		}
		f.armed = false
		return errInjectedRedisTimeout
	}
}

func (f *fenceScriptFault) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (f *fenceScriptFault) matches(cmd redis.Cmder) bool {
	if name := cmd.Name(); name != "evalsha" && name != "eval" {
		return false
	}
	for _, arg := range cmd.Args() {
		if s, ok := arg.(string); ok && s == f.fenceKey {
			return true
		}
	}
	return false
}

// countingLocator 在 fenceLocator 之上数 GetSession:deletePlayerSession(DeleteSession)的第一步就是它,
// 数到 0 才能说明收尾根本没跑(会话不存在时 MarkOffline 本来就不发,只看 MarkOffline 分不出来)。
type countingLocator struct {
	*fenceLocator
	getSessionCalls int
}

func (c *countingLocator) GetSession(ctx context.Context, in *plpb.GetSessionRequest, opts ...grpc.CallOption) (*plpb.GetSessionResponse, error) {
	c.getSessionCalls++
	return c.fenceLocator.GetSession(ctx, in, opts...)
}

func armFenceFault(rdb *redis.Client, executed bool) *fenceScriptFault {
	fault := &fenceScriptFault{fenceKey: redisslot.SessionFenceKey(fenceWiringKey.String()), armed: true, executed: executed}
	rdb.AddHook(fault)
	return fault
}

func loginSessionExists(t *testing.T, ctx context.Context, rdb *redis.Client) bool {
	t.Helper()
	details, ok := ctxkeys.GetSessionDetails(ctx)
	require.True(t, ok)
	ref, ok := loginsession.RefOf(details)
	require.True(t, ok)
	n, err := rdb.Exists(ctx, ref.Key()).Result()
	require.NoError(t, err)
	return n == 1
}

func TestLeaveGame_FenceLeaveErrorFailsWithoutCleanup(t *testing.T) {
	svcCtx, rdb, locator := newFenceWiring(t)
	counting := &countingLocator{fenceLocator: locator}
	svcCtx.PlayerLocatorClient = counting
	ctx := fenceWiringCtx(fenceWiringPlayerID)
	details, _ := ctxkeys.GetSessionDetails(ctx)
	seedLoginSession(t, ctx, rdb, details, "acct-leave-fence")
	_, gen, err := sessionfence.CheckEnterGame(ctx, rdb, fenceWiringKey, fenceWiringPlayerID, sessionFenceTTL)
	require.NoError(t, err)
	outcome, err := sessionfence.BindPlayer(ctx, rdb, fenceWiringKey, fenceWiringPlayerID, gen)
	require.NoError(t, err)
	require.Equal(t, sessionfence.OutcomeOK, outcome)
	locator.current = &plpb.PlayerSession{SessionId: fenceWiringSessionID, SessionVersion: 5}

	fault := armFenceFault(rdb, false)
	resp, err := NewLeaveGameLogic(ctx, svcCtx).LeaveGame(&login_proto.LeaveGameRequest{})
	require.False(t, fault.armed, "scenario never reached the fence leave")
	require.Error(t, err, "a leave the fence did not record must not be acknowledged")
	require.Equal(t, codes.Unavailable, status.Code(err))
	require.Nil(t, resp)
	require.Zero(t, counting.getSessionCalls, "deletePlayerSession must not run when the fence leave failed")
	require.Empty(t, locator.markedOfflinePlayers())
	require.True(t, loginSessionExists(t, ctx, rdb), "login session must be left for the retry")
	outcome, err = sessionfence.Recheck(ctx, rdb, fenceWiringKey, gen)
	require.NoError(t, err)
	require.Equal(t, sessionfence.OutcomeOK, outcome, "the failed leave wrote nothing to the fence")

	// 客户端重发,栅栏恢复:照常记下离开、清登录态、删会话。
	_, err = NewLeaveGameLogic(ctx, svcCtx).LeaveGame(&login_proto.LeaveGameRequest{})
	require.NoError(t, err)
	require.Equal(t, []uint64{fenceWiringPlayerID}, locator.markedOfflinePlayers())
	require.False(t, loginSessionExists(t, ctx, rdb))
	outcome, err = sessionfence.Recheck(ctx, rdb, fenceWiringKey, gen)
	require.NoError(t, err)
	require.Equal(t, sessionfence.OutcomeLeft, outcome)
}

// 脚本已执行、回包丢了:这次 LeaveGame 回错误;重发时 Leave 已无绑定可解除,仍必须返回第一次解除的玩家。
// 会话头带 player_id = 0(后台链已绑定、BindSession 还没到 gate),栅栏是这个玩家唯一的线索。
func TestLeaveGame_RetryAfterLostLeaveReplyStillSettlesBoundPlayer(t *testing.T) {
	svcCtx, rdb, locator := newFenceWiring(t)
	ctx := fenceWiringCtx(0)
	_, gen, err := sessionfence.CheckEnterGame(ctx, rdb, fenceWiringKey, fenceWiringPlayerID, sessionFenceTTL)
	require.NoError(t, err)
	outcome, err := sessionfence.BindPlayer(ctx, rdb, fenceWiringKey, fenceWiringPlayerID, gen)
	require.NoError(t, err)
	require.Equal(t, sessionfence.OutcomeOK, outcome)
	locator.current = &plpb.PlayerSession{SessionId: fenceWiringSessionID, SessionVersion: 5}

	fault := armFenceFault(rdb, true)
	_, err = NewLeaveGameLogic(ctx, svcCtx).LeaveGame(&login_proto.LeaveGameRequest{})
	require.False(t, fault.armed, "scenario never reached the fence leave")
	require.Error(t, err)
	require.Empty(t, locator.markedOfflinePlayers())
	outcome, err = sessionfence.Recheck(ctx, rdb, fenceWiringKey, gen)
	require.NoError(t, err)
	require.Equal(t, sessionfence.OutcomeLeft, outcome, "precondition: the first leave did run on Redis")

	_, err = NewLeaveGameLogic(ctx, svcCtx).LeaveGame(&login_proto.LeaveGameRequest{})
	require.NoError(t, err)
	require.Equal(t, []uint64{fenceWiringPlayerID}, locator.markedOfflinePlayers(),
		"the retried leave must settle the player the lost first run unbound, or it stays ONLINE forever")
}
