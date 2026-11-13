//go:build integration

package playerstore

// 审计发件箱写入端的真库用例(docs/design/player-audit-outbox.md §6)。跑法与环境变量同 store_integration_test.go:
//
//	GODB_MYSQL_ITEST_DSN='root:pwd@tcp(127.0.0.1:3306)/' go test -tags=integration ./internal/playerstore/...

import (
	"context"
	"database/sql"
	"testing"

	"google.golang.org/protobuf/proto"

	"proto/common/component"
	dbpb "proto/common/database"
	rollbackpb "proto/common/rollback"
	storepb "proto/player_store"
)

func outboxCount(t *testing.T, ctx context.Context, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM player_audit_outbox").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func auditRequest(seq uint64, instance []byte, txIDs ...uint64) *storepb.SavePlayerRequest {
	req := &storepb.SavePlayerRequest{PlayerId: 7, OwnerEpoch: 100, SaveSeq: seq, SchemaGen: 1, InstanceId: instance, Rows: []*storepb.PlayerStoreRow{hot("h")}}
	for _, txID := range txIDs {
		req.TransactionLogs = append(req.TransactionLogs, &rollbackpb.TransactionLogEntry{TxId: txID, ToPlayer: 7, CurrencyDelta: 10})
	}
	return req
}

// 提交的存盘同事务写进发件箱;同一份请求重试拿到 EQUAL、不再写;被接管之后旧持有者带流水的存盘被拒,一条都不写。
func TestIT_AuditOutboxCommitsOnlyWithTheSave(t *testing.T) {
	store, db, ctx := itStore(t)
	seedOwner(t, ctx, db, 7)
	wantClaim(t, claim(t, ctx, store, 7, 100, instanceA), storepb.LoadAndClaimResult_LOAD_AND_CLAIM_CLAIMED)

	resp, err := store.Save(ctx, auditRequest(1, instanceA, 501, 502))
	if err != nil || resp.GetResult() != storepb.SavePlayerResult_SAVE_PLAYER_COMMITTED || !resp.GetAuditOutboxWritten() {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if n := outboxCount(t, ctx, db); n != 2 {
		t.Fatalf("outbox rows = %d, want 2", n)
	}
	// 回包丢了、scene 同内容同序号重试。
	resp, err = store.Save(ctx, auditRequest(1, instanceA, 501, 502))
	if err != nil || resp.GetResult() != storepb.SavePlayerResult_SAVE_PLAYER_EQUAL || !resp.GetAuditOutboxWritten() {
		t.Fatalf("retry resp=%+v err=%v", resp, err)
	}
	if n := outboxCount(t, ctx, db); n != 2 {
		t.Fatalf("an EQUAL retry must not write again, rows = %d", n)
	}
	// 别的节点接管;旧持有者再带着流水存盘。
	wantClaim(t, claim(t, ctx, store, 7, 101, instanceB), storepb.LoadAndClaimResult_LOAD_AND_CLAIM_CLAIMED)
	resp, err = store.Save(ctx, auditRequest(2, instanceA, 503))
	if err != nil || resp.GetResult() == storepb.SavePlayerResult_SAVE_PLAYER_COMMITTED || resp.GetAuditOutboxWritten() {
		t.Fatalf("stale holder resp=%+v err=%v", resp, err)
	}
	if n := outboxCount(t, ctx, db); n != 2 {
		t.Fatalf("a rejected save must not write audit records, rows = %d", n)
	}
	var record, seq uint64
	var shard uint32
	if err := db.QueryRowContext(ctx, "SELECT shard,record_id,save_seq FROM player_audit_outbox ORDER BY record_id LIMIT 1").Scan(&shard, &record, &seq); err != nil {
		t.Fatal(err)
	}
	if shard != AuditShardOf(7) || record != 501 || seq != 1 {
		t.Fatalf("row = shard %d record %d seq %d", shard, record, seq)
	}
}

// 发件箱写不进(缺表):整笔回滚,存档也没提交,序号不前进;scene 按结果未知同序号重试,不会出现「存档在、流水没」。
func TestIT_AuditOutboxFailureLeavesTheSaveUncommitted(t *testing.T) {
	store, db, ctx := itStore(t)
	seedOwner(t, ctx, db, 7)
	wantClaim(t, claim(t, ctx, store, 7, 100, instanceA), storepb.LoadAndClaimResult_LOAD_AND_CLAIM_CLAIMED)
	if _, err := db.ExecContext(ctx, "DROP TABLE player_audit_outbox"); err != nil {
		t.Fatal(err)
	}
	if resp, err := store.Save(ctx, auditRequest(1, instanceA, 501)); err == nil {
		t.Fatalf("resp = %+v, want an error when the outbox cannot be written", resp)
	}
	var seq uint64
	if err := db.QueryRowContext(ctx, "SELECT save_seq FROM player_store WHERE player_id=7 AND kind=0").Scan(&seq); err != nil {
		t.Fatal(err)
	}
	if seq != 0 {
		t.Fatalf("save_seq = %d, want 0: the save must roll back with the outbox write", seq)
	}
	var rows int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM player_store WHERE player_id=7 AND kind>0").Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("data rows = %d, %v; want none", rows, err)
	}
}

// 快照在提交事务里按这一笔写完之后的行拼:读已提交下同一事务读得到自己刚写的行,快照里是这一笔的内容,不是上一笔的。
func TestIT_SnapshotSeesTheRowsOfItsOwnSave(t *testing.T) {
	store, db, ctx := itStore(t)
	seedOwner(t, ctx, db, 7)
	wantClaim(t, claim(t, ctx, store, 7, 100, instanceA), storepb.LoadAndClaimResult_LOAD_AND_CLAIM_CLAIMED)
	level := func(n uint32) *storepb.PlayerStoreRow {
		payload, err := proto.Marshal(&dbpb.PlayerDatabase{LevelComponent: &component.LevelComp{Level: n}})
		if err != nil {
			t.Fatal(err)
		}
		return &storepb.PlayerStoreRow{Kind: storepb.PlayerStoreKind_PLAYER_STORE_KIND_CHARACTER, Payload: payload}
	}
	wantSave(t, save(t, ctx, store, 7, 100, 1, instanceA, false, level(10)), storepb.SavePlayerResult_SAVE_PLAYER_COMMITTED)
	req := &storepb.SavePlayerRequest{PlayerId: 7, OwnerEpoch: 100, SaveSeq: 2, SchemaGen: 1, InstanceId: instanceA,
		Rows: []*storepb.PlayerStoreRow{level(11)}, AuditSnapshots: []*rollbackpb.PlayerSnapshotEntry{{SnapshotId: 900}}}
	resp, err := store.Save(ctx, req)
	if err != nil || resp.GetResult() != storepb.SavePlayerResult_SAVE_PLAYER_COMMITTED {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	var payload []byte
	if err := db.QueryRowContext(ctx, "SELECT payload FROM player_audit_outbox WHERE record_id=900").Scan(&payload); err != nil {
		t.Fatal(err)
	}
	entry := &rollbackpb.PlayerSnapshotEntry{}
	data := &dbpb.PlayerDatabase{}
	if err := proto.Unmarshal(payload, entry); err != nil {
		t.Fatal(err)
	}
	if err := proto.Unmarshal(entry.GetPlayerDatabaseBlob(), data); err != nil {
		t.Fatal(err)
	}
	if data.GetLevelComponent().GetLevel() != 11 || data.GetPlayerId() != 7 {
		t.Fatalf("snapshot = %+v, want the rows written by this save (level 11)", data)
	}
}
