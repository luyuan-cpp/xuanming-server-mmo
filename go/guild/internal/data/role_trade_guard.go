package data

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"shared/authoritysql"
	"shared/rolefence"
	"shared/roleidentity"
)

// ConfigureRoleFence 只在进程装配期调用。角色归属锁本库的护栏副本(docs/design/horizontal-scale-logical-db.md §3),
// 目录只有 data_service 连,这里不跨库读。副本表由 -migrate 建,这里只读核形状。
// 不调用就是现有独立仓储夹具,不加护栏;在线进程必须经本函数启用提交互锁。
func (r *GuildRepo) ConfigureRoleFence(reader rolefence.StateReader, backend authoritysql.Backend) error {
	if reader == nil {
		return errors.New("帮会角色护栏缺目录客户端")
	}
	probe, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := authoritysql.AssertCurrentSession(probe, r.db, backend); err != nil {
		return err
	}
	if err := rolefence.VerifySchema(probe, r.db); err != nil {
		return err
	}
	r.fence = rolefence.NewGuard(r.db, reader)
	return nil
}

type roleTargetsKey struct{}

// 调用者在进入事务前给出全部可能发资产/入帮的角色；runTxBody统一与原actor合并升序锁定。
func withRoleTargets(ctx context.Context, players ...uint64) context.Context {
	old, _ := ctx.Value(roleTargetsKey{}).([]uint64)
	return context.WithValue(ctx, roleTargetsKey{}, append(append([]uint64(nil), old...), players...))
}

// lockRolePrefix 按player升序锁全部副本行,先于原G→state→member→seq→outbox锁序。
// 目标只判冻结和删除;原actor还要核会话捕获的账号、修订。副本判不了回 *StaleError,由 inTx 在事务外自愈后重来。
func (r *GuildRepo) lockRolePrefix(ctx context.Context, tx *sql.Tx) error {
	if r.fence == nil {
		return nil // 仅原独立仓储测试构造；在线装配拒绝空护栏。
	}
	players, _ := ctx.Value(roleTargetsKey{}).([]uint64)
	expects := make([]rolefence.Expect, 0, len(players)+1)
	for _, player := range players {
		if player != 0 {
			expects = append(expects, rolefence.Expect{PlayerID: player, Reject: ErrWriteConflict})
		}
	}
	if original, ok := roleidentity.Original(ctx); ok && original.Role.PlayerId != 0 {
		expects = append(expects, rolefence.Expect{PlayerID: original.Role.PlayerId, Account: original.Account, OwnerRevision: original.OwnerRevision, Reject: ErrWriteConflict})
	}
	_, err := rolefence.LockAll(ctx, tx, expects...)
	return err
}

// 受理预检用于读接口；读本库副本,不加锁。所有写仍在runTxBody持副本行锁复核同一份原会话。
func (r *GuildRepo) CheckOriginalRole(ctx context.Context) error {
	original, ok := roleidentity.Original(ctx)
	if !ok || r.fence == nil {
		return ErrWriteConflict
	}
	player := original.Role.PlayerId
	expect := rolefence.Expect{PlayerID: player, Account: original.Account, OwnerRevision: original.OwnerRevision, Reject: ErrWriteConflict}
	return r.fence.RunTx(ctx, func() error {
		fence, err := rolefence.Read(ctx, r.db, player)
		return rolefence.Check(fence, err, expect)
	})
}
