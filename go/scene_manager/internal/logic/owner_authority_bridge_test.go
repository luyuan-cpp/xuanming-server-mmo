package logic

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

	"scene_manager/internal/config"
	"scene_manager/internal/svc"
	"shared/ownerauthority"
)

type bridgeLeaseRow struct {
	uid      string
	epoch    uint32
	deadline int64
}

type bridgeMemState struct {
	mu      sync.Mutex
	records map[uint64]ownerauthority.OwnerRecord
	leases  map[string]bridgeLeaseRow
}

var (
	bridgeDriverOnce sync.Once
	bridgeStatesMu   sync.Mutex
	bridgeStates     = map[string]*bridgeMemState{}
)

type bridgeMemDriver struct{}

func (bridgeMemDriver) Open(dsn string) (driver.Conn, error) {
	bridgeStatesMu.Lock()
	st := bridgeStates[dsn]
	bridgeStatesMu.Unlock()
	if st == nil {
		return nil, errors.New("unknown dsn")
	}
	return &bridgeMemConn{st: st}, nil
}

type bridgeMemConn struct {
	st     *bridgeMemState
	locked bool
}

func (c *bridgeMemConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unsupported") }
func (c *bridgeMemConn) Close() error                        { return nil }
func (c *bridgeMemConn) Begin() (driver.Tx, error)           { return c.BeginTx(context.Background(), driver.TxOptions{}) }
func (c *bridgeMemConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.st.mu.Lock()
	c.locked = true
	return &bridgeMemTx{conn: c}, nil
}

type bridgeMemTx struct{ conn *bridgeMemConn }

func (t *bridgeMemTx) Commit() error {
	if t.conn.locked {
		t.conn.locked = false
		t.conn.st.mu.Unlock()
	}
	return nil
}

func (t *bridgeMemTx) Rollback() error {
	if t.conn.locked {
		t.conn.locked = false
		t.conn.st.mu.Unlock()
	}
	return nil
}

func bridgeU64(v driver.Value) uint64 {
	switch x := v.(type) {
	case int64:
		return uint64(x)
	case uint64:
		return x
	default:
		panic("unexpected uint64 arg")
	}
}

func (c *bridgeMemConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	q := strings.TrimSpace(query)
	switch {
	case strings.HasPrefix(q, "INSERT INTO owner_record"):
		rec := ownerauthority.OwnerRecord{
			PlayerID:   bridgeU64(args[0].Value),
			OwnerEpoch: bridgeU64(args[1].Value),
			OwnerType:  ownerauthority.OwnerType(args[2].Value.(int64)),
			Phase:      ownerauthority.Phase(args[3].Value.(int64)),
			Target: ownerauthority.OwnerTarget{
				PodName:       args[4].Value.(string),
				InstanceUID:   args[5].Value.(string),
				InstanceEpoch: uint32(args[6].Value.(int64)),
				AssignmentID:  args[7].Value.(string),
				ReleaseTrack:  args[8].Value.(string),
			},
			OperationID:      args[9].Value.(string),
			AdmitNotBeforeMs: args[10].Value.(int64),
			SourceRevision:   bridgeU64(args[11].Value),
			UpdatedAtMs:      args[12].Value.(int64),
		}
		c.st.records[rec.PlayerID] = rec
		return driver.RowsAffected(1), nil
	case strings.HasPrefix(q, "UPDATE owner_record SET phase = ?"):
		pid := bridgeU64(args[2].Value)
		rec := c.st.records[pid]
		rec.Phase = ownerauthority.Phase(args[0].Value.(int64))
		rec.UpdatedAtMs = args[1].Value.(int64)
		c.st.records[pid] = rec
		return driver.RowsAffected(1), nil
	case strings.HasPrefix(q, "UPDATE owner_record"):
		pid := bridgeU64(args[4].Value)
		rec := c.st.records[pid]
		rec.OwnerType = ownerauthority.OwnerType(args[0].Value.(int64))
		rec.Phase = ownerauthority.Phase(args[1].Value.(int64))
		rec.Target = ownerauthority.OwnerTarget{}
		rec.AdmitNotBeforeMs = args[2].Value.(int64)
		rec.UpdatedAtMs = args[3].Value.(int64)
		c.st.records[pid] = rec
		return driver.RowsAffected(1), nil
	case strings.HasPrefix(q, "INSERT INTO owner_instance_lease"):
		c.st.leases[args[0].Value.(string)] = bridgeLeaseRow{
			uid:      args[1].Value.(string),
			epoch:    uint32(args[2].Value.(int64)),
			deadline: args[3].Value.(int64),
		}
		return driver.RowsAffected(1), nil
	case strings.HasPrefix(q, "INSERT INTO owner_transition_log"):
		return driver.RowsAffected(1), nil
	default:
		return nil, errors.New("unexpected exec: " + q)
	}
}

func (c *bridgeMemConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if !c.locked {
		c.st.mu.Lock()
		defer c.st.mu.Unlock()
	}
	q := strings.TrimSpace(query)
	switch {
	case strings.Contains(q, "FROM owner_record"):
		rec, ok := c.st.records[bridgeU64(args[0].Value)]
		if !ok {
			return &bridgeMemRows{cols: make([]string, 13)}, nil
		}
		return &bridgeMemRows{
			cols: make([]string, 13),
			data: [][]driver.Value{{
				int64(rec.PlayerID), int64(rec.OwnerEpoch), int64(rec.OwnerType), int64(rec.Phase),
				rec.Target.PodName, rec.Target.InstanceUID, int64(rec.Target.InstanceEpoch),
				rec.Target.AssignmentID, rec.Target.ReleaseTrack, rec.OperationID,
				rec.AdmitNotBeforeMs, int64(rec.SourceRevision), rec.UpdatedAtMs,
			}},
		}, nil
	case strings.Contains(q, "FROM owner_instance_lease"):
		row, ok := c.st.leases[args[0].Value.(string)]
		if !ok {
			return &bridgeMemRows{cols: make([]string, 3)}, nil
		}
		return &bridgeMemRows{
			cols: make([]string, 3),
			data: [][]driver.Value{{row.uid, int64(row.epoch), row.deadline}},
		}, nil
	default:
		return nil, errors.New("unexpected query: " + q)
	}
}

type bridgeMemRows struct {
	cols []string
	data [][]driver.Value
	idx  int
}

func (r *bridgeMemRows) Columns() []string { return r.cols }
func (r *bridgeMemRows) Close() error      { return nil }
func (r *bridgeMemRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.idx])
	r.idx++
	return nil
}

func newBridgeTestSvc(t *testing.T, mode string) (*svc.ServiceContext, *bridgeMemState) {
	t.Helper()
	bridgeDriverOnce.Do(func() { sql.Register("mem_sm_owner_bridge", bridgeMemDriver{}) })
	dsn := t.Name() + "_" + mode
	st := &bridgeMemState{
		records: map[uint64]ownerauthority.OwnerRecord{},
		leases:  map[string]bridgeLeaseRow{},
	}
	bridgeStatesMu.Lock()
	bridgeStates[dsn] = st
	bridgeStatesMu.Unlock()
	db, err := sql.Open("mem_sm_owner_bridge", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := ownerauthority.NewStore(db, 2*time.Second)
	return &svc.ServiceContext{
		Config: config.Config{
			OwnerAuthority: config.OwnerAuthorityConfig{Mode: mode, DSN: dsn},
		},
		OwnerAuthority: store,
	}, st
}

func TestOwnerAuthorityBridge_ShadowAndEnforce(t *testing.T) {
	ctx := context.Background()

	// 1. Enforce mode: enter commits -> normal scene switch with same OwnerRevision succeeds ->
	// stale revision rejected -> active lease barrier blocks -> rollback & leave release cleanly.
	enforceSvc, _ := newBridgeTestSvc(t, "enforce")
	if _, err := syncOwnerAuthorityEnter(ctx, enforceSvc, nil, 7001, 100, "scene-a", 1, 1, 10, true); err != nil {
		t.Fatalf("initial enforce enter failed: %v", err)
	}
	// Normal map/scene switch keeps the same account-role OwnerRevision (10) and must succeed.
	recSwitched, err := syncOwnerAuthorityEnter(ctx, enforceSvc, nil, 7001, 105, "scene-a", 1, 2, 10, true)
	if err != nil || recSwitched.Target.AssignmentID != "z1:s105" {
		t.Fatalf("same-revision scene switch failed: rec=%+v err=%v", recSwitched, err)
	}
	// Stale ownerRevision (<10) rejected fail-closed in enforce mode.
	if _, err := syncOwnerAuthorityEnter(ctx, enforceSvc, nil, 7001, 101, "scene-b", 1, 3, 9, true); !errors.Is(err, ownerauthority.ErrSourceRevisionStale) {
		t.Fatalf("stale revision err = %v, want ErrSourceRevisionStale", err)
	}
	// Active lease on scene-a creates admit_not_before barrier that blocks immediate cross-node enter.
	if _, err := enforceSvc.OwnerAuthority.RenewInstanceLease(ctx, ownerauthority.RenewLeaseRequest{
		PodName: "scene-a", InstanceUID: "scene-a", InstanceEpoch: 1, LeaseSeconds: 10,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := syncOwnerAuthorityEnter(ctx, enforceSvc, nil, 7001, 101, "scene-b", 1, 3, 10, true); !errors.Is(err, ownerauthority.ErrBarrierNotOpen) {
		t.Fatalf("barrier err = %v, want ErrBarrierNotOpen", err)
	}
	// Leave on mismatched target is a safe no-op; leave on matching target clears ownership.
	if err := syncOwnerAuthorityLeave(ctx, enforceSvc, nil, 7001, 100, "scene-a", 1); err != nil {
		t.Fatalf("mismatched leave failed: %v", err)
	}
	if err := syncOwnerAuthorityLeave(ctx, enforceSvc, nil, 7001, 101, "scene-b", 1); err != nil {
		t.Fatalf("matching leave failed: %v", err)
	}
	rec, found, err := enforceSvc.OwnerAuthority.Query(ctx, 7001)
	if err != nil || !found || rec.Phase != ownerauthority.PhaseNone {
		t.Fatalf("after matching leave rec=%+v found=%v err=%v", rec, found, err)
	}

	// 1b. Route-failure rollback releases the newly admitted SQL record cleanly.
	recRoll, err := syncOwnerAuthorityEnter(ctx, enforceSvc, nil, 7003, 300, "scene-c", 1, 1, 1, true)
	if err != nil {
		t.Fatalf("enter before rollback failed: %v", err)
	}
	syncOwnerAuthorityRollback(ctx, enforceSvc, nil, recRoll)
	afterRoll, found, err := enforceSvc.OwnerAuthority.Query(ctx, 7003)
	if err != nil || !found || afterRoll.Phase != ownerauthority.PhaseNone {
		t.Fatalf("after rollback rec=%+v found=%v err=%v", afterRoll, found, err)
	}

	// 2. Shadow mode: never blocks even when source_revision is stale.
	shadowSvc, _ := newBridgeTestSvc(t, "shadow")
	if _, err := syncOwnerAuthorityEnter(ctx, shadowSvc, nil, 7002, 200, "scene-a", 1, 1, 20, false); err != nil {
		t.Fatalf("shadow initial enter failed: %v", err)
	}
	if _, err := syncOwnerAuthorityEnter(ctx, shadowSvc, nil, 7002, 201, "scene-b", 1, 2, 10, false); err != nil {
		t.Fatalf("shadow stale revision must not block, got %v", err)
	}
}
