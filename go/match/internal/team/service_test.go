package team

// 服务层不经存储的部分(设计文档 docs/design/team-system.md §I.3 #12-#14):home zone 查询 fail-closed、
// 缺 session 拒绝、推送收件人与 scene 信号、归队信号、展示资料。经存储的流程在 sql_store_integration_test.go(真 TiDB)。
//
// 确定性:提交由纯函数规则算出(teamLedger),不起 SQL;会话与位置放 miniredis,时钟固定在 ruleNow;异步副作用经 asyncFn
// 同步执行,推送与 scene 信号由桩(pushFn / sceneRefreshFn)记录。测试之间不并行(包级测试缝)。

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"match/generated/pb/game"
	"match/internal/discovery"
	"match/internal/metrics"
	"match/internal/pkg/ctxkeys"
	"match/internal/playercontract"
	"match/internal/svc"
	"shared/drainproof"
	"shared/playerredis"

	base "proto/common/base"
	event "proto/common/event"
	kafkapb "proto/contracts/kafka"
	dspb "proto/data_service"
	plpb "proto/player_locator"
	smpb "proto/scene_manager"
	teampb "proto/team"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const (
	testZone         uint32 = ruleZone
	testSceneNodeKey        = "SceneNodeService.rpc/zone/1/node_type/3/node_id/1"
	testSceneUuid           = "scene-node-uuid-1"
)

// ---- 桩 ----

type pushRecord struct {
	playerId  uint64
	messageId uint32
	msg       proto.Message
}

type fakeHomeZones struct {
	zones map[uint64]uint32
	err   error
}

func (f *fakeHomeZones) HomeZones(_ context.Context, ids []uint64) (map[uint64]uint32, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[uint64]uint32, len(ids))
	for _, id := range ids {
		if zone, ok := f.zones[id]; ok {
			out[id] = zone
		}
	}
	return out, nil
}

// ---- 夹具 ----

type serviceHarness struct {
	t       *testing.T
	mr      *miniredis.Miniredis
	svcCtx  *svc.ServiceContext
	service *Service
	port    *fakeBattlePort

	mu     sync.Mutex
	pushes []pushRecord
	scenes []*kafkapb.SceneCommand
}

func newServiceHarness(t *testing.T) *serviceHarness {
	t.Helper()
	mr := miniredis.RunT(t)
	mr.SetTime(time.UnixMilli(int64(ruleNow)).UTC())
	rds := redis.MustNewRedis(redis.RedisConf{Host: mr.Addr(), Type: "node"})
	playerRedis, err := playerredis.New(playerredis.Config{Addrs: []string{mr.Addr()}, PoolSizePerNode: 16})
	require.NoError(t, err)
	t.Cleanup(func() { _ = playerRedis.Close() })
	svcCtx := &svc.ServiceContext{
		RelayAdmission: drainproof.New(),
		MatchRedis:     rds,
		PlayerRedis:    playerRedis,
		SceneNodes:     discovery.NewNodeWatcher("scene", svc.SceneNodeRpcPrefix, nil, nil),
	}
	svcCtx.SceneNodes.Upsert(testSceneNodeKey, discovery.NodeEntry{
		NodeId: 1, ZoneId: testZone, NodeUuid: testSceneUuid, Endpoint: "127.0.0.1:1",
	})
	port := &fakeBattlePort{}
	h := &serviceHarness{t: t, mr: mr, svcCtx: svcCtx, port: port}
	h.service = &Service{svcCtx: svcCtx, presence: presenceReader{rds: playerRedis}, starter: port}

	prevPush, prevScene, prevAsync := pushFn, sceneRefreshFn, asyncFn
	pushFn = func(_ context.Context, _ *svc.ServiceContext, playerId uint64, messageId uint32, msg proto.Message) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.pushes = append(h.pushes, pushRecord{playerId: playerId, messageId: messageId, msg: proto.Clone(msg)})
		return nil
	}
	sceneRefreshFn = func(_ context.Context, _ *svc.ServiceContext, cmd *kafkapb.SceneCommand) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.scenes = append(h.scenes, proto.Clone(cmd).(*kafkapb.SceneCommand))
		return nil
	}
	asyncFn = func(_ string, fn func()) { fn() }
	t.Cleanup(func() { pushFn, sceneRefreshFn, asyncFn = prevPush, prevScene, prevAsync })
	return h
}

// addPlayers 铺 ONLINE 会话与场景位置(节点 1)。
func (h *serviceHarness) addPlayers(ids ...uint64) {
	h.t.Helper()
	for _, id := range ids {
		session, err := proto.Marshal(&plpb.PlayerSession{PlayerId: id, State: plpb.PlayerSessionState_SESSION_STATE_ONLINE})
		require.NoError(h.t, err)
		require.NoError(h.t, h.mr.Set(playercontract.SessionKey(id), string(session)))
		location, err := proto.Marshal(&smpb.PlayerLocation{SceneId: 1, NodeId: "1", ZoneId: testZone})
		require.NoError(h.t, err)
		require.NoError(h.t, h.mr.Set(playercontract.LocationKey(id), string(location)))
	}
}

func asPlayer(playerId uint64) context.Context {
	return ctxkeys.WithSessionDetails(context.Background(), &base.SessionDetails{PlayerId: playerId})
}

func (h *serviceHarness) takePushes() []pushRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.pushes
	h.pushes = nil
	return out
}

func (h *serviceHarness) takeScenes() []*kafkapb.SceneCommand {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.scenes
	h.scenes = nil
	return out
}

// requireSceneRefresh 断言这一段操作发出的 scene 信号恰好发给 want,且每条都满足 §F.3 契约。
func (h *serviceHarness) requireSceneRefresh(want ...uint64) {
	h.t.Helper()
	got := make([]uint64, 0, len(want))
	for _, cmd := range h.takeScenes() {
		require.Equal(h.t, kafkapb.SceneCommand_DispatchEvent, cmd.GetCommandType())
		require.Equal(h.t, testSceneUuid, cmd.GetTargetInstanceId(), "target_instance_id 必填(AGENTS §7 #2)")
		require.Equal(h.t, uint32(1), cmd.GetTargetSceneId())
		require.Equal(h.t, uint32(game.PlayerTeamRefreshEventEventId), cmd.GetEventId())
		payload := &event.PlayerTeamRefreshEvent{}
		require.NoError(h.t, proto.Unmarshal(cmd.GetPayload(), payload))
		require.Equal(h.t, cmd.GetPlayerId(), payload.GetPlayerId())
		got = append(got, cmd.GetPlayerId())
	}
	require.ElementsMatch(h.t, want, got)
}

func requireSnapshot(t *testing.T, p pushRecord, to uint64, reason teampb.TeamChangeReason) *teampb.TeamSnapshotS2C {
	t.Helper()
	require.Equal(t, to, p.playerId)
	require.Equal(t, uint32(game.ClientPlayerTeamNotifyTeamSnapshotMessageId), p.messageId)
	snap, ok := p.msg.(*teampb.TeamSnapshotS2C)
	require.True(t, ok, "推送类型 %T", p.msg)
	require.Equal(t, reason, snap.GetReason())
	return snap
}

func requireEvent(t *testing.T, p pushRecord, to uint64, kind teampb.TeamEventType) *teampb.TeamEventS2C {
	t.Helper()
	require.Equal(t, to, p.playerId)
	require.Equal(t, uint32(game.ClientPlayerTeamNotifyTeamEventMessageId), p.messageId)
	ev, ok := p.msg.(*teampb.TeamEventS2C)
	require.True(t, ok, "推送类型 %T", p.msg)
	require.Equal(t, kind, ev.GetType())
	return ev
}

// teamLedger 用纯函数规则算出一连串提交,代替存储层:version 每次提交 +1,进出队的人 epoch 前进,
// 与 sqlTeamStore.commitDecision 同口径。集合契约由 validateCommitSets 把关,和真实提交走同一道校验。
type teamLedger struct {
	t        *testing.T
	rec      *teampb.TeamRecord
	version  uint64
	epochs   map[uint64]uint64
	sessions Sessions
	now      uint64
}

func newTeamLedger(t *testing.T, sessions Sessions) *teamLedger {
	return &teamLedger{t: t, epochs: map[uint64]uint64{}, sessions: sessions, now: ruleNow}
}

func (l *teamLedger) commit(op Op) *CommitResult {
	l.t.Helper()
	if l.rec != nil && FindMember(l.rec, op.Caller) != nil {
		op.CallerTeamId = l.rec.GetTeamId()
	}
	d := Apply(op, l.rec, l.now, l.sessions, RuleConfig{})
	require.Zero(l.t, d.Code)
	require.True(l.t, d.Changed)
	require.NoError(l.t, validateCommitSets(ruleTid, d))
	l.version++
	indexes := make(map[uint64]IndexEntry, len(d.Joined)+len(d.Kept)+len(d.Left))
	for _, pid := range d.Joined {
		l.epochs[pid]++
		indexes[pid] = IndexEntry{TeamId: ruleTid, Epoch: l.epochs[pid]}
	}
	for _, pid := range d.Kept {
		indexes[pid] = IndexEntry{TeamId: ruleTid, Epoch: l.epochs[pid]}
	}
	for _, pid := range d.Left {
		l.epochs[pid]++
		indexes[pid] = IndexEntry{Epoch: l.epochs[pid]}
	}
	l.rec = d.Record
	return &CommitResult{TeamId: ruleTid, Version: l.version, Decision: d, Indexes: indexes, NowMs: l.now}
}

// ---- #12 / #27:home zone 查询 fail-closed ----

func TestHomeZoneOfFailsClosed(t *testing.T) {
	ctx := context.Background()
	zones := &fakeHomeZones{zones: map[uint64]uint32{1: testZone}}
	s := &Service{homeZones: zones}

	zone, code := s.homeZoneOf(ctx, 1)
	require.Zero(t, code)
	require.Equal(t, testZone, zone)

	_, code = s.homeZoneOf(ctx, 2)
	require.Equal(t, ErrHomeZoneUnknown, code, "未映射不是故障,按业务拒绝")

	// 查询失败按故障处理(§I.3 #27)。
	zones.err = status.Error(codes.Unavailable, "connection refused")
	_, code = s.homeZoneOf(ctx, 1)
	require.Equal(t, ErrInternal, code)

	_, code = (&Service{}).homeZoneOf(ctx, 1)
	require.Equal(t, ErrInternal, code, "未注入查询端口按故障处理,不 panic")
}

// 真读路由行的用例在 sql_store_integration_test.go(TestTeamTiDBHomeZoneResolvesMergedZone)。
func TestAuthorityHomeZoneWithoutStoreFailsClosed(t *testing.T) {
	ctx := context.Background()
	_, err := NewAuthorityHomeZone(nil, 0).HomeZones(ctx, []uint64{1})
	require.ErrorIs(t, err, errHomeZoneStoreMissing)

	// 存储没装配:回 Internal,而不是 panic 或拖垮进程。
	_, code := (&Service{homeZones: NewAuthorityHomeZone(nil, 0)}).homeZoneOf(ctx, 1)
	require.Equal(t, ErrInternal, code)
}

// ---- #13:缺 session fail-closed;Notify* 无副作用 ----

// 外壳在进 Service 之前就拒绝,所以这里的 Service 没有存储、端口与任何依赖:一旦越过外壳就会 panic。
func TestMissingSessionFailsClosed(t *testing.T) {
	server := NewServer(&Service{})
	contexts := map[string]context.Context{
		"无 session":   context.Background(),
		"player_id=0": ctxkeys.WithSessionDetails(context.Background(), &base.SessionDetails{}),
	}
	for name, ctx := range contexts {
		calls := map[string]func() (*teampb.TeamResponse, error){
			methodCreateTeam: func() (*teampb.TeamResponse, error) {
				return server.CreateTeam(ctx, &teampb.CreateTeamRequest{})
			},
			methodGetMyTeam: func() (*teampb.TeamResponse, error) {
				return server.GetMyTeam(ctx, &teampb.GetMyTeamRequest{NotifyOnline: true})
			},
			methodApplyJoinTeam: func() (*teampb.TeamResponse, error) {
				return server.ApplyJoinTeam(ctx, &teampb.ApplyJoinTeamRequest{TargetPlayerId: 1})
			},
			methodHandleApplication: func() (*teampb.TeamResponse, error) {
				return server.HandleApplication(ctx, &teampb.HandleApplicationRequest{ApplicantId: 1, Approve: true, ExpectedTeamId: 1})
			},
			methodInviteToTeam: func() (*teampb.TeamResponse, error) {
				return server.InviteToTeam(ctx, &teampb.InviteToTeamRequest{TargetPlayerId: 1, ExpectedTeamId: 1})
			},
			methodRespondInvite: func() (*teampb.TeamResponse, error) {
				return server.RespondInvite(ctx, &teampb.RespondInviteRequest{TeamId: 1, Accept: true})
			},
			methodLeaveTeam: func() (*teampb.TeamResponse, error) {
				return server.LeaveTeam(ctx, &teampb.LeaveTeamRequest{ExpectedTeamId: 1})
			},
			methodKickMember: func() (*teampb.TeamResponse, error) {
				return server.KickMember(ctx, &teampb.KickMemberRequest{TargetPlayerId: 1, ExpectedTeamId: 1})
			},
			methodTransferLeader: func() (*teampb.TeamResponse, error) {
				return server.TransferLeader(ctx, &teampb.TransferLeaderRequest{TargetPlayerId: 1, ExpectedTeamId: 1})
			},
			methodDisbandTeam: func() (*teampb.TeamResponse, error) {
				return server.DisbandTeam(ctx, &teampb.DisbandTeamRequest{ExpectedTeamId: 1})
			},
			methodStartTeamMatch: func() (*teampb.TeamResponse, error) {
				return server.StartTeamMatch(ctx, &teampb.StartTeamMatchRequest{BattleConfigId: 1, ExpectedTeamId: 1})
			},
		}
		for method, call := range calls {
			rejectedBefore := metrics.TeamRPCValue(method, rpcOutcomeNoSession)
			resp, err := call()
			require.NoError(t, err, "%s/%s", name, method)
			require.Equal(t, ErrPlayerId, resp.GetErrorMessage().GetId(), "%s/%s", name, method)
			require.Nil(t, resp.GetTeam(), "%s/%s:没有身份不回任何视图", name, method)
			require.Equal(t, rejectedBefore+1, metrics.TeamRPCValue(method, rpcOutcomeNoSession), "%s/%s", name, method)
		}
		invites, err := server.ListMyInvites(ctx, &teampb.ListMyInvitesRequest{})
		require.NoError(t, err)
		require.Equal(t, ErrPlayerId, invites.GetErrorMessage().GetId(), name)
	}

	// 客户端误调推送占位方法:返回 Empty,不读不写。
	_, err := server.NotifyTeamSnapshot(asPlayer(1), &teampb.TeamSnapshotS2C{Team: &teampb.TeamView{TeamId: 1}})
	require.NoError(t, err)
	_, err = server.NotifyTeamInvite(asPlayer(1), &teampb.TeamInviteS2C{Invite: &teampb.TeamIncomingInviteView{TeamId: 1}})
	require.NoError(t, err)
	_, err = server.NotifyTeamEvent(asPlayer(1), &teampb.TeamEventS2C{TeamId: 1})
	require.NoError(t, err)
}

// ---- #14:推送收件人、reason、队长可见性、各人 epoch、scene 信号 ----

func TestPushAndSceneRecipients(t *testing.T) {
	h := newServiceHarness(t)
	h.addPlayers(1, 2, 3, 4)
	l := newTeamLedger(t, allOnline(1, 2, 3, 4))

	// 建队:回包即视图,不推送;scene 只通知队长。
	h.service.publish(1, []*CommitResult{l.commit(CreateOp(1, ruleTid, testZone))})
	require.Empty(t, h.takePushes())
	h.requireSceneRefresh(1)

	// 申请:只推队长 APPLICATION_CHANGED,队长视图带申请;成员关系没变,不发 scene。
	h.service.publish(2, []*CommitResult{l.commit(ApplyOp(2, testZone))})
	pushes := h.takePushes()
	require.Len(t, pushes, 1)
	snap := requireSnapshot(t, pushes[0], 1, teampb.TeamChangeReason_TEAM_CHANGE_REASON_APPLICATION_CHANGED)
	require.Len(t, snap.GetTeam().GetApplications(), 1)
	require.Equal(t, uint64(2), snap.GetTeam().GetApplications()[0].GetPlayer().GetPlayerId())
	h.requireSceneRefresh()
	h.service.publish(3, []*CommitResult{l.commit(ApplyOp(3, testZone))})
	h.takePushes()

	// 同意 2:新人收 MEMBER_JOINED(队长是调用者,看回包);非队长看不到申请列表,只看到计数;
	// 各人 epoch 取自同一次提交;scene 只通知新人。
	h.service.publish(1, []*CommitResult{l.commit(HandleApplicationOp(1, 2, true))})
	pushes = h.takePushes()
	require.Len(t, pushes, 1)
	snap = requireSnapshot(t, pushes[0], 2, teampb.TeamChangeReason_TEAM_CHANGE_REASON_MEMBER_JOINED)
	require.Equal(t, uint64(2), snap.GetActorId())
	require.Equal(t, ruleTid, snap.GetTeam().GetTeamId())
	require.Empty(t, snap.GetTeam().GetApplications(), "申请只下发给队长")
	require.Empty(t, snap.GetTeam().GetPendingInvites())
	require.Equal(t, uint32(1), snap.GetTeam().GetApplicationCount())
	require.Equal(t, l.epochs[2], snap.GetTeam().GetMembershipEpoch())
	require.Equal(t, l.version, snap.GetTeam().GetVersion())
	require.Len(t, snap.GetTeam().GetMembers(), 2)
	h.requireSceneRefresh(2)

	// 拒绝 3:申请人不在队里、收不到视图,只收 APPLICATION_REJECTED;队长是调用者,不另推快照。
	h.service.publish(1, []*CommitResult{l.commit(HandleApplicationOp(1, 3, false))})
	pushes = h.takePushes()
	require.Len(t, pushes, 1)
	ev := requireEvent(t, pushes[0], 3, teampb.TeamEventType_TEAM_EVENT_TYPE_APPLICATION_REJECTED)
	require.Equal(t, ruleTid, ev.GetTeamId())
	require.Equal(t, uint64(1), ev.GetActorId())
	h.requireSceneRefresh()

	// 邀请 4:被邀请人收 NotifyTeamInvite。
	h.service.publish(1, []*CommitResult{l.commit(InviteOp(1, 4, testZone))})
	pushes = h.takePushes()
	require.Len(t, pushes, 1)
	require.Equal(t, uint64(4), pushes[0].playerId)
	require.Equal(t, uint32(game.ClientPlayerTeamNotifyTeamInviteMessageId), pushes[0].messageId)
	invite, isInvite := pushes[0].msg.(*teampb.TeamInviteS2C)
	require.True(t, isInvite)
	require.Equal(t, ruleTid, invite.GetInvite().GetTeamId())
	require.Equal(t, uint64(1), invite.GetInvite().GetInviter().GetPlayerId())
	require.Equal(t, uint64(1), invite.GetInvite().GetLeaderId())
	require.Equal(t, uint32(2), invite.GetInvite().GetMemberCount())
	require.Equal(t, ruleNow+InviteTTLMs, invite.GetInvite().GetExpireAtMs())
	require.Equal(t, ruleNow, invite.GetServerTimeMs())
	h.requireSceneRefresh()

	// 踢 2:被踢者收 team_id=0 的空视图 MEMBER_KICKED,epoch 为提交后的新值;scene 只通知被踢者。
	h.service.publish(1, []*CommitResult{l.commit(KickOp(1, 2))})
	pushes = h.takePushes()
	require.Len(t, pushes, 1)
	snap = requireSnapshot(t, pushes[0], 2, teampb.TeamChangeReason_TEAM_CHANGE_REASON_MEMBER_KICKED)
	require.Zero(t, snap.GetTeam().GetTeamId())
	require.Zero(t, snap.GetTeam().GetVersion())
	require.Equal(t, l.epochs[2], snap.GetTeam().GetMembershipEpoch())
	h.requireSceneRefresh(2)

	// 解散:未过期邀请的被邀请人收 INVITE_REVOKED;scene 通知全体成员(此时只剩队长)。
	h.service.publish(1, []*CommitResult{l.commit(DisbandOp(1))})
	pushes = h.takePushes()
	require.Len(t, pushes, 1)
	ev = requireEvent(t, pushes[0], 4, teampb.TeamEventType_TEAM_EVENT_TYPE_INVITE_REVOKED)
	require.Equal(t, ruleTid, ev.GetTeamId())
	h.requireSceneRefresh(1)
}

// 冲突次数进 team_commit_retry_total;只有提交了才派发副作用(被拒的写不推送、不发 scene、不 bump)。
func TestMutateEffects(t *testing.T) {
	h := newServiceHarness(t)
	h.addPlayers(1)
	l := newTeamLedger(t, allOnline(1))
	digest := h.withDigestRecorder()

	retries := metrics.TeamCommitRetryValue(methodKickMember)
	h.service.mutateEffects(1, methodKickMember, &MutateResult{Outcome: OutcomeRejected, Code: ErrStateChanged, Conflicts: CommitRetries})
	require.Equal(t, retries+float64(CommitRetries), metrics.TeamCommitRetryValue(methodKickMember))
	require.Empty(t, h.takePushes())
	h.requireSceneRefresh()
	require.Empty(t, digest.take(t))

	h.service.mutateEffects(1, methodCreateTeam, &MutateResult{Outcome: OutcomeCommitted, Commit: l.commit(CreateOp(1, ruleTid, testZone))})
	h.requireSceneRefresh(1)
	require.Equal(t, []uint64{1}, digest.take(t)[domTeam])
}

func TestSceneRefreshFailsClosed(t *testing.T) {
	h := newServiceHarness(t)
	h.addPlayers(1, 2)
	ctx := context.Background()

	// 节点 uuid 为空:不发(AGENTS §7 #2),只记指标。
	h.svcCtx.SceneNodes.Upsert(testSceneNodeKey, discovery.NodeEntry{NodeId: 1, ZoneId: testZone, Endpoint: "127.0.0.1:1"})
	emptyUuid := metrics.TeamSceneRefreshValue(sceneOutcomeEmptyUuid)
	h.service.refreshScene(ctx, 1)
	h.requireSceneRefresh()
	require.Equal(t, emptyUuid+1, metrics.TeamSceneRefreshValue(sceneOutcomeEmptyUuid))

	// 玩家不在场景:不发,下次进场自己拉。
	h.svcCtx.SceneNodes.Upsert(testSceneNodeKey, discovery.NodeEntry{
		NodeId: 1, ZoneId: testZone, NodeUuid: testSceneUuid, Endpoint: "127.0.0.1:1",
	})
	h.mr.Del(playercontract.LocationKey(2))
	noLocation := metrics.TeamSceneRefreshValue(sceneOutcomeNoLocation)
	h.service.refreshScene(ctx, 2)
	h.requireSceneRefresh()
	require.Equal(t, noLocation+1, metrics.TeamSceneRefreshValue(sceneOutcomeNoLocation))

	// 位置与 uuid 都在:发。
	h.service.refreshScene(ctx, 1)
	h.requireSceneRefresh(1)
}

// ---- 组队跟随 v1.1 V.9:GetMyTeam{regroup} 只给请求归队的队员本人发 scene 信号 ----

func TestRegroupSignalsOnlyTheCallingMember(t *testing.T) {
	h := newServiceHarness(t)
	h.addPlayers(1, 2, 9)
	team := &teampb.TeamView{TeamId: ruleTid, LeaderId: 1}

	// 队员请求归队:只给他自己发;不推任何快照(视图没变)。
	h.service.publishRegroup(2, team)
	h.requireSceneRefresh(2)
	require.Empty(t, h.takePushes())

	// 队长自己请求归队:没有可归的队长,不发。
	h.service.publishRegroup(1, team)
	h.requireSceneRefresh()

	// 不在任何队伍里(空视图)或读失败没带视图:不发。
	h.service.publishRegroup(9, &teampb.TeamView{})
	h.requireSceneRefresh()
	h.service.publishRegroup(9, nil)
	h.requireSceneRefresh()
}

// 归队信号照常遵守 §F.3:请求者不在任何场景(没有位置记录)时不发,也不报错。
func TestRegroupWithoutLocationSendsNothing(t *testing.T) {
	h := newServiceHarness(t)
	h.addPlayers(2)
	h.mr.Del(playercontract.LocationKey(2))

	h.service.publishRegroup(2, &teampb.TeamView{TeamId: ruleTid, LeaderId: 1})
	h.requireSceneRefresh()
}

// displayDataService 只实现 BatchGetPlayerDisplay。
type displayDataService struct {
	dspb.DataServiceClient
	players map[uint64]*dspb.PlayerDisplay
	err     error
}

func (f *displayDataService) BatchGetPlayerDisplay(_ context.Context, _ *dspb.BatchGetPlayerDisplayRequest, _ ...grpc.CallOption) (*dspb.BatchGetPlayerDisplayResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &dspb.BatchGetPlayerDisplayResponse{Players: f.players}, nil
}

// 展示资料从 data_service 读。
func TestLoadDisplay_PlayerStoreReadsDataService(t *testing.T) {
	reader := presenceReader{displays: &displayDataService{players: map[uint64]*dspb.PlayerDisplay{
		7: {Name: "青云", Level: 12, ClassId: 3, Gender: 1, AppearanceId: "daoshi"},
	}}}
	display := reader.loadDisplay(context.Background(), []uint64{7, 8})
	got := display[7]
	if got.Name != "青云" || got.Level != 12 || got.ClassId != 3 || got.Gender != 1 || got.AppearanceId != "daoshi" {
		t.Fatalf("display = %+v", got)
	}
	if missing := display[8]; missing.Name != "" || missing.Level != 0 {
		t.Fatalf("a player without a display must stay zero: %+v", missing)
	}
}

// 读失败按全部缺失处理,不让视图失败:同 mget 的失败语义。
func TestLoadDisplay_PlayerStoreFailureLeavesZeroValues(t *testing.T) {
	reader := presenceReader{displays: &displayDataService{err: errors.New("data_service down")}}
	display := reader.loadDisplay(context.Background(), []uint64{7})
	if got := display[7]; got.Name != "" || got.Level != 0 {
		t.Fatalf("display = %+v", got)
	}
}
