package authoritysql

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	databasepb "proto/common/database"
)

// gateConn 按语句里的表名回固定的一行,不连库:只验闸门把哪种状态判成冻结、哪种判成资格不符。
type gateConn struct {
	meta, slot, registry []driver.Value
	touched              int64
}

func (c *gateConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("不走预处理") }
func (c *gateConn) Close() error                        { return nil }
func (c *gateConn) Begin() (driver.Tx, error)           { return nil, errors.New("不走事务") }
func (c *gateConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(c.touched), nil
}
func (c *gateConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	switch {
	case strings.Contains(query, "route_meta"):
		return &gateRows{row: c.meta}, nil
	case strings.Contains(query, "writer_slot"):
		return &gateRows{row: c.slot}, nil
	case strings.Contains(query, "writer_registry"):
		return &gateRows{row: c.registry}, nil
	}
	return nil, errors.New("意外语句: " + query)
}

type gateRows struct {
	row  []driver.Value
	done bool
}

func (r *gateRows) Columns() []string { return make([]string, len(r.row)) }
func (r *gateRows) Close() error      { return nil }
func (r *gateRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	copy(dest, r.row)
	return nil
}

func gateConfig(primitive databasepb.SqlWriterBootGrant_LockPrimitive) WriterConfig {
	return WriterConfig{boot: &writerBoot{grant: &databasepb.SqlWriterBootGrant{
		WriterKey: bytes.Repeat([]byte{1}, 16), InstanceId: bytes.Repeat([]byte{2}, 16), Generation: "ab",
		AuthoritySchema: "gate_db", ProtectedSchemas: []string{"gate_db"}, SqlPrincipal: []byte("writer@%"),
		RegistrationDigest: bytes.Repeat([]byte{3}, 32), LockPrimitive: primitive,
	}}}
}

func healthyGateConn() *gateConn {
	return &gateConn{
		meta:     []driver.Value{"ab", int64(1), int64(7), int64(1), []byte{}},
		slot:     []driver.Value{"ab", int64(7), int64(1)},
		registry: []driver.Value{"ab", bytes.Repeat([]byte{2}, 16), []byte("writer@%"), bytes.Repeat([]byte{3}, 32), int64(1)},
		touched:  1,
	}
}

func TestGateClassifiesFrozenApartFromDenied(t *testing.T) {
	const (
		admitted = iota
		frozen
		denied
	)
	tests := []struct {
		name      string
		primitive databasepb.SqlWriterBootGrant_LockPrimitive
		change    func(*gateConn)
		want      int
	}{
		{name: "正常放行", primitive: databasepb.SqlWriterBootGrant_SELECT_ONLY, change: func(*gateConn) {}, want: admitted},
		{name: "LOCK_TOUCH 正常放行", primitive: databasepb.SqlWriterBootGrant_LOCK_TOUCH, change: func(*gateConn) {}, want: admitted},
		{name: "进入前就读到 writable=0", primitive: databasepb.SqlWriterBootGrant_SELECT_ONLY, change: func(c *gateConn) { c.meta[1] = int64(0) }, want: frozen},
		{name: "世代不符是资格问题", primitive: databasepb.SqlWriterBootGrant_SELECT_ONLY, change: func(c *gateConn) { c.meta[0] = "cd" }, want: denied},
		{name: "slot 的 epoch 变了", primitive: databasepb.SqlWriterBootGrant_SELECT_ONLY, change: func(c *gateConn) { c.slot[1] = int64(8) }, want: frozen},
		{name: "写者已退休是资格问题", primitive: databasepb.SqlWriterBootGrant_SELECT_ONLY, change: func(c *gateConn) { c.slot[2] = int64(2) }, want: denied},
		{name: "LOCK_TOUCH 条件写没命中、slot 仍 ACTIVE", primitive: databasepb.SqlWriterBootGrant_LOCK_TOUCH, change: func(c *gateConn) { c.touched = 0 }, want: frozen},
		{name: "LOCK_TOUCH 条件写没命中、写者已退休", primitive: databasepb.SqlWriterBootGrant_LOCK_TOUCH, change: func(c *gateConn) { c.touched = 0; c.slot[2] = int64(2) }, want: denied},
		{name: "registry 主体不符是资格问题", primitive: databasepb.SqlWriterBootGrant_SELECT_ONLY, change: func(c *gateConn) { c.registry[2] = []byte("other@%") }, want: denied},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			conn := healthyGateConn()
			test.change(conn)
			gate := runtimeGate{config: gateConfig(test.primitive)}
			attempt, err := gate.beforeBegin(context.Background(), conn)
			if err == nil {
				err = attempt.enter(context.Background(), conn)
			}
			switch test.want {
			case admitted:
				if err != nil {
					t.Fatalf("应放行: %v", err)
				}
			case frozen:
				if !errors.Is(err, ErrWriterFrozen) || !errors.Is(err, ErrWriterDenied) {
					t.Fatalf("应判冻结且能被 ErrWriterDenied 认出: %v", err)
				}
			case denied:
				if !errors.Is(err, ErrWriterDenied) || errors.Is(err, ErrWriterFrozen) {
					t.Fatalf("应判资格不符、不是可重试的冻结: %v", err)
				}
			}
		})
	}
}

// 事务进入期间 writable 被置 0:第一次读到可写,持 slot 之后复核读到冻结。
func TestGateFreezeDuringEnterIsFrozen(t *testing.T) {
	conn := healthyGateConn()
	gate := runtimeGate{config: gateConfig(databasepb.SqlWriterBootGrant_SELECT_ONLY)}
	attempt, err := gate.beforeBegin(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	conn.meta[1] = int64(0)
	if err = attempt.enter(context.Background(), conn); !errors.Is(err, ErrWriterFrozen) {
		t.Fatalf("进入期间冻结: %v", err)
	}
}

// 观察位只认冻结,资格不符不算。
func TestWriterFrozenObserver(t *testing.T) {
	ctx, seen := ObserveWriterFrozen(context.Background())
	NoteWriterFrozen(ctx, ErrWriterDenied)
	if seen() {
		t.Fatal("资格不符置上了观察位")
	}
	NoteWriterFrozen(ctx, ErrWriterFrozen)
	if !seen() {
		t.Fatal("冻结没有置上观察位")
	}
	NoteWriterFrozen(context.Background(), ErrWriterFrozen) // 没带观察位的 ctx 不出错
}

// 结局未知的观察位只认 ErrOutcomeUnknown,和冻结互不影响。
func TestWriterOutcomeUnknownObserver(t *testing.T) {
	ctx, frozen := ObserveWriterFrozen(context.Background())
	NoteWriterOutcomeUnknown(ctx, ErrWriterFrozen)
	if WriterOutcomeUnknownSeen(ctx) {
		t.Fatal("冻结置上了结局未知")
	}
	NoteWriterOutcomeUnknown(ctx, ErrOutcomeUnknown)
	if !WriterOutcomeUnknownSeen(ctx) || frozen() {
		t.Fatalf("unknown=%t frozen=%t", WriterOutcomeUnknownSeen(ctx), frozen())
	}
	NoteWriterOutcomeUnknown(context.Background(), ErrOutcomeUnknown) // 没带观察位的 ctx 不出错
	if WriterOutcomeUnknownSeen(context.Background()) {
		t.Fatal("没带观察位的 ctx 不该读到结局未知")
	}
}

// 「确定没写、重试就行」的观察位和冻结、结局未知互不影响。
func TestNoWriteRetryObserver(t *testing.T) {
	ctx, frozen := ObserveWriterFrozen(context.Background())
	if NoWriteRetrySeen(ctx) {
		t.Fatal("没置过就读到了")
	}
	NoteNoWriteRetry(ctx)
	if !NoWriteRetrySeen(ctx) || frozen() || WriterOutcomeUnknownSeen(ctx) {
		t.Fatalf("retry=%t frozen=%t unknown=%t", NoWriteRetrySeen(ctx), frozen(), WriterOutcomeUnknownSeen(ctx))
	}
	NoteNoWriteRetry(context.Background()) // 没带观察位的 ctx 不出错
	if NoWriteRetrySeen(context.Background()) {
		t.Fatal("没带观察位的 ctx 不该读到")
	}
}

// 「冻结,确定没写」要求错误树的每个叶子都是冻结,混进别的失败就不能按没写处理。结局未知优先。
func TestIsWriterFrozenRequiresEveryLeafFrozen(t *testing.T) {
	wrapped := fmt.Errorf("save: %w", ErrWriterFrozen)
	tests := []struct {
		name    string
		err     error
		frozen  bool
		outcome Outcome
	}{
		{"没有错误", nil, false, OutcomeOK},
		{"冻结本身", ErrWriterFrozen, true, OutcomeFrozen},
		{"包一层", wrapped, true, OutcomeFrozen},
		{"两个都是冻结", errors.Join(ErrWriterFrozen, wrapped), true, OutcomeFrozen},
		{"资格不符", ErrWriterDenied, false, OutcomeFailed},
		{"结局未知", unknown("提交", errors.New("broken pipe")), false, OutcomeUnknown},
		{"冻结混结局未知", errors.Join(wrapped, unknown("提交", nil)), false, OutcomeUnknown},
		{"冻结混别的失败", errors.Join(wrapped, errors.New("rpc failed")), false, OutcomeFailed},
		{"哨兵加冻结的双 %w", fmt.Errorf("%w: %w", errors.New("store unavailable"), ErrWriterFrozen), false, OutcomeFailed},
		{"升级成未知之后认不出冻结", unknown("回滚", ErrWriterFrozen), false, OutcomeUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsWriterFrozen(test.err); got != test.frozen {
				t.Fatalf("IsWriterFrozen=%v, want %v: %v", got, test.frozen, test.err)
			}
			if got := ClassifyOutcome(test.err); got != test.outcome {
				t.Fatalf("ClassifyOutcome=%d, want %d: %v", got, test.outcome, test.err)
			}
		})
	}
}

// 闸门挡回去的错误原样到调用方:一条语句都没发,结局确定,连接不隔离。
func TestFrozenGateErrorStaysDeterminate(t *testing.T) {
	for name, set := range map[string]func(*writerFakeGate){
		"进入事务之前":    func(g *writerFakeGate) { g.beforeErr = ErrWriterFrozen },
		"持 slot 之后": func(g *writerFakeGate) { g.enterErr = ErrWriterFrozen },
	} {
		t.Run(name, func(t *testing.T) {
			w := &writerFakeWire{}
			c := writerFixture(w)
			set(c.gate.(*writerFakeGate))
			_, err := c.ExecContext(context.Background(), "UPDATE t SET n=n+1", nil)
			if !IsWriterFrozen(err) || ClassifyOutcome(err) != OutcomeFrozen {
				t.Fatalf("要能认出冻结: %v", err)
			}
			if w.execs != 0 || w.commits != 0 || !c.IsValid() {
				t.Fatalf("冻结不发业务语句,连接留着: execs=%d commits=%d valid=%v", w.execs, w.commits, c.IsValid())
			}
		})
	}
}

// 冻结之后回滚没确认:连接已隔离,这时不能再说「确定没写」。
func TestFrozenWithUnconfirmedRollbackIsUnknown(t *testing.T) {
	w := &writerFakeWire{rollbackErr: errors.New("connection reset")}
	c := writerFixture(w)
	c.gate.(*writerFakeGate).enterErr = ErrWriterFrozen
	_, err := c.ExecContext(context.Background(), "UPDATE t SET n=n+1", nil)
	if IsWriterFrozen(err) || ClassifyOutcome(err) != OutcomeUnknown || c.IsValid() {
		t.Fatalf("回滚没确认要算结局未知: %v valid=%v", err, c.IsValid())
	}
}

// 启动阶段:这个 boot 还没有事务过过闸门时只读语句放行,否则启动核对撞上窗口就把一次性 grant 烧了。写和显式事务任何时候都不放。
func TestFrozenGateLetsStartupReadsThrough(t *testing.T) {
	ctx := context.Background()
	w := &writerFakeWire{}
	c := writerFixture(w)
	c.gate.(*writerFakeGate).beforeErr = ErrWriterFrozen
	rows, err := c.QueryContext(ctx, "SELECT n FROM t", nil)
	if err != nil {
		t.Fatalf("启动阶段的读要放行: %v", err)
	}
	if err = rows.Close(); err != nil {
		t.Fatal(err)
	}
	if w.queries != 1 || w.begins != 0 || !c.IsValid() {
		t.Fatalf("不开事务直接读: queries=%d begins=%d valid=%v", w.queries, w.begins, c.IsValid())
	}
	if _, err = c.ExecContext(ctx, "UPDATE t SET n=n+1", nil); !IsWriterFrozen(err) {
		t.Fatalf("写不放行: %v", err)
	}
	if _, err = c.BeginTx(ctx, driver.TxOptions{}); !IsWriterFrozen(err) {
		t.Fatalf("显式事务不放行: %v", err)
	}
	if _, err = c.QueryContext(ctx, "DELETE FROM t", nil); !IsWriterFrozen(err) {
		t.Fatalf("不是读语句不放行: %v", err)
	}
	// 第一笔事务过闸之后回到常规语义:读也暂停。
	c.permission.writer.boot.noteAdmitted()
	if _, err = c.QueryContext(ctx, "SELECT n FROM t", nil); !IsWriterFrozen(err) {
		t.Fatalf("过闸之后读也要被挡: %v", err)
	}
	// Open 自己的核对不看阶段:同一个 boot 再开一个池也不能撞闸门。
	rows, err = c.QueryContext(startupProbe(ctx), "SELECT n FROM t", nil)
	if err != nil {
		t.Fatalf("开池核对要放行: %v", err)
	}
	if err = rows.Close(); err != nil {
		t.Fatal(err)
	}
}

// 开池的核对撞上冻结不能报成「实际库不匹配」,带标记时直接过。
func TestVerifyPoolSessionSurvivesFrozenGate(t *testing.T) {
	server := healthyMySQL()
	conn := &backendFakeConn{answer: func(query string) ([]string, []driver.Value, error) {
		if query == "SELECT DATABASE()" {
			return fakeColumns(1), []driver.Value{[]byte("fixture")}, nil
		}
		return server.answer(query)
	}}
	wire := &writerFakeWire{}
	permission := writerUnitPermission()
	permission.writer.boot.noteAdmitted()
	guarded, err := newGuardedConnector(&backendFakeConnector{conn: conn}, &writerFakeGate{wire: wire, beforeErr: ErrWriterFrozen}, writerFakeSession{wire: wire}, mysqlStatementPolicy{}, permission)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(guarded)
	defer db.Close()
	if err = verifyPoolSession(startupProbe(context.Background()), db, "fixture", BackendMySQL); err != nil {
		t.Fatalf("开池核对要过: %v", err)
	}
	if err = verifyPoolSession(startupProbe(context.Background()), db, "other", BackendMySQL); err == nil || !strings.Contains(err.Error(), "实际库不匹配") {
		t.Fatalf("连错库照样拒绝: %v", err)
	}
	if err = verifyPoolSession(context.Background(), db, "fixture", BackendMySQL); !IsWriterFrozen(err) {
		t.Fatalf("没有标记时要原样报冻结,不是库不匹配: %v", err)
	}
}

func TestFrozenGateSeconds(t *testing.T) {
	var g frozenGate
	start := time.Unix(1000, 0)
	if got := g.seconds(start); got != 0 {
		t.Fatalf("没冻结: %v", got)
	}
	g.observe(start)
	g.observe(start.Add(10 * time.Second))
	if got := g.seconds(start.Add(12 * time.Second)); got != 12 {
		t.Fatalf("从第一次撞到算起: %v", got)
	}
	// 解冻之后一直没有新事务:不能一直涨。
	if got := g.seconds(start.Add(10*time.Second + frozenObservationTTL + time.Second)); got != 0 {
		t.Fatalf("超过观察期算已解冻: %v", got)
	}
	// 隔了很久又撞到:算新的一段。
	later := start.Add(time.Hour)
	g.observe(later)
	if got := g.seconds(later.Add(time.Second)); got != 1 {
		t.Fatalf("新的一段: %v", got)
	}
	g.clear()
	if got := g.seconds(later.Add(2 * time.Second)); got != 0 {
		t.Fatalf("过闸之后归零: %v", got)
	}
}

// 闸门挡回去时开始计时,过闸时归零并记下这个 boot 过过闸门。
func TestGateTracksFrozenAndAdmission(t *testing.T) {
	config := gateConfig(databasepb.SqlWriterBootGrant_SELECT_ONLY)
	gate := runtimeGate{config: config}
	frozen := healthyGateConn()
	frozen.meta[1] = int64(0)
	if _, err := gate.beforeBegin(context.Background(), frozen); !IsWriterFrozen(err) {
		t.Fatalf("应判冻结: %v", err)
	}
	if !config.boot.frozen.active.Load() || config.boot.admitted.Load() {
		t.Fatal("撞到冻结要开始计时,不算过过闸门")
	}
	healthy := healthyGateConn()
	attempt, err := gate.beforeBegin(context.Background(), healthy)
	if err != nil {
		t.Fatal(err)
	}
	if err = attempt.enter(context.Background(), healthy); err != nil {
		t.Fatal(err)
	}
	if config.boot.frozen.active.Load() || !config.boot.admitted.Load() {
		t.Fatal("过闸要归零并记下")
	}
}
