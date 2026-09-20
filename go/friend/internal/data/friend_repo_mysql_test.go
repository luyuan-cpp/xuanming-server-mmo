package data

// friend 数据层的真实 InnoDB 回归。
//
// 为什么必须是真 MySQL:本层的正确性全压在锁语义上(FOR UPDATE 的当前读、容量行串行化、
// RowsAffected 的 fail-closed 门禁)。mock 出来的"锁"只能复述我们自己的假设,
// 不可能复现 1213、也不可能复现 RC 下"普通 SELECT 读语句快照"这条真正咬人的性质。
// 所以本文件与同目录的 friend_guard_lock_order_mysql_test.go / block_repo_mysql_test.go /
// sweep_repo_test.go 一律用 FRIEND_TEST_MYSQL_DSN 门控,未设置就 Skip。
//
// ⚠ 本文件同时是本包所有集成用例的**公共夹具所有者**:schema 重建、临时 repo、
// 不变量断言都在这里,其它测试文件直接用(同包)。改 schema 只改这一处。
//
// ⚠⚠ 接缝假设:F2 批的 B1–B4 与本文件**并行落码**,没有编译器兜底。下面 "── 接缝假设 ──"
// 那一段把所有对生产代码签名的假设收在一处;如果 B1 的实际签名不同,**只改那一段**,
// 各用例的断言不必动。交付说明里已把这些假设逐条列为待核对项。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	drivermysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// ⚠ 本包的测试**不能** import friend/internal/config:config 包 import data 取
// data.DatabaseName(F1 §3.1),包内测试(package data)再反向 import 就是 import cycle,
// Go 直接拒编译。所以 sweep 的 mode 在本包的测试里一律写**字面量** "report_only" / "delete"
// —— 这恰好也是我们想钉的:data 层必须认这两个拼法。config 侧的常量拼写由
// internal/logic 的测试对齐(那边可以 import config)。

// ── 接缝假设 ────────────────────────────────────────────────────
//
// 规格 §3 只定了事务形状与语义,没有逐字定签名。这里按规格描述推导,并用瘦包装隔离:
// 生产签名一旦确定,改动面只有本段。

// testLimits 是一次写路径要用到的全部上限,与 config.FriendConf 的同名字段一一对应。
// 收成结构只为让每个调用点不必重复四个位置参数,读起来也看得出哪个数是哪条上限。
type testLimits struct {
	MaxFriends          uint32
	MaxPendingRequests  uint32 // 出站:我挂着的 pending
	MaxIncomingRequests uint32 // 入站:别人发给我的 pending
	MaxBlocks           uint32
}

// defaultTestLimits:各上限都设得很大,用例只调自己要验的那一条,
// 免得"另一条上限先命中"把断言意义换掉(那种测试红了也查不出原因)。
func defaultTestLimits() testLimits {
	return testLimits{MaxFriends: 1000, MaxPendingRequests: 1000, MaxIncomingRequests: 1000, MaxBlocks: 1000}
}

// callAddFriend —— 规格 §3.1 的权威事务入口。
//
// testLimits 比 data.AddFriendLimits 多一个 MaxBlocks(Block 的用例也用同一份夹具),
// 所以这里做一次显式转换,而不是把两个结构合并 —— 合并会让 AddFriendRequest 的签名
// 多出一个它根本不看的字段。
func callAddFriend(ctx context.Context, repo *FriendRepo, from, to uint64, lim testLimits) error {
	return repo.AddFriendRequest(ctx, from, to, AddFriendLimits{
		MaxFriends:          lim.MaxFriends,
		MaxPendingRequests:  lim.MaxPendingRequests,
		MaxIncomingRequests: lim.MaxIncomingRequests,
	})
}

// callBlock / callUnblock —— 规格 §3.4。Block 是 FriendRepo 的方法(block_repo.go 只是分文件):
// 它要失效双方的好友缓存、要复用同一个容量守卫,与 FriendRepo 共用 db/rdb 是最省的形状。
func callBlock(ctx context.Context, repo *FriendRepo, me, target uint64, lim testLimits) error {
	return repo.Block(ctx, me, target, lim.MaxBlocks)
}

func callUnblock(ctx context.Context, repo *FriendRepo, me, target uint64) error {
	return repo.Unblock(ctx, me, target)
}

// callListBlocks —— 列表读的硬上限不走参数:它在 NewFriendRepo 时交给 repo 保管
// (F2 §4 的实现选择,理由是缓存里存的是已截断的列表,逐调用传上限会让"这份列表按谁的
// 上限截的"取决于谁先回填)。所以要验限幅就得**换一个 repo**,见 newFriendTestRepoWithLimit。
func callListBlocks(ctx context.Context, repo *FriendRepo, me uint64) ([]BlockEntry, error) {
	return repo.ListBlocks(ctx, me)
}

// callSweep —— 规格 §3.8 的 SQL 侧。report_only 只 COUNT,delete 才真删;
// 两个模式都返回"本轮看到的待清理行数"与"实际删除行数",让调用方与测试都能分辨。
// nowMs 由调用方给(data 层不自己取 time.Now),所以保留期用例不依赖墙钟。
func callSweep(ctx context.Context, repo *FriendRepo, mode string, retentionDays, batchLimit int, nowMs int64) (pending int64, deleted int64, err error) {
	return repo.SweepTerminalRequests(ctx, mode, retentionDays, batchLimit, nowMs)
}

// ── 公共夹具 ────────────────────────────────────────────────────

// friendTestDSNEnv 是本包所有集成用例的唯一门控开关。
// 名字与 F1 已有用例一致,别再引入第二个门控:两个门控必然有一个长期没人设,
// 那条路径就会长期"SKIP 当绿"(A 仓 2026-08-11 的 1213 正是这样被盖住一个多月的)。
const friendTestDSNEnv = "FRIEND_TEST_MYSQL_DSN"

// friendRequireMySQLEnv:验收时打开它,就能把"跑了并且 PASS"与"跳过了"在报告里分开,
// 不必靠人肉读 -v 输出。它**不是**第二个门控 —— 门控仍是 FRIEND_TEST_MYSQL_DSN 一个,
// 这个开关只决定"跳过算不算失败"。
// A 仓 2026-08-11 那次 1213 被盖了一个多月,就是因为"全体 Skip"在 CI 报告里与"全绿"长得一样。
const friendRequireMySQLEnv = "FRIEND_REQUIRE_MYSQL_TESTS"

// TestFriendIntegrationGateIsHonored 是"全体 Skip 不算通过"的机械保障。
//
// 本包有 19 条集成用例(含 4 个并发锁序场景)全部挂在 openFriendTestDB 的 Skip 上;
// 不走 MySQL 的只剩 3 条缓存用例。没有这条标记用例时,"DSN 忘了设"与"锁序全对"
// 在 CI 报告里是同一个绿。
func TestFriendIntegrationGateIsHonored(t *testing.T) {
	if os.Getenv(friendRequireMySQLEnv) == "" {
		t.Skipf("%s 未设置:本用例只在验收时启用", friendRequireMySQLEnv)
	}
	if strings.TrimSpace(os.Getenv(friendTestDSNEnv)) == "" {
		t.Fatalf("%s 要求跑 MySQL 集成用例,但 %s 为空 —— 本包的 19 条集成用例(含 4 个并发锁序场景)"+
			"会全部静默 Skip", friendRequireMySQLEnv, friendTestDSNEnv)
	}
}

// openFriendTestDB 门控 + 建连 + 重建 schema + 注册清理。
//
// 清理分两层(AGENTS.md §11.4"集成测试必须清理自身状态"):
//   - 用例开头 DROP/CREATE,保证不被上一条用例的残留污染(不依赖执行顺序);
//   - 用例结束 TRUNCATE,保证不把自己造的数据留给别人 / 留给人工排障时的困惑。
//
// t.Cleanup 是 LIFO,所以先注册 Close 再注册 TRUNCATE —— 否则连接先关,TRUNCATE 必失败。
func openFriendTestDB(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(friendTestDSNEnv))
	if dsn == "" {
		t.Skipf("%s 未设置,跳过真实 MySQL 集成用例", friendTestDSNEnv)
	}

	db, err := sql.Open("mysql", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	// 并发用例要 16 条连接同时在事务里;池比并发度小的话,后来的 goroutine 会在
	// database/sql 的队列里排队,压根进不了 InnoDB —— 那样"并发 16"是假的,
	// 死锁窗口根本不会被打开(这个坑会让锁序测试永远绿)。
	db.SetMaxOpenConns(32)
	db.SetMaxIdleConns(32)

	// 整个用例的时间预算显式给死:集成测试不许无界等待。真死锁 / 真锁等待会在这里超时,
	// 而不是把 CI 挂住。
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	require.NoError(t, db.PingContext(ctx))

	resetFriendIntegrationSchema(t, ctx, db)
	t.Cleanup(func() { truncateFriendIntegrationTables(t, db) })
	return db, ctx
}

// newFriendTestRepo 给一个挂在 miniredis 上的 repo。
// 缓存用 miniredis 而不是真 Redis:本文件要验的是 MySQL 侧语义,缓存只需要"能用、
// 能失效",miniredis 自带 gopher-lua,两条 Lua 真实执行,够了。
func newFriendTestRepo(t *testing.T, db *sql.DB) (*FriendRepo, *miniredis.Miniredis) {
	t.Helper()
	// 默认上限取得足够大,让绝大多数用例不必关心它;要验限幅的用例自己传小值。
	return newFriendTestRepoWithLimit(t, db, testListReadHardLimit)
}

// testListReadHardLimit 是默认的列表读硬上限:比任何用例造的数据都大,
// 这样"被截断"永远只会是被测代码的行为,不会是夹具的副作用。
const testListReadHardLimit uint32 = 1000

func newFriendTestRepoWithLimit(t *testing.T, db *sql.DB, listReadHardLimit uint32) (*FriendRepo, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.MustNewRedis(redis.RedisConf{Host: mr.Addr(), Type: "node"})
	return NewFriendRepo(rdb, db, time.Minute, listReadHardLimit), mr
}

// newCacheOnlyRepo 给一个**没有 DB**的 repo,只用来验缓存键与失效脚本。
// db 传 nil 是刻意的:凡是会触库的用例都必须走 openFriendTestDB 的门控,
// 一个 nil DB 会让"不小心触库"立刻 panic 而不是静默连到别处。
func newCacheOnlyRepo(t *testing.T) (*FriendRepo, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.MustNewRedis(redis.RedisConf{Host: mr.Addr(), Type: "node"})
	return NewFriendRepo(rdb, nil, time.Minute, testListReadHardLimit), mr
}

// ── 不变量断言(所有并发用例共用) ──────────────────────────────
//
// 这四条是 friend 存储层的**全部**结构不变量。并发用例不去猜"哪个事务该赢",
// 只断言"无论谁赢,库里不许出现这四种形状" —— 这样断言与调度无关,不会 flaky,
// 也不会因为换了个锁序实现就要重写。

// assertCapacityMatchesEdges:friend_count 必须等于 friend 表里该玩家的实际出边数。
// 漏减(§3.4 ⑤)、漏加、或在 RowsAffected==0 时照样加减,都只会在这条上暴露 ——
// 而且这类偏差在生产上是静默的:玩家只是"永远加不满好友",没有任何报错。
func assertCapacityMatchesEdges(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
		SELECT c.player_id, c.friend_count,
		       (SELECT COUNT(*) FROM friend f WHERE f.player_id = c.player_id)
		FROM friend_capacity c`)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var playerID uint64
		var counted, actual int64
		require.NoError(t, rows.Scan(&playerID, &counted, &actual))
		assert.Equal(t, actual, counted,
			"player %d 的 friend_count=%d 与实际边数 %d 不一致:计数漂移", playerID, counted, actual)
	}
	require.NoError(t, rows.Err())

	// 反向:有边却没有容量行 = 这个玩家的硬上限当场失效(下次 ensure 会按权威边数补回来,
	// 但在补回来之前的那一刻上限是不存在的)。
	var orphanEdges int64
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM (SELECT DISTINCT player_id FROM friend) e
		WHERE NOT EXISTS (SELECT 1 FROM friend_capacity c WHERE c.player_id = e.player_id)`).Scan(&orphanEdges))
	assert.Zero(t, orphanEdges, "存在有好友边、却没有 friend_capacity 行的玩家:该玩家的好友上限失效")
}

// assertNoFriendAndBlocked:同一对玩家不得"既是好友又拉黑"。
// Block 要删双向边,AddFriend/AcceptFriend 要在守卫内复核拉黑;锁序写错时这两件事会交错,
// 结果是玩家在好友列表里看得见一个自己拉黑过的人,且客户端无从解释。
func assertNoFriendAndBlocked(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	var bad int64
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM friend f JOIN friend_block b
		  ON (b.player_id = f.player_id AND b.blocked_player_id = f.friend_player_id)
		  OR (b.player_id = f.friend_player_id AND b.blocked_player_id = f.player_id)`).Scan(&bad))
	assert.Zero(t, bad, "出现了'既是好友又拉黑'的对:Block 的删边与建边路径没有被容量守卫串行化")
}

// assertNoPendingBetweenBlockedPairs:拉黑之后不得残留 pending 申请。
// Block 会把两个方向的 pending 置终态(§3.4 ⑥);而并发的 AddFriend 必须在守卫内看到
// 拉黑并拒绝。两者任一失守都会留下"我拉黑了他、他的申请还在我的收件箱里"。
func assertNoPendingBetweenBlockedPairs(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	var bad int64
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM friend_request r JOIN friend_block b
		  ON (b.player_id = r.from_player_id AND b.blocked_player_id = r.to_player_id)
		  OR (b.player_id = r.to_player_id AND b.blocked_player_id = r.from_player_id)
		WHERE r.status = 1`).Scan(&bad))
	assert.Zero(t, bad, "拉黑后仍有 pending 申请:Block 的 pending 取消与 AddFriend 的拉黑复核至少有一处不在守卫内")
}

// assertNoPendingBetweenFriends:已是好友的一对不得还挂着 pending 申请。
// 这条专门钉规格 §2.1 的 ABBA 环:同一对玩家并发 AddFriend / AcceptFriend 时,
// 若两个事务都"看不见"对方,就会同时产出"好友边"与"新的 pending",客户端界面上
// 表现为一个已经是好友的人还在申请列表里、点接受又说没有申请。
func assertNoPendingBetweenFriends(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	var bad int64
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM friend_request r JOIN friend f
		  ON f.player_id = r.from_player_id AND f.friend_player_id = r.to_player_id
		WHERE r.status = 1`).Scan(&bad))
	assert.Zero(t, bad, "已是好友的一对还挂着 pending 申请:AddFriend 的'已是好友'复核没有在守卫内做当前读")
}

func assertFriendInvariants(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	assertCapacityMatchesEdges(t, ctx, db)
	assertNoFriendAndBlocked(t, ctx, db)
	assertNoPendingBetweenBlockedPairs(t, ctx, db)
	assertNoPendingBetweenFriends(t, ctx, db)
}

// isInnoDBDeadlock 把 1213 从其它错误里单独挑出来。
//
// 不这样做的后果(A 仓 2026-08-11 的原始症状):死锁被包进一句"意外错误",
// 看起来像业务失败,没人想到去查锁序。1213 是**可重试**错误,所以它也绝不能被
// 当成"偶发抖动"忽略 —— 本层不重试,一次 1213 就是一次玩家可见的失败。
func isInnoDBDeadlock(err error) bool {
	if err == nil {
		return false
	}
	var my *drivermysql.MySQLError
	if errors.As(err, &my) && my.Number == 1213 {
		return true
	}
	// 兜底:错误被 fmt.Errorf 包了多层而 errors.As 链断掉时(例如中间有一层只存了 Error() 文本)。
	return strings.Contains(err.Error(), "Error 1213") || strings.Contains(err.Error(), "Deadlock found")
}

func assertNoDeadlock(t *testing.T, err error, what string) {
	t.Helper()
	if isInnoDBDeadlock(err) {
		t.Fatalf("%s 触发 InnoDB 死锁(1213):任何写事务在拿到容量守卫之前不得做任何锁定读"+
			"(规格 §2.2),锁序一旦反过来这条必挂 —— %v", what, err)
	}
}

// ── schema ─────────────────────────────────────────────────────

// resetFriendIntegrationSchema 重建四张表。
//
// 列名与索引与 proto/friend/friend_table.proto 逐字对齐(那是唯一事实源,由 go/schemamigrate 建表):
//   - friend_request 的 updated_ms 是 F2 新增列,sweep 的 (status, updated_ms) 索引靠它;
//     没有这列,sweep 的保留期过滤退化成"全表都过期"。
//   - friend_block 是 F2 新增表。
//   - status 用 INT UNSIGNED(表 proto 里是 uint32),不是原来的 TINYINT:
//     测试 schema 与生产 schema 的类型不一致会让"列宽相关"的缺陷只在生产出现。
func resetFriendIntegrationSchema(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	statements := []string{
		"DROP TABLE IF EXISTS friend_block",
		"DROP TABLE IF EXISTS friend_capacity",
		"DROP TABLE IF EXISTS friend_request",
		"DROP TABLE IF EXISTS friend",
		`CREATE TABLE friend (
            player_id BIGINT UNSIGNED NOT NULL,
            friend_player_id BIGINT UNSIGNED NOT NULL,
            since_ms BIGINT UNSIGNED NOT NULL DEFAULT 0,
            PRIMARY KEY (player_id, friend_player_id),
            KEY idx_friend_player (friend_player_id)
        ) ENGINE=InnoDB`,
		`CREATE TABLE friend_request (
            from_player_id BIGINT UNSIGNED NOT NULL,
            to_player_id BIGINT UNSIGNED NOT NULL,
            request_time_ms BIGINT UNSIGNED NOT NULL DEFAULT 0,
            status INT UNSIGNED NOT NULL DEFAULT 1,
            updated_ms BIGINT UNSIGNED NOT NULL DEFAULT 0,
            PRIMARY KEY (from_player_id, to_player_id),
            KEY idx_to_player (to_player_id, status),
            KEY idx_status_updated (status, updated_ms)
        ) ENGINE=InnoDB`,
		`CREATE TABLE friend_capacity (
            player_id BIGINT UNSIGNED NOT NULL,
            friend_count INT UNSIGNED NOT NULL DEFAULT 0,
            PRIMARY KEY (player_id)
        ) ENGINE=InnoDB`,
		`CREATE TABLE friend_block (
            player_id BIGINT UNSIGNED NOT NULL,
            blocked_player_id BIGINT UNSIGNED NOT NULL,
            since_ms BIGINT UNSIGNED NOT NULL DEFAULT 0,
            PRIMARY KEY (player_id, blocked_player_id),
            KEY idx_blocked_player (blocked_player_id)
        ) ENGINE=InnoDB`,
	}
	for i, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("reset friend integration schema step %d failed: %v\n%s", i, err, fmt.Sprintf("%.120s", statement))
		}
	}
}

// truncateFriendIntegrationTables 只清数据、不动表结构:留着表让排障时还能看一眼
// 上一次跑的形状(DROP 掉反而不方便),但不留行 —— 留行会让下一个人误以为是生产数据。
func truncateFriendIntegrationTables(t *testing.T, db *sql.DB) {
	t.Helper()
	// 用独立的短 ctx:主用例的 ctx 可能已经因为超时被 cancel,那时清理仍必须跑完。
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, table := range []string{"friend_block", "friend_capacity", "friend_request", "friend"} {
		if _, err := db.ExecContext(ctx, "TRUNCATE TABLE "+table); err != nil {
			t.Logf("清理 %s 失败(不影响本次结论,但会污染下一次人工排障): %v", table, err)
		}
	}
}

// seedPending 直接写一条 pending 申请行,绕过业务事务。
// 造前置状态用直写而不是调 AddFriend:用例要验的那条上限本身会挡住"造够数量"这件事,
// 用业务接口造数据会让前置和断言互相纠缠。
func seedPending(t *testing.T, ctx context.Context, db *sql.DB, from, to uint64) {
	t.Helper()
	now := time.Now().UnixMilli()
	_, err := db.ExecContext(ctx,
		`INSERT INTO friend_request (from_player_id, to_player_id, request_time_ms, status, updated_ms)
		 VALUES (?, ?, ?, 1, ?)`, from, to, now, now)
	require.NoError(t, err)
}

// seedFriendEdges 给 playerID 造 n 条出边并把容量行对齐,用于把它顶到好友上限。
func seedFriendEdges(t *testing.T, ctx context.Context, db *sql.DB, playerID uint64, friendIDs ...uint64) {
	t.Helper()
	for _, fid := range friendIDs {
		_, err := db.ExecContext(ctx,
			"INSERT IGNORE INTO friend (player_id, friend_player_id, since_ms) VALUES (?, ?, 1)", playerID, fid)
		require.NoError(t, err)
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO friend_capacity (player_id, friend_count)
		VALUES (?, (SELECT COUNT(*) FROM friend f WHERE f.player_id = ?))
		ON DUPLICATE KEY UPDATE friend_count = VALUES(friend_count)`, playerID, playerID)
	require.NoError(t, err)
}

func seedBlock(t *testing.T, ctx context.Context, db *sql.DB, playerID, blockedID uint64) {
	t.Helper()
	_, err := db.ExecContext(ctx,
		"INSERT IGNORE INTO friend_block (player_id, blocked_player_id, since_ms) VALUES (?, ?, 1)",
		playerID, blockedID)
	require.NoError(t, err)
}

func mustCount(t *testing.T, ctx context.Context, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.QueryRowContext(ctx, query, args...).Scan(&n))
	return n
}

func readRequestUpdatedMs(t *testing.T, ctx context.Context, db *sql.DB, from, to uint64) uint64 {
	t.Helper()
	var v uint64
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT updated_ms FROM friend_request WHERE from_player_id=? AND to_player_id=?",
		from, to).Scan(&v))
	return v
}

func readRequestStatus(t *testing.T, ctx context.Context, db *sql.DB, from, to uint64) int64 {
	t.Helper()
	var v int64
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT status FROM friend_request WHERE from_player_id=? AND to_player_id=?",
		from, to).Scan(&v))
	return v
}

// ── AcceptFriend:并发硬上限(F1 既有用例,一行未动的业务断言) ──

// TestAcceptFriend_ConcurrentHardLimit 验"好友数硬上限不被并发穿透"(业务性质)。
// 它与 friend_guard_lock_order_mysql_test.go 验的"不产生 1213"(锁序性质)不可互相替代:
// 把守卫挪到锁定读之后,本用例仍可能碰巧过,而那个文件必红。
func TestAcceptFriend_ConcurrentHardLimit(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	const (
		targetID       uint64 = 9000
		requesterCount        = 20
		maxFriends     uint32 = 5
	)
	for i := 0; i < requesterCount; i++ {
		seedPending(t, ctx, db, uint64(1000+i), targetID)
	}

	start := make(chan struct{})
	results := make(chan error, requesterCount)
	var wg sync.WaitGroup
	for i := 0; i < requesterCount; i++ {
		fromID := uint64(1000 + i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- repo.AcceptFriend(ctx, fromID, targetID, maxFriends)
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	succeeded := 0
	full := 0
	for err := range results {
		assertNoDeadlock(t, err, "并发 AcceptFriend")
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrAcceptorFriendsFull):
			full++
		default:
			t.Fatalf("unexpected AcceptFriend result: %v", err)
		}
	}
	assert.Equal(t, int(maxFriends), succeeded)
	assert.Equal(t, requesterCount-int(maxFriends), full)

	assert.EqualValues(t, maxFriends, mustCount(t, ctx, db,
		"SELECT COUNT(*) FROM friend WHERE player_id = ?", targetID))
	assert.EqualValues(t, maxFriends, mustCount(t, ctx, db,
		"SELECT friend_count FROM friend_capacity WHERE player_id = ?", targetID))
	assert.EqualValues(t, maxFriends, mustCount(t, ctx, db,
		"SELECT COUNT(*) FROM friend_request WHERE to_player_id = ? AND status = 2", targetID))
	assert.EqualValues(t, requesterCount-int(maxFriends), mustCount(t, ctx, db,
		"SELECT COUNT(*) FROM friend_request WHERE to_player_id = ? AND status = 1", targetID),
		"满员失败必须回滚 pending->accepted，申请仍可在腾出名额后处理")
	assertFriendInvariants(t, ctx, db)

	var acceptedFrom uint64
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT from_player_id FROM friend_request WHERE to_player_id = ? AND status = 2 LIMIT 1",
		targetID).Scan(&acceptedFrom))
	assert.ErrorIs(t, repo.AcceptFriend(ctx, acceptedFrom, targetID, 100), ErrRequestNotFound,
		"已接受申请不能被二次消费")

	var rejectedFrom uint64
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT from_player_id FROM friend_request WHERE to_player_id = ? AND status = 1 LIMIT 1",
		targetID).Scan(&rejectedFrom))
	_, err := db.ExecContext(ctx,
		"UPDATE friend_request SET status=3 WHERE from_player_id=? AND to_player_id=?",
		rejectedFrom, targetID)
	require.NoError(t, err)
	assert.ErrorIs(t, repo.AcceptFriend(ctx, rejectedFrom, targetID, 100), ErrRequestNotFound,
		"已拒绝申请不能建立好友关系")

	const neverRequested uint64 = 777777
	assert.ErrorIs(t, repo.AcceptFriend(ctx, neverRequested, targetID, 100), ErrRequestNotFound,
		"从未申请的玩家不能建立好友关系")
	assert.Zero(t, mustCount(t, ctx, db,
		"SELECT COUNT(*) FROM friend_capacity WHERE player_id = ?", neverRequested),
		"无申请调用也不能制造无界 capacity 空行")
}

// ── AddFriend:权威上限三条(F2 新增) ─────────────────────────
//
// 这三条为什么必须在数据层验、不能只在 logic 层验:F1 的 logic 层做的是事务外
// check-then-act(读一次 COUNT 再写),两个并发请求都能通过检查。上限只有在
// 守卫锁内的当前读里才是权威 —— 所以断言要落在"库里最终有几行",不是"接口回了什么码"。

func TestAddFriend_OutgoingPendingLimitIsAuthoritative(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	const me uint64 = 20001
	lim := defaultTestLimits()
	lim.MaxPendingRequests = 3
	for i := 0; i < int(lim.MaxPendingRequests); i++ {
		seedPending(t, ctx, db, me, uint64(20100+i))
	}

	err := callAddFriend(ctx, repo, me, 20999, lim)
	assert.ErrorIs(t, err, ErrTooManyPending, "出站 pending 已达 MaxPendingRequests,必须 fail-closed")
	assert.EqualValues(t, lim.MaxPendingRequests, mustCount(t, ctx, db,
		"SELECT COUNT(*) FROM friend_request WHERE from_player_id=? AND status=1", me),
		"被拒的申请不得落库")
	assert.Zero(t, mustCount(t, ctx, db,
		"SELECT COUNT(*) FROM friend_request WHERE from_player_id=? AND to_player_id=?", me, uint64(20999)))

	// 终态行不占出站名额:reject/accept 只翻 status 不删行,若 COUNT 不带 status=1,
	// 玩家会在处理掉几条申请之后"永久"发不出新申请,而且没有任何提示。
	_, err = db.ExecContext(ctx,
		"UPDATE friend_request SET status=3, updated_ms=1 WHERE from_player_id=? AND to_player_id=?",
		me, uint64(20100))
	require.NoError(t, err)
	assert.NoError(t, callAddFriend(ctx, repo, me, 20999, lim),
		"终态(status<>1)的历史申请行不应占用出站名额")
	assert.Equal(t, int64(1), readRequestStatus(t, ctx, db, me, 20999))
	assertFriendInvariants(t, ctx, db)
}

func TestAddFriend_TargetInboxLimitIsAuthoritative(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	const target uint64 = 21000
	lim := defaultTestLimits()
	lim.MaxIncomingRequests = 2
	for i := 0; i < int(lim.MaxIncomingRequests); i++ {
		seedPending(t, ctx, db, uint64(21100+i), target)
	}

	// 入站上限是被骚扰者的保护:出站上限只管住"单个刷子挂多少条",
	// 挡不住 N 个小号各发一条把一个人的收件箱撑爆。
	err := callAddFriend(ctx, repo, 21999, target, lim)
	assert.ErrorIs(t, err, ErrTargetInboxFull, "target 收件箱已满,必须 fail-closed")
	assert.EqualValues(t, lim.MaxIncomingRequests, mustCount(t, ctx, db,
		"SELECT COUNT(*) FROM friend_request WHERE to_player_id=? AND status=1", target))
	assertFriendInvariants(t, ctx, db)
}

func TestAddFriend_FriendCountLimitsBothSides(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	lim := defaultTestLimits()
	lim.MaxFriends = 2

	// 我已满:再发申请也没有意义 —— 对方接受时必然失败,现在拒掉比让对方白点一次好。
	const meFull uint64 = 22001
	seedFriendEdges(t, ctx, db, meFull, 22101, 22102)
	assert.ErrorIs(t, callAddFriend(ctx, repo, meFull, 22999, lim), ErrSenderFriendsFull)
	assert.Zero(t, mustCount(t, ctx, db,
		"SELECT COUNT(*) FROM friend_request WHERE from_player_id=?", meFull))

	// 对方已满:同理,而且这条只能在事务内查 —— 对方的 friend_count 随时在变。
	const targetFull uint64 = 22002
	seedFriendEdges(t, ctx, db, targetFull, 22201, 22202)
	assert.ErrorIs(t, callAddFriend(ctx, repo, 22998, targetFull, lim), ErrAcceptorFriendsFull)
	assert.Zero(t, mustCount(t, ctx, db,
		"SELECT COUNT(*) FROM friend_request WHERE to_player_id=?", targetFull))
	assertFriendInvariants(t, ctx, db)
}

func TestAddFriend_RejectsBlockedInEitherDirection(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)
	lim := defaultTestLimits()

	// 对方拉黑了我:这是拉黑的主要用途,必须挡住。
	const a, b uint64 = 23001, 23002
	seedBlock(t, ctx, db, b, a)
	assert.ErrorIs(t, callAddFriend(ctx, repo, a, b, lim), ErrBlocked)

	// 我拉黑了对方:也必须挡住,而不是"我方单向拉黑不影响我发申请"。
	// 放过去会造出"我拉黑的人出现在我的好友列表里"这种无法向玩家解释的状态。
	const c, d uint64 = 23003, 23004
	seedBlock(t, ctx, db, c, d)
	assert.ErrorIs(t, callAddFriend(ctx, repo, c, d, lim), ErrBlocked)

	assert.Zero(t, mustCount(t, ctx, db, "SELECT COUNT(*) FROM friend_request"))
	assertFriendInvariants(t, ctx, db)
}

func TestAddFriend_RejectsDuplicateAndExistingFriendship(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)
	lim := defaultTestLimits()

	const a, b uint64 = 24001, 24002
	require.NoError(t, callAddFriend(ctx, repo, a, b, lim))
	assert.ErrorIs(t, callAddFriend(ctx, repo, a, b, lim), ErrRequestAlreadySent,
		"同一方向的 pending 申请不得重复计一条")
	assert.Equal(t, int64(1), mustCount(t, ctx, db,
		"SELECT COUNT(*) FROM friend_request WHERE from_player_id=? AND to_player_id=?", a, b))

	require.NoError(t, repo.AcceptFriend(ctx, a, b, lim.MaxFriends))
	assert.ErrorIs(t, callAddFriend(ctx, repo, a, b, lim), ErrAlreadyFriends)
	assertFriendInvariants(t, ctx, db)
}

// TestAddFriend_WritesUpdatedMs 钉 F2-14:updated_ms 的写入方。
// 没有写入方时全表恒为 0,sweep 的 delete 模式会把**每一行**判成已过期 ——
// 一次定时任务清空整张 friend_request。所以"有没有写 updated_ms"必须有测试,
// 不能靠 code review 记得。
func TestAddFriend_WritesUpdatedMs(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)
	lim := defaultTestLimits()

	const a, b uint64 = 25001, 25002
	require.NoError(t, callAddFriend(ctx, repo, a, b, lim))
	assert.NotZero(t, readRequestUpdatedMs(t, ctx, db, a, b), "AddFriend 必须写 updated_ms")

	// 用直写把 updated_ms 归零,再看状态迁移是否**重新**写上:
	// 只断言"非零"会被"插入时写过一次、之后 UPDATE 都不写"糊弄过去。
	_, err := db.ExecContext(ctx,
		"UPDATE friend_request SET updated_ms=0 WHERE from_player_id=? AND to_player_id=?", a, b)
	require.NoError(t, err)
	require.NoError(t, repo.AcceptFriend(ctx, a, b, lim.MaxFriends))
	assert.NotZero(t, readRequestUpdatedMs(t, ctx, db, a, b), "AcceptFriend 的 status=2 必须同时写 updated_ms")

	const c, d uint64 = 25003, 25004
	require.NoError(t, callAddFriend(ctx, repo, c, d, lim))
	_, err = db.ExecContext(ctx,
		"UPDATE friend_request SET updated_ms=0 WHERE from_player_id=? AND to_player_id=?", c, d)
	require.NoError(t, err)
	require.NoError(t, repo.RejectFriend(ctx, c, d))
	assert.NotZero(t, readRequestUpdatedMs(t, ctx, db, c, d), "RejectFriend 的 status=3 必须同时写 updated_ms")
}

// TestAcceptFriend_AlsoResolvesReversePending 钉规格 §3.2 的"反向 pending 一并 accept"。
// 漏了这条会留下一条**永远 pending 的孤儿申请**:两人已经是好友,其中一方的申请列表里
// 却还挂着对方的申请,点接受回"已是好友",点拒绝又显得莫名其妙,而且 sweep 也清不掉它
// (sweep 只碰终态行)。
func TestAcceptFriend_AlsoResolvesReversePending(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	const a, b uint64 = 26001, 26002
	seedPending(t, ctx, db, a, b)
	seedPending(t, ctx, db, b, a) // 互相申请:两人同时点了"加好友"

	require.NoError(t, repo.AcceptFriend(ctx, a, b, 100))

	assert.Equal(t, int64(2), readRequestStatus(t, ctx, db, a, b), "正向申请必须变 accepted")
	assert.Equal(t, int64(2), readRequestStatus(t, ctx, db, b, a), "反向 pending 必须被一并收敛,不能留孤儿")
	assert.NotZero(t, readRequestUpdatedMs(t, ctx, db, b, a), "反向行的状态迁移同样要写 updated_ms")
	assert.Zero(t, mustCount(t, ctx, db, "SELECT COUNT(*) FROM friend_request WHERE status=1"))

	// 边与计数只能各建一条 / 各加一次:反向 pending 也被处理时最容易出的错是重复加计数。
	assert.Equal(t, int64(1), mustCount(t, ctx, db, "SELECT COUNT(*) FROM friend WHERE player_id=?", a))
	assert.Equal(t, int64(1), mustCount(t, ctx, db, "SELECT COUNT(*) FROM friend WHERE player_id=?", b))
	assertFriendInvariants(t, ctx, db)
}

// TestAcceptFriend_RejectsBlockedPair:拉黑之后,残留在收件箱里的旧申请不得还能被接受。
func TestAcceptFriend_RejectsBlockedPair(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	const a, b uint64 = 27001, 27002
	seedPending(t, ctx, db, a, b)
	seedBlock(t, ctx, db, b, a)

	assert.ErrorIs(t, repo.AcceptFriend(ctx, a, b, 100), ErrBlocked)
	assert.Zero(t, mustCount(t, ctx, db, "SELECT COUNT(*) FROM friend"))
	assertFriendInvariants(t, ctx, db)
}

// TestRemoveFriend_NonFriendDoesNotCreateCapacityRows 是 F2-15 的回归。
//
// 旧写法直接 ensureFriendCapacityRows,于是**任意** target_player_id 每次调用都凭空造出
// 2 行 friend_capacity:客户端拿一个自增计数器循环调 RemoveFriend 就能把这张表无界撑大。
// 它不破坏"有行则行值 == 边数"的一致性,所以除了行数以外没有任何症状 —— 必须有测试钉住。
func TestRemoveFriend_NonFriendDoesNotCreateCapacityRows(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	const me, stranger uint64 = 28001, 28002
	// 幂等语义:删一个本来就不是好友的人不报错(客户端重试、双击都会走到这里)。
	require.NoError(t, repo.RemoveFriend(ctx, me, stranger))
	assert.Zero(t, mustCount(t, ctx, db,
		"SELECT COUNT(*) FROM friend_capacity WHERE player_id IN (?, ?)", me, stranger),
		"对非好友调用 RemoveFriend 不得创建 friend_capacity 行(客户端可无界撑大该表)")

	// 真是好友时照常工作,且计数要减回去。
	lim := defaultTestLimits()
	seedPending(t, ctx, db, me, stranger)
	require.NoError(t, repo.AcceptFriend(ctx, me, stranger, lim.MaxFriends))
	require.Equal(t, int64(1), mustCount(t, ctx, db,
		"SELECT friend_count FROM friend_capacity WHERE player_id=?", me))
	require.NoError(t, repo.RemoveFriend(ctx, me, stranger))
	assert.Zero(t, mustCount(t, ctx, db, "SELECT COUNT(*) FROM friend WHERE player_id=?", me))
	assertFriendInvariants(t, ctx, db)
}

// ── 列表读硬上限(F2-7)────────────────────────────────────────
//
// 规格 §4 要求 GetFriendList / GetPendingRequests / ListBlocks **三条**读路径都加
// LIMIT ListReadHardLimit。ListBlocks 那一半在 block_repo_mysql_test.go;
// 这里补另外两条 —— 漏掉其中一条不会让任何别的用例变红。
//
// ⚠ 这两条走版本化缓存,所以每条用例必须用**独立的 playerID 段**(别复用别的用例的 id):
// 复用会命中上一次回填的快照,限幅断言恒绿。换 repo 也换了 miniredis,两重保险都要有。

func TestGetFriendList_AppliesHardLimit(t *testing.T) {
	db, ctx := openFriendTestDB(t)

	const rows = 7
	const capped uint64 = 39001 // 被截断的那一侧
	const full uint64 = 39002   // 全量返回的那一侧(必须是另一个 id,见上面的缓存说明)
	for _, owner := range []uint64{capped, full} {
		friendIDs := make([]uint64, 0, rows)
		for i := 0; i < rows; i++ {
			friendIDs = append(friendIDs, uint64(39100+i))
		}
		seedFriendEdges(t, ctx, db, owner, friendIDs...)
	}

	const hardLimit uint32 = 5
	tight, _ := newFriendTestRepoWithLimit(t, db, hardLimit)
	got, err := tight.GetFriendList(ctx, capped)
	require.NoError(t, err)
	assert.Len(t, got, int(hardLimit),
		"好友列表的 SQL 必须带 LIMIT ListReadHardLimit:没有它,一个被撑大的列表会一次性拖回整张表")

	wide, _ := newFriendTestRepoWithLimit(t, db, 100)
	all, err := wide.GetFriendList(ctx, full)
	require.NoError(t, err)
	assert.Len(t, all, rows, "上限大于实际行数时必须全部返回(限幅不能变成恒定截断)")
}

func TestGetPendingRequests_AppliesHardLimit(t *testing.T) {
	db, ctx := openFriendTestDB(t)

	const rows = 7
	const capped uint64 = 39003
	const full uint64 = 39004
	for i := 0; i < rows; i++ {
		seedPending(t, ctx, db, uint64(39200+i), capped)
		seedPending(t, ctx, db, uint64(39300+i), full)
	}

	const hardLimit uint32 = 5
	tight, _ := newFriendTestRepoWithLimit(t, db, hardLimit)
	got, err := tight.GetPendingRequests(ctx, capped)
	require.NoError(t, err)
	assert.Len(t, got, int(hardLimit),
		"申请列表的 SQL 同样必须带 LIMIT:入站条数由别人控制,正是最容易被撑大的那条")

	wide, _ := newFriendTestRepoWithLimit(t, db, 100)
	all, err := wide.GetPendingRequests(ctx, full)
	require.NoError(t, err)
	assert.Len(t, all, rows)
}

// ── 缓存(不需要 MySQL) ────────────────────────────────────────

func TestInvalidateCaches_PropagatesRedisFailure(t *testing.T) {
	repo, mr := newCacheOnlyRepo(t)
	mr.Close()

	err := repo.invalidateCaches(context.Background(), "friends:1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalidate friend cache")
}

// TestMissingCapacityRowUsesAuthoritativeFriendCount 钉住 D-10 修订后**仍然有效**的那条不变量:
// friend_capacity 缺行时,初值必须来自 friend 表的权威边数,绝不能直接写 0。
// 猜 0 会让这个玩家的好友硬上限凭空放宽一轮,且全程零报错 —— 所以这条测试与"回填就绪闸"
// (已按 F1 §6.1 退役)无关,不能随闸门一起删。
func TestMissingCapacityRowUsesAuthoritativeFriendCount(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	// 先造出"有 friend 边、但没有 capacity 行"的状态:这正是 ensure 唯一需要算初值的场合。
	const playerID uint64 = 4242
	for _, friendID := range []uint64{5001, 5002, 5003} {
		_, err := db.ExecContext(ctx,
			"INSERT INTO friend (player_id, friend_player_id, since_ms) VALUES (?, ?, 1)",
			playerID, friendID)
		require.NoError(t, err)
	}

	require.NoError(t, repo.ensureFriendCapacityRows(ctx, playerID))

	assert.Equal(t, int64(3), mustCount(t, ctx, db,
		"SELECT friend_count FROM friend_capacity WHERE player_id=?", playerID),
		"缺行必须从 friend 权威边计数，不能初始化为 0")
}

func TestVersionedCache_RejectsStaleFillAfterWriteInvalidation(t *testing.T) {
	repo, mr := newCacheOnlyRepo(t)
	rdb := redis.MustNewRedis(redis.RedisConf{Host: mr.Addr(), Type: "node"})
	ctx := context.Background()
	key := friendListKey(42)

	// reader 在 DB 读取前观察到 generation=0；writer 随后提交并失效缓存。
	require.NoError(t, repo.invalidateCaches(ctx, key))
	// go-zero 的 EvalCtx 吃脚本正文、返回 (any, error);Lua number 经 go-redis 解出来是 int64。
	result, err := rdb.EvalCtx(
		ctx,
		fillFriendCacheScript,
		[]string{friendCacheGenerationKey(key), key},
		"0",
		`[{"friend_player_id":99}]`,
		strconv.FormatInt(time.Minute.Milliseconds(), 10),
	)
	require.NoError(t, err)
	assert.Equal(t, int64(0), result)
	assert.False(t, mr.Exists(key), "写入前读到的旧快照不得在失效之后回填")
}

// TestCacheKeysAreClusterSafe 钉 F2-11:缓存键必须带 hash tag,且键与它的 generation 键
// 落在**同一个** hash tag 里。
//
// 为什么这条值得一个测试:两条 Lua 各自同时操作 `<key>` 与 `<key>:generation`。
// Redis Cluster 按 `{...}` 里的内容算 slot,两个键不同 slot 时 EVAL 直接回 CROSSSLOT ——
// 而本地单库形态下永远不报错,所以这个缺陷只会在 staging/prod 配了 Cluster 之后才炸,
// 且炸的形态是"好友列表永远读不到缓存",不是明显的报错。
func TestCacheKeysAreClusterSafe(t *testing.T) {
	const playerID uint64 = 4242
	for name, key := range map[string]string{
		"friendList":      friendListKey(playerID),
		"pendingRequests": pendingRequestsKey(playerID),
	} {
		t.Run(name, func(t *testing.T) {
			tag := hashTagOf(key)
			require.NotEmpty(t, tag, "键 %q 没有 hash tag:Cluster 下与它的 generation 键会分到不同 slot", key)
			assert.Equal(t, tag, hashTagOf(friendCacheGenerationKey(key)),
				"键与 generation 键的 hash tag 必须一致,否则两条 Lua 在 Cluster 上回 CROSSSLOT")
			assert.Contains(t, tag, strconv.FormatUint(playerID, 10),
				"hash tag 必须含 player_id,否则所有玩家的缓存挤在同一个 slot 上(热点)")
			assert.Contains(t, key, ":v3",
				"键名改了就要抬版本:不抬版本会继续命中旧键名时代写进去的快照")
		})
	}
}

// hashTagOf 取 Redis Cluster 的 hash tag(第一对 `{}` 之间的内容),没有就返回空串。
// 这是 Redis 自己的算法(第一个 `{` 之后、它之后第一个 `}` 之前,且中间非空)。
func hashTagOf(key string) string {
	open := strings.Index(key, "{")
	if open < 0 {
		return ""
	}
	closeIdx := strings.Index(key[open+1:], "}")
	if closeIdx <= 0 {
		return ""
	}
	return key[open+1 : open+1+closeIdx]
}
