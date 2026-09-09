package logic

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"

	"player_locator/internal/svc"
	kafkapb "proto/contracts/kafka"
	pb "proto/player_locator"
	smpb "proto/scene_manager"
	"shared/kafkacmd"
	"shared/safego"
)

const (
	leaseClaimTTL               = 30 * time.Second
	leaseClaimHeartbeatInterval = 10 * time.Second
)

// StartLeaseMonitor polls for expired disconnect leases and publishes cleanup events.
func StartLeaseMonitor(ctx context.Context, svcCtx *svc.ServiceContext, pollInterval time.Duration, batchSize int) {
	if pollInterval <= 0 {
		pollInterval = time.Second
	}
	if batchSize <= 0 {
		batchSize = 100
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	logx.Infof("LeaseMonitor started: poll=%v batch=%d", pollInterval, batchSize)

	for {
		select {
		case <-ctx.Done():
			logx.Info("LeaseMonitor stopped")
			return
		case <-ticker.C:
			// recover 的作用域精确到"一轮":某一轮 panic(例如某条 claim 的载荷
			// 触发了下游的空指针)只丢掉这一轮,下一个 tick 照常继续。
			// 不包的话,整条 LeaseMonitor 会永久停摆 —— 进程还活着、日志不再有新行,
			// 所有断线玩家的会话从此没人清理,是最难被发现的一类故障。
			safego.Run("player_locator.lease_monitor.round", func() {
				processExpiredLeases(ctx, svcCtx, batchSize)
			})
		}
	}
}

func processExpiredLeases(ctx context.Context, svcCtx *svc.ServiceContext, batchSize int) {
	claims, err := claimExpiredLeases(ctx, svcCtx, time.Now(), batchSize, leaseClaimTTL)
	if err != nil {
		logx.Errorf("LeaseMonitor: claim expired leases failed: %v", err)
		return
	}
	stopHeartbeat := startLeaseClaimHeartbeat(ctx, svcCtx, claims, leaseClaimHeartbeatInterval, leaseClaimTTL)
	defer stopHeartbeat()

	for _, claim := range claims {
		handleLeaseExpiry(ctx, svcCtx, claim)
	}
}

func startLeaseClaimHeartbeat(
	ctx context.Context,
	svcCtx *svc.ServiceContext,
	claims []leaseClaim,
	interval time.Duration,
	claimTTL time.Duration,
) func() {
	if len(claims) == 0 {
		return func() {}
	}
	if interval <= 0 {
		interval = leaseClaimHeartbeatInterval
	}
	if claimTTL <= interval {
		claimTTL = interval * 3
	}

	heartbeatCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	// 用 safego.Go 而不是裸 `go func`:这条续期循环一旦 panic,裸写法会把整个
	// player_locator 进程打死 —— 而它跑在"已经领了 claim、副作用做到一半"的窗口里,
	// 正是最不该整进程消失的时刻。外层兜住 panic 保住进程,内层 safego.Run 把
	// recover 的作用域收到**一轮**:某一次续期炸掉只丢这一轮,循环继续按节拍跑,
	// 否则 claim 会在无人续期的情况下静默过期、被别的 worker 重复领走。
	// defer close(done) 在 panic 展开时照样执行,所以下面返回的 stop 函数不会挂死。
	safego.Go("player_locator.lease_claim_heartbeat", func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case now := <-ticker.C:
				safego.Run("player_locator.lease_claim_heartbeat.round", func() {
					if _, err := renewLeaseClaims(heartbeatCtx, svcCtx, claims, now.Add(claimTTL)); err != nil && heartbeatCtx.Err() == nil {
						logx.Errorf("LeaseMonitor: renew processing claims failed: %v", err)
					}
				})
			}
		}
	})

	return func() {
		cancel()
		<-done
	}
}

func handleLeaseExpiry(ctx context.Context, svcCtx *svc.ServiceContext, claim leaseClaim) {
	session := &pb.PlayerSession{}
	if len(claim.payload) == 0 || proto.Unmarshal(claim.payload, session) != nil {
		// 无法解析的载荷不可能构造安全通知。claim payload 仍保留了原字节，先按
		// exact CAS 清理坏 session，再 ack，避免毒任务永远占满 processing。
		result, err := commitLeaseExpiry(ctx, svcCtx, claim)
		if err != nil {
			logx.Errorf("LeaseMonitor: commit malformed session claim player=%d failed: %v", claim.playerID, err)
			return
		}
		if result == 1 {
			logx.Errorf("LeaseMonitor: malformed/missing session payload for player %d; notifications unavailable", claim.playerID)
		}
		if err := ackLeaseClaim(ctx, svcCtx, claim); err != nil {
			logx.Errorf("LeaseMonitor: ack malformed claim player=%d failed: %v", claim.playerID, err)
		}
		return
	}
	if session.PlayerId != claim.playerID {
		// payload 身份不可信时既不能拿它通知另一个玩家，也不能 ACK 后丢掉唯一
		// receipt。保留 processing token/payload，claim deadline 后由新 token 重试；
		// 在数据被修复前 SetSession 继续 fail-closed，避免留下无主 DISCONNECTING
		// 会话却对外伪装成已经完成清理。
		logx.Errorf("LeaseMonitor: claim player_id mismatch member=%d payload=%d; claim retained for retry",
			claim.playerID, session.PlayerId)
		return
	}
	if session.State != pb.PlayerSessionState_SESSION_STATE_DISCONNECTING {
		// 兼容历史残留 ready 条目。新路径的 Reconnect/SetSession 会原子撤销 claim。
		logx.Infof("LeaseMonitor: player %d state=%v is not disconnecting; stale lease acked",
			claim.playerID, session.State)
		if err := ackLeaseClaim(ctx, svcCtx, claim); err != nil {
			logx.Errorf("LeaseMonitor: ack stale claim player=%d failed: %v", claim.playerID, err)
		}
		return
	}

	// AFK 判断必须在确认 claim 对应同一 DISCONNECTING session 后执行；rearm Lua
	// 还会再次比较完整载荷，堵住探测期间发生的 Reconnect。
	// 合成的登出清理条目(MarkOffline 的 LeaveScene 失败后入队)没有活会话键。
	// 挂机月卡只保护**断线未重连**的玩家,显式登出不适用;而且对无会话键的
	// claim 走 rearm,Lua 会因 `not current` 判成换代把 claim 静默丢弃,
	// 清理就永久丢了。所以只在会话键仍然存在时才走 AFK 探测;读键失败
	// (非 Nil)按"存在"处理 —— 宁可多探一次,不能误清月卡玩家。
	if _, sessErr := svcCtx.RedisClient.Get(ctx, sessionKey(claim.playerID)).Result(); !errors.Is(sessErr, redis.Nil) {
		switch afkState := afkPassState(ctx, svcCtx, claim.playerID); afkState {
		case afkPassActive:
			if err := rearmLeaseClaim(ctx, svcCtx, claim, afkPassLeaseTTLSeconds); err != nil {
				logx.Errorf("LeaseMonitor: re-arm AFK lease player=%d failed: %v", claim.playerID, err)
				return
			}
			logx.Infof("LeaseMonitor: player %d has active AFK pass, lease extended", claim.playerID)
			return
		case afkPassUnknown:
			if err := rearmLeaseClaim(ctx, svcCtx, claim, afkPassProbeRetrySeconds); err != nil {
				logx.Errorf("LeaseMonitor: defer AFK probe player=%d failed: %v", claim.playerID, err)
				return
			}
			logx.Errorf("LeaseMonitor: AFK pass probe failed for player %d, deferring cleanup by %ds",
				claim.playerID, afkPassProbeRetrySeconds)
			return
		}
	}

	result, err := commitLeaseExpiry(ctx, svcCtx, claim)
	if err != nil {
		logx.Errorf("LeaseMonitor: commit expiry player=%d failed: %v", claim.playerID, err)
		return
	}
	if result != 1 {
		// 0=claim 被 Reconnect/别的 worker 撤销；2=session 已换代。两者都不能发旧通知。
		return
	}

	// processing payload 在 ack 前一直保留。任一外部副作用失败或进程此刻崩溃，
	// claim 超时后会重试；Gate 以 session_id 校验，LeaveScene 本身也按当前位置幂等。
	var sideEffectErr error
	if err := sendLeaseExpiredToGate(ctx, svcCtx, session); err != nil {
		sideEffectErr = errors.Join(sideEffectErr, err)
	}
	if err := notifySceneManagerLeave(ctx, svcCtx, session); err != nil {
		sideEffectErr = errors.Join(sideEffectErr, err)
	}
	if sideEffectErr != nil {
		logx.Errorf("LeaseMonitor: cleanup side effects player=%d failed; claim retained for retry: %v",
			claim.playerID, sideEffectErr)
		return
	}
	if err := ackLeaseClaim(ctx, svcCtx, claim); err != nil {
		logx.Errorf("LeaseMonitor: ack expiry player=%d failed: %v", claim.playerID, err)
		return
	}

	logx.Infof("LeaseMonitor: lease expired player=%d session=%d version=%d gate=%s scene=%d",
		claim.playerID, session.SessionId, session.SessionVersion, session.GateId, session.SceneId)
}

// ContractsKafkaPlayerLeaseExpiredEventEventId matches event_id.txt entry 34.
// TODO: use generated constant once event_id.go is produced for player_locator.
const contractsKafkaPlayerLeaseExpiredEventEventId uint32 = 34

func sendLeaseExpiredToGate(ctx context.Context, svcCtx *svc.ServiceContext, session *pb.PlayerSession) error {
	if session.GateId == "" {
		return nil
	}
	if svcCtx.KafkaWriter == nil {
		return fmt.Errorf("Kafka writer is not configured")
	}

	// Build inner event payload (use contracts/kafka type to match C++ dispatch)
	event := &kafkapb.PlayerLeaseExpiredEvent{
		PlayerId:       session.PlayerId,
		SessionId:      session.SessionId,
		SceneNodeId:    session.SceneNodeId,
		SceneId:        session.SceneId,
		GateId:         session.GateId,
		GateInstanceId: session.GateInstanceId,
	}
	eventPayload, err := proto.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal PlayerLeaseExpiredEvent: %w", err)
	}

	// Wrap in GateCommand for the Gate Kafka consumer
	eventId := contractsKafkaPlayerLeaseExpiredEventEventId
	cmd := &kafkapb.GateCommand{
		PlayerId:         session.PlayerId,
		SessionId:        session.SessionId,
		Payload:          eventPayload,
		TargetGateId:     parseGateID(session.GateId),
		TargetInstanceId: session.GateInstanceId,
		EventId:          eventId,
	}
	cmdBytes, err := proto.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal GateCommand: %w", err)
	}

	// topic + 分区必须一起算:控制面命令改成 gate-cmd_gN 的 node_id % P 号分区
	// (docs/design/control-plane-topic-partitioning-20260908.md)。
	// gate_id 解析不出数字就没法寻址,fail-closed 让 claim 留待重试,
	// 不能把"发到 0 号分区"当成清理已完成。
	msg, err := kafkacmd.GateCommandMessage(session.GateId,
		fmt.Sprintf("%d", session.PlayerId), cmdBytes)
	if err != nil {
		return fmt.Errorf("address LeaseExpired command: %w", err)
	}
	if err := svcCtx.KafkaWriter.WriteMessages(ctx, msg); err != nil {
		return fmt.Errorf("publish LeaseExpired to %s partition %d: %w", msg.Topic, msg.Partition, err)
	}
	return nil
}

func parseGateID(gateID string) uint32 {
	id, err := strconv.ParseUint(gateID, 10, 32)
	if err != nil {
		logx.Errorf("parseGateID: invalid gateID %q: %v", gateID, err)
		return 0
	}
	return uint32(id)
}

// notifySceneManagerLeave 调用 SceneManager.LeaveScene 清理位置与实例人数。
// 只要仍有权威 scene 位置，client 缺失或 RPC 失败都必须返回错误，让 processing
// receipt 留待重试；不能把未执行的清理当作成功 ACK。
func notifySceneManagerLeave(ctx context.Context, svcCtx *svc.ServiceContext, session *pb.PlayerSession) error {
	sceneID := session.SceneId
	if sceneID == 0 {
		var err error
		sceneID, err = resolveSceneManagerSceneID(ctx, svcCtx, session.PlayerId)
		if err != nil {
			return err
		}
	}
	if sceneID == 0 {
		// SceneManager 没有位置记录意味着 LeaveScene 已完成或玩家尚未真正入场。
		return nil
	}
	if svcCtx.SceneManagerClient == nil {
		return fmt.Errorf("SceneManager client is not configured for player=%d scene=%d",
			session.PlayerId, sceneID)
	}

	_, err := svcCtx.SceneManagerClient.LeaveScene(ctx, &smpb.LeaveSceneRequest{
		PlayerId: session.PlayerId,
		SceneId:  sceneID,
	})
	if err != nil {
		return fmt.Errorf("SceneManager.LeaveScene player=%d scene=%d: %w",
			session.PlayerId, sceneID, err)
	}
	logx.Infof("LeaseMonitor: notified SceneManager.LeaveScene player=%d scene=%d",
		session.PlayerId, sceneID)
	return nil
}

// resolveSceneManagerSceneID 从 SceneManager 已有的 Redis 权威位置键回退读取。
// 这关闭了首次登录 session.SceneId 恒为 0 的现状缺口，同时不要求复制另一份位置。
func resolveSceneManagerSceneID(ctx context.Context, svcCtx *svc.ServiceContext, playerID uint64) (uint64, error) {
	data, err := svcCtx.RedisClient.Get(ctx, sceneManagerLocationKey(playerID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read SceneManager location for player %d: %w", playerID, err)
	}
	location := &smpb.PlayerLocation{}
	if err := proto.Unmarshal(data, location); err != nil {
		return 0, fmt.Errorf("decode SceneManager location for player %d: %w", playerID, err)
	}
	return location.SceneId, nil
}

// --- AFK Pass (挂机月卡) ---

const (
	afkPassKeyPrefix       = "player:afk_pass:"
	afkPassLeaseTTLSeconds = 300 // re-lease for 5 minutes when AFK pass is active
	// afkPassProbeRetrySeconds 是月卡探测失败时把租约顺延的时长。
	// 短到不至于让真掉线玩家的清理拖太久,长到能跨过一次 Redis 抖动。
	afkPassProbeRetrySeconds = 10
)

// afkPassProbeResult 区分「确无月卡」与「查不出来」——两者的正确动作相反:
// 前者继续清理,后者必须推迟清理(fail-closed)。旧实现把两者都当成 false,
// 一次 Redis 抖动就会误清付费月卡玩家。
type afkPassProbeResult int

const (
	afkPassInactive afkPassProbeResult = iota
	afkPassActive
	afkPassUnknown
)

func afkPassKey(playerID uint64) string {
	return fmt.Sprintf("%s%d", afkPassKeyPrefix, playerID)
}

// afkPassState checks whether the player has a valid AFK pass in Redis.
// Key: player:afk_pass:{player_id} -> expiry unix timestamp (set by purchase/activation flow).
func afkPassState(ctx context.Context, svcCtx *svc.ServiceContext, playerID uint64) afkPassProbeResult {
	val, err := svcCtx.RedisClient.Get(ctx, afkPassKey(playerID)).Result()
	if errors.Is(err, redis.Nil) {
		return afkPassInactive // 确定没有月卡
	}
	if err != nil {
		return afkPassUnknown // 查询本身失败,状态未知
	}
	expiry, err := strconv.ParseInt(val, 10, 64)
	if err != nil {
		// 值格式非法 = 数据坏了,按无月卡处理但要能看见。
		logx.Errorf("LeaseMonitor: malformed AFK pass value for player %d: %q", playerID, val)
		return afkPassInactive
	}
	if time.Now().Unix() < expiry {
		return afkPassActive
	}
	return afkPassInactive
}
