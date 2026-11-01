package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"math"

	dspb "proto/data_service"
	"shared/accountdirectory"
	"shared/authoritysql"
)

// deadHolderAckDigest 是判死的持有者在 ack_digest 里的标记。真实 ACK 的摘要是请求的 SHA-256,不会撞上它。
var deadHolderAckDigest = func() []byte {
	sum := sha256.Sum256([]byte("account-role-dead-holder-v1"))
	return sum[:]
}()

// lockedPlayerStoreOwner 是写权行里判断持有者用的列。released 不算:释放不是终态,同实例重连后接着存。
type lockedPlayerStoreOwner struct {
	ownerEpoch uint64
	holder     []byte
	// claimSeq 每次认领加 1。同实例同代次重新认领时前两列写回原值,只有它变,判死比较并写时靠它比得出来。
	claimSeq uint64
}

// deadHolderPlan 是目录事务读到的一轮判死输入。
type deadHolderPlan struct {
	listing, player uint64
	// version 是读到时的护栏版本。记 holder 时版本变了,这一轮的结论不算数。
	version uint64
	// pending 是还没排空的持有者,maxEpoch 是它们登记的最大代次。
	pending  []string
	maxEpoch uint64
}

// deadHolderVerdict 是一轮判死的结论。
type deadHolderVerdict struct {
	// alive:有一个待判的持有者是写权行的现任,可能活着、内存里还有没落盘的进度,要等真实的 ACK。
	alive bool
	// raise:要把代次抬到 epoch。false 表示写权行已经挡住了全部待判持有者(上一轮抬过之后崩了)。
	raise bool
	epoch uint64
}

// decideDeadHolders 是纯判定。只增:epoch 恒大于写权行当前代次,已经挡住时不再抬,所以中途崩溃重跑不会把代次一直往上抬。
func decideDeadHolders(seen lockedPlayerStoreOwner, plan *deadHolderPlan) (deadHolderVerdict, error) {
	for _, instance := range plan.pending {
		if bytes.Equal(seen.holder, []byte(instance)) {
			return deadHolderVerdict{alive: true}, nil
		}
	}
	if len(seen.holder) == 0 && seen.ownerEpoch > plan.maxEpoch {
		return deadHolderVerdict{}, nil
	}
	floor := max(seen.ownerEpoch, plan.maxEpoch)
	if floor == math.MaxUint64 {
		return deadHolderVerdict{}, accountdirectory.ErrConflict
	}
	return deadHolderVerdict{raise: true, epoch: floor + 1}, nil
}

// fenceDeadTradeHolders 把已经写不了存档的持有者判为排空(规格 §6)。存档库和目录不在一个事务里,顺序要严格:
//
//  0. 目录普通读:没有未排空的持有者就到此为止,不碰存档库。PREPARING 还没有 holder,所以存档库故障时取消照样能完成。
//     这一读过期了也没事:漏判一次只是持有者接着等 ACK,下一次重放再判。
//  1. 存档库普通读写权行,记下 (E, H, claim_seq)。
//  2. 目录事务:trade 还开着、角色还锁在这个 listing 上,取待判持有者和它们登记的代次。令牌挡住了新的 Register,所以待判集合不会再变。
//  3. 判定。
//  4. 存档库事务:写权行还是第 1 步看到的 (E, H, claim_seq) 才抬代次。先抬代次,持有者才真的写不了存档。
//  5. 目录事务:角色还锁在这个 listing 上、版本没变才记 holder 排空、关登记。
//
// 第 1 步排在第 2 步前面,这是第 4 步比较并写的依据:第 2 步看到角色还锁着,说明第 1 步读到的写权行还在锁定期内,不是解锁之后新持有者的。
// 之后取消赢了、新的持有者认领了,写权行就变了,第 4 步什么都不做。所以一条挂住的判死不会踢掉解锁之后的新持有者。
// 玩家同节点重登时 (E, H) 写回原值,只有 claim_seq 变,所以三列一起比。
//
// 第 4 步之后崩溃重跑:第 3 步看到写权行已经挡住全部待判持有者,不再抬,接着走第 5 步。第 5 步之前崩溃不留下「记了排空但没抬代次」的状态。
//
// 为什么不等 ACK:scene 实例崩溃之后永远不会发 AckDrain,它的登记也永远不会关闭。不处理的话这笔交易卡在 FREEZING,
// 取消也卡在 CANCEL_PENDING,这个角色之后每一笔交易都会再次卡住。
//
// 判据:持有者 I 能提交存档的前提是写权行的 holder_instance 是它(go/db 否则回 NOT_HOLDER)。释放不是终态:断线后 30 秒内
// 重连会复用实体接着存,所以 released=1 的现任也算现任。只要有一个未排空的持有者还是现任,就什么都不做。
//
// 否则这些持有者都写不了存档,只剩「认领在途」一种可能。认领带的是登记时的代次(RoleAdmissionIdentity.owner_epoch),那是铸造时的
// 墙钟,通常远大于库里的代次,所以只抬一次挡不住:要抬到所有待判持有者登记的代次之上,go/db 才会判 STALE。
// 持有者拿不到更高的代次:新的入场要先登记,而登记已经被 trade_listing_id 挡住。下一个合法持有者的代次由 scene_manager 铸造,
// 不小于这里的值(铸造取「观察值 + 1」和墙钟的较大者),所以不会被挡在外面。
//
// 现任崩溃、scene_manager 又没有判死它(没有 location,或者释放之后、Close 之前崩溃)时不会走到这里,运维用 cmd/admission_recover 摘掉它。
//
// 第 4 步之前都只读,读失败走 readFailed。第 4 步、第 5 步是写,提交失败照旧算结局未知。
func (s *AccountDirectoryStore) fenceDeadTradeHolders(ctx context.Context, listing, player uint64) error {
	pending, err := s.steps.hasPendingHolders(ctx, player, listing)
	if err != nil {
		return readFailed(err)
	}
	if !pending {
		return nil
	}
	storage, err := s.playerStorage(ctx, player)
	if errors.Is(err, accountdirectory.ErrMissing) || errors.Is(err, accountdirectory.ErrUnavailable) {
		// 路由查不到或落点不在白名单:判不了,持有者照旧等 ACK。
		return nil
	}
	if err != nil {
		return readFailed(err)
	}
	seen, err := s.steps.readStoreOwner(ctx, storage, player)
	if err != nil {
		return readFailed(err)
	}
	if seen == nil {
		return nil
	}
	plan, err := s.steps.readDeadHolderPlan(ctx, listing, player)
	if err != nil {
		return readFailed(err)
	}
	if plan == nil || len(plan.pending) == 0 {
		return nil
	}
	verdict, err := decideDeadHolders(*seen, plan)
	if err != nil || verdict.alive {
		return err
	}
	if verdict.raise {
		raised, err := s.steps.raiseStoreEpoch(ctx, storage, player, *seen, verdict.epoch)
		if err != nil {
			return err
		}
		if !raised {
			// 写权行变了:有人认领了。这一轮的结论不算数,下一次重放再判。
			return nil
		}
	}
	// 可能已经抬过代次了,这里失败不能回确定性的拒绝。下一次重放接着记。
	return notRejection(s.steps.markHoldersDrained(ctx, plan))
}

// hasPendingHolders 普通读这个 listing 有没有未排空的持有者。
func (st *sqlDirectorySteps) hasPendingHolders(ctx context.Context, player, listing uint64) (bool, error) {
	var found uint32
	err := st.shards.forPlayer(player).QueryRowContext(ctx, "SELECT 1 FROM account_role_trade_holder WHERE listing_id=? AND drained=0 LIMIT 1", listing).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// lockStoreOwner 在存档库事务里锁住写权行。没有写权行回 nil。
func lockStoreOwner(ctx context.Context, tx *sql.Tx, player uint64) (*lockedPlayerStoreOwner, error) {
	var row lockedPlayerStoreOwner
	err := tx.QueryRowContext(ctx, "SELECT owner_epoch,holder_instance,claim_seq FROM player_store WHERE player_id=? AND kind=? FOR UPDATE", player, playerStoreOwnerKind).Scan(&row.ownerEpoch, &row.holder, &row.claimSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("锁玩家写权行: %w", err)
	}
	return &row, nil
}

// readStoreOwner 普通读写权行。没有写权行回 nil。
func (st *sqlDirectorySteps) readStoreOwner(ctx context.Context, storage uint32, player uint64) (*lockedPlayerStoreOwner, error) {
	db, _, err := st.directory.stores.DB(ctx, storage)
	if err != nil {
		return nil, err
	}
	var row lockedPlayerStoreOwner
	err = db.QueryRowContext(ctx, "SELECT owner_epoch,holder_instance,claim_seq FROM player_store WHERE player_id=? AND kind=?", player, playerStoreOwnerKind).Scan(&row.ownerEpoch, &row.holder, &row.claimSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读玩家写权行: %w", err)
	}
	return &row, nil
}

// raiseStoreEpoch 在存档库事务里比较并写:写权行还是 seen 才抬。回是否抬了。
func (st *sqlDirectorySteps) raiseStoreEpoch(ctx context.Context, storage uint32, player uint64, seen lockedPlayerStoreOwner, epoch uint64) (bool, error) {
	db, _, err := st.directory.stores.DB(ctx, storage)
	if err != nil {
		return false, err
	}
	raised := false
	err = shardTx(ctx, db, func(tx *sql.Tx) error {
		current, err := lockStoreOwner(ctx, tx, player)
		if err != nil || current == nil {
			return err
		}
		if current.ownerEpoch != seen.ownerEpoch || !bytes.Equal(current.holder, seen.holder) || current.claimSeq != seen.claimSeq {
			return nil
		}
		if _, err := tx.ExecContext(ctx, "UPDATE player_store SET owner_epoch=?,holder_instance=?,released=1 WHERE player_id=? AND kind=?", epoch, []byte{}, player, playerStoreOwnerKind); err != nil {
			return fmt.Errorf("挡住已死的持有者: %w", err)
		}
		raised = true
		return nil
	})
	return raised, err
}

// readDeadHolderPlan 读一轮判死的输入。trade 不是开着的、角色没锁在这个 listing 上都回 nil。
func (st *sqlDirectorySteps) readDeadHolderPlan(ctx context.Context, listing, player uint64) (*deadHolderPlan, error) {
	var plan *deadHolderPlan
	err := shardTx(ctx, st.shards.forPlayer(player), func(tx *sql.Tx) error {
		r, err := readRoleTrade(ctx, tx, listing)
		if errors.Is(err, accountdirectory.ErrMissing) {
			return nil
		}
		if err != nil {
			return err
		}
		if r.PlayerId != player || (r.State != uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FREEZING) && r.State != uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCEL_PENDING)) {
			return nil
		}
		owned, version, err := lockFencedOwnership(ctx, tx, player)
		if err != nil {
			return err
		}
		if owned.TradeListingId != listing {
			return nil
		}
		holders, err := readRoleTradeHolders(ctx, tx, listing)
		if err != nil {
			return err
		}
		next := &deadHolderPlan{listing: listing, player: player, version: version}
		for _, h := range holders {
			if h.Drained {
				continue
			}
			admission, err := readAdmission(ctx, tx, player, h.Seal.SceneInstanceId)
			if err != nil {
				return err
			}
			identity, err := admissionIdentity(admission)
			if err != nil {
				return err
			}
			next.pending = append(next.pending, h.Seal.SceneInstanceId)
			next.maxEpoch = max(next.maxEpoch, identity.OwnerEpoch)
		}
		plan = next
		return nil
	})
	if err != nil {
		return nil, err
	}
	return plan, nil
}

// markHoldersDrained 把这一轮判死的持有者记成排空、关登记。存档库已经抬过代次,这里只记结论。
// 真实 ACK 抢先的持有者不动它。
func (st *sqlDirectorySteps) markHoldersDrained(ctx context.Context, plan *deadHolderPlan) error {
	return shardTx(ctx, st.shards.forPlayer(plan.player), func(tx *sql.Tx) error {
		if _, err := readRoleTrade(ctx, tx, plan.listing); err != nil {
			return err
		}
		owned, version, err := lockFencedOwnership(ctx, tx, plan.player)
		if err != nil {
			return err
		}
		if owned.TradeListingId != plan.listing || version != plan.version {
			return nil
		}
		holders, err := readRoleTradeHolders(ctx, tx, plan.listing)
		if err != nil {
			return err
		}
		drained := make(map[string]bool, len(holders))
		for _, h := range holders {
			drained[h.Seal.SceneInstanceId] = h.Drained
		}
		for _, name := range plan.pending {
			if already, ok := drained[name]; !ok || already {
				continue
			}
			instance := []byte(name)
			if _, err := tx.ExecContext(ctx, "UPDATE account_role_trade_holder SET drained=1,ack_digest=?,updated_ms="+dbNowMsSQL+" WHERE listing_id=? AND scene_instance_id=?",
				deadHolderAckDigest, plan.listing, instance); err != nil {
				return err
			}
			// 登记也要关:不关的话下一笔交易又会把这个死实例拍进持有者。close_digest 记判死标记,迟到的真实 Close 按成功重放。
			if _, err := tx.ExecContext(ctx, "UPDATE account_role_admission SET closed_through=highest_generation,close_digest=?,updated_ms="+dbNowMsSQL+" WHERE player_id=? AND scene_instance_id=?",
				deadHolderAckDigest, plan.player, instance); err != nil {
				return err
			}
		}
		return nil
	})
}

// requireGateOpenTx 锁住本库的闸门行,核它开着;冻结回 authoritysql.ErrWriterFrozen。
// 维护身份不过写者闸门,直接写库的工具要自己看。行锁持到提交:FreezeWriters 也要这把锁,所以冻结一定排在这笔之后,这笔的写跟着复制过去。
// 判据同 tools/merge_zone 的 routeStore.Locked。
func requireGateOpenTx(ctx context.Context, tx *sql.Tx) error {
	var writable uint64
	if err := tx.QueryRowContext(ctx, "SELECT writable FROM route_meta WHERE id=1 FOR UPDATE").Scan(&writable); err != nil {
		return fmt.Errorf("锁闸门行: %w", err)
	}
	if writable != 1 {
		return authoritysql.ErrWriterFrozen
	}
	return nil
}

// ReleaseHolderInstance 把已确认死掉的实例从写权行上摘掉,给 cmd/admission_recover 用。
// 只有写权行的现任确实是 instance 才动(已释放也算);否则回 ErrConflict,说明有人已经认领了。apply 为 false 时只核对。
// 搬库或维护把这个库的闸门冻结时回 authoritysql.ErrWriterFrozen,什么都不写:冻结的库不该再变。
func (s *AccountDirectoryStore) ReleaseHolderInstance(ctx context.Context, playerID uint64, instance string, apply bool) (epochBefore uint64, err error) {
	if s == nil || playerID == 0 || instance == "" {
		return 0, accountdirectory.ErrInvalid
	}
	storage, err := s.playerStorage(ctx, playerID)
	if err != nil {
		return 0, err
	}
	db, _, err := s.stores.DB(ctx, storage)
	if err != nil {
		return 0, err
	}
	err = shardTx(ctx, db, func(tx *sql.Tx) error {
		if err := requireGateOpenTx(ctx, tx); err != nil {
			return err
		}
		owner, err := lockStoreOwner(ctx, tx, playerID)
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
		_, err = tx.ExecContext(ctx, "UPDATE player_store SET owner_epoch=?,holder_instance=?,released=1 WHERE player_id=? AND kind=?",
			owner.ownerEpoch+1, []byte{}, playerID, playerStoreOwnerKind)
		return err
	})
	return epochBefore, err
}
