package commerce

// tick_isolation_db_test.go —— 第一轮审计 §2.5 go-asset-services#0 / #1 的真库回归(2026-10-20)。
//
// 跑法(二选一;每例新建随机库、结束删库,只能指向一次性实例,账号要能 CREATE / DROP DATABASE):
//   - 一次性 MySQL:COMMERCE_TEST_MYSQL_DSN='root:<密码>@tcp(127.0.0.1:<端口>)/'  COMMERCE_REQUIRE_MYSQL=1
//   - 一次性 TiDB :COMMERCE_TEST_DSN=...(与 engine_tidb_test.go 同一门控)     COMMERCE_REQUIRE_TIDB=1
// 两个 DSN 都设时用 MySQL。都没设时整组 Skip —— Skip 不代表通过,验收必须带 REQUIRE 开关并在 -v 里逐条看到 PASS。
// 资产终局由夹具或生产写路径(Claim → Abandon、ResolveManually)直接落在 outbox 上,验证的是交易 SQL 状态机,
// 不冒充 scene 端到端。
//
// 2026-10-22 按复核意见补(文件后半):生产 3s 预算下队头卡住 / 行锁被占、旧版本推迟与隔离落空、
// 隔离截止在途托管扣款、角色冲突分钟级复查、乐观冲突不推迟、拍品身份不合格时退款、坏对手单移出盘口、
// 两个副本同时推进只补偿一次。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	assetpb "proto/common/asset"
	base "proto/common/base"
	componentpb "proto/common/component"
	dspb "proto/data_service"
	tradepb "proto/trade"
	"shared/assetop"
	"trade/internal/data"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// realDBEngine 给本文件的用例选一个真库:优先一次性 MySQL,否则沿用 TiDB 门控(tiDBEngine)。
func realDBEngine(t *testing.T) *Engine {
	t.Helper()
	if dsn := os.Getenv("COMMERCE_TEST_MYSQL_DSN"); dsn != "" {
		return openEngineDB(t, dsn, false)
	}
	if os.Getenv("COMMERCE_REQUIRE_MYSQL") == "1" {
		t.Fatal("缺少 COMMERCE_TEST_MYSQL_DSN")
	}
	return tiDBEngine(t)
}

// testClock 是可拨动的引擎时钟(Engine.Now):推迟到期只靠拨钟,不睡墙钟。
// 库侧的 CURRENT_TIMESTAMP(截止判定)仍是真时钟,用例里的截止时间都留足一小时,拨钟两分钟不跨越它。
type testClock struct{ ms atomic.Int64 }

func newTestClock() *testClock {
	c := &testClock{}
	c.ms.Store(time.Now().UnixMilli())
	return c
}
func (c *testClock) Now() time.Time          { return time.UnixMilli(c.ms.Load()) }
func (c *testClock) Advance(d time.Duration) { c.ms.Add(d.Milliseconds()) }

func consignment(t *testing.T, e *Engine, seller uint64, requestID string) *tradepb.TradeCommerceRecord {
	t.Helper()
	r, err := e.CreateConsignment(context.Background(), seller, &tradepb.CreateConsignmentRequest{
		ClientRequestId: requestID, Assets: &tradepb.TradeAssetSelection{Items: map[uint32]uint32{10: 2}},
		CurrencyType: 1, Price: 50, DurationMs: 3_600_000,
	})
	require.NoError(t, err)
	return r
}

// fillPendingDebits 走生产路径(EnsureSeqRows → AllocateSeqsTx → InsertOp)给 player 的 TRADE_DEBIT 流塞 n 条未决行。
// ref_kind=LISTING:不挂在任何合同上,用例可以单独终结它们。
func fillPendingDebits(t *testing.T, e *Engine, player uint64, n int) {
	t.Helper()
	ctx := context.Background()
	key := data.SeqKey{PlayerID: player, Stream: assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT}
	require.NoError(t, e.Repo.Ops.EnsureSeqRows(ctx, e.now(), key))
	payload, err := proto.Marshal(&assetpb.AssetBundle{Items: []*assetpb.ItemGrant{{ConfigId: 10, Count: 1}}})
	require.NoError(t, err)
	for i := 0; i < n; i++ {
		opID, err := e.OpIDs.Next(ctx)
		require.NoError(t, err)
		require.NoError(t, e.Repo.Tx(ctx, func(tx *sql.Tx) error {
			alloc, err := e.Repo.Ops.AllocateSeqsTx(ctx, tx, e.now(), key)
			if err != nil {
				return err
			}
			a := alloc[key]
			return e.Repo.Ops.InsertOp(ctx, tx, &tradepb.TradeAssetOpRecord{
				OpId: opID, PlayerId: player, Stream: uint32(key.Stream), StreamEpoch: a.Epoch, Seq: a.Seq,
				Kind: tradepb.TradeAssetOpKind_TRADE_ASSET_OP_KIND_ESCROW_DEBIT, Status: tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_PENDING,
				NextAttemptMs: e.now(), RefKind: tradepb.TradeAssetOpRefKind_TRADE_ASSET_OP_REF_KIND_LISTING, RefId: 7000 + uint64(i),
				Payload: payload, CreatedMs: e.now(), UpdatedMs: e.now(),
			})
		}))
	}
}

func dueIDs(t *testing.T, e *Engine) map[uint64]bool {
	t.Helper()
	rows, err := e.Repo.Due(context.Background(), e.now(), tickDueLimit)
	require.NoError(t, err)
	out := make(map[uint64]bool, len(rows))
	for _, r := range rows {
		out[r.CommerceId] = true
	}
	return out
}

func opCount(t *testing.T, e *Engine, player uint64, stream uint32) int {
	t.Helper()
	var n int
	require.NoError(t, e.Repo.DB.QueryRow("SELECT COUNT(*) FROM trade_asset_op WHERE player_id=? AND stream=?", player, stream).Scan(&n))
	return n
}

// buyerDebit 取寄售购买的扣款腿(角色名 buyer_debit_<序号>_<请求键>)。
func buyerDebit(w *tradepb.TradeCommerceWorkflow) *tradepb.TradeCommerceLeg {
	for _, l := range w.Legs {
		if strings.HasPrefix(l.Role, "buyer_debit_") {
			return l
		}
	}
	return nil
}

// abandonOp 走生产的自动放弃写路径:Claim 领租约 → Abandon 带令牌 CAS(刻意不置 durable)。
// 放弃之前的六条证据(玩家无位置、账本确认未见……)由 assetop.Loop.AbandonOnce 复核,不在本文件范围。
func abandonOp(t *testing.T, e *Engine, opID uint64) {
	t.Helper()
	ctx := context.Background()
	now := e.now()
	op, claimed, err := e.Repo.Ops.Claim(ctx, opID, now, now+60_000, now+120_000, 77)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, e.Repo.Ops.Abandon(ctx, op, now))
	var status int32
	var durable bool
	require.NoError(t, e.Repo.DB.QueryRow("SELECT status,durable FROM trade_asset_op WHERE op_id=?", opID).Scan(&status, &durable))
	require.EqualValues(t, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_ABANDONED, status)
	require.False(t, durable, "放弃不置 durable:修复前合同正是因此读不到它")
}

// 审计 #0 的原始触发:队头合同撞上 ErrTooManyPending(对方离线攒满 16 条未决)。
// 修复前 Tick 在这一行整轮返回、updated_ms 不变,后面的合同每一轮都轮不到。
func TestRealDBTickFailingHeadRowDoesNotBlockOthers(t *testing.T) {
	e := realDBEngine(t)
	clock := newTestClock()
	e.Now = clock.Now
	fillPendingDebits(t, e, 11, int(assetop.DefaultLimits.MaxPending))
	head := consignment(t, e, 11, "blocked-head-01")
	var others []uint64
	for p := uint64(12); p <= 14; p++ {
		clock.Advance(time.Millisecond) // 让队头的 updated_ms 严格最早
		others = append(others, consignment(t, e, p, fmt.Sprintf("healthy-offer-%d", p)).CommerceId)
	}

	tick(t, e)
	for _, id := range others {
		_, w := state(t, e, id)
		debit := leg(w, "seller_debit")
		require.NotNil(t, debit, "队头失败不能挡住后面的合同")
		require.NotZero(t, debit.OpId)
	}
	hr, hw := state(t, e, head.CommerceId)
	require.Nil(t, leg(hw, "seller_debit"), "失败那一轮的合同事务整体回滚,不留半截托管")
	require.Equal(t, head.Version, hr.Version, "推迟不是业务变化,不改版本")
	nowMs := e.now()
	pendingMs := uint64(tickPendingBackoff.Milliseconds())
	require.GreaterOrEqual(t, hr.UpdatedMs, nowMs+pendingMs*8/10, "失败行按积压背压的退避推迟")
	require.LessOrEqual(t, hr.UpdatedMs, nowMs+pendingMs*12/10, "推迟有界")
	require.False(t, dueIDs(t, e)[head.CommerceId], "推迟期内 Due 不再把它排在队头")

	tick(t, e)
	again, _ := state(t, e, head.CommerceId)
	require.Equal(t, hr.UpdatedMs, again.UpdatedMs, "推迟期内不重试")
	require.Equal(t, hr.Version, again.Version)

	// 积压消化(那 16 条终结)且推迟到期之后照常推进:推迟不是丢弃。
	_, err := e.Repo.DB.Exec("UPDATE trade_asset_op SET status=3,durable=1 WHERE ref_kind=1 AND status=1")
	require.NoError(t, err)
	clock.Advance(2 * time.Minute)
	tick(t, e)
	_, hw = state(t, e, head.CommerceId)
	require.NotNil(t, leg(hw, "seller_debit"))
	require.NotZero(t, leg(hw, "seller_debit").OpId)
}

// 合同自身数据坏了(解不开 / plan 判错)是确定性失败:隔离为 Manual 并移出 Due,不按暂时失败无限重试,也不挡其余合同。
func TestRealDBTickQuarantinesCorruptContracts(t *testing.T) {
	e := realDBEngine(t)
	ctx := context.Background()
	now := e.now()
	garbage := []byte{0xff, 0xff, 0xff} // 不完整的 varint,proto 必解不开
	refund, err := proto.MarshalOptions{Deterministic: true}.Marshal(&tradepb.TradeCommerceWorkflow{Stage: "bid_refund", SellerIdentity: &tradepb.TradeRoleIdentity{Account: "account-32", OwnerRevision: 1}})
	require.NoError(t, err)
	undecodable := &tradepb.TradeCommerceRecord{CommerceId: 900001, RequestKey: "corrupt-undecodable", RequestDigest: []byte{1},
		Kind: 3, Status: Escrowing, SellerPlayerId: 31, ExpiresMs: now + 3_600_000, CreatedMs: now - 2000, UpdatedMs: now - 2000,
		Version: 1, WorkflowProto: garbage}
	// 退款阶段却没有已提交的冻结扣款:plan 判"竞价退款缺已提交扣款"。
	contradictory := &tradepb.TradeCommerceRecord{CommerceId: 900002, RequestKey: "corrupt-plan", RequestDigest: []byte{2},
		Kind: BidKind, Status: CancelPending, SellerPlayerId: 32, CreatedMs: now - 1000, UpdatedMs: now - 1000,
		Version: 1, WorkflowProto: refund}
	for _, r := range []*tradepb.TradeCommerceRecord{undecodable, contradictory} {
		require.NoError(t, e.Repo.Tx(ctx, func(tx *sql.Tx) error { return e.Repo.Insert(ctx, tx, r) }))
	}
	healthy := consignment(t, e, 33, "healthy-behind-corrupt")

	tick(t, e)
	_, hw := state(t, e, healthy.CommerceId)
	require.NotNil(t, leg(hw, "seller_debit"), "坏数据行不能挡住后面的合同")

	bad, err := e.Repo.Get(ctx, undecodable.CommerceId)
	require.NoError(t, err)
	require.Equal(t, Manual, bad.Status, "解不开的合同隔离为人工")
	require.Equal(t, garbage, bad.WorkflowProto, "坏字节原样留作取证")
	require.EqualValues(t, 2, bad.Version)
	pr, pw := state(t, e, contradictory.CommerceId)
	require.Equal(t, Manual, pr.Status)
	require.Equal(t, "manual", pw.Stage, "stage 改成 manual:玩家操作与撮合都不会把它拉回推进")
	require.EqualValues(t, 2, pr.Version)
	require.Zero(t, opCount(t, e, 32, 4), "隔离不发出任何资产指令")

	tick(t, e)
	for _, id := range []uint64{undecodable.CommerceId, contradictory.CommerceId} {
		r, err := e.Repo.Get(ctx, id)
		require.NoError(t, err)
		require.EqualValues(t, 2, r.Version, "隔离之后不再被 Due 取到")
	}
}

// 审计 #1 的原始场景:买家点击购买后下线不回来,扣款腿被自动放弃(durable 保持 0)。
// 修复前合同永远停在 pay,卖家托管的资产被扣作人质;现在放弃 = scene 从未扣过买家,寄售重新开放,
// 之后到期下架只退回卖家一次,买家不收任何"退款"。
func TestRealDBAbandonedBuyerDebitReleasesSellerEscrowOnce(t *testing.T) {
	e := realDBEngine(t)
	ctx := context.Background()
	offer := consignment(t, e, 11, "abandon-offer-01")
	tick(t, e)
	settleLegs(t, e, 2)
	tick(t, e)
	_, w := state(t, e, offer.CommerceId)
	require.Equal(t, "open", w.Stage)
	_, err := e.Mutate(ctx, 22, offer.CommerceId, "buy", "abandon-buy-0001")
	require.NoError(t, err)
	tick(t, e)
	_, w = state(t, e, offer.CommerceId)
	require.Equal(t, "pay", w.Stage)
	debit := buyerDebit(w)
	require.NotNil(t, debit)
	require.NotZero(t, debit.OpId)

	abandonOp(t, e, debit.OpId)

	tick(t, e)
	r, w := state(t, e, offer.CommerceId)
	require.Equal(t, "open", w.Stage, "放弃的扣款按没扣到处理:寄售重新开放,卖家托管不再被扣作人质")
	require.Zero(t, r.BuyerPlayerId)
	require.Equal(t, Escrowing, r.Status)
	require.EqualValues(t, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_ABANDONED, buyerDebit(w).Outcome)
	require.Zero(t, opCount(t, e, 22, 4), "买家从未被扣,不给买家发任何退款")

	// 到期下架:退回卖家的 credit 只建一次,反复推进不再加。
	_, err = e.Repo.DB.Exec("UPDATE trade_commerce SET expires_ms=1 WHERE commerce_id=?", offer.CommerceId)
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		tick(t, e)
	}
	r, w = state(t, e, offer.CommerceId)
	require.Equal(t, "return", w.Stage)
	require.Equal(t, CancelPending, r.Status)
	require.Equal(t, 1, opCount(t, e, 11, 4))
	settleLegs(t, e, 2)
	tick(t, e)
	tick(t, e)
	r, _ = state(t, e, offer.CommerceId)
	require.Equal(t, Cancelled, r.Status)
	require.Equal(t, 1, opCount(t, e, 11, 4), "结案后不再补偿")
	require.Equal(t, 1, opCount(t, e, 22, 3), "被放弃的那条 seq 不重发")
}

// 人工终结同样不置 durable。修复前人工终结过的腿能不能被合同读到,取决于它此前某次重排有没有恰好写过 durable=1。
func TestRealDBManuallyResolvedLegsAdvanceContracts(t *testing.T) {
	e := realDBEngine(t)
	ctx := context.Background()
	resolve := func(opID uint64, final assetop.Status) {
		t.Helper()
		ok, err := assetop.ResolveManually(ctx, e.Repo.Ops, assetop.ManualResolution{
			OpID: opID, Final: final, Operator: "ops-regression", Reason: "按 scene 流水 correlation_id 判定",
		}, nil, time.Now)
		require.NoError(t, err)
		require.True(t, ok)
	}

	// 寄售:卖家托管人工判定成功(堆叠材料,不需要托管原件)→ 上架。
	offer := consignment(t, e, 41, "manual-offer-01")
	tick(t, e)
	_, w := state(t, e, offer.CommerceId)
	resolve(leg(w, "seller_debit").OpId, assetop.StatusApplied)
	tick(t, e)
	_, w = state(t, e, offer.CommerceId)
	require.Equal(t, "open", w.Stage)

	// 双方交易:买家托管人工判定中止 → 只退卖家一次,不给买家发货也不给买家退款。
	trade, err := e.CreatePlayerTrade(ctx, 51, &tradepb.CreatePlayerTradeRequest{ClientRequestId: "manual-trade-01", BuyerPlayerId: 52,
		SellerAssets: &tradepb.TradeAssetSelection{Items: map[uint32]uint32{10: 2}},
		BuyerAssets:  &tradepb.TradeAssetSelection{CurrencyType: 1, CurrencyAmount: 70}})
	require.NoError(t, err)
	tick(t, e)
	_, w = state(t, e, trade.CommerceId)
	resolve(leg(w, "seller_debit").OpId, assetop.StatusApplied)
	tick(t, e)
	_, err = e.Mutate(ctx, 52, trade.CommerceId, "confirm")
	require.NoError(t, err)
	tick(t, e)
	_, w = state(t, e, trade.CommerceId)
	require.Equal(t, "buyer_escrow", w.Stage)
	resolve(leg(w, "buyer_debit").OpId, assetop.StatusAborted)
	for i := 0; i < 3; i++ {
		tick(t, e)
	}
	r, w := state(t, e, trade.CommerceId)
	require.Equal(t, "return", w.Stage)
	require.Equal(t, CancelPending, r.Status)
	require.NotNil(t, leg(w, "seller_return"))
	require.Nil(t, leg(w, "buyer_return"))
	require.Nil(t, leg(w, "buyer_receive"))
	require.Equal(t, 1, opCount(t, e, 51, 4))
	require.Zero(t, opCount(t, e, 52, 4))
}

// 人工判定"实例扣款已成功"时 outbox 行没有托管原件(ResolveManually 不写 escrow_snapshot;自动路径缺原件根本不终结)。
// 合同必须在记下结局时就隔离为人工,不能先上架、等买家付了款才在发货时撞错,把第二个玩家的钱也压进人工单。
func TestRealDBManualAppliedInstanceDebitWithoutOriginalIsQuarantined(t *testing.T) {
	e := realDBEngine(t)
	ctx := context.Background()
	e.Policy.AllowInstances = true
	offer, err := e.CreateConsignment(ctx, 81, &tradepb.CreateConsignmentRequest{ClientRequestId: "manual-instance-01",
		Assets: &tradepb.TradeAssetSelection{ItemUuids: []uint64{77}}, CurrencyType: 1, Price: 50, DurationMs: 3_600_000})
	require.NoError(t, err)
	tick(t, e)
	_, w := state(t, e, offer.CommerceId)
	debit := leg(w, "seller_debit")
	require.NotNil(t, debit)
	ok, err := assetop.ResolveManually(ctx, e.Repo.Ops, assetop.ManualResolution{
		OpID: debit.OpId, Final: assetop.StatusApplied, Operator: "ops-regression", Reason: "scene 流水显示已扣出",
	}, nil, time.Now)
	require.NoError(t, err)
	require.True(t, ok)

	tick(t, e)
	r, w := state(t, e, offer.CommerceId)
	require.Equal(t, Manual, r.Status, "没有托管原件就不能上架")
	require.Equal(t, "manual", w.Stage)
	require.Zero(t, opCount(t, e, 81, 4), "隔离不发出任何退回")
	require.False(t, dueIDs(t, e)[offer.CommerceId])
	_, err = e.Mutate(ctx, 82, offer.CommerceId, "buy", "manual-instance-buy-1")
	require.ErrorIs(t, err, ErrConflict, "隔离后的寄售不接受购买")
}

// bid_ready 的出价还没被拍品接受,拍品不引用它。拍品行缺失(或解不开)时这条出价按不合格原币退回,
// 不能像以前那样每轮返回错误、冻结的钱永远退不回来;退款腿只建一次。
func TestRealDBReadyBidWithMissingLotIsRefundedOnce(t *testing.T) {
	e := realDBEngine(t)
	ctx := context.Background()
	now := e.now()
	sel := &tradepb.TradeAssetSelection{CurrencyType: 1, CurrencyAmount: 60}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(&tradepb.TradeCommerceWorkflow{Stage: "bid_ready", SellerAssets: sel, SellerIdentity: &tradepb.TradeRoleIdentity{Account: "account-71", OwnerRevision: 1},
		Legs: []*tradepb.TradeCommerceLeg{{Role: "bid_debit", PlayerId: 71, Stream: 3, Bundle: Bundle(sel), OpId: 424242, Outcome: 2}}})
	require.NoError(t, err)
	bid := &tradepb.TradeCommerceRecord{CommerceId: 900101, RequestKey: "orphan-ready-bid", RequestDigest: []byte{3}, Kind: BidKind,
		Status: Escrowing, SellerPlayerId: 71, ParentId: 999999, UnitPrice: 60, CurrencyType: 1,
		CreatedMs: now - 1000, UpdatedMs: now - 1000, Version: 1, WorkflowProto: raw}
	require.NoError(t, e.Repo.Tx(ctx, func(tx *sql.Tx) error { return e.Repo.Insert(ctx, tx, bid) }))

	tick(t, e)
	r, w := state(t, e, bid.CommerceId)
	require.Equal(t, "bid_refund", w.Stage)
	require.Equal(t, CancelPending, r.Status)
	for i := 0; i < 3; i++ {
		tick(t, e)
	}
	_, w = state(t, e, bid.CommerceId)
	require.Equal(t, "return", w.Stage)
	refund := leg(w, "bid_return")
	require.NotNil(t, refund)
	require.EqualValues(t, 60, refund.Bundle.Currencies[0].Amount)
	require.Equal(t, 1, opCount(t, e, 71, 4), "原币退款只建一次")
}

// 持有中的竞价在拍品结算前无事可做。修复前它的 updated_ms 不变、每轮都排在 Due 队头,
// 有出价的在拍拍品数一到 Due 上限,Due 就只回这些行,拍品自己也推进不了。
func TestRealDBHeldBidWaitsOutsideDueHead(t *testing.T) {
	e := realDBEngine(t)
	ctx := context.Background()
	lot, err := e.CreateLot(ctx, 11, &tradepb.CreateAuctionLotRequest{ClientRequestId: "held-lot-0001",
		Assets: &tradepb.TradeAssetSelection{Items: map[uint32]uint32{10: 1}}, CurrencyType: 1, OpeningPrice: 50, MinimumIncrement: 5, DurationMs: 3_600_000})
	require.NoError(t, err)
	tick(t, e)
	settleLegs(t, e, 2)
	tick(t, e)
	placeHeldBid := func(player, amount uint64, requestID string) *tradepb.TradeCommerceRecord {
		t.Helper()
		bid, err := e.PlaceBid(ctx, player, lot.CommerceId, &tradepb.PlaceAuctionBidRequest{LotId: fmt.Sprint(lot.CommerceId), ClientRequestId: requestID, Amount: amount})
		require.NoError(t, err)
		tick(t, e)
		settleLegs(t, e, 2)
		tick(t, e)
		tick(t, e)
		_, bw := state(t, e, bid.CommerceId)
		require.Equal(t, "bid_held", bw.Stage)
		return bid
	}
	first := placeHeldBid(21, 60, "held-bid-0001")
	before, _ := state(t, e, first.CommerceId)

	tick(t, e)
	held, _ := state(t, e, first.CommerceId)
	lr, _ := state(t, e, lot.CommerceId)
	require.Equal(t, lr.ExpiresMs, held.UpdatedMs, "推迟到拍品截止:之前它不可能结算")
	require.Equal(t, before.Version, held.Version, "推迟不改版本")
	require.False(t, dueIDs(t, e)[first.CommerceId])

	// 被超价时照常立刻回到推进:新竞价接受时把它改为 bid_refund,Save 把 updated_ms 写回现在。
	placeHeldBid(22, 70, "held-bid-0002")
	_, fw := state(t, e, first.CommerceId)
	require.Equal(t, "bid_refund", fw.Stage)
	require.True(t, dueIDs(t, e)[first.CommerceId])
	tick(t, e)
	require.Equal(t, 1, opCount(t, e, 21, 4), "被超价者原币退款待办")
}

// recordingApplier 记录收据 ACK 尝试过哪些 op(correlation_id = op_id),一律回本地 NOT_HERE(卖家离线)。
type recordingApplier struct {
	mu   sync.Mutex
	seen []uint64
}

func (a *recordingApplier) Do(_ context.Context, _ assetop.RPC, req *assetpb.AssetOpRequest) (assetop.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seen = append(a.seen, req.GetCorrelationId())
	return assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_NOT_HERE, Local: true, Absent: true}, nil
}

func (a *recordingApplier) attempts() []uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]uint64(nil), a.seen...)
}

func (a *recordingApplier) distinct() map[uint64]bool {
	out := map[uint64]bool{}
	for _, id := range a.attempts() {
		out[id] = true
	}
	return out
}

// emptyLedger 实现 SQLCommitLedgerReader:AcknowledgeReceipt 要求账本读取器能读 SQL 提交证据。
// 本用例的 ACK 在 Applier 那一步就回 NOT_HERE,走不到读账本。
type emptyLedger struct{}

func (emptyLedger) ReadPersistedLedger(context.Context, uint64) (*componentpb.PlayerAssetOpLedgerComp, error) {
	return nil, nil
}
func (emptyLedger) ReadSQLCommittedLedger(context.Context, uint64) (*componentpb.PlayerAssetOpLedgerComp, error) {
	return nil, nil
}

// 离线卖家的收据 ACK 一直 done=false。修复前每轮都从表头取同样的 100 条,后面的 ACK 永远轮不到。
func TestRealDBReceiptAckPagesPastStuckHead(t *testing.T) {
	e := realDBEngine(t)
	ctx := context.Background()
	applier := &recordingApplier{}
	e.Applier = applier
	e.Ledger = emptyLedger{}
	payload, err := proto.Marshal(&assetpb.AssetBundle{Items: []*assetpb.ItemGrant{{ConfigId: 10, Count: 1}}})
	require.NoError(t, err)
	total := tickAckLimit + tickAckLimit/2
	now := e.now()
	for i := 1; i <= total; i++ {
		rec := &tradepb.TradeAssetOpRecord{OpId: uint64(i), PlayerId: 61, Stream: uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT),
			StreamEpoch: 1, Seq: uint64(i), Kind: tradepb.TradeAssetOpKind_TRADE_ASSET_OP_KIND_ESCROW_DEBIT,
			Status: tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED, Durable: true, ReceiptAckPending: true,
			RefKind: tradepb.TradeAssetOpRefKind_TRADE_ASSET_OP_REF_KIND_ORDER, RefId: 1, Payload: payload,
			NextAttemptMs: now, CreatedMs: now, UpdatedMs: now}
		require.NoError(t, e.Repo.Tx(ctx, func(tx *sql.Tx) error { return e.Repo.Ops.InsertOp(ctx, tx, rec) }))
	}

	tick(t, e)
	require.Len(t, applier.distinct(), tickAckLimit)
	tick(t, e)
	require.Len(t, applier.distinct(), total, "第二轮接着上一页往后取,表头卡住的 ACK 不再挡住后面的")
	before := len(applier.attempts())
	tick(t, e)
	after := applier.attempts()
	require.Greater(t, len(after), before)
	require.Equal(t, uint64(1), after[before], "到表尾后从头轮转")
}

// 同一页前两条分别模拟 RPC 失败和 SQL 账本失败，其余均正常离线。
// 依赖错误必须由 Tick 上报，同时不能阻止同页余项或下一页；待办保留到再次轮转。
type dependencyFailureAckApplier struct {
	recordingApplier
	rpcError error
}

func (a *dependencyFailureAckApplier) Do(ctx context.Context, rpc assetop.RPC, req *assetpb.AssetOpRequest) (assetop.Result, error) {
	result, err := a.recordingApplier.Do(ctx, rpc, req)
	switch req.GetCorrelationId() {
	case 1:
		return assetop.Result{}, a.rpcError
	case 2:
		return assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED}, nil
	default:
		return result, err
	}
}

type dependencyFailureAckLedger struct {
	emptyLedger
	sqlError error
}

func (r dependencyFailureAckLedger) ReadSQLCommittedLedger(context.Context, uint64) (*componentpb.PlayerAssetOpLedgerComp, error) {
	return nil, r.sqlError
}

func TestRealDBReceiptAckReportsDependencyErrorsWithoutBlockingPages(t *testing.T) {
	e := realDBEngine(t)
	ctx := context.Background()
	rpcFailure := errors.New("测试收据RPC不可用")
	sqlFailure := errors.New("测试SQL账本不可用")
	applier := &dependencyFailureAckApplier{rpcError: rpcFailure}
	e.Applier = applier
	e.Ledger = dependencyFailureAckLedger{sqlError: sqlFailure}
	payload, err := proto.Marshal(&assetpb.AssetBundle{Items: []*assetpb.ItemGrant{{ConfigId: 10, Count: 1}}})
	require.NoError(t, err)
	now := e.now()
	for i := 1; i <= tickAckLimit+1; i++ {
		rec := &tradepb.TradeAssetOpRecord{OpId: uint64(i), PlayerId: 63, Stream: uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT),
			StreamEpoch: 1, Seq: uint64(i), Kind: tradepb.TradeAssetOpKind_TRADE_ASSET_OP_KIND_ESCROW_DEBIT,
			Status: tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED, Durable: true, ReceiptAckPending: true,
			RefKind: tradepb.TradeAssetOpRefKind_TRADE_ASSET_OP_REF_KIND_ORDER, RefId: 1, Payload: payload,
			NextAttemptMs: now, CreatedMs: now, UpdatedMs: now}
		require.NoError(t, e.Repo.Tx(ctx, func(tx *sql.Tx) error { return e.Repo.Ops.InsertOp(ctx, tx, rec) }))
	}

	err = e.Tick(ctx)
	require.ErrorIs(t, err, rpcFailure, "RPC故障不能伪装成正常离线或成功Tick")
	require.ErrorIs(t, err, sqlFailure, "页末汇总仍须保留SQL账本故障原因")
	require.Len(t, applier.attempts(), tickAckLimit, "前两条失败不能阻止本页余项")
	require.EqualValues(t, tickAckLimit, e.ackAfterOpID)
	var pending int
	require.NoError(t, e.Repo.DB.QueryRow("SELECT COUNT(*) FROM trade_asset_op WHERE receipt_ack_pending=1").Scan(&pending))
	require.Equal(t, tickAckLimit+1, pending, "依赖错误与正常离线都不能清除未获SQL确认的收据待办")

	require.NoError(t, e.Tick(ctx), "正常离线done=false不属于故障，下一页仍应成功")
	require.Len(t, applier.distinct(), tickAckLimit+1)
	require.Zero(t, e.ackAfterOpID, "读到尾页后回到表头")
	err = e.Tick(ctx)
	require.ErrorIs(t, err, rpcFailure, "原失败项仍须再次尝试")
	require.ErrorIs(t, err, sqlFailure)
	require.EqualValues(t, 1, applier.attempts()[tickAckLimit+1], "页尾后须重新尝试未清理的失败项")
}

// 一行收据 ACK 的 payload 解不开时,修复前 AckDue 整页报错、游标停在它前面,全服收据 ACK 停摆。
// 现在跳过这一行,同页其余 ACK 照常尝试,Tick 不报错;坏行原样留在表里等人工(不清 receipt_ack_pending)。
func TestRealDBReceiptAckSkipsUndecodablePayload(t *testing.T) {
	e := realDBEngine(t)
	ctx := context.Background()
	applier := &recordingApplier{}
	e.Applier = applier
	e.Ledger = emptyLedger{}
	good, err := proto.Marshal(&assetpb.AssetBundle{Items: []*assetpb.ItemGrant{{ConfigId: 10, Count: 1}}})
	require.NoError(t, err)
	now := e.now()
	for i := 1; i <= 3; i++ {
		payload := good
		if i == 1 {
			payload = []byte{0xff, 0xff, 0xff}
		}
		rec := &tradepb.TradeAssetOpRecord{OpId: uint64(i), PlayerId: 62, Stream: uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT),
			StreamEpoch: 1, Seq: uint64(i), Kind: tradepb.TradeAssetOpKind_TRADE_ASSET_OP_KIND_ESCROW_DEBIT,
			Status: tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED, Durable: true, ReceiptAckPending: true,
			RefKind: tradepb.TradeAssetOpRefKind_TRADE_ASSET_OP_REF_KIND_ORDER, RefId: 1, Payload: payload,
			NextAttemptMs: now, CreatedMs: now, UpdatedMs: now}
		require.NoError(t, e.Repo.Tx(ctx, func(tx *sql.Tx) error { return e.Repo.Ops.InsertOp(ctx, tx, rec) }))
	}

	tick(t, e)
	seen := applier.distinct()
	require.False(t, seen[1], "解不开的 payload 不发 ACK")
	require.True(t, seen[2], "坏行不能挡住同页后面的 ACK")
	require.True(t, seen[3])
	var pending bool
	require.NoError(t, e.Repo.DB.QueryRow("SELECT receipt_ack_pending FROM trade_asset_op WHERE op_id=1").Scan(&pending))
	require.True(t, pending, "坏行保留 ACK 待办,等人工")
}

// ---- 2026-10-22 按复核意见补的回归 ----

// productionTickBudget 与 svc.AssetChannel.Start 每轮给合同推进的子预算一致。
const productionTickBudget = 3 * time.Second

// stallingRoles 让涉及 stalled 玩家的角色互锁一直等到 ctx 结束才返回,模拟"一行卡住":
// 在 account_role 上等锁、目录事务持锁、依赖 RPC 不回包,表现都是只在调用方的 ctx 到期时才返回。
type stallingRoles struct {
	fixtureRoles
	stalled uint64
}

func (s stallingRoles) Lock(ctx context.Context, tx *sql.Tx, listing uint64, players ...uint64) error {
	if slices.Contains(players, s.stalled) {
		<-ctx.Done()
		return ctx.Err()
	}
	return s.fixtureRoles.Lock(ctx, tx, listing, players...)
}

// 复核 must_fix:单行子预算以前固定 5s,比 svc 给的 3s 还长,卡住的队头与父 ctx 同时到期,
// Tick 直接返回、不推迟它,下一秒它又排在队头吃满整轮 —— 依赖卡多久,全服合同推进就停多久。
func TestRealDBTickStalledHeadRowIsCutBeforeTheRoundBudget(t *testing.T) {
	e := realDBEngine(t)
	clock := newTestClock()
	e.Now = clock.Now
	head := consignment(t, e, 91, "stalled-head-01")
	var behind []uint64
	for p := uint64(92); p <= 94; p++ {
		clock.Advance(time.Millisecond) // 让队头的 updated_ms 严格最早
		behind = append(behind, consignment(t, e, p, fmt.Sprintf("behind-stalled-%d", p)).CommerceId)
	}
	e.Roles = stallingRoles{stalled: 91}

	round, cancel := context.WithTimeout(context.Background(), productionTickBudget)
	defer cancel()
	require.NoError(t, e.Tick(round), "卡住的行只算它自己的失败,整轮不报错")
	require.NoError(t, round.Err(), "整轮在父预算之内结束:卡住的行撞上的是自己的子预算")

	for _, id := range behind {
		_, w := state(t, e, id)
		require.NotZero(t, leg(w, "seller_debit").GetOpId(), "队头卡住时同一轮后面的合同照常推进")
	}
	hr, hw := state(t, e, head.CommerceId)
	require.Nil(t, leg(hw, "seller_debit"), "卡住那一轮的合同事务整体回滚,不留半截托管")
	require.Equal(t, head.Version, hr.Version, "推迟不改版本")
	nowMs := e.now()
	retryMs := uint64(tickRetryBackoff.Milliseconds())
	require.GreaterOrEqual(t, hr.UpdatedMs, nowMs+retryMs*8/10, "父 ctx 还有余量,卡住的行被推迟,下一轮不再排在队头")
	require.LessOrEqual(t, hr.UpdatedMs, nowMs+retryMs*12/10)
	require.False(t, dueIDs(t, e)[head.CommerceId])
}

// 复核 must_fix 的另一种卡法:别的事务(比如事务中途被分区的另一个副本)一直持有队头合同行的行锁。
// 这时推迟也要这把锁,同样拿不到 —— 这一行下一轮仍在队头,但每轮只花它自己的子预算与推迟预算,
// 同一轮其余合同照常推进;锁一放开它就照常推进。
func TestRealDBTickLockedHeadRowDoesNotStarveTheRound(t *testing.T) {
	e := realDBEngine(t)
	clock := newTestClock()
	e.Now = clock.Now
	ctx := context.Background()
	head := consignment(t, e, 95, "locked-head-01")
	var behind []uint64
	for p := uint64(96); p <= 98; p++ {
		clock.Advance(time.Millisecond)
		behind = append(behind, consignment(t, e, p, fmt.Sprintf("behind-locked-%d", p)).CommerceId)
	}
	blocker, err := e.Repo.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = blocker.Rollback() }()
	var locked uint64
	require.NoError(t, blocker.QueryRowContext(ctx, "SELECT commerce_id FROM trade_commerce WHERE commerce_id=? FOR UPDATE", head.CommerceId).Scan(&locked))

	round, cancel := context.WithTimeout(ctx, productionTickBudget)
	err = e.Tick(round)
	cancel()
	require.NoError(t, err)
	for _, id := range behind {
		_, w := state(t, e, id)
		require.NotZero(t, leg(w, "seller_debit").GetOpId(), "队头行锁被占时同一轮后面的合同照常推进")
	}
	hr, _ := state(t, e, head.CommerceId)
	require.Equal(t, head.Version, hr.Version)
	require.Equal(t, head.UpdatedMs, hr.UpdatedMs, "行锁在别人手里时推迟同样拿不到锁,这一行保持到期")

	require.NoError(t, blocker.Rollback())
	tick(t, e)
	_, hw := state(t, e, head.CommerceId)
	require.NotZero(t, leg(hw, "seller_debit").GetOpId(), "锁一放开,下一轮照常推进")
}

// Due 之后行被别的写者推进过:拿 Due 读到的旧版本去推迟或隔离都必须落空,不改 updated_ms、status 与 stage(不旧盖新)。
func TestRealDBStaleObservationNeitherDefersNorQuarantines(t *testing.T) {
	e := realDBEngine(t)
	ctx := context.Background()
	offer := consignment(t, e, 101, "stale-defer-01")
	stale, err := e.Repo.Get(ctx, offer.CommerceId) // Due 读到的那一版
	require.NoError(t, err)
	tick(t, e) // 别的写者(这里是一轮推进)把它推进到下一版
	fresh, freshW := state(t, e, offer.CommerceId)
	require.Greater(t, fresh.Version, stale.Version)

	e.deferCommerce(ctx, stale, errors.New("store timeout"))
	e.deferCommerce(ctx, stale, corrupt(errors.New("workflow 解不开")))
	e.deferCommerce(ctx, stale, roleConflict())

	after, afterW := state(t, e, offer.CommerceId)
	require.Equal(t, fresh.Version, after.Version, "旧观察不能改新行")
	require.Equal(t, fresh.UpdatedMs, after.UpdatedMs, "旧版本的推迟落空")
	require.Equal(t, fresh.Status, after.Status, "旧版本的隔离落空")
	require.Equal(t, freshW.Stage, afterW.Stage)
	require.True(t, dueIDs(t, e)[offer.CommerceId], "新状态照常到期")
}

// 复核 should_fix:隔离以前只改合同行,在途的托管扣款照常投递,隔离之后还会继续把资产扣进人工单。
// 现在隔离与玩家取消同一写法:把合同名下仍待办的托管扣款截止提前到现在,由资产通道用同一个 seq 改发 Abort 定论。
func TestRealDBQuarantineExpiresInFlightEscrowDebits(t *testing.T) {
	e := realDBEngine(t)
	ctx := context.Background()
	offer := consignment(t, e, 102, "quarantine-inflight-01")
	tick(t, e)
	settleLegs(t, e, 2)
	tick(t, e)
	_, err := e.Mutate(ctx, 103, offer.CommerceId, "buy", "quarantine-buy-0001")
	require.NoError(t, err)
	tick(t, e)
	r, w := state(t, e, offer.CommerceId)
	require.Equal(t, "pay", w.Stage)
	debit := buyerDebit(w)
	require.NotZero(t, debit.GetOpId())
	var deadline uint64
	require.NoError(t, e.Repo.DB.QueryRow("SELECT deadline_ms FROM trade_asset_op WHERE op_id=?", debit.OpId).Scan(&deadline))
	require.Greater(t, deadline, e.now(), "买家扣款原截止是寄售到期")

	// 制造确定性坏数据:一条已发出却查不到 outbox 行的腿(数据事故的典型形状),排在仍在途的买家扣款之后。
	w.Legs = append(w.Legs, &tradepb.TradeCommerceLeg{Role: "ghost_debit", PlayerId: 102, Stream: 3, OpId: 987654321})
	require.NoError(t, e.Repo.Tx(ctx, func(tx *sql.Tx) error {
		cur, err := e.Repo.Lock(ctx, tx, r.CommerceId)
		if err != nil {
			return err
		}
		return e.Repo.Save(ctx, tx, cur, w, e.now())
	}))

	tick(t, e)
	qr, qw := state(t, e, offer.CommerceId)
	require.Equal(t, Manual, qr.Status)
	require.Equal(t, "manual", qw.Stage)
	var status int32
	require.NoError(t, e.Repo.DB.QueryRow("SELECT status,deadline_ms FROM trade_asset_op WHERE op_id=?", debit.OpId).Scan(&status, &deadline))
	require.EqualValues(t, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_PENDING, status, "隔离不替 scene 判结局:同一个 seq 由资产通道改发 Abort 定论")
	require.LessOrEqual(t, deadline, e.now(), "在途扣款截止提前到隔离时刻:没扣到的买家不会再被扣进人工单")
	require.Zero(t, opCount(t, e, 103, 4), "隔离不发出任何退款")
	require.Zero(t, opCount(t, e, 102, 4), "隔离不退回任何托管")
}

// 复核 should_fix:角色换主是长期状态,以前按暂时失败每 10 秒重试一次并打 ERROR,指标上与存储抖动分不开。
func TestRealDBRoleConflictBacksOffForMinutes(t *testing.T) {
	e := realDBEngine(t)
	clock := newTestClock()
	e.Now = clock.Now
	offer := consignment(t, e, 104, "role-conflict-01")
	// 卖家角色挂单之后换了主:合同里存的原归属身份(account-104, 修订 1)永远对不上了。
	e.Roles = fixtureRoles{Values: map[uint64]*dspb.AccountRoleOwnership{
		104: {Account: "account-new-owner", OwnerRevision: 2, Role: &base.AccountSimplePlayer{PlayerId: 104}},
	}}

	tick(t, e)
	r, w := state(t, e, offer.CommerceId)
	require.Nil(t, leg(w, "seller_debit"), "已换主的角色不托管、不发资产")
	require.Equal(t, offer.Version, r.Version, "推迟不改版本")
	require.NotEqual(t, Manual, r.Status, "角色冲突不是坏数据,不隔离")
	nowMs := e.now()
	backoffMs := uint64(tickRoleConflictBackoff.Milliseconds())
	require.GreaterOrEqual(t, r.UpdatedMs, nowMs+backoffMs*8/10, "按分钟级复查,不再每 10 秒重试")
	require.LessOrEqual(t, r.UpdatedMs, nowMs+backoffMs*12/10)
}

// racingRoles 在 acceptBid 拿角色锁的那一刻(预读拍品之后、锁拍品之前)跑一次 race,
// 模拟另一个副本恰好在这个窗口里接受了别的出价。只在角色集合同时包含 bidder 与 lotSeller 时触发(即 acceptBid)。
type racingRoles struct {
	fixtureRoles
	bidder, lotSeller uint64
	fired             *atomic.Bool
	race              func(context.Context) error
}

func (r racingRoles) Lock(ctx context.Context, tx *sql.Tx, listing uint64, players ...uint64) error {
	if slices.Contains(players, r.bidder) && slices.Contains(players, r.lotSeller) && r.fired.CompareAndSwap(false, true) {
		if err := r.race(ctx); err != nil {
			return err
		}
	}
	return r.fixtureRoles.Lock(ctx, tx, listing, players...)
}

// 复核 should_fix:乐观冲突以前按暂时失败推迟 10 秒。拍品临近截止时,这会把一条已冻结的有效高价出价拖过截止、
// 变成退款,改变拍卖结果。现在乐观冲突不推迟,下一轮按新状态立即重试。
func TestRealDBBidRaceIsRetriedNextRoundNotDeferred(t *testing.T) {
	e := realDBEngine(t)
	clock := newTestClock()
	e.Now = clock.Now
	ctx := context.Background()
	lot, err := e.CreateLot(ctx, 11, &tradepb.CreateAuctionLotRequest{ClientRequestId: "race-lot-0001",
		Assets: &tradepb.TradeAssetSelection{Items: map[uint32]uint32{10: 1}}, CurrencyType: 1, OpeningPrice: 50, MinimumIncrement: 5, DurationMs: 3_600_000})
	require.NoError(t, err)
	tick(t, e)
	settleLegs(t, e, 2)
	tick(t, e)
	high, err := e.PlaceBid(ctx, 21, lot.CommerceId, &tradepb.PlaceAuctionBidRequest{LotId: fmt.Sprint(lot.CommerceId), ClientRequestId: "race-bid-high", Amount: 70})
	require.NoError(t, err)
	low, err := e.PlaceBid(ctx, 22, lot.CommerceId, &tradepb.PlaceAuctionBidRequest{LotId: fmt.Sprint(lot.CommerceId), ClientRequestId: "race-bid-low1", Amount: 60})
	require.NoError(t, err)
	tick(t, e)
	settleLegs(t, e, 2)
	tick(t, e)
	before, bw := state(t, e, high.CommerceId)
	require.Equal(t, "bid_ready", bw.Stage)

	// 高价出价预读拍品之后、锁拍品之前,"另一个副本"接受了低价出价,拍品的买家变了。
	e.Roles = racingRoles{bidder: 21, lotSeller: 11, fired: &atomic.Bool{}, race: func(ctx context.Context) error {
		return e.acceptBid(ctx, low.CommerceId)
	}}
	tick(t, e)
	raced, rw := state(t, e, high.CommerceId)
	require.Equal(t, "bid_ready", rw.Stage)
	require.Equal(t, before.Version, raced.Version)
	require.Equal(t, before.UpdatedMs, raced.UpdatedMs, "乐观冲突不推迟")
	require.True(t, dueIDs(t, e)[high.CommerceId], "下一轮立即重试")

	tick(t, e)
	_, hw := state(t, e, high.CommerceId)
	require.Equal(t, "bid_held", hw.Stage, "重读之后高价出价照常成为最高价")
	_, lw := state(t, e, low.CommerceId)
	require.Equal(t, "bid_refund", lw.Stage, "被超价的低价出价原币退回")
}

// 复核 should_fix:拍品自己缺原归属身份时,以前被隔离为人工的是这条 bid_ready 出价,冻结的钱一起进了人工单。
// 现在拍品不可接受出价(缺身份 / workflow 解不开)一律按不合格原币退款;坏拍品由它自己那一行隔离。
func TestRealDBReadyBidOnUnusableLotIsRefunded(t *testing.T) {
	e := realDBEngine(t)
	ctx := context.Background()
	now := e.now()
	insert := func(r *tradepb.TradeCommerceRecord) {
		t.Helper()
		require.NoError(t, e.Repo.Tx(ctx, func(tx *sql.Tx) error { return e.Repo.Insert(ctx, tx, r) }))
	}
	noIdentity, err := proto.MarshalOptions{Deterministic: true}.Marshal(&tradepb.TradeCommerceWorkflow{Stage: "lot_open", MinimumIncrement: 5,
		SellerAssets: &tradepb.TradeAssetSelection{Items: map[uint32]uint32{10: 1}}})
	require.NoError(t, err)
	lots := []*tradepb.TradeCommerceRecord{
		{CommerceId: 900201, RequestKey: "lot-without-identity", RequestDigest: []byte{4}, Kind: 5, Status: Escrowing, SellerPlayerId: 72,
			UnitPrice: 50, CurrencyType: 1, ExpiresMs: now + 3_600_000, CreatedMs: now - 5000, UpdatedMs: now - 5000, Version: 1, WorkflowProto: noIdentity},
		{CommerceId: 900202, RequestKey: "lot-undecodable", RequestDigest: []byte{5}, Kind: 5, Status: Escrowing, SellerPlayerId: 73,
			UnitPrice: 50, CurrencyType: 1, ExpiresMs: now + 3_600_000, CreatedMs: now - 5000, UpdatedMs: now - 5000, Version: 1, WorkflowProto: []byte{0xff, 0xff, 0xff}},
	}
	sel := &tradepb.TradeAssetSelection{CurrencyType: 1, CurrencyAmount: 60}
	for i, lot := range lots {
		insert(lot)
		bidder := uint64(74 + i)
		raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(&tradepb.TradeCommerceWorkflow{Stage: "bid_ready", SellerAssets: sel,
			SellerIdentity: &tradepb.TradeRoleIdentity{Account: fmt.Sprintf("account-%d", bidder), OwnerRevision: 1},
			Legs:           []*tradepb.TradeCommerceLeg{{Role: "bid_debit", PlayerId: bidder, Stream: 3, Bundle: Bundle(sel), OpId: 525250 + uint64(i), Outcome: 2}}})
		require.NoError(t, err)
		insert(&tradepb.TradeCommerceRecord{CommerceId: 900211 + uint64(i), RequestKey: fmt.Sprintf("bid-on-unusable-lot-%d", i), RequestDigest: []byte{byte(6 + i)},
			Kind: BidKind, Status: Escrowing, SellerPlayerId: bidder, ParentId: lot.CommerceId, UnitPrice: 60, CurrencyType: 1,
			ExpiresMs: lot.ExpiresMs, CreatedMs: now - 1000, UpdatedMs: now - 1000, Version: 1, WorkflowProto: raw})
	}

	for i := 0; i < 3; i++ {
		tick(t, e)
	}
	for i, lot := range lots {
		bidder := uint64(74 + i)
		br, bw := state(t, e, 900211+uint64(i))
		require.Equal(t, "return", bw.Stage, "拍品不可接受出价时这条出价原币退回,不进人工单")
		require.NotEqual(t, Manual, br.Status)
		require.NotNil(t, leg(bw, "bid_return"))
		require.Equal(t, 1, opCount(t, e, bidder, 4), "原币退款只建一次")
		lr, err := e.Repo.Get(ctx, lot.CommerceId)
		require.NoError(t, err)
		require.Equal(t, Manual, lr.Status, "坏拍品由它自己那一行隔离")
	}
}

// 复核 should_fix:对手单自己坏了时,以前报错的是 taker,taker 每轮打一条 ERROR,坏单停在最优价位上挡住整个盘口。
// 现在坏对手单在撮合事务里被隔离、移出盘口,taker 不报错,下一轮按次优价照常成交。
func TestRealDBUntradableCounterpartyIsTakenOffTheBook(t *testing.T) {
	e := realDBEngine(t)
	ctx := context.Background()
	open := func(player uint64, side tradepb.MarketOrderSide, price uint64) uint64 {
		t.Helper()
		r, err := e.PlaceMarket(ctx, player, &tradepb.PlaceMarketOrderRequest{ClientRequestId: fmt.Sprintf("book-open-%d", player), MarketId: 1, ItemConfigId: 10, Side: side, UnitPrice: price, Quantity: 1, CurrencyType: 1})
		require.NoError(t, err)
		require.NoError(t, e.advance(ctx, r))
		settleLegs(t, e, 2)
		r, _ = state(t, e, r.CommerceId)
		require.NoError(t, e.advance(ctx, r))
		return r.CommerceId
	}
	good := open(41, 1, 55)
	// 盘口最优的买单缺原归属身份(存量数据)。updated_ms 放在将来:它不在 Due 里,只有撮合会碰到它。
	now := e.now()
	sel := &tradepb.TradeAssetSelection{CurrencyType: 1, CurrencyAmount: 60}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(&tradepb.TradeCommerceWorkflow{Stage: "market_open", SellerAssets: sel,
		Legs: []*tradepb.TradeCommerceLeg{{Role: "seller_debit", PlayerId: 43, Stream: 3, Bundle: Bundle(sel), OpId: 626262, Outcome: 2}}})
	require.NoError(t, err)
	bad := &tradepb.TradeCommerceRecord{CommerceId: 900301, RequestKey: "book-without-identity", RequestDigest: []byte{9}, Kind: 4, Status: Escrowing,
		SellerPlayerId: 43, MarketId: 1, ItemConfigId: 10, Side: 1, UnitPrice: 60, Quantity: 1, CurrencyType: 1,
		CreatedMs: now - 10_000, UpdatedMs: now + 3_600_000, Version: 1, WorkflowProto: raw}
	require.NoError(t, e.Repo.Tx(ctx, func(tx *sql.Tx) error { return e.Repo.Insert(ctx, tx, bad) }))
	sell := open(42, 2, 50)

	require.NoError(t, e.matchMarket(ctx, sell), "对手单坏了不算 taker 的失败")
	br, bw := state(t, e, bad.CommerceId)
	require.Equal(t, Manual, br.Status, "坏对手单移出盘口")
	require.Equal(t, "manual", bw.Stage)
	require.Zero(t, br.FilledQuantity, "坏对手单没有成交")

	require.NoError(t, e.matchMarket(ctx, sell))
	var child uint64
	require.NoError(t, e.Repo.DB.QueryRow("SELECT commerce_id FROM trade_commerce WHERE kind=4 AND side=0 AND parent_id=? AND counterparty_id=?", good, sell).Scan(&child),
		"坏单移出盘口之后按次优价照常撮合")
}

// 两个副本同时对同一个库跑 Tick(生产就是这样:每个副本的 trade.commerce.reconcile 都推进同一张合同表)。
// 推进、放弃补偿、到期退回都只能各做一次:扣款 seq 不重发,退回卖家的 credit 只建一次,买家不收任何退款。
func TestRealDBTwoReplicasTickingTogetherCompensateOnce(t *testing.T) {
	a := realDBEngine(t)
	clock := newTestClock()
	a.Now = clock.Now
	b := *a // 同一个库、同一套号源(op_id / 合同号不撞)、同一个时钟,各自的 ACK 游标:等同两个副本
	ctx := context.Background()
	sellers := []uint64{111, 112, 113, 114}
	buyers := []uint64{121, 122, 123, 124}
	offers := make([]uint64, len(sellers))
	for i, seller := range sellers {
		offers[i] = consignment(t, a, seller, fmt.Sprintf("replica-offer-%d", i)).CommerceId
	}
	round := func() {
		t.Helper()
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, replica := range []*Engine{a, &b} {
			wg.Add(1)
			go func(i int, replica *Engine) {
				defer wg.Done()
				c, cancel := context.WithTimeout(ctx, 15*time.Second)
				defer cancel()
				errs[i] = replica.Tick(c)
			}(i, replica)
		}
		wg.Wait()
		require.NoError(t, errors.Join(errs...))
		clock.Advance(time.Minute) // 两个副本撞锁而被推迟的行(10s±20%)下一轮必然到期
	}
	untilAll := func(what string, done func(*tradepb.TradeCommerceRecord, *tradepb.TradeCommerceWorkflow) bool) {
		t.Helper()
		for attempt := 0; attempt < 10; attempt++ {
			round()
			all := true
			for _, id := range offers {
				if r, w := state(t, a, id); !done(r, w) {
					all = false
					break
				}
			}
			if all {
				return
			}
		}
		t.Fatalf("两个副本 10 轮内没有把全部合同推进到:%s", what)
	}

	untilAll("托管扣款已发出", func(_ *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) bool {
		return leg(w, "seller_debit").GetOpId() != 0
	})
	settleLegs(t, a, 2)
	untilAll("上架", func(_ *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) bool { return w.Stage == "open" })
	for i, id := range offers {
		_, err := a.Mutate(ctx, buyers[i], id, "buy", fmt.Sprintf("replica-buy-%04d", i))
		require.NoError(t, err)
	}
	untilAll("买家扣款已发出", func(_ *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) bool {
		return w.Stage == "pay" && buyerDebit(w).GetOpId() != 0
	})
	for _, id := range offers {
		_, w := state(t, a, id)
		abandonOp(t, a, buyerDebit(w).OpId)
	}
	untilAll("放弃后重新开放", func(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) bool {
		return w.Stage == "open" && r.BuyerPlayerId == 0
	})
	for _, id := range offers {
		_, err := a.Repo.DB.Exec("UPDATE trade_commerce SET expires_ms=1 WHERE commerce_id=?", id)
		require.NoError(t, err)
	}
	untilAll("退回卖家已发出", func(_ *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) bool {
		return w.Stage == "return" && leg(w, "seller_return").GetOpId() != 0
	})
	settleLegs(t, a, 2)
	untilAll("结案", func(r *tradepb.TradeCommerceRecord, _ *tradepb.TradeCommerceWorkflow) bool { return r.Status == Cancelled })

	for i := range offers {
		require.Equal(t, 1, opCount(t, a, sellers[i], 3), "卖家托管扣款只发一次")
		require.Equal(t, 1, opCount(t, a, sellers[i], 4), "退回卖家只建一次")
		require.Equal(t, 1, opCount(t, a, buyers[i], 3), "被放弃的买家扣款 seq 不重发")
		require.Zero(t, opCount(t, a, buyers[i], 4), "买家从未被扣,不收任何退款")
	}
}
