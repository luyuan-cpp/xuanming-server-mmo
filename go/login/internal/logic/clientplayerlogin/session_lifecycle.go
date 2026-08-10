package clientplayerloginlogic

import (
	"context"
	"time"

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

	// 这一步是整条清理状态机的**唯一**入口:只有 SetDisconnecting 的 Lua 会把
	// 玩家 ZADD 进租约 ZSET,LeaseMonitor 只消费该 ZSET。会话键又是无 TTL 写入
	// 的,所以这条 RPC 丢了 = 会话永久停留 ONLINE(公会永久显示在线、
	// LeaveScene 永不执行、位置与实例人数泄漏),没有任何对账路径能补救。
	// gate 侧发出通知前就删了本地会话、也不重试 —— login 是最后一道有状态的
	// 关口,不能一次失败就吞掉。带退避重试若干次,吃掉 player_locator
	// 发版/重启量级的不可用窗口;仍失败则 CRITICAL 级日志留人工线索
	// (docs/ops/release-checklist.md 的 #B-1 SCAN 巡检就是对应的兜底)。
	const maxAttempts = 4
	backoff := 200 * time.Millisecond
	var lastErr error
	for attempt := 1; ; attempt++ {
		lastErr = sessionmanager.SetSessionDisconnecting(ctx, svcCtx.PlayerLocatorClient, playerID, sessionID)
		if lastErr == nil {
			logx.Infof("Session disconnecting with 30s lease: player=%d session=%d", playerID, sessionID)
			return
		}
		if attempt >= maxAttempts || ctx.Err() != nil {
			break
		}
		logx.Errorf("SetSessionDisconnecting attempt %d/%d failed for player %d: %v",
			attempt, maxAttempts, playerID, lastErr)
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff *= 2
	}

	logx.Severef("CRITICAL: session stuck ONLINE — SetSessionDisconnecting exhausted retries "+
		"player=%d session=%d err=%v (manual fix: see release-checklist #B-1 sweep)",
		playerID, sessionID, lastErr)
}
