package data

// 帮会经济 repo 与资产 Store 的测试(设计 05-economy.md §5.39,经 D2 / X-03 / X-04 / 07 §7.4.1 订正)。
//
// 分两层,与 guild_manage_repo_test.go 同一套纪律:
//
//  1. **纯单测**(不连库,任何环境都跑):手写列清单与 proto 字段序、状态映射、后台重试分类、
//     入参校验必须在碰库之前失败。这些是"编译期看不出来、运行期才炸"的那类错误的唯一看守。
//  2. **真库用例**(GUILD_TEST_MYSQL_DSN 未设即 Skip,库名只许 guild_test / guild_it_<pid>_<n>):
//     upsert 的 RowsAffected 语义、CAS 只赢一次、锁序下的对侧账、提前截止的过滤条件 —— 只有真 InnoDB 才有。
//     用例名带 Reserve / Finalize / TooMany / ListDue / Claim / UpgradeGuild / Accelerate / Cleanup / Recent / Resolve,
//     便于 `-run` 一次选中。
//  3. **并发锁序回归**(真库,名字带 EconomyLockOrder,另有 ① ② ③ 三个并发语义用例):2026-09-21 死锁修复契约 P6。
//     判据是 SHOW ENGINE INNODB STATUS 的 LATEST DETECTED DEADLOCK 前后比对(econDeadlockWatch;DSN 指向 TiDB 时
//     自动改看 INFORMATION_SCHEMA.CLUSTER_DEADLOCKS),因为 inTx / WithTxRetry 会把 1213 吸收掉重跑,光看返回值看不见死锁。
//     **测试账号要有 PROCESS 权限**。
//     2026-09-21 第二轮复核补了:预留 ‖ 同一 (player, stream, epoch) 前缀上的清理(审计 #15)、离帮持成员锁期间
//     并发预留的候选集完整性(审计 #10 修法第 2 点)、后台重排 / 毒行排在悲观写者之后(G-C2,TiDB 上才有鉴别力)。
//     第四轮(死锁复核 C2 / C6):首次建 seq 行的建行者在成员行上串行(首插者回滚不再引出 1213,钩子与 econDeadlockWatch
//     都严格为零)、事务内建行与 assetop 建行逐列同义;兑换孤儿退款 ‖ 别帮兑换同一商品(计数行守卫,TiDB 上才有鉴别力)。
//     这些用例都要求实例上没有别的测试并发(econDeadlockWatch 看的是整个实例),Codex 请用 `-p 1` 或独占实例。
//
// 时间一律用 testNowMs 派生的常量显式传入,不读真实墙钟(AGENTS §11.4)。
// 并发用例里 goroutine 只收集错误、不调 t.Fatal / require(那只会结束当前 goroutine、让用例挂住或漏报),
// 断言全部在 WaitGroup 之后的主 goroutine 里做。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	assetpb "proto/common/asset"
	rollbackpb "proto/common/rollback"
	pb "proto/guild"

	"shared/assetop"
	"shared/gameday"

	"guild/internal/constants"
)

// ── 纯单测 ────────────────────────────────────────────────────

// TestAssetOpColumnsCoverEveryProtoField:assetOpColumns 是手写列清单,插入参数与扫描顺序都按它排。
// 往 GuildAssetOpRecord 加字段却忘了同步,表现是"新列永远零值"或运行期 Scan 数量不符,编译期都不报 ——
// 这里用 proto 描述符机械比对列名与顺序,并核对 INSERT 的占位符数与参数个数。
func TestAssetOpColumnsCoverEveryProtoField(t *testing.T) {
	fields := (&pb.GuildAssetOpRecord{}).ProtoReflect().Descriptor().Fields()
	want := make([]string, 0, fields.Len())
	for i := 0; i < fields.Len(); i++ {
		want = append(want, string(fields.Get(i).Name()))
	}

	got := econSplitColumns(assetOpColumns)
	require.Equal(t, want, got, "assetOpColumns 的列名与顺序必须与 GuildAssetOpRecord 的字段序逐一一致")
	assert.Equal(t, assetOpColumnCount, len(got), "assetOpColumnCount 与列清单不符")
	assert.Len(t, assetOpInsertArgs(&pb.GuildAssetOpRecord{}), fields.Len(), "插入参数个数必须等于字段数")
	assert.Equal(t, fields.Len(), strings.Count(sqlInsertAssetOp, "?"), "INSERT 的占位符数必须等于字段数")
}

// econSplitColumns 把 "`a`, `b`" 拆成 ["a","b"]。
func econSplitColumns(cols string) []string {
	parts := strings.Split(cols, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.Trim(strings.TrimSpace(p), "`"))
	}
	return out
}

// TestStatusToRecordMapsEverySemanticStatus:assetop.Status 只表达语义,库值只在 StatusToRecord 一处映射。
// 漏一个分支就会把已终结的行落成 UNSPECIFIED;未知值落成 PENDING 会被循环反复领走,落成 APPLIED 是伪装成功。
func TestStatusToRecordMapsEverySemanticStatus(t *testing.T) {
	cases := []struct {
		in   assetop.Status
		want pb.GuildAssetOpStatus
	}{
		{assetop.StatusPending, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING},
		{assetop.StatusApplied, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED},
		{assetop.StatusRejected, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_REJECTED},
		{assetop.StatusAborted, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_ABORTED},
		{assetop.StatusAppliedPartial, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED_PARTIAL},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, StatusToRecord(tc.in), "StatusToRecord(%s)", tc.in)
	}
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_UNSPECIFIED, StatusToRecord(assetop.Status(250)))
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_UNSPECIFIED, StatusToRecord(assetop.Status(0)))
}

// TestPendingStatusIsNotZero:零值行(写坏 / 半截插入)绝不能被当成待办领走,所以 PENDING 的库值不能是 0。
func TestPendingStatusIsNotZero(t *testing.T) {
	require.NotZero(t, PendingStatus())
	assert.Equal(t, uint32(pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING), PendingStatus())
}

// TestGuildSeqTablesUsesGuildTables:表名与 guild_db.proto 的 OptionTableName 逐字一致,未决库值与 Store 同源。
func TestGuildSeqTablesUsesGuildTables(t *testing.T) {
	tables, err := GuildSeqTables()
	require.NoError(t, err)
	assert.Equal(t, "guild_player_op_seq", tables.SeqTable)
	assert.Equal(t, "guild_asset_op", tables.OpTable)
	assert.Equal(t, PendingStatus(), tables.PendingStatus)
}

// TestIsRetryableBackground:后台写把 1213 / 1205 / 9007 当可重试(包一层也要认得),其余一律不重试 ——
// 把撞唯一键判成可重试,会让一次注定失败的终结反复重放。
func TestIsRetryableBackground(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"死锁 1213", &mysqlDriver.MySQLError{Number: 1213}, true},
		{"锁等待超时 1205", &mysqlDriver.MySQLError{Number: 1205}, true},
		{"TiDB 写冲突 9007", &mysqlDriver.MySQLError{Number: 9007}, true},
		{"唯一键冲突 1062", &mysqlDriver.MySQLError{Number: 1062}, false},
		{"包了一层的锁等待", fmt.Errorf("terminal CAS: %w", &mysqlDriver.MySQLError{Number: 1205}), true},
		{"普通错误", errors.New("connection refused"), false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, isRetryableBackground(tc.err), tc.name)
	}
}

// TestNewEconomyRepoAndStoreRejectNilGuildRepo:没有 GuildRepo 就没有 inTx 与缓存失效,构造期就拒绝。
func TestNewEconomyRepoAndStoreRejectNilGuildRepo(t *testing.T) {
	_, err := NewEconomyRepo(nil)
	assert.Error(t, err)
	_, err = NewGuildAssetStore(nil)
	assert.Error(t, err)
}

// econNoDBRepo 造一个没有连接池的 GuildRepo:任何越过入参校验去碰库的路径都会当场 panic,
// 从而证明"畸形输入在碰库之前就失败"。
func econNoDBRepo(t *testing.T) *GuildRepo {
	t.Helper()
	return NewGuildRepo(nil, nil, time.Minute)
}

// TestFinalizeAndResolveRefuseNonFinalStatusBeforeTouchingStorage:终结只能落成四个终态之一。
// 落成 PENDING 等于没终结(循环会立刻再领走),落成 UNSPECIFIED 会让客服查不到结局。
func TestFinalizeAndResolveRefuseNonFinalStatusBeforeTouchingStorage(t *testing.T) {
	store, err := NewGuildAssetStore(econNoDBRepo(t))
	require.NoError(t, err)
	ctx := context.Background()
	for _, status := range []assetop.Status{assetop.StatusPending, assetop.Status(0), assetop.Status(250)} {
		finalized, err := store.Finalize(ctx, assetop.Op{OpID: 1}, status, assetop.Result{}, testNowMs)
		assert.Error(t, err, "Finalize status=%d", status)
		assert.False(t, finalized)

		resolved, err := store.ResolveManually(ctx,
			assetop.ManualResolution{OpID: 1, Final: status, Operator: "ops", Reason: "test"}, testNowMs)
		assert.Error(t, err, "ResolveManually status=%d", status)
		assert.False(t, resolved)
	}
}

// TestReserveRejectsMalformedInputBeforeTouchingStorage:编程错误(0 值 id / 空 payload / 0 上限 / 0 截止)
// 在碰库之前失败;一次买的份数超过周期限购是纯判断,同样不必碰库。
func TestReserveRejectsMalformedInputBeforeTouchingStorage(t *testing.T) {
	econ, err := NewEconomyRepo(econNoDBRepo(t))
	require.NoError(t, err)
	ctx := context.Background()

	good := DonationReserve{
		OpID: 1, PlayerID: 2, GuildID: 3, DonateID: 1, MinGuildLevel: 1, ContributionGain: 10, FundsGain: 1000,
		DailyLimit: 5, PeriodKey: 20260921, DeadlineMs: testNowMs + 600_000, LeaseUntilMs: testNowMs + 10_000,
		LeaseToken: 7, NowMs: testNowMs, Payload: []byte{0x08},
	}
	breakers := []struct {
		name    string
		breakIt func(*DonationReserve)
	}{
		{"op_id 为 0", func(in *DonationReserve) { in.OpID = 0 }},
		{"lease token 为 0", func(in *DonationReserve) { in.LeaseToken = 0 }},
		{"payload 为空", func(in *DonationReserve) { in.Payload = nil }},
		{"每日上限为 0", func(in *DonationReserve) { in.DailyLimit = 0 }},
		{"截止为 0 即永不中止", func(in *DonationReserve) { in.DeadlineMs = 0 }},
		{"租约不晚于 now", func(in *DonationReserve) { in.LeaseUntilMs = in.NowMs }},
		{"周期键为 0", func(in *DonationReserve) { in.PeriodKey = 0 }},
	}
	for _, b := range breakers {
		in := good
		b.breakIt(&in)
		_, err := econ.ReserveDonation(ctx, in)
		assert.Error(t, err, b.name)
	}

	shop := ShopReserve{
		OpID: 1, PlayerID: 2, GuildID: 3, GoodsID: 101, Count: 6, RequiredGuildLevel: 1, Cost: 180,
		LimitCount: 5, PeriodKey: 20260921, LeaseUntilMs: testNowMs + 10_000, LeaseToken: 7, NowMs: testNowMs,
		Payload: []byte{0x08},
	}
	_, err = econ.ReserveShopOrder(ctx, shop)
	assert.ErrorIs(t, err, ErrShopLimit, "份数超过周期限购,不碰库直接拒")

	shop.Count, shop.Cost = 1, 0
	_, err = econ.ReserveShopOrder(ctx, shop)
	assert.Error(t, err, "cost 为 0 会让扣帮贡的写入自检误判,必须拒")
	shop.Cost, shop.PeriodKey = 30, 0
	_, err = econ.ReserveShopOrder(ctx, shop)
	assert.Error(t, err, "限购份数与周期键必须同为 0 或同非 0")
}

// TestAccelerateRequiresNonZeroNow:deadline_ms = 0 在本表是"永不中止",传 0 的 now 会把所有未决捐献
// 改成永不中止 —— 必须在发语句之前拒绝(tx 为 nil,越过校验就会 panic)。
func TestAccelerateRequiresNonZeroNow(t *testing.T) {
	err := accelerateDonationDeadlines(context.Background(), nil, 1, []uint64{2}, 0)
	assert.Error(t, err)
}

// TestEconomyCandidateReadsTakeNoLocks:2026-09-21 死锁修复的两条静态纪律(不连库,任何环境都跑)。
//
//  1. "按二级条件找行"的候选读(提前截止、清理)必须是**普通读**:谁顺手补一个 FOR UPDATE,它就又变回
//     "经二级索引先锁二级项、再锁聚簇记录",与主键 CAS / 点删在同一行上反序成环(friend 审计 #5 / #10 / #15)。
//     执行计划 EXPLAIN 回归管不到它们(非锁定读走哪个索引都行),只能在这里钉文本。
//  2. guild_member 上本文件拥有的锁定读 / UPDATE 必须带 FORCE INDEX (PRIMARY):EXPLAIN 回归只在有真库时跑,
//     这里让"提示子句被删"在无库环境也红。sqlLockMemberRole 归 B2,由那边的回归看守。
func TestEconomyCandidateReadsTakeNoLocks(t *testing.T) {
	candidateReads := map[string]string{
		"accelerate candidates":     sqlSelectAccelerateCandidatesHead + placeholders(2) + sqlSelectAccelerateCandidatesTail,
		"sqlListCleanupTerminalOps": sqlListCleanupTerminalOps,
		"sqlListCleanupCounters":    sqlListCleanupCounters,
		// ensureSeqRowTx 的前置读:它在成员行锁之下只判存在,带锁定子句只会重复取锁(AllocateSeq 随后就锁这一行),
		// TiDB 下还会去锁不存在的 key。
		"sqlSeqRowExists": sqlSeqRowExists,
	}
	for name, query := range candidateReads {
		upper := strings.ToUpper(query)
		for _, clause := range []string{"FOR UPDATE", "FOR SHARE", "LOCK IN SHARE MODE"} {
			assert.NotContains(t, upper, clause, "%s 是候选读,必须是普通读:%s", name, query)
		}
	}

	memberStatements := map[string]string{
		"sqlLockMemberBalance":  sqlLockMemberBalance,
		"sqlDebitContribution":  sqlDebitContribution,
		"sqlCreditContribution": sqlCreditContribution,
		"sqlRefundContribution": sqlRefundContribution,
	}
	for name, query := range memberStatements {
		assert.Contains(t, query, "guild_member FORCE INDEX (PRIMARY)", "%s 必须强制走主键:%s", name, query)
	}

	// 点改 / 点删的定位必须是完整主键等值(复核条件可以另带)。
	assert.Contains(t, sqlAccelerateDonationDeadline, "WHERE `op_id` = ? AND")
	assert.Contains(t, sqlCleanupTerminalOp, "WHERE `op_id` = ? AND")
	assert.Contains(t, sqlCleanupCounter, "player_id = ? AND counter_kind = ? AND ref_id = ? AND period_key = ? AND")

	// op 行写前的点锁(C5 / V1:清理、提前截止、终结、重排、毒行共用)、计数行清理点锁(C5)与退款分支的 seq 行守卫(C6)
	// **只许**是完整主键等值 + FOR UPDATE,不许带任何复核条件:TiDB 只有这种形状才走 Point_Get 快路径("PRIMARY key → 行 key"),
	// 与退款 / 预留同序;带上条件就可能改走语句末尾整批并行加锁,又回到"各持一半"。复核条件留在随后的点改 / 点删里。
	// 这几条不能放进上面 candidateReads 的 map。"写之前必先点锁"的调用顺序由 point_lock_order_test.go 静态钉住。
	assert.Equal(t, "SELECT `op_id` FROM "+guildAssetOpTable+" WHERE `op_id` = ? FOR UPDATE", sqlLockAssetOp)
	assert.True(t, strings.HasSuffix(sqlLockCleanupCounter,
		"player_id = ? AND counter_kind = ? AND ref_id = ? AND period_key = ? FOR UPDATE"), sqlLockCleanupCounter)
	assert.True(t, strings.HasSuffix(sqlLockSeqGuard, "WHERE `player_id` = ? AND `stream` = ? FOR UPDATE"), sqlLockSeqGuard)
	// 事务内建 seq 行必须仍是"已存在即空操作"的 INSERT IGNORE、next_seq 从 1 起(与 assetop.EnsureSeqRow 同义)。
	assert.True(t, strings.HasPrefix(sqlEnsureSeqRow, "INSERT IGNORE INTO "+guildPlayerOpSeqTable+" "), sqlEnsureSeqRow)
	assert.Contains(t, sqlEnsureSeqRow, "VALUES (?, ?, 1, ?, ?)")
}

// TestCounterRefundIsTheOnlyRefundRule:退次数 / 退限购的判定只此一处(counterRefund),lockCounterparty 的 seq 行守卫
// 与 applyCounterparty 的退款都按它走 —— 两边条件一分叉,就会出现"退了款却没先锁 seq 行"(C6 的环回来)。
// 纯函数,不连库。
func TestCounterRefundIsTheOnlyRefundRule(t *testing.T) {
	donate := assetOpImmutable{Kind: pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE, RefCount: 1, PeriodKey: 20260921}
	shop := assetOpImmutable{Kind: pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_SHOP, RefCount: 3, PeriodKey: 20260921}
	activity := assetOpImmutable{Kind: pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_ACTIVITY_REWARD, RefCount: 1, PeriodKey: 20260921}

	for _, status := range []assetop.Status{assetop.StatusRejected, assetop.StatusAborted} {
		kind, n, ok := counterRefund(donate, status)
		assert.True(t, ok, "捐献 %s 要退次数", status)
		assert.Equal(t, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_DONATE, kind)
		assert.Equal(t, uint32(1), n)

		kind, n, ok = counterRefund(shop, status)
		assert.True(t, ok, "兑换 %s 要退限购", status)
		assert.Equal(t, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_SHOP, kind)
		assert.Equal(t, uint32(3), n)

		_, _, ok = counterRefund(activity, status)
		assert.False(t, ok, "活动发奖不占计数行")
	}
	for _, status := range []assetop.Status{assetop.StatusApplied, assetop.StatusAppliedPartial} {
		_, _, ok := counterRefund(donate, status)
		assert.False(t, ok, "%s 不退", status)
		_, _, ok = counterRefund(shop, status)
		assert.False(t, ok, "%s 不退", status)
	}
	unlimited := shop
	unlimited.PeriodKey = 0
	_, _, ok := counterRefund(unlimited, assetop.StatusRejected)
	assert.False(t, ok, "不限购的商品当初没占计数行")
	zeroCount := shop
	zeroCount.RefCount = 0
	_, _, ok = counterRefund(zeroCount, assetop.StatusRejected)
	assert.False(t, ok, "0 份不退")
}

// TestCounterCleanupCutoffsSparePreviousPeriod:计数行清理的截止键删不到"切周 / 切日后仍可能被在途预留 upsert 写的上一周期行"
// (minCounterCleanupAge,死锁复核 C5 补遗)。否则 TiDB 下清理的 Point_Get 与 upsert 的语句末尾并行加锁会在同一行上各持一半,
// 清理先删了还会让在途预留重建一行 used_count = n 的旧周期计数、绕过一次限购。纯函数,不连库。
func TestCounterCleanupCutoffsSparePreviousPeriod(t *testing.T) {
	const day = 24 * time.Hour
	const minRetention = 7 * day // config 的 minRetentionDays(未导出,按值抄):配置允许的最短计数保留期
	// 2026-09-14 是周一,05:00 切周(与 shared/gameday 单测同一锚点)。
	weekSwitch := time.Date(2026, 9, 14, gameday.ResetHour, 0, 0, 0, gameday.Zone)
	before, after := weekSwitch.Add(-time.Second), weekSwitch.Add(time.Second)

	// 前提:不夹紧时,保留期 7 天在切周后 1 秒算出的周截止恰是上一周 —— 这正是要防的情形。
	require.Equal(t, gameday.WeekKey(before), gameday.WeekKey(after.Add(-minRetention)))

	dayCutoff, weekCutoff := counterCleanupCutoffs(after, minRetention)
	assert.Less(t, weekCutoff, gameday.WeekKey(before), "切周后 1 秒:上一周的计数行不能进清理范围")
	assert.Less(t, dayCutoff, gameday.DayKey(before), "切日后 1 秒:上一日的计数行不能进清理范围")

	// 逐小时扫 6 周(含 2026-W53 → 2027-W01 跨 ISO 年):任意时刻,24h 内开始的请求所写的日键 / 周键都严格大于截止键。
	scanFrom := time.Date(2026, 12, 14, 0, 30, 0, 0, gameday.Zone)
	for now := scanFrom; now.Before(scanFrom.Add(6 * 7 * day)); now = now.Add(time.Hour) {
		for _, retention := range []time.Duration{time.Nanosecond, minRetention} {
			d, w := counterCleanupCutoffs(now, retention)
			recent := now.Add(-day)
			if w >= gameday.WeekKey(recent) || d >= gameday.DayKey(recent) {
				t.Fatalf("now=%v retention=%v:截止 (day=%d, week=%d) 覆盖了 24h 内开始的请求的周期 (day=%d, week=%d)",
					now, retention, d, w, gameday.DayKey(recent), gameday.WeekKey(recent))
			}
		}
	}

	// 保留期长于下限时不夹紧:截止就是 now − retention 所在的周期。
	d, w := counterCleanupCutoffs(after, 30*day)
	assert.Equal(t, gameday.DayKey(after.Add(-30*day)), d)
	assert.Equal(t, gameday.WeekKey(after.Add(-30*day)), w)
}

// ── 真库夹具 ──────────────────────────────────────────────────

const (
	econLeaseMs    uint64 = 10_000
	econDeadlineMs uint64 = 600_000
	econDayMs      uint64 = 24 * 3_600_000
)

// econDayKey 是 testNowMs 所在的游戏日键。
var econDayKey = gameday.DayKey(time.UnixMilli(int64(testNowMs)))

// econGuildLevels 逐行抄自 generated/tables/guildlevel.json(2026-09-21 实值):data 包不加载配表,
// 升级用例的期望值(1→2 扣 20000、上限 35)就以这份为准;配表改了这里要同步。
var econGuildLevels = map[uint32]struct {
	cost       uint64
	maxMembers uint32
}{
	1: {20000, 30}, 2: {50000, 35}, 3: {100000, 40}, 4: {180000, 45}, 5: {300000, 50},
	6: {460000, 60}, 7: {680000, 70}, 8: {960000, 80}, 9: {1300000, 90}, 10: {0, 100},
}

func econLevelLookup(level uint32) (uint64, uint32, bool) {
	row, ok := econGuildLevels[level]
	return row.cost, row.maxMembers, ok
}

// econFixture 把真库用例的装配收拢:经济 repo 与资产 store 挂在同一个 GuildRepo 上(与生产接线一致)。
type econFixture struct {
	ctx   context.Context
	db    *sql.DB
	repo  *GuildRepo
	econ  *EconomyRepo
	store *GuildAssetStore
}

func openEconomyFixture(t *testing.T) econFixture {
	t.Helper()
	ctx, db, repo := openGuildIntegrationRepo(t)
	econ, err := NewEconomyRepo(repo)
	require.NoError(t, err)
	store, err := NewGuildAssetStore(repo)
	require.NoError(t, err)
	return econFixture{ctx: ctx, db: db, repo: repo, econ: econ, store: store}
}

func econDonatePayload(t *testing.T, amount uint64) []byte {
	t.Helper()
	payload, err := proto.Marshal(&assetpb.AssetBundle{
		Currencies: []*assetpb.CurrencyAmount{{CurrencyType: 0, Amount: amount}},
	})
	require.NoError(t, err)
	return payload
}

func econShopPayload(t *testing.T, itemID, count uint32) []byte {
	t.Helper()
	payload, err := proto.Marshal(&assetpb.AssetBundle{
		Items: []*assetpb.ItemGrant{{ConfigId: itemID, Count: count}},
	})
	require.NoError(t, err)
	return payload
}

// econDonation 造一笔"银两小捐"(GuildDonate 第 1 行的数值):扣 10000、帮贡 10、资金 1000、每日 5 次。
func econDonation(t *testing.T, opID, playerID, guildID, now uint64) DonationReserve {
	t.Helper()
	return DonationReserve{
		OpID: opID, PlayerID: playerID, GuildID: guildID,
		DonateID: 1, MinGuildLevel: 1, ContributionGain: 10, FundsGain: 1000, DailyLimit: 5,
		PeriodKey: econDayKey, DeadlineMs: now + econDeadlineMs, LeaseUntilMs: now + econLeaseMs,
		LeaseToken: opID | 1<<62, NowMs: now, Payload: econDonatePayload(t, 10000),
	}
}

// econShopOrder 造一笔商品 101 的兑换;limit > 0 时按日限购。
func econShopOrder(t *testing.T, opID, playerID, guildID uint64, count uint32, cost uint64, limit uint32, now uint64) ShopReserve {
	t.Helper()
	var periodKey uint32
	if limit > 0 {
		periodKey = econDayKey
	}
	return ShopReserve{
		OpID: opID, PlayerID: playerID, GuildID: guildID,
		GoodsID: 101, Count: count, RequiredGuildLevel: 1, Cost: cost, LimitCount: limit, PeriodKey: periodKey,
		LeaseUntilMs: now + econLeaseMs, LeaseToken: opID | 1<<62, NowMs: now,
		Payload: econShopPayload(t, 15, 5*count),
	}
}

func econSetContribution(t *testing.T, f econFixture, guildID, playerID, total, balance uint64) {
	t.Helper()
	mustExec(t, f.ctx, f.db,
		"UPDATE guild_member SET contribution_total=?, contribution_balance=? WHERE guild_id=? AND player_id=?",
		total, balance, guildID, playerID)
}

func econFunds(t *testing.T, f econFixture, guildID uint64) uint64 {
	t.Helper()
	var funds uint64
	require.NoError(t, f.db.QueryRowContext(f.ctx, "SELECT funds FROM guild WHERE guild_id=?", guildID).Scan(&funds))
	return funds
}

// econContribution 读帮贡两列;成员行不在时 found=false。
func econContribution(t *testing.T, f econFixture, guildID, playerID uint64) (total, balance uint64, found bool) {
	t.Helper()
	total, balance, found, err := f.econ.MemberContribution(f.ctx, guildID, playerID)
	require.NoError(t, err)
	return total, balance, found
}

// econCounterUsed 读一行计数;行不存在时 found=false。
func econCounterUsed(t *testing.T, f econFixture, playerID uint64, kind pb.GuildDailyCounterKind, refID, periodKey uint32) (uint32, bool) {
	t.Helper()
	var used uint32
	err := f.db.QueryRowContext(f.ctx,
		"SELECT used_count FROM guild_daily_counter WHERE player_id=? AND counter_kind=? AND ref_id=? AND period_key=?",
		playerID, int32(kind), refID, periodKey).Scan(&used)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false
	}
	require.NoError(t, err)
	return used, true
}

// econRecord 读一整行(含 AssetOpRow 不暴露的 durable / lease / resolved_by 等列)。
func econRecord(t *testing.T, f econFixture, opID uint64) *pb.GuildAssetOpRecord {
	t.Helper()
	rec, found, err := f.store.getRecord(f.ctx, opID)
	require.NoError(t, err)
	require.True(t, found, "op %d 应当存在", opID)
	return rec
}

func econOpExists(t *testing.T, f econFixture, opID uint64) bool {
	t.Helper()
	_, found, err := f.store.getRecord(f.ctx, opID)
	require.NoError(t, err)
	return found
}

// econInsertOp 直接插一行指令(走生产的 INSERT 语句与参数顺序),用来造 ListDue / 清理 / 排序要的各种状态。
func econInsertOp(t *testing.T, f econFixture, rec *pb.GuildAssetOpRecord) {
	t.Helper()
	_, err := f.db.ExecContext(f.ctx, sqlInsertAssetOp, assetOpInsertArgs(rec)...)
	require.NoError(t, err, "insert op %d", rec.GetOpId())
}

// econShopRecord 造一行商店指令的骨架;调用方再改状态 / 次数 / 时刻。
func econShopRecord(t *testing.T, opID, playerID, epoch, seq uint64) *pb.GuildAssetOpRecord {
	t.Helper()
	return &pb.GuildAssetOpRecord{
		OpId: opID, PlayerId: playerID, Stream: uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT),
		Seq: seq, GuildId: 7700, Kind: pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_SHOP,
		Status:        pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING,
		NextAttemptMs: testNowMs - 10, Payload: econShopPayload(t, 15, 5), RefId: 101, RefCount: 1,
		CreatedMs: testNowMs - 1000, UpdatedMs: testNowMs - 1000,
		TxType: uint32(rollbackpb.TransactionType_TX_GUILD_SHOP), StreamEpoch: epoch,
	}
}

// econCaptureOrphans 把 orphan 计数换成记录器,返回读取函数。
func econCaptureOrphans(t *testing.T) func() []string {
	t.Helper()
	var got []string
	original := recordAssetOrphan
	recordAssetOrphan = func(kind, what string) { got = append(got, kind+"/"+what) }
	t.Cleanup(func() { recordAssetOrphan = original })
	return func() []string { return got }
}

// econAppliedResult 是 scene 已落盘的全额应用答复。
func econAppliedResult() assetop.Result {
	return assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED, Durable: true}
}

// econRejectedResult 是 scene 已落盘的永久拒绝答复。
func econRejectedResult() assetop.Result {
	return assetop.Result{
		Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED,
		Reason:  assetop.ReasonCurrencyInsufficient,
		Durable: true,
	}
}

// econLockOrderBudget:循环多轮的并发回归自带的总预算。openGuildIntegrationRepo 给的 ctx 只有 20s
// (含重建 schema),多轮对撞放在它上面会在慢机器上被截断成"半截通过",所以这类用例换成自己的 ctx。
const econLockOrderBudget = 120 * time.Second

// econRunTogether 用一个起跑闸同时放行若干闭包,返回与入参一一对应的错误。
// 闭包里只许调被测接口、写自己那一格结果,**不许**调 t.Fatal / require(契约 P6);断言由调用方在返回后做。
// 与 guild_manage_repo_test.go 的 runConcurrently 同形;单独写一份是为了不依赖另一个批次文件里的助手。
func econRunTogether(fns ...func() error) []error {
	errs := make([]error, len(fns))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, fn := range fns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = fn()
		}()
	}
	close(start)
	wg.Wait()
	return errs
}

// econDeadlockWatch 是并发锁序回归的死锁判据(2026-09-21 死锁修复契约 P6):
// 比对 SHOW ENGINE INNODB STATUS 里 LATEST DETECTED DEADLOCK 一段的前后文本。
//
// 为什么不看返回值或重试计数:inTx 与 assetop.WithTxRetry 都会把 1213 吸收掉整事务重跑,调用方只看得见最终成功;
// guild_tx_deadlock_total 在测试进程里不累加(go-zero 的指标在未启用 Prometheus 时 Inc 是空操作),也没有注入钩子。
// InnoDB 自己记下的"最近一次死锁"不经过任何一层重试,是唯一看得见被吸收掉的 1213 的地方。
//
// 判定宁红勿绿:
//   - 前后文本相同 → 期间没有任何死锁,通过;
//   - 变了且现场里出现本测试库名 → 本库死锁,红;
//   - 变了但现场不是本库 → 同实例上别的测试覆盖了现场,证明不了本库零死锁,同样红(提示单独重跑)。
//
// 测试账号必须有 PROCESS 权限,没有时直接红而不是 Skip(Skip 会被当成通过)。
//
// DSN 指向 TiDB 时(VERSION() 含 "TiDB";完整性复核 G3):TiDB 不支持 SHOW ENGINE INNODB STATUS,改比对
// INFORMATION_SCHEMA.CLUSTER_DEADLOCKS 各实例的最大 DEADLOCK_ID(见 econTiDBDeadlockMarker)。两点前提,Codex 跑 TiDB 时要配上:
//   - pessimistic-txn.deadlock-history-collect-retryable = true:默认只记"不可重试"的死锁;语句级被 TiDB 内部重试吸收的
//     那一侧(例如 C6 的 IODKU 一方)不记,判据就看不见它;
//   - 集群上没有别的测试并发:CLUSTER_DEADLOCKS 是集群级视图,不分库。
type econDeadlockWatch struct {
	db     *sql.DB
	schema string
	tidb   bool
	before string
}

func econWatchDeadlocks(t *testing.T, f econFixture) econDeadlockWatch {
	t.Helper()
	var schema, version string
	require.NoError(t, f.db.QueryRowContext(f.ctx, "SELECT DATABASE(), VERSION()").Scan(&schema, &version))
	w := econDeadlockWatch{db: f.db, schema: schema, tidb: strings.Contains(version, "TiDB")}
	w.before = w.marker(t, f.ctx)
	return w
}

// marker 取"到目前为止最近一次死锁"的指纹:MySQL 是 LATEST DETECTED DEADLOCK 一段,TiDB 是各实例的最大 DEADLOCK_ID。
func (w econDeadlockWatch) marker(t *testing.T, ctx context.Context) string {
	t.Helper()
	if w.tidb {
		return econTiDBDeadlockMarker(t, ctx, w.db)
	}
	return econLatestDeadlock(t, ctx, w.db)
}

// assertNone 在全部并发都结束之后、主 goroutine 里调用。
func (w econDeadlockWatch) assertNone(t *testing.T, ctx context.Context, what string) {
	t.Helper()
	after := w.marker(t, ctx)
	if after == w.before {
		return
	}
	if w.tidb {
		t.Fatalf("%s:TiDB 记录了新的死锁(CLUSTER_DEADLOCKS 指纹 %q → %q)。它是集群级视图、不分库,若集群上有别的测试并发,"+
			"请独占集群重跑确认。现场:\n%s", what, w.before, after, econTiDBDeadlockDump(ctx, w.db))
	}
	if strings.Contains(after, "`"+w.schema+"`") {
		t.Fatalf("%s:本测试库 %s 出现了新的 InnoDB 死锁(1213,已被事务重试吸收,返回值看不出来)。现场:\n%s",
			what, w.schema, after)
	}
	t.Fatalf("%s:期间同一实例记录了一次别库的死锁,LATEST DETECTED DEADLOCK 已被覆盖,无法证明本库零死锁;"+
		"请在没有其它测试并发的实例上单独重跑本用例。现场:\n%s", what, after)
}

// econLatestDeadlock 取 LATEST DETECTED DEADLOCK 一段(含时间戳,任何新死锁都会让它变);没有这一段时返回空串。
func econLatestDeadlock(t *testing.T, ctx context.Context, db *sql.DB) string {
	t.Helper()
	var engine, name, status sql.NullString
	err := db.QueryRowContext(ctx, "SHOW ENGINE INNODB STATUS").Scan(&engine, &name, &status)
	require.NoError(t, err, "SHOW ENGINE INNODB STATUS 失败:测试账号需要 PROCESS 权限,没有它死锁判据无从成立(不许改成 Skip)")
	const header = "LATEST DETECTED DEADLOCK"
	start := strings.Index(status.String, header)
	if start < 0 {
		return ""
	}
	section := status.String[start:]
	if end := strings.Index(section, "\nTRANSACTIONS\n"); end >= 0 {
		section = section[:end]
	}
	return section
}

// econTiDBDeadlockMarker 返回 TiDB 各实例死锁历史的最大 DEADLOCK_ID(形如 "实例#id,实例#id")。
// 不数行数:每个实例的死锁历史是环形缓冲(容量 pessimistic-txn.deadlock-history-capacity,默认 10),条数封顶后不再增长;
// DEADLOCK_ID 在实例内自增,任何一次新死锁都会让它变。没有任何记录时返回空串。
func econTiDBDeadlockMarker(t *testing.T, ctx context.Context, db *sql.DB) string {
	t.Helper()
	rows, err := db.QueryContext(ctx,
		"SELECT INSTANCE, MAX(DEADLOCK_ID) FROM INFORMATION_SCHEMA.CLUSTER_DEADLOCKS GROUP BY INSTANCE ORDER BY INSTANCE")
	require.NoError(t, err, "读 INFORMATION_SCHEMA.CLUSTER_DEADLOCKS 失败:测试账号需要 PROCESS 权限,没有它死锁判据无从成立(不许改成 Skip)")
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var (
			instance string
			maxID    sql.NullInt64
		)
		require.NoError(t, rows.Scan(&instance, &maxID))
		parts = append(parts, fmt.Sprintf("%s#%d", instance, maxID.Int64))
	}
	require.NoError(t, rows.Err())
	return strings.Join(parts, ",")
}

// econTiDBDeadlockDump 把 CLUSTER_DEADLOCKS 全部行按"列=值"印出来当现场(列名随版本可能增减,这里不写死)。
// 只在判红时调用;读失败不再 require,把错误写进现场文本,免得掩盖真正的判红原因。
func econTiDBDeadlockDump(ctx context.Context, db *sql.DB) string {
	rows, err := db.QueryContext(ctx, "SELECT * FROM INFORMATION_SCHEMA.CLUSTER_DEADLOCKS")
	if err != nil {
		return fmt.Sprintf("(读现场失败:%v)", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return fmt.Sprintf("(读列名失败:%v)", err)
	}
	var b strings.Builder
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			fmt.Fprintf(&b, "(扫描失败:%v)\n", err)
			break
		}
		for i, col := range cols {
			fmt.Fprintf(&b, "%s=%s ", col, vals[i].String)
		}
		b.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(&b, "(遍历失败:%v)\n", err)
	}
	return b.String()
}

// econInsertOpsBulk 一条多行 INSERT 插一批指令(生产的列清单与参数顺序),给需要成百上千行夹具的并发回归用。
// 每行 28 个占位符,调用方一批别超过 2000 行(MySQL 单语句占位符上限 65535)。
func econInsertOpsBulk(t *testing.T, f econFixture, recs []*pb.GuildAssetOpRecord) {
	t.Helper()
	if len(recs) == 0 {
		return
	}
	require.LessOrEqual(t, len(recs)*assetOpColumnCount, 65535, "一批行数过多,占位符会超上限")
	tuple := "(" + placeholders(assetOpColumnCount) + ")"
	var b strings.Builder
	b.WriteString("INSERT INTO " + guildAssetOpTable + " (" + assetOpColumns + ") VALUES ")
	args := make([]any, 0, len(recs)*assetOpColumnCount)
	for i, rec := range recs {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(tuple)
		args = append(args, assetOpInsertArgs(rec)...)
	}
	mustExec(t, f.ctx, f.db, b.String(), args...)
}

// econDonateRecord 造一行捐献指令的骨架(未决、截止在 testNowMs 之后);调用方再改状态 / 时刻。
// payload 由调用方先造好传进来:本函数也会在准备并发夹具时被循环调用,不在里面反复 Marshal。
func econDonateRecord(opID, playerID, guildID, epoch, seq uint64, payload []byte) *pb.GuildAssetOpRecord {
	return &pb.GuildAssetOpRecord{
		OpId: opID, PlayerId: playerID, Stream: uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT),
		Seq: seq, GuildId: guildID, Kind: pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE,
		Status:        pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING,
		NextAttemptMs: testNowMs + econLeaseMs, DeadlineMs: testNowMs + econDeadlineMs, Payload: payload,
		RefId: 1, RefCount: 1, PeriodKey: econDayKey, ContributionDelta: 10, FundsDelta: 1000,
		CreatedMs: testNowMs, UpdatedMs: testNowMs,
		TxType: uint32(rollbackpb.TransactionType_TX_GUILD_DONATE), StreamEpoch: epoch,
	}
}

// econOpCountInRange 数 op_id ∈ [lo, hi] 的行(按主键范围,不依赖状态)。
func econOpCountInRange(t *testing.T, f econFixture, lo, hi uint64) int {
	t.Helper()
	var n int
	require.NoError(t, f.db.QueryRowContext(f.ctx,
		"SELECT COUNT(*) FROM guild_asset_op WHERE op_id BETWEEN ? AND ?", lo, hi).Scan(&n))
	return n
}

// econNextSeq 读 seq 行的 next_seq。
func econNextSeq(t *testing.T, f econFixture, playerID uint64, stream assetpb.AssetOpStream) uint64 {
	t.Helper()
	var next uint64
	require.NoError(t, f.db.QueryRowContext(f.ctx,
		"SELECT next_seq FROM guild_player_op_seq WHERE player_id=? AND stream=?", playerID, uint32(stream)).Scan(&next))
	return next
}

// ── T-D 捐献预留 ─────────────────────────────────────────────

// TestReserveDonation_WritesPendingRowAndOccupiesCount:成功路径写满 28 列、占一次今日次数,
// 可空的三列写成空值而不是 NULL,合服闸门拿到的是事务内读到的 zone。
func TestReserveDonation_WritesPendingRowAndOccupiesCount(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7501
		leader  uint64 = 8501
		donor   uint64 = 8502
		zone    uint32 = 3
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, zone, 1, 50, leader, map[uint64]uint32{donor: constants.RoleMember})

	var fencedZone uint32
	in := econDonation(t, 9_500_001, donor, guildID, testNowMs)
	in.Fence = func(_ context.Context, zoneID uint32) error { fencedZone = zoneID; return nil }
	res, err := f.econ.ReserveDonation(f.ctx, in)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), res.Seq, "首笔 seq 从 1 起")
	assert.Equal(t, testNowMs, res.StreamEpoch, "纪元 = 事务内建 seq 行的时刻")
	assert.Equal(t, zone, fencedZone, "闸门必须拿事务内读到的 guild.zone_id")

	rec := econRecord(t, f, in.OpID)
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING, rec.GetStatus())
	assert.Equal(t, pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE, rec.GetKind())
	assert.Equal(t, uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT), rec.GetStream())
	assert.Equal(t, guildID, rec.GetGuildId())
	assert.Equal(t, in.DeadlineMs, rec.GetDeadlineMs())
	assert.Equal(t, in.LeaseUntilMs, rec.GetLeaseUntilMs())
	assert.Equal(t, in.LeaseUntilMs, rec.GetNextAttemptMs(), "同步投递握着租约,租约到期前循环不该碰它")
	assert.Equal(t, in.LeaseToken, rec.GetLeaseToken())
	assert.Equal(t, uint32(rollbackpb.TransactionType_TX_GUILD_DONATE), rec.GetTxType())
	assert.Equal(t, in.Payload, rec.GetPayload())
	assert.Equal(t, econDayKey, rec.GetPeriodKey())
	assert.Equal(t, uint32(1), rec.GetRefId())
	assert.Equal(t, uint32(1), rec.GetRefCount())
	assert.Equal(t, uint64(10), rec.GetContributionDelta())
	assert.Equal(t, uint64(1000), rec.GetFundsDelta())
	assert.Equal(t, res.StreamEpoch, rec.GetStreamEpoch())

	var payloadNull, resolvedByNull, resolveReasonNull bool
	require.NoError(t, f.db.QueryRowContext(f.ctx,
		"SELECT payload IS NULL, resolved_by IS NULL, resolve_reason IS NULL FROM guild_asset_op WHERE op_id=?", in.OpID).
		Scan(&payloadNull, &resolvedByNull, &resolveReasonNull))
	assert.False(t, payloadNull || resolvedByNull || resolveReasonNull, "三个可空列必须显式写值,不能留 NULL")

	usage, err := f.econ.DonateUsage(f.ctx, donor, econDayKey)
	require.NoError(t, err)
	assert.Equal(t, map[uint32]uint32{1: 1}, usage)

	pending, err := f.econ.PendingOps(f.ctx, donor, assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT, 16)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, in.OpID, pending[0].OpID)

	state, found, err := f.econ.OpState(f.ctx, in.OpID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING, state.Status)
	_, found, err = f.econ.OpState(f.ctx, 1)
	require.NoError(t, err)
	assert.False(t, found)
}

// TestReserveDonation_LimitReachedRollsBack:达上限整体回滚 —— 行不落、seq 不前进、次数不再加。
// seq 前进而行没落,scene 会看到一个永远不来的空洞 seq,后面的指令全部被窗口判定挡住。
func TestReserveDonation_LimitReachedRollsBack(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7511
		leader  uint64 = 8511
		donor   uint64 = 8512
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{donor: constants.RoleMember})

	first := econDonation(t, 9_510_001, donor, guildID, testNowMs)
	first.DailyLimit = 1
	_, err := f.econ.ReserveDonation(f.ctx, first)
	require.NoError(t, err)

	second := econDonation(t, 9_510_002, donor, guildID, testNowMs+1)
	second.DailyLimit = 1
	_, err = f.econ.ReserveDonation(f.ctx, second)
	assert.ErrorIs(t, err, ErrDonateLimit)

	assert.False(t, econOpExists(t, f, second.OpID), "达上限时指令行必须随事务回滚")
	var nextSeq uint64
	require.NoError(t, f.db.QueryRowContext(f.ctx,
		"SELECT next_seq FROM guild_player_op_seq WHERE player_id=? AND stream=?",
		donor, uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT)).Scan(&nextSeq))
	assert.Equal(t, uint64(2), nextSeq, "seq 不许因为被拒的那一笔前进")
	used, _ := econCounterUsed(t, f, donor, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_DONATE, 1, econDayKey)
	assert.Equal(t, uint32(1), used)
}

// TestReserveDonation_Rejections:帮会不在 / 不是成员 / 等级不够 / 合服闸门(含闸门读失败)各自回对应哨兵,
// 且拒绝之后库里什么都没留下。
func TestReserveDonation_Rejections(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID  uint64 = 7521
		missing  uint64 = 7529
		leader   uint64 = 8521
		donor    uint64 = 8522
		outsider uint64 = 8523
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{donor: constants.RoleMember})

	_, err := f.econ.ReserveDonation(f.ctx, econDonation(t, 9_520_001, donor, missing, testNowMs))
	assert.ErrorIs(t, err, ErrGuildGone)

	_, err = f.econ.ReserveDonation(f.ctx, econDonation(t, 9_520_002, outsider, guildID, testNowMs))
	assert.ErrorIs(t, err, ErrNotGuildMember)

	tooHigh := econDonation(t, 9_520_003, donor, guildID, testNowMs)
	tooHigh.MinGuildLevel = 2
	_, err = f.econ.ReserveDonation(f.ctx, tooHigh)
	assert.ErrorIs(t, err, ErrGuildLevelTooLow)

	merging := econDonation(t, 9_520_004, donor, guildID, testNowMs)
	merging.Fence = func(context.Context, uint32) error { return ErrZoneMerging }
	_, err = f.econ.ReserveDonation(f.ctx, merging)
	assert.ErrorIs(t, err, ErrZoneMerging)

	unreadable := econDonation(t, 9_520_005, donor, guildID, testNowMs)
	unreadable.Fence = func(context.Context, uint32) error { return errors.New("redis down") }
	_, err = f.econ.ReserveDonation(f.ctx, unreadable)
	assert.ErrorIs(t, err, ErrZoneMerging, "闸门读不出来也按拒绝处理(fail-closed)")

	var ops int
	require.NoError(t, f.db.QueryRowContext(f.ctx, "SELECT COUNT(*) FROM guild_asset_op").Scan(&ops))
	assert.Zero(t, ops, "被拒的预留不能留下指令行")
	_, found := econCounterUsed(t, f, donor, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_DONATE, 1, econDayKey)
	assert.False(t, found, "被拒的预留不能占次数")
}

// TestReserveDonation_TooManyPending:未决行达到 assetop.DefaultLimits.MaxPending 之后,守卫拒绝下一笔,
// 错误可被 errors.Is(assetop.ErrTooManyPending) 识别(logic 据此回 GuildAssetPending),且被拒那笔不占次数。
func TestReserveDonation_TooManyPending(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7531
		leader  uint64 = 8531
		donor   uint64 = 8532
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{donor: constants.RoleMember})

	maxPending := assetop.DefaultLimits.MaxPending
	for i := uint32(0); i < maxPending; i++ {
		in := econDonation(t, 9_530_000+uint64(i), donor, guildID, testNowMs+uint64(i))
		in.DailyLimit = 100
		_, err := f.econ.ReserveDonation(f.ctx, in)
		require.NoError(t, err, "第 %d 笔", i+1)
	}
	over := econDonation(t, 9_539_999, donor, guildID, testNowMs+1000)
	over.DailyLimit = 100
	_, err := f.econ.ReserveDonation(f.ctx, over)
	assert.ErrorIs(t, err, assetop.ErrTooManyPending)
	assert.False(t, econOpExists(t, f, over.OpID))
	used, _ := econCounterUsed(t, f, donor, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_DONATE, 1, econDayKey)
	assert.Equal(t, maxPending, used, "被守卫拒绝的那笔不占次数")
}

// TestReserveDonation_ConcurrentSamePlayerHonorsDailyLimit(05 §5.39 ①):同一玩家 10 路并发捐献、每日上限 5,
// 恰好 5 路成功、拿到的 seq 恰为 {1..5},其余一律被每日上限拒绝。
//
// 它钉的是"本人成员行锁把同一玩家的预留串行化":次数 upsert 与 seq 分配都在这把锁之后,并发者看到的永远是
// 前一个已提交的结果 —— 若串行化失效,会出现超卖(>5 成功)或 seq 空洞(被拒那笔把 seq 推进了)。
func TestReserveDonation_ConcurrentSamePlayerHonorsDailyLimit(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7911
		leader  uint64 = 8911
		donor   uint64 = 8912
		opBase  uint64 = 9_910_000
		workers        = 10
		limit   uint32 = 5
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{donor: constants.RoleMember})

	inputs := make([]DonationReserve, workers)
	for i := range inputs {
		inputs[i] = econDonation(t, opBase+uint64(i), donor, guildID, testNowMs)
		inputs[i].DailyLimit = limit
	}
	results := make([]Reserved, workers)
	fns := make([]func() error, workers)
	for i := range fns {
		fns[i] = func() error {
			res, err := f.econ.ReserveDonation(f.ctx, inputs[i])
			results[i] = res
			return err
		}
	}
	errs := econRunTogether(fns...)

	var seqs []uint64
	for i, err := range errs {
		if err == nil {
			seqs = append(seqs, results[i].Seq)
			continue
		}
		assert.ErrorIs(t, err, ErrDonateLimit, "第 %d 路:超额的只能被每日上限拒绝,不能是写冲突或内部错误", i)
	}
	slices.Sort(seqs)
	assert.Equal(t, []uint64{1, 2, 3, 4, 5}, seqs, "恰好 5 路成功,seq 连续无空洞")
	used, _ := econCounterUsed(t, f, donor, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_DONATE, 1, econDayKey)
	assert.Equal(t, limit, used)
	assert.Equal(t, 5, econOpCountInRange(t, f, opBase, opBase+workers-1), "被拒的预留不能留下指令行")
	assert.Equal(t, uint64(6), econNextSeq(t, f, donor, assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT),
		"seq 只因成功的 5 笔前进")
}

// ── T-S 兑换预留 ─────────────────────────────────────────────

// TestReserveShopOrder_DebitsContributionAndOccupiesLimit:扣可用帮贡、按份数占限购、写商店指令(永不中止);
// 达上限那一笔整体回滚,帮贡与限购都不动。
func TestReserveShopOrder_DebitsContributionAndOccupiesLimit(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7541
		leader  uint64 = 8541
		buyer   uint64 = 8542
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{buyer: constants.RoleMember})
	econSetContribution(t, f, guildID, buyer, 1000, 1000)

	first := econShopOrder(t, 9_540_001, buyer, guildID, 2, 300, 5, testNowMs)
	res, err := f.econ.ReserveShopOrder(f.ctx, first)
	require.NoError(t, err)
	assert.Equal(t, uint64(700), res.BalanceAfter)
	assert.Equal(t, uint64(1), res.Seq)

	rec := econRecord(t, f, first.OpID)
	assert.Equal(t, pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_SHOP, rec.GetKind())
	assert.Equal(t, uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT), rec.GetStream())
	assert.Zero(t, rec.GetDeadlineMs(), "商店指令永不中止")
	assert.Equal(t, uint32(101), rec.GetRefId())
	assert.Equal(t, uint32(2), rec.GetRefCount())
	assert.Equal(t, uint64(300), rec.GetContributionDelta())
	assert.Zero(t, rec.GetFundsDelta())
	assert.Equal(t, econDayKey, rec.GetPeriodKey())
	assert.Equal(t, uint32(rollbackpb.TransactionType_TX_GUILD_SHOP), rec.GetTxType())

	second := econShopOrder(t, 9_540_002, buyer, guildID, 3, 450, 5, testNowMs+1)
	res, err = f.econ.ReserveShopOrder(f.ctx, second)
	require.NoError(t, err)
	assert.Equal(t, uint64(250), res.BalanceAfter)

	third := econShopOrder(t, 9_540_003, buyer, guildID, 1, 150, 5, testNowMs+2)
	_, err = f.econ.ReserveShopOrder(f.ctx, third)
	assert.ErrorIs(t, err, ErrShopLimit)
	assert.False(t, econOpExists(t, f, third.OpID))

	total, balance, found := econContribution(t, f, guildID, buyer)
	require.True(t, found)
	assert.Equal(t, uint64(1000), total, "兑换只扣可用余额,不动累计")
	assert.Equal(t, uint64(250), balance, "被拒那笔不扣帮贡")

	usage, err := f.econ.ShopUsage(f.ctx, buyer, econDayKey, gameday.WeekKey(time.UnixMilli(int64(testNowMs))))
	require.NoError(t, err)
	assert.Equal(t, map[ShopUsageKey]uint32{{GoodsID: 101, PeriodKey: econDayKey}: 5}, usage)
}

// TestReserveShopOrder_Rejections:帮贡不足 / 等级不够回对应哨兵且不留痕;不限购的商品不占计数行。
func TestReserveShopOrder_Rejections(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7551
		leader  uint64 = 8551
		buyer   uint64 = 8552
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{buyer: constants.RoleMember})
	econSetContribution(t, f, guildID, buyer, 100, 100)

	poor := econShopOrder(t, 9_550_001, buyer, guildID, 1, 300, 5, testNowMs)
	_, err := f.econ.ReserveShopOrder(f.ctx, poor)
	assert.ErrorIs(t, err, ErrContributionInsufficient)
	assert.False(t, econOpExists(t, f, poor.OpID))
	_, found := econCounterUsed(t, f, buyer, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_SHOP, 101, econDayKey)
	assert.False(t, found, "帮贡不足时不能占限购")

	tooHigh := econShopOrder(t, 9_550_002, buyer, guildID, 1, 30, 5, testNowMs)
	tooHigh.RequiredGuildLevel = 6
	_, err = f.econ.ReserveShopOrder(f.ctx, tooHigh)
	assert.ErrorIs(t, err, ErrGuildLevelTooLow)

	unlimited := econShopOrder(t, 9_550_003, buyer, guildID, 1, 30, 0, testNowMs)
	res, err := f.econ.ReserveShopOrder(f.ctx, unlimited)
	require.NoError(t, err)
	assert.Equal(t, uint64(70), res.BalanceAfter)
	assert.Zero(t, econRecord(t, f, unlimited.OpID).GetPeriodKey(), "不限购 = 不占计数行,period_key 记 0")
	var counters int
	require.NoError(t, f.db.QueryRowContext(f.ctx,
		"SELECT COUNT(*) FROM guild_daily_counter WHERE player_id=?", buyer).Scan(&counters))
	assert.Zero(t, counters)
}

// TestReserve_AdjacentNewMembersConcurrentAllSucceed(05 §5.39 ②):50 个 player_id 相邻、从没用过经济的新成员,
// 各发 1 笔捐献 + 1 笔兑换,100 路同时放行,必须全部成功、零死锁。
//
// 这是"写事务是 READ COMMITTED"的行为证据:相邻主键的并发插入(seq 行、指令行的主键与唯一键、计数行)在 RR 下会被
// 间隙 / next-key 锁串成互等,RC 下没有间隙锁,各玩家的行互不相干。任何一路回 ErrWriteConflict(死锁重试耗尽
// 或锁等待超时)都说明有人在跨玩家取锁;被吸收掉的 1213 由 econDeadlockWatch 兜底看住。
func TestReserve_AdjacentNewMembersConcurrentAllSucceed(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID     uint64 = 7921
		leader      uint64 = 8920
		firstPlayer uint64 = 8921
		players            = 50
	)
	roles := make(map[uint64]uint32, players)
	for i := uint64(0); i < players; i++ {
		roles[firstPlayer+i] = constants.RoleMember
	}
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 100, leader, roles)
	mustExec(t, f.ctx, f.db,
		"UPDATE guild_member SET contribution_total=?, contribution_balance=? WHERE guild_id=?", 1000, 1000, guildID)
	watch := econWatchDeadlocks(t, f)

	fns := make([]func() error, 0, 2*players)
	for i := uint64(0); i < players; i++ {
		donation := econDonation(t, 9_920_000+i, firstPlayer+i, guildID, testNowMs)
		shop := econShopOrder(t, 9_921_000+i, firstPlayer+i, guildID, 1, 30, 5, testNowMs)
		fns = append(fns,
			func() error { _, err := f.econ.ReserveDonation(f.ctx, donation); return err },
			func() error { _, err := f.econ.ReserveShopOrder(f.ctx, shop); return err })
	}
	errs := econRunTogether(fns...)

	for i, err := range errs {
		assert.NoError(t, err, "第 %d 路(玩家 %d)必须成功:不同玩家之间不应有任何锁冲突", i, firstPlayer+uint64(i/2))
	}
	var ops int
	require.NoError(t, f.db.QueryRowContext(f.ctx,
		"SELECT COUNT(*) FROM guild_asset_op WHERE guild_id=? AND status=?", guildID, PendingStatus()).Scan(&ops))
	assert.Equal(t, 2*players, ops)
	for i := uint64(0); i < players; i++ {
		player := firstPlayer + i
		assert.Equal(t, uint64(2), econNextSeq(t, f, player, assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT), "player %d", player)
		assert.Equal(t, uint64(2), econNextSeq(t, f, player, assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT), "player %d", player)
	}
	watch.assertNone(t, f.ctx, "相邻新成员并发预留")
}

// ── T-U 升级 ─────────────────────────────────────────────────

// TestUpgradeGuild_ChargesCurrentLevelCostAndRaisesCap:1→2 级扣 GuildLevel[1].upgrade_cost_funds(20000),
// 上限取 GuildLevel[2].max_members(35);职位、expected_level、资金、满级、配表缺行、闸门各自拒绝。
func TestUpgradeGuild_ChargesCurrentLevelCostAndRaisesCap(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7561
		leader  uint64 = 8561
		officer uint64 = 8562
		member  uint64 = 8563
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 30, leader, map[uint64]uint32{
		officer: constants.RoleOfficer, member: constants.RoleMember,
	})
	mustExec(t, f.ctx, f.db, "UPDATE guild SET funds=? WHERE guild_id=?", 25000, guildID)

	// 先把快照读进缓存,才能证明升级之后缓存真的失效了。
	cached, err := f.repo.GetGuild(f.ctx, guildID)
	require.NoError(t, err)
	require.Equal(t, uint32(1), cached.Level)

	_, err = f.econ.UpgradeGuild(f.ctx, guildID, member, 1, econLevelLookup, nil)
	assert.ErrorIs(t, err, ErrRankTooLow, "普通成员不能升级")

	_, err = f.econ.UpgradeGuild(f.ctx, guildID, officer, 1, econLevelLookup,
		func(context.Context, uint32) error { return ErrZoneMerging })
	assert.ErrorIs(t, err, ErrZoneMerging)

	stale, err := f.econ.UpgradeGuild(f.ctx, guildID, officer, 2, econLevelLookup, nil)
	require.NoError(t, err)
	assert.False(t, stale.Changed, "expected_level 对不上 = 无改动")
	assert.Equal(t, uint32(1), stale.NewLevel)
	assert.Equal(t, uint64(25000), econFunds(t, f, guildID))

	res, err := f.econ.UpgradeGuild(f.ctx, guildID, officer, 1, econLevelLookup, nil)
	require.NoError(t, err)
	assert.True(t, res.Changed)
	assert.Equal(t, uint32(2), res.NewLevel)
	assert.Equal(t, []uint64{leader, officer, member}, res.MemberIDs, "收件人 player_id 升序")
	assert.Equal(t, uint64(5000), econFunds(t, f, guildID), "扣当前等级行的 20000")
	var maxMembers uint32
	require.NoError(t, f.db.QueryRowContext(f.ctx,
		"SELECT max_members FROM guild WHERE guild_id=?", guildID).Scan(&maxMembers))
	assert.Equal(t, uint32(35), maxMembers, "新上限取下一级行")

	fresh, err := f.repo.GetGuild(f.ctx, guildID)
	require.NoError(t, err)
	assert.Equal(t, uint32(2), fresh.Level, "提交后必须失效帮会快照缓存")

	again, err := f.econ.UpgradeGuild(f.ctx, guildID, officer, 1, econLevelLookup, nil)
	require.NoError(t, err)
	assert.False(t, again.Changed, "重复点击不能连升两级")
	assert.Equal(t, uint64(5000), econFunds(t, f, guildID))

	_, err = f.econ.UpgradeGuild(f.ctx, guildID, leader, 2, econLevelLookup, nil)
	assert.ErrorIs(t, err, ErrFundsInsufficient)

	// 过期视图自愈:模拟"上一次升级已提交、COMMIT 回执丢失、当时没失效缓存"—— 先把快照读进缓存,再绕过 repo 直接改库。
	// 调用方按旧等级重试,命中 expected_level 不符的无改动分支;这一支不扣钱,但必须失效缓存。
	cached, err = f.repo.GetGuild(f.ctx, guildID)
	require.NoError(t, err)
	require.Equal(t, uint32(2), cached.Level)
	mustExec(t, f.ctx, f.db, "UPDATE guild SET level=? WHERE guild_id=?", 3, guildID)
	healed, err := f.econ.UpgradeGuild(f.ctx, guildID, leader, 2, econLevelLookup, nil)
	require.NoError(t, err)
	assert.False(t, healed.Changed)
	assert.Equal(t, uint32(3), healed.NewLevel)
	assert.Equal(t, uint64(5000), econFunds(t, f, guildID), "无改动分支不扣钱")
	fresh, err = f.repo.GetGuild(f.ctx, guildID)
	require.NoError(t, err)
	assert.Equal(t, uint32(3), fresh.Level, "expected_level 不符 = 调用方视图过期,必须顺手失效缓存")

	mustExec(t, f.ctx, f.db, "UPDATE guild SET level=?, funds=? WHERE guild_id=?", 10, 9_000_000, guildID)
	_, err = f.econ.UpgradeGuild(f.ctx, guildID, leader, 0, econLevelLookup, nil)
	assert.ErrorIs(t, err, ErrGuildMaxLevel)

	mustExec(t, f.ctx, f.db, "UPDATE guild SET level=? WHERE guild_id=?", 9, guildID)
	missingNext := func(level uint32) (uint64, uint32, bool) {
		if level == 10 {
			return 0, 0, false
		}
		return econLevelLookup(level)
	}
	_, err = f.econ.UpgradeGuild(f.ctx, guildID, leader, 0, missingNext, nil)
	assert.ErrorIs(t, err, ErrGuildLevelConfigMissing, "配表缺下一级行按错误处理,不默认放行")
}

// TestUpgradeGuild_ConcurrentSameExpectedLevelUpgradesOnce(05 §5.39 ③):帮主与长老同时按 expected_level=1 点升级,
// 只升一级、只扣一次钱,另一方拿到 Changed=false(NewLevel=2),两方都不报错。
// 串行化靠 guild 行 FOR UPDATE:后到的一方在锁内读到等级已是 2,落进"expected_level 不符 = 无改动"分支。
func TestUpgradeGuild_ConcurrentSameExpectedLevelUpgradesOnce(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7931
		leader  uint64 = 8931
		officer uint64 = 8932
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 30, leader, map[uint64]uint32{officer: constants.RoleOfficer})
	mustExec(t, f.ctx, f.db, "UPDATE guild SET funds=? WHERE guild_id=?", 25000, guildID)

	var results [2]UpgradeResult
	errs := econRunTogether(
		func() error {
			res, err := f.econ.UpgradeGuild(f.ctx, guildID, leader, 1, econLevelLookup, nil)
			results[0] = res
			return err
		},
		func() error {
			res, err := f.econ.UpgradeGuild(f.ctx, guildID, officer, 1, econLevelLookup, nil)
			results[1] = res
			return err
		})

	require.NoError(t, errs[0], "帮主")
	require.NoError(t, errs[1], "长老")
	changed := 0
	for _, res := range results {
		if res.Changed {
			changed++
		}
		assert.Equal(t, uint32(2), res.NewLevel, "两方看到的都是升级后的等级")
	}
	assert.Equal(t, 1, changed, "只有一方真的升级")
	assert.Equal(t, uint64(5000), econFunds(t, f, guildID), "只扣一次 20000")
	var level uint32
	require.NoError(t, f.db.QueryRowContext(f.ctx, "SELECT level FROM guild WHERE guild_id=?", guildID).Scan(&level))
	assert.Equal(t, uint32(2), level, "不能连升两级")
}

// ── Store:领取、重排 ─────────────────────────────────────────

// TestListDue_FreshRowsFirst:第一段只取新行并优先占名额,第二段只补缺口;租约未到期、未到期、非未决的行都不出现。
func TestListDue_FreshRowsFirst(t *testing.T) {
	f := openEconomyFixture(t)
	const player uint64 = 8571
	epoch := testNowMs - 100_000

	for i, opID := range []uint64{101, 102, 103} { // 老行:attempts 已达 FreshAttemptLimit,且更早到期
		rec := econShopRecord(t, opID, player, epoch, uint64(i+1))
		rec.Attempts = assetop.FreshAttemptLimit + 2
		rec.NextAttemptMs = testNowMs - 50_000 + uint64(i)
		econInsertOp(t, f, rec)
	}
	for i, opID := range []uint64{201, 202} { // 新行
		rec := econShopRecord(t, opID, player, epoch, uint64(10+i))
		rec.NextAttemptMs = testNowMs - 10 + uint64(i)
		econInsertOp(t, f, rec)
	}
	notDue := econShopRecord(t, 301, player, epoch, 20)
	notDue.NextAttemptMs = testNowMs + 1000
	econInsertOp(t, f, notDue)
	leased := econShopRecord(t, 302, player, epoch, 21)
	leased.LeaseUntilMs = testNowMs + 5000
	econInsertOp(t, f, leased)
	done := econShopRecord(t, 303, player, epoch, 22)
	done.Status = pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED
	econInsertOp(t, f, done)

	ids, err := f.store.ListDue(f.ctx, testNowMs, 2)
	require.NoError(t, err)
	assert.Equal(t, []uint64{201, 202}, ids, "新行满额时老行一条都不取")

	ids, err = f.store.ListDue(f.ctx, testNowMs, 4)
	require.NoError(t, err)
	assert.Equal(t, []uint64{201, 202, 101, 102}, ids, "第二段只补缺口,新行排在前面")

	ids, err = f.store.ListDue(f.ctx, testNowMs, 100)
	require.NoError(t, err)
	assert.Equal(t, []uint64{201, 202, 101, 102, 103}, ids)
}

// TestClaim_ReturnsFullOpAndHoldsLease:领到的 Op 带齐纪元 / correlation(= op_id)/ 令牌 / 包;租约期内别人领不到。
func TestClaim_ReturnsFullOpAndHoldsLease(t *testing.T) {
	f := openEconomyFixture(t)
	const player uint64 = 8581
	rec := econShopRecord(t, 401, player, testNowMs-100_000, 7)
	rec.Attempts = 2
	rec.LastReason = assetop.ReasonBagFull
	econInsertOp(t, f, rec)

	const token uint64 = 0xabc1
	op, claimed, err := f.store.Claim(f.ctx, 401, testNowMs, testNowMs+econLeaseMs, testNowMs+3_600_000, token)
	require.NoError(t, err)
	require.True(t, claimed)
	assert.Equal(t, uint64(401), op.OpID)
	assert.Equal(t, player, op.PlayerID)
	assert.Equal(t, assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT, op.Stream)
	assert.Equal(t, uint64(7), op.Seq)
	assert.Equal(t, rec.GetStreamEpoch(), op.StreamEpoch)
	assert.Equal(t, uint64(401), op.CorrelationID, "correlation_id 取 op_id,不是 ref_id")
	assert.Equal(t, token, op.LeaseToken)
	assert.Equal(t, uint32(rollbackpb.TransactionType_TX_GUILD_SHOP), op.TxType)
	assert.Equal(t, uint32(2), op.Attempts)
	assert.Equal(t, assetop.ReasonBagFull, op.LastReason)
	wantBundle := &assetpb.AssetBundle{}
	require.NoError(t, proto.Unmarshal(rec.GetPayload(), wantBundle))
	assert.True(t, proto.Equal(wantBundle, op.Bundle))

	stored := econRecord(t, f, 401)
	assert.Equal(t, testNowMs+econLeaseMs, stored.GetLeaseUntilMs())
	assert.Equal(t, token, stored.GetLeaseToken())

	_, claimed, err = f.store.Claim(f.ctx, 401, testNowMs+1, testNowMs+1+econLeaseMs, testNowMs+3_600_000, 0xabc2)
	require.NoError(t, err)
	assert.False(t, claimed, "租约期内另一个副本领不到")
}

// TestClaim_PoisonRowIsDeferred:payload 解不开或为 NULL 都是毒行 —— 推迟到循环给的 poisonUntilMs、
// 放掉租约、last_outcome 记未知,并回 assetop.ErrPoisonRow;不许把空包下发给 scene。
func TestClaim_PoisonRowIsDeferred(t *testing.T) {
	f := openEconomyFixture(t)
	const player uint64 = 8591
	garbage := econShopRecord(t, 501, player, testNowMs-100_000, 1)
	garbage.Payload = []byte{0xff, 0xff}
	econInsertOp(t, f, garbage)
	null := econShopRecord(t, 502, player, testNowMs-100_000, 2)
	null.Payload = nil // 插入时绑定 NULL
	econInsertOp(t, f, null)

	const poisonUntil = testNowMs + 3_600_000
	for _, opID := range []uint64{501, 502} {
		_, claimed, err := f.store.Claim(f.ctx, opID, testNowMs, testNowMs+econLeaseMs, poisonUntil, 0x77)
		assert.ErrorIs(t, err, assetop.ErrPoisonRow, "op %d", opID)
		assert.False(t, claimed)

		stored := econRecord(t, f, opID)
		assert.Equal(t, poisonUntil, stored.GetNextAttemptMs(), "毒行推迟到循环算好的时刻,不自写常量")
		assert.Zero(t, stored.GetLeaseUntilMs(), "毒行放掉租约")
		assert.Equal(t, uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN), stored.GetLastOutcome())
		assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING, stored.GetStatus(), "毒行不终结")
	}

	// 竞态:Claim 的 CAS 写下令牌之后、推迟毒行之前,assetopfix 已把行人工终结(它的 CAS 只看 status、不换令牌)。
	// 这里直接造出"令牌仍是本次、状态已是终态"的行再推迟:终态行必须一个字不动 —— next_attempt_ms 仍是终结时刻、
	// last_outcome 仍是人工终结保留的证据(07 §7.2 / §7.4.1)。
	const (
		token      uint64 = 0x78
		resolvedAt uint64 = testNowMs - 5
	)
	resolved := econShopRecord(t, 503, player, testNowMs-100_000, 3)
	resolved.Payload = []byte{0xff, 0xff}
	resolved.Status = pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_ABORTED
	resolved.LeaseToken = token
	resolved.NextAttemptMs = resolvedAt
	resolved.UpdatedMs = resolvedAt
	resolved.LastOutcome = uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_RETRY)
	econInsertOp(t, f, resolved)

	f.store.markPoison(f.ctx, 503, token, testNowMs, poisonUntil)
	stored := econRecord(t, f, 503)
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_ABORTED, stored.GetStatus())
	assert.Equal(t, resolvedAt, stored.GetNextAttemptMs(), "终态行的 next_attempt_ms 必须仍是终结时刻")
	assert.Equal(t, resolvedAt, stored.GetUpdatedMs(), "终态行此后没有任何路径再改")
	assert.Equal(t, uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_RETRY), stored.GetLastOutcome(), "证据不能被抹成 0")
}

// TestClaimThenReschedule_LeaseLost:Reschedule 带错令牌必须回 ErrLeaseLost(不能伪装成功);
// 带对令牌则推进次数、放租约、记下本次答复。
func TestClaimThenReschedule_LeaseLost(t *testing.T) {
	f := openEconomyFixture(t)
	const player uint64 = 8601
	econInsertOp(t, f, econShopRecord(t, 601, player, testNowMs-100_000, 1))

	op, claimed, err := f.store.Claim(f.ctx, 601, testNowMs, testNowMs+econLeaseMs, testNowMs+3_600_000, 0xa1)
	require.NoError(t, err)
	require.True(t, claimed)

	res := assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_RETRY, Reason: assetop.ReasonInBattle}
	stolen := op
	stolen.LeaseToken = 0xb2
	err = f.store.Reschedule(f.ctx, stolen, testNowMs+5000, res, testNowMs+1)
	assert.ErrorIs(t, err, assetop.ErrLeaseLost)
	assert.Zero(t, econRecord(t, f, 601).GetAttempts(), "租约不是我的,一个字都不能写进去")

	require.NoError(t, f.store.Reschedule(f.ctx, op, testNowMs+5000, res, testNowMs+1))
	stored := econRecord(t, f, 601)
	assert.Equal(t, uint32(1), stored.GetAttempts())
	assert.Equal(t, testNowMs+5000, stored.GetNextAttemptMs())
	assert.Zero(t, stored.GetLeaseUntilMs())
	assert.Zero(t, stored.GetDurable())
	assert.Equal(t, uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_RETRY), stored.GetLastOutcome())
	assert.Equal(t, assetop.ReasonInBattle, stored.GetLastReason())
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING, stored.GetStatus())
}

// ── Store:终结与对侧账 ───────────────────────────────────────

// TestFinalize_DonateAppliedCreditsFundsAndContribution:APPLIED 按 op.guild_id 记资金与帮贡、同写
// next_attempt_ms = now(07 §7.4.1)、失效缓存、推送一次;重复终结 CAS 落空,对侧账不做第二遍。
func TestFinalize_DonateAppliedCreditsFundsAndContribution(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7611
		leader  uint64 = 8611
		donor   uint64 = 8612
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{donor: constants.RoleMember})
	in := econDonation(t, 9_610_001, donor, guildID, testNowMs)
	_, err := f.econ.ReserveDonation(f.ctx, in)
	require.NoError(t, err)

	var pushed []FinalizedOp
	f.store.OnFinalized = func(_ context.Context, op FinalizedOp) { pushed = append(pushed, op) }
	cached, err := f.repo.GetGuild(f.ctx, guildID)
	require.NoError(t, err)
	require.Zero(t, cached.Funds)

	finalNow := testNowMs + 1234
	op := assetop.Op{OpID: in.OpID, PlayerID: donor}
	finalized, err := f.store.Finalize(f.ctx, op, assetop.StatusApplied, econAppliedResult(), finalNow)
	require.NoError(t, err)
	assert.True(t, finalized)

	assert.Equal(t, uint64(1000), econFunds(t, f, guildID))
	total, balance, found := econContribution(t, f, guildID, donor)
	require.True(t, found)
	assert.Equal(t, uint64(10), total)
	assert.Equal(t, uint64(10), balance)

	rec := econRecord(t, f, in.OpID)
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED, rec.GetStatus())
	assert.Equal(t, uint32(1), rec.GetDurable())
	assert.Equal(t, finalNow, rec.GetNextAttemptMs(), "终态行的 next_attempt_ms 必须等于终结时刻(回档检查按它判)")
	assert.Equal(t, finalNow, rec.GetUpdatedMs())
	assert.Zero(t, rec.GetLeaseUntilMs())
	assert.Zero(t, rec.GetReasonTipId())
	assert.Equal(t, uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED), rec.GetLastOutcome())

	fresh, err := f.repo.GetGuild(f.ctx, guildID)
	require.NoError(t, err)
	assert.Equal(t, uint64(1000), fresh.Funds, "改过帮会行必须失效快照缓存")

	require.Len(t, pushed, 1)
	assert.Equal(t, FinalizedOp{
		OpID: in.OpID, PlayerID: donor, GuildID: guildID,
		Kind: pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE, Status: assetop.StatusApplied,
	}, pushed[0])

	again, err := f.store.Finalize(f.ctx, op, assetop.StatusApplied, econAppliedResult(), finalNow+1)
	require.NoError(t, err)
	assert.False(t, again, "第二次终结 CAS 落空")
	assert.Equal(t, uint64(1000), econFunds(t, f, guildID), "对侧账只记一次")
	assert.Len(t, pushed, 1, "只有本次终结才推送")
	used, _ := econCounterUsed(t, f, donor, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_DONATE, 1, econDayKey)
	assert.Equal(t, uint32(1), used, "APPLIED 保留次数")
}

// TestFinalize_DonateAppliedGuildGoneIsOrphan:D2 —— 绑定的帮会已解散,资金与帮贡都记不上,
// 计 orphan{donate,guild_gone},不补偿,也不能凭空把帮会行写回来。
func TestFinalize_DonateAppliedGuildGoneIsOrphan(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7621
		leader  uint64 = 8621
		donor   uint64 = 8622
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{donor: constants.RoleMember})
	in := econDonation(t, 9_620_001, donor, guildID, testNowMs)
	_, err := f.econ.ReserveDonation(f.ctx, in)
	require.NoError(t, err)
	_, err = f.repo.DisbandGuild(f.ctx, guildID, leader, testNowMs+10, nil)
	require.NoError(t, err)

	orphans := econCaptureOrphans(t)
	finalized, err := f.store.Finalize(f.ctx, assetop.Op{OpID: in.OpID, PlayerID: donor},
		assetop.StatusApplied, econAppliedResult(), testNowMs+20)
	require.NoError(t, err)
	assert.True(t, finalized, "帮会不在也要终结,否则这一行会被永远重投")
	assert.Equal(t, []string{"donate/guild_gone"}, orphans())
	assert.Zero(t, guildRowCount(t, f.ctx, f.db, guildID), "资金无处可记,帮会行不能被写回来")
	assert.Zero(t, memberCount(t, f.ctx, f.db, donor))
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED, econRecord(t, f, in.OpID).GetStatus())
}

// TestFinalize_DonateAppliedMemberGoneCreditsFundsOnly:D2 —— 捐献者已离帮而帮会还在,资金照记给
// op.guild_id,帮贡跳过,计 orphan{donate,member_gone}。
func TestFinalize_DonateAppliedMemberGoneCreditsFundsOnly(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7631
		leader  uint64 = 8631
		donor   uint64 = 8632
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{donor: constants.RoleMember})
	in := econDonation(t, 9_630_001, donor, guildID, testNowMs)
	_, err := f.econ.ReserveDonation(f.ctx, in)
	require.NoError(t, err)
	_, err = f.repo.LeaveGuild(f.ctx, guildID, donor, testNowMs+10)
	require.NoError(t, err)

	orphans := econCaptureOrphans(t)
	finalized, err := f.store.Finalize(f.ctx, assetop.Op{OpID: in.OpID, PlayerID: donor},
		assetop.StatusApplied, econAppliedResult(), testNowMs+20)
	require.NoError(t, err)
	assert.True(t, finalized)
	assert.Equal(t, uint64(1000), econFunds(t, f, guildID), "资金照记给发起时绑定的帮会")
	_, _, found := econContribution(t, f, guildID, donor)
	assert.False(t, found, "成员行不能被写回来")
	assert.Equal(t, []string{"donate/member_gone"}, orphans())
	leaderTotal, _, _ := econContribution(t, f, guildID, leader)
	assert.Zero(t, leaderTotal, "帮贡不能错记到别人头上")
}

// TestFinalize_DonateRejectedOrAbortedRefundsDailyCount:REJECTED / ABORTED 退回今日次数;
// reason_tip_id 只在 REJECTED 时写 scene 的原因码(ABORTED 写 0);资金不动。
func TestFinalize_DonateRejectedOrAbortedRefundsDailyCount(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7641
		leader  uint64 = 8641
		donor   uint64 = 8642
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{donor: constants.RoleMember})

	rejected := econDonation(t, 9_640_001, donor, guildID, testNowMs)
	_, err := f.econ.ReserveDonation(f.ctx, rejected)
	require.NoError(t, err)
	finalized, err := f.store.Finalize(f.ctx, assetop.Op{OpID: rejected.OpID, PlayerID: donor}, assetop.StatusRejected,
		assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED, Reason: assetop.ReasonCurrencyInsufficient, Durable: true},
		testNowMs+10)
	require.NoError(t, err)
	require.True(t, finalized)
	used, _ := econCounterUsed(t, f, donor, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_DONATE, 1, econDayKey)
	assert.Zero(t, used, "REJECTED 退回次数")
	rec := econRecord(t, f, rejected.OpID)
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_REJECTED, rec.GetStatus())
	assert.Equal(t, assetop.ReasonCurrencyInsufficient, rec.GetReasonTipId())

	aborted := econDonation(t, 9_640_002, donor, guildID, testNowMs+20)
	_, err = f.econ.ReserveDonation(f.ctx, aborted)
	require.NoError(t, err)
	finalized, err = f.store.Finalize(f.ctx, assetop.Op{OpID: aborted.OpID, PlayerID: donor}, assetop.StatusAborted,
		assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED, Durable: true}, testNowMs+30)
	require.NoError(t, err)
	require.True(t, finalized)
	used, _ = econCounterUsed(t, f, donor, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_DONATE, 1, econDayKey)
	assert.Zero(t, used, "ABORTED 退回次数")
	assert.Zero(t, econRecord(t, f, aborted.OpID).GetReasonTipId(), "ABORTED 的 reason_tip_id 写 0")
	assert.Zero(t, econFunds(t, f, guildID), "被拒 / 中止的捐献不记资金")
}

// TestFinalize_AppliedPartialOnlyCAS:部分发放只终结、不做对侧账(X-03):商店不退帮贡不退限购,捐献不记资金不退次数。
func TestFinalize_AppliedPartialOnlyCAS(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7651
		leader  uint64 = 8651
		player  uint64 = 8652
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{player: constants.RoleMember})
	econSetContribution(t, f, guildID, player, 1000, 1000)

	shop := econShopOrder(t, 9_650_001, player, guildID, 2, 300, 5, testNowMs)
	_, err := f.econ.ReserveShopOrder(f.ctx, shop)
	require.NoError(t, err)
	donate := econDonation(t, 9_650_002, player, guildID, testNowMs+1)
	_, err = f.econ.ReserveDonation(f.ctx, donate)
	require.NoError(t, err)

	partial := assetop.Result{
		Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED, Reason: assetop.ReasonPartialApplied,
		Durable: true, Partial: true,
	}
	for _, opID := range []uint64{shop.OpID, donate.OpID} {
		finalized, err := f.store.Finalize(f.ctx, assetop.Op{OpID: opID, PlayerID: player},
			assetop.StatusAppliedPartial, partial, testNowMs+50)
		require.NoError(t, err)
		require.True(t, finalized)
		rec := econRecord(t, f, opID)
		assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED_PARTIAL, rec.GetStatus())
		assert.Equal(t, testNowMs+50, rec.GetNextAttemptMs())
		assert.Zero(t, rec.GetReasonTipId())
	}
	_, balance, _ := econContribution(t, f, guildID, player)
	assert.Equal(t, uint64(700), balance, "部分发放不退帮贡")
	shopUsed, _ := econCounterUsed(t, f, player, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_SHOP, 101, econDayKey)
	assert.Equal(t, uint32(2), shopUsed, "部分发放不退限购")
	donateUsed, _ := econCounterUsed(t, f, player, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_DONATE, 1, econDayKey)
	assert.Equal(t, uint32(1), donateUsed, "部分发放不退次数")
	assert.Zero(t, econFunds(t, f, guildID), "部分发放不记资金")
}

// TestFinalize_ShopRejectedRefundsContributionAndLimit:永久拒绝退帮贡与限购;兑换者已离帮时帮贡退不回去,
// 计 orphan{shop,refund_member_gone},限购照退。
func TestFinalize_ShopRejectedRefundsContributionAndLimit(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7661
		leader  uint64 = 8661
		buyer   uint64 = 8662
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{buyer: constants.RoleMember})
	econSetContribution(t, f, guildID, buyer, 1000, 1000)

	first := econShopOrder(t, 9_660_001, buyer, guildID, 2, 300, 5, testNowMs)
	_, err := f.econ.ReserveShopOrder(f.ctx, first)
	require.NoError(t, err)
	finalized, err := f.store.Finalize(f.ctx, assetop.Op{OpID: first.OpID, PlayerID: buyer}, assetop.StatusRejected,
		assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED, Reason: assetop.ReasonBagFull, Durable: true},
		testNowMs+10)
	require.NoError(t, err)
	require.True(t, finalized)
	_, balance, _ := econContribution(t, f, guildID, buyer)
	assert.Equal(t, uint64(1000), balance, "永久拒绝退帮贡")
	used, _ := econCounterUsed(t, f, buyer, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_SHOP, 101, econDayKey)
	assert.Zero(t, used, "永久拒绝退限购")
	assert.Equal(t, assetop.ReasonBagFull, econRecord(t, f, first.OpID).GetReasonTipId())

	second := econShopOrder(t, 9_660_002, buyer, guildID, 1, 150, 5, testNowMs+20)
	_, err = f.econ.ReserveShopOrder(f.ctx, second)
	require.NoError(t, err)
	_, err = f.repo.LeaveGuild(f.ctx, guildID, buyer, testNowMs+30)
	require.NoError(t, err)

	orphans := econCaptureOrphans(t)
	finalized, err = f.store.Finalize(f.ctx, assetop.Op{OpID: second.OpID, PlayerID: buyer}, assetop.StatusAborted,
		assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED, Durable: true}, testNowMs+40)
	require.NoError(t, err)
	require.True(t, finalized)
	assert.Equal(t, []string{"shop/refund_member_gone"}, orphans())
	_, _, found := econContribution(t, f, guildID, buyer)
	assert.False(t, found, "离帮者的成员行不能被退款写回来")
	used, _ = econCounterUsed(t, f, buyer, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_SHOP, 101, econDayKey)
	assert.Zero(t, used, "限购属于玩家,照退")
}

// TestFinalize_ActivityRewardOnlyCAS(05 §5.39 ④):活动发奖的帮贡与资金在入队事务里已记完,终结(APPLIED 或 REJECTED)
// 只做 CAS —— 帮会资金、成员帮贡、任何计数行都不动,不计 orphan;终态行照样同写 next_attempt_ms = now(07 §7.4.1)。
// 行上故意带着非 0 的 funds_delta / contribution_delta 与计数键:哪条对侧账分支误认了这个 kind,这里就会看到变化。
func TestFinalize_ActivityRewardOnlyCAS(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7941
		leader  uint64 = 8941
		player  uint64 = 8942
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{player: constants.RoleMember})
	mustExec(t, f.ctx, f.db, "UPDATE guild SET funds=? WHERE guild_id=?", 777, guildID)
	econSetContribution(t, f, guildID, player, 100, 60)
	counterKinds := []pb.GuildDailyCounterKind{
		pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_DONATE,
		pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_SHOP,
		pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_ACTIVITY,
	}
	for _, kind := range counterKinds {
		mustExec(t, f.ctx, f.db,
			`INSERT INTO guild_daily_counter (player_id, counter_kind, ref_id, period_key, used_count, updated_ms) VALUES (?, ?, ?, ?, ?, ?)`,
			player, int32(kind), 1, econDayKey, 2, testNowMs)
	}
	activity := func(opID, seq uint64) *pb.GuildAssetOpRecord {
		rec := econShopRecord(t, opID, player, testNowMs-100_000, seq)
		rec.GuildId = guildID
		rec.Kind = pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_ACTIVITY_REWARD
		rec.TxType = uint32(rollbackpb.TransactionType_TX_GUILD_ACTIVITY_REWARD)
		rec.RefId, rec.RefCount, rec.PeriodKey = 1, 1, econDayKey
		rec.ContributionDelta, rec.FundsDelta = 50, 500
		return rec
	}
	econInsertOp(t, f, activity(9_940_001, 1))
	econInsertOp(t, f, activity(9_940_002, 2))

	orphans := econCaptureOrphans(t)
	var pushed []FinalizedOp
	f.store.OnFinalized = func(_ context.Context, op FinalizedOp) { pushed = append(pushed, op) }

	cases := []struct {
		opID   uint64
		status assetop.Status
		res    assetop.Result
		want   pb.GuildAssetOpStatus
		now    uint64
	}{
		{9_940_001, assetop.StatusApplied, econAppliedResult(), pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED, testNowMs + 100},
		{9_940_002, assetop.StatusRejected, econRejectedResult(), pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_REJECTED, testNowMs + 200},
	}
	for _, c := range cases {
		finalized, err := f.store.Finalize(f.ctx, assetop.Op{OpID: c.opID, PlayerID: player}, c.status, c.res, c.now)
		require.NoError(t, err, "op %d", c.opID)
		require.True(t, finalized, "op %d", c.opID)
		rec := econRecord(t, f, c.opID)
		assert.Equal(t, c.want, rec.GetStatus(), "op %d", c.opID)
		assert.Equal(t, c.now, rec.GetNextAttemptMs(), "op %d:终态行的 next_attempt_ms 必须等于终结时刻", c.opID)
		assert.Equal(t, c.now, rec.GetUpdatedMs(), "op %d", c.opID)
	}

	assert.Equal(t, uint64(777), econFunds(t, f, guildID), "活动发奖终结不动帮会资金")
	total, balance, found := econContribution(t, f, guildID, player)
	require.True(t, found)
	assert.Equal(t, uint64(100), total, "活动发奖终结不动帮贡")
	assert.Equal(t, uint64(60), balance)
	for _, kind := range counterKinds {
		used, found := econCounterUsed(t, f, player, kind, 1, econDayKey)
		require.True(t, found, "kind=%s", kind)
		assert.Equal(t, uint32(2), used, "活动发奖终结不退任何计数(kind=%s)", kind)
	}
	assert.Empty(t, orphans(), "只 CAS 的终结不计 orphan")
	require.Len(t, pushed, 2, "本次终结照常推送")
	for _, op := range pushed {
		assert.Equal(t, pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_ACTIVITY_REWARD, op.Kind)
	}
}

// TestResolveManually_WritesAuditAndNextAttempt:人工终结经包级 assetop.ResolveManually 走同一把 CAS 与同一份对侧账,
// 留下操作人与依据、同写 next_attempt_ms = now,不置 durable、不改 last_outcome;之后自动终结落空。
func TestResolveManually_WritesAuditAndNextAttempt(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7671
		leader  uint64 = 8671
		donor   uint64 = 8672
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{donor: constants.RoleMember})
	in := econDonation(t, 9_670_001, donor, guildID, testNowMs)
	_, err := f.econ.ReserveDonation(f.ctx, in)
	require.NoError(t, err)

	resolveNow := testNowMs + 7_200_000
	clock := func() time.Time { return time.UnixMilli(int64(resolveNow)) }
	manual := assetop.ManualResolution{OpID: in.OpID, Final: assetop.StatusApplied, Operator: "ops-alice", Reason: "交易流水已核对,已扣款"}
	resolved, err := assetop.ResolveManually(f.ctx, f.store, manual, nil, clock)
	require.NoError(t, err)
	assert.True(t, resolved)

	rec := econRecord(t, f, in.OpID)
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED, rec.GetStatus())
	assert.Equal(t, "ops-alice", rec.GetResolvedBy())
	assert.Equal(t, manual.Reason, rec.GetResolveReason())
	assert.Equal(t, resolveNow, rec.GetNextAttemptMs(), "人工终结同样同写 next_attempt_ms = now")
	assert.Equal(t, resolveNow, rec.GetUpdatedMs())
	assert.Zero(t, rec.GetDurable(), "人工终结不是 scene 确认的落盘结局,不置 durable")
	assert.Zero(t, rec.GetLastOutcome(), "last_outcome 保留最后一次 scene 真实答复")
	assert.Zero(t, rec.GetLeaseUntilMs())
	assert.Equal(t, uint64(1000), econFunds(t, f, guildID), "人工判 APPLIED 同样记资金")

	resolved, err = assetop.ResolveManually(f.ctx, f.store, manual, nil, clock)
	require.NoError(t, err)
	assert.False(t, resolved, "已非 PENDING")
	finalized, err := f.store.Finalize(f.ctx, assetop.Op{OpID: in.OpID, PlayerID: donor},
		assetop.StatusApplied, econAppliedResult(), resolveNow+1)
	require.NoError(t, err)
	assert.False(t, finalized, "人工与自动只有一个赢家")
	assert.Equal(t, uint64(1000), econFunds(t, f, guildID))
}

// ── 离帮 / 被踢 / 解散提前截止 ───────────────────────────────

// TestAccelerate_LeaveKickDisbandPullDeadlineToNow:三个事务把本帮未决捐献的截止提前到 now、
// next_attempt_ms 拉回 now,不抢租约;商店指令与绑定别的帮会的捐献一律不动。
func TestAccelerate_LeaveKickDisbandPullDeadlineToNow(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7681
		other   uint64 = 7682
		leader  uint64 = 8681
		leaver  uint64 = 8682
		kicked  uint64 = 8683
		stayer  uint64 = 8684
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{
		leaver: constants.RoleMember, kicked: constants.RoleMember, stayer: constants.RoleMember,
	})
	seedManagedGuild(t, f.ctx, f.db, other, 2, 1, 50, 8685, nil)
	econSetContribution(t, f, guildID, leaver, 500, 500)

	reserve := func(opID, playerID uint64) DonationReserve {
		in := econDonation(t, opID, playerID, guildID, testNowMs)
		_, err := f.econ.ReserveDonation(f.ctx, in)
		require.NoError(t, err)
		return in
	}
	leaverDonation := reserve(9_680_001, leaver)
	kickedDonation := reserve(9_680_002, kicked)
	stayerDonation := reserve(9_680_003, stayer)
	leaverShop := econShopOrder(t, 9_680_004, leaver, guildID, 1, 30, 0, testNowMs)
	_, err := f.econ.ReserveShopOrder(f.ctx, leaverShop)
	require.NoError(t, err)
	// 绑定别的帮会的一笔(比如早先在那边发起、至今未决):过滤条件 guild_id 必须把它排除。
	foreign := &pb.GuildAssetOpRecord{
		OpId: 9_680_005, PlayerId: leaver, Stream: uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT),
		Seq: 100, GuildId: other, Kind: pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE,
		Status:        pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING,
		NextAttemptMs: testNowMs + 30_000, DeadlineMs: testNowMs + econDeadlineMs, Payload: econDonatePayload(t, 100),
		RefId: 1, RefCount: 1, PeriodKey: econDayKey, CreatedMs: testNowMs, UpdatedMs: testNowMs,
		TxType: uint32(rollbackpb.TransactionType_TX_GUILD_DONATE), StreamEpoch: testNowMs,
	}
	econInsertOp(t, f, foreign)

	leaveAt, kickAt, disbandAt := testNowMs+100, testNowMs+200, testNowMs+300
	_, err = f.repo.LeaveGuild(f.ctx, guildID, leaver, leaveAt)
	require.NoError(t, err)
	rec := econRecord(t, f, leaverDonation.OpID)
	assert.Equal(t, leaveAt, rec.GetDeadlineMs(), "退帮:截止提前到 now")
	assert.Equal(t, leaveAt, rec.GetNextAttemptMs(), "退帮:next_attempt_ms 拉回 now")
	assert.Equal(t, leaverDonation.LeaseUntilMs, rec.GetLeaseUntilMs(), "不抢租约")
	assert.Equal(t, kickedDonation.DeadlineMs, econRecord(t, f, kickedDonation.OpID).GetDeadlineMs(), "别人的捐献不动")
	shopRec := econRecord(t, f, leaverShop.OpID)
	assert.Zero(t, shopRec.GetDeadlineMs(), "商店指令永不中止,不被提前截止")
	assert.Equal(t, leaverShop.LeaseUntilMs, shopRec.GetNextAttemptMs())
	foreignRec := econRecord(t, f, foreign.GetOpId())
	assert.Equal(t, foreign.GetDeadlineMs(), foreignRec.GetDeadlineMs(), "绑定别的帮会的捐献不动")
	assert.Equal(t, foreign.GetNextAttemptMs(), foreignRec.GetNextAttemptMs())

	_, err = f.repo.KickMember(f.ctx, guildID, leader, kicked, kickAt)
	require.NoError(t, err)
	assert.Equal(t, kickAt, econRecord(t, f, kickedDonation.OpID).GetDeadlineMs(), "被踢:截止提前到 now")

	_, err = f.repo.DisbandGuild(f.ctx, guildID, leader, disbandAt, nil)
	require.NoError(t, err)
	assert.Equal(t, disbandAt, econRecord(t, f, stayerDonation.OpID).GetDeadlineMs(), "解散:全体成员的捐献截止提前到 now")
	assert.Equal(t, leaveAt, econRecord(t, f, leaverDonation.OpID).GetDeadlineMs(), "已到期的行不会被再改")
	assert.Equal(t, kickAt, econRecord(t, f, kickedDonation.OpID).GetDeadlineMs())
}

// TestAccelerate_ChunksCandidatesAndSkipsNonMatching:提前截止改成"候选普通读 + 主键点改"之后的过滤与分块语义
// (2026-09-21 死锁修复,friend 审计 #5 / #10)。
//
// 150 个玩家跨两批(100 + 50)候选读,命中行分布在两批的首尾;五类不该动的行各放一条。直接在一个 RC 事务里调
// accelerateDonationDeadlines(它不依赖成员行 —— 成员锁是调用方的前提,不是它自己的步骤)。
// 再用更晚的 now 重放一次:已提前的行 deadline_ms 已不大于新 now,不再是候选,重放是 no-op。
func TestAccelerate_ChunksCandidatesAndSkipsNonMatching(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID     uint64 = 7951
		otherGuild  uint64 = 7952
		firstPlayer uint64 = 20_000
		players            = 150
		opBase      uint64 = 9_950_000
		epoch              = testNowMs - 100_000
	)
	payload := econDonatePayload(t, 100)
	playerIDs := make([]uint64, players)
	for i := range playerIDs {
		playerIDs[i] = firstPlayer + uint64(i)
	}

	// 命中:本帮、DEBIT 流、DONATE、PENDING、截止在 now 之后。下标 0 / 99 在第一批,100 / 149 在第二批。
	hits := map[uint64]uint64{} // op_id → player_id
	for k, idx := range []uint64{0, 99, 100, 149} {
		opID := opBase + uint64(k)
		econInsertOp(t, f, econDonateRecord(opID, firstPlayer+idx, guildID, epoch, 1, payload))
		hits[opID] = firstPlayer + idx
	}
	// 不命中的五类,各一行。
	shop := econShopRecord(t, opBase+10, firstPlayer+1, epoch, 1)
	shop.GuildId = guildID
	foreign := econDonateRecord(opBase+11, firstPlayer+2, otherGuild, epoch, 1, payload)
	terminal := econDonateRecord(opBase+12, firstPlayer+3, guildID, epoch, 1, payload)
	terminal.Status = pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_REJECTED
	due := econDonateRecord(opBase+13, firstPlayer+4, guildID, epoch, 1, payload)
	due.DeadlineMs = testNowMs // 已到期:过滤条件是 deadline_ms > now
	outsider := econDonateRecord(opBase+14, firstPlayer+500, guildID, epoch, 1, payload)
	untouched := []*pb.GuildAssetOpRecord{shop, foreign, terminal, due, outsider}
	for _, rec := range untouched {
		econInsertOp(t, f, rec)
	}

	accelerate := func(now uint64) {
		tx, err := f.db.BeginTx(f.ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		require.NoError(t, err)
		defer tx.Rollback()
		require.NoError(t, accelerateDonationDeadlines(f.ctx, tx, guildID, playerIDs, now))
		require.NoError(t, tx.Commit())
	}
	accelerateAt := testNowMs + 50
	accelerate(accelerateAt)

	for opID, playerID := range hits {
		rec := econRecord(t, f, opID)
		assert.Equal(t, accelerateAt, rec.GetDeadlineMs(), "op %d(player %d):截止提前到 now", opID, playerID)
		assert.Equal(t, accelerateAt, rec.GetNextAttemptMs(), "op %d:next_attempt_ms 拉回 now", opID)
		assert.Equal(t, accelerateAt, rec.GetUpdatedMs(), "op %d", opID)
		assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING, rec.GetStatus(), "op %d:只改截止,不终结", opID)
	}
	for _, want := range untouched {
		rec := econRecord(t, f, want.GetOpId())
		assert.Equal(t, want.GetDeadlineMs(), rec.GetDeadlineMs(), "op %d 不该被提前", want.GetOpId())
		assert.Equal(t, want.GetNextAttemptMs(), rec.GetNextAttemptMs(), "op %d", want.GetOpId())
		assert.Equal(t, want.GetUpdatedMs(), rec.GetUpdatedMs(), "op %d", want.GetOpId())
	}

	accelerate(accelerateAt + 1000)
	for opID := range hits {
		rec := econRecord(t, f, opID)
		assert.Equal(t, accelerateAt, rec.GetDeadlineMs(), "op %d:重放不能把已提前的截止再推后", opID)
		assert.Equal(t, accelerateAt, rec.GetUpdatedMs(), "op %d:重放是 no-op", opID)
	}
}

// ── 清理 ─────────────────────────────────────────────────────

// TestCleanupOnce_KeepsPendingAndPartial:只删保留期外的 APPLIED / REJECTED / ABORTED;APPLIED_PARTIAL(待人工补偿)
// 与 PENDING 永不自动删;过期的日键 / 周键计数行删掉,当期的保留。
func TestCleanupOnce_KeepsPendingAndPartial(t *testing.T) {
	f := openEconomyFixture(t)
	const player uint64 = 8691
	now := time.UnixMilli(int64(testNowMs))
	conf := CleanupConf{Interval: time.Minute, TerminalRetention: 30 * 24 * time.Hour, CounterRetention: 30 * 24 * time.Hour}
	oldMs := testNowMs - 31*econDayMs
	recentMs := testNowMs - econDayMs

	type opCase struct {
		opID     uint64
		status   pb.GuildAssetOpStatus
		finalMs  uint64
		survives bool
	}
	cases := []opCase{
		{701, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED, oldMs, false},
		{702, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_REJECTED, oldMs, false},
		{703, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_ABORTED, oldMs, false},
		{704, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED_PARTIAL, oldMs, true},
		{705, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING, oldMs, true},
		{706, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED, recentMs, true},
	}
	for i, c := range cases {
		rec := econShopRecord(t, c.opID, player, oldMs, uint64(i+1))
		rec.Status = c.status
		rec.NextAttemptMs = c.finalMs
		rec.CreatedMs, rec.UpdatedMs = oldMs, c.finalMs
		econInsertOp(t, f, rec)
	}

	insertCounter := func(periodKey uint32) {
		mustExec(t, f.ctx, f.db,
			`INSERT INTO guild_daily_counter (player_id, counter_kind, ref_id, period_key, used_count, updated_ms) VALUES (?, ?, ?, ?, ?, ?)`,
			player, int32(pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_SHOP), 101, periodKey, 1, testNowMs)
	}
	oldDay := gameday.DayKey(now.Add(-31 * 24 * time.Hour))
	recentDay := gameday.DayKey(now.Add(-24 * time.Hour))
	oldWeek := gameday.WeekKey(now.Add(-60 * 24 * time.Hour))
	currentWeek := gameday.WeekKey(now)
	for _, key := range []uint32{oldDay, recentDay, oldWeek, currentWeek} {
		insertCounter(key)
	}

	deleted := map[string]int64{}
	original := recordCleanupDeleted
	recordCleanupDeleted = func(table string, n int64) { deleted[table] += n }
	t.Cleanup(func() { recordCleanupDeleted = original })

	require.NoError(t, f.store.CleanupOnce(f.ctx, now, conf))

	for _, c := range cases {
		assert.Equal(t, c.survives, econOpExists(t, f, c.opID), "op %d status=%s", c.opID, c.status)
	}
	for key, survives := range map[uint32]bool{oldDay: false, recentDay: true, oldWeek: false, currentWeek: true} {
		_, found := econCounterUsed(t, f, player, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_SHOP, 101, key)
		assert.Equal(t, survives, found, "period_key=%d", key)
	}
	assert.Equal(t, map[string]int64{"guild_asset_op": 3, "guild_daily_counter": 2}, deleted)
}

// ── 读查询 ───────────────────────────────────────────────────

// TestRecentOps_OrderedByEpochThenSeqDesc:最近结果按 (stream_epoch, seq) 倒序,新纪元整体排在旧纪元之前
// (只按 seq 排会把库恢复前后的两个纪元交错);待结算按同一对键升序;别的流不混进来。
func TestRecentOps_OrderedByEpochThenSeqDesc(t *testing.T) {
	f := openEconomyFixture(t)
	const player uint64 = 8701
	oldEpoch, newEpoch := testNowMs-200_000, testNowMs-100_000

	insert := func(opID, epoch, seq uint64, status pb.GuildAssetOpStatus) {
		rec := econShopRecord(t, opID, player, epoch, seq)
		rec.Status = status
		econInsertOp(t, f, rec)
	}
	insert(801, oldEpoch, 1, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED)
	insert(802, oldEpoch, 2, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING)
	insert(803, oldEpoch, 3, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_REJECTED)
	insert(804, newEpoch, 1, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING)
	debit := econShopRecord(t, 805, player, newEpoch, 9)
	debit.Stream = uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT)
	econInsertOp(t, f, debit)

	recent, err := f.econ.RecentOps(f.ctx, player, assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT, 3)
	require.NoError(t, err)
	assert.Equal(t, []uint64{804, 803, 802}, econOpIDs(recent))

	pending, err := f.econ.PendingOps(f.ctx, player, assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT, 16)
	require.NoError(t, err)
	assert.Equal(t, []uint64{802, 804}, econOpIDs(pending))
	for _, row := range pending {
		assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING, row.Status)
		assert.Equal(t, assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT, row.Stream)
	}

	stuck, err := f.store.ListStuck(f.ctx, testNowMs, 10)
	require.NoError(t, err)
	assert.ElementsMatch(t, []uint64{802, 804, 805}, econOpIDs(stuck))
	row, found, err := f.store.GetOp(f.ctx, 803)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_REJECTED, row.Status)
	_, found, err = f.store.GetOp(f.ctx, 999)
	require.NoError(t, err)
	assert.False(t, found)
}

func econOpIDs(rows []AssetOpRow) []uint64 {
	out := make([]uint64, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.OpID)
	}
	return out
}

// ── 并发锁序回归(2026-09-21 死锁修复契约 P6,05 §5.39 ⑤)──────────

// TestEconomyLockOrder_ReserveDonationVersusFinalizeRejected:同一玩家"捐献预留"循环 ‖ "终结上一笔为 REJECTED"循环。
//
// 修复前的环(friend 审计 #4 / #7):预留里 AllocateSeq 的未决行查询带 FOR UPDATE,经 idx_guild_asset_op_2
// 先锁上一笔的二级项、再等它的聚簇记录;终结的主键 CAS 先锁聚簇记录、再要 delete-mark 同一条 idx_2 项(status 在里面)。
// 修复后(assetop 修法 A + 死锁复核 C6 的计数行守卫):
//   - 预留:guild(普通读)→ guild_member(p) X → guild_player_op_seq(p,DEBIT) X → 插新 op 行 → guild_daily_counter X;
//     对上一笔的 op 行**不加任何锁**(未决行是普通读)。
//   - 终结 REJECTED:guild_player_op_seq(p,DEBIT) X(计数行守卫)→ 上一笔 op 聚簇 X → 它的 idx_0 / idx_2 项 →
//     guild_daily_counter X(退次数)。
//
// 两边先在 seq 行上串行,再到计数行;终结从不要成员行,预留从不锁已有的 op 行 —— 只会单向等待。
// MySQL 上修 C6 之前也不成环(计数行两边都只拿聚簇这一把);TiDB 上 IODKU 与退款的 Point_Get 会在计数行的
// {行 key, PRIMARY key} 上各持一半,seq 行守卫之后才不再有这种可能。
//
// 形状:预留协程每成功一笔就把 op_id 交给终结协程(无缓冲通道),交接完立刻预留下一笔,于是"预留第 i+1 笔"
// 与"终结第 i 笔"恰好重叠;任一时刻未决行至多两笔,不会撞 MaxPending。判据:两边零错误、全部行终结、
// 次数全额退回,且 econDeadlockWatch 看不到本库的新死锁。
func TestEconomyLockOrder_ReserveDonationVersusFinalizeRejected(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7961
		leader  uint64 = 8961
		donor   uint64 = 8962
		opBase  uint64 = 9_960_000
		rounds         = 200
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{donor: constants.RoleMember})
	ctx, cancel := context.WithTimeout(context.Background(), econLockOrderBudget)
	defer cancel()
	f.ctx = ctx
	watch := econWatchDeadlocks(t, f)

	// 入参全部在主 goroutine 里造好:econDonation 内部有 require,不能在子 goroutine 里调。
	inputs := make([]DonationReserve, rounds)
	for i := range inputs {
		inputs[i] = econDonation(t, opBase+uint64(i), donor, guildID, testNowMs+uint64(i))
		inputs[i].DailyLimit = 1000
	}
	rejected := econRejectedResult()

	var (
		wg           sync.WaitGroup
		reserveErr   error   // 只由预留协程写
		finalizeErrs []error // 只由终结协程写
		reserved     = make(chan uint64)
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer close(reserved)
		for i, in := range inputs {
			if _, err := f.econ.ReserveDonation(ctx, in); err != nil {
				reserveErr = fmt.Errorf("第 %d 笔预留: %w", i, err)
				return
			}
			reserved <- in.OpID
		}
	}()
	go func() {
		defer wg.Done()
		// 一直收到通道关闭:预留协程中途失败也会 close,这里不会让它卡在发送上。
		for opID := range reserved {
			finalized, err := f.store.Finalize(ctx, assetop.Op{OpID: opID, PlayerID: donor},
				assetop.StatusRejected, rejected, testNowMs+rounds+(opID-opBase))
			switch {
			case err != nil:
				finalizeErrs = append(finalizeErrs, fmt.Errorf("终结 op %d: %w", opID, err))
			case !finalized:
				finalizeErrs = append(finalizeErrs, fmt.Errorf("终结 op %d: CAS 落空(只有本协程终结它,不该落空)", opID))
			}
		}
	}()
	wg.Wait()

	require.NoError(t, reserveErr)
	require.Empty(t, finalizeErrs)
	var rejectedRows int
	require.NoError(t, f.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM guild_asset_op WHERE player_id=? AND status=?",
		donor, int32(pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_REJECTED)).Scan(&rejectedRows))
	assert.Equal(t, rounds, rejectedRows, "每一笔都被终结成 REJECTED")
	used, _ := econCounterUsed(t, f, donor, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_DONATE, 1, econDayKey)
	assert.Zero(t, used, "REJECTED 全额退次数")
	assert.Equal(t, uint64(rounds+1), econNextSeq(t, f, donor, assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT))
	watch.assertNone(t, ctx, "捐献预留 ‖ 终结 REJECTED")
}

// TestEconomyLockOrder_LeaveVersusCleanupVersusFinalize:离帮(含提前截止)‖ 清理 ‖ 终结同一玩家的未决捐献,多轮三方同时放行。
//
// 修复前的环(friend 审计 #5 / #10):提前截止是一条经二级索引的多条件 UPDATE,走 idx_2 时先锁本玩家历史终态行的
// 二级项、再等聚簇记录;清理的范围 DELETE 已持有同一行聚簇记录、要 delete-mark 那条二级项 —— 反序;走 idx_2 / idx_0
// 时还与终结 CAS 在未决行上反序。每轮因此都给玩家预置一批**保留期外、同一 (player, stream, epoch) 前缀、同一 guild_id**
// 的终态行,让修复前的计划恰好扫过它们。
//
// 修复后三方的取锁序列:
//   - 离帮:guild(A) X → guild_player_state(p) X → guild_member(A,p) X → (删申请:主键点删)→
//     候选**普通读**(不锁)→ 按 op_id 升序逐行:op 聚簇 X → 它的 idx_0 项;
//   - 清理:每行一个 RC 短事务,只锁一行:op 聚簇 X(点锁)→ 点删它的全部二级项;计数行同理;
//   - 终结 REJECTED:seq(p,DEBIT) X → op 聚簇 X → idx_0 / idx_2 项 → guild_daily_counter X。
//
// 任何事务都先拿 op 行的聚簇记录、再碰它的二级项;没有谁经二级索引锁 op 行 —— 不存在反向边,不成环。
//
// 每轮判据:三方零错误;第二笔(没被终结的)未决捐献被提前到离帮时刻(候选读是完整的);本轮预置的旧终态行
// 全部被清理删掉(清理确实跑了,不是空转)。全部轮次结束后 econDeadlockWatch 看不到本库的新死锁。
func TestEconomyLockOrder_LeaveVersusCleanupVersusFinalize(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID     uint64 = 7971
		leader      uint64 = 8971
		donor       uint64 = 8972
		opBase      uint64 = 9_970_000
		oldOpBase   uint64 = 9_980_000
		rounds             = 30
		oldPerRound        = 200
		roundStepMs uint64 = 10_000
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{donor: constants.RoleMember})
	// 离帮事务要锁本人的 guild_player_state 行(契约 P4);LeaveGuild 自己会在事务外补建,这里先建好只是让夹具不依赖那一步。
	mustExec(t, f.ctx, f.db, "INSERT IGNORE INTO guild_player_state (player_id, updated_ms) VALUES (?, ?)", donor, testNowMs)
	ctx, cancel := context.WithTimeout(context.Background(), econLockOrderBudget)
	defer cancel()
	f.ctx = ctx
	watch := econWatchDeadlocks(t, f)

	conf := CleanupConf{Interval: time.Minute, TerminalRetention: 30 * 24 * time.Hour, CounterRetention: 30 * 24 * time.Hour}
	payload := econDonatePayload(t, 100)
	rejected := econRejectedResult()
	aborted := assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED, Durable: true}

	for round := uint64(0); round < rounds; round++ {
		now := testNowMs + round*roundStepMs
		if round > 0 {
			seedMemberRow(t, ctx, f.db, guildID, donor, constants.RoleMember) // 上一轮离帮了,重新入帮
		}
		first := econDonation(t, opBase+2*round, donor, guildID, now)
		second := econDonation(t, opBase+2*round+1, donor, guildID, now+1)
		first.DailyLimit, second.DailyLimit = 10_000, 10_000
		reservedFirst, err := f.econ.ReserveDonation(ctx, first)
		require.NoError(t, err, "第 %d 轮第一笔预留", round)
		_, err = f.econ.ReserveDonation(ctx, second)
		require.NoError(t, err, "第 %d 轮第二笔预留", round)

		// 保留期外的旧终态行:seq 取 100 万起的独立区段,不与真实分配的 seq 撞唯一键。
		oldLo := oldOpBase + round*oldPerRound
		olds := make([]*pb.GuildAssetOpRecord, 0, oldPerRound)
		for k := uint64(0); k < oldPerRound; k++ {
			rec := econDonateRecord(oldLo+k, donor, guildID, reservedFirst.StreamEpoch, 1_000_000+round*oldPerRound+k, payload)
			rec.Status = pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED
			rec.NextAttemptMs = now - 31*econDayMs
			rec.CreatedMs, rec.UpdatedMs = rec.NextAttemptMs, rec.NextAttemptMs
			olds = append(olds, rec)
		}
		econInsertOpsBulk(t, f, olds)

		var finalized bool // 只由第三个闭包写,econRunTogether 返回之后才读
		errs := econRunTogether(
			func() error {
				_, err := f.repo.LeaveGuild(ctx, guildID, donor, now)
				return err
			},
			func() error { return f.store.CleanupOnce(ctx, time.UnixMilli(int64(now)), conf) },
			func() error {
				var err error
				finalized, err = f.store.Finalize(ctx, assetop.Op{OpID: first.OpID, PlayerID: donor},
					assetop.StatusRejected, rejected, now)
				return err
			})
		require.NoError(t, errs[0], "第 %d 轮离帮", round)
		require.NoError(t, errs[1], "第 %d 轮清理", round)
		require.NoError(t, errs[2], "第 %d 轮终结", round)
		require.True(t, finalized, "第 %d 轮:第一笔只有这一个终结者", round)

		assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_REJECTED, econRecord(t, f, first.OpID).GetStatus())
		rec := econRecord(t, f, second.OpID)
		require.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING, rec.GetStatus(), "第 %d 轮第二笔", round)
		assert.Equal(t, now, rec.GetDeadlineMs(), "第 %d 轮:候选读漏了第二笔,截止没被提前", round)
		assert.Equal(t, now, rec.GetNextAttemptMs(), "第 %d 轮:next_attempt_ms 没被拉回离帮时刻", round)
		assert.Zero(t, memberCount(t, ctx, f.db, donor), "第 %d 轮:离帮之后成员行必须不在", round)
		assert.Zero(t, econOpCountInRange(t, f, oldLo, oldLo+oldPerRound-1), "第 %d 轮:保留期外的终态行应被清理删光", round)

		// 收尾:第二笔也终结掉,免得未决行跨轮累积撞上 MaxPending。
		finalized, err = f.store.Finalize(ctx, assetop.Op{OpID: second.OpID, PlayerID: donor},
			assetop.StatusAborted, aborted, now+2)
		require.NoError(t, err, "第 %d 轮收尾", round)
		require.True(t, finalized, "第 %d 轮收尾", round)
	}
	watch.assertNone(t, ctx, "离帮 ‖ 清理 ‖ 终结")
}

// ── 2026-09-21 第二轮复核补的回归 ──────────────────────────────

// TestReserveDonation_FirstSeqRowCreatorsSerializeOnMember:死锁复核 C2(G-C3 的 seq 行形态,friend 审计 #16)的确定性回归。
//
// 首插者 A 在事务里先锁 p 的成员行、再插 seq 行 (p, DEBIT) 未提交(与生产的 T-D 同序);B、C 两笔捐献预留同时放行,然后 A 回滚。
// 修复前(事务外自动提交建行):B、C 的前置普通读看不见 A 的未提交行,都在事务外发 INSERT IGNORE、排在 A 的 seq 记录上等 S;
// A 回滚后两者的 S 在 RC 下被继承成同一段间隙上的间隙 S,插入意向互挡,1213(被重试吸收,经 txDeadlockObserved 记下)。
// 修复后:B、C 在事务里先锁成员行,都排在 A 的成员行锁上,碰不到 seq 记录;A 回滚后先拿到成员行的一方在事务内建行、分配、
// 提交,另一方随后读到已提交的行直接分配。
// 判据:钩子与 econDeadlockWatch 都严格为零;两笔都成功、seq 恰为 {1, 2}、表里恰好一行。
// 不会 1205:A 至多持有约 500ms,小于 innodb_lock_wait_timeout(1s)。
func TestReserveDonation_FirstSeqRowCreatorsSerializeOnMember(t *testing.T) {
	f := openEconomyFixture(t)
	deadlocks := watchGuildTxDeadlocks(t)
	const (
		guildID uint64 = 7985
		leader  uint64 = 8987
		player  uint64 = 8985
		stream         = assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{player: constants.RoleMember})
	watch := econWatchDeadlocks(t, f)
	// 入参在主 goroutine 里造(econDonation 内部有 require),goroutine 里只调被测接口。
	inB := econDonation(t, 9_985_001, player, guildID, testNowMs+1)
	inC := econDonation(t, 9_985_002, player, guildID, testNowMs+2)

	first, err := f.db.BeginTx(f.ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer first.Rollback() // 下面已显式回滚时是空操作
	var role uint32
	require.NoError(t, first.QueryRowContext(f.ctx, sqlLockMemberRole, guildID, player).Scan(&role))
	require.NoError(t, ensureSeqRowTx(f.ctx, first, player, stream, testNowMs))

	var resB, resC Reserved // 只由各自的 goroutine 写;从通道取到结果之后主 goroutine 才读
	b := lockOrderGo(func() error {
		res, err := f.econ.ReserveDonation(f.ctx, inB)
		resB = res
		return err
	})
	c := lockOrderGo(func() error {
		res, err := f.econ.ReserveDonation(f.ctx, inC)
		resC = res
		return err
	})
	// 修复后两个锁等待都在成员行上(修复前在 seq 记录上)。锁等待上限是 1s:最多等到 500ms 就回滚首插者。
	lockOrderAwaitLockWaits(f.ctx, f.db, 2, 500*time.Millisecond)
	require.NoError(t, first.Rollback())

	errB, errC := <-b, <-c
	require.NoError(t, errB, "首插者回滚后 B 必须成功")
	require.NoError(t, errC, "首插者回滚后 C 必须成功")
	assert.ElementsMatch(t, []uint64{1, 2}, []uint64{resB.Seq, resC.Seq}, "两笔在成员行上串行,seq 连续")
	var rows int
	require.NoError(t, f.db.QueryRowContext(f.ctx,
		"SELECT COUNT(*) FROM "+guildPlayerOpSeqTable+" WHERE player_id=? AND stream=?", player, uint32(stream)).Scan(&rows))
	assert.Equal(t, 1, rows, "B、C 合起来恰好建出一行")
	deadlocks.assertNone(t, "首次建 seq 行:首插者回滚(建行者在成员行上串行)")
	watch.assertNone(t, f.ctx, "首次建 seq 行:首插者回滚")
}

// TestEnsureSeqRowTx_MatchesAssetopEnsureSeqRow:sqlEnsureSeqRow 是 assetop.EnsureSeqRow 建行语句的一份有记录的副本
// (DRY 偏离说明见该常量)。两条路径各给一个玩家建行,逐列比对 (next_seq, epoch, updated_ms);再各调一次,已存在即空操作、
// 纪元不许被改写。任何一边改了建行语义这里就红 —— assetop 提供事务版建行之后,本用例随常量一起删掉。
func TestEnsureSeqRowTx_MatchesAssetopEnsureSeqRow(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		viaAssetop uint64 = 8986
		viaTx      uint64 = 8988
		stream            = assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT
	)
	ensureInTx := func(now uint64) error {
		tx, err := f.db.BeginTx(f.ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		if err != nil {
			return err
		}
		defer tx.Rollback() // 已提交时是空操作
		if err := ensureSeqRowTx(f.ctx, tx, viaTx, stream, now); err != nil {
			return err
		}
		return tx.Commit()
	}
	type seqRow struct{ nextSeq, epoch, updatedMs uint64 }
	read := func(playerID uint64) seqRow {
		var row seqRow
		require.NoError(t, f.db.QueryRowContext(f.ctx,
			"SELECT next_seq, epoch, updated_ms FROM "+guildPlayerOpSeqTable+" WHERE player_id=? AND stream=?",
			playerID, uint32(stream)).Scan(&row.nextSeq, &row.epoch, &row.updatedMs))
		return row
	}

	require.NoError(t, assetop.EnsureSeqRow(f.ctx, f.db, f.econ.seq, viaAssetop, stream, testNowMs))
	require.NoError(t, ensureInTx(testNowMs))
	want := seqRow{nextSeq: 1, epoch: testNowMs, updatedMs: testNowMs}
	assert.Equal(t, want, read(viaAssetop), "assetop.EnsureSeqRow 的建行语义")
	assert.Equal(t, read(viaAssetop), read(viaTx), "事务内建行必须与 assetop 逐列同义")

	require.NoError(t, assetop.EnsureSeqRow(f.ctx, f.db, f.econ.seq, viaAssetop, stream, testNowMs+1))
	require.NoError(t, ensureInTx(testNowMs+1))
	assert.Equal(t, want, read(viaAssetop), "已存在即空操作,纪元只在建行时写一次")
	assert.Equal(t, want, read(viaTx), "已存在即空操作,纪元只在建行时写一次")
}

// TestEconomyLockOrder_ReserveVersusCleanupSameStream:审计 #15 —— 预留与清理在同一 (player, stream, epoch) 前缀上对撞。
//
// 修复前的环:AllocateSeq 的未决行查询带 FOR UPDATE,执行计划若走 uk_guild_asset_op(player_id, stream, stream_epoch, seq)
// 或 idx_2 的等值前缀,会先锁到本玩家本纪元历史行的二级项、再去等聚簇记录 —— 包括保留期外的终态行;清理的 DELETE
// 先锁聚簇记录、再 delete-mark 同一条二级项。两边在终态行上反序。只有 guild 有清理,所以回归放在这里。
// 修复后:
//   - 捐献 / 兑换预留:guild(普通读)→ guild_member(p) X → guild_player_op_seq(p, stream) X → 未决行**普通读** →
//     插新 op 行 → guild_daily_counter(捐献)/ 回到已锁的成员行扣帮贡(兑换);对任何历史 op 行都不加锁;
//   - 清理:候选普通读 → 每行一个 RC 短事务只锁一行(聚簇点锁 → 点删它的二级项)。
// 两边没有共享的锁,不可能成环。每轮给两条流各预置 100 条同纪元、保留期外的终态行,让修复前的计划恰好扫过它们。
func TestEconomyLockOrder_ReserveVersusCleanupSameStream(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID     uint64 = 7981
		leader      uint64 = 8981
		player      uint64 = 8982
		opBase      uint64 = 9_990_000
		oldOpBase   uint64 = 9_100_000
		rounds             = 30
		oldPerRound        = 200 // 一半在捐献流,一半在兑换流
		roundStepMs uint64 = 10_000
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{player: constants.RoleMember})
	econSetContribution(t, f, guildID, player, 1000, 1000)
	// 先建两条流的 seq 行,纪元定为 testNowMs:预置的旧终态行与之后的预留同纪元,才落在同一 (player, stream, epoch) 前缀上。
	require.NoError(t, assetop.EnsureSeqRow(f.ctx, f.db, f.econ.seq, player, assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT, testNowMs))
	require.NoError(t, assetop.EnsureSeqRow(f.ctx, f.db, f.econ.seq, player, assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT, testNowMs))
	ctx, cancel := context.WithTimeout(context.Background(), econLockOrderBudget)
	defer cancel()
	f.ctx = ctx
	watch := econWatchDeadlocks(t, f)

	conf := CleanupConf{Interval: time.Minute, TerminalRetention: 30 * 24 * time.Hour, CounterRetention: 30 * 24 * time.Hour}
	payload := econDonatePayload(t, 100)
	rejected := econRejectedResult()

	for round := uint64(0); round < rounds; round++ {
		now := testNowMs + round*roundStepMs
		donation := econDonation(t, opBase+2*round, player, guildID, now)
		donation.DailyLimit = 10_000
		shop := econShopOrder(t, opBase+2*round+1, player, guildID, 1, 30, 0, now)

		oldLo := oldOpBase + round*oldPerRound
		olds := make([]*pb.GuildAssetOpRecord, 0, oldPerRound)
		for k := uint64(0); k < oldPerRound; k++ {
			// seq 取 100 万起的独立区段,不与真实分配的 seq 撞唯一键。
			rec := econDonateRecord(oldLo+k, player, guildID, testNowMs, 1_000_000+round*oldPerRound+k, payload)
			if k%2 == 1 {
				rec.Stream = uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT)
				rec.Kind = pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_SHOP
				rec.TxType = uint32(rollbackpb.TransactionType_TX_GUILD_SHOP)
			}
			rec.Status = pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED
			rec.NextAttemptMs = now - 31*econDayMs
			rec.CreatedMs, rec.UpdatedMs = rec.NextAttemptMs, rec.NextAttemptMs
			olds = append(olds, rec)
		}
		econInsertOpsBulk(t, f, olds)

		errs := econRunTogether(
			func() error { _, err := f.econ.ReserveDonation(ctx, donation); return err },
			func() error { _, err := f.econ.ReserveShopOrder(ctx, shop); return err },
			func() error { return f.store.CleanupOnce(ctx, time.UnixMilli(int64(now)), conf) },
		)
		require.NoError(t, errs[0], "第 %d 轮捐献预留", round)
		require.NoError(t, errs[1], "第 %d 轮兑换预留", round)
		require.NoError(t, errs[2], "第 %d 轮清理", round)
		assert.Zero(t, econOpCountInRange(t, f, oldLo, oldLo+oldPerRound-1), "第 %d 轮:保留期外的终态行应被清理删光", round)

		// 收尾:两笔都终结掉,免得未决行跨轮累积撞上 MaxPending。它们的终结时刻在保留期内,之后的清理不会删它们。
		for _, opID := range []uint64{donation.OpID, shop.OpID} {
			finalized, err := f.store.Finalize(ctx, assetop.Op{OpID: opID, PlayerID: player}, assetop.StatusRejected, rejected, now+1)
			require.NoError(t, err, "第 %d 轮收尾 op %d", round, opID)
			require.True(t, finalized, "第 %d 轮收尾 op %d", round, opID)
		}
	}
	watch.assertNone(t, ctx, "捐献 / 兑换预留 ‖ 同一 (player, stream, epoch) 前缀上的清理")
}

// TestEconomyLockOrder_MemberLockBlocksReserveDonation:提前截止候选集完整性的前提(审计 #10 修法第 2 点),确定性版本。
//
// 离帮 / 被踢 / 解散的提前截止只做**普通读**取候选,它之所以不漏行,靠的是:调用方已持有这些人的成员行 X 锁,
// 而 ReserveDonation 插 PENDING DONATE 行之前必须先 FOR UPDATE 同一成员行。这里用占锁事务扮演"持成员锁的离帮事务":
// 锁住 (G,p) 后放一笔捐献预留,等它在库里锁等待,再删掉成员行并提交 —— 预留必须失败(ErrNotGuildMember;慢机器上等满
// 1s 则是 ErrWriteConflict,两者都证明它被挡住了),且不留指令行、不占次数。
// 若有人把预留里的成员锁拿掉,它会在占锁事务提交之前就插进一行 —— 那一行正是离帮的候选读看不见的。
func TestEconomyLockOrder_MemberLockBlocksReserveDonation(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7991
		leader  uint64 = 8991
		donor   uint64 = 8992
		opID    uint64 = 9_991_001
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{donor: constants.RoleMember})
	in := econDonation(t, opID, donor, guildID, testNowMs)
	// 先把 seq 行建好(纪元定为 testNowMs):预留在事务里第一把锁就是成员行,等的只可能是它;seq 行在不在不影响这一点。
	require.NoError(t, assetop.EnsureSeqRow(f.ctx, f.db, f.econ.seq, donor, assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT, testNowMs))

	holder, err := f.db.BeginTx(f.ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer holder.Rollback() // 下面已提交时是空操作
	var role uint32
	require.NoError(t, holder.QueryRowContext(f.ctx, sqlLockMemberRole, guildID, donor).Scan(&role))

	done := lockOrderGo(func() error { _, err := f.econ.ReserveDonation(f.ctx, in); return err })
	// 锁等待上限是 1s:最多等到 500ms 就删行提交。
	lockOrderAwaitLockWaits(f.ctx, f.db, 1, 500*time.Millisecond)
	_, err = holder.ExecContext(f.ctx, sqlDeleteMember, guildID, donor)
	require.NoError(t, err)
	require.NoError(t, holder.Commit())

	err = <-done
	require.Error(t, err, "持成员锁期间的捐献预留必须被挡住,不能先插进一行")
	assertErrorIn(t, err, ErrNotGuildMember, ErrWriteConflict)
	assert.False(t, econOpExists(t, f, opID), "被挡住的预留不能留下指令行")
	_, found := econCounterUsed(t, f, donor, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_DONATE, 1, econDayKey)
	assert.False(t, found, "被挡住的预留不能占次数")
}

// TestEconomyLockOrder_LeaveVersusReserveDonationCandidateComplete:同一前提的并发版本(审计 #10 修法第 2 点)。
//
// 离帮 ‖ 同一玩家的捐献预留,多轮同时放行。两种先后都必须落在合法结局:
//   - 预留先拿到成员行锁并提交:离帮随后拿到成员锁,候选普通读看得见这一行,截止被提前到离帮时刻;
//   - 离帮先拿到成员行锁:预留排在它后面,离帮删行提交后预留锁读不到成员 → ErrNotGuildMember,不留行。
// 不合法的结局只有一种:预留成功、成员已不在,而这一行的截止没被提前 —— 那就是候选集漏行。
// 取锁序列(两边第一把共享锁就是成员行):离帮 guild(G) → state(p) → member(G,p) → 申请 → op 聚簇 → idx_0;
// 预留 guild(普通读)→ member(G,p) → seq(p) → 插 op → counter。全部轮次后 econDeadlockWatch 看不到本库的新死锁。
func TestEconomyLockOrder_LeaveVersusReserveDonationCandidateComplete(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID     uint64 = 7993
		leader      uint64 = 8993
		donor       uint64 = 8994
		opBase      uint64 = 9_993_000
		rounds             = 30
		roundStepMs uint64 = 10_000
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{donor: constants.RoleMember})
	mustExec(t, f.ctx, f.db, "INSERT IGNORE INTO guild_player_state (player_id, updated_ms) VALUES (?, ?)", donor, testNowMs)
	require.NoError(t, assetop.EnsureSeqRow(f.ctx, f.db, f.econ.seq, donor, assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT, testNowMs))
	ctx, cancel := context.WithTimeout(context.Background(), econLockOrderBudget)
	defer cancel()
	f.ctx = ctx
	watch := econWatchDeadlocks(t, f)
	aborted := assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED, Durable: true}

	reservedRounds := 0
	for round := uint64(0); round < rounds; round++ {
		now := testNowMs + round*roundStepMs
		leaveAt := now + 5
		if round > 0 {
			seedMemberRow(t, ctx, f.db, guildID, donor, constants.RoleMember) // 上一轮离帮了,重新入帮
		}
		in := econDonation(t, opBase+round, donor, guildID, now)
		in.DailyLimit = 10_000

		errs := econRunTogether(
			func() error { _, err := f.econ.ReserveDonation(ctx, in); return err },
			func() error { _, err := f.repo.LeaveGuild(ctx, guildID, donor, leaveAt); return err },
		)
		require.NoError(t, errs[1], "第 %d 轮离帮", round)
		assert.Zero(t, memberCount(t, ctx, f.db, donor), "第 %d 轮:离帮之后成员行必须不在", round)

		if errs[0] != nil {
			assert.ErrorIs(t, errs[0], ErrNotGuildMember, "第 %d 轮:预留只能因为已不是成员而失败", round)
			assert.False(t, econOpExists(t, f, in.OpID), "第 %d 轮:失败的预留不能留下指令行", round)
			continue
		}
		reservedRounds++
		rec := econRecord(t, f, in.OpID)
		require.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING, rec.GetStatus(), "第 %d 轮", round)
		assert.Equal(t, leaveAt, rec.GetDeadlineMs(), "第 %d 轮:预留先提交,离帮的候选读却漏了它,截止没被提前", round)
		assert.Equal(t, leaveAt, rec.GetNextAttemptMs(), "第 %d 轮:next_attempt_ms 没被拉回离帮时刻", round)

		// 收尾:终结掉,免得未决行跨轮累积撞上 MaxPending。
		finalized, err := f.store.Finalize(ctx, assetop.Op{OpID: in.OpID, PlayerID: donor},
			assetop.StatusAborted, aborted, leaveAt+1)
		require.NoError(t, err, "第 %d 轮收尾", round)
		require.True(t, finalized, "第 %d 轮收尾", round)
	}
	t.Logf("%d 轮中预留先提交 %d 轮、离帮先拿到成员锁 %d 轮", rounds, reservedRounds, rounds-reservedRounds)
	watch.assertNone(t, ctx, "离帮 ‖ 捐献预留(候选集完整性)")
}

// TestEconomyLockOrder_BackgroundCASQueuesBehindPessimisticWriter:G-C2 回归(TiDB 上才有鉴别力)。
//
// 形状:写者 B 在显式 RC 事务里对一行未决捐献做提前截止(离帮 / 被踢 / 解散事务里的那两条:sqlLockAssetOp 主键点锁 →
// sqlAccelerateDonationDeadline 点改,与 accelerateDonationDeadlines 同序),暂不提交;
// 此时循环对同一行做重排(Reschedule)或推迟毒行(markPoison);随后 B 提交。
//   - 修复前在 TiDB 上:重排 / 毒行是自动提交 = 乐观事务,prewrite 按 region 并行,先锁住 idx_0 旧项、再在行 key 上撞到
//     B 的悲观锁;B 提交时要删的正是这条 idx_0 旧项 —— 只能等乐观锁 TTL(小事务约 3s)过期,B 的 COMMIT 因此卡约 3s,
//     且这类互等不进 TiKV 死锁检测器(INFORMATION_SCHEMA.DEADLOCKS 里没有记录)。
//   - 修复后:重排 / 毒行在显式事务里首句 sqlLockAssetOp 点锁(PRIMARY key → 行 key,V1),在 B 已持有的 PRIMARY key 上
//     排队,只等到 B 提交;B 立即提交。
//   - MySQL:两边都是"聚簇 → idx_0",修复前后都只是单向等待;这里跑的是行为回归(进事务后语义不变:排在 B 后面,
//     B 提交后照常写进去)。
// 判据:B 的 COMMIT 耗时 < commitCeiling(比 TiDB 乐观锁默认 TTL 短一截;修复后是毫秒级),重排 / 毒行最终写入。
// 这一条上限就是判据本身(症状是"COMMIT 卡满 TTL"),不是可有可无的墙钟断言。
// Codex 请把 GUILD_TEST_MYSQL_DSN 指向 TiDB(tidb_txn_mode=pessimistic、pessimistic-auto-commit 保持默认 false)再跑一遍,
// 交付里写明两种库各自 PASS。
func TestEconomyLockOrder_BackgroundCASQueuesBehindPessimisticWriter(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7995
		player  uint64 = 8995
		token   uint64 = 0x6b6b
		// commitCeiling:TiDB 小事务乐观锁的默认 TTL 约 3s;修复后的 COMMIT 不等任何人。
		commitCeiling = 2 * time.Second
	)
	payload := econDonatePayload(t, 100)
	acceleratedAt := testNowMs + 1
	cases := []struct {
		name  string
		opID  uint64
		seq   uint64
		run   func(op assetop.Op) error
		check func(t *testing.T, rec *pb.GuildAssetOpRecord)
	}{
		{
			name: "Reschedule", opID: 9_995_001, seq: 1,
			run: func(op assetop.Op) error {
				res := assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_RETRY, Reason: assetop.ReasonInBattle}
				return f.store.Reschedule(f.ctx, op, testNowMs+5000, res, testNowMs+2)
			},
			check: func(t *testing.T, rec *pb.GuildAssetOpRecord) {
				assert.Equal(t, uint32(1), rec.GetAttempts())
				assert.Equal(t, testNowMs+5000, rec.GetNextAttemptMs(), "重排排在 B 之后写入")
				assert.Zero(t, rec.GetLeaseUntilMs())
			},
		},
		{
			name: "markPoison", opID: 9_995_002, seq: 2,
			run: func(op assetop.Op) error {
				f.store.markPoison(f.ctx, op.OpID, op.LeaseToken, testNowMs+2, testNowMs+3_600_000)
				return nil
			},
			check: func(t *testing.T, rec *pb.GuildAssetOpRecord) {
				assert.Equal(t, testNowMs+3_600_000, rec.GetNextAttemptMs(), "毒行推迟排在 B 之后写入")
				assert.Equal(t, uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN), rec.GetLastOutcome())
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := econDonateRecord(tc.opID, player, guildID, testNowMs, tc.seq, payload)
			// 同步投递期间的形状:握着租约,next_attempt_ms = 租约到期时刻 > now(提前截止的 LEAST 真的会改 idx_0)。
			rec.LeaseToken = token
			rec.LeaseUntilMs = testNowMs + econLeaseMs
			econInsertOp(t, f, rec)

			writer, err := f.db.BeginTx(f.ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
			require.NoError(t, err)
			defer writer.Rollback() // 下面已提交时是空操作
			// 与生产的 accelerateDonationDeadlines 同序:先主键点锁,再带复核条件点改(这里要拿到影响行数,所以不直接调它)。
			found, err := lockRowExists(f.ctx, writer, sqlLockAssetOp, tc.opID)
			require.NoError(t, err)
			require.True(t, found, "夹具行必须存在")
			res, err := writer.ExecContext(f.ctx, sqlAccelerateDonationDeadline,
				acceleratedAt, acceleratedAt, acceleratedAt, tc.opID, PendingStatus(), acceleratedAt)
			require.NoError(t, err)
			n, err := res.RowsAffected()
			require.NoError(t, err)
			require.Equal(t, int64(1), n, "夹具行必须命中提前截止")

			done := lockOrderGo(func() error {
				return tc.run(assetop.Op{OpID: tc.opID, PlayerID: player, LeaseToken: token})
			})
			lockOrderAwaitLockWaits(f.ctx, f.db, 1, 300*time.Millisecond)
			start := time.Now()
			require.NoError(t, writer.Commit())
			commitElapsed := time.Since(start)
			require.NoError(t, <-done)

			assert.Less(t, commitElapsed, commitCeiling,
				"持锁写者的 COMMIT 被后台写卡了 %v:在 TiDB 上这就是 G-C2(自动提交的乐观 prewrite 先占了 idx_0 旧项)", commitElapsed)
			stored := econRecord(t, f, tc.opID)
			assert.Equal(t, acceleratedAt, stored.GetDeadlineMs(), "B 的提前截止已提交")
			assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING, stored.GetStatus())
			tc.check(t, stored)
		})
	}
}

// ── 2026-09-21 第四轮(死锁复核 C6)────────────────────────────

// TestEconomyLockOrder_ShopOrphanRefundVersusReserveElsewhere:C6 的兑换孤儿形态(计数行守卫)。
//
// p 在帮 A 下过 orphans 笔限购商品 101 的兑换(PENDING;商店指令永不中止),随后离开 A、加入 B。每轮同时放行:
//   - 在 B 兑换同一商品(T-S):guild(B) 普通读 → member(B,p) X → seq(p,CREDIT) X → 插新 op → counter(p,SHOP,101,今日) IODKU
//     → 回到 member(B,p) 扣帮贡;
//   - 把 A 的一笔旧订单终结成 REJECTED(退限购):member(A,p) 锁不到(已离帮,RC 下未命中不加锁)→ seq(p,CREDIT) X(C6 守卫)
//     → op 聚簇 X → idx_0 / idx_2 → counter(p,SHOP,101,今日) 退款。
// 修复前两边除计数行外没有公共锁:TiDB 下 IODKU 在语句末尾并行锁计数行的 {行 key, PRIMARY key},退款的 Point_Get 先 PRIMARY key
// 后行 key,可能各持一半互等(1213)。修复后两边先在 seq 行上串行,计数行上不再同时在途。
// MySQL 上两边在计数行上都只拿聚簇这一把,修复前后都不成环 —— 这里只是行为回归;C6 的鉴别要把 DSN 指向 TiDB
// (悲观模式、tidb_lock_unchanged_keys 保持默认 ON、pessimistic-txn.deadlock-history-collect-retryable = true)再跑一遍。
// 判据:econDeadlockWatch 与 txDeadlockObserved 钩子都零新增;两边零错误;旧订单全部 REJECTED 且各计一次
// shop/refund_member_gone 孤儿;最终今日限购占用 = 新订单份数(旧订单全额退回),B 里的帮贡只扣新订单。
func TestEconomyLockOrder_ShopOrphanRefundVersusReserveElsewhere(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		oldGuild  uint64 = 7996
		newGuild  uint64 = 7997
		oldLeader uint64 = 8996
		newLeader uint64 = 8997
		buyer     uint64 = 8998
		oldBase   uint64 = 9_996_000
		newBase   uint64 = 9_997_000
		orphans          = 12 // 旧未决 12 笔 + 每轮至多 1 笔新未决 ≤ assetop.DefaultLimits.MaxPending(16)
		cost      uint64 = 30
		limit     uint32 = 1000
		balance   uint64 = 10_000
	)
	seedManagedGuild(t, f.ctx, f.db, oldGuild, 2, 1, 50, oldLeader, map[uint64]uint32{buyer: constants.RoleMember})
	seedManagedGuild(t, f.ctx, f.db, newGuild, 2, 1, 50, newLeader, nil)
	econSetContribution(t, f, oldGuild, buyer, balance, balance)
	for i := uint64(0); i < orphans; i++ {
		_, err := f.econ.ReserveShopOrder(f.ctx, econShopOrder(t, oldBase+i, buyer, oldGuild, 1, cost, limit, testNowMs+i))
		require.NoError(t, err, "在 A 下第 %d 笔旧订单", i)
	}
	_, err := f.repo.LeaveGuild(f.ctx, oldGuild, buyer, testNowMs+orphans)
	require.NoError(t, err, "离开 A(商店指令不被提前截止,仍 PENDING)")
	seedMemberRow(t, f.ctx, f.db, newGuild, buyer, constants.RoleMember)
	econSetContribution(t, f, newGuild, buyer, balance, balance)

	// 入参全部在主 goroutine 里造:econShopOrder 内部有 require,不能在子 goroutine 里调。
	newOrders := make([]ShopReserve, orphans)
	for i := range newOrders {
		newOrders[i] = econShopOrder(t, newBase+uint64(i), buyer, newGuild, 1, cost, limit, testNowMs+1000+uint64(i)*10)
	}
	ctx, cancel := context.WithTimeout(context.Background(), econLockOrderBudget)
	defer cancel()
	f.ctx = ctx
	deadlocks := watchGuildTxDeadlocks(t)
	watch := econWatchDeadlocks(t, f)
	gotOrphans := econCaptureOrphans(t)
	rejected := econRejectedResult()

	for i := uint64(0); i < orphans; i++ {
		order := newOrders[i]
		now := order.NowMs
		var finalized bool // 只由第二个闭包写,econRunTogether 返回之后才读
		errs := econRunTogether(
			func() error { _, err := f.econ.ReserveShopOrder(ctx, order); return err },
			func() error {
				var err error
				finalized, err = f.store.Finalize(ctx, assetop.Op{OpID: oldBase + i, PlayerID: buyer},
					assetop.StatusRejected, rejected, now)
				return err
			})
		require.NoError(t, errs[0], "第 %d 轮:在 B 兑换", i)
		require.NoError(t, errs[1], "第 %d 轮:终结 A 的旧订单", i)
		require.True(t, finalized, "第 %d 轮:旧订单只有这一个终结者", i)

		// 收尾:新订单 APPLIED(商店 APPLIED 不退限购、不退帮贡),免得未决行累积撞上 MaxPending。
		finalized, err := f.store.Finalize(ctx, assetop.Op{OpID: order.OpID, PlayerID: buyer},
			assetop.StatusApplied, econAppliedResult(), now+1)
		require.NoError(t, err, "第 %d 轮收尾", i)
		require.True(t, finalized, "第 %d 轮收尾", i)
	}

	var rejectedRows int
	require.NoError(t, f.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM guild_asset_op WHERE op_id BETWEEN ? AND ? AND status=?",
		oldBase, oldBase+orphans-1, int32(pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_REJECTED)).Scan(&rejectedRows))
	assert.Equal(t, orphans, rejectedRows, "A 的旧订单全部 REJECTED")
	wantOrphans := make([]string, orphans)
	for i := range wantOrphans {
		wantOrphans[i] = orphanKindShop + "/" + orphanWhatRefundMemberGone
	}
	assert.Equal(t, wantOrphans, gotOrphans(), "兑换者已离开 A:帮贡退不回去,每笔计一次孤儿")
	used, _ := econCounterUsed(t, f, buyer, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_SHOP, 101, econDayKey)
	assert.Equal(t, uint32(orphans), used, "旧订单的限购全额退回,只剩新订单的占用")
	_, newBalance, found := econContribution(t, f, newGuild, buyer)
	require.True(t, found)
	assert.Equal(t, balance-cost*orphans, newBalance, "B 里只扣新订单的帮贡")
	deadlocks.assertNone(t, "兑换孤儿退款 ‖ 别帮兑换同一商品")
	watch.assertNone(t, ctx, "兑换孤儿退款 ‖ 别帮兑换同一商品")
}
