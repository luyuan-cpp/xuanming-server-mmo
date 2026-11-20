package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"sync"
	"time"

	dspb "proto/data_service"
	rfpb "proto/role_fence"
	"shared/accountdirectory"
	"shared/rolefence"
	"shared/safego"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// 账号注销的目录一侧(docs/design/account-deletion.md §4、§7)。
//
// 状态只在账号行(account_directory,账号分片)上变:0 正常、1 冷静期、2 执行中。申请、撤销、认领、删行都持账号行锁;
// 建角与过户买入的 reserveRef 在同一把锁里判状态(注销中名额按 0 算),所以状态一旦不是 0 就不会再有新 ref。
// 删角本身仍走现有的 Remove(两阶段,login 的执行者逐名调),这里不碰角色行。
// 四个动作都幂等:申请重复回原状态、不刷新时刻;撤销重复回 0;认领按条件更新;删行按「状态 2 且 ref 为空」条件删。
// 账号行删掉之后同一账号串再登录,lockDirectory(create=true) 建出的是状态 0 的新行;回执只存账号的 SHA-256。

const AccountDeletionReceiptTableName = "account_deletion_receipt"

const accountDeletionReceiptDDL = "CREATE TABLE IF NOT EXISTS account_deletion_receipt (account_digest VARBINARY(32) NOT NULL COMMENT 'pb:1'," +
	"requested_ms BIGINT UNSIGNED NOT NULL COMMENT 'pb:2',deleted_ms BIGINT UNSIGNED NOT NULL COMMENT 'pb:3',source INT UNSIGNED NOT NULL COMMENT 'pb:4'," +
	"PRIMARY KEY(account_digest)) ENGINE=InnoDB"

const (
	deletionNone     uint32 = 0
	deletionPending  uint32 = 1
	deletionDeleting uint32 = 2

	// DeletionSourceUser / DeletionSourceAdmin 是申请来源,记进账号行与回执。
	DeletionSourceUser  uint32 = 1
	DeletionSourceAdmin uint32 = 2

	// MaxDeletionCoolingOff 是冷静期配置的上限。
	MaxDeletionCoolingOff = 30 * 24 * time.Hour
	// MaxDeletionClaim 是一次认领的上限。
	MaxDeletionClaim = 32
	// DeletionOverdueAfter:冷静期满这么久还没删完算逾期(gauge data_service_account_deletion_overdue)。
	DeletionOverdueAfter = 24 * time.Hour
	// deletionLease 是执行者认领一个账号的租约。一轮处理用不了这么久;到期没删完就让别的副本接着做,每一步都幂等。
	deletionLease = 2 * time.Minute

	// 预检里目录自己判的两种挂起:角色挂着寄售(最终冻结),角色在投票(过户或删角的准备令牌)。参与者回的原因原样透传。
	blockerRoleListed    = "role_listed"
	blockerRolePreparing = "role_preparing"
	// blockerUnnamed:参与者说有挂起却没给原因时的兜底。
	blockerUnnamed = "pending"
	// deletionProbePoint 是预检并行试探的 safego 埋点名。
	deletionProbePoint = "data_service.account_deletion.probe"
)

func accountDigest(account string) []byte {
	d := sha256.Sum256([]byte(account))
	return d[:]
}

func deletionOutcome(outcome dspb.AccountDeletionOutcome, lifecycle *dspb.AccountLifecycle) *dspb.AccountDeletionResponse {
	return &dspb.AccountDeletionResponse{Outcome: outcome, Lifecycle: lifecycle}
}

// readLifecycleTx 在事务里读账号行的注销状态。调用方已经锁了这一行。
func readLifecycleTx(ctx context.Context, tx *sql.Tx, account string) (*dspb.AccountLifecycle, error) {
	var state uint32
	var executeAfter uint64
	if err := tx.QueryRowContext(ctx, "SELECT deletion_state,deletion_execute_after_ms FROM account_directory WHERE account=?", []byte(account)).
		Scan(&state, &executeAfter); err != nil {
		return nil, err
	}
	return &dspb.AccountLifecycle{DeletionState: dspb.AccountDeletionState(state), DeletionExecuteAfterMs: executeAfter}, nil
}

// RequestDeletion 申请注销:预检账号的全部角色,通过才在账号行锁里 0 → 1,写申请时刻与到期时刻(库时钟)。
// 已经是 1 或 2 原样回,不刷新时刻,也不再预检。读失败、参与者问不齐、没装配协调者都带内回 RETRY,什么都没写。
// 提交语句本身失败回错误(结局未知);同参数重放幂等。
func (s *AccountDirectoryStore) RequestDeletion(ctx context.Context, account string, source uint32, coolingOff time.Duration) (*dspb.AccountDeletionResponse, error) {
	if !accountdirectory.ValidAccount(account) || (source != DeletionSourceUser && source != DeletionSourceAdmin) ||
		coolingOff < 0 || coolingOff > MaxDeletionCoolingOff {
		return nil, accountdirectory.ErrInvalid
	}
	row, refs, err := s.steps.readAccount(ctx, account)
	if err != nil {
		logx.Errorf("[account-deletion] read account before the precheck failed: %v", err)
		return deletionOutcome(dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_RETRY, nil), nil
	}
	if !row.exists {
		return deletionOutcome(dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_NOT_FOUND, nil), nil
	}
	if row.deletionState != deletionNone {
		return deletionOutcome(dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_OK, &dspb.AccountLifecycle{
			DeletionState: dspb.AccountDeletionState(row.deletionState), DeletionExecuteAfterMs: row.executeAfterMs}), nil
	}
	blockers, ok := s.deletionBlockers(ctx, account, refs)
	if !ok {
		return deletionOutcome(dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_RETRY, nil), nil
	}
	if source == DeletionSourceAdmin {
		blockers = withoutSelfServiceBlockers(blockers)
	}
	if len(blockers) > 0 {
		out := deletionOutcome(dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_BLOCKED, &dspb.AccountLifecycle{})
		out.Blockers = blockers
		return out, nil
	}
	var lifecycle *dspb.AccountLifecycle
	err = shardTx(ctx, s.shards.forAccount(account), func(tx *sql.Tx) error {
		state, err := lockDirectoryState(ctx, tx, account, false)
		if err != nil {
			return err
		}
		if state == deletionNone {
			if _, err := tx.ExecContext(ctx, "UPDATE account_directory SET deletion_state=?,deletion_requested_ms="+dbNowMsSQL+
				",deletion_execute_after_ms="+dbNowMsSQL+"+?,deletion_lease_until_ms=0,deletion_source=? WHERE account=?",
				deletionPending, coolingOff.Milliseconds(), source, []byte(account)); err != nil {
				return err
			}
		}
		lifecycle, err = readLifecycleTx(ctx, tx, account)
		return err
	})
	if errors.Is(err, accountdirectory.ErrMissing) {
		return deletionOutcome(dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_NOT_FOUND, nil), nil
	}
	if err != nil {
		return nil, err
	}
	return deletionOutcome(dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_OK, lifecycle), nil
}

// deletionBlockers 预检账号的全部角色(rolesOfRefs 只留真相是这个账号的),逐名并行,一次预检约等于一次试探的上限。
// ok=false 表示这一次没判成:读失败、问不齐,或者没装配协调者(少问一个参与者就可能漏掉挂起,不能当没有)。
func (s *AccountDirectoryStore) deletionBlockers(ctx context.Context, account string, refs []roleRef) ([]*dspb.AccountDeletionBlocker, bool) {
	if s.fences == nil {
		return nil, false
	}
	roles, err := s.rolesOfRefs(ctx, account, refs)
	if err != nil {
		logx.Errorf("[account-deletion] read roles for the precheck failed: %v", err)
		return nil, false
	}
	type probe struct {
		reasons []string
		ok      bool
	}
	probes := make([]probe, len(roles.Players))
	var wg sync.WaitGroup
	for i, role := range roles.Players {
		wg.Add(1)
		safego.Go(deletionProbePoint, func() {
			defer wg.Done()
			probes[i].reasons, probes[i].ok = s.roleBlockers(ctx, role.PlayerId)
		})
	}
	wg.Wait()
	var out []*dspb.AccountDeletionBlocker
	for i, p := range probes {
		if !p.ok {
			return nil, false
		}
		for _, reason := range p.reasons {
			out = append(out, &dspb.AccountDeletionBlocker{PlayerId: roles.Players[i].PlayerId, Reason: reason})
		}
	}
	return out, true
}

// selfServiceBlockers 是只要本人先处理、执行时由删角收尾代为处理的挂起(参与者只在试探里回,rolefence.Probing):
// 帮主(guild 收尾自动让位或解散,docs/design/account-deletion.md §6.3)。本人申请照拦,让他自己决定交给谁;
// 运维代办(客服核实身份后提交)没有玩家可以去处理,不拦。其余挂起是在途的业务,两种来源一律拦。
var selfServiceBlockers = map[string]bool{"guild_leader": true}

func withoutSelfServiceBlockers(blockers []*dspb.AccountDeletionBlocker) []*dspb.AccountDeletionBlocker {
	kept := blockers[:0:0]
	for _, b := range blockers {
		if !selfServiceBlockers[b.GetReason()] {
			kept = append(kept, b)
		}
	}
	return kept
}

// roleBlockers 判一名角色有没有挂起:有冻结令牌直接算,否则试探全部参与者(不写副本)。墓碑没有挂起。
func (s *AccountDirectoryStore) roleBlockers(ctx context.Context, player uint64) ([]string, bool) {
	state, err := s.steps.readFence(ctx, player)
	if errors.Is(err, accountdirectory.ErrMissing) {
		return nil, true
	}
	if err != nil {
		logx.Errorf("[account-deletion] read role fence for the precheck failed: player=%d err=%v", player, err)
		return nil, false
	}
	switch {
	case state.Account == "":
		return nil, true
	case state.FreezeToken != 0 && state.FreezeFinal:
		return []string{blockerRoleListed}, true
	case state.FreezeToken != 0:
		return []string{blockerRolePreparing}, true
	}
	// 试探要带删角的准备令牌:参与者只认带令牌的 PrepareRoleFreeze(没有令牌按非法参数拒),而且按令牌种类选判据——
	// 删角令牌走删角的挂起全集,过户令牌走过户的(要求先离帮)。只是试探,目录行一个字都不写。
	probe := proto.Clone(state).(*rfpb.RoleFenceState)
	probe.FreezeToken = rolefence.RemovalToken(player)
	probe.FreezeFinal = false
	outcome, reasons := s.fences.DryRunBlockers(ctx, probe)
	switch outcome {
	case rolefence.PrepareClean:
		return nil, true
	case rolefence.PrepareBlocked:
		if len(reasons) == 0 {
			reasons = []string{blockerUnnamed}
		}
		return reasons, true
	default:
		return nil, false
	}
}

// CancelDeletion 撤销:账号行锁里 1 → 0 并清掉注销的几列;0 原样回;2 回 TOO_LATE(执行中不可撤销)。
func (s *AccountDirectoryStore) CancelDeletion(ctx context.Context, account string) (*dspb.AccountDeletionResponse, error) {
	if !accountdirectory.ValidAccount(account) {
		return nil, accountdirectory.ErrInvalid
	}
	outcome := dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_OK
	var lifecycle *dspb.AccountLifecycle
	err := shardTx(ctx, s.shards.forAccount(account), func(tx *sql.Tx) error {
		state, err := lockDirectoryState(ctx, tx, account, false)
		if err != nil {
			return err
		}
		switch state {
		case deletionPending:
			if _, err := tx.ExecContext(ctx, "UPDATE account_directory SET deletion_state=0,deletion_requested_ms=0,deletion_execute_after_ms=0,"+
				"deletion_lease_until_ms=0,deletion_source=0 WHERE account=?", []byte(account)); err != nil {
				return err
			}
		case deletionDeleting:
			outcome = dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_TOO_LATE
		}
		lifecycle, err = readLifecycleTx(ctx, tx, account)
		return err
	})
	if errors.Is(err, accountdirectory.ErrMissing) {
		return deletionOutcome(dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_NOT_FOUND, nil), nil
	}
	if err != nil {
		return nil, err
	}
	return deletionOutcome(outcome, lifecycle), nil
}

// ClaimDeletions 给执行者认领:冷静期满的 1 与租约过期的 2,逐行条件改成 2 并写租约,回改成的账号。1 → 2 之后不可撤销。
// 逐片扫、逐轮换起点。一片读或改失败不影响别的片,失败汇总在 error 里;认领到的照样回(改的结局未知的那一行等租约过期再被认领)。
func (s *AccountDirectoryStore) ClaimDeletions(ctx context.Context, limit int) ([]string, error) {
	if limit < 1 || limit > MaxDeletionClaim {
		return nil, accountdirectory.ErrInvalid
	}
	k := s.shards.k
	start := int(s.deletionCursor.Add(1) % uint64(k))
	var claimed []string
	var failure error
	for i := 0; i < k && len(claimed) < limit; i++ {
		db := s.shards.pools[(start+i)%k]
		due, err := dueDeletions(ctx, db, limit-len(claimed))
		if err != nil {
			failure = errors.Join(failure, err)
			continue
		}
		for _, account := range due {
			won := false
			err := shardTx(ctx, db, func(tx *sql.Tx) error {
				res, err := tx.ExecContext(ctx, "UPDATE account_directory SET deletion_state=?,deletion_lease_until_ms="+dbNowMsSQL+"+? "+
					"WHERE account=? AND deletion_state IN (1,2) AND deletion_execute_after_ms<="+dbNowMsSQL+" AND deletion_lease_until_ms<="+dbNowMsSQL,
					deletionDeleting, deletionLease.Milliseconds(), []byte(account))
				if err != nil {
					return err
				}
				n, err := res.RowsAffected()
				won = n == 1
				return err
			})
			if err != nil {
				failure = errors.Join(failure, err)
				continue
			}
			if won {
				claimed = append(claimed, account)
			}
		}
	}
	return claimed, failure
}

func dueDeletions(ctx context.Context, db *sql.DB, limit int) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT account FROM account_directory WHERE deletion_state IN (1,2) AND deletion_execute_after_ms<="+dbNowMsSQL+
		" AND deletion_lease_until_ms<="+dbNowMsSQL+" ORDER BY deletion_execute_after_ms LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var account []byte
		if err := rows.Scan(&account); err != nil {
			return nil, err
		}
		out = append(out, string(account))
	}
	return out, rows.Err()
}

// FinishDeletion 收尾:先清能证明陈旧的 ref(删角墓碑之后没来得及删的),再在账号行锁里要求状态是 2、一条 ref 都不剩,
// 写回执、删账号行。还有 ref(角色没删完、在途建角 / 过户的 PENDING、标了保留待人工)回 Done=false。行已不在回 Done(幂等)。
func (s *AccountDirectoryStore) FinishDeletion(ctx context.Context, account string) (*dspb.FinishAccountDeletionResponse, error) {
	if !accountdirectory.ValidAccount(account) {
		return nil, accountdirectory.ErrInvalid
	}
	row, refs, err := s.steps.readAccount(ctx, account)
	if err != nil {
		return nil, err
	}
	if !row.exists {
		return &dspb.FinishAccountDeletionResponse{Done: true}, nil
	}
	if row.deletionState != deletionDeleting {
		return nil, accountdirectory.ErrConflict
	}
	if len(refs) > 0 {
		if _, err := s.reapRefs(ctx, account, refs); err != nil {
			logx.Infof("[account-deletion] reap stale refs before finishing: %v", err)
		}
	}
	out := &dspb.FinishAccountDeletionResponse{}
	err = shardTx(ctx, s.shards.forAccount(account), func(tx *sql.Tx) error {
		state, err := lockDirectoryState(ctx, tx, account, false)
		if errors.Is(err, accountdirectory.ErrMissing) {
			out.Done = true
			return nil
		}
		if err != nil {
			return err
		}
		if state != deletionDeleting {
			return accountdirectory.ErrConflict
		}
		left, err := readRefs(ctx, tx, account)
		if err != nil {
			return err
		}
		if len(left) > 0 {
			out.RemainingRefs = uint32(len(left))
			return nil
		}
		var requested uint64
		var source uint32
		if err := tx.QueryRowContext(ctx, "SELECT deletion_requested_ms,deletion_source FROM account_directory WHERE account=?", []byte(account)).
			Scan(&requested, &source); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO "+AccountDeletionReceiptTableName+"(account_digest,requested_ms,deleted_ms,source) VALUES (?,?,"+dbNowMsSQL+",?) "+
			"ON DUPLICATE KEY UPDATE requested_ms=?,deleted_ms="+dbNowMsSQL+",source=?", accountDigest(account), requested, source, requested, source); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM account_directory WHERE account=?", []byte(account)); err != nil {
			return err
		}
		out.Done = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AccountDeletionState 点读账号行的注销状态,不取锁。没有这一行回 NONE:注销完成的账号行已删,它的角色也都是墓碑,Lookup 先回不存在。
// EnterGame 经 LookupAccountRole 顺带问它;与角色行不在一个事务,只是入口的闸门,不是提交点(docs/design/account-deletion.md §4.3)。
func (s *AccountDirectoryStore) AccountDeletionState(ctx context.Context, account string) (dspb.AccountDeletionState, error) {
	if !accountdirectory.ValidAccount(account) {
		return 0, accountdirectory.ErrInvalid
	}
	var state uint32
	err := s.shards.forAccount(account).QueryRowContext(ctx, "SELECT deletion_state FROM account_directory WHERE account=?", []byte(account)).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return dspb.AccountDeletionState_ACCOUNT_DELETION_STATE_NONE, nil
	}
	if err != nil {
		return 0, err
	}
	return dspb.AccountDeletionState(state), nil
}

// AccountDeletionsOverdue 数冷静期满超过 olderThan 还没删完的账号,各片合计。读失败回错误,调用方这一轮不写 gauge。
func (s *AccountDirectoryStore) AccountDeletionsOverdue(ctx context.Context, olderThan time.Duration) (int, error) {
	total := 0
	for _, db := range s.shards.pools {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM account_directory WHERE deletion_state IN (1,2) AND deletion_execute_after_ms<="+dbNowMsSQL+"-?",
			olderThan.Milliseconds()).Scan(&n); err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}
