package commerce

// manual_db_test.go —— 人工处置(manual.go、manual_plan.go)的真库回归:锁序、版本条件、留痕,以及引擎接手发号。
// 门控同 tick_isolation_db_test.go(COMMERCE_TEST_MYSQL_DSN + COMMERCE_REQUIRE_MYSQL=1,或 TiDB 那一组);
// 都没设时 Skip,Skip 不代表通过。四类残余(jubaozhai-market.md §15.7.1)每类都有正常路径、重跑落空、
// 与引擎或另一个操作人并发三种情形。资产终局由夹具直接落在 outbox 上,验证的是交易 SQL 状态机,不冒充 scene 端到端。

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	assetpb "proto/common/asset"
	base "proto/common/base"
	dspb "proto/data_service"
	tradepb "proto/trade"
	"shared/assetop"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func manualDeps(e *Engine) ManualDeps { return ManualDeps{Repo: e.Repo, Roles: e.Roles} }

// setOpStatus 把一条在途的资产指令直接落成终局(夹具,同 settleLegs 的口径),并唤醒它所属的合同。
func setOpStatus(t *testing.T, e *Engine, opID uint64, status tradepb.TradeAssetOpStatus) {
	t.Helper()
	ctx := context.Background()
	result, err := e.Repo.DB.ExecContext(ctx, "UPDATE trade_asset_op SET status=?,durable=1 WHERE op_id=? AND status=1", int32(status), opID)
	require.NoError(t, err)
	n, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, n, "op_id=%d 应当还在途", opID)
	var ref uint64
	require.NoError(t, e.Repo.DB.QueryRowContext(ctx, "SELECT ref_id FROM trade_asset_op WHERE op_id=?", opID).Scan(&ref))
	require.NoError(t, e.Repo.Wake(ctx, ref, e.now()))
}

// opPayload 读一条资产指令的载荷(要发 / 要扣的资产)。
func opPayload(t *testing.T, e *Engine, opID uint64) *assetpb.AssetBundle {
	t.Helper()
	var raw []byte
	require.NoError(t, e.Repo.DB.QueryRow("SELECT payload FROM trade_asset_op WHERE op_id=?", opID).Scan(&raw))
	b := &assetpb.AssetBundle{}
	require.NoError(t, proto.Unmarshal(raw, b))
	return b
}

func injectCorrupt(t *testing.T, e *Engine, id uint64) {
	t.Helper()
	r, _ := state(t, e, id)
	_, quarantined, err := e.quarantine(context.Background(), r, errors.New("回归用例注入的坏数据"))
	require.NoError(t, err)
	require.True(t, quarantined)
}

// 上架后被隔离的寄售整单退回:计划不写库;执行后版本加一、留痕、新腿不带 op_id;同一条命令重跑不再生效;
// 引擎下一轮给退回腿发号写 outbox,到账后合同按取消结案,只退一次。
func TestRealDBManualRefundOfQuarantinedConsignment(t *testing.T) {
	e := realDBEngine(t)
	ctx := context.Background()
	resolve := func(opID uint64) {
		t.Helper()
		ok, err := assetop.ResolveManually(ctx, e.Repo.Ops, assetop.ManualResolution{
			OpID: opID, Final: assetop.StatusApplied, Operator: "ops-regression", Reason: "按 scene 流水 correlation_id 判定",
		}, nil, time.Now)
		require.NoError(t, err)
		require.True(t, ok)
	}

	offer := consignment(t, e, 61, "manual-refund-01")
	tick(t, e)
	_, w := state(t, e, offer.CommerceId)
	resolve(leg(w, "seller_debit").OpId)
	tick(t, e)
	r, w := state(t, e, offer.CommerceId)
	require.Equal(t, "open", w.Stage)

	stage, quarantined, err := e.quarantine(ctx, r, errors.New("回归用例注入的坏数据"))
	require.NoError(t, err)
	require.True(t, quarantined)
	require.Equal(t, "open", stage)
	r, w = state(t, e, offer.CommerceId)
	require.Equal(t, Manual, r.Status)
	require.Equal(t, "open", w.Quarantine.GetStage())
	require.Equal(t, manualReasonCorrupt, w.Quarantine.GetReason())

	req := ManualRequest{CommerceID: offer.CommerceId, ExpectVersion: r.Version, Action: ManualRefund, Operator: "ops", Reason: "ticket-1"}
	plan, err := ResolveManual(ctx, manualDeps(e), req, e.now(), false)
	require.NoError(t, err)
	require.Equal(t, []string{"seller_debit/refund"}, plan.NewLegs)
	unchanged, _ := state(t, e, offer.CommerceId)
	require.Equal(t, r.Version, unchanged.Version, "只算计划不写库")

	result, err := ResolveManual(ctx, manualDeps(e), req, e.now(), true)
	require.NoError(t, err)
	require.Equal(t, r.Version, result.VersionBefore)
	r2, w2 := state(t, e, offer.CommerceId)
	require.Equal(t, r.Version+1, r2.Version)
	require.Equal(t, CancelPending, r2.Status)
	require.Equal(t, "return", w2.Stage)
	refund := leg(w2, "seller_debit/refund")
	require.NotNil(t, refund)
	require.Zero(t, refund.OpId, "人工处置不发号,由引擎发")
	require.Len(t, w2.ManualActions, 1)
	require.Equal(t, "ops", w2.ManualActions[0].Operator)
	require.Equal(t, r.Version, w2.ManualActions[0].VersionBefore)

	_, err = ResolveManual(ctx, manualDeps(e), req, e.now(), true)
	require.ErrorIs(t, err, ErrManualVersionChanged, "同一条命令重跑不能再加一次腿")

	tick(t, e)
	_, w3 := state(t, e, offer.CommerceId)
	refund = leg(w3, "seller_debit/refund")
	require.NotZero(t, refund.OpId)
	require.Equal(t, 1, opCount(t, e, 61, 4), "只退一次")

	resolve(refund.OpId)
	tick(t, e)
	r4, w4 := state(t, e, offer.CommerceId)
	require.Equal(t, Cancelled, r4.Status)
	require.Equal(t, "done", w4.Stage)
	require.Equal(t, 1, opCount(t, e, 61, 4))
}

// 没转人工的合同不能处理;不存在的合同回 ErrManualNotFound。
func TestRealDBManualRefusesLiveAndMissingContracts(t *testing.T) {
	e := realDBEngine(t)
	ctx := context.Background()
	offer := consignment(t, e, 62, "manual-live-01")
	tick(t, e)
	r, _ := state(t, e, offer.CommerceId)

	_, err := ResolveManual(ctx, manualDeps(e), ManualRequest{CommerceID: offer.CommerceId, ExpectVersion: r.Version, Action: ManualResume, Operator: "ops", Reason: "t"}, e.now(), true)
	require.ErrorIs(t, err, ErrManualNotAllowed)
	after, _ := state(t, e, offer.CommerceId)
	require.Equal(t, r.Version, after.Version)

	_, err = ResolveManual(ctx, manualDeps(e), ManualRequest{CommerceID: 987654321, ExpectVersion: 1, Action: ManualResume, Operator: "ops", Reason: "t"}, e.now(), true)
	require.ErrorIs(t, err, ErrManualNotFound)
}

// ── 第 1 类:原主已换 ────────────────────────────────────────────

// 卖家角色在架期间过户:引擎当场转人工(owner_changed),不再按角色冲突每 5 分钟空转;别的动作拒绝,先改绑。
// 改绑到原账号名下的另一名角色:计划不写库、目标被冻结时拒绝(与目录并发)、执行留痕、重跑落空;
// 之后引擎按新的一方推进,撤单时托管退给目标角色,不退给已过户的原角色。
func TestRealDBOwnerChangeQuarantinesAndRedirectRebinds(t *testing.T) {
	e := realDBEngine(t)
	ctx := context.Background()
	roles := fixtureRoles{Values: map[uint64]*dspb.AccountRoleOwnership{}}
	e.Roles = roles
	offer := consignment(t, e, 131, "owner-change-01")
	tick(t, e)
	settleLegs(t, e, 2)
	tick(t, e)
	_, w := state(t, e, offer.CommerceId)
	require.Equal(t, "open", w.Stage)

	// 卖家角色过户:合同里存的原归属身份(account-131, 修订 1)永远对不上了;原账号名下另有角色 132。
	roles.Values[131] = &dspb.AccountRoleOwnership{Account: "account-new-owner", OwnerRevision: 2, Role: &base.AccountSimplePlayer{PlayerId: 131}}
	roles.Values[132] = &dspb.AccountRoleOwnership{Account: "account-131", OwnerRevision: 4, Role: &base.AccountSimplePlayer{PlayerId: 132}}
	require.NoError(t, e.Repo.Wake(ctx, offer.CommerceId, e.now()))
	tick(t, e)
	r, w := state(t, e, offer.CommerceId)
	require.Equal(t, Manual, r.Status, "换主不可逆:当场转人工")
	require.Equal(t, manualReasonOwnerChanged, w.Quarantine.GetReason())
	require.Equal(t, "open", w.Quarantine.GetStage())
	require.Contains(t, w.Quarantine.GetDetail(), "seller player=131")
	require.NotContains(t, w.Quarantine.GetDetail(), "account-new-owner", "转人工记录不写账号原文")

	deps := manualDeps(e)
	_, err := ResolveManual(ctx, deps, ManualRequest{CommerceID: offer.CommerceId, ExpectVersion: r.Version, Action: ManualRefund, Operator: "ops", Reason: "t"}, e.now(), true)
	require.ErrorIs(t, err, ErrManualNotAllowed, "一方已不归原身份时退回发不出去,先改绑")

	req := ManualRequest{CommerceID: offer.CommerceId, ExpectVersion: r.Version, Action: ManualRedirect, Party: partySeller, To: 132,
		Operator: "ops", Reason: "ticket-2: account-131 asks to move the listing to role 132"}
	plan, err := ResolveManual(ctx, deps, req, e.now(), false)
	require.NoError(t, err)
	require.Equal(t, uint64(132), plan.ToPlayer)
	unchanged, _ := state(t, e, offer.CommerceId)
	require.Equal(t, r.Version, unchanged.Version, "只算计划不写库")

	// 目标角色正被另一条挂单冻结:拒绝,不改合同。
	roles.Values[132] = &dspb.AccountRoleOwnership{Account: "account-131", OwnerRevision: 4, TradeListingId: 99, Role: &base.AccountSimplePlayer{PlayerId: 132}}
	_, err = ResolveManual(ctx, deps, req, e.now(), true)
	require.ErrorIs(t, err, ErrManualNotAllowed)
	unchanged, _ = state(t, e, offer.CommerceId)
	require.Equal(t, r.Version, unchanged.Version)
	roles.Values[132] = &dspb.AccountRoleOwnership{Account: "account-131", OwnerRevision: 4, Role: &base.AccountSimplePlayer{PlayerId: 132}}

	_, err = ResolveManual(ctx, deps, req, e.now(), true)
	require.NoError(t, err)
	r2, w2 := state(t, e, offer.CommerceId)
	require.Equal(t, uint64(132), r2.SellerPlayerId, "seller_player_id 随 Save 落库")
	require.True(t, proto.Equal(&tradepb.TradeRoleIdentity{Account: "account-131", OwnerRevision: 4}, w2.SellerIdentity))
	require.Equal(t, "open", w2.Stage)
	require.Len(t, w2.ManualActions, 1)
	audit := w2.ManualActions[0]
	require.Equal(t, partySeller, audit.Party)
	require.Equal(t, uint64(131), audit.FromPlayerId)
	require.Equal(t, uint64(132), audit.ToPlayerId)
	require.True(t, proto.Equal(&tradepb.TradeRoleIdentity{Account: "account-131", OwnerRevision: 1}, audit.IdentityBefore), "改前的原身份只在留痕里")

	_, err = ResolveManual(ctx, deps, req, e.now(), true)
	require.ErrorIs(t, err, ErrManualVersionChanged, "同一条命令重跑只生效一次")

	// 引擎接手:原归属身份核对按新的一方通过;卖家(现在是 132)撤单,托管退回 132。
	tick(t, e)
	_, err = e.Mutate(ctx, 132, offer.CommerceId, "cancel")
	require.NoError(t, err)
	tick(t, e)
	require.Equal(t, 1, opCount(t, e, 132, 4))
	require.Zero(t, opCount(t, e, 131, 4), "资产不发给角色的新主")
}

// 两个操作人拿同一个版本同时改绑:只有一个生效,另一个回版本已变,不会加两遍腿。
func TestRealDBConcurrentRedirectsApplyOnce(t *testing.T) {
	e := realDBEngine(t)
	ctx := context.Background()
	roles := fixtureRoles{Values: map[uint64]*dspb.AccountRoleOwnership{}}
	e.Roles = roles
	offer := consignment(t, e, 133, "owner-change-02")
	tick(t, e)
	settleLegs(t, e, 2)
	tick(t, e)
	roles.Values[133] = &dspb.AccountRoleOwnership{Account: "account-new-owner", OwnerRevision: 2}
	roles.Values[134] = &dspb.AccountRoleOwnership{Account: "account-133", OwnerRevision: 3}
	require.NoError(t, e.Repo.Wake(ctx, offer.CommerceId, e.now()))
	tick(t, e)
	r, _ := state(t, e, offer.CommerceId)
	require.Equal(t, Manual, r.Status)

	req := ManualRequest{CommerceID: offer.CommerceId, ExpectVersion: r.Version, Action: ManualRedirect, Party: partySeller, To: 134, Operator: "ops", Reason: "ticket-3"}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = ResolveManual(ctx, manualDeps(e), req, e.now(), true)
		}(i)
	}
	wg.Wait()
	succeeded := 0
	for _, err := range errs {
		if err == nil {
			succeeded++
			continue
		}
		require.ErrorIs(t, err, ErrManualVersionChanged)
	}
	require.Equal(t, 1, succeeded)
	after, w := state(t, e, offer.CommerceId)
	require.Equal(t, r.Version+1, after.Version)
	require.Len(t, w.ManualActions, 1)
}

// ── 第 2 类:部分发放 ────────────────────────────────────────────

// 双方交易交付时买家收货腿部分到账(物品到了、货币那条失败):按缺口补发,只补货币;重跑落空;
// 两个操作人并发只生效一次;卖家所得一直压到买家那份补齐(先买家、后卖家,delivery_order.go);
// 补发到账后才发卖家所得,它到账后合同结案,原腿与补发腿各一条。
func TestRealDBPartialCreditReissuesTheShortfall(t *testing.T) {
	e := realDBEngine(t)
	ctx := context.Background()
	offer, err := e.CreatePlayerTrade(ctx, 141, &tradepb.CreatePlayerTradeRequest{ClientRequestId: "partial-trade-01", BuyerPlayerId: 142,
		SellerAssets: &tradepb.TradeAssetSelection{Items: map[uint32]uint32{10: 2}, CurrencyType: 1, CurrencyAmount: 30},
		BuyerAssets:  &tradepb.TradeAssetSelection{CurrencyType: 1, CurrencyAmount: 70}})
	require.NoError(t, err)
	tick(t, e)
	settleLegs(t, e, 2)
	tick(t, e)
	_, err = e.Mutate(ctx, 142, offer.CommerceId, "confirm")
	require.NoError(t, err)
	tick(t, e)
	settleLegs(t, e, 2)
	tick(t, e)
	_, err = e.Mutate(ctx, 141, offer.CommerceId, "confirm")
	require.NoError(t, err)
	tick(t, e)
	_, w := state(t, e, offer.CommerceId)
	require.Equal(t, "deliver", w.Stage)
	receive := leg(w, "buyer_receive")
	require.Zero(t, leg(w, "seller_receive").OpId, "卖家所得等买家那条到账才发")
	setOpStatus(t, e, receive.OpId, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED_PARTIAL)
	tick(t, e)
	r, w := state(t, e, offer.CommerceId)
	require.Equal(t, Manual, r.Status)
	require.Equal(t, manualReasonAppliedPartial, w.Quarantine.GetReason())

	part, err := ParsePartialPart("buyer_receive=currency:1")
	require.NoError(t, err)
	req := ManualRequest{CommerceID: offer.CommerceId, ExpectVersion: r.Version, Action: ManualPartial, Parts: []PartialPart{part},
		Operator: "ops", Reason: "ticket-4: [AssetOp] partial failed=currency_type=1"}
	_, err = ResolveManual(ctx, manualDeps(e), ManualRequest{CommerceID: offer.CommerceId, ExpectVersion: r.Version, Action: ManualReissue, Operator: "ops", Reason: "t"}, e.now(), true)
	require.ErrorIs(t, err, ErrManualNotAllowed, "部分发放不能按全量补发")

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = ResolveManual(ctx, manualDeps(e), req, e.now(), true)
		}(i)
	}
	wg.Wait()
	succeeded := 0
	for _, err := range errs {
		if err == nil {
			succeeded++
			continue
		}
		require.ErrorIs(t, err, ErrManualVersionChanged)
	}
	require.Equal(t, 1, succeeded, "两个操作人并发只生效一次")
	_, err = ResolveManual(ctx, manualDeps(e), req, e.now(), true)
	require.ErrorIs(t, err, ErrManualVersionChanged, "重跑落空")

	_, w = state(t, e, offer.CommerceId)
	require.Equal(t, "buyer_receive#2", leg(w, "buyer_receive").SupersededBy)
	require.Len(t, w.ManualActions, 1)
	require.Len(t, w.ManualActions[0].PartialParts, 1)
	tick(t, e)
	_, w = state(t, e, offer.CommerceId)
	shortfall := leg(w, "buyer_receive#2")
	require.NotZero(t, shortfall.OpId, "引擎给缺口补发腿发号")
	sent := opPayload(t, e, shortfall.OpId)
	require.Empty(t, sent.Items, "已到账的物品不再发")
	require.True(t, proto.Equal(&assetpb.CurrencyAmount{CurrencyType: 1, Amount: 30}, sent.Currencies[0]))
	require.Zero(t, leg(w, "seller_receive").OpId, "买家那份还没补齐,卖家所得仍压着")
	setOpStatus(t, e, shortfall.OpId, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED)
	tick(t, e)
	r, w = state(t, e, offer.CommerceId)
	require.Equal(t, Delivering, r.Status)
	proceeds := leg(w, "seller_receive")
	require.NotZero(t, proceeds.OpId, "买家那份补齐,放行卖家所得")
	setOpStatus(t, e, proceeds.OpId, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED)
	tick(t, e)
	r, _ = state(t, e, offer.CommerceId)
	require.Equal(t, Settled, r.Status)
	require.Equal(t, 2, opCount(t, e, 142, 4), "原腿与缺口补发各一条")
	require.Equal(t, 1, opCount(t, e, 141, 4), "卖家所得只发一次")
}

// ── 第 3 类:竞价与拍品互相引用 ─────────────────────────────────

// openLotWithBid 建一件拍品、出一次价,推进到出价被接受(bid_held、拍品引用它)。
func openLotWithBid(t *testing.T, e *Engine, seller, bidder uint64, tag string) (uint64, uint64) {
	t.Helper()
	ctx := context.Background()
	lot, err := e.CreateLot(ctx, seller, &tradepb.CreateAuctionLotRequest{ClientRequestId: "lot-" + tag, Assets: &tradepb.TradeAssetSelection{Items: map[uint32]uint32{10: 1}},
		CurrencyType: 1, OpeningPrice: 50, MinimumIncrement: 5, DurationMs: 3_600_000})
	require.NoError(t, err)
	tick(t, e)
	settleLegs(t, e, 2)
	tick(t, e)
	bid, err := e.PlaceBid(ctx, bidder, lot.CommerceId, &tradepb.PlaceAuctionBidRequest{LotId: fmt.Sprint(lot.CommerceId), ClientRequestId: "bid-" + tag, Amount: 80})
	require.NoError(t, err)
	tick(t, e)
	settleLegs(t, e, 2)
	tick(t, e)
	tick(t, e)
	_, bw := state(t, e, bid.CommerceId)
	require.Equal(t, "bid_held", bw.Stage)
	_, lw := state(t, e, lot.CommerceId)
	require.Equal(t, bid.CommerceId, lw.WinningBidId)
	return lot.CommerceId, bid.CommerceId
}

// 拍品转人工、它引用的出价还在 bid_held:作废拍卖在同一事务里改两张合同。中间引擎推迟持有出价(Defer 不改版本)
// 不影响操作人看过的版本;重跑落空;之后引擎把拍品托管退回卖家、把冻结款退回出价人,两张合同都按取消结案。
func TestRealDBVoidAuctionRefundsLotAndBidTogether(t *testing.T) {
	e := realDBEngine(t)
	ctx := context.Background()
	lotID, bidID := openLotWithBid(t, e, 151, 152, "void-01")
	injectCorrupt(t, e, lotID)
	lr, _ := state(t, e, lotID)
	br, _ := state(t, e, bidID)

	// 与引擎并发:持有出价等拍品结算时只推迟、不改版本,操作人看过的出价版本仍然有效。
	require.NoError(t, e.settleHeldBid(ctx, br))
	after, _ := state(t, e, bidID)
	require.Equal(t, br.Version, after.Version)

	req := ManualRequest{CommerceID: lotID, ExpectVersion: lr.Version, Action: ManualVoidAuction, LinkedID: bidID, LinkedVersion: br.Version,
		Operator: "ops", Reason: "ticket-5: lot escrow unusable, refund both sides"}
	stale := req
	stale.LinkedVersion = br.Version + 1
	_, err := ResolveManual(ctx, manualDeps(e), stale, e.now(), true)
	require.ErrorIs(t, err, ErrManualVersionChanged, "出价版本对不上不改任何一张")

	result, err := ResolveManual(ctx, manualDeps(e), req, e.now(), true)
	require.NoError(t, err)
	require.Equal(t, bidID, result.LinkedID)
	lr2, lw2 := state(t, e, lotID)
	br2, bw2 := state(t, e, bidID)
	require.Equal(t, CancelPending, lr2.Status)
	require.Equal(t, "return", lw2.Stage)
	require.Equal(t, CancelPending, br2.Status)
	require.Equal(t, "bid_refund", bw2.Stage)
	require.Equal(t, bidID, lw2.ManualActions[0].LinkedCommerceId)
	require.Equal(t, lotID, bw2.ManualActions[0].LinkedCommerceId, "两张合同的留痕互相指向")

	_, err = ResolveManual(ctx, manualDeps(e), req, e.now(), true)
	require.ErrorIs(t, err, ErrManualVersionChanged)

	tick(t, e)
	settleLegs(t, e, 2)
	tick(t, e)
	tick(t, e)
	lr3, _ := state(t, e, lotID)
	br3, _ := state(t, e, bidID)
	require.Equal(t, Cancelled, lr3.Status)
	require.Equal(t, Cancelled, br3.Status)
	require.Equal(t, 1, opCount(t, e, 151, 4), "拍品托管退回卖家一次")
	require.Equal(t, 1, opCount(t, e, 152, 4), "冻结款退回出价人一次")
}

// 出价能不能单独退回只看拍品是否引用它:被引用的拒绝(冻结款归拍品结算);接受之前转人工的可以退,只退一次。
func TestRealDBBidRefundOnlyWhenTheLotDoesNotReferenceIt(t *testing.T) {
	e := realDBEngine(t)
	ctx := context.Background()
	_, heldID := openLotWithBid(t, e, 153, 154, "void-02")
	injectCorrupt(t, e, heldID)
	held, _ := state(t, e, heldID)
	_, err := ResolveManual(ctx, manualDeps(e), ManualRequest{CommerceID: heldID, ExpectVersion: held.Version, Action: ManualRefund, Operator: "ops", Reason: "t"}, e.now(), true)
	require.ErrorIs(t, err, ErrManualNotAllowed, "拍品引用这条出价")

	lot, err := e.CreateLot(ctx, 155, &tradepb.CreateAuctionLotRequest{ClientRequestId: "lot-void-03", Assets: &tradepb.TradeAssetSelection{Items: map[uint32]uint32{10: 1}},
		CurrencyType: 1, OpeningPrice: 50, MinimumIncrement: 5, DurationMs: 3_600_000})
	require.NoError(t, err)
	tick(t, e)
	settleLegs(t, e, 2)
	tick(t, e)
	bid, err := e.PlaceBid(ctx, 156, lot.CommerceId, &tradepb.PlaceAuctionBidRequest{LotId: fmt.Sprint(lot.CommerceId), ClientRequestId: "bid-void-03", Amount: 60})
	require.NoError(t, err)
	tick(t, e)
	settleLegs(t, e, 2)
	tick(t, e)
	_, bw := state(t, e, bid.CommerceId)
	require.Equal(t, "bid_ready", bw.Stage, "冻结完成、还没被拍品接受")
	injectCorrupt(t, e, bid.CommerceId)
	br, _ := state(t, e, bid.CommerceId)

	result, err := ResolveManual(ctx, manualDeps(e), ManualRequest{CommerceID: bid.CommerceId, ExpectVersion: br.Version, Action: ManualRefund, Operator: "ops", Reason: "ticket-6"}, e.now(), true)
	require.NoError(t, err)
	require.Equal(t, []string{"bid_debit/refund"}, result.NewLegs)
	tick(t, e)
	_, lw := state(t, e, lot.CommerceId)
	require.Zero(t, lw.WinningBidId, "转人工的出价不会再被拍品接受")
	settleLegs(t, e, 2)
	tick(t, e)
	br, _ = state(t, e, bid.CommerceId)
	require.Equal(t, Cancelled, br.Status)
	require.Equal(t, 1, opCount(t, e, 156, 4), "只退一次")
}

// ── 第 4 类:已部分成交的市场单 ──────────────────────────────────

// openMarketOrder 下一张材料订单并推进到托管完成、进盘口。
func openMarketOrder(t *testing.T, e *Engine, player uint64, side tradepb.MarketOrderSide, price uint64, qty uint32) uint64 {
	t.Helper()
	ctx := context.Background()
	r, err := e.PlaceMarket(ctx, player, &tradepb.PlaceMarketOrderRequest{ClientRequestId: fmt.Sprintf("manual-market-%d", player), MarketId: 1, ItemConfigId: 10, Side: side, UnitPrice: price, Quantity: qty, CurrencyType: 1})
	require.NoError(t, err)
	require.NoError(t, e.advance(ctx, r))
	settleLegs(t, e, 2)
	r, _ = state(t, e, r.CommerceId)
	require.NoError(t, e.advance(ctx, r))
	return r.CommerceId
}

func fillOf(t *testing.T, e *Engine, buy uint64) uint64 {
	t.Helper()
	var id uint64
	require.NoError(t, e.Repo.DB.QueryRow("SELECT commerce_id FROM trade_commerce WHERE parent_id=? AND kind=4 AND side=0", buy).Scan(&id))
	return id
}

// 已部分成交的卖单转人工:整单退回只退剩余 3 件,成交的 2 件归成交子单照常交付;子单推进不碰母单行,
// 操作人看过的版本仍然有效;重跑落空;子单结案时唤醒两张母单(卖单是 counterparty),卖单随之按取消结案。
func TestRealDBPartiallyFilledMarketOrderRefundsOnlyTheRemainder(t *testing.T) {
	e := realDBEngine(t)
	ctx := context.Background()
	sell := openMarketOrder(t, e, 161, 2, 10, 5)
	buy := openMarketOrder(t, e, 162, 1, 10, 2)
	require.NoError(t, e.matchMarket(ctx, buy))
	sr, _ := state(t, e, sell)
	require.EqualValues(t, 2, sr.FilledQuantity)
	child := fillOf(t, e, buy)
	injectCorrupt(t, e, sell)
	sr, _ = state(t, e, sell)

	// 与引擎并发:子单推进不碰母单行(唤醒也只动推进中的行),转人工的母单版本不变。
	cr, _ := state(t, e, child)
	require.NoError(t, e.advance(ctx, cr))
	after, _ := state(t, e, sell)
	require.Equal(t, sr.Version, after.Version)

	req := ManualRequest{CommerceID: sell, ExpectVersion: sr.Version, Action: ManualRefund, Operator: "ops", Reason: "ticket-7"}
	result, err := ResolveManual(ctx, manualDeps(e), req, e.now(), true)
	require.NoError(t, err)
	require.Equal(t, []string{"seller_debit/refund"}, result.NewLegs)
	_, sw := state(t, e, sell)
	require.True(t, proto.Equal(&assetpb.AssetBundle{Items: []*assetpb.ItemGrant{{ConfigId: 10, Count: 3}}}, leg(sw, "seller_debit/refund").Bundle), "只退剩余 3 件")
	_, err = ResolveManual(ctx, manualDeps(e), req, e.now(), true)
	require.ErrorIs(t, err, ErrManualVersionChanged)

	tick(t, e)
	settleLegs(t, e, 2)
	tick(t, e)
	// 先买家、后卖家(delivery_order.go):子单的买家那条到账之后才发卖家货款,再落一轮终局。
	settleLegs(t, e, 2)
	for i := 0; i < 3; i++ {
		tick(t, e)
	}
	cr, _ = state(t, e, child)
	require.Equal(t, Settled, cr.Status, "成交子单照常交付")
	sr, _ = state(t, e, sell)
	require.Equal(t, Cancelled, sr.Status)
	require.Equal(t, 2, opCount(t, e, 161, 4), "卖家:成交货款一条 + 剩余退回一条")
	require.Equal(t, 1, opCount(t, e, 162, 4), "买家:成交物品一条")
}

// 成交子单转人工不再连带母单(2026-11-11):母单剩余挂单量照常可撤,撤单之后停在 market_wait 等子单;
// 子单人工恢复、引擎把它推进到结案的那一步唤醒母单,母单随之按取消结案。
func TestRealDBManualFillDoesNotQuarantineTheParent(t *testing.T) {
	e := realDBEngine(t)
	ctx := context.Background()
	sell := openMarketOrder(t, e, 171, 2, 10, 5)
	buy := openMarketOrder(t, e, 172, 1, 10, 2)
	require.NoError(t, e.matchMarket(ctx, buy))
	child := fillOf(t, e, buy)
	cr, _ := state(t, e, child)
	require.NoError(t, e.advance(ctx, cr))
	injectCorrupt(t, e, child)

	sr, _ := state(t, e, sell)
	require.NoError(t, e.advance(ctx, sr))
	sr, sw := state(t, e, sell)
	require.NotEqual(t, Manual, sr.Status, "子单转人工不连带母单")
	require.Equal(t, "market_open", sw.Stage)

	_, err := e.Mutate(ctx, 171, sell, "cancel")
	require.NoError(t, err, "剩余挂单量照常可撤")
	sr, _ = state(t, e, sell)
	require.NoError(t, e.advance(ctx, sr))
	settleLegs(t, e, 2)
	sr, _ = state(t, e, sell)
	require.NoError(t, e.advance(ctx, sr))
	sr, sw = state(t, e, sell)
	require.Equal(t, "market_wait", sw.Stage, "母单自己的托管已退完,等转人工的子单")
	require.Equal(t, CancelPending, sr.Status)

	cr, _ = state(t, e, child)
	_, err = ResolveManual(ctx, manualDeps(e), ManualRequest{CommerceID: child, ExpectVersion: cr.Version, Action: ManualResume, Operator: "ops", Reason: "ticket-8"}, e.now(), true)
	require.NoError(t, err)
	tick(t, e)
	// 先买家、后卖家(delivery_order.go):买家那条在上面已落终局,恢复后这一步才发卖家货款,再落一轮终局子单才结案。
	settleLegs(t, e, 2)
	tick(t, e)
	tick(t, e)
	cr, _ = state(t, e, child)
	require.Equal(t, Settled, cr.Status)
	sr, _ = state(t, e, sell)
	require.Equal(t, Cancelled, sr.Status, "子单结案唤醒母单")
}
