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
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	gproto "google.golang.org/protobuf/proto"

	"proto/scene_manager"
	"scene_manager/internal/config"
	"scene_manager/internal/constants"
	"scene_manager/internal/metrics"
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
	// queued 非空时按调用顺序逐个取用,取完之后回落到 durability / durableErr。两段回执里铸造与回滚(或确认)都经这个
	// 接口,用例据此分别指定两次的落盘结论。
	queued []fakeDurability
	// scripts 按调用顺序记下经过本接口的脚本(断言哪段脚本走了铸造通道)。
	scripts []string
	// unknownAfterEval > 0 时接下来这么多次调用先把 EVAL 执行掉,再回 svc.ErrOwnerEpochMintResultUnknown:模拟首发已生效、
	// 回复丢了,重发也没拿到结论(铸造通道的「结果未知」)。
	unknownAfterEval int
	// resendAfterEval > 0 时接下来这么多次调用把 EVAL 执行两遍,交回第二遍的回复且 Attempts = 2:模拟首发已生效、回复丢了,
	// 新连接上的原样重发拿到了回复(不凭标记的铸造会回 0)。
	resendAfterEval int
}

// fakeDurability 是一次排队的落盘结论。
type fakeDurability struct {
	durability svc.MintDurability
	err        error
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
	m.scripts = append(m.scripts, script)
	durability, durableErr, notSent, afterEval := m.durability, m.durableErr, m.notSent, m.afterEval
	if len(m.queued) > 0 {
		durability, durableErr = m.queued[0].durability, m.queued[0].err
		m.queued = m.queued[1:]
	}
	unknown := m.unknownAfterEval > 0
	if unknown {
		m.unknownAfterEval--
	}
	resend := !unknown && m.resendAfterEval > 0
	if resend {
		m.resendAfterEval--
	}
	m.mu.Unlock()
	if notSent {
		return svc.MintEvalResult{}, svc.ErrOwnerEpochMintNotSent
	}
	reply, err := m.rds.EvalCtx(ctx, script, keys, args...)
	if err != nil {
		return svc.MintEvalResult{Attempts: 1}, err
	}
	if unknown {
		return svc.MintEvalResult{Attempts: 3}, fmt.Errorf("%w after 3 attempt(s): test transport error", svc.ErrOwnerEpochMintResultUnknown)
	}
	attempts := 1
	if resend {
		reply, err = m.rds.EvalCtx(ctx, script, keys, args...)
		if err != nil {
			return svc.MintEvalResult{Attempts: 2}, fmt.Errorf("%w: %w", svc.ErrOwnerEpochMintResultUnknown, err)
		}
		attempts = 2
	}
	if afterEval != nil {
		afterEval()
	}
	return svc.MintEvalResult{Reply: reply, Durability: durability, DurableErr: durableErr, Attempts: attempts, Elapsed: time.Millisecond}, nil
}

// setUnknownAfterEval 让接下来 n 次调用执行 EVAL 之后回「结果未知」。
func (m *fakeOwnerEpochMinter) setUnknownAfterEval(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unknownAfterEval = n
}

// setResendAfterEval 让接下来 n 次调用把 EVAL 执行两遍,交回第二遍的回复(Attempts = 2)。
func (m *fakeOwnerEpochMinter) setResendAfterEval(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resendAfterEval = n
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

// queueDurability 让接下来的调用按顺序取这些落盘结论(取完回落到 setDurability 设的值)。
func (m *fakeOwnerEpochMinter) queueDurability(results ...fakeDurability) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queued = append(m.queued, results...)
}

// scriptCalls 返回按顺序经过本接口的脚本。
func (m *fakeOwnerEpochMinter) scriptCalls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.scripts...)
}

// resetScriptCalls 清掉已记下的脚本:用例的准备步骤(例如 UpdatePlayerLocation 铸造初始位置)也经过本接口,
// 断言只看被测请求时先清掉。
func (m *fakeOwnerEpochMinter) resetScriptCalls() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scripts = nil
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
	adoptSceneMember(mr, oldScene, playerID)
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

// 同 zone 跨节点交接凭标记铸出 2,WAITAOF 份数不足;回滚之后的落盘确认同样不足(替身对两次调用都回 not_durable):
// 不推路由,epoch 单调回滚到 3,location 回源节点带**待确认回执**,标记转写到 3,两边人数退回;回 7,回显 3。
// 回滚走共享连接池(两段回执的第一段),确认没成,回执停在待确认形态:C++ 源端见到它不采纳 3(判定表 B5′)。
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
	seedNodeMembers(mr, testZoneId, "10", 3)
	seedNodeMembers(mr, testZoneId, "20", 4)
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, oldScene, "10", testZoneId))
	adoptSceneMember(mr, oldScene, playerID)
	adoptNodeMember(mr, testZoneId, "10", playerID)
	writeHandoffMarker(t, sc, playerID, 1)
	withReachableSceneNode(t, sc, "10", "20")
	testMinter(t, sc).setDurability(svc.MintDurabilityNotDurable, errTestWaitAOFShort)
	testMinter(t, sc).resetScriptCalls()

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
	assert.Equal(t, ownerepoch.PendingRollbackReceipt("1:1757000000000"), loc.RollbackReceipt,
		"回滚没确认落盘:回执停在待确认形态,源端不得采纳")
	forwarded, _ := sc.Redis.Get(ownerepoch.HandoffKey(playerID))
	assert.Equal(t, "3:1757000000000", forwarded)
	assert.Equal(t, "4", sceneCountText(sc, targetID), "目标场景预占退回")
	assert.Equal(t, "3", sceneCountText(sc, oldScene), "旧场景人数还回去")
	assert.Equal(t, "4", nodeCountText(sc, testZoneId, "20"), "目标节点预占退回")
	assert.Equal(t, "3", nodeCountText(sc, testZoneId, "10"), "旧节点人数还回去")
	assert.Equal(t, []string{luaMintEpochAndSetLocation, luaConfirmPendingRollback}, testMinter(t, sc).scriptCalls(),
		"回滚走共享连接池,铸造通道只收到铸造与之后的落盘确认")
}

// 同上,但回滚之后经铸造通道的落盘确认到了份数:回执按 CAS 改成正式回执(两段回执的第二段),源端可以凭它采纳 3;
// 读得回 location 说明修订侧车与原文仍一致。
func TestEnterScene_MintNotDurableRollbackConfirmedUpgradesReceipt(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6912)
		oldScene = uint64(7911)
		targetID = uint64(7912)
	)
	seedSceneOnNode(mr, testZoneId, oldScene, "10", "3")
	seedSceneOnNode(mr, testZoneId, targetID, "20", "4")
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, oldScene, "10", testZoneId))
	adoptSceneMember(mr, oldScene, playerID)
	writeHandoffMarker(t, sc, playerID, 1)
	withReachableSceneNode(t, sc, "10", "20")
	testMinter(t, sc).queueDurability(
		fakeDurability{durability: svc.MintDurabilityNotDurable, err: errTestWaitAOFShort},
		fakeDurability{durability: svc.MintDurabilityConfirmed},
	)
	testMinter(t, sc).resetScriptCalls()

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrKafkaRoute, resp.ErrorCode)
	assert.Equal(t, uint64(3), resp.OwnerEpochAfterRollback)
	assert.Empty(t, *captured)
	assert.Equal(t, "3", ownerEpochRaw(t, sc, playerID))
	loc, locErr := GetPlayerLocation(context.Background(), sc, playerID)
	require.NoError(t, locErr, "改写回执之后修订侧车必须与新原文一致")
	require.NotNil(t, loc)
	assert.Equal(t, "10", loc.NodeId)
	assert.Equal(t, uint64(3), loc.OwnerEpoch)
	assert.Equal(t, "1:1757000000000", loc.RollbackReceipt, "回滚确认落盘之后才改成正式回执")
	assert.Equal(t, []string{luaMintEpochAndSetLocation, luaConfirmPendingRollback}, testMinter(t, sc).scriptCalls())
}

// 跨 zone 第一条腿凭标记铸出 2,WAITAOF 出错(例如回复读失败):不推重定向,epoch 回滚到 3,location 回到
// 源 zone,旧场景人数还回去;回 7。回滚之后的落盘确认到了份数,回执改成正式回执(两段回执)。
func TestEnterScene_MintDurabilityErrorRollsBackCrossZoneAdmission(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6903)
		oldScene = uint64(7905)
	)
	seedSceneOnNode(mr, 1, oldScene, "10", "3")
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, oldScene, "10", 1))
	adoptSceneMember(mr, oldScene, playerID)
	writeHandoffMarker(t, sc, playerID, 1)
	seedDefaultWorldChannel(t, mr, 2)
	testMinter(t, sc).queueDurability(
		fakeDurability{durability: svc.MintDurabilityError, err: errors.New("WAITAOF: i/o timeout")},
		fakeDurability{durability: svc.MintDurabilityConfirmed},
	)

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
	oldCount := sceneCountText(sc, oldScene)
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
	count := sceneCountText(sc, targetID)
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
	adoptSceneMember(mr, oldScene, playerID)
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
	oldCount := sceneCountText(sc, oldScene)
	targetCount := sceneCountText(sc, targetID)
	assert.Equal(t, "3", oldCount)
	assert.Equal(t, "4", targetCount)
}

// --- 铸造结果未知(errOwnerEpochMintResultUnknown)与同节点 epoch 0 的残留值(§4.3.4) ------------------------------

// 首次落点的铸造 EVAL 结果未知(首发其实已生效、回复丢了,重发也没拿到结论):不推路由,按计划好的落点原字节回滚 ——
// location 删掉、epoch 再进一格作废,目标场景预占退回,回 7。以前回 3、不回滚:生效了的落点(没确认落盘的 1)留在 Redis 里,
// 之后的同落点重连会原样下发它。
func TestEnterScene_FirstPlacementMintResultUnknownRollsBack(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6921)
		targetID = uint64(7921)
	)
	seedSceneOnNode(mr, testZoneId, targetID, "10", "5")
	testMinter(t, sc).setUnknownAfterEval(1)

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrKafkaRoute, resp.ErrorCode)
	assert.Empty(t, *captured, "结果未知的铸造值一次都不下发")
	assert.False(t, mr.Exists(getPlayerLocationKey(playerID)), "生效了的首次落点被撤掉")
	assert.Equal(t, "2", ownerEpochRaw(t, sc, playerID), "铸出 1 → 回滚到 2,1 作废")
	assert.Equal(t, "5", sceneCountText(sc, targetID), "目标场景预占退回")
}

// 首次落点的铸造首发已生效、回复丢了,新连接上的重发拿到了回复,却是 0(不凭标记的铸造认不出自己的写):按结果未知收尾,
// 回滚撤掉那次落点,回 7。以前按并发冲突回 19、不回滚。
func TestEnterScene_UnmarkedMintResendReturningZeroRollsBack(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6922)
		targetID = uint64(7922)
	)
	seedSceneOnNode(mr, testZoneId, targetID, "10", "5")
	testMinter(t, sc).setResendAfterEval(1)

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrKafkaRoute, resp.ErrorCode, "重发回 0 不再当成并发冲突")
	assert.Empty(t, *captured)
	assert.False(t, mr.Exists(getPlayerLocationKey(playerID)), "首发写下的落点被撤掉")
	assert.Equal(t, "2", ownerEpochRaw(t, sc, playerID))
	assert.Equal(t, "5", sceneCountText(sc, targetID), "目标场景预占退回")
}

// 凭标记的跨节点交接,铸造 EVAL 结果未知:按计划好的落点回滚到源节点,两段回执照常走 —— 这里回滚之后的落盘确认到了份数,
// 回执改成正式回执,源端可以凭它采纳 3。不推路由,回 7;两边人数与回滚之后的 location 对得上。
func TestEnterScene_HandoffMintResultUnknownRollsBackWithReceipt(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6923)
		oldScene = uint64(7923)
		targetID = uint64(7924)
	)
	seedSceneOnNode(mr, testZoneId, oldScene, "10", "3")
	seedSceneOnNode(mr, testZoneId, targetID, "20", "4")
	seedNodeMembers(mr, testZoneId, "10", 3)
	seedNodeMembers(mr, testZoneId, "20", 4)
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, oldScene, "10", testZoneId))
	adoptSceneMember(mr, oldScene, playerID)
	adoptNodeMember(mr, testZoneId, "10", playerID)
	writeHandoffMarker(t, sc, playerID, 1)
	withReachableSceneNode(t, sc, "10", "20")
	testMinter(t, sc).setUnknownAfterEval(1)
	testMinter(t, sc).resetScriptCalls()

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrKafkaRoute, resp.ErrorCode)
	assert.Equal(t, uint64(3), resp.OwnerEpochAfterRollback)
	assert.Empty(t, *captured)
	assert.Equal(t, "3", ownerEpochRaw(t, sc, playerID), "铸出 2 → 回滚到 3")
	loc, locErr := GetPlayerLocation(context.Background(), sc, playerID)
	require.NoError(t, locErr)
	require.NotNil(t, loc)
	assert.Equal(t, oldScene, loc.SceneId)
	assert.Equal(t, "10", loc.NodeId)
	assert.Equal(t, "1:1757000000000", loc.RollbackReceipt, "回滚之后的确认到了份数,改成正式回执")
	assert.Equal(t, "4", sceneCountText(sc, targetID), "目标场景预占退回")
	assert.Equal(t, "3", sceneCountText(sc, oldScene), "旧场景身份不动")
	assert.Equal(t, "4", nodeCountText(sc, testZoneId, "20"), "目标节点预占退回")
	assert.Equal(t, "3", nodeCountText(sc, testZoneId, "10"), "旧节点身份不动")
	assert.Equal(t, []string{luaMintEpochAndSetLocation, luaConfirmPendingRollback}, testMinter(t, sc).scriptCalls(),
		"回滚走共享连接池,铸造通道只收到铸造与之后的落盘确认")
}

// 跨 zone 第一条腿凭标记铸造,EVAL 结果未知:不推重定向,按计划好的等待落点回滚,location 回到源 zone,旧场景人数不动;回 7。
func TestEnterScene_CrossZoneMintResultUnknownRollsBack(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6925)
		oldScene = uint64(7927)
	)
	seedSceneOnNode(mr, 1, oldScene, "10", "3")
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, oldScene, "10", 1))
	adoptSceneMember(mr, oldScene, playerID)
	writeHandoffMarker(t, sc, playerID, 1)
	seedDefaultWorldChannel(t, mr, 2)
	testMinter(t, sc).setUnknownAfterEval(1)

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
	assert.Nil(t, resp.Redirect, "结果未知时不推重定向")
	assert.Equal(t, uint64(3), resp.OwnerEpochAfterRollback)
	assert.Empty(t, *captured)
	assert.Equal(t, "3", ownerEpochRaw(t, sc, playerID))
	loc, locErr := GetPlayerLocation(context.Background(), sc, playerID)
	require.NoError(t, locErr)
	require.NotNil(t, loc)
	assert.Equal(t, uint32(1), loc.ZoneId, "location 回到源 zone")
	assert.Equal(t, oldScene, loc.SceneId)
	assert.Equal(t, "1:1757000000000", loc.RollbackReceipt)
	assert.Equal(t, "3", sceneCountText(sc, oldScene), "旧场景人数不动")
}

// 同节点 epoch 0 的铸造没确认落盘、keep 回滚之后,键里留着没确认的 1,location 仍记 0。重试同一次换图时必须再铸一次
// (新值确认之后才下发),不能把 1 原样下发:1 可能随 Redis 回退消失(zone-redis-regression-guard-20261022.md §4.3.4)。
func TestEnterScene_SameNodeLeftoverOfUnconfirmedZeroMintIsReminted(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6926)
		oldScene = uint64(7928)
		targetID = uint64(7929)
	)
	seedSceneOnNode(mr, testZoneId, oldScene, "10", "3")
	seedSceneOnNode(mr, testZoneId, targetID, "10", "4")
	_, err := placePlayerLocation(sc, playerID, oldScene, "10", testZoneId, placementGuard{observedEpoch: 0, mint: false})
	require.NoError(t, err)
	adoptSceneMember(mr, oldScene, playerID)
	withReachableSceneNode(t, sc, "10")
	enter := func() *scene_manager.EnterSceneResponse {
		resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
			PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
			GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
		})
		require.NoError(t, err)
		return resp
	}

	testMinter(t, sc).setDurability(svc.MintDurabilityNotDurable, errTestWaitAOFShort)
	require.Equal(t, constants.ErrKafkaRoute, enter().ErrorCode)
	require.Equal(t, "1", ownerEpochRaw(t, sc, playerID), "keep 回滚:键停在没确认的 1")
	require.Empty(t, *captured)

	testMinter(t, sc).setDurability(svc.MintDurabilityConfirmed, nil)
	require.Equal(t, uint32(0), enter().ErrorCode)
	require.Len(t, *captured, 1)
	assert.Equal(t, uint64(2), decodeRoutePlayerEvent(t, (*captured)[0]).OwnerEpoch, "再铸一次:下发确认过的 2,不下发没确认的 1")
	assert.Equal(t, "2", ownerEpochRaw(t, sc, playerID))
}

// 同上的残留状态(键 1 没确认、location 记 0),玩家重连回原场景(同落点):不铸造,下发 location 记的 0,不下发没确认的 1。
func TestEnterScene_SamePlacementDoesNotRouteLeftoverOfZeroMint(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6927)
		oldScene = uint64(7930)
		targetID = uint64(7931)
	)
	seedSceneOnNode(mr, testZoneId, oldScene, "10", "3")
	seedSceneOnNode(mr, testZoneId, targetID, "10", "4")
	_, err := placePlayerLocation(sc, playerID, oldScene, "10", testZoneId, placementGuard{observedEpoch: 0, mint: false})
	require.NoError(t, err)
	adoptSceneMember(mr, oldScene, playerID)
	withReachableSceneNode(t, sc, "10")

	testMinter(t, sc).setDurability(svc.MintDurabilityNotDurable, errTestWaitAOFShort)
	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	require.Equal(t, constants.ErrKafkaRoute, resp.ErrorCode)
	require.Equal(t, "1", ownerEpochRaw(t, sc, playerID), "keep 回滚:键停在没确认的 1")

	testMinter(t, sc).setDurability(svc.MintDurabilityConfirmed, nil)
	resp, err = NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: oldScene, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	require.Equal(t, uint32(0), resp.ErrorCode)
	require.Len(t, *captured, 1)
	assert.Equal(t, uint64(0), decodeRoutePlayerEvent(t, (*captured)[0]).OwnerEpoch, "同落点重连下发 location 记的 0")
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID), "同落点重连不铸造")
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
	count := sceneCountText(sc, targetID)
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
	adoptSceneMember(mr, oldScene, playerID)
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

// ---------------------------------------------------------------------------
// 两段回执(docs/design/zone-redis-regression-guard-20261022.md §4.3.4)
//
// 凭标记的回滚(推路由失败或铸造没确认落盘)走共享连接池,恢复出的 location 先带待确认回执;之后经铸造通道写回原值 + WAITAOF 确认
// 落盘,才改成正式回执。location 带待确认回执时,不铸造的落点下发它记的 epoch 之前先确认。下面的直接 EVAL 用例用
// owner_epoch 上的 TTL 判断脚本有没有对它发写(SET 会清掉 TTL),理由同 TestMintEpochLuaReplayBranchRewritesEpochForWaitAOF。
// ---------------------------------------------------------------------------

// 回滚脚本的重放分支(回 2)与回 0 的分支一个字节都不写:落盘确认由铸造通道上的确认脚本负责,回滚脚本不为它写回原值。
func TestRollbackLuaReplayBranchWritesNothing(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	const playerID = uint64(6921)
	marker := ownerepoch.Handoff{Epoch: 1, SavedAtMs: 1757000000000}.String()
	mr.Set(ownerepoch.OwnerEpochKey(playerID), "2")
	mr.Set(getPlayerLocationKey(playerID), "placed-bytes")
	mr.Set(ownerepoch.HandoffKey(playerID), marker)
	placed := placedLocation{epoch: 2, minted: true, raw: "placed-bytes"}
	plan := routeRollbackPlan{mode: rollbackModeBump, restoredRaw: "restored-bytes", epochAfter: 3,
		requiredMarker: marker, forwardMarker: "3:1757000000000"}
	args := rollbackPlacementArgs(placed, plan)

	first, err := sc.Redis.Eval(luaRollbackPlayerPlacement, rollbackPlacementKeys(playerID), args...)
	require.NoError(t, err)
	require.Equal(t, "-3", fmt.Sprint(first))

	mr.SetTTL(ownerepoch.OwnerEpochKey(playerID), time.Hour)
	mr.Del(ownerepoch.HandoffKey(playerID)) // 源端取证删掉了本族标记
	replay, err := sc.Redis.Eval(luaRollbackPlayerPlacement, rollbackPlacementKeys(playerID), args...)
	require.NoError(t, err)
	assert.Equal(t, "2", fmt.Sprint(replay), "重放识别命中")
	assert.Equal(t, "3", ownerEpochRaw(t, sc, playerID), "不再 INCR")
	assert.Greater(t, mr.TTL(ownerepoch.OwnerEpochKey(playerID)), time.Duration(0), "重放分支一个字节都不写")
	assert.False(t, mr.Exists(ownerepoch.HandoffKey(playerID)), "删掉的转写标记不能被重新写活")

	mr.Set(getPlayerLocationKey(playerID), "someone-else-bytes")
	mr.SetTTL(ownerepoch.OwnerEpochKey(playerID), time.Hour)
	other, err := sc.Redis.Eval(luaRollbackPlayerPlacement, rollbackPlacementKeys(playerID), args...)
	require.NoError(t, err)
	assert.Equal(t, "0", fmt.Sprint(other))
	assert.Greater(t, mr.TTL(ownerepoch.OwnerEpochKey(playerID)), time.Duration(0), "回 0 的分支一个字节都不写")
}

// 确认脚本:三把键原样未变才写回 owner_epoch 原值(给 WAITAOF 一个可等的偏移),任一变了一个字节都不写。
func TestConfirmPendingRollbackLuaWritesBackOnlyWhenUnchanged(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	const playerID = uint64(6922)
	keys := rollbackReceiptKeys(playerID)
	mr.Set(ownerepoch.OwnerEpochKey(playerID), "3")
	mr.Set(getPlayerLocationKey(playerID), "pending-bytes")

	mr.SetTTL(ownerepoch.OwnerEpochKey(playerID), time.Hour)
	ok, err := sc.Redis.Eval(luaConfirmPendingRollback, keys, "3", "pending-bytes", "")
	require.NoError(t, err)
	assert.Equal(t, "1", fmt.Sprint(ok))
	assert.Equal(t, "3", ownerEpochRaw(t, sc, playerID))
	assert.Zero(t, mr.TTL(ownerepoch.OwnerEpochKey(playerID)), "确认分支对 owner_epoch 发了一次写")

	for name, args := range map[string][]any{
		"epoch_changed":    {"4", "pending-bytes", ""},
		"location_changed": {"3", "other-bytes", ""},
		"tag_changed":      {"3", "pending-bytes", "some-tag"},
	} {
		mr.SetTTL(ownerepoch.OwnerEpochKey(playerID), time.Hour)
		got, err := sc.Redis.Eval(luaConfirmPendingRollback, keys, args...)
		require.NoError(t, err, name)
		assert.Equal(t, "0", fmt.Sprint(got), name)
		assert.Greater(t, mr.TTL(ownerepoch.OwnerEpochKey(playerID)), time.Duration(0), "%s: 一个字节都不写", name)
	}
}

// 待确认回执改成正式回执时,修订侧车(location 原文的摘要)跟着改;侧车、原文或 epoch 不是期望的那一份时一个字节都不写;
// 重复改写认成已改写。改写之后按快照读 location 必须通过侧车校验。
func TestUpgradeRollbackReceiptRewritesRevisionTag(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	const playerID = uint64(6923)
	log := logx.WithContext(context.Background())
	site := metrics.OwnerEpochRollbackReceiptSiteRollback
	pendingLoc := &scene_manager.PlayerLocation{SceneId: 7923, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 3,
		RollbackReceipt: ownerepoch.PendingRollbackReceipt("1:1757000000000")}
	pendingRaw := identMarshalLocation(t, pendingLoc)
	finalLoc := gproto.Clone(pendingLoc).(*scene_manager.PlayerLocation)
	finalLoc.RollbackReceipt = "1:1757000000000"
	finalRaw := identMarshalLocation(t, finalLoc)
	pendingTag, finalTag := locationRevisionTag(5, pendingRaw), locationRevisionTag(5, finalRaw)
	require.NotEqual(t, pendingTag, finalTag)
	mr.Set(ownerepoch.OwnerEpochKey(playerID), "3")
	mr.Set(getPlayerLocationKey(playerID), pendingRaw)
	mr.Set(locationRevisionKey(playerID), pendingTag)

	assert.Equal(t, metrics.OwnerEpochRollbackReceiptSuperseded,
		upgradeRollbackReceipt(sc, log, site, playerID, 3, pendingRaw, "stale-tag", finalRaw, finalTag), "侧车不是期望的那一份")
	assert.Equal(t, metrics.OwnerEpochRollbackReceiptSuperseded,
		upgradeRollbackReceipt(sc, log, site, playerID, 4, pendingRaw, pendingTag, finalRaw, finalTag), "epoch 不是期望的那一份")
	raw, _ := mr.Get(getPlayerLocationKey(playerID))
	require.Equal(t, pendingRaw, raw, "复核不过一个字节都不写")

	assert.Equal(t, metrics.OwnerEpochRollbackReceiptConfirmed,
		upgradeRollbackReceipt(sc, log, site, playerID, 3, pendingRaw, pendingTag, finalRaw, finalTag))
	raw, _ = mr.Get(getPlayerLocationKey(playerID))
	tag, _ := mr.Get(locationRevisionKey(playerID))
	assert.Equal(t, finalRaw, raw)
	assert.Equal(t, finalTag, tag, "侧车跟着原文改")
	loc, _, _, err := getPlayerLocationSnapshot(sc, playerID)
	require.NoError(t, err, "改写之后侧车校验必须通过")
	assert.Equal(t, "1:1757000000000", loc.RollbackReceipt)

	assert.Equal(t, metrics.OwnerEpochRollbackReceiptConfirmed,
		upgradeRollbackReceipt(sc, log, site, playerID, 3, pendingRaw, pendingTag, finalRaw, finalTag), "重复改写认成已改写")
}

// 源端见到待确认回执按 B5′ 收口(踢线、不存盘销毁)之后,玩家重登落回原场景:location 仍带待确认回执。同落点重连不铸造、
// 要原样下发 location 记的 epoch,下发之前先经铸造通道确认落盘,并把回执改成正式回执;路由带的就是这个值。
func TestEnterScene_SamePlacementConfirmsPendingRollbackBeforeRouting(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	const (
		playerID = uint64(6924)
		sceneID  = uint64(7924)
	)
	seedSceneOnNode(mr, testZoneId, sceneID, "10", "1")
	seedNodeMembers(mr, testZoneId, "10", 1)
	identSeedLocation(t, sc, playerID, &scene_manager.PlayerLocation{SceneId: sceneID, NodeId: "10", ZoneId: testZoneId,
		OwnerEpoch: 3, UpdateTime: 12345, RollbackReceipt: ownerepoch.PendingRollbackReceipt("1:1757000000000")})
	adoptSceneMember(mr, sceneID, playerID)
	adoptNodeMember(mr, testZoneId, "10", playerID)

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: sceneID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	require.Equal(t, uint32(0), resp.ErrorCode)
	require.Len(t, *captured, 1)
	assert.Equal(t, uint64(3), decodeRoutePlayerEvent(t, (*captured)[0]).OwnerEpoch, "确认之后下发的仍是这个值,不铸造")
	got := identLocation(t, sc, playerID)
	require.NotNil(t, got)
	assert.Equal(t, "1:1757000000000", got.RollbackReceipt, "确认落盘之后改成正式回执")
	assert.Equal(t, "3", ownerEpochRaw(t, sc, playerID))
	assert.Equal(t, []string{luaConfirmPendingRollback}, testMinter(t, sc).scriptCalls())
}

// 同上,但确认不到份数:不下发、回 7,location 原样留在待确认回执,人数不动。
func TestEnterScene_SamePlacementRefusesToRouteUnconfirmedPendingRollback(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	const (
		playerID = uint64(6925)
		sceneID  = uint64(7925)
	)
	seedSceneOnNode(mr, testZoneId, sceneID, "10", "1")
	seedNodeMembers(mr, testZoneId, "10", 1)
	seededRaw := identSeedLocation(t, sc, playerID, &scene_manager.PlayerLocation{SceneId: sceneID, NodeId: "10", ZoneId: testZoneId,
		OwnerEpoch: 3, UpdateTime: 12345, RollbackReceipt: ownerepoch.PendingRollbackReceipt("1:1757000000000")})
	adoptSceneMember(mr, sceneID, playerID)
	adoptNodeMember(mr, testZoneId, "10", playerID)
	testMinter(t, sc).setDurability(svc.MintDurabilityNotDurable, errTestWaitAOFShort)

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: sceneID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrKafkaRoute, resp.ErrorCode)
	assert.Empty(t, *captured, "没确认落盘的 epoch 不得下发")
	assert.Equal(t, seededRaw, identLocationRaw(t, sc, playerID), "location 原样,仍是待确认回执")
	assert.Equal(t, "3", ownerEpochRaw(t, sc, playerID))
	assert.Equal(t, "1", sceneCountText(sc, sceneID))
	assert.Equal(t, "1", nodeCountText(sc, testZoneId, "10"))
}

// 铸造的落点不需要确认待确认回执:凭标记的跨节点交接从带待确认回执的 location 出发,照常铸造新值并整条重写 location,
// 铸造通道只收到铸造脚本。
func TestEnterScene_MintingPlacementSkipsPendingRollbackConfirmation(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)
	const (
		playerID = uint64(6926)
		oldScene = uint64(7926)
		targetID = uint64(7927)
	)
	seedSceneOnNode(mr, testZoneId, oldScene, "10", "1")
	seedSceneOnNode(mr, testZoneId, targetID, "20", "0")
	identSeedLocation(t, sc, playerID, &scene_manager.PlayerLocation{SceneId: oldScene, NodeId: "10", ZoneId: testZoneId,
		OwnerEpoch: 3, UpdateTime: 12345, RollbackReceipt: ownerepoch.PendingRollbackReceipt("1:1757000000000")})
	adoptSceneMember(mr, oldScene, playerID)
	writeHandoffMarker(t, sc, playerID, 3)
	withReachableSceneNode(t, sc, "10", "20")

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	require.Equal(t, uint32(0), resp.ErrorCode)
	require.Len(t, *captured, 1)
	assert.Equal(t, uint64(4), decodeRoutePlayerEvent(t, (*captured)[0]).OwnerEpoch)
	got := identLocation(t, sc, playerID)
	require.NotNil(t, got)
	assert.Equal(t, "20", got.NodeId)
	assert.Empty(t, got.RollbackReceipt, "新落点整条重写 location,不带任何回执")
	assert.Equal(t, []string{luaMintEpochAndSetLocation}, testMinter(t, sc).scriptCalls())
}

// 推路由失败的回滚同样两段(§4.3.4):铸造确认过,回滚写下的 3 是另一个值,要单独确认落盘。确认不到份数时回执停在
// 待确认形态,源端见到它不采纳(B5′);回滚本身走共享连接池,铸造通道依次只收到铸造与确认两段脚本。
func TestEnterScene_RouteFailureRollbackWithoutConfirmationLeavesPendingReceipt(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Kafka = &countingKafkaWriter{err: errors.New("broker unavailable")}
	sc.Config.KafkaWriteTimeoutSeconds = 1
	const (
		playerID = uint64(6927)
		oldScene = uint64(7928)
		targetID = uint64(7929)
	)
	seedSceneOnNode(mr, testZoneId, oldScene, "10", "3")
	seedSceneOnNode(mr, testZoneId, targetID, "20", "4")
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, oldScene, "10", testZoneId))
	adoptSceneMember(mr, oldScene, playerID)
	writeHandoffMarker(t, sc, playerID, 1)
	withReachableSceneNode(t, sc, "10", "20")
	minter := testMinter(t, sc)
	minter.queueDurability(
		fakeDurability{durability: svc.MintDurabilityConfirmed},
		fakeDurability{durability: svc.MintDurabilityNotDurable, err: errTestWaitAOFShort},
	)
	minter.resetScriptCalls()

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrKafkaRoute, resp.ErrorCode)
	assert.Equal(t, uint64(3), resp.OwnerEpochAfterRollback)
	assert.Equal(t, "3", ownerEpochRaw(t, sc, playerID))
	loc, locErr := GetPlayerLocation(context.Background(), sc, playerID)
	require.NoError(t, locErr)
	require.NotNil(t, loc)
	assert.Equal(t, "10", loc.NodeId)
	assert.Equal(t, ownerepoch.PendingRollbackReceipt("1:1757000000000"), loc.RollbackReceipt,
		"回滚没确认落盘:回执停在待确认形态,源端不得采纳")
	assert.Equal(t, []string{luaMintEpochAndSetLocation, luaConfirmPendingRollback}, minter.scriptCalls())
}
