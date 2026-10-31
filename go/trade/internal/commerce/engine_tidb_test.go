package commerce

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"net/http"
	"net/http/httptest"
	"os"
	assetpb "proto/common/asset"
	tradepb "proto/trade"
	"schemamigrate"
	"shared/assetop"
	"shared/authoritysql/sqltest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"trade/internal/data"
)

type testIDs struct{ value atomic.Uint64 }

func (i *testIDs) Next(context.Context) (uint64, error) { return i.value.Add(1), nil }

// 每例仅清理自身随机库；不复用、更不删除本地游戏库。
func tiDBEngine(t *testing.T) *Engine {
	t.Helper()
	dsn := os.Getenv("COMMERCE_TEST_DSN")
	if dsn == "" {
		if os.Getenv("COMMERCE_REQUIRE_TIDB") == "1" {
			t.Fatal("缺少 COMMERCE_TEST_DSN")
		}
		t.Skip("未配置隔离TiDB")
	}
	return openEngineDB(t, dsn, true)
}

// openEngineDB 在 dsn 指向的实例上新建随机库、按 proto 建表并装配 Engine;用例结束删库。
// requireTiDB=false 给一次性 MySQL 用(tick_isolation_db_test.go 的 realDBEngine):会话仍固定 RC,只是不设 TiDB 专用变量。
func openEngineDB(t *testing.T, dsn string, requireTiDB bool) *Engine {
	t.Helper()
	if !requireTiDB {
		t.Fatal("全球writer真实fixture必须使用已核TiDB资格，不能将MySQL冒充TiDB")
	}
	raw := make([]byte, 10)
	_, err := rand.Read(raw)
	require.NoError(t, err)
	name := "commerce_it_" + hex.EncodeToString(raw)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	opts, err := sqltest.OptionsFromEnv(dsn, name+"_writer", name)
	require.NoError(t, err)
	fixture, err := sqltest.New(ctx, t, opts)
	require.NoError(t, err)
	migrationDB, err := fixture.OpenSchema(ctx, name)
	require.NoError(t, err)
	report, err := schemamigrate.Up(ctx, migrationDB, schemamigrate.Options{Database: name, Tables: data.Tables()})
	require.NoError(t, err)
	require.Empty(t, report.Manual)
	require.NoError(t, fixture.GrantProtoTables(ctx, name, data.Tables()))
	db, err := fixture.Open(ctx, name, 12)
	require.NoError(t, err)
	ops, err := data.NewAssetOpRepo(db, 3*time.Second)
	require.NoError(t, err)
	return &Engine{Repo: &data.CommerceRepo{DB: db, Ops: ops}, IDs: &testIDs{}, OpIDs: &testIDs{}, Policy: Policy{Items: []uint32{10}, ItemTitles: map[uint32]string{10: "玄铁"}, Currencies: []uint32{1}}, Now: time.Now, Roles: fixtureRoles{}}
}
func tick(t *testing.T, e *Engine) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	require.NoError(t, e.Tick(ctx))
}

// 此夹具只为交易库状态机提供已提交的资产终局；不冒充scene真实资产端到端验收。
func settleLegs(t *testing.T, e *Engine, outcome uint32) {
	t.Helper()
	_, err := e.Repo.DB.Exec("UPDATE trade_asset_op SET status=?,durable=1 WHERE status=1", outcome)
	require.NoError(t, err)
}
func state(t *testing.T, e *Engine, id uint64) (*tradepb.TradeCommerceRecord, *tradepb.TradeCommerceWorkflow) {
	t.Helper()
	r, err := e.Repo.Get(context.Background(), id)
	require.NoError(t, err)
	w, err := data.Workflow(r)
	require.NoError(t, err)
	return r, w
}

func TestTiDBConsignmentConcurrentBuyAndWorkerRestart(t *testing.T) {
	e := tiDBEngine(t)
	ctx := context.Background()
	offer, err := e.CreateConsignment(ctx, 11, &tradepb.CreateConsignmentRequest{ClientRequestId: "offer_0001", Assets: &tradepb.TradeAssetSelection{Items: map[uint32]uint32{10: 3}}, CurrencyType: 1, Price: 50, DurationMs: 60000})
	require.NoError(t, err)
	tick(t, e)
	r, _ := state(t, e, offer.CommerceId)
	require.Equal(t, Escrowing, r.Status, "未提交扣出不得上架")
	settleLegs(t, e, 2)
	tick(t, e)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for p := uint64(21); p < 29; p++ {
		wg.Add(1)
		go func(p uint64) {
			defer wg.Done()
			_, err := e.Mutate(ctx, p, offer.CommerceId, "buy", fmt.Sprintf("buy-request-%d", p))
			if err == nil {
				wins.Add(1)
			} else {
				require.ErrorIs(t, err, ErrConflict)
			}
		}(p)
	}
	wg.Wait()
	require.EqualValues(t, 1, wins.Load())
	// 新进程只保留同一SQL库，读取原outbox/seq继续，不创建第二笔扣款。
	restarted := *e
	tick(t, &restarted)
	settleLegs(t, &restarted, 2)
	tick(t, &restarted)
	r, _ = state(t, e, offer.CommerceId)
	require.Equal(t, Delivering, r.Status)
	var credits int
	require.NoError(t, e.Repo.DB.QueryRow("SELECT COUNT(*) FROM trade_asset_op WHERE stream=4").Scan(&credits))
	require.Equal(t, 2, credits)
	settleLegs(t, &restarted, 2)
	tick(t, &restarted)
	r, _ = state(t, e, offer.CommerceId)
	require.Equal(t, Settled, r.Status)
	tick(t, &restarted)
	require.NoError(t, e.Repo.DB.QueryRow("SELECT COUNT(*) FROM trade_asset_op WHERE stream=4").Scan(&credits))
	require.Equal(t, 2, credits)
}

func TestTiDBPlayerTradeRequiresBothEscrowSummariesAndReturnsOnCancel(t *testing.T) {
	e := tiDBEngine(t)
	ctx := context.Background()
	offer, err := e.CreatePlayerTrade(ctx, 11, &tradepb.CreatePlayerTradeRequest{ClientRequestId: "trade_0001", BuyerPlayerId: 22, SellerAssets: &tradepb.TradeAssetSelection{Items: map[uint32]uint32{10: 2}}, BuyerAssets: &tradepb.TradeAssetSelection{CurrencyType: 1, CurrencyAmount: 70}})
	require.NoError(t, err)
	tick(t, e)
	_, err = e.Mutate(ctx, 11, offer.CommerceId, "confirm")
	require.ErrorIs(t, err, ErrConflict)
	settleLegs(t, e, 2)
	tick(t, e)
	_, err = e.Mutate(ctx, 11, offer.CommerceId, "confirm")
	require.ErrorIs(t, err, ErrConflict)
	_, err = e.Mutate(ctx, 22, offer.CommerceId, "confirm")
	require.NoError(t, err)
	tick(t, e)
	settleLegs(t, e, 2)
	tick(t, e)
	_, err = e.Mutate(ctx, 11, offer.CommerceId, "cancel")
	require.NoError(t, err)
	tick(t, e)
	r, w := state(t, e, offer.CommerceId)
	require.Equal(t, CancelPending, r.Status)
	require.NotNil(t, leg(w, "seller_return"))
	require.NotNil(t, leg(w, "buyer_return"))
	require.Nil(t, leg(w, "seller_receive"))
	settleLegs(t, e, 2)
	tick(t, e)
	r, _ = state(t, e, offer.CommerceId)
	require.Equal(t, Cancelled, r.Status)
}

// 材料订单的托管腿没有截止(ExpiresMs=0)。托管扣款可能一直回 RETRY 而不记终态(按数量扣出遇到带更新版本字段的
// 物品,混版本设计 §4.4 第 6 批),所以卖家在托管阶段必须能撤:撤单把在途托管扣款截止到当下,资产通道据此改发 Abort。
// 没扣到 → 按取消收口、无可退;Abort 输给已落地的扣款 → 照常进盘口后立刻按撤单整单退回。
func TestTiDBSellerCancelsMarketOrderStillInEscrow(t *testing.T) {
	e := tiDBEngine(t)
	ctx := context.Background()
	place := func(requestID string) (uint64, uint64) {
		r, err := e.PlaceMarket(ctx, 11, &tradepb.PlaceMarketOrderRequest{ClientRequestId: requestID, MarketId: 1, ItemConfigId: 10, Side: 2, UnitPrice: 10, Quantity: 2, CurrencyType: 1})
		require.NoError(t, err)
		require.NoError(t, e.advance(ctx, r))
		var opID, deadline uint64
		require.NoError(t, e.Repo.DB.QueryRow("SELECT op_id,deadline_ms FROM trade_asset_op WHERE ref_id=? AND stream=3", r.CommerceId).Scan(&opID, &deadline))
		require.Zero(t, deadline, "材料订单的托管扣款没有截止")
		_, w := state(t, e, r.CommerceId)
		require.Equal(t, "escrow", w.Stage)
		_, err = e.Mutate(ctx, 11, r.CommerceId, "cancel")
		require.NoError(t, err, "托管阶段卖家可以撤单")
		require.NoError(t, e.Repo.DB.QueryRow("SELECT deadline_ms FROM trade_asset_op WHERE op_id=?", opID).Scan(&deadline))
		require.NotZero(t, deadline, "撤单把在途托管扣款截止到当下")
		return r.CommerceId, opID
	}

	aborted, _ := place("market-escrow-cancel-aborted")
	settleLegs(t, e, uint32(tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_ABORTED))
	r, _ := state(t, e, aborted)
	require.NoError(t, e.advance(ctx, r))
	r, w := state(t, e, aborted)
	require.Equal(t, Cancelled, r.Status, "没扣到:按取消收口")
	require.Nil(t, leg(w, "seller_return"), "没扣到就没有可退的")

	applied, _ := place("market-escrow-cancel-applied")
	settleLegs(t, e, uint32(tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED))
	r, _ = state(t, e, applied)
	require.NoError(t, e.advance(ctx, r))
	r, w = state(t, e, applied)
	require.Equal(t, CancelPending, r.Status)
	require.Equal(t, "return", w.Stage, "已扣到:不进盘口撮合,直接按撤单退回")
	require.NotNil(t, leg(w, "seller_return"))
	require.EqualValues(t, 2, leg(w, "seller_return").Bundle.Items[0].Count, "整单未成交,全数退回")
	settleLegs(t, e, uint32(tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED))
	r, _ = state(t, e, applied)
	require.NoError(t, e.advance(ctx, r))
	r, _ = state(t, e, applied)
	require.Equal(t, Cancelled, r.Status)
}

func TestTiDBRejectedBuyRetryKeepsOriginalOutcomeAndSeq(t *testing.T) {
	e := tiDBEngine(t)
	ctx := context.Background()
	offer, err := e.CreateConsignment(ctx, 11, &tradepb.CreateConsignmentRequest{ClientRequestId: "retry-offer-001", Assets: &tradepb.TradeAssetSelection{Items: map[uint32]uint32{10: 1}}, CurrencyType: 1, Price: 50, DurationMs: 60000})
	require.NoError(t, err)
	tick(t, e)
	settleLegs(t, e, 2)
	tick(t, e)
	_, err = e.Mutate(ctx, 22, offer.CommerceId, "buy", "buy-retry-001")
	require.NoError(t, err)
	tick(t, e)
	settleLegs(t, e, 3)
	tick(t, e)
	_, before := state(t, e, offer.CommerceId)
	require.Equal(t, "open", before.Stage)
	_, err = e.Mutate(ctx, 22, offer.CommerceId, "buy", "buy-retry-001")
	require.ErrorIs(t, err, ErrConflict)
	tick(t, e)
	_, after := state(t, e, offer.CommerceId)
	require.True(t, proto.Equal(before, after), "被拒绝的同请求不能重开扣款")
	var count int
	require.NoError(t, e.Repo.DB.QueryRow("SELECT COUNT(*) FROM trade_asset_op WHERE player_id=22 AND stream=3").Scan(&count))
	require.Equal(t, 1, count)
	_, err = e.Mutate(ctx, 22, offer.CommerceId, "buy", "buy-retry-002")
	require.NoError(t, err, "显式新请求才允许重新购买")
	tick(t, e)
	require.NoError(t, e.Repo.DB.QueryRow("SELECT COUNT(*) FROM trade_asset_op WHERE player_id=22 AND stream=3").Scan(&count))
	require.Equal(t, 2, count)
}

func TestTiDBOrderBookPriceTimePartialCancelWaitsForChildren(t *testing.T) {
	e := tiDBEngine(t)
	ctx := context.Background()
	open := func(player uint64, side tradepb.MarketOrderSide, price uint64, qty uint32) uint64 {
		r, err := e.PlaceMarket(ctx, player, &tradepb.PlaceMarketOrderRequest{ClientRequestId: fmt.Sprintf("market-open-%d", player), MarketId: 1, ItemConfigId: 10, Side: side, UnitPrice: price, Quantity: qty, CurrencyType: 1})
		require.NoError(t, err)
		require.NoError(t, e.advance(ctx, r))
		settleLegs(t, e, 2)
		r, _ = state(t, e, r.CommerceId)
		require.NoError(t, e.advance(ctx, r))
		return r.CommerceId
	}
	expensive := open(11, 2, 12, 2)
	first := open(12, 2, 10, 2)
	second := open(13, 2, 10, 2)
	buy := open(22, 1, 15, 3)
	require.NoError(t, e.matchMarket(ctx, buy))
	require.NoError(t, e.matchMarket(ctx, buy))
	rows, err := e.Repo.DB.Query("SELECT commerce_id,counterparty_id,unit_price,quantity FROM trade_commerce WHERE parent_id=? ORDER BY commerce_id", buy)
	require.NoError(t, err)
	var children []uint64
	for rows.Next() {
		var id, seller, price uint64
		var qty uint32
		require.NoError(t, rows.Scan(&id, &seller, &price, &qty))
		children = append(children, id)
		require.EqualValues(t, 10, price, "按最优卖价和maker价成交")
		if len(children) == 1 {
			require.Equal(t, first, seller)
			require.EqualValues(t, 2, qty)
		} else {
			require.Equal(t, second, seller)
			require.EqualValues(t, 1, qty)
		}
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Len(t, children, 2)
	r, _ := state(t, e, expensive)
	require.Zero(t, r.FilledQuantity, "价格更差的旧单不抢占最优价")
	for _, id := range children {
		r, _ := state(t, e, id)
		require.NoError(t, e.advance(ctx, r))
		_, w := state(t, e, id)
		require.Equal(t, uint64(r.Quantity)*5, leg(w, "buyer_surplus").Bundle.Currencies[0].Amount)
	}
	_, err = e.Mutate(ctx, 13, second, "cancel")
	require.NoError(t, err)
	r, _ = state(t, e, second)
	require.NoError(t, e.advance(ctx, r))
	_, w := state(t, e, second)
	require.EqualValues(t, 1, leg(w, "seller_return").Bundle.Items[0].Count, "仅退未成交数量")
	_, err = e.Repo.DB.Exec("UPDATE trade_asset_op SET status=2,durable=1 WHERE ref_id=? AND stream=4", second)
	require.NoError(t, err)
	r, _ = state(t, e, second)
	require.NoError(t, e.advance(ctx, r))
	r, w = state(t, e, second)
	require.Equal(t, CancelPending, r.Status, "子成交交付未结束不能声称撤单完全完成")
	require.Equal(t, "market_wait", w.Stage)
	settleLegs(t, e, 2)
	for _, id := range children {
		r, _ = state(t, e, id)
		require.NoError(t, e.advance(ctx, r))
	}
	r, _ = state(t, e, second)
	require.NoError(t, e.advance(ctx, r))
	r, _ = state(t, e, second)
	require.Equal(t, Cancelled, r.Status)
}

func TestTiDBPartialStopsBeforeCounterpartyOrRefund(t *testing.T) {
	e := tiDBEngine(t)
	offer, err := e.CreateConsignment(context.Background(), 11, &tradepb.CreateConsignmentRequest{ClientRequestId: "partial_001", Assets: &tradepb.TradeAssetSelection{Items: map[uint32]uint32{10: 3}}, CurrencyType: 1, Price: 50, DurationMs: 60000})
	require.NoError(t, err)
	tick(t, e)
	settleLegs(t, e, 5)
	tick(t, e)
	r, _ := state(t, e, offer.CommerceId)
	require.Equal(t, Manual, r.Status)
	var credits int
	require.NoError(t, e.Repo.DB.QueryRow("SELECT COUNT(*) FROM trade_asset_op WHERE stream=4").Scan(&credits))
	require.Zero(t, credits)
}

func TestTiDBBidOutbidRefundAndLateBid(t *testing.T) {
	e := tiDBEngine(t)
	ctx := context.Background()
	lot, err := e.CreateLot(ctx, 11, &tradepb.CreateAuctionLotRequest{ClientRequestId: "auction_001", Assets: &tradepb.TradeAssetSelection{Items: map[uint32]uint32{10: 1}}, CurrencyType: 1, OpeningPrice: 50, MinimumIncrement: 5, DurationMs: 60000})
	require.NoError(t, err)
	tick(t, e)
	settleLegs(t, e, 2)
	tick(t, e)
	for i, p := range []uint64{21, 22} {
		bid, err := e.PlaceBid(ctx, p, lot.CommerceId, &tradepb.PlaceAuctionBidRequest{LotId: fmt.Sprint(lot.CommerceId), ClientRequestId: fmt.Sprintf("bid_000%d", i), Amount: uint64(50 + i*10)})
		require.NoError(t, err)
		tick(t, e)
		settleLegs(t, e, 2)
		tick(t, e)
		tick(t, e)
		_, bw := state(t, e, bid.CommerceId)
		require.Equal(t, "bid_held", bw.Stage)
	}
	tick(t, e)
	var refunds int
	require.NoError(t, e.Repo.DB.QueryRow("SELECT COUNT(*) FROM trade_asset_op WHERE stream=4").Scan(&refunds))
	require.Equal(t, 1, refunds, "被超价者原币退款待办")
	_, err = e.Repo.DB.Exec("UPDATE trade_commerce SET expires_ms=1 WHERE commerce_id=?", lot.CommerceId)
	require.NoError(t, err)
	_, err = e.PlaceBid(ctx, 23, lot.CommerceId, &tradepb.PlaceAuctionBidRequest{ClientRequestId: "late_bid_01", Amount: 100})
	require.ErrorIs(t, err, ErrEnded)
}

func TestTiDBBidReplayKeepsOriginalContractAndDebitAfterPriceAdvanceAndExpiry(t *testing.T) {
	e := tiDBEngine(t)
	ctx := context.Background()
	lot, err := e.CreateLot(ctx, 11, &tradepb.CreateAuctionLotRequest{ClientRequestId: "bid-replay-lot-001", Assets: &tradepb.TradeAssetSelection{Items: map[uint32]uint32{10: 1}}, CurrencyType: 1, OpeningPrice: 50, MinimumIncrement: 5, DurationMs: 60000})
	require.NoError(t, err)
	require.NoError(t, e.advance(ctx, lot))
	settleLegs(t, e, 2)
	lot, _ = state(t, e, lot.CommerceId)
	require.NoError(t, e.advance(ctx, lot))
	input := &tradepb.PlaceAuctionBidRequest{LotId: fmt.Sprint(lot.CommerceId), ClientRequestId: "bid-replay-request-001", Amount: 50}
	var wg sync.WaitGroup
	type bidResult struct {
		bid *tradepb.TradeCommerceRecord
		err error
	}
	bids := make(chan bidResult, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bid, err := e.PlaceBid(ctx, 21, lot.CommerceId, input)
			bids <- bidResult{bid: bid, err: err}
		}()
	}
	wg.Wait()
	close(bids)
	var original *tradepb.TradeCommerceRecord
	results := 0
	for result := range bids {
		results++
		require.NoError(t, result.err)
		bid := result.bid
		require.NotNil(t, bid)
		if original == nil {
			original = bid
		}
		require.Equal(t, original.CommerceId, bid.CommerceId, "并发重复请求只能创建一份出价合同")
	}
	require.Equal(t, 8, results)
	require.NoError(t, e.advance(ctx, original))
	var opID, epoch, seq, deadline uint64
	require.NoError(t, e.Repo.DB.QueryRow("SELECT op_id,stream_epoch,seq,deadline_ms FROM trade_asset_op WHERE ref_id=? AND stream=3", original.CommerceId).Scan(&opID, &epoch, &seq, &deadline))
	settleLegs(t, e, 2)
	original, _ = state(t, e, original.CommerceId)
	require.NoError(t, e.advance(ctx, original))
	require.NoError(t, e.acceptBid(ctx, original.CommerceId))
	_, workflow := state(t, e, original.CommerceId)
	require.Equal(t, "bid_held", workflow.Stage)
	// 成为最高价后，原金额已不满足下一次加价；原请求仍必须返回原合同。
	replayed, err := e.PlaceBid(ctx, 21, lot.CommerceId, input)
	require.NoError(t, err)
	require.Equal(t, original.CommerceId, replayed.CommerceId)
	_, err = e.Repo.DB.Exec("UPDATE trade_commerce SET expires_ms=1 WHERE commerce_id=?", lot.CommerceId)
	require.NoError(t, err)
	replayed, err = e.PlaceBid(ctx, 21, lot.CommerceId, input)
	require.NoError(t, err, "拍品截止不改变已受理请求的身份")
	require.Equal(t, original.CommerceId, replayed.CommerceId)
	changed := proto.Clone(input).(*tradepb.PlaceAuctionBidRequest)
	changed.Amount++
	_, err = e.PlaceBid(ctx, 21, lot.CommerceId, changed)
	require.ErrorIs(t, err, ErrReplay, "同请求号换金额不能被截止检查掩盖")
	changed = proto.Clone(input).(*tradepb.PlaceAuctionBidRequest)
	changed.LotId = "another-lot"
	_, err = e.PlaceBid(ctx, 21, lot.CommerceId, changed)
	require.ErrorIs(t, err, ErrReplay, "拍品身份同样属于幂等摘要")
	changed = proto.Clone(input).(*tradepb.PlaceAuctionBidRequest)
	changed.ClientRequestId = "bid-replay-new-late-001"
	_, err = e.PlaceBid(ctx, 21, lot.CommerceId, changed)
	require.ErrorIs(t, err, ErrEnded, "只有已有请求可重放，不能接受截止后的新出价")
	var contracts, debits int
	var finalOpID, finalEpoch, finalSeq uint64
	require.NoError(t, e.Repo.DB.QueryRow("SELECT COUNT(*) FROM trade_commerce WHERE kind=?", BidKind).Scan(&contracts))
	require.NoError(t, e.Repo.DB.QueryRow("SELECT COUNT(*),MAX(op_id),MAX(stream_epoch),MAX(seq) FROM trade_asset_op WHERE player_id=21 AND stream=3").Scan(&debits, &finalOpID, &finalEpoch, &finalSeq))
	require.Equal(t, 1, contracts)
	require.Equal(t, 1, debits)
	require.Equal(t, opID, finalOpID, "重放不得另发扣款seq")
	require.Equal(t, epoch, finalEpoch)
	require.Equal(t, seq, finalSeq)
	require.Equal(t, lot.ExpiresMs, deadline, "扣款待办必须继承拍品截止，不能无限期保留可执行扣款")
}

func TestTransferPreservesInstanceUnknownBytes(t *testing.T) {
	raw := []byte{0x08, 0x01, 0xf8, 0x7f, 0x03}
	s := &assetpb.AssetSnapshot{Items: []*assetpb.AssetItemInstance{{ItemUuid: 77, ConfigId: 10, StackSize: 1, InstanceProto: raw}}}
	out, err := Transfer(&assetpb.AssetBundle{ItemUuids: []uint64{77}}, s)
	require.NoError(t, err)
	require.True(t, proto.Equal(s.Items[0], out.ItemInstances[0]))
}

func TestTiDBContractCannotFinalizeFromRedisDurableOnly(t *testing.T) {
	e := tiDBEngine(t)
	offer, err := e.CreateConsignment(context.Background(), 11, &tradepb.CreateConsignmentRequest{ClientRequestId: "proof-guard-001", Assets: &tradepb.TradeAssetSelection{Items: map[uint32]uint32{10: 1}}, CurrencyType: 1, Price: 50, DurationMs: 60000})
	require.NoError(t, err)
	tick(t, e)
	var id uint64
	require.NoError(t, e.Repo.DB.QueryRow("SELECT op_id FROM trade_asset_op WHERE ref_id=?", offer.CommerceId).Scan(&id))
	ok, err := e.Repo.Ops.Finalize(context.Background(), assetop.Op{OpID: id}, assetop.StatusApplied, assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED, Durable: true}, uint64(time.Now().UnixMilli()))
	require.Error(t, err)
	require.False(t, ok)
	var status uint32
	require.NoError(t, e.Repo.DB.QueryRow("SELECT status FROM trade_asset_op WHERE op_id=?", id).Scan(&status))
	require.EqualValues(t, 1, status)
}

func TestTiDBMallBridgeWaitsForCommittedLegAndRecoversLostReceipt(t *testing.T) {
	e := tiDBEngine(t)
	ctx := context.Background()
	in := mallIntent{OrderID: "00112233-4455-6677-8899-aabbccddeeff", BuyerID: "22", BuyerAccount: "account-22", BuyerOwnerRevision: "1", SKU: 1, Quantity: 1, Config: 10, Count: 2, Title: "玄铁"}
	var acknowledgements atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "trade", r.Header.Get("X-Commerce-Subject"))
		require.Len(t, r.Header.Get("X-Commerce-Signature"), 64)
		if r.URL.Path == "/internal/deliveries/poll" {
			require.NoError(t, json.NewEncoder(w).Encode(mallPollPage{Entries: []mallPollEntry{mallTestEntry(in, "")}}))
			return
		}
		if r.URL.Path == "/internal/deliveries/receipt" {
			var receipt map[string]string
			require.NoError(t, json.NewDecoder(r.Body).Decode(&receipt))
			require.Equal(t, "DELIVERED", receipt["state"])
			require.Equal(t, in.OrderID, receipt["orderId"])
			if acknowledgements.Add(1) == 1 {
				w.WriteHeader(503)
			}
			return
		}
		w.WriteHeader(404)
	}))
	defer server.Close()
	m, err := NewMall(server.URL, "gateway-only-test-secret-00000000001", "delivery-only-test-secret-0000000002", e)
	require.NoError(t, err)
	require.NoError(t, m.Tick(ctx))
	tick(t, e)
	require.NoError(t, m.Tick(ctx))
	require.Zero(t, acknowledgements.Load(), "仅RPC成功或待办入库不能通知商城已发货")
	var ops int
	require.NoError(t, e.Repo.DB.QueryRow("SELECT COUNT(*) FROM trade_asset_op WHERE stream=4").Scan(&ops))
	require.Equal(t, 1, ops)
	settleLegs(t, e, 2)
	tick(t, e)
	require.Error(t, m.Tick(ctx), "丢失回执保持同一合同")
	require.NoError(t, m.Tick(ctx))
	require.EqualValues(t, 2, acknowledgements.Load())
	require.NoError(t, e.Repo.DB.QueryRow("SELECT COUNT(*) FROM trade_asset_op WHERE stream=4").Scan(&ops))
	require.Equal(t, 1, ops, "恢复不得换seq重发资产")
}

func mallTestEntry(in mallIntent, cursor string) mallPollEntry {
	return mallPollEntry{Intent: &in, Cursor: &cursor}
}
func mallTestCursor(after, upper string) string {
	return "v1:1:" + after + ":1:" + upper
}

// 固定有限队列的HTTP替身；真实SQL翻页由mall_game的DeliveryStoreTest覆盖。
// 允许从任一已尝试项续扫，且故意持续返回旧意图，以验证worker重启仍沿用原订单。
type mallPageProbe struct {
	mu              sync.Mutex
	requests        []string
	receiptOrderIDs []string
	receipts        atomic.Int32
}

func newMallPageServer(t *testing.T, intents []mallIntent) (*httptest.Server, *mallPageProbe) {
	t.Helper()
	pages := make(map[string]mallPollPage)
	upper := intents[len(intents)-1].OrderID
	for after := range intents {
		key := ""
		if after > 0 {
			key = mallTestCursor(intents[after-1].OrderID, upper)
		}
		page := mallPollPage{Entries: []mallPollEntry{}}
		for i := after; i < min(after+20, len(intents)); i++ {
			cursor := mallTestCursor(intents[i].OrderID, upper)
			if i == len(intents)-1 {
				cursor = ""
			}
			page.Entries = append(page.Entries, mallTestEntry(intents[i], cursor))
		}
		pages[key] = page
	}
	probe := &mallPageProbe{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal/deliveries/poll":
			var input struct {
				Limit  int    `json:"limit"`
				Cursor string `json:"cursor"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Errorf("读取poll请求: %v", err)
				w.WriteHeader(400)
				return
			}
			probe.mu.Lock()
			probe.requests = append(probe.requests, input.Cursor)
			probe.mu.Unlock()
			page, exists := pages[input.Cursor]
			if !exists || input.Limit != 20 {
				t.Errorf("未知poll游标或批量: %q / %d", input.Cursor, input.Limit)
				w.WriteHeader(400)
				return
			}
			if err := json.NewEncoder(w).Encode(page); err != nil {
				t.Errorf("写poll响应: %v", err)
			}
		case "/internal/deliveries/receipt":
			var receipt struct {
				OrderID string `json:"orderId"`
				State   string `json:"state"`
			}
			if err := json.NewDecoder(r.Body).Decode(&receipt); err != nil || receipt.State != "DELIVERED" {
				t.Errorf("读取交付回执: state=%q, err=%v", receipt.State, err)
				w.WriteHeader(400)
				return
			}
			probe.mu.Lock()
			probe.receiptOrderIDs = append(probe.receiptOrderIDs, receipt.OrderID)
			probe.receipts.Add(1)
			probe.mu.Unlock()
		default:
			w.WriteHeader(404)
		}
	}))
	return server, probe
}

func mallTestIntents(count int) []mallIntent {
	intents := make([]mallIntent, 0, count)
	for i := range count {
		intents = append(intents, mallIntent{OrderID: fmt.Sprintf("%08x-0000-0000-0000-000000000000", i+1), BuyerID: fmt.Sprint(100 + i), BuyerAccount: fmt.Sprint("account-", 100+i), BuyerOwnerRevision: "1", SKU: 1, Quantity: 1, Config: 10, Count: 1, Title: "玄铁"})
	}
	return intents
}

func TestTiDBMallPagingReachesOfflineTailAndRestartKeepsOriginalContractAndSeq(t *testing.T) {
	e := tiDBEngine(t)
	ctx := context.Background()
	intents := mallTestIntents(25)
	server, probe := newMallPageServer(t, intents)
	defer server.Close()
	m, err := NewMall(server.URL, "gateway-only-test-secret-00000000001", "delivery-only-test-secret-0000000002", e)
	require.NoError(t, err)
	require.NoError(t, m.Tick(ctx))
	require.Equal(t, mallTestCursor(intents[19].OrderID, intents[24].OrderID), m.pollCursor)
	first, err := m.accept(ctx, intents[0])
	require.NoError(t, err)
	require.NoError(t, m.Tick(ctx), "前20个买家尚未到账，第21至25单仍必须建合同")
	require.Empty(t, m.pollCursor)
	var contracts int
	require.NoError(t, e.Repo.DB.QueryRow("SELECT COUNT(*) FROM trade_commerce WHERE kind=1").Scan(&contracts))
	require.Equal(t, 25, contracts)
	require.Zero(t, probe.receipts.Load(), "翻页不是持久到账证据")
	type creditIdentity struct{ OpID, Epoch, Seq uint64 }
	creditIdentities := func() []creditIdentity {
		rows, err := e.Repo.DB.Query("SELECT op_id,stream_epoch,seq FROM trade_asset_op WHERE stream=4 ORDER BY op_id")
		require.NoError(t, err)
		defer rows.Close()
		var identities []creditIdentity
		for rows.Next() {
			var identity creditIdentity
			require.NoError(t, rows.Scan(&identity.OpID, &identity.Epoch, &identity.Seq))
			identities = append(identities, identity)
		}
		require.NoError(t, rows.Err())
		return identities
	}
	tick(t, e)
	originalOps := creditIdentities()
	require.Len(t, originalOps, 25)
	// 新worker从首页重读；cursor包装不进入旧Intent的摘要，不另建合同。
	restarted, err := NewMall(server.URL, m.GatewaySecret, m.DeliverySecret, e)
	require.NoError(t, err)
	require.NoError(t, restarted.Tick(ctx))
	require.NoError(t, restarted.Tick(ctx))
	replayed, err := restarted.accept(ctx, intents[0])
	require.NoError(t, err)
	require.Equal(t, first.CommerceId, replayed.CommerceId)
	require.Equal(t, first.RequestDigest, replayed.RequestDigest)
	tick(t, e)
	require.Equal(t, originalOps, creditIdentities(), "worker重启重放不得替换原SQL op_id/epoch/seq")
	// 仅后五名玩家取得SQL终局；首页二十名仍离线待办时，后页已能独立回报到账。
	for _, intent := range intents[20:] {
		player, err := strconv.ParseUint(intent.BuyerID, 10, 64)
		require.NoError(t, err)
		contract, err := e.Repo.ByRequest(ctx, RequestKey("official", player, intent.OrderID))
		require.NoError(t, err)
		result, err := e.Repo.DB.Exec("UPDATE trade_asset_op SET status=2,durable=1 WHERE ref_id=? AND stream=4 AND status=1", contract.CommerceId)
		require.NoError(t, err)
		updated, err := result.RowsAffected()
		require.NoError(t, err)
		require.EqualValues(t, 1, updated)
	}
	tick(t, e)
	require.NoError(t, restarted.Tick(ctx))
	require.Zero(t, probe.receipts.Load(), "首页二十项仍未到账")
	require.NoError(t, restarted.Tick(ctx))
	require.EqualValues(t, 5, probe.receipts.Load(), "首页未终局不挡后五项已提交交付回执")
	probe.mu.Lock()
	tailReceipts := append([]string(nil), probe.receiptOrderIDs...)
	probe.mu.Unlock()
	var expectedTail []string
	for _, intent := range intents[20:] {
		expectedTail = append(expectedTail, intent.OrderID)
	}
	require.Equal(t, expectedTail, tailReceipts, "这五条回执必须恰为后五单，不能由首页订单充数")
	var pending int
	require.NoError(t, e.Repo.DB.QueryRow("SELECT COUNT(*) FROM trade_asset_op WHERE stream=4 AND status=1").Scan(&pending))
	require.Equal(t, 20, pending)
	settleLegs(t, e, 2)
	tick(t, e)
	require.NoError(t, restarted.Tick(ctx))
	require.EqualValues(t, 25, probe.receipts.Load())
	tick(t, e)
	require.Equal(t, originalOps, creditIdentities(), "原订单跨页/重启/丢回执后保留全部25条原SQL op_id/epoch/seq")
}

type cancelFirstMallID struct {
	next   IDs
	cancel context.CancelFunc
	calls  atomic.Int32
}

func (i *cancelFirstMallID) Next(ctx context.Context) (uint64, error) {
	n, err := i.next.Next(ctx)
	if i.calls.Add(1) == 1 {
		i.cancel()
	}
	return n, err
}

func TestTiDBMallPagingCancellationNeverSkipsUnattemptedEntries(t *testing.T) {
	e := tiDBEngine(t)
	intents := mallTestIntents(3)
	server, _ := newMallPageServer(t, intents)
	defer server.Close()
	m, err := NewMall(server.URL, "gateway-only-test-secret-00000000001", "delivery-only-test-secret-0000000002", e)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	originalIDs := e.IDs
	ids := &cancelFirstMallID{next: originalIDs, cancel: cancel}
	e.IDs = ids
	require.ErrorIs(t, m.Tick(ctx), context.Canceled)
	require.EqualValues(t, 1, ids.calls.Load())
	require.Equal(t, mallTestCursor(intents[0].OrderID, intents[2].OrderID), m.pollCursor, "只前移已尝试的首项，不能跳过第二、三项")
	e.IDs = originalIDs
	require.NoError(t, m.Tick(context.Background()))
	require.Empty(t, m.pollCursor)
	require.NoError(t, m.Tick(context.Background()), "下轮绕回补首项提交未知的原订单")
	var contracts int
	require.NoError(t, e.Repo.DB.QueryRow("SELECT COUNT(*) FROM trade_commerce WHERE kind=1").Scan(&contracts))
	require.Equal(t, 3, contracts)
}

func TestMallPagingRejectsInvalidResponseWithoutMovingCursor(t *testing.T) {
	intents := mallTestIntents(3)
	requested := mallTestCursor(intents[0].OrderID, intents[2].OrderID)
	for _, response := range []struct {
		body   string
		status int
	}{
		{`{}`, 200}, {`{"entries":null}`, 200}, {`{"entries":[{"intent":{},"cursor":""}]}`, 200},
		{`{"entries":[{"intent":{"orderId":"00000002-0000-0000-0000-000000000000"},"cursor":"v2:1:00000002-0000-0000-0000-000000000000:1:00000003-0000-0000-0000-000000000000"}]}`, 200},
		{`{"entries":[]}`, 503},
	} {
		t.Run(fmt.Sprintf("%d-%s", response.status, response.body), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(response.status)
				_, _ = w.Write([]byte(response.body))
			}))
			defer server.Close()
			m, err := NewMall(server.URL, "gateway-only-test-secret-00000000001", "delivery-only-test-secret-0000000002", nil)
			require.NoError(t, err)
			m.pollCursor = requested
			require.Error(t, m.Tick(context.Background()))
			require.Equal(t, requested, m.pollCursor)
		})
	}
	t.Run("空尾页绕回", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"entries":[]}`)) }))
		defer server.Close()
		m, err := NewMall(server.URL, "gateway-only-test-secret-00000000001", "delivery-only-test-secret-0000000002", nil)
		require.NoError(t, err)
		m.pollCursor = requested
		require.NoError(t, m.Tick(context.Background()))
		require.Empty(t, m.pollCursor)
	})
}
