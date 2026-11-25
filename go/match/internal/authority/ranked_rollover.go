package authority

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	pb "proto/match"
	rankpb "proto/rank"
	"shared/generated/pb/table"
	"shared/serviceauth"
)

// RankedNameLookup 解析榜首玩家展示昵称;查不到返回空串。
type RankedNameLookup func(ctx context.Context, playerID uint64) string

// RankedBoardReader 只读查询已封榜的排行榜第一页(用于校验封榜第 1 名与名次快照)。
type RankedBoardReader interface {
	GetRankPage(ctx context.Context, in *rankpb.GetRankPageRequest) (*rankpb.GetRankPageResponse, error)
}

// AdvanceRankedSeasonRollover 推进可恢复的论剑赛季换季状态机(Stage 0..8):
//
//	0->1 ADMISSION_STOPPED: 到期转 SETTLING 并关闭旧季尚未成局的 PVP_TEAM 排队票与整队队列;
//	1->2 FORMATIONS_DRAINED: 等待旧季已接纳成局全部落定终局(state 5 或 state 4 且 retry_at_ms=NoRetry);
//	2->3 RATINGS_DRAINED: 等待旧季所有 COMPLETE 终局全部写入 match_rating_applied;
//	3->4 RANK_STOPPED: StopRankSource(epoch=season.SourceEpoch, PVP_TEAM) 锁定最终游标和;
//	4->5 RANK_DRAINED: 确保旧季榜已打开且 64 片 committed_cursor == produced_cursor;
//	5->6 BOARD_SEALED: 调用 RankAdmin.FreezeRankGeneration 封榜(含 0 人空榜);
//	6->7 HONORS_SNAPSHOT: 写入全员 final_rank、天枢/无双徽章与称号,仅第 1 且终分>=无双授予榜首题名(否则空缺);
//	7->8 DONE: 旧季置 CLOSED 并同事务开启下一赛季(ACTIVE,新 generation/source_epoch,Elo 仍在 match_rating 继承)。
//
// 每一步均幂等可恢复,任意阶段进程重启后可无损续跑。
func (s *Store) AdvanceRankedSeasonRollover(
	ctx context.Context,
	rankAdmin rankpb.RankAdminClient,
	rankReader RankedBoardReader,
	rankSecret string,
	lookupName RankedNameLookup,
) error {
	if s == nil || s.DB == nil {
		return nil
	}
	cur, err := s.EnsureActiveSeason(ctx)
	if err != nil {
		return err
	}
	stamp, err := s.Now(ctx)
	if err != nil {
		return err
	}
	if cur.State == uint32(pb.RankedSeasonState_RANKED_SEASON_STATE_ACTIVE) && (cur.EndMs == 0 || stamp < cur.EndMs) {
		return nil
	}
	if cur.State == uint32(pb.RankedSeasonState_RANKED_SEASON_STATE_CLOSED) {
		return nil
	}
	switch cur.RolloverStage {
	case RolloverStageNone:
		return s.rolloverStopAdmission(ctx, cur.SeasonId)
	case RolloverStageAdmissionStopped:
		return s.rolloverCheckFormationsDrained(ctx, cur.SeasonId)
	case RolloverStageFormationsDrained:
		return s.rolloverCheckRatingsDrained(ctx, cur.SeasonId)
	case RolloverStageRatingsDrained:
		return s.rolloverStopRankSource(ctx, cur)
	case RolloverStageRankStopped:
		return s.rolloverCheckRankDrained(ctx, cur, rankAdmin, rankSecret)
	case RolloverStageRankDrained:
		return s.rolloverSealBoard(ctx, cur, rankAdmin, rankSecret)
	case RolloverStageBoardSealed:
		return s.rolloverSnapshotHonors(ctx, cur, rankReader, lookupName)
	case RolloverStageHonorsSnapshot:
		return s.rolloverOpenNextSeason(ctx, cur)
	default:
		return nil
	}
}

func (s *Store) rolloverStopAdmission(ctx context.Context, seasonID uint32) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cur, err := scanSeason(tx.QueryRowContext(ctx, "SELECT "+seasonColumns+" FROM match_ranked_season WHERE season_id=? FOR UPDATE", seasonID))
	if err != nil {
		return err
	}
	stamp, err := now(ctx, tx)
	if err != nil {
		return err
	}
	if cur.RolloverStage != RolloverStageNone || (cur.State == uint32(pb.RankedSeasonState_RANKED_SEASON_STATE_ACTIVE) && stamp < cur.EndMs) {
		return tx.Commit()
	}
	if _, err = tx.ExecContext(ctx, "UPDATE match_ranked_season SET state=?,rollover_stage=?,updated_ms=? WHERE season_id=?",
		uint32(pb.RankedSeasonState_RANKED_SEASON_STATE_SETTLING), RolloverStageAdmissionStopped, stamp, seasonID); err != nil {
		return err
	}
	// 关闭旧季尚未成局的 PVP_TEAM 排队单排票与整队排队原件;已成局(state 2/3)留在旧季排空。
	if _, err = tx.ExecContext(ctx, "UPDATE match_authority_ticket SET state=4 WHERE mode=? AND state IN(0,1) AND formation_id=0",
		uint32(pb.MatchMode_MATCH_MODE_PVP_TEAM)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE team_launch SET state=5 WHERE mode=? AND state=2",
		uint32(pb.MatchMode_MATCH_MODE_PVP_TEAM)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE team_launch_role r JOIN team_launch l ON l.actor_id=r.actor_id AND l.request_high=r.request_high AND l.request_low=r.request_low SET r.state=5 WHERE l.mode=? AND l.state=5 AND r.state=2",
		uint32(pb.MatchMode_MATCH_MODE_PVP_TEAM)); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	logx.Infof("[ranked] Season rollover stage 1 (ADMISSION_STOPPED) committed season=%d", seasonID)
	return nil
}

func (s *Store) rolloverCheckFormationsDrained(ctx context.Context, seasonID uint32) error {
	var inFlight uint64
	err := s.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM match_formation WHERE ranked_season_id=? AND (state IN(1,2,3) OR (state=4 AND retry_at_ms<>?))",
		seasonID, terminalOutcomeNoRetryMs).Scan(&inFlight)
	if err != nil {
		return err
	}
	if inFlight > 0 {
		return nil
	}
	stamp, err := s.Now(ctx)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx,
		"UPDATE match_ranked_season SET rollover_stage=?,updated_ms=? WHERE season_id=? AND rollover_stage=?",
		RolloverStageFormationsDrained, stamp, seasonID, RolloverStageAdmissionStopped)
	if err == nil {
		logx.Infof("[ranked] Season rollover stage 2 (FORMATIONS_DRAINED) committed season=%d", seasonID)
	}
	return err
}

func (s *Store) rolloverCheckRatingsDrained(ctx context.Context, seasonID uint32) error {
	var pending uint64
	err := s.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM match_formation f JOIN match_battle_completion c ON c.battle_id=f.battle_id WHERE f.ranked_season_id=? AND c.decision=? AND NOT EXISTS(SELECT 1 FROM match_rating_applied a WHERE a.battle_id=f.battle_id)",
		seasonID, uint32(pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE)).Scan(&pending)
	if err != nil {
		return err
	}
	if pending > 0 {
		return nil
	}
	stamp, err := s.Now(ctx)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx,
		"UPDATE match_ranked_season SET rollover_stage=?,updated_ms=? WHERE season_id=? AND rollover_stage=?",
		RolloverStageRatingsDrained, stamp, seasonID, RolloverStageFormationsDrained)
	if err == nil {
		logx.Infof("[ranked] Season rollover stage 3 (RATINGS_DRAINED) committed season=%d", seasonID)
	}
	return err
}

func (s *Store) rolloverStopRankSource(ctx context.Context, cur *pb.MatchRankedSeasonRecord) error {
	epoch := cur.SourceEpoch
	if epoch == 0 {
		epoch = uint64(cur.SeasonId)
	}
	expected, err := s.StopRankSource(ctx, epoch, uint32(pb.MatchMode_MATCH_MODE_PVP_TEAM))
	if err != nil {
		return err
	}
	stamp, err := s.Now(ctx)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx,
		"UPDATE match_ranked_season SET expected_rank_cursor=?,rollover_stage=?,updated_ms=? WHERE season_id=? AND rollover_stage=?",
		expected, RolloverStageRankStopped, stamp, cur.SeasonId, RolloverStageRatingsDrained)
	if err == nil {
		logx.Infof("[ranked] Season rollover stage 4 (RANK_STOPPED) committed season=%d expected_cursor=%d", cur.SeasonId, expected)
	}
	return err
}

func (s *Store) ensureSeasonRankBoard(ctx context.Context, cur *pb.MatchRankedSeasonRecord, rankAdmin rankpb.RankAdminClient, rankSecret string) (uint64, error) {
	if cur.RankBoardId != 0 {
		return cur.RankBoardId, nil
	}
	if rankAdmin == nil {
		return 0, errors.New("ranked: rank admin client not configured")
	}
	boardType, _ := RankBoardType(uint32(pb.MatchMode_MATCH_MODE_PVP_TEAM))
	epoch := cur.SourceEpoch
	if epoch == 0 {
		epoch = uint64(cur.SeasonId)
	}
	gen := cur.RankGeneration
	if gen == 0 {
		gen = uint64(cur.SeasonId)
	}
	req := &rankpb.OpenRankGenerationRequest{
		Board:       &rankpb.RankBoardKey{BoardType: boardType, Scope: rankpb.RankScope_RANK_SCOPE_GLOBAL, Period: cur.SeasonKey, Generation: gen},
		Producer:    "match",
		SourceEpoch: epoch,
		RequestId:   uint64(boardType)*100000 + epoch,
	}
	if err := serviceauth.Sign(rankpb.RankAdmin_OpenRankGeneration_FullMethodName, req, "match", rankSecret, uint64(time.Now().UnixMilli())); err != nil {
		return 0, err
	}
	resp, err := rankAdmin.OpenRankGeneration(ctx, req)
	if err != nil {
		return 0, err
	}
	if resp.GetErrorMessage().GetId() != 0 || resp.GetBoardId() == 0 {
		return 0, fmt.Errorf("ranked: OpenRankGeneration rejected for season=%d tip=%d", cur.SeasonId, resp.GetErrorMessage().GetId())
	}
	boardID := resp.GetBoardId()
	_, err = s.DB.ExecContext(ctx, "UPDATE match_ranked_season SET rank_board_id=? WHERE season_id=? AND rank_board_id=0", boardID, cur.SeasonId)
	if err != nil {
		return 0, err
	}
	_, _ = s.DB.ExecContext(ctx, "UPDATE match_rank_source SET board_id=? WHERE source_epoch=? AND mode=? AND board_id=0", boardID, epoch, uint32(pb.MatchMode_MATCH_MODE_PVP_TEAM))
	return boardID, nil
}

func (s *Store) rolloverCheckRankDrained(ctx context.Context, cur *pb.MatchRankedSeasonRecord, rankAdmin rankpb.RankAdminClient, rankSecret string) error {
	epoch := cur.SourceEpoch
	if epoch == 0 {
		epoch = uint64(cur.SeasonId)
	}
	var remaining uint64
	err := s.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM match_rank_source WHERE source_epoch=? AND mode=? AND committed_cursor<produced_cursor",
		epoch, uint32(pb.MatchMode_MATCH_MODE_PVP_TEAM)).Scan(&remaining)
	if err != nil {
		return err
	}
	if remaining > 0 {
		return nil
	}
	if rankAdmin != nil && cur.RankBoardId == 0 {
		if _, err := s.ensureSeasonRankBoard(ctx, cur, rankAdmin, rankSecret); err != nil {
			return err
		}
	}
	stamp, err := s.Now(ctx)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx,
		"UPDATE match_ranked_season SET rollover_stage=?,updated_ms=? WHERE season_id=? AND rollover_stage=?",
		RolloverStageRankDrained, stamp, cur.SeasonId, RolloverStageRankStopped)
	if err == nil {
		logx.Infof("[ranked] Season rollover stage 5 (RANK_DRAINED) committed season=%d", cur.SeasonId)
	}
	return err
}

func (s *Store) rolloverSealBoard(ctx context.Context, cur *pb.MatchRankedSeasonRecord, rankAdmin rankpb.RankAdminClient, rankSecret string) error {
	if rankAdmin != nil {
		boardID, err := s.ensureSeasonRankBoard(ctx, cur, rankAdmin, rankSecret)
		if err != nil {
			return err
		}
		epoch := cur.SourceEpoch
		if epoch == 0 {
			epoch = uint64(cur.SeasonId)
		}
		gen := cur.RankGeneration
		if gen == 0 {
			gen = uint64(cur.SeasonId)
		}
		req := &rankpb.FreezeRankGenerationRequest{
			BoardId:              boardID,
			Generation:           gen,
			SourceEpoch:          epoch,
			ExpectedSourceCursor: cur.ExpectedRankCursor,
			TopN:                 100,
			RequestId:            uint64(cur.SeasonId)*1000 + 1,
		}
		if err := serviceauth.Sign(rankpb.RankAdmin_FreezeRankGeneration_FullMethodName, req, "match", rankSecret, uint64(time.Now().UnixMilli())); err != nil {
			return err
		}
		resp, err := rankAdmin.FreezeRankGeneration(ctx, req)
		if err != nil {
			return err
		}
		tip := resp.GetErrorMessage().GetId()
		if tip != 0 && tip != uint32(table.RankError_kRankGenerationClosed) {
			return fmt.Errorf("ranked: FreezeRankGeneration rejected season=%d tip=%d", cur.SeasonId, tip)
		}
	}
	stamp, err := s.Now(ctx)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx,
		"UPDATE match_ranked_season SET rollover_stage=?,updated_ms=? WHERE season_id=? AND rollover_stage=?",
		RolloverStageBoardSealed, stamp, cur.SeasonId, RolloverStageRankDrained)
	if err == nil {
		logx.Infof("[ranked] Season rollover stage 6 (BOARD_SEALED) committed season=%d", cur.SeasonId)
	}
	return err
}

type rankedStandingRow struct {
	playerID    uint64
	ratingCenti int64
	achievedMs  uint64
	peakTier    pb.RankedTier
	honorBits   uint32
}

func (s *Store) rolloverSnapshotHonors(ctx context.Context, cur *pb.MatchRankedSeasonRecord, rankReader RankedBoardReader, lookupName RankedNameLookup) error {
	// 可选从已封榜 rank 读第 1 名核对;核心同分排序口径与 rank_score(score DESC, achieved_ms ASC, entity_id ASC) 完全一致。
	var boardTopPlayer uint64
	var boardTopScore int64
	if rankReader != nil {
		boardType, _ := RankBoardType(uint32(pb.MatchMode_MATCH_MODE_PVP_TEAM))
		gen := cur.RankGeneration
		if gen == 0 {
			gen = uint64(cur.SeasonId)
		}
		pageResp, err := rankReader.GetRankPage(ctx, &rankpb.GetRankPageRequest{
			Board:    &rankpb.RankBoardKey{BoardType: boardType, Scope: rankpb.RankScope_RANK_SCOPE_GLOBAL, Period: cur.SeasonKey, Generation: gen},
			Page:     1,
			PageSize: 1,
		})
		if err == nil && pageResp != nil && len(pageResp.GetEntries()) > 0 {
			boardTopPlayer = pageResp.GetEntries()[0].GetEntityId()
			boardTopScore = pageResp.GetEntries()[0].GetScore()
		}
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	locked, err := scanSeason(tx.QueryRowContext(ctx, "SELECT "+seasonColumns+" FROM match_ranked_season WHERE season_id=? FOR UPDATE", cur.SeasonId))
	if err != nil {
		return err
	}
	if locked.RolloverStage != RolloverStageBoardSealed {
		return tx.Commit()
	}
	stamp, err := now(ctx, tx)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx,
		"SELECT ps.player_id,ps.rating_centi,COALESCE(NULLIF(r.score_changed_ms,0),ps.qualified_at_ms),ps.peak_tier,ps.honor_bits "+
			"FROM match_ranked_player_season ps LEFT JOIN match_rating r ON r.player_id=ps.player_id AND r.mode=? "+
			"WHERE ps.season_id=? AND ps.qualified=1",
		uint32(pb.MatchMode_MATCH_MODE_PVP_TEAM), cur.SeasonId)
	if err != nil {
		return err
	}
	var standings []rankedStandingRow
	for rows.Next() {
		var row rankedStandingRow
		var peakTier uint32
		if err = rows.Scan(&row.playerID, &row.ratingCenti, &row.achievedMs, &peakTier, &row.honorBits); err != nil {
			rows.Close()
			return err
		}
		row.peakTier = pb.RankedTier(peakTier)
		standings = append(standings, row)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}

	sort.Slice(standings, func(i, j int) bool {
		if standings[i].ratingCenti != standings[j].ratingCenti {
			return standings[i].ratingCenti > standings[j].ratingCenti
		}
		if standings[i].achievedMs != standings[j].achievedMs {
			return standings[i].achievedMs < standings[j].achievedMs
		}
		return standings[i].playerID < standings[j].playerID
	})

	cat := s.RankedCatalog()
	wushuangMin := int64(cat.Rule().GetTierWushuangCenti())

	var champPlayer uint64
	var champScore int64
	if boardTopPlayer != 0 {
		if boardTopScore >= wushuangMin {
			champPlayer = boardTopPlayer
			champScore = boardTopScore
		}
	} else if len(standings) > 0 && standings[0].ratingCenti >= wushuangMin {
		champPlayer = standings[0].playerID
		champScore = standings[0].ratingCenti
	}
	champName := ""
	if champPlayer != 0 && lookupName != nil {
		champName = lookupName(ctx, champPlayer)
	}

	for idx, st := range standings {
		rankPos := uint64(idx + 1)
		honorBits := st.honorBits
		if st.peakTier >= pb.RankedTier_RANKED_TIER_TIANSHU {
			tierCode := "tianshu"
			if st.peakTier == pb.RankedTier_RANKED_TIER_WUSHUANG {
				tierCode = "wushuang"
			}
			if _, err = grantRankedRewardTx(ctx, tx, st.playerID, cur.SeasonId, pb.RankedRewardKind_RANKED_REWARD_TIER_BADGE, uint32(st.peakTier), cur.SeasonKey+"_tier_badge_"+tierCode, stamp); err != nil {
				return err
			}
			if _, err = grantRankedRewardTx(ctx, tx, st.playerID, cur.SeasonId, pb.RankedRewardKind_RANKED_REWARD_TIER_TITLE, uint32(st.peakTier), cur.SeasonKey+"_tier_title_"+tierCode, stamp); err != nil {
				return err
			}
			honorBits |= 1 | 2
		}
		if champPlayer != 0 && st.playerID == champPlayer {
			if _, err = grantRankedRewardTx(ctx, tx, st.playerID, cur.SeasonId, pb.RankedRewardKind_RANKED_REWARD_CHAMPION_INSCRIPTION, uint32(pb.RankedTier_RANKED_TIER_WUSHUANG), cur.SeasonKey+"_champion_inscription", stamp); err != nil {
				return err
			}
			honorBits |= 4
		}
		if _, err = tx.ExecContext(ctx,
			"UPDATE match_ranked_player_season SET final_rank=?,honor_bits=?,updated_ms=? WHERE season_id=? AND player_id=?",
			rankPos, honorBits, stamp, cur.SeasonId, st.playerID); err != nil {
			return err
		}
	}

	if _, err = tx.ExecContext(ctx,
		"UPDATE match_ranked_season SET champion_player_id=?,champion_name=?,champion_rating_centi=?,rollover_stage=?,updated_ms=? WHERE season_id=?",
		champPlayer, champName, champScore, RolloverStageHonorsSnapshot, stamp, cur.SeasonId); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	logx.Infof("[ranked] Season rollover stage 7 (HONORS_SNAPSHOT) committed season=%d qualified=%d champion=%d score=%d", cur.SeasonId, len(standings), champPlayer, champScore)
	return nil
}

func (s *Store) rolloverOpenNextSeason(ctx context.Context, cur *pb.MatchRankedSeasonRecord) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	locked, err := scanSeason(tx.QueryRowContext(ctx, "SELECT "+seasonColumns+" FROM match_ranked_season WHERE season_id=? FOR UPDATE", cur.SeasonId))
	if err != nil {
		return err
	}
	if locked.RolloverStage != RolloverStageHonorsSnapshot {
		return tx.Commit()
	}
	stamp, err := now(ctx, tx)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx,
		"UPDATE match_ranked_season SET state=?,rollover_stage=?,updated_ms=? WHERE season_id=?",
		uint32(pb.RankedSeasonState_RANKED_SEASON_STATE_CLOSED), RolloverStageDone, stamp, locked.SeasonId); err != nil {
		return err
	}
	cat := s.RankedCatalog()
	nextID := locked.SeasonId + 1
	startMs := locked.EndMs
	if startMs == 0 {
		startMs = stamp
	}
	endMs := startMs + uint64(cat.Rule().GetSeasonDurationDays())*millisPerDay
	next := &pb.MatchRankedSeasonRecord{
		SeasonId:       nextID,
		SeasonKey:      fmt.Sprintf("s%d", nextID),
		SeasonName:     seasonNameFor(nextID),
		RankGeneration: uint64(nextID),
		SourceEpoch:    uint64(nextID),
		RuleVersion:    cat.RuleVersion(),
		State:          uint32(pb.RankedSeasonState_RANKED_SEASON_STATE_ACTIVE),
		RolloverStage:  RolloverStageNone,
		StartMs:        startMs,
		EndMs:          endMs,
		UpdatedMs:      stamp,
	}
	if _, err = tx.ExecContext(ctx, "INSERT IGNORE INTO match_ranked_season("+seasonColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		next.SeasonId, next.SeasonKey, next.SeasonName, next.RankGeneration, next.SourceEpoch, next.RuleVersion, next.State, next.RolloverStage,
		next.StartMs, next.EndMs, next.RankBoardId, next.ExpectedRankCursor, next.ChampionPlayerId, next.ChampionName, next.ChampionRatingCenti, next.UpdatedMs); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	logx.Infof("[ranked] Season rollover stage 8 (DONE) closed season=%d and opened season=%d key=%s", locked.SeasonId, next.SeasonId, next.SeasonKey)
	return nil
}
