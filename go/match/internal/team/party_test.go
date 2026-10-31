package team

// 整队开战与排队里不经存储的部分:成员预检、视图匹配态装饰及其熔断、能力位、模式归一。
// 票据域用假端口 fakeBattlePort;经存储的流程(launch 原件、入队、撤单)在 sql_store_integration_test.go(真 TiDB)。

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"match/internal/config"
	"match/internal/constants"
	"match/internal/playercontract"

	matchpb "proto/match"
	plpb "proto/player_locator"
	smpb "proto/scene_manager"
	teampb "proto/team"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

const (
	testBattleConfig    uint32 = 1
	fakePartyQueuedAtMs uint64 = 1_700_000_123_000
)

type fakePartyTicket struct {
	teamId     uint64
	state      uint32
	targetSize uint32
	configId   uint32
	queuedAtMs uint64
	mode       uint32
}

// fakeBattlePort 假票据域端口:一份内存里的「谁持有哪支队伍的排队票」,并记录读票次数。
type fakeBattlePort struct {
	mu         sync.Mutex
	blocked    map[uint64]bool
	party      map[uint64]fakePartyTicket // player_id → 排队票
	stateErr   error                      // 非 nil:PartyQueueState 回 err
	modeErr    error                      // 非 nil:PartyQueueMode 回 err
	stateCalls int                        // PartyQueueState 被调用的次数
	stateDelay time.Duration              // >0:PartyQueueState 先睡这么久再返回(模拟票据库慢,不看 ctx)
}

var _ BattleStarter = (*fakeBattlePort)(nil)

func (f *fakeBattlePort) TeamSizeFor(_, _ uint32) uint32 { return Capacity }

func (f *fakeBattlePort) MatchLockTTLSeconds(int) int { return 83 }

func (f *fakeBattlePort) TicketBlocked(_ context.Context, playerId uint64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.blocked[playerId], nil
}

func (f *fakeBattlePort) PartyQueueState(_ context.Context, playerId, teamId uint64) (uint32, uint32, uint32, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stateCalls++
	if f.stateDelay > 0 {
		time.Sleep(f.stateDelay)
	}
	if f.stateErr != nil {
		return constants.PartyQueueNone, 0, 0, 0, f.stateErr
	}
	ticket, ok := f.party[playerId]
	if !ok || ticket.teamId != teamId {
		return constants.PartyQueueNone, 0, 0, 0, nil
	}
	if ticket.state != constants.PartyQueueQueued {
		return ticket.state, 0, 0, 0, nil
	}
	return ticket.state, ticket.targetSize, ticket.configId, ticket.queuedAtMs, nil
}

func (f *fakeBattlePort) PartyQueueMode(_ context.Context, playerId, _ uint64) (uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.modeErr != nil {
		return 0, f.modeErr
	}
	return f.party[playerId].mode, nil
}

func (f *fakeBattlePort) ReserveSQLTeamMatch(context.Context, *teampb.TeamLaunchRecord) (*matchpb.MatchFormationRecord, error) {
	return nil, errors.New("用例不应走到成局")
}

func (f *fakeBattlePort) stateReads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stateCalls
}

func queuedTicket(mode matchpb.MatchMode, target uint32) fakePartyTicket {
	return fakePartyTicket{
		teamId: ruleTid, state: constants.PartyQueueQueued, targetSize: target, configId: testBattleConfig,
		queuedAtMs: fakePartyQueuedAtMs, mode: uint32(mode),
	}
}

func requireQueuedView(t *testing.T, view *teampb.TeamView, target uint32) {
	t.Helper()
	require.Equal(t, teampb.TeamMatchState_TEAM_MATCH_STATE_QUEUED, view.GetMatchState())
	require.Equal(t, target, view.GetMatchTargetSize())
	require.Equal(t, fakePartyQueuedAtMs, view.GetMatchQueuedAtMs())
	require.Equal(t, testBattleConfig, view.GetMatchBattleConfigId())
}

func requireIdleView(t *testing.T, view *teampb.TeamView) {
	t.Helper()
	require.Equal(t, teampb.TeamMatchState_TEAM_MATCH_STATE_IDLE, view.GetMatchState())
	require.Zero(t, view.GetMatchTargetSize())
	require.Zero(t, view.GetMatchQueuedAtMs())
	require.Zero(t, view.GetMatchBattleConfigId())
}

// decorated 为 viewer 现算一份视图的匹配态。
func (h *serviceHarness) decorated(viewer uint64) *teampb.TeamView {
	view := &teampb.TeamView{TeamId: ruleTid}
	h.service.decorateQueueState(context.Background(), viewer, view)
	return view
}

// 视图里的匹配态按接收者自己的票据现算。
func TestViewQueueStateFollowsViewerTicket(t *testing.T) {
	h := newServiceHarness(t)
	requireIdleView(t, h.decorated(101))

	other := queuedTicket(matchpb.MatchMode_MATCH_MODE_PVE_TEAM, 4)
	other.teamId = ruleTid + 1
	// 只有 101、102 持有本队的票;103 的票属于别的队伍,与本队无关。
	h.port.party = map[uint64]fakePartyTicket{
		101: queuedTicket(matchpb.MatchMode_MATCH_MODE_PVP_TEAM, 5),
		102: {teamId: ruleTid, state: constants.PartyQueueMatched, mode: uint32(matchpb.MatchMode_MATCH_MODE_PVE_TEAM)},
		103: other,
	}
	queued := h.decorated(101)
	requireQueuedView(t, queued, 5)
	require.Equal(t, matchpb.MatchMode_MATCH_MODE_PVP_TEAM, queued.GetMatchMode(), "模式来自本人的票据")
	matched := h.decorated(102)
	require.Equal(t, teampb.TeamMatchState_TEAM_MATCH_STATE_STARTING, matched.GetMatchState(), "已成局:集合中")
	require.Equal(t, matchpb.MatchMode_MATCH_MODE_PVE_TEAM, matched.GetMatchMode())
	require.Zero(t, matched.GetMatchTargetSize())
	requireIdleView(t, h.decorated(103))

	// 开战锁已经让视图是 STARTING 时不读票、不动。
	starting := &teampb.TeamView{TeamId: ruleTid, MatchState: teampb.TeamMatchState_TEAM_MATCH_STATE_STARTING}
	before := h.port.stateReads()
	h.service.decorateQueueState(context.Background(), 101, starting)
	require.Equal(t, before, h.port.stateReads())
	require.Equal(t, teampb.TeamMatchState_TEAM_MATCH_STATE_STARTING, starting.GetMatchState())

	// 无队伍的空视图不读票。
	h.service.decorateQueueState(context.Background(), 101, &teampb.TeamView{})
	require.Equal(t, before, h.port.stateReads())

	// 读票失败:视图照常下发,按没在排处理。
	h.port.stateErr = errors.New("ticket store down")
	requireIdleView(t, h.decorated(101))
}

// 读到票却读不到模式:不把模式未知的状态发给客户端,视图按没在排下发。
func TestViewQueueStateSkipsWhenModeUnreadable(t *testing.T) {
	h := newServiceHarness(t)
	h.port.party = map[uint64]fakePartyTicket{101: queuedTicket(matchpb.MatchMode_MATCH_MODE_PVP_TEAM, 5)}
	h.port.modeErr = errors.New("ticket store down")
	requireIdleView(t, h.decorated(101))
}

// 视图装饰读票的熔断:读票出错之后的一小段时间里不再读,一律按没在排下发 ——
// 一批推送逐个接收者各读一次,不熔断的话票据库一慢,与匹配无关的队伍推送也会被拖到超时。
func TestQueueStateReadBreakerSkipsFurtherReads(t *testing.T) {
	h := newServiceHarness(t)
	h.port.party = map[uint64]fakePartyTicket{
		101: queuedTicket(matchpb.MatchMode_MATCH_MODE_PVE_TEAM, 5),
		102: queuedTicket(matchpb.MatchMode_MATCH_MODE_PVE_TEAM, 5),
	}
	requireQueuedView(t, h.decorated(102), 5)

	h.port.stateErr = errors.New("ticket store down")
	requireIdleView(t, h.decorated(102)) // 这一次读失败,熔断开始
	before := h.port.stateReads()
	h.port.stateErr = nil
	requireIdleView(t, h.decorated(102)) // 票据库已恢复,但熔断期内不读:仍按没在排
	requireIdleView(t, h.decorated(101))
	require.Equal(t, before, h.port.stateReads(), "熔断期内不再读票")
	require.True(t, h.decorated(101).GetMatchFillSupported(), "能力位不受熔断影响")

	// 熔断到期后恢复。
	h.service.queueStateBreakerUntil.Store(0)
	requireQueuedView(t, h.decorated(102), 5)
}

// 熔断只认票据库自己的问题:调用方的时间预算已经用完时不读、也不熔断,之后的正常读取照常。
func TestQueueStateBreakerIgnoresCallerDeadline(t *testing.T) {
	h := newServiceHarness(t)
	h.port.party = map[uint64]fakePartyTicket{102: queuedTicket(matchpb.MatchMode_MATCH_MODE_PVE_TEAM, 5)}

	expired, cancel := context.WithCancel(context.Background())
	cancel()
	h.port.stateErr = context.Canceled // 即使读了也只会得到这个错误
	view := &teampb.TeamView{TeamId: ruleTid}
	before := h.port.stateReads()
	h.service.decorateQueueState(expired, 102, view)

	require.Equal(t, teampb.TeamMatchState_TEAM_MATCH_STATE_IDLE, view.GetMatchState())
	require.Equal(t, before, h.port.stateReads(), "预算已用完:不读票")
	require.Zero(t, h.service.queueStateBreakerUntil.Load(), "不算票据库故障")
	h.port.stateErr = nil
	requireQueuedView(t, h.decorated(102), 5)
}

// 读后的判断按这一次读自己的耗时:读得不慢、只是调用方预算到期 → 不熔断;
// 读得慢(票据库慢到把调用方拖到期)→ 熔断,这正是熔断要防的情形。
func TestQueueStateBreakerJudgesTheReadItself(t *testing.T) {
	h := newServiceHarness(t)
	h.port.party = map[uint64]fakePartyTicket{102: queuedTicket(matchpb.MatchMode_MATCH_MODE_PVE_TEAM, 5)}

	fastCtx, cancelFast := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancelFast()
	h.port.stateDelay = 40 * time.Millisecond
	h.port.stateErr = context.DeadlineExceeded
	h.service.decorateQueueState(fastCtx, 102, &teampb.TeamView{TeamId: ruleTid})
	require.Zero(t, h.service.queueStateBreakerUntil.Load(), "读得不慢,只是调用方预算到期:不熔断")

	slowCtx, cancelSlow := context.WithTimeout(context.Background(), queueStateSlowThreshold/3)
	defer cancelSlow()
	h.port.stateDelay = 2 * queueStateSlowThreshold
	view := &teampb.TeamView{TeamId: ruleTid}
	h.service.decorateQueueState(slowCtx, 102, view)
	require.Equal(t, teampb.TeamMatchState_TEAM_MATCH_STATE_IDLE, view.GetMatchState())
	require.NotZero(t, h.service.queueStateBreakerUntil.Load(), "读得慢:熔断,哪怕调用方预算因此到期")

	h.port.stateDelay = 0
	h.port.stateErr = nil
	h.service.queueStateBreakerUntil.Store(0)
	requireQueuedView(t, h.decorated(102), 5)
}

// 能力位 match_fill_supported:有队伍的视图都要标;关了开关就是 false;无队伍的空视图不标。
// 客户端只在它为 true 时给出目标人数选项。PVP 能力位同样由服务端声明。
func TestViewsCarryFillSupportedFlag(t *testing.T) {
	h := newServiceHarness(t)

	view := &teampb.TeamView{TeamId: ruleTid}
	h.service.markFillSupported(view)
	require.True(t, view.GetMatchFillSupported())
	require.True(t, view.GetPvpMatchSupported())

	empty := &teampb.TeamView{}
	h.service.markFillSupported(empty)
	require.False(t, empty.GetMatchFillSupported(), "无队伍的空视图不标")
	require.False(t, empty.GetPvpMatchSupported())
	h.service.markFillSupported(nil)

	h.svcCtx.Config = config.Config{Team: config.TeamConf{FillQueueDisabled: true}}
	view = &teampb.TeamView{TeamId: ruleTid}
	h.service.markFillSupported(view)
	require.False(t, view.GetMatchFillSupported(), "关掉开关:视图如实反映,客户端不再给出选项")
	require.True(t, view.GetPvpMatchSupported(), "开关只管 PVE 补位,不关闭独立声明能力的真人 PVP")
}

// rosterChanged:有人加入或移出才算,只改队长或开战状态不算。
func TestRosterChanged(t *testing.T) {
	require.False(t, rosterChanged(nil))
	require.False(t, rosterChanged(&CommitResult{Decision: Decision{Kept: []uint64{1, 2}}}))
	require.True(t, rosterChanged(&CommitResult{Decision: Decision{Joined: []uint64{3}, Kept: []uint64{1}}}))
	require.True(t, rosterChanged(&CommitResult{Decision: Decision{Left: []uint64{1, 2}}}), "解散时移出者即全员")
}

func TestNormalizeTeamMatchMode(t *testing.T) {
	mode, ok := normalizeTeamMatchMode(uint32(matchpb.MatchMode_MATCH_MODE_UNSPECIFIED))
	require.True(t, ok)
	require.Equal(t, matchpb.MatchMode_MATCH_MODE_PVE_TEAM, mode, "未填模式按组队 PVE,兼容不填模式的开战请求")
	mode, ok = normalizeTeamMatchMode(uint32(matchpb.MatchMode_MATCH_MODE_PVE_TEAM))
	require.True(t, ok)
	require.Equal(t, matchpb.MatchMode_MATCH_MODE_PVE_TEAM, mode)
	mode, ok = normalizeTeamMatchMode(uint32(matchpb.MatchMode_MATCH_MODE_PVP_TEAM))
	require.True(t, ok)
	require.Equal(t, matchpb.MatchMode_MATCH_MODE_PVP_TEAM, mode)
	for _, invalid := range []matchpb.MatchMode{matchpb.MatchMode_MATCH_MODE_1V1, matchpb.MatchMode_MATCH_MODE_5V5, matchpb.MatchMode_MATCH_MODE_PVP_CHALLENGE} {
		_, ok = normalizeTeamMatchMode(uint32(invalid))
		require.False(t, ok, invalid.String())
	}
}

// ---- 开战预检(§E.1 第 5 步):按 roster 顺序,第一个不满足的人带进 parameters[0] ----

func TestPreflightMatch(t *testing.T) {
	roster := []uint64{101, 102, 103}
	cases := []struct {
		name  string
		setup func(h *serviceHarness)
		code  uint32
		param uint64
	}{
		{name: "队员离线", setup: func(h *serviceHarness) { h.mr.Del(playercontract.SessionKey(103)) }, code: ErrMemberOffline, param: 103},
		{name: "队员断线等重连也不算在线", setup: func(h *serviceHarness) {
			raw, err := proto.Marshal(&plpb.PlayerSession{PlayerId: 102, State: plpb.PlayerSessionState_SESSION_STATE_DISCONNECTING})
			require.NoError(h.t, err)
			require.NoError(h.t, h.mr.Set(playercontract.SessionKey(102), string(raw)))
		}, code: ErrMemberOffline, param: 102},
		{name: "队员在战斗中", setup: func(h *serviceHarness) {
			require.NoError(h.t, h.mr.Set(playercontract.BattleLockKey(102), "777"))
		}, code: ErrMemberInBattle, param: 102},
		{name: "队员不在场景", setup: func(h *serviceHarness) { h.mr.Del(playercontract.LocationKey(103)) }, code: ErrMemberNotReady, param: 103},
		{name: "队员跨 zone 交接中", setup: func(h *serviceHarness) {
			raw, err := proto.Marshal(&smpb.PlayerLocation{SceneId: 1, ZoneId: testZone})
			require.NoError(h.t, err)
			require.NoError(h.t, h.mr.Set(playercontract.LocationKey(103), string(raw)))
		}, code: ErrMemberNotReady, param: 103},
		{name: "队员已有在途票据", setup: func(h *serviceHarness) { h.port.blocked = map[uint64]bool{102: true} }, code: ErrMemberNotReady, param: 102},
		{name: "会话读不出来", setup: func(h *serviceHarness) {
			require.NoError(h.t, h.mr.Set(playercontract.SessionKey(101), "not-a-session"))
		}, code: ErrInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newServiceHarness(t)
			h.addPlayers(roster...)
			tc.setup(h)

			code, param := h.service.preflightMatch(context.Background(), roster)
			require.Equal(t, tc.code, code)
			require.Equal(t, tc.param, param)
		})
	}

	h := newServiceHarness(t)
	h.addPlayers(roster...)
	code, param := h.service.preflightMatch(context.Background(), roster)
	require.Zero(t, code)
	require.Zero(t, param)
}
