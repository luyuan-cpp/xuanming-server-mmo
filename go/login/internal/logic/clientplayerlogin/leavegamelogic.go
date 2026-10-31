package clientplayerloginlogic

import (
	"context"
	"login/internal/logic/pkg/loginsession"
	"login/internal/logic/pkg/sessionfence"
	"login/internal/svc"
	login_proto "proto/login"

	"github.com/zeromicro/go-zero/core/logx"
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
		logx.Errorf("LeaveGame session fence update failed: sessionId=%d err=%v", sessionDetails.SessionId, err)
		bound = 0
	}

	cleanupLoginSessionState(l.ctx, l.svcCtx, sessionRef, "leave")

	// Remove player session from Redis (replaces Centre notification)
	// 带上本连接所在的 gate 实例号,语义同 Disconnect(docs/design/scene-enter-chain-residuals.md §6)。
	for _, playerID := range sessionPlayersToSettle(sessionDetails.PlayerId, bound) {
		deletePlayerSession(l.ctx, l.svcCtx, playerID, sessionDetails.SessionId, sessionDetails.GetGateInstanceId())
	}

	return resp, nil
}
