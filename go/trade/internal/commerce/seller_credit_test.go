package commerce

import (
	"context"
	"fmt"
	"testing"

	dspb "proto/data_service"
	tradepb "proto/trade"

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

func TestSellerCreditClearsToZeroOnRemoteNewDeviceAndBlocksSameEnvWashReviews(t *testing.T) {
	ctx := context.Background()
	const now uint64 = 1_760_000_000_000
	owners := stubOwnershipLookup{owners: map[uint64]*dspb.AccountRoleOwnership{
		5001: {Account: "seller-env", OwnerRevision: 1},
	}}
	svc := &SellerCreditService{
		Store:     NewMemorySellerCreditStore(),
		Ownership: owners,
		Policy: SellerCreditPolicy{
			ReviewWindowMs:                  DefaultReviewWindowMs,
			PairScoreWindowMs:               DefaultPairScoreWindowMs,
			MaxScoredReviewsPerPairInWindow: 1,
			MinScoreAmountMinor:             100,
			PostEnvChangeScoreCooldownMs:    5_000, // 异地换机清 0 后 5 秒内防突击刷好评
		},
	}

	// 1) 首次记录基线 IP + 设备，并攒 2 笔真实好评（60 -> 64 分）
	p0, cleared, err := svc.RecordSellerEnvironment(ctx, SellerEnvironmentEvent{
		SellerPlayerID: 5001, OwnerAccount: "seller-env", OwnerRevision: 1,
		IP: "203.0.113.10", DeviceID: "DEV-AAA", ObservedAtMs: now,
	})
	require.NoError(t, err)
	require.False(t, cleared)
	require.EqualValues(t, 60, p0.CreditScore)

	for i := 1; i <= 2; i++ {
		_, _, _, err = svc.SubmitReview(ctx, SellerReviewRequest{
			Trade: CompletedTradeFact{
				OrderID: fmt.Sprintf("init-good-%d", i), SellerPlayerID: 5001, SellerAccount: "seller-env", SellerOwnerRevision: 1,
				BuyerPlayerID: uint64(5900 + i), BuyerAccount: fmt.Sprintf("init-buyer-%d", i),
				BuyerIP: fmt.Sprintf("192.0.2.%d", i), BuyerDeviceID: fmt.Sprintf("init-dev-%d", i),
				AmountMinor: 500, SettledAtMs: now + 500,
			},
			Rating: SellerRatingPositive,
		}, now+1_000)
		require.NoError(t, err)
	}

	// 2) 坐高铁回家 / 切 5G：设备没变（仍是 dev-aaa），跨城换 IP -> 0 影响（保住 64 分）！
	pTrain, cleared, err := svc.RecordSellerEnvironment(ctx, SellerEnvironmentEvent{
		SellerPlayerID: 5001, OwnerAccount: "seller-env", OwnerRevision: 1,
		IP: "198.51.100.22", DeviceID: "dev-aaa", ObservedAtMs: now + 2_000,
	})
	require.NoError(t, err)
	require.False(t, cleared)
	require.EqualValues(t, 64, pTrain.CreditScore)
	require.EqualValues(t, 2, pTrain.PositiveReviews)

	// 3) 家里同一 Wi-Fi 下手机切电脑（IP 没变，新增常用设备 dev-home-pc）-> 0 影响并记入常用设备
	pHomePC, cleared, err := svc.RecordSellerEnvironment(ctx, SellerEnvironmentEvent{
		SellerPlayerID: 5001, OwnerAccount: "seller-env", OwnerRevision: 1,
		IP: "198.51.100.22", DeviceID: "dev-home-pc", ObservedAtMs: now + 3_000,
	})
	require.NoError(t, err)
	require.False(t, cleared)
	require.EqualValues(t, 64, pHomePC.CreditScore)

	// 4) 切回已登记的常用手机 dev-aaa 并切 5G 新 IP -> 已是常用设备，0 影响！
	pBackPhone, cleared, err := svc.RecordSellerEnvironment(ctx, SellerEnvironmentEvent{
		SellerPlayerID: 5001, OwnerAccount: "seller-env", OwnerRevision: 1,
		IP: "198.51.100.33", DeviceID: "dev-aaa", ObservedAtMs: now + 4_000,
	})
	require.NoError(t, err)
	require.False(t, cleared)
	require.EqualValues(t, 64, pBackPhone.CreditScore)

	// 5) 私下卖号 / 外地借号 / 盗号换机：从未见过的陌生新设备 dev-ccc + 新 IP 198.51.100.88 -> 直接清 0！
	pWiped, cleared, err := svc.RecordSellerEnvironment(ctx, SellerEnvironmentEvent{
		SellerPlayerID: 5001, OwnerAccount: "seller-env", OwnerRevision: 1,
		IP: "198.51.100.88", DeviceID: "dev-ccc", ObservedAtMs: now + 8_000,
	})
	require.NoError(t, err)
	require.True(t, cleared)
	require.True(t, pWiped.EnvCreditCleared)
	require.EqualValues(t, 1, pWiped.EnvResetCount)
	require.Zero(t, pWiped.CreditScore)
	require.EqualValues(t, 10, pWiped.StarRatingTenths) // 1.0★ 警示
	require.Zero(t, pWiped.CompletedOrders)
	require.Zero(t, pWiped.PositiveReviews)
	require.Equal(t, SellerRiskTierRestricted, pWiped.RiskTier)
	require.Equal(t, []string{"dev-ccc"}, pWiped.KnownDevices)

	// 6) 刚异地换机清 0 后 2 秒内（< PostEnvChangeScoreCooldownMs 5s）突击刷好评：留痕但不涨分（仍为 0）
	recDuringCool, pCool, _, err := svc.SubmitReview(ctx, SellerReviewRequest{
		Trade: CompletedTradeFact{
			OrderID: "env-cool-order-1", SellerPlayerID: 5001, SellerAccount: "seller-env", SellerOwnerRevision: 1,
			BuyerPlayerID: 6001, BuyerAccount: "buyer-clean-1", BuyerIP: "192.0.2.10", BuyerDeviceID: "dev-buyer-1",
			AmountMinor: 500, SettledAtMs: now + 9_000,
		},
		Rating: SellerRatingPositive,
	}, now+10_000)
	require.NoError(t, err)
	require.False(t, recDuringCool.CountedForScore)
	require.Zero(t, pCool.CreditScore)

	// 7) 过冷静期后，买卖双方同 IP（198.51.100.88）或同设备（dev-ccc）互评：严格拦截不计分（仍为 0）！
	recSameIP, pSameIP, _, err := svc.SubmitReview(ctx, SellerReviewRequest{
		Trade: CompletedTradeFact{
			OrderID: "same-ip-order", SellerPlayerID: 5001, SellerAccount: "seller-env", SellerOwnerRevision: 1,
			BuyerPlayerID: 6002, BuyerAccount: "buyer-same-ip", BuyerIP: "198.51.100.88", BuyerDeviceID: "dev-buyer-2",
			AmountMinor: 500, SettledAtMs: now + 14_000,
		},
		Rating: SellerRatingPositive,
	}, now+15_000)
	require.NoError(t, err)
	require.False(t, recSameIP.CountedForScore)
	require.Zero(t, pSameIP.CreditScore)

	recSameDev, pSameDev, _, err := svc.SubmitReview(ctx, SellerReviewRequest{
		Trade: CompletedTradeFact{
			OrderID: "same-dev-order", SellerPlayerID: 5001, SellerAccount: "seller-env", SellerOwnerRevision: 1,
			BuyerPlayerID: 6003, BuyerAccount: "buyer-same-dev", BuyerIP: "192.0.2.30", BuyerDeviceID: "dev-ccc",
			AmountMinor: 500, SettledAtMs: now + 14_000,
		},
		Rating: SellerRatingPositive,
	}, now+15_000)
	require.NoError(t, err)
	require.False(t, recSameDev.CountedForScore)
	require.Zero(t, pSameDev.CreditScore)

	// 8) 真实独立买家第 1 次好评：从 0 分重新起步（0 -> 2 分，绝不回弹到 60 分）；
	// 换不同小号但共用同一买家设备或同一买家 IP 7 天内再评：一律拦截不计分！
	recFirstReal, pReal, _, err := svc.SubmitReview(ctx, SellerReviewRequest{
		Trade: CompletedTradeFact{
			OrderID: "real-buyer-1", SellerPlayerID: 5001, SellerAccount: "seller-env", SellerOwnerRevision: 1,
			BuyerPlayerID: 6010, BuyerAccount: "alt-acct-1", BuyerIP: "192.0.2.50", BuyerDeviceID: "emu-phone-1",
			AmountMinor: 500, SettledAtMs: now + 15_000,
		},
		Rating: SellerRatingPositive,
	}, now+16_000)
	require.NoError(t, err)
	require.True(t, recFirstReal.CountedForScore)
	require.EqualValues(t, 2, pReal.CreditScore)

	recAltSameDev, pAlt1, _, err := svc.SubmitReview(ctx, SellerReviewRequest{
		Trade: CompletedTradeFact{
			OrderID: "alt-same-dev", SellerPlayerID: 5001, SellerAccount: "seller-env", SellerOwnerRevision: 1,
			BuyerPlayerID: 6011, BuyerAccount: "alt-acct-2", BuyerIP: "192.0.2.51", BuyerDeviceID: "emu-phone-1",
			AmountMinor: 500, SettledAtMs: now + 15_000,
		},
		Rating: SellerRatingPositive,
	}, now+16_100)
	require.NoError(t, err)
	require.False(t, recAltSameDev.CountedForScore)
	require.EqualValues(t, 2, pAlt1.CreditScore)

	recAltSameIP, pAlt2, _, err := svc.SubmitReview(ctx, SellerReviewRequest{
		Trade: CompletedTradeFact{
			OrderID: "alt-same-ip", SellerPlayerID: 5001, SellerAccount: "seller-env", SellerOwnerRevision: 1,
			BuyerPlayerID: 6012, BuyerAccount: "alt-acct-3", BuyerIP: "192.0.2.50", BuyerDeviceID: "emu-phone-2",
			AmountMinor: 500, SettledAtMs: now + 15_000,
		},
		Rating: SellerRatingPositive,
	}, now+16_200)
	require.NoError(t, err)
	require.False(t, recAltSameIP.CountedForScore)
	require.EqualValues(t, 2, pAlt2.CreditScore)

	// 9) 防洗白验证：账号背上违约记录后，故意再异地换机也不得把 ViolationCount 洗白！
	_, err = svc.RecordViolation(ctx, SellerViolationRecord{
		CaseID: "CASE-NO-WASH-1", OrderID: "real-buyer-1", SellerPlayerID: 5001,
		SellerAccount: "seller-env", SellerOwnerRevision: 1, Reason: "fraud", RecordedAtMs: now + 17_000,
	})
	require.NoError(t, err)
	pTryWash, cleared2, err := svc.RecordSellerEnvironment(ctx, SellerEnvironmentEvent{
		SellerPlayerID: 5001, OwnerAccount: "seller-env", OwnerRevision: 1,
		IP: "198.51.100.99", DeviceID: "dev-wash-attempt", ObservedAtMs: now + 18_000,
	})
	require.NoError(t, err)
	require.True(t, cleared2)
	require.EqualValues(t, 1, pTryWash.ViolationCount, "严禁借异地换机洗白已有违约黑历史")
	require.Zero(t, pTryWash.CreditScore)
	require.Equal(t, SellerRiskTierRestricted, pTryWash.RiskTier)

	// 10) 官方聚宝斋正规过户换新主人（owner_revision 1 -> 2）：新持有周期干净重置
	pTransferred, err := svc.OnCharacterTransferred(ctx, 5001, "new-clean-owner", 2, now+25_000)
	require.NoError(t, err)
	require.False(t, pTransferred.EnvCreditCleared)
	require.Zero(t, pTransferred.EnvResetCount)
	require.Zero(t, pTransferred.ViolationCount)
	require.Empty(t, pTransferred.LastIP)
	require.Empty(t, pTransferred.LastDeviceID)
	require.Empty(t, pTransferred.KnownDevices)
	require.EqualValues(t, DefaultSellerInitialScore, pTransferred.CreditScore)
}

func TestSettledTradeFactExtractsOfficialEscrowAndIgnoresFaceToFaceOrSelf(t *testing.T) {
	const now uint64 = 1_760_000_000_000
	idSeller := &tradepb.TradeRoleIdentity{Account: "acct-seller", OwnerRevision: 2}
	idBuyer := &tradepb.TradeRoleIdentity{Account: "acct-buyer", OwnerRevision: 1}

	// 1) 游戏内当面交易（kind=2）绝不计入卖家店铺信用成交（防熟人免税互刷店分）
	_, ok := SettledTradeFact(
		&tradepb.TradeCommerceRecord{CommerceId: 11, Kind: 2, Status: Settled, SellerPlayerId: 101, BuyerPlayerId: 202},
		&tradepb.TradeCommerceWorkflow{SellerIdentity: idSeller, BuyerIdentity: idBuyer}, now)
	require.False(t, ok)

	// 2) 官方托管固定价寄售（kind=3）提取完整成交事实
	factConsign, ok := SettledTradeFact(
		&tradepb.TradeCommerceRecord{CommerceId: 12, Kind: 3, Status: Settled, SellerPlayerId: 101, BuyerPlayerId: 202, UnitPrice: 8800},
		&tradepb.TradeCommerceWorkflow{SellerIdentity: idSeller, BuyerIdentity: idBuyer}, now)
	require.True(t, ok)
	require.Equal(t, "commerce:12", factConsign.OrderID)
	require.EqualValues(t, 8800, factConsign.AmountMinor)
	require.EqualValues(t, 2, factConsign.SellerOwnerRevision)

	// 3) 市场撮合母单（kind=4, side!=0）不计，仅子单（side==0）计入
	_, ok = SettledTradeFact(
		&tradepb.TradeCommerceRecord{CommerceId: 13, Kind: 4, Side: 2, Status: Settled, SellerPlayerId: 101, BuyerPlayerId: 202, UnitPrice: 100, Quantity: 5},
		&tradepb.TradeCommerceWorkflow{SellerIdentity: idSeller, BuyerIdentity: idBuyer}, now)
	require.False(t, ok)

	factFill, ok := SettledTradeFact(
		&tradepb.TradeCommerceRecord{CommerceId: 14, Kind: 4, Side: 0, Status: Settled, SellerPlayerId: 101, BuyerPlayerId: 202, UnitPrice: 100, Quantity: 5},
		&tradepb.TradeCommerceWorkflow{SellerIdentity: idSeller, BuyerIdentity: idBuyer}, now)
	require.True(t, ok)
	require.EqualValues(t, 500, factFill.AmountMinor)

	// 4) 拍卖成交（kind=5）取最高出价者与成交价
	factLot, ok := SettledTradeFact(
		&tradepb.TradeCommerceRecord{CommerceId: 15, Kind: 5, Status: Settled, SellerPlayerId: 101},
		&tradepb.TradeCommerceWorkflow{SellerIdentity: idSeller, BuyerIdentity: idBuyer, HighestBidder: 303, HighestBid: 12000}, now)
	require.True(t, ok)
	require.EqualValues(t, 303, factLot.BuyerPlayerID)
	require.EqualValues(t, 12000, factLot.AmountMinor)

	// 5) 人民币非角色寄售（kind=6, CharacterPlayerId==0）计入且优先复用 MallOrderId；角色过户（CharacterPlayerId!=0）走清空链路不计入
	factRmbItem, ok := SettledTradeFact(
		&tradepb.TradeCommerceRecord{CommerceId: 16, Kind: 6, Status: Settled, SellerPlayerId: 101, BuyerPlayerId: 202},
		&tradepb.TradeCommerceWorkflow{Rmb: &tradepb.TradeRmbConsignmentWorkflow{
			SellerAccount: "acct-seller", SellerOwnerRevision: 2, BuyerAccount: "acct-buyer",
			PriceFen: 3500, MallOrderId: "00112233-4455-6677-8899-aabbccddeeff",
		}}, now)
	require.True(t, ok)
	require.Equal(t, "00112233-4455-6677-8899-aabbccddeeff", factRmbItem.OrderID)
	require.EqualValues(t, 3500, factRmbItem.AmountMinor)

	_, ok = SettledTradeFact(
		&tradepb.TradeCommerceRecord{CommerceId: 17, Kind: 6, Status: Settled, SellerPlayerId: 101, BuyerPlayerId: 202},
		&tradepb.TradeCommerceWorkflow{Rmb: &tradepb.TradeRmbConsignmentWorkflow{
			SellerAccount: "acct-seller", SellerOwnerRevision: 2, BuyerAccount: "acct-buyer",
			CharacterPlayerId: 101, PriceFen: 9900, MallOrderId: "00112233-4455-6677-8899-aabbccddee99",
		}}, now)
	require.False(t, ok)

	// 6) 同账号自成交被过滤
	_, ok = SettledTradeFact(
		&tradepb.TradeCommerceRecord{CommerceId: 18, Kind: 3, Status: Settled, SellerPlayerId: 101, BuyerPlayerId: 102, UnitPrice: 500},
		&tradepb.TradeCommerceWorkflow{SellerIdentity: idSeller, BuyerIdentity: &tradepb.TradeRoleIdentity{Account: "acct-seller", OwnerRevision: 1}}, now)
	require.False(t, ok)
}
