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
// < O guild_asset_op < C guild_daily_counter,B6a 起接 P guild_activity_progress,
// B6b 起再接 T guild_trial_battle < W guild_trial_reward_owed)。
// 下游写事务按这里的先后取锁:任何事务都不得在持有靠后表的行锁之后回头去锁靠前的表。
// B6a 曾在这里预先登记过一条例外(B6b 历练结算"逐人 (Q → O)"、靠"新插 O 行别人拿不到"才不成环)。
// B6b 落码时**没有采用**它:结算改成"先按 player_id 升序锁全部 Q,再逐人插 O",严格守表间全序,不需要任何例外
// (见下方 B6a 注释块的历练结算一条与 activity_repo.go 文件头)。
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
		// B6b 再接 guild_trial_battle(9,记号 T)< guild_trial_reward_owed(10,记号 W),都排在 P 之后(见本函数末尾两表的注释)。
		//
		// 为什么排在计数行 C 之后:06-activities.md 设计的每一类碰本表的事务,都是把前面各表的行取完才取它
		// (先用计数行 upsert 的 RowsAffected 判定"本人今天还能不能参与",再给帮会进度 +1);01-storage.md §2.1
		// 与 90-consistency.md part2 §1 也已按位置 8 登记。取完 P 之后对 G / M 行的资金、帮贡 UPDATE 是对**已持有行**
		// 的再写(与升级、捐献终结记资金同形),不算新的取锁位置。各事务的取锁序列:
		//   - 点灯 / 团圆(op=activity):G FOR UPDATE → 闸门 → M(G,p) 主键点锁 → [缺行插 Q]
		//     → Q(p, GUILD_CREDIT) FOR UPDATE(有物品奖励时由 AllocateSeq 锁,没有物品奖励也由 sqlLockSeqGuard 点锁 ——
		//       **恒锁 Q**,它是计数行 C 的写者守卫)→ [有物品奖励时插 O]
		//     → C(p, ACTIVITY, activity_id, 游戏日) 带上限 upsert → P(G, activity_id, 档期键) upsert + 主键点锁读回;
		//   - 历练结算(B6b,op=trial_settle,activity_repo.go 的 SettleTrialBattleTx,已落码):G FOR UPDATE → 闸门
		//     → [普通读 T 判重复 / 判归属,不加锁] → M(候选人主键升序逐行点锁)→ Q(合格者 player_id 升序:缺行先建、再点锁,**先锁完全部 Q**)
		//     → [普通读各人今日计数,定得奖名单] → O(得奖者升序逐人插新行;AllocateSeq 再锁的是本事务已持有的 Q 行,不是新锁)
		//     → C(得奖者升序,带上限 upsert)→ P(upsert + 主键点锁读回)→ 再写已持有的 G.funds / P 计数 / M 帮贡
		//     → T(缺行插入,有行则主键点锁后点改)→ W(未决已满者升序插新行)。
		//     B6a 预先登记的写法是"逐人 (Q → O)":从第二人起持着前一人新插的 O 行去锁下一人的 Q,要靠"新插 O 行别人拿不到"
		//     这条例外才不成环。落码时改成先锁完全部 Q 再插 O,多的只是每人一次已持有行的重复点锁,换来的是不依赖任何例外:
		//     以后有人给 guild_asset_op 加锁定范围扫描、或按 (player_id, stream) 前缀锁行,结算也不会因此成环。
		//   - 解散(guild_manage_repo.go 的 DisbandGuild,X-14):G → 闸门 → S(全员↑)→ M(全员↑,锁后即删)→ A↑
		//     → O↑(提前截止)→ **删 P** → **删 T(只删本帮 STARTED 行,主键升序逐行点删)** → 删 G。
		//     删 P / 删 T 必须插在提前截止之后、删 guild 行之前;只做到"删 guild_member 之后"不够 ——
		//     排到删申请 / 提前截止前面,就是持着 P / T 回头取 A / O,违反全序。
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
		// B6b:同道历练两表,锁序位置 9(记号 T)与 10(记号 W)—— 全序变为 … < C < P < T < W。
		//
		// T guild_trial_battle(一局一行;SETTLED 是结算的幂等闸门)与 P 守同一条不变量:
		// **本表任一行的插入者 / 锁定者 / 写者,都先持有该行 guild_id 对应的 guild 行锁(FOR UPDATE)**。
		// 写者一共五类,第一把锁都是 G:登记(RegisterTrialBattle:G → 插 T)、结算(见上)、毒消息标记(MarkTrialBattlePoison:
		// G → T)、巡检判 EXPIRED(ExpireTrialBattle:G → T)、解散(删本帮 STARTED 行)。由此:
		//   - 同一帮的 T 行任何时刻只有一个持锁者在推进,TiDB 下插入 / 点改 / 点删在语句末尾并行锁 {行 key, PRIMARY key}
		//     (非聚簇主键)不会与别人各持一半;同一 battle_id 的插入者也至多一个在途,首插者回滚时没有排队者,
		//     不会出现"查重锁被继承成间隙锁、插入意向互挡"的 1213(guild_manage_repo.go 文件头 (d) 的那一类);
		//   - 持着 G 之后对 T 的**普通读**就是权威的(同帮写者都排在 G 后面),所以结算把"是否已结算 / 是否属于本帮"
		//     放在事务开头用普通读判定,重复的结果事件不必先把发奖做一遍再回滚;锁定语句仍排在位置 9;
		//   - **帮会已解散时没有任何路径写 T**:G 行不存在,结算回 GuildGone、标记与判 EXPIRED 都是空操作,不落行
		//     (06 §6.30 的"单表补插 SETTLED/GUILD_GONE"因此作废 —— 那是一条不持守卫的 upsert,consumer 与巡检器同时处理同一局时
		//     在 TiDB 上会各持 {行 key, PRIMARY key} 的一半)。解散删掉本帮全部 STARTED 行之后,该帮剩下的 SETTLED / EXPIRED 行不可变。
		//   - 不持 G 的路径对 T **只许普通读**,共三处:视图(TrialBattle)、巡检候选(ListOverdueTrialBattles)、结算入口的终态预读
		//     (logic.SettleTrialResult 在开事务之前调 TrialBattle)。预读只把"SETTLED 且归属相符"当结论:SETTLED 行不可变
		//     (上面五类写者见到它都不再写,解散只删 STARTED 行),不持锁读到即权威;读到别的状态不下结论,仍由持着 G 的普通读判。
		//     它们都是事务之外的自动提交读,不取任何锁,不占全序里的位置。
		//   前提(不在无死锁论证之内的输入):同一 battle_id 的结果事件只带同一个 guild_id —— 上下文由 guild 自己发给 match、
		//   battle 原样回显。带着别的 guild_id 来的事件在普通读那一步就判 ContextMismatch,不会去锁别帮的行。
		//
		// 解散为什么只删 STARTED 行、不删全部:v1 不清理历练行,一个活跃帮会每天新增的行数与对局数同阶(满员帮可达上百行),
		// 历史行随帮会寿命线性增长。在解散事务(子预算 txBudgetDisband)里逐行点删全部历史,老帮会将永远超预算、解散不掉。
		// STARTED 行(在途对局)数有上界(一人同时至多在一局里),而且**必须**删:帮会行没了就没人能再写它,留着会一直卡在
		// 巡检器 `state = STARTED` 扫描的队头。SETTLED / EXPIRED 行不在任何扫描里,留作历史。
		&pb.GuildTrialBattleRecord{},
		// W guild_trial_reward_owed(结算时未决指令已满者的待入队物品)。写者:
		//   - 结算在位置 9 之后插新行(主键 (player_id, battle_id);幂等闸门已保证首次结算,不会与已提交的行撞键);
		//   - 转换(ConvertOwedReward):Q(p, GUILD_CREDIT) FOR UPDATE(AllocateSeq)→ 插 O 新行 → 完整主键点删 W。
		//     第一把锁是 Q;它不建 seq 行(有 W 行就一定已有 seq 行),所以不需要成员行守卫;同一行的两个转换者先在 Q(p) 上串行;
		//   - 诊断计数:自动提交、只改无索引列 attempts(与 guild_asset_op 的领取同形:变更集合只有行 key,只会单向等待)。
		// 解散不碰本表(物品属于玩家)。读路径一律普通读。
		&pb.GuildTrialRewardOwedRecord{},
	}
}
