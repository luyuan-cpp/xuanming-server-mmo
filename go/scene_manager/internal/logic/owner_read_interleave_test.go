package logic

// 回归:「先读属主、再销毁」的两条路径,在读与销毁之间被改派时的行为。
//
//   - 死节点收尾 reconcileDeadNodeScenes:读到属主是死节点后,有写者用 reassignSceneNode 把实例改派到活节点
//     (修复时 2026-10-22 是 resolveScene 的死节点自愈;10-23 起它只重建世界频道,这里模拟的是纵深防御要挡的
//     写者)。修复前收尾用不比对属主的 force 脚本照样销毁:DestroyScene 发到新节点,新节点的 scene_count /
//     player_count 被扣,玩家刚进的场景被拆。现在销毁脚本比对期望属主,落空就一个键都不动。
//   - 显式 DestroyScene:快照读到属主 A 后,另一个写者(rebalance 的机会迁移等)把场景改派到 B。修复前按快照
//     把 RPC 发到 A、计数扣在 A 上,B 的 scene_count 与反向索引永久多一份,B 上的实体成了孤儿。现在经
//     destroyInstanceForce 原子认领,以脚本读到的属主 B 为准。
//
// 交错用 go-redis hook 注入(redis.WithHook,见 newIntegrationSvcCtxWithRedisOptions):调用方第一次
// GET scene:{id}:node 拿到结果之后、继续往下走之前,同步提交一次 reassignSceneNode。注入点就是调用方自己的
// 「读属主」。两条用例都从修复前就存在的入口进(reconcileDeadNodeScenes、DestroySceneLogic.DestroyScene),
// 不直接调被修改的 destroyOrphanOfDeadNode / AtomicDestroyForce,所以能原样放到修复前代码上取红。
//
// 修复前红的取法:本文件只用修复前(940ef9746)已有的入口与辅助函数,可直接取红,不需要变异。
// 在 %TEMP% 下 git worktree add --detach <tmp> 940ef9746,把当前的 integration_test.go(多了
// newIntegrationSvcCtxWithRedisOptions)与本文件拷进 <tmp>/go/scene_manager/internal/logic/,
// 跑 go test ./internal/logic/ -count=1 -v -run 'TestOwnerReadInterleave_',期望两条都红:
//   - ReconcileSkipsSceneReassignedAfterOwnerRead:失败信息含「收尾不得向新属主发 DestroyScene」;
//   - ExplicitDestroyFollowsOwnerAtClaimTime:失败信息含「DestroyScene 必须发给认领时的属主」。

import (
	"context"
	"errors"
	"fmt"
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

// afterOwnerReadHook 在第一次「GET key」返回之后、把结果交还调用方之前,同步执行一次 inject。
// inject 里发出的命令同样经过本 hook;fired 先置位,所以不会重入。arm 之前一律放行,用例的现场搭建不受影响。
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
	require.NoError(t, sc.Redis.Set(fmtInstancePlayerCountKey(sceneID), "2"))
	require.NoError(t, sc.Redis.Set(nodePlayerCountKey(interleaveZone, interleaveNodeLive), "5"))

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
	require.EqualValues(t, 5, readInt64(sc, nodePlayerCountKey(interleaveZone, interleaveNodeLive)), "新属主的 player_count 不得被扣")
	require.EqualValues(t, 2, readInt64(sc, fmtInstancePlayerCountKey(sceneID)), "实例人数不得被抹")
	requireActiveInstance(t, sc, interleaveZone, sceneID, true)
}

// 显式 DestroyScene 快照读到属主 A 之后,场景被改派到 B(注入点即它自己的 GET)。销毁以认领时的属主 B 为准:
// RPC 发给 B,计数与反向索引从 B 上摘,A 不再被扣。
// 修复前:按快照把 RPC 发到 A、A 的 scene_count 被扣成 -1,B 的 scene_count 与反向索引多一份,B 上的实体成孤儿。
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

	require.True(t, hook.fired.Load(), "改派必须真的插在 DestroyScene 读属主之后")
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

// fmtInstancePlayerCountKey 是 instance:{id}:player_count,与生产代码同一格式串。
func fmtInstancePlayerCountKey(sceneID uint64) string {
	return fmt.Sprintf(InstancePlayerCountKey, sceneID)
}
