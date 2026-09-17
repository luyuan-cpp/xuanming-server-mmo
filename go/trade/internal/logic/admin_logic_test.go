package logic

import (
	"context"
	"errors"
	"testing"

	"trade/internal/config"
	"trade/internal/constants"

	tradepb "proto/trade"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/service"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (f *fixture) admin() *AdminLogic {
	// 内部调用不带会话:SeedListing 不依赖会话身份。
	return NewAdminLogic(context.Background(), f.deps)
}

func TestSeedListingRejectedOutsideDevAndTest(t *testing.T) {
	for _, mode := range []string{service.ProMode, service.PreMode, service.RtMode, ""} {
		t.Run("mode="+mode, func(t *testing.T) {
			f := newFixture(t, config.ScopeZone)
			f.deps.Config.Mode = mode

			resp, err := f.admin().SeedListing(validSeedRequest())

			assert.Nil(t, resp)
			assert.Equal(t, codes.PermissionDenied, status.Code(err), "非 dev/test 必须回 gRPC PermissionDenied")
			assert.Empty(t, f.store.calls)
			assert.Empty(t, f.homes.calls)
			assert.Zero(t, f.ids.calls, "被拒绝的调用不许消耗号段")
		})
	}
}

func TestSeedListingAllowedInTestMode(t *testing.T) {
	f := newFixture(t, config.ScopeZone)
	f.deps.Config.Mode = service.TestMode

	resp, err := f.admin().SeedListing(validSeedRequest())

	require.NoError(t, err)
	assert.Zero(t, resp.GetErrorMessage().GetId())
	assert.Len(t, f.store.insertedListings, 1)
}

func TestSeedListingValidationIsInBand(t *testing.T) {
	f := newFixture(t, config.ScopeZone)
	in := validSeedRequest()
	in.PriceFen = 0

	resp, err := f.admin().SeedListing(in)

	require.NoError(t, err)
	assert.Equal(t, constants.ErrInvalidParameter, resp.GetErrorMessage().GetId())
	assert.Empty(t, f.store.calls)
	assert.Empty(t, f.homes.calls)
	assert.Zero(t, f.ids.calls)
}

func TestSeedListingSuccess(t *testing.T) {
	f := newFixture(t, config.ScopeZone)
	f.homes.zones[playerC] = 7 // 市场分区必须来自 home_zone 查询
	in := validSeedRequest()
	in.SellerPlayerId = playerC
	in.NoticeDurationMs = hourMs
	in.SaleDurationMs = 2 * hourMs

	resp, err := f.admin().SeedListing(in)

	require.NoError(t, err)
	require.Zero(t, resp.GetErrorMessage().GetId())
	assert.Equal(t, uint64(5001), resp.GetListingId())
	assert.Equal(t, uint32(7), resp.GetMarketZone())
	assert.Equal(t, []uint64{playerC}, f.homes.calls)
	assert.Equal(t, 1, f.ids.calls)

	require.Len(t, f.store.insertedListings, 1)
	rec := f.store.insertedListings[0]
	assert.Equal(t, uint64(5001), rec.GetListingId())
	assert.Equal(t, playerC, rec.GetSellerPlayerId())
	assert.Empty(t, rec.GetSellerAccount())
	assert.Equal(t, uint32(7), rec.GetMarketZone())
	assert.Equal(t, uint32(7), rec.GetSellerZoneAtListing())
	assert.Equal(t, tradepb.ListingStatus_LISTING_STATUS_LISTED, rec.GetStatus())
	assert.Equal(t, testNowMs+hourMs, rec.GetNoticeEndMs(), "notice_end = now + notice")
	assert.Equal(t, testNowMs+3*hourMs, rec.GetSaleEndMs(), "sale_end = notice_end + sale")
	assert.Equal(t, testNowMs, rec.GetCreatedMs())
	assert.Equal(t, testNowMs, rec.GetUpdatedMs())
	assert.Zero(t, rec.GetVersion())
	assert.Equal(t, in.GetTitle(), rec.GetTitle())
	assert.Equal(t, in.GetCategory(), rec.GetCategory())
	assert.Equal(t, in.GetSubcategory(), rec.GetSubcategory())
	assert.Equal(t, in.GetPriceFen(), rec.GetPriceFen())
	assert.Equal(t, in.GetLevel(), rec.GetLevel())
	assert.Equal(t, in.GetSummary(), rec.GetSummary())
	assert.Equal(t, in.GetDescription(), rec.GetDescription())
	assert.Equal(t, in.GetIconKey(), rec.GetIconKey())
	assert.Equal(t, tradepb.ListingPhase_LISTING_PHASE_PUBLIC_NOTICE, Phase(rec, testNowMs))
}

func TestSeedListingWithoutNoticeIsImmediatelyOnSale(t *testing.T) {
	f := newFixture(t, config.ScopeZone)

	resp, err := f.admin().SeedListing(validSeedRequest()) // notice 0,sale 1 小时

	require.NoError(t, err)
	require.Zero(t, resp.GetErrorMessage().GetId())
	rec := f.store.insertedListings[0]
	assert.Equal(t, testNowMs, rec.GetNoticeEndMs(), "无公示期:notice_end 等于上架时刻")
	assert.Equal(t, tradepb.ListingPhase_LISTING_PHASE_ON_SALE, Phase(rec, testNowMs))
}

func TestSeedListingFailures(t *testing.T) {
	cases := []struct {
		name       string
		prepare    func(f *fixture, in *tradepb.SeedListingRequest)
		wantCode   uint32
		wantMinted bool
	}{
		{"卖家 home_zone 未映射", func(f *fixture, in *tradepb.SeedListingRequest) { in.SellerPlayerId = playerU },
			constants.ErrHomeZoneUnknown, false},
		{"home_zone 查询故障", func(f *fixture, _ *tradepb.SeedListingRequest) { f.homes.err = errors.New("boom") },
			constants.ErrServiceUnavailable, false},
		{"号段未接线", func(f *fixture, _ *tradepb.SeedListingRequest) { f.deps.ListingIDs = nil },
			constants.ErrServiceUnavailable, false},
		{"号段故障", func(f *fixture, _ *tradepb.SeedListingRequest) { f.ids.err = errors.New("segment unavailable") },
			constants.ErrServiceUnavailable, true},
		{"号段返回 0", func(f *fixture, _ *tradepb.SeedListingRequest) { f.ids.id = 0 },
			constants.ErrServiceUnavailable, true},
		{"插入故障", func(f *fixture, _ *tradepb.SeedListingRequest) { f.store.failOn["InsertListing"] = errStoreDown },
			constants.ErrServiceUnavailable, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, config.ScopeZone)
			in := validSeedRequest()
			tc.prepare(f, in)

			resp, err := f.admin().SeedListing(in)

			require.NoError(t, err, "业务 / 故障结果必须 in-band")
			assert.Equal(t, tc.wantCode, resp.GetErrorMessage().GetId())
			assert.Zero(t, resp.GetListingId())
			assert.Equal(t, tc.wantMinted, f.ids.calls > 0)
			assert.Empty(t, f.store.insertedListings)
		})
	}
}
