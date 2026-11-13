package logic

// 回归(2026-11-05):改派 CAS 与 Agones 名额转移的「已执行、回包丢失、重发结论翻转」(R5 的同类,
// 「场景销毁认领」会话 11-05 核对时报来)。
//
// go-redis 对读超时 / EOF 会原样重发 EVAL(go-zero 的客户端 MaxRetries=3)。第一次已经执行、回包丢了,重发看到的是
// 执行之后的状态:
//   - luaReassignSceneNode:属主已是 newNode、不等于 oldNode,回 0。调用方按「抢输」走:转名额、rebalance 拆老实体
//     与级联镜像都被跳过。
//   - luaCommitAgonesRoomTransfer:记录已在新 GameServer 上,回 2(2026-11-10 起在途占用已消费,回 3)。调用方把刚记账的
//     新名额还掉,旧名额永不归还。
// 修法与销毁认领、新场景登记同一套:每次调用一个 token、一把自己的短 TTL 回执键,成功时写回执,同一 token 再来原样回
// 当时的结论;Eval 最终回错时用同一个 token 跑 luaResolveCasReceipt 问清结局,没落地的同时写下作废标记,之后才到的
// EVAL 一个键不动。
//
// 三种注入:
//   - resendEvalHook:第一次 EVAL 连续执行两遍、只交第二遍的应答,模拟「回包丢失后原样重发」(同一组参数、同一个 token,
//     与 go-redis 内部重发一致);
//   - lostReplyEvalHook(scene_claim_atomic_test.go):脚本真的执行、应答换成错误,模拟重发也都没拿到应答;
//   - heldEvalHook:第一次 EVAL 根本不发、直接回错并记下参数,用例随后原样补发,模拟「请求卡在路上、调用方先超时、
//     请求晚到」。
//
// 用例只经 reassignSceneNode、reassignWorldChannelNode 与改派的名额两步(moveAgonesRoomForTest:先占名额,CAS 之后
// commitReservedAgonesRoom 提交,2026-11-11 起取代 TransferAgonesRoomForScene)进;回执键的形状是本次新加的,
// 修复前红按 PROGRESS 2026-11-05 两条「回执」条目里的变异取。

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"scene_manager/internal/constants"
	"scene_manager/internal/svc"
)

// 节点号不与别的用例重名,理由同 reassign_cas_test.go 的 casNode* 常量。
const (
	replayNodeOld   = "8740"
	replayNodeNew   = "8750"
	replayNodeOther = "8760"
)

// resendEvalHook:见文件头。fired 先置位,第二遍执行与之后的命令不会再触发。
type resendEvalHook struct {
	script string
	fired  atomic.Bool
}

func (h *resendEvalHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *resendEvalHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *resendEvalHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if isEvalOf(cmd, h.script) && h.fired.CompareAndSwap(false, true) {
			if err := next(ctx, cmd); err != nil {
				return err
			}
			// 第一遍的应答丢了,原样重发。
			return next(ctx, cmd)
		}
		return next(ctx, cmd)
	}
}

// heldEvalHook:见文件头。只拦第一次,args 是那次 EVAL 的完整参数(含命令名、脚本、键数)。
type heldEvalHook struct {
	script string
	fired  atomic.Bool
	args   []any
}

func (h *heldEvalHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *heldEvalHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *heldEvalHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if isEvalOf(cmd, h.script) && h.fired.CompareAndSwap(false, true) {
			h.args = append([]any(nil), cmd.Args()...)
			cmd.SetErr(errInjectedEval)
			return errInjectedEval
		}
		return next(ctx, cmd)
	}
}

// deliverLate 把被拦下的那次 EVAL 原样补发(同一组键、同一个 token),返回它的应答。
// go-redis 的 Eval 参数是 {"eval", 脚本, 键数, 键..., 参数...}。
func (h *heldEvalHook) deliverLate(t *testing.T, sc *svc.ServiceContext) any {
	t.Helper()
	require.True(t, h.fired.Load(), "前提:EVAL 被拦下过")
	numKeys, ok := h.args[2].(int)
	require.True(t, ok, "EVAL 的键数参数应当是 int,实际 %T", h.args[2])
	keys := make([]string, numKeys)
	for i := range keys {
		keys[i] = h.args[3+i].(string)
	}
	raw, err := sc.Redis.Eval(h.script, keys, h.args[3+numKeys:]...)
	require.NoError(t, err)
	return raw
}

// receiptValuesForTest 返回某个场景、某种回执(keyPrefix 形如 "scene:{ctl:<z>}:<id>:reassign_receipt:",即 token 为空串时
// 构造函数拼出的键)现存的全部回执值。
func receiptValuesForTest(t *testing.T, mr *miniredis.Miniredis, keyPrefix string) []string {
	t.Helper()
	var values []string
	for _, key := range mr.Keys() {
		if !strings.HasPrefix(key, keyPrefix) {
			continue
		}
		v, err := mr.Get(key)
		require.NoError(t, err)
		values = append(values, v)
	}
	return values
}

// 改派已落地、回包丢失后重发:仍回「已改派」,计数只挪一次。另一次调用(新 token)照常按属主判定。
// 两个子例分别走不带 / 带在役核对的两种 KEYS 布局。
// 修复前:重发看到属主已是 new,回 0,调用方以为抢输。
func TestReassignSceneNode_ResendAfterLostReplyKeepsAppliedVerdict(t *testing.T) {
	cases := []struct {
		name      string
		sceneID   uint64
		inService bool // true = 走 reassignWorldChannelNode(带在役核对)
	}{
		{name: "scene", sceneID: 87201},
		{name: "world_channel", sceneID: 87202, inService: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hook := &resendEvalHook{script: luaReassignSceneNode}
			sc, mr := newIntegrationSvcCtxWithRedisOptions(t, hook)
			channelSetKey := worldChannelsKey(reassignCASZone, 621)
			reassign := func(sc *svc.ServiceContext, oldNode, newNode string) bool {
				if tc.inService {
					return reassignWorldChannelNode(sc, reassignCASZone, tc.sceneID, oldNode, newNode, channelSetKey, freshPlacementGeneration(t, sc, reassignCASZone))
				}
				return reassignSceneNode(sc, reassignCASZone, tc.sceneID, oldNode, newNode, freshPlacementGeneration(t, sc, reassignCASZone))
			}
			seedSceneOwnedBy(t, mr, reassignCASZone, tc.sceneID, replayNodeOld)
			if tc.inService {
				_, err := mr.SAdd(channelSetKey, strconv.FormatUint(tc.sceneID, 10))
				require.NoError(t, err)
			}

			require.True(t, reassign(sc, replayNodeOld, replayNodeNew), "重发不得把已落地的改派翻成落空")
			require.True(t, hook.fired.Load(), "前提:改派的 EVAL 真的被重发了")

			owner, err := mr.Get(sceneNodeKey(reassignCASZone, tc.sceneID))
			require.NoError(t, err)
			require.Equal(t, replayNodeNew, owner)
			requireSceneIndexedOnlyOn(t, sc, reassignCASZone, tc.sceneID, replayNodeNew, replayNodeOld)
			require.EqualValues(t, 1, readInt64(sc, nodeSceneCountKey(reassignCASZone, replayNodeNew)), "新节点的 scene_count 只加一次")
			require.EqualValues(t, 0, readInt64(sc, nodeSceneCountKey(reassignCASZone, replayNodeOld)), "老节点的 scene_count 只减一次")

			// 回执只认同一个 token:另一次拿着陈旧判断的改派照常落空,一个键不动。
			require.False(t, reassign(sc, replayNodeOld, replayNodeOther))
			owner, err = mr.Get(sceneNodeKey(reassignCASZone, tc.sceneID))
			require.NoError(t, err)
			require.Equal(t, replayNodeNew, owner)
			require.False(t, mr.Exists(nodeSceneCountKey(reassignCASZone, replayNodeOther)))
		})
	}
}

// 名额改写已落地、回包丢失后重发:仍按「已改写」处理,还旧名额一次,新名额留着。
// 修复前:重发看到记录已是 gs-new,回 2,调用方把 gs-new 还掉,gs-old 永不归还。
func TestWorldChannel_TransferResendAfterLostReplyReleasesOldRoomOnce(t *testing.T) {
	hook := &resendEvalHook{script: luaCommitAgonesRoomTransfer}
	sc, _ := newIntegrationSvcCtxWithRedisOptions(t, hook)
	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-old", podIP: "10.0.0.8", state: "Allocated", capacity: 4},
		&fakeGameServer{name: "gs-new", podIP: "10.0.0.9", state: "Ready", capacity: 4},
	)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, "10", "10.0.0.8", testZoneId, constants.SceneNodeTypeMainWorld)
	bindPodIP(t, "20", "10.0.0.9", testZoneId, constants.SceneNodeTypeMainWorld)
	const sceneID uint64 = 87203
	require.NoError(t, sc.Redis.Set(sceneNodeKey(testZoneId, sceneID), "20"))
	seedAgonesRoom(t, sc, alloc, sceneID, "gs-old")

	require.True(t, moveAgonesRoomForTest(t, sc, sceneID, "20"))

	require.True(t, hook.fired.Load(), "前提:转移的 EVAL 真的被重发了")
	assert.EqualValues(t, 1, alloc.roomsOf("gs-new"), "刚记账的新名额不得被还掉")
	assert.EqualValues(t, 0, alloc.roomsOf("gs-old"), "旧名额必须还一次")
	requireAgonesRecordOn(t, sc, alloc, sceneID, "gs-new")
}

// Eval 最终回错(脚本执行了、应答换成错误),调用方用同一个 token 问清结局:
//   - applied:回执已写,按已改派返回,计数只挪一次。修复前按结论未知回 false,rebalance 会跳过拆老实体。
//   - not_applied:属主早已不是 old,脚本落空、不写回执;问的时候写下作废标记,回 false,一个键不动。
func TestReassignSceneNode_ReplyLostConfirmedByReceipt(t *testing.T) {
	t.Run("applied", func(t *testing.T) {
		hook := &lostReplyEvalHook{script: luaReassignSceneNode}
		sc, mr := newIntegrationSvcCtxWithRedisOptions(t, hook)
		const sceneID uint64 = 87204
		seedSceneOwnedBy(t, mr, reassignCASZone, sceneID, replayNodeOld)

		require.True(t, reassignSceneNode(sc, reassignCASZone, sceneID, replayNodeOld, replayNodeNew, freshPlacementGeneration(t, sc, reassignCASZone)),
			"回包丢了但回执确认已落地,必须按已改派返回")
		require.True(t, hook.fired.Load(), "前提:改派的 EVAL 执行了、应答被换成错误")

		owner, err := mr.Get(sceneNodeKey(reassignCASZone, sceneID))
		require.NoError(t, err)
		require.Equal(t, replayNodeNew, owner)
		requireSceneIndexedOnlyOn(t, sc, reassignCASZone, sceneID, replayNodeNew, replayNodeOld)
		require.EqualValues(t, 1, readInt64(sc, nodeSceneCountKey(reassignCASZone, replayNodeNew)))
		require.EqualValues(t, 0, readInt64(sc, nodeSceneCountKey(reassignCASZone, replayNodeOld)))
	})

	t.Run("not_applied", func(t *testing.T) {
		hook := &lostReplyEvalHook{script: luaReassignSceneNode}
		sc, mr := newIntegrationSvcCtxWithRedisOptions(t, hook)
		const sceneID uint64 = 87205
		seedSceneOwnedBy(t, mr, reassignCASZone, sceneID, replayNodeOther)

		require.False(t, reassignSceneNode(sc, reassignCASZone, sceneID, replayNodeOld, replayNodeNew, freshPlacementGeneration(t, sc, reassignCASZone)),
			"脚本落空时没有本次的回执,不能凭空当成已落地")
		require.True(t, hook.fired.Load())
		owner, err := mr.Get(sceneNodeKey(reassignCASZone, sceneID))
		require.NoError(t, err)
		require.Equal(t, replayNodeOther, owner)
		require.Equal(t, []string{casReceiptAborted},
			receiptValuesForTest(t, mr, sceneReassignReceiptKey(reassignCASZone, sceneID, "")),
			"落空不写 applied;问结局时留下的只有作废标记")
		require.False(t, mr.Exists(nodeSceneCountKey(reassignCASZone, replayNodeNew)))
	})
}

// 改派的 EVAL 卡在路上、调用方先回错:问结局时写下作废标记,按落空返回;那次 EVAL 晚到时一个键都不动。
// 只读回执判断不了这一种:读的时候还没有回执,读完之后那次 EVAL 才执行、改了归属,调用方却已经按落空回收了
// 新节点上预建的实体(rebalance 也没拆老实体),归属指向一个没有实体的节点。
func TestReassignSceneNode_LateEvalAfterResolveChangesNothing(t *testing.T) {
	hook := &heldEvalHook{script: luaReassignSceneNode}
	sc, mr := newIntegrationSvcCtxWithRedisOptions(t, hook)
	const sceneID uint64 = 87207
	seedSceneOwnedBy(t, mr, reassignCASZone, sceneID, replayNodeOld)

	require.False(t, reassignSceneNode(sc, reassignCASZone, sceneID, replayNodeOld, replayNodeNew, freshPlacementGeneration(t, sc, reassignCASZone)),
		"EVAL 没执行:问结局时作废,按落空返回")
	require.EqualValues(t, reassignOwnerChanged, toInt64(hook.deliverLate(t, sc)), "作废之后才到的 EVAL 必须落空")

	owner, err := mr.Get(sceneNodeKey(reassignCASZone, sceneID))
	require.NoError(t, err)
	require.Equal(t, replayNodeOld, owner, "晚到的 EVAL 不得改归属")
	requireSceneIndexedOnlyOn(t, sc, reassignCASZone, sceneID, replayNodeOld, replayNodeNew)
	require.EqualValues(t, 1, readInt64(sc, nodeSceneCountKey(reassignCASZone, replayNodeOld)))
	require.False(t, mr.Exists(nodeSceneCountKey(reassignCASZone, replayNodeNew)), "晚到的 EVAL 不得记账")
}

// 名额改写执行了、Eval 最终回错:问结局得到「已改写」,还旧名额一次,新名额留着。
// 修复前:按结论未知两边都不还,gs-old 多占一份,直到对账告警。
func TestWorldChannel_TransferReplyLostConfirmedByReceiptReleasesOldRoom(t *testing.T) {
	hook := &lostReplyEvalHook{script: luaCommitAgonesRoomTransfer}
	sc, _ := newIntegrationSvcCtxWithRedisOptions(t, hook)
	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-old", podIP: "10.0.0.8", state: "Allocated", capacity: 4},
		&fakeGameServer{name: "gs-new", podIP: "10.0.0.9", state: "Ready", capacity: 4},
	)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, "10", "10.0.0.8", testZoneId, constants.SceneNodeTypeMainWorld)
	bindPodIP(t, "20", "10.0.0.9", testZoneId, constants.SceneNodeTypeMainWorld)
	const sceneID uint64 = 87206
	require.NoError(t, sc.Redis.Set(sceneNodeKey(testZoneId, sceneID), "20"))
	seedAgonesRoom(t, sc, alloc, sceneID, "gs-old")

	require.True(t, moveAgonesRoomForTest(t, sc, sceneID, "20"))

	require.True(t, hook.fired.Load(), "前提:转移的 EVAL 执行了、应答被换成错误")
	assert.EqualValues(t, 1, alloc.roomsOf("gs-new"), "新名额已记账,不得还掉")
	assert.EqualValues(t, 0, alloc.roomsOf("gs-old"), "回执确认已改写,旧名额必须还一次")
	requireAgonesRecordOn(t, sc, alloc, sceneID, "gs-new")
}

// 转移的 EVAL 卡在路上、调用方先回错:问结局时作废,确定没改写,还掉刚占的新名额、旧记录不动;那次 EVAL 晚到时
// 也不改记录。修复前:按结论未知两边都不还,新名额泄漏;晚到的 EVAL 还会把记录改到一份已经还掉的名额上。
func TestWorldChannel_TransferLateEvalAfterResolveChangesNothing(t *testing.T) {
	hook := &heldEvalHook{script: luaCommitAgonesRoomTransfer}
	sc, _ := newIntegrationSvcCtxWithRedisOptions(t, hook)
	alloc := newFakeAllocator(
		&fakeGameServer{name: "gs-old", podIP: "10.0.0.8", state: "Allocated", capacity: 4},
		&fakeGameServer{name: "gs-new", podIP: "10.0.0.9", state: "Ready", capacity: 4},
	)
	withAgones(t, sc, alloc, true)
	bindPodIP(t, "10", "10.0.0.8", testZoneId, constants.SceneNodeTypeMainWorld)
	bindPodIP(t, "20", "10.0.0.9", testZoneId, constants.SceneNodeTypeMainWorld)
	const sceneID uint64 = 87208
	require.NoError(t, sc.Redis.Set(sceneNodeKey(testZoneId, sceneID), "20"))
	record := seedAgonesRoom(t, sc, alloc, sceneID, "gs-old")

	require.True(t, moveAgonesRoomForTest(t, sc, sceneID, "20"))

	assert.EqualValues(t, 0, alloc.roomsOf("gs-new"), "确定没改写:刚占的新名额没人记账,必须还掉")
	assert.EqualValues(t, 1, alloc.roomsOf("gs-old"), "旧记录没动,旧名额不得还")

	parts := toSlice(hook.deliverLate(t, sc))
	require.Len(t, parts, 2)
	require.EqualValues(t, agonesTransferStale, toInt64(parts[0]), "作废之后才到的 EVAL 必须落空")
	gs, err := sc.Redis.Get(sceneAgonesGsKey(testZoneId, sceneID))
	require.NoError(t, err)
	assert.Equal(t, record, gs, "晚到的 EVAL 不得把记录改到已经还掉的名额上")
	assert.Empty(t, agonesHoldsOf(t, sc, testZoneId), "放弃时在途占用一起删")
}
