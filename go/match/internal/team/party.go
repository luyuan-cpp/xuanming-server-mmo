package team

import (
	"context"
	"fmt"
	"time"

	"match/generated/pb/game"
	"match/internal/constants"
	"match/internal/metrics"
	"match/internal/playercontract"

	base "proto/common/base"
	matchpb "proto/match"
	plpb "proto/player_locator"
	teampb "proto/team"

	"github.com/zeromicro/go-zero/core/logx"
)

// 整队排队的组队域部分(设计文档 docs/design/team-backfill-matching.md §4.3 / §4.5)。
//
// 分工:票据与队列在 SQL(launch 原件进队列见 sqlTeamStore.createLaunch,名单变化撤单见 commitDecision);本文件只做
// 「队伍记录怎么说」这一半 —— 成组前复核(svc.PartyTeamPort)、视图里的匹配态装饰与推送。
//
// 关键取舍:**队伍记录里不存匹配态**,票据是「是否在排」的唯一事实来源。所以
//   - 视图里的 QUEUED 是构建视图时按接收者自己的票据现算的(decorateQueueState),不进记录、不改 version;
//     客户端对同版本视图是「后到者生效」,极端乱序下的旧状态由客户端的排队轮询纠正(设计 §5)。

const (
	// team_match_total{outcome} 的排队撤出取值(其余取值见 rpcOutcome)。
	matchOutcomeQueueCanceled = "queue_canceled"

	// queueStateSlowThreshold / queueStateBreakerWindow 视图装饰读票的熔断(见 decorateQueueState):
	// 一次读出错或超过阈值,之后这段时间内不再读。
	queueStateSlowThreshold = 300 * time.Millisecond
	queueStateBreakerWindow = 5 * time.Second
)

// rosterChanged 这次提交是否改了成员集合(有人加入或移出;解散时移出者即全员)。
func rosterChanged(c *CommitResult) bool {
	return c != nil && len(c.Decision.Joined)+len(c.Decision.Left) > 0
}

// decorateQueueState 按接收者自己的排队票据给视图补上匹配态(设计 §4.5):补位票 queued → QUEUED + 目标人数 /
// 入队时间 / 副本;已凑满弹出(matched)→ STARTING。开战锁已经让视图是 STARTING 时不动。
// 读票失败按「没在排」处理、只记日志,视图照常下发:队伍视图的可用性不该依赖排队票据所在的库。
// 这份视图因此可能是错的「没在排」—— 客户端不据此退出排队态,而是向 GetQueueStatus 求证(设计 §5 规则 3)。
func (s *Service) decorateQueueState(ctx context.Context, viewer uint64, view *teampb.TeamView) {
	s.markFillSupported(view)
	if view == nil || view.GetTeamId() == 0 || viewer == 0 ||
		view.GetMatchState() != teampb.TeamMatchState_TEAM_MATCH_STATE_IDLE {
		return
	}
	// 熔断:读票出错或明显变慢之后的一小段时间里不再读,一律按没在排下发。装饰读的是排队票据所在的库,
	// 而一批推送(逐个接收者各读一次)与组队 RPC 的回包共用各自那一份时间预算 —— 票据库一慢,
	// 不熔断的话与匹配无关的队伍推送和回包会被它拖到超时。
	// 只认票据库自己的问题,按这一次读自己的耗时判断:
	//   - 调用方的时间预算在读之前就用完了(同一批推送里前面的接收者卡在 Kafka 上、组队请求的预算用尽):不读、不熔断;
	//   - 读得慢(超过阈值):熔断 —— 哪怕调用方的预算因此到期。票据库慢到把调用方拖到期,恰恰是最该熔断的情形
	//     (go-redis 的单次读只受自身 ReadTimeout 约束,不看调用方 ctx);
	//   - 读得不慢、却因为调用方的预算到期而失败:不算票据库的问题,不熔断。
	// 否则一次无关的卡顿会让全实例 5 秒内的视图都丢掉匹配态,或者票据库真慢时永远不熔断。
	if ctx.Err() != nil {
		return
	}
	now := time.Now()
	if now.UnixNano() < s.queueStateBreakerUntil.Load() {
		return
	}
	state, targetSize, configId, queuedAtMs, err := s.starter.PartyQueueState(ctx, viewer, view.GetTeamId())
	elapsed := time.Since(now)
	callerOnly := ctx.Err() != nil && elapsed <= queueStateSlowThreshold
	tripped := !callerOnly && (err != nil || elapsed > queueStateSlowThreshold)
	if tripped {
		s.queueStateBreakerUntil.Store(time.Now().Add(queueStateBreakerWindow).UnixNano())
	}
	if err != nil {
		if tripped {
			logx.WithContext(ctx).Errorf("[team] Failed to read party queue state for view; skipping queue decoration for %s player=%d team=%d elapsed=%s: %v",
				queueStateBreakerWindow, viewer, view.GetTeamId(), elapsed, err)
		} else {
			logx.WithContext(ctx).Infof("[team] Party queue state read abandoned because the caller deadline passed; view sent without queue decoration player=%d team=%d elapsed=%s: %v",
				viewer, view.GetTeamId(), elapsed, err)
		}
		return
	}
	if state == constants.PartyQueueQueued || state == constants.PartyQueueMatched {
		mode, err := s.starter.PartyQueueMode(ctx, viewer, view.GetTeamId())
		if err != nil {
			return
		}
		view.MatchMode = matchpb.MatchMode(mode)
	}
	switch state {
	case constants.PartyQueueQueued:
		view.MatchState = teampb.TeamMatchState_TEAM_MATCH_STATE_QUEUED
		view.MatchTargetSize = targetSize
		view.MatchQueuedAtMs = queuedAtMs
		view.MatchBattleConfigId = configId
	case constants.PartyQueueMatched:
		view.MatchState = teampb.TeamMatchState_TEAM_MATCH_STATE_STARTING
	}
}

// markFillSupported 给有队伍的视图标上「这台服务端支持队伍补位匹配」(TeamView.match_fill_supported):
// 应急开关没关即支持。客户端据此决定是否给出目标人数选项 —— 不认识 target_team_size 的服务端
// 会把它静默当成直接开战,所以这一位必须由服务端主动声明,不能让客户端默认认为支持。
// 每一个构建视图的出口都要过这里(改了成员集合的提交不做匹配态装饰,但能力位照标)。
func (s *Service) markFillSupported(view *teampb.TeamView) {
	if view == nil || view.GetTeamId() == 0 {
		return
	}
	view.MatchFillSupported = !s.svcCtx.Config.Team.FillQueueDisabled
	view.PvpMatchSupported = true
}

// pushMembersSnapshot 由一次 ReadMembers 给 recipients 里仍在本队的人推当前视图(epoch 同源,§C.4)。
// 索引 tid ≠ 本队的人不推(已离队 / 索引缺失),交给他自己的拉取自愈。
func (s *Service) pushMembersSnapshot(ctx context.Context, snap *MembersSnapshot, recipients []uint64,
	reason teampb.TeamChangeReason, actor uint64, tip *base.TipInfoMessage,
) {
	if snap == nil || snap.Record == nil || len(recipients) == 0 {
		return
	}
	dc := s.presence.loadDisplay(ctx, rosterIds(snap.Record))
	zones := s.mergedZones()
	for _, pid := range recipients {
		view, ok := viewFromMembers(snap, pid, dc, zones)
		if !ok {
			continue
		}
		s.decorateQueueState(ctx, pid, view)
		s.push(ctx, pid, uint32(game.ClientPlayerTeamNotifyTeamSnapshotMessageId), pushKindSnapshot,
			&teampb.TeamSnapshotS2C{Team: view, Reason: reason, ActorId: actor, Tip: tip})
	}
}

// ---- svc.PartyTeamPort:logic 反过来问 / 通知 team 域(方法签名只用内建类型,原因码见 constants.PartyEnd*) ----

// CheckParty 整队成局前的复核(设计 §4.2):队伍记录还在、成员集合与名单相等且每个人的
// 索引都指向本队、没有人掉线。返回 (PartyEndNone, 0, nil) = 仍然有效。
// 记录或会话读不到时返回 err:不做失效判定(fail-closed 的方向是「这一单本轮不成组」,不是「把队伍踢出队列」)。
func (s *Service) CheckParty(ctx context.Context, teamId uint64, members []uint64) (uint32, uint64, error) {
	snap, err := s.store.ReadMembers(ctx, teamId)
	if err != nil {
		return constants.PartyEndNone, 0, err
	}
	if snap.Record == nil || !sameIdSet(MemberIds(snap.Record), members) {
		return constants.PartyEndRosterChanged, 0, nil
	}
	for _, pid := range members {
		if snap.Indexes[pid].TeamId != teamId {
			return constants.PartyEndRosterChanged, 0, nil
		}
	}
	// 在线判定。逐人读会话而不是用 presence 的批量读:那个读法把 Redis 错误吞成「未知」,调用方分不清
	// 「读不到」和「不在线」,一次抖动就会把在线的整队判成掉线。
	//   - 读失败 / 解析失败 → 返回 err,不做判定;
	//   - 会话 key 不存在,或状态明确是 OFFLINE → 掉线,撤单;
	//   - ONLINE,以及断线等重连的宽限期(DISCONNECTING,player_locator 的 30s 租约)→ 放行。
	//     单人排队本来就不看会话;宽限期内没连回来的人会在开局时备战失败,那时按「肇事者是本队」整单出局。
	//     开战预检要求严格 ONLINE,那是发起时刻的门槛,与这里不同。
	for _, pid := range members {
		session, err := playercontract.LoadSession(ctx, s.svcCtx, pid)
		if err != nil {
			return constants.PartyEndNone, 0, fmt.Errorf("read session player=%d: %w", pid, err)
		}
		if session == nil || session.GetState() == plpb.PlayerSessionState_SESSION_STATE_OFFLINE {
			return constants.PartyEndMemberOffline, pid, nil
		}
	}
	return constants.PartyEndNone, 0, nil
}

// PartyEnded 一支补位单已被撤出队列(不是成功开战):给这一单里仍在本队的人推当前视图 + MATCH_QUEUE_CANCELED。
// 主动取消时 actor_id = 取消者、不带 tip;其余原因 actor_id = 0、tip 带原因(parameters[0] 可能是相关成员)。
// 后台执行、至多一次投递;丢了由客户端的排队轮询自愈。
func (s *Service) PartyEnded(teamId uint64, members []uint64, reason uint32, culprit uint64) {
	metrics.ObserveTeamMatch(matchOutcomeQueueCanceled)
	actor := uint64(0)
	var tip *base.TipInfoMessage
	switch reason {
	case constants.PartyEndCanceled:
		actor = culprit
	case constants.PartyEndMemberOffline:
		tip = tipOf(ErrMemberOffline, culprit)
	case constants.PartyEndMemberNotReady:
		tip = tipOf(ErrMemberNotReady, culprit)
	default:
		tip = tipOf(ErrStateChanged, 0)
	}
	members = append([]uint64(nil), members...)
	s.launchRelay("match.team.party_ended", func() {
		ctx, cancel := context.WithTimeout(context.Background(), pushBatchBudget)
		defer cancel()
		snap, err := s.store.ReadMembers(ctx, teamId)
		if err != nil {
			logx.WithContext(ctx).Errorf("[team] Party queue end push failed to read members team=%d: %v", teamId, err)
			metrics.ObserveTeamPush(pushKindSnapshot, pushOutcomeError)
			return
		}
		s.pushMembersSnapshot(ctx, snap, members,
			teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_QUEUE_CANCELED, actor, tip)
	})
}
