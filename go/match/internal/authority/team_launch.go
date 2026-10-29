package authority

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"match/internal/config"
	"sort"

	pb "proto/match"
	teampb "proto/team"

	"google.golang.org/protobuf/proto"
)

const teamLaunchColumns = "actor_id,request_high,request_low,team_id,team_version,lock_token,config_id,target_size,member_count,state,enqueued_ms,not_before_ms,battle_id,payload,owner_account,owner_revision,mode"

func scanTeamLaunch(row scanner) (*teampb.TeamLaunchRecord, error) {
	launch := &teampb.TeamLaunchRecord{}
	err := row.Scan(&launch.ActorId, &launch.RequestHigh, &launch.RequestLow, &launch.TeamId, &launch.TeamVersion, &launch.LockToken, &launch.ConfigId, &launch.TargetSize, &launch.MemberCount, &launch.State, &launch.EnqueuedMs, &launch.NotBeforeMs, &launch.BattleId, &launch.Payload, &launch.OwnerAccount, &launch.OwnerRevision, &launch.Mode)
	return launch, err
}

func (s *Store) TeamLaunchTx(ctx context.Context, tx *sql.Tx, actor, high, low uint64) (*teampb.TeamLaunchRecord, error) {
	launch, err := scanTeamLaunch(tx.QueryRowContext(ctx, "SELECT "+teamLaunchColumns+" FROM team_launch WHERE actor_id=? AND request_high=? AND request_low=? FOR UPDATE", actor, high, low))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return launch, err
}

func (s *Store) InsertTeamLaunchTx(ctx context.Context, tx *sql.Tx, launch *teampb.TeamLaunchRecord) error {
	data, err := DecodeTeamLaunch(launch)
	if err != nil {
		return err
	}
	if err = s.requireTeamLaunchHomesTx(ctx, tx, data); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO team_launch("+teamLaunchColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", launch.ActorId, launch.RequestHigh, launch.RequestLow, launch.TeamId, launch.TeamVersion, launch.LockToken, launch.ConfigId, launch.TargetSize, launch.MemberCount, launch.State, launch.EnqueuedMs, launch.NotBeforeMs, launch.BattleId, launch.Payload, launch.OwnerAccount, launch.OwnerRevision, launch.Mode)
	if err != nil {
		return err
	}
	for _, member := range data.Members {
		if _, err = tx.ExecContext(ctx, "INSERT INTO team_launch_role(actor_id,request_high,request_low,player_id,state) VALUES(?,?,?,?,?)", launch.ActorId, launch.RequestHigh, launch.RequestLow, member.Original.PlayerId, launch.State); err != nil {
			return err
		}
	}
	return err
}

func DecodeTeamLaunch(launch *teampb.TeamLaunchRecord) (*teampb.TeamLaunchData, error) {
	data := &teampb.TeamLaunchData{}
	if launch == nil || proto.Unmarshal(launch.Payload, data) != nil || data.Original == nil || len(data.Members) == 0 || len(data.Members) > config.TeamCapacity || uint32(len(data.Members)) != launch.MemberCount || !proto.Equal(&teampb.TeamLaunchData{Members: data.Members}, &teampb.TeamLaunchData{Members: data.Original.Members}) {
		return nil, errors.New("team: 开战原件损坏")
	}
	original := data.Original
	mode := uint32(original.GetRequest().GetMatchMode())
	if mode == 0 {
		mode = uint32(pb.MatchMode_MATCH_MODE_PVE_TEAM)
	}
	if !validTeamQueueMode(mode) || TeamLaunchMode(launch) != mode {
		return nil, errors.New("team: 开战模式与原意图不一致")
	}
	target := original.GetRequest().GetTargetTeamSize()
	if target == 0 {
		target = launch.MemberCount
	}
	if target != launch.TargetSize || target < launch.MemberCount || target > config.TeamCapacity {
		return nil, errors.New("team: 开战人数与原意图不一致")
	}
	request, err := proto.MarshalOptions{Deterministic: true}.Marshal(original.Request)
	if err != nil || !bytes.Equal(request, data.RequestPayload) || original.TeamId != launch.TeamId || original.TeamVersion != launch.TeamVersion || original.LockToken != launch.LockToken || original.Actor.GetPlayerId() != launch.ActorId || original.Actor.GetOwnerAccount() != launch.OwnerAccount || original.Actor.GetOwnerRevision() != launch.OwnerRevision || original.GetRequest().GetBattleConfigId() != launch.ConfigId || original.EnqueuedMs != launch.EnqueuedMs {
		return nil, errors.New("team: 开战原件投影不一致")
	}
	key, err := DirectUUIDKey(1, launch.ActorId, original.GetRequest().GetRequestId())
	if err != nil || key.RequestHigh != launch.RequestHigh || key.RequestLow != launch.RequestLow {
		return nil, ErrDirectConflict
	}
	sort.Slice(data.Members, func(i, j int) bool { return data.Members[i].RosterOrder < data.Members[j].RosterOrder })
	seen := map[uint64]bool{}
	for index, member := range data.Members {
		if member.RosterOrder != uint32(index+1) || member.Original.GetPlayerId() == 0 || seen[member.Original.PlayerId] || member.Ticket == "" {
			return nil, errors.New("team: 开战原名单/顺序无效")
		}
		seen[member.Original.PlayerId] = true
	}
	exitPlayers := map[uint64]bool{}
	for _, exit := range data.SpectatorExits {
		if validateSpectator(exit) != nil || exit.State != 3 || exitPlayers[exit.PlayerId] {
			return nil, errors.New("team: 原观战退出列表损坏")
		}
		found := false
		for _, member := range data.Members {
			if member.Original.PlayerId == exit.PlayerId && member.Original.OwnerAccount == exit.OwnerAccount && member.Original.OwnerRevision == exit.OwnerRevision {
				found = true
			}
		}
		if !found {
			return nil, ErrOriginalPlayer
		}
		exitPlayers[exit.PlayerId] = true
	}
	return data, nil
}

func teamLaunchOriginals(data *teampb.TeamLaunchData) []*teampb.TeamOriginalRole {
	out := make([]*teampb.TeamOriginalRole, 0, len(data.Members))
	for _, member := range data.Members {
		out = append(out, member.Original)
	}
	return out
}

func (s *Store) TeamLaunch(ctx context.Context, actor uint64, request string) (*teampb.TeamLaunchRecord, error) {
	key, err := DirectUUIDKey(1, actor, request)
	if err != nil {
		return nil, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = s.lockActor(ctx, tx, actor); err != nil {
		return nil, err
	}
	launch, err := scanTeamLaunch(tx.QueryRowContext(ctx, "SELECT "+teamLaunchColumns+" FROM team_launch WHERE actor_id=? AND request_high=? AND request_low=?", actor, key.RequestHigh, key.RequestLow))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	original := originalPlayer(ctx, actor)
	if launch.OwnerAccount != original.OwnerAccount || launch.OwnerRevision != original.OwnerRevision {
		return nil, ErrOriginalPlayer
	}
	if _, err = DecodeTeamLaunch(launch); err != nil {
		return nil, err
	}
	return launch, tx.Commit()
}

// Team行先于launch，所有成员P必须已在调用层升序持有。非名单申请/邀请变化可增版本，
// 但原队长、显式join_seq名单、原owner与开战token不可变化。
func (s *Store) lockTeamLaunch(ctx context.Context, tx *sql.Tx, observed *teampb.TeamLaunchRecord) (*teampb.TeamLaunchRecord, *teampb.TeamAuthorityData, uint64, error) {
	var version uint64
	var state uint32
	var payload []byte
	if err := tx.QueryRowContext(ctx, "SELECT version,payload,state FROM team_authority WHERE team_id=? FOR UPDATE", observed.TeamId).Scan(&version, &payload, &state); err != nil {
		return nil, nil, 0, err
	}
	team := &teampb.TeamAuthorityData{}
	if proto.Unmarshal(payload, team) != nil || state != 1 || version < observed.TeamVersion {
		return nil, nil, 0, errors.New("team: 原队伍已变化")
	}
	current, err := scanTeamLaunch(tx.QueryRowContext(ctx, "SELECT "+teamLaunchColumns+" FROM team_launch WHERE actor_id=? AND request_high=? AND request_low=? FOR UPDATE", observed.ActorId, observed.RequestHigh, observed.RequestLow))
	if err != nil {
		return nil, nil, 0, err
	}
	if !bytes.Equal(current.Payload, observed.Payload) || current.TeamId != observed.TeamId || current.TeamVersion != observed.TeamVersion || current.LockToken != observed.LockToken || current.OwnerAccount != observed.OwnerAccount || current.OwnerRevision != observed.OwnerRevision || TeamLaunchMode(current) != TeamLaunchMode(observed) {
		return nil, nil, 0, ErrDirectConflict
	}
	data, err := DecodeTeamLaunch(current)
	if err != nil {
		return nil, nil, 0, err
	}
	if !sameTeamLaunchRoster(team, data) {
		return nil, nil, 0, errors.New("team: 原队长/名册/角色身份已变化")
	}
	if current.State == 1 || current.State == 3 {
		if team.GetRecord().GetMatchLockToken() != current.LockToken {
			return nil, nil, 0, errors.New("team: 原开战token已变化")
		}
	}
	return current, team, version, nil
}

func sameTeamLaunchRoster(team *teampb.TeamAuthorityData, data *teampb.TeamLaunchData) bool {
	if team.GetRecord().GetLeaderId() != data.GetTeamRecord().GetLeaderId() || len(team.GetRecord().GetMembers()) != len(data.Members) || len(team.Members) != len(data.Members) {
		return false
	}
	for _, member := range data.Members {
		foundRole, foundMember := false, false
		for _, role := range team.Members {
			if proto.Equal(role, member.Original) {
				foundRole = true
			}
		}
		for _, current := range team.GetRecord().GetMembers() {
			if current.PlayerId == member.Original.PlayerId {
				for _, original := range data.GetTeamRecord().GetMembers() {
					if proto.Equal(current, original) {
						foundMember = true
					}
				}
			}
		}
		if !foundRole || !foundMember {
			return false
		}
	}
	return true
}

func (s *Store) ReserveTeamLaunch(ctx context.Context, observed *teampb.TeamLaunchRecord, formation *pb.MatchFormationRecord, group []*pb.MatchAuthorityTicketRecord) (*pb.MatchFormationRecord, error) {
	data, err := DecodeTeamLaunch(observed)
	if err != nil {
		return nil, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = s.LockTeamOriginals(ctx, tx, teamLaunchOriginals(data)); err != nil {
		return nil, err
	}
	players := make([]uint64, 0, len(data.Members))
	for _, member := range data.Members {
		players = append(players, member.Original.PlayerId)
	}
	if err = s.RequireNoSpectators(ctx, tx, players); err != nil {
		return nil, err
	}
	current, _, _, err := s.lockTeamLaunch(ctx, tx, observed)
	if err != nil {
		return nil, err
	}
	if current.BattleId != 0 {
		original, err := scanFormation(tx.QueryRowContext(ctx, "SELECT "+formationColumns+" FROM match_formation WHERE battle_id=?", current.BattleId))
		if err != nil {
			return nil, err
		}
		return original, tx.Commit()
	}
	if current.State != 1 || len(group) != len(data.Members) || formation.ConfigId != current.ConfigId || formation.Mode != uint32(pb.MatchMode_MATCH_MODE_PVE_TEAM) {
		return nil, ErrDirectConflict
	}
	if err = s.RequireSpectatorExitsClosedTx(ctx, tx, data.SpectatorExits); err != nil {
		return nil, err
	}
	if err = s.requireTeamHomeGroupTx(ctx, tx, group); err != nil {
		return nil, err
	}
	for _, member := range data.Members {
		found := false
		for _, ticket := range group {
			if ticket.PlayerId == member.Original.PlayerId && ticket.Ticket == member.Ticket && ticket.TeamId == current.TeamId && ticket.OwnerAccount == member.Original.OwnerAccount && ticket.OwnerRevision == member.Original.OwnerRevision && ticket.ZoneId == data.Original.Zones[ticket.PlayerId] {
				found = true
			}
		}
		if !found {
			return nil, ErrOriginalPlayer
		}
	}
	if _, err = tx.ExecContext(ctx, "INSERT IGNORE INTO match_direct_intent(kind,owner_id,request_high,request_low,payload,battle_id,cancelled,owner_account,owner_revision) VALUES(1,?,?,?,?,0,0,?,?)", current.ActorId, current.RequestHigh, current.RequestLow, current.Payload, current.OwnerAccount, current.OwnerRevision); err != nil {
		return nil, err
	}
	var payload []byte
	var battle uint64
	var cancelled bool
	if err = tx.QueryRowContext(ctx, "SELECT payload,battle_id,cancelled FROM match_direct_intent WHERE kind=1 AND owner_id=? AND request_high=? AND request_low=? FOR UPDATE", current.ActorId, current.RequestHigh, current.RequestLow).Scan(&payload, &battle, &cancelled); err != nil {
		return nil, err
	}
	if cancelled || !bytes.Equal(payload, current.Payload) {
		return nil, ErrDirectConflict
	}
	if battle != 0 {
		return nil, ErrDirectConflict
	}
	if err = s.reserveGroupTx(ctx, tx, formation, group, &matchEntryOriginal{teams: []*teampb.TeamLaunchRecord{current}, battleID: formation.BattleId}); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE match_direct_intent SET battle_id=? WHERE kind=1 AND owner_id=? AND request_high=? AND request_low=?", formation.BattleId, current.ActorId, current.RequestHigh, current.RequestLow); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE team_launch SET state=3,battle_id=? WHERE actor_id=? AND request_high=? AND request_low=? AND state=1", formation.BattleId, current.ActorId, current.RequestHigh, current.RequestLow); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE team_launch_role SET state=3 WHERE actor_id=? AND request_high=? AND request_low=?", current.ActorId, current.RequestHigh, current.RequestLow); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return s.Formation(ctx, formation.BattleId)
}
