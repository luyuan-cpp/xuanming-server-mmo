package data

import (
	"context"
	"database/sql"
	"errors"
)

// 同榜写入按来源片并行(2026-11-11,缺口清单第 17 条;docs/design/leaderboard-system.md「2026-11-11 同榜写入按来源片并行」)。
//
// 锁序只有一条:榜行 → 片行(按片号升序)→ 分数行(按 entity_id 升序)。
//   - Submit 先锁自己那一片的行,不碰榜行;本片还没有行时才去锁榜行、在榜行锁里建行。
//   - 封榜、移除、隐藏整代先锁榜行,再按片号升序锁本代全部片行(lockAllShards),与在途批次互斥。
//     建片行要先拿榜行锁,所以它们锁住的就是本代此刻全部的片行,之后不会冒出新片行。

// maxSourceShards 是来源片号的上限。封榜、移除、隐藏整代要锁住本代全部片行,片数必须有界;现有生产者 match 用 64 片。
const maxSourceShards = 1024

// shardRow 是 rank_board_shard 的一行。
type shardRow struct {
	shard     uint32
	committed uint64 // 本片已落账到的游标
	entries   uint64 // 记在本片行上的实体数,见 RankBoardShardRecord.entries 的注释
}

// lockShard 锁住 (board, generation, shard) 的片行并回它;本片还没有行时回 (nil, nil)。
func lockShard(ctx context.Context, tx *sql.Tx, board, generation uint64, shard uint32) (*shardRow, error) {
	row := &shardRow{shard: shard}
	err := tx.QueryRowContext(ctx, "SELECT committed_cursor,entries FROM rank_board_shard WHERE board_id=? AND generation=? AND source_shard=? FOR UPDATE",
		board, generation, shard).Scan(&row.committed, &row.entries)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return row, nil
}

// lockAllShards 按片号升序锁住本代全部片行。调用方必须已持榜行锁(见文件头的锁序)。
func lockAllShards(ctx context.Context, tx *sql.Tx, board, generation uint64) ([]shardRow, error) {
	rows, err := tx.QueryContext(ctx, "SELECT source_shard,committed_cursor,entries FROM rank_board_shard WHERE board_id=? AND generation=? ORDER BY source_shard FOR UPDATE",
		board, generation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var shards []shardRow
	for rows.Next() {
		var row shardRow
		if err := rows.Scan(&row.shard, &row.committed, &row.entries); err != nil {
			return nil, err
		}
		shards = append(shards, row)
	}
	return shards, rows.Err()
}

// committedTotal 是各片已落账游标之和:本代已落账的批次总数,封榜时与生产者的各片最终游标之和比对。
func committedTotal(shards []shardRow) uint64 {
	var total uint64
	for _, row := range shards {
		total += row.committed
	}
	return total
}

// shardToDecrement 选移除一个实体时减计数的片行:片号最小、计数大于 0 的那一行。实体记在哪一片行上没有业务含义,
// 只有各片之和有意义;调用方持全部片行锁,减哪一行和都对。全部为 0 时回 false(计数与数据对不上)。
func shardToDecrement(shards []shardRow) (uint32, bool) {
	for _, row := range shards {
		if row.entries > 0 {
			return row.shard, true
		}
	}
	return 0, false
}

// boardEntries 回本代榜上现有的实体数(各片行 entries 之和)。不加锁:读路径在可重复读事务里调,
// 与同一快照里的这一页、本人名次一致。
func boardEntries(ctx context.Context, q rowQuerier, board, generation uint64) (uint64, error) {
	var entries uint64
	err := q.QueryRowContext(ctx, "SELECT COALESCE(SUM(entries),0) FROM rank_board_shard WHERE board_id=? AND generation=?", board, generation).Scan(&entries)
	return entries, err
}
