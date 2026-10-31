package logic

import (
	"context"
	"errors"
	tradepb "proto/trade"
	"shared/generated/pb/table"
	"shared/roleidentity"
	"strconv"
	"strings"
	"trade/internal/commerce"
	"trade/internal/constants"
	"trade/internal/data"
	"trade/internal/session"
)

func commerceCode(err error) uint32 {
	if err == nil {
		return 0
	}
	var r commerce.Rejection
	if errors.As(err, &r) {
		return uint32(r)
	}
	return constants.ErrServiceUnavailable
}
func orderView(r *tradepb.TradeCommerceRecord) *tradepb.CommerceOrderView {
	if r == nil {
		return nil
	}
	w, _ := data.Workflow(r)
	v := &tradepb.CommerceOrderView{OrderId: strconv.FormatUint(r.CommerceId, 10), Kind: tradepb.CommerceOrderKind(r.Kind), State: tradepb.CommerceOrderState(r.Status), SellerPlayerId: r.SellerPlayerId, BuyerPlayerId: r.BuyerPlayerId, Amount: r.UnitPrice, Currency: strconv.FormatUint(uint64(r.CurrencyType), 10), CreatedMs: r.CreatedMs, Version: r.Version, RelatedId: strconv.FormatUint(r.ParentId, 10)}
	if w != nil {
		v.SellerConfirmed = w.SellerConfirmed
		v.BuyerConfirmed = w.BuyerConfirmed
		if r.Kind == 1 {
			v.OrderId = w.ExternalOrderSn
			v.RelatedId = strconv.FormatUint(w.SkuId, 10)
			v.Amount = w.ExternalAmount
			v.Currency = w.ExternalCurrency
		}
	}
	if r.Kind == commerce.BidKind {
		v.Kind = tradepb.CommerceOrderKind_COMMERCE_ORDER_KIND_AUCTION_LOT
	}
	return v
}
func orderAnswer(r *tradepb.TradeCommerceRecord, err error) *tradepb.CommerceOrderResponse {
	out := &tradepb.CommerceOrderResponse{ErrorMessage: tipOf(commerceCode(err)), Order: orderView(r)}
	if r == nil || err != nil {
		return out
	}
	w, decodeErr := data.Workflow(r)
	if decodeErr != nil {
		return &tradepb.CommerceOrderResponse{ErrorMessage: tipOf(constants.ErrServiceUnavailable)}
	}
	out.SellerAssets, out.BuyerAssets = w.SellerAssets, w.BuyerAssets
	for _, l := range w.Legs {
		if l.Outcome != 2 || l.Snapshot == nil {
			continue
		}
		var items []*tradepb.TradeOfferInstance
		for _, i := range l.Snapshot.Items {
			items = append(items, &tradepb.TradeOfferInstance{InstanceId: i.ItemUuid, ConfigId: i.ConfigId, Count: i.StackSize})
		}
		var pet *tradepb.TradeOfferInstance
		if p := l.Snapshot.Pet; p != nil {
			pet = &tradepb.TradeOfferInstance{InstanceId: p.PetId, ConfigId: p.PetTableId, Name: p.Name, Level: p.Level}
		}
		if l.Role == "seller_debit" {
			out.SellerItemInstances, out.SellerPet = items, pet
		}
		if l.Role == "buyer_debit" {
			out.BuyerItemInstances, out.BuyerPet = items, pet
		}
	}
	return out
}
func marketView(r *tradepb.TradeCommerceRecord) *tradepb.MarketOrderView {
	if r == nil {
		return nil
	}
	w, _ := data.Workflow(r)
	state := ""
	if w != nil {
		state = w.Stage
	}
	return &tradepb.MarketOrderView{MarketOrderId: strconv.FormatUint(r.CommerceId, 10), MarketId: r.MarketId, ItemConfigId: r.ItemConfigId, Side: tradepb.MarketOrderSide(r.Side), Quantity: r.Quantity, FilledQuantity: r.FilledQuantity, UnitPrice: r.UnitPrice, CurrencyType: r.CurrencyType, State: state, CreatedMs: r.CreatedMs}
}
func lotView(r *tradepb.TradeCommerceRecord) *tradepb.AuctionLotView {
	if r == nil {
		return nil
	}
	w, _ := data.Workflow(r)
	if w == nil {
		return nil
	}
	return &tradepb.AuctionLotView{LotId: strconv.FormatUint(r.CommerceId, 10), SellerPlayerId: r.SellerPlayerId, CurrencyType: r.CurrencyType, OpeningPrice: r.UnitPrice, MinimumIncrement: w.MinimumIncrement, HighestBid: w.HighestBid, HighestBidder: w.HighestBidder, EndMs: r.ExpiresMs, State: w.Stage, Version: r.Version, OrderId: strconv.FormatUint(r.CommerceId, 10)}
}
func (l *JubaozhaiLogic) commerceCall(fn func(context.Context, uint64, *commerce.Engine) (*tradepb.TradeCommerceRecord, error)) (*tradepb.CommerceOrderResponse, error) {
	l, cancel := l.withRequestBudget()
	defer cancel()
	me, ok := session.ClientPlayerID(l.ctx)
	if !ok {
		return orderAnswer(nil, commerce.ErrInvalid), nil
	}
	if l.deps.Commerce == nil {
		return &tradepb.CommerceOrderResponse{ErrorMessage: tipOf(constants.ErrFeatureDisabled)}, nil
	}
	owned, identityErr := l.actorIdentity()
	if identityErr != nil {
		return orderAnswer(nil, identityErr), nil
	}
	l.ctx, identityErr = roleidentity.Capture(l.ctx, owned)
	if identityErr != nil {
		return orderAnswer(nil, identityErr), nil
	}
	r, err := fn(l.ctx, me, l.deps.Commerce)
	if err != nil && commerceCode(err) == constants.ErrServiceUnavailable {
		l.Errorf("交易业务调用失败: %v", err)
	}
	out := orderAnswer(r, err)
	if err == nil && r != nil {
		for _, pair := range []struct {
			selection *tradepb.TradeAssetSelection
			instances *[]*tradepb.TradeOfferInstance
		}{{out.SellerAssets, &out.SellerItemInstances}, {out.BuyerAssets, &out.BuyerItemInstances}} {
			for id, count := range pair.selection.GetItems() {
				*pair.instances = append(*pair.instances, &tradepb.TradeOfferInstance{ConfigId: id, Count: count, Name: l.deps.Commerce.Policy.ItemTitles[id]})
			}
			for _, instance := range *pair.instances {
				if instance.Name == "" {
					instance.Name = l.deps.Commerce.Policy.ItemTitles[instance.ConfigId]
				}
			}
		}
	}
	out.ServerNowMs = nowMillis(l.deps.Now)
	return out, nil
}
func (l *JubaozhaiLogic) CreatePlayerTrade(in *tradepb.CreatePlayerTradeRequest) (*tradepb.CommerceOrderResponse, error) {
	return l.commerceCall(func(ctx context.Context, me uint64, e *commerce.Engine) (*tradepb.TradeCommerceRecord, error) {
		zone, err := l.deps.HomeZones.HomeZone(ctx, in.GetBuyerPlayerId())
		if err != nil {
			return nil, err
		}
		if zone == 0 {
			return nil, commerce.Rejection(table.TradeError_kTradeCounterpartyUnknown)
		}
		return e.CreatePlayerTrade(ctx, me, in)
	})
}
func (l *JubaozhaiLogic) ConfirmPlayerTrade(in *tradepb.ConfirmPlayerTradeRequest) (*tradepb.CommerceOrderResponse, error) {
	return l.commerceCall(func(ctx context.Context, me uint64, e *commerce.Engine) (*tradepb.TradeCommerceRecord, error) {
		id, err := strconv.ParseUint(in.GetOrderId(), 10, 64)
		if err != nil {
			return nil, commerce.ErrInvalid
		}
		return e.Mutate(ctx, me, id, "confirm")
	})
}
func (l *JubaozhaiLogic) CreateConsignment(in *tradepb.CreateConsignmentRequest) (*tradepb.CommerceOrderResponse, error) {
	return l.commerceCall(func(ctx context.Context, me uint64, e *commerce.Engine) (*tradepb.TradeCommerceRecord, error) {
		return e.CreateConsignment(ctx, me, in)
	})
}
func (l *JubaozhaiLogic) BuyConsignment(in *tradepb.BuyConsignmentRequest) (*tradepb.CommerceOrderResponse, error) {
	return l.commerceCall(func(ctx context.Context, me uint64, e *commerce.Engine) (*tradepb.TradeCommerceRecord, error) {
		id, err := strconv.ParseUint(in.GetListingId(), 10, 64)
		if err != nil || len(in.GetClientRequestId()) < 8 {
			return nil, commerce.ErrInvalid
		}
		return e.Mutate(ctx, me, id, "buy", in.GetClientRequestId())
	})
}
func (l *JubaozhaiLogic) CancelConsignment(in *tradepb.CancelConsignmentRequest) (*tradepb.CommerceOrderResponse, error) {
	return l.cancelCommerce(in.GetListingId())
}
func (l *JubaozhaiLogic) CancelCommerceOrder(in *tradepb.CancelCommerceOrderRequest) (*tradepb.CommerceOrderResponse, error) {
	if strings.Contains(in.GetOrderId(), "-") {
		return l.mallOrder(in.GetOrderId(), true)
	}
	return l.cancelCommerce(in.GetOrderId())
}
func (l *JubaozhaiLogic) cancelCommerce(value string) (*tradepb.CommerceOrderResponse, error) {
	return l.commerceCall(func(ctx context.Context, me uint64, e *commerce.Engine) (*tradepb.TradeCommerceRecord, error) {
		id, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return nil, commerce.ErrInvalid
		}
		return e.Mutate(ctx, me, id, "cancel")
	})
}
func (l *JubaozhaiLogic) GetCommerceOrder(in *tradepb.GetCommerceOrderRequest) (*tradepb.CommerceOrderResponse, error) {
	if strings.Contains(in.GetOrderId(), "-") {
		return l.mallOrder(in.GetOrderId(), false)
	}
	return l.commerceCall(func(ctx context.Context, me uint64, e *commerce.Engine) (*tradepb.TradeCommerceRecord, error) {
		id, err := strconv.ParseUint(in.GetOrderId(), 10, 64)
		if err != nil {
			return nil, commerce.ErrInvalid
		}
		return e.Get(ctx, me, id)
	})
}
func (l *JubaozhaiLogic) mallOrder(id string, cancelOrder bool) (*tradepb.CommerceOrderResponse, error) {
	l, cancel := l.withRequestBudget()
	defer cancel()
	me, ok := session.ClientPlayerID(l.ctx)
	if !ok {
		return orderAnswer(nil, commerce.ErrInvalid), nil
	}
	if l.deps.Mall == nil {
		return &tradepb.CommerceOrderResponse{ErrorMessage: tipOf(constants.ErrFeatureDisabled)}, nil
	}
	owned, err := l.actorIdentity()
	if err != nil {
		return orderAnswer(nil, err), nil
	}
	l.ctx, err = roleidentity.Capture(l.ctx, owned)
	if err != nil {
		return orderAnswer(nil, err), nil
	}
	out, err := l.deps.Mall.Order(l.ctx, me, id, cancelOrder)
	if err != nil {
		return orderAnswer(nil, err), nil
	}
	return out, nil
}
func (l *JubaozhaiLogic) CreateOfficialOrder(in *tradepb.CreateOfficialOrderRequest) (*tradepb.CommerceOrderResponse, error) {
	l, cancel := l.withRequestBudget()
	defer cancel()
	me, ok := session.ClientPlayerID(l.ctx)
	if !ok {
		return orderAnswer(nil, commerce.ErrInvalid), nil
	}
	if l.deps.Mall == nil {
		return &tradepb.CommerceOrderResponse{ErrorMessage: tipOf(constants.ErrFeatureDisabled)}, nil
	}
	owned, err := l.actorIdentity()
	if err != nil {
		return orderAnswer(nil, err), nil
	}
	l.ctx, err = roleidentity.Capture(l.ctx, owned)
	if err != nil {
		return orderAnswer(nil, err), nil
	}
	out, err := l.deps.Mall.Create(l.ctx, me, in)
	if err != nil {
		return orderAnswer(nil, err), nil
	}
	return out, nil
}
func (l *JubaozhaiLogic) ListOfficialSkus(in *tradepb.ListOfficialSkusRequest) (*tradepb.ListOfficialSkusResponse, error) {
	l, cancel := l.withRequestBudget()
	defer cancel()
	me, ok := session.ClientPlayerID(l.ctx)
	if !ok {
		return &tradepb.ListOfficialSkusResponse{ErrorMessage: tipOf(constants.ErrInvalidParameter)}, nil
	}
	if l.deps.Mall == nil {
		return &tradepb.ListOfficialSkusResponse{ErrorMessage: tipOf(constants.ErrFeatureDisabled)}, nil
	}
	out, err := l.deps.Mall.Skus(l.ctx, me, in.GetAfterSkuId(), in.GetLimit())
	if err != nil {
		return &tradepb.ListOfficialSkusResponse{ErrorMessage: tipOf(commerceCode(err))}, nil
	}
	return out, nil
}
func (l *JubaozhaiLogic) ListCommerceOrders(in *tradepb.ListCommerceOrdersRequest) (*tradepb.ListCommerceOrdersResponse, error) {
	l, cancel := l.withRequestBudget()
	defer cancel()
	me, ok := session.ClientPlayerID(l.ctx)
	if !ok {
		return &tradepb.ListCommerceOrdersResponse{ErrorMessage: tipOf(constants.ErrInvalidParameter)}, nil
	}
	if l.deps.Commerce == nil {
		return &tradepb.ListCommerceOrdersResponse{ErrorMessage: tipOf(constants.ErrFeatureDisabled)}, nil
	}
	owned, identityErr := l.actorIdentity()
	if identityErr != nil {
		return &tradepb.ListCommerceOrdersResponse{ErrorMessage: tipOf(commerceCode(identityErr))}, nil
	}
	l.ctx, identityErr = roleidentity.Capture(l.ctx, owned)
	if identityErr != nil {
		return &tradepb.ListCommerceOrdersResponse{ErrorMessage: tipOf(commerceCode(identityErr))}, nil
	}
	limit := min(max(int(in.GetLimit()), 1), 100)
	if l.deps.Mall != nil && (in.GetCursor() == "" || strings.HasPrefix(in.GetCursor(), "mall:")) {
		cursor := strings.TrimPrefix(in.GetCursor(), "mall:")
		orders, err := l.deps.Mall.List(l.ctx, me, cursor, uint32(limit))
		if err != nil {
			return &tradepb.ListCommerceOrdersResponse{ErrorMessage: tipOf(commerceCode(err))}, nil
		}
		if len(orders) > 0 {
			return &tradepb.ListCommerceOrdersResponse{Orders: orders, NextCursor: "mall:" + orders[len(orders)-1].OrderId, ServerNowMs: nowMillis(l.deps.Now)}, nil
		}
	}
	cursor := in.GetCursor()
	if strings.HasPrefix(cursor, "mall:") {
		cursor = ""
	}
	rows, err := l.deps.Commerce.Repo.List(l.ctx, me, data.ParseCommerceCursor(cursor), limit)
	out := &tradepb.ListCommerceOrdersResponse{ErrorMessage: tipOf(commerceCode(err)), ServerNowMs: nowMillis(l.deps.Now)}
	for _, r := range rows {
		out.NextCursor = data.CommerceCursorOf(r).String()
		workflow, decodeErr := data.Workflow(r)
		if decodeErr == nil && commerce.OwnsRecord(l.ctx, r, workflow) {
			out.Orders = append(out.Orders, orderView(r))
		}
	}
	return out, nil
}
func (l *JubaozhaiLogic) PlaceMarketOrder(in *tradepb.PlaceMarketOrderRequest) (*tradepb.MarketOrderResponse, error) {
	answer, err := l.commerceCall(func(ctx context.Context, me uint64, e *commerce.Engine) (*tradepb.TradeCommerceRecord, error) {
		expected := uint32(1)
		if in.GetMarketId() != expected {
			return nil, commerce.ErrInvalid
		}
		return e.PlaceMarket(ctx, me, in)
	})
	out := &tradepb.MarketOrderResponse{ErrorMessage: answer.ErrorMessage}
	if answer.Order != nil {
		id, _ := strconv.ParseUint(answer.Order.OrderId, 10, 64)
		r, readErr := l.deps.Commerce.Repo.Get(l.ctx, id)
		if readErr != nil {
			out.ErrorMessage = tipOf(constants.ErrServiceUnavailable)
		} else {
			out.Order = marketView(r)
		}
	}
	return out, err
}
func (l *JubaozhaiLogic) CancelMarketOrder(in *tradepb.CancelMarketOrderRequest) (*tradepb.MarketOrderResponse, error) {
	var r *tradepb.TradeCommerceRecord
	a, err := l.commerceCall(func(ctx context.Context, me uint64, e *commerce.Engine) (*tradepb.TradeCommerceRecord, error) {
		id, err := strconv.ParseUint(in.GetMarketOrderId(), 10, 64)
		if err != nil {
			return nil, commerce.ErrInvalid
		}
		r, err = e.Mutate(ctx, me, id, "cancel")
		return r, err
	})
	return &tradepb.MarketOrderResponse{ErrorMessage: a.ErrorMessage, Order: marketView(r)}, err
}
func (l *JubaozhaiLogic) ListMarketOrders(in *tradepb.ListMarketOrdersRequest) (*tradepb.ListMarketOrdersResponse, error) {
	l, cancel := l.withRequestBudget()
	defer cancel()
	me, ok := session.ClientPlayerID(l.ctx)
	if !ok {
		return &tradepb.ListMarketOrdersResponse{ErrorMessage: tipOf(constants.ErrInvalidParameter)}, nil
	}
	if l.deps.Commerce == nil {
		return &tradepb.ListMarketOrdersResponse{ErrorMessage: tipOf(constants.ErrFeatureDisabled)}, nil
	}
	after := data.ParseCommerceCursor(in.GetCursor())
	limit := min(max(int(in.GetLimit()), 1), 100)
	rows, err := l.deps.Commerce.Repo.Query(l.ctx, "SELECT "+data.CommerceColumns+" FROM trade_commerce WHERE kind=4 AND side<>0 AND seller_player_id=? AND (created_ms>? OR (created_ms=? AND commerce_id>?)) ORDER BY created_ms,commerce_id LIMIT ?", me, after.CreatedMs, after.CreatedMs, after.CommerceId, limit)
	out := &tradepb.ListMarketOrdersResponse{ErrorMessage: tipOf(commerceCode(err))}
	for _, r := range rows {
		out.Orders = append(out.Orders, marketView(r))
		out.NextCursor = data.CommerceCursorOf(r).String()
	}
	return out, nil
}
func (l *JubaozhaiLogic) GetMarketDepth(in *tradepb.GetMarketDepthRequest) (*tradepb.GetMarketDepthResponse, error) {
	l, cancel := l.withRequestBudget()
	defer cancel()
	me, ok := session.ClientPlayerID(l.ctx)
	if !ok || l.deps.Commerce == nil {
		return &tradepb.GetMarketDepthResponse{ErrorMessage: tipOf(constants.ErrFeatureDisabled)}, nil
	}
	_ = me
	expected := uint32(1)
	if in.GetMarketId() != expected || len(l.deps.Commerce.Policy.Currencies) == 0 {
		return &tradepb.GetMarketDepthResponse{ErrorMessage: tipOf(constants.ErrInvalidParameter)}, nil
	}
	out := &tradepb.GetMarketDepthResponse{ServerNowMs: nowMillis(l.deps.Now)}
	limit := min(max(int(in.GetLimit()), 1), 50)
	for _, side := range []uint32{1, 2} {
		order := "ASC"
		if side == 1 {
			order = "DESC"
		}
		rows, err := l.deps.Commerce.Repo.DB.QueryContext(l.ctx, "SELECT unit_price,SUM(quantity-filled_quantity) FROM trade_commerce WHERE kind=4 AND side=? AND market_id=? AND item_config_id=? AND currency_type=? AND status=3 AND filled_quantity<quantity GROUP BY unit_price ORDER BY unit_price "+order+" LIMIT ?", side, expected, in.GetItemConfigId(), l.deps.Commerce.Policy.Currencies[0], limit)
		if err != nil {
			out.ErrorMessage = tipOf(constants.ErrServiceUnavailable)
			return out, nil
		}
		for rows.Next() {
			v := &tradepb.MarketDepthLevel{}
			if err := rows.Scan(&v.UnitPrice, &v.Quantity); err != nil {
				rows.Close()
				out.ErrorMessage = tipOf(constants.ErrServiceUnavailable)
				return out, nil
			}
			if side == 1 {
				out.Bids = append(out.Bids, v)
			} else {
				out.Asks = append(out.Asks, v)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			out.ErrorMessage = tipOf(constants.ErrServiceUnavailable)
			return out, nil
		}
	}
	return out, nil
}

// 全服材料市场目录来自服务端交易白名单，客户端不能另造地域市场或道具模板。
func (l *JubaozhaiLogic) ListTradeMarkets(_ *tradepb.ListTradeMarketsRequest) (*tradepb.ListTradeMarketsResponse, error) {
	if _, ok := session.ClientPlayerID(l.ctx); !ok {
		return &tradepb.ListTradeMarketsResponse{ErrorMessage: tipOf(constants.ErrInvalidParameter)}, nil
	}
	if l.deps.Commerce == nil {
		return &tradepb.ListTradeMarketsResponse{ErrorMessage: tipOf(constants.ErrFeatureDisabled)}, nil
	}
	out := &tradepb.ListTradeMarketsResponse{}
	for _, item := range l.deps.Commerce.Policy.Items {
		for index, currency := range l.deps.Commerce.Policy.Currencies {
			if index > 0 {
				break
			}
			out.Markets = append(out.Markets, &tradepb.TradeMarketView{MarketId: 1, ItemConfigId: item, Title: l.deps.Commerce.Policy.ItemTitles[item], CurrencyType: currency})
		}
	}
	return out, nil
}
func (l *JubaozhaiLogic) CreateAuctionLot(in *tradepb.CreateAuctionLotRequest) (*tradepb.AuctionLotResponse, error) {
	a, err := l.commerceCall(func(ctx context.Context, me uint64, e *commerce.Engine) (*tradepb.TradeCommerceRecord, error) {
		return e.CreateLot(ctx, me, in)
	})
	out := &tradepb.AuctionLotResponse{ErrorMessage: a.ErrorMessage, ServerNowMs: a.ServerNowMs}
	if a.Order != nil {
		id, _ := strconv.ParseUint(a.Order.OrderId, 10, 64)
		r, readErr := l.deps.Commerce.Repo.Get(l.ctx, id)
		if readErr != nil {
			out.ErrorMessage = tipOf(constants.ErrServiceUnavailable)
		} else {
			out.Lot = lotView(r)
		}
	}
	return out, err
}
func (l *JubaozhaiLogic) PlaceAuctionBid(in *tradepb.PlaceAuctionBidRequest) (*tradepb.AuctionLotResponse, error) {
	var lot *tradepb.TradeCommerceRecord
	a, err := l.commerceCall(func(ctx context.Context, me uint64, e *commerce.Engine) (*tradepb.TradeCommerceRecord, error) {
		id, err := strconv.ParseUint(in.GetLotId(), 10, 64)
		if err != nil {
			return nil, commerce.ErrInvalid
		}
		r, err := e.PlaceBid(ctx, me, id, in)
		if err != nil {
			return nil, err
		}
		lot, err = e.Get(ctx, me, id)
		return r, err
	})
	if a.ErrorMessage.GetId() == 0 {
		a.ErrorMessage = tipOf(uint32(table.TradeError_kTradeAssetDeliveryPending))
	}
	return &tradepb.AuctionLotResponse{ErrorMessage: a.ErrorMessage, Lot: lotView(lot), ServerNowMs: a.ServerNowMs}, err
}
func (l *JubaozhaiLogic) GetAuctionLot(in *tradepb.GetAuctionLotRequest) (*tradepb.AuctionLotResponse, error) {
	var record *tradepb.TradeCommerceRecord
	a, err := l.commerceCall(func(ctx context.Context, me uint64, e *commerce.Engine) (*tradepb.TradeCommerceRecord, error) {
		id, err := strconv.ParseUint(in.GetLotId(), 10, 64)
		if err != nil {
			return nil, commerce.ErrInvalid
		}
		record, err = e.Get(ctx, me, id)
		if err == nil && record.Kind != 5 {
			return nil, commerce.ErrMissing
		}
		return record, err
	})
	return &tradepb.AuctionLotResponse{ErrorMessage: a.ErrorMessage, Lot: lotView(record), ServerNowMs: a.ServerNowMs}, err
}
func (l *JubaozhaiLogic) CancelAuctionLot(in *tradepb.CancelAuctionLotRequest) (*tradepb.AuctionLotResponse, error) {
	var r *tradepb.TradeCommerceRecord
	a, err := l.commerceCall(func(ctx context.Context, me uint64, e *commerce.Engine) (*tradepb.TradeCommerceRecord, error) {
		id, err := strconv.ParseUint(in.GetLotId(), 10, 64)
		if err != nil {
			return nil, commerce.ErrInvalid
		}
		r, err = e.Mutate(ctx, me, id, "cancel")
		return r, err
	})
	return &tradepb.AuctionLotResponse{ErrorMessage: a.ErrorMessage, Lot: lotView(r), ServerNowMs: a.ServerNowMs}, err
}

func (l *JubaozhaiLogic) BrowseCommerceOffers(in *tradepb.BrowseCommerceOffersRequest) (*tradepb.BrowseCommerceOffersResponse, error) {
	l, cancel := l.withRequestBudget()
	defer cancel()
	if _, ok := session.ClientPlayerID(l.ctx); !ok {
		return &tradepb.BrowseCommerceOffersResponse{ErrorMessage: tipOf(constants.ErrInvalidParameter)}, nil
	}
	if l.deps.Commerce == nil {
		return &tradepb.BrowseCommerceOffersResponse{ErrorMessage: tipOf(constants.ErrFeatureDisabled)}, nil
	}
	if in.GetKind() != 3 && in.GetKind() != 5 {
		return &tradepb.BrowseCommerceOffersResponse{ErrorMessage: tipOf(constants.ErrInvalidParameter)}, nil
	}
	after, _ := strconv.ParseUint(in.GetCursor(), 10, 64)
	limit := min(max(int(in.GetLimit()), 1), 100)
	now := nowMillis(l.deps.Now)
	rows, err := l.deps.Commerce.Repo.Query(l.ctx, "SELECT "+data.CommerceColumns+" FROM trade_commerce WHERE kind=? AND status=3 AND commerce_id>? AND expires_ms>? ORDER BY commerce_id LIMIT ?", in.GetKind(), after, now, limit)
	out := &tradepb.BrowseCommerceOffersResponse{ErrorMessage: tipOf(commerceCode(err)), ServerNowMs: now}
	for _, r := range rows {
		out.NextCursor = strconv.FormatUint(r.CommerceId, 10)
		w, err := data.Workflow(r)
		if err != nil {
			out.ErrorMessage = tipOf(constants.ErrServiceUnavailable)
			return out, nil
		}
		if w.Stage != "open" && w.Stage != "lot_open" {
			continue
		}
		v := &tradepb.CommerceOfferView{Order: orderView(r), Assets: w.SellerAssets}
		for id, count := range w.SellerAssets.GetItems() {
			v.ItemInstances = append(v.ItemInstances, &tradepb.TradeOfferInstance{ConfigId: id, Count: count, Name: l.deps.Commerce.Policy.ItemTitles[id]})
		}
		for _, leg := range w.Legs {
			if leg.Role != "seller_debit" || leg.Snapshot == nil {
				continue
			}
			for _, item := range leg.Snapshot.Items {
				v.ItemInstances = append(v.ItemInstances, &tradepb.TradeOfferInstance{InstanceId: item.ItemUuid, ConfigId: item.ConfigId, Count: item.StackSize, Name: l.deps.Commerce.Policy.ItemTitles[item.ConfigId]})
			}
			if p := leg.Snapshot.Pet; p != nil {
				v.Pet = &tradepb.TradeOfferInstance{InstanceId: p.PetId, ConfigId: p.PetTableId, Name: p.Name, Level: p.Level}
			}
		}
		if r.Kind == 5 {
			v.Auction = lotView(r)
		}
		out.Offers = append(out.Offers, v)
	}
	return out, nil
}
