package logic

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2/server"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/logx"
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

// fakeOwnerEpochMinter 是单测用的 svc.OwnerEpochMinter:铸造 EVAL 走玩家域句柄(与生产的铸造通道同域;miniredis 没有
// WAITAOF),落盘结论由用例指定,缺省 Confirmed。三个 svcCtx 构造助手(newTestSvcCtx、newTestSvcCtxWithWorldScenes、
// newIntegrationSvcCtxWithRedisOptions)缺省装上它。
//
// 墙钟缺省 0:墙钟地板不起作用,铸造退化为观察值 + 1,既有用例里的 1 / 2 / 3 照旧成立;要测地板的用例用 setNowMs
// 钉一个固定毫秒(单测不读真实墙钟,AGENTS §11.4)。
type fakeOwnerEpochMinter struct {
	rds goredis.UniversalClient

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
	// withholdEval > 0 时接下来这么多次调用**不执行** EVAL,只把它原样记进 withheld,回 svc.ErrOwnerEpochMintResultUnknown:
	// 模拟首发卡在连接里、调用方已经放弃(铸造通道的「结果未知」,而且首发还没到 Redis)。用例之后用 deliverWithheld 让它迟到执行。
	withholdEval int
	withheld     []withheldMintEval
}

// withheldMintEval 是一条被扣住、还没到 Redis 的 EVAL(脚本、KEYS、ARGV 原样)。
type withheldMintEval struct {
	script string
	keys   []string
	args   []any
}

// fakeDurability 是一次排队的落盘结论。
type fakeDurability struct {
	durability svc.MintDurability
	err        error
}

var _ svc.OwnerEpochMinter = (*fakeOwnerEpochMinter)(nil)

func newFakeOwnerEpochMinter(rds goredis.UniversalClient) *fakeOwnerEpochMinter {
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
	withhold := !notSent && !unknown && !resend && m.withholdEval > 0
	if withhold {
		m.withholdEval--
		m.withheld = append(m.withheld, withheldMintEval{script: script, keys: append([]string(nil), keys...), args: append([]any(nil), args...)})
	}
	m.mu.Unlock()
	if notSent {
		return svc.MintEvalResult{}, svc.ErrOwnerEpochMintNotSent
	}
	if withhold {
		return svc.MintEvalResult{Attempts: 3}, fmt.Errorf("%w after 3 attempt(s): test transport error, first attempt still in flight", svc.ErrOwnerEpochMintResultUnknown)
	}
	reply, err := m.rds.Eval(ctx, script, keys, args...).Result()
	if err != nil {
		return svc.MintEvalResult{Attempts: 1}, err
	}
	if unknown {
		return svc.MintEvalResult{Attempts: 3}, fmt.Errorf("%w after 3 attempt(s): test transport error", svc.ErrOwnerEpochMintResultUnknown)
	}
	attempts := 1
	if resend {
		reply, err = m.rds.Eval(ctx, script, keys, args...).Result()
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

// setWithholdEval 让接下来 n 次调用不执行 EVAL、只记下来,并回「结果未知」(首发还没到 Redis)。
func (m *fakeOwnerEpochMinter) setWithholdEval(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.withholdEval = n
}

// deliverWithheld 让被扣住的第 i 条 EVAL 现在才执行(迟到的首发),返回脚本回复的十进制文本。
func (m *fakeOwnerEpochMinter) deliverWithheld(t *testing.T, i int) string {
	t.Helper()
	m.mu.Lock()
	held := append([]withheldMintEval(nil), m.withheld...)
	m.mu.Unlock()
	require.Less(t, i, len(held), "没有第 %d 条被扣住的 EVAL", i)
	reply, err := m.rds.Eval(context.Background(), held[i].script, held[i].keys, held[i].args...).Result()
	require.NoError(t, err)
	return fmt.Sprint(reply)
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
	forwarded, _ := playerRedisGet(sc, ownerepoch.HandoffKey(playerID))
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
	oldRaw, _ := playerRedisGet(sc, getPlayerLocationKey(playerID))
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
	restoredRaw, _ := playerRedisGet(sc, getPlayerLocationKey(playerID))
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

// --- 结果未知而首发还没到 Redis:作废(§4.3.5) ---------------------------------------------------------------

// 首次落点的铸造 EVAL 结果未知,而且首发还卡在连接里没执行:收尾的回滚脚本认出「状态还是观察时那一份」,直接写成回滚之后的
// 状态(epoch 越过本该铸出的值)。之后首发才到 Redis,CAS 不过,一个字节都写不进去。以前回滚 CAS 不过、什么都不改,迟到的
// 首发照样生效,留下一条没人知道的落点。
func TestEnterScene_FirstPlacementUnknownMintNotYetAppliedIsVoided(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6931)
		targetID = uint64(7941)
	)
	seedSceneOnNode(mr, testZoneId, targetID, "10", "5")
	minter := testMinter(t, sc)
	minter.setWithholdEval(1)

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrKafkaRoute, resp.ErrorCode)
	assert.Equal(t, uint64(2), resp.OwnerEpochAfterRollback)
	assert.Empty(t, *captured)
	assert.False(t, mr.Exists(getPlayerLocationKey(playerID)))
	assert.Equal(t, "2", ownerEpochRaw(t, sc, playerID), "作废:epoch 越过本该铸出的 1,与「生效了再回滚」同一个终态")
	assert.Equal(t, "5", sceneCountText(sc, targetID), "目标场景预占退回")

	assert.Equal(t, "0", minter.deliverWithheld(t, 0), "迟到的首发 CAS 不过")
	assert.False(t, mr.Exists(getPlayerLocationKey(playerID)), "迟到的首发一个字节都写不进去")
	assert.Equal(t, "2", ownerEpochRaw(t, sc, playerID))
}

// 凭标记的跨节点交接,铸造 EVAL 结果未知而首发还没执行:作废只删掉所凭的标记,epoch 与 location 一个字节都不动。源端仍
// 持有玩家、手里的 epoch 仍然有效,收到 7 之后取证读到「epoch 没变」照常解冻。迟到的首发过不了标记比对。没有回执要确认,
// 铸造通道只收到那一条铸造。
func TestEnterScene_HandoffUnknownMintNotYetAppliedVoidsMarker(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6932)
		oldScene = uint64(7942)
		targetID = uint64(7943)
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
	seededRaw := identLocationRaw(t, sc, playerID)
	minter := testMinter(t, sc)
	minter.setWithholdEval(1)
	minter.resetScriptCalls()

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrKafkaRoute, resp.ErrorCode)
	assert.Zero(t, resp.OwnerEpochAfterRollback, "epoch 没动,不回显")
	assert.Empty(t, *captured)
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID), "源端手里的 epoch 仍然有效")
	assert.Equal(t, seededRaw, identLocationRaw(t, sc, playerID), "location 一个字节都不动,没有回执")
	assert.False(t, mr.Exists(ownerepoch.HandoffKey(playerID)), "所凭的标记被删掉")
	assert.Equal(t, "4", sceneCountText(sc, targetID), "目标场景预占退回")
	assert.Equal(t, "3", sceneCountText(sc, oldScene), "旧场景身份不动")
	assert.Equal(t, "4", nodeCountText(sc, testZoneId, "20"), "目标节点预占退回")
	assert.Equal(t, "3", nodeCountText(sc, testZoneId, "10"), "旧节点身份不动")
	assert.Equal(t, []string{luaMintEpochAndSetLocation}, minter.scriptCalls(), "没有回执要确认")

	assert.Equal(t, "-1", minter.deliverWithheld(t, 0), "迟到的首发过不了标记比对")
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID))
	assert.Equal(t, seededRaw, identLocationRaw(t, sc, playerID))
}

// 跨 zone 第一条腿同理:凭标记的铸造结果未知而首发还没执行,删掉所凭的标记,不推重定向,location 留在源 zone 原样不动。
func TestEnterScene_CrossZoneUnknownMintNotYetAppliedVoidsMarker(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6933)
		oldScene = uint64(7944)
	)
	seedSceneOnNode(mr, 1, oldScene, "10", "3")
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, oldScene, "10", 1))
	adoptSceneMember(mr, oldScene, playerID)
	writeHandoffMarker(t, sc, playerID, 1)
	seedDefaultWorldChannel(t, mr, 2)
	seededRaw := identLocationRaw(t, sc, playerID)
	minter := testMinter(t, sc)
	minter.setWithholdEval(1)

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
	assert.Zero(t, resp.OwnerEpochAfterRollback)
	assert.Empty(t, *captured)
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID))
	assert.Equal(t, seededRaw, identLocationRaw(t, sc, playerID), "location 留在源 zone,原样不动")
	assert.False(t, mr.Exists(ownerepoch.HandoffKey(playerID)), "所凭的标记被删掉")
	assert.Equal(t, "3", sceneCountText(sc, oldScene), "旧场景人数不动")

	assert.Equal(t, "-1", minter.deliverWithheld(t, 0), "迟到的首发过不了标记比对")
	assert.Equal(t, seededRaw, identLocationRaw(t, sc, playerID))
}

// 同节点 epoch 0 的铸造(keep)不作废:持有节点可能已经缓存了铸出值,抬 epoch 会废黜它。结果未知而首发没执行时,
// 回滚照旧什么都不改;迟到的首发照样生效,但它留下的值没有已确认位点,不会被原样下发(见下面「已确认位点」一组)。
func TestEnterScene_SameNodeZeroEpochUnknownMintIsNotVoided(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6934)
		oldScene = uint64(7945)
		targetID = uint64(7946)
	)
	seedSceneOnNode(mr, testZoneId, oldScene, "10", "3")
	seedSceneOnNode(mr, testZoneId, targetID, "10", "4")
	_, err := placePlayerLocation(sc, playerID, oldScene, "10", testZoneId, placementGuard{observedEpoch: 0, mint: false})
	require.NoError(t, err)
	adoptSceneMember(mr, oldScene, playerID)
	withReachableSceneNode(t, sc, "10")
	seededRaw := identLocationRaw(t, sc, playerID)
	minter := testMinter(t, sc)
	minter.setWithholdEval(1)

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrKafkaRoute, resp.ErrorCode)
	assert.Empty(t, *captured)
	assert.Equal(t, "", ownerEpochRaw(t, sc, playerID), "keep 不作废:epoch 键不动")
	assert.Equal(t, seededRaw, identLocationRaw(t, sc, playerID))

	assert.Equal(t, "1", minter.deliverWithheld(t, 0), "迟到的首发照样生效")
	assert.False(t, mr.Exists(ownerepoch.ConfirmedKey(playerID)), "它铸出的值没有已确认位点")
}

// 回滚脚本的作废分支,不凭标记(只有带上 ARGV[11..14] 才走):
//   - 不带作废参数:首发没生效时照旧回 0,什么都不改(推路由失败的回滚不作废);
//   - 带作废参数、状态还是观察到的那一份:写成回滚之后的状态,回 6;原样重发走重放识别回 2;
//   - 带作废参数、首发先到已生效:照常回滚,回 -after;
//   - 状态被别的请求改过:回 0,一个字节都不写。
func TestRollbackLuaVoidsUnmarkedMintThatHasNotApplied(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	const playerID = uint64(6935)
	keys := rollbackPlacementKeys(playerID)
	placed := placedLocation{epoch: 8, minted: true, raw: "placed-bytes",
		observedEpoch: 5, observedRaw: "observed-bytes", observedKnown: true}
	plan := routeRollbackPlan{mode: rollbackModeBump, restoredRaw: "restored-bytes", epochAfter: 9}
	plainArgs := rollbackPlacementArgs(placed, plan)
	plan.voidUnapplied = true
	voidArgs := rollbackPlacementArgs(placed, plan)
	require.Len(t, plainArgs, 10, "不作废的回滚不带 ARGV[11..14]")
	require.Len(t, voidArgs, 14)
	locKey, epochKey := getPlayerLocationKey(playerID), ownerepoch.OwnerEpochKey(playerID)
	mr.Set(epochKey, "5")
	mr.Set(locKey, "observed-bytes")

	got, err := playerRedisEval(sc, luaRollbackPlayerPlacement, keys, plainArgs...)
	require.NoError(t, err)
	assert.Equal(t, "0", fmt.Sprint(got), "不带作废参数:首发没生效时什么都不改")
	assert.Equal(t, "5", ownerEpochRaw(t, sc, playerID))

	got, err = playerRedisEval(sc, luaRollbackPlayerPlacement, keys, voidArgs...)
	require.NoError(t, err)
	assert.Equal(t, "6", fmt.Sprint(got))
	assert.Equal(t, "9", ownerEpochRaw(t, sc, playerID), "epoch 越过本该铸出的 8")
	raw, _ := mr.Get(locKey)
	assert.Equal(t, "restored-bytes", raw, "location 写成回滚之后的那一份")

	replay, err := playerRedisEval(sc, luaRollbackPlayerPlacement, keys, voidArgs...)
	require.NoError(t, err)
	assert.Equal(t, "2", fmt.Sprint(replay), "作废之后的状态就是回滚之后的状态,原样重发走重放识别")

	mr.Set(epochKey, "8")
	mr.Set(locKey, "placed-bytes")
	got, err = playerRedisEval(sc, luaRollbackPlayerPlacement, keys, voidArgs...)
	require.NoError(t, err)
	assert.Equal(t, "-9", fmt.Sprint(got), "首发先到、已生效:带着作废参数照常回滚")

	mr.Set(epochKey, "5")
	mr.Set(locKey, "someone-else-bytes")
	mr.SetTTL(epochKey, time.Hour)
	got, err = playerRedisEval(sc, luaRollbackPlayerPlacement, keys, voidArgs...)
	require.NoError(t, err)
	assert.Equal(t, "0", fmt.Sprint(got), "状态既不是观察到的那一份,也不是本次落点")
	assert.Greater(t, mr.TTL(epochKey), time.Duration(0), "回 0 的分支一个字节都不写")
}

// 回滚脚本的作废分支,凭标记:只删所凭的那一份标记;标记已不在或已换成别的,同样回 7 而不碰它。epoch 与 location 都不写。
func TestRollbackLuaVoidOfMarkedMintOnlyRemovesItsMarker(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	const playerID = uint64(6936)
	keys := rollbackPlacementKeys(playerID)
	marker := ownerepoch.Handoff{Epoch: 5, SavedAtMs: 1757000000000}.String()
	placed := placedLocation{epoch: 8, minted: true, raw: "placed-bytes",
		observedEpoch: 5, observedRaw: "observed-bytes", observedKnown: true}
	plan := routeRollbackPlan{mode: rollbackModeBump, restoredRaw: "restored-bytes", epochAfter: 9,
		requiredMarker: marker, forwardMarker: "9:1757000000000", voidUnapplied: true}
	args := rollbackPlacementArgs(placed, plan)
	locKey, epochKey, markerKey := getPlayerLocationKey(playerID), ownerepoch.OwnerEpochKey(playerID), ownerepoch.HandoffKey(playerID)
	mr.Set(epochKey, "5")
	mr.Set(locKey, "observed-bytes")

	const newerMarker = "5:1757000009999"
	for _, c := range []struct {
		name   string
		seeded string // "" = 标记已不在
		left   string // 执行之后标记键的值;"" = 不存在
	}{
		{"marker_present", marker, ""},
		{"marker_gone", "", ""},
		{"marker_replaced", newerMarker, newerMarker},
	} {
		mr.Del(markerKey)
		if c.seeded != "" {
			mr.Set(markerKey, c.seeded)
		}
		mr.SetTTL(epochKey, time.Hour)
		got, err := playerRedisEval(sc, luaRollbackPlayerPlacement, keys, args...)
		require.NoError(t, err, c.name)
		assert.Equal(t, "7", fmt.Sprint(got), c.name)
		assert.Greater(t, mr.TTL(epochKey), time.Duration(0), "%s: epoch 键没被写过", c.name)
		raw, _ := mr.Get(locKey)
		assert.Equal(t, "observed-bytes", raw, "%s: location 不动", c.name)
		if c.left == "" {
			assert.False(t, mr.Exists(markerKey), c.name)
		} else {
			left, _ := mr.Get(markerKey)
			assert.Equal(t, c.left, left, "%s: 别的标记不碰", c.name)
		}
	}
}

// --- 已确认位点(§4.3.5) -------------------------------------------------------------------------------------

// 铸造确认落盘之后记下已确认位点(带 TTL);没确认落盘的铸造不记。
func TestPlacePlayerLocation_ConfirmedMintRecordsConfirmedMark(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	const playerID = uint64(6937)
	key := ownerepoch.ConfirmedKey(playerID)

	testMinter(t, sc).setDurability(svc.MintDurabilityNotDurable, errTestWaitAOFShort)
	_, err := placePlayerLocation(sc, playerID, 7950, "10", testZoneId, placementGuard{observedEpoch: 0, mint: true})
	require.ErrorIs(t, err, errOwnerEpochMintNotDurable)
	assert.False(t, mr.Exists(key), "没确认落盘的铸造不记位点")

	testMinter(t, sc).setDurability(svc.MintDurabilityConfirmed, nil)
	placed, err := placePlayerLocation(sc, playerID, 7950, "10", testZoneId, placementGuard{observedEpoch: 1, mint: true})
	require.NoError(t, err)
	got, _ := mr.Get(key)
	assert.Equal(t, strconv.FormatUint(placed.epoch, 10), got)
	assert.Greater(t, mr.TTL(key), time.Duration(0), "位点带 TTL,过期只会多确认一次")
}

// 同落点重连不铸造,要下发观察到的 epoch。位点等于它:直接下发,铸造通道一条都不收。位点缺失(那次铸造没走完:没确认而回滚
// 又没做成、scene_manager 中途退出,或位点写丢了、过期了):先经铸造通道确认落盘、记下位点,再下发;下发的仍是这个值。
func TestEnterScene_SamePlacementConfirmsEpochMissingConfirmedMark(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6938)
		sceneID  = uint64(7951)
	)
	seedSceneOnNode(mr, testZoneId, sceneID, "10", "1")
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, sceneID, "10", testZoneId))
	adoptSceneMember(mr, sceneID, playerID)
	minter := testMinter(t, sc)
	enter := func() *scene_manager.EnterSceneResponse {
		resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
			PlayerId: playerID, SceneId: sceneID, ZoneId: testZoneId,
			GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
		})
		require.NoError(t, err)
		return resp
	}

	minter.resetScriptCalls()
	require.Equal(t, uint32(0), enter().ErrorCode)
	require.Len(t, *captured, 1)
	assert.Empty(t, minter.scriptCalls(), "位点等于要下发的值:不经铸造通道")

	mr.Del(ownerepoch.ConfirmedKey(playerID))
	require.Equal(t, uint32(0), enter().ErrorCode)
	require.Len(t, *captured, 2)
	assert.Equal(t, uint64(1), decodeRoutePlayerEvent(t, (*captured)[1]).OwnerEpoch, "确认之后下发的仍是这个值,不铸造")
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID))
	assert.Equal(t, []string{luaConfirmPendingRollback}, minter.scriptCalls(), "位点缺失:下发之前先确认落盘")
	got, _ := mr.Get(ownerepoch.ConfirmedKey(playerID))
	assert.Equal(t, "1", got, "确认之后记下位点")
}

// 同上,但确认不到份数:不下发、回 7,状态原样,位点仍然缺失;人数不动。
func TestEnterScene_SamePlacementRefusesEpochThatCannotBeConfirmed(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6939)
		sceneID  = uint64(7952)
	)
	seedSceneOnNode(mr, testZoneId, sceneID, "10", "1")
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, sceneID, "10", testZoneId))
	adoptSceneMember(mr, sceneID, playerID)
	seededRaw := identLocationRaw(t, sc, playerID)
	mr.Del(ownerepoch.ConfirmedKey(playerID))
	testMinter(t, sc).setDurability(svc.MintDurabilityNotDurable, errTestWaitAOFShort)

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
		PlayerId: playerID, SceneId: sceneID, ZoneId: testZoneId,
		GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrKafkaRoute, resp.ErrorCode)
	assert.Empty(t, *captured, "没确认落盘的 epoch 不得下发")
	assert.Equal(t, seededRaw, identLocationRaw(t, sc, playerID))
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID))
	assert.False(t, mr.Exists(ownerepoch.ConfirmedKey(playerID)), "没确认就不记位点")
	assert.Equal(t, "1", sceneCountText(sc, sceneID))
}

// 同物理节点换图(不铸造)同理:位点等于观察值时铸造通道一条都不收;位点缺失时先确认落盘,再落点、下发。
func TestEnterScene_SameNodeSceneChangeConfirmsEpochMissingConfirmedMark(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6940)
		sceneA   = uint64(7953)
		sceneB   = uint64(7954)
	)
	seedSceneOnNode(mr, testZoneId, sceneA, "10", "3")
	seedSceneOnNode(mr, testZoneId, sceneB, "10", "4")
	require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, sceneA, "10", testZoneId))
	adoptSceneMember(mr, sceneA, playerID)
	minter := testMinter(t, sc)
	enter := func(sceneID uint64) *scene_manager.EnterSceneResponse {
		resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
			PlayerId: playerID, SceneId: sceneID, ZoneId: testZoneId,
			GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
		})
		require.NoError(t, err)
		return resp
	}

	minter.resetScriptCalls()
	require.Equal(t, uint32(0), enter(sceneB).ErrorCode)
	require.Len(t, *captured, 1)
	assert.Empty(t, minter.scriptCalls(), "位点等于观察值:同节点换图不经铸造通道")

	mr.Del(ownerepoch.ConfirmedKey(playerID))
	require.Equal(t, uint32(0), enter(sceneA).ErrorCode)
	require.Len(t, *captured, 2)
	assert.Equal(t, uint64(1), decodeRoutePlayerEvent(t, (*captured)[1]).OwnerEpoch)
	assert.Equal(t, "1", ownerEpochRaw(t, sc, playerID), "不铸造")
	assert.Equal(t, []string{luaConfirmPendingRollback}, minter.scriptCalls(), "位点缺失:落点之前先确认落盘")
	loc, _ := GetPlayerLocation(context.Background(), sc, playerID)
	require.NotNil(t, loc)
	assert.Equal(t, sceneA, loc.SceneId)
}

// 最坏的一种叠加:铸造结果未知,收尾的回滚又没到 Redis(这里让回滚的 EVAL 直接出错),首发之后才生效 —— 留下一条指向从未载入
// 玩家的目标、epoch 没确认落盘的落点,而且与正常落点逐字节相同。玩家重试落回同一落点时不铸造;下发之前那道闸发现位点对不上,
// 先确认落盘再下发。
func TestEnterScene_LeftoverOfLateMintIsConfirmedBeforeRouting(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	captured := capturingKafkaWriter(sc)

	const (
		playerID = uint64(6941)
		targetID = uint64(7955)
	)
	seedSceneOnNode(mr, testZoneId, targetID, "10", "5")
	minter := testMinter(t, sc)
	minter.setWithholdEval(1)
	enter := func() *scene_manager.EnterSceneResponse {
		resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(&scene_manager.EnterSceneRequest{OwnerRevision: 1,
			PlayerId: playerID, SceneId: targetID, ZoneId: testZoneId,
			GateZoneId: testZoneId, GateId: "1", GateInstanceId: "gate-uuid-test",
		})
		require.NoError(t, err)
		return resp
	}

	// 钩子跑在 miniredis 的连接 goroutine 上,只读写原子标志;与 mr.SetError 共用钩子槽位,本用例不用 SetError。
	var rollbackFailed atomic.Bool
	mr.Server().SetPreHook(func(c *server.Peer, cmd string, args ...string) bool {
		if cmd != "EVAL" || len(args) == 0 || args[0] != luaRollbackPlayerPlacement || !rollbackFailed.CompareAndSwap(false, true) {
			return false
		}
		c.WriteError("ERR injected rollback failure")
		return true
	})
	t.Cleanup(func() { mr.Server().SetPreHook(nil) })

	require.Equal(t, constants.ErrKafkaRoute, enter().ErrorCode)
	mr.Server().SetPreHook(nil)
	require.True(t, rollbackFailed.Load(), "回滚的 EVAL 确实没执行")
	require.Empty(t, *captured)
	require.Equal(t, "1", minter.deliverWithheld(t, 0), "回滚没到 Redis,迟到的首发照样生效")
	require.Equal(t, "1", ownerEpochRaw(t, sc, playerID))
	require.False(t, mr.Exists(ownerepoch.ConfirmedKey(playerID)), "这次铸造没有人确认过")

	minter.resetScriptCalls()
	require.Equal(t, uint32(0), enter().ErrorCode)
	require.Len(t, *captured, 1)
	assert.Equal(t, uint64(1), decodeRoutePlayerEvent(t, (*captured)[0]).OwnerEpoch)
	assert.Equal(t, []string{luaConfirmPendingRollback}, minter.scriptCalls(), "不铸造就下发之前先确认落盘")
	got, _ := mr.Get(ownerepoch.ConfirmedKey(playerID))
	assert.Equal(t, "1", got)
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
	raw, _ := playerRedisGet(sc, getPlayerLocationKey(playerID))
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

	first, err := playerRedisEval(sc, luaMintEpochAndSetLocation, keys, mintLuaArgs("1", "placed-bytes", marker)...)
	require.NoError(t, err)
	require.Equal(t, "2", fmt.Sprint(first))

	mr.SetTTL(ownerepoch.OwnerEpochKey(playerID), time.Hour)
	replay, err := playerRedisEval(sc, luaMintEpochAndSetLocation, keys, mintLuaArgs("1", "placed-bytes", marker)...)
	require.NoError(t, err)
	assert.Equal(t, "-2", fmt.Sprint(replay), "重放识别命中")
	assert.Equal(t, "2", ownerEpochRaw(t, sc, playerID), "写回的是原值")
	assert.Zero(t, mr.TTL(ownerepoch.OwnerEpochKey(playerID)), "识别分支对 owner_epoch 发了一次写")

	mr.SetTTL(ownerepoch.OwnerEpochKey(playerID), time.Hour)
	other, err := playerRedisEval(sc, luaMintEpochAndSetLocation, keys, mintLuaArgs("1", "someone-else-bytes", marker)...)
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
	forwarded, _ := playerRedisGet(sc, ownerepoch.HandoffKey(playerID))
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
	require.NoError(t, setPlayerKeyForTest(sc, ownerepoch.OwnerEpochKey(playerID), "1"))
	require.NoError(t, setPlayerKeyForTest(sc, getPlayerLocationKey(playerID), "old-bytes"))
	require.NoError(t, setPlayerKeyForTest(sc, ownerepoch.HandoffKey(playerID), marker))
	target := strconv.FormatUint(testWallClockMs, 10)
	args := mintLuaArgsWithTarget(target, "1", "placed-bytes", marker)

	result, err := playerRedisEval(sc, luaMintEpochAndSetLocation, mintKeys, args...)
	require.NoError(t, err)
	require.Equal(t, target, fmt.Sprint(result))
	require.Equal(t, target, ownerEpochRaw(t, sc, playerID))

	result, err = playerRedisEval(sc, luaMintEpochAndSetLocation, mintKeys, args...)
	require.NoError(t, err)
	assert.Equal(t, "-"+target, fmt.Sprint(result), "原样重发:认出本请求自己的写")

	rolledBack := strconv.FormatUint(testWallClockMs+1, 10)
	result, err = playerRedisEval(sc, luaRollbackPlayerPlacement, rollbackKeys,
		"placed-bytes", "restored-bytes", target, "bump", marker, rolledBack+":1757000000000", "300")
	require.NoError(t, err)
	require.Equal(t, "-"+rolledBack, fmt.Sprint(result), "回滚仍是 INCR 一格")

	result, err = playerRedisEval(sc, luaMintEpochAndSetLocation, mintKeys, args...)
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
			require.NoError(t, setPlayerKeyForTest(sc, ownerepoch.OwnerEpochKey(playerID), fmt.Sprint(c.args[0])))
			_, err := playerRedisEval(sc, luaMintEpochAndSetLocation, mintKeys, c.args...)
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
	result, err := playerRedisEval(sc, luaMintEpochAndSetLocation, mintKeys, mintLuaArgsWithTarget(maxText, "0", "loc", "")...)
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

	first, err := playerRedisEval(sc, luaRollbackPlayerPlacement, rollbackPlacementKeys(playerID), args...)
	require.NoError(t, err)
	require.Equal(t, "-3", fmt.Sprint(first))

	mr.SetTTL(ownerepoch.OwnerEpochKey(playerID), time.Hour)
	mr.Del(ownerepoch.HandoffKey(playerID)) // 源端取证删掉了本族标记
	replay, err := playerRedisEval(sc, luaRollbackPlayerPlacement, rollbackPlacementKeys(playerID), args...)
	require.NoError(t, err)
	assert.Equal(t, "2", fmt.Sprint(replay), "重放识别命中")
	assert.Equal(t, "3", ownerEpochRaw(t, sc, playerID), "不再 INCR")
	assert.Greater(t, mr.TTL(ownerepoch.OwnerEpochKey(playerID)), time.Duration(0), "重放分支一个字节都不写")
	assert.False(t, mr.Exists(ownerepoch.HandoffKey(playerID)), "删掉的转写标记不能被重新写活")

	mr.Set(getPlayerLocationKey(playerID), "someone-else-bytes")
	mr.SetTTL(ownerepoch.OwnerEpochKey(playerID), time.Hour)
	other, err := playerRedisEval(sc, luaRollbackPlayerPlacement, rollbackPlacementKeys(playerID), args...)
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
	ok, err := playerRedisEval(sc, luaConfirmPendingRollback, keys, "3", "pending-bytes", "")
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
		got, err := playerRedisEval(sc, luaConfirmPendingRollback, keys, args...)
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
