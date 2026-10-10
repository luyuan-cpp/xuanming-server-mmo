package logic

// PVE 人数档匹配(设计文档 docs/design/pve-team-size-matching.md)的 miniredis 集成测试:
// 队列 key 的人数段往返与同 slot / 各人数档互不串队 / 1 人档即时开战 / 不填人数 = 副本上限 /
// 超上限与未开放的拒绝 / 上限调低后残留队列不再弹组 / 取消出队 / 指标按人数档分开上报。
// gather 要 gRPC 到 scene / battle,用 runGatherFn 换成记录器。

import (
	"context"
	"sync"
	"testing"
	"time"

	"match/internal/constants"
	"match/internal/svc"

	matchpb "proto/match"

	"github.com/stretchr/testify/require"
)

const pveTeamMode = int32(matchpb.MatchMode_MATCH_MODE_PVE_TEAM)

// pveGatherCall 记录器收到的一次开局调用。比 crosszone_test 的 gatherCall 多记模式、配置与
// "失败是否回队列":人数档测试要区分即时开战(不回队列)与队列凑单(回队列)。
type pveGatherCall struct {
	mode    matchpb.MatchMode
	config  uint32
	members []uint64
	requeue bool
}

func stubPveGather(t *testing.T) <-chan pveGatherCall {
	t.Helper()
	calls := make(chan pveGatherCall, 8)
	prev := runGatherFn
	runGatherFn = func(_ *svc.ServiceContext, mode matchpb.MatchMode, config uint32, members []uint64, requeue bool, _ map[uint64]string) {
		calls <- pveGatherCall{mode: mode, config: config, members: members, requeue: requeue}
	}
	t.Cleanup(func() { runGatherFn = prev })
	return calls
}

func expectPveGather(t *testing.T, calls <-chan pveGatherCall) pveGatherCall {
	t.Helper()
	select {
	case call := <-calls:
		return call
	case <-time.After(3 * time.Second):
		t.Fatal("没有等到开局调用")
		return pveGatherCall{}
	}
}

func expectNoPveGather(t *testing.T, calls <-chan pveGatherCall) {
	t.Helper()
	select {
	case call := <-calls:
		t.Fatalf("不应开局,却开了 %v", call.members)
	case <-time.After(200 * time.Millisecond):
	}
}

// joinPveTeam 以 PVE_TEAM 模式入队;teamSize 是请求里的人数档(0 = 不填)。
func joinPveTeam(t *testing.T, svcCtx *svc.ServiceContext, playerId uint64, config uint32, teamSize uint32) *matchpb.JoinQueueResponse {
	t.Helper()
	resp, err := NewJoinQueueLogic(context.Background(), svcCtx).JoinQueue(&matchpb.JoinQueueRequest{
		PlayerId:       playerId,
		Mode:           matchpb.MatchMode_MATCH_MODE_PVE_TEAM,
		BattleConfigId: config,
		TeamSize:       teamSize,
	})
	require.NoError(t, err)
	return resp
}

func queueMembers(t *testing.T, svcCtx *svc.ServiceContext, queueKey string) []string {
	t.Helper()
	members, err := svcCtx.MatchRedis.Lrange(queueKey, 0, -1)
	require.NoError(t, err)
	return members
}

// 人数档队列 key:(mode, config) 仍能从尾部解析,人数从人数段读回,rank / lock key 带同一个人数段;
// 不带人数段的 key 人数为 0,推出的锁 key 与 matcherLockKey 逐字相同(新旧实例混跑抢同一把锁)。
func TestSizedQueueKeyRoundTrip(t *testing.T) {
	for _, size := range []uint32{2, 3, 4, 5} {
		key := matchSizedQueueKey(pveTeamMode, 7, size)
		mode, config, err := parseQueueKey(key)
		require.NoError(t, err, key)
		require.Equal(t, pveTeamMode, mode, key)
		require.Equal(t, uint32(7), config, key)
		require.Equal(t, size, queueTeamSize(key), key)

		rankKey, err := rankKeyForQueue(key)
		require.NoError(t, err)
		lockKey, err := lockKeyForQueue(key)
		require.NoError(t, err)
		require.NotEqual(t, key, rankKey)
		require.NotEqual(t, key, lockKey)
		require.NotEqual(t, rankKey, lockKey)
		// rank / lock 不是队列 key 形态,读不出人数,也不会被当成队列弹组。
		require.Zero(t, queueTeamSize(rankKey), rankKey)
		require.Zero(t, queueTeamSize(lockKey), lockKey)

		// 三类 key 与注册集同 slot:enqueue / requeue / prune 的多 key Lua 在集群下不 CROSSSLOT。
		want := redisHashSlot(matchQueueIndexKey)
		require.Equal(t, want, redisHashSlot(key), key)
		require.Equal(t, want, redisHashSlot(rankKey), rankKey)
		require.Equal(t, want, redisHashSlot(lockKey), lockKey)

		// 不同人数档、以及同副本不带人数段的队列,key 两两不同。
		require.NotEqual(t, key, matchSizedQueueKey(pveTeamMode, 7, size+1))
		require.NotEqual(t, key, matchQueueKey(pveTeamMode, 7))
	}

	for _, c := range []struct {
		mode   int32
		config uint32
	}{{3, 0}, {1, 0}, {5, 1}} {
		plain := matchQueueKey(c.mode, c.config)
		require.Zero(t, queueTeamSize(plain), plain)
		lockKey, err := lockKeyForQueue(plain)
		require.NoError(t, err)
		require.Equal(t, matcherLockKey(c.mode, c.config), lockKey)
		require.Zero(t, queueTeamSize(legacyMatchQueueKey(c.mode, c.config)))
	}

	// 异物:不是队列 key 就读不出人数,也推不出锁 key。
	require.Zero(t, queueTeamSize(""))
	require.Zero(t, queueTeamSize("match:{mq}:sizeX:queue:5:1"))
	require.Zero(t, queueTeamSize("match:{mq}:size3:index"))
	_, err := lockKeyForQueue("match:{mq}:index")
	require.Error(t, err)
}

// 凑满人数口径:人数档以 key 为准但不超过副本上限;不带人数段的 PVE_TEAM 队列(存量)按上限凑满。
func TestRequiredPlayersForQueue(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	svcCtx.Config.PveTeamSizeByConfigId = map[string]uint32{"1": 4, "2": 10}

	require.Equal(t, uint32(2), requiredPlayersForQueue(svcCtx, matchSizedQueueKey(pveTeamMode, 1, 2), pveTeamMode, 1))
	require.Equal(t, uint32(4), requiredPlayersForQueue(svcCtx, matchSizedQueueKey(pveTeamMode, 1, 4), pveTeamMode, 1))
	require.Zero(t, requiredPlayersForQueue(svcCtx, matchSizedQueueKey(pveTeamMode, 1, 5), pveTeamMode, 1), "超过副本上限的人数档不弹组")
	require.Equal(t, uint32(kMaxBattleTeamSize), requiredPlayersForQueue(svcCtx, matchSizedQueueKey(pveTeamMode, 2, 5), pveTeamMode, 2), "配置 10 按引擎上限 5 收口")
	require.Zero(t, requiredPlayersForQueue(svcCtx, matchSizedQueueKey(pveTeamMode, 99, 2), pveTeamMode, 99), "未配置的副本")

	require.Equal(t, uint32(4), requiredPlayersForQueue(svcCtx, matchQueueKey(pveTeamMode, 1), pveTeamMode, 1), "存量队列按上限凑满")
	mode1v1 := int32(matchpb.MatchMode_MATCH_MODE_1V1)
	require.Equal(t, uint32(2), requiredPlayersForQueue(svcCtx, matchQueueKey(mode1v1, 0), mode1v1, 0))
	require.Zero(t, requiredPlayersForQueue(svcCtx, matchSizedQueueKey(mode1v1, 0, 2), mode1v1, 0), "只有 PVE_TEAM 有人数档")
}

// 同一副本的 2 人档与 3 人档互不串队:各凑各的,先满先开。
func TestPveTeamSizeTiersQueueSeparately(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	svcCtx.Config.PveTeamSizeByConfigId = map[string]uint32{"1": 5}
	calls := stubPveGather(t)
	for pid := uint64(21001); pid <= 21005; pid++ {
		setPlayerLocation(t, mr, pid, 1)
	}
	queue2 := matchSizedQueueKey(pveTeamMode, 1, 2)
	queue3 := matchSizedQueueKey(pveTeamMode, 1, 3)

	require.Zero(t, joinPveTeam(t, svcCtx, 21001, 1, 2).ErrorCode)
	require.Zero(t, joinPveTeam(t, svcCtx, 21002, 1, 3).ErrorCode)
	require.Zero(t, joinPveTeam(t, svcCtx, 21003, 1, 3).ErrorCode)
	require.Equal(t, []string{"21001"}, queueMembers(t, svcCtx, queue2))
	require.Equal(t, []string{"21002", "21003"}, queueMembers(t, svcCtx, queue3))
	ticket, err := loadTicket(svcCtx, 21001)
	require.NoError(t, err)
	require.Equal(t, queue2, ticket.QueueKey)
	require.Equal(t, ticketStateQueued, ticket.State)

	// 两档都没满:总人数 3 够一个 3 人组,但不同档不能互相凑。
	runMatcherRound(context.Background(), svcCtx)
	expectNoPveGather(t, calls)
	require.Equal(t, []string{"21001"}, queueMembers(t, svcCtx, queue2))
	require.Equal(t, []string{"21002", "21003"}, queueMembers(t, svcCtx, queue3))

	// 2 人档来了第二个人:只开 2 人档,3 人档原样等着。
	require.Zero(t, joinPveTeam(t, svcCtx, 21004, 1, 2).ErrorCode)
	runMatcherRound(context.Background(), svcCtx)
	call := expectPveGather(t, calls)
	require.Equal(t, []uint64{21001, 21004}, call.members)
	require.Equal(t, matchpb.MatchMode_MATCH_MODE_PVE_TEAM, call.mode)
	require.Equal(t, uint32(1), call.config)
	require.True(t, call.requeue, "队列凑单失败要回队首")
	expectNoPveGather(t, calls)
	require.Empty(t, queueMembers(t, svcCtx, queue2))
	require.Equal(t, []string{"21002", "21003"}, queueMembers(t, svcCtx, queue3))
	ticket, err = loadTicket(svcCtx, 21004)
	require.NoError(t, err)
	require.Equal(t, ticketStateMatched, ticket.State)
	require.False(t, mr.Exists(mustLockKey(t, queue2)), "2 人档的锁必须释放")

	// 3 人档凑满。
	require.Zero(t, joinPveTeam(t, svcCtx, 21005, 1, 3).ErrorCode)
	runMatcherRound(context.Background(), svcCtx)
	call = expectPveGather(t, calls)
	require.Equal(t, []uint64{21002, 21003, 21005}, call.members)
	require.Empty(t, queueMembers(t, svcCtx, queue3))
}

func mustLockKey(t *testing.T, queueKey string) string {
	t.Helper()
	lockKey, err := lockKeyForQueue(queueKey)
	require.NoError(t, err)
	return lockKey
}

// 1 人档不排队:与 PVE_SOLO 同一条即时开战路径(matched 票、不入队、失败不回队列)。
func TestPveTeamSizeOneStartsImmediately(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	svcCtx.Config.PveTeamSizeByConfigId = map[string]uint32{"1": 5}
	calls := stubPveGather(t)
	setPlayerLocation(t, mr, 22001, 1)

	resp := joinPveTeam(t, svcCtx, 22001, 1, 1)
	require.Zero(t, resp.ErrorCode)
	require.NotEmpty(t, resp.QueueTicket)

	call := expectPveGather(t, calls)
	require.Equal(t, []uint64{22001}, call.members)
	require.Equal(t, matchpb.MatchMode_MATCH_MODE_PVE_TEAM, call.mode)
	require.False(t, call.requeue, "即时开战没有队列可回")

	ticket, err := loadTicket(svcCtx, 22001)
	require.NoError(t, err)
	require.Equal(t, ticketStateMatched, ticket.State)
	require.Empty(t, ticket.QueueKey)
	queues, err := svcCtx.MatchRedis.Smembers(matchQueueIndexKey)
	require.NoError(t, err)
	require.Empty(t, queues, "不入队就不该登记任何队列")
}

// 不填人数 = 副本上限,且与显式填上限的人进同一条队列(否则同一档的人被拆成两堆,谁也凑不满)。
func TestPveTeamSizeZeroMeansDungeonLimit(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	svcCtx.Config.PveTeamSizeByConfigId = map[string]uint32{"1": 3}
	calls := stubPveGather(t)
	for pid := uint64(23001); pid <= 23003; pid++ {
		setPlayerLocation(t, mr, pid, 1)
	}
	queue3 := matchSizedQueueKey(pveTeamMode, 1, 3)

	require.Zero(t, joinPveTeam(t, svcCtx, 23001, 1, 0).ErrorCode)
	require.Zero(t, joinPveTeam(t, svcCtx, 23002, 1, 3).ErrorCode)
	require.Zero(t, joinPveTeam(t, svcCtx, 23003, 1, 0).ErrorCode)
	require.Equal(t, []string{"23001", "23002", "23003"}, queueMembers(t, svcCtx, queue3))
	require.False(t, mr.Exists(matchQueueKey(pveTeamMode, 1)), "不再往不带人数段的队列写")

	runMatcherRound(context.Background(), svcCtx)
	require.Equal(t, []uint64{23001, 23002, 23003}, expectPveGather(t, calls).members)
}

// 人数超过副本上限、副本未开放组队:拒绝且不留票据。
func TestPveTeamSizeRejections(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	svcCtx.Config.PveTeamSizeByConfigId = map[string]uint32{"1": 3}
	calls := stubPveGather(t)
	setPlayerLocation(t, mr, 24001, 1)

	resp := joinPveTeam(t, svcCtx, 24001, 1, 4)
	require.Equal(t, constants.ErrModeNotOpen, resp.ErrorCode, "副本上限 3,不接受 4 人档")
	require.Equal(t, constants.ErrModeNotOpen, resp.GetErrorMessage().GetId())

	resp = joinPveTeam(t, svcCtx, 24001, 99, 2)
	require.Equal(t, constants.ErrTeamSizeNotConfigured, resp.ErrorCode, "未配置的副本不开放组队,填什么人数都一样")

	ticket, err := loadTicket(svcCtx, 24001)
	require.NoError(t, err)
	require.Nil(t, ticket)
	expectNoPveGather(t, calls)

	// 被拒之后照常能排合法的人数档。
	require.Zero(t, joinPveTeam(t, svcCtx, 24001, 1, 3).ErrorCode)
}

// 运维把副本上限调低后,残留的高人数档队列不再弹组(不把超编的一组人送去冻结再被引擎拒绝);
// 队列与票据原样保留,玩家取消后可以改排别的档。
func TestSizedQueueStopsMatchingWhenLimitLowered(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	svcCtx.Config.PveTeamSizeByConfigId = map[string]uint32{"1": 5}
	calls := stubPveGather(t)
	ids := []uint64{25001, 25002, 25003, 25004}
	for _, pid := range ids {
		setPlayerLocation(t, mr, pid, 1)
		require.Zero(t, joinPveTeam(t, svcCtx, pid, 1, 4).ErrorCode)
	}
	queue4 := matchSizedQueueKey(pveTeamMode, 1, 4)

	svcCtx.Config.PveTeamSizeByConfigId = map[string]uint32{"1": 3}
	runMatcherRound(context.Background(), svcCtx)
	expectNoPveGather(t, calls)
	require.Equal(t, []string{"25001", "25002", "25003", "25004"}, queueMembers(t, svcCtx, queue4))

	// 取消走票据里记的队列 key,与人数档无关。
	ticket, err := loadTicket(svcCtx, 25002)
	require.NoError(t, err)
	_, err = NewCancelQueueLogic(context.Background(), svcCtx).CancelQueue(&matchpb.CancelQueueRequest{
		PlayerId: 25002, QueueTicket: ticket.Ticket,
	})
	require.NoError(t, err)
	require.Equal(t, []string{"25001", "25003", "25004"}, queueMembers(t, svcCtx, queue4))
	ticket, err = loadTicket(svcCtx, 25002)
	require.NoError(t, err)
	require.Nil(t, ticket)
	require.Zero(t, joinPveTeam(t, svcCtx, 25002, 1, 3).ErrorCode)

	// 上限调回来,剩下三人加上新来的一个照常成组。
	svcCtx.Config.PveTeamSizeByConfigId = map[string]uint32{"1": 5}
	setPlayerLocation(t, mr, 25005, 1)
	require.Zero(t, joinPveTeam(t, svcCtx, 25005, 1, 4).ErrorCode)
	runMatcherRound(context.Background(), svcCtx)
	require.Equal(t, []uint64{25001, 25003, 25004, 25005}, expectPveGather(t, calls).members)
}

// queue_depth 按人数档分开上报:同一副本的两个档不能互相覆盖同一条时间序列。
func TestQueueDepthReportedPerTeamSize(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	svcCtx.Config.PveTeamSizeByConfigId = map[string]uint32{"1": 5}
	stubPveGather(t)

	var mu sync.Mutex
	depths := map[string]int{}
	prev := setQueueDepthFn
	setQueueDepthFn = func(mode string, config string, teamSize string, depth int) {
		mu.Lock()
		defer mu.Unlock()
		depths[mode+"/"+config+"/"+teamSize] = depth
	}
	t.Cleanup(func() { setQueueDepthFn = prev })

	for pid, size := range map[uint64]uint32{26001: 2, 26002: 3, 26003: 3} {
		setPlayerLocation(t, mr, pid, 1)
		require.Zero(t, joinPveTeam(t, svcCtx, pid, 1, size).ErrorCode)
	}
	setPlayerLocation(t, mr, 26004, 1)
	require.Zero(t, joinQueue1v1(t, svcCtx, 26004).ErrorCode)

	runMatcherRound(context.Background(), svcCtx)

	mu.Lock()
	defer mu.Unlock()
	pveTeamName := matchpb.MatchMode_MATCH_MODE_PVE_TEAM.String()
	require.Equal(t, 1, depths[pveTeamName+"/1/2"])
	require.Equal(t, 2, depths[pveTeamName+"/1/3"])
	require.Equal(t, 1, depths[matchpb.MatchMode_MATCH_MODE_1V1.String()+"/0/0"], "没有人数档的队列标签为 0")
}

// 上限调低后残留的人数档:还有人排着就留在注册集;人都取消之后必须离开注册集并把 queue_depth 归零。
// JoinQueue 已经不放人进这个档,不摘的话它会永远留在注册集里每轮被扫,gauge 停在最后一次上报的值。
func TestOverLimitSizedQueuePrunedOnceEmpty(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	svcCtx.Config.PveTeamSizeByConfigId = map[string]uint32{"1": 5}
	stubPveGather(t)

	var mu sync.Mutex
	depthsBySize := map[string][]int{}
	prev := setQueueDepthFn
	setQueueDepthFn = func(_ string, _ string, teamSize string, depth int) {
		mu.Lock()
		defer mu.Unlock()
		depthsBySize[teamSize] = append(depthsBySize[teamSize], depth)
	}
	t.Cleanup(func() { setQueueDepthFn = prev })

	setPlayerLocation(t, mr, 27001, 1)
	resp := joinPveTeam(t, svcCtx, 27001, 1, 4)
	require.Zero(t, resp.ErrorCode)
	queue4 := matchSizedQueueKey(pveTeamMode, 1, 4)

	svcCtx.Config.PveTeamSizeByConfigId = map[string]uint32{"1": 3}
	runMatcherRound(context.Background(), svcCtx)
	queues, err := svcCtx.MatchRedis.Smembers(matchQueueIndexKey)
	require.NoError(t, err)
	require.Contains(t, queues, queue4, "还有人排着:留在注册集,等他取消或配置恢复")
	require.Equal(t, []string{"27001"}, queueMembers(t, svcCtx, queue4))

	_, err = NewCancelQueueLogic(context.Background(), svcCtx).CancelQueue(&matchpb.CancelQueueRequest{
		PlayerId: 27001, QueueTicket: resp.QueueTicket,
	})
	require.NoError(t, err)
	runMatcherRound(context.Background(), svcCtx)
	queues, err = svcCtx.MatchRedis.Smembers(matchQueueIndexKey)
	require.NoError(t, err)
	require.NotContains(t, queues, queue4, "人走光后必须离开注册集")

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []int{0}, depthsBySize["4"], "只在剔除时上报一次归零;不能凑单的队列不占凑单锁,平时不上报深度")
}

// 同一个人在 5 人档留着一份没被摘掉的 list 项(没取消就离开,票据过期后改排了 2 人档):
// 5 人档凑单时不能把他带进去 —— 他的票据记的是 2 人档。残留项被摘掉,票据原样不动。
func TestStaleEntryFromOtherTierIsDroppedNotMatched(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	svcCtx.Config.PveTeamSizeByConfigId = map[string]uint32{"1": 5}
	calls := stubPveGather(t)
	queue2 := matchSizedQueueKey(pveTeamMode, 1, 2)
	queue5 := matchSizedQueueKey(pveTeamMode, 1, 5)

	// 28001 现在排的是 2 人档;5 人档队首有他的一份残留。
	setPlayerLocation(t, mr, 28001, 1)
	require.Zero(t, joinPveTeam(t, svcCtx, 28001, 1, 2).ErrorCode)
	require.NoError(t, enqueueAtomic(svcCtx, queue5, 28001, defaultRating))

	// 5 人档来了 4 个真人:list 长度够 5,但有效的只有 4 个。
	for pid := uint64(28002); pid <= 28005; pid++ {
		setPlayerLocation(t, mr, pid, 1)
		require.Zero(t, joinPveTeam(t, svcCtx, pid, 1, 5).ErrorCode)
	}
	require.Len(t, queueMembers(t, svcCtx, queue5), 5)

	runMatcherRound(context.Background(), svcCtx)
	expectNoPveGather(t, calls)
	require.Equal(t, []string{"28002", "28003", "28004", "28005"}, queueMembers(t, svcCtx, queue5), "残留项被摘掉,真人原序留在队列")
	assertQueueMirrorConsistent(t, mr, queue5)
	require.Equal(t, []string{"28001"}, queueMembers(t, svcCtx, queue2), "他在 2 人档的排队不受影响")
	ticket, err := loadTicket(svcCtx, 28001)
	require.NoError(t, err)
	require.NotNil(t, ticket)
	require.Equal(t, ticketStateQueued, ticket.State)
	require.Equal(t, queue2, ticket.QueueKey)

	// 第 5 个真人到了,5 人档照常成组,组里没有 28001。
	setPlayerLocation(t, mr, 28006, 1)
	require.Zero(t, joinPveTeam(t, svcCtx, 28006, 1, 5).ErrorCode)
	runMatcherRound(context.Background(), svcCtx)
	require.Equal(t, []uint64{28002, 28003, 28004, 28005, 28006}, expectPveGather(t, calls).members)
}
