package authority

import (
	"context"
	"database/sql"
	"errors"

	"match/internal/catalog"
	"match/internal/scope"

	pb "proto/match"
	"shared/placement"
)

// 排队成局在提交点复核的玩法规则（docs/design/global-match-hierarchical-pools.md §5.5）。
// 撮合（poolgroup）已经按同一套规则挑过人；这里用加锁读出的原票和数据库时钟再判一次，
// 预检不能代替提交校验（AGENTS.md §11.3）。

var (
	// ErrScopeWait：组里有单位还没等到允许这一组跨到的范围层级（zone → 大区 → 全服）。
	ErrScopeWait = errors.New("成局成员尚未等到允许的匹配范围")
	// ErrTemplateClosed：(模式, 模板) 不在开放的 PlayMode 入口里。
	ErrTemplateClosed = errors.New("匹配入口未开放")
	// ErrProbeRequired：没有有效的地域探测（未探测、计划不符或已过期），客户端应重新探测。
	ErrProbeRequired = errors.New("缺少有效地域探测")
	// ErrNoRegionWithinLatency：探测有效，但没有任何机房在硬延迟以内。
	ErrNoRegionWithinLatency = errors.New("没有机房满足硬延迟")
)

// ConfigureMatchCatalog 在启动时装配模板目录与逐级放宽阈值，之后只读。
// 两者都进全局策略指纹（EnsureGlobalPolicy），所有副本必须一致。
// 未装配的 Store（单测）：没有白名单，大区全视为未分组，阈值为 0 即入队就允许全服，期限取默认值。
func (s *Store) ConfigureMatchCatalog(c *catalog.Catalog, schedule scope.Schedule) error {
	if c == nil {
		return errors.New("匹配模板目录未加载")
	}
	if err := schedule.Validate(); err != nil {
		return err
	}
	s.catalog, s.schedule = c, schedule
	return nil
}

// Catalog 返回装配的模板目录（可能为 nil，见 ConfigureMatchCatalog）。
func (s *Store) Catalog() *catalog.Catalog { return s.catalog }

// Schedule 返回逐级放宽阈值。
func (s *Store) Schedule() scope.Schedule { return s.schedule }

// battleDeadlineMs 是成局期限：提交时刻 + 备战预算（一代工作租约）+ 模板战斗时长。
// 模板没有时长时与旧的固定 300 秒一致（catalog.DefaultBattleSeconds + 120 秒）。
func (s *Store) battleDeadlineMs(nowMs uint64, configID uint32) uint64 {
	return nowMs + WorkLeaseMs + uint64(s.catalog.BattleSeconds(configID))*1000
}

// queueUnits 把一组票按"不可拆单位"分组：同一 team_id 的整队是一个单位，单排票各自一个单位。
// 输出顺序与第一次出现的顺序一致，结果确定。
func queueUnits(group []*pb.MatchAuthorityTicketRecord) [][]*pb.MatchAuthorityTicketRecord {
	var units [][]*pb.MatchAuthorityTicketRecord
	team := map[uint64]int{}
	for _, ticket := range group {
		if ticket.TeamId == 0 {
			units = append(units, []*pb.MatchAuthorityTicketRecord{ticket})
			continue
		}
		if index, ok := team[ticket.TeamId]; ok {
			units[index] = append(units[index], ticket)
			continue
		}
		team[ticket.TeamId] = len(units)
		units = append(units, []*pb.MatchAuthorityTicketRecord{ticket})
	}
	return units
}

// singleUnit 报告这组票是不是只有一个单位（单人档、满员整队）。组里没有陌生人时：
// 不受范围树约束，首选机房没有节点也不必等跨机房阈值（D3）。
func singleUnit(group []*pb.MatchAuthorityTicketRecord) bool {
	return len(queueUnits(group)) == 1
}

// admitScope 用加锁读出的原票（home zone、原始入队时刻）与数据库时钟判定范围树双向同意。
func (s *Store) admitScope(group []*pb.MatchAuthorityTicketRecord, nowMs uint64) (scope.Level, bool) {
	units := queueUnits(group)
	members := make([][]scope.Member, len(units))
	for i, unit := range units {
		for _, ticket := range unit {
			members[i] = append(members[i], scope.Member{Node: s.catalog.ZoneNode(ticket.ZoneId), EnqueuedMs: ticket.EnqueuedMs})
		}
	}
	return scope.Admit(s.schedule, members, nowMs)
}

// PlayerHomeZone 读玩家的 home zone（角色归属，不是当前所在场景的 zone），已解析到最终区
// （别名合服，docs/design/player-data-tidb-scaling-plan.md §7.1）。入队时写进票据，
// 之后撮合与提交点都读票上的值；整队提交另在 route 锁内逐人复核当前 home。
func (s *Store) PlayerHomeZone(ctx context.Context, player uint64) (uint32, error) {
	var home sql.NullInt64
	err := s.DB.QueryRowContext(ctx, "SELECT home_zone FROM `"+s.roleSchema+"`.player_route WHERE route_shard=? AND player_id=?", placement.RouteRowShard(player), player).Scan(&home)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (!home.Valid || home.Int64 <= 0)) {
		return 0, ErrTeamRoute
	}
	if err != nil {
		return 0, err
	}
	return placement.ResolveMergedZoneSQL(ctx, s.DB, s.roleSchema, uint32(home.Int64))
}

// requireTicketHomesTx 在调用方已锁住的 route 前缀下，逐人核对票上的 home zone 仍是解析后的当前 home。
// 不要求全组同 home：跨 home 成局由范围树双向同意决定（admitScope），这里只防迁移或合服后的旧 home。
func (s *Store) requireTicketHomesTx(ctx context.Context, tx *sql.Tx, group []*pb.MatchAuthorityTicketRecord) error {
	if _, err := TeamUnitHomeZone(group); err != nil {
		return err
	}
	for _, ticket := range group {
		if err := s.RequireTeamHomeZoneTx(ctx, tx, ticket.PlayerId, ticket.ZoneId); err != nil {
			return err
		}
	}
	return nil
}
