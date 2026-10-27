package main

// 只验证原件选择键、强读失败及事务提交边界；此假驱动不证明TiDB实际JOIN或锁语义。
import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"shared/drainproof"
)

func normalFreezeOptions() options {
	return options{routeSource: routeSourceDB, routeDBDSN: "fixture@tcp(127.0.0.1:4000)/fixture", maintenanceRunID: strings.Repeat("1", 32), freezeTicketID: strings.Repeat("2", 32), freezeRequestID: strings.Repeat("3", 32), freezeReceiptSHA256: strings.Repeat("4", 64), frozenWriterEpoch: 7}
}

func TestNormalFreezeRequiresCompleteImmutableSelectors(t *testing.T) {
	if _, err := parseNormalFreezeProof(normalFreezeOptions()); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*options){
		func(o *options) { o.routeSource = routeSourceRedis }, func(o *options) { o.routeDBDSN = "" }, func(o *options) { o.assumeKafkaDrained = true },
		func(o *options) { o.maintenanceRunID = "" }, func(o *options) { o.freezeTicketID = strings.Repeat("0", 32) }, func(o *options) { o.freezeRequestID = strings.Repeat("A", 32) },
		func(o *options) { o.freezeReceiptSHA256 = strings.Repeat("4", 63) }, func(o *options) { o.frozenWriterEpoch = 0 },
	} {
		o := normalFreezeOptions()
		mutate(&o)
		if _, err := parseNormalFreezeProof(o); !errors.Is(err, errNormalFreezeProof) {
			t.Fatalf("不完整原件选择键必须拒绝: %v", err)
		}
	}
}

type freezeProofConnector struct {
	f     *fakeRouteSQL
	reply fakeRouteSQLReply
}

func (c freezeProofConnector) Driver() driver.Driver { return fakeRouteSQLDriver{} }
func (c freezeProofConnector) Connect(context.Context) (driver.Conn, error) {
	return &freezeProofConn{fakeRouteSQLConn: &fakeRouteSQLConn{f: c.f}, reply: c.reply}, nil
}

type freezeProofConn struct {
	*fakeRouteSQLConn
	reply fakeRouteSQLReply
}

func (c *freezeProofConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if q == "SELECT writable FROM `fixture`.`route_meta` WHERE id = 1 FOR UPDATE" {
		return c.fakeRouteSQLConn.QueryContext(ctx, routeLockProbeGuardSQL, args)
	}
	if q != normalFrozenProofSQLForSchema("fixture") {
		return c.fakeRouteSQLConn.QueryContext(ctx, q, args)
	}
	values := make([]driver.Value, len(args))
	for i, a := range args {
		values[i] = a.Value
	}
	c.f.mu.Lock()
	c.f.calls = append(c.f.calls, fakeRouteSQLCall{label: "NORMAL_FREEZE", query: q, args: values})
	c.f.mu.Unlock()
	if c.reply.err != nil {
		return nil, c.reply.err
	}
	return &fakeRouteSQLRows{cols: 1, rows: append([][]driver.Value(nil), c.reply.rows...)}, nil
}

func freezeProofDB(t *testing.T, reply fakeRouteSQLReply, script routeScript) (*sql.DB, *fakeRouteSQL) {
	t.Helper()
	f := &fakeRouteSQL{replies: script}
	db := sql.OpenDB(freezeProofConnector{f: f, reply: reply})
	t.Cleanup(func() { _ = db.Close() })
	return db, f
}

func TestNormalFreezeStrongReadBindsAllOriginalSelectors(t *testing.T) {
	p, err := parseNormalFreezeProof(normalFreezeOptions())
	if err != nil {
		t.Fatal(err)
	}
	db, f := freezeProofDB(t, rsOne(), routeScript{})
	if err = p.check(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	calls := f.callsOf("NORMAL_FREEZE")
	want := []driver.Value{uint64(7), p.runID, p.ticketID, p.requestID, p.receiptDigest}
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].args, want) {
		t.Fatalf("强读必须使用原选择键: %+v", calls)
	}
	for _, reply := range []fakeRouteSQLReply{rsNone(), rsRows([]driver.Value{int64(0)}), rsRows([]driver.Value{int64(2)}), rsErr(errors.New("未知查询结果"))} {
		bad, _ := freezeProofDB(t, reply, routeScript{})
		if err = p.check(context.Background(), bad); !errors.Is(err, errNormalFreezeProof) {
			t.Fatalf("不存在/非唯一/UNKNOWN必须HOLD: %v", err)
		}
	}
}

func TestMaintenanceRouteTransactionLocksMetaBeforeActualWrite(t *testing.T) {
	p, _ := parseNormalFreezeProof(normalFreezeOptions())
	ctx, _ := testMaintenanceContext()
	ctx = withNormalFreezeProof(ctx, p)
	for _, valid := range []bool{true, false} {
		t.Run(map[bool]string{true: "真实结局", false: "结局不匹配"}[valid], func(t *testing.T) {
			count := int64(0)
			script := routeScript{}.on("GUARD", rsRows([]driver.Value{int64(0)}))
			if valid {
				count = 1
				script = script.on("GUARD", rsRows([]driver.Value{int64(0)}), rsRows([]driver.Value{int64(0)})).on("MOVE_HOME", rsAffected(1))
			}
			db, f := freezeProofDB(t, rsRows([]driver.Value{count}), script)
			st := &mergeRouteStore{db: db, sleep: sleepCtx, backoff: routeLockRetryBackoff}
			err := st.runTx(ctx, "维护写", func(tx *sql.Tx) error {
				if err := requireRouteWritable(ctx, tx); err != nil {
					return err
				}
				_, err := tx.ExecContext(ctx, routeMoveHomeSQL)
				return err
			})
			if valid {
				if err != nil {
					t.Fatal(err)
				}
				assertRouteLabels(t, f, "BEGIN", "GUARD", "NORMAL_FREEZE", "GUARD", "NORMAL_FREEZE", "MOVE_HOME", "COMMIT")
			} else {
				if !errors.Is(err, errNormalFreezeProof) {
					t.Fatal(err)
				}
				assertRouteLabels(t, f, "BEGIN", "GUARD", "NORMAL_FREEZE", "ROLLBACK")
			}
		})
	}
	db, f := freezeProofDB(t, rsOne(), routeScript{})
	if err := requireRouteWritable(ctx, db); !errors.Is(err, errNormalFreezeProof) {
		t.Fatalf("事务外预检不能当提交锁: %v", err)
	}
	if len(f.labels()) != 0 {
		t.Fatal("事务外入口应在SQL副作用前拒绝")
	}
}

// 只替换SQL资格读取接缝，不把普通冻结原件当TOOL；生产只由Load/BindMaintenance构造。
func testMaintenanceContext() (context.Context, *maintenanceTool) {
	t := &maintenanceTool{session: drainproof.New(), check: func(context.Context) error { return nil }}
	ctx := context.WithValue(context.Background(), maintenanceContextKey{}, t)
	t.context = ctx
	return ctx, t
}

func TestMaintenanceRedisAcceptedUntilNativeReturns(t *testing.T) {
	_, owner := testMaintenanceContext()
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	hook := maintenanceRedisHook{tool: owner}.ProcessHook(func(context.Context, redis.Cmder) error { close(entered); <-release; return nil })
	go func() { done <- hook(context.Background(), redis.NewCmd(context.Background(), "SET", "k", "v")) }()
	<-entered
	owner.session.CloseAdmission()
	if owner.session.Pending() != 1 {
		t.Fatal("native未返回不能settle")
	}
	calls := 0
	rejected := maintenanceRedisHook{tool: owner}.ProcessHook(func(context.Context, redis.Cmder) error { calls++; return nil })
	if err := rejected(context.Background(), redis.NewCmd(context.Background(), "SET", "k", "later")); !errors.Is(err, drainproof.ErrClosed) || calls != 0 {
		t.Fatalf("Close后必须零native拒绝: %v %d", err, calls)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := owner.session.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestMaintenanceRedisUnknownCannotBeWashedByLaterSuccess(t *testing.T) {
	for _, kind := range []string{"error", "timeout", "panic", "pipeline-command"} {
		t.Run(kind, func(t *testing.T) {
			_, owner := testMaintenanceContext()
			cmd := redis.NewCmd(context.Background(), "SET", "k", "v")
			errNative := errors.New("发送后未确认")
			if kind == "pipeline-command" {
				hook := maintenanceRedisHook{tool: owner}.ProcessPipelineHook(func(context.Context, []redis.Cmder) error { cmd.SetErr(errNative); return nil })
				_ = hook(context.Background(), []redis.Cmder{cmd})
			} else {
				hook := maintenanceRedisHook{tool: owner}.ProcessHook(func(context.Context, redis.Cmder) error {
					switch kind {
					case "panic":
						panic("native panic")
					case "timeout":
						return context.DeadlineExceeded
					}
					return errNative
				})
				func() {
					defer func() {
						value := recover()
						if kind == "panic" && value == nil {
							t.Error("panic不能吞掉")
						}
						if kind != "panic" && value != nil {
							t.Error(value)
						}
					}()
					_ = hook(context.Background(), cmd)
				}()
			}
			ok := maintenanceRedisHook{tool: owner}.ProcessHook(func(context.Context, redis.Cmder) error { return nil })
			if err := ok(context.Background(), redis.NewCmd(context.Background(), "PING")); err != nil {
				t.Fatal(err)
			}
			owner.session.CloseAdmission()
			if owner.session.Pending() != 0 || !errors.Is(owner.session.Wait(context.Background()), drainproof.ErrHold) {
				t.Fatal("已返回只代表join，UNKNOWN必须永久HOLD")
			}
		})
	}
}

func TestMaintenanceRedisKnownNilAndBackgroundContext(t *testing.T) {
	_, owner := testMaintenanceContext()
	hook := maintenanceRedisHook{tool: owner}.ProcessHook(func(context.Context, redis.Cmder) error { return redis.Nil })
	if err := hook(context.Background(), redis.NewCmd(context.Background(), "GET", "missing")); !errors.Is(err, redis.Nil) {
		t.Fatal(err)
	}
	owner.session.CloseAdmission()
	if err := owner.session.Wait(context.Background()); err != nil {
		t.Fatalf("已知缺席不是UNKNOWN: %v", err)
	}
}

type maintenanceTestCloser struct {
	mu    sync.Mutex
	count int
	err   error
}

func (c *maintenanceTestCloser) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.count++
	return c.err
}
func TestMaintenanceCloseErrorPreservedAfterRealJoin(t *testing.T) {
	ctx, owner := testMaintenanceContext()
	closeErr := errors.New("关闭结果未知")
	resource := &maintenanceTestCloser{err: closeErr}
	if err := registerMaintenanceResource(ctx, resource); err != nil {
		t.Fatal(err)
	}
	if err := registerMaintenanceResource(ctx, resource); err != nil {
		t.Fatal(err)
	}
	if err := owner.finish(ctx); !errors.Is(err, closeErr) || !errors.Is(err, drainproof.ErrHold) {
		t.Fatal(err)
	}
	if err := owner.finish(ctx); !errors.Is(err, closeErr) || resource.count != 1 {
		t.Fatalf("首次Close错误不可重试洗白: %v count=%d", err, resource.count)
	}
	if _, err := owner.session.Enter(); !errors.Is(err, drainproof.ErrClosed) {
		t.Fatal("闭合后不得接受派生命令")
	}
}
func TestMaintenanceStopPanicCannotBeReenteredAsJoined(t *testing.T) {
	ctx, owner := testMaintenanceContext()
	resource := &maintenanceTestCloser{}
	if err := registerMaintenanceResource(ctx, resource); err != nil {
		t.Fatal(err)
	}
	if err := registerMaintenanceStop(ctx, func(context.Context) error { panic("未join") }); err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic不得吞掉")
			}
		}()
		_ = owner.finish(ctx)
	}()
	if err := owner.finish(ctx); !errors.Is(err, drainproof.ErrHold) || resource.count != 0 {
		t.Fatalf("panic不能缓存成join成功: %v count=%d", err, resource.count)
	}
}
func TestMaintenanceFinishRequiresRealJoinBeforeResourceClose(t *testing.T) {
	ctx, owner := testMaintenanceContext()
	resource := &maintenanceTestCloser{}
	if err := registerMaintenanceResource(ctx, resource); err != nil {
		t.Fatal(err)
	}
	if err := registerMaintenanceResource(ctx, resource); err != nil {
		t.Fatal(err)
	}
	finish, err := enterMaintenanceWork(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	if err := registerMaintenanceStop(ctx, func(wait context.Context) error {
		select {
		case <-done:
			return nil
		case <-wait.Done():
			return wait.Err()
		}
	}); err != nil {
		t.Fatal(err)
	}
	wait, cancel := context.WithCancel(ctx)
	cancel()
	if err := owner.finish(wait); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if resource.count != 0 || owner.session.Pending() != 1 {
		t.Fatal("超时/取消不能伪造join或提前关池")
	}
	finish()
	close(done)
}

func TestMaintenanceFenceWatcherJoinsBeforeClosingAccepted(t *testing.T) {
	ctx, owner := testMaintenanceContext()
	fence := &maintenanceTestFence{}
	_, err := maintenanceFenceContext(ctx, fence)
	if err != nil {
		t.Fatal(err)
	}
	if owner.session.Pending() != 1 {
		t.Fatal("watcher必须有实际accepted owner")
	}
	deadline, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := owner.stop(deadline); err != nil {
		t.Fatal(err)
	}
	owner.session.CloseAdmission()
	if err := owner.session.Wait(deadline); err != nil {
		t.Fatal(err)
	}
}

type maintenanceTestFence struct{}

func (*maintenanceTestFence) release(context.Context)                          {}
func (*maintenanceTestFence) refuseAndRelease(string, ...any)                  {}
func (*maintenanceTestFence) check() error                                     { return nil }
func (*maintenanceTestFence) writeContext(ctx context.Context) context.Context { return ctx }
