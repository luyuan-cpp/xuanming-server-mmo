package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
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

// readPlayerStoreProofRows 锁住并读出这名玩家的所有行,写权行在最前。读完就关游标:同一事务后面还要发语句。
func readPlayerStoreProofRows(ctx context.Context, tx *sql.Tx, schema string, player uint64) ([]playerStoreProofRow, error) {
	rows, err := tx.QueryContext(ctx, "SELECT kind,owner_epoch,save_seq,schema_gen,released,holder_instance,payload FROM `"+schema+"`.`player_store` WHERE player_id=? ORDER BY kind FOR UPDATE", player)
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

func (v *SQLRoleFreezeVerifier) verifyFrozenPlayerStore(ctx context.Context, tx *sql.Tx, r *dbpb.AccountRoleTrade, holders int, schema string) (*dspb.AccountRoleFreezeEvidence, error) {
	rows, err := readPlayerStoreProofRows(ctx, tx, schema, r.PlayerId)
	if err != nil {
		return nil, err
	}
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
		var marker, revision uint64
		if err := tx.QueryRowContext(ctx, "SELECT initial_player_commit,owner_revision FROM account_role WHERE player_id=? FOR UPDATE", r.PlayerId).Scan(&marker, &revision); err != nil {
			return nil, err
		}
		if marker != 1 || revision != 1 || r.SellerOwnerRevision != revision {
			return nil, accountdirectory.ErrConflict
		}
		kind = dspb.AccountRoleFreezeEvidenceKind_ACCOUNT_ROLE_FREEZE_EVIDENCE_KIND_INITIAL_COMMIT
	}
	return &dspb.AccountRoleFreezeEvidence{PlayerId: r.PlayerId, ListingId: r.ListingId, OwnerRevision: r.SellerOwnerRevision,
		OwnerEpoch: owner.ownerEpoch, SaveSeq: owner.saveSeq, PersistedDigest: digestPlayerStoreRows(schema, rows), Kind: kind}, nil
}
