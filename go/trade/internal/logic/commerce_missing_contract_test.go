package logic

// 商城合同(commerce)客户端入口点名的合同不存在时的回码(2026-11-11 缺陷修复的回归测试),不连库。
//
// 缺陷:Engine.Mutate(确认 / 购买 / 撤单)、Engine.PlaceBid、RmbConsignments.Cancel 的预读查不到合同时把 sql.ErrNoRows
// 原样上抛,commerceCode 把认不出的错误一律定性为 kServiceUnavailable(Tip.xlsx fault=1):客户端撤销 / 出价一张不存在的单
// 收到故障码,RootAdmission 还会把这次请求当成结局未知、把排空证明打成 Hold。Engine.Get 早就把查不到映射成 ErrMissing。
//
// 这里从客户端入口一路打到存储:录制驱动对按主键读合同回空结果集(或回一个存储错误),断言回给客户端的 tip、tip 的定性,
// 以及查不到时只读了一次合同、没有开事务(没锁角色、没写库)。GetCommerceOrder / GetAuctionLot 修复前就是对的,一起列着当对照。

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"

	base "proto/common/base"
	dspb "proto/data_service"
	tradepb "proto/trade"
	"shared/generated/pb/table"
	"shared/serverbase"
	"trade/internal/commerce"
	"trade/internal/config"
	"trade/internal/constants"
	"trade/internal/data"
	"trade/internal/session"

	"github.com/stretchr/testify/require"
)

const (
	missingContractID      = uint64(7001)
	missingContractText    = "7001"
	missingContractRequest = "missing-contract-0001"
)

var errMissingContractDriver = errors.New("合同缺失录制驱动:这条语句不该到达驱动")

// missingContractStore 是录制驱动的共享状态:记下到达驱动的语句标签。按主键读合同回空结果集;getErr 非 nil 时改回这个错误。
// getErr 在打开连接之前写好,之后只读。
type missingContractStore struct {
	mu     sync.Mutex
	stmts  []string
	getErr error
}

func (s *missingContractStore) record(label string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stmts = append(s.stmts, label)
}

func (s *missingContractStore) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.stmts...)
}

func (s *missingContractStore) open(t *testing.T) *sql.DB {
	t.Helper()
	// 用 sql.OpenDB + 自定义 Connector,不用 sql.Register:注册是全局的,名字撞了就 panic。
	db := sql.OpenDB(missingContractConnector{s: s})
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// missingContractLabel 把语句归成短标签;认不出的原样记下,便于看出多发了什么。
func missingContractLabel(query string) string {
	q := strings.Join(strings.Fields(query), " ")
	switch {
	case strings.Contains(q, "FROM trade_commerce WHERE commerce_id=") && strings.HasSuffix(q, "FOR UPDATE"):
		return "lock"
	case strings.Contains(q, "FROM trade_commerce WHERE commerce_id="):
		return "get"
	case strings.Contains(q, "FROM trade_commerce WHERE request_key="):
		return "byRequest"
	}
	return q
}

type missingContractConnector struct{ s *missingContractStore }

func (c missingContractConnector) Connect(context.Context) (driver.Conn, error) {
	return missingContractConn{s: c.s}, nil
}
func (c missingContractConnector) Driver() driver.Driver { return missingContractDriver{s: c.s} }

type missingContractDriver struct{ s *missingContractStore }

func (d missingContractDriver) Open(string) (driver.Conn, error) {
	return missingContractConn{s: d.s}, nil
}

type missingContractConn struct{ s *missingContractStore }

func (c missingContractConn) Prepare(query string) (driver.Stmt, error) {
	c.s.record(missingContractLabel(query))
	return nil, errMissingContractDriver
}
func (c missingContractConn) Close() error { return nil }
func (c missingContractConn) Begin() (driver.Tx, error) {
	c.s.record("BEGIN")
	return nil, errMissingContractDriver
}
func (c missingContractConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.s.record("BEGIN")
	return nil, errMissingContractDriver
}
func (c missingContractConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.s.record(missingContractLabel(query))
	return nil, errMissingContractDriver
}
func (c missingContractConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	label := missingContractLabel(query)
	c.s.record(label)
	if label != "get" {
		return nil, errMissingContractDriver
	}
	if c.s.getErr != nil {
		return nil, c.s.getErr
	}
	return &missingContractRows{columns: strings.Split(data.CommerceColumns, ",")}, nil
}

// missingContractRows 是查不到行的结果集:QueryRow.Scan 据此回 sql.ErrNoRows,与真库查不到同一条路径。
type missingContractRows struct{ columns []string }

func (r *missingContractRows) Columns() []string         { return r.columns }
func (r *missingContractRows) Close() error              { return nil }
func (r *missingContractRows) Next([]driver.Value) error { return io.EOF }

// missingContractIdentity 把会话里的角色当成已核过的原归属:这里测的是合同读路径,不是身份核对。
type missingContractIdentity struct{}

func (missingContractIdentity) Resolve(ctx context.Context, target uint64) (*dspb.AccountRoleOwnership, error) {
	if target == 0 {
		target, _ = session.ClientPlayerID(ctx)
	}
	return &dspb.AccountRoleOwnership{Account: "account-" + strconv.FormatUint(target, 10), OwnerRevision: 1, Role: &base.AccountSimplePlayer{PlayerId: target}}, nil
}

func newMissingContractLogic(t *testing.T, store *missingContractStore) *JubaozhaiLogic {
	t.Helper()
	f := newFixture(t, config.ScopeZone)
	engine := &commerce.Engine{
		Repo: &data.CommerceRepo{DB: store.open(t), Ops: &data.AssetOpRepo{}},
		Now:  f.deps.Now,
	}
	f.deps.Commerce = engine
	f.deps.Rmb = &commerce.RmbConsignments{Engine: engine}
	f.deps.Mall = &commerce.Mall{Engine: engine} // 只用来过「人民币寄售依赖已装配」的检查,撤单不调商城
	f.deps.RmbIdentity = missingContractIdentity{}
	return f.as(playerA)
}

// missingContractEntries 是按客户端给的单号读合同的全部客户端入口(商城订单号带「-」的走外部商城,不在此列)。
var missingContractEntries = []struct {
	name string
	call func(l *JubaozhaiLogic) (*base.TipInfoMessage, error)
}{
	{"ConfirmPlayerTrade", func(l *JubaozhaiLogic) (*base.TipInfoMessage, error) {
		out, err := l.ConfirmPlayerTrade(&tradepb.ConfirmPlayerTradeRequest{OrderId: missingContractText})
		return out.GetErrorMessage(), err
	}},
	{"BuyConsignment", func(l *JubaozhaiLogic) (*base.TipInfoMessage, error) {
		out, err := l.BuyConsignment(&tradepb.BuyConsignmentRequest{ListingId: missingContractText, ClientRequestId: missingContractRequest})
		return out.GetErrorMessage(), err
	}},
	{"CancelConsignment", func(l *JubaozhaiLogic) (*base.TipInfoMessage, error) {
		out, err := l.CancelConsignment(&tradepb.CancelConsignmentRequest{ListingId: missingContractText})
		return out.GetErrorMessage(), err
	}},
	{"CancelCommerceOrder", func(l *JubaozhaiLogic) (*base.TipInfoMessage, error) {
		out, err := l.CancelCommerceOrder(&tradepb.CancelCommerceOrderRequest{OrderId: missingContractText})
		return out.GetErrorMessage(), err
	}},
	{"CancelMarketOrder", func(l *JubaozhaiLogic) (*base.TipInfoMessage, error) {
		out, err := l.CancelMarketOrder(&tradepb.CancelMarketOrderRequest{MarketOrderId: missingContractText})
		return out.GetErrorMessage(), err
	}},
	{"CancelAuctionLot", func(l *JubaozhaiLogic) (*base.TipInfoMessage, error) {
		out, err := l.CancelAuctionLot(&tradepb.CancelAuctionLotRequest{LotId: missingContractText})
		return out.GetErrorMessage(), err
	}},
	{"PlaceAuctionBid", func(l *JubaozhaiLogic) (*base.TipInfoMessage, error) {
		out, err := l.PlaceAuctionBid(&tradepb.PlaceAuctionBidRequest{LotId: missingContractText, Amount: 6, ClientRequestId: missingContractRequest})
		return out.GetErrorMessage(), err
	}},
	{"CancelRmbConsignment", func(l *JubaozhaiLogic) (*base.TipInfoMessage, error) {
		out, err := l.CancelRmbConsignment(&tradepb.CancelRmbConsignmentRequest{ListingId: missingContractID})
		return out.GetErrorMessage(), err
	}},
	// 对照:Engine.Get 修复前就映射了查不到。
	{"GetCommerceOrder", func(l *JubaozhaiLogic) (*base.TipInfoMessage, error) {
		out, err := l.GetCommerceOrder(&tradepb.GetCommerceOrderRequest{OrderId: missingContractText})
		return out.GetErrorMessage(), err
	}},
	{"GetAuctionLot", func(l *JubaozhaiLogic) (*base.TipInfoMessage, error) {
		out, err := l.GetAuctionLot(&tradepb.GetAuctionLotRequest{LotId: missingContractText})
		return out.GetErrorMessage(), err
	}},
}

// 点名的合同不存在:回「订单不存在」业务码,不是故障码;只读一次合同就拒绝,不查原请求、不开事务、不锁角色。
func TestCommerceEntriesAnswerMissingContractWithOrderNotFound(t *testing.T) {
	for _, tc := range missingContractEntries {
		t.Run(tc.name, func(t *testing.T) {
			store := &missingContractStore{}
			tip, err := tc.call(newMissingContractLogic(t, store))
			require.NoError(t, err, "业务失败必须 in-band")
			require.Equal(t, uint32(table.TradeError_kTradeOrderNotFound), tip.GetId(), "查不到合同是业务拒绝,不是服务端故障")
			require.Equal(t, serverbase.VerdictBizReject, serverbase.TipVerdict(tip.GetId()), "故障码会让 RootAdmission 把排空证明打成 Hold")
			require.Equal(t, []string{"get"}, store.recorded())
		})
	}
}

// 预读本身失败(连接断开、超时)仍是故障:只有「查不到」改判为业务拒绝,真故障不能被吞成「订单不存在」。
func TestCommerceEntriesKeepContractReadFaultsAsFaults(t *testing.T) {
	for _, tc := range missingContractEntries {
		t.Run(tc.name, func(t *testing.T) {
			store := &missingContractStore{getErr: errors.New("connection reset by peer")}
			tip, err := tc.call(newMissingContractLogic(t, store))
			require.NoError(t, err, "业务失败必须 in-band")
			require.Equal(t, constants.ErrServiceUnavailable, tip.GetId())
			require.Equal(t, serverbase.VerdictFault, serverbase.TipVerdict(tip.GetId()))
			require.Equal(t, []string{"get"}, store.recorded())
		})
	}
}
