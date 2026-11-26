package authority

import (
	"context"
	"database/sql"
	"errors"

	battlepb "proto/battle"
	kafkapb "proto/contracts/kafka"
	pb "proto/match"

	"github.com/go-sql-driver/mysql"
)

const (
	MaxJianghuGrudgeRivals       = 50
	MaxJianghuGrudgeLogsPerRival = 20
)

// ApplyJianghuDuelBattleResult 在单事务内幂等入账一场天墉生死状的个人恩怨录与宿敌连胜/终结统计。
// 规则：
//   - 1V1 记录双方本人；2V2–5V5 仅记录双方队长（challenger_captain_id ↔ target_captain_id）。
//   - 按 (player_id, owner_revision, peer_player_id) 隔离：官方角色过户换手后旧恩怨自动清空。
//   - 单向「一笔勾销」后，若再次交手自动重新显现(dismissed=false)。
//   - 上限控制：每名玩家保留最近 50 位宿敌，每位宿敌保留最近 20 条交手战报摘要。
func (s *Store) ApplyJianghuDuelBattleResult(
	ctx context.Context,
	event *kafkapb.BattleResultEvent,
	ownerRevs map[uint64]uint64,
) (JianghuBattleGrudgeSummary, bool, error) {
	var summary JianghuBattleGrudgeSummary
	if s == nil || s.DB == nil || event == nil || event.GetBattleId() == 0 {
		return summary, false, nil
	}
	ctxDuel := event.GetJianghuDuelContext()
	if ctxDuel == nil || ctxDuel.GetChallengerCaptainId() == 0 || ctxDuel.GetTargetCaptainId() == 0 {
		return summary, false, nil
	}
	if event.GetTotalRounds() == 0 || event.GetOutcome() == battlepb.EBattleOutcome_BATTLE_OUTCOME_VOID {
		return summary, false, nil
	}
	var outcomeA, outcomeB uint32
	switch event.GetOutcome() {
	case battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN:
		outcomeA, outcomeB = 1, 2
		summary.WinnerPlayerID = ctxDuel.GetChallengerCaptainId()
		summary.LoserPlayerID = ctxDuel.GetTargetCaptainId()
	case battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_B_WIN:
		outcomeA, outcomeB = 2, 1
		summary.WinnerPlayerID = ctxDuel.GetTargetCaptainId()
		summary.LoserPlayerID = ctxDuel.GetChallengerCaptainId()
	case battlepb.EBattleOutcome_BATTLE_OUTCOME_DRAW:
		outcomeA, outcomeB = 3, 3
		summary.IsDraw = true
	default:
		return summary, false, nil
	}

	capA := ctxDuel.GetChallengerCaptainId()
	capB := ctxDuel.GetTargetCaptainId()
	revA := ownerRevs[capA]
	revB := ownerRevs[capB]
	finishedMs := event.GetFinishedAtMs()
	if finishedMs == 0 {
		finishedMs = uint64(event.GetBattleId())
	}

	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return summary, false, err
	}
	defer tx.Rollback()

	type sideUpdate struct {
		playerID    uint64
		ownerRev    uint64
		peerID      uint64
		peerName    string
		outcomeSelf uint32
	}
	sides := []sideUpdate{
		{playerID: capA, ownerRev: revA, peerID: capB, peerName: ctxDuel.GetTargetCaptainName(), outcomeSelf: outcomeA},
		{playerID: capB, ownerRev: revB, peerID: capA, peerName: ctxDuel.GetChallengerCaptainName(), outcomeSelf: outcomeB},
	}
	// 按 player_id 升序加锁，杜绝双方同时打完多局时的交叉死锁。
	if sides[0].playerID > sides[1].playerID {
		sides[0], sides[1] = sides[1], sides[0]
	}

	for _, side := range sides {
		// 1. 先按主键插入日志表作为严格幂等锚点。
		if _, err = tx.ExecContext(ctx,
			`INSERT INTO match_grudge_log(player_id,owner_revision,peer_player_id,battle_id,outcome_for_self,total_rounds,writ_tier,team_size,finished_ms)
			 VALUES(?,?,?,?,?,?,?,?,?)`,
			side.playerID, side.ownerRev, side.peerID, event.GetBattleId(),
			side.outcomeSelf, event.GetTotalRounds(), ctxDuel.GetWritTier(), ctxDuel.GetTeamSize(), finishedMs,
		); err != nil {
			var dup *mysql.MySQLError
			if errors.As(err, &dup) && dup.Number == 1062 {
				return summary, false, nil
			}
			return summary, false, err
		}

		// 2. 读取/锁定并更新恩怨聚合行。
		var wins, losses, draws, winStreak, loseStreak uint32
		err = tx.QueryRowContext(ctx,
			`SELECT wins,losses,draws,win_streak,lose_streak
			 FROM match_grudge_relation
			 WHERE player_id=? AND owner_revision=? AND peer_player_id=? FOR UPDATE`,
			side.playerID, side.ownerRev, side.peerID,
		).Scan(&wins, &losses, &draws, &winStreak, &loseStreak)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return summary, false, err
		}
		prevWinStreak := winStreak
		switch side.outcomeSelf {
		case 1:
			wins++
			winStreak++
			loseStreak = 0
			if side.playerID == summary.WinnerPlayerID {
				summary.WinnerNewWinStreak = winStreak
			}
		case 2:
			losses++
			winStreak = 0
			loseStreak++
			if side.playerID == summary.LoserPlayerID {
				summary.LoserPrevWinStreak = prevWinStreak
			}
		case 3:
			draws++
		}
		if _, err = tx.ExecContext(ctx,
			`INSERT INTO match_grudge_relation(player_id,owner_revision,peer_player_id,peer_name,wins,losses,draws,win_streak,lose_streak,last_battle_ms,dismissed)
			 VALUES(?,?,?,?,?,?,?,?,?,?,FALSE)
			 ON DUPLICATE KEY UPDATE
			   peer_name=VALUES(peer_name), wins=VALUES(wins), losses=VALUES(losses), draws=VALUES(draws),
			   win_streak=VALUES(win_streak), lose_streak=VALUES(lose_streak), last_battle_ms=VALUES(last_battle_ms), dismissed=FALSE`,
			side.playerID, side.ownerRev, side.peerID, side.peerName,
			wins, losses, draws, winStreak, loseStreak, finishedMs,
		); err != nil {
			return summary, false, err
		}

		// 3. 裁剪单宿敌历史战报至最新 20 条。
		if err = trimGrudgeLogs(ctx, tx, side.playerID, side.ownerRev, side.peerID); err != nil {
			return summary, false, err
		}
		// 4. 裁剪个人宿敌列表至最新 50 位。
		if err = trimGrudgeRivals(ctx, tx, side.playerID, side.ownerRev); err != nil {
			return summary, false, err
		}
	}

	if err = tx.Commit(); err != nil {
		return summary, false, err
	}
	return summary, true, nil
}

func trimGrudgeLogs(ctx context.Context, tx *sql.Tx, playerID, ownerRev, peerID uint64) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT battle_id FROM match_grudge_log
		 WHERE player_id=? AND owner_revision=? AND peer_player_id=?
		 ORDER BY finished_ms DESC, battle_id DESC LIMIT 100 OFFSET ?`,
		playerID, ownerRev, peerID, MaxJianghuGrudgeLogsPerRival)
	if err != nil {
		return err
	}
	var stale []uint64
	for rows.Next() {
		var bid uint64
		if scanErr := rows.Scan(&bid); scanErr != nil {
			rows.Close()
			return scanErr
		}
		stale = append(stale, bid)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, bid := range stale {
		if _, err = tx.ExecContext(ctx,
			"DELETE FROM match_grudge_log WHERE player_id=? AND owner_revision=? AND peer_player_id=? AND battle_id=?",
			playerID, ownerRev, peerID, bid); err != nil {
			return err
		}
	}
	return nil
}

func trimGrudgeRivals(ctx context.Context, tx *sql.Tx, playerID, ownerRev uint64) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT peer_player_id FROM match_grudge_relation
		 WHERE player_id=? AND owner_revision=?
		 ORDER BY last_battle_ms DESC, peer_player_id DESC LIMIT 100 OFFSET ?`,
		playerID, ownerRev, MaxJianghuGrudgeRivals)
	if err != nil {
		return err
	}
	var stalePeers []uint64
	for rows.Next() {
		var pid uint64
		if scanErr := rows.Scan(&pid); scanErr != nil {
			rows.Close()
			return scanErr
		}
		stalePeers = append(stalePeers, pid)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, pid := range stalePeers {
		if _, err = tx.ExecContext(ctx,
			"DELETE FROM match_grudge_log WHERE player_id=? AND owner_revision=? AND peer_player_id=?",
			playerID, ownerRev, pid); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx,
			"DELETE FROM match_grudge_relation WHERE player_id=? AND owner_revision=? AND peer_player_id=?",
			playerID, ownerRev, pid); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ListJianghuGrudges(ctx context.Context, playerID, ownerRevision uint64) ([]*pb.JianghuGrudgeEntry, error) {
	if s == nil || s.DB == nil || playerID == 0 {
		return nil, nil
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT peer_player_id,peer_name,wins,losses,draws,win_streak,lose_streak,last_battle_ms
		 FROM match_grudge_relation
		 WHERE player_id=? AND owner_revision=? AND dismissed=FALSE
		 ORDER BY last_battle_ms DESC, peer_player_id DESC LIMIT ?`,
		playerID, ownerRevision, MaxJianghuGrudgeRivals)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	entries := make([]*pb.JianghuGrudgeEntry, 0, 16)
	for rows.Next() {
		e := &pb.JianghuGrudgeEntry{}
		if scanErr := rows.Scan(&e.PeerPlayerId, &e.PeerName, &e.Wins, &e.Losses, &e.Draws, &e.WinStreak, &e.LoseStreak, &e.LastBattleMs); scanErr != nil {
			return nil, scanErr
		}
		entries = append(entries, e)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}

	for _, e := range entries {
		logRows, logErr := s.DB.QueryContext(ctx,
			`SELECT battle_id,outcome_for_self,total_rounds,writ_tier,team_size,finished_ms
			 FROM match_grudge_log
			 WHERE player_id=? AND owner_revision=? AND peer_player_id=?
			 ORDER BY finished_ms DESC, battle_id DESC LIMIT ?`,
			playerID, ownerRevision, e.PeerPlayerId, MaxJianghuGrudgeLogsPerRival)
		if logErr != nil {
			return nil, logErr
		}
		for logRows.Next() {
			l := &pb.JianghuGrudgeBattleLogEntry{}
			var writTier uint32
			if scanErr := logRows.Scan(&l.BattleId, &l.OutcomeForSelf, &l.TotalRounds, &writTier, &l.TeamSize, &l.FinishedAtMs); scanErr != nil {
				logRows.Close()
				return nil, scanErr
			}
			l.WritTier = pb.JianghuWritTier(writTier)
			e.RecentLogs = append(e.RecentLogs, l)
		}
		if err = logRows.Err(); err != nil {
			logRows.Close()
			return nil, err
		}
		logRows.Close()
	}
	return entries, nil
}

func (s *Store) DismissJianghuGrudge(ctx context.Context, playerID, ownerRevision, peerPlayerID uint64) (bool, error) {
	if s == nil || s.DB == nil || playerID == 0 || peerPlayerID == 0 {
		return false, nil
	}
	res, err := s.DB.ExecContext(ctx,
		`UPDATE match_grudge_relation SET dismissed=TRUE
		 WHERE player_id=? AND owner_revision=? AND peer_player_id=? AND dismissed=FALSE`,
		playerID, ownerRevision, peerPlayerID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
