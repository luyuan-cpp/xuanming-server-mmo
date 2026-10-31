package data

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"sort"
	"time"

	"shared/accountdirectory"
	"shared/authoritysql"
	"shared/placement"
	"shared/roleidentity"
)

var roleAuthoritySchema = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// ConfigureRoleAuthority 只在进程装配期调用；生产传显式全局目录schema，不从DSN猜。
// 原构造器保留给现有独立仓储夹具；在线进程必须经本函数启用提交互锁。
func (r *GuildRepo) ConfigureRoleAuthority(schema string, backend authoritysql.Backend) error {
	if !roleAuthoritySchema.MatchString(schema) {
		return errors.New("全局角色目录schema未配置")
	}
	probe, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := authoritysql.AssertCurrentSession(probe, r.db, backend); err != nil {
		return err
	}
	if err := placement.CheckRoleRouteContract(probe, r.db, schema); err != nil {
		return err
	}
	r.roleAuthority = schema
	return nil
}

// 只在统一角色前缀内调用；按player升序先于原G→state→member→seq→outbox锁序。
func (r *GuildRepo) lockRoleForMembership(ctx context.Context, tx *sql.Tx, player uint64) error {
	if r.roleAuthority == "" {
		return nil
	} // 仅原独立仓储测试构造；在线装配拒绝空schema。
	var listing, revision uint64
	var account []byte
	err := tx.QueryRowContext(ctx, "SELECT account,owner_revision,trade_listing_id FROM `"+r.roleAuthority+"`.account_role WHERE player_id=? FOR UPDATE", player).Scan(&account, &revision, &listing)
	if err != nil {
		return err
	}
	if !accountdirectory.ValidAccount(string(account)) || revision == 0 || listing != 0 {
		return ErrWriteConflict
	}
	if original, ok := roleidentity.Original(ctx); ok && original.Role.PlayerId == player && (original.Account != string(account) || original.OwnerRevision != revision) {
		return ErrWriteConflict
	}
	return nil
}

type roleTargetsKey struct{}

// 调用者在进入事务前给出全部可能发资产/入帮的角色；runTxBody统一与原actor合并升序锁定。
func withRoleTargets(ctx context.Context, players ...uint64) context.Context {
	old, _ := ctx.Value(roleTargetsKey{}).([]uint64)
	return context.WithValue(ctx, roleTargetsKey{}, append(append([]uint64(nil), old...), players...))
}
func (r *GuildRepo) lockRolePrefix(ctx context.Context, tx *sql.Tx) error {
	players, _ := ctx.Value(roleTargetsKey{}).([]uint64)
	players = append([]uint64(nil), players...)
	if original, ok := roleidentity.Original(ctx); ok {
		players = append(players, original.Role.PlayerId)
	}
	sort.Slice(players, func(i, j int) bool { return players[i] < players[j] })
	unique := players[:0]
	var previous uint64
	for _, player := range players {
		if player == 0 || player == previous {
			continue
		}
		previous = player
		unique = append(unique, player)
	}
	if r.roleAuthority != "" && len(unique) != 0 {
		if err := placement.LockRoleRoutes(ctx, tx, r.roleAuthority, unique); err != nil {
			if errors.Is(err, placement.ErrRoleRouteFenced) {
				// 路由行被冻结(搬库中)或预读后被改过:暂时性拒绝,按写冲突让客户端稍后重试。
				return ErrWriteConflict
			}
			return err
		}
	}
	for _, player := range unique {
		if err := r.lockRoleForMembership(ctx, tx, player); err != nil {
			return err
		}
	}
	return nil
}

// 受理预检用于读接口；所有写仍在runTxBody持P锁复核同一份原会话。
func (r *GuildRepo) CheckOriginalRole(ctx context.Context) error {
	original, ok := roleidentity.Original(ctx)
	if !ok || !roleAuthoritySchema.MatchString(r.roleAuthority) {
		return ErrWriteConflict
	}
	var account string
	var revision, listing uint64
	if err := r.db.QueryRowContext(ctx, "SELECT account,owner_revision,trade_listing_id FROM `"+r.roleAuthority+"`.account_role WHERE player_id=?", original.Role.PlayerId).Scan(&account, &revision, &listing); err != nil {
		return err
	}
	if account != original.Account || revision != original.OwnerRevision || listing != 0 {
		return ErrWriteConflict
	}
	return nil
}
