package data

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"shared/rolefence"
	"shared/roleidentity"
)

var ErrOriginalRole = errors.New("好友原会话角色归属已失效或被冻结")

// ConfigureRoleFence 仅在启动装配调用(docs/design/horizontal-scale-logical-db.md §3)。
// 未配置构造器只供显式系统/旧独立仓储夹具；带原会话的请求绝不回落到该路径。
func (r *FriendRepo) ConfigureRoleFence(guard *rolefence.Guard) {
	r.guard = guard
}

func (r *FriendRepo) requireRoleActor(ctx context.Context, player uint64) error {
	original, ok := roleidentity.Original(ctx)
	if r.guard == nil {
		if ok {
			return ErrOriginalRole
		}
		return nil // 显式系统仓储入口，外部逻辑入口仍要求原会话。
	}
	if !ok || player == 0 || original.Role.GetPlayerId() != player {
		return ErrOriginalRole
	}
	return nil
}

// lockRolePrefix 是好友事务的第一把锁:按 player_id 升序锁 actor 和对端的副本。
// actor 要核原身份;对端没有原身份,只判冻结和删除。absent 是目录里确认不存在的对端,不锁、不写副本。
func (r *FriendRepo) lockRolePrefix(ctx context.Context, tx *sql.Tx, absent map[uint64]bool, players ...uint64) error {
	original, ok := roleidentity.Original(ctx)
	if !ok {
		return ErrOriginalRole
	}
	actor := original.Role.GetPlayerId()
	if len(players) == 0 {
		players = []uint64{actor}
	}
	expects := make([]rolefence.Expect, 0, 2)
	included := false
	for _, player := range ascendingUniqueIDs(players) {
		switch {
		case player == 0:
			return ErrOriginalRole
		case player == actor:
			included = true
			expects = append(expects, rolefence.Expect{PlayerID: actor, Account: original.Account, OwnerRevision: original.OwnerRevision, Reject: ErrOriginalRole})
		case !absent[player]:
			expects = append(expects, rolefence.Expect{PlayerID: player, Reject: ErrOriginalRole})
		}
	}
	if !included {
		return ErrOriginalRole
	}
	_, err := rolefence.LockAll(ctx, tx, expects...)
	return err
}

type roleSQLReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// 读取持 P 到所有行读完；只读事务以回滚释放锁，不在退出时产生写提交。
func (r *FriendRepo) roleReader(ctx context.Context, player uint64) (roleSQLReader, func(), error) {
	if err := r.requireRoleActor(ctx, player); err != nil {
		return nil, nil, err
	}
	if r.guard == nil {
		return r.db, func() {}, nil
	}
	tx, err := r.beginWriteTx(ctx, player)
	if err != nil {
		return nil, nil, err
	}
	return tx, func() { _ = tx.Rollback() }, nil
}

func (r *FriendRepo) ensureBoundCapacity(ctx context.Context, players ...uint64) error {
	tx, err := r.beginWriteTx(ctx, players...)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, player := range ascendingUniqueIDs(players) {
		var count uint32
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM friend WHERE player_id = ?", player).Scan(&count); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, ensureCapacityRowSQL, player, count, time.Now().UnixMilli()); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return tx.Commit()
}

// beginLockedTx 开事务并取角色锁。取锁失败时事务已回滚,所以 RunTx 能在两次之间调 RPC。
func (r *FriendRepo) beginLockedTx(ctx context.Context, absent map[uint64]bool, players ...uint64) (*sql.Tx, error) {
	tx, err := r.beginSystemWriteTx(ctx)
	if err != nil {
		return nil, err
	}
	if err := r.lockRolePrefix(ctx, tx, absent, players...); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

// beginGuardedTx 开一个已持角色锁的事务。副本判不了时读权威状态再开一次。
// 对端在目录里不存在时不写副本,按原逻辑跳过:本来就没有归属可保护,关系写由守卫之后的业务判定收口。actor 自己不存在是拒绝。
func (r *FriendRepo) beginGuardedTx(ctx context.Context, players ...uint64) (*sql.Tx, error) {
	var tx *sql.Tx
	var absent map[uint64]bool
	begin := func() (err error) {
		tx, err = r.beginLockedTx(ctx, absent, players...)
		return err
	}
	err := r.guard.RunTx(ctx, begin)
	var missing *rolefence.RoleNotFoundError
	if errors.As(err, &missing) {
		absent = make(map[uint64]bool, len(missing.Players))
		for _, player := range missing.Players {
			absent[player] = true
		}
		err = r.guard.RunTx(ctx, begin)
	}
	if errors.Is(err, rolefence.ErrRoleNotFound) {
		return nil, ErrOriginalRole
	}
	if err != nil {
		return nil, err
	}
	return tx, nil
}
