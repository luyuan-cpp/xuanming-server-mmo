package clientplayerloginlogic

import (
	"context"
	"login/internal/logic/pkg/loginsession"
	"login/internal/logic/pkg/sessionfence"
	"login/internal/svc"
	login_proto "proto/login"

	"github.com/zeromicro/go-zero/core/logx"
)

type DisconnectLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewDisconnectLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DisconnectLogic {
	return &DisconnectLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *DisconnectLogic) Disconnect(in *login_proto.LoginNodeDisconnectRequest) (*login_proto.LoginEmptyResponse, error) {
	sessionDetails, ok := getSessionDetailsForAction(l.ctx, "disconnect")
	if !ok {
		return &login_proto.LoginEmptyResponse{}, nil
	}

	// 没有完整连接身份的连接过不了 Login,也进不了游戏,没有任何状态要收尾。
	sessionRef, hasIdentity := loginsession.RefOf(sessionDetails)
	if !hasIdentity {
		logMissingConnectionIdentity("Disconnect", sessionDetails)
		return &login_proto.LoginEmptyResponse{}, nil
	}

	// gate 连接会话栅栏(docs/design/client-request-ordering.md §6.2):先标记这条连接已关闭,再清理登录态。
	// 之后晚到的 Login 不再写 login_session;EnterGame 后台链在绑定点看到关闭会自己补挂断线租约。
	// 反过来,后台链已经绑定了玩家、而 gate 会话还没收到 BindSession 时,从栅栏读回绑定的玩家补挂租约:
	// 这时会话头里的 player_id 是 0(首次进游戏),或是换角前的旧角色(gate 在 LeaveGame 后不清 playerId),
	// 只认会话头的话新角色会永久停在 ONLINE。两者都收尾,见 sessionPlayersToSettle。
	bound, err := sessionfence.Close(l.ctx, l.svcCtx.LoginRedis, sessionRef.Fence(), sessionFenceTTL)
	if err != nil {
		// 栅栏写失败不阻断清理:照旧按会话里的身份清,只是失去「补挂租约」这一层兜底。
		logx.Errorf("Disconnect session fence close failed: sessionId=%d err=%v", sessionDetails.SessionId, err)
		bound = 0
	}

	cleanupLoginSessionState(l.ctx, l.svcCtx, sessionRef, "disconnect")

	// Mark session as disconnecting with 30s lease in Redis (replaces Centre's DelayedCleanupTimer).
	// When the Redis key TTL expires, the session is effectively cleaned up.
	// 带上本连接所在的 gate 实例号:同号跨实例时,旧实例迟到的断线由 player_locator 按实例号忽略
	// (docs/design/scene-enter-chain-residuals.md §6)。栅栏绑定的玩家来自同一个含实例号的栅栏键,用同一个实例号。
	for _, playerID := range sessionPlayersToSettle(sessionDetails.PlayerId, bound) {
		markPlayerSessionDisconnecting(l.ctx, l.svcCtx, playerID, in.SessionId, sessionDetails.GetGateInstanceId())
	}

	return &login_proto.LoginEmptyResponse{}, nil
}
