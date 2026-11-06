package logic

// 回归:「先读属主、再销毁」的两条路径,在读与销毁之间被改派时的行为。
//
//   - 死节点收尾 reconcileDeadNodeScenes:读到属主是死节点后,有写者用 reassignSceneNode 把实例改派到活节点
//     (修复时 2026-10-22 是 resolveScene 的死节点自愈;10-23 起它只重建世界频道,这里模拟的是纵深防御要挡的
//     写者)。修复前收尾用不比对属主的 force 脚本照样销毁:DestroyScene 发到新节点,新节点的 scene_count /
//     player_count 被扣,玩家刚进的场景被拆。现在销毁脚本比对期望属主,落空就一个键都不动。
//   - 显式 DestroyScene:另一个写者(rebalance 的机会迁移等)在销毁之前把场景从 A 改派到 B。修复前(10-23 之前)
//     它先快照读到 A,按快照把 RPC 发到 A、计数扣在 A 上,B 的 scene_count 与反向索引永久多一份,B 上的实体成了
//     孤儿。现在经 destroyInstanceForce 原子认领,副作用一律以认领应答里的属主 B 为准。
//
// 交错用 go-redis hook 注入(redis.WithHook,见 newIntegrationSvcCtxWithRedisOptions),在下面两个时刻中先到的
// 那一个同步提交一次 reassignSceneNode,只注入一次:
//   - 调用方第一次 GET scene:{id}:node 拿到结果之后:收尾的快速过滤就是这一读,注入点即「读完属主、销毁之前」;
//   - 调用方发出以 scene:{id}:node 为 KEYS[1] 的 EVAL 之前:2026-11-05 起 destroyInstanceInternal 在认领前不再读属主
//     (一律取认领应答),显式 DestroyScene 的第一次触碰就是认领脚本,注入点是「认领之前」。
// 两条用例都从生产入口进(reconcileDeadNodeScenes、DestroySceneLogic.DestroyScene)。
//
// 修复前红的取法(本文件依赖 10-31 起的成员集合辅助函数,拷不回修复前的提交,按变异取红):
//   - ReconcileSkipsSceneReassignedAfterOwnerRead:按 reconcile_owner_cas_test.go 文件头的变异 C,把收尾里的
//     destroyOrphanOfDeadNode 换回修复前的 destroyInstanceForce(..., "node_death")。失败信息含「收尾不得向新属主发
//     DestroyScene」。
//   - ExplicitDestroyFollowsOwnerAtClaimTime:变异 E,模拟修复前「副作用用自己快照读到的属主」。在 destroyInstanceInternal
//     认领之前加 `snapshotOwner, _ := svcCtx.Redis.Get(sceneNodeKey(sceneId))`,并把 `nodeId := claim.NodeID` 换成
//     `nodeId := snapshotOwner`。失败信息含「DestroyScene 必须发给认领时的属主 B」。

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"

	"proto/scene_manager"
	"scene_manager/internal/svc"
)

const interleaveZone uint32 = 1

// 节点号不与别的用例重名,理由同 reassign_cas_test.go 的 casNode* 常量:再入屏障会读进程内的推迟摘除队列,
// 别的用例在 zone 1 留下的常用节点推迟态会把这里注入的改派静默挡下。
const (
	interleaveNodeDead = "8970"
	interleaveNodeLive = "8975"
	interleaveNodeA    = "8980"
	interleaveNodeB    = "8985"
)

// afterOwnerReadHook 在调用方第一次触碰属主键时同步执行一次 inject,两种触碰取先到的:
//   - 「GET key」返回之后、把结果交还调用方之前;
//   - 以 key 为 KEYS[1] 的 EVAL 发出之前(认领脚本在脚本里读属主,调用方事先不读)。
// inject 里发出的命令(reassignSceneNode 自己的 EVAL 也以 key 为 KEYS[1])同样经过本 hook;fired 先置位,所以不会
// 重入。arm 之前一律放行,用例的现场搭建不受影响。
type afterOwnerReadHook struct {
	key    string
	inject func()
	armed  atomic.Bool
	fired  atomic.Bool
}

func (h *afterOwnerReadHook) arm(inject func()) {
	h.inject = inject
	h.armed.Store(true)
}

func (h *afterOwnerReadHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *afterOwnerReadHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *afterOwnerReadHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if h.armed.Load() && isEvalOnFirstKey(cmd, h.key) && h.fired.CompareAndSwap(false, true) {
			h.inject()
		}
		err := next(ctx, cmd)
		if h.armed.Load() && isGetOf(cmd, h.key) && h.fired.CompareAndSwap(false, true) {
			h.inject()
		}
		return err
	}
}

// isGetOf 报告 cmd 是不是对 key 的 GET。
func isGetOf(cmd redis.Cmder, key string) bool {
	args := cmd.Args()
	if cmd.Name() != "get" || len(args) != 2 {
		return false
	}
	k, ok := args[1].(string)
	return ok && k == key
}

// isEvalOnFirstKey 报告 cmd 是不是 KEYS[1] 为 key 的 EVAL。go-redis 的参数形状是
// ["eval", script, numkeys, KEYS[1], ...]。
func isEvalOnFirstKey(cmd redis.Cmder, key string) bool {
	args := cmd.Args()
	if cmd.Name() != "eval" || len(args) < 4 {
		return false
	}
	k, ok := args[3].(string)
	return ok && k == key
}

// newInterleaveSvcCtx 起一个带 hook 的 svcCtx,并把 harnesses 注册成活节点。
func newInterleaveSvcCtx(t *testing.T, hook *afterOwnerReadHook, harnesses ...*sceneNodeHarness) *svc.ServiceContext {
	t.Helper()
	clearKnownNodesForTest()
	ResetNodeConnCacheForTest()
	t.Cleanup(clearKnownNodesForTest)

	sc, _ := newIntegrationSvcCtxWithRedisOptions(t, redis.WithHook(hook))
	installBufconnDialer(t, harnesses...)
	for _, h := range harnesses {
		registerWorldNodeFromHarness(t, sc, interleaveZone, h)
	}
	return sc
}

// requireActiveInstance 断言场景在 / 不在 zone 的 active 集合里。按成员查(go-zero Zscore 对缺成员回 redis.Nil),
// 不靠键是否存在:集合里有别的成员时,键在不代表这个成员在。
func requireActiveInstance(t *testing.T, sc *svc.ServiceContext, zoneID uint32, sceneID uint64, want bool) {
	t.Helper()
	_, err := sc.Redis.Zscore(activeInstancesKey(zoneID), strconv.FormatUint(sceneID, 10))
	if errors.Is(err, redis.Nil) {
		require.False(t, want, "scene %d 必须仍在 active 集合里", sceneID)
		return
	}
	require.NoError(t, err)
	require.True(t, want, "scene %d 不得留在 active 集合里", sceneID)
}

// 收尾读到属主是 dead 之后,有写者把实例改派到 live(注入点即收尾自己的 GET)。销毁脚本比对期望属主落空:
// 实例原样留给 live,live 的计数一个都不扣,不向 live 发 DestroyScene。
// 修复前:脚本不比对,抹掉全部场景键,DestroyScene 发到 live,live 的 scene_count 与 player_count 被扣。
func TestOwnerReadInterleave_ReconcileSkipsSceneReassignedAfterOwnerRead(t *testing.T) {
	const sceneID uint64 = 89701
	member := strconv.FormatUint(sceneID, 10)
	hook := &afterOwnerReadHook{key: sceneNodeKey(sceneID)}
	live := startFakeSceneNode(t, interleaveNodeLive)
	sc := newInterleaveSvcCtx(t, hook, live)

	require.NoError(t, sc.Redis.Set(sceneZoneKey(sceneID), strconv.FormatUint(uint64(interleaveZone), 10)))
	require.NoError(t, sc.Redis.Set(sceneNodeKey(sceneID), interleaveNodeDead))
	_, err := sc.Redis.Sadd(nodeScenesKey(interleaveZone, interleaveNodeDead), member)
	require.NoError(t, err)
	require.NoError(t, sc.Redis.Set(nodeSceneCountKey(interleaveZone, interleaveNodeDead), "1"))
	_, err = sc.Redis.Zadd(activeInstancesKey(interleaveZone), nowUnix(), member)
	require.NoError(t, err)
	seedSceneMembersVia(sc, sceneID, 2)
	seedNodeMembersVia(sc, interleaveZone, interleaveNodeLive, 5)

	// 前提:dead 已判死、屏障放行,live 存活。
	require.False(t, IsNodeAlive(sc, interleaveZone, interleaveNodeDead))
	require.True(t, IsNodeAlive(sc, interleaveZone, interleaveNodeLive))
	allowed, _ := CanReclaimDeadNode(sc, interleaveZone, interleaveNodeDead)
	require.True(t, allowed)

	hook.arm(func() {
		require.True(t, reassignSceneNode(sc, interleaveZone, sceneID, interleaveNodeDead, interleaveNodeLive),
			"注入的改派必须提交成功")
	})
	entry := nodeEntry{nodeID: interleaveNodeDead}
	entry.reg.ZoneId = interleaveZone
	reconcileDeadNodeScenes(context.Background(), sc, entry, []string{member})

	require.True(t, hook.fired.Load(), "改派必须真的插在收尾读属主之后")
	require.EqualValues(t, 0, live.fake.destroyCalls.Load(), "收尾不得向新属主发 DestroyScene")
	owner, err := sc.Redis.Get(sceneNodeKey(sceneID))
	require.NoError(t, err)
	require.Equal(t, interleaveNodeLive, owner, "属主映射必须原样留给 live")
	requireSceneIndexedOnlyOn(t, sc, interleaveZone, sceneID, interleaveNodeLive, interleaveNodeDead)
	require.EqualValues(t, 1, readInt64(sc, nodeSceneCountKey(interleaveZone, interleaveNodeLive)), "新属主的 scene_count 不得被扣")
	require.Equal(t, "5", nodeCountText(sc, interleaveZone, interleaveNodeLive), "新属主的成员不得被摘")
	require.Equal(t, "2", sceneCountText(sc, sceneID), "实例成员不得被抹")
	requireActiveInstance(t, sc, interleaveZone, sceneID, true)
}

// 显式 DestroyScene 认领之前,场景被改派到 B(注入点即它对属主键的第一次触碰,现在是认领脚本本身)。销毁以认领
// 应答里的属主 B 为准:RPC 发给 B,计数与反向索引从 B 上摘,A 不再被扣。
// 修复前:先快照读到 A,按快照把 RPC 发到 A、A 的 scene_count 被扣成 -1,B 的 scene_count 与反向索引多一份,
// B 上的实体成孤儿。
func TestOwnerReadInterleave_ExplicitDestroyFollowsOwnerAtClaimTime(t *testing.T) {
	const sceneID uint64 = 89702
	member := strconv.FormatUint(sceneID, 10)
	hook := &afterOwnerReadHook{key: sceneNodeKey(sceneID)}
	nodeA := startFakeSceneNode(t, interleaveNodeA)
	nodeB := startFakeSceneNode(t, interleaveNodeB)
	sc := newInterleaveSvcCtx(t, hook, nodeA, nodeB)

	require.NoError(t, sc.Redis.Set(sceneZoneKey(sceneID), strconv.FormatUint(uint64(interleaveZone), 10)))
	require.NoError(t, sc.Redis.Set(sceneNodeKey(sceneID), interleaveNodeA))
	_, err := sc.Redis.Sadd(nodeScenesKey(interleaveZone, interleaveNodeA), member)
	require.NoError(t, err)
	require.NoError(t, sc.Redis.Set(nodeSceneCountKey(interleaveZone, interleaveNodeA), "1"))

	hook.arm(func() {
		require.True(t, reassignSceneNode(sc, interleaveZone, sceneID, interleaveNodeA, interleaveNodeB),
			"注入的改派必须提交成功")
	})
	_, err = NewDestroySceneLogic(context.Background(), sc).DestroyScene(&scene_manager.DestroySceneRequest{
		SceneId: sceneID,
		ZoneId:  interleaveZone,
	})
	require.NoError(t, err)

	require.True(t, hook.fired.Load(), "改派必须真的插在 DestroyScene 认领之前")
	require.EqualValues(t, 1, nodeB.fake.destroyCalls.Load(), "DestroyScene 必须发给认领时的属主 B")
	require.Equal(t, sceneID, nodeB.fake.lastDestroyScene.Load())
	require.EqualValues(t, 0, nodeA.fake.destroyCalls.Load(), "A 已不是属主,不得收到 DestroyScene")

	owner, err := sc.Redis.Get(sceneNodeKey(sceneID))
	require.NoError(t, err)
	require.Empty(t, owner, "属主映射必须被抹掉")
	for _, n := range []string{interleaveNodeA, interleaveNodeB} {
		in, err := sc.Redis.Sismember(nodeScenesKey(interleaveZone, n), member)
		require.NoError(t, err)
		require.False(t, in, "scene %d 不得留在 %s 的反向索引里", sceneID, n)
		require.EqualValues(t, 0, readInt64(sc, nodeSceneCountKey(interleaveZone, n)), "%s 的 scene_count 必须恰好归零", n)
	}
}
