package logic

// 复审修复(cross-zone-matchmaking.md 2026-09-02 复审)的 miniredis 测试:
// requeueFront 先 CAS 后 LPUSH / 弹出后被取消的成员不进 gather / CancelQueue 的
// Lua CAS / JoinQueue 孤儿票据自愈 / 票据条件创建 / 组内去重 / 旧格式队列迁移 /
// 补偿前续期。并发时序里的"另一实例"用 requeueFrontHook / matcherAfterPopHook 插入。

import (
	"context"
	"testing"
	"time"

	"match/internal/constants"

	matchpb "proto/match"

	"github.com/stretchr/testify/require"
)

func withRequeueFrontHook(t *testing.T, fn func(playerId uint64)) {
	t.Helper()
	prev := requeueFrontHook
	requeueFrontHook = fn
	t.Cleanup(func() { requeueFrontHook = prev })
}

func withMatcherAfterPopHook(t *testing.T, fn func(members []uint64)) {
	t.Helper()
	prev := matcherAfterPopHook
	matcherAfterPopHook = fn
	t.Cleanup(func() { matcherAfterPopHook = prev })
}

// ---- requeueFront:先 CAS 回 queued,再 LPUSH ----

// 另一实例的 matcher 在 requeueFront 的两步之间弹出:旧顺序(先 LPUSH 后 CAS)会让
// 它读到 matched 而丢弃、随后 CAS 又把票据改回 queued —— 票据 queued、队列无人。
// 新顺序下窗口内票据已是 queued,不论对方是否弹到,终态都不是孤儿。
func TestRequeueFrontCasBeforePushLeavesNoOrphan(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	setPlayerLocation(t, mr, 8001, 1)
	ticketId := joinQueue1v1(t, svcCtx, 8001).QueueTicket
	queueKey := matchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_1V1), 0)
	_, err := mr.Lpop(queueKey)
	require.NoError(t, err)
	written, err := setTicketMatched(svcCtx, 8001, ticketId, matchedTicketTTLFor(svcCtx, 2))
	require.NoError(t, err)
	require.True(t, written)

	hookRan := false
	withRequeueFrontHook(t, func(playerId uint64) {
		hookRan = true
		require.Equal(t, uint64(8001), playerId)
		// 此刻票据必须已是 queued(证明顺序),队列里还没有人。
		ticket, err := loadTicket(svcCtx, 8001)
		require.NoError(t, err)
		require.Equal(t, ticketStateQueued, ticket.State)
		require.False(t, mr.Exists(queueKey))
		// 另一实例此刻弹组:弹不到人,也不会把任何票据丢弃。
		members, _, ok := popGroup(svcCtx, queueKey, 1)
		require.False(t, ok)
		require.Empty(t, members)
	})

	requeueFront(svcCtx, queueKey, []uint64{8001}, map[uint64]string{8001: ticketId})
	require.True(t, hookRan)

	ticket, err := loadTicket(svcCtx, 8001)
	require.NoError(t, err)
	require.Equal(t, ticketStateQueued, ticket.State)
	ttl, err := svcCtx.MatchRedis.Ttl(matchTicketKey(8001))
	require.NoError(t, err)
	require.Greater(t, ttl, int(svcCtx.Config.MatchedTicketTTLSeconds))
	list, err := mr.List(queueKey)
	require.NoError(t, err)
	require.Equal(t, []string{"8001"}, list, "票据 queued 就必须在队列里")

	// 回队首后再被弹出:票据 queued,正常成组而不是被丢弃。
	members, tickets, ok := popGroup(svcCtx, queueKey, 1)
	require.True(t, ok)
	require.Equal(t, []uint64{8001}, members)
	require.Equal(t, ticketId, tickets[8001])
}

// LPUSH 失败(队列 key 被占成别的类型 → WRONGTYPE):票据不能停在 queued,删票让
// 玩家可立即重排。
func TestRequeueFrontDeletesTicketWhenPushFails(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	setPlayerLocation(t, mr, 8002, 1)
	ticketId := joinQueue1v1(t, svcCtx, 8002).QueueTicket
	queueKey := matchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_1V1), 0)
	_, err := mr.Lpop(queueKey)
	require.NoError(t, err)
	written, err := setTicketMatched(svcCtx, 8002, ticketId, matchedTicketTTLFor(svcCtx, 2))
	require.NoError(t, err)
	require.True(t, written)
	require.NoError(t, mr.Set(queueKey, "not-a-list"))

	requeueFront(svcCtx, queueKey, []uint64{8002}, map[uint64]string{8002: ticketId})

	require.False(t, mr.Exists(matchTicketKey(8002)), "入队失败必须删票,不留 queued 孤儿")
	// 玩家可以立即重排(队列 key 恢复为 list 后)。
	mr.Del(queueKey)
	require.Zero(t, joinQueue1v1(t, svcCtx, 8002).ErrorCode)
}

// ---- matcher:弹出后 CAS 失败的成员出局,其余回队首 ----

// 1V1:A、B 被 popGroup 弹出后、setTicketMatched 之前,A 的 CancelQueue 到达
// (票据仍 queued → 取消成功)。A 不能进 gather;B 回队首、票据 queued + 长 TTL。
func TestMatcherDropsMemberCancelledAfterPop(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	groups := stubGather(t)
	setPlayerLocation(t, mr, 8101, 1)
	setPlayerLocation(t, mr, 8102, 2)
	ticketA := joinQueue1v1(t, svcCtx, 8101).QueueTicket
	ticketB := joinQueue1v1(t, svcCtx, 8102).QueueTicket
	queueKey := matchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_1V1), 0)

	withMatcherAfterPopHook(t, func(members []uint64) {
		require.Equal(t, []uint64{8101, 8102}, members, "同评分按等待先后弹出")
		_, err := NewCancelQueueLogic(context.Background(), svcCtx).CancelQueue(&matchpb.CancelQueueRequest{
			PlayerId: 8101, QueueTicket: ticketA,
		})
		require.NoError(t, err)
		require.False(t, mr.Exists(matchTicketKey(8101)), "弹出后、matched 前取消必须成功")
	})

	runMatcherRound(context.Background(), svcCtx)

	select {
	case call := <-groups:
		t.Fatalf("已取消的成员不得进 gather,却弹出了 %v", call.members)
	case <-time.After(200 * time.Millisecond):
	}
	require.False(t, mr.Exists(matchTicketKey(8101)), "A 的票据保持已删,玩家可重排")
	ticket, err := loadTicket(svcCtx, 8102)
	require.NoError(t, err)
	require.Equal(t, ticketB, ticket.Ticket)
	require.Equal(t, ticketStateQueued, ticket.State, "B 必须回到 queued")
	ttl, err := svcCtx.MatchRedis.Ttl(matchTicketKey(8102))
	require.NoError(t, err)
	require.Greater(t, ttl, int(svcCtx.Config.MatchedTicketTTLSeconds), "B 恢复长 TTL")
	list, err := mr.List(queueKey)
	require.NoError(t, err)
	require.Equal(t, []string{"8102"}, list, "B 回队首,A 不在队列")
	require.False(t, mr.Exists(matcherLockKey(int32(matchpb.MatchMode_MATCH_MODE_1V1), 0)), "锁必须释放")

	// A 重排后与 B 正常成组。
	require.Zero(t, joinQueue1v1(t, svcCtx, 8101).ErrorCode)
	withMatcherAfterPopHook(t, nil)
	runMatcherRound(context.Background(), svcCtx)
	select {
	case call := <-groups:
		require.ElementsMatch(t, []uint64{8101, 8102}, call.members)
	case <-time.After(3 * time.Second):
		t.Fatal("A 重排后没有成组")
	}
}

// ---- CancelQueue:删票是 (ticket id, state==queued) 的 Lua CAS ----

func TestCancelTicketIfQueuedIsCasOnState(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	setPlayerLocation(t, mr, 8201, 1)
	ticketId := joinQueue1v1(t, svcCtx, 8201).QueueTicket

	// ticket id 不一致:不删。
	deleted, err := cancelTicketIfQueued(svcCtx, 8201, "stale")
	require.NoError(t, err)
	require.False(t, deleted)
	require.True(t, mr.Exists(matchTicketKey(8201)))

	// 已被 matcher 推进 matched:不删(取消太迟在存储层成立)。
	written, err := setTicketMatched(svcCtx, 8201, ticketId, 30)
	require.NoError(t, err)
	require.True(t, written)
	deleted, err = cancelTicketIfQueued(svcCtx, 8201, ticketId)
	require.NoError(t, err)
	require.False(t, deleted)
	require.Equal(t, ticketStateMatched, mr.HGet(matchTicketKey(8201), ticketFieldState))

	// 全流程:读到 matched 的 CancelQueue 幂等返回,票据仍在。
	_, err = NewCancelQueueLogic(context.Background(), svcCtx).CancelQueue(&matchpb.CancelQueueRequest{PlayerId: 8201})
	require.NoError(t, err)
	require.True(t, mr.Exists(matchTicketKey(8201)))

	// 回到 queued 后才删得掉。
	written, err = casTicketFields(svcCtx, 8201, ticketId, 0, ticketFieldState, ticketStateQueued)
	require.NoError(t, err)
	require.True(t, written)
	deleted, err = cancelTicketIfQueued(svcCtx, 8201, ticketId)
	require.NoError(t, err)
	require.True(t, deleted)
	require.False(t, mr.Exists(matchTicketKey(8201)))
}

// ---- JoinQueue:queued 但不在队列的孤儿票据自愈 ----

// 票据 queued(6h TTL)但队列里没有人(故障切换丢 RPUSH / popGroup 与 matched 之间
// 崩溃 / requeueFront CAS 与 LPUSH 之间崩溃):JoinQueue 不再无条件 ErrAlreadyQueued,
// 清掉残留后按本次请求正常入队。
func TestJoinQueueHealsOrphanQueuedTicket(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	setPlayerLocation(t, mr, 8301, 1)
	queueKey := matchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_1V1), 0)
	mustCreateTicket(t, svcCtx, 8301, &queueTicket{
		Ticket: "orphan", Mode: int32(matchpb.MatchMode_MATCH_MODE_1V1), Config: 0,
		State: ticketStateQueued, EnqueuedAtMs: nowMs() - 60000, ZoneId: 1, QueueKey: queueKey,
	})
	require.False(t, mr.Exists(queueKey), "前置:队列里没有他")

	// 用另一个模式重排:自愈后按新请求入队(不是把旧票据补回旧队列)。
	resp, err := NewJoinQueueLogic(context.Background(), svcCtx).JoinQueue(&matchpb.JoinQueueRequest{
		PlayerId: 8301, Mode: matchpb.MatchMode_MATCH_MODE_5V5, BattleConfigId: 7,
	})
	require.NoError(t, err)
	require.Zero(t, resp.ErrorCode, "孤儿票据必须被清掉后正常入队")
	require.NotEmpty(t, resp.QueueTicket)
	require.NotEqual(t, "orphan", resp.QueueTicket)

	ticket, err := loadTicket(svcCtx, 8301)
	require.NoError(t, err)
	require.Equal(t, resp.QueueTicket, ticket.Ticket)
	require.Equal(t, int32(matchpb.MatchMode_MATCH_MODE_5V5), ticket.Mode)
	newKey := matchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_5V5), 7)
	require.Equal(t, newKey, ticket.QueueKey)
	list, err := mr.List(newKey)
	require.NoError(t, err)
	require.Equal(t, []string{"8301"}, list)
	require.False(t, mr.Exists(queueKey), "旧队列不能被塞回去")
}

// 真在队列里的 queued 票据、以及 matched 票据,JoinQueue 仍按 ErrAlreadyQueued 拒。
func TestJoinQueueStillRejectsLiveTickets(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	setPlayerLocation(t, mr, 8302, 1)
	setPlayerLocation(t, mr, 8303, 1)
	queueKey := matchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_1V1), 0)

	// 在队列里。
	first := joinQueue1v1(t, svcCtx, 8302).QueueTicket
	resp := joinQueue1v1(t, svcCtx, 8302)
	require.Equal(t, constants.ErrAlreadyQueued, resp.ErrorCode)
	require.Equal(t, first, resp.QueueTicket)
	list, err := mr.List(queueKey)
	require.NoError(t, err)
	require.Equal(t, []string{"8302"}, list, "不能塞成两份")

	// matched(已弹出,gather 在途)。
	ticketId := joinQueue1v1(t, svcCtx, 8303).QueueTicket
	_, err = mr.Lpop(queueKey) // 8302
	require.NoError(t, err)
	_, err = mr.Lpop(queueKey) // 8303
	require.NoError(t, err)
	written, err := setTicketMatched(svcCtx, 8303, ticketId, 30)
	require.NoError(t, err)
	require.True(t, written)
	resp = joinQueue1v1(t, svcCtx, 8303)
	require.Equal(t, constants.ErrAlreadyQueued, resp.ErrorCode)
	require.Equal(t, ticketStateMatched, mr.HGet(matchTicketKey(8303), ticketFieldState))

	// 旧票据(无 queue_key)、成员在旧格式 key 里:视为在途,不误判成孤儿。
	setPlayerLocation(t, mr, 8304, 1)
	writeLegacyTicket(t, mr, 8304, "tk-legacy", matchpb.MatchMode_MATCH_MODE_1V1, 0)
	_, err = mr.RPush(legacyMatchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_1V1), 0), "8304")
	require.NoError(t, err)
	resp = joinQueue1v1(t, svcCtx, 8304)
	require.Equal(t, constants.ErrAlreadyQueued, resp.ErrorCode)
	require.Equal(t, "tk-legacy", resp.QueueTicket)
}

// ---- 票据"不存在才创建" ----

func TestCreateTicketIfAbsentIsConditional(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	first := &queueTicket{Ticket: "t1", Mode: 3, Config: 0, State: ticketStateQueued, EnqueuedAtMs: 1, ZoneId: 1, QueueKey: "k"}
	created, err := createTicketIfAbsent(svcCtx, 8401, first, 100)
	require.NoError(t, err)
	require.True(t, created)
	ttl, err := svcCtx.MatchRedis.Ttl(matchTicketKey(8401))
	require.NoError(t, err)
	require.Greater(t, ttl, 0)
	require.LessOrEqual(t, ttl, 100)

	second := &queueTicket{Ticket: "t2", Mode: 5, Config: 9, State: ticketStateQueued, EnqueuedAtMs: 2, ZoneId: 2, QueueKey: "k2"}
	created, err = createTicketIfAbsent(svcCtx, 8401, second, 100)
	require.NoError(t, err)
	require.False(t, created, "已存在不得覆盖")
	require.Equal(t, "t1", mr.HGet(matchTicketKey(8401), ticketFieldTicket))
	require.Equal(t, "k", mr.HGet(matchTicketKey(8401), ticketFieldQueueKey))
	ticket, err := loadTicket(svcCtx, 8401)
	require.NoError(t, err)
	require.Equal(t, uint32(1), ticket.ZoneId)
}

// 并发 JoinQueue 的后来者:票据已存在(先到者刚创建、尚未入队)→ ErrAlreadyQueued
// 且回先到者的 ticket id;队列里只有一份。
func TestJoinQueueConcurrentDuplicateRejected(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	setPlayerLocation(t, mr, 8402, 1)
	queueKey := matchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_1V1), 0)
	// 模拟先到者:票据已创建但 enqueueAtomic 还没执行时,后来者的 loadTicket
	// 已经过了(读到 nil),直接撞 createTicketIfAbsent。
	mustCreateTicket(t, svcCtx, 8402, &queueTicket{
		Ticket: "first", Mode: int32(matchpb.MatchMode_MATCH_MODE_1V1), Config: 0,
		State: ticketStateQueued, EnqueuedAtMs: nowMs(), ZoneId: 1, QueueKey: queueKey,
	})
	resp := NewJoinQueueLogic(context.Background(), svcCtx).createTicket(8402, &queueTicket{
		Ticket: "second", Mode: int32(matchpb.MatchMode_MATCH_MODE_1V1), Config: 0,
		State: ticketStateQueued, EnqueuedAtMs: nowMs(), ZoneId: 1, QueueKey: queueKey,
	}, ticketTTLSeconds(svcCtx), "1V1")
	require.NotNil(t, resp)
	require.Equal(t, constants.ErrAlreadyQueued, resp.ErrorCode)
	require.Equal(t, "first", resp.QueueTicket)
	require.Equal(t, "first", mr.HGet(matchTicketKey(8402), ticketFieldTicket))
	require.False(t, mr.Exists(queueKey), "后来者不得入队")
}

// ---- popGroup:同一玩家两份只留一份 ----

func TestPopGroupDedupesDuplicateEntries(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	setPlayerLocation(t, mr, 8501, 1)
	setPlayerLocation(t, mr, 8502, 1)
	queueKey := matchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_1V1), 0)
	require.Zero(t, joinQueue1v1(t, svcCtx, 8501).ErrorCode)
	require.NoError(t, enqueueAtomic(svcCtx, queueKey, 8501, defaultRating)) // 多余的一份
	require.Zero(t, joinQueue1v1(t, svcCtx, 8502).ErrorCode)

	members, tickets, ok := popGroup(svcCtx, queueKey, 2)
	require.True(t, ok)
	require.Equal(t, []uint64{8501, 8502}, members, "同评分按等待先后弹出")
	require.Len(t, tickets, 2)
	require.False(t, mr.Exists(queueKey), "多余的一份被丢弃,队列弹空")
}

// ---- 旧格式队列迁移 ----

// 改造前的 `match:queue:<mode>:<config>` 里的成员搬进新队列并排在现有成员之前
// (原相对顺序不变),票据补 queue_key,旧 list 消失;新格式 key 不被当旧 key 搬。
func TestMigrateLegacyQueuesMovesMembersToFront(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	legacyKey := legacyMatchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_1V1), 0)
	newKey := matchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_1V1), 0)
	writeLegacyTicket(t, mr, 8601, "tk-a", matchpb.MatchMode_MATCH_MODE_1V1, 0)
	writeLegacyTicket(t, mr, 8602, "tk-b", matchpb.MatchMode_MATCH_MODE_1V1, 0)
	_, err := mr.RPush(legacyKey, "8601", "8602")
	require.NoError(t, err)
	setPlayerLocation(t, mr, 8603, 1)
	require.Zero(t, joinQueue1v1(t, svcCtx, 8603).ErrorCode)
	// 别的模式的旧队列也一起搬;票据缺失的成员照搬(popGroup 会丢弃)。
	legacyOther := legacyMatchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_5V5), 7)
	_, err = mr.RPush(legacyOther, "8604")
	require.NoError(t, err)

	migrateLegacyQueues(svcCtx)

	list, err := mr.List(newKey)
	require.NoError(t, err)
	require.Equal(t, []string{"8601", "8602", "8603"}, list, "旧成员排前面且保持原序")
	require.False(t, mr.Exists(legacyKey), "旧 list 必须消失")
	require.False(t, mr.Exists(legacyOther))
	otherList, err := mr.List(matchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_5V5), 7))
	require.NoError(t, err)
	require.Equal(t, []string{"8604"}, otherList)
	for _, key := range []string{newKey, matchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_5V5), 7)} {
		inIndex, err := mr.IsMember(matchQueueIndexKey, key)
		require.NoError(t, err)
		require.True(t, inIndex, "搬进的队列必须在注册集 %s", key)
	}
	require.Equal(t, newKey, mr.HGet(matchTicketKey(8601), ticketFieldQueueKey), "旧票据补 queue_key")
	require.Equal(t, newKey, mr.HGet(matchTicketKey(8602), ticketFieldQueueKey))
	require.False(t, mr.Exists(matchTicketKey(8604)), "票据缺失的成员不得被凭空造出 hash")

	// 幂等:再跑一次什么都不变。
	migrateLegacyQueues(svcCtx)
	list, err = mr.List(newKey)
	require.NoError(t, err)
	require.Equal(t, []string{"8601", "8602", "8603"}, list)

	// 搬进来的旧票据玩家能被正常凑组。
	groups := stubGather(t)
	runMatcherRound(context.Background(), svcCtx)
	select {
	case call := <-groups:
		require.Equal(t, []uint64{8601, 8602}, call.members, "同评分按等待先后弹出")
	case <-time.After(3 * time.Second):
		t.Fatal("迁移后的旧票据没有成组")
	}
}

// ---- 补偿前续期 ----

func TestExtendMatchedTicketsRefreshesOnlyOwnedTickets(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	setPlayerLocation(t, mr, 8701, 1)
	setPlayerLocation(t, mr, 8702, 1)
	ticketA := joinQueue1v1(t, svcCtx, 8701).QueueTicket
	ticketB := joinQueue1v1(t, svcCtx, 8702).QueueTicket
	queueKey := matchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_1V1), 0)
	mr.Del(queueKey)
	for pid, tk := range map[uint64]string{8701: ticketA, 8702: ticketB} {
		written, err := setTicketMatched(svcCtx, pid, tk, 30)
		require.NoError(t, err)
		require.True(t, written)
	}
	// 20s 过去:票据只剩 10s,补偿(10 人 ≈ 40s)会在中途把票据过期掉。
	mr.FastForward(20 * time.Second)
	ttl, err := svcCtx.MatchRedis.Ttl(matchTicketKey(8701))
	require.NoError(t, err)
	require.LessOrEqual(t, ttl, 10)

	extended := extendMatchedTickets(svcCtx, []uint64{8701, 8702, 8703},
		map[uint64]string{8701: ticketA, 8702: "stale", 8703: "missing"}, compensationTicketTTLFor(10))

	require.Equal(t, 1, extended)
	ttl, err = svcCtx.MatchRedis.Ttl(matchTicketKey(8701))
	require.NoError(t, err)
	require.Greater(t, ttl, 30, "续期后要盖住整个补偿窗口")
	require.LessOrEqual(t, ttl, compensationTicketTTLFor(10))
	require.Equal(t, ticketStateMatched, mr.HGet(matchTicketKey(8701), ticketFieldState), "续期不改状态")
	ttl, err = svcCtx.MatchRedis.Ttl(matchTicketKey(8702))
	require.NoError(t, err)
	require.LessOrEqual(t, ttl, 10, "ticket id 不一致的不得续期")
	require.False(t, mr.Exists(matchTicketKey(8703)))

	// 续期后的补偿收尾:requeueFront 仍能把幸存者送回队首。
	mr.FastForward(35 * time.Second)
	require.False(t, mr.Exists(matchTicketKey(8702)), "未续期的票据此刻已过期")
	requeueFront(svcCtx, queueKey, []uint64{8701, 8702}, map[uint64]string{8701: ticketA, 8702: ticketB})
	list, err := mr.List(queueKey)
	require.NoError(t, err)
	require.Equal(t, []string{"8701"}, list)
	require.Equal(t, ticketStateQueued, mr.HGet(matchTicketKey(8701), ticketFieldState))
}
