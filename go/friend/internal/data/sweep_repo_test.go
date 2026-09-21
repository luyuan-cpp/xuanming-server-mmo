package data

// sweep_repo_test.go —— 终态申请清理(规格 §3.8)与零好友容量行回收的回归。
// 上半个文件是 SweepTerminalRequests,下半个(⑤–⑦,文件后段另有说明)是 SweepIdleCapacityRows。
//
// ⚠ 文件名按规格 §5 第 19 项给的是 `sweep_repo_test.go`(没有 `_mysql` 后缀),但内容必须
// 打真 MySQL:要验的正是 `DELETE ... WHERE status IN (2, 3) AND updated_ms < ? LIMIT ?` 这条 SQL
// 的过滤与限幅。所以门控仍是 FRIEND_TEST_MYSQL_DSN(与本包其它集成用例同一个开关)。
//
// # 为什么这个文件的断言全部围着 updated_ms 转
//
// sweep 是本批唯一一个**会删玩家数据**的路径,而它的判据 `updated_ms` 是 F2 新增列。
// 只要写入方漏了任何一处(§3.1 的 upsert、§3.2 的两条 UPDATE、§3.3 的 CAS、§3.4 ⑥ 的取消),
// 那些行就恒为 0 → 恒小于任何 cutoff → **delete 模式会把它们全删掉**。
// 这不是"少清理一点"的温和故障,是一次定时任务清空历史。所以:
//
//	① status=1(pending)永不匹配 —— 玩家正在等的申请一条都不许被碰;
//	② report_only(默认)一行都不许删;
//	③ delete 必须真的按 updated_ms 过滤,而不是"看起来过滤了";
//	④ 规格 §3.8 那道"待删行数异常就不删"的保险必须真的挡住。
//	   实现的判据是"**存在 updated_ms 恒为 0 的终态行**"(见 sweep_repo.go 的
//	   countTerminalRequestsMissingUpdatedMs):那样的行只可能来自写入方漏写,
//	   而 0 恒小于任何 cutoff,一轮就会把它们全删掉。
//
// # 不许用墙钟
//
// 保留期的时间基准一律**显式注入**(nowMs 参数)+ 显式写入 updated_ms 的绝对值。
// 用 time.Sleep 等保留期过去是不可能的(7 天),用"睡一会儿再看"更是把不确定性写进断言 ——
// AGENTS.md §11.4:单元测试不得依赖真实墙钟。
//
// 公共夹具(门控、schema、mustCount、不变量断言)在 friend_repo_mysql_test.go。

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sweep 的两个模式在 data 层只能是这两个字面量(见 friend_repo_mysql_test.go 顶部说明:
// 本包测试不能 import config,所以这里写字面量,同时也就钉住了 data 层必须认这个拼法)。
const (
	testSweepModeReportOnly = "report_only"
	testSweepModeDelete     = "delete"
)

const testDayMs = int64(24 * time.Hour / time.Millisecond)

// 申请状态(与 proto/friend/friend_table.proto 的注释一致:1=pending 2=accepted 3=rejected)。
const (
	testStatusPending  int64 = 1
	testStatusAccepted int64 = 2
	testStatusRejected int64 = 3
)

// seedRequestRow 直写一条指定状态与 updated_ms 的申请行。
// 直写而不是走业务路径:业务路径写的 updated_ms 永远是"现在",造不出"7 天前"。
func seedRequestRow(t *testing.T, ctx context.Context, db *sql.DB, from, to uint64, status int64, updatedMs int64) {
	t.Helper()
	_, err := db.ExecContext(ctx,
		`INSERT INTO friend_request (from_player_id, to_player_id, request_time_ms, status, updated_ms)
		 VALUES (?, ?, ?, ?, ?)`, from, to, updatedMs, status, updatedMs)
	require.NoError(t, err)
}

// sweepFixture 造一套固定的四行,覆盖"该删 / 不该删"的四个象限。
//
//	expiredAccepted  终态 + 超期  → delete 模式该删
//	expiredRejected  终态 + 超期  → delete 模式该删
//	freshAccepted    终态 + 未超期 → 任何模式都不该删(玩家刚刚处理完,界面上还可能在看)
//	oldPending       pending + 极老 → **任何模式都不该删**(①)
func sweepFixture(t *testing.T, ctx context.Context, db *sql.DB, nowMs int64, retentionDays int) {
	t.Helper()
	cutoff := nowMs - int64(retentionDays)*testDayMs
	seedRequestRow(t, ctx, db, 41001, 41002, testStatusAccepted, cutoff-testDayMs)    // 超期 1 天
	seedRequestRow(t, ctx, db, 41003, 41004, testStatusRejected, cutoff-30*testDayMs) // 超期 30 天
	seedRequestRow(t, ctx, db, 41005, 41006, testStatusAccepted, nowMs-time.Hour.Milliseconds())
	seedRequestRow(t, ctx, db, 41007, 41008, testStatusPending, nowMs-365*testDayMs) // 极老的 pending
}

func totalRequestRows(t *testing.T, ctx context.Context, db *sql.DB) int64 {
	t.Helper()
	return mustCount(t, ctx, db, "SELECT COUNT(*) FROM friend_request")
}

// TestSweep_ReportOnlyCountsButNeverDeletes 钉 ②。
//
// report_only 是**默认模式**(config 的 default 标签),也是 F1 §6.3 登记的那条前置:
// updated_ms 的写入方落地前不得配 delete。所以"默认模式绝不删行"必须有测试 ——
// 它是误配 / 误改的最后一道拦网。
func TestSweep_ReportOnlyCountsButNeverDeletes(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	nowMs := time.Now().UnixMilli()
	const retentionDays = 7
	sweepFixture(t, ctx, db, nowMs, retentionDays)
	require.Equal(t, int64(4), totalRequestRows(t, ctx, db))

	pending, deleted, err := callSweep(ctx, repo, testSweepModeReportOnly, retentionDays, 1000, nowMs)
	require.NoError(t, err)

	assert.Zero(t, deleted, "report_only 一行都不许删")
	assert.Equal(t, int64(2), pending,
		"待清理行数必须只数终态且超期的那两条(数多了说明 status 或 updated_ms 的过滤有一处没写)")
	assert.Equal(t, int64(4), totalRequestRows(t, ctx, db), "report_only 之后表里仍是 4 行")

	// 再跑一次:report_only 必须是纯读,重复执行不改变任何东西。
	pendingAgain, deletedAgain, err := callSweep(ctx, repo, testSweepModeReportOnly, retentionDays, 1000, nowMs)
	require.NoError(t, err)
	assert.Zero(t, deletedAgain)
	assert.Equal(t, pending, pendingAgain)
	assert.Equal(t, int64(4), totalRequestRows(t, ctx, db))
}

// TestSweep_DeleteFiltersByUpdatedMsAndSkipsPending 钉 ① 与 ③。
func TestSweep_DeleteFiltersByUpdatedMsAndSkipsPending(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	nowMs := time.Now().UnixMilli()
	const retentionDays = 7
	sweepFixture(t, ctx, db, nowMs, retentionDays)
	// 多加两行"未超期的终态":它们是"不该删"的一侧,顺带让"删了多少"这个数字
	// 在总行数里显出来。夹具里没有 updated_ms==0 的行,所以 §3.8 的保险不会触发
	// (保险单独在下面那条用例里验)。
	seedRequestRow(t, ctx, db, 41009, 41010, testStatusRejected, nowMs-time.Hour.Milliseconds())
	seedRequestRow(t, ctx, db, 41011, 41012, testStatusAccepted, nowMs)
	require.Equal(t, int64(6), totalRequestRows(t, ctx, db))

	_, deleted, err := callSweep(ctx, repo, testSweepModeDelete, retentionDays, 1000, nowMs)
	require.NoError(t, err)
	assert.Equal(t, int64(2), deleted, "只该删终态且超期的那两条")

	// 逐行点名,不只看总数:总数对而删错行是最难发现的一类错。
	assert.Zero(t, mustCount(t, ctx, db,
		"SELECT COUNT(*) FROM friend_request WHERE from_player_id=?", uint64(41001)), "超期 accepted 应被删")
	assert.Zero(t, mustCount(t, ctx, db,
		"SELECT COUNT(*) FROM friend_request WHERE from_player_id=?", uint64(41003)), "超期 rejected 应被删")
	assert.Equal(t, int64(1), mustCount(t, ctx, db,
		"SELECT COUNT(*) FROM friend_request WHERE from_player_id=?", uint64(41005)), "未超期的终态不许删")
	assert.Equal(t, int64(1), mustCount(t, ctx, db,
		"SELECT COUNT(*) FROM friend_request WHERE from_player_id=?", uint64(41007)),
		"pending 无论多老都不许删:玩家还在等这条申请的结果(①)")

	// 幂等:再跑一次没有新的候选,删 0 行且不报错(多副本各跑各的 ticker,必然会重复执行)。
	_, deletedAgain, err := callSweep(ctx, repo, testSweepModeDelete, retentionDays, 1000, nowMs)
	require.NoError(t, err)
	assert.Zero(t, deletedAgain, "无候选时删 0 行,不报错(DELETE 幂等,多副本重复执行安全)")

	// sweep 只碰 friend_request,不许连带动好友边 / 黑名单 / 容量行。
	assertFriendInvariants(t, ctx, db)
}

// TestSweep_DeleteRespectsBatchLimit:BatchLimit 是保护线上 MySQL 的,不是建议值。
// 一次删光会长时间持有行锁并把 binlog 撑爆,写路径在那段时间全部超时。
func TestSweep_DeleteRespectsBatchLimit(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	nowMs := time.Now().UnixMilli()
	const retentionDays = 7
	cutoff := nowMs - int64(retentionDays)*testDayMs
	// 5 条超期终态 + 5 条未超期终态。全部 updated_ms 非零,所以保险不会触发。
	for i := 0; i < 5; i++ {
		seedRequestRow(t, ctx, db, uint64(42100+i), uint64(42200+i), testStatusAccepted, cutoff-testDayMs)
		seedRequestRow(t, ctx, db, uint64(42300+i), uint64(42400+i), testStatusAccepted, nowMs)
	}

	_, deleted, err := callSweep(ctx, repo, testSweepModeDelete, retentionDays, 2, nowMs)
	require.NoError(t, err)
	assert.Equal(t, int64(2), deleted, "单轮删除必须受 BatchLimit 限制(SQL 里要有 LIMIT)")
	assert.Equal(t, int64(8), totalRequestRows(t, ctx, db))

	// 剩下的在后续轮次里慢慢清掉:三轮之后 5 条候选清完,未超期的 5 条一条不动。
	for i := 0; i < 3; i++ {
		if _, _, err := callSweep(ctx, repo, testSweepModeDelete, retentionDays, 2, nowMs); err != nil {
			t.Fatalf("第 %d 轮 sweep 失败: %v", i+2, err)
		}
	}
	assert.Equal(t, int64(5), totalRequestRows(t, ctx, db), "多轮之后恰好剩下未超期的 5 行")
}

// TestSweep_DeleteRefusesWhenUpdatedMsWasNeverWritten 钉 ④ —— 规格 §3.8 那道保险。
//
// 它防的是唯一一种会造成灾难的误配:updated_ms 的写入方漏了 → 那些行恒为 0 →
// 0 恒小于任何 cutoff → delete 模式一轮清空整张表。判据是"存在 updated_ms 为 0 的终态行",
// 而不是时间上的判断 —— 因为"很久以前"和"从来没写过"在 SQL 里长得一模一样,
// 只有 0 这个**不可能是真实时间戳**的值能把两者分开。
//
// 契约(sweep_repo.go 写明):保险触发时返回 deleted=0 且 **err=nil** ——
// 它不是故障,是刻意的拒绝,错误日志在 data 层打。所以这里断言 err==nil,
// 断言成"必须报错"会把一次正确的保护判成失败。
func TestSweep_DeleteRefusesWhenUpdatedMsWasNeverWritten(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	nowMs := time.Now().UnixMilli()
	const retentionDays = 7
	// 模拟"updated_ms 从来没被写过":终态行的 updated_ms 恒为 0。
	// 再放两行**正常**的超期终态行:保险必须是"一行都不删",而不是"只跳过那几行 0"——
	// 混着 0 行的表说明写入方漏写,这一轮的任何删除都不可信。
	for i := 0; i < 3; i++ {
		seedRequestRow(t, ctx, db, uint64(43100+i), uint64(43200+i), testStatusAccepted, 0)
	}
	cutoff := nowMs - int64(retentionDays)*testDayMs
	seedRequestRow(t, ctx, db, 43300, 43400, testStatusRejected, cutoff-testDayMs)
	seedRequestRow(t, ctx, db, 43301, 43401, testStatusRejected, cutoff-2*testDayMs)
	require.Equal(t, int64(5), totalRequestRows(t, ctx, db))

	_, deleted, err := callSweep(ctx, repo, testSweepModeDelete, retentionDays, 1000, nowMs)
	require.NoError(t, err, "保险触发不是故障,是刻意的拒绝:契约要求 err=nil(错误日志在 data 层打)")
	assert.Zero(t, deleted, "存在 updated_ms 为 0 的终态行时必须一行都不删(§3.8 的保险)")
	assert.Equal(t, int64(5), totalRequestRows(t, ctx, db),
		"混着 updated_ms=0 的表说明写入方漏写,这一轮的任何删除都不可信 —— 全部保留")
}

// TestSweep_RefusesNonPositiveCutoff 钉 sweep_repo.go 里 `cutoffMs <= 0` 那道护栏。
//
// nowMs 远小于保留期时截止点为负;负数传给 bigint unsigned 的 updated_ms 会被按无符号
// 解释成天文数字 = 匹配全表,delete 模式下就是清空整张 friend_request。
// nowMs 是注入的,所以这条用例完全确定、不依赖墙钟。
func TestSweep_RefusesNonPositiveCutoff(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	const retentionDays = 7
	// 预置的行 updated_ms 都很小:如果护栏失守、cutoff 被按无符号解释成天文数字,
	// 这些行会全部匹配上并被删掉 —— 那正是要挡的灾难形态。
	seedRequestRow(t, ctx, db, 44001, 44002, testStatusAccepted, 1)
	seedRequestRow(t, ctx, db, 44003, 44004, testStatusRejected, 2)
	seedRequestRow(t, ctx, db, 44005, 44006, testStatusPending, 3)
	before := totalRequestRows(t, ctx, db)
	require.Equal(t, int64(3), before)

	// nowMs 只有 1000ms(进程时钟没设对的典型形态),远小于 7 天。
	pending, deleted, err := callSweep(ctx, repo, testSweepModeDelete, retentionDays, 1000, 1000)
	require.NoError(t, err, "时钟没设对不是故障,本轮什么也不做即可")
	assert.Zero(t, pending)
	assert.Zero(t, deleted)
	assert.Equal(t, before, totalRequestRows(t, ctx, db),
		"截止点非正时一行都不许删:负数截止点会被 MySQL 按无符号解释成匹配全表")
}

// TestSweep_RejectsNonPositiveBatchLimit 钉 sweep_repo.go 里 `batchLimit <= 0` 那道护栏:
// 不带 LIMIT 的 DELETE 会用一条长事务锁住整张 friend_request,把好友申请写路径一起卡住。
// 所以 0 必须 fail-fast 报错,不许被当成"用默认值"。
func TestSweep_RejectsNonPositiveBatchLimit(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	nowMs := time.Now().UnixMilli()
	const retentionDays = 7
	sweepFixture(t, ctx, db, nowMs, retentionDays)
	before := totalRequestRows(t, ctx, db)

	for _, batchLimit := range []int{0, -1} {
		_, deleted, err := callSweep(ctx, repo, testSweepModeDelete, retentionDays, batchLimit, nowMs)
		require.Error(t, err, "batchLimit=%d 必须 fail-fast,不许当默认值", batchLimit)
		assert.Zero(t, deleted)
	}
	assert.Equal(t, before, totalRequestRows(t, ctx, db), "护栏触发时一行都不许被删")
}

// TestSweep_UnknownModeDeletesNothing:模式是字符串,拼错是最容易犯的错。
// 契约是"未知模式按 report_only 处理"—— 即**一行都不删**且不报错。
// 反过来的实现("不认识就当 delete")会让一个 yaml 拼写错误变成一次数据清空。
func TestSweep_UnknownModeDeletesNothing(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	nowMs := time.Now().UnixMilli()
	const retentionDays = 7
	sweepFixture(t, ctx, db, nowMs, retentionDays)

	for _, mode := range []string{"", "Delete", "DELETE", "report-only", "purge"} {
		_, deleted, err := callSweep(ctx, repo, mode, retentionDays, 1000, nowMs)
		require.NoError(t, err, "未知模式按 report_only 处理,不是故障(模式 %q)", mode)
		assert.Zero(t, deleted, "未知模式 %q 不得删行:拼错模式必须退到只统计", mode)
	}
	assert.Equal(t, int64(4), totalRequestRows(t, ctx, db))
}

// ── 第二类清理对象:零好友的 friend_capacity 行(SweepIdleCapacityRows)──────────
//
// 与上半个文件同一套纪律(nowMs 显式注入、created_ms 直写绝对值、逐行点名而不只看总数),
// 但判据换了,危险的方向也跟着换:
//
//	⑤ 只删 `friend_count = 0` 的行。friend_capacity 是好友数的**权威计数行**(D-10),删掉一行有好友的
//	   计数 —— 虽然 ensure 能按边数重算回来 —— 等于让"只删零好友行"这条前提靠运气成立;
//	⑥ 只删 `created_ms < 截止点` 的行。刚建出来的守卫行被立刻回收,写路径就会反复撞上守卫缺行;
//	⑦ **created_ms = 0 的零好友行会被回收** —— 与 updated_ms 的保险(④)方向正好相反。
//	   updated_ms=0 意味着"写入方漏写",拿它判过期会清空整表,所以拒删;
//	   created_ms=0 只意味着"从旧库搬来的存量行",而零好友的存量行删掉没有任何语义损失
//	   (ensure 补行时按 friend 表的权威边数重算,见 TestMissingCapacityRowUsesAuthoritativeFriendCount)。
//	   谁照着 ④ 给回收也"补"一道 created_ms=0 拒删,存量空行就永远回收不掉。

// callSweepIdleCapacity —— 与 friend_repo_mysql_test.go 的 callSweep 同一个用途:
// 对生产签名的假设只出现在这一处。
func callSweepIdleCapacity(ctx context.Context, repo *FriendRepo, mode string, retentionDays, batchLimit int, nowMs int64) (idle int64, deleted int64, err error) {
	return repo.SweepIdleCapacityRows(ctx, mode, retentionDays, batchLimit, nowMs)
}

// seedIdleCapacityRow 直写一行零好友的容量行,created_ms 给绝对值。
// 直写而不是走 ensure:ensure 写的 created_ms 永远是"现在",造不出"7 天前",也造不出存量行的 0。
func seedIdleCapacityRow(t *testing.T, ctx context.Context, db *sql.DB, playerID uint64, createdMs int64) {
	t.Helper()
	_, err := db.ExecContext(ctx,
		"INSERT INTO friend_capacity (player_id, friend_count, created_ms) VALUES (?, 0, ?)", playerID, createdMs)
	require.NoError(t, err)
}

// seedBefriendedCapacityRow 造一行**有好友**的容量行(边是真的,计数与边数一致),再把 created_ms 改成给定值。
// 不直接写一个 friend_count=2 的光杆行:那样 assertFriendInvariants 会先因为"计数与边数不一致"变红,
// 把本用例真正要看的"这行有没有被误删"盖住。
func seedBefriendedCapacityRow(t *testing.T, ctx context.Context, db *sql.DB, playerID uint64, createdMs int64, friendIDs ...uint64) {
	t.Helper()
	seedFriendEdges(t, ctx, db, playerID, friendIDs...)
	_, err := db.ExecContext(ctx,
		"UPDATE friend_capacity SET created_ms=? WHERE player_id=?", createdMs, playerID)
	require.NoError(t, err)
}

func capacityRowCount(t *testing.T, ctx context.Context, db *sql.DB, playerID uint64) int64 {
	t.Helper()
	return mustCount(t, ctx, db, "SELECT COUNT(*) FROM friend_capacity WHERE player_id=?", playerID)
}

func totalCapacityRows(t *testing.T, ctx context.Context, db *sql.DB) int64 {
	t.Helper()
	return mustCount(t, ctx, db, "SELECT COUNT(*) FROM friend_capacity")
}

// 容量行夹具的玩家 id(逐行点名用)。
const (
	capExpiredIdleA      uint64 = 45001 // 零好友 + 超期 1 天   → delete 该删
	capExpiredIdleB      uint64 = 45002 // 零好友 + 超期 30 天  → delete 该删
	capFreshIdle         uint64 = 45003 // 零好友 + 未超期      → 不该删(⑥)
	capBoundaryIdle      uint64 = 45004 // 零好友 + created_ms 恰好等于截止点 → 不该删(判据是 <,不是 <=)
	capExpiredBefriended uint64 = 45005 // 有好友 + 超期 30 天  → **任何模式都不该删**(⑤)
)

// capacitySweepFixture 造五行,覆盖"该删 / 不该删"的各个象限;可回收的恰好是两行。
func capacitySweepFixture(t *testing.T, ctx context.Context, db *sql.DB, nowMs int64, retentionDays int) {
	t.Helper()
	cutoff := nowMs - int64(retentionDays)*testDayMs
	seedIdleCapacityRow(t, ctx, db, capExpiredIdleA, cutoff-testDayMs)
	seedIdleCapacityRow(t, ctx, db, capExpiredIdleB, cutoff-30*testDayMs)
	seedIdleCapacityRow(t, ctx, db, capFreshIdle, nowMs-time.Hour.Milliseconds())
	seedIdleCapacityRow(t, ctx, db, capBoundaryIdle, cutoff)
	seedBefriendedCapacityRow(t, ctx, db, capExpiredBefriended, cutoff-30*testDayMs, 45101, 45102)
}

// TestSweepIdleCapacity_ReportOnlyCountsButNeverDeletes:默认模式只数不删(与 ② 同一道拦网)。
func TestSweepIdleCapacity_ReportOnlyCountsButNeverDeletes(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	nowMs := time.Now().UnixMilli()
	const retentionDays = 7
	capacitySweepFixture(t, ctx, db, nowMs, retentionDays)
	require.Equal(t, int64(5), totalCapacityRows(t, ctx, db))

	idle, deleted, err := callSweepIdleCapacity(ctx, repo, testSweepModeReportOnly, retentionDays, 1000, nowMs)
	require.NoError(t, err)
	assert.Zero(t, deleted, "report_only 一行都不许删")
	assert.Equal(t, int64(2), idle,
		"可回收行数必须只数零好友且超期的那两行(数多了说明 friend_count 或 created_ms 的过滤有一处没写)")
	assert.Equal(t, int64(5), totalCapacityRows(t, ctx, db), "report_only 之后表里仍是 5 行")

	// 再跑一次:report_only 必须是纯读。
	idleAgain, deletedAgain, err := callSweepIdleCapacity(ctx, repo, testSweepModeReportOnly, retentionDays, 1000, nowMs)
	require.NoError(t, err)
	assert.Zero(t, deletedAgain)
	assert.Equal(t, idle, idleAgain)
	assert.Equal(t, int64(5), totalCapacityRows(t, ctx, db))
}

// TestSweepIdleCapacity_DeleteOnlyRemovesIdleExpiredRows 钉 ⑤ 与 ⑥。
func TestSweepIdleCapacity_DeleteOnlyRemovesIdleExpiredRows(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	nowMs := time.Now().UnixMilli()
	const retentionDays = 7
	capacitySweepFixture(t, ctx, db, nowMs, retentionDays)
	// 旁边放一条**超期的终态申请行**:两个 Sweep 方法共用截止点与模式,最容易出的错是复制粘贴时
	// 表名没换干净。容量行回收一行 friend_request 都不许碰。
	seedRequestRow(t, ctx, db, 45201, 45202, testStatusRejected, nowMs-365*testDayMs)

	idle, deleted, err := callSweepIdleCapacity(ctx, repo, testSweepModeDelete, retentionDays, 1000, nowMs)
	require.NoError(t, err)
	assert.Equal(t, int64(2), idle)
	assert.Equal(t, int64(2), deleted, "只该删零好友且超期的那两行")

	// 逐行点名,不只看总数:总数对而删错行是最难发现的一类错。
	assert.Zero(t, capacityRowCount(t, ctx, db, capExpiredIdleA), "零好友 + 超期 应被回收")
	assert.Zero(t, capacityRowCount(t, ctx, db, capExpiredIdleB), "零好友 + 超期 应被回收")
	assert.Equal(t, int64(1), capacityRowCount(t, ctx, db, capFreshIdle),
		"未超期的零好友行不许删:刚建出来的守卫行被立刻回收,写路径会反复撞上守卫缺行(⑥)")
	assert.Equal(t, int64(1), capacityRowCount(t, ctx, db, capBoundaryIdle),
		"created_ms 恰好等于截止点的行不许删:判据是 created_ms < 截止点,与 updated_ms 那一半同一个口径")
	assert.Equal(t, int64(1), capacityRowCount(t, ctx, db, capExpiredBefriended),
		"有好友的行无论多老都不许删:它是该玩家好友数的权威计数(⑤)")
	assert.Equal(t, int64(2), mustCount(t, ctx, db,
		"SELECT friend_count FROM friend_capacity WHERE player_id=?", capExpiredBefriended),
		"没被删还不够,计数也不许被动过")
	assert.Equal(t, int64(1), totalRequestRows(t, ctx, db), "容量行回收不得碰 friend_request")

	// 幂等:再跑一次没有新的候选,删 0 行且不报错(多副本各跑各的 ticker,必然会重复执行)。
	idleAgain, deletedAgain, err := callSweepIdleCapacity(ctx, repo, testSweepModeDelete, retentionDays, 1000, nowMs)
	require.NoError(t, err)
	assert.Zero(t, idleAgain)
	assert.Zero(t, deletedAgain)

	assertFriendInvariants(t, ctx, db)
}

// TestSweepIdleCapacity_DeleteRespectsBatchLimit:两个返回值都受 BatchLimit 封顶。
//
// 回收是逐行删的,所以这里的上限不只保护 MySQL,还直接决定单轮的语句条数 ——
// 候选查询漏了 LIMIT 时,一次误配就是一轮几十万条 DELETE。
func TestSweepIdleCapacity_DeleteRespectsBatchLimit(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	nowMs := time.Now().UnixMilli()
	const retentionDays = 7
	cutoff := nowMs - int64(retentionDays)*testDayMs
	// 5 行可回收 + 5 行未超期。
	for i := 0; i < 5; i++ {
		seedIdleCapacityRow(t, ctx, db, uint64(45300+i), cutoff-testDayMs)
		seedIdleCapacityRow(t, ctx, db, uint64(45400+i), nowMs)
	}

	idle, deleted, err := callSweepIdleCapacity(ctx, repo, testSweepModeDelete, retentionDays, 2, nowMs)
	require.NoError(t, err)
	assert.Equal(t, int64(2), idle, "看到的可回收行数在 BatchLimit 处饱和:等于上限只说明积压 ≥ 一批")
	assert.Equal(t, int64(2), deleted, "单轮删除必须受 BatchLimit 限制(候选查询里要有 LIMIT)")
	assert.Equal(t, int64(8), totalCapacityRows(t, ctx, db))

	// report_only 同样封顶:它与 delete 共用候选查询,但单独钉一下,免得有人给它换成无界的 COUNT(*)。
	idle, _, err = callSweepIdleCapacity(ctx, repo, testSweepModeReportOnly, retentionDays, 2, nowMs)
	require.NoError(t, err)
	assert.Equal(t, int64(2), idle, "report_only 的计数同样在 BatchLimit 处饱和(剩 3 行可回收,只报 2)")

	// 剩下的在后续轮次里清掉:两轮之后 5 行候选清完,未超期的 5 行一行不动。
	for i := 0; i < 2; i++ {
		if _, _, err := callSweepIdleCapacity(ctx, repo, testSweepModeDelete, retentionDays, 2, nowMs); err != nil {
			t.Fatalf("第 %d 轮容量行回收失败: %v", i+2, err)
		}
	}
	assert.Equal(t, int64(5), totalCapacityRows(t, ctx, db), "多轮之后恰好剩下未超期的 5 行")
	assert.Equal(t, int64(5), mustCount(t, ctx, db,
		"SELECT COUNT(*) FROM friend_capacity WHERE player_id BETWEEN 45400 AND 45404"), "剩下的必须正是未超期的那 5 行")
}

// TestSweepIdleCapacity_RefusesNonPositiveCutoff:与 TestSweep_RefusesNonPositiveCutoff 同一道护栏
// (两个方法共用 sweepCutoffMs),但必须各验一遍 —— 护栏共用是实现细节,谁把回收改回自己算截止点,
// 只有这条会红。created_ms 同样是 bigint unsigned,负截止点同样会被解释成"匹配全表"。
func TestSweepIdleCapacity_RefusesNonPositiveCutoff(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	const retentionDays = 7
	// created_ms 都很小(含存量行的 0):护栏失守时它们会全部匹配上并被删掉。
	seedIdleCapacityRow(t, ctx, db, 45501, 0)
	seedIdleCapacityRow(t, ctx, db, 45502, 1)
	seedIdleCapacityRow(t, ctx, db, 45503, 2)

	// nowMs 只有 1000ms(进程时钟没设对的典型形态),远小于 7 天。
	idle, deleted, err := callSweepIdleCapacity(ctx, repo, testSweepModeDelete, retentionDays, 1000, 1000)
	require.NoError(t, err, "时钟没设对不是故障,本轮什么也不做即可")
	assert.Zero(t, idle)
	assert.Zero(t, deleted)
	assert.Equal(t, int64(3), totalCapacityRows(t, ctx, db), "截止点非正时一行都不许删")
}

// TestSweepIdleCapacity_RejectsInvalidParameters:batchLimit <= 0 与越界的 retentionDays 必须 fail-fast。
// batchLimit=0 在这里的后果比终态申请那一半更直接:`LIMIT 0` 会让回收静默地永远什么都不做,
// 而"当成不限"则是一轮删光全部候选;两种都不许,只能报错。
func TestSweepIdleCapacity_RejectsInvalidParameters(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	nowMs := time.Now().UnixMilli()
	const retentionDays = 7
	capacitySweepFixture(t, ctx, db, nowMs, retentionDays)
	before := totalCapacityRows(t, ctx, db)

	for _, batchLimit := range []int{0, -1} {
		_, deleted, err := callSweepIdleCapacity(ctx, repo, testSweepModeDelete, retentionDays, batchLimit, nowMs)
		require.Error(t, err, "batchLimit=%d 必须 fail-fast,不许当默认值", batchLimit)
		assert.Zero(t, deleted)
	}
	// retentionDays=0 会把刚建的守卫行立刻判成可回收;超过 maxRetentionDays 的值是溢出护栏
	// (乘上每天毫秒数会在 int64 上回绕成负数)。
	for _, days := range []int{0, -1, maxRetentionDays + 1} {
		_, deleted, err := callSweepIdleCapacity(ctx, repo, testSweepModeDelete, days, 1000, nowMs)
		require.Error(t, err, "retentionDays=%d 必须 fail-fast", days)
		assert.Zero(t, deleted)
	}
	assert.Equal(t, before, totalCapacityRows(t, ctx, db), "护栏触发时一行都不许被删")
}

// TestSweepIdleCapacity_UnknownModeDeletesNothing:未知模式只数不删(理由同 TestSweep_UnknownModeDeletesNothing)。
func TestSweepIdleCapacity_UnknownModeDeletesNothing(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	nowMs := time.Now().UnixMilli()
	const retentionDays = 7
	capacitySweepFixture(t, ctx, db, nowMs, retentionDays)

	for _, mode := range []string{"", "Delete", "DELETE", "report-only", "purge"} {
		idle, deleted, err := callSweepIdleCapacity(ctx, repo, mode, retentionDays, 1000, nowMs)
		require.NoError(t, err, "未知模式按 report_only 处理,不是故障(模式 %q)", mode)
		assert.Zero(t, deleted, "未知模式 %q 不得删行:拼错模式必须退到只统计", mode)
		assert.Equal(t, int64(2), idle, "未知模式 %q 仍要如实统计:否则拼错模式会同时把积压指标抹成 0", mode)
	}
	assert.Equal(t, int64(5), totalCapacityRows(t, ctx, db))
}

// TestSweepIdleCapacity_ReclaimsLegacyRowsWithZeroCreatedMs 钉 ⑦ —— 与 ④ 方向**相反**的那条。
//
// 见本节开头的说明:created_ms=0 是"从旧库搬来的存量行"的形态,零好友的存量行应当被回收。
// 这条用例存在的意义是拦住一次好心的"对称化":照着 SweepTerminalRequests 给回收也加一道
// "发现 0 就拒删",结果是存量空行永远删不掉,而且没有任何报错。
func TestSweepIdleCapacity_ReclaimsLegacyRowsWithZeroCreatedMs(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	nowMs := time.Now().UnixMilli()
	const retentionDays = 7
	const legacyIdle, legacyBefriended, freshIdle uint64 = 45601, 45602, 45603
	seedIdleCapacityRow(t, ctx, db, legacyIdle, 0)
	// 对照一:同样是 created_ms=0,但有好友 → 不许删(⑤ 不因为"存量行"而放宽)。
	seedBefriendedCapacityRow(t, ctx, db, legacyBefriended, 0, 45701)
	// 对照二:混着 0 行的表里,正常的未超期行照样不删、正常流程照样进行 ——
	// 回收没有 ④ 那种"发现 0 就整轮拒删"的行为。
	seedIdleCapacityRow(t, ctx, db, freshIdle, nowMs)

	idle, deleted, err := callSweepIdleCapacity(ctx, repo, testSweepModeDelete, retentionDays, 1000, nowMs)
	require.NoError(t, err)
	assert.Equal(t, int64(1), idle)
	assert.Equal(t, int64(1), deleted, "created_ms=0 的零好友存量行必须被回收(与 updated_ms=0 的拒删相反)")
	assert.Zero(t, capacityRowCount(t, ctx, db, legacyIdle))
	assert.Equal(t, int64(1), capacityRowCount(t, ctx, db, legacyBefriended), "有好友的存量行不许删")
	assert.Equal(t, int64(1), capacityRowCount(t, ctx, db, freshIdle))

	// 删掉之后写路径照常能把它建回来,且新行带着真实的 created_ms —— "删行无害"的另一半。
	require.NoError(t, repo.ensureFriendCapacityRows(ctx, legacyIdle))
	assert.NotZero(t, readCapacityCreatedMs(t, ctx, db, legacyIdle))
	assertFriendInvariants(t, ctx, db)
}

// TestDeleteIdleCapacityRow_RechecksAtCommitPoint 直调 deleteIdleCapacityRow,钉它 WHERE 里的提交点复核。
//
// 为什么上面那几条 TestSweepIdleCapacity_* 钉不住它:它们全部经由 SweepIdleCapacityRows 进来,候选名单来自
// listIdleCapacityRowsBefore(`friend_count = 0 AND created_ms < ?`),有好友的行 / 未超期的行在名单里就被
// 滤掉了 —— 单线程下交给 DELETE 的永远是本来就满足条件的行。把 DELETE 改成 `WHERE player_id = ?`,
// capExpiredBefriended / capFreshIdle / capBoundaryIdle / legacyBefriended 的断言一条都不会红。
// 复核要挡的是"候选读之后、DELETE 之前这行变了"(被 AcceptFriend 加了好友、或被删后由 ensure 重建);
// 那个时序在并发场景 (f) 里只能碰运气撞上,这里绕开名单直接把"已经变了的行"递给 DELETE,确定性地验。
func TestDeleteIdleCapacityRow_RechecksAtCommitPoint(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	nowMs := time.Now().UnixMilli()
	cutoff := uint64(nowMs - 7*testDayMs)
	const (
		befriendedOld uint64 = 45801 // 名单读出之后被加了好友的老行
		rebuiltFresh  uint64 = 45802 // 被别的回收者删掉、又由 ensure 重建出来的新行
		boundaryIdle  uint64 = 45803 // created_ms 恰好等于截止点
		expiredIdle   uint64 = 45804 // 正向对照:真的该删
		friendA       uint64 = 45811
		friendB       uint64 = 45812
	)

	// ① 有好友的老行不许删。
	seedBefriendedCapacityRow(t, ctx, db, befriendedOld, 1, friendA, friendB)
	removed, err := repo.deleteIdleCapacityRow(ctx, befriendedOld, cutoff)
	require.NoError(t, err)
	assert.Zero(t, removed, "DELETE 漏了 `friend_count = 0` 提交点复核:删掉了一行有好友的权威计数")
	assert.Equal(t, int64(1), capacityRowCount(t, ctx, db, befriendedOld),
		"DELETE 漏了 `friend_count = 0` 提交点复核:有好友的容量行不见了")
	assert.Equal(t, int64(2), mustCount(t, ctx, db,
		"SELECT friend_count FROM friend_capacity WHERE player_id=?", befriendedOld), "复核挡下的行必须原样不动")

	// ② 重建的新行不许删。
	seedIdleCapacityRow(t, ctx, db, rebuiltFresh, nowMs)
	removed, err = repo.deleteIdleCapacityRow(ctx, rebuiltFresh, cutoff)
	require.NoError(t, err)
	assert.Zero(t, removed, "DELETE 漏了 `created_ms < ?` 提交点复核:删掉了刚由 ensure 重建的守卫行")
	assert.Equal(t, int64(1), capacityRowCount(t, ctx, db, rebuiltFresh),
		"DELETE 漏了 `created_ms < ?` 提交点复核:刚重建的守卫行不见了(写路径会因此反复撞上守卫缺行)")

	// ②' 边界行不许删:DELETE 自己的判据也必须是 <,不是 <=。
	// (capBoundaryIdle 那条断言同样只由名单的 WHERE 守着,管不到 DELETE 这一侧。)
	seedIdleCapacityRow(t, ctx, db, boundaryIdle, int64(cutoff))
	removed, err = repo.deleteIdleCapacityRow(ctx, boundaryIdle, cutoff)
	require.NoError(t, err)
	assert.Zero(t, removed, "DELETE 的 created_ms 判据必须是 <(与候选名单一致),恰好等于截止点的行不删")
	assert.Equal(t, int64(1), capacityRowCount(t, ctx, db, boundaryIdle))

	// ③ 正向对照:没有这一支,上面三条在"deleteIdleCapacityRow 恒不删"的坏实现下也照绿。
	seedIdleCapacityRow(t, ctx, db, expiredIdle, int64(cutoff)-1)
	removed, err = repo.deleteIdleCapacityRow(ctx, expiredIdle, cutoff)
	require.NoError(t, err)
	assert.Equal(t, int64(1), removed, "零好友且 created_ms 早于截止点的行必须被删掉")
	assert.Zero(t, capacityRowCount(t, ctx, db, expiredIdle))

	assertFriendInvariants(t, ctx, db)
}
