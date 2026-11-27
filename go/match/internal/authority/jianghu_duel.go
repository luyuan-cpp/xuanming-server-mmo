package authority

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	kafkapb "proto/contracts/kafka"
	pb "proto/match"
	teampb "proto/team"

	"google.golang.org/protobuf/proto"
)

var (
	ErrJianghuSameTargetCooldown      = errors.New("same target jianghu duel cooldown active")
	ErrJianghuDailyInviteLimitReached = errors.New("daily unmatched jianghu duel invite limit reached")
	ErrJianghuInvitePending           = errors.New("target already has a pending jianghu duel invite")
	ErrJianghuInviteNotFound          = errors.New("jianghu duel invite not found or state changed")
)

// JianghuBattleGrudgeSummary 是单局生死状结算写入个人恩怨录后的连胜/终结摘要，供上层决定高光播报。
type JianghuBattleGrudgeSummary struct {
	WinnerPlayerID     uint64
	LoserPlayerID      uint64
	WinnerNewWinStreak uint32
	LoserPrevWinStreak uint32
	IsDraw             bool
}

// JianghuStore 抽象江湖恩怨与天墉生死状的 SQL 权威读写，生产由 *Store 实现，单测可注入内存替身。
type JianghuStore interface {
	CreateJianghuDuelInvite(ctx context.Context, rec *pb.MatchJianghuDuelInvitationRecord, sameTargetCooldownMs uint64, dailyUnmatchedLimit uint32, nowMs uint64) (*pb.MatchJianghuDuelInvitationRecord, error)
	GetJianghuDuelInvite(ctx context.Context, duelInviteID uint64) (*pb.MatchJianghuDuelInvitationRecord, error)
	UpdateJianghuDuelInvite(ctx context.Context, rec *pb.MatchJianghuDuelInvitationRecord, expectedState uint32) (bool, error)
	// MutateJianghuDuelInvite 在单事务 SELECT ... FOR UPDATE 锁内原子读改写战书，杜绝多人并发确认丢更新与状态竞态。
	MutateJianghuDuelInvite(ctx context.Context, duelInviteID uint64, mutate func(rec *pb.MatchJianghuDuelInvitationRecord, checkTargetBusy func(targetCaptainID, nowMs uint64) (bool, error)) (commit bool, err error)) (*pb.MatchJianghuDuelInvitationRecord, error)
	SetJianghuDuelInviteBattleID(ctx context.Context, duelInviteID, battleID uint64) error
	AbortMatchedJianghuDuelInvite(ctx context.Context, duelInviteID uint64) error
	DecrementDailyUnmatchedOnMatch(ctx context.Context, challengerID uint64, nowMs uint64) error
	ListActiveJianghuDuelInvitesForPlayer(ctx context.Context, playerID uint64, nowMs uint64) ([]*pb.MatchJianghuDuelInvitationRecord, error)
	ApplyJianghuDuelBattleResult(ctx context.Context, event *kafkapb.BattleResultEvent, ownerRevs map[uint64]uint64) (JianghuBattleGrudgeSummary, bool, error)
	ListJianghuGrudges(ctx context.Context, playerID, ownerRevision uint64) ([]*pb.JianghuGrudgeEntry, error)
	DismissJianghuGrudge(ctx context.Context, playerID, ownerRevision, peerPlayerID uint64) (bool, error)
	LoadTeamMemberIDs(ctx context.Context, captainID uint64) (teamID uint64, leaderID uint64, members []uint64, err error)
}

var _ JianghuStore = (*Store)(nil)

// JianghuGameDayKey 计算 UTC+8 每日 05:00 游戏日键（YYYYMMDD）。
func JianghuGameDayKey(nowMs uint64) uint32 {
	const utcPlus8Sec = 8 * 3600
	const resetHourOffsetSec = 5 * 3600
	sec := int64(nowMs/1000) + utcPlus8Sec - resetHourOffsetSec
	t := time.Unix(sec, 0).UTC()
	return uint32(t.Year()*10000 + int(t.Month())*100 + t.Day())
}

// NormalizeJianghuTeamIDs 规范化一侧成员列表：保证队长位于首位、去重且成员数在 1..5。
func NormalizeJianghuTeamIDs(captainID uint64, provided []uint64) ([]uint64, bool) {
	if captainID == 0 {
		return nil, false
	}
	seen := map[uint64]struct{}{captainID: {}}
	out := []uint64{captainID}
	for _, id := range provided {
		if id == 0 {
			return nil, false
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	if len(out) == 0 || len(out) > 5 {
		return nil, false
	}
	return out, true
}

// EncodeMembersSnapshot 将双队成员与确认态快照确定性编码为 MatchJianghuDuelInvitationRecord.members_payload。
func EncodeMembersSnapshot(challenger, target []*pb.JianghuDuelMemberView) ([]byte, error) {
	carrier := &pb.JianghuDuelInviteView{
		ChallengerMembers: challenger,
		TargetMembers:     target,
	}
	return proto.MarshalOptions{Deterministic: true}.Marshal(carrier)
}

// DecodeMembersSnapshot 解码双队成员与确认态快照。
func DecodeMembersSnapshot(raw []byte) ([]*pb.JianghuDuelMemberView, []*pb.JianghuDuelMemberView, error) {
	if len(raw) == 0 {
		return nil, nil, nil
	}
	carrier := &pb.JianghuDuelInviteView{}
	if err := proto.Unmarshal(raw, carrier); err != nil {
		return nil, nil, err
	}
	return carrier.GetChallengerMembers(), carrier.GetTargetMembers(), nil
}

const jianghuInviteColumns = "duel_invite_id,writ_tier,team_size,state,challenger_captain_id,target_captain_id," +
	"challenger_account,challenger_revision,target_account,target_revision,zone_id," +
	"side_a_guild_id,side_a_guild_name,side_b_guild_id,side_b_guild_name,members_payload," +
	"created_ms,expires_ms,confirm_deadline_ms,battle_id"

func scanJianghuInvite(s interface{ Scan(...any) error }) (*pb.MatchJianghuDuelInvitationRecord, error) {
	rec := &pb.MatchJianghuDuelInvitationRecord{}
	err := s.Scan(
		&rec.DuelInviteId, &rec.WritTier, &rec.TeamSize, &rec.State,
		&rec.ChallengerCaptainId, &rec.TargetCaptainId,
		&rec.ChallengerAccount, &rec.ChallengerRevision,
		&rec.TargetAccount, &rec.TargetRevision, &rec.ZoneId,
		&rec.SideAGuildId, &rec.SideAGuildName, &rec.SideBGuildId, &rec.SideBGuildName,
		&rec.MembersPayload, &rec.CreatedMs, &rec.ExpiresMs, &rec.ConfirmDeadlineMs, &rec.BattleId,
	)
	if err != nil {
		return nil, err
	}
	return rec, nil
}

func (s *Store) CreateJianghuDuelInvite(
	ctx context.Context,
	rec *pb.MatchJianghuDuelInvitationRecord,
	sameTargetCooldownMs uint64,
	dailyUnmatchedLimit uint32,
	nowMs uint64,
) (*pb.MatchJianghuDuelInvitationRecord, error) {
	if s == nil || s.DB == nil || rec == nil || rec.DuelInviteId == 0 {
		return nil, errors.New("invalid jianghu duel invite store state")
	}
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// 1. 核验同一目标 5 分钟冷却行。
	var cdUntil uint64
	err = tx.QueryRowContext(ctx,
		"SELECT cooldown_until_ms FROM match_jianghu_duel_cooldown WHERE challenger_player_id=? AND target_player_id=? FOR UPDATE",
		rec.ChallengerCaptainId, rec.TargetCaptainId).Scan(&cdUntil)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil && cdUntil > nowMs {
		return nil, ErrJianghuSameTargetCooldown
	}

	// 2. 核验每日未成局总次数上限（target_player_id = 0 行）。
	dayKey := JianghuGameDayKey(nowMs)
	var storedDay, dailyCount uint32
	err = tx.QueryRowContext(ctx,
		"SELECT day_key, daily_unmatched_count FROM match_jianghu_duel_cooldown WHERE challenger_player_id=? AND target_player_id=0 FOR UPDATE",
		rec.ChallengerCaptainId).Scan(&storedDay, &dailyCount)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil && storedDay == dayKey && dailyUnmatchedLimit > 0 && dailyCount >= dailyUnmatchedLimit {
		return nil, ErrJianghuDailyInviteLimitReached
	}
	nextDailyCount := uint32(1)
	if err == nil && storedDay == dayKey {
		nextDailyCount = dailyCount + 1
	}

	// 3. 同一目标队长同时仅挂一张待应答/待确认的生死状。
	var existingID uint64
	err = tx.QueryRowContext(ctx,
		"SELECT duel_invite_id FROM match_jianghu_duel_invitation WHERE target_captain_id=? AND state IN (2, 3) AND expires_ms>? LIMIT 1 FOR UPDATE",
		rec.TargetCaptainId, nowMs).Scan(&existingID)
	if err == nil && existingID != 0 {
		return nil, ErrJianghuInvitePending
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	membersPayload := rec.MembersPayload
	if membersPayload == nil {
		membersPayload = []byte{}
	}
	if _, err = tx.ExecContext(ctx,
		"INSERT INTO match_jianghu_duel_invitation("+jianghuInviteColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		rec.DuelInviteId, rec.WritTier, rec.TeamSize, rec.State,
		rec.ChallengerCaptainId, rec.TargetCaptainId,
		rec.ChallengerAccount, rec.ChallengerRevision,
		rec.TargetAccount, rec.TargetRevision, rec.ZoneId,
		rec.SideAGuildId, rec.SideAGuildName, rec.SideBGuildId, rec.SideBGuildName,
		membersPayload, rec.CreatedMs, rec.ExpiresMs, rec.ConfirmDeadlineMs, rec.BattleId,
	); err != nil {
		return nil, err
	}

	// 写入同目标冷却行 + 当日未成局计数行。
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO match_jianghu_duel_cooldown(challenger_player_id,target_player_id,cooldown_until_ms,day_key,daily_unmatched_count,updated_ms)
		 VALUES(?,?,?,?,?,?)
		 ON DUPLICATE KEY UPDATE cooldown_until_ms=VALUES(cooldown_until_ms), day_key=VALUES(day_key), updated_ms=VALUES(updated_ms)`,
		rec.ChallengerCaptainId, rec.TargetCaptainId, nowMs+sameTargetCooldownMs, dayKey, uint32(0), nowMs); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO match_jianghu_duel_cooldown(challenger_player_id,target_player_id,cooldown_until_ms,day_key,daily_unmatched_count,updated_ms)
		 VALUES(?,0,0,?,?,?)
		 ON DUPLICATE KEY UPDATE day_key=VALUES(day_key), daily_unmatched_count=VALUES(daily_unmatched_count), updated_ms=VALUES(updated_ms)`,
		rec.ChallengerCaptainId, dayKey, nextDailyCount, nowMs); err != nil {
		return nil, err
	}

	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return proto.Clone(rec).(*pb.MatchJianghuDuelInvitationRecord), nil
}

func (s *Store) GetJianghuDuelInvite(ctx context.Context, duelInviteID uint64) (*pb.MatchJianghuDuelInvitationRecord, error) {
	if s == nil || s.DB == nil || duelInviteID == 0 {
		return nil, ErrJianghuInviteNotFound
	}
	rec, err := scanJianghuInvite(s.DB.QueryRowContext(ctx,
		"SELECT "+jianghuInviteColumns+" FROM match_jianghu_duel_invitation WHERE duel_invite_id=?", duelInviteID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrJianghuInviteNotFound
	}
	return rec, err
}

func (s *Store) UpdateJianghuDuelInvite(ctx context.Context, rec *pb.MatchJianghuDuelInvitationRecord, expectedState uint32) (bool, error) {
	if s == nil || s.DB == nil || rec == nil {
		return false, ErrJianghuInviteNotFound
	}
	membersPayload := rec.MembersPayload
	if membersPayload == nil {
		membersPayload = []byte{}
	}
	res, err := s.DB.ExecContext(ctx,
		`UPDATE match_jianghu_duel_invitation
		 SET state=?, members_payload=?, expires_ms=?, confirm_deadline_ms=?, battle_id=?
		 WHERE duel_invite_id=? AND state=?`,
		rec.State, membersPayload, rec.ExpiresMs, rec.ConfirmDeadlineMs, rec.BattleId,
		rec.DuelInviteId, expectedState)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) MutateJianghuDuelInvite(
	ctx context.Context,
	duelInviteID uint64,
	mutate func(rec *pb.MatchJianghuDuelInvitationRecord, checkTargetBusy func(targetCaptainID, nowMs uint64) (bool, error)) (commit bool, err error),
) (*pb.MatchJianghuDuelInvitationRecord, error) {
	if s == nil || s.DB == nil || duelInviteID == 0 || mutate == nil {
		return nil, ErrJianghuInviteNotFound
	}
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	rec, err := scanJianghuInvite(tx.QueryRowContext(ctx,
		"SELECT "+jianghuInviteColumns+" FROM match_jianghu_duel_invitation WHERE duel_invite_id=? FOR UPDATE",
		duelInviteID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrJianghuInviteNotFound
	}
	if err != nil {
		return nil, err
	}

	checkTargetBusy := func(targetCaptainID, nowMs uint64) (bool, error) {
		var existingID uint64
		qErr := tx.QueryRowContext(ctx,
			"SELECT duel_invite_id FROM match_jianghu_duel_invitation WHERE target_captain_id=? AND duel_invite_id<>? AND state IN (2, 3) AND expires_ms>? LIMIT 1 FOR UPDATE",
			targetCaptainID, duelInviteID, nowMs).Scan(&existingID)
		if errors.Is(qErr, sql.ErrNoRows) {
			return false, nil
		}
		if qErr != nil {
			return false, qErr
		}
		return existingID != 0, nil
	}

	commit, mErr := mutate(rec, checkTargetBusy)
	if mErr != nil {
		return proto.Clone(rec).(*pb.MatchJianghuDuelInvitationRecord), mErr
	}
	if !commit {
		return proto.Clone(rec).(*pb.MatchJianghuDuelInvitationRecord), nil
	}

	membersPayload := rec.MembersPayload
	if membersPayload == nil {
		membersPayload = []byte{}
	}
	if _, err = tx.ExecContext(ctx,
		`UPDATE match_jianghu_duel_invitation
		 SET state=?, members_payload=?, expires_ms=?, confirm_deadline_ms=?, battle_id=?
		 WHERE duel_invite_id=?`,
		rec.State, membersPayload, rec.ExpiresMs, rec.ConfirmDeadlineMs, rec.BattleId, rec.DuelInviteId); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return proto.Clone(rec).(*pb.MatchJianghuDuelInvitationRecord), nil
}

func (s *Store) SetJianghuDuelInviteBattleID(ctx context.Context, duelInviteID, battleID uint64) error {
	if s == nil || s.DB == nil || duelInviteID == 0 || battleID == 0 {
		return nil
	}
	_, err := s.DB.ExecContext(ctx,
		"UPDATE match_jianghu_duel_invitation SET battle_id=? WHERE duel_invite_id=? AND state=?",
		battleID, duelInviteID, uint32(pb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_MATCHED))
	return err
}

func (s *Store) AbortMatchedJianghuDuelInvite(ctx context.Context, duelInviteID uint64) error {
	if s == nil || s.DB == nil || duelInviteID == 0 {
		return nil
	}
	_, err := s.DB.ExecContext(ctx,
		"UPDATE match_jianghu_duel_invitation SET state=? WHERE duel_invite_id=? AND state=? AND battle_id=0",
		uint32(pb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_ABORTED),
		duelInviteID, uint32(pb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_MATCHED))
	return err
}

func (s *Store) DecrementDailyUnmatchedOnMatch(ctx context.Context, challengerID uint64, nowMs uint64) error {
	if s == nil || s.DB == nil || challengerID == 0 {
		return nil
	}
	dayKey := JianghuGameDayKey(nowMs)
	_, err := s.DB.ExecContext(ctx,
		`UPDATE match_jianghu_duel_cooldown
		 SET daily_unmatched_count = CASE WHEN daily_unmatched_count > 0 THEN daily_unmatched_count - 1 ELSE 0 END,
		     updated_ms = ?
		 WHERE challenger_player_id=? AND target_player_id=0 AND day_key=?`,
		nowMs, challengerID, dayKey)
	return err
}

func (s *Store) ListActiveJianghuDuelInvitesForPlayer(ctx context.Context, playerID uint64, nowMs uint64) ([]*pb.MatchJianghuDuelInvitationRecord, error) {
	if s == nil || s.DB == nil || playerID == 0 {
		return nil, nil
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT `+jianghuInviteColumns+` FROM match_jianghu_duel_invitation
		 WHERE (challenger_captain_id=? OR target_captain_id=?) AND state IN (1, 2, 3) AND expires_ms>?
		 ORDER BY created_ms DESC LIMIT 20`,
		playerID, playerID, nowMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.MatchJianghuDuelInvitationRecord
	for rows.Next() {
		rec, scanErr := scanJianghuInvite(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) LoadTeamMemberIDs(ctx context.Context, captainID uint64) (uint64, uint64, []uint64, error) {
	if s == nil || s.DB == nil || captainID == 0 {
		return 0, 0, nil, nil
	}
	var teamID uint64
	err := s.DB.QueryRowContext(ctx, "SELECT team_id FROM team_membership WHERE player_id=?", captainID).Scan(&teamID)
	if errors.Is(err, sql.ErrNoRows) || teamID == 0 {
		return 0, captainID, []uint64{captainID}, nil
	}
	if err != nil {
		return 0, 0, nil, fmt.Errorf("load team membership for player %d: %w", captainID, err)
	}
	var leaderID uint64
	var teamPayload []byte
	var teamState uint32
	if tErr := s.DB.QueryRowContext(ctx, "SELECT payload, state FROM team_authority WHERE team_id=?", teamID).Scan(&teamPayload, &teamState); tErr == nil && teamState == 1 && len(teamPayload) > 0 {
		data := &teampb.TeamAuthorityData{}
		if uErr := proto.Unmarshal(teamPayload, data); uErr == nil && data.GetRecord() != nil {
			leaderID = data.GetRecord().GetLeaderId()
		}
	}
	rows, err := s.DB.QueryContext(ctx, "SELECT player_id FROM team_membership WHERE team_id=? ORDER BY player_id ASC", teamID)
	if err != nil {
		return 0, 0, nil, err
	}
	defer rows.Close()
	members := make([]uint64, 0, 5)
	for rows.Next() {
		var pid uint64
		if scanErr := rows.Scan(&pid); scanErr != nil {
			return 0, 0, nil, scanErr
		}
		if pid != 0 {
			members = append(members, pid)
		}
	}
	if err = rows.Err(); err != nil {
		return 0, 0, nil, err
	}
	return teamID, leaderID, members, nil
}
