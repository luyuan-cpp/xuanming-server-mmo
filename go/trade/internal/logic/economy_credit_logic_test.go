package logic

import (
	"context"
	"testing"
	"time"

	base "proto/common/base"
	dspb "proto/data_service"
	tradepb "proto/trade"
	"shared/currencytype"
	"trade/internal/commerce"
	"trade/internal/config"
	"trade/internal/session"

	"github.com/stretchr/testify/require"
)

type stubLogicOwnership struct {
	owners map[uint64]*dspb.AccountRoleOwnership
}

func (s stubLogicOwnership) Lookup(_ context.Context, playerID uint64) (*dspb.AccountRoleOwnership, error) {
	if owned, ok := s.owners[playerID]; ok {
		return owned, nil
	}
	return nil, commerce.ErrMissing
}

type stubLogicRefPrice struct {
	ref commerce.ReferencePrice
}

func (s stubLogicRefPrice) ReferencePrice(_ context.Context, _, _ uint32, _ uint64) (commerce.ReferencePrice, error) {
	return s.ref, nil
}

type stubLogicDailyOut struct {
	used uint64
}

func (s stubLogicDailyOut) DailyCurrencyOutflow(_ context.Context, _ uint64, _ uint32, _ uint64) (uint64, error) {
	return s.used, nil
}

func clientSessionCtx(playerID uint64) context.Context {
	return session.WithDetails(context.Background(), &base.SessionDetails{PlayerId: playerID})
}

func TestEconomyCreditLogicGetSellerCreditMarketP50AndQuota(t *testing.T) {
	fixedNow := time.UnixMilli(1_760_000_000_000)
	creditSvc := &commerce.SellerCreditService{
		Store: commerce.NewMemorySellerCreditStore(),
		Ownership: stubLogicOwnership{owners: map[uint64]*dspb.AccountRoleOwnership{
			7001: {Account: "acct-7001", OwnerRevision: 2},
		}},
	}
	// 先模拟异地陌生设备清 0 + 保留 1 次违约，确保对外视图准确脱敏且呈现高危态。
	_, _, err := creditSvc.RecordSellerEnvironment(context.Background(), commerce.SellerEnvironmentEvent{
		SellerPlayerID: 7001, OwnerAccount: "acct-7001", OwnerRevision: 2,
		IP: "203.0.113.1", DeviceID: "dev-home", ObservedAtMs: uint64(fixedNow.UnixMilli()) - 5000,
	})
	require.NoError(t, err)
	_, err = creditSvc.RecordViolation(context.Background(), commerce.SellerViolationRecord{
		CaseID: "CASE-LOGIC-1", OrderID: "ord-1", SellerPlayerID: 7001,
		SellerAccount: "acct-7001", SellerOwnerRevision: 2, Reason: "fraud",
		RecordedAtMs: uint64(fixedNow.UnixMilli()) - 3000,
	})
	require.NoError(t, err)
	_, cleared, err := creditSvc.RecordSellerEnvironment(context.Background(), commerce.SellerEnvironmentEvent{
		SellerPlayerID: 7001, OwnerAccount: "acct-7001", OwnerRevision: 2,
		IP: "198.51.100.77", DeviceID: "dev-stranger", ObservedAtMs: uint64(fixedNow.UnixMilli()) - 1000,
	})
	require.NoError(t, err)
	require.True(t, cleared)

	deps := Deps{
		Config: config.Config{
			Timeout: 4000,
			Commerce: config.CommerceConf{
				PlayerTrade: config.PlayerTradeConf{
					Mode:                   commerce.PlayerTradeModeStrictConsumables,
					ConsumableItems:        []uint32{501, 502},
					MaxDailyCurrencyAmount: 10_000,
				},
			},
		},
		Now:     func() time.Time { return fixedNow },
		Credits: creditSvc,
		Commerce: &commerce.Engine{
			PlayerTradeGuard: &commerce.PlayerTradeGuard{
				DailyOut: stubLogicDailyOut{used: 2_500},
			},
		},
		ReferencePrices: stubLogicRefPrice{ref: commerce.ReferencePrice{
			ItemConfigID:       3403,
			CurrencyType:       currencytype.Gold,
			MedianUnitPrice:    24_800,
			SampleCount:        26,
			InsufficientSample: false,
		}},
	}

	logic := NewJubaozhaiLogic(clientSessionCtx(8001), deps)

	// 1) GetSellerCredit：命中异地换机清 0 且保留违约，返回受限档
	creditResp, err := logic.GetSellerCredit(&tradepb.GetSellerCreditRequest{SellerPlayerId: 7001})
	require.NoError(t, err)
	require.Nil(t, creditResp.ErrorMessage)
	require.NotNil(t, creditResp.Profile)
	require.True(t, creditResp.Profile.EnvCreditCleared)
	require.Zero(t, creditResp.Profile.CreditScore)
	require.EqualValues(t, 10, creditResp.Profile.StarRatingTenths)
	require.EqualValues(t, 1, creditResp.Profile.ViolationCount)
	require.Equal(t, string(commerce.SellerRiskTierRestricted), creditResp.Profile.RiskTier)

	// 2) GetMarketReferencePrice：返回真实近 7 日 p50 与有效样本量
	p50Resp, err := logic.GetMarketReferencePrice(&tradepb.GetMarketReferencePriceRequest{
		ItemConfigId: 3403,
		CurrencyType: currencytype.Gold,
	})
	require.NoError(t, err)
	require.Nil(t, p50Resp.ErrorMessage)
	require.EqualValues(t, 24_800, p50Resp.MedianUnitPrice)
	require.EqualValues(t, 26, p50Resp.SampleCount)
	require.False(t, p50Resp.InsufficientSample)
	require.EqualValues(t, 7, p50Resp.WindowDays)

	// 3) GetPlayerTradeQuota：返回方案 A 与今日剩余额度 7,500 / 10,000
	quotaResp, err := logic.GetPlayerTradeQuota(&tradepb.GetPlayerTradeQuotaRequest{CurrencyType: currencytype.Gold})
	require.NoError(t, err)
	require.Nil(t, quotaResp.ErrorMessage)
	require.Equal(t, commerce.PlayerTradeModeStrictConsumables, quotaResp.Mode)
	require.EqualValues(t, 10_000, quotaResp.DailyCurrencyLimit)
	require.EqualValues(t, 2_500, quotaResp.DailyCurrencyUsed)
	require.EqualValues(t, 7_500, quotaResp.DailyCurrencyRemaining)
	require.Equal(t, []uint32{501, 502}, quotaResp.ConsumableItemIds)
}
