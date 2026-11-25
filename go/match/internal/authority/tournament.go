package authority

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"match/internal/tournament"

	"github.com/go-sql-driver/mysql"
	"google.golang.org/protobuf/proto"
	battlepb "proto/battle"
	kafkapb "proto/contracts/kafka"
	mailpb "proto/mail"
	pb "proto/match"
)

const (
	tournamentSeasonCols = "season_id,season_key,season_name,rule_version,stage,registration_start_ms,qualifier_start_ms,knockout_start_ms,settle_start_ms,end_ms,battle_team_size,roster_max_size,knockout_team_count,champion_squad_id,champion_squad_name,champion_member_summary,settled_ms,updated_ms"
	tournamentSquadCols  = "squad_id,season_id,name,name_norm,captain_player_id,state,member_count,qualifier_points,qualifier_played,qualifier_wins,qualifier_losses,qualifier_draws,seed_rank,final_rank,cheer_count,registered_ms,updated_ms"
	tournamentMemberCols = "season_id,player_id,squad_id,role,starter_slot,player_name,zone_id,joined_ms"
	tournamentInviteCols = "squad_id,invitee_player_id,season_id,inviter_player_id,state,created_ms,expires_ms"
	tournamentMatchCols  = "match_id,season_id,phase,round_index,slot_index,squad_a_id,squad_b_id,lineup_a,lineup_b,ready_a,ready_b,state,battle_id,winner_squad_id,outcome,featured_spectate,scheduled_start_ms,deadline_ms,finished_ms,updated_ms"
	tournamentRewardCols = "season_id,player_id,tier_kind,reward_id,squad_id,final_rank,tier_id,gold_amount,attachments,title_code,badge_code,delivery_state,mail_id,claimed_ms,updated_ms"
)

var (
	ErrTournamentNotOpen           = errors.New("tournament: feature disabled")
	ErrTournamentStageClosed       = errors.New("tournament: action not allowed in current stage")
	ErrTournamentSquadNameInvalid  = errors.New("tournament: invalid or duplicate squad name")
	ErrTournamentAlreadyInSquad    = errors.New("tournament: player already in a squad this season")
	ErrTournamentSquadNotFound     = errors.New("tournament: squad not found")
	ErrTournamentNotCaptain        = errors.New("tournament: caller is not squad captain")
	ErrTournamentSquadFull         = errors.New("tournament: squad roster full")
	ErrTournamentSquadLocked       = errors.New("tournament: squad registration locked")
	ErrTournamentInviteNotFound    = errors.New("tournament: invite not found or expired")
	ErrTournamentMemberNotEnough   = errors.New("tournament: not enough squad members")
	ErrTournamentLineupInvalid     = errors.New("tournament: lineup invalid")
	ErrTournamentQualifierLimit    = errors.New("tournament: qualifier match cap reached")
	ErrTournamentMatchNotReady     = errors.New("tournament: match not ready or already finished")
	ErrTournamentAlreadyCheered    = errors.New("tournament: already cheered this season")
	ErrTournamentRewardUnavailable = errors.New("tournament: no claimable rewards")
)

// TournamentLaunchSpec 描述一场双方已就绪、可直接进入 gather 建房的锦标赛对局。
type TournamentLaunchSpec struct {
	MatchID        uint64
	SeasonID       uint32
	Phase          pb.TournamentMatchPhase
	RoundIndex     uint32
	BattleConfigID uint32
	TeamA          []uint64
	TeamB          []uint64
	Featured       bool
}

// SwapTournamentCatalog 在启动与表包热更时原子切换锦标赛配置目录。
func (s *Store) SwapTournamentCatalog(cat *tournament.Catalog) {
	if s != nil {
		s.tournamentCatalog.Store(cat)
	}
}

// TournamentCatalog 返回当前生效的锦标赛目录;单测未装配时回落首测默认目录。
func (s *Store) TournamentCatalog() *tournament.Catalog {
	if s == nil {
		return tournament.DefaultFallback()
	}
	if c := s.tournamentCatalog.Load(); c != nil {
		return c
	}
	return tournament.DefaultFallback()
}

func scanTournamentSeason(row scanner) (*pb.MatchTournamentSeasonRecord, error) {
	r := &pb.MatchTournamentSeasonRecord{}
	err := row.Scan(
		&r.SeasonId, &r.SeasonKey, &r.SeasonName, &r.RuleVersion, &r.Stage,
		&r.RegistrationStartMs, &r.QualifierStartMs, &r.KnockoutStartMs, &r.SettleStartMs, &r.EndMs,
		&r.BattleTeamSize, &r.RosterMaxSize, &r.KnockoutTeamCount,
		&r.ChampionSquadId, &r.ChampionSquadName, &r.ChampionMemberSummary, &r.SettledMs, &r.UpdatedMs,
	)
	return r, err
}

func scanTournamentSquad(row scanner) (*pb.MatchTournamentSquadRecord, error) {
	r := &pb.MatchTournamentSquadRecord{}
	err := row.Scan(
		&r.SquadId, &r.SeasonId, &r.Name, &r.NameNorm, &r.CaptainPlayerId, &r.State, &r.MemberCount,
		&r.QualifierPoints, &r.QualifierPlayed, &r.QualifierWins, &r.QualifierLosses, &r.QualifierDraws,
		&r.SeedRank, &r.FinalRank, &r.CheerCount, &r.RegisteredMs, &r.UpdatedMs,
	)
	return r, err
}

func scanTournamentMember(row scanner) (*pb.MatchTournamentMemberRecord, error) {
	r := &pb.MatchTournamentMemberRecord{}
	err := row.Scan(&r.SeasonId, &r.PlayerId, &r.SquadId, &r.Role, &r.StarterSlot, &r.PlayerName, &r.ZoneId, &r.JoinedMs)
	return r, err
}

func scanTournamentInvite(row scanner) (*pb.MatchTournamentInviteRecord, error) {
	r := &pb.MatchTournamentInviteRecord{}
	err := row.Scan(&r.SquadId, &r.InviteePlayerId, &r.SeasonId, &r.InviterPlayerId, &r.State, &r.CreatedMs, &r.ExpiresMs)
	return r, err
}

func scanTournamentMatch(row scanner) (*pb.MatchTournamentMatchRecord, error) {
	r := &pb.MatchTournamentMatchRecord{}
	err := row.Scan(
		&r.MatchId, &r.SeasonId, &r.Phase, &r.RoundIndex, &r.SlotIndex,
		&r.SquadAId, &r.SquadBId, &r.LineupA, &r.LineupB, &r.ReadyA, &r.ReadyB,
		&r.State, &r.BattleId, &r.WinnerSquadId, &r.Outcome, &r.FeaturedSpectate,
		&r.ScheduledStartMs, &r.DeadlineMs, &r.FinishedMs, &r.UpdatedMs,
	)
	return r, err
}

func scanTournamentReward(row scanner) (*pb.MatchTournamentRewardRecord, error) {
	r := &pb.MatchTournamentRewardRecord{}
	err := row.Scan(
		&r.SeasonId, &r.PlayerId, &r.TierKind, &r.RewardId, &r.SquadId, &r.FinalRank,
		&r.TierId, &r.GoldAmount, &r.Attachments, &r.TitleCode, &r.BadgeCode,
		&r.DeliveryState, &r.MailId, &r.ClaimedMs, &r.UpdatedMs,
	)
	return r, err
}

func tournamentSeasonName(id uint32) string {
	return fmt.Sprintf("第%d赛季·群仙问道赛", id)
}

func buildNewTournamentSeason(seasonID uint32, startMs uint64, cat *tournament.Catalog) *pb.MatchTournamentSeasonRecord {
	rule := cat.Rule()
	qStart := startMs + uint64(rule.GetRegistrationDays())*millisPerDay
	kStart := qStart + uint64(rule.GetQualifierDays())*millisPerDay
	sStart := kStart + uint64(rule.GetKnockoutDays())*millisPerDay
	endMs := sStart + uint64(rule.GetSettleDisplayDays())*millisPerDay
	return &pb.MatchTournamentSeasonRecord{
		SeasonId:            seasonID,
		SeasonKey:           fmt.Sprintf("ts%d", seasonID),
		SeasonName:          tournamentSeasonName(seasonID),
		RuleVersion:         rule.GetRuleVersion(),
		Stage:               uint32(pb.TournamentStage_TOURNAMENT_STAGE_REGISTRATION),
		RegistrationStartMs: startMs,
		QualifierStartMs:    qStart,
		KnockoutStartMs:     kStart,
		SettleStartMs:       sStart,
		EndMs:               endMs,
		BattleTeamSize:      rule.GetBattleTeamSize(),
		RosterMaxSize:       rule.GetRosterMaxSize(),
		UpdatedMs:           startMs,
	}
}

func (s *Store) ensureTournamentSeasonTx(ctx context.Context, tx *sql.Tx, stamp uint64) (*pb.MatchTournamentSeasonRecord, error) {
	cur, err := scanTournamentSeason(tx.QueryRowContext(ctx, "SELECT "+tournamentSeasonCols+" FROM match_tournament_season ORDER BY season_id DESC LIMIT 1"))
	if err == nil {
		return cur, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	s1 := buildNewTournamentSeason(1, stamp, s.TournamentCatalog())
	_, err = tx.ExecContext(ctx,
		"INSERT INTO match_tournament_season("+tournamentSeasonCols+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		s1.SeasonId, s1.SeasonKey, s1.SeasonName, s1.RuleVersion, s1.Stage,
		s1.RegistrationStartMs, s1.QualifierStartMs, s1.KnockoutStartMs, s1.SettleStartMs, s1.EndMs,
		s1.BattleTeamSize, s1.RosterMaxSize, s1.KnockoutTeamCount,
		s1.ChampionSquadId, s1.ChampionSquadName, s1.ChampionMemberSummary, s1.SettledMs, s1.UpdatedMs,
	)
	if err != nil {
		var dup *mysql.MySQLError
		if errors.As(err, &dup) && dup.Number == 1062 {
			return scanTournamentSeason(tx.QueryRowContext(ctx, "SELECT "+tournamentSeasonCols+" FROM match_tournament_season WHERE season_id=1"))
		}
		return nil, err
	}
	return s1, nil
}

func (s *Store) toTournamentSeasonView(r *pb.MatchTournamentSeasonRecord, registeredCount uint32) *pb.TournamentSeasonView {
	rule := s.TournamentCatalog().Rule()
	return &pb.TournamentSeasonView{
		SeasonId:                r.SeasonId,
		SeasonKey:               r.SeasonKey,
		SeasonName:              r.SeasonName,
		RuleVersion:             r.RuleVersion,
		Stage:                   pb.TournamentStage(r.Stage),
		Enabled:                 rule.GetEnabled(),
		BattleTeamSize:          r.BattleTeamSize,
		RosterMaxSize:           r.RosterMaxSize,
		MinLevel:                rule.GetMinLevel(),
		QualifierMaxMatches:     rule.GetQualifierMaxMatches(),
		ParticipationMinMatches: rule.GetParticipationMinMatches(),
		KnockoutMaxTeams:        rule.GetKnockoutMaxTeams(),
		KnockoutTeamCount:       r.KnockoutTeamCount,
		RegisteredSquadCount:    registeredCount,
		RegistrationStartMs:     r.RegistrationStartMs,
		QualifierStartMs:        r.QualifierStartMs,
		KnockoutStartMs:         r.KnockoutStartMs,
		SettleStartMs:           r.SettleStartMs,
		EndMs:                   r.EndMs,
		ChampionSquadId:         r.ChampionSquadId,
		ChampionSquadName:       r.ChampionSquadName,
		ChampionMemberSummary:   r.ChampionMemberSummary,
	}
}

func loadSquadMembersTx(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, squadID uint64) ([]*pb.TournamentMemberView, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+tournamentMemberCols+" FROM match_tournament_member WHERE squad_id=? ORDER BY role ASC, starter_slot ASC, joined_ms ASC", squadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.TournamentMemberView
	for rows.Next() {
		m, err := scanTournamentMember(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, &pb.TournamentMemberView{
			PlayerId:    m.PlayerId,
			PlayerName:  m.PlayerName,
			ZoneId:      m.ZoneId,
			Role:        pb.TournamentMemberRole(m.Role),
			StarterSlot: m.StarterSlot,
			JoinedMs:    m.JoinedMs,
		})
	}
	return out, rows.Err()
}

func toTournamentSquadView(r *pb.MatchTournamentSquadRecord, members []*pb.TournamentMemberView) *pb.TournamentSquadView {
	if r == nil {
		return nil
	}
	return &pb.TournamentSquadView{
		SquadId:         r.SquadId,
		SeasonId:        r.SeasonId,
		Name:            r.Name,
		CaptainPlayerId: r.CaptainPlayerId,
		State:           pb.TournamentSquadState(r.State),
		MemberCount:     r.MemberCount,
		QualifierPoints: r.QualifierPoints,
		QualifierPlayed: r.QualifierPlayed,
		QualifierWins:   r.QualifierWins,
		QualifierLosses: r.QualifierLosses,
		QualifierDraws:  r.QualifierDraws,
		SeedRank:        r.SeedRank,
		FinalRank:       r.FinalRank,
		CheerCount:      r.CheerCount,
		RegisteredMs:    r.RegisteredMs,
		Members:         members,
	}
}

func toTournamentRewardView(r *pb.MatchTournamentRewardRecord) *pb.TournamentPlayerRewardView {
	v := &pb.TournamentPlayerRewardView{
		RewardId:      r.RewardId,
		SeasonId:      r.SeasonId,
		TierKind:      pb.TournamentRewardTierKind(r.TierKind),
		TierId:        r.TierId,
		FinalRank:     r.FinalRank,
		GoldAmount:    r.GoldAmount,
		TitleCode:     r.TitleCode,
		BadgeCode:     r.BadgeCode,
		DeliveryState: pb.TournamentRewardDeliveryState(r.DeliveryState),
		MailId:        r.MailId,
		ClaimedMs:     r.ClaimedMs,
	}
	if len(r.Attachments) > 0 {
		var att mailpb.MailAttachments
		if proto.Unmarshal(r.Attachments, &att) == nil {
			for _, it := range att.Items {
				if it.ConfigId != 0 && it.Count != 0 {
					v.Items = append(v.Items, &pb.TournamentRewardItem{ItemId: it.ConfigId, Count: it.Count})
				}
			}
		}
	}
	return v
}

func toTournamentMatchView(m *pb.MatchTournamentMatchRecord, squads map[uint64]*pb.MatchTournamentSquadRecord) *pb.TournamentMatchView {
	if m == nil {
		return nil
	}
	v := &pb.TournamentMatchView{
		MatchId:          m.MatchId,
		SeasonId:         m.SeasonId,
		Phase:            pb.TournamentMatchPhase(m.Phase),
		RoundIndex:       m.RoundIndex,
		SlotIndex:        m.SlotIndex,
		SquadAId:         m.SquadAId,
		SquadBId:         m.SquadBId,
		ReadyA:           m.ReadyA,
		ReadyB:           m.ReadyB,
		State:            pb.TournamentMatchState(m.State),
		BattleId:         m.BattleId,
		WinnerSquadId:    m.WinnerSquadId,
		Outcome:          battlepb.EBattleOutcome(m.Outcome),
		FeaturedSpectate: m.FeaturedSpectate,
		ScheduledStartMs: m.ScheduledStartMs,
		DeadlineMs:       m.DeadlineMs,
		FinishedMs:       m.FinishedMs,
	}
	if a := squads[m.SquadAId]; a != nil {
		v.SquadAName = a.Name
		v.SquadASeed = a.SeedRank
	}
	if b := squads[m.SquadBId]; b != nil {
		v.SquadBName = b.Name
		v.SquadBSeed = b.SeedRank
	}
	return v
}

func (s *Store) ensureTournamentOpen() error {
	if !s.TournamentCatalog().Rule().GetEnabled() {
		return ErrTournamentNotOpen
	}
	return nil
}

// CreateTournamentSquad 创建本赛季战队(队名已由上层完成敏感词审核;squadID 在事务外由 BattleIDGen 预分配)。
func (s *Store) CreateTournamentSquad(ctx context.Context, playerID uint64, playerName string, zoneID uint32, squadName string, squadID uint64) (*pb.TournamentSquadView, error) {
	if err := s.ensureTournamentOpen(); err != nil {
		return nil, err
	}
	display, norm, ok := tournament.NormalizeSquadName(squadName)
	if !ok {
		return nil, ErrTournamentSquadNameInvalid
	}
	if playerID == 0 || squadID == 0 {
		return nil, errors.New("tournament: invalid player or squad id")
	}
	var view *pb.TournamentSquadView
	err := s.Fenced(ctx, func() error {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		stamp, err := now(ctx, tx)
		if err != nil {
			return err
		}
		cur, err := s.ensureTournamentSeasonTx(ctx, tx, stamp)
		if err != nil {
			return err
		}
		if cur.Stage != uint32(pb.TournamentStage_TOURNAMENT_STAGE_REGISTRATION) {
			return ErrTournamentStageClosed
		}
		// 事务内锁定防一人双队(主键 (season_id, player_id) 兜底)。
		var existingSquad uint64
		if err = tx.QueryRowContext(ctx, "SELECT squad_id FROM match_tournament_member WHERE season_id=? AND player_id=? FOR UPDATE", cur.SeasonId, playerID).Scan(&existingSquad); err == nil {
			return ErrTournamentAlreadyInSquad
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		squad := &pb.MatchTournamentSquadRecord{
			SquadId:         squadID,
			SeasonId:        cur.SeasonId,
			Name:            display,
			NameNorm:        norm,
			CaptainPlayerId: playerID,
			State:           uint32(pb.TournamentSquadState_TOURNAMENT_SQUAD_STATE_DRAFT),
			MemberCount:     1,
			UpdatedMs:       stamp,
		}
		_, err = tx.ExecContext(ctx,
			"INSERT INTO match_tournament_squad("+tournamentSquadCols+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
			squad.SquadId, squad.SeasonId, squad.Name, squad.NameNorm, squad.CaptainPlayerId, squad.State, squad.MemberCount,
			0, 0, 0, 0, 0, 0, 0, 0, 0, stamp,
		)
		if err != nil {
			var dup *mysql.MySQLError
			if errors.As(err, &dup) && dup.Number == 1062 {
				return ErrTournamentSquadNameInvalid
			}
			return err
		}
		_, err = tx.ExecContext(ctx,
			"INSERT INTO match_tournament_member("+tournamentMemberCols+") VALUES(?,?,?,?,?,?,?,?)",
			cur.SeasonId, playerID, squadID, uint32(pb.TournamentMemberRole_TOURNAMENT_MEMBER_ROLE_CAPTAIN), uint32(1), playerName, zoneID, stamp,
		)
		if err != nil {
			var dup *mysql.MySQLError
			if errors.As(err, &dup) && dup.Number == 1062 {
				return ErrTournamentAlreadyInSquad
			}
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		view = toTournamentSquadView(squad, []*pb.TournamentMemberView{{
			PlayerId:    playerID,
			PlayerName:  playerName,
			ZoneId:      zoneID,
			Role:        pb.TournamentMemberRole_TOURNAMENT_MEMBER_ROLE_CAPTAIN,
			StarterSlot: 1,
			JoinedMs:    stamp,
		}})
		return nil
	})
	return view, err
}

// InviteTournamentMember 队长邀请全服任意玩家加入本赛季未锁定战队。
func (s *Store) InviteTournamentMember(ctx context.Context, captainID, targetPlayerID uint64, inviterName string) (*pb.TournamentInviteView, error) {
	if err := s.ensureTournamentOpen(); err != nil {
		return nil, err
	}
	if captainID == 0 || targetPlayerID == 0 || captainID == targetPlayerID {
		return nil, ErrTournamentLineupInvalid
	}
	var out *pb.TournamentInviteView
	err := s.Fenced(ctx, func() error {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		stamp, err := now(ctx, tx)
		if err != nil {
			return err
		}
		cur, err := s.ensureTournamentSeasonTx(ctx, tx, stamp)
		if err != nil {
			return err
		}
		if cur.Stage != uint32(pb.TournamentStage_TOURNAMENT_STAGE_REGISTRATION) {
			return ErrTournamentStageClosed
		}
		var squadID uint64
		if err = tx.QueryRowContext(ctx, "SELECT squad_id FROM match_tournament_member WHERE season_id=? AND player_id=?", cur.SeasonId, captainID).Scan(&squadID); errors.Is(err, sql.ErrNoRows) {
			return ErrTournamentSquadNotFound
		} else if err != nil {
			return err
		}
		squad, err := scanTournamentSquad(tx.QueryRowContext(ctx, "SELECT "+tournamentSquadCols+" FROM match_tournament_squad WHERE squad_id=? FOR UPDATE", squadID))
		if err != nil {
			return err
		}
		if squad.CaptainPlayerId != captainID {
			return ErrTournamentNotCaptain
		}
		if squad.State != uint32(pb.TournamentSquadState_TOURNAMENT_SQUAD_STATE_DRAFT) {
			return ErrTournamentSquadLocked
		}
		if squad.MemberCount >= cur.RosterMaxSize {
			return ErrTournamentSquadFull
		}
		var targetSquad uint64
		if err = tx.QueryRowContext(ctx, "SELECT squad_id FROM match_tournament_member WHERE season_id=? AND player_id=?", cur.SeasonId, targetPlayerID).Scan(&targetSquad); err == nil {
			return ErrTournamentAlreadyInSquad
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		ttlSec := uint64(s.TournamentCatalog().Rule().GetInviteTtlSeconds())
		if ttlSec == 0 {
			ttlSec = 259200
		}
		expiresMs := stamp + ttlSec*1000
		_, err = tx.ExecContext(ctx,
			"INSERT INTO match_tournament_invite("+tournamentInviteCols+") VALUES(?,?,?,?,1,?,?) "+
				"ON DUPLICATE KEY UPDATE inviter_player_id=VALUES(inviter_player_id),state=1,created_ms=VALUES(created_ms),expires_ms=VALUES(expires_ms)",
			squad.SquadId, targetPlayerID, cur.SeasonId, captainID, stamp, expiresMs,
		)
		if err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		out = &pb.TournamentInviteView{
			SquadId:            squad.SquadId,
			SeasonId:           cur.SeasonId,
			SquadName:          squad.Name,
			InviterPlayerId:    captainID,
			InviterName:        inviterName,
			CurrentMemberCount: squad.MemberCount,
			RosterMaxSize:      cur.RosterMaxSize,
			CreatedMs:          stamp,
			ExpiresMs:          expiresMs,
		}
		return nil
	})
	return out, err
}

// RespondTournamentInvite 受邀者接受或拒绝战队邀请。
func (s *Store) RespondTournamentInvite(ctx context.Context, playerID uint64, playerName string, zoneID uint32, squadID uint64, accept bool) (*pb.TournamentSquadView, error) {
	if err := s.ensureTournamentOpen(); err != nil {
		return nil, err
	}
	var view *pb.TournamentSquadView
	err := s.Fenced(ctx, func() error {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		stamp, err := now(ctx, tx)
		if err != nil {
			return err
		}
		cur, err := s.ensureTournamentSeasonTx(ctx, tx, stamp)
		if err != nil {
			return err
		}
		inv, err := scanTournamentInvite(tx.QueryRowContext(ctx, "SELECT "+tournamentInviteCols+" FROM match_tournament_invite WHERE squad_id=? AND invitee_player_id=? FOR UPDATE", squadID, playerID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTournamentInviteNotFound
		}
		if err != nil {
			return err
		}
		if inv.SeasonId != cur.SeasonId || inv.State != 1 || inv.ExpiresMs <= stamp {
			return ErrTournamentInviteNotFound
		}
		if !accept {
			if _, err = tx.ExecContext(ctx, "UPDATE match_tournament_invite SET state=3 WHERE squad_id=? AND invitee_player_id=?", squadID, playerID); err != nil {
				return err
			}
			return tx.Commit()
		}
		if cur.Stage != uint32(pb.TournamentStage_TOURNAMENT_STAGE_REGISTRATION) {
			return ErrTournamentStageClosed
		}
		squad, err := scanTournamentSquad(tx.QueryRowContext(ctx, "SELECT "+tournamentSquadCols+" FROM match_tournament_squad WHERE squad_id=? FOR UPDATE", squadID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTournamentSquadNotFound
		}
		if err != nil {
			return err
		}
		if squad.State != uint32(pb.TournamentSquadState_TOURNAMENT_SQUAD_STATE_DRAFT) {
			return ErrTournamentSquadLocked
		}
		if squad.MemberCount >= cur.RosterMaxSize {
			return ErrTournamentSquadFull
		}
		var existing uint64
		if err = tx.QueryRowContext(ctx, "SELECT squad_id FROM match_tournament_member WHERE season_id=? AND player_id=? FOR UPDATE", cur.SeasonId, playerID).Scan(&existing); err == nil {
			return ErrTournamentAlreadyInSquad
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// 若当前首发空位未满,自动分配下一个空闲首发位;否则为替补。
		members, err := loadSquadMembersTx(ctx, tx, squadID)
		if err != nil {
			return err
		}
		usedSlots := make(map[uint32]bool, len(members))
		for _, m := range members {
			if m.StarterSlot > 0 {
				usedSlots[m.StarterSlot] = true
			}
		}
		role := uint32(pb.TournamentMemberRole_TOURNAMENT_MEMBER_ROLE_SUBSTITUTE)
		slot := uint32(0)
		for cand := uint32(1); cand <= cur.BattleTeamSize; cand++ {
			if !usedSlots[cand] {
				role = uint32(pb.TournamentMemberRole_TOURNAMENT_MEMBER_ROLE_STARTER)
				slot = cand
				break
			}
		}
		if _, err = tx.ExecContext(ctx,
			"INSERT INTO match_tournament_member("+tournamentMemberCols+") VALUES(?,?,?,?,?,?,?,?)",
			cur.SeasonId, playerID, squadID, role, slot, playerName, zoneID, stamp,
		); err != nil {
			var dup *mysql.MySQLError
			if errors.As(err, &dup) && dup.Number == 1062 {
				return ErrTournamentAlreadyInSquad
			}
			return err
		}
		squad.MemberCount++
		squad.UpdatedMs = stamp
		if _, err = tx.ExecContext(ctx, "UPDATE match_tournament_squad SET member_count=?,updated_ms=? WHERE squad_id=?", squad.MemberCount, stamp, squadID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE match_tournament_invite SET state=2 WHERE squad_id=? AND invitee_player_id=?", squadID, playerID); err != nil {
			return err
		}
		// 入队后清理本人本季其余待处理邀请。
		if _, err = tx.ExecContext(ctx, "UPDATE match_tournament_invite SET state=4 WHERE invitee_player_id=? AND season_id=? AND state=1 AND squad_id<>?", playerID, cur.SeasonId, squadID); err != nil {
			return err
		}
		members, err = loadSquadMembersTx(ctx, tx, squadID)
		if err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		view = toTournamentSquadView(squad, members)
		return nil
	})
	return view, err
}

// LeaveTournamentSquad 成员主动退队、队长踢人或队长解散未锁定战队。
func (s *Store) LeaveTournamentSquad(ctx context.Context, actorID, targetPlayerID uint64) (disbanded bool, view *pb.TournamentSquadView, err error) {
	if err := s.ensureTournamentOpen(); err != nil {
		return false, nil, err
	}
	if targetPlayerID == 0 {
		targetPlayerID = actorID
	}
	err = s.Fenced(ctx, func() error {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		stamp, err := now(ctx, tx)
		if err != nil {
			return err
		}
		cur, err := s.ensureTournamentSeasonTx(ctx, tx, stamp)
		if err != nil {
			return err
		}
		if cur.Stage != uint32(pb.TournamentStage_TOURNAMENT_STAGE_REGISTRATION) {
			return ErrTournamentStageClosed
		}
		var squadID uint64
		if err = tx.QueryRowContext(ctx, "SELECT squad_id FROM match_tournament_member WHERE season_id=? AND player_id=?", cur.SeasonId, actorID).Scan(&squadID); errors.Is(err, sql.ErrNoRows) {
			return ErrTournamentSquadNotFound
		} else if err != nil {
			return err
		}
		squad, err := scanTournamentSquad(tx.QueryRowContext(ctx, "SELECT "+tournamentSquadCols+" FROM match_tournament_squad WHERE squad_id=? FOR UPDATE", squadID))
		if err != nil {
			return err
		}
		if squad.State != uint32(pb.TournamentSquadState_TOURNAMENT_SQUAD_STATE_DRAFT) {
			return ErrTournamentSquadLocked
		}
		if targetPlayerID != actorID && squad.CaptainPlayerId != actorID {
			return ErrTournamentNotCaptain
		}
		var targetSquad uint64
		if err = tx.QueryRowContext(ctx, "SELECT squad_id FROM match_tournament_member WHERE season_id=? AND player_id=?", cur.SeasonId, targetPlayerID).Scan(&targetSquad); errors.Is(err, sql.ErrNoRows) || targetSquad != squadID {
			return ErrTournamentSquadNotFound
		} else if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM match_tournament_member WHERE season_id=? AND player_id=?", cur.SeasonId, targetPlayerID); err != nil {
			return err
		}
		remaining, err := loadSquadMembersTx(ctx, tx, squadID)
		if err != nil {
			return err
		}
		if len(remaining) == 0 {
			// 最后一人退出:解散战队并释放归一化名(后缀附带 squad_id 释放原名给别人)。
			releasedNorm := fmt.Sprintf("__disbanded_%d_%s", squadID, squad.NameNorm)
			if len(releasedNorm) > 190 {
				releasedNorm = releasedNorm[:190]
			}
			if _, err = tx.ExecContext(ctx, "UPDATE match_tournament_squad SET state=?,member_count=0,name_norm=?,updated_ms=? WHERE squad_id=?",
				uint32(pb.TournamentSquadState_TOURNAMENT_SQUAD_STATE_DISBANDED), releasedNorm, stamp, squadID); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "UPDATE match_tournament_invite SET state=4 WHERE squad_id=? AND state=1", squadID); err != nil {
				return err
			}
			disbanded = true
			return tx.Commit()
		}
		// 若队长主动退出,顺位把队长交给最早入队的剩余成员。
		if targetPlayerID == squad.CaptainPlayerId {
			newCaptain := remaining[0].PlayerId
			squad.CaptainPlayerId = newCaptain
			newSlot := remaining[0].StarterSlot
			if newSlot == 0 {
				newSlot = 1
			}
			if _, err = tx.ExecContext(ctx, "UPDATE match_tournament_member SET role=?,starter_slot=? WHERE season_id=? AND player_id=?",
				uint32(pb.TournamentMemberRole_TOURNAMENT_MEMBER_ROLE_CAPTAIN), newSlot, cur.SeasonId, newCaptain); err != nil {
				return err
			}
		}
		squad.MemberCount = uint32(len(remaining))
		squad.UpdatedMs = stamp
		if _, err = tx.ExecContext(ctx, "UPDATE match_tournament_squad SET captain_player_id=?,member_count=?,updated_ms=? WHERE squad_id=?",
			squad.CaptainPlayerId, squad.MemberCount, stamp, squadID); err != nil {
			return err
		}
		if targetPlayerID != actorID {
			remaining, err = loadSquadMembersTx(ctx, tx, squadID)
			if err != nil {
				return err
			}
			view = toTournamentSquadView(squad, remaining)
		}
		return tx.Commit()
	})
	return disbanded, view, err
}

// UpdateTournamentLineup 队长调整首发与替补名单(可在报名期与海选/淘汰备战期调整,仅限本队成员)。
func (s *Store) UpdateTournamentLineup(ctx context.Context, captainID uint64, starterIDs []uint64) (*pb.TournamentSquadView, error) {
	if err := s.ensureTournamentOpen(); err != nil {
		return nil, err
	}
	var view *pb.TournamentSquadView
	err := s.Fenced(ctx, func() error {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		stamp, err := now(ctx, tx)
		if err != nil {
			return err
		}
		cur, err := s.ensureTournamentSeasonTx(ctx, tx, stamp)
		if err != nil {
			return err
		}
		if cur.Stage == uint32(pb.TournamentStage_TOURNAMENT_STAGE_SETTLED) {
			return ErrTournamentStageClosed
		}
		var squadID uint64
		if err = tx.QueryRowContext(ctx, "SELECT squad_id FROM match_tournament_member WHERE season_id=? AND player_id=?", cur.SeasonId, captainID).Scan(&squadID); errors.Is(err, sql.ErrNoRows) {
			return ErrTournamentSquadNotFound
		} else if err != nil {
			return err
		}
		squad, err := scanTournamentSquad(tx.QueryRowContext(ctx, "SELECT "+tournamentSquadCols+" FROM match_tournament_squad WHERE squad_id=? FOR UPDATE", squadID))
		if err != nil {
			return err
		}
		if squad.CaptainPlayerId != captainID {
			return ErrTournamentNotCaptain
		}
		if len(starterIDs) != int(cur.BattleTeamSize) {
			return ErrTournamentLineupInvalid
		}
		members, err := loadSquadMembersTx(ctx, tx, squadID)
		if err != nil {
			return err
		}
		memberSet := make(map[uint64]bool, len(members))
		for _, m := range members {
			memberSet[m.PlayerId] = true
		}
		slotByPID := make(map[uint64]uint32, len(starterIDs))
		for i, pid := range starterIDs {
			if pid == 0 || !memberSet[pid] || slotByPID[pid] != 0 {
				return ErrTournamentLineupInvalid
			}
			slotByPID[pid] = uint32(i + 1)
		}
		for _, m := range members {
			slot := slotByPID[m.PlayerId]
			role := uint32(pb.TournamentMemberRole_TOURNAMENT_MEMBER_ROLE_SUBSTITUTE)
			if m.PlayerId == squad.CaptainPlayerId {
				role = uint32(pb.TournamentMemberRole_TOURNAMENT_MEMBER_ROLE_CAPTAIN)
			} else if slot > 0 {
				role = uint32(pb.TournamentMemberRole_TOURNAMENT_MEMBER_ROLE_STARTER)
			}
			if _, err = tx.ExecContext(ctx, "UPDATE match_tournament_member SET role=?,starter_slot=? WHERE season_id=? AND player_id=?", role, slot, cur.SeasonId, m.PlayerId); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, "UPDATE match_tournament_squad SET updated_ms=? WHERE squad_id=?", stamp, squadID); err != nil {
			return err
		}
		updatedMembers, err := loadSquadMembersTx(ctx, tx, squadID)
		if err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		view = toTournamentSquadView(squad, updatedMembers)
		return nil
	})
	return view, err
}

// RegisterTournamentSquad 队长确认锁定本赛季战队报名(锁定后不再允许增删队员)。
func (s *Store) RegisterTournamentSquad(ctx context.Context, captainID uint64) (*pb.TournamentSquadView, error) {
	if err := s.ensureTournamentOpen(); err != nil {
		return nil, err
	}
	var view *pb.TournamentSquadView
	err := s.Fenced(ctx, func() error {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		stamp, err := now(ctx, tx)
		if err != nil {
			return err
		}
		cur, err := s.ensureTournamentSeasonTx(ctx, tx, stamp)
		if err != nil {
			return err
		}
		if cur.Stage != uint32(pb.TournamentStage_TOURNAMENT_STAGE_REGISTRATION) {
			return ErrTournamentStageClosed
		}
		var squadID uint64
		if err = tx.QueryRowContext(ctx, "SELECT squad_id FROM match_tournament_member WHERE season_id=? AND player_id=?", cur.SeasonId, captainID).Scan(&squadID); errors.Is(err, sql.ErrNoRows) {
			return ErrTournamentSquadNotFound
		} else if err != nil {
			return err
		}
		squad, err := scanTournamentSquad(tx.QueryRowContext(ctx, "SELECT "+tournamentSquadCols+" FROM match_tournament_squad WHERE squad_id=? FOR UPDATE", squadID))
		if err != nil {
			return err
		}
		if squad.CaptainPlayerId != captainID {
			return ErrTournamentNotCaptain
		}
		if squad.State == uint32(pb.TournamentSquadState_TOURNAMENT_SQUAD_STATE_REGISTERED) {
			members, err := loadSquadMembersTx(ctx, tx, squadID)
			if err != nil {
				return err
			}
			view = toTournamentSquadView(squad, members)
			return tx.Commit()
		}
		if squad.State != uint32(pb.TournamentSquadState_TOURNAMENT_SQUAD_STATE_DRAFT) {
			return ErrTournamentSquadLocked
		}
		if squad.MemberCount < cur.BattleTeamSize {
			return ErrTournamentMemberNotEnough
		}
		squad.State = uint32(pb.TournamentSquadState_TOURNAMENT_SQUAD_STATE_REGISTERED)
		squad.RegisteredMs = stamp
		squad.UpdatedMs = stamp
		if _, err = tx.ExecContext(ctx, "UPDATE match_tournament_squad SET state=?,registered_ms=?,updated_ms=? WHERE squad_id=?",
			squad.State, stamp, stamp, squadID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE match_tournament_invite SET state=4 WHERE squad_id=? AND state=1", squadID); err != nil {
			return err
		}
		members, err := loadSquadMembersTx(ctx, tx, squadID)
		if err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		view = toTournamentSquadView(squad, members)
		return nil
	})
	return view, err
}

// CheerTournamentSquad 全服玩家免费为一支本赛季有效战队助威(每人每季限 1 票,零消耗零押注)。
func (s *Store) CheerTournamentSquad(ctx context.Context, playerID, squadID uint64) (uint32, error) {
	if err := s.ensureTournamentOpen(); err != nil {
		return 0, err
	}
	var newCount uint32
	err := s.Fenced(ctx, func() error {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		stamp, err := now(ctx, tx)
		if err != nil {
			return err
		}
		cur, err := s.ensureTournamentSeasonTx(ctx, tx, stamp)
		if err != nil {
			return err
		}
		if cur.Stage == uint32(pb.TournamentStage_TOURNAMENT_STAGE_SETTLED) {
			return ErrTournamentStageClosed
		}
		squad, err := scanTournamentSquad(tx.QueryRowContext(ctx, "SELECT "+tournamentSquadCols+" FROM match_tournament_squad WHERE squad_id=? FOR UPDATE", squadID))
		if errors.Is(err, sql.ErrNoRows) || squad.SeasonId != cur.SeasonId {
			return ErrTournamentSquadNotFound
		}
		if err != nil {
			return err
		}
		if squad.State != uint32(pb.TournamentSquadState_TOURNAMENT_SQUAD_STATE_REGISTERED) {
			return ErrTournamentSquadNotFound
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO match_tournament_cheer(season_id,player_id,squad_id,rewarded,created_ms) VALUES(?,?,?,0,?)",
			cur.SeasonId, playerID, squadID, stamp); err != nil {
			var dup *mysql.MySQLError
			if errors.As(err, &dup) && dup.Number == 1062 {
				return ErrTournamentAlreadyCheered
			}
			return err
		}
		newCount = squad.CheerCount + 1
		if _, err = tx.ExecContext(ctx, "UPDATE match_tournament_squad SET cheer_count=?,updated_ms=? WHERE squad_id=?", newCount, stamp, squadID); err != nil {
			return err
		}
		return tx.Commit()
	})
	return newCount, err
}

// LoadTournamentOverview 加载主厅总览(赛季状态、我的战队、邀请、活跃对局、精选观战、助威、奖励与冠军殿堂)。
func (s *Store) LoadTournamentOverview(ctx context.Context, playerID uint64, includeHistory bool) (*pb.GetTournamentOverviewResponse, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	stamp, err := now(ctx, tx)
	if err != nil {
		return nil, err
	}
	cur, err := s.ensureTournamentSeasonTx(ctx, tx, stamp)
	if err != nil {
		return nil, err
	}
	var regCount uint32
	_ = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM match_tournament_squad WHERE season_id=? AND state IN(2,3,4)", cur.SeasonId).Scan(&regCount)

	resp := &pb.GetTournamentOverviewResponse{
		CurrentSeason: s.toTournamentSeasonView(cur, regCount),
		RewardTiers:   s.TournamentCatalog().RewardTierViews(),
		ServerTimeMs:  stamp,
	}

	// 我的战队
	var mySquadID uint64
	if err = tx.QueryRowContext(ctx, "SELECT squad_id FROM match_tournament_member WHERE season_id=? AND player_id=?", cur.SeasonId, playerID).Scan(&mySquadID); err == nil && mySquadID != 0 {
		if sq, e := scanTournamentSquad(tx.QueryRowContext(ctx, "SELECT "+tournamentSquadCols+" FROM match_tournament_squad WHERE squad_id=?", mySquadID)); e == nil {
			members, e2 := loadSquadMembersTx(ctx, tx, mySquadID)
			if e2 != nil {
				return nil, e2
			}
			resp.MySquad = toTournamentSquadView(sq, members)
		}
	}

	// 收集需要展示名称的 squad 索引
	squadCache := map[uint64]*pb.MatchTournamentSquadRecord{}
	loadSquad := func(id uint64) *pb.MatchTournamentSquadRecord {
		if id == 0 {
			return nil
		}
		if sq, ok := squadCache[id]; ok {
			return sq
		}
		sq, e := scanTournamentSquad(tx.QueryRowContext(ctx, "SELECT "+tournamentSquadCols+" FROM match_tournament_squad WHERE squad_id=?", id))
		if e == nil {
			squadCache[id] = sq
		}
		return sq
	}

	// 待处理邀请
	invRows, err := tx.QueryContext(ctx, "SELECT "+tournamentInviteCols+" FROM match_tournament_invite WHERE invitee_player_id=? AND state=1 AND expires_ms>? ORDER BY created_ms DESC LIMIT 20", playerID, stamp)
	if err != nil {
		return nil, err
	}
	var rawInvites []*pb.MatchTournamentInviteRecord
	for invRows.Next() {
		inv, e := scanTournamentInvite(invRows)
		if e != nil {
			invRows.Close()
			return nil, e
		}
		if inv.SeasonId == cur.SeasonId {
			rawInvites = append(rawInvites, inv)
		}
	}
	invRows.Close()
	for _, inv := range rawInvites {
		sq := loadSquad(inv.SquadId)
		if sq == nil || sq.State != uint32(pb.TournamentSquadState_TOURNAMENT_SQUAD_STATE_DRAFT) {
			continue
		}
		var inviterName string
		_ = tx.QueryRowContext(ctx, "SELECT player_name FROM match_tournament_member WHERE season_id=? AND player_id=?", cur.SeasonId, inv.InviterPlayerId).Scan(&inviterName)
		resp.PendingInvites = append(resp.PendingInvites, &pb.TournamentInviteView{
			SquadId:            inv.SquadId,
			SeasonId:           inv.SeasonId,
			SquadName:          sq.Name,
			InviterPlayerId:    inv.InviterPlayerId,
			InviterName:        inviterName,
			CurrentMemberCount: sq.MemberCount,
			RosterMaxSize:      cur.RosterMaxSize,
			CreatedMs:          inv.CreatedMs,
			ExpiresMs:          inv.ExpiresMs,
		})
	}

	// 我的活跃对局(等待对手 / 待开打 / 激战中)
	if mySquadID != 0 {
		mRow := tx.QueryRowContext(ctx,
			"SELECT "+tournamentMatchCols+" FROM match_tournament_match WHERE season_id=? AND (squad_a_id=? OR squad_b_id=?) AND state IN(1,2,3) ORDER BY phase DESC, round_index DESC, match_id DESC LIMIT 1",
			cur.SeasonId, mySquadID, mySquadID)
		if m, e := scanTournamentMatch(mRow); e == nil {
			loadSquad(m.SquadAId)
			loadSquad(m.SquadBId)
			resp.MyActiveMatch = toTournamentMatchView(m, squadCache)
		}
	}

	// 当前进行中的全服精选观战局
	featRows, err := tx.QueryContext(ctx, "SELECT "+tournamentMatchCols+" FROM match_tournament_match WHERE season_id=? AND state=3 ORDER BY featured_spectate DESC, phase DESC, round_index DESC LIMIT 10", cur.SeasonId)
	if err != nil {
		return nil, err
	}
	var featMatches []*pb.MatchTournamentMatchRecord
	for featRows.Next() {
		m, e := scanTournamentMatch(featRows)
		if e != nil {
			featRows.Close()
			return nil, e
		}
		featMatches = append(featMatches, m)
	}
	featRows.Close()
	for _, m := range featMatches {
		loadSquad(m.SquadAId)
		loadSquad(m.SquadBId)
		resp.FeaturedMatches = append(resp.FeaturedMatches, toTournamentMatchView(m, squadCache))
	}

	// 本季助威记录
	var cheeredID uint64
	if err = tx.QueryRowContext(ctx, "SELECT squad_id FROM match_tournament_cheer WHERE season_id=? AND player_id=?", cur.SeasonId, playerID).Scan(&cheeredID); err == nil && cheeredID != 0 {
		resp.MyCheeredSquadId = cheeredID
		if sq := loadSquad(cheeredID); sq != nil {
			resp.MyCheeredSquadName = sq.Name
		}
	}

	// 我的奖励账单(本季 + 历史)
	rewQuery := "SELECT " + tournamentRewardCols + " FROM match_tournament_reward WHERE player_id=? AND season_id=? ORDER BY tier_kind ASC"
	rewArgs := []any{playerID, cur.SeasonId}
	if includeHistory {
		rewQuery = "SELECT " + tournamentRewardCols + " FROM match_tournament_reward WHERE player_id=? ORDER BY season_id DESC, tier_kind ASC LIMIT 50"
		rewArgs = []any{playerID}
	}
	rewRows, err := tx.QueryContext(ctx, rewQuery, rewArgs...)
	if err != nil {
		return nil, err
	}
	for rewRows.Next() {
		r, e := scanTournamentReward(rewRows)
		if e != nil {
			rewRows.Close()
			return nil, e
		}
		resp.MyRewards = append(resp.MyRewards, toTournamentRewardView(r))
	}
	rewRows.Close()

	// 历届冠军殿堂
	if includeHistory {
		hRows, err := tx.QueryContext(ctx, "SELECT "+tournamentSeasonCols+" FROM match_tournament_season WHERE stage=? AND champion_squad_id>0 ORDER BY season_id DESC LIMIT 20",
			uint32(pb.TournamentStage_TOURNAMENT_STAGE_SETTLED))
		if err != nil {
			return nil, err
		}
		for hRows.Next() {
			hs, e := scanTournamentSeason(hRows)
			if e != nil {
				hRows.Close()
				return nil, e
			}
			resp.HallOfFame = append(resp.HallOfFame, s.toTournamentSeasonView(hs, 0))
		}
		hRows.Close()
	}

	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return resp, nil
}

// ListTournamentStandings 分页返回指定赛季的全服战队积分榜与淘汰赛对阵树。
func (s *Store) ListTournamentStandings(ctx context.Context, seasonID uint32, playerID uint64, page, pageSize uint32) (*pb.ListTournamentStandingsResponse, error) {
	if page == 0 {
		page = 1
	}
	if pageSize == 0 {
		pageSize = 20
	}
	if pageSize > 50 {
		pageSize = 50
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	stamp, err := now(ctx, tx)
	if err != nil {
		return nil, err
	}
	var season *pb.MatchTournamentSeasonRecord
	if seasonID == 0 {
		season, err = s.ensureTournamentSeasonTx(ctx, tx, stamp)
	} else {
		season, err = scanTournamentSeason(tx.QueryRowContext(ctx, "SELECT "+tournamentSeasonCols+" FROM match_tournament_season WHERE season_id=?", seasonID))
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTournamentSquadNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+tournamentSquadCols+" FROM match_tournament_squad WHERE season_id=? AND state IN(2,3,4)", season.SeasonId)
	if err != nil {
		return nil, err
	}
	var all []*pb.MatchTournamentSquadRecord
	byID := make(map[uint64]*pb.MatchTournamentSquadRecord)
	for rows.Next() {
		sq, e := scanTournamentSquad(rows)
		if e != nil {
			rows.Close()
			return nil, e
		}
		all = append(all, sq)
		byID[sq.SquadId] = sq
	}
	rows.Close()

	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		// 已定最终名次按 final_rank 升序排前;其余按确定性海选积分序排。
		if a.FinalRank > 0 || b.FinalRank > 0 {
			if a.FinalRank == 0 {
				return false
			}
			if b.FinalRank == 0 {
				return true
			}
			if a.FinalRank != b.FinalRank {
				return a.FinalRank < b.FinalRank
			}
		}
		if a.QualifierPoints != b.QualifierPoints {
			return a.QualifierPoints > b.QualifierPoints
		}
		if a.QualifierWins != b.QualifierWins {
			return a.QualifierWins > b.QualifierWins
		}
		if a.QualifierDraws != b.QualifierDraws {
			return a.QualifierDraws > b.QualifierDraws
		}
		if a.RegisteredMs != b.RegisteredMs {
			return a.RegisteredMs < b.RegisteredMs
		}
		return a.SquadId < b.SquadId
	})

	start := int((page - 1) * pageSize)
	end := start + int(pageSize)
	if start > len(all) {
		start = len(all)
	}
	if end > len(all) {
		end = len(all)
	}
	resp := &pb.ListTournamentStandingsResponse{
		Season:       s.toTournamentSeasonView(season, uint32(len(all))),
		TotalSquads:  uint32(len(all)),
		ServerTimeMs: stamp,
	}
	for _, sq := range all[start:end] {
		members, e := loadSquadMembersTx(ctx, tx, sq.SquadId)
		if e != nil {
			return nil, e
		}
		resp.Standings = append(resp.Standings, toTournamentSquadView(sq, members))
	}

	if playerID != 0 {
		var mySquadID uint64
		if tx.QueryRowContext(ctx, "SELECT squad_id FROM match_tournament_member WHERE season_id=? AND player_id=?", season.SeasonId, playerID).Scan(&mySquadID) == nil && mySquadID != 0 {
			if sq := byID[mySquadID]; sq != nil {
				members, e := loadSquadMembersTx(ctx, tx, mySquadID)
				if e != nil {
					return nil, e
				}
				resp.MySquad = toTournamentSquadView(sq, members)
			}
		}
	}

	// 读取淘汰赛对阵树
	mRows, err := tx.QueryContext(ctx, "SELECT "+tournamentMatchCols+" FROM match_tournament_match WHERE season_id=? AND phase=? ORDER BY round_index ASC, slot_index ASC",
		season.SeasonId, uint32(pb.TournamentMatchPhase_TOURNAMENT_MATCH_PHASE_KNOCKOUT))
	if err != nil {
		return nil, err
	}
	for mRows.Next() {
		m, e := scanTournamentMatch(mRows)
		if e != nil {
			mRows.Close()
			return nil, e
		}
		resp.KnockoutBracket = append(resp.KnockoutBracket, toTournamentMatchView(m, byID))
	}
	mRows.Close()

	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return resp, nil
}

// PrepareTournamentMatch 海选发起匹配或淘汰赛确认备战。当对阵双方均就绪时返回非空 TournamentLaunchSpec 供上层立即建房。
// newMatchID 须在事务外预分配(仅在海选新建等待单时消费)。
func (s *Store) PrepareTournamentMatch(ctx context.Context, captainID, matchID uint64, lineupIDs []uint64, newMatchID uint64) (*pb.TournamentMatchView, *TournamentLaunchSpec, error) {
	if err := s.ensureTournamentOpen(); err != nil {
		return nil, nil, err
	}
	var matchView *pb.TournamentMatchView
	var launch *TournamentLaunchSpec
	err := s.Fenced(ctx, func() error {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		stamp, err := now(ctx, tx)
		if err != nil {
			return err
		}
		cur, err := s.ensureTournamentSeasonTx(ctx, tx, stamp)
		if err != nil {
			return err
		}
		rule := s.TournamentCatalog().Rule()
		if cur.Stage != uint32(pb.TournamentStage_TOURNAMENT_STAGE_QUALIFIER) &&
			cur.Stage != uint32(pb.TournamentStage_TOURNAMENT_STAGE_KNOCKOUT) {
			return ErrTournamentStageClosed
		}
		var mySquadID uint64
		if err = tx.QueryRowContext(ctx, "SELECT squad_id FROM match_tournament_member WHERE season_id=? AND player_id=?", cur.SeasonId, captainID).Scan(&mySquadID); errors.Is(err, sql.ErrNoRows) {
			return ErrTournamentSquadNotFound
		} else if err != nil {
			return err
		}
		mySquad, err := scanTournamentSquad(tx.QueryRowContext(ctx, "SELECT "+tournamentSquadCols+" FROM match_tournament_squad WHERE squad_id=? FOR UPDATE", mySquadID))
		if err != nil {
			return err
		}
		if mySquad.CaptainPlayerId != captainID {
			return ErrTournamentNotCaptain
		}
		if mySquad.State != uint32(pb.TournamentSquadState_TOURNAMENT_SQUAD_STATE_REGISTERED) {
			return ErrTournamentStageClosed
		}
		members, err := loadSquadMembersTx(ctx, tx, mySquadID)
		if err != nil {
			return err
		}
		lineup, err := resolveAndValidateLineup(members, lineupIDs, int(cur.BattleTeamSize))
		if err != nil {
			return err
		}
		encodedLineup := tournament.EncodeLineup(lineup)

		// 淘汰赛分支:确认已有淘汰对阵就绪。
		if cur.Stage == uint32(pb.TournamentStage_TOURNAMENT_STAGE_KNOCKOUT) {
			var m *pb.MatchTournamentMatchRecord
			if matchID != 0 {
				m, err = scanTournamentMatch(tx.QueryRowContext(ctx, "SELECT "+tournamentMatchCols+" FROM match_tournament_match WHERE match_id=? FOR UPDATE", matchID))
			} else {
				m, err = scanTournamentMatch(tx.QueryRowContext(ctx,
					"SELECT "+tournamentMatchCols+" FROM match_tournament_match WHERE season_id=? AND phase=? AND (squad_a_id=? OR squad_b_id=?) AND state IN(1,2) ORDER BY round_index ASC LIMIT 1 FOR UPDATE",
					cur.SeasonId, uint32(pb.TournamentMatchPhase_TOURNAMENT_MATCH_PHASE_KNOCKOUT), mySquadID, mySquadID))
			}
			if errors.Is(err, sql.ErrNoRows) || m.SeasonId != cur.SeasonId || m.Phase != uint32(pb.TournamentMatchPhase_TOURNAMENT_MATCH_PHASE_KNOCKOUT) {
				return ErrTournamentMatchNotReady
			}
			if err != nil {
				return err
			}
			if m.State != uint32(pb.TournamentMatchState_TOURNAMENT_MATCH_STATE_WAITING_OPPONENT) &&
				m.State != uint32(pb.TournamentMatchState_TOURNAMENT_MATCH_STATE_READY) {
				return ErrTournamentMatchNotReady
			}
			if m.SquadAId == 0 || m.SquadBId == 0 {
				return ErrTournamentMatchNotReady
			}
			switch mySquadID {
			case m.SquadAId:
				m.LineupA = encodedLineup
				m.ReadyA = true
			case m.SquadBId:
				m.LineupB = encodedLineup
				m.ReadyB = true
			default:
				return ErrTournamentMatchNotReady
			}
			m.UpdatedMs = stamp
			if m.ReadyA && m.ReadyB {
				m.State = uint32(pb.TournamentMatchState_TOURNAMENT_MATCH_STATE_READY)
				launch = &TournamentLaunchSpec{
					MatchID:        m.MatchId,
					SeasonID:       cur.SeasonId,
					Phase:          pb.TournamentMatchPhase_TOURNAMENT_MATCH_PHASE_KNOCKOUT,
					RoundIndex:     m.RoundIndex,
					BattleConfigID: rule.GetBattleConfigId(),
					TeamA:          tournament.DecodeLineup(m.LineupA),
					TeamB:          tournament.DecodeLineup(m.LineupB),
					Featured:       true,
				}
			}
			if _, err = tx.ExecContext(ctx, "UPDATE match_tournament_match SET lineup_a=?,lineup_b=?,ready_a=?,ready_b=?,state=?,updated_ms=? WHERE match_id=?",
				nonNilBytes(m.LineupA), nonNilBytes(m.LineupB), m.ReadyA, m.ReadyB, m.State, stamp, m.MatchId); err != nil {
				return err
			}
			squads := map[uint64]*pb.MatchTournamentSquadRecord{mySquadID: mySquad}
			otherID := m.SquadAId
			if otherID == mySquadID {
				otherID = m.SquadBId
			}
			if other, e := scanTournamentSquad(tx.QueryRowContext(ctx, "SELECT "+tournamentSquadCols+" FROM match_tournament_squad WHERE squad_id=?", otherID)); e == nil {
				squads[otherID] = other
			}
			matchView = toTournamentMatchView(m, squads)
			return tx.Commit()
		}

		// 海选积分赛分支:检查海选场次上限 + 是否已有活跃海选对局。
		if mySquad.QualifierPlayed >= rule.GetQualifierMaxMatches() {
			return ErrTournamentQualifierLimit
		}
		if existing, e := scanTournamentMatch(tx.QueryRowContext(ctx,
			"SELECT "+tournamentMatchCols+" FROM match_tournament_match WHERE season_id=? AND phase=? AND (squad_a_id=? OR squad_b_id=?) AND state IN(1,2,3) LIMIT 1 FOR UPDATE",
			cur.SeasonId, uint32(pb.TournamentMatchPhase_TOURNAMENT_MATCH_PHASE_QUALIFIER), mySquadID, mySquadID)); e == nil {
			// 已在等待对手:刷新阵容并幂等返回。
			if existing.State == uint32(pb.TournamentMatchState_TOURNAMENT_MATCH_STATE_WAITING_OPPONENT) && existing.SquadAId == mySquadID {
				existing.LineupA = encodedLineup
				existing.UpdatedMs = stamp
				_, _ = tx.ExecContext(ctx, "UPDATE match_tournament_match SET lineup_a=?,updated_ms=? WHERE match_id=?", nonNilBytes(encodedLineup), stamp, existing.MatchId)
			}
			matchView = toTournamentMatchView(existing, map[uint64]*pb.MatchTournamentSquadRecord{mySquadID: mySquad})
			return tx.Commit()
		}

		// 统计全服活跃海选战队数以确定同对交手上限(<=4 队允许至多 2 场,否则至多 1 场防刷分)。
		var totalReg uint32
		_ = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM match_tournament_squad WHERE season_id=? AND state=?",
			cur.SeasonId, uint32(pb.TournamentSquadState_TOURNAMENT_SQUAD_STATE_REGISTERED)).Scan(&totalReg)
		maxPairMeetings := uint32(1)
		if totalReg <= 4 {
			maxPairMeetings = 2
		}

		// 查找一支已在海选等待中的对手队(跳过超同对交手上限者)。
		waitRows, err := tx.QueryContext(ctx,
			"SELECT "+tournamentMatchCols+" FROM match_tournament_match WHERE season_id=? AND phase=? AND state=? AND squad_a_id<>? AND squad_b_id=0 ORDER BY match_id ASC LIMIT 16 FOR UPDATE",
			cur.SeasonId, uint32(pb.TournamentMatchPhase_TOURNAMENT_MATCH_PHASE_QUALIFIER),
			uint32(pb.TournamentMatchState_TOURNAMENT_MATCH_STATE_WAITING_OPPONENT), mySquadID)
		if err != nil {
			return err
		}
		var waiting []*pb.MatchTournamentMatchRecord
		for waitRows.Next() {
			w, e := scanTournamentMatch(waitRows)
			if e != nil {
				waitRows.Close()
				return e
			}
			waiting = append(waiting, w)
		}
		waitRows.Close()

		var paired *pb.MatchTournamentMatchRecord
		for _, cand := range waiting {
			var met uint32
			_ = tx.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM match_tournament_match WHERE season_id=? AND phase=? AND state IN(2,3,4,6) AND ((squad_a_id=? AND squad_b_id=?) OR (squad_a_id=? AND squad_b_id=?))",
				cur.SeasonId, uint32(pb.TournamentMatchPhase_TOURNAMENT_MATCH_PHASE_QUALIFIER),
				mySquadID, cand.SquadAId, cand.SquadAId, mySquadID).Scan(&met)
			if met < maxPairMeetings {
				paired = cand
				break
			}
		}

		if paired != nil {
			paired.SquadBId = mySquadID
			paired.LineupB = encodedLineup
			paired.ReadyB = true
			paired.State = uint32(pb.TournamentMatchState_TOURNAMENT_MATCH_STATE_READY)
			paired.UpdatedMs = stamp
			if _, err = tx.ExecContext(ctx, "UPDATE match_tournament_match SET squad_b_id=?,lineup_b=?,ready_b=1,state=?,updated_ms=? WHERE match_id=?",
				paired.SquadBId, nonNilBytes(paired.LineupB), paired.State, stamp, paired.MatchId); err != nil {
				return err
			}
			launch = &TournamentLaunchSpec{
				MatchID:        paired.MatchId,
				SeasonID:       cur.SeasonId,
				Phase:          pb.TournamentMatchPhase_TOURNAMENT_MATCH_PHASE_QUALIFIER,
				RoundIndex:     paired.RoundIndex,
				BattleConfigID: rule.GetBattleConfigId(),
				TeamA:          tournament.DecodeLineup(paired.LineupA),
				TeamB:          lineup,
				Featured:       false,
			}
			squads := map[uint64]*pb.MatchTournamentSquadRecord{mySquadID: mySquad}
			if other, e := scanTournamentSquad(tx.QueryRowContext(ctx, "SELECT "+tournamentSquadCols+" FROM match_tournament_squad WHERE squad_id=?", paired.SquadAId)); e == nil {
				squads[paired.SquadAId] = other
			}
			matchView = toTournamentMatchView(paired, squads)
			return tx.Commit()
		}

		// 尚无对手:挂出一条海选等待对局单。
		if newMatchID == 0 {
			return errors.New("tournament: missing pre-allocated match_id")
		}
		created := &pb.MatchTournamentMatchRecord{
			MatchId:          newMatchID,
			SeasonId:         cur.SeasonId,
			Phase:            uint32(pb.TournamentMatchPhase_TOURNAMENT_MATCH_PHASE_QUALIFIER),
			RoundIndex:       mySquad.QualifierPlayed + 1,
			SlotIndex:        0,
			SquadAId:         mySquadID,
			SquadBId:         0,
			LineupA:          encodedLineup,
			LineupB:          []byte{},
			ReadyA:           true,
			ReadyB:           false,
			State:            uint32(pb.TournamentMatchState_TOURNAMENT_MATCH_STATE_WAITING_OPPONENT),
			ScheduledStartMs: stamp,
			DeadlineMs:       cur.KnockoutStartMs,
			UpdatedMs:        stamp,
		}
		if _, err = tx.ExecContext(ctx,
			"INSERT INTO match_tournament_match("+tournamentMatchCols+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
			created.MatchId, created.SeasonId, created.Phase, created.RoundIndex, created.SlotIndex,
			created.SquadAId, created.SquadBId, nonNilBytes(created.LineupA), nonNilBytes(created.LineupB),
			created.ReadyA, created.ReadyB, created.State, 0, 0, 0, false,
			created.ScheduledStartMs, created.DeadlineMs, 0, stamp,
		); err != nil {
			return err
		}
		matchView = toTournamentMatchView(created, map[uint64]*pb.MatchTournamentSquadRecord{mySquadID: mySquad})
		return tx.Commit()
	})
	return matchView, launch, err
}

func resolveAndValidateLineup(members []*pb.TournamentMemberView, requested []uint64, required int) ([]uint64, error) {
	if len(members) < required {
		return nil, ErrTournamentMemberNotEnough
	}
	if len(requested) == 0 {
		// 未显式传阵容则按已保存首发槽位取前 required 人。
		var starters []uint64
		for _, m := range members {
			if m.StarterSlot > 0 {
				starters = append(starters, m.PlayerId)
			}
		}
		if len(starters) < required {
			starters = starters[:0]
			for i := 0; i < required && i < len(members); i++ {
				starters = append(starters, members[i].PlayerId)
			}
		}
		if len(starters) != required {
			return nil, ErrTournamentLineupInvalid
		}
		return starters, nil
	}
	if len(requested) != required {
		return nil, ErrTournamentLineupInvalid
	}
	valid := make(map[uint64]bool, len(members))
	for _, m := range members {
		valid[m.PlayerId] = true
	}
	seen := make(map[uint64]bool, required)
	out := make([]uint64, 0, required)
	for _, pid := range requested {
		if pid == 0 || !valid[pid] || seen[pid] {
			return nil, ErrTournamentLineupInvalid
		}
		seen[pid] = true
		out = append(out, pid)
	}
	return out, nil
}

// BindTournamentMatchBattle 建房保留成功后把 match_id 与 battle_id 原子绑定;建房前失败则释放就绪态供重试。
func (s *Store) BindTournamentMatchBattle(ctx context.Context, matchID, battleID uint64, featured bool) error {
	return s.Fenced(ctx, func() error {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		stamp, err := now(ctx, tx)
		if err != nil {
			return err
		}
		if battleID == 0 {
			// 回滚就绪态(海选回到等待对手,淘汰赛清空就绪待双方重新确认)。
			m, err := scanTournamentMatch(tx.QueryRowContext(ctx, "SELECT "+tournamentMatchCols+" FROM match_tournament_match WHERE match_id=? FOR UPDATE", matchID))
			if err != nil {
				return err
			}
			if m.State != uint32(pb.TournamentMatchState_TOURNAMENT_MATCH_STATE_READY) {
				return tx.Commit()
			}
			if m.Phase == uint32(pb.TournamentMatchPhase_TOURNAMENT_MATCH_PHASE_QUALIFIER) {
				_, err = tx.ExecContext(ctx, "UPDATE match_tournament_match SET squad_b_id=0,lineup_b=?,ready_b=0,state=?,updated_ms=? WHERE match_id=?",
					[]byte{}, uint32(pb.TournamentMatchState_TOURNAMENT_MATCH_STATE_WAITING_OPPONENT), stamp, matchID)
			} else {
				_, err = tx.ExecContext(ctx, "UPDATE match_tournament_match SET ready_a=0,ready_b=0,state=?,updated_ms=? WHERE match_id=?",
					uint32(pb.TournamentMatchState_TOURNAMENT_MATCH_STATE_WAITING_OPPONENT), stamp, matchID)
			}
			if err != nil {
				return err
			}
			return tx.Commit()
		}
		_, err = tx.ExecContext(ctx, "UPDATE match_tournament_match SET battle_id=?,state=?,featured_spectate=?,updated_ms=? WHERE match_id=? AND state IN(1,2)",
			battleID, uint32(pb.TournamentMatchState_TOURNAMENT_MATCH_STATE_IN_BATTLE), featured, stamp, matchID)
		if err != nil {
			return err
		}
		return tx.Commit()
	})
}

// ApplyTournamentBattleResult 幂等消费一场锦标赛终局结果:更新海选积分或推进淘汰赛晋级与冠军。
func (s *Store) ApplyTournamentBattleResult(ctx context.Context, event *kafkapb.BattleResultEvent, roundLimit uint32) (bool, error) {
	if event == nil || event.GetBattleId() == 0 {
		return false, nil
	}
	var applied bool
	err := s.Fenced(ctx, func() error {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		stamp, err := now(ctx, tx)
		if err != nil {
			return err
		}
		m, err := scanTournamentMatch(tx.QueryRowContext(ctx, "SELECT "+tournamentMatchCols+" FROM match_tournament_match WHERE battle_id=? FOR UPDATE", event.GetBattleId()))
		if errors.Is(err, sql.ErrNoRows) {
			return tx.Commit()
		}
		if err != nil {
			return err
		}
		if m.State == uint32(pb.TournamentMatchState_TOURNAMENT_MATCH_STATE_FINISHED) ||
			m.State == uint32(pb.TournamentMatchState_TOURNAMENT_MATCH_STATE_FORFEIT) {
			return tx.Commit()
		}
		season, err := scanTournamentSeason(tx.QueryRowContext(ctx, "SELECT "+tournamentSeasonCols+" FROM match_tournament_season WHERE season_id=? FOR UPDATE", m.SeasonId))
		if err != nil {
			return err
		}
		// 统一按 squad_id 升序加锁避免死锁。
		firstID, secondID := m.SquadAId, m.SquadBId
		if firstID > secondID {
			firstID, secondID = secondID, firstID
		}
		sqFirst, err := scanTournamentSquad(tx.QueryRowContext(ctx, "SELECT "+tournamentSquadCols+" FROM match_tournament_squad WHERE squad_id=? FOR UPDATE", firstID))
		if err != nil {
			return err
		}
		sqSecond, err := scanTournamentSquad(tx.QueryRowContext(ctx, "SELECT "+tournamentSquadCols+" FROM match_tournament_squad WHERE squad_id=? FOR UPDATE", secondID))
		if err != nil {
			return err
		}
		sqA, sqB := sqFirst, sqSecond
		if sqA.SquadId != m.SquadAId {
			sqA, sqB = sqSecond, sqFirst
		}

		outcome := event.GetOutcome()
		if outcome == battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_B_WIN && event.GetTotalRounds() >= roundLimit && roundLimit > 0 {
			// 回合打满检查 A 侧仍有存活则按平局口径。
			if hasSideSurvivors(event, 0) {
				outcome = battlepb.EBattleOutcome_BATTLE_OUTCOME_DRAW
			}
		}

		rule := s.TournamentCatalog().Rule()
		var winnerSquadID uint64
		if m.Phase == uint32(pb.TournamentMatchPhase_TOURNAMENT_MATCH_PHASE_QUALIFIER) {
			sqA.QualifierPlayed++
			sqB.QualifierPlayed++
			switch outcome {
			case battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN:
				winnerSquadID = sqA.SquadId
				sqA.QualifierWins++
				sqA.QualifierPoints += rule.GetQualifierWinPoints()
				sqB.QualifierLosses++
				sqB.QualifierPoints += rule.GetQualifierLossPoints()
			case battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_B_WIN:
				winnerSquadID = sqB.SquadId
				sqB.QualifierWins++
				sqB.QualifierPoints += rule.GetQualifierWinPoints()
				sqA.QualifierLosses++
				sqA.QualifierPoints += rule.GetQualifierLossPoints()
			default:
				sqA.QualifierDraws++
				sqA.QualifierPoints += rule.GetQualifierDrawPoints()
				sqB.QualifierDraws++
				sqB.QualifierPoints += rule.GetQualifierDrawPoints()
			}
			if _, err = tx.ExecContext(ctx, "UPDATE match_tournament_squad SET qualifier_points=?,qualifier_played=?,qualifier_wins=?,qualifier_losses=?,qualifier_draws=?,updated_ms=? WHERE squad_id=?",
				sqA.QualifierPoints, sqA.QualifierPlayed, sqA.QualifierWins, sqA.QualifierLosses, sqA.QualifierDraws, stamp, sqA.SquadId); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "UPDATE match_tournament_squad SET qualifier_points=?,qualifier_played=?,qualifier_wins=?,qualifier_losses=?,qualifier_draws=?,updated_ms=? WHERE squad_id=?",
				sqB.QualifierPoints, sqB.QualifierPlayed, sqB.QualifierWins, sqB.QualifierLosses, sqB.QualifierDraws, stamp, sqB.SquadId); err != nil {
				return err
			}
		} else {
			// 淘汰赛必须产出唯一胜者(平局按确定性四级决胜规则)。
			switch outcome {
			case battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN:
				winnerSquadID = sqA.SquadId
			case battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_B_WIN:
				winnerSquadID = sqB.SquadId
			default:
				tieA := extractKnockoutTieMetrics(event, 0, sqA.SquadId, sqA.SeedRank)
				tieB := extractKnockoutTieMetrics(event, 1, sqB.SquadId, sqB.SeedRank)
				if tournament.ResolveKnockoutDrawWinner(tieA, tieB) == 0 {
					winnerSquadID = sqA.SquadId
				} else {
					winnerSquadID = sqB.SquadId
				}
			}
			loser := sqB
			if winnerSquadID == sqB.SquadId {
				loser = sqA
			}
			if err = advanceKnockoutWinnerTx(ctx, tx, season, m, winnerSquadID, loser, stamp); err != nil {
				return err
			}
		}

		if _, err = tx.ExecContext(ctx, "UPDATE match_tournament_match SET state=?,winner_squad_id=?,outcome=?,finished_ms=?,updated_ms=? WHERE match_id=?",
			uint32(pb.TournamentMatchState_TOURNAMENT_MATCH_STATE_FINISHED), winnerSquadID, uint32(outcome), stamp, stamp, m.MatchId); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		applied = true
		return nil
	})
	return applied, err
}

func hasSideSurvivors(event *kafkapb.BattleResultEvent, teamIndex uint32) bool {
	for _, st := range event.GetSettlements() {
		if st.GetPlayerTeamIndex() != teamIndex {
			continue
		}
		if !st.GetIsDead() && !st.GetFled() {
			return true
		}
		for _, pet := range st.GetPets() {
			if !pet.GetIsDead() {
				return true
			}
		}
	}
	return false
}

func extractKnockoutTieMetrics(event *kafkapb.BattleResultEvent, teamIndex uint32, squadID uint64, seedRank uint32) tournament.KnockoutTieMetrics {
	m := tournament.KnockoutTieMetrics{SquadID: squadID, SeedRank: seedRank}
	for _, st := range event.GetSettlements() {
		if st.GetPlayerTeamIndex() != teamIndex {
			continue
		}
		if !st.GetIsDead() && !st.GetFled() {
			m.SurvivingUnits++
			m.RemainingHpBpsSum += st.GetHealth()
		}
		for _, pet := range st.GetPets() {
			if !pet.GetIsDead() {
				m.SurvivingUnits++
				m.RemainingHpBpsSum += pet.GetHealth()
			}
		}
	}
	return m
}

func advanceKnockoutWinnerTx(ctx context.Context, tx *sql.Tx, season *pb.MatchTournamentSeasonRecord, m *pb.MatchTournamentMatchRecord, winnerSquadID uint64, loser *pb.MatchTournamentSquadRecord, stamp uint64) error {
	totalRounds := uint32(tournament.TotalKnockoutRounds(int(season.KnockoutTeamCount)))
	elimRank := tournament.RankForEliminatedRound(int(season.KnockoutTeamCount), int(m.RoundIndex))
	if loser != nil && loser.SquadId != 0 {
		if _, err := tx.ExecContext(ctx, "UPDATE match_tournament_squad SET state=?,final_rank=?,updated_ms=? WHERE squad_id=?",
			uint32(pb.TournamentSquadState_TOURNAMENT_SQUAD_STATE_ELIMINATED), elimRank, stamp, loser.SquadId); err != nil {
			return err
		}
	}
	if m.RoundIndex >= totalRounds {
		// 总决赛结束:标记冠军战队并刷新赛季冠军摘要(结算发奖交由 AdvanceTournamentSeason 统一幂等落账)。
		var champName string
		_ = tx.QueryRowContext(ctx, "SELECT name FROM match_tournament_squad WHERE squad_id=?", winnerSquadID).Scan(&champName)
		members, _ := loadSquadMembersTx(ctx, tx, winnerSquadID)
		names := make([]string, 0, len(members))
		for _, mem := range members {
			if mem.PlayerName != "" {
				names = append(names, mem.PlayerName)
			} else {
				names = append(names, fmt.Sprintf("#%d", mem.PlayerId))
			}
		}
		summary := strings.Join(names, " / ")
		if _, err := tx.ExecContext(ctx, "UPDATE match_tournament_squad SET state=?,final_rank=1,updated_ms=? WHERE squad_id=?",
			uint32(pb.TournamentSquadState_TOURNAMENT_SQUAD_STATE_CHAMPION), stamp, winnerSquadID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "UPDATE match_tournament_season SET champion_squad_id=?,champion_squad_name=?,champion_member_summary=?,updated_ms=? WHERE season_id=?",
			winnerSquadID, champName, summary, stamp, season.SeasonId)
		return err
	}
	// 非决赛:将胜者填入下一轮 (round_index+1, slot_index/2) 的 A 或 B 槽位。
	nextRound := m.RoundIndex + 1
	nextSlot := m.SlotIndex / 2
	col := "squad_a_id"
	if m.SlotIndex%2 == 1 {
		col = "squad_b_id"
	}
	_, err := tx.ExecContext(ctx, "UPDATE match_tournament_match SET "+col+"=?,updated_ms=? WHERE season_id=? AND phase=? AND round_index=? AND slot_index=?",
		winnerSquadID, stamp, season.SeasonId, uint32(pb.TournamentMatchPhase_TOURNAMENT_MATCH_PHASE_KNOCKOUT), nextRound, nextSlot)
	return err
}

// AdvanceTournamentSeason 可恢复驱动锦标赛四阶段推进(REGISTRATION -> QUALIFIER -> KNOCKOUT -> SETTLED -> 下届)。
// idAlloc 须在事务外预分配所需永久 ID(严禁事务内调 RPC)。
func (s *Store) AdvanceTournamentSeason(ctx context.Context, nowOverrideMs uint64, idAlloc func(count int) ([]uint64, error)) error {
	if s == nil || s.DB == nil {
		return nil
	}
	stamp := nowOverrideMs
	if stamp == 0 {
		var err error
		if stamp, err = s.Now(ctx); err != nil {
			return err
		}
	}
	// 先只读探测当前赛季阶段与下一动作所需 ID 数量(在事务外预分配,严禁事务内 RPC)。
	var cur *pb.MatchTournamentSeasonRecord
	if err := s.Fenced(ctx, func() error {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		cur, err = s.ensureTournamentSeasonTx(ctx, tx, stamp)
		if err != nil {
			return err
		}
		return tx.Commit()
	}); err != nil {
		return err
	}

	switch pb.TournamentStage(cur.Stage) {
	case pb.TournamentStage_TOURNAMENT_STAGE_REGISTRATION:
		if stamp < cur.QualifierStartMs {
			return nil
		}
		return s.transitionToQualifier(ctx, cur.SeasonId, stamp)

	case pb.TournamentStage_TOURNAMENT_STAGE_QUALIFIER:
		if stamp < cur.KnockoutStartMs {
			return nil
		}
		// 预分配最多 15 场淘汰赛对局 ID(16 强总场次 8+4+2+1=15)。
		ids, err := idAlloc(15)
		if err != nil {
			return err
		}
		return s.transitionToKnockout(ctx, cur.SeasonId, stamp, ids)

	case pb.TournamentStage_TOURNAMENT_STAGE_KNOCKOUT:
		// 1. 先检查到期未开打的淘汰赛槽位做弃权裁决并推进轮空。
		if err := s.resolveOverdueKnockoutMatches(ctx, cur.SeasonId, stamp); err != nil {
			return err
		}
		// 2. 检查是否已决出总冠军或已到达强制结算时刻。
		readyToSettle, needRewardIDs, err := s.estimateTournamentSettlement(ctx, cur.SeasonId, stamp)
		if err != nil || !readyToSettle {
			return err
		}
		ids, err := idAlloc(needRewardIDs + 16)
		if err != nil {
			return err
		}
		return s.settleTournamentSeason(ctx, cur.SeasonId, stamp, ids)

	case pb.TournamentStage_TOURNAMENT_STAGE_SETTLED:
		if stamp < cur.EndMs {
			return nil
		}
		return s.openNextTournamentSeason(ctx, cur.SeasonId, stamp)
	}
	return nil
}

func (s *Store) transitionToQualifier(ctx context.Context, seasonID uint32, stamp uint64) error {
	return s.Fenced(ctx, func() error {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		cur, err := scanTournamentSeason(tx.QueryRowContext(ctx, "SELECT "+tournamentSeasonCols+" FROM match_tournament_season WHERE season_id=? FOR UPDATE", seasonID))
		if err != nil || cur.Stage != uint32(pb.TournamentStage_TOURNAMENT_STAGE_REGISTRATION) || stamp < cur.QualifierStartMs {
			return err
		}
		// 人数已达出战门槛的草稿队自动转为已报名锁定;不足人数草稿队标记作废。
		if _, err = tx.ExecContext(ctx, "UPDATE match_tournament_squad SET state=?,registered_ms=?,updated_ms=? WHERE season_id=? AND state=? AND member_count>=?",
			uint32(pb.TournamentSquadState_TOURNAMENT_SQUAD_STATE_REGISTERED), stamp, stamp, seasonID,
			uint32(pb.TournamentSquadState_TOURNAMENT_SQUAD_STATE_DRAFT), cur.BattleTeamSize); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE match_tournament_squad SET state=?,updated_ms=? WHERE season_id=? AND state=? AND member_count<?",
			uint32(pb.TournamentSquadState_TOURNAMENT_SQUAD_STATE_DISBANDED), stamp, seasonID,
			uint32(pb.TournamentSquadState_TOURNAMENT_SQUAD_STATE_DRAFT), cur.BattleTeamSize); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE match_tournament_invite SET state=4 WHERE season_id=? AND state=1", seasonID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE match_tournament_season SET stage=?,updated_ms=? WHERE season_id=?",
			uint32(pb.TournamentStage_TOURNAMENT_STAGE_QUALIFIER), stamp, seasonID); err != nil {
			return err
		}
		return tx.Commit()
	})
}

func (s *Store) transitionToKnockout(ctx context.Context, seasonID uint32, stamp uint64, preallocatedIDs []uint64) error {
	return s.Fenced(ctx, func() error {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		cur, err := scanTournamentSeason(tx.QueryRowContext(ctx, "SELECT "+tournamentSeasonCols+" FROM match_tournament_season WHERE season_id=? FOR UPDATE", seasonID))
		if err != nil || cur.Stage != uint32(pb.TournamentStage_TOURNAMENT_STAGE_QUALIFIER) || stamp < cur.KnockoutStartMs {
			return err
		}
		// 关闭海选期未成局的等待单。
		if _, err = tx.ExecContext(ctx, "UPDATE match_tournament_match SET state=?,updated_ms=? WHERE season_id=? AND phase=? AND state=1",
			uint32(pb.TournamentMatchState_TOURNAMENT_MATCH_STATE_FORFEIT), stamp, seasonID, uint32(pb.TournamentMatchPhase_TOURNAMENT_MATCH_PHASE_QUALIFIER)); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, "SELECT "+tournamentSquadCols+" FROM match_tournament_squad WHERE season_id=? AND state=? FOR UPDATE",
			seasonID, uint32(pb.TournamentSquadState_TOURNAMENT_SQUAD_STATE_REGISTERED))
		if err != nil {
			return err
		}
		var squads []*pb.MatchTournamentSquadRecord
		for rows.Next() {
			sq, e := scanTournamentSquad(rows)
			if e != nil {
				rows.Close()
				return e
			}
			squads = append(squads, sq)
		}
		rows.Close()

		entries := make([]tournament.StandingEntry, len(squads))
		byID := make(map[uint64]*pb.MatchTournamentSquadRecord, len(squads))
		for i, sq := range squads {
			entries[i] = tournament.StandingEntry{
				SquadID:         sq.SquadId,
				QualifierPoints: sq.QualifierPoints,
				QualifierWins:   sq.QualifierWins,
				QualifierDraws:  sq.QualifierDraws,
				RegisteredMs:    sq.RegisteredMs,
			}
			byID[sq.SquadId] = sq
		}
		tournament.SortStandings(entries)

		rule := s.TournamentCatalog().Rule()
		bracketSize := tournament.KnockoutBracketSize(len(entries), rule.GetKnockoutMaxTeams())
		// 写入海选种子排名;未入围淘汰赛者定档 final_rank 并标记 ELIMINATED。
		for idx, ent := range entries {
			seed := uint32(idx + 1)
			sq := byID[ent.SquadID]
			sq.SeedRank = seed
			if bracketSize == 0 || int(seed) > bracketSize {
				sq.State = uint32(pb.TournamentSquadState_TOURNAMENT_SQUAD_STATE_ELIMINATED)
				sq.FinalRank = seed
				if _, err = tx.ExecContext(ctx, "UPDATE match_tournament_squad SET seed_rank=?,final_rank=?,state=?,updated_ms=? WHERE squad_id=?",
					seed, seed, sq.State, stamp, sq.SquadId); err != nil {
					return err
				}
			} else {
				if _, err = tx.ExecContext(ctx, "UPDATE match_tournament_squad SET seed_rank=?,updated_ms=? WHERE squad_id=?",
					seed, stamp, sq.SquadId); err != nil {
					return err
				}
			}
		}

		if bracketSize == 0 {
			// 冷启动不足 2 支队:直接进 KNOCKOUT 且 knockout_team_count=0,随后自动转 SETTLED 发保底奖不流产。
			if _, err = tx.ExecContext(ctx, "UPDATE match_tournament_season SET stage=?,knockout_team_count=0,updated_ms=? WHERE season_id=?",
				uint32(pb.TournamentStage_TOURNAMENT_STAGE_KNOCKOUT), stamp, seasonID); err != nil {
				return err
			}
			return tx.Commit()
		}

		totalRounds := tournament.TotalKnockoutRounds(bracketSize)
		roundWindowMs := uint64(rule.GetKnockoutRoundTimeoutSeconds()) * 1000
		if roundWindowMs == 0 {
			roundWindowMs = 1800_000
		}
		pairs := tournament.SeedPairs(bracketSize)
		idCursor := 0
		nextID := func() (uint64, error) {
			if idCursor >= len(preallocatedIDs) || preallocatedIDs[idCursor] == 0 {
				return 0, errors.New("tournament: insufficient pre-allocated knockout match IDs")
			}
			id := preallocatedIDs[idCursor]
			idCursor++
			return id, nil
		}

		// 生成全部轮次对阵树行(第 1 轮填种子对位,后续轮次预建空槽)。
		roundMatches := make([][]*pb.MatchTournamentMatchRecord, totalRounds+1)
		for r := 1; r <= totalRounds; r++ {
			slots := bracketSize >> r
			rStart := stamp + uint64(r-1)*roundWindowMs
			rDeadline := rStart + roundWindowMs
			for slot := 0; slot < slots; slot++ {
				mid, err := nextID()
				if err != nil {
					return err
				}
				rec := &pb.MatchTournamentMatchRecord{
					MatchId:          mid,
					SeasonId:         seasonID,
					Phase:            uint32(pb.TournamentMatchPhase_TOURNAMENT_MATCH_PHASE_KNOCKOUT),
					RoundIndex:       uint32(r),
					SlotIndex:        uint32(slot),
					LineupA:          []byte{},
					LineupB:          []byte{},
					State:            uint32(pb.TournamentMatchState_TOURNAMENT_MATCH_STATE_WAITING_OPPONENT),
					FeaturedSpectate: true,
					ScheduledStartMs: rStart,
					DeadlineMs:       rDeadline,
					UpdatedMs:        stamp,
				}
				if r == 1 {
					seedA, seedB := pairs[slot][0], pairs[slot][1]
					if seedA <= len(entries) {
						rec.SquadAId = entries[seedA-1].SquadID
					}
					if seedB <= len(entries) {
						rec.SquadBId = entries[seedB-1].SquadID
					}
					if rec.SquadAId != 0 && rec.SquadBId == 0 {
						rec.State = uint32(pb.TournamentMatchState_TOURNAMENT_MATCH_STATE_BYE)
						rec.WinnerSquadId = rec.SquadAId
						rec.FinishedMs = stamp
					} else if rec.SquadBId != 0 && rec.SquadAId == 0 {
						rec.State = uint32(pb.TournamentMatchState_TOURNAMENT_MATCH_STATE_BYE)
						rec.WinnerSquadId = rec.SquadBId
						rec.FinishedMs = stamp
					}
				}
				roundMatches[r] = append(roundMatches[r], rec)
			}
		}
		// 把首轮 BYE 胜者预先填入第 2 轮槽位。
		if totalRounds >= 2 {
			for _, m := range roundMatches[1] {
				if m.State == uint32(pb.TournamentMatchState_TOURNAMENT_MATCH_STATE_BYE) && m.WinnerSquadId != 0 {
					target := roundMatches[2][m.SlotIndex/2]
					if m.SlotIndex%2 == 0 {
						target.SquadAId = m.WinnerSquadId
					} else {
						target.SquadBId = m.WinnerSquadId
					}
				}
			}
		}
		for r := 1; r <= totalRounds; r++ {
			for _, rec := range roundMatches[r] {
				if _, err = tx.ExecContext(ctx,
					"INSERT INTO match_tournament_match("+tournamentMatchCols+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
					rec.MatchId, rec.SeasonId, rec.Phase, rec.RoundIndex, rec.SlotIndex,
					rec.SquadAId, rec.SquadBId, nonNilBytes(rec.LineupA), nonNilBytes(rec.LineupB),
					rec.ReadyA, rec.ReadyB, rec.State, rec.BattleId, rec.WinnerSquadId, rec.Outcome,
					rec.FeaturedSpectate, rec.ScheduledStartMs, rec.DeadlineMs, rec.FinishedMs, stamp,
				); err != nil {
					return err
				}
			}
		}
		if _, err = tx.ExecContext(ctx, "UPDATE match_tournament_season SET stage=?,knockout_team_count=?,updated_ms=? WHERE season_id=?",
			uint32(pb.TournamentStage_TOURNAMENT_STAGE_KNOCKOUT), uint32(bracketSize), stamp, seasonID); err != nil {
			return err
		}
		return tx.Commit()
	})
}

func (s *Store) resolveOverdueKnockoutMatches(ctx context.Context, seasonID uint32, stamp uint64) error {
	return s.Fenced(ctx, func() error {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		season, err := scanTournamentSeason(tx.QueryRowContext(ctx, "SELECT "+tournamentSeasonCols+" FROM match_tournament_season WHERE season_id=? FOR UPDATE", seasonID))
		if err != nil || season.Stage != uint32(pb.TournamentStage_TOURNAMENT_STAGE_KNOCKOUT) || season.KnockoutTeamCount == 0 {
			return err
		}
		rows, err := tx.QueryContext(ctx,
			"SELECT "+tournamentMatchCols+" FROM match_tournament_match WHERE season_id=? AND phase=? AND state IN(1,2) AND deadline_ms>0 AND deadline_ms<=? ORDER BY round_index ASC, slot_index ASC FOR UPDATE",
			seasonID, uint32(pb.TournamentMatchPhase_TOURNAMENT_MATCH_PHASE_KNOCKOUT), stamp)
		if err != nil {
			return err
		}
		var overdue []*pb.MatchTournamentMatchRecord
		for rows.Next() {
			m, e := scanTournamentMatch(rows)
			if e != nil {
				rows.Close()
				return e
			}
			overdue = append(overdue, m)
		}
		rows.Close()
		for _, m := range overdue {
			if m.SquadAId == 0 && m.SquadBId == 0 {
				continue
			}
			var winnerID, loserID uint64
			if m.SquadAId != 0 && m.SquadBId == 0 {
				winnerID = m.SquadAId
			} else if m.SquadBId != 0 && m.SquadAId == 0 {
				winnerID = m.SquadBId
			} else if m.ReadyA && !m.ReadyB {
				winnerID, loserID = m.SquadAId, m.SquadBId
			} else if m.ReadyB && !m.ReadyA {
				winnerID, loserID = m.SquadBId, m.SquadAId
			} else {
				// 双方皆未就绪或同时过期:按海选种子高者(SeedRank 小)自动晋级,绝不卡死对阵树。
				var seedA, seedB uint32
				_ = tx.QueryRowContext(ctx, "SELECT seed_rank FROM match_tournament_squad WHERE squad_id=?", m.SquadAId).Scan(&seedA)
				_ = tx.QueryRowContext(ctx, "SELECT seed_rank FROM match_tournament_squad WHERE squad_id=?", m.SquadBId).Scan(&seedB)
				if seedB == 0 || (seedA > 0 && seedA <= seedB) {
					winnerID, loserID = m.SquadAId, m.SquadBId
				} else {
					winnerID, loserID = m.SquadBId, m.SquadAId
				}
			}
			var loser *pb.MatchTournamentSquadRecord
			if loserID != 0 {
				loser = &pb.MatchTournamentSquadRecord{SquadId: loserID}
			}
			if err = advanceKnockoutWinnerTx(ctx, tx, season, m, winnerID, loser, stamp); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "UPDATE match_tournament_match SET state=?,winner_squad_id=?,finished_ms=?,updated_ms=? WHERE match_id=?",
				uint32(pb.TournamentMatchState_TOURNAMENT_MATCH_STATE_FORFEIT), winnerID, stamp, stamp, m.MatchId); err != nil {
				return err
			}
		}
		return tx.Commit()
	})
}

func (s *Store) estimateTournamentSettlement(ctx context.Context, seasonID uint32, stamp uint64) (ready bool, neededIDs int, err error) {
	season, err := scanTournamentSeason(s.DB.QueryRowContext(ctx, "SELECT "+tournamentSeasonCols+" FROM match_tournament_season WHERE season_id=?", seasonID))
	if err != nil || season.Stage != uint32(pb.TournamentStage_TOURNAMENT_STAGE_KNOCKOUT) {
		return false, 0, err
	}
	if season.KnockoutTeamCount > 0 && season.ChampionSquadId == 0 && stamp < season.SettleStartMs {
		return false, 0, nil
	}
	var memberCount, cheerCount int
	_ = s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM match_tournament_member WHERE season_id=?", seasonID).Scan(&memberCount)
	_ = s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM match_tournament_cheer WHERE season_id=?", seasonID).Scan(&cheerCount)
	return true, memberCount + cheerCount, nil
}

func (s *Store) settleTournamentSeason(ctx context.Context, seasonID uint32, stamp uint64, preallocatedIDs []uint64) error {
	return s.Fenced(ctx, func() error {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		season, err := scanTournamentSeason(tx.QueryRowContext(ctx, "SELECT "+tournamentSeasonCols+" FROM match_tournament_season WHERE season_id=? FOR UPDATE", seasonID))
		if err != nil || season.Stage != uint32(pb.TournamentStage_TOURNAMENT_STAGE_KNOCKOUT) {
			return err
		}
		cat := s.TournamentCatalog()
		idIdx := 0
		nextID := func() (uint64, error) {
			if idIdx >= len(preallocatedIDs) || preallocatedIDs[idIdx] == 0 {
				return 0, errors.New("tournament: insufficient pre-allocated reward IDs")
			}
			id := preallocatedIDs[idIdx]
			idIdx++
			return id, nil
		}

		// 1. 读取本赛季所有有效参赛战队并为达标成员落账奖励行(主键去重)。
		sqRows, err := tx.QueryContext(ctx, "SELECT "+tournamentSquadCols+" FROM match_tournament_squad WHERE season_id=? AND state IN(2,3,4)", seasonID)
		if err != nil {
			return err
		}
		var squads []*pb.MatchTournamentSquadRecord
		top4Squads := map[uint64]bool{}
		for sqRows.Next() {
			sq, e := scanTournamentSquad(sqRows)
			if e != nil {
				sqRows.Close()
				return e
			}
			squads = append(squads, sq)
			if sq.FinalRank >= 1 && sq.FinalRank <= 4 {
				top4Squads[sq.SquadId] = true
			}
		}
		sqRows.Close()

		for _, sq := range squads {
			tier := cat.RewardForPlacement(sq.FinalRank, sq.QualifierPlayed)
			if tier == nil {
				continue
			}
			_, rawAtt, err := tournament.BuildMailAttachments(tier)
			if err != nil {
				return err
			}
			members, err := loadSquadMembersTx(ctx, tx, sq.SquadId)
			if err != nil {
				return err
			}
			for _, mem := range members {
				rid, err := nextID()
				if err != nil {
					return err
				}
				_, err = tx.ExecContext(ctx,
					"INSERT IGNORE INTO match_tournament_reward("+tournamentRewardCols+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,0,0,?)",
					seasonID, mem.PlayerId, tier.GetTierKind(), rid, sq.SquadId, sq.FinalRank,
					tier.GetId(), tier.GetGoldAmount(), nonNilBytes(rawAtt), tier.GetTitleCode(), tier.GetBadgeCode(),
					uint32(pb.TournamentRewardDeliveryState_TOURNAMENT_REWARD_DELIVERY_CLAIMABLE), stamp,
				)
				if err != nil {
					return err
				}
			}
		}

		// 2. 免费助威命中四强或总冠军者落账纪念小礼。
		if cheerTier := cat.CheerReward(); cheerTier != nil && len(top4Squads) > 0 {
			_, rawCheerAtt, err := tournament.BuildMailAttachments(cheerTier)
			if err != nil {
				return err
			}
			cRows, err := tx.QueryContext(ctx, "SELECT player_id,squad_id FROM match_tournament_cheer WHERE season_id=? AND rewarded=0", seasonID)
			if err != nil {
				return err
			}
			type cheerHit struct{ pid, sid uint64 }
			var hits []cheerHit
			for cRows.Next() {
				var pid, sid uint64
				if e := cRows.Scan(&pid, &sid); e != nil {
					cRows.Close()
					return e
				}
				if top4Squads[sid] {
					hits = append(hits, cheerHit{pid: pid, sid: sid})
				}
			}
			cRows.Close()
			for _, h := range hits {
				rid, err := nextID()
				if err != nil {
					return err
				}
				if _, err = tx.ExecContext(ctx,
					"INSERT IGNORE INTO match_tournament_reward("+tournamentRewardCols+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,0,0,?)",
					seasonID, h.pid, cheerTier.GetTierKind(), rid, h.sid, uint32(0),
					cheerTier.GetId(), cheerTier.GetGoldAmount(), nonNilBytes(rawCheerAtt), cheerTier.GetTitleCode(), cheerTier.GetBadgeCode(),
					uint32(pb.TournamentRewardDeliveryState_TOURNAMENT_REWARD_DELIVERY_CLAIMABLE), stamp,
				); err != nil {
					return err
				}
				if _, err = tx.ExecContext(ctx, "UPDATE match_tournament_cheer SET rewarded=1 WHERE season_id=? AND player_id=?", seasonID, h.pid); err != nil {
					return err
				}
			}
		}

		if _, err = tx.ExecContext(ctx, "UPDATE match_tournament_season SET stage=?,settled_ms=?,updated_ms=? WHERE season_id=?",
			uint32(pb.TournamentStage_TOURNAMENT_STAGE_SETTLED), stamp, stamp, seasonID); err != nil {
			return err
		}
		return tx.Commit()
	})
}

func (s *Store) openNextTournamentSeason(ctx context.Context, currentSeasonID uint32, stamp uint64) error {
	return s.Fenced(ctx, func() error {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		cur, err := scanTournamentSeason(tx.QueryRowContext(ctx, "SELECT "+tournamentSeasonCols+" FROM match_tournament_season WHERE season_id=? FOR UPDATE", currentSeasonID))
		if err != nil || cur.Stage != uint32(pb.TournamentStage_TOURNAMENT_STAGE_SETTLED) || stamp < cur.EndMs {
			return err
		}
		next := buildNewTournamentSeason(currentSeasonID+1, stamp, s.TournamentCatalog())
		_, err = tx.ExecContext(ctx,
			"INSERT IGNORE INTO match_tournament_season("+tournamentSeasonCols+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
			next.SeasonId, next.SeasonKey, next.SeasonName, next.RuleVersion, next.Stage,
			next.RegistrationStartMs, next.QualifierStartMs, next.KnockoutStartMs, next.SettleStartMs, next.EndMs,
			next.BattleTeamSize, next.RosterMaxSize, next.KnockoutTeamCount,
			0, "", "", 0, stamp,
		)
		if err != nil {
			return err
		}
		return tx.Commit()
	})
}

// ClaimableTournamentRewards 查询玩家待领取的赛季锦标赛奖励原件(用于主动领取或邮件发件箱投递)。
func (s *Store) ClaimableTournamentRewards(ctx context.Context, playerID uint64, seasonID uint32) ([]*pb.MatchTournamentRewardRecord, error) {
	query := "SELECT " + tournamentRewardCols + " FROM match_tournament_reward WHERE player_id=? AND delivery_state=?"
	args := []any{playerID, uint32(pb.TournamentRewardDeliveryState_TOURNAMENT_REWARD_DELIVERY_CLAIMABLE)}
	if seasonID != 0 {
		query += " AND season_id=?"
		args = append(args, seasonID)
	}
	query += " ORDER BY season_id DESC, tier_kind ASC"
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.MatchTournamentRewardRecord
	for rows.Next() {
		r, e := scanTournamentReward(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkTournamentRewardClaimed 幂等标记单条锦标赛奖励已投递/已领取(严禁重复发放)。
func (s *Store) MarkTournamentRewardClaimed(ctx context.Context, seasonID uint32, playerID uint64, tierKind uint32, mailID uint64) (*pb.TournamentPlayerRewardView, error) {
	var view *pb.TournamentPlayerRewardView
	err := s.Fenced(ctx, func() error {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		stamp, err := now(ctx, tx)
		if err != nil {
			return err
		}
		r, err := scanTournamentReward(tx.QueryRowContext(ctx, "SELECT "+tournamentRewardCols+" FROM match_tournament_reward WHERE season_id=? AND player_id=? AND tier_kind=? FOR UPDATE",
			seasonID, playerID, tierKind))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTournamentRewardUnavailable
		}
		if err != nil {
			return err
		}
		if r.DeliveryState == uint32(pb.TournamentRewardDeliveryState_TOURNAMENT_REWARD_DELIVERY_CLAIMABLE) {
			r.DeliveryState = uint32(pb.TournamentRewardDeliveryState_TOURNAMENT_REWARD_DELIVERY_CLAIMED)
			r.MailId = mailID
			r.ClaimedMs = stamp
			r.UpdatedMs = stamp
			if _, err = tx.ExecContext(ctx, "UPDATE match_tournament_reward SET delivery_state=?,mail_id=?,claimed_ms=?,updated_ms=? WHERE season_id=? AND player_id=? AND tier_kind=?",
				r.DeliveryState, mailID, stamp, stamp, seasonID, playerID, tierKind); err != nil {
				return err
			}
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		view = toTournamentRewardView(r)
		return nil
	})
	return view, err
}

func nonNilBytes(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}
