package logic

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/proc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"player_locator/internal/config"
	"player_locator/internal/svc"
	kafkapb "proto/contracts/kafka"
	pb "proto/player_locator"
	smpb "proto/scene_manager"
	"shared/kafkacmd"
	"shared/redisslot"
	"shared/safego"
)

const (
	leaseClaimTTL               = 30 * time.Second
	leaseClaimHeartbeatInterval = 10 * time.Second
	// leaseProbeSlotsPerPipeline 是探测到期租约时一个 pipeline 里的 slot 数。
	leaseProbeSlotsPerPipeline = 64
	// leaseScanStallIntervals:一拍没走完超过这么多个 PollInterval(且超过段租约周期)就算停扫,让出段。
	leaseScanStallIntervals = 3
)

// LeaseMonitorParams 是 StartLeaseMonitor 的装配参数。
type LeaseMonitorParams struct {
	// InstanceID 是本副本在分片里的身份(成员登记与段租约的值)。
	InstanceID string
	// PollInterval 即 Lease.PollInterval:拍长,0 = 1s。
	PollInterval time.Duration
	// BatchSize 即 Lease.BatchSize:一拍最多认领的租约数,0 = 100。
	BatchSize int
	// ScanRound 即 Lease.ScanRound():探测完持有的全部 slot 用多久,0 = config.DefaultLeaseScanRoundSeconds。
	ScanRound time.Duration
}

// StartLeaseMonitor 按段租约分到的 slot 轮转探测到期的断线租约,认领并处理。阻塞到 ctx 结束,调用方 go 出去。
//
// 分片只分担负载,认领脚本才是互斥手段:没有段的副本也能认领,这正是 MarkOffline 叫醒本进程处理一个 slot 的做法。
func StartLeaseMonitor(ctx context.Context, svcCtx *svc.ServiceContext, p LeaseMonitorParams) {
	monitor := newLeaseMonitor(svcCtx, nil, p)
	shard := newSlotShard(svcCtx.PlayerRedis, shardRoleLeaseMonitor, p.InstanceID, monitor.scanFresh)
	monitor.owner = shard
	proc.AddShutdownListener(shard.Stop)
	safego.Go("player_locator.lease_monitor_shard", func() { shard.Run(ctx) })

	logx.Infof("LeaseMonitor started: poll=%v batch=%d scan_round=%v", monitor.poll, monitor.batchSize, monitor.scanRound)
	monitor.run(ctx)
	logx.Info("LeaseMonitor stopped")
}

// leaseMonitor 是一个副本上的租约监视。nextSlot 只由 run 所在的 goroutine 访问。
type leaseMonitor struct {
	svcCtx    *svc.ServiceContext
	owner     slotOwner
	poll      time.Duration
	batchSize int
	scanRound time.Duration
	now       func() time.Time

	// nextSlot 是轮转探测的游标:下一拍从不小于它的第一个持有 slot 开始。按 slot 值记,不按下标记,持有的段变了也接得上。
	nextSlot int
	// lastTickNanos 是最近一拍走完的时刻(unix 纳秒),分片的 goroutine 读。
	lastTickNanos atomic.Int64
}

func newLeaseMonitor(svcCtx *svc.ServiceContext, owner slotOwner, p LeaseMonitorParams) *leaseMonitor {
	m := &leaseMonitor{
		svcCtx:    svcCtx,
		owner:     owner,
		poll:      p.PollInterval,
		batchSize: p.BatchSize,
		scanRound: p.ScanRound,
		now:       time.Now,
	}
	if m.poll <= 0 {
		m.poll = time.Second
	}
	if m.batchSize <= 0 {
		m.batchSize = 100
	}
	if m.scanRound <= 0 {
		m.scanRound = config.DefaultLeaseScanRoundSeconds * time.Second
	}
	m.lastTickNanos.Store(m.now().UnixNano())
	return m
}

func (m *leaseMonitor) run(ctx context.Context) {
	ticker := time.NewTicker(m.poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// recover 的作用域精确到"一轮":某一轮 panic(例如某条 claim 的载荷
			// 触发了下游的空指针)只丢掉这一轮,下一个 tick 照常继续。
			// 不包的话,整条 LeaseMonitor 会永久停摆 —— 进程还活着、日志不再有新行,
			// 所有断线玩家的会话从此没人清理,是最难被发现的一类故障。
			safego.Run("player_locator.lease_monitor.round", func() {
				m.tick(ctx)
			})
		case slot := <-m.svcCtx.LeaseCleanupWake:
			safego.Run("player_locator.lease_monitor.wake", func() {
				m.processSlots(ctx, []uint16{slot})
			})
		}
	}
}

// scanFresh 是分片的健康判定:最近一拍走完得够近。一拍可能卡在下游(LeaveScene、Kafka),卡得比段租约周期还久就让出段,
// 让别的副本扫。拍长配置得很长时按拍长放宽,否则永远算停扫。
func (m *leaseMonitor) scanFresh() bool {
	stallAfter := max(shardLeaseTTL, leaseScanStallIntervals*m.poll)
	return m.now().UnixNano()-m.lastTickNanos.Load() <= stallAfter.Nanoseconds()
}

// tick 是一拍:探测下一批持有的 slot,认领到期的租约并处理。走完才记 lastTickNanos,panic 的拍不算。
func (m *leaseMonitor) tick(ctx context.Context) {
	slots := m.nextSlots(m.owner.OwnedSlots())
	if resumeAt, budgetUsed := m.processSlots(ctx, slots); budgetUsed {
		m.nextSlot = resumeAt
	}
	m.lastTickNanos.Store(m.now().UnixNano())
}

// nextSlots 从游标开始取这一拍要探测的持有 slot,取的数量让持有的 slot 在一个 scanRound 内走完一遍。
// 所以全服的探测量是常数(两个 ZSET × 16384 / scanRound),与副本数无关。
func (m *leaseMonitor) nextSlots(owned []uint16) []uint16 {
	if len(owned) == 0 {
		return nil
	}
	count := min(max(int((int64(len(owned))*int64(m.poll)+int64(m.scanRound)-1)/int64(m.scanRound)), 1), len(owned))
	start := sort.Search(len(owned), func(i int) bool { return int(owned[i]) >= m.nextSlot })
	slots := make([]uint16, 0, count)
	for i := 0; i < count; i++ {
		slots = append(slots, owned[(start+i)%len(owned)])
	}
	m.nextSlot = int(slots[count-1]) + 1
	return slots
}

// processSlots 探测 slots、认领到期的租约并逐个处理。一拍最多认领 batchSize 个;用完时返回 (那个 slot, true),
// 下一拍从它接着来,轮转不跳过还没认领完的 slot。
func (m *leaseMonitor) processSlots(ctx context.Context, slots []uint16) (resumeAt int, budgetUsed bool) {
	if len(slots) == 0 {
		return 0, false
	}
	now := m.now()
	due, err := probeDueLeaseSlots(ctx, m.svcCtx.PlayerRedis, slots, now)
	if err != nil {
		// 能探测到的 slot 照常处理:读失败的那些下一轮再来。
		logx.Errorf("LeaseMonitor: probe due leases failed for some slots: %v", err)
	}
	budget := m.batchSize
	var claims []leaseClaim
	for _, slot := range due {
		got, err := claimExpiredLeases(ctx, m.svcCtx, slot, now, budget, leaseClaimTTL)
		if err != nil {
			logx.Errorf("LeaseMonitor: claim expired leases slot=%d failed: %v", slot, err)
		}
		claims = append(claims, got...)
		budget -= len(got)
		if budget <= 0 {
			resumeAt, budgetUsed = int(slot), true
			break
		}
	}
	stopHeartbeat := startLeaseClaimHeartbeat(ctx, m.svcCtx, claims, leaseClaimHeartbeatInterval, leaseClaimTTL)
	defer stopHeartbeat()

	for _, claim := range claims {
		handleLeaseExpiry(ctx, m.svcCtx, claim)
	}
	return resumeAt, budgetUsed
}

// probeDueLeaseSlots 返回 slots 里有到期租约(ready 到期,或 processing 超时要回收)的那些,保持输入顺序。
// 探测只读,按 leaseProbeSlotsPerPipeline 分批走 pipeline(Cluster 下按节点拆)。读失败的 slot 跳过,合并后的错误一并返回。
func probeDueLeaseSlots(ctx context.Context, rdb redis.UniversalClient, slots []uint16, now time.Time) ([]uint16, error) {
	maxScore := strconv.FormatInt(now.Unix(), 10)
	var due []uint16
	var firstErr error
	failed := 0
	for start := 0; start < len(slots); start += leaseProbeSlotsPerPipeline {
		chunk := slots[start:min(start+leaseProbeSlotsPerPipeline, len(slots))]
		ready := make([]*redis.IntCmd, len(chunk))
		processing := make([]*redis.IntCmd, len(chunk))
		_, _ = rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
			for i, slot := range chunk {
				keys := leaseKeysForSlot(slot)
				ready[i] = p.ZCount(ctx, keys.ready, "-inf", maxScore)
				processing[i] = p.ZCount(ctx, keys.processing, "-inf", maxScore)
			}
			return nil
		})
		for i, slot := range chunk {
			err := errors.Join(ready[i].Err(), processing[i].Err())
			if err != nil {
				failed++
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if ready[i].Val() > 0 || processing[i].Val() > 0 {
				due = append(due, slot)
			}
		}
	}
	if firstErr != nil {
		return due, fmt.Errorf("%d slot(s) failed, first: %w", failed, firstErr)
	}
	return due, nil
}

// wakeLeaseMonitorAt 让本进程的 LeaseMonitor 到点处理这名玩家的 slot,不等常规轮转。给登出清理的首拍用:清理在途期间
// 同账号首次登录被拒(replaceSessionCleanupPending),这段时间不该再加上一个探测周期。叫不醒(通道满了)也无妨,常规轮转兜底。
func wakeLeaseMonitorAt(svcCtx *svc.ServiceContext, playerID uint64, readyAt time.Time) {
	wake := svcCtx.LeaseCleanupWake
	if wake == nil {
		return
	}
	slot := redisslot.PlayerSlot(playerID)
	time.AfterFunc(time.Until(readyAt), func() {
		select {
		case wake <- slot:
		default:
		}
	})
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
	if _, sessErr := svcCtx.PlayerRedis.Get(ctx, sessionKey(claim.playerID)).Result(); !errors.Is(sessErr, redis.Nil) {
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
		if isOnlyHolderReleasePending(sideEffectErr) {
			// 不是故障:location 的持有节点还没释放(docs/design/location-release-binding.md §3.4),scene_manager 已请它
			// 释放。claim 照旧保留,按 claim 超时节拍重试;等不到时由 scene_manager 的上限兜底。
			logx.Infof("LeaseMonitor: waiting for location holder to release player=%d; claim retained for retry: %v",
				claim.playerID, sideEffectErr)
			return
		}
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
//
// 请求带**被删那条会话记录的原值** (session_id, login_generation)(docs/design/scene-enter-chain-residuals.md §3.2):
// scene_manager 拿它与 location 里记的会话身份比较,location 已属于更新的登录时不删,迟到的离场就删不掉
// 新登录的 location。MarkOffline 同步路径、租约到期路径、enqueue 重试都经过这里;enqueue 用 proto.Clone
// 保留了代次,所以只在这一处填。旧版 scene_manager 忽略这两个字段,按旧行为删除。
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
		PlayerId:        session.PlayerId,
		SceneId:         sceneID,
		SessionId:       session.SessionId,
		LoginGeneration: session.LoginGeneration,
	})
	if err != nil {
		if status.Code(err) == codes.FailedPrecondition {
			// scene_manager 拿不到「持有者已释放」证据,保留了 location(docs/design/location-release-binding.md §3.2)。
			// 照样回错误让调用方保留清理任务,但包成 errHolderReleasePending,调用方据此不按故障记录。
			return fmt.Errorf("%w: SceneManager.LeaveScene player=%d scene=%d session=%d login_generation=%d: %v",
				errHolderReleasePending, session.PlayerId, sceneID, session.SessionId, session.LoginGeneration, err)
		}
		return fmt.Errorf("SceneManager.LeaveScene player=%d scene=%d session=%d login_generation=%d: %w",
			session.PlayerId, sceneID, session.SessionId, session.LoginGeneration, err)
	}
	logx.Infof("LeaseMonitor: notified SceneManager.LeaveScene player=%d scene=%d session=%d login_generation=%d",
		session.PlayerId, sceneID, session.SessionId, session.LoginGeneration)
	return nil
}

// errHolderReleasePending:scene_manager 的 LeaveScene 回 FailedPrecondition —— location 的持有节点还没把当前代状态
// 落盘释放,location 被保留(docs/design/location-release-binding.md §3.2 / §3.4)。它和 RPC 失败一样要求保留清理任务
// 重试,区别只在日志级别与 MarkOffline 入队的首拍延后。
var errHolderReleasePending = errors.New("location holder release pending")

// holderReleaseFirstRetryDelay:MarkOffline 因等持有者释放而入队时,首拍延后的时长。gate 收到 LeaveGame 时已代发 ExitGame,
// 退出存盘与 A1′ 标记通常在几十毫秒内落地;延后 2s 让第一次重试大概率就拿到标记,不必等满 claim 超时。
const holderReleaseFirstRetryDelay = 2 * time.Second

// isOnlyHolderReleasePending:清理副作用里唯一的失败是「等持有者释放」(LeaseExpired 投递等其它失败一律按故障记录)。
func isOnlyHolderReleasePending(err error) bool {
	if !errors.Is(err, errHolderReleasePending) {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range joined.Unwrap() {
			if !errors.Is(e, errHolderReleasePending) {
				return false
			}
		}
	}
	return true
}

// resolveSceneManagerSceneID 从 SceneManager 已有的 Redis 权威位置键回退读取。
// 这关闭了首次登录 session.SceneId 恒为 0 的现状缺口，同时不要求复制另一份位置。
func resolveSceneManagerSceneID(ctx context.Context, svcCtx *svc.ServiceContext, playerID uint64) (uint64, error) {
	data, err := svcCtx.PlayerRedis.Get(ctx, sceneManagerLocationKey(playerID)).Bytes()
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
	return redisslot.AfkPassKey(playerID)
}

// afkPassState checks whether the player has a valid AFK pass in Redis.
// Key: player:afk_pass:{<player_id>} -> expiry unix timestamp (set by purchase/activation flow).
func afkPassState(ctx context.Context, svcCtx *svc.ServiceContext, playerID uint64) afkPassProbeResult {
	val, err := svcCtx.PlayerRedis.Get(ctx, afkPassKey(playerID)).Result()
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
