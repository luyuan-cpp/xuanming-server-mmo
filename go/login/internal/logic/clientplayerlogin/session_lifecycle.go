package clientplayerloginlogic

import (
	"context"

	"login/internal/logic/pkg/ctxkeys"
	"login/internal/logic/pkg/loginsession"
	"login/internal/logic/pkg/sessionmanager"
	"login/internal/svc"
	login_proto_common "proto/common/base"

	"github.com/zeromicro/go-zero/core/logx"
)

func getSessionDetailsForAction(ctx context.Context, action string) (*login_proto_common.SessionDetails, bool) {
	sessionDetails, ok := ctxkeys.GetSessionDetails(ctx)
	if !ok {
		logx.Errorf("Session not found in context during %s", action)
		return nil, false
	}

	return sessionDetails, true
}

func cleanupLoginSessionState(ctx context.Context, svcCtx *svc.ServiceContext, sessionID uint32, logicTag string) {
	loginsession.Cleanup(ctx, svcCtx.RedisClient, sessionID, logicTag)
}

func deletePlayerSession(ctx context.Context, svcCtx *svc.ServiceContext, playerID uint64, sessionID uint32) {
	if playerID == 0 || sessionID == 0 {
		return
	}

	if err := sessionmanager.DeleteSession(ctx, svcCtx.PlayerLocatorClient, playerID, sessionID); err != nil {
		logx.Errorf("Failed to delete session for player %d: %v", playerID, err)
	}
}

func markPlayerSessionDisconnecting(ctx context.Context, svcCtx *svc.ServiceContext, playerID uint64, sessionID uint32) {
	if playerID == 0 {
		return
	}

	if err := sessionmanager.SetSessionDisconnecting(ctx, svcCtx.PlayerLocatorClient, playerID, sessionID); err != nil {
		logx.Errorf("Failed to set session disconnecting for player %d: %v", playerID, err)
		return
	}

	logx.Infof("Session disconnecting with 30s lease: player=%d session=%d", playerID, sessionID)
}
