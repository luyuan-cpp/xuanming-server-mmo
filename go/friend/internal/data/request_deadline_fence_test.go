package data

// 请求截止之后不再提交(docs/design/client-request-ordering.md §6.1 / O11 / §14 第 4 条)的回归测试。
//
// gate 对同一会话的路由类请求单飞:路由服在 ForwardTimeoutMs 回超时,gate 就放行下一条。friend 的 handler
// 若在截止之后仍把写发出去,这条写就会晚于下一条请求落地,例如「同意好友 → 删好友」执行成「删好友 → 同意好友」,
// 两条都回成功、最终仍是好友(§1.3)。
//
// 本文件不连库、不受 FRIEND_TEST_MYSQL_DSN 门控:用录制驱动把「有没有语句到达驱动」钉死。
// 这是纯 SQL 层的事实 —— database/sql 在取连接(db.conn)与 Tx.Commit 时先看 ctx,ctx 已结束就不发任何语句 ——
// 所以它能抓住的回归正是「写路径上某一处改用了 context.Background() / WithoutCancel」。
// 真库上的锁语义与提交结果仍由同目录的 *_mysql_test.go 证明;这里只证明截止之后一条语句都不会发出,
// 以及"语句已经发出、执行中跑过截止"的写(Unblock)停在事务里、不发 COMMIT。

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
)

// ── 录制驱动 ────────────────────────────────────────────────
//
// 每条到达驱动的语句(含 BEGIN)都录下来。respond 为 nil 或对该语句返回 nil 时一律报错:
// 截止之后本不该有任何语句到达驱动,真到了要一眼看见,而不是悄悄"成功"。
//
// 事务用例(newTxRecorder)另外打开 BEGIN,并把 COMMIT / ROLLBACK 也录进来:它们要证明的是
// "语句已经发出、执行中跑过了截止"时不提交,这需要驱动真的开出一个事务。

type deadlineRecorder struct {
	mu      sync.Mutex
	stmts   []string
	respond func(query string) driver.Rows
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

func (r *deadlineRecorder) openDB(t *testing.T) *sql.DB {
	t.Helper()
	db := sql.OpenDB(deadlineConnector{r: r})
	t.Cleanup(func() { _ = db.Close() })
	return db
}

var errDeadlineDriver = errors.New("截止栅栏录制驱动:这条语句不该到达驱动")

type deadlineConnector struct{ r *deadlineRecorder }

func (c deadlineConnector) Connect(context.Context) (driver.Conn, error) {
	return deadlineConn{r: c.r}, nil
}
func (c deadlineConnector) Driver() driver.Driver { return deadlineDriver{r: c.r} }

type deadlineDriver struct{ r *deadlineRecorder }

func (d deadlineDriver) Open(string) (driver.Conn, error) { return deadlineConn{r: d.r}, nil }

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
	if c.r.respond != nil {
		if rows := c.r.respond(strings.Join(strings.Fields(query), " ")); rows != nil {
			return rows, nil
		}
	}
	return nil, errDeadlineDriver
}

type deadlineTx struct{ r *deadlineRecorder }

func (t deadlineTx) Commit() error   { t.r.endTx("COMMIT"); return nil }
func (t deadlineTx) Rollback() error { t.r.endTx("ROLLBACK"); return nil }

// oneRowThenCancel 回一行结果,读到末尾(io.EOF)时调用 cancel:模拟请求恰好在这次预读完成之后截止。
// 取消放在 EOF 而不是首行:首行之前取消会让 database/sql 直接以 ctx 错误关掉结果集,预读本身就失败了,
// 用例就测不到「预读成功之后、写之前」这一段。
type oneRowThenCancel struct {
	cols   []string
	row    []driver.Value
	sent   bool
	cancel context.CancelFunc
}

func (r *oneRowThenCancel) Columns() []string { return r.cols }
func (r *oneRowThenCancel) Close() error      { return nil }
func (r *oneRowThenCancel) Next(dest []driver.Value) error {
	if r.sent {
		r.cancel()
		return io.EOF
	}
	r.sent = true
	copy(dest, r.row)
	return nil
}

func newDeadlineRepo(t *testing.T, rec *deadlineRecorder) (*FriendRepo, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := newControlTestRedis(t, mr)
	return NewFriendRepo(rdb, rec.openDB(t), testFriendRepoOptions(testListReadHardLimit)), mr
}

// canceledRequestCtx 是一个已经截止的请求 ctx(go-zero 超时拦截器到期、或 friend 自己的整请求预算到期后,
// handler 手里拿到的就是这种 ctx)。
func canceledRequestCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// 客户端请求路径上的全部写入口(logic 层逐个把请求 ctx 原样传进来,见 logic/friend_logic.go)。
func TestWritesAfterRequestDeadlineSendNoStatement(t *testing.T) {
	const me, target uint64 = 41001, 41002
	cases := []struct {
		name string
		call func(ctx context.Context, repo *FriendRepo) error
	}{
		{"AddFriend(CreateRequest)", func(ctx context.Context, repo *FriendRepo) error {
			_, err := repo.CreateRequest(ctx, me, target, AddFriendLimits{MaxFriends: 100, MaxPendingRequests: 100, MaxIncomingRequests: 100})
			return err
		}},
		{"AcceptFriend(AcceptRequest)", func(ctx context.Context, repo *FriendRepo) error {
			_, err := repo.AcceptRequest(ctx, 777, me, 100)
			return err
		}},
		{"RejectFriend(RejectRequest)", func(ctx context.Context, repo *FriendRepo) error {
			_, err := repo.RejectRequest(ctx, 777, me)
			return err
		}},
		{"RemoveFriend", func(ctx context.Context, repo *FriendRepo) error {
			_, err := repo.RemoveFriend(ctx, me, target)
			return err
		}},
		{"Block", func(ctx context.Context, repo *FriendRepo) error {
			_, err := repo.Block(ctx, me, target, 100)
			return err
		}},
		{"Unblock", func(ctx context.Context, repo *FriendRepo) error {
			return repo.Unblock(ctx, me, target)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &deadlineRecorder{}
			repo, mr := newDeadlineRepo(t, rec)

			err := tc.call(canceledRequestCtx(), repo)

			require.ErrorIs(t, err, context.Canceled, "截止之后必须如实报错(logic 定性 ErrStorage),不能回成功")
			require.Empty(t, rec.recorded(), "截止之后不得有任何语句到达驱动(含容量行补齐与 BEGIN)")
			require.Empty(t, mr.Keys(), "没有提交就不得失效缓存")
		})
	}
}

// 截止恰好落在「按申请代次定位」之后:AcceptRequest 已经读到申请行,下一步是补齐容量行、开写事务建边。
// 这一刻之后不得再发任何语句,否则建边会晚于 gate 放行的下一条 RemoveFriend 落地。
func TestAcceptRequestDeadlineAfterLookupOpensNoWriteTransaction(t *testing.T) {
	const from, accepter, requestID uint64 = 42001, 42002, 4242
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &deadlineRecorder{}
	rec.respond = func(query string) driver.Rows {
		if !strings.HasPrefix(query, "SELECT from_player_id, to_player_id FROM friend_request WHERE request_id=") {
			return nil
		}
		return &oneRowThenCancel{
			cols:   []string{"from_player_id", "to_player_id"},
			row:    []driver.Value{int64(from), int64(accepter)},
			cancel: cancel,
		}
	}
	repo, mr := newDeadlineRepo(t, rec)

	_, err := repo.AcceptRequest(ctx, requestID, accepter, 100)

	require.ErrorIs(t, err, context.Canceled)
	stmts := rec.recorded()
	require.Len(t, stmts, 1, "预读之后截止:不得补齐容量行、不得开写事务,实际发出 %v", stmts)
	require.True(t, strings.HasPrefix(stmts[0], "SELECT from_player_id, to_player_id FROM friend_request"), stmts[0])
	require.Empty(t, mr.Keys(), "没有提交就不得失效缓存")
}

// 截止落在 Unblock 的 DELETE 执行期间(典型是这条 DELETE 正排在这一行的锁后面)。
//
// 这条 DELETE 原先自动提交:go-sql-driver 在 ctx 结束时只断开连接、不发 KILL QUERY,服务端拿到锁后照样执行并提交,
// 晚于 gate 放行的下一条落地。现在它在单语句事务里,截止之后不得发 COMMIT,事务以回滚收场。
// 两种驱动表现都覆盖:语句执行中 ctx 结束、驱动断开连接回 ctx 错误(go-sql-driver 的实际行为);
// 或语句恰好在截止那一刻执行完、驱动照常回结果。
// 去掉事务(退回 r.db.ExecContext)时,录到的第一条不是 BEGIN、也不会有 ROLLBACK,用例确定判红。
// 不发 COMMIT 并不只靠提交前那道显式 ctx 检查:tx.ctx 已结束时 database/sql 的 Tx.Commit 自己也不发 COMMIT。
// 那道检查管的是错误语义(错误链里必定是 ctx 错误,而非后台回滚抢先后的 sql.ErrTxDone);只去掉它时,
// "语句恰在截止时执行完"子用例的 ErrorIs 能否判红取决于后台回滚是否抢先,是概率性的,不当作它的确定回归。
func TestUnblockStatementRunningPastDeadlineDoesNotCommit(t *testing.T) {
	const me, target uint64 = 43001, 43002
	cases := []struct {
		name   string
		result func() (driver.Result, error)
	}{
		{"驱动在截止时中断语句并回ctx错误", func() (driver.Result, error) { return nil, context.Canceled }},
		{"语句恰在截止时执行完", func() (driver.Result, error) { return driver.RowsAffected(1), nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rec := newTxRecorder()
			rec.exec = func(query string) (driver.Result, error) {
				if query != unblockSQL {
					return nil, errDeadlineDriver
				}
				cancel() // 请求截止发生在这条 DELETE 执行期间
				return tc.result()
			}
			repo, _ := newDeadlineRepo(t, rec)

			err := repo.Unblock(ctx, me, target)

			require.ErrorIs(t, err, context.Canceled, "截止之后必须如实报错(logic 定性 ErrStorage),不能回成功")
			// BEGIN 与 DELETE 在方法返回之前同步录下;COMMIT 只可能由被测方法自己同步发出,所以此刻没有就永远没有。
			sent := rec.recorded()
			require.GreaterOrEqual(t, len(sent), 2, "实际发出 %v", sent)
			require.Equal(t, []string{"BEGIN", unblockSQL}, sent[:2], "DELETE 必须在显式事务里,不能自动提交")
			require.NotContains(t, sent, "COMMIT", "截止之后不得提交")
			require.Equal(t, "ROLLBACK", rec.awaitTxEnd(t), "跑过截止的事务必须回滚")
			require.Equal(t, []string{"BEGIN", unblockSQL, "ROLLBACK"}, rec.recorded())
			require.Equal(t, driver.IsolationLevel(sql.LevelReadCommitted), rec.beginIsolation(),
				"Unblock 的事务隔离级别必须是 RC(friendWriteTxIsolation)")
		})
	}
}

// 正常路径:截止之前跑完,恰好一次 BEGIN → DELETE → COMMIT,不回滚、不重发。
func TestUnblockWithinDeadlineCommitsOnce(t *testing.T) {
	const me, target uint64 = 44001, 44002
	rec := newTxRecorder()
	rec.exec = func(query string) (driver.Result, error) {
		if query != unblockSQL {
			return nil, errDeadlineDriver
		}
		return driver.RowsAffected(1), nil
	}
	repo, mr := newDeadlineRepo(t, rec)

	require.NoError(t, repo.Unblock(context.Background(), me, target))

	require.Equal(t, "COMMIT", rec.awaitTxEnd(t))
	require.Equal(t, []string{"BEGIN", unblockSQL, "COMMIT"}, rec.recorded())
	require.Equal(t, driver.IsolationLevel(sql.LevelReadCommitted), rec.beginIsolation())
	require.Empty(t, mr.Keys(), "黑名单不进缓存,Unblock 不碰 Redis")
}
