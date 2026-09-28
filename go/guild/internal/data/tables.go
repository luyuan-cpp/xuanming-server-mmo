// Package data 的库名与表清单:mmorpg_guild 独占库(port-decisions D-14,§8 已修订)。
// 表结构唯一事实源是 proto/guild/guild_db.proto;建表 / 加列只经 go/schemamigrate,本包不写 DDL。
package data

import (
	pb "proto/guild"

	"google.golang.org/protobuf/proto"
)

// DatabaseName 是帮会独占的逻辑库名,本地与 K8s 同名。config.Validate 断言 DSN 库名等于它,
// schemamigrate 连接后再断言 SELECT DATABASE() 等于它;tools/merge_zone 的 defaultGuildSchema 镜像本常量。
const DatabaseName = "mmorpg_guild"

// Tables 返回本库全部表,顺序即全库表间锁序:docs/design/guild-phase2/01-storage.md §2.1 的加锁位置,
// 2026-09-21 死锁修复后以 docs/design/guild-phase2/92-handoff.md §12.2 的全序为准
// (G guild < S guild_player_state < M guild_member < A guild_application < Q guild_player_op_seq
// < O guild_asset_op < C guild_daily_counter,B6a 起接 P guild_activity_progress)。
// 下游写事务按这里的先后取锁:任何事务都不得在持有靠后表的行锁之后回头去锁靠前的表。
// 本文件预先登记了一条例外:B6b 历练结算的"逐人 (Q → O)",理由与复核要求见下方 B6a 注释块里的"例外:新插 O 行";
// 它只在那条理由成立时才不成环,不是"回头锁"的通用许可。
//
// 新增表 = guild_db.proto 加 message 并在这里按锁序位置追加(schemamigrate 会补建后加的表),
// 同时追加两份删表清单(逆锁序,数量由测试按 len(Tables())+1 守着,漏加即红):
// internal/data/guild_repo_test.go 的 guildTestDropTables、internal/logic/economy_flow_integration_test.go 的 economyFlowDropTables。
func Tables() []proto.Message {
	return []proto.Message{
		&pb.GuildRecord{},
		&pb.GuildPlayerStateRecord{},
		&pb.GuildMemberRecord{},
		&pb.GuildApplicationRecord{},
		// B5a:资产三表。先 seq 行、再指令行、最后计数行 —— 经济事务按这个顺序第一次触碰各表。
		&pb.GuildPlayerOpSeqRecord{},
		&pb.GuildAssetOpRecord{},
		&pb.GuildDailyCounterRecord{},
		// B6a:帮会活动进度(记号 P),锁序位置 8 —— 全序变为 … < Q < O < C < P;
		// B6b 再接 guild_trial_battle(9)< guild_trial_reward_owed(10),都排在 P 之后。
		//
		// 为什么排在计数行 C 之后:06-activities.md 设计的每一类碰本表的事务,都是把前面各表的行取完才取它
		// (先用计数行 upsert 的 RowsAffected 判定"本人今天还能不能参与",再给帮会进度 +1);01-storage.md §2.1
		// 与 90-consistency.md part2 §1 也已按位置 8 登记。取完 P 之后对 G / M 行的资金、帮贡 UPDATE 是对**已持有行**
		// 的再写(与升级、捐献终结记资金同形),不算新的取锁位置。各事务的取锁序列:
		//   - 点灯 / 团圆(op=activity):G FOR UPDATE → 闸门 → M(G,p) 主键点锁 → [缺行插 Q]
		//     → Q(p, GUILD_CREDIT) FOR UPDATE(有物品奖励时由 AllocateSeq 锁,没有物品奖励也由 sqlLockSeqGuard 点锁 ——
		//       **恒锁 Q**,它是计数行 C 的写者守卫)→ [有物品奖励时插 O]
		//     → C(p, ACTIVITY, activity_id, 游戏日) 带上限 upsert → P(G, activity_id, 档期键) upsert + 主键点锁读回;
		//   - 历练结算(B6b,op=trial_settle,**预先登记、尚未落码**):G FOR UPDATE → M(候选人主键升序逐行点锁)
		//     → 按 player_id 升序逐人 (Q → O) → C(升序)→ P → guild_trial_battle → guild_trial_reward_owed。
		//     从第二人起,这是持着前一人**新插**的 O 行去锁下一人的 Q,字面上违反上面"不得回头锁靠前的表",
		//     只靠下面这条例外才不成环:
		//     例外:新插 O 行。本事务插入的 O 行在提交前,任何其他加锁者都拿不到它的 op_id 或索引项 ——
		//     Claim / ListDue / 提前截止都是先普通读出候选、再按 op_id 主键点锁**已提交**的行,AllocateSeq 数未决行是普通读,
		//     本库对 guild_asset_op 的锁定 / 写语句全部以 op_id 主键等值定位(asset_store.go、economy_repo.go),没有锁定范围扫描;
		//     同一 (p, 流) 的其他插入者又都先排在 Q(p) 上,而 Q(p) 已在本事务手里。所以没有人会在持有后一人的 Q 时
		//     等前一人的新 O 行,等待链闭合不了,不成环。
		//     **此例外需在 B6b 落码时于 92-handoff §12.2 登记并复核**:届时若任何路径新增了对 guild_asset_op 的锁定范围扫描、
		//     或按 (player_id, stream) 等索引前缀锁行,本例外即失效,结算必须改成"先按 player_id 升序锁全部 Q,再逐人插 O";
		//   - 解散(guild_manage_repo.go 的 DisbandGuild,X-14):G → 闸门 → S(全员↑)→ M(全员↑,锁后即删)→ A↑
		//     → O↑(提前截止)→ **删 P** →(B6b:删 guild_trial_battle)→ 删 G。删 P 必须插在提前截止之后、删 guild 行之前;
		//     只做到"删 guild_member 之后"不够 —— 排到删申请 / 提前截止前面,就是持着 P 回头取 A / O,违反全序。
		//
		// 真正挡住本表上死锁的是一条不变量:**本表任一行的锁定者 / 写者,都先持有同一 guild_id 的 guild 行锁**
		// (上面三类事务的第一把锁都是它)。同一帮的 P 行因此永远只有一个持锁者在推进:TiDB 下 upsert / 删行在语句末尾
		// 并行锁 {行 key, PRIMARY key}(非聚簇主键)也不会与别人各持一半;解散时候选集合也是稳定的(插行者都先锁 guild 行)。
		// 据此,后续读写本表的代码守三条:
		//   1. 只在已持有该帮 guild 行锁(FOR UPDATE)的事务里锁 / 写本表;捐献 / 兑换只普通读 guild 行,不得碰本表。
		//   2. 锁定语句只做完整主键 (guild_id, activity_id, period_key) 等值点操作(§12.2 第 2 条);解散按"普通读 guild_id
		//      前缀候选 → 主键升序逐行点删"做,不写 `DELETE … WHERE guild_id = ?` 前缀范围删(锁集随执行计划变,
		//      DisbandGuild 函数头已记录旧写法的问题)。
		//   3. 读路径(GetGuildActivities、团圆预检读进度)只用普通读。将来不持 guild 行锁的旧期清理(v1.1)照 asset_store.go
		//      的计数行清理做:逐行 RC 短事务"主键点锁 → 带复核点删",且只删已不可能再被写的旧期行(截止键早于任何进行中的档期)。
		&pb.GuildActivityProgressRecord{},
	}
}
