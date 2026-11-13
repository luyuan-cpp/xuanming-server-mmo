package logic

// ListMarketOrders 只回属于调用者原身份的挂单,与 ListCommerceOrders、Engine.Get 用同一条规则(commerce.OwnsRecord),不连库。
//
// 角色过户之后,按 seller_player_id 仍能查到旧主留下的历史挂单;它们的合同身份是旧主的,不能展示给新主
// (docs/design/gate-online-identity.md §9.3)。旧主自己的连接在入口归属预检就被拒,走不到这里。

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"

	base "proto/common/base"
	dspb "proto/data_service"
	tradepb "proto/trade"
	"shared/roleidentity"
	"trade/internal/commerce"
	"trade/internal/config"
	"trade/internal/data"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

var errMarketOrderDriver = errors.New("盘口挂单录制驱动:这条语句不该到达驱动")

// marketOrderConnector 只应答「按卖家列盘口挂单」这一条查询,回预置的行;其余语句一律报错。
type marketOrderConnector struct{ rows [][]driver.Value }

func (c marketOrderConnector) Connect(context.Context) (driver.Conn, error) {
	return marketOrderConn{rows: c.rows}, nil
}
func (c marketOrderConnector) Driver() driver.Driver { return nil }

type marketOrderConn struct{ rows [][]driver.Value }

func (c marketOrderConn) Prepare(string) (driver.Stmt, error) { return nil, errMarketOrderDriver }
func (c marketOrderConn) Close() error                        { return nil }
func (c marketOrderConn) Begin() (driver.Tx, error)           { return nil, errMarketOrderDriver }
func (c marketOrderConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if !strings.Contains(query, "kind=4 AND side<>0 AND seller_player_id=?") {
		return nil, errMarketOrderDriver
	}
	return &marketOrderRows{columns: strings.Split(data.CommerceColumns, ","), rows: c.rows}, nil
}

type marketOrderRows struct {
	columns []string
	rows    [][]driver.Value
	next    int
}

func (r *marketOrderRows) Columns() []string { return r.columns }
func (r *marketOrderRows) Close() error      { return nil }
func (r *marketOrderRows) Next(dest []driver.Value) error {
	if r.next >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.next])
	r.next++
	return nil
}

// marketOrderRow 是一张盘口卖单在 trade_commerce 里的一行,列序同 data.CommerceColumns。owner 是下单时记进合同的原身份。
func marketOrderRow(t *testing.T, id, seller, createdMs uint64, owner *tradepb.TradeRoleIdentity) []driver.Value {
	t.Helper()
	workflow, err := proto.Marshal(&tradepb.TradeCommerceWorkflow{Stage: "market_open", SellerIdentity: owner})
	require.NoError(t, err)
	row := []driver.Value{
		int64(id), "market-" + strconv.FormatUint(id, 10), []byte{}, int64(4), int64(3), int64(seller), int64(0),
		int64(1), int64(10), int64(tradepb.MarketOrderSide_MARKET_ORDER_SIDE_SELL), int64(50), int64(2), int64(0), int64(1),
		int64(0), int64(createdMs), int64(createdMs), int64(1), workflow, int64(0), int64(0), false,
	}
	require.Len(t, row, len(strings.Split(data.CommerceColumns, ",")), "列数要跟 data.CommerceColumns 对上")
	return row
}

// marketOrderActor 是过了入口准入的客户端请求:会话里有角色,ctx 里有入口准入捕获的原身份。
func marketOrderActor(t *testing.T, f *fixture, player uint64, account string, revision uint64) *JubaozhaiLogic {
	t.Helper()
	ctx, err := roleidentity.Capture(playerCtx(player), &dspb.AccountRoleOwnership{
		Account: account, OwnerRevision: revision, Role: &base.AccountSimplePlayer{PlayerId: player},
	})
	require.NoError(t, err)
	return NewJubaozhaiLogic(ctx, f.deps)
}

func TestListMarketOrdersShowsOnlyOrdersOfTheCallersOriginalIdentity(t *testing.T) {
	f := newFixture(t, config.ScopeZone)
	seller := &tradepb.TradeRoleIdentity{Account: "seller", OwnerRevision: 1}
	buyer := &tradepb.TradeRoleIdentity{Account: "buyer", OwnerRevision: 2}
	db := sql.OpenDB(marketOrderConnector{rows: [][]driver.Value{
		marketOrderRow(t, 7001, playerA, 100, seller), // 过户之前旧主挂的单
		marketOrderRow(t, 7002, playerA, 200, buyer),  // 过户之后新主挂的单
		marketOrderRow(t, 7003, playerA, 300, nil),    // 缺原身份的坏合同:谁也不展示
	}})
	t.Cleanup(func() { _ = db.Close() })
	f.deps.Commerce = &commerce.Engine{Repo: &data.CommerceRepo{DB: db, Ops: &data.AssetOpRepo{}}, Now: f.deps.Now}

	listed := func(l *JubaozhaiLogic) ([]string, string) {
		t.Helper()
		out, err := l.ListMarketOrders(&tradepb.ListMarketOrdersRequest{Limit: 10})
		require.NoError(t, err)
		require.Zero(t, out.GetErrorMessage().GetId())
		ids := make([]string, 0, len(out.Orders))
		for _, order := range out.Orders {
			ids = append(ids, order.MarketOrderId)
		}
		return ids, out.NextCursor
	}

	ids, cursor := listed(marketOrderActor(t, f, playerA, "buyer", 2))
	require.Equal(t, []string{"7002"}, ids, "新主只看到自己挂的单,看不到旧主留下的历史挂单")
	require.Equal(t, "300:7003", cursor, "游标照样越过被筛掉的行,翻页不会卡住")

	// 旧主的连接在入口归属预检就被拒;这里只钉住筛选规则本身是按合同身份,不是按角色。
	ids, _ = listed(marketOrderActor(t, f, playerA, "seller", 1))
	require.Equal(t, []string{"7001"}, ids)

	// ctx 里没有原身份(入口准入没装在前面才会出现):一张都不展示。
	ids, _ = listed(f.as(playerA))
	require.Empty(t, ids)
}
