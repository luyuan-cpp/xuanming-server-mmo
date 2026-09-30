package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

// PlayerNameStore 的**对外语义**回归:哪种库内情形回哪个 ReserveOutcome / ReleaseOutcome、
// 发几条语句、发的是不是生产那两段 SQL 文本。
//
// 为什么要这一份(而不是只靠真库用例):终局判定是 login 建角路径的契约(Inserted / AlreadyOwned 算成功,
// Taken 要回 owner,Conflict 是 ID 复用事故),而真库用例需要 Docker、默认 skip —— 只有它们守着的话,
// 一次"顺手改写 SQL 或调整分支"在 `go test ./...` 全绿的情况下就能把契约改掉。这里用一个只认本 store 那
// **六条**语句的假驱动把这些组合钉死,不需要任何外部依赖,也不引入新的三方库。
//
// 六条语句 = 两条写(playerNameReserveSQL / playerNameReleaseSQL)+ 四条读(ownedNormOf / ownerOfNorm /
// createdMsOf / BatchGet 的 IN 查询),对应下面五个应答队列(两条写共用 execs,按调用顺序消费)。
//
// 假驱动**不模拟 InnoDB 的锁与唯一约束**:并发下"谁赢"、会不会 1213,只有真库能证(见
// player_name_store_integration_test.go)。这里只证"给定库的应答,本层的判定与语句不变"。

// fakeNameExec 是一次写语句(Reserve 的 ODKU / Release 的 DELETE)的应答。err 非 nil 时 affected 被忽略。
type fakeNameExec struct {
	affected int64
	err      error
}

// fakeNameLookup 是一次等值读的应答。found=false → 驱动返回零行,database/sql 给出 sql.ErrNoRows。
type fakeNameLookup struct {
	value any
	found bool
	err   error
}

// fakeNameStmt 记录一条真的发到驱动的语句,用来断言 SQL 文本**与参数**。
// 参数不是摆设:少了它,Reserve 的 name / norm 两个实参互换、Release 的 (player_id, norm, minCreatedMs)
// 顺序错位,下面十几个表驱动用例照样全绿 —— 而"没有 Docker 也拦得住顺手改写"正是本文件存在的理由。
type fakeNameStmt struct {
	sql  string
	args []driver.Value
}

// fakeNameRow 是 BatchGet 那条 IN 查询返回的一行。
type fakeNameRow struct {
	id   uint64
	name string
}

// fakeNameBatch 是一次 BatchGet 查询的应答。rowsErr 非 nil 时在吐完 rows 之后报错(测 rows.Err() 那条路径)。
type fakeNameBatch struct {
	rows    []fakeNameRow
	err     error
	rowsErr error
}

// fakeNameDB 按脚本应答。五个队列覆盖 store 的六条语句(两条写共用 execs),按调用顺序消费;
// 队列空了就返回错误(而不是给个默认值),免得"脚本写少了"被当成正常路径悄悄通过。
type fakeNameDB struct {
	mu       sync.Mutex
	script   fakeNameScript
	gotExec  []fakeNameStmt
	gotQuery []fakeNameStmt
}

// fakeNameScript 是一次用例给出的应答脚本。刻意与 fakeNameDB 分开:脚本里没有锁,
// 可以直接写进表驱动的用例表并按值拷贝(fakeNameDB 带 sync.Mutex,拷贝它会被 go vet 拦下)。
type fakeNameScript struct {
	execs   []fakeNameExec   // playerNameReserveSQL / playerNameReleaseSQL
	owned   []fakeNameLookup // ownedNormOf:name_norm(string)
	owner   []fakeNameLookup // ownerOfNorm:player_id(uint64)
	created []fakeNameLookup // createdMsOf:created_ms(uint64)
	batch   []fakeNameBatch  // BatchGet:player_id, name(多行)

	// afterFirstExec 在第一条写语句被应答之后调用一次。只给"退避途中 ctx 结束"那个用例用:
	// database/sql 在取连接之前就会检查 ctx,提前取消的话第一条语句根本发不出去,测不到退避这一段。
	afterFirstExec func()
}

var errFakeNameScriptExhausted = errors.New("fake player_name db: 脚本用尽,被测代码发的语句比用例预期的多")

// newFakeNameStore 把假驱动接成一个 PlayerNameStore(同包,直接装字段,生产代码不必为测试开口子),
// 同时把假库还给用例,用来断言"发了几条什么语句"。
//
// releaseSQL 按**新结构**(name_norm 是聚簇主键)装:生产上跑完迁移之后就是这一条。
// 旧结构那一条由 newFakeLegacyNameStore 装,两者只在 SQL 文本上不同,绑参与分支完全共用。
func newFakeNameStore(t *testing.T, script fakeNameScript) (*PlayerNameStore, *fakeNameDB) {
	t.Helper()
	return newFakeNameStoreWithRelease(t, script, playerNameReleaseSQL)
}

// newFakeLegacyNameStore 装旧主键形态(迁移没跑过 / TiDB)那一条 Release 语句。
func newFakeLegacyNameStore(t *testing.T, script fakeNameScript) (*PlayerNameStore, *fakeNameDB) {
	t.Helper()
	st, fake := newFakeNameStoreWithRelease(t, script, playerNameReleaseLegacySQL)
	st.legacyKeyedByPlayerID = true
	return st, fake
}

func newFakeNameStoreWithRelease(t *testing.T, script fakeNameScript, releaseSQL string) (*PlayerNameStore, *fakeNameDB) {
	t.Helper()
	fake := &fakeNameDB{script: script}
	db := sql.OpenDB(fakeNameConnector{fake: fake})
	t.Cleanup(func() { _ = db.Close() })
	return &PlayerNameStore{db: db, releaseSQL: releaseSQL}, fake
}

func (f *fakeNameDB) recordExec(stmt fakeNameStmt) (fakeNameExec, error) {
	f.mu.Lock()
	f.gotExec = append(f.gotExec, stmt)
	first := len(f.gotExec) == 1
	hook := f.script.afterFirstExec
	var next fakeNameExec
	var err error
	if len(f.script.execs) == 0 {
		err = fmt.Errorf("%w: exec %q", errFakeNameScriptExhausted, stmt.sql)
	} else {
		next = f.script.execs[0]
		f.script.execs = f.script.execs[1:]
	}
	f.mu.Unlock()

	if first && hook != nil {
		hook()
	}
	return next, err
}

func (f *fakeNameDB) recordBatch(stmt fakeNameStmt) (fakeNameBatch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotQuery = append(f.gotQuery, stmt)
	if len(f.script.batch) == 0 {
		return fakeNameBatch{}, fmt.Errorf("%w: batch query %q", errFakeNameScriptExhausted, stmt.sql)
	}
	next := f.script.batch[0]
	f.script.batch = f.script.batch[1:]
	return next, nil
}

func (f *fakeNameDB) recordQuery(stmt fakeNameStmt, queue *[]fakeNameLookup) (fakeNameLookup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotQuery = append(f.gotQuery, stmt)
	if len(*queue) == 0 {
		return fakeNameLookup{}, fmt.Errorf("%w: query %q", errFakeNameScriptExhausted, stmt.sql)
	}
	next := (*queue)[0]
	*queue = (*queue)[1:]
	return next, nil
}

// execCount / queryCount / execSQLs 给断言用。
func (f *fakeNameDB) execCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.gotExec)
}

func (f *fakeNameDB) queryCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.gotQuery)
}

func (f *fakeNameDB) execAt(i int) fakeNameStmt {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotExec[i]
}

func (f *fakeNameDB) queryAt(i int) fakeNameStmt {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotQuery[i]
}

// ── 驱动实现(只够跑本 store 的六条语句)──────────────────────────────────────────

type fakeNameConnector struct{ fake *fakeNameDB }

func (c fakeNameConnector) Connect(context.Context) (driver.Conn, error) {
	return &fakeNameConn{fake: c.fake}, nil
}

func (c fakeNameConnector) Driver() driver.Driver { return fakeNameDriver{} }

type fakeNameDriver struct{}

func (fakeNameDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("fake player_name db: 只支持 sql.OpenDB(connector)")
}

type fakeNameConn struct{ fake *fakeNameDB }

// Prepare / Begin 不会被用到:本 store 只走 ExecContext / QueryRowContext,
// 而 database/sql 对实现了 ExecerContext / QueryerContext 的连接不再走 Prepare。
func (c *fakeNameConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("fake player_name db: 不支持 Prepare")
}

func (c *fakeNameConn) Close() error { return nil }

func (c *fakeNameConn) Begin() (driver.Tx, error) {
	return nil, errors.New("fake player_name db: 不支持事务(本 store 不开显式事务)")
}

func (c *fakeNameConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	stmt := fakeNameStmt{sql: query, args: namedToValues(args)}
	next, err := c.fake.recordExec(stmt)
	if err != nil {
		return nil, err
	}
	if next.err != nil {
		return nil, next.err
	}
	return driver.RowsAffected(next.affected), nil
}

func (c *fakeNameConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	stmt := fakeNameStmt{sql: query, args: namedToValues(args)}

	// BatchGet 的 IN 查询排在最前面分流:它的前缀是 "SELECT player_id, name FROM player_name",
	// 与 ownerOfNorm 的 "SELECT player_id FROM player_name" 不互为前缀(逗号 vs 空格),但先判它更不容易看错。
	if strings.HasPrefix(query, "SELECT player_id, name FROM player_name") {
		next, err := c.fake.recordBatch(stmt)
		if err != nil {
			return nil, err
		}
		if next.err != nil {
			return nil, next.err
		}
		rows := &fakeNameRows{cols: []string{"player_id", "name"}, rowsErr: next.rowsErr}
		for _, r := range next.rows {
			rows.rows = append(rows.rows, []driver.Value{int64(r.id), r.name})
		}
		return rows, nil
	}

	var queue *[]fakeNameLookup
	var column string
	switch {
	case strings.HasPrefix(query, "SELECT name_norm FROM player_name"):
		queue, column = &c.fake.script.owned, "name_norm"
	case strings.HasPrefix(query, "SELECT player_id FROM player_name"):
		queue, column = &c.fake.script.owner, "player_id"
	case strings.HasPrefix(query, "SELECT created_ms FROM player_name"):
		queue, column = &c.fake.script.created, "created_ms"
	default:
		return nil, fmt.Errorf("fake player_name db: 未预期的查询 %q", query)
	}

	next, err := c.fake.recordQuery(stmt, queue)
	if err != nil {
		return nil, err
	}
	if next.err != nil {
		return nil, next.err
	}
	rows := &fakeNameRows{cols: []string{column}}
	if next.found {
		val, err := toDriverValue(next.value)
		if err != nil {
			return nil, err
		}
		rows.rows = [][]driver.Value{{val}}
	}
	return rows, nil
}

// toDriverValue 把脚本里的值转成 driver.Value 允许的类型(uint64 走 int64,用例里的 id 都远小于 2^63)。
func toDriverValue(v any) (driver.Value, error) {
	switch typed := v.(type) {
	case string:
		return typed, nil
	case uint64:
		return int64(typed), nil
	default:
		return nil, fmt.Errorf("fake player_name db: 脚本值类型 %T 不支持", v)
	}
}

func namedToValues(args []driver.NamedValue) []driver.Value {
	out := make([]driver.Value, 0, len(args))
	for _, a := range args {
		out = append(out, a.Value)
	}
	return out
}

// fakeNameRows 吐 rows 里的每一行,吐完之后返回 rowsErr(nil 时是 io.EOF)。
// rowsErr 这一支专门测 database/sql 的 rows.Err():只有它能区分"读完了"和"读一半断了"。
type fakeNameRows struct {
	cols    []string
	rows    [][]driver.Value
	rowsErr error
}

func (r *fakeNameRows) Columns() []string { return r.cols }

func (r *fakeNameRows) Close() error { return nil }

func (r *fakeNameRows) Next(dest []driver.Value) error {
	if len(r.rows) == 0 {
		if r.rowsErr != nil {
			return r.rowsErr
		}
		return io.EOF
	}
	copy(dest, r.rows[0])
	r.rows = r.rows[1:]
	return nil
}

// ── Reserve 的终局契约 ──────────────────────────────────────────────────────────

// TestReserveOutcomeContract 钉住"库回什么 → Reserve 回什么",一格都不许动:
// Inserted / AlreadyOwned 是成功(login 按成功继续建角),Taken 必须带出 owner(login 靠它识别自己的重试),
// Conflict 是 ID 复用事故(调用方必须当故障,不能删旧行)。
func TestReserveOutcomeContract(t *testing.T) {
	const (
		player uint64 = 4242
		other  uint64 = 9001
		norm          = "yunzhongjun"
	)

	for _, tc := range []struct {
		name        string
		script      fakeNameScript
		wantOutcome ReserveOutcome
		wantOwner   uint64
		wantErr     bool
		wantExecs   int
		wantQueries int
	}{
		{
			name:        "插入成功_1行",
			script:      fakeNameScript{execs: []fakeNameExec{{affected: 1}}},
			wantOutcome: ReserveInserted,
			wantExecs:   1,
			// 插入成功不再问任何人:两条等值读只在撞键之后才发。
			wantQueries: 0,
		},
		{
			name: "撞键_同player同名字_幂等成功",
			script: fakeNameScript{
				execs: []fakeNameExec{{affected: 0}},
				owned: []fakeNameLookup{{value: norm, found: true}},
			},
			wantOutcome: ReserveAlreadyOwned,
			wantExecs:   1,
			wantQueries: 1,
		},
		{
			name: "撞键_同player已有别的名字_ID复用事故",
			script: fakeNameScript{
				execs: []fakeNameExec{{affected: 0}},
				owned: []fakeNameLookup{{value: "laomingzi", found: true}},
			},
			wantOutcome: ReserveConflict,
			wantExecs:   1,
			// 只问第一条:撞的是主键,不必再问名字归谁。
			wantQueries: 1,
		},
		{
			name: "撞键_名字被别人占_回出owner",
			script: fakeNameScript{
				execs: []fakeNameExec{{affected: 0}},
				owned: []fakeNameLookup{{found: false}},
				owner: []fakeNameLookup{{value: other, found: true}},
			},
			wantOutcome: ReserveTaken,
			wantOwner:   other,
			wantExecs:   1,
			wantQueries: 2,
		},
		{
			name: "撞键后行已消失_下一轮重插成功",
			script: fakeNameScript{
				execs: []fakeNameExec{{affected: 0}, {affected: 1}},
				owned: []fakeNameLookup{{found: false}},
				owner: []fakeNameLookup{{found: false}},
			},
			wantOutcome: ReserveInserted,
			wantExecs:   2,
			wantQueries: 2,
		},
		{
			name: "1062也走撞键分辨_纵深防御",
			script: fakeNameScript{
				execs: []fakeNameExec{{err: mysqlErrNo(mysqlErrDupEntry)}},
				owned: []fakeNameLookup{{value: norm, found: true}},
			},
			wantOutcome: ReserveAlreadyOwned,
			wantExecs:   1,
			wantQueries: 1,
		},
		{
			name: "1213_退避后重来_成功",
			script: fakeNameScript{
				execs: []fakeNameExec{{err: mysqlErrNo(mysqlErrDeadlock)}, {affected: 1}},
			},
			wantOutcome: ReserveInserted,
			wantExecs:   2,
			wantQueries: 0,
		},
		{
			name: "1205_退避后重来_撞键成Taken",
			script: fakeNameScript{
				execs: []fakeNameExec{{err: mysqlErrNo(mysqlErrLockWaitTimeout)}, {affected: 0}},
				owned: []fakeNameLookup{{found: false}},
				owner: []fakeNameLookup{{value: other, found: true}},
			},
			wantOutcome: ReserveTaken,
			wantOwner:   other,
			wantExecs:   2,
			wantQueries: 2,
		},
		{
			name: "1213_用尽预算_按存储错误返回",
			script: fakeNameScript{
				execs: []fakeNameExec{
					{err: mysqlErrNo(mysqlErrDeadlock)},
					{err: mysqlErrNo(mysqlErrDeadlock)},
					{err: mysqlErrNo(mysqlErrDeadlock)},
				},
			},
			wantErr:   true,
			wantExecs: playerNameReserveAttempts,
		},
		{
			name:      "非锁错误_立刻返回不重试",
			script:    fakeNameScript{execs: []fakeNameExec{{err: mysqlErrNo(1064)}}},
			wantErr:   true,
			wantExecs: 1,
		},
		{
			name:      "受影响2行_fail_closed",
			script:    fakeNameScript{execs: []fakeNameExec{{affected: 2}}},
			wantErr:   true,
			wantExecs: 1,
		},
		{
			name: "等值读失败_原样返回错误",
			script: fakeNameScript{
				execs: []fakeNameExec{{affected: 0}},
				owned: []fakeNameLookup{{err: mysqlErrNo(1146)}},
			},
			wantErr:     true,
			wantExecs:   1,
			wantQueries: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, fake := newFakeNameStore(t, tc.script)

			outcome, owner, err := st.Reserve(context.Background(), player, "YunZhongJun", norm, 1700000000000)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got outcome=%s owner=%d", outcome, owner)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if outcome != tc.wantOutcome {
					t.Fatalf("outcome = %s, want %s", outcome, tc.wantOutcome)
				}
				if owner != tc.wantOwner {
					t.Fatalf("owner = %d, want %d(只有 Taken 才给 owner,其余必须是 0)", owner, tc.wantOwner)
				}
			}
			if got := fake.execCount(); got != tc.wantExecs {
				t.Fatalf("发出 %d 条写语句,want %d", got, tc.wantExecs)
			}
			if got := fake.queryCount(); got != tc.wantQueries {
				t.Fatalf("发出 %d 条等值读,want %d", got, tc.wantQueries)
			}
			// 实参顺序必须是 (player_id, name, name_norm, created_ms)。name 与 name_norm 互换在这里必须变红:
			// 互换之后库里存的展示名是归一化串、判重键是原串,唯一性口径整个错位,而 SQL 文本一个字都没变。
			// uint64 经 driver.DefaultParameterConverter 落成 int64,所以期望值也写 int64。
			wantArgs := []driver.Value{int64(player), "YunZhongJun", norm, int64(1700000000000)}
			for i := 0; i < fake.execCount(); i++ {
				stmt := fake.execAt(i)
				if stmt.sql != playerNameReserveSQL {
					t.Fatalf("第 %d 条写语句不是 playerNameReserveSQL:\n got=%q\nwant=%q", i, stmt.sql, playerNameReserveSQL)
				}
				if !reflect.DeepEqual(stmt.args, wantArgs) {
					t.Fatalf("第 %d 条写语句的实参不对:\n got=%#v\nwant=%#v(顺序必须是 player_id, name, name_norm, created_ms)",
						i, stmt.args, wantArgs)
				}
			}
		})
	}
}

// TestReserveRejectsInvalidArgumentsBeforeAnyStatement:入参非法时一条语句都不许发
// (player_id=0 一旦写进去就是一行永远没有主人、也没人释放、却占着名字的行)。
func TestReserveRejectsInvalidArgumentsBeforeAnyStatement(t *testing.T) {
	for _, tc := range []struct {
		name   string
		player uint64
		norm   string
	}{
		{"player_id为0", 0, "abc"},
		{"name_norm为空", 42, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, fake := newFakeNameStore(t, fakeNameScript{})
			if _, _, err := st.Reserve(context.Background(), tc.player, "Abc", tc.norm, 1); !errors.Is(err, ErrPlayerNameInvalidArgument) {
				t.Fatalf("want ErrPlayerNameInvalidArgument, got %v", err)
			}
			if fake.execCount() != 0 || fake.queryCount() != 0 {
				t.Fatalf("入参非法却发了语句:exec=%d query=%d", fake.execCount(), fake.queryCount())
			}
		})
	}
}

// TestReserveLockRetriesCounted:1213/1205 的重来次数必须记进 lockRetries —— 真库用例靠它区分
// "写法真的不成环"与"环被重试悄悄吸收了"(见 Reserve 的「残余」)。
func TestReserveLockRetriesCounted(t *testing.T) {
	st, _ := newFakeNameStore(t, fakeNameScript{execs: []fakeNameExec{{err: mysqlErrNo(mysqlErrDeadlock)}, {affected: 1}}})
	if _, _, err := st.Reserve(context.Background(), 7, "A", "a", 1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := st.LockRetriesForTest(); got != 1 {
		t.Fatalf("lockRetries = %d, want 1", got)
	}

	// "撞键后行已消失"那一轮不是锁冲突,不许计数、也不许退避。
	script2 := fakeNameScript{
		execs: []fakeNameExec{{affected: 0}, {affected: 1}},
		owned: []fakeNameLookup{{found: false}},
		owner: []fakeNameLookup{{found: false}},
	}
	st2, _ := newFakeNameStore(t, script2)
	if _, _, err := st2.Reserve(context.Background(), 7, "A", "a", 1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := st2.LockRetriesForTest(); got != 0 {
		t.Fatalf("lockRetries = %d, want 0(并发 Release 把行删了不是锁冲突)", got)
	}
}

// TestReserveLockRetryStopsOnCanceledContext:预算还在(1213 之后还能重来两次)但 ctx 已结束时,
// 退避必须立刻返回、不再发第二条语句 —— 否则重试会吃掉调用方剩下的全部时间预算,还去撞一条注定超时的语句。
func TestReserveLockRetryStopsOnCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, fake := newFakeNameStore(t, fakeNameScript{
		// 第二条脚本刻意留着:真的重来了就能看出来(exec 数会变成 2)。
		execs:          []fakeNameExec{{err: mysqlErrNo(mysqlErrDeadlock)}, {affected: 1}},
		afterFirstExec: cancel,
	})
	if _, _, err := st.Reserve(ctx, 7, "A", "a", 1); err == nil {
		t.Fatal("ctx 已取消却返回成功")
	}
	if got := fake.execCount(); got != 1 {
		t.Fatalf("ctx 取消后仍在重试:exec=%d,want 1", got)
	}
	if got := st.LockRetriesForTest(); got != 1 {
		t.Fatalf("lockRetries = %d, want 1(这一次锁冲突要计数,只是退避被 ctx 打断)", got)
	}
}

// ── Release 的终局契约 ──────────────────────────────────────────────────────────

// TestReleaseOutcomeContract 钉住"删了几行 + 行的登记时刻 → 哪个 ReleaseOutcome":
// Deleted / Absent 都是成功(幂等),OutsideWindow 必须是拒绝(它拦的是"删别人在役角色的名字")。
//
// 两种表形态各跑一遍:终局判定、语句条数、实参顺序**都不许因为形态不同而变**,
// 变的只有 SQL 文本(新结构钉 PRIMARY、旧结构钉 uk_player_name)。这一条正是"改主键不改对外语义"的机械证据。
func TestReleaseOutcomeContract(t *testing.T) {
	t.Run("新结构_主键是name_norm", func(t *testing.T) {
		runReleaseOutcomeContract(t, newFakeNameStore, playerNameReleaseSQL)
	})
	t.Run("旧结构_主键还是player_id", func(t *testing.T) {
		runReleaseOutcomeContract(t, newFakeLegacyNameStore, playerNameReleaseLegacySQL)
	})
}

func runReleaseOutcomeContract(
	t *testing.T,
	newStore func(*testing.T, fakeNameScript) (*PlayerNameStore, *fakeNameDB),
	wantSQL string,
) {
	t.Helper()
	const (
		player       uint64 = 4242
		norm                = "yunzhongjun"
		minCreatedMs uint64 = 1_700_000_000_000
	)

	for _, tc := range []struct {
		name         string
		minCreatedMs uint64
		script       fakeNameScript
		wantOutcome  ReleaseOutcome
		wantErr      bool
		wantExecs    int
		wantQueries  int
	}{
		{
			name:         "删到了",
			minCreatedMs: minCreatedMs,
			script:       fakeNameScript{execs: []fakeNameExec{{affected: 1}}},
			wantOutcome:  ReleaseDeleted,
			wantExecs:    1,
		},
		{
			name:         "不限时间_没删到即行不存在_幂等成功",
			minCreatedMs: 0,
			script:       fakeNameScript{execs: []fakeNameExec{{affected: 0}}},
			wantOutcome:  ReleaseAbsent,
			wantExecs:    1,
			// minCreatedMs=0 时时间条件恒真,"没删到"只能是行不存在,不必再探一次。
			wantQueries: 0,
		},
		{
			name:         "限时间_行不存在_幂等成功",
			minCreatedMs: minCreatedMs,
			script: fakeNameScript{
				execs:   []fakeNameExec{{affected: 0}},
				created: []fakeNameLookup{{found: false}},
			},
			wantOutcome: ReleaseAbsent,
			wantExecs:   1,
			wantQueries: 1,
		},
		{
			name:         "限时间_行太老_拒绝",
			minCreatedMs: minCreatedMs,
			script: fakeNameScript{
				execs:   []fakeNameExec{{affected: 0}},
				created: []fakeNameLookup{{value: minCreatedMs - 1, found: true}},
			},
			wantOutcome: ReleaseOutsideWindow,
			wantExecs:   1,
			wantQueries: 1,
		},
		{
			name:         "限时间_行在窗口内却没删到_重删",
			minCreatedMs: minCreatedMs,
			script: fakeNameScript{
				execs:   []fakeNameExec{{affected: 0}, {affected: 1}},
				created: []fakeNameLookup{{value: minCreatedMs, found: true}},
			},
			wantOutcome: ReleaseDeleted,
			wantExecs:   2,
			wantQueries: 1,
		},
		{
			name:         "1213_退避后重来_删到",
			minCreatedMs: minCreatedMs,
			script: fakeNameScript{
				execs: []fakeNameExec{{err: mysqlErrNo(mysqlErrDeadlock)}, {affected: 1}},
			},
			wantOutcome: ReleaseDeleted,
			wantExecs:   2,
		},
		{
			name:         "1213_用尽预算_按存储错误返回",
			minCreatedMs: minCreatedMs,
			script: fakeNameScript{
				execs: []fakeNameExec{
					{err: mysqlErrNo(mysqlErrDeadlock)},
					{err: mysqlErrNo(mysqlErrDeadlock)},
					{err: mysqlErrNo(mysqlErrDeadlock)},
				},
			},
			wantErr:   true,
			wantExecs: playerNameReleaseAttempts,
		},
		{
			name:         "非锁错误_立刻返回不重试",
			minCreatedMs: minCreatedMs,
			script:       fakeNameScript{execs: []fakeNameExec{{err: mysqlErrNo(1064)}}},
			wantErr:      true,
			wantExecs:    1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, fake := newStore(t, tc.script)

			outcome, err := st.Release(context.Background(), player, norm, tc.minCreatedMs)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got outcome=%s", outcome)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if outcome != tc.wantOutcome {
					t.Fatalf("outcome = %s, want %s", outcome, tc.wantOutcome)
				}
			}
			if got := fake.execCount(); got != tc.wantExecs {
				t.Fatalf("发出 %d 条 DELETE,want %d", got, tc.wantExecs)
			}
			if got := fake.queryCount(); got != tc.wantQueries {
				t.Fatalf("发出 %d 条探测读,want %d", got, tc.wantQueries)
			}
			// 实参顺序必须是 (player_id, name_norm, min_created_ms)。错位之后"删谁"与"时间窗下界"会互换,
			// 而 SQL 文本一个字都没变 —— 这是本文件要拦的那一类改动。
			wantArgs := []driver.Value{int64(player), norm, int64(tc.minCreatedMs)}
			for i := 0; i < fake.execCount(); i++ {
				stmt := fake.execAt(i)
				if stmt.sql != wantSQL {
					t.Fatalf("第 %d 条 DELETE 不是本形态该用的语句:\n got=%q\nwant=%q", i, stmt.sql, wantSQL)
				}
				if !reflect.DeepEqual(stmt.args, wantArgs) {
					t.Fatalf("第 %d 条 DELETE 的实参不对:\n got=%#v\nwant=%#v(顺序必须是 player_id, name_norm, min_created_ms)",
						i, stmt.args, wantArgs)
				}
			}
		})
	}
}

// TestReleaseRejectsInvalidArguments:同 Reserve,入参非法一条语句都不发。
func TestReleaseRejectsInvalidArguments(t *testing.T) {
	for _, tc := range []struct {
		name   string
		player uint64
		norm   string
	}{
		{"player_id为0", 0, "abc"},
		{"name_norm为空", 42, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, fake := newFakeNameStore(t, fakeNameScript{})
			if _, err := st.Release(context.Background(), tc.player, tc.norm, 0); !errors.Is(err, ErrPlayerNameInvalidArgument) {
				t.Fatalf("want ErrPlayerNameInvalidArgument, got %v", err)
			}
			if fake.execCount() != 0 || fake.queryCount() != 0 {
				t.Fatalf("入参非法却发了语句:exec=%d query=%d", fake.execCount(), fake.queryCount())
			}
		})
	}
}

// TestBatchGetEarlyReturnsIssueNoQuery:空入参、全 0 入参、超限入参都不该碰库。
func TestBatchGetEarlyReturnsIssueNoQuery(t *testing.T) {
	st, fake := newFakeNameStore(t, fakeNameScript{})
	ctx := context.Background()

	got, err := st.BatchGet(ctx, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("空入参:got=%v err=%v", got, err)
	}
	got, err = st.BatchGet(ctx, []uint64{0, 0})
	if err != nil || len(got) != 0 {
		t.Fatalf("全 0 入参应被跳过且不算错误:got=%v err=%v", got, err)
	}
	if _, err := st.BatchGet(ctx, make([]uint64, PlayerNameBatchLimit+1)); !errors.Is(err, ErrPlayerNameBatchTooLarge) {
		t.Fatalf("超限入参:want ErrPlayerNameBatchTooLarge, got %v", err)
	}
	if fake.queryCount() != 0 {
		t.Fatalf("提前返回的三种入参却发了 %d 条查询", fake.queryCount())
	}
}

// TestBatchGetFillsMapAndSkipsZeroIDs 覆盖 BatchGet 真正走库那一段(上一个用例只覆盖三条提前返回):
// 占位符与实参只为非 0 的 id 生成,返回的每一行都进 map,缺席的 id **不出现**在结果里(读侧 fail-open 的契约)。
func TestBatchGetFillsMapAndSkipsZeroIDs(t *testing.T) {
	st, fake := newFakeNameStore(t, fakeNameScript{
		batch: []fakeNameBatch{{rows: []fakeNameRow{
			{id: 7, name: "QiHao"},
			{id: 9, name: "JiuHao"},
		}}},
	})

	// 中间夹一个 0:它既不该进占位符,也不该进实参,更不该让整批失败。
	got, err := st.BatchGet(context.Background(), []uint64{7, 0, 9, 11})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[uint64]string{7: "QiHao", 9: "JiuHao"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BatchGet = %#v, want %#v(11 没有行,必须缺席而不是空串)", got, want)
	}

	if fake.queryCount() != 1 {
		t.Fatalf("发出 %d 条查询,want 1", fake.queryCount())
	}
	stmt := fake.queryAt(0)
	const wantSQL = "SELECT player_id, name FROM player_name WHERE player_id IN (?,?,?)"
	if stmt.sql != wantSQL {
		t.Fatalf("BatchGet 的语句不对(0 应被跳过,占位符只留 3 个):\n got=%q\nwant=%q", stmt.sql, wantSQL)
	}
	wantArgs := []driver.Value{int64(7), int64(9), int64(11)}
	if !reflect.DeepEqual(stmt.args, wantArgs) {
		t.Fatalf("BatchGet 的实参不对:\n got=%#v\nwant=%#v(0 不许进实参)", stmt.args, wantArgs)
	}
}

// TestBatchGetReportsRowsErr:读到一半断了必须报错,不能把"只读到前几行"当成完整结果返回。
// 少了 rows.Err() 这一步,一次网络中断会让调用方拿到一份**静默残缺**的名字表,而缺席在本契约里
// 恰好是合法的(缺席 = 没有这个名字),于是永远不会有人发现。
func TestBatchGetReportsRowsErr(t *testing.T) {
	boom := errors.New("connection reset mid-stream")
	st, _ := newFakeNameStore(t, fakeNameScript{
		batch: []fakeNameBatch{{rows: []fakeNameRow{{id: 7, name: "QiHao"}}, rowsErr: boom}},
	})

	got, err := st.BatchGet(context.Background(), []uint64{7, 9})
	if err == nil {
		t.Fatalf("行迭代出错却返回成功:got=%#v", got)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("错误没有裹住原因:%v", err)
	}
}

// ── 两段 SQL 文本的形状 ─────────────────────────────────────────────────────────

// TestReserveSQLUpdateClauseIsNoOp 钉住 Reserve 判定口径的**另一半**前提(见 Reserve 的「写法」):
// ODKU 的更新子句必须是 no-op(`player_id = player_id`,引用已有行自己的列值)。一旦它真改了数据,
// 受影响行数会变成 2,"0=撞键 / 1=插入"的口径就失效;若改成刷 created_ms,还会把释放窗口整体推后。
func TestReserveSQLUpdateClauseIsNoOp(t *testing.T) {
	const marker = "ON DUPLICATE KEY UPDATE"
	idx := strings.Index(playerNameReserveSQL, marker)
	if idx < 0 {
		t.Fatalf("playerNameReserveSQL 不再是 ODKU:%q —— 改回普通 INSERT 会把**聚簇索引**上的重复检查从 X 退回 S,"+
			"两个 S 同时被授予之后各自要升级成 X、互相挡住(手册 E1 的三会话例就建在聚簇主键上)。"+
			"2026-09-29 真库探针实测:新结构配普通 INSERT 是 15/15 全成环,三种 player_id 顺序一个都跑不掉 —— "+
			"「名字是聚簇主键」与「ODKU」两个条件缺一不可", playerNameReserveSQL)
	}
	update := strings.TrimSpace(playerNameReserveSQL[idx+len(marker):])
	if update != "player_id = player_id" {
		t.Fatalf("ODKU 的更新子句是 %q,必须保持 no-op 的 `player_id = player_id`", update)
	}
	if strings.Contains(playerNameReserveSQL, "VALUES(") {
		t.Fatal("更新子句引用了 VALUES():那会真的改数据,受影响行数变 2,Reserve 的行数口径失效")
	}
}

// TestReleaseSQLForcesIndexPerTableShape 钉住 Release 的取锁顺序(见 Release 的「锁序」):
// 两条语句各自把索引钉死,否则优化器可能挑另一条唯一约束,与 Reserve 的 ODKU 反序取锁,两方即可成环。
// 真库用例用 EXPLAIN 再验一次执行计划;这一条在没有 Docker 时也能拦住文本被改回去。
func TestReleaseSQLForcesIndexPerTableShape(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stmt  string
		index string
		why   string
	}{
		{
			name:  "新结构钉PRIMARY",
			stmt:  playerNameReleaseSQL,
			index: playerNameReleaseForcedIndex,
			why: "新结构里 name_norm 是聚簇主键,Reserve 的 ODKU 先锁它;Release 不钉 PRIMARY 时优化器可能挑 " +
				"uk_player_name_owner(player_id),那就是先二级、后聚簇,与 Reserve 反序",
		},
		{
			name:  "旧结构钉uk_player_name",
			stmt:  playerNameReleaseLegacySQL,
			index: PlayerNameLegacyNormUniqueKey,
			why:   "旧结构里名字是二级唯一索引,Reserve 撞名时先锁它;不钉时优化器会挑主键 player_id,反序",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.stmt, "FORCE INDEX ("+tc.index+")") {
				t.Fatalf("丢了 FORCE INDEX (%s):%q —— %s", tc.index, tc.stmt, tc.why)
			}
			if !strings.HasPrefix(tc.stmt, "DELETE player_name FROM player_name") {
				t.Fatalf("不再是多表 DELETE 语法(单表 DELETE 不接受索引提示):%q", tc.stmt)
			}
			for _, col := range []string{"player_id = ?", "name_norm = ?", "created_ms >= ?"} {
				if !strings.Contains(tc.stmt, col) {
					t.Fatalf("丢了条件 %q:%q", col, tc.stmt)
				}
			}
		})
	}
}

// TestReleaseStatementsShareArgumentOrder:两条 Release 语句由**同一段**绑参代码发出
// (Release 里的 s.releaseSQL),占位符顺序一旦分叉,就是"删谁"与"时间窗下界"错位 ——
// 删的还是一行、还是没有语法错误,只是删错了人,而且全程零报错。
//
// 判据取"带占位符的条件按在语句里出现的先后排出来,两条必须一模一样",比数一数占位符个数强:
// 它连"个数对、顺序也对,但某个条件换了一个列"都能拦住。
func TestReleaseStatementsShareArgumentOrder(t *testing.T) {
	got, want := releaseConditionOrder(playerNameReleaseLegacySQL), releaseConditionOrder(playerNameReleaseSQL)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("旧结构 Release 的条件顺序 %v 与新结构 %v 不同:两条语句共用同一段绑参代码(Release 里的 s.releaseSQL),"+
			"顺序分叉 = 实参错位,删掉的是别人的名字、或用错了时间窗下界,而且不会报错", got, want)
	}
	if len(want) != 3 {
		t.Fatalf("Release 的带占位符条件只认出 %v(want 三条:player_id / name_norm / created_ms):"+
			"条件写法被改过,本用例已经拦不住实参错位了,先修判据", want)
	}
	for _, stmt := range []string{playerNameReleaseSQL, playerNameReleaseLegacySQL} {
		if n := strings.Count(stmt, "?"); n != 3 {
			t.Fatalf("Release 语句的占位符是 %d 个,want 3:%q", n, stmt)
		}
	}
}

// releaseConditionOrder 把 Release 语句里带占位符的条件按出现先后列出来 —— 也就是实参的绑定顺序。
func releaseConditionOrder(stmt string) []string {
	type placed struct {
		at  int
		col string
	}
	var found []placed
	for _, col := range []string{"player_id = ?", "name_norm = ?", "created_ms >= ?"} {
		if i := strings.Index(stmt, col); i >= 0 {
			found = append(found, placed{at: i, col: col})
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].at < found[j].at })
	out := make([]string, 0, len(found))
	for _, f := range found {
		out = append(out, f.col)
	}
	return out
}

// TestBootstrapDDLMatchesStoreAssumptions 把 schema.go 的建表语句与本 store 的假设钉在一起。
//
// 这两份真相分处两个文件:store 假定"名字是聚簇主键、player_id 上另有一条唯一键",
// 而真正建表的是 schema.go 的 playerNameBootstrapDDL。漂移的症状分两种,都不好查 ——
// 主键漂回 player_id:NewPlayerNameStore 会探到旧形态并静默退回旧语句(功能对,但死锁根治没了);
// player_id 的唯一键没了:Reserve 的 ID 复用检测与 ownedNormOf 一起静默失效。
// 本用例不连库,是这两处漂移在 `go test ./...` 里唯一的拦截点。
func TestBootstrapDDLMatchesStoreAssumptions(t *testing.T) {
	if !strings.Contains(playerNameBootstrapDDL, "PRIMARY KEY (`name_norm`)") {
		t.Fatalf("playerNameBootstrapDDL 的主键不是 name_norm:%q —— 名字抢注的死锁根治(真库实测 0/15)"+
			"的全部前提就是「名字是聚簇主键」,改回二级唯一索引会让生产常态那一路重新 5/5 成环", playerNameBootstrapDDL)
	}
	if !strings.Contains(playerNameBootstrapDDL, "UNIQUE KEY `"+PlayerNameOwnerUniqueKey+"` (`player_id`)") {
		t.Fatalf("playerNameBootstrapDDL 里没有 UNIQUE KEY %s(player_id):"+
			"Reserve 靠它把「这个 player_id 已经有别的名字」变成受影响 0 行(→ ReserveConflict),"+
			"ownedNormOf 也靠它保证最多一行。键没了这两处都会静默失效", PlayerNameOwnerUniqueKey)
	}
	if strings.Contains(playerNameBootstrapDDL, "`"+PlayerNameLegacyNormUniqueKey+"`") {
		t.Fatalf("playerNameBootstrapDDL 里又出现了 %s:name_norm 上**不能**再有二级唯一索引 —— "+
			"ODKU 的重复扫描会连带在它上面取 X next-key,把根治掉的那条环原样带回来",
			PlayerNameLegacyNormUniqueKey)
	}
	if !strings.Contains(playerNameBootstrapDDL, "COLLATE=utf8mb4_bin") {
		t.Fatalf("playerNameBootstrapDDL 丢了 utf8mb4_bin:归一化在 Go 侧做,库必须逐字节比较;" +
			"主键建在 name_norm 上之后,collation 同时决定「库认为哪两个名字是同一个主键」")
	}
	// player_id 也必须进启动期的唯一性守卫,否则上面那条 UNIQUE KEY 被人从存量库上 DROP 掉时没人会发现。
	var guarded bool
	for _, col := range bootstrapUniqueColumns[PlayerNameTableName] {
		if col == "player_id" {
			guarded = true
		}
	}
	if !guarded {
		t.Fatalf("bootstrapUniqueColumns[%s] = %v,没把 player_id 列进去:"+
			"存量库上 %s 被 DROP 之后启动检查照样全过,而 Reserve 的 ID 复用检测已经静默失效",
			PlayerNameTableName, bootstrapUniqueColumns[PlayerNameTableName], PlayerNameOwnerUniqueKey)
	}
}

// TestStrayNameNormUniqueIndexNames 钉住"哪些索引算 name_norm 上多余的唯一键"这条判据。
//
// 它同时被两条**相反**的路径用(见 strayNameNormUniqueIndexNames 的注释):旧形态下这些索引承担着
// 名字唯一、换主键时必须全删;目标形态下它们是残留、同样要删。判错的后果各不相同但都很坏 ——
// 把 PRIMARY 算进去会让迁移去 DROP 主键,把组合键 / 前缀键算进去会删掉别人有用的索引,
// 而漏掉一条就留下"主键对了、形状没收敛"的中间态(死锁悄悄回来,功能全对、零报错)。
// 顺序也在判据里:DDL 不能因为 map 遍历顺序不同而在两次运行里生成不同的语句。
func TestStrayNameNormUniqueIndexNames(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   map[string]uniqueIndexColumns
		want []string
	}{
		{
			name: "目标形态:只有主键,没有多余的",
			in: map[string]uniqueIndexColumns{
				"PRIMARY":                {cols: []string{"name_norm"}},
				PlayerNameOwnerUniqueKey: {cols: []string{"player_id"}},
			},
		},
		{
			name: "旧形态:承担名字唯一的那条要被找出来",
			in: map[string]uniqueIndexColumns{
				"PRIMARY":                     {cols: []string{"player_id"}},
				PlayerNameLegacyNormUniqueKey: {cols: []string{"name_norm"}},
			},
			want: []string{PlayerNameLegacyNormUniqueKey},
		},
		{
			name: "中间态:主键已是 name_norm,旧键还残留着 —— 必须被认出来",
			in: map[string]uniqueIndexColumns{
				"PRIMARY":                     {cols: []string{"name_norm"}},
				PlayerNameLegacyNormUniqueKey: {cols: []string{"name_norm"}},
				PlayerNameOwnerUniqueKey:      {cols: []string{"player_id"}},
			},
			want: []string{PlayerNameLegacyNormUniqueKey},
		},
		{
			name: "多条时按名字排序全部返回(DDL 要一次收敛干净,且语句必须确定)",
			in: map[string]uniqueIndexColumns{
				"PRIMARY":                     {cols: []string{"name_norm"}},
				"zz_name_norm_copy":           {cols: []string{"name_norm"}},
				PlayerNameLegacyNormUniqueKey: {cols: []string{"name_norm"}},
			},
			want: []string{PlayerNameLegacyNormUniqueKey, "zz_name_norm_copy"},
		},
		{
			name: "组合唯一键与前缀键都不算:删掉它们既不必要也可能误伤",
			in: map[string]uniqueIndexColumns{
				"PRIMARY":      {cols: []string{"name_norm"}},
				"uk_composite": {cols: []string{"name_norm", "player_id"}},
				"uk_prefixed":  {cols: []string{"name_norm"}, prefixed: true},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := strayNameNormUniqueIndexNames(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("strayNameNormUniqueIndexNames = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestMigrateOptionsPlayerNameLegacyDefaultsToMigrating:跳过换主键的逃生口默认必须是**关**的。
//
// 它打开之后 player_name 会停在旧形态 —— 功能正确,但名字抢注在生产常态那一路仍然成环(真库实测 5/5),
// 只能靠有界 1213 重试吸收。这种"明知会成环"的形态是用户明确不接受的,只能由运维在窗口不够时显式选择,
// 不允许任何代码路径顺手置 true(与 AllowMissingGuidUniqueKey 同口径)。
func TestMigrateOptionsPlayerNameLegacyDefaultsToMigrating(t *testing.T) {
	if (MigrateOptions{}).AllowLegacyPlayerNamePrimaryKey {
		t.Fatal("AllowLegacyPlayerNamePrimaryKey 的零值成了 true:默认必须执行换主键迁移," +
			"否则存量库会静默停在「生产常态那一路 5/5 成环、靠重试兜」的旧形态上")
	}
}

// TestOutcomeStringsAreStableLowCardinality:两个 outcome 的 String() 进日志与指标 label,
// 值必须稳定且低基数(四个 / 三个固定值),改一个字就会让既有看板与告警规则错位。
func TestOutcomeStringsAreStableLowCardinality(t *testing.T) {
	for _, tc := range []struct {
		got  string
		want string
	}{
		{ReserveInserted.String(), "inserted"},
		{ReserveAlreadyOwned.String(), "already_owned"},
		{ReserveTaken.String(), "taken"},
		{ReserveConflict.String(), "conflict"},
		{ReserveOutcome(0).String(), "unknown"},
		{ReleaseDeleted.String(), "deleted"},
		{ReleaseAbsent.String(), "absent"},
		{ReleaseOutsideWindow.String(), "outside_window"},
		{ReleaseOutcome(0).String(), "unknown"},
	} {
		if tc.got != tc.want {
			t.Fatalf("outcome label = %q, want %q", tc.got, tc.want)
		}
	}
}
