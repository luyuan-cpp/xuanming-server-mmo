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

// CheckOriginalRole 是客户端入口的归属预检(docs/design/gate-online-identity.md §9.3),读接口靠它核归属:不加锁读本库副本,
// 只判角色此刻是否仍归原会话身份,冻结不拦(rolefence.Guard.VerifyOwner,与 match、trade 同一个判据)。
// 所有写仍在 runTxBody 持副本行锁按 rolefence.Check 复核同一份原会话:冻结的角色读得到自己的帮会数据,写不进去。
// 确定不归回 ErrWriteConflict(目录里没有这名角色回 *rolefence.RoleNotFoundError);其余错误是这一次判不了。
func (r *GuildRepo) CheckOriginalRole(ctx context.Context) error {
	original, ok := roleidentity.Original(ctx)
	if !ok || r.fence == nil {
		return ErrWriteConflict
	}
	return r.fence.VerifyOwner(ctx, rolefence.Expect{PlayerID: original.Role.PlayerId, Account: original.Account, OwnerRevision: original.OwnerRevision, Reject: ErrWriteConflict})
}
