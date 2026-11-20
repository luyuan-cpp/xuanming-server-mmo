package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// rolePurgeSteps 是删角收尾的逐表清理,按锁序排列:mail_box → 邮件 → 状态 → seq。list 普通读出要删的主键(参数是玩家),
// 列序与 remove 的占位符一一对应;remove 一律完整主键点删。
var rolePurgeSteps = []struct{ table, list, remove string }{
	// 收件人是他的个人邮件。先删邮件再删状态,与过期清理同序(Sweep 先锁邮件、再删状态)。
	{"mail_message", "SELECT mail_id FROM mail_message WHERE owner_player_id=? ORDER BY mail_id", "DELETE FROM mail_message WHERE mail_id=?"},
	{"mail_state", "SELECT player_id,mail_id FROM mail_state WHERE player_id=? ORDER BY mail_id", "DELETE FROM mail_state WHERE player_id=? AND mail_id=?"},
	{"mail_player_op_seq", "SELECT player_id,stream FROM mail_player_op_seq WHERE player_id=? ORDER BY stream", "DELETE FROM mail_player_op_seq WHERE player_id=? AND stream=?"},
	// 只删已封存名单(state=2)里的他:还在导入的名单封存时要核收件人行数等于导入人数,删了这一行整份名单再也封不上,
	// 其他收件人的奖励跟着发不出去。导入中的那一行留着(只有 player_id)。
	{"mail_campaign_recipient", "SELECT q.sender_kind,q.campaign_id,q.manifest_generation,q.player_id FROM mail_campaign c JOIN mail_campaign_recipient q" +
		" ON q.sender_kind=c.sender_kind AND q.campaign_id=c.campaign_id AND q.manifest_generation=c.manifest_generation AND q.player_id=?" +
		" WHERE c.state=2 ORDER BY q.sender_kind,q.campaign_id,q.manifest_generation",
		"DELETE FROM mail_campaign_recipient WHERE sender_kind=? AND campaign_id=? AND manifest_generation=? AND player_id=?"},
	{"mail_box", "SELECT player_id FROM mail_box WHERE player_id=?", "DELETE FROM mail_box WHERE player_id=?"},
}

// RolePurge 是 mail 的删角收尾清理(docs/design/account-deletion.md §6.2),在 PurgeRemovedRole 的事务里跑:调用方已持这名玩家的
// 护栏副本行锁、核过是墓碑。删他的邮箱行、收件人是他的个人邮件、他的邮件状态与 seq 行、已封存名单里的他;终态 mail_asset_op 是资产审计,
// 保留(删角前挂起判定已挡住未终态的指令)。满箱、共享邮件上限都是锁内 COUNT,没有计数列要改。
//
// seq 行只在领取里建、分配,领取先锁他的副本,所以 assetop「seq 行建出后永不删除」的前提对他不再需要。
// 先锁 mail_box:发件、物化、领取、终结都先锁它(inbox.go lockBox),我们提交之前它们排在后面。系统发件方(交易、榜单、战斗)
// 给他寄个人邮件时在事务里先锁他的副本复核(lockLiveRecipient,与这里同一把锁):这里先提交,之后的发件读到墓碑拒收
// (ErrRecipientDeleted),不会再给已删除的角色存邮件;发件先提交,那一封随这里一起删掉。发件的预检在建邮箱行之前,
// 只有这里恰好提交在预检与建行之间时会留下一行空邮箱行(只有 player_id,没有邮件)。
func RolePurge(ctx context.Context, tx *sql.Tx, player uint64) error {
	var box uint64
	if err := tx.QueryRowContext(ctx, "SELECT player_id FROM mail_box WHERE player_id=? FOR UPDATE", player).Scan(&box); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("lock mail_box of removed player %d: %w", player, err)
	}
	for _, step := range rolePurgeSteps {
		keys, err := rolePurgeKeys(ctx, tx, step.list, player)
		if err != nil {
			return fmt.Errorf("list %s of removed player %d: %w", step.table, player, err)
		}
		for _, key := range keys {
			if _, err = tx.ExecContext(ctx, step.remove, key...); err != nil {
				return fmt.Errorf("purge %s of removed player %d: %w", step.table, player, err)
			}
		}
	}
	return nil
}

// rolePurgeKeys 普通读出一组主键(每列都是无符号整数),读完关游标再写:同一个事务只有一条连接。
func rolePurgeKeys(ctx context.Context, tx *sql.Tx, query string, player uint64) ([][]any, error) {
	rows, err := tx.QueryContext(ctx, query, player)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var keys [][]any
	for rows.Next() {
		values := make([]uint64, len(columns))
		dest := make([]any, len(values))
		for i := range values {
			dest[i] = &values[i]
		}
		if err = rows.Scan(dest...); err != nil {
			return nil, err
		}
		key := make([]any, len(values))
		for i, value := range values {
			key[i] = value
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}
