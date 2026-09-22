package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"data_service/cmd/debugutil"
)

func TestLoadExportFilePlayerMode(t *testing.T) {
	content := `{
  "mode": "player",
  "player_id": 12345,
  "home_zone_id": 1,
  "version": 42,
  "field_count": 2,
  "fields": {
    "bag": {
      "kind": "protobuf",
      "size": 10,
      "raw_base64": "` + base64.StdEncoding.EncodeToString([]byte("bag-binary")) + `",
      "proto_type": "PlayerDatabase"
    },
    "currency": {
      "kind": "text",
      "size": 5,
      "raw_base64": "` + base64.StdEncoding.EncodeToString([]byte("hello")) + `",
      "text": "hello"
    }
  }
}`
	path := filepath.Join(t.TempDir(), "export.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	data, err := loadExportFile(path)
	if err != nil {
		t.Fatalf("loadExportFile: %v", err)
	}
	if data.Mode != "player" {
		t.Fatalf("expected mode=player, got %q", data.Mode)
	}
	if data.PlayerID != 12345 {
		t.Fatalf("expected player_id=12345, got %d", data.PlayerID)
	}
	if data.HomeZoneID != 1 {
		t.Fatalf("expected home_zone_id=1, got %d", data.HomeZoneID)
	}
	if len(data.Fields) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(data.Fields))
	}

	bag := data.Fields["bag"]
	if bag.RawBase64 == "" {
		t.Fatal("expected bag.RawBase64 to be set")
	}
	decoded, _ := base64.StdEncoding.DecodeString(bag.RawBase64)
	if string(decoded) != "bag-binary" {
		t.Fatalf("expected bag raw bytes = 'bag-binary', got %q", decoded)
	}
}

func TestLoadExportFileMissingMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte(`{"player_id": 1}`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := loadExportFile(path)
	if err == nil {
		t.Fatal("expected error for missing mode")
	}
}

func TestResolveColumnValueNullAndScalar(t *testing.T) {
	// null
	v, err := resolveColumnValue(json.RawMessage(`null`))
	if err != nil || v != nil {
		t.Fatalf("null: got %v, err %v", v, err)
	}

	// string
	v, err = resolveColumnValue(json.RawMessage(`"hello"`))
	if err != nil || v != "hello" {
		t.Fatalf("string: got %v, err %v", v, err)
	}

	// integer
	v, err = resolveColumnValue(json.RawMessage(`42`))
	if err != nil {
		t.Fatalf("int: err %v", err)
	}
	if v != int64(42) {
		t.Fatalf("int: got %v (%T)", v, v)
	}

	// float
	v, err = resolveColumnValue(json.RawMessage(`3.14`))
	if err != nil {
		t.Fatalf("float: err %v", err)
	}
	if v != 3.14 {
		t.Fatalf("float: got %v", v)
	}
}

func TestResolveColumnValueRenderedBinary(t *testing.T) {
	raw := []byte{0x08, 0x96, 0x01}
	b64 := base64.StdEncoding.EncodeToString(raw)

	rv := map[string]any{
		"kind":       "base64",
		"size":       3,
		"raw_base64": b64,
	}
	encoded, _ := json.Marshal(rv)

	v, err := resolveColumnValue(json.RawMessage(encoded))
	if err != nil {
		t.Fatalf("resolveColumnValue: %v", err)
	}

	got, ok := v.([]byte)
	if !ok {
		t.Fatalf("expected []byte, got %T", v)
	}
	if len(got) != len(raw) || got[0] != raw[0] || got[1] != raw[1] || got[2] != raw[2] {
		t.Fatalf("bytes mismatch: got %v, want %v", got, raw)
	}
}

func TestResolveColumnValueRenderedText(t *testing.T) {
	rv := map[string]any{
		"kind": "text",
		"size": 5,
		"text": "hello",
	}
	encoded, _ := json.Marshal(rv)

	v, err := resolveColumnValue(json.RawMessage(encoded))
	if err != nil {
		t.Fatalf("resolveColumnValue: %v", err)
	}
	if v != "hello" {
		t.Fatalf("expected 'hello', got %v", v)
	}
}

// TestImportTableTxIsReadCommitted 钉住审计 #13 的修法**在调用点上**生效:importTable 开的事务必须显式
// READ COMMITTED,且 DELETE 与 INSERT 都在这一个事务里、最后提交。退回 BeginTx(ctx, nil)(= 库默认 RR)时,
// 两个并发导入的"DELETE 未命中 + INSERT"会在主键间隙上成环(见 importTableTxOptions)。
//
// 只断言 importTableTxOptions() 的返回值钉不住调用点(有人把调用点改回 nil,那条断言照样绿),所以这里用一个
// 记录 BeginTx 选项与每条语句的假驱动,直接跑 importTable 本身。
func TestImportTableTxIsReadCommitted(t *testing.T) {
	rec := &txRecorder{}
	db := sql.OpenDB(recordingConnector{rec: rec})
	defer db.Close()

	tbl := tableImport{
		Table:       "player_database",
		MatchColumn: "player_id",
		Columns:     []string{"data", "player_id"},
		Rows:        [][]any{{[]byte("blob-1"), int64(42)}, {[]byte("blob-2"), int64(42)}},
	}
	if err := importTable(context.Background(), db, "`player_database`", tbl, 42); err != nil {
		t.Fatalf("importTable: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.begins) != 1 {
		t.Fatalf("每张表应恰好开 1 个事务,got %d", len(rec.begins))
	}
	if got := rec.begins[0].Isolation; got != driver.IsolationLevel(sql.LevelReadCommitted) {
		t.Fatalf("导入事务隔离级别应为 READ COMMITTED(%d),got %d:调用点退回了库默认的 REPEATABLE READ?",
			sql.LevelReadCommitted, got)
	}
	if rec.begins[0].ReadOnly {
		t.Fatal("导入事务要写库,不能是只读事务")
	}
	if rec.commits != 1 || rec.rollbacks != 0 {
		t.Fatalf("应提交 1 次、不回滚,got commits=%d rollbacks=%d", rec.commits, rec.rollbacks)
	}
	if len(rec.execs) != 1+len(tbl.Rows) {
		t.Fatalf("应执行 1 条 DELETE + %d 条 INSERT,got %d 条", len(tbl.Rows), len(rec.execs))
	}
	for i, e := range rec.execs {
		if !e.inTx {
			t.Fatalf("第 %d 条语句不在导入事务里:%s", i, e.query)
		}
		wantPrefix := "INSERT INTO `player_database`"
		if i == 0 {
			wantPrefix = "DELETE FROM `player_database`"
		}
		if !strings.HasPrefix(e.query, wantPrefix) {
			t.Fatalf("第 %d 条语句应以 %q 开头,got %q", i, wantPrefix, e.query)
		}
	}
}

// ── 记录 BeginTx 选项与语句的假驱动(只实现 importTable 用得到的那部分)──

type recordedExec struct {
	query string
	inTx  bool
}

type txRecorder struct {
	mu        sync.Mutex
	begins    []driver.TxOptions
	execs     []recordedExec
	commits   int
	rollbacks int
	inTx      bool
}

type recordingConnector struct{ rec *txRecorder }

func (c recordingConnector) Connect(context.Context) (driver.Conn, error) {
	return &recordingConn{rec: c.rec}, nil
}

func (recordingConnector) Driver() driver.Driver { return recordingDriver{} }

type recordingDriver struct{}

func (recordingDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("录制驱动只经 sql.OpenDB(recordingConnector) 使用")
}

type recordingConn struct{ rec *txRecorder }

func (c *recordingConn) Prepare(query string) (driver.Stmt, error) {
	return nil, fmt.Errorf("录制驱动不支持 Prepare(导入路径应走 ExecContext): %s", query)
}

func (c *recordingConn) Close() error { return nil }

func (c *recordingConn) Begin() (driver.Tx, error) {
	return nil, errors.New("录制驱动只接受 BeginTx:旧式 Begin 带不出隔离级别")
}

func (c *recordingConn) BeginTx(_ context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.rec.mu.Lock()
	defer c.rec.mu.Unlock()
	c.rec.begins = append(c.rec.begins, opts)
	c.rec.inTx = true
	return recordingTx{rec: c.rec}, nil
}

func (c *recordingConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.rec.mu.Lock()
	defer c.rec.mu.Unlock()
	c.rec.execs = append(c.rec.execs, recordedExec{query: query, inTx: c.rec.inTx})
	return driver.RowsAffected(1), nil
}

type recordingTx struct{ rec *txRecorder }

func (tx recordingTx) Commit() error {
	tx.rec.mu.Lock()
	defer tx.rec.mu.Unlock()
	tx.rec.commits++
	tx.rec.inTx = false
	return nil
}

func (tx recordingTx) Rollback() error {
	tx.rec.mu.Lock()
	defer tx.rec.mu.Unlock()
	tx.rec.rollbacks++
	tx.rec.inTx = false
	return nil
}

func TestQuoteIdentifierSafety(t *testing.T) {
	good := []string{"player_table", "data", "col1"}
	for _, name := range good {
		if _, err := debugutil.QuoteIdentifier(name); err != nil {
			t.Fatalf("expected %q to pass: %v", name, err)
		}
	}

	bad := []string{"", "player-table", "t;drop", "a b"}
	for _, name := range bad {
		if _, err := debugutil.QuoteIdentifier(name); err == nil {
			t.Fatalf("expected %q to be rejected", name)
		}
	}
}
