//go:build integration

package store_test

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"data_service/internal/store"
	"data_service/internal/store/storetest"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 号段发号(设计 §6.2 / §6.3)在真实 InnoDB 上的不变量:
//   1. 首次使用从 1 起,区间半开 [lo, hi),step 回写到行、0 继承行上的 step;
//   2. 32 goroutine × 50 次并发领段(含并发首次使用):互不相交、每个持有者单调、
//      全体恰好铺满 [1, 1+N*step) 无空洞;
//   3. step / biz_tag 校验拒绝在任何写入之前;
//   4. 2^55 上限:越界请求 fail-closed 且表状态零变更;表上的 CHECK 还兜底手工 SQL;
//   5. 行的生命周期(设计 §7.5 第 7 条):生产形态缺行即拒绝、零写入;行由迁移 BootstrapTags
//      幂等预建、绝不降低 max_id;player / guild 的水位被抬到消费表最大号 +1;dev 形态仍可补种。

// newIdSegmentStore 是 dev 形态(AllowAutoSeed=true)的 store:前四组不变量都以"首次使用
// 自动种行"为起点。生产形态用 newIdSegmentStoreOpts。
func newIdSegmentStore(t *testing.T, db *storetest.DB) *store.IdSegmentStore {
	t.Helper()
	return newIdSegmentStoreOpts(t, db, store.IdSegmentOptions{AllowAutoSeed: true})
}

func newIdSegmentStoreOpts(t *testing.T, db *storetest.DB, opts store.IdSegmentOptions) *store.IdSegmentStore {
	t.Helper()
	st, err := store.NewIdSegmentStore(db.Cfg, opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// migrateWithTags 再跑一次迁移并预建 tags 的行(迁移幂等,对已建好的表零变更)。
func migrateWithTags(t *testing.T, db *storetest.DB, tags ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	require.NoError(t, store.MigrateSchema(ctx, db.Cfg, store.MigrateOptions{BootstrapTags: tags}))
}

type idSegmentRow struct {
	maxID   uint64
	step    uint32
	version uint64
}

func readIdSegmentRow(t *testing.T, db *storetest.DB, bizTag string) (idSegmentRow, bool) {
	t.Helper()
	var r idSegmentRow
	err := db.Raw.QueryRow(`SELECT max_id, step, version FROM id_segment WHERE biz_tag = ?`, bizTag).
		Scan(&r.maxID, &r.step, &r.version)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false
	}
	require.NoError(t, err)
	return r, true
}

func TestIdSegmentStore_FirstUseStartsAtOneAndInheritsStep(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	st := newIdSegmentStore(t, db)
	ctx := context.Background()

	_, seeded := readIdSegmentRow(t, db, "player")
	require.False(t, seeded, "fresh database must have no player row")

	lo, hi, err := st.Allocate(ctx, "player", 100)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), lo, "first segment starts at 1 (0 stays invalid)")
	assert.Equal(t, uint64(101), hi, "half-open: hi = lo + step")

	row, ok := readIdSegmentRow(t, db, "player")
	require.True(t, ok, "first use must create the row")
	assert.Equal(t, idSegmentRow{maxID: 101, step: 100, version: 1}, row)

	// step=0 沿用行上的 step,且紧接上一段。
	lo, hi, err = st.Allocate(ctx, "player", 0)
	require.NoError(t, err)
	assert.Equal(t, uint64(101), lo)
	assert.Equal(t, uint64(201), hi)

	// 显式 step 覆盖并回写。
	lo, hi, err = st.Allocate(ctx, "player", 7)
	require.NoError(t, err)
	assert.Equal(t, uint64(201), lo)
	assert.Equal(t, uint64(208), hi)
	row, _ = readIdSegmentRow(t, db, "player")
	assert.Equal(t, idSegmentRow{maxID: 208, step: 7, version: 3}, row)

	// 不同 biz_tag 各自独立从 1 起。
	lo, hi, err = st.Allocate(ctx, "guild", 10)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), lo)
	assert.Equal(t, uint64(11), hi)
}

func TestIdSegmentStore_StepAndTagValidationWriteNothing(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	st := newIdSegmentStore(t, db)
	ctx := context.Background()

	cases := []struct {
		name string
		tag  string
		step uint32
		want error
	}{
		{"step above max", "player", store.IdSegmentMaxStep + 1, store.ErrIdSegmentInvalidStep},
		{"first use with step 0", "player", 0, store.ErrIdSegmentInvalidStep},
		{"empty tag", "", 1, store.ErrIdSegmentInvalidTag},
		{"uppercase tag", "Player", 1, store.ErrIdSegmentInvalidTag},
		{"dash in tag", "player-1", 1, store.ErrIdSegmentInvalidTag},
		{"tag over 64 chars", strings.Repeat("a", 65), 1, store.ErrIdSegmentInvalidTag},
		{"sql-ish tag", "player'; drop table id_segment;--", 1, store.ErrIdSegmentInvalidTag},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lo, hi, err := st.Allocate(ctx, tc.tag, tc.step)
			require.ErrorIs(t, err, tc.want)
			assert.Zero(t, lo)
			assert.Zero(t, hi)
		})
	}
	assert.Equal(t, int64(0), db.Count(t, "id_segment", ""), "rejected calls must not seed any row")

	// 行存在后 step 上限仍然生效,且不推进。
	_, _, err := st.Allocate(ctx, "player", 5)
	require.NoError(t, err)
	_, _, err = st.Allocate(ctx, "player", store.IdSegmentMaxStep+1)
	require.ErrorIs(t, err, store.ErrIdSegmentInvalidStep)
	row, _ := readIdSegmentRow(t, db, "player")
	assert.Equal(t, idSegmentRow{maxID: 6, step: 5, version: 1}, row)

	// 边界:恰好 64 字符、恰好 IdSegmentMaxStep 都合法。
	lo, hi, err := st.Allocate(ctx, strings.Repeat("z", 64), store.IdSegmentMaxStep)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), lo)
	assert.Equal(t, uint64(1)+uint64(store.IdSegmentMaxStep), hi)
}

func TestIdSegmentStore_CapAt2Pow55IsFailClosed(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	st := newIdSegmentStore(t, db)
	ctx := context.Background()
	limit := store.IdSegmentCap

	// 种一行贴着上限:还剩 10 个号。
	_, err := db.Raw.Exec(`INSERT INTO id_segment (biz_tag, max_id, step, version) VALUES ('item', ?, 5, 7)`, limit-10)
	require.NoError(t, err)

	lo, hi, err := st.Allocate(ctx, "item", 0)
	require.NoError(t, err)
	assert.Equal(t, limit-10, lo)
	assert.Equal(t, limit-5, hi)

	// 会越过上限:拒绝,且 max_id / version 都不动。
	_, _, err = st.Allocate(ctx, "item", 6)
	require.ErrorIs(t, err, store.ErrIdSegmentExhausted)
	row, _ := readIdSegmentRow(t, db, "item")
	assert.Equal(t, idSegmentRow{maxID: limit - 5, step: 5, version: 8}, row, "exhausted call must leave the row untouched")

	// 恰好到 limit-1 还能发(max_id 变成 limit-1 < cap)。
	lo, hi, err = st.Allocate(ctx, "item", 4)
	require.NoError(t, err)
	assert.Equal(t, limit-5, lo)
	assert.Equal(t, limit-1, hi)

	// hi == cap 也拒绝:max_id 会等于 2^55,违反 CHECK (max_id < 2^55)。
	_, _, err = st.Allocate(ctx, "item", 1)
	require.ErrorIs(t, err, store.ErrIdSegmentExhausted)
	row, _ = readIdSegmentRow(t, db, "item")
	assert.Equal(t, idSegmentRow{maxID: limit - 1, step: 4, version: 9}, row)

	// 表上的 CHECK 兜底绕过 store 的手工 SQL(MySQL 8.0.16+ 报 3819)。
	_, err = db.Raw.Exec(`UPDATE id_segment SET max_id = ? WHERE biz_tag = 'item'`, limit)
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		assert.Equal(t, uint16(3819), me.Number, "CHECK constraint must reject max_id = 2^55")
	} else {
		// 老版本 MySQL / TiDB < 7.2 只解析不校验 CHECK;应用层校验才是主闸,这里只记录。
		t.Logf("CHECK constraint not enforced by this server (err=%v); application-level cap is the guard", err)
	}
}

func TestIdSegmentStore_ConcurrentSegmentsAreDisjointAndMonotonic(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	st := newIdSegmentStore(t, db)

	const (
		goroutines = 32
		calls      = 50
		step       = 3
		bizTag     = "player"
	)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	type seg struct{ lo, hi uint64 }
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		all      []seg
		firstErr error
	)
	// 所有 goroutine 同时起跑,首次使用(种行)也在并发下发生。
	start := make(chan struct{})
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			var prevHi uint64
			for i := 0; i < calls; i++ {
				lo, hi, err := st.Allocate(ctx, bizTag, step)
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					return
				}
				// 同一持有者串行领段:必须严格向后,且段长恒等于 step。
				if lo < prevHi || hi != lo+step {
					mu.Lock()
					if firstErr == nil {
						firstErr = errors.New("non-monotonic or wrong-sized segment")
					}
					mu.Unlock()
					return
				}
				prevHi = hi
				mu.Lock()
				all = append(all, seg{lo, hi})
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	require.NoError(t, firstErr)
	require.Len(t, all, goroutines*calls)

	// 全体排序后必须严丝合缝铺满 [1, 1+N*step):无重叠、无空洞。
	sort.Slice(all, func(i, j int) bool { return all[i].lo < all[j].lo })
	expectLo := uint64(1)
	for i, s := range all {
		require.Equalf(t, expectLo, s.lo, "segment %d: gap or overlap before [%d,%d)", i, s.lo, s.hi)
		expectLo = s.hi
	}
	assert.Equal(t, uint64(1+goroutines*calls*step), expectLo)

	row, _ := readIdSegmentRow(t, db, bizTag)
	assert.Equal(t, idSegmentRow{maxID: 1 + goroutines*calls*step, step: step, version: goroutines * calls}, row,
		"version must count exactly one successful CAS per allocation")
}

// ── 行的生命周期(设计 §7.5 第 7 条)────────────────────────────────────────

// TestIdSegmentStore_UnknownTagRejectedWhenAutoSeedDisabled 生产形态:缺行即拒绝、零写入。
// 这是"全局库被重置后从 1 重发、login 静默覆盖别人角色行"的唯一防线。
func TestIdSegmentStore_UnknownTagRejectedWhenAutoSeedDisabled(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	st := newIdSegmentStoreOpts(t, db, store.IdSegmentOptions{AllowAutoSeed: false})
	ctx := context.Background()

	for _, step := range []uint32{100, 0} {
		lo, hi, err := st.Allocate(ctx, "player", step)
		require.ErrorIs(t, err, store.ErrIdSegmentUnknownTag, "step=%d", step)
		assert.Contains(t, err.Error(), "biz_tag=player", "error must name the tag for the operator")
		assert.Zero(t, lo)
		assert.Zero(t, hi)
	}
	assert.Equal(t, int64(0), db.Count(t, "id_segment", ""), "unknown tag must not create any row")

	// tag / step 校验仍在缺行判定之前(它们是调用方参数问题,不是缺行)。
	_, _, err := st.Allocate(ctx, "Player", 1)
	require.ErrorIs(t, err, store.ErrIdSegmentInvalidTag)
	_, _, err = st.Allocate(ctx, "player", store.IdSegmentMaxStep+1)
	require.ErrorIs(t, err, store.ErrIdSegmentInvalidStep)

	// 迁移预建之后同一个 store 立刻可发号,从 1 起。
	migrateWithTags(t, db, "player")
	lo, hi, err := st.Allocate(ctx, "player", 100)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), lo)
	assert.Equal(t, uint64(101), hi)
}

// TestIdSegmentStore_AutoSeedStillWorksWhenAllowed dev 形态:BootstrapTags 之外的 tag 首次使用照样补种。
func TestIdSegmentStore_AutoSeedStillWorksWhenAllowed(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	migrateWithTags(t, db, "player")
	st := newIdSegmentStoreOpts(t, db, store.IdSegmentOptions{AllowAutoSeed: true})
	ctx := context.Background()

	lo, hi, err := st.Allocate(ctx, "pet", 10)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), lo)
	assert.Equal(t, uint64(11), hi)
	row, ok := readIdSegmentRow(t, db, "pet")
	require.True(t, ok, "auto-seed must create the row")
	assert.Equal(t, idSegmentRow{maxID: 11, step: 10, version: 1}, row)
	assert.Equal(t, int64(2), db.Count(t, "id_segment", ""))
}

// TestMigrateSchema_BootstrapsIdSegmentRowsIdempotently 迁移按 BootstrapTags 预建五行
// (max_id=1, step=100, version=0),重复跑零变更,且**绝不降低**已推进的 max_id。
func TestMigrateSchema_BootstrapsIdSegmentRowsIdempotently(t *testing.T) {
	db := storetest.NewEmptyDB(t)
	ctx := context.Background()

	migrateWithTags(t, db, store.DefaultIdSegmentBootstrapTags...)
	require.Equal(t, int64(len(store.DefaultIdSegmentBootstrapTags)), db.Count(t, "id_segment", ""))
	for _, tag := range store.DefaultIdSegmentBootstrapTags {
		row, ok := readIdSegmentRow(t, db, tag)
		require.True(t, ok, "bootstrap must create biz_tag=%s", tag)
		assert.Equal(t, idSegmentRow{maxID: store.IdSegmentFirstID, step: store.IdSegmentBootstrapStep, version: 0}, row, tag)
	}

	// 幂等:第二次一字不变。
	migrateWithTags(t, db, store.DefaultIdSegmentBootstrapTags...)
	require.Equal(t, int64(len(store.DefaultIdSegmentBootstrapTags)), db.Count(t, "id_segment", ""))
	row, _ := readIdSegmentRow(t, db, "player")
	assert.Equal(t, idSegmentRow{maxID: 1, step: 100, version: 0}, row)

	// 推进水位后再迁移:绝不降低(否则每次部署都会把水位打回 1)。
	st := newIdSegmentStoreOpts(t, db, store.IdSegmentOptions{AllowAutoSeed: false})
	_, _, err := st.Allocate(ctx, "player", 100)
	require.NoError(t, err)
	_, err = db.Raw.Exec(`UPDATE id_segment SET max_id = ?, step = 7, version = 42 WHERE biz_tag = 'item'`, uint64(999_999))
	require.NoError(t, err)
	migrateWithTags(t, db, store.DefaultIdSegmentBootstrapTags...)
	row, _ = readIdSegmentRow(t, db, "player")
	assert.Equal(t, idSegmentRow{maxID: 101, step: 100, version: 1}, row, "bootstrap must never lower an advanced player row")
	row, _ = readIdSegmentRow(t, db, "item")
	assert.Equal(t, idSegmentRow{maxID: 999_999, step: 7, version: 42}, row, "bootstrap must not touch an existing item row")

	// 清单可以只是子集 / 含新 tag:只补缺的。
	migrateWithTags(t, db, "player", "pet")
	require.Equal(t, int64(len(store.DefaultIdSegmentBootstrapTags)+1), db.Count(t, "id_segment", ""))
	row, ok := readIdSegmentRow(t, db, "pet")
	require.True(t, ok)
	assert.Equal(t, idSegmentRow{maxID: 1, step: 100, version: 0}, row)

	// 一份手滑的 yaml:非法 tag 让整次迁移失败,而不是往主键表里塞垃圾行。
	migCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	err = store.MigrateSchema(migCtx, db.Cfg, store.MigrateOptions{BootstrapTags: []string{"player", "Bad-Tag"}})
	require.ErrorIs(t, err, store.ErrIdSegmentInvalidTag)
	_, ok = readIdSegmentRow(t, db, "Bad-Tag")
	assert.False(t, ok)
}

// TestMigrateSchema_RaisesIdSegmentFloorFromConsumingTables 从备份恢复 / 库被重置的兜底:
// 同库能查到消费表时,player / guild 的 max_id 被抬到 MAX(主键)+1;存量 snowflake 号
// (≥ 2^55)不参与;消费表不在本库就静默跳过;水位已经领先时零变更。
func TestMigrateSchema_RaisesIdSegmentFloorFromConsumingTables(t *testing.T) {
	db := storetest.NewEmptyDB(t)
	ctx := context.Background()

	// 一张"消费表"的最小替身:只要主键列在,与真表其余列无关。
	_, err := db.Raw.Exec("CREATE TABLE `player_database` (`player_id` bigint unsigned NOT NULL, `blob` mediumblob, PRIMARY KEY (`player_id`)) ENGINE=InnoDB")
	require.NoError(t, err)
	const snowflakePlayer = uint64(280_000_000_000_000_000) // ≈ 2.8e17,存量 snowflake 布局,> 2^55
	_, err = db.Raw.Exec("INSERT INTO `player_database` (`player_id`) VALUES (5), (250), (?)", snowflakePlayer)
	require.NoError(t, err)

	// 库刚被重建:bootstrap 种 player=1,随即被地板校验抬到 251;guild 表不在本库 → 保持 1。
	migrateWithTags(t, db, store.DefaultIdSegmentBootstrapTags...)
	row, _ := readIdSegmentRow(t, db, "player")
	assert.Equal(t, idSegmentRow{maxID: 251, step: 100, version: 1}, row, "player floor must be MAX(player_id < 2^55)+1, snowflake ids ignored")
	row, _ = readIdSegmentRow(t, db, "guild")
	assert.Equal(t, idSegmentRow{maxID: 1, step: 100, version: 0}, row, "guild table absent: skip silently")
	for _, tag := range []string{"item", "txlog", "snapshot"} {
		row, _ = readIdSegmentRow(t, db, tag)
		assert.Equal(t, idSegmentRow{maxID: 1, step: 100, version: 0}, row, "%s has no queryable consuming table", tag)
	}

	// 下一段从 251 起:5 / 250 不会被重发。
	st := newIdSegmentStoreOpts(t, db, store.IdSegmentOptions{AllowAutoSeed: false})
	lo, hi, err := st.Allocate(ctx, "player", 100)
	require.NoError(t, err)
	assert.Equal(t, uint64(251), lo)
	assert.Equal(t, uint64(351), hi)

	// MAX == max_id 也算落后("已分配到,不含"的不变量是 max_id > 已发出的最大号)。
	_, err = db.Raw.Exec(`UPDATE id_segment SET max_id = 250 WHERE biz_tag = 'player'`)
	require.NoError(t, err)
	migrateWithTags(t, db, store.DefaultIdSegmentBootstrapTags...)
	row, _ = readIdSegmentRow(t, db, "player")
	assert.Equal(t, uint64(251), row.maxID, "MAX == max_id must still raise to MAX+1")

	// 水位领先消费表:零变更(version 也不动)。
	_, err = db.Raw.Exec(`UPDATE id_segment SET max_id = 1000, version = 9 WHERE biz_tag = 'player'`)
	require.NoError(t, err)
	migrateWithTags(t, db, store.DefaultIdSegmentBootstrapTags...)
	row, _ = readIdSegmentRow(t, db, "player")
	assert.Equal(t, idSegmentRow{maxID: 1000, step: 100, version: 9}, row, "a counter ahead of the table must not be touched")

	// guild 表出现在本库之后,同样的规则。
	_, err = db.Raw.Exec("CREATE TABLE `guild` (`guild_id` bigint unsigned NOT NULL, `name` varchar(64) NOT NULL, PRIMARY KEY (`guild_id`)) ENGINE=InnoDB")
	require.NoError(t, err)
	_, err = db.Raw.Exec("INSERT INTO `guild` (`guild_id`, `name`) VALUES (40, 'a'), (?, 'b')", uint64(67_000_000_000_000_000))
	require.NoError(t, err)
	migrateWithTags(t, db, store.DefaultIdSegmentBootstrapTags...)
	row, _ = readIdSegmentRow(t, db, "guild")
	assert.Equal(t, idSegmentRow{maxID: 41, step: 100, version: 1}, row, "guild floor must follow MAX(guild_id < 2^55)+1")
	lo, hi, err = st.Allocate(ctx, "guild", 10)
	require.NoError(t, err)
	assert.Equal(t, uint64(41), lo)
	assert.Equal(t, uint64(51), hi)

	// 消费表在、但 id_segment 里没有这一行(不在 BootstrapTags):不由地板校验创建。
	_, err = db.Raw.Exec(`DELETE FROM id_segment WHERE biz_tag = 'guild'`)
	require.NoError(t, err)
	migrateWithTags(t, db, "player")
	_, ok := readIdSegmentRow(t, db, "guild")
	assert.False(t, ok, "floor check only raises existing rows; creation is bootstrap's job")
}
