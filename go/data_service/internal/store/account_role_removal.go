package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"sort"
	"time"

	dspb "proto/data_service"
	rfpb "proto/role_fence"
	"shared/accountdirectory"
	"shared/rolefence"
)

const AccountRoleRemovalTableName = "account_role_removal"
const accountRoleRemovalDDL = "CREATE TABLE IF NOT EXISTS account_role_removal (player_id BIGINT UNSIGNED NOT NULL COMMENT 'pb:1',account VARBINARY(191) NOT NULL COMMENT 'pb:2',owner_revision BIGINT UNSIGNED NOT NULL COMMENT 'pb:3',started_ms BIGINT UNSIGNED NOT NULL COMMENT 'pb:4',PRIMARY KEY(player_id) /*T![clustered_index] NONCLUSTERED */) ENGINE=InnoDB /*T! SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=4 */"

// Remove 删角,两阶段,准备令牌是 RemovalToken(规格 §3.5)。
//
// 多名角色逐个走,不再是一个事务:中途失败时前面的已经删了,原请求重放幂等。这时不能回确定性的拒绝,一律回 ErrRetryLater(规格 §3.6)。
// 有挂起回 ErrConflict,令牌已清。参与者不可达回 ErrRetryLater,令牌留着,重放或后台 ResumeRemovals 接着做:写过的只有第 1 步,提交已确认,没有结局未知。
// 准备令牌不挡入场:墓碑事务持角色行锁再核一次登记全部关闭,投票期间进了场就放弃,回 ErrConflict。
//
// 墓碑提交之后的收尾(settleRemovedRole):释放这名角色登记的名字,再删小表那一行。收尾失败不改删角的结局(墓碑已提交),
// 小表那一行留着,重放或后台读到墓碑接着收尾(docs/design/guild-phase2/03-names.md §3.4a)。
func (s *AccountDirectoryStore) Remove(ctx context.Context, account string, roles []*dspb.AccountRoleVersion) error {
	if len(roles) == 0 || len(roles) > 100 || !accountdirectory.ValidAccount(account) {
		return accountdirectory.ErrInvalid
	}
	ordered := append([]*dspb.AccountRoleVersion(nil), roles...)
	for _, r := range ordered {
		if r == nil || r.PlayerId == 0 || r.OwnerRevision == 0 {
			return accountdirectory.ErrInvalid
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].PlayerId < ordered[j].PlayerId })
	for i, r := range ordered {
		if i > 0 && r.PlayerId == ordered[i-1].PlayerId {
			return accountdirectory.ErrInvalid
		}
	}
	if s.fences == nil {
		return accountdirectory.ErrRetryLater
	}
	for i, r := range ordered {
		if err := s.removeRole(ctx, account, r.PlayerId, r.OwnerRevision); err != nil {
			if i > 0 {
				return notRejection(err)
			}
			return err
		}
	}
	return nil
}

func (s *AccountDirectoryStore) removeRole(ctx context.Context, account string, player, revision uint64) error {
	state, err := s.beginRemoval(ctx, account, player, revision)
	if err != nil || state == nil {
		return err
	}
	switch s.fences.Prepare(ctx, state) {
	case rolefence.PrepareClean:
		err := s.finishRemoval(ctx, state)
		if errors.Is(err, accountdirectory.ErrConflict) {
			// 投票期间有实例进场了。令牌不能留着,否则重放和后台永远停在这里。
			return s.abortRemoval(ctx, state)
		}
		if err != nil {
			return err
		}
		// 墓碑已经提交,ref 现在能证明陈旧。失败无害:读路径会过滤,上限判定和后台都会再清。
		s.reapOwnRef(ctx, account, player)
		s.settleRemovedRole(ctx, player)
		return nil
	case rolefence.PrepareBlocked:
		return s.abortRemoval(ctx, state)
	default:
		return accountdirectory.ErrRetryLater
	}
}

// beginRemoval 是第 1 步,在玩家分片的一个事务里:核归属和入场登记,写准备令牌和小表。回 nil state 表示角色已经删了,幂等成功。
// 准备令牌不挡 RegisterRoleAdmission,开放登记到墓碑事务里再判一次。不锁账号 guard:它在账号分片,归属只看角色行。
func (s *AccountDirectoryStore) beginRemoval(ctx context.Context, account string, player, revision uint64) (*rfpb.RoleFenceState, error) {
	var state *rfpb.RoleFenceState
	var gone bool
	err := s.playerTx(ctx, player, func(tx *sql.Tx) error {
		old, version, err := lockFencedOwnership(ctx, tx, player)
		if errors.Is(err, accountdirectory.ErrMissing) {
			return nil
		}
		if err != nil {
			return err
		}
		if old.Account == "" {
			gone = true
			return nil
		}
		if old.Account != account || old.OwnerRevision != revision {
			return accountdirectory.ErrConflict
		}
		if old.OwnerRevision == math.MaxUint64 {
			return errors.New("角色归属修订号耗尽")
		}
		token := rolefence.RemovalToken(player)
		if old.PrepareToken == token {
			// 重放或后台接着做:令牌和小表都在,只再问一遍参与者。
			state = fenceStateOf(player, old, version)
			return nil
		}
		if old.TradeListingId != 0 || old.PrepareToken != 0 {
			return accountdirectory.ErrConflict
		}
		if err := requireClosedAdmissions(ctx, tx, player); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE account_role SET prepare_token=?,fence_version=fence_version+1,updated_ms="+dbNowMsSQL+" WHERE player_id=?", token, player); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO account_role_removal(player_id,account,owner_revision,started_ms) VALUES (?,?,?,"+dbNowMsSQL+")", player, []byte(account), revision); err != nil {
			return err
		}
		old.PrepareToken = token
		state = fenceStateOf(player, old, version+1)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if gone {
		// 重放或后台接着做:上一次写完墓碑之后没来得及清 ref、释放名字、删小表那一行。
		s.reapOwnRef(ctx, account, player)
		s.settleRemovedRole(ctx, player)
	}
	return state, nil
}

// errRemovalDone 表示另一条路径已经写了墓碑,这次删角算成功。只在本文件内流转。
var errRemovalDone = errors.New("角色已删除")

// lockRemoving 锁角色行并复核第 1 步写下的令牌和版本都还在。
func lockRemoving(ctx context.Context, tx *sql.Tx, state *rfpb.RoleFenceState) error {
	old, version, err := lockFencedOwnership(ctx, tx, state.PlayerId)
	if err != nil {
		return err
	}
	switch {
	case old.Account == "":
		return errRemovalDone
	case old.PrepareToken == 0:
		// 另一条路径因为有挂起放弃了。
		return accountdirectory.ErrConflict
	case old.PrepareToken != state.FreezeToken || version != state.Version:
		// Prepare 用的是这个版本。版本变了说明又开始了一轮,这一轮的结论不算数。事务随之回滚,这一步确定没写。
		return accountdirectory.ErrRetryLater
	}
	return nil
}

// finishRemoval 全部参与者通过之后写墓碑。保留墓碑与单调修订号:删除后这个 player_id 不能再建角,旧登录链也不能通过 ABA 重获修改权。
// 准备令牌不挡入场,这里持角色行锁再核一次登记全部关闭:和 Register 串行,墓碑之后修订号变了,旧世代再也登记不进来。
// 小表那一行这里不删:它是墓碑之后收尾(释放名字)的待办,收尾做完才删(settleRemovedRole),没做完后台按它接着做。
func (s *AccountDirectoryStore) finishRemoval(ctx context.Context, state *rfpb.RoleFenceState) error {
	var tombstone *rfpb.RoleFenceState
	err := s.playerTx(ctx, state.PlayerId, func(tx *sql.Tx) error {
		if err := lockRemoving(ctx, tx, state); err != nil {
			return err
		}
		if err := requireClosedAdmissions(ctx, tx, state.PlayerId); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE account_role SET account='',owner_revision=owner_revision+1,trade_listing_id=0,prepare_token=0,fence_version=fence_version+1,updated_ms="+dbNowMsSQL+" WHERE player_id=?", state.PlayerId); err != nil {
			return err
		}
		var err error
		tombstone, err = readFenceState(ctx, tx, state.PlayerId)
		return err
	})
	if errors.Is(err, errRemovalDone) {
		return nil
	}
	if err != nil {
		return err
	}
	s.pushFence(ctx, tombstone)
	return nil
}

// abortRemoval 在有挂起时放弃:清令牌、版本加 1、删小表。角色回到没开始删的样子,调用方拿到 ErrConflict。
func (s *AccountDirectoryStore) abortRemoval(ctx context.Context, state *rfpb.RoleFenceState) error {
	var thawed *rfpb.RoleFenceState
	err := s.playerTx(ctx, state.PlayerId, func(tx *sql.Tx) error {
		if err := lockRemoving(ctx, tx, state); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE account_role SET prepare_token=0,fence_version=fence_version+1,updated_ms="+dbNowMsSQL+" WHERE player_id=?", state.PlayerId); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM account_role_removal WHERE player_id=?", state.PlayerId); err != nil {
			return err
		}
		var err error
		thawed, err = readFenceState(ctx, tx, state.PlayerId)
		return err
	})
	if errors.Is(err, errRemovalDone) {
		return nil
	}
	if err != nil {
		return err
	}
	s.pushFence(ctx, thawed)
	return accountdirectory.ErrConflict
}

// RemovalResumeResult 是一轮后台收尾的结果。Failed 留在小表里,下一轮再试。
type RemovalResumeResult struct {
	Finished, Abandoned, Failed int
}

// ResumeRemovals 把开始超过 olderThan 的删角接着做:全通过写墓碑,有挂起放弃,不可达留给下一轮。
// 流程和 Remove 相同,多副本同时跑靠角色行锁和令牌串行。每轮最多 limit 行,逐片读,每轮换一个起点,积压多的片不会饿死别的片。
func (s *AccountDirectoryStore) ResumeRemovals(ctx context.Context, olderThan time.Duration, limit int) (RemovalResumeResult, error) {
	var out RemovalResumeResult
	if s.fences == nil {
		return out, accountdirectory.ErrUnavailable
	}
	removals, err := s.pendingRemovals(ctx, olderThan, limit)
	if err != nil {
		return out, err
	}
	var failure error
	for _, r := range removals {
		err := s.removeRole(ctx, r.account, r.player, r.revision)
		switch {
		case err == nil:
			out.Finished++
		case errors.Is(err, accountdirectory.ErrConflict):
			out.Abandoned++
		default:
			out.Failed++
			failure = errors.Join(failure, err)
		}
	}
	return out, failure
}

type pendingRemoval struct {
	player, revision uint64
	account          string
}

func (s *AccountDirectoryStore) pendingRemovals(ctx context.Context, olderThan time.Duration, limit int) ([]pendingRemoval, error) {
	k := s.shards.k
	start := int(s.removalCursor.Add(1)) % k
	var removals []pendingRemoval
	for i := 0; i < k && len(removals) < limit; i++ {
		found, err := readPendingRemovals(ctx, s.shards.pools[(start+i)%k], olderThan, limit-len(removals))
		if err != nil {
			return nil, err
		}
		removals = append(removals, found...)
	}
	return removals, nil
}

func readPendingRemovals(ctx context.Context, db *sql.DB, olderThan time.Duration, limit int) ([]pendingRemoval, error) {
	rows, err := db.QueryContext(ctx, "SELECT player_id,account,owner_revision FROM account_role_removal WHERE started_ms+?<"+dbNowMsSQL+" ORDER BY started_ms LIMIT ?", olderThan.Milliseconds(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pendingRemoval
	for rows.Next() {
		var r pendingRemoval
		if err := rows.Scan(&r.player, &r.account, &r.revision); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
