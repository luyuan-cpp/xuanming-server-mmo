package logic

// 负载分刷新、清扫与孤儿清理的行为用例(2026-10-16,load_refresh.go;2026-11-05 zone 登记集合删除后按「只处理本 zone」改写,
// docs/design/control-plane-zone-scoping-20261105.md §4.3)。
//
// outcome 一律写字面量。用到较新符号的用例放在 load_refresh_test.go。
//
// 钉住的行为:
//   - 跟随者一条 Redis 命令都不发,nodes_by_role 只发布本 zone,不导出每节点序列;降级后序列消失;
//   - 领导者每拍刷新镜像里的节点、重建负载集成员;每个 zone 的分数写入不逐节点发 ZADD;不再写 zone 登记集合;
//   - 计数读失败(命令报错 / 值是空串)不按 0 写:成员保留旧分,非成员以「最后才被选中」的分数加入;
//   - 清扫与孤儿清理只处理本进程的 zone(同组别的 zone 在同一个实例里也不碰),一条 SCAN 都不发;期望数字段最后删。
//
// 节点 / zone 用 9xx 段:每节点 gauge 与 outcome 计数器在默认 Prometheus 注册表里是包级全局的,
// 9xx 不与其它用例的序列混淆;计数器只比较前后差值。用例不 sleep、不比较时间。

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"scene_manager/internal/constants"
	"scene_manager/internal/svc"
)

// removedZoneRegistryKeyForTest 是 2026-11-05 删除的 zone 登记集合;用例只断言它不再被写。
const removedZoneRegistryKeyForTest = "scene_manager:zones"

// redisCommandRecorderForTest 按到达顺序记下 miniredis 收到的业务命令,形如 "GET node:zone:1:910:scene_count"
// (命令名 + 第一个参数)。
type redisCommandRecorderForTest struct {
	mu   sync.Mutex
	cmds []string
}

func (r *redisCommandRecorderForTest) record(cmd string) {
	r.mu.Lock()
	r.cmds = append(r.cmds, cmd)
	r.mu.Unlock()
}

func (r *redisCommandRecorderForTest) commands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.cmds...)
}

// count 返回命令名为 name 的条数;firstArg 非空时还要求第一个参数相同。
func (r *redisCommandRecorderForTest) count(name, firstArg string) int {
	n := 0
	for _, c := range r.commands() {
		gotName, gotArg, _ := strings.Cut(c, " ")
		if gotName == name && (firstArg == "" || gotArg == firstArg) {
			n++
		}
	}
	return n
}

// 连接握手 / 保活命令不算业务命令。
var redisHandshakeCommandsForTest = map[string]bool{
	"HELLO": true, "CLIENT": true, "PING": true, "AUTH": true, "SELECT": true, "READONLY": true,
}

// installRedisCommandRecorderForTest 给 miniredis 装命令记录钩子。fail 非 nil 且返回非空串时,
// 该命令不执行,以这个串作为错误回复。约束同 installNodeDetachFaultsForTest:
//   - 钩子会被 Lua 里的 redis.call 带锁重入,fail 里只能读写自己的状态,绝不调 mr.*;
//   - 每个 svcCtx 注入的失败控制在 5 次以内,否则会触发 go-zero 自带的熔断;
//   - 与 mr.SetError 共用同一个钩子槽位,不要混用。
func installRedisCommandRecorderForTest(t *testing.T, mr *miniredis.Miniredis, fail func(cmd string, args []string) string) *redisCommandRecorderForTest {
	t.Helper()
	rec := &redisCommandRecorderForTest{}
	mr.Server().SetPreHook(func(c *server.Peer, cmd string, args ...string) bool {
		if redisHandshakeCommandsForTest[cmd] {
			return false
		}
		first := ""
		if len(args) > 0 {
			first = args[0]
		}
		rec.record(cmd + " " + first)
		if fail != nil {
			if msg := fail(cmd, args); msg != "" {
				c.WriteError(msg)
				return true
			}
		}
		return false
	})
	t.Cleanup(func() { mr.Server().SetPreHook(nil) })
	return rec
}

// newLoadRefreshScopeCtx:干净的注册表镜像与死节点队列 + 自己的 miniredis + 显式的负载分权重,默认是领导者。
func newLoadRefreshScopeCtx(t *testing.T) (*svc.ServiceContext, *miniredis.Miniredis) {
	t.Helper()
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	resetNodeDeathQueuesForTest(t)
	t.Cleanup(SetLeaderCheckForTest(func() bool { return true }))
	sc, mr := newTestSvcCtx(t, "")
	sc.Config.NodeLoadWeightSceneCount = 1
	sc.Config.NodeLoadWeightPlayerCount = 0.01
	return sc, mr
}

// seedNodeCountersForTest 写一个节点的场景数与成员集合;传空串表示不写那个键(读作 0)。
func seedNodeCountersForTest(t *testing.T, mr *miniredis.Miniredis, zoneID uint32, nodeID, sceneCount, playerCount string) {
	t.Helper()
	if sceneCount != "" {
		require.NoError(t, mr.Set(nodeSceneCountKey(zoneID, nodeID), sceneCount))
	}
	if playerCount != "" {
		seedNodeMembers(mr, zoneID, nodeID, mustAtoi(playerCount))
	}
}

// loadScoreForTest 返回节点在负载集里的分数;不是成员时 ok=false(mr.ZScore 对不存在的成员返回 0,不能直接用)。
func loadScoreForTest(t *testing.T, mr *miniredis.Miniredis, zoneID uint32, nodeID string) (float64, bool) {
	t.Helper()
	if !slices.Contains(loadSetMembersForTest(t, mr, zoneID), nodeID) {
		return 0, false
	}
	score, err := mr.ZScore(nodeLoadKey(zoneID), nodeID)
	require.NoError(t, err)
	return score, true
}

// nodeGaugeSeriesForTest 读默认注册表里一个每节点 gauge 的全部序列,键为 "zone/node"。
func nodeGaugeSeriesForTest(t *testing.T, name string) map[string]float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	series := make(map[string]float64)
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			var zone, node string
			for _, pair := range metric.GetLabel() {
				switch pair.GetName() {
				case "zone_id":
					zone = pair.GetValue()
				case "node_id":
					node = pair.GetValue()
				}
			}
			series[zone+"/"+node] = metric.GetGauge().GetValue()
		}
	}
	return series
}

func loadRefreshOutcomeCountForTest(t *testing.T, outcome string) float64 {
	t.Helper()
	return counterValueForTest(t, "scene_manager_load_refresh_total", map[string]string{"outcome": outcome})
}

// ── 跟随者 ────────────────────────────────────────────────────────────────

// 跟随者每拍刷新与 watch PUT 的单节点刷新都不碰 Redis:以前它每个节点每拍发 2 个 GET,只为发每节点指标。
func TestRefreshLoadScores_FollowerIssuesNoRedisCommands(t *testing.T) {
	sc, mr := newLoadRefreshScopeCtx(t)
	t.Cleanup(SetLeaderCheckForTest(func() bool { return false }))
	registerKnownNodeForTest(t, "SceneNodeService.rpc/scope-910", 1, "910")
	registerKnownNodeForTest(t, "SceneNodeService.rpc/scope-911", 1, "911")
	registerKnownNodeForTest(t, "SceneNodeService.rpc/scope-920", 2, "920")
	seedNodeCountersForTest(t, mr, 1, "910", "1", "10")
	seedNodeCountersForTest(t, mr, 1, "911", "2", "20")
	seedNodeCountersForTest(t, mr, 2, "920", "3", "30")
	rec := installRedisCommandRecorderForTest(t, mr, nil)

	refreshLoadScores(sc)
	entry := nodeEntry{nodeID: "910"}
	entry.reg.ZoneId = 1
	updateNodeLoad(sc, entry)

	assert.Empty(t, rec.commands(), "跟随者不得向 Redis 发任何命令")
	assert.Empty(t, loadSetMembersForTest(t, mr, 1))
	assert.Empty(t, loadSetMembersForTest(t, mr, 2))
}

// 跟随者只发布本进程 ZoneId 的 nodes_by_role(PoolEmpty 右支靠它补 0),不发别的 zone,也不导出每节点序列。
func TestRefreshLoadScores_FollowerPublishesOnlyOwnZone(t *testing.T) {
	sc, mr := newLoadRefreshScopeCtx(t)
	t.Cleanup(SetLeaderCheckForTest(func() bool { return false }))
	sc.Config.ZoneId = 2
	registerKnownNodeForTest(t, "SceneNodeService.rpc/scope-912", 1, "912")
	registerKnownNodeForTest(t, "SceneNodeService.rpc/scope-913", 1, "913")
	registerKnownNodeForTest(t, "SceneNodeService.rpc/scope-921", 2, "921")
	seedNodeCountersForTest(t, mr, 1, "912", "1", "")
	seedNodeCountersForTest(t, mr, 2, "921", "1", "")

	refreshLoadScores(sc)

	byRole := nodesByRoleSeriesForTest(t)
	for key := range byRole {
		assert.False(t, strings.HasPrefix(key, "1/"), "跟随者不得发布别的 zone: %s", key)
	}
	assert.Equal(t, 1.0, byRole["2/main_world"])
	v, ok := byRole["2/instance"]
	assert.True(t, ok, "本 zone 的空角色仍要补 0")
	assert.Zero(t, v)

	scores := nodeGaugeSeriesForTest(t, "scene_manager_node_load_score")
	for _, key := range []string{"1/912", "1/913", "2/921"} {
		_, exported := scores[key]
		assert.False(t, exported, "跟随者不得导出每节点序列: %s", key)
	}
}

// 领导者导出每节点序列;降级后的第一拍就把它们清掉(切主时序列换 pod,不能两个 pod 各留一份)。
func TestRefreshLoadScores_LeaderDemotionDropsNodeSeries(t *testing.T) {
	sc, mr := newLoadRefreshScopeCtx(t)
	var leading atomic.Bool
	leading.Store(true)
	t.Cleanup(SetLeaderCheckForTest(leading.Load))
	registerKnownNodeForTest(t, "SceneNodeService.rpc/scope-930", 1, "930")
	seedNodeCountersForTest(t, mr, 1, "930", "2", "")

	refreshLoadScores(sc)
	_, exported := nodeGaugeSeriesForTest(t, "scene_manager_node_load_score")["1/930"]
	require.True(t, exported, "前提:领导者导出每节点序列")

	leading.Store(false)
	refreshLoadScores(sc)
	_, exported = nodeGaugeSeriesForTest(t, "scene_manager_node_load_score")["1/930"]
	assert.False(t, exported, "降级后每节点序列必须消失")
}

// ── 领导者 ────────────────────────────────────────────────────────────────

// 领导者每拍刷新镜像里的全部节点,并把 Redis 丢掉的成员加回负载集(自愈)。
func TestRefreshLoadScores_LeaderRefreshesEveryZoneAndHealsMembership(t *testing.T) {
	sc, mr := newLoadRefreshScopeCtx(t)
	registerKnownNodeForTest(t, "SceneNodeService.rpc/scope-940", 1, "940")
	registerKnownNodeForTest(t, "SceneNodeService.rpc/scope-950", 2, "950")
	seedNodeCountersForTest(t, mr, 1, "940", "2", "300")
	seedNodeCountersForTest(t, mr, 2, "950", "1", "0")

	refreshLoadScores(sc)

	score, ok := loadScoreForTest(t, mr, 1, "940")
	require.True(t, ok, "丢掉的成员必须被加回负载集")
	assert.Equal(t, 5.0, score, "2·1 + 300·0.01")
	score, ok = loadScoreForTest(t, mr, 2, "950")
	require.True(t, ok)
	assert.Equal(t, 1.0, score)
	assert.True(t, mr.Exists(nodeSceneNodeTypeKey(1, "940")), "类型镜像")
	assert.True(t, mr.Exists(nodeSceneNodeTypeKey(2, "950")))
	assert.False(t, mr.Exists(removedZoneRegistryKeyForTest), "zone 登记集合已删除,刷新不再写它")
}

// 写入按 zone 合并:每个 zone 的分数不逐节点发 ZADD,读也不超过每节点两条 GET。
// 只钉上界,不钉精确形状(批大小、类型镜像怎么写都可以改)。
func TestRefreshLoadScores_LeaderBatchesScoreWritesPerZone(t *testing.T) {
	sc, mr := newLoadRefreshScopeCtx(t)
	nodes := map[uint32][]string{1: {"980", "981", "982"}, 2: {"983", "984", "985"}}
	for zone, ids := range nodes {
		for _, id := range ids {
			registerKnownNodeForTest(t, "SceneNodeService.rpc/scope-"+id, zone, id)
			seedNodeCountersForTest(t, mr, zone, id, "1", "")
		}
	}
	rec := installRedisCommandRecorderForTest(t, mr, nil)

	refreshLoadScores(sc)

	assert.LessOrEqual(t, rec.count("ZADD", ""), len(nodes), "每个 zone 至多一条 ZADD")
	assert.LessOrEqual(t, rec.count("GET", "")+rec.count("SCARD", ""), 2*6, "每个节点至多两条读(场景数 GET + 成员 SCARD)")
	assert.Zero(t, rec.count("SADD", removedZoneRegistryKeyForTest), "zone 登记集合已删除")
	for zone, ids := range nodes {
		for _, id := range ids {
			score, ok := loadScoreForTest(t, mr, zone, id)
			assert.True(t, ok, "zone=%d node=%s 必须在负载集里", zone, id)
			assert.Equal(t, 1.0, score)
		}
	}
}

// 已在负载集里的节点读不到计数:保留旧分,不按 0 写(按 0 写会把满载节点伪装成最空闲的)。
func TestRefreshLoadScores_ReadErrorKeepsPreviousScore(t *testing.T) {
	sc, mr := newLoadRefreshScopeCtx(t)
	registerKnownNodeForTest(t, "SceneNodeService.rpc/scope-960", 1, "960")
	registerKnownNodeForTest(t, "SceneNodeService.rpc/scope-961", 1, "961")
	_, err := mr.ZAdd(nodeLoadKey(1), 100, "960")
	require.NoError(t, err)
	_, err = mr.ZAdd(nodeLoadKey(1), 100, "961")
	require.NoError(t, err)
	seedNodeCountersForTest(t, mr, 1, "960", "4", "")
	seedNodeCountersForTest(t, mr, 1, "961", "7", "")
	failedKey := nodeMembersKey(1, "960")
	installRedisCommandRecorderForTest(t, mr, func(cmd string, args []string) string {
		if cmd == "SCARD" && len(args) > 0 && args[0] == failedKey {
			return "ERR injected counter read failure"
		}
		return ""
	})
	partialBefore := loadRefreshOutcomeCountForTest(t, "partial")

	refreshLoadScores(sc)

	score, ok := loadScoreForTest(t, mr, 1, "960")
	require.True(t, ok, "读失败不得摘掉成员资格")
	assert.Equal(t, 100.0, score, "读失败保留旧分")
	score, ok = loadScoreForTest(t, mr, 1, "961")
	require.True(t, ok)
	assert.Equal(t, 7.0, score, "同批读成功的节点照常更新")
	assert.Equal(t, partialBefore+1, loadRefreshOutcomeCountForTest(t, "partial"))
}

// 计数键的值是空串:与「读失败」同样处理,不当 0。
func TestRefreshLoadScores_EmptyCounterValueKeepsPreviousScore(t *testing.T) {
	sc, mr := newLoadRefreshScopeCtx(t)
	registerKnownNodeForTest(t, "SceneNodeService.rpc/scope-964", 1, "964")
	registerKnownNodeForTest(t, "SceneNodeService.rpc/scope-965", 1, "965")
	_, err := mr.ZAdd(nodeLoadKey(1), 100, "964")
	require.NoError(t, err)
	require.NoError(t, mr.Set(nodeSceneCountKey(1, "964"), ""))
	seedNodeCountersForTest(t, mr, 1, "965", "2", "")
	partialBefore := loadRefreshOutcomeCountForTest(t, "partial")

	refreshLoadScores(sc)

	score, ok := loadScoreForTest(t, mr, 1, "964")
	require.True(t, ok)
	assert.Equal(t, 100.0, score, "空串不是 0:保留旧分")
	score, ok = loadScoreForTest(t, mr, 1, "965")
	require.True(t, ok)
	assert.Equal(t, 2.0, score)
	assert.Equal(t, partialBefore+1, loadRefreshOutcomeCountForTest(t, "partial"))
}

// 不在负载集里的节点读不到计数:照样加进负载集(成员资格是存活信号,扣下它会让活节点被判死),
// 但分数排在读成功的节点之后,选点最后才轮到它。
func TestRefreshLoadScores_NonMemberReadFailureJoinsLast(t *testing.T) {
	sc, mr := newLoadRefreshScopeCtx(t)
	registerKnownNodeForTest(t, "SceneNodeService.rpc/scope-962", 1, "962")
	registerKnownNodeForTest(t, "SceneNodeService.rpc/scope-963", 1, "963")
	seedNodeCountersForTest(t, mr, 1, "963", "3", "")
	failedKey := nodeSceneCountKey(1, "962")
	installRedisCommandRecorderForTest(t, mr, func(cmd string, args []string) string {
		if cmd == "GET" && len(args) > 0 && args[0] == failedKey {
			return "ERR injected counter read failure"
		}
		return ""
	})

	refreshLoadScores(sc)

	unknownScore, ok := loadScoreForTest(t, mr, 1, "962")
	require.True(t, ok, "读失败的非成员也必须加入负载集,否则 IsNodeAlive 会判它已死")
	knownScore, ok := loadScoreForTest(t, mr, 1, "963")
	require.True(t, ok)
	assert.Equal(t, 3.0, knownScore)
	assert.Greater(t, unknownScore, knownScore, "负载未知的节点排在最后")
	assert.True(t, IsNodeAlive(sc, 1, "962"))

	best, err := GetBestNodeForPurpose(context.Background(), sc, 1, constants.NodePurposeWorld)
	require.NoError(t, err)
	assert.Equal(t, "963", best, "选点不能先挑负载未知的节点")
}

// ── 清扫与孤儿清理:只处理本 zone,不 SCAN ──────────────────────────────────

// 本 zone 一个节点都没有(最后一个节点在无领导窗口里死掉)照样扫;同组别的 zone 的负载集在同一个实例里也不碰。
func TestSweepStaleLoadSetMembers_OnlyOwnZoneNoScan(t *testing.T) {
	sc, mr := newLoadRefreshScopeCtx(t)
	_, err := mr.ZAdd(nodeLoadKey(testZoneId), 0, "9301")
	require.NoError(t, err)
	_, err = mr.ZAdd(nodeLoadKey(93), 0, "9302")
	require.NoError(t, err)
	rec := installRedisCommandRecorderForTest(t, mr, func(cmd string, _ []string) string {
		if cmd == "SCAN" {
			return "ERR injected: SCAN must not be used"
		}
		return ""
	})
	seen := map[uint32]map[string]struct{}{}

	require.True(t, sweepStaleLoadSetMembers(sc, seen, readLoadSetBaseline(sc, nodeViewZones(sc)), nil))

	assert.True(t, mr.Exists(nodeDeathAtKey(testZoneId, "9301")), "本 zone 的陈旧成员要先补记死亡时刻")
	assert.NotContains(t, loadSetMembersForTest(t, mr, testZoneId), "9301")
	assert.Contains(t, loadSetMembersForTest(t, mr, 93), "9302", "别的 zone 由它自己的领导者清扫")
	assert.False(t, mr.Exists(nodeDeathAtKey(93, "9302")))
	assert.Zero(t, rec.count("SCAN", ""), "不得全库 SCAN")
	assert.Zero(t, rec.count("SMEMBERS", ""), "不读已删除的 zone 登记集合")
}

// 孤儿频道组靠本 zone 期望数 Hash 的字段发现,一条 SCAN 都不发;同组别的 zone 的孤儿组不碰。
func TestCleanupOrphanWorldChannels_OwnZoneOnlyWithoutScan(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	resetNodeDeathQueuesForTest(t)
	sc, mr := newAutoscaleCtx(t)
	rec := installRedisCommandRecorderForTest(t, mr, func(cmd string, _ []string) string {
		if cmd == "SCAN" {
			return "ERR injected: SCAN must not be used"
		}
		return ""
	})
	const zone, otherZone, deadConf = uint32(95), uint32(99), uint64(777) // deadConf 不在 World 表里(newAutoscaleCtx 只登记了 autoscaleTestConfID)
	// 本进程是 zone 95 的 scene_manager。
	sc.Config.ZoneId = zone
	seedChannels(t, sc, zone, deadConf, map[uint64]int64{9501: 0})
	seedChannels(t, sc, otherZone, deadConf, map[uint64]int64{9901: 0})

	assert.Equal(t, 1, CleanupOrphanWorldChannels(context.Background(), sc))

	assert.False(t, mr.Exists(worldChannelsKey(zone, deadConf)))
	assert.Equal(t, "", mr.HGet(worldDesiredChannelsKey(zone), "777"), "期望数字段一并删除")
	assert.True(t, mr.Exists(worldChannelsKey(otherZone, deadConf)), "别的 zone 的孤儿组由它自己的领导者清")
	assert.Zero(t, rec.count("SCAN", ""), "不得全库 SCAN")
}

// 期望数字段是孤儿清理的发现索引,必须在频道集合删掉之后才删:中途失败时下一轮还能找到这组孤儿。
func TestDeleteOrphanChannel_KeepsDesiredFieldUntilSetDeleted(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	resetNodeDeathQueuesForTest(t)
	sc, mr := newAutoscaleCtx(t)
	const zone, deadConf = uint32(96), uint64(778)
	sc.Config.ZoneId = zone
	setKey := worldChannelsKey(zone, deadConf)
	var failSetDel atomic.Bool
	failSetDel.Store(true)
	installRedisCommandRecorderForTest(t, mr, func(cmd string, args []string) string {
		if cmd == "DEL" && slices.Contains(args, setKey) && failSetDel.Load() {
			return "ERR injected channel set delete failure"
		}
		return ""
	})
	seedChannels(t, sc, zone, deadConf, map[uint64]int64{9601: 0})

	assert.Equal(t, 0, CleanupOrphanWorldChannels(context.Background(), sc))
	assert.Equal(t, "1", mr.HGet(worldDesiredChannelsKey(zone), "778"), "集合没删掉之前期望数字段必须保留")

	failSetDel.Store(false)
	assert.Equal(t, 1, CleanupOrphanWorldChannels(context.Background(), sc), "下一轮重新发现并清掉")
	assert.False(t, mr.Exists(setKey))
	assert.Equal(t, "", mr.HGet(worldDesiredChannelsKey(zone), "778"))
}

// 播种 / 写回期望频道数不再登记 zone(登记集合 2026-11-05 删除)。
func TestDesiredWorldChannelCount_DoesNotWriteRemovedZoneRegistry(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	const conf = uint64(1)
	rec := installRedisCommandRecorderForTest(t, mr, nil)

	mustDesiredWorldChannelCount(t, sc, testZoneId, conf)
	setDesiredWorldChannelCount(sc, testZoneId, conf, 2)
	assert.Equal(t, 2, mustDesiredWorldChannelCount(t, sc, testZoneId, conf))
	assert.Zero(t, rec.count("SADD", ""), "不写 zone 登记集合")
	assert.False(t, mr.Exists(removedZoneRegistryKeyForTest))
}
