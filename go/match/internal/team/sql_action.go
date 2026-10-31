package team

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"

	teampb "proto/team"
	"shared/generated/pb/table"
	"shared/roleidentity"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

type sqlActionKey struct{}
type sqlAction struct {
	actor             *teampb.TeamOriginalRole
	high, low         uint64
	method            uint32
	payload           []byte
	committedResponse *teampb.TeamResponse
	replayed          bool
}

var errTeamIntentConflict = errors.New("team: 原请求号已绑定其它操作/载荷")

func teamActor(ctx context.Context, caller uint64) (*teampb.TeamOriginalRole, error) {
	original, ok := roleidentity.Original(ctx)
	if !ok || original.Role.GetPlayerId() != caller {
		return nil, errors.New("team: 原会话角色身份缺失")
	}
	return &teampb.TeamOriginalRole{PlayerId: caller, OwnerAccount: original.Account, OwnerRevision: original.OwnerRevision}, nil
}

func (s *Service) sqlAction(ctx context.Context, caller uint64, method uint32, in proto.Message, call func(context.Context) *teampb.TeamResponse) *teampb.TeamResponse {
	request, ok := in.(interface{ GetRequestId() string })
	if !ok {
		return s.respond(ctx, caller, uint32(table.CommonError_kInvalidParameter), 0)
	}
	id, err := uuid.Parse(request.GetRequestId())
	if err != nil || id == uuid.Nil || id.String() != request.GetRequestId() {
		return s.respond(ctx, caller, uint32(table.CommonError_kInvalidParameter), 0)
	}
	actor, err := teamActor(ctx, caller)
	if err != nil {
		return s.respond(ctx, caller, ErrInternal, 0)
	}
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(in)
	if err != nil {
		return s.respond(ctx, caller, ErrInternal, 0)
	}
	action := &sqlAction{actor: actor, high: binary.BigEndian.Uint64(id[:8]), low: binary.BigEndian.Uint64(id[8:]), method: method, payload: payload}
	replay, err := s.store.sql.replayAction(ctx, action)
	if err != nil {
		code := ErrInternal
		if errors.Is(err, errTeamIntentConflict) {
			code = uint32(table.CommonError_kInvalidParameter)
		}
		return s.respond(ctx, caller, code, 0)
	}
	if replay != nil {
		return s.decorateSQLReplay(ctx, replay)
	}
	ctx = context.WithValue(ctx, sqlActionKey{}, action)
	response := call(ctx)
	if action.replayed {
		return s.decorateSQLReplay(ctx, action.committedResponse)
	}
	if action.committedResponse != nil {
		return s.decorateSQLReplay(ctx, action.committedResponse)
	}
	// 纯预检拒绝也绑定原请求，防止旧失败请求在名单变动后变成新意图。
	// 存储故障不固化为业务终态，客户端可持同UUID继续查询/重试。
	if response.GetErrorMessage().GetId() == ErrInternal || ctx.Err() != nil {
		return response
	}
	if err = s.store.sql.recordNoMutationAction(ctx, action, response); err != nil {
		return s.respond(ctx, caller, ErrInternal, 0)
	}
	if action.replayed {
		return s.decorateSQLReplay(ctx, action.committedResponse)
	}
	return response
}

func (s *Service) decorateSQLReplay(ctx context.Context, response *teampb.TeamResponse) *teampb.TeamResponse {
	if response == nil || response.Team == nil {
		return response
	}
	view := response.Team
	var players []uint64
	for _, member := range view.Members {
		players = append(players, member.PlayerId)
	}
	for _, application := range view.Applications {
		players = append(players, application.GetPlayer().GetPlayerId())
	}
	for _, invite := range view.PendingInvites {
		players = append(players, invite.GetInvitee().GetPlayerId())
	}
	display := s.presence.loadDisplay(ctx, uniqueIds(players))
	// 改过名册的回执存原始区(sqlMutationResponse),无变更回执存的是当时的视图。这里都按当前映射再解析一次,和其余视图出口同一口径;
	// 撤销合服之后重放无变更回执,回包可能显示旧的最终区,只影响显示。
	zones := s.mergedZones()
	view.ZoneId = zones.Final(view.ZoneId)
	for index, member := range view.Members {
		view.Members[index] = memberView(member.PlayerId, zones.Final(member.ZoneId), member.JoinSeq, view.LeaderId, display)
	}
	for _, application := range view.Applications {
		member := application.GetPlayer()
		application.Player = memberView(member.GetPlayerId(), zones.Final(member.GetZoneId()), member.GetJoinSeq(), view.LeaderId, display)
	}
	for _, invite := range view.PendingInvites {
		member := invite.GetInvitee()
		invite.Invitee = memberView(member.GetPlayerId(), zones.Final(member.GetZoneId()), member.GetJoinSeq(), view.LeaderId, display)
	}
	s.markFillSupported(view)
	if original, ok := roleidentity.Original(ctx); ok {
		s.decorateQueueState(ctx, original.Role.GetPlayerId(), view)
	}
	return response
}

func loadSQLAction(ctx context.Context, tx *sql.Tx, action *sqlAction) (*teampb.TeamResponse, error) {
	var method uint32
	var payload, result []byte
	var account string
	var revision uint64
	err := tx.QueryRowContext(ctx, "SELECT method,payload,owner_account,owner_revision,result FROM team_action_receipt WHERE actor_id=? AND request_high=? AND request_low=?", action.actor.PlayerId, action.high, action.low).Scan(&method, &payload, &account, &revision, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if method != action.method || !bytes.Equal(payload, action.payload) || account != action.actor.OwnerAccount || revision != action.actor.OwnerRevision {
		return nil, errTeamIntentConflict
	}
	response := &teampb.TeamResponse{}
	if err := proto.Unmarshal(result, response); err != nil {
		return nil, err
	}
	return response, nil
}

func writeSQLAction(ctx context.Context, tx *sql.Tx, action *sqlAction, response *teampb.TeamResponse, stamp uint64) error {
	if action == nil {
		return nil
	}
	// 回执保留业务结果与原版本；在线/战斗展示不作为持久事实。
	stable := proto.Clone(response).(*teampb.TeamResponse)
	for _, member := range stable.GetTeam().GetMembers() {
		member.IsOnline, member.InBattle = false, false
	}
	for _, application := range stable.GetTeam().GetApplications() {
		if application.Player != nil {
			application.Player.IsOnline, application.Player.InBattle = false, false
		}
	}
	for _, invitation := range stable.GetTeam().GetPendingInvites() {
		if invitation.Invitee != nil {
			invitation.Invitee.IsOnline, invitation.Invitee.InBattle = false, false
		}
	}
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(stable)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO team_action_receipt(actor_id,request_high,request_low,method,payload,owner_account,owner_revision,result,created_ms) VALUES(?,?,?,?,?,?,?,?,?)", action.actor.PlayerId, action.high, action.low, action.method, action.payload, action.actor.OwnerAccount, action.actor.OwnerRevision, payload, stamp)
	if err == nil {
		action.committedResponse = stable
	}
	return err
}

func (s *sqlTeamStore) replayAction(ctx context.Context, action *sqlAction) (*teampb.TeamResponse, error) {
	tx, err := s.authority.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = s.authority.LockTeamOriginals(ctx, tx, []*teampb.TeamOriginalRole{action.actor}); err != nil {
		return nil, err
	}
	response, err := loadSQLAction(ctx, tx, action)
	if err != nil {
		return nil, err
	}
	return response, tx.Commit()
}

func (s *sqlTeamStore) recordNoMutationAction(ctx context.Context, action *sqlAction, response *teampb.TeamResponse) error {
	tx, err := s.authority.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.authority.LockTeamOriginals(ctx, tx, []*teampb.TeamOriginalRole{action.actor}); err != nil {
		return err
	}
	previous, err := loadSQLAction(ctx, tx, action)
	if err != nil || previous != nil {
		if previous != nil {
			action.committedResponse, action.replayed = previous, true
		}
		return err
	}
	stamp, err := teamSQLNow(ctx, tx)
	if err != nil {
		return err
	}
	if err = writeSQLAction(ctx, tx, action, response, stamp); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) CreateTeam(ctx context.Context, caller uint64, in *teampb.CreateTeamRequest) *teampb.TeamResponse {
	return s.sqlAction(ctx, caller, uint32(OpCreate), in, func(ctx context.Context) *teampb.TeamResponse { return s.createTeam(ctx, caller, in) })
}
func (s *Service) ApplyJoinTeam(ctx context.Context, caller uint64, in *teampb.ApplyJoinTeamRequest) *teampb.TeamResponse {
	return s.sqlAction(ctx, caller, uint32(OpApply), in, func(ctx context.Context) *teampb.TeamResponse { return s.applyJoinTeam(ctx, caller, in) })
}
func (s *Service) HandleApplication(ctx context.Context, caller uint64, in *teampb.HandleApplicationRequest) *teampb.TeamResponse {
	return s.sqlAction(ctx, caller, uint32(OpHandleApplication), in, func(ctx context.Context) *teampb.TeamResponse { return s.handleApplication(ctx, caller, in) })
}
func (s *Service) InviteToTeam(ctx context.Context, caller uint64, in *teampb.InviteToTeamRequest) *teampb.TeamResponse {
	return s.sqlAction(ctx, caller, uint32(OpInvite), in, func(ctx context.Context) *teampb.TeamResponse { return s.inviteToTeam(ctx, caller, in) })
}
func (s *Service) RespondInvite(ctx context.Context, caller uint64, in *teampb.RespondInviteRequest) *teampb.TeamResponse {
	return s.sqlAction(ctx, caller, uint32(OpRespondInvite), in, func(ctx context.Context) *teampb.TeamResponse { return s.respondInvite(ctx, caller, in) })
}
func (s *Service) LeaveTeam(ctx context.Context, caller uint64, in *teampb.LeaveTeamRequest) *teampb.TeamResponse {
	return s.sqlAction(ctx, caller, uint32(OpLeave), in, func(ctx context.Context) *teampb.TeamResponse { return s.leaveTeam(ctx, caller, in) })
}
func (s *Service) KickMember(ctx context.Context, caller uint64, in *teampb.KickMemberRequest) *teampb.TeamResponse {
	return s.sqlAction(ctx, caller, uint32(OpKick), in, func(ctx context.Context) *teampb.TeamResponse { return s.kickMember(ctx, caller, in) })
}
func (s *Service) TransferLeader(ctx context.Context, caller uint64, in *teampb.TransferLeaderRequest) *teampb.TeamResponse {
	return s.sqlAction(ctx, caller, uint32(OpTransferLeader), in, func(ctx context.Context) *teampb.TeamResponse { return s.transferLeader(ctx, caller, in) })
}
func (s *Service) DisbandTeam(ctx context.Context, caller uint64, in *teampb.DisbandTeamRequest) *teampb.TeamResponse {
	return s.sqlAction(ctx, caller, uint32(OpDisband), in, func(ctx context.Context) *teampb.TeamResponse { return s.disbandTeam(ctx, caller, in) })
}
