package team

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"sort"

	teampb "proto/team"

	"google.golang.org/protobuf/proto"
)

func consentByPlayer(consents []*teampb.TeamConsentOriginal, player uint64) *teampb.TeamConsentOriginal {
	for _, consent := range consents {
		if consent.GetPlayerId() == player {
			return consent
		}
	}
	return nil
}

func originalByPlayer(originals []*teampb.TeamOriginalRole, player uint64) *teampb.TeamOriginalRole {
	for _, original := range originals {
		if original.GetPlayerId() == player {
			return original
		}
	}
	return nil
}

func mutationOriginals(data *teampb.TeamAuthorityData, op Op) ([]*teampb.TeamOriginalRole, error) {
	originals := append([]*teampb.TeamOriginalRole(nil), data.GetMembers()...)
	if len(originals) != len(data.GetRecord().GetMembers()) {
		return nil, errors.New("team: 原成员身份缺失")
	}
	for _, member := range data.GetRecord().GetMembers() {
		if originalByPlayer(originals, member.GetPlayerId()) == nil {
			return nil, errors.New("team: 原成员身份与名册不一致")
		}
	}
	var consent *teampb.TeamConsentOriginal
	if op.Kind == OpHandleApplication {
		consent = consentByPlayer(data.GetApplications(), op.Target)
	}
	if op.Kind == OpRespondInvite {
		consent = consentByPlayer(data.GetInvites(), op.Caller)
	}
	if consent != nil {
		originals = append(originals, consent.Initiator, consent.Recipient)
	}
	return originals, nil
}

func sameSQLTeam(a, b *sqlTeamRow) bool {
	return a.version == b.version && a.state == b.state && proto.Equal(a.data, b.data)
}

func (s *sqlTeamStore) mutate(ctx context.Context, store *Store, bind Binding, op Op) (*MutateResult, error) {
	if op.Kind != OpRefresh {
		action, ok := ctx.Value(sqlActionKey{}).(*sqlAction)
		if !ok || action.method != uint32(op.Kind) || action.actor.GetPlayerId() != op.Caller {
			return nil, errors.New("team: 名册写缺少稳定原操作意图")
		}
	}
	for attempt := 0; attempt < CommitRetries; attempt++ {
		observed, err := loadSQLTeam(ctx, s.authority.DB, bind.teamId, false)
		if err != nil {
			return nil, err
		}
		originals, err := mutationOriginals(observed.data, op)
		if err != nil {
			return nil, err
		}
		sessions := store.loadSessions(ctx, observed.data.Record)
		tx, err := s.authority.DB.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		result, retry, err := s.mutateTx(ctx, tx, bind, op, observed, originals, sessions, store.cfg)
		if err != nil || retry {
			tx.Rollback()
			if err != nil {
				return nil, err
			}
			continue
		}
		if err = tx.Commit(); err != nil {
			return nil, err // 原UUID回执与业务在同tx；未知结果只能原请求查询/重试。
		}
		result.Conflicts = attempt
		return result, nil
	}
	return &MutateResult{Outcome: OutcomeRejected, Code: ErrStateChanged, Conflicts: CommitRetries}, nil
}

func (s *sqlTeamStore) mutateTx(ctx context.Context, tx *sql.Tx, bind Binding, op Op, observed *sqlTeamRow, originals []*teampb.TeamOriginalRole, sessions Sessions, cfg RuleConfig) (*MutateResult, bool, error) {
	var newTarget uint64
	if op.Kind == OpInvite {
		newTarget = op.Target
	}
	locked, err := s.authority.CaptureTeamOriginals(ctx, tx, op.Caller, originals, newTarget)
	if err != nil {
		return nil, false, err
	}
	if op.Kind == OpCreate || op.Kind == OpApply {
		if err = s.authority.RequireTeamHomeZoneTx(ctx, tx, op.Caller, op.CallerZone); err != nil {
			return nil, false, err
		}
	}
	if op.Kind == OpInvite {
		if err = s.authority.RequireTeamHomeZoneTx(ctx, tx, op.Target, op.TargetZone); err != nil {
			return nil, false, err
		}
	}
	action, _ := ctx.Value(sqlActionKey{}).(*sqlAction)
	if action != nil {
		previous, err := loadSQLAction(ctx, tx, action)
		if err != nil {
			return nil, false, err
		}
		if previous != nil {
			action.committedResponse, action.replayed = previous, true
			return &MutateResult{Outcome: OutcomeUnchanged}, false, nil
		}
	}
	row, err := loadSQLTeam(ctx, tx, bind.teamId, true)
	if err != nil {
		return nil, false, err
	}
	if !sameSQLTeam(observed, row) {
		return nil, true, nil
	}
	stamp, err := teamSQLNow(ctx, tx)
	if err != nil {
		return nil, false, err
	}
	index, err := loadSQLMembership(ctx, tx, bind.playerId, true)
	if err != nil {
		return nil, false, err
	}
	result := &MutateResult{Snapshot: *sqlTeamSnapshot(bind.playerId, bind.teamId, stamp, index, row)}
	switch {
	case bind.mode == bindModeCaller && (bind.teamId == 0 || index.TeamId != bind.teamId):
		result.Outcome = OutcomeNotBound
	case bind.mode == bindModeCreate && row.version != 0:
		result.Outcome, result.Code = OutcomeRejected, ErrStateChanged
	case bind.mode != bindModeCreate && row.state != 1:
		result.Outcome = OutcomeRecordMissing
	default:
		op.CallerTeamId = index.TeamId
		if op.Kind == OpInvite {
			targetIndex, readErr := loadSQLMembership(ctx, tx, op.Target, true)
			if readErr != nil {
				return nil, false, readErr
			}
			if targetIndex.TeamId != 0 {
				result.Outcome, result.Code, result.CodeParam = OutcomeRejected, ErrMemberInTeam, op.Target
				break
			}
		}
		cfg.Zones = s.authority.MergedZones()
		decision := Apply(op, row.data.Record, stamp, sessions, cfg)
		result.Decision = decision
		if decision.Code != 0 {
			result.Outcome, result.Code, result.CodeParam = OutcomeRejected, decision.Code, decision.CodeParam
		} else if !decision.Changed {
			result.Outcome = OutcomeUnchanged
		} else {
			data, err := nextSQLTeamData(row.data, decision, op, locked)
			if err != nil {
				return nil, false, err
			}
			commit, code, param, err := s.commitDecision(ctx, tx, bind.teamId, row, data, decision, stamp)
			if err != nil {
				return nil, false, err
			}
			if code != 0 {
				result.Outcome, result.Code, result.CodeParam = OutcomeRejected, code, param
			} else {
				result.Outcome, result.Commit = OutcomeCommitted, commit
			}
		}
	}
	response := sqlMutationResponse(bind.playerId, op.Kind, result)
	if err = writeSQLAction(ctx, tx, action, response, stamp); err != nil {
		return nil, false, err
	}
	return result, false, nil
}

func nextSQLTeamData(old *teampb.TeamAuthorityData, decision Decision, op Op, originals []*teampb.TeamOriginalRole) (*teampb.TeamAuthorityData, error) {
	data := &teampb.TeamAuthorityData{Record: decision.Record}
	if decision.Record == nil {
		return data, nil
	}
	for _, member := range decision.Record.Members {
		original := originalByPlayer(originals, member.PlayerId)
		if original == nil {
			return nil, errors.New("team: 新成员缺少原身份授权")
		}
		data.Members = append(data.Members, original)
	}
	for _, application := range decision.Record.Applications {
		consent := consentByPlayer(old.Applications, application.PlayerId)
		if op.Kind == OpApply && application.PlayerId == op.Caller {
			consent = &teampb.TeamConsentOriginal{PlayerId: op.Caller, Initiator: originalByPlayer(originals, op.Caller), Recipient: originalByPlayer(originals, decision.Record.LeaderId)}
		}
		if consent == nil || consent.Initiator == nil || consent.Recipient == nil {
			return nil, errors.New("team: 申请原身份缺失")
		}
		data.Applications = append(data.Applications, consent)
	}
	for _, invitation := range decision.Record.Invites {
		consent := consentByPlayer(old.Invites, invitation.InviteeId)
		if op.Kind == OpInvite && invitation.InviteeId == op.Target {
			consent = &teampb.TeamConsentOriginal{PlayerId: op.Target, Initiator: originalByPlayer(originals, op.Caller), Recipient: originalByPlayer(originals, op.Target)}
		}
		if consent == nil || consent.Initiator == nil || consent.Recipient == nil {
			return nil, errors.New("team: 邀请原身份缺失")
		}
		data.Invites = append(data.Invites, consent)
	}
	return data, nil
}

// sqlMutationResponse 组回执里存的回包。视图的区留原始区(zones 传 nil):回执要存很久,映射会变,
// 给客户端之前由 decorateSQLReplay 按当时的映射解析。
func sqlMutationResponse(caller uint64, kind OpKind, result *MutateResult) *teampb.TeamResponse {
	code := result.Code
	if result.Outcome == OutcomeNotBound || result.Outcome == OutcomeRecordMissing {
		code = ErrNoTeam
		if kind == OpLeave {
			code = 0
		}
	}
	if result.Commit != nil {
		view, _ := viewFromCommit(result.Commit, caller, nil, nil)
		return &teampb.TeamResponse{ErrorMessage: tipOf(code, result.CodeParam), Team: view}
	}
	view, _ := viewFromSnapshot(&result.Snapshot, nil, nil)
	return &teampb.TeamResponse{ErrorMessage: tipOf(code, result.CodeParam), Team: view}
}

func (s *sqlTeamStore) commitDecision(ctx context.Context, tx *sql.Tx, teamID uint64, old *sqlTeamRow, data *teampb.TeamAuthorityData, decision Decision, stamp uint64) (*CommitResult, uint32, uint64, error) {
	if err := validateCommitSets(teamID, decision); err != nil {
		return nil, 0, 0, err
	}
	if old.version == math.MaxUint64 {
		return nil, 0, 0, errors.New("team: 队伍版本耗尽")
	}
	people := append(append(append([]uint64(nil), decision.Joined...), decision.Kept...), decision.Left...)
	sort.Slice(people, func(i, j int) bool { return people[i] < people[j] })
	indexes := make(map[uint64]IndexEntry, len(people))
	joined, left := map[uint64]bool{}, map[uint64]bool{}
	for _, player := range decision.Joined {
		joined[player] = true
	}
	for _, player := range decision.Left {
		left[player] = true
	}
	for _, player := range people {
		index, err := loadSQLMembership(ctx, tx, player, true)
		if err != nil {
			return nil, 0, 0, err
		}
		if joined[player] && index.TeamId != 0 && index.TeamId != teamID {
			return nil, ErrMemberInTeam, player, nil
		}
		if !joined[player] && index.TeamId != teamID {
			return nil, 0, 0, errors.New("team: SQL成员原索引不一致")
		}
		if joined[player] || left[player] {
			if index.Epoch == math.MaxUint64 {
				return nil, 0, 0, errors.New("team: 成员代次耗尽")
			}
			index.Epoch++
			if index.Epoch < stamp {
				index.Epoch = stamp
			}
			index.TeamId = teamID
			if left[player] {
				index.TeamId = 0
			}
		}
		indexes[player] = index
	}
	// 新增邀请均持被邀请人P，使用当前读计数，避免事务快照早于等待P锁时漏算。
	for _, invite := range decision.InvitesAdded {
		rows, err := tx.QueryContext(ctx, "SELECT team_id FROM team_invite_index WHERE invitee_id=? AND expires_ms>? AND team_id<>? FOR UPDATE", invite.InviteeId, stamp, teamID)
		if err != nil {
			return nil, 0, 0, err
		}
		count := 0
		for rows.Next() {
			var id uint64
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return nil, 0, 0, err
			}
			count++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, 0, 0, err
		}
		if count >= MaxPendingInvitesPerInvitee {
			return nil, ErrInviteLimit, invite.InviteeId, nil
		}
	}
	if old.state == 1 && (len(decision.Joined)+len(decision.Left) != 0 || old.data.GetRecord().GetLeaderId() != decision.Record.GetLeaderId()) {
		if err := s.authority.CancelTeamQueuedTx(ctx, tx, teamID); err != nil {
			return nil, 0, 0, err
		}
	}
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(data)
	if err != nil {
		return nil, 0, 0, err
	}
	state := uint32(1)
	if decision.Record == nil {
		state = 2
	}
	version := old.version + 1
	if old.version == 0 {
		_, err = tx.ExecContext(ctx, "INSERT INTO team_authority(team_id,version,payload,state,idle_expires_ms) VALUES(?,?,?,?,?)", teamID, version, payload, state, stamp+TeamIdleTTLSeconds*1000)
	} else {
		_, err = tx.ExecContext(ctx, "UPDATE team_authority SET version=?,payload=?,state=?,idle_expires_ms=? WHERE team_id=? AND version=?", version, payload, state, stamp+TeamIdleTTLSeconds*1000, teamID, old.version)
	}
	if err != nil {
		return nil, 0, 0, err
	}
	for _, player := range people {
		index := indexes[player]
		if _, err = tx.ExecContext(ctx, "INSERT INTO team_membership(player_id,team_id,epoch) VALUES(?,?,?) ON DUPLICATE KEY UPDATE team_id=VALUES(team_id),epoch=VALUES(epoch)", player, index.TeamId, index.Epoch); err != nil {
			return nil, 0, 0, err
		}
	}
	for _, player := range decision.InvitesRemoved {
		if _, err = tx.ExecContext(ctx, "DELETE FROM team_invite_index WHERE invitee_id=? AND team_id=?", player, teamID); err != nil {
			return nil, 0, 0, err
		}
	}
	for _, invite := range decision.InvitesAdded {
		if _, err = tx.ExecContext(ctx, "INSERT INTO team_invite_index(invitee_id,team_id,expires_ms) VALUES(?,?,?) ON DUPLICATE KEY UPDATE expires_ms=VALUES(expires_ms)", invite.InviteeId, teamID, invite.ExpireAtMs); err != nil {
			return nil, 0, 0, err
		}
	}
	applications := map[uint64]uint64{}
	for _, application := range decision.Record.GetApplications() {
		applications[application.PlayerId] = application.ExpireAtMs
	}
	for _, application := range old.data.GetRecord().GetApplications() {
		if _, ok := applications[application.PlayerId]; !ok {
			if _, err = tx.ExecContext(ctx, "DELETE FROM team_application_index WHERE applicant_id=? AND team_id=?", application.PlayerId, teamID); err != nil {
				return nil, 0, 0, err
			}
		}
	}
	for player, expiry := range applications {
		if _, err = tx.ExecContext(ctx, "INSERT INTO team_application_index(applicant_id,team_id,expires_ms) VALUES(?,?,?) ON DUPLICATE KEY UPDATE expires_ms=VALUES(expires_ms)", player, teamID, expiry); err != nil {
			return nil, 0, 0, err
		}
	}
	return &CommitResult{TeamId: teamID, Version: version, Decision: decision, Indexes: indexes, NowMs: stamp}, 0, 0, nil
}
