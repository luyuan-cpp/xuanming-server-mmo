package data

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"
)

// rolePurgePeersSQL 普通读出与这名玩家有任何好友关系行的对端(好友边、申请、拉黑,两个方向)。不加锁:墓碑之后没有人能给他
// 新增关系行(涉及他的写事务都先锁他的护栏副本,见 lockRolePrefix),读到的就是全集。
const rolePurgePeersSQL = "SELECT friend_player_id FROM friend WHERE player_id = ?" +
	" UNION SELECT player_id FROM friend WHERE friend_player_id = ?" +
	" UNION SELECT to_player_id FROM friend_request WHERE from_player_id = ?" +
	" UNION SELECT from_player_id FROM friend_request WHERE to_player_id = ?" +
	" UNION SELECT blocked_player_id FROM friend_block WHERE player_id = ?" +
	" UNION SELECT player_id FROM friend_block WHERE blocked_player_id = ?"

// RolePurge 是 friend 的删角收尾清理(docs/design/account-deletion.md §6.2),在 PurgeRemovedRole 的事务里跑:调用方已持这名玩家的
// 护栏副本行锁、核过是墓碑。删 friend、friend_request、friend_block 里他在任一侧的行与他的 friend_capacity;对端因好友边被删
// 少掉的好友数在同一事务里减掉,与删好友、拉黑共用 deleteFriendEdges(按 RowsAffected 减,顺带刷新 created_ms)。
//
// 锁序照写路径:副本 → 容量行(他与全部对端,按 player_id 升序逐行主键点锁)→ 关系行,写一律完整主键点操作
// (friend_repo.go 顶部锁序说明 (2)(4)(6))。先点锁容量行,deleteFriendEdges 里带 friend_count > 0 的 UPDATE 才不会在 TiDB 上
// 与别人的守卫点锁各持一半;缺行(对端只有申请或拉黑)不加锁,那条 UPDATE 也影响 0 行。
// 申请数、拉黑数都是锁内 COUNT,没有计数列要改。缓存不用失效:列表读一律回源,提交后也没有挂点。
func RolePurge(ctx context.Context, tx *sql.Tx, player uint64) error {
	peers, err := rolePurgePeers(ctx, tx, player)
	if err != nil {
		return err
	}
	guards := append([]uint64{player}, peers...)
	slices.Sort(guards)
	for _, id := range slices.Compact(guards) {
		if _, err := lockRowExists(ctx, tx, lockCapacityRowSQL, id); err != nil {
			return fmt.Errorf("lock friend capacity row %d: %w", id, err)
		}
	}
	nowMs := time.Now().UnixMilli()
	for _, peer := range peers {
		if _, err := deleteFriendEdges(ctx, tx, player, peer, nowMs); err != nil {
			return err
		}
		for _, pair := range [2][2]uint64{{player, peer}, {peer, player}} {
			if _, err := tx.ExecContext(ctx, "DELETE FROM friend_request WHERE from_player_id = ? AND to_player_id = ?", pair[0], pair[1]); err != nil {
				return fmt.Errorf("purge friend request %d->%d: %w", pair[0], pair[1], err)
			}
			if _, err := tx.ExecContext(ctx, unblockSQL, pair[0], pair[1]); err != nil {
				return fmt.Errorf("purge block %d->%d: %w", pair[0], pair[1], err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM friend_capacity WHERE player_id = ?", player); err != nil {
		return fmt.Errorf("purge friend capacity %d: %w", player, err)
	}
	return nil
}

// rolePurgePeers 读出对端并按 player_id 升序去重:对端的容量行由此按守卫的同一全序取锁。
func rolePurgePeers(ctx context.Context, tx *sql.Tx, player uint64) ([]uint64, error) {
	rows, err := tx.QueryContext(ctx, rolePurgePeersSQL, player, player, player, player, player, player)
	if err != nil {
		return nil, fmt.Errorf("list friend peers of removed player %d: %w", player, err)
	}
	defer rows.Close()
	var peers []uint64
	for rows.Next() {
		var peer uint64
		if err := rows.Scan(&peer); err != nil {
			return nil, fmt.Errorf("scan friend peer of removed player %d: %w", player, err)
		}
		peers = append(peers, peer)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate friend peers of removed player %d: %w", player, err)
	}
	slices.Sort(peers)
	return slices.Compact(peers), nil
}
