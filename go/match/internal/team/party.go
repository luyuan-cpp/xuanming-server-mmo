package team

import (
	"context"
	"errors"
	"fmt"
	"time"

	"match/generated/pb/game"
	"match/internal/constants"
	"match/internal/metrics"
	"match/internal/playercontract"

	base "proto/common/base"
	plpb "proto/player_locator"
	teampb "proto/team"

	"github.com/zeromicro/go-zero/core/logx"
)

// 队伍补位匹配的组队域部分(设计文档 docs/design/team-backfill-matching.md §4.1 / §4.3 / §4.5)。
//
// 分工:票据与队列在 logic(经 BattleStarter 的 EnqueueParty / CancelParty / PartyQueueState);本文件只做
// 「队伍记录怎么说」这一半 —— 入队编排、名单变化后的撤单、弹组前复核(svc.PartyTeamPort)、
// 视图里的匹配态装饰与推送。
//
// 关键取舍:**队伍记录里不存匹配态**,票据是「是否在排」的唯一事实来源。所以
//   - 补位不加开战锁(开战锁是带截止时间的短时锁,排队可能很久),名单也不冻结:名单一变就整单撤出;
//   - 视图里的 QUEUED 是构建视图时按接收者自己的票据现算的(decorateQueueState),不进记录、不改 version;
//     客户端对同版本视图是「后到者生效」,极端乱序下的旧状态由客户端的排队轮询纠正(设计 §5)。

const (
	// team_match_total{outcome} 的补位取值(其余取值见 service.go)。
	matchOutcomeQueued        = "queued"
	matchOutcomeQueueCanceled = "queue_canceled"

	// partyCancelBudget 名单变化 / 入队回滚时后台撤单的独立预算(不继承请求 ctx,§A.3 第 7 条)。
	partyCancelBudget = 3 * time.Second

	// queueStateSlowThreshold / queueStateBreakerWindow 视图装饰读票的熔断(见 decorateQueueState):
	// 一次读出错或超过阈值,之后这段时间内不再读。
	queueStateSlowThreshold = 300 * time.Millisecond
	queueStateBreakerWindow = 5 * time.Second
)

// startBackfill StartTeamMatch 的补位分支(设计 §4.1 第 2–4 步)。调用前本轮已通过队长 / 开战锁 / 人数校验
// 与逐成员预检;snap 是本轮 S_READ,roster 是按它排出的名单。
//
//  1. EnqueueParty:逐人建 queued 补位票 + 一条 Lua 整单入队;任何一人已有票 → 实现方已回滚,回 MemberNotReady;
//  2. 入队后再读一次成员(S_READ_MEMBERS):集合与入队名单不同 → 静默撤单,回 StateChanged。
//     预检到入队之间的名单变化由这一步接住;这一步之后的变化由提交路径上的 cancelPartyOnRosterChange 接住
//     (那次提交发生在本单票据可见之后,必然读得到);
//  3. 给其余队员推 MATCH_QUEUED;回包是调用者的 QUEUED 视图(与推送同一次 S_READ_MEMBERS)。
func (s *Service) startBackfill(ctx context.Context, caller uint64, configId, targetSize uint32, snap *Snapshot,
	roster []uint64, zones map[uint64]uint32,
) *teampb.TeamResponse {
	log := logx.WithContext(ctx)
	teamId := snap.TeamId
	failed, err := s.starter.EnqueueParty(ctx, configId, targetSize, teamId, roster, zones)
	if failed != 0 {
		return s.matchReject(ctx, caller, ErrMemberNotReady, failed, snap)
	}
	if err != nil {
		log.Errorf("[team] StartTeamMatch failed to enqueue party for backfill player=%d team=%d: %v", caller, teamId, err)
		return s.matchReject(ctx, caller, ErrInternal, 0, nil)
	}

	members, err := s.store.ReadMembers(ctx, teamId, roster)
	switch {
	case errors.Is(err, ErrMembersChanged):
		members = nil
	case err != nil:
		// 复核不了就不留单(fail-closed):名单若真变了,这一单会带着旧名单一直排到被弹组时才发现。
		log.Errorf("[team] StartTeamMatch failed to re-read members after enqueue; withdrawing party player=%d team=%d: %v",
			caller, teamId, err)
		s.cancelPartyInBackground(teamId, roster)
		return s.matchReject(ctx, caller, ErrInternal, 0, nil)
	}
	stale := members == nil || members.Record == nil || !sameIdSet(MemberIds(members.Record), roster)
	if !stale {
		// 与 CheckParty 同口径:名单里每个人的索引都要指向本队(索引丢失后被拉进别队的人,记录里还挂着他)。
		for _, pid := range roster {
			if members.Indexes[pid].TeamId != teamId {
				stale = true
				break
			}
		}
	}
	if stale {
		log.Infof("[team] Roster changed while enqueueing party; withdrawing team=%d roster=%v", teamId, roster)
		s.cancelPartyInBackground(teamId, roster)
		return s.matchReject(ctx, caller, ErrStateChanged, 0, nil)
	}

	metrics.ObserveTeamMatch(matchOutcomeQueued)
	log.Infof("[team] Party queued for backfill team=%d config=%d target=%d roster=%v", teamId, configId, targetSize, roster)
	recipients := make([]uint64, 0, len(roster))
	for _, pid := range roster {
		if pid != caller {
			recipients = append(recipients, pid)
		}
	}
	s.launchRelay("match.team.party_queued", func() {
		pushCtx, cancel := context.WithTimeout(context.Background(), pushBatchBudget)
		defer cancel()
		s.pushMembersSnapshot(pushCtx, members, recipients,
			teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_QUEUED, caller, nil)
	})

	dc := s.presence.loadDisplay(ctx, rosterIds(members.Record))
	view, ok := viewFromMembers(members, caller, dc)
	if !ok {
		return s.respond(ctx, caller, 0, 0)
	}
	s.decorateQueueState(ctx, caller, view)
	return &teampb.TeamResponse{Team: view}
}

// cancelPartyInBackground 入队回滚用的静默撤单(reason = PartyEndNone:不给队员推「匹配已取消」——
// 他们还没收到过「开始匹配」)。独立预算,不继承请求 ctx。
func (s *Service) cancelPartyInBackground(teamId uint64, candidates []uint64) {
	candidates = append([]uint64(nil), candidates...)
	s.launchRelay("match.team.party_cancel", func() {
		ctx, cancel := context.WithTimeout(context.Background(), partyCancelBudget)
		defer cancel()
		if _, err := s.starter.CancelParty(ctx, teamId, candidates, nil, constants.PartyEndNone, 0); err != nil {
			s.svcCtx.HoldRelay(err)
			logx.Errorf("[team] Failed to withdraw party after enqueue rollback (matcher re-check will remove it) team=%d: %v", teamId, err)
		}
	})
}

// rosterChanged 这次提交是否改了成员集合(有人加入或移出;解散时移出者即全员)。
func rosterChanged(c *CommitResult) bool {
	return c != nil && len(c.Decision.Joined)+len(c.Decision.Left) > 0
}

// cancelPartyOnRosterChange 名单变化后的主动撤单(设计 §4.3):提交改了成员集合 → 本队若有在排的补位单,
// 整单撤出并通知(logic 回调 PartyEnded 推 MATCH_QUEUE_CANCELED)。由 publish 对每一条已落盘提交调用;
// 自己起后台任务、用独立预算(partyCancelBudget),不占推送批次的时间。
// candidates 取提交前就在队里的人(Kept ∪ Left):在排的补位单里每个人都持有票据,读到任何一张就能定位整单。
// 没在排时的代价是至多 Capacity 次读票。失败只记日志:漏掉的单由弹组前复核与票据自愈兜底。
func (s *Service) cancelPartyOnRosterChange(c *CommitResult) {
	if s.starter == nil || !rosterChanged(c) {
		return
	}
	d := c.Decision
	candidates := uniqueIds(append(append([]uint64(nil), d.Kept...), d.Left...))
	if len(candidates) == 0 {
		return // 建队:此前没有成员
	}
	// 提交后的成员集合:这次撤单是提交之后才异步执行的,若队长已经按新名单重新入队,那一单的名单正等于它,不能撤。
	var committed []uint64
	if d.Record != nil {
		committed = MemberIds(d.Record)
	}
	teamId := c.TeamId
	s.launchRelay("match.team.party_cancel", func() {
		ctx, cancel := context.WithTimeout(context.Background(), partyCancelBudget)
		defer cancel()
		if _, err := s.starter.CancelParty(ctx, teamId, candidates, committed, constants.PartyEndRosterChanged, 0); err != nil {
			s.svcCtx.HoldRelay(err)
			logx.Errorf("[team] Failed to withdraw party after roster change (matcher re-check will remove it) team=%d: %v", teamId, err)
		}
	})
}

// decorateQueueState 按接收者自己的排队票据给视图补上匹配态(设计 §4.5):补位票 queued → QUEUED + 目标人数 /
// 入队时间 / 副本;已凑满弹出(matched)→ STARTING。开战锁已经让视图是 STARTING 时不动。
// 读票失败按「没在排」处理、只记日志,视图照常下发:队伍视图的可用性不该依赖排队票据所在的库。
// 这份视图因此可能是错的「没在排」—— 客户端不据此退出排队态,而是向 GetQueueStatus 求证(设计 §5 规则 3)。
func (s *Service) decorateQueueState(ctx context.Context, viewer uint64, view *teampb.TeamView) {
	s.markFillSupported(view)
	if s.starter == nil || view == nil || view.GetTeamId() == 0 || viewer == 0 ||
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
// 票据域已接线且应急开关没关。客户端据此决定是否给出目标人数选项 —— 不认识 target_team_size 的服务端
// 会把它静默当成直接开战,所以这一位必须由服务端主动声明,不能让客户端默认认为支持。
// 每一个构建视图的出口都要过这里(改了成员集合的提交不做匹配态装饰,但能力位照标)。
func (s *Service) markFillSupported(view *teampb.TeamView) {
	if view == nil || view.GetTeamId() == 0 {
		return
	}
	view.MatchFillSupported = s.starter != nil && !s.svcCtx.Config.Team.FillQueueDisabled
}

// pushMembersSnapshot 由一次 S_READ_MEMBERS 给 recipients 里仍在本队的人推当前视图(epoch 同源,§C.4)。
// 索引 tid ≠ 本队的人不推(已离队 / 索引缺失),交给他自己的拉取自愈。
func (s *Service) pushMembersSnapshot(ctx context.Context, snap *MembersSnapshot, recipients []uint64,
	reason teampb.TeamChangeReason, actor uint64, tip *base.TipInfoMessage,
) {
	if snap == nil || snap.Record == nil || len(recipients) == 0 {
		return
	}
	dc := s.presence.loadDisplay(ctx, rosterIds(snap.Record))
	for _, pid := range recipients {
		view, ok := viewFromMembers(snap, pid, dc)
		if !ok {
			continue
		}
		s.decorateQueueState(ctx, pid, view)
		s.push(ctx, pid, uint32(game.ClientPlayerTeamNotifyTeamSnapshotMessageId), pushKindSnapshot,
			&teampb.TeamSnapshotS2C{Team: view, Reason: reason, ActorId: actor, Tip: tip})
	}
}

// ---- svc.PartyTeamPort:logic 反过来问 / 通知 team 域(方法签名只用内建类型,原因码见 constants.PartyEnd*) ----

// CheckParty 补位单弹组前 / 回队首前 / 自愈时的复核(设计 §4.2):队伍记录还在、成员集合与名单相等且每个人的
// 索引都指向本队(名单持续在变的 ErrMembersChanged 同样按名单变化)、没有人掉线。返回 (PartyEndNone, 0, nil) = 仍然有效。
// 记录或会话读不到时返回 err:不做失效判定(fail-closed 的方向是「这一单本轮不成组」,不是「把队伍踢出队列」)。
func (s *Service) CheckParty(ctx context.Context, teamId uint64, members []uint64) (uint32, uint64, error) {
	snap, err := s.store.ReadMembers(ctx, teamId, members)
	if errors.Is(err, ErrMembersChanged) {
		return constants.PartyEndRosterChanged, 0, nil
	}
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
	//     单人排队本来就不看会话;宽限期内没连回来的人会在开局时备战失败,那时按「肇事者是本队」整单出局
	//     (logic.requeueFront 的完整性规则)。开战预检要求严格 ONLINE,那是发起时刻的门槛,与这里不同。
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

// PartyStarted 一支补位单凑满并开局成功:给这一单里仍在本队的人推当前视图 + MATCH_ENDED(与直接开战清锁后的
// 推送同一个原因)。此刻票据是 ready,装饰结果是 IDLE。进战斗由 BattleStartS2C 驱动,这条只让面板归位;
// 后台执行、至多一次,丢了由客户端自己的「开战即结束这一次排队」兜底。
func (s *Service) PartyStarted(teamId uint64, members []uint64) {
	metrics.ObserveTeamMatch(matchOutcomeSuccess)
	members = append([]uint64(nil), members...)
	s.launchRelay("match.team.party_started", func() {
		ctx, cancel := context.WithTimeout(context.Background(), pushBatchBudget)
		defer cancel()
		snap, err := s.store.ReadMembers(ctx, teamId, members)
		if errors.Is(err, ErrMembersChanged) {
			metrics.ObserveTeamPush(pushKindMembersChanged, pushOutcomeSkipped)
			return
		}
		if err != nil {
			logx.WithContext(ctx).Errorf("[team] Party start push failed to read members team=%d: %v", teamId, err)
			metrics.ObserveTeamPush(pushKindSnapshot, pushOutcomeError)
			return
		}
		s.pushMembersSnapshot(ctx, snap, members, teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_ENDED, 0, nil)
	})
}

// PartyEnded 一支补位单已被撤出队列(不是成功开战):给这一单里仍在本队的人推当前视图 + MATCH_QUEUE_CANCELED。
// 主动取消时 actor_id = 取消者、不带 tip;其余原因 actor_id = 0、tip 带原因(parameters[0] 可能是相关成员)。
// 后台执行、至多一次投递;丢了由客户端的排队轮询自愈。调用方可能持有凑单锁,这里不能阻塞。
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
		snap, err := s.store.ReadMembers(ctx, teamId, members)
		if errors.Is(err, ErrMembersChanged) {
			metrics.ObserveTeamPush(pushKindMembersChanged, pushOutcomeSkipped)
			return
		}
		if err != nil {
			logx.WithContext(ctx).Errorf("[team] Party queue end push failed to read members team=%d: %v", teamId, err)
			metrics.ObserveTeamPush(pushKindSnapshot, pushOutcomeError)
			return
		}
		s.pushMembersSnapshot(ctx, snap, members,
			teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_QUEUE_CANCELED, actor, tip)
	})
}
