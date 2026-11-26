package logic

import (
	"bytes"
	"context"
	"errors"

	"match/internal/metrics"
	"match/internal/pkg/ctxkeys"
	"match/internal/playercontract"
	"match/internal/svc"

	battlepb "proto/battle"
	matchpb "proto/match"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// StartGuildWarBattleLogic 帮会全天帮战 V2 无匹配池直打入口（MatchInternal.StartGuildWarBattle）。
// 支持两类模式：
//   - MATCH_MODE_GUILD_WAR_PVP (9)：真人双方 1..5v1..5 直打（界面活人、野外路战、灵脉攻防、追杀结单）；
//   - MATCH_MODE_GUILD_WAR_SIEGE (10)：进攻方 1..5 人攻打 AI 托管守旗阵快照（ai_defender_snapshots）。
// 当任一真人目标已在战斗中时返回 ACTIVITY_BATTLE_REJECT_MEMBER_IN_BATTLE 与 offender_battle_id，
// 便于客户端直接切换一键实时观战。
type StartGuildWarBattleLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewStartGuildWarBattleLogic(ctx context.Context, svcCtx *svc.ServiceContext) *StartGuildWarBattleLogic {
	return &StartGuildWarBattleLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func guildWarStartPayload(in *matchpb.StartGuildWarBattleRequest) ([]byte, error) {
	unsigned := proto.Clone(in).(*matchpb.StartGuildWarBattleRequest)
	unsigned.Auth = nil
	return proto.MarshalOptions{Deterministic: true}.Marshal(unsigned)
}

func validateStartGuildWarBattle(in *matchpb.StartGuildWarBattleRequest) string {
	if in == nil {
		return "nil request"
	}
	if in.GetRequestId() == 0 {
		return "request_id is 0"
	}
	if in.GetBattleConfigId() == 0 {
		return "battle_config_id is 0"
	}
	mode := matchpb.MatchMode(in.GetMatchMode())
	if mode != matchpb.MatchMode_MATCH_MODE_GUILD_WAR_PVP && mode != matchpb.MatchMode_MATCH_MODE_GUILD_WAR_SIEGE {
		return "unsupported match_mode for guild war"
	}
	actx := in.GetActivityContext()
	if actx == nil || actx.GetKind() != battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_GUILD_WAR || actx.GetGuildWar() == nil {
		return "missing or invalid guild war activity context"
	}
	gw := actx.GetGuildWar()
	if gw.GetAttackerGuildId() == 0 {
		return "attacker_guild_id is 0"
	}
	attackers := in.GetAttackerPlayerIds()
	if len(attackers) == 0 || len(attackers) > kMaxBattleTeamSize {
		return "attacker count out of range"
	}
	seen := make(map[uint64]struct{}, len(attackers)+len(in.GetDefenderPlayerIds())+len(in.GetAiDefenderSnapshots()))
	for _, pid := range attackers {
		if pid == 0 {
			return "attacker contains 0"
		}
		if _, dup := seen[pid]; dup {
			return "duplicate attacker"
		}
		seen[pid] = struct{}{}
	}
	if mode == matchpb.MatchMode_MATCH_MODE_GUILD_WAR_PVP {
		defenders := in.GetDefenderPlayerIds()
		if len(defenders) == 0 || len(defenders) > kMaxBattleTeamSize {
			return "defender count out of range for live pvp"
		}
		if len(in.GetAiDefenderSnapshots()) != 0 {
			return "live pvp must not carry ai_defender_snapshots"
		}
		for _, pid := range defenders {
			if pid == 0 {
				return "defender contains 0"
			}
			if _, dup := seen[pid]; dup {
				return "duplicate or overlapping defender"
			}
			seen[pid] = struct{}{}
		}
	} else {
		if len(in.GetDefenderPlayerIds()) != 0 {
			return "siege mode must not carry human defender_player_ids"
		}
		aiDefenders := in.GetAiDefenderSnapshots()
		if len(aiDefenders) == 0 || len(aiDefenders) > kMaxBattleTeamSize {
			return "siege mode requires 1..5 ai_defender_snapshots"
		}
		for _, snap := range aiDefenders {
			if snap == nil || snap.GetPlayerId() == 0 {
				return "ai defender snapshot missing player_id"
			}
			if _, dup := seen[snap.GetPlayerId()]; dup {
				return "duplicate or overlapping ai defender player_id"
			}
			seen[snap.GetPlayerId()] = struct{}{}
		}
	}
	return ""
}

func (l *StartGuildWarBattleLogic) reject(reject matchpb.ActivityBattleReject, offender uint64, offenderBattleID uint64) *matchpb.StartGuildWarBattleResponse {
	metrics.ObserveActivityBattle("guild_war", activityRejectResult(reject))
	return &matchpb.StartGuildWarBattleResponse{
		Reject:           reject,
		OffenderPlayerId: offender,
		OffenderBattleId: offenderBattleID,
	}
}

// precheckGuildWarMember 预检真人参与者；若玩家正处于战斗中，顺带返回其 battle_id 供一键观战。
func (l *StartGuildWarBattleLogic) precheckGuildWarMember(pid uint64) (matchpb.ActivityBattleReject, uint64) {
	session, err := playercontract.LoadSession(l.ctx, l.svcCtx, pid)
	if err != nil {
		l.Errorf("[match] Guild war precheck failed to read session player=%d: %v", pid, err)
		return matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL, 0
	}
	if !playercontract.IsSessionOnline(session) {
		return matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_OFFLINE, 0
	}

	battleID, locked, err := playercontract.BattleLockBattleID(l.ctx, l.svcCtx, pid)
	if err != nil {
		l.Errorf("[match] Guild war precheck failed to read battle lock player=%d: %v", pid, err)
		return matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL, 0
	}
	if locked {
		return matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_IN_BATTLE, battleID
	}

	loc, err := playercontract.LoadLocation(l.ctx, l.svcCtx, pid)
	if err != nil {
		l.Errorf("[match] Guild war precheck failed to read location player=%d: %v", pid, err)
		return matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL, 0
	}
	if loc == nil {
		return matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_NOT_READY, 0
	}

	ctx, cancel := context.WithTimeout(l.ctx, activityTicketReadBudget)
	defer cancel()
	ticket, err := l.svcCtx.Authority.Ticket(ctx, pid)
	if err != nil {
		l.Errorf("[match] Guild war precheck failed to read ticket player=%d: %v", pid, err)
		return matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL, 0
	}
	if ticket == nil || ticket.State == 0 || ticket.State >= 4 {
		return matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_NONE, 0
	}
	released := false
	if ticket.State == 3 {
		if released, err = releaseGlobalFinished(ctx, l.svcCtx, ticket); err != nil {
			l.Errorf("[match] Guild war precheck failed to release finished ticket player=%d: %v", pid, err)
			return matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL, 0
		}
	}
	if !released {
		if ticket.FormationId != 0 {
			return matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_IN_BATTLE, ticket.FormationId
		}
		return matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_NOT_READY, 0
	}
	return matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_NONE, 0
}

func (l *StartGuildWarBattleLogic) StartGuildWarBattle(in *matchpb.StartGuildWarBattleRequest) (*matchpb.StartGuildWarBattleResponse, error) {
	if detail, ok := ctxkeys.GetSessionDetails(l.ctx); ok && detail != nil {
		l.Errorf("[match] Rejecting client-originated MatchInternal.StartGuildWarBattle call session_id=%d player=%d",
			detail.GetSessionId(), detail.GetPlayerId())
		return nil, status.Error(codes.PermissionDenied, "MatchInternal is not callable by clients")
	}
	if reason := validateStartGuildWarBattle(in); reason != "" {
		l.Errorf("[match] Invalid guild war battle request: %s config=%d mode=%d", reason, in.GetBattleConfigId(), in.GetMatchMode())
		return l.reject(matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INVALID_ARGUMENT, 0, 0), nil
	}
	if !guildCallVerified(l.ctx, l.svcCtx, matchpb.MatchInternal_StartGuildWarBattle_FullMethodName, in) {
		return l.reject(matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL, 0, 0), nil
	}
	if l.svcCtx == nil || l.svcCtx.Authority == nil {
		return l.reject(matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL, 0, 0), nil
	}

	gw := in.GetActivityContext().GetGuildWar()
	key := &matchpb.MatchDirectIntentRecord{
		Kind:       6,
		OwnerId:    gw.GetAttackerGuildId(),
		RequestLow: in.GetRequestId(),
	}
	payload, err := guildWarStartPayload(in)
	if err != nil {
		return l.reject(matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL, 0, 0), nil
	}
	old, original, err := l.svcCtx.Authority.FindDirect(l.ctx, key)
	if err != nil {
		return l.reject(matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL, 0, 0), nil
	}
	if old != nil {
		if !bytes.Equal(original, payload) {
			return l.reject(matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INVALID_ARGUMENT, 0, 0), nil
		}
		if old.State < 4 {
			launchGlobalFormation(l.svcCtx, old)
		}
		return &matchpb.StartGuildWarBattleResponse{
			BattleId:       old.BattleId,
			FormationState: old.State,
		}, nil
	}

	humanMembers := make([]uint64, 0, len(in.GetAttackerPlayerIds())+len(in.GetDefenderPlayerIds()))
	memberTeams := make(map[uint64]uint32, len(in.GetAttackerPlayerIds())+len(in.GetDefenderPlayerIds()))
	for _, pid := range in.GetAttackerPlayerIds() {
		humanMembers = append(humanMembers, pid)
		memberTeams[pid] = 0
	}
	for _, pid := range in.GetDefenderPlayerIds() {
		humanMembers = append(humanMembers, pid)
		memberTeams[pid] = 1
	}

	for _, pid := range humanMembers {
		rej, conflictBattleID := l.precheckGuildWarMember(pid)
		if rej != matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_NONE {
			return l.reject(rej, pid, conflictBattleID), nil
		}
	}

	mode := matchpb.MatchMode(in.GetMatchMode())
	f, err := reserveGlobalDirect(l.ctx, l.svcCtx, key, payload, mode, in.GetBattleConfigId(), humanMembers, directOptions{
		activityContext: in.GetActivityContext(),
		memberTeams:     memberTeams,
	})
	if err != nil {
		// 并发抢点落败：检查谁刚被别人抢进战斗，顺带返回 offender_battle_id 供一键观战。
		for _, pid := range humanMembers {
			if bid, locked, lockErr := playercontract.BattleLockBattleID(l.ctx, l.svcCtx, pid); lockErr == nil && locked {
				return l.reject(matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_IN_BATTLE, pid, bid), nil
			}
			if t, tErr := l.svcCtx.Authority.Ticket(l.ctx, pid); tErr == nil && t != nil && t.FormationId != 0 && t.State > 0 && t.State < 4 {
				return l.reject(matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_IN_BATTLE, pid, t.FormationId), nil
			}
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			l.Errorf("[match] StartGuildWarBattle reserveGlobalDirect failed req=%d: %v", in.GetRequestId(), err)
		}
		return l.reject(matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL, 0, 0), nil
	}

	launchGlobalFormation(l.svcCtx, f)
	metrics.ObserveActivityBattle("guild_war", activityResultStarted)
	return &matchpb.StartGuildWarBattleResponse{
		BattleId:       f.BattleId,
		FormationState: f.State,
	}, nil
}
