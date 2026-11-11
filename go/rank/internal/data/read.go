package data

import (
	"context"
	"database/sql"
	"errors"

	rankpb "proto/rank"
)

// rowQuerier 是 *sql.DB 与 *sql.Tx 共有的单行查询。
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (r *Repo) lookup(ctx context.Context, q rowQuerier, key *rankpb.RankBoardKey) (*rankpb.RankBoardRecord, error) {
	if key == nil || key.BoardType == 0 || key.Scope < rankpb.RankScope_RANK_SCOPE_GLOBAL || key.Scope > rankpb.RankScope_RANK_SCOPE_CUSTOM || len(key.Period) > 64 {
		return nil, ErrInvalid
	}
	query := "SELECT " + boardColumns + " FROM rank_board WHERE board_type=? AND scope=? AND scope_id=? AND deleted_ms=0"
	args := []any{key.BoardType, uint32(key.Scope), key.ScopeId}
	if key.Period != "" {
		query += " AND period=?"
		args = append(args, key.Period)
	}
	if key.Generation > 0 {
		query += " AND generation=?"
		args = append(args, key.Generation)
	}
	if key.Period == "" {
		query += " ORDER BY created_ms DESC,board_id DESC LIMIT 1"
	} else {
		query += " ORDER BY generation DESC LIMIT 1"
	}
	return scanBoard(q.QueryRowContext(ctx, query, args...))
}

func boardKey(b *rankpb.RankBoardRecord) *rankpb.RankBoardKey {
	return &rankpb.RankBoardKey{BoardType: b.BoardType, Scope: rankpb.RankScope(b.Scope), ScopeId: b.ScopeId, Period: b.Period, Generation: b.Generation}
}

// readTx 在一个可重复读事务里跑一组只读语句:榜行、总数、一页与本人名次取自同一快照,不会拼出旧总数配新名次。
// 不设 READ ONLY:authoritysql 的写者池进事务时要改写闸门的 writer_slot 行,只读事务会拒绝那条语句。
func (r *Repo) readTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// rankedEntry 是 player 在本代的分数行和名次,orderKey 是它在名次索引上的位置。
type rankedEntry struct {
	entry    *rankpb.RankEntry
	orderKey int64
}

// selfRank 求 player 在本代的名次:主键点查拿到他的排序键,再数索引上严格排在他前面的行。
// 范围计数走 (board_id,generation,order_key,achieved_ms,entity_id) 索引,开销随名次增长、与榜长无关;
// 以前每次都对全榜 ROW_NUMBER(缺口清单第 18 条)。不在榜上回 (nil, nil)。
func selfRank(ctx context.Context, q rowQuerier, b *rankpb.RankBoardRecord, player uint64) (*rankedEntry, error) {
	self := &rankedEntry{entry: &rankpb.RankEntry{EntityId: player}}
	err := q.QueryRowContext(ctx, "SELECT score,order_key,achieved_ms FROM rank_score WHERE board_id=? AND generation=? AND entity_id=?",
		b.BoardId, b.Generation, player).Scan(&self.entry.Score, &self.orderKey, &self.entry.AchievedMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ahead uint64
	args := append([]any{b.BoardId, b.Generation}, orderTupleArgs(self.orderKey, self.entry.AchievedMs, player)...)
	if err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM rank_score WHERE board_id=? AND generation=? AND "+aheadOfTuple, args...).Scan(&ahead); err != nil {
		return nil, err
	}
	self.entry.Rank = ahead + 1
	return self, nil
}

// scanEntries 读出 entity_id,score,achieved_ms 三列的若干行,名次由调用方按位置补。
func scanEntries(rows *sql.Rows) ([]*rankpb.RankEntry, error) {
	defer rows.Close()
	var out []*rankpb.RankEntry
	for rows.Next() {
		entry := &rankpb.RankEntry{}
		if err := rows.Scan(&entry.EntityId, &entry.Score, &entry.AchievedMs); err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

// Page 回一页与本人名次,同一快照(readTx)。页码 / 页长的钳制与以前相同:0→1、0→20、>50→50,越界页码钳到末页,
// 空榜 page_count = 1。总数是各片行 entries 之和(boardEntries,一代至多 maxSourceShards 行);这一页顺着名次索引
// LIMIT / OFFSET 取,开销随页码增长、与榜长无关。
func (r *Repo) Page(ctx context.Context, player uint64, in *rankpb.GetRankPageRequest) (*rankpb.GetRankPageResponse, error) {
	if in == nil || player == 0 {
		return nil, ErrInvalid
	}
	size := in.PageSize
	if size == 0 {
		size = 20
	}
	if size > 50 {
		size = 50
	}
	page := uint64(in.Page)
	if page == 0 {
		page = 1
	}
	var result *rankpb.GetRankPageResponse
	err := r.readTx(ctx, func(tx *sql.Tx) error {
		b, err := r.lookup(ctx, tx, in.Board)
		if err != nil {
			return err
		}
		total, err := boardEntries(ctx, tx, b.BoardId, b.Generation)
		if err != nil {
			return err
		}
		pages := (total + uint64(size) - 1) / uint64(size)
		if pages == 0 {
			pages = 1
		}
		selected := min(page, pages)
		result = &rankpb.GetRankPageResponse{
			Board:      boardKey(b),
			State:      rankpb.RankGenerationState(b.State),
			TotalCount: total,
			Page:       uint32(selected),
			PageSize:   size,
			PageCount:  pages,
		}
		offset := (selected - 1) * uint64(size)
		rows, err := tx.QueryContext(ctx, "SELECT entity_id,score,achieved_ms FROM rank_score WHERE board_id=? AND generation=? ORDER BY "+rankIndexOrder+" LIMIT ? OFFSET ?",
			b.BoardId, b.Generation, size, offset)
		if err != nil {
			return err
		}
		entries, err := scanEntries(rows)
		if err != nil {
			return err
		}
		for i, entry := range entries {
			entry.Rank = offset + uint64(i) + 1
			if entry.EntityId == player {
				result.Self = entry
			}
		}
		result.Entries = entries
		if result.Self != nil {
			return nil
		}
		self, err := selfRank(ctx, tx, b, player)
		if err != nil {
			return err
		}
		if self != nil {
			result.Self = self.entry
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Around 回本人与前后各 radius 名(radius 0→10、>25→25),同一快照。不在榜上回空列表,与以前相同。
// 前后邻居都从本人在名次索引上的位置顺着 / 倒着扫 radius 行,名次由本人名次推出,不整榜编号。
func (r *Repo) Around(ctx context.Context, player uint64, in *rankpb.GetRankAroundRequest) (*rankpb.GetRankAroundResponse, error) {
	if in == nil || player == 0 {
		return nil, ErrInvalid
	}
	radius := in.Radius
	if radius == 0 {
		radius = 10
	}
	if radius > 25 {
		radius = 25
	}
	var result *rankpb.GetRankAroundResponse
	err := r.readTx(ctx, func(tx *sql.Tx) error {
		b, err := r.lookup(ctx, tx, in.Board)
		if err != nil {
			return err
		}
		result = &rankpb.GetRankAroundResponse{Board: boardKey(b)}
		self, err := selfRank(ctx, tx, b, player)
		if err != nil || self == nil {
			return err
		}
		tuple := orderTupleArgs(self.orderKey, self.entry.AchievedMs, player)
		rows, err := tx.QueryContext(ctx, "SELECT entity_id,score,achieved_ms FROM rank_score WHERE board_id=? AND generation=? AND "+aheadOfTuple+" ORDER BY "+rankIndexOrderReverse+" LIMIT ?",
			append(append([]any{b.BoardId, b.Generation}, tuple...), radius)...)
		if err != nil {
			return err
		}
		before, err := scanEntries(rows)
		if err != nil {
			return err
		}
		rows, err = tx.QueryContext(ctx, "SELECT entity_id,score,achieved_ms FROM rank_score WHERE board_id=? AND generation=? AND "+behindOfTuple+" ORDER BY "+rankIndexOrder+" LIMIT ?",
			append(append([]any{b.BoardId, b.Generation}, tuple...), radius)...)
		if err != nil {
			return err
		}
		after, err := scanEntries(rows)
		if err != nil {
			return err
		}
		entries := make([]*rankpb.RankEntry, 0, len(before)+1+len(after))
		// before 是倒着扫出来的(离本人最近的在前),翻成名次升序;它的长度不超过本人前面的人数,名次不会到 0。
		for i := len(before) - 1; i >= 0; i-- {
			before[i].Rank = self.entry.Rank - uint64(i) - 1
			entries = append(entries, before[i])
		}
		entries = append(entries, self.entry)
		for i, entry := range after {
			entry.Rank = self.entry.Rank + uint64(i) + 1
			entries = append(entries, entry)
		}
		result.Entries = entries
		result.Self = self.entry
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
