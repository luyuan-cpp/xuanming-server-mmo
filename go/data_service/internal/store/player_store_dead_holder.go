package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"

	dspb "proto/data_service"
	"shared/accountdirectory"
)

// LeaveScene 判死释放的两个存档库步骤(docs/design/player-save-direct-commit.md §5.5)。顺序与交易判死
// (account_role_dead_holder.go fenceDeadTradeHolders)同一口径,中间那一步在 scene_manager:
//
//  1. ReadPlayerStoreHolder:普通读写权行,记下 (E, H, claim_seq)。
//  2. scene_manager 向 etcd 做线性一致读,复核 H 仍不在册(logic.releaseDeadPlayerStoreHolder)。
//  3. ReleaseDeadPlayerStoreHolderIfUnchanged:存档库事务里写权行还是第 1 步读到的那一份,才抬代次、清持有者、置 released=1。
//
// 第 1 步排在第 2 步前面是第 3 步比较并写的依据:第 2 步看到 H 不在册,H 之后再认领(只有重新注册之后才进得了场)
// 一定晚于第 1 步,claim_seq 必然变了,第 3 步什么都不做。玩家同节点重登时 (E, H) 写回原值,只有 claim_seq 变,所以三列一起比。
// 只比代次的话,判死之后持有进程以同一 UUID 重注册、玩家重登认领,挂住的释放照样执行,在线玩家被踢。

// ReadPlayerStoreHolder 普通读写权行判持有者用的三列。没有路由或写权行回 (nil, nil):没有东西要释放,
// 与 ReleaseDeadPlayerStoreHolder 同一口径;落点不在白名单回 ErrUnavailable。
// 不占展示读的名额(readSlots):那个名额本来就是为了不让展示读挤掉判死。
func (s *AccountDirectoryStore) ReadPlayerStoreHolder(ctx context.Context, playerID uint64) (*dspb.PlayerStoreHolder, error) {
	if s == nil || playerID == 0 {
		return nil, accountdirectory.ErrInvalid
	}
	storage, err := s.playerStorage(ctx, playerID)
	if errors.Is(err, accountdirectory.ErrMissing) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	seen, err := s.steps.readStoreOwner(ctx, storage, playerID)
	if err != nil || seen == nil {
		return nil, err
	}
	return &dspb.PlayerStoreHolder{OwnerEpoch: seen.ownerEpoch, HolderInstance: seen.holder, ClaimSeq: seen.claimSeq}, nil
}

// ReleaseDeadPlayerStoreHolderIfUnchanged 是判死释放的提交点:存档库一个事务里锁写权行,按 decideDeadHolderRelease 判定,
// 要释放就抬到 ownerEpoch+1、清持有者、置 released=1。ownerEpoch 是判死时 location 这一代,seen 是 ReadPlayerStoreHolder 读到的。
//
// 读之后有人认领过回 accountdirectory.ErrConflict,什么都不写;调用方保留 location,下一次重读、重新复核。
// 没有路由或写权行回 nil:没有东西要释放。只增、幂等:重试时库里已是 ownerEpoch+1,判为「已比它新」。
func (s *AccountDirectoryStore) ReleaseDeadPlayerStoreHolderIfUnchanged(ctx context.Context, playerID, ownerEpoch uint64, seen *dspb.PlayerStoreHolder) error {
	if s == nil || playerID == 0 || ownerEpoch == 0 || ownerEpoch == ^uint64(0) || seen == nil {
		return accountdirectory.ErrInvalid
	}
	storage, err := s.playerStorage(ctx, playerID)
	if errors.Is(err, accountdirectory.ErrMissing) {
		return nil
	}
	if err != nil {
		return err
	}
	db, _, err := s.stores.DB(ctx, storage)
	if err != nil {
		return err
	}
	expected := lockedPlayerStoreOwner{ownerEpoch: seen.GetOwnerEpoch(), holder: seen.GetHolderInstance(), claimSeq: seen.GetClaimSeq()}
	return shardTx(ctx, db, func(tx *sql.Tx) error {
		owner, err := lockStoreOwner(ctx, tx, playerID)
		if err != nil || owner == nil {
			return err
		}
		release, err := decideDeadHolderRelease(*owner, ownerEpoch, expected)
		if err != nil || !release {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE player_store SET owner_epoch=?,holder_instance=?,released=1 WHERE player_id=? AND kind=?",
			ownerEpoch+1, []byte{}, playerID, playerStoreOwnerKind); err != nil {
			return fmt.Errorf("释放玩家写权行: %w", err)
		}
		return nil
	})
}

// decideDeadHolderRelease 是判死释放提交点的纯判定。current 是事务里锁到的写权行,locationEpoch 是判死时 location 这一代,
// seen 是 scene_manager 复核持有实例之前读到的那一份。
//   - 代次已比 locationEpoch 新:新的持有者认领过了(或上一次释放已经做完),不动。排在比较之前:这时释放本来就不该发生,
//     回冲突只会让调用方白白重试。
//   - 写权行不是 seen:读之后有人认领过,复核的结论说明不了现任,回 ErrConflict。
//   - 其余:释放。
func decideDeadHolderRelease(current lockedPlayerStoreOwner, locationEpoch uint64, seen lockedPlayerStoreOwner) (bool, error) {
	if current.ownerEpoch > locationEpoch {
		return false, nil
	}
	if current.ownerEpoch != seen.ownerEpoch || !bytes.Equal(current.holder, seen.holder) || current.claimSeq != seen.claimSeq {
		return false, accountdirectory.ErrConflict
	}
	return true, nil
}
