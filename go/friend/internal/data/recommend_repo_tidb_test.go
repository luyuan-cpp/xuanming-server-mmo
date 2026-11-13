package data

// recommend_repo_tidb_test.go —— 好友推荐两条 SQL 在**真 TiDB** 上的回归(结果 + 执行计划)。
//
// # 为什么单独要一组 TiDB 用例
//
// 数据层迁 TiDB 是既定方向(AGENTS §1),而推荐查询在两种库上靠的是两套互不相认的优化器提示(recommend_sql.go)。
// MySQL 那组真库用例(recommend_repo_mysql_test.go)对 TiDB 什么都证明不了,2026-10-09 就出过一次:
// 兜底查询里数共同好友的子查询,第一稿在 MySQL 上计划、读数、结果全对,拿到 TiDB 上却是每返回一行全扫一遍 friend
// (88 万条边的库 17,600,000 行、13.6 s)。那种错只有在 TiDB 上看计划才看得见,所以这里把"看计划"固化成用例。
//
// # 门控
//
// 只认环境变量 FRIEND_TEST_TIDB_DSN(与 MySQL 那组的 FRIEND_TEST_MYSQL_DSN 互不相干)。没设时本文件两条**顶层**用例
// 各自 SKIP —— 门控在顶层用例上判(friendTiDBTestDSN),不放进子用例:Go 里子用例 Skip 不会让父用例变成 SKIP,
// 那样"没连 TiDB"与"TiDB 计划守卫全过"在顶层结果里是同一个 PASS。
// 验收时另设 FRIEND_REQUIRE_TIDB_TESTS=1:没给 DSN 就判红而不是 SKIP(与 MySQL 那组的 FRIEND_REQUIRE_MYSQL_TESTS 同一个用意;
// 不带 -v 跑整包时 SKIP 行根本不打印,只有这个开关能把"跳过了"变成看得见的失败)。
// DSN 必须指向**专用测试库**:用例会 DROP / 重建 friend 的四张表,并对其中三张(推荐查询读的那三张)LOCK STATS / UNLOCK STATS。
// 指到 MySQL 上会在入口就判失败(sqldialect.Detect 判方言),不会误跑。
//
// # 断言的是什么
//
//  1. 结果:四类排除、调用方 exclude、共同好友数,与 MySQL 那组用同一批夹具、同一组期望值。
//  2. 计划:在三种统计状态下(新鲜 / 声称空表 / 停在表还很小的时候),TiDB 文本的 EXPLAIN 里
//     - 没有任何整表扫描或全索引扫描(算子名含 FullScan);
//     - 五条排除、RecommendByMutual 的 f2、兜底查询里数共同好友的 m2,都是"按连接键定位的主键范围读"
//     (access object 是 index:PRIMARY,operator info 里有 range: decided by);
//     - 没有任何警告 —— 提示写错别名时 TiDB 不报错,只给一条 1815,然后悄悄退回代价估算。
//     后两种统计状态正是不加提示时会退化成整表扫描的状态(2026-10-10 在 v8.5.2 上实测:同一批数据、MySQL 那份文本,
//     兜底查询的 b_in 全索引扫描,RecommendByMutual 的 b_in 与 f2 全索引扫描)。用例把 MySQL 文本在同一状态下的
//     计划也跑一遍、只打日志不断言:优化器哪天变聪明了不该让用例变红,但日志里能看出这组断言还有没有鉴别力。
//
// 断言看的是 EXPLAIN 的算子与访问方式,不看行数估计、不看耗时。

import (
	"context"
	"database/sql"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	drivermysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"friend/internal/sqldialect"
)

// friendTestTiDBDSNEnv 是本文件全部用例的唯一门控。
const friendTestTiDBDSNEnv = "FRIEND_TEST_TIDB_DSN"

// friendRequireTiDBEnv:验收时打开它,"没给 TiDB 的 DSN"就是失败而不是 SKIP。
// 它不是第二个门控 —— 门控仍是 FRIEND_TEST_TIDB_DSN 一个,这个开关只决定"跳过算不算失败"。
const friendRequireTiDBEnv = "FRIEND_REQUIRE_TIDB_TESTS"

// tidbExplainStatsLoadWaitMs 是计划用例那条连接上的 tidb_stats_load_sync_wait(毫秒)。
//
// TiDB 规划一条语句时要用到的列统计若还没加载进内存,会同步等它,最多等这么久(默认 100 毫秒);超时后按
// tidb_stats_load_pseudo_timeout(默认 ON)退回 pseudo 统计继续规划。对"统计新鲜"那一档来说,那就不是在测新鲜统计了 ——
// ANALYZE 之后的第一条 EXPLAIN 正好落在"统计刚写完、还没加载"的时刻,机器一忙就会超时。等待上限给到一分钟,
// 让三种统计状态各自测的确实是它名字说的那一种;仍然有界(用例整体另有 5 分钟的 ctx)。
const tidbExplainStatsLoadWaitMs = 60_000

// friendTiDBStatsTables 是用例会锁 / 解锁统计的三张表(推荐查询只读这三张)。
const friendTiDBStatsTables = "friend, friend_block, friend_request"

// friendTiDBTestDSN 是本文件的门控,**在顶层用例上调用**(理由见文件头"门控"):
// 没设 FRIEND_TEST_TIDB_DSN 时让调用它的那条用例 SKIP;同时设了 FRIEND_REQUIRE_TIDB_TESTS 时改为判红。
func friendTiDBTestDSN(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(friendTestTiDBDSNEnv))
	if dsn != "" {
		return dsn
	}
	if os.Getenv(friendRequireTiDBEnv) != "" {
		t.Fatalf("%s 要求跑真实 TiDB 用例,但 %s 为空 —— 推荐查询在 TiDB 上的计划守卫会整组静默跳过",
			friendRequireTiDBEnv, friendTestTiDBDSNEnv)
	}
	t.Skipf("%s 未设置,跳过真实 TiDB 用例", friendTestTiDBDSNEnv)
	return ""
}

// openFriendTiDBTestDB 连上 dsn(friendTiDBTestDSN 的返回值)、确认对端确实是 TiDB、把四张表重建成生产形态。
// 每个子用例各调一次:各自一条连接池、各自重建一遍表,互不依赖执行顺序。
func openFriendTiDBTestDB(t *testing.T, dsn string) (*sql.DB, context.Context) {
	t.Helper()
	db := openFriendTiDBTestPool(t, dsn)

	// 灌数最多的用例要写约 5.7 万行;预算给足,但仍然有界。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	require.NoError(t, db.PingContext(ctx))

	dialect, info, err := sqldialect.Detect(ctx, db)
	require.NoError(t, err)
	require.Equal(t, sqldialect.TiDB, dialect,
		"%s 指向的不是 TiDB(%s):这组用例断言的是 TiDB 的执行计划,在别的库上没有意义", friendTestTiDBDSNEnv, info)

	resetFriendTiDBSchema(t, ctx, db)
	return db, ctx
}

// openFriendTiDBTestPool 只建独立物理连接池，不重置表，供双副本共享同一事务域。
// data 不能反向 import svc；会话参数与 svc.BuildDSN 的生产强读策略一致，
// authority_db_test.go 另以生产入口核验每条物理连接。
func openFriendTiDBTestPool(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	parsed, err := drivermysql.ParseDSN(dsn)
	require.NoError(t, err)
	if parsed.Params == nil {
		parsed.Params = make(map[string]string)
	}
	parsed.Params["transaction_isolation"] = "'READ-COMMITTED'"
	parsed.Params["sql_mode"] = "'STRICT_TRANS_TABLES'"
	parsed.Params["tidb_txn_mode"] = "'pessimistic'"
	parsed.Params["tidb_read_staleness"] = "0"
	parsed.Params["tidb_low_resolution_tso"] = "OFF"
	parsed.Params["tidb_replica_read"] = "'leader'"
	parsed.Params["tidb_read_consistency"] = "'strict'"
	parsed.Params["tidb_snapshot"] = "''"
	db, err := sql.Open("mysql", parsed.FormatDSN())
	require.NoError(t, err)
	db.SetMaxOpenConns(32)
	db.SetMaxIdleConns(32)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// resetFriendTiDBSchema 把四张表重建成生产形态:主键 NONCLUSTERED + SHARD_ROW_ID_BITS / PRE_SPLIT_REGIONS,
// 与 proto/friend/friend_table.proto 的 TiDB 选项一致(聚簇与否会改变 TiDB 的访问路径,计划用例必须按生产形态测)。
// 二级索引的名字沿用 MySQL 那组夹具的写法;推荐查询只用主键(FORCE INDEX (PRIMARY)),名字不影响结论。
func resetFriendTiDBSchema(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	// 上一轮异常退出可能留下被锁住的统计:先解锁。表不存在时这条会报错,忽略即可。
	_, _ = db.ExecContext(ctx, "UNLOCK STATS "+friendTiDBStatsTables)

	const tidbTableOptions = " /*T! SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=4 */"
	statements := []string{
		"DROP TABLE IF EXISTS friend_block",
		"DROP TABLE IF EXISTS friend_capacity",
		"DROP TABLE IF EXISTS friend_request",
		"DROP TABLE IF EXISTS friend",
		`CREATE TABLE friend (
            player_id BIGINT UNSIGNED NOT NULL DEFAULT 0,
            friend_player_id BIGINT UNSIGNED NOT NULL DEFAULT 0,
            since_ms BIGINT UNSIGNED NOT NULL DEFAULT 0,
            PRIMARY KEY (player_id, friend_player_id) /*T![clustered_index] NONCLUSTERED */,
            KEY idx_friend_player (friend_player_id)
        )` + tidbTableOptions,
		`CREATE TABLE friend_request (
            from_player_id BIGINT UNSIGNED NOT NULL DEFAULT 0,
            to_player_id BIGINT UNSIGNED NOT NULL DEFAULT 0,
            request_time_ms BIGINT UNSIGNED NOT NULL DEFAULT 0,
            status INT UNSIGNED NOT NULL DEFAULT 0,
            updated_ms BIGINT UNSIGNED NOT NULL DEFAULT 0,
            request_id BIGINT UNSIGNED NOT NULL DEFAULT 0,
            PRIMARY KEY (from_player_id, to_player_id) /*T![clustered_index] NONCLUSTERED */,
            KEY idx_to_player (to_player_id, status),
            KEY idx_status_updated (status, updated_ms),
            KEY idx_request_id (request_id)
        )` + tidbTableOptions,
		`CREATE TABLE friend_capacity (
            player_id BIGINT UNSIGNED NOT NULL DEFAULT 0,
            friend_count INT UNSIGNED NOT NULL DEFAULT 0,
            created_ms BIGINT UNSIGNED NOT NULL DEFAULT 0,
            PRIMARY KEY (player_id) /*T![clustered_index] NONCLUSTERED */,
            KEY idx_count_created (friend_count, created_ms)
        )` + tidbTableOptions,
		`CREATE TABLE friend_block (
            player_id BIGINT UNSIGNED NOT NULL DEFAULT 0,
            blocked_player_id BIGINT UNSIGNED NOT NULL DEFAULT 0,
            since_ms BIGINT UNSIGNED NOT NULL DEFAULT 0,
            PRIMARY KEY (player_id, blocked_player_id) /*T![clustered_index] NONCLUSTERED */,
            KEY idx_blocked_player (blocked_player_id)
        )` + tidbTableOptions,
	}
	for _, stmt := range statements {
		_, err := db.ExecContext(ctx, stmt)
		require.NoError(t, err, "重建 TiDB 夹具表失败: %s", stmt)
	}

	// 收尾:解锁统计、删表,不把锁着统计的表留给后来的人。用独立的 ctx —— 用例自己的 ctx 这时可能已经超时。
	// t.Cleanup 是 LIFO:本回调在 openFriendTiDBTestDB 注册 Close 之后注册,所以先于 Close 执行,连接还开着。
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if _, err := db.ExecContext(cleanupCtx, "UNLOCK STATS "+friendTiDBStatsTables); err != nil {
			t.Logf("解锁 TiDB 夹具表的统计失败(下一轮开头会再解一次): %v", err)
		}
		for _, table := range []string{"friend_block", "friend_capacity", "friend_request", "friend"} {
			if _, err := db.ExecContext(cleanupCtx, "DROP TABLE IF EXISTS "+table); err != nil {
				t.Logf("删除 TiDB 夹具表 %s 失败: %v", table, err)
			}
		}
	})
}

// newFriendTiDBTestRepo 与 newFriendTestRepo 的装配只差方言一项:推荐查询因此渲染成 TiDB 那份文本。
func newFriendTiDBTestRepo(t *testing.T, db *sql.DB) *FriendRepo {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := newControlTestRedis(t, mr)
	opts := testFriendRepoOptions(testListReadHardLimit)
	opts.SQLDialect = sqldialect.TiDB
	return NewFriendRepo(rdb, db, opts)
}

// tidbRowQuerier 是 *sql.DB 与 *sql.Conn 的公共查询面:计划用例要在同一条连接上先 EXPLAIN 再 SHOW WARNINGS。
type tidbRowQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// queryTiDBRows 执行一条查询,把结果集读成"列名 → 值"的行(NULL 记为空串)。
func queryTiDBRows(t *testing.T, ctx context.Context, q tidbRowQuerier, query string) []map[string]string {
	t.Helper()
	rows, err := q.QueryContext(ctx, query)
	require.NoError(t, err, "查询失败: %.200s", query)
	defer rows.Close()
	cols, err := rows.Columns()
	require.NoError(t, err)
	var out []map[string]string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		require.NoError(t, rows.Scan(ptrs...))
		row := make(map[string]string, len(cols))
		for i, c := range cols {
			row[c] = vals[i].String
		}
		out = append(out, row)
	}
	require.NoError(t, rows.Err())
	return out
}

// tidbPlanTable 从 EXPLAIN 的 access object 列取出表别名:"table:b_in, index:PRIMARY(player_id, blocked_player_id)" → "b_in"。
func tidbPlanTable(accessObject string) (string, bool) {
	rest, ok := strings.CutPrefix(accessObject, "table:")
	if !ok {
		return "", false
	}
	alias, _, _ := strings.Cut(rest, ",")
	return strings.TrimSpace(alias), true
}

// tidbPlanSummary 是一份 EXPLAIN 里与本组断言有关的三样东西。
type tidbPlanSummary struct {
	fullScans []string        // 整表 / 全索引扫描的算子,形如 "IndexFullScan_146@b_in"
	located   map[string]bool // 走"主键 + 按连接键定位"的表别名
	warnings  []string        // SHOW WARNINGS 的原文,形如 "Warning 1815 ..."
}

// explainOnTiDB 在同一条连接上对 stmt 做 EXPLAIN 并取警告,归纳成 tidbPlanSummary。
func explainOnTiDB(t *testing.T, ctx context.Context, db *sql.DB, stmt string) tidbPlanSummary {
	t.Helper()
	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	defer conn.Close()

	// 会话变量只对这条连接生效;连接还回池子后别的语句可能复用它,多等一会儿统计加载对它们没有影响。
	_, err = conn.ExecContext(ctx, "SET SESSION tidb_stats_load_sync_wait = "+strconv.Itoa(tidbExplainStatsLoadWaitMs))
	require.NoError(t, err, "设置 tidb_stats_load_sync_wait 失败(理由见 tidbExplainStatsLoadWaitMs)")

	plan := queryTiDBRows(t, ctx, conn, "EXPLAIN "+stmt)
	require.NotEmpty(t, plan, "EXPLAIN 没有返回任何行")
	_, hasID := plan[0]["id"]
	_, hasAccess := plan[0]["access object"]
	_, hasInfo := plan[0]["operator info"]
	require.True(t, hasID && hasAccess && hasInfo,
		"EXPLAIN 的列不是 TiDB 的 id / access object / operator info(实际 %v):本组断言按这三列写", plan[0])

	summary := tidbPlanSummary{located: make(map[string]bool)}
	for _, row := range plan {
		operator := strings.TrimLeft(row["id"], " │├└─")
		table, hasTable := tidbPlanTable(row["access object"])
		if strings.Contains(operator, "FullScan") {
			summary.fullScans = append(summary.fullScans, operator+"@"+table)
		}
		if hasTable && strings.Contains(row["access object"], "index:PRIMARY") &&
			strings.Contains(row["operator info"], "range: decided by") {
			summary.located[table] = true
		}
	}
	for _, w := range queryTiDBRows(t, ctx, conn, "SHOW WARNINGS") {
		summary.warnings = append(summary.warnings, w["Level"]+" "+w["Code"]+" "+w["Message"])
	}
	return summary
}

// assertTiDBPlanIsIndexJoins 断言 stmt(TiDB 文本)的计划满足文件头第 2 条;wantLocated 是必须"按连接键定位"的表别名。
func assertTiDBPlanIsIndexJoins(t *testing.T, ctx context.Context, db *sql.DB, label, stmt string, wantLocated []string) {
	t.Helper()
	plan := explainOnTiDB(t, ctx, db, stmt)
	assert.Empty(t, plan.fullScans,
		"%s:计划里出现整表 / 全索引扫描 —— 扫描量会跟着那张表的总行数走,不再由窗口与好友上限封顶", label)
	for _, alias := range wantLocated {
		assert.True(t, plan.located[alias],
			"%s:%s 没有走「主键 + 按连接键定位」(实际按连接键定位的表:%v)。提示没生效,或连接条件让相关列落不到主键前缀上", label, alias, plan.located)
	}
	assert.Empty(t, plan.warnings,
		"%s:EXPLAIN 带警告。提示写错别名 / 查询块名时 TiDB 只给一条 1815、然后悄悄退回代价估算", label)
}

// tidbStatsState 是计划用例要走一遍的三种统计状态;prepare 在灌数之前跑,finish 在灌数之后跑。
type tidbStatsState struct {
	name    string
	prepare func(t *testing.T, ctx context.Context, db *sql.DB)
	finish  func(t *testing.T, ctx context.Context, db *sql.DB)
}

func lockFriendTiDBStats(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	_, err := db.ExecContext(ctx, "LOCK STATS "+friendTiDBStatsTables)
	require.NoError(t, err)
}

func tidbStatsStates() []tidbStatsState {
	return []tidbStatsState{
		{
			// 对照:灌完数据后 ANALYZE。
			name:   "统计新鲜",
			finish: analyzeRecommendTables,
		},
		{
			// 建表后立刻锁统计再灌数:统计声称三张表都是空的(物理导入后还没 ANALYZE 的那一段)。
			name:    "统计声称空表",
			prepare: lockFriendTiDBStats,
		},
		{
			// 每张表先放一两行与夹具无关的数据、ANALYZE、锁统计,再灌数:统计停在"表还很小"的时候。
			// 2026-10-10 实测,这个状态下不加提示的文本是:兜底查询的 b_in 全索引扫描;RecommendByMutual 的 b_in 与 f2 全索引扫描。
			name: "统计停在表还很小的时候",
			prepare: func(t *testing.T, ctx context.Context, db *sql.DB) {
				t.Helper()
				seedRecommendFriendship(t, ctx, db, 7, 8)
				seedBlock(t, ctx, db, 7, 8)
				seedPending(t, ctx, db, 7, 8)
				analyzeRecommendTables(t, ctx, db)
				lockFriendTiDBStats(t, ctx, db)
			},
		},
	}
}

// TestTiDBRecommend_PlanStaysIndexJoin 是 TiDB 上的计划守卫(文件头"断言的是什么"第 2 条)。
// 夹具用 MySQL 那组的 seedMutualAdversarialGraph:2 万个拉黑我的人、1.5 万条全服 pending、2 万条发给我的终态申请 ——
// 排除表足够大,整表扫描与逐候选定位在计划上分得开。每种统计状态各建一次库。
// 计划断言之后把两条查询真跑一遍:提示只该改变访问方式,不该改变结果。
func TestTiDBRecommend_PlanStaysIndexJoin(t *testing.T) {
	dsn := friendTiDBTestDSN(t)
	exclusionAliases := recommendExclusionAliases[:]
	for _, state := range tidbStatsStates() {
		t.Run(state.name, func(t *testing.T) {
			db, ctx := openFriendTiDBTestDB(t, dsn)
			repo := newFriendTiDBTestRepo(t, db)
			if state.prepare != nil {
				state.prepare(t, ctx, db)
			}
			g := seedMutualAdversarialGraph(t, ctx, db)
			if state.finish != nil {
				state.finish(t, ctx, db)
			}

			// 兜底查询:pivot 取 me 的 id,窗口里有我、我的好友和全部候选;me 有 30 个好友,所以数共同好友的 m2 也在计划里。
			anchorQuery, anchorArgs := repo.recommend.anchor(g.me, g.callerExclude, g.me, mutualAdvProductionLimit)
			assertTiDBPlanIsIndexJoins(t, ctx, db, "recommendAnchor / "+state.name,
				inlineNumericArgs(t, anchorQuery, anchorArgs...), append([]string{"m2"}, exclusionAliases...))

			mutualQuery, mutualArgs := repo.recommend.byMutual(g.me, g.callerExclude, mutualAdvProductionLimit)
			assertTiDBPlanIsIndexJoins(t, ctx, db, "RecommendByMutual / "+state.name,
				inlineNumericArgs(t, mutualQuery, mutualArgs...), append([]string{"f2"}, exclusionAliases...))

			// 只打日志的对照:同一状态下 MySQL 那份文本在 TiDB 上的计划(见文件头)。
			mysqlText := testMySQLRecommendStatements()
			anchorProbe, anchorProbeArgs := mysqlText.anchor(g.me, g.callerExclude, g.me, mutualAdvProductionLimit)
			mutualProbe, mutualProbeArgs := mysqlText.byMutual(g.me, g.callerExclude, mutualAdvProductionLimit)
			for _, probe := range []struct{ name, stmt string }{
				{"recommendAnchor", inlineNumericArgs(t, anchorProbe, anchorProbeArgs...)},
				{"RecommendByMutual", inlineNumericArgs(t, mutualProbe, mutualProbeArgs...)},
			} {
				plan := explainOnTiDB(t, ctx, db, probe.stmt)
				t.Logf("对照(不断言)%s / %s:MySQL 文本在 TiDB 上的整表 / 全索引扫描 = %v,警告 %d 条",
					probe.name, state.name, plan.fullScans, len(plan.warnings))
			}

			// 结果:候选共 293 人,少于窗口,limit 取 W 时每个合格者都必须出现、共同好友数逐人相同。
			got, err := repo.RecommendByMutual(ctx, g.me, g.callerExclude, RecommendAnchorWindow)
			require.NoError(t, err)
			g.assertCandidates(t, got, true)

			fromAnchor, err := repo.recommendAnchor(ctx, g.me, g.callerExclude, g.me, RecommendAnchorWindow)
			require.NoError(t, err)
			seen := make(map[uint64]bool, len(fromAnchor))
			for _, c := range fromAnchor {
				seen[c.CandidatePlayerID] = true
				if reason, bad := g.excluded[c.CandidatePlayerID]; bad {
					t.Errorf("兜底结果里多出 %d:%s", c.CandidatePlayerID, reason)
					continue
				}
				// friend 表里有出边的只有我、我的好友与候选(第三种状态另有 7 / 8 两人,id 小于 pivot、不在窗口里),
				// 所以兜底返回的每个人都必须是合格候选,共同好友数与 mutual 那条数出来的一致(两条路径数的是同一个数)。
				want, ok := g.want[c.CandidatePlayerID]
				if assert.True(t, ok, "兜底结果里出现了夹具之外的人 %d", c.CandidatePlayerID) {
					assert.Equal(t, want, c.MutualFriends, "候选 %d 的共同好友数(兜底路径)", c.CandidatePlayerID)
				}
			}
			for id := range g.want {
				assert.True(t, seen[id], "兜底结果里缺了合格候选 %d", id)
			}
		})
	}
}

// TestTiDBRecommend_ExclusionsMatchMySQLSemantics 用 MySQL 那组的两份小夹具再验一遍四类排除、反向对照、
// 顺序与共同好友数:期望值与 TestRecommendAnchor_AppliesAllFourExclusions / TestRecommendByMutual_AppliesAllFourExclusions
// 逐项相同。夹具很小(十来个人),这里验的是语义,不是计划。
func TestTiDBRecommend_ExclusionsMatchMySQLSemantics(t *testing.T) {
	dsn := friendTiDBTestDSN(t)
	t.Run("recommendAnchor", func(t *testing.T) {
		db, ctx := openFriendTiDBTestDB(t, dsn)
		repo := newFriendTiDBTestRepo(t, db)
		c, hub := seedAnchorGraph(t, ctx, db)
		analyzeRecommendTables(t, ctx, db)

		got, err := repo.recommendAnchor(ctx, c.me, nil, 1, 100)
		require.NoError(t, err)
		assert.Equal(t, []uint64{hub, c.plain, c.rejected, c.bystander}, recommendCandidateIDs(got),
			"按 player_id 升序、每人一次;被排除的五类人都不出现")
		wantMutual := map[uint64]uint32{hub: 1, c.plain: 0, c.rejected: 0, c.bystander: 0}
		for _, cand := range got {
			assert.Equal(t, wantMutual[cand.CandidatePlayerID], cand.MutualFriends, "候选 %d 的共同好友数", cand.CandidatePlayerID)
		}

		// 调用方 exclude 与 limit。
		got, err = repo.recommendAnchor(ctx, c.me, []uint64{hub, c.outsider, c.rejected}, 1, 100)
		require.NoError(t, err)
		assert.Equal(t, []uint64{c.plain, c.bystander}, recommendCandidateIDs(got))
		got, err = repo.recommendAnchor(ctx, c.me, nil, 1, 2)
		require.NoError(t, err)
		assert.Equal(t, []uint64{hub, c.plain}, recommendCandidateIDs(got))
	})

	t.Run("RecommendByMutual", func(t *testing.T) {
		db, ctx := openFriendTiDBTestDB(t, dsn)
		repo := newFriendTiDBTestRepo(t, db)
		c, hub, hub2 := seedMutualGraph(t, ctx, db)
		analyzeRecommendTables(t, ctx, db)

		got, err := repo.RecommendByMutual(ctx, c.me, nil, 100)
		require.NoError(t, err)
		assertRecommendIDs(t, c, []uint64{c.plain, c.rejected, c.bystander}, got)
		require.NotEmpty(t, got)
		assert.Equal(t, c.plain, got[0].CandidatePlayerID, "共同好友数最多的候选必须排在最前")
		for _, cand := range got {
			assert.NotEqual(t, hub, cand.CandidatePlayerID, "hub 已是我的好友,不该被推荐")
			assert.NotEqual(t, hub2, cand.CandidatePlayerID, "hub2 已是我的好友,不该被推荐")
			want := uint32(1)
			if cand.CandidatePlayerID == c.plain {
				want = 2
			}
			assert.Equal(t, want, cand.MutualFriends, "候选 %d 的共同好友数", cand.CandidatePlayerID)
		}
	})
}
