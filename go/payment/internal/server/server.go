// Package server 是 ClientPlayerPayment 的 gRPC 适配:取 gate 会话里的玩家,交给 core,把结论写成应答与 tip 码。
// handler 一律 `return resp, nil`,故障与拒绝都在 error_message 里(serverbase 按 Tip.xlsx 的 fault 列定性)。
package server

import (
	"context"
	"errors"
	"time"

	base "proto/common/base"
	paymentpb "proto/payment"

	"payment/internal/constants"
	"payment/internal/core"
	"shared/domainhost"
)

type Server struct {
	paymentpb.UnimplementedClientPlayerPaymentServer
	svc *core.Service
}

func New(svc *core.Service) *Server { return &Server{svc: svc} }

func nowMs() uint64 { return uint64(time.Now().UnixMilli()) }

// tip 把 core 的结论映射成 tip 码。不认识的错误(存储失败、写库结局未知)一律按服务故障回。
func tip(err error) *base.TipInfoMessage {
	if err == nil {
		return nil
	}
	code := constants.ErrServiceUnavailable
	switch {
	case errors.Is(err, core.ErrInvalid):
		code = constants.ErrInvalidParameter
	case errors.Is(err, core.ErrChannelUnavailable):
		code = constants.ErrChannelUnavailable
	case errors.Is(err, core.ErrOrderNotFound):
		code = constants.ErrOrderNotFound
	case errors.Is(err, core.ErrOrderNotPayable):
		code = constants.ErrOrderNotPayable
	case errors.Is(err, core.ErrOrderExpired):
		code = constants.ErrOrderExpired
	case errors.Is(err, core.ErrChannelMismatch):
		code = constants.ErrChannelMismatch
	case errors.Is(err, core.ErrCurrencyUnsupported):
		code = constants.ErrCurrencyUnsupported
	case errors.Is(err, core.ErrBusy):
		code = constants.ErrBusy
	case errors.Is(err, core.ErrLoginRequired):
		code = constants.ErrLoginRequired
	}
	return &base.TipInfoMessage{Id: code}
}

func (s *Server) ListPayChannels(ctx context.Context, in *paymentpb.ListPayChannelsRequest) (*paymentpb.ListPayChannelsResponse, error) {
	if _, ok := domainhost.Player(ctx); !ok {
		return &paymentpb.ListPayChannelsResponse{ErrorMessage: tip(core.ErrInvalid)}, nil
	}
	channels, err := s.svc.ListChannels(in.GetPlatform())
	if err != nil {
		return &paymentpb.ListPayChannelsResponse{ErrorMessage: tip(err)}, nil
	}
	return &paymentpb.ListPayChannelsResponse{Channels: channels}, nil
}

func (s *Server) CreatePayIntent(ctx context.Context, in *paymentpb.CreatePayIntentRequest) (*paymentpb.CreatePayIntentResponse, error) {
	player, ok := domainhost.Player(ctx)
	if !ok {
		return &paymentpb.CreatePayIntentResponse{ErrorMessage: tip(core.ErrInvalid), ServerNowMs: nowMs()}, nil
	}
	created, err := s.svc.CreateIntent(ctx, core.CreateRequest{Player: player, OrderID: in.GetOrderId(), Channel: in.GetChannel(),
		Platform: in.GetPlatform(), LoginCode: in.GetLoginCode(), DeviceOS: in.GetDeviceOs()})
	resp := &paymentpb.CreatePayIntentResponse{ErrorMessage: tip(err), Intent: created.Intent, ServerNowMs: nowMs()}
	if err == nil {
		resp.WechatApp = created.Params.Wechat
		resp.AlipayApp = created.Params.Alipay
		resp.Devpay = created.Params.DevPay
		resp.WechatMinigame = created.Params.WechatMinigame
		resp.DouyinMinigame = created.Params.Douyin
		resp.Apple = created.Params.Apple
		resp.Google = created.Params.Google
	}
	return resp, nil
}

func (s *Server) GetPayIntent(ctx context.Context, in *paymentpb.GetPayIntentRequest) (*paymentpb.GetPayIntentResponse, error) {
	player, ok := domainhost.Player(ctx)
	if !ok {
		return &paymentpb.GetPayIntentResponse{ErrorMessage: tip(core.ErrInvalid), ServerNowMs: nowMs()}, nil
	}
	view, err := s.svc.GetIntent(ctx, player, in.GetOrderId())
	return &paymentpb.GetPayIntentResponse{ErrorMessage: tip(err), Intent: view, ServerNowMs: nowMs()}, nil
}

func (s *Server) SubmitPurchase(ctx context.Context, in *paymentpb.SubmitPurchaseRequest) (*paymentpb.SubmitPurchaseResponse, error) {
	player, ok := domainhost.Player(ctx)
	if !ok {
		return &paymentpb.SubmitPurchaseResponse{ErrorMessage: tip(core.ErrInvalid), ServerNowMs: nowMs()}, nil
	}
	submitted, err := s.svc.SubmitPurchase(ctx, core.SubmitRequest{Player: player, Channel: in.GetChannel(), OrderID: in.GetOrderId(),
		TransactionID: in.GetAppleTransactionId(), PurchaseToken: in.GetGooglePurchaseToken(), ProductID: in.GetGoogleProductId()})
	return &paymentpb.SubmitPurchaseResponse{ErrorMessage: tip(err), Intent: submitted.Intent, FinishTransaction: err == nil && submitted.Finish,
		ServerNowMs: nowMs()}, nil
}
