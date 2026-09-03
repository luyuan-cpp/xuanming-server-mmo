package logic

// 跨 zone 匹配(设计文档 cross-zone-matchmaking.md)的 miniredis 集成测试:
// 注册集 / Lua 入队 / JoinQueue 取 zone / matcher 跨 zone 混编 + matched 短 TTL /
// 空队列剔除 / 取消用 QueueKey / 挑战 DEL 拆分。gather 要 gRPC 到 scene/battle,
// 用 runGatherFn 换成记录器。

import (
	"context"
	"sort"
	"strconv"
	"testing"
	"time"

	"match/internal/config"
	"match/internal/constants"
	"match/internal/svc"

	matchpb "proto/match"
	smpb "proto/scene_manager"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"google.golang.org/protobuf/proto"
)

// newTestSvcCtx 直接构造 ServiceContext(NewServiceContext 需要 etcd),
// MatchRedis / SharedRedis 指向同一个 miniredis = 本地单库形态。
func newTestSvcCtx(t *testing.T) (*svc.ServiceContext, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rds := redis.MustNewRedis(redis.RedisConf{Host: mr.Addr(), Type: "node"})
	c := config.Config{
		TicketTTLSeconds:         21600,
		MatchedTicketTTLSeconds:  30,
		MatcherLockTTLSeconds:    10,
		BattleMaxDurationSeconds: 300,
	}
	return &svc.ServiceContext{
		Config:      c,
		MatchRedis:  rds,
		SharedRedis: rds,
		Redis:       rds,
		InstanceID:  "test-instance",
	}, mr
}

// setPlayerLocation 模拟 scene_manager 写 player:{id}:location。
func setPlayerLocation(t *testing.T, mr *miniredis.Miniredis, playerId uint64, zoneId uint32) {
	t.Helper()
	raw, err := proto.Marshal(&smpb.PlayerLocation{SceneId: 1, NodeId: "1", ZoneId: zoneId})
	require.NoError(t, err)
	require.NoError(t, mr.Set(getPlayerLocationKey(playerId), string(raw)))
}

// mustCreateTicket 用生产路径的"不存在才创建"写票据(测试铺数据用)。
func mustCreateTicket(t *testing.T, svcCtx *svc.ServiceContext, playerId uint64, ticket *queueTicket) {
	t.Helper()
	created, err := createTicketIfAbsent(svcCtx, playerId, ticket, ticketTTLSeconds(svcCtx))
	require.NoError(t, err)
	require.True(t, created, "测试铺数据时票据不应已存在 player=%d", playerId)
}

// gatherCall 是记录器收到的一次弹组:成员与弹出时刻的 ticket id。
type gatherCall struct {
	members []uint64
	tickets map[uint64]string
}

// stubGather 把 runGatherFn 换成记录器,返回收组 channel。
func stubGather(t *testing.T) <-chan gatherCall {
	t.Helper()
	groups := make(chan gatherCall, 8)
	prev := runGatherFn
	runGatherFn = func(_ *svc.ServiceContext, _ matchpb.MatchMode, _ uint32, members []uint64, _ bool, tickets map[uint64]string) {
		groups <- gatherCall{members: members, tickets: tickets}
	}
	t.Cleanup(func() { runGatherFn = prev })
	return groups
}

func joinQueue1v1(t *testing.T, svcCtx *svc.ServiceContext, playerId uint64) *matchpb.JoinQueueResponse {
	t.Helper()
	resp, err := NewJoinQueueLogic(context.Background(), svcCtx).JoinQueue(&matchpb.JoinQueueRequest{
		PlayerId: playerId,
		Mode:     matchpb.MatchMode_MATCH_MODE_1V1,
	})
	require.NoError(t, err)
	return resp
}

func TestEnqueueAtomicRegistersQueue(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	key := matchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_1V1), 0)

	require.NoError(t, enqueueAtomic(svcCtx, key, 1001, defaultRating))
	require.NoError(t, enqueueAtomic(svcCtx, key, 1002, defaultRating))

	inIndex, err := mr.IsMember(matchQueueIndexKey, key)
	require.NoError(t, err)
	require.True(t, inIndex, "队列 key 必须进注册集")
	list, err := mr.List(key)
	require.NoError(t, err)
	require.Equal(t, []string{"1001", "1002"}, list, "RPUSH 保持 FIFO")
}

func TestJoinQueueRejectsWithoutLocation(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)

	resp := joinQueue1v1(t, svcCtx, 2001)
	require.Equal(t, constants.ErrNotInScene, resp.ErrorCode)
	require.Empty(t, resp.QueueTicket)

	ticket, err := loadTicket(svcCtx, 2001)
	require.NoError(t, err)
	require.Nil(t, ticket, "被拒不能留票据")
	require.False(t, mr.Exists(matchQueueIndexKey), "被拒不能登记队列")
}

func TestJoinQueueRecordsZoneAndQueueKey(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	setPlayerLocation(t, mr, 2002, 2)

	resp := joinQueue1v1(t, svcCtx, 2002)
	require.Zero(t, resp.ErrorCode)
	require.NotEmpty(t, resp.QueueTicket)

	ticket, err := loadTicket(svcCtx, 2002)
	require.NoError(t, err)
	require.NotNil(t, ticket)
	require.Equal(t, uint32(2), ticket.ZoneId)
	require.Equal(t, ticketStateQueued, ticket.State)
	expectKey := matchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_1V1), 0)
	require.Equal(t, expectKey, ticket.QueueKey)
	require.Equal(t, "2", mr.HGet(matchTicketKey(2002), ticketFieldZoneId), "zone_id 必须落盘")

	list, err := mr.List(expectKey)
	require.NoError(t, err)
	require.Equal(t, []string{"2002"}, list)
	inIndex, err := mr.IsMember(matchQueueIndexKey, expectKey)
	require.NoError(t, err)
	require.True(t, inIndex)

	// 长 TTL(queued 态)。
	ttl, err := svcCtx.MatchRedis.Ttl(matchTicketKey(2002))
	require.NoError(t, err)
	require.Greater(t, ttl, 30)
}

// 两个不同 zone 的玩家在同一 1V1 队列被凑成一组:匹配池不看 zone(D1),
// 票据进 matched 且 TTL 收紧到按组大小算的 matched TTL(D5:1V1 两人取配置下限)。
func TestMatcherMixesZonesAndShortensMatchedTTL(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	groups := stubGather(t)
	setPlayerLocation(t, mr, 3001, 1)
	setPlayerLocation(t, mr, 3002, 2)
	require.Zero(t, joinQueue1v1(t, svcCtx, 3001).ErrorCode)
	require.Zero(t, joinQueue1v1(t, svcCtx, 3002).ErrorCode)

	runMatcherRound(context.Background(), svcCtx)

	select {
	case call := <-groups:
		members := append([]uint64(nil), call.members...)
		sort.Slice(members, func(i, j int) bool { return members[i] < members[j] })
		require.Equal(t, []uint64{3001, 3002}, members)
		require.Len(t, call.tickets, 2, "弹组必须把各成员 ticket id 带进 gather")
	case <-time.After(3 * time.Second):
		t.Fatal("matcher 没有把两个 zone 的玩家凑成一组")
	}

	for _, pid := range []uint64{3001, 3002} {
		ticket, err := loadTicket(svcCtx, pid)
		require.NoError(t, err)
		require.NotNil(t, ticket)
		require.Equal(t, ticketStateMatched, ticket.State)
		ttl, err := svcCtx.MatchRedis.Ttl(matchTicketKey(pid))
		require.NoError(t, err)
		require.Greater(t, ttl, 0)
		require.GreaterOrEqual(t, ttl, matchedWorstCaseSeconds(2), "TTL 必须覆盖 2 人串行 gather 最坏耗时")
		require.GreaterOrEqual(t, ttl, int(svcCtx.Config.MatchedTicketTTLSeconds), "TTL 不得低于配置下限")
		require.Less(t, ttl, int(svcCtx.Config.TicketTTLSeconds), "matched 态必须收紧,不能还是长 TTL")
	}
	zones := map[string]bool{
		mr.HGet(matchTicketKey(3001), ticketFieldZoneId): true,
		mr.HGet(matchTicketKey(3002), ticketFieldZoneId): true,
	}
	require.Len(t, zones, 2, "两张票据记录了不同 zone")

	// 队列弹空;锁按持有者释放。
	require.False(t, mr.Exists(matchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_1V1), 0)))
	require.False(t, mr.Exists(matcherLockKey(int32(matchpb.MatchMode_MATCH_MODE_1V1), 0)))
}

// 凑不满的队列不动,回队首经 requeueFront 恢复 queued + 长 TTL。
func TestRequeueFrontRestoresQueuedAndLongTTL(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	setPlayerLocation(t, mr, 3101, 1)
	require.Zero(t, joinQueue1v1(t, svcCtx, 3101).ErrorCode)
	queueKey := matchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_1V1), 0)

	ticketId := mr.HGet(matchTicketKey(3101), ticketFieldTicket)
	written, err := setTicketMatched(svcCtx, 3101, ticketId, matchedTicketTTLFor(svcCtx, 2))
	require.NoError(t, err)
	require.True(t, written)
	// 模拟弹出:列表清空,注册集被别的实例剔除。
	mr.Del(queueKey)
	_, err = mr.SRem(matchQueueIndexKey, queueKey)
	require.NoError(t, err)

	requeueFront(svcCtx, "unused-fallback", []uint64{3101}, map[uint64]string{3101: ticketId})

	ticket, err := loadTicket(svcCtx, 3101)
	require.NoError(t, err)
	require.Equal(t, ticketStateQueued, ticket.State)
	ttl, err := svcCtx.MatchRedis.Ttl(matchTicketKey(3101))
	require.NoError(t, err)
	require.Greater(t, ttl, int(svcCtx.Config.MatchedTicketTTLSeconds))
	list, err := mr.List(queueKey)
	require.NoError(t, err)
	require.Equal(t, []string{"3101"}, list, "用票据 QueueKey 回队,不用 fallback")
	inIndex, err := mr.IsMember(matchQueueIndexKey, queueKey)
	require.NoError(t, err)
	require.True(t, inIndex, "回队首必须重新登记注册集")
}

func TestMatcherPrunesEmptyQueueFromIndex(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	stubGather(t)
	emptyKey := matchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_1V1), 0)
	liveKey := matchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_1V1), 9)
	_, err := mr.SetAdd(matchQueueIndexKey, emptyKey)
	require.NoError(t, err)
	setPlayerLocation(t, mr, 4001, 1)
	mustCreateTicket(t, svcCtx, 4001, &queueTicket{
		Ticket: "t", Mode: int32(matchpb.MatchMode_MATCH_MODE_1V1), Config: 9,
		State: ticketStateQueued, EnqueuedAtMs: nowMs(), ZoneId: 1, QueueKey: liveKey,
	})
	require.NoError(t, enqueueAtomic(svcCtx, liveKey, 4001, defaultRating))

	runMatcherRound(context.Background(), svcCtx)

	inIndex, err := mr.IsMember(matchQueueIndexKey, emptyKey)
	require.NoError(t, err)
	require.False(t, inIndex, "空队列必须被剔除")
	inIndex, err = mr.IsMember(matchQueueIndexKey, liveKey)
	require.NoError(t, err)
	require.True(t, inIndex, "有人的队列(凑不满)必须留在注册集")
}

func TestCancelQueueUsesTicketQueueKey(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	// 票据里的 QueueKey 故意与 (mode, config) 重算值不同,证明出队走票据。
	customKey := "match:{mq}:queue:3:777"
	mustCreateTicket(t, svcCtx, 5001, &queueTicket{
		Ticket: "tk-5001", Mode: int32(matchpb.MatchMode_MATCH_MODE_1V1), Config: 0,
		State: ticketStateQueued, EnqueuedAtMs: nowMs(), ZoneId: 1, QueueKey: customKey,
	})
	require.NoError(t, enqueueAtomic(svcCtx, customKey, 5001, defaultRating))

	_, err := NewCancelQueueLogic(context.Background(), svcCtx).CancelQueue(&matchpb.CancelQueueRequest{
		PlayerId: 5001, QueueTicket: "tk-5001",
	})
	require.NoError(t, err)

	require.False(t, mr.Exists(matchTicketKey(5001)))
	require.False(t, mr.Exists(customKey), "玩家应从票据记录的队列 key 出队")
}

// writeLegacyTicket 模拟改造前实例写的票据:无 queue_key / zone_id 字段。
func writeLegacyTicket(t *testing.T, mr *miniredis.Miniredis, playerId uint64, ticketId string, mode matchpb.MatchMode, config uint32) {
	t.Helper()
	key := matchTicketKey(playerId)
	mr.HSet(key, ticketFieldTicket, ticketId)
	mr.HSet(key, ticketFieldMode, strconv.Itoa(int(mode)))
	mr.HSet(key, ticketFieldConfig, strconv.FormatUint(uint64(config), 10))
	mr.HSet(key, ticketFieldState, ticketStateQueued)
	mr.HSet(key, ticketFieldEnqueuedAt, strconv.FormatUint(nowMs(), 10))
	mr.SetTTL(key, 21600*time.Second)
}

// 旧票据没有 queue_key 字段,成员在**真正的旧格式** key `match:queue:<mode>:<config>`
// (无 hash tag、不在注册集)里:CancelQueue 必须把旧 list 里的成员也摘掉,
// 否则无 TTL 的旧 list 永久残留。新格式 key 上若也有一份(迁移后)同样摘掉。
func TestCancelQueueLegacyTicketFallsBack(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	legacyKey := legacyMatchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_1V1), 0)
	require.Equal(t, "match:queue:3:0", legacyKey, "必须是改造前的无 tag 格式")
	newKey := matchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_1V1), 0)
	writeLegacyTicket(t, mr, 5002, "tk-5002", matchpb.MatchMode_MATCH_MODE_1V1, 0)
	_, err := mr.RPush(legacyKey, "5002")
	require.NoError(t, err)
	_, err = mr.RPush(newKey, "5002")
	require.NoError(t, err)

	_, err = NewCancelQueueLogic(context.Background(), svcCtx).CancelQueue(&matchpb.CancelQueueRequest{PlayerId: 5002})
	require.NoError(t, err)
	require.False(t, mr.Exists(matchTicketKey(5002)))
	require.False(t, mr.Exists(legacyKey), "旧格式 list 里的成员必须被摘掉")
	require.False(t, mr.Exists(newKey), "新格式 key 上的一份同样摘掉")
}

// 挑战记录双 key DEL 拆成两条单 key DEL(D7),两 key 都要删掉。
func TestDeleteChallengeRecordSplitsDel(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	mr.HSet(challengeKey(9001), challengeFieldTarget, "6002")
	require.NoError(t, mr.Set(challengeTargetKey(6002), "9001"))

	deleteChallengeRecord(svcCtx, 9001, 6002)

	require.False(t, mr.Exists(challengeKey(9001)))
	require.False(t, mr.Exists(challengeTargetKey(6002)))
}
