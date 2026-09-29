package data

// asset_op_seq_guard_test.go —— 建 seq 行的守卫锁序回归(不连库)。
//
// 被测对象是 AssetOpRepo.EnsureSeqRowsTx 与它和 assetop.AllocateSeq 合起来的**取锁顺序**。
// 真库上的死锁证据在 asset_op_seq_guard_integration_test.go(带 integration 标签);
// 本文件用录制驱动把"发了哪几条语句、什么顺序、什么形状"钉死,这些是纯 SQL 层的事实,
// 不需要 MySQL 就能判红,所以它必须在无 Docker 的环境里也跑得到。
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
	stmts []string
	// isolation 记下 BeginTx 拿到的隔离级,用来钉住"调用方事务必须 RC"这条契约。
	isolation driver.IsolationLevel
}

func newSeqScript() *seqScript {
	return &seqScript{existing: map[[2]uint64]bool{}, nextSeq: 1, epoch: 1_700_000_000_000}
}

func (s *seqScript) record(stmt string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stmts = append(s.stmts, collapseSpaces(stmt))
}

func (s *seqScript) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.stmts...)
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
	c.s.mu.Unlock()
	c.s.record("BEGIN")
	return seqFakeTx{s: c.s}, nil
}

func (c *seqFakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.s.record(query)
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

// TestSeqRowExistsProbeTakesNoLocks:前置探针必须是**普通读**。
//
// 给它加上锁定子句就等于取消了整个守卫:RC 下未命中的锁定读不取间隙锁、什么都拦不住,
// 而命中时它会在玩家 seq 行上先取一把锁 —— 于是出现"持玩家 seq 行、等守卫行"的事务,
// 与"持守卫行、等玩家 seq 行"的事务反序成环,正是本次要消掉的那个形状。
func TestSeqRowExistsProbeTakesNoLocks(t *testing.T) {
	q := strings.ToUpper(sqlSeqRowExists)
	for _, clause := range []string{"FOR UPDATE", "FOR SHARE", "LOCK IN SHARE MODE"} {
		if strings.Contains(q, clause) {
			t.Fatalf("EnsureSeqRowsTx 的前置探针不得带 %q(会把守卫锁序反过来,见 asset_op_repo.go 守卫一节): %s",
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
// "首次建行排在一行已提交的守卫行上"只消掉了"首插者回滚"那一种环。一旦有人给 seq 表加了清理 /
// 回收 DELETE,就会出现**已提交的删除标记记录**,并发补行的重复键检查取 S、发现要就地复活、各自升 X ——
// friend 的场景 (g) 与 trade 收藏的红对照都是这个形状,守卫行拦不住它(守卫只保证同一时刻一个插入者,
// 而这个环只要"插入者 + 一个删行者"两方就能成)。真加清理的话必须先重做锁分析,不是加个重试了事。
//
// 用源码扫描而不是人工约定:下一个 AI 不会记得这条前提,但它会看到这条红灯。
func TestSeqTableRowsAreNeverDeleted(t *testing.T) {
	for file, line := range seqTableSQLLines(t) {
		if strings.Contains(strings.ToUpper(line), "DELETE") {
			t.Errorf("%s 出现了针对 %s 的 DELETE:%s\n"+
				"删 seq 行会造出已提交的删除标记记录,并发补行在它上面 S→X 成环,守卫行拦不住 —— "+
				"见本用例头注,加清理之前先重做锁分析", file, AssetOpSeqTableName, line)
		}
	}
}

// TestNoAggregateQueryOverSeqTable:哨兵行 (0, 0) 混在 seq 表里,任何聚合 / 分组查询都会把它算进去。
// 目前全仓对这张表只有完整主键等值语句(assetop 的点查点改 + 本包的探针),所以哨兵行不会被误处理;
// 新增聚合时必须显式排除 (0, 0) 并改这条断言,而不是让它悄悄多数一行。
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

// seqTableSQLLines 返回本包非测试源码里**像 SQL 且提到 seq 表**的行,键是文件名。
// 判据刻意宽松(带表名常量 + 一个 SQL 动词),宁可多扫几行也不要漏掉新加的语句;
// 表名常量自己的声明行不含 SQL 动词,不会被扫进来。
func seqTableSQLLines(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读包目录失败: %v", err)
	}
	out := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", name, err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || !strings.Contains(trimmed, "AssetOpSeqTableName") {
				continue
			}
			up := strings.ToUpper(trimmed)
			hasVerb := false
			for _, verb := range []string{"SELECT ", "UPDATE ", "DELETE ", "INSERT "} {
				if strings.Contains(up, verb) {
					hasVerb = true
					break
				}
			}
			if hasVerb {
				out[fmt.Sprintf("%s:%d", name, i+1)] = trimmed
			}
		}
	}
	// 扫不到任何行 = 判据失效(文件改名、常量改名、语句挪走),此时上面两条守卫会静默全绿。
	// 宁可在这里判红让人来改判据,也不要留两条什么都不看的绿灯。
	if len(out) == 0 {
		t.Fatalf("在本包非测试源码里扫不到任何提到 %s 的 SQL 行:判据已失效,"+
			"TestSeqTableRowsAreNeverDeleted 与 TestNoAggregateQueryOverSeqTable 此刻什么都没看住", AssetOpSeqTableName)
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

// beginRC 开一个 READ COMMITTED 事务(EnsureSeqRowsTx 的硬契约)。
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

// TestEnsureSeqRowsTxLocksGuardOnlyWhenRowMissing 是本文件最要紧的一条:
//
//   - 缺行时的顺序必须是"普通读 → 守卫行 FOR UPDATE → 建行",一条不能少、顺序不能换。
//     守卫行排在建行之后 = 白锁一把(插入已经发生了);排在普通读之前 = 每一次上架都去抢那一行,
//     把稳态吞吐串行化。
//   - 行已存在时**一次都不许**碰守卫行:那是稳态路径(每玩家每流只有第一笔会缺行),
//     多一把全局锁就是把全服上架串起来。
func TestEnsureSeqRowsTxLocksGuardOnlyWhenRowMissing(t *testing.T) {
	key := SeqKey{PlayerID: guardTestPlayer, Stream: guardTestStream}

	t.Run("缺行:普通读 → 守卫行 FOR UPDATE → 建行", func(t *testing.T) {
		s := newSeqScript()
		s.guardExists = true
		repo := newSeqGuardRepo(t, s)
		tx := beginRC(t, repo)

		if err := repo.EnsureSeqRowsTx(context.Background(), tx, guardTestNowMs, key); err != nil {
			t.Fatalf("EnsureSeqRowsTx: %v", err)
		}

		want := []string{
			"BEGIN",
			collapseSpaces(sqlSeqRowExists),
			collapseSpaces(sqlLockSeqGuardRow),
			legacyEnsureSeqRowSQL,
		}
		assertStmts(t, s.recorded(), want)
		if s.isolation != driver.IsolationLevel(sql.LevelReadCommitted) {
			t.Errorf("事务隔离级 = %d,契约要求 READ COMMITTED", s.isolation)
		}
	})

	t.Run("行已存在:只有普通读,不碰守卫行", func(t *testing.T) {
		s := newSeqScript()
		s.guardExists = true
		s.existing[[2]uint64{key.PlayerID, uint64(key.Stream)}] = true
		repo := newSeqGuardRepo(t, s)
		tx := beginRC(t, repo)

		if err := repo.EnsureSeqRowsTx(context.Background(), tx, guardTestNowMs, key); err != nil {
			t.Fatalf("EnsureSeqRowsTx: %v", err)
		}
		assertStmts(t, s.recorded(), []string{"BEGIN", collapseSpaces(sqlSeqRowExists)})
	})
}

// TestEnsureSeqRowsTxLocksGuardOnceForSeveralMissingKeys:多个 key 缺行时,守卫行只锁一次,
// 且**所有**建行都在那一把守卫之后。
//
// 这是"必须一次把全部 key 传进来"那条契约的机械保障:分两次调用的话,第二次的守卫行请求会发生在
// 第一次的 AllocateSeq 已经拿到玩家 seq 行 X 锁之后 —— 于是出现"持玩家 seq 行、等守卫行"的事务,
// 与"持守卫行、等玩家 seq 行"的事务反序成环。P3 的交付(买家 + 卖家两条 CREDIT)正是两个 key。
func TestEnsureSeqRowsTxLocksGuardOnceForSeveralMissingKeys(t *testing.T) {
	s := newSeqScript()
	s.guardExists = true
	repo := newSeqGuardRepo(t, s)
	tx := beginRC(t, repo)

	keys := []SeqKey{
		{PlayerID: guardTestPlayer, Stream: assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT},
		{PlayerID: guardTestPlayer + 1, Stream: assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_CREDIT},
	}
	if err := repo.EnsureSeqRowsTx(context.Background(), tx, guardTestNowMs, keys...); err != nil {
		t.Fatalf("EnsureSeqRowsTx: %v", err)
	}

	got := s.recorded()
	guardStmt := collapseSpaces(sqlLockSeqGuardRow)
	guardAt, guards := -1, 0
	inserts := 0
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
}

// TestEnsureSeqRowsTxFailsClosedWhenGuardRowMissing:哨兵行不在时必须失败,而且**不许建行**。
//
// 退化成"没有守卫就直接插"会把审计 #16 的环原样装回去,而且只在 bootstrap 失败的机器上复现 ——
// 那是最难查的一类。fail-closed 的代价只是首次上架报错,bootstrap 的 ERROR 日志已经指出补救动作。
func TestEnsureSeqRowsTxFailsClosedWhenGuardRowMissing(t *testing.T) {
	s := newSeqScript() // guardExists = false
	repo := newSeqGuardRepo(t, s)
	tx := beginRC(t, repo)

	err := repo.EnsureSeqRowsTx(context.Background(), tx, guardTestNowMs,
		SeqKey{PlayerID: guardTestPlayer, Stream: guardTestStream})
	if !errors.Is(err, ErrSeqGuardRowMissing) {
		t.Fatalf("err = %v, want errors.Is(..., ErrSeqGuardRowMissing)", err)
	}
	for _, stmt := range s.recorded() {
		if strings.HasPrefix(stmt, "INSERT IGNORE INTO "+AssetOpSeqTableName) {
			t.Fatalf("守卫行缺失时发出了建行语句(绕过守卫 = 死锁装回去): %s", stmt)
		}
	}
}

// TestEnsureSeqRowsTxRejectsSentinelKey:哨兵键不得当业务流用。
// 放行的话,同一个事务会先锁守卫行再把它当玩家 seq 行读写,AllocateSeq 还会推进它的 next_seq ——
// 守卫行从此带着业务语义,任何人想清理它都要先搞懂这段历史。
func TestEnsureSeqRowsTxRejectsSentinelKey(t *testing.T) {
	cases := []struct {
		name string
		key  SeqKey
	}{
		{"哨兵整键", SeqKey{PlayerID: seqGuardPlayerID, Stream: seqGuardStream}},
		{"player_id=0", SeqKey{PlayerID: 0, Stream: guardTestStream}},
		{"stream=UNSPECIFIED", SeqKey{PlayerID: guardTestPlayer, Stream: seqGuardStream}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newSeqScript()
			s.guardExists = true
			repo := newSeqGuardRepo(t, s)
			tx := beginRC(t, repo)
			if err := repo.EnsureSeqRowsTx(context.Background(), tx, guardTestNowMs, tc.key); err == nil {
				t.Fatal("应被拒绝")
			}
			if got := s.recorded(); len(got) != 1 || got[0] != "BEGIN" {
				t.Errorf("非法键必须在发出任何语句之前被拒:\n%s", strings.Join(got, "\n"))
			}
		})
	}
}

// TestEnsureSeqRowsTxRequiresTx:nil 事务要当场报错,而不是 panic。
// 这个入口的全部正确性都建立在"跑在调用方的业务事务里、守卫锁在手"之上,没有事务就没有守卫。
func TestEnsureSeqRowsTxRequiresTx(t *testing.T) {
	repo, err := NewAssetOpRepo(nil, time.Second)
	if err != nil {
		t.Fatalf("NewAssetOpRepo: %v", err)
	}
	if err := repo.EnsureSeqRowsTx(context.Background(), nil, guardTestNowMs,
		SeqKey{PlayerID: guardTestPlayer, Stream: guardTestStream}); err == nil {
		t.Fatal("tx 为 nil 时应返回错误")
	}
}

// TestEscrowTransactionLockOrder 钉住托管事务的**完整取锁顺序**:
//
//	守卫行 FOR UPDATE → 建 seq 行 → 玩家 seq 行 FOR UPDATE → 未决行普通读 → 推进 next_seq → 插 outbox 行
//
// 这正是 asset_op_repo.go 守卫一节写下的全序在 seq 与 outbox 两张表上的那一段,也是
// reconcile.Pipeline.EnqueueEscrowDebit 必须照着调的顺序(它在另一个包,本用例按同样的顺序
// 直接调 repo + assetop,等于把那个顺序的"正确答案"钉在这里;端到端顺序由 integration 用例覆盖)。
//
// 顺序一旦反过来(先 AllocateSeq 再补行),缺行时 AllocateSeq 直接回 ErrSeqRowMissing、上架必失败;
// 而"先拿玩家 seq 行再去拿守卫行"则会与正常路径反序成环 —— 前者响亮,后者只在并发下偶发。
func TestEscrowTransactionLockOrder(t *testing.T) {
	s := newSeqScript()
	s.guardExists = true
	repo := newSeqGuardRepo(t, s)
	tx := beginRC(t, repo)
	ctx := context.Background()
	key := SeqKey{PlayerID: guardTestPlayer, Stream: guardTestStream}

	if err := repo.EnsureSeqRowsTx(ctx, tx, guardTestNowMs, key); err != nil {
		t.Fatalf("EnsureSeqRowsTx: %v", err)
	}
	alloc, err := assetop.AllocateSeq(ctx, tx, repo.Tables(), key.PlayerID, key.Stream, assetop.DefaultLimits, guardTestNowMs)
	if err != nil {
		t.Fatalf("AllocateSeq: %v", err)
	}
	if alloc.Epoch != s.epoch {
		t.Errorf("纪元 = %d,应为 %d", alloc.Epoch, s.epoch)
	}

	got := s.recorded()
	// 只断言这几个锚点按序出现,不逐字比对 assetop 的语句文本:那几条的事实源在 shared/assetop,
	// 抄进来就成了第二份真相,assetop 改一个空格这里就假红。
	anchors := []struct {
		what   string
		prefix string
	}{
		{"守卫行 FOR UPDATE", collapseSpaces(sqlLockSeqGuardRow)},
		{"建 seq 行", "INSERT IGNORE INTO " + AssetOpSeqTableName},
		{"玩家 seq 行 FOR UPDATE", "SELECT next_seq, epoch FROM " + AssetOpSeqTableName},
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
	// 玩家 seq 行的锁定读必须**晚于**守卫行:反过来就是本次要消掉的那个环。
	if idxOfPrefix(got, "SELECT next_seq, epoch FROM "+AssetOpSeqTableName) <
		idxOfPrefix(got, collapseSpaces(sqlLockSeqGuardRow)) {
		t.Fatalf("玩家 seq 行的锁定读排在守卫行之前:\n%s", strings.Join(got, "\n"))
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
