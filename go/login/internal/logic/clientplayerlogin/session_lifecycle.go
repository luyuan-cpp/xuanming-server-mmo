package clientplayerloginlogic

import (
	"context"
	"errors"
	"time"

	"login/internal/logic/pkg/ctxkeys"
	"login/internal/logic/pkg/loginsession"
	"login/internal/logic/pkg/sessionmanager"
	"login/internal/svc"
	login_proto_common "proto/common/base"

	"github.com/redis/go-redis/v9"
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

// cleanupLoginSessionState 删本条 gate 连接的登录态(按 loginsession.RefOf 的连接身份,不按裸 session_id)。
func cleanupLoginSessionState(ctx context.Context, svcCtx *svc.ServiceContext, ref loginsession.Ref, logicTag string) {
	loginsession.Cleanup(ctx, svcCtx.LoginRedis, ref, logicTag)
}

// logMissingConnectionIdentity 记一条「经 gate 转发的请求没有完整连接身份」。gate 每条连接都带实例号与会话代次,
// 走到这里是接线缺陷,不是玩家行为。
func logMissingConnectionIdentity(action string, details *login_proto_common.SessionDetails) {
	logx.Errorf("%s rejected: incomplete gate connection identity session_id=%d gate_instance=%q incarnation=%d",
		action, details.GetSessionId(), details.GetGateInstanceId(), details.GetSessionIncarnation())
}

// logGetAccountFailure 记 CreatePlayer / EnterGame 读不到本连接登录态的原因。redis.Nil = 本连接没有成功 Login 过
// (或登录态已过期、已被 LeaveGame / Disconnect 清理);gate 重启后跳过 Login 的请求也落在这里,不是服务端故障。
func logGetAccountFailure(action string, details *login_proto_common.SessionDetails, err error) {
	if errors.Is(err, redis.Nil) {
		logx.Infof("%s rejected: no login session on this gate connection session_id=%d gate_instance=%q incarnation=%d",
			action, details.GetSessionId(), details.GetGateInstanceId(), details.GetSessionIncarnation())
		return
	}
	logx.Errorf("%s GetAccount failed: session_id=%d err=%v", action, details.GetSessionId(), err)
}

// sessionPlayersToSettle 返回 Disconnect / LeaveGame 要收尾的玩家:gate 会话头带来的玩家,
// 加上会话栅栏里绑定的玩家(不同且非 0 时)。两者都要处理:gate 在 LeaveGame 之后不清 playerId,
// 同一连接换角时会话头带的是旧角色,只认它会把栅栏刚绑定的新角色永久留在 ONLINE;
// 反过来旧角色之前的 MarkOffline 若失败过,会话头里的它是清理它的唯一线索。
// 下游 SetDisconnecting / DeleteSession 都按 session_id 做 CAS,对不属于本会话的玩家是 no-op。
func sessionPlayersToSettle(fromGate, bound uint64) []uint64 {
	players := make([]uint64, 0, 2)
	if fromGate != 0 {
		players = append(players, fromGate)
	}
	if bound != 0 && bound != fromGate {
		players = append(players, bound)
	}
	return players
}

// deletePlayerSession / markPlayerSessionDisconnecting 的 gateInstanceID 是发起这次清理的 gate 实例 uuid
// (Disconnect / LeaveGame 取 SessionDetails.gate_instance_id,入场链补偿取 state.gateInstanceID)。
// player_locator 据此忽略旧实例迟到的同号上报;空 = 不校验(docs/design/scene-enter-chain-residuals.md §6)。
func deletePlayerSession(ctx context.Context, svcCtx *svc.ServiceContext, playerID uint64, sessionID uint32, gateInstanceID string) {
	if playerID == 0 || sessionID == 0 {
		return
	}

	if err := sessionmanager.DeleteSession(ctx, svcCtx.PlayerLocatorClient, playerID, sessionID, gateInstanceID); err != nil {
		logx.Errorf("Failed to delete session for player %d: %v", playerID, err)
	}
}

func markPlayerSessionDisconnecting(ctx context.Context, svcCtx *svc.ServiceContext, playerID uint64, sessionID uint32, gateInstanceID string) {
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
		lastErr = sessionmanager.SetSessionDisconnecting(ctx, svcCtx.PlayerLocatorClient, playerID, sessionID, gateInstanceID)
		if lastErr == nil {
			logx.Infof("Session disconnecting with 30s lease: player=%d session=%d gate_instance=%q", playerID, sessionID, gateInstanceID)
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
		"player=%d session=%d gate_instance=%q err=%v (manual fix: see release-checklist #B-1 sweep)",
		playerID, sessionID, gateInstanceID, lastErr)
}
