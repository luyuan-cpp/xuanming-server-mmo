package logic

// 队伍补位匹配:设计评审(docs/design/team-backfill-matching.md §8)提出的边界情形的回归用例。
// 环境与辅助函数见 party_test.go。测试之间不并行(包级测试缝)。

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"match/internal/constants"
	"match/internal/playercontract"
	"match/internal/svc"

	matchpb "proto/match"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/stretchr/testify/require"
)

// redisHook miniredis 的命令前置钩子:返回 true = 钩子已代答,命令不再执行。
// 钩子在命令执行之前、miniredis 的内部锁之外、连接自己的协程里被调用:钩子里可以直接改库(mr.Del 等),
// 也可以再发命令(走别的连接)。被测代码与钩子不在同一个协程,钩子里的状态一律用原子量。
// 「内部锁之外」只对顶层命令成立:Lua 里的 redis.call 也会经过钩子,那时持有内部锁,在那里改库会死锁 ——
// 下面的钩子都只匹配顶层的 EVAL / GET / HGETALL。
type redisHook = func(c *server.Peer, cmd string, args ...string) bool

// installRedisHooks 装一组钩子(依次问,谁先代答谁生效),用例结束时摘掉。与 mr.SetError 共用同一个槽位。
func installRedisHooks(t *testing.T, mr *miniredis.Miniredis, hooks ...redisHook) {
	t.Helper()
	mr.Server().SetPreHook(func(c *server.Peer, cmd string, args ...string) bool {
		for _, hook := range hooks {
			if hook(c, cmd, args...) {
				return true
			}
		}
		return false
	})
	t.Cleanup(func() { mr.Server().SetPreHook(nil) })
}

// evalOf 匹配执行 script 的那条 EVAL;arg 非空时还要求参数里出现它(某个 key 或某个成员 id)。
func evalOf(script, arg string) func(cmd string, args []string) bool {
	return func(cmd string, args []string) bool {
		if cmd != "EVAL" || len(args) == 0 || args[0] != script {
			return false
		}
		if arg == "" {
			return true
		}
		for _, a := range args[1:] {
			if a == arg {
				return true
			}
		}
		return false
	}
}

// failWhen 让满足 match 的命令失败,其余照常。
func failWhen(match func(cmd string, args []string) bool) redisHook {
	return func(c *server.Peer, cmd string, args ...string) bool {
		if !match(cmd, args) {
			return false
		}
		c.WriteError("ERR injected failure")
		return true
	}
}

// onceBefore 在第一条满足 match 的命令执行之前跑一次 action,用来把「另一条路径」插进被测代码的两步之间。
// action 里再发的命令不会再次触发。第二个返回值报告它是否触发过。
func onceBefore(match func(cmd string, args []string) bool, action func()) (redisHook, func() bool) {
	var fired atomic.Bool
	hook := func(_ *server.Peer, cmd string, args ...string) bool {
		if fired.Load() || !match(cmd, args) {
			return false
		}
		if fired.CompareAndSwap(false, true) {
			action()
		}
		return false
	}
	return hook, fired.Load
}

func idString(id uint64) string { return strconv.FormatUint(id, 10) }

// team 复核读不到的补位单只在本轮搁置:不撤单、不通知,也不能堵住后面本可成组的路人。
func TestUnverifiablePartyDoesNotBlockSolosBehind(t *testing.T) {
	svcCtx, mr, port := newPartySvcCtx(t)
	calls := stubPveGather(t)
	const teamId = uint64(9200)
	queueKey := matchSizedQueueKey(pveTeamMode, partyTestConfig, 3)
	enqueueParty(t, svcCtx, teamId, 3, 32001, 32002)
	for pid := uint64(32003); pid <= 32005; pid++ {
		setPlayerLocation(t, mr, pid, 1)
		require.Zero(t, joinPveTeam(t, svcCtx, pid, partyTestConfig, 3).ErrorCode)
	}
	port.setVerdict(teamId, partyVerdict{err: errors.New("shared redis down")})

	matchQueueOnce(svcCtx, queueKey)

	require.Equal(t, []uint64{32003, 32004, 32005}, expectPveGather(t, calls).members,
		"队首的队伍核不了,后面三个路人照常成组")
	expectNoPveGather(t, calls)
	require.Equal(t, idStrings(32001, 32002), queueMembers(t, svcCtx, queueKey), "队伍原样留在队首")
	requireTicketState(t, svcCtx, ticketStateQueued, 32001, 32002)
	require.Empty(t, port.endedCalls())
	assertQueueMirrorConsistent(t, mr, queueKey)
}

// 开局失败回队首后,队员身上残留的备战锁(scene 那边的解冻丢了)不能把整支队伍踢出队列:
// 宽限内本轮搁置;锁消失后照常成组。
func TestResidualBattleLockAfterRequeueDefersParty(t *testing.T) {
	svcCtx, mr, port := newPartySvcCtx(t)
	calls := stubPveGather(t)
	queueKey, tickets := popMatchedParty(t, svcCtx, mr, 9210, 32011, 32012, 32013)
	const failedBattle = uint64(77001)
	// gather 的失败出口(没有肇事者):全员回队首,票据记下那一局的 battle_id。
	requeueFrontAfterBattle(svcCtx, queueKey, []uint64{32011, 32012, 32013}, tickets, failedBattle)
	require.Equal(t, idStrings(32011, 32012, 32013), queueMembers(t, svcCtx, queueKey))
	requeued, err := loadTicket(svcCtx, 32012)
	require.NoError(t, err)
	require.Equal(t, failedBattle, requeued.LastFailedBattleId, "回队首要记下失败的那一局")
	// scene 那边的解冻丢了:备战锁还在,锁值就是那一局的 battle_id。
	require.NoError(t, mr.Set(playercontract.BattleLockKey(32012), strconv.FormatUint(failedBattle, 10)))

	for i := 0; i < 2; i++ {
		matchQueueOnce(svcCtx, queueKey)
		expectNoPveGather(t, calls)
	}
	require.Equal(t, idStrings(32011, 32012, 32013), queueMembers(t, svcCtx, queueKey), "不摘 list")
	requireTicketState(t, svcCtx, ticketStateQueued, 32011, 32012, 32013)
	require.Empty(t, port.endedCalls(), "不撤单")

	mr.Del(playercontract.BattleLockKey(32012))
	matchQueueOnce(svcCtx, queueKey)
	require.Equal(t, []uint64{32011, 32012, 32013}, expectPveGather(t, calls).members)
}

// 同一把残留锁,单人回队首后也只是搁置(改动前会被静默删票踢出队列)。
func TestResidualBattleLockAfterRequeueDefersSolo(t *testing.T) {
	svcCtx, mr, _ := newPartySvcCtx(t)
	calls := stubPveGather(t)
	queueKey := matchSizedQueueKey(pveTeamMode, partyTestConfig, 2)
	for _, pid := range []uint64{32021, 32022} {
		setPlayerLocation(t, mr, pid, 1)
		require.Zero(t, joinPveTeam(t, svcCtx, pid, partyTestConfig, 2).ErrorCode)
	}
	members, tickets, ok := popGroup(svcCtx, queueKey, 2)
	require.True(t, ok)
	for _, pid := range members {
		written, err := setTicketMatched(svcCtx, pid, tickets[pid], matchedTicketTTLFor(svcCtx, 2))
		require.NoError(t, err)
		require.True(t, written)
	}
	requeueFrontAfterBattle(svcCtx, queueKey, members, tickets, 77002)
	require.NoError(t, mr.Set(playercontract.BattleLockKey(32021), "77002"))

	matchQueueOnce(svcCtx, queueKey)
	expectNoPveGather(t, calls)
	requireTicketState(t, svcCtx, ticketStateQueued, 32021, 32022)
	require.Equal(t, idStrings(32021, 32022), queueMembers(t, svcCtx, queueKey))

	mr.Del(playercontract.BattleLockKey(32021))
	matchQueueOnce(svcCtx, queueKey)
	require.Equal(t, []uint64{32021, 32022}, expectPveGather(t, calls).members)
}

// 战斗锁里是别的 battle_id:那就是真的进了别的战斗(排队期间应了切磋等)—— 按原规则删票出局,
// 所在补位单整单撤出。没经历过开局失败的票据同理(与改动前一致)。
func TestBattleLockOfAnotherBattleStillEvicts(t *testing.T) {
	svcCtx, mr, port := newPartySvcCtx(t)
	calls := stubPveGather(t)
	const teamId = uint64(9220)
	queueKey, tickets := popMatchedParty(t, svcCtx, mr, teamId, 32031, 32032, 32033)
	requeueFrontAfterBattle(svcCtx, queueKey, []uint64{32031, 32032, 32033}, tickets, 77003)
	require.NoError(t, mr.Set(playercontract.BattleLockKey(32032), "88888")) // 不是 77003

	matchQueueOnce(svcCtx, queueKey)

	expectNoPveGather(t, calls)
	requireNoTicket(t, svcCtx, 32031, 32032)
	requireTicketState(t, svcCtx, ticketStateQueued, 32033)
	require.Equal(t, idStrings(32033), queueMembers(t, svcCtx, queueKey))
	require.Len(t, port.endedCalls(), 1)
	require.Equal(t, constants.PartyEndMemberNotReady, port.endedCalls()[0].reason)
	require.Equal(t, uint64(32032), port.endedCalls()[0].culprit)

	// 没经历过开局失败的补位单:带锁的成员直接出局。
	enqueueParty(t, svcCtx, teamId+1, 3, 32034, 32035)
	require.NoError(t, mr.Set(playercontract.BattleLockKey(32035), "77004"))
	matchQueueOnce(svcCtx, queueKey)
	expectNoPveGather(t, calls)
	requireNoTicket(t, svcCtx, 32034, 32035)
}

// 缺席成员仍持有本单的票(另一个持锁方正在对这一单逐人推进 matched —— 凑单锁过期 / 丢锁后的并发):
// 本轮不判不完整,不能把一支已经凑满的队伍撤掉。
func TestPartyBeingMatchedElsewhereIsNotEvicted(t *testing.T) {
	svcCtx, mr, port := newPartySvcCtx(t)
	calls := stubPveGather(t)
	queueKey := matchSizedQueueKey(pveTeamMode, partyTestConfig, 3)
	enqueueParty(t, svcCtx, 9230, 3, 32041, 32042)
	setPlayerLocation(t, mr, 32043, 1)
	require.Zero(t, joinPveTeam(t, svcCtx, 32043, partyTestConfig, 3).ErrorCode)
	ticketA, err := loadTicket(svcCtx, 32041)
	require.NoError(t, err)
	// 另一实例已把 32041 推进 matched,32042 还没轮到。
	written, err := setTicketMatched(svcCtx, 32041, ticketA.Ticket, matchedTicketTTLFor(svcCtx, 3))
	require.NoError(t, err)
	require.True(t, written)

	matchQueueOnce(svcCtx, queueKey)

	expectNoPveGather(t, calls)
	requireTicketState(t, svcCtx, ticketStateMatched, 32041)
	requireTicketState(t, svcCtx, ticketStateQueued, 32042, 32043)
	require.Empty(t, port.endedCalls(), "不撤单、不通知")
	require.Contains(t, queueMembers(t, svcCtx, queueKey), "32042")
}

// 取消落在 matcher 两次票据 CAS 之间:队友的票已被推进 matched、取消者的票还是 queued。
// 取消成功;matcher 随后发现取消者的票没了,整单不进 gather。全队只收到一条通知,原因是「某某取消」。
func TestCancelBetweenTicketAdvancesNotifiesOnce(t *testing.T) {
	svcCtx, mr, port := newPartySvcCtx(t)
	calls := stubPveGather(t)
	const teamId = uint64(9240)
	queueKey := matchSizedQueueKey(pveTeamMode, partyTestConfig, 3)
	enqueueParty(t, svcCtx, teamId, 3, 32051, 32052)
	setPlayerLocation(t, mr, 32053, 1)
	require.Zero(t, joinPveTeam(t, svcCtx, 32053, partyTestConfig, 3).ErrorCode)

	withMatcherAfterPopHook(t, func(members []uint64) {
		// 模拟 matcher 已经推进了第一个人,第二个人的取消此刻到达。
		first, err := loadTicket(svcCtx, 32051)
		require.NoError(t, err)
		written, err := setTicketMatched(svcCtx, 32051, first.Ticket, matchedTicketTTLFor(svcCtx, 3))
		require.NoError(t, err)
		require.True(t, written)
		cancelQueueAs(t, svcCtx, 32052)
	})
	matchQueueOnce(svcCtx, queueKey)

	expectNoPveGather(t, calls)
	requireNoTicket(t, svcCtx, 32051, 32052)
	requireTicketState(t, svcCtx, ticketStateQueued, 32053)
	require.Equal(t, idStrings(32053), queueMembers(t, svcCtx, queueKey))
	require.Equal(t, []partyEndedCall{{teamId: teamId, members: []uint64{32051, 32052},
		reason: constants.PartyEndCanceled, culprit: 32052}}, port.endedCalls(), "一单只通知一次,主动取消的原因优先")
}

// 半撤状态:有人的票已经删了、另一人的票还 queued 且在 list 里(撤单时在他身上遇到 Redis 抖动)。
// 队列人数不到凑满人数,matcher 不会扫到;靠 JoinQueue / 开战预检的自愈发现并撤干净。
func TestHealClearsHalfCancelledParty(t *testing.T) {
	svcCtx, mr, port := newPartySvcCtx(t)
	const teamId = uint64(9250)
	queueKey := matchSizedQueueKey(pveTeamMode, partyTestConfig, 5)
	enqueueParty(t, svcCtx, teamId, 5, 32061, 32062)
	mr.Del(matchTicketKey(32061)) // 32061 的票已删,32062 被跳过
	setPlayerLocation(t, mr, 32062, 1)

	// team 认为队伍没变:只靠 team 复核发现不了这一单已经坏了。
	blocked, err := NewTeamBattleStarter(svcCtx).TicketBlocked(t.Context(), 32062)
	require.NoError(t, err)
	require.False(t, blocked, "残留的半撤补位票被自愈清掉,不再挡开战预检")

	requireNoTicket(t, svcCtx, 32061, 32062)
	require.Empty(t, queueMembers(t, svcCtx, queueKey))
	assertQueueMirrorConsistent(t, mr, queueKey)
	require.Empty(t, port.checkCalls(), "同单票据已不完整时不必再问 team")
	require.Len(t, port.endedCalls(), 1)
	require.Equal(t, uint64(32061), port.endedCalls()[0].culprit)
}

// 回队首按「单位」压回:一支补位单一条 Lua、单人一人一条,整体顺序与弹出时相同。
func TestRequeueFrontPushesUnitByUnitPreservingOrder(t *testing.T) {
	svcCtx, mr, _ := newPartySvcCtx(t)
	queueKey := matchSizedQueueKey(pveTeamMode, partyTestConfig, 5)
	setPlayerLocation(t, mr, 32071, 1)
	require.Zero(t, joinPveTeam(t, svcCtx, 32071, partyTestConfig, 5).ErrorCode)
	enqueueParty(t, svcCtx, 9260, 5, 32072, 32073)
	setPlayerLocation(t, mr, 32074, 1)
	require.Zero(t, joinPveTeam(t, svcCtx, 32074, partyTestConfig, 5).ErrorCode)
	enqueueParty(t, svcCtx, 9261, 5, 32075)
	members, tickets, ok := popGroup(svcCtx, queueKey, 5)
	require.True(t, ok)
	require.Equal(t, []uint64{32071, 32072, 32073, 32074, 32075}, members)
	for _, pid := range members {
		written, err := setTicketMatched(svcCtx, pid, tickets[pid], matchedTicketTTLFor(svcCtx, 5))
		require.NoError(t, err)
		require.True(t, written)
	}

	requeueFront(svcCtx, queueKey, members, tickets)

	require.Equal(t, idStrings(32071, 32072, 32073, 32074, 32075), queueMembers(t, svcCtx, queueKey))
	assertQueueMirrorConsistent(t, mr, queueKey)
	requireTicketState(t, svcCtx, ticketStateQueued, members...)
}

// 弹出到回队首之间队伍名单变了(那段时间票据是 matched,名单变化的主动撤单按「太迟」不动):
// 回队首前再向 team 复核一次,不过的整单不放回队列;复核读不到的照常回队。
func TestRequeueFrontRechecksPartyWithTeam(t *testing.T) {
	svcCtx, mr, port := newPartySvcCtx(t)
	const teamId = uint64(9270)
	queueKey, tickets := popMatchedParty(t, svcCtx, mr, teamId, 32081, 32082, 32083)
	port.setVerdict(teamId, partyVerdict{reason: constants.PartyEndRosterChanged})

	requeueFrontAfterBattle(svcCtx, queueKey, []uint64{32081, 32082, 32083}, tickets, 77010)

	requireNoTicket(t, svcCtx, 32081, 32082)
	requireTicketState(t, svcCtx, ticketStateQueued, 32083)
	require.Equal(t, idStrings(32083), queueMembers(t, svcCtx, queueKey))
	assertQueueMirrorConsistent(t, mr, queueKey)
	require.Equal(t, []partyEndedCall{{teamId: teamId, members: []uint64{32081, 32082},
		reason: constants.PartyEndRosterChanged}}, port.endedCalls())

	// 复核读不到:不做判定,整队照常回队首。
	const otherTeam = uint64(9271)
	queueKey2 := matchSizedQueueKey(pveTeamMode, partyTestConfig, 4)
	enqueueParty(t, svcCtx, otherTeam, 4, 32084, 32085, 32086)
	setPlayerLocation(t, mr, 32087, 1)
	require.Zero(t, joinPveTeam(t, svcCtx, 32087, partyTestConfig, 4).ErrorCode)
	members, tickets2, ok := popGroup(svcCtx, queueKey2, 4)
	require.True(t, ok)
	for _, pid := range members {
		written, err := setTicketMatched(svcCtx, pid, tickets2[pid], matchedTicketTTLFor(svcCtx, 4))
		require.NoError(t, err)
		require.True(t, written)
	}
	port.setVerdict(otherTeam, partyVerdict{err: errors.New("shared redis down")})
	requeueFrontAfterBattle(svcCtx, queueKey2, members, tickets2, 77011)
	require.Equal(t, idStrings(32084, 32085, 32086, 32087), queueMembers(t, svcCtx, queueKey2))
	requireTicketState(t, svcCtx, ticketStateQueued, 32084, 32085, 32086, 32087)
	require.Len(t, port.endedCalls(), 1)
}

// 补位单凑满并开局成功:每单通知 team 一次(路人不通知),名单取自票据。
func TestNotifyPartiesStartedOncePerParty(t *testing.T) {
	svcCtx, mr, port := newPartySvcCtx(t)
	const teamId = uint64(9280)
	_, tickets := popMatchedParty(t, svcCtx, mr, teamId, 32091, 32092, 32093)
	for pid, ticketId := range tickets {
		markTicketReady(svcCtx, pid, ticketId, 77020)
	}

	notifyPartiesStarted(svcCtx, []uint64{32091, 32092, 32093})

	require.Equal(t, []partyEndedCall{{teamId: teamId, members: []uint64{32091, 32092}}}, port.startedCalls())
	require.Empty(t, port.endedCalls())
	requireTicketState(t, svcCtx, ticketStateReady, 32091, 32092, 32093)

	// 没有组队服务的进程:不读票、不通知。
	svcCtx.SetPartyTeam(nil)
	notifyPartiesStarted(svcCtx, []uint64{32091, 32092, 32093})
	require.Len(t, port.startedCalls(), 1)
}

// 回队首按单位压回:某一条 Lua 失败只影响它那个单位 —— 补位单整单删票并通知一次,路人各回各的;
// 单人那一条失败只影响他一个人(与加补位之前逐人回队的失败半径相同)。
func TestRequeueFrontPushFailureAffectsOnlyThatUnit(t *testing.T) {
	// 铺出「路人、2 人队、路人、路人」已弹出并全员推进 matched 的现场;成员 id 是 base+1..base+5,队伍是 +2 / +3。
	prepare := func(t *testing.T, teamId, base uint64) (*svc.ServiceContext, *miniredis.Miniredis, *fakePartyPort, string, []uint64, map[uint64]string) {
		t.Helper()
		svcCtx, mr, port := newPartySvcCtx(t)
		queueKey := matchSizedQueueKey(pveTeamMode, partyTestConfig, 5)
		setPlayerLocation(t, mr, base+1, 1)
		require.Zero(t, joinPveTeam(t, svcCtx, base+1, partyTestConfig, 5).ErrorCode)
		enqueueParty(t, svcCtx, teamId, 5, base+2, base+3)
		for _, pid := range []uint64{base + 4, base + 5} {
			setPlayerLocation(t, mr, pid, 1)
			require.Zero(t, joinPveTeam(t, svcCtx, pid, partyTestConfig, 5).ErrorCode)
		}
		members, tickets, ok := popGroup(svcCtx, queueKey, 5)
		require.True(t, ok)
		require.Equal(t, []uint64{base + 1, base + 2, base + 3, base + 4, base + 5}, members)
		for _, pid := range members {
			written, err := setTicketMatched(svcCtx, pid, tickets[pid], matchedTicketTTLFor(svcCtx, 5))
			require.NoError(t, err)
			require.True(t, written)
		}
		return svcCtx, mr, port, queueKey, members, tickets
	}

	t.Run("补位单那一条失败", func(t *testing.T) {
		const teamId = uint64(9300)
		svcCtx, mr, port, queueKey, members, tickets := prepare(t, teamId, 32100)
		installRedisHooks(t, mr, failWhen(evalOf(pushMembersScript, idString(32102))))

		requeueFront(svcCtx, queueKey, members, tickets)

		requireNoTicket(t, svcCtx, 32102, 32103)
		requireTicketState(t, svcCtx, ticketStateQueued, 32101, 32104, 32105)
		require.Equal(t, idStrings(32101, 32104, 32105), queueMembers(t, svcCtx, queueKey), "路人按原序回到队首")
		assertQueueMirrorConsistent(t, mr, queueKey)
		require.Equal(t, []partyEndedCall{{teamId: teamId, members: []uint64{32102, 32103},
			reason: constants.PartyEndMemberNotReady}}, port.endedCalls())
	})

	t.Run("单人那一条失败", func(t *testing.T) {
		svcCtx, mr, port, queueKey, members, tickets := prepare(t, 9301, 32110)
		installRedisHooks(t, mr, failWhen(evalOf(pushMembersScript, idString(32114))))

		requeueFront(svcCtx, queueKey, members, tickets)

		requireNoTicket(t, svcCtx, 32114)
		requireTicketState(t, svcCtx, ticketStateQueued, 32111, 32112, 32113, 32115)
		require.Equal(t, idStrings(32111, 32112, 32113, 32115), queueMembers(t, svcCtx, queueKey), "只少了他一个人")
		assertQueueMirrorConsistent(t, mr, queueKey)
		require.Empty(t, port.endedCalls())
	})
}

// 两条撤单路径几乎同时收尾同一支 3 人单(两名队员同时取消、取消与 matcher 判不完整重叠):
// 本路径读到票、去删的那一瞬间,另一条路径已经把全队的票删了。没删到不等于「太迟」——
// 这一单其实已经收干净,通知照发一次;当成太迟的话两条路径各自以为该由对方通知,结果谁都不通知。
func TestCancelPartyRacingAnotherCancelStillNotifies(t *testing.T) {
	svcCtx, mr, port := newPartySvcCtx(t)
	const teamId = uint64(9310)
	queueKey := matchSizedQueueKey(pveTeamMode, partyTestConfig, 5)
	enqueueParty(t, svcCtx, teamId, 5, 32121, 32122, 32123)
	seed, err := loadTicket(svcCtx, 32121)
	require.NoError(t, err)
	require.NotNil(t, seed)
	hook, raced := onceBefore(evalOf(ticketCancelScript, ""), func() {
		for _, pid := range []uint64{32121, 32122, 32123} {
			mr.Del(matchTicketKey(pid))
		}
	})
	installRedisHooks(t, mr, hook)

	members := cancelParty(svcCtx, seed, constants.PartyEndCanceled, 32122)

	require.True(t, raced(), "应走到按 ticket id 删票那一步")
	require.Equal(t, []uint64{32121, 32122, 32123}, members)
	require.Equal(t, []partyEndedCall{{teamId: teamId, members: []uint64{32121, 32122, 32123},
		reason: constants.PartyEndCanceled, culprit: 32122}}, port.endedCalls())
	require.Empty(t, queueMembers(t, svcCtx, queueKey), "list 里的残留项一并摘掉")
	assertQueueMirrorConsistent(t, mr, queueKey)
}

// 评分队列(1v1)里每个锚点都会把其余人当候选再校验一遍:带残留备战锁、本轮被搁置的人只读一次票,
// 结论在本轮内缓存;锁消失后照常成组。
func TestDeferredMemberIsValidatedOncePerRoundInRatedQueue(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	calls := stubGather(t)
	queueKey := matchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_1V1), 0)
	for _, pid := range []uint64{32131, 32132} {
		setPlayerLocation(t, mr, pid, 1)
		require.Zero(t, joinQueue1v1(t, svcCtx, pid).ErrorCode)
	}
	members, tickets, ok := popGroup(svcCtx, queueKey, 2)
	require.True(t, ok)
	for _, pid := range members {
		written, err := setTicketMatched(svcCtx, pid, tickets[pid], matchedTicketTTLFor(svcCtx, 2))
		require.NoError(t, err)
		require.True(t, written)
	}
	const failedBattle = uint64(77030)
	requeueFrontAfterBattle(svcCtx, queueKey, members, tickets, failedBattle)
	require.NoError(t, mr.Set(playercontract.BattleLockKey(32131), idString(failedBattle)))

	var ticketReads atomic.Int32
	installRedisHooks(t, mr, func(_ *server.Peer, cmd string, args ...string) bool {
		if cmd == "HGETALL" && len(args) == 1 && args[0] == matchTicketKey(32131) {
			ticketReads.Add(1)
		}
		return false
	})
	matchQueueOnce(svcCtx, queueKey)
	select {
	case group := <-calls:
		t.Fatalf("带残留备战锁的人不应成组: %v", group.members)
	case <-time.After(200 * time.Millisecond):
	}
	require.EqualValues(t, 1, ticketReads.Load(), "被搁置的人一轮只读一次票")
	requireTicketState(t, svcCtx, ticketStateQueued, 32131, 32132)
	require.ElementsMatch(t, idStrings(32131, 32132), queueMembers(t, svcCtx, queueKey), "不摘 list")

	mr.Del(playercontract.BattleLockKey(32131))
	matchQueueOnce(svcCtx, queueKey)
	select {
	case group := <-calls:
		require.ElementsMatch(t, []uint64{32131, 32132}, group.members)
	case <-time.After(3 * time.Second):
		t.Fatal("锁消失后应成组")
	}
}

// 回队首途中有人取消(补位单逐人 CAS 回 queued 的两次往返之间):先改回来的人已经可以被取消,
// 取消方删得掉他的票、删不掉还是 matched 的队友(太迟,不通知)。整单压回之后的复读要接住它:
// 整单出局、只通知一次、原因是「某某取消」;一起回队的路人不受影响。
func TestCancelDuringRequeueWithdrawsWholeParty(t *testing.T) {
	svcCtx, mr, port := newPartySvcCtx(t)
	const teamId = uint64(9320)
	queueKey, tickets := popMatchedParty(t, svcCtx, mr, teamId, 32141, 32142, 32143)
	cancelErrs := make(chan error, 1)
	// 插在「32141 已改回 queued、32142 还是 matched」那一刻。
	hook, fired := onceBefore(evalOf(ticketCasScript, matchTicketKey(32142)), func() {
		_, err := NewCancelQueueLogic(context.Background(), svcCtx).CancelQueue(&matchpb.CancelQueueRequest{PlayerId: 32141})
		cancelErrs <- err
	})
	installRedisHooks(t, mr, hook)

	requeueFront(svcCtx, queueKey, []uint64{32141, 32142, 32143}, tickets)

	require.True(t, fired())
	require.NoError(t, <-cancelErrs)
	requireNoTicket(t, svcCtx, 32141, 32142)
	requireTicketState(t, svcCtx, ticketStateQueued, 32143)
	require.Equal(t, idStrings(32143), queueMembers(t, svcCtx, queueKey), "缺人的单不能留在队列里")
	assertQueueMirrorConsistent(t, mr, queueKey)
	require.Equal(t, []partyEndedCall{{teamId: teamId, members: []uint64{32141, 32142},
		reason: constants.PartyEndCanceled, culprit: 32141}}, port.endedCalls())
}

// 选好人、去摘的那一瞬间有路人取消:摘出人数不足,已摘出的按单位放回队首(补位单整单放回)。
// 放回那一条 Lua 失败的单位不能留成「票据 queued 但人不在队列」—— 删票让人可以立刻重排,补位单通知一次。
func TestPutBackWhenAMemberLeavesBetweenSelectAndRemove(t *testing.T) {
	// 3 人档:2 人队(base+1 / base+2)+ 路人 base+3;返回的钩子在摘出那条 Lua 之前让路人取消。
	prepare := func(t *testing.T, teamId, base uint64) (*svc.ServiceContext, *miniredis.Miniredis, *fakePartyPort, string, redisHook, func() bool) {
		t.Helper()
		svcCtx, mr, port := newPartySvcCtx(t)
		queueKey := matchSizedQueueKey(pveTeamMode, partyTestConfig, 3)
		enqueueParty(t, svcCtx, teamId, 3, base+1, base+2)
		setPlayerLocation(t, mr, base+3, 1)
		require.Zero(t, joinPveTeam(t, svcCtx, base+3, partyTestConfig, 3).ErrorCode)
		hook, fired := onceBefore(evalOf(removeQueueMembersScript, idString(base+1)), func() {
			_, _ = NewCancelQueueLogic(context.Background(), svcCtx).CancelQueue(&matchpb.CancelQueueRequest{PlayerId: base + 3})
		})
		return svcCtx, mr, port, queueKey, hook, fired
	}

	t.Run("放回成功", func(t *testing.T) {
		svcCtx, mr, port, queueKey, leave, left := prepare(t, 9370, 32190)
		installRedisHooks(t, mr, leave)

		_, _, ok := popGroup(svcCtx, queueKey, 3)

		require.True(t, left())
		require.False(t, ok)
		requireNoTicket(t, svcCtx, 32193)
		requireTicketState(t, svcCtx, ticketStateQueued, 32191, 32192)
		require.Equal(t, idStrings(32191, 32192), queueMembers(t, svcCtx, queueKey))
		assertQueueMirrorConsistent(t, mr, queueKey)
		require.Empty(t, port.endedCalls())
	})

	t.Run("放回失败", func(t *testing.T) {
		const teamId = uint64(9371)
		svcCtx, mr, port, queueKey, leave, left := prepare(t, teamId, 32200)
		installRedisHooks(t, mr, leave, failWhen(evalOf(pushMembersScript, idString(32201))))

		_, _, ok := popGroup(svcCtx, queueKey, 3)

		require.True(t, left())
		require.False(t, ok)
		requireNoTicket(t, svcCtx, 32201, 32202, 32203)
		require.Empty(t, queueMembers(t, svcCtx, queueKey))
		assertQueueMirrorConsistent(t, mr, queueKey)
		require.Equal(t, []partyEndedCall{{teamId: teamId, members: []uint64{32201, 32202},
			reason: constants.PartyEndMemberNotReady}}, port.endedCalls())
	})
}

// 战斗锁的归类:「在不在」和「是哪一局」出自同一个锁值。
func TestClassifyBattleLock(t *testing.T) {
	failed := &queueTicket{LastFailedBattleId: 77040}
	fresh := &queueTicket{}
	require.Equal(t, battleLockNone, classifyBattleLock("", failed), "没有锁")
	require.Equal(t, battleLockNone, classifyBattleLock("", fresh))
	require.Equal(t, battleLockResidual, classifyBattleLock("77040", failed), "上一局开局失败留下的备战锁")
	require.Equal(t, battleLockOther, classifyBattleLock("77041", failed), "别的战斗")
	require.Equal(t, battleLockOther, classifyBattleLock("77040", fresh), "票据没经历过开局失败:任何锁都是别的战斗")
	require.Equal(t, battleLockOther, classifyBattleLock("not-a-number", failed))
	require.Equal(t, battleLockOther, classifyBattleLock("0", fresh), "锁值 0 不能和「没失败过」的 0 对上")
	require.Equal(t, battleLockOther, classifyBattleLock("77040", nil))
}

// matcher 校验排队成员时对战斗锁只读一次(GET):先 EXISTS 再 GET 的话,锁在两次读之间消失
// 会被当成「在别的战斗里」而删票,连带整支补位单被撤。
func TestValidateReadsBattleLockOnce(t *testing.T) {
	svcCtx, mr, port := newPartySvcCtx(t)
	calls := stubPveGather(t)
	queueKey, tickets := popMatchedParty(t, svcCtx, mr, 9330, 32151, 32152, 32153)
	const failedBattle = uint64(77040)
	requeueFrontAfterBattle(svcCtx, queueKey, []uint64{32151, 32152, 32153}, tickets, failedBattle)
	lockKey := playercontract.BattleLockKey(32152)
	require.NoError(t, mr.Set(lockKey, idString(failedBattle)))
	var gets, others atomic.Int32
	installRedisHooks(t, mr, func(_ *server.Peer, cmd string, args ...string) bool {
		for _, a := range args {
			if a != lockKey {
				continue
			}
			if cmd == "GET" {
				gets.Add(1)
			} else {
				others.Add(1)
			}
		}
		return false
	})

	matchQueueOnce(svcCtx, queueKey)

	expectNoPveGather(t, calls)
	require.EqualValues(t, 1, gets.Load())
	require.Zero(t, others.Load(), "不再先 EXISTS")
	requireTicketState(t, svcCtx, ticketStateQueued, 32151, 32152, 32153)
	require.Empty(t, port.endedCalls())
}

// 名单变化的撤单落在「逐人推进 matched」的中途:已推进的那张票删不掉(太迟),通知留给 matcher 收尾 ——
// 但原因在撤单入口就登记了,收尾时说的是「名单变化」,而不是把无关的人点名成「未就绪」。
func TestTooLateCancelStillRegistersItsReason(t *testing.T) {
	svcCtx, mr, port := newPartySvcCtx(t)
	calls := stubPveGather(t)
	const teamId = uint64(9340)
	queueKey := matchSizedQueueKey(pveTeamMode, partyTestConfig, 3)
	enqueueParty(t, svcCtx, teamId, 3, 32161, 32162)
	setPlayerLocation(t, mr, 32163, 1)
	require.Zero(t, joinPveTeam(t, svcCtx, 32163, partyTestConfig, 3).ErrorCode)
	cancelErrs := make(chan error, 1)
	// 插在「32161 已是 matched、32162 还是 queued」那一刻。
	hook, fired := onceBefore(evalOf(ticketCasScript, matchTicketKey(32162)), func() {
		_, err := NewTeamBattleStarter(svcCtx).CancelParty(context.Background(), teamId, []uint64{32161, 32162}, nil,
			constants.PartyEndRosterChanged, 0)
		cancelErrs <- err
	})
	installRedisHooks(t, mr, hook)

	matchQueueOnce(svcCtx, queueKey)

	require.True(t, fired())
	require.NoError(t, <-cancelErrs)
	expectNoPveGather(t, calls)
	requireNoTicket(t, svcCtx, 32161, 32162)
	requireTicketState(t, svcCtx, ticketStateQueued, 32163)
	require.Equal(t, idStrings(32163), queueMembers(t, svcCtx, queueKey))
	require.Equal(t, []partyEndedCall{{teamId: teamId, members: []uint64{32161, 32162},
		reason: constants.PartyEndRosterChanged}}, port.endedCalls())
}

// 建票循环中途这一单被撤(迟到的名单变化撤单按 team_id 找票,设计文档 §6):已建好的票被删、后面的照常建。
// 整批入队后的复读要接住 —— 撤掉其余部分、按「该成员没能入队」返回,不留半单。全队只收到那次撤单自己的一条通知。
func TestEnqueuePartyWithdrawnMidwayLeavesNoHalfParty(t *testing.T) {
	svcCtx, mr, port := newPartySvcCtx(t)
	const teamId = uint64(9350)
	queueKey := matchSizedQueueKey(pveTeamMode, partyTestConfig, 5)
	cancelErrs := make(chan error, 1)
	// 插在「32171 的票已建好、32172 的还没建」那一刻:一次迟到的名单变化撤单(提交后的成员集合与这份名单不同)。
	partyIds := make(chan string, 1)
	hook, fired := onceBefore(evalOf(ticketCreateScript, matchTicketKey(32172)), func() {
		seed, _ := loadTicket(svcCtx, 32171)
		_, err := NewTeamBattleStarter(svcCtx).CancelParty(context.Background(), teamId, []uint64{32171},
			[]uint64{32171, 32179}, constants.PartyEndRosterChanged, 0)
		cancelErrs <- err
		// 抹掉这一单的通知去重标记:入队方的收尾若带原因去撤单,就会再通知一次;只有它确实是静默的,下面才仍然只有一条。
		if seed != nil {
			mr.Del(matchPartyEndNotifiedKey(seed.PartyId))
			partyIds <- seed.PartyId
		} else {
			partyIds <- ""
		}
	})
	installRedisHooks(t, mr, hook)

	failed, err := NewTeamBattleStarter(svcCtx).EnqueueParty(context.Background(), partyTestConfig, 5, teamId,
		[]uint64{32171, 32172}, nil)

	require.True(t, fired())
	require.NoError(t, <-cancelErrs)
	require.NotEmpty(t, <-partyIds, "钩子触发时 32171 的补位票应已建好")
	require.NoError(t, err)
	require.Equal(t, uint64(32171), failed)
	requireNoTicket(t, svcCtx, 32171, 32172)
	require.Empty(t, queueMembers(t, svcCtx, queueKey))
	assertQueueMirrorConsistent(t, mr, queueKey)
	require.Equal(t, []partyEndedCall{{teamId: teamId, members: []uint64{32171, 32172},
		reason: constants.PartyEndRosterChanged}}, port.endedCalls(), "只有那次撤单的一条;入队方的收尾是静默的")
}

// 名单彼此对不上的补位单(票据被写坏):不会成组,扫完整条队列后整单撤出,不影响别人。
func TestMalformedPartyIsEvictedAsAWhole(t *testing.T) {
	svcCtx, mr, port := newPartySvcCtx(t)
	calls := stubPveGather(t)
	const teamId = uint64(9360)
	queueKey := matchSizedQueueKey(pveTeamMode, partyTestConfig, 3)
	enqueueParty(t, svcCtx, teamId, 3, 32181, 32182)
	setPlayerLocation(t, mr, 32183, 1)
	require.Zero(t, joinPveTeam(t, svcCtx, 32183, partyTestConfig, 3).ErrorCode)
	mr.HSet(matchTicketKey(32182), ticketFieldPartyMembers, formatPartyMembers([]uint64{32181, 32182, 32199}))

	matchQueueOnce(svcCtx, queueKey)

	expectNoPveGather(t, calls)
	requireNoTicket(t, svcCtx, 32181, 32182)
	requireTicketState(t, svcCtx, ticketStateQueued, 32183)
	require.Equal(t, idStrings(32183), queueMembers(t, svcCtx, queueKey))
	assertQueueMirrorConsistent(t, mr, queueKey)
	require.Equal(t, []partyEndedCall{{teamId: teamId, members: []uint64{32181, 32182},
		reason: constants.PartyEndMemberNotReady}}, port.endedCalls())
}

// 排队中的人应战切磋并开打:立刻退出排队 —— 单排的删票出队,补位单整单撤出并通知全队(点名是他)。
// 不等 matcher 哪天扫到:人数不够凑满的队列 matcher 不扫,票据会留到切磋打完之后。
func TestChallengeBattleStartLeavesQueue(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	useTestCatalog(t, svcCtx, map[uint32]uint32{1: 5})
	port := &fakePartyPort{}
	svcCtx.SetPartyTeam(port)
	stubGatherRPCs(t, nil)
	const teamId = uint64(9380)
	const member, mate, solo, bystander = uint64(32211), uint64(32212), uint64(32213), uint64(32214)
	queueKey := matchSizedQueueKey(pveTeamMode, partyTestConfig, 5)
	for _, pid := range []uint64{member, mate, solo, bystander} {
		setPlayerLocation(t, mr, pid, 1)
	}
	enqueueParty(t, svcCtx, teamId, 5, member, mate)
	require.Zero(t, joinPveTeam(t, svcCtx, solo, partyTestConfig, 5).ErrorCode)
	require.Zero(t, joinPveTeam(t, svcCtx, bystander, partyTestConfig, 5).ErrorCode)

	require.True(t, RunChallengeGather(svcCtx, 0, member, solo), "切磋开局成功")

	requireNoTicket(t, svcCtx, member, mate, solo)
	requireTicketState(t, svcCtx, ticketStateQueued, bystander)
	require.Equal(t, idStrings(bystander), queueMembers(t, svcCtx, queueKey), "没参战的人不受影响")
	assertQueueMirrorConsistent(t, mr, queueKey)
	require.Equal(t, []partyEndedCall{{teamId: teamId, members: []uint64{member, mate},
		reason: constants.PartyEndMemberNotReady, culprit: member}}, port.endedCalls())
}

// 已经凑满弹出(matched)的票不归这条规则管:那一局自己会因他在战斗中而开局失败,按肇事者收尾。
func TestLeaveQueueForOtherBattleLeavesMatchedTicketAlone(t *testing.T) {
	svcCtx, mr, port := newPartySvcCtx(t)
	_, tickets := popMatchedParty(t, svcCtx, mr, 9381, 32221, 32222, 32223)

	leaveQueueForOtherBattle(svcCtx, 32221)
	leaveQueueForOtherBattle(svcCtx, 32223)
	leaveQueueForOtherBattle(svcCtx, 32229) // 没有票据的人:什么都不做

	for _, pid := range []uint64{32221, 32222, 32223} {
		ticket, err := loadTicket(svcCtx, pid)
		require.NoError(t, err)
		require.NotNil(t, ticket)
		require.Equal(t, ticketStateMatched, ticket.State)
		require.Equal(t, tickets[pid], ticket.Ticket)
	}
	require.Empty(t, port.endedCalls())
}

// 弹出到回队之间进了别的战斗的人(例如组被弹出、票据 matched 时他应战切磋并开打,随后这一局败在别人身上)不回队:
// 补位单整单出局并点名是他;单人只删他自己的票。锁值是刚失败那一局的备战锁不算。
func TestRequeueFrontDropsMemberWhoEnteredAnotherBattle(t *testing.T) {
	const failedBattle = uint64(77050)

	t.Run("补位单队员", func(t *testing.T) {
		svcCtx, mr, port := newPartySvcCtx(t)
		const teamId = uint64(9390)
		queueKey, tickets := popMatchedParty(t, svcCtx, mr, teamId, 32231, 32232, 32233)
		require.NoError(t, mr.Set(playercontract.BattleLockKey(32232), "99001")) // 切磋那一场

		requeueFrontAfterBattle(svcCtx, queueKey, []uint64{32231, 32232, 32233}, tickets, failedBattle)

		requireNoTicket(t, svcCtx, 32231, 32232)
		requireTicketState(t, svcCtx, ticketStateQueued, 32233)
		require.Equal(t, idStrings(32233), queueMembers(t, svcCtx, queueKey))
		assertQueueMirrorConsistent(t, mr, queueKey)
		require.Equal(t, []partyEndedCall{{teamId: teamId, members: []uint64{32231, 32232},
			reason: constants.PartyEndMemberNotReady, culprit: 32232}}, port.endedCalls())
	})

	t.Run("单人", func(t *testing.T) {
		svcCtx, mr, port := newPartySvcCtx(t)
		queueKey, tickets := popMatchedParty(t, svcCtx, mr, 9391, 32241, 32242, 32243)
		require.NoError(t, mr.Set(playercontract.BattleLockKey(32243), "99002"))

		requeueFrontAfterBattle(svcCtx, queueKey, []uint64{32241, 32242, 32243}, tickets, failedBattle)

		requireNoTicket(t, svcCtx, 32243)
		requireTicketState(t, svcCtx, ticketStateQueued, 32241, 32242)
		require.Equal(t, idStrings(32241, 32242), queueMembers(t, svcCtx, queueKey))
		require.Empty(t, port.endedCalls())
	})

	t.Run("刚失败那一局的备战锁不算", func(t *testing.T) {
		svcCtx, mr, port := newPartySvcCtx(t)
		queueKey, tickets := popMatchedParty(t, svcCtx, mr, 9392, 32251, 32252, 32253)
		require.NoError(t, mr.Set(playercontract.BattleLockKey(32252), idString(failedBattle)))

		requeueFrontAfterBattle(svcCtx, queueKey, []uint64{32251, 32252, 32253}, tickets, failedBattle)

		requireTicketState(t, svcCtx, ticketStateQueued, 32251, 32252, 32253)
		require.Equal(t, idStrings(32251, 32252, 32253), queueMembers(t, svcCtx, queueKey))
		require.Empty(t, port.endedCalls())
	})
}
