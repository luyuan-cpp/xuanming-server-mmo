package logic

import (
	"strings"

	tradepb "proto/trade"
	"shared/roleidentity"
	"trade/internal/commerce"
	"trade/internal/config"
	"trade/internal/constants"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func (l *AdminLogic) verifyCreditInternalCall(method string, in proto.Message) error {
	if _, isClient := roleidentity.Original(l.ctx); isClient {
		return status.Error(codes.PermissionDenied, "clients cannot call internal seller credit admin endpoints")
	}
	if l.deps.ManualOps != nil {
		return l.deps.ManualOps.VerifyAdminCall(l.ctx, method, in)
	}
	if !config.IsRelaxedMode(l.deps.Config.Mode) {
		return status.Error(codes.PermissionDenied, "internal seller credit admin endpoint requires signed serviceauth")
	}
	return nil
}

// RecordSellerEnvironment 接收上游登录/风控服务上报的脱敏环境信号（IP+设备指纹仅做服务端哈希比对，绝不落日志）。
// 坐高铁/切5G仅换IP不扣分；异地新IP+陌生新设备同时命中时直接清0正面信用并保留历史差评与违约。
func (l *AdminLogic) RecordSellerEnvironment(in *tradepb.RecordSellerEnvironmentRequest) (*tradepb.RecordSellerEnvironmentResponse, error) {
	l, cancel := l.withRequestBudget()
	defer cancel()
	if err := l.verifyCreditInternalCall(tradepb.TradeAdmin_RecordSellerEnvironment_FullMethodName, in); err != nil {
		return nil, err
	}
	out := &tradepb.RecordSellerEnvironmentResponse{}
	if l.deps.Credits == nil {
		out.ErrorMessage = tipOf(constants.ErrFeatureDisabled)
		return out, nil
	}
	observedMs := in.GetObservedAtMs()
	if observedMs == 0 {
		observedMs = nowMillis(l.deps.Now)
	}
	prof, cleared, err := l.deps.Credits.RecordSellerEnvironment(l.ctx, commerce.SellerEnvironmentEvent{
		SellerPlayerID: in.GetSellerPlayerId(),
		OwnerAccount:   strings.TrimSpace(in.GetOwnerAccount()),
		OwnerRevision:  in.GetOwnerRevision(),
		IP:             strings.TrimSpace(in.GetClientIp()),
		DeviceID:       strings.TrimSpace(in.GetDeviceId()),
		ObservedAtMs:   observedMs,
	})
	if err != nil {
		code := commerceCode(err)
		if code == constants.ErrServiceUnavailable {
			l.Errorf("[trade] RecordSellerEnvironment failed seller_id=%d: %v", in.GetSellerPlayerId(), err)
		}
		out.ErrorMessage = tipOf(code)
		return out, nil
	}
	out.EnvCreditCleared = cleared
	out.Profile = sellerCreditView(prof)
	return out, nil
}

// RecordSellerViolation 记录经核实的卖家违约黑历史（按 case_id 幂等，异地换机清 0 时永久保留防洗白）。
func (l *AdminLogic) RecordSellerViolation(in *tradepb.RecordSellerViolationRequest) (*tradepb.RecordSellerViolationResponse, error) {
	l, cancel := l.withRequestBudget()
	defer cancel()
	if err := l.verifyCreditInternalCall(tradepb.TradeAdmin_RecordSellerViolation_FullMethodName, in); err != nil {
		return nil, err
	}
	out := &tradepb.RecordSellerViolationResponse{}
	if l.deps.Credits == nil {
		out.ErrorMessage = tipOf(constants.ErrFeatureDisabled)
		return out, nil
	}
	recordedMs := in.GetRecordedAtMs()
	if recordedMs == 0 {
		recordedMs = nowMillis(l.deps.Now)
	}
	prof, err := l.deps.Credits.RecordViolation(l.ctx, commerce.SellerViolationRecord{
		CaseID:              strings.TrimSpace(in.GetCaseId()),
		OrderID:             strings.TrimSpace(in.GetOrderId()),
		SellerPlayerID:      in.GetSellerPlayerId(),
		SellerAccount:       strings.TrimSpace(in.GetSellerAccount()),
		SellerOwnerRevision: in.GetSellerOwnerRevision(),
		Reason:              strings.TrimSpace(in.GetReason()),
		RecordedAtMs:        recordedMs,
	})
	if err != nil {
		code := commerceCode(err)
		if code == constants.ErrServiceUnavailable {
			l.Errorf("[trade] RecordSellerViolation failed seller_id=%d case_id=%s: %v", in.GetSellerPlayerId(), in.GetCaseId(), err)
		}
		out.ErrorMessage = tipOf(code)
		return out, nil
	}
	out.Profile = sellerCreditView(prof)
	return out, nil
}
