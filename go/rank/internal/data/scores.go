package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/go-sql-driver/mysql"
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

// scoreWrite 是 Submit 合并后要写回的一行。existed:本代原来就有这一行(已被 loadScores 锁住)。
type scoreWrite struct {
	entity   uint64
	score    int64
	achieved uint64
	version  uint64
	existed  bool
}

// errScoreInsertRace:这一批要新插入的实体,刚被别的片的批次抢先插入了(唯一键冲突)。整笔事务回滚重试即可:
// 重试时那一行已经存在,会被 loadScores 锁住、按它的现值合并,不会拿旧值盖掉对方的分数。
var errScoreInsertRace = errors.New("排行榜实体被并发插入")

// loadScores 一次读出这一批里(没被移除的)实体在本代的现有行,并锁住它们(FOR UPDATE,按 entity_id 升序)。
// 同榜不同片的批次可以并行(缺口清单第 17 条),同一实体的合并靠这把行锁串行:锁住之后读到的就是提交点的值。
// 本代还没有的实体不在结果里,由 writeScores 普通 INSERT,撞唯一键回 errScoreInsertRace。
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
	rows, err := tx.QueryContext(ctx, "SELECT entity_id,score,achieved_ms,score_version FROM rank_score WHERE board_id=? AND generation=? AND entity_id IN ("+placeholders(len(ids))+") FOR UPDATE", args...)
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

// writeScores 写回这一批合并后的结果,至多两条多行语句:
//   - 原来就有的行(本事务已锁住):值是按锁住的现值算好的,重复键时直接覆盖;
//   - 新实体:普通 INSERT,不带 ON DUPLICATE KEY。别的片的批次可能刚插入同一个实体,撞唯一键(1062)回
//     errScoreInsertRace 让整笔重试,而不是拿没锁住的旧值把对方的分数盖掉。
func writeScores(ctx context.Context, tx *sql.Tx, b *rankpb.RankBoardRecord, cursor uint64, writes []scoreWrite) error {
	var existed, fresh []scoreWrite
	for _, w := range writes {
		if w.existed {
			existed = append(existed, w)
		} else {
			fresh = append(fresh, w)
		}
	}
	if len(existed) > 0 {
		values, args := scoreRows(b, cursor, existed)
		if _, err := tx.ExecContext(ctx, "INSERT INTO rank_score ("+scoreColumns+") VALUES "+values+
			" ON DUPLICATE KEY UPDATE score=VALUES(score),order_key=VALUES(order_key),achieved_ms=VALUES(achieved_ms),source_cursor=VALUES(source_cursor),score_version=VALUES(score_version)",
			args...); err != nil {
			return err
		}
	}
	if len(fresh) > 0 {
		values, args := scoreRows(b, cursor, fresh)
		_, err := tx.ExecContext(ctx, "INSERT INTO rank_score ("+scoreColumns+") VALUES "+values, args...)
		var duplicate *mysql.MySQLError
		if errors.As(err, &duplicate) && duplicate.Number == 1062 {
			return fmt.Errorf("%w: %v", errScoreInsertRace, err)
		}
		return err
	}
	return nil
}

// scoreColumns 与 scoreRows 的列序一致。
const scoreColumns = "board_id,generation,entity_id,score,order_key,achieved_ms,source_cursor,score_version"

// scoreRows 拼多行 VALUES 与实参,列序同 scoreColumns。
func scoreRows(b *rankpb.RankBoardRecord, cursor uint64, writes []scoreWrite) (string, []any) {
	const columns = 8
	args := make([]any, 0, len(writes)*columns)
	rowsSQL := make([]string, 0, len(writes))
	for _, w := range writes {
		rowsSQL = append(rowsSQL, "("+placeholders(columns)+")")
		args = append(args, b.BoardId, b.Generation, w.entity, w.score, orderKey(b.Ascending, w.score), w.achieved, cursor, w.version)
	}
	return strings.Join(rowsSQL, ","), args
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
