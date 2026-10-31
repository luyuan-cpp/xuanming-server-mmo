package data

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"time"

	"shared/accountdirectory"
	"shared/authoritysql"
	"shared/roleidentity"
)

var roleAuthoritySchema = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
var ErrOriginalRole = errors.New("好友原会话角色归属已失效或被冻结")

// ConfigureRoleAuthority 仅在启动装配调用；目录和好友表必须位于同一事务域(同一 TiDB 集群或同一 MySQL 主库)。
// 未配置构造器只供显式系统/旧独立仓储夹具；带原会话的请求绝不回落到该路径。
func (r *FriendRepo) ConfigureRoleAuthority(schema string, backend authoritysql.Backend) error {
	if !roleAuthoritySchema.MatchString(schema) {
		return errors.New("好友全局角色目录schema未配置")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := authoritysql.AssertCurrentSession(ctx, r.db, backend); err != nil {
		return err
	}
	rows, err := r.db.QueryContext(ctx, "SELECT account,owner_revision,trade_listing_id FROM `"+schema+"`.account_role LIMIT 0")
	if err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	r.roleAuthority = schema
	return nil
}

func (r *FriendRepo) requireRoleActor(ctx context.Context, player uint64) error {
	original, ok := roleidentity.Original(ctx)
	if r.roleAuthority == "" {
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

func (r *FriendRepo) lockRolePrefix(ctx context.Context, tx *sql.Tx, players ...uint64) error {
	original, ok := roleidentity.Original(ctx)
	if r.roleAuthority == "" {
		if ok {
			return ErrOriginalRole
		}
		return nil
	}
	if !ok {
		return ErrOriginalRole
	}
	if len(players) == 0 {
		players = []uint64{original.Role.GetPlayerId()}
	}
	included := false
	for _, player := range ascendingUniqueIDs(players) {
		if player == 0 {
			return ErrOriginalRole
		}
		var account []byte
		var revision, listing uint64
		err := tx.QueryRowContext(ctx, "SELECT account,owner_revision,trade_listing_id FROM `"+r.roleAuthority+"`.account_role WHERE player_id=? FOR UPDATE", player).Scan(&account, &revision, &listing)
		if errors.Is(err, sql.ErrNoRows) && player != original.Role.GetPlayerId() {
			continue
		}
		if err != nil {
			return err
		}
		if !accountdirectory.ValidAccount(string(account)) || revision == 0 || listing != 0 {
			return ErrOriginalRole
		}
		if player == original.Role.GetPlayerId() {
			included = true
			if string(account) != original.Account || revision != original.OwnerRevision {
				return ErrOriginalRole
			}
		}
	}
	if !included {
		return ErrOriginalRole
	}
	return nil
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
	if r.roleAuthority == "" {
		return r.db, func() {}, nil
	}
	tx, err := r.beginWriteTx(ctx, player)
	if err != nil {
		return nil, nil, err
	}
	return tx, func() { _ = tx.Rollback() }, nil
}

// 受理预检覆盖 Redis 在线目录；关系写仍在最终事务重新持 P 复核原件。
func (r *FriendRepo) CheckOriginalRole(ctx context.Context) error {
	original, ok := roleidentity.Original(ctx)
	if !ok || r.roleAuthority == "" {
		return ErrOriginalRole
	}
	_, closeRead, err := r.roleReader(ctx, original.Role.GetPlayerId())
	if err != nil {
		return err
	}
	closeRead()
	return nil
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
