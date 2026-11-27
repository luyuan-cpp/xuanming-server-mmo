package commerce

// 悬赏求书求购单(kind 7,wanted_skill_book.go)的状态机、快照核对、税与退回金额。纯函数用例,不读库:
// 揭榜走 fulfillWantedLocked(角色归属用 fixtureRoles,不碰事务),推进走 plan,腿的结局直接写在腿上。

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	assetpb "proto/common/asset"
	componentpb "proto/common/component"
	rollbackpb "proto/common/rollback"
	tradepb "proto/trade"
	"shared/currencytype"
)

const (
	wantedPoster    = uint64(2)
	wantedFulfiller = uint64(1)
	wantedSkillID   = uint32(501)
	wantedReward    = uint64(1000)
	wantedBookUUID  = uint64(9001)
	wantedRequest   = "fulfill-request-0001"
)

func goldBundle(amount uint64) *assetpb.AssetBundle {
	return &assetpb.AssetBundle{Currencies: []*assetpb.CurrencyAmount{{CurrencyType: currencytype.Gold, Amount: amount}}}
}

// sealedBook 是 scene 扣出时交回的一枚封卷道籍实例;edit 改出各种不符的情形。
func sealedBook(t *testing.T, edit func(*componentpb.ItemComp)) *assetpb.AssetItemInstance {
	t.Helper()
	item := &componentpb.ItemComp{ItemId: wantedBookUUID, ConfigId: SealedSkillBookConfigID, Size: 1,
		SealedSkillBook: &componentpb.SealedSkillBookData{SkillTableId: wantedSkillID, ScrollId: 41, CrafterName: "宗师甲", CrafterPlayerId: 77}}
	if edit != nil {
		edit(item)
	}
	raw, err := proto.Marshal(item)
	require.NoError(t, err)
	return &assetpb.AssetItemInstance{ItemUuid: wantedBookUUID, ConfigId: item.GetConfigId(), StackSize: item.GetSize(), InstanceProto: raw}
}

// openWantedOrder 是一张托管已成、挂榜中的求购单。
func openWantedOrder(signature bool) (*tradepb.TradeCommerceRecord, *tradepb.TradeCommerceWorkflow) {
	r := &tradepb.TradeCommerceRecord{CommerceId: 41, Kind: KindWantedSkillBook, Status: Waiting, BuyerPlayerId: wantedPoster,
		UnitPrice: wantedReward, ItemConfigId: SealedSkillBookConfigID, Version: 3}
	w := &tradepb.TradeCommerceWorkflow{Stage: stageWantedOpen, BuyerIdentity: identity("account-2", 1),
		WantedSkillBook: &tradepb.TradeWantedSkillBookTerms{SkillTableId: wantedSkillID, ScrollId: 41, RequireMasterSignature: signature,
			RewardGold: wantedReward, SellerNetGold: WantedSellerNetGold(wantedReward)},
		Legs: []*tradepb.TradeCommerceLeg{manualLeg("buyer_debit", wantedPoster, 3, 100, outcomeApplied, goldBundle(wantedReward))}}
	return r, w
}

// fulfill 让 me 用 requestID 揭榜,回 fulfillWantedLocked 的结论。
func fulfill(t *testing.T, r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow, me uint64, requestID string, sealedItemID uint64) (bool, error) {
	t.Helper()
	return (&Engine{Roles: fixtureRoles{}}).fulfillWantedLocked(context.Background(), nil, me, r, w, requestID, sealedItemID)
}

// landDebit 给当前这次揭榜的托管腿写上结局与 scene 交回的快照(advance 在锁内读 outbox 行之后做的事)。
func landDebit(w *tradepb.TradeCommerceWorkflow, outcome uint32, book *assetpb.AssetItemInstance) *tradepb.TradeCommerceLeg {
	debit := wantedAttemptDebit(w, w.QuantityReserved)
	debit.OpId, debit.Outcome = 200+w.QuantityReserved, outcome
	if book != nil {
		debit.Snapshot = &assetpb.AssetSnapshot{Items: []*assetpb.AssetItemInstance{book}}
	}
	return debit
}

func plan(t *testing.T, r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow, now uint64) {
	t.Helper()
	require.NoError(t, (&Engine{}).plan(r, w, now))
}

// 税与 C++ ProxyTaxGold / ProxyNetGold 同一个公式:悬赏额 × 5% 向上取整。
func TestWantedTaxRoundsUp(t *testing.T) {
	for _, tc := range []struct{ reward, tax, net uint64 }{
		{0, 0, 0}, {100, 5, 95}, {101, 6, 95}, {119, 6, 113}, {1000, 50, 950}, {50000, 2500, 47500},
	} {
		require.Equal(t, tc.tax, WantedTaxGold(tc.reward), "reward=%d", tc.reward)
		require.Equal(t, tc.net, WantedSellerNetGold(tc.reward), "reward=%d", tc.reward)
	}
}

// 发榜之后先托管悬赏银两(只扣流通银两):扣到挂榜,没扣到(银两不足)按撤单结案,在途时原地等。
func TestWantedEscrowOpensOrCancels(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome uint32
		stage   string
		status  uint32
	}{
		{"在途", 0, stageWantedEscrow, Escrowing},
		{"扣到挂榜", outcomeApplied, stageWantedOpen, Waiting},
		{"银两不足结案", outcomeRejected, "done", Cancelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &tradepb.TradeCommerceRecord{CommerceId: 42, Kind: KindWantedSkillBook, Status: Escrowing, BuyerPlayerId: wantedPoster}
			w := &tradepb.TradeCommerceWorkflow{Stage: stageWantedEscrow, WantedSkillBook: &tradepb.TradeWantedSkillBookTerms{SkillTableId: wantedSkillID, RewardGold: wantedReward, SellerNetGold: 950}}
			plan(t, r, w, 1000)
			escrow := leg(w, "buyer_debit")
			require.NotNil(t, escrow)
			require.Equal(t, wantedPoster, escrow.PlayerId)
			require.Equal(t, uint32(3), escrow.Stream)
			require.True(t, proto.Equal(goldBundle(wantedReward), escrow.Bundle), "托管的是流通银两的全额悬赏")
			if tc.outcome != 0 {
				escrow.OpId, escrow.Outcome = 100, tc.outcome
				plan(t, r, w, 2000)
			}
			require.Equal(t, tc.stage, w.Stage)
			require.Equal(t, tc.status, r.Status)
		})
	}
}

// 相符的书:先交书给发榜人,到账之后才发税后银两给揭榜人;两条都到账按成交结案,5% 税不发给任何人。
func TestWantedMatchedBookDeliversBookFirstThenNetGold(t *testing.T) {
	r, w := openWantedOrder(true)
	replayed, err := fulfill(t, r, w, wantedFulfiller, wantedRequest, wantedBookUUID)
	require.NoError(t, err)
	require.False(t, replayed)
	require.Equal(t, stageWantedFill, w.Stage)
	require.Equal(t, Escrowing, r.Status)
	require.Equal(t, wantedFulfiller, r.SellerPlayerId)
	require.Equal(t, "account-1", w.SellerIdentity.GetAccount())

	debit := wantedAttemptDebit(w, 1)
	require.NotNil(t, debit)
	require.Equal(t, []uint64{wantedBookUUID}, debit.Bundle.GetItemUuids())
	require.Equal(t, uint32(rollbackpb.TransactionType_TX_AUCTION_SELL), legTxType(debit))

	plan(t, r, w, 1000)
	require.Equal(t, stageWantedFill, w.Stage, "书还在扣,等着")

	book := sealedBook(t, nil)
	landDebit(w, outcomeApplied, book)
	plan(t, r, w, 2000)
	require.Equal(t, "deliver", w.Stage)
	require.Equal(t, Delivering, r.Status)
	toPoster := leg(w, "buyer_receive")
	require.NotNil(t, toPoster)
	require.Equal(t, wantedPoster, toPoster.PlayerId)
	require.Equal(t, wantedFulfiller, toPoster.EscrowOwnerId)
	require.Len(t, toPoster.Bundle.GetItemInstances(), 1)
	require.True(t, proto.Equal(book, toPoster.Bundle.GetItemInstances()[0]), "交给发榜人的是 scene 交回的原件")
	toFulfiller := leg(w, "seller_receive")
	require.NotNil(t, toFulfiller)
	require.Equal(t, wantedFulfiller, toFulfiller.PlayerId)
	require.Equal(t, wantedPoster, toFulfiller.EscrowOwnerId)
	require.True(t, proto.Equal(goldBundle(950), toFulfiller.Bundle), "揭榜人拿税后 95%")
	require.Equal(t, uint32(rollbackpb.TransactionType_TX_AUCTION_BUY), legTxType(toPoster))
	require.Equal(t, uint32(rollbackpb.TransactionType_TX_AUCTION_BUY), legTxType(toFulfiller))
	require.True(t, legReadyToSend(w, toPoster))
	require.False(t, legReadyToSend(w, toFulfiller), "书没到发榜人手里之前不付钱")

	toPoster.OpId, toPoster.Outcome = 301, outcomeApplied
	require.True(t, legReadyToSend(w, toFulfiller))
	plan(t, r, w, 3000)
	require.Equal(t, "deliver", w.Stage)
	toFulfiller.OpId, toFulfiller.Outcome = 302, outcomeApplied
	plan(t, r, w, 4000)
	require.Equal(t, "done", w.Stage)
	require.Equal(t, Settled, r.Status)
	require.Equal(t, tradepb.WantedSkillBookOrderPhase_WANTED_SKILL_BOOK_ORDER_PHASE_SETTLED, WantedPhase(r, w))
}

// 不符的书原样退给揭榜人(退回腿,不受获取封禁约束),退书到账之后揭榜人清零、单子回到挂榜,别人可以再揭。
func TestWantedMismatchedBookReturnsAndReopens(t *testing.T) {
	for _, tc := range []struct {
		name      string
		signature bool
		edit      func(*componentpb.ItemComp)
	}{
		{"技能不对", false, func(i *componentpb.ItemComp) { i.SealedSkillBook.SkillTableId = wantedSkillID + 1 }},
		{"已绑定", false, func(i *componentpb.ItemComp) { i.Bound = true }},
		{"稀世书", false, func(i *componentpb.ItemComp) { i.SealedSkillBook.RarityTier = 1 }},
		{"要署名却没有署名", true, func(i *componentpb.ItemComp) { i.SealedSkillBook.CrafterName = "" }},
		{"要署名却没有宗师号", true, func(i *componentpb.ItemComp) { i.SealedSkillBook.CrafterPlayerId = 0 }},
		{"不是封卷道籍", false, func(i *componentpb.ItemComp) { i.ConfigId = 3415 }},
		{"没有封卷数据", false, func(i *componentpb.ItemComp) { i.SealedSkillBook = nil }},
		{"堆叠数不是 1", false, func(i *componentpb.ItemComp) { i.Size = 2 }},
		{"实例身份与扣出的不一致", false, func(i *componentpb.ItemComp) { i.ItemId = wantedBookUUID + 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, w := openWantedOrder(tc.signature)
			_, err := fulfill(t, r, w, wantedFulfiller, wantedRequest, wantedBookUUID)
			require.NoError(t, err)
			book := sealedBook(t, tc.edit)
			landDebit(w, outcomeApplied, book)
			plan(t, r, w, 1000)

			require.Equal(t, stageWantedFill, w.Stage, "退书期间仍是交易中")
			require.Equal(t, wantedFulfiller, r.SellerPlayerId, "退书腿的角色锁与 seq 行按当时的揭榜人取")
			require.Nil(t, leg(w, "buyer_receive"), "不符的书不交给发榜人")
			back := leg(w, wantedReturnRole(1))
			require.NotNil(t, back)
			require.Equal(t, wantedFulfiller, back.PlayerId)
			require.Equal(t, wantedFulfiller, back.EscrowOwnerId)
			require.Equal(t, uint32(4), back.Stream)
			require.True(t, proto.Equal(book, back.Bundle.GetItemInstances()[0]), "原样退回")
			require.Equal(t, uint32(rollbackpb.TransactionType_TX_AUCTION_RETURN), legTxType(back))

			back.OpId, back.Outcome = 301, outcomeApplied
			plan(t, r, w, 2000)
			require.Equal(t, stageWantedOpen, w.Stage)
			require.Equal(t, Waiting, r.Status)
			require.Zero(t, r.SellerPlayerId)
			require.Nil(t, w.SellerIdentity)

			// 同一揭榜人按同一请求号重发:查到这一次已因书不符作废。
			replayed, err := fulfill(t, r, w, wantedFulfiller, wantedRequest, wantedBookUUID)
			require.True(t, replayed)
			require.ErrorIs(t, err, ErrWantedBookMismatch)

			// 别人可以接着揭,揭榜序号往后走,上一次的腿留作证据。
			_, err = fulfill(t, r, w, 3, "fulfill-request-0002", 9002)
			require.NoError(t, err)
			require.Equal(t, uint64(2), w.QuantityReserved)
			require.NotNil(t, wantedAttemptDebit(w, 2))
		})
	}
}

// 书没扣到(已不在背包、已绑定、被战斗预留):这次揭榜作废,直接回挂榜,不建任何发放腿;重发同一请求号回「不可交易」。
func TestWantedDebitRejectedReopensWithoutLegs(t *testing.T) {
	r, w := openWantedOrder(false)
	_, err := fulfill(t, r, w, wantedFulfiller, wantedRequest, wantedBookUUID)
	require.NoError(t, err)
	landDebit(w, outcomeRejected, nil)
	plan(t, r, w, 1000)
	require.Equal(t, stageWantedOpen, w.Stage)
	require.Equal(t, Waiting, r.Status)
	require.Zero(t, r.SellerPlayerId)
	for _, l := range w.Legs {
		require.NotEqual(t, uint32(4), l.Stream, "没扣到不发任何东西: %s", l.Role)
	}
	replayed, err := fulfill(t, r, w, wantedFulfiller, wantedRequest, wantedBookUUID)
	require.True(t, replayed)
	require.ErrorIs(t, err, ErrNotTradable)
}

// 退书腿终局没到账:书留在托管里,转人工;不回挂榜,免得揭榜人被清零之后这本书没人认领。
func TestWantedMismatchReturnNotLandedGoesManual(t *testing.T) {
	r, w := openWantedOrder(false)
	_, err := fulfill(t, r, w, wantedFulfiller, wantedRequest, wantedBookUUID)
	require.NoError(t, err)
	landDebit(w, outcomeApplied, sealedBook(t, func(i *componentpb.ItemComp) { i.Bound = true }))
	plan(t, r, w, 1000)
	back := leg(w, wantedReturnRole(1))
	back.OpId, back.Outcome = 301, outcomeRejected
	plan(t, r, w, 2000)
	require.Equal(t, Manual, r.Status)
	require.Equal(t, stageWantedFill, w.Quarantine.GetStage())
	require.Equal(t, manualReasonFinalCredit, w.Quarantine.GetReason())
	require.Equal(t, wantedFulfiller, r.SellerPlayerId)
}

// 揭榜的入口约束:禁自揭;同一张单同时只允许一次揭榜在途;撤单中、揭榜次数用满不能揭;同一请求号重放查同一次揭榜,换了书算请求冲突。
func TestWantedFulfillGuards(t *testing.T) {
	t.Run("禁自揭", func(t *testing.T) {
		r, w := openWantedOrder(false)
		_, err := fulfill(t, r, w, wantedPoster, wantedRequest, wantedBookUUID)
		require.ErrorIs(t, err, ErrSelf)
	})
	t.Run("别人正在交书", func(t *testing.T) {
		r, w := openWantedOrder(false)
		_, err := fulfill(t, r, w, wantedFulfiller, wantedRequest, wantedBookUUID)
		require.NoError(t, err)
		_, err = fulfill(t, r, w, 3, "fulfill-request-0003", 9003)
		require.ErrorIs(t, err, ErrConflict)
	})
	t.Run("撤单中", func(t *testing.T) {
		r, w := openWantedOrder(false)
		w.CancelRequested, r.Status = true, CancelPending
		_, err := fulfill(t, r, w, wantedFulfiller, wantedRequest, wantedBookUUID)
		require.ErrorIs(t, err, ErrConflict)
	})
	t.Run("揭榜次数用满", func(t *testing.T) {
		r, w := openWantedOrder(false)
		w.QuantityReserved = maxWantedFulfillAttempts
		_, err := fulfill(t, r, w, wantedFulfiller, wantedRequest, wantedBookUUID)
		require.ErrorIs(t, err, ErrConflict)
	})
	t.Run("不是求购单", func(t *testing.T) {
		r, w := openWantedOrder(false)
		r.Kind = 3
		_, err := fulfill(t, r, w, wantedFulfiller, wantedRequest, wantedBookUUID)
		require.ErrorIs(t, err, ErrConflict)
	})
	t.Run("重放", func(t *testing.T) {
		r, w := openWantedOrder(false)
		_, err := fulfill(t, r, w, wantedFulfiller, wantedRequest, wantedBookUUID)
		require.NoError(t, err)
		before := proto.Clone(w).(*tradepb.TradeCommerceWorkflow)
		replayed, err := fulfill(t, r, w, wantedFulfiller, wantedRequest, wantedBookUUID)
		require.NoError(t, err, "当前这一次揭榜在途,重放回合同")
		require.True(t, replayed)
		require.True(t, proto.Equal(before, w), "重放不改合同")
		replayed, err = fulfill(t, r, w, wantedFulfiller, wantedRequest, wantedBookUUID+5)
		require.True(t, replayed)
		require.ErrorIs(t, err, ErrReplay, "同一请求号换了书")
	})
}

// 挂榜中撤单:托管的悬赏原样全额退回发榜人(退回腿);托管阶段撤单等托管腿有结局再收口。
func TestWantedCancelReturnsTheFullEscrow(t *testing.T) {
	r, w := openWantedOrder(false)
	w.CancelRequested, r.Status = true, CancelPending
	require.Equal(t, tradepb.WantedSkillBookOrderPhase_WANTED_SKILL_BOOK_ORDER_PHASE_CANCELLING, WantedPhase(r, w))
	plan(t, r, w, 1000)
	require.Equal(t, "return", w.Stage)
	require.Equal(t, CancelPending, r.Status)
	back := leg(w, "buyer_return")
	require.NotNil(t, back)
	require.Equal(t, wantedPoster, back.PlayerId)
	require.True(t, proto.Equal(goldBundle(wantedReward), back.Bundle), "撤单退全额")
	require.Equal(t, uint32(rollbackpb.TransactionType_TX_AUCTION_RETURN), legTxType(back))
	back.OpId, back.Outcome = 301, outcomeApplied
	plan(t, r, w, 2000)
	require.Equal(t, "done", w.Stage)
	require.Equal(t, Cancelled, r.Status)
}

// 托管阶段就撤单:托管腿被截止(没扣到)按撤单结案;截止输给了已落地的扣款,照常挂榜后立刻退回。
func TestWantedCancelDuringEscrow(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome uint32
		stage   string
		status  uint32
	}{
		{"没扣到", uint32(tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_ABORTED), "done", Cancelled},
		{"已扣到", outcomeApplied, "return", CancelPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &tradepb.TradeCommerceRecord{CommerceId: 43, Kind: KindWantedSkillBook, Status: CancelPending, BuyerPlayerId: wantedPoster}
			w := &tradepb.TradeCommerceWorkflow{Stage: stageWantedEscrow, CancelRequested: true,
				WantedSkillBook: &tradepb.TradeWantedSkillBookTerms{SkillTableId: wantedSkillID, RewardGold: wantedReward, SellerNetGold: 950},
				Legs:            []*tradepb.TradeCommerceLeg{manualLeg("buyer_debit", wantedPoster, 3, 100, tc.outcome, goldBundle(wantedReward))}}
			plan(t, r, w, 1000)
			require.Equal(t, tc.stage, w.Stage)
			require.Equal(t, tc.status, r.Status)
		})
	}
}

func TestWantedBookMatchesAcceptsTheRealThing(t *testing.T) {
	for _, signature := range []bool{false, true} {
		book := &assetpb.AssetBundle{ItemInstances: []*assetpb.AssetItemInstance{sealedBook(t, nil)}}
		require.True(t, wantedBookMatches(book, &tradepb.TradeWantedSkillBookTerms{SkillTableId: wantedSkillID, RequireMasterSignature: signature}))
	}
	unsigned := &assetpb.AssetBundle{ItemInstances: []*assetpb.AssetItemInstance{sealedBook(t, func(i *componentpb.ItemComp) {
		i.SealedSkillBook.CrafterName, i.SealedSkillBook.CrafterPlayerId = "", 0
	})}}
	require.True(t, wantedBookMatches(unsigned, &tradepb.TradeWantedSkillBookTerms{SkillTableId: wantedSkillID}), "不要求署名时无署名也收")
	garbage := &assetpb.AssetBundle{ItemInstances: []*assetpb.AssetItemInstance{{ItemUuid: wantedBookUUID, ConfigId: SealedSkillBookConfigID, StackSize: 1, InstanceProto: []byte{0xff, 0xff}}}}
	require.False(t, wantedBookMatches(garbage, &tradepb.TradeWantedSkillBookTerms{SkillTableId: wantedSkillID}), "解不开的实例按不符退回")
}

func TestWantedPhaseFollowsTheContract(t *testing.T) {
	for _, tc := range []struct {
		stage  string
		status uint32
		want   tradepb.WantedSkillBookOrderPhase
	}{
		{stageWantedEscrow, Escrowing, tradepb.WantedSkillBookOrderPhase_WANTED_SKILL_BOOK_ORDER_PHASE_ESCROWING},
		{stageWantedOpen, Waiting, tradepb.WantedSkillBookOrderPhase_WANTED_SKILL_BOOK_ORDER_PHASE_OPEN},
		{stageWantedFill, Escrowing, tradepb.WantedSkillBookOrderPhase_WANTED_SKILL_BOOK_ORDER_PHASE_FULFILLING},
		{"deliver", Delivering, tradepb.WantedSkillBookOrderPhase_WANTED_SKILL_BOOK_ORDER_PHASE_DELIVERING},
		{"return", CancelPending, tradepb.WantedSkillBookOrderPhase_WANTED_SKILL_BOOK_ORDER_PHASE_CANCELLING},
		{"done", Settled, tradepb.WantedSkillBookOrderPhase_WANTED_SKILL_BOOK_ORDER_PHASE_SETTLED},
		{"done", Cancelled, tradepb.WantedSkillBookOrderPhase_WANTED_SKILL_BOOK_ORDER_PHASE_CANCELLED},
		{"manual", Manual, tradepb.WantedSkillBookOrderPhase_WANTED_SKILL_BOOK_ORDER_PHASE_MANUAL},
	} {
		got := WantedPhase(&tradepb.TradeCommerceRecord{Status: tc.status}, &tradepb.TradeCommerceWorkflow{Stage: tc.stage})
		require.Equal(t, tc.want, got, "stage=%s status=%d", tc.stage, tc.status)
	}
}

// 挂榜中的求购单只等揭榜或撤单:排到一小时兜底复查,不每秒空转。
func TestWantedOpenOrderWaitsForPlayers(t *testing.T) {
	r, w := openWantedOrder(false)
	require.Equal(t, uint64(1000)+uint64(idleRecheck.Milliseconds()), nextDueMs(r, w, 1000))
}

func TestWantedBoardCursorRoundTrip(t *testing.T) {
	c := WantedBoardCursor{RewardGold: 50000, CommerceID: 123456789}
	got, ok := ParseWantedBoardCursor(c.String())
	require.True(t, ok)
	require.Equal(t, c, got)
	for _, raw := range []string{"", "50000", "x:1", "1:y"} {
		_, ok := ParseWantedBoardCursor(raw)
		require.False(t, ok, raw)
	}
}
