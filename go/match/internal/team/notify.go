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
//   - 每份视图的 epoch 只取自 S_COMMIT / S_READ_MEMBERS 的同源结果(view.go);
//   - RPC 调用者本人不收快照推送:他的回包里已经是同一次提交构建的视图;
//   - scene 信号只是"去重读一下 SharedRedis",不带权威数据,乱序 / 重复 / 丢失都安全(§B.2)。

const (
	// pushBatchBudget 一批推送 + scene 信号的总预算(§G.3)。
	pushBatchBudget = 3 * time.Second

	pushKindSnapshot       = "snapshot"
	pushKindInvite         = "invite"
	pushKindEvent          = "event"
	pushKindMembersChanged = "members_changed"

	pushOutcomeOK      = "ok"
	pushOutcomeOffline = "offline"
	pushOutcomeError   = "error"
	pushOutcomeSkipped = "skipped"

	sceneOutcomeOK         = "ok"
	sceneOutcomeNoLocation = "no_location"
	sceneOutcomeNoNode     = "no_node"
	sceneOutcomeEmptyUuid  = "empty_uuid"
	sceneOutcomeError      = "error"

	healKindOrphanIndex   = "orphan_index"   // S_HEAL_ORPHAN:索引指向已不存在的记录
	healKindIndexMismatch = "index_mismatch" // {-2} 修复提交:保留成员的索引已指向别队
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

// publish 异步执行一批已落盘提交的推送与 scene 信号,另给 healed 里的玩家发 scene 信号。
// commits 按落盘顺序传入(修复提交在前、主提交在后),调用方交出后不得再修改。
func (s *Service) publish(caller uint64, commits []*CommitResult, healed []uint64) {
	if len(commits) == 0 && len(healed) == 0 {
		return
	}
	asyncFn("match.team.publish", func() {
		ctx, cancel := context.WithTimeout(context.Background(), pushBatchBudget)
		defer cancel()
		var refresh []uint64
		for _, c := range commits {
			if c == nil {
				continue
			}
			s.pushCommit(ctx, caller, c)
			// tid 发生变化的人:新加入者与移出者(解散时移出者即全员;修复提交里被移出的成员也发,§F.3)。
			refresh = append(refresh, c.Decision.Joined...)
			refresh = append(refresh, c.Decision.Left...)
		}
		refresh = append(refresh, healed...)
		for _, pid := range uniqueIds(refresh) {
			s.refreshScene(ctx, pid)
		}
	})
}

// publishOnline GetMyTeam{notify_online}:把当前视图推给其他在线队员(MEMBER_ONLINE,version 不变)。
func (s *Service) publishOnline(caller, teamId uint64, knownMembers []uint64) {
	asyncFn("match.team.online", func() {
		ctx, cancel := context.WithTimeout(context.Background(), pushBatchBudget)
		defer cancel()
		s.pushOnlineRefresh(ctx, caller, teamId, knownMembers)
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
	for _, pid := range recipients {
		if pid == caller {
			continue
		}
		view, ok := viewFromCommit(c, pid, dc)
		if !ok {
			continue // 索引已指向别队(修复提交里被移出者):不拼视图,交给他自己的拉取
		}
		s.push(ctx, pid, uint32(game.ClientPlayerTeamNotifyTeamSnapshotMessageId), pushKindSnapshot,
			&teampb.TeamSnapshotS2C{Team: view, Reason: d.Reason, ActorId: d.Actor, Tip: c.PushTip})
	}
	if d.InvitedPlayer != 0 {
		if invite := incomingInviteView(d.Record, d.InvitedPlayer, c.NowMs, dc); invite != nil {
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
//   - 其余(加入、离队、踢人、转让、解散、自愈、开战状态):J ∪ K ∪ L 全员,
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

// pushOnlineRefresh 用 S_READ_MEMBERS 给 tid==本队 的其他在线队员推当前视图(§C.5、§I.3 #26)。
func (s *Service) pushOnlineRefresh(ctx context.Context, caller, teamId uint64, knownMembers []uint64) {
	snap, err := s.store.ReadMembers(ctx, teamId, knownMembers)
	if errors.Is(err, ErrMembersChanged) {
		metrics.ObserveTeamPush(pushKindMembersChanged, pushOutcomeSkipped)
		return
	}
	if err != nil {
		logx.WithContext(ctx).Errorf("[team] 在线态刷新读成员失败 team=%d: %v", teamId, err)
		metrics.ObserveTeamPush(pushKindSnapshot, pushOutcomeError)
		return
	}
	if snap.Record == nil {
		return
	}
	dc := s.presence.loadDisplay(ctx, rosterIds(snap.Record))
	for _, pid := range MemberIds(snap.Record) {
		if pid == caller || !dc[pid].Online {
			continue
		}
		view, ok := viewFromMembers(snap, pid, dc)
		if !ok {
			continue // 索引 tid ≠ 本队(已离队 / 索引缺失):不推,交给他自己的拉取自愈
		}
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
		logx.WithContext(ctx).Errorf("[team] 推送失败 kind=%s player=%d message_id=%d: %v", kind, playerId, messageId, err)
	}
}

// refreshScene 给玩家所在 scene 节点发 PlayerTeamRefreshEvent(§F.3)。
// 玩家不在场景 → 不发(下次进场会自己拉);节点查不到或 uuid 为空 → 不发(AGENTS §7 #2 fail-closed)。
func (s *Service) refreshScene(ctx context.Context, playerId uint64) {
	log := logx.WithContext(ctx)
	loc, err := playercontract.LoadLocation(ctx, s.svcCtx, playerId)
	if err != nil {
		metrics.ObserveTeamSceneRefresh(sceneOutcomeError)
		log.Errorf("[team] scene 刷新读位置失败 player=%d: %v", playerId, err)
		return
	}
	if loc == nil {
		metrics.ObserveTeamSceneRefresh(sceneOutcomeNoLocation)
		return
	}
	if s.svcCtx.SceneNodes == nil {
		metrics.ObserveTeamSceneRefresh(sceneOutcomeNoNode)
		log.Errorf("[team] scene 节点发现未初始化,放弃刷新 player=%d", playerId)
		return
	}
	entry, err := s.svcCtx.SceneNodes.EntryOf(loc.GetZoneId(), loc.GetNodeId())
	if err != nil {
		metrics.ObserveTeamSceneRefresh(sceneOutcomeNoNode)
		log.Infof("[team] scene 节点不可定位,放弃刷新 player=%d: %v", playerId, err)
		return
	}
	if entry.NodeUuid == "" {
		metrics.ObserveTeamSceneRefresh(sceneOutcomeEmptyUuid)
		log.Errorf("[team] scene 节点 uuid 为空,按防僵尸约束不发 player=%d zone=%d node=%d",
			playerId, entry.ZoneId, entry.NodeId)
		return
	}
	payload, err := proto.Marshal(&event.PlayerTeamRefreshEvent{PlayerId: playerId})
	if err != nil {
		metrics.ObserveTeamSceneRefresh(sceneOutcomeError)
		log.Errorf("[team] 序列化 PlayerTeamRefreshEvent 失败 player=%d: %v", playerId, err)
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
		metrics.ObserveTeamSceneRefresh(sceneOutcomeError)
		log.Errorf("[team] scene 刷新信号写 Kafka 失败 player=%d node=%d: %v", playerId, entry.NodeId, err)
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
