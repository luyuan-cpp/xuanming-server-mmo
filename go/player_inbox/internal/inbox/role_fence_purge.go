package inbox

import (
	"context"
	"database/sql"
	"fmt"
)

// RolePurge 是收件箱的删角收尾清理(docs/design/account-deletion.md §6.2),在 PurgeRemovedRole 的事务里跑:调用方已持这名玩家的
// 护栏副本行锁、核过是墓碑。删他的流行与条目;运维审计 player_inbox_audit 保留,它的第 10–14 列是条目原件的副本,不依赖条目行。
// open_count 在流行上,随流行一起删,没有跨玩家的计数。
//
// 锁序照本库:副本 → 流行 → 条目(store.go 顶部),写按主键前缀 (shard, player_id),不经二级索引加锁。墓碑之后入箱、消费、运维写入
// 都在他的副本上被拒;不过护栏的只有提醒簿记(一条按 next_seq 比较的单行 UPDATE),行没了就影响 0 行。
func RolePurge(ctx context.Context, tx *sql.Tx, player uint64) error {
	shard := RowShard(player)
	for _, table := range []string{streamTable, itemTable} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE `shard` = ? AND `player_id` = ?", shard, player); err != nil {
			return fmt.Errorf("inbox: purge %s of removed player %d: %w", table, player, err)
		}
	}
	return nil
}
