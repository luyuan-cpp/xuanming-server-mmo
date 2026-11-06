package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// PlayerNameStore 的**对外语义**回归:哪种库内情形回哪个 ReserveOutcome / ReleaseOutcome / BindOutcome、
// 发几条语句、发的是不是生产那几段 SQL 文本。
//
// 为什么要这一份(而不是只靠真库用例):终局判定是 login 建角路径的契约(Inserted / AlreadyOwned 算成功,
// Taken 要回 owner,Conflict 是 ID 复用事故),而真库用例需要 Docker、默认 skip —— 只有它们守着的话,
// 一次"顺手改写 SQL 或调整分支"在 `go test ./...` 全绿的情况下就能把契约改掉。这里用一个只认本 store 那
// **十一条**语句的假驱动把这些组合钉死,不需要任何外部依赖,也不引入新的三方库。
//
// 十一条语句 = 名字行四条写(playerNameReserveSQL / playerNameReleaseSQL / playerNameBindSQL /
// playerNameReleaseRemovedSQL)+ owner 行两条写(playerNameOwnerUpsertSQL / playerNameOwnerDeleteSQL)
// + 五条读(ownedNormOf / ownerOfNorm / nameRowStateOf / nameHoldingOf / BatchGet 的 IN 查询),
// 对应下面七个应答队列(两张表的写各共用一个队列,按调用顺序消费)。
//
// 假驱动**不模拟 InnoDB 的锁与唯一约束**:并发下"谁赢"、会不会 1213,只有真库能证(见
// player_name_store_integration_test.go)。这里只证"给定库的应答,本层的判定与语句不变"。

// fakeNameExec 是一次写语句(ODKU / DELETE)的应答。err 非 nil 时 affected 被忽略。
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

// fakeNamePair 是一次读两列(都是 uint64)的应答:nameRowStateOf 是 (created_ms, bound_ms),
// nameHoldingOf 是 (player_id, bound_ms)。found=false → 零行。
type fakeNamePair struct {
	first, second uint64
	found         bool
	err           error
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

// fakeNameDB 按脚本应答。七个队列覆盖 store 的十一条语句(两张表的写各共用一个),按调用顺序消费;
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
	execs      []fakeNameExec   // 名字行:playerNameReserveSQL / playerNameReleaseSQL / playerNameBindSQL / playerNameReleaseRemovedSQL
	ownerExecs []fakeNameExec   // owner 行:playerNameOwnerUpsertSQL / playerNameOwnerDeleteSQL
	owned      []fakeNameLookup // ownedNormOf:name_norm(string)
	owner      []fakeNameLookup // ownerOfNorm:player_id(uint64)
	rowState   []fakeNamePair   // nameRowStateOf:created_ms, bound_ms
	holding    []fakeNamePair   // nameHoldingOf:player_id, bound_ms
	batch      []fakeNameBatch  // BatchGet:player_id, name(多行)

	// afterFirstExec 在第一条写语句被应答之后调用一次。只给"退避途中 ctx 结束"那个用例用:
	// database/sql 在取连接之前就会检查 ctx,提前取消的话第一条语句根本发不出去,测不到退避这一段。
	afterFirstExec func()
}

var errFakeNameScriptExhausted = errors.New("fake player_name db: 脚本用尽,被测代码发的语句比用例预期的多")

// newFakeNameStore 把假驱动接成一个 K=1 的 PlayerNameStore(同包,直接装字段,生产代码不必为测试开口子),
// 同时把假库还给用例,用来断言"发了几条什么语句"。
func newFakeNameStore(t *testing.T, script fakeNameScript) (*PlayerNameStore, *fakeNameDB) {
	t.Helper()
	fake := &fakeNameDB{script: script}
	db := sql.OpenDB(fakeNameConnector{fake: fake})
	t.Cleanup(func() { _ = db.Close() })
	return &PlayerNameStore{pools: singleShardPools(db, "")}, fake
}

// newFakeNameShards 接成 K=4,每片一个假库。没给脚本的片一条语句都不该收到:脚本用尽会报错。
func newFakeNameShards(t *testing.T, scripts map[int]fakeNameScript) (*PlayerNameStore, []*fakeNameDB) {
	t.Helper()
	const k = 4
	pools := &shardPools{k: k, names: make([]string, k), pools: make([]*sql.DB, k)}
	fakes := make([]*fakeNameDB, k)
	for slot := range fakes {
		fakes[slot] = &fakeNameDB{script: scripts[slot]}
		db := sql.OpenDB(fakeNameConnector{fake: fakes[slot]})
		t.Cleanup(func() { _ = db.Close() })
		pools.pools[slot] = db
	}
	return &PlayerNameStore{pools: pools}, fakes
}

// isOwnerStatement 判断一条写语句落在哪张表。
func isOwnerStatement(query string) bool {
	return query == playerNameOwnerUpsertSQL || query == playerNameOwnerDeleteSQL
}

func (f *fakeNameDB) recordExec(stmt fakeNameStmt) (fakeNameExec, error) {
	f.mu.Lock()
	f.gotExec = append(f.gotExec, stmt)
	first := len(f.gotExec) == 1
	hook := f.script.afterFirstExec
	queue := &f.script.execs
	if isOwnerStatement(stmt.sql) {
		queue = &f.script.ownerExecs
	}
	var next fakeNameExec
	var err error
	if len(*queue) == 0 {
		err = fmt.Errorf("%w: exec %q", errFakeNameScriptExhausted, stmt.sql)
	} else {
		next = (*queue)[0]
		*queue = (*queue)[1:]
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

func (f *fakeNameDB) recordPairQuery(stmt fakeNameStmt, queue *[]fakeNamePair) (fakeNamePair, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotQuery = append(f.gotQuery, stmt)
	if len(*queue) == 0 {
		return fakeNamePair{}, fmt.Errorf("%w: query %q", errFakeNameScriptExhausted, stmt.sql)
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

// nameExecCount / ownerExecCount 分别数两张表上的写。
func (f *fakeNameDB) nameExecCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, stmt := range f.gotExec {
		if !isOwnerStatement(stmt.sql) {
			n++
		}
	}
	return n
}

func (f *fakeNameDB) ownerExecCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, stmt := range f.gotExec {
		if isOwnerStatement(stmt.sql) {
			n++
		}
	}
	return n
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

// ── 驱动实现(只够跑本 store 的八条语句)──────────────────────────────────────────

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

	// BatchGet 的 IN 查询排在最前面分流:它的前缀是 "SELECT player_id, name FROM player_name_owner",
	// 与 ownerOfNorm 的 "SELECT player_id FROM player_name" 不互为前缀(逗号 vs 空格),但先判它更不容易看错。
	if strings.HasPrefix(query, "SELECT player_id, name FROM player_name_owner") {
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

	// 两列的读按生产文本整条匹配:前缀 "SELECT player_id, bound_ms" 与 ownerOfNorm 的 "SELECT player_id FROM" 不互为前缀,
	// 但整条比对更不容易被一次改写悄悄分流到别的队列。
	var pairs *[]fakeNamePair
	var pairCols []string
	switch query {
	case playerNameRowStateSQL:
		pairs, pairCols = &c.fake.script.rowState, []string{"created_ms", "bound_ms"}
	case playerNameHoldingSQL:
		pairs, pairCols = &c.fake.script.holding, []string{"player_id", "bound_ms"}
	}
	if pairs != nil {
		next, err := c.fake.recordPairQuery(stmt, pairs)
		if err != nil {
			return nil, err
		}
		if next.err != nil {
			return nil, next.err
		}
		rows := &fakeNameRows{cols: pairCols}
		if next.found {
			rows.rows = [][]driver.Value{{int64(next.first), int64(next.second)}}
		}
		return rows, nil
	}

	var queue *[]fakeNameLookup
	var column string
	switch {
	case strings.HasPrefix(query, "SELECT name_norm FROM player_name_owner"):
		queue, column = &c.fake.script.owned, "name_norm"
	case strings.HasPrefix(query, "SELECT player_id FROM player_name"):
		queue, column = &c.fake.script.owner, "player_id"
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

const (
	contractNamePlayer uint64 = 4242
	contractOther      uint64 = 9001
	contractNorm              = "yunzhongjun"
	contractName              = "YunZhongJun"
	contractNowMs      uint64 = 1700000000000
)

// TestReserveOutcomeContract 钉住"库回什么 → Reserve 回什么",一格都不许动:
// Inserted / AlreadyOwned 是成功(login 按成功继续建角),Taken 必须带出 owner(login 靠它识别自己的重试),
// Conflict 是 ID 复用事故(调用方必须当故障,不能删旧行)。
//
// 顺序见 Reserve:第 0 步读 owner 行,第 1 步写名字行,第 2 步写 owner 行。wantNameExecs / wantOwnerExecs 分别数两张表上的写。
func TestReserveOutcomeContract(t *testing.T) {
	noOwnerRow := fakeNameLookup{found: false}
	ownedHere := fakeNameLookup{value: contractNorm, found: true}
	ownedElsewhere := fakeNameLookup{value: "laomingzi", found: true}
	heldByMe := fakeNameLookup{value: contractNamePlayer, found: true}
	heldByOther := fakeNameLookup{value: contractOther, found: true}
	nameFree := fakeNameLookup{found: false}

	for _, tc := range []struct {
		name           string
		script         fakeNameScript
		wantOutcome    ReserveOutcome
		wantOwner      uint64
		wantErr        bool
		wantNameExecs  int
		wantOwnerExecs int
		wantQueries    int
	}{
		{
			name: "全新_两张表各写一行",
			script: fakeNameScript{
				owned:      []fakeNameLookup{noOwnerRow},
				execs:      []fakeNameExec{{affected: 1}},
				ownerExecs: []fakeNameExec{{affected: 1}},
			},
			wantOutcome:   ReserveInserted,
			wantNameExecs: 1, wantOwnerExecs: 1,
			// 插入成功不问名字归谁,只有第 0 步那一条读。
			wantQueries: 1,
		},
		{
			name: "重放_两边都在_幂等成功",
			script: fakeNameScript{
				owned:      []fakeNameLookup{ownedHere, ownedHere},
				execs:      []fakeNameExec{{affected: 0}},
				owner:      []fakeNameLookup{heldByMe},
				ownerExecs: []fakeNameExec{{affected: 0}},
			},
			wantOutcome:   ReserveAlreadyOwned,
			wantNameExecs: 1, wantOwnerExecs: 1,
			wantQueries: 3,
		},
		{
			name: "重放_上次第2步失败_补写owner行",
			script: fakeNameScript{
				owned:      []fakeNameLookup{noOwnerRow},
				execs:      []fakeNameExec{{affected: 0}},
				owner:      []fakeNameLookup{heldByMe},
				ownerExecs: []fakeNameExec{{affected: 1}},
			},
			wantOutcome:   ReserveAlreadyOwned,
			wantNameExecs: 1, wantOwnerExecs: 1,
			wantQueries: 2,
		},
		{
			name: "已有别的名字且确实是自己的_ID复用事故_什么都不写",
			script: fakeNameScript{
				owned: []fakeNameLookup{ownedElsewhere},
				owner: []fakeNameLookup{heldByMe},
			},
			wantOutcome: ReserveConflict,
			wantQueries: 2,
		},
		{
			name: "owner行指的名字已释放_陈旧_清掉再登记",
			script: fakeNameScript{
				owned:      []fakeNameLookup{ownedElsewhere},
				owner:      []fakeNameLookup{nameFree},
				execs:      []fakeNameExec{{affected: 1}},
				ownerExecs: []fakeNameExec{{affected: 1}, {affected: 1}},
			},
			wantOutcome:   ReserveInserted,
			wantNameExecs: 1, wantOwnerExecs: 2,
			wantQueries: 2,
		},
		{
			name: "owner行指的名字归了别人_陈旧_清掉再登记",
			script: fakeNameScript{
				owned:      []fakeNameLookup{ownedElsewhere},
				owner:      []fakeNameLookup{heldByOther},
				execs:      []fakeNameExec{{affected: 1}},
				ownerExecs: []fakeNameExec{{affected: 1}, {affected: 1}},
			},
			wantOutcome:   ReserveInserted,
			wantNameExecs: 1, wantOwnerExecs: 2,
			wantQueries: 2,
		},
		{
			name: "清陈旧owner行失败_回错误_不碰名字行",
			script: fakeNameScript{
				owned:      []fakeNameLookup{ownedElsewhere},
				owner:      []fakeNameLookup{nameFree},
				ownerExecs: []fakeNameExec{{err: mysqlErrNo(1146)}},
			},
			wantErr:        true,
			wantOwnerExecs: 1,
			wantQueries:    2,
		},
		{
			name: "名字被别人占_回出owner_不写owner行",
			script: fakeNameScript{
				owned: []fakeNameLookup{noOwnerRow},
				execs: []fakeNameExec{{affected: 0}},
				owner: []fakeNameLookup{heldByOther},
			},
			wantOutcome:   ReserveTaken,
			wantOwner:     contractOther,
			wantNameExecs: 1,
			wantQueries:   2,
		},
		{
			name: "名字被别人占_自己的owner行还指着它_顺手清掉",
			script: fakeNameScript{
				owned:      []fakeNameLookup{ownedHere},
				execs:      []fakeNameExec{{affected: 0}},
				owner:      []fakeNameLookup{heldByOther},
				ownerExecs: []fakeNameExec{{affected: 1}},
			},
			wantOutcome:   ReserveTaken,
			wantOwner:     contractOther,
			wantNameExecs: 1, wantOwnerExecs: 1,
			wantQueries: 2,
		},
		{
			name: "名字被别人占_清owner行失败_结局不变",
			script: fakeNameScript{
				owned:      []fakeNameLookup{ownedHere},
				execs:      []fakeNameExec{{affected: 0}},
				owner:      []fakeNameLookup{heldByOther},
				ownerExecs: []fakeNameExec{{err: mysqlErrNo(1146)}},
			},
			wantOutcome:   ReserveTaken,
			wantOwner:     contractOther,
			wantNameExecs: 1, wantOwnerExecs: 1,
			wantQueries: 2,
		},
		{
			name: "撞键后行已消失_下一轮重插成功",
			script: fakeNameScript{
				owned:      []fakeNameLookup{noOwnerRow},
				execs:      []fakeNameExec{{affected: 0}, {affected: 1}},
				owner:      []fakeNameLookup{nameFree},
				ownerExecs: []fakeNameExec{{affected: 1}},
			},
			wantOutcome:   ReserveInserted,
			wantNameExecs: 2, wantOwnerExecs: 1,
			wantQueries: 2,
		},
		{
			name: "1062也走撞键分辨_纵深防御",
			script: fakeNameScript{
				owned:      []fakeNameLookup{noOwnerRow},
				execs:      []fakeNameExec{{err: mysqlErrNo(mysqlErrDupEntry)}},
				owner:      []fakeNameLookup{heldByMe},
				ownerExecs: []fakeNameExec{{affected: 1}},
			},
			wantOutcome:   ReserveAlreadyOwned,
			wantNameExecs: 1, wantOwnerExecs: 1,
			wantQueries: 2,
		},
		{
			name: "1213_退避后重来_成功",
			script: fakeNameScript{
				owned:      []fakeNameLookup{noOwnerRow},
				execs:      []fakeNameExec{{err: mysqlErrNo(mysqlErrDeadlock)}, {affected: 1}},
				ownerExecs: []fakeNameExec{{affected: 1}},
			},
			wantOutcome:   ReserveInserted,
			wantNameExecs: 2, wantOwnerExecs: 1,
			wantQueries: 1,
		},
		{
			name: "1205_退避后重来_撞键成Taken",
			script: fakeNameScript{
				owned: []fakeNameLookup{noOwnerRow},
				execs: []fakeNameExec{{err: mysqlErrNo(mysqlErrLockWaitTimeout)}, {affected: 0}},
				owner: []fakeNameLookup{heldByOther},
			},
			wantOutcome:   ReserveTaken,
			wantOwner:     contractOther,
			wantNameExecs: 2,
			wantQueries:   2,
		},
		{
			name: "1213_用尽预算_按存储错误返回",
			script: fakeNameScript{
				owned: []fakeNameLookup{noOwnerRow},
				execs: []fakeNameExec{
					{err: mysqlErrNo(mysqlErrDeadlock)},
					{err: mysqlErrNo(mysqlErrDeadlock)},
					{err: mysqlErrNo(mysqlErrDeadlock)},
				},
			},
			wantErr:       true,
			wantNameExecs: playerNameReserveAttempts,
			wantQueries:   1,
		},
		{
			name: "非锁错误_立刻返回不重试",
			script: fakeNameScript{
				owned: []fakeNameLookup{noOwnerRow},
				execs: []fakeNameExec{{err: mysqlErrNo(1064)}},
			},
			wantErr:       true,
			wantNameExecs: 1,
			wantQueries:   1,
		},
		{
			name: "受影响2行_fail_closed",
			script: fakeNameScript{
				owned: []fakeNameLookup{noOwnerRow},
				execs: []fakeNameExec{{affected: 2}},
			},
			wantErr:       true,
			wantNameExecs: 1,
			wantQueries:   1,
		},
		{
			name:        "第0步读失败_什么都不写",
			script:      fakeNameScript{owned: []fakeNameLookup{{err: mysqlErrNo(1146)}}},
			wantErr:     true,
			wantQueries: 1,
		},
		{
			name: "撞键后读名字行失败_原样返回错误",
			script: fakeNameScript{
				owned: []fakeNameLookup{noOwnerRow},
				execs: []fakeNameExec{{affected: 0}},
				owner: []fakeNameLookup{{err: mysqlErrNo(1146)}},
			},
			wantErr:       true,
			wantNameExecs: 1,
			wantQueries:   2,
		},
		{
			// 名字行留着:login 同参数重试走 AlreadyOwned 补写,或者 Release 把两边都清掉。
			name: "第2步失败_回错误_名字行不撤销",
			script: fakeNameScript{
				owned:      []fakeNameLookup{noOwnerRow},
				execs:      []fakeNameExec{{affected: 1}},
				ownerExecs: []fakeNameExec{{err: mysqlErrNo(mysqlErrDeadlock)}},
			},
			wantErr:       true,
			wantNameExecs: 1, wantOwnerExecs: 1,
			wantQueries: 1,
		},
		{
			name: "第2步受影响2行_fail_closed",
			script: fakeNameScript{
				owned:      []fakeNameLookup{noOwnerRow},
				execs:      []fakeNameExec{{affected: 1}},
				ownerExecs: []fakeNameExec{{affected: 2}},
			},
			wantErr:       true,
			wantNameExecs: 1, wantOwnerExecs: 1,
			wantQueries: 1,
		},
		{
			// 第 0 步之后这个 player_id 又登记了别的名字。本次新插入的名字行要撤销,不然它占着名字却永远不会有人释放。
			name: "第2步发现已是别的名字_新插入的名字行撤销",
			script: fakeNameScript{
				owned:      []fakeNameLookup{noOwnerRow, ownedElsewhere},
				execs:      []fakeNameExec{{affected: 1}, {affected: 1}},
				ownerExecs: []fakeNameExec{{affected: 0}},
			},
			wantOutcome:   ReserveConflict,
			wantNameExecs: 2, wantOwnerExecs: 1,
			wantQueries: 2,
		},
		{
			// 名字行留下了,不能回 ReserveConflict:它映射成确定性的拒绝,login 和排空证明都按「确定没写」算。
			name: "第2步发现已是别的名字_撤销失败_回错误",
			script: fakeNameScript{
				owned:      []fakeNameLookup{noOwnerRow, ownedElsewhere},
				execs:      []fakeNameExec{{affected: 1}, {err: mysqlErrNo(1146)}},
				ownerExecs: []fakeNameExec{{affected: 0}},
			},
			wantErr:       true,
			wantNameExecs: 2, wantOwnerExecs: 1,
			wantQueries: 2,
		},
		{
			// 名字行不是这一次插的,不能撤:它可能属于一个在役角色。
			name: "第2步发现已是别的名字_名字行不是本次插入_不撤销",
			script: fakeNameScript{
				owned:      []fakeNameLookup{noOwnerRow, ownedElsewhere},
				execs:      []fakeNameExec{{affected: 0}},
				owner:      []fakeNameLookup{heldByMe},
				ownerExecs: []fakeNameExec{{affected: 0}},
			},
			wantOutcome:   ReserveConflict,
			wantNameExecs: 1, wantOwnerExecs: 1,
			wantQueries: 3,
		},
		{
			name: "第2步撞键后owner行消失_并发释放_回错误",
			script: fakeNameScript{
				owned:      []fakeNameLookup{noOwnerRow, noOwnerRow},
				execs:      []fakeNameExec{{affected: 1}},
				ownerExecs: []fakeNameExec{{affected: 0}},
			},
			wantErr:       true,
			wantNameExecs: 1, wantOwnerExecs: 1,
			wantQueries: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, fake := newFakeNameStore(t, tc.script)

			outcome, owner, err := st.Reserve(context.Background(), contractNamePlayer, contractName, contractNorm, contractNowMs)
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
			if got := fake.nameExecCount(); got != tc.wantNameExecs {
				t.Fatalf("名字行上发出 %d 条写,want %d", got, tc.wantNameExecs)
			}
			if got := fake.ownerExecCount(); got != tc.wantOwnerExecs {
				t.Fatalf("owner 行上发出 %d 条写,want %d", got, tc.wantOwnerExecs)
			}
			if got := fake.queryCount(); got != tc.wantQueries {
				t.Fatalf("发出 %d 条等值读,want %d", got, tc.wantQueries)
			}
			// 实参顺序按语句钉住。name 与 name_norm 互换在这里必须变红:互换之后库里存的展示名是归一化串、判重键是原串,
			// 唯一性口径整个错位,而 SQL 文本一个字都没变。uint64 经 driver.DefaultParameterConverter 落成 int64,所以期望值也写 int64。
			wantArgs := map[string][]driver.Value{
				playerNameReserveSQL:     {int64(contractNamePlayer), contractName, contractNorm, int64(contractNowMs)},
				playerNameOwnerUpsertSQL: {int64(contractNamePlayer), contractNorm, contractName},
				// 撤销只删这一次插入的行:登记时刻就是下界。
				playerNameReleaseSQL: {int64(contractNamePlayer), contractNorm, int64(contractNowMs)},
			}
			for i := 0; i < fake.execCount(); i++ {
				stmt := fake.execAt(i)
				if stmt.sql == playerNameOwnerDeleteSQL {
					if stmt.args[0] != int64(contractNamePlayer) {
						t.Fatalf("删 owner 行的 player_id 不对:%#v", stmt.args)
					}
					continue
				}
				want, ok := wantArgs[stmt.sql]
				if !ok {
					t.Fatalf("第 %d 条写语句不是生产文本:%q", i, stmt.sql)
				}
				if !reflect.DeepEqual(stmt.args, want) {
					t.Fatalf("第 %d 条写语句的实参不对:\n got=%#v\nwant=%#v", i, stmt.args, want)
				}
			}
		})
	}
}

// 顺序钉住:先读 owner 行,再写名字行(提交点),最后写 owner 行。反过来的话,名字没占到就留下了 owner 行。
func TestReserveWritesNameRowBeforeOwnerRow(t *testing.T) {
	st, fake := newFakeNameStore(t, fakeNameScript{
		owned:      []fakeNameLookup{{found: false}},
		execs:      []fakeNameExec{{affected: 1}},
		ownerExecs: []fakeNameExec{{affected: 1}},
	})
	if _, _, err := st.Reserve(context.Background(), contractNamePlayer, contractName, contractNorm, contractNowMs); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := fake.execAt(0).sql; got != playerNameReserveSQL {
		t.Fatalf("第一条写不是名字行:%q", got)
	}
	if got := fake.execAt(1).sql; got != playerNameOwnerUpsertSQL {
		t.Fatalf("第二条写不是 owner 行:%q", got)
	}
}

// 第 2 步失败之后,同参数重放把 owner 行补上,结局是 AlreadyOwned。
func TestReserveReplayFinishesOwnerRow(t *testing.T) {
	st, fake := newFakeNameStore(t, fakeNameScript{
		owned:      []fakeNameLookup{{found: false}, {found: false}},
		execs:      []fakeNameExec{{affected: 1}, {affected: 0}},
		owner:      []fakeNameLookup{{value: contractNamePlayer, found: true}},
		ownerExecs: []fakeNameExec{{err: mysqlErrNo(mysqlErrDeadlock)}, {affected: 1}},
	})
	if _, _, err := st.Reserve(context.Background(), contractNamePlayer, contractName, contractNorm, contractNowMs); err == nil {
		t.Fatal("第一次第 2 步失败应当回错误")
	}
	outcome, _, err := st.Reserve(context.Background(), contractNamePlayer, contractName, contractNorm, contractNowMs)
	if err != nil || outcome != ReserveAlreadyOwned {
		t.Fatalf("重放 = %s, %v, want already_owned", outcome, err)
	}
	if got := fake.ownerExecCount(); got != 2 {
		t.Fatalf("owner 行上发出 %d 条写,want 2", got)
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
	st, _ := newFakeNameStore(t, fakeNameScript{
		owned:      []fakeNameLookup{{found: false}},
		execs:      []fakeNameExec{{err: mysqlErrNo(mysqlErrDeadlock)}, {affected: 1}},
		ownerExecs: []fakeNameExec{{affected: 1}},
	})
	if _, _, err := st.Reserve(context.Background(), 7, "A", "a", 1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := st.LockRetriesForTest(); got != 1 {
		t.Fatalf("lockRetries = %d, want 1", got)
	}

	// "撞键后行已消失"那一轮不是锁冲突,不许计数、也不许退避。
	script2 := fakeNameScript{
		owned:      []fakeNameLookup{{found: false}},
		execs:      []fakeNameExec{{affected: 0}, {affected: 1}},
		owner:      []fakeNameLookup{{found: false}},
		ownerExecs: []fakeNameExec{{affected: 1}},
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
		owned: []fakeNameLookup{{found: false}},
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

// TestReleaseOutcomeContract 钉住"删了几行 + 行的登记时刻、绑定时刻 → 哪个 ReleaseOutcome":
// Deleted / Absent 都是成功(幂等),OutsideWindow 必须是拒绝(它拦的是"删别人在役角色的名字"),
// Bound 必须是拒绝,而且不限时间(运维 token)也一样(已绑定的名字只有删角之后能释放)。
// owner 行只在名字行确定不再属于这个玩家之后才删;OutsideWindow、Bound 时两边都不动。
func TestReleaseOutcomeContract(t *testing.T) {
	const (
		player       uint64 = 4242
		norm                = "yunzhongjun"
		minCreatedMs uint64 = 1_700_000_000_000
		boundMs      uint64 = minCreatedMs + 5
	)

	for _, tc := range []struct {
		name           string
		minCreatedMs   uint64
		script         fakeNameScript
		wantOutcome    ReleaseOutcome
		wantErr        bool
		wantNameExecs  int
		wantOwnerExecs int
		wantQueries    int
	}{
		{
			name:         "删到了",
			minCreatedMs: minCreatedMs,
			script: fakeNameScript{
				execs:      []fakeNameExec{{affected: 1}},
				ownerExecs: []fakeNameExec{{affected: 1}},
			},
			wantOutcome:   ReleaseDeleted,
			wantNameExecs: 1, wantOwnerExecs: 1,
		},
		{
			name:         "不限时间_没删到_行不存在_幂等成功",
			minCreatedMs: 0,
			script: fakeNameScript{
				execs:      []fakeNameExec{{affected: 0}},
				rowState:   []fakeNamePair{{found: false}},
				ownerExecs: []fakeNameExec{{affected: 0}},
			},
			wantOutcome:   ReleaseAbsent,
			wantNameExecs: 1, wantOwnerExecs: 1,
			// 时间条件恒真,但"没删到"还可能是已绑定:不限时间也要探一次。
			wantQueries: 1,
		},
		{
			// 运维 token 只放宽时间窗,不放宽状态:建成的角色的名字带 token 也删不掉。
			name:         "不限时间_行已绑定_拒绝_两边都不动",
			minCreatedMs: 0,
			script: fakeNameScript{
				execs:    []fakeNameExec{{affected: 0}},
				rowState: []fakeNamePair{{first: minCreatedMs, second: boundMs, found: true}},
			},
			wantOutcome:   ReleaseBound,
			wantNameExecs: 1,
			wantQueries:   1,
		},
		{
			// 建角回滚(带时间窗)碰上已绑定的名字:login 判错了这次建角的结局,或者调用方身份不对。拒绝,owner 行也不动。
			name:         "限时间_行已绑定_拒绝_两边都不动",
			minCreatedMs: minCreatedMs,
			script: fakeNameScript{
				execs:    []fakeNameExec{{affected: 0}},
				rowState: []fakeNamePair{{first: minCreatedMs, second: boundMs, found: true}},
			},
			wantOutcome:   ReleaseBound,
			wantNameExecs: 1,
			wantQueries:   1,
		},
		{
			name:         "限时间_行不存在_幂等成功",
			minCreatedMs: minCreatedMs,
			script: fakeNameScript{
				execs:      []fakeNameExec{{affected: 0}},
				rowState:   []fakeNamePair{{found: false}},
				ownerExecs: []fakeNameExec{{affected: 0}},
			},
			wantOutcome:   ReleaseAbsent,
			wantNameExecs: 1, wantOwnerExecs: 1,
			wantQueries: 1,
		},
		{
			name:         "限时间_行太老_拒绝_两边都不动",
			minCreatedMs: minCreatedMs,
			script: fakeNameScript{
				execs:    []fakeNameExec{{affected: 0}},
				rowState: []fakeNamePair{{first: minCreatedMs - 1, found: true}},
			},
			wantOutcome:   ReleaseOutsideWindow,
			wantNameExecs: 1,
			wantQueries:   1,
		},
		{
			name:         "限时间_行在窗口内却没删到_重删",
			minCreatedMs: minCreatedMs,
			script: fakeNameScript{
				execs:      []fakeNameExec{{affected: 0}, {affected: 1}},
				rowState:   []fakeNamePair{{first: minCreatedMs, found: true}},
				ownerExecs: []fakeNameExec{{affected: 1}},
			},
			wantOutcome:   ReleaseDeleted,
			wantNameExecs: 2, wantOwnerExecs: 1,
			wantQueries: 1,
		},
		{
			name:         "1213_退避后重来_删到",
			minCreatedMs: minCreatedMs,
			script: fakeNameScript{
				execs:      []fakeNameExec{{err: mysqlErrNo(mysqlErrDeadlock)}, {affected: 1}},
				ownerExecs: []fakeNameExec{{affected: 1}},
			},
			wantOutcome:   ReleaseDeleted,
			wantNameExecs: 2, wantOwnerExecs: 1,
		},
		{
			name:         "1213_用尽预算_按存储错误返回_owner行不动",
			minCreatedMs: minCreatedMs,
			script: fakeNameScript{
				execs: []fakeNameExec{
					{err: mysqlErrNo(mysqlErrDeadlock)},
					{err: mysqlErrNo(mysqlErrDeadlock)},
					{err: mysqlErrNo(mysqlErrDeadlock)},
				},
			},
			wantErr:       true,
			wantNameExecs: playerNameReleaseAttempts,
		},
		{
			name:          "非锁错误_立刻返回不重试",
			minCreatedMs:  minCreatedMs,
			script:        fakeNameScript{execs: []fakeNameExec{{err: mysqlErrNo(1064)}}},
			wantErr:       true,
			wantNameExecs: 1,
		},
		{
			name:         "名字行删了_owner行删失败_回错误",
			minCreatedMs: minCreatedMs,
			script: fakeNameScript{
				execs:      []fakeNameExec{{affected: 1}},
				ownerExecs: []fakeNameExec{{err: mysqlErrNo(1146)}},
			},
			wantErr:       true,
			wantNameExecs: 1, wantOwnerExecs: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, fake := newFakeNameStore(t, tc.script)

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
			if got := fake.nameExecCount(); got != tc.wantNameExecs {
				t.Fatalf("名字行上发出 %d 条 DELETE,want %d", got, tc.wantNameExecs)
			}
			if got := fake.ownerExecCount(); got != tc.wantOwnerExecs {
				t.Fatalf("owner 行上发出 %d 条 DELETE,want %d", got, tc.wantOwnerExecs)
			}
			if got := fake.queryCount(); got != tc.wantQueries {
				t.Fatalf("发出 %d 条探测读,want %d", got, tc.wantQueries)
			}
			// 实参顺序必须是 (player_id, name_norm, min_created_ms)。错位之后"删谁"与"时间窗下界"会互换,
			// 而 SQL 文本一个字都没变 —— 这是本文件要拦的那一类改动。
			wantNameArgs := []driver.Value{int64(player), norm, int64(tc.minCreatedMs)}
			wantOwnerArgs := []driver.Value{int64(player), norm}
			for i := 0; i < fake.execCount(); i++ {
				stmt := fake.execAt(i)
				switch stmt.sql {
				case playerNameReleaseSQL:
					if !reflect.DeepEqual(stmt.args, wantNameArgs) {
						t.Fatalf("名字行 DELETE 的实参不对:\n got=%#v\nwant=%#v", stmt.args, wantNameArgs)
					}
				case playerNameOwnerDeleteSQL:
					if !reflect.DeepEqual(stmt.args, wantOwnerArgs) {
						t.Fatalf("owner 行 DELETE 的实参不对:\n got=%#v\nwant=%#v", stmt.args, wantOwnerArgs)
					}
				default:
					t.Fatalf("第 %d 条写语句不是生产文本:%q", i, stmt.sql)
				}
			}
		})
	}
}

// 名字行删了、owner 行没删掉:重放时名字行已不在,走 ReleaseAbsent 接着把 owner 行删掉。
func TestReleaseReplayFinishesOwnerRow(t *testing.T) {
	st, fake := newFakeNameStore(t, fakeNameScript{
		execs:      []fakeNameExec{{affected: 1}, {affected: 0}},
		rowState:   []fakeNamePair{{found: false}},
		ownerExecs: []fakeNameExec{{err: mysqlErrNo(mysqlErrDeadlock)}, {affected: 1}},
	})
	if _, err := st.Release(context.Background(), 4242, "abc", 0); err == nil {
		t.Fatal("第一次删 owner 行失败应当回错误")
	}
	outcome, err := st.Release(context.Background(), 4242, "abc", 0)
	if err != nil || outcome != ReleaseAbsent {
		t.Fatalf("重放 = %s, %v, want absent", outcome, err)
	}
	if got := fake.ownerExecCount(); got != 2 {
		t.Fatalf("owner 行上发出 %d 条 DELETE,want 2", got)
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

// ── Bind / ReleaseRemoved 的终局契约(「预留 → 绑定」状态机)──────────────────────────────

// TestBindOutcomeContract 钉住"库回什么 → Bind 回什么":Bound / AlreadyBound / Restored 是成功(名字行已以已绑定提交),
// Taken 必须带出占用者、一个字节都不写(建成的角色和别人共用一个名字,交给核账报告),Conflict 是 ID 复用事故。
// 名字行不在时按原名走 Reserve 重新登记(连同 owner 行),下一轮再置绑定。
func TestBindOutcomeContract(t *testing.T) {
	reservedByMe := fakeNamePair{first: contractNamePlayer, found: true}
	boundToMe := fakeNamePair{first: contractNamePlayer, second: contractNowMs, found: true}
	boundToOther := fakeNamePair{first: contractOther, second: contractNowMs, found: true}
	reservedByOther := fakeNamePair{first: contractOther, found: true}
	noNameRow := fakeNamePair{found: false}

	for _, tc := range []struct {
		name           string
		script         fakeNameScript
		wantOutcome    BindOutcome
		wantOwner      uint64
		wantErr        bool
		wantNameExecs  int
		wantOwnerExecs int
		wantQueries    int
	}{
		{
			name:          "预留态_置为已绑定",
			script:        fakeNameScript{execs: []fakeNameExec{{affected: 1}}},
			wantOutcome:   BindBound,
			wantNameExecs: 1,
		},
		{
			name: "已绑定_重放幂等_什么都不写",
			script: fakeNameScript{
				execs:   []fakeNameExec{{affected: 0}},
				holding: []fakeNamePair{boundToMe},
			},
			wantOutcome:   BindAlreadyBound,
			wantNameExecs: 1,
			wantQueries:   1,
		},
		{
			name: "已绑定在别人名下_一名两角_什么都不写",
			script: fakeNameScript{
				execs:   []fakeNameExec{{affected: 0}},
				holding: []fakeNamePair{boundToOther},
			},
			wantOutcome:   BindTaken,
			wantOwner:     contractOther,
			wantNameExecs: 1,
			wantQueries:   1,
		},
		{
			// 别人的预留态同样不碰:那可能是另一次在途的建角,本角色没有资格把它改成自己的。
			name: "别人的预留态_也不动",
			script: fakeNameScript{
				execs:   []fakeNameExec{{affected: 0}},
				holding: []fakeNamePair{reservedByOther},
			},
			wantOutcome:   BindTaken,
			wantOwner:     contractOther,
			wantNameExecs: 1,
			wantQueries:   1,
		},
		{
			name: "本角色的预留态在UPDATE之后才提交_下一轮绑定",
			script: fakeNameScript{
				execs:   []fakeNameExec{{affected: 0}, {affected: 1}},
				holding: []fakeNamePair{reservedByMe},
			},
			wantOutcome:   BindBound,
			wantNameExecs: 2,
			wantQueries:   1,
		},
		{
			// 角色已经提交,名字行却没了(多集群恢复点错开,或被误释放):按原名重新登记,owner 行一起补,再置绑定。
			name: "名字行不在_按原名重新登记再绑定",
			script: fakeNameScript{
				execs:      []fakeNameExec{{affected: 0}, {affected: 1}, {affected: 1}},
				holding:    []fakeNamePair{noNameRow},
				owned:      []fakeNameLookup{{found: false}},
				ownerExecs: []fakeNameExec{{affected: 1}},
			},
			wantOutcome:   BindRestored,
			wantNameExecs: 3, wantOwnerExecs: 1,
			wantQueries: 2,
		},
		{
			name: "名字行不在_重新登记时被别人抢先_什么都不写",
			script: fakeNameScript{
				execs:   []fakeNameExec{{affected: 0}, {affected: 0}},
				holding: []fakeNamePair{noNameRow},
				owned:   []fakeNameLookup{{found: false}},
				owner:   []fakeNameLookup{{value: contractOther, found: true}},
			},
			wantOutcome:   BindTaken,
			wantOwner:     contractOther,
			wantNameExecs: 2,
			wantQueries:   3,
		},
		{
			name: "名字行不在_这个player_id名下是别的名字_ID复用事故",
			script: fakeNameScript{
				execs:   []fakeNameExec{{affected: 0}},
				holding: []fakeNamePair{noNameRow},
				owned:   []fakeNameLookup{{value: "laomingzi", found: true}},
				owner:   []fakeNameLookup{{value: contractNamePlayer, found: true}},
			},
			wantOutcome:   BindConflict,
			wantNameExecs: 1,
			wantQueries:   3,
		},
		{
			name: "名字行不在_重新登记的owner行失败_回错误",
			script: fakeNameScript{
				execs:      []fakeNameExec{{affected: 0}, {affected: 1}},
				holding:    []fakeNamePair{noNameRow},
				owned:      []fakeNameLookup{{found: false}},
				ownerExecs: []fakeNameExec{{err: mysqlErrNo(mysqlErrDeadlock)}},
			},
			wantErr:       true,
			wantNameExecs: 2, wantOwnerExecs: 1,
			wantQueries: 2,
		},
		{
			name:          "1213_退避后重来_绑定",
			script:        fakeNameScript{execs: []fakeNameExec{{err: mysqlErrNo(mysqlErrDeadlock)}, {affected: 1}}},
			wantOutcome:   BindBound,
			wantNameExecs: 2,
		},
		{
			name: "1213_用尽预算_按存储错误返回",
			script: fakeNameScript{execs: []fakeNameExec{
				{err: mysqlErrNo(mysqlErrDeadlock)},
				{err: mysqlErrNo(mysqlErrDeadlock)},
				{err: mysqlErrNo(mysqlErrDeadlock)},
			}},
			wantErr:       true,
			wantNameExecs: playerNameBindAttempts,
		},
		{
			name:          "非锁错误_立刻返回不重试",
			script:        fakeNameScript{execs: []fakeNameExec{{err: mysqlErrNo(1064)}}},
			wantErr:       true,
			wantNameExecs: 1,
		},
		{
			name:          "受影响2行_fail_closed",
			script:        fakeNameScript{execs: []fakeNameExec{{affected: 2}}},
			wantErr:       true,
			wantNameExecs: 1,
		},
		{
			name: "分辨读失败_回错误",
			script: fakeNameScript{
				execs:   []fakeNameExec{{affected: 0}},
				holding: []fakeNamePair{{err: mysqlErrNo(1146)}},
			},
			wantErr:       true,
			wantNameExecs: 1,
			wantQueries:   1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, fake := newFakeNameStore(t, tc.script)

			outcome, owner, err := st.Bind(context.Background(), contractNamePlayer, contractName, contractNorm, contractNowMs)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got outcome=%s owner=%d", outcome, owner)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if outcome != tc.wantOutcome || owner != tc.wantOwner {
					t.Fatalf("Bind = (%s, %d), want (%s, %d)", outcome, owner, tc.wantOutcome, tc.wantOwner)
				}
			}
			if got := fake.nameExecCount(); got != tc.wantNameExecs {
				t.Fatalf("名字行上发出 %d 条写,want %d", got, tc.wantNameExecs)
			}
			if got := fake.ownerExecCount(); got != tc.wantOwnerExecs {
				t.Fatalf("owner 行上发出 %d 条写,want %d", got, tc.wantOwnerExecs)
			}
			if got := fake.queryCount(); got != tc.wantQueries {
				t.Fatalf("发出 %d 条读,want %d", got, tc.wantQueries)
			}
			// 实参按语句钉住:绑定时刻与 player_id 错位之后,UPDATE 会去改「player_id = 时间戳」那一行,永远改不到,而且不报错。
			wantArgs := map[string][]driver.Value{
				playerNameBindSQL:        {int64(contractNowMs), contractNorm, int64(contractNamePlayer)},
				playerNameReserveSQL:     {int64(contractNamePlayer), contractName, contractNorm, int64(contractNowMs)},
				playerNameOwnerUpsertSQL: {int64(contractNamePlayer), contractNorm, contractName},
			}
			for i := 0; i < fake.execCount(); i++ {
				stmt := fake.execAt(i)
				want, ok := wantArgs[stmt.sql]
				if !ok {
					t.Fatalf("第 %d 条写语句不是 Bind 该发的生产文本:%q", i, stmt.sql)
				}
				if !reflect.DeepEqual(stmt.args, want) {
					t.Fatalf("第 %d 条写语句的实参不对:\n got=%#v\nwant=%#v", i, stmt.args, want)
				}
			}
		})
	}
}

// 绑定之后,建角回滚(带时间窗)和运维清孤儿(不限时间)的释放都必须被拒,名字行与 owner 行一个字节都不动
// (用例「已绑定的名字不可被释放」的库层一半;真库一半见 player_name_store_integration_test.go)。
func TestBoundNameRefusesRollbackRelease(t *testing.T) {
	bound := fakeNamePair{first: contractNowMs, second: contractNowMs, found: true}
	st, fake := newFakeNameStore(t, fakeNameScript{
		execs:    []fakeNameExec{{affected: 1}, {affected: 0}, {affected: 0}},
		rowState: []fakeNamePair{bound, bound},
	})
	ctx := context.Background()
	if outcome, _, err := st.Bind(ctx, contractNamePlayer, contractName, contractNorm, contractNowMs); err != nil || outcome != BindBound {
		t.Fatalf("Bind = %s, %v", outcome, err)
	}
	for _, minCreatedMs := range []uint64{contractNowMs - 1, 0} {
		outcome, err := st.Release(ctx, contractNamePlayer, contractNorm, minCreatedMs)
		if err != nil || outcome != ReleaseBound {
			t.Fatalf("Release(min_created_ms=%d) = %s, %v, want bound", minCreatedMs, outcome, err)
		}
	}
	if got := fake.ownerExecCount(); got != 0 {
		t.Fatalf("已绑定的名字被拒绝释放时 owner 行上发出了 %d 条写", got)
	}
}

// 入参非法一条语句都不发:绑定时刻 0 会让 UPDATE 改成一行「已绑定却 bound_ms=0」的预留态,等于没绑。
func TestBindAndReleaseRemovedRejectInvalidArguments(t *testing.T) {
	for _, tc := range []struct {
		name   string
		player uint64
		norm   string
		nowMs  uint64
	}{
		{"player_id为0", 0, "abc", 1},
		{"name_norm为空", 42, "", 1},
		{"绑定时刻为0", 42, "abc", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, fake := newFakeNameStore(t, fakeNameScript{})
			if _, _, err := st.Bind(context.Background(), tc.player, "Abc", tc.norm, tc.nowMs); !errors.Is(err, ErrPlayerNameInvalidArgument) {
				t.Fatalf("Bind: want ErrPlayerNameInvalidArgument, got %v", err)
			}
			if tc.nowMs != 0 {
				if _, err := st.ReleaseRemoved(context.Background(), tc.player, tc.norm); !errors.Is(err, ErrPlayerNameInvalidArgument) {
					t.Fatalf("ReleaseRemoved: want ErrPlayerNameInvalidArgument, got %v", err)
				}
			}
			if fake.execCount() != 0 || fake.queryCount() != 0 {
				t.Fatalf("入参非法却发了语句:exec=%d query=%d", fake.execCount(), fake.queryCount())
			}
		})
	}
}

// TestReleaseRemovedOutcomeContract:删角之后的释放不看状态与登记时刻,删到 = Deleted,删不到(不在,或已归别人)= Absent;
// 名字行之后删 owner 行,锁冲突按同一套预算重来。
func TestReleaseRemovedOutcomeContract(t *testing.T) {
	for _, tc := range []struct {
		name           string
		script         fakeNameScript
		wantOutcome    ReleaseOutcome
		wantErr        bool
		wantNameExecs  int
		wantOwnerExecs int
	}{
		{
			name:          "删到了_owner行一起删",
			script:        fakeNameScript{execs: []fakeNameExec{{affected: 1}}, ownerExecs: []fakeNameExec{{affected: 1}}},
			wantOutcome:   ReleaseDeleted,
			wantNameExecs: 1, wantOwnerExecs: 1,
		},
		{
			name:          "行不在或已归别人_幂等成功",
			script:        fakeNameScript{execs: []fakeNameExec{{affected: 0}}, ownerExecs: []fakeNameExec{{affected: 0}}},
			wantOutcome:   ReleaseAbsent,
			wantNameExecs: 1, wantOwnerExecs: 1,
		},
		{
			name: "1213_退避后重来_删到",
			script: fakeNameScript{
				execs:      []fakeNameExec{{err: mysqlErrNo(mysqlErrDeadlock)}, {affected: 1}},
				ownerExecs: []fakeNameExec{{affected: 1}},
			},
			wantOutcome:   ReleaseDeleted,
			wantNameExecs: 2, wantOwnerExecs: 1,
		},
		{
			name: "1213_用尽预算_owner行不动",
			script: fakeNameScript{execs: []fakeNameExec{
				{err: mysqlErrNo(mysqlErrDeadlock)},
				{err: mysqlErrNo(mysqlErrDeadlock)},
				{err: mysqlErrNo(mysqlErrDeadlock)},
			}},
			wantErr:       true,
			wantNameExecs: playerNameReleaseAttempts,
		},
		{
			name:          "名字行删了_owner行删失败_回错误",
			script:        fakeNameScript{execs: []fakeNameExec{{affected: 1}}, ownerExecs: []fakeNameExec{{err: mysqlErrNo(1146)}}},
			wantErr:       true,
			wantNameExecs: 1, wantOwnerExecs: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, fake := newFakeNameStore(t, tc.script)
			outcome, err := st.ReleaseRemoved(context.Background(), contractNamePlayer, contractNorm)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got outcome=%s", outcome)
				}
			} else if err != nil || outcome != tc.wantOutcome {
				t.Fatalf("ReleaseRemoved = %s, %v, want %s", outcome, err, tc.wantOutcome)
			}
			if got := fake.nameExecCount(); got != tc.wantNameExecs {
				t.Fatalf("名字行上发出 %d 条 DELETE,want %d", got, tc.wantNameExecs)
			}
			if got := fake.ownerExecCount(); got != tc.wantOwnerExecs {
				t.Fatalf("owner 行上发出 %d 条 DELETE,want %d", got, tc.wantOwnerExecs)
			}
			if fake.queryCount() != 0 {
				t.Fatalf("删角之后的释放不该再读:query=%d", fake.queryCount())
			}
			want := []driver.Value{int64(contractNamePlayer), contractNorm}
			for i := 0; i < fake.execCount(); i++ {
				stmt := fake.execAt(i)
				if stmt.sql != playerNameReleaseRemovedSQL && stmt.sql != playerNameOwnerDeleteSQL {
					t.Fatalf("第 %d 条写语句不是生产文本:%q", i, stmt.sql)
				}
				if !reflect.DeepEqual(stmt.args, want) {
					t.Fatalf("第 %d 条写语句的实参不对:\n got=%#v\nwant=%#v", i, stmt.args, want)
				}
			}
		})
	}
}

// TestNameRowStateReadsOneRowAndWritesNothing:释放之前那一次只读。发的就是 playerNameRowStateSQL、实参 (player_id, name_norm),
// 两列按 (created_ms, bound_ms) 回;没有这一行回 found=false 不是错误;一条写都不发;入参非法一条语句都不发。
func TestNameRowStateReadsOneRowAndWritesNothing(t *testing.T) {
	st, fake := newFakeNameStore(t, fakeNameScript{rowState: []fakeNamePair{
		{first: contractNowMs, second: contractNowMs + 5, found: true},
		{found: false},
	}})
	ctx := context.Background()
	createdMs, boundMs, found, err := st.NameRowState(ctx, contractNamePlayer, contractNorm)
	if err != nil || !found || createdMs != contractNowMs || boundMs != contractNowMs+5 {
		t.Fatalf("NameRowState = (%d, %d, %t, %v)", createdMs, boundMs, found, err)
	}
	if _, _, found, err = st.NameRowState(ctx, contractNamePlayer, contractNorm); err != nil || found {
		t.Fatalf("没有这一行要回 found=false 且不是错误:found=%t err=%v", found, err)
	}
	if fake.execCount() != 0 || fake.queryCount() != 2 {
		t.Fatalf("只读:exec=%d query=%d", fake.execCount(), fake.queryCount())
	}
	want := []driver.Value{int64(contractNamePlayer), contractNorm}
	for i := 0; i < 2; i++ {
		if stmt := fake.queryAt(i); stmt.sql != playerNameRowStateSQL || !reflect.DeepEqual(stmt.args, want) {
			t.Fatalf("第 %d 条读不是生产文本或实参不对:%q %#v", i, stmt.sql, stmt.args)
		}
	}

	for _, bad := range []struct {
		player uint64
		norm   string
	}{{0, contractNorm}, {contractNamePlayer, ""}} {
		empty, emptyFake := newFakeNameStore(t, fakeNameScript{})
		if _, _, _, err := empty.NameRowState(ctx, bad.player, bad.norm); !errors.Is(err, ErrPlayerNameInvalidArgument) {
			t.Fatalf("player_id=%d name_norm=%q: want ErrPlayerNameInvalidArgument, got %v", bad.player, bad.norm, err)
		}
		if emptyFake.queryCount() != 0 {
			t.Fatalf("入参非法不该碰库:query=%d", emptyFake.queryCount())
		}
	}
}

// TestUndoRestoreDeletesOnlyThisRestore:撤销只删本次补登记写下的那一行(player_id、判重键、登记时刻、绑定时刻四个都对上),
// 删到了才删名字副本;没删到两边都不动;入参非法一条语句都不发。
func TestUndoRestoreDeletesOnlyThisRestore(t *testing.T) {
	ctx := context.Background()
	st, fake := newFakeNameStore(t, fakeNameScript{execs: []fakeNameExec{{affected: 1}}, ownerExecs: []fakeNameExec{{affected: 1}}})
	outcome, err := st.UndoRestore(ctx, contractNamePlayer, contractNorm, contractNowMs)
	if err != nil || outcome != ReleaseDeleted {
		t.Fatalf("UndoRestore = %s, %v", outcome, err)
	}
	want := []driver.Value{int64(contractNamePlayer), contractNorm, int64(contractNowMs), int64(contractNowMs)}
	if got := fake.execAt(0); got.sql != playerNameUndoRestoreSQL || !reflect.DeepEqual(got.args, want) {
		t.Fatalf("撤销语句或实参不对:%q %#v", got.sql, got.args)
	}
	if fake.execCount() != 2 || fake.execAt(1).sql != playerNameOwnerDeleteSQL {
		t.Fatalf("删到了要接着删名字副本:exec=%d", fake.execCount())
	}

	st, fake = newFakeNameStore(t, fakeNameScript{execs: []fakeNameExec{{affected: 0}}})
	outcome, err = st.UndoRestore(ctx, contractNamePlayer, contractNorm, contractNowMs)
	if err != nil || outcome != ReleaseAbsent || fake.ownerExecCount() != 0 {
		t.Fatalf("没删到要回 absent 且不碰名字副本:%s %v owner=%d", outcome, err, fake.ownerExecCount())
	}

	for _, bad := range []struct {
		player uint64
		norm   string
		ms     uint64
	}{{0, contractNorm, contractNowMs}, {contractNamePlayer, "", contractNowMs}, {contractNamePlayer, contractNorm, 0}} {
		empty, emptyFake := newFakeNameStore(t, fakeNameScript{})
		if _, err := empty.UndoRestore(ctx, bad.player, bad.norm, bad.ms); !errors.Is(err, ErrPlayerNameInvalidArgument) {
			t.Fatalf("%+v: want ErrPlayerNameInvalidArgument, got %v", bad, err)
		}
		if emptyFake.execCount() != 0 {
			t.Fatalf("%+v: 入参非法不该碰库", bad)
		}
	}
	if !strings.HasPrefix(playerNameUndoRestoreSQL, "DELETE player_name FROM player_name FORCE INDEX ("+playerNameReleaseForcedIndex+")") ||
		!strings.HasSuffix(playerNameUndoRestoreSQL, " WHERE player_id = ? AND name_norm = ? AND created_ms = ? AND bound_ms = ?") {
		t.Fatalf("撤销语句被改过:%q", playerNameUndoRestoreSQL)
	}
}

// TestBoundToReadsTheOwnersNameRow:按名字副本找判重键,再按主键读名字行,一条写都不发。只有名字行在这名角色名下而且已绑定才算;
// 没有名字副本时不再读名字行。
func TestBoundToReadsTheOwnersNameRow(t *testing.T) {
	ctx := context.Background()
	owned := []fakeNameLookup{{value: contractNorm, found: true}}
	for _, tc := range []struct {
		name        string
		script      fakeNameScript
		want        bool
		wantQueries int
	}{
		{"没有名字副本", fakeNameScript{owned: []fakeNameLookup{{found: false}}}, false, 1},
		{"已绑定在本角色名下", fakeNameScript{owned: owned, holding: []fakeNamePair{{first: contractNamePlayer, second: contractNowMs, found: true}}}, true, 2},
		{"还是预留态", fakeNameScript{owned: owned, holding: []fakeNamePair{{first: contractNamePlayer, found: true}}}, false, 2},
		{"归了别人", fakeNameScript{owned: owned, holding: []fakeNamePair{{first: contractOther, second: contractNowMs, found: true}}}, false, 2},
		{"名字行不在", fakeNameScript{owned: owned, holding: []fakeNamePair{{found: false}}}, false, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, fake := newFakeNameStore(t, tc.script)
			got, err := st.BoundTo(ctx, contractNamePlayer)
			if err != nil || got != tc.want {
				t.Fatalf("BoundTo = %t, %v, want %t", got, err, tc.want)
			}
			if fake.queryCount() != tc.wantQueries || fake.execCount() != 0 {
				t.Fatalf("只读:query=%d(want %d) exec=%d", fake.queryCount(), tc.wantQueries, fake.execCount())
			}
		})
	}
	if _, err := (&PlayerNameStore{}).BoundTo(ctx, 0); !errors.Is(err, ErrPlayerNameInvalidArgument) {
		t.Fatalf("player_id=0: want ErrPlayerNameInvalidArgument, got %v", err)
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
	const wantSQL = "SELECT player_id, name FROM player_name_owner WHERE player_id IN (?,?,?)"
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

// ── K=4:两张表落在不同的片上 ───────────────────────────────────────────────────

// 这几个值的片号由 shared/dirshard 的金样钉住:yunzhongjun 在 K=4 第 1 片,玩家 4242 在 K=4 第 2 片。
const (
	shardedNameSlot   = 1
	shardedPlayerSlot = 2
)

func TestShardedReserveWritesEachTableOnItsOwnShard(t *testing.T) {
	st, fakes := newFakeNameShards(t, map[int]fakeNameScript{
		shardedNameSlot:   {execs: []fakeNameExec{{affected: 1}}},
		shardedPlayerSlot: {owned: []fakeNameLookup{{found: false}}, ownerExecs: []fakeNameExec{{affected: 1}}},
	})
	outcome, _, err := st.Reserve(context.Background(), contractNamePlayer, contractName, contractNorm, contractNowMs)
	if err != nil || outcome != ReserveInserted {
		t.Fatalf("Reserve = %s, %v", outcome, err)
	}
	if fakes[shardedNameSlot].nameExecCount() != 1 || fakes[shardedNameSlot].ownerExecCount() != 0 {
		t.Fatal("名字行要写在名字所在的片,这片不该有 owner 行")
	}
	if fakes[shardedPlayerSlot].ownerExecCount() != 1 || fakes[shardedPlayerSlot].nameExecCount() != 0 {
		t.Fatal("owner 行要写在玩家所在的片,这片不该有名字行")
	}
	for slot, fake := range fakes {
		if slot != shardedNameSlot && slot != shardedPlayerSlot && (fake.execCount() != 0 || fake.queryCount() != 0) {
			t.Fatalf("第 %d 片不该被碰到", slot)
		}
	}
}

// 撞名的判定问名字所在的片,不问玩家所在的片。
func TestShardedReserveTakenAsksTheNameShard(t *testing.T) {
	st, fakes := newFakeNameShards(t, map[int]fakeNameScript{
		shardedNameSlot: {
			execs: []fakeNameExec{{affected: 0}},
			owner: []fakeNameLookup{{value: contractOther, found: true}},
		},
		shardedPlayerSlot: {owned: []fakeNameLookup{{found: false}}},
	})
	outcome, owner, err := st.Reserve(context.Background(), contractNamePlayer, contractName, contractNorm, contractNowMs)
	if err != nil || outcome != ReserveTaken || owner != contractOther {
		t.Fatalf("Reserve = %s, %d, %v", outcome, owner, err)
	}
	if fakes[shardedPlayerSlot].ownerExecCount() != 0 {
		t.Fatal("名字被别人占了,不能写 owner 行")
	}
}

func TestShardedReleaseDeletesEachRowOnItsOwnShard(t *testing.T) {
	st, fakes := newFakeNameShards(t, map[int]fakeNameScript{
		shardedNameSlot:   {execs: []fakeNameExec{{affected: 1}}},
		shardedPlayerSlot: {ownerExecs: []fakeNameExec{{affected: 1}}},
	})
	outcome, err := st.Release(context.Background(), contractNamePlayer, contractNorm, 0)
	if err != nil || outcome != ReleaseDeleted {
		t.Fatalf("Release = %s, %v", outcome, err)
	}
	if fakes[shardedNameSlot].nameExecCount() != 1 || fakes[shardedPlayerSlot].ownerExecCount() != 1 {
		t.Fatal("两行要各在自己的片上删")
	}
}

// 绑定只改名字所在的片;删角之后的释放和 Release 一样,两行各在自己的片上删。别的片一条语句都不该收到(脚本用尽即报错)。
func TestShardedBindAndReleaseRemovedTouchOnlyTheirShards(t *testing.T) {
	st, fakes := newFakeNameShards(t, map[int]fakeNameScript{
		shardedNameSlot:   {execs: []fakeNameExec{{affected: 1}, {affected: 1}}},
		shardedPlayerSlot: {ownerExecs: []fakeNameExec{{affected: 1}}},
	})
	ctx := context.Background()
	if outcome, _, err := st.Bind(ctx, contractNamePlayer, contractName, contractNorm, contractNowMs); err != nil || outcome != BindBound {
		t.Fatalf("Bind = %s, %v", outcome, err)
	}
	if fakes[shardedPlayerSlot].execCount() != 0 {
		t.Fatal("绑定只改名字行,不该碰玩家片")
	}
	if outcome, err := st.ReleaseRemoved(ctx, contractNamePlayer, contractNorm); err != nil || outcome != ReleaseDeleted {
		t.Fatalf("ReleaseRemoved = %s, %v", outcome, err)
	}
	if fakes[shardedNameSlot].nameExecCount() != 2 || fakes[shardedPlayerSlot].ownerExecCount() != 1 {
		t.Fatal("名字行在名字片、owner 行在玩家片")
	}
	for slot, fake := range fakes {
		if slot != shardedNameSlot && slot != shardedPlayerSlot && (fake.execCount() != 0 || fake.queryCount() != 0) {
			t.Fatalf("第 %d 片不该被碰到", slot)
		}
	}
}

// 批量读按玩家分片拆组,只碰到有玩家的片,不做全分片扫描。任一片失败整批报错:半份结果会被当成「这些人没有名字」。
func TestShardedBatchGetGroupsByPlayerShard(t *testing.T) {
	// K=4:1、5 在第 1 片,4 在第 0 片,第 2、3 片没有人。
	ids := []uint64{1, 4, 5}
	st, fakes := newFakeNameShards(t, map[int]fakeNameScript{
		0: {batch: []fakeNameBatch{{rows: []fakeNameRow{{id: 4, name: "Si"}}}}},
		1: {batch: []fakeNameBatch{{rows: []fakeNameRow{{id: 1, name: "Yi"}, {id: 5, name: "Wu"}}}}},
	})
	got, err := st.BatchGet(context.Background(), ids)
	if err != nil {
		t.Fatalf("BatchGet: %v", err)
	}
	if want := (map[uint64]string{1: "Yi", 4: "Si", 5: "Wu"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("BatchGet = %#v, want %#v", got, want)
	}
	if fakes[2].queryCount() != 0 || fakes[3].queryCount() != 0 {
		t.Fatal("没有玩家的片不该被查")
	}
	stmt := fakes[1].queryAt(0)
	if want := []driver.Value{int64(1), int64(5)}; !reflect.DeepEqual(stmt.args, want) {
		t.Fatalf("第 1 片的实参 = %#v, want %#v", stmt.args, want)
	}

	boom := errors.New("shard down")
	broken, _ := newFakeNameShards(t, map[int]fakeNameScript{
		0: {batch: []fakeNameBatch{{err: boom}}},
		1: {batch: []fakeNameBatch{{rows: []fakeNameRow{{id: 1, name: "Yi"}}}}},
	})
	if got, err := broken.BatchGet(context.Background(), ids); !errors.Is(err, boom) || got != nil {
		t.Fatalf("一片失败要整批报错:got=%#v err=%v", got, err)
	}
}

// ── 语句文本的形状 ──────────────────────────────────────────────────────────────

// TestReserveSQLUpdateClauseIsNoOp 钉住 Reserve 判定口径的**另一半**前提(见 Reserve 的「写法」):
// ODKU 的更新子句必须是 no-op(`player_id = player_id`,引用已有行自己的列值)。一旦它真改了数据,
// 受影响行数会变成 2,"0=撞键 / 1=插入"的口径就失效;若改成刷 created_ms,还会把释放窗口整体推后。
func TestReserveSQLUpdateClauseIsNoOp(t *testing.T) {
	const marker = "ON DUPLICATE KEY UPDATE"
	for _, stmt := range []string{playerNameReserveSQL, playerNameOwnerUpsertSQL} {
		idx := strings.Index(stmt, marker)
		if idx < 0 {
			t.Fatalf("%q 不再是 ODKU:改回普通 INSERT 会把**聚簇索引**上的重复检查从 X 退回 S,"+
				"两个 S 同时被授予之后各自要升级成 X、互相挡住(手册的三会话例就建在聚簇主键上)。"+
				"2026-09-29 真库探针实测:新结构配普通 INSERT 是 15/15 全成环,三种 player_id 顺序一个都跑不掉 —— "+
				"「名字是聚簇主键」与「ODKU」两个条件缺一不可", stmt)
		}
		update := strings.TrimSpace(stmt[idx+len(marker):])
		if update != "player_id = player_id" {
			t.Fatalf("%q 的更新子句是 %q,必须保持 no-op 的 `player_id = player_id`", stmt, update)
		}
		if strings.Contains(stmt, "VALUES(") {
			t.Fatalf("%q 的更新子句引用了 VALUES():那会真的改数据,受影响行数变 2,Reserve 的行数口径失效", stmt)
		}
	}
}

// TestReleaseSQLShape:单表 DELETE 不接受索引提示,所以是多表语法;三个条件的占位符顺序就是绑参顺序。
// 真库用例用 EXPLAIN 再验一次执行计划;这一条在没有 Docker 时也能拦住文本被改回去。
func TestReleaseSQLShape(t *testing.T) {
	if !strings.Contains(playerNameReleaseSQL, "FORCE INDEX ("+playerNameReleaseForcedIndex+")") {
		t.Fatalf("丢了 FORCE INDEX (%s):%q", playerNameReleaseForcedIndex, playerNameReleaseSQL)
	}
	if !strings.HasPrefix(playerNameReleaseSQL, "DELETE player_name FROM player_name") {
		t.Fatalf("不再是多表 DELETE 语法(单表 DELETE 不接受索引提示):%q", playerNameReleaseSQL)
	}
	// 三个占位符的顺序就是绑参顺序;`bound_ms = 0` 是字面量,不占绑参:少了它,建角回滚就能删掉建成的角色的名字。
	if !strings.HasSuffix(playerNameReleaseSQL, "WHERE player_id = ? AND name_norm = ? AND created_ms >= ? AND bound_ms = 0") {
		t.Fatalf("条件被改过,绑参顺序与「只删预留态」都要跟着看:%q", playerNameReleaseSQL)
	}
	// owner 行按名字删:这个玩家名下已经是别的名字时不能动。
	if !strings.HasSuffix(playerNameOwnerDeleteSQL, "WHERE player_id = ? AND name_norm = ?") {
		t.Fatalf("owner 行的删除必须带名字:%q", playerNameOwnerDeleteSQL)
	}
}

// TestBindAndReleaseRemovedSQLShape:绑定只改「这名角色的、预留态的」那一行;删角之后的释放钉 PRIMARY、只按 (player_id, name_norm) 删,
// 不带状态与时间条件(角色已删,墓碑由目录核过)。
func TestBindAndReleaseRemovedSQLShape(t *testing.T) {
	if playerNameBindSQL != "UPDATE player_name SET bound_ms = ? WHERE name_norm = ? AND player_id = ? AND bound_ms = 0" {
		t.Fatalf("绑定语句被改过:必须按主键点改、只改本角色的预留态行,绑参顺序 (bound_ms, name_norm, player_id):%q", playerNameBindSQL)
	}
	if !strings.HasPrefix(playerNameReleaseRemovedSQL, "DELETE player_name FROM player_name FORCE INDEX ("+playerNameReleaseForcedIndex+")") {
		t.Fatalf("删角之后的释放不再是钉 PRIMARY 的多表 DELETE:%q", playerNameReleaseRemovedSQL)
	}
	if !strings.HasSuffix(playerNameReleaseRemovedSQL, " WHERE player_id = ? AND name_norm = ?") {
		t.Fatalf("删角之后的释放条件被改过:%q", playerNameReleaseRemovedSQL)
	}
	if playerNameRowStateSQL != "SELECT created_ms, bound_ms FROM player_name WHERE player_id = ? AND name_norm = ?" ||
		playerNameHoldingSQL != "SELECT player_id, bound_ms FROM player_name WHERE name_norm = ?" {
		t.Fatal("两条分辨成因的读被改过:列序就是 Scan 的顺序")
	}
}

// TestBootstrapDDLMatchesStoreAssumptions 把 schema.go 的建表语句与本 store 的假设钉在一起。
//
// 这两份真相分处两个文件:store 假定"名字是聚簇主键、表上没有别的键,player_id 的唯一性在 player_name_owner 的主键上",
// 而真正建表的是 schema.go。本用例不连库,是这两处漂移在 `go test ./...` 里唯一的拦截点。
func TestBootstrapDDLMatchesStoreAssumptions(t *testing.T) {
	if !strings.Contains(playerNameBootstrapDDL, "PRIMARY KEY (`name_norm`)") {
		t.Fatalf("playerNameBootstrapDDL 的主键不是 name_norm:%q —— 名字抢注的死锁根治(真库实测 0/15)"+
			"的全部前提就是「名字是聚簇主键」,改回二级唯一索引会让生产常态那一路重新 5/5 成环", playerNameBootstrapDDL)
	}
	if strings.Contains(playerNameBootstrapDDL, "UNIQUE KEY") || strings.Contains(playerNameBootstrapDDL, "  KEY ") {
		t.Fatalf("playerNameBootstrapDDL 上不能再有二级键:%q —— 名字按 name_norm 分片,按 player_id 查的是 player_name_owner;"+
			"name_norm 上的二级唯一索引还会把根治掉的那条环原样带回来", playerNameBootstrapDDL)
	}
	if !strings.Contains(playerNameOwnerBootstrapDDL, "PRIMARY KEY (`player_id`)") {
		t.Fatalf("playerNameOwnerBootstrapDDL 的主键不是 player_id:%q —— 它是「一个角色只登记一个名字」的全部实现", playerNameOwnerBootstrapDDL)
	}
	for table, ddl := range map[string]string{PlayerNameTableName: playerNameBootstrapDDL, PlayerNameOwnerTableName: playerNameOwnerBootstrapDDL} {
		if !strings.Contains(ddl, "COLLATE=utf8mb4_bin") {
			t.Fatalf("%s 丢了 utf8mb4_bin:归一化在 Go 侧做,库必须逐字节比较", table)
		}
		if bootstrapTables[table] != ddl {
			t.Fatalf("bootstrapTables[%s] 不是它的 DDL 常量:迁移不会建这张表", table)
		}
	}
	// 两条主键也要进启动期的唯一性守卫,否则被人从存量库上改掉时没人会发现。
	for table, want := range map[string]string{PlayerNameTableName: "name_norm", PlayerNameOwnerTableName: "player_id"} {
		if got := bootstrapUniqueColumns[table]; !reflect.DeepEqual(got, []string{want}) {
			t.Fatalf("bootstrapUniqueColumns[%s] = %v,want [%s]", table, got, want)
		}
	}
}

// TestOutcomeStringsAreStableLowCardinality:三个 outcome 的 String() 进日志与指标 label,
// 值必须稳定且低基数(四个 / 四个 / 五个固定值),改一个字就会让既有看板与告警规则错位。
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
		{ReleaseBound.String(), "bound"},
		{ReleaseOutcome(0).String(), "unknown"},
		{BindBound.String(), "bound"},
		{BindAlreadyBound.String(), "already_bound"},
		{BindRestored.String(), "restored"},
		{BindTaken.String(), "taken"},
		{BindConflict.String(), "conflict"},
		{BindOutcome(0).String(), "unknown"},
	} {
		if tc.got != tc.want {
			t.Fatalf("outcome label = %q, want %q", tc.got, tc.want)
		}
	}
}
