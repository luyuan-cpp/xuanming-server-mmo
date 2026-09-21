package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// block_repo.go —— 黑名单(friend_block)的存储访问(F2 §3.4)。
//
// 拉黑不是"好友边上的一个 flag":它是**单向**关系,而且被拉黑的人通常根本不是好友
// (被骚扰时才去拉黑),塞进好友边会让"非好友的拉黑"无处存放(见 friend_table.proto)。
//
// 三个方法的并发形态刻意不一样,原因都写在各自函数上:
//   - Block   会新增名额、会删好友边、会取消 pending → 走完整的容量守卫事务;
//   - Unblock 只会让数量变少 → 单条 DELETE,不要事务也不要守卫;
//   - ListBlocks 纯读 → 不缓存。
//
// 本文件与 friend_repo.go 是同一个包、同一个 FriendRepo:Block 必须与 AddFriend /
// AcceptFriend 共用**同一对容量守卫行**才能互斥(拆成独立的 BlockRepo 就得再拿一个 db 句柄,
// 而两个句柄各自开事务时没有任何东西能保证它们锁同一对行)。

// BlockEntry 是黑名单里的一条(ListBlocks 的返回形状)。
// SinceMs 在表里是 uint64、在 wire proto 的 BlockEntry.since_ms 是 int64,
// 所以这里直接用 int64 承接,logic 层不必再转一次(毫秒时间戳不会溢出 int63)。
type BlockEntry struct {
	BlockedPlayerID uint64
	SinceMs         int64
}

// Block 把 targetPlayerID 加入 playerID 的黑名单,并把两人之间的既有关系一并了结。
//
// 事务形状(逐字照 F2 §3.4,锁序见 friend_repo.go 顶部说明):
//
//	事务外 ① 快速失败:名额已满直接拒,**不** ensure 容量行(见下方理由;不进 runGuardedWrite 的重试)
//	runGuardedWrite(守卫缺行时整段至多重跑两次,见 friend_repo.go 顶部锁序说明 (5)):
//	  事务外 ensure 双方容量行
//	  BeginTx(RC)
//	    ① 容量守卫(升序锁 me 与 target 两行)—— 与 AddFriend / AcceptFriend 互斥的唯一依据
//	    body:
//	      ② 已拉黑?(锁定读)→ 幂等提交,不占新名额
//	      ③ 名额(锁定读 COUNT)≥ maxBlocks → ErrBlockListFull
//	      ④ INSERT IGNORE friend_block
//	      ⑤ 删双向好友边 + 按 RowsAffected 减 friend_count
//	      ⑥ 两个方向的 pending 置 rejected 并写 updated_ms
//	  Commit
//	提交后 → 失效双方的好友缓存与申请缓存
//
// **不推送**:被拉黑的人不该收到"你被拉黑了"的通知(照 A 仓的原则),
// 拉黑者自己也不需要推送(他的 RPC 回包就是结果)。
func (r *FriendRepo) Block(ctx context.Context, playerID, targetPlayerID uint64, maxBlocks uint32) error {
	// 拉黑自己没有任何语义,而且会让 ⑤ 的双向删边退化成删同一行两次。
	// 与 AddFriend 一样,logic 层已经先挡过一遍(§3.4),这里是防御性的第二道。
	if playerID == 0 || targetPlayerID == 0 || playerID == targetPlayerID {
		return errInvalidPlayerPair(playerID, targetPlayerID)
	}

	// 事务外快速失败:名额已满时直接拒,**不** ensure 容量行。
	//
	// 与 AcceptFriend 的 pending 预检、RemoveFriend 的 F2-15 前置判定同一个目的:
	// ensure 会给任意 target 凭空造出两行 friend_capacity。零好友的容量行现在由 sweep 的
	// SweepIdleCapacityRows 在保留期后回收(sweep_repo.go),但这道前置判定仍然保留:
	// 它挡的是回收周期**之内**的增长速度 —— Block 是本域唯一没有 §3.6 频率配额的写路径,不挡这一下,
	// 客户端就能用互不相同的随机 target 在一个保留期内任意撑大 friend_capacity(F2-15 的同类缺陷)。
	// 这一探是**普通读**:此刻还没有容量守卫,按顶部锁序 (2) 不允许做锁定读;
	// 权威判定仍是下面 ③ 守卫内的那条 FOR UPDATE COUNT,漏判一两条只是多进一次事务。
	//
	// ⚠ 它只封住"稳态名额"这一维:**Block + Unblock 反复换目标仍可绕过**
	// (每次 Unblock 之后名额又空出来,而 capacity 行已经建了,要等保留期过了才被回收)。
	// 那一维的增长速度只能靠 gate 按消息号
	// 限流(data/MessageLimiter.xlsx),本批按 §1.2 不改 xlsx —— 已在交付说明里登记:
	// 合并回 main 跑 regen 时,Block 要和三个新 tip 码一起进 MessageLimiter。
	if maxBlocks > 0 {
		var blocked, alreadyBlocked uint32
		if err := r.db.QueryRowContext(ctx,
			`SELECT COUNT(*), COUNT(CASE WHEN blocked_player_id = ? THEN 1 END)
			 FROM friend_block WHERE player_id = ?`,
			targetPlayerID, playerID).Scan(&blocked, &alreadyBlocked); err != nil {
			return fmt.Errorf("probe block count %d: %w", playerID, err)
		}
		// alreadyBlocked > 0 时**不能**按名额拒:重复拉黑同一个人是幂等成功、不占新名额
		// (与事务内 ② 的判定是同一条规则)。漏掉这个条件会让名单满之后
		// "再拉黑一次同一个人"被报满,玩家会以为拉黑失效了 —— 而且那时的 Block 还要靠
		// ⑤⑥ 去收敛残留的好友边与 pending,直接拒会把收敛一起跳过。
		if alreadyBlocked == 0 && blocked >= maxBlocks {
			return ErrBlockListFull
		}
	}

	// 容量行必须在事务外补齐(顶部锁序说明 (1)),哪怕 Block 只在 ⑤ 里可能改 friend_count:
	// 守卫需要行存在才能锁住它。ensure、BeginTx、① 与 Commit 都在 runGuardedWrite 里;
	// 上面的名额探针在它之外,不随回收竞态的重试重跑。
	//
	// ① 容量守卫(runGuardedWrite 取;Block 不用 friend_count 的值,所以闭包不收 counts)。
	// 锁**双方**而不是只锁 me:⑤ 要减 target 的 friend_count,
	// 而且只有锁住 target 才能与"以 target 为发起方的 AddFriend / AcceptFriend"互斥 ——
	// 只锁 me 的话,并发的 AcceptFriend(target 视角)仍能在本事务删边之后把边插回来,
	// 结果是"既是好友又在黑名单里"。
	err := r.runGuardedWrite(ctx, playerID, targetPlayerID, func(ctx context.Context, tx *sql.Tx, _ map[uint64]uint32) error {
		// 一个事务只取一次时刻:拉黑时间与被取消申请的 updated_ms 表达的是同一件事发生的时间,
		// 分两次 time.Now() 会让它们相差几毫秒,排查时序时反而要先怀疑是不是两次操作。
		// 取在闭包里:时刻属于真正拿到守卫的那一遍尝试,而不是可能已因回收竞态作废的前一遍。
		nowMs := time.Now().UnixMilli()

		// ② 幂等命中:已经拉黑过就直接提交(而不是 Rollback)。
		// 提交一个没有写入的事务与回滚在库层面等价,但语义上表达的是"本次调用成功"——
		// 而且下面的缓存失效仍然照做:客户端重复点"拉黑"时至少能拿到一份最新列表。
		// ⚠ 命中时**不**走 ③ 的名额校验:重复拉黑不占新名额,否则名单满了之后
		// 连"再拉黑一次同一个人"都会被拒,玩家会以为拉黑失效了。
		alreadyBlocked, err := blockExistsForUpdate(ctx, tx, playerID, targetPlayerID)
		if err != nil {
			return err
		}
		if !alreadyBlocked {
			// ③ 名额校验:守卫锁内的**锁定读** COUNT 才是权威。
			//
			// 这条 COUNT 保留 FOR UPDATE(与 AddFriendRequest 的两条 pending COUNT 不同,
			// 那两条刻意用普通读,理由见那里):它的加锁集合恒是"player_id = 操作者自己"的行,
			// 而任何会往这个集合里插行的事务(只有 Block 本身)都必须先持有**同一个** me 的
			// 容量守卫行,所以锁集不会跨玩家交叉,不存在 AddFriend 那种 ABBA。
			if maxBlocks > 0 {
				var blocked uint32
				if err := tx.QueryRowContext(ctx,
					"SELECT COUNT(*) FROM friend_block WHERE player_id=? FOR UPDATE",
					playerID).Scan(&blocked); err != nil {
					return fmt.Errorf("count blocks %d: %w", playerID, err)
				}
				// >= 而不是 >:本次要新增一条,等于上限时再加就超了。
				if blocked >= maxBlocks {
					return ErrBlockListFull
				}
			}

			// ④ 写黑名单。INSERT IGNORE 而不是 INSERT:② 与这里之间没有任何窗口
			//(同一事务、同一把守卫),但 IGNORE 让这条语句本身幂等,重试整个事务也安全。
			if _, err := tx.ExecContext(ctx,
				"INSERT IGNORE INTO friend_block (player_id, blocked_player_id, since_ms) VALUES (?, ?, ?)",
				playerID, targetPlayerID, nowMs); err != nil {
				return fmt.Errorf("insert block %d->%d: %w", playerID, targetPlayerID, err)
			}
		}

		// ⑤ 删双向好友边,**并按 RowsAffected 给对应的 friend_count 减 1**。
		//
		// ⚠ 这一步与 RemoveFriend 共用 deleteFriendEdges(那里有完整说明):漏减计数就是
		// 计数向上漂移,玩家永远加不满好友,而且**没有任何报错** —— 表现只是"明明只有 3 个好友
		// 却说列表满了"。block_repo_mysql_test.go 里有一条用例专门钉死这件事,别删它。
		//
		// 即使 ② 判定"早就拉黑过"也照样执行:拉黑与删边是两条独立的写,
		// 历史上可能出现"拉黑成功但删边没成功"(旧版本、人工改库),重复调用应当把它收敛干净。
		if err := deleteFriendEdges(ctx, tx, playerID, targetPlayerID, nowMs); err != nil {
			return err
		}

		// ⑥ 取消两人之间**两个方向**的 pending 申请(单条语句覆盖两行,天然原子)。
		// 置 rejected 而不是删行:玩家的申请列表要能解释"我发的申请怎么没了";
		// 行本身由 sweep 在保留期后回收。updated_ms 必须一起写(F2-14),
		// 否则这些行在 sweep 眼里永远停在 0。
		if _, err := tx.ExecContext(ctx,
			`UPDATE friend_request SET status=?, updated_ms=?
		 WHERE status=? AND ((from_player_id=? AND to_player_id=?) OR (from_player_id=? AND to_player_id=?))`,
			requestStatusRejected, nowMs, requestStatusPending,
			playerID, targetPlayerID, targetPlayerID, playerID); err != nil {
			return fmt.Errorf("cancel pending requests %d-%d: %w", playerID, targetPlayerID, err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	// 四个键都要失效:双方的好友列表(⑤ 删了边)、双方的申请列表(⑥ 两个方向的 pending 都动了)。
	// 漏掉任一个都会让对应视图最多陈旧 CacheTTL(默认 30 分钟),表现是"拉黑了还在列表里"。
	r.invalidateCachesAfterCommit(ctx,
		friendListKey(playerID),
		friendListKey(targetPlayerID),
		pendingRequestsKey(playerID),
		pendingRequestsKey(targetPlayerID),
	)
	return nil
}

// Unblock 从黑名单移除,幂等(删不到行不报错)。**不自动恢复好友关系** ——
// 拉黑时删掉的边是真删,恢复要靠重新加好友(这也是玩家的预期:解除拉黑不等于重新做朋友)。
//
// 刻意**不加容量守卫、不开事务**:它只会让黑名单数量变少,不可能越过任何上限,
// 而单条 DELETE 本身就是原子的。为它加一把守卫只会白白扩大与写路径的锁冲突面。
//
// 已知的良性竞态:Unblock 与 AddFriend 并发时,AddFriend 的拉黑探针可能在解除之前那一瞬
// 读到"仍被拉黑"而拒绝本次申请。玩家重试即可,不产生任何不一致状态,所以不为它加锁。
func (r *FriendRepo) Unblock(ctx context.Context, playerID, targetPlayerID uint64) error {
	if playerID == 0 || targetPlayerID == 0 || playerID == targetPlayerID {
		return errInvalidPlayerPair(playerID, targetPlayerID)
	}
	if _, err := r.db.ExecContext(ctx,
		"DELETE FROM friend_block WHERE player_id=? AND blocked_player_id=?",
		playerID, targetPlayerID); err != nil {
		return fmt.Errorf("unblock %d->%d: %w", playerID, targetPlayerID, err)
	}
	// 黑名单不进缓存(见 ListBlocks),所以这里没有缓存要失效。
	// 好友列表也不用动:Unblock 不改任何 friend 边。
	return nil
}

// ListBlocks 返回该玩家拉黑的人。
//
// **不缓存**,与好友列表 / 申请列表不同:
//   - 打开黑名单界面是低频操作,缓存收益近乎为零;
//   - 而缓存的代价是要再维护一套失效路径(Block / Unblock 都得记得失效),
//     少维护一条就会出现"解除拉黑后对方还在名单里"。KISS(AGENTS §11.2)。
//
// LIMIT 是防御性硬上限(F2-7,值 = Friend.ListReadHardLimit),不是分页:
// 正常条数由 MaxBlocks 封顶,远低于它;真被截断说明上限判定已经坏了,
// 此时宁可少返回几行,也不要把几万行塞进一个 gate 包体。
// 不加 ORDER BY:查询是 PK 的 player_id 前缀区间扫描,返回顺序本身稳定(按 blocked_player_id),
// 再排一次只是给每次读加一次 filesort。
func (r *FriendRepo) ListBlocks(ctx context.Context, playerID uint64) ([]BlockEntry, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT blocked_player_id, since_ms FROM friend_block WHERE player_id=? LIMIT ?",
		playerID, r.listReadHardLimit)
	if err != nil {
		return nil, fmt.Errorf("query blocks %d: %w", playerID, err)
	}
	defer rows.Close()

	blocks := []BlockEntry{}
	for rows.Next() {
		var entry BlockEntry
		if err := rows.Scan(&entry.BlockedPlayerID, &entry.SinceMs); err != nil {
			return nil, fmt.Errorf("scan block row %d: %w", playerID, err)
		}
		blocks = append(blocks, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate blocks %d: %w", playerID, err)
	}
	return blocks, nil
}

// blockExistsForUpdate 报告 playerID 是否已经拉黑了 targetPlayerID(**单向**,守卫之后的锁定读)。
//
// 与 friend_repo.go 的 blockedEitherWay 是两件事,别互相替换:
//   - 这里问的是"我的名单里有没有他"(决定要不要占一个新名额);
//   - blockedEitherWay 问的是"两人之间有没有任何方向的拉黑"(决定能不能建立关系)。
//     把 Block 的幂等判定换成双向,会让"A 拉黑了 B"直接导致"B 拉黑 A"被当成重复而静默不写。
func blockExistsForUpdate(ctx context.Context, tx *sql.Tx, playerID, targetPlayerID uint64) (bool, error) {
	var probe int
	err := tx.QueryRowContext(ctx,
		"SELECT 1 FROM friend_block WHERE player_id=? AND blocked_player_id=? FOR UPDATE",
		playerID, targetPlayerID).Scan(&probe)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return false, fmt.Errorf("check block %d->%d: %w", playerID, targetPlayerID, err)
}
