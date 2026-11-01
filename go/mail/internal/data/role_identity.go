package data

import (
	"context"
	"database/sql"
	"errors"

	rfpb "proto/role_fence"
	"shared/rolefence"
	"shared/roleidentity"
)

// ConfigureRoleFence 只在装配期调用。没配时带原会话的请求一律拒绝,后台任务不受影响。
func (r *Repo) ConfigureRoleFence(guard *rolefence.Guard) {
	r.guard = guard
}

func validateActor(ctx context.Context, player uint64) error {
	if original, present := roleidentity.Original(ctx); present && original.Role.GetPlayerId() != player {
		return ErrDenied
	}
	return nil
}

// runGuarded 给带原会话的事务接上角色护栏:副本判不了时事务已回滚,读完权威状态再整笔跑一次(docs/design/horizontal-scale-logical-db.md §3)。
// 后台任务不制造客户端actor,不经过这里的自愈。
func (r *Repo) runGuarded(ctx context.Context, action func() error) error {
	if _, present := roleidentity.Original(ctx); !present {
		return action()
	}
	if r.guard == nil {
		return ErrDenied
	}
	err := r.guard.RunTx(ctx, action)
	if errors.Is(err, rolefence.ErrRoleNotFound) {
		return ErrDenied
	}
	return err
}

// 原身份存在时，先锁角色再取邮箱/邮件/操作锁，并持有到业务提交。后台任务不制造客户端actor。
func (r *Repo) lockActor(ctx context.Context, tx *sql.Tx) error {
	original, present := roleidentity.Original(ctx)
	if !present {
		return nil
	}
	_, err := rolefence.LockAll(ctx, tx, rolefence.Expect{PlayerID: original.Role.PlayerId, Account: original.Account, OwnerRevision: original.OwnerRevision, Reject: ErrDenied})
	return err
}

// roleBlockerAssetOp 是低基数枚举,进协调者的指标 label。
const roleBlockerAssetOp = "mail_asset_op"

// RolePending 是 mail 的挂起全集,在 PrepareRoleFreeze 的事务里跑,此时已持有副本行锁。
// 完整终态只有 APPLIED=2、REJECTED=3、ABANDONED=5;MANUAL=4、0、未知、NULL 都阻塞。
func RolePending(ctx context.Context, tx *sql.Tx, state *rfpb.RoleFenceState) (string, error) {
	var op uint64
	err := tx.QueryRowContext(ctx, "SELECT op_id FROM mail_asset_op WHERE player_id=? AND (status NOT IN (2,3,5) OR status IS NULL) LIMIT 1 FOR UPDATE", state.PlayerId).Scan(&op)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return roleBlockerAssetOp, nil
}
