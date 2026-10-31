package authority

import (
	"context"
	"database/sql"
	"errors"

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
	return nil
}

// teamHomeZoneTx 读路由行上的原始 home。调用方已在本事务锁住这一行（lockTeamRoutes），读到的就是提交时刻的值。
func (s *Store) teamHomeZoneTx(ctx context.Context, tx *sql.Tx, player uint64) (uint32, error) {
	var home sql.NullInt64
	if err := tx.QueryRowContext(ctx, "SELECT home_zone FROM `"+s.roleSchema+"`.player_route WHERE route_shard=? AND player_id=?", placement.RouteRowShard(player), player).Scan(&home); err != nil {
		return 0, err
	}
	if !home.Valid || home.Int64 <= 0 {
		return 0, ErrTeamRoute
	}
	return uint32(home.Int64), nil
}

// route前缀已锁住该参与者原行；提交不能把RPC预检期间的旧home写进新名册/consent。
// expected 和路由行存的都是原始区，合服不改写任何一边，两边解析到最终区再比；同一份映射快照，不会被一次刷新夹在中间。
// 所以合服提交、撤销都不会让在途的票和 launch 在这里失败，失败只说明这名玩家的归属真的变了。
func (s *Store) RequireTeamHomeZoneTx(ctx context.Context, tx *sql.Tx, player uint64, expected uint32) error {
	if expected == 0 {
		return ErrTeamRoute
	}
	home, err := s.teamHomeZoneTx(ctx, tx, player)
	if err != nil {
		return err
	}
	zones := s.MergedZones()
	if zones.Final(home) != zones.Final(expected) {
		return ErrTeamRoute
	}
	return nil
}
