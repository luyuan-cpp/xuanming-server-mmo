package commerce

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tradepb "proto/trade"
	"shared/currencytype"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

type stubRefPrices struct {
	prices map[uint32]ReferencePrice
}

func (s stubRefPrices) ReferencePrice(_ context.Context, itemConfigID, currencyType uint32, _ uint64) (ReferencePrice, error) {
	if p, ok := s.prices[itemConfigID]; ok && p.CurrencyType == currencyType {
		return p, nil
	}
	return ReferencePrice{ItemConfigID: itemConfigID, CurrencyType: currencyType, InsufficientSample: true}, nil
}

type stubFriends struct {
	isFriend   bool
	durationMs uint64
}

func (s stubFriends) FriendshipDurationMs(_ context.Context, _, _, _ uint64) (bool, uint64, error) {
	return s.isFriend, s.durationMs, nil
}

type stubDailyOut struct {
	out map[uint64]uint64
}

func (s stubDailyOut) DailyCurrencyOutflow(_ context.Context, playerID uint64, _ uint32, _ uint64) (uint64, error) {
	return s.out[playerID], nil
}

func TestComputeReferencePriceFiltersWashTradesAndSmallSamples(t *testing.T) {
	const now uint64 = 10 * 24 * 3600 * 1000
	samples := make([]MarketFillSample, 0, 15)
	// 8 笔正常样本 + 5 笔同账号对敲天价样本（应被剔除，导致有效样本 8 < 10 -> 样本不足）。
	for i := 0; i < 8; i++ {
		samples = append(samples, MarketFillSample{
			ItemConfigID:  3001,
			CurrencyType:  currencytype.Gold,
			UnitPrice:     100,
			Quantity:      1,
			SellerAccount: "seller-real",
			BuyerAccount:  "buyer-real",
			SettledAtMs:   now - uint64(i+1)*3600*1000,
		})
	}
	for i := 0; i < 5; i++ {
		samples = append(samples, MarketFillSample{
			ItemConfigID:  3001,
			CurrencyType:  currencytype.Gold,
			UnitPrice:     999_999,
			Quantity:      1,
			SellerAccount: "wash-account",
			BuyerAccount:  "wash-account",
			SettledAtMs:   now - 1000,
		})
	}
	insufficient := ComputeReferencePrice(samples, 3001, currencytype.Gold, now, DefaultReferenceWindowMs, 10)
	require.True(t, insufficient.InsufficientSample)
	require.Equal(t, 8, insufficient.SampleCount)
	require.Zero(t, insufficient.MedianUnitPrice)

	// 再补 2 笔真实成交（120、140）以及 1 笔 8 天前过期成交（应忽略）。
	samples = append(samples,
		MarketFillSample{ItemConfigID: 3001, CurrencyType: currencytype.Gold, UnitPrice: 120, Quantity: 1, SellerAccount: "s1", BuyerAccount: "b1", SettledAtMs: now - 500},
		MarketFillSample{ItemConfigID: 3001, CurrencyType: currencytype.Gold, UnitPrice: 140, Quantity: 1, SellerAccount: "s2", BuyerAccount: "b2", SettledAtMs: now - 400},
		MarketFillSample{ItemConfigID: 3001, CurrencyType: currencytype.Gold, UnitPrice: 5000, Quantity: 1, SellerAccount: "s3", BuyerAccount: "b3", SettledAtMs: now - 8*24*3600*1000},
	)
	valid := ComputeReferencePrice(samples, 3001, currencytype.Gold, now, DefaultReferenceWindowMs, 10)
	require.False(t, valid.InsufficientSample)
	require.Equal(t, 10, valid.SampleCount)
	require.EqualValues(t, 100, valid.MedianUnitPrice)
}

func TestPlayerTradeGuardStrictConsumablesModeA(t *testing.T) {
	ctx := context.Background()
	const now uint64 = 1_760_000_000_000
	guard := &PlayerTradeGuard{
		Policy: PlayerTradePolicy{
			Mode:                   PlayerTradeModeStrictConsumables,
			ConsumableItems:        []uint32{501, 502},
			MaxDailyCurrencyAmount: 10_000,
		},
		DailyOut: stubDailyOut{out: map[uint64]uint64{101: 7_000}},
	}

	// 允许：低价耗材 + 当日剩余额度内的银两
	res, err := guard.Evaluate(ctx, 101, 202,
		&tradepb.TradeAssetSelection{Items: map[uint32]uint32{501: 5}, CurrencyType: currencytype.Gold, CurrencyAmount: 3_000},
		&tradepb.TradeAssetSelection{Items: map[uint32]uint32{502: 2}},
		now)
	require.NoError(t, err)
	require.False(t, res.RequireCoolingLock)

	// 拒绝：超过单日累计银两上限（已用 7000 + 本次 3001 > 10000）
	_, err = guard.Evaluate(ctx, 101, 202,
		&tradepb.TradeAssetSelection{CurrencyType: currencytype.Gold, CurrencyAmount: 3_001},
		&tradepb.TradeAssetSelection{Items: map[uint32]uint32{502: 1}},
		now)
	require.ErrorIs(t, err, ErrNotTradable)

	// 拒绝：唯一装备实例、宠物与非耗材核心材料（如打造料 3001）一律禁止走免费当面
	_, err = guard.Evaluate(ctx, 101, 202,
		&tradepb.TradeAssetSelection{ItemUuids: []uint64{90001}},
		&tradepb.TradeAssetSelection{CurrencyType: currencytype.Gold, CurrencyAmount: 1},
		now)
	require.ErrorIs(t, err, ErrNotTradable)

	_, err = guard.Evaluate(ctx, 101, 202,
		&tradepb.TradeAssetSelection{PetId: 777},
		&tradepb.TradeAssetSelection{CurrencyType: currencytype.Gold, CurrencyAmount: 1},
		now)
	require.ErrorIs(t, err, ErrNotTradable)

	_, err = guard.Evaluate(ctx, 101, 202,
		&tradepb.TradeAssetSelection{Items: map[uint32]uint32{3001: 20}},
		&tradepb.TradeAssetSelection{CurrencyType: currencytype.Gold, CurrencyAmount: 1},
		now)
	require.ErrorIs(t, err, ErrNotTradable)
}

func TestPlayerTradeGuardAcquaintanceModeBEnforcesFriendDaysAndReferenceDeviation(t *testing.T) {
	ctx := context.Background()
	const now uint64 = 1_760_000_000_000
	prices := stubRefPrices{prices: map[uint32]ReferencePrice{
		3001: {ItemConfigID: 3001, CurrencyType: currencytype.Gold, MedianUnitPrice: 1_000, SampleCount: 20},
	}}

	// 1) 陌生人 / 好友不足 7 天：禁止 1 银两换贵重核心材料或装备，防骗子现加现骗
	shortFriend := &PlayerTradeGuard{
		Policy: PlayerTradePolicy{
			Mode:                         PlayerTradeModeAcquaintanceLocked,
			MinFriendDurationMs:          DefaultMinFriendDurationMs,
			MaxReferenceDeviationPct:     50,
			CoolingLockDurationMs:        DefaultCoolingLockDurationMs,
			AllowAcquaintanceCoolingLock: true,
		},
		Friends: stubFriends{isFriend: true, durationMs: DefaultMinFriendDurationMs - 1},
		Prices:  prices,
	}
	_, err := shortFriend.Evaluate(ctx, 101, 202,
		&tradepb.TradeAssetSelection{Items: map[uint32]uint32{3001: 10}},
		&tradepb.TradeAssetSelection{CurrencyType: currencytype.Gold, CurrencyAmount: 1},
		now)
	require.ErrorIs(t, err, ErrNotTradable)

	// 2) 满 7 天熟人 + 合理公允对价（10 * 1000 = 10000，对手出 8000 银两，偏离 20% <= 50%）：直接放行不加冷静锁
	longFriend := &PlayerTradeGuard{
		Policy:  shortFriend.Policy,
		Friends: stubFriends{isFriend: true, durationMs: DefaultMinFriendDurationMs},
		Prices:  prices,
	}
	fair, err := longFriend.Evaluate(ctx, 101, 202,
		&tradepb.TradeAssetSelection{Items: map[uint32]uint32{3001: 10}},
		&tradepb.TradeAssetSelection{CurrencyType: currencytype.Gold, CurrencyAmount: 8_000},
		now)
	require.NoError(t, err)
	require.False(t, fair.RequireCoolingLock)

	// 3) 满 7 天熟人 + 1 银两换 10000 银两材料（严重偏离 > 50%）：放行但强制触发 48 小时冷静保护锁
	deviated, err := longFriend.Evaluate(ctx, 101, 202,
		&tradepb.TradeAssetSelection{Items: map[uint32]uint32{3001: 10}},
		&tradepb.TradeAssetSelection{CurrencyType: currencytype.Gold, CurrencyAmount: 1},
		now)
	require.NoError(t, err)
	require.True(t, deviated.RequireCoolingLock)
	require.Equal(t, DefaultCoolingLockDurationMs, deviated.CoolingLockDurationMs)

	// 4) 若未开启熟人冷静锁，则严重偏离直接拒绝（要求改走官方指定买家定向单）
	longFriend.Policy.AllowAcquaintanceCoolingLock = false
	_, err = longFriend.Evaluate(ctx, 101, 202,
		&tradepb.TradeAssetSelection{Items: map[uint32]uint32{3001: 10}},
		&tradepb.TradeAssetSelection{CurrencyType: currencytype.Gold, CurrencyAmount: 1},
		now)
	require.ErrorIs(t, err, ErrNotTradable)
}

func TestRmbIncomesValidatesDisputeFrozenFen(t *testing.T) {
	payload := map[string]any{
		"incomes": []map[string]any{
			{
				"orderId":          "00112233-4455-6677-8899-aabbccddeeff",
				"grossFen":         1000,
				"feeFen":           50,
				"netFen":           950,
				"reservedFen":      100,
				"paidFen":          200,
				"forfeitedFen":     50,
				"disputeFrozenFen": 600,
				"disputeCaseId":    "CASE-001",
				"availableMs":      123456,
			},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/internal/consignments/incomes", r.URL.Path)
		require.NoError(t, json.NewEncoder(w).Encode(payload))
	}))
	t.Cleanup(srv.Close)

	mall, err := NewMall(srv.URL, strings.Repeat("g", 32), strings.Repeat("d", 32), nil)
	require.NoError(t, err)

	resp, err := mall.RmbIncomes(context.Background(), "seller-acct", "", 20)
	require.NoError(t, err)
	require.Len(t, resp.Incomes, 1)
	require.EqualValues(t, 950, resp.Incomes[0].NetFen)

	// 超界争议冻结（100+200+50+601 > 950）必须被 fail-closed 拒收
	payload["incomes"].([]map[string]any)[0]["disputeFrozenFen"] = 601
	_, err = mall.RmbIncomes(context.Background(), "seller-acct", "", 20)
	require.Error(t, err)
}

type stubWorkflowQueryConnector struct {
	sellerRows [][]byte
	buyerRows  [][]byte
}

func (c *stubWorkflowQueryConnector) Connect(context.Context) (driver.Conn, error) {
	return &stubWorkflowQueryConn{c: c}, nil
}
func (c *stubWorkflowQueryConnector) Driver() driver.Driver { return nil }

type stubWorkflowQueryConn struct{ c *stubWorkflowQueryConnector }

func (c *stubWorkflowQueryConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *stubWorkflowQueryConn) Close() error                        { return nil }
func (c *stubWorkflowQueryConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }
func (c *stubWorkflowQueryConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, "seller_player_id=?") {
		return &stubByteRows{rows: c.c.sellerRows}, nil
	}
	return &stubByteRows{rows: c.c.buyerRows}, nil
}

type stubByteRows struct {
	rows [][]byte
	idx  int
}

func (r *stubByteRows) Columns() []string { return []string{"workflow_proto"} }
func (r *stubByteRows) Close() error      { return nil }
func (r *stubByteRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.rows) {
		return io.EOF
	}
	dest[0] = r.rows[r.idx]
	r.idx++
	return nil
}

func mustMarshalWorkflow(t *testing.T, w *tradepb.TradeCommerceWorkflow) []byte {
	t.Helper()
	raw, err := proto.Marshal(w)
	require.NoError(t, err)
	return raw
}

func TestSQLDailyPlayerTradeTrackerSumsBothSidesAndFailsClosed(t *testing.T) {
	ctx := context.Background()
	const now uint64 = 1_760_000_000_000

	conn := &stubWorkflowQueryConnector{
		sellerRows: [][]byte{
			mustMarshalWorkflow(t, &tradepb.TradeCommerceWorkflow{
				SellerAssets: &tradepb.TradeAssetSelection{CurrencyType: currencytype.Gold, CurrencyAmount: 3_500},
				BuyerAssets:  &tradepb.TradeAssetSelection{CurrencyType: currencytype.Gold, CurrencyAmount: 9_999}, // 对方流出不计入卖家
			}),
			mustMarshalWorkflow(t, &tradepb.TradeCommerceWorkflow{
				SellerAssets: &tradepb.TradeAssetSelection{CurrencyType: currencytype.Diamond, CurrencyAmount: 500}, // 其他币种不计入银两
			}),
		},
		buyerRows: [][]byte{
			mustMarshalWorkflow(t, &tradepb.TradeCommerceWorkflow{
				SellerAssets: &tradepb.TradeAssetSelection{CurrencyType: currencytype.Gold, CurrencyAmount: 8_888},
				BuyerAssets:  &tradepb.TradeAssetSelection{CurrencyType: currencytype.Gold, CurrencyAmount: 4_000},
			}),
		},
	}
	db := sql.OpenDB(conn)
	t.Cleanup(func() { _ = db.Close() })

	tracker := &SQLDailyPlayerTradeTracker{DB: db}
	used, err := tracker.DailyCurrencyOutflow(ctx, 101, currencytype.Gold, (now/dayMs)*dayMs)
	require.NoError(t, err)
	require.EqualValues(t, 7_500, used)

	// 接入 PlayerTradeGuard 后：剩余额度 2500，2500 放行、2501 拦截（防拆单绕单日限额）。
	guard := &PlayerTradeGuard{
		Policy: PlayerTradePolicy{
			Mode:                   PlayerTradeModeStrictConsumables,
			ConsumableItems:        []uint32{501},
			MaxDailyCurrencyAmount: 10_000,
		},
		DailyOut: tracker,
	}
	_, err = guard.Evaluate(ctx, 101, 202,
		&tradepb.TradeAssetSelection{CurrencyType: currencytype.Gold, CurrencyAmount: 2_500},
		&tradepb.TradeAssetSelection{Items: map[uint32]uint32{501: 1}},
		now)
	require.NoError(t, err)

	_, err = guard.Evaluate(ctx, 101, 202,
		&tradepb.TradeAssetSelection{CurrencyType: currencytype.Gold, CurrencyAmount: 2_501},
		&tradepb.TradeAssetSelection{Items: map[uint32]uint32{501: 1}},
		now)
	require.ErrorIs(t, err, ErrNotTradable)

	// 溢出饱和保护 + 坏字节 fail-closed
	require.EqualValues(t, uint64(math.MaxUint64), saturatingAddUint64(math.MaxUint64-10, 20))
	conn.sellerRows = [][]byte{[]byte("not-a-valid-protobuf-wire")}
	_, err = tracker.DailyCurrencyOutflow(ctx, 101, currencytype.Gold, (now/dayMs)*dayMs)
	require.Error(t, err)

	// 单侧触达扫描上限（1000 笔/日）直接饱和拒放，杜绝脚本海量微单绕限额
	oneGold := mustMarshalWorkflow(t, &tradepb.TradeCommerceWorkflow{
		SellerAssets: &tradepb.TradeAssetSelection{CurrencyType: currencytype.Gold, CurrencyAmount: 1},
	})
	conn.sellerRows = make([][]byte, maxDailyPlayerTradeScanRows)
	for i := range conn.sellerRows {
		conn.sellerRows[i] = oneGold
	}
	conn.buyerRows = nil
	saturated, err := tracker.DailyCurrencyOutflow(ctx, 101, currencytype.Gold, (now/dayMs)*dayMs)
	require.NoError(t, err)
	require.EqualValues(t, uint64(math.MaxUint64), saturated)
}
