package authority

import (
	"context"
	"database/sql"
	"fmt"

	pb "proto/match"
	teampb "proto/team"
	"shared/placement"
)

// ErrTeamCrossZone：禁止跨区组队时，开战名单解析到最终区之后仍不同区。只出现在 launch 诞生时的那一次判定（requireTeamLaunchHomesTx）。
// 包着 ErrTeamRoute，既有 errors.Is 判断不变；team 据它回 kTeamCrossZoneDenied，而不是内部错误。
var ErrTeamCrossZone = fmt.Errorf("%w: 开战名单不同区", ErrTeamRoute)

// TeamUnitHomeZone 返回完整unit的共同home：全员原始区相同给原始区；原始区不同、合服后解析到同一个最终区给最终区；
// 解析后仍不同的混合unit为0（投影 team_launch_region.home_zone 的0不进任何按区候选）；缺原home是损坏。
// home是角色归属，不能用实际战斗机房或队长所在zone替代。zones 是调用方取的一份映射快照，nil = 没有合服；
// 只想核结构合法的调用方传 nil。
func TeamUnitHomeZone(group []*pb.MatchAuthorityTicketRecord, zones *placement.MergedZones) (uint32, error) {
	if len(group) == 0 {
		return 0, ErrTeamRoute
	}
	var home uint32
	sameOriginal, sameFinal := true, true
	seen := make(map[uint64]bool, len(group))
	for index, ticket := range group {
		if ticket == nil || ticket.PlayerId == 0 || ticket.ZoneId == 0 || seen[ticket.PlayerId] {
			return 0, ErrTeamRoute
		}
		seen[ticket.PlayerId] = true
		if index == 0 {
			home = ticket.ZoneId
			continue
		}
		sameOriginal = sameOriginal && ticket.ZoneId == home
		sameFinal = sameFinal && zones.Final(ticket.ZoneId) == zones.Final(home)
	}
	switch {
	case sameOriginal:
		return home, nil
	case sameFinal:
		return zones.Final(home), nil
	}
	return 0, nil
}

// 完整route/fence/P前缀已经由事务入口锁齐；这里只读复核，不追加任何锁集合。
// 只逐人核归属没有变，不判同区：同区只在 launch 诞生时判一次（requireTeamLaunchHomesTx），之后映射怎么变都不重判。
// 这里还给已进队列、已成局的在途 launch 用，它们报错恢复循环就会反复失败、撤不掉也开不了局；
// 撤销合服后留下的跨区 unit 走完这一局，见 docs/ops/merge-zone-runbook.md §7。
func (s *Store) requireTeamHomeGroupTx(ctx context.Context, tx *sql.Tx, group []*pb.MatchAuthorityTicketRecord) error {
	if _, err := TeamUnitHomeZone(group, nil); err != nil {
		return err
	}
	for _, ticket := range group {
		if err := s.RequireTeamHomeZoneTx(ctx, tx, ticket.PlayerId, ticket.ZoneId); err != nil {
			return err
		}
	}
	return nil
}

// requireTeamLaunchHomesTx 是 launch 诞生时的准入：逐人核归属，禁止跨区组队时名单解析到最终区后必须同区。
func (s *Store) requireTeamLaunchHomesTx(ctx context.Context, tx *sql.Tx, data *teampb.TeamLaunchData) error {
	group := make([]*pb.MatchAuthorityTicketRecord, 0, len(data.Members))
	for _, member := range data.Members {
		group = append(group, &pb.MatchAuthorityTicketRecord{PlayerId: member.Original.PlayerId, ZoneId: data.Original.Zones[member.Original.PlayerId]})
	}
	if err := s.requireTeamHomeGroupTx(ctx, tx, group); err != nil {
		return err
	}
	if s.TeamConfig.AllowCrossZone {
		return nil
	}
	home, err := TeamUnitHomeZone(group, s.MergedZones())
	if err != nil {
		return err
	}
	if home == 0 {
		return ErrTeamCrossZone
	}
	return nil
}

// TeamHomeZonesTx 读 players 每人路由行上的原始 home，写进开战原件（TeamLaunchOriginal.zones，之后抄到票上）。
// 调用方已在本事务锁住这些路由行，不会把预检期间的旧 home 写进原件。
// 记归属、不记玩家当前位置：位置是物理节点的 zone，访客和还连着源区节点的玩家都和归属对不上。
func (s *Store) TeamHomeZonesTx(ctx context.Context, tx *sql.Tx, players []uint64) (map[uint64]uint32, error) {
	homes := make(map[uint64]uint32, len(players))
	for _, player := range players {
		home, err := s.teamHomeZoneTx(ctx, tx, player)
		if err != nil {
			return nil, err
		}
		homes[player] = home
	}
	return homes, nil
}
