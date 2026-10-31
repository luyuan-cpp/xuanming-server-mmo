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

	"github.com/redis/go-redis/v9"
	"shared/drainproof"
)

func normalFreezeOptions() options {
	return options{routeDBDSN: "fixture@tcp(127.0.0.1:4000)/fixture", maintenanceRunID: strings.Repeat("1", 32), freezeTicketID: strings.Repeat("2", 32), freezeRequestID: strings.Repeat("3", 32), freezeReceiptSHA256: strings.Repeat("4", 64), frozenWriterEpoch: 7}
}

func TestNormalFreezeRequiresCompleteImmutableSelectors(t *testing.T) {
	if _, err := parseNormalFreezeProof(normalFreezeOptions()); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*options){
		func(o *options) { o.routeDBDSN = "" },
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

func TestNormalFreezeStrongReadBindsAllOriginalSelectors(t *testing.T) {
	p, err := parseNormalFreezeProof(normalFreezeOptions())
	if err != nil {
		t.Fatal(err)
	}
	st, f := newFakeRouteSQLStore(t, routeScript{}.on("NORMAL_FREEZE", rsOne()))
	if err = p.check(context.Background(), st.db); err != nil {
		t.Fatal(err)
	}
	calls := f.callsOf("NORMAL_FREEZE")
	want := []driver.Value{uint64(7), p.runID, p.ticketID, p.requestID, p.receiptDigest}
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].args, want) {
		t.Fatalf("强读必须使用原选择键: %+v", calls)
	}
	for _, reply := range []fakeRouteSQLReply{rsNone(), rsRows([]driver.Value{int64(0)}), rsRows([]driver.Value{int64(2)}), rsErr(errors.New("未知查询结果"))} {
		bad, _ := newFakeRouteSQLStore(t, routeScript{}.on("NORMAL_FREEZE", reply))
		if err = p.check(context.Background(), bad.db); !errors.Is(err, errNormalFreezeProof) {
			t.Fatalf("不存在/非唯一/UNKNOWN必须HOLD: %v", err)
		}
	}
}

func TestMaintenanceRouteTransactionLocksMetaBeforeActualWrite(t *testing.T) {
	ctx := routeSQLTestContext(t)
	for _, valid := range []bool{true, false} {
		t.Run(map[bool]string{true: "真实结局", false: "结局不匹配"}[valid], func(t *testing.T) {
			count := int64(0)
			script := routeScript{}.on("LOCK_META", rsRows([]driver.Value{int64(0)}))
			if valid {
				count = 1
				script = script.on("DELETE_MERGED", rsAffected(1))
			}
			script = script.on("NORMAL_FREEZE", rsRows([]driver.Value{count}))
			st, f := newFakeRouteSQLStore(t, script)
			err := st.runTx(ctx, "维护写", func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, routeDeleteMergedIntoSQL, uint64(101), uint64(102), testRouteRun)
				return err
			})
			if valid {
				if err != nil {
					t.Fatal(err)
				}
				assertRouteLabels(t, f, "BEGIN", "LOCK_META", "NORMAL_FREEZE", "DELETE_MERGED", "COMMIT")
			} else {
				if !errors.Is(err, errNormalFreezeProof) {
					t.Fatal(err)
				}
				assertRouteLabels(t, f, "BEGIN", "LOCK_META", "NORMAL_FREEZE", "ROLLBACK")
			}
		})
	}
	// 写开关开着 = 不是冻结状态,强读之前就拒绝。
	st, f := newFakeRouteSQLStore(t, routeScript{}.on("LOCK_META", rsRows([]driver.Value{int64(1)})))
	if err := st.runTx(ctx, "维护写", func(*sql.Tx) error { return errors.New("body must not run") }); !errors.Is(err, errNormalFreezeProof) {
		t.Fatal(err)
	}
	assertRouteLabels(t, f, "BEGIN", "LOCK_META", "ROLLBACK")
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
func TestMaintenanceFinishRequiresRealJoinBeforeResourceClose(t *testing.T) {
	ctx, owner := testMaintenanceContext()
	resource := &maintenanceTestCloser{}
	if err := registerMaintenanceResource(ctx, resource); err != nil {
		t.Fatal(err)
	}
	finish, err := enterMaintenanceWork(ctx)
	if err != nil {
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
}
