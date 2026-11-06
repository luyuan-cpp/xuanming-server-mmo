package logic

// 神秘商人调度的单元测试,对应 docs/design/mystery-merchant.md §4。
//
// 这里能验证的是 scene_manager 这一侧的契约:什么时候算新的一轮、挑哪条线、掷出什么货架、
// 写进 Redis 的记录解析出来是什么、什么时候换线。「scene 节点读到记录并卖东西」在 C++ 侧,
// 要靠联调验证;两侧唯一的共同约定是键名 / field 名与 MysteryMerchantState 这个 proto,
// 前者由文件末尾的字面量断言钉住。
//
// 确定性:时间由用例传入;随机数用 scriptedMerchantRand 注入;配表用 merchantTables 字面量,
// 不读全局表管理器(那些要靠导表产物,单测里没有)。

import (
	"fmt"
	"testing"
	"time"

	scenepb "proto/scene"

	"scene_manager/internal/config"
	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gproto "google.golang.org/protobuf/proto"
)

const (
	merchantTestConfID = uint32(1)
	// 整点对齐的基准时刻:1757001600 = 488056 × 3600。轮次号就是这一轮出现时刻的 Unix 秒。
	merchantTestRoundStart = int64(1757001600)
	merchantTestRoundID    = uint64(merchantTestRoundStart)
)

// scriptedMerchantRand 按脚本依次返回 values[i] % n;脚本用完之后恒返回 0。
// asked 记下每次调用的 n,用例据此断言「掷了几次、各自的范围」。
type scriptedMerchantRand struct {
	values []uint64
	next   int
	asked  []uint64
}

func (r *scriptedMerchantRand) Uint64N(n uint64) uint64 {
	r.asked = append(r.asked, n)
	if r.next >= len(r.values) {
		return 0
	}
	v := r.values[r.next] % n
	r.next++
	return v
}

// merchantTestTables:每小时一轮、停留 30 分钟、货架 3 件;商品池 4 件;天墉城 2 个站位。
func merchantTestTables() merchantTables {
	return merchantTables{
		rule: merchantRule{
			intervalSeconds: 3600,
			staySeconds:     1800,
			shelfSize:       3,
			discountWeights: [3]uint64{50, 35, 15},
		},
		goods: []merchantGoods{{id: 1, weight: 100}, {id: 2, weight: 100}, {id: 3, weight: 50}, {id: 4, weight: 10}},
		spotsByConf: map[uint32][]merchantSpot{
			merchantTestConfID: {{id: 11, x: 176, y: 206, z: 0}, {id: 12, x: 161, y: 159, z: 0}},
		},
	}
}

func newMerchantCtx(t *testing.T) (*svc.ServiceContext, *miniredis.Miniredis) {
	t.Helper()
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	resetMerchantMissing(testZoneId)
	t.Cleanup(func() { resetMerchantMissing(testZoneId) })
	return sc, mr
}

type merchantTestLine struct {
	sceneID   uint64
	channelNo uint32
	state     scenepb.SceneChannelState
}

// publishMerchantDirectory 把一张图的分线目录直接摆进 miniredis(等价于目录发布循环跑过一轮)。
func publishMerchantDirectory(t *testing.T, mr *miniredis.Miniredis, confID uint32, lines ...merchantTestLine) {
	t.Helper()
	directory := &scenepb.SceneChannelDirectory{ZoneId: testZoneId, SceneConfigId: confID}
	for _, line := range lines {
		directory.Channels = append(directory.Channels, &scenepb.SceneChannelInfo{
			SceneId: line.sceneID, ChannelNo: line.channelNo, State: line.state,
		})
	}
	payload, err := gproto.Marshal(directory)
	require.NoError(t, err)
	require.NoError(t, mr.Set(worldChannelDirectoryKey(testZoneId, uint64(confID)), string(payload)))
}

func smoothLine(sceneID uint64, channelNo uint32) merchantTestLine {
	return merchantTestLine{sceneID: sceneID, channelNo: channelNo, state: scenepb.SceneChannelState_SCENE_CHANNEL_STATE_SMOOTH}
}

// readMerchantStateForTest 读出并解析已写入的记录;不存在返回 nil。
func readMerchantStateForTest(t *testing.T, mr *miniredis.Miniredis) *scenepb.MysteryMerchantState {
	t.Helper()
	if !mr.Exists(mysteryMerchantKey(testZoneId)) {
		return nil
	}
	raw := mr.HGet(mysteryMerchantKey(testZoneId), MysteryMerchantStateField)
	if raw == "" {
		return nil
	}
	state := &scenepb.MysteryMerchantState{}
	require.NoError(t, gproto.Unmarshal([]byte(raw), state))
	return state
}

func merchantAt(offsetSeconds int64) time.Time {
	return time.Unix(merchantTestRoundStart+offsetSeconds, 0)
}

// ── 纯函数 ───────────────────────────────────────────────────────────────

func TestMerchantRoundAt_AlignsToInterval(t *testing.T) {
	cases := []struct {
		name               string
		now                int64
		wantID             uint64
		wantAppear, wantEx int64
	}{
		{"整点那一秒就是新的一轮", merchantTestRoundStart, merchantTestRoundID, merchantTestRoundStart, merchantTestRoundStart + 1800},
		{"整点前一秒还是上一轮", merchantTestRoundStart - 1, merchantTestRoundID - 3600, merchantTestRoundStart - 3600, merchantTestRoundStart - 1800},
		{"停留期过了仍属于这一轮", merchantTestRoundStart + 3599, merchantTestRoundID, merchantTestRoundStart, merchantTestRoundStart + 1800},
		{"时钟为负按 0 处理", -5, 0, 0, 1800},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			round := merchantRoundAt(tc.now, 3600, 1800)
			assert.Equal(t, tc.wantID, round.id)
			assert.Equal(t, tc.wantAppear, round.appearAt)
			assert.Equal(t, tc.wantEx, round.expireAt)
		})
	}
}

func TestEligibleMerchantChannels_OnlyEnterableLines(t *testing.T) {
	directory := &scenepb.SceneChannelDirectory{SceneConfigId: merchantTestConfID, Channels: []*scenepb.SceneChannelInfo{
		{SceneId: 101, ChannelNo: 1, State: scenepb.SceneChannelState_SCENE_CHANNEL_STATE_SMOOTH},
		{SceneId: 102, ChannelNo: 2, State: scenepb.SceneChannelState_SCENE_CHANNEL_STATE_BUSY},
		{SceneId: 103, ChannelNo: 3, State: scenepb.SceneChannelState_SCENE_CHANNEL_STATE_FULL},
		{SceneId: 104, ChannelNo: 4, State: scenepb.SceneChannelState_SCENE_CHANNEL_STATE_CLOSING},
		{SceneId: 105, ChannelNo: 5, State: scenepb.SceneChannelState_SCENE_CHANNEL_STATE_UNAVAILABLE},
		{SceneId: 0, ChannelNo: 6, State: scenepb.SceneChannelState_SCENE_CHANNEL_STATE_SMOOTH},
	}}
	got := eligibleMerchantChannels(directory)
	assert.Equal(t, []merchantChannel{
		{confID: merchantTestConfID, sceneID: 101, channelNo: 1},
		{confID: merchantTestConfID, sceneID: 102, channelNo: 2},
	}, got, "有流畅 / 繁忙的线时只选它们;爆满、回收中、不可用、scene_id 为 0 的都不选")
}

func TestEligibleMerchantChannels_FallsBackToFullLines(t *testing.T) {
	directory := &scenepb.SceneChannelDirectory{SceneConfigId: merchantTestConfID, Channels: []*scenepb.SceneChannelInfo{
		{SceneId: 103, ChannelNo: 3, State: scenepb.SceneChannelState_SCENE_CHANNEL_STATE_FULL},
		{SceneId: 104, ChannelNo: 4, State: scenepb.SceneChannelState_SCENE_CHANNEL_STATE_CLOSING},
	}}
	assert.Equal(t, []merchantChannel{{confID: merchantTestConfID, sceneID: 103, channelNo: 3}},
		eligibleMerchantChannels(directory), "一条可进的线都没有时退而用爆满的线;回收中的仍不选")

	onlyClosing := &scenepb.SceneChannelDirectory{SceneConfigId: merchantTestConfID, Channels: []*scenepb.SceneChannelInfo{
		{SceneId: 104, ChannelNo: 4, State: scenepb.SceneChannelState_SCENE_CHANNEL_STATE_CLOSING},
	}}
	assert.Empty(t, eligibleMerchantChannels(onlyClosing))
}

func TestPickMerchantChannel_AvoidsPreviousLineWhenPossible(t *testing.T) {
	candidates := []merchantChannel{{sceneID: 101, channelNo: 1}, {sceneID: 102, channelNo: 2}, {sceneID: 103, channelNo: 3}}

	rnd := &scriptedMerchantRand{values: []uint64{0}}
	got, ok := pickMerchantChannel(candidates, 101, rnd)
	require.True(t, ok)
	assert.Equal(t, uint64(102), got.sceneID, "避开 101 之后池子是 [102, 103],下标 0 = 102")
	assert.Equal(t, []uint64{2}, rnd.asked, "应当在去掉上一轮那条线之后的 2 条里掷")

	rnd = &scriptedMerchantRand{values: []uint64{2}}
	got, ok = pickMerchantChannel(candidates, 0, rnd)
	require.True(t, ok)
	assert.Equal(t, uint64(103), got.sceneID, "avoid = 0 表示不避让,在全部 3 条里掷")

	only := []merchantChannel{{sceneID: 101, channelNo: 1}}
	got, ok = pickMerchantChannel(only, 101, &scriptedMerchantRand{})
	require.True(t, ok)
	assert.Equal(t, uint64(101), got.sceneID, "只有一条线时即使是上一轮那条也只能选它")

	_, ok = pickMerchantChannel(nil, 0, &scriptedMerchantRand{})
	assert.False(t, ok)
}

func TestPickMerchantWeightedIndex_Boundaries(t *testing.T) {
	weights := []uint64{50, 35, 15}
	for roll, want := range map[uint64]int{0: 0, 49: 0, 50: 1, 84: 1, 85: 2, 99: 2} {
		rnd := &scriptedMerchantRand{values: []uint64{roll}}
		assert.Equal(t, want, pickWeightedIndex(weights, rnd), "roll=%d", roll)
		assert.Equal(t, []uint64{100}, rnd.asked, "应当在权重之和的范围里掷")
	}
	// 权重为 0 的档永远掷不到:roll 落在它「该在的位置」时归后一档。
	assert.Equal(t, 2, pickWeightedIndex([]uint64{10, 0, 5}, &scriptedMerchantRand{values: []uint64{10}}))
}

func TestRollMerchantShelf_DrawsWithoutReplacementAndSortsByGoodsID(t *testing.T) {
	pool := []merchantGoods{{id: 7, weight: 1}, {id: 3, weight: 1}, {id: 9, weight: 1}, {id: 5, weight: 1}}
	// 每件消耗两次随机:先挑商品(在剩余池的权重和里),再挑折扣(在 100 里)。
	// 第 1 件:池 [7,3,9,5] 掷 2 → 9;折扣掷 0 → 60
	// 第 2 件:池 [7,3,5]   掷 0 → 7;折扣掷 50 → 50
	// 第 3 件:池 [3,5]     掷 1 → 5;折扣掷 99 → 40
	rnd := &scriptedMerchantRand{values: []uint64{2, 0, 0, 50, 1, 99}}
	shelf := rollMerchantShelf(pool, 3, [3]uint64{50, 35, 15}, rnd)

	require.Len(t, shelf, 3)
	assert.Equal(t, []uint64{4, 100, 3, 100, 2, 100}, rnd.asked, "商品在逐件缩小的池里掷,折扣在权重和里掷")
	got := map[uint32]uint32{}
	for _, entry := range shelf {
		got[entry.GetGoodsId()] = entry.GetDiscountPercent()
	}
	assert.Equal(t, map[uint32]uint32{9: 60, 7: 50, 5: 40}, got)
	assert.Equal(t, []uint32{5, 7, 9}, []uint32{shelf[0].GetGoodsId(), shelf[1].GetGoodsId(), shelf[2].GetGoodsId()},
		"货架按 goods_id 升序,这就是客户端的展示顺序")
	assert.Len(t, pool, 4, "不许改调用方传进来的池子")
}

func TestRollMerchantShelf_PoolSmallerThanShelf(t *testing.T) {
	pool := []merchantGoods{{id: 2, weight: 5}, {id: 1, weight: 5}}
	shelf := rollMerchantShelf(pool, 6, [3]uint64{0, 0, 1}, &scriptedMerchantRand{})
	require.Len(t, shelf, 2, "池子不够时有几件上几件")
	for _, entry := range shelf {
		assert.Equal(t, uint32(40), entry.GetDiscountPercent(), "只有 4 折有权重时每件都是 4 折")
	}
}

func TestValidateMerchantRule(t *testing.T) {
	good := merchantRule{intervalSeconds: 3600, staySeconds: 1800, shelfSize: 6, discountWeights: [3]uint64{50, 35, 15}}
	require.NoError(t, validateMerchantRule(good))

	alwaysPresent := good
	alwaysPresent.staySeconds = 3600
	require.NoError(t, validateMerchantRule(alwaysPresent), "停留 = 间隔是合法的:一直在场,只是每轮换线换货")

	bad := map[string]func(*merchantRule){
		"间隔太短":    func(r *merchantRule) { r.intervalSeconds = 59 },
		"间隔太长":    func(r *merchantRule) { r.intervalSeconds = 86401 },
		"停留太短":    func(r *merchantRule) { r.staySeconds = 29 },
		"停留比间隔还长": func(r *merchantRule) { r.staySeconds = 3601 },
		"货架为 0":   func(r *merchantRule) { r.shelfSize = 0 },
		"货架过大":    func(r *merchantRule) { r.shelfSize = 33 },
		"折扣权重全 0": func(r *merchantRule) { r.discountWeights = [3]uint64{} },
	}
	for name, mutate := range bad {
		rule := good
		mutate(&rule)
		assert.Error(t, validateMerchantRule(rule), name)
	}
}

func TestPinMerchantSpot(t *testing.T) {
	spots := map[uint32][]merchantSpot{
		1: {{id: 11, x: 176, y: 206}, {id: 12, x: 161, y: 159}},
		4: {{id: 41, x: 10, y: 20}},
	}
	pinned, err := pinMerchantSpot(spots, 12)
	require.NoError(t, err)
	assert.Equal(t, map[uint32][]merchantSpot{1: {{id: 12, x: 161, y: 159}}}, pinned, "只留下被钉的站位与它所在的地图")
	assert.Len(t, spots[1], 2, "不许改传进来的表")

	_, err = pinMerchantSpot(spots, 99)
	assert.Error(t, err, "钉一个不存在的站位是配置笔误,不能悄悄退回随机")
}

func TestConfig_MysteryMerchantEffectiveTickSeconds(t *testing.T) {
	for configured, want := range map[int64]int64{0: 5, -3: 1, 1: 1, 7: 7, 60: 60, 600: 60} {
		assert.Equal(t, want, config.MysteryMerchantConfig{TickSeconds: configured}.EffectiveTickSeconds(), "TickSeconds=%d", configured)
	}
}

// ── 一拍调度 ─────────────────────────────────────────────────────────────

func TestScheduleMysteryMerchant_CreatesOncePerRoundThenKeeps(t *testing.T) {
	sc, mr := newMerchantCtx(t)
	publishMerchantDirectory(t, mr, merchantTestConfID, smoothLine(101, 1), smoothLine(102, 2), smoothLine(103, 3))
	tables := merchantTestTables()

	// 随机脚本:线掷 1 → 2 线;货架 3 件 × (商品, 折扣);站位掷 1 → spot 12。
	rnd := &scriptedMerchantRand{values: []uint64{1, 0, 0, 0, 60, 0, 90, 1}}
	now := merchantAt(7)
	outcome, err := scheduleMysteryMerchant(sc, testZoneId, now, rnd, tables)
	require.NoError(t, err)
	require.Equal(t, metrics.MerchantScheduleCreated, outcome)

	state := readMerchantStateForTest(t, mr)
	require.NotNil(t, state)
	assert.Equal(t, uint32(testZoneId), state.GetZoneId())
	assert.Equal(t, merchantTestRoundID, state.GetRoundId())
	assert.Equal(t, uint32(1), state.GetRevision())
	assert.Equal(t, merchantTestConfID, state.GetSceneConfigId())
	assert.Equal(t, uint64(102), state.GetSceneId())
	assert.Equal(t, uint32(2), state.GetChannelNo())
	assert.Equal(t, uint32(12), state.GetSpotId())
	assert.Equal(t, 161.0, state.GetPosition().GetX())
	assert.Equal(t, 159.0, state.GetPosition().GetY())
	assert.Equal(t, uint64(merchantTestRoundStart)*1000, state.GetAppearAtMs(), "出现时刻是这一轮的整点,不是调度跑到的那一秒")
	assert.Equal(t, uint64(merchantTestRoundStart+1800)*1000, state.GetExpireAtMs())
	assert.Equal(t, uint32(3600), state.GetIntervalSeconds())
	assert.Equal(t, uint64(now.UnixMilli()), state.GetUpdatedAtMs())
	require.Len(t, state.GetShelf(), 3)
	assert.Equal(t, []uint32{1, 2, 3}, []uint32{state.GetShelf()[0].GetGoodsId(), state.GetShelf()[1].GetGoodsId(), state.GetShelf()[2].GetGoodsId()})
	assert.Equal(t, []uint32{60, 50, 40}, []uint32{state.GetShelf()[0].GetDiscountPercent(), state.GetShelf()[1].GetDiscountPercent(), state.GetShelf()[2].GetDiscountPercent()})

	assert.Equal(t, fmt.Sprintf("%d", merchantTestRoundID*1000+1), mr.HGet(mysteryMerchantKey(testZoneId), MysteryMerchantVersionField))
	assert.Equal(t, 24*time.Hour, mr.TTL(mysteryMerchantKey(testZoneId)), "TTL = 2 个间隔,但不低于最长间隔(merchantMaxIntervalSeconds)")

	// 同一轮内再跑:线还在,什么都不写。
	before := mr.HGet(mysteryMerchantKey(testZoneId), MysteryMerchantStateField)
	outcome, err = scheduleMysteryMerchant(sc, testZoneId, merchantAt(600), &scriptedMerchantRand{values: []uint64{2, 2, 2}}, tables)
	require.NoError(t, err)
	assert.Equal(t, metrics.MerchantScheduleKept, outcome)
	assert.Equal(t, before, mr.HGet(mysteryMerchantKey(testZoneId), MysteryMerchantStateField), "同一轮内记录不许变")

	// 停留期过了、还没到下一个整点:不换线也不补排。
	outcome, err = scheduleMysteryMerchant(sc, testZoneId, merchantAt(1800), &scriptedMerchantRand{}, tables)
	require.NoError(t, err)
	assert.Equal(t, metrics.MerchantScheduleIdle, outcome)

	// 下一个整点:新的一轮,并且避开上一轮的 2 线(池子变成 [1 线, 3 线],掷 0 → 1 线)。
	outcome, err = scheduleMysteryMerchant(sc, testZoneId, merchantAt(3600), &scriptedMerchantRand{values: []uint64{0}}, tables)
	require.NoError(t, err)
	require.Equal(t, metrics.MerchantScheduleCreated, outcome)
	next := readMerchantStateForTest(t, mr)
	assert.Equal(t, merchantTestRoundID+3600, next.GetRoundId(), "轮次号 = 出现时刻的 Unix 秒")
	assert.Equal(t, uint32(1), next.GetRevision())
	assert.Equal(t, uint64(101), next.GetSceneId(), "新的一轮应当避开上一轮那条线")
}

func TestScheduleMysteryMerchant_DoesNotBackfillAfterStayWindow(t *testing.T) {
	sc, mr := newMerchantCtx(t)
	publishMerchantDirectory(t, mr, merchantTestConfID, smoothLine(101, 1))

	// 调度在整点之后 31 分钟才第一次跑到:这一轮的停留期已过,不补排。
	outcome, err := scheduleMysteryMerchant(sc, testZoneId, merchantAt(1860), &scriptedMerchantRand{}, merchantTestTables())
	require.NoError(t, err)
	assert.Equal(t, metrics.MerchantScheduleIdle, outcome)
	assert.Nil(t, readMerchantStateForTest(t, mr))
}

func TestScheduleMysteryMerchant_LateStartStillCreatesWithinStayWindow(t *testing.T) {
	sc, mr := newMerchantCtx(t)
	publishMerchantDirectory(t, mr, merchantTestConfID, smoothLine(101, 1))

	// 整点之后 10 分钟才跑到:还在停留期内,照常排,出现时刻仍记整点。
	outcome, err := scheduleMysteryMerchant(sc, testZoneId, merchantAt(600), &scriptedMerchantRand{}, merchantTestTables())
	require.NoError(t, err)
	require.Equal(t, metrics.MerchantScheduleCreated, outcome)
	state := readMerchantStateForTest(t, mr)
	assert.Equal(t, uint64(merchantTestRoundStart)*1000, state.GetAppearAtMs())
}

func TestScheduleMysteryMerchant_NoEnterableChannelWritesNothing(t *testing.T) {
	sc, mr := newMerchantCtx(t)

	// 目录还没发布。
	outcome, err := scheduleMysteryMerchant(sc, testZoneId, merchantAt(5), &scriptedMerchantRand{}, merchantTestTables())
	require.NoError(t, err)
	assert.Equal(t, metrics.MerchantScheduleNoChannel, outcome)
	assert.Nil(t, readMerchantStateForTest(t, mr))

	// 目录里只有回收中 / 不可用的线。
	publishMerchantDirectory(t, mr, merchantTestConfID,
		merchantTestLine{sceneID: 101, channelNo: 1, state: scenepb.SceneChannelState_SCENE_CHANNEL_STATE_CLOSING},
		merchantTestLine{sceneID: 102, channelNo: 2, state: scenepb.SceneChannelState_SCENE_CHANNEL_STATE_UNAVAILABLE})
	outcome, err = scheduleMysteryMerchant(sc, testZoneId, merchantAt(10), &scriptedMerchantRand{}, merchantTestTables())
	require.NoError(t, err)
	assert.Equal(t, metrics.MerchantScheduleNoChannel, outcome)
	assert.Nil(t, readMerchantStateForTest(t, mr))

	// 线恢复之后的下一拍就排上。
	publishMerchantDirectory(t, mr, merchantTestConfID, smoothLine(101, 1))
	outcome, err = scheduleMysteryMerchant(sc, testZoneId, merchantAt(15), &scriptedMerchantRand{}, merchantTestTables())
	require.NoError(t, err)
	assert.Equal(t, metrics.MerchantScheduleCreated, outcome)
}

func TestScheduleMysteryMerchant_IgnoresMapsWithoutSpots(t *testing.T) {
	sc, mr := newMerchantCtx(t)
	// 地图 2 有线但表里没有它的站位:商人不去。
	publishMerchantDirectory(t, mr, 2, smoothLine(201, 1))
	outcome, err := scheduleMysteryMerchant(sc, testZoneId, merchantAt(5), &scriptedMerchantRand{}, merchantTestTables())
	require.NoError(t, err)
	assert.Equal(t, metrics.MerchantScheduleNoChannel, outcome)
	assert.Nil(t, readMerchantStateForTest(t, mr))
}

func TestScheduleMysteryMerchant_RelocatesImmediatelyWhenLineIsClosing(t *testing.T) {
	sc, mr := newMerchantCtx(t)
	publishMerchantDirectory(t, mr, merchantTestConfID, smoothLine(101, 1), smoothLine(102, 2))
	tables := merchantTestTables()

	outcome, err := scheduleMysteryMerchant(sc, testZoneId, merchantAt(5), &scriptedMerchantRand{}, tables)
	require.NoError(t, err)
	require.Equal(t, metrics.MerchantScheduleCreated, outcome)
	original := readMerchantStateForTest(t, mr)
	require.Equal(t, uint64(101), original.GetSceneId())

	// 1 线开始回收。
	publishMerchantDirectory(t, mr, merchantTestConfID,
		merchantTestLine{sceneID: 101, channelNo: 1, state: scenepb.SceneChannelState_SCENE_CHANNEL_STATE_CLOSING},
		smoothLine(102, 2))
	relocateAt := merchantAt(300)
	outcome, err = scheduleMysteryMerchant(sc, testZoneId, relocateAt, &scriptedMerchantRand{values: []uint64{0, 1}}, tables)
	require.NoError(t, err)
	require.Equal(t, metrics.MerchantScheduleRelocated, outcome)

	moved := readMerchantStateForTest(t, mr)
	assert.Equal(t, original.GetRoundId(), moved.GetRoundId(), "换线不换轮次:限购记录按轮次号记")
	assert.Equal(t, uint32(2), moved.GetRevision())
	assert.Equal(t, uint64(102), moved.GetSceneId())
	assert.Equal(t, uint32(2), moved.GetChannelNo())
	assert.Equal(t, uint32(12), moved.GetSpotId(), "站位重新掷")
	assert.Equal(t, original.GetAppearAtMs(), moved.GetAppearAtMs())
	assert.Equal(t, original.GetExpireAtMs(), moved.GetExpireAtMs())
	assert.True(t, gproto.Equal(&scenepb.MysteryMerchantState{Shelf: original.GetShelf()}, &scenepb.MysteryMerchantState{Shelf: moved.GetShelf()}),
		"换线不换货架")
	assert.Equal(t, uint64(relocateAt.UnixMilli()), moved.GetUpdatedAtMs())
}

func TestScheduleMysteryMerchant_RelocatesOnlyAfterLineIsMissingForSeveralTicks(t *testing.T) {
	sc, mr := newMerchantCtx(t)
	publishMerchantDirectory(t, mr, merchantTestConfID, smoothLine(101, 1), smoothLine(102, 2))
	tables := merchantTestTables()

	_, err := scheduleMysteryMerchant(sc, testZoneId, merchantAt(5), &scriptedMerchantRand{}, tables)
	require.NoError(t, err)
	require.Equal(t, uint64(101), readMerchantStateForTest(t, mr).GetSceneId())

	// 1 线从目录里消失。前两拍只是观望(排空起步时目录会短暂缺它)。
	publishMerchantDirectory(t, mr, merchantTestConfID, smoothLine(102, 2))
	for tick := int64(1); tick < mysteryMerchantMissingTicksBeforeRelocate; tick++ {
		outcome, err := scheduleMysteryMerchant(sc, testZoneId, merchantAt(5+5*tick), &scriptedMerchantRand{}, tables)
		require.NoError(t, err)
		assert.Equal(t, metrics.MerchantScheduleKept, outcome, "第 %d 拍缺席还不该换线", tick)
		assert.Equal(t, uint64(101), readMerchantStateForTest(t, mr).GetSceneId())
	}
	// 第 3 拍仍然缺席:换线。
	outcome, err := scheduleMysteryMerchant(sc, testZoneId, merchantAt(30), &scriptedMerchantRand{}, tables)
	require.NoError(t, err)
	require.Equal(t, metrics.MerchantScheduleRelocated, outcome)
	assert.Equal(t, uint64(102), readMerchantStateForTest(t, mr).GetSceneId())
}

func TestScheduleMysteryMerchant_MissingCounterResetsWhenLineComesBack(t *testing.T) {
	sc, mr := newMerchantCtx(t)
	publishMerchantDirectory(t, mr, merchantTestConfID, smoothLine(101, 1), smoothLine(102, 2))
	tables := merchantTestTables()
	_, err := scheduleMysteryMerchant(sc, testZoneId, merchantAt(5), &scriptedMerchantRand{}, tables)
	require.NoError(t, err)

	// 缺席两拍 → 回来一拍 → 再缺席两拍:中间回来过,计数应当清零,所以仍不换线。
	absent := func(offset int64) {
		publishMerchantDirectory(t, mr, merchantTestConfID, smoothLine(102, 2))
		outcome, err := scheduleMysteryMerchant(sc, testZoneId, merchantAt(offset), &scriptedMerchantRand{}, tables)
		require.NoError(t, err)
		assert.Equal(t, metrics.MerchantScheduleKept, outcome)
	}
	absent(10)
	absent(15)
	publishMerchantDirectory(t, mr, merchantTestConfID, smoothLine(101, 1), smoothLine(102, 2))
	outcome, err := scheduleMysteryMerchant(sc, testZoneId, merchantAt(20), &scriptedMerchantRand{}, tables)
	require.NoError(t, err)
	assert.Equal(t, metrics.MerchantScheduleKept, outcome)
	absent(25)
	absent(30)
	assert.Equal(t, uint64(101), readMerchantStateForTest(t, mr).GetSceneId())
	assert.Equal(t, uint32(1), readMerchantStateForTest(t, mr).GetRevision())
}

func TestScheduleMysteryMerchant_KeepsWhenLineIsOnlyUnavailableOrDirectoryIsGone(t *testing.T) {
	sc, mr := newMerchantCtx(t)
	publishMerchantDirectory(t, mr, merchantTestConfID, smoothLine(101, 1), smoothLine(102, 2))
	tables := merchantTestTables()
	_, err := scheduleMysteryMerchant(sc, testZoneId, merchantAt(5), &scriptedMerchantRand{}, tables)
	require.NoError(t, err)

	// 所在节点刚判死:线会在别的节点按同一个 scene_id 重建,不换线。
	publishMerchantDirectory(t, mr, merchantTestConfID,
		merchantTestLine{sceneID: 101, channelNo: 1, state: scenepb.SceneChannelState_SCENE_CHANNEL_STATE_UNAVAILABLE},
		smoothLine(102, 2))
	for tick := int64(0); tick < 5; tick++ {
		outcome, err := scheduleMysteryMerchant(sc, testZoneId, merchantAt(10+5*tick), &scriptedMerchantRand{}, tables)
		require.NoError(t, err)
		assert.Equal(t, metrics.MerchantScheduleKept, outcome)
	}

	// 目录整个缺失(领导者刚切换):无从判断,同样不动,也不累计缺席。
	mr.Del(worldChannelDirectoryKey(testZoneId, uint64(merchantTestConfID)))
	for tick := int64(0); tick < 5; tick++ {
		outcome, err := scheduleMysteryMerchant(sc, testZoneId, merchantAt(60+5*tick), &scriptedMerchantRand{}, tables)
		require.NoError(t, err)
		assert.Equal(t, metrics.MerchantScheduleKept, outcome)
	}
	state := readMerchantStateForTest(t, mr)
	assert.Equal(t, uint64(101), state.GetSceneId())
	assert.Equal(t, uint32(1), state.GetRevision())
}

func TestScheduleMysteryMerchant_NoRelocationTargetKeepsRecord(t *testing.T) {
	sc, mr := newMerchantCtx(t)
	publishMerchantDirectory(t, mr, merchantTestConfID, smoothLine(101, 1))
	tables := merchantTestTables()
	_, err := scheduleMysteryMerchant(sc, testZoneId, merchantAt(5), &scriptedMerchantRand{}, tables)
	require.NoError(t, err)

	// 唯一的线开始回收,没有别的线可去:记录保持原样(线上还没走的人照样能买完)。
	publishMerchantDirectory(t, mr, merchantTestConfID,
		merchantTestLine{sceneID: 101, channelNo: 1, state: scenepb.SceneChannelState_SCENE_CHANNEL_STATE_CLOSING})
	outcome, err := scheduleMysteryMerchant(sc, testZoneId, merchantAt(60), &scriptedMerchantRand{}, tables)
	require.NoError(t, err)
	assert.Equal(t, metrics.MerchantScheduleNoChannel, outcome)
	state := readMerchantStateForTest(t, mr)
	assert.Equal(t, uint64(101), state.GetSceneId())
	assert.Equal(t, uint32(1), state.GetRevision())
}

func TestScheduleMysteryMerchant_CorruptRecordIsReplaced(t *testing.T) {
	sc, mr := newMerchantCtx(t)
	publishMerchantDirectory(t, mr, merchantTestConfID, smoothLine(101, 1))
	// 坏记录:state 不是合法的 protobuf,v 不是数字。
	mr.HSet(mysteryMerchantKey(testZoneId), MysteryMerchantStateField, "\xff\xff\xff\xff", MysteryMerchantVersionField, "garbage")

	outcome, err := scheduleMysteryMerchant(sc, testZoneId, merchantAt(5), &scriptedMerchantRand{}, merchantTestTables())
	require.NoError(t, err)
	require.Equal(t, metrics.MerchantScheduleCreated, outcome, "坏记录按无记录处理,不能把调度永远卡住")
	assert.Equal(t, merchantTestRoundID, readMerchantStateForTest(t, mr).GetRoundId())
}

func TestScheduleMysteryMerchant_CorruptRecordWithAValidVersionIsStillReplaced(t *testing.T) {
	sc, mr := newMerchantCtx(t)
	publishMerchantDirectory(t, mr, merchantTestConfID, smoothLine(101, 1))
	// state 坏了,v 却恰好是本轮第 1 次落位的合法版本:不强制覆盖的话,重掷出来的同版本记录会被脚本拒掉,
	// 这一轮就没有商人了。
	mr.HSet(mysteryMerchantKey(testZoneId), MysteryMerchantStateField, "\xff\xff\xff\xff",
		MysteryMerchantVersionField, fmt.Sprintf("%d", merchantTestRoundID*1000+1))

	outcome, err := scheduleMysteryMerchant(sc, testZoneId, merchantAt(5), &scriptedMerchantRand{}, merchantTestTables())
	require.NoError(t, err)
	require.Equal(t, metrics.MerchantScheduleCreated, outcome)
	assert.Equal(t, uint64(101), readMerchantStateForTest(t, mr).GetSceneId())
}

// 联调时把间隔从 120 秒改回 3600 秒(冒烟配置 → 表值)是文档自己要求的操作:旧记录是 12:58 那一轮,
// 新时间轴上「现在」属于 12:00 那一轮,出现时刻更早、版本号更小。不强制覆盖的话要等旧键 TTL 过期才写得进去。
func TestScheduleMysteryMerchant_IntervalChangeDiscardsTheOldTimeline(t *testing.T) {
	sc, mr := newMerchantCtx(t)
	publishMerchantDirectory(t, mr, merchantTestConfID, smoothLine(101, 1), smoothLine(102, 2))

	short := merchantTestTables()
	short.rule.intervalSeconds, short.rule.staySeconds = 120, 120
	now := merchantAt(1500) // 整点后 25 分钟:120 秒时间轴上是 1440 秒那一轮,3600 秒时间轴上还在停留期内
	outcome, err := scheduleMysteryMerchant(sc, testZoneId, now, &scriptedMerchantRand{}, short)
	require.NoError(t, err)
	require.Equal(t, metrics.MerchantScheduleCreated, outcome)
	old := readMerchantStateForTest(t, mr)
	require.Equal(t, uint64(merchantTestRoundStart+1440), old.GetRoundId())

	outcome, err = scheduleMysteryMerchant(sc, testZoneId, now.Add(5*time.Second), &scriptedMerchantRand{}, merchantTestTables())
	require.NoError(t, err)
	require.Equal(t, metrics.MerchantScheduleCreated, outcome, "旧时间轴的记录版本号更大,也必须让位")
	fresh := readMerchantStateForTest(t, mr)
	assert.Equal(t, merchantTestRoundID, fresh.GetRoundId())
	assert.Equal(t, uint32(3600), fresh.GetIntervalSeconds())
	assert.Equal(t, uint64(merchantTestRoundStart+1800)*1000, fresh.GetExpireAtMs())

	// 之后回到正常路径:同一轮内不再重写。
	outcome, err = scheduleMysteryMerchant(sc, testZoneId, now.Add(10*time.Second), &scriptedMerchantRand{}, merchantTestTables())
	require.NoError(t, err)
	assert.Equal(t, metrics.MerchantScheduleKept, outcome)
}

func TestScheduleMysteryMerchant_DirectoryOfAnotherMapUnderTheKeyIsAnError(t *testing.T) {
	sc, mr := newMerchantCtx(t)
	// 键是地图 1 的,内容却写着地图 2(数据被写坏)。
	directory := &scenepb.SceneChannelDirectory{ZoneId: testZoneId, SceneConfigId: 2, Channels: []*scenepb.SceneChannelInfo{
		{SceneId: 201, ChannelNo: 1, State: scenepb.SceneChannelState_SCENE_CHANNEL_STATE_SMOOTH},
	}}
	payload, err := gproto.Marshal(directory)
	require.NoError(t, err)
	require.NoError(t, mr.Set(worldChannelDirectoryKey(testZoneId, uint64(merchantTestConfID)), string(payload)))

	outcome, err := scheduleMysteryMerchant(sc, testZoneId, merchantAt(5), &scriptedMerchantRand{}, merchantTestTables())
	require.Error(t, err)
	assert.Equal(t, metrics.MerchantScheduleError, outcome)
	assert.Nil(t, readMerchantStateForTest(t, mr))
}

func TestWriteMysteryMerchantState_VersionOnlyMovesForward(t *testing.T) {
	sc, mr := newMerchantCtx(t)
	base := &scenepb.MysteryMerchantState{ZoneId: testZoneId, RoundId: merchantTestRoundID, Revision: 2, SceneId: 101, IntervalSeconds: 3600}

	written, err := writeMysteryMerchantState(sc, base, false)
	require.NoError(t, err)
	require.True(t, written)

	// 同一版本的另一份(领导权交接窗口里另一个副本掷出的):不许盖。
	rival := gproto.Clone(base).(*scenepb.MysteryMerchantState)
	rival.SceneId = 999
	written, err = writeMysteryMerchantState(sc, rival, false)
	require.NoError(t, err)
	assert.False(t, written)

	// 更旧的版本:不许盖。
	stale := gproto.Clone(base).(*scenepb.MysteryMerchantState)
	stale.Revision = 1
	stale.SceneId = 998
	written, err = writeMysteryMerchantState(sc, stale, false)
	require.NoError(t, err)
	assert.False(t, written)
	assert.Equal(t, uint64(101), readMerchantStateForTest(t, mr).GetSceneId())

	// 更新的版本(同一轮换线、或下一轮):放行。
	newer := gproto.Clone(base).(*scenepb.MysteryMerchantState)
	newer.Revision = 3
	newer.SceneId = 102
	written, err = writeMysteryMerchantState(sc, newer, false)
	require.NoError(t, err)
	assert.True(t, written)
	nextRound := gproto.Clone(base).(*scenepb.MysteryMerchantState)
	nextRound.RoundId += 3600
	nextRound.Revision = 1
	nextRound.SceneId = 103
	written, err = writeMysteryMerchantState(sc, nextRound, false)
	require.NoError(t, err)
	assert.True(t, written)
	assert.Equal(t, uint64(103), readMerchantStateForTest(t, mr).GetSceneId())

	// 强制覆盖:版本更旧也写得进去(只用于现有记录已经不可信的情形)。
	forced := gproto.Clone(base).(*scenepb.MysteryMerchantState)
	forced.SceneId = 104
	written, err = writeMysteryMerchantState(sc, forced, true)
	require.NoError(t, err)
	assert.True(t, written)
	assert.Equal(t, uint64(104), readMerchantStateForTest(t, mr).GetSceneId())
	assert.Equal(t, fmt.Sprintf("%d", merchantTestRoundID*1000+2), mr.HGet(mysteryMerchantKey(testZoneId), MysteryMerchantVersionField),
		"强制覆盖后 v 也跟着回到这份记录自己的版本")
}

func TestScheduleMysteryMerchant_RedisFailureIsReportedAndWritesNothing(t *testing.T) {
	sc, mr := newMerchantCtx(t)
	publishMerchantDirectory(t, mr, merchantTestConfID, smoothLine(101, 1))
	mr.SetError("ERR injected")
	outcome, err := scheduleMysteryMerchant(sc, testZoneId, merchantAt(5), &scriptedMerchantRand{}, merchantTestTables())
	mr.SetError("")
	require.Error(t, err)
	assert.Equal(t, metrics.MerchantScheduleError, outcome)
	assert.Nil(t, readMerchantStateForTest(t, mr))
}

func TestScheduleMysteryMerchantForZone_FollowerDoesNothing(t *testing.T) {
	sc, mr := newMerchantCtx(t)
	publishMerchantDirectory(t, mr, merchantTestConfID, smoothLine(101, 1))
	t.Cleanup(SetLeaderCheckForTest(func() bool { return false }))

	ScheduleMysteryMerchantForZone(sc, testZoneId, merchantAt(5))
	assert.False(t, mr.Exists(mysteryMerchantKey(testZoneId)), "跟随者不许调度")
}

// ── 跨语言契约 ───────────────────────────────────────────────────────────

// C++ scene 节点按同一格式拼键、读同一个 field(player_mystery_merchant.cpp 的 kMysteryMerchantKeyFmt /
// kMysteryMerchantStateField)。这里把本侧的字面量与渲染结果钉死:改动任何一个都必须同批改 C++。
func TestMysteryMerchantKey_CrossLanguageContract(t *testing.T) {
	assert.Equal(t, "mystery_merchant:zone:%d", MysteryMerchantKeyFmt)
	assert.Equal(t, "mystery_merchant:zone:7", mysteryMerchantKey(7))
	assert.Equal(t, "state", MysteryMerchantStateField)
	assert.Equal(t, "v", MysteryMerchantVersionField)
	assert.Equal(t, [3]uint32{60, 50, 40}, mysteryMerchantDiscountPercents)
}
