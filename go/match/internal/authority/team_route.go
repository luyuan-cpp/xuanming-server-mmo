package authority

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"shared/placement"
)

var ErrTeamRoute = errors.New("team: 全局路由未允许原队伍事务")

// 完整route前缀在全部P之前执行；与Inbox/Guild共用同一64片真源算法。
func (s *Store) lockTeamRoutes(ctx context.Context, tx *sql.Tx, players []uint64) error {
	if err := placement.LockRoleRoutes(ctx, tx, s.roleSchema, players); err != nil {
		if errors.Is(err, placement.ErrRoleRouteFenced) {
			return ErrTeamRoute
		}
		return err
	}
	return nil
}
func (s *Store) CheckTeamRouteContract(ctx context.Context) error {
	if s == nil {
		return ErrTeamRoute
	}
	if err := placement.CheckRoleRouteContract(ctx, s.DB, s.roleSchema); err != nil {
		if errors.Is(err, placement.ErrRoleRouteFenced) {
			return ErrTeamRoute
		}
		return err
	}
	// 归属要解析到最终区（PlayerHomeZone、RequireTeamHomeZoneTx）。启动时就核一次读权限，别留到第一次组队才报错。
	if _, err := placement.ResolveMergedZoneSQL(ctx, s.DB, s.roleSchema, 0); err != nil {
		return fmt.Errorf("team: zone_merged_into 不可读: %w", err)
	}
	return nil
}

// route前缀已锁住该参与者原行；提交不能把RPC预检期间的旧home写进新名册/consent。
// expected 是解析到最终区的 home（data_service 回的、票上记的都是），所以先把库里的原始 home 解析了再比。
func (s *Store) RequireTeamHomeZoneTx(ctx context.Context, tx *sql.Tx, player uint64, expected uint32) error {
	var home sql.NullInt64
	if expected == 0 {
		return ErrTeamRoute
	}
	if err := tx.QueryRowContext(ctx, "SELECT home_zone FROM `"+s.roleSchema+"`.player_route WHERE route_shard=? AND player_id=?", placement.RouteRowShard(player), player).Scan(&home); err != nil {
		return err
	}
	if !home.Valid || home.Int64 <= 0 {
		return ErrTeamRoute
	}
	resolved, err := placement.ResolveMergedZoneSQL(ctx, tx, s.roleSchema, uint32(home.Int64))
	if err != nil {
		return err
	}
	if resolved != expected {
		return ErrTeamRoute
	}
	return nil
}
