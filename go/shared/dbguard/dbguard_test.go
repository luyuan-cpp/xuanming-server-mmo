package dbguard

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"
)

type fakeRows struct {
	cols []string
	data [][]driver.Value
	idx  int
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.idx])
	r.idx++
	return nil
}

type fakeConn struct {
	queryFn func(query string) ([][]driver.Value, []string, error)
	execFn  func(query string, args []driver.NamedValue) (int64, error)
}

func (c *fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not supported") }
func (c *fakeConn) Close() error                        { return nil }
func (c *fakeConn) Begin() (driver.Tx, error)           { return nil, errors.New("not supported") }
func (c *fakeConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	rows, cols, err := c.queryFn(query)
	if err != nil {
		return nil, err
	}
	return &fakeRows{cols: cols, data: rows}, nil
}
func (c *fakeConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	n, err := c.execFn(query, args)
	if err != nil {
		return nil, err
	}
	return driver.RowsAffected(n), nil
}

type fakeConnector struct{ conn *fakeConn }

func (f *fakeConnector) Connect(context.Context) (driver.Conn, error) { return f.conn, nil }
func (f *fakeConnector) Driver() driver.Driver                        { return nil }

func TestParseModeAndSweepCutoffMs(t *testing.T) {
	for _, s := range []string{"", "report_only", "report", "report-only"} {
		m, err := ParseMode(s)
		if err != nil || m != ModeReportOnly {
			t.Fatalf("ParseMode(%q) = %v, %v; want ModeReportOnly, nil", s, m, err)
		}
	}
	if m, err := ParseMode("delete"); err != nil || m != ModeDelete {
		t.Fatalf("ParseMode(delete) = %v, %v", m, err)
	}
	if m, err := ParseMode("del"); err == nil || m != ModeReportOnly {
		t.Fatalf("typo must fail and stay report_only, got %v %v", m, err)
	}
	if _, ok, err := SweepCutoffMs(90, 100, 1000); err != nil || ok {
		t.Fatalf("non-positive cutoff must skip, got ok=%v err=%v", ok, err)
	}
	if _, _, err := SweepCutoffMs(MaxRetentionDays+1, 100, 1_700_000_000_000); err == nil {
		t.Fatal("retentionDays above MaxRetentionDays must fail")
	}
}

func TestCheckPayload_ThreeTiers(t *testing.T) {
	limit := PayloadLimit{DB: "mmorpg_inbox", Table: "player_inbox", Column: "payload", Max: 100, AllowEqual: true, Hint: "check inbox max_payload_bytes"}
	if err := CheckPayload(context.Background(), limit, make([]byte, 50)); err != nil {
		t.Fatalf("below warn ratio must pass: %v", err)
	}
	if err := CheckPayload(context.Background(), limit, make([]byte, 85)); err != nil {
		t.Fatalf("approaching warn ratio must still allow: %v", err)
	}
	if err := CheckPayload(context.Background(), limit, make([]byte, 100)); err != nil {
		t.Fatalf("AllowEqual=true exact limit must allow: %v", err)
	}
	if err := CheckPayload(context.Background(), limit, make([]byte, 101)); err == nil {
		t.Fatal("exceeding Max must fail-closed")
	}
}

func TestGuardCheckAndStrictModeAndSweepTable(t *testing.T) {
	var lastExec string
	conn := &fakeConn{
		queryFn: func(query string) ([][]driver.Value, []string, error) {
			switch {
			case strings.Contains(query, "@@session.sql_mode"):
				return [][]driver.Value{{"STRICT_TRANS_TABLES,NO_ENGINE_SUBSTITUTION"}}, []string{"mode"}, nil
			case strings.Contains(query, "information_schema.TABLES"):
				return [][]driver.Value{{"player_inbox", int64(50000), int64(10_000_000), int64(2048)}}, []string{"t", "r", "b", "a"}, nil
			case strings.HasPrefix(query, "SELECT COUNT(*) FROM `player_inbox` WHERE status = 2"):
				return [][]driver.Value{{int64(42)}}, []string{"n"}, nil
			default:
				return nil, nil, errors.New("unexpected query: " + query)
			}
		},
		execFn: func(query string, _ []driver.NamedValue) (int64, error) {
			lastExec = query
			return 10, nil
		},
	}
	db := sql.OpenDB(&fakeConnector{conn: conn})
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	if err := AssertStrictMode(ctx, db); err != nil {
		t.Fatalf("AssertStrictMode failed: %v", err)
	}

	g := New(db, "mmorpg_inbox", []TableBudget{
		{Table: "player_inbox", MaxRows: 10000, MaxAvgRowBytes: 1024, Note: "check inbox sweep"},
	}, nil)
	res := g.Check(ctx)
	if res.Checked != 1 || len(res.Violations) != 2 {
		t.Fatalf("expected 2 violations (rows + avg_row_bytes), got %+v", res)
	}

	out, err := SweepTable(ctx, db, ModeReportOnly, "mmorpg_inbox", "player_inbox", "status = 2", 10)
	if err != nil || out.Matched != 42 || out.Deleted != 0 || out.Cleaned() {
		t.Fatalf("unexpected report_only outcome: %+v err=%v", out, err)
	}

	outDel, err := SweepTable(ctx, db, ModeDelete, "mmorpg_inbox", "player_inbox", "status = 2", 10)
	if err != nil || outDel.Deleted != 10 || !outDel.Truncated || !outDel.Cleaned() {
		t.Fatalf("unexpected delete outcome: %+v err=%v", outDel, err)
	}
	if !strings.Contains(lastExec, "WHERE status = 2 LIMIT ?") {
		t.Fatalf("expected shared WHERE + LIMIT in delete SQL, got %q", lastExec)
	}
}
