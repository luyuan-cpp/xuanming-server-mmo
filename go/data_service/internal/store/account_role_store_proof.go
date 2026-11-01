package store

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"

	dbpb "proto/common/database"
	dspb "proto/data_service"
	"shared/accountdirectory"
	"shared/generated/persistschema"
)

// player_store 上的交易冻结证据(docs/design/player-save-direct-commit.md §6)。
// 版本看写权行;摘要覆盖这名玩家的所有行,保留原字节(含未知字段所在的 payload)。

type playerStoreProofRow struct {
	kind                  int32
	ownerEpoch, saveSeq   uint64
	schemaGen, released   uint32
	holderInstance, bytes []byte
}

const playerStoreProofSQL = "SELECT kind,owner_epoch,save_seq,schema_gen,released,holder_instance,payload FROM player_store WHERE player_id=? ORDER BY kind"

// readPlayerStoreProofRows 读出这名玩家的所有行,写权行在最前。一条语句读完,看到的是同一个时刻。
// lock 为 true 时锁住这些行,只在存档库自己的事务里用。读完就关游标:同一事务后面还要发语句。
func readPlayerStoreProofRows(ctx context.Context, q directoryQuery, player uint64, lock bool) ([]playerStoreProofRow, error) {
	query := playerStoreProofSQL
	if lock {
		query += " FOR UPDATE"
	}
	rows, err := q.QueryContext(ctx, query, player)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []playerStoreProofRow
	total := 0
	for rows.Next() {
		var row playerStoreProofRow
		if err := rows.Scan(&row.kind, &row.ownerEpoch, &row.saveSeq, &row.schemaGen, &row.released, &row.holderInstance, &row.bytes); err != nil {
			return nil, err
		}
		total += len(row.holderInstance) + len(row.bytes)
		if total > playerStoreProofMaxBytes {
			return nil, errors.New("玩家 SQL 证据超过预算")
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, accountdirectory.ErrMissing
	}
	if out[0].kind != playerStoreOwnerKind {
		return nil, errors.New("玩家 SQL 证据缺写权行")
	}
	return out, nil
}

// digestPlayerStoreRows 是冻结证据的摘要。
func digestPlayerStoreRows(schema string, rows []playerStoreProofRow) []byte {
	digest := sha256.New()
	writeProofPart(digest, []byte("account-role-store-proof-v1"))
	writeProofPart(digest, []byte(schema))
	for _, row := range rows {
		var head [32]byte
		binary.BigEndian.PutUint32(head[0:], uint32(row.kind))
		binary.BigEndian.PutUint64(head[4:], row.ownerEpoch)
		binary.BigEndian.PutUint64(head[12:], row.saveSeq)
		binary.BigEndian.PutUint32(head[20:], row.schemaGen)
		binary.BigEndian.PutUint32(head[24:], row.released)
		writeProofPart(digest, head[:])
		writeProofPart(digest, row.holderInstance)
		writeProofPart(digest, row.bytes)
	}
	return digest.Sum(nil)
}

// isInitialPlayerStore:写权行还是建角时的 (1, 1),而且只有建角写的那几行。认领会抬 E,存盘会抬 q,所以这名玩家从没进过场。
func isInitialPlayerStore(rows []playerStoreProofRow) bool {
	for _, row := range rows {
		if row.ownerEpoch != accountdirectory.InitialOwnerEpoch || row.saveSeq != accountdirectory.InitialSaveSeq {
			return false
		}
	}
	return true
}

// decideFrozenEvidence 由存档行得出证据候选。读到的版本低于下限、世代超前、初始形态对不上都拒绝。
// 初始提交还要在目录事务里核 initial_player_commit 和修订号(checkEvidenceInTx)。
func decideFrozenEvidence(r *dbpb.AccountRoleTrade, holders int, schema string, rows []playerStoreProofRow) (*dspb.AccountRoleFreezeEvidence, error) {
	owner := rows[0]
	if !accountdirectory.ValidSaveFloor(owner.ownerEpoch, owner.saveSeq) {
		return nil, errors.New("玩家 SQL 证据围栏非法")
	}
	if uint64(owner.schemaGen) > uint64(persistschema.Gen) {
		return nil, fmt.Errorf("玩家 SQL 证据世代超前: %d", owner.schemaGen)
	}
	if saveFloorAfter(r.RequiredOwnerEpoch, r.RequiredSaveSeq, owner.ownerEpoch, owner.saveSeq) {
		return nil, accountdirectory.ErrConflict
	}
	kind := dspb.AccountRoleFreezeEvidenceKind_ACCOUNT_ROLE_FREEZE_EVIDENCE_KIND_SCENE
	if owner.ownerEpoch == accountdirectory.InitialOwnerEpoch && owner.saveSeq == accountdirectory.InitialSaveSeq {
		if r.RequiredOwnerEpoch != 0 || r.RequiredSaveSeq != 0 || holders != 0 || !isInitialPlayerStore(rows) {
			return nil, accountdirectory.ErrConflict
		}
		kind = dspb.AccountRoleFreezeEvidenceKind_ACCOUNT_ROLE_FREEZE_EVIDENCE_KIND_INITIAL_COMMIT
	}
	return &dspb.AccountRoleFreezeEvidence{PlayerId: r.PlayerId, ListingId: r.ListingId, OwnerRevision: r.SellerOwnerRevision,
		OwnerEpoch: owner.ownerEpoch, SaveSeq: owner.saveSeq, PersistedDigest: digestPlayerStoreRows(schema, rows), Kind: kind}, nil
}
