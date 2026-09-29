package data

// asset_op_seq_guard_test.go —— 建 seq 行的守卫锁序回归(不连库)。
//
// 被测对象是 AssetOpRepo.EnsureSeqRows(短事务 A)、AssetOpRepo.AllocateSeqsTx(业务事务 B)
// 以及两者合起来的**取锁顺序**。真库上的死锁证据在 asset_op_seq_guard_integration_test.go
// (带 integration 标签);本文件用录制驱动把"发了哪几条语句、什么顺序、什么形状、在哪个事务里"
// 钉死,这些是纯 SQL 层的事实,不需要 MySQL 就能判红,所以它必须在无 Docker 的环境里也跑得到。
//
// 为什么这两层都要:真库用例会在没有 DSN 时 Skip(Skip 在报告里与通过长得一样,见 friend 的
// 那一个月教训),而本文件永远参与 go test;反过来,本文件看不见 InnoDB 到底取了什么锁,
// 所以"零 1213"只能由真库用例证。

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	assetpb "proto/common/asset"

	"shared/assetop"
)

// ── 录制驱动 ────────────────────────────────────────────────
//
// 只实现被测路径真正会走的入口:BeginTx / QueryContext / ExecContext / Commit / Rollback。
// 其余入口(Prepare、无 ctx 的 Begin)一律报错 —— 真走到了应当一眼看见,而不是悄悄换一条路。

// seqScript 是一次用例的库状态与录音带。
type seqScript struct {
	mu sync.Mutex

	// existing 是已存在的 seq 行,键 = (player_id, stream)。对应"普通读命中"。
	existing map[[2]uint64]bool
	// guardExists 是哨兵守卫行 (0, 0) 是否已 bootstrap。
	guardExists bool
	// nextSeq / epoch 是 AllocateSeq 读到的值;epoch 必须 > 0,否则 assetop 判行损坏。
	nextSeq uint64
	epoch   uint64

	// stmts 是按发出顺序录下的语句(已折叠空白),begin / commit / rollback 以伪语句入带。
	// key 是这条语句前两个实参解出的 (player_id, stream),没有则为 nil —— 顺序断言要靠它。
	stmts []recordedStmt
	// isolation 记下**最后一次** BeginTx 拿到的隔离级;本文件的用例里每个事务都必须是 RC。
	isolation driver.IsolationLevel
	// txCount 是开过的事务数。短事务拆出来之后,"稳态一个事务都不开"成了可断言的事实。
	txCount int
}

// recordedStmt 是录音带上的一条:语句文本 + 它作用在哪个 (player_id, stream) 上。
type recordedStmt struct {
	sql string
	key *SeqKey
}

func newSeqScript() *seqScript {
	return &seqScript{existing: map[[2]uint64]bool{}, nextSeq: 1, epoch: 1_700_000_000_000}
}

func (s *seqScript) record(stmt string) { s.recordKeyed(stmt, nil) }

// recordKeyed 录一条语句,并在能解出实参时把 (player_id, stream) 一并记下。
func (s *seqScript) recordKeyed(stmt string, args []driver.NamedValue) {
	var key *SeqKey
	if player, stream, err := twoUintArgs(args); err == nil {
		key = &SeqKey{PlayerID: player, Stream: assetpb.AssetOpStream(stream)}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stmts = append(s.stmts, recordedStmt{sql: collapseSpaces(stmt), key: key})
}

func (s *seqScript) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.stmts))
	for _, r := range s.stmts {
		out = append(out, r.sql)
	}
	return out
}

// keysOf 返回所有以 prefix 打头的语句作用的 (player_id, stream),保持发出顺序。
func (s *seqScript) keysOf(prefix string) []SeqKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []SeqKey
	for _, r := range s.stmts {
		if r.key != nil && strings.HasPrefix(r.sql, prefix) {
			out = append(out, *r.key)
		}
	}
	return out
}

func (s *seqScript) transactions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.txCount
}

// collapseSpaces 把语句里的连续空白折成单个空格,让断言不受换行与缩进影响。
func collapseSpaces(s string) string { return strings.Join(strings.Fields(s), " ") }

func (s *seqScript) openDB(t *testing.T) *sql.DB {
	t.Helper()
	db := sql.OpenDB(seqFakeConnector{s: s})
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type seqFakeConnector struct{ s *seqScript }

func (c seqFakeConnector) Connect(context.Context) (driver.Conn, error) {
	return &seqFakeConn{s: c.s}, nil
}
func (c seqFakeConnector) Driver() driver.Driver { return seqFakeDriver{s: c.s} }

type seqFakeDriver struct{ s *seqScript }

func (d seqFakeDriver) Open(string) (driver.Conn, error) { return &seqFakeConn{s: d.s}, nil }

type seqFakeConn struct{ s *seqScript }

func (c *seqFakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("录制驱动不支持 Prepare:被测路径全部走 QueryContext / ExecContext")
}
func (c *seqFakeConn) Close() error { return nil }
func (c *seqFakeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("录制驱动只支持 BeginTx:隔离级是本次要断言的契约之一")
}

func (c *seqFakeConn) BeginTx(_ context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.s.mu.Lock()
	c.s.isolation = opts.Isolation
	c.s.txCount++
	c.s.mu.Unlock()
	c.s.record("BEGIN")
	return seqFakeTx{s: c.s}, nil
}

func (c *seqFakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.s.recordKeyed(query, args)
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	switch {
	case strings.HasPrefix(query, "SELECT 1 FROM "+AssetOpSeqTableName):
		// 存在性探针(普通读)与守卫行的锁定点查共用这个前缀,只差尾部的 FOR UPDATE。
		player, stream, err := twoUintArgs(args)
		if err != nil {
			return nil, err
		}
		if player == seqGuardPlayerID && stream == uint64(seqGuardStream) {
			if !c.s.guardExists {
				return &seqFakeRows{cols: []string{"1"}}, nil
			}
			return &seqFakeRows{cols: []string{"1"}, data: [][]driver.Value{{int64(1)}}}, nil
		}
		if !c.s.existing[[2]uint64{player, stream}] {
			return &seqFakeRows{cols: []string{"1"}}, nil
		}
		return &seqFakeRows{cols: []string{"1"}, data: [][]driver.Value{{int64(1)}}}, nil

	case strings.HasPrefix(query, "SELECT next_seq, epoch FROM "+AssetOpSeqTableName):
		player, stream, err := twoUintArgs(args)
		if err != nil {
			return nil, err
		}
		if !c.s.existing[[2]uint64{player, stream}] {
			return &seqFakeRows{cols: []string{"next_seq", "epoch"}}, nil
		}
		return &seqFakeRows{
			cols: []string{"next_seq", "epoch"},
			data: [][]driver.Value{{int64(c.s.nextSeq), int64(c.s.epoch)}},
		}, nil

	case strings.HasPrefix(query, "SELECT seq FROM "+AssetOpTableName):
		// 本纪元未决行:用例都从零未决起跑,守卫不拒。
		return &seqFakeRows{cols: []string{"seq"}}, nil
	}
	return nil, fmt.Errorf("录制驱动遇到未预期的查询: %s", query)
}

func (c *seqFakeConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.s.record(query)
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	switch {
	case strings.HasPrefix(query, "INSERT IGNORE INTO "+AssetOpSeqTableName):
		// assetop 的建行语句形状是 (player_id, stream, next_seq, epoch, updated_ms)。
		if len(args) < 2 {
			return nil, fmt.Errorf("建 seq 行语句参数不足: %d", len(args))
		}
		player, stream, err := twoUintArgs(args[:2])
		if err != nil {
			return nil, err
		}
		if c.s.existing[[2]uint64{player, stream}] {
			return driver.RowsAffected(0), nil // INSERT IGNORE 撞已存在的行 = 空操作
		}
		c.s.existing[[2]uint64{player, stream}] = true
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(query, "UPDATE "+AssetOpSeqTableName+" SET next_seq"):
		c.s.nextSeq++
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(query, "INSERT INTO "+AssetOpTableName):
		return driver.RowsAffected(1), nil
	}
	return nil, fmt.Errorf("录制驱动遇到未预期的写语句: %s", query)
}

// twoUintArgs 取前两个整数实参(player_id, stream)。驱动层拿到的整数一律是 int64。
func twoUintArgs(args []driver.NamedValue) (uint64, uint64, error) {
	if len(args) < 2 {
		return 0, 0, fmt.Errorf("需要 2 个实参,实际 %d 个", len(args))
	}
	out := [2]uint64{}
	for i := 0; i < 2; i++ {
		v, ok := args[i].Value.(int64)
		if !ok {
			return 0, 0, fmt.Errorf("第 %d 个实参不是整数: %#v", i+1, args[i].Value)
		}
		out[i] = uint64(v)
	}
	return out[0], out[1], nil
}

type seqFakeTx struct{ s *seqScript }

func (t seqFakeTx) Commit() error {
	t.s.record("COMMIT")
	return nil
}

func (t seqFakeTx) Rollback() error {
	t.s.record("ROLLBACK")
	return nil
}

type seqFakeRows struct {
	cols []string
	data [][]driver.Value
	next int
}

func (r *seqFakeRows) Columns() []string { return r.cols }
func (r *seqFakeRows) Close() error      { return nil }
func (r *seqFakeRows) Next(dest []driver.Value) error {
	if r.next >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.next])
	r.next++
	return nil
}

// ── 语句形状 ────────────────────────────────────────────────

// TestSeqRowExistsProbeTakesNoLocks:探针 / 确认读必须是**普通读**。
//
// 给它加上锁定子句就等于取消了整个守卫:RC 下未命中的锁定读不取间隙锁、什么都拦不住,
// 而命中时它会在玩家 seq 行上先取一把锁 —— 短事务 A 会因此在拿哨兵行之前先持有玩家 seq 行,
// 与"持哨兵行、等玩家 seq 行"的另一个 A 正好反序成环,正是本次要消掉的那个形状。
func TestSeqRowExistsProbeTakesNoLocks(t *testing.T) {
	q := strings.ToUpper(sqlSeqRowExists)
	for _, clause := range []string{"FOR UPDATE", "FOR SHARE", "LOCK IN SHARE MODE"} {
		if strings.Contains(q, clause) {
			t.Fatalf("seq 行探针不得带 %q(会把守卫锁序反过来,见 asset_op_repo.go 守卫一节): %s",
				clause, sqlSeqRowExists)
		}
	}
}

// TestSeqGuardLockIsPrimaryKeyPointLookup:守卫行的锁定读必须是完整主键等值点查 + FOR UPDATE。
//
// 主键是 (player_id, stream),两列都给等值条件,锁集才**恰好**落在哨兵行上。少一列(或改成
// IN / 范围)会让执行计划退化成扫描,把别的玩家的 seq 行一起锁进来,与 AllocateSeq 跨玩家成环。
// 真库上的 EXPLAIN 断言(key=PRIMARY、key_len=12)在 integration 用例里。
func TestSeqGuardLockIsPrimaryKeyPointLookup(t *testing.T) {
	if want := sqlSeqRowExists + " FOR UPDATE"; sqlLockSeqGuardRow != want {
		t.Fatalf("守卫行锁定读应当只在探针语句尾部加 FOR UPDATE:\n  got  %s\n  want %s", sqlLockSeqGuardRow, want)
	}
	for _, col := range []string{"`player_id` = ?", "`stream` = ?"} {
		if !strings.Contains(sqlLockSeqGuardRow, col) {
			t.Errorf("守卫行锁定读缺主键列等值条件 %s(锁集会越出哨兵行): %s", col, sqlLockSeqGuardRow)
		}
	}
	for _, forbidden := range []string{" IN (", " OR ", " LIKE ", " > ", " < "} {
		if strings.Contains(sqlLockSeqGuardRow, forbidden) {
			t.Errorf("守卫行锁定读出现 %q:必须是完整主键等值点查: %s", forbidden, sqlLockSeqGuardRow)
		}
	}
}

// TestSeqGuardKeyIsNotARealStream:哨兵键不能与任何一条真流撞上,否则守卫行会被当成业务 seq 行
// 分配号(玩家 0 的流水),而业务事务又在锁它 —— 那就是自己跟自己抢。
func TestSeqGuardKeyIsNotARealStream(t *testing.T) {
	if seqGuardPlayerID != 0 {
		t.Errorf("哨兵 player_id = %d,应为 0(真玩家的 PlayerId 由 login 的 snowflake 发,不发 0)", seqGuardPlayerID)
	}
	if seqGuardStream != assetpb.AssetOpStream_ASSET_OP_STREAM_UNSPECIFIED {
		t.Errorf("哨兵 stream = %v,应为 UNSPECIFIED", seqGuardStream)
	}
	for _, s := range []assetpb.AssetOpStream{
		assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT,
		assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_CREDIT,
	} {
		if s == seqGuardStream {
			t.Errorf("哨兵 stream 与 trade 独占的流 %v 撞了", s)
		}
	}
}

// legacyEnsureSeqRowSQL 是 assetop 建 seq 行的语句文本(表名已代入 trade 的)。
//
// 事实源是 shared/assetop 的 ensureSeqRowFormat,**未导出**,所以这里是一份带机械看守的副本:
// 真库红对照(asset_op_seq_guard_integration_test.go)要让这条语句在一个**不提交**的事务里扮演首插者,
// 而 assetop 只提供吃 *sql.DB 的自动提交入口 EnsureSeqRow,拿不到"插了先别提交"的形态,只能写字面量。
// TestLegacyEnsureSeqRowSQLMatchesAssetop 用录制驱动把它与 assetop 真正发出的语句逐字比对,
// assetop 改了语句形状这里必红 —— 副本不会静默分叉。
const legacyEnsureSeqRowSQL = "INSERT IGNORE INTO " + AssetOpSeqTableName +
	" (player_id, stream, next_seq, epoch, updated_ms) VALUES (?, ?, 1, ?, ?)"

// TestLegacyEnsureSeqRowSQLMatchesAssetop:上面那份副本必须与 assetop.EnsureSeqRowTx 实际发出的语句一致。
func TestLegacyEnsureSeqRowSQLMatchesAssetop(t *testing.T) {
	s := newSeqScript()
	s.guardExists = true
	repo := newSeqGuardRepo(t, s)
	tx := beginRC(t, repo)
	if err := assetop.EnsureSeqRowTx(context.Background(), tx, repo.Tables(),
		guardTestPlayer, guardTestStream, guardTestNowMs); err != nil {
		t.Fatalf("assetop.EnsureSeqRowTx: %v", err)
	}
	got := s.recorded()
	if len(got) != 2 || got[1] != legacyEnsureSeqRowSQL {
		t.Fatalf("legacyEnsureSeqRowSQL 与 assetop 的建行语句不一致:\n  got  %v\n  want %s", got, legacyEnsureSeqRowSQL)
	}
}

// TestSeqTableRowsAreNeverDeleted 是本次修法最关键的一条前提:**trade 从不删 seq 行**。
//
// 它顶着两件事:
//   - "首次建行排在一行已提交的守卫行上"只消掉了"首插者回滚"那一种环。一旦有人给 seq 表加了清理 /
//     回收 DELETE,就会出现**已提交的删除标记记录**,并发补行的重复键检查取 S、发现要就地复活、
//     各自升 X —— friend 的场景 (g) 与 trade 收藏的红对照都是这个形状,守卫行拦不住它(守卫只保证
//     同一时刻一个插入者,而这个环只要"插入者 + 一个删行者"两方就能成)。
//   - 2026-09-29 建行挪进独立短事务之后,"短事务提交到业务事务开始之间 seq 行不可能消失"这条论证
//     (asset_op_repo.go 守卫一节第 2 点)直接依赖本用例。行会被删的话,业务事务的确认读会
//     fail-closed 报 ErrSeqRowMissingInTx —— 响亮,但那时整套锁分析都要重做。
//
// 用源码扫描而不是人工约定:下一个 AI 不会记得这两条前提,但它会看到这条红灯。
// 扫描范围是**整个 trade 模块**的非测试源码(见 seqTableSQLLines),与注释里"全仓 / 生产代码"的
// 措辞一致 —— 判据只看一个目录、注释却说全仓,是上一轮复审点名的状态,不要再退回去。
func TestSeqTableRowsAreNeverDeleted(t *testing.T) {
	for file, line := range seqTableSQLLines(t) {
		if strings.Contains(strings.ToUpper(line), "DELETE") {
			t.Errorf("%s 出现了针对 %s 的 DELETE:%s\n"+
				"删 seq 行会造出已提交的删除标记记录,并发补行在它上面 S→X 成环,守卫行拦不住;"+
				"而且会让"短事务提交后行不会消失"这条论证作废 —— 见本用例头注,加清理之前先重做锁分析",
				file, AssetOpSeqTableName, line)
		}
	}
}

// TestNoAggregateQueryOverSeqTable:哨兵行 (0, 0) 混在 seq 表里,任何聚合 / 分组查询都会把它算进去。
// 目前全模块对这张表只有完整主键等值语句(assetop 的点查点改 + 本包的探针 / 确认读 / 守卫锁定读),
// 所以哨兵行不会被误处理;新增聚合时必须显式排除 (0, 0) 并改这条断言,而不是让它悄悄多数一行。
func TestNoAggregateQueryOverSeqTable(t *testing.T) {
	for file, line := range seqTableSQLLines(t) {
		up := strings.ToUpper(line)
		for _, agg := range []string{"COUNT(", "MIN(", "MAX(", "SUM(", "AVG(", "GROUP BY"} {
			if strings.Contains(up, agg) {
				t.Errorf("%s 出现了针对 %s 的聚合 %s:%s\n"+
					"哨兵守卫行 (player_id=0, stream=0) 会被算进去 —— 要么显式排除它,要么改本用例的判据",
					file, AssetOpSeqTableName, agg, line)
			}
		}
	}
}

// tradeModuleRoot 从本文件位置往上找到带 go.mod 的目录,即 go/trade 模块根。
// 用 runtime.Caller 而不是相对路径:go test 的工作目录是包目录,写死 "../.." 的话
// 文件挪个位置就静默扫了个空目录(而扫空会被 seqTableSQLLines 末尾的自检抓住,
// 但直接定位到 go.mod 更不容易出错)。
func tradeModuleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("拿不到本文件路径:无法定位 trade 模块根,两条源码扫描守卫此刻什么都没看住")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("从 %s 往上找不到 go.mod:无法定位 trade 模块根", filepath.Dir(file))
		}
		dir = parent
	}
}

// seqTableSQLLines 返回**整个 trade 模块**非测试源码里"像 SQL 且提到 seq 表"的行,键是 "相对路径:行号"。
//
// 判据刻意宽松(带表名常量 + 一个 SQL 动词),宁可多扫几行也不要漏掉新加的语句;
// 表名常量自己的声明行不含 SQL 动词,不会被扫进来。包外引用写作 data.AssetOpSeqTableName,
// 同样含这个子串,所以 internal/logic、internal/svc 里新加的访问也看得见。
func seqTableSQLLines(t *testing.T) map[string]string {
	t.Helper()
	root := tradeModuleRoot(t)
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// vendor 是别人的代码,.git / 构建产物目录里没有本模块的源码。
			if name := d.Name(); name == "vendor" || name == ".git" || strings.HasPrefix(name, ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		for i, line := range strings.Split(string(body), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || !strings.Contains(trimmed, "AssetOpSeqTableName") {
				continue
			}
			up := strings.ToUpper(trimmed)
			for _, verb := range []string{"SELECT ", "UPDATE ", "DELETE ", "INSERT "} {
				if strings.Contains(up, verb) {
					out[fmt.Sprintf("%s:%d", rel, i+1)] = trimmed
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 trade 模块源码失败: %v", err)
	}
	// 扫不到任何行 = 判据失效(文件改名、常量改名、语句挪走),此时上面两条守卫会静默全绿。
	// 宁可在这里判红让人来改判据,也不要留两条什么都不看的绿灯。
	if len(out) == 0 {
		t.Fatalf("在 %s 的非测试源码里扫不到任何提到 %s 的 SQL 行:判据已失效,"+
			"TestSeqTableRowsAreNeverDeleted 与 TestNoAggregateQueryOverSeqTable 此刻什么都没看住",
			root, AssetOpSeqTableName)
	}
	return out
}

// ── 行为:什么时候锁守卫行 ──────────────────────────────────

// newSeqGuardRepo 建一个挂在录制驱动上的仓库。
func newSeqGuardRepo(t *testing.T, s *seqScript) *AssetOpRepo {
	t.Helper()
	repo, err := NewAssetOpRepo(s.openDB(t), 5*time.Second)
	if err != nil {
		t.Fatalf("NewAssetOpRepo: %v", err)
	}
	return repo
}

// beginRC 开一个 READ COMMITTED 事务(建行 / 分配两条路径的硬契约)。
func beginRC(t *testing.T, repo *AssetOpRepo) *sql.Tx {
	t.Helper()
	tx, err := repo.DB().BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

const guardTestPlayer uint64 = 4242
const guardTestNowMs uint64 = 1_800_000_000_000

var guardTestStream = assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT

// TestEnsureSeqRowsOpensItsOwnShortTransaction 是本文件最要紧的一条:
//
//   - 缺行时:先发一条**自动提交**的普通读探针(没有 BEGIN),确认缺行之后才开事务,
//     事务里是"再探一次 → 守卫行 FOR UPDATE → 建行 → COMMIT"。守卫行排在建行之后 = 白锁一把;
//     排在事务外 = 根本没有守卫。
//   - 行已存在时:**一个事务都不开、一条写语句都不发**。这是稳态路径(每玩家每流只有第一笔会缺行),
//     多开一个事务就是给全服每一次上架加一次无谓的 BEGIN/COMMIT 往返。
//   - 短事务必须是 RC:确认读要看得见前一个建行者已提交的行。
func TestEnsureSeqRowsOpensItsOwnShortTransaction(t *testing.T) {
	key := SeqKey{PlayerID: guardTestPlayer, Stream: guardTestStream}

	t.Run("缺行:自动提交探针 → 短事务(再探 → 守卫行 FOR UPDATE → 建行 → COMMIT)", func(t *testing.T) {
		s := newSeqScript()
		s.guardExists = true
		repo := newSeqGuardRepo(t, s)

		if err := repo.EnsureSeqRows(context.Background(), guardTestNowMs, key); err != nil {
			t.Fatalf("EnsureSeqRows: %v", err)
		}

		want := []string{
			collapseSpaces(sqlSeqRowExists), // 自动提交的前置探针:BEGIN 之前
			"BEGIN",
			collapseSpaces(sqlSeqRowExists), // 事务内再探一次:期间别的副本可能已经建出来了
			collapseSpaces(sqlLockSeqGuardRow),
			legacyEnsureSeqRowSQL,
			"COMMIT",
		}
		assertStmts(t, s.recorded(), want)
		if s.isolation != driver.IsolationLevel(sql.LevelReadCommitted) {
			t.Errorf("短事务隔离级 = %d,契约要求 READ COMMITTED", s.isolation)
		}
		if got := s.transactions(); got != 1 {
			t.Errorf("开了 %d 个事务,应为 1 个", got)
		}
	})

	t.Run("行已存在:只有一条自动提交探针,不开事务、不碰守卫行", func(t *testing.T) {
		s := newSeqScript()
		s.guardExists = true
		s.existing[[2]uint64{key.PlayerID, uint64(key.Stream)}] = true
		repo := newSeqGuardRepo(t, s)

		if err := repo.EnsureSeqRows(context.Background(), guardTestNowMs, key); err != nil {
			t.Fatalf("EnsureSeqRows: %v", err)
		}
		assertStmts(t, s.recorded(), []string{collapseSpaces(sqlSeqRowExists)})
		if got := s.transactions(); got != 0 {
			t.Errorf("稳态路径开了 %d 个事务,应为 0 个(探针命中时连 BEGIN 都不该发)", got)
		}
	})
}

// TestEnsureSeqRowsLocksGuardOnceForSeveralMissingKeys:多个 key 缺行时,守卫行只锁一次,
// 且**所有**建行都在那一把守卫之后、都在同一个短事务里。
//
// 这是"必须一次把全部 key 传进来"那条契约的机械保障:分两次调用 EnsureSeqRows 会开两个短事务,
// 各自锁一次守卫行 —— 虽然不成环(每次都是空手去拿守卫),但会把开服首日的串行点踩两遍;
// 更要紧的是它会让调用方误以为"分次传也行",进而在事务 B 里补第二次建行,那才是真的反序成环。
func TestEnsureSeqRowsLocksGuardOnceForSeveralMissingKeys(t *testing.T) {
	s := newSeqScript()
	s.guardExists = true
	repo := newSeqGuardRepo(t, s)

	keys := []SeqKey{
		{PlayerID: guardTestPlayer, Stream: assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT},
		{PlayerID: guardTestPlayer + 1, Stream: assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_CREDIT},
	}
	if err := repo.EnsureSeqRows(context.Background(), guardTestNowMs, keys...); err != nil {
		t.Fatalf("EnsureSeqRows: %v", err)
	}

	got := s.recorded()
	guardStmt := collapseSpaces(sqlLockSeqGuardRow)
	guardAt, guards, inserts := -1, 0, 0
	for i, stmt := range got {
		switch {
		case stmt == guardStmt:
			guards++
			if guardAt < 0 {
				guardAt = i
			}
		case strings.HasPrefix(stmt, "INSERT IGNORE INTO "+AssetOpSeqTableName):
			inserts++
			if guardAt < 0 {
				t.Fatalf("第 %d 条语句是建行,但守卫行还没锁:\n%s", i, strings.Join(got, "\n"))
			}
		}
	}
	if guards != 1 {
		t.Errorf("守卫行被锁了 %d 次,应为 1 次:\n%s", guards, strings.Join(got, "\n"))
	}
	if inserts != len(keys) {
		t.Errorf("建行 %d 次,应为 %d 次:\n%s", inserts, len(keys), strings.Join(got, "\n"))
	}
	if txs := s.transactions(); txs != 1 {
		t.Errorf("开了 %d 个事务,应为 1 个(全部 key 必须在同一个短事务里建完)", txs)
	}
}

// TestSeqKeysAreProcessedInAscendingOrder:多 key 一律按 (player_id, stream) **升序**处理,
// 建行(事务 A)与分配(事务 B)都是。
//
// 这条钉的是本服务对多把玩家 seq 行锁的确定性全序。哨兵行只串行化"建行",不串行化"分配":
// 行建出之后所有分配都只走普通读 + 玩家行 FOR UPDATE,全程不碰守卫行。P3 的交付要在同一个事务里
// 给买家 CREDIT 和卖家 CREDIT,两笔交易若以相反顺序锁两条都已存在的玩家行就直接成环 ——
// 那时守卫行一次都不会被碰,这条用例是唯一看着它的机制。
//
// 入参刻意乱序 + 带重复,断言:语句里出现的 (player, stream) 严格升序、无重复。
func TestSeqKeysAreProcessedInAscendingOrder(t *testing.T) {
	buyer := SeqKey{PlayerID: guardTestPlayer + 7, Stream: assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_CREDIT}
	seller := SeqKey{PlayerID: guardTestPlayer, Stream: assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT}
	sellerCredit := SeqKey{PlayerID: guardTestPlayer, Stream: assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_CREDIT}
	// 乱序:大 player 在前;同 player 时大 stream 在前;再加一个重复键。
	shuffled := []SeqKey{buyer, sellerCredit, seller, buyer}
	wantOrder := []SeqKey{seller, sellerCredit, buyer} // (P, DEBIT) < (P, CREDIT) < (P+7, CREDIT)

	t.Run("建行(事务 A)按升序 INSERT IGNORE", func(t *testing.T) {
		s := newSeqScript()
		s.guardExists = true
		repo := newSeqGuardRepo(t, s)
		if err := repo.EnsureSeqRows(context.Background(), guardTestNowMs, shuffled...); err != nil {
			t.Fatalf("EnsureSeqRows: %v", err)
		}
		assertKeyOrder(t, s.recorded(), "INSERT IGNORE INTO "+AssetOpSeqTableName, wantOrder)
	})

	t.Run("分配(事务 B)按升序 FOR UPDATE", func(t *testing.T) {
		s := newSeqScript()
		s.guardExists = true
		for _, k := range wantOrder {
			s.existing[[2]uint64{k.PlayerID, uint64(k.Stream)}] = true
		}
		repo := newSeqGuardRepo(t, s)
		tx := beginRC(t, repo)
		allocs, err := repo.AllocateSeqsTx(context.Background(), tx, guardTestNowMs, shuffled...)
		if err != nil {
			t.Fatalf("AllocateSeqsTx: %v", err)
		}
		if len(allocs) != len(wantOrder) {
			t.Fatalf("分配结果 %d 条,应为 %d 条(重复键必须去重)", len(allocs), len(wantOrder))
		}
		assertKeyOrder(t, s.recorded(), "SELECT next_seq, epoch FROM "+AssetOpSeqTableName, wantOrder)
	})
}

// assertKeyOrder 从录音带里挑出所有以 prefix 打头的语句,断言它们携带的 (player, stream)
// 恰好等于 want 这个序列。录制驱动只记语句文本、不记实参,所以这里靠"语句出现的次序 + 每个 key
// 只被处理一次"来判定:先按 prefix 数出条数,再用 script 的实参回放顺序比对。
//
// 实参回放用的是 seqScript 里已经写下的 existing/nextSeq 副作用不够用(它们是无序 map),
// 所以这里改用一份专门的顺序记录:orderedKeys。
func assertKeyOrder(t *testing.T, stmts []string, prefix string, want []SeqKey) {
	t.Helper()
	got := 0
	for _, s := range stmts {
		if strings.HasPrefix(s, prefix) {
			got++
		}
	}
	if got != len(want) {
		t.Fatalf("以 %q 打头的语句有 %d 条,应为 %d 条:\n%s", prefix, got, len(want), strings.Join(stmts, "\n"))
	}
	if !seqKeyOrderRecorder.matches(want) {
		t.Fatalf("处理顺序不是 (player_id, stream) 升序:\n  got  %v\n  want %v",
			seqKeyOrderRecorder.snapshot(), want)
	}
}

// TestEnsureSeqRowsFailsClosedWhenGuardRowMissing:哨兵行不在时必须失败,而且**不许建行**。
//
// 退化成"没有守卫就直接插"会把审计 #16 的环原样装回去,而且只在 bootstrap 失败的机器上复现 ——
// 那是最难查的一类。fail-closed 的代价只是首次上架报错,bootstrap 的 ERROR 日志与
// trade_assetop_seq_guard_bootstrap_failures_total 已经指出补救动作。
func TestEnsureSeqRowsFailsClosedWhenGuardRowMissing(t *testing.T) {
	s := newSeqScript() // guardExists = false
	repo := newSeqGuardRepo(t, s)

	err := repo.EnsureSeqRows(context.Background(), guardTestNowMs,
		SeqKey{PlayerID: guardTestPlayer, Stream: guardTestStream})
	if !errors.Is(err, ErrSeqGuardRowMissing) {
		t.Fatalf("err = %v, want errors.Is(..., ErrSeqGuardRowMissing)", err)
	}
	for _, stmt := range s.recorded() {
		if strings.HasPrefix(stmt, "INSERT IGNORE INTO "+AssetOpSeqTableName) {
			t.Fatalf("守卫行缺失时发出了建行语句(绕过守卫 = 死锁装回去): %s", stmt)
		}
	}
	// 失败必须是**不可重试**的:IsRetryableTxError 认了它的话,WithTxRetry 会围着一个
	// 永远不会自己变好的条件空转,把 5s 预算耗光,错误现场还被最后一次尝试覆盖掉。
	if IsRetryableTxError(err) {
		t.Error("ErrSeqGuardRowMissing 不得被判成可重试错误")
	}
}

// TestSeqRowKeysRejectSentinel:哨兵键不得当业务流用,两个入口都要拒,而且要在发出任何语句之前拒。
// 放行的话,同一个事务会先锁守卫行再把它当玩家 seq 行读写,AllocateSeq 还会推进它的 next_seq ——
// 守卫行从此带着业务语义,任何人想清理它都要先搞懂这段历史。
func TestSeqRowKeysRejectSentinel(t *testing.T) {
	cases := []struct {
		name string
		key  SeqKey
	}{
		{"哨兵整键", SeqKey{PlayerID: seqGuardPlayerID, Stream: seqGuardStream}},
		{"player_id=0", SeqKey{PlayerID: 0, Stream: guardTestStream}},
		{"stream=UNSPECIFIED", SeqKey{PlayerID: guardTestPlayer, Stream: seqGuardStream}},
	}
	for _, tc := range cases {
		t.Run("EnsureSeqRows/"+tc.name, func(t *testing.T) {
			s := newSeqScript()
			s.guardExists = true
			repo := newSeqGuardRepo(t, s)
			if err := repo.EnsureSeqRows(context.Background(), guardTestNowMs, tc.key); err == nil {
				t.Fatal("应被拒绝")
			}
			if got := s.recorded(); len(got) != 0 {
				t.Errorf("非法键必须在发出任何语句之前被拒:\n%s", strings.Join(got, "\n"))
			}
		})
		t.Run("AllocateSeqsTx/"+tc.name, func(t *testing.T) {
			s := newSeqScript()
			s.guardExists = true
			repo := newSeqGuardRepo(t, s)
			tx := beginRC(t, repo)
			if _, err := repo.AllocateSeqsTx(context.Background(), tx, guardTestNowMs, tc.key); err == nil {
				t.Fatal("应被拒绝")
			}
			if got := s.recorded(); len(got) != 1 || got[0] != "BEGIN" {
				t.Errorf("非法键必须在发出任何语句之前被拒:\n%s", strings.Join(got, "\n"))
			}
		})
	}
}

// TestAllocateSeqsTxRequiresTx:nil 事务要当场报错,而不是 panic。
// 这个入口的全部正确性都建立在"跑在调用方的业务事务里"之上,没有事务就没有原子性可言。
func TestAllocateSeqsTxRequiresTx(t *testing.T) {
	repo, err := NewAssetOpRepo(nil, time.Second)
	if err != nil {
		t.Fatalf("NewAssetOpRepo: %v", err)
	}
	if _, err := repo.AllocateSeqsTx(context.Background(), nil, guardTestNowMs,
		SeqKey{PlayerID: guardTestPlayer, Stream: guardTestStream}); err == nil {
		t.Fatal("tx 为 nil 时应返回错误")
	}
}

// TestAllocateSeqsTxFailsClosedWhenRowVanished:业务事务里确认读落空时必须 fail-closed,
// **绝不就地补行**。
//
// 这条守的是 2026-09-29 拆短事务时新增的那条缝:如果这里退化成"读不到就自己建",业务事务就会在
// 已经可能持有业务行锁(P3 的 trade_listing / trade_order)之后去拿哨兵行 —— 那正是被消掉的反向边,
// 而且只在"有人删了 seq 行"这种罕见前提下才复现,几乎不可能被压测撞到。
// 顺带断言"一把 X 都还没拿":确认读全部做完才开始 FOR UPDATE,失败时锁集为空,重试代价最小。
func TestAllocateSeqsTxFailsClosedWhenRowVanished(t *testing.T) {
	s := newSeqScript() // existing 为空:模拟"短事务建过,但行没了"
	s.guardExists = true
	repo := newSeqGuardRepo(t, s)
	tx := beginRC(t, repo)

	_, err := repo.AllocateSeqsTx(context.Background(), tx, guardTestNowMs,
		SeqKey{PlayerID: guardTestPlayer, Stream: guardTestStream})
	if !errors.Is(err, ErrSeqRowMissingInTx) {
		t.Fatalf("err = %v, want errors.Is(..., ErrSeqRowMissingInTx)", err)
	}
	for _, stmt := range s.recorded() {
		if strings.HasPrefix(stmt, "INSERT IGNORE INTO "+AssetOpSeqTableName) {
			t.Fatalf("业务事务里就地补行了(把守卫的锁序反了过来): %s", stmt)
		}
		if stmt == collapseSpaces(sqlLockSeqGuardRow) {
			t.Fatalf("业务事务里去锁了哨兵守卫行:全序要求哨兵行只在短事务 A 里取: %s", stmt)
		}
		if strings.HasPrefix(stmt, "SELECT next_seq, epoch FROM "+AssetOpSeqTableName) {
			t.Fatalf("确认读落空之后仍然去取了玩家 seq 行的 X:应当在一把锁都没拿之前就失败: %s", stmt)
		}
	}
	if IsRetryableTxError(err) {
		t.Error("ErrSeqRowMissingInTx 不得被判成可重试错误:行不会自己回来,重试只是空转")
	}
}

// TestBusinessTxNeverTouchesGuardRow:业务事务(事务 B)一次都不许提到哨兵键。
//
// 这是短事务化之后"消环"论证的**核心**:哨兵行只在事务 A 里取,而 A 取它的时候手上没有任何别的锁。
// 只要事务 B 里出现哨兵行,那条"持业务锁 → 等哨兵行"的反向边就立刻画得出来。
func TestBusinessTxNeverTouchesGuardRow(t *testing.T) {
	s := newSeqScript()
	s.guardExists = true
	key := SeqKey{PlayerID: guardTestPlayer, Stream: guardTestStream}
	s.existing[[2]uint64{key.PlayerID, uint64(key.Stream)}] = true
	repo := newSeqGuardRepo(t, s)
	tx := beginRC(t, repo)

	if _, err := repo.AllocateSeqsTx(context.Background(), tx, guardTestNowMs, key); err != nil {
		t.Fatalf("AllocateSeqsTx: %v", err)
	}
	guardStmt := collapseSpaces(sqlLockSeqGuardRow)
	for _, stmt := range s.recorded() {
		if stmt == guardStmt {
			t.Fatalf("业务事务里出现了哨兵守卫行的锁定读:\n%s", strings.Join(s.recorded(), "\n"))
		}
	}
}

// TestEscrowTransactionLockOrder 钉住托管两段事务的**完整取锁顺序**:
//
//	[事务 A] 自动提交探针 → BEGIN → 探针 → 守卫行 FOR UPDATE → 建 seq 行 → COMMIT
//	[事务 B] BEGIN → 确认读 → 玩家 seq 行 FOR UPDATE → 未决行普通读 → 推进 next_seq → 插 outbox 行
//
// 这正是 asset_op_repo.go 守卫一节写下的全序在 seq 与 outbox 两张表上的那一段。
// reconcile.Pipeline.EnqueueEscrowDebit 是产品调用点,它的端到端语句顺序由
// internal/reconcile/pipeline_lock_order_test.go 单独看住 —— 本用例只证 data 层这两个方法
// 各自发什么、按什么顺序发。
//
// 顺序一旦反过来(先 AllocateSeqsTx 再 EnsureSeqRows),缺行时确认读落空、上架必失败(响亮);
// 而"在事务 B 里去拿守卫行"则会与正常路径反序成环 —— 只在并发下偶发,由上面两条用例分别盯住。
func TestEscrowTransactionLockOrder(t *testing.T) {
	s := newSeqScript()
	s.guardExists = true
	repo := newSeqGuardRepo(t, s)
	ctx := context.Background()
	key := SeqKey{PlayerID: guardTestPlayer, Stream: guardTestStream}

	if err := repo.EnsureSeqRows(ctx, guardTestNowMs, key); err != nil {
		t.Fatalf("EnsureSeqRows: %v", err)
	}
	guardAt := idxOfPrefix(s.recorded(), collapseSpaces(sqlLockSeqGuardRow))
	commitAt := idxOfPrefix(s.recorded(), "COMMIT")
	if guardAt < 0 || commitAt < 0 || commitAt < guardAt {
		t.Fatalf("短事务 A 必须在拿过守卫行之后提交:\n%s", strings.Join(s.recorded(), "\n"))
	}
	aStmts := len(s.recorded())

	tx := beginRC(t, repo)
	allocs, err := repo.AllocateSeqsTx(ctx, tx, guardTestNowMs, key)
	if err != nil {
		t.Fatalf("AllocateSeqsTx: %v", err)
	}
	if allocs[key].Epoch != s.epoch {
		t.Errorf("纪元 = %d,应为 %d", allocs[key].Epoch, s.epoch)
	}

	got := s.recorded()
	// 只断言这几个锚点按序出现,不逐字比对 assetop 的语句文本:那几条的事实源在 shared/assetop,
	// 抄进来就成了第二份真相,assetop 改一个空格这里就假红。
	anchors := []struct {
		what   string
		prefix string
	}{
		{"守卫行 FOR UPDATE(事务 A)", collapseSpaces(sqlLockSeqGuardRow)},
		{"建 seq 行(事务 A)", "INSERT IGNORE INTO " + AssetOpSeqTableName},
		{"事务 A 提交", "COMMIT"},
		{"确认读(事务 B)", collapseSpaces(sqlSeqRowExists)},
		{"玩家 seq 行 FOR UPDATE(事务 B)", "SELECT next_seq, epoch FROM " + AssetOpSeqTableName},
		{"未决行普通读", "SELECT seq FROM " + AssetOpTableName},
		{"推进 next_seq", "UPDATE " + AssetOpSeqTableName + " SET next_seq"},
	}
	at := 0
	for _, a := range anchors {
		found := -1
		for i := at; i < len(got); i++ {
			if strings.HasPrefix(got[i], a.prefix) {
				found = i
				break
			}
		}
		if found < 0 {
			t.Fatalf("取锁顺序里找不到「%s」(或它排在前一个锚点之前):\n%s", a.what, strings.Join(got, "\n"))
		}
		at = found + 1
	}
	// 玩家 seq 行的锁定读必须落在事务 B 里(即短事务 A 的全部语句之后)。
	if idxOfPrefix(got, "SELECT next_seq, epoch FROM "+AssetOpSeqTableName) < aStmts {
		t.Fatalf("玩家 seq 行的锁定读发生在短事务 A 里:分配必须在业务事务 B 中做\n%s", strings.Join(got, "\n"))
	}
}

func idxOfPrefix(stmts []string, prefix string) int {
	for i, s := range stmts {
		if strings.HasPrefix(s, prefix) {
			return i
		}
	}
	return -1
}

// assertStmts 逐条比对录音带。
func assertStmts(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("发了 %d 条语句,应为 %d 条:\n  got:\n    %s\n  want:\n    %s",
			len(got), len(want), strings.Join(got, "\n    "), strings.Join(want, "\n    "))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 条语句不符:\n  got  %s\n  want %s", i, got[i], want[i])
		}
	}
}
