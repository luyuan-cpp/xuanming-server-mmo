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
// delivery 与 ticket 只在准入成功时有值：L0 为 DIRECT，L1 / L2 为 LOBBY 并带观战凭证。
type globalWatchResult struct {
	battleID uint64
	verdict  globalWatchVerdict
	cause    error
	delivery pb.ESpectateDelivery
	ticket   []byte
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
	resp := &pb.WatchBattleResponse{BattleId: result.battleID, Delivery: result.delivery, SpectateTicket: result.ticket}
	if result.delivery != pb.ESpectateDelivery_SPECTATE_DELIVERY_LOBBY {
		// L1 / L2 观众在 battle 没有观战原操作,不回操作编号(与 Redis 路径一致)。
		resp.SpectatorOperationId = in.RequestId
	}
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
		return l.closedIntentResult(in, player, intent)
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
		metrics.ObserveSpectateTier(metrics.SpectateTierDirect)
		return globalWatchResult{battleID: current.BattleId, verdict: globalWatchVerdict{outcome: "ok"}, delivery: pb.ESpectateDelivery_SPECTATE_DELIVERY_DIRECT}
	case 5:
		// battle 拒绝、准入截止或观众已不在：原件已证实关闭，结论读意图上持久化的原因；
		// 直连席已满且中继可用时意图是 LOBBY，这次请求接着做 L1 准入。
		return l.closedGlobalWatch(in, player)
	default:
		return globalWatchFailure(errGlobalSpectatePending)
	}
}

// prepareGlobalWatch 处理尚未发出 Add 的意图：清退旧观战、选场、核会话，本场还是 DIRECT 就落原件走 L0；
// 已升档的场做 L1 / L2 准入并以 LOBBY 结案，不写 match_spectator、不锁 match_formation（§15.6）。返回 nil 原件时第二个
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
	target, rejectTip, err := l.locateGlobalWatch(battle, player)
	if err != nil {
		return nil, globalWatchFailure(err)
	}
	if rejectTip != 0 {
		return nil, l.closeUnpreparedGlobalWatch(in, player, rejectTip)
	}
	tier, err := spectateAdmissionTier(l.ctx, s, battle)
	if err != nil {
		return nil, globalWatchFailure(err)
	}
	if tier != battlepb.ESpectateTier_SPECTATE_TIER_DIRECT {
		return nil, l.admitAndCloseGlobalLobby(in, player, target, tier)
	}
	// 公共显示名只来自角色资料，原登录账号不进入观众广播。
	name := challengePublicName(l.ctx, s, player)
	original, err := s.Authority.BeginSpectator(l.ctx, player, in, target.formation, &battlepb.AddObserverRequest{
		ObserverName: name, Routing: &battlepb.BattleRouting{SessionId: target.sessionId, GateNodeId: target.gateNodeId, GateInstanceId: target.gateInstanceId, ZoneId: l.observerZoneId(player)},
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

// globalWatchTarget 是全局观战选定的一场:成局原件,加上观众此刻的会话路由。
type globalWatchTarget struct {
	formation      *pb.MatchFormationRecord
	sessionId      uint32
	gateNodeId     uint32
	gateInstanceId string
}

// locateGlobalWatch 核对目标成局与观众会话(首次 prepare 与 LOBBY 重放共用)。rejectTip≠0 是能证明的业务拒绝:
// 不存在、成局未确认或已中止、私密局同落 16018(与 Redis 路径同一文案,不泄露存在性);会话不在线,或本请求所在会话
// 已被顶号或重连替换(观众路由不再属于这次请求)落 16019。err 是读取故障或会话数据损坏,不算结论。
func (l *WatchBattleLogic) locateGlobalWatch(battle, player uint64) (target globalWatchTarget, rejectTip uint32, err error) {
	s := l.svcCtx
	formation, err := s.Authority.Formation(l.ctx, battle)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !globalFormationWatchable(formation) {
		return target, constants.ErrBattleNotWatchable, nil
	}
	if err != nil {
		return target, 0, err
	}
	session, err := loadPlayerSession(s, player)
	if err != nil {
		return target, 0, err
	}
	if !isSessionOnline(session) {
		return target, constants.ErrSpectateOffline, nil
	}
	detail, present := ctxkeys.GetSessionDetails(l.ctx)
	if !present || detail == nil {
		return target, 0, errors.New("观战请求缺少会话元数据")
	}
	gate, err := strconv.ParseUint(session.GateId, 10, 32)
	if err != nil || gate == 0 {
		// gate_id 是数字节点 id 的字符串形态；解析不了是会话数据损坏（与 Redis 路径同口径），不关闭意图。
		return target, 0, fmt.Errorf("会话 gate_id 无效 %q", session.GateId)
	}
	if detail.SessionId != session.SessionId || detail.GateInstanceId != session.GateInstanceId || uint32(gate) != detail.GateNodeId {
		return target, constants.ErrSpectateOffline, nil
	}
	return globalWatchTarget{formation: formation, sessionId: session.SessionId, gateNodeId: uint32(gate), gateInstanceId: session.GateInstanceId}, 0, nil
}

// admitGlobalLobby 对选定的一场做 L1 / L2 准入(首次与重放共用)。battle 回房间不存在时摘索引(与列表读到已结束的场同口径):
// 成局 state 4 只说明建房成功,结束后不一定立刻翻成 5,这场不能再留在观战列表与随机选场里。
func (l *WatchBattleLogic) admitGlobalLobby(player uint64, target globalWatchTarget, tier battlepb.ESpectateTier) (spectateLobbyAdmission, uint32, error) {
	record, err := spectateRecordFromFormation(target.formation)
	if err != nil {
		return spectateLobbyAdmission{}, 0, err
	}
	viewer := spectateLobbyViewer{playerId: player, gateNodeId: target.gateNodeId, gateInstanceId: target.gateInstanceId}
	admission, err := admitSpectateLobby(l.ctx, l.svcCtx, record, tier, viewer)
	if admission.rejectTip != 0 {
		closeTip, _, roomMissing := spectateRejectVerdict(admission.rejectTip)
		if roomMissing {
			removeSpectateBattle(l.svcCtx, target.formation.GetBattleId())
		}
		l.Infof("全局观战中继准入被battle拒绝 battle=%d battle_tip=%d close_tip=%d", target.formation.GetBattleId(), admission.rejectTip, closeTip)
		return admission, closeTip, nil
	}
	return admission, 0, err
}

// admitAndCloseGlobalLobby 处理首次到达、本场已升档的请求:先准入,成功后才以 LOBBY 结案。顺序不能换:结案后的意图只能走重放,
// 而能证明的拒绝要落成持久原因(同 request_id 重放同结论);battle 或存储故障时意图保持未准备,同 request_id 重试即可。
func (l *WatchBattleLogic) admitAndCloseGlobalLobby(in *pb.WatchBattleRequest, player uint64, target globalWatchTarget, tier battlepb.ESpectateTier) globalWatchResult {
	battle := target.formation.GetBattleId()
	admission, closeTip, err := l.admitGlobalLobby(player, target, tier)
	if err != nil {
		return globalWatchFailure(err)
	}
	if closeTip != 0 {
		return l.closeUnpreparedGlobalWatch(in, player, closeTip)
	}
	stored, err := l.svcCtx.Authority.CloseLobbySpectator(l.ctx, player, in, battle)
	if err != nil {
		return globalWatchFailure(err)
	}
	if stored.GetDelivery() != uint32(pb.ESpectateDelivery_SPECTATE_DELIVERY_LOBBY) {
		// 意图已被并发关闭(业务退出等):结论读意图上先写的原因,这张凭证不发出去。
		return closedGlobalWatchResult(stored)
	}
	return globalLobbyResult(battle, admission)
}

// replayGlobalLobby 重放以 LOBBY 结案的意图:先复核(仍是最新请求、不在排队或战斗中),再重新做幂等的 L1 / L2 准入并签新凭证。
// 客户端大厅重连后靠它拿新凭证,观众所在的 gate 可能已经变了,所以路由取当前会话,目标场取意图上固定的那一场(随机模式不重选)。
// 这里的拒绝不落库:意图已结案,结论随战斗与会话的当前状态变化。
func (l *WatchBattleLogic) replayGlobalLobby(in *pb.WatchBattleRequest, player uint64, intent *pb.MatchSpectatorIntentRecord) globalWatchResult {
	pinned := &pb.MatchSpectatorRecord{}
	if err := proto.Unmarshal(intent.Original, pinned); err != nil {
		return globalWatchFailure(err)
	}
	battle := pinned.BattleId
	if err := l.svcCtx.Authority.RequireLobbyReplay(l.ctx, player, in.RequestId); err != nil {
		return globalWatchFailure(err)
	}
	target, rejectTip, err := l.locateGlobalWatch(battle, player)
	if err != nil {
		return globalWatchFailure(err)
	}
	if rejectTip != 0 {
		return globalWatchResult{battleID: battle, verdict: closedWatchVerdict(rejectTip)}
	}
	tier, err := spectateAdmissionTier(l.ctx, l.svcCtx, battle)
	if err != nil {
		return globalWatchFailure(err)
	}
	if tier == battlepb.ESpectateTier_SPECTATE_TIER_DIRECT {
		// LOBBY 结案证明这场的直连席满过;档位键丢了(Redis 故障切换)就补回来,中继已关(部署回滚)则按满员回。
		if tier, err = spectateDirectFullToRelay(l.ctx, l.svcCtx, battle); err != nil {
			return globalWatchFailure(err)
		}
		if tier == battlepb.ESpectateTier_SPECTATE_TIER_DIRECT {
			return globalWatchResult{battleID: battle, verdict: closedWatchVerdict(constants.ErrSpectateFull)}
		}
	}
	admission, closeTip, err := l.admitGlobalLobby(player, target, tier)
	if err != nil {
		return globalWatchFailure(err)
	}
	if closeTip != 0 {
		return globalWatchResult{battleID: battle, verdict: closedWatchVerdict(closeTip)}
	}
	return globalLobbyResult(battle, admission)
}

func globalLobbyResult(battle uint64, admission spectateLobbyAdmission) globalWatchResult {
	return globalWatchResult{battleID: battle, verdict: globalWatchVerdict{outcome: "lobby"}, delivery: pb.ESpectateDelivery_SPECTATE_DELIVERY_LOBBY, ticket: admission.ticket}
}

// globalFormationWatchable 判定全局成局此刻能否接纳观众:已确认开房(state 4),且建房原件冻结的可见性为
// PUBLIC(formationSpectatePublic)。提交点 BeginSpectator 只在事务里复核 state 与落点、不再读可见性:可见性随
// create_request 冻结(成局 1→2 时写入一次,之后只允许逐字节相同的重写),state 4 时必已落库,这里判过的结论
// 到提交点不会变。
func globalFormationWatchable(formation *pb.MatchFormationRecord) bool {
	return formation.GetState() == 4 && formationSpectatePublic(formation.GetCreateRequest())
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
	return l.closedIntentResult(in, player, intent)
}

// closedIntentResult 给已关闭的意图一个结论:以 LOBBY 结案的重新做准入并签新凭证,其余回持久化的关闭原因。
func (l *WatchBattleLogic) closedIntentResult(in *pb.WatchBattleRequest, player uint64, intent *pb.MatchSpectatorIntentRecord) globalWatchResult {
	if intent.GetDelivery() == uint32(pb.ESpectateDelivery_SPECTATE_DELIVERY_LOBBY) {
		return l.replayGlobalLobby(in, player, intent)
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

// cancelGlobalSpectator 沿原件取消一次观战并关闭到 5。前台传本次新请求 head;后台只推进原 op。Remove 固定原截止 / 实例 / op,
// 优先拿正证明再关闭 SQL。拿不到证明且原实例已不存在(spectatorSourceGoneReason)时不带证明关闭,否则玩家的新观战、排队和组队都会卡死。
// 新观战、排队、组队与后台恢复都走这里,判定只有一份。
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
	if err = removeOriginalObserver(ctx, original, add); err != nil {
		// 实例消失时 RemoveObserver 常常卡满调用方的预算(地址已不通),沿用它走不到下面的 SQL,所以另起预算。
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), spectatorUnprovenCloseBudget)
		defer cancel()
		reason := spectatorSourceGoneReason(closeCtx, s, original)
		if reason == "" {
			return err
		}
		metrics.ObserveSpectatorUnprovenClose(reason)
		logx.Infof("[spectate] Closing global spectator original without cancel proof battle=%d player=%d reason=%s: %v", original.BattleId, original.PlayerId, reason, err)
		ctx = closeCtx
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

// removeOriginalObserver 沿原件向原实例发 RemoveObserver。返回 nil 才是取消证明(判据见 observerCancelProven)。
func removeOriginalObserver(ctx context.Context, original *pb.MatchSpectatorRecord, add *battlepb.AddObserverRequest) error {
	conn, err := discovery.DialEndpoint(original.TargetEndpoint)
	if err != nil {
		return err
	}
	request := &battlepb.RemoveObserverRequest{
		BattleId: original.BattleId, ObserverPlayerId: original.PlayerId, Reason: "global_original_cancel", SpectatorOperationId: original.OperationId, AdmitBeforeMs: add.AdmitBeforeMs, TargetInstanceId: original.TargetInstanceId,
	}
	reply, err := battlepb.NewBattleNodeClient(conn).RemoveObserver(ctx, request)
	if err != nil {
		return err
	}
	if !observerCancelProven(request, reply) {
		return errGlobalSpectatePending
	}
	return nil
}

// spectatorUnprovenCloseBudget 是拿不到取消证明之后,判定原实例已不存在并关闭原件的 SQL 预算。
const spectatorUnprovenCloseBudget = 3 * time.Second

// spectatorSourceGoneReason 判定观战原件指向的 battle 实例是否已不存在,返回 match_spectator_unproven_close_total 的 reason,
// 空串 = 不能判定。任一条件成立即算:
//   - 成局 state 5,或已过成局 deadline_ms + 终局提交宽限:房间必已被 battle 定时器收尾(authority.SpectatorFormationEnded);
//   - 注册表里同 node_id 已是别的 uuid(battleInstanceReplaced,查不到不算)。
//
// 原实例不在了就永远拿不到取消证明:battle 对实例不符的 Remove 回空包,旧地址的 RPC 只会报错。不放宽的话,这名玩家的新观战、
// 排队和组队都要等人工改库。放宽是安全的:观众对战斗状态零写权,不带证明关闭碰不到资产;Add 带 target_instance_id,
// 新实例不接纳旧 op,旧 op 不会复活;实例其实还活着(租约失效的僵尸)时,最坏只是它多发几帧。
// 读成局失败按不能判定处理,下一轮再试。
func spectatorSourceGoneReason(ctx context.Context, s *svc.ServiceContext, original *pb.MatchSpectatorRecord) string {
	formation, err := s.Authority.Formation(ctx, original.BattleId)
	if err != nil {
		return ""
	}
	stamp, err := s.Authority.Now(ctx)
	if err != nil {
		return ""
	}
	if authority.SpectatorFormationEnded(formation, stamp) {
		return metrics.SpectatorUnprovenCloseFormationEnded
	}
	if battleInstanceReplaced(s, formation.GetTargetNodeId(), original.TargetInstanceId) {
		return metrics.SpectatorUnprovenCloseInstanceReplaced
	}
	return ""
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

// rejectGlobalSpectatorToLobby 处理 battle 对原 Add 回「直连席已满」且中继可用的情形:与 rejectGlobalSpectator 同样要拿关闭证明
// (Remove 给这个 op 立封闭墓碑,挡住还在途中的 Add),只是意图上记的不是 16021,而是 LOBBY。取消未确认时结论已落库,
// 前台重试或后台恢复关闭到 5 之后,同 request_id 重放走 L1 / L2 准入。
func rejectGlobalSpectatorToLobby(ctx context.Context, s *svc.ServiceContext, original *pb.MatchSpectatorRecord, expectedHead string) error {
	ok, err := s.Authority.RejectSpectatorToLobby(ctx, original, expectedHead)
	if err != nil {
		return err
	}
	if !ok {
		// 原op已证实关闭：结论以意图上先写的为准。
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
	// 已准入的原件:原实例已不存在时查询永远等不到答案(报错或空包),行就一直停在 2。直接走取消,由它沿同一判定关闭。
	if original.State == 2 && spectatorSourceGoneReason(ctx, s, original) != "" {
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
		closeTip, _, roomMissing := spectateRejectVerdict(battleTip)
		logx.Infof("全局观战被battle拒绝 battle=%d battle_tip=%d close_tip=%d", original.BattleId, battleTip, closeTip)
		if roomMissing {
			// 成局 state 4 只说明建房成功,结束后不一定翻成 5,这场不能再留在观战列表与随机选场里(与 admitGlobalLobby 同口径)。
			removeSpectateBattle(s, original.BattleId)
		}
		if closeTip == constants.ErrSpectateFull && s.Config.Spectate.RelayUsable() {
			// 直连席已满且中继可用:这次请求以 LOBBY 结案、不写 16021,后台恢复走到这里同样如此。先落 SQL 结论再升档:结论落库之后
			// 不会再发这个 op 的 Add。反过来的话,升档失败时行还停在 state 1,重试同 op 的 Add 拿不到首次的满员结论,请求可能被关成 16018。
			// 升档只是让后来的观众不再撞 L0,失败只记日志:重放 LOBBY 意图时会补升(replayGlobalLobby)。
			err := rejectGlobalSpectatorToLobby(ctx, s, original, expectedHead)
			raiseCtx, cancel := spectateBookkeepingContext(ctx)
			defer cancel()
			if _, raiseErr := raiseSpectateTier(raiseCtx, s, original.BattleId, battlepb.ESpectateTier_SPECTATE_TIER_RELAY); raiseErr != nil {
				logx.Errorf("[spectate] Failed to raise tier after direct seats full (replay raises it again) battle=%d: %v", original.BattleId, raiseErr)
			}
			return err
		}
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

// 全局观战的低频维护节拍。两件事都只做幂等删除,每个副本各跑各的,同时跑无害。
const (
	globalSpectateCleanupInterval      = time.Minute
	globalSpectatorIntentPurgeInterval = time.Minute
)

// startGlobalSpectateMaintenance 启动全局模式的两个低频维护循环(本地模式的索引清理在 runMatcherRound 里):
//   - 观战索引:ZSET 成员没有 TTL,没有终局提交就结束的场(实例崩溃)只能靠这里按过期分界摘除;不清的话过期成员越积越多,
//     随机观战最多试 3 次,全落空就把请求永久关成「没有可观战的战斗」;
//   - 意图保留期(globalSpectatorIntentPurge)。
func startGlobalSpectateMaintenance(ctx context.Context, s *svc.ServiceContext, cleanupEvery, purgeEvery time.Duration) {
	s.StartRelayLoop(ctx, "match.global.spectate.cleanup", cleanupEvery, func(context.Context) { cleanupExpiredSpectateIndex(s) })
	purge := &globalSpectatorIntentPurge{}
	s.StartRelayLoop(ctx, "match.global.spectator_intent.purge", purgeEvery, func(ctx context.Context) { purge.round(ctx, s.Authority) })
}

// globalSpectatorIntentPurgeBudget 是一页保留期清理(一次读加一次删)的时间预算,与到期终局恢复同量。
const globalSpectatorIntentPurgeBudget = 3 * time.Second

// globalSpectatorIntentPurgeBatch 是每轮最多处理的行数。一分钟一轮,一天能删约 144 万行;只读索引、按主键删,不长期持锁。
const globalSpectatorIntentPurgeBatch = 1000

// spectatorIntentPurger 是保留期清理要的能力(生产实现 *authority.Store),收窄成接口便于替身测试。
type spectatorIntentPurger interface {
	PurgeClosedSpectatorIntents(ctx context.Context, after authority.SpectatorIntentCursor, limit int) (authority.SpectatorIntentCursor, int64, error)
}

// globalSpectatorIntentPurge 是 match_spectator_intent 保留期清理的循环状态。翻页游标只由这一个循环的 goroutine 读写,
// 不加锁(同 globalOutcomeRecovery);进程重启从头扫一遍即可。
type globalSpectatorIntentPurge struct {
	after authority.SpectatorIntentCursor
}

// round 清一页。失败只记日志、不动游标,下一轮重试:意图保留没有未知结局,不进 HoldRelay。
func (p *globalSpectatorIntentPurge) round(ctx context.Context, purger spectatorIntentPurger) {
	purgeCtx, cancel := context.WithTimeout(ctx, globalSpectatorIntentPurgeBudget)
	defer cancel()
	next, deleted, err := purger.PurgeClosedSpectatorIntents(purgeCtx, p.after, globalSpectatorIntentPurgeBatch)
	if err != nil {
		logx.Errorf("[spectate] Spectator intent retention purge failed; retrying next round: %v", err)
		return
	}
	p.after = next
	metrics.ObserveSpectatorIntentPurged(deleted)
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
