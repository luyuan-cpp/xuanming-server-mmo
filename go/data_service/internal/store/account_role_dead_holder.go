package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"math"

	dbpb "proto/common/database"
	dspb "proto/data_service"
	"shared/accountdirectory"
)

// deadHolderAckDigest 是判死的持有者在 ack_digest 里的标记。真实 ACK 的摘要是请求的 SHA-256,不会撞上它。
var deadHolderAckDigest = func() []byte {
	sum := sha256.Sum256([]byte("account-role-dead-holder-v1"))
	return sum[:]
}()

// lockedPlayerStoreOwner 是写权行里判断持有者用的两列。released 不算:释放不是终态,同实例重连后接着存。
type lockedPlayerStoreOwner struct {
	ownerEpoch uint64
	holder     []byte
}

// lockPlayerStoreOwner 锁住写权行。返回 (schema, nil, nil) 表示没有写权行;schema 为空表示路由查不到或落点不在白名单。
func (s *AccountDirectoryStore) lockPlayerStoreOwner(ctx context.Context, tx *sql.Tx, playerID uint64) (string, *lockedPlayerStoreOwner, error) {
	schema, err := s.playerStoreSchema(ctx, tx, playerID)
	if err != nil {
		if errors.Is(err, accountdirectory.ErrMissing) || errors.Is(err, accountdirectory.ErrUnavailable) {
			return "", nil, nil
		}
		return "", nil, err
	}
	var row lockedPlayerStoreOwner
	err = tx.QueryRowContext(ctx, "SELECT owner_epoch,holder_instance FROM `"+schema+"`.`player_store` WHERE player_id=? AND kind=? FOR UPDATE",
		playerID, playerStoreOwnerKind).Scan(&row.ownerEpoch, &row.holder)
	if errors.Is(err, sql.ErrNoRows) {
		return schema, nil, nil
	}
	if err != nil {
		return "", nil, fmt.Errorf("锁玩家写权行: %w", err)
	}
	return schema, &row, nil
}

// fenceDeadTradeHolders 把已经写不了存档的持有者判为排空。调用方已持账号锁和角色行锁,trade_listing_id 已是这笔交易,
// 所以 Register 不会再放进新的入场。
//
// 为什么不等 ACK:scene 实例崩溃之后永远不会发 AckDrain,它的登记也永远不会关闭。不处理的话这笔交易卡在 FREEZING,
// 取消也卡在 CANCEL_PENDING,这个角色之后每一笔交易都会再次卡住。
//
// 判据:持有者 I 能提交存档的前提是写权行的 holder_instance 是它(go/db 否则回 NOT_HOLDER)。释放不是终态:断线后 30 秒内
// 重连会复用实体接着存,所以 released=1 的现任也算现任。只要有一个未排空的持有者还是现任,就什么都不做,它可能活着、内存里
// 还有没落盘的进度,要等真实的 ACK。
//
// 否则这些持有者都写不了存档,只剩「认领在途」一种可能。认领带的是登记时的代次(RoleAdmissionIdentity.owner_epoch),那是铸造时的
// 墙钟,通常远大于库里的代次,所以只抬一次挡不住:要抬到所有待判持有者登记的代次之上,go/db 才会判 STALE。
// 持有者拿不到更高的代次:新的入场要先登记,而登记已经被 trade_listing_id 挡住。下一个合法持有者的代次由 scene_manager 铸造,
// 不小于这里的值(铸造取「观察值 + 1」和墙钟的较大者),所以不会被挡在外面。
//
// 现任崩溃、scene_manager 又没有判死它(没有 location,或者释放之后、Close 之前崩溃)时不会走到这里,运维用 cmd/admission_recover 摘掉它。
func fenceDeadTradeHolders(ctx context.Context, tx *sql.Tx, directory *AccountDirectoryStore, r *dbpb.AccountRoleTrade) error {
	holders, err := readRoleTradeHolders(ctx, tx, r.ListingId)
	if err != nil {
		return err
	}
	var pending []*dspb.AccountRoleTradeHolder
	for _, h := range holders {
		if !h.Drained {
			pending = append(pending, h)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	schema, owner, err := directory.lockPlayerStoreOwner(ctx, tx, r.PlayerId)
	if err != nil || owner == nil {
		return err
	}
	floor := owner.ownerEpoch
	for _, h := range pending {
		if bytes.Equal(owner.holder, []byte(h.Seal.SceneInstanceId)) {
			return nil
		}
		admission, err := readAdmission(ctx, tx, r.PlayerId, h.Seal.SceneInstanceId)
		if err != nil {
			return err
		}
		identity, err := admissionIdentity(admission)
		if err != nil {
			return err
		}
		if identity.OwnerEpoch > floor {
			floor = identity.OwnerEpoch
		}
	}
	if floor == math.MaxUint64 {
		return accountdirectory.ErrConflict
	}
	if _, err := tx.ExecContext(ctx, "UPDATE `"+schema+"`.`player_store` SET owner_epoch=?,holder_instance=?,released=1 WHERE player_id=? AND kind=?",
		floor+1, []byte{}, r.PlayerId, playerStoreOwnerKind); err != nil {
		return fmt.Errorf("挡住已死的持有者: %w", err)
	}
	for _, h := range pending {
		instance := []byte(h.Seal.SceneInstanceId)
		if _, err := tx.ExecContext(ctx, "UPDATE account_role_trade_holder SET drained=1,ack_digest=?,updated_ms="+dbNowMsSQL+" WHERE listing_id=? AND scene_instance_id=?",
			deadHolderAckDigest, r.ListingId, instance); err != nil {
			return err
		}
		// 登记也要关:不关的话下一笔交易又会把这个死实例拍进持有者。close_digest 记判死标记,迟到的真实 Close 按成功重放。
		if _, err := tx.ExecContext(ctx, "UPDATE account_role_admission SET closed_through=highest_generation,close_digest=?,updated_ms="+dbNowMsSQL+" WHERE player_id=? AND scene_instance_id=?",
			deadHolderAckDigest, r.PlayerId, instance); err != nil {
			return err
		}
	}
	return nil
}

// ReleaseHolderInstance 把已确认死掉的实例从写权行上摘掉,给 cmd/admission_recover 用。
// 只有写权行的现任确实是 instance 才动(已释放也算);否则回 ErrConflict,说明有人已经认领了。apply 为 false 时只核对。
func (s *AccountDirectoryStore) ReleaseHolderInstance(ctx context.Context, playerID uint64, instance string, apply bool) (epochBefore uint64, err error) {
	if s == nil || playerID == 0 || instance == "" {
		return 0, accountdirectory.ErrInvalid
	}
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		schema, owner, err := s.lockPlayerStoreOwner(ctx, tx, playerID)
		if err != nil {
			return err
		}
		if owner == nil {
			return accountdirectory.ErrMissing
		}
		if !bytes.Equal(owner.holder, []byte(instance)) || owner.ownerEpoch == math.MaxUint64 {
			return accountdirectory.ErrConflict
		}
		epochBefore = owner.ownerEpoch
		if !apply {
			return nil
		}
		_, err = tx.ExecContext(ctx, "UPDATE `"+schema+"`.`player_store` SET owner_epoch=?,holder_instance=?,released=1 WHERE player_id=? AND kind=?",
			owner.ownerEpoch+1, []byte{}, playerID, playerStoreOwnerKind)
		return err
	})
	return epochBefore, err
}
