package clientplayerloginlogic

import (
	"context"
	"login/internal/logic/pkg/loginsession"
	"login/internal/logic/pkg/sessionfence"
	"login/internal/svc"
	login_proto "proto/login"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type LeaveGameLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewLeaveGameLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LeaveGameLogic {
	return &LeaveGameLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *LeaveGameLogic) LeaveGame(in *login_proto.LeaveGameRequest) (*login_proto.LoginEmptyResponse, error) {
	resp := &login_proto.LoginEmptyResponse{}

	sessionDetails, ok := getSessionDetailsForAction(l.ctx, "leave game")
	if !ok {
		return resp, nil
	}

	// 没有完整连接身份的连接过不了 Login,也进不了游戏,没有任何状态要收尾。
	sessionRef, hasIdentity := loginsession.RefOf(sessionDetails)
	if !hasIdentity {
		logMissingConnectionIdentity("LeaveGame", sessionDetails)
		return resp, nil
	}

	// gate 连接会话栅栏(docs/design/client-request-ordering.md §6.2):记一次离开游戏(gen + 1)并解除绑定。
	// LeaveGame 不拿玩家锁,会与 EnterGame 的后台链并发;后台链在绑定点 / 进场前复查看到 gen 变了就中止,
	// 不会在这次离开之后又把会话与位置写回去。gate 会话还没收到 BindSession 时(会话头的 player_id 为 0,
	// 或是换角前的旧角色),也用栅栏里绑定的玩家删会话;两者都收尾,见 sessionPlayersToSettle。
	bound, err := sessionfence.Leave(l.ctx, l.svcCtx.LoginRedis, sessionRef.Fence(), sessionFenceTTL)
	if err != nil {
		// fail-closed(F4,§6.2 第三种竞态):栅栏没记下这次离开(gen 没递增、绑定没解除),就不收尾、不回成功。
		// 回成功的话:在途的 EnterGame 后台链在绑定点 / 进场前复查看不到 gen 变化,会在这次「离开」之后把会话与位置写回去;
		// 绑定也还在,而进游戏入口对已绑定未关闭的记录续满一周寿命(sessionfence checkEnterGameScript),残留绑定一直续命,
		// 这条连接之后换角一律被拒。回错误:路由服转成带请求 id 的 1003,客户端重发 LeaveGame。重发是安全的:
		// gate 在发出 LeaveGame 时(直连模式收包即发出)做的 OnClientLeaveGame 可重入(scene_route_helper.h ResetSceneRoute
		// 对同一角色的重复重置只抬高不覆盖;路由已清,不会再代发 ExitGame),loginSucceeded 不随 LeaveGame 清,重发照样转到 login;
		// login 这边 Leave 可重放(脚本已执行、回包丢了时重放返回同一个解除的玩家,fence.go),清登录态按键删除,
		// DeleteSession 按 session_id + gate 实例做 CAS,都幂等。
		logx.Errorf("LeaveGame aborted: session fence leave failed, nothing cleaned up, client must retry: "+
			"session_id=%d gate_instance=%q incarnation=%d err=%v",
			sessionDetails.GetSessionId(), sessionDetails.GetGateInstanceId(), sessionDetails.GetSessionIncarnation(), err)
		return nil, status.Error(codes.Unavailable, "login session fence unavailable, retry LeaveGame")
	}

	cleanupLoginSessionState(l.ctx, l.svcCtx, sessionRef, "leave")

	// Remove player session from Redis (replaces Centre notification)
	// 带上本连接所在的 gate 实例号,语义同 Disconnect(docs/design/scene-enter-chain-residuals.md §6)。
	for _, playerID := range sessionPlayersToSettle(sessionDetails.PlayerId, bound) {
		deletePlayerSession(l.ctx, l.svcCtx, playerID, sessionDetails.SessionId, sessionDetails.GetGateInstanceId())
	}

	return resp, nil
}
