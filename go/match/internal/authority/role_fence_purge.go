package authority

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// rolePurgeDeletes 是删角收尾按玩家整删的行,都是他一个人的,别的玩家没有指向它们的引用。
var rolePurgeDeletes = []struct{ table, query string }{
	{"match_rating", "DELETE FROM match_rating WHERE player_id=?"},
	{"match_network_profile", "DELETE FROM match_network_profile WHERE player_id=?"},
	// 只删已到期的:未到期的占用由挂起判定挡住删角,走到这里的只会是到期行;万一不是,留给历练释放。
	{"match_role_occupancy", "DELETE FROM match_role_occupancy WHERE player_id=? AND expires_ms<=" + fenceNowMsSQL},
	// 观战与入队意图只删终态:观战占用 5=已证实关闭,观战意图 5=已关闭,入队意图 2=已提交原票、3=已取消。
	{"match_spectator", "DELETE FROM match_spectator WHERE player_id=? AND state=5"},
	{"match_spectator_intent", "DELETE FROM match_spectator_intent WHERE player_id=? AND state=5"},
	{"match_join_intent", "DELETE FROM match_join_intent WHERE player_id=? AND state IN (2,3)"},
	{"team_action_receipt", "DELETE FROM team_action_receipt WHERE actor_id=?"},
}

// RoleFencePurge 是 match(含组队)的删角收尾清理(docs/design/account-deletion.md §6.2),在 PurgeRemovedRole 的事务里跑:
// 调用方已持这名玩家的护栏副本行锁、核过是墓碑。删他一个人的行:评分、网络画像、本人战报列表、已到期的历练占用、终态的观战占用与
// 观战 / 入队意图、组队动作回执、本人的战斗剧本与选用(审核流水按审计保留);评级榜上的条目经待摘榜表异步摘掉(queueRankRemovalsTx)。
// 对局、结算、匹配票原件、切磋邀请、整队与活动意图是多名玩家共有的,保留;
// match_rating_applied 按对局 id 记「这一局的评分只入账一次」,不按玩家,删了重放的结果会给同局其他人再算一次分,也保留。
//
// 墓碑之后写这些行的入口(入队、观战、组队、占用、终局)都先锁他的副本,在我们之后被拒;剩下的只有观战意图的保留期清理与占用释放,
// 一次只碰他的行、不等别的锁。唯一与别人共有的是战报正文:本人列表行删掉后给正文减引用数,减到 0 删正文,与终局挤出同一段
// (releaseBattleReportsTx,battle_id 升序加锁)。
func RoleFencePurge(ctx context.Context, tx *sql.Tx, player uint64) error {
	released, err := purgeBattleReportListTx(ctx, tx, player)
	if err != nil {
		return fmt.Errorf("purge match_player_battle_report of removed player %d: %w", player, err)
	}
	if err = releaseBattleReportsTx(ctx, tx, released); err != nil {
		return fmt.Errorf("release battle reports of removed player %d: %w", player, err)
	}
	// 先记待摘榜再删评分行:评分行在,榜上就可能有他。
	if err = queueRankRemovalsTx(ctx, tx, player); err != nil {
		return fmt.Errorf("queue rank removals of removed player %d: %w", player, err)
	}
	for _, purge := range rolePurgeDeletes {
		if _, err = tx.ExecContext(ctx, purge.query, player); err != nil {
			return fmt.Errorf("purge %s of removed player %d: %w", purge.table, player, err)
		}
	}
	if err = purgeBattleScriptsTx(ctx, tx, player); err != nil {
		return fmt.Errorf("purge battle scripts of removed player %d: %w", player, err)
	}
	// 请求头指向的意图读不到按损坏拒绝(readSpectatorHead 的读者):还有没终结的意图就留着头。
	var open int
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM match_spectator_intent WHERE player_id=?", player).Scan(&open); err != nil {
		return fmt.Errorf("count spectator intents of removed player %d: %w", player, err)
	}
	if open == 0 {
		if _, err = tx.ExecContext(ctx, "DELETE FROM match_spectator_head WHERE player_id=?", player); err != nil {
			return fmt.Errorf("purge match_spectator_head of removed player %d: %w", player, err)
		}
	}
	return nil
}

// queueRankRemovalsTx 按他有评分的计分模式各记一行待摘榜(match_rank_removal),投榜循环随后向 rank 摘掉他(publishRankRemovals)。
// 评分行与榜项同生:有评分的模式才可能在那张榜上;不计分的模式没有榜,不记。重放时评分行已删(上一次提交了)就什么都不记,
// 没提交则整笔回滚,所以同一 (模式, 玩家) 至多一行;主键冲突按已记处理。
func queueRankRemovalsTx(ctx context.Context, tx *sql.Tx, player uint64) error {
	rows, err := tx.QueryContext(ctx, "SELECT mode FROM match_rating WHERE player_id=? ORDER BY mode", player)
	if err != nil {
		return err
	}
	var modes []uint32
	for rows.Next() {
		var mode uint32
		if err = rows.Scan(&mode); err != nil {
			rows.Close()
			return err
		}
		modes = append(modes, mode)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, mode := range modes {
		if _, rated := RankBoardType(mode); !rated {
			continue
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO match_rank_removal (mode,player_id,created_ms) VALUES (?,?,"+fenceNowMsSQL+
			") ON DUPLICATE KEY UPDATE created_ms=created_ms", mode, player); err != nil {
			return err
		}
	}
	return nil
}

// purgeBattleScriptsTx 删这名玩家自己写的战斗剧本和他的选用(docs/design/battle-script.md §6):剧本正文是他写的内容,随角色删;
// 选用只指向本人剧本或官方剧本,别的玩家没有指向它们的引用。审核流水 match_battle_script_review 是审核留痕(谁在什么时候审了
// 哪一版正文),按审计保留,不删(docs/design/account-deletion.md §6.2)。
// 剧本行先按 owner 索引普通读候选,再按主键逐行删,与审核对剧本行的 FOR UPDATE 点锁串行:审核先提交,我们随后删掉;
// 我们先删,审核读不到行按过时拒绝。玩家自己的提交、删除、选用都先锁他的副本,墓碑之后不会再有新行。
func purgeBattleScriptsTx(ctx context.Context, tx *sql.Tx, player uint64) error {
	rows, err := tx.QueryContext(ctx, "SELECT script_id FROM match_battle_script WHERE owner_player_id=? ORDER BY script_id", player)
	if err != nil {
		return err
	}
	var scripts []uint64
	for rows.Next() {
		var id uint64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		scripts = append(scripts, id)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM match_battle_script_loadout WHERE player_id=?", player); err != nil {
		return err
	}
	for _, id := range scripts {
		if _, err = tx.ExecContext(ctx, "DELETE FROM match_battle_script WHERE script_id=? AND owner_player_id=?", id, player); err != nil {
			return err
		}
	}
	return nil
}

// purgeBattleReportListTx 删这名玩家的全部战报列表行,回 battle_id → 要给正文减的引用数。与 evictBattleReportsTx 同一口径:
// 只按真正删掉的行计数,同一行被并发删到 0 行时不多减。
func purgeBattleReportListTx(ctx context.Context, tx *sql.Tx, player uint64) (map[uint64]uint32, error) {
	rows, err := tx.QueryContext(ctx, "SELECT created_ms,battle_id FROM match_player_battle_report WHERE player_id=? ORDER BY created_ms,battle_id", player)
	if err != nil {
		return nil, err
	}
	type listKey struct{ createdMs, battleID uint64 }
	var victims []listKey
	for rows.Next() {
		var k listKey
		if err = rows.Scan(&k.createdMs, &k.battleID); err != nil {
			rows.Close()
			return nil, err
		}
		victims = append(victims, k)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	released := map[uint64]uint32{}
	for _, v := range victims {
		result, err := tx.ExecContext(ctx, "DELETE FROM match_player_battle_report WHERE player_id=? AND created_ms=? AND battle_id=?", player, v.createdMs, v.battleID)
		if err != nil {
			return nil, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n == 1 {
			released[v.battleID]++
		}
	}
	return released, nil
}
