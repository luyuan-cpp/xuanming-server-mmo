package maintenancephase

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"google.golang.org/protobuf/proto"
	pb "proto/common/database"
	"shared/authoritysql"
	"shared/drainproof"
)

// AuthorityPhase只负责既有Catalog对接；portable规则全部复用restore.go，不复制SQL事务或外部观察。
func AuthorityPhase(ctx context.Context, catalog *authoritysql.Catalog, runtime *drainproof.Runtime, action string, request *pb.SqlWriterMaintenanceReceipt, ticket *pb.SqlWriterFreezeTicket) (*pb.SqlWriterMaintenanceReceipt, error) {
	if catalog == nil || runtime == nil || request == nil {
		return nil, errors.New("authority phase装配不完整")
	}
	if err := ValidateAuthorityPlan(runtime.Plan); err != nil {
		return nil, err
	}
	sealed, err := authoritysql.SealMaintenanceRequest(request)
	if err != nil || !bytes.Equal(sealed.RequestDigest, request.RequestDigest) {
		return nil, errors.New("authority原请求摘要不符")
	}
	sum := sha256.Sum256(runtime.Request.PlanProto)
	if !bytes.Equal(request.PlanProto, runtime.Request.PlanProto) || request.Evidence == nil || !bytes.Equal(request.RunId, runtime.Plan.RunId) || !bytes.Equal(request.Evidence.ManifestDigest, sum[:]) || request.Generation != runtime.Request.Generation {
		return nil, errors.New("authority phase换run或完整manifest")
	}
	// 已确认原件不重做外部观察；票据参数仍必须交回唯一Catalog owner核对绑定。
	// 这些Catalog调用首先读同ID原件，不会以认重为由重放副作用或接受另一个ticket。
	if existing, readErr := catalog.MaintenanceReceipt(ctx, request.RequestId); readErr == nil {
		if !bytes.Equal(existing.RequestDigest, request.RequestDigest) {
			return nil, errors.New("authority原ID对应不同请求")
		}
		switch action {
		case "advance":
			if request.Kind != pb.SqlWriterMaintenanceReceipt_PHASE_EVENT && request.Kind != pb.SqlWriterMaintenanceReceipt_ROLE_CLOSED && request.Kind != pb.SqlWriterMaintenanceReceipt_RELAY_QUIESCED {
				return nil, errors.New("advance原件kind不符")
			}
			return existing, nil
		case "grant-ticket":
			if ticket == nil {
				return nil, errors.New("缺原ticket")
			}
			return catalog.GrantTicket(ctx, ticket, request)
		case "revoke-ticket":
			if ticket == nil {
				return nil, errors.New("缺原ticket")
			}
			return catalog.RevokeTicket(ctx, ticket.TicketId, request)
		case "freeze":
			if ticket == nil {
				return nil, errors.New("缺原ticket")
			}
			return catalog.Freeze(ctx, ticket.TicketId, request)
		case "release":
			if ticket == nil {
				return nil, errors.New("缺原ticket")
			}
			return catalog.Release(ctx, ticket.TicketId, request)
		default:
			return nil, errors.New("未知authority phase")
		}
	} else if !errors.Is(readErr, sql.ErrNoRows) {
		return nil, readErr
	}
	readClosure := func(receipt *pb.SqlWriterMaintenanceReceipt) error {
		closure := receipt.Evidence.GetNonSqlClosure()
		if closure == nil {
			return errors.New("缺nonSQL原收据")
		}
		root, err := catalog.MaintenanceReceipt(ctx, closure.RootClosedReceiptId)
		if err != nil {
			return err
		}
		relay, err := catalog.MaintenanceReceipt(ctx, closure.RelayQuiescedReceiptId)
		if err != nil {
			return err
		}
		return VerifyAuthorityEvidence(ctx, runtime.Plan, runtime.Request.PlanProto, receipt, root, relay)
	}
	switch action {
	case "advance":
		switch request.RunPhase {
		case pb.SqlWriterDrainRun_PLAN_FIXED:
			if request.Kind != pb.SqlWriterMaintenanceReceipt_PHASE_EVENT || request.AttemptRevision != 1 {
				return nil, errors.New("PLAN_FIXED原件非法")
			}
		case pb.SqlWriterDrainRun_ROOTS_CLOSED:
			if request.Kind != pb.SqlWriterMaintenanceReceipt_ROLE_CLOSED {
				return nil, errors.New("ROOTS_CLOSED缺ROLE_CLOSED原件")
			}
			if err = VerifyAuthorityEvidence(ctx, runtime.Plan, runtime.Request.PlanProto, request, nil, nil); err != nil {
				return nil, err
			}
		case pb.SqlWriterDrainRun_DRAINING:
			if request.Kind == pb.SqlWriterMaintenanceReceipt_RELAY_QUIESCED {
				if err = VerifyAuthorityEvidence(ctx, runtime.Plan, runtime.Request.PlanProto, request, nil, nil); err != nil {
					return nil, err
				}
			} else if request.Kind != pb.SqlWriterMaintenanceReceipt_PHASE_EVENT {
				return nil, errors.New("DRAINING阶段kind错误")
			}
		case pb.SqlWriterDrainRun_NON_SQL_CLOSED:
			if err = readClosure(request); err != nil {
				return nil, err
			}
		case pb.SqlWriterDrainRun_RELEASE_READY:
			if err = VerifyAuthorityReleaseReadiness(ctx, catalog, runtime.Plan, request); err != nil {
				return nil, err
			}
		case pb.SqlWriterDrainRun_PUBLICATION_REOPENED:
			if err = VerifyAuthorityPublicationReadiness(ctx, catalog, runtime.Plan, request); err != nil {
				return nil, err
			}
		case pb.SqlWriterDrainRun_ABORTED_CLOSED:
			// 失败阶段只保持closed，不产生正常nonSQL已排空或放流结论。
			if err = VerifyAuthorityEmergency(ctx, catalog, runtime.Plan, request, false); err != nil {
				return nil, err
			}
		default:
			return nil, errors.New("该阶段必须使用对应票据/冻结/释放/准备owner，不能跳阶Advance")
		}
		next := &pb.SqlWriterDrainRun{OperationId: bytes.Clone(request.RunId), RequestDigest: bytes.Clone(request.RequestDigest), Generation: request.Generation, AttemptRevision: request.AttemptRevision, ManifestProto: bytes.Clone(request.PlanProto), ManifestDigest: sum[:], Phase: request.RunPhase}
		return catalog.Advance(ctx, next, request)
	case "grant-ticket":
		if ticket == nil {
			return nil, errors.New("缺正式freeze ticket")
		}
		if ticket.AuthorizationKind == pb.SqlWriterFreezeTicket_EMERGENCY_HOLD_FREEZE {
			if len(ticket.NonSqlReceiptDigest) != 0 {
				return nil, errors.New("紧急HOLD不得伪填normal nonSQL摘要")
			}
			if err = VerifyAuthorityEmergency(ctx, catalog, runtime.Plan, request, true); err != nil {
				return nil, err
			}
			return catalog.GrantTicket(ctx, ticket, request)
		}
		if ticket.AuthorizationKind != pb.SqlWriterFreezeTicket_NORMAL_DRAIN_FREEZE {
			return nil, errors.New("未知冻结授权种类")
		}
		prior, err := catalog.MaintenanceReceipt(ctx, request.PriorReceiptId)
		if err != nil {
			return nil, err
		}
		if prior.RunPhase != pb.SqlWriterDrainRun_NON_SQL_CLOSED || !bytes.Equal(prior.ReceiptDigest, ticket.NonSqlReceiptDigest) {
			return nil, errors.New("票据未消费同attempt nonSQL收据")
		}
		if err = readClosure(prior); err != nil {
			return nil, err
		}
		return catalog.GrantTicket(ctx, ticket, request)
	case "revoke-ticket":
		if ticket == nil {
			return nil, errors.New("撤销缺原ticket身份")
		}
		return catalog.RevokeTicket(ctx, ticket.TicketId, request)
	case "freeze":
		if ticket == nil {
			return nil, errors.New("冻结缺原ticket身份")
		}
		actual, err := catalog.FreezeTicket(ctx, ticket.TicketId)
		if err != nil {
			return nil, err
		}
		grant, err := catalog.MaintenanceReceipt(ctx, actual.GrantReceiptId)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(grant.ReceiptDigest, actual.GrantReceiptDigest) {
			return nil, errors.New("冻结票据grant原收据不符")
		}
		if actual.AuthorizationKind == pb.SqlWriterFreezeTicket_EMERGENCY_HOLD_FREEZE {
			if err = VerifyAuthorityEmergency(ctx, catalog, runtime.Plan, request, true); err != nil {
				return nil, err
			}
			return catalog.Freeze(ctx, ticket.TicketId, request)
		}
		prior, err := catalog.MaintenanceReceipt(ctx, grant.PriorReceiptId)
		if err != nil {
			return nil, err
		}
		if err = readClosure(prior); err != nil {
			return nil, err
		}
		return catalog.Freeze(ctx, ticket.TicketId, request)
	case "release":
		if ticket == nil {
			return nil, errors.New("release缺原consumed ticket")
		}
		prior, err := catalog.MaintenanceReceipt(ctx, request.PriorReceiptId)
		if err != nil {
			return nil, err
		}
		if prior.RunPhase != pb.SqlWriterDrainRun_RELEASE_READY || !bytes.Equal(prior.ReceiptDigest, request.PriorReceiptDigest) {
			return nil, errors.New("release缺原RELEASE_READY阶段收据")
		}
		if err = VerifyAuthorityReleaseReadiness(ctx, catalog, runtime.Plan, prior); err != nil {
			return nil, err
		}
		if !proto.Equal(prior.Evidence.GetReleaseReadiness(), request.Evidence.GetReleaseReadiness()) {
			return nil, errors.New("release更换原释放原件")
		}
		return catalog.Release(ctx, ticket.TicketId, request)
	default:
		return nil, errors.New("未知authority phase")
	}
}

// ReadAuthorityOutcome是UNKNOWN之后的唯一续查入口，全部强读同一Catalog，不执行变更。
func ReadAuthorityOutcome(ctx context.Context, catalog *authoritysql.Catalog, requestID, runID, ticketID []byte) (*pb.SqlWriterMaintenanceReceipt, *pb.SqlWriterDrainRun, *pb.SqlWriterFreezeTicket, error) {
	receipt, receiptErr := catalog.MaintenanceReceipt(ctx, requestID)
	run, runErr := catalog.DrainRun(ctx, runID)
	var ticket *pb.SqlWriterFreezeTicket
	var ticketErr error
	if len(ticketID) != 0 {
		ticket, ticketErr = catalog.FreezeTicket(ctx, ticketID)
	}
	return receipt, run, ticket, errors.Join(receiptErr, runErr, ticketErr)
}

var _ proto.Message = (*pb.SqlWriterMaintenanceReceipt)(nil)
