package commerce

import (
	"context"
	"fmt"
	"testing"

	dspb "proto/data_service"

	"github.com/stretchr/testify/require"
)

type stubOwnershipLookup struct {
	owners map[uint64]*dspb.AccountRoleOwnership
}

func (s stubOwnershipLookup) Lookup(_ context.Context, playerID uint64) (*dspb.AccountRoleOwnership, error) {
	owned, ok := s.owners[playerID]
	if !ok {
		return nil, ErrMissing
	}
	return owned, nil
}

func TestSellerCreditAccumulatesAndResetsCompletelyOnOwnerTransfer(t *testing.T) {
	ctx := context.Background()
	const baseNow uint64 = 1_760_000_000_000
	owners := stubOwnershipLookup{owners: map[uint64]*dspb.AccountRoleOwnership{
		1001: {Account: "old-owner-acct", OwnerRevision: 1},
	}}
	svc := &SellerCreditService{
		Store:     NewMemorySellerCreditStore(),
		Ownership: owners,
		Policy: SellerCreditPolicy{
			MinScoreAmountMinor: 100,
		},
	}

	// 1) 原号主通过 20 名不同买家的真实托管成交累积成为五星高信誉老店（Trusted）
	for i := 1; i <= 20; i++ {
		fact := CompletedTradeFact{
			OrderID:             fmt.Sprintf("order-old-%02d", i),
			SellerPlayerID:      1001,
			SellerAccount:       "old-owner-acct",
			SellerOwnerRevision: 1,
			BuyerPlayerID:       uint64(2000 + i),
			BuyerAccount:        fmt.Sprintf("buyer-acct-%02d", i),
			AmountMinor:         500,
			SettledAtMs:         baseNow + uint64(i)*1000,
		}
		rec, _, replayed, err := svc.SubmitReview(ctx, SellerReviewRequest{Trade: fact, Rating: SellerRatingPositive}, fact.SettledAtMs+500)
		require.NoError(t, err)
		require.False(t, replayed)
		require.True(t, rec.CountedForScore)
	}
	oldProfile, err := svc.GetProfile(ctx, 1001, baseNow+30_000)
	require.NoError(t, err)
	require.EqualValues(t, 20, oldProfile.CompletedOrders)
	require.EqualValues(t, 20, oldProfile.PositiveReviews)
	require.EqualValues(t, 100, oldProfile.CreditScore)
	require.EqualValues(t, 50, oldProfile.StarRatingTenths) // 5.0 星
	require.EqualValues(t, 10000, oldProfile.PositiveRateBps)
	require.Equal(t, SellerRiskTierTrusted, oldProfile.RiskTier)
	require.False(t, oldProfile.OwnershipChanged)

	// 2) 角色过户给新号主（owner_revision: 1 -> 2, account 变更）：
	// 必须强制清空全部历史评分、成交笔数与好评，并显式标出“持有者已变更、信用重算”，降回新店保护档！
	transferTime := baseNow + 100_000
	owners.owners[1001] = &dspb.AccountRoleOwnership{Account: "new-owner-acct", OwnerRevision: 2}
	_, err = svc.OnCharacterTransferred(ctx, 1001, "new-owner-acct", 2, transferTime)
	require.NoError(t, err)

	newProfile, err := svc.GetProfile(ctx, 1001, transferTime+1_000)
	require.NoError(t, err)
	require.Equal(t, "new-owner-acct", newProfile.OwnerAccount)
	require.EqualValues(t, 2, newProfile.OwnerRevision)
	require.True(t, newProfile.OwnershipChanged)
	require.EqualValues(t, transferTime, newProfile.OwnershipChangedAtMs)
	require.Zero(t, newProfile.CompletedOrders)
	require.Zero(t, newProfile.PositiveReviews)
	require.Zero(t, newProfile.PositiveRateBps)
	require.EqualValues(t, DefaultSellerInitialScore, newProfile.CreditScore)
	require.EqualValues(t, DefaultSellerInitialStarTenths, newProfile.StarRatingTenths)
	require.Equal(t, SellerRiskTierNewOrTransferred, newProfile.RiskTier)

	// 3) 即使过户前旧交易的迟到好评此时到达，也只落旧 revision=1，绝不污染新号主（revision=2）的信用！
	lateOldOrder := CompletedTradeFact{
		OrderID:             "order-old-late-99",
		SellerPlayerID:      1001,
		SellerAccount:       "old-owner-acct",
		SellerOwnerRevision: 1,
		BuyerPlayerID:       2999,
		BuyerAccount:        "buyer-late-99",
		AmountMinor:         500,
		SettledAtMs:         transferTime - 1_000,
	}
	_, _, _, err = svc.SubmitReview(ctx, SellerReviewRequest{Trade: lateOldOrder, Rating: SellerRatingPositive}, transferTime+2_000)
	require.NoError(t, err)
	stillClean, err := svc.GetProfile(ctx, 1001, transferTime+3_000)
	require.NoError(t, err)
	require.Zero(t, stillClean.CompletedOrders)
	require.Zero(t, stillClean.PositiveReviews)
	require.EqualValues(t, DefaultSellerInitialScore, stillClean.CreditScore)
}

func TestSellerCreditPreventsSelfReviewWashBoostingAndPenalizesViolations(t *testing.T) {
	ctx := context.Background()
	const now uint64 = 1_760_000_000_000
	owners := stubOwnershipLookup{owners: map[uint64]*dspb.AccountRoleOwnership{
		3001: {Account: "seller-acct", OwnerRevision: 1},
	}}
	svc := &SellerCreditService{
		Store:     NewMemorySellerCreditStore(),
		Ownership: owners,
		Policy: SellerCreditPolicy{
			ReviewWindowMs:                  DefaultReviewWindowMs,
			PairScoreWindowMs:               DefaultPairScoreWindowMs,
			MaxScoredReviewsPerPairInWindow: 1,
			MinScoreAmountMinor:             100,
		},
	}

	// 1) 同账号自买自评直接拒绝
	_, _, _, err := svc.SubmitReview(ctx, SellerReviewRequest{
		Trade: CompletedTradeFact{
			OrderID: "self-order", SellerPlayerID: 3001, SellerAccount: "seller-acct", SellerOwnerRevision: 1,
			BuyerPlayerID: 3002, BuyerAccount: "seller-acct", AmountMinor: 1000, SettledAtMs: now,
		},
		Rating: SellerRatingPositive,
	}, now+100)
	require.ErrorIs(t, err, ErrSelf)

	// 2) 1 银两小额低于计分门槛（MinScoreAmountMinor=100）：留痕但不涨分
	recLow, profLow, _, err := svc.SubmitReview(ctx, SellerReviewRequest{
		Trade: CompletedTradeFact{
			OrderID: "tiny-order", SellerPlayerID: 3001, SellerAccount: "seller-acct", SellerOwnerRevision: 1,
			BuyerPlayerID: 4001, BuyerAccount: "buyer-a", AmountMinor: 1, SettledAtMs: now,
		},
		Rating: SellerRatingPositive,
	}, now+100)
	require.NoError(t, err)
	require.False(t, recLow.CountedForScore)
	require.Zero(t, profLow.PositiveReviews)
	require.EqualValues(t, DefaultSellerInitialScore, profLow.CreditScore)

	// 3) 同买家 7 天内连续两笔达标好评：第 1 笔计分，第 2 笔不计分（防刷五星），重放第 1 笔幂等，改评冲突拒回
	firstTrade := CompletedTradeFact{
		OrderID: "pair-order-1", SellerPlayerID: 3001, SellerAccount: "seller-acct", SellerOwnerRevision: 1,
		BuyerPlayerID: 4002, BuyerAccount: "buyer-b", AmountMinor: 500, SettledAtMs: now,
	}
	rec1, prof1, replayed, err := svc.SubmitReview(ctx, SellerReviewRequest{Trade: firstTrade, Rating: SellerRatingPositive}, now+200)
	require.NoError(t, err)
	require.False(t, replayed)
	require.True(t, rec1.CountedForScore)
	require.EqualValues(t, 62, prof1.CreditScore)

	// 同单幂等重放不加分；改评不同分数拒回 ErrReplay
	_, _, replayed2, err := svc.SubmitReview(ctx, SellerReviewRequest{Trade: firstTrade, Rating: SellerRatingPositive}, now+300)
	require.NoError(t, err)
	require.True(t, replayed2)
	_, _, _, err = svc.SubmitReview(ctx, SellerReviewRequest{Trade: firstTrade, Rating: SellerRatingNegative}, now+300)
	require.ErrorIs(t, err, ErrReplay)

	secondTradeSamePair := firstTrade
	secondTradeSamePair.OrderID = "pair-order-2"
	rec2, prof2, _, err := svc.SubmitReview(ctx, SellerReviewRequest{Trade: secondTradeSamePair, Rating: SellerRatingPositive}, now+400)
	require.NoError(t, err)
	require.False(t, rec2.CountedForScore, "同买家 7 天内超出上限不得重复刷分")
	require.EqualValues(t, 62, prof2.CreditScore)

	// 4) 核实违约/拒付按 CaseID 幂等扣重分并立降为 Restricted
	profViol, err := svc.RecordViolation(ctx, SellerViolationRecord{
		CaseID: "CASE-FRAUD-01", OrderID: "pair-order-1", SellerPlayerID: 3001,
		SellerAccount: "seller-acct", SellerOwnerRevision: 1, Reason: "confirmed chargeback fraud", RecordedAtMs: now + 500,
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, profViol.ViolationCount)
	require.EqualValues(t, 37, profViol.CreditScore)
	require.Equal(t, SellerRiskTierRestricted, profViol.RiskTier)
	// 同 CaseID 幂等重放不重复扣分
	profViolReplay, err := svc.RecordViolation(ctx, SellerViolationRecord{
		CaseID: "CASE-FRAUD-01", OrderID: "pair-order-1", SellerPlayerID: 3001,
		SellerAccount: "seller-acct", SellerOwnerRevision: 1, Reason: "confirmed chargeback fraud", RecordedAtMs: now + 600,
	})
	require.NoError(t, err)
	require.EqualValues(t, 37, profViolReplay.CreditScore)

	// 5) 即使未经 OnCharacterTransferred 主动回调（例如 GM/外部直接过户导致 owner_revision 递增），
	// 下次 GetProfile 查权威归属时也必须自动清空前任的违约记录与评分，从初始重算！
	owners.owners[3001] = &dspb.AccountRoleOwnership{Account: "brand-new-owner", OwnerRevision: 2}
	lazyReset, err := svc.GetProfile(ctx, 3001, now+10_000)
	require.NoError(t, err)
	require.True(t, lazyReset.OwnershipChanged)
	require.Zero(t, lazyReset.ViolationCount)
	require.Zero(t, lazyReset.CompletedOrders)
	require.EqualValues(t, DefaultSellerInitialScore, lazyReset.CreditScore)
	require.Equal(t, SellerRiskTierNewOrTransferred, lazyReset.RiskTier)
}
