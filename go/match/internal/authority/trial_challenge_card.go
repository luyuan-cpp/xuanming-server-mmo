package authority

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	battlepb "proto/battle"
	kafkapb "proto/contracts/kafka"
	pb "proto/match"

	"google.golang.org/protobuf/proto"
)

const (
	// MaxTrialChallengeCardsPerPlayer 每名玩家最多同时保有的试炼约战帖数；超出按最旧 FIFO 淘汰并释放录像引用。
	MaxTrialChallengeCardsPerPlayer = 10
	// TrialChallengeCardTTLMs 试炼约战帖有效期（14 天，毫秒）。
	TrialChallengeCardTTLMs uint64 = 14 * 24 * 60 * 60 * 1000
)

var (
	// ErrTrialChallengeCardNotFound 约战帖不存在或已过期。
	ErrTrialChallengeCardNotFound = errors.New("trial challenge card not found or expired")
	// ErrTrialChallengeBattleNotEligible 目标对局不属于本人有效通关的试炼志 PVE 战报。
	ErrTrialChallengeBattleNotEligible = errors.New("battle is not eligible for trial challenge card")
	// ErrTrialChallengeReplayNotFound 约战帖无效/已过期，或对应录像正文已不存在。
	ErrTrialChallengeReplayNotFound = errors.New("shared trial replay not found")
)

func isTrialVictoryForPlayer(event *kafkapb.BattleResultEvent, settlement *battlepb.BattleSettlementData) bool {
	if event == nil || settlement == nil || settlement.GetFled() || event.GetTotalRounds() == 0 {
		return false
	}
	outcome := event.GetOutcome()
	team := settlement.GetPlayerTeamIndex()
	return (outcome == battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN && team == 0) ||
		(outcome == battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_B_WIN && team == 1)
}

// CreateTrialChallengeCard 将本人一场有效通关的试炼志 PVE 战报签发为可分享的权威约战帖。
// 同一 (owner_player_id, battle_id) 若已有未过期约战帖则幂等复用，不重复增加录像引用计数；
// 新签发时在同事务内给 match_battle_report.ref_count +1，并淘汰过期或超出 10 张的最旧约战帖。
func (s *Store) CreateTrialChallengeCard(
	ctx context.Context,
	ownerPlayerID, battleID uint64,
	nextCardID func() (uint64, error),
) (*pb.TrialChallengeCardView, error) {
	if s == nil || s.DB == nil {
		return nil, errors.New("trial challenge card store is not configured")
	}
	if ownerPlayerID == 0 || battleID == 0 || nextCardID == nil {
		return nil, ErrTrialChallengeBattleNotEligible
	}
	var out *pb.TrialChallengeCardView
	err := s.Fenced(ctx, func() error {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		view, err := s.createTrialChallengeCardTx(ctx, tx, ownerPlayerID, battleID, nextCardID)
		if err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		out = view
		return nil
	})
	return out, err
}

func (s *Store) createTrialChallengeCardTx(
	ctx context.Context,
	tx *sql.Tx,
	ownerPlayerID, battleID uint64,
	nextCardID func() (uint64, error),
) (*pb.TrialChallengeCardView, error) {
	if err := s.lockActor(ctx, tx, ownerPlayerID); err != nil {
		return nil, err
	}
	var nowMs uint64
	if err := tx.QueryRowContext(ctx, "SELECT "+fenceNowMsSQL).Scan(&nowMs); err != nil {
		return nil, err
	}

	// 1. 核验本人参战列表包含该局，防越权把他人对局签成约战帖。
	var listed int
	if err := tx.QueryRowContext(ctx,
		"SELECT 1 FROM match_player_battle_report WHERE player_id=? AND battle_id=? LIMIT 1",
		ownerPlayerID, battleID,
	).Scan(&listed); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTrialChallengeBattleNotEligible
	} else if err != nil {
		return nil, err
	}

	// 2. 锁定本人当前全部约战帖（最多 10 张），先做幂等复用，再收集待淘汰项。
	rows, err := tx.QueryContext(ctx,
		"SELECT card_id,battle_id,expires_ms,card_view FROM match_trial_challenge_card WHERE owner_player_id=? ORDER BY created_ms ASC,card_id ASC FOR UPDATE",
		ownerPlayerID,
	)
	if err != nil {
		return nil, err
	}
	type existingCard struct {
		cardID    uint64
		battleID  uint64
		expiresMs uint64
		viewBytes []byte
	}
	var allCards []existingCard
	for rows.Next() {
		var c existingCard
		if err = rows.Scan(&c.cardID, &c.battleID, &c.expiresMs, &c.viewBytes); err != nil {
			rows.Close()
			return nil, err
		}
		allCards = append(allCards, c)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}

	var liveCards []existingCard
	var evictCards []existingCard
	for _, c := range allCards {
		if c.expiresMs <= nowMs {
			evictCards = append(evictCards, c)
			continue
		}
		if c.battleID == battleID {
			reused := &pb.TrialChallengeCardView{}
			if err = proto.Unmarshal(c.viewBytes, reused); err != nil {
				return nil, fmt.Errorf("corrupt trial challenge card %d: %w", c.cardID, err)
			}
			return reused, nil
		}
		liveCards = append(liveCards, c)
	}

	// 3. 读取终局原件与建房原件，严格核验为试炼志 PVE 胜局。
	var rawCompletion []byte
	var decision uint32
	if err = tx.QueryRowContext(ctx,
		"SELECT raw_payload,decision FROM match_battle_completion WHERE battle_id=?",
		battleID,
	).Scan(&rawCompletion, &decision); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTrialChallengeBattleNotEligible
	} else if err != nil {
		return nil, err
	}
	if decision != uint32(pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE) || len(rawCompletion) == 0 {
		return nil, ErrTrialChallengeBattleNotEligible
	}
	event := &kafkapb.BattleResultEvent{}
	if err = proto.Unmarshal(rawCompletion, event); err != nil {
		return nil, fmt.Errorf("corrupt completion payload for battle %d: %w", battleID, err)
	}
	mode := pb.MatchMode(event.GetMatchMode())
	if mode != pb.MatchMode_MATCH_MODE_PVE_SOLO && mode != pb.MatchMode_MATCH_MODE_PVE_TEAM {
		return nil, ErrTrialChallengeBattleNotEligible
	}
	var mySettlement *battlepb.BattleSettlementData
	for _, item := range event.GetSettlements() {
		if item.GetPlayerId() == ownerPlayerID {
			mySettlement = item
			break
		}
	}
	if !isTrialVictoryForPlayer(event, mySettlement) || mySettlement.GetTrialFacts() == nil || mySettlement.GetTrialFacts().GetJournalEntryId() == 0 {
		return nil, ErrTrialChallengeBattleNotEligible
	}

	var rawCreate []byte
	if err = tx.QueryRowContext(ctx, "SELECT create_request FROM match_formation WHERE battle_id=?", battleID).Scan(&rawCreate); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTrialChallengeBattleNotEligible
	} else if err != nil {
		return nil, err
	}
	original := &battlepb.CreateBattleRequest{}
	if err = proto.Unmarshal(rawCreate, original); err != nil {
		return nil, fmt.Errorf("corrupt create_request for battle %d: %w", battleID, err)
	}
	ownerName := ""
	for _, p := range original.GetPlayers() {
		if p.GetPlayerId() == ownerPlayerID {
			ownerName = p.GetPlayerName()
			break
		}
	}

	// 检查录像正文是否存在（锁定正文行前确认存在，确保 ref_count 与卡片绑定一致）。
	var reportExists int
	hasReplay := false
	err = tx.QueryRowContext(ctx, "SELECT 1 FROM match_battle_report WHERE battle_id=? FOR UPDATE", battleID).Scan(&reportExists)
	if err == nil {
		hasReplay = true
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	// 超出上限按最旧 FIFO 淘汰。
	for len(liveCards) >= MaxTrialChallengeCardsPerPlayer {
		evictCards = append(evictCards, liveCards[0])
		liveCards = liveCards[1:]
	}
	released := map[uint64]uint32{}
	for _, victim := range evictCards {
		res, delErr := tx.ExecContext(ctx, "DELETE FROM match_trial_challenge_card WHERE card_id=? AND owner_player_id=?", victim.cardID, ownerPlayerID)
		if delErr != nil {
			return nil, delErr
		}
		if n, _ := res.RowsAffected(); n == 1 {
			v := &pb.TrialChallengeCardView{}
			if proto.Unmarshal(victim.viewBytes, v) == nil && v.GetHasReplay() {
				released[victim.battleID]++
			}
		}
	}

	cardID, err := nextCardID()
	if err != nil {
		return nil, err
	}
	facts := mySettlement.GetTrialFacts()
	artifacts := append([]uint32(nil), facts.GetUsedArtifactEffectIds()...)
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i] < artifacts[j] })
	view := &pb.TrialChallengeCardView{
		CardId:            cardID,
		OwnerPlayerId:     ownerPlayerID,
		OwnerName:         ownerName,
		BattleId:          battleID,
		JournalEntryId:    facts.GetJournalEntryId(),
		TrialRuleVersion:  facts.GetTrialRuleVersion(),
		VowMask:           facts.GetChallengeVowMask(),
		TotalRounds:       event.GetTotalRounds(),
		PartySize:         facts.GetInitialHumanCount(),
		PetTableId:        facts.GetUsedPetTableId(),
		ArtifactEffectIds: artifacts,
		SeasonSealId:      facts.GetUsedSeasonSealId(),
		HasReplay:         hasReplay,
		CreatedMs:         nowMs,
		ExpiresMs:         nowMs + TrialChallengeCardTTLMs,
	}
	viewBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(view)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx,
		"INSERT INTO match_trial_challenge_card(card_id,owner_player_id,battle_id,journal_entry_id,vow_mask,total_rounds,created_ms,expires_ms,card_view) VALUES(?,?,?,?,?,?,?,?,?)",
		cardID, ownerPlayerID, battleID, view.JournalEntryId, view.VowMask, view.TotalRounds, view.CreatedMs, view.ExpiresMs, viewBytes,
	); err != nil {
		return nil, err
	}
	if hasReplay {
		if _, err = tx.ExecContext(ctx, "UPDATE match_battle_report SET ref_count=ref_count+1 WHERE battle_id=?", battleID); err != nil {
			return nil, err
		}
	}
	if len(released) > 0 {
		if err = releaseBattleReportsTx(ctx, tx, released); err != nil {
			return nil, err
		}
	}
	return view, nil
}

// GetTrialChallengeCard 按约战帖 ID 只读查询未过期约战帖详情。
func (s *Store) GetTrialChallengeCard(ctx context.Context, cardID uint64) (*pb.TrialChallengeCardView, error) {
	if s == nil || s.DB == nil {
		return nil, errors.New("trial challenge card store is not configured")
	}
	if cardID == 0 {
		return nil, ErrTrialChallengeCardNotFound
	}
	var viewBytes []byte
	err := s.DB.QueryRowContext(ctx,
		"SELECT card_view FROM match_trial_challenge_card WHERE card_id=? AND expires_ms>"+fenceNowMsSQL,
		cardID,
	).Scan(&viewBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTrialChallengeCardNotFound
	}
	if err != nil {
		return nil, err
	}
	view := &pb.TrialChallengeCardView{}
	if err = proto.Unmarshal(viewBytes, view); err != nil {
		return nil, fmt.Errorf("corrupt trial challenge card %d: %w", cardID, err)
	}
	return view, nil
}

// GetSharedTrialReplay 仅凭有效未过期约战帖 card_id 读取对应试炼回放正文；
// 不放松既有 GetBattleReport(battle_id) 的 BR-9 隐私边界。
func (s *Store) GetSharedTrialReplay(ctx context.Context, cardID uint64) (*battlepb.BattleReplayEnvelope, error) {
	if s == nil || s.DB == nil {
		return nil, errors.New("trial challenge card store is not configured")
	}
	if cardID == 0 {
		return nil, ErrTrialChallengeReplayNotFound
	}
	var battleID uint64
	err := s.DB.QueryRowContext(ctx,
		"SELECT battle_id FROM match_trial_challenge_card WHERE card_id=? AND expires_ms>"+fenceNowMsSQL,
		cardID,
	).Scan(&battleID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTrialChallengeReplayNotFound
	}
	if err != nil {
		return nil, err
	}
	var body []byte
	err = s.DB.QueryRowContext(ctx, "SELECT envelope FROM match_battle_report WHERE battle_id=?", battleID).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTrialChallengeReplayNotFound
	}
	if err != nil {
		return nil, err
	}
	env := &battlepb.BattleReplayEnvelope{}
	if err = proto.Unmarshal(body, env); err != nil {
		return nil, fmt.Errorf("corrupt shared trial replay body for card %d: %w", cardID, err)
	}
	return env, nil
}

// purgeTrialChallengeCardsTx 删角收尾时清掉该玩家名下全部约战帖，并把已钉住的录像引用并入 released 统一释放。
func purgeTrialChallengeCardsTx(ctx context.Context, tx *sql.Tx, player uint64, released map[uint64]uint32) error {
	rows, err := tx.QueryContext(ctx,
		"SELECT card_id,battle_id,card_view FROM match_trial_challenge_card WHERE owner_player_id=? ORDER BY card_id",
		player,
	)
	if err != nil {
		return err
	}
	type victim struct {
		cardID    uint64
		battleID  uint64
		viewBytes []byte
	}
	var victims []victim
	for rows.Next() {
		var v victim
		if err = rows.Scan(&v.cardID, &v.battleID, &v.viewBytes); err != nil {
			rows.Close()
			return err
		}
		victims = append(victims, v)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, v := range victims {
		res, err := tx.ExecContext(ctx, "DELETE FROM match_trial_challenge_card WHERE card_id=? AND owner_player_id=?", v.cardID, player)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			view := &pb.TrialChallengeCardView{}
			if proto.Unmarshal(v.viewBytes, view) == nil && view.GetHasReplay() {
				released[v.battleID]++
			}
		}
	}
	return nil
}
