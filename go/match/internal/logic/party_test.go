package logic

// 队伍补位匹配(设计文档 docs/design/team-backfill-matching.md)的 miniredis 集成测试:
// 整单入队与回滚 / 按单位成组(队伍不可拆、人数恰好凑满、队首优先但不堵队)/ 弹组前向 team 复核 /
// 不完整的单整单撤出 / 队员取消 = 整单退出 / 取消与弹组并发 / 开局失败回队首的完整性规则 / 票据自愈。
// team 域用假端口 fakePartyPort;gather 用 runGatherFn 记录器(同 pve_team_size_test.go)。
// 测试之间不并行(包级测试缝)。

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"

	"match/internal/constants"
	"match/internal/svc"

	matchpb "proto/match"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
)

type partyEndedCall struct {
	teamId  uint64
	members []uint64
	reason  uint32
	culprit uint64
}

type partyVerdict struct {
	reason  uint32
	culprit uint64
	err     error
}

// fakePartyPort 假的 svc.PartyTeamPort:按 team_id 给复核结论(没配 = 有效),记录每次复核与撤单通知。
type fakePartyPort struct {
	mu       sync.Mutex
	verdicts map[uint64]partyVerdict
	checks   []partyEndedCall // 只用 teamId / members 两项
	ended    []partyEndedCall
	started  []partyEndedCall // 只用 teamId / members 两项
}

var _ svc.PartyTeamPort = (*fakePartyPort)(nil)

func (f *fakePartyPort) CheckParty(_ context.Context, teamId uint64, members []uint64) (uint32, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks = append(f.checks, partyEndedCall{teamId: teamId, members: append([]uint64(nil), members...)})
	v := f.verdicts[teamId]
	return v.reason, v.culprit, v.err
}

func (f *fakePartyPort) PartyEnded(teamId uint64, members []uint64, reason uint32, culprit uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ended = append(f.ended, partyEndedCall{teamId: teamId, members: append([]uint64(nil), members...), reason: reason, culprit: culprit})
}

func (f *fakePartyPort) PartyStarted(teamId uint64, members []uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, partyEndedCall{teamId: teamId, members: append([]uint64(nil), members...)})
}

func (f *fakePartyPort) startedCalls() []partyEndedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]partyEndedCall(nil), f.started...)
}

func (f *fakePartyPort) setVerdict(teamId uint64, v partyVerdict) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.verdicts == nil {
		f.verdicts = make(map[uint64]partyVerdict)
	}
	f.verdicts[teamId] = v
}

func (f *fakePartyPort) endedCalls() []partyEndedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]partyEndedCall(nil), f.ended...)
}

func (f *fakePartyPort) checkCalls() []partyEndedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]partyEndedCall(nil), f.checks...)
}

const partyTestConfig uint32 = 1

// newPartySvcCtx 副本 1 上限 5 人、注入假 team 端口的测试环境。
func newPartySvcCtx(t *testing.T) (*svc.ServiceContext, *miniredis.Miniredis, *fakePartyPort) {
	t.Helper()
	svcCtx, mr := newTestSvcCtx(t)
	useTestCatalog(t, svcCtx, map[uint32]uint32{1: 5})
	port := &fakePartyPort{}
	svcCtx.SetPartyTeam(port)
	return svcCtx, mr, port
}

// enqueueParty 用生产入口把 roster 作为 teamId 的补位单放进 target 人数档。
func enqueueParty(t *testing.T, svcCtx *svc.ServiceContext, teamId uint64, target uint32, roster ...uint64) {
	t.Helper()
	failed, err := NewTeamBattleStarter(svcCtx).EnqueueParty(context.Background(), partyTestConfig, target, teamId, roster, nil)
	require.NoError(t, err)
	require.Zero(t, failed)
}

func cancelQueueAs(t *testing.T, svcCtx *svc.ServiceContext, playerId uint64) {
	t.Helper()
	_, err := NewCancelQueueLogic(context.Background(), svcCtx).CancelQueue(&matchpb.CancelQueueRequest{PlayerId: playerId})
	require.NoError(t, err)
}

func requireNoTicket(t *testing.T, svcCtx *svc.ServiceContext, playerIds ...uint64) {
	t.Helper()
	for _, pid := range playerIds {
		ticket, err := loadTicket(svcCtx, pid)
		require.NoError(t, err)
		require.Nil(t, ticket, "player=%d 不应再有票据", pid)
	}
}

func requireTicketState(t *testing.T, svcCtx *svc.ServiceContext, state string, playerIds ...uint64) {
	t.Helper()
	for _, pid := range playerIds {
		ticket, err := loadTicket(svcCtx, pid)
		require.NoError(t, err)
		require.NotNil(t, ticket, "player=%d 应有票据", pid)
		require.Equal(t, state, ticket.State, "player=%d", pid)
	}
}

func idStrings(ids ...uint64) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, strconv.FormatUint(id, 10))
	}
	return out
}

// ---- 名单编解码与选人的纯函数 ----

func TestPartyMembersCodec(t *testing.T) {
	roster := []uint64{31003, 31001, 31002}
	require.Equal(t, "31003,31001,31002", formatPartyMembers(roster))
	require.Equal(t, roster, parsePartyMembers(formatPartyMembers(roster)), "顺序是契约:它决定站位")
	require.Nil(t, parsePartyMembers(""))
	// 损坏的名单一律读成 nil:拿不到完整名单就凑不齐这一单,matcher 会把它撤掉。
	for _, raw := range []string{"1,x", "1,,2", "1,1", "0,2", "-1", "1, 2"} {
		require.Nil(t, parsePartyMembers(raw), raw)
	}
}

// selectUnits:人数之和恰好等于凑满人数;多解取等待序字典序最小;不可用的单位不参与。
func TestSelectUnitsExactSumPrefersEarliest(t *testing.T) {
	unit := func(size int) *queueUnit {
		members := make([]uint64, size)
		return &queueUnit{members: members, complete: true}
	}
	pick := func(required int, sizes ...int) []int {
		units := make([]*queueUnit, 0, len(sizes))
		for _, size := range sizes {
			if size < 0 { // 负数 = 这个位置上是一个不可用的单位
				u := unit(-size)
				u.complete = false
				units = append(units, u)
				continue
			}
			units = append(units, unit(size))
		}
		var chosen []int
		for _, u := range selectUnits(units, required) {
			for i := range units {
				if units[i] == u {
					chosen = append(chosen, i)
				}
			}
		}
		return chosen
	}

	require.Equal(t, []int{0, 1, 2}, pick(3, 1, 1, 1, 1), "全是单人 = 前 required 个")
	require.Nil(t, pick(3, 1, 1), "人数不够")
	require.Nil(t, pick(3, 2, 2), "两支 2 人队凑不成 3:队伍不可拆")
	require.Equal(t, []int{0, 2}, pick(3, 2, 2, 1), "队首的 2 人队 + 后面的单人")
	require.Equal(t, []int{0, 2}, pick(5, 4, 2, 1), "队首 4 人队等到一个单人就优先成组")
	require.Equal(t, []int{1, 2}, pick(5, 4, 3, 2), "队首凑不满时后面的照常成组,不堵队")
	require.Equal(t, []int{0, 2}, pick(5, 2, 2, 3), "贪心先拿 2+2 会卡住,必须能回头选 2+3")
	require.Equal(t, []int{1, 2, 3}, pick(3, -2, 1, 1, 1), "不可用(未凑齐)的单位被跳过")
	require.Nil(t, pick(0, 1, 1))
}

// ---- 入队 ----

func TestEnqueuePartyWritesTicketsAndQueue(t *testing.T) {
	svcCtx, mr, _ := newPartySvcCtx(t)
	const teamId = uint64(9001)
	enqueueParty(t, svcCtx, teamId, 3, 31002, 31001) // 队长 31002 在前

	queueKey := matchSizedQueueKey(pveTeamMode, partyTestConfig, 3)
	require.Equal(t, idStrings(31002, 31001), queueMembers(t, svcCtx, queueKey), "整单按名单顺序进队尾")
	assertQueueMirrorConsistent(t, mr, queueKey)
	registered, err := svcCtx.MatchRedis.Sismember(matchQueueIndexKey, queueKey)
	require.NoError(t, err)
	require.True(t, registered, "队列必须在注册集里,否则 matcher 扫不到")

	leader, err := loadTicket(svcCtx, 31002)
	require.NoError(t, err)
	member, err := loadTicket(svcCtx, 31001)
	require.NoError(t, err)
	require.NotEmpty(t, leader.PartyId)
	require.Equal(t, leader.PartyId, member.PartyId, "同一单共用 party_id")
	require.NotEqual(t, leader.Ticket, member.Ticket, "每人一张票")
	for _, ticket := range []*queueTicket{leader, member} {
		require.Equal(t, ticketStateQueued, ticket.State)
		require.Equal(t, queueKey, ticket.QueueKey)
		require.Equal(t, teamId, ticket.TeamId)
		require.Equal(t, []uint64{31002, 31001}, ticket.PartyMembers)
		require.Equal(t, pveTeamMode, ticket.Mode)
		require.Equal(t, partyTestConfig, ticket.Config)
	}
	ttl, err := svcCtx.MatchRedis.Ttl(matchTicketKey(31001))
	require.NoError(t, err)
	require.Greater(t, ttl, 3600, "排队票是长 TTL")

	starter := NewTeamBattleStarter(svcCtx)
	state, target, config, queuedAt, err := starter.PartyQueueState(context.Background(), 31001, teamId)
	require.NoError(t, err)
	require.Equal(t, constants.PartyQueueQueued, state)
	require.Equal(t, uint32(3), target)
	require.Equal(t, partyTestConfig, config)
	require.Equal(t, leader.EnqueuedAtMs, queuedAt)
	// 别的队伍问同一个人:不是它的补位票。
	state, _, _, _, err = starter.PartyQueueState(context.Background(), 31001, teamId+1)
	require.NoError(t, err)
	require.Equal(t, constants.PartyQueueNone, state)
	// 没有票据的人。
	state, _, _, _, err = starter.PartyQueueState(context.Background(), 31999, teamId)
	require.NoError(t, err)
	require.Equal(t, constants.PartyQueueNone, state)
}

// 名单里有人已经在排别的单:整单不入队,已建的票回滚,那个人原来的票不动。
func TestEnqueuePartyConflictRollsBack(t *testing.T) {
	svcCtx, mr, _ := newPartySvcCtx(t)
	setPlayerLocation(t, mr, 31012, 1)
	solo := joinPveTeam(t, svcCtx, 31012, partyTestConfig, 2)
	require.Zero(t, solo.ErrorCode)

	failed, err := NewTeamBattleStarter(svcCtx).EnqueueParty(context.Background(), partyTestConfig, 4, 9002,
		[]uint64{31011, 31012, 31013}, nil)
	require.NoError(t, err)
	require.Equal(t, uint64(31012), failed)

	requireNoTicket(t, svcCtx, 31011, 31013)
	kept, err := loadTicket(svcCtx, 31012)
	require.NoError(t, err)
	require.Equal(t, solo.QueueTicket, kept.Ticket, "冲突成员持有的是别的票,不能被回滚删掉")
	require.Empty(t, queueMembers(t, svcCtx, matchSizedQueueKey(pveTeamMode, partyTestConfig, 4)))
}

func TestEnqueuePartyRejectsBadArguments(t *testing.T) {
	svcCtx, _, _ := newPartySvcCtx(t)
	starter := NewTeamBattleStarter(svcCtx)
	cases := []struct {
		name   string
		target uint32
		roster []uint64
	}{
		{"目标人数等于当前人数(该走直接开战)", 2, []uint64{31021, 31022}},
		{"目标人数小于当前人数", 2, []uint64{31021, 31022, 31023}},
		{"超过副本上限", 6, []uint64{31021, 31022}},
		{"空名单", 3, nil},
	}
	for _, c := range cases {
		failed, err := starter.EnqueueParty(context.Background(), partyTestConfig, c.target, 9003, c.roster, nil)
		require.Error(t, err, c.name)
		require.Zero(t, failed, c.name)
	}
	requireNoTicket(t, svcCtx, 31021, 31022, 31023)
	// 未开放组队的副本(上限 0)。
	_, err := starter.EnqueueParty(context.Background(), 77, 3, 9003, []uint64{31021, 31022}, nil)
	require.Error(t, err)
}

// ---- 成组 ----

// 2 人队 + 1 个路人凑满 3 人档:三个人进同一场,队伍在前(先到)、站位按名单;弹组前向 team 复核过一次。
func TestPartyBackfillMatchesWithSolo(t *testing.T) {
	svcCtx, mr, port := newPartySvcCtx(t)
	calls := stubPveGather(t)
	const teamId = uint64(9010)
	queueKey := matchSizedQueueKey(pveTeamMode, partyTestConfig, 3)

	enqueueParty(t, svcCtx, teamId, 3, 31032, 31031)
	matchQueueOnce(svcCtx, queueKey)
	expectNoPveGather(t, calls) // 只有 2 人,不够
	require.Equal(t, idStrings(31032, 31031), queueMembers(t, svcCtx, queueKey))

	setPlayerLocation(t, mr, 31033, 2)
	require.Zero(t, joinPveTeam(t, svcCtx, 31033, partyTestConfig, 3).ErrorCode)
	matchQueueOnce(svcCtx, queueKey)
	call := expectPveGather(t, calls)
	require.Equal(t, []uint64{31032, 31031, 31033}, call.members)
	require.True(t, call.requeue)
	require.Equal(t, uint32(partyTestConfig), call.config)
	require.Empty(t, queueMembers(t, svcCtx, queueKey))
	assertQueueMirrorConsistent(t, mr, queueKey)
	requireTicketState(t, svcCtx, ticketStateMatched, 31031, 31032, 31033)

	checks := port.checkCalls()
	require.Len(t, checks, 1, "只在真要弹出这一单时复核一次")
	require.Equal(t, teamId, checks[0].teamId)
	require.Equal(t, []uint64{31032, 31031}, checks[0].members)
	require.Empty(t, port.endedCalls())

	state, _, _, _, err := NewTeamBattleStarter(svcCtx).PartyQueueState(context.Background(), 31031, teamId)
	require.NoError(t, err)
	require.Equal(t, constants.PartyQueueMatched, state)
}

// 3 人档里两支 2 人队:总人数 4 ≥ 3,但队伍不可拆,凑不出恰好 3 人 —— 不开局、不动任何数据。
func TestTwoPartiesThatCannotFitAreNeverSplit(t *testing.T) {
	svcCtx, mr, port := newPartySvcCtx(t)
	calls := stubPveGather(t)
	queueKey := matchSizedQueueKey(pveTeamMode, partyTestConfig, 3)
	enqueueParty(t, svcCtx, 9020, 3, 31041, 31042)
	enqueueParty(t, svcCtx, 9021, 3, 31043, 31044)

	for i := 0; i < 2; i++ {
		matchQueueOnce(svcCtx, queueKey)
		expectNoPveGather(t, calls)
	}
	require.Equal(t, idStrings(31041, 31042, 31043, 31044), queueMembers(t, svcCtx, queueKey))
	assertQueueMirrorConsistent(t, mr, queueKey)
	requireTicketState(t, svcCtx, ticketStateQueued, 31041, 31042, 31043, 31044)
	require.Empty(t, port.checkCalls(), "选不出组就不该去打扰 team")
	require.Empty(t, port.endedCalls())

	// 来一个路人:队首那支队伍先走,第二支原样留下。
	setPlayerLocation(t, mr, 31045, 1)
	require.Zero(t, joinPveTeam(t, svcCtx, 31045, partyTestConfig, 3).ErrorCode)
	matchQueueOnce(svcCtx, queueKey)
	require.Equal(t, []uint64{31041, 31042, 31045}, expectPveGather(t, calls).members)
	expectNoPveGather(t, calls)
	require.Equal(t, idStrings(31043, 31044), queueMembers(t, svcCtx, queueKey))
	requireTicketState(t, svcCtx, ticketStateQueued, 31043, 31044)
}

// 5 人档:队首 4 人队眼下凑不满(后面是 3 人队和 2 人队),后两支照常成组,不被队首堵住;
// 之后来一个路人,4 人队成组。
func TestHeadPartyDoesNotBlockQueueButGoesFirstWhenFillable(t *testing.T) {
	svcCtx, mr, _ := newPartySvcCtx(t)
	calls := stubPveGather(t)
	queueKey := matchSizedQueueKey(pveTeamMode, partyTestConfig, 5)
	enqueueParty(t, svcCtx, 9030, 5, 31051, 31052, 31053, 31054)
	enqueueParty(t, svcCtx, 9031, 5, 31055, 31056, 31057)
	enqueueParty(t, svcCtx, 9032, 5, 31058, 31059)

	matchQueueOnce(svcCtx, queueKey)
	require.Equal(t, []uint64{31055, 31056, 31057, 31058, 31059}, expectPveGather(t, calls).members)
	expectNoPveGather(t, calls)
	require.Equal(t, idStrings(31051, 31052, 31053, 31054), queueMembers(t, svcCtx, queueKey))
	requireTicketState(t, svcCtx, ticketStateQueued, 31051, 31052, 31053, 31054)

	setPlayerLocation(t, mr, 31060, 1)
	require.Zero(t, joinPveTeam(t, svcCtx, 31060, partyTestConfig, 5).ErrorCode)
	matchQueueOnce(svcCtx, queueKey)
	require.Equal(t, []uint64{31051, 31052, 31053, 31054, 31060}, expectPveGather(t, calls).members)
	require.Empty(t, queueMembers(t, svcCtx, queueKey))
	assertQueueMirrorConsistent(t, mr, queueKey)
}

// ---- 弹组前复核 ----

// team 说这支队伍的名单变了:整单撤出(删票、摘 list、通知),路人原样留在队列里,本轮不开局。
func TestPartyEvictedWhenTeamCheckFails(t *testing.T) {
	svcCtx, mr, port := newPartySvcCtx(t)
	calls := stubPveGather(t)
	const teamId = uint64(9040)
	queueKey := matchSizedQueueKey(pveTeamMode, partyTestConfig, 3)
	enqueueParty(t, svcCtx, teamId, 3, 31061, 31062)
	setPlayerLocation(t, mr, 31063, 1)
	require.Zero(t, joinPveTeam(t, svcCtx, 31063, partyTestConfig, 3).ErrorCode)
	port.setVerdict(teamId, partyVerdict{reason: constants.PartyEndMemberOffline, culprit: 31062})

	matchQueueOnce(svcCtx, queueKey)
	expectNoPveGather(t, calls)
	requireNoTicket(t, svcCtx, 31061, 31062)
	requireTicketState(t, svcCtx, ticketStateQueued, 31063)
	require.Equal(t, idStrings(31063), queueMembers(t, svcCtx, queueKey))
	assertQueueMirrorConsistent(t, mr, queueKey)
	require.Equal(t, []partyEndedCall{{teamId: teamId, members: []uint64{31061, 31062},
		reason: constants.PartyEndMemberOffline, culprit: 31062}}, port.endedCalls())
	require.False(t, mr.Exists(mustLockKey(t, queueKey)), "凑单锁必须释放")
}

// team 复核读不到(Redis 抖动):这一单本轮搁置 —— 不开局、不撤单、不动数据;恢复后正常成组。
// (搁置期间后面的路人照常成组,见 party_review_test.go TestUnverifiablePartyDoesNotBlockSolosBehind。)
func TestPartyCheckErrorDefersPartyWithoutEviction(t *testing.T) {
	svcCtx, mr, port := newPartySvcCtx(t)
	calls := stubPveGather(t)
	const teamId = uint64(9050)
	queueKey := matchSizedQueueKey(pveTeamMode, partyTestConfig, 3)
	enqueueParty(t, svcCtx, teamId, 3, 31071, 31072)
	setPlayerLocation(t, mr, 31073, 1)
	require.Zero(t, joinPveTeam(t, svcCtx, 31073, partyTestConfig, 3).ErrorCode)
	port.setVerdict(teamId, partyVerdict{err: errors.New("shared redis down")})

	matchQueueOnce(svcCtx, queueKey)
	expectNoPveGather(t, calls)
	require.Equal(t, idStrings(31071, 31072, 31073), queueMembers(t, svcCtx, queueKey))
	requireTicketState(t, svcCtx, ticketStateQueued, 31071, 31072, 31073)
	require.Empty(t, port.endedCalls())

	port.setVerdict(teamId, partyVerdict{})
	matchQueueOnce(svcCtx, queueKey)
	require.Equal(t, []uint64{31071, 31072, 31073}, expectPveGather(t, calls).members)
}

// 补位单里有人的票据没了(过期 / 被别的路径删掉):这一单凑不齐,扫完整单撤出,通知里带缺席的人。
func TestIncompletePartyEvictedAsAWhole(t *testing.T) {
	svcCtx, mr, port := newPartySvcCtx(t)
	calls := stubPveGather(t)
	const teamId = uint64(9060)
	queueKey := matchSizedQueueKey(pveTeamMode, partyTestConfig, 3)
	enqueueParty(t, svcCtx, teamId, 3, 31081, 31082)
	setPlayerLocation(t, mr, 31083, 1)
	require.Zero(t, joinPveTeam(t, svcCtx, 31083, partyTestConfig, 3).ErrorCode)
	mr.Del(matchTicketKey(31082)) // 模拟票据过期

	matchQueueOnce(svcCtx, queueKey)
	expectNoPveGather(t, calls)
	requireNoTicket(t, svcCtx, 31081, 31082)
	require.Equal(t, idStrings(31083), queueMembers(t, svcCtx, queueKey), "路人不受影响,同队残留项一并摘掉")
	assertQueueMirrorConsistent(t, mr, queueKey)
	require.Equal(t, []partyEndedCall{{teamId: teamId, members: []uint64{31081, 31082},
		reason: constants.PartyEndMemberNotReady, culprit: 31082}}, port.endedCalls())
}

// ---- 取消 ----

// 任何一名队员取消 = 整单退出:全员票据删除、全单出队、通知里 culprit 是取消者。
func TestCancelQueueByMemberCancelsWholeParty(t *testing.T) {
	svcCtx, mr, port := newPartySvcCtx(t)
	const teamId = uint64(9070)
	queueKey := matchSizedQueueKey(pveTeamMode, partyTestConfig, 4)
	enqueueParty(t, svcCtx, teamId, 4, 31091, 31092, 31093)
	setPlayerLocation(t, mr, 31094, 1)
	require.Zero(t, joinPveTeam(t, svcCtx, 31094, partyTestConfig, 4).ErrorCode)

	cancelQueueAs(t, svcCtx, 31092) // 不是队长

	requireNoTicket(t, svcCtx, 31091, 31092, 31093)
	require.Equal(t, idStrings(31094), queueMembers(t, svcCtx, queueKey))
	assertQueueMirrorConsistent(t, mr, queueKey)
	require.Equal(t, []partyEndedCall{{teamId: teamId, members: []uint64{31091, 31092, 31093},
		reason: constants.PartyEndCanceled, culprit: 31092}}, port.endedCalls())

	// 重复取消幂等;撤单后每个人都可以立刻单排。
	cancelQueueAs(t, svcCtx, 31092)
	require.Len(t, port.endedCalls(), 1)
	setPlayerLocation(t, mr, 31091, 1)
	require.Zero(t, joinPveTeam(t, svcCtx, 31091, partyTestConfig, 2).ErrorCode)
}

// 凑满弹出之后再取消:太迟,整单照常开局,票据不动。
func TestCancelQueueTooLateAfterPartyMatched(t *testing.T) {
	svcCtx, mr, port := newPartySvcCtx(t)
	calls := stubPveGather(t)
	queueKey := matchSizedQueueKey(pveTeamMode, partyTestConfig, 3)
	enqueueParty(t, svcCtx, 9080, 3, 31101, 31102)
	setPlayerLocation(t, mr, 31103, 1)
	require.Zero(t, joinPveTeam(t, svcCtx, 31103, partyTestConfig, 3).ErrorCode)
	matchQueueOnce(svcCtx, queueKey)
	require.Equal(t, []uint64{31101, 31102, 31103}, expectPveGather(t, calls).members)

	cancelQueueAs(t, svcCtx, 31102)
	requireTicketState(t, svcCtx, ticketStateMatched, 31101, 31102, 31103)
	require.Empty(t, port.endedCalls())
}

// 弹出之后、推进 matched 之前,队员的取消到达(票据仍 queued → 取消成功):整单不进 gather,
// 同队另一人的票也被撤掉,路人回队首继续等。
func TestMemberCancelBetweenPopAndMatchedDropsWholeParty(t *testing.T) {
	svcCtx, mr, port := newPartySvcCtx(t)
	calls := stubPveGather(t)
	const teamId = uint64(9090)
	queueKey := matchSizedQueueKey(pveTeamMode, partyTestConfig, 3)
	enqueueParty(t, svcCtx, teamId, 3, 31111, 31112)
	setPlayerLocation(t, mr, 31113, 1)
	require.Zero(t, joinPveTeam(t, svcCtx, 31113, partyTestConfig, 3).ErrorCode)

	hookRan := false
	withMatcherAfterPopHook(t, func(members []uint64) {
		hookRan = true
		require.Equal(t, []uint64{31111, 31112, 31113}, members)
		cancelQueueAs(t, svcCtx, 31112)
	})
	matchQueueOnce(svcCtx, queueKey)

	require.True(t, hookRan)
	expectNoPveGather(t, calls)
	requireNoTicket(t, svcCtx, 31111, 31112)
	requireTicketState(t, svcCtx, ticketStateQueued, 31113)
	require.Equal(t, idStrings(31113), queueMembers(t, svcCtx, queueKey))
	assertQueueMirrorConsistent(t, mr, queueKey)
	ended := port.endedCalls()
	require.NotEmpty(t, ended)
	require.Equal(t, partyEndedCall{teamId: teamId, members: []uint64{31111, 31112},
		reason: constants.PartyEndCanceled, culprit: 31112}, ended[0])
}

// ---- 开局失败回队首 ----

// popMatchedParty 铺出「2 人队 + 1 路人已弹出并推进 matched」的现场,返回弹组时的 ticket id。
func popMatchedParty(t *testing.T, svcCtx *svc.ServiceContext, mr *miniredis.Miniredis, teamId uint64, a, b, solo uint64) (string, map[uint64]string) {
	t.Helper()
	queueKey := matchSizedQueueKey(pveTeamMode, partyTestConfig, 3)
	enqueueParty(t, svcCtx, teamId, 3, a, b)
	setPlayerLocation(t, mr, solo, 1)
	require.Zero(t, joinPveTeam(t, svcCtx, solo, partyTestConfig, 3).ErrorCode)
	members, tickets, ok := popGroup(svcCtx, queueKey, 3)
	require.True(t, ok)
	require.Equal(t, []uint64{a, b, solo}, members)
	for _, pid := range members {
		written, err := setTicketMatched(svcCtx, pid, tickets[pid], matchedTicketTTLFor(svcCtx, 3))
		require.NoError(t, err)
		require.True(t, written)
	}
	require.Empty(t, queueMembers(t, svcCtx, queueKey))
	return queueKey, tickets
}

// 肇事者是路人(或没有肇事者):队伍整体回队首,顺序不变、票据回 queued、仍是同一单。
func TestRequeueFrontKeepsPartyTogether(t *testing.T) {
	svcCtx, mr, port := newPartySvcCtx(t)
	queueKey, tickets := popMatchedParty(t, svcCtx, mr, 9100, 31121, 31122, 31123)
	// 队尾先有别人在排,回队首的人必须插到他前面。
	setPlayerLocation(t, mr, 31124, 1)
	require.Zero(t, joinPveTeam(t, svcCtx, 31124, partyTestConfig, 3).ErrorCode)

	deleteTicketIfOwned(svcCtx, 31123, tickets[31123]) // gather 的失败出口:肇事者(路人)删票
	requeueFront(svcCtx, queueKey, []uint64{31121, 31122}, tickets)

	require.Equal(t, idStrings(31121, 31122, 31124), queueMembers(t, svcCtx, queueKey))
	assertQueueMirrorConsistent(t, mr, queueKey)
	requireTicketState(t, svcCtx, ticketStateQueued, 31121, 31122)
	leader, err := loadTicket(svcCtx, 31121)
	require.NoError(t, err)
	require.Equal(t, tickets[31121], leader.Ticket)
	require.Equal(t, []uint64{31121, 31122}, leader.PartyMembers)
	ttl, err := svcCtx.MatchRedis.Ttl(matchTicketKey(31121))
	require.NoError(t, err)
	require.Greater(t, ttl, 3600, "回队后恢复长 TTL")
	require.Empty(t, port.endedCalls())
}

// 肇事者是队员:他不在回队名单里 → 同队其余人也不回队(删票、通知),路人照常回队首。
func TestRequeueFrontDropsPartyWhenAMemberIsMissing(t *testing.T) {
	svcCtx, mr, port := newPartySvcCtx(t)
	const teamId = uint64(9110)
	queueKey, tickets := popMatchedParty(t, svcCtx, mr, teamId, 31131, 31132, 31133)

	deleteTicketIfOwned(svcCtx, 31132, tickets[31132]) // 肇事者(队员)删票
	requeueFront(svcCtx, queueKey, []uint64{31131, 31133}, tickets)

	requireNoTicket(t, svcCtx, 31131, 31132)
	requireTicketState(t, svcCtx, ticketStateQueued, 31133)
	require.Equal(t, idStrings(31133), queueMembers(t, svcCtx, queueKey))
	assertQueueMirrorConsistent(t, mr, queueKey)
	require.Equal(t, []partyEndedCall{{teamId: teamId, members: []uint64{31131, 31132},
		reason: constants.PartyEndMemberNotReady, culprit: 31132}}, port.endedCalls())
}

// 队员在名单里但票据已被替换(matched TTL 到期后重排):同样算缺席,整单不回队。
func TestRequeueFrontDropsPartyWhenAMemberTicketWasReplaced(t *testing.T) {
	svcCtx, mr, port := newPartySvcCtx(t)
	const teamId = uint64(9120)
	queueKey, tickets := popMatchedParty(t, svcCtx, mr, teamId, 31141, 31142, 31143)
	mr.Del(matchTicketKey(31142))
	mustCreateTicket(t, svcCtx, 31142, &queueTicket{Ticket: "replaced", Mode: pveTeamMode, Config: partyTestConfig,
		State: ticketStateQueued, EnqueuedAtMs: nowMs()})

	requeueFront(svcCtx, queueKey, []uint64{31141, 31142, 31143}, tickets)

	requireNoTicket(t, svcCtx, 31141)
	replaced, err := loadTicket(svcCtx, 31142)
	require.NoError(t, err)
	require.Equal(t, "replaced", replaced.Ticket, "别人的新票不能动")
	require.Equal(t, idStrings(31143), queueMembers(t, svcCtx, queueKey))
	require.Len(t, port.endedCalls(), 1)
	require.Equal(t, uint64(31142), port.endedCalls()[0].culprit)
}

// ---- team 侧撤单入口与自愈 ----

func TestCancelPartyByTeam(t *testing.T) {
	svcCtx, mr, port := newPartySvcCtx(t)
	const teamId = uint64(9130)
	queueKey := matchSizedQueueKey(pveTeamMode, partyTestConfig, 4)
	enqueueParty(t, svcCtx, teamId, 4, 31151, 31152)
	starter := NewTeamBattleStarter(svcCtx)

	// 候选里先是没有票的人、别的队伍:都跳过。
	canceled, err := starter.CancelParty(context.Background(), teamId+1, []uint64{31151, 31152}, nil, constants.PartyEndRosterChanged, 0)
	require.NoError(t, err)
	require.Nil(t, canceled, "别的队伍的 id 定位不到这一单")
	requireTicketState(t, svcCtx, ticketStateQueued, 31151, 31152)

	// 这一单的名单恰好等于「提交后的成员集合」:它就是按新名单入队的有效单,迟到的撤单不能动它。
	canceled, err = starter.CancelParty(context.Background(), teamId, []uint64{31151, 31152}, []uint64{31152, 31151},
		constants.PartyEndRosterChanged, 0)
	require.NoError(t, err)
	require.Nil(t, canceled)
	requireTicketState(t, svcCtx, ticketStateQueued, 31151, 31152)
	require.Empty(t, port.endedCalls())

	// 提交后的成员集合与这一单不同(有人离队了):撤。
	canceled, err = starter.CancelParty(context.Background(), teamId, []uint64{31159, 31152}, []uint64{31152},
		constants.PartyEndRosterChanged, 0)
	require.NoError(t, err)
	require.Equal(t, []uint64{31151, 31152}, canceled)
	requireNoTicket(t, svcCtx, 31151, 31152)
	require.Empty(t, queueMembers(t, svcCtx, queueKey))
	assertQueueMirrorConsistent(t, mr, queueKey)
	require.Equal(t, []partyEndedCall{{teamId: teamId, members: []uint64{31151, 31152},
		reason: constants.PartyEndRosterChanged}}, port.endedCalls())

	// 已经没有在排的单:幂等。
	canceled, err = starter.CancelParty(context.Background(), teamId, []uint64{31151, 31152}, nil, constants.PartyEndRosterChanged, 0)
	require.NoError(t, err)
	require.Nil(t, canceled)
	require.Len(t, port.endedCalls(), 1)

	// 静默撤单(入队回滚):撤掉但不通知。
	enqueueParty(t, svcCtx, teamId, 4, 31151, 31152)
	canceled, err = starter.CancelParty(context.Background(), teamId, []uint64{31151}, nil, constants.PartyEndNone, 0)
	require.NoError(t, err)
	require.Equal(t, []uint64{31151, 31152}, canceled)
	requireNoTicket(t, svcCtx, 31151, 31152)
	require.Len(t, port.endedCalls(), 1, "PartyEndNone 不通知")
}

// 队员在补位期间自己去单排:队伍仍然有效 → 已在排队;team 说这一单已失效(主动撤单没成功的兜底)→
// 撤整单后放行,同队其他人的票也清掉。
func TestJoinQueueHealsStalePartyTicket(t *testing.T) {
	svcCtx, mr, port := newPartySvcCtx(t)
	const teamId = uint64(9140)
	queueKey := matchSizedQueueKey(pveTeamMode, partyTestConfig, 3)
	enqueueParty(t, svcCtx, teamId, 3, 31161, 31162)
	setPlayerLocation(t, mr, 31161, 1)

	resp := joinPveTeam(t, svcCtx, 31161, partyTestConfig, 2)
	require.Equal(t, constants.ErrAlreadyQueued, resp.ErrorCode)
	requireTicketState(t, svcCtx, ticketStateQueued, 31161, 31162)

	port.setVerdict(teamId, partyVerdict{reason: constants.PartyEndRosterChanged})
	resp = joinPveTeam(t, svcCtx, 31161, partyTestConfig, 2)
	require.Zero(t, resp.ErrorCode, "失效的补位单不能把人卡在「已在排队」")
	requireNoTicket(t, svcCtx, 31162)
	require.Empty(t, queueMembers(t, svcCtx, queueKey))
	solo, err := loadTicket(svcCtx, 31161)
	require.NoError(t, err)
	require.Equal(t, resp.QueueTicket, solo.Ticket)
	require.Empty(t, solo.PartyId)
	require.Equal(t, []partyEndedCall{{teamId: teamId, members: []uint64{31161, 31162},
		reason: constants.PartyEndRosterChanged}}, port.endedCalls())
}

// 补位票「queued 但不在 list」:入队宽限内按正在入队处理(不能把刚建好票、还没 RPUSH 的整单当孤儿撤掉);
// 超过宽限 = 崩溃残留,撤整单后放行。
func TestOrphanPartyTicketRespectsEnqueueGrace(t *testing.T) {
	svcCtx, mr, port := newPartySvcCtx(t)
	const teamId = uint64(9150)
	queueKey := matchSizedQueueKey(pveTeamMode, partyTestConfig, 3)
	enqueueParty(t, svcCtx, teamId, 3, 31171, 31172)
	mr.Del(queueKey) // 模拟:票已建、整批入队还没发生(或入队丢失)
	rankKey, err := rankKeyForQueue(queueKey)
	require.NoError(t, err)
	mr.Del(rankKey)
	setPlayerLocation(t, mr, 31171, 1)

	resp := joinPveTeam(t, svcCtx, 31171, partyTestConfig, 2)
	require.Equal(t, constants.ErrAlreadyQueued, resp.ErrorCode, "宽限内不自愈")
	requireTicketState(t, svcCtx, ticketStateQueued, 31171, 31172)
	require.Empty(t, port.checkCalls(), "不在 list 时不问 team")

	stale := strconv.FormatUint(nowMs()-uint64(2*partyEnqueueGrace.Milliseconds()), 10)
	mr.HSet(matchTicketKey(31171), ticketFieldEnqueuedAt, stale)
	mr.HSet(matchTicketKey(31172), ticketFieldEnqueuedAt, stale)
	resp = joinPveTeam(t, svcCtx, 31171, partyTestConfig, 2)
	require.Zero(t, resp.ErrorCode)
	requireNoTicket(t, svcCtx, 31172)
	require.Len(t, port.endedCalls(), 1)
	require.Equal(t, constants.PartyEndMemberNotReady, port.endedCalls()[0].reason)
}

// ready 票(已开战,短暂停留)不算在补位:不能让一场打完的战斗把队伍面板锁在「集合中」。
func TestPartyQueueStateIgnoresReadyTicket(t *testing.T) {
	svcCtx, mr, _ := newPartySvcCtx(t)
	const teamId = uint64(9160)
	enqueueParty(t, svcCtx, teamId, 3, 31181, 31182)
	mr.HSet(matchTicketKey(31181), ticketFieldState, ticketStateReady)
	state, _, _, _, err := NewTeamBattleStarter(svcCtx).PartyQueueState(context.Background(), 31181, teamId)
	require.NoError(t, err)
	require.Equal(t, constants.PartyQueueNone, state)
}

// team 端口还没注入(进程刚重启、Redis 里留着存量补位单):「没人能复核」不等于「仍然有效」,
// 补位单不成组、原样留在队列里;路人之间照常成组。端口注入后恢复正常。
func TestPartyNotMatchedWhileTeamPortIsMissing(t *testing.T) {
	svcCtx, mr, port := newPartySvcCtx(t)
	calls := stubPveGather(t)
	queueKey := matchSizedQueueKey(pveTeamMode, partyTestConfig, 3)
	enqueueParty(t, svcCtx, 9170, 3, 31191, 31192)
	setPlayerLocation(t, mr, 31193, 1)
	require.Zero(t, joinPveTeam(t, svcCtx, 31193, partyTestConfig, 3).ErrorCode)
	svcCtx.SetPartyTeam(nil)

	matchQueueOnce(svcCtx, queueKey)
	expectNoPveGather(t, calls)
	require.Equal(t, idStrings(31191, 31192, 31193), queueMembers(t, svcCtx, queueKey))
	requireTicketState(t, svcCtx, ticketStateQueued, 31191, 31192, 31193)

	// 再来两个路人:三个路人成组,补位单继续等。
	for _, pid := range []uint64{31194, 31195} {
		setPlayerLocation(t, mr, pid, 1)
		require.Zero(t, joinPveTeam(t, svcCtx, pid, partyTestConfig, 3).ErrorCode)
	}
	matchQueueOnce(svcCtx, queueKey)
	require.Equal(t, []uint64{31193, 31194, 31195}, expectPveGather(t, calls).members)
	require.Equal(t, idStrings(31191, 31192), queueMembers(t, svcCtx, queueKey))

	// 端口注入之后,补位单与下一个路人正常成组。
	svcCtx.SetPartyTeam(port)
	setPlayerLocation(t, mr, 31196, 1)
	require.Zero(t, joinPveTeam(t, svcCtx, 31196, partyTestConfig, 3).ErrorCode)
	matchQueueOnce(svcCtx, queueKey)
	require.Equal(t, []uint64{31191, 31192, 31196}, expectPveGather(t, calls).members)
}
