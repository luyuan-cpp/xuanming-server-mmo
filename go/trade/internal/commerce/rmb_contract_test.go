package commerce

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tradepb "proto/trade"

	"github.com/stretchr/testify/require"
)

func TestMallRmbRegisterWireContractWithTradePrefixAndDesignatedBuyer(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/internal/consignments/register", r.URL.Path)
		require.Equal(t, "trade", r.Header.Get("X-Commerce-Subject"))
		require.NotEmpty(t, r.Header.Get("X-Commerce-Signature"))
		require.NoError(t, json.NewDecoder(r.Body).Decode(&captured))
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"listingId":              "8802",
			"sellerPlayerId":         "1002",
			"marketZone":             1,
			"assetKind":              "ITEMS",
			"title":                  "玄铁材料包",
			"priceFen":               200,
			"feeBps":                 500,
			"coolingMs":              259200000,
			"noticeEndMs":            1760000000000,
			"saleEndMs":              1760604800000,
			"state":                  "LISTED",
			"phase":                  "ON_SALE",
			"orderId":                "",
			"designatedBuyerAccount": "designated-buyer-acct",
		}))
	}))
	t.Cleanup(srv.Close)

	mall, err := NewMall(srv.URL, strings.Repeat("g", 32), strings.Repeat("d", 32), nil)
	require.NoError(t, err)

	rec := &tradepb.TradeCommerceRecord{CommerceId: 8802, SellerPlayerId: 1002}
	wf := &tradepb.TradeCommerceWorkflow{
		Rmb: &tradepb.TradeRmbConsignmentWorkflow{
			SellerAccount:   "seller-acct",
			MarketZone:      1,
			AssetKind:       "ITEMS",
			EscrowReference: "trade:8802",
			EscrowProof:     strings.Repeat("e", 64),
			PriceFen:        200,
			Title:           "玄铁材料包",
		},
	}

	view, err := mall.rmbRegister(context.Background(), rec, wf)
	require.NoError(t, err)
	require.Equal(t, "trade:8802", captured["escrowReference"], "G-1: Go 侧发送规范 trade:<uint64> 托管引用")
	require.Equal(t, "8802", captured["listingId"])
	require.EqualValues(t, 8802, view.ListingId)
	require.Equal(t, "ON_SALE", view.Phase)
	require.EqualValues(t, 1760000000000, view.NoticeEndMs)
}
