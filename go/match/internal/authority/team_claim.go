package authority

import (
	"context"
	"errors"
	"match/internal/config"
	"math"
	"sort"

	pb "proto/match"
	teampb "proto/team"

	"google.golang.org/protobuf/proto"
)

// ClaimTeamUnits 将所有完整队伍与单排占用一次提交。全部P→全部Team→全部launch
// →全部票→formation，不得让两个实例把同一队拆进不同房间。
func (s *Store) ClaimTeamUnits(ctx context.Context, formation *pb.MatchFormationRecord, group []*pb.MatchAuthorityTicketRecord, units []*teampb.TeamLaunchRecord) error {
	target := uint32(len(group))
	maxPlayers := config.TeamCapacity
	if formation.Mode == uint32(pb.MatchMode_MATCH_MODE_PVP_TEAM) {
		target = uint32(len(group) / 2)
		maxPlayers *= 2
	}
	if len(group) == 0 || len(group) > maxPlayers || !validTeamQueueMode(formation.Mode) {
		return ErrDirectConflict
	}
	if err := validatePvpTeamFormation(formation, group); err != nil {
		return err
	}
	if _, err := TeamUnitHomeZone(group); err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	originals := make([]*teampb.TeamOriginalRole, 0, len(group))
	for _, ticket := range group {
		originals = append(originals, &teampb.TeamOriginalRole{PlayerId: ticket.PlayerId, OwnerAccount: ticket.OwnerAccount, OwnerRevision: ticket.OwnerRevision})
	}
	if err = s.LockTeamOriginals(ctx, tx, originals); err != nil {
		return err
	}
	entry := &matchEntryOriginal{teams: units, battleID: formation.BattleId}
	if err = s.requireMatchEntryTx(ctx, tx, group, entry); err != nil {
		return err
	}
	players := make([]uint64, 0, len(group))
	for _, ticket := range group {
		players = append(players, ticket.PlayerId)
	}
	if err = s.RequireNoSpectators(ctx, tx, players); err != nil {
		return err
	}
	// 逐人核对票上的 home 仍是当前 home；跨 home 成局由 claimTx 里的范围树双向同意决定，
	// 不再要求补位后的整组同 home（负责人 2026-10-24 D2，覆盖 team-system.md:2539 对匹配组的适用范围）。
	if err = s.requireTicketHomesTx(ctx, tx, group); err != nil {
		return err
	}
	ordered := append([]*teampb.TeamLaunchRecord(nil), units...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].TeamId < ordered[j].TeamId })
	for index, unit := range ordered {
		if index > 0 && ordered[index-1].TeamId == unit.TeamId {
			return ErrDirectConflict
		}
		var version uint64
		if err = tx.QueryRowContext(ctx, "SELECT version FROM team_authority WHERE team_id=? FOR UPDATE", unit.TeamId).Scan(&version); err != nil {
			return err
		}
	}
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.ActorId != b.ActorId {
			return a.ActorId < b.ActorId
		}
		if a.RequestHigh != b.RequestHigh {
			return a.RequestHigh < b.RequestHigh
		}
		return a.RequestLow < b.RequestLow
	})
	covered := map[uint64]bool{}
	for _, unit := range ordered {
		current, team, version, err := s.lockTeamLaunch(ctx, tx, unit)
		if err != nil {
			return err
		}
		if current.State != 2 || current.ConfigId != formation.ConfigId || current.TargetSize != target || TeamLaunchMode(current) != formation.Mode || version == math.MaxUint64 {
			return ErrDirectConflict
		}
		data, err := DecodeTeamLaunch(current)
		if err != nil {
			return err
		}
		for _, member := range data.Members {
			found := false
			for _, ticket := range group {
				if ticket.PlayerId == member.Original.PlayerId && ticket.TeamId == current.TeamId && ticket.Ticket == member.Ticket && ticket.OwnerAccount == member.Original.OwnerAccount && ticket.OwnerRevision == member.Original.OwnerRevision && ticket.ZoneId == data.Original.Zones[ticket.PlayerId] && !covered[ticket.PlayerId] {
					covered[ticket.PlayerId] = true
					found = true
					break
				}
			}
			if !found {
				return ErrOriginalPlayer
			}
		}
		if team.Record.MatchLockToken != "" {
			return ErrTeamLaunchActive
		}
		team.Record.MatchLockToken = current.LockToken
		team.Record.MatchLockExpireAtMs = 0
		team.Record.MatchLockRoster = nil
		for _, member := range data.Members {
			team.Record.MatchLockRoster = append(team.Record.MatchLockRoster, member.Original.PlayerId)
		}
		payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(team)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE team_authority SET version=?,payload=? WHERE team_id=? AND version=?", version+1, payload, current.TeamId, version); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE team_launch SET state=3,battle_id=? WHERE actor_id=? AND request_high=? AND request_low=? AND state=2", formation.BattleId, current.ActorId, current.RequestHigh, current.RequestLow); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE team_launch_role SET state=3 WHERE actor_id=? AND request_high=? AND request_low=?", current.ActorId, current.RequestHigh, current.RequestLow); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM team_launch_region WHERE actor_id=? AND request_high=? AND request_low=?", current.ActorId, current.RequestHigh, current.RequestLow); err != nil {
			return err
		}
	}
	for _, ticket := range group {
		if ticket.TeamId != 0 && !covered[ticket.PlayerId] {
			return errors.New("team: 队伍票缺完整原unit")
		}
	}
	// 队伍失败不拆队重排，但这不能关闭逐人等待围栏（与requeue策略无关）。
	check := append([]*pb.MatchAuthorityTicketRecord(nil), group...)
	sort.Slice(check, func(i, j int) bool { return check[i].PlayerId < check[j].PlayerId })
	stamp, err := now(ctx, tx)
	if err != nil {
		return err
	}
	var actual []*pb.MatchAuthorityTicketRecord
	allLocal := true
	for _, want := range check {
		current, readErr := scanTicket(tx.QueryRowContext(ctx, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE player_id=? FOR UPDATE", want.PlayerId))
		if readErr != nil {
			return readErr
		}
		if current.ZoneId != want.ZoneId || current.TeamId != want.TeamId || current.Mode != formation.Mode || current.ConfigId != formation.ConfigId || current.TeamSize != target {
			return ErrDirectConflict
		}
		profile, decodeErr := DecodeProfile(current.NetworkProfile)
		if decodeErr != nil {
			return decodeErr
		}
		allLocal = allLocal && PreferredRegion(profile, stamp) == formation.PhysicalRegion
		actual = append(actual, current)
	}
	if !allLocal && !singleUnit(actual) {
		wait := uint64(s.Config.CrossRegionWaitSeconds) * 1000
		for _, ticket := range actual {
			if stamp < wait || ticket.EnqueuedMs > stamp-wait {
				return errors.New("team: 跨区须全员原等待达到阈值")
			}
		}
	}
	// F5只恢复原票；整队region仍为空，必须由原launch事务整单恢复后才可再次撮合。
	// 单排同时沿原规则重排，不因本局包含party而被静默退出。
	formation.RequeueOnFailure = true
	if err = s.claimTx(ctx, tx, formation, group, entry); err != nil {
		return err
	}
	return tx.Commit()
}
