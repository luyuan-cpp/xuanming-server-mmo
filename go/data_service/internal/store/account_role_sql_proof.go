package store

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"hash"

	dbpb "proto/common/database"
	dspb "proto/data_service"
	"shared/accountdirectory"
	"shared/placement"
)

// SQLRoleFreezeVerifier 只使用同一个 TiDB 权威池/事务。配置白名单不能改写权威落点，
// 仅限制允许跨 schema 强读的 storage_id；缺权限、缺行、旧围栏一律拒绝。
type SQLRoleFreezeVerifier struct {
	allowed map[uint32]bool
}

func NewSQLRoleFreezeVerifier(storageIDs []uint32) (*SQLRoleFreezeVerifier, error) {
	if len(storageIDs) == 0 || len(storageIDs) > 1024 {
		return nil, accountdirectory.ErrInvalid
	}
	v := &SQLRoleFreezeVerifier{allowed: make(map[uint32]bool, len(storageIDs))}
	for _, id := range storageIDs {
		if _, ok := placement.StoreDBName(id); !ok || v.allowed[id] {
			return nil, accountdirectory.ErrInvalid
		}
		v.allowed[id] = true
	}
	return v, nil
}

func (v *SQLRoleFreezeVerifier) VerifyFrozen(ctx context.Context, tx *sql.Tx, r *dbpb.AccountRoleTrade) (*dspb.AccountRoleFreezeEvidence, error) {
	if v == nil || tx == nil || r == nil || !accountdirectory.ValidSaveFloor(r.RequiredOwnerEpoch, r.RequiredSaveSeq) {
		return nil, accountdirectory.ErrUnavailable
	}
	holders, err := readRoleTradeHolders(ctx, tx, r.ListingId)
	if err != nil {
		return nil, err
	}
	for _, h := range holders {
		if !h.Drained || h.Seal.PlayerId != r.PlayerId || h.Seal.OwnerRevision != r.SellerOwnerRevision {
			return nil, accountdirectory.ErrConflict
		}
	}
	// 路由锁先于两张 zone 行，阻止读证据期间迁移切库。
	var home, storage, version sql.NullInt64
	var frozen sql.NullString
	var rev uint64
	err = tx.QueryRowContext(ctx, "SELECT home_zone,storage_id,placement_version,frozen_run_id,rev FROM player_route WHERE route_shard=? AND player_id=? FOR UPDATE", placement.RouteRowShard(r.PlayerId), r.PlayerId).Scan(&home, &storage, &version, &frozen, &rev)
	if err != nil {
		return nil, fmt.Errorf("角色存盘证据缺权威落点: %w", err)
	}
	if !home.Valid || home.Int64 < 1 || home.Int64 > int64(placement.MaxZoneStorageID) || rev == 0 || frozen.Valid {
		return nil, accountdirectory.ErrConflict
	}
	// 建角恒写落点,和 go/db 一样不回落 home_zone:猜一个库做证明,比拒绝更糟。
	if !storage.Valid || storage.Int64 < 1 || storage.Int64 > int64(^uint32(0)) || !version.Valid || version.Int64 < 1 {
		return nil, accountdirectory.ErrConflict
	}
	storageID := uint32(storage.Int64)
	if !v.allowed[storageID] {
		return nil, accountdirectory.ErrUnavailable
	}
	schema, ok := placement.StoreDBName(storageID)
	if !ok {
		return nil, accountdirectory.ErrUnavailable
	}
	return v.verifyFrozenPlayerStore(ctx, tx, r, len(holders), schema)
}

func writeProofPart(h hash.Hash, part []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(part)))
	_, _ = h.Write(size[:])
	_, _ = h.Write(part)
}
