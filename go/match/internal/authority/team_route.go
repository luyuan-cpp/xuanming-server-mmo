package authority

import (
	"context"
	"database/sql"
	"errors"

	"shared/rolefence"
)

var ErrTeamRoute = errors.New("team: 角色归属区未允许原队伍事务")

// teamHomeZoneTx 读护栏副本上的原始 home。调用方已在本事务锁住这一行（lockOriginalPlayers），home_zone 建角后不变，读到的就是提交时刻的值。
func (s *Store) teamHomeZoneTx(ctx context.Context, tx *sql.Tx, player uint64) (uint32, error) {
	fence, err := rolefence.Lock(ctx, tx, player)
	if errors.Is(err, rolefence.ErrMissing) {
		return 0, ErrTeamRoute
	}
	if err != nil {
		return 0, err
	}
	return fence.HomeZone, nil
}

// 副本行已锁住：提交不能把RPC预检期间的旧home写进新名册/consent。
// expected 和副本行存的都是原始区，合服不改写任何一边，两边解析到最终区再比；同一份映射快照，不会被一次刷新夹在中间。
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
