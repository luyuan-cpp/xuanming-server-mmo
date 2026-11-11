package data

// 请求截止之后不再提交(docs/design/client-request-ordering.md §6.1 / O11 / §14 第 4 条)的回归测试(不连库)。
//
// 挂牌(listing)这一侧客户端请求路径上只有收藏两处写(SetFavorite → InsertFavorite / DeleteFavorite);
// 合同(commerce:寄售、拍卖、材料挂单、面对面交易、人民币寄售)的客户端写见 commerce/request_deadline_fence_test.go。gate 对同一会话的
// 路由类请求单飞,路由服回超时就放行下一条:截止之后才落库的「收藏」会晚于紧随其后的「取消收藏」,
// 玩家看到取消成功、商品却仍在收藏里。
//
// 用录制驱动把「有没有语句到达驱动」钉死:database/sql 取连接前先看 ctx,两条收藏写的
// assetop.WithTxRetry 每次尝试前也先看 ctx —— 两者任一被改成不受请求 ctx 约束,这里就判红。
// 另钉住「语句已经发出、执行中跑过截止」时停在事务里、不发 COMMIT(DeleteFavorite 原先自动提交,做不到这一点)。
// 带 integration 标签的真库用例不在默认 go test 里跑,所以这条必须不依赖 MySQL。

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	tradepb "proto/trade"
)

// deadlineRecorder 录下每条到达驱动的语句。默认(零值)一律报错;事务用例(newTxRecorder)打开 BEGIN,
// 并把 COMMIT / ROLLBACK 也录进来,用来证明"语句已经发出、执行中跑过了截止"时不提交。
type deadlineRecorder struct {
	mu    sync.Mutex
	stmts []string
	// exec 为 nil 时写语句一律报错;非 nil 时由它决定写语句的结果(参数是去掉多余空白的语句)。
	exec func(query string) (driver.Result, error)
	// beginOK 为 false 时 BEGIN 一律报错(截止栅栏用例的默认)。
	beginOK bool
	// isolation 是最近一次 BEGIN 请求的隔离级别。
	isolation driver.IsolationLevel
	// txEnded 在事务结束时收到 "COMMIT" 或 "ROLLBACK"。ctx 结束后的回滚由 database/sql 的后台协程执行,
	// 可能晚于被测方法返回,所以用例等这个驱动事件,而不是在方法返回后立即断言回滚。
	txEnded chan string
}

func newTxRecorder() *deadlineRecorder {
	return &deadlineRecorder{beginOK: true, txEnded: make(chan string, 4)}
}

func (r *deadlineRecorder) record(stmt string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stmts = append(r.stmts, strings.Join(strings.Fields(stmt), " "))
}

func (r *deadlineRecorder) beginIsolation() driver.IsolationLevel {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.isolation
}

func (r *deadlineRecorder) endTx(stmt string) {
	r.record(stmt)
	r.txEnded <- stmt
}

// awaitTxEnd 等事务结束。上限只用于事务永不结束(回归)时及早判红、不把 go test 挂到超时,正常路径不依赖它。
func (r *deadlineRecorder) awaitTxEnd(t *testing.T) string {
	t.Helper()
	select {
	case stmt := <-r.txEnded:
		return stmt
	case <-time.After(10 * time.Second):
		t.Fatal("事务既没有提交也没有回滚")
		return ""
	}
}

func (r *deadlineRecorder) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.stmts...)
}

var errDeadlineDriver = errors.New("截止栅栏录制驱动:这条语句不该到达驱动")

type deadlineConnector struct{ r *deadlineRecorder }

func (c deadlineConnector) Connect(context.Context) (driver.Conn, error) {
	return deadlineConn{r: c.r}, nil
}
func (c deadlineConnector) Driver() driver.Driver { return deadlineDriver{r: c.r} }

type deadlineDriver struct{ r *deadlineRecorder }

func (d deadlineDriver) Open(string) (driver.Conn, error) { return deadlineConn{r: d.r}, nil }

// deadlineConn 录下每条到达驱动的语句(含 BEGIN)。默认一律报错:截止之后本不该有任何语句到达这里;
// 事务用例由 deadlineRecorder 的 beginOK / exec 放行。
type deadlineConn struct{ r *deadlineRecorder }

func (c deadlineConn) Prepare(query string) (driver.Stmt, error) {
	c.r.record(query)
	return nil, errDeadlineDriver
}
func (c deadlineConn) Close() error { return nil }
func (c deadlineConn) Begin() (driver.Tx, error) {
	c.r.record("BEGIN")
	return nil, errDeadlineDriver
}
func (c deadlineConn) BeginTx(_ context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.r.record("BEGIN")
	if !c.r.beginOK {
		return nil, errDeadlineDriver
	}
	c.r.mu.Lock()
	c.r.isolation = opts.Isolation
	c.r.mu.Unlock()
	return deadlineTx{r: c.r}, nil
}
func (c deadlineConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.r.record(query)
	if c.r.exec != nil {
		return c.r.exec(strings.Join(strings.Fields(query), " "))
	}
	return nil, errDeadlineDriver
}

func (c deadlineConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.r.record(query)
	return nil, errDeadlineDriver
}

type deadlineTx struct{ r *deadlineRecorder }

func (t deadlineTx) Commit() error   { t.r.endTx("COMMIT"); return nil }
func (t deadlineTx) Rollback() error { t.r.endTx("ROLLBACK"); return nil }

func TestFavoriteWritesAfterRequestDeadlineSendNoStatement(t *testing.T) {
	cases := []struct {
		name string
		call func(ctx context.Context, repo *ListingRepo) error
	}{
		{"InsertFavorite", func(ctx context.Context, repo *ListingRepo) error {
			return repo.InsertFavorite(ctx, &tradepb.TradeFavoriteRecord{PlayerId: 1002, ListingId: 11, CreatedMs: 1})
		}},
		{"DeleteFavorite", func(ctx context.Context, repo *ListingRepo) error {
			return repo.DeleteFavorite(ctx, 1002, 11)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &deadlineRecorder{}
			db := sql.OpenDB(deadlineConnector{r: rec})
			t.Cleanup(func() { _ = db.Close() })
			// 单次上限给足:用例要证明的是「请求 ctx 已截止」挡住了写,不是 opTimeout。
			repo := NewListingRepo(db, time.Minute)
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // 已经截止的请求 ctx(go-zero 超时拦截器到期后 handler 拿到的就是这种 ctx)

			err := tc.call(ctx, repo)

			if !errors.Is(err, context.Canceled) {
				t.Fatalf("截止之后必须如实报错(logic 定性 kServiceUnavailable),实际 err=%v", err)
			}
			if got := rec.recorded(); len(got) != 0 {
				t.Fatalf("截止之后不得有任何语句到达驱动(含 BEGIN),实际发出 %v", got)
			}
		})
	}
}

// favoriteWriteCases 是收藏的两条写:每条都必须是 execFavoriteWriteTx 里的单语句事务。
var favoriteWriteCases = []struct {
	name string
	sql  string
	call func(ctx context.Context, repo *ListingRepo) error
}{
	{"InsertFavorite", insertFavoriteSQL, func(ctx context.Context, repo *ListingRepo) error {
		return repo.InsertFavorite(ctx, &tradepb.TradeFavoriteRecord{PlayerId: 1002, ListingId: 11, CreatedMs: 1})
	}},
	{"DeleteFavorite", deleteFavoriteSQL, func(ctx context.Context, repo *ListingRepo) error {
		return repo.DeleteFavorite(ctx, 1002, 11)
	}},
}

// 截止落在收藏写语句执行期间(典型是这条语句正排在这一行的锁后面)。
//
// 自动提交时 go-sql-driver 在 ctx 结束时只断开连接、不发 KILL QUERY,服务端拿到锁后照样执行并提交,晚于 gate
// 放行的下一条落地(DeleteFavorite 原先就是这样)。单语句事务里截止之后不得发 COMMIT,事务以回滚收场。
// 两种驱动表现都覆盖:语句执行中 ctx 结束、驱动断开连接回 ctx 错误(go-sql-driver 的实际行为);
// 或语句恰好在截止那一刻执行完、驱动照常回结果。
// 退回自动提交(r.db.ExecContext)时,录到的第一条不是 BEGIN、也不会有 ROLLBACK,用例确定判红。
// 不发 COMMIT 并不只靠事务体末尾那道显式 ctx 检查:tx.ctx 已结束时 database/sql 的 Tx.Commit 自己也不发 COMMIT。
// 那道检查管的是错误语义(错误链里必定是 ctx 错误,而非后台回滚抢先后的 sql.ErrTxDone);只去掉它时,
// "语句恰在截止时执行完"子用例的 errors.Is 能否判红取决于后台回滚是否抢先,是概率性的,不当作它的确定回归。
func TestFavoriteWriteRunningPastDeadlineDoesNotCommit(t *testing.T) {
	results := []struct {
		name   string
		result func() (driver.Result, error)
	}{
		{"驱动在截止时中断语句并回ctx错误", func() (driver.Result, error) { return nil, context.Canceled }},
		{"语句恰在截止时执行完", func() (driver.Result, error) { return driver.RowsAffected(1), nil }},
	}
	for _, write := range favoriteWriteCases {
		for _, res := range results {
			t.Run(write.name+"/"+res.name, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				rec := newTxRecorder()
				rec.exec = func(query string) (driver.Result, error) {
					if query != write.sql {
						return nil, errDeadlineDriver
					}
					cancel() // 请求截止发生在这条写语句执行期间
					return res.result()
				}
				db := sql.OpenDB(deadlineConnector{r: rec})
				t.Cleanup(func() { _ = db.Close() })
				// 单次上限给足:用例要证明的是请求 ctx 截止挡住了提交,不是 opTimeout。
				repo := NewListingRepo(db, time.Minute)

				err := write.call(ctx, repo)

				if !errors.Is(err, context.Canceled) {
					t.Fatalf("截止之后必须如实报错(logic 定性 kServiceUnavailable),实际 err=%v", err)
				}
				// BEGIN 与写语句在方法返回之前同步录下;COMMIT 只可能由被测方法自己同步发出,所以此刻没有就永远没有。
				sent := rec.recorded()
				if len(sent) < 2 || sent[0] != "BEGIN" || sent[1] != write.sql {
					t.Fatalf("写语句必须在显式事务里(BEGIN 在前),不能自动提交;实际发出 %v", sent)
				}
				for _, stmt := range sent {
					if stmt == "COMMIT" {
						t.Fatalf("截止之后不得提交,实际发出 %v", sent)
					}
				}
				if ended := rec.awaitTxEnd(t); ended != "ROLLBACK" {
					t.Fatalf("跑过截止的事务必须回滚,实际以 %s 结束", ended)
				}
				// ctx 错误不可重试:只有一次尝试,不得重开事务。
				if got := rec.recorded(); len(got) != 3 || got[2] != "ROLLBACK" {
					t.Fatalf("期望恰好 BEGIN → 写语句 → ROLLBACK,实际 %v", got)
				}
				if iso := rec.beginIsolation(); iso != driver.IsolationLevel(sql.LevelReadCommitted) {
					t.Fatalf("收藏写事务的隔离级别必须是 RC(WithTxRetry 默认),实际 %v", iso)
				}
			})
		}
	}
}

// 正常路径:截止之前跑完,恰好一次 BEGIN → 写语句 → COMMIT,不回滚、不重试。
func TestFavoriteWriteWithinDeadlineCommitsOnce(t *testing.T) {
	for _, write := range favoriteWriteCases {
		t.Run(write.name, func(t *testing.T) {
			rec := newTxRecorder()
			rec.exec = func(query string) (driver.Result, error) {
				if query != write.sql {
					return nil, errDeadlineDriver
				}
				return driver.RowsAffected(1), nil
			}
			db := sql.OpenDB(deadlineConnector{r: rec})
			t.Cleanup(func() { _ = db.Close() })
			repo := NewListingRepo(db, time.Minute)

			if err := write.call(context.Background(), repo); err != nil {
				t.Fatalf("截止之前的收藏写必须成功,实际 err=%v", err)
			}

			if ended := rec.awaitTxEnd(t); ended != "COMMIT" {
				t.Fatalf("期望提交,实际以 %s 结束", ended)
			}
			if got := rec.recorded(); len(got) != 3 || got[0] != "BEGIN" || got[1] != write.sql || got[2] != "COMMIT" {
				t.Fatalf("期望恰好 BEGIN → 写语句 → COMMIT,实际 %v", got)
			}
			if iso := rec.beginIsolation(); iso != driver.IsolationLevel(sql.LevelReadCommitted) {
				t.Fatalf("收藏写事务的隔离级别必须是 RC(WithTxRetry 默认),实际 %v", iso)
			}
		})
	}
}
