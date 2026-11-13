package loginqueue

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 本文件钉住 gate 排空迁移在 login 一侧的判定(docs/design/gate-drain-session-migration.md §4.4)。

func migratePolicy() DrainMigratePolicy {
	return DrainMigratePolicy{
		Enabled:             true,
		RatePerSecond:       20,
		StartAfterSeconds:   0,
		DrainedBelowPlayers: 0,
		LeaseMs:             15000,
	}
}

func drainingGate(id uint32, online uint32) GateOnline {
	return GateOnline{NodeID: id, PlayerCount: online, InstanceID: "uuid-" + strconv.FormatUint(uint64(id), 10)}
}

// ── DecideDrainMigration ────────────────────────────────────────────────

func TestDecideDrainMigration_DueCarriesLeaseFields(t *testing.T) {
	lease, ok := DecideDrainMigration(drainingGate(7, 300), "1000", 1010, migratePolicy())
	require.True(t, ok)
	assert.Equal(t, DrainMigrationLease{
		NodeID:        7,
		InstanceID:    "uuid-7",
		DrainMarkedAt: 1000,
		Online:        300,
		RatePerSecond: 20,
		LeaseMs:       15000,
	}, lease)
}

func TestDecideDrainMigration_EachConditionCanVeto(t *testing.T) {
	disabled := migratePolicy()
	disabled.Enabled = false
	zeroRate := migratePolicy()
	zeroRate.RatePerSecond = 0
	zeroLease := migratePolicy()
	zeroLease.LeaseMs = 0
	threshold := migratePolicy()
	threshold.DrainedBelowPlayers = 5
	notYet := migratePolicy()
	notYet.StartAfterSeconds = 60

	cases := []struct {
		name   string
		gate   GateOnline
		marked string
		now    int64
		policy DrainMigratePolicy
	}{
		{"迁移关闭", drainingGate(7, 300), "1000", 1010, disabled},
		{"速率为 0 等同关闭", drainingGate(7, 300), "1000", 1010, zeroRate},
		{"租期为 0 的许可没有意义", drainingGate(7, 300), "1000", 1010, zeroLease},
		{"不在排空", drainingGate(7, 300), "", 1010, migratePolicy()},
		{"没有实例 uuid 就没法定向发命令", GateOnline{NodeID: 7, PlayerCount: 300}, "1000", 1010, migratePolicy()},
		{"在线数已不大于排空阈值(相等也不迁)", drainingGate(7, 5), "1000", 1010, threshold},
		{"还没满 StartAfter", drainingGate(7, 300), "1000", 1059, notYet},
	}
	for _, c := range cases {
		_, ok := DecideDrainMigration(c.gate, c.marked, c.now, c.policy)
		assert.False(t, ok, c.name)
	}

	_, ok := DecideDrainMigration(drainingGate(7, 300), "1000", 1060, notYet)
	assert.True(t, ok, "恰好满 StartAfter 就发")
	_, ok = DecideDrainMigration(drainingGate(7, 6), "1000", 1010, threshold)
	assert.True(t, ok, "在线数大于阈值仍要迁")
}

// 标记时刻在未来(时钟回拨 / 标记来自别的机器):按刚标记处理,StartAfter 不会被负数提前满足。
func TestDecideDrainMigration_ClockSkewWaitsForStartAfter(t *testing.T) {
	p := migratePolicy()
	p.StartAfterSeconds = 30
	_, ok := DecideDrainMigration(drainingGate(7, 300), "2000", 1000, p)
	assert.False(t, ok)

	p.StartAfterSeconds = 0
	lease, ok := DecideDrainMigration(drainingGate(7, 300), "2000", 1000, p)
	require.True(t, ok, "StartAfter = 0 时立即开始")
	assert.EqualValues(t, 2000, lease.DrainMarkedAt, "标记值原样带给 gate,只用于日志")
}

// ── DrainMigrationsDue ──────────────────────────────────────────────────

func TestDrainMigrationsDue_OnlyDrainingGatesAboveThreshold(t *testing.T) {
	rdb, _ := newDrainRedis(t)
	ctx := context.Background()
	require.NoError(t, rdb.Set(ctx, gateDrainingKey(1), "1000", time.Hour).Err())
	require.NoError(t, rdb.Set(ctx, gateDrainingKey(2), "1000", time.Hour).Err())

	p := migratePolicy()
	p.DrainedBelowPlayers = 3
	gates := []GateOnline{drainingGate(1, 100), drainingGate(2, 3), drainingGate(3, 500)}

	due := DrainMigrationsDue(ctx, rdb, gates, p, 1010)
	require.Len(t, due, 1, "2 已够阈值,3 没在排空")
	assert.EqualValues(t, 1, due[0].NodeID)
	assert.Equal(t, "uuid-1", due[0].InstanceID)
	assert.EqualValues(t, 100, due[0].Online)
}

func TestDrainMigrationsDue_DisabledReadsNothing(t *testing.T) {
	rdb, mr := newDrainRedis(t)
	ctx := context.Background()
	require.NoError(t, rdb.Set(ctx, gateDrainingKey(1), "1000", time.Hour).Err())
	mr.Close() // 关闭时若还去读 Redis 会报错;返回 nil 而不出错说明根本没读

	p := migratePolicy()
	p.Enabled = false
	assert.Nil(t, DrainMigrationsDue(ctx, rdb, []GateOnline{drainingGate(1, 100)}, p, 1010))
}

// 读不到标记时本拍一条都不发:方向是少迁,不误迁。
func TestDrainMigrationsDue_ReadErrorSendsNothing(t *testing.T) {
	rdb, mr := newDrainRedis(t)
	mr.Close()
	assert.Nil(t, DrainMigrationsDue(context.Background(), rdb, []GateOnline{drainingGate(1, 100)}, migratePolicy(), 1010))
}

// ── DrainingGateSet ─────────────────────────────────────────────────────

func TestDrainingGateSet_ReportsMarksAndFailsClosed(t *testing.T) {
	rdb, mr := newDrainRedis(t)
	ctx := context.Background()
	require.NoError(t, rdb.Set(ctx, gateDrainingKey(2), "1000", time.Hour).Err())

	set, err := DrainingGateSet(ctx, rdb, []uint32{1, 2, 3})
	require.NoError(t, err)
	assert.Equal(t, map[uint32]bool{2: true}, set)

	empty, err := DrainingGateSet(ctx, rdb, nil)
	require.NoError(t, err)
	assert.Empty(t, empty)

	mr.Close()
	set, err = DrainingGateSet(ctx, rdb, []uint32{1, 2})
	assert.Error(t, err, "读失败必须报错:签票时不知道谁在排空就不签")
	assert.Nil(t, set)

	_, err = DrainingGateSet(ctx, nil, []uint32{1})
	assert.Error(t, err)
}

// ── PickMigrationTarget ─────────────────────────────────────────────────

// intnZero 让二选一固定抽前两个(TwoChoices:i = 0,j 从剩下的里取 0 再跳过 i = 1)。
func intnZero(int) int { return 0 }

func TestPickMigrationTarget_ExcludesCallerDrainingStaleAndFull(t *testing.T) {
	cs := candidates(1, 2, 3, 4, 5)
	cs[2].Stale = true
	cs[3].PlayerCount, cs[3].MaxConnections = 90, 100 // 90% 即满载
	draining := map[uint32]bool{1: true, 2: true}

	target, ok := PickMigrationTarget(cs, draining, 1, intnZero)
	require.True(t, ok)
	assert.EqualValues(t, 5, target.NodeID, "只剩 5 合格")
	assert.Equal(t, []uint32{1, 2, 3, 4, 5}, nodeIDsOf(cs), "不改入参")
}

// 迁移不回落:全部不合格时返回 false,不像登录那样回落全集。
func TestPickMigrationTarget_NoFallback(t *testing.T) {
	cs := candidates(1, 2, 3)
	cs[1].Stale = true
	cs[2].PlayerCount, cs[2].MaxConnections = 100, 100

	_, ok := PickMigrationTarget(cs, map[uint32]bool{1: true}, 1, intnZero)
	assert.False(t, ok, "2 停写、3 满载、1 是调用方:没有目标")

	_, ok = PickMigrationTarget(candidates(1), nil, 1, intnZero)
	assert.False(t, ok, "只有调用方自己")

	_, ok = PickMigrationTarget(nil, nil, 1, intnZero)
	assert.False(t, ok)
}

func TestPickMigrationTarget_TwoChoicesTakesTheLighter(t *testing.T) {
	cs := candidates(5, 6)
	cs[0].PlayerCount = 10
	cs[1].PlayerCount = 3

	target, ok := PickMigrationTarget(cs, nil, 1, intnZero)
	require.True(t, ok)
	assert.EqualValues(t, 6, target.NodeID)
}

// 逐台换版:1 在排空(旧构建),2 已换成新构建,3、4 还是旧构建。目标只在新构建里挑,哪怕它人更多。
func TestPickMigrationTarget_PrefersGatesNotOnTheOutgoingBuild(t *testing.T) {
	cs := candidates(1, 2, 3, 4)
	cs[0].Build, cs[1].Build, cs[2].Build, cs[3].Build = "v1", "v2", "v1", "v1"
	cs[1].PlayerCount = 50
	draining := map[uint32]bool{1: true}

	for _, intn := range []func(int) int{intnZero, func(n int) int { return n - 1 }} {
		target, ok := PickMigrationTarget(cs, draining, 1, intn)
		require.True(t, ok)
		assert.EqualValues(t, 2, target.NodeID, "3、4 与被排空的 1 同构建,迁过去还要再迁一次")
	}
	assert.Equal(t, []uint32{1, 2, 3, 4}, nodeIDsOf(cs), "不改入参")
}

// 偏好不能把人留在要下线的 gate 上:没有新构建的合格目标时用全部合格目标。
func TestPickMigrationTarget_BuildPreferenceFallsBackToAllEligible(t *testing.T) {
	// 第一台排空时还没有新构建的 gate。
	cs := candidates(1, 2, 3)
	cs[0].Build, cs[1].Build, cs[2].Build = "v1", "v1", "v1"
	cs[1].PlayerCount = 9
	target, ok := PickMigrationTarget(cs, map[uint32]bool{1: true}, 1, intnZero)
	require.True(t, ok)
	assert.EqualValues(t, 3, target.NodeID, "同构建的两台里二选一取轻的")

	// 新构建的那台满了:它在合格筛选里就被剔掉,剩下旧构建的照迁。
	cs = candidates(1, 2, 3)
	cs[0].Build, cs[1].Build, cs[2].Build = "v1", "v2", "v1"
	cs[1].PlayerCount, cs[1].MaxConnections = 100, 100
	target, ok = PickMigrationTarget(cs, map[uint32]bool{1: true}, 1, intnZero)
	require.True(t, ok)
	assert.EqualValues(t, 3, target.NodeID)
}

// 没上报构建就没有依据:排空中的 gate 没报构建时不产生偏好;目标没报构建时算首选。
func TestPickMigrationTarget_UnreportedBuildCarriesNoPreference(t *testing.T) {
	cs := candidates(1, 2, 3)
	cs[1].Build, cs[2].Build = "v2", "v1"
	cs[1].PlayerCount = 9
	target, ok := PickMigrationTarget(cs, map[uint32]bool{1: true}, 1, intnZero)
	require.True(t, ok)
	assert.EqualValues(t, 3, target.NodeID, "1 没报构建:没有外出构建,按负载挑")

	cs = candidates(1, 2, 3)
	cs[0].Build, cs[2].Build = "v1", "v1"
	target, ok = PickMigrationTarget(cs, map[uint32]bool{1: true}, 1, intnZero)
	require.True(t, ok)
	assert.EqualValues(t, 2, target.NodeID, "2 没报构建算首选,3 在外出构建上")
}

func TestOutgoingBuilds(t *testing.T) {
	cs := candidates(1, 2, 3, 4)
	cs[0].Build, cs[1].Build, cs[2].Build = "v1", "v2", "v3"

	assert.Empty(t, outgoingBuilds(cs, nil), "没有排空中的 gate")
	assert.Equal(t, map[string]bool{"v1": true, "v3": true}, outgoingBuilds(cs, map[uint32]bool{1: true, 3: true, 4: true}),
		"4 在排空但没报构建,不算")
}

// ── 票据 ────────────────────────────────────────────────────────────────

func TestSignMigrationGateToken_BindsPlayerAndNeverPinsZone(t *testing.T) {
	gate := &GateCandidate{NodeID: 9, IP: "gate-9.example.com", Port: 30009, ZoneID: 3}
	secret := []byte("0123456789abcdef0123456789abcdef")

	admit, err := SignMigrationGateToken(gate, secret, 5*time.Minute, 42)
	require.NoError(t, err)
	p := decodeGatePayload(t, admit)
	assert.EqualValues(t, 9, p.GetGateNodeId())
	assert.EqualValues(t, 3, p.GetZoneId())
	assert.EqualValues(t, 42, p.GetPlayerId(), "游戏中的连接绑定本人")
	assert.EqualValues(t, 0, p.GetTargetZoneId(), "钉了本 zone,到达时就不会回原场景(设计文档 §3 第 1 条)")
	assert.Len(t, p.GetHmacSessionKey(), hmacSessionKeyLen)
	assert.Equal(t, string(hmacHex(secret, admit.TokenPayload)), string(admit.TokenSignature))
	assert.Equal(t, "gate-9.example.com", admit.IP)
	assert.EqualValues(t, 30009, admit.Port)
	assert.Equal(t, p.GetExpireTimestamp(), admit.TokenDeadline)

	unbound, err := SignMigrationGateToken(gate, secret, 5*time.Minute, 0)
	require.NoError(t, err)
	assert.EqualValues(t, 0, decodeGatePayload(t, unbound).GetPlayerId(), "选角界面的连接不绑玩家")
}

// 普通 AssignGate 票据与重构前相同:不绑持票人,不钉 zone。
func TestSignGateToken_StaysUnbound(t *testing.T) {
	admit, err := SignGateToken(&GateCandidate{NodeID: 9, ZoneID: 3}, []byte("0123456789abcdef0123456789abcdef"), time.Minute)
	require.NoError(t, err)
	p := decodeGatePayload(t, admit)
	assert.EqualValues(t, 0, p.GetPlayerId())
	assert.EqualValues(t, 0, p.GetTargetZoneId())
}
