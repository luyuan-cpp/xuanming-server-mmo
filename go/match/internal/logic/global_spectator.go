package logic

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
	"match/internal/authority"
	"match/internal/constants"
	"match/internal/discovery"
	"match/internal/metrics"
	"match/internal/pkg/ctxkeys"
	"match/internal/svc"
	battlepb "proto/battle"
	pb "proto/match"
)

// errGlobalSpectatePending 标记原观战操作尚未拿到 battle 的终局证明：取消回执未确认、准入查询非终态，
// 或状态 CAS 没有推进。不是服务端故障：同 request_id 重试或后台恢复会沿原件继续推进。
// 包装 ErrSpectatorBusy，沿用既有 errors.Is 判断。
var errGlobalSpectatePending = fmt.Errorf("%w: 原观战操作尚未获得终局证明", authority.ErrSpectatorBusy)

// globalWatchVerdict 是全局观战一个出口回给客户端的结论。tip=0 表示准入成功；
// fault 只标服务端故障（存储、传输、原件损坏），只有它打 Error 日志。
type globalWatchVerdict struct {
	tip     uint32
	outcome string // match_watch_battle_total 的 outcome 标签，取值固定、低基数
	msg     string
	fault   bool
}

// globalWatchResult 是一次全局观战请求的结论；cause 只进日志，不回客户端。
type globalWatchResult struct {
	battleID uint64
	verdict  globalWatchVerdict
	cause    error
}

func globalWatchFailure(err error) globalWatchResult {
	return globalWatchResult{verdict: globalWatchErrVerdict(err), cause: err}
}

// globalWatchErrVerdict 把全局观战链路上的错误映射为客户端结论。判定顺序不能换：
//   - 票据已建房同时包装了 ErrMatchEntryOccupied，必须先于「匹配中」判定；
//   - 观战侧细分都包装 ErrSpectatorBusy，必须先于裸 Busy 判定；
//   - 裸 ErrSpectatorBusy、ErrOriginalPlayer（owner 变更、正文冲突、原件损坏）与存储 / 传输错误
//     一律 16004 并按故障计（fail-closed）。
func globalWatchErrVerdict(err error) globalWatchVerdict {
	switch {
	case errors.Is(err, authority.ErrTicketInBattle):
		return globalWatchVerdict{tip: constants.ErrSpectateWhileInBattle, outcome: "in_battle", msg: "战斗尚未结束，无法观战"}
	case errors.Is(err, authority.ErrMatchEntryOccupied):
		return globalWatchVerdict{tip: constants.ErrSpectateWhileQueued, outcome: "queued", msg: "匹配中无法观战"}
	case errors.Is(err, authority.ErrSpectatorFormationClosed):
		return closedWatchVerdict(constants.ErrBattleNotWatchable)
	case errors.Is(err, authority.ErrSpectatorIntentClosed):
		return closedWatchVerdict(0)
	case errors.Is(err, authority.ErrSpectatorSuperseded), errors.Is(err, authority.ErrSpectatorOccupied):
		// 被替代时 SpectatorIntent 已给旧意图记下同码原因，首答与旧 request_id 重放得到逐字相同的结论。
		return closedWatchVerdict(constants.ErrAlreadyWatching)
	case errors.Is(err, errGlobalSpectatePending):
		return globalWatchVerdict{tip: constants.ErrInternal, outcome: "pending", msg: "观战原请求尚未确认，请复用原请求重试"}
	default:
		return globalWatchVerdict{tip: constants.ErrInternal, outcome: "internal", msg: "观战原请求尚未确认，请稍后重试", fault: true}
	}
}

// closedWatchVerdict 把意图上持久化的关闭原因还原为客户端结论；outcome 只由原因决定，首答与重放计同一标签。
// 0 = 未记录原因（业务退出、battle 报观众已不在或空回包而关闭），回通用的「原观战请求已关闭」。
func closedWatchVerdict(closeTip uint32) globalWatchVerdict {
	switch closeTip {
	case 0:
		return globalWatchVerdict{tip: constants.ErrBattleNotWatchable, outcome: "closed", msg: "原观战请求已关闭"}
	case constants.ErrNoWatchableBattle:
		return globalWatchVerdict{tip: closeTip, outcome: "no_battle", msg: "当前没有可观战的战斗"}
	case constants.ErrAlreadyWatching:
		// 被更新的观战请求替代（authority.SpectatorIntent 写入）。
		return globalWatchVerdict{tip: closeTip, outcome: "already_watching", msg: "已在观战中"}
	case constants.ErrBattleNotWatchable:
		// 意图只存客户端 tip：成局已变、准入截止与 battle 的各类拒绝（含房间不存在）同落 16018，
		// 全局路径一律记 not_found，不再细分 rejected（细分须给意图另存关闭类别，见 resumeGlobalSpectator）。
		return globalWatchVerdict{tip: closeTip, outcome: "not_found", msg: "该战斗当前无法观战"}
	case constants.ErrSpectateFull:
		return globalWatchVerdict{tip: closeTip, outcome: "full", msg: "观战人数已满"}
	case constants.ErrSpectateOffline:
		return globalWatchVerdict{tip: closeTip, outcome: "offline", msg: "观战会话已断开"}
	default:
		// 原因只由本服务写入；不认识的值（如滚动升级中新版本写入的原因）照原码回给客户端，不改判成别的结论。
		return globalWatchVerdict{tip: closeTip, outcome: "rejected", msg: "该战斗当前无法观战"}
	}
}

// globalWatchBattle 是全局（TiDB 权威）观战入口。业务拒绝回明确 tip；参数错、ErrOriginalPlayer 与存储 / 传输
// 故障维持 16004（fail-closed）；只有故障打 Error 日志；每个出口都计 match_watch_battle_total。
func globalWatchBattle(l *WatchBattleLogic, in *pb.WatchBattleRequest, player uint64) (*pb.WatchBattleResponse, error) {
	id, err := uuid.Parse(in.RequestId)
	if err != nil || id == uuid.Nil || id.String() != in.RequestId || (in.PlayerId != 0 && in.PlayerId != player) {
		// 参数错在任何 SQL 之前拒绝；不是服务端故障，沿用 16004（与 Redis 路径缺身份同口径），不打 Error。
		metrics.ObserveWatchBattle("internal")
		return &pb.WatchBattleResponse{ErrorMessage: tipErr(constants.ErrInternal, "观战请求参数无效")}, nil
	}
	result := l.globalWatch(in, player)
	verdict := result.verdict
	switch {
	case verdict.fault:
		l.Errorf("全局观战失败 player=%d request=%s outcome=%s: %v", player, in.RequestId, verdict.outcome, result.cause)
	case result.cause != nil:
		l.Infof("全局观战未准入 player=%d request=%s outcome=%s: %v", player, in.RequestId, verdict.outcome, result.cause)
	}
	metrics.ObserveWatchBattle(verdict.outcome)
	resp := &pb.WatchBattleResponse{BattleId: result.battleID, SpectatorOperationId: in.RequestId}
	if verdict.tip != 0 {
		resp.ErrorMessage = tipErr(verdict.tip, verdict.msg)
	}
	return resp, nil
}

// globalWatch 先占定请求头，再按意图状态续做：已关闭回持久化原因；已绑定原件沿原件续推；
// 尚未发出 Add 的先选场、核会话并落原件。最后按当前占用给结论。
func (l *WatchBattleLogic) globalWatch(in *pb.WatchBattleRequest, player uint64) globalWatchResult {
	// 在读取current、清退旧op或选择随机战场之前，占定不可回退的原请求头。
	intent, err := l.svcCtx.Authority.SpectatorIntent(l.ctx, player, in)
	if err != nil {
		return globalWatchFailure(err)
	}
	if intent.State == 5 {
		return closedGlobalWatchResult(intent)
	}
	var original *pb.MatchSpectatorRecord
	if intent.State == 2 {
		original = &pb.MatchSpectatorRecord{}
		if err = proto.Unmarshal(intent.Original, original); err != nil {
			return globalWatchFailure(err)
		}
	} else {
		var stop globalWatchResult
		if original, stop = l.prepareGlobalWatch(in, player); original == nil {
			return stop
		}
	}
	if err = resumeGlobalSpectator(l.ctx, l.svcCtx, original, in.RequestId); err != nil {
		return globalWatchFailure(err)
	}
	current, err := l.svcCtx.Authority.SpectatorForActor(l.ctx, player, in.RequestId)
	if err != nil {
		return globalWatchFailure(err)
	}
	if current == nil || current.OperationId != original.OperationId {
		return globalWatchFailure(errors.New("当前观战占用与原件不符"))
	}
	switch current.State {
	case 2:
		return globalWatchResult{battleID: current.BattleId, verdict: globalWatchVerdict{outcome: "ok"}}
	case 5:
		// battle 拒绝、准入截止或观众已不在：原件已证实关闭，结论读意图上持久化的原因。
		return l.closedGlobalWatch(in, player)
	default:
		return globalWatchFailure(errGlobalSpectatePending)
	}
}

// prepareGlobalWatch 处理尚未发出 Add 的意图：清退旧观战、选场、核会话并落原件。返回 nil 原件时第二个
// 返回值就是本请求结论；能证明的业务拒绝先关闭意图并记下原因，同 request_id 重放得到同一结论。
func (l *WatchBattleLogic) prepareGlobalWatch(in *pb.WatchBattleRequest, player uint64) (*pb.MatchSpectatorRecord, globalWatchResult) {
	s := l.svcCtx
	current, err := s.Authority.SpectatorForActor(l.ctx, player, in.RequestId)
	if err != nil {
		return nil, globalWatchFailure(err)
	}
	if current != nil && current.State != 5 {
		if err = cancelGlobalSpectator(l.ctx, s, current, in.RequestId); err != nil {
			return nil, globalWatchFailure(err)
		}
	}
	battle := in.BattleId
	if battle == 0 {
		// 随机选场只在首次 prepare 固定进原件；之后遇到满员等拒绝也不换场，一个 request_id 只有一份原件。
		if battle, err = pickRandomBattle(s); err != nil {
			return nil, globalWatchFailure(err)
		}
		if battle == 0 {
			return nil, l.closeUnpreparedGlobalWatch(in, player, constants.ErrNoWatchableBattle)
		}
	}
	formation, err := s.Authority.Formation(l.ctx, battle)
	if errors.Is(err, sql.ErrNoRows) || err == nil && formation.State != 4 {
		return nil, l.closeUnpreparedGlobalWatch(in, player, constants.ErrBattleNotWatchable)
	}
	if err != nil {
		return nil, globalWatchFailure(err)
	}
	session, err := loadPlayerSession(s, player)
	if err != nil {
		return nil, globalWatchFailure(err)
	}
	if !isSessionOnline(session) {
		return nil, l.closeUnpreparedGlobalWatch(in, player, constants.ErrSpectateOffline)
	}
	detail, present := ctxkeys.GetSessionDetails(l.ctx)
	if !present || detail == nil {
		return nil, globalWatchFailure(errors.New("观战请求缺少会话元数据"))
	}
	gate, err := strconv.ParseUint(session.GateId, 10, 32)
	if err != nil || gate == 0 {
		// gate_id 是数字节点 id 的字符串形态；解析不了是会话数据损坏（与 Redis 路径同口径），不关闭意图。
		return nil, globalWatchFailure(fmt.Errorf("会话 gate_id 无效 %q", session.GateId))
	}
	if detail.SessionId != session.SessionId || detail.GateInstanceId != session.GateInstanceId || uint32(gate) != detail.GateNodeId {
		// 本请求所在会话已被顶号或重连替换，观众路由不再属于这次请求：关闭意图使结论可重放。
		return nil, l.closeUnpreparedGlobalWatch(in, player, constants.ErrSpectateOffline)
	}
	// 公共显示名只来自角色资料，原登录账号不进入观众广播。
	name := challengePublicName(l.ctx, s, player)
	original, err := s.Authority.BeginSpectator(l.ctx, player, in, formation, &battlepb.AddObserverRequest{
		ObserverName: name, Routing: &battlepb.BattleRouting{SessionId: session.SessionId, GateNodeId: uint32(gate), GateInstanceId: session.GateInstanceId, ZoneId: l.observerZoneId(player)},
	})
	switch {
	case err == nil:
		return original, globalWatchResult{}
	case errors.Is(err, authority.ErrSpectatorFormationClosed):
		// 成局在选场之后已变化；Add 尚未发出，可直接关闭意图。
		return nil, l.closeUnpreparedGlobalWatch(in, player, constants.ErrBattleNotWatchable)
	case errors.Is(err, authority.ErrSpectatorIntentClosed):
		// 意图已被并发关闭（更新的请求或业务退出）：结论读意图上先写的原因。
		return nil, l.closedGlobalWatch(in, player)
	default:
		return nil, globalWatchFailure(err)
	}
}

// closeUnpreparedGlobalWatch 关闭尚未发出 Add 的意图并持久化关闭原因。先写的原因为准：首答回的就是
// 意图上实际保存的原因，与同 request_id 重放一致。未准备原件没有选进原件的战斗，不回 battle_id。
func (l *WatchBattleLogic) closeUnpreparedGlobalWatch(in *pb.WatchBattleRequest, player uint64, closeTip uint32) globalWatchResult {
	stored, err := l.svcCtx.Authority.CloseUnpreparedSpectator(l.ctx, player, in, closeTip)
	if err != nil {
		return globalWatchFailure(err)
	}
	return globalWatchResult{verdict: closedWatchVerdict(stored)}
}

// closedGlobalWatch 重读本请求的意图（同一请求的只读重放），已关闭则回持久化的关闭原因。
func (l *WatchBattleLogic) closedGlobalWatch(in *pb.WatchBattleRequest, player uint64) globalWatchResult {
	intent, err := l.svcCtx.Authority.SpectatorIntent(l.ctx, player, in)
	if err != nil {
		return globalWatchFailure(err)
	}
	if intent.State != 5 {
		return globalWatchFailure(errGlobalSpectatePending)
	}
	return closedGlobalWatchResult(intent)
}

func closedGlobalWatchResult(intent *pb.MatchSpectatorIntentRecord) globalWatchResult {
	original := &pb.MatchSpectatorRecord{}
	if len(intent.Original) != 0 {
		if err := proto.Unmarshal(intent.Original, original); err != nil {
			return globalWatchFailure(err)
		}
	}
	return globalWatchResult{battleID: original.BattleId, verdict: closedWatchVerdict(intent.CloseTip)}
}

func advanceGlobalSpectator(ctx context.Context, s *svc.ServiceContext, original *pb.MatchSpectatorRecord, state uint32, expectedHead string) (bool, error) {
	if expectedHead != "" {
		return s.Authority.AdvanceSpectatorForHead(ctx, original, state, expectedHead)
	}
	return s.Authority.AdvanceSpectator(ctx, original, state)
}

// 调用者已有同业务永久意图绑定的原退出列表；不读取或清退现在的另一个watch。
func closeCapturedSpectatorExit(ctx context.Context, s *svc.ServiceContext, original *pb.MatchSpectatorRecord) error {
	closed, err := s.Authority.SpectatorExitClosed(ctx, original)
	if err != nil {
		return err
	}
	if closed {
		return nil
	}
	return cancelGlobalSpectator(ctx, s, original, "")
}

// 前台传本次新请求head；后台只推进原op。Remove必须固定原截止/实例/op，正证明才关闭SQL。
func cancelGlobalSpectator(ctx context.Context, s *svc.ServiceContext, original *pb.MatchSpectatorRecord, expectedHead string) error {
	if original.State == 5 {
		return nil
	}
	ok, err := advanceGlobalSpectator(ctx, s, original, 3, expectedHead)
	if err != nil {
		return err
	}
	if !ok {
		return errGlobalSpectatePending
	}
	add := &battlepb.AddObserverRequest{}
	if err = proto.Unmarshal(original.AddRequest, add); err != nil {
		return err
	}
	conn, err := discovery.DialEndpoint(original.TargetEndpoint)
	if err != nil {
		return err
	}
	reply, err := battlepb.NewBattleNodeClient(conn).RemoveObserver(ctx, &battlepb.RemoveObserverRequest{
		BattleId: original.BattleId, ObserverPlayerId: original.PlayerId, Reason: "global_original_cancel", SpectatorOperationId: original.OperationId, AdmitBeforeMs: add.AdmitBeforeMs, TargetInstanceId: original.TargetInstanceId,
	})
	if err != nil {
		return err
	}
	if reply == nil || !reply.Cancelled || reply.SpectatorOperationId != original.OperationId || reply.TargetInstanceId != original.TargetInstanceId {
		return errGlobalSpectatePending
	}
	ok, err = advanceGlobalSpectator(ctx, s, original, 5, expectedHead)
	if err != nil {
		return err
	}
	if !ok {
		return errGlobalSpectatePending
	}
	return nil
}

// rejectGlobalSpectator 处理 battle 对原 Add 的明确拒绝或准入截止。拒绝即终态：先在同一事务里转入待取消
// 并记下关闭原因（先写为准），再沿原件取消拿关闭证明；同 op 迟到的 Add 回包不能把待取消翻回准入。
// 取消未确认时原因已落库，前台重试或后台恢复关闭到5后，重放读到的仍是这条原因。
func rejectGlobalSpectator(ctx context.Context, s *svc.ServiceContext, original *pb.MatchSpectatorRecord, expectedHead string, closeTip uint32) error {
	ok, err := s.Authority.RejectSpectator(ctx, original, closeTip, expectedHead)
	if err != nil {
		return err
	}
	if !ok {
		// 原op已证实关闭：结论以意图上先写的原因为准。
		return nil
	}
	return cancelGlobalSpectator(ctx, s, original, expectedHead)
}

func resumeGlobalSpectator(ctx context.Context, s *svc.ServiceContext, original *pb.MatchSpectatorRecord, expectedHead string) error {
	if expectedHead == "" {
		current, superseded, err := s.Authority.SpectatorRecoveryCurrent(ctx, original)
		if err != nil {
			return err
		}
		original = current
		if original.State != 5 && superseded {
			return cancelGlobalSpectator(ctx, s, original, "")
		}
	}
	if original.State == 5 {
		return nil
	}
	add := &battlepb.AddObserverRequest{}
	if err := proto.Unmarshal(original.AddRequest, add); err != nil {
		return err
	}
	if original.State == 3 {
		return cancelGlobalSpectator(ctx, s, original, expectedHead)
	}
	conn, err := discovery.DialEndpoint(original.TargetEndpoint)
	if err != nil {
		return err
	}
	client := battlepb.NewBattleNodeClient(conn)
	if original.State == 2 {
		reply, err := client.QueryObserver(ctx, &battlepb.QueryObserverRequest{BattleId: original.BattleId, ObserverPlayerId: original.PlayerId, SpectatorOperationId: original.OperationId, TargetInstanceId: original.TargetInstanceId})
		if err != nil {
			return err
		}
		if reply == nil || reply.SpectatorOperationId != original.OperationId || reply.TargetInstanceId != original.TargetInstanceId {
			return errGlobalSpectatePending
		}
		switch reply.State {
		case battlepb.ObserverAdmissionState_OBSERVER_ADMISSION_ACTIVE:
			return nil
		case battlepb.ObserverAdmissionState_OBSERVER_ADMISSION_ABSENT:
			return cancelGlobalSpectator(ctx, s, original, expectedHead)
		default:
			return errGlobalSpectatePending
		}
	}
	stamp, err := s.Authority.Now(ctx)
	if err != nil {
		return err
	}
	if stamp >= add.AdmitBeforeMs {
		// 准入截止已过，battle 不会再接纳这份 Add：按拒绝收尾并记下原因。
		return rejectGlobalSpectator(ctx, s, original, expectedHead, constants.ErrBattleNotWatchable)
	}
	reply, err := client.AddObserver(ctx, add)
	if err != nil {
		// ACK未知保留相同Add/截止，不能清SQL占用；UNKNOWN本身也在恢复索引中。
		if _, persistErr := advanceGlobalSpectator(ctx, s, original, 4, expectedHead); persistErr != nil && !errors.Is(persistErr, authority.ErrSpectatorBusy) {
			logx.Error("全局观战ACK未知，原SQL占用仍保留")
		}
		return err
	}
	if reply == nil {
		// 空回包不是 battle 的业务结论：不记原因，只沿原件取消拿关闭证明。
		return cancelGlobalSpectator(ctx, s, original, expectedHead)
	}
	if battleTip := reply.GetErrorMessage().GetId(); battleTip != 0 {
		// 与 Redis 路径共用拒绝码映射，但只取客户端 tip：意图只持久化 close_tip，首答与重放的 outcome 都由它推出。
		// 代价是 outcome 不细分：房间不存在与 battle 的其它拒绝同落 16018，全局路径记 not_found，与 Redis 路径 /
		// Java 版对其它拒绝记 rejected 的口径不同；要细分须给意图另存关闭类别。原始 battle 码只进这条 Info 日志。
		closeTip, _, _ := spectateRejectVerdict(battleTip)
		logx.Infof("全局观战被battle拒绝 battle=%d battle_tip=%d close_tip=%d", original.BattleId, battleTip, closeTip)
		return rejectGlobalSpectator(ctx, s, original, expectedHead, closeTip)
	}
	accepted, err := advanceGlobalSpectator(ctx, s, original, 2, expectedHead)
	if err != nil {
		return err
	}
	if !accepted {
		return errGlobalSpectatePending
	}
	return nil
}

// 固定高水位覆盖旧占用；每状态索引页合并、两在途许可、单操作3秒，未知继续游标。
// 网络调用在SQL事务之外；新输入不会延长当前尾界，也不占撮合工作槽。
func StartGlobalSpectatorRecovery(ctx context.Context, s *svc.ServiceContext) {
	if s.Authority == nil {
		return
	}
	startGlobalDirectAdmissionRecovery(ctx, s)
	startGlobalJoinAdmissionRecovery(ctx, s)
	s.LaunchRelayTask("match.global_spectator_recovery", func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		var after, through uint64
		active := false
		slots := make(chan struct{}, 2)
		var lastWarning time.Time
		var operationWarningMu sync.Mutex
		var lastOperationWarning time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if len(slots) == cap(slots) {
				continue
			}
			work, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			if !active {
				var err error
				through, err = s.Authority.SpectatorRecoveryHighWater(work)
				if err != nil {
					cancel()
					if time.Since(lastWarning) >= 10*time.Second {
						lastWarning = time.Now()
						logx.Error("全局观战恢复高水位读取失败")
					}
					continue
				}
				after, active = 0, true
			}
			rows, err := s.Authority.SpectatorsThrough(work, after, through)
			cancel()
			if err != nil {
				if time.Since(lastWarning) >= 10*time.Second {
					lastWarning = time.Now()
					logx.Error("全局观战恢复读取失败")
				}
				continue
			}
			if len(rows) == 0 {
				active = false
				continue
			}
			for _, row := range rows {
				if len(slots) == cap(slots) {
					break
				}
				slots <- struct{}{}
				after = row.PlayerId
				original := row
				s.LaunchRelayTask("match.global_spectator_operation", func() {
					defer func() { <-slots }()
					work, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
					defer cancel()
					// 原op CAS允许在新head之后收尾旧取消，但绝不推进别的current。
					if err := resumeGlobalSpectator(work, s, original, ""); err != nil {
						s.HoldRelay(err)
						operationWarningMu.Lock()
						if time.Since(lastOperationWarning) >= 10*time.Second {
							lastOperationWarning = time.Now()
							logx.Info("全局观战原件尚未获得终局证明")
						}
						operationWarningMu.Unlock()
					}
				})
			}
		}
	})
}
