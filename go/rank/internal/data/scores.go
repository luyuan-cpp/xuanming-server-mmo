package data

import (
	"context"
	"database/sql"
	"strings"

	rankpb "proto/rank"
)

// rankIndexOrder 是名次顺序,与 rank_score 的索引 (board_id,generation,order_key,achieved_ms,entity_id) 同向:
// 排序键小者在前;同分先达到者在前(achieved_ms 小);再同,按 entity_id 字节序(leaderboard-system.md L-9)。
// 名次连续,不并列。
const rankIndexOrder = "order_key,achieved_ms,entity_id"

// rankIndexOrderReverse 是 rankIndexOrder 的逆序,从某一行往前找邻居时用(仍走同一索引,倒着扫)。
const rankIndexOrderReverse = "order_key DESC,achieved_ms DESC,entity_id DESC"

// 在名次顺序上严格排在 (order_key, achieved_ms, entity_id) = (?, ?, ?) 之前 / 之后的行。三段 OR 都落在同一索引上,
// 优化器按多个范围扫;参数顺序固定为 key, key, achieved, key, achieved, entity(见 orderTupleArgs)。
const (
	aheadOfTuple  = "(order_key<? OR (order_key=? AND achieved_ms<?) OR (order_key=? AND achieved_ms=? AND entity_id<?))"
	behindOfTuple = "(order_key>? OR (order_key=? AND achieved_ms>?) OR (order_key=? AND achieved_ms=? AND entity_id>?))"
)

func orderTupleArgs(key int64, achieved, entity uint64) []any {
	return []any{key, key, achieved, key, achieved, entity}
}

// orderKey 把分数换成「名次越靠前越小」的排序键:升序榜就是分数本身;降序榜取按位取反(^score = -score-1),
// 单调递减、覆盖整个 int64、不会像取负那样在 MinInt64 上溢出。有了它,两种榜的名次顺序都与索引同向,
// 分页、求名次、封榜取前 N 都顺着索引走,不再整榜排序(缺口清单第 18 条)。
func orderKey(ascending uint32, score int64) int64 {
	if ascending != 0 {
		return score
	}
	return ^score
}

// storedScore 是 rank_score 里一行在合并时要用的字段。
type storedScore struct {
	score    int64
	achieved uint64
	version  uint64
}

// scoreWrite 是 Submit 合并后要写回的一行。
type scoreWrite struct {
	entity   uint64
	score    int64
	achieved uint64
	version  uint64
}

// loadScores 一次读出这一批里(没被移除的)实体在本代的现有行。调用方持榜行锁:同榜写入已串行,读到的就是提交点的值。
func loadScores(ctx context.Context, tx *sql.Tx, board, generation uint64, entries []*rankpb.RankScoreInput, removed map[uint64]bool) (map[uint64]storedScore, error) {
	ids := make([]any, 0, len(entries))
	for _, entry := range entries {
		if !removed[entry.EntityId] {
			ids = append(ids, entry.EntityId)
		}
	}
	existing := make(map[uint64]storedScore, len(ids))
	if len(ids) == 0 {
		return existing, nil
	}
	args := append([]any{board, generation}, ids...)
	rows, err := tx.QueryContext(ctx, "SELECT entity_id,score,achieved_ms,score_version FROM rank_score WHERE board_id=? AND generation=? AND entity_id IN ("+placeholders(len(ids))+")", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var entity uint64
		var s storedScore
		if err := rows.Scan(&entity, &s.score, &s.achieved, &s.version); err != nil {
			return nil, err
		}
		existing[entity] = s
	}
	return existing, rows.Err()
}

// upsertScores 一条多行语句写回这一批合并后的结果。值都是调用方在榜行锁里按现有行算好的,所以重复键时直接覆盖。
func upsertScores(ctx context.Context, tx *sql.Tx, b *rankpb.RankBoardRecord, cursor uint64, writes []scoreWrite) error {
	if len(writes) == 0 {
		return nil
	}
	const columns = 8
	args := make([]any, 0, len(writes)*columns)
	rowsSQL := make([]string, 0, len(writes))
	for _, w := range writes {
		rowsSQL = append(rowsSQL, "("+placeholders(columns)+")")
		args = append(args, b.BoardId, b.Generation, w.entity, w.score, orderKey(b.Ascending, w.score), w.achieved, cursor, w.version)
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO rank_score (board_id,generation,entity_id,score,order_key,achieved_ms,source_cursor,score_version) VALUES "+
		strings.Join(rowsSQL, ",")+
		" ON DUPLICATE KEY UPDATE score=VALUES(score),order_key=VALUES(order_key),achieved_ms=VALUES(achieved_ms),source_cursor=VALUES(source_cursor),score_version=VALUES(score_version)",
		args...)
	return err
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
