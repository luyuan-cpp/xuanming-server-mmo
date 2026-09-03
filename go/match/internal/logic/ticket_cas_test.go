package logic

// 审计修复(cross-zone-matchmaking.md D5 / D5c)的 miniredis 测试:
// matched TTL 按组大小计算 / 票据状态写入带 ticket id 的 Lua CAS /
// queue_depth 只由持锁实例上报且剔除归零。

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"match/internal/svc"

	matchpb "proto/match"

	"github.com/stretchr/testify/require"
)

// matchedWorstCaseSeconds 是 D5 公式里"required 人串行 gather 最坏耗时 + 10s"
// 的独立算法(测试不复用被测函数的表达式,用 gather.go / spectate.go 常量重新拼):
// 每人观战清退(RemoveObserver 3s)+ PrepareBattle 3s,全齐后 CreateBattle 5s,
// CreateBattle 失败先 DestroyBattle 3s 才进补偿。
func matchedWorstCaseSeconds(required int) int {
	perMember := int(removeObserverTimeout/time.Second) + int(prepareBattleTimeout/time.Second)
	return required*perMember + int(createBattleTimeout/time.Second) + int(rollbackTimeout/time.Second) + 10
}

// compensationWorstCaseSeconds 补偿路径最坏耗时:已冻结者逐人 CancelBattlePrepare + 余量。
func compensationWorstCaseSeconds(prepared int) int {
	return prepared*int(rollbackTimeout/time.Second) + 10
}

// depthRecorder 把 setQueueDepthFn 换成记录器:按 (mode, config) 记下所有上报值序列。
type depthRecorder struct {
	mu   sync.Mutex
	seen map[string][]int
}

func stubQueueDepth(t *testing.T) *depthRecorder {
	t.Helper()
	rec := &depthRecorder{seen: map[string][]int{}}
	prev := setQueueDepthFn
	setQueueDepthFn = func(mode string, config string, depth int) {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.seen[mode+"/"+config] = append(rec.seen[mode+"/"+config], depth)
	}
	t.Cleanup(func() { setQueueDepthFn = prev })
	return rec
}

func (r *depthRecorder) values(mode matchpb.MatchMode, config uint32) []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.seen[mode.String()+"/"+strconv.FormatUint(uint64(config), 10)]...)
}

func joinQueue(t *testing.T, svcCtx *svc.ServiceContext, playerId uint64, mode matchpb.MatchMode, config uint32) {
	t.Helper()
	resp, err := NewJoinQueueLogic(context.Background(), svcCtx).JoinQueue(&matchpb.JoinQueueRequest{
		PlayerId:       playerId,
		Mode:           mode,
		BattleConfigId: config,
	})
	require.NoError(t, err)
	require.Zero(t, resp.ErrorCode)
}

// ---- D5:matched TTL 按组大小计算 ----

func TestMatchedTicketTTLFormula(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	// PVE solo 一人:最坏 6+5+3+10=24s < 配置 30s,取配置下限。
	require.Equal(t, 30, matchedTicketTTLFor(svcCtx, 1))
	// 1V1 两人:2×6+5+3+10=30s,恰好等于配置下限。
	require.Equal(t, 30, matchedTicketTTLFor(svcCtx, 2))
	// PVE_TEAM 五人:5×6+18=48s > 30s。
	require.Equal(t, matchedWorstCaseSeconds(5), matchedTicketTTLFor(svcCtx, 5))
	require.Equal(t, 48, matchedTicketTTLFor(svcCtx, 5))
	// 5v5 十人:10×6+5+3+10=78s,旧公式 45s 盖不住"十人都在观战"的成功路径(65s)。
	require.Equal(t, matchedWorstCaseSeconds(10), matchedTicketTTLFor(svcCtx, 10))
	require.Equal(t, 78, matchedTicketTTLFor(svcCtx, 10))
	require.Greater(t, matchedTicketTTLFor(svcCtx, 10),
		10*int((removeObserverTimeout+prepareBattleTimeout)/time.Second)+int(createBattleTimeout/time.Second),
		"必须覆盖含观战清退的成功路径")
	// 配置抬高到 120s 时所有组都取配置。
	svcCtx.Config.MatchedTicketTTLSeconds = 120
	require.Equal(t, 120, matchedTicketTTLFor(svcCtx, 10))
	// 漏配(0)退回 30s 下限。
	svcCtx.Config.MatchedTicketTTLSeconds = 0
	require.Equal(t, 30, matchedTicketTTLFor(svcCtx, 1))

	// 补偿窗口:十人全冻结后逐人解冻 10×3+10=40s。
	require.Equal(t, compensationWorstCaseSeconds(10), compensationTicketTTLFor(10))
	require.Equal(t, 40, compensationTicketTTLFor(10))
}

// 5V5 十人弹组:matched TTL ≥ 10×(3+3)+5+3+10=78s 且 ≥ 配置值,固定 30s / 旧公式 45s
// 都会在正常 gather 途中过期。
func TestMatcher5v5MatchedTTLCoversWorstCaseGather(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	groups := stubGather(t)
	var members []uint64
	for i := 0; i < required5v5Players; i++ {
		pid := uint64(5100 + i)
		setPlayerLocation(t, mr, pid, uint32(1+i%3))
		joinQueue(t, svcCtx, pid, matchpb.MatchMode_MATCH_MODE_5V5, 7)
		members = append(members, pid)
	}
	require.Equal(t, uint32(required5v5Players), requiredPlayers(svcCtx, int32(matchpb.MatchMode_MATCH_MODE_5V5), 7))

	runMatcherRound(context.Background(), svcCtx)

	select {
	case call := <-groups:
		require.Equal(t, members, call.members, "同评分(全员默认 1500)仍按等待时间先后弹出")
		require.Len(t, call.tickets, required5v5Players)
	case <-time.After(3 * time.Second):
		t.Fatal("matcher 没有把 10 人凑成一组")
	}

	for _, pid := range members {
		ticket, err := loadTicket(svcCtx, pid)
		require.NoError(t, err)
		require.NotNil(t, ticket)
		require.Equal(t, ticketStateMatched, ticket.State)
		ttl, err := svcCtx.MatchRedis.Ttl(matchTicketKey(pid))
		require.NoError(t, err)
		require.GreaterOrEqual(t, ttl, matchedWorstCaseSeconds(required5v5Players))
		require.GreaterOrEqual(t, ttl, int(svcCtx.Config.MatchedTicketTTLSeconds))
		require.Greater(t, ttl, int(svcCtx.Config.MatchedTicketTTLSeconds), "十人组必须超过 30s 配置下限")
	}
}

// ---- D5:票据状态 CAS ----

// 票据被替换后,旧 gather 的 markTicketReady / requeueFront / 删票都不动新票据。
// 场景:玩家 A 被弹出(matched,旧 ticket),matched TTL 到期票据消失,玩家重排拿到
// 新 ticket(queued,已在队列里);此时旧实例的 gather 迟到收尾。
func TestTicketCasRejectsStaleGatherWrites(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	setPlayerLocation(t, mr, 6001, 1)
	queueKey := matchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_1V1), 0)

	oldTicket := joinQueue1v1(t, svcCtx, 6001).QueueTicket
	require.NotEmpty(t, oldTicket)
	// 模拟弹出:出队 + matched。
	_, err := mr.Lpop(queueKey)
	require.NoError(t, err)
	written, err := setTicketMatched(svcCtx, 6001, oldTicket, matchedTicketTTLFor(svcCtx, 2))
	require.NoError(t, err)
	require.True(t, written)

	// matched TTL 到期,票据自灭;玩家重排拿到新 ticket。
	mr.FastForward(time.Duration(matchedTicketTTLFor(svcCtx, 2)+1) * time.Second)
	require.False(t, mr.Exists(matchTicketKey(6001)), "matched 票据到期必须自灭")
	newTicket := joinQueue1v1(t, svcCtx, 6001).QueueTicket
	require.NotEmpty(t, newTicket)
	require.NotEqual(t, oldTicket, newTicket)
	list, err := mr.List(queueKey)
	require.NoError(t, err)
	require.Equal(t, []string{"6001"}, list)

	assertNewTicketUntouched := func(step string) {
		t.Helper()
		ticket, err := loadTicket(svcCtx, 6001)
		require.NoError(t, err)
		require.NotNil(t, ticket, step)
		require.Equal(t, newTicket, ticket.Ticket, step)
		require.Equal(t, ticketStateQueued, ticket.State, step)
		require.Empty(t, mr.HGet(matchTicketKey(6001), ticketFieldBattleId), step)
		ttl, err := svcCtx.MatchRedis.Ttl(matchTicketKey(6001))
		require.NoError(t, err)
		require.Greater(t, ttl, int(svcCtx.Config.ReadyTicketTTLSeconds)+1, step+":TTL 不能被 ready 收紧")
		list, err := mr.List(queueKey)
		require.NoError(t, err)
		require.Equal(t, []string{"6001"}, list, step+":队列里不能塞成两份")
	}

	// 旧 gather 迟到:matched 再写一次 → 拒。
	written, err = setTicketMatched(svcCtx, 6001, oldTicket, 30)
	require.NoError(t, err)
	require.False(t, written)
	assertNewTicketUntouched("stale matched")

	// 旧 gather 成功收尾:ready → 拒。
	markTicketReady(svcCtx, 6001, oldTicket, 987654321)
	assertNewTicketUntouched("stale ready")

	// 旧 gather 失败收尾:回队首 → 不入队不改票。
	requeueFront(svcCtx, queueKey, []uint64{6001}, map[uint64]string{6001: oldTicket})
	assertNewTicketUntouched("stale requeue")

	// 旧 gather 判定它是肇事者删票 → 拒。
	deleteTicketIfOwned(svcCtx, 6001, oldTicket)
	assertNewTicketUntouched("stale delete")

	// 缺少 ticket id 的成员回队首也不动(防御:调用方漏传)。
	requeueFront(svcCtx, queueKey, []uint64{6001}, nil)
	assertNewTicketUntouched("missing expected")
}

// CAS 正向:ticket id 一致时 ready 同时写 state + battle_id 并收紧 TTL;
// 回队首恢复 queued + 长 TTL;删票生效。
func TestTicketCasAcceptsMatchingTicket(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	svcCtx.Config.ReadyTicketTTLSeconds = 60
	setPlayerLocation(t, mr, 6002, 2)
	queueKey := matchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_1V1), 0)
	ticketId := joinQueue1v1(t, svcCtx, 6002).QueueTicket
	_, err := mr.Lpop(queueKey)
	require.NoError(t, err)

	written, err := setTicketMatched(svcCtx, 6002, ticketId, matchedTicketTTLFor(svcCtx, 2))
	require.NoError(t, err)
	require.True(t, written)

	markTicketReady(svcCtx, 6002, ticketId, 424242)
	ticket, err := loadTicket(svcCtx, 6002)
	require.NoError(t, err)
	require.Equal(t, ticketStateReady, ticket.State)
	require.Equal(t, "424242", mr.HGet(matchTicketKey(6002), ticketFieldBattleId))
	ttl, err := svcCtx.MatchRedis.Ttl(matchTicketKey(6002))
	require.NoError(t, err)
	require.Greater(t, ttl, 0)
	require.LessOrEqual(t, ttl, 60)

	requeueFront(svcCtx, "unused-fallback", []uint64{6002}, map[uint64]string{6002: ticketId})
	ticket, err = loadTicket(svcCtx, 6002)
	require.NoError(t, err)
	require.Equal(t, ticketStateQueued, ticket.State)
	ttl, err = svcCtx.MatchRedis.Ttl(matchTicketKey(6002))
	require.NoError(t, err)
	require.Greater(t, ttl, 60)
	list, err := mr.List(queueKey)
	require.NoError(t, err)
	require.Equal(t, []string{"6002"}, list)

	deleteTicketIfOwned(svcCtx, 6002, ticketId)
	require.False(t, mr.Exists(matchTicketKey(6002)))
}

// ---- D5c:queue_depth 单实例上报 + 归零 ----

// 凑不满(1 人在 1V1 队列):持锁实例照样上报深度 1;队列继续留在注册集。
func TestQueueDepthReportedByLockHolderWhenShort(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	stubGather(t)
	rec := stubQueueDepth(t)
	setPlayerLocation(t, mr, 7001, 1)
	require.Zero(t, joinQueue1v1(t, svcCtx, 7001).ErrorCode)

	runMatcherRound(context.Background(), svcCtx)

	require.Equal(t, []int{1}, rec.values(matchpb.MatchMode_MATCH_MODE_1V1, 0))
	require.False(t, mr.Exists(matcherLockKey(int32(matchpb.MatchMode_MATCH_MODE_1V1), 0)), "锁必须释放")
	inIndex, err := mr.IsMember(matchQueueIndexKey, matchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_1V1), 0))
	require.NoError(t, err)
	require.True(t, inIndex)
}

// 锁被别的实例持有:本实例不上报、不弹组、不释放别人的锁。
func TestQueueDepthNotReportedWithoutLock(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	groups := stubGather(t)
	rec := stubQueueDepth(t)
	setPlayerLocation(t, mr, 7002, 1)
	setPlayerLocation(t, mr, 7003, 2)
	require.Zero(t, joinQueue1v1(t, svcCtx, 7002).ErrorCode)
	require.Zero(t, joinQueue1v1(t, svcCtx, 7003).ErrorCode)
	lockKey := matcherLockKey(int32(matchpb.MatchMode_MATCH_MODE_1V1), 0)
	require.NoError(t, mr.Set(lockKey, "other-instance"))

	runMatcherRound(context.Background(), svcCtx)

	require.Empty(t, rec.values(matchpb.MatchMode_MATCH_MODE_1V1, 0), "拿不到锁不得上报 queue_depth")
	select {
	case call := <-groups:
		t.Fatalf("拿不到锁不得弹组,却弹出了 %v", call.members)
	default:
	}
	got, err := mr.Get(lockKey)
	require.NoError(t, err)
	require.Equal(t, "other-instance", got, "不得释放别的实例的锁")

	// 对方释放后本实例接管:上报 2 并弹组。
	mr.Del(lockKey)
	runMatcherRound(context.Background(), svcCtx)
	require.Equal(t, []int{2}, rec.values(matchpb.MatchMode_MATCH_MODE_1V1, 0))
	select {
	case call := <-groups:
		require.Len(t, call.members, 2)
	case <-time.After(3 * time.Second):
		t.Fatal("接管后没有弹组")
	}
}

// 队列被 pruneQueueScript 从注册集剔除时 gauge 归零。
func TestQueueDepthZeroedWhenQueuePruned(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	stubGather(t)
	rec := stubQueueDepth(t)
	emptyKey := matchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_5V5), 3)
	_, err := mr.SetAdd(matchQueueIndexKey, emptyKey)
	require.NoError(t, err)

	runMatcherRound(context.Background(), svcCtx)

	// 唯一成员被剔除后注册集本身也随之消失(空 set 在 Redis 里不存在)。
	if mr.Exists(matchQueueIndexKey) {
		inIndex, err := mr.IsMember(matchQueueIndexKey, emptyKey)
		require.NoError(t, err)
		require.False(t, inIndex, "空队列必须被剔除")
	}
	values := rec.values(matchpb.MatchMode_MATCH_MODE_5V5, 3)
	require.NotEmpty(t, values)
	require.Equal(t, 0, values[len(values)-1], "剔除后 gauge 最后一次写入必须是 0")
	for _, v := range values {
		require.Equal(t, 0, v)
	}
}
