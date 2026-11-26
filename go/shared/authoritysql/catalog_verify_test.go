package authoritysql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	databasepb "proto/common/database"
	dbopts "proto/db"
)

type catalogFakeRows struct {
	cols []string
	rows [][]driver.Value
	idx  int
}

func (r *catalogFakeRows) Columns() []string { return r.cols }
func (r *catalogFakeRows) Close() error      { return nil }
func (r *catalogFakeRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.idx])
	r.idx++
	return nil
}

type catalogFakeConn struct {
	colRows [][]driver.Value
	idxRows [][]driver.Value
	queries []string
	argLens []int
}

func (*catalogFakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unexpected prepare") }
func (*catalogFakeConn) Begin() (driver.Tx, error)           { return nil, errors.New("unexpected begin") }
func (*catalogFakeConn) Close() error                        { return nil }
func (c *catalogFakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.queries = append(c.queries, query)
	c.argLens = append(c.argLens, len(args))
	if strings.Contains(query, "information_schema.columns") {
		return &catalogFakeRows{cols: []string{"table_name", "column_name"}, rows: c.colRows}, nil
	}
	if strings.Contains(query, "information_schema.statistics") {
		return &catalogFakeRows{cols: []string{"table_name", "index_name", "column_name", "non_unique", "sub_part"}, rows: c.idxRows}, nil
	}
	return nil, errors.New("unexpected query: " + query)
}

type catalogFakeConnector struct {
	conn     *catalogFakeConn
	connects int
}

func (c *catalogFakeConnector) Connect(context.Context) (driver.Conn, error) {
	c.connects++
	return c.conn, nil
}
func (*catalogFakeConnector) Driver() driver.Driver { return opaqueDriver{} }

func healthyCatalogRows(t *testing.T) ([][]driver.Value, [][]driver.Value) {
	t.Helper()
	var colRows, idxRows [][]driver.Value
	for _, model := range []proto.Message{
		&databasepb.RouteMeta{}, &databasepb.SqlWriterRegistry{}, &databasepb.SqlWriterSlot{},
		&databasepb.SqlWriterLifecycleReceipt{}, &databasepb.SqlWriterDrainRun{},
		&databasepb.SqlWriterFreezeTicket{}, &databasepb.SqlWriterMaintenanceReceipt{},
	} {
		desc := model.ProtoReflect().Descriptor()
		table := proto.GetExtension(desc.Options(), dbopts.E_OptionTableName).(string)
		for i := 0; i < desc.Fields().Len(); i++ {
			colRows = append(colRows, []driver.Value{table, string(desc.Fields().Get(i).Name())})
		}
		pk := proto.GetExtension(desc.Options(), dbopts.E_OptionPrimaryKey).(string)
		for _, col := range strings.Split(pk, ",") {
			idxRows = append(idxRows, []driver.Value{table, "PRIMARY", strings.TrimSpace(col), int64(0), nil})
		}
	}
	for _, idx := range []struct {
		table, name string
		cols        []string
		nonUnique   int64
	}{
		{"writer_registry", "uk_principal_seq", []string{"sql_principal", "boot_seq"}, 0},
		{"writer_registry", "uk_instance", []string{"instance_id"}, 0},
		{"sql_writer_freeze_ticket", "uk_op_rev", []string{"operation_id", "attempt_revision"}, 0},
		{"sql_writer_freeze_ticket", "uk_freeze_req", []string{"freeze_request_id"}, 0},
		{"writer_lifecycle_receipt", "idx_kind_digest", []string{"kind", "request_digest"}, 1},
		{"writer_lifecycle_receipt", "idx_rev_req", []string{"catalog_revision", "request_id"}, 1},
	} {
		for _, col := range idx.cols {
			idxRows = append(idxRows, []driver.Value{idx.table, idx.name, col, idx.nonUnique, nil})
		}
	}
	return colRows, idxRows
}

func TestVerifyCatalogSchemaBatchesOnSingleConnectionAndEnforcesShape(t *testing.T) {
	colRows, idxRows := healthyCatalogRows(t)
	conn := &catalogFakeConn{colRows: colRows, idxRows: idxRows}
	connector := &catalogFakeConnector{conn: conn}
	db := sql.OpenDB(connector)
	defer db.Close()

	if err := VerifyCatalogSchema(context.Background(), db, "game"); err != nil {
		t.Fatalf("healthy catalog rejected: %v", err)
	}
	if connector.connects != 1 {
		t.Fatalf("expected 1 physical connection, got %d", connector.connects)
	}
	if len(conn.queries) != 2 {
		t.Fatalf("expected 2 batched metadata queries, got %d: %v", len(conn.queries), conn.queries)
	}

	// 1. 缺正式 proto 列必须拒绝。
	missingCol := sql.OpenDB(&catalogFakeConnector{conn: &catalogFakeConn{colRows: colRows[1:], idxRows: idxRows}})
	defer missingCol.Close()
	if err := VerifyCatalogSchema(context.Background(), missingCol, "game"); err == nil {
		t.Fatal("missing proto column must be rejected")
	}

	// 2. 主键列缺失必须拒绝。
	missingPK := sql.OpenDB(&catalogFakeConnector{conn: &catalogFakeConn{colRows: colRows, idxRows: idxRows[1:]}})
	defer missingPK.Close()
	if err := VerifyCatalogSchema(context.Background(), missingPK, "game"); err == nil {
		t.Fatal("mismatched primary key must be rejected")
	}

	// 3. 唯一防复用索引退化为非唯一必须拒绝。
	nonUnique := make([][]driver.Value, len(idxRows))
	for i, r := range idxRows {
		cp := append([]driver.Value(nil), r...)
		if cp[0] == "writer_registry" && cp[1] == "uk_instance" {
			cp[3] = int64(1)
		}
		nonUnique[i] = cp
	}
	badUniqueDB := sql.OpenDB(&catalogFakeConnector{conn: &catalogFakeConn{colRows: colRows, idxRows: nonUnique}})
	defer badUniqueDB.Close()
	if err := VerifyCatalogSchema(context.Background(), badUniqueDB, "game"); err == nil {
		t.Fatal("non-unique index for unique check must be rejected")
	}

	// 4. 前缀索引(sub_part 非空)必须拒绝。
	prefixIdx := make([][]driver.Value, len(idxRows))
	for i, r := range idxRows {
		cp := append([]driver.Value(nil), r...)
		if cp[0] == "writer_registry" && cp[1] == "uk_principal_seq" && cp[2] == "sql_principal" {
			cp[4] = int64(32)
		}
		prefixIdx[i] = cp
	}
	badPrefixDB := sql.OpenDB(&catalogFakeConnector{conn: &catalogFakeConn{colRows: colRows, idxRows: prefixIdx}})
	defer badPrefixDB.Close()
	if err := VerifyCatalogSchema(context.Background(), badPrefixDB, "game"); err == nil {
		t.Fatal("prefix index must be rejected")
	}
}
