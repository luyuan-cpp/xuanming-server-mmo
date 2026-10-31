package logic

// 成员集合对账(scene_member_reconcile.go,docs/design/redis-player-store-cluster.md §5.10)。

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gproto "google.golang.org/protobuf/proto"

	"proto/scene_manager"
	"scene_manager/internal/svc"
)

const (
	reconcileMemberNode  = "10"
	reconcileMemberScene = uint64(7901)
)

// newMemberReconcileCase 摆一个在册节点和它名下的一个场景,集合都是空的。
func newMemberReconcileCase(t *testing.T) (*svc.ServiceContext, *miniredis.Miniredis) {
	t.Helper()
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	_, err := mr.SetAdd(sceneManagerZonesKey, strconv.FormatUint(uint64(testZoneId), 10))
	require.NoError(t, err)
	_, err = mr.ZAdd(nodeLoadKey(testZoneId), 0, reconcileMemberNode)
	require.NoError(t, err)
	_, err = mr.SetAdd(nodeScenesKey(testZoneId, reconcileMemberNode), strconv.FormatUint(reconcileMemberScene, 10))
	require.NoError(t, err)
	return sc, mr
}

func seedPlayerLocation(t *testing.T, mr *miniredis.Miniredis, player, sceneID uint64, nodeID string) {
	t.Helper()
	raw, err := gproto.Marshal(&scene_manager.PlayerLocation{SceneId: sceneID, NodeId: nodeID, ZoneId: testZoneId, OwnerEpoch: 1})
	require.NoError(t, err)
	require.NoError(t, mr.Set(getPlayerLocationKey(player), string(raw)))
}

func addReconcileMembers(t *testing.T, mr *miniredis.Miniredis, key string, players ...uint64) {
	t.Helper()
	for _, player := range players {
		_, err := mr.SetAdd(key, strconv.FormatUint(player, 10))
		require.NoError(t, err)
	}
}

// membersOf 读集合成员,排好序。键不存在是空切片。
func membersOf(t *testing.T, mr *miniredis.Miniredis, key string) []string {
	t.Helper()
	if !mr.Exists(key) {
		return []string{}
	}
	members, err := mr.SMembers(key)
	require.NoError(t, err)
	sort.Strings(members)
	return members
}

// 崩溃留下的成员被摘掉,location 指向这里的成员不动。场景集合与节点集合各自按自己的那一半判。
func TestSceneMemberReconcileRemovesStaleMembers(t *testing.T) {
	sc, mr := newMemberReconcileCase(t)
	const (
		here      = uint64(1001) // 就在这个场景
		otherScn  = uint64(1002) // 同节点的另一个场景:场景集合里是残留,节点集合里不是
		otherNode = uint64(1003) // 别的节点
		noLoc     = uint64(1004) // 没有 location
	)
	seedPlayerLocation(t, mr, here, reconcileMemberScene, reconcileMemberNode)
	seedPlayerLocation(t, mr, otherScn, reconcileMemberScene+1, reconcileMemberNode)
	seedPlayerLocation(t, mr, otherNode, reconcileMemberScene+2, "11")
	sceneKey, nodeKey := instanceMembersKey(reconcileMemberScene), nodeMembersKey(testZoneId, reconcileMemberNode)
	addReconcileMembers(t, mr, sceneKey, here, otherScn, otherNode, noLoc)
	addReconcileMembers(t, mr, nodeKey, here, otherScn, otherNode, noLoc)

	(&sceneMemberReconciler{}).runRound(context.Background(), sc, 100, 100)

	assert.Equal(t, []string{"1001"}, membersOf(t, mr, sceneKey))
	assert.Equal(t, []string{"1001", "1002"}, membersOf(t, mr, nodeKey))
}

// 空闲实例的残留摘掉之后,销毁判据看到 0 人。这是对账存在的理由。
func TestSceneMemberReconcileUnblocksIdleDestroy(t *testing.T) {
	sc, mr := newMemberReconcileCase(t)
	addReconcileMembers(t, mr, instanceMembersKey(reconcileMemberScene), 1101)
	require.EqualValues(t, 1, readScenePlayerCount(sc, reconcileMemberScene))

	(&sceneMemberReconciler{}).runRound(context.Background(), sc, 100, 100)

	assert.EqualValues(t, 0, readScenePlayerCount(sc, reconcileMemberScene))
}

// 每轮读 location 的成员数不超过预算,余下的下一轮接着走,不会漏掉。SSCAN 一批可能比预算大(miniredis 忽略 COUNT,小集合一次回完)。
func TestSceneMemberReconcileHonorsMemberBudgetAndResumes(t *testing.T) {
	sc, mr := newMemberReconcileCase(t)
	sceneKey := instanceMembersKey(reconcileMemberScene)
	addReconcileMembers(t, mr, sceneKey, 1201, 1202, 1203, 1204, 1205)
	var mu sync.Mutex
	locationReads := 0
	mr.Server().SetPreHook(func(_ *server.Peer, cmd string, args ...string) bool {
		if cmd == "MGET" {
			mu.Lock()
			locationReads += len(args)
			mu.Unlock()
		}
		return false
	})
	t.Cleanup(func() { mr.Server().SetPreHook(nil) })
	reads := func() int {
		mu.Lock()
		defer mu.Unlock()
		return locationReads
	}

	r := &sceneMemberReconciler{}
	// 节点集合排在场景集合前面,它是空的,只占一条 SSCAN。
	r.runRound(context.Background(), sc, 2, 100)
	assert.Equal(t, 2, reads(), "第一轮只读预算内的成员")
	assert.Len(t, membersOf(t, mr, sceneKey), 3)
	r.runRound(context.Background(), sc, 2, 100)
	assert.Equal(t, 4, reads())
	assert.Len(t, membersOf(t, mr, sceneKey), 1)
	r.runRound(context.Background(), sc, 2, 100)
	assert.Empty(t, membersOf(t, mr, sceneKey), "第三轮走完余下的成员")
}

// SSCAN 预算用完就停,下一轮接着走下一个集合。
func TestSceneMemberReconcileHonorsScanBudget(t *testing.T) {
	sc, mr := newMemberReconcileCase(t)
	sceneKey := instanceMembersKey(reconcileMemberScene)
	addReconcileMembers(t, mr, sceneKey, 1301)

	r := &sceneMemberReconciler{}
	r.runRound(context.Background(), sc, 100, 1) // 这一条用在排在前面的节点集合上
	assert.Equal(t, []string{"1301"}, membersOf(t, mr, sceneKey), "预算用完,还没走到场景集合")
	r.runRound(context.Background(), sc, 100, 1)
	assert.Empty(t, membersOf(t, mr, sceneKey))
}

// 走完一遍之后新出现的残留,下一遍能看到。
func TestSceneMemberReconcileStartsNextPass(t *testing.T) {
	sc, mr := newMemberReconcileCase(t)
	sceneKey := instanceMembersKey(reconcileMemberScene)

	r := &sceneMemberReconciler{}
	r.runRound(context.Background(), sc, 100, 100)
	addReconcileMembers(t, mr, sceneKey, 1401)
	r.runRound(context.Background(), sc, 100, 100)
	r.runRound(context.Background(), sc, 100, 100)
	assert.Empty(t, membersOf(t, mr, sceneKey))
}

// 在途预占(还没落点)被摘掉是安全的:落点提交之后再 SADD 一次,成员身份回来,下一遍不会再摘。
func TestSceneMemberReconcileInFlightReservationRecovers(t *testing.T) {
	sc, mr := newMemberReconcileCase(t)
	const player = uint64(1501)
	sceneKey := instanceMembersKey(reconcileMemberScene)
	addReconcileMembers(t, mr, sceneKey, player)

	r := &sceneMemberReconciler{}
	r.runRound(context.Background(), sc, 100, 100)
	require.Empty(t, membersOf(t, mr, sceneKey), "前提:预占被当成残留摘掉")

	seedPlayerLocation(t, mr, player, reconcileMemberScene, reconcileMemberNode)
	require.NoError(t, addSceneMember(sc, player, reconcileMemberScene, testZoneId, reconcileMemberNode))
	r.reset()
	r.runRound(context.Background(), sc, 100, 100)
	assert.Equal(t, []string{"1501"}, membersOf(t, mr, sceneKey))
}

// 在途预占不会被摘:成员要连续超过宽限不指向这里才摘,中途落点就重新计时。
func TestSceneMemberReconcileWaitsOutGraceBeforeRemoving(t *testing.T) {
	sc, mr := newMemberReconcileCase(t)
	const (
		stale  = uint64(1521) // 一直没有 location
		landed = uint64(1522) // 第二遍之前落点,第三遍之前又离开
	)
	sceneKey := instanceMembersKey(reconcileMemberScene)
	addReconcileMembers(t, mr, sceneKey, stale, landed)
	clock := time.Unix(1000, 0)
	r := &sceneMemberReconciler{grace: time.Minute, now: func() time.Time { return clock }}
	pass := func(elapsed time.Duration) {
		clock = clock.Add(elapsed)
		r.runRound(context.Background(), sc, 100, 100)
	}

	pass(0)
	assert.Equal(t, []string{"1521", "1522"}, membersOf(t, mr, sceneKey), "第一次看到只记时刻")

	seedPlayerLocation(t, mr, landed, reconcileMemberScene, reconcileMemberNode)
	pass(59 * time.Second)
	assert.Equal(t, []string{"1521", "1522"}, membersOf(t, mr, sceneKey), "还没到宽限")

	mr.Del(getPlayerLocationKey(landed))
	pass(time.Second)
	assert.Equal(t, []string{"1522"}, membersOf(t, mr, sceneKey), "到宽限的摘掉;落过点的重新计时")

	pass(59 * time.Second)
	assert.Equal(t, []string{"1522"}, membersOf(t, mr, sceneKey))
	pass(time.Second)
	assert.Empty(t, membersOf(t, mr, sceneKey))
	assert.Empty(t, r.suspects, "摘掉之后不留记录")
}

// 一遍里没再看到的记录作废:成员被别的路径摘掉、很久以后又进来,要重新等宽限。reset 只丢记录,宽限还在。
func TestSceneMemberReconcileForgetsSuspectsMissingFromAPass(t *testing.T) {
	sc, mr := newMemberReconcileCase(t)
	const player = uint64(1531)
	sceneKey := instanceMembersKey(reconcileMemberScene)
	addReconcileMembers(t, mr, sceneKey, player)
	clock := time.Unix(1000, 0)
	r := &sceneMemberReconciler{grace: time.Minute, now: func() time.Time { return clock }}

	r.runRound(context.Background(), sc, 100, 100)
	require.Len(t, r.suspects, 1)
	_, err := mr.SRem(sceneKey, strconv.FormatUint(player, 10))
	require.NoError(t, err)
	r.runRound(context.Background(), sc, 100, 100)
	require.Empty(t, r.suspects)

	addReconcileMembers(t, mr, sceneKey, player)
	clock = clock.Add(time.Hour)
	r.runRound(context.Background(), sc, 100, 100)
	assert.Equal(t, []string{"1531"}, membersOf(t, mr, sceneKey), "重新进来要重新等")

	r.reset()
	assert.Empty(t, r.suspects)
	clock = clock.Add(time.Hour)
	r.runRound(context.Background(), sc, 100, 100)
	assert.Equal(t, []string{"1531"}, membersOf(t, mr, sceneKey), "reset 之后仍按宽限走")
}

// 强制销毁时场景里剩的成员逐个按 location 判:指向同节点别的场景的玩家留在节点集合里,其余摘掉。
func TestForceDestroyRemovesResidualMembersByLocation(t *testing.T) {
	sc, mr := newMemberReconcileCase(t)
	const (
		stale      = uint64(1541) // 没有 location
		otherScene = uint64(1542) // location 指向同节点的另一个场景
	)
	seedPlayerLocation(t, mr, otherScene, reconcileMemberScene+1, reconcileMemberNode)
	sceneKey, nodeKey := instanceMembersKey(reconcileMemberScene), nodeMembersKey(testZoneId, reconcileMemberNode)
	addReconcileMembers(t, mr, sceneKey, stale, otherScene)
	addReconcileMembers(t, mr, nodeKey, stale, otherScene)
	require.NoError(t, mr.Set(fmt.Sprintf(SceneNodeKeyFmt, reconcileMemberScene), reconcileMemberNode))
	// 节点不在负载集里就不发 DestroyScene,用例不用起假节点;摘成员的路径和活节点是同一段。
	_, err := mr.ZRem(nodeLoadKey(testZoneId), reconcileMemberNode)
	require.NoError(t, err)

	destroyed, err := destroyInstanceForce(context.Background(), sc, testZoneId, reconcileMemberScene, "explicit")

	require.NoError(t, err)
	require.True(t, destroyed)
	assert.Empty(t, membersOf(t, mr, sceneKey))
	assert.Equal(t, []string{"1542"}, membersOf(t, mr, nodeKey))
}

// afterLocationReadHook 挂在玩家域句柄上:第一次读完 location 快照之后同步执行一次 inject。
type afterLocationReadHook struct {
	fired  atomic.Bool
	inject func()
}

func (h *afterLocationReadHook) DialHook(next goredis.DialHook) goredis.DialHook { return next }

func (h *afterLocationReadHook) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return next
}

func (h *afterLocationReadHook) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		err := next(ctx, cmd)
		args := cmd.Args()
		if cmd.Name() == "eval" && len(args) >= 2 && args[1] == luaReadLocationSnapshot && h.fired.CompareAndSwap(false, true) {
			h.inject()
		}
		return err
	}
}

// 摘除时第一次读 location 之后、SREM 之前玩家落到这里:SREM 之后的第二次读把成员加回去(§5.10 第 3 条)。
func TestSceneMemberReconcileAddsBackWhenPlayerArrivesDuringRemoval(t *testing.T) {
	sc, mr := newMemberReconcileCase(t)
	const player = uint64(1551)
	sceneKey := instanceMembersKey(reconcileMemberScene)
	addReconcileMembers(t, mr, sceneKey, player)
	hook := &afterLocationReadHook{inject: func() { seedPlayerLocation(t, mr, player, reconcileMemberScene, reconcileMemberNode) }}
	sc.PlayerRedis.AddHook(hook)

	(&sceneMemberReconciler{}).runRound(context.Background(), sc, 100, 100)

	require.True(t, hook.fired.Load(), "前提:对账判它是残留,走到了摘除")
	assert.Equal(t, []string{"1551"}, membersOf(t, mr, sceneKey))
}

// 领导权丢了就不动,遍历位置也丢掉。
func TestSceneMemberReconcileFollowerDoesNothing(t *testing.T) {
	sc, mr := newMemberReconcileCase(t)
	sceneKey := instanceMembersKey(reconcileMemberScene)
	addReconcileMembers(t, mr, sceneKey, 1601)
	restore := SetLeaderCheckForTest(func() bool { return false })
	t.Cleanup(restore)

	r := &sceneMemberReconciler{pending: []sceneMemberSet{{key: sceneKey, sceneID: reconcileMemberScene, zoneID: testZoneId}}}
	r.runRound(context.Background(), sc, 100, 100)

	assert.Equal(t, []string{"1601"}, membersOf(t, mr, sceneKey))
	assert.Empty(t, r.pending)
}

// location 写坏:这是另一个事故,成员不动。读 location 失败同样不动,不能把「读不到」当成「不指向这里」。
func TestSceneMemberReconcileKeepsMemberWhenLocationUnknown(t *testing.T) {
	t.Run("corrupt_location", func(t *testing.T) {
		sc, mr := newMemberReconcileCase(t)
		const player = uint64(1701)
		sceneKey := instanceMembersKey(reconcileMemberScene)
		addReconcileMembers(t, mr, sceneKey, player)
		require.NoError(t, mr.Set(getPlayerLocationKey(player), "\xff\xff\xff"))
		(&sceneMemberReconciler{}).runRound(context.Background(), sc, 100, 100)
		assert.Equal(t, []string{"1701"}, membersOf(t, mr, sceneKey))
	})
	t.Run("location_read_fails", func(t *testing.T) {
		sc, mr := newMemberReconcileCase(t)
		sceneKey := instanceMembersKey(reconcileMemberScene)
		addReconcileMembers(t, mr, sceneKey, 1702)
		mr.Server().SetPreHook(func(peer *server.Peer, cmd string, _ ...string) bool {
			if cmd == "MGET" {
				peer.WriteError("ERR injected")
				return true
			}
			return false
		})
		t.Cleanup(func() { mr.Server().SetPreHook(nil) })
		(&sceneMemberReconciler{}).runRound(context.Background(), sc, 100, 100)
		assert.Equal(t, []string{"1702"}, membersOf(t, mr, sceneKey))
	})
}

// 不是合法 player_id 的成员没有 location 可核,摘掉。
func TestSceneMemberReconcileRemovesMalformedMember(t *testing.T) {
	sc, mr := newMemberReconcileCase(t)
	sceneKey := instanceMembersKey(reconcileMemberScene)
	_, err := mr.SetAdd(sceneKey, "not-a-player", "0")
	require.NoError(t, err)

	(&sceneMemberReconciler{}).runRound(context.Background(), sc, 100, 100)

	assert.Empty(t, membersOf(t, mr, sceneKey))
}

// 负载集里没有的节点(已判死)不归对账管:它的集合由死节点收尾删。
func TestSceneMemberReconcileIgnoresNodesOutOfLoadSet(t *testing.T) {
	sc, mr := newMemberReconcileCase(t)
	deadKey := nodeMembersKey(testZoneId, "99")
	addReconcileMembers(t, mr, deadKey, 1801)

	(&sceneMemberReconciler{}).runRound(context.Background(), sc, 100, 100)

	assert.Equal(t, []string{"1801"}, membersOf(t, mr, deadKey))
}

// 空集合没有可核对的成员:一轮走完不报错,也不空转到预算耗尽。
func TestSceneMemberReconcileNoSetsIsNoop(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)
	r := &sceneMemberReconciler{}
	r.runRound(context.Background(), sc, 100, 100)
	assert.Empty(t, r.pending)
}
