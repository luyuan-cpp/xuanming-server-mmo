// Package ownerauthority implements linearizable SQL-backed player ownership
// authority and fencing (ported from xuanming-server/services/runtime/owner).
// See docs/design/linearizable-owner-authority.md for invariants and lock order.
package ownerauthority

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"

	"shared/authoritysql"
)

const (
	// DefaultSkewMargin is added to an outgoing holder's lease deadline when
	// computing admit_not_before_ms across instance handoffs.
	DefaultSkewMargin = 2 * time.Second

	// MaxLeaseSeconds caps RenewInstanceLease to bound split-brain wait windows.
	MaxLeaseSeconds int64 = 30

	maxDetailLen = 512
)

// DDLMySQL defines the MySQL-compatible schema for the three authority tables.
const DDLMySQL = `
CREATE TABLE IF NOT EXISTS owner_record (
    player_id          BIGINT UNSIGNED NOT NULL,
    owner_epoch        BIGINT UNSIGNED NOT NULL,
    owner_type         TINYINT         NOT NULL DEFAULT 0,
    phase              TINYINT         NOT NULL DEFAULT 0,
    pod_name           VARCHAR(128)    NOT NULL DEFAULT '',
    instance_uid       VARCHAR(128)    NOT NULL DEFAULT '',
    instance_epoch     INT UNSIGNED    NOT NULL DEFAULT 0,
    assignment_id      VARCHAR(128)    NOT NULL DEFAULT '',
    release_track      VARCHAR(64)     NOT NULL DEFAULT '',
    operation_id       CHAR(36)        NOT NULL DEFAULT '',
    admit_not_before_ms BIGINT         NOT NULL DEFAULT 0,
    source_revision    BIGINT UNSIGNED NOT NULL DEFAULT 0,
    updated_at_ms      BIGINT          NOT NULL DEFAULT 0,
    PRIMARY KEY (player_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS owner_instance_lease (
    pod_name          VARCHAR(128) NOT NULL,
    instance_uid      VARCHAR(128) NOT NULL,
    instance_epoch    INT UNSIGNED NOT NULL,
    lease_deadline_ms BIGINT       NOT NULL,
    updated_at_ms     BIGINT       NOT NULL,
    PRIMARY KEY (pod_name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS owner_transition_log (
    id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    player_id     BIGINT UNSIGNED NOT NULL,
    owner_epoch   BIGINT UNSIGNED NOT NULL,
    operation_id  CHAR(36)        NOT NULL,
    action        VARCHAR(32)     NOT NULL,
    detail        VARCHAR(512)    NOT NULL DEFAULT '',
    created_at_ms BIGINT          NOT NULL,
    PRIMARY KEY (id),
    KEY idx_player_epoch (player_id, owner_epoch),
    KEY idx_created_at (created_at_ms)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
`

// DDLTiDB defines the TiDB-optimized schema with non-clustered PK region splitting.
const DDLTiDB = `
CREATE TABLE IF NOT EXISTS owner_record (
    player_id          BIGINT UNSIGNED NOT NULL,
    owner_epoch        BIGINT UNSIGNED NOT NULL,
    owner_type         TINYINT         NOT NULL DEFAULT 0,
    phase              TINYINT         NOT NULL DEFAULT 0,
    pod_name           VARCHAR(128)    NOT NULL DEFAULT '',
    instance_uid       VARCHAR(128)    NOT NULL DEFAULT '',
    instance_epoch     INT UNSIGNED    NOT NULL DEFAULT 0,
    assignment_id      VARCHAR(128)    NOT NULL DEFAULT '',
    release_track      VARCHAR(64)     NOT NULL DEFAULT '',
    operation_id       CHAR(36)        NOT NULL DEFAULT '',
    admit_not_before_ms BIGINT         NOT NULL DEFAULT 0,
    source_revision    BIGINT UNSIGNED NOT NULL DEFAULT 0,
    updated_at_ms      BIGINT          NOT NULL DEFAULT 0,
    PRIMARY KEY (player_id) /*T![clustered_index] NONCLUSTERED */
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin
  SHARD_ROW_ID_BITS = 4 PRE_SPLIT_REGIONS = 4;

CREATE TABLE IF NOT EXISTS owner_instance_lease (
    pod_name          VARCHAR(128) NOT NULL,
    instance_uid      VARCHAR(128) NOT NULL,
    instance_epoch    INT UNSIGNED NOT NULL,
    lease_deadline_ms BIGINT       NOT NULL,
    updated_at_ms     BIGINT       NOT NULL,
    PRIMARY KEY (pod_name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS owner_transition_log (
    id            BIGINT UNSIGNED NOT NULL /*T![auto_rand] AUTO_RANDOM(5) */,
    player_id     BIGINT UNSIGNED NOT NULL,
    owner_epoch   BIGINT UNSIGNED NOT NULL,
    operation_id  CHAR(36)        NOT NULL,
    action        VARCHAR(32)     NOT NULL,
    detail        VARCHAR(512)    NOT NULL DEFAULT '',
    created_at_ms BIGINT          NOT NULL,
    PRIMARY KEY (id),
    KEY idx_player_epoch (player_id, owner_epoch),
    KEY idx_created_at (created_at_ms)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
`

// OwnerType classifies the active authority domain owning a player.
type OwnerType int8

const (
	OwnerNone     OwnerType = 0
	OwnerHub      OwnerType = 1
	OwnerInstance OwnerType = 2
)

// Phase tracks the transition lifecycle of an ownership epoch.
type Phase int8

const (
	PhaseNone     Phase = 0
	PhasePending  Phase = 1
	PhaseAdmitted Phase = 2
)

// OwnerTarget uniquely identifies a target runtime holder (exact 5-tuple).
type OwnerTarget struct {
	PodName       string
	InstanceUID   string
	InstanceEpoch uint32
	AssignmentID  string
	ReleaseTrack  string
}

// Complete reports whether required identity fields are present.
func (t OwnerTarget) Complete() bool {
	return t.PodName != "" && t.InstanceUID != "" && t.InstanceEpoch > 0 && t.AssignmentID != ""
}

// Equal checks exact 5-tuple equality.
func (t OwnerTarget) Equal(other OwnerTarget) bool {
	return t == other
}

// SameInstance reports whether two targets refer to the same physical process generation.
func (t OwnerTarget) SameInstance(other OwnerTarget) bool {
	return t.PodName == other.PodName &&
		t.InstanceUID == other.InstanceUID &&
		t.InstanceEpoch == other.InstanceEpoch
}

// OwnerRecord is the authoritative ownership row for a single player.
type OwnerRecord struct {
	PlayerID         uint64
	OwnerEpoch       uint64
	OwnerType        OwnerType
	Phase            Phase
	Target           OwnerTarget
	OperationID      string
	AdmitNotBeforeMs int64
	SourceRevision   uint64
	UpdatedAtMs      int64
}

// BeginRequest requests a CAS ownership transition to a new target holder.
type BeginRequest struct {
	PlayerID       uint64
	ExpectEpoch    uint64
	TargetType     OwnerType
	Target         OwnerTarget
	OperationID    string
	SourceRevision uint64
	Detail         string
}

// AdmitRequest requests promotion of a PENDING epoch to ADMITTED.
type AdmitRequest struct {
	PlayerID    uint64
	OwnerEpoch  uint64
	OperationID string
	Target      OwnerTarget
}

// RenewLeaseRequest extends a runtime instance's lease deadline.
type RenewLeaseRequest struct {
	PodName       string
	InstanceUID   string
	InstanceEpoch uint32
	LeaseSeconds  int64
}

// ReleaseRequest releases ownership iff (OwnerEpoch, OperationID) matches the live row.
type ReleaseRequest struct {
	PlayerID    uint64
	OwnerEpoch  uint64
	OperationID string
	Detail      string
}

var (
	ErrEpochConflict       = errors.New("ownerauthority: expect_epoch conflict")
	ErrBarrierNotOpen      = errors.New("ownerauthority: admit barrier not open")
	ErrIdentityMismatch    = errors.New("ownerauthority: target or operation identity mismatch")
	ErrLeaseRegressed      = errors.New("ownerauthority: instance lease regressed")
	ErrInvalidOperation    = errors.New("ownerauthority: invalid operation or target")
	ErrSourceRevisionStale = errors.New("ownerauthority: source revision stale")
)

// EpochConflictError carries the authoritative current record on CAS conflict.
type EpochConflictError struct {
	Current OwnerRecord
}

func (e *EpochConflictError) Error() string {
	return fmt.Sprintf("%s: current_epoch=%d", ErrEpochConflict.Error(), e.Current.OwnerEpoch)
}

func (e *EpochConflictError) Unwrap() error { return ErrEpochConflict }

// BarrierNotOpenError carries the exact barrier timestamp and retry hint.
type BarrierNotOpenError struct {
	AdmitNotBeforeMs int64
	RetryAfterMs     uint32
}

func (e *BarrierNotOpenError) Error() string {
	return fmt.Sprintf("%s: admit_not_before_ms=%d retry_after_ms=%d",
		ErrBarrierNotOpen.Error(), e.AdmitNotBeforeMs, e.RetryAfterMs)
}

func (e *BarrierNotOpenError) Unwrap() error { return ErrBarrierNotOpen }

// Store executes linearizable SQL transactions for player ownership authority.
type Store struct {
	db         *sql.DB
	skewMargin time.Duration
	now        func() time.Time
	newUUID    func() string
}

// NewStore builds a Store over an existing strong-read SQL pool.
func NewStore(db *sql.DB, skewMargin time.Duration) *Store {
	if skewMargin <= 0 {
		skewMargin = DefaultSkewMargin
	}
	return &Store{
		db:         db,
		skewMargin: skewMargin,
		now:        time.Now,
		newUUID:    uuid.NewString,
	}
}

// OpenStrongDB opens a strong-read MySQL/TiDB pool for owner authority and verifies connectivity.
func OpenStrongDB(ctx context.Context, dsn, expectedDatabase, backendRaw string, maxOpen int) (*sql.DB, error) {
	backend, err := authoritysql.ParseBackend(backendRaw)
	if err != nil {
		return nil, fmt.Errorf("ownerauthority invalid backend: %w", err)
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, errors.New("ownerauthority invalid mysql dsn")
	}
	if expectedDatabase != "" && cfg.DBName != expectedDatabase {
		return nil, fmt.Errorf("ownerauthority dsn database mismatch: want %s", expectedDatabase)
	}
	if cfg.Params == nil {
		cfg.Params = make(map[string]string)
	}
	cfg.Params["autocommit"] = "1"
	cfg.Params["transaction_isolation"] = "'READ-COMMITTED'"
	cfg.Params["sql_mode"] = "'STRICT_TRANS_TABLES'"
	if backend == authoritysql.BackendTiDB {
		cfg.Params["tidb_snapshot"] = "''"
		cfg.Params["tidb_read_consistency"] = "'strict'"
		cfg.Params["tidb_txn_mode"] = "'pessimistic'"
		cfg.Params["tidb_read_staleness"] = "0"
		cfg.Params["tidb_low_resolution_tso"] = "OFF"
		cfg.Params["tidb_replica_read"] = "'leader'"
		cfg.Params["tidb_constraint_check_in_place_pessimistic"] = "ON"
	}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, errors.New("ownerauthority failed to create mysql connector")
	}
	db := sql.OpenDB(connector)
	if maxOpen <= 0 {
		maxOpen = 16
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxOpen / 2)
	db.SetConnMaxIdleTime(3 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ownerauthority db ping failed: %w", err)
	}
	if err := authoritysql.AssertCurrentSession(ctx, db, backend); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ownerauthority session check failed: %w", err)
	}
	return db, nil
}

// Query reads the authoritative owner_record row for playerID.
func (s *Store) Query(ctx context.Context, playerID uint64) (OwnerRecord, bool, error) {
	if playerID == 0 {
		return OwnerRecord{}, false, ErrInvalidOperation
	}
	rec, found, err := scanOwnerRow(s.db.QueryRowContext(ctx,
		`SELECT player_id, owner_epoch, owner_type, phase, pod_name, instance_uid, instance_epoch,
		        assignment_id, release_track, operation_id, admit_not_before_ms, source_revision, updated_at_ms
		 FROM owner_record WHERE player_id = ?`, playerID))
	if err != nil {
		return OwnerRecord{}, false, fmt.Errorf("ownerauthority query: %w", err)
	}
	return rec, found, nil
}

// BeginTransition atomically checks expect_epoch, source_revision monotonicity,
// and outgoing instance lease barrier in a single transaction (lock order:
// owner_record FOR UPDATE -> owner_instance_lease FOR UPDATE).
func (s *Store) BeginTransition(ctx context.Context, req BeginRequest) (OwnerRecord, error) {
	if req.PlayerID == 0 || (req.TargetType != OwnerHub && req.TargetType != OwnerInstance) || !req.Target.Complete() {
		return OwnerRecord{}, ErrInvalidOperation
	}
	opID := strings.TrimSpace(req.OperationID)
	if opID == "" {
		opID = s.newUUID()
	} else {
		u, err := uuid.Parse(opID)
		if err != nil || u.Version() != 4 {
			return OwnerRecord{}, ErrInvalidOperation
		}
		opID = u.String()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OwnerRecord{}, fmt.Errorf("ownerauthority begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	cur, found, err := scanOwnerRow(tx.QueryRowContext(ctx,
		`SELECT player_id, owner_epoch, owner_type, phase, pod_name, instance_uid, instance_epoch,
		        assignment_id, release_track, operation_id, admit_not_before_ms, source_revision, updated_at_ms
		 FROM owner_record WHERE player_id = ? FOR UPDATE`, req.PlayerID))
	if err != nil {
		return OwnerRecord{}, fmt.Errorf("ownerauthority lock record: %w", err)
	}

	if found {
		// Exact operation_id replay check.
		if cur.OperationID == opID {
			if cur.OwnerType == req.TargetType && cur.Target.Equal(req.Target) {
				return cur, nil
			}
			return OwnerRecord{}, ErrIdentityMismatch
		}
		// Idempotent no-op when already assigned/admitted to the exact same target.
		if cur.Phase != PhaseNone && cur.OwnerType == req.TargetType && cur.Target.Equal(req.Target) &&
			(req.SourceRevision == 0 || req.SourceRevision == cur.SourceRevision) {
			return cur, nil
		}
		if cur.OwnerEpoch != req.ExpectEpoch {
			return OwnerRecord{}, &EpochConflictError{Current: cur}
		}
		if cur.SourceRevision > 0 {
			if req.SourceRevision == 0 || req.SourceRevision < cur.SourceRevision {
				return OwnerRecord{}, ErrSourceRevisionStale
			}
			if req.SourceRevision == cur.SourceRevision && !cur.Target.Equal(req.Target) {
				return OwnerRecord{}, ErrSourceRevisionStale
			}
		}
	} else if req.ExpectEpoch != 0 {
		return OwnerRecord{}, &EpochConflictError{}
	}

	nowMs := s.now().UnixMilli()
	admitNotBeforeMs := nowMs
	// Cross-instance handoff: lock outgoing holder lease in fixed second order.
	if found && cur.Phase != PhaseNone && cur.Target.PodName != "" && !cur.Target.SameInstance(req.Target) {
		var (
			leaseUID      string
			leaseEpoch    uint32
			leaseDeadline int64
		)
		lerr := tx.QueryRowContext(ctx,
			`SELECT instance_uid, instance_epoch, lease_deadline_ms FROM owner_instance_lease WHERE pod_name = ? FOR UPDATE`,
			cur.Target.PodName).Scan(&leaseUID, &leaseEpoch, &leaseDeadline)
		if lerr != nil && !errors.Is(lerr, sql.ErrNoRows) {
			return OwnerRecord{}, fmt.Errorf("ownerauthority lock lease: %w", lerr)
		}
		if lerr == nil && leaseUID == cur.Target.InstanceUID && leaseEpoch == cur.Target.InstanceEpoch {
			barrier := leaseDeadline + s.skewMargin.Milliseconds()
			if barrier > admitNotBeforeMs {
				admitNotBeforeMs = barrier
			}
		}
	}

	nextSourceRev := cur.SourceRevision
	if req.SourceRevision > nextSourceRev {
		nextSourceRev = req.SourceRevision
	}
	next := OwnerRecord{
		PlayerID:         req.PlayerID,
		OwnerEpoch:       cur.OwnerEpoch + 1,
		OwnerType:        req.TargetType,
		Phase:            PhasePending,
		Target:           req.Target,
		OperationID:      opID,
		AdmitNotBeforeMs: admitNotBeforeMs,
		SourceRevision:   nextSourceRev,
		UpdatedAtMs:      nowMs,
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO owner_record (
		    player_id, owner_epoch, owner_type, phase, pod_name, instance_uid, instance_epoch,
		    assignment_id, release_track, operation_id, admit_not_before_ms, source_revision, updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
		    owner_epoch = VALUES(owner_epoch),
		    owner_type = VALUES(owner_type),
		    phase = VALUES(phase),
		    pod_name = VALUES(pod_name),
		    instance_uid = VALUES(instance_uid),
		    instance_epoch = VALUES(instance_epoch),
		    assignment_id = VALUES(assignment_id),
		    release_track = VALUES(release_track),
		    operation_id = VALUES(operation_id),
		    admit_not_before_ms = VALUES(admit_not_before_ms),
		    source_revision = VALUES(source_revision),
		    updated_at_ms = VALUES(updated_at_ms)`,
		next.PlayerID, next.OwnerEpoch, int8(next.OwnerType), int8(next.Phase),
		next.Target.PodName, next.Target.InstanceUID, next.Target.InstanceEpoch,
		next.Target.AssignmentID, next.Target.ReleaseTrack, next.OperationID,
		next.AdmitNotBeforeMs, next.SourceRevision, next.UpdatedAtMs); err != nil {
		return OwnerRecord{}, fmt.Errorf("ownerauthority upsert record: %w", err)
	}

	if err := insertTransitionLog(ctx, tx, next.PlayerID, next.OwnerEpoch, next.OperationID, "begin", req.Detail, nowMs); err != nil {
		return OwnerRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return OwnerRecord{}, fmt.Errorf("ownerauthority commit begin: %w", err)
	}
	return next, nil
}

// Admit promotes a matching PENDING record to ADMITTED once now >= admit_not_before_ms.
func (s *Store) Admit(ctx context.Context, req AdmitRequest) (OwnerRecord, error) {
	if req.PlayerID == 0 || req.OwnerEpoch == 0 || !req.Target.Complete() {
		return OwnerRecord{}, ErrInvalidOperation
	}
	u, err := uuid.Parse(strings.TrimSpace(req.OperationID))
	if err != nil || u.Version() != 4 {
		return OwnerRecord{}, ErrInvalidOperation
	}
	opID := u.String()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OwnerRecord{}, fmt.Errorf("ownerauthority begin admit tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	cur, found, err := scanOwnerRow(tx.QueryRowContext(ctx,
		`SELECT player_id, owner_epoch, owner_type, phase, pod_name, instance_uid, instance_epoch,
		        assignment_id, release_track, operation_id, admit_not_before_ms, source_revision, updated_at_ms
		 FROM owner_record WHERE player_id = ? FOR UPDATE`, req.PlayerID))
	if err != nil {
		return OwnerRecord{}, fmt.Errorf("ownerauthority lock record for admit: %w", err)
	}
	if !found || cur.OwnerEpoch != req.OwnerEpoch || cur.OperationID != opID || !cur.Target.Equal(req.Target) {
		return OwnerRecord{}, ErrIdentityMismatch
	}
	if cur.Phase == PhaseAdmitted {
		return cur, nil
	}
	if cur.Phase != PhasePending {
		return OwnerRecord{}, ErrIdentityMismatch
	}
	nowMs := s.now().UnixMilli()
	if nowMs < cur.AdmitNotBeforeMs {
		retry := clampRetryMs(cur.AdmitNotBeforeMs - nowMs)
		return OwnerRecord{}, &BarrierNotOpenError{AdmitNotBeforeMs: cur.AdmitNotBeforeMs, RetryAfterMs: retry}
	}

	cur.Phase = PhaseAdmitted
	cur.UpdatedAtMs = nowMs
	if _, err := tx.ExecContext(ctx,
		`UPDATE owner_record SET phase = ?, updated_at_ms = ? WHERE player_id = ?`,
		int8(cur.Phase), cur.UpdatedAtMs, cur.PlayerID); err != nil {
		return OwnerRecord{}, fmt.Errorf("ownerauthority update admit phase: %w", err)
	}
	if err := insertTransitionLog(ctx, tx, cur.PlayerID, cur.OwnerEpoch, cur.OperationID, "admit", "", nowMs); err != nil {
		return OwnerRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return OwnerRecord{}, fmt.Errorf("ownerauthority commit admit: %w", err)
	}
	return cur, nil
}

// RenewInstanceLease extends a runtime node's lease deadline, clamped at MaxLeaseSeconds.
func (s *Store) RenewInstanceLease(ctx context.Context, req RenewLeaseRequest) (int64, error) {
	if req.PodName == "" || req.InstanceUID == "" || req.InstanceEpoch == 0 || req.LeaseSeconds <= 0 {
		return 0, ErrInvalidOperation
	}
	leaseSec := req.LeaseSeconds
	if leaseSec > MaxLeaseSeconds {
		leaseSec = MaxLeaseSeconds
	}
	nowMs := s.now().UnixMilli()
	newDeadline := nowMs + leaseSec*1000

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("ownerauthority begin lease tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		curUID      string
		curEpoch    uint32
		curDeadline int64
	)
	qerr := tx.QueryRowContext(ctx,
		`SELECT instance_uid, instance_epoch, lease_deadline_ms FROM owner_instance_lease WHERE pod_name = ? FOR UPDATE`,
		req.PodName).Scan(&curUID, &curEpoch, &curDeadline)
	if qerr != nil && !errors.Is(qerr, sql.ErrNoRows) {
		return 0, fmt.Errorf("ownerauthority lock lease for renew: %w", qerr)
	}
	if qerr == nil {
		if curUID == req.InstanceUID {
			if req.InstanceEpoch < curEpoch || (req.InstanceEpoch == curEpoch && newDeadline < curDeadline) {
				return 0, ErrLeaseRegressed
			}
		} else if req.InstanceEpoch <= curEpoch {
			return 0, ErrLeaseRegressed
		}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO owner_instance_lease (pod_name, instance_uid, instance_epoch, lease_deadline_ms, updated_at_ms)
		 VALUES (?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE
		     instance_uid = VALUES(instance_uid),
		     instance_epoch = VALUES(instance_epoch),
		     lease_deadline_ms = VALUES(lease_deadline_ms),
		     updated_at_ms = VALUES(updated_at_ms)`,
		req.PodName, req.InstanceUID, req.InstanceEpoch, newDeadline, nowMs); err != nil {
		return 0, fmt.Errorf("ownerauthority upsert lease: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("ownerauthority commit lease: %w", err)
	}
	return newDeadline, nil
}

// Release clears active ownership iff (OwnerEpoch, OperationID) matches.
// Late or mismatched releases return (current, false, nil) without disturbing a newer owner.
func (s *Store) Release(ctx context.Context, req ReleaseRequest) (OwnerRecord, bool, error) {
	if req.PlayerID == 0 || req.OwnerEpoch == 0 || strings.TrimSpace(req.OperationID) == "" {
		return OwnerRecord{}, false, ErrInvalidOperation
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OwnerRecord{}, false, fmt.Errorf("ownerauthority begin release tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	cur, found, err := scanOwnerRow(tx.QueryRowContext(ctx,
		`SELECT player_id, owner_epoch, owner_type, phase, pod_name, instance_uid, instance_epoch,
		        assignment_id, release_track, operation_id, admit_not_before_ms, source_revision, updated_at_ms
		 FROM owner_record WHERE player_id = ? FOR UPDATE`, req.PlayerID))
	if err != nil {
		return OwnerRecord{}, false, fmt.Errorf("ownerauthority lock record for release: %w", err)
	}
	if !found || cur.Phase == PhaseNone || cur.OwnerEpoch != req.OwnerEpoch || cur.OperationID != req.OperationID {
		return cur, false, nil
	}

	nowMs := s.now().UnixMilli()
	cur.OwnerType = OwnerNone
	cur.Phase = PhaseNone
	cur.Target = OwnerTarget{}
	cur.AdmitNotBeforeMs = 0
	cur.UpdatedAtMs = nowMs

	if _, err := tx.ExecContext(ctx,
		`UPDATE owner_record
		 SET owner_type = ?, phase = ?, pod_name = '', instance_uid = '', instance_epoch = 0,
		     assignment_id = '', release_track = '', admit_not_before_ms = 0, updated_at_ms = ?
		 WHERE player_id = ?`,
		int8(cur.OwnerType), int8(cur.Phase), cur.UpdatedAtMs, cur.PlayerID); err != nil {
		return OwnerRecord{}, false, fmt.Errorf("ownerauthority update release: %w", err)
	}
	if err := insertTransitionLog(ctx, tx, cur.PlayerID, cur.OwnerEpoch, cur.OperationID, "release", req.Detail, nowMs); err != nil {
		return OwnerRecord{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return OwnerRecord{}, false, fmt.Errorf("ownerauthority commit release: %w", err)
	}
	return cur, true, nil
}

func scanOwnerRow(row *sql.Row) (OwnerRecord, bool, error) {
	var (
		rec       OwnerRecord
		ownerType int8
		phase     int8
	)
	err := row.Scan(
		&rec.PlayerID, &rec.OwnerEpoch, &ownerType, &phase,
		&rec.Target.PodName, &rec.Target.InstanceUID, &rec.Target.InstanceEpoch,
		&rec.Target.AssignmentID, &rec.Target.ReleaseTrack,
		&rec.OperationID, &rec.AdmitNotBeforeMs, &rec.SourceRevision, &rec.UpdatedAtMs,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return OwnerRecord{}, false, nil
	}
	if err != nil {
		return OwnerRecord{}, false, err
	}
	rec.OwnerType = OwnerType(ownerType)
	rec.Phase = Phase(phase)
	return rec, true, nil
}

func insertTransitionLog(ctx context.Context, tx *sql.Tx, playerID, epoch uint64, opID, action, detail string, createdAtMs int64) error {
	if len(detail) > maxDetailLen {
		detail = detail[:maxDetailLen]
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO owner_transition_log (player_id, owner_epoch, operation_id, action, detail, created_at_ms)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		playerID, epoch, opID, action, detail, createdAtMs); err != nil {
		return fmt.Errorf("ownerauthority insert transition log: %w", err)
	}
	return nil
}

func clampRetryMs(deltaMs int64) uint32 {
	if deltaMs <= 0 {
		return 1
	}
	if deltaMs > 60_000 {
		return 60_000
	}
	return uint32(deltaMs)
}
