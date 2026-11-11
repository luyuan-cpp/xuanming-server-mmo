package commerce

// 聚宝斋合同(commerce)客户端写路径「请求截止之后不提交」的回归测试(docs/design/client-request-ordering.md §6.1),不连库。
//
// 客户端写方法(logic.JubaozhaiLogic 经 commerceCall / Rmb 入口)落到三条存储路径:
//   - Engine.Create(上架寄售 / 拍卖 / 材料挂单 / 面对面交易 / 出价 / 人民币寄售):重放预读 → 领号 → roleTx 里插合同;
//   - Engine.Mutate(确认 / 购买 / 撤单):预读 → roleTx 里锁合同、改状态、按版本重存;
//   - RmbConsignments.Cancel:预读 → roleTx 里撤单。
// roleTx = Roles.RunTx(ctx) → CommerceRepo.Tx(ctx) → assetop.WithTxRetry(ctx):每次尝试前查 ctx,BeginTx 与事务内每条语句
// 都用请求 ctx,Tx.Commit 在 ctx 结束后不发 COMMIT(生产的写者池 authoritysql 在 COMMIT 前还会再查一次)。
// 这条链上任何一环被换成 context.Background / WithoutCancel(例如「客户端走了也把提交做完」),下面的用例就判红。
//
// 用录制驱动钉住「到达驱动的语句与顺序」;语句按标签记录(fenceLabel),认不出的原样记下,便于看出多发了什么。

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tradepb "proto/trade"
	"shared/currencytype"
	"trade/internal/data"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

const (
	fenceSeller    = uint64(1001)
	fenceBuyer     = uint64(1002)
	fenceContract  = uint64(7001)
	fenceNewID     = uint64(9001)
	fenceDBNowMs   = int64(1_000)
	fenceRequestID = "fence-request-0001"
)

var errFenceDriver = errors.New("截止栅栏录制驱动:这条语句不该到达驱动")

// fenceLabel 把语句归成短标签,断言读起来就是调用顺序。
func fenceLabel(query string) string {
	q := strings.Join(strings.Fields(query), " ")
	switch {
	case q == "BEGIN" || q == "COMMIT" || q == "ROLLBACK":
		return q
	case strings.Contains(q, "FROM trade_commerce WHERE request_key="):
		return "byRequest"
	case strings.Contains(q, "FROM trade_commerce WHERE commerce_id=") && strings.HasSuffix(q, "FOR UPDATE"):
		return "lock"
	case strings.Contains(q, "FROM trade_commerce WHERE commerce_id="):
		return "get"
	case strings.HasPrefix(q, "SELECT CAST(UNIX_TIMESTAMP("):
		return "dbNow"
	case strings.HasPrefix(q, "UPDATE trade_commerce SET status="):
		return "save"
	case strings.HasPrefix(q, "INSERT INTO trade_commerce ("):
		return "insert"
	}
	return q
}

// fenceDB 是录制驱动的共享状态。查询按标签回 rows 里登记的结果集,没登记的一律报错;写语句交给 exec,nil 时一律报错。
type fenceDB struct {
	mu        sync.Mutex
	stmts     []string
	isolation driver.IsolationLevel
	rows      map[string]func() ([]string, [][]driver.Value)
	exec      func(label string) (driver.Result, error)
	// txEnded 在事务结束时收到 COMMIT 或 ROLLBACK。ctx 结束后的回滚由 database/sql 的后台协程执行,
	// 可能晚于被测方法返回,所以等这个驱动事件,而不是方法一返回就断言。
	txEnded chan string
}

func newFenceDB() *fenceDB {
	return &fenceDB{rows: map[string]func() ([]string, [][]driver.Value){}, txEnded: make(chan string, 4)}
}

func (f *fenceDB) record(label string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stmts = append(f.stmts, label)
}

func (f *fenceDB) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.stmts...)
}

func (f *fenceDB) beginIsolation() driver.IsolationLevel {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.isolation
}

// awaitTxEnd 的上限只用于事务永不结束(回归)时及早判红,正常路径不依赖它。
func (f *fenceDB) awaitTxEnd(t *testing.T) string {
	t.Helper()
	select {
	case stmt := <-f.txEnded:
		return stmt
	case <-time.After(10 * time.Second):
		t.Fatal("事务既没有提交也没有回滚")
		return ""
	}
}

func (f *fenceDB) open(t *testing.T) *sql.DB {
	t.Helper()
	db := sql.OpenDB(fenceConnector{f: f})
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type fenceConnector struct{ f *fenceDB }

func (c fenceConnector) Connect(context.Context) (driver.Conn, error) { return fenceConn{f: c.f}, nil }
func (c fenceConnector) Driver() driver.Driver                        { return fenceDriver{f: c.f} }

type fenceDriver struct{ f *fenceDB }

func (d fenceDriver) Open(string) (driver.Conn, error) { return fenceConn{f: d.f}, nil }

type fenceConn struct{ f *fenceDB }

func (c fenceConn) Prepare(query string) (driver.Stmt, error) {
	c.f.record(fenceLabel(query))
	return nil, errFenceDriver
}
func (c fenceConn) Close() error { return nil }
func (c fenceConn) Begin() (driver.Tx, error) {
	c.f.record("BEGIN")
	return nil, errFenceDriver
}
func (c fenceConn) BeginTx(_ context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.f.record("BEGIN")
	c.f.mu.Lock()
	c.f.isolation = opts.Isolation
	c.f.mu.Unlock()
	return fenceTx{f: c.f}, nil
}
func (c fenceConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	label := fenceLabel(query)
	c.f.record(label)
	if c.f.exec == nil {
		return nil, errFenceDriver
	}
	return c.f.exec(label)
}
func (c fenceConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	label := fenceLabel(query)
	c.f.record(label)
	answer := c.f.rows[label]
	if answer == nil {
		return nil, errFenceDriver
	}
	columns, values := answer()
	return &fenceRows{columns: columns, values: values}, nil
}

type fenceTx struct{ f *fenceDB }

func (t fenceTx) Commit() error   { t.f.record("COMMIT"); t.f.txEnded <- "COMMIT"; return nil }
func (t fenceTx) Rollback() error { t.f.record("ROLLBACK"); t.f.txEnded <- "ROLLBACK"; return nil }

type fenceRows struct {
	columns []string
	values  [][]driver.Value
	next    int
}

func (r *fenceRows) Columns() []string { return r.columns }
func (r *fenceRows) Close() error      { return nil }
func (r *fenceRows) Next(dest []driver.Value) error {
	if r.next >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.next])
	r.next++
	return nil
}

// fenceCommerceRow 按 data.CommerceColumns 的列序把合同摆成一行;没列出的列给 0(另一会话加的新列不会让夹具失配)。
func fenceCommerceRow(r *tradepb.TradeCommerceRecord) func() ([]string, [][]driver.Value) {
	return func() ([]string, [][]driver.Value) {
		columns := strings.Split(data.CommerceColumns, ",")
		byName := map[string]driver.Value{
			"commerce_id": int64(r.CommerceId), "request_key": r.RequestKey, "request_digest": r.RequestDigest,
			"kind": int64(r.Kind), "status": int64(r.Status), "seller_player_id": int64(r.SellerPlayerId),
			"buyer_player_id": int64(r.BuyerPlayerId), "unit_price": int64(r.UnitPrice), "currency_type": int64(r.CurrencyType),
			"expires_ms": int64(r.ExpiresMs), "created_ms": int64(r.CreatedMs), "updated_ms": int64(r.UpdatedMs),
			"version": int64(r.Version), "workflow_proto": r.WorkflowProto,
		}
		row := make([]driver.Value, len(columns))
		for i, column := range columns {
			if v, ok := byName[column]; ok {
				row[i] = v
			} else {
				row[i] = int64(0)
			}
		}
		return columns, [][]driver.Value{row}
	}
}

// fenceNoRows 是查不到行的结果集(重放预读没有旧合同)。
func fenceNoRows() ([]string, [][]driver.Value) {
	return strings.Split(data.CommerceColumns, ","), nil
}

func fenceDBNow() ([]string, [][]driver.Value) {
	return []string{"now_ms"}, [][]driver.Value{{fenceDBNowMs}}
}

// fenceIDs 记下领号次数;ctx 已结束时与真实号段客户端一样不发号。
type fenceIDs struct{ calls atomic.Int32 }

func (i *fenceIDs) Next(ctx context.Context) (uint64, error) {
	i.calls.Add(1)
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return fenceNewID, nil
}

func fenceEngineOn(db *sql.DB, ids *fenceIDs) *Engine {
	return &Engine{
		Repo:   &data.CommerceRepo{DB: db, Ops: &data.AssetOpRepo{}},
		IDs:    ids,
		Policy: Policy{Items: []uint32{10}, Currencies: []uint32{currencytype.Diamond}},
		Roles:  fixtureRoles{},
		Now:    func() time.Time { return time.UnixMilli(fenceDBNowMs) },
	}
}

// fenceConfirmContract 是一笔等买家确认的面对面交易:卖家托管已到账,买家确认后进入 buyer_escrow 并按版本重存。
func fenceConfirmContract(t *testing.T) *tradepb.TradeCommerceRecord {
	t.Helper()
	w := &tradepb.TradeCommerceWorkflow{
		Stage:          "confirm",
		SellerIdentity: &tradepb.TradeRoleIdentity{Account: "account-1001", OwnerRevision: 1},
		BuyerIdentity:  &tradepb.TradeRoleIdentity{Account: "account-1002", OwnerRevision: 1},
		Legs:           []*tradepb.TradeCommerceLeg{{Role: "seller_debit", PlayerId: fenceSeller, Outcome: 2}},
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(w)
	require.NoError(t, err)
	return &tradepb.TradeCommerceRecord{CommerceId: fenceContract, Kind: 2, Status: Escrowing, SellerPlayerId: fenceSeller,
		BuyerPlayerId: fenceBuyer, ExpiresMs: 1 << 40, Version: 3, WorkflowProto: raw}
}

func fenceConsignmentRequest() *tradepb.CreateConsignmentRequest {
	return &tradepb.CreateConsignmentRequest{Assets: &tradepb.TradeAssetSelection{Items: map[uint32]uint32{10: 1}}, Price: 5,
		CurrencyType: currencytype.Diamond, DurationMs: 60000, ClientRequestId: fenceRequestID}
}

// 已截止的请求 ctx(go-zero 超时拦截器到期后 handler 拿到的就是这种):每个客户端写入口都不得有语句到达驱动(含 BEGIN),也不领号。
func TestCommerceClientWritesAfterRequestDeadlineSendNoStatement(t *testing.T) {
	selling := &tradepb.TradeAssetSelection{Items: map[uint32]uint32{10: 1}}
	paying := &tradepb.TradeAssetSelection{CurrencyType: currencytype.Diamond, CurrencyAmount: 5}
	cases := []struct {
		name string
		call func(ctx context.Context, e *Engine) error
	}{
		{"CreatePlayerTrade", func(ctx context.Context, e *Engine) error {
			_, err := e.CreatePlayerTrade(ctx, fenceSeller, &tradepb.CreatePlayerTradeRequest{BuyerPlayerId: fenceBuyer, SellerAssets: selling, BuyerAssets: paying, ClientRequestId: fenceRequestID})
			return err
		}},
		{"CreateConsignment", func(ctx context.Context, e *Engine) error {
			_, err := e.CreateConsignment(ctx, fenceSeller, fenceConsignmentRequest())
			return err
		}},
		{"CreateLot", func(ctx context.Context, e *Engine) error {
			_, err := e.CreateLot(ctx, fenceSeller, &tradepb.CreateAuctionLotRequest{Assets: selling, OpeningPrice: 5, CurrencyType: currencytype.Diamond, MinimumIncrement: 1, DurationMs: 60000, ClientRequestId: fenceRequestID})
			return err
		}},
		{"PlaceMarket", func(ctx context.Context, e *Engine) error {
			_, err := e.PlaceMarket(ctx, fenceSeller, &tradepb.PlaceMarketOrderRequest{MarketId: 1, ItemConfigId: 10, Side: tradepb.MarketOrderSide(2), Quantity: 1, UnitPrice: 5, CurrencyType: currencytype.Diamond, ClientRequestId: fenceRequestID})
			return err
		}},
		{"PlaceBid", func(ctx context.Context, e *Engine) error {
			_, err := e.PlaceBid(ctx, fenceBuyer, fenceContract, &tradepb.PlaceAuctionBidRequest{Amount: 6, ClientRequestId: fenceRequestID})
			return err
		}},
		{"Mutate/confirm", func(ctx context.Context, e *Engine) error {
			_, err := e.Mutate(ctx, fenceBuyer, fenceContract, "confirm")
			return err
		}},
		{"Mutate/buy", func(ctx context.Context, e *Engine) error {
			_, err := e.Mutate(ctx, fenceBuyer, fenceContract, "buy", fenceRequestID)
			return err
		}},
		{"Mutate/cancel", func(ctx context.Context, e *Engine) error {
			_, err := e.Mutate(ctx, fenceSeller, fenceContract, "cancel")
			return err
		}},
		{"RmbConsignments.Cancel", func(ctx context.Context, e *Engine) error {
			return (&RmbConsignments{Engine: e}).Cancel(ctx, "account-1001", fenceContract)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFenceDB()
			ids := &fenceIDs{}
			e := fenceEngineOn(f.open(t), ids)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			err := tc.call(ctx, e)

			require.ErrorIs(t, err, context.Canceled, "截止之后必须如实报错(logic 定性为故障码)")
			require.Empty(t, f.recorded(), "截止之后不得有任何语句到达驱动(含 BEGIN)")
			require.Zero(t, ids.calls.Load(), "截止之后不再领号")
		})
	}
}

// commerceWriteCase 是走到提交点的两类客户端写:Mutate(锁合同后按版本重存)与 Create(插新合同)。
type commerceWriteCase struct {
	name string
	// write 是事务里最后一条业务写的标签,截止就安排在它执行期间。
	write string
	// committed 是截止之前跑完时到达驱动的完整语句序列。
	committed []string
	arrange   func(t *testing.T, f *fenceDB)
	call      func(ctx context.Context, e *Engine) (*tradepb.TradeCommerceRecord, error)
}

func commerceWriteCases() []commerceWriteCase {
	return []commerceWriteCase{
		{
			name:      "Mutate/confirm",
			write:     "save",
			committed: []string{"get", "BEGIN", "lock", "dbNow", "save", "COMMIT"},
			arrange: func(t *testing.T, f *fenceDB) {
				contract := fenceConfirmContract(t)
				f.rows["get"] = fenceCommerceRow(contract)
				f.rows["lock"] = fenceCommerceRow(contract)
				f.rows["dbNow"] = fenceDBNow
			},
			call: func(ctx context.Context, e *Engine) (*tradepb.TradeCommerceRecord, error) {
				return e.Mutate(ctx, fenceBuyer, fenceContract, "confirm")
			},
		},
		{
			name:      "CreateConsignment",
			write:     "insert",
			committed: []string{"byRequest", "BEGIN", "insert", "COMMIT"},
			arrange: func(_ *testing.T, f *fenceDB) {
				f.rows["byRequest"] = fenceNoRows
			},
			call: func(ctx context.Context, e *Engine) (*tradepb.TradeCommerceRecord, error) {
				return e.CreateConsignment(ctx, fenceSeller, fenceConsignmentRequest())
			},
		},
	}
}

// 截止落在事务里最后一条业务写执行期间(典型是它正排在别人的行锁后面):不得发 COMMIT,事务以回滚收场,
// 也不得把这次尝试在内存里改出来、并未提交的合同回给客户端。两种驱动表现都覆盖:语句执行中 ctx 结束、驱动断开连接
// 回 ctx 错误(go-sql-driver 的实际行为);或语句恰好在截止那一刻执行完、驱动照常回结果 —— 后者只能靠提交点的 ctx 检查挡住。
func TestCommerceClientWriteRunningPastDeadlineDoesNotCommit(t *testing.T) {
	results := []struct {
		name   string
		result func() (driver.Result, error)
	}{
		{"驱动在截止时中断语句并回ctx错误", func() (driver.Result, error) { return nil, context.Canceled }},
		{"语句恰在截止时执行完", func() (driver.Result, error) { return driver.RowsAffected(1), nil }},
	}
	for _, write := range commerceWriteCases() {
		for _, res := range results {
			t.Run(write.name+"/"+res.name, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				f := newFenceDB()
				write.arrange(t, f)
				f.exec = func(label string) (driver.Result, error) {
					if label != write.write {
						return nil, errFenceDriver
					}
					cancel() // 请求截止发生在这条写语句执行期间
					return res.result()
				}
				e := fenceEngineOn(f.open(t), &fenceIDs{})

				got, err := write.call(ctx, e)

				require.Error(t, err, "截止之后必须如实报错(logic 定性为故障码)")
				require.Nil(t, got, "没有提交的合同不得回给客户端")
				if ended := f.awaitTxEnd(t); ended != "ROLLBACK" {
					t.Fatalf("跑过截止的事务必须回滚,实际以 %s 结束", ended)
				}
				// 期望恰好是「截止之前跑完」的序列去掉 COMMIT、换成 ROLLBACK:写语句确实发出过(夹具走到了提交点),
				// 截止之后没有重开事务,失败之后的回读也没有到达驱动。
				want := append(append([]string(nil), write.committed[:len(write.committed)-1]...), "ROLLBACK")
				require.Equal(t, want, f.recorded())
				require.Equal(t, driver.IsolationLevel(sql.LevelReadCommitted), f.beginIsolation(), "合同事务的隔离级别必须是 RC")
			})
		}
	}
}

// 正常路径:截止之前跑完,恰好一次 BEGIN → … → COMMIT,不回滚、不重试。它同时证明上一条用例的夹具确实走得到提交点。
func TestCommerceClientWriteWithinDeadlineCommitsOnce(t *testing.T) {
	for _, write := range commerceWriteCases() {
		t.Run(write.name, func(t *testing.T) {
			f := newFenceDB()
			write.arrange(t, f)
			f.exec = func(label string) (driver.Result, error) {
				if label != write.write {
					return nil, errFenceDriver
				}
				return driver.RowsAffected(1), nil
			}
			e := fenceEngineOn(f.open(t), &fenceIDs{})

			got, err := write.call(context.Background(), e)

			require.NoError(t, err)
			require.NotNil(t, got)
			if ended := f.awaitTxEnd(t); ended != "COMMIT" {
				t.Fatalf("期望提交,实际以 %s 结束", ended)
			}
			require.Equal(t, write.committed, f.recorded())
		})
	}
}

// 迟到提交的幂等:COMMIT 已发出、回包丢了(结局未知),客户端用同一个 ClientRequestId 重发时,持久的 request_key / request_digest
// 意图让重放只读出那条合同原样返回,不再开事务、不再领号;同号不同载荷按 ErrReplay 拒绝。
func TestCommerceCreateReplayOfCommittedRequestWritesNothing(t *testing.T) {
	in := fenceConsignmentRequest()
	for _, tc := range []struct {
		name    string
		digest  func(ctx context.Context) []byte
		wantErr error
	}{
		{"同号同载荷原样返回", func(ctx context.Context) []byte { return digest(ctx, in) }, nil},
		{"同号不同载荷拒绝", func(context.Context) []byte { return []byte("another payload") }, ErrReplay},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			committed := &tradepb.TradeCommerceRecord{CommerceId: fenceNewID, RequestKey: RequestKey("player", fenceSeller, fenceRequestID),
				RequestDigest: tc.digest(ctx), Kind: 3, Status: Escrowing, SellerPlayerId: fenceSeller, Version: 1}
			f := newFenceDB()
			f.rows["byRequest"] = fenceCommerceRow(committed)
			ids := &fenceIDs{}
			e := fenceEngineOn(f.open(t), ids)

			got, err := e.CreateConsignment(ctx, fenceSeller, in)

			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				require.Nil(t, got)
			} else {
				require.NoError(t, err)
				require.Equal(t, fenceNewID, got.GetCommerceId())
			}
			require.Equal(t, []string{"byRequest"}, f.recorded(), "重放只读,不开事务")
			require.Zero(t, ids.calls.Load(), "重放不再领号")
		})
	}
}
