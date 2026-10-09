package logic

// 同道历练 logic 层的单测(不连真库、不连 match;06-activities.md §6.40 的 activity_trial_test.go 部分)。
//
// 套路与 activity_logic_test.go 相同,多三样替身:
//   - fakeTrialStore:历练仓储(trialStore)的内存替身。事务语义(取锁、幂等闸门、发奖名单的过滤)不在这里验 ——
//     那是 data/activity_repo_integration_test.go 的 I13–I23;这里只验 logic 传给仓储什么、拿到每种结果之后做什么。
//   - fakeMatch:match 内部服务的替身,按脚本作答,并记下每次请求与当时的 ctx。
//   - 两个 miniredis:一个当 guild 全局 Redis(邀请房间的 Lua 真跑在上面),一个当 PlayerLocatorRedis
//     (会话、战斗锁、对局结果记录)。分开是为了能单独把其中一个关掉来注入故障。
//
// 时间一律走注入的 trialTestClock(房间过期、周期键、结算的"现在"),不读墙钟;唯一与墙钟有关的是调 match 的预算,
// 那几条用例用"没有截止 / 截止已过 / 预算上限小于下限"这类与机器快慢无关的输入(AGENTS.md §11.4)。
//
// 用户决策 U2:阵亡者也得奖,候选 = 己方 − 逃跑者(TestTrialCandidates)。

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"guild/internal/activity"
	"guild/internal/config"
	"guild/internal/constants"
	"guild/internal/data"
	battlepb "proto/battle"
	assetpb "proto/common/asset"
	kafkapb "proto/contracts/kafka"
	pb "proto/guild"
	matchpb "proto/match"
	plpb "proto/player_locator"
	tablepb "shared/generated/pb/table"
)

// ── 替身 ──────────────────────────────────────────────────────

// trialTestClock 是可拨动的注入时钟。
type trialTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *trialTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *trialTestClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// seqMinter 是发号器替身:从 next 起递增发号;err 非 nil 时一律失败。记调用次数。
type seqMinter struct {
	mu    sync.Mutex
	next  uint64
	err   error
	calls int
}

func (m *seqMinter) Mint(context.Context) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.err != nil {
		return 0, m.err
	}
	id := m.next
	m.next++
	return id, nil
}

func (m *seqMinter) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// convertedOwed 记一次待入队物品的转换调用。
type convertedOwed struct {
	owed  data.OwedReward
	opID  uint64
	nowMs uint64
}

// fakeTrialStore 是 trialStore 的内存替身。零值可用:查无此人 / 查无此局 / 结算回 Settled+LOSS / 转换即成功。
// 加锁:后台循环的用例与 safego 派生的协程可能并发访问。
type fakeTrialStore struct {
	mu sync.Mutex

	// TrialRosterMembers:本帮成员 → join_time_ms。
	members     map[uint64]uint64
	membersErr  error
	memberReads int

	// TrialBattle:battlesErr 非 nil 时读失败。
	battles    map[uint64]data.TrialBattleRow
	battlesErr error

	// RegisterTrialBattle
	registered  []data.TrialBattleKey
	registerErr error

	// SettleTrialBattleTx:settle 为 nil 时回 Settled + LOSS。
	settleInputs []data.TrialSettleInput
	settle       func(in data.TrialSettleInput) (data.TrialSettleResult, error)

	// MarkTrialBattlePoison:markOutcome 为零值时回 TrialMarkDone。
	marked      []data.TrialBattleKey
	markOutcome data.TrialMarkOutcome
	markErr     error

	// ExpireTrialBattle
	expired   []uint64
	expireErr error

	// ListOverdueTrialBattles
	overdue       []data.OverdueTrialBattle
	overdueBefore []uint64
	overdueErr    error

	// 待入队物品:owedRows 按 (created_ms, player_id, battle_id) 升序;convert 为 nil 时转换即成功并移除该行。
	owedRows   []data.OwedReward
	listCalls  []data.OwedRewardCursor
	listErr    error
	convert    func(owed data.OwedReward) (data.OwedConvertStatus, error)
	converted  []convertedOwed
	owedCounts map[uint32]uint32
	countCalls int
}

var _ trialStore = (*fakeTrialStore)(nil)

func (s *fakeTrialStore) TrialRosterMembers(_ context.Context, _ uint64, playerIDs []uint64) (map[uint64]uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.memberReads++
	if s.membersErr != nil {
		return nil, s.membersErr
	}
	out := make(map[uint64]uint64, len(playerIDs))
	for _, id := range playerIDs {
		if joinMs, ok := s.members[id]; ok {
			out[id] = joinMs
		}
	}
	return out, nil
}

func (s *fakeTrialStore) TrialBattle(_ context.Context, battleID uint64) (data.TrialBattleRow, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.battlesErr != nil {
		return data.TrialBattleRow{}, false, s.battlesErr
	}
	row, ok := s.battles[battleID]
	return row, ok, nil
}

func (s *fakeTrialStore) RegisterTrialBattle(_ context.Context, battle data.TrialBattleKey, _ uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.registerErr != nil {
		return s.registerErr
	}
	s.registered = append(s.registered, battle)
	return nil
}

func (s *fakeTrialStore) SettleTrialBattleTx(_ context.Context, in data.TrialSettleInput) (data.TrialSettleResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settleInputs = append(s.settleInputs, in)
	if s.settle != nil {
		return s.settle(in)
	}
	return data.TrialSettleResult{Status: data.TrialSettleSettled, Result: pb.GuildTrialSettleResult_GUILD_TRIAL_SETTLE_RESULT_LOSS}, nil
}

func (s *fakeTrialStore) MarkTrialBattlePoison(_ context.Context, battle data.TrialBattleKey, _, _ uint64) (data.TrialMarkOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.markErr != nil {
		return 0, s.markErr
	}
	s.marked = append(s.marked, battle)
	if s.markOutcome != 0 {
		return s.markOutcome, nil
	}
	return data.TrialMarkDone, nil
}

func (s *fakeTrialStore) ExpireTrialBattle(_ context.Context, battleID, _, _ uint64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.expireErr != nil {
		return false, s.expireErr
	}
	s.expired = append(s.expired, battleID)
	return true, nil
}

func (s *fakeTrialStore) ListOverdueTrialBattles(_ context.Context, createdBeforeMs uint64, limit int) ([]data.OverdueTrialBattle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.overdueBefore = append(s.overdueBefore, createdBeforeMs)
	if s.overdueErr != nil {
		return nil, s.overdueErr
	}
	var out []data.OverdueTrialBattle
	for _, row := range s.overdue {
		if row.CreatedMs < createdBeforeMs && len(out) < limit {
			out = append(out, row)
		}
	}
	return out, nil
}

// owedAfter 报告 row 是否排在游标之后(与 data 层的键集游标同一全序)。
func owedAfter(row data.OwedReward, after data.OwedRewardCursor) bool {
	if row.CreatedMs != after.CreatedMs {
		return row.CreatedMs > after.CreatedMs
	}
	if row.PlayerID != after.PlayerID {
		return row.PlayerID > after.PlayerID
	}
	return row.BattleID > after.BattleID
}

func (s *fakeTrialStore) ListOwedRewards(_ context.Context, after data.OwedRewardCursor, limit int) ([]data.OwedReward, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCalls = append(s.listCalls, after)
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []data.OwedReward
	for _, row := range s.owedRows {
		if owedAfter(row, after) && len(out) < limit {
			out = append(out, row)
		}
	}
	return out, nil
}

func (s *fakeTrialStore) ConvertOwedReward(_ context.Context, owed data.OwedReward, opID, nowMs uint64) (data.OwedConvertStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.converted = append(s.converted, convertedOwed{owed: owed, opID: opID, nowMs: nowMs})
	status, err := data.OwedConverted, error(nil)
	if s.convert != nil {
		status, err = s.convert(owed)
	}
	if err == nil && (status == data.OwedConverted || status == data.OwedGone) {
		s.owedRows = slices.DeleteFunc(s.owedRows, func(row data.OwedReward) bool {
			return row.PlayerID == owed.PlayerID && row.BattleID == owed.BattleID
		})
	}
	return status, err
}

func (s *fakeTrialStore) CountOwedRewards(_ context.Context, limit int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.countCalls++
	return min(len(s.owedRows), limit), nil
}

func (s *fakeTrialStore) OwedRewardCounts(context.Context, uint64) (map[uint32]uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.owedCounts, nil
}

func (s *fakeTrialStore) settleCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.settleInputs)
}

// fakeMatch 是 match 内部服务的替身:回脚本里的 resp / err,记下每次请求、当时 ctx 有没有截止、有没有带出站 metadata。
type fakeMatch struct {
	mu          sync.Mutex
	resp        *matchpb.StartActivityBattleResponse
	err         error
	requests    []*matchpb.StartActivityBattleRequest
	hadDeadline []bool
	hadOutMD    []bool
}

var _ TrialBattleStarter = (*fakeMatch)(nil)

func (m *fakeMatch) StartActivityBattle(ctx context.Context, in *matchpb.StartActivityBattleRequest, _ ...grpc.CallOption) (*matchpb.StartActivityBattleResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, proto.Clone(in).(*matchpb.StartActivityBattleRequest))
	_, hasDeadline := ctx.Deadline()
	m.hadDeadline = append(m.hadDeadline, hasDeadline)
	_, hasMD := metadata.FromOutgoingContext(ctx)
	m.hadOutMD = append(m.hadOutMD, hasMD)
	return m.resp, m.err
}

func (m *fakeMatch) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

// ── 夹具 ──────────────────────────────────────────────────────

const (
	trialTestGuildID    uint64 = 9
	trialTestActivityID uint32 = 3
)

// trialResultKey 是 battle 写结果记录的键(跨语言契约:battle:activity_result:{battle_id})。在这里写成字面量,
// logic 侧的销账若删错了键,用例会直接看到记录还在。
func trialResultKey(battleID uint64) string {
	return "battle:activity_result:" + strconv.FormatUint(battleID, 10)
}

// trialFixture:帮会 9(zone 2)在缓存里;MySQL 是 noMembershipMySQL(活动视图的读全回空);
// 历练的仓储、match、推送都是替身;两个 miniredis 分别当 guild 全局 Redis(房间)与 PlayerLocatorRedis(会话 / 锁 / 结果记录)。
type trialFixture struct {
	l        *GuildLogic
	deps     *ActivityDeps
	store    *fakeTrialStore
	match    *fakeMatch
	notifier *recordingNotifier
	clock    *trialTestClock
	lobbies  *miniredis.Miniredis
	locator  *miniredis.Miniredis
	fence    *fakeFence
}

// newTrialFixture 建夹具。调用前先用 swapActivityTables 布好配表。mutate(可为 nil)在装配前改依赖,
// 用来关掉某个依赖或接上资产通道。
func newTrialFixture(t *testing.T, g data.GuildData, mutate func(d *ActivityDeps)) *trialFixture {
	t.Helper()
	repo, cache := newNoMembershipRepo(t)
	seedGuild(t, cache, g, 0)
	store := &fakeTrialStore{members: map[uint64]uint64{}}
	for _, m := range g.Members {
		seedMembership(t, cache, m.PlayerID, g.GuildID)
		store.members[m.PlayerID] = m.JoinTimeMs
	}

	lobbies := miniredis.RunT(t)
	lobbyRedis := goredis.NewClient(&goredis.Options{Addr: lobbies.Addr()})
	locator := miniredis.RunT(t)
	locatorRedis := goredis.NewClient(&goredis.Options{Addr: locator.Addr()})
	t.Cleanup(func() {
		lobbyRedis.Close()
		locatorRedis.Close()
	})
	lobbyRepo, err := data.NewTrialLobbyRepo(lobbyRedis)
	require.NoError(t, err)
	results, err := data.NewTrialResultRecords(locatorRedis)
	require.NoError(t, err)
	activityRepo, err := data.NewActivityRepo(repo)
	require.NoError(t, err)

	f := &trialFixture{
		store:    store,
		match:    &fakeMatch{resp: &matchpb.StartActivityBattleResponse{BattleId: 777}},
		notifier: &recordingNotifier{},
		clock:    &trialTestClock{now: activityTestNow},
		lobbies:  lobbies,
		locator:  locator,
		fence:    &fakeFence{},
	}
	deps := ActivityDeps{
		Repo:        activityRepo,
		Now:         f.clock.Now,
		Lobby:       lobbyRepo,
		Match:       f.match,
		Results:     results,
		BattleLocks: locatorRedis,
		trials:      store,
	}
	if mutate != nil {
		mutate(&deps)
	}
	f.l = NewGuildLogic(repo, nil, NewOnlineStatusResolver(locatorRedis), f.fence, &fakeHomeZones{zone: g.ZoneID},
		WithNotifier(f.notifier), WithActivities(deps))
	f.deps = f.l.activities
	require.NotNil(t, f.deps)
	return f
}

func (f *trialFixture) setOnline(t *testing.T, ids ...uint64) {
	t.Helper()
	for _, id := range ids {
		writeSession(t, f.locator, id, plpb.PlayerSessionState_SESSION_STATE_ONLINE)
	}
}

func (f *trialFixture) nowMs() uint64 { return uint64(f.clock.Now().UnixMilli()) }

// start 以 initiator 身份建房。
func (f *trialFixture) start(initiator uint64, roster ...uint64) (*pb.StartGuildTrialResponse, error) {
	return f.l.StartGuildTrial(clientCtx(initiator), &pb.StartGuildTrialRequest{ActivityId: trialTestActivityID, MemberPlayerIds: roster})
}

// mustStart 建房并要求成功,返回房间视图。
func (f *trialFixture) mustStart(t *testing.T, initiator uint64, roster ...uint64) *pb.GuildTrialLobbyView {
	t.Helper()
	resp, err := f.start(initiator, roster...)
	require.NoError(t, err)
	require.Nil(t, resp.GetErrorMessage(), "建房应当成功")
	lobby := resp.GetActivity().GetTrialLobby()
	require.NotNil(t, lobby, "建房成功的回包必须带房间")
	return lobby
}

func (f *trialFixture) respond(playerID, lobbyID uint64, accept bool) (*pb.RespondGuildTrialInviteResponse, error) {
	return f.l.RespondGuildTrialInvite(clientCtx(playerID), &pb.RespondGuildTrialInviteRequest{LobbyId: lobbyID, Accept: accept})
}

// lobbyCreated 报告是否真的建过房间(发号键只在建房时被 INCR)。
func (f *trialFixture) lobbyCreated() bool { return f.lobbies.Exists("guild:trial:lobby:seq") }

// putResult 按 battle 的写法落一条结果记录。
func (f *trialFixture) putResult(t *testing.T, ev *kafkapb.BattleResultEvent) {
	t.Helper()
	raw, err := proto.Marshal(ev)
	require.NoError(t, err)
	require.NoError(t, f.locator.Set(trialResultKey(ev.GetBattleId()), string(raw)))
}

func (f *trialFixture) hasResult(battleID uint64) bool {
	return f.locator.Exists(trialResultKey(battleID))
}

// activityPushes 返回已记录的 ACTIVITY_CHANGED 推送。
func (f *trialFixture) activityPushes() []recordedPush {
	return f.notifier.pushesOf(pb.GuildChangeKind_GUILD_CHANGE_KIND_ACTIVITY_CHANGED)
}

// useTrialTables 布好只有历练一行的配表(外加 Reward[1]);row 为 nil 用默认的历练行。
func useTrialTables(t *testing.T, row *tablepb.GuildActivityTable, rule *tablepb.GuildRuleTable) {
	t.Helper()
	if row == nil {
		row = trialRow()
	}
	if rule == nil {
		rule = activityTestRule()
	}
	swapActivityTables(t, []*tablepb.GuildActivityTable{row}, rule, rewardItemRow())
}

// trialRowWithReward:历练行带 Reward[1](合并后 item 1001 × 4)。
func trialRowWithReward() *tablepb.GuildActivityTable {
	row := trialRow()
	row.RewardId = 1
	return row
}

// trialEvent 造一条己方获胜的历练结果事件(帮会 9、活动 3、发起人 42,周期键取测试时钟所在的游戏日)。
func trialEvent(battleID uint64, team0 ...uint64) *kafkapb.BattleResultEvent {
	day := activity.DayKey(activityTestNow)
	return &kafkapb.BattleResultEvent{
		BattleId:       battleID,
		MatchMode:      uint32(matchpb.MatchMode_MATCH_MODE_PVE_TEAM),
		BattleConfigId: 1,
		Outcome:        battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN,
		Teams: []*kafkapb.BattleResultTeam{
			{TeamIndex: 0, PlayerIds: team0},
			{TeamIndex: 1, PlayerIds: []uint64{900001}},
		},
		FinishedAtMs: activityTestNowMs() - 2_000,
		ActivityContext: &battlepb.BattleActivityContext{
			Kind:              battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_GUILD_TRIAL,
			GuildId:           trialTestGuildID,
			ActivityId:        trialTestActivityID,
			PeriodKey:         day,
			InitiatorPlayerId: 42,
			GuildPeriodKey:    day,
		},
	}
}

func trialEventKey(ev *kafkapb.BattleResultEvent) data.TrialBattleKey {
	actx := ev.GetActivityContext()
	return data.TrialBattleKey{
		BattleID: ev.GetBattleId(), GuildID: actx.GetGuildId(), ActivityID: actx.GetActivityId(),
		PeriodKey: actx.GetPeriodKey(), GuildPeriodKey: actx.GetGuildPeriodKey(), InitiatorPlayerID: actx.GetInitiatorPlayerId(),
	}
}

// ── 纯函数 ────────────────────────────────────────────────────

// TestNormalizeTrialRoster(06 §6.40):含 0 / 重复 → duplicate(带重复的那个 id);不含发起人 → initiator_missing;
// 人数越界 → size;合法时发起人到首位、其余保持相对顺序。
func TestNormalizeTrialRoster(t *testing.T) {
	cases := []struct {
		name         string
		ids          []uint64
		wantRoster   []uint64
		wantReason   string
		wantOffender uint64
	}{
		{"含 0", []uint64{42, 0}, nil, trialReasonDuplicate, 0},
		{"重复带出那个 id", []uint64{42, 43, 43}, nil, trialReasonDuplicate, 43},
		{"发起人自己重复", []uint64{42, 42}, nil, trialReasonDuplicate, 42},
		{"不含发起人", []uint64{43, 44}, nil, trialReasonInitiatorMissing, 0},
		{"人数不足", []uint64{42}, nil, trialReasonSize, 0},
		{"空名单按人数不足之前的缺发起人答", nil, nil, trialReasonInitiatorMissing, 0},
		{"人数超限", []uint64{42, 43, 44, 45, 46, 47}, nil, trialReasonSize, 0},
		{"超限先于重复:超长名单不逐个看", []uint64{42, 43, 43, 43, 43, 43}, nil, trialReasonSize, 0},
		{"恰好下限", []uint64{42, 43}, []uint64{42, 43}, "", 0},
		{"恰好上限", []uint64{43, 44, 45, 46, 42}, []uint64{42, 43, 44, 45, 46}, "", 0},
		{"发起人挪到首位,其余保持相对顺序", []uint64{44, 42, 43}, []uint64{42, 44, 43}, "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := slices.Clone(tc.ids)

			roster, reason, offender := normalizeTrialRoster(42, tc.ids, 2, 5)

			assert.Equal(t, tc.wantRoster, roster)
			assert.Equal(t, tc.wantReason, reason)
			assert.Equal(t, tc.wantOffender, offender)
			assert.Equal(t, input, tc.ids, "不得改动调用方的切片")
		})
	}
}

// TestClampMatchBudget:预算 = min(MatchBudget, 剩余 − 500ms 尾巴),低于 300ms 不调 match。
// 800ms 是分界:剩余恰好 800ms 可调(给 match 300ms),差 1ms 就不调。
func TestClampMatchBudget(t *testing.T) {
	cases := []struct {
		name               string
		budget, remaining  time.Duration
		wantBudget         time.Duration
		wantEnoughForMatch bool
	}{
		{"余量充足取上限", 1500 * time.Millisecond, 5 * time.Second, 1500 * time.Millisecond, true},
		{"余量把预算截短", 1500 * time.Millisecond, 1200 * time.Millisecond, 700 * time.Millisecond, true},
		{"恰好 800ms 可调", 1500 * time.Millisecond, 800 * time.Millisecond, 300 * time.Millisecond, true},
		{"差 1ms 不调", 1500 * time.Millisecond, 799 * time.Millisecond, 299 * time.Millisecond, false},
		{"截止已过", 1500 * time.Millisecond, -time.Second, -1500 * time.Millisecond, false},
		{"上限本身小于下限", 100 * time.Millisecond, 5 * time.Second, 100 * time.Millisecond, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			budget, ok := clampMatchBudget(tc.budget, tc.remaining)

			assert.Equal(t, tc.wantBudget, budget)
			assert.Equal(t, tc.wantEnoughForMatch, ok)
		})
	}
}

// TestMatchBudgetForDeadline:没有截止按满额算;截止已过、或离截止至多 700ms(此后时间只会更少)→ 恒不够。
func TestMatchBudgetForDeadline(t *testing.T) {
	budget, ok := matchBudgetFor(context.Background(), 1500*time.Millisecond)
	assert.True(t, ok)
	assert.Equal(t, 1500*time.Millisecond, budget)

	expired, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancel()
	_, ok = matchBudgetFor(expired, 1500*time.Millisecond)
	assert.False(t, ok)

	short, cancelShort := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancelShort()
	_, ok = matchBudgetFor(short, 1500*time.Millisecond)
	assert.False(t, ok, "扣掉 500ms 尾巴后至多 200ms,低于 300ms 的下限")
}

// TestTrialMinMatchBudgetMirrorsConfig:config 不能 import logic,只能镜像"给 match 的最小预算"。两者分叉(下限调大而
// config 没跟)时,启动校验会放行一个小于 minMatchBudget 的 MatchRpc.Timeout:进程能起、历练显示开放、能建房能同意,
// 而每一次开战都判"预算不足"、一次也不调 match(TestLaunchSkipsMatchWhenBudgetTooSmall 演示的就是这种形态)。
func TestTrialMinMatchBudgetMirrorsConfig(t *testing.T) {
	assert.Equal(t, minMatchBudget, time.Duration(config.MinMatchRpcTimeoutMs)*time.Millisecond)
}

// TestTrialCandidates(06 §6.40,按用户决策 U2 改口径):候选 = team 0 − 逃跑者;**阵亡者仍在候选**;
// 对方队伍、0、重复 id 不进候选;结果升序。
func TestTrialCandidates(t *testing.T) {
	ev := &kafkapb.BattleResultEvent{
		Teams: []*kafkapb.BattleResultTeam{
			{TeamIndex: 0, PlayerIds: []uint64{44, 42, 0, 43, 42, 45}},
			{TeamIndex: 1, PlayerIds: []uint64{7, 8}},
		},
		FledPlayerIds: []uint64{43, 8},
		DeadPlayerIds: []uint64{44, 7},
	}

	assert.Equal(t, []uint64{42, 44, 45}, trialCandidates(ev), "43 逃跑出局;44 阵亡照样得奖;对方的 7、8 与 0 不算")
	assert.Equal(t, []uint64{44, 42, 43, 42, 45}, trialTeamZero(ev), "推送名单是己方全员(含逃跑者),只去掉 0")
	assert.Empty(t, trialCandidates(&kafkapb.BattleResultEvent{}))
	assert.Empty(t, trialCandidates(nil))
}

// ── 未开放 ────────────────────────────────────────────────────

// TestTrialClosedWhenAnyDependencyMissing:match / 房间 / 结果记录任一没接线,历练就是"未开放":
// 两个写 RPC 回 kGuildActivityNotOpen 且不碰仓储与 match,活动页里历练强制 DISABLED。
// 没有结果记录(= 结算后销不了账、巡检器兜不了底)也算缺依赖:没有结算就不许开战。
func TestTrialClosedWhenAnyDependencyMissing(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(d *ActivityDeps)
	}{
		{"没有 match", func(d *ActivityDeps) { d.Match = nil }},
		{"没有邀请房间", func(d *ActivityDeps) { d.Lobby = nil }},
		{"没有结果记录", func(d *ActivityDeps) { d.Results = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useTrialTables(t, nil, nil)
			f := newTrialFixture(t, activityGuild(1, 42, 43), tc.mutate)
			f.setOnline(t, 42, 43)

			start, err := f.start(42, 42, 43)
			require.NoError(t, err)
			assert.Equal(t, constants.ErrActivityNotOpen, start.GetErrorMessage().GetId())

			respond, err := f.respond(43, 1, true)
			require.NoError(t, err)
			assert.Equal(t, constants.ErrActivityNotOpen, respond.GetErrorMessage().GetId())

			assert.Zero(t, f.store.memberReads, "未开放时不该去核对名单")
			assert.Zero(t, f.match.callCount())

			page, err := f.l.GetGuildActivities(clientCtx(42), &pb.GetGuildActivitiesRequest{})
			require.NoError(t, err)
			require.Len(t, page.GetActivities(), 1)
			assert.Equal(t, pb.GuildActivityState_GUILD_ACTIVITY_STATE_DISABLED, page.GetActivities()[0].GetState())
			assert.Equal(t, constants.ErrActivityNotOpen, page.GetActivities()[0].GetBlockedTipId())
		})
	}
}

// ── 建房 ──────────────────────────────────────────────────────

// TestStartGuildTrialCreatesLobbyAndInvites(06 §6.24):建房成功 → 视图带 PENDING 房间(发起人在首位且已同意)、
// 逐个被邀请人各推一条 target = 被邀请人的 ACTIVITY_CHANGED;发起人自己不推。紧接着再建 → 冷却中,带剩余秒数。
func TestStartGuildTrialCreatesLobbyAndInvites(t *testing.T) {
	useTrialTables(t, nil, nil)
	f := newTrialFixture(t, activityGuild(1, 42, 43, 44), nil)
	f.setOnline(t, 42, 43, 44)

	resp, err := f.start(42, 43, 42, 44)

	require.NoError(t, err)
	require.Nil(t, resp.GetErrorMessage())
	view := resp.GetActivity()
	require.NotNil(t, view)
	assert.Equal(t, pb.GuildActivityState_GUILD_ACTIVITY_STATE_OPEN, view.GetState(), "依赖齐全时历练按配表开放")
	lobby := view.GetTrialLobby()
	require.NotNil(t, lobby)
	assert.NotZero(t, lobby.GetLobbyId())
	assert.Equal(t, uint64(42), lobby.GetInitiatorPlayerId())
	assert.Equal(t, []uint64{42, 43, 44}, lobby.GetMemberPlayerIds(), "发起人挪到首位")
	assert.Equal(t, []uint64{42}, lobby.GetAcceptedPlayerIds(), "发起人建房即同意")
	assert.Equal(t, pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_PENDING, lobby.GetState())
	assert.Equal(t, f.nowMs()+30_000, lobby.GetExpireAtMs(), "邀请有效期取 GuildRule.trial_invite_ttl_seconds")

	pushes := f.activityPushes()
	require.Len(t, pushes, 2)
	for i, invitee := range []uint64{43, 44} {
		assert.Equal(t, []uint64{invitee}, pushes[i].recipients)
		assert.Equal(t, invitee, pushes[i].change.GetTargetPlayerId(), "target = 被邀请人,客户端据此弹邀请")
		assert.Equal(t, uint64(42), pushes[i].change.GetActorPlayerId())
		assert.Equal(t, trialTestGuildID, pushes[i].change.GetGuildId())
	}

	again, err := f.start(42, 42, 43)
	require.NoError(t, err)
	assert.Equal(t, constants.ErrTrialInviteCooldown, again.GetErrorMessage().GetId())
	assert.Equal(t, []string{"10"}, again.GetErrorMessage().GetParameters(), "剩余秒数向上取整")
	assert.Nil(t, again.GetActivity())
	assert.Len(t, f.activityPushes(), 2, "被冷却挡下的建房不推送")
}

// TestStartGuildTrialRosterRejections:名单不合法一律回 kGuildTrialTeamInvalid [reason, player_id],
// 且都发生在建房之前 —— 不建房、不消耗冷却、不推送(06 §6.24 末段)。
// "非成员且离线"只答 not_member:在线状态只对确认是本帮成员的人作答,否则可以拿名单探测任意玩家在不在线。
func TestStartGuildTrialRosterRejections(t *testing.T) {
	cases := []struct {
		name       string
		roster     []uint64
		prepare    func(f *trialFixture)
		rule       func(r *tablepb.GuildRuleTable)
		wantParams []string
	}{
		{"重复", []uint64{42, 43, 43}, nil, nil, []string{"duplicate", "43"}},
		{"含 0", []uint64{42, 0}, nil, nil, []string{"duplicate", "0"}},
		{"不含发起人", []uint64{43, 44}, nil, nil, []string{"initiator_missing", "0"}},
		{"人数不足", []uint64{42}, nil, nil, []string{"size", "0"}},
		{"人数超限", []uint64{42, 43, 44, 45, 46, 47}, nil, nil, []string{"size", "0"}},
		{"不是本帮成员", []uint64{42, 43, 44}, func(f *trialFixture) { delete(f.store.members, 44) }, nil, []string{"not_member", "44"}},
		{"非成员且离线只答不是成员", []uint64{42, 99}, nil, nil, []string{"not_member", "99"}},
		{"入帮未满 N 小时", []uint64{42, 43}, func(f *trialFixture) { f.store.members[43] = activityTestNowMs() - 3_600_000 },
			func(r *tablepb.GuildRuleTable) { r.ActivityJoinMinHours = 24 }, []string{"join_recent", "43"}},
		{"不在线", []uint64{42, 43, 44}, func(f *trialFixture) { f.locator.Del(playerSessionKey(44)) }, nil, []string{"offline", "44"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rule := activityTestRule()
			if tc.rule != nil {
				tc.rule(rule)
			}
			useTrialTables(t, nil, rule)
			f := newTrialFixture(t, activityGuild(1, 42, 43, 44, 45, 46, 47), nil)
			f.setOnline(t, 42, 43, 44, 45, 46, 47)
			if tc.prepare != nil {
				tc.prepare(f)
			}

			resp, err := f.start(42, tc.roster...)

			require.NoError(t, err, "名单不合法是业务拒绝,不是 gRPC 错误")
			assert.Equal(t, constants.ErrTrialTeamInvalid, resp.GetErrorMessage().GetId())
			assert.Equal(t, tc.wantParams, resp.GetErrorMessage().GetParameters())
			assert.Nil(t, resp.GetActivity())
			assert.False(t, f.lobbyCreated(), "名单不合法不建房")
			assert.False(t, f.lobbies.Exists("guild:trial:cooldown:42"), "名单不合法不消耗冷却")
			assert.Empty(t, f.activityPushes())
		})
	}
}

// TestStartGuildTrialBusyMember:被邀请人还在另一个有效房间里 → [busy, 那个人];不影响那个房间。
func TestStartGuildTrialBusyMember(t *testing.T) {
	useTrialTables(t, nil, nil)
	f := newTrialFixture(t, activityGuild(1, 42, 43, 44), nil)
	f.setOnline(t, 42, 43, 44)
	first := f.mustStart(t, 42, 42, 43)

	resp, err := f.start(44, 44, 43)

	require.NoError(t, err)
	assert.Equal(t, constants.ErrTrialTeamInvalid, resp.GetErrorMessage().GetId())
	assert.Equal(t, []string{"busy", "43"}, resp.GetErrorMessage().GetParameters())

	// 43 仍然能应答原来的房间。
	accept, err := f.respond(43, first.GetLobbyId(), false)
	require.NoError(t, err)
	assert.Nil(t, accept.GetErrorMessage())
}

// TestStartGuildTrialInitiatorPrechecks:发起人自己的门槛与灯会 / 团圆同一套预检、同一优先级。
func TestStartGuildTrialInitiatorPrechecks(t *testing.T) {
	t.Run("请求的 id 不是当前选中行", func(t *testing.T) {
		useTrialTables(t, nil, nil)
		f := newTrialFixture(t, activityGuild(1, 42, 43), nil)
		f.setOnline(t, 42, 43)

		resp, err := f.l.StartGuildTrial(clientCtx(42), &pb.StartGuildTrialRequest{ActivityId: 99, MemberPlayerIds: []uint64{42, 43}})

		require.NoError(t, err)
		assert.Equal(t, constants.ErrActivityNotOpen, resp.GetErrorMessage().GetId())
		assert.Zero(t, f.store.memberReads)
	})
	t.Run("帮会等级不足带 [min_level]", func(t *testing.T) {
		row := trialRow()
		row.MinGuildLevel = 3
		useTrialTables(t, row, nil)
		f := newTrialFixture(t, activityGuild(2, 42, 43), nil)
		f.setOnline(t, 42, 43)

		resp, err := f.start(42, 42, 43)

		require.NoError(t, err)
		assert.Equal(t, constants.ErrActivityLevelTooLow, resp.GetErrorMessage().GetId())
		assert.Equal(t, []string{"3"}, resp.GetErrorMessage().GetParameters())
		assert.False(t, f.lobbyCreated())
	})
	t.Run("发起人入帮未满 N 小时带 [N]", func(t *testing.T) {
		rule := activityTestRule()
		rule.ActivityJoinMinHours = 24
		useTrialTables(t, nil, rule)
		g := activityGuild(1, 42, 43)
		g.Members[0].JoinTimeMs = activityTestNowMs() - 3_600_000
		f := newTrialFixture(t, g, nil)
		f.setOnline(t, 42, 43)

		resp, err := f.start(42, 42, 43)

		require.NoError(t, err)
		assert.Equal(t, constants.ErrActivityJoinTooRecent, resp.GetErrorMessage().GetId())
		assert.Equal(t, []string{"24"}, resp.GetErrorMessage().GetParameters())
	})
}

// TestTrialRefusedWhileZoneIsMerging:帮会所在 zone 正在合服(或闸门读不到)时不许发起、不许投"同意"票
// (它可能就是凑齐开战的那一票),回 kGuildZoneMerging;拒绝 / 取消照常,房间能被主动解散。
// 合服窗口里开出去的对局,结果会把结果消费卡在那条消息上直到合服结束。
func TestTrialRefusedWhileZoneIsMerging(t *testing.T) {
	useTrialTables(t, nil, nil)
	f := newTrialFixture(t, activityGuild(1, 42, 43, 44), nil)
	f.setOnline(t, 42, 43, 44)
	lobby := f.mustStart(t, 42, 42, 43)

	f.fence.merging = true
	start, err := f.start(44, 44, 42)
	require.NoError(t, err)
	assert.Equal(t, constants.ErrZoneMerging, start.GetErrorMessage().GetId())
	assert.Equal(t, uint32(2), f.fence.lastZone, "按帮会所在 zone 查闸门")

	accept, err := f.respond(43, lobby.GetLobbyId(), true)
	require.NoError(t, err)
	assert.Equal(t, constants.ErrZoneMerging, accept.GetErrorMessage().GetId())
	assert.Zero(t, f.match.callCount(), "合服期间不开战")

	f.fence.merging, f.fence.err = false, errors.New("redis: connection refused")
	unreadable, err := f.respond(43, lobby.GetLobbyId(), true)
	require.NoError(t, err)
	assert.Equal(t, constants.ErrZoneMerging, unreadable.GetErrorMessage().GetId(), "闸门读不到按封锁处理")

	f.fence.merging, f.fence.err = true, nil
	decline, err := f.respond(43, lobby.GetLobbyId(), false)
	require.NoError(t, err)
	require.Nil(t, decline.GetErrorMessage(), "拒绝不受闸门限制")
	assert.Equal(t, pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_ENDED, decline.GetActivity().GetTrialLobby().GetState())
}

// TestStartGuildTrialRewardChannel:历练配了物品而资产通道关闭 / 发号器没接线 → 建房之前拒绝(fail-closed):
// 放这一局开打,打赢之后物品要么插进去没人投递、要么发不出号。没配物品的历练不受通道开关影响。
func TestStartGuildTrialRewardChannel(t *testing.T) {
	t.Run("带物品且通道关闭", func(t *testing.T) {
		useTrialTables(t, trialRowWithReward(), nil)
		f := newTrialFixture(t, activityGuild(1, 42, 43), func(d *ActivityDeps) { d.OpIDs = &seqMinter{next: 1} })
		f.setOnline(t, 42, 43)

		resp, err := f.start(42, 42, 43)

		require.NoError(t, err)
		assert.Equal(t, constants.ErrAssetPending, resp.GetErrorMessage().GetId())
		assert.False(t, f.lobbyCreated())
	})
	t.Run("带物品但发号器未接线", func(t *testing.T) {
		useTrialTables(t, trialRowWithReward(), nil)
		loop, _, _ := newCountingLoop(t)
		f := newTrialFixture(t, activityGuild(1, 42, 43), func(d *ActivityDeps) { d.Loop = loop })
		f.setOnline(t, 42, 43)

		resp, err := f.start(42, 42, 43)

		require.NoError(t, err)
		assert.Equal(t, constants.ErrIDGenUnavailable, resp.GetErrorMessage().GetId())
		assert.False(t, f.lobbyCreated())
	})
	t.Run("带物品且通道齐全", func(t *testing.T) {
		useTrialTables(t, trialRowWithReward(), nil)
		loop, _, _ := newCountingLoop(t)
		f := newTrialFixture(t, activityGuild(1, 42, 43), func(d *ActivityDeps) {
			d.Loop, d.OpIDs = loop, &seqMinter{next: 1}
		})
		f.setOnline(t, 42, 43)

		f.mustStart(t, 42, 42, 43)
	})
}

// TestStartGuildTrialOwnFaultsAreErrors:guild 自己的库 / Redis 故障回 gRPC 错误(不折成业务提示,也不当成"不在帮 / 离线"),
// 且不建房(06 §6.10 末行、§6.27 L7)。
func TestStartGuildTrialOwnFaultsAreErrors(t *testing.T) {
	t.Run("成员核对读库失败", func(t *testing.T) {
		useTrialTables(t, nil, nil)
		f := newTrialFixture(t, activityGuild(1, 42, 43), nil)
		f.setOnline(t, 42, 43)
		f.store.membersErr = errors.New("driver: bad connection")

		resp, err := f.start(42, 42, 43)

		require.Error(t, err)
		assert.Nil(t, resp)
		assert.False(t, f.lobbyCreated())
	})
	t.Run("在线状态读不到", func(t *testing.T) {
		useTrialTables(t, nil, nil)
		f := newTrialFixture(t, activityGuild(1, 42, 43), nil)
		f.locator.Close()

		resp, err := f.start(42, 42, 43)

		require.ErrorIs(t, err, ErrOnlineStateUnknown)
		assert.Nil(t, resp)
		assert.False(t, f.lobbyCreated())
	})
	t.Run("房间 Redis 不可用", func(t *testing.T) {
		useTrialTables(t, nil, nil)
		f := newTrialFixture(t, activityGuild(1, 42, 43), nil)
		f.setOnline(t, 42, 43)
		f.lobbies.Close()

		resp, err := f.start(42, 42, 43)

		require.Error(t, err)
		assert.Nil(t, resp)
		assert.Empty(t, f.activityPushes())
	})
}

// ── 同意 / 拒绝 / 取消 ────────────────────────────────────────

// TestRespondAcceptAndDecline(06 §6.25):同意 → 记下并通知名单里的其他人;有人拒绝 → 房间结束、原因带拒绝者;
// 之后:此前已同意的人再点是幂等成功,没表过态的人拿到"邀请已失效"。
func TestRespondAcceptAndDecline(t *testing.T) {
	useTrialTables(t, nil, nil)
	f := newTrialFixture(t, activityGuild(1, 42, 43, 44, 45), nil)
	f.setOnline(t, 42, 43, 44, 45)
	lobby := f.mustStart(t, 42, 42, 43, 44, 45)
	invitePushes := len(f.activityPushes())

	accepted, err := f.respond(43, lobby.GetLobbyId(), true)
	require.NoError(t, err)
	require.Nil(t, accepted.GetErrorMessage())
	view := accepted.GetActivity().GetTrialLobby()
	require.NotNil(t, view)
	assert.Equal(t, []uint64{42, 43}, view.GetAcceptedPlayerIds())
	assert.Equal(t, pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_PENDING, view.GetState())
	pushes := f.activityPushes()
	require.Len(t, pushes, invitePushes+1)
	assert.Equal(t, []uint64{42, 44, 45}, pushes[invitePushes].recipients, "通知名单里除自己以外的人")
	assert.Zero(t, pushes[invitePushes].change.GetTargetPlayerId())
	assert.Zero(t, f.match.callCount(), "还没凑齐,不开战")

	declined, err := f.respond(44, lobby.GetLobbyId(), false)
	require.NoError(t, err)
	require.Nil(t, declined.GetErrorMessage(), "拒绝是一次成功的响应")
	view = declined.GetActivity().GetTrialLobby()
	require.NotNil(t, view)
	assert.Equal(t, pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_ENDED, view.GetState())
	assert.Equal(t, constants.ErrTrialInviteDeclined, view.GetEndTipId())
	assert.Equal(t, []string{"44"}, view.GetEndParameters(), "结束原因带拒绝者")
	assert.Equal(t, []uint64{42, 43, 45}, f.activityPushes()[invitePushes+1].recipients)

	repeat, err := f.respond(43, lobby.GetLobbyId(), true)
	require.NoError(t, err)
	assert.Nil(t, repeat.GetErrorMessage(), "此前已同意的人重复点击按成功处理")
	assert.Equal(t, pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_ENDED, repeat.GetActivity().GetTrialLobby().GetState())

	late, err := f.respond(45, lobby.GetLobbyId(), true)
	require.NoError(t, err)
	assert.Equal(t, constants.ErrTrialInviteExpired, late.GetErrorMessage().GetId(), "房间已结束,没表过态的人不能再同意")
	assert.Zero(t, f.match.callCount())
}

// TestRespondInitiatorCancels:发起人 accept=false = 取消房间。
func TestRespondInitiatorCancels(t *testing.T) {
	useTrialTables(t, nil, nil)
	f := newTrialFixture(t, activityGuild(1, 42, 43), nil)
	f.setOnline(t, 42, 43)
	lobby := f.mustStart(t, 42, 42, 43)

	resp, err := f.respond(42, lobby.GetLobbyId(), false)

	require.NoError(t, err)
	require.Nil(t, resp.GetErrorMessage())
	view := resp.GetActivity().GetTrialLobby()
	assert.Equal(t, pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_ENDED, view.GetState())
	assert.Equal(t, []string{"42"}, view.GetEndParameters())
}

// TestRespondInviteNoLongerValid:房间不存在 / 不在名单 / 属于别的帮 / 已过期,一律同一句"邀请已失效",
// 不向调用者透露别人的房间状态;都不会走到开战。
func TestRespondInviteNoLongerValid(t *testing.T) {
	useTrialTables(t, nil, nil)
	f := newTrialFixture(t, activityGuild(1, 42, 43, 44), nil)
	f.setOnline(t, 42, 43, 44)
	lobby := f.mustStart(t, 42, 42, 43)

	requireExpired := func(t *testing.T, resp *pb.RespondGuildTrialInviteResponse, err error) {
		t.Helper()
		require.NoError(t, err)
		assert.Equal(t, constants.ErrTrialInviteExpired, resp.GetErrorMessage().GetId())
		assert.Nil(t, resp.GetActivity())
	}

	t.Run("房间不存在", func(t *testing.T) {
		resp, err := f.respond(43, lobby.GetLobbyId()+1000, true)
		requireExpired(t, resp, err)
	})
	t.Run("不在名单里", func(t *testing.T) {
		resp, err := f.respond(44, lobby.GetLobbyId(), true)
		requireExpired(t, resp, err)
	})
	t.Run("别的帮的房间", func(t *testing.T) {
		// 直接经房间仓储造一个帮会 10 的房间,名单里恰好有 44。
		other, err := f.deps.Lobby.Create(context.Background(), data.TrialLobbyCreate{
			GuildID: 10, ActivityID: trialTestActivityID, Roster: []uint64{7001, 44}, TTL: 30 * time.Second, NowMs: f.nowMs(),
		})
		require.NoError(t, err)
		require.Equal(t, data.TrialLobbyCreated, other.Status)

		resp, err := f.respond(44, other.Lobby.LobbyID, true)
		requireExpired(t, resp, err)
	})
	t.Run("邀请已过期", func(t *testing.T) {
		f.clock.advance(31 * time.Second)
		resp, err := f.respond(43, lobby.GetLobbyId(), true)
		requireExpired(t, resp, err)
	})
	assert.Zero(t, f.match.callCount())
}

// ── 确认开战 ──────────────────────────────────────────────────

// TestRespondLastAcceptLaunches(06 §6.26):凑齐最后一票的那次调用负责开战 —— 把名单(发起人在首位)、副本与
// 活动上下文(两个周期键取开战时刻)交给 match,拿到 battle_id 后登记对局行、房间进 LAUNCHED,并通知其他人。
// 调 match 的 ctx 带截止、不带出站 metadata(match 对带会话的调用回 PermissionDenied)。重复点击不会再开一局。
func TestRespondLastAcceptLaunches(t *testing.T) {
	useTrialTables(t, nil, nil)
	f := newTrialFixture(t, activityGuild(1, 42, 43, 44), nil)
	f.setOnline(t, 42, 43, 44)
	lobby := f.mustStart(t, 42, 44, 42, 43)
	_, err := f.respond(44, lobby.GetLobbyId(), true)
	require.NoError(t, err)
	pushesBefore := len(f.activityPushes())

	resp, err := f.respond(43, lobby.GetLobbyId(), true)

	require.NoError(t, err)
	require.Nil(t, resp.GetErrorMessage())
	view := resp.GetActivity().GetTrialLobby()
	require.NotNil(t, view)
	assert.Equal(t, pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_LAUNCHED, view.GetState())
	assert.Equal(t, uint64(777), view.GetBattleId())
	assert.Zero(t, view.GetEndTipId())

	require.Equal(t, 1, f.match.callCount())
	day := activity.DayKey(activityTestNow)
	req := f.match.requests[0]
	assert.Equal(t, uint32(1), req.GetBattleConfigId(), "副本取 GuildActivity.dungeon_id")
	assert.Equal(t, []uint64{42, 44, 43}, req.GetMemberPlayerIds(), "站位顺序 = 房间名单,发起人在首位")
	actx := req.GetActivityContext()
	assert.Equal(t, battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_GUILD_TRIAL, actx.GetKind())
	assert.Equal(t, trialTestGuildID, actx.GetGuildId())
	assert.Equal(t, trialTestActivityID, actx.GetActivityId())
	assert.Equal(t, day, actx.GetPeriodKey())
	assert.Equal(t, day, actx.GetGuildPeriodKey(), "历练的帮会期键 = 游戏日键")
	assert.Equal(t, uint64(42), actx.GetInitiatorPlayerId())
	assert.Equal(t, []bool{true}, f.match.hadDeadline, "调 match 必须带预算")
	assert.Equal(t, []bool{false}, f.match.hadOutMD, "不得把客户端会话 metadata 转给 match")

	assert.Equal(t, []data.TrialBattleKey{{
		BattleID: 777, GuildID: trialTestGuildID, ActivityID: trialTestActivityID,
		PeriodKey: day, GuildPeriodKey: day, InitiatorPlayerID: 42,
	}}, f.store.registered, "登记的行与发给 match 的上下文逐字段相同")

	pushes := f.activityPushes()
	require.Len(t, pushes, pushesBefore+1)
	assert.Equal(t, []uint64{42, 44}, pushes[pushesBefore].recipients)

	repeat, err := f.respond(43, lobby.GetLobbyId(), true)
	require.NoError(t, err)
	assert.Nil(t, repeat.GetErrorMessage())
	assert.Equal(t, 1, f.match.callCount(), "重复点击不会再开一局")
}

// TestLaunchUsesKeysOfLaunchTime:周期键取**确认开战**那一刻,不取建房那一刻(房间可以跨过 05:00 的切日点)。
func TestLaunchUsesKeysOfLaunchTime(t *testing.T) {
	useTrialTables(t, nil, nil)
	f := newTrialFixture(t, activityGuild(1, 42, 43), nil)
	f.setOnline(t, 42, 43)
	// UTC+8 04:59:50 建房,15 秒后(05:00:05,已是下一个游戏日)同意。
	f.clock.now = time.Date(2026, 9, 28, 20, 59, 50, 0, time.UTC)
	lobby := f.mustStart(t, 42, 42, 43)
	beforeReset := activity.DayKey(f.clock.Now())
	f.clock.advance(15 * time.Second)
	afterReset := activity.DayKey(f.clock.Now())
	require.NotEqual(t, beforeReset, afterReset, "用例前提:两次调用落在切日点两侧")

	_, err := f.respond(43, lobby.GetLobbyId(), true)

	require.NoError(t, err)
	require.Len(t, f.store.registered, 1)
	assert.Equal(t, afterReset, f.store.registered[0].PeriodKey)
	assert.Equal(t, afterReset, f.match.requests[0].GetActivityContext().GetPeriodKey())
}

// TestLaunchRegisterFailureStillSucceeds(06 §6.26 第 8 步、§6.27 L4):match 已回 battle_id 就是开战成功,
// 登记失败只打日志计数,不让本次 RPC 失败(结算会补登记);房间照样进 LAUNCHED。
func TestLaunchRegisterFailureStillSucceeds(t *testing.T) {
	for _, registerErr := range []error{errors.New("driver: bad connection"), data.ErrGuildGone} {
		t.Run(registerErr.Error(), func(t *testing.T) {
			useTrialTables(t, nil, nil)
			f := newTrialFixture(t, activityGuild(1, 42, 43), nil)
			f.setOnline(t, 42, 43)
			f.store.registerErr = registerErr
			lobby := f.mustStart(t, 42, 42, 43)

			resp, err := f.respond(43, lobby.GetLobbyId(), true)

			require.NoError(t, err)
			require.Nil(t, resp.GetErrorMessage())
			assert.Equal(t, pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_LAUNCHED, resp.GetActivity().GetTrialLobby().GetState())
			assert.Equal(t, uint64(777), resp.GetActivity().GetTrialLobby().GetBattleId())
		})
	}
}

// launchScenario 建一个两人房间并让 43 同意(触发开战),返回响应。prepare 在建房之后、同意之前执行;
// 它拿到的是**当前用例**的 t:里面若再换配表,还原动作必须登记在同一个 t 上,才会按后进先出的次序换回去。
func launchScenario(t *testing.T, mutate func(d *ActivityDeps), prepare func(t *testing.T, f *trialFixture)) (*trialFixture, *pb.RespondGuildTrialInviteResponse, error) {
	t.Helper()
	useTrialTables(t, nil, nil)
	f := newTrialFixture(t, activityGuild(1, 42, 43), mutate)
	f.setOnline(t, 42, 43)
	lobby := f.mustStart(t, 42, 42, 43)
	if prepare != nil {
		prepare(t, f)
	}
	resp, err := f.respond(43, lobby.GetLobbyId(), true)
	return f, resp, err
}

// requireLaunchEnded:开战失败的统一断言 —— 响应同时带 error_message(原因)与视图(房间已 ENDED、原因一致),
// 没有登记任何对局,并通知了名单里的其他人。wantParams 为 nil 表示该原因没有占位参数(房间里不存参数)。
func requireLaunchEnded(t *testing.T, f *trialFixture, resp *pb.RespondGuildTrialInviteResponse, err error, wantTip uint32, wantParams []string) {
	t.Helper()
	require.NoError(t, err, "开战失败回 tip,不回 gRPC 错误(否则客户端进重连隔离)")
	assert.Equal(t, wantTip, resp.GetErrorMessage().GetId())
	if wantParams != nil {
		assert.Equal(t, wantParams, resp.GetErrorMessage().GetParameters())
	}
	lobby := resp.GetActivity().GetTrialLobby()
	require.NotNil(t, lobby, "开战失败的响应要带视图:客户端先应用视图再显示原因")
	assert.Equal(t, pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_ENDED, lobby.GetState())
	assert.Equal(t, wantTip, lobby.GetEndTipId())
	assert.Equal(t, wantParams, lobby.GetEndParameters())
	assert.Zero(t, lobby.GetBattleId())
	assert.Empty(t, f.store.registered, "没开成就不登记对局")
	pushes := f.activityPushes()
	assert.Equal(t, []uint64{42}, pushes[len(pushes)-1].recipients, "通知名单里的其他人房间已结束")
}

// TestLaunchMatchFailuresBecomeTips(06 §6.40 的 TestLaunchMatchBudget + §6.26 第 7 步):match 的任何失败都以 tip 收场。
// gRPC 错误 / INTERNAL / 不认识的拒绝码 / 回成功却没有 battle_id → kGuildTrialServiceBusy;
// 成员不满足 → kGuildTrialTeamInvalid [原因, 当事人];请求非法 → [invalid, 0]。
func TestLaunchMatchFailuresBecomeTips(t *testing.T) {
	reject := func(code matchpb.ActivityBattleReject, offender uint64) func(*testing.T, *trialFixture) {
		return func(_ *testing.T, f *trialFixture) {
			f.match.resp = &matchpb.StartActivityBattleResponse{Reject: code, OffenderPlayerId: offender}
		}
	}
	cases := []struct {
		name       string
		prepare    func(t *testing.T, f *trialFixture)
		wantTip    uint32
		wantParams []string
	}{
		{"gRPC Unavailable", func(_ *testing.T, f *trialFixture) {
			f.match.resp, f.match.err = nil, status.Error(codes.Unavailable, "match is down")
		}, constants.ErrTrialServiceBusy, nil},
		{"调用超时", func(_ *testing.T, f *trialFixture) {
			f.match.resp, f.match.err = nil, context.DeadlineExceeded
		}, constants.ErrTrialServiceBusy, nil},
		{"match 内部故障", reject(matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL, 0), constants.ErrTrialServiceBusy, nil},
		{"不认识的拒绝码", reject(matchpb.ActivityBattleReject(99), 0), constants.ErrTrialServiceBusy, nil},
		{"回成功却没有 battle_id", func(_ *testing.T, f *trialFixture) {
			f.match.resp = &matchpb.StartActivityBattleResponse{}
		}, constants.ErrTrialServiceBusy, nil},
		{"成员离线", reject(matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_OFFLINE, 43),
			constants.ErrTrialTeamInvalid, []string{"offline", "43"}},
		{"成员在战斗中", reject(matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_IN_BATTLE, 42),
			constants.ErrTrialTeamInvalid, []string{"in_battle", "42"}},
		{"成员未就绪", reject(matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_NOT_READY, 43),
			constants.ErrTrialTeamInvalid, []string{"not_ready", "43"}},
		{"请求非法", reject(matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INVALID_ARGUMENT, 0),
			constants.ErrTrialTeamInvalid, []string{"invalid", "0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, resp, err := launchScenario(t, nil, tc.prepare)

			requireLaunchEnded(t, f, resp, err, tc.wantTip, tc.wantParams)
			assert.Equal(t, 1, f.match.callCount())
		})
	}
}

// TestLaunchSkipsMatchWhenBudgetTooSmall:给 match 的预算不足 300ms 就不调它,房间以服务繁忙结束。
// 用"预算上限本身小于下限"构造,结论与请求耗时无关(剩余时间的边界由 TestClampMatchBudget 以数值覆盖)。
func TestLaunchSkipsMatchWhenBudgetTooSmall(t *testing.T) {
	f, resp, err := launchScenario(t, func(d *ActivityDeps) { d.MatchBudget = 100 * time.Millisecond }, nil)

	requireLaunchEnded(t, f, resp, err, constants.ErrTrialServiceBusy, nil)
	assert.Zero(t, f.match.callCount(), "预算不够不调 match")
}

// TestLaunchRechecksBeforeCallingMatch(06 §6.26 第 1–4 步):建房之后变了的条件在调 match 之前被复核出来,
// 房间带着具体原因结束,match 一次都不会被调。
func TestLaunchRechecksBeforeCallingMatch(t *testing.T) {
	cases := []struct {
		name       string
		prepare    func(t *testing.T, f *trialFixture)
		wantTip    uint32
		wantParams []string
	}{
		{"发起人已下线", func(_ *testing.T, f *trialFixture) { f.locator.Del(playerSessionKey(42)) },
			constants.ErrTrialTeamInvalid, []string{"offline", "42"}},
		{"发起人已被踢出帮会", func(_ *testing.T, f *trialFixture) { delete(f.store.members, 42) },
			constants.ErrTrialTeamInvalid, []string{"not_member", "42"}},
		{"活动已关闭", func(t *testing.T, _ *trialFixture) {
			closed := trialRow()
			closed.Enabled = false
			useTrialTables(t, closed, nil)
		}, constants.ErrActivityNotOpen, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, resp, err := launchScenario(t, nil, tc.prepare)

			requireLaunchEnded(t, f, resp, err, tc.wantTip, tc.wantParams)
			assert.Zero(t, f.match.callCount())
		})
	}
}

// TestLaunchOwnFaultEndsLobbyAndReturnsError:开战前复核时 guild 自己的库读失败 → 返回 gRPC 错误,
// 但房间不留在"开战中":立刻以服务繁忙结束,别人不必等 5 秒。
func TestLaunchOwnFaultEndsLobbyAndReturnsError(t *testing.T) {
	useTrialTables(t, nil, nil)
	f := newTrialFixture(t, activityGuild(1, 42, 43), nil)
	f.setOnline(t, 42, 43)
	lobby := f.mustStart(t, 42, 42, 43)
	f.store.membersErr = errors.New("driver: bad connection")

	resp, err := f.respond(43, lobby.GetLobbyId(), true)

	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Zero(t, f.match.callCount())
	stored, loadErr := f.deps.Lobby.Load(context.Background(), lobby.GetLobbyId())
	require.NoError(t, loadErr)
	require.NotNil(t, stored)
	assert.Equal(t, pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_ENDED, stored.State)
	assert.Equal(t, constants.ErrTrialServiceBusy, stored.EndTipID)
	pushes := f.activityPushes()
	assert.Equal(t, []uint64{42}, pushes[len(pushes)-1].recipients, "其他人要被告知房间已结束")
}

// ── 视图 ──────────────────────────────────────────────────────

// trialPage 以 playerID 身份拉活动页,返回历练那一行。
func trialPage(t *testing.T, f *trialFixture, playerID uint64) *pb.GuildActivityView {
	t.Helper()
	resp, err := f.l.GetGuildActivities(clientCtx(playerID), &pb.GetGuildActivitiesRequest{})
	require.NoError(t, err)
	require.Nil(t, resp.GetErrorMessage())
	require.Len(t, resp.GetActivities(), 1)
	return resp.GetActivities()[0]
}

// TestMyTrialBattleIDFromLock(06 §6.40):"进行中"以战斗锁为准 —— 锁的值是一局仍为 STARTED 的历练对局才显示;
// 锁不在、对局已结算、锁指向的不是历练对局、锁的值不是数字 → 0。会话 Redis 读不到只降级,不让整页失败。
func TestMyTrialBattleIDFromLock(t *testing.T) {
	started := data.TrialBattleRow{TrialBattleKey: data.TrialBattleKey{BattleID: 77, GuildID: trialTestGuildID},
		State: pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_STARTED}
	settled := started
	settled.State = pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_SETTLED
	expired := started
	expired.State = pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_EXPIRED
	const lockKey = "battle:lock:42"

	cases := []struct {
		name    string
		prepare func(t *testing.T, f *trialFixture)
		want    uint64
	}{
		{"锁指向 STARTED 的历练对局", func(t *testing.T, f *trialFixture) {
			require.NoError(t, f.locator.Set(lockKey, "77"))
			f.store.battles = map[uint64]data.TrialBattleRow{77: started}
		}, 77},
		{"没有战斗锁", func(t *testing.T, f *trialFixture) {
			f.store.battles = map[uint64]data.TrialBattleRow{77: started}
		}, 0},
		{"对局已结算", func(t *testing.T, f *trialFixture) {
			require.NoError(t, f.locator.Set(lockKey, "77"))
			f.store.battles = map[uint64]data.TrialBattleRow{77: settled}
		}, 0},
		{"对局已被巡检器判过期", func(t *testing.T, f *trialFixture) {
			require.NoError(t, f.locator.Set(lockKey, "77"))
			f.store.battles = map[uint64]data.TrialBattleRow{77: expired}
		}, 0},
		{"锁指向的不是历练对局", func(t *testing.T, f *trialFixture) {
			require.NoError(t, f.locator.Set(lockKey, "88"))
		}, 0},
		{"锁的值不是数字", func(t *testing.T, f *trialFixture) {
			require.NoError(t, f.locator.Set(lockKey, "not-a-battle"))
		}, 0},
		{"会话 Redis 不可用时降级", func(t *testing.T, f *trialFixture) {
			f.locator.Close()
		}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useTrialTables(t, nil, nil)
			f := newTrialFixture(t, activityGuild(1, 42, 43), nil)
			tc.prepare(t, f)

			view := trialPage(t, f, 42)

			assert.Equal(t, tc.want, view.GetMyTrialBattleId())
			assert.Equal(t, pb.GuildActivityState_GUILD_ACTIVITY_STATE_OPEN, view.GetState())
		})
	}
}

// TestTrialPageShowsInviteAndOwed:被邀请人打开活动页就能看到邀请(推送丢了也不要紧);别的帮的房间不显示;
// 待入队物品计入"待发放";房间 Redis 读不到只降级为"没有邀请"。
func TestTrialPageShowsInviteAndOwed(t *testing.T) {
	useTrialTables(t, nil, nil)
	f := newTrialFixture(t, activityGuild(1, 42, 43, 44), nil)
	f.setOnline(t, 42, 43, 44)
	f.store.owedCounts = map[uint32]uint32{trialTestActivityID: 2}
	lobby := f.mustStart(t, 42, 42, 43)

	invited := trialPage(t, f, 43)
	require.NotNil(t, invited.GetTrialLobby())
	assert.Equal(t, lobby.GetLobbyId(), invited.GetTrialLobby().GetLobbyId())
	assert.Equal(t, uint32(2), invited.GetMyPendingRewardCount(), "待入队行也是还在路上的奖励")

	assert.Nil(t, trialPage(t, f, 44).GetTrialLobby(), "没被邀请的人看不到房间")

	// 44 被另一个帮会(10)的房间占着:不属于本帮本活动,不显示。
	other, err := f.deps.Lobby.Create(context.Background(), data.TrialLobbyCreate{
		GuildID: 10, ActivityID: trialTestActivityID, Roster: []uint64{7001, 44}, TTL: 30 * time.Second, NowMs: f.nowMs(),
	})
	require.NoError(t, err)
	require.Equal(t, data.TrialLobbyCreated, other.Status)
	assert.Nil(t, trialPage(t, f, 44).GetTrialLobby())

	// 邀请过期后折算成 ENDED + 已过期,客户端不必自己判超时。
	f.clock.advance(31 * time.Second)
	expired := trialPage(t, f, 43).GetTrialLobby()
	require.NotNil(t, expired)
	assert.Equal(t, pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_ENDED, expired.GetState())
	assert.Equal(t, constants.ErrTrialInviteExpired, expired.GetEndTipId())

	f.lobbies.Close()
	assert.Nil(t, trialPage(t, f, 43).GetTrialLobby(), "房间 Redis 读不到只降级,不让整页失败")
}

// ── 结算 ──────────────────────────────────────────────────────

// TestSettleSkipsEventsThatAreNotGuildTrials:没有活动上下文、kind=NONE、或是别的活动类型的结果与 guild 无关 ——
// 不读不写、**不销账**(别的活动的结果记录不归 guild 删),未接线时也照样跳过(普通对局的结果占 topic 的绝大多数)。
func TestSettleSkipsEventsThatAreNotGuildTrials(t *testing.T) {
	useTrialTables(t, nil, nil)
	f := newTrialFixture(t, activityGuild(1, 42, 43), nil)
	plain := trialEvent(501, 42, 43)
	plain.ActivityContext = nil
	none := trialEvent(502, 42, 43)
	none.ActivityContext.Kind = battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_NONE
	foreign := trialEvent(503, 42, 43)
	foreign.ActivityContext.Kind = battlepb.EBattleActivityKind(99)

	for _, ev := range []*kafkapb.BattleResultEvent{plain, none, foreign, nil} {
		if ev != nil {
			f.putResult(t, ev)
		}

		outcome, err := f.l.SettleTrialResult(context.Background(), ev)

		require.NoError(t, err)
		assert.Equal(t, TrialOutcomeSkipped, outcome)
		if ev != nil {
			assert.True(t, f.hasResult(ev.GetBattleId()), "不是历练的结果记录不许删")
		}
	}
	assert.Zero(t, f.store.settleCount())

	unwired := NewGuildLogic(nil, nil, nil, nil, nil)
	outcome, err := unwired.SettleTrialResult(context.Background(), plain)
	require.NoError(t, err)
	assert.Equal(t, TrialOutcomeSkipped, outcome)
}

// TestSettleBadContextAcksWithoutTouchingRepo(06 §6.40 的 TestSettleFiltersEvents):是历练的结果但上下文残缺
// 或对局模式不对 → BadContext,不调仓储;battle_id 非 0 时销账(重发多少次都一样,没有保留的意义)。
func TestSettleBadContextAcksWithoutTouchingRepo(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(ev *kafkapb.BattleResultEvent)
	}{
		{"guild_id 为 0", func(ev *kafkapb.BattleResultEvent) { ev.ActivityContext.GuildId = 0 }},
		{"activity_id 为 0", func(ev *kafkapb.BattleResultEvent) { ev.ActivityContext.ActivityId = 0 }},
		{"period_key 为 0", func(ev *kafkapb.BattleResultEvent) { ev.ActivityContext.PeriodKey = 0 }},
		{"guild_period_key 为 0", func(ev *kafkapb.BattleResultEvent) { ev.ActivityContext.GuildPeriodKey = 0 }},
		{"不是组队 PVE", func(ev *kafkapb.BattleResultEvent) { ev.MatchMode = uint32(matchpb.MatchMode_MATCH_MODE_PVE_SOLO) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useTrialTables(t, nil, nil)
			f := newTrialFixture(t, activityGuild(1, 42, 43), nil)
			ev := trialEvent(501, 42, 43)
			tc.mutate(ev)
			f.putResult(t, ev)

			outcome, err := f.l.SettleTrialResult(context.Background(), ev)

			require.NoError(t, err)
			assert.Equal(t, TrialOutcomeBadContext, outcome)
			assert.Zero(t, f.store.settleCount(), "上下文不可用不调仓储")
			assert.Empty(t, f.store.marked)
			assert.False(t, f.hasResult(501), "battle_id 非 0 时销账")
		})
	}

	t.Run("battle_id 为 0 时无账可销", func(t *testing.T) {
		useTrialTables(t, nil, nil)
		f := newTrialFixture(t, activityGuild(1, 42, 43), nil)
		ev := trialEvent(0, 42, 43)

		outcome, err := f.l.SettleTrialResult(context.Background(), ev)

		require.NoError(t, err)
		assert.Equal(t, TrialOutcomeBadContext, outcome)
		assert.Zero(t, f.store.settleCount())
	})
}

// TestSettleWinPassesCandidatesAndAcks(06 §6.29):胜局把"己方 − 逃跑者"(含阵亡者,U2)交给仓储,带上开战时的
// 周期键、对局结束时刻、本次调用的"现在"与合服闸门;提交后销账。本局计了帮会资金 → 推给全帮成员。
func TestSettleWinPassesCandidatesAndAcks(t *testing.T) {
	useTrialTables(t, nil, nil)
	f := newTrialFixture(t, activityGuild(1, 42, 43, 44, 45), nil)
	ev := trialEvent(501, 44, 42, 43)
	ev.FledPlayerIds = []uint64{43}
	ev.DeadPlayerIds = []uint64{44}
	f.putResult(t, ev)
	f.store.settle = func(data.TrialSettleInput) (data.TrialSettleResult, error) {
		return data.TrialSettleResult{
			Status: data.TrialSettleSettled, Result: pb.GuildTrialSettleResult_GUILD_TRIAL_SETTLE_RESULT_WIN,
			Rewarded: []uint64{42, 44}, FundsGranted: true, CountedWins: 1,
		}, nil
	}

	outcome, err := f.l.SettleTrialResult(context.Background(), ev)

	require.NoError(t, err)
	assert.Equal(t, TrialOutcomeSettled, outcome)
	require.Len(t, f.store.settleInputs, 1)
	in := f.store.settleInputs[0]
	assert.Equal(t, trialEventKey(ev), in.Battle)
	require.NotNil(t, in.Activity)
	assert.Equal(t, trialTestActivityID, in.Activity.GetId())
	assert.True(t, in.Win)
	assert.Equal(t, []uint64{42, 44}, in.Candidates, "逃跑的 43 出局,阵亡的 44 照样在候选(U2)")
	assert.Nil(t, in.Reward, "没配物品奖励就不预备指令")
	assert.Equal(t, ev.GetFinishedAtMs(), in.FinishedAtMs)
	assert.Equal(t, f.nowMs(), in.NowMs)
	require.NotNil(t, in.Fence, "结算要过合服闸门")
	f.fence.merging = true
	assert.ErrorIs(t, in.Fence(context.Background(), 2), data.ErrZoneMerging, "闸门就是帮会写事务共用的那一个")

	assert.False(t, f.hasResult(501), "提交后销账")
	pushes := f.activityPushes()
	require.Len(t, pushes, 1)
	assert.Subset(t, pushes[0].recipients, []uint64{42, 43, 44, 45}, "计了资金:全帮成员都要知道")
	assert.Equal(t, trialTestGuildID, pushes[0].change.GetGuildId())
}

// TestSettleLossNotifiesParticipantsOnly:负 / 平局不发奖(Win=false 交给仓储记 LOSS),照样销账;
// 只通知己方参战者(他们的"进行中"要消掉),不打扰全帮。
func TestSettleLossNotifiesParticipantsOnly(t *testing.T) {
	for _, loss := range []battlepb.EBattleOutcome{battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_B_WIN, battlepb.EBattleOutcome_BATTLE_OUTCOME_DRAW} {
		t.Run(loss.String(), func(t *testing.T) {
			useTrialTables(t, trialRowWithReward(), nil)
			minter := &seqMinter{next: 1}
			f := newTrialFixture(t, activityGuild(1, 42, 43, 44), func(d *ActivityDeps) { d.OpIDs = minter })
			ev := trialEvent(501, 42, 43)
			ev.Outcome = loss
			f.putResult(t, ev)

			outcome, err := f.l.SettleTrialResult(context.Background(), ev)

			require.NoError(t, err)
			assert.Equal(t, TrialOutcomeSettled, outcome)
			require.Len(t, f.store.settleInputs, 1)
			assert.False(t, f.store.settleInputs[0].Win)
			assert.Nil(t, f.store.settleInputs[0].Reward)
			assert.Zero(t, minter.callCount(), "不发奖的局不烧号")
			assert.False(t, f.hasResult(501))
			pushes := f.activityPushes()
			require.Len(t, pushes, 1)
			assert.Equal(t, []uint64{42, 43}, pushes[0].recipients)
		})
	}
}

// TestSettleWithItemsPreparesOneOpPerCandidate(06 §6.29 第 5 步):带物品的胜局,事务前给**每位候选人**各发一个号、
// 生成一个租约令牌(谁最终得奖事务里才知道);提交后对插下的指令各同步投递一次。
func TestSettleWithItemsPreparesOneOpPerCandidate(t *testing.T) {
	useTrialTables(t, trialRowWithReward(), nil)
	loop, applier, _ := newCountingLoop(t)
	minter := &seqMinter{next: 9001}
	f := newTrialFixture(t, activityGuild(1, 42, 43, 44), func(d *ActivityDeps) { d.Loop, d.OpIDs = loop, minter })
	ev := trialEvent(501, 42, 43, 44)
	f.store.settle = func(in data.TrialSettleInput) (data.TrialSettleResult, error) {
		res := data.TrialSettleResult{Status: data.TrialSettleSettled, Result: pb.GuildTrialSettleResult_GUILD_TRIAL_SETTLE_RESULT_WIN,
			Rewarded: []uint64{42, 43, 44}, Owed: []uint64{44}}
		// 42、43 入队,44 未决已满转存待入队表。
		for i, op := range in.Reward.Ops[:2] {
			res.Enqueued = append(res.Enqueued, data.TrialEnqueuedReward{
				PlayerID: op.PlayerID, OpID: op.OpID, Seq: uint64(i + 1), StreamEpoch: 1, LeaseToken: op.LeaseToken,
			})
		}
		return res, nil
	}

	outcome, err := f.l.SettleTrialResult(context.Background(), ev)

	require.NoError(t, err)
	assert.Equal(t, TrialOutcomeSettled, outcome)
	reward := f.store.settleInputs[0].Reward
	require.NotNil(t, reward)
	wantPayload, err := proto.Marshal(&assetpb.AssetBundle{Items: []*assetpb.ItemGrant{{ConfigId: 1001, Count: 4}}})
	require.NoError(t, err)
	assert.Equal(t, wantPayload, reward.Payload, "奖励包按 Reward 表合并后序列化")
	assert.Equal(t, f.nowMs()+10_000, reward.LeaseUntilMs, "插行租约 = 默认 10s")
	require.Len(t, reward.Ops, 3, "每位候选人恰好一项")
	seenOps := map[uint64]bool{}
	for i, op := range reward.Ops {
		assert.Equal(t, []uint64{42, 43, 44}[i], op.PlayerID)
		assert.NotZero(t, op.OpID)
		assert.NotZero(t, op.LeaseToken)
		assert.False(t, seenOps[op.OpID], "指令号不重复")
		seenOps[op.OpID] = true
	}
	assert.Equal(t, 3, minter.callCount())
	assert.Equal(t, 2, applier.count(), "插下的两条指令各同步投递一次;转存待入队的那一位不投")
}

// TestSettleWithItemsWhileChannelClosed:开战之后资产通道才被关掉 —— 结算照常(指令行照插,保持 PENDING,
// 通道重新打开后由重投循环投递),只是不做同步投递。永不中止的物品不能因为一次运维操作被跳过。
func TestSettleWithItemsWhileChannelClosed(t *testing.T) {
	useTrialTables(t, trialRowWithReward(), nil)
	f := newTrialFixture(t, activityGuild(1, 42, 43), func(d *ActivityDeps) { d.OpIDs = &seqMinter{next: 1} })
	ev := trialEvent(501, 42, 43)
	f.putResult(t, ev)
	f.store.settle = func(in data.TrialSettleInput) (data.TrialSettleResult, error) {
		op := in.Reward.Ops[0]
		return data.TrialSettleResult{Status: data.TrialSettleSettled, Result: pb.GuildTrialSettleResult_GUILD_TRIAL_SETTLE_RESULT_WIN,
			Rewarded: []uint64{42}, Enqueued: []data.TrialEnqueuedReward{{PlayerID: 42, OpID: op.OpID, Seq: 1, StreamEpoch: 1, LeaseToken: op.LeaseToken}}}, nil
	}

	outcome, err := f.l.SettleTrialResult(context.Background(), ev)

	require.NoError(t, err)
	assert.Equal(t, TrialOutcomeSettled, outcome)
	require.NotNil(t, f.store.settleInputs[0].Reward)
	assert.False(t, f.hasResult(501))
}

// TestSettleTemporaryFailuresKeepTheRecord:暂时性失败(发号失败、写冲突、合服闸门、库故障)原样返回错误,
// 让调用方退避重试 —— 不标毒、不销账(销了账这一局的奖励就永久丢了)。
func TestSettleTemporaryFailuresKeepTheRecord(t *testing.T) {
	t.Run("发号失败:不进事务", func(t *testing.T) {
		useTrialTables(t, trialRowWithReward(), nil)
		f := newTrialFixture(t, activityGuild(1, 42, 43), func(d *ActivityDeps) {
			d.OpIDs = &seqMinter{err: errors.New("segment fenced")}
		})
		ev := trialEvent(501, 42, 43)
		f.putResult(t, ev)

		outcome, err := f.l.SettleTrialResult(context.Background(), ev)

		require.Error(t, err)
		assert.Zero(t, outcome)
		assert.Zero(t, f.store.settleCount())
		assert.Empty(t, f.store.marked)
		assert.True(t, f.hasResult(501))
	})
	t.Run("发号器未接线:不进事务", func(t *testing.T) {
		useTrialTables(t, trialRowWithReward(), nil)
		f := newTrialFixture(t, activityGuild(1, 42, 43), nil)
		ev := trialEvent(501, 42, 43)
		f.putResult(t, ev)

		_, err := f.l.SettleTrialResult(context.Background(), ev)

		require.Error(t, err)
		assert.Zero(t, f.store.settleCount())
		assert.True(t, f.hasResult(501))
	})
	for _, repoErr := range []error{
		data.ErrWriteConflict,
		fmt.Errorf("%w: fence unreadable", data.ErrZoneMerging),
		errors.New("driver: bad connection"),
	} {
		t.Run("事务回 "+repoErr.Error(), func(t *testing.T) {
			useTrialTables(t, nil, nil)
			f := newTrialFixture(t, activityGuild(1, 42, 43), nil)
			ev := trialEvent(501, 42, 43)
			f.putResult(t, ev)
			f.store.settle = func(data.TrialSettleInput) (data.TrialSettleResult, error) { return data.TrialSettleResult{}, repoErr }

			outcome, err := f.l.SettleTrialResult(context.Background(), ev)

			require.ErrorIs(t, err, repoErr)
			assert.Zero(t, outcome)
			assert.Empty(t, f.store.marked, "暂时性失败不标毒")
			assert.True(t, f.hasResult(501), "暂时性失败不销账")
			assert.Empty(t, f.activityPushes())
		})
	}
}

// TestSettleUsesFreshNowOnEachCall:每次重试都重取"现在"(仓储按它判"结果是否过旧"),不沿用上一次的时刻。
func TestSettleUsesFreshNowOnEachCall(t *testing.T) {
	useTrialTables(t, nil, nil)
	f := newTrialFixture(t, activityGuild(1, 42, 43), nil)
	ev := trialEvent(501, 42, 43)
	f.store.settle = func(data.TrialSettleInput) (data.TrialSettleResult, error) {
		return data.TrialSettleResult{}, data.ErrWriteConflict
	}

	_, err := f.l.SettleTrialResult(context.Background(), ev)
	require.Error(t, err)
	f.clock.advance(30 * time.Second)
	_, err = f.l.SettleTrialResult(context.Background(), ev)
	require.Error(t, err)

	require.Len(t, f.store.settleInputs, 2)
	assert.Equal(t, f.store.settleInputs[0].NowMs+30_000, f.store.settleInputs[1].NowMs)
}

// TestSettlePoisonMarksAndAcks(06 §6.40、§6.31):确定性失败(溢出 / 结果过旧 / 输入畸形 / 奖励包坏)不重试 ——
// 把对局标成 POISON、销账、回 (Poison, nil),让消费者提交 offset,分区不被卡住。
func TestSettlePoisonMarksAndAcks(t *testing.T) {
	for _, repoErr := range []error{
		fmt.Errorf("%w: funds of guild 9", data.ErrActivityPoison),
		fmt.Errorf("%w: 17 candidates", data.ErrTrialInputInvalid),
	} {
		t.Run("事务回 "+repoErr.Error(), func(t *testing.T) {
			useTrialTables(t, nil, nil)
			f := newTrialFixture(t, activityGuild(1, 42, 43), nil)
			ev := trialEvent(501, 42, 43)
			f.putResult(t, ev)
			f.store.settle = func(data.TrialSettleInput) (data.TrialSettleResult, error) { return data.TrialSettleResult{}, repoErr }

			outcome, err := f.l.SettleTrialResult(context.Background(), ev)

			require.NoError(t, err)
			assert.Equal(t, TrialOutcomePoison, outcome)
			assert.Equal(t, []data.TrialBattleKey{trialEventKey(ev)}, f.store.marked, "标记带完整上下文,没登记过的局也能留下一行")
			assert.False(t, f.hasResult(501), "标毒之后销账")
		})
	}

	t.Run("奖励包构建失败:不进事务直接标毒", func(t *testing.T) {
		row := trialRow()
		row.RewardId = 404 // Reward 表里没有这一行
		useTrialTables(t, row, nil)
		f := newTrialFixture(t, activityGuild(1, 42, 43), func(d *ActivityDeps) { d.OpIDs = &seqMinter{next: 1} })
		ev := trialEvent(501, 42, 43)
		f.putResult(t, ev)

		outcome, err := f.l.SettleTrialResult(context.Background(), ev)

		require.NoError(t, err)
		assert.Equal(t, TrialOutcomePoison, outcome)
		assert.Zero(t, f.store.settleCount())
		assert.Len(t, f.store.marked, 1)
		assert.False(t, f.hasResult(501))
	})
}

// TestSettlePoisonMarkOutcomes:标毒时发现这一局其实已有定论,就按那个定论答复(仍然销账);
// 标记本身遇到库故障则是暂时性失败,不销账、下一轮再来。
func TestSettlePoisonMarkOutcomes(t *testing.T) {
	poison := func(data.TrialSettleInput) (data.TrialSettleResult, error) {
		return data.TrialSettleResult{}, data.ErrActivityPoison
	}
	cases := []struct {
		name string
		mark data.TrialMarkOutcome
		want TrialSettleOutcome
	}{
		{"早已结算(这只是一条坏的重复消息)", data.TrialMarkAlreadySettled, TrialOutcomeDuplicate},
		{"帮会已解散", data.TrialMarkGuildGone, TrialOutcomeGuildGone},
		{"登记在别的帮名下", data.TrialMarkMismatch, TrialOutcomeContextMismatch},
		{"没有登记行可标", data.TrialMarkMissing, TrialOutcomePoison},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useTrialTables(t, nil, nil)
			f := newTrialFixture(t, activityGuild(1, 42, 43), nil)
			ev := trialEvent(501, 42, 43)
			f.putResult(t, ev)
			f.store.settle, f.store.markOutcome = poison, tc.mark

			outcome, err := f.l.SettleTrialResult(context.Background(), ev)

			require.NoError(t, err)
			assert.Equal(t, tc.want, outcome)
			assert.False(t, f.hasResult(501))
		})
	}

	t.Run("标记遇到写冲突:暂时性失败", func(t *testing.T) {
		useTrialTables(t, nil, nil)
		f := newTrialFixture(t, activityGuild(1, 42, 43), nil)
		ev := trialEvent(501, 42, 43)
		f.putResult(t, ev)
		f.store.settle, f.store.markErr = poison, data.ErrWriteConflict

		outcome, err := f.l.SettleTrialResult(context.Background(), ev)

		require.ErrorIs(t, err, data.ErrWriteConflict)
		assert.Zero(t, outcome)
		assert.True(t, f.hasResult(501), "没标上就不销账")
	})
}

// TestSettleTerminalStatusesAck:重复事件 / 帮会已解散 / 登记在别帮名下 —— 都是"已有定论",销账、不推送、不重试。
func TestSettleTerminalStatusesAck(t *testing.T) {
	cases := []struct {
		status data.TrialSettleStatus
		want   TrialSettleOutcome
	}{
		{data.TrialSettleDuplicate, TrialOutcomeDuplicate},
		{data.TrialSettleGuildGone, TrialOutcomeGuildGone},
		{data.TrialSettleContextMismatch, TrialOutcomeContextMismatch},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("status %d", tc.status), func(t *testing.T) {
			useTrialTables(t, nil, nil)
			f := newTrialFixture(t, activityGuild(1, 42, 43), nil)
			ev := trialEvent(501, 42, 43)
			f.putResult(t, ev)
			f.store.settle = func(data.TrialSettleInput) (data.TrialSettleResult, error) {
				return data.TrialSettleResult{Status: tc.status}, nil
			}

			outcome, err := f.l.SettleTrialResult(context.Background(), ev)

			require.NoError(t, err)
			assert.Equal(t, tc.want, outcome)
			assert.False(t, f.hasResult(501))
			assert.Empty(t, f.activityPushes())
			assert.Empty(t, f.store.marked)
		})
	}
}

// TestSettleShortCircuitsAlreadySettledBattles(06 §6.29 落码修正第 10 点):对局行已是 SETTLED —— 正常结算过、被标成
// 毒消息、或运维按 §6.35a 做法 A 手工写成终态 —— 入口处直接回 Duplicate 并销账,不构建奖励包、不发号、不进事务。
// 这是 battle 重发副本(逐字节相同,至多 30 份)的常规去向,也是"卡住的消息"的人工出口,所以必须排在发号与合服闸门
// 之前:用例里发号器是坏的、事务一进就回"正在合服",预读排错了位置,这条结果就一直是暂时性失败。
func TestSettleShortCircuitsAlreadySettledBattles(t *testing.T) {
	useTrialTables(t, trialRowWithReward(), nil)
	minter := &seqMinter{err: errors.New("segment fenced")}
	f := newTrialFixture(t, activityGuild(1, 42, 43), func(d *ActivityDeps) { d.OpIDs = minter })
	f.store.settle = func(data.TrialSettleInput) (data.TrialSettleResult, error) {
		return data.TrialSettleResult{}, data.ErrZoneMerging
	}
	ev := trialEvent(501, 42, 43)
	f.putResult(t, ev)
	f.store.battles = map[uint64]data.TrialBattleRow{501: {
		TrialBattleKey: trialEventKey(ev),
		State:          pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_SETTLED,
		Result:         pb.GuildTrialSettleResult_GUILD_TRIAL_SETTLE_RESULT_POISON,
	}}

	// 同一局的副本来多少份都走同一条路(第二份起结果记录已经不在,销账是幂等的)。
	for i := 0; i < 3; i++ {
		outcome, err := f.l.SettleTrialResult(context.Background(), ev)

		require.NoError(t, err)
		assert.Equal(t, TrialOutcomeDuplicate, outcome)
	}
	assert.Zero(t, minter.callCount(), "已有终态的局不烧号")
	assert.Zero(t, f.store.settleCount(), "已有终态的局不进事务")
	assert.Empty(t, f.store.marked)
	assert.False(t, f.hasResult(501), "重复的结果照样销账,battle 的重发随之停止")
	assert.Empty(t, f.activityPushes())
}

// TestSettlePreReadOnlyTrustsSettledRows:预读只认"SETTLED 且登记在结果所说的那个帮名下"。已登记未结算、已被巡检器
// 判过期(迟到的结果仍要结算)、终态行登记在别的帮名下(归属由事务里持锁的闸门判),都照常进事务;
// 预读本身读失败是 guild 自己的库故障 → 暂时性错误,不烧号、不销账。
func TestSettlePreReadOnlyTrustsSettledRows(t *testing.T) {
	ev := trialEvent(501, 42, 43)
	rowOf := func(guildID uint64, state pb.GuildTrialBattleState) data.TrialBattleRow {
		key := trialEventKey(ev)
		key.GuildID = guildID
		return data.TrialBattleRow{TrialBattleKey: key, State: state}
	}
	cases := []struct {
		name string
		row  data.TrialBattleRow
	}{
		{"已登记未结算", rowOf(trialTestGuildID, pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_STARTED)},
		{"已被巡检器判过期", rowOf(trialTestGuildID, pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_EXPIRED)},
		{"终态行登记在别的帮名下", rowOf(trialTestGuildID+1, pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_SETTLED)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useTrialTables(t, nil, nil)
			f := newTrialFixture(t, activityGuild(1, 42, 43), nil)
			f.putResult(t, ev)
			f.store.battles = map[uint64]data.TrialBattleRow{501: tc.row}

			outcome, err := f.l.SettleTrialResult(context.Background(), ev)

			require.NoError(t, err)
			assert.Equal(t, TrialOutcomeSettled, outcome)
			assert.Equal(t, 1, f.store.settleCount(), "预读不下结论,交给事务里持锁的闸门")
		})
	}

	t.Run("预读失败是暂时性错误", func(t *testing.T) {
		useTrialTables(t, trialRowWithReward(), nil)
		minter := &seqMinter{next: 1}
		f := newTrialFixture(t, activityGuild(1, 42, 43), func(d *ActivityDeps) { d.OpIDs = minter })
		f.putResult(t, ev)
		readErr := errors.New("driver: bad connection")
		f.store.battlesErr = readErr

		outcome, err := f.l.SettleTrialResult(context.Background(), ev)

		require.ErrorIs(t, err, readErr)
		assert.Zero(t, outcome)
		assert.Zero(t, minter.callCount(), "读不到对局行就不往下走,不白烧号")
		assert.Zero(t, f.store.settleCount())
		assert.Empty(t, f.store.marked)
		assert.True(t, f.hasResult(501), "暂时性失败不销账")
	})
}

// TestSettleConfigMissing(06 §6.29 第 2 步):活动行已不在配表里(或已不是历练)→ 把 nil 交给仓储,由它记
// CONFIG_MISSING、不发奖;不预备任何指令。
func TestSettleConfigMissing(t *testing.T) {
	// 配表里只有灯会:活动 3 不存在。
	swapActivityTables(t, []*tablepb.GuildActivityTable{lanternRow()}, activityTestRule(), rewardItemRow())
	minter := &seqMinter{next: 1}
	f := newTrialFixture(t, activityGuild(1, 42, 43), func(d *ActivityDeps) { d.OpIDs = minter })
	ev := trialEvent(501, 42, 43)
	f.putResult(t, ev)
	f.store.settle = func(data.TrialSettleInput) (data.TrialSettleResult, error) {
		return data.TrialSettleResult{Status: data.TrialSettleSettled, Result: pb.GuildTrialSettleResult_GUILD_TRIAL_SETTLE_RESULT_CONFIG_MISSING}, nil
	}

	outcome, err := f.l.SettleTrialResult(context.Background(), ev)

	require.NoError(t, err)
	assert.Equal(t, TrialOutcomeSettled, outcome)
	require.Len(t, f.store.settleInputs, 1)
	assert.Nil(t, f.store.settleInputs[0].Activity)
	assert.Nil(t, f.store.settleInputs[0].Reward)
	assert.Zero(t, minter.callCount())
	assert.False(t, f.hasResult(501))

	// 活动 1 存在但不是历练:同样按缺行处理,不能拿灯会的奖励去结算历练。
	wrongType := trialEvent(502, 42, 43)
	wrongType.ActivityContext.ActivityId = 1
	_, err = f.l.SettleTrialResult(context.Background(), wrongType)
	require.NoError(t, err)
	assert.Nil(t, f.store.settleInputs[1].Activity)
}

// TestSettleNotWiredIsAnError:历练的结果到了,而仓储 / 结果记录没接线 —— 按暂时性错误返回(消费者停在这条消息上重试),
// 绝不当成"处理完了"把结果丢掉。
func TestSettleNotWiredIsAnError(t *testing.T) {
	ev := trialEvent(501, 42, 43)

	_, err := NewGuildLogic(nil, nil, nil, nil, nil).SettleTrialResult(context.Background(), ev)
	require.ErrorIs(t, err, errTrialNotWired)

	useTrialTables(t, nil, nil)
	f := newTrialFixture(t, activityGuild(1, 42, 43), func(d *ActivityDeps) { d.Results = nil })
	_, err = f.l.SettleTrialResult(context.Background(), ev)
	require.ErrorIs(t, err, errTrialNotWired)
	assert.Zero(t, f.store.settleCount())
}

// TestSettleDoesNotDependOnTrialBeingOpen:关掉历练(match / 房间没接线)之后,在途对局的结果照常结算。
func TestSettleDoesNotDependOnTrialBeingOpen(t *testing.T) {
	useTrialTables(t, nil, nil)
	f := newTrialFixture(t, activityGuild(1, 42, 43), func(d *ActivityDeps) { d.Match, d.Lobby = nil, nil })
	ev := trialEvent(501, 42, 43)
	f.putResult(t, ev)

	outcome, err := f.l.SettleTrialResult(context.Background(), ev)

	require.NoError(t, err)
	assert.Equal(t, TrialOutcomeSettled, outcome)
	assert.False(t, f.hasResult(501))
}
