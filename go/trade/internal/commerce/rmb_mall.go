package commerce

import (
	"context"
	"errors"
	"strconv"

	tradepb "proto/trade"
)

// 以下是商城HTTP契约适配，不是玩家持久化模型；账号只由已认证的trade入口注入。
type rmbListing struct {
	ID        string `json:"listingId"`
	Seller    string `json:"sellerPlayerId"`
	Zone      uint32 `json:"marketZone"`
	Kind      string `json:"assetKind"`
	Title     string `json:"title"`
	Price     uint64 `json:"priceFen"`
	Fee       uint32 `json:"feeBps"`
	Cooling   uint64 `json:"coolingMs"`
	NoticeEnd uint64 `json:"noticeEndMs"`
	SaleEnd   uint64 `json:"saleEndMs"`
	State     string `json:"state"`
	Phase     string `json:"phase"`
	Order     string `json:"orderId"`
}

func canonicalID(raw string) (uint64, error) {
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 || strconv.FormatUint(id, 10) != raw {
		return 0, errors.New("商城身份不是规范uint64")
	}
	return id, nil
}
func (r rmbListing) view() (*tradepb.RmbConsignmentView, error) {
	id, err := canonicalID(r.ID)
	if err != nil {
		return nil, err
	}
	seller, err := canonicalID(r.Seller)
	if err != nil {
		return nil, err
	}
	if r.Zone == 0 || r.Price == 0 || r.Price > 9_000_000_000_000_000 || r.Fee > 10000 || r.NoticeEnd == 0 || r.SaleEnd <= r.NoticeEnd || len(r.Title) == 0 || !rmbKind(r.Kind) {
		return nil, errors.New("商城寄售条款应答非法")
	}
	switch r.State {
	case "LISTED", "LOCKED", "SOLD", "RETURNING", "RETURNED", "MANUAL":
	default:
		return nil, errors.New("未知商城寄售状态")
	}
	if r.Order != "" && !mallOrderID.MatchString(r.Order) {
		return nil, errors.New("寄售订单号非法")
	}
	return &tradepb.RmbConsignmentView{ListingId: id, SellerPlayerId: seller, MarketZone: r.Zone, AssetKind: r.Kind, Title: r.Title, PriceFen: r.Price, FeeBps: r.Fee, CoolingMs: r.Cooling, NoticeEndMs: r.NoticeEnd, SaleEndMs: r.SaleEnd, State: r.State, Phase: r.Phase, OrderId: r.Order}, nil
}
func rmbKind(kind string) bool {
	return kind == "ITEMS" || kind == "PET" || kind == "CURRENCY" || kind == "CHARACTER"
}
func (m *Mall) consignmentCall(ctx context.Context, action string, in, out any) error {
	if m == nil {
		return errors.New("人民币商城未装配")
	}
	return m.call(ctx, "delivery", "trade", "POST", "/internal/consignments/"+action, in, out)
}
func (m *Mall) RmbGet(ctx context.Context, id uint64) (*tradepb.RmbConsignmentView, error) {
	if id == 0 {
		return nil, ErrInvalid
	}
	var out rmbListing
	if err := m.consignmentCall(ctx, "get", map[string]any{"listingId": strconv.FormatUint(id, 10)}, &out); err != nil {
		return nil, err
	}
	v, err := out.view()
	if err == nil && v.ListingId != id {
		return nil, errors.New("商城返回了其它寄售")
	}
	return v, err
}
func (m *Mall) RmbList(ctx context.Context, after string, limit uint32) (*tradepb.ListRmbConsignmentsResponse, error) {
	if after != "" {
		if _, err := canonicalID(after); err != nil {
			return nil, ErrInvalid
		}
	}
	var out struct {
		Listings []rmbListing `json:"listings"`
	}
	if err := m.consignmentCall(ctx, "list", map[string]any{"afterListingId": after, "limit": min(max(limit, 1), 100)}, &out); err != nil {
		return nil, err
	}
	if out.Listings == nil || len(out.Listings) > int(min(max(limit, 1), 100)) {
		return nil, errors.New("寄售分页应答非法")
	}
	v := &tradepb.ListRmbConsignmentsResponse{}
	previous := after
	for _, r := range out.Listings {
		p, err := r.view()
		if err != nil {
			return nil, err
		}
		if r.ID <= previous {
			return nil, errors.New("寄售分页顺序非法")
		}
		previous = r.ID
		v.Listings = append(v.Listings, p)
		v.NextCursor = r.ID
	}
	return v, nil
}
func (m *Mall) rmbRegister(ctx context.Context, r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) (*tradepb.RmbConsignmentView, error) {
	t := w.Rmb
	var out rmbListing
	input := map[string]any{"listingId": strconv.FormatUint(r.CommerceId, 10), "sellerPlayerId": strconv.FormatUint(r.SellerPlayerId, 10), "sellerAccount": t.SellerAccount, "marketZone": t.MarketZone, "assetKind": t.AssetKind, "escrowReference": t.EscrowReference, "escrowProof": t.EscrowProof, "priceFen": t.PriceFen, "title": t.Title, "assetCurrencyType": nil}
	if t.AssetKind == "CURRENCY" {
		input["assetCurrencyType"] = w.SellerAssets.GetCurrencyType()
	}
	if err := m.consignmentCall(ctx, "register", input, &out); err != nil {
		return nil, err
	}
	v, err := out.view()
	if err != nil {
		return nil, err
	}
	if v.ListingId != r.CommerceId || v.SellerPlayerId != r.SellerPlayerId || v.MarketZone != t.MarketZone || v.PriceFen != t.PriceFen || v.Title != t.Title || v.AssetKind != t.AssetKind {
		return nil, errors.New("商城登记条款与原托管不符")
	}
	return v, nil
}
func (m *Mall) RmbCancel(ctx context.Context, account string, id uint64) (*tradepb.RmbConsignmentView, error) {
	var out rmbListing
	if err := m.consignmentCall(ctx, "cancel", map[string]any{"listingId": strconv.FormatUint(id, 10), "sellerAccount": account}, &out); err != nil {
		return nil, err
	}
	v, err := out.view()
	if err == nil && v.ListingId != id {
		return nil, errors.New("商城取消返回其它寄售")
	}
	return v, err
}
func rmbOrderAnswer(r MallOrder) (*tradepb.CommerceOrderResponse, error) {
	out, err := mallAnswer(r)
	if err != nil {
		return nil, err
	}
	if r.SKU != 0 || r.Quantity != 1 || r.Currency != "CNY" || r.Amount == 0 {
		return nil, errors.New("人民币订单条款非法")
	}
	out.Order.Kind = tradepb.CommerceOrderKind_COMMERCE_ORDER_KIND_RMB_CONSIGNMENT
	return out, nil
}
func (m *Mall) RmbBuy(ctx context.Context, account string, player, revision uint64, in *tradepb.BuyRmbConsignmentRequest) (*tradepb.CommerceOrderResponse, error) {
	if in.GetListingId() == 0 || len(in.GetClientRequestId()) < 8 || len(in.GetClientRequestId()) > 80 {
		return nil, ErrInvalid
	}
	var out MallOrder
	err := m.consignmentCall(ctx, "buy", map[string]any{"listingId": strconv.FormatUint(in.ListingId, 10), "buyerPlayerId": strconv.FormatUint(player, 10), "buyerAccount": account, "buyerOwnerRevision": strconv.FormatUint(revision, 10), "requestId": in.ClientRequestId}, &out)
	if err != nil {
		return nil, err
	}
	if out.BuyerID != strconv.FormatUint(player, 10) {
		return nil, errors.New("人民币订单买家不符")
	}
	return rmbOrderAnswer(out)
}
func (m *Mall) RmbOrder(ctx context.Context, account, id string, cancel bool) (*tradepb.CommerceOrderResponse, error) {
	if !mallOrderID.MatchString(id) {
		return nil, ErrInvalid
	}
	action := "orders/get"
	if cancel {
		action = "orders/cancel"
	}
	var out MallOrder
	if err := m.consignmentCall(ctx, action, map[string]any{"account": account, "orderId": id}, &out); err != nil {
		return nil, err
	}
	if out.OrderID != id {
		return nil, errors.New("人民币订单原件不符")
	}
	return rmbOrderAnswer(out)
}
func (m *Mall) RmbOrders(ctx context.Context, account, after string, limit uint32) (*tradepb.ListCommerceOrdersResponse, error) {
	if after != "" && !mallOrderID.MatchString(after) {
		return nil, ErrInvalid
	}
	var out []MallOrder
	if err := m.consignmentCall(ctx, "orders/list", map[string]any{"account": account, "afterOrderId": after, "limit": min(max(limit, 1), 100)}, &out); err != nil {
		return nil, err
	}
	if out == nil || len(out) > int(min(max(limit, 1), 100)) {
		return nil, errors.New("人民币订单页非法")
	}
	v := &tradepb.ListCommerceOrdersResponse{}
	for _, r := range out {
		p, err := rmbOrderAnswer(r)
		if err != nil {
			return nil, err
		}
		v.Orders = append(v.Orders, p.Order)
		v.NextCursor = r.OrderID
	}
	return v, nil
}
func (m *Mall) RmbIncomes(ctx context.Context, account, after string, limit uint32) (*tradepb.ListSellerIncomesResponse, error) {
	if after != "" && !mallOrderID.MatchString(after) {
		return nil, ErrInvalid
	}
	var out struct {
		Incomes []struct {
			Order     string `json:"orderId"`
			Gross     uint64 `json:"grossFen"`
			Fee       uint64 `json:"feeFen"`
			Net       uint64 `json:"netFen"`
			Reserved  uint64 `json:"reservedFen"`
			Paid      uint64 `json:"paidFen"`
			Forfeited uint64 `json:"forfeitedFen"`
			Available uint64 `json:"availableMs"`
		} `json:"incomes"`
	}
	if err := m.consignmentCall(ctx, "incomes", map[string]any{"account": account, "afterOrderId": after, "limit": min(max(limit, 1), 100)}, &out); err != nil {
		return nil, err
	}
	if out.Incomes == nil || len(out.Incomes) > int(min(max(limit, 1), 100)) {
		return nil, errors.New("收入页非法")
	}
	v := &tradepb.ListSellerIncomesResponse{}
	for _, r := range out.Incomes {
		if !mallOrderID.MatchString(r.Order) || r.Gross > 9_000_000_000_000_000 || r.Fee > r.Gross || r.Net != r.Gross-r.Fee || r.Paid > r.Net || r.Reserved > r.Net-r.Paid || r.Forfeited > r.Net-r.Paid-r.Reserved {
			return nil, errors.New("收入原件非法")
		}
		v.Incomes = append(v.Incomes, &tradepb.RmbSellerIncome{OrderId: r.Order, GrossFen: r.Gross, FeeFen: r.Fee, NetFen: r.Net, ReservedFen: r.Reserved, PaidFen: r.Paid, AvailableMs: r.Available})
		v.NextCursor = r.Order
	}
	return v, nil
}
func (m *Mall) RmbWithdrawal(ctx context.Context, account, request, id string, amount uint64) (*tradepb.WithdrawalResponse, error) {
	input := map[string]any{"account": account, "withdrawalId": id}
	action := "withdrawals/get"
	if request != "" {
		if len(request) < 8 || len(request) > 80 || amount == 0 || amount > 9_000_000_000_000_000 {
			return nil, ErrInvalid
		}
		action = "withdrawals/create"
		input = map[string]any{"account": account, "requestId": request, "amountFen": amount}
	} else if !mallOrderID.MatchString(id) {
		return nil, ErrInvalid
	}
	var out struct {
		ID      string `json:"withdrawalId"`
		Amount  uint64 `json:"amountFen"`
		State   string `json:"state"`
		Channel string `json:"channel"`
		Tx      string `json:"transactionId"`
	}
	if err := m.consignmentCall(ctx, action, input, &out); err != nil {
		return nil, err
	}
	if !mallOrderID.MatchString(out.ID) || out.Amount == 0 || out.Amount > 9_000_000_000_000_000 || request == "" && out.ID != id || request != "" && out.Amount != amount {
		return nil, errors.New("提现原件非法")
	}
	switch out.State {
	case "REQUESTED", "PAID", "FAILED":
	default:
		return nil, errors.New("未知提现状态")
	}
	return &tradepb.WithdrawalResponse{Withdrawal: &tradepb.RmbWithdrawalView{WithdrawalId: out.ID, AmountFen: out.Amount, State: out.State, Channel: out.Channel, TransactionId: out.Tx}}, nil
}
