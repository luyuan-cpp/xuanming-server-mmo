package logic

// 负载分刷新里会加成员的写带选主锁核对(leader_gate.go 的 leaderFence、load_refresh.go 的 luaFencedLeaderZadd)。
//
// 接了选主之后,本进程的 isLeader 为真不再够:选主锁键里必须仍是本任期写进去的值,Redis 才执行这条 ZADD。
// 这些用例里 isLeader 一律为真(newLoadRefreshScopeCtx),模拟的正是「自己以为还在任」的副本。
// 节点用 95x 段,理由同 load_refresh_scope_test.go;用例不 sleep、不比较时间。

import (
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"scene_manager/internal/metrics"
)

// leaderTokenForTest 是本副本这一任期写进选主锁的值。
const leaderTokenForTest = "replica-a:0123456789abcdef"

// wireLeaderFenceForTest 让本副本以 leaderTokenForTest 接上选主;held 是此刻选主锁键里的值,空串表示锁键不存在
// (过期了,或主从切换之后新主上没有这把锁)。
func wireLeaderFenceForTest(t *testing.T, mr *miniredis.Miniredis, zoneID uint32, held string) {
	t.Helper()
	t.Cleanup(SetLeaderFenceForTest(func() string { return leaderTokenForTest }))
	if held != "" {
		require.NoError(t, mr.Set(LeaderLockKey(zoneID), held))
	}
}

// 锁还在自己手里:两种会加成员的 ZADD(读成功节点的分数、读失败节点的哨兵分)都照常落地,而且都是在
// 带围栏的脚本里执行的,没有一条绕过核对直接发给 Redis。
func TestRefreshLoadScores_FencedWritesLandWhileTheLockIsHeld(t *testing.T) {
	sc, mr := newLoadRefreshScopeCtx(t)
	wireLeaderFenceForTest(t, mr, 1, leaderTokenForTest)
	registerKnownNodeForTest(t, "SceneNodeService.rpc/fence-950", 1, "950")
	registerKnownNodeForTest(t, "SceneNodeService.rpc/fence-951", 1, "951")
	seedNodeCountersForTest(t, mr, 1, "950", "3", "")
	failedKey := nodeMembersKey(1, "951")
	rec := installRedisCommandRecorderForTest(t, mr, func(cmd string, args []string) string {
		if cmd == "SCARD" && len(args) > 0 && args[0] == failedKey {
			return "ERR injected counter read failure"
		}
		return ""
	})

	refreshLoadScores(sc)

	score, ok := loadScoreForTest(t, mr, 1, "950")
	require.True(t, ok)
	assert.Equal(t, 3.0, score)
	score, ok = loadScoreForTest(t, mr, 1, "951")
	require.True(t, ok, "读失败的非成员照常以哨兵分加入")
	assert.Equal(t, float64(nodeLoadScoreUnknown), score)
	// 钩子也会记下脚本里的 redis.call:负载集上的每条 ZADD 都对应一次 EVAL,没有裸发的。
	assert.Equal(t, 2, rec.count("EVAL", ""), "分数 ZADD 与哨兵 ZADD 各走一次带围栏的脚本")
	assert.Equal(t, 2, rec.count("ZADD", nodeLoadKey(1)))
	assert.True(t, mr.Exists(nodeSceneNodeTypeKey(1, "950")), "类型镜像照常写")
}

// 丢了领导权而自己还不知道的副本:选主锁已被别的副本拿走,或者已经没有了。它的镜像里 952 还活着,
// 新领导者却已经把 952 摘出了负载集。这一拍刷新不能把 952 加回去,也不能覆盖现任领导者给 953 写的分数;
// 记 lost_leadership,不记 write_failed,不导出每节点指标。
func TestRefreshLoadScores_ReplicaWithoutTheLockCannotAddMembers(t *testing.T) {
	cases := map[string]string{
		"lock_taken_by_another_replica": "replica-b:fedcba9876543210",
		"lock_gone":                     "",
	}
	for name, held := range cases {
		t.Run(name, func(t *testing.T) {
			sc, mr := newLoadRefreshScopeCtx(t)
			wireLeaderFenceForTest(t, mr, 1, held)
			registerKnownNodeForTest(t, "SceneNodeService.rpc/fence-952", 1, "952")
			registerKnownNodeForTest(t, "SceneNodeService.rpc/fence-953", 1, "953")
			_, err := mr.ZAdd(nodeLoadKey(1), 100, "953")
			require.NoError(t, err)
			seedNodeCountersForTest(t, mr, 1, "952", "2", "")
			seedNodeCountersForTest(t, mr, 1, "953", "7", "")
			lostBefore := loadRefreshOutcomeCountForTest(t, metrics.LoadRefreshOutcomeLostLeadership)
			writeFailedBefore := loadRefreshOutcomeCountForTest(t, metrics.LoadRefreshOutcomeWriteFailed)

			refreshLoadScores(sc)

			_, ok := loadScoreForTest(t, mr, 1, "952")
			assert.False(t, ok, "被摘掉的节点不能被没有锁的副本加回负载集")
			score, ok := loadScoreForTest(t, mr, 1, "953")
			require.True(t, ok)
			assert.Equal(t, 100.0, score, "现任领导者写的分数不被覆盖")
			assert.Equal(t, lostBefore+1, loadRefreshOutcomeCountForTest(t, metrics.LoadRefreshOutcomeLostLeadership))
			assert.Equal(t, writeFailedBefore, loadRefreshOutcomeCountForTest(t, metrics.LoadRefreshOutcomeWriteFailed),
				"被围栏拒掉是丢了领导权,不是写失败")
			_, exported := nodeGaugeSeriesForTest(t, "scene_manager_node_load_score")["1/953"]
			assert.False(t, exported, "没写进 Redis 的分数不得作为每节点指标导出")
		})
	}
}

// 围栏值为空 = 选主器已经知道自己不在任(只是 isLeader 的读者还没看到)。这时会加成员的写一条都不发。
func TestRefreshLoadScores_OutOfOfficeSendsNoMembershipWrite(t *testing.T) {
	sc, mr := newLoadRefreshScopeCtx(t)
	t.Cleanup(SetLeaderFenceForTest(func() string { return "" }))
	registerKnownNodeForTest(t, "SceneNodeService.rpc/fence-954", 1, "954")
	seedNodeCountersForTest(t, mr, 1, "954", "1", "")
	rec := installRedisCommandRecorderForTest(t, mr, nil)
	lostBefore := loadRefreshOutcomeCountForTest(t, metrics.LoadRefreshOutcomeLostLeadership)

	refreshLoadScores(sc)

	assert.Zero(t, rec.count("ZADD", ""))
	assert.Zero(t, rec.count("EVAL", ""))
	_, ok := loadScoreForTest(t, mr, 1, "954")
	assert.False(t, ok)
	assert.Equal(t, lostBefore+1, loadRefreshOutcomeCountForTest(t, metrics.LoadRefreshOutcomeLostLeadership))
}

// 发给 Redis 的参数形状:会加成员的 ZADD 改成一次 EVAL,KEYS 是「选主锁键, 负载集键」(同一个 zone 的 tag),
// ARGV 是「围栏值, ZADD 在键之后的全部参数」;不加成员的命令、没接选主时的命令原样发。
func TestLoadRefreshWireArgs(t *testing.T) {
	zadd := loadRefreshWriteCmd{zoneID: 7, scores: true, addsMembers: true, args: []any{"ZADD", nodeLoadKey(7), int64(3), "950"}}
	zaddNX := loadRefreshWriteCmd{zoneID: 7, addsMembers: true, args: []any{"ZADD", nodeLoadKey(7), "NX", nodeLoadScoreUnknown, "951"}}
	set := loadRefreshWriteCmd{zoneID: 7, args: []any{"SET", nodeSceneNodeTypeKey(7, "950"), "1"}}

	// 没接选主:全部原样。
	t.Cleanup(SetLeaderFenceForTest(nil))
	for _, c := range []loadRefreshWriteCmd{zadd, zaddNX, set} {
		args, inOffice := loadRefreshWireArgs(c)
		require.True(t, inOffice)
		assert.Equal(t, c.args, args)
	}

	// 接了选主:两种 ZADD 带围栏,SET 原样。锁键按命令的 zone 取。
	restore := SetLeaderFenceForTest(func() string { return leaderTokenForTest })
	args, inOffice := loadRefreshWireArgs(zadd)
	require.True(t, inOffice)
	assert.Equal(t, []any{"EVAL", luaFencedLeaderZadd, 2, LeaderLockKey(7), nodeLoadKey(7), leaderTokenForTest, int64(3), "950"}, args)
	args, inOffice = loadRefreshWireArgs(zaddNX)
	require.True(t, inOffice)
	assert.Equal(t, []any{"EVAL", luaFencedLeaderZadd, 2, LeaderLockKey(7), nodeLoadKey(7), leaderTokenForTest, "NX", nodeLoadScoreUnknown, "951"}, args)
	args, inOffice = loadRefreshWireArgs(set)
	require.True(t, inOffice)
	assert.Equal(t, set.args, args)
	restore()

	// 接了选主但已不在任:会加成员的写不得发,不加成员的照旧。
	t.Cleanup(SetLeaderFenceForTest(func() string { return "" }))
	_, inOffice = loadRefreshWireArgs(zadd)
	assert.False(t, inOffice)
	_, inOffice = loadRefreshWireArgs(set)
	assert.True(t, inOffice)
}

// 一条 ZADD 最多带 loadRefreshZaddMaxMembers 个成员(脚本里 unpack 展开参数有上限),多了拆成几条;
// 拆开之后「分数, 成员」仍成对,每条都标着会加成员,读成功的那几条都标着 scores。
func TestBuildLoadRefreshWrites_SplitsLargeZadds(t *testing.T) {
	sc, _ := newLoadRefreshScopeCtx(t)
	const zone = uint32(96)
	total := loadRefreshZaddMaxMembers + 5
	nodes := make([]loadRefreshNode, 0, total+1)
	for i := 0; i < total; i++ {
		nodes = append(nodes, loadRefreshNode{zoneID: zone, nodeID: fmt.Sprintf("96%04d", i), sceneCount: int64(i), readOK: true})
	}
	nodes = append(nodes, loadRefreshNode{zoneID: zone, nodeID: "96unread"})

	cmds := buildLoadRefreshWrites(sc, []uint32{zone}, map[uint32][]loadRefreshNode{zone: nodes})

	scoredMembers, scoredCmds, sentinelCmds := 0, 0, 0
	for _, c := range cmds {
		if c.args[0] != "ZADD" {
			assert.False(t, c.addsMembers, "类型镜像不加成员")
			continue
		}
		require.True(t, c.addsMembers, "两种 ZADD 都会加成员")
		require.Equal(t, nodeLoadKey(zone), c.args[1])
		if c.scores {
			pairs := c.args[2:]
			require.Zero(t, len(pairs)%2, "分数与成员成对")
			require.LessOrEqual(t, len(pairs)/2, loadRefreshZaddMaxMembers)
			for i := 0; i < len(pairs); i += 2 {
				_, isScore := pairs[i].(int64)
				_, isMember := pairs[i+1].(string)
				require.True(t, isScore && isMember, "第 %d 对不是「分数, 成员」: %v %v", i/2, pairs[i], pairs[i+1])
			}
			scoredMembers += len(pairs) / 2
			scoredCmds++
			continue
		}
		assert.Equal(t, []any{"ZADD", nodeLoadKey(zone), "NX", nodeLoadScoreUnknown, "96unread"}, c.args)
		sentinelCmds++
	}
	assert.Equal(t, total, scoredMembers, "拆开之后一个成员都不丢")
	assert.Equal(t, 2, scoredCmds)
	assert.Equal(t, 1, sentinelCmds)
}
