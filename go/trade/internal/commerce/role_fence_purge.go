package commerce

import (
	"context"
	"database/sql"
	"fmt"
)

// RolePurge 是 trade 的删角收尾清理(docs/design/account-deletion.md §6.2),在 PurgeRemovedRole 的事务里跑:调用方已持这名玩家的
// 护栏副本行锁、核过是墓碑。只删他的收藏;挂单、合同、资产指令是与交易对手共有的成交与对账记录,按 §6.2 保留。
// 收藏只有按玩家的 COUNT,挂单上没有收藏数列,没有计数要改。收藏的写者(收藏 / 取消收藏)只碰本人的行,墓碑之后他不会再有会话。
//
// trade_player_op_seq 不删(偏离 §6.2,待负责人定):本模块的锁分析以「seq 行建出后永不删除」为前提(data/asset_op_repo.go
// 「拆开之后为什么仍然正确」第 2 点),由 TestSeqTableRowsAreNeverDeleted 扫源码机械守着。墓碑之后给他分配 seq 的合同事务都先在
// 他的副本上被拒,删了也安全,但要改那条守卫与它背后的论证;这一行只有计数,没有账号,与保留的资产指令里的 player_id 相同。
func RolePurge(ctx context.Context, tx *sql.Tx, player uint64) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM trade_favorite WHERE `player_id` = ?", player); err != nil {
		return fmt.Errorf("purge trade_favorite of removed player %d: %w", player, err)
	}
	return nil
}
