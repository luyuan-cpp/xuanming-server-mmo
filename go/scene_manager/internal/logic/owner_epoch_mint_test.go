package logic

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"

	"proto/scene_manager"
	"scene_manager/internal/config"
	"scene_manager/internal/constants"
	"scene_manager/internal/svc"
	"shared/ownerepoch"
)

// ---------------------------------------------------------------------------
// 铸造之后的落盘确认(docs/design/zone-redis-regression-guard-20261022.md §4.3.2 G3b)
//
// 生产的铸造通道(svc.RedisOwnerEpochMinter)把铸造 EVAL 与 WAITAOF 放在同一条连接的同一个 pipeline 里;
// 那一层的连接、预算与重发由 svc/owner_epoch_mint_test.go 覆盖。本文件经同一个接口(svc.OwnerEpochMinter)
// 注入替身,只断言 logic 对落盘结论的处置:确认之前不推路由 / 重定向;没确认按推送失败单调回滚、回 7。
// ---------------------------------------------------------------------------

// fakeOwnerEpochMinter 是单测用的 svc.OwnerEpochMinter:铸造 EVAL 走 svcCtx 同一个 go-zero 客户端(miniredis 没有
// WAITAOF),落盘结论由用例指定,缺省 Confirmed。三个 svcCtx 构造助手(newTestSvcCtx、newTestSvcCtxWithWorldScenes、
// newIntegrationSvcCtxWithRedisOptions)缺省装上它。
//
// 墙钟缺省 0:墙钟地板不起作用,铸造退化为观察值 + 1,既有用例里的 1 / 2 / 3 照旧成立;要测地板的用例用 setNowMs
// 钉一个固定毫秒(单测不读真实墙钟,AGENTS §11.4)。
type fakeOwnerEpochMinter struct {
	rds *redis.Redis

	mu         sync.Mutex
	nowMs      uint64
	durability svc.MintDurability
	durableErr error
	// notSent 为 true 时模拟「剩余预算不够发一次 WAITAOF」:一条都不发,返回 svc.ErrOwnerEpochMintNotSent。
	notSent bool
	// afterEval 非 nil 时在 EVAL 执行之后、交回落盘结论之前调用(断言确认先于推路由)。
	afterEval func()
	calls     int
}

var _ svc.OwnerEpochMinter = (*fakeOwnerEpochMinter)(nil)

func newFakeOwnerEpochMinter(rds *redis.Redis) *fakeOwnerEpochMinter {
	return &fakeOwnerEpochMinter{rds: rds, durability: svc.MintDurabilityConfirmed}
}

func (m *fakeOwnerEpochMinter) NowMs() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.nowMs
}

// setNowMs 钉住之后每次铸造读到的墙钟(Unix 毫秒)。
func (m *fakeOwnerEpochMinter) setNowMs(ms uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nowMs = ms
}

func (m *fakeOwnerEpochMinter) EvalAndConfirm(ctx context.Context, script string, keys []string, args ...any) (svc.MintEvalResult, error) {
	m.mu.Lock()
	m.calls++
	durability, durableErr, notSent, afterEval := m.durability, m.durableErr, m.notSent, m.afterEval
	m.mu.Unlock()
	if notSent {
		return svc.MintEvalResult{}, svc.ErrOwnerEpochMintNotSent
	}
	reply, err := m.rds.EvalCtx(ctx, script, keys, args...)
	if err != nil {
		return svc.MintEvalResult{Attempts: 1}, err
	}
	if afterEval != nil {
		afterEval()
	}
	return svc.MintEvalResult{Reply: reply, Durability: durability, DurableErr: durableErr, Attempts: 1, Elapsed: time.Millisecond}, nil
}

// setDurability 改之后每次铸造的落盘结论。
func (m *fakeOwnerEpochMinter) setDurability(d svc.MintDurability, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.durability, m.durableErr = d, err
}

func (m *fakeOwnerEpochMinter) setNotSent(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notSent = v
}

func (m *fakeOwnerEpochMinter) setAfterEval(fn func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.afterEval = fn
}

func (m *fakeOwnerEpochMinter) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// testMinter 取出 svcCtx 构造助手装上的替身。
func testMinter(t *testing.T, sc *svc.ServiceContext) *fakeOwnerEpochMinter {
	t.Helper()
	m, ok := sc.OwnerEpochMint.(*fakeOwnerEpochMinter)
	require.True(t, ok, "svcCtx 构造助手应装上 fakeOwnerEpochMinter")
	return m
}

var errTestWaitAOFShort = errors.New("WAITAOF acked numlocal=0 numreplicas=0, want at least 1 and 0")

// mintLuaArgs 把直接 EVAL 铸造脚本的用例参数补齐到 ARGV[9]:传进来的 ARGV[1..k] 原样,ARGV[k+1..8] 补空串
// (location 期望 = any、不带位置修订),ARGV[9] = ARGV[1] + 1 —— 等同墙钟地板不起作用的铸造,与 G3a 之前 INCR
// 的结果相同。生产只经 placePlayerLocationCtx 发这段脚本,ARGV[9] 由 mintTargetEpoch 给出。ARGV[1] 不是十进制时
// panic(用例写错)。
func mintLuaArgs(args ...any) []any {
	observed, err := strconv.ParseUint(fmt.Sprint(args[0]), 10, 64)
	if err != nil {
		panic(fmt.Sprintf("mintLuaArgs: ARGV[1] %v is not a decimal epoch: %v", args[0], err))
	}
	return mintLuaArgsWithTarget(strconv.FormatUint(observed+1, 10), args...)
}

// mintLuaArgsWithTarget 同 mintLuaArgs,ARGV[9] 由用例显式给出(测地板与目标值校验)。
func mintLuaArgsWithTarget(target string, args ...any) []any {
	if len(args) > 8 {
		panic("mintLuaArgsWithTarget: at most 8 ARGV before the target")
	}
	out := make([]any, 0, 9)
	out = append(out, args...)
	for len(out) < 8 {
		out = append(out, "")
	}
	return append(out, target)
}

// --- 确认先于路由 -------------------------------------------------------------

// 凭标记的跨节点交接:落盘确认返回之前一条路由都没发;确认之后才发,且带的是铸出的 epoch。
func TestEnterScene_MintConfirmedBeforeRoutePush(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6901)
		oldScene = uint64(7901)
		targetID = uint64(7902)
	)
	seedSceneOnNode(mr, testZoneId, oldScene, "10", "3")
	seedSceneOnNode(mr, testZoneId, targetID, "20", "4")
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, oldScene, "10", testZoneId))
	writeHandoffMarker(t, sc, playerID, 1)
	withReachableSceneNode(t, sc, "10", "20")

	minter := testMinter(t, sc)
	routedAtConfirm := -1
	minter.setAfterEval(func() { routedAtConfirm = len(*captured) })

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	require.Equal(t, uint32(0), resp.ErrorCode)
	assert.Equal(t, 0, routedAtConfirm, "落盘确认交回之前不得推任何路由")
	require.Len(t, *captured, 1, "确认之后推一条路由")
	assert.Equal(t, uint64(2), decodeRoutePlayerEvent(t, (*captured)[0]).OwnerEpoch)
	assert.Equal(t, "2", ownerEpochRaw(t, sc, playerID))
}

// --- 没确认落盘:按推送失败回滚 ------------------------------------------------

// 同 zone 跨节点交接凭标记铸出 2,WAITAOF 份数不足:不推路由,epoch 单调回滚到 3,location 回源节点带回执,
// 标记转写到 3,两边人数退回;回 7,回显 3(与推路由失败同一终态,C++ 源端凭回执采纳 3)。
func TestEnterScene_MintNotDurableRollsBackHandoffWithoutRouting(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6902)
		oldScene = uint64(7903)
		targetID = uint64(7904)
	)
	seedSceneOnNode(mr, testZoneId, oldScene, "10", "3")
	seedSceneOnNode(mr, testZoneId, targetID, "20", "4")
	mr.Set(nodePlayerCountKey(testZoneId, "10"), "3")
	mr.Set(nodePlayerCountKey(testZoneId, "20"), "4")
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, oldScene, "10", testZoneId))
	writeHandoffMarker(t, sc, playerID, 1)
	withReachableSceneNode(t, sc, "10", "20")
	testMinter(t, sc).setDurability(svc.MintDurabilityNotDurable, errTestWaitAOFShort)

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrKafkaRoute, resp.ErrorCode)
	assert.Equal(t, uint64(3), resp.OwnerEpochAfterRollback)
	assert.Empty(t, *captured, "没确认落盘的 epoch 不得随路由交出去")

	assert.Equal(t, "3", ownerEpochRaw(t, sc, playerID), "epoch 只进不退:铸出 2 → 回滚到 3")
	loc, locErr := GetPlayerLocation(context.Background(), sc, playerID)
	require.NoError(t, locErr)
	require.NotNil(t, loc)
	assert.Equal(t, oldScene, loc.SceneId)
	assert.Equal(t, "10", loc.NodeId)
	assert.Equal(t, uint64(3), loc.OwnerEpoch)
	assert.Equal(t, "1:1757000000000", loc.RollbackReceipt)
	forwarded, _ := sc.Redis.Get(ownerepoch.HandoffKey(playerID))
	assert.Equal(t, "3:1757000000000", forwarded)
	for key, want := range map[string]string{
		fmt.Sprintf(InstancePlayerCountKey, targetID): "4",
		fmt.Sprintf(InstancePlayerCountKey, oldScene): "3",
		nodePlayerCountKey(testZoneId, "20"):          "4",
		nodePlayerCountKey(testZoneId, "10"):          "3",
	} {
		got, _ := sc.Redis.Get(key)
		assert.Equal(t, want, got, "人数全部退回: %s", key)
	}
}

// 跨 zone 第一条腿凭标记铸出 2,WAITAOF 出错(例如回复读失败):不推重定向,epoch 回滚到 3,location 回到
// 源 zone 带回执,旧场景人数还回去;回 7。
func TestEnterScene_MintDurabilityErrorRollsBackCrossZoneAdmission(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6903)
		oldScene = uint64(7905)
	)
	seedSceneOnNode(mr, 1, oldScene, "10", "3")
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, oldScene, "10", 1))
	writeHandoffMarker(t, sc, playerID, 1)
	seedDefaultWorldChannel(t, mr, 2)
	testMinter(t, sc).setDurability(svc.MintDurabilityError, errors.New("WAITAOF: i/o timeout"))

	logic := NewEnterSceneLogic(context.Background(), sc)
	logic.assignGateForZone = func(context.Context, *svc.ServiceContext, uint32, uint64) (*scene_manager.RedirectToGateInfo, error) {
		return &scene_manager.RedirectToGateInfo{TargetGateIp: "10.2.0.8", TargetGatePort: 7001}, nil
	}
	resp, err := logic.EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, ZoneId: 2,
		GateZoneId: 1, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrKafkaRoute, resp.ErrorCode)
	assert.Nil(t, resp.Redirect)
	assert.Equal(t, uint64(3), resp.OwnerEpochAfterRollback)
	assert.Empty(t, *captured, "没确认落盘时不推重定向")

	assert.Equal(t, "3", ownerEpochRaw(t, sc, playerID))
	loc, locErr := GetPlayerLocation(context.Background(), sc, playerID)
	require.NoError(t, locErr)
	require.NotNil(t, loc)
	assert.Equal(t, uint32(1), loc.ZoneId, "location 回到源 zone")
	assert.Equal(t, "10", loc.NodeId)
	assert.Equal(t, oldScene, loc.SceneId)
	assert.Equal(t, "1:1757000000000", loc.RollbackReceipt)
	oldCount, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, oldScene))
	assert.Equal(t, "3", oldCount, "旧场景人数退回")
}

// 首次落点(没有旧 location、不凭标记)铸出 1 后没确认落盘:location 删掉(bump 回滚 + DEL),epoch 前进到 2,
// 目标场景预占退回;回 7。之后的重试从 2 继续铸,1 不会被再铸一次。
func TestEnterScene_FirstPlacementMintNotDurableDeletesLocation(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6904)
		targetID = uint64(7906)
	)
	seedSceneOnNode(mr, testZoneId, targetID, "10", "5")
	testMinter(t, sc).setDurability(svc.MintDurabilityNotDurable, errTestWaitAOFShort)

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrKafkaRoute, resp.ErrorCode)
	assert.Empty(t, *captured)
	assert.False(t, mr.Exists(getPlayerLocationKey(playerID)), "首次落点回滚 = 删除 location")
	assert.Equal(t, "2", ownerEpochRaw(t, sc, playerID), "铸出 1 → 回滚到 2")
	count, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, targetID))
	assert.Equal(t, "5", count, "目标场景预占退回")

	testMinter(t, sc).setDurability(svc.MintDurabilityConfirmed, nil)
	resp, err = NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	require.Equal(t, uint32(0), resp.ErrorCode)
	require.Len(t, *captured, 1)
	assert.Equal(t, uint64(3), decodeRoutePlayerEvent(t, (*captured)[0]).OwnerEpoch, "重试从回滚后的 2 继续铸出 3")
}

// 同节点 epoch 0 的铸造没确认落盘:走 keep(与该路径推路由失败相同),只退 location,epoch 停在铸出的 1、
// 不再 INCR;不推路由。持有节点缓存仍是 0(存盘不带 guard),1 丢不丢都拒不了它。
func TestEnterScene_SameNodeZeroEpochMintNotDurableKeepsEpoch(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6905)
		oldScene = uint64(7907)
		targetID = uint64(7908)
	)
	seedSceneOnNode(mr, testZoneId, oldScene, "10", "3")
	seedSceneOnNode(mr, testZoneId, targetID, "10", "4")
	_, err := placePlayerLocation(sc, playerID, oldScene, "10", testZoneId, placementGuard{observedEpoch: 0, mint: false})
	require.NoError(t, err)
	oldRaw, _ := sc.Redis.Get(getPlayerLocationKey(playerID))
	withReachableSceneNode(t, sc, "10")
	testMinter(t, sc).setDurability(svc.MintDurabilityNotDurable, errTestWaitAOFShort)

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrKafkaRoute, resp.ErrorCode)
	assert.Zero(t, resp.OwnerEpochAfterRollback, "keep 回滚不回显")
	assert.Empty(t, *captured)
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID))
	restoredRaw, _ := sc.Redis.Get(getPlayerLocationKey(playerID))
	assert.Equal(t, oldRaw, restoredRaw)
	oldCount, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, oldScene))
	targetCount, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, targetID))
	assert.Equal(t, "3", oldCount)
	assert.Equal(t, "4", targetCount)
}

// 落盘确认关闭(两项份数都配 0):铸造照常放行、照常推路由。
func TestEnterScene_MintDurabilityDisabledRoutesAsBefore(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6906)
		targetID = uint64(7909)
	)
	seedSceneOnNode(mr, testZoneId, targetID, "10", "0")
	testMinter(t, sc).setDurability(svc.MintDurabilityDisabled, nil)

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	require.Equal(t, uint32(0), resp.ErrorCode)
	require.Len(t, *captured, 1)
	assert.Equal(t, uint64(1), decodeRoutePlayerEvent(t, (*captured)[0]).OwnerEpoch)
}

// --- 一个字节都没写的拒绝 ------------------------------------------------------

// 剩余预算不够发一次 WAITAOF:铸造通道一条都没发,location / epoch 不变,预占退回,回 3(按「结果可能已写」
// 的保守码,C++ 不据此判定未写)。
func TestEnterScene_MintNotSentLeavesPlacementUntouched(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6907)
		targetID = uint64(7910)
	)
	seedSceneOnNode(mr, testZoneId, targetID, "10", "2")
	testMinter(t, sc).setNotSent(true)

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrUpdateLocation, resp.ErrorCode)
	assert.Empty(t, *captured)
	assert.False(t, mr.Exists(getPlayerLocationKey(playerID)))
	assert.False(t, mr.Exists(ownerepoch.OwnerEpochKey(playerID)))
	count, _ := sc.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, targetID))
	assert.Equal(t, "2", count)
}

// 没装铸造通道(生产恒有):拒绝铸造,不退回 go-zero 上无确认的写法,Redis 一个字节都不写。
func TestPlacePlayerLocation_MintWithoutMinterFailsClosed(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.OwnerEpochMint = nil
	const playerID = uint64(6908)

	_, err := placePlayerLocation(sc, playerID, 7911, "10", testZoneId, placementGuard{observedEpoch: 0, mint: true})
	require.ErrorIs(t, err, errOwnerEpochMinterMissing)
	assert.False(t, mr.Exists(getPlayerLocationKey(playerID)))
	assert.False(t, mr.Exists(ownerepoch.OwnerEpochKey(playerID)))
}

// 不铸造的 CAS 写不经铸造通道(不发 WAITAOF)。
func TestPlacePlayerLocation_UnmintedCASBypassesMinter(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)
	minter := testMinter(t, sc)
	const playerID = uint64(6909)

	_, err := placePlayerLocation(sc, playerID, 7912, "10", testZoneId, placementGuard{observedEpoch: 0, mint: false})
	require.NoError(t, err)
	assert.Zero(t, minter.callCount())

	_, err = placePlayerLocation(sc, playerID, 7913, "20", testZoneId, placementGuard{observedEpoch: 0, mint: true})
	require.NoError(t, err)
	assert.Equal(t, 1, minter.callCount())
}

// 没确认落盘时 placePlayerLocation 交回的 placed 有效(调用方要拿它回滚),错误可用 errors.Is 认出。
func TestPlacePlayerLocation_NotDurableReturnsUsablePlacement(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)
	testMinter(t, sc).setDurability(svc.MintDurabilityNotDurable, errTestWaitAOFShort)
	const playerID = uint64(6910)

	placed, err := placePlayerLocation(sc, playerID, 7914, "10", testZoneId, placementGuard{observedEpoch: 0, mint: true})
	require.ErrorIs(t, err, errOwnerEpochMintNotDurable)
	assert.Equal(t, uint64(1), placed.epoch)
	assert.True(t, placed.minted)
	raw, _ := sc.Redis.Get(getPlayerLocationKey(playerID))
	assert.Equal(t, placed.raw, raw, "铸造确实写进了 Redis,placed.raw 就是写进去的原文")
}

// --- 时间预算 ---------------------------------------------------------------

// 铸造的截止 = 请求截止 − (Kafka 同步写超时 + 余量):WAITAOF 不得吃掉之后推路由与回滚要用的时间。
// 请求没有截止时间时原样返回(WAITAOF 按配置值等)。
func TestOwnerEpochMintContextReservesRoutePushBudget(t *testing.T) {
	c := config.Config{KafkaWriteTimeoutSeconds: 2}
	deadline := time.Now().Add(8 * time.Second)
	parent, cancelParent := context.WithDeadline(context.Background(), deadline)
	defer cancelParent()

	mintCtx, cancel := ownerEpochMintContext(parent, &c)
	defer cancel()
	got, ok := mintCtx.Deadline()
	require.True(t, ok)
	assert.Equal(t, deadline.Add(-(2*time.Second + ownerEpochMintRouteReserveSlack)), got)

	bare, cancelBare := ownerEpochMintContext(context.Background(), &c)
	defer cancelBare()
	_, ok = bare.Deadline()
	assert.False(t, ok, "没有截止时间的请求不凭空加截止")

	// KafkaWriteTimeoutSeconds 没配(0)时按 5s 默认值留出,与 kafka.Writer 实际用的同一个值。
	var zero config.Config
	defaulted, cancelDefaulted := ownerEpochMintContext(parent, &zero)
	defer cancelDefaulted()
	got, ok = defaulted.Deadline()
	require.True(t, ok)
	assert.Equal(t, deadline.Add(-(5*time.Second + ownerEpochMintRouteReserveSlack)), got)
}

// --- 铸造脚本的重放识别分支 ----------------------------------------------------

// 重放识别命中时脚本必须对 owner_epoch 发一次写:WAITAOF 只覆盖「本连接传播过的写」,重发走新连接,只读的识别
// 分支会让 WAITAOF 立即回满份数,首发那次铸造却未必落盘。用一个人为的 TTL 观察这次写(SET 会清掉 TTL),
// 值本身必须不变。CAS 不过、不是本请求的写(回 0)时不写。
func TestMintEpochLuaReplayBranchRewritesEpochForWaitAOF(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	const playerID = uint64(6911)
	marker := ownerepoch.Handoff{Epoch: 1, SavedAtMs: 1757000000000}.String()
	keys := []string{ownerepoch.OwnerEpochKey(playerID), getPlayerLocationKey(playerID), ownerepoch.HandoffKey(playerID),
		locationRevisionKey(playerID), locationRevisionFloorKey(playerID)}
	mr.Set(ownerepoch.OwnerEpochKey(playerID), "1")
	mr.Set(ownerepoch.HandoffKey(playerID), marker)

	first, err := sc.Redis.Eval(luaMintEpochAndSetLocation, keys, mintLuaArgs("1", "placed-bytes", marker)...)
	require.NoError(t, err)
	require.Equal(t, "2", fmt.Sprint(first))

	mr.SetTTL(ownerepoch.OwnerEpochKey(playerID), time.Hour)
	replay, err := sc.Redis.Eval(luaMintEpochAndSetLocation, keys, mintLuaArgs("1", "placed-bytes", marker)...)
	require.NoError(t, err)
	assert.Equal(t, "-2", fmt.Sprint(replay), "重放识别命中")
	assert.Equal(t, "2", ownerEpochRaw(t, sc, playerID), "写回的是原值")
	assert.Zero(t, mr.TTL(ownerepoch.OwnerEpochKey(playerID)), "识别分支对 owner_epoch 发了一次写")

	mr.SetTTL(ownerepoch.OwnerEpochKey(playerID), time.Hour)
	other, err := sc.Redis.Eval(luaMintEpochAndSetLocation, keys, mintLuaArgs("1", "someone-else-bytes", marker)...)
	require.NoError(t, err)
	assert.Equal(t, "0", fmt.Sprint(other), "不是本请求的写:回 0")
	assert.Greater(t, mr.TTL(ownerepoch.OwnerEpochKey(playerID)), time.Duration(0), "回 0 的分支一个字节都不写")
}

// ---------------------------------------------------------------------------
// G3a 墙钟地板(docs/design/zone-redis-regression-guard-20261022.md §4.3.1)
//
// 铸造值 = max(观察值 + 1, 当前毫秒)。zone Redis 回退丢掉最近的铸造之后,下一次铸造取墙钟,必然越过回退前铸出的
// 一切值,不会把已经发给某个节点的值再铸一次给另一个节点(identity-routing-redis#1)。
// ---------------------------------------------------------------------------

// 用例钉住的墙钟(2025-10-09 前后的 Unix 毫秒,约 1.76e12,远小于 2^53)。
const testWallClockMs uint64 = 1_760_000_000_000

func TestMintTargetEpoch(t *testing.T) {
	cases := []struct {
		name        string
		observed    uint64
		nowMs       uint64
		want        uint64
		clockBehind bool
	}{
		{"floor_above_observed_plus_one", 1, testWallClockMs, testWallClockMs, false},
		{"first_mint_from_zero", 0, testWallClockMs, testWallClockMs, false},
		{"floor_equals_observed_plus_one", testWallClockMs - 1, testWallClockMs, testWallClockMs, false},
		{"floor_equals_observed", testWallClockMs, testWallClockMs, testWallClockMs + 1, true},
		{"floor_below_observed", testWallClockMs + 5, testWallClockMs, testWallClockMs + 6, true},
		{"clock_unset", 4, 0, 5, true},
		{"just_below_max", maxMintedOwnerEpoch - 1, 0, maxMintedOwnerEpoch, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, behind, err := mintTargetEpoch(c.observed, c.nowMs)
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
			assert.Equal(t, c.clockBehind, behind)
		})
	}

	for name, in := range map[string][2]uint64{
		"observed_at_max":          {maxMintedOwnerEpoch, 0},
		"observed_uint64_max":      {^uint64(0), 0},
		"clock_in_nanoseconds":     {1, testWallClockMs * 1_000_000},
		"clock_one_above_max":      {1, maxMintedOwnerEpoch + 1},
		"clock_in_nanoseconds_max": {1, ^uint64(0)},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := mintTargetEpoch(in[0], in[1])
			require.ErrorIs(t, err, errOwnerEpochMintTargetOutOfRange)
		})
	}
}

// 首次落点取墙钟:键、location 与下发的 RoutePlayerEvent 都是当前毫秒,不是 1。
func TestEnterScene_WallClockFloorMintsCurrentMillis(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	testMinter(t, sc).setNowMs(testWallClockMs)

	const (
		playerID = uint64(6920)
		targetID = uint64(7920)
	)
	seedSceneOnNode(mr, testZoneId, targetID, "10", "0")

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	require.Equal(t, uint32(0), resp.ErrorCode)
	want := strconv.FormatUint(testWallClockMs, 10)
	assert.Equal(t, want, ownerEpochRaw(t, sc, playerID))
	loc, locErr := GetPlayerLocation(context.Background(), sc, playerID)
	require.NoError(t, locErr)
	require.NotNil(t, loc)
	assert.Equal(t, testWallClockMs, loc.OwnerEpoch, "location 与键同值")
	require.Len(t, *captured, 1)
	assert.Equal(t, testWallClockMs, decodeRoutePlayerEvent(t, (*captured)[0]).OwnerEpoch)
}

// 凭标记的跨节点交接铸出墙钟值 T 之后推路由失败:回滚仍是 INCR 一格(T → T+1),回执是原标记,标记转写成
// "{T+1}:…"。C++ 源端按「回滚后 ≥ 标记 epoch + 2」采纳(player_lifecycle.h ClassifyOwnership)。
func TestEnterScene_WallClockFloorRouteFailureBumpsOneAndForwardsMarker(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Kafka = &countingKafkaWriter{err: errors.New("broker unavailable")}
	sc.Config.KafkaWriteTimeoutSeconds = 1

	const (
		playerID = uint64(6921)
		oldScene = uint64(7921)
		targetID = uint64(7922)
	)
	seedSceneOnNode(mr, testZoneId, oldScene, "10", "3")
	seedSceneOnNode(mr, testZoneId, targetID, "20", "4")
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, oldScene, "10", testZoneId))
	writeHandoffMarker(t, sc, playerID, 1)
	withReachableSceneNode(t, sc, "10", "20")
	testMinter(t, sc).setNowMs(testWallClockMs)

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrKafkaRoute, resp.ErrorCode)
	rolledBack := testWallClockMs + 1
	assert.Equal(t, rolledBack, resp.OwnerEpochAfterRollback)
	assert.Equal(t, strconv.FormatUint(rolledBack, 10), ownerEpochRaw(t, sc, playerID), "回滚只 INCR 一格")
	loc, locErr := GetPlayerLocation(context.Background(), sc, playerID)
	require.NoError(t, locErr)
	require.NotNil(t, loc)
	assert.Equal(t, oldScene, loc.SceneId)
	assert.Equal(t, rolledBack, loc.OwnerEpoch, "location 与键同值(C++ 交叉校验)")
	assert.Equal(t, "1:1757000000000", loc.RollbackReceipt)
	forwarded, _ := sc.Redis.Get(ownerepoch.HandoffKey(playerID))
	assert.Equal(t, strconv.FormatUint(rolledBack, 10)+":1757000000000", forwarded)
}

// 墙钟远超上限(这里取 uint64 最大值,纳秒级的时钟同样超限)时 fail-closed:铸造通道一次都不调,Redis 一个字节都不写。
func TestPlacePlayerLocation_WallClockOutOfRangeFailsClosed(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	minter := testMinter(t, sc)
	minter.setNowMs(^uint64(0))
	const playerID = uint64(6922)

	_, err := placePlayerLocation(sc, playerID, 7923, "10", testZoneId, placementGuard{observedEpoch: 0, mint: true})
	require.ErrorIs(t, err, errOwnerEpochMintTargetOutOfRange)
	assert.Zero(t, minter.callCount())
	assert.False(t, mr.Exists(getPlayerLocationKey(playerID)))
	assert.False(t, mr.Exists(ownerepoch.OwnerEpochKey(playerID)))
}

// 铸造脚本直接按 ARGV[9] 落值;重发认的是同一个目标值;被 bump 回滚(T+1)之后的重发不再认。
func TestMintEpochLuaFloorTargetReplayAndRollback(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)
	const playerID = uint64(6923)
	marker := ownerepoch.Handoff{Epoch: 1, SavedAtMs: 1757000000000}.String()
	mintKeys := []string{ownerepoch.OwnerEpochKey(playerID), getPlayerLocationKey(playerID), ownerepoch.HandoffKey(playerID)}
	rollbackKeys := []string{getPlayerLocationKey(playerID), ownerepoch.OwnerEpochKey(playerID), ownerepoch.HandoffKey(playerID)}
	require.NoError(t, sc.Redis.Set(ownerepoch.OwnerEpochKey(playerID), "1"))
	require.NoError(t, sc.Redis.Set(getPlayerLocationKey(playerID), "old-bytes"))
	require.NoError(t, sc.Redis.Set(ownerepoch.HandoffKey(playerID), marker))
	target := strconv.FormatUint(testWallClockMs, 10)
	args := mintLuaArgsWithTarget(target, "1", "placed-bytes", marker)

	result, err := sc.Redis.Eval(luaMintEpochAndSetLocation, mintKeys, args...)
	require.NoError(t, err)
	require.Equal(t, target, fmt.Sprint(result))
	require.Equal(t, target, ownerEpochRaw(t, sc, playerID))

	result, err = sc.Redis.Eval(luaMintEpochAndSetLocation, mintKeys, args...)
	require.NoError(t, err)
	assert.Equal(t, "-"+target, fmt.Sprint(result), "原样重发:认出本请求自己的写")

	rolledBack := strconv.FormatUint(testWallClockMs+1, 10)
	result, err = sc.Redis.Eval(luaRollbackPlayerPlacement, rollbackKeys,
		"placed-bytes", "restored-bytes", target, "bump", marker, rolledBack+":1757000000000", "300")
	require.NoError(t, err)
	require.Equal(t, "-"+rolledBack, fmt.Sprint(result), "回滚仍是 INCR 一格")

	result, err = sc.Redis.Eval(luaMintEpochAndSetLocation, mintKeys, args...)
	require.NoError(t, err)
	assert.Equal(t, "0", fmt.Sprint(result), "回滚之后的重发:目标值对不上,不是本请求已生效")
	assert.Equal(t, rolledBack, ownerEpochRaw(t, sc, playerID))
}

// 目标值不合格时脚本回 error_reply,任何读写之前就拒绝:缺省、空串、前导零、非数字、不大于观察值、超过 2^53 − 2、
// 观察值本身不规范。按十进制串比较,不经 Lua double:2^53 相邻的两个值也分得开。
func TestMintEpochLuaRejectsInvalidTargetWithoutWriting(t *testing.T) {
	cases := []struct {
		name string
		args []any
	}{
		{"target_missing", []any{"1", "loc", "", "", "", "", "", ""}},
		{"target_empty", mintLuaArgsWithTarget("", "1", "loc", "")},
		{"target_leading_zero", mintLuaArgsWithTarget("02", "1", "loc", "")},
		{"target_not_decimal", mintLuaArgsWithTarget("2x", "1", "loc", "")},
		{"target_equals_observed", mintLuaArgsWithTarget("1", "1", "loc", "")},
		{"target_below_observed", mintLuaArgsWithTarget("9", "10", "loc", "")},
		{"target_above_max", mintLuaArgsWithTarget("9007199254740991", "1", "loc", "")},
		{"target_far_above_max", mintLuaArgsWithTarget("18446744073709551615", "1", "loc", "")},
		{"observed_leading_zero", mintLuaArgsWithTarget("2", "01", "loc", "")},
		{"observed_not_decimal", mintLuaArgsWithTarget("2", "x", "loc", "")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sc, mr := newTestSvcCtxWithWorldScenes(t)
			const playerID = uint64(6924)
			mintKeys := []string{ownerepoch.OwnerEpochKey(playerID), getPlayerLocationKey(playerID), ownerepoch.HandoffKey(playerID)}
			require.NoError(t, sc.Redis.Set(ownerepoch.OwnerEpochKey(playerID), fmt.Sprint(c.args[0])))
			_, err := sc.Redis.Eval(luaMintEpochAndSetLocation, mintKeys, c.args...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "owner epoch mint target invalid")
			assert.Equal(t, fmt.Sprint(c.args[0]), ownerEpochRaw(t, sc, playerID), "epoch 一个字节都不动")
			assert.False(t, mr.Exists(getPlayerLocationKey(playerID)), "location 一个字节都不写")
		})
	}

	// 上限本身合法;按串比较能区分 2^53 − 2 与 2^53 − 1(double 下前者 +1 与后者相等)。
	sc, _ := newTestSvcCtxWithWorldScenes(t)
	const playerID = uint64(6925)
	mintKeys := []string{ownerepoch.OwnerEpochKey(playerID), getPlayerLocationKey(playerID), ownerepoch.HandoffKey(playerID)}
	maxText := strconv.FormatUint(maxMintedOwnerEpoch, 10)
	require.Equal(t, "9007199254740990", maxText, "luaMintTargetCheck 的字面量必须与 maxMintedOwnerEpoch 同值")
	result, err := sc.Redis.Eval(luaMintEpochAndSetLocation, mintKeys, mintLuaArgsWithTarget(maxText, "0", "loc", "")...)
	require.NoError(t, err)
	assert.Equal(t, maxText, fmt.Sprint(result))
}
