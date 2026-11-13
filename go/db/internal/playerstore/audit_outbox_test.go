package playerstore

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"db/internal/dbtest"

	"proto/common/component"
	dbpb "proto/common/database"
	rollbackpb "proto/common/rollback"
	storepb "proto/player_store"
)

// auditNow 是单测里 go/db 的墙钟:发件箱的 created_ms 与快照时间都取它。
var auditNow = time.UnixMilli(1_800_000_000_123)

func openAuditStore(t *testing.T, h dbtest.Handler) (*Store, *dbtest.Recorder) {
	t.Helper()
	store, rec := openStore(t, h)
	store.now = func() time.Time { return auditNow }
	return store, rec
}

func txlog(txID uint64) *rollbackpb.TransactionLogEntry {
	return &rollbackpb.TransactionLogEntry{TxId: txID, Timestamp: 1_800_000_000, ToPlayer: 7, CurrencyType: 1, CurrencyDelta: 100}
}

// outboxRows 把一条发件箱 INSERT 的参数按行切开(每行 7 列)。
func outboxRows(t *testing.T, args []driver.NamedValue) [][]driver.Value {
	t.Helper()
	if len(args)%7 != 0 {
		t.Fatalf("outbox insert has %d args, not a multiple of 7", len(args))
	}
	var rows [][]driver.Value
	for i := 0; i < len(args); i += 7 {
		row := make([]driver.Value, 7)
		for j := range row {
			row[j] = args[i+j].Value
		}
		rows = append(rows, row)
	}
	return rows
}

// 提交时流水和脏行在同一个事务里写:BEGIN → 锁写权行 → 写存档 → 写发件箱 → COMMIT。每行带片号、kind、提交时刻、玩家、
// 记录号、这一笔的序号和原样的消息体;回包置 audit_outbox_written。
func TestSave_WritesAuditRecordsInTheSaveTransaction(t *testing.T) {
	var outboxArgs []driver.NamedValue
	store, rec := openAuditStore(t, func(_ context.Context, query string, args []driver.NamedValue) (*dbtest.Rows, error) {
		switch {
		case strings.Contains(query, "FOR UPDATE"):
			return ownerValues(9, 5, 1, instanceA), nil
		case strings.HasPrefix(query, sqlAuditOutboxHead):
			outboxArgs = args
		}
		return nil, nil
	})
	req := validSave()
	req.TransactionLogs = []*rollbackpb.TransactionLogEntry{txlog(501), txlog(502)}
	resp, err := store.Save(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetResult() != storepb.SavePlayerResult_SAVE_PLAYER_COMMITTED || !resp.GetAuditOutboxWritten() {
		t.Fatalf("response = %+v, want COMMITTED with the outbox written", resp)
	}
	got := rec.Statements()
	if len(got) != 5 || got[0] != dbtest.StatementBegin || !strings.HasPrefix(got[2], "INSERT INTO player_store") ||
		!strings.HasPrefix(got[3], sqlAuditOutboxHead) || got[4] != dbtest.StatementCommit {
		t.Fatalf("statements = %v, want the outbox insert inside the save transaction, before COMMIT", got)
	}
	rows := outboxRows(t, outboxArgs)
	if len(rows) != 2 {
		t.Fatalf("outbox rows = %d, want 2", len(rows))
	}
	for i, txID := range []uint64{501, 502} {
		row := rows[i]
		want := []driver.Value{int64(AuditShardOf(7)), int64(storepb.PlayerAuditKind_PLAYER_AUDIT_KIND_TRANSACTION_LOG),
			auditNow.UnixMilli(), int64(7), int64(txID), int64(6)}
		for j := range want {
			if row[j] != want[j] {
				t.Fatalf("row %d column %d = %v, want %v (row %v)", i, j, row[j], want[j], row)
			}
		}
		entry := &rollbackpb.TransactionLogEntry{}
		if err := proto.Unmarshal(row[6].([]byte), entry); err != nil || entry.GetTxId() != txID || entry.GetCurrencyDelta() != 100 {
			t.Fatalf("payload of row %d = %v, %v", i, entry, err)
		}
	}
}

// 被拒绝的存盘一条审计记录都不写;EQUAL 也不写(同一份请求之前提交时同一个事务已经写过),但回包确认写过。
func TestSave_RejectedAndEqualSavesWriteNoAuditRecords(t *testing.T) {
	cases := []struct {
		name        string
		owner       *dbtest.Rows
		want        storepb.SavePlayerResult
		wantWritten bool
	}{
		{"被接管", ownerValues(20, 9, 1, instanceB), storepb.SavePlayerResult_SAVE_PLAYER_STALE_OWNER, false},
		{"同序号重试", ownerValues(10, 6, 1, instanceA), storepb.SavePlayerResult_SAVE_PLAYER_EQUAL, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, rec := openAuditStore(t, func(context.Context, string, []driver.NamedValue) (*dbtest.Rows, error) {
				return tc.owner, nil
			})
			req := validSave()
			req.TransactionLogs = []*rollbackpb.TransactionLogEntry{txlog(501)}
			req.AuditSnapshots = []*rollbackpb.PlayerSnapshotEntry{{SnapshotId: 900}}
			resp, err := store.Save(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if resp.GetResult() != tc.want || resp.GetAuditOutboxWritten() != tc.wantWritten {
				t.Fatalf("response = %+v, want %v written=%v", resp, tc.want, tc.wantWritten)
			}
			if rec.Contains(AuditOutboxTable) || rec.Contains(dbtest.StatementCommit) {
				t.Fatalf("statements = %v, want no outbox write and no commit", rec.Statements())
			}
		})
	}
}

// 发件箱写失败按存档写失败处理:整笔回滚、回错误(结果未知),scene 同内容同序号重试。不能出现「存档提交了、流水没写」。
func TestSave_AuditOutboxFailureRollsTheSaveBack(t *testing.T) {
	boom := errors.New("table player_audit_outbox doesn't exist")
	store, rec := openAuditStore(t, func(_ context.Context, query string, _ []driver.NamedValue) (*dbtest.Rows, error) {
		switch {
		case strings.Contains(query, "FOR UPDATE"):
			return ownerValues(9, 5, 1, instanceA), nil
		case strings.HasPrefix(query, sqlAuditOutboxHead):
			return nil, boom
		}
		return nil, nil
	})
	req := validSave()
	req.TransactionLogs = []*rollbackpb.TransactionLogEntry{txlog(501)}
	resp, err := store.Save(context.Background(), req)
	if !errors.Is(err, boom) || resp != nil {
		t.Fatalf("resp=%+v err=%v, want the outbox error as an unknown outcome", resp, err)
	}
	if rec.Contains(dbtest.StatementCommit) || !rec.Contains(dbtest.StatementRollback) {
		t.Fatalf("statements = %v, want a rollback and no commit", rec.Statements())
	}
}

// 快照按这一笔写完之后的全部数据行拼:拼接即合并,填玩家号和这一笔的世代;请求里 scene 带的整档、时间、大小一律被覆盖,
// 其余字段(触发、版本、zone)原样保留。读行在写存档之后、写发件箱之前,同一个事务。
func TestSave_SnapshotIsBuiltFromTheCommittedRows(t *testing.T) {
	character, err := proto.Marshal(&dbpb.PlayerDatabase{LevelComponent: &component.LevelComp{Level: 30}})
	if err != nil {
		t.Fatal(err)
	}
	wallet, err := proto.Marshal(&dbpb.PlayerDatabase{Currency: &component.CurrencyComp{Values: []uint64{0, 500}}})
	if err != nil {
		t.Fatal(err)
	}
	var outboxArgs []driver.NamedValue
	store, rec := openAuditStore(t, func(_ context.Context, query string, args []driver.NamedValue) (*dbtest.Rows, error) {
		switch {
		case strings.Contains(query, "FOR UPDATE"):
			return ownerValues(9, 5, 1, instanceA), nil
		case strings.Contains(query, "kind>0"):
			return &dbtest.Rows{Columns: []string{"kind", "payload"}, Values: [][]driver.Value{
				{int64(storepb.PlayerStoreKind_PLAYER_STORE_KIND_WALLET), wallet},
				{int64(storepb.PlayerStoreKind_PLAYER_STORE_KIND_CHARACTER), character},
			}}, nil
		case strings.HasPrefix(query, sqlAuditOutboxHead):
			outboxArgs = args
		}
		return nil, nil
	})
	req := validSave()
	req.AuditSnapshots = []*rollbackpb.PlayerSnapshotEntry{{
		SnapshotId: 900, PlayerId: 7, Trigger: rollbackpb.SnapshotTrigger_SNAPSHOT_LOGIN, SchemaVersion: "v1", ZoneId: 3,
		SnapshotTime: 1, PlayerDatabaseBlob: []byte("from scene, must be replaced"), TotalBytes: 1,
	}}
	resp, err := store.Save(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetResult() != storepb.SavePlayerResult_SAVE_PLAYER_COMMITTED || !resp.GetAuditOutboxWritten() {
		t.Fatalf("response = %+v", resp)
	}
	got := rec.Statements()
	if len(got) != 6 || !strings.HasPrefix(got[2], "INSERT INTO player_store") || !strings.Contains(got[3], "kind>0") ||
		!strings.HasPrefix(got[4], sqlAuditOutboxHead) || got[5] != dbtest.StatementCommit {
		t.Fatalf("statements = %v, want write rows → read rows → write outbox → commit", got)
	}
	rows := outboxRows(t, outboxArgs)
	if len(rows) != 1 || rows[0][1] != int64(storepb.PlayerAuditKind_PLAYER_AUDIT_KIND_PLAYER_SNAPSHOT) || rows[0][4] != int64(900) {
		t.Fatalf("outbox rows = %v", rows)
	}
	payload := rows[0][6].([]byte)
	entry := &rollbackpb.PlayerSnapshotEntry{}
	if err := proto.Unmarshal(payload, entry); err != nil {
		t.Fatal(err)
	}
	// total_bytes 照 scene 原来的做法量:先按 0 序列化量出大小再填进去,所以不含它自己那个字段的几个字节。
	if entry.GetPlayerId() != 7 || entry.GetSnapshotTime() != uint64(auditNow.Unix()) ||
		entry.GetTotalBytes() == 0 || entry.GetTotalBytes() > uint64(len(payload)) {
		t.Fatalf("snapshot header = player %d time %d total %d (payload %d bytes)", entry.GetPlayerId(), entry.GetSnapshotTime(), entry.GetTotalBytes(), len(payload))
	}
	if entry.GetTrigger() != rollbackpb.SnapshotTrigger_SNAPSHOT_LOGIN || entry.GetSchemaVersion() != "v1" || entry.GetZoneId() != 3 {
		t.Fatalf("request fields must be kept: %+v", entry)
	}
	data := &dbpb.PlayerDatabase{}
	if err := proto.Unmarshal(entry.GetPlayerDatabaseBlob(), data); err != nil {
		t.Fatal(err)
	}
	if data.GetPlayerId() != 7 || data.GetPersistSchemaGen() != req.GetSchemaGen() || data.GetLevelComponent().GetLevel() != 30 ||
		len(data.GetCurrency().GetValues()) != 2 || data.GetCurrency().GetValues()[1] != 500 {
		t.Fatalf("snapshot body = %+v, want the merged committed rows", data)
	}
}

// 坏的单条记录只跳过,存盘照常提交:tx_id 为 0、超过单条上限的流水、snapshot_id 为 0 或玩家号对不上的快照请求。
func TestSave_BadAuditRecordsAreSkippedButTheSaveCommits(t *testing.T) {
	var outboxArgs []driver.NamedValue
	store, rec := openAuditStore(t, func(_ context.Context, query string, args []driver.NamedValue) (*dbtest.Rows, error) {
		switch {
		case strings.Contains(query, "FOR UPDATE"):
			return ownerValues(9, 5, 1, instanceA), nil
		case strings.Contains(query, "kind>0"):
			return &dbtest.Rows{Columns: []string{"kind", "payload"}}, nil
		case strings.HasPrefix(query, sqlAuditOutboxHead):
			outboxArgs = args
		}
		return nil, nil
	})
	oversized := txlog(504)
	oversized.Extra = strings.Repeat("x", MaxAuditTransactionLogBytes)
	req := validSave()
	req.TransactionLogs = []*rollbackpb.TransactionLogEntry{txlog(0), txlog(503), oversized}
	req.AuditSnapshots = []*rollbackpb.PlayerSnapshotEntry{{SnapshotId: 0}, {SnapshotId: 901, PlayerId: 8}}
	resp, err := store.Save(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetResult() != storepb.SavePlayerResult_SAVE_PLAYER_COMMITTED || !resp.GetAuditOutboxWritten() || !rec.Contains(dbtest.StatementCommit) {
		t.Fatalf("response = %+v, statements = %v: bad audit records must not fail the save", resp, rec.Statements())
	}
	rows := outboxRows(t, outboxArgs)
	if len(rows) != 1 || rows[0][4] != int64(503) {
		t.Fatalf("outbox rows = %v, want only tx 503", rows)
	}
}

// 这名玩家有本 go/db 不认识的行种:拼出来会缺字段,宁可少一份快照,也不落一份残缺的。存档照常提交;没有别的记录时不写发件箱。
func TestSave_SnapshotIsSkippedWhenARowKindIsUnknown(t *testing.T) {
	store, rec := openAuditStore(t, func(_ context.Context, query string, _ []driver.NamedValue) (*dbtest.Rows, error) {
		switch {
		case strings.Contains(query, "FOR UPDATE"):
			return ownerValues(9, 5, 1, instanceA), nil
		case strings.Contains(query, "kind>0"):
			return &dbtest.Rows{Columns: []string{"kind", "payload"}, Values: [][]driver.Value{{int64(99), []byte{}}}}, nil
		}
		return nil, nil
	})
	req := validSave()
	req.AuditSnapshots = []*rollbackpb.PlayerSnapshotEntry{{SnapshotId: 900}}
	resp, err := store.Save(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetResult() != storepb.SavePlayerResult_SAVE_PLAYER_COMMITTED || !rec.Contains(dbtest.StatementCommit) {
		t.Fatalf("response = %+v", resp)
	}
	if rec.Contains(AuditOutboxTable) {
		t.Fatalf("statements = %v, want no outbox write", rec.Statements())
	}
}

// 没有审计记录的存盘不多发任何语句。
func TestSave_NoAuditRecordsNoExtraStatement(t *testing.T) {
	store, rec := openAuditStore(t, func(_ context.Context, query string, _ []driver.NamedValue) (*dbtest.Rows, error) {
		if strings.Contains(query, "FOR UPDATE") {
			return ownerValues(9, 5, 1, instanceA), nil
		}
		return nil, nil
	})
	resp, err := store.Save(context.Background(), validSave())
	if err != nil || !resp.GetAuditOutboxWritten() {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if rec.Contains(AuditOutboxTable) || rec.Contains("kind>0") {
		t.Fatalf("statements = %v", rec.Statements())
	}
}

// 行预算调大到装不下审计记录时拒绝启动:否则 scene 会在本地按 TOO_LARGE 拒掉整笔存盘。
func TestNew_RejectsARowBudgetWithNoRoomForAuditRecords(t *testing.T) {
	db, _ := dbtest.OpenTx(t, noQuery(t))
	_, err := New(db, 1, Limits{MaxRowBytes: 1 << 20, MaxRequestBytes: 3<<20 + MaxAuditBytesPerSave})
	if err == nil || !strings.Contains(err.Error(), "audit") {
		t.Fatalf("err = %v, want the audit budget rejection", err)
	}
	if _, err := New(db, 1, DefaultLimits()); err != nil {
		t.Fatalf("the default limits must leave room for audit records: %v", err)
	}
}

// 两边的上限要同一个值:C++ 一笔存盘最多带 kMaxBytesPerSave 字节的审计记录,go/db 按同一个数核行预算还装不装得下审计;
// 单条上限两边一致,scene 收下的流水 go/db 才不会再丢。
func TestAuditLimits_MatchCpp(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "cpp", "libs", "modules", "audit", "audit_outbox.h"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		fmt.Sprintf("kMaxBytesPerSave = %d * 1024", MaxAuditBytesPerSave>>10),
		fmt.Sprintf("kMaxRecordBytes = %d * 1024", MaxAuditTransactionLogBytes>>10),
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("audit_outbox.h must declare %q", want)
		}
	}
}

func TestAuditShardOf(t *testing.T) {
	for player, want := range map[uint64]uint32{7: 7, 23: 7, 16: 0, 0: 0} {
		if got := AuditShardOf(player); got != want {
			t.Fatalf("AuditShardOf(%d) = %d, want %d", player, got, want)
		}
	}
}
