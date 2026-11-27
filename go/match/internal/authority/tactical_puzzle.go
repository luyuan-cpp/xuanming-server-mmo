package authority

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"match/internal/puzzle"

	battlepb "proto/battle"
	kafkapb "proto/contracts/kafka"
	pb "proto/match"

	"google.golang.org/protobuf/proto"
)

var (
	ErrPuzzleNotFound               = errors.New("tactical puzzle not found")
	ErrPuzzleNotOwner               = errors.New("tactical puzzle not owner")
	ErrPuzzleLimitReached           = errors.New("tactical puzzle limit reached")
	ErrPuzzleSpecInvalid            = errors.New("tactical puzzle spec invalid")
	ErrPuzzleNotPublished           = errors.New("tactical puzzle not published")
	ErrPuzzleCannotLikeOwn          = errors.New("tactical puzzle cannot like own")
	ErrPuzzleClearRequiredToLike    = errors.New("tactical puzzle clear required to like")
	ErrPuzzleSolutionReplayNotFound = errors.New("tactical puzzle solution replay not found")
)

const puzzleSelectColumns = "puzzle_id,author_player_id,author_name,state,spec_revision,title,hint_text,tag_mask,spec_payload,verified_battle_id,author_best_rounds,attempt_count,clear_count,global_best_rounds,like_count,created_ms,published_ms,updated_ms"

func scanTacticalPuzzleRow(s scanner) (*pb.MatchTacticalPuzzleRecord, error) {
	r := &pb.MatchTacticalPuzzleRecord{}
	if err := s.Scan(
		&r.PuzzleId, &r.AuthorPlayerId, &r.AuthorName, &r.State, &r.SpecRevision,
		&r.Title, &r.HintText, &r.TagMask, &r.SpecPayload, &r.VerifiedBattleId,
		&r.AuthorBestRounds, &r.AttemptCount, &r.ClearCount, &r.GlobalBestRounds,
		&r.LikeCount, &r.CreatedMs, &r.PublishedMs, &r.UpdatedMs,
	); err != nil {
		return nil, err
	}
	return r, nil
}

func buildPuzzleSummaryView(r *pb.MatchTacticalPuzzleRecord, clearRow *pb.MatchTacticalPuzzleClearRecord) (*pb.TacticalPuzzleSummaryView, error) {
	spec := &battlepb.TacticalPuzzleSpec{}
	if len(r.SpecPayload) > 0 {
		if err := proto.Unmarshal(r.SpecPayload, spec); err != nil {
			return nil, fmt.Errorf("corrupt tactical puzzle spec %d: %w", r.PuzzleId, err)
		}
	}
	view := &pb.TacticalPuzzleSummaryView{
		PuzzleId:          r.PuzzleId,
		AuthorPlayerId:    r.AuthorPlayerId,
		AuthorName:        r.AuthorName,
		State:             pb.TacticalPuzzleState(r.State),
		SpecRevision:      r.SpecRevision,
		Title:             r.Title,
		HintText:          r.HintText,
		TagMask:           r.TagMask,
		Spec:              spec,
		AuthorBestRounds:  r.AuthorBestRounds,
		AttemptCount:      r.AttemptCount,
		ClearCount:        r.ClearCount,
		GlobalBestRounds:  r.GlobalBestRounds,
		LikeCount:         r.LikeCount,
		HasSolutionReplay: r.VerifiedBattleId != 0,
		PublishedMs:       r.PublishedMs,
		UpdatedMs:         r.UpdatedMs,
	}
	if clearRow != nil {
		view.SelfCleared = clearRow.Cleared
		view.SelfBestRounds = clearRow.BestRounds
		view.SelfLiked = clearRow.Liked
	}
	return view, nil
}

// SaveTacticalPuzzleDraft 新建或修改本人残局草稿。
// 修改已发布残局时原子递增 spec_revision、回落为 DRAFT，并释放旧标准解回放的 ref_count。
func (s *Store) SaveTacticalPuzzleDraft(
	ctx context.Context,
	authorPlayerID uint64,
	authorName string,
	puzzleID uint64,
	title, hint string,
	tagMask uint32,
	spec *battlepb.TacticalPuzzleSpec,
	nextPuzzleID func() (uint64, error),
) (*pb.TacticalPuzzleSummaryView, error) {
	if s == nil || s.DB == nil {
		return nil, errors.New("tactical puzzle store is not configured")
	}
	if authorPlayerID == 0 || nextPuzzleID == nil {
		return nil, ErrPuzzleSpecInvalid
	}
	if err := puzzle.ValidateSpec(spec); err != nil {
		return nil, ErrPuzzleSpecInvalid
	}
	if err := puzzle.ValidateTagMask(tagMask); err != nil {
		return nil, ErrPuzzleSpecInvalid
	}
	specBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(spec)
	if err != nil {
		return nil, err
	}
	var out *pb.TacticalPuzzleSummaryView
	err = s.Fenced(ctx, func() error {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err = s.lockActor(ctx, tx, authorPlayerID); err != nil {
			return err
		}
		stamp, err := now(ctx, tx)
		if err != nil {
			return err
		}
		var rec *pb.MatchTacticalPuzzleRecord
		if puzzleID == 0 {
			var ownedCount int
			if err = tx.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM match_tactical_puzzle WHERE author_player_id=? FOR UPDATE",
				authorPlayerID,
			).Scan(&ownedCount); err != nil {
				return err
			}
			if ownedCount >= puzzle.MaxPuzzlesPerPlayer {
				return ErrPuzzleLimitReached
			}
			newID, err := nextPuzzleID()
			if err != nil {
				return err
			}
			rec = &pb.MatchTacticalPuzzleRecord{
				PuzzleId:       newID,
				AuthorPlayerId: authorPlayerID,
				AuthorName:     authorName,
				State:          puzzle.StateDraft,
				SpecRevision:   1,
				Title:          title,
				HintText:       hint,
				TagMask:        tagMask,
				SpecPayload:    specBytes,
				CreatedMs:      stamp,
				UpdatedMs:      stamp,
			}
			if _, err = tx.ExecContext(ctx,
				"INSERT INTO match_tactical_puzzle("+puzzleSelectColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
				rec.PuzzleId, rec.AuthorPlayerId, rec.AuthorName, rec.State, rec.SpecRevision,
				rec.Title, rec.HintText, rec.TagMask, rec.SpecPayload, rec.VerifiedBattleId,
				rec.AuthorBestRounds, rec.AttemptCount, rec.ClearCount, rec.GlobalBestRounds,
				rec.LikeCount, rec.CreatedMs, rec.PublishedMs, rec.UpdatedMs,
			); err != nil {
				return err
			}
		} else {
			existing, err := scanTacticalPuzzleRow(tx.QueryRowContext(ctx,
				"SELECT "+puzzleSelectColumns+" FROM match_tactical_puzzle WHERE puzzle_id=? FOR UPDATE",
				puzzleID,
			))
			if errors.Is(err, sql.ErrNoRows) {
				return ErrPuzzleNotFound
			}
			if err != nil {
				return err
			}
			if existing.AuthorPlayerId != authorPlayerID {
				return ErrPuzzleNotOwner
			}
			oldVerifiedBattleID := existing.VerifiedBattleId
			existing.AuthorName = authorName
			existing.State = puzzle.StateDraft
			existing.SpecRevision++
			existing.Title = title
			existing.HintText = hint
			existing.TagMask = tagMask
			existing.SpecPayload = specBytes
			existing.VerifiedBattleId = 0
			existing.AuthorBestRounds = 0
			existing.UpdatedMs = stamp
			if _, err = tx.ExecContext(ctx,
				"UPDATE match_tactical_puzzle SET author_name=?,state=?,spec_revision=?,title=?,hint_text=?,tag_mask=?,spec_payload=?,verified_battle_id=0,author_best_rounds=0,updated_ms=? WHERE puzzle_id=?",
				existing.AuthorName, existing.State, existing.SpecRevision, existing.Title, existing.HintText,
				existing.TagMask, existing.SpecPayload, existing.UpdatedMs, existing.PuzzleId,
			); err != nil {
				return err
			}
			if oldVerifiedBattleID != 0 {
				if err = releaseBattleReportsTx(ctx, tx, map[uint64]uint32{oldVerifiedBattleID: 1}); err != nil {
					return err
				}
			}
			rec = existing
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		out, err = buildPuzzleSummaryView(rec, nil)
		return err
	})
	return out, err
}

// DeleteTacticalPuzzle 删除本人残局，同事务清理全部个人通关记录并释放标准解录像引用。
func (s *Store) DeleteTacticalPuzzle(ctx context.Context, authorPlayerID, puzzleID uint64) error {
	if s == nil || s.DB == nil {
		return errors.New("tactical puzzle store is not configured")
	}
	if authorPlayerID == 0 || puzzleID == 0 {
		return ErrPuzzleNotFound
	}
	return s.Fenced(ctx, func() error {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err = s.lockActor(ctx, tx, authorPlayerID); err != nil {
			return err
		}
		var ownerID, verifiedBattleID uint64
		err = tx.QueryRowContext(ctx,
			"SELECT author_player_id,verified_battle_id FROM match_tactical_puzzle WHERE puzzle_id=? FOR UPDATE",
			puzzleID,
		).Scan(&ownerID, &verifiedBattleID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrPuzzleNotFound
		}
		if err != nil {
			return err
		}
		if ownerID != authorPlayerID {
			return ErrPuzzleNotOwner
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM match_tactical_puzzle_clear WHERE puzzle_id=?", puzzleID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM match_tactical_puzzle WHERE puzzle_id=? AND author_player_id=?", puzzleID, authorPlayerID); err != nil {
			return err
		}
		if verifiedBattleID != 0 {
			if err = releaseBattleReportsTx(ctx, tx, map[uint64]uint32{verifiedBattleID: 1}); err != nil {
				return err
			}
		}
		return tx.Commit()
	})
}

// GetTacticalPuzzle 只读查询单题详情（草稿仅作者本人可见）。
func (s *Store) GetTacticalPuzzle(ctx context.Context, viewerID, puzzleID uint64) (*pb.TacticalPuzzleSummaryView, error) {
	if s == nil || s.DB == nil {
		return nil, errors.New("tactical puzzle store is not configured")
	}
	if puzzleID == 0 {
		return nil, ErrPuzzleNotFound
	}
	rec, err := scanTacticalPuzzleRow(s.DB.QueryRowContext(ctx,
		"SELECT "+puzzleSelectColumns+" FROM match_tactical_puzzle WHERE puzzle_id=?",
		puzzleID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPuzzleNotFound
	}
	if err != nil {
		return nil, err
	}
	if rec.State != puzzle.StatePublished && rec.AuthorPlayerId != viewerID {
		return nil, ErrPuzzleNotFound
	}
	clearRow, err := s.readPuzzleClearRow(ctx, puzzleID, viewerID)
	if err != nil {
		return nil, err
	}
	return buildPuzzleSummaryView(rec, clearRow)
}

func (s *Store) readPuzzleClearRow(ctx context.Context, puzzleID, playerID uint64) (*pb.MatchTacticalPuzzleClearRecord, error) {
	if playerID == 0 {
		return nil, nil
	}
	c := &pb.MatchTacticalPuzzleClearRecord{}
	err := s.DB.QueryRowContext(ctx,
		"SELECT puzzle_id,player_id,attempts,cleared,best_rounds,liked,first_cleared_ms,updated_ms FROM match_tactical_puzzle_clear WHERE puzzle_id=? AND player_id=?",
		puzzleID, playerID,
	).Scan(&c.PuzzleId, &c.PlayerId, &c.Attempts, &c.Cleared, &c.BestRounds, &c.Liked, &c.FirstClearedMs, &c.UpdatedMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

// ListTacticalPuzzles 按 5 大页签与可选标签掩码分页列出残局，同时返回本人荣誉档案。
func (s *Store) ListTacticalPuzzles(
	ctx context.Context,
	viewerID uint64,
	tab pb.TacticalPuzzleListTab,
	tagFilterMask, offset, limit uint32,
) ([]*pb.TacticalPuzzleSummaryView, uint32, *pb.TacticalPuzzleProfileView, error) {
	if s == nil || s.DB == nil {
		return nil, 0, nil, errors.New("tactical puzzle store is not configured")
	}
	if tab == pb.TacticalPuzzleListTab_TACTICAL_PUZZLE_LIST_TAB_UNSPECIFIED {
		tab = pb.TacticalPuzzleListTab_TACTICAL_PUZZLE_LIST_TAB_HOT
	}
	if limit == 0 {
		limit = puzzle.DefaultListLimit
	}
	if limit > puzzle.MaxListLimit {
		limit = puzzle.MaxListLimit
	}
	tagFilterMask &= puzzle.MaxTagMask

	var (
		rows *sql.Rows
		err  error
	)
	switch tab {
	case pb.TacticalPuzzleListTab_TACTICAL_PUZZLE_LIST_TAB_MY_CREATED:
		rows, err = s.DB.QueryContext(ctx,
			"SELECT "+puzzleSelectColumns+" FROM match_tactical_puzzle WHERE author_player_id=? ORDER BY updated_ms DESC,puzzle_id DESC LIMIT 200",
			viewerID,
		)
	case pb.TacticalPuzzleListTab_TACTICAL_PUZZLE_LIST_TAB_MY_CLEARED:
		rows, err = s.DB.QueryContext(ctx,
			"SELECT p.puzzle_id,p.author_player_id,p.author_name,p.state,p.spec_revision,p.title,p.hint_text,p.tag_mask,p.spec_payload,p.verified_battle_id,p.author_best_rounds,p.attempt_count,p.clear_count,p.global_best_rounds,p.like_count,p.created_ms,p.published_ms,p.updated_ms "+
				"FROM match_tactical_puzzle p INNER JOIN match_tactical_puzzle_clear c ON p.puzzle_id=c.puzzle_id "+
				"WHERE c.player_id=? AND c.cleared=1 ORDER BY c.updated_ms DESC,p.puzzle_id DESC LIMIT 200",
			viewerID,
		)
	case pb.TacticalPuzzleListTab_TACTICAL_PUZZLE_LIST_TAB_NEWEST:
		rows, err = s.DB.QueryContext(ctx,
			"SELECT "+puzzleSelectColumns+" FROM match_tactical_puzzle WHERE state=? ORDER BY published_ms DESC,puzzle_id DESC LIMIT 200",
			puzzle.StatePublished,
		)
	default: // HOT & HARDEST
		rows, err = s.DB.QueryContext(ctx,
			"SELECT "+puzzleSelectColumns+" FROM match_tactical_puzzle WHERE state=? ORDER BY like_count DESC,clear_count DESC,puzzle_id DESC LIMIT 200",
			puzzle.StatePublished,
		)
	}
	if err != nil {
		return nil, 0, nil, err
	}
	var all []*pb.MatchTacticalPuzzleRecord
	for rows.Next() {
		rec, scanErr := scanTacticalPuzzleRow(rows)
		if scanErr != nil {
			rows.Close()
			return nil, 0, nil, scanErr
		}
		if tagFilterMask != 0 && (rec.TagMask&tagFilterMask) == 0 {
			continue
		}
		all = append(all, rec)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, 0, nil, err
	}

	if tab == pb.TacticalPuzzleListTab_TACTICAL_PUZZLE_LIST_TAB_HARDEST {
		sort.SliceStable(all, func(i, j int) bool {
			a, b := all[i], all[j]
			aTried, bTried := a.AttemptCount > 0, b.AttemptCount > 0
			if aTried != bTried {
				return aTried
			}
			// 通过率 clear/attempt 越低越难：a.Clear * b.Attempt < b.Clear * a.Attempt
			lhs := uint64(a.ClearCount) * uint64(maxUint32(b.AttemptCount, 1))
			rhs := uint64(b.ClearCount) * uint64(maxUint32(a.AttemptCount, 1))
			if lhs != rhs {
				return lhs < rhs
			}
			if a.AttemptCount != b.AttemptCount {
				return a.AttemptCount > b.AttemptCount
			}
			return a.PuzzleId > b.PuzzleId
		})
	}

	totalCount := uint32(len(all))
	start := int(offset)
	if start > len(all) {
		start = len(all)
	}
	end := start + int(limit)
	if end > len(all) {
		end = len(all)
	}
	page := all[start:end]

	clearMap := map[uint64]*pb.MatchTacticalPuzzleClearRecord{}
	if viewerID != 0 && len(page) > 0 {
		for _, rec := range page {
			c, readErr := s.readPuzzleClearRow(ctx, rec.PuzzleId, viewerID)
			if readErr != nil {
				return nil, 0, nil, readErr
			}
			if c != nil {
				clearMap[rec.PuzzleId] = c
			}
		}
	}

	views := make([]*pb.TacticalPuzzleSummaryView, 0, len(page))
	for _, rec := range page {
		v, buildErr := buildPuzzleSummaryView(rec, clearMap[rec.PuzzleId])
		if buildErr != nil {
			return nil, 0, nil, buildErr
		}
		views = append(views, v)
	}

	profile, err := s.readPuzzleProfile(ctx, viewerID)
	if err != nil {
		return nil, 0, nil, err
	}
	return views, totalCount, profile, nil
}

func (s *Store) readPuzzleProfile(ctx context.Context, playerID uint64) (*pb.TacticalPuzzleProfileView, error) {
	prof := &pb.TacticalPuzzleProfileView{}
	if playerID == 0 {
		return prof, nil
	}
	rows, err := s.DB.QueryContext(ctx,
		"SELECT state,like_count FROM match_tactical_puzzle WHERE author_player_id=?",
		playerID,
	)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var st, likes uint32
		if err = rows.Scan(&st, &likes); err != nil {
			rows.Close()
			return nil, err
		}
		if st == puzzle.StatePublished {
			prof.PublishedCount++
			prof.TotalLikesReceived += likes
		} else {
			prof.DraftCount++
		}
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if err = s.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM match_tactical_puzzle_clear WHERE player_id=? AND cleared=1",
		playerID,
	).Scan(&prof.TotalClearedCount); err != nil {
		return nil, err
	}
	prof.HonorTier, prof.HonorTitle = puzzle.DeriveHonor(prof.PublishedCount, prof.TotalClearedCount, prof.TotalLikesReceived)
	return prof, nil
}

// LikeTacticalPuzzle 仅允许已通关该题的非作者玩家幂等点赞。
func (s *Store) LikeTacticalPuzzle(ctx context.Context, playerID, puzzleID uint64) (uint32, bool, error) {
	if s == nil || s.DB == nil {
		return 0, false, errors.New("tactical puzzle store is not configured")
	}
	if playerID == 0 || puzzleID == 0 {
		return 0, false, ErrPuzzleNotFound
	}
	var likeCount uint32
	var selfLiked bool
	err := s.Fenced(ctx, func() error {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err = s.lockActor(ctx, tx, playerID); err != nil {
			return err
		}
		var authorID uint64
		var state, curLikes uint32
		err = tx.QueryRowContext(ctx,
			"SELECT author_player_id,state,like_count FROM match_tactical_puzzle WHERE puzzle_id=? FOR UPDATE",
			puzzleID,
		).Scan(&authorID, &state, &curLikes)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrPuzzleNotFound
		}
		if err != nil {
			return err
		}
		if state != puzzle.StatePublished {
			return ErrPuzzleNotPublished
		}
		if authorID == playerID {
			return ErrPuzzleCannotLikeOwn
		}
		var cleared, liked bool
		err = tx.QueryRowContext(ctx,
			"SELECT cleared,liked FROM match_tactical_puzzle_clear WHERE puzzle_id=? AND player_id=? FOR UPDATE",
			puzzleID, playerID,
		).Scan(&cleared, &liked)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !cleared) {
			return ErrPuzzleClearRequiredToLike
		}
		if err != nil {
			return err
		}
		if liked {
			likeCount = curLikes
			selfLiked = true
			return tx.Commit()
		}
		stamp, err := now(ctx, tx)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx,
			"UPDATE match_tactical_puzzle_clear SET liked=1,updated_ms=? WHERE puzzle_id=? AND player_id=?",
			stamp, puzzleID, playerID,
		); err != nil {
			return err
		}
		curLikes++
		if _, err = tx.ExecContext(ctx,
			"UPDATE match_tactical_puzzle SET like_count=?,updated_ms=? WHERE puzzle_id=?",
			curLikes, stamp, puzzleID,
		); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		likeCount = curLikes
		selfLiked = true
		return nil
	})
	return likeCount, selfLiked, err
}

// GetTacticalPuzzleSolutionReplay 仅凭 puzzle_id 查表读取作者钉住的 verified_battle_id 对应回放，绝不接受客户端 battle_id。
func (s *Store) GetTacticalPuzzleSolutionReplay(ctx context.Context, viewerID, puzzleID uint64) (*battlepb.BattleReplayEnvelope, error) {
	if s == nil || s.DB == nil {
		return nil, errors.New("tactical puzzle store is not configured")
	}
	if puzzleID == 0 {
		return nil, ErrPuzzleNotFound
	}
	var authorID, verifiedBattleID uint64
	var state uint32
	err := s.DB.QueryRowContext(ctx,
		"SELECT author_player_id,state,verified_battle_id FROM match_tactical_puzzle WHERE puzzle_id=?",
		puzzleID,
	).Scan(&authorID, &state, &verifiedBattleID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPuzzleNotFound
	}
	if err != nil {
		return nil, err
	}
	if state != puzzle.StatePublished && authorID != viewerID {
		return nil, ErrPuzzleNotPublished
	}
	if verifiedBattleID == 0 {
		return nil, ErrPuzzleSolutionReplayNotFound
	}
	var raw []byte
	err = s.DB.QueryRowContext(ctx, "SELECT envelope FROM match_battle_report WHERE battle_id=?", verifiedBattleID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) || len(raw) == 0 {
		return nil, ErrPuzzleSolutionReplayNotFound
	}
	if err != nil {
		return nil, err
	}
	env := &battlepb.BattleReplayEnvelope{}
	if err = proto.Unmarshal(raw, env); err != nil {
		return nil, fmt.Errorf("corrupt tactical puzzle solution replay %d: %w", verifiedBattleID, err)
	}
	return env, nil
}

// PrepareTacticalPuzzleLaunch 校验开战资格并构建冻结的 TacticalPuzzleBattleContext。
func (s *Store) PrepareTacticalPuzzleLaunch(
	ctx context.Context,
	playerID, puzzleID uint64,
	isAuthorVerify bool,
) (*battlepb.TacticalPuzzleBattleContext, error) {
	if s == nil || s.DB == nil {
		return nil, errors.New("tactical puzzle store is not configured")
	}
	if playerID == 0 || puzzleID == 0 {
		return nil, ErrPuzzleNotFound
	}
	rec, err := scanTacticalPuzzleRow(s.DB.QueryRowContext(ctx,
		"SELECT "+puzzleSelectColumns+" FROM match_tactical_puzzle WHERE puzzle_id=?",
		puzzleID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPuzzleNotFound
	}
	if err != nil {
		return nil, err
	}
	if isAuthorVerify {
		if rec.AuthorPlayerId != playerID {
			return nil, ErrPuzzleNotOwner
		}
	} else {
		if rec.State != puzzle.StatePublished {
			return nil, ErrPuzzleNotPublished
		}
	}
	spec := &battlepb.TacticalPuzzleSpec{}
	if err = proto.Unmarshal(rec.SpecPayload, spec); err != nil {
		return nil, fmt.Errorf("corrupt tactical puzzle spec %d: %w", puzzleID, err)
	}
	if err = puzzle.ValidateSpec(spec); err != nil {
		return nil, ErrPuzzleSpecInvalid
	}
	return &battlepb.TacticalPuzzleBattleContext{
		PuzzleId:       rec.PuzzleId,
		SpecRevision:   rec.SpecRevision,
		IsAuthorVerify: isAuthorVerify,
		PuzzleTitle:    rec.Title,
		AuthorPlayerId: rec.AuthorPlayerId,
		AuthorName:     rec.AuthorName,
		Spec:           spec,
	}, nil
}

// recordTacticalPuzzleBattleCompletionTx 在 CommitBattleOutcome 唯一终局 SQL 事务内原子结算残局自证发布与破局统计。
// 依赖 match_battle_completion 主键防重放，保证同一 battle_id 仅结算一次。
func (s *Store) recordTacticalPuzzleBattleCompletionTx(
	ctx context.Context,
	tx *sql.Tx,
	f *pb.MatchFormationRecord,
	event *kafkapb.BattleResultEvent,
	stamp uint64,
) error {
	if f == nil || len(f.TacticalPuzzleContext) == 0 || event == nil {
		return nil
	}
	pctx := &battlepb.TacticalPuzzleBattleContext{}
	if err := proto.Unmarshal(f.TacticalPuzzleContext, pctx); err != nil || pctx.GetPuzzleId() == 0 {
		return nil
	}
	var playerID uint64
	var settlement *battlepb.BattleSettlementData
	for _, st := range event.GetSettlements() {
		if st != nil && st.GetPlayerId() != 0 {
			playerID = st.GetPlayerId()
			settlement = st
			break
		}
	}
	if playerID == 0 || settlement == nil {
		return nil
	}
	won := event.GetOutcome() == battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN &&
		settlement.GetPlayerTeamIndex() == 0 &&
		!settlement.GetFled() &&
		event.GetTotalRounds() > 0
	rounds := event.GetTotalRounds()

	rec, err := scanTacticalPuzzleRow(tx.QueryRowContext(ctx,
		"SELECT "+puzzleSelectColumns+" FROM match_tactical_puzzle WHERE puzzle_id=? FOR UPDATE",
		pctx.GetPuzzleId(),
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}

	if pctx.GetIsAuthorVerify() {
		if !won || playerID != rec.AuthorPlayerId || pctx.GetSpecRevision() != rec.SpecRevision {
			return nil
		}
		var reportExists int
		hasReport := false
		if qErr := tx.QueryRowContext(ctx,
			"SELECT 1 FROM match_battle_report WHERE battle_id=? FOR UPDATE",
			f.BattleId,
		).Scan(&reportExists); qErr == nil {
			hasReport = true
		} else if !errors.Is(qErr, sql.ErrNoRows) {
			return qErr
		}
		oldVerified := rec.VerifiedBattleId
		newVerified := oldVerified
		if hasReport {
			newVerified = f.BattleId
			if newVerified != oldVerified {
				if _, err = tx.ExecContext(ctx, "UPDATE match_battle_report SET ref_count=ref_count+1 WHERE battle_id=?", newVerified); err != nil {
					return err
				}
			}
		}
		authorBest := rec.AuthorBestRounds
		if authorBest == 0 || rounds < authorBest {
			authorBest = rounds
		}
		globalBest := rec.GlobalBestRounds
		if globalBest == 0 || rounds < globalBest {
			globalBest = rounds
		}
		pubMs := rec.PublishedMs
		if pubMs == 0 {
			pubMs = stamp
		}
		if _, err = tx.ExecContext(ctx,
			"UPDATE match_tactical_puzzle SET state=?,verified_battle_id=?,author_best_rounds=?,global_best_rounds=?,published_ms=?,updated_ms=? WHERE puzzle_id=?",
			puzzle.StatePublished, newVerified, authorBest, globalBest, pubMs, stamp, rec.PuzzleId,
		); err != nil {
			return err
		}
		if hasReport && oldVerified != 0 && oldVerified != newVerified {
			if err = releaseBattleReportsTx(ctx, tx, map[uint64]uint32{oldVerified: 1}); err != nil {
				return err
			}
		}
		return nil
	}

	// 普通挑战者攻关：仅统计已发布残局
	if rec.State != puzzle.StatePublished {
		return nil
	}
	var curAttempts, curBest uint32
	var curCleared, curLiked bool
	var curFirstClearedMs uint64
	rowExists := true
	err = tx.QueryRowContext(ctx,
		"SELECT attempts,cleared,best_rounds,liked,first_cleared_ms FROM match_tactical_puzzle_clear WHERE puzzle_id=? AND player_id=? FOR UPDATE",
		rec.PuzzleId, playerID,
	).Scan(&curAttempts, &curCleared, &curBest, &curLiked, &curFirstClearedMs)
	if errors.Is(err, sql.ErrNoRows) {
		rowExists = false
	} else if err != nil {
		return err
	}

	newAttempts := curAttempts + 1
	newlyCleared := false
	newCleared := curCleared
	newBest := curBest
	newFirstClearedMs := curFirstClearedMs
	if won {
		if !curCleared {
			newlyCleared = true
			newCleared = true
			newFirstClearedMs = stamp
		}
		if newBest == 0 || rounds < newBest {
			newBest = rounds
		}
	}
	if !rowExists {
		if _, err = tx.ExecContext(ctx,
			"INSERT INTO match_tactical_puzzle_clear(puzzle_id,player_id,attempts,cleared,best_rounds,liked,first_cleared_ms,updated_ms) VALUES(?,?,?,?,?,?,?,?)",
			rec.PuzzleId, playerID, newAttempts, newCleared, newBest, false, newFirstClearedMs, stamp,
		); err != nil {
			return err
		}
	} else {
		if _, err = tx.ExecContext(ctx,
			"UPDATE match_tactical_puzzle_clear SET attempts=?,cleared=?,best_rounds=?,first_cleared_ms=?,updated_ms=? WHERE puzzle_id=? AND player_id=?",
			newAttempts, newCleared, newBest, newFirstClearedMs, stamp, rec.PuzzleId, playerID,
		); err != nil {
			return err
		}
	}

	newAttemptCount := rec.AttemptCount + 1
	newClearCount := rec.ClearCount
	if newlyCleared {
		newClearCount++
	}
	newGlobalBest := rec.GlobalBestRounds
	if won && (newGlobalBest == 0 || rounds < newGlobalBest) {
		newGlobalBest = rounds
	}
	_, err = tx.ExecContext(ctx,
		"UPDATE match_tactical_puzzle SET attempt_count=?,clear_count=?,global_best_rounds=?,updated_ms=? WHERE puzzle_id=?",
		newAttemptCount, newClearCount, newGlobalBest, stamp, rec.PuzzleId,
	)
	return err
}

// purgeTacticalPuzzlesTx 删角收尾：删除该角色创作的全部残局及个人通关记录，并登记待释放的标准解录像引用。
func purgeTacticalPuzzlesTx(ctx context.Context, tx *sql.Tx, player uint64, released map[uint64]uint32) error {
	rows, err := tx.QueryContext(ctx,
		"SELECT puzzle_id,verified_battle_id FROM match_tactical_puzzle WHERE author_player_id=? ORDER BY puzzle_id",
		player,
	)
	if err != nil {
		return err
	}
	type owned struct{ puzzleID, battleID uint64 }
	var items []owned
	for rows.Next() {
		var o owned
		if err = rows.Scan(&o.puzzleID, &o.battleID); err != nil {
			rows.Close()
			return err
		}
		items = append(items, o)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, o := range items {
		if _, err = tx.ExecContext(ctx, "DELETE FROM match_tactical_puzzle_clear WHERE puzzle_id=?", o.puzzleID); err != nil {
			return err
		}
		res, delErr := tx.ExecContext(ctx, "DELETE FROM match_tactical_puzzle WHERE puzzle_id=? AND author_player_id=?", o.puzzleID, player)
		if delErr != nil {
			return delErr
		}
		if n, _ := res.RowsAffected(); n == 1 && o.battleID != 0 {
			released[o.battleID]++
		}
	}
	_, err = tx.ExecContext(ctx, "DELETE FROM match_tactical_puzzle_clear WHERE player_id=?", player)
	return err
}

func maxUint32(a, b uint32) uint32 {
	if a > b {
		return a
	}
	return b
}
