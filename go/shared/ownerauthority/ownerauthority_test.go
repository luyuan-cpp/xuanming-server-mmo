package ownerauthority

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type memLeaseRow struct {
	uid      string
	epoch    uint32
	deadline int64
	updated  int64
}

type memLogRow struct {
	playerID    uint64
	epoch       uint64
	opID        string
	action      string
	detail      string
	createdAtMs int64
}

type memOwnerState struct {
	mu      sync.Mutex
	records map[uint64]OwnerRecord
	leases  map[string]memLeaseRow
	logs    []memLogRow
}

var (
	memOwnerDriverOnce sync.Once
	memOwnerStatesMu   sync.Mutex
	memOwnerStates     = map[string]*memOwnerState{}
)

type memOwnerDriver struct{}

func (memOwnerDriver) Open(dsn string) (driver.Conn, error) {
	memOwnerStatesMu.Lock()
	st := memOwnerStates[dsn]
	memOwnerStatesMu.Unlock()
	if st == nil {
		return nil, errors.New("unknown dsn")
	}
	return &memOwnerConn{st: st}, nil
}

type memOwnerConn struct {
	st     *memOwnerState
	locked bool
}

func (c *memOwnerConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unsupported") }
func (c *memOwnerConn) Close() error                        { return nil }
func (c *memOwnerConn) Begin() (driver.Tx, error)           { return c.BeginTx(context.Background(), driver.TxOptions{}) }
func (c *memOwnerConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.st.mu.Lock()
	c.locked = true
	return &memOwnerTx{conn: c}, nil
}

type memOwnerTx struct{ conn *memOwnerConn }

func (t *memOwnerTx) Commit() error {
	if t.conn.locked {
		t.conn.locked = false
		t.conn.st.mu.Unlock()
	}
	return nil
}

func (t *memOwnerTx) Rollback() error {
	if t.conn.locked {
		t.conn.locked = false
		t.conn.st.mu.Unlock()
	}
	return nil
}

func argU64(v driver.Value) uint64 {
	switch x := v.(type) {
	case int64:
		return uint64(x)
	case uint64:
		return x
	default:
		panic("unexpected uint64 arg")
	}
}

func (c *memOwnerConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	q := strings.TrimSpace(query)
	switch {
	case strings.HasPrefix(q, "INSERT INTO owner_record"):
		rec := OwnerRecord{
			PlayerID:   argU64(args[0].Value),
			OwnerEpoch: argU64(args[1].Value),
			OwnerType:  OwnerType(args[2].Value.(int64)),
			Phase:      Phase(args[3].Value.(int64)),
			Target: OwnerTarget{
				PodName:       args[4].Value.(string),
				InstanceUID:   args[5].Value.(string),
				InstanceEpoch: uint32(args[6].Value.(int64)),
				AssignmentID:  args[7].Value.(string),
				ReleaseTrack:  args[8].Value.(string),
			},
			OperationID:      args[9].Value.(string),
			AdmitNotBeforeMs: args[10].Value.(int64),
			SourceRevision:   argU64(args[11].Value),
			UpdatedAtMs:      args[12].Value.(int64),
		}
		c.st.records[rec.PlayerID] = rec
		return driver.RowsAffected(1), nil
	case strings.HasPrefix(q, "UPDATE owner_record SET phase = ?"):
		pid := argU64(args[2].Value)
		rec := c.st.records[pid]
		rec.Phase = Phase(args[0].Value.(int64))
		rec.UpdatedAtMs = args[1].Value.(int64)
		c.st.records[pid] = rec
		return driver.RowsAffected(1), nil
	case strings.HasPrefix(q, "UPDATE owner_record"):
		pid := argU64(args[3].Value)
		rec := c.st.records[pid]
		rec.OwnerType = OwnerType(args[0].Value.(int64))
		rec.Phase = Phase(args[1].Value.(int64))
		rec.Target = OwnerTarget{}
		rec.AdmitNotBeforeMs = 0
		rec.UpdatedAtMs = args[2].Value.(int64)
		c.st.records[pid] = rec
		return driver.RowsAffected(1), nil
	case strings.HasPrefix(q, "INSERT INTO owner_instance_lease"):
		pod := args[0].Value.(string)
		c.st.leases[pod] = memLeaseRow{
			uid:      args[1].Value.(string),
			epoch:    uint32(args[2].Value.(int64)),
			deadline: args[3].Value.(int64),
			updated:  args[4].Value.(int64),
		}
		return driver.RowsAffected(1), nil
	case strings.HasPrefix(q, "INSERT INTO owner_transition_log"):
		c.st.logs = append(c.st.logs, memLogRow{
			playerID:    argU64(args[0].Value),
			epoch:       argU64(args[1].Value),
			opID:        args[2].Value.(string),
			action:      args[3].Value.(string),
			detail:      args[4].Value.(string),
			createdAtMs: args[5].Value.(int64),
		})
		return driver.RowsAffected(1), nil
	default:
		return nil, errors.New("unexpected exec: " + q)
	}
}

func (c *memOwnerConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if !c.locked {
		c.st.mu.Lock()
		defer c.st.mu.Unlock()
	}
	q := strings.TrimSpace(query)
	switch {
	case strings.Contains(q, "FROM owner_record"):
		pid := argU64(args[0].Value)
		rec, ok := c.st.records[pid]
		if !ok {
			return &memOwnerRows{cols: make([]string, 13)}, nil
		}
		return &memOwnerRows{
			cols: make([]string, 13),
			data: [][]driver.Value{{
				int64(rec.PlayerID), int64(rec.OwnerEpoch), int64(rec.OwnerType), int64(rec.Phase),
				rec.Target.PodName, rec.Target.InstanceUID, int64(rec.Target.InstanceEpoch),
				rec.Target.AssignmentID, rec.Target.ReleaseTrack, rec.OperationID,
				rec.AdmitNotBeforeMs, int64(rec.SourceRevision), rec.UpdatedAtMs,
			}},
		}, nil
	case strings.Contains(q, "FROM owner_instance_lease"):
		pod := args[0].Value.(string)
		row, ok := c.st.leases[pod]
		if !ok {
			return &memOwnerRows{cols: make([]string, 3)}, nil
		}
		return &memOwnerRows{
			cols: make([]string, 3),
			data: [][]driver.Value{{row.uid, int64(row.epoch), row.deadline}},
		}, nil
	default:
		return nil, errors.New("unexpected query: " + q)
	}
}

type memOwnerRows struct {
	cols []string
	data [][]driver.Value
	idx  int
}

func (r *memOwnerRows) Columns() []string { return r.cols }
func (r *memOwnerRows) Close() error      { return nil }
func (r *memOwnerRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.idx])
	r.idx++
	return nil
}

func openMemOwnerStore(t *testing.T) (*Store, *memOwnerState) {
	t.Helper()
	memOwnerDriverOnce.Do(func() { sql.Register("mem_owner_authority", memOwnerDriver{}) })
	dsn := t.Name()
	st := &memOwnerState{
		records: map[uint64]OwnerRecord{},
		leases:  map[string]memLeaseRow{},
	}
	memOwnerStatesMu.Lock()
	memOwnerStates[dsn] = st
	memOwnerStatesMu.Unlock()
	db, err := sql.Open("mem_owner_authority", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewStore(db, 2*time.Second), st
}

func TestStore_TransitionBarrierLeaseFenceAndRelease(t *testing.T) {
	store, st := openMemOwnerStore(t)
	now := time.UnixMilli(1_700_000_000_000)
	store.now = func() time.Time { return now }
	ctx := context.Background()

	hubA := OwnerTarget{PodName: "scene-0", InstanceUID: "uid-a", InstanceEpoch: 1, AssignmentID: "assign-1", ReleaseTrack: "stable"}
	hubB := OwnerTarget{PodName: "scene-1", InstanceUID: "uid-b", InstanceEpoch: 1, AssignmentID: "assign-2", ReleaseTrack: "stable"}

	// 1. Lease clamp at MaxLeaseSeconds (30s) & epoch regression guard.
	deadline, err := store.RenewInstanceLease(ctx, RenewLeaseRequest{
		PodName: hubA.PodName, InstanceUID: hubA.InstanceUID, InstanceEpoch: 1, LeaseSeconds: 120,
	})
	if err != nil {
		t.Fatal(err)
	}
	if deadline != now.UnixMilli()+MaxLeaseSeconds*1000 {
		t.Fatalf("lease deadline = %d, want clamped +30s", deadline)
	}
	// Recreated pod with same/lower epoch must be rejected.
	if _, err := store.RenewInstanceLease(ctx, RenewLeaseRequest{
		PodName: hubA.PodName, InstanceUID: "uid-a-new", InstanceEpoch: 1, LeaseSeconds: 10,
	}); !errors.Is(err, ErrLeaseRegressed) {
		t.Fatalf("recreated pod same epoch err = %v, want ErrLeaseRegressed", err)
	}

	// 2. Initial BeginTransition (epoch 0 -> 1) + detail clamp at 512 bytes.
	rec1, err := store.BeginTransition(ctx, BeginRequest{
		PlayerID: 1001, ExpectEpoch: 0, TargetType: OwnerHub, Target: hubA,
		SourceRevision: 10, Detail: strings.Repeat("x", 800),
	})
	if err != nil || rec1.OwnerEpoch != 1 || rec1.Phase != PhasePending {
		t.Fatalf("begin 1 = %+v, err=%v", rec1, err)
	}
	if len(st.logs) != 1 || len(st.logs[0].detail) != maxDetailLen {
		t.Fatalf("transition log detail len = %d, want %d", len(st.logs[0].detail), maxDetailLen)
	}

	// Idempotent replay with same operation_id returns identical record.
	replay1, err := store.BeginTransition(ctx, BeginRequest{
		PlayerID: 1001, ExpectEpoch: 999, TargetType: OwnerHub, Target: hubA, OperationID: rec1.OperationID,
	})
	if err != nil || replay1.OwnerEpoch != 1 {
		t.Fatalf("idempotent replay failed: %+v, err=%v", replay1, err)
	}

	// Admit succeeds immediately (no outgoing holder).
	admitted1, err := store.Admit(ctx, AdmitRequest{
		PlayerID: 1001, OwnerEpoch: 1, OperationID: rec1.OperationID, Target: hubA,
	})
	if err != nil || admitted1.Phase != PhaseAdmitted {
		t.Fatalf("admit 1 = %+v, err=%v", admitted1, err)
	}
	// Idempotent Admit replay succeeds.
	if _, err := store.Admit(ctx, AdmitRequest{
		PlayerID: 1001, OwnerEpoch: 1, OperationID: rec1.OperationID, Target: hubA,
	}); err != nil {
		t.Fatalf("admit replay failed: %v", err)
	}

	// 3. Stale expect_epoch rejected with authoritative current record.
	_, err = store.BeginTransition(ctx, BeginRequest{
		PlayerID: 1001, ExpectEpoch: 0, TargetType: OwnerInstance, Target: hubB, SourceRevision: 11,
	})
	var conflict *EpochConflictError
	if !errors.As(err, &conflict) || conflict.Current.OwnerEpoch != 1 {
		t.Fatalf("expected EpochConflictError with epoch 1, got %v", err)
	}

	// 4. Stale / legacy-0 source_revision rejected.
	for _, staleRev := range []uint64{0, 9, 10} {
		if _, err := store.BeginTransition(ctx, BeginRequest{
			PlayerID: 1001, ExpectEpoch: 1, TargetType: OwnerInstance, Target: hubB, SourceRevision: staleRev,
		}); !errors.Is(err, ErrSourceRevisionStale) {
			t.Fatalf("source_revision=%d err = %v, want ErrSourceRevisionStale", staleRev, err)
		}
	}

	// 5. Cross-instance handoff computes admit_not_before = old_lease + skew_margin (30s + 2s = 32s).
	rec2, err := store.BeginTransition(ctx, BeginRequest{
		PlayerID: 1001, ExpectEpoch: 1, TargetType: OwnerInstance, Target: hubB, SourceRevision: 11,
	})
	if err != nil || rec2.OwnerEpoch != 2 {
		t.Fatalf("begin 2 = %+v, err=%v", rec2, err)
	}
	wantBarrier := deadline + 2000
	if rec2.AdmitNotBeforeMs != wantBarrier {
		t.Fatalf("admit_not_before = %d, want %d", rec2.AdmitNotBeforeMs, wantBarrier)
	}

	// Admit before barrier returns ErrBarrierNotOpen with retry hint.
	_, err = store.Admit(ctx, AdmitRequest{
		PlayerID: 1001, OwnerEpoch: 2, OperationID: rec2.OperationID, Target: hubB,
	})
	var barrierErr *BarrierNotOpenError
	if !errors.As(err, &barrierErr) || barrierErr.RetryAfterMs != 32000 {
		t.Fatalf("expected BarrierNotOpenError(32000), got %v", err)
	}

	// Advance clock past barrier -> Admit succeeds.
	now = time.UnixMilli(wantBarrier)
	if _, err := store.Admit(ctx, AdmitRequest{
		PlayerID: 1001, OwnerEpoch: 2, OperationID: rec2.OperationID, Target: hubB,
	}); err != nil {
		t.Fatalf("admit after barrier failed: %v", err)
	}

	// 6. Late release from epoch 1 is a safe no-op.
	cur, released, err := store.Release(ctx, ReleaseRequest{
		PlayerID: 1001, OwnerEpoch: 1, OperationID: rec1.OperationID,
	})
	if err != nil || released || cur.OwnerEpoch != 2 || cur.Phase != PhaseAdmitted {
		t.Fatalf("late release disturbed epoch 2: rec=%+v released=%v err=%v", cur, released, err)
	}

	// Matching release clears ownership while preserving monotonic epoch and source_revision.
	afterRel, released, err := store.Release(ctx, ReleaseRequest{
		PlayerID: 1001, OwnerEpoch: 2, OperationID: rec2.OperationID,
	})
	if err != nil || !released || afterRel.Phase != PhaseNone || afterRel.OwnerEpoch != 2 || afterRel.SourceRevision != 11 {
		t.Fatalf("matching release = %+v released=%v err=%v", afterRel, released, err)
	}
}

func TestResolveResume_AllStates(t *testing.T) {
	// 1. Authority error -> WAIT(AuthorityUnavailable)
	d := ResolveResume(ResumeInput{QueryErr: errors.New("db down"), HasRole: true})
	if d.State != EntryWait || d.WaitReason != WaitAuthorityUnavailable || d.RetryAfterMs != DefaultResumeRetryAfterMs {
		t.Fatalf("query error decision = %+v", d)
	}
	// 2. No role selected -> ROLE_REQUIRED
	d = ResolveResume(ResumeInput{HasRole: false})
	if d.State != EntryRoleRequired {
		t.Fatalf("no role decision = %+v", d)
	}
	// 3. No owner assigned -> WAIT(NoOwnerAssigned)
	d = ResolveResume(ResumeInput{HasRole: true, Found: false})
	if d.State != EntryWait || d.WaitReason != WaitNoOwnerAssigned {
		t.Fatalf("no owner decision = %+v", d)
	}
	// 4. Pending behind barrier -> WAIT(BarrierPending, exact remaining ms)
	d = ResolveResume(ResumeInput{
		HasRole: true, Found: true, NowMs: 1000,
		Record: OwnerRecord{OwnerEpoch: 2, OwnerType: OwnerHub, Phase: PhasePending, AdmitNotBeforeMs: 3500},
	})
	if d.State != EntryWait || d.WaitReason != WaitBarrierPending || d.RetryAfterMs != 2500 {
		t.Fatalf("barrier pending decision = %+v", d)
	}
	// 5. Pending past barrier -> WAIT(TransitionInProgress)
	d = ResolveResume(ResumeInput{
		HasRole: true, Found: true, NowMs: 4000,
		Record: OwnerRecord{OwnerEpoch: 2, OwnerType: OwnerHub, Phase: PhasePending, AdmitNotBeforeMs: 3500},
	})
	if d.State != EntryWait || d.WaitReason != WaitTransitionInProgress {
		t.Fatalf("transition in progress decision = %+v", d)
	}
	// 6. Admitted -> TARGET
	d = ResolveResume(ResumeInput{
		HasRole: true, Found: true, NowMs: 4000,
		Record: OwnerRecord{OwnerEpoch: 2, OwnerType: OwnerInstance, Phase: PhaseAdmitted},
	})
	if d.State != EntryTarget || d.Record.OwnerEpoch != 2 {
		t.Fatalf("admitted decision = %+v", d)
	}
}
