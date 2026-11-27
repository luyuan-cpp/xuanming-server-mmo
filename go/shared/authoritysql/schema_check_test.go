package authoritysql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

type schemaFakeState struct {
	tables  []string
	columns map[string][][]driver.Value // table -> rows of [name, data_type, is_nullable, column_key]
}

var (
	schemaFakeOnce   sync.Once
	schemaFakeMu     sync.Mutex
	schemaFakeStates = map[string]*schemaFakeState{}
)

type schemaFakeDriver struct{}

func (schemaFakeDriver) Open(dsn string) (driver.Conn, error) {
	schemaFakeMu.Lock()
	st := schemaFakeStates[dsn]
	schemaFakeMu.Unlock()
	if st == nil {
		return nil, errors.New("unknown schema fake dsn")
	}
	return &schemaFakeConn{st: st}, nil
}

type schemaFakeConn struct{ st *schemaFakeState }

func (c *schemaFakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unsupported") }
func (c *schemaFakeConn) Close() error                        { return nil }
func (c *schemaFakeConn) Begin() (driver.Tx, error)           { return nil, errors.New("unsupported") }

func (c *schemaFakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	q := strings.ToLower(query)
	switch {
	case strings.Contains(q, "information_schema.tables"):
		var data [][]driver.Value
		for _, t := range c.st.tables {
			data = append(data, []driver.Value{t})
		}
		return &schemaFakeRows{cols: []string{"table_name"}, data: data}, nil
	case strings.Contains(q, "data_type") && strings.Contains(q, "information_schema.columns"):
		tbl := args[0].Value.(string)
		return &schemaFakeRows{
			cols: []string{"column_name", "data_type", "is_nullable", "column_key"},
			data: c.st.columns[tbl],
		}, nil
	case strings.Contains(q, "information_schema.columns"):
		tbl := args[0].Value.(string)
		var data [][]driver.Value
		for _, row := range c.st.columns[tbl] {
			data = append(data, []driver.Value{row[0]})
		}
		return &schemaFakeRows{cols: []string{"column_name"}, data: data}, nil
	default:
		return nil, errors.New("unexpected query: " + query)
	}
}

type schemaFakeRows struct {
	cols []string
	data [][]driver.Value
	idx  int
}

func (r *schemaFakeRows) Columns() []string { return r.cols }
func (r *schemaFakeRows) Close() error      { return nil }
func (r *schemaFakeRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.idx])
	r.idx++
	return nil
}

func openSchemaFakeDB(t *testing.T, st *schemaFakeState) *sql.DB {
	t.Helper()
	schemaFakeOnce.Do(func() { sql.Register("authoritysql_schema_fake", schemaFakeDriver{}) })
	dsn := t.Name()
	schemaFakeMu.Lock()
	schemaFakeStates[dsn] = st
	schemaFakeMu.Unlock()
	db, err := sql.Open("authoritysql_schema_fake", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSchemaChecksFailClosedAndPass(t *testing.T) {
	ctx := context.Background()
	db := openSchemaFakeDB(t, &schemaFakeState{
		tables: []string{"owner_record", "owner_instance_lease"},
		columns: map[string][][]driver.Value{
			"owner_record": {
				{"player_id", "bigint", "NO", "PRI"},
				{"owner_epoch", "bigint", "NO", ""},
				{"source_revision", "int", "YES", ""},
			},
		},
	})

	if err := CheckTables(ctx, db, "DDLTiDB", "owner_record", "owner_instance_lease"); err != nil {
		t.Fatalf("CheckTables present failed: %v", err)
	}
	if err := CheckTables(ctx, db, "DDLTiDB", "owner_record", "owner_transition_log"); err == nil || !strings.Contains(err.Error(), "owner_transition_log") {
		t.Fatalf("CheckTables missing err = %v, want owner_transition_log", err)
	}

	if err := CheckColumns(ctx, db, "DDLTiDB", "owner_record", "player_id", "owner_epoch"); err != nil {
		t.Fatalf("CheckColumns present failed: %v", err)
	}
	if err := CheckColumns(ctx, db, "DDLTiDB", "owner_record", "admit_not_before_ms"); err == nil || !strings.Contains(err.Error(), "admit_not_before_ms") {
		t.Fatalf("CheckColumns missing err = %v, want admit_not_before_ms", err)
	}

	if err := CheckColumnSpecs(ctx, db, "DDLTiDB", "owner_record",
		ColumnSpec{Name: "player_id", DataType: "bigint", Nullable: "NO", Key: "PRI"},
	); err != nil {
		t.Fatalf("CheckColumnSpecs matching failed: %v", err)
	}

	err := CheckColumnSpecs(ctx, db, "DDLTiDB", "owner_record",
		ColumnSpec{Name: "source_revision", DataType: "bigint", Nullable: "NO"},
	)
	if err == nil || !strings.Contains(err.Error(), "data_type=int") || !strings.Contains(err.Error(), "is_nullable=YES") {
		t.Fatalf("CheckColumnSpecs mismatch err = %v, want data_type & is_nullable mismatch", err)
	}
}
