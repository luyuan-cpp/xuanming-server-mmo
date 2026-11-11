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

// RankExactLimit 是精确名次的上限(2026-11-11,缺口清单第 18 条,负责人定「前 N 名精确、之后显示 N+」):
// 本人与附近名次只精确数到这一名,翻页也只翻到这一名为止;排在它之后的条目 rank 为 0、beyond_rank_limit 为 true,
// 客户端显示「{rank_limit}+」。这样每次读的范围计数与 OFFSET 都以它为界,不随榜长和名次增长。
const RankExactLimit uint64 = 10000

// rankedEntry 是 player 在本代的分数行和位置:orderKey 是它在名次索引上的排序键;ahead 是索引上严格排在它前面的行数,
// 只数到 selfRank 的 countCap 为止。capped:数满了 countCap,只知道「前面至少这么多人」,名次不再精确。
type rankedEntry struct {
	entry    *rankpb.RankEntry
	orderKey int64
	ahead    uint64
	capped   bool
}

// selfRank 找 player 在本代的位置:主键点查拿到他的排序键,再数索引上严格排在他前面的行,最多数 countCap 行。
// 范围计数走 (board_id,generation,order_key,achieved_ms,entity_id) 索引,开销以 countCap 为界、与榜长和名次无关
// (缺口清单第 18 条)。entry.Rank 不在这里填,由调用方按上限用 rankOrBeyond 填。不在榜上回 (nil, nil)。
func selfRank(ctx context.Context, q rowQuerier, b *rankpb.RankBoardRecord, player, countCap uint64) (*rankedEntry, error) {
	self := &rankedEntry{entry: &rankpb.RankEntry{EntityId: player}}
	err := q.QueryRowContext(ctx, "SELECT score,order_key,achieved_ms FROM rank_score WHERE board_id=? AND generation=? AND entity_id=?",
		b.BoardId, b.Generation, player).Scan(&self.entry.Score, &self.orderKey, &self.entry.AchievedMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	args := append([]any{b.BoardId, b.Generation}, orderTupleArgs(self.orderKey, self.entry.AchievedMs, player)...)
	args = append(args, countCap)
	if err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM (SELECT 1 FROM rank_score WHERE board_id=? AND generation=? AND "+aheadOfTuple+" LIMIT ?) AS ahead",
		args...).Scan(&self.ahead); err != nil {
		return nil, err
	}
	self.capped = self.ahead >= countCap
	return self, nil
}

// positionAround 是本人前后第 delta 位(本人为 0、前面为负)的名次;本人的位置没数出来(数满了 countCap)时回 0,
// 表示在上限之后。纯函数。
func positionAround(self *rankedEntry, delta int64) uint64 {
	if self.capped {
		return 0
	}
	return uint64(int64(self.ahead) + 1 + delta)
}

// rankOrBeyond 给 entry 填名次:rank 在 [1, limit] 之内填精确名次;否则(包括 0 = 没数出来)rank 留 0、
// BeyondRankLimit 置 true,客户端显示「{limit}+」。纯函数。
func rankOrBeyond(entry *rankpb.RankEntry, rank, limit uint64) {
	if rank == 0 || rank > limit {
		entry.Rank = 0
		entry.BeyondRankLimit = true
		return
	}
	entry.Rank = rank
	entry.BeyondRankLimit = false
}

// rankPage 是一次翻页落在名次上的窗口:第 page 页(共 pages 页),从第 offset+1 名起取 rows 行。
type rankPage struct {
	page, pages, offset, rows uint64
}

// pageWindowFor 把请求的页码钳进 [1, pages]。pages 按 min(total, limit) 算,空榜 1 页;
// 最后一页只取到第 limit 名为止。页长 size 由调用方钳过(1~50)。纯函数。
func pageWindowFor(total, page, size, limit uint64) rankPage {
	ranked := min(total, limit)
	pages := max((ranked+size-1)/size, 1)
	selected := min(max(page, 1), pages)
	offset := (selected - 1) * size
	rows := size
	if offset+rows > ranked {
		rows = ranked - offset // ranked == 0 时 offset 也是 0;否则 offset < ranked
	}
	return rankPage{page: selected, pages: pages, offset: offset, rows: rows}
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

// Page 回一页与本人名次,同一快照(readTx)。页长钳制 0→20、>50→50;页码 0→1,越界钳到末页。
// 只翻到第 rankLimit 名为止(pageWindowFor),空榜 page_count = 1。总数是各片行 entries 之和(boardEntries,
// 一代至多 maxSourceShards 行),仍是真实人数。这一页顺着名次索引 LIMIT / OFFSET 取,OFFSET 不超过 rankLimit;
// 本人名次最多数 rankLimit 行,排在其后回 beyond_rank_limit(缺口清单第 18 条)。
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
		window := pageWindowFor(total, page, uint64(size), r.rankLimit)
		result = &rankpb.GetRankPageResponse{
			Board:      boardKey(b),
			State:      rankpb.RankGenerationState(b.State),
			TotalCount: total,
			Page:       uint32(window.page),
			PageSize:   size,
			PageCount:  window.pages,
			RankLimit:  r.rankLimit,
		}
		rows, err := tx.QueryContext(ctx, "SELECT entity_id,score,achieved_ms FROM rank_score WHERE board_id=? AND generation=? ORDER BY "+rankIndexOrder+" LIMIT ? OFFSET ?",
			b.BoardId, b.Generation, window.rows, window.offset)
		if err != nil {
			return err
		}
		entries, err := scanEntries(rows)
		if err != nil {
			return err
		}
		for i, entry := range entries {
			entry.Rank = window.offset + uint64(i) + 1 // 窗口只到第 rankLimit 名,页内名次都是精确的
			if entry.EntityId == player {
				result.Self = entry
			}
		}
		result.Entries = entries
		if result.Self != nil {
			return nil
		}
		self, err := selfRank(ctx, tx, b, player, r.rankLimit)
		if err != nil {
			return err
		}
		if self != nil {
			rankOrBeyond(self.entry, positionAround(self, 0), r.rankLimit)
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
// 本人前面最多数 rankLimit + radius 行:没数满时本人名次精确,前后邻居按它推出,超过 rankLimit 的标 beyond_rank_limit;
// 数满了说明本人与这 radius 名邻居都排在 rankLimit 之后,全部标 beyond_rank_limit(缺口清单第 18 条)。
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
		result = &rankpb.GetRankAroundResponse{Board: boardKey(b), RankLimit: r.rankLimit}
		self, err := selfRank(ctx, tx, b, player, r.rankLimit+uint64(radius))
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
			rankOrBeyond(before[i], positionAround(self, -int64(i)-1), r.rankLimit)
			entries = append(entries, before[i])
		}
		rankOrBeyond(self.entry, positionAround(self, 0), r.rankLimit)
		entries = append(entries, self.entry)
		for i, entry := range after {
			rankOrBeyond(entry, positionAround(self, int64(i)+1), r.rankLimit)
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
