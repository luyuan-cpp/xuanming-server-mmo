package reconcile

// pipeline_lock_order_test.go —— 产品调用点(EnqueueEscrowDebit)的语句顺序回归(不连库)。
//
// # 为什么需要它
//
// internal/data 那边的 TestEscrowTransactionLockOrder 是自己按"正确答案"依次调 repo 的两个方法,
// 证的是 data 层各自发什么;而真正会把环装回去的是**调用点**:把 EnsureSeqRows 挪到业务事务里面、
// 或者在业务事务里再补一次建行,data 层的用例一条都不会红。
// 上一轮复审点名的正是这个缺口:"仓库对同类前提已经用源码扫描机械看住,唯独最能把环装回去的
// 这一条只有注释"。
//
// # 判据
//
// 用录制驱动把 EnqueueEscrowDebit 真正发出去的语句按序录下来,断言:
//  1. 建行与分配**分属两个事务**,建行那个先提交(BEGIN…COMMIT 各两段);
//  2. 哨兵守卫行的锁定读只出现在**第一个**事务里,业务事务一次都不碰它
//     —— 这是"消环"论证的核心,业务事务里出现哨兵行就等于把反向边画回来;
//  3. 守卫行的下标严格小于第一条 `SELECT next_seq, epoch`(分配永远排在建行之后);
//  4. outbox 的插入排在分配之后、与分配同一个事务。
//
// 提交后的那次同步投递(ProcessOne)必然失败(Caller 没有 Resolver),但 EnqueueEscrowDebit
// 只记日志不算失败,所以它不影响上面的断言;它会再发一条自动提交的重排 UPDATE,录音带里允许有。

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"trade/internal/data"
)

// ── 录制驱动 ────────────────────────────────────────────────
//
// 这是 internal/data 那份录制驱动的**第二份**,刻意没有抽成公共 testutil:
// 两者的断言面不同(那边逐字比对语句文本与隔离级,这边只关心"哪条语句落在哪个事务里"),
// 而且跨包共享测试夹具要新开一个非测试包,代价比这一百来行重复大。改动 SQL 形状时两边都会红。

type lockOrderScript struct {
	mu sync.Mutex

	// seqRowExists 是被测玩家的 seq 行是否已存在。用例从"不存在"起跑,好走到建行分支。
	seqRowExists bool

	// stmts 是按发出顺序录下的语句(已折叠空白);BEGIN / COMMIT / ROLLBACK 以伪语句入带。
	stmts []string
}

func (s *lockOrderScript) record(stmt string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stmts = append(s.stmts, strings.Join(strings.Fields(stmt), " "))
}

func (s *lockOrderScript) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.stmts...)
}

type lockOrderConnector struct{ s *lockOrderScript }

func (c lockOrderConnector) Connect(context.Context) (driver.Conn, error) {
	return &lockOrderConn{s: c.s}, nil
}
func (c lockOrderConnector) Driver() driver.Driver { return lockOrderDriver{s: c.s} }

type lockOrderDriver struct{ s *lockOrderScript }

func (d lockOrderDriver) Open(string) (driver.Conn, error) { return &lockOrderConn{s: d.s}, nil }

type lockOrderConn struct{ s *lockOrderScript }

func (c *lockOrderConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("录制驱动不支持 Prepare:被测路径全部走 QueryContext / ExecContext")
}
func (c *lockOrderConn) Close() error { return nil }
func (c *lockOrderConn) Begin() (driver.Tx, error) {
	return nil, errors.New("录制驱动只支持 BeginTx")
}

func (c *lockOrderConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.s.record(stmtBegin)
	return lockOrderTx{s: c.s}, nil
}

func (c *lockOrderConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.s.record(query)
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	switch {
	case strings.HasPrefix(query, "SELECT 1 FROM "+data.AssetOpSeqTableName):
		// 哨兵守卫行的存在性:用例里它总是已 bootstrap,所以一律返回一行。
		// 玩家行按 seqRowExists 决定,好让用例从"缺行"起跑。
		if strings.Contains(query, "FOR UPDATE") || c.s.seqRowExists {
			return &lockOrderRows{cols: []string{"1"}, data: [][]driver.Value{{int64(1)}}}, nil
		}
		return &lockOrderRows{cols: []string{"1"}}, nil

	case strings.HasPrefix(query, "SELECT next_seq, epoch FROM "+data.AssetOpSeqTableName):
		if !c.s.seqRowExists {
			return &lockOrderRows{cols: []string{"next_seq", "epoch"}}, nil
		}
		return &lockOrderRows{
			cols: []string{"next_seq", "epoch"},
			data: [][]driver.Value{{int64(1), int64(1_700_000_000_000)}},
		}, nil

	case strings.HasPrefix(query, "SELECT seq FROM "+data.AssetOpTableName):
		return &lockOrderRows{cols: []string{"seq"}}, nil // 零未决行,I5 守卫不拒
	}
	return nil, fmt.Errorf("录制驱动遇到未预期的查询: %s", query)
}

func (c *lockOrderConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.s.record(query)
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	switch {
	case strings.HasPrefix(query, "INSERT IGNORE INTO "+data.AssetOpSeqTableName):
		c.s.seqRowExists = true
		return driver.RowsAffected(1), nil
	case strings.HasPrefix(query, "UPDATE "+data.AssetOpSeqTableName+" SET next_seq"):
		return driver.RowsAffected(1), nil
	case strings.HasPrefix(query, "INSERT INTO "+data.AssetOpTableName):
		return driver.RowsAffected(1), nil
	case strings.HasPrefix(query, "UPDATE "+data.AssetOpTableName):
		// 提交后那次同步投递失败(Caller 没有 Resolver)之后的重排;与本用例的断言无关。
		return driver.RowsAffected(1), nil
	}
	return nil, fmt.Errorf("录制驱动遇到未预期的写语句: %s", query)
}

type lockOrderTx struct{ s *lockOrderScript }

func (t lockOrderTx) Commit() error {
	t.s.record(stmtCommit)
	return nil
}

func (t lockOrderTx) Rollback() error {
	t.s.record(stmtRollback)
	return nil
}

type lockOrderRows struct {
	cols []string
	data [][]driver.Value
	next int
}

func (r *lockOrderRows) Columns() []string { return r.cols }
func (r *lockOrderRows) Close() error      { return nil }
func (r *lockOrderRows) Next(dest []driver.Value) error {
	if r.next >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.next])
	r.next++
	return nil
}

const (
	stmtBegin    = "BEGIN"
	stmtCommit   = "COMMIT"
	stmtRollback = "ROLLBACK"
)

// ── 用例 ────────────────────────────────────────────────────

// TestEnqueueEscrowDebitStatementOrder 钉住产品调用点的端到端语句顺序。
//
// 把 pipeline.go 里的 EnsureSeqRows 挪进 WithTxRetry 的事务体,本用例会在"哨兵行出现在业务事务里"
// 那条断言上稳定变红 —— 那正是上一轮复审指出的、除注释之外没人看着的那个改动。
func TestEnqueueEscrowDebitStatementOrder(t *testing.T) {
	script := &lockOrderScript{}
	db := sql.OpenDB(lockOrderConnector{s: script})
	t.Cleanup(func() { _ = db.Close() })

	repo, err := data.NewAssetOpRepo(db, 5*time.Second)
	if err != nil {
		t.Fatalf("NewAssetOpRepo: %v", err)
	}
	p, err := New(Deps{Ops: repo, Caller: newTestCaller(t), OpIDs: &fakeIDs{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := p.EnqueueEscrowDebit(context.Background(), EscrowRequest{
		SellerPlayerID: 777_001,
		ListingID:      555_001,
		Bundle:         validDebitBundle(),
	}); err != nil {
		// 首次投递失败不算失败(行已在 outbox),所以这里拿到错误说明 1–3 步真的出了问题。
		t.Fatalf("EnqueueEscrowDebit: %v", err)
	}

	got := script.recorded()
	dump := strings.Join(got, "\n")

	guardStmt := "SELECT 1 FROM " + data.AssetOpSeqTableName
	guardAt := indexOfMatch(got, func(s string) bool {
		return strings.HasPrefix(s, guardStmt) && strings.Contains(s, "FOR UPDATE")
	})
	if guardAt < 0 {
		t.Fatalf("没有看到哨兵守卫行的锁定读:缺行时必须先锁它再建行\n%s", dump)
	}
	insertSeqAt := indexOfPrefix(got, "INSERT IGNORE INTO "+data.AssetOpSeqTableName)
	allocAt := indexOfPrefix(got, "SELECT next_seq, epoch FROM "+data.AssetOpSeqTableName)
	insertOpAt := indexOfPrefix(got, "INSERT INTO "+data.AssetOpTableName)

	if insertSeqAt < 0 || allocAt < 0 || insertOpAt < 0 {
		t.Fatalf("建行 / 分配 / 插 outbox 至少缺一条(建行=%d 分配=%d 插行=%d)\n%s",
			insertSeqAt, allocAt, insertOpAt, dump)
	}
	if !(guardAt < insertSeqAt) {
		t.Fatalf("守卫行的锁定读排在建行之后(守卫=%d 建行=%d):那一把锁白拿了\n%s", guardAt, insertSeqAt, dump)
	}
	// 上一轮复审点名要钉的那一条:守卫语句的下标严格小于第一条 SELECT next_seq。
	if !(guardAt < allocAt) {
		t.Fatalf("守卫行的锁定读排在分配之后(守卫=%d 分配=%d):\"持玩家 seq 行、等守卫行\" 与正常路径反序成环\n%s",
			guardAt, allocAt, dump)
	}
	if !(allocAt < insertOpAt) {
		t.Fatalf("插 outbox 行排在分配之前(分配=%d 插行=%d)\n%s", allocAt, insertOpAt, dump)
	}

	// 两段事务:建行那段必须**先提交**,分配与插行落在第二段里。
	begins := indexesOf(got, stmtBegin)
	commits := indexesOf(got, stmtCommit)
	if len(begins) != 2 || len(commits) != 2 {
		t.Fatalf("应当恰好两个事务(建行短事务 + 业务事务),实际 BEGIN=%d COMMIT=%d\n%s",
			len(begins), len(commits), dump)
	}
	shortTxCommit := commits[0]
	businessTxBegin := begins[1]
	if !(guardAt < shortTxCommit && shortTxCommit < businessTxBegin) {
		t.Fatalf("哨兵守卫行没有落在第一个(短)事务里,或短事务没有在业务事务之前提交:"+
			"守卫=%d 短事务提交=%d 业务事务开始=%d\n%s", guardAt, shortTxCommit, businessTxBegin, dump)
	}
	if !(businessTxBegin < allocAt && allocAt < insertOpAt && insertOpAt < commits[1]) {
		t.Fatalf("分配与插 outbox 行没有落在同一个业务事务里:"+
			"业务事务=[%d,%d] 分配=%d 插行=%d\n%s", businessTxBegin, commits[1], allocAt, insertOpAt, dump)
	}

	// 核心断言:业务事务里一次都不许碰哨兵守卫行。
	// 碰了就说明"持业务锁 → 等哨兵行"这条反向边又画得出来了,消环论证当场作废。
	for i := businessTxBegin; i <= commits[1]; i++ {
		if strings.HasPrefix(got[i], guardStmt) && strings.Contains(got[i], "FOR UPDATE") {
			t.Fatalf("业务事务里出现了哨兵守卫行的锁定读(第 %d 条):建行必须留在它自己的短事务里\n%s", i, dump)
		}
		if strings.HasPrefix(got[i], "INSERT IGNORE INTO "+data.AssetOpSeqTableName) {
			t.Fatalf("业务事务里出现了建 seq 行(第 %d 条):业务事务只做普通读确认,不建行\n%s", i, dump)
		}
	}
}

// TestEnqueueEscrowDebitSkipsShortTxWhenSeqRowExists:稳态(行已存在)只开**业务事务**一个,
// 建行那段连 BEGIN 都不发。
//
// 这条守的是短事务化的收益面:如果哪天有人把"无条件开短事务"写回来,全服每一次上架都会多一次
// BEGIN/COMMIT 往返,而所有功能用例仍然全绿。
func TestEnqueueEscrowDebitSkipsShortTxWhenSeqRowExists(t *testing.T) {
	script := &lockOrderScript{seqRowExists: true}
	db := sql.OpenDB(lockOrderConnector{s: script})
	t.Cleanup(func() { _ = db.Close() })

	repo, err := data.NewAssetOpRepo(db, 5*time.Second)
	if err != nil {
		t.Fatalf("NewAssetOpRepo: %v", err)
	}
	p, err := New(Deps{Ops: repo, Caller: newTestCaller(t), OpIDs: &fakeIDs{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.EnqueueEscrowDebit(context.Background(), EscrowRequest{
		SellerPlayerID: 777_002,
		ListingID:      555_002,
		Bundle:         validDebitBundle(),
	}); err != nil {
		t.Fatalf("EnqueueEscrowDebit: %v", err)
	}

	got := script.recorded()
	dump := strings.Join(got, "\n")
	if n := len(indexesOf(got, stmtBegin)); n != 1 {
		t.Fatalf("稳态路径开了 %d 个事务,应为 1 个(seq 行已存在时不该开建行短事务)\n%s", n, dump)
	}
	for _, s := range got {
		if strings.HasPrefix(s, "INSERT IGNORE INTO "+data.AssetOpSeqTableName) {
			t.Fatalf("行已存在却发了建行语句: %s\n%s", s, dump)
		}
		if strings.HasPrefix(s, "SELECT 1 FROM "+data.AssetOpSeqTableName) && strings.Contains(s, "FOR UPDATE") {
			t.Fatalf("行已存在却去锁了哨兵守卫行(稳态多一把全局锁 = 全服上架串行): %s\n%s", s, dump)
		}
	}
}

func indexOfPrefix(stmts []string, prefix string) int {
	return indexOfMatch(stmts, func(s string) bool { return strings.HasPrefix(s, prefix) })
}

func indexOfMatch(stmts []string, pred func(string) bool) int {
	for i, s := range stmts {
		if pred(s) {
			return i
		}
	}
	return -1
}

func indexesOf(stmts []string, want string) []int {
	var out []int
	for i, s := range stmts {
		if s == want {
			out = append(out, i)
		}
	}
	return out
}
