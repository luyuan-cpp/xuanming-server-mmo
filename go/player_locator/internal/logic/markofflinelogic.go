package logic

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"

	"player_locator/internal/svc"
	common "proto/common/base"
	pb "proto/player_locator"
)

type MarkOfflineLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewMarkOfflineLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MarkOfflineLogic {
	return &MarkOfflineLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// MarkOffline 是**正常登出**(LeaveGame)的终点:玩家不再在线,三份状态都要清掉。
//
// 旧实现只删了 location 键,而会话键是无 TTL 写入的(SetSession 用 expiration=0),
// 正常登出路径又从不调 SetDisconnecting,所以它既不进租约 ZSET、也永远等不到
// LeaseMonitor —— 后果三条:
//  1. player:session:* 随累计登录过的账号数单调增长,没有任何回收路径;
//  2. guild 的在线判定直接看这个键存不存在,正常登出的玩家对公会**永久显示在线**;
//  3. 下次登录读到 State=ONLINE 的陈旧会话,被判成顶号重登而非首次登录,
//     还会朝早已消失的旧 gate 发一次 Kick。
func (l *MarkOfflineLogic) MarkOffline(in *pb.MarkOfflineRequest) (*common.Empty, error) {
	if in == nil || in.PlayerId == 0 {
		return nil, fmt.Errorf("player_id is required")
	}
	if in.ExpectedSessionId == 0 || in.ExpectedSessionVersion == 0 {
		return nil, fmt.Errorf("expected_session_id and expected_session_version are required")
	}

	playerID := in.PlayerId
	data, err := l.svcCtx.RedisClient.Get(l.ctx, sessionKey(playerID)).Bytes()
	if err != nil && err != redis.Nil {
		return nil, err
	}
	if err == redis.Nil {
		// 没有可证明属于本次 LeaveGame 的会话时不碰 location/lease。旧请求不能
		// 凭 player_id 单独清理，因为此刻可能正处于新会话的建链窗口。
		return &common.Empty{}, nil
	}

	current := &pb.PlayerSession{}
	if err := proto.Unmarshal(data, current); err != nil {
		return nil, fmt.Errorf("decode current session: %w", err)
	}
	if current.PlayerId != playerID {
		return nil, fmt.Errorf("session player mismatch: key player=%d payload player=%d", playerID, current.PlayerId)
	}
	if current.SessionId != in.ExpectedSessionId || current.SessionVersion != in.ExpectedSessionVersion {
		l.Infof(
			"MarkOffline: stale cleanup ignored player=%d expected_session=%d/%d current_session=%d/%d",
			playerID,
			in.ExpectedSessionId,
			in.ExpectedSessionVersion,
			current.SessionId,
			current.SessionVersion,
		)
		return &common.Empty{}, nil
	}

	// Lua 比较完整 protobuf 原始字节后才删除。该字节包含已经在上面核对过的
	// session_id/session_version，因此比只比较这两个字段更强：二次 GET 到 Lua
	// 执行之间任何会话变化都会让 CAS 失败。
	deleted, err := deleteSessionIfUnchanged(l.ctx, l.svcCtx, playerID, data)
	if err != nil {
		return nil, err
	}
	if !deleted {
		// GET 之后已有新版本写入：旧 MarkOffline 不能删除它。
		l.Infof("MarkOffline: session changed during CAS for player %d; stale cleanup ignored", playerID)
		return &common.Empty{}, nil
	}

	// 会话删除成功后必须清理 SceneManager 侧的权威位置与实例人数。
	//
	// 此前正常登出从不走这一步:全仓 SceneManager.LeaveScene 的唯一调用点在
	// LeaseMonitor(断线租约到期路径),而正常登出删掉会话后 SetDisconnecting
	// 因 redis.Nil no-op,租约链永远不会启动 —— 权威键 player:{id}:location
	// 无 TTL 永久残留,实例 player_count 幽灵 +1(destroy-on-empty 永不触发),
	// 多世界节点区服下次登录选到别的节点还会被 unsafe-handoff 闸门确定性拒绝,
	// 玩家永久进不了游戏。
	l.notifySceneManagerLeaveOrEnqueue(playerID, current)
	return &common.Empty{}, nil
}

// notifySceneManagerLeaveOrEnqueue 先同步调 LeaveScene(快路径);失败则把
// 会话快照(改成 DISCONNECTING 态)入租约 ready 队列,交 LeaseMonitor 的
// at-least-once 机制重试 —— 与断线路径共用同一条清理通道,不能只打日志吞掉。
func (l *MarkOfflineLogic) notifySceneManagerLeaveOrEnqueue(
	playerID uint64,
	session *pb.PlayerSession,
) {
	leaveErr := notifySceneManagerLeave(l.ctx, l.svcCtx, session)
	if leaveErr == nil {
		return
	}
	l.Errorf("MarkOffline: LeaveScene failed for player %d, enqueueing for lease-monitor retry: %v",
		playerID, leaveErr)

	// 入队的快照必须是 DISCONNECTING 态:LeaseMonitor 对 State!=DISCONNECTING
	// 的条目按"历史残留"直接 ack 丢弃。原字节是 ONLINE 态,不能直接用。
	retrySession := proto.Clone(session).(*pb.PlayerSession)
	retrySession.State = pb.PlayerSessionState_SESSION_STATE_DISCONNECTING
	retryBytes, err := proto.Marshal(retrySession)
	if err != nil {
		l.Errorf("MarkOffline: marshal retry session for player %d failed, scene cleanup LOST: %v",
			playerID, err)
		return
	}

	enqueued, err := enqueueOfflineCleanup(l.ctx, l.svcCtx, playerID, retryBytes)
	if err != nil {
		// 双重失败(LeaveScene RPC + Redis 入队都挂):此时只剩日志可依赖。
		// 位置残留的后果与修复手段见 PROGRESS.md 2026-08-09 节。
		l.Errorf("MarkOffline: enqueue cleanup retry for player %d failed, scene cleanup LOST: %v",
			playerID, err)
		return
	}
	if !enqueued {
		// 玩家已瞬间重登(会话键重新出现):新登录的 EnterScene 会改写位置,
		// 旧清理不再需要。
		l.Infof("MarkOffline: player %d re-logged in before cleanup enqueue; skip", playerID)
	}
}
