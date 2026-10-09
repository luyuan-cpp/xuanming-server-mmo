package data

// trial_lobby_repo_test.go —— 同道历练在 Redis 一侧的两个仓库:邀请房间(trial_lobby_repo.go,06 §6.40 的 8 条)与
// 活动对局结果记录的读 / 销账(trial_result_record.go)。
//
// 不带 build tag、不连 MySQL:Lua 脚本在 miniredis 里真跑(它内嵌了 Lua 解释器),普通 `go test ./...` 必跑。
// 时间有两个来源,测试里分别控制:脚本里的"现在"由调用方传入(这里一律从 lobbyNowMs 派生,不读墙钟);
// 键的 TTL 由 miniredis 的时钟管,只有 FastForward 才会前进。
//
// 结果记录的测试放在本文件末尾:B6b-srv2 的文件清单没有给 trial_result_record.go 单列测试文件,
// 它与房间同属"历练的 Redis 侧"、共用同一个 miniredis 夹具。

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "proto/guild"

	"guild/internal/constants"
)

const (
	lobbyNowMs      uint64 = 1_700_000_000_000
	lobbyTTL               = 30 * time.Second
	lobbyCooldown          = 10 * time.Second
	lobbyLaunchWait        = 5 * time.Second

	lobbyGuildID  uint64 = 7001
	lobbyActivity uint32 = 3
)

// 玩家 id 取 2^53 以上:脚本若把 id 经过 Lua 的 number(double),这些值会被悄悄舍入,名单匹配就错了。
const (
	lobbyPlayerA uint64 = 9_007_199_254_740_993 // 2^53 + 1
	lobbyPlayerB uint64 = 9_007_199_254_740_995
	lobbyPlayerC uint64 = 9_007_199_254_740_997
	lobbyPlayerD uint64 = 9_007_199_254_740_999
)

type lobbyFixture struct {
	ctx  context.Context
	mr   *miniredis.Miniredis
	repo *TrialLobbyRepo
}

func openLobbyFixture(t *testing.T) lobbyFixture {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	repo, err := NewTrialLobbyRepo(rdb)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return lobbyFixture{ctx: ctx, mr: mr, repo: repo}
}

func lobbyCreateInput(nowMs uint64, roster ...uint64) TrialLobbyCreate {
	return TrialLobbyCreate{
		GuildID: lobbyGuildID, ActivityID: lobbyActivity, Roster: roster,
		TTL: lobbyTTL, Cooldown: lobbyCooldown, NowMs: nowMs,
	}
}

// mustCreateLobby 建一个房间并要求成功。
func mustCreateLobby(t *testing.T, f lobbyFixture, nowMs uint64, roster ...uint64) *TrialLobby {
	t.Helper()
	res, err := f.repo.Create(f.ctx, lobbyCreateInput(nowMs, roster...))
	require.NoError(t, err)
	require.Equal(t, TrialLobbyCreated, res.Status)
	require.NotNil(t, res.Lobby)
	return res.Lobby
}

func (f lobbyFixture) respond(t *testing.T, lobbyID, playerID uint64, accept bool, nowMs uint64) TrialLobbyRespondResult {
	t.Helper()
	res, err := f.repo.Respond(f.ctx, lobbyID, playerID, accept, nowMs, lobbyLaunchWait)
	require.NoError(t, err)
	return res
}

func (f lobbyFixture) load(t *testing.T, lobbyID uint64) *TrialLobby {
	t.Helper()
	lobby, err := f.repo.Load(f.ctx, lobbyID)
	require.NoError(t, err)
	require.NotNil(t, lobby, "房间 %d 应当存在", lobbyID)
	return lobby
}

func TestNewTrialLobbyRepoRejectsNilRedis(t *testing.T) {
	_, err := NewTrialLobbyRepo(nil)
	assert.Error(t, err)
}

// TestTrialLobbyCreate_WritesRoomPointersAndCooldown(06 §6.40 第 1 条):建房后 hash 字段、每人一把 of 键、冷却键的 TTL 都对;
// 同一发起人在冷却内再建 → 冷却中、剩余时间 > 0,且第二次什么都没写。
func TestTrialLobbyCreate_WritesRoomPointersAndCooldown(t *testing.T) {
	f := openLobbyFixture(t)
	created := mustCreateLobby(t, f, lobbyNowMs, lobbyPlayerA, lobbyPlayerB, lobbyPlayerC)

	want := TrialLobby{
		LobbyID: created.LobbyID, GuildID: lobbyGuildID, InitiatorID: lobbyPlayerA, ActivityID: lobbyActivity,
		Roster: []uint64{lobbyPlayerA, lobbyPlayerB, lobbyPlayerC}, AcceptedMask: 1,
		State:      pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_PENDING,
		ExpireAtMs: lobbyNowMs + uint64(lobbyTTL.Milliseconds()), CreatedMs: lobbyNowMs,
	}
	assert.Equal(t, want, *created, "Create 返回的初始状态")
	assert.Equal(t, want, *f.load(t, created.LobbyID), "库里读回的房间")
	assert.Equal(t, []uint64{lobbyPlayerA}, created.Accepted(), "发起人建房即同意")

	keyTTL := lobbyTTL + trialLobbyKeyGrace
	assert.Equal(t, keyTTL, f.mr.TTL(trialLobbyKey(created.LobbyID)), "房间键 TTL = 邀请有效期 + 60s")
	for _, playerID := range want.Roster {
		pointer, err := f.mr.Get(trialLobbyOfKey(playerID))
		require.NoError(t, err, "玩家 %d 的 of 键", playerID)
		assert.Equal(t, strconv.FormatUint(created.LobbyID, 10), pointer)
		assert.Equal(t, keyTTL, f.mr.TTL(trialLobbyOfKey(playerID)))
	}
	assert.Equal(t, lobbyCooldown, f.mr.TTL(trialCooldownKey(lobbyPlayerA)), "发起人冷却")
	assert.False(t, f.mr.Exists(trialCooldownKey(lobbyPlayerB)), "被邀请人不进冷却")

	// 冷却内再建(换一批被邀请人,排除"有人在别的房间"的干扰)。
	again, err := f.repo.Create(f.ctx, lobbyCreateInput(lobbyNowMs+1_000, lobbyPlayerA, lobbyPlayerD))
	require.NoError(t, err)
	assert.Equal(t, TrialLobbyCooldown, again.Status)
	assert.Positive(t, again.CooldownRemaining)
	assert.LessOrEqual(t, again.CooldownRemaining, lobbyCooldown)
	assert.Nil(t, again.Lobby)
	assert.False(t, f.mr.Exists(trialLobbyOfKey(lobbyPlayerD)), "被冷却挡下的建房不写任何键")
}

// TestTrialLobbyCreate_NoCooldownWhenZero:冷却配 0 = 不限,不写冷却键。
func TestTrialLobbyCreate_NoCooldownWhenZero(t *testing.T) {
	f := openLobbyFixture(t)
	in := lobbyCreateInput(lobbyNowMs, lobbyPlayerA, lobbyPlayerB)
	in.Cooldown = 0
	res, err := f.repo.Create(f.ctx, in)
	require.NoError(t, err)
	require.Equal(t, TrialLobbyCreated, res.Status)
	assert.False(t, f.mr.Exists(trialCooldownKey(lobbyPlayerA)))
}

// TestTrialLobbyCreate_MemberBusy(第 2 条):被邀请人还在另一个未过期的 PENDING 房间里 → 报出这个人,不建房、不消耗冷却;
// 那个房间过期(按传入的 now 判,不靠键 TTL)或已结束之后就可以建。
func TestTrialLobbyCreate_MemberBusy(t *testing.T) {
	f := openLobbyFixture(t)
	first := mustCreateLobby(t, f, lobbyNowMs, lobbyPlayerA, lobbyPlayerB)

	// C 邀请 D 与 B:B 还在 A 的房间里。名单下标 2 的人被报出来。
	busy, err := f.repo.Create(f.ctx, lobbyCreateInput(lobbyNowMs+1_000, lobbyPlayerC, lobbyPlayerD, lobbyPlayerB))
	require.NoError(t, err)
	assert.Equal(t, TrialLobbyMemberBusy, busy.Status)
	assert.Equal(t, lobbyPlayerB, busy.BusyPlayerID)
	assert.False(t, f.mr.Exists(trialCooldownKey(lobbyPlayerC)), "名单不合法不消耗冷却")
	assert.False(t, f.mr.Exists(trialLobbyOfKey(lobbyPlayerD)))

	// 时钟推过 A 房间的截止时刻:房间键还在(TTL 多留 60s),但按 now 判已失效。
	afterExpiry := first.ExpireAtMs
	second := mustCreateLobby(t, f, afterExpiry, lobbyPlayerC, lobbyPlayerD, lobbyPlayerB)
	pointer, err := f.mr.Get(trialLobbyOfKey(lobbyPlayerB))
	require.NoError(t, err)
	assert.Equal(t, strconv.FormatUint(second.LobbyID, 10), pointer, "B 的 of 键改指新房间")

	// 已结束的房间同样不占人:D 拒绝之后,A(冷却已过)可以再邀请 D。
	require.Equal(t, TrialLobbyRespondDeclined, f.respond(t, second.LobbyID, lobbyPlayerD, false, afterExpiry+1_000).Status)
	f.mr.FastForward(lobbyCooldown + time.Second)
	mustCreateLobby(t, f, afterExpiry+2_000, lobbyPlayerA, lobbyPlayerD)
}

// TestTrialLobbyCreate_LaunchingRoomHoldsMembersUntilDeadline:LAUNCHING 的房间在开战截止之前仍占着人,过了截止才放。
func TestTrialLobbyCreate_LaunchingRoomHoldsMembersUntilDeadline(t *testing.T) {
	f := openLobbyFixture(t)
	first := mustCreateLobby(t, f, lobbyNowMs, lobbyPlayerA, lobbyPlayerB)
	require.Equal(t, TrialLobbyRespondLaunch, f.respond(t, first.LobbyID, lobbyPlayerB, true, lobbyNowMs+1_000).Status)
	deadline := lobbyNowMs + 1_000 + uint64(lobbyLaunchWait.Milliseconds())

	busy, err := f.repo.Create(f.ctx, lobbyCreateInput(deadline-1, lobbyPlayerC, lobbyPlayerB))
	require.NoError(t, err)
	assert.Equal(t, TrialLobbyMemberBusy, busy.Status)
	assert.Equal(t, lobbyPlayerB, busy.BusyPlayerID)

	mustCreateLobby(t, f, deadline, lobbyPlayerC, lobbyPlayerB)
}

// TestTrialLobbyRespond_AcceptInOrder(第 3 条):B 同意 → 等其他人;C 同意 → 凑齐,本次调用负责开战,
// 房间进 LAUNCHING、开战截止 = now + 窗口;C 再点一次 → 已不在等待确认、本人已同意。
func TestTrialLobbyRespond_AcceptInOrder(t *testing.T) {
	f := openLobbyFixture(t)
	lobby := mustCreateLobby(t, f, lobbyNowMs, lobbyPlayerA, lobbyPlayerB, lobbyPlayerC)

	assert.Equal(t, TrialLobbyRespondResult{Status: TrialLobbyRespondAccepted}, f.respond(t, lobby.LobbyID, lobbyPlayerB, true, lobbyNowMs+1_000))
	assert.Equal(t, []uint64{lobbyPlayerA, lobbyPlayerB}, f.load(t, lobby.LobbyID).Accepted())
	// 重复同意:还在 PENDING,不改变任何东西。
	assert.Equal(t, TrialLobbyRespondResult{Status: TrialLobbyRespondAccepted}, f.respond(t, lobby.LobbyID, lobbyPlayerB, true, lobbyNowMs+1_500))

	launchAt := lobbyNowMs + 2_000
	assert.Equal(t, TrialLobbyRespondResult{Status: TrialLobbyRespondLaunch}, f.respond(t, lobby.LobbyID, lobbyPlayerC, true, launchAt))
	got := f.load(t, lobby.LobbyID)
	assert.Equal(t, pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_LAUNCHING, got.State)
	assert.Equal(t, launchAt+uint64(lobbyLaunchWait.Milliseconds()), got.LaunchDeadlineMs)
	assert.Equal(t, []uint64{lobbyPlayerA, lobbyPlayerB, lobbyPlayerC}, got.Accepted())

	assert.Equal(t, TrialLobbyRespondResult{Status: TrialLobbyRespondNotPending, AlreadyAccepted: true},
		f.respond(t, lobby.LobbyID, lobbyPlayerC, true, launchAt+100), "拿到开战权的只有一次调用")
}

// TestTrialLobbyRespond_Decline(第 4 条):B 拒绝 → 房间结束,原因是"有人拒绝"、参数是 B;之后 C 再同意 →
// 已不在等待确认,且 C 此前并未同意。发起人拒绝走同一条路(即取消)。
func TestTrialLobbyRespond_Decline(t *testing.T) {
	f := openLobbyFixture(t)
	lobby := mustCreateLobby(t, f, lobbyNowMs, lobbyPlayerA, lobbyPlayerB, lobbyPlayerC)

	assert.Equal(t, TrialLobbyRespondResult{Status: TrialLobbyRespondDeclined}, f.respond(t, lobby.LobbyID, lobbyPlayerB, false, lobbyNowMs+1_000))
	got := f.load(t, lobby.LobbyID)
	assert.Equal(t, pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_ENDED, got.State)
	assert.Equal(t, constants.ErrTrialInviteDeclined, got.EndTipID)
	assert.Equal(t, []string{strconv.FormatUint(lobbyPlayerB, 10)}, got.EndParams)

	assert.Equal(t, TrialLobbyRespondResult{Status: TrialLobbyRespondNotPending, AlreadyAccepted: false},
		f.respond(t, lobby.LobbyID, lobbyPlayerC, true, lobbyNowMs+2_000))
	assert.Equal(t, TrialLobbyRespondResult{Status: TrialLobbyRespondNotPending, AlreadyAccepted: true},
		f.respond(t, lobby.LobbyID, lobbyPlayerA, true, lobbyNowMs+2_000), "发起人建房时已同意")

	// 发起人取消。
	f.mr.FastForward(lobbyCooldown + time.Second)
	cancelled := mustCreateLobby(t, f, lobbyNowMs+3_000, lobbyPlayerA, lobbyPlayerD)
	assert.Equal(t, TrialLobbyRespondDeclined, f.respond(t, cancelled.LobbyID, lobbyPlayerA, false, lobbyNowMs+4_000).Status)
	assert.Equal(t, []string{strconv.FormatUint(lobbyPlayerA, 10)}, f.load(t, cancelled.LobbyID).EndParams)
}

// TestTrialLobbyRespond_Gone(第 5 条):邀请已过期(now ≥ exp,恰好等于也算过期)、调用者不在名单、房间不存在 →
// 都归为"邀请已失效";过期的那一次不改库里的状态(过期只在读的时候折算)。
func TestTrialLobbyRespond_Gone(t *testing.T) {
	f := openLobbyFixture(t)
	lobby := mustCreateLobby(t, f, lobbyNowMs, lobbyPlayerA, lobbyPlayerB)

	assert.Equal(t, TrialLobbyRespondResult{Status: TrialLobbyRespondGone}, f.respond(t, lobby.LobbyID, lobbyPlayerD, true, lobbyNowMs+1_000), "不在名单")
	assert.Equal(t, TrialLobbyRespondResult{Status: TrialLobbyRespondGone}, f.respond(t, lobby.LobbyID+1_000, lobbyPlayerB, true, lobbyNowMs+1_000), "房间不存在")
	assert.Equal(t, TrialLobbyRespondResult{Status: TrialLobbyRespondGone}, f.respond(t, lobby.LobbyID, lobbyPlayerB, true, lobby.ExpireAtMs), "恰好到截止时刻")
	assert.Equal(t, TrialLobbyRespondResult{Status: TrialLobbyRespondGone}, f.respond(t, lobby.LobbyID, lobbyPlayerB, false, lobby.ExpireAtMs+1), "过期后拒绝也无效")

	got := f.load(t, lobby.LobbyID)
	assert.Equal(t, pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_PENDING, got.State)
	assert.Equal(t, []uint64{lobbyPlayerA}, got.Accepted())

	// 截止前一毫秒仍然有效。
	assert.Equal(t, TrialLobbyRespondLaunch, f.respond(t, lobby.LobbyID, lobbyPlayerB, true, lobby.ExpireAtMs-1).Status)
}

// TestTrialLobbyLoadForPlayer_FoldsTimeouts(第 6 条):PENDING 过期折算成"已结束 + 邀请超时";
// LAUNCHING 超过开战截止折算成"已结束 + 服务繁忙";未到时刻的原样返回。折算不写回库。
func TestTrialLobbyLoadForPlayer_FoldsTimeouts(t *testing.T) {
	f := openLobbyFixture(t)
	lobby := mustCreateLobby(t, f, lobbyNowMs, lobbyPlayerA, lobbyPlayerB)

	live, err := f.repo.LoadForPlayer(f.ctx, lobbyPlayerB, lobby.ExpireAtMs-1)
	require.NoError(t, err)
	require.NotNil(t, live)
	assert.Equal(t, pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_PENDING, live.State)
	assert.Equal(t, lobby.LobbyID, live.LobbyID)

	expired, err := f.repo.LoadForPlayer(f.ctx, lobbyPlayerB, lobby.ExpireAtMs)
	require.NoError(t, err)
	require.NotNil(t, expired)
	assert.Equal(t, pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_ENDED, expired.State)
	assert.Equal(t, constants.ErrTrialInviteExpired, expired.EndTipID)
	assert.Equal(t, pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_PENDING, f.load(t, lobby.LobbyID).State, "折算不写回")

	launchAt := lobbyNowMs + 1_000
	require.Equal(t, TrialLobbyRespondLaunch, f.respond(t, lobby.LobbyID, lobbyPlayerB, true, launchAt).Status)
	deadline := launchAt + uint64(lobbyLaunchWait.Milliseconds())

	launching, err := f.repo.LoadForPlayer(f.ctx, lobbyPlayerA, deadline-1)
	require.NoError(t, err)
	require.NotNil(t, launching)
	assert.Equal(t, pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_LAUNCHING, launching.State)

	stuck, err := f.repo.LoadForPlayer(f.ctx, lobbyPlayerA, deadline)
	require.NoError(t, err)
	require.NotNil(t, stuck)
	assert.Equal(t, pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_ENDED, stuck.State)
	assert.Equal(t, constants.ErrTrialServiceBusy, stuck.EndTipID)

	// 没进过任何房间的人、键已过期的人:没有房间。
	none, err := f.repo.LoadForPlayer(f.ctx, lobbyPlayerD, lobbyNowMs)
	require.NoError(t, err)
	assert.Nil(t, none)
	f.mr.FastForward(lobbyTTL + trialLobbyKeyGrace + time.Second)
	gone, err := f.repo.LoadForPlayer(f.ctx, lobbyPlayerA, lobbyNowMs)
	require.NoError(t, err)
	assert.Nil(t, gone)
	missing, err := f.repo.Load(f.ctx, lobby.LobbyID)
	require.NoError(t, err)
	assert.Nil(t, missing)
}

// TestTrialLobbyFinish(第 7 条):只有当前状态等于 from 才迁移。开战成功写 battle_id;开战失败写原因与参数;
// 开战超过窗口后对局仍开成了,房间照样写成 LAUNCHED(只比状态,不看截止时刻)。
func TestTrialLobbyFinish(t *testing.T) {
	f := openLobbyFixture(t)
	const battleID uint64 = 9_007_199_254_741_111 // 同样取 2^53 以上
	pending, launching, launched, ended := pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_PENDING,
		pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_LAUNCHING,
		pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_LAUNCHED,
		pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_ENDED

	lobby := mustCreateLobby(t, f, lobbyNowMs, lobbyPlayerA, lobbyPlayerB)
	moved, err := f.repo.Finish(f.ctx, lobby.LobbyID, launching, launched, battleID, 0, nil)
	require.NoError(t, err)
	assert.False(t, moved, "还在 PENDING,不是 LAUNCHING")
	assert.Equal(t, pending, f.load(t, lobby.LobbyID).State)

	require.Equal(t, TrialLobbyRespondLaunch, f.respond(t, lobby.LobbyID, lobbyPlayerB, true, lobbyNowMs+1_000).Status)
	moved, err = f.repo.Finish(f.ctx, lobby.LobbyID, launching, launched, battleID, 0, nil)
	require.NoError(t, err)
	assert.True(t, moved)
	got := f.load(t, lobby.LobbyID)
	assert.Equal(t, launched, got.State)
	assert.Equal(t, battleID, got.BattleID)
	// 过了开战截止再读:LAUNCHED 不参与折算。
	assert.Equal(t, launched, got.EffectiveAt(got.LaunchDeadlineMs+60_000).State)

	moved, err = f.repo.Finish(f.ctx, lobby.LobbyID, launching, ended, 0, constants.ErrTrialServiceBusy, nil)
	require.NoError(t, err)
	assert.False(t, moved, "已 LAUNCHED,不能再迁成失败")

	// 开战失败:原因与参数原样存取。
	f.mr.FastForward(lobbyCooldown + time.Second)
	failed := mustCreateLobby(t, f, lobbyNowMs+2_000, lobbyPlayerC, lobbyPlayerD)
	require.Equal(t, TrialLobbyRespondLaunch, f.respond(t, failed.LobbyID, lobbyPlayerD, true, lobbyNowMs+3_000).Status)
	params := []string{"offline", strconv.FormatUint(lobbyPlayerD, 10)}
	moved, err = f.repo.Finish(f.ctx, failed.LobbyID, launching, ended, 0, constants.ErrTrialTeamInvalid, params)
	require.NoError(t, err)
	assert.True(t, moved)
	got = f.load(t, failed.LobbyID)
	assert.Equal(t, ended, got.State)
	assert.Equal(t, constants.ErrTrialTeamInvalid, got.EndTipID)
	assert.Equal(t, params, got.EndParams)

	moved, err = f.repo.Finish(f.ctx, failed.LobbyID+1_000, launching, ended, 0, 0, nil)
	require.NoError(t, err)
	assert.False(t, moved, "房间不存在")
	_, err = f.repo.Finish(f.ctx, failed.LobbyID, ended, ended, 0, 0, []string{"bad" + trialLobbyParamSep + "param"})
	assert.Error(t, err, "参数里带分隔符存进去就拆不回来,拒绝")
}

// TestTrialLobbyCreate_SeqCollision(第 8 条):Redis 被清空后序号回卷、撞上还活着的房间键 → 自动换下一个号;
// 连撞两次才报错,且不覆盖那两个房间。
func TestTrialLobbyCreate_SeqCollision(t *testing.T) {
	f := openLobbyFixture(t)
	// 预置"下一个号"对应的房间键(模拟 seq 被清、房间键还在)。
	f.mr.HSet(trialLobbyKey(1), trialLobbyFieldState, "3")
	lobby := mustCreateLobby(t, f, lobbyNowMs, lobbyPlayerA, lobbyPlayerB)
	assert.Equal(t, uint64(2), lobby.LobbyID, "撞号后换下一个号")
	assert.Equal(t, "3", f.mr.HGet(trialLobbyKey(1), trialLobbyFieldState), "撞到的房间原样不动")

	f.mr.HSet(trialLobbyKey(3), trialLobbyFieldState, "3")
	f.mr.HSet(trialLobbyKey(4), trialLobbyFieldState, "3")
	_, err := f.repo.Create(f.ctx, lobbyCreateInput(lobbyNowMs, lobbyPlayerC, lobbyPlayerD))
	assert.Error(t, err, "连撞两次 = Redis 状态异常,按故障上抛")
	assert.False(t, f.mr.Exists(trialLobbyOfKey(lobbyPlayerC)))
	assert.False(t, f.mr.Exists(trialCooldownKey(lobbyPlayerC)))
}

// TestTrialLobbyCreate_RejectsMalformedInput:结构不合法的输入在碰 Redis 之前拒绝(调用方的编程错误)。
func TestTrialLobbyCreate_RejectsMalformedInput(t *testing.T) {
	f := openLobbyFixture(t)
	breakers := map[string]func(*TrialLobbyCreate){
		"只有发起人":     func(in *TrialLobbyCreate) { in.Roster = []uint64{lobbyPlayerA} },
		"名单含 0":     func(in *TrialLobbyCreate) { in.Roster = []uint64{lobbyPlayerA, 0} },
		"名单有重复":     func(in *TrialLobbyCreate) { in.Roster = []uint64{lobbyPlayerA, lobbyPlayerB, lobbyPlayerB} },
		"名单超上限":     func(in *TrialLobbyCreate) { in.Roster = make([]uint64, maxTrialLobbyRoster+1) },
		"guild 为 0": func(in *TrialLobbyCreate) { in.GuildID = 0 },
		"活动为 0":     func(in *TrialLobbyCreate) { in.ActivityID = 0 },
		"now 为 0":   func(in *TrialLobbyCreate) { in.NowMs = 0 },
		"有效期不为正":    func(in *TrialLobbyCreate) { in.TTL = 0 },
		"冷却为负":      func(in *TrialLobbyCreate) { in.Cooldown = -time.Second },
	}
	for name, breakIt := range breakers {
		in := lobbyCreateInput(lobbyNowMs, lobbyPlayerA, lobbyPlayerB)
		breakIt(&in)
		_, err := f.repo.Create(f.ctx, in)
		assert.Error(t, err, name)
	}
	assert.Empty(t, f.mr.Keys(), "被拒的输入不发号、不写任何键")

	_, err := f.repo.Respond(f.ctx, 1, lobbyPlayerA, true, lobbyNowMs, 0)
	assert.Error(t, err, "开战窗口必须为正")
}

// TestTrialLobbyLoad_CorruptRoomIsAnError:房间键在、字段却不全 → 报错,而不是当成"没有房间"。
func TestTrialLobbyLoad_CorruptRoomIsAnError(t *testing.T) {
	f := openLobbyFixture(t)
	f.mr.HSet(trialLobbyKey(77), trialLobbyFieldState, "1")
	_, err := f.repo.Load(f.ctx, 77)
	assert.Error(t, err)
}

// TestTrialLobbyLoadForPlayer_IgnoresReusedRoomNumber:of 键指向的房间号被别人的房间复用(Redis 清空后回卷),
// 而这个人不在那个房间的名单里 → 没有房间,不把别人的邀请展示给他。
func TestTrialLobbyLoadForPlayer_IgnoresReusedRoomNumber(t *testing.T) {
	f := openLobbyFixture(t)
	lobby := mustCreateLobby(t, f, lobbyNowMs, lobbyPlayerA, lobbyPlayerB)
	require.NoError(t, f.mr.Set(trialLobbyOfKey(lobbyPlayerD), strconv.FormatUint(lobby.LobbyID, 10)))
	got, err := f.repo.LoadForPlayer(f.ctx, lobbyPlayerD, lobbyNowMs)
	require.NoError(t, err)
	assert.Nil(t, got)
}

// TestTrialSweepLease:租约被别的实例持有时抢不到;到期后可以再抢。
func TestTrialSweepLease(t *testing.T) {
	f := openLobbyFixture(t)
	const ttl = 55 * time.Second

	got, err := f.repo.AcquireSweepLease(f.ctx, "guild-a", ttl)
	require.NoError(t, err)
	assert.True(t, got)
	got, err = f.repo.AcquireSweepLease(f.ctx, "guild-b", ttl)
	require.NoError(t, err)
	assert.False(t, got, "别的实例持有期间抢不到")
	holder, err := f.mr.Get(trialSweepLeaseKey)
	require.NoError(t, err)
	assert.Equal(t, "guild-a", holder)

	f.mr.FastForward(ttl + time.Second)
	got, err = f.repo.AcquireSweepLease(f.ctx, "guild-b", ttl)
	require.NoError(t, err)
	assert.True(t, got)

	_, err = f.repo.AcquireSweepLease(f.ctx, "", ttl)
	assert.Error(t, err)
}

// ── 活动对局结果记录(trial_result_record.go)──────────────────

// TestActivityResultKeyMatchesBattleNode 钉住跨语言键契约:与 C++ battle_result_activity.h 的
// kActivityResultKeyPrefix + 十进制 battle_id 逐字一致。改这里之前先改 C++ 那一侧。
func TestActivityResultKeyMatchesBattleNode(t *testing.T) {
	assert.Equal(t, "battle:activity_result:", activityResultKeyPrefix)
	assert.Equal(t, "battle:activity_result:1", activityResultKey(1))
	assert.Equal(t, "battle:activity_result:18446744073709551615", activityResultKey(^uint64(0)))
}

// TestTrialResultRecords_LoadAndAck:读到 battle 写下的原始字节;销账后读不到;销账幂等;不误删别的局。
func TestTrialResultRecords_LoadAndAck(t *testing.T) {
	_, err := NewTrialResultRecords(nil)
	assert.Error(t, err)

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	records, err := NewTrialResultRecords(rdb)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	const (
		battleID uint64 = 9_007_199_254_741_001
		otherID  uint64 = 9_007_199_254_741_002
	)
	// 值是 protobuf 字节,不是文本:带 0x00 与非 UTF-8 字节,原样读回。
	payload := string([]byte{0x08, 0x00, 0xff, 0x10, 0x01})
	require.NoError(t, mr.Set(activityResultKey(battleID), payload))
	require.NoError(t, mr.Set(activityResultKey(otherID), "other"))

	got, found, err := records.Load(ctx, battleID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, []byte(payload), got)

	require.NoError(t, records.Ack(ctx, battleID))
	_, found, err = records.Load(ctx, battleID)
	require.NoError(t, err)
	assert.False(t, found, "销账后记录不在")
	require.NoError(t, records.Ack(ctx, battleID), "重复销账不算错")
	assert.True(t, mr.Exists(activityResultKey(otherID)), "只删这一局的键")

	_, _, err = records.Load(ctx, 0)
	assert.Error(t, err)
	assert.Error(t, records.Ack(ctx, 0))

	// Redis 故障:读与销账都返回错误,不把"读不到"伪装成"没有记录"。
	mr.Close()
	_, found, err = records.Load(ctx, otherID)
	assert.Error(t, err)
	assert.False(t, found)
	assert.Error(t, records.Ack(ctx, otherID))
}
