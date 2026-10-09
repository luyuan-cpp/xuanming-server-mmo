package logic

// 分线目录(线号分配 + 状态判定 + 发布)的单元测试,对应 docs/design/world-channel-switch.md §4.2 / §4.3。
//
// 这里能验证的是 scene_manager 这一侧的契约:线号表的内容、写进 Redis 的目录字节解析出来是什么。
// 「scene 节点读到目录并推给客户端」在 C++ 侧,要靠联调验证;两侧唯一的共同约定是键名格式与
// SceneChannelDirectory 这个 proto,前者由文件末尾的字面量断言钉住。
//
// 时间:updated_at_ms 由调用方传入;TTL 用 miniredis 记录的值断言,不读墙钟。

import (
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	scenepb "proto/scene"

	"scene_manager/internal/config"
	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gproto "google.golang.org/protobuf/proto"
)

const directoryTestConfID = uint64(1)

// 线号用例刻意选 > 2^53 的 scene_id(真实 snowflake 就在这个量级),并且包含两组陷阱:
//   - lineSceneA 与 lineSceneB 只差 1,转成 double 是同一个数 —— 脚本若把 scene_id 当 number 用,
//     两条线会被当成一条;
//   - lineSceneD 比 lineSceneC 多一位,字符串序里 "1000…" 排在 "9223…" 前面,数值序相反 ——
//     排序若在 Lua 里按字符串做,存量线补号的顺序就错了。
//
// 数值升序:E < A < B < C < D。
const (
	lineSceneA = uint64(9007199254740992)     // 2^53
	lineSceneB = uint64(9007199254740993)     // 2^53 + 1
	lineSceneC = uint64(9223372036854775809)  // 2^63 + 1
	lineSceneD = uint64(10000000000000000000) // 10^19,20 位
	lineSceneE = uint64(9007199254740000)     // 比 A 小,用来验证「后来的线不按 id 插队」
)

// directoryTestNow 是用例里固定的发布时刻。
var directoryTestNow = time.UnixMilli(1757000000123)

// newChannelDirectoryCtx 造一个把 directoryTestConfID 登记成大世界地图、节点 "10" 存活的 svcCtx。
func newChannelDirectoryCtx(t *testing.T) (*svc.ServiceContext, *miniredis.Miniredis) {
	t.Helper()
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	t.Cleanup(SetWorldConfIdsForTest([]uint64{directoryTestConfID}))
	_, err := mr.ZAdd(nodeLoadKey(testZoneId), 0, "10")
	require.NoError(t, err)
	return sc, mr
}

// seedDirectoryChannel 摆一条在役的线:集合成员、节点映射、人数。nodeID 为空表示不摆映射。
func seedDirectoryChannel(t *testing.T, mr *miniredis.Miniredis, sceneID uint64, nodeID string, players int64) {
	t.Helper()
	_, err := mr.SAdd(worldChannelsKey(testZoneId, directoryTestConfID), strconv.FormatUint(sceneID, 10))
	require.NoError(t, err)
	if nodeID != "" {
		require.NoError(t, mr.Set(sceneNodeKey(sceneID), nodeID))
	}
	require.NoError(t, mr.Set(fmt.Sprintf(InstancePlayerCountKey, sceneID), strconv.FormatInt(players, 10)))
}

// moveChannelToDraining 模拟 autoscaler 开始排空一条线:从在役集合摘掉,放进回收中集合。
func moveChannelToDraining(t *testing.T, mr *miniredis.Miniredis, sceneID uint64) {
	t.Helper()
	member := strconv.FormatUint(sceneID, 10)
	_, err := mr.SRem(worldChannelsKey(testZoneId, directoryTestConfID), member)
	require.NoError(t, err)
	_, err = mr.SAdd(worldDrainingSetKey(testZoneId, directoryTestConfID), member)
	require.NoError(t, err)
}

// readPublishedDirectory 读出并解析已发布的目录;键不存在直接失败。
func readPublishedDirectory(t *testing.T, mr *miniredis.Miniredis) *scenepb.SceneChannelDirectory {
	t.Helper()
	raw, err := mr.Get(worldChannelDirectoryKey(testZoneId, directoryTestConfID))
	require.NoError(t, err, "目录键应当存在")
	directory := &scenepb.SceneChannelDirectory{}
	require.NoError(t, gproto.Unmarshal([]byte(raw), directory))
	return directory
}

func lineNoTable(t *testing.T, sc *svc.ServiceContext) map[string]string {
	t.Helper()
	table, err := sc.Redis.Hgetall(worldChannelLineNoKey(testZoneId, directoryTestConfID))
	require.NoError(t, err)
	return table
}

func sceneKey(sceneID uint64) string { return strconv.FormatUint(sceneID, 10) }

// ── 线号分配 ─────────────────────────────────────────────────────────────

// 存量线首次补号:按 scene_id **数值**升序编成 1..N,与输入顺序、字符串序无关。
func TestWorldChannelLineNumbers_BackfillFollowsNumericSceneIDOrder(t *testing.T) {
	sc, _ := newChannelDirectoryCtx(t)

	got, err := allocateWorldChannelLineNumbers(sc, testZoneId, directoryTestConfID,
		[]uint64{lineSceneD, lineSceneB, lineSceneC, lineSceneA})
	require.NoError(t, err)
	assert.Equal(t, map[uint64]uint32{lineSceneA: 1, lineSceneB: 2, lineSceneC: 3, lineSceneD: 4}, got)

	// 落进 Redis 的表与返回值一致,而且 A / B 这两个 double 相同的 id 各占一行。
	assert.Equal(t, map[string]string{
		sceneKey(lineSceneA): "1",
		sceneKey(lineSceneB): "2",
		sceneKey(lineSceneC): "3",
		sceneKey(lineSceneD): "4",
	}, lineNoTable(t, sc))
}

// 线号一经分配就不再变:重复运行、输入乱序、输入里有重复与 0 都不改变结果。
func TestWorldChannelLineNumbers_StableAcrossRuns(t *testing.T) {
	sc, _ := newChannelDirectoryCtx(t)
	want := map[uint64]uint32{lineSceneA: 1, lineSceneB: 2, lineSceneC: 3}

	first, err := allocateWorldChannelLineNumbers(sc, testZoneId, directoryTestConfID,
		[]uint64{lineSceneA, lineSceneB, lineSceneC})
	require.NoError(t, err)
	require.Equal(t, want, first)

	again, err := allocateWorldChannelLineNumbers(sc, testZoneId, directoryTestConfID,
		[]uint64{lineSceneC, 0, lineSceneA, lineSceneB, lineSceneA})
	require.NoError(t, err)
	assert.Equal(t, want, again)
	assert.Len(t, lineNoTable(t, sc), 3)
}

// 线彻底消失后号被释放;之后出现的新线拿最小的空闲号,不按自己的 scene_id 插队,
// 也不挤动还在的线。
func TestWorldChannelLineNumbers_ReleasedNumberIsReusedBySmallestFree(t *testing.T) {
	sc, _ := newChannelDirectoryCtx(t)

	_, err := allocateWorldChannelLineNumbers(sc, testZoneId, directoryTestConfID,
		[]uint64{lineSceneA, lineSceneB, lineSceneC})
	require.NoError(t, err)

	// B(2 线)彻底销毁:它的号从表里摘掉,A / C 不动。
	afterGone, err := allocateWorldChannelLineNumbers(sc, testZoneId, directoryTestConfID,
		[]uint64{lineSceneA, lineSceneC})
	require.NoError(t, err)
	assert.Equal(t, map[uint64]uint32{lineSceneA: 1, lineSceneC: 3}, afterGone)
	assert.NotContains(t, lineNoTable(t, sc), sceneKey(lineSceneB), "销毁的线必须从线号表里摘掉")

	// 新线 D 复用空出来的 2。
	withD, err := allocateWorldChannelLineNumbers(sc, testZoneId, directoryTestConfID,
		[]uint64{lineSceneA, lineSceneC, lineSceneD})
	require.NoError(t, err)
	assert.Equal(t, map[uint64]uint32{lineSceneA: 1, lineSceneC: 3, lineSceneD: 2}, withD)

	// 再来一条 id 比谁都小的线 E:1..3 都占着,它拿 4,已有的线一个都不动
	// (「3线」不能因为别的线出现就悄悄变成「4线」)。
	withE, err := allocateWorldChannelLineNumbers(sc, testZoneId, directoryTestConfID,
		[]uint64{lineSceneE, lineSceneA, lineSceneC, lineSceneD})
	require.NoError(t, err)
	assert.Equal(t, map[uint64]uint32{lineSceneA: 1, lineSceneC: 3, lineSceneD: 2, lineSceneE: 4}, withE)
}

// 输入为空不清表:Redis 抖动时 SMEMBERS 可能读空,清表会让恢复后的线号全部重排。
// Go 侧不执行脚本;脚本自己对空 ARGV 也什么都不做(第二道保险)。
func TestWorldChannelLineNumbers_EmptyInputDoesNotClearTable(t *testing.T) {
	sc, _ := newChannelDirectoryCtx(t)

	_, err := allocateWorldChannelLineNumbers(sc, testZoneId, directoryTestConfID, []uint64{lineSceneA, lineSceneB})
	require.NoError(t, err)
	before := lineNoTable(t, sc)
	require.Len(t, before, 2)

	for _, empty := range [][]uint64{nil, {}, {0}} {
		got, allocErr := allocateWorldChannelLineNumbers(sc, testZoneId, directoryTestConfID, empty)
		require.NoError(t, allocErr)
		assert.Empty(t, got)
		assert.Equal(t, before, lineNoTable(t, sc), "空输入不得改动线号表")
	}

	_, err = sc.Redis.Eval(luaAllocWorldChannelLineNumbers,
		[]string{worldChannelLineNoKey(testZoneId, directoryTestConfID)})
	require.NoError(t, err)
	assert.Equal(t, before, lineNoTable(t, sc), "脚本对空 ARGV 也不得清表")
}

// 表里的坏值(非数字 / 0 / 重号)与不在输入里的残留成员:坏的重发、残留的删掉,
// 结果仍是一张「每条线一个互不相同的正整数」的表。不修的话 Go 侧每轮都解析失败,目录永远发不出来。
func TestWorldChannelLineNumbers_CorruptEntriesAreReassigned(t *testing.T) {
	sc, mr := newChannelDirectoryCtx(t)
	key := worldChannelLineNoKey(testZoneId, directoryTestConfID)
	mr.HSet(key,
		sceneKey(lineSceneA), "abc", // 非数字
		sceneKey(lineSceneB), "2",
		sceneKey(lineSceneC), "2", // 与 B 重号
		sceneKey(lineSceneD), "0", // 线号从 1 起
		"424242", "1", // 早已不存在的线
	)

	got, err := allocateWorldChannelLineNumbers(sc, testZoneId, directoryTestConfID,
		[]uint64{lineSceneA, lineSceneB, lineSceneC, lineSceneD})
	require.NoError(t, err)
	require.Len(t, got, 4)

	numbers := make([]uint32, 0, len(got))
	for _, lineNo := range got {
		numbers = append(numbers, lineNo)
	}
	assert.ElementsMatch(t, []uint32{1, 2, 3, 4}, numbers, "修复后必须是 1..4 各一次")
	// B / C 重号时谁保住 2 取决于 HGETALL 的遍历顺序,不断言;但 2 必须仍在这两条线之一手里。
	assert.True(t, got[lineSceneB] == 2 || got[lineSceneC] == 2)

	table := lineNoTable(t, sc)
	assert.Len(t, table, 4)
	assert.NotContains(t, table, "424242")
	for sceneID, lineNo := range got {
		assert.Equal(t, strconv.FormatUint(uint64(lineNo), 10), table[sceneKey(sceneID)])
	}
}

// 脚本返回值的任何异常都必须变成 error(本轮不发布),不能悄悄给某条线 0 号或错号。
func TestParseWorldChannelLineNumbers_RejectsMalformedReplies(t *testing.T) {
	ids := []uint64{lineSceneA, lineSceneB}
	a, b := sceneKey(lineSceneA), sceneKey(lineSceneB)

	valid, err := parseWorldChannelLineNumbers([]any{a, "1", b, "2"}, ids)
	require.NoError(t, err)
	assert.Equal(t, map[uint64]uint32{lineSceneA: 1, lineSceneB: 2}, valid)

	// go-redis 在某些传输下把 bulk 回成 []byte、把整数回成 int64,解析要容忍。
	mixed, err := parseWorldChannelLineNumbers([]any{[]byte(a), int64(7), b, "2"}, ids)
	require.NoError(t, err)
	assert.Equal(t, map[uint64]uint32{lineSceneA: 7, lineSceneB: 2}, mixed)

	cases := []struct {
		name string
		raw  any
	}{
		{"不是数组", "OK"},
		{"nil", nil},
		{"元素个数不对", []any{a, "1"}},
		{"元素个数为奇数", []any{a, "1", b}},
		{"scene_id 非数字", []any{"x", "1", b, "2"}},
		{"scene_id 不在输入里", []any{a, "1", "12345", "2"}},
		{"scene_id 重复", []any{a, "1", a, "2"}},
		{"线号为 0", []any{a, "0", b, "2"}},
		{"线号非数字", []any{a, "one", b, "2"}},
		{"线号为负", []any{a, "-1", b, "2"}},
		{"线号超出 uint32", []any{a, "4294967296", b, "2"}},
		{"线号重复", []any{a, "1", b, "1"}},
		{"线号为空", []any{a, nil, b, "2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, parseErr := parseWorldChannelLineNumbers(tc.raw, ids)
			assert.Error(t, parseErr)
			assert.Nil(t, got)
		})
	}
}

// ── 状态判定 ─────────────────────────────────────────────────────────────

// 设计文档 §4.3 的状态表逐行,外加边界:人数恰等于上限、恰等于繁忙线、上限为 0。
func TestWorldChannelState_TableAndBoundaries(t *testing.T) {
	const (
		smooth      = scenepb.SceneChannelState_SCENE_CHANNEL_STATE_SMOOTH
		busy        = scenepb.SceneChannelState_SCENE_CHANNEL_STATE_BUSY
		full        = scenepb.SceneChannelState_SCENE_CHANNEL_STATE_FULL
		closing     = scenepb.SceneChannelState_SCENE_CHANNEL_STATE_CLOSING
		unavailable = scenepb.SceneChannelState_SCENE_CHANNEL_STATE_UNAVAILABLE
	)
	cases := []struct {
		name        string
		closing     bool
		nodeUsable  bool
		players     int64
		maxPlayers  int64
		busyPercent int64
		want        scenepb.SceneChannelState
	}{
		// 第 1 行:回收中压过其它一切。
		{"回收中_节点可用_空线", true, true, 0, 100, 60, closing},
		{"回收中_节点不可用", true, false, 0, 100, 60, closing},
		{"回收中_已满", true, true, 100, 100, 60, closing},
		// 第 2 行:节点不可用压过人数。
		{"节点不可用_空线", false, false, 0, 100, 60, unavailable},
		{"节点不可用_已满", false, false, 500, 100, 60, unavailable},
		{"节点不可用_不设上限", false, false, 5, 0, 60, unavailable},
		// 第 3 行:人数 ≥ 上限。
		{"人数恰等于上限", false, true, 100, 100, 60, full},
		{"人数超过上限", false, true, 101, 100, 60, full},
		{"人数比上限少1", false, true, 99, 100, 60, busy},
		// 第 4 行:人数 × 100 ≥ 上限 × 繁忙百分比。
		{"人数恰等于繁忙线", false, true, 60, 100, 60, busy},
		{"人数比繁忙线少1", false, true, 59, 100, 60, smooth},
		{"繁忙线不是整数_够到", false, true, 3, 5, 60, busy},
		{"繁忙线不是整数_没够到", false, true, 2, 5, 60, smooth},
		{"繁忙百分比100_差1人满", false, true, 99, 100, 100, smooth},
		{"繁忙百分比1_有1人", false, true, 1, 100, 1, busy},
		{"繁忙百分比1_空线", false, true, 0, 100, 1, smooth},
		{"上限为1_空线", false, true, 0, 1, 60, smooth},
		{"上限为1_有1人", false, true, 1, 1, 60, full},
		// 第 5 行:其它。上限 ≤ 0 = 不设上限,永远不会满也不会繁忙。
		{"空线", false, true, 0, 100, 60, smooth},
		{"上限为0_人很多", false, true, 1 << 40, 0, 60, smooth},
		{"上限为负_人很多", false, true, 9999, -1, 60, smooth},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want,
				worldChannelState(tc.closing, tc.nodeUsable, tc.players, tc.maxPlayers, tc.busyPercent))
		})
	}
}

// 生效配置的最大取值下乘法不溢出:上限钳到 uint32 上限、百分比 ≤ 100 时判定仍然正确
// (溢出的话 players×100 会翻成负数,差一人满的线会被判成流畅)。
func TestWorldChannelState_NoOverflowAtClampedLimit(t *testing.T) {
	cfg := config.Config{}
	cfg.ChannelSwitch.MaxPlayersPerChannel = 1 << 62
	maxPlayers := cfg.ChannelMaxPlayers()
	require.Equal(t, int64(1<<32-1), maxPlayers)

	assert.Equal(t, scenepb.SceneChannelState_SCENE_CHANNEL_STATE_FULL,
		worldChannelState(false, true, maxPlayers, maxPlayers, 100))
	assert.Equal(t, scenepb.SceneChannelState_SCENE_CHANNEL_STATE_BUSY,
		worldChannelState(false, true, maxPlayers-1, maxPlayers, 99))
	assert.Equal(t, scenepb.SceneChannelState_SCENE_CHANNEL_STATE_SMOOTH,
		worldChannelState(false, true, maxPlayers-1, maxPlayers, 100))
	assert.Equal(t, scenepb.SceneChannelState_SCENE_CHANNEL_STATE_SMOOTH,
		worldChannelState(false, true, 0, maxPlayers, 1))
}

// 目录 TTL = 3 × 发布周期 + 一个选主竞选间隔(锁 TTL / 3,向上取整到秒)。
func TestWorldChannelDirectoryTTLSeconds(t *testing.T) {
	const day = 24 * 60 * 60
	cases := []struct {
		refreshSeconds       int64
		leaderLockTTLSeconds int64
		want                 int
	}{
		{2, 30, 16},             // 默认:3×2 + 30/3
		{0, 30, 10},             // 不发布时只剩竞选间隔;函数本身不依赖调用方先判过
		{1, 30, 13},             // 3×1 + 10
		{30, 30, 100},           // 3×30 + 10
		{2, 60, 26},             // 锁 TTL 调大,竞选间隔跟着变长:3×2 + 20
		{2, 24, 14},             // 3×2 + 8
		{2, 25, 15},             // 25/3 = 8.33 → 向上取整 9
		{2, 31, 17},             // 31/3 = 10.33 → 11
		{2, 1, 7},               // 不足 3 秒也按 1 秒算
		{2, 0, 6},               // 锁 TTL 非正:不加(调用方传的是 EffectiveLeaderLockTTLSeconds,恒为正)
		{2, -5, 6},              // 同上
		{2, 1 << 40, 6 + day},   // 竞选间隔钳到一天
		{day, 1 << 40, 4 * day}, // 两项都到上限:4 天,换算成 Redis EX 不溢出
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, worldChannelDirectoryTTLSeconds(tc.refreshSeconds, tc.leaderLockTTLSeconds),
			"refresh=%d leader_lock_ttl=%d", tc.refreshSeconds, tc.leaderLockTTLSeconds)
	}
}

// 目录 TTL 的设计意图:盖住领导者优雅让位时最长的无发布间隔(让位前最后一次发布 ≤ 1 个周期
// + 接任者等竞选 ≤ 1 个竞选间隔 + 当选后等到下一个 tick ≤ 1 个周期),并留有余量。
// 这条用例钉住「TTL 严格大于那个间隔」:换成与选主参数无关的固定下限(比如 10s)就会在这里失败 ——
// 默认值下那个间隔是 2 + 10 + 2 = 14s。
func TestWorldChannelDirectoryTTLSeconds_CoversGracefulLeaderHandover(t *testing.T) {
	for _, refreshSeconds := range []int64{1, 2, 5, 30} {
		for _, leaderLockTTLSeconds := range []int64{24, 25, 30, 60} {
			campaignCeilSeconds := (leaderLockTTLSeconds + 2) / 3
			longestGapSeconds := 2*refreshSeconds + campaignCeilSeconds
			assert.Greater(t, int64(worldChannelDirectoryTTLSeconds(refreshSeconds, leaderLockTTLSeconds)), longestGapSeconds,
				"refresh=%d leader_lock_ttl=%d", refreshSeconds, leaderLockTTLSeconds)
		}
	}
}

// 选主锁 TTL 的生效值:选主接线(scene_manager_service.go)与目录 TTL 共用这一个取值,≤ 0 取默认 30。
func TestConfig_EffectiveLeaderLockTTLSeconds(t *testing.T) {
	cases := []struct{ configured, want int64 }{
		{0, 30},  // 缺省 → 默认
		{-1, 30}, // 非正 → 默认
		{24, 24}, // 正数原样
		{30, 30}, // 正数原样
		{90, 90}, // 正数原样
	}
	for _, tc := range cases {
		cfg := config.Config{LeaderLockTTLSeconds: tc.configured}
		assert.Equal(t, tc.want, cfg.EffectiveLeaderLockTTLSeconds(), "LeaderLockTTLSeconds=%d", tc.configured)
	}
}

// ── 发布 ─────────────────────────────────────────────────────────────────

// 发布一轮:目录键存在且带 TTL,解析出来的字段、线号顺序、各线状态都对;
// 回收中的线留在目录里、保留线号、标 CLOSING;节点不存活 / 映射缺失的线标 UNAVAILABLE。
func TestPublishWorldChannelDirectory_WritesSortedDirectoryWithStates(t *testing.T) {
	sc, mr := newChannelDirectoryCtx(t)
	sc.Config.ChannelSwitch = config.ChannelSwitchConfig{
		CooldownSeconds:      7,
		MaxPlayersPerChannel: 100,
		BusyPercent:          60,
	}
	ctx := context.Background()

	const lineSceneF = uint64(10000000000000000001) // 在役集合里有它,但没有节点映射(僵尸成员)
	seedDirectoryChannel(t, mr, lineSceneE, "10", 3)
	seedDirectoryChannel(t, mr, lineSceneA, "10", 10)
	seedDirectoryChannel(t, mr, lineSceneB, "10", 60)
	seedDirectoryChannel(t, mr, lineSceneC, "10", 100)
	seedDirectoryChannel(t, mr, lineSceneD, "99", 5) // 节点 99 不在负载集里
	seedDirectoryChannel(t, mr, lineSceneF, "", 0)

	// 第一轮:全部在役。线号按 scene_id 数值升序:E1 A2 B3 C4 D5 F6。
	require.Equal(t, 1, PublishWorldChannelDirectoriesForZone(ctx, sc, testZoneId, directoryTestNow))

	// 第二轮之前 E 开始排空。
	moveChannelToDraining(t, mr, lineSceneE)
	later := directoryTestNow.Add(2 * time.Second)
	require.Equal(t, 1, PublishWorldChannelDirectoriesForZone(ctx, sc, testZoneId, later))

	dirKey := worldChannelDirectoryKey(testZoneId, directoryTestConfID)
	assert.Equal(t, 16*time.Second, mr.TTL(dirKey), "默认 2s 周期、默认 30s 选主锁:TTL = 3×2 + 30/3 秒")

	directory := readPublishedDirectory(t, mr)
	assert.Equal(t, testZoneId, directory.GetZoneId())
	assert.Equal(t, uint32(directoryTestConfID), directory.GetSceneConfigId())
	assert.Equal(t, uint64(later.UnixMilli()), directory.GetUpdatedAtMs())
	assert.Equal(t, uint32(7), directory.GetSwitchCooldownSeconds())
	assert.Equal(t, uint32(100), directory.GetMaxPlayersPerChannel())
	assert.True(t, directory.GetSwitchEnabled())

	type line struct {
		sceneID uint64
		no      uint32
		players uint32
		state   scenepb.SceneChannelState
	}
	got := make([]line, 0, len(directory.GetChannels()))
	for _, ch := range directory.GetChannels() {
		got = append(got, line{ch.GetSceneId(), ch.GetChannelNo(), ch.GetPlayerCount(), ch.GetState()})
	}
	// 断言的是切片本身:顺序即目录里的顺序,必须按线号升序。
	assert.Equal(t, []line{
		{lineSceneE, 1, 3, scenepb.SceneChannelState_SCENE_CHANNEL_STATE_CLOSING},
		{lineSceneA, 2, 10, scenepb.SceneChannelState_SCENE_CHANNEL_STATE_SMOOTH},
		{lineSceneB, 3, 60, scenepb.SceneChannelState_SCENE_CHANNEL_STATE_BUSY},
		{lineSceneC, 4, 100, scenepb.SceneChannelState_SCENE_CHANNEL_STATE_FULL},
		{lineSceneD, 5, 5, scenepb.SceneChannelState_SCENE_CHANNEL_STATE_UNAVAILABLE},
		{lineSceneF, 6, 0, scenepb.SceneChannelState_SCENE_CHANNEL_STATE_UNAVAILABLE},
	}, got)
}

// 回收中的线彻底销毁之后,它的号被释放并被之后新建的线复用;目录里不再有它。
func TestPublishWorldChannelDirectory_DrainedLineReleasesItsNumber(t *testing.T) {
	sc, mr := newChannelDirectoryCtx(t)
	ctx := context.Background()
	seedDirectoryChannel(t, mr, lineSceneA, "10", 1)
	seedDirectoryChannel(t, mr, lineSceneB, "10", 2)
	seedDirectoryChannel(t, mr, lineSceneC, "10", 3)
	require.Equal(t, 1, PublishWorldChannelDirectoriesForZone(ctx, sc, testZoneId, directoryTestNow))

	// B 排空中:仍占着 2 线。
	moveChannelToDraining(t, mr, lineSceneB)
	require.Equal(t, 1, PublishWorldChannelDirectoriesForZone(ctx, sc, testZoneId, directoryTestNow))
	assert.Equal(t, "2", lineNoTable(t, sc)[sceneKey(lineSceneB)])

	// B 走空并销毁(finishDrainedChannel:摘出回收中集合、删映射与人数),随后扩出新线 D。
	_, err := mr.SRem(worldDrainingSetKey(testZoneId, directoryTestConfID), sceneKey(lineSceneB))
	require.NoError(t, err)
	mr.Del(sceneNodeKey(lineSceneB))
	mr.Del(fmt.Sprintf(InstancePlayerCountKey, lineSceneB))
	seedDirectoryChannel(t, mr, lineSceneD, "10", 0)
	require.Equal(t, 1, PublishWorldChannelDirectoriesForZone(ctx, sc, testZoneId, directoryTestNow))

	directory := readPublishedDirectory(t, mr)
	require.Len(t, directory.GetChannels(), 3)
	assert.Equal(t, []uint64{lineSceneA, lineSceneD, lineSceneC}, []uint64{
		directory.GetChannels()[0].GetSceneId(),
		directory.GetChannels()[1].GetSceneId(),
		directory.GetChannels()[2].GetSceneId(),
	}, "D 复用 B 空出来的 2 线,A / C 的线号不变")
	assert.Equal(t, uint32(2), directory.GetChannels()[1].GetChannelNo())
}

// 线恰好在发布的两次 SMEMBERS 之间从在役移到回收中(一步完成的原子移动):它不能从目录里消失,线号也不能变。
// 钉住的是读的顺序 —— 发布先读在役(移走之前,读得到)、再读回收中(移过去之后,也读得到),两边都有时按
// 回收中处理;先读回收中、再读在役就两头落空,线号被释放,下一轮拿到更小的空号。
//
// 用例模拟的是原子移动。现状下 beginDrainWorldChannel 是 SREM 与 SADD 两步,两次读都落在两步之间时仍会
// 丢线丢号(luaAllocWorldChannelLineNumbers 的「已知残余」),那要到 world_autoscale.go 里才能根治。
// 本用例用 SetPreHook,不能与 mr.SetError 同时用(两者共用同一个钩子槽位)。
func TestPublishWorldChannelDirectory_MoveToDrainingBetweenTheTwoReadsKeepsLineAndNumber(t *testing.T) {
	sc, mr := newChannelDirectoryCtx(t)
	ctx := context.Background()
	activeKey := worldChannelsKey(testZoneId, directoryTestConfID)
	drainingKey := worldDrainingSetKey(testZoneId, directoryTestConfID)
	seedDirectoryChannel(t, mr, lineSceneA, "10", 1)
	seedDirectoryChannel(t, mr, lineSceneB, "10", 2)
	seedDirectoryChannel(t, mr, lineSceneC, "10", 3)
	require.Equal(t, 1, PublishWorldChannelDirectoriesForZone(ctx, sc, testZoneId, directoryTestNow))

	// A(1 线)彻底销毁,腾出 1 号:此后 C 只要丢过一次号,重发时就会从 3 变成 1。
	_, err := mr.SRem(activeKey, sceneKey(lineSceneA))
	require.NoError(t, err)
	mr.Del(sceneNodeKey(lineSceneA))
	mr.Del(fmt.Sprintf(InstancePlayerCountKey, lineSceneA))
	require.Equal(t, 1, PublishWorldChannelDirectoriesForZone(ctx, sc, testZoneId, directoryTestNow))
	wantLineNos := map[string]string{sceneKey(lineSceneB): "2", sceneKey(lineSceneC): "3"}
	require.Equal(t, wantLineNos, lineNoTable(t, sc))

	// 钩子在装上之后的第二条 SMEMBERS(读回收中集合)执行之前,也就是读在役集合已经返回之后,把 C 移过去。
	// 钩子跑在 miniredis 的连接 goroutine 上,不在这里用 require;移没移成由下面直接读集合来断言。
	movingMember := sceneKey(lineSceneC)
	var smembersCalls atomic.Int32
	mr.Server().SetPreHook(func(_ *server.Peer, cmd string, _ ...string) bool {
		if cmd == "SMEMBERS" && smembersCalls.Add(1) == 2 {
			_, _ = mr.SRem(activeKey, movingMember)
			_, _ = mr.SAdd(drainingKey, movingMember)
		}
		return false
	})
	t.Cleanup(func() { mr.Server().SetPreHook(nil) })

	require.Equal(t, 1, PublishWorldChannelDirectoriesForZone(ctx, sc, testZoneId, directoryTestNow))

	// B 还在在役集合里,所以这把集合键仍然存在,可以直接查成员。
	stillActive, err := mr.SIsMember(activeKey, movingMember)
	require.NoError(t, err)
	require.False(t, stillActive, "C 应当已被钩子移出在役集合,否则用例空转")
	nowDraining, err := mr.SIsMember(drainingKey, movingMember)
	require.NoError(t, err)
	require.True(t, nowDraining, "C 应当已被钩子移进回收中集合,否则用例空转")

	assert.Equal(t, wantLineNos, lineNoTable(t, sc), "C 的线号不得被释放")
	directory := readPublishedDirectory(t, mr)
	require.Len(t, directory.GetChannels(), 2, "C 不得从这一轮的目录里消失")
	assert.Equal(t, lineSceneB, directory.GetChannels()[0].GetSceneId())
	assert.Equal(t, uint32(2), directory.GetChannels()[0].GetChannelNo())
	assert.Equal(t, lineSceneC, directory.GetChannels()[1].GetSceneId())
	assert.Equal(t, uint32(3), directory.GetChannels()[1].GetChannelNo())
	assert.Equal(t, scenepb.SceneChannelState_SCENE_CHANNEL_STATE_CLOSING, directory.GetChannels()[1].GetState(),
		"同时读到在役与回收中时按回收中处理")
}

// 切线关闭 / 冷却关闭 / 上限跟随扩容线 / 自定义发布周期与选主锁 TTL,都如实反映在目录里。
// 关闭切线时目录照发(客户端要靠它把整面板置灰,而不是显示「暂不可用」)。
func TestPublishWorldChannelDirectory_ReflectsEffectiveConfig(t *testing.T) {
	sc, mr := newChannelDirectoryCtx(t)
	sc.Config.ChannelSwitch = config.ChannelSwitchConfig{
		Disabled:                true,
		CooldownSeconds:         -1,
		DirectoryRefreshSeconds: 30,
	}
	sc.Config.LeaderLockTTLSeconds = 60
	sc.Config.WorldAutoscale.ScaleOutPlayerThreshold = 300
	seedDirectoryChannel(t, mr, lineSceneA, "10", 299)

	require.Equal(t, 1, PublishWorldChannelDirectoriesForZone(context.Background(), sc, testZoneId, directoryTestNow))

	directory := readPublishedDirectory(t, mr)
	assert.False(t, directory.GetSwitchEnabled(), "Disabled → switch_enabled=false")
	assert.Equal(t, uint32(0), directory.GetSwitchCooldownSeconds(), "负数 = 不设冷却 → 0")
	assert.Equal(t, uint32(300), directory.GetMaxPlayersPerChannel(), "没配上限 → 跟随扩容线")
	require.Len(t, directory.GetChannels(), 1)
	assert.Equal(t, scenepb.SceneChannelState_SCENE_CHANNEL_STATE_BUSY, directory.GetChannels()[0].GetState())
	assert.Equal(t, 110*time.Second, mr.TTL(worldChannelDirectoryKey(testZoneId, directoryTestConfID)),
		"30s 周期、60s 选主锁:TTL = 3 × 30 + 60 / 3 秒")
}

// 人数键写坏 / 为负 / 不存在都按 0 展示,不影响发布。
func TestPublishWorldChannelDirectory_BadPlayerCountShowsZero(t *testing.T) {
	sc, mr := newChannelDirectoryCtx(t)
	seedDirectoryChannel(t, mr, lineSceneA, "10", -5)
	seedDirectoryChannel(t, mr, lineSceneB, "10", 0)
	require.NoError(t, mr.Set(fmt.Sprintf(InstancePlayerCountKey, lineSceneB), "not-a-number"))
	seedDirectoryChannel(t, mr, lineSceneC, "10", 0)
	mr.Del(fmt.Sprintf(InstancePlayerCountKey, lineSceneC))

	require.Equal(t, 1, PublishWorldChannelDirectoriesForZone(context.Background(), sc, testZoneId, directoryTestNow))

	directory := readPublishedDirectory(t, mr)
	require.Len(t, directory.GetChannels(), 3)
	for _, ch := range directory.GetChannels() {
		assert.Equal(t, uint32(0), ch.GetPlayerCount(), "scene=%d", ch.GetSceneId())
		assert.Equal(t, scenepb.SceneChannelState_SCENE_CHANNEL_STATE_SMOOTH, ch.GetState(), "scene=%d", ch.GetSceneId())
	}
}

// 非领导者不发布:既不写目录,也不碰线号表。
func TestPublishWorldChannelDirectory_FollowerDoesNotPublish(t *testing.T) {
	sc, mr := newChannelDirectoryCtx(t)
	seedDirectoryChannel(t, mr, lineSceneA, "10", 1)
	t.Cleanup(SetLeaderCheckForTest(func() bool { return false }))

	assert.Equal(t, 0, PublishWorldChannelDirectoriesForZone(context.Background(), sc, testZoneId, directoryTestNow))
	assert.False(t, mr.Exists(worldChannelDirectoryKey(testZoneId, directoryTestConfID)))
	assert.False(t, mr.Exists(worldChannelLineNoKey(testZoneId, directoryTestConfID)))
}

// 一条线都没有:不发布,也不清线号表(读空与「线真的没了」在这一层分不开,不能据此清表)。
// 已有的旧目录不被覆盖,靠 TTL 自然过期。
func TestPublishWorldChannelDirectory_NoChannelsPublishesNothing(t *testing.T) {
	sc, mr := newChannelDirectoryCtx(t)
	ctx := context.Background()
	seedDirectoryChannel(t, mr, lineSceneA, "10", 1)
	require.Equal(t, 1, PublishWorldChannelDirectoriesForZone(ctx, sc, testZoneId, directoryTestNow))
	before := lineNoTable(t, sc)
	require.Len(t, before, 1)
	publishedBefore := readPublishedDirectory(t, mr)

	// 在役 / 回收中两个集合都读成空。
	mr.Del(worldChannelsKey(testZoneId, directoryTestConfID))

	later := directoryTestNow.Add(time.Minute)
	assert.Equal(t, 0, PublishWorldChannelDirectoriesForZone(ctx, sc, testZoneId, later))
	assert.Equal(t, before, lineNoTable(t, sc), "没有线时不得清线号表")
	assert.Equal(t, publishedBefore.GetUpdatedAtMs(), readPublishedDirectory(t, mr).GetUpdatedAtMs(),
		"没有线时不覆盖旧目录")
}

// Redis 读失败:本轮不发布,线号表与旧目录都不动。读失败与读空必须区分 ——
// 把失败当成「没有线」去对齐线号表,恢复后线号会全部重排。
func TestPublishWorldChannelDirectory_RedisFailureKeepsLineNumbersAndOldDirectory(t *testing.T) {
	sc, mr := newChannelDirectoryCtx(t)
	ctx := context.Background()
	seedDirectoryChannel(t, mr, lineSceneA, "10", 1)
	seedDirectoryChannel(t, mr, lineSceneB, "10", 2)
	require.Equal(t, 1, PublishWorldChannelDirectoriesForZone(ctx, sc, testZoneId, directoryTestNow))
	before := lineNoTable(t, sc)
	require.Len(t, before, 2)

	mr.SetError("ERR injected")
	published := PublishWorldChannelDirectoriesForZone(ctx, sc, testZoneId, directoryTestNow.Add(time.Minute))
	mr.SetError("")

	assert.Equal(t, 0, published)
	assert.Equal(t, before, lineNoTable(t, sc), "读失败不得改动线号表")
	assert.Equal(t, uint64(directoryTestNow.UnixMilli()), readPublishedDirectory(t, mr).GetUpdatedAtMs(),
		"读失败不覆盖旧目录")
}

// 一张图失败(这里是线号表被写成了别的类型,脚本报 WRONGTYPE)不影响别的图,也不写半份目录。
func TestPublishWorldChannelDirectory_OneMapFailingDoesNotBlockOthers(t *testing.T) {
	sc, mr := newChannelDirectoryCtx(t)
	const otherConf = uint64(2)
	t.Cleanup(SetWorldConfIdsForTest([]uint64{directoryTestConfID, otherConf}))

	seedDirectoryChannel(t, mr, lineSceneA, "10", 1)
	require.NoError(t, mr.Set(worldChannelLineNoKey(testZoneId, directoryTestConfID), "not-a-hash"))

	_, err := mr.SAdd(worldChannelsKey(testZoneId, otherConf), sceneKey(lineSceneB))
	require.NoError(t, err)
	require.NoError(t, mr.Set(sceneNodeKey(lineSceneB), "10"))

	assert.Equal(t, 1, PublishWorldChannelDirectoriesForZone(context.Background(), sc, testZoneId, directoryTestNow),
		"只有 otherConf 发布成功")
	assert.False(t, mr.Exists(worldChannelDirectoryKey(testZoneId, directoryTestConfID)), "失败的图不写目录")
	assert.True(t, mr.Exists(worldChannelDirectoryKey(testZoneId, otherConf)))
}

// 地图从 World 表移除后,孤儿清理要把这张图的线号表与目录一起带走:
// 线号表没有 TTL,而且它们的键名不带 world_channels:zone: 前缀,清理的 SCAN 扫不到。
func TestOrphanCleanup_DropsLineNoTableAndDirectory(t *testing.T) {
	sc, mr := newChannelDirectoryCtx(t)
	ctx := context.Background()
	const deadConf = uint64(779)

	// 这张图还在 World 表里时铺过线、发布过目录。
	restore := SetWorldConfIdsForTest([]uint64{directoryTestConfID, deadConf})
	seedChannels(t, sc, testZoneId, deadConf, map[uint64]int64{901: 0})
	seedDirectoryChannel(t, mr, lineSceneA, "10", 1)
	require.Equal(t, 2, PublishWorldChannelDirectoriesForZone(ctx, sc, testZoneId, directoryTestNow))
	// 策划把它从 World 表删掉。
	restore()

	require.True(t, mr.Exists(worldChannelLineNoKey(testZoneId, deadConf)))
	require.True(t, mr.Exists(worldChannelDirectoryKey(testZoneId, deadConf)))

	require.Equal(t, 1, CleanupOrphanWorldChannels(ctx, sc))

	assert.False(t, mr.Exists(worldChannelLineNoKey(testZoneId, deadConf)), "线号表没有 TTL,必须点名删")
	assert.False(t, mr.Exists(worldChannelDirectoryKey(testZoneId, deadConf)))
	// 仍在 World 表里的图一个字节都不动。
	assert.True(t, mr.Exists(worldChannelLineNoKey(testZoneId, directoryTestConfID)))
	assert.True(t, mr.Exists(worldChannelDirectoryKey(testZoneId, directoryTestConfID)))
}

// 发布结果的 outcome 取值是 Prometheus label,这里钉住三个常量与实际返回值的对应关系。
func TestPublishWorldChannelDirectory_Outcomes(t *testing.T) {
	sc, mr := newChannelDirectoryCtx(t)

	outcome, err := publishWorldChannelDirectory(sc, testZoneId, directoryTestConfID, directoryTestNow)
	require.NoError(t, err)
	assert.Equal(t, metrics.ChannelDirectoryPublishEmpty, outcome)

	seedDirectoryChannel(t, mr, lineSceneA, "10", 1)
	outcome, err = publishWorldChannelDirectory(sc, testZoneId, directoryTestConfID, directoryTestNow)
	require.NoError(t, err)
	assert.Equal(t, metrics.ChannelDirectoryPublishOK, outcome)

	// 线号表被写成了别的类型:分配脚本报 WRONGTYPE。(miniredis 的直写 Set 不允许覆盖非字符串键,先删。)
	mr.Del(worldChannelLineNoKey(testZoneId, directoryTestConfID))
	require.NoError(t, mr.Set(worldChannelLineNoKey(testZoneId, directoryTestConfID), "not-a-hash"))
	outcome, err = publishWorldChannelDirectory(sc, testZoneId, directoryTestConfID, directoryTestNow)
	require.Error(t, err)
	assert.Equal(t, metrics.ChannelDirectoryPublishError, outcome)
}

// ── 跨语言契约 ───────────────────────────────────────────────────────────

// 目录键名是 Go 与 C++ 之间的契约:scene 节点按同一格式拼键来读
// (cpp/libs/services/scene/player/system/player_scene.cpp 的 kWorldChannelDirectoryKeyFmt,
// 字面量是 "world_channel_directory:zone:%u:%u" —— 占位符写法随语言不同,渲染出来的键必须逐字节相同)。
// 这条断言钉住 Go 侧字面量与渲染结果;改它之前必须同批改 C++。
func TestWorldChannelDirectoryKeyFmt_CrossLanguageContract(t *testing.T) {
	assert.Equal(t, "world_channel_directory:zone:%d:%d", WorldChannelDirectoryKeyFmt)
	assert.Equal(t, "world_channel_directory:zone:3:1001", worldChannelDirectoryKey(3, 1001))

	// 线号表只有 scene_manager 自己读写,不是跨语言契约;钉住它是为了守住「不用 world_channels:zone:
	// 前缀」这条约束(孤儿清理按那个前缀 SCAN,会把它当成频道集合)。
	assert.Equal(t, "world_channel_lineno:zone:%d:%d", WorldChannelLineNoKeyFmt)
	for _, key := range []string{worldChannelDirectoryKey(3, 1001), worldChannelLineNoKey(3, 1001)} {
		_, _, isChannelSet := parseWorldChannelsKey(key)
		assert.False(t, isChannelSet, "%s 不得被孤儿清理解析成频道集合", key)
	}
}
