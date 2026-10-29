package barrierrestore

// AllowHomeFallback(2026-10-20 修复 r2-mz#1)的回归:copy 合服 / 撤销后的玩家没有落点记录,go/db 按 home 的 zone 库读他的行;
// 计划显式允许时,恢复工具接受「无落点记录且 home == zone」的玩家,但 home≠zone、落点键存在(含空串、他库、冻结)、
// 屏障与 SQL 不同版、缓存 / 证据 / 补投桶 / 合服围栏冲突仍一律拒绝,「落点记录仍不存在」在写屏障的同一段 Lua 里原子复核。
// 夹具沿用 restore_test.go 的录制 SQL 驱动与 miniredis(真实 Lua),route_test.go 的 splitFixture。

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
	"shared/placement"
	"shared/savebarrier"
)

func prepareFallback(t *testing.T, r Restorer, legacy bool) Plan {
	t.Helper()
	r.AllowHomeFallback = true
	p, err := r.Prepare(context.Background(), "zone_17_db", 17, "维护工单", []string{"7"}, legacy)
	require.NoError(t, err)
	return p
}

func TestHomeFallbackRestoresAPlayerWithoutPlacementRecord(t *testing.T) {
	ctx := context.Background()
	r, s, m := fixture(t, "8", "90")
	m.Del(placement.Key(7))
	// 缺省不放宽:无落点记录者照旧拒收,与修复前一致。
	_, err := r.Prepare(ctx, "zone_17_db", 17, "维护工单", []string{"7"}, false)
	require.Error(t, err)

	p := prepareFallback(t, r, false)
	require.Equal(t, Format, p.Format, "同实例本区恢复仍是 v1 计划")
	require.True(t, p.AllowHomeFallback, "放宽必须写进计划,随计划进 SHA-256")
	require.Equal(t, "", p.Players[0].Placement)
	require.Equal(t, "17", p.Players[0].HomeZone)

	// 执行以计划为准:Restorer 上的开关不参与 apply;同计划重投幂等。
	r.AllowHomeFallback = false
	for i := 0; i < 2; i++ {
		require.NoError(t, r.ApplyOne(ctx, p, 0), "第 %d 次", i+1)
	}
	v := savebarrier.Version(8, 90)
	for _, field := range []string{"pending", "player_database", "player_database_1"} {
		require.Equal(t, v, m.HGet(savebarrier.Key(7), field), field)
	}
	require.False(t, m.Exists(placement.Key(7)), "恢复工具不得替玩家补写落点记录")
	require.Zero(t, s.commits, "非零围栏恢复不改 SQL")
}

func TestHomeFallbackStillRejectsHomeMismatchAndPresentRecords(t *testing.T) {
	for name, change := range map[string]func(*miniredis.Miniredis){
		"home 不是本区":     func(m *miniredis.Miniredis) { m.Set(placement.HomeZoneKey(7), "18") },
		"home 非规范":      func(m *miniredis.Miniredis) { m.Set(placement.HomeZoneKey(7), "017") },
		"home 缺失":       func(m *miniredis.Miniredis) { m.Del(placement.HomeZoneKey(7)) },
		"落点键存在但为空串":    func(m *miniredis.Miniredis) { m.Set(placement.Key(7), "") },
		"落点指向他库":       func(m *miniredis.Miniredis) { m.Set(placement.Key(7), "18:1") },
		"落点冻结(搬库在途)":   func(m *miniredis.Miniredis) { m.Set(placement.Key(7), "17:1:frozen:run1") },
		"落点值畸形":        func(m *miniredis.Miniredis) { m.Set(placement.Key(7), "garbage") },
	} {
		t.Run(name, func(t *testing.T) {
			r, s, m := fixture(t, "8", "90")
			m.Del(placement.Key(7))
			change(m)
			r.AllowHomeFallback = true
			_, err := r.Prepare(context.Background(), "zone_17_db", 17, "维护工单", []string{"7"}, false)
			require.Error(t, err)
			require.False(t, m.Exists(savebarrier.Key(7)))
			require.Zero(t, s.commits)
		})
	}
}

func TestHomeFallbackRechecksAbsenceAtomically(t *testing.T) {
	ctx := context.Background()
	r, s, m := fixture(t, "8", "90")
	m.Del(placement.Key(7))
	p := prepareFallback(t, r, false)

	// 计划审定后有人补写了落点记录(钉落点 / 搬库):Go 侧路由复核先拒绝,无副作用。
	require.NoError(t, m.Set(placement.Key(7), "17:1"))
	before := m.Dump()
	require.Error(t, r.ApplyOne(ctx, p, 0))
	require.Equal(t, before, m.Dump())
	require.Zero(t, s.commits)

	// 跳过 Go 侧复核、直接走写屏障的那段 Lua:「落点记录仍不存在」在同一原子段里复核,照样拒绝且不写屏障。
	// (2026-10-22 注)这一段在修复前的 Lua 下也会拒绝:旧脚本拿 GET 结果与计划里的空串比,"17:1" 本来就不等于空串。
	// 所以它只防「EXISTS 分支被写成放行」;原子 EXISTS 分支确实存在,要靠正向用例
	// TestHomeFallbackRestoresAPlayerWithoutPlacementRecord 在旧脚本下变红来证明(落点键不存在时 GET 回 false,同样不等于空串)。
	err := r.inspect(ctx, p, p.Players[0], true, true, 1)
	require.ErrorContains(t, err, "authoritative route changed")
	require.False(t, m.Exists(savebarrier.Key(7)))

	// 记录撤掉之后,同一计划可以继续。
	m.Del(placement.Key(7))
	require.NoError(t, r.ApplyOne(ctx, p, 0))
	require.Equal(t, savebarrier.Version(8, 90), m.HGet(savebarrier.Key(7), "pending"))
}

func TestHomeFallbackLegacyRecheckAfterSQLCommit(t *testing.T) {
	r, s, m := fixture(t, "0", "0")
	m.Del(placement.Key(7))
	p := prepareFallback(t, r, true)
	// 零围栏迁移:SQL 先提交,重锁复核时落点记录已出现 → 不发布屏障;SQL 元数据已提交的前缀按同计划重入。
	s.onCommit = func() { m.Set(placement.Key(7), "17:1") }
	require.Error(t, r.ApplyOne(context.Background(), p, 0))
	require.Equal(t, 1, s.commits)
	require.False(t, m.Exists(savebarrier.Key(7)), "落点在重锁期间出现,不得发布屏障")
}

func TestHomeFallbackKeepsEveryOtherAtomicCheck(t *testing.T) {
	for name, change := range map[string]func(*miniredis.Miniredis, []string){
		"屏障与SQL不同版": func(m *miniredis.Miniredis, k []string) { m.HSet(k[7], "pending", savebarrier.Version(8, 91)) },
		"父快照仍在":     func(m *miniredis.Miniredis, k []string) { m.Set(k[2], "旧父快照") },
		"子缓存仍在":     func(m *miniredis.Miniredis, k []string) { m.Set(k[3], "旧子表") },
		"资产证据仍在":    func(m *miniredis.Miniredis, k []string) { m.Set(k[5], "旧证据") },
		"交接仍在":      func(m *miniredis.Miniredis, k []string) { m.Set(k[6], "8:1") },
		"补投桶未排空":    func(m *miniredis.Miniredis, k []string) { m.ZAdd(k[71], 0, "其他玩家") },
		"home被改走":    func(m *miniredis.Miniredis, k []string) { m.Set(k[72], "18") },
		"合服围栏仍在":    func(m *miniredis.Miniredis, k []string) { m.Set(k[74], "维护中") },
		"维护号改变":     func(m *miniredis.Miniredis, k []string) { m.Set(k[0], "新维护") },
		"归属改变":      func(m *miniredis.Miniredis, k []string) { m.Set(k[1], "10") },
	} {
		t.Run(name, func(t *testing.T) {
			r, s, m := fixture(t, "8", "90")
			m.Del(placement.Key(7))
			p := prepareFallback(t, r, false)
			change(m, recoveryKeys(17, 7))
			before := m.Dump()
			require.Error(t, r.ApplyOne(context.Background(), p, 0))
			require.Equal(t, before, m.Dump(), "拒绝留下副作用")
			require.Zero(t, s.commits)
		})
	}
}

func TestHomeFallbackPlanCannotBeStrippedOfTheOption(t *testing.T) {
	r, _, m := fixture(t, "8", "90")
	m.Del(placement.Key(7))
	p := prepareFallback(t, r, false)
	// 篡改计划去掉开关(CLI 另有 SHA-256 拦截):空落点不再被接受,执行前就拒绝。
	bad := p
	bad.AllowHomeFallback = false
	require.Error(t, bad.Validate())
	require.Error(t, r.ApplyOne(context.Background(), bad, 0))
	require.False(t, m.Exists(savebarrier.Key(7)))
}

func TestHomeFallbackSplitMappingOnlyForTheHomeZoneDatabase(t *testing.T) {
	ctx := context.Background()
	r, s, _, mapping, _ := splitFixture(t, false)
	mapping.Del(placement.Key(7))
	// 不放宽:Redis 权威路由下的空落点照旧拒绝。
	_, err := r.Prepare(ctx, "zone_17_db", 17, "维护工单", []string{"7"}, true)
	require.Error(t, err)

	r.AllowHomeFallback = true
	// 只回退到 home 自己的 zone 库:异区库(player_store)不能按 home 回退。
	_, err = r.Prepare(ctx, "player_store_1000000_db", 17, "维护工单", []string{"7"}, true)
	require.Error(t, err)
	p, err := r.Prepare(ctx, "zone_17_db", 17, "维护工单", []string{"7"}, true)
	require.NoError(t, err)
	require.Equal(t, FormatV2, p.Format)
	require.True(t, p.AllowHomeFallback)

	// 计划审定后 mapping 上出现落点记录:拒绝,SQL 与屏障都不动。
	require.NoError(t, mapping.Set(placement.Key(7), "17:1"))
	require.Error(t, r.ApplyOne(ctx, p, 0))
	require.Zero(t, s.commits)
	n, err := r.Redis.Exists(ctx, savebarrier.Key(7)).Result()
	require.NoError(t, err)
	require.Zero(t, n)

	mapping.Del(placement.Key(7))
	require.NoError(t, r.ApplyOne(ctx, p, 0))
	require.Equal(t, 1, s.commits, "零围栏迁移只提交一次")
	got, err := r.Redis.HGet(ctx, savebarrier.Key(7), "pending").Result()
	require.NoError(t, err)
	require.Equal(t, savebarrier.Version(9, 1), got)
}
