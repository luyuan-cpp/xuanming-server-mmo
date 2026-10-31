package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"shared/placement"

	"github.com/go-sql-driver/mysql"
)

// route_reconcile.go(R23–R30)的**对外语义**回归:库回什么 → 结局 / 错误、发了哪几条语句(含 BEGIN / COMMIT / ROLLBACK)、
// 每条语句的实参。做法同 route_store_contract_test.go(假驱动 + mysqlLikeConvert 参数转换),但用一个只认本文件语句与 R18 的
// 独立假驱动:那边的语句标签表(routeStmtLabel)属于第 1a / 1b-1 批写入方的契约,本批不往里加。
// 假驱动不模拟 InnoDB:锁、租约谓词在库时钟上的真实判定、并发只有真库能证(route_reconcile_integration_test.go)。

// reconcileFakeDB 按语句标签排队应答;认不出的语句、队列用尽都报错(脚本写少了不能被当成正常路径)。
type reconcileFakeDB struct {
	mu      sync.Mutex
	replies map[string][]fakeRouteReply
	calls   []fakeRouteCall
}

// reconcileStmtLabel 把 SQL 文本映射成编号,只认逐字相同的生产文本。
func reconcileStmtLabel(query string) string {
	switch query {
	case routeReadMetaSQL:
		return "R18"
	case routeRenewLeaseSQL:
		return "R23"
	case routePeekLeaseSQL:
		return "R24"
	case routeReleaseLeaseSQL:
		return "R25"
	case routeLockReconcileSQL:
		return "R26"
	case routeAdvanceReconcileSQL:
		return "R27"
	case routeScanUpdatedSinceSQL:
		return "R29"
	case routeDBNowSQL:
		return "R30"
	}
	return ""
}

// newReconcileFakeStore 把假驱动接成 RouteStore(同包直接装字段,不跑探针)。replies 会被拷一份再消费。
func newReconcileFakeStore(t *testing.T, replies map[string][]fakeRouteReply) (*RouteStore, *reconcileFakeDB) {
	t.Helper()
	fake := &reconcileFakeDB{replies: make(map[string][]fakeRouteReply, len(replies))}
	for k, v := range replies {
		fake.replies[k] = append([]fakeRouteReply(nil), v...)
	}
	db := sql.OpenDB(reconcileConnector{fake: fake})
	t.Cleanup(func() { _ = db.Close() })
	return &RouteStore{db: db, writer: fakeRouteWriter}, fake
}

func (f *reconcileFakeDB) next(query string, args []driver.Value) (fakeRouteReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	label := reconcileStmtLabel(query)
	f.calls = append(f.calls, fakeRouteCall{label: label, sql: query, args: args})
	if label == "" {
		return fakeRouteReply{}, fmt.Errorf("reconcile fake db: unexpected statement %q", query)
	}
	q := f.replies[label]
	if len(q) == 0 {
		return fakeRouteReply{}, fmt.Errorf("%w: %s %q", errFakeRouteScriptExhausted, label, query)
	}
	f.replies[label] = q[1:]
	return q[0], nil
}

func (f *reconcileFakeDB) record(label string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeRouteCall{label: label})
}

func (f *reconcileFakeDB) labels() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.label)
	}
	return out
}

func (f *reconcileFakeDB) callsWithLabel(label string) []fakeRouteCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeRouteCall
	for _, c := range f.calls {
		if c.label == label {
			out = append(out, c)
		}
	}
	return out
}

// ── 驱动实现 ─────────────────────────────────────────────────────────────────────

type reconcileConnector struct{ fake *reconcileFakeDB }

func (c reconcileConnector) Connect(context.Context) (driver.Conn, error) {
	return &reconcileConn{fake: c.fake}, nil
}

func (c reconcileConnector) Driver() driver.Driver { return reconcileDriver{} }

type reconcileDriver struct{}

func (reconcileDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("reconcile fake db: only sql.OpenDB(connector) is supported")
}

type reconcileConn struct{ fake *reconcileFakeDB }

func (c *reconcileConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("reconcile fake db: Prepare is not supported")
}

func (c *reconcileConn) Close() error { return nil }

func (c *reconcileConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *reconcileConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.fake.record("BEGIN")
	return reconcileTx{fake: c.fake}, nil
}

// CheckNamedValue 与 route_store_contract_test.go 同口径:按 go-sql-driver 的规则转换实参(uint64 原样放行)。
func (c *reconcileConn) CheckNamedValue(nv *driver.NamedValue) error {
	v, err := mysqlLikeConvert(nv.Value)
	if err != nil {
		return err
	}
	nv.Value = v
	return nil
}

func (c *reconcileConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	reply, err := c.fake.next(query, namedToValues(args))
	if err != nil {
		return nil, err
	}
	if reply.err != nil {
		return nil, reply.err
	}
	return driver.RowsAffected(reply.affected), nil
}

func (c *reconcileConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	reply, err := c.fake.next(query, namedToValues(args))
	if err != nil {
		return nil, err
	}
	if reply.err != nil {
		return nil, reply.err
	}
	cols := 1
	if len(reply.rows) > 0 {
		cols = len(reply.rows[0])
	}
	return &fakeRouteRows{cols: cols, rows: append([][]driver.Value(nil), reply.rows...)}, nil
}

type reconcileTx struct{ fake *reconcileFakeDB }

func (t reconcileTx) Commit() error {
	t.fake.record("COMMIT")
	return nil
}

func (t reconcileTx) Rollback() error {
	t.fake.record("ROLLBACK")
	return nil
}

// ── 应答与断言的小工具 ───────────────────────────────────────────────────────────

const (
	rcLeader = "route_leader@contract:0000abcd"
	rcOther  = "route_leader@other:00001234"
	rcGen    = "00112233445566778899aabbccddeeff"
)

const (
	rcNow uint64 = 1_760_000_200_000
	rcWM  uint64 = rcNow - 1000
	rcCvm uint64 = rcNow - 60_000
)

// rcRow24 是 R24 读到的一行:leader(空串 = NULL)、lease_until_ms(0 = NULL)、库时钟。
func rcRow24(leader string, until, now uint64) fakeRouteReply {
	return fakeRouteReply{rows: [][]driver.Value{{dvStr(leader), dv64(until), int64(now)}}}
}

// rcRow26 是 R26 读到的一行:leader、lease_until_ms(0 = NULL)、watermark_ms、cache_verified_ms、库时钟。
func rcRow26(leader string, until, wm, cvm, now uint64) fakeRouteReply {
	return fakeRouteReply{rows: [][]driver.Value{{dvStr(leader), dv64(until), int64(wm), int64(cvm), int64(now)}}}
}

// rcHeld 是「本 leader 持有租约」的 R26 应答。
func rcHeld() fakeRouteReply { return rcRow26(rcLeader, rcNow+5000, rcWM, rcCvm, rcNow) }

// rcMeta 是 R18 读到的 route_meta 行。
func rcMeta(gen string, writable int64) fakeRouteReply {
	return fakeRouteReply{rows: [][]driver.Value{{gen, writable}}}
}

// rcTick 是本用例集的缺省每拍请求(无水位键、不写 cvm、冻结时限取下限);mut 可改其中字段。
func rcTick(mut func(*ReconcileTickRequest)) ReconcileTickRequest {
	req := ReconcileTickRequest{LeaderID: rcLeader, FreezeTimeout: RouteFreezeTimeoutMin}
	if mut != nil {
		mut(&req)
	}
	return req
}

func rcKey(gen string, wm uint64) *placement.RouteWatermark {
	return &placement.RouteWatermark{V: placement.RouteWatermarkFormatVersion, Generation: gen, WatermarkMs: wm}
}

func rcU64(v uint64) *uint64 { return &v }

func assertReconcileLabels(t *testing.T, fake *reconcileFakeDB, want ...string) {
	t.Helper()
	if got := fake.labels(); !reflect.DeepEqual(got, want) {
		t.Fatalf("statement sequence:\n got=%v\nwant=%v", got, want)
	}
}

func assertReconcileArgs(t *testing.T, fake *reconcileFakeDB, label string, want ...driver.Value) {
	t.Helper()
	calls := fake.callsWithLabel(label)
	if len(calls) != 1 {
		t.Fatalf("%s sent %d times, want 1", label, len(calls))
	}
	if len(calls[0].args) == 0 && len(want) == 0 {
		return // namedToValues 对零个实参回空切片,want 是 nil:两者等价
	}
	if !reflect.DeepEqual(calls[0].args, want) {
		t.Fatalf("%s args:\n got=%#v\nwant=%#v", label, calls[0].args, want)
	}
}

func assertTickResult(t *testing.T, got, want ReconcileTickResult) {
	t.Helper()
	if got != want {
		t.Fatalf("result:\n got=%+v (%s, %s)\nwant=%+v (%s, %s)", got, got.Outcome, got.FreezeReason, want, want.Outcome, want.FreezeReason)
	}
}

// ── 租约 ────────────────────────────────────────────────────────────────────────

// TestRenewReconcileLeaseAcquireRenewRefuseTakeover:R23 受影响 1 行即持有(不再读);0 行回落 R24,按读到的持有者返回。
// 「过期才可接管」的判据在库里(R23 的 WHERE),假驱动只能给出它的结论;真库用例 TestRouteReconcile_LeaseContention 取证。
func TestRenewReconcileLeaseAcquireRenewRefuseTakeover(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replies map[string][]fakeRouteReply
		want    RouteLease
		held    bool
		labels  []string
	}{
		{"空闲时获取", map[string][]fakeRouteReply{"R23": {replyAffected(1)}},
			RouteLease{Held: true, Holder: rcLeader}, true, []string{"R23"}},
		{"自己续约", map[string][]fakeRouteReply{"R23": {replyAffected(1)}},
			RouteLease{Held: true, Holder: rcLeader}, true, []string{"R23"}},
		{"他人租约已过期_接管", map[string][]fakeRouteReply{"R23": {replyAffected(1)}},
			RouteLease{Held: true, Holder: rcLeader}, true, []string{"R23"}},
		{"他人持有有效租约_拒绝", map[string][]fakeRouteReply{"R23": {replyAffected(0)}, "R24": {rcRow24(rcOther, rcNow+5000, rcNow)}},
			RouteLease{Held: true, Holder: rcOther, LeaseUntilMs: rcNow + 5000, NowMs: rcNow}, false, []string{"R23", "R24"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, fake := newReconcileFakeStore(t, tc.replies)
			got, err := st.RenewReconcileLease(context.Background(), rcLeader, 10_000)
			if err != nil {
				t.Fatalf("RenewReconcileLease: %v", err)
			}
			if got != tc.want || got.HeldBy(rcLeader) != tc.held {
				t.Fatalf("lease = %+v (heldBy me %t), want %+v (heldBy me %t)", got, got.HeldBy(rcLeader), tc.want, tc.held)
			}
			assertReconcileLabels(t, fake, tc.labels...)
			assertReconcileArgs(t, fake, "R23", rcLeader, uint64(10_000), rcLeader)
		})
	}
}

// TestRenewZeroRowsFallsBackToPeek:R23 受影响 0 行时(同一毫秒里续约、值没变也是 0 行)再读 R24 判定;缺行、库时钟为 0 一律报错。
func TestRenewZeroRowsFallsBackToPeek(t *testing.T) {
	t.Run("同毫秒续约_仍持有", func(t *testing.T) {
		st, fake := newReconcileFakeStore(t, map[string][]fakeRouteReply{
			"R23": {replyAffected(0)}, "R24": {rcRow24(rcLeader, rcNow+10_000, rcNow)},
		})
		got, err := st.RenewReconcileLease(context.Background(), rcLeader, 10_000)
		if err != nil || !got.HeldBy(rcLeader) || got.LeaseUntilMs != rcNow+10_000 || got.NowMs != rcNow {
			t.Fatalf("RenewReconcileLease = (%+v, %v), want held by me until %d", got, err, rcNow+10_000)
		}
		assertReconcileLabels(t, fake, "R23", "R24")
	})
	t.Run("回落读到无人持有", func(t *testing.T) {
		st, _ := newReconcileFakeStore(t, map[string][]fakeRouteReply{"R23": {replyAffected(0)}, "R24": {rcRow24("", 0, rcNow)}})
		got, err := st.RenewReconcileLease(context.Background(), rcLeader, 10_000)
		if err != nil || got.Held || got.Holder != "" {
			t.Fatalf("RenewReconcileLease = (%+v, %v), want not held", got, err)
		}
	})
	t.Run("对账行缺失", func(t *testing.T) {
		st, _ := newReconcileFakeStore(t, map[string][]fakeRouteReply{"R23": {replyAffected(0)}, "R24": {replyNoRows}})
		if _, err := st.RenewReconcileLease(context.Background(), rcLeader, 10_000); !errors.Is(err, ErrRouteReconcileMissing) {
			t.Fatalf("err = %v, want ErrRouteReconcileMissing", err)
		}
	})
	t.Run("库时钟为0", func(t *testing.T) {
		st, _ := newReconcileFakeStore(t, map[string][]fakeRouteReply{"R23": {replyAffected(0)}, "R24": {rcRow24(rcLeader, rcNow, 0)}})
		if _, err := st.RenewReconcileLease(context.Background(), rcLeader, 10_000); err == nil {
			t.Fatal("a zero database clock must be an error")
		}
	})
	t.Run("受影响行数越界", func(t *testing.T) {
		st, _ := newReconcileFakeStore(t, map[string][]fakeRouteReply{"R23": {replyAffected(2)}})
		if _, err := st.RenewReconcileLease(context.Background(), rcLeader, 10_000); err == nil {
			t.Fatal("2 affected rows on a single-row table must be an error")
		}
	})
}

// TestPeekReconcileLease:R24 的三种观测 —— 有效、已过期(持有者仍在)、无人(NULL)。
func TestPeekReconcileLease(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply fakeRouteReply
		want  RouteLease
	}{
		{"有效", rcRow24(rcOther, rcNow, rcNow), RouteLease{Held: true, Holder: rcOther, LeaseUntilMs: rcNow, NowMs: rcNow}},
		{"已过期", rcRow24(rcOther, rcNow-1, rcNow), RouteLease{Holder: rcOther, LeaseUntilMs: rcNow - 1, NowMs: rcNow}},
		{"租约为NULL", rcRow24(rcOther, 0, rcNow), RouteLease{Holder: rcOther, NowMs: rcNow}},
		{"无人", rcRow24("", 0, rcNow), RouteLease{NowMs: rcNow}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, fake := newReconcileFakeStore(t, map[string][]fakeRouteReply{"R24": {tc.reply}})
			got, err := st.PeekReconcileLease(context.Background())
			if err != nil || got != tc.want {
				t.Fatalf("PeekReconcileLease = (%+v, %v), want %+v", got, err, tc.want)
			}
			assertReconcileLabels(t, fake, "R24")
		})
	}
}

// TestReleaseReconcileLease:R25 只清自己的;0 行 = 已不是持有者,不是错误。
func TestReleaseReconcileLease(t *testing.T) {
	for _, tc := range []struct {
		name     string
		affected int64
		want     bool
	}{
		{"释放自己的", 1, true},
		{"已不是持有者", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, fake := newReconcileFakeStore(t, map[string][]fakeRouteReply{"R25": {replyAffected(tc.affected)}})
			got, err := st.ReleaseReconcileLease(context.Background(), rcLeader)
			if err != nil || got != tc.want {
				t.Fatalf("ReleaseReconcileLease = (%t, %v), want %t", got, err, tc.want)
			}
			assertReconcileLabels(t, fake, "R25")
			assertReconcileArgs(t, fake, "R25", rcLeader)
		})
	}
}

// TestReconcileLeaseRejectsInvalidArguments:入参非法时不发任何语句。
func TestReconcileLeaseRejectsInvalidArguments(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		call func(*RouteStore) error
	}{
		{"续约_空标识", func(st *RouteStore) error {
			_, err := st.RenewReconcileLease(ctx, "", 10_000)
			return err
		}},
		{"续约_非法字符", func(st *RouteStore) error {
			_, err := st.RenewReconcileLease(ctx, "a b", 10_000)
			return err
		}},
		{"续约_租约为0", func(st *RouteStore) error {
			_, err := st.RenewReconcileLease(ctx, rcLeader, 0)
			return err
		}},
		{"续约_租约超上限", func(st *RouteStore) error {
			_, err := st.RenewReconcileLease(ctx, rcLeader, routeLeaseMaxMs+1)
			return err
		}},
		{"释放_空标识", func(st *RouteStore) error {
			_, err := st.ReleaseReconcileLease(ctx, "")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, fake := newReconcileFakeStore(t, nil)
			if err := tc.call(st); !errors.Is(err, ErrRouteInvalidArgument) {
				t.Fatalf("err = %v, want ErrRouteInvalidArgument", err)
			}
			if got := fake.labels(); len(got) != 0 {
				t.Fatalf("invalid arguments still sent statements: %v", got)
			}
		})
	}
}

// ── 每拍 ────────────────────────────────────────────────────────────────────────

// TestAdvanceReconcileStatementSequence:正常一拍是 BEGIN → R26 → R18 → R27 → COMMIT;R27 绑 R26 读到的库时钟、
// cvm 不给定时绑 NULL(COALESCE 保持原值)、leaderID(租约谓词);结果是提交后的水位 = max(原水位, 库时钟)。
func TestAdvanceReconcileStatementSequence(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  *placement.RouteWatermark
	}{
		{"无水位键", nil},
		{"水位键与库一致", rcKey(rcGen, rcWM)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, fake := newReconcileFakeStore(t, map[string][]fakeRouteReply{
				"R26": {rcHeld()}, "R18": {rcMeta(rcGen, 1)}, "R27": {replyAffected(1)},
			})
			got, err := st.AdvanceReconcile(context.Background(), rcTick(func(r *ReconcileTickRequest) { r.WatermarkKey = tc.key }))
			if err != nil {
				t.Fatalf("AdvanceReconcile: %v", err)
			}
			assertTickResult(t, got, ReconcileTickResult{
				Outcome: ReconcileTickAdvanced, Generation: rcGen, WatermarkMs: rcNow, CacheVerifiedMs: rcCvm, NowMs: rcNow,
			})
			assertReconcileLabels(t, fake, "BEGIN", "R26", "R18", "R27", "COMMIT")
			assertReconcileArgs(t, fake, "R27", rcNow, nil, rcLeader)
		})
	}
}

// TestAdvanceReconcileCacheVerifiedExactWrite:给定 cvm 时按给定值精确写(可以变小、可以为 0);超过库时钟时按库时钟写。
func TestAdvanceReconcileCacheVerifiedExactWrite(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  uint64
		want uint64
	}{
		{"变小", rcCvm - 1000, rcCvm - 1000},
		{"为0", 0, 0},
		{"推进到库时钟", rcNow, rcNow},
		{"超过库时钟_截到库时钟", rcNow + 5, rcNow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, fake := newReconcileFakeStore(t, map[string][]fakeRouteReply{
				"R26": {rcHeld()}, "R18": {rcMeta(rcGen, 1)}, "R27": {replyAffected(1)},
			})
			got, err := st.AdvanceReconcile(context.Background(), rcTick(func(r *ReconcileTickRequest) { r.SetCacheVerifiedMs = rcU64(tc.set) }))
			if err != nil {
				t.Fatalf("AdvanceReconcile: %v", err)
			}
			if got.Outcome != ReconcileTickAdvanced || got.CacheVerifiedMs != tc.want {
				t.Fatalf("result = %+v, want Advanced with cvm %d", got, tc.want)
			}
			assertReconcileArgs(t, fake, "R27", rcNow, tc.want, rcLeader)
		})
	}
}

// TestAdvanceReconcileFreezeIssuesNoWatermarkUpdate:危险只返回带完整观察的错误，事务回滚、零控制写；原水位与 cvm 不变。
func TestAdvanceReconcileFreezeIssuesNoWatermarkUpdate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		key    *placement.RouteWatermark
		tol    uint64
		reason RouteFreezeReason
	}{
		{"世代不符", rcKey("ffffffffffffffffffffffffffffffff", 1), 0, RouteFreezeGenerationMismatch},
		{"水位倒退_容差0", rcKey(rcGen, rcWM+1), 0, RouteFreezeWatermarkRegressed},
		{"水位倒退_容差1000_超出", rcKey(rcGen, rcWM+1001), 1000, RouteFreezeWatermarkRegressed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, fake := newReconcileFakeStore(t, map[string][]fakeRouteReply{"R26": {rcHeld()}, "R18": {rcMeta(rcGen, 1)}})
			got, err := st.AdvanceReconcile(context.Background(), rcTick(func(r *ReconcileTickRequest) {
				r.WatermarkKey, r.ToleranceMs, r.SetCacheVerifiedMs = tc.key, tc.tol, rcU64(rcNow)
			}))
			var danger *RouteEmergencyError
			if !errors.As(err, &danger) {
				t.Fatalf("危险必须返回正式观察错误: %v", err)
			}
			assertTickResult(t, got, ReconcileTickResult{})
			assertTickResult(t, danger.Observation, ReconcileTickResult{Outcome: ReconcileTickFrozen, FreezeReason: tc.reason,
				Generation: rcGen, WatermarkMs: rcWM, CacheVerifiedMs: rcCvm, NowMs: rcNow})
			assertReconcileLabels(t, fake, "BEGIN", "R26", "R18", "ROLLBACK")
			if len(fake.callsWithLabel("R27")) != 0 {
				t.Fatal("危险观察不得推进水位或 cvm")
			}
		})
	}
	t.Run("独立operator已先冻结", func(t *testing.T) {
		st, fake := newReconcileFakeStore(t, map[string][]fakeRouteReply{"R26": {rcHeld()}, "R18": {rcMeta(rcGen, 0)}})
		got, err := st.AdvanceReconcile(context.Background(), rcTick(func(r *ReconcileTickRequest) { r.WatermarkKey = rcKey(rcGen, rcWM+1) }))
		if err != nil {
			t.Fatal(err)
		}
		assertTickResult(t, got, ReconcileTickResult{Outcome: ReconcileTickAlreadyFrozen, Generation: rcGen, WatermarkMs: rcWM, CacheVerifiedMs: rcCvm, NowMs: rcNow})
		assertReconcileLabels(t, fake, "BEGIN", "R26", "R18", "ROLLBACK")
	})
	t.Run("危险重试仍不得自行冻结", func(t *testing.T) {
		st, fake := newReconcileFakeStore(t, map[string][]fakeRouteReply{"R26": {rcHeld(), rcHeld()}, "R18": {rcMeta(rcGen, 1), rcMeta(rcGen, 1)}})
		for i := 0; i < 2; i++ {
			got, err := st.AdvanceReconcile(context.Background(), rcTick(func(r *ReconcileTickRequest) { r.WatermarkKey = rcKey(rcGen, rcWM+1) }))
			var danger *RouteEmergencyError
			if !errors.As(err, &danger) || danger.Observation.FreezeReason != RouteFreezeWatermarkRegressed {
				t.Fatalf("错误猜测已冻结: %+v %v", got, err)
			}
			assertTickResult(t, got, ReconcileTickResult{})
		}
		assertReconcileLabels(t, fake, "BEGIN", "R26", "R18", "ROLLBACK", "BEGIN", "R26", "R18", "ROLLBACK")
	})
}

// TestAdvanceReconcileWithinToleranceAdvances:水位键高出库水位但没超过容差 → 照常推进(容差 1000 一档)。
func TestAdvanceReconcileWithinToleranceAdvances(t *testing.T) {
	st, fake := newReconcileFakeStore(t, map[string][]fakeRouteReply{
		"R26": {rcHeld()}, "R18": {rcMeta(rcGen, 1)}, "R27": {replyAffected(1)},
	})
	got, err := st.AdvanceReconcile(context.Background(), rcTick(func(r *ReconcileTickRequest) {
		r.WatermarkKey, r.ToleranceMs = rcKey(rcGen, rcWM+1000), 1000
	}))
	if err != nil || got.Outcome != ReconcileTickAdvanced {
		t.Fatalf("AdvanceReconcile = (%+v, %v), want Advanced", got, err)
	}
	assertReconcileLabels(t, fake, "BEGIN", "R26", "R18", "R27", "COMMIT")
}

// TestAdvanceReconcileLeaseLostWritesNothing:不持有租约时零写入(只有 R26 的锁与 R18 的普通读,随后回滚);
// R27 因执行时的库时钟走过租约截止而 0 行 → 事务内复读 R24 确认 → LeaseLost、回滚。
func TestAdvanceReconcileLeaseLostWritesNothing(t *testing.T) {
	lost := ReconcileTickResult{Outcome: ReconcileTickLeaseLost, Generation: rcGen, WatermarkMs: rcWM, CacheVerifiedMs: rcCvm, NowMs: rcNow}
	for _, tc := range []struct {
		name string
		r26  fakeRouteReply
	}{
		{"他人持有", rcRow26(rcOther, rcNow+5000, rcWM, rcCvm, rcNow)},
		{"已过期", rcRow26(rcLeader, rcNow-1, rcWM, rcCvm, rcNow)},
		{"租约为NULL", rcRow26(rcLeader, 0, rcWM, rcCvm, rcNow)},
		{"无人持有", rcRow26("", 0, rcWM, rcCvm, rcNow)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, fake := newReconcileFakeStore(t, map[string][]fakeRouteReply{"R26": {tc.r26}, "R18": {rcMeta(rcGen, 1)}})
			got, err := st.AdvanceReconcile(context.Background(), rcTick(func(r *ReconcileTickRequest) {
				r.WatermarkKey, r.SetCacheVerifiedMs = rcKey("ffffffffffffffffffffffffffffffff", rcWM+1), rcU64(1)
			}))
			if err != nil {
				t.Fatalf("AdvanceReconcile: %v", err)
			}
			assertTickResult(t, got, lost)
			assertReconcileLabels(t, fake, "BEGIN", "R26", "R18", "ROLLBACK")
		})
	}

	t.Run("R27时租约已过期", func(t *testing.T) {
		st, fake := newReconcileFakeStore(t, map[string][]fakeRouteReply{
			"R26": {rcHeld()}, "R18": {rcMeta(rcGen, 1)}, "R27": {replyAffected(0)},
			"R24": {rcRow24(rcLeader, rcNow+5000, rcNow+5001)},
		})
		got, err := st.AdvanceReconcile(context.Background(), rcTick(nil))
		if err != nil {
			t.Fatalf("AdvanceReconcile: %v", err)
		}
		assertTickResult(t, got, lost)
		assertReconcileLabels(t, fake, "BEGIN", "R26", "R18", "R27", "R24", "ROLLBACK")
	})
}

// TestAdvanceReconcileAlreadyFrozenWritesNothing:route_meta 已冻结时零写入,即使水位键给出了回档证据(不重复冻结、不推进水位)。
func TestAdvanceReconcileAlreadyFrozenWritesNothing(t *testing.T) {
	st, fake := newReconcileFakeStore(t, map[string][]fakeRouteReply{"R26": {rcHeld()}, "R18": {rcMeta(rcGen, 0)}})
	got, err := st.AdvanceReconcile(context.Background(), rcTick(func(r *ReconcileTickRequest) {
		r.WatermarkKey, r.SetCacheVerifiedMs = rcKey(rcGen, rcWM+1), rcU64(rcNow)
	}))
	if err != nil {
		t.Fatalf("AdvanceReconcile: %v", err)
	}
	assertTickResult(t, got, ReconcileTickResult{Outcome: ReconcileTickAlreadyFrozen, Generation: rcGen, WatermarkMs: rcWM, CacheVerifiedMs: rcCvm, NowMs: rcNow})
	assertReconcileLabels(t, fake, "BEGIN", "R26", "R18", "ROLLBACK")
}

// TestAdvanceReconcileZeroRowsWhenUnchanged:R27 0 行而复读仍持有:结论本就是「值不变」(库水位已超过库时钟、cvm 不给定)→ 照常提交;
// 结论要改值却 0 行 → fail-closed 报错、回滚。
func TestAdvanceReconcileZeroRowsWhenUnchanged(t *testing.T) {
	ahead := rcNow + 3_600_000
	t.Run("值不变_提交", func(t *testing.T) {
		st, fake := newReconcileFakeStore(t, map[string][]fakeRouteReply{
			"R26": {rcRow26(rcLeader, rcNow+5000, ahead, rcCvm, rcNow)}, "R18": {rcMeta(rcGen, 1)},
			"R27": {replyAffected(0)}, "R24": {rcRow24(rcLeader, rcNow+5000, rcNow+1)},
		})
		got, err := st.AdvanceReconcile(context.Background(), rcTick(nil))
		if err != nil {
			t.Fatalf("AdvanceReconcile: %v", err)
		}
		assertTickResult(t, got, ReconcileTickResult{Outcome: ReconcileTickAdvanced, Generation: rcGen, WatermarkMs: ahead, CacheVerifiedMs: rcCvm, NowMs: rcNow})
		assertReconcileLabels(t, fake, "BEGIN", "R26", "R18", "R27", "R24", "COMMIT")
	})
	t.Run("应改值却零行_报错", func(t *testing.T) {
		st, fake := newReconcileFakeStore(t, map[string][]fakeRouteReply{
			"R26": {rcHeld()}, "R18": {rcMeta(rcGen, 1)}, "R27": {replyAffected(0)}, "R24": {rcRow24(rcLeader, rcNow+5000, rcNow+1)},
		})
		if _, err := st.AdvanceReconcile(context.Background(), rcTick(nil)); err == nil {
			t.Fatal("R27 affected 0 rows although the watermark must move: must fail closed")
		}
		assertReconcileLabels(t, fake, "BEGIN", "R26", "R18", "R27", "R24", "ROLLBACK")
	})
}

// TestAdvanceReconcileRejectsInvalidRequest:入参非法时不开事务。手工拼的零值水位键(世代为空)会被当成「世代不符」冻结写者,必须先拦下。
func TestAdvanceReconcileRejectsInvalidRequest(t *testing.T) {
	newerKey := &placement.RouteWatermark{V: placement.RouteWatermarkFormatVersion + 1, Generation: rcGen, WatermarkMs: rcWM}
	for _, tc := range []struct {
		name string
		mut  func(*ReconcileTickRequest)
	}{
		{"空标识", func(r *ReconcileTickRequest) { r.LeaderID = "" }},
		{"冻结时限为0", func(r *ReconcileTickRequest) { r.FreezeTimeout = 0 }},
		{"冻结时限低于下限", func(r *ReconcileTickRequest) { r.FreezeTimeout = RouteFreezeTimeoutMin - time.Millisecond }},
		{"零值水位键", func(r *ReconcileTickRequest) { r.WatermarkKey = &placement.RouteWatermark{} }},
		{"水位键世代非法", func(r *ReconcileTickRequest) { r.WatermarkKey = rcKey("XYZ", rcWM) }},
		{"水位键水位为0", func(r *ReconcileTickRequest) { r.WatermarkKey = rcKey(rcGen, 0) }},
		{"水位键版本不认识", func(r *ReconcileTickRequest) { r.WatermarkKey = newerKey }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, fake := newReconcileFakeStore(t, nil)
			if _, err := st.AdvanceReconcile(context.Background(), rcTick(tc.mut)); !errors.Is(err, ErrRouteInvalidArgument) {
				t.Fatalf("err = %v, want ErrRouteInvalidArgument", err)
			}
			if got := fake.labels(); len(got) != 0 {
				t.Fatalf("invalid request still sent statements: %v", got)
			}
		})
	}
}

// TestAdvanceReconcileMissingRowsAndBadClock:缺对账行 / 缺 meta 行回各自的哨兵;库时钟读成 0 报错。都回滚、零写入。
func TestAdvanceReconcileMissingRowsAndBadClock(t *testing.T) {
	t.Run("对账行缺失", func(t *testing.T) {
		st, fake := newReconcileFakeStore(t, map[string][]fakeRouteReply{"R26": {replyNoRows}})
		if _, err := st.AdvanceReconcile(context.Background(), rcTick(nil)); !errors.Is(err, ErrRouteReconcileMissing) {
			t.Fatalf("err = %v, want ErrRouteReconcileMissing", err)
		}
		assertReconcileLabels(t, fake, "BEGIN", "R26", "ROLLBACK")
	})
	t.Run("meta行缺失", func(t *testing.T) {
		st, fake := newReconcileFakeStore(t, map[string][]fakeRouteReply{"R26": {rcHeld()}, "R18": {replyNoRows}})
		if _, err := st.AdvanceReconcile(context.Background(), rcTick(nil)); !errors.Is(err, ErrRouteMetaMissing) {
			t.Fatalf("err = %v, want ErrRouteMetaMissing", err)
		}
		assertReconcileLabels(t, fake, "BEGIN", "R26", "R18", "ROLLBACK")
	})
	t.Run("库时钟为0", func(t *testing.T) {
		st, fake := newReconcileFakeStore(t, map[string][]fakeRouteReply{"R26": {rcRow26(rcLeader, rcNow, rcWM, rcCvm, 0)}})
		if _, err := st.AdvanceReconcile(context.Background(), rcTick(nil)); err == nil {
			t.Fatal("a zero database clock must be an error")
		}
		assertReconcileLabels(t, fake, "BEGIN", "R26", "ROLLBACK")
	})
}

// TestAdvanceReconcileRetriesOnlyDeadlockAndWriteConflict:1213 / 9007 整事务重跑并计数;1205 不重试、原样返回。
func TestAdvanceReconcileRetriesOnlyDeadlockAndWriteConflict(t *testing.T) {
	t.Run("1213与9007各一次后成功", func(t *testing.T) {
		st, fake := newReconcileFakeStore(t, map[string][]fakeRouteReply{
			"R26": {replyErr(mysqlErrNo(mysqlErrDeadlock)), replyErr(mysqlErrNo(tidbErrWriteConflict)), rcHeld()},
			"R18": {rcMeta(rcGen, 1)},
			"R27": {replyAffected(1)},
		})
		got, err := st.AdvanceReconcile(context.Background(), rcTick(nil))
		if err != nil || got.Outcome != ReconcileTickAdvanced {
			t.Fatalf("AdvanceReconcile = (%+v, %v), want Advanced", got, err)
		}
		if n := st.LockRetriesForTest(); n != 2 {
			t.Fatalf("lockRetries = %d, want 2", n)
		}
		assertReconcileLabels(t, fake,
			"BEGIN", "R26", "ROLLBACK",
			"BEGIN", "R26", "ROLLBACK",
			"BEGIN", "R26", "R18", "R27", "COMMIT")
	})
	t.Run("1205不重试", func(t *testing.T) {
		st, fake := newReconcileFakeStore(t, map[string][]fakeRouteReply{
			"R26": {replyErr(mysqlErrNo(mysqlErrLockWaitTimeout)), rcHeld()},
		})
		_, err := st.AdvanceReconcile(context.Background(), rcTick(nil))
		var me *mysql.MySQLError
		if !errors.As(err, &me) || me.Number != mysqlErrLockWaitTimeout {
			t.Fatalf("err = %v, want the 1205 storage error", err)
		}
		if n := st.LockRetriesForTest(); n != 0 {
			t.Fatalf("lockRetries = %d, want 0", n)
		}
		assertReconcileLabels(t, fake, "BEGIN", "R26", "ROLLBACK")
	})
}

// ── leader 冻结 ─────────────────────────────────────────────────────────────────

// TestFreezeByLeader:持有租约且可写只返回危险观察；已冻结 / 租约丢失照旧零写入回滚。
func TestFreezeByLeader(t *testing.T) {
	base := ReconcileTickResult{Generation: rcGen, WatermarkMs: rcWM, CacheVerifiedMs: rcCvm, NowMs: rcNow}
	withOutcome := func(o ReconcileTickOutcome, r RouteFreezeReason) ReconcileTickResult {
		res := base
		res.Outcome, res.FreezeReason = o, r
		return res
	}
	for _, tc := range []struct {
		name    string
		replies map[string][]fakeRouteReply
		want    ReconcileTickResult
		labels  []string
	}{
		{"冻结", map[string][]fakeRouteReply{"R26": {rcHeld()}, "R18": {rcMeta(rcGen, 1)}},
			withOutcome(ReconcileTickFrozen, RouteFreezeCacheAhead), []string{"BEGIN", "R26", "R18", "ROLLBACK"}},
		{"已冻结", map[string][]fakeRouteReply{"R26": {rcHeld()}, "R18": {rcMeta(rcGen, 0)}},
			withOutcome(ReconcileTickAlreadyFrozen, RouteFreezeNone), []string{"BEGIN", "R26", "R18", "ROLLBACK"}},
		{"租约丢失", map[string][]fakeRouteReply{"R26": {rcRow26(rcOther, rcNow+5000, rcWM, rcCvm, rcNow)}, "R18": {rcMeta(rcGen, 1)}},
			withOutcome(ReconcileTickLeaseLost, RouteFreezeNone), []string{"BEGIN", "R26", "R18", "ROLLBACK"}},
		{"被抢先冻结", map[string][]fakeRouteReply{"R26": {rcHeld()}, "R18": {rcMeta(rcGen, 0)}},
			withOutcome(ReconcileTickAlreadyFrozen, RouteFreezeNone), []string{"BEGIN", "R26", "R18", "ROLLBACK"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, fake := newReconcileFakeStore(t, tc.replies)
			got, err := st.FreezeByLeader(context.Background(), rcLeader, RouteFreezeCacheAhead, RouteFreezeTimeoutMin)
			if tc.want.Outcome == ReconcileTickFrozen {
				var danger *RouteEmergencyError
				if !errors.As(err, &danger) {
					t.Fatalf("必须交付危险观察: %v", err)
				}
				assertTickResult(t, danger.Observation, tc.want)
				// 观察只在带类型错误中交给root/relay owner；成功返回值必须为空，不能冒充控制表已冻结。
				assertTickResult(t, got, ReconcileTickResult{})
			} else {
				if err != nil {
					t.Fatalf("FreezeByLeader: %v", err)
				}
				assertTickResult(t, got, tc.want)
			}
			assertReconcileLabels(t, fake, tc.labels...)
		})
	}

	for _, tc := range []struct {
		name    string
		leader  string
		reason  RouteFreezeReason
		timeout time.Duration
	}{
		{"原因为none", rcLeader, RouteFreezeNone, RouteFreezeTimeoutMin},
		{"原因越界", rcLeader, routeFreezeReasonCount, RouteFreezeTimeoutMin},
		{"时限过短", rcLeader, RouteFreezeCacheAhead, time.Second},
		{"空标识", "", RouteFreezeCacheAhead, RouteFreezeTimeoutMin},
	} {
		t.Run("拒绝_"+tc.name, func(t *testing.T) {
			st, fake := newReconcileFakeStore(t, nil)
			if _, err := st.FreezeByLeader(context.Background(), tc.leader, tc.reason, tc.timeout); !errors.Is(err, ErrRouteInvalidArgument) {
				t.Fatalf("err = %v, want ErrRouteInvalidArgument", err)
			}
			if got := fake.labels(); len(got) != 0 {
				t.Fatalf("invalid arguments still sent statements: %v", got)
			}
		})
	}
}

// ── 增量扫描与库时钟 ─────────────────────────────────────────────────────────────

// rcScanRow 是 R29 读到的一行(列序同 scanRouteRow withKey):存的片、player_id、home、无落点、rev 1、updated_at_ms。
func rcScanRow(shard, player uint64, updatedAt int64) []driver.Value {
	return []driver.Value{int64(shard), int64(player), int64(7), nil, nil, nil, int64(1), fakeRouteOldWriter, fakeRouteCreatedMs, updatedAt}
}

// TestScanRoutesUpdatedSinceSQLAndShardCheck:R29 的实参(片、游标两次、player_id、limit)与结果顺序;
// 错片行(库里存的片与 player_id 不符,或与入参片不符)→ ErrRouteShardMismatch;键不在游标之后 / 不递增 / 超过 limit → 报错;
// 入参非法不发语句。
func TestScanRoutesUpdatedSinceSQLAndShardCheck(t *testing.T) {
	const shard uint32 = 5 // 5、69、133 都在片 5(64 片)
	ctx := context.Background()

	t.Run("正常翻页", func(t *testing.T) {
		st, fake := newReconcileFakeStore(t, map[string][]fakeRouteReply{"R29": {{rows: [][]driver.Value{
			rcScanRow(5, 5, 1000), rcScanRow(5, 69, 1000), rcScanRow(5, 133, 1200),
		}}}})
		rows, err := st.ScanRoutesUpdatedSince(ctx, shard, RouteUpdatedSince(1000), 3)
		if err != nil {
			t.Fatalf("ScanRoutesUpdatedSince: %v", err)
		}
		var got []RouteUpdatedCursor
		for _, r := range rows {
			got = append(got, RouteUpdatedCursorAt(r))
		}
		want := []RouteUpdatedCursor{{1000, 5}, {1000, 69}, {1200, 133}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("rows = %v, want %v", got, want)
		}
		assertReconcileArgs(t, fake, "R29", uint64(shard), uint64(1000), uint64(1000), uint64(0), int64(3))
	})

	for _, tc := range []struct {
		name      string
		rows      [][]driver.Value
		after     RouteUpdatedCursor
		limit     int
		mismatch  bool
		wantError bool
	}{
		{name: "库里存的片与player_id不符", rows: [][]driver.Value{rcScanRow(5, 6, 1000)}, after: RouteUpdatedSince(0), limit: 10, mismatch: true, wantError: true},
		{name: "别的片的行", rows: [][]driver.Value{rcScanRow(6, 6, 1000)}, after: RouteUpdatedSince(0), limit: 10, mismatch: true, wantError: true},
		{name: "不在游标之后", rows: [][]driver.Value{rcScanRow(5, 69, 999)}, after: RouteUpdatedSince(1000), limit: 10, wantError: true},
		{name: "等于游标", rows: [][]driver.Value{rcScanRow(5, 69, 1000)}, after: RouteUpdatedCursor{UpdatedAtMs: 1000, PlayerID: 69}, limit: 10, wantError: true},
		{name: "不递增", rows: [][]driver.Value{rcScanRow(5, 133, 1200), rcScanRow(5, 5, 1000)}, after: RouteUpdatedSince(0), limit: 10, wantError: true},
		{name: "超过limit", rows: [][]driver.Value{rcScanRow(5, 5, 1000), rcScanRow(5, 69, 1000)}, after: RouteUpdatedSince(0), limit: 1, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, _ := newReconcileFakeStore(t, map[string][]fakeRouteReply{"R29": {{rows: tc.rows}}})
			_, err := st.ScanRoutesUpdatedSince(ctx, shard, tc.after, tc.limit)
			if (err != nil) != tc.wantError || errors.Is(err, ErrRouteShardMismatch) != tc.mismatch {
				t.Fatalf("err = %v, want error=%t shard_mismatch=%t", err, tc.wantError, tc.mismatch)
			}
		})
	}

	t.Run("查询失败", func(t *testing.T) {
		st, _ := newReconcileFakeStore(t, map[string][]fakeRouteReply{"R29": {replyErr(mysqlErrNo(mysqlErrLockWaitTimeout))}})
		if _, err := st.ScanRoutesUpdatedSince(ctx, shard, RouteUpdatedSince(0), 10); err == nil {
			t.Fatal("a query error must surface")
		}
	})

	for _, tc := range []struct {
		name  string
		shard uint32
		limit int
	}{
		{"片越界", placement.RouteRowShards, 10},
		{"limit为0", shard, 0},
		{"limit超上限", shard, RouteBatchLimit + 1},
	} {
		t.Run("拒绝_"+tc.name, func(t *testing.T) {
			st, fake := newReconcileFakeStore(t, nil)
			if _, err := st.ScanRoutesUpdatedSince(ctx, tc.shard, RouteUpdatedSince(0), tc.limit); !errors.Is(err, ErrRouteInvalidArgument) {
				t.Fatalf("err = %v, want ErrRouteInvalidArgument", err)
			}
			if got := fake.labels(); len(got) != 0 {
				t.Fatalf("invalid arguments still sent statements: %v", got)
			}
		})
	}
}

// TestDBNowMs:R30 原样回库时钟;读成 0 或查询失败都报错。
func TestDBNowMs(t *testing.T) {
	st, fake := newReconcileFakeStore(t, map[string][]fakeRouteReply{"R30": {
		{rows: [][]driver.Value{{int64(rcNow)}}}, {rows: [][]driver.Value{{int64(0)}}}, replyErr(mysqlErrNo(mysqlErrLockWaitTimeout)),
	}})
	ctx := context.Background()
	if got, err := st.DBNowMs(ctx); err != nil || got != rcNow {
		t.Fatalf("DBNowMs = (%d, %v), want %d", got, err, rcNow)
	}
	if _, err := st.DBNowMs(ctx); err == nil {
		t.Fatal("a zero database clock must be an error")
	}
	if _, err := st.DBNowMs(ctx); err == nil {
		t.Fatal("a query error must surface")
	}
	assertReconcileLabels(t, fake, "R30", "R30", "R30")
}
