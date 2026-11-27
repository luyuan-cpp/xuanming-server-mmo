package commerce

// 悬赏求书求购单(kind 7)的真库用例:要隔离 TiDB(COMMERCE_TEST_DSN,COMMERCE_REQUIRE_TIDB=1 时缺了就失败),每例建自己的随机库。
// 资产终局由夹具直接写进 outbox 行(同 engine_tidb_test.go 的 settleLegs),不冒充 scene 端到端;揭榜托管腿额外写上
// scene 扣出时交回的封卷道籍快照(escrow_snapshot)。

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	assetpb "proto/common/asset"
	componentpb "proto/common/component"
	tradepb "proto/trade"
)

// testWantedSkills 是只认技能 501 的假配表。
type testWantedSkills struct{}

func (testWantedSkills) WantedSkill(id uint32) (WantedSkill, bool) {
	if id == 501 {
		return WantedSkill{ScrollID: 41, SkillName: "疾风剑", ScrollName: "剑宗卷"}, true
	}
	return WantedSkill{}, false
}

// sealedBookSnapshot 是 scene 扣出一枚封卷道籍时交回的快照。
func sealedBookSnapshot(t *testing.T, uuid uint64, skill uint32) []byte {
	t.Helper()
	item, err := proto.Marshal(&componentpb.ItemComp{ItemId: uuid, ConfigId: SealedSkillBookConfigID, Size: 1,
		SealedSkillBook: &componentpb.SealedSkillBookData{SkillTableId: skill, ScrollId: 41}})
	require.NoError(t, err)
	raw, err := proto.Marshal(&assetpb.AssetSnapshot{Items: []*assetpb.AssetItemInstance{{ItemUuid: uuid, ConfigId: SealedSkillBookConfigID, StackSize: 1, InstanceProto: item}}})
	require.NoError(t, err)
	return raw
}

// landWantedDebit 让当前这次揭榜的托管腿按 APPLIED 落地并带上快照,然后唤醒合同(生产由资产指令终结一侧唤醒)。
func landWantedDebit(t *testing.T, e *Engine, id uint64, snapshot []byte) {
	t.Helper()
	_, w := state(t, e, id)
	debit := wantedAttemptDebit(w, w.QuantityReserved)
	require.NotNil(t, debit)
	require.NotZero(t, debit.OpId, "托管腿应已发出")
	result, err := e.Repo.DB.Exec("UPDATE trade_asset_op SET status=2,durable=1,escrow_snapshot=? WHERE op_id=? AND status=1", snapshot, debit.OpId)
	require.NoError(t, err)
	n, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	require.NoError(t, e.Repo.Wake(context.Background(), id, e.now()))
}

func pendingCredits(t *testing.T, e *Engine) int {
	t.Helper()
	var n int
	require.NoError(t, e.Repo.DB.QueryRow("SELECT COUNT(*) FROM trade_asset_op WHERE stream=4 AND status=1").Scan(&n))
	return n
}

// 发榜 → 托管 → 上榜;不符的书原样退回、单子回到挂榜;相符的书先交给发榜人,到账之后才发税后银两,两条都到账成交。
func TestTiDBWantedSkillBookMismatchReopensThenMatchSettles(t *testing.T) {
	e := tiDBEngine(t)
	e.WantedSkills = testWantedSkills{}
	ctx := context.Background()
	order, err := e.CreateWantedSkillBookOrder(ctx, 11, &tradepb.CreateWantedSkillBookOrderRequest{ClientRequestId: "wanted-create-0001", SkillTableId: 501, RewardGold: 1000})
	require.NoError(t, err)
	tick(t, e)
	r, w := state(t, e, order.CommerceId)
	require.Equal(t, stageWantedEscrow, w.Stage, "悬赏没托管成之前不上榜")
	board, err := e.ListOpenWantedSkillBookOrders(ctx, WantedBoardCursor{}, false, 10)
	require.NoError(t, err)
	require.Empty(t, board)

	settleLegs(t, e, 2)
	tick(t, e)
	r, w = state(t, e, order.CommerceId)
	require.Equal(t, Waiting, r.Status)
	require.Equal(t, stageWantedOpen, w.Stage)
	board, err = e.ListOpenWantedSkillBookOrders(ctx, WantedBoardCursor{}, false, 10)
	require.NoError(t, err)
	require.Len(t, board, 1)
	require.Equal(t, order.CommerceId, board[0].CommerceId)

	// 第一次:交了一本技能不对的书。
	_, err = e.FulfillWantedSkillBookOrder(ctx, 21, order.CommerceId, "wanted-fulfill-0001", 9001)
	require.NoError(t, err)
	_, err = e.FulfillWantedSkillBookOrder(ctx, 22, order.CommerceId, "wanted-fulfill-0002", 9002)
	require.ErrorIs(t, err, ErrConflict, "同一张单同时只允许一次揭榜在途")
	tick(t, e)
	landWantedDebit(t, e, order.CommerceId, sealedBookSnapshot(t, 9001, 502))
	tick(t, e)
	r, w = state(t, e, order.CommerceId)
	require.Equal(t, uint64(21), r.SellerPlayerId, "退书期间仍是交易中")
	require.NotNil(t, leg(w, wantedReturnRole(1)))
	require.Nil(t, leg(w, "buyer_receive"))
	settleLegs(t, e, 2)
	tick(t, e)
	r, w = state(t, e, order.CommerceId)
	require.Equal(t, Waiting, r.Status)
	require.Equal(t, stageWantedOpen, w.Stage)
	require.Zero(t, r.SellerPlayerId)
	_, err = e.FulfillWantedSkillBookOrder(ctx, 21, order.CommerceId, "wanted-fulfill-0001", 9001)
	require.ErrorIs(t, err, ErrWantedBookMismatch, "按同一请求号重发,查到那一次因书不符作废")

	// 第二次:别人交了相符的书。
	_, err = e.FulfillWantedSkillBookOrder(ctx, 22, order.CommerceId, "wanted-fulfill-0002", 9002)
	require.NoError(t, err)
	tick(t, e)
	landWantedDebit(t, e, order.CommerceId, sealedBookSnapshot(t, 9002, 501))
	tick(t, e)
	r, _ = state(t, e, order.CommerceId)
	require.Equal(t, Delivering, r.Status)
	require.Equal(t, 1, pendingCredits(t, e), "先只发书,税后银两等书到账")
	settleLegs(t, e, 2)
	tick(t, e)
	require.Equal(t, 1, pendingCredits(t, e), "书到账之后才发税后银两")
	var payload []byte
	require.NoError(t, e.Repo.DB.QueryRow("SELECT payload FROM trade_asset_op WHERE stream=4 AND status=1 AND player_id=22").Scan(&payload))
	proceeds := &assetpb.AssetBundle{}
	require.NoError(t, proto.Unmarshal(payload, proceeds))
	require.True(t, proto.Equal(&assetpb.AssetBundle{Currencies: []*assetpb.CurrencyAmount{{CurrencyType: 0, Amount: 950}}}, proceeds), "揭榜人拿税后 95%")
	settleLegs(t, e, 2)
	tick(t, e)
	r, _ = state(t, e, order.CommerceId)
	require.Equal(t, Settled, r.Status)
	board, err = e.ListOpenWantedSkillBookOrders(ctx, WantedBoardCursor{}, false, 10)
	require.NoError(t, err)
	require.Empty(t, board, "成交之后下榜")
}

// 挂榜中撤单:悬赏全额退回;交书途中发榜人撤不了。
func TestTiDBWantedSkillBookCancelReturnsTheReward(t *testing.T) {
	e := tiDBEngine(t)
	e.WantedSkills = testWantedSkills{}
	ctx := context.Background()
	order, err := e.CreateWantedSkillBookOrder(ctx, 11, &tradepb.CreateWantedSkillBookOrderRequest{ClientRequestId: "wanted-create-0002", SkillTableId: 501, RewardGold: 2000})
	require.NoError(t, err)
	tick(t, e)
	settleLegs(t, e, 2)
	tick(t, e)

	_, err = e.FulfillWantedSkillBookOrder(ctx, 21, order.CommerceId, "wanted-fulfill-0003", 9003)
	require.NoError(t, err)
	_, err = e.Mutate(ctx, 11, order.CommerceId, "cancel")
	require.ErrorIs(t, err, ErrConflict, "有人正在交书,发榜人暂时撤不了")
	tick(t, e)
	settleLegs(t, e, 3) // 书没扣到(已不在背包):这次揭榜作废,回到挂榜
	tick(t, e)
	r, _ := state(t, e, order.CommerceId)
	require.Equal(t, Waiting, r.Status)

	_, err = e.Mutate(ctx, 21, order.CommerceId, "cancel")
	require.ErrorIs(t, err, ErrMissing, "揭榜人不是发榜人,撤不了")
	_, err = e.Mutate(ctx, 11, order.CommerceId, "cancel")
	require.NoError(t, err)
	tick(t, e)
	_, w := state(t, e, order.CommerceId)
	back := leg(w, "buyer_return")
	require.NotNil(t, back)
	require.True(t, proto.Equal(&assetpb.AssetBundle{Currencies: []*assetpb.CurrencyAmount{{CurrencyType: 0, Amount: 2000}}}, back.Bundle))
	settleLegs(t, e, 2)
	tick(t, e)
	r, _ = state(t, e, order.CommerceId)
	require.Equal(t, Cancelled, r.Status)
}

// 每人同时生效的求购单至多 3 张,并发发榜也不超:计数与插入在同一把角色锁下(真护栏副本行锁)。
func TestTiDBWantedSkillBookPosterLimitHoldsUnderConcurrency(t *testing.T) {
	e, directory := fenceEngine(t)
	e.WantedSkills = testWantedSkills{}
	directory.set(fenceOf(11, "poster", 1, 0, 1))
	ctx := fenceActor(t, 11, "poster", 1)
	create := func(i int) error {
		_, err := e.CreateWantedSkillBookOrder(ctx, 11, &tradepb.CreateWantedSkillBookOrderRequest{ClientRequestId: fmt.Sprintf("wanted-concurrent-%04d", i), SkillTableId: 501, RewardGold: 100 + uint64(i)})
		return err
	}
	// 先单独建一张,把护栏副本行自愈出来;之后的并发只争这一把行锁。
	require.NoError(t, create(0))

	var mu sync.Mutex
	var errs []error
	var wg sync.WaitGroup
	for i := 1; i <= 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := create(i)
			mu.Lock()
			errs = append(errs, err)
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	wins, limited := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrWantedOrderLimit):
			limited++
		default:
			t.Fatalf("并发发榜只该成功或撞上限: %v", err)
		}
	}
	require.Equal(t, MaxActiveWantedOrders-1, wins)
	require.Equal(t, 6-(MaxActiveWantedOrders-1), limited)
	var active int
	require.NoError(t, e.Repo.DB.QueryRow("SELECT COUNT(*) FROM trade_commerce WHERE kind=? AND buyer_player_id=11", KindWantedSkillBook).Scan(&active))
	require.Equal(t, MaxActiveWantedOrders, active)
}
