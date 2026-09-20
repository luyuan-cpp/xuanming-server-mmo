package data

// sweep_repo_test.go —— 终态申请清理(规格 §3.8)的回归。
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
	seedRequestRow(t, ctx, db, 41001, 41002, testStatusAccepted, cutoff-testDayMs)       // 超期 1 天
	seedRequestRow(t, ctx, db, 41003, 41004, testStatusRejected, cutoff-30*testDayMs)    // 超期 30 天
	seedRequestRow(t, ctx, db, 41005, 41006, testStatusAccepted, nowMs-time.Hour.Milliseconds())
	seedRequestRow(t, ctx, db, 41007, 41008, testStatusPending, nowMs-365*testDayMs)     // 极老的 pending
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
