package logic

import (
	"context"
	"errors"

	base "proto/common/base"
	dspb "proto/data_service"
	tradepb "proto/trade"
	"shared/roleidentity"
	"trade/internal/commerce"
	"trade/internal/constants"
	"trade/internal/data"
	"trade/internal/session"
)

type RmbIdentityResolver interface {
	Resolve(context.Context, *base.SessionDetails, uint64) (*dspb.AccountRoleOwnership, error)
}

func (l *JubaozhaiLogic) actorIdentity() (*dspb.AccountRoleOwnership, error) {
	if l.deps.RmbIdentity == nil {
		return nil, errors.New("原登录会话账号权威未装配")
	}
	return l.deps.RmbIdentity.Resolve(l.ctx, session.Details(l.ctx), 0)
}

func (l *JubaozhaiLogic) rmbIdentity(target uint64) (*dspb.AccountRoleOwnership, error) {
	if l.deps.Rmb == nil || l.deps.Mall == nil || l.deps.RmbIdentity == nil {
		return nil, errors.New("人民币寄售依赖未开放")
	}
	if _, ok := session.ClientPlayerID(l.ctx); !ok {
		return nil, commerce.ErrInvalid
	}
	actor, err := l.deps.RmbIdentity.Resolve(l.ctx, session.Details(l.ctx), 0)
	if err != nil {
		return nil, err
	}
	l.ctx, err = roleidentity.Capture(l.ctx, actor)
	if err != nil {
		return nil, err
	}
	if target == 0 || target == actor.Role.PlayerId {
		return actor, nil
	}
	return l.deps.RmbIdentity.Resolve(l.ctx, session.Details(l.ctx), target)
}
func (l *JubaozhaiLogic) CreateRmbConsignment(in *tradepb.CreateRmbConsignmentRequest) (*tradepb.RmbConsignmentResponse, error) {
	l, cancel := l.withRequestBudget()
	defer cancel()
	out := &tradepb.RmbConsignmentResponse{ServerNowMs: nowMillis(l.deps.Now)}
	owned, err := l.rmbIdentity(in.GetCharacterPlayerId())
	if err != nil {
		out.ErrorMessage = tipOf(commerceCode(err))
		return out, nil
	}
	zone, err := l.deps.HomeZones.HomeZone(l.ctx, owned.Role.PlayerId)
	if err != nil || zone == 0 {
		out.ErrorMessage = tipOf(constants.ErrServiceUnavailable)
		return out, nil
	}
	r, err := l.deps.Rmb.Create(l.ctx, owned.Account, owned.Role.PlayerId, owned.OwnerRevision, zone, in)
	out.ErrorMessage = tipOf(commerceCode(err))
	if err != nil {
		return out, nil
	}
	w, err := data.Workflow(r)
	if err != nil || w.Rmb == nil {
		out.ErrorMessage = tipOf(constants.ErrServiceUnavailable)
		return out, nil
	}
	if w.Rmb.RegisteredListing != nil {
		out.Listing = w.Rmb.RegisteredListing
	} else {
		out.Listing = localRmbView(r, w)
	}
	return out, nil
}
func (l *JubaozhaiLogic) GetRmbConsignment(in *tradepb.GetRmbConsignmentRequest) (*tradepb.RmbConsignmentResponse, error) {
	l, cancel := l.withRequestBudget()
	defer cancel()
	out := &tradepb.RmbConsignmentResponse{ServerNowMs: nowMillis(l.deps.Now)}
	owned, err := l.rmbIdentity(0)
	if err != nil {
		out.ErrorMessage = tipOf(commerceCode(err))
		return out, nil
	}
	view, err := l.deps.Mall.RmbGet(l.ctx, in.GetListingId())
	if errors.Is(err, commerce.ErrMissing) {
		r, readErr := l.deps.Commerce.Repo.Get(l.ctx, in.GetListingId())
		if readErr == nil && r.Kind == 6 {
			w, decodeErr := data.Workflow(r)
			if decodeErr == nil && w.Rmb != nil && w.Rmb.SellerAccount == owned.Account {
				view = localRmbView(r, w)
				err = nil
			}
		}
	}
	out.Listing = view
	out.ErrorMessage = tipOf(commerceCode(err))
	return out, nil
}
func localRmbView(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) *tradepb.RmbConsignmentView {
	state := "ESCROWING"
	switch r.Status {
	case commerce.CancelPending:
		state = "CANCEL_PENDING"
	case commerce.Cancelled:
		state = "RETURNED"
	case commerce.Settled:
		state = "SOLD"
	case commerce.Manual:
		state = "MANUAL"
	}
	return &tradepb.RmbConsignmentView{ListingId: r.CommerceId, SellerPlayerId: r.SellerPlayerId, MarketZone: w.Rmb.MarketZone, AssetKind: w.Rmb.AssetKind, Title: w.Rmb.Title, PriceFen: w.Rmb.PriceFen, State: state, Phase: w.Stage}
}
func (l *JubaozhaiLogic) ListRmbConsignments(in *tradepb.ListRmbConsignmentsRequest) (*tradepb.ListRmbConsignmentsResponse, error) {
	l, cancel := l.withRequestBudget()
	defer cancel()
	if _, err := l.rmbIdentity(0); err != nil {
		return &tradepb.ListRmbConsignmentsResponse{ErrorMessage: tipOf(commerceCode(err))}, nil
	}
	out, err := l.deps.Mall.RmbList(l.ctx, in.GetCursor(), in.GetLimit())
	if err != nil {
		return &tradepb.ListRmbConsignmentsResponse{ErrorMessage: tipOf(commerceCode(err))}, nil
	}
	out.ServerNowMs = nowMillis(l.deps.Now)
	return out, nil
}
func (l *JubaozhaiLogic) BuyRmbConsignment(in *tradepb.BuyRmbConsignmentRequest) (*tradepb.CommerceOrderResponse, error) {
	l, cancel := l.withRequestBudget()
	defer cancel()
	owned, err := l.rmbIdentity(0)
	if err != nil {
		return orderAnswer(nil, err), nil
	}
	// 买的是角色而账号已满:不让付款。不算数,付款之后满了由交付走退款(规格 §5.4)。
	if err = l.deps.Rmb.CheckBuyerSlot(l.ctx, owned.Account, in.GetListingId()); err != nil {
		return orderAnswer(nil, err), nil
	}
	out, err := l.deps.Mall.RmbBuy(l.ctx, owned.Account, owned.Role.PlayerId, owned.OwnerRevision, in)
	if err != nil {
		return orderAnswer(nil, err), nil
	}
	return out, nil
}
func (l *JubaozhaiLogic) CancelRmbConsignment(in *tradepb.CancelRmbConsignmentRequest) (*tradepb.RmbConsignmentResponse, error) {
	l, cancel := l.withRequestBudget()
	defer cancel()
	out := &tradepb.RmbConsignmentResponse{ServerNowMs: nowMillis(l.deps.Now)}
	owned, err := l.rmbIdentity(0)
	if err != nil {
		out.ErrorMessage = tipOf(commerceCode(err))
		return out, nil
	}
	err = l.deps.Rmb.Cancel(l.ctx, owned.Account, in.GetListingId())
	out.ErrorMessage = tipOf(commerceCode(err))
	if err == nil {
		out.Listing = &tradepb.RmbConsignmentView{ListingId: in.GetListingId(), State: "CANCEL_PENDING", Phase: "等待托管回退"}
	}
	return out, nil
}
func (l *JubaozhaiLogic) GetRmbOrder(in *tradepb.GetCommerceOrderRequest) (*tradepb.CommerceOrderResponse, error) {
	return l.rmbOrder(in.GetOrderId(), false)
}
func (l *JubaozhaiLogic) CancelRmbOrder(in *tradepb.CancelCommerceOrderRequest) (*tradepb.CommerceOrderResponse, error) {
	return l.rmbOrder(in.GetOrderId(), true)
}
func (l *JubaozhaiLogic) rmbOrder(id string, cancelOrder bool) (*tradepb.CommerceOrderResponse, error) {
	l, cancel := l.withRequestBudget()
	defer cancel()
	owned, err := l.rmbIdentity(0)
	if err != nil {
		return orderAnswer(nil, err), nil
	}
	out, err := l.deps.Mall.RmbOrder(l.ctx, owned.Account, id, cancelOrder)
	if err != nil {
		return orderAnswer(nil, err), nil
	}
	return out, nil
}
func (l *JubaozhaiLogic) ListRmbOrders(in *tradepb.ListCommerceOrdersRequest) (*tradepb.ListCommerceOrdersResponse, error) {
	l, cancel := l.withRequestBudget()
	defer cancel()
	owned, err := l.rmbIdentity(0)
	if err != nil {
		return &tradepb.ListCommerceOrdersResponse{ErrorMessage: tipOf(commerceCode(err))}, nil
	}
	out, err := l.deps.Mall.RmbOrders(l.ctx, owned.Account, in.GetCursor(), in.GetLimit())
	if err != nil {
		return &tradepb.ListCommerceOrdersResponse{ErrorMessage: tipOf(commerceCode(err))}, nil
	}
	out.ServerNowMs = nowMillis(l.deps.Now)
	return out, nil
}
func (l *JubaozhaiLogic) ListSellerIncomes(in *tradepb.ListSellerIncomesRequest) (*tradepb.ListSellerIncomesResponse, error) {
	l, cancel := l.withRequestBudget()
	defer cancel()
	owned, err := l.rmbIdentity(0)
	if err != nil {
		return &tradepb.ListSellerIncomesResponse{ErrorMessage: tipOf(commerceCode(err))}, nil
	}
	out, err := l.deps.Mall.RmbIncomes(l.ctx, owned.Account, in.GetCursor(), in.GetLimit())
	if err != nil {
		return &tradepb.ListSellerIncomesResponse{ErrorMessage: tipOf(commerceCode(err))}, nil
	}
	out.ServerNowMs = nowMillis(l.deps.Now)
	return out, nil
}
func (l *JubaozhaiLogic) CreateWithdrawal(in *tradepb.CreateWithdrawalRequest) (*tradepb.WithdrawalResponse, error) {
	l, cancel := l.withRequestBudget()
	defer cancel()
	owned, err := l.rmbIdentity(0)
	if err != nil {
		return &tradepb.WithdrawalResponse{ErrorMessage: tipOf(commerceCode(err))}, nil
	}
	out, err := l.deps.Mall.RmbWithdrawal(l.ctx, owned.Account, in.GetClientRequestId(), "", in.GetAmountFen())
	if err != nil {
		return &tradepb.WithdrawalResponse{ErrorMessage: tipOf(commerceCode(err))}, nil
	}
	return out, nil
}
func (l *JubaozhaiLogic) GetWithdrawal(in *tradepb.GetWithdrawalRequest) (*tradepb.WithdrawalResponse, error) {
	l, cancel := l.withRequestBudget()
	defer cancel()
	owned, err := l.rmbIdentity(0)
	if err != nil {
		return &tradepb.WithdrawalResponse{ErrorMessage: tipOf(commerceCode(err))}, nil
	}
	out, err := l.deps.Mall.RmbWithdrawal(l.ctx, owned.Account, "", in.GetWithdrawalId(), 0)
	if err != nil {
		return &tradepb.WithdrawalResponse{ErrorMessage: tipOf(commerceCode(err))}, nil
	}
	return out, nil
}
