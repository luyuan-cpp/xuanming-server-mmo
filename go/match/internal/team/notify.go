package team

import (
	"context"
	"errors"
	"strconv"
	"time"

	"match/generated/pb/game"
	"match/internal/metrics"
	"match/internal/playercontract"
	"match/internal/svc"

	event "proto/common/event"
	kafkapb "proto/contracts/kafka"
	teampb "proto/team"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
	"shared/kafkacmd"
	"shared/safego"
)

// 提交后的副作用:S2C 推送与 scene 刷新信号(设计文档 docs/design/team-system.md §G.3、§F.3)。
//
// 契约:
//   - 全部异步执行(asyncFn),整批一个独立预算 pushBatchBudget,不继承请求 ctx
//     (请求结束即取消,§A.3 第 7 条);失败只记日志与指标,绝不影响 RPC 结果;
//   - 推送至多一次投递(playercontract.PushToPlayer):不在线就不推,客户端靠 GetMyTeam /
//     ListMyInvites 拉取自愈(J-26);
//   - 每份视图的 epoch 只取自提交 / ReadMembers 的同源结果(view.go);
//   - RPC 调用者本人不收快照推送:他的回包里已经是同一次提交构建的视图;
//   - scene 信号只是"去读一下队伍原件",不带权威数据,乱序 / 重复 / 丢失都安全(§B.2);
//   - 落地摘要 bump 也从 publish 派发(syncdigest.go),同一个异步上下文、先于推送,失败同样只记日志与指标。

const (
	// pushBatchBudget 一批推送 + scene 信号的总预算(§G.3)。
	pushBatchBudget = 3 * time.Second

	pushKindSnapshot = "snapshot"
	pushKindInvite   = "invite"
	pushKindEvent    = "event"

	pushOutcomeOK      = "ok"
	pushOutcomeOffline = "offline"
	pushOutcomeError   = "error"

	sceneOutcomeOK         = "ok"
	sceneOutcomeNoLocation = "no_location"
	sceneOutcomeNoNode     = "no_node"
	sceneOutcomeEmptyUuid  = "empty_uuid"
	sceneOutcomeError      = "error"
)

// 测试缝(照 logic.runGatherFn 的包级变量模式,§I.3 #14)。生产值不可在运行期修改。
var (
	// pushFn 下行推送入口。
	pushFn = playercontract.PushToPlayer
	// sceneRefreshFn 把一条组好的 SceneCommand 写进 scene-cmd topic 的目标分区。
	sceneRefreshFn = writeSceneCommand
	// asyncFn 异步副作用派发入口;测试换成同步执行,断言才确定。
	asyncFn = safego.Go
)

// publish 异步执行一批已落盘提交的落地摘要 bump、推送与 scene 信号。
// commits 按落盘顺序传入,调用方交出后不得再修改。
// caller 是 RPC 调用者(后台提交传 0):推送排除他,摘要 bump 包含他(syncdigest.go)。
func (s *Service) publish(caller uint64, commits []*CommitResult) {
	if len(commits) == 0 {
		return
	}
	s.launchRelay("match.team.publish", func() {
		ctx, cancel := context.WithTimeout(context.Background(), pushBatchBudget)
		defer cancel()
		// 摘要先于推送:推送至多一次、丢了靠拉取自愈,而 bump 正是落地后那次拉取的触发条件。
		s.bumpSyncDigest(ctx, caller, commits)
		var refresh []uint64
		for _, c := range commits {
			if c == nil {
				continue
			}
			s.pushCommit(ctx, caller, c)
			// tid 发生变化的人:新加入者与移出者(解散时移出者即全员,§F.3)。
			refresh = append(refresh, c.Decision.Joined...)
			refresh = append(refresh, c.Decision.Left...)
		}
		for _, pid := range uniqueIds(refresh) {
			s.refreshScene(ctx, pid)
		}
	})
}

// publishOnline GetMyTeam{notify_online}:把当前视图推给其他在线队员(MEMBER_ONLINE,version 不变)。
func (s *Service) publishOnline(caller, teamId uint64) {
	s.launchRelay("match.team.online", func() {
		ctx, cancel := context.WithTimeout(context.Background(), pushBatchBudget)
		defer cancel()
		s.pushOnlineRefresh(ctx, caller, teamId)
	})
}

// publishRegroup GetMyTeam{regroup}:队员请求归队。调用者是队员(不是队长)时给他所在的 scene 发一次刷新信号,
// 瞬移 / 换图 / 不动由 scene 按既有跟随规则决定(cpp/libs/services/scene/player/system/player_team.h),这里不判距离。
// 以本次回包视图为准(同一次读出来的),不另读:无队、队长自己、读失败没带视图都不发。
// 频率由 gate 的 MessageLimiter(GetMyTeam 每秒 5 次)兜底,scene 侧每次至多瞬移一次且只在离得远 / 看不见时。
func (s *Service) publishRegroup(caller uint64, team *teampb.TeamView) {
	if team.GetTeamId() == 0 || team.GetLeaderId() == 0 || team.GetLeaderId() == caller {
		return
	}
	teamId, leaderId := team.GetTeamId(), team.GetLeaderId()
	s.launchRelay("match.team.regroup", func() {
		ctx, cancel := context.WithTimeout(context.Background(), pushBatchBudget)
		defer cancel()
		logx.WithContext(ctx).Infof("[team] Regroup requested player=%d team=%d leader=%d", caller, teamId, leaderId)
		s.refreshScene(ctx, caller)
	})
}

// pushCommit 按 Decision 给各接收者推送(§D.5 "推送"列)。
func (s *Service) pushCommit(ctx context.Context, caller uint64, c *CommitResult) {
	d := c.Decision
	recipients := snapshotRecipients(c)
	var dc displayCache
	if d.Record != nil && (len(recipients) > 0 || d.InvitedPlayer != 0) {
		dc = s.presence.loadDisplay(ctx, rosterIds(d.Record))
	}
	zones := s.mergedZones()
	for _, pid := range recipients {
		if pid == caller {
			continue
		}
		view, ok := viewFromCommit(c, pid, dc, zones)
		if !ok {
			continue // 索引已指向别队:不拼视图,交给他自己的拉取
		}
		if rosterChanged(c) {
			s.markFillSupported(view) // 名单变化的提交不做匹配态装饰,理由见 commitResponse
		} else {
			s.decorateQueueState(ctx, pid, view)
		}
		s.push(ctx, pid, uint32(game.ClientPlayerTeamNotifyTeamSnapshotMessageId), pushKindSnapshot,
			&teampb.TeamSnapshotS2C{Team: view, Reason: d.Reason, ActorId: d.Actor})
	}
	if d.InvitedPlayer != 0 {
		if invite := incomingInviteView(d.Record, d.InvitedPlayer, c.NowMs, dc, zones); invite != nil {
			s.push(ctx, d.InvitedPlayer, uint32(game.ClientPlayerTeamNotifyTeamInviteMessageId), pushKindInvite,
				&teampb.TeamInviteS2C{Invite: invite, ServerTimeMs: c.NowMs})
		}
	}
	if d.RejectedApplicant != 0 && d.RejectedApplicant != caller {
		s.push(ctx, d.RejectedApplicant, uint32(game.ClientPlayerTeamNotifyTeamEventMessageId), pushKindEvent,
			&teampb.TeamEventS2C{
				Type:    teampb.TeamEventType_TEAM_EVENT_TYPE_APPLICATION_REJECTED,
				TeamId:  c.TeamId,
				ActorId: d.Record.GetLeaderId(),
			})
	}
	for _, pid := range uniqueIds(d.RevokedInvitees) {
		if pid == caller {
			continue
		}
		s.push(ctx, pid, uint32(game.ClientPlayerTeamNotifyTeamEventMessageId), pushKindEvent,
			&teampb.TeamEventS2C{
				Type:    teampb.TeamEventType_TEAM_EVENT_TYPE_INVITE_REVOKED,
				TeamId:  c.TeamId,
				ActorId: d.Actor,
			})
	}
}

// snapshotRecipients 快照推送的接收者(调用者由 pushCommit 另行排除)。
//   - 建队:无(回包即视图);
//   - 申请 / 邀请增减、过期清理:只推队长(申请与已发邀请只有队长可见);
//     同一次提交若惰性转让了队长,成员都要知道,改推全员;
//   - 其余(加入、离队、踢人、转让、解散、开战状态):J ∪ K ∪ L 全员,
//     移出者收到 team_id=0 的空视图。
func snapshotRecipients(c *CommitResult) []uint64 {
	d := c.Decision
	if !d.LeaderOfflineTransferred {
		switch d.Reason {
		case teampb.TeamChangeReason_TEAM_CHANGE_REASON_CREATED:
			return nil
		case teampb.TeamChangeReason_TEAM_CHANGE_REASON_APPLICATION_CHANGED,
			teampb.TeamChangeReason_TEAM_CHANGE_REASON_INVITE_CHANGED:
			if d.Record == nil {
				return nil
			}
			return []uint64{d.Record.GetLeaderId()}
		}
	}
	all := make([]uint64, 0, len(d.Joined)+len(d.Kept)+len(d.Left))
	all = append(append(append(all, d.Joined...), d.Kept...), d.Left...)
	return uniqueIds(all)
}

// pushOnlineRefresh 用 ReadMembers 给 tid==本队 的其他在线队员推当前视图(§C.5、§I.3 #26)。
func (s *Service) pushOnlineRefresh(ctx context.Context, caller, teamId uint64) {
	snap, err := s.store.ReadMembers(ctx, teamId)
	if err != nil {
		logx.WithContext(ctx).Errorf("[team] Online state refresh failed to read members team=%d: %v", teamId, err)
		metrics.ObserveTeamPush(pushKindSnapshot, pushOutcomeError)
		return
	}
	if snap.Record == nil {
		return
	}
	dc := s.presence.loadDisplay(ctx, rosterIds(snap.Record))
	zones := s.mergedZones()
	for _, pid := range MemberIds(snap.Record) {
		if pid == caller || !dc[pid].Online {
			continue
		}
		view, ok := viewFromMembers(snap, pid, dc, zones)
		if !ok {
			continue // 索引 tid ≠ 本队(已离队 / 索引缺失):不推,交给他自己的拉取自愈
		}
		s.decorateQueueState(ctx, pid, view)
		s.push(ctx, pid, uint32(game.ClientPlayerTeamNotifyTeamSnapshotMessageId), pushKindSnapshot,
			&teampb.TeamSnapshotS2C{
				Team:    view,
				Reason:  teampb.TeamChangeReason_TEAM_CHANGE_REASON_MEMBER_ONLINE,
				ActorId: caller,
			})
	}
}

// push 推一条 S2C 并记 team_push_total;离线不算错误。
func (s *Service) push(ctx context.Context, playerId uint64, messageId uint32, kind string, msg proto.Message) {
	err := pushFn(ctx, s.svcCtx, playerId, messageId, msg)
	switch {
	case err == nil:
		metrics.ObserveTeamPush(kind, pushOutcomeOK)
	case errors.Is(err, playercontract.ErrPlayerOffline):
		metrics.ObserveTeamPush(kind, pushOutcomeOffline)
	default:
		metrics.ObserveTeamPush(kind, pushOutcomeError)
		logx.WithContext(ctx).Errorf("[team] Push failed kind=%s player=%d message_id=%d: %v", kind, playerId, messageId, err)
	}
}

// refreshScene 给玩家所在 scene 节点发 PlayerTeamRefreshEvent(§F.3)。
// 玩家不在场景 → 不发(下次进场会自己拉);节点查不到或 uuid 为空 → 不发(AGENTS §7 #2 fail-closed)。
func (s *Service) refreshScene(ctx context.Context, playerId uint64) {
	log := logx.WithContext(ctx)
	loc, err := playercontract.LoadLocation(ctx, s.svcCtx, playerId)
	if err != nil {
		metrics.ObserveTeamSceneRefresh(sceneOutcomeError)
		log.Errorf("[team] Scene refresh failed to read location player=%d: %v", playerId, err)
		return
	}
	if loc == nil {
		metrics.ObserveTeamSceneRefresh(sceneOutcomeNoLocation)
		return
	}
	if s.svcCtx.SceneNodes == nil {
		metrics.ObserveTeamSceneRefresh(sceneOutcomeNoNode)
		log.Errorf("[team] Scene node discovery uninitialized; abandoning refresh player=%d", playerId)
		return
	}
	entry, err := s.svcCtx.SceneNodes.EntryOf(loc.GetZoneId(), loc.GetNodeId())
	if err != nil {
		metrics.ObserveTeamSceneRefresh(sceneOutcomeNoNode)
		log.Infof("[team] Could not locate scene node; abandoning refresh player=%d: %v", playerId, err)
		return
	}
	if entry.NodeUuid == "" {
		metrics.ObserveTeamSceneRefresh(sceneOutcomeEmptyUuid)
		log.Errorf("[team] Scene node uuid empty; skipping send under zombie-prevention contract player=%d zone=%d node=%d",
			playerId, entry.ZoneId, entry.NodeId)
		return
	}
	payload, err := proto.Marshal(&event.PlayerTeamRefreshEvent{PlayerId: playerId})
	if err != nil {
		metrics.ObserveTeamSceneRefresh(sceneOutcomeError)
		log.Errorf("[team] Failed to serialize PlayerTeamRefreshEvent player=%d: %v", playerId, err)
		return
	}
	cmd := &kafkapb.SceneCommand{
		CommandType:      kafkapb.SceneCommand_DispatchEvent,
		PlayerId:         playerId,
		TargetSceneId:    entry.NodeId,
		Payload:          payload,
		TargetInstanceId: entry.NodeUuid,
		EventId:          proto.Uint32(uint32(game.PlayerTeamRefreshEventEventId)),
	}
	if err := sceneRefreshFn(ctx, s.svcCtx, cmd); err != nil {
		s.svcCtx.HoldRelay(err)
		metrics.ObserveTeamSceneRefresh(sceneOutcomeError)
		log.Errorf("[team] Failed to write scene refresh signal to Kafka player=%d node=%d: %v", playerId, entry.NodeId, err)
		return
	}
	metrics.ObserveTeamSceneRefresh(sceneOutcomeOK)
}

// writeSceneCommand 生产实现:topic = scene-cmd_g<N>,partition = scene_node_id % P(kafkacmd 唯一寻址入口),
// key = player_id(AGENTS §7 #3)。复用 match 的 Kafka writer(CommandPartitionBalancer 遵守显式分区)。
func writeSceneCommand(ctx context.Context, svcCtx *svc.ServiceContext, cmd *kafkapb.SceneCommand) error {
	if svcCtx.Kafka == nil {
		return errors.New("team: Kafka writer 未初始化")
	}
	value, err := proto.Marshal(cmd)
	if err != nil {
		return err
	}
	msg, err := kafkacmd.SceneCommandMessage(
		strconv.FormatUint(uint64(cmd.GetTargetSceneId()), 10),
		strconv.FormatUint(cmd.GetPlayerId(), 10),
		value)
	if err != nil {
		return err
	}
	return svcCtx.Kafka.WriteMessages(ctx, msg)
}

// launchRelay 在异步测试/生产调度器之前登记同一 boot；保留既有 asyncFn 注入接缝。
func (s *Service) launchRelay(point string, fn func()) {
	gate := s.svcCtx.RelayAdmission
	if gate == nil {
		return
	}
	finish, err := gate.Enter()
	if err != nil {
		gate.Hold()
		return
	}
	asyncFn(point, func() {
		defer finish()
		defer func() {
			if recovered := recover(); recovered != nil {
				gate.Hold()
				panic(recovered)
			}
		}()
		fn()
	})
}
