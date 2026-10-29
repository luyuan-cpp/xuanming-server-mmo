package logic

// 回归:死节点收尾 reconcileDeadNodeScenes 先 GET scene:{id}:node,确认等于死节点后才强制销毁,而修复前的
// 销毁脚本(AtomicDestroyForce)不带期望属主。GET 之后若有写者经 reassignSceneNode 把实例改派到活节点并
// CreateScene,收尾仍会把它抹掉:DestroyScene 发到新节点,扣掉新节点的 scene_count 与 player_count,玩家刚进的
// 场景被拆。修复时(2026-10-22)这样的写者是 resolveScene 的死节点自愈;10-23 起它只重建世界频道,用例里用
// reassignSceneNode 模拟一个在收尾读属主之后改派实例的写者,守的是纵深防御。
//
// 本文件守收尾用的销毁入口 destroyOrphanOfDeadNode 与脚本层的契约,手法与 reassign_cas_test.go /
// resolve_scene_reassign_cas_test.go 相同:先摆出「收尾已读到属主是 dead」的现场,再用 reassignSceneNode 提交
// 改派,最后拿那次陈旧的观察调销毁入口。经 reconcileDeadNodeScenes 入口、在它自己的 GET 之后注入改派的用例
// 在 owner_read_interleave_test.go(那边能在修复前代码上直接取红)。
//
// 修复前红的取法(修复前没有 destroyOrphanOfDeadNode,本文件编不过,所以按变异取红):
//   - 变异 A:把 destroyOrphanOfDeadNode 末行传给 destroyInstanceInternal 的 deadNode 换成 ""。期望属主为空时
//     脚本跳过比较,整条链路与修复前 reconcile 调的 destroyInstanceForce(..., "node_death") 等价。
//     本文件 ReassignedAfterOwnerRead 红(失败信息含「收尾不得销毁」),其余四条绿;
//     owner_read_interleave_test.go 的 ReconcileSkipsSceneReassignedAfterOwnerRead 也红。
//   - 变异 B:恢复 A,删掉 luaAtomicDestroyInstanceForce 开头比较期望属主的 if ... end 三行。
//     本文件 ReassignedAfterOwnerRead 与 ExpectedOwnerMismatch 两条红,其余三条绿;其中 NoExpectedOwner 与
//     StillOwnedByDeadNode 守的是「不比对的 force 路径语义不变」与「属主仍是死节点时照常销毁」。
//   - 变异 C:恢复 B,把 reconcileDeadNodeScenes 里的 destroyOrphanOfDeadNode(ctx, svcCtx, zoneId, sceneId,
//     entry.nodeID) 换成修复前的 destroyInstanceForce(ctx, svcCtx, zoneId, sceneId, "node_death")(两者签名相同,
//     直接替换)。本文件五条全绿,只有 owner_read_interleave_test.go 的 ReconcileSkipsSceneReassignedAfterOwnerRead
//     红 —— 调用点由那条守。
//   - DestroyScriptErrorKeepsMemberIndexed 守「收尾出错的成员留在反向索引里」,取红方法见该用例注释。

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"

	"scene_manager/internal/svc"
)

const reconcileCASZone uint32 = 1

// 节点号不与别的用例重名,理由同 reassign_cas_test.go 的 casNode* 常量:再入屏障会读进程内的推迟摘除队列,
// 别的用例在 zone 1 留下的常用节点推迟态会把这里的改派静默挡下。
const (
	reconcileNodeDead = "8950"
	reconcileNodeLive = "8960"
)

// 新节点上已有的玩家数(含刚进这个实例的两个);修复前收尾会把实例人数从它身上扣掉。
const (
	reconcileLiveNodePlayers = "5"
	reconcileScenePlayers    = "2"
)

type reconcileDeadOwnerCase struct {
	sc      *svc.ServiceContext
	mr      *miniredis.Miniredis
	live    *sceneNodeHarness
	sceneID uint64
}

// newReconcileDeadOwnerCase 摆出现场:sceneID 是 dead 名下的实例(属主映射、反向索引、scene_count=1、
// active 集合、实例人数齐全),dead 已摘出负载集且没有 death_at(屏障放行);live 是活节点,带 bufconn 桩,
// 收到的 DestroyScene 记在桩上。
func newReconcileDeadOwnerCase(t *testing.T, sceneID uint64) *reconcileDeadOwnerCase {
	t.Helper()
	clearKnownNodesForTest()
	ResetNodeConnCacheForTest()
	t.Cleanup(clearKnownNodesForTest)

	sc, mr := newIntegrationSvcCtx(t)
	live := startFakeSceneNode(t, reconcileNodeLive)
	installBufconnDialer(t, live)
	registerWorldNodeFromHarness(t, sc, reconcileCASZone, live)

	member := strconv.FormatUint(sceneID, 10)
	require.NoError(t, mr.Set(sceneZoneKey(sceneID), strconv.FormatUint(uint64(reconcileCASZone), 10)))
	seedSceneOwnedBy(t, mr, reconcileCASZone, sceneID, reconcileNodeDead)
	_, err := mr.ZAdd(activeInstancesKey(reconcileCASZone), float64(nowUnix()), member)
	require.NoError(t, err)
	require.NoError(t, mr.Set(fmtInstancePlayerCountKey(sceneID), reconcileScenePlayers))

	// 前提:dead 已判死、屏障放行,live 存活。
	require.False(t, IsNodeAlive(sc, reconcileCASZone, reconcileNodeDead))
	require.True(t, IsNodeAlive(sc, reconcileCASZone, reconcileNodeLive))
	allowed, _ := CanReclaimDeadNode(sc, reconcileCASZone, reconcileNodeDead)
	require.True(t, allowed)

	return &reconcileDeadOwnerCase{sc: sc, mr: mr, live: live, sceneID: sceneID}
}

// 收尾读到属主是 dead 之后,有写者把实例改派到 live 并在 live 上 CreateScene;收尾拿着陈旧的观察销毁。
// 脚本比对属主落空:实例原样留给 live,live 的计数一个都不扣,不向 live 发 DestroyScene。
// 修复前:脚本不比对,抹掉全部场景键,DestroyScene 发到 live,live 的 scene_count 与 player_count 被扣。
func TestReconcileDeadNode_ReassignedAfterOwnerRead_LeavesNewOwnerIntact(t *testing.T) {
	c := newReconcileDeadOwnerCase(t, 89501)
	ctx := context.Background()
	require.NoError(t, c.mr.Set(nodePlayerCountKey(reconcileCASZone, reconcileNodeLive), reconcileLiveNodePlayers))
	// 改派成功的写者已把名额转到 live 的 GameServer(TransferAgonesRoomForScene),映射指向新名额。
	require.NoError(t, c.mr.Set(sceneAgonesGsKey(c.sceneID), "gs-live"))

	// 收尾的那次 GET:属主仍是 dead,收尾据此决定销毁。
	owner, err := c.mr.Get(sceneNodeKey(c.sceneID))
	require.NoError(t, err)
	require.Equal(t, reconcileNodeDead, owner)

	// 窗口里另一写者提交改派:属主、两侧反向索引与 scene_count 同批改到 live。
	require.True(t, reassignSceneNode(c.sc, reconcileCASZone, c.sceneID, reconcileNodeDead, reconcileNodeLive))
	require.EqualValues(t, 1, readInt64(c.sc, nodeSceneCountKey(reconcileCASZone, reconcileNodeLive)))

	ok, err := destroyOrphanOfDeadNode(ctx, c.sc, reconcileCASZone, c.sceneID, reconcileNodeDead)
	require.NoError(t, err)
	require.False(t, ok, "属主已改派到 live,收尾不得销毁这个实例")

	owner, err = c.mr.Get(sceneNodeKey(c.sceneID))
	require.NoError(t, err)
	require.Equal(t, reconcileNodeLive, owner, "属主映射必须原样留给 live")
	requireSceneIndexedOnlyOn(t, c.sc, reconcileCASZone, c.sceneID, reconcileNodeLive, reconcileNodeDead)
	require.EqualValues(t, 1, readInt64(c.sc, nodeSceneCountKey(reconcileCASZone, reconcileNodeLive)), "新节点的 scene_count 不得被扣")
	require.Equal(t, reconcileLiveNodePlayers, mustGet(t, c.mr, nodePlayerCountKey(reconcileCASZone, reconcileNodeLive)),
		"新节点的 player_count 不得被扣掉实例人数")
	require.EqualValues(t, 0, c.live.fake.destroyCalls.Load(), "不得向新节点发 DestroyScene")

	// 其余场景键一个都没动。
	require.Equal(t, reconcileScenePlayers, mustGet(t, c.mr, fmtInstancePlayerCountKey(c.sceneID)))
	require.Equal(t, "gs-live", mustGet(t, c.mr, sceneAgonesGsKey(c.sceneID)))
	require.Equal(t, strconv.FormatUint(uint64(reconcileCASZone), 10), mustGet(t, c.mr, sceneZoneKey(c.sceneID)))
	requireActiveInstance(t, c.sc, reconcileCASZone, c.sceneID, true)
}

// 没有插队的改派时,收尾照常销毁:场景键全部抹掉,记账只扣在死节点上,死节点不发 RPC,活节点一点不碰。
func TestReconcileDeadNode_StillOwnedByDeadNode_Destroys(t *testing.T) {
	c := newReconcileDeadOwnerCase(t, 89502)
	ctx := context.Background()
	require.NoError(t, c.mr.Set(nodePlayerCountKey(reconcileCASZone, reconcileNodeDead), reconcileScenePlayers))
	require.NoError(t, c.mr.Set(nodePlayerCountKey(reconcileCASZone, reconcileNodeLive), reconcileLiveNodePlayers))

	ok, err := destroyOrphanOfDeadNode(ctx, c.sc, reconcileCASZone, c.sceneID, reconcileNodeDead)
	require.NoError(t, err)
	require.True(t, ok, "属主仍是死节点,必须销毁")

	member := strconv.FormatUint(c.sceneID, 10)
	require.False(t, c.mr.Exists(sceneNodeKey(c.sceneID)))
	require.False(t, c.mr.Exists(fmtInstancePlayerCountKey(c.sceneID)))
	require.False(t, c.mr.Exists(sceneZoneKey(c.sceneID)))
	requireActiveInstance(t, c.sc, reconcileCASZone, c.sceneID, false)
	in, err := c.sc.Redis.Sismember(nodeScenesKey(reconcileCASZone, reconcileNodeDead), member)
	require.NoError(t, err)
	require.False(t, in)
	require.EqualValues(t, 0, readInt64(c.sc, nodeSceneCountKey(reconcileCASZone, reconcileNodeDead)))
	require.EqualValues(t, 0, readInt64(c.sc, nodePlayerCountKey(reconcileCASZone, reconcileNodeDead)))

	require.False(t, c.mr.Exists(nodeSceneCountKey(reconcileCASZone, reconcileNodeLive)), "活节点不得被记账")
	require.Equal(t, reconcileLiveNodePlayers, mustGet(t, c.mr, nodePlayerCountKey(reconcileCASZone, reconcileNodeLive)))
	require.EqualValues(t, 0, c.live.fake.destroyCalls.Load())
}

// 脚本层:带期望属主而属主不符时,六个场景键(属主、人数、镜像标志、源、zone、agones_gs)与 active 集合成员
// 一个都不动,返回全空;映射已不存在时同样不动(删映射的那条路径负责其余清理,见 destroyOrphanOfDeadNode 注释)。
// 两个场景放在同一个 active 集合里,成员资格逐个按成员查。
func TestAtomicDestroyForce_ExpectedOwnerMismatchTouchesNothing(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const reassigned, unmapped uint64 = 89503, 89504
	for _, id := range []uint64{reassigned, unmapped} {
		require.NoError(t, mr.Set(sceneZoneKey(id), "1"))
		require.NoError(t, mr.Set(fmtInstancePlayerCountKey(id), "3"))
		require.NoError(t, mr.Set(sceneMirrorFlagKey(id), "1"))
		require.NoError(t, mr.Set(sceneSourceKey(id), "77"))
		require.NoError(t, mr.Set(sceneAgonesGsKey(id), "gs-x"))
		_, err := mr.ZAdd(activeInstancesKey(reconcileCASZone), 1, strconv.FormatUint(id, 10))
		require.NoError(t, err)
	}
	require.NoError(t, mr.Set(sceneNodeKey(reassigned), reconcileNodeLive))

	for _, id := range []uint64{reassigned, unmapped} {
		node, gs, pc, err := AtomicDestroyForce(sc, reconcileCASZone, id, reconcileNodeDead)
		require.NoError(t, err)
		require.Empty(t, node)
		require.Empty(t, gs)
		require.Zero(t, pc)
	}
	for _, id := range []uint64{reassigned, unmapped} {
		require.Equal(t, "1", mustGet(t, mr, sceneZoneKey(id)))
		require.Equal(t, "3", mustGet(t, mr, fmtInstancePlayerCountKey(id)))
		require.Equal(t, "1", mustGet(t, mr, sceneMirrorFlagKey(id)))
		require.Equal(t, "77", mustGet(t, mr, sceneSourceKey(id)))
		require.Equal(t, "gs-x", mustGet(t, mr, sceneAgonesGsKey(id)))
		requireActiveInstance(t, sc, reconcileCASZone, id, true)
	}
	require.Equal(t, reconcileNodeLive, mustGet(t, mr, sceneNodeKey(reassigned)))
	require.False(t, mr.Exists(sceneNodeKey(unmapped)), "不得替不存在的映射建键")
}

// 脚本层:期望属主为空串时不比对,与修复前同一语义 —— explicit / cascade / source_migrated / source_scaled_in
// (都经 destroyInstanceForce)依赖它,属主是谁都照抹,返回抹除前的 (属主, 名额, 人数)。
func TestAtomicDestroyForce_NoExpectedOwnerWipesWhoeverOwnsIt(t *testing.T) {
	sc, mr := newIntegrationSvcCtx(t)
	const sceneID, bystander uint64 = 89505, 89506
	require.NoError(t, mr.Set(sceneNodeKey(sceneID), reconcileNodeLive))
	require.NoError(t, mr.Set(fmtInstancePlayerCountKey(sceneID), "4"))
	require.NoError(t, mr.Set(sceneAgonesGsKey(sceneID), "gs-live"))
	for _, id := range []uint64{sceneID, bystander} {
		_, err := mr.ZAdd(activeInstancesKey(reconcileCASZone), 1, strconv.FormatUint(id, 10))
		require.NoError(t, err)
	}

	node, gs, pc, err := AtomicDestroyForce(sc, reconcileCASZone, sceneID, "")
	require.NoError(t, err)
	require.Equal(t, reconcileNodeLive, node)
	require.Equal(t, "gs-live", gs)
	require.EqualValues(t, 4, pc)

	require.False(t, mr.Exists(sceneNodeKey(sceneID)))
	require.False(t, mr.Exists(fmtInstancePlayerCountKey(sceneID)))
	require.False(t, mr.Exists(sceneAgonesGsKey(sceneID)))
	requireActiveInstance(t, sc, reconcileCASZone, sceneID, false)
	requireActiveInstance(t, sc, reconcileCASZone, bystander, true)
}

// errInjectedEval 是 failFirstEvalHook 注入的错误。
var errInjectedEval = errors.New("injected: EVAL not sent")

// failFirstEvalHook 让 script 的第一次 EVAL 不发给 Redis、直接回错误(脚本没执行),之后一律放行。
type failFirstEvalHook struct {
	script string
	fired  atomic.Bool
}

func (h *failFirstEvalHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *failFirstEvalHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *failFirstEvalHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		args := cmd.Args()
		if cmd.Name() == "eval" && len(args) >= 2 && args[1] == h.script && h.fired.CompareAndSwap(false, true) {
			cmd.SetErr(errInjectedEval)
			return errInjectedEval
		}
		return next(ctx, cmd)
	}
}

// 销毁脚本报错(结论未知)时,收尾把它计入 failed,并且不从死节点的反向索引里摘掉:属主映射仍指向死节点,
// 摘掉会让索引与映射不一致,同一 node_id 下次判死时也抄不进快照。
// 修复前(940ef9746 报错被吞、照样计 destroyed;7a23b4fed 已计 failed)快照成员一律 SREM,反向索引被摘掉。
// 红的取法:把 reconcileDeadNodeScenes 末尾的 membersExcept(members, failedMembers) 换成 membersExcept(members, nil)。
func TestReconcileDeadNode_DestroyScriptErrorKeepsMemberIndexed(t *testing.T) {
	clearKnownNodesForTest()
	ResetNodeConnCacheForTest()
	t.Cleanup(clearKnownNodesForTest)

	hook := &failFirstEvalHook{script: luaAtomicDestroyInstanceForce}
	sc, mr := newIntegrationSvcCtxWithRedisOptions(t, redis.WithHook(hook))
	const sceneID uint64 = 89507
	member := strconv.FormatUint(sceneID, 10)
	require.NoError(t, mr.Set(sceneZoneKey(sceneID), strconv.FormatUint(uint64(reconcileCASZone), 10)))
	seedSceneOwnedBy(t, mr, reconcileCASZone, sceneID, reconcileNodeDead)
	_, err := mr.ZAdd(activeInstancesKey(reconcileCASZone), float64(nowUnix()), member)
	require.NoError(t, err)

	entry := nodeEntry{nodeID: reconcileNodeDead}
	entry.reg.ZoneId = reconcileCASZone
	reconcileDeadNodeScenes(context.Background(), sc, entry, []string{member})

	require.True(t, hook.fired.Load(), "注入的脚本错误必须真的发生")
	require.Equal(t, reconcileNodeDead, mustGet(t, mr, sceneNodeKey(sceneID)), "脚本没执行,属主映射原样")
	requireSceneIndexedOnlyOn(t, sc, reconcileCASZone, sceneID, reconcileNodeDead)
	require.EqualValues(t, 1, readInt64(sc, nodeSceneCountKey(reconcileCASZone, reconcileNodeDead)))
	requireActiveInstance(t, sc, reconcileCASZone, sceneID, true)
}

// mustGet 读一个必须存在的字符串键。
func mustGet(t *testing.T, mr *miniredis.Miniredis, key string) string {
	t.Helper()
	v, err := mr.Get(key)
	require.NoError(t, err, "键 %s 必须存在", key)
	return v
}
