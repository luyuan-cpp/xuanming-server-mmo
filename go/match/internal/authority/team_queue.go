package authority

import (
	"context"
	"database/sql"
	"errors"
	"match/internal/config"
	"math"
	"sort"

	pb "proto/match"
	teampb "proto/team"
)

var ErrTeamLaunchActive = errors.New("team: 原整队成局尚未确认结束")

// 调用方已按全员P→Team→launch加锁；所有占用、整队unit与候选投影同tx可见。
func (s *Store) EnqueueTeamLaunchTx(ctx context.Context, tx *sql.Tx, launch *teampb.TeamLaunchRecord) error {
	data, err := DecodeTeamLaunch(launch)
	if err != nil {
		return err
	}
	mode := TeamLaunchMode(launch)
	if launch.State != 1 || !validTeamQueueMode(mode) || launch.TargetSize < launch.MemberCount || (launch.TargetSize == launch.MemberCount && mode != uint32(pb.MatchMode_MATCH_MODE_PVP_TEAM)) || launch.TargetSize > config.TeamCapacity {
		return ErrDirectConflict
	}
	entryGroup := make([]*pb.MatchAuthorityTicketRecord, 0, len(data.Members))
	for _, member := range data.Members {
		entryGroup = append(entryGroup, &pb.MatchAuthorityTicketRecord{PlayerId: member.Original.PlayerId, Ticket: member.Ticket, TeamId: launch.TeamId, OwnerAccount: member.Original.OwnerAccount, OwnerRevision: member.Original.OwnerRevision})
	}
	if err = s.requireMatchEntryTx(ctx, tx, entryGroup, &matchEntryOriginal{teams: []*teampb.TeamLaunchRecord{launch}}); err != nil {
		return err
	}
	if err = s.RequireSpectatorExitsClosedTx(ctx, tx, data.SpectatorExits); err != nil {
		return err
	}
	stamp, err := now(ctx, tx)
	if err != nil {
		return err
	}
	members := append([]*teampb.TeamLaunchMember(nil), data.Members...)
	sort.Slice(members, func(i, j int) bool { return members[i].Original.PlayerId < members[j].Original.PlayerId })
	var group []*pb.MatchAuthorityTicketRecord
	for _, member := range members {
		player := member.Original.PlayerId
		if _, err = tx.ExecContext(ctx, "INSERT IGNORE INTO match_authority_ticket("+ticketColumns+") VALUES(?,'','',0,0,0,0,0,0,0,0,0,0,?,0,0,0,'',0)", player, []byte{}); err != nil {
			return err
		}
		old, err := scanTicket(tx.QueryRowContext(ctx, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE player_id=? FOR UPDATE", player))
		if err != nil {
			return err
		}
		if TicketOccupied(old) {
			return errors.New("team: 成员已有在途票")
		}
		var payload []byte
		var expiry uint64
		if err = tx.QueryRowContext(ctx, "SELECT payload,expires_ms FROM match_network_profile WHERE player_id=?", player).Scan(&payload, &expiry); err != nil {
			return err
		}
		profile, err := DecodeProfile(payload)
		if err != nil {
			return err
		}
		if profile.OwnerAccount != member.Original.OwnerAccount || profile.OwnerRevision != member.Original.OwnerRevision {
			return ErrOriginalPlayer
		}
		if expiry <= stamp || len(Latencies(profile, stamp, s.Config.HardLatencyMs)) == 0 {
			return errors.New("team: 原成员地域测量已失效")
		}
		ticket := &pb.MatchAuthorityTicketRecord{PlayerId: player, Ticket: member.Ticket, RequestId: data.Original.Request.RequestId, Mode: mode, ConfigId: launch.ConfigId, TeamSize: launch.TargetSize, State: 1, EnqueuedMs: launch.EnqueuedMs, NotBeforeMs: launch.NotBeforeMs, ZoneId: data.Original.Zones[player], TeamId: launch.TeamId, NetworkProfile: payload, ExpiresMs: expiry, OwnerAccount: member.Original.OwnerAccount, OwnerRevision: member.Original.OwnerRevision}
		if _, err = tx.ExecContext(ctx, "UPDATE match_authority_ticket SET ticket=?,request_id=?,mode=?,config_id=?,team_size=?,state=1,enqueued_ms=?,not_before_ms=?,zone_id=?,rating_centi=0,formation_id=0,team_id=?,network_profile=?,expires_ms=?,selected_region=0,owner_account=?,owner_revision=? WHERE player_id=?", ticket.Ticket, ticket.RequestId, ticket.Mode, ticket.ConfigId, ticket.TeamSize, ticket.EnqueuedMs, ticket.NotBeforeMs, ticket.ZoneId, ticket.TeamId, ticket.NetworkProfile, ticket.ExpiresMs, ticket.OwnerAccount, ticket.OwnerRevision, player); err != nil {
			return err
		}
		if err = s.replaceTicketRegions(ctx, tx, ticket, stamp); err != nil {
			return err
		}
		group = append(group, ticket)
	}
	if err = s.replaceTeamLaunchRegions(ctx, tx, launch, group, stamp); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE team_launch SET state=2 WHERE actor_id=? AND request_high=? AND request_low=? AND state=1", launch.ActorId, launch.RequestHigh, launch.RequestLow)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE team_launch_role SET state=2 WHERE actor_id=? AND request_high=? AND request_low=?", launch.ActorId, launch.RequestHigh, launch.RequestLow)
	return err
}

func (s *Store) replaceTeamLaunchRegions(ctx context.Context, tx *sql.Tx, launch *teampb.TeamLaunchRecord, group []*pb.MatchAuthorityTicketRecord, stamp uint64) error {
	data, err := DecodeTeamLaunch(launch)
	if err != nil {
		return err
	}
	if len(group) != len(data.Members) {
		return ErrDirectConflict
	}
	for _, ticket := range group {
		if ticket.ZoneId != data.Original.Zones[ticket.PlayerId] {
			return ErrTeamRoute
		}
	}
	if err = s.requireTeamHomeGroupTx(ctx, tx, group); err != nil {
		return err
	}
	home, err := TeamUnitHomeZone(group)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM team_launch_region WHERE actor_id=? AND request_high=? AND request_low=?", launch.ActorId, launch.RequestHigh, launch.RequestLow); err != nil {
		return err
	}
	common := map[uint32]*teampb.TeamLaunchRegionRecord{}
	for index, ticket := range group {
		profile, err := DecodeProfile(ticket.NetworkProfile)
		if err != nil {
			return err
		}
		latencies := Latencies(profile, stamp, math.MaxUint32)
		preferred := PreferredRegion(profile, stamp)
		if index == 0 {
			for region, rtt := range latencies {
				common[region] = &teampb.TeamLaunchRegionRecord{ActorId: launch.ActorId, RequestHigh: launch.RequestHigh, RequestLow: launch.RequestLow, PhysicalRegion: region, ConfigId: launch.ConfigId, TargetSize: launch.TargetSize, MemberCount: launch.MemberCount, LocalPreferred: preferred == region, EnqueuedMs: launch.EnqueuedMs, ExpiresMs: ticket.ExpiresMs, MaxRttMs: rtt, HomeZone: home, Mode: TeamLaunchMode(launch)}
			}
		} else {
			for region, row := range common {
				rtt, ok := latencies[region]
				if !ok {
					delete(common, region)
					continue
				}
				row.LocalPreferred = row.LocalPreferred && preferred == region
				row.MaxRttMs = max(row.MaxRttMs, rtt)
				row.ExpiresMs = min(row.ExpiresMs, ticket.ExpiresMs)
			}
		}
	}
	regions := make([]uint32, 0, len(common))
	for region := range common {
		regions = append(regions, region)
	}
	sort.Slice(regions, func(i, j int) bool { return regions[i] < regions[j] })
	for _, region := range regions {
		row := common[region]
		if _, err := tx.ExecContext(ctx, "INSERT INTO team_launch_region(actor_id,request_high,request_low,physical_region,config_id,target_size,member_count,local_preferred,enqueued_ms,expires_ms,max_rtt_ms,home_zone,mode) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)", row.ActorId, row.RequestHigh, row.RequestLow, row.PhysicalRegion, row.ConfigId, row.TargetSize, row.MemberCount, row.LocalPreferred, row.EnqueuedMs, row.ExpiresMs, row.MaxRttMs, row.HomeZone, row.Mode); err != nil {
			return err
		}
	}
	return nil
}

// 名册/队长变化在Team同tx关闭旧queued整单。forming/UNKNOWN禁止按TTL猜结束。
func (s *Store) CancelTeamQueuedTx(ctx context.Context, tx *sql.Tx, teamID uint64) error {
	rows, err := tx.QueryContext(ctx, "SELECT "+teamLaunchColumns+" FROM team_launch WHERE team_id=? AND state IN(1,2,3) ORDER BY actor_id,request_high,request_low FOR UPDATE", teamID)
	if err != nil {
		return err
	}
	var launches []*teampb.TeamLaunchRecord
	for rows.Next() {
		launch, err := scanTeamLaunch(rows)
		if err != nil {
			rows.Close()
			return err
		}
		launches = append(launches, launch)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, launch := range launches {
		if launch.State != 2 {
			return ErrTeamLaunchActive
		}
		if err = s.closeQueuedTeamLaunchTx(ctx, tx, launch); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) closeQueuedTeamLaunchTx(ctx context.Context, tx *sql.Tx, launch *teampb.TeamLaunchRecord) error {
	data, err := DecodeTeamLaunch(launch)
	if err != nil {
		return err
	}
	members := append([]*teampb.TeamLaunchMember(nil), data.Members...)
	sort.Slice(members, func(i, j int) bool { return members[i].Original.PlayerId < members[j].Original.PlayerId })
	for _, member := range members {
		if _, err = tx.ExecContext(ctx, "UPDATE match_authority_ticket SET state=4 WHERE player_id=? AND ticket=? AND team_id=? AND formation_id=0 AND state=1", member.Original.PlayerId, member.Ticket, launch.TeamId); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM match_ticket_region WHERE player_id=? AND ticket=?", member.Original.PlayerId, member.Ticket); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM team_launch_region WHERE actor_id=? AND request_high=? AND request_low=?", launch.ActorId, launch.RequestHigh, launch.RequestLow); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE team_launch SET state=5 WHERE actor_id=? AND request_high=? AND request_low=? AND state=2", launch.ActorId, launch.RequestHigh, launch.RequestLow)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE team_launch_role SET state=5 WHERE actor_id=? AND request_high=? AND request_low=?", launch.ActorId, launch.RequestHigh, launch.RequestLow)
	return err
}

// 只作预检，真正占用仍在全员P→Team→票事务里复核。
func (s *Store) TeamMemberHasTicket(ctx context.Context, player uint64) (bool, error) {
	ticket, err := scanTicket(s.DB.QueryRowContext(ctx, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE player_id=?", player))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return TicketOccupied(ticket), err
}
