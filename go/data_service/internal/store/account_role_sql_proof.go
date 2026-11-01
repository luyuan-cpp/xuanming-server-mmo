package store

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash"

	dbpb "proto/common/database"
	dspb "proto/data_service"
	"shared/accountdirectory"
	"shared/placement"
)

// SQLRoleFreezeVerifier 经存档库连接池读证据。配置白名单不能改写权威落点,仅限制允许读的 storage_id;缺权限、缺行、旧围栏一律拒绝。
//
// 证据在目录事务外读(规格 §6):存档库和目录可能不在一个集群。调用方先核过这一轮的持有者全部排空,之后存档不再变,
// 判死抬代次只发生在读证据之前,所以这份证据到目录事务提交时仍然成立。目录事务再核收据、下限和初始提交标记。
type SQLRoleFreezeVerifier struct {
	directory *AccountDirectoryStore
	allowed   map[uint32]bool
}

func NewSQLRoleFreezeVerifier(directory *AccountDirectoryStore, storageIDs []uint32) (*SQLRoleFreezeVerifier, error) {
	if directory == nil || len(storageIDs) == 0 || len(storageIDs) > 1024 {
		return nil, accountdirectory.ErrInvalid
	}
	v := &SQLRoleFreezeVerifier{directory: directory, allowed: make(map[uint32]bool, len(storageIDs))}
	for _, id := range storageIDs {
		if _, ok := placement.StoreDBName(id); !ok || v.allowed[id] {
			return nil, accountdirectory.ErrInvalid
		}
		v.allowed[id] = true
	}
	return v, nil
}

// ReadFrozenEvidence 读证据候选。holders 是这一轮固定的持有者数。
func (v *SQLRoleFreezeVerifier) ReadFrozenEvidence(ctx context.Context, r *dbpb.AccountRoleTrade, holders int) (*dspb.AccountRoleFreezeEvidence, error) {
	if v == nil || r == nil || !accountdirectory.ValidSaveFloor(r.RequiredOwnerEpoch, r.RequiredSaveSeq) {
		return nil, accountdirectory.ErrUnavailable
	}
	route, found, err := v.directory.steps.readRoute(ctx, r.PlayerId)
	if err != nil {
		return nil, fmt.Errorf("角色存盘证据缺权威落点: %w", err)
	}
	if !found {
		return nil, fmt.Errorf("角色存盘证据缺权威落点: %w", accountdirectory.ErrMissing)
	}
	// 建角恒写落点,和 go/db 一样不回落 home_zone:猜一个库做证明,比拒绝更糟。
	if route.HomeZone == 0 || route.HomeZone > placement.MaxZoneStorageID || route.Rev == 0 || !route.HasPlacement() || route.PlacementVersion == 0 {
		return nil, accountdirectory.ErrConflict
	}
	if !v.allowed[route.StorageID] {
		return nil, accountdirectory.ErrUnavailable
	}
	db, schema, err := v.directory.stores.DB(ctx, route.StorageID)
	if err != nil {
		return nil, err
	}
	rows, err := readPlayerStoreProofRows(ctx, db, r.PlayerId, false)
	if err != nil {
		return nil, err
	}
	return decideFrozenEvidence(r, holders, schema, rows)
}

func writeProofPart(h hash.Hash, part []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(part)))
	_, _ = h.Write(size[:])
	_, _ = h.Write(part)
}
