package team

// 整队开战编排的测试(设计文档 docs/design/team-system.md §E.1;§I.3 #17、#18、#24、#28、#30 的 team 部分)。
//
// 票据域用假端口 fakeBattlePort;真实实现 logic.TeamBattleStarter 的票据 / gather 用例在
// internal/logic/team_battle_test.go。队伍记录、开战锁、EndMatch 走真 Store + miniredis。
// 确定性:沿用 serviceHarness(miniredis 时钟固定、asyncFn 同步、推送 / scene 桩);EndMatch 退避经
// endMatchSleepFn 记录、不真睡;并发时序用 afterReadHook / afterPreflightHook / afterMatchReadHook
// 在固定点插入。测试之间不并行(包级测试缝)。

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"match/internal/config"
	"match/internal/playercontract"

	teampb "proto/team"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

const (
	testBattleConfig uint32 = 1
	testLockSeconds         = 83
)

type ticketCall struct {
	configId uint32
	teamId   uint64
	roster   []uint64
	zones    map[uint64]uint32
}

type gatherRun struct {
	configId uint32
	roster   []uint64
	tickets  map[uint64]string
}

// fakeBattlePort 假票据域端口:按配置给人数 / 票据阻塞 / 建票失败者 / gather 结果,并记录每次调用。
type fakeBattlePort struct {
	mu          sync.Mutex
	sizes       map[uint32]uint32
	blocked     map[uint64]bool
	failTicket  uint64
	gatherOK    bool
	onGather    func(run gatherRun)
	checks      []uint64
	ticketCalls []ticketCall
	gathers     []gatherRun
}

var _ BattleStarter = (*fakeBattlePort)(nil)

func (f *fakeBattlePort) TeamSizeFor(battleConfigId uint32) uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sizes[battleConfigId]
}

func (f *fakeBattlePort) MatchLockTTLSeconds(int) int { return testLockSeconds }

func (f *fakeBattlePort) TicketBlocked(_ context.Context, playerId uint64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks = append(f.checks, playerId)
	return f.blocked[playerId], nil
}

func (f *fakeBattlePort) CreateMatchedTickets(_ context.Context, battleConfigId uint32, teamId uint64,
	roster []uint64, zones map[uint64]uint32,
) (map[uint64]string, uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ticketCalls = append(f.ticketCalls, ticketCall{
		configId: battleConfigId, teamId: teamId, roster: append([]uint64(nil), roster...), zones: zones,
	})
	if f.failTicket != 0 {
		return nil, f.failTicket
	}
	tickets := make(map[uint64]string, len(roster))
	for _, pid := range roster {
		tickets[pid] = "tk-" + strconv.FormatUint(pid, 10)
	}
	return tickets, 0
}

func (f *fakeBattlePort) RunGather(battleConfigId uint32, roster []uint64, tickets map[uint64]string) bool {
	run := gatherRun{configId: battleConfigId, roster: append([]uint64(nil), roster...), tickets: tickets}
	f.mu.Lock()
	f.gathers = append(f.gathers, run)
	onGather, ok := f.onGather, f.gatherOK
	f.mu.Unlock()
	if onGather != nil {
		onGather(run)
	}
	return ok
}

type battleHarness struct {
	*serviceHarness
	port   *fakeBattlePort
	teamId uint64
	sleeps []time.Duration
}

// newBattleHarness members[0] 建队当队长,其余按顺序申请入队(join_seq 1..n);全员在线、在场景(节点 1)。
func newBattleHarness(t *testing.T, members ...uint64) *battleHarness {
	t.Helper()
	h := newServiceHarness(t, config.TeamConf{})
	b := &battleHarness{
		serviceHarness: h,
		port:           &fakeBattlePort{sizes: map[uint32]uint32{testBattleConfig: Capacity}, gatherOK: true},
	}
	h.service.starter = b.port
	prevPreflight, prevMatchRead, prevSleep := afterPreflightHook, afterMatchReadHook, endMatchSleepFn
	endMatchSleepFn = func(d time.Duration) { b.sleeps = append(b.sleeps, d) }
	t.Cleanup(func() {
		afterPreflightHook, afterMatchReadHook, endMatchSleepFn = prevPreflight, prevMatchRead, prevSleep
	})
	h.addPlayers(testZone, members...)
	b.teamId = h.create(members[0])
	for _, pid := range members[1:] {
		h.join(members[0], b.teamId, pid)
	}
	h.takePushes()
	h.takeScenes()
	return b
}

func (b *battleHarness) start(caller uint64) *teampb.TeamResponse {
	b.t.Helper()
	return b.ok(b.server.StartTeamMatch(asPlayer(caller),
		&teampb.StartTeamMatchRequest{BattleConfigId: testBattleConfig, ExpectedTeamId: b.teamId}))
}

func requireTip(t *testing.T, resp *teampb.TeamResponse, code uint32, param uint64) {
	t.Helper()
	requireCode(t, code, resp)
	if param == 0 {
		require.Empty(t, resp.GetErrorMessage().GetParameters())
		return
	}
	require.Equal(t, []string{strconv.FormatUint(param, 10)}, resp.GetErrorMessage().GetParameters())
}

// snapshotsByReason 取出指定原因的快照推送:接收者 → 推送内容(同一原因不应重复推给同一人)。
func snapshotsByReason(t *testing.T, pushes []pushRecord, reason teampb.TeamChangeReason) map[uint64]*teampb.TeamSnapshotS2C {
	t.Helper()
	out := map[uint64]*teampb.TeamSnapshotS2C{}
	for _, p := range pushes {
		snap, ok := p.msg.(*teampb.TeamSnapshotS2C)
		if !ok || snap.GetReason() != reason {
			continue
		}
		_, dup := out[p.playerId]
		require.False(t, dup, "原因 %v 重复推给 %d", reason, p.playerId)
		out[p.playerId] = snap
	}
	return out
}

func pushedTo(m map[uint64]*teampb.TeamSnapshotS2C) []uint64 {
	ids := make([]uint64, 0, len(m))
	for pid := range m {
		ids = append(ids, pid)
	}
	return ids
}

// ---- 纯函数规则 ----

func TestMatchLockRules(t *testing.T) {
	require.Equal(t, 5, Capacity, "logic 测试同样钉住 kMaxBattleTeamSize == 5(两包互不 import),改容量要两边同步")

	rec := newRecord(1, 2, 3)
	rec.LeaderId = 2
	require.Equal(t, []uint64{2, 1, 3}, MatchRoster(rec), "队长在前,其余按 join_seq")

	require.Equal(t, ErrDungeonNotOpen, CheckMatchTeamSize(rec, 0))
	require.Equal(t, ErrSizeExceeded, CheckMatchTeamSize(rec, 2))
	require.Zero(t, CheckMatchTeamSize(rec, 3))
	require.Zero(t, CheckMatchTeamSize(rec, 5), "人数少于上限允许开战(J-5)")
	require.Equal(t, ErrNotLeader, CheckMatchStart(rec, 1, ruleNow))
	require.Zero(t, CheckMatchStart(rec, 2, ruleNow))

	orig := proto.Clone(rec)
	d := LockMatch(rec, 2, "tok", []uint64{2, 1, 3}, ruleNow+83_000, ruleNow)
	require.Zero(t, d.Code)
	require.True(t, d.Changed)
	require.True(t, proto.Equal(orig, rec), "纯函数不改入参")
	require.Equal(t, teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_STARTED, d.Reason)
	require.ElementsMatch(t, []uint64{1, 2, 3}, d.Kept)
	require.Empty(t, d.Joined)
	require.Empty(t, d.Left)
	require.Equal(t, "tok", d.Record.GetMatchLockToken())
	require.Equal(t, ruleNow+83_000, d.Record.GetMatchLockExpireAtMs())
	require.Equal(t, []uint64{2, 1, 3}, d.Record.GetMatchLockRoster())
	require.Equal(t, ErrStateChanged, LockMatch(rec, 2, "tok", []uint64{2, 1}, ruleNow+83_000, ruleNow).Code,
		"名单与成员集合不等绝不加锁")
	require.Equal(t, ErrInMatch, LockMatch(d.Record, 2, "tok2", []uint64{2, 1, 3}, ruleNow+84_000, ruleNow+1).Code)

	locked := d.Record
	require.False(t, ReleaseMatchLock(locked, "other", true, ruleNow+1, nil).Changed, "token 不符不清")
	require.False(t, ReleaseMatchLock(locked, "tok", true, ruleNow+83_000, nil).Changed, "锁已过期不清")
	failed := ReleaseMatchLock(locked, "tok", false, ruleNow+1, nil)
	require.True(t, failed.Changed)
	require.Equal(t, teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_FAILED, failed.Reason)
	require.Empty(t, failed.Record.GetMatchLockToken())
	require.Zero(t, failed.Record.GetMatchLockExpireAtMs())
	require.Empty(t, failed.Record.GetMatchLockRoster())
	require.ElementsMatch(t, []uint64{1, 2, 3}, failed.Kept)
	require.Equal(t, teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_ENDED,
		ReleaseMatchLock(locked, "tok", true, ruleNow+1, nil).Reason)

	// 锁期间 {-2} 修复移出成员:锁内名单同步去掉他,保持 roster == members。
	fix := RepairRemoveMember(locked, 3, ruleNow+1, nil)
	require.True(t, fix.Changed)
	require.Equal(t, []uint64{2, 1}, fix.Record.GetMatchLockRoster())
	require.ElementsMatch(t, fix.Record.GetMatchLockRoster(), MemberIds(fix.Record))
}

// ---- #17:加锁前的拒绝分支 ----

func TestStartTeamMatchRejectsBeforeLocking(t *testing.T) {
	cases := []struct {
		name   string
		caller uint64
		setup  func(b *battleHarness)
		code   uint32
		param  uint64
	}{
		{name: "非队长", caller: 102, code: ErrNotLeader},
		{name: "副本未开放组队", caller: 101, setup: func(b *battleHarness) { b.port.sizes = map[uint32]uint32{} }, code: ErrDungeonNotOpen},
		{name: "人数超过副本上限", caller: 101, setup: func(b *battleHarness) { b.port.sizes[testBattleConfig] = 2 }, code: ErrSizeExceeded},
		{name: "队员离线", caller: 101, setup: func(b *battleHarness) { b.mr.Del(playercontract.SessionKey(103)) },
			code: ErrMemberOffline, param: 103},
		{name: "队员在战斗中", caller: 101, setup: func(b *battleHarness) {
			require.NoError(b.t, b.mr.Set(playercontract.BattleLockKey(102), "777"))
		}, code: ErrMemberInBattle, param: 102},
		{name: "队员不在场景", caller: 101, setup: func(b *battleHarness) { b.mr.Del(playercontract.LocationKey(103)) },
			code: ErrMemberNotReady, param: 103},
		{name: "队员已有在途票据", caller: 101, setup: func(b *battleHarness) { b.port.blocked = map[uint64]bool{102: true} },
			code: ErrMemberNotReady, param: 102},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newBattleHarness(t, 101, 102, 103)
			if tc.setup != nil {
				tc.setup(b)
			}
			ver := b.versionOf(b.teamId)

			resp := b.start(tc.caller)

			requireTip(t, resp, tc.code, tc.param)
			require.Equal(t, b.teamId, resp.GetTeam().GetTeamId(), "拒绝回包带调用者当前视图")
			require.Equal(t, teampb.TeamMatchState_TEAM_MATCH_STATE_IDLE, resp.GetTeam().GetMatchState())
			require.Equal(t, ver, b.versionOf(b.teamId), "加锁前拒绝不写记录")
			require.Empty(t, loadRecord(t, b.mr, b.teamId).GetMatchLockToken())
			require.Empty(t, b.port.ticketCalls)
			require.Empty(t, b.port.gathers)
			require.Empty(t, b.takePushes())
		})
	}
}

// ---- #17:建票失败 → 锁被释放,推 MATCH_FAILED ----

func TestStartTeamMatchTicketFailureReleasesLock(t *testing.T) {
	b := newBattleHarness(t, 101, 102, 103, 104)
	b.port.failTicket = 104
	ver0 := b.versionOf(b.teamId)

	resp := b.start(101)

	requireTip(t, resp, ErrMemberNotReady, 104)
	require.Empty(t, b.port.gathers, "建票失败不进 gather")
	require.Len(t, b.port.ticketCalls, 1)
	rec := loadRecord(t, b.mr, b.teamId)
	require.Empty(t, rec.GetMatchLockToken(), "锁已释放")
	require.Equal(t, ver0+2, b.versionOf(b.teamId), "加锁一次 + 清锁一次")
	pushes := b.takePushes()
	require.ElementsMatch(t, []uint64{102, 103, 104},
		pushedTo(snapshotsByReason(t, pushes, teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_STARTED)))
	failed := snapshotsByReason(t, pushes, teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_FAILED)
	require.ElementsMatch(t, []uint64{101, 102, 103, 104}, pushedTo(failed), "结果推全员(含发起人)")
	for _, s := range failed {
		require.Equal(t, teampb.TeamMatchState_TEAM_MATCH_STATE_IDLE, s.GetTeam().GetMatchState())
		require.Equal(t, ver0+2, s.GetTeam().GetVersion())
		require.Equal(t, ErrMemberNotReady, s.GetTip().GetId(), "MATCH_FAILED 的原因在 tip 字段(§B.1、§H.2)")
		require.Equal(t, []string{"104"}, s.GetTip().GetParameters(), "parameters[0] = 出问题的成员")
	}
}

// ---- #18:roster 顺序、锁字段、gather 结束后清锁并推结果 ----

func TestStartTeamMatchLocksRosterAndEndsMatch(t *testing.T) {
	for _, gatherOK := range []bool{true, false} {
		t.Run("gather_ok="+strconv.FormatBool(gatherOK), func(t *testing.T) {
			b := newBattleHarness(t, 101, 102, 103)
			// 队长转给 join_seq=2 的 102:roster 应为 [102, 101, 103]。
			requireCode(t, 0, b.ok(b.server.TransferLeader(asPlayer(101),
				&teampb.TransferLeaderRequest{TargetPlayerId: 102, ExpectedTeamId: b.teamId})))
			b.takePushes()
			b.port.gatherOK = gatherOK
			ver0 := b.versionOf(b.teamId)
			wantRoster := []uint64{102, 101, 103}
			var duringGather *teampb.TeamRecord
			b.port.onGather = func(gatherRun) { duringGather = loadRecord(t, b.mr, b.teamId) }

			resp := b.start(102)

			requireCode(t, 0, resp)
			require.Equal(t, teampb.TeamMatchState_TEAM_MATCH_STATE_STARTING, resp.GetTeam().GetMatchState(), "回包 = 加锁提交的视图")
			require.Equal(t, ver0+1, resp.GetTeam().GetVersion())

			require.Len(t, b.port.ticketCalls, 1)
			call := b.port.ticketCalls[0]
			require.Equal(t, wantRoster, call.roster, "队长在前再按 join_seq")
			require.Equal(t, b.teamId, call.teamId, "票据带 team_id")
			require.Equal(t, testBattleConfig, call.configId)
			require.Equal(t, map[uint64]uint32{101: testZone, 102: testZone, 103: testZone}, call.zones)
			require.Len(t, b.port.gathers, 1)
			require.Equal(t, wantRoster, b.port.gathers[0].roster)
			require.Len(t, b.port.gathers[0].tickets, len(wantRoster))

			require.NotNil(t, duringGather)
			require.NotEmpty(t, duringGather.GetMatchLockToken())
			require.Equal(t, wantRoster, duringGather.GetMatchLockRoster(), "锁内名单 == roster")
			require.ElementsMatch(t, duringGather.GetMatchLockRoster(), MemberIds(duringGather))
			require.Equal(t, baseMs()+testLockSeconds*1000, duringGather.GetMatchLockExpireAtMs(), "锁截止 = Redis TIME + 时长")

			after := loadRecord(t, b.mr, b.teamId)
			require.Empty(t, after.GetMatchLockToken(), "gather 结束后锁被清除")
			require.Empty(t, after.GetMatchLockRoster())
			require.Zero(t, after.GetMatchLockExpireAtMs())
			require.Equal(t, ver0+2, b.versionOf(b.teamId))

			pushes := b.takePushes()
			started := snapshotsByReason(t, pushes, teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_STARTED)
			require.ElementsMatch(t, []uint64{101, 103}, pushedTo(started), "发起人看回包,其余队员收 MATCH_STARTED")
			for _, s := range started {
				require.Equal(t, teampb.TeamMatchState_TEAM_MATCH_STATE_STARTING, s.GetTeam().GetMatchState())
				require.Equal(t, ver0+1, s.GetTeam().GetVersion())
			}
			endReason := teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_FAILED
			if gatherOK {
				endReason = teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_ENDED
			}
			ended := snapshotsByReason(t, pushes, endReason)
			require.ElementsMatch(t, wantRoster, pushedTo(ended), "结果推全员(含发起人)")
			for _, s := range ended {
				require.Equal(t, teampb.TeamMatchState_TEAM_MATCH_STATE_IDLE, s.GetTeam().GetMatchState())
				require.Equal(t, ver0+2, s.GetTeam().GetVersion())
				require.Nil(t, s.GetTip(), "gather 结果不带 tip(§E.1 第 8 步)")
			}
			require.Empty(t, b.takeScenes(), "开战不改 TeamId,不发 scene 信号")
		})
	}
}

// ---- #18 / #30:锁自然过期后 EndMatch 不写;之后能再次开战 ----

func TestStartTeamMatchAgainAfterLockExpiry(t *testing.T) {
	b := newBattleHarness(t, 101, 102)
	// gather 途中 Redis 时钟越过锁截止:EndMatch 发现锁已自然过期 → 停止、不写,仍推一次当前视图。
	b.port.onGather = func(gatherRun) { setRedisMs(b.mr, baseMs()+testLockSeconds*1000) }

	requireCode(t, 0, b.start(101))

	require.NotEmpty(t, loadRecord(t, b.mr, b.teamId).GetMatchLockToken(), "锁已过期的记录不写(token 残留但按时钟无效)")
	verAfter := b.versionOf(b.teamId)
	ended := snapshotsByReason(t, b.takePushes(), teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_ENDED)
	require.ElementsMatch(t, []uint64{101, 102}, pushedTo(ended))
	for _, s := range ended {
		require.Equal(t, teampb.TeamMatchState_TEAM_MATCH_STATE_IDLE, s.GetTeam().GetMatchState(), "按 Redis 时钟锁已无效")
		require.Equal(t, verAfter, s.GetTeam().GetVersion(), "不经提交的视图版本不变")
	}
	require.Empty(t, b.sleeps, "停止分支不退避")

	b.port.onGather = nil
	resp := b.start(101)

	requireCode(t, 0, resp)
	require.Equal(t, teampb.TeamMatchState_TEAM_MATCH_STATE_STARTING, resp.GetTeam().GetMatchState())
	require.Len(t, b.port.gathers, 2, "锁过期后能再次开战")
	require.Empty(t, loadRecord(t, b.mr, b.teamId).GetMatchLockToken())
}

// ---- #18:并发两次开战,只有一次拿到锁 ----

func TestStartTeamMatchConcurrentOnlyOneLocks(t *testing.T) {
	b := newBattleHarness(t, 101, 102)
	var queue []func()
	asyncFn = func(_ string, fn func()) { queue = append(queue, fn) } // 后台任务排队,最后统一执行
	var inner *teampb.TeamResponse
	fired := false
	afterPreflightHook = func(uint64, []uint64) {
		if fired {
			return
		}
		fired = true
		inner = b.start(101) // 第二个请求在第一个预检之后、加锁之前完成加锁
	}

	outer := b.start(101)

	requireCode(t, 0, inner)
	require.Equal(t, teampb.TeamMatchState_TEAM_MATCH_STATE_STARTING, inner.GetTeam().GetMatchState())
	requireCode(t, ErrInMatch, outer)
	require.Equal(t, teampb.TeamMatchState_TEAM_MATCH_STATE_STARTING, outer.GetTeam().GetMatchState(), "重复开战视为进行中(§D.6)")
	require.Len(t, b.port.ticketCalls, 1, "只有一次拿到锁并建票")
	require.Empty(t, b.port.gathers, "gather 仍在后台队列")

	for len(queue) > 0 {
		fn := queue[0]
		queue = queue[1:]
		fn()
	}
	require.Len(t, b.port.gathers, 1)
	require.Empty(t, loadRecord(t, b.mr, b.teamId).GetMatchLockToken())
}

// ---- #24:迟到的 StartTeamMatch(expected=A)不给玩家的新队伍 B 开战 ----

func TestLateStartTeamMatchDoesNotStartNewTeam(t *testing.T) {
	b := newBattleHarness(t, 101, 102)
	oldTeam := b.teamId
	var newTeam uint64
	fired := false
	afterReadHook = func(bind Binding) {
		if fired || bind.PlayerId() != 101 {
			return
		}
		fired = true
		// 旧请求停在 S_READ 之后:玩家离开 A 并新建 B。
		requireCode(t, 0, b.ok(b.server.LeaveTeam(asPlayer(101), &teampb.LeaveTeamRequest{ExpectedTeamId: oldTeam})))
		newTeam = b.create(101)
	}
	verB := uint64(0)

	resp := b.start(101)

	requireCode(t, ErrNoTeam, resp)
	require.NotZero(t, newTeam)
	require.Equal(t, newTeam, resp.GetTeam().GetTeamId(), "回调用者当前(B)的视图")
	verB = b.versionOf(newTeam)
	require.Equal(t, uint64(1), verB, "B 只有建队那一次提交")
	require.Empty(t, loadRecord(t, b.mr, newTeam).GetMatchLockToken(), "不给 B 加锁")
	require.Empty(t, loadRecord(t, b.mr, oldTeam).GetMatchLockToken(), "A 的旧快照加锁被 ver CAS 挡住")
	require.Empty(t, b.port.ticketCalls)
}

// ---- #28:预检与加锁之间名单变化 → 整轮重来 ----

func TestStartTeamMatchRestartsRoundWhenRosterChanges(t *testing.T) {
	t.Run("队员离队", func(t *testing.T) {
		b := newBattleHarness(t, 101, 102, 103)
		fired := false
		afterPreflightHook = func(_ uint64, roster []uint64) {
			if fired {
				return
			}
			fired = true
			require.Equal(t, []uint64{101, 102, 103}, roster)
			requireCode(t, 0, b.ok(b.server.LeaveTeam(asPlayer(103), &teampb.LeaveTeamRequest{ExpectedTeamId: b.teamId})))
		}
		var lockRoster, members []uint64
		b.port.onGather = func(gatherRun) {
			rec := loadRecord(t, b.mr, b.teamId)
			lockRoster, members = rec.GetMatchLockRoster(), MemberIds(rec)
		}

		requireCode(t, 0, b.start(101))

		require.Equal(t, []uint64{101, 102, 103, 101, 102}, b.port.checks, "加锁 {0} 后整轮重来、重新预检")
		require.Len(t, b.port.ticketCalls, 1)
		require.Equal(t, []uint64{101, 102}, b.port.ticketCalls[0].roster, "不给已离队者建票")
		require.Equal(t, []uint64{101, 102}, lockRoster)
		require.ElementsMatch(t, members, lockRoster, "锁内 match_lock_roster == members")
	})

	t.Run("被邀请人接受邀请", func(t *testing.T) {
		b := newBattleHarness(t, 101, 102)
		b.addPlayers(testZone, 104)
		requireCode(t, 0, b.ok(b.server.InviteToTeam(asPlayer(101),
			&teampb.InviteToTeamRequest{TargetPlayerId: 104, ExpectedTeamId: b.teamId})))
		b.takePushes()
		fired := false
		afterPreflightHook = func(uint64, []uint64) {
			if fired {
				return
			}
			fired = true
			// 队外被邀请人自己接受,不经队长:同样让 ver+1。
			requireCode(t, 0, b.ok(b.server.RespondInvite(asPlayer(104),
				&teampb.RespondInviteRequest{TeamId: b.teamId, Accept: true})))
		}
		var lockRoster, members []uint64
		b.port.onGather = func(gatherRun) {
			rec := loadRecord(t, b.mr, b.teamId)
			lockRoster, members = rec.GetMatchLockRoster(), MemberIds(rec)
		}

		requireCode(t, 0, b.start(101))

		require.Equal(t, []uint64{101, 102, 101, 102, 104}, b.port.checks, "新加入者也经过预检")
		require.Len(t, b.port.ticketCalls, 1)
		require.Equal(t, []uint64{101, 102, 104}, b.port.ticketCalls[0].roster)
		require.Equal(t, []uint64{101, 102, 104}, lockRoster)
		require.ElementsMatch(t, members, lockRoster, "锁内 match_lock_roster == members")
	})
}

// ---- §E.3:开战锁提交结果未知 → 按 token 补偿,锁不白挂到自然过期 ----

// injectLockEval 在开战锁的 S_COMMIT 发出前插一次(只触发一次):landed=true 时先自己执行同一条 EVAL
// (第一次投递已落锁);随后返回 evalErr —— 非 nil = 回复丢失、调用方拿到错误;nil = go-redis 重发,真正的 EVAL 回 {0}。
func injectLockEval(b *battleHarness, landed bool, evalErr error) {
	b.t.Helper()
	fired := false
	prev := beforeCommitEvalHook
	b.t.Cleanup(func() { beforeCommitEvalHook = prev })
	beforeCommitEvalHook = func(d Decision, keys []string, args []any) error {
		if fired || d.Reason != teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_STARTED {
			return nil
		}
		fired = true
		if landed {
			_, err := b.service.store.rds.EvalCtx(context.Background(), scriptCommit, keys, args...)
			require.NoError(b.t, err)
		}
		return evalErr
	}
}

func TestStartTeamMatchSettlesUnconfirmedLock(t *testing.T) {
	errLost := errors.New("read tcp: i/o timeout")

	t.Run("报错但已落锁:后台按 token 清锁并推 MATCH_FAILED,队长可立即重试", func(t *testing.T) {
		b := newBattleHarness(t, 101, 102)
		ver0 := b.versionOf(b.teamId)
		injectLockEval(b, true, errLost)

		requireTip(t, b.start(101), ErrInternal, 0)

		require.Empty(t, loadRecord(t, b.mr, b.teamId).GetMatchLockToken(), "锁不白挂到自然过期")
		require.Equal(t, ver0+2, b.versionOf(b.teamId), "落锁一次 + 补偿清锁一次")
		require.Empty(t, b.port.ticketCalls)
		pushes := b.takePushes()
		require.Empty(t, snapshotsByReason(t, pushes, teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_STARTED))
		require.ElementsMatch(t, []uint64{101, 102},
			pushedTo(snapshotsByReason(t, pushes, teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_FAILED)))

		requireCode(t, 0, b.start(101))
		require.Len(t, b.port.gathers, 1, "锁已清,重试能开战")
	})

	t.Run("报错且未写入:补偿只读,不写不推", func(t *testing.T) {
		b := newBattleHarness(t, 101, 102)
		injectLockEval(b, false, errLost)
		before := b.mr.Dump()

		requireTip(t, b.start(101), ErrInternal, 0)

		require.Equal(t, before, b.mr.Dump())
		require.Empty(t, b.takePushes(), "锁没写入不推 MATCH_FAILED")
		require.Empty(t, b.sleeps, "token 不符即停止,不退避")
	})

	t.Run("同一条 EVAL 被重发(第二次回 {0}):重来前清掉自己的锁,本次请求照常开战", func(t *testing.T) {
		b := newBattleHarness(t, 101, 102)
		ver0 := b.versionOf(b.teamId)
		injectLockEval(b, true, nil)

		resp := b.start(101)

		requireCode(t, 0, resp)
		require.Equal(t, teampb.TeamMatchState_TEAM_MATCH_STATE_STARTING, resp.GetTeam().GetMatchState())
		require.Equal(t, []uint64{101, 102, 101, 102}, b.port.checks, "整轮重来、重新预检,没被自己的锁挡成 TeamInMatch")
		require.Len(t, b.port.ticketCalls, 1)
		require.Len(t, b.port.gathers, 1)
		require.Empty(t, loadRecord(t, b.mr, b.teamId).GetMatchLockToken())
		require.Equal(t, ver0+4, b.versionOf(b.teamId), "重发落的锁 + 确认清锁 + 本轮加锁 + EndMatch")
		pushes := b.takePushes()
		require.ElementsMatch(t, []uint64{101, 102},
			pushedTo(snapshotsByReason(t, pushes, teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_FAILED)))
		require.ElementsMatch(t, []uint64{101, 102},
			pushedTo(snapshotsByReason(t, pushes, teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_ENDED)))
	})
}

// ---- #30:EndMatch 连续冲突最终清锁;token 不符 / 锁过期 / 记录缺失停止且不写 ----

func TestEndMatchRetriesThroughConflicts(t *testing.T) {
	b := newBattleHarness(t, 101, 102)
	outsiders := []uint64{201, 202, 203, 204, 205, 206}
	b.addPlayers(testZone, outsiders...)
	injected := 0
	afterMatchReadHook = func(uint64) {
		if injected >= len(outsiders) {
			return
		}
		pid := outsiders[injected]
		injected++
		// 锁期间队外玩家申请照常允许(§E.4),每次让 ver+1,制造 EndMatch 的版本冲突。
		requireCode(t, 0, b.apply(pid, 101))
	}

	requireCode(t, 0, b.start(101))

	require.Equal(t, len(outsiders), injected)
	rec := loadRecord(t, b.mr, b.teamId)
	require.Empty(t, rec.GetMatchLockToken(), "连续 %d 次冲突后最终清锁", len(outsiders))
	require.Len(t, rec.GetApplications(), len(outsiders), "锁期间的申请都保留")
	require.Len(t, b.sleeps, len(outsiders), "每次冲突退避一次")
	for i, d := range b.sleeps {
		base := endMatchBackoffInitial << i
		if base > endMatchBackoffMax {
			base = endMatchBackoffMax
		}
		require.GreaterOrEqual(t, d, time.Duration(float64(base)*(1-endMatchBackoffJitter)), "第 %d 次退避", i)
		require.LessOrEqual(t, d, time.Duration(float64(base)*(1+endMatchBackoffJitter)), "第 %d 次退避", i)
	}
	ended := snapshotsByReason(t, b.takePushes(), teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_ENDED)
	require.ElementsMatch(t, []uint64{101, 102}, pushedTo(ended))
}

func TestEndMatchStopsWithoutWriting(t *testing.T) {
	b := newBattleHarness(t, 101, 102)
	store := b.service.store
	snap, _, err := store.ReadFree(context.Background(), 101)
	require.NoError(t, err)
	roster := MatchRoster(snap.Record)
	lock, err := store.CommitMatchLock(context.Background(), snap, 101, "tok-A", roster, snap.NowMs+testLockSeconds*1000)
	require.NoError(t, err)
	require.NotNil(t, lock.Commit)

	// token 不符(锁已被清或重新加锁):停止、不写,仍给队员推一次当前视图(锁仍有效 → STARTING)。
	before := b.mr.Dump()
	b.service.finishMatch(b.teamId, "tok-B", true, roster, nil)
	require.Equal(t, before, b.mr.Dump())
	current := snapshotsByReason(t, b.takePushes(), teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_ENDED)
	require.ElementsMatch(t, roster, pushedTo(current))
	for _, s := range current {
		require.Equal(t, teampb.TeamMatchState_TEAM_MATCH_STATE_STARTING, s.GetTeam().GetMatchState())
		require.Equal(t, lock.Commit.Version, s.GetTeam().GetVersion())
	}
	require.Equal(t, EndMatchTokenMismatch, store.EndMatch(b.teamId, "tok-B", true).Stop)

	// 锁已按 Redis 时钟过期:停止、不写。
	setRedisMs(b.mr, snap.NowMs+testLockSeconds*1000)
	before = b.mr.Dump()
	res := store.EndMatch(b.teamId, "tok-A", false)
	require.Equal(t, EndMatchLockExpired, res.Stop)
	require.Nil(t, res.Commit)
	require.Equal(t, before, b.mr.Dump())

	// 记录不存在:停止、不写。
	res = store.EndMatch(99_999, "tok-A", true)
	require.Equal(t, EndMatchRecordMissing, res.Stop)
	require.Equal(t, before, b.mr.Dump())
	require.Empty(t, b.sleeps, "停止分支不退避")
}
