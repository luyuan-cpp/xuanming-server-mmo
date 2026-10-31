package logic

// LeaseMonitor 按 slot 轮转探测与认领的行为测试。SceneManager 假实现复用 scene_manager_client_fake_test.go 的全包唯一一份;
// 会话不带 gate(GateId 留空),没有 Kafka 副作用,用例只看 LeaveScene 与租约簿记。辅助标识符加 scanTest 前缀。

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"player_locator/internal/svc"
	pb "proto/player_locator"
	"shared/redisslot"
)

// scanTestDisconnect 写一条断线中的会话(1 秒后到期),gate 留空。
func scanTestDisconnect(t *testing.T, sc *svc.ServiceContext, playerID uint64) {
	t.Helper()
	session := makeTestSession(playerID)
	session.GateId = ""
	session.GateInstanceId = ""
	ctx := context.Background()
	_, err := NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: session})
	require.NoError(t, err)
	_, err = NewSetDisconnectingLogic(ctx, sc).SetDisconnecting(&pb.SetDisconnectingRequest{
		PlayerId: playerID, SessionId: session.SessionId, LeaseTtlSeconds: 1,
	})
	require.NoError(t, err)
}

// scanTestMonitor 的时钟固定在 at:分片与真实时间无关。
func scanTestMonitor(sc *svc.ServiceContext, owner slotOwner, p LeaseMonitorParams, at time.Time) *leaseMonitor {
	m := newLeaseMonitor(sc, owner, p)
	m.now = func() time.Time { return at }
	return m
}

func scanTestSetup(t *testing.T) (*svc.ServiceContext, *miniredis.Miniredis, *fakeSceneManagerClient) {
	t.Helper()
	sc, mr := newTestSvcCtx(t)
	fake := &fakeSceneManagerClient{}
	sc.SceneManagerClient = fake
	return sc, mr, fake
}

// 只探测持有的 slot:别的 slot 上到期的租约归持有它的副本。
func TestLeaseMonitor_OnlyClaimsOwnedSlots(t *testing.T) {
	sc, mr, fake := scanTestSetup(t)
	const mine, theirs = uint64(21001), uint64(21002)
	require.NotEqual(t, redisslot.PlayerSlot(mine), redisslot.PlayerSlot(theirs))
	scanTestDisconnect(t, sc, mine)
	scanTestDisconnect(t, sc, theirs)
	at := time.Now().Add(5 * time.Second)
	m := scanTestMonitor(sc, fixedSlots{redisslot.PlayerSlot(mine)}, LeaseMonitorParams{BatchSize: 10}, at)

	m.tick(context.Background())

	calls := fake.leaveCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, mine, calls[0].PlayerId)
	assert.False(t, mr.Exists(sessionKey(mine)), "清理完成")
	assert.False(t, mr.Exists(leaseKeysForPlayer(mine).processing), "副作用成功后 claim 已 ack")
	assert.True(t, mr.Exists(sessionKey(theirs)))
	assert.Equal(t, []string{"21002"}, leaseMembers(t, mr), "别的 slot 的租约原样留着")
}

// 还没到期的不认领。
func TestLeaseMonitor_IgnoresLeasesNotYetDue(t *testing.T) {
	sc, mr, fake := scanTestSetup(t)
	const playerID = uint64(21003)
	scanTestDisconnect(t, sc, playerID)
	m := scanTestMonitor(sc, fixedSlots{redisslot.PlayerSlot(playerID)}, LeaseMonitorParams{BatchSize: 10}, time.Now().Add(-time.Minute))

	m.tick(context.Background())

	assert.Empty(t, fake.leaveCalls())
	assert.Equal(t, []string{"21003"}, leaseMembers(t, mr))
}

// 没分到段的拍什么都不发,照样算走完一拍:分片的健康判定不会因为手里没段就把自己判成停扫。
func TestLeaseMonitor_TickWithoutSlotsSendsNothing(t *testing.T) {
	sc, _, _ := scanTestSetup(t)
	rec := newCommandRecorder()
	sc.PlayerRedis.AddHook(rec)
	at := time.Now()
	m := scanTestMonitor(sc, fixedSlots(nil), LeaseMonitorParams{}, at)
	m.lastTickNanos.Store(at.Add(-time.Hour).UnixNano())

	m.tick(context.Background())

	assert.Zero(t, rec.count("zcount"))
	assert.True(t, m.scanFresh())
}

// 每拍探测的 slot 数让持有的 slot 在一个 ScanRound 内走完一遍;游标按 slot 值记,持有的段变了也接得上,走到头绕回去。
func TestLeaseMonitor_NextSlotsCoverOwnedSlotsWithinARound(t *testing.T) {
	sc, _, _ := scanTestSetup(t)
	m := scanTestMonitor(sc, nil, LeaseMonitorParams{PollInterval: time.Second, ScanRound: 4 * time.Second}, time.Now())
	owned := []uint16{10, 20, 30, 40, 50, 60, 70, 80}

	assert.Equal(t, []uint16{10, 20}, m.nextSlots(owned))
	assert.Equal(t, []uint16{30, 40}, m.nextSlots(owned))
	assert.Equal(t, []uint16{50, 60}, m.nextSlots(owned))
	assert.Equal(t, []uint16{70, 80}, m.nextSlots(owned), "4 拍走完一遍")
	assert.Equal(t, []uint16{10, 20}, m.nextSlots(owned), "绕回去")

	// 持有的段变了:从不小于游标的第一个 slot 接着走(4 个 slot、4 拍一遍,每拍 1 个)。
	changed := []uint16{5, 35, 45, 90}
	assert.Equal(t, []uint16{35}, m.nextSlots(changed))
	assert.Equal(t, []uint16{45}, m.nextSlots(changed))
	assert.Equal(t, []uint16{90}, m.nextSlots(changed))
	assert.Equal(t, []uint16{5}, m.nextSlots(changed), "到头绕回")

	assert.Nil(t, m.nextSlots(nil))
}

// 每拍至少探测一个 slot,最多探测全部:拍长比 ScanRound 还长时一拍走完,持有很少时也不会停。
func TestLeaseMonitor_NextSlotsCountIsBounded(t *testing.T) {
	sc, _, _ := scanTestSetup(t)
	longPoll := scanTestMonitor(sc, nil, LeaseMonitorParams{PollInterval: time.Minute, ScanRound: time.Second}, time.Now())
	assert.Len(t, longPoll.nextSlots([]uint16{1, 2, 3}), 3)

	shortPoll := scanTestMonitor(sc, nil, LeaseMonitorParams{PollInterval: time.Millisecond, ScanRound: time.Hour}, time.Now())
	assert.Len(t, shortPoll.nextSlots([]uint16{1, 2, 3}), 1)
}

// 每拍批量有上限:用完预算时游标停在没认领完的 slot 上,下一拍从它接着来,不跳过;别的 slot 也轮得到。
func TestLeaseMonitor_BatchBudgetResumesAtTheSameSlot(t *testing.T) {
	sc, mr, fake := scanTestSetup(t)
	pids := pidsInSameSlot(21010, 3)
	crowded := redisslot.PlayerSlot(pids[0])
	var other uint64 = 21011
	for redisslot.PlayerSlot(other) <= crowded { // other 的 slot 排在后面,预算用完时还没轮到它
		other++
	}
	for _, pid := range pids {
		scanTestDisconnect(t, sc, pid)
	}
	scanTestDisconnect(t, sc, other)
	at := time.Now().Add(5 * time.Second)
	owner := fixedSlots{crowded, redisslot.PlayerSlot(other)}
	m := scanTestMonitor(sc, owner, LeaseMonitorParams{BatchSize: 2, PollInterval: time.Second, ScanRound: time.Second}, at)
	ctx := context.Background()

	m.tick(ctx)
	assert.Len(t, fake.leaveCalls(), 2, "一拍只认领 BatchSize 个")
	assert.Equal(t, int(crowded), m.nextSlot)
	assert.Contains(t, leaseMembers(t, mr), strconv.FormatUint(other, 10))

	m.tick(ctx)
	assert.Len(t, fake.leaveCalls(), 4, "剩下的 1 个加上别的 slot 的 1 个")
	assert.Empty(t, leaseMembers(t, mr))
}

// 探测同时看 processing:认领超时的任务(worker 崩溃、副作用失败)靠这个回到 ready 再被认领。
func TestProbeDueLeaseSlots(t *testing.T) {
	sc, mr, _ := scanTestSetup(t)
	ctx := context.Background()
	at := time.Unix(1_800_000_000, 0)
	readyDue, readyLater, processingDue, poisoned, empty := uint16(1), uint16(2), uint16(3), uint16(4), uint16(5)
	_, err := mr.ZAdd(leaseKeysForSlot(readyDue).ready, float64(at.Unix()), "1")
	require.NoError(t, err)
	_, err = mr.ZAdd(leaseKeysForSlot(readyLater).ready, float64(at.Unix()+1), "2")
	require.NoError(t, err)
	_, err = mr.ZAdd(leaseKeysForSlot(processingDue).processing, float64(at.Unix()-1), "3")
	require.NoError(t, err)
	require.NoError(t, mr.Set(leaseKeysForSlot(poisoned).ready, "not a zset"))

	due, err := probeDueLeaseSlots(ctx, sc.PlayerRedis, []uint16{readyDue, readyLater, processingDue, poisoned, empty}, at)

	assert.Equal(t, []uint16{readyDue, processingDue}, due, "读失败的 slot 跳过,别的照常")
	require.Error(t, err)
	assert.True(t, isKeyTypeError(err))
}

// 探测按批走 pipeline,批量大小有上限。
func TestProbeDueLeaseSlots_BatchesPipelines(t *testing.T) {
	sc, _, _ := scanTestSetup(t)
	rec := newCommandRecorder()
	var largest int
	sc.PlayerRedis.AddHook(rec)
	sc.PlayerRedis.AddHook(pipelineSizeHook{observe: func(n int) { largest = max(largest, n) }})

	due, err := probeDueLeaseSlots(context.Background(), sc.PlayerRedis, slotRange(0, 3*leaseProbeSlotsPerPipeline+1), time.Now())

	require.NoError(t, err)
	assert.Empty(t, due)
	assert.Equal(t, 2*(3*leaseProbeSlotsPerPipeline+1), rec.count("zcount"), "每个 slot 看 ready 与 processing")
	assert.Equal(t, 2*leaseProbeSlotsPerPipeline, largest)
}

// pipelineSizeHook 记录每条 pipeline 的命令数。
type pipelineSizeHook struct {
	observe func(n int)
}

func (h pipelineSizeHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h pipelineSizeHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook { return next }

func (h pipelineSizeHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		h.observe(len(cmds))
		return next(ctx, cmds)
	}
}

// 分片的健康判定:一拍卡得比段租约周期还久才算停扫;拍长配置得很长时按拍长放宽。
func TestLeaseMonitor_ScanFresh(t *testing.T) {
	sc, _, _ := scanTestSetup(t)
	clock := newTestClock()
	m := newLeaseMonitor(sc, fixedSlots(nil), LeaseMonitorParams{PollInterval: time.Second})
	m.now = clock.Now
	m.tick(context.Background())

	clock.Advance(shardLeaseTTL)
	assert.True(t, m.scanFresh())
	clock.Advance(time.Second)
	assert.False(t, m.scanFresh(), "超过段租约周期没走完一拍")

	m.tick(context.Background())
	assert.True(t, m.scanFresh(), "走完一拍就恢复")

	slow := newLeaseMonitor(sc, fixedSlots(nil), LeaseMonitorParams{PollInterval: time.Minute})
	slow.now = clock.Now
	slow.tick(context.Background())
	clock.Advance(2 * time.Minute)
	assert.True(t, slow.scanFresh(), "拍长 1 分钟时 2 分钟没走完不算停扫")
	clock.Advance(2 * time.Minute)
	assert.False(t, slow.scanFresh())
}

// MarkOffline 入队清理后叫醒本进程:不等常规轮转,没有持段也能处理(认领脚本才是互斥手段)。
func TestLeaseMonitor_WakeProcessesCleanupWithoutOwningTheSlot(t *testing.T) {
	sc, mr, fake := scanTestSetup(t)
	sc.LeaseCleanupWake = make(chan uint16, 1)
	const playerID = uint64(21020)
	session := makeTestSession(playerID)
	session.GateId = ""
	session.GateInstanceId = ""
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: session})
	require.NoError(t, err)
	fake.setLeaveErr(status.Error(codes.Unavailable, "scene manager down"))
	_, err = NewMarkOfflineLogic(ctx, sc).MarkOffline(&pb.MarkOfflineRequest{
		PlayerId: playerID, ExpectedSessionId: session.SessionId, ExpectedSessionVersion: session.SessionVersion,
	})
	require.NoError(t, err)
	require.Len(t, fake.leaveCalls(), 1)
	fake.setLeaveErr(nil)

	m := newLeaseMonitor(sc, fixedSlots(nil), LeaseMonitorParams{PollInterval: time.Hour})
	var running sync.WaitGroup
	running.Add(1)
	go func() {
		defer running.Done()
		m.run(ctx)
	}()

	require.Eventually(t, func() bool { return len(fake.leaveCalls()) == 2 }, 5*time.Second, 10*time.Millisecond,
		"叫醒之后本进程处理这条清理,不等一小时的拍")
	require.Eventually(t, func() bool { return !mr.Exists(leaseKeysForPlayer(playerID).processing) }, 5*time.Second, 10*time.Millisecond,
		"LeaveScene 成功后 claim 已 ack")
	cancel()
	running.Wait()
}

// 通道满了不阻塞调用方:常规轮转兜底。
func TestWakeLeaseMonitorAt_DoesNotBlockWhenChannelIsFull(t *testing.T) {
	sc, _, _ := scanTestSetup(t)
	wakeLeaseMonitorAt(sc, 21021, time.Now()) // 没配通道:什么都不做

	sc.LeaseCleanupWake = make(chan uint16, 1)
	sc.LeaseCleanupWake <- 7
	wakeLeaseMonitorAt(sc, 21021, time.Now())
	wakeLeaseMonitorAt(sc, 21022, time.Now().Add(-time.Second))

	assert.Equal(t, uint16(7), <-sc.LeaseCleanupWake)
}

// 认领之后、钉载荷之前被 Reconnect 撤销:令牌已失效,这个 claim 直接丢弃,不会拿旧载荷去清理在线的新会话。
func TestClaimExpiredLeases_DropsClaimCancelledBeforePin(t *testing.T) {
	sc, mr, _ := scanTestSetup(t)
	ctx := context.Background()
	const playerID = uint64(21030)
	slot := redisslot.PlayerSlot(playerID)
	scanTestDisconnect(t, sc, playerID)
	other := secondClient(t, sc, mr)
	claimKeys := claimExpiredLeasesKeys(slot)
	var once sync.Once
	rec := newCommandRecorder()
	rec.after = func(cmd redis.Cmder) {
		if !isScriptOn(cmd, claimKeys...) || cmd.Err() != nil {
			return
		}
		once.Do(func() {
			resp, err := NewReconnectLogic(ctx, other).Reconnect(&pb.ReconnectRequest{
				PlayerId: playerID, NewSessionId: 201, GateId: "2", GateInstanceId: idxGateB, RequestId: "rc-1",
			})
			require.NoError(t, err)
			require.True(t, resp.Success, resp.ErrorMessage)
		})
	}
	sc.PlayerRedis.AddHook(rec)

	claims, err := claimExpiredLeases(ctx, sc, slot, time.Now().Add(5*time.Second), 10, 30*time.Second)

	require.NoError(t, err)
	assert.Empty(t, claims)
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_ONLINE, readSession(t, sc, playerID).State)
	assert.False(t, mr.Exists(leaseKeysForPlayer(playerID).claimPayloads), "没有留下载荷")
}

// 钉载荷覆盖旧值:会话还在就取会话;会话没了才用钉过的那份。
func TestClaimExpiredLeases_PinPrefersLiveSession(t *testing.T) {
	sc, mr, _ := scanTestSetup(t)
	ctx := context.Background()
	const playerID = uint64(21031)
	slot := redisslot.PlayerSlot(playerID)
	scanTestDisconnect(t, sc, playerID)
	live := readSessionBytes(t, sc, playerID)
	mr.HSet(leaseKeysForPlayer(playerID).claimPayloads, strconv.FormatUint(playerID, 10), "stale")

	claims, err := claimExpiredLeases(ctx, sc, slot, time.Now().Add(5*time.Second), 10, 1*time.Second)
	require.NoError(t, err)
	require.Len(t, claims, 1)
	assert.Equal(t, live, claims[0].payload)

	// 提交删掉会话;超时回收后再认领,载荷还是钉住的那份。
	committed, err := commitLeaseExpiry(ctx, sc, claims[0])
	require.NoError(t, err)
	require.Equal(t, 1, committed)
	claims, err = claimExpiredLeases(ctx, sc, slot, time.Now().Add(10*time.Second), 10, 1*time.Second)
	require.NoError(t, err)
	require.Len(t, claims, 1)
	assert.Equal(t, live, claims[0].payload)
}

// 租约 ZSET 里混进了坏成员(写路径不会写出来):声明不出同槽的会话键,这一个留在 processing 里周期性地喊,不拖累同批的其他人。
func TestClaimExpiredLeases_MalformedMemberDoesNotBlockBatch(t *testing.T) {
	sc, mr, _ := scanTestSetup(t)
	ctx := context.Background()
	const playerID, stranger = uint64(21001), uint64(21002)
	slot := redisslot.PlayerSlot(playerID)
	require.NotEqual(t, slot, redisslot.PlayerSlot(stranger))
	scanTestDisconnect(t, sc, playerID)
	ready := leaseKeysForSlot(slot).ready
	at := time.Now().Add(5 * time.Second)
	_, err := mr.ZAdd(ready, 1, "junk")
	require.NoError(t, err)
	_, err = mr.ZAdd(ready, 1, strconv.FormatUint(stranger, 10))
	require.NoError(t, err)

	claims, err := claimExpiredLeases(ctx, sc, slot, at, 10, 30*time.Second)

	require.NoError(t, err)
	require.Len(t, claims, 1)
	assert.Equal(t, playerID, claims[0].playerID)
	processing, err := mr.ZMembers(leaseKeysForSlot(slot).processing)
	require.NoError(t, err)
	assert.Contains(t, processing, "junk")
	assert.Contains(t, processing, strconv.FormatUint(stranger, 10))
}

// 心跳按 slot 分组续期:一批 claim 来自多个 slot 时每个 slot 各发一段脚本,都续得上。
func TestRenewLeaseClaims_GroupsBySlot(t *testing.T) {
	sc, _, _ := scanTestSetup(t)
	ctx := context.Background()
	const first, second = uint64(21001), uint64(21002)
	require.NotEqual(t, redisslot.PlayerSlot(first), redisslot.PlayerSlot(second))
	scanTestDisconnect(t, sc, first)
	scanTestDisconnect(t, sc, second)
	at := time.Now().Add(5 * time.Second)
	var claims []leaseClaim
	for _, pid := range []uint64{first, second} {
		got, err := claimExpiredLeases(ctx, sc, redisslot.PlayerSlot(pid), at, 10, 30*time.Second)
		require.NoError(t, err)
		claims = append(claims, got...)
	}
	require.Len(t, claims, 2)

	renewed, err := renewLeaseClaims(ctx, sc, claims, at.Add(time.Minute))

	require.NoError(t, err)
	assert.Equal(t, 2, renewed)
}
