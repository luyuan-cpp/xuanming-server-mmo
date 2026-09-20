package team

// 服务层测试(设计文档 docs/design/team-system.md §I.3 #12-#15、#23-#27)。
//
// 确定性:miniredis 时钟固定在 storeBase(store_test.go),不读 Go 墙钟;异步副作用经 asyncFn
// 同步执行,推送与 scene 信号由桩(pushFn / sceneRefreshFn)记录;并发时序用 afterReadHook /
// beforeInvitePruneHook 在固定点插入。测试之间不并行(包级测试缝)。

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"match/generated/pb/game"
	"match/internal/config"
	"match/internal/discovery"
	"match/internal/metrics"
	"match/internal/pkg/ctxkeys"
	"match/internal/playercontract"
	"match/internal/svc"

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
	testZone         uint32 = 1
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

// fakeDataService 只实现 BatchGetPlayerHomeZone;嵌入的接口为 nil,误调其它方法会直接 panic。
type fakeDataService struct {
	dspb.DataServiceClient
	zones map[uint64]uint32
	err   error
}

func (f *fakeDataService) BatchGetPlayerHomeZone(_ context.Context, in *dspb.BatchGetPlayerHomeZoneRequest, _ ...grpc.CallOption) (*dspb.BatchGetPlayerHomeZoneResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := map[uint64]uint32{}
	for _, id := range in.GetPlayerIds() {
		if zone, ok := f.zones[id]; ok {
			out[id] = zone
		}
	}
	return &dspb.BatchGetPlayerHomeZoneResponse{PlayerZoneMap: out}, nil
}

type seqIds struct {
	mu   sync.Mutex
	next uint64
}

func (g *seqIds) Generate() (uint64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.next++
	return g.next, nil
}

// ---- 夹具 ----

type serviceHarness struct {
	t       *testing.T
	mr      *miniredis.Miniredis
	svcCtx  *svc.ServiceContext
	service *Service
	server  *Server
	zones   *fakeHomeZones

	mu     sync.Mutex
	pushes []pushRecord
	scenes []*kafkapb.SceneCommand
}

func newServiceHarness(t *testing.T, teamConf config.TeamConf) *serviceHarness {
	t.Helper()
	mr := miniredis.RunT(t)
	mr.SetTime(storeBase)
	rds := redis.MustNewRedis(redis.RedisConf{Host: mr.Addr(), Type: "node"})
	svcCtx := &svc.ServiceContext{
		Config:      config.Config{Team: teamConf},
		MatchRedis:  rds,
		SharedRedis: rds,
		Redis:       rds,
		SceneNodes:  discovery.NewNodeWatcher("scene", svc.SceneNodeRpcPrefix, nil, nil),
	}
	svcCtx.SceneNodes.Upsert(testSceneNodeKey, discovery.NodeEntry{
		NodeId: 1, ZoneId: testZone, NodeUuid: testSceneUuid, Endpoint: "127.0.0.1:1",
	})
	h := &serviceHarness{t: t, mr: mr, svcCtx: svcCtx, zones: &fakeHomeZones{zones: map[uint64]uint32{}}}
	h.service = NewService(svcCtx, h.zones, nil)
	h.service.ids = &seqIds{next: 9_000_000}
	h.server = NewServer(h.service)

	prevPush, prevScene, prevAsync := pushFn, sceneRefreshFn, asyncFn
	prevRead, prevPrune := afterReadHook, beforeInvitePruneHook
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
	t.Cleanup(func() {
		pushFn, sceneRefreshFn, asyncFn = prevPush, prevScene, prevAsync
		afterReadHook, beforeInvitePruneHook = prevRead, prevPrune
	})
	return h
}

// addPlayers 铺 ONLINE 会话、场景位置(节点 1)与 home zone。
func (h *serviceHarness) addPlayers(zone uint32, ids ...uint64) {
	h.t.Helper()
	for _, id := range ids {
		session, err := proto.Marshal(&plpb.PlayerSession{PlayerId: id, State: plpb.PlayerSessionState_SESSION_STATE_ONLINE})
		require.NoError(h.t, err)
		require.NoError(h.t, h.mr.Set(playercontract.SessionKey(id), string(session)))
		location, err := proto.Marshal(&smpb.PlayerLocation{SceneId: 1, NodeId: "1", ZoneId: zone})
		require.NoError(h.t, err)
		require.NoError(h.t, h.mr.Set(playercontract.LocationKey(id), string(location)))
		h.zones.zones[id] = zone
	}
}

func asPlayer(playerId uint64) context.Context {
	return ctxkeys.WithSessionDetails(context.Background(), &base.SessionDetails{PlayerId: playerId})
}

func (h *serviceHarness) ok(resp *teampb.TeamResponse, err error) *teampb.TeamResponse {
	h.t.Helper()
	require.NoError(h.t, err)
	require.NotNil(h.t, resp)
	return resp
}

func requireCode(t *testing.T, want uint32, resp *teampb.TeamResponse) {
	t.Helper()
	require.Equal(t, want, resp.GetErrorMessage().GetId(), "resp=%v", resp)
}

func (h *serviceHarness) create(leader uint64) uint64 {
	h.t.Helper()
	resp := h.ok(h.server.CreateTeam(asPlayer(leader), &teampb.CreateTeamRequest{}))
	requireCode(h.t, 0, resp)
	require.NotZero(h.t, resp.GetTeam().GetTeamId())
	return resp.GetTeam().GetTeamId()
}

func (h *serviceHarness) apply(applicant, target uint64) *teampb.TeamResponse {
	h.t.Helper()
	return h.ok(h.server.ApplyJoinTeam(asPlayer(applicant), &teampb.ApplyJoinTeamRequest{TargetPlayerId: target}))
}

// join 走真实路径入队:申请 → 队长同意。
func (h *serviceHarness) join(leader, teamId, playerId uint64) {
	h.t.Helper()
	requireCode(h.t, 0, h.apply(playerId, leader))
	requireCode(h.t, 0, h.ok(h.server.HandleApplication(asPlayer(leader),
		&teampb.HandleApplicationRequest{ApplicantId: playerId, Approve: true, ExpectedTeamId: teamId})))
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

func (h *serviceHarness) epochOf(playerId uint64) uint64 {
	return parseUint(h.mr.HGet(playerIndexKey(playerId), indexFieldEpoch))
}

func (h *serviceHarness) versionOf(teamId uint64) uint64 {
	return parseUint(h.mr.HGet(recordKey(teamId), recFieldVer))
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

func requireNoTeamKeys(t *testing.T, mr *miniredis.Miniredis) {
	t.Helper()
	for _, key := range mr.Keys() {
		require.False(t, strings.HasPrefix(key, "team:"), "不应写入组队 key: %s", key)
	}
}

// ---- #12 / #27:home zone 查询 fail-closed ----

func TestHomeZoneMissingFailsClosed(t *testing.T) {
	h := newServiceHarness(t, config.TeamConf{})
	h.addPlayers(testZone, 1)
	delete(h.zones.zones, 1)

	resp := h.ok(h.server.CreateTeam(asPlayer(1), &teampb.CreateTeamRequest{}))
	requireCode(t, ErrHomeZoneUnknown, resp)
	require.Zero(t, resp.GetTeam().GetTeamId())
	requireNoTeamKeys(t, h.mr)
}

func TestHomeZoneLookupErrorIsInternal(t *testing.T) {
	h := newServiceHarness(t, config.TeamConf{})
	h.addPlayers(testZone, 1)
	// NonBlock 客户端在 data_service 未起时返回 Unavailable(§I.3 #27)。
	h.zones.err = status.Error(codes.Unavailable, "connection refused")

	requireCode(t, ErrInternal, h.ok(h.server.CreateTeam(asPlayer(1), &teampb.CreateTeamRequest{})))
	requireNoTeamKeys(t, h.mr)
}

func TestDataServiceHomeZone(t *testing.T) {
	ctx := context.Background()

	lookup := NewDataServiceHomeZone(&fakeDataService{zones: map[uint64]uint32{1: 3}}, 0)
	zones, err := lookup.HomeZones(ctx, []uint64{1, 2})
	require.NoError(t, err)
	require.Equal(t, uint32(3), zones[1])
	_, mapped := zones[2]
	require.False(t, mapped, "batch 回包缺项 = 未映射,原样交给调用方")

	down := NewDataServiceHomeZone(&fakeDataService{err: status.Error(codes.Unavailable, "down")}, 0)
	_, err = down.HomeZones(ctx, []uint64{1})
	require.Error(t, err)
	require.Equal(t, codes.Unavailable, status.Code(err))

	_, err = NewDataServiceHomeZone(nil, 0).HomeZones(ctx, []uint64{1})
	require.ErrorIs(t, err, errHomeZoneClientMissing)

	// DataServiceRpc 未配置(客户端为 nil):建队回 Internal,而不是 panic 或拖垮进程。
	h := newServiceHarness(t, config.TeamConf{})
	h.addPlayers(testZone, 1)
	h.service.homeZones = NewDataServiceHomeZone(nil, 0)
	requireCode(t, ErrInternal, h.ok(h.server.CreateTeam(asPlayer(1), &teampb.CreateTeamRequest{})))
	requireNoTeamKeys(t, h.mr)
}

func TestApplyAndInviteHomeZoneFailClosed(t *testing.T) {
	h := newServiceHarness(t, config.TeamConf{})
	h.addPlayers(testZone, 1, 2, 3)
	tid := h.create(1)

	delete(h.zones.zones, 2)
	requireCode(t, ErrHomeZoneUnknown, h.apply(2, 1))

	delete(h.zones.zones, 3)
	requireCode(t, ErrHomeZoneUnknown, h.ok(h.server.InviteToTeam(asPlayer(1),
		&teampb.InviteToTeamRequest{TargetPlayerId: 3, ExpectedTeamId: tid})))

	rec := loadRecord(t, h.mr, tid)
	require.Empty(t, rec.GetApplications())
	require.Empty(t, rec.GetInvites())
}

// ---- #13:缺 session fail-closed;Notify* 无副作用 ----

func TestMissingSessionFailsClosed(t *testing.T) {
	h := newServiceHarness(t, config.TeamConf{})
	h.addPlayers(testZone, 1)
	before := h.mr.Dump()

	contexts := map[string]context.Context{
		"无 session":   context.Background(),
		"player_id=0": ctxkeys.WithSessionDetails(context.Background(), &base.SessionDetails{}),
	}
	for name, ctx := range contexts {
		calls := map[string]func() (*teampb.TeamResponse, error){
			methodCreateTeam: func() (*teampb.TeamResponse, error) {
				return h.server.CreateTeam(ctx, &teampb.CreateTeamRequest{})
			},
			methodGetMyTeam: func() (*teampb.TeamResponse, error) {
				return h.server.GetMyTeam(ctx, &teampb.GetMyTeamRequest{NotifyOnline: true})
			},
			methodApplyJoinTeam: func() (*teampb.TeamResponse, error) {
				return h.server.ApplyJoinTeam(ctx, &teampb.ApplyJoinTeamRequest{TargetPlayerId: 1})
			},
			methodHandleApplication: func() (*teampb.TeamResponse, error) {
				return h.server.HandleApplication(ctx, &teampb.HandleApplicationRequest{ApplicantId: 1, Approve: true, ExpectedTeamId: 1})
			},
			methodInviteToTeam: func() (*teampb.TeamResponse, error) {
				return h.server.InviteToTeam(ctx, &teampb.InviteToTeamRequest{TargetPlayerId: 1, ExpectedTeamId: 1})
			},
			methodRespondInvite: func() (*teampb.TeamResponse, error) {
				return h.server.RespondInvite(ctx, &teampb.RespondInviteRequest{TeamId: 1, Accept: true})
			},
			methodLeaveTeam: func() (*teampb.TeamResponse, error) {
				return h.server.LeaveTeam(ctx, &teampb.LeaveTeamRequest{ExpectedTeamId: 1})
			},
			methodKickMember: func() (*teampb.TeamResponse, error) {
				return h.server.KickMember(ctx, &teampb.KickMemberRequest{TargetPlayerId: 1, ExpectedTeamId: 1})
			},
			methodTransferLeader: func() (*teampb.TeamResponse, error) {
				return h.server.TransferLeader(ctx, &teampb.TransferLeaderRequest{TargetPlayerId: 1, ExpectedTeamId: 1})
			},
			methodDisbandTeam: func() (*teampb.TeamResponse, error) {
				return h.server.DisbandTeam(ctx, &teampb.DisbandTeamRequest{ExpectedTeamId: 1})
			},
			methodStartTeamMatch: func() (*teampb.TeamResponse, error) {
				return h.server.StartTeamMatch(ctx, &teampb.StartTeamMatchRequest{BattleConfigId: 1, ExpectedTeamId: 1})
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
		invites, err := h.server.ListMyInvites(ctx, &teampb.ListMyInvitesRequest{})
		require.NoError(t, err)
		require.Equal(t, ErrPlayerId, invites.GetErrorMessage().GetId(), name)
	}

	// 客户端误调推送占位方法:返回 Empty,不读不写。
	_, err := h.server.NotifyTeamSnapshot(asPlayer(1), &teampb.TeamSnapshotS2C{Team: &teampb.TeamView{TeamId: 1}})
	require.NoError(t, err)
	_, err = h.server.NotifyTeamInvite(asPlayer(1), &teampb.TeamInviteS2C{Invite: &teampb.TeamIncomingInviteView{TeamId: 1}})
	require.NoError(t, err)
	_, err = h.server.NotifyTeamEvent(asPlayer(1), &teampb.TeamEventS2C{TeamId: 1})
	require.NoError(t, err)

	require.Equal(t, before, h.mr.Dump())
	require.Empty(t, h.takePushes())
	require.Empty(t, h.takeScenes())
}

// ---- #14:推送收件人、reason、队长可见性、各人 epoch、scene 信号 ----

func TestPushAndSceneRecipients(t *testing.T) {
	h := newServiceHarness(t, config.TeamConf{})
	h.addPlayers(testZone, 1, 2, 3, 4)

	// 建队:回包即视图,不推送;scene 只通知队长。
	tid := h.create(1)
	require.Empty(t, h.takePushes())
	h.requireSceneRefresh(1)

	// 申请:只推队长 APPLICATION_CHANGED,队长视图带申请;成员关系没变,不发 scene。
	requireCode(t, 0, h.apply(2, 1))
	pushes := h.takePushes()
	require.Len(t, pushes, 1)
	snap := requireSnapshot(t, pushes[0], 1, teampb.TeamChangeReason_TEAM_CHANGE_REASON_APPLICATION_CHANGED)
	require.Len(t, snap.GetTeam().GetApplications(), 1)
	require.Equal(t, uint64(2), snap.GetTeam().GetApplications()[0].GetPlayer().GetPlayerId())
	h.requireSceneRefresh()
	requireCode(t, 0, h.apply(3, 1))
	h.takePushes()

	// 同意 2:新人收 MEMBER_JOINED(队长是调用者,看回包);非队长看不到申请列表,只看到计数;
	// 各人 epoch 取自同一次提交;scene 只通知新人。
	resp := h.ok(h.server.HandleApplication(asPlayer(1),
		&teampb.HandleApplicationRequest{ApplicantId: 2, Approve: true, ExpectedTeamId: tid}))
	requireCode(t, 0, resp)
	require.Len(t, resp.GetTeam().GetApplications(), 1, "队长回包带剩余申请")
	require.Equal(t, h.epochOf(1), resp.GetTeam().GetMembershipEpoch())
	require.Equal(t, h.versionOf(tid), resp.GetTeam().GetVersion())
	pushes = h.takePushes()
	require.Len(t, pushes, 1)
	snap = requireSnapshot(t, pushes[0], 2, teampb.TeamChangeReason_TEAM_CHANGE_REASON_MEMBER_JOINED)
	require.Equal(t, uint64(2), snap.GetActorId())
	require.Equal(t, tid, snap.GetTeam().GetTeamId())
	require.Empty(t, snap.GetTeam().GetApplications(), "申请只下发给队长")
	require.Empty(t, snap.GetTeam().GetPendingInvites())
	require.Equal(t, uint32(1), snap.GetTeam().GetApplicationCount())
	require.Equal(t, h.epochOf(2), snap.GetTeam().GetMembershipEpoch())
	require.Equal(t, h.versionOf(tid), snap.GetTeam().GetVersion())
	require.Len(t, snap.GetTeam().GetMembers(), 2)
	h.requireSceneRefresh(2)

	// 拒绝 3:申请人不在队里、收不到视图,只收 APPLICATION_REJECTED;队长是调用者,不另推快照。
	requireCode(t, 0, h.ok(h.server.HandleApplication(asPlayer(1),
		&teampb.HandleApplicationRequest{ApplicantId: 3, Approve: false, ExpectedTeamId: tid})))
	pushes = h.takePushes()
	require.Len(t, pushes, 1)
	ev := requireEvent(t, pushes[0], 3, teampb.TeamEventType_TEAM_EVENT_TYPE_APPLICATION_REJECTED)
	require.Equal(t, tid, ev.GetTeamId())
	require.Equal(t, uint64(1), ev.GetActorId())
	h.requireSceneRefresh()

	// 邀请 4:被邀请人收 NotifyTeamInvite;队长回包带已发出的邀请。
	resp = h.ok(h.server.InviteToTeam(asPlayer(1), &teampb.InviteToTeamRequest{TargetPlayerId: 4, ExpectedTeamId: tid}))
	requireCode(t, 0, resp)
	require.Len(t, resp.GetTeam().GetPendingInvites(), 1)
	pushes = h.takePushes()
	require.Len(t, pushes, 1)
	require.Equal(t, uint64(4), pushes[0].playerId)
	require.Equal(t, uint32(game.ClientPlayerTeamNotifyTeamInviteMessageId), pushes[0].messageId)
	invite, isInvite := pushes[0].msg.(*teampb.TeamInviteS2C)
	require.True(t, isInvite)
	require.Equal(t, tid, invite.GetInvite().GetTeamId())
	require.Equal(t, uint64(1), invite.GetInvite().GetInviter().GetPlayerId())
	require.Equal(t, uint64(1), invite.GetInvite().GetLeaderId())
	require.Equal(t, uint32(2), invite.GetInvite().GetMemberCount())
	require.Equal(t, baseMs()+InviteTTLMs, invite.GetInvite().GetExpireAtMs())
	require.Equal(t, baseMs(), invite.GetServerTimeMs())
	h.requireSceneRefresh()

	// 踢 2:被踢者收 team_id=0 的空视图 MEMBER_KICKED,epoch 为提交后的新值;scene 只通知被踢者。
	requireCode(t, 0, h.ok(h.server.KickMember(asPlayer(1), &teampb.KickMemberRequest{TargetPlayerId: 2, ExpectedTeamId: tid})))
	pushes = h.takePushes()
	require.Len(t, pushes, 1)
	snap = requireSnapshot(t, pushes[0], 2, teampb.TeamChangeReason_TEAM_CHANGE_REASON_MEMBER_KICKED)
	require.Zero(t, snap.GetTeam().GetTeamId())
	require.Zero(t, snap.GetTeam().GetVersion())
	require.Equal(t, h.epochOf(2), snap.GetTeam().GetMembershipEpoch())
	require.Equal(t, "0", h.mr.HGet(playerIndexKey(2), indexFieldTid))
	h.requireSceneRefresh(2)

	// 解散:未过期邀请的被邀请人收 INVITE_REVOKED;scene 通知全体成员(此时只剩队长)。
	resp = h.ok(h.server.DisbandTeam(asPlayer(1), &teampb.DisbandTeamRequest{ExpectedTeamId: tid}))
	requireCode(t, 0, resp)
	require.Zero(t, resp.GetTeam().GetTeamId())
	require.Equal(t, h.epochOf(1), resp.GetTeam().GetMembershipEpoch())
	pushes = h.takePushes()
	require.Len(t, pushes, 1)
	ev = requireEvent(t, pushes[0], 4, teampb.TeamEventType_TEAM_EVENT_TYPE_INVITE_REVOKED)
	require.Equal(t, tid, ev.GetTeamId())
	h.requireSceneRefresh(1)
	require.False(t, h.mr.Exists(recordKey(tid)))
	require.False(t, h.mr.Exists(projectionKey(tid)))
}

// ---- #7 / §H.3:整队 24h 空闲过期后,GetMyTeam 与未绑定回包的空视图能被客户端接受 ----

func TestExpiredTeamEmptyViewIsAcceptedByClient(t *testing.T) {
	h := newServiceHarness(t, config.TeamConf{})
	h.addPlayers(testZone, 1, 2)
	tid := h.create(1)
	h.join(1, tid, 2)
	h.takePushes()
	h.takeScenes()
	// 客户端当前值 = 最后一次提交后各人的 (epoch, version)(队长看回包,2 看 MEMBER_JOINED 推送)。
	ver := h.versionOf(tid)
	curEpoch := map[uint64]uint64{1: h.epochOf(1), 2: h.epochOf(2)}

	// 全队挂机 24h 无提交、无人打开面板:记录、投影、全员索引同批过期(Redis 时钟同步前进)。
	setRedisMs(h.mr, baseMs()+25*3600*1000)
	h.mr.FastForward(25 * time.Hour)
	require.False(t, h.mr.Exists(playerIndexKey(2)))

	requireAccepted := func(pid uint64, resp *teampb.TeamResponse) {
		t.Helper()
		require.Zero(t, resp.GetTeam().GetTeamId())
		require.True(t, clientAccepts(curEpoch[pid], ver, resp.GetTeam().GetMembershipEpoch(), resp.GetTeam().GetVersion()),
			"player %d 当前 epoch=%d,空视图 epoch=%d 被 §H.3 丢弃", pid, curEpoch[pid], resp.GetTeam().GetMembershipEpoch())
	}
	mine := h.ok(h.server.GetMyTeam(asPlayer(2), &teampb.GetMyTeamRequest{}))
	requireCode(t, 0, mine)
	requireAccepted(2, mine)
	left := h.ok(h.server.LeaveTeam(asPlayer(2), &teampb.LeaveTeamRequest{ExpectedTeamId: tid}))
	requireCode(t, 0, left)
	requireAccepted(2, left)
	disband := h.ok(h.server.DisbandTeam(asPlayer(1), &teampb.DisbandTeamRequest{ExpectedTeamId: tid}))
	requireCode(t, ErrNoTeam, disband)
	requireAccepted(1, disband)

	requireNoTeamKeys(t, h.mr) // 读路径与未绑定分支都不写
	require.Empty(t, h.takePushes())
	h.requireSceneRefresh()
}

func TestSceneRefreshFailsClosed(t *testing.T) {
	h := newServiceHarness(t, config.TeamConf{})
	h.addPlayers(testZone, 1, 2)

	// 节点 uuid 为空:不发(AGENTS §7 #2),只记指标。
	h.svcCtx.SceneNodes.Upsert(testSceneNodeKey, discovery.NodeEntry{NodeId: 1, ZoneId: testZone, Endpoint: "127.0.0.1:1"})
	emptyUuid := metrics.TeamSceneRefreshValue(sceneOutcomeEmptyUuid)
	h.create(1)
	h.requireSceneRefresh()
	require.Equal(t, emptyUuid+1, metrics.TeamSceneRefreshValue(sceneOutcomeEmptyUuid))

	// 玩家不在场景:不发,下次进场自己拉。
	h.svcCtx.SceneNodes.Upsert(testSceneNodeKey, discovery.NodeEntry{
		NodeId: 1, ZoneId: testZone, NodeUuid: testSceneUuid, Endpoint: "127.0.0.1:1",
	})
	h.mr.Del(playercontract.LocationKey(2))
	noLocation := metrics.TeamSceneRefreshValue(sceneOutcomeNoLocation)
	h.create(2)
	h.requireSceneRefresh()
	require.Equal(t, noLocation+1, metrics.TeamSceneRefreshValue(sceneOutcomeNoLocation))
}

// ---- #15:3 次冲突耗尽回 TeamStateChanged ----

func TestCommitConflictsExhaustReturnStateChanged(t *testing.T) {
	h := newServiceHarness(t, config.TeamConf{})
	h.addPlayers(testZone, 1, 2)
	tid := h.create(1)
	h.join(1, tid, 2)
	h.takePushes()
	h.takeScenes()

	retries := metrics.TeamCommitRetryValue(methodKickMember)
	rounds := 0
	afterReadHook = func(bind Binding) {
		if bind.PlayerId() != 1 || bind.TeamId() != tid {
			return
		}
		rounds++
		// 每轮读完都有别的实例抢先提交:ver 前移,本轮 S_COMMIT 必然 {0}。
		h.mr.HSet(recordKey(tid), recFieldVer, strconv.FormatUint(h.versionOf(tid)+1, 10))
	}
	resp := h.ok(h.server.KickMember(asPlayer(1), &teampb.KickMemberRequest{TargetPlayerId: 2, ExpectedTeamId: tid}))
	afterReadHook = nil

	requireCode(t, ErrStateChanged, resp)
	require.Equal(t, CommitRetries, rounds)
	require.Equal(t, retries+float64(CommitRetries), metrics.TeamCommitRetryValue(methodKickMember))
	require.NotNil(t, FindMember(loadRecord(t, h.mr, tid), 2), "冲突耗尽不得写入")
	require.Equal(t, u64s(tid), h.mr.HGet(playerIndexKey(2), indexFieldTid))
	require.Equal(t, tid, resp.GetTeam().GetTeamId(), "失败回包带调用者自由读视图")
	require.Empty(t, h.takePushes())
	h.requireSceneRefresh()
}

// ---- #24:迟到执行不误伤新队伍 ----

func TestLateLeaveDoesNotHurtNewTeam(t *testing.T) {
	h := newServiceHarness(t, config.TeamConf{})
	h.addPlayers(testZone, 1, 2, 3)
	teamA := h.create(1)
	h.join(1, teamA, 2)
	teamB := h.create(3)

	fired := false
	var afterHook string
	afterReadHook = func(bind Binding) {
		if fired || bind.PlayerId() != 2 || bind.TeamId() != teamA {
			return
		}
		fired = true
		// 旧 LeaveTeam 挂在 S_READ 之后;期间玩家经别的实例离开 A、接受 B 的邀请。
		requireCode(t, 0, h.ok(h.server.LeaveTeam(asPlayer(2), &teampb.LeaveTeamRequest{ExpectedTeamId: teamA})))
		requireCode(t, 0, h.ok(h.server.InviteToTeam(asPlayer(3), &teampb.InviteToTeamRequest{TargetPlayerId: 2, ExpectedTeamId: teamB})))
		requireCode(t, 0, h.ok(h.server.RespondInvite(asPlayer(2), &teampb.RespondInviteRequest{TeamId: teamB, Accept: true})))
		afterHook = h.mr.Dump()
	}
	resp := h.ok(h.server.LeaveTeam(asPlayer(2), &teampb.LeaveTeamRequest{ExpectedTeamId: teamA}))
	afterReadHook = nil

	require.True(t, fired)
	requireCode(t, 0, resp)
	require.Equal(t, teamB, resp.GetTeam().GetTeamId(), "回调用者当前(B)的视图")
	require.Equal(t, afterHook, h.mr.Dump(), "迟到的旧请求不得写入")
	require.Equal(t, u64s(teamB), h.mr.HGet(playerIndexKey(2), indexFieldTid))
	require.NotNil(t, FindMember(loadRecord(t, h.mr, teamB), 2))
}

func TestLateDisbandDoesNotHurtNewTeam(t *testing.T) {
	h := newServiceHarness(t, config.TeamConf{})
	h.addPlayers(testZone, 4)
	teamA := h.create(4)

	fired := false
	var teamB uint64
	var afterHook string
	afterReadHook = func(bind Binding) {
		if fired || bind.PlayerId() != 4 || bind.TeamId() != teamA {
			return
		}
		fired = true
		requireCode(t, 0, h.ok(h.server.DisbandTeam(asPlayer(4), &teampb.DisbandTeamRequest{ExpectedTeamId: teamA})))
		teamB = h.create(4)
		afterHook = h.mr.Dump()
	}
	resp := h.ok(h.server.DisbandTeam(asPlayer(4), &teampb.DisbandTeamRequest{ExpectedTeamId: teamA}))
	afterReadHook = nil

	require.True(t, fired)
	requireCode(t, ErrNoTeam, resp)
	require.Equal(t, teamB, resp.GetTeam().GetTeamId(), "4013 + 调用者当前(B)的视图")
	require.Equal(t, afterHook, h.mr.Dump(), "迟到的旧请求不得写入")
	require.Equal(t, uint64(4), loadRecord(t, h.mr, teamB).GetLeaderId())
}

// ---- #25:ctx 已过期时不提交 ----

func TestExpiredContextSkipsCommit(t *testing.T) {
	h := newServiceHarness(t, config.TeamConf{})
	h.addPlayers(testZone, 1, 2)
	tid := h.create(1)
	h.join(1, tid, 2)
	h.takePushes()
	h.takeScenes()

	ctx, cancel := context.WithCancel(asPlayer(1))
	defer cancel()
	var beforeCommit string
	afterReadHook = func(bind Binding) {
		if bind.PlayerId() != 1 {
			return
		}
		// 请求预算在读完之后耗尽:go-zero 超时拦截器已回错,handler 仍在跑(§D.6 迟到执行)。
		cancel()
		beforeCommit = h.mr.Dump()
	}
	resp := h.ok(h.server.KickMember(ctx, &teampb.KickMemberRequest{TargetPlayerId: 2, ExpectedTeamId: tid}))
	afterReadHook = nil

	requireCode(t, ErrStateChanged, resp)
	require.Equal(t, beforeCommit, h.mr.Dump(), "不得执行 S_COMMIT")
	require.NotNil(t, FindMember(loadRecord(t, h.mr, tid), 2))
	require.Empty(t, h.takePushes())
	h.requireSceneRefresh()
}

// ---- #26:GetMyTeam{notify_online} 只推同源 tid==本队 的在线队员 ----

func TestGetMyTeamNotifyOnlineSkipsForeignIndex(t *testing.T) {
	h := newServiceHarness(t, config.TeamConf{})
	h.addPlayers(testZone, 1, 2, 3, 4)
	tid := h.create(1)
	for _, pid := range []uint64{2, 3, 4} {
		h.join(1, tid, pid)
	}
	h.takePushes()
	h.takeScenes()

	// 3 的索引已指向别的队(淘汰后去了别处);4 已下线(会话不存在,非队长不触发转让)。
	h.mr.HSet(playerIndexKey(3), indexFieldTid, "424242")
	h.mr.Del(playercontract.SessionKey(4))

	resp := h.ok(h.server.GetMyTeam(asPlayer(1), &teampb.GetMyTeamRequest{NotifyOnline: true}))
	requireCode(t, 0, resp)
	require.Equal(t, tid, resp.GetTeam().GetTeamId())

	pushes := h.takePushes()
	require.Len(t, pushes, 1, "只推 tid==本队 且在线的其他队员")
	snap := requireSnapshot(t, pushes[0], 2, teampb.TeamChangeReason_TEAM_CHANGE_REASON_MEMBER_ONLINE)
	require.Equal(t, uint64(1), snap.GetActorId())
	require.Equal(t, h.epochOf(2), snap.GetTeam().GetMembershipEpoch(), "epoch 取自同一次 S_READ_MEMBERS")
	require.Equal(t, h.versionOf(tid), snap.GetTeam().GetVersion())
	require.Equal(t, resp.GetTeam().GetVersion(), snap.GetTeam().GetVersion(), "在线态刷新不改 version")
	online := map[uint64]bool{}
	for _, m := range snap.GetTeam().GetMembers() {
		online[m.GetPlayerId()] = m.GetIsOnline()
	}
	require.Equal(t, map[uint64]bool{1: true, 2: true, 3: true, 4: false}, online)
	h.requireSceneRefresh()
}

// ---- #23:ListMyInvites 清理残留索引不误删队长刚重邀写入的新项 ----

func TestListMyInvitesPruneKeepsReinvitedIndex(t *testing.T) {
	h := newServiceHarness(t, config.TeamConf{})
	h.addPlayers(testZone, 1, 5)
	tid := h.create(1)
	key := inviteIndexKey(5)
	// 残留索引项:记录里没有给 5 的邀请,score 与之后的重邀不同。
	_, err := h.mr.ZAdd(key, float64(baseMs()+30_000), u64s(tid))
	require.NoError(t, err)

	fired := false
	beforeInvitePruneHook = func(playerId, teamId uint64) {
		if fired || playerId != 5 || teamId != tid {
			return
		}
		fired = true
		// S_INVITE_LIST 之后、S_INVITE_PRUNE 之前,队长重邀:索引 score 被刷新。
		requireCode(t, 0, h.ok(h.server.InviteToTeam(asPlayer(1), &teampb.InviteToTeamRequest{TargetPlayerId: 5, ExpectedTeamId: tid})))
	}
	listed, err := h.server.ListMyInvites(asPlayer(5), &teampb.ListMyInvitesRequest{})
	beforeInvitePruneHook = nil
	require.NoError(t, err)
	require.True(t, fired)
	require.Zero(t, listed.GetErrorMessage().GetId())
	members, err := h.mr.ZMembers(key)
	require.NoError(t, err)
	require.Contains(t, members, u64s(tid), "score 已变,旧的清理不得删掉新索引项")
	score, err := h.mr.ZScore(key, u64s(tid))
	require.NoError(t, err)
	require.Equal(t, float64(baseMs()+InviteTTLMs), score)

	// 再列一次:正文与索引一致,正常列出;没有正文的残留项被清理。
	_, err = h.mr.ZAdd(key, float64(baseMs()+30_000), "777")
	require.NoError(t, err)
	listed, err = h.server.ListMyInvites(asPlayer(5), &teampb.ListMyInvitesRequest{})
	require.NoError(t, err)
	require.Len(t, listed.GetInvites(), 1)
	got := listed.GetInvites()[0]
	require.Equal(t, tid, got.GetTeamId())
	require.Equal(t, uint64(1), got.GetInviter().GetPlayerId())
	require.True(t, got.GetInviter().GetIsLeader())
	require.Equal(t, uint32(1), got.GetMemberCount())
	require.Equal(t, baseMs()+InviteTTLMs, got.GetExpireAtMs())
	require.Equal(t, baseMs(), listed.GetServerTimeMs())
	members, err = h.mr.ZMembers(key)
	require.NoError(t, err)
	require.Equal(t, []string{u64s(tid)}, members)
}
