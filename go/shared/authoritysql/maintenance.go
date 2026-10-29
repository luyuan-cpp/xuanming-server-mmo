package authoritysql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"
	pb "proto/common/database"
)

// SealMaintenanceRequest固定调用输入；SQL生成的committed_ms/receipt_digest不参与请求摘要。
func SealMaintenanceRequest(input *pb.SqlWriterMaintenanceReceipt) (*pb.SqlWriterMaintenanceReceipt, error) {
	if input == nil {
		return nil, ErrWriterDenied
	}
	r := proto.Clone(input).(*pb.SqlWriterMaintenanceReceipt)
	r.RequestDigest = nil
	r.ReceiptDigest = nil
	r.CommittedMs = 0
	digest, err := messageDigest(r)
	if err != nil {
		return nil, err
	}
	r.RequestDigest = digest
	return r, nil
}
func (c *Catalog) checkRequest(input *pb.SqlWriterMaintenanceReceipt) error {
	if input != nil && hasUnknownControl(input) {
		return ErrWriterDenied
	}
	if input == nil || len(input.RequestId) != 16 || len(input.RunId) != 16 || input.Generation != c.config.Generation || input.Evidence == nil || !bytes.Equal(input.RunId, input.Evidence.OperationId) || len(input.Evidence.ManifestDigest) != 32 {
		return ErrWriterDenied
	}
	sealed, err := SealMaintenanceRequest(input)
	if err != nil {
		return err
	}
	if !bytes.Equal(sealed.RequestDigest, input.RequestDigest) {
		return ErrWriterDenied
	}
	return nil
}
func sqlClock(ctx context.Context, tx interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (uint64, error) {
	var now uint64
	err := tx.QueryRowContext(ctx, "SELECT CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 AS UNSIGNED)").Scan(&now)
	return now, err
}
func (c *Catalog) readReceipt(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id []byte) (*pb.SqlWriterMaintenanceReceipt, error) {
	r := &pb.SqlWriterMaintenanceReceipt{RequestId: bytes.Clone(id)}
	var evidence []byte
	err := q.QueryRowContext(ctx, "SELECT request_digest,run_id,generation,before_epoch,after_epoch,catalog_revision,phase,plan_proto,committed_ms,kind,run_phase,attempt_revision,prior_receipt_id,prior_receipt_digest,evidence,receipt_digest FROM "+c.table("sql_writer_maintenance_receipt")+" WHERE request_id=?", id).Scan(&r.RequestDigest, &r.RunId, &r.Generation, &r.BeforeEpoch, &r.AfterEpoch, &r.CatalogRevision, &r.Phase, &r.PlanProto, &r.CommittedMs, &r.Kind, &r.RunPhase, &r.AttemptRevision, &r.PriorReceiptId, &r.PriorReceiptDigest, &evidence, &r.ReceiptDigest)
	if err != nil {
		return nil, err
	}
	r.Evidence = &pb.SqlWriterMaintenanceReceipt_Evidence{}
	if err = proto.Unmarshal(evidence, r.Evidence); err != nil {
		return nil, err
	}
	if hasUnknownControl(r) || r.CommittedMs == 0 || c.checkRequest(r) != nil {
		return nil, ErrWriterDenied
	}
	copy := proto.Clone(r).(*pb.SqlWriterMaintenanceReceipt)
	copy.ReceiptDigest = nil
	digest, err := messageDigest(copy)
	if err != nil || !bytes.Equal(digest, r.ReceiptDigest) {
		return nil, ErrWriterDenied
	}
	return r, nil
}
func (c *Catalog) MaintenanceReceipt(ctx context.Context, id []byte) (*pb.SqlWriterMaintenanceReceipt, error) {
	return c.readReceipt(ctx, c.db, id)
}
func (c *Catalog) existingReceipt(ctx context.Context, input *pb.SqlWriterMaintenanceReceipt) (*pb.SqlWriterMaintenanceReceipt, error) {
	r, err := c.MaintenanceReceipt(ctx, input.RequestId)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(r.RequestDigest, input.RequestDigest) {
		return nil, ErrWriterDenied
	}
	return r, nil
}
func (c *Catalog) insertReceipt(ctx context.Context, tx *sql.Tx, input *pb.SqlWriterMaintenanceReceipt) (*pb.SqlWriterMaintenanceReceipt, error) {
	r := proto.Clone(input).(*pb.SqlWriterMaintenanceReceipt)
	now, err := sqlClock(ctx, tx)
	if err != nil {
		return nil, err
	}
	r.CommittedMs = now
	r.ReceiptDigest = nil
	digest, err := messageDigest(r)
	if err != nil {
		return nil, err
	}
	r.ReceiptDigest = digest
	evidence, err := deterministic(r.Evidence)
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO "+c.table("sql_writer_maintenance_receipt")+"(request_id,request_digest,run_id,generation,before_epoch,after_epoch,catalog_revision,phase,plan_proto,committed_ms,kind,run_phase,attempt_revision,prior_receipt_id,prior_receipt_digest,evidence,receipt_digest) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", r.RequestId, r.RequestDigest, r.RunId, r.Generation, r.BeforeEpoch, r.AfterEpoch, r.CatalogRevision, r.Phase, r.PlanProto, r.CommittedMs, r.Kind, r.RunPhase, r.AttemptRevision, r.PriorReceiptId, r.PriorReceiptDigest, evidence, r.ReceiptDigest)
	return r, err
}
func (c *Catalog) readRun(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id []byte, lock bool) (*pb.SqlWriterDrainRun, error) {
	r := &pb.SqlWriterDrainRun{OperationId: bytes.Clone(id)}
	query := "SELECT request_digest,generation,attempt_revision,manifest_proto,manifest_digest,phase,latest_receipt_digest,frozen_request_id,updated_ms,latest_receipt_id FROM " + c.table("sql_writer_drain_run") + " WHERE operation_id=?"
	if lock {
		query += " FOR UPDATE"
	}
	err := q.QueryRowContext(ctx, query, id).Scan(&r.RequestDigest, &r.Generation, &r.AttemptRevision, &r.ManifestProto, &r.ManifestDigest, &r.Phase, &r.LatestReceiptDigest, &r.FrozenRequestId, &r.UpdatedMs, &r.LatestReceiptId)
	return r, err
}
func (c *Catalog) DrainRun(ctx context.Context, id []byte) (*pb.SqlWriterDrainRun, error) {
	return c.readRun(ctx, c.db, id, false)
}
func (c *Catalog) writeRun(ctx context.Context, tx *sql.Tx, run *pb.SqlWriterDrainRun, receipt *pb.SqlWriterMaintenanceReceipt) error {
	_, err := tx.ExecContext(ctx, "UPDATE "+c.table("sql_writer_drain_run")+" SET phase=?,attempt_revision=?,latest_receipt_id=?,latest_receipt_digest=?,frozen_request_id=?,updated_ms=? WHERE operation_id=?", run.Phase, run.AttemptRevision, receipt.RequestId, receipt.ReceiptDigest, run.FrozenRequestId, receipt.CommittedMs, run.OperationId)
	return err
}
func phaseTransition(from, to pb.SqlWriterDrainRun_Phase) bool {
	switch from {
	case pb.SqlWriterDrainRun_UNKNOWN:
		return to == pb.SqlWriterDrainRun_PLAN_FIXED
	case pb.SqlWriterDrainRun_PLAN_FIXED:
		return to == pb.SqlWriterDrainRun_ROOTS_CLOSED || to == pb.SqlWriterDrainRun_ABORTED_CLOSED
	case pb.SqlWriterDrainRun_ROOTS_CLOSED:
		return to == pb.SqlWriterDrainRun_DRAINING || to == pb.SqlWriterDrainRun_ABORTED_CLOSED
	case pb.SqlWriterDrainRun_DRAINING:
		return to == pb.SqlWriterDrainRun_NON_SQL_CLOSED || to == pb.SqlWriterDrainRun_ABORTED_CLOSED
	case pb.SqlWriterDrainRun_ABORTED_CLOSED:
		return to == pb.SqlWriterDrainRun_DRAINING
	case pb.SqlWriterDrainRun_SQL_FROZEN:
		return to == pb.SqlWriterDrainRun_RELEASE_READY
	case pb.SqlWriterDrainRun_SQL_RELEASED:
		return to == pb.SqlWriterDrainRun_PUBLICATION_REOPENED
	default:
		return false
	}
}

// Advance只消费已经由便携verifier核验的typed原件；不读文件、Redis或Kafka。
// SQL_FROZEN/RELEASED/FREEZE_AUTHORIZED只能由下方对应原子方法推进。
func (c *Catalog) Advance(ctx context.Context, next *pb.SqlWriterDrainRun, input *pb.SqlWriterMaintenanceReceipt) (*pb.SqlWriterMaintenanceReceipt, error) {
	if err := c.checkRequest(input); err != nil {
		return nil, err
	}
	if input.Phase != pb.SqlWriterMaintenanceReceipt_PHASE_UNKNOWN || (input.Kind != pb.SqlWriterMaintenanceReceipt_PHASE_EVENT && input.Kind != pb.SqlWriterMaintenanceReceipt_ROLE_CLOSED && input.Kind != pb.SqlWriterMaintenanceReceipt_RELAY_QUIESCED) {
		return nil, ErrWriterDenied
	}
	if next == nil || !bytes.Equal(next.OperationId, input.RunId) || next.Generation != c.config.Generation || next.Phase != input.RunPhase || next.AttemptRevision != input.AttemptRevision || !bytes.Equal(next.ManifestDigest, input.Evidence.ManifestDigest) {
		return nil, ErrWriterDenied
	}
	manifestDigest := sha256.Sum256(next.ManifestProto)
	if len(next.ManifestProto) == 0 || !bytes.Equal(manifestDigest[:], next.ManifestDigest) {
		return nil, ErrWriterDenied
	}
	if old, err := c.existingReceipt(ctx, input); err == nil {
		return old, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var result *pb.SqlWriterMaintenanceReceipt
	err := c.transaction(ctx, func(tx *sql.Tx) error {
		if err := c.lockReconcile(ctx, tx); err != nil {
			return err
		}
		meta, err := c.meta(ctx, tx, true)
		if err != nil {
			return err
		}
		if meta.WriterEpoch != input.BeforeEpoch || input.AfterEpoch != input.BeforeEpoch || meta.CatalogRevision != input.CatalogRevision {
			return ErrWriterDenied
		}
		run, err := c.readRun(ctx, tx, next.OperationId, true)
		if errors.Is(err, sql.ErrNoRows) {
			if next.Phase != pb.SqlWriterDrainRun_PLAN_FIXED || next.AttemptRevision != 1 || meta.Writable != 1 || len(meta.FreezeRunId) != 0 || len(input.PriorReceiptId) != 0 {
				return ErrWriterDenied
			}
			run = proto.Clone(next).(*pb.SqlWriterDrainRun)
			run.Phase = pb.SqlWriterDrainRun_UNKNOWN
			if !bytes.Equal(input.PlanProto, next.ManifestProto) {
				return ErrWriterDenied
			}
			if _, err = c.checkPlan(input); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, "INSERT INTO "+c.table("sql_writer_drain_run")+"(operation_id,request_digest,generation,attempt_revision,manifest_proto,manifest_digest,phase,latest_receipt_digest,frozen_request_id,updated_ms,latest_receipt_id) VALUES(?,?,?,?,?,?,1,?,?,0,?)", run.OperationId, run.RequestDigest, run.Generation, run.AttemptRevision, run.ManifestProto, run.ManifestDigest, []byte{}, []byte{}, []byte{})
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "UPDATE "+c.table("route_meta")+" SET freeze_run_id=? WHERE id=1", run.OperationId); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if run.Generation != input.Generation {
			return ErrWriterDenied
		}
		samePhaseRelay := run.Phase == pb.SqlWriterDrainRun_DRAINING && next.Phase == run.Phase && input.Kind == pb.SqlWriterMaintenanceReceipt_RELAY_QUIESCED && input.Evidence.GetPublication() != nil
		if (!phaseTransition(run.Phase, next.Phase) && !samePhaseRelay) || !bytes.Equal(run.ManifestDigest, next.ManifestDigest) || !bytes.Equal(run.ManifestProto, next.ManifestProto) || !bytes.Equal(run.LatestReceiptId, input.PriorReceiptId) || !bytes.Equal(run.LatestReceiptDigest, input.PriorReceiptDigest) {
			return ErrWriterDenied
		}
		if run.Phase == pb.SqlWriterDrainRun_ABORTED_CLOSED {
			if next.AttemptRevision != run.AttemptRevision+1 {
				return ErrWriterDenied
			}
		} else if next.AttemptRevision != run.AttemptRevision {
			return ErrWriterDenied
		}
		if next.Phase == pb.SqlWriterDrainRun_ROOTS_CLOSED && input.Evidence.GetPublication() == nil {
			return ErrWriterDenied
		}
		if next.Phase == pb.SqlWriterDrainRun_NON_SQL_CLOSED && input.Evidence.GetNonSqlClosure() == nil {
			return ErrWriterDenied
		}
		if !bytes.Equal(input.PlanProto, run.ManifestProto) {
			return ErrWriterDenied
		}
		if next.Phase == pb.SqlWriterDrainRun_RELEASE_READY && input.Evidence.GetReleaseReadiness() == nil {
			return ErrWriterDenied
		}
		if next.Phase == pb.SqlWriterDrainRun_PUBLICATION_REOPENED && input.Evidence.GetPublicationReadiness() == nil {
			return ErrWriterDenied
		}
		plan, err := c.checkPlan(input)
		if err != nil {
			return err
		}
		if next.Phase != pb.SqlWriterDrainRun_PUBLICATION_REOPENED {
			expected, err := c.plannedActive(ctx, tx, plan, meta.CatalogRevision)
			if err != nil {
				return err
			}
			current, err := c.active(ctx, tx, false)
			if err != nil {
				return err
			}
			if !exactKeys(expected, current) {
				return ErrWriterDenied
			}
			if next.Phase == pb.SqlWriterDrainRun_NON_SQL_CLOSED && len(expected) != 0 {
				return ErrWriterDenied
			}
		}
		if next.Phase == pb.SqlWriterDrainRun_RELEASE_READY {
			if meta.Writable != 0 {
				return ErrWriterDenied
			}
			proof := input.Evidence.GetReleaseReadiness()
			frozen, err := c.readReceipt(ctx, tx, run.FrozenRequestId)
			if err != nil {
				return err
			}
			if frozen.Kind != pb.SqlWriterMaintenanceReceipt_FREEZE || frozen.Phase != pb.SqlWriterMaintenanceReceipt_FROZEN || !bytes.Equal(frozen.RunId, run.OperationId) || !bytes.Equal(frozen.PlanProto, run.ManifestProto) || !bytes.Equal(proof.OriginalFreezeReceiptId, frozen.RequestId) || !bytes.Equal(proof.OriginalFreezeReceiptDigest, frozen.ReceiptDigest) {
				return ErrWriterDenied
			}
			if err := validateMaintenanceClosure(plan, proof, frozen.CommittedMs); err != nil {
				return err
			}
		} else if meta.Writable != 1 {
			return ErrWriterDenied
		}
		result, err = c.insertReceipt(ctx, tx, input)
		if err != nil {
			return err
		}
		run.Phase = next.Phase
		run.AttemptRevision = next.AttemptRevision
		if err = c.writeRun(ctx, tx, run, result); err != nil {
			return err
		}
		if next.Phase == pb.SqlWriterDrainRun_PUBLICATION_REOPENED {
			_, err = tx.ExecContext(ctx, "UPDATE "+c.table("route_meta")+" SET freeze_run_id=? WHERE id=1", []byte{})
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (c *Catalog) readTicket(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id []byte, lock bool) (*pb.SqlWriterFreezeTicket, error) {
	t := &pb.SqlWriterFreezeTicket{TicketId: bytes.Clone(id)}
	query := "SELECT operation_id,attempt_revision,generation,manifest_digest,non_sql_receipt_digest,freeze_request_id,freeze_request_digest,not_before_ms,expires_ms,state,consumed_receipt_digest,revocation_digest,updated_ms,authorization_kind,grant_receipt_id,grant_receipt_digest FROM " + c.table("sql_writer_freeze_ticket") + " WHERE ticket_id=?"
	if lock {
		query += " FOR UPDATE"
	}
	err := q.QueryRowContext(ctx, query, id).Scan(&t.OperationId, &t.AttemptRevision, &t.Generation, &t.ManifestDigest, &t.NonSqlReceiptDigest, &t.FreezeRequestId, &t.FreezeRequestDigest, &t.NotBeforeMs, &t.ExpiresMs, &t.State, &t.ConsumedReceiptDigest, &t.RevocationDigest, &t.UpdatedMs, &t.AuthorizationKind, &t.GrantReceiptId, &t.GrantReceiptDigest)
	return t, err
}
func (c *Catalog) FreezeTicket(ctx context.Context, id []byte) (*pb.SqlWriterFreezeTicket, error) {
	return c.readTicket(ctx, c.db, id, false)
}

// SealFreezeTicketRequest覆盖授权时间窗和目标；数据库提交后才填写的字段不参与输入摘要。
func SealFreezeTicketRequest(ticket *pb.SqlWriterFreezeTicket) ([]byte, error) {
	if ticket == nil || hasUnknownControl(ticket) {
		return nil, ErrWriterDenied
	}
	copy := proto.Clone(ticket).(*pb.SqlWriterFreezeTicket)
	copy.GrantReceiptId = nil
	copy.GrantReceiptDigest = nil
	copy.UpdatedMs = 0
	copy.ConsumedReceiptDigest = nil
	copy.RevocationDigest = nil
	return messageDigest(copy)
}

func (c *Catalog) GrantTicket(ctx context.Context, ticket *pb.SqlWriterFreezeTicket, input *pb.SqlWriterMaintenanceReceipt) (*pb.SqlWriterMaintenanceReceipt, error) {
	if err := c.checkRequest(input); err != nil {
		return nil, err
	}
	if ticket == nil || len(ticket.TicketId) != 16 || len(ticket.FreezeRequestId) != 16 || len(ticket.FreezeRequestDigest) != 32 || ticket.Generation != c.config.Generation || !bytes.Equal(ticket.OperationId, input.RunId) || ticket.AttemptRevision != input.AttemptRevision || ticket.State != pb.SqlWriterFreezeTicket_AVAILABLE || ticket.ExpiresMs <= ticket.NotBeforeMs || ticket.ExpiresMs-ticket.NotBeforeMs > 60000 || input.Kind != pb.SqlWriterMaintenanceReceipt_TICKET_GRANTED || input.Phase != pb.SqlWriterMaintenanceReceipt_PHASE_UNKNOWN || input.RunPhase != pb.SqlWriterDrainRun_FREEZE_AUTHORIZED {
		return nil, ErrWriterDenied
	}
	binding := input.Evidence.GetTicket()
	if ticket.AuthorizationKind == pb.SqlWriterFreezeTicket_EMERGENCY_HOLD_FREEZE {
		binding = input.Evidence.GetEmergency().GetAuthorization()
	}
	digest, err := SealFreezeTicketRequest(ticket)
	if err != nil || binding == nil || !bytes.Equal(binding.TicketId, ticket.TicketId) || !bytes.Equal(binding.BoundRequestId, ticket.FreezeRequestId) || !bytes.Equal(binding.BoundRequestDigest, ticket.FreezeRequestDigest) || !bytes.Equal(binding.AuthorityOperationId, ticket.OperationId) || !bytes.Equal(binding.TicketRequestDigest, digest) || len(ticket.GrantReceiptId) != 0 || len(ticket.GrantReceiptDigest) != 0 || ticket.UpdatedMs != 0 || len(ticket.ConsumedReceiptDigest) != 0 || len(ticket.RevocationDigest) != 0 {
		return nil, ErrWriterDenied
	}
	if old, err := c.existingReceipt(ctx, input); err == nil {
		return old, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var result *pb.SqlWriterMaintenanceReceipt
	err = c.transaction(ctx, func(tx *sql.Tx) error {
		if err := c.lockReconcile(ctx, tx); err != nil {
			return err
		}
		meta, err := c.meta(ctx, tx, true)
		if err != nil {
			return err
		}
		run, err := c.readRun(ctx, tx, input.RunId, true)
		if err != nil {
			return err
		}
		if meta.Writable != 1 || !bytes.Equal(meta.FreezeRunId, input.RunId) || run.AttemptRevision != input.AttemptRevision || !bytes.Equal(run.ManifestDigest, ticket.ManifestDigest) || !bytes.Equal(run.LatestReceiptId, input.PriorReceiptId) || !bytes.Equal(run.LatestReceiptDigest, input.PriorReceiptDigest) {
			return ErrWriterDenied
		}
		if input.BeforeEpoch != meta.WriterEpoch || input.AfterEpoch != meta.WriterEpoch || input.CatalogRevision != meta.CatalogRevision {
			return ErrWriterDenied
		}
		if run.Generation != input.Generation || !bytes.Equal(run.ManifestProto, input.PlanProto) || !bytes.Equal(run.ManifestDigest, input.Evidence.ManifestDigest) || ticket.AttemptRevision != run.AttemptRevision {
			return ErrWriterDenied
		}
		plan, err := c.checkPlan(input)
		if err != nil {
			return err
		}
		expected, err := c.plannedActive(ctx, tx, plan, meta.CatalogRevision)
		if err != nil {
			return err
		}
		current, err := c.active(ctx, tx, false)
		if err != nil {
			return err
		}
		if !exactKeys(expected, current) {
			return ErrWriterDenied
		}
		if ticket.AuthorizationKind == pb.SqlWriterFreezeTicket_NORMAL_DRAIN_FREEZE && len(expected) != 0 {
			return ErrWriterDenied
		}
		if ticket.AuthorizationKind == pb.SqlWriterFreezeTicket_NORMAL_DRAIN_FREEZE {
			binding := input.Evidence.GetTicket()
			if binding == nil || !bytes.Equal(binding.TicketId, ticket.TicketId) || !bytes.Equal(binding.BoundRequestId, ticket.FreezeRequestId) || !bytes.Equal(binding.BoundRequestDigest, ticket.FreezeRequestDigest) || !bytes.Equal(binding.AuthorityOperationId, ticket.OperationId) {
				return ErrWriterDenied
			}
		}
		if ticket.AuthorizationKind == pb.SqlWriterFreezeTicket_NORMAL_DRAIN_FREEZE {
			if run.Phase != pb.SqlWriterDrainRun_NON_SQL_CLOSED || !bytes.Equal(ticket.NonSqlReceiptDigest, run.LatestReceiptDigest) {
				return ErrWriterDenied
			}
		} else if ticket.AuthorizationKind == pb.SqlWriterFreezeTicket_EMERGENCY_HOLD_FREEZE {
			if input.Evidence.GetEmergency() == nil {
				return ErrWriterDenied
			}
		} else {
			return ErrWriterDenied
		}
		now, err := sqlClock(ctx, tx)
		if err != nil {
			return err
		}
		if now < ticket.NotBeforeMs || now >= ticket.ExpiresMs {
			return ErrWriterDenied
		}
		result, err = c.insertReceipt(ctx, tx, input)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO "+c.table("sql_writer_freeze_ticket")+"(ticket_id,operation_id,attempt_revision,generation,manifest_digest,non_sql_receipt_digest,freeze_request_id,freeze_request_digest,not_before_ms,expires_ms,state,consumed_receipt_digest,revocation_digest,updated_ms,authorization_kind,grant_receipt_id,grant_receipt_digest) VALUES(?,?,?,?,?,?,?,?,?,?,1,?,?,?,?,?,?)", ticket.TicketId, ticket.OperationId, ticket.AttemptRevision, ticket.Generation, ticket.ManifestDigest, ticket.NonSqlReceiptDigest, ticket.FreezeRequestId, ticket.FreezeRequestDigest, ticket.NotBeforeMs, ticket.ExpiresMs, []byte{}, []byte{}, now, ticket.AuthorizationKind, result.RequestId, result.ReceiptDigest)
		if err != nil {
			return err
		}
		run.Phase = pb.SqlWriterDrainRun_FREEZE_AUTHORIZED
		return c.writeRun(ctx, tx, run, result)
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (c *Catalog) RevokeTicket(ctx context.Context, ticketID []byte, input *pb.SqlWriterMaintenanceReceipt) (*pb.SqlWriterMaintenanceReceipt, error) {
	if err := c.checkRequest(input); err != nil {
		return nil, err
	}
	if input.Kind != pb.SqlWriterMaintenanceReceipt_TICKET_REVOKED || input.RunPhase != pb.SqlWriterDrainRun_ABORTED_CLOSED {
		return nil, ErrWriterDenied
	}
	binding := input.Evidence.GetTicket()
	if binding == nil || len(ticketID) != 16 || !bytes.Equal(binding.TicketId, ticketID) || !bytes.Equal(binding.AuthorityOperationId, input.RunId) || len(binding.BoundRequestId) != 16 || len(binding.BoundRequestDigest) != 32 {
		return nil, ErrWriterDenied
	}
	if old, err := c.existingReceipt(ctx, input); err == nil {
		return old, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var result *pb.SqlWriterMaintenanceReceipt
	err := c.transaction(ctx, func(tx *sql.Tx) error {
		if err := c.lockReconcile(ctx, tx); err != nil {
			return err
		}
		meta, err := c.meta(ctx, tx, true)
		if err != nil {
			return err
		}
		run, err := c.readRun(ctx, tx, input.RunId, true)
		if err != nil {
			return err
		}
		ticket, err := c.readTicket(ctx, tx, ticketID, true)
		if err != nil {
			return err
		}
		if meta.Writable != 1 || run.Phase != pb.SqlWriterDrainRun_FREEZE_AUTHORIZED || ticket.State != pb.SqlWriterFreezeTicket_AVAILABLE || !bytes.Equal(ticket.OperationId, input.RunId) || ticket.AttemptRevision != input.AttemptRevision || !bytes.Equal(run.LatestReceiptId, input.PriorReceiptId) || !bytes.Equal(run.LatestReceiptDigest, input.PriorReceiptDigest) {
			return ErrWriterDenied
		}
		if !bytes.Equal(binding.BoundRequestId, ticket.FreezeRequestId) || !bytes.Equal(binding.BoundRequestDigest, ticket.FreezeRequestDigest) || !bytes.Equal(meta.FreezeRunId, input.RunId) || meta.WriterEpoch != input.BeforeEpoch || input.AfterEpoch != input.BeforeEpoch || meta.CatalogRevision != input.CatalogRevision || !bytes.Equal(run.ManifestProto, input.PlanProto) || !bytes.Equal(run.ManifestDigest, input.Evidence.ManifestDigest) || ticket.Generation != input.Generation || run.Generation != input.Generation {
			return ErrWriterDenied
		}
		result, err = c.insertReceipt(ctx, tx, input)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE "+c.table("sql_writer_freeze_ticket")+" SET state=3,revocation_digest=?,updated_ms=? WHERE ticket_id=?", result.ReceiptDigest, result.CommittedMs, ticketID); err != nil {
			return err
		}
		run.Phase = pb.SqlWriterDrainRun_ABORTED_CLOSED
		return c.writeRun(ctx, tx, run, result)
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (c *Catalog) checkPlan(input *pb.SqlWriterMaintenanceReceipt) (*pb.SqlWriterMaintenanceReceipt_Plan, error) {
	plan := &pb.SqlWriterMaintenanceReceipt_Plan{}
	if err := proto.Unmarshal(input.PlanProto, plan); err != nil {
		return nil, err
	}
	if err := c.validatePlan(plan); err != nil {
		return nil, err
	}
	if !bytes.Equal(plan.RunId, input.RunId) || plan.CatalogRevision > input.CatalogRevision {
		return nil, ErrWriterDenied
	}
	expected := plan.BeforeEpoch
	if input.Kind == pb.SqlWriterMaintenanceReceipt_RELEASE || input.RunPhase == pb.SqlWriterDrainRun_RELEASE_READY {
		var err error
		expected, err = checkedIncrement(expected)
		if err != nil {
			return nil, err
		}
	}
	if input.RunPhase == pb.SqlWriterDrainRun_PUBLICATION_REOPENED {
		var err error
		expected, err = checkedIncrement(expected)
		if err != nil {
			return nil, err
		}
		expected, err = checkedIncrement(expected)
		if err != nil {
			return nil, err
		}
	}
	if expected == 0 || expected != input.BeforeEpoch {
		return nil, ErrWriterDenied
	}
	return plan, nil
}

// 一个P0最多固定1024个物理writer及每类1024个逻辑角色；超限明确拒绝，避免目录漂移变成无限扫描。
const maxPlannedWriters = 1024
const maxClosureReceiptChain = 4096

func (c *Catalog) validatePlan(plan *pb.SqlWriterMaintenanceReceipt_Plan) error {
	if plan == nil || hasUnknownControl(plan) || len(plan.RunId) != 16 || plan.BeforeEpoch == 0 || plan.CatalogRevision == 0 || len(plan.ActiveWriterKeys) > maxPlannedWriters || !bytes.Equal(plan.PhysicalClusterIdentity, c.config.PhysicalAuthoritySHA256) || plan.AuthoritySchema != c.config.AuthoritySchema || !equalStrings(plan.ProtectedSchemas, c.config.ProtectedSchemas) {
		return ErrWriterDenied
	}
	keys := map[string]bool{}
	for i, key := range plan.ActiveWriterKeys {
		if len(key) != 16 || i > 0 && bytes.Compare(plan.ActiveWriterKeys[i-1], key) >= 0 {
			return ErrWriterDenied
		}
		keys[string(key)] = true
	}
	covered := map[string]bool{}
	for _, group := range [][]*pb.SqlWriterMaintenanceReceipt_IssuerClosure{plan.RootIssuers, plan.QueueMutators} {
		if len(group) > maxPlannedWriters {
			return ErrWriterDenied
		}
		seen := map[string]bool{}
		for _, seed := range group {
			if seed == nil || len(seed.IssuerId) != 16 || len(seed.InstanceId) != 16 || len(seed.PermanentRevivalFenceId) != 16 || seed.LogicalRole == "" || len(seed.LogicalRole) > 128 || len(seed.PhysicalWriterKey) != 0 && !keys[string(seed.PhysicalWriterKey)] || seed.AcceptingClosedMs != 0 || seed.AcceptedSettledMs != 0 || len(seed.AcceptedFrontierDigest) != 0 || seed.UnresolvedPublications != 0 || len(seed.SourceEvidence) == 0 {
				return ErrWriterDenied
			}
			id := string(seed.IssuerId) + seed.LogicalRole
			if seen[id] {
				return ErrWriterDenied
			}
			seen[id] = true
			if len(seed.PhysicalWriterKey) != 0 {
				covered[string(seed.PhysicalWriterKey)] = true
			}
		}
	}
	if len(covered) != len(keys) {
		return ErrWriterDenied
	}
	if len(plan.MaintenanceIssuers) > maxPlannedWriters {
		return ErrWriterDenied
	}
	toolIDs := map[string]bool{}
	for _, seed := range plan.MaintenanceIssuers {
		if seed == nil || len(seed.IssuerId) != 16 || len(seed.InstanceId) != 16 || len(seed.PermanentRevivalFenceId) != 16 || seed.LogicalRole != "MAINTENANCE" || len(seed.PhysicalWriterKey) != 0 || seed.AcceptingClosedMs != 0 || seed.AcceptedSettledMs != 0 || len(seed.AcceptedFrontierDigest) != 0 || seed.UnresolvedPublications != 0 || len(seed.SourceEvidence) == 0 || len(seed.SourceEvidence) > 128 || toolIDs[string(seed.IssuerId)] {
			return ErrWriterDenied
		}
		for _, group := range [][]*pb.SqlWriterMaintenanceReceipt_IssuerClosure{plan.RootIssuers, plan.QueueMutators} {
			for _, old := range group {
				if bytes.Equal(old.IssuerId, seed.IssuerId) || bytes.Equal(old.InstanceId, seed.InstanceId) {
					return ErrWriterDenied
				}
			}
		}
		toolIDs[string(seed.IssuerId)] = true
	}
	if len(plan.ManualAssignments) > 16384 {
		return ErrWriterDenied
	}
	manualSeen := map[string]bool{}
	for _, row := range plan.ManualAssignments {
		if row == nil || len(row.IssuerId) != 16 || len(row.InstanceId) != 16 || row.ConsumerId == "" || len(row.ConsumerId) > 256 || len(row.ClusterId) == 0 || len(row.TopicId) != 16 || row.TopicName == "" || row.PartitionCount == 0 || row.PartitionCount > 4096 || row.TargetNodeId == 0 || row.Partition != row.TargetNodeId%row.PartitionCount || !bytes.Equal(row.TargetInstanceId, row.InstanceId) || row.AssignmentRevision == 0 || row.StartOffset < 0 || row.Position < row.StartOffset || row.Earliest < 0 || row.Earliest > row.Position || row.ExclusiveEnd < row.Position || row.ObservedMs == 0 {
			return ErrWriterDenied
		}
		sourceFound := false
		for _, source := range plan.RootIssuers {
			if bytes.Equal(source.IssuerId, row.IssuerId) && bytes.Equal(source.InstanceId, row.InstanceId) {
				sourceFound = true
			}
		}
		key := fmt.Sprintf("%x/%q/%q/%d", row.IssuerId, row.ConsumerId, row.TopicName, row.Partition)
		if !sourceFound || manualSeen[key] {
			return ErrWriterDenied
		}
		manualSeen[key] = true
	}
	return nil
}

// foldRetirements只承认连续、同run、同E的永久RETIRE；P0的正文、目录版本及ACTIVE初值始终不变。
func (c *Catalog) foldRetirements(plan *pb.SqlWriterMaintenanceReceipt_Plan, revision uint64, receipts []*pb.SqlWriterLifecycleReceipt) ([][]byte, error) {
	if err := c.validatePlan(plan); err != nil {
		return nil, err
	}
	if revision < plan.CatalogRevision || revision-plan.CatalogRevision > uint64(len(plan.ActiveWriterKeys)) || uint64(len(receipts)) != revision-plan.CatalogRevision {
		return nil, ErrWriterDenied
	}
	active := map[string]bool{}
	for _, key := range plan.ActiveWriterKeys {
		active[string(key)] = true
	}
	for index, receipt := range receipts {
		if receipt == nil || hasUnknownControl(receipt) || receipt.Kind != pb.SqlWriterLifecycleReceipt_RETIRE || receipt.Generation != c.config.Generation || receipt.PreviousEpoch != plan.BeforeEpoch || receipt.ResultingEpoch != plan.BeforeEpoch || receipt.CatalogRevision != plan.CatalogRevision+uint64(index)+1 {
			return nil, ErrWriterDenied
		}
		request := &pb.SqlWriterLifecycleReceipt_Request{}
		if proto.Unmarshal(receipt.RequestProto, request) != nil {
			return nil, ErrWriterDenied
		}
		raw, digest, err := c.validateLifecycle(request, pb.SqlWriterLifecycleReceipt_RETIRE)
		if err != nil || !bytes.Equal(raw, receipt.RequestProto) || !bytes.Equal(digest, receipt.RequestDigest) || !bytes.Equal(request.RequestId, receipt.RequestId) || !bytes.Equal(request.MaintenanceRunId, plan.RunId) || request.ExpectedEpoch != plan.BeforeEpoch || request.ExpectedCatalogRevision != receipt.CatalogRevision-1 || !active[string(request.Writer.WriterKey)] {
			return nil, ErrWriterDenied
		}
		delete(active, string(request.Writer.WriterKey))
	}
	keys := make([][]byte, 0, len(active))
	for _, key := range plan.ActiveWriterKeys {
		if active[string(key)] {
			keys = append(keys, bytes.Clone(key))
		}
	}
	return keys, nil
}

func (c *Catalog) plannedActive(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, plan *pb.SqlWriterMaintenanceReceipt_Plan, revision uint64) ([][]byte, error) {
	if err := c.validatePlan(plan); err != nil {
		return nil, err
	}
	if revision < plan.CatalogRevision || revision-plan.CatalogRevision > uint64(len(plan.ActiveWriterKeys)) {
		return nil, ErrWriterDenied
	}
	rows, err := q.QueryContext(ctx, "SELECT request_id,kind,generation,request_digest,request_proto,previous_epoch,resulting_epoch,catalog_revision FROM "+c.table("writer_lifecycle_receipt")+" WHERE catalog_revision>? AND catalog_revision<=? ORDER BY catalog_revision,request_id LIMIT ?", plan.CatalogRevision, revision, len(plan.ActiveWriterKeys)+1)
	if err != nil {
		return nil, err
	}
	var receipts []*pb.SqlWriterLifecycleReceipt
	for rows.Next() {
		r := &pb.SqlWriterLifecycleReceipt{}
		if err = rows.Scan(&r.RequestId, &r.Kind, &r.Generation, &r.RequestDigest, &r.RequestProto, &r.PreviousEpoch, &r.ResultingEpoch, &r.CatalogRevision); err != nil {
			break
		}
		receipts = append(receipts, r)
	}
	rowErr, closeErr := rows.Err(), rows.Close()
	if err != nil {
		return nil, err
	}
	if rowErr != nil {
		return nil, rowErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return c.foldRetirements(plan, revision, receipts)
}

func closureMatches(seed, closed *pb.SqlWriterMaintenanceReceipt_IssuerClosure) bool {
	if seed == nil || closed == nil || hasUnknownControl(closed) || closed.AcceptingClosedMs == 0 || closed.AcceptedSettledMs < closed.AcceptingClosedMs || closed.UnresolvedPublications != 0 || len(closed.AcceptedFrontierDigest) != 32 {
		return false
	}
	identity := proto.Clone(closed).(*pb.SqlWriterMaintenanceReceipt_IssuerClosure)
	identity.AcceptingClosedMs = 0
	identity.AcceptedSettledMs = 0
	identity.AcceptedFrontierDigest = nil
	identity.UnresolvedPublications = 0
	return proto.Equal(seed, identity)
}

// validateMaintenanceClosure 只校验已固定的维护工具全集；原文件读取由外层portable owner在事务外完成。
// P7与释放提交都调用它，空工具目录不接受事后增加的发行者。
func validateMaintenanceClosure(plan *pb.SqlWriterMaintenanceReceipt_Plan, ready *pb.SqlWriterMaintenanceReceipt_ReleaseReadiness, frozenMS uint64) error {
	if plan == nil || ready == nil {
		return ErrWriterDenied
	}
	publication := ready.MaintenancePublication
	if len(plan.MaintenanceIssuers) == 0 {
		if publication != nil {
			return ErrWriterDenied
		}
		return nil
	}
	if frozenMS == 0 || publication == nil || hasUnknownControl(publication) || len(publication.LogicalRoles) != len(plan.MaintenanceIssuers) || len(publication.SourceEvidence) != len(plan.MaintenanceIssuers) {
		return ErrWriterDenied
	}
	inventory := &pb.SqlWriterMaintenanceReceipt_PublicationEvidence{LogicalRoles: plan.MaintenanceIssuers}
	digest, err := messageDigest(inventory)
	if err != nil || !bytes.Equal(digest, publication.IssuerInventoryDigest) {
		return ErrWriterDenied
	}
	expected := map[string]*pb.SqlWriterMaintenanceReceipt_IssuerClosure{}
	for _, seed := range plan.MaintenanceIssuers {
		expected[string(seed.IssuerId)] = seed
	}
	for _, closed := range publication.LogicalRoles {
		if closed == nil || closed.AcceptingClosedMs < frozenMS || !closureMatches(expected[string(closed.IssuerId)], closed) {
			return ErrWriterDenied
		}
		delete(expected, string(closed.IssuerId))
	}
	if len(expected) != 0 {
		return ErrWriterDenied
	}
	return nil
}

// authorizeRunRetirement在own slots→meta→run之后只读SQL永久事实，不在锁内访问文件、Kafka或Redis。
func (c *Catalog) authorizeRunRetirement(ctx context.Context, tx *sql.Tx, run *pb.SqlWriterDrainRun, request *pb.SqlWriterLifecycleReceipt_Request, revision uint64) error {
	if run.Generation != c.config.Generation || (run.Phase != pb.SqlWriterDrainRun_ROOTS_CLOSED && run.Phase != pb.SqlWriterDrainRun_DRAINING) || !bytes.Equal(run.OperationId, request.MaintenanceRunId) {
		return ErrWriterDenied
	}
	plan := &pb.SqlWriterMaintenanceReceipt_Plan{}
	if proto.Unmarshal(run.ManifestProto, plan) != nil || c.validatePlan(plan) != nil || !bytes.Equal(plan.RunId, run.OperationId) || plan.BeforeEpoch != request.ExpectedEpoch {
		return ErrWriterDenied
	}
	manifest := sha256.Sum256(run.ManifestProto)
	if !bytes.Equal(manifest[:], run.ManifestDigest) {
		return ErrWriterDenied
	}
	expected, err := c.plannedActive(ctx, tx, plan, revision)
	if err != nil {
		return err
	}
	current, err := c.active(ctx, tx, false)
	if err != nil {
		return err
	}
	if !exactKeys(expected, current) {
		return ErrWriterDenied
	}
	wanted := map[string]*pb.SqlWriterMaintenanceReceipt_IssuerClosure{}
	relay := map[string]bool{}
	for groupIndex, group := range [][]*pb.SqlWriterMaintenanceReceipt_IssuerClosure{plan.RootIssuers, plan.QueueMutators} {
		for _, seed := range group {
			if !bytes.Equal(seed.PhysicalWriterKey, request.Writer.WriterKey) {
				continue
			}
			if !bytes.Equal(seed.InstanceId, request.Writer.InstanceId) {
				return ErrWriterDenied
			}
			role := string(seed.IssuerId) + seed.LogicalRole
			if _, duplicate := wanted[role]; duplicate {
				return ErrWriterDenied
			}
			wanted[role] = seed
			relay[role] = groupIndex == 1
		}
	}
	if len(wanted) == 0 {
		return ErrWriterDenied
	}
	for role := range wanted {
		if relay[role] && run.Phase != pb.SqlWriterDrainRun_DRAINING {
			return ErrWriterDenied
		}
	}
	id, digest := run.LatestReceiptId, run.LatestReceiptDigest
	seen := map[string]bool{}
	for count := 0; len(id) != 0 && count < maxClosureReceiptChain; count++ {
		if len(id) != 16 || len(digest) != 32 || seen[string(id)] {
			return ErrWriterDenied
		}
		seen[string(id)] = true
		r, err := c.readReceipt(ctx, tx, id)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.ReceiptDigest, digest) || !bytes.Equal(r.RunId, run.OperationId) || r.Generation != run.Generation || !bytes.Equal(r.PlanProto, run.ManifestProto) || r.AttemptRevision == 0 || r.AttemptRevision > run.AttemptRevision || r.Evidence == nil || !bytes.Equal(r.Evidence.OperationId, run.OperationId) || !bytes.Equal(r.Evidence.ManifestDigest, run.ManifestDigest) {
			return ErrWriterDenied
		}
		if publication := r.Evidence.GetPublication(); publication != nil {
			rootFact := r.RunPhase == pb.SqlWriterDrainRun_ROOTS_CLOSED && (r.Kind == pb.SqlWriterMaintenanceReceipt_ROLE_CLOSED || r.Kind == pb.SqlWriterMaintenanceReceipt_PHASE_EVENT)
			relayFact := r.RunPhase == pb.SqlWriterDrainRun_DRAINING && r.Kind == pb.SqlWriterMaintenanceReceipt_RELAY_QUIESCED
			for _, closed := range publication.LogicalRoles {
				if closed == nil {
					return ErrWriterDenied
				}
				role := string(closed.IssuerId) + closed.LogicalRole
				seed := wanted[role]
				if seed == nil {
					continue
				}
				if (!relay[role] && rootFact || relay[role] && relayFact) && closureMatches(seed, closed) {
					delete(wanted, role)
				}
			}
		}
		if len(wanted) == 0 {
			return nil
		}
		id, digest = r.PriorReceiptId, r.PriorReceiptDigest
	}
	return ErrWriterDenied
}

// Freeze唯有SQLticket可达；先全slot，再reconcile→meta→run→ticket，锁内外部IO为零。
func (c *Catalog) Freeze(ctx context.Context, ticketID []byte, input *pb.SqlWriterMaintenanceReceipt) (*pb.SqlWriterMaintenanceReceipt, error) {
	return c.changeWritable(ctx, ticketID, input, false)
}
func (c *Catalog) Release(ctx context.Context, ticketID []byte, input *pb.SqlWriterMaintenanceReceipt) (*pb.SqlWriterMaintenanceReceipt, error) {
	return c.changeWritable(ctx, ticketID, input, true)
}
func (c *Catalog) changeWritable(ctx context.Context, ticketID []byte, input *pb.SqlWriterMaintenanceReceipt, release bool) (*pb.SqlWriterMaintenanceReceipt, error) {
	if err := c.checkRequest(input); err != nil {
		return nil, err
	}
	plan, err := c.checkPlan(input)
	if err != nil {
		return nil, err
	}
	expectedKind := pb.SqlWriterMaintenanceReceipt_FREEZE
	if release {
		expectedKind = pb.SqlWriterMaintenanceReceipt_RELEASE
	}
	if input.Kind != expectedKind && !(input.Kind == pb.SqlWriterMaintenanceReceipt_EMERGENCY_FREEZE && !release) {
		return nil, ErrWriterDenied
	}
	if len(ticketID) != 16 {
		return nil, ErrWriterDenied
	}
	boundTicket, err := c.readTicket(ctx, c.db, ticketID, false)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(boundTicket.OperationId, input.RunId) || boundTicket.Generation != input.Generation || boundTicket.AttemptRevision != input.AttemptRevision || !bytes.Equal(boundTicket.ManifestDigest, input.Evidence.ManifestDigest) {
		return nil, ErrWriterDenied
	}
	if release {
		proof := input.Evidence.GetReleaseReadiness()
		if proof == nil || !bytes.Equal(proof.OriginalFreezeReceiptId, boundTicket.FreezeRequestId) || !bytes.Equal(proof.OriginalFreezeReceiptDigest, boundTicket.ConsumedReceiptDigest) {
			return nil, ErrWriterDenied
		}
	} else if !bytes.Equal(boundTicket.FreezeRequestId, input.RequestId) || !bytes.Equal(boundTicket.FreezeRequestDigest, input.RequestDigest) {
		return nil, ErrWriterDenied
	}
	if old, readErr := c.existingReceipt(ctx, input); readErr == nil {
		return old, nil
	} else if !errors.Is(readErr, sql.ErrNoRows) {
		return nil, readErr
	}
	nextEpoch, err := checkedIncrement(input.BeforeEpoch)
	if err != nil || input.AfterEpoch != nextEpoch {
		return nil, ErrWriterDenied
	}
	// 正常流程到phase5已完成全部P1/P3退役；紧急流程允许仍活跃的精确目录。预读不改变P0。
	plannedKeys, err := c.plannedActive(ctx, c.db, plan, input.CatalogRevision)
	if err != nil {
		return nil, err
	}
	var result *pb.SqlWriterMaintenanceReceipt
	var observedDeadline time.Time
	err = c.transaction(ctx, func(tx *sql.Tx) error {
		if err := c.lockSlots(ctx, tx, plannedKeys, input.BeforeEpoch); err != nil {
			return err
		}
		if err := c.lockReconcile(ctx, tx); err != nil {
			return err
		}
		meta, err := c.meta(ctx, tx, true)
		if err != nil {
			return err
		}
		if meta.WriterEpoch != input.BeforeEpoch || meta.CatalogRevision != input.CatalogRevision || !bytes.Equal(meta.FreezeRunId, input.RunId) {
			return ErrWriterDenied
		}
		folded, err := c.plannedActive(ctx, tx, plan, meta.CatalogRevision)
		if err != nil {
			return err
		}
		if !exactKeys(folded, plannedKeys) {
			return ErrWriterDenied
		}
		keys, err := c.active(ctx, tx, true)
		if err != nil {
			return err
		}
		if !exactKeys(keys, plannedKeys) {
			return ErrWriterDenied
		}
		run, err := c.readRun(ctx, tx, input.RunId, true)
		if err != nil {
			return err
		}
		ticket, err := c.readTicket(ctx, tx, ticketID, true)
		if err != nil {
			return err
		}
		if run.Generation != input.Generation || ticket.Generation != input.Generation || run.AttemptRevision != input.AttemptRevision || ticket.AttemptRevision != input.AttemptRevision || !bytes.Equal(ticket.OperationId, input.RunId) || !bytes.Equal(run.ManifestProto, input.PlanProto) || !bytes.Equal(run.ManifestDigest, input.Evidence.ManifestDigest) || !bytes.Equal(ticket.ManifestDigest, run.ManifestDigest) {
			return ErrWriterDenied
		}
		if release {
			if !bytes.Equal(run.LatestReceiptId, input.PriorReceiptId) || !bytes.Equal(run.LatestReceiptDigest, input.PriorReceiptDigest) {
				return ErrWriterDenied
			}
			if meta.Writable != 0 || run.Phase != pb.SqlWriterDrainRun_RELEASE_READY || ticket.State != pb.SqlWriterFreezeTicket_CONSUMED || input.Phase != pb.SqlWriterMaintenanceReceipt_RELEASED || input.RunPhase != pb.SqlWriterDrainRun_SQL_RELEASED {
				return ErrWriterDenied
			}
			ready, err := c.readReceipt(ctx, tx, run.LatestReceiptId)
			if err != nil {
				return err
			}
			proof := ready.Evidence.GetReleaseReadiness()
			if ready.RunPhase != pb.SqlWriterDrainRun_RELEASE_READY || !bytes.Equal(ready.ReceiptDigest, run.LatestReceiptDigest) || proof == nil || len(proof.RecoveryAudit) == 0 || !bytes.Equal(proof.OriginalFreezeReceiptId, run.FrozenRequestId) {
				return ErrWriterDenied
			}
			if !proto.Equal(proof, input.Evidence.GetReleaseReadiness()) {
				return ErrWriterDenied
			}
			if !c.config.RequireReconcile && proof.RouteWatermarkFloorMs != 0 {
				return ErrWriterDenied
			}
			frozen, err := c.readReceipt(ctx, tx, run.FrozenRequestId)
			if err != nil {
				return err
			}
			if frozen.Kind != pb.SqlWriterMaintenanceReceipt_FREEZE || frozen.Phase != pb.SqlWriterMaintenanceReceipt_FROZEN || frozen.BeforeEpoch != plan.BeforeEpoch || frozen.AfterEpoch != input.BeforeEpoch || !bytes.Equal(frozen.PlanProto, run.ManifestProto) || !bytes.Equal(frozen.ReceiptDigest, ticket.ConsumedReceiptDigest) || !bytes.Equal(frozen.ReceiptDigest, proof.OriginalFreezeReceiptDigest) {
				return ErrWriterDenied
			}
			if err := validateMaintenanceClosure(plan, proof, frozen.CommittedMs); err != nil {
				return err
			}
			closed, err := c.readReceipt(ctx, tx, proof.NonSqlClosedReceiptId)
			if err != nil {
				return err
			}
			if closed.RunPhase != pb.SqlWriterDrainRun_NON_SQL_CLOSED || !bytes.Equal(closed.RunId, run.OperationId) || !bytes.Equal(closed.ReceiptDigest, proof.NonSqlClosedReceiptDigest) || !bytes.Equal(closed.ReceiptDigest, ticket.NonSqlReceiptDigest) {
				return ErrWriterDenied
			}
		} else {
			if meta.Writable != 1 || run.Phase != pb.SqlWriterDrainRun_FREEZE_AUTHORIZED || ticket.State != pb.SqlWriterFreezeTicket_AVAILABLE || !bytes.Equal(ticket.FreezeRequestId, input.RequestId) || !bytes.Equal(ticket.FreezeRequestDigest, input.RequestDigest) {
				return ErrWriterDenied
			}
			// 输入指向已知pre-grant事实；ticket永久绑定新grant，避免freeze摘要与grant收据互相依赖。
			grant, err := c.readReceipt(ctx, tx, ticket.GrantReceiptId)
			if err != nil {
				return err
			}
			if grant.Kind != pb.SqlWriterMaintenanceReceipt_TICKET_GRANTED || grant.RunPhase != pb.SqlWriterDrainRun_FREEZE_AUTHORIZED || !bytes.Equal(grant.RunId, input.RunId) || grant.Generation != input.Generation || grant.AttemptRevision != input.AttemptRevision || !bytes.Equal(grant.PlanProto, input.PlanProto) || !bytes.Equal(grant.ReceiptDigest, ticket.GrantReceiptDigest) || !bytes.Equal(run.LatestReceiptId, grant.RequestId) || !bytes.Equal(run.LatestReceiptDigest, grant.ReceiptDigest) || !bytes.Equal(grant.PriorReceiptId, input.PriorReceiptId) || !bytes.Equal(grant.PriorReceiptDigest, input.PriorReceiptDigest) {
				return ErrWriterDenied
			}
			prior, err := c.readReceipt(ctx, tx, input.PriorReceiptId)
			if err != nil {
				return err
			}
			if !bytes.Equal(prior.ReceiptDigest, input.PriorReceiptDigest) || !bytes.Equal(prior.RunId, input.RunId) || prior.AttemptRevision != input.AttemptRevision || !bytes.Equal(prior.PlanProto, input.PlanProto) {
				return ErrWriterDenied
			}
			if ticket.AuthorizationKind == pb.SqlWriterFreezeTicket_NORMAL_DRAIN_FREEZE && (prior.RunPhase != pb.SqlWriterDrainRun_NON_SQL_CLOSED || !bytes.Equal(prior.ReceiptDigest, ticket.NonSqlReceiptDigest) || len(keys) != 0) {
				return ErrWriterDenied
			}
			now, err := sqlClock(ctx, tx)
			if err != nil {
				return err
			}
			if now < ticket.NotBeforeMs || now >= ticket.ExpiresMs {
				return ErrWriterDenied
			}
			observedDeadline = time.Now().Add(time.Duration(ticket.ExpiresMs-now) * time.Millisecond)
			if ticket.AuthorizationKind == pb.SqlWriterFreezeTicket_NORMAL_DRAIN_FREEZE {
				if input.Kind != pb.SqlWriterMaintenanceReceipt_FREEZE || input.Phase != pb.SqlWriterMaintenanceReceipt_FROZEN || input.RunPhase != pb.SqlWriterDrainRun_SQL_FROZEN || len(ticket.NonSqlReceiptDigest) != 32 {
					return ErrWriterDenied
				}
			} else if ticket.AuthorizationKind == pb.SqlWriterFreezeTicket_EMERGENCY_HOLD_FREEZE {
				if input.Kind != pb.SqlWriterMaintenanceReceipt_EMERGENCY_FREEZE || input.Phase != pb.SqlWriterMaintenanceReceipt_PHASE_UNKNOWN || input.RunPhase != pb.SqlWriterDrainRun_EMERGENCY_FROZEN_HOLD || input.Evidence.GetEmergency() == nil {
					return ErrWriterDenied
				}
			} else {
				return ErrWriterDenied
			}
		}
		for _, key := range keys {
			result, err := tx.ExecContext(ctx, "UPDATE "+c.table("writer_slot")+" SET writer_epoch=? WHERE writer_key=? AND writer_epoch=? AND state=1", nextEpoch, key, input.BeforeEpoch)
			if err != nil {
				return err
			}
			if err = affected(result, WriterSlotCount); err != nil {
				return err
			}
		}
		if release && c.config.RequireReconcile {
			if _, err = tx.ExecContext(ctx, "UPDATE "+c.table("route_reconcile")+" SET watermark_ms=GREATEST(watermark_ms,?) WHERE id=1", input.Evidence.GetReleaseReadiness().RouteWatermarkFloorMs); err != nil {
				return err
			}
		}
		writable := 0
		if release {
			writable = 1
		}
		if _, err = tx.ExecContext(ctx, "UPDATE "+c.table("route_meta")+" SET writer_epoch=?,writable=? WHERE id=1", nextEpoch, writable); err != nil {
			return err
		}
		result, err = c.insertReceipt(ctx, tx, input)
		if err != nil {
			return err
		}
		if !release {
			if _, err = tx.ExecContext(ctx, "UPDATE "+c.table("sql_writer_freeze_ticket")+" SET state=2,consumed_receipt_digest=?,updated_ms=? WHERE ticket_id=?", result.ReceiptDigest, result.CommittedMs, ticketID); err != nil {
				return err
			}
			run.FrozenRequestId = bytes.Clone(input.RequestId)
		}
		run.Phase = input.RunPhase
		return c.writeRun(ctx, tx, run, result)
	})
	if err != nil {
		return nil, err
	}
	if !observedDeadline.IsZero() && !time.Now().Before(observedDeadline) {
		return nil, unknown("freeze回包跨授权窗", nil)
	}
	return result, nil
}

// VerifyPublicationPrestart只允许同run永久释放后的prepare-only启动；调用者不能据此开放SOURCE。
// 校验锁与SQL转换共用reconcile→meta→run顺序，结束后由控制器保持同run launcher围栏。
func (c *Catalog) VerifyPublicationPrestart(ctx context.Context, released *pb.SqlWriterMaintenanceReceipt) (*pb.SqlWriterMaintenanceReceipt, error) {
	if released == nil || hasUnknownControl(released) || c.checkRequest(released) != nil {
		return nil, ErrWriterDenied
	}
	var result *pb.SqlWriterMaintenanceReceipt
	err := c.transaction(ctx, func(tx *sql.Tx) error {
		current, _, err := c.publicationReleased(ctx, tx, released.RunId)
		if err != nil {
			return err
		}
		if !proto.Equal(current, released) {
			return ErrWriterDenied
		}
		result = current
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// VerifyPublicationRuntime核实际prepared boot与当前SQL永久事实；runtime只是绑定模板，不是RELEASE收据。
// 此方法不读文件或开放入口。控制器须先验私有原件SHA，再于返回成功后独占写open请求；
// SQL与文件没有分布式提交，节点仍必须在同一consumer owner lease内复核并开放。
func (c *Catalog) VerifyPublicationRuntime(ctx context.Context, runtime *pb.SqlWriterMaintenanceReceipt, ready *pb.SqlWriterMaintenanceReceipt_IssuerReadiness) (*pb.SqlWriterMaintenanceReceipt, error) {
	if runtime == nil || ready == nil || hasUnknownControl(runtime) || hasUnknownControl(ready) || runtime.RunPhase != pb.SqlWriterDrainRun_SQL_RELEASED || runtime.Evidence == nil {
		return nil, ErrWriterDenied
	}
	plan := &pb.SqlWriterMaintenanceReceipt_Plan{}
	if proto.Unmarshal(runtime.PlanProto, plan) != nil || c.validatePlan(plan) != nil {
		return nil, ErrWriterDenied
	}
	var result *pb.SqlWriterMaintenanceReceipt
	err := c.transaction(ctx, func(tx *sql.Tx) error {
		released, meta, err := c.publicationReleased(ctx, tx, runtime.RunId)
		if err != nil {
			return err
		}
		if err = validatePublicationRuntime(runtime, ready, plan, released); err != nil {
			return err
		}
		now, err := sqlClock(ctx, tx)
		if err != nil {
			return err
		}
		if ready.PreparedClosedMs > now {
			return ErrWriterDenied
		}
		if len(ready.PhysicalWriterKey) != 0 {
			registration, err := c.readLifecycleReceipt(ctx, tx, ready.RegistrationReceiptId)
			if err != nil {
				return err
			}
			request := &pb.SqlWriterLifecycleReceipt_Request{}
			if proto.Unmarshal(registration.RequestProto, request) != nil || request.Writer == nil || registration.Kind != pb.SqlWriterLifecycleReceipt_REGISTER || !bytes.Equal(registration.RequestDigest, ready.RegistrationReceiptDigest) || registration.ResultingEpoch != meta.WriterEpoch || registration.CatalogRevision > meta.CatalogRevision || !bytes.Equal(request.MaintenanceRunId, runtime.RunId) || !bytes.Equal(request.Writer.WriterKey, ready.PhysicalWriterKey) || !bytes.Equal(request.Writer.InstanceId, ready.InstanceId) {
				return ErrWriterDenied
			}
			writer := &pb.SqlWriterRegistry{}
			if err = tx.QueryRowContext(ctx, "SELECT generation,instance_id,sql_principal,registration_digest,state FROM "+c.table("writer_registry")+" WHERE writer_key=?", ready.PhysicalWriterKey).Scan(&writer.Generation, &writer.InstanceId, &writer.SqlPrincipal, &writer.RegistrationDigest, &writer.State); err != nil {
				return err
			}
			if writer.State != pb.SqlWriterRegistry_ACTIVE || writer.Generation != c.config.Generation || !bytes.Equal(writer.InstanceId, ready.InstanceId) || !bytes.Equal(writer.SqlPrincipal, request.Writer.SqlPrincipal) || !bytes.Equal(writer.RegistrationDigest, request.Writer.RegistrationDigest) {
				return ErrWriterDenied
			}
			var allSlots, activeSlots uint32
			if err = tx.QueryRowContext(ctx, "SELECT COUNT(*),COALESCE(SUM(CASE WHEN generation=? AND writer_epoch=? AND state=1 AND slot<? THEN 1 ELSE 0 END),0) FROM "+c.table("writer_slot")+" WHERE writer_key=?", c.config.Generation, meta.WriterEpoch, WriterSlotCount, ready.PhysicalWriterKey).Scan(&allSlots, &activeSlots); err != nil {
				return err
			}
			if allSlots != WriterSlotCount || activeSlots != WriterSlotCount {
				return ErrWriterDenied
			}
		}
		result = released
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (c *Catalog) publicationReleased(ctx context.Context, tx *sql.Tx, runID []byte) (*pb.SqlWriterMaintenanceReceipt, *pb.RouteMeta, error) {
	if len(runID) != 16 {
		return nil, nil, ErrWriterDenied
	}
	if err := c.lockReconcile(ctx, tx); err != nil {
		return nil, nil, err
	}
	meta, err := c.meta(ctx, tx, true)
	if err != nil {
		return nil, nil, err
	}
	if meta.Writable != 1 || !bytes.Equal(meta.FreezeRunId, runID) {
		return nil, nil, ErrWriterDenied
	}
	run, err := c.readRun(ctx, tx, runID, true)
	if err != nil {
		return nil, nil, err
	}
	if run.Generation != c.config.Generation || run.Phase != pb.SqlWriterDrainRun_SQL_RELEASED || len(run.LatestReceiptId) != 16 || len(run.LatestReceiptDigest) != 32 {
		return nil, nil, ErrWriterDenied
	}
	released, err := c.readReceipt(ctx, tx, run.LatestReceiptId)
	if err != nil {
		return nil, nil, err
	}
	if released.Kind != pb.SqlWriterMaintenanceReceipt_RELEASE || released.Phase != pb.SqlWriterMaintenanceReceipt_RELEASED || released.RunPhase != pb.SqlWriterDrainRun_SQL_RELEASED || !bytes.Equal(released.RunId, runID) || released.Generation != run.Generation || released.AttemptRevision != run.AttemptRevision || released.AfterEpoch != meta.WriterEpoch || released.CatalogRevision > meta.CatalogRevision || !bytes.Equal(released.PlanProto, run.ManifestProto) || !bytes.Equal(released.Evidence.ManifestDigest, run.ManifestDigest) || !bytes.Equal(released.ReceiptDigest, run.LatestReceiptDigest) {
		return nil, nil, ErrWriterDenied
	}
	if _, err = c.checkPlan(released); err != nil {
		return nil, nil, err
	}
	return released, meta, nil
}

func validatePublicationRuntime(runtime *pb.SqlWriterMaintenanceReceipt, ready *pb.SqlWriterMaintenanceReceipt_IssuerReadiness, plan *pb.SqlWriterMaintenanceReceipt_Plan, released *pb.SqlWriterMaintenanceReceipt) error {
	if runtime == nil || ready == nil || plan == nil || released == nil || runtime.Evidence == nil || released.Evidence == nil || runtime.RunPhase != pb.SqlWriterDrainRun_SQL_RELEASED || !bytes.Equal(runtime.RunId, released.RunId) || !bytes.Equal(plan.RunId, released.RunId) || runtime.Generation != released.Generation || runtime.AttemptRevision != released.AttemptRevision || runtime.AfterEpoch != released.AfterEpoch || !bytes.Equal(runtime.PlanProto, released.PlanProto) || !bytes.Equal(runtime.Evidence.OperationId, released.RunId) || !bytes.Equal(runtime.Evidence.ManifestDigest, released.Evidence.ManifestDigest) {
		return ErrWriterDenied
	}
	publication := runtime.Evidence.GetPublication()
	if publication == nil || len(publication.LogicalRoles) == 0 || len(publication.LogicalRoles) > 2 || ready.LogicalRole != "SOURCE" || len(ready.IssuerId) != 16 || len(ready.InstanceId) != 16 || ready.PreparedClosedMs < released.CommittedMs || len(ready.ConsumerReadyFrontierDigest) != 32 || len(ready.SourcePins) == 0 {
		return ErrWriterDenied
	}
	oldRoles := map[string]*pb.SqlWriterMaintenanceReceipt_IssuerClosure{}
	oldKeys := map[string]bool{}
	oldBoots := map[string]bool{}
	for _, group := range [][]*pb.SqlWriterMaintenanceReceipt_IssuerClosure{plan.RootIssuers, plan.QueueMutators} {
		for _, seed := range group {
			if seed == nil {
				return ErrWriterDenied
			}
			oldRoles[string(seed.IssuerId)+"/"+seed.LogicalRole] = seed
			oldBoots[string(seed.InstanceId)] = true
			if len(seed.PhysicalWriterKey) != 0 {
				oldKeys[string(seed.PhysicalWriterKey)] = true
			}
		}
	}
	sourceOld := oldRoles[string(ready.IssuerId)+"/SOURCE"]
	if sourceOld == nil || oldBoots[string(ready.InstanceId)] || oldKeys[string(ready.PhysicalWriterKey)] {
		return ErrWriterDenied
	}
	seen := map[string]bool{}
	var sourceSeed *pb.SqlWriterMaintenanceReceipt_IssuerClosure
	for _, seed := range publication.LogicalRoles {
		if seed == nil {
			return ErrWriterDenied
		}
		old := oldRoles[string(seed.IssuerId)+"/"+seed.LogicalRole]
		if old == nil || old.LogicalRole != seed.LogicalRole || !bytes.Equal(old.InstanceId, sourceOld.InstanceId) || !bytes.Equal(old.PhysicalWriterKey, sourceOld.PhysicalWriterKey) || seen[seed.LogicalRole] || (seed.LogicalRole != "SOURCE" && seed.LogicalRole != "RELAY") || !bytes.Equal(seed.InstanceId, ready.InstanceId) || !bytes.Equal(seed.PhysicalWriterKey, ready.PhysicalWriterKey) || len(seed.PermanentRevivalFenceId) != 16 || len(seed.SourceEvidence) == 0 || seed.AcceptingClosedMs != 0 || seed.AcceptedSettledMs != 0 || seed.UnresolvedPublications != 0 || len(seed.AcceptedFrontierDigest) != 0 {
			return ErrWriterDenied
		}
		if len(old.PhysicalWriterKey) == 0 {
			if len(seed.PhysicalWriterKey) != 0 {
				return ErrWriterDenied
			}
		} else if len(seed.PhysicalWriterKey) != 16 {
			return ErrWriterDenied
		}
		seen[seed.LogicalRole] = true
		if seed.LogicalRole == "SOURCE" {
			if !bytes.Equal(seed.IssuerId, ready.IssuerId) {
				return ErrWriterDenied
			}
			sourceSeed = seed
		}
	}
	if sourceSeed == nil || len(ready.SourcePins) != len(sourceSeed.SourceEvidence)+1 {
		return ErrWriterDenied
	}
	for index, pin := range sourceSeed.SourceEvidence {
		if !proto.Equal(pin, ready.SourcePins[index]) {
			return ErrWriterDenied
		}
	}
	if !bytes.Equal(ready.SourcePins[len(ready.SourcePins)-1].GetSha256(), ready.ConsumerReadyFrontierDigest) {
		return ErrWriterDenied
	}
	if len(ready.PhysicalWriterKey) == 0 {
		if len(ready.RegistrationReceiptId) != 0 || len(ready.RegistrationReceiptDigest) != 0 {
			return ErrWriterDenied
		}
	} else if len(ready.RegistrationReceiptId) != 16 || len(ready.RegistrationReceiptDigest) != 32 {
		return ErrWriterDenied
	}
	frontierPins := 0
	seenPins := map[string]bool{}
	for _, pin := range ready.SourcePins {
		if pin == nil || pin.AbsolutePath == "" || len(pin.Sha256) != 32 || seenPins[pin.AbsolutePath] {
			return ErrWriterDenied
		}
		seenPins[pin.AbsolutePath] = true
		if bytes.Equal(pin.Sha256, ready.ConsumerReadyFrontierDigest) {
			frontierPins++
		}
	}
	if frontierPins != 1 {
		return ErrWriterDenied
	}
	return nil
}
