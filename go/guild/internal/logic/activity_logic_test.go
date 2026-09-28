package logic

// 帮会活动 logic 层的单测(不连真库;06-activities.md §6.40 的 activity_logic_test.go 部分,按 90 Y-01 / 裁决 K 订正)。
//
// 套路与 economy_logic_test.go 相同:
//   - 走不到仓储的分支把 repo 传 nil,顺序一旦改错、提前碰库就当场 nil panic。
//   - 需要"前置与预检都走完"的用例用 newNoMembershipRepo:缓存走 miniredis,MySQL 是只会回"查无此行"的替身
//     (视图的三个读都回空,开事务一律失败)。于是"业务拒绝发生在事务之前"可以直接断言:真走到事务,
//     结果会是一个 gRPC 错误而不是 tip。
//   - 配表经 swapActivityTables 换成本文件造的行(写进临时目录再 Load),用例结束换回原实例。
//     与 loadEconomyFlowTables 同一前提:本包用例都不调 t.Parallel。
//
// 共用替身:clientCtx / fakeHomeZones / newCacheOnlyRepo / seedGuild 在 client_zone_test.go;
// managementCall 在 guild_manage_logic_test.go;countingMinter / countingAssetStore / newCountingLoop /
// seedMembership / newNoMembershipRepo 在 economy_logic_test.go。
//
// 需要 MySQL 的事务语义(I1–I12)在 data/activity_repo_integration_test.go。

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"guild/internal/activity"
	"guild/internal/constants"
	"guild/internal/data"
	assetpb "proto/common/asset"
	rollbackpb "proto/common/rollback"
	pb "proto/guild"
	plpb "proto/player_locator"
	"shared/assetop"
	tablepb "shared/generated/pb/table"
	"shared/generated/table"
)

// activityTestNow 是本文件的固定时钟(UTC 12:00 = UTC+8 20:00,离 05:00 切点足够远)。
var activityTestNow = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

func activityTestNowMs() uint64 { return uint64(activityTestNow.UnixMilli()) }

// activityTestClock 是注入给 ActivityDeps.Now 与 assetop.Loop 的固定时钟:用例的结论不随真实墙钟变化(AGENTS.md §11.4)。
func activityTestClock() time.Time { return activityTestNow }

// longAgoJoinMs:入帮已满任何合法 activity_join_min_hours(上限 720 小时)。
func longAgoJoinMs() uint64 { return activityTestNowMs() - 800*3_600_000 }

// ── 配表夹具 ──────────────────────────────────────────────────

// 三行默认活动(06 §6.2.2 的开发值,常开 0/0)。奖励默认 0,需要物品的用例自己改。
func lanternRow() *tablepb.GuildActivityTable {
	return &tablepb.GuildActivityTable{
		Id:                   1,
		Name:                 "元宵灯会",
		Type:                 activity.TypeLantern,
		Enabled:              true,
		MinGuildLevel:        1,
		PersonalContribution: 20,
		GuildFunds:           500,
		GuildThreshold:       3,
		DailyLimit:           1,
	}
}

func reunionRow() *tablepb.GuildActivityTable {
	return &tablepb.GuildActivityTable{
		Id:                   2,
		Name:                 "中秋团圆",
		Type:                 activity.TypeReunion,
		Enabled:              true,
		MinGuildLevel:        1,
		PersonalContribution: 30,
		GuildThreshold:       3,
		DailyLimit:           1,
	}
}

func trialRow() *tablepb.GuildActivityTable {
	return &tablepb.GuildActivityTable{
		Id:                   3,
		Name:                 "同道历练",
		Type:                 activity.TypeTrial,
		Enabled:              true,
		MinGuildLevel:        1,
		PersonalContribution: 50,
		GuildFunds:           300,
		GuildThreshold:       3,
		DungeonId:            1,
		TeamSizeMin:          2,
		TeamSizeMax:          5,
		DailyLimit:           2,
	}
}

// lanternWithReward / reunionWithReward:同一行改成带 Reward[1] 的物品奖励(合并后 item 1001 × 4)。
func lanternWithReward() *tablepb.GuildActivityTable {
	row := lanternRow()
	row.RewardId = 1
	return row
}

func reunionWithReward() *tablepb.GuildActivityTable {
	row := reunionRow()
	row.RewardId = 1
	return row
}

// activityTestRule 是 GuildRule 规则行的活动相关列(90 D1 的最终行:团圆兜底 3、入帮时长 0)。
func activityTestRule() *tablepb.GuildRuleTable {
	return &tablepb.GuildRuleTable{
		Id:                         activity.RuleRowID,
		ReunionMinOnlineMembers:    3,
		ActivityJoinMinHours:       0,
		TrialInviteTtlSeconds:      30,
		TrialInviteCooldownSeconds: 10,
	}
}

// rewardItemRow:Reward[1] 两槽同一物品,合并后是 item 1001 × 4。
func rewardItemRow() *tablepb.RewardTable {
	return &tablepb.RewardTable{Id: 1, Reward: []*tablepb.Rewardreward{
		{RewardItem: 1001, RewardCount: 2}, {RewardItem: 1001, RewardCount: 2},
	}}
}

// swapActivityTables 把 GuildActivity / GuildRule / Reward 三张表换成给定行:写成导表产物同形的 JSON 再用生成的 Load 读,
// 与生产走同一条解析路径。t.Cleanup 换回原实例,还原"未加载配表"的状态(前提:本包用例不调 t.Parallel)。
func swapActivityTables(t *testing.T, rows []*tablepb.GuildActivityTable, rule *tablepb.GuildRuleTable, rewards ...*tablepb.RewardTable) {
	t.Helper()
	dir := t.TempDir()
	writeTableJSON(t, dir, "guildactivity.json", &tablepb.GuildActivityTableData{Data: rows})
	var rules []*tablepb.GuildRuleTable
	if rule != nil {
		rules = append(rules, rule)
	}
	writeTableJSON(t, dir, "guildrule.json", &tablepb.GuildRuleTableData{Data: rules})
	writeTableJSON(t, dir, "reward.json", &tablepb.RewardTableData{Data: rewards})

	origActivity := table.GuildActivityTableManagerInstance
	origRule := table.GuildRuleTableManagerInstance
	origReward := table.RewardTableManagerInstance
	t.Cleanup(func() {
		table.GuildActivityTableManagerInstance = origActivity
		table.GuildRuleTableManagerInstance = origRule
		table.RewardTableManagerInstance = origReward
	})
	table.GuildActivityTableManagerInstance = table.NewGuildActivityTableManager()
	table.GuildRuleTableManagerInstance = table.NewGuildRuleTableManager()
	table.RewardTableManagerInstance = table.NewRewardTableManager()
	require.NoError(t, table.GuildActivityTableManagerInstance.Load(dir, false))
	require.NoError(t, table.GuildRuleTableManagerInstance.Load(dir, false))
	require.NoError(t, table.RewardTableManagerInstance.Load(dir, false))
}

func writeTableJSON(t *testing.T, dir, name string, msg proto.Message) {
	t.Helper()
	raw, err := protojson.Marshal(msg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), raw, 0o600))
}

// ── logic 夹具 ────────────────────────────────────────────────

// activityFixture:缓存里有帮会 9(zone 2)与全体成员的映射;MySQL 是 noMembershipMySQL(读全空、开事务失败);
// 会话 Redis 单独一个 miniredis,用例用 setOnline 铺在线状态、或关掉它模拟 Redis 故障。
type activityFixture struct {
	l        *GuildLogic
	sessions *miniredis.Miniredis
}

func newActivityFixture(t *testing.T, g data.GuildData, deps ActivityDeps, homeZone uint32) *activityFixture {
	t.Helper()
	repo, mr := newNoMembershipRepo(t)
	seedGuild(t, mr, g, 0)
	for _, m := range g.Members {
		seedMembership(t, mr, m.PlayerID, g.GuildID)
	}
	sessions := miniredis.RunT(t)
	srdb := goredis.NewClient(&goredis.Options{Addr: sessions.Addr()})
	t.Cleanup(func() { srdb.Close() })

	activityRepo, err := data.NewActivityRepo(repo)
	require.NoError(t, err)
	deps.Repo = activityRepo
	if deps.Now == nil {
		deps.Now = activityTestClock
	}
	l := NewGuildLogic(repo, nil, NewOnlineStatusResolver(srdb), nil, &fakeHomeZones{zone: homeZone}, WithActivities(deps))
	return &activityFixture{l: l, sessions: sessions}
}

// setOnline 按 player_locator 的会话键契约写 ONLINE 会话。
func (f *activityFixture) setOnline(t *testing.T, ids ...uint64) {
	t.Helper()
	for _, id := range ids {
		writeSession(t, f.sessions, id, plpb.PlayerSessionState_SESSION_STATE_ONLINE)
	}
}

func writeSession(t *testing.T, mr *miniredis.Miniredis, playerID uint64, state plpb.PlayerSessionState) {
	t.Helper()
	raw, err := proto.Marshal(&plpb.PlayerSession{
		PlayerId:       playerID,
		SessionId:      uint32(playerID),
		GateId:         "1",
		GateInstanceId: "inst-1",
		State:          state,
	})
	require.NoError(t, err)
	require.NoError(t, mr.Set(playerSessionKey(playerID), string(raw)))
}

// activityGuild:帮会 9、zone 2,成员全部入帮已久。
func activityGuild(level uint32, memberIDs ...uint64) data.GuildData {
	g := data.GuildData{GuildID: 9, ZoneID: 2, Level: level}
	for i, id := range memberIDs {
		role := constants.RoleMember
		if i == 0 {
			role = constants.RoleLeader
		}
		g.Members = append(g.Members, data.MemberData{PlayerID: id, Role: role, JoinTimeMs: longAgoJoinMs()})
	}
	return g
}

// activityRPCs 把五个活动 RPC 抹平成同一形状,好让公共前置用一张表覆盖全部入口。
func activityRPCs() []managementCall {
	return []managementCall{
		{"GetGuildActivities", func(l *GuildLogic, ctx context.Context) (uint32, error) {
			resp, err := l.GetGuildActivities(ctx, &pb.GetGuildActivitiesRequest{})
			return resp.GetErrorMessage().GetId(), err
		}},
		{"LightGuildLantern", func(l *GuildLogic, ctx context.Context) (uint32, error) {
			resp, err := l.LightGuildLantern(ctx, &pb.LightGuildLanternRequest{ActivityId: 1})
			return resp.GetErrorMessage().GetId(), err
		}},
		{"ClaimGuildReunion", func(l *GuildLogic, ctx context.Context) (uint32, error) {
			resp, err := l.ClaimGuildReunion(ctx, &pb.ClaimGuildReunionRequest{ActivityId: 2})
			return resp.GetErrorMessage().GetId(), err
		}},
		{"StartGuildTrial", func(l *GuildLogic, ctx context.Context) (uint32, error) {
			resp, err := l.StartGuildTrial(ctx, &pb.StartGuildTrialRequest{ActivityId: 3, MemberPlayerIds: []uint64{42, 43}})
			return resp.GetErrorMessage().GetId(), err
		}},
		{"RespondGuildTrialInvite", func(l *GuildLogic, ctx context.Context) (uint32, error) {
			resp, err := l.RespondGuildTrialInvite(ctx, &pb.RespondGuildTrialInviteRequest{LobbyId: 1, Accept: true})
			return resp.GetErrorMessage().GetId(), err
		}},
	}
}

// ── 接线与公共前置 ────────────────────────────────────────────

// TestWithActivities:Repo 为 nil 不装配(半装配比不装配更危险);零值字段补默认;显式值原样保留。
// 依赖落在 GuildLogic.activities 字段上(90 Y-02 的函数式 Option),不经任何包级状态。
func TestWithActivities(t *testing.T) {
	t.Run("Repo 为 nil 不装配", func(t *testing.T) {
		l := NewGuildLogic(nil, nil, nil, nil, nil, WithActivities(ActivityDeps{SyncBudget: time.Second}))

		assert.Nil(t, l.activities)
	})
	t.Run("零值字段补默认", func(t *testing.T) {
		l := NewGuildLogic(nil, nil, nil, nil, nil, WithActivities(ActivityDeps{Repo: &data.ActivityRepo{}}))

		d := l.activities
		require.NotNil(t, d)
		assert.NotNil(t, d.Now)
		assert.Equal(t, 2500*time.Millisecond, d.SyncBudget)
		assert.Equal(t, 10*time.Second, d.Lease)
		assert.Nil(t, d.Loop, "Loop 为 nil 表示资产通道关闭,不能被补成别的值")
		assert.Nil(t, d.OpIDs)
	})
	t.Run("显式值原样保留", func(t *testing.T) {
		l := NewGuildLogic(nil, nil, nil, nil, nil, WithActivities(ActivityDeps{
			Repo: &data.ActivityRepo{}, Now: activityTestClock, SyncBudget: time.Second, Lease: 30 * time.Second,
		}))

		d := l.activities
		require.NotNil(t, d)
		assert.Equal(t, activityTestNow, d.Now(), "注入的时钟原样保留,不被换成 time.Now")
		assert.Equal(t, time.Second, d.SyncBudget)
		assert.Equal(t, 30*time.Second, d.Lease)
	})
}

// TestActivityRPCsRefuseWithoutDeps:未接线时五个 RPC 回明确的 tip(kGuildActivityNotOpen),不回 gRPC 错误、
// 更不 nil 解引用崩掉整个进程。repo 为 nil:判定若排在任何仓储读之后,这里会当场 panic。
func TestActivityRPCsRefuseWithoutDeps(t *testing.T) {
	l := NewGuildLogic(nil, nil, nil, nil, nil)

	for _, tc := range activityRPCs() {
		t.Run(tc.name, func(t *testing.T) {
			tipID, err := tc.run(l, clientCtx(42))

			require.NoError(t, err, "未接线回 tip,不回 gRPC 错误(否则客户端进重连隔离)")
			assert.Equal(t, constants.ErrActivityNotOpen, tipID)
		})
	}
}

// TestActivityNoSessionPermissionDenied(06 §6.40):五个 RPC 无会话 → PermissionDenied。
// 请求体里没有 player_id,内部调用拿不出可信身份;repo 为 nil,越过这一步就会 panic。
func TestActivityNoSessionPermissionDenied(t *testing.T) {
	l := NewGuildLogic(nil, nil, nil, nil, nil)
	l.activities = &ActivityDeps{Now: activityTestClock}

	for _, tc := range activityRPCs() {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.run(l, context.Background())

			assert.Equal(t, codes.PermissionDenied, status.Code(err))
		})
	}
}

// TestTrialStubsB6a(06 §6.40):前置通过后 Start / Respond 恒回未开放;写路径不查归属 zone(Y-01),
// homeZones 为 nil 也不影响。MySQL 为 nil:桩走到任何读库都会 panic。
func TestTrialStubsB6a(t *testing.T) {
	repo, mr := newCacheOnlyRepo(t)
	seedGuild(t, mr, activityGuild(1, 42, 43), 0)
	seedMembership(t, mr, 42, 9)
	seedMembership(t, mr, 43, 9)
	l := NewGuildLogic(repo, nil, nil, nil, nil)
	l.activities = &ActivityDeps{Now: activityTestClock}

	start, err := l.StartGuildTrial(clientCtx(42), &pb.StartGuildTrialRequest{ActivityId: 3, MemberPlayerIds: []uint64{42, 43}})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrActivityNotOpen, start.GetErrorMessage().GetId())

	respond, err := l.RespondGuildTrialInvite(clientCtx(43), &pb.RespondGuildTrialInviteRequest{LobbyId: 7, Accept: true})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrActivityNotOpen, respond.GetErrorMessage().GetId())
}

// TestTrialStubsAnswerNotInGuildFirst:不在帮的人先看到"未入帮"(以 MySQL 复核过的 0),与 B6b 落地后的答复一致。
func TestTrialStubsAnswerNotInGuildFirst(t *testing.T) {
	repo, _ := newNoMembershipRepo(t)
	l := NewGuildLogic(repo, nil, nil, nil, nil)
	l.activities = &ActivityDeps{Now: activityTestClock}

	resp, err := l.StartGuildTrial(clientCtx(42), &pb.StartGuildTrialRequest{ActivityId: 3})

	require.NoError(t, err)
	assert.Equal(t, constants.ErrNotInGuild, resp.GetErrorMessage().GetId())
}

// ── 视图 ──────────────────────────────────────────────────────

// TestTrialViewDisabledInB6a:B6a 的历练视图强制 DISABLED + 未开放,且不带房间与对局(06 §6.7 末段)。
func TestTrialViewDisabledInB6a(t *testing.T) {
	view, err := buildActivityView(trialRow(), activityViewData{
		now:    activityTestNow,
		rule:   activityTestRule(),
		guild:  &data.GuildData{GuildID: 9, Level: 5},
		member: data.MemberData{PlayerID: 42, JoinTimeMs: longAgoJoinMs()},
	})

	require.NoError(t, err)
	assert.Equal(t, pb.GuildActivityState_GUILD_ACTIVITY_STATE_DISABLED, view.GetState())
	assert.Equal(t, constants.ErrActivityNotOpen, view.GetBlockedTipId())
	assert.Zero(t, view.GetMyTrialBattleId())
	assert.Nil(t, view.GetTrialLobby())
}

// TestBuildActivityViewKeys:视图按 (activity_id, GuildPeriodKey) 取进度、按 activity_id 取今日次数与待发状态 ——
// 键取错(比如拿游戏日键去查档期进度)会让视图永远显示 0。
func TestBuildActivityViewKeys(t *testing.T) {
	row := lanternRow()
	gpk := activity.GuildPeriodKey(row, activityTestNow)
	view, err := buildActivityView(row, activityViewData{
		now:      activityTestNow,
		rule:     activityTestRule(),
		guild:    &data.GuildData{GuildID: 9, Level: 1},
		member:   data.MemberData{PlayerID: 42, JoinTimeMs: longAgoJoinMs()},
		usage:    map[uint32]uint32{1: 1},
		progress: map[data.ProgressKey]activity.Progress{{ActivityID: 1, PeriodKey: gpk}: {Count: 2}},
		rewards:  map[uint32]data.RewardStatus{1: {PendingCount: 1, PendingReasonTipID: assetop.ReasonBagFull}},
	})

	require.NoError(t, err)
	assert.Equal(t, pb.GuildActivityState_GUILD_ACTIVITY_STATE_OPEN, view.GetState())
	assert.Equal(t, uint32(1), view.GetMyUsedCount())
	assert.Equal(t, uint32(2), view.GetProgress())
	assert.Equal(t, constants.ErrActivityAlreadyClaimed, view.GetBlockedTipId(), "今日 1 次已用完")
	assert.Equal(t, uint32(1), view.GetMyPendingRewardCount())
	assert.Equal(t, assetop.ReasonBagFull, view.GetMyPendingReasonTipId(), "背包满是暂时原因,原样透给视图")
	assert.Equal(t, activityTestNowMs(), view.GetServerTimeMs())
}

// TestGetGuildActivitiesViews:读路径全流程(MySQL 替身回空 = 今日 0 次、无进度、无待发)。
// 团圆的进度是"入帮满 N 小时且在线"的人数;历练在 B6a 恒为 DISABLED;三行按 type 升序。
func TestGetGuildActivitiesViews(t *testing.T) {
	swapActivityTables(t, []*tablepb.GuildActivityTable{trialRow(), reunionRow(), lanternRow()}, activityTestRule())
	f := newActivityFixture(t, activityGuild(1, 42, 43, 44), ActivityDeps{}, 2)
	f.setOnline(t, 42, 43)
	writeSession(t, f.sessions, 44, plpb.PlayerSessionState_SESSION_STATE_OFFLINE)

	resp, err := f.l.GetGuildActivities(clientCtx(42), &pb.GetGuildActivitiesRequest{})

	require.NoError(t, err)
	require.Nil(t, resp.GetErrorMessage())
	views := resp.GetActivities()
	require.Len(t, views, 3)
	lantern, reunion, trial := views[0], views[1], views[2]

	assert.Equal(t, pb.GuildActivityType_GUILD_ACTIVITY_TYPE_LANTERN, lantern.GetType())
	assert.Equal(t, pb.GuildActivityState_GUILD_ACTIVITY_STATE_OPEN, lantern.GetState())
	assert.Zero(t, lantern.GetBlockedTipId(), "开放、未点过 = 可参与")
	assert.Zero(t, lantern.GetProgress())

	assert.Equal(t, pb.GuildActivityType_GUILD_ACTIVITY_TYPE_REUNION, reunion.GetType())
	assert.Equal(t, uint32(2), reunion.GetProgress(), "在线的 42、43;44 离线")
	assert.Equal(t, uint32(3), reunion.GetGuildThreshold())
	assert.Equal(t, constants.ErrActivityThresholdNotReached, reunion.GetBlockedTipId())

	assert.Equal(t, pb.GuildActivityType_GUILD_ACTIVITY_TYPE_TRIAL, trial.GetType())
	assert.Equal(t, pb.GuildActivityState_GUILD_ACTIVITY_STATE_DISABLED, trial.GetState())
	assert.Equal(t, constants.ErrActivityNotOpen, trial.GetBlockedTipId())
}

// TestGetGuildActivitiesOnlineFailureDegrades:只读路径读不到在线状态时降级为 0,不让整页失败(06 §6.8)。
func TestGetGuildActivitiesOnlineFailureDegrades(t *testing.T) {
	swapActivityTables(t, []*tablepb.GuildActivityTable{reunionRow()}, activityTestRule())
	f := newActivityFixture(t, activityGuild(1, 42, 43), ActivityDeps{}, 2)
	f.sessions.Close()

	resp, err := f.l.GetGuildActivities(clientCtx(42), &pb.GetGuildActivitiesRequest{})

	require.NoError(t, err)
	require.Len(t, resp.GetActivities(), 1)
	assert.Zero(t, resp.GetActivities()[0].GetProgress())
}

// TestGetGuildActivitiesOtherZoneIsNotInGuild:读路径保留 clientZone + visibleIn(Y-01):
// 帮会不在本人归属区时答"未入帮",不透露"它在别的区";也不碰任何活动读。
func TestGetGuildActivitiesOtherZoneIsNotInGuild(t *testing.T) {
	swapActivityTables(t, []*tablepb.GuildActivityTable{lanternRow()}, activityTestRule())
	f := newActivityFixture(t, activityGuild(1, 42), ActivityDeps{}, 3)

	resp, err := f.l.GetGuildActivities(clientCtx(42), &pb.GetGuildActivitiesRequest{})

	require.NoError(t, err)
	assert.Equal(t, constants.ErrNotInGuild, resp.GetErrorMessage().GetId())
	assert.Empty(t, resp.GetActivities())
}

// ── 灯会预检 ──────────────────────────────────────────────────

// runLantern 布好配表(只有 row 一行活动 + Reward[1])与夹具后,以 42 号身份点一次灯。
func runLantern(t *testing.T, row *tablepb.GuildActivityTable, rule *tablepb.GuildRuleTable, g data.GuildData,
	deps ActivityDeps, reqID uint32) (*pb.LightGuildLanternResponse, error) {
	t.Helper()
	swapActivityTables(t, []*tablepb.GuildActivityTable{row}, rule, rewardItemRow())
	f := newActivityFixture(t, g, deps, 2)
	return f.l.LightGuildLantern(clientCtx(42), &pb.LightGuildLanternRequest{ActivityId: reqID})
}

// requireRefusedBeforeTx:业务拒绝回 tip、不回 gRPC 错误,且被拒绝时不带视图。
// MySQL 替身开不了事务,走到事务的结果只会是 gRPC 错误,所以这里的 NoError 同时证明"没走到事务"。
func requireRefusedBeforeTx(t *testing.T, resp *pb.LightGuildLanternResponse, err error, want uint32, wantParams ...string) {
	t.Helper()
	require.NoError(t, err, "业务拒绝回 tip,不回 gRPC 错误;走到事务才会出错")
	assert.Equal(t, want, resp.GetErrorMessage().GetId())
	if len(wantParams) > 0 {
		assert.Equal(t, wantParams, resp.GetErrorMessage().GetParameters(), "带参数的码只放参数本身")
	}
	assert.Nil(t, resp.GetActivity(), "被拒绝时不带视图")
}

// TestLanternPrechecksRefuseBeforeTx:每一条业务拒绝都发生在事务与发号之前(06 §6.11.1)。
func TestLanternPrechecksRefuseBeforeTx(t *testing.T) {
	t.Run("请求的 id 不是当前选中行", func(t *testing.T) {
		resp, err := runLantern(t, lanternRow(), activityTestRule(), activityGuild(1, 42), ActivityDeps{}, 99)

		requireRefusedBeforeTx(t, resp, err, constants.ErrActivityNotOpen)
	})
	t.Run("帮会等级不足带 [min_level]", func(t *testing.T) {
		row := lanternRow()
		row.MinGuildLevel = 2

		resp, err := runLantern(t, row, activityTestRule(), activityGuild(1, 42), ActivityDeps{}, 1)

		requireRefusedBeforeTx(t, resp, err, constants.ErrActivityLevelTooLow, "2")
	})
	t.Run("入帮未满 N 小时带 [N]", func(t *testing.T) {
		rule := activityTestRule()
		rule.ActivityJoinMinHours = 24
		g := activityGuild(1, 42)
		g.Members[0].JoinTimeMs = activityTestNowMs() - 3_600_000

		resp, err := runLantern(t, lanternRow(), rule, g, ActivityDeps{}, 1)

		requireRefusedBeforeTx(t, resp, err, constants.ErrActivityJoinTooRecent, "24")
	})
	t.Run("带物品但资产通道关闭:发号之前拒绝", func(t *testing.T) {
		minter := &countingMinter{id: 77}

		resp, err := runLantern(t, lanternWithReward(), activityTestRule(), activityGuild(1, 42), ActivityDeps{OpIDs: minter}, 1)

		requireRefusedBeforeTx(t, resp, err, constants.ErrAssetPending)
		assert.Zero(t, minter.calls, "通道关着不发号(号段只进不退)")
	})
	t.Run("带物品但发号器未接线", func(t *testing.T) {
		loop, _, _ := newCountingLoop(t)

		resp, err := runLantern(t, lanternWithReward(), activityTestRule(), activityGuild(1, 42), ActivityDeps{Loop: loop}, 1)

		requireRefusedBeforeTx(t, resp, err, constants.ErrIDGenUnavailable)
	})
	t.Run("带物品但发号器拒绝发号", func(t *testing.T) {
		loop, applier, _ := newCountingLoop(t)
		minter := &countingMinter{err: errors.New("segment fenced")}

		resp, err := runLantern(t, lanternWithReward(), activityTestRule(), activityGuild(1, 42), ActivityDeps{Loop: loop, OpIDs: minter}, 1)

		requireRefusedBeforeTx(t, resp, err, constants.ErrIDGenUnavailable)
		assert.Equal(t, 1, minter.calls)
		assert.Zero(t, applier.count(), "没发到号就不会有指令,更不会投递")
	})
}

// TestLanternAllPrechecksPassReachesTx:预检全过就进事务。替身库开不了事务,于是结果是故障(gRPC 错误)
// 而不是任何业务 tip —— 证明前面几条用例的 tip 确实来自预检,而不是事务。
func TestLanternAllPrechecksPassReachesTx(t *testing.T) {
	swapActivityTables(t, []*tablepb.GuildActivityTable{lanternRow()}, activityTestRule())
	f := newActivityFixture(t, activityGuild(1, 42), ActivityDeps{}, 2)

	resp, err := f.l.LightGuildLantern(clientCtx(42), &pb.LightGuildLanternRequest{ActivityId: 1})

	require.Error(t, err)
	assert.Nil(t, resp)
}

// ── 团圆预检 ──────────────────────────────────────────────────

// TestReunionThresholdNotReachedBeforeMinting:人数不够回 [online, threshold],发生在发号之前(号段只进不退)。
// 入帮未满 N 小时的在线成员不计数(防"刷完活动就换帮再刷")。
func TestReunionThresholdNotReachedBeforeMinting(t *testing.T) {
	rule := activityTestRule()
	rule.ActivityJoinMinHours = 24
	swapActivityTables(t, []*tablepb.GuildActivityTable{reunionWithReward()}, rule, rewardItemRow())
	g := activityGuild(1, 42, 43)
	g.Members = append(g.Members, data.MemberData{PlayerID: 44, Role: constants.RoleMember, JoinTimeMs: activityTestNowMs() - 3_600_000})
	loop, applier, _ := newCountingLoop(t)
	minter := &countingMinter{id: 77}
	f := newActivityFixture(t, g, ActivityDeps{Loop: loop, OpIDs: minter}, 2)
	f.setOnline(t, 42, 43, 44)

	resp, err := f.l.ClaimGuildReunion(clientCtx(42), &pb.ClaimGuildReunionRequest{ActivityId: 2})

	require.NoError(t, err)
	assert.Equal(t, constants.ErrActivityThresholdNotReached, resp.GetErrorMessage().GetId())
	assert.Equal(t, []string{"2", "3"}, resp.GetErrorMessage().GetParameters(), "44 在线但入帮未满 24 小时,不计数")
	assert.Zero(t, minter.calls, "人数不够不发号")
	assert.Zero(t, applier.count())
}

// TestReunionOnlineLookupFailureIsFault:数人数时会话 Redis 读不到 → 整次领奖按故障返回(fail-closed),
// 不当"人数不足"(会把故障伪装成业务结论),更不当在线多算(锁存不可撤销);也不发号。
func TestReunionOnlineLookupFailureIsFault(t *testing.T) {
	swapActivityTables(t, []*tablepb.GuildActivityTable{reunionWithReward()}, activityTestRule(), rewardItemRow())
	loop, _, _ := newCountingLoop(t)
	minter := &countingMinter{id: 77}
	f := newActivityFixture(t, activityGuild(1, 42, 43, 44), ActivityDeps{Loop: loop, OpIDs: minter}, 2)
	f.sessions.Close()

	resp, err := f.l.ClaimGuildReunion(clientCtx(42), &pb.ClaimGuildReunionRequest{ActivityId: 2})

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrOnlineStateUnknown)
	assert.Nil(t, resp)
	assert.Zero(t, minter.calls)
}

// TestReunionEnoughOnlineMintsThenReachesTx:人数够 → 发号 → 进事务(替身库开不了事务,以故障收场)。
func TestReunionEnoughOnlineMintsThenReachesTx(t *testing.T) {
	swapActivityTables(t, []*tablepb.GuildActivityTable{reunionWithReward()}, activityTestRule(), rewardItemRow())
	loop, applier, _ := newCountingLoop(t)
	minter := &countingMinter{id: 77}
	f := newActivityFixture(t, activityGuild(1, 42, 43, 44), ActivityDeps{Loop: loop, OpIDs: minter}, 2)
	f.setOnline(t, 42, 43, 44)

	resp, err := f.l.ClaimGuildReunion(clientCtx(42), &pb.ClaimGuildReunionRequest{ActivityId: 2})

	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Equal(t, 1, minter.calls, "物品指令的 op_id 在事务之前发")
	assert.Zero(t, applier.count(), "事务没提交就不投递")
}

// TestReunionEligible:只数入帮满 N 小时的成员;minHours=0 全算;入帮时刻为 0(未知)在有下限时不算。
func TestReunionEligible(t *testing.T) {
	nowMs := activityTestNowMs()
	g := &data.GuildData{Members: []data.MemberData{
		{PlayerID: 1, JoinTimeMs: nowMs - 25*3_600_000},
		{PlayerID: 2, JoinTimeMs: nowMs - 23*3_600_000},
		{PlayerID: 3, JoinTimeMs: 0},
	}}

	assert.Equal(t, []uint64{1, 2, 3}, reunionEligible(g, nowMs, 0))
	assert.Equal(t, []uint64{1}, reunionEligible(g, nowMs, 24))
	assert.Nil(t, reunionEligible(nil, nowMs, 0))
}

// ── 错误映射与指标 ────────────────────────────────────────────

// TestActivityTxTipMapping(06 §6.10,按 90 part2 §2 第 5 条订正):事务哨兵 → tip / 故障;写冲突是 tip 不是 gRPC 错误。
func TestActivityTxTipMapping(t *testing.T) {
	l := NewGuildLogic(nil, nil, nil, nil, nil) // repo 为 nil:mapWriteErr 的映射自愈直接跳过
	row := lanternRow()
	row.MinGuildLevel = 3
	rule := activityTestRule()
	rule.ActivityJoinMinHours = 24
	a := activityActor{playerID: 42, guild: &data.GuildData{GuildID: 9}}
	pre := activityPrecheck{row: row, rule: rule}

	cases := []struct {
		name       string
		err        error
		wantTip    uint32
		wantParams []string
		wantResult string
		wantFault  bool
	}{
		{"帮会等级", data.ErrGuildLevelTooLow, constants.ErrActivityLevelTooLow, []string{"3"}, activityResultLevelLow, false},
		{"入帮时长", data.ErrActivityJoinTooRecent, constants.ErrActivityJoinTooRecent, []string{"24"}, activityResultJoinRecent, false},
		{"今日已领", data.ErrActivityAlreadyClaimed, constants.ErrActivityAlreadyClaimed, nil, activityResultClaimed, false},
		{"团圆人数", data.ErrActivityThresholdNotReached, constants.ErrActivityThresholdNotReached, []string{"2", "3"}, activityResultThreshold, false},
		{"未决指令过多", fmt.Errorf("allocate seq: %w", assetop.ErrTooManyPending), constants.ErrAssetPending, nil, activityResultAssetPending, false},
		{"写冲突", data.ErrWriteConflict, constants.ErrBusyRetry, nil, activityResultBusyRetry, false},
		{"合服闸门", fmt.Errorf("%w: fence unreadable", data.ErrZoneMerging), constants.ErrZoneMerging, nil, activityResultMerging, false},
		{"不是成员", data.ErrNotGuildMember, constants.ErrNotInGuild, nil, activityResultNotInGuild, false},
		{"帮会不存在", data.ErrGuildGone, constants.ErrGuildNotFound, nil, activityResultNotInGuild, false},
		{"溢出是故障", fmt.Errorf("%w: funds", data.ErrActivityPoison), 0, nil, activityResultError, true},
		{"未知错误是故障", errors.New("driver: bad connection"), 0, nil, activityResultError, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tip, fault := l.activityTxTip(context.Background(), a, pre, 2, 3, tc.err)

			assert.Equal(t, tc.wantResult, activityResultOf(tip, fault))
			if tc.wantFault {
				require.Error(t, fault)
				assert.Nil(t, tip)
				return
			}
			require.NoError(t, fault)
			assert.Equal(t, tc.wantTip, tip.GetId())
			if tc.wantParams != nil {
				assert.Equal(t, tc.wantParams, tip.GetParameters())
			}
		})
	}

	tip, fault := l.activityTxTip(context.Background(), a, pre, 0, 0, nil)
	assert.Nil(t, tip)
	assert.NoError(t, fault)
}

// TestActivityResultOf:label 取值有界 —— 故障按 gRPC 码分三类,未列出的 tip 归 other_reject。
func TestActivityResultOf(t *testing.T) {
	assert.Equal(t, activityResultOK, activityResultOf(nil, nil))
	assert.Equal(t, activityResultDenied, activityResultOf(nil, status.Error(codes.PermissionDenied, "x")))
	assert.Equal(t, activityResultUnavailable, activityResultOf(nil, status.Error(codes.Unavailable, "x")))
	assert.Equal(t, activityResultError, activityResultOf(nil, errors.New("x")))
	assert.Equal(t, activityResultNotOpen, activityResultOf(tipErr(constants.ErrActivityNotOpen, "x"), nil))
	assert.Equal(t, activityResultHomeZone, activityResultOf(tipErr(constants.ErrHomeZoneUnknown, "x"), nil))
	assert.Equal(t, activityResultIDUnavailable, activityResultOf(tipErr(constants.ErrIDGenUnavailable, "x"), nil))
	assert.Equal(t, activityResultOtherReject, activityResultOf(tipErr(constants.ErrGuildFull, "x"), nil))
}

// ── 同步投递预算(06 §6.40 TestLanternSyncBudget,按裁决 K 改为从 ctx 截止时间倒推)──

// deadlineApplier 是假 scene:一律回 RETRY,记下调用次数与每次收到的 ctx 截止时间。
type deadlineApplier struct {
	mu        sync.Mutex
	calls     int
	deadlines []time.Time
}

var _ assetop.Applier = (*deadlineApplier)(nil)

func (a *deadlineApplier) Do(ctx context.Context, _ assetop.RPC, _ *assetpb.AssetOpRequest) (assetop.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if deadline, ok := ctx.Deadline(); ok {
		a.deadlines = append(a.deadlines, deadline)
	}
	return assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_RETRY}, nil
}

func (a *deadlineApplier) snapshot() (int, []time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls, append([]time.Time(nil), a.deadlines...)
}

// TestLanternSyncBudget:预算 = min(SyncBudget, ctx 剩余 − 1000ms 尾巴),不足 300ms 就不投、Loop 不碰 Store
// (行保持插行租约,到期交给重投循环);余量充足 → 恰好投一次,投递 ctx 的截止 = 建 ctx 那一刻 + SyncBudget。
//
// 确定性(AGENTS.md §11.4):父 ctx 的截止由用例注入(已过 / 至多 900ms / 没有截止),Loop 的时钟注入固定值,
// 断言只用"单调钟只进不退"推得出的不等式,结论与机器快慢、调度时延无关 ——
//   - 跳过:注入的截止要么已过、要么离建 ctx 至多 900ms;此后时间只会流逝,扣掉 1000ms 尾巴恒为负,恒跳过。
//   - 投递:SyncBudget 取 500ms,小于 Loop 自己给投递的上限(OpBudget − settleBudget = 1800ms),所以 Do 看到的截止
//     恰是"deliverActivityReward 建 ctx 那一刻 + 500ms"。那一刻夹在调用前后两次读钟之间,截止也就恒落在
//     [before+500ms, after+500ms] 里;预算若没取 SyncBudget(取大了或取小了),两端必有一端不成立。
//     旧写法断言"截止 − 调用前读钟 ≤ SyncBudget",成立与否取决于调用前后隔了多久,是调度时延决定的结论。
//
// clampSyncBudget 本身的边界(恰好 300ms、差 1ms)由 economy_logic_test.go 的 TestClampSyncBudget 以注入的剩余时长覆盖。
func TestLanternSyncBudget(t *testing.T) {
	op := assetop.Op{
		OpID:          1,
		PlayerID:      42,
		Stream:        assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT,
		Seq:           1,
		StreamEpoch:   1,
		CorrelationID: 1,
		TxType:        uint32(rollbackpb.TransactionType_TX_GUILD_ACTIVITY_REWARD),
		Bundle:        &assetpb.AssetBundle{Items: []*assetpb.ItemGrant{{ConfigId: 1001, Count: 4}}},
		LeaseToken:    1,
	}
	newDeps := func(t *testing.T, syncBudget time.Duration) (*ActivityDeps, *deadlineApplier, *countingAssetStore) {
		t.Helper()
		applier := &deadlineApplier{}
		store := &countingAssetStore{}
		loop, err := assetop.NewLoop(assetop.DefaultLoopConfig(), store, applier, nil, activityTestClock)
		require.NoError(t, err)
		return &ActivityDeps{Loop: loop, SyncBudget: syncBudget}, applier, store
	}
	l := NewGuildLogic(nil, nil, nil, nil, nil)

	skips := []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
	}{
		{"截止已过", func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.Background(), time.Unix(0, 0))
		}},
		{"只剩至多 900ms", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 900*time.Millisecond)
		}},
	}
	for _, tc := range skips {
		t.Run(tc.name+":不投、不碰 Store", func(t *testing.T) {
			d, applier, store := newDeps(t, defaultSyncBudget)
			ctx, cancel := tc.ctx()
			defer cancel()

			l.deliverActivityReward(ctx, d, op)

			calls, _ := applier.snapshot()
			assert.Zero(t, calls, "预算不足必须跳过同步投递")
			assert.Zero(t, store.count(), "跳过时 Loop 不写行,行保持插行时的租约")
		})
	}

	t.Run("余量充足:恰投一次,截止 = 建 ctx 时刻 + SyncBudget", func(t *testing.T) {
		const syncBudget = 500 * time.Millisecond
		d, applier, _ := newDeps(t, syncBudget)

		before := time.Now()
		l.deliverActivityReward(context.Background(), d, op)
		after := time.Now()

		calls, deadlines := applier.snapshot()
		require.Equal(t, 1, calls, "满额预算时同步投递恰好一次")
		require.Len(t, deadlines, 1)
		assert.False(t, deadlines[0].Before(before.Add(syncBudget)), "投递预算不能小于 SyncBudget(截止 %v,调用前 %v)", deadlines[0], before)
		assert.False(t, deadlines[0].After(after.Add(syncBudget)), "投递预算不能大于 SyncBudget(截止 %v,调用后 %v)", deadlines[0], after)
	})
}

// TestDeliverActivityRewardWithoutLoop:Loop 未接线时只记日志,不 panic(预检本应已拒绝,这里是防御)。
func TestDeliverActivityRewardWithoutLoop(t *testing.T) {
	l := NewGuildLogic(nil, nil, nil, nil, nil)

	assert.NotPanics(t, func() {
		l.deliverActivityReward(context.Background(), &ActivityDeps{SyncBudget: defaultSyncBudget}, assetop.Op{OpID: 1})
	})
}

// ── BatchResolveStrict ───────────────────────────────────────

// TestBatchResolveStrictStates:判据与 BatchResolve 相同(只有 ONLINE 算在线,键不存在 = 离线),输入去重。
func TestBatchResolveStrictStates(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	writeSession(t, mr, 1, plpb.PlayerSessionState_SESSION_STATE_ONLINE)
	writeSession(t, mr, 2, plpb.PlayerSessionState_SESSION_STATE_OFFLINE)
	writeSession(t, mr, 3, plpb.PlayerSessionState_SESSION_STATE_ONLINE)
	// 5 号刻意不写:键不存在是确定的离线,不是"读不到"。

	online, err := NewOnlineStatusResolver(rdb).BatchResolveStrict(context.Background(), []uint64{1, 2, 3, 5, 1})

	require.NoError(t, err)
	assert.Equal(t, map[uint64]bool{1: true, 3: true}, online)
}

// TestBatchResolveStrictFailsClosed:任何一人的状态读不到就整体失败,不把"读不到"当"离线"。
func TestBatchResolveStrictFailsClosed(t *testing.T) {
	t.Run("会话值解不开", func(t *testing.T) {
		mr := miniredis.RunT(t)
		rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { rdb.Close() })
		writeSession(t, mr, 1, plpb.PlayerSessionState_SESSION_STATE_ONLINE)
		// 0x08 = 字段 1 的 varint 标签后面什么都没有,proto.Unmarshal 必定失败。
		require.NoError(t, mr.Set(playerSessionKey(4), string([]byte{0x08})))

		online, err := NewOnlineStatusResolver(rdb).BatchResolveStrict(context.Background(), []uint64{1, 4})

		require.ErrorIs(t, err, ErrOnlineStateUnknown)
		assert.Nil(t, online)
	})
	t.Run("Redis 不可达", func(t *testing.T) {
		mr := miniredis.RunT(t)
		rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { rdb.Close() })
		mr.Close()
		r := NewOnlineStatusResolver(rdb)
		r.strictTimeout = 200 * time.Millisecond

		_, err := r.BatchResolveStrict(context.Background(), []uint64{1})

		require.ErrorIs(t, err, ErrOnlineStateUnknown)
	})
	t.Run("未配置会话 Redis", func(t *testing.T) {
		var r *OnlineStatusResolver

		_, err := r.BatchResolveStrict(context.Background(), []uint64{1})

		require.ErrorIs(t, err, ErrOnlineStateUnknown)
	})
	t.Run("没有候选人时不读 Redis,0 人在线是真话", func(t *testing.T) {
		var r *OnlineStatusResolver

		online, err := r.BatchResolveStrict(context.Background(), nil)

		require.NoError(t, err)
		assert.Empty(t, online)
	})
	t.Run("父 ctx 已取消", func(t *testing.T) {
		r := NewOnlineStatusResolver(stalledRedis(t))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		// 只断言 fail-closed:取消原因可能来自本协程的 ctx 分支,也可能来自 go-redis 取连接时的包装,形态不定。
		_, err := r.BatchResolveStrict(ctx, []uint64{1})

		require.ErrorIs(t, err, ErrOnlineStateUnknown)
	})
}

// TestBatchResolveStrictHasOwnTimeout(92-handoff §8.2 的旧问题):Redis 收下连接却一直不回话时,
// 严格版靠自己的上限返回,而不是陪着套接字等(locator 客户端没开 ContextTimeoutEnabled,ctx 截止管不住套接字读)。
//
// 确定性(AGENTS.md §11.4):不量耗时。stalledRedis 的客户端**关掉了套接字读超时**,假 Redis 又永不回话,
// 所以 MGET 那一路永远不会自己返回;父 ctx 没有截止、也不取消 —— 能让 BatchResolveStrict 返回、并带上
// context.DeadlineExceeded 的,只剩它自己的独立上限这一条路。独立上限一旦失效,调用就永不返回,
// 下面的兜底等待必然触发(它只防整个测试进程挂死,离 50ms 的上限有三个数量级的余量,不是结论的一部分)。
// 旧写法断言"耗时 < 1s",结论取决于机器负载与调度时延;而且在读超时打开时,go-redis 3s 后的重试退避也会回
// DeadlineExceeded,单看错误类型分不出是谁超的时 —— 关掉读超时正是为了排除这条旁路。
func TestBatchResolveStrictHasOwnTimeout(t *testing.T) {
	r := NewOnlineStatusResolver(stalledRedis(t))
	r.strictTimeout = 50 * time.Millisecond

	done := make(chan error, 1)
	go func() {
		_, err := r.BatchResolveStrict(context.Background(), []uint64{1, 2})
		done <- err
	}()

	var err error
	select {
	case err = <-done:
	case <-time.After(time.Minute):
		t.Fatal("BatchResolveStrict 没有返回:独立上限失效,调用方会陪着一个不回话的 Redis 一直等下去")
	}
	require.ErrorIs(t, err, ErrOnlineStateUnknown)
	assert.ErrorIs(t, err, context.DeadlineExceeded, "返回必须来自自己的独立上限(父 ctx 没有截止)")
}

// stalledRedis 返回一个连到"只收连接、永不回话"的假 Redis 的客户端。客户端关掉套接字读 / 写超时(-1 = 一直阻塞):
// 这样 MGET 自己永远不会返回,任何返回都只能来自调用方的 ctx 或独立上限,用例的结论不依赖读超时与重试退避的时长。
// t.Cleanup 关掉监听与全部连接,被丢下的 MGET 协程随之拿到读错误(EOF)退出,不会泄漏到别的用例。
func stalledRedis(t *testing.T) *goredis.Client {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var (
		mu    sync.Mutex
		conns []net.Conn
	)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()
	rdb := goredis.NewClient(&goredis.Options{Addr: ln.Addr().String(), ReadTimeout: -1, WriteTimeout: -1})
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, conn := range conns {
			_ = conn.Close()
		}
		mu.Unlock()
		_ = rdb.Close()
	})
	return rdb
}
