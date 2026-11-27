package logic

// 悬赏求书求购单(kind 7)客户端入口的开关、入参校验与视图(wanted_skill_book_logic.go)。不连库:用 commerce_missing_contract_test.go
// 的录制驱动,断言被拒的请求一条语句都没到存储;合同的状态机与真库用例在 commerce 包。

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	tradepb "proto/trade"
	"shared/generated/pb/table"
	"trade/internal/commerce"
	"trade/internal/constants"
)

const (
	wantedEligibleSkill   = uint32(501)
	wantedIneligibleSkill = uint32(502) // 配表里有,但不是常规永久技能(赛季、稀世等)
)

// fakeWantedSkills 是只认两门技能的假配表。
type fakeWantedSkills struct{}

func (fakeWantedSkills) WantedSkill(id uint32) (commerce.WantedSkill, bool) {
	switch id {
	case wantedEligibleSkill:
		return commerce.WantedSkill{ScrollID: 41, SkillName: "疾风剑", ScrollName: "剑宗卷"}, true
	case wantedIneligibleSkill:
		return commerce.WantedSkill{ScrollID: 42, SkillName: "赛季剑"}, false
	}
	return commerce.WantedSkill{}, false
}

// 开关关着:发榜、揭榜、公开榜都回「功能未开放」,一条语句都不到存储;本人的单照常查(好让发榜人撤单取回悬赏)。
func TestWantedSkillBookEntriesNeedTheFeature(t *testing.T) {
	store := &missingContractStore{}
	l := newMissingContractLogic(t, store)
	l.deps.Commerce.WantedSkills = nil

	created, err := l.CreateWantedSkillBookOrder(&tradepb.CreateWantedSkillBookOrderRequest{ClientRequestId: "wanted-create-0001", SkillTableId: wantedEligibleSkill, RewardGold: 1000})
	require.NoError(t, err)
	require.Equal(t, constants.ErrFeatureDisabled, created.GetErrorMessage().GetId())
	fulfilled, err := l.FulfillWantedSkillBookOrder(&tradepb.FulfillWantedSkillBookOrderRequest{OrderId: "7001", ClientRequestId: "wanted-fulfill-0001", SealedItemId: 9001})
	require.NoError(t, err)
	require.Equal(t, constants.ErrFeatureDisabled, fulfilled.GetErrorMessage().GetId())
	board, err := l.ListWantedSkillBookOrders(&tradepb.ListWantedSkillBookOrdersRequest{})
	require.NoError(t, err)
	require.Equal(t, constants.ErrFeatureDisabled, board.GetErrorMessage().GetId())
	require.Empty(t, store.recorded(), "开关关着不碰存储")

	mine, err := l.ListWantedSkillBookOrders(&tradepb.ListWantedSkillBookOrdersRequest{OnlyMine: true})
	require.NoError(t, err)
	require.NotEqual(t, constants.ErrFeatureDisabled, mine.GetErrorMessage().GetId(), "开关关了也要能看到自己的单")
	require.NotEmpty(t, store.recorded(), "本人的单照常去存储查")
}

// 发榜的无状态校验在碰存储之前做完:悬赏额 100..50000、只收常规永久技能。
func TestCreateWantedSkillBookOrderRejectsBadInputBeforeStorage(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   *tradepb.CreateWantedSkillBookOrderRequest
	}{
		{"悬赏额太低", &tradepb.CreateWantedSkillBookOrderRequest{ClientRequestId: "wanted-create-0001", SkillTableId: wantedEligibleSkill, RewardGold: commerce.WantedRewardMinGold - 1}},
		{"悬赏额太高", &tradepb.CreateWantedSkillBookOrderRequest{ClientRequestId: "wanted-create-0001", SkillTableId: wantedEligibleSkill, RewardGold: commerce.WantedRewardMaxGold + 1}},
		{"不能悬赏的技能", &tradepb.CreateWantedSkillBookOrderRequest{ClientRequestId: "wanted-create-0001", SkillTableId: wantedIneligibleSkill, RewardGold: 1000}},
		{"配表里没有的技能", &tradepb.CreateWantedSkillBookOrderRequest{ClientRequestId: "wanted-create-0001", SkillTableId: 9999, RewardGold: 1000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &missingContractStore{}
			out, err := newMissingContractLogic(t, store).CreateWantedSkillBookOrder(tc.in)
			require.NoError(t, err)
			require.Equal(t, uint32(table.CommonError_kInvalidParameter), out.GetErrorMessage().GetId())
			require.Nil(t, out.GetOrder())
			require.Empty(t, store.recorded())
		})
	}
}

// 揭榜的入参校验在碰存储之前做完:单号、请求号长度、封卷道籍号。
func TestFulfillWantedSkillBookOrderRejectsBadInputBeforeStorage(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   *tradepb.FulfillWantedSkillBookOrderRequest
	}{
		{"单号不是数字", &tradepb.FulfillWantedSkillBookOrderRequest{OrderId: "abc", ClientRequestId: "wanted-fulfill-0001", SealedItemId: 9001}},
		{"单号为 0", &tradepb.FulfillWantedSkillBookOrderRequest{OrderId: "0", ClientRequestId: "wanted-fulfill-0001", SealedItemId: 9001}},
		{"请求号太短", &tradepb.FulfillWantedSkillBookOrderRequest{OrderId: "7001", ClientRequestId: "short", SealedItemId: 9001}},
		{"没给封卷道籍", &tradepb.FulfillWantedSkillBookOrderRequest{OrderId: "7001", ClientRequestId: "wanted-fulfill-0001"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &missingContractStore{}
			out, err := newMissingContractLogic(t, store).FulfillWantedSkillBookOrder(tc.in)
			require.NoError(t, err)
			require.Equal(t, uint32(table.CommonError_kInvalidParameter), out.GetErrorMessage().GetId())
			require.Empty(t, store.recorded())
		})
	}
}

// 视图:发榜人是合同买家,揭榜人是合同卖家;阶段由合同推导;技能名按当前配表现查;只有发榜人自己 is_mine。
func TestWantedOrderViewMapsTheContract(t *testing.T) {
	w := &tradepb.TradeCommerceWorkflow{Stage: "wanted_fill", WantedSkillBook: &tradepb.TradeWantedSkillBookTerms{
		SkillTableId: wantedEligibleSkill, ScrollId: 41, RequireMasterSignature: true, RewardGold: 1000, SellerNetGold: 950, PosterName: "发榜人"}}
	raw, err := proto.Marshal(w)
	require.NoError(t, err)
	r := &tradepb.TradeCommerceRecord{CommerceId: 7001, Kind: commerce.KindWantedSkillBook, Status: commerce.Escrowing,
		BuyerPlayerId: playerA, SellerPlayerId: playerB, CreatedMs: 123, Version: 5, WorkflowProto: raw}

	v, ok := wantedOrderView(r, playerA, fakeWantedSkills{})
	require.True(t, ok)
	require.Equal(t, "7001", v.GetOrderId())
	require.Equal(t, playerA, v.GetPosterPlayerId())
	require.Equal(t, "发榜人", v.GetPosterName())
	require.Equal(t, playerB, v.GetFulfillerPlayerId())
	require.Equal(t, tradepb.WantedSkillBookOrderPhase_WANTED_SKILL_BOOK_ORDER_PHASE_FULFILLING, v.GetPhase())
	require.Equal(t, "疾风剑", v.GetSkillName())
	require.Equal(t, "剑宗卷", v.GetScrollName())
	require.Equal(t, uint64(950), v.GetSellerNetGold())
	require.True(t, v.GetRequireMasterSignature())
	require.True(t, v.GetIsMine())

	other, ok := wantedOrderView(r, playerB, nil)
	require.True(t, ok)
	require.False(t, other.GetIsMine())
	require.Empty(t, other.GetSkillName(), "没开求购单时不查表")

	r.Kind = 3
	_, ok = wantedOrderView(r, playerA, fakeWantedSkills{})
	require.False(t, ok, "不是求购单")
}
