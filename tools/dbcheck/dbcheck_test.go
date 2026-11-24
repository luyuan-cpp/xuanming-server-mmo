package main

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

type stubRows struct {
	cols []string
	data [][]driver.Value
	idx  int
}

func (r *stubRows) Columns() []string { return r.cols }
func (r *stubRows) Close() error      { return nil }
func (r *stubRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.idx])
	r.idx++
	return nil
}

type stubConn struct {
	handler func(query string) (*stubRows, error)
}

func (c *stubConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (c *stubConn) Close() error                        { return nil }
func (c *stubConn) Begin() (driver.Tx, error)           { return nil, errors.New("unused") }
func (c *stubConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	return nil, errors.New("dbcheck must be strictly read-only, unexpected Exec: " + query)
}
func (c *stubConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	upper := strings.ToUpper(strings.TrimSpace(query))
	if !strings.HasPrefix(upper, "SELECT ") {
		return nil, errors.New("dbcheck must only execute SELECT, got: " + query)
	}
	return c.handler(query)
}

type stubConnector struct{ conn *stubConn }

func (c *stubConnector) Connect(context.Context) (driver.Conn, error) { return c.conn, nil }
func (c *stubConnector) Driver() driver.Driver                        { return nil }

func TestRunCheck_RegistryIndexesOutboxSnapshotAndBigFields(t *testing.T) {
	reg := map[string]map[string]tableEntry{
		"mmorpg_test": {
			"outbox_events": {Class: classOutbox},
			"swept_logs": {
				Class:           classSwept,
				RequiredIndexes: []indexSpec{{Name: "idx_created", Columns: []string{"created_at"}}},
				PendingWhere:    "created_at < NOW()",
			},
		},
	}
	fields := []fieldBudget{
		{DB: "mmorpg_test", Table: "swept_logs", Column: "payload", PK: "id", MaxBytes: 1024},
	}

	idxCols := [][]driver.Value{{"created_at"}, {"id"}}
	outboxRows := int64(5)
	overRows := int64(0)

	db := sql.OpenDB(&stubConnector{conn: &stubConn{handler: func(query string) (*stubRows, error) {
		switch {
		case strings.Contains(query, "information_schema.TABLES"):
			return &stubRows{
				cols: []string{"TABLE_SCHEMA", "TABLE_NAME", "TABLE_ROWS", "AVG_ROW_LENGTH"},
				data: [][]driver.Value{
					{"mmorpg_test", "outbox_events", int64(1), int64(256)},
					{"mmorpg_test", "swept_logs", int64(100), int64(512)},
					{"mmorpg_test", "schema_migrations", int64(3), int64(64)},
				},
			}, nil
		case strings.Contains(query, "information_schema.STATISTICS"):
			return &stubRows{cols: []string{"COLUMN_NAME"}, data: idxCols}, nil
		case strings.Contains(query, "FROM `mmorpg_test`.`outbox_events`"):
			return &stubRows{cols: []string{"c"}, data: [][]driver.Value{{outboxRows}}}, nil
		case strings.Contains(query, "WHERE created_at < NOW()"):
			return &stubRows{cols: []string{"c"}, data: [][]driver.Value{{int64(42)}}}, nil
		case strings.Contains(query, "MAX(LENGTH(`payload`))"):
			return &stubRows{cols: []string{"c", "m", "a", "o"}, data: [][]driver.Value{{int64(100), int64(2048), float64(400), overRows}}}, nil
		case strings.Contains(query, "ORDER BY n DESC"):
			return &stubRows{cols: []string{"pk", "n"}, data: [][]driver.Value{{"999", int64(2048)}}}, nil
		default:
			return nil, errors.New("unexpected query: " + query)
		}
	}}})
	defer func() { _ = db.Close() }()

	snapFile := filepath.Join(t.TempDir(), "snap.json")
	var buf bytes.Buffer
	violations, err := runCheck(context.Background(), db, reg, fields, options{
		schemaLike:     "mmorpg%",
		strictRegistry: true,
		snapshotOut:    snapFile,
		outboxMax:      10,
		pending:        true,
		sizeCheck:      true,
		topRows:        1,
	}, &buf)
	if err != nil {
		t.Fatalf("runCheck error: %v", err)
	}
	if violations != 0 {
		t.Fatalf("expected 0 violations, got %d; out=\n%s", violations, buf.String())
	}
	if !strings.Contains(buf.String(), "pending_rows=42") {
		t.Fatalf("expected pending report in output, got:\n%s", buf.String())
	}

	// 制造三项违规：outbox 积压 + 清理索引列序错 + 大字段超限
	outboxRows = 50
	idxCols = [][]driver.Value{{"wrong_col"}}
	overRows = 2
	buf.Reset()
	violations, err = runCheck(context.Background(), db, reg, fields, options{
		schemaLike:     "mmorpg%",
		strictRegistry: true,
		compareWith:    snapFile,
		outboxMax:      10,
		sizeCheck:      true,
		topRows:        1,
	}, &buf)
	if err != nil {
		t.Fatalf("runCheck error: %v", err)
	}
	if violations != 3 {
		t.Fatalf("expected 3 violations (outbox + index + bigfield), got %d; out=\n%s", violations, buf.String())
	}
	if !strings.Contains(buf.String(), "+45 (5 -> 50)") || !strings.Contains(buf.String(), "id=999 bytes=2048") {
		t.Fatalf("expected delta and top row in output, got:\n%s", buf.String())
	}
}
