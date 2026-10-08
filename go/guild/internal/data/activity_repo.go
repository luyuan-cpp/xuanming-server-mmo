package data

// 帮会活动的写事务与读:B6a 的元宵灯会 / 中秋团圆,B6b 的同道历练(登记对局、结算、毒消息标记、巡检判过期、
// 待入队物品转换),解散时的进度行与在途对局行清理,以及活动视图 / 后台循环要的读
// (设计 docs/design/guild-phase2/06-activities.md §6.8 / §6.9 / §6.11 / §6.12 / §6.14 / §6.26 / §6.29–§6.33;
// 90-consistency X-13 / X-14 / Y-04 与 92-handoff.md §12.2 / §12.4 的订正优先于 06 正文 —— 06 写于死锁修复之前)。
//
// 与 economy_repo.go 同一套基座:写事务只走 GuildRepo.inTx(READ COMMITTED、1213 / 9007 整事务重跑、1205 与子预算到期
// 归一成 ErrWriteConflict,logic 回 GuildBusyRetry);资产指令同一套写法(事务内 ensureSeqRowTx → assetop.AllocateSeq →
// insertAssetOp,只插 PENDING 行、不自己改玩家资产);计数行同一条带上限 upsert(upsertCounterWithLimit,X-13)。
// 本文件多出三张表:进度表 guild_activity_progress(记号 P,锁序位置 8)、历练对局表 guild_trial_battle(T,位置 9)、
// 待入队物品表 guild_trial_reward_owed(W,位置 10),见 tables.go。
//
// 规则判定(阈值是否达成、资金该不该发、入帮满没满 N 小时、个人 / 帮会周期键)一律调 guild/internal/activity 的纯函数,
// 不在这里另写一份:同一条规则要在视图、预检、事务三处得出相同结论(activity 包头)。repo 不读墙钟,
// 两个周期键都由调用方给的 NowMs 按 activity 包的规则现算,调用方无法传进一对彼此矛盾的键。
//
// # 取锁序列(表间全序 G < S < M < A < Q < O < C < P < T < W,92-handoff §12.2 第 1 条 + tables.go)
//
// 点灯 / 团圆(participate,op=activity):
//
//	G(guild_id) FOR UPDATE → M(G,p) 主键点锁(FORCE INDEX (PRIMARY))→ [Q(p, GUILD_CREDIT) 缺行则在事务内建]
//	→ Q(p, GUILD_CREDIT) FOR UPDATE(有物品奖励时由 AllocateSeq 锁,否则 sqlLockSeqGuard)→ [插 O 新行]
//	→ C(p, ACTIVITY, activity_id, 游戏日) 带上限 upsert → P(G, activity_id, 帮会期键) upsert → P 主键点锁读回
//	→ 对本事务已持有行的再写:G.funds、P 的锁存 / 资金标记、M 的帮贡。
//
// 解散(guild_manage_repo.go 的 DisbandGuild 调 deleteGuildActivityProgress、deleteGuildTrialBattles):
//
//	… → O↑(提前截止)→ P 普通读候选 → P 主键升序逐行点删 → T 普通读本帮 STARTED 候选 → T 主键升序逐行点删 → 删 G。
//
// 历练结算(SettleTrialBattleTx → settleTrialBattle,op=trial_settle):
//
//	G(guild_id) FOR UPDATE → 闸门 → [T 普通读:判重复 / 判归属,不加锁]
//	→ M(G, 候选人) 主键升序逐行点锁(FORCE INDEX (PRIMARY);不在帮的人跳过)
//	→ Q(合格者, GUILD_CREDIT) 按 player_id 升序:缺行在事务内建、再 sqlLockSeqGuard 点锁 —— **碰 O 之前锁完全部 Q**
//	→ [C 普通读:各人该游戏日已用次数,定得奖名单]
//	→ O:得奖者升序逐人插新行(AllocateSeq 再锁的是已持有的 Q 行;未决已满者不插、记入待入队名单)
//	→ C:得奖者升序带上限 upsert → P upsert → P 主键点锁读回
//	→ 对本事务已持有行的再写:G.funds、P 的胜场计数、M 的帮贡
//	→ T:缺行则插入终态行;有行则主键点锁 → 完整主键点改 → W:待入队名单升序插新行。
//
// 历练登记 / 毒消息标记 / 巡检判过期(RegisterTrialBattle / MarkTrialBattlePoison / ExpireTrialBattle,
// op=trial_register / trial_mark):
//
//	G(guild_id) FOR UPDATE → [T 普通读] → T 插入新行,或主键点锁 → 完整主键点改。
//
// 待入队物品转换(ConvertOwedReward,op=trial_owed):
//
//	Q(p, GUILD_CREDIT) FOR UPDATE(AllocateSeq)→ 插 O 新行 → W(p, battle_id) 完整主键点删。
//
// 为什么不成环(逐条对 §12.2):
//  1. 第一把锁是 G 的主键点锁。任何锁 guild 行的事务都把它当第一把锁,等 G 的一方手里没有别的库锁,等待链走到 G 就断
//     (与升级、捐献终结记资金、解散同形)。
//  2. M 在 G 之后、带 FORCE INDEX (PRIMARY):只经主键锁聚簇记录,与离帮 / 被踢 / 解散按主键删这一行同向(§12.2 第 2 条)。
//  3. Q 在 M 之后:seq 行的全部建行者都先持本人成员行(ensureSeqRowTx 的守卫不变量,死锁复核 C2)。
//     **没有物品奖励也锁 Q**:计数行的每个悲观写者都要先持同一 (p, 流) 的 seq 行(economy_repo.go 文件头第 2 条、
//     asset_store.go 文件头"计数行上的写者与取锁全序",死锁复核 C6);活动计数行归 GUILD_CREDIT —— 活动发奖走的那条流。
//     MySQL 上它还顺带把同一玩家的计数 upsert 与兑换预留插新计数行排成一列(C6 本身只在 TiDB 成环;MySQL READ COMMITTED 下
//     计数行 upsert 在聚簇主键上的重复键检查只加记录锁 LOCK_REC_NOT_GAP、不锁间隙,排成一列不是为了挡间隙锁冲突)。
//  4. O 只插新行;C 是带上限 upsert,不"锁不存在的行再插";两者都在 Q 之后,与 T-D / T-S / 终结同向。
//  5. P 在 C 之后。P 上的死锁由一条不变量挡住:**P 的任一锁定者 / 写者都先持同一 guild_id 的 G 行锁**(tables.go)。
//     同一帮的 P 行永远只有一个持锁者在推进,所以"先 upsert、再主键点锁读回"不会在 TiDB 上与别人各持一半
//     ({行 key, PRIMARY key} 在语句末尾并行加锁也没有第二个竞争者)。MySQL READ COMMITTED 下,upsert 命中已有行时
//     聚簇主键的重复键检查只加**记录锁**(LOCK_X | LOCK_REC_NOT_GAP),不锁间隙、不跨到相邻行;本表除主键外没有任何
//     二级索引(更没有二级唯一键,guild_db.proto),插新行也只在聚簇记录上留隐式锁 —— 别帮在相邻主键上插行时的插入意向锁
//     不受影响。结论不变:落在 P 上的等待只可能单向(本事务此后只再写已持有的行、不再新取锁),不成环。
//  6. P 之后对 G / M 的写都是本事务已持有行的再写,不是新的取锁位置;funds、帮贡两列不在任何二级索引里,
//     两条 UPDATE 都是完整主键等值、新值由锁内读到的旧值在 Go 里算好(溢出在 Go 里判,不靠 SQL 的无符号减法)。
//
// 历练(B6b)在上面六条之上再加五条:
//  7. 结算是本文件唯一一次锁多个玩家的事务。同表多行一律按主键升序逐行点锁(§12.2 第 2 条:`IN (…)` 拆成升序点查):
//     M 与踢人 / 任免的 lockMemberPair、解散的 lockAllMembers 同为 player_id 升序;Q 按 player_id 升序。
//     **Q 在碰 O 之前全部锁完**,事务因此严格守 Q < O,不需要 B6a 在 tables.go 预先登记的"新插 O 行"例外
//     (那是"逐人 (Q → O)"的写法:持着前一人新插的 O 行去锁下一人的 Q)。代价只是 AllocateSeq 对已持有的 Q 行再点锁一次。
//     持有全部合格者的本帮 M 行之后,还能与结算争 Q(p, GUILD_CREDIT) 的只剩两类不经**本帮**成员行的事务:
//     兑换被拒的终结(记给玩家旧帮的指令:[M(旧帮, p)] → Q → 已提交 op 行点锁 → 退 SHOP 计数行;同帮的那种要先拿 M(本帮, p),
//     排在结算后面)与待入队转换(Q → 插 O 新行 → 点删已提交的 W 行)。它们拿到 Q(p) 之后要的都不是结算持有的行
//     (结算在 O / W 上只有自己未提交的新行,在 C 上只碰 ACTIVITY 计数行 —— 活动发奖不退任何计数),只会让结算单向等待。
//     捐献的退款分支锁的是 GUILD_DEBIT 流的 seq 行,与结算没有交集。
//  8. 计数行:结算与点灯 / 团圆一样,先持 Q(p, GUILD_CREDIT) 再 upsert(死锁复核 C6)。得奖名单用**持有 Q 之后**的普通读定:
//     活动计数行的每个悲观写者都要先持同一把 Q,读到的值在提交前不会被别人改,所以正常运行下 upsert 不会再判"达上限";
//     真判到了(有人不守 C6 绕过 Q 写了计数行)就回 errRetryTx 整事务重跑,新一轮读到新值、该人不再得奖(06 §6.30 e 步)。
//     结算写的周期键是**开战时**的游戏日,不是"请求开头所在的周期",所以它不天然满足 upsertCounterWithLimit 的前提
//     (清理短事务不持 Q,靠"只删 8 天前的周期行"与 upsert 永不相遇)。trialCounterPeriodWritable 把这条前提补回来:
//     周期键已落进清理可能触及的范围时不发奖,回 ErrActivityPoison(确定性失败,转人工)。
//  9. P:结算同样先持 G 再写 P,第 5 条的不变量原样成立。历练进度行按游戏日一期一行,只在"本局有人得奖且配了资金"时才碰。
//  10. T(tables.go 的不变量):**T 行的每个插入者 / 锁定者 / 写者都先持该行 guild_id 的 G 行锁**。登记、结算、标记、判过期、
//     解散的第一把锁都是 G,同一帮的 T 行因此只有一个持锁者在推进;持着 G 之后对 T 的普通读就是权威的,所以"已结算 / 属于别帮"
//     在事务开头用普通读判掉,锁定语句仍排在位置 9。有行时"主键点锁 → 完整主键点改",缺行时普通 INSERT(不用 upsert:
//     同帮没有第二个插入者,撞键只可能来自带着别的 guild_id 的事件,那是内部错误,不该被 ON DUPLICATE 悄悄吞掉)。
//     帮会已解散(G 行不存在)时这几条路径都不写 T —— 没有守卫就不写,06 §6.30 的"单表补插 GUILD_GONE 行"作废。
//     不持 G 的路径对 T 只有普通读,共三处:视图(TrialBattle)、巡检候选(ListOverdueTrialBattles)、结算入口的终态预读
//     (logic 在开事务之前调 TrialBattle,只认不可变的 SETTLED)。它们是事务之外的自动提交读,不取任何锁,不进上面的取锁序列。
//  11. W:结算在全序末尾插新行;转换以 Q(p) 为第一把锁(不建 seq 行,所以不需要成员行守卫:有 W 行就一定已有 seq 行),
//     点删是不带复核条件的完整主键等值(TiDB 点写快路径,同 sqlDeleteActivityProgress);同一 W 行的两个转换者先在 Q(p) 上串行,
//     后到者点删影响 0 行即整体回滚。诊断计数(attempts)是自动提交、只改无索引列的单 key 写(与资产指令的领取同形)。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/zeromicro/go-zero/core/logx"

	assetpb "proto/common/asset"
	rollbackpb "proto/common/rollback"
	pb "proto/guild"

	"shared/assetop"
	tablepb "shared/generated/pb/table"

	"guild/internal/activity"
)

// 活动事务的业务拒绝。帮会不存在 / 不是成员 / 帮会等级不足 / 合服闸门 / 写冲突 / 未决指令过多直接复用已有哨兵
// (ErrGuildGone / ErrNotGuildMember / ErrGuildLevelTooLow / ErrZoneMerging / ErrWriteConflict / assetop.ErrTooManyPending),
// 不另造同义错误。定义在本文件而不是 guild_repo.go 的哨兵块:B6a-srv 的文件清单不含 guild_repo.go(交付说明已登记)。
var (
	// ErrActivityJoinTooRecent:入帮未满 GuildRule.activity_join_min_hours(事务内按成员行 join_time_ms 复核)。
	// logic 映射 kGuildActivityJoinTooRecent,参数 [N]。
	ErrActivityJoinTooRecent = errors.New("guild activity: joined the guild too recently")

	// ErrActivityAlreadyClaimed:本人本游戏日该活动次数已满(计数行带上限 upsert 判为达上限)。
	ErrActivityAlreadyClaimed = errors.New("guild activity: daily limit reached")

	// ErrActivityThresholdNotReached:团圆本档期未锁存,且调用方预检数到的合格在线人数不够。
	// logic 映射 kGuildActivityThresholdNotReached,参数用它预检时数到的 [online, threshold]。
	ErrActivityThresholdNotReached = errors.New("guild activity: reunion online threshold not reached")

	// ErrActivityPoison:确定性失败 —— 帮会资金 / 帮贡相加会溢出 uint64。整事务回滚,重试也一样,不是"忙"。
	// 单人写 RPC 下按内部故障返回(配表有上限,实际不可达);导出是给 B6b 结算用:结算撞上它要 markPoison 并销账,
	// 而不是让消费者无限重试同一条消息。
	ErrActivityPoison = errors.New("guild activity: deterministic failure (overflow)")
)

// guildActivityProgressTable 是进度表表名,唯一事实源是 proto/guild/guild_db.proto 的 OptionTableName。
const guildActivityProgressTable = "guild_activity_progress"

// activityReadBudget:活动读查询的子预算,与经济读同一档(90 part2 §2 第 8 条的 1000ms)。
// 读的都是主键 / 索引点查,跑满 1s 说明库出了状况,尽早回错比吃满请求预算好。
const activityReadBudget = economyReadBudget

const (
	// recentRejectedRewardScan:视图第 7b 步最多扫本人最近几条被拒指令(06 §6.8)。扫得少是刻意的:
	// 这只是"最近一次被拒的原因"提示,不是对账;按 uk 倒序取前几条,与历史行数无关。
	recentRejectedRewardScan = 4

	// rejectedRewardWindowMs:被拒原因在视图里保留 24h。终态行的 next_attempt_ms = 终结时刻(07 §7.4.1),按它判龄。
	rejectedRewardWindowMs uint64 = 86_400_000
)

const (
	// sqlLockActivityGuild:锁序位置 1。选 funds 是为了在 Go 里判资金溢出并算出新值(见 sqlSetActivityFunds)。
	sqlLockActivityGuild = `SELECT level, zone_id, funds FROM guild WHERE guild_id = ? FOR UPDATE`

	// sqlLockActivityMember:锁序位置 3。FORCE INDEX (PRIMARY) 的理由同 guild_manage_repo.go 的 sqlLockMemberRole:
	// 完整主键等值同时钉死了 uk_guild_member(player_id),走 uk 就是"先二级后主键",与按主键删成员行反序。
	sqlLockActivityMember = `SELECT join_time_ms, contribution_total, contribution_balance FROM guild_member FORCE INDEX (PRIMARY) WHERE guild_id = ? AND player_id = ? FOR UPDATE`

	// sqlSetActivityContribution:帮贡两列写成锁内算好的新值。完整主键等值,行已由 sqlLockActivityMember 锁住;
	// 帮贡列不在任何二级索引里,只动聚簇记录。增量 > 0 时新值一定不同于旧值,RowsAffected 恒为 1(ClientFoundRows=false)。
	sqlSetActivityContribution = `UPDATE guild_member FORCE INDEX (PRIMARY) SET contribution_total = ?, contribution_balance = ? WHERE guild_id = ? AND player_id = ?`

	// sqlSetActivityFunds:帮会资金写成锁内算好的新值(完整主键等值,行已由 sqlLockActivityGuild 锁住)。
	sqlSetActivityFunds = `UPDATE guild SET funds = ? WHERE guild_id = ?`
)

// 进度表 P 的语句。锁定 / 写语句一律完整主键 (guild_id, activity_id, period_key) 等值(§12.2 第 2 条);
// 按 guild_id 前缀找行的只有两条普通读(视图读、解散候选读),不带锁定子句。
const (
	// sqlUpsertLanternProgress:灯会给本档期进度 +1,行不存在就建成 1。不先"锁不存在的行再插"(01-storage §2.1 规则 3):
	// 同一帮的 P 写者都已在 G 行锁上串行(文件头第 5 条),upsert 一条语句就够。
	sqlUpsertLanternProgress = "INSERT INTO " + guildActivityProgressTable +
		" (guild_id, activity_id, period_key, progress_count, threshold_reached_ms, funds_granted, updated_ms)" +
		" VALUES (?, ?, ?, 1, 0, 0, ?) ON DUPLICATE KEY UPDATE progress_count = progress_count + 1, updated_ms = ?"

	// sqlEnsureReunionProgress:团圆只保证本档期进度行存在(progress_count 恒 0),锁存由随后的 sqlLatchActivityProgress 写。
	// 未达阈值时整事务回滚,新建的行随之消失(06 §6.41 I6)。参数形状与 sqlUpsertLanternProgress 相同。
	// 历练结算(grantTrialFunds)也用它保证当天的进度行存在:胜场计数要先加锁读回、判过每日上限才加,不能在 upsert 里盲加。
	sqlEnsureReunionProgress = "INSERT INTO " + guildActivityProgressTable +
		" (guild_id, activity_id, period_key, progress_count, threshold_reached_ms, funds_granted, updated_ms)" +
		" VALUES (?, ?, ?, 0, 0, 0, ?) ON DUPLICATE KEY UPDATE updated_ms = ?"

	// sqlLockActivityProgress:upsert 之后的主键点锁读回。阈值判定必须基于这一行:只看自己 +1 前的值,
	// 两个串行的点灯者会各自以为自己是"第 N 个"(06 §6.11.2 e/f 步)。
	sqlLockActivityProgress = "SELECT progress_count, threshold_reached_ms, funds_granted FROM " + guildActivityProgressTable +
		" WHERE guild_id = ? AND activity_id = ? AND period_key = ? FOR UPDATE"

	// sqlLatchActivityProgress:写锁存时刻与资金标记(值在 Go 里算好;只在本次首次达阈值或本次发资金时执行,值必变,RowsAffected 恒为 1)。
	sqlLatchActivityProgress = "UPDATE " + guildActivityProgressTable +
		" SET threshold_reached_ms = ?, funds_granted = ?, updated_ms = ? WHERE guild_id = ? AND activity_id = ? AND period_key = ?"

	// sqlSelectActivityProgress:视图 / 团圆预检的普通读(不加锁)。
	sqlSelectActivityProgress = "SELECT progress_count, threshold_reached_ms, funds_granted FROM " + guildActivityProgressTable +
		" WHERE guild_id = ? AND activity_id = ? AND period_key = ?"

	// sqlSelectActivityProgressKeysOfGuild:解散的候选普通读,按主键序返回,随后逐行 sqlDeleteActivityProgress。
	sqlSelectActivityProgressKeysOfGuild = "SELECT activity_id, period_key FROM " + guildActivityProgressTable +
		" WHERE guild_id = ? ORDER BY activity_id, period_key"

	// sqlDeleteActivityProgress:完整主键等值点删,不带复核条件(TiDB 走点写快路径,同 sqlDeleteApplication,不另加点锁)。
	sqlDeleteActivityProgress = "DELETE FROM " + guildActivityProgressTable +
		" WHERE guild_id = ? AND activity_id = ? AND period_key = ?"
)

// 视图读(06 §6.8 第 3 / 7 步)。counter_kind / stream / status / kind 一律绑生成常量,不写数字字面量(90 part2 §3 末行)。
const (
	// sqlSelectActivityUsage:本人某游戏日全部活动的已用次数,走主键 (player_id, counter_kind) 前缀。
	sqlSelectActivityUsage = `SELECT ref_id, used_count FROM guild_daily_counter WHERE player_id = ? AND counter_kind = ? AND period_key = ?`

	// sqlSelectPendingActivityRewards:7a,本人 GUILD_CREDIT 流上未决的活动发奖行,按 (纪元, seq) 升序(90 part2 §3 订正)。
	// 走 idx_guild_asset_op_2 的 (player_id, stream) 前缀;kind 不在索引里,前缀内过滤。LIMIT 由调用方给 MaxPending。
	sqlSelectPendingActivityRewards = "SELECT ref_id, last_reason FROM " + guildAssetOpTable +
		" WHERE player_id = ? AND stream = ? AND status = ? AND kind = ? ORDER BY stream_epoch ASC, seq ASC LIMIT ?"

	// sqlSelectRecentRejectedRewards:7b,本人 GUILD_CREDIT 流上最近几条被拒行(任意 kind),kind 与时间窗在 Go 里过滤。
	sqlSelectRecentRejectedRewards = "SELECT ref_id, kind, reason_tip_id, next_attempt_ms FROM " + guildAssetOpTable +
		" WHERE player_id = ? AND stream = ? AND status = ? ORDER BY stream_epoch DESC, seq DESC LIMIT ?"
)

// ActivityRepo 是帮会活动写事务与读查询的入口。它持有 *GuildRepo 以复用 inTx 与提交后缓存失效
// (90 part2 §2 第 1 条、Y-04),不另起连接池、不另写事务基座。
//
// 线程模型:构造后只读,可被多个请求 goroutine 共享。
type ActivityRepo struct {
	guilds *GuildRepo
	db     *sql.DB
	seq    assetop.SeqTables
}

// NewActivityRepo。guilds 为 nil 时报错:没有它就没有 inTx,也就没有统一的隔离级与重试语义。
func NewActivityRepo(guilds *GuildRepo) (*ActivityRepo, error) {
	if guilds == nil {
		return nil, errors.New("guild activity repo: nil GuildRepo")
	}
	seq, err := GuildSeqTables()
	if err != nil {
		return nil, fmt.Errorf("guild activity repo: %w", err)
	}
	return &ActivityRepo{guilds: guilds, db: guilds.db, seq: seq}, nil
}

// ── 写事务:点灯 / 团圆 ───────────────────────────────────────

// ActivityRewardOp 是一次参与要发的物品奖励(06 §6.9)。事务里插一行 ACTIVITY_REWARD 指令(GUILD_CREDIT 流、PENDING、
// 永不中止),提交后由 logic 按预算同步投递一次,其余交给重投循环;repo 不碰玩家背包。
type ActivityRewardOp struct {
	// OpID 同时作 correlation_id;事务前由 logic 发号(号段 guild_asset_op,无 snowflake 回退),非 0。
	// 1213 / 9007 整事务重跑时沿用同一个 OpID:上一轮已整体回滚,op 行不存在。
	OpID uint64

	// Payload = proto.Marshal(activity.BuildRewardBundle(row.reward_id)),非空。由 logic 序列化:
	// 同一个包还要原样交给同步投递,两处各序列化一次就可能不是同一份字节。
	Payload []byte

	// LeaseUntilMs / LeaseToken:插行即持租约,给提交后的同步投递用(与捐献 / 兑换相同,S4 §4.19);
	// 令牌非 0,租约晚于 NowMs。租约到期前重投循环不会领这一行。
	LeaseUntilMs, LeaseToken uint64
}

// ActivityTxInput 是点灯 / 团圆事务的全部输入。时间、令牌、奖励包全部由 logic 给(显式依赖),repo 不读墙钟。
type ActivityTxInput struct {
	PlayerID, GuildID uint64

	// Activity 是 logic 用 activity.PickForWrite 在同一个 now 下选出的配表行(只读,不得修改)。
	// 类型必须与调用的事务一致;在 NowMs 时刻必须是 Open,否则按调用方编程错误回内部错误。
	Activity *tablepb.GuildActivityTable

	// JoinMinHours 是 GuildRule.activity_join_min_hours;事务内按成员行 join_time_ms 复核(activity.JoinedLongEnough)。
	JoinMinHours uint32

	// NowMs 是本次请求唯一的"现在"(UTC 毫秒,> 0)。个人游戏日键 activity.DayKey 与帮会期键 activity.GuildPeriodKey
	// 都由它算:计数行清理不持 seq 行、只删 8 天前的周期(asset_store.go minCounterCleanupAge),它与本事务的 upsert
	// 不相遇的前提就是"周期键取自请求开头的时刻",调用方不得拿更早的时刻来算。
	NowMs uint64

	// Reward 为 nil = 本活动没有物品奖励(reward_id = 0 或奖励包合并后为空)。
	Reward *ActivityRewardOp

	// Fence 是合服闸门;zone 一律取本事务锁住的 guild.zone_id(90 part2 §2 第 10 条)。nil = 不设闸。
	Fence FenceFunc
}

// ActivityTxResult 是一次成功参与的结果。
type ActivityTxResult struct {
	// Enqueued:本次插了一行 ACTIVITY_REWARD 指令。提交后按预算同步投递一次,Seq / StreamEpoch 原样带给 scene。
	Enqueued bool

	Seq, StreamEpoch uint64

	// DayKey / GuildPeriodKey:本事务写计数行、进度行用的键。视图回读用同一对键,不必再算一遍。
	DayKey, GuildPeriodKey uint32

	// Progress:提交时进度行的样子(已含本次的 +1、锁存、资金标记)。
	Progress activity.Progress

	// ReachedNow:本次首次达到阈值(写了锁存),提交后推 ACTIVITY_CHANGED(06 §6.11.3:不达阈值不推);
	// FundsGranted:本次给帮会发了 guild_funds(本档期只会有一次为真)。
	ReachedNow, FundsGranted bool
}

// validate 在碰库之前拒掉畸形输入。这些都是调用方的编程错误(不是玩家可触发的拒绝),回内部错误。
// DailyLimit 为 0 尤其要拒:带上限 upsert 会永远判"达上限",把一个坏配表伪装成"今日已领"。
func (in ActivityTxInput) validate(typ uint32) error {
	row := in.Activity
	switch {
	case in.PlayerID == 0 || in.GuildID == 0:
		return fmt.Errorf("guild activity tx: ids must be non-zero (player=%d guild=%d)", in.PlayerID, in.GuildID)
	case row == nil:
		return errors.New("guild activity tx: nil GuildActivity row")
	case row.GetType() != typ:
		return fmt.Errorf("guild activity tx: GuildActivity[%d] type %d, want %d", row.GetId(), row.GetType(), typ)
	case row.GetId() == 0 || row.GetDailyLimit() == 0:
		return fmt.Errorf("guild activity tx: GuildActivity[%d] id and daily_limit (%d) must be non-zero", row.GetId(), row.GetDailyLimit())
	case in.NowMs == 0 || in.NowMs > math.MaxInt64:
		return fmt.Errorf("guild activity tx: GuildActivity[%d] now %d out of range", row.GetId(), in.NowMs)
	case activity.StateOf(row, in.NowMs) != activity.StateOpen:
		// PickForWrite 与本事务用的是同一个 now,走到这里说明调用方没过闸或传了另一个时刻。
		return fmt.Errorf("guild activity tx: GuildActivity[%d] not open at %d (call PickForWrite with the same now)", row.GetId(), in.NowMs)
	}
	if rw := in.Reward; rw != nil {
		switch {
		case rw.OpID == 0 || rw.LeaseToken == 0:
			return fmt.Errorf("guild activity tx: reward op id (%d) and lease token must be non-zero", rw.OpID)
		case rw.LeaseUntilMs <= in.NowMs:
			return fmt.Errorf("guild activity tx: reward op %d lease (%d) must be after now (%d)", rw.OpID, rw.LeaseUntilMs, in.NowMs)
		case len(rw.Payload) == 0:
			return fmt.Errorf("guild activity tx: reward op %d has empty payload", rw.OpID)
		}
	}
	return nil
}

// LightLanternTx 元宵灯会点灯事务(06 §6.11.2,按 §12.2 的锁序订正)。
//
// 事务 op=activity,取锁序列见文件头。在一个事务里:复核帮会等级 → 合服闸门 → 锁本人成员行、复核入帮时长 →
// [发物品:分 seq、插指令行] → 占今日次数 → 帮会本档期进度 +1 并加锁读回 → 本次恰好达到阈值则写锁存、
// 本档期资金未发且 guild_funds > 0 则发资金 → 加个人帮贡。
//
// 错误语义:ErrGuildGone / ErrNotGuildMember(logic 按 Y-01 调 VerifyPlayerGuildID 自愈后回 NotInGuild)/
// ErrGuildLevelTooLow / ErrZoneMerging / ErrActivityJoinTooRecent / ErrActivityAlreadyClaimed /
// 包了 assetop.ErrTooManyPending 的错误(errors.Is 可判,回 GuildAssetPending)/ ErrActivityPoison / ErrWriteConflict;
// 其余为内部错误。任何拒绝都整体回滚:次数、进度、资金、帮贡、指令行、seq 一起不动(06 §6.13 C1)。
// 同一玩家并发:第二个事务排在 G 行锁上,拿到后计数 upsert 判达上限 → ErrActivityAlreadyClaimed(C10);
// 同帮多人并发:阈值与资金在同一把 G 行锁下判定,资金本档期只发一次(C9)。
func (r *ActivityRepo) LightLanternTx(ctx context.Context, in ActivityTxInput) (ActivityTxResult, error) {
	return r.participate(ctx, in, activity.TypeLantern, false)
}

// ClaimReunionTx 中秋团圆领奖事务(06 §6.12.2,按 §12.2 的锁序订正)。
//
// observed 是 logic 在事务外用 BatchResolveStrict 数到的"入帮满 N 小时且在线"人数是否达到
// activity.ReunionThreshold(activity.ReunionObserved 的结果)。人数只用来开门、事务内不重数(C11)。
// 本档期已锁存时不看 observed;未锁存且 observed=false → ErrActivityThresholdNotReached(整体回滚,进度行也不留)。
// 其余步骤、错误语义与 LightLanternTx 相同。
func (r *ActivityRepo) ClaimReunionTx(ctx context.Context, in ActivityTxInput, observed bool) (ActivityTxResult, error) {
	return r.participate(ctx, in, activity.TypeReunion, observed)
}

// participate 是点灯与团圆共用的事务体,两者只在进度行那一步不同(灯会 +1、团圆只保证行存在)
// 与"是否达标"的判定函数不同(activity.LanternOutcome / activity.ReunionOutcome)。
// 语句顺序即取锁顺序,由 activity_repo_static_test.go(不带 build tag,普通 go test 必跑)的 TestActivityLockOrderInSource
// 按源码顺序钉住。
func (r *ActivityRepo) participate(ctx context.Context, in ActivityTxInput, typ uint32, observed bool) (ActivityTxResult, error) {
	if err := in.validate(typ); err != nil {
		return ActivityTxResult{}, err
	}
	row := in.Activity
	now := time.UnixMilli(int64(in.NowMs))
	dayKey := activity.DayKey(now)
	key := ProgressKey{ActivityID: row.GetId(), PeriodKey: activity.GuildPeriodKey(row, now)}
	const stream = assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT

	var out ActivityTxResult
	err := r.guilds.inTx(ctx, opActivity, func(ctx context.Context, tx *sql.Tx) error {
		// G:锁序位置 1。之后对 guild 行的资金写是再写已持有行,不再新增锁。
		var (
			level, zoneID uint32
			funds         uint64
		)
		err := tx.QueryRowContext(ctx, sqlLockActivityGuild, in.GuildID).Scan(&level, &zoneID, &funds)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrGuildGone
		}
		if err != nil {
			return fmt.Errorf("lock guild %d for activity %d: %w", in.GuildID, row.GetId(), err)
		}
		if level < row.GetMinGuildLevel() {
			return ErrGuildLevelTooLow
		}
		if err := checkFence(ctx, in.Fence, zoneID); err != nil {
			return err
		}

		// M:锁序位置 3。成员行锁也是下面建 seq 行的守卫(ensureSeqRowTx 的前置,C2)。
		var joinTimeMs, contributionTotal, contributionBalance uint64
		err = tx.QueryRowContext(ctx, sqlLockActivityMember, in.GuildID, in.PlayerID).
			Scan(&joinTimeMs, &contributionTotal, &contributionBalance)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotGuildMember
		}
		if err != nil {
			return fmt.Errorf("lock member %d of guild %d for activity %d: %w", in.PlayerID, in.GuildID, row.GetId(), err)
		}
		if !activity.JoinedLongEnough(joinTimeMs, in.NowMs, in.JoinMinHours) {
			return ErrActivityJoinTooRecent
		}
		// 溢出在任何写之前判掉:personal_contribution 有配表上限(activity/config.go),实际不可达;
		// 真出现就是确定性失败,重试也一样,不能让它变成"加穿了回绕成小数"。
		gain := row.GetPersonalContribution()
		if contributionTotal > math.MaxUint64-gain || contributionBalance > math.MaxUint64-gain {
			return fmt.Errorf("%w: contribution of member %d in guild %d plus %d", ErrActivityPoison, in.PlayerID, in.GuildID, gain)
		}

		// Q(+ O):锁序位置 5 / 6。有物品奖励时 AllocateSeq 锁 seq 行并插指令行;没有也要锁住同一行,
		// 作下面计数 upsert 的守卫(文件头第 3 条)。seq 行缺就在成员行锁之下建(C2)。
		if err := ensureSeqRowTx(ctx, tx, in.PlayerID, stream, in.NowMs); err != nil {
			return err
		}
		var alloc assetop.Alloc
		if in.Reward != nil {
			alloc, err = assetop.AllocateSeq(ctx, tx, r.seq, in.PlayerID, stream, assetop.DefaultLimits, in.NowMs)
			if err != nil {
				return err // ErrTooManyPending 已由 assetop 用 %w 包好,原样上抛(logic 回 GuildAssetPending,什么都没提交)
			}
			if err := insertAssetOp(ctx, tx, activityRewardRecord(in, alloc, dayKey)); err != nil {
				return err
			}
		} else {
			found, err := lockRowExists(ctx, tx, sqlLockSeqGuard, in.PlayerID, uint32(stream))
			if err != nil {
				return fmt.Errorf("lock %s guard (player=%d) for activity %d: %w", guildPlayerOpSeqTable, in.PlayerID, row.GetId(), err)
			}
			if !found {
				// 上一步刚保证过这一行存在;不在只可能是有人在事务外删了 seq 行 —— 守卫缺位时不写计数行(fail-closed)。
				return fmt.Errorf("%s row (player=%d stream=%d) vanished right after ensure", guildPlayerOpSeqTable, in.PlayerID, int32(stream))
			}
		}

		// C:锁序位置 7。带上限 upsert(X-13);RowsAffected 0 = 今日次数已满。
		limited, err := upsertCounterWithLimit(ctx, tx, in.PlayerID, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_ACTIVITY,
			row.GetId(), dayKey, 1, row.GetDailyLimit(), in.NowMs)
		if err != nil {
			return err
		}
		if limited {
			return ErrActivityAlreadyClaimed
		}

		// P:锁序位置 8。先 upsert 再主键点锁读回(文件头第 5 条)。
		progressUpsert := sqlUpsertLanternProgress
		if typ == activity.TypeReunion {
			progressUpsert = sqlEnsureReunionProgress
		}
		if _, err := tx.ExecContext(ctx, progressUpsert, in.GuildID, key.ActivityID, key.PeriodKey, in.NowMs, in.NowMs); err != nil {
			return fmt.Errorf("upsert %s (%d,%d,%d): %w", guildActivityProgressTable, in.GuildID, key.ActivityID, key.PeriodKey, err)
		}
		progress, found, err := scanActivityProgress(tx.QueryRowContext(ctx, sqlLockActivityProgress,
			in.GuildID, key.ActivityID, key.PeriodKey))
		if err != nil {
			return fmt.Errorf("lock %s (%d,%d,%d): %w", guildActivityProgressTable, in.GuildID, key.ActivityID, key.PeriodKey, err)
		}
		if !found {
			return fmt.Errorf("%s (%d,%d,%d) vanished right after upsert", guildActivityProgressTable, in.GuildID, key.ActivityID, key.PeriodKey)
		}

		var outcome activity.GuildOutcome
		if typ == activity.TypeLantern {
			outcome = activity.LanternOutcome(row, progress)
		} else {
			var ok bool
			if outcome, ok = activity.ReunionOutcome(row, progress, observed); !ok {
				return ErrActivityThresholdNotReached
			}
		}

		// 以下都是再写本事务已持有的行(G / P / M),不新增取锁位置。
		if outcome.GrantFunds {
			grant := row.GetGuildFunds()
			if funds > math.MaxUint64-grant {
				return fmt.Errorf("%w: funds of guild %d plus %d", ErrActivityPoison, in.GuildID, grant)
			}
			if err := execExactlyOneRow(ctx, tx, fmt.Sprintf("grant activity %d funds to guild %d", key.ActivityID, in.GuildID),
				sqlSetActivityFunds, funds+grant, in.GuildID); err != nil {
				return err
			}
			progress.FundsGranted = true
		}
		if outcome.ReachedNow {
			progress.ThresholdReachedMs = in.NowMs
		}
		if outcome.ReachedNow || outcome.GrantFunds {
			if err := execExactlyOneRow(ctx, tx, fmt.Sprintf("latch %s (%d,%d,%d)", guildActivityProgressTable, in.GuildID, key.ActivityID, key.PeriodKey),
				sqlLatchActivityProgress, progress.ThresholdReachedMs, fundsGrantedColumn(progress.FundsGranted), in.NowMs,
				in.GuildID, key.ActivityID, key.PeriodKey); err != nil {
				return err
			}
		}
		if gain > 0 {
			if err := execExactlyOneRow(ctx, tx, fmt.Sprintf("credit activity %d contribution to member %d in guild %d", key.ActivityID, in.PlayerID, in.GuildID),
				sqlSetActivityContribution, contributionTotal+gain, contributionBalance+gain, in.GuildID, in.PlayerID); err != nil {
				return err
			}
		}

		// 重试契约:结果只在成功返回前赋给外层变量(guild_manage_repo.go retryOnDeadlock)。
		out = ActivityTxResult{
			Enqueued:       in.Reward != nil,
			Seq:            alloc.Seq,
			StreamEpoch:    alloc.Epoch,
			DayKey:         dayKey,
			GuildPeriodKey: key.PeriodKey,
			Progress:       progress,
			ReachedNow:     outcome.ReachedNow,
			FundsGranted:   outcome.GrantFunds,
		}
		return nil
	})
	if err != nil {
		return ActivityTxResult{}, err
	}
	// 帮贡与资金都在帮会快照缓存里(GuildData.Funds、MemberData.Contribution*),提交后失效(Y-04)。
	r.guilds.invalidateAfterCommit(ctx, opActivity, in.GuildID)
	return out, nil
}

// activityRewardRecord 造点灯 / 团圆的一行活动发奖指令。next_attempt_ms = 租约到期时刻:
// 同步投递握着租约期间重投循环不碰它。列的取值规则见 activityRewardRow.record。
func activityRewardRecord(in ActivityTxInput, alloc assetop.Alloc, dayKey uint32) *pb.GuildAssetOpRecord {
	rw := in.Reward
	return activityRewardRow{
		OpID: rw.OpID, PlayerID: in.PlayerID, GuildID: in.GuildID,
		ActivityID: in.Activity.GetId(), PeriodKey: dayKey, Payload: rw.Payload,
		NextAttemptMs: rw.LeaseUntilMs, LeaseUntilMs: rw.LeaseUntilMs, LeaseToken: rw.LeaseToken,
		NowMs: in.NowMs,
	}.record(alloc)
}

// activityRewardRow 是一行活动发奖指令(ACTIVITY_REWARD)里随调用点变化的部分;点灯 / 团圆、历练结算、
// 待入队物品转换三处共用 record,指令行的形状只有这一个定义点。
type activityRewardRow struct {
	OpID, PlayerID, GuildID uint64
	// ActivityID 进 ref_id;PeriodKey 是本人游戏日键,只作审计。
	ActivityID, PeriodKey uint32
	Payload               []byte
	// NextAttemptMs:重投循环最早何时可以领这一行。插行即持租约的填租约到期时刻,不持租约的填 NowMs(立刻可领)。
	// LeaseUntilMs / LeaseToken 同为 0 = 不持租约。
	NextAttemptMs, LeaseUntilMs, LeaseToken uint64
	NowMs                                   uint64
}

// record 造一行活动发奖指令(06 §6.9 的列表):GUILD_CREDIT 流、PENDING。
//
// 刻意为 0 的列:deadline_ms(永不中止,物品属于玩家,背包满就保持 PENDING 等腾出空间,C6);
// contribution_delta / funds_delta(帮贡与资金已在入队事务里记完,Finalize 对 ACTIVITY_REWARD 只做 CAS、不做对侧账);
// ref_count(活动发奖不退任何计数,asset_store.go counterRefund 对本 kind 恒不退)。
func (row activityRewardRow) record(alloc assetop.Alloc) *pb.GuildAssetOpRecord {
	return &pb.GuildAssetOpRecord{
		OpId:          row.OpID,
		PlayerId:      row.PlayerID,
		Stream:        uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT),
		Seq:           alloc.Seq,
		GuildId:       row.GuildID,
		Kind:          pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_ACTIVITY_REWARD,
		Status:        pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING,
		NextAttemptMs: row.NextAttemptMs,
		DeadlineMs:    0,
		Payload:       row.Payload,
		RefId:         row.ActivityID,
		PeriodKey:     row.PeriodKey,
		CreatedMs:     row.NowMs,
		UpdatedMs:     row.NowMs,
		LeaseUntilMs:  row.LeaseUntilMs,
		LeaseToken:    row.LeaseToken,
		TxType:        uint32(rollbackpb.TransactionType_TX_GUILD_ACTIVITY_REWARD),
		StreamEpoch:   alloc.Epoch,
	}
}

// fundsGrantedColumn:funds_granted 列是 0/1 的 uint32。
func fundsGrantedColumn(granted bool) uint32 {
	if granted {
		return 1
	}
	return 0
}

// scanActivityProgress 扫一行进度(三列同 sqlLockActivityProgress / sqlSelectActivityProgress)。行不存在 → found=false。
func scanActivityProgress(row *sql.Row) (activity.Progress, bool, error) {
	var (
		progress activity.Progress
		granted  uint32
	)
	err := row.Scan(&progress.Count, &progress.ThresholdReachedMs, &granted)
	if errors.Is(err, sql.ErrNoRows) {
		return activity.Progress{}, false, nil
	}
	if err != nil {
		return activity.Progress{}, false, err
	}
	progress.FundsGranted = granted != 0
	return progress, true, nil
}

// ── 解散(X-14) ───────────────────────────────────────────────

// deleteGuildActivityProgress 删本帮全部活动进度行,由 DisbandGuild 在提前截止之后、删 guild 行之前调用。
//
// 前置:调用方已持有该帮 guild 行 FOR UPDATE,且锁序更靠前的表(S / M / A / O)都已做完 —— 本函数之后只剩删 guild 行。
//
// 普通读候选 → 主键升序逐行点删(§12.2 第 2 条),不写 `DELETE … WHERE guild_id = ?`:前缀范围删的锁集随执行计划变
// (MySQL 走 PRIMARY 前缀还是全扫、TiDB 语句末尾并行锁哪些 key),逐行完整主键等值删的锁集只由这里的循环决定。
// 候选集完整:P 行的插入者(点灯 / 团圆,B6b 的历练结算)都先锁同一帮的 guild 行,解散持有它直到提交;
// RC 下候选读发生在拿到 guild 行锁之后,看得见此前已提交的全部行。同一把锁下没人能并发删它们,
// 点删影响 0 行不会发生;真出现也只说明行已不在,不作错误。
// 行数量级(**未经测量,是已登记的风险**):v1 不清理旧期(06 §6.14),本帮的进度行只增不减 —— 常开开发行按游戏日各一行,
// 正式档期每届一行,B6b 的历练进度按游戏日各一行(正式环境也逐日增长),每帮每天至多 3 行,所以解散要删的行数随帮会
// 存在的天数线性增长(上限约每年千行量级),每行一次候选读项加一条点删往返。同一个 txBudgetDisband(guild_manage_repo.go)
// 还要容纳全员 S / M 的点锁与点删、申请删除、提前截止,"远在预算之内"在不清理旧期时没有依据。
// 上线前要在真库回归里测"满员帮 + N 天进度行"(N 至少取预期的最长帮会寿命)的解散 p99,再定预算或提前做 v1.1 旧期清理
// (按 period_key 删已不可能再被写的旧期,需给本表补 period_key 索引,见 guild_db.proto 本表注释)。
// 超预算的后果是解散整事务回滚、玩家看到"繁忙请重试"且重试也一样 —— 不丢数据,但帮会解散不掉。
func deleteGuildActivityProgress(ctx context.Context, tx *sql.Tx, guildID uint64) error {
	keys, err := readActivityProgressKeys(ctx, tx, guildID)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if _, err := tx.ExecContext(ctx, sqlDeleteActivityProgress, guildID, key.ActivityID, key.PeriodKey); err != nil {
			return fmt.Errorf("delete %s (%d,%d,%d) of disbanded guild: %w", guildActivityProgressTable, guildID, key.ActivityID, key.PeriodKey, err)
		}
	}
	return nil
}

// readActivityProgressKeys 普通读本帮全部进度行的主键尾 (activity_id, period_key),按主键升序,读完即关游标
// (同一个 *sql.Tx 只有一条连接,游标没关就发下一条语句会被驱动拒绝)。
func readActivityProgressKeys(ctx context.Context, tx *sql.Tx, guildID uint64) ([]ProgressKey, error) {
	rows, err := tx.QueryContext(ctx, sqlSelectActivityProgressKeysOfGuild, guildID)
	if err != nil {
		return nil, fmt.Errorf("read %s keys of guild %d: %w", guildActivityProgressTable, guildID, err)
	}
	defer rows.Close()

	var keys []ProgressKey
	for rows.Next() {
		var key ProgressKey
		if err := rows.Scan(&key.ActivityID, &key.PeriodKey); err != nil {
			return nil, fmt.Errorf("scan %s key of guild %d: %w", guildActivityProgressTable, guildID, err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s keys of guild %d: %w", guildActivityProgressTable, guildID, err)
	}
	return keys, nil
}

// ── 读(不加锁,1000ms 子预算;06 §6.8)──────────────────────

// ProgressKey 是进度行主键里帮会之后的两段:活动 id 与帮会期键(activity.GuildPeriodKey)。
type ProgressKey struct {
	ActivityID, PeriodKey uint32
}

// ActivityUsage 返回本人某游戏日各活动已参与次数:activity_id → used_count。没有计数行的活动不在 map 里(即 0 次)。
// 计数挂在玩家上、不分帮会:退帮换帮当天仍算已领(06 §6.12 防刷要点)。
func (r *ActivityRepo) ActivityUsage(ctx context.Context, playerID uint64, dayKey uint32) (map[uint32]uint32, error) {
	ctx, cancel := context.WithTimeout(ctx, activityReadBudget)
	defer cancel()
	rows, err := r.db.QueryContext(ctx, sqlSelectActivityUsage,
		playerID, int32(pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_ACTIVITY), dayKey)
	if err != nil {
		return nil, fmt.Errorf("read activity usage of player %d: %w", playerID, err)
	}
	defer rows.Close()

	out := make(map[uint32]uint32)
	for rows.Next() {
		var activityID, used uint32
		if err := rows.Scan(&activityID, &used); err != nil {
			return nil, fmt.Errorf("scan activity usage of player %d: %w", playerID, err)
		}
		out[activityID] = used
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate activity usage of player %d: %w", playerID, err)
	}
	return out, nil
}

// GuildProgress 读本帮若干期的进度(视图第 4 步、团圆预检第 9 步)。每个键一次完整主键普通读(至多 3 个键);
// 行不存在的键不在 map 里,调用方按零值(未达阈值、资金未发)处理。
func (r *ActivityRepo) GuildProgress(ctx context.Context, guildID uint64, keys []ProgressKey) (map[ProgressKey]activity.Progress, error) {
	ctx, cancel := context.WithTimeout(ctx, activityReadBudget)
	defer cancel()
	out := make(map[ProgressKey]activity.Progress, len(keys))
	for _, key := range keys {
		progress, found, err := scanActivityProgress(r.db.QueryRowContext(ctx, sqlSelectActivityProgress,
			guildID, key.ActivityID, key.PeriodKey))
		if err != nil {
			return nil, fmt.Errorf("read %s (%d,%d,%d): %w", guildActivityProgressTable, guildID, key.ActivityID, key.PeriodKey, err)
		}
		if found {
			out[key] = progress
		}
	}
	return out, nil
}

// RewardStatus 是本人某个活动的物品发放状态(06 §6.8 第 7 步 7a / 7b;视图的 my_pending_* 与 my_last_reward_reject_tip_id)。
// B6b 的历练待入队行(7c)由调用方另加到 PendingCount。
type RewardStatus struct {
	// PendingCount:仍在发放中的指令行数(PENDING)。
	PendingCount uint32

	// PendingReasonTipID:最早一条 PENDING 行的 last_reason(如 assetop.ReasonBagFull 背包满);0 = 正常排队。
	PendingReasonTipID uint32

	// LastRejectTipID:近 24h 最近一次永久拒绝(封禁 / 非法包)的原因;0 = 无。
	LastRejectTipID uint32
}

// RewardStatuses 返回本人各活动的物品发放状态:activity_id → RewardStatus。没有待发、也没有近期被拒的活动不在 map 里。
// nowMs 只用来判"近 24h",由调用方给(与视图同一个 now)。
func (r *ActivityRepo) RewardStatuses(ctx context.Context, playerID, nowMs uint64) (map[uint32]RewardStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, activityReadBudget)
	defer cancel()
	out := make(map[uint32]RewardStatus)
	if err := r.readPendingActivityRewards(ctx, playerID, out); err != nil {
		return nil, err
	}
	if err := r.readRecentRejectedRewards(ctx, playerID, nowMs, out); err != nil {
		return nil, err
	}
	return out, nil
}

// readPendingActivityRewards 是 7a:按 (纪元, seq) 升序数每个活动的未决行,原因取最早那一行的 last_reason。
// 一个玩家本纪元在本流上至多 MaxPending 行未决(AllocateSeq 的守卫),LIMIT 取同一个数。
func (r *ActivityRepo) readPendingActivityRewards(ctx context.Context, playerID uint64, out map[uint32]RewardStatus) error {
	rows, err := r.db.QueryContext(ctx, sqlSelectPendingActivityRewards,
		playerID, uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT), PendingStatus(),
		int32(pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_ACTIVITY_REWARD), int(assetop.DefaultLimits.MaxPending))
	if err != nil {
		return fmt.Errorf("read pending activity rewards of player %d: %w", playerID, err)
	}
	defer rows.Close()

	for rows.Next() {
		var activityID, lastReason uint32
		if err := rows.Scan(&activityID, &lastReason); err != nil {
			return fmt.Errorf("scan pending activity reward of player %d: %w", playerID, err)
		}
		status := out[activityID]
		if status.PendingCount == 0 {
			status.PendingReasonTipID = lastReason
		}
		status.PendingCount++
		out[activityID] = status
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate pending activity rewards of player %d: %w", playerID, err)
	}
	return nil
}

// readRecentRejectedRewards 是 7b:沿 uk 倒序取最近几条被拒行(任意 kind),在 Go 里只留活动发奖、且终结于近 24h 的,
// 每个活动取最新的一条。
func (r *ActivityRepo) readRecentRejectedRewards(ctx context.Context, playerID, nowMs uint64, out map[uint32]RewardStatus) error {
	rows, err := r.db.QueryContext(ctx, sqlSelectRecentRejectedRewards,
		playerID, uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT),
		int32(pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_REJECTED), recentRejectedRewardScan)
	if err != nil {
		return fmt.Errorf("read rejected activity rewards of player %d: %w", playerID, err)
	}
	defer rows.Close()

	// 窗口下沿先算好;now 比窗口还小(只可能是测试时钟)时整段都算"近 24h"。
	var since uint64
	if nowMs > rejectedRewardWindowMs {
		since = nowMs - rejectedRewardWindowMs
	}
	seen := make(map[uint32]bool)
	for rows.Next() {
		var (
			activityID, reasonTipID uint32
			kind                    int32
			finishedMs              uint64
		)
		if err := rows.Scan(&activityID, &kind, &reasonTipID, &finishedMs); err != nil {
			return fmt.Errorf("scan rejected activity reward of player %d: %w", playerID, err)
		}
		if pb.GuildAssetOpKind(kind) != pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_ACTIVITY_REWARD || finishedMs < since || seen[activityID] {
			continue
		}
		seen[activityID] = true
		status := out[activityID]
		status.LastRejectTipID = reasonTipID
		out[activityID] = status
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate rejected activity rewards of player %d: %w", playerID, err)
	}
	return nil
}

// ── 同道历练(B6b)──────────────────────────────────────────────
//
// 一局历练在 guild 侧的生命周期(06 §6.26 / §6.29–§6.33,取锁序列与"为什么不成环"见文件头第 7–11 条):
//
//	RegisterTrialBattle   开战成功后登记一行 STARTED(失败不致命:结算会补登记)
//	SettleTrialBattleTx   结果到达(Kafka 或巡检器从 SharedRedis 记录取回):发帮贡 / 物品 / 帮会资金,行进 SETTLED
//	MarkTrialBattlePoison 确定性失败(溢出、奖励包构建失败、结果过旧):行进 SETTLED/POISON,不发奖
//	ExpireTrialBattle     巡检器判定长时间无结果:STARTED → EXPIRED(迟到的结果仍可结算)
//	ConvertOwedReward     结算时未决指令已满者的物品,等窗口有空位再转成资产指令行
//
// 结果事件可能重复投递(battle 未销账就重发、Kafka 重放、巡检器与消费者同时处理):幂等闸门是 T 行的 SETTLED。
// SharedRedis 上的结果记录由 logic 在本文件的写成功之后销账(trial_result_record.go),repo 不碰 Redis。

// 历练两表表名,唯一事实源是 proto/guild/guild_db.proto 的 OptionTableName。
const (
	guildTrialBattleTable     = "guild_trial_battle"
	guildTrialRewardOwedTable = "guild_trial_reward_owed"
)

const (
	// maxTrialCandidates:一局的发奖候选上限。历练队伍至多 5 人(activity.ValidateTables 的 maxTrialTeamSize);
	// 取 16 是给配表留余量,同时给结算事务的大小封顶(每位得奖者约 10 条主键点语句;子预算是否够用未经测量,
	// 5 人满编的 p99 要在真库回归里量,见 92-handoff §12.2 的 B6b 落码修正)。
	maxTrialCandidates = 16

	// trialCounterCleanupMargin:结算写历史周期计数行时,相对计数行清理截止时刻留的余量(见 trialCounterPeriodWritable)。
	// 要盖住的是"本次结算的 NowMs"与"同时在跑的那一轮清理的墙钟"之差:事务子预算与重试是秒级,副本时钟偏差按 < 1s 设计
	// (gameday 包头),一小时绰绰有余。取大了只是让"过旧"的判定早一小时生效,而那个范围的结果本来就在 7 天保留期之外。
	trialCounterCleanupMargin = time.Hour
)

var (
	// ErrTrialInputInvalid:历练写事务的入参畸形 —— 调用方的编程错误,或结果事件的上下文残缺。碰库之前返回。
	// 它是**确定性**的:同一份输入重试多少次都一样。结算消费者不得把它当暂时性错误退避重试(消费是单协程串行的,
	// 会把那个 guild 副本名下全部分区的历练结果一起卡住),
	// 应按毒消息处理(06 §6.31:标记、销账、提交 offset)。其余非哨兵错误才是暂时性的。
	ErrTrialInputInvalid = errors.New("guild trial: invalid input")

	// errTrialAlreadySettled / errTrialForeignGuild / errTrialRowMissing / errOwedWindowFull / errOwedRowGone
	// 是事务闭包交给外层的"无改动结束"信号:闭包回它们 → 事务回滚(此前没有任何写)→ 导出方法翻成各自的结果枚举、error 为 nil。
	// 不导出:调用方按结果枚举分支,不按错误分支。
	errTrialAlreadySettled = errors.New("guild trial: battle already settled")
	errTrialForeignGuild   = errors.New("guild trial: battle row belongs to another guild")
	errTrialRowMissing     = errors.New("guild trial: battle row missing")
	errOwedWindowFull      = errors.New("guild trial: owed reward still blocked by pending window")
	errOwedRowGone         = errors.New("guild trial: owed reward row already converted")
)

// trialAfterCounterReadHook:结算读完各人计数、尚未占用时调用。生产恒为 nil;集成测试用它在这道缝里插一行计数,
// 复现"读到的计数已过期"(06 §6.41 I20)。与 txDeadlockObserved 同一惯例:包级可替换钩子,只许单测替换。
var trialAfterCounterReadHook func(ctx context.Context)

// G 行与成员 / 计数的历练专用语句。
const (
	// sqlLockTrialGuild:历练各写事务的第一把锁(锁序位置 1),同时是 T 行的守卫(文件头第 10 条)。
	// 取 zone_id 给合服闸门,取 funds 给资金溢出判定与新值计算(写回用 sqlSetActivityFunds)。
	sqlLockTrialGuild = "SELECT zone_id, funds FROM guild WHERE guild_id = ? FOR UPDATE"

	// sqlSelectActivityCounter:结算在持有 Q 之后读某人某游戏日的已用次数(普通读,完整主键等值)。
	sqlSelectActivityCounter = "SELECT used_count FROM guild_daily_counter WHERE player_id = ? AND counter_kind = ? AND ref_id = ? AND period_key = ?"

	// sqlSetTrialProgressCount:把当天已计资金胜场写成锁内算好的新值(完整主键等值,行已由 sqlLockActivityProgress 锁住)。
	sqlSetTrialProgressCount = "UPDATE " + guildActivityProgressTable +
		" SET progress_count = ?, updated_ms = ? WHERE guild_id = ? AND activity_id = ? AND period_key = ?"

	// 开战前的权威成员核对(06 §6.24 第 7 步 / §6.26 第 3 步):普通读,IN 列表在运行期拼接。不带锁定子句,
	// 走主键前缀还是 uk 都不加行锁;结论只用来拒绝开战,发奖时的成员资格由结算事务在锁内复核。
	sqlSelectTrialRosterMembersHead = "SELECT player_id, join_time_ms FROM guild_member WHERE guild_id = ? AND player_id IN ("
	sqlSelectTrialRosterMembersTail = ")"
)

// 对局表 T 的语句。锁定 / 写语句一律完整主键 battle_id 等值、不带复核条件(状态在锁内读回、在 Go 里判);
// 按 guild_id / state 找行的只有普通读。state / settle_result 一律绑生成常量(90 part2 §3 末行)。
const (
	trialBattleColumns = "battle_id, guild_id, activity_id, period_key, guild_period_key, initiator_player_id," +
		" state, settle_result, rewarded_count, created_ms, settled_ms"

	// sqlSelectTrialBattleGate:持有 G 之后的普通读,判"有没有登记行 / 属于哪个帮 / 结算了没有"。
	sqlSelectTrialBattleGate = "SELECT guild_id, state FROM " + guildTrialBattleTable + " WHERE battle_id = ?"

	// sqlSelectTrialBattle:整行普通读(活动视图判"我的对局是否仍在进行"、排障)。
	sqlSelectTrialBattle = "SELECT " + trialBattleColumns + " FROM " + guildTrialBattleTable + " WHERE battle_id = ?"

	// sqlLockTrialBattle:写前的主键点锁,只许是完整主键等值 + FOR UPDATE(TiDB 的 Point_Get 快路径:PRIMARY key → 行 key)。
	sqlLockTrialBattle = "SELECT state FROM " + guildTrialBattleTable + " WHERE battle_id = ? FOR UPDATE"

	// sqlInsertTrialBattle:普通 INSERT,不是 upsert。同帮的插入者都在 G 上串行、且先用普通读确认过缺行;
	// 撞键(1062)只可能来自带着别的 guild_id 的事件抢先插了同一 battle_id,那要报出来,不能被 ON DUPLICATE 吞掉。
	sqlInsertTrialBattle = "INSERT INTO " + guildTrialBattleTable + " (" + trialBattleColumns + ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"

	// sqlSetTrialBattleState:状态迁移(→ SETTLED / → EXPIRED)。state 在 idx_0 / idx_1 里,取锁顺序是"聚簇记录 → 两条二级项";
	// 没有任何语句经这两条二级索引加锁(按它们找行的都是普通读)。每次迁移 state 必变,RowsAffected 恒为 1。
	sqlSetTrialBattleState = "UPDATE " + guildTrialBattleTable +
		" SET state = ?, settle_result = ?, rewarded_count = ?, settled_ms = ? WHERE battle_id = ?"

	// sqlSelectOverdueTrialBattles:巡检候选(普通读,走 idx_1 (state, created_ms))。
	sqlSelectOverdueTrialBattles = "SELECT battle_id, guild_id, created_ms FROM " + guildTrialBattleTable +
		" WHERE state = ? AND created_ms < ? ORDER BY created_ms, battle_id LIMIT ?"

	// sqlSelectStartedTrialBattlesOfGuild:解散的候选普通读(走 idx_0 (guild_id, state);行数 = 本帮在途对局数,与历史行数无关)。
	sqlSelectStartedTrialBattlesOfGuild = "SELECT battle_id FROM " + guildTrialBattleTable +
		" WHERE guild_id = ? AND state = ? ORDER BY battle_id"

	// sqlDeleteTrialBattle:完整主键等值点删,不带复核条件(同 sqlDeleteActivityProgress)。
	sqlDeleteTrialBattle = "DELETE FROM " + guildTrialBattleTable + " WHERE battle_id = ?"
)

// 待入队物品表 W 的语句。
const (
	owedRewardColumns = "player_id, battle_id, guild_id, activity_id, period_key, payload, attempts, created_ms"

	// sqlInsertOwedReward:结算在全序末尾插新行;payload 可空列,显式写。
	sqlInsertOwedReward = "INSERT INTO " + guildTrialRewardOwedTable + " (" + owedRewardColumns + ") VALUES (?, ?, ?, ?, ?, ?, 0, ?)"

	// sqlSelectOwedRewardsPage:后台循环的翻页普通读,按 (created_ms, player_id, battle_id) 升序、键集游标。
	// 游标条件写成展开式而不是行比较 `(a, b, c) > (?, ?, ?)`:MySQL 对行比较不做范围优化。
	sqlSelectOwedRewardsPage = "SELECT " + owedRewardColumns + " FROM " + guildTrialRewardOwedTable +
		" WHERE created_ms > ? OR (created_ms = ? AND (player_id > ? OR (player_id = ? AND battle_id > ?)))" +
		" ORDER BY created_ms, player_id, battle_id LIMIT ?"

	// sqlDeleteOwedReward:完整主键等值点删,不带复核条件(TiDB 点写快路径)。影响 0 行 = 别的实例已转换。
	sqlDeleteOwedReward = "DELETE FROM " + guildTrialRewardOwedTable + " WHERE player_id = ? AND battle_id = ?"

	// sqlBumpOwedRewardAttempts:诊断计数,自动提交。attempts 不在任何索引里,变更集合只有行 key(文件头第 11 条)。
	sqlBumpOwedRewardAttempts = "UPDATE " + guildTrialRewardOwedTable + " SET attempts = attempts + 1 WHERE player_id = ? AND battle_id = ?"

	// sqlCountOwedRewardsByActivity:视图第 7c 步,本人各活动的待入队行数(主键 player_id 前缀)。
	sqlCountOwedRewardsByActivity = "SELECT activity_id, COUNT(*) FROM " + guildTrialRewardOwedTable + " WHERE player_id = ? GROUP BY activity_id"

	// sqlCountOwedRewardsCapped:积压量 gauge,数到上限为止(06 §6.32 第 3 步)。
	sqlCountOwedRewardsCapped = "SELECT COUNT(*) FROM (SELECT 1 FROM " + guildTrialRewardOwedTable + " LIMIT ?) AS capped"
)

// TrialBattleKey 是一局历练的身份与归属:开战时 guild 发给 match 的 BattleActivityContext,结果事件原样回显。
// 登记、结算、标记三处用同一份,保证补登记出来的行与开战时登记的行逐列相同。
type TrialBattleKey struct {
	// BattleID 是 match 发的对局 id;GuildID 是发起帮会(T 行的守卫按它取,插入后不变)。
	BattleID, GuildID uint64

	// ActivityID 是 GuildActivity.id;PeriodKey 是开战时的游戏日键(个人次数记在这一天);
	// GuildPeriodKey 是开战时的帮会期键(历练 = 游戏日键,每日计资金胜场上限记在这一期)。
	ActivityID, PeriodKey, GuildPeriodKey uint32

	InitiatorPlayerID uint64
}

// complete:登记一行所需的字段是否齐全。巡检器手里只有登记行的 (battle_id, guild_id) 时为假。
func (k TrialBattleKey) complete() bool {
	return k.BattleID != 0 && k.GuildID != 0 && k.ActivityID != 0 && k.PeriodKey != 0 && k.GuildPeriodKey != 0
}

func (k TrialBattleKey) validateIdentity() error {
	if k.BattleID == 0 || k.GuildID == 0 {
		return fmt.Errorf("%w: battle id (%d) and guild id (%d) must be non-zero", ErrTrialInputInvalid, k.BattleID, k.GuildID)
	}
	return nil
}

func (k TrialBattleKey) validateComplete() error {
	if !k.complete() {
		return fmt.Errorf("%w: battle %d of guild %d needs activity id (%d), period key (%d) and guild period key (%d)",
			ErrTrialInputInvalid, k.BattleID, k.GuildID, k.ActivityID, k.PeriodKey, k.GuildPeriodKey)
	}
	return nil
}

func validateTrialNow(nowMs uint64) error {
	if nowMs == 0 || nowMs > math.MaxInt64 {
		return fmt.Errorf("%w: now %d out of range", ErrTrialInputInvalid, nowMs)
	}
	return nil
}

// TrialBattleRow 是对局表一行的只读视图;枚举列已转成生成类型。
type TrialBattleRow struct {
	TrialBattleKey
	State                pb.GuildTrialBattleState
	Result               pb.GuildTrialSettleResult
	RewardedCount        uint32
	CreatedMs, SettledMs uint64
}

// trialGate 是持有 G 之后普通读到的登记行状态。exists=false = 没有登记行(登记失败过,或本帮在途行已被解散删掉)。
type trialGate struct {
	exists bool
	state  pb.GuildTrialBattleState
}

// lockTrialGuild 锁帮会行(锁序位置 1),返回行里的 zone 与资金。帮会不存在 → ErrGuildGone。
func lockTrialGuild(ctx context.Context, tx *sql.Tx, guildID uint64) (zoneID uint32, funds uint64, err error) {
	err = tx.QueryRowContext(ctx, sqlLockTrialGuild, guildID).Scan(&zoneID, &funds)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, ErrGuildGone
	}
	if err != nil {
		return 0, 0, fmt.Errorf("lock guild %d for trial: %w", guildID, err)
	}
	return zoneID, funds, nil
}

// readTrialGate 普通读登记行。前置:调用方已持有 battle.GuildID 的 G 行锁 —— 同帮的 T 写者都排在它后面,
// 这次读到的值在本事务提交前不会变(文件头第 10 条)。
// 行属于别的帮 → errTrialForeignGuild(不去锁别帮的行);已是 SETTLED → errTrialAlreadySettled。
func readTrialGate(ctx context.Context, tx *sql.Tx, battle TrialBattleKey) (trialGate, error) {
	var (
		guildID uint64
		state   int32
	)
	err := tx.QueryRowContext(ctx, sqlSelectTrialBattleGate, battle.BattleID).Scan(&guildID, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return trialGate{}, nil
	}
	if err != nil {
		return trialGate{}, fmt.Errorf("read %s %d: %w", guildTrialBattleTable, battle.BattleID, err)
	}
	if guildID != battle.GuildID {
		return trialGate{}, fmt.Errorf("%w: battle %d is registered to guild %d, caller says guild %d",
			errTrialForeignGuild, battle.BattleID, guildID, battle.GuildID)
	}
	if pb.GuildTrialBattleState(state) == pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_SETTLED {
		return trialGate{}, errTrialAlreadySettled
	}
	return trialGate{exists: true, state: pb.GuildTrialBattleState(state)}, nil
}

// writeTrialBattleState 把对局行写成 to(锁序位置 9)。gate 是同一事务里 readTrialGate 的结果:
//   - 缺行:插入一行(createdMs 作登记时刻);
//   - 有行:主键点锁读回 → 与 gate 复核 → 完整主键点改。
//
// 进 SETTLED / EXPIRED 时 settled_ms = nowMs;登记(STARTED)时为 0。
// 点锁读回的状态与 gate 不同,说明"T 的写者都先持 G"这条不变量被破坏了(有路径不锁 G 就改了这一行):
// 不猜、不覆盖,回内部错误整体回滚(fail-closed)。撞键同理:同帮没有第二个插入者。
func writeTrialBattleState(ctx context.Context, tx *sql.Tx, battle TrialBattleKey, gate trialGate,
	to pb.GuildTrialBattleState, result pb.GuildTrialSettleResult, rewardedCount uint32, createdMs, nowMs uint64) error {
	var settledMs uint64
	if to != pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_STARTED {
		settledMs = nowMs
	}
	if !gate.exists {
		if _, err := tx.ExecContext(ctx, sqlInsertTrialBattle,
			battle.BattleID, battle.GuildID, battle.ActivityID, battle.PeriodKey, battle.GuildPeriodKey, battle.InitiatorPlayerID,
			int32(to), int32(result), rewardedCount, createdMs, settledMs); err != nil {
			return fmt.Errorf("insert %s %d (guild %d): %w", guildTrialBattleTable, battle.BattleID, battle.GuildID, err)
		}
		return nil
	}
	var state int32
	err := tx.QueryRowContext(ctx, sqlLockTrialBattle, battle.BattleID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s %d vanished under the guild %d row lock", guildTrialBattleTable, battle.BattleID, battle.GuildID)
	}
	if err != nil {
		return fmt.Errorf("lock %s %d: %w", guildTrialBattleTable, battle.BattleID, err)
	}
	if pb.GuildTrialBattleState(state) != gate.state {
		return fmt.Errorf("%s %d changed state %d -> %d under the guild %d row lock",
			guildTrialBattleTable, battle.BattleID, int32(gate.state), state, battle.GuildID)
	}
	return execExactlyOneRow(ctx, tx, fmt.Sprintf("move %s %d to state %d", guildTrialBattleTable, battle.BattleID, int32(to)),
		sqlSetTrialBattleState, int32(to), int32(result), rewardedCount, settledMs, battle.BattleID)
}

// ── 登记 ─────────────────────────────────────────────────────

// RegisterTrialBattle 在开战成功(match 已返回 battle_id)之后登记一行 STARTED(06 §6.26 第 8 步)。
//
// 事务 op=trial_register:G FOR UPDATE → T 普通读 → 缺行才插。**不是**设计正文的自动提交 upsert:
// T 的写者必须先持 G(文件头第 10 条),登记若不守,它就成了能与结算 / 标记同时插同一 battle_id 的无守卫插入者。
//
// 幂等:行已存在(重复调用,或结果先到、结算已补登记乃至已结算)→ 什么都不改,返回 nil。
// 错误语义:ErrTrialInputInvalid(字段不全)/ ErrGuildGone(帮会刚被解散:不登记,结果到达时回 GuildGone)/
// ErrWriteConflict;登记行属于别的帮或其余情况为内部错误。
// 调用方(launchTrial)对任何错误都只打 ERROR 并计数、不向玩家报失败:战斗已经开始,结算会补登记(06 §6.27 L4)。
func (r *ActivityRepo) RegisterTrialBattle(ctx context.Context, battle TrialBattleKey, nowMs uint64) error {
	if err := battle.validateComplete(); err != nil {
		return err
	}
	if err := validateTrialNow(nowMs); err != nil {
		return err
	}
	err := r.guilds.inTx(ctx, opTrialRegister, func(ctx context.Context, tx *sql.Tx) error {
		if _, _, err := lockTrialGuild(ctx, tx, battle.GuildID); err != nil {
			return err
		}
		gate, err := readTrialGate(ctx, tx, battle)
		if err != nil {
			return err
		}
		if gate.exists {
			return nil
		}
		return writeTrialBattleState(ctx, tx, battle, gate, pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_STARTED,
			pb.GuildTrialSettleResult_GUILD_TRIAL_SETTLE_RESULT_UNSPECIFIED, 0, nowMs, nowMs)
	})
	if errors.Is(err, errTrialAlreadySettled) {
		return nil
	}
	return err
}

// ── 结算 ─────────────────────────────────────────────────────

// TrialRewardOp 是结算给某位候选人预备的物品指令参数(事务前由 logic 发号、生成令牌)。
type TrialRewardOp struct {
	PlayerID uint64
	// OpID 同时作 correlation_id,非 0。未用上的号(该人最终不得奖 / 进了待入队名单)就浪费掉,号段只进不退。
	OpID uint64
	// LeaseToken 非 0:插行即持租约,给提交后的同步投递用。
	LeaseToken uint64
}

// TrialReward 是本局的物品奖励(同一份包发给每位得奖者)。
type TrialReward struct {
	// Payload = proto.Marshal(activity.BuildRewardBundle(row.reward_id)),非空;待入队行存的也是这份字节。
	Payload []byte
	// LeaseUntilMs:各指令行共用的租约到期时刻,晚于 NowMs。
	LeaseUntilMs uint64
	// Ops:**每位候选人恰好一项**(PlayerID 不重复)。候选人里谁最终得奖要到事务里才知道,所以按候选人全量预备。
	Ops []TrialRewardOp
}

// TrialSettleInput 是结算事务的全部输入。时间、指令号、令牌、奖励包都由 logic 给(显式依赖),repo 不读墙钟。
type TrialSettleInput struct {
	// Battle 取自结果事件的 activity_context(字段齐全,否则 ErrTrialInputInvalid)。
	Battle TrialBattleKey

	// Activity 是 Battle.ActivityID 对应的配表行,类型必须是历练;nil = 配表缺行或类型不符 →
	// 结论记 CONFIG_MISSING、不发奖(06 §6.29 第 2 步)。不要求此刻仍在档期内:对局是开战时合法发起的。
	Activity *tablepb.GuildActivityTable

	// Win:己方(team 0)获胜。只有 Win 且 Activity 非 nil 才发奖。
	Win bool

	// Candidates:发奖候选 = team 0 的玩家 − 逃跑者(用户决策 U2:**阵亡者也在内**)。顺序任意、可重复,repo 去重升序;
	// 不得含 0,至多 maxTrialCandidates 人。
	Candidates []uint64

	// Reward 为 nil = 本活动没有物品奖励。不发奖的局(负 / 平 / 配表缺行)忽略它。
	Reward *TrialReward

	// FinishedAtMs 是结果事件的 finished_at_ms,只在补登记时作 created_ms;0 则用 NowMs。
	FinishedAtMs uint64

	// NowMs 是**本次调用开始时**的墙钟(UTC 毫秒)。每次重试都要重取,不得沿用事件里的时刻:
	// "结果是否过旧"(trialCounterPeriodWritable)按它判。
	NowMs uint64

	// Fence 是合服闸门,zone 取事务内锁住的 guild.zone_id。nil = 不设闸。拒绝时回 ErrZoneMerging,
	// 消费者应按暂时性错误退避重试(合服结束后照常结算),不是毒消息。
	Fence FenceFunc
}

// TrialSettleStatus 是结算事务的去向。零值无效。
type TrialSettleStatus uint8

const (
	// TrialSettleSettled:本次把这一局结算成 SETTLED(已提交)。
	TrialSettleSettled TrialSettleStatus = iota + 1
	// TrialSettleDuplicate:这一局此前已是 SETTLED,什么都没改。
	TrialSettleDuplicate
	// TrialSettleGuildGone:帮会不存在(已解散),什么都没写。
	TrialSettleGuildGone
	// TrialSettleContextMismatch:登记行属于别的帮,什么都没改;调用方打 ERROR。
	TrialSettleContextMismatch
)

// TrialEnqueuedReward 是本次结算插下的一行物品指令,提交后由 logic 按预算同步投递一次。
type TrialEnqueuedReward struct {
	PlayerID, OpID, Seq, StreamEpoch, LeaseToken uint64
}

// TrialSettleResult 是结算结果。除 Status 外的字段只在 Status == TrialSettleSettled 时有意义。
// 四种 Status 都意味着"这一局在 guild 侧已有定论",调用方都应销账(删 SharedRedis 结果记录)。
type TrialSettleResult struct {
	Status TrialSettleStatus

	// Result:写进对局行的结论(WIN / LOSS / CONFIG_MISSING)。
	Result pb.GuildTrialSettleResult

	// Rewarded:得帮贡、占了当日次数的人(升序)。候选人里结算时已不在帮、或当日次数已满的不在其中。
	Rewarded []uint64

	// Enqueued:已插指令行的物品(Rewarded 的子集);Owed:未决指令已满、物品转存待入队表的人(Rewarded 的子集,升序)。
	Enqueued []TrialEnqueuedReward
	Owed     []uint64

	// FundsGranted:本局计入了帮会资金(当天计资金胜场未满);CountedWins:提交后当天已计资金胜场(没碰进度行时为 0)。
	FundsGranted bool
	CountedWins  uint32
}

// trialSettlePlan 是入参校验后的规范化结果(纯数据,不碰库)。
type trialSettlePlan struct {
	candidates []uint64 // 去重、升序
	result     pb.GuildTrialSettleResult
	grant      bool                     // 本局要发奖:Win、配表行在、候选非空
	ops        map[uint64]TrialRewardOp // grant 且有物品时非 nil:候选人 → 指令参数
}

// plan 在碰库之前校验并规范化输入。任何不合规都包 ErrTrialInputInvalid(确定性失败,见该哨兵的注释)。
// DailyLimit 为 0 要拒:带上限 upsert 会永远判"达上限",把坏配表伪装成"人人次数已满"。
func (in TrialSettleInput) plan() (trialSettlePlan, error) {
	if err := in.Battle.validateComplete(); err != nil {
		return trialSettlePlan{}, err
	}
	if err := validateTrialNow(in.NowMs); err != nil {
		return trialSettlePlan{}, err
	}
	invalid := func(format string, args ...any) (trialSettlePlan, error) {
		return trialSettlePlan{}, fmt.Errorf("%w: battle %d: %s", ErrTrialInputInvalid, in.Battle.BattleID, fmt.Sprintf(format, args...))
	}
	row := in.Activity
	if row != nil {
		switch {
		case row.GetType() != activity.TypeTrial:
			return invalid("GuildActivity[%d] type %d is not a trial (pass nil for a config miss)", row.GetId(), row.GetType())
		case row.GetId() != in.Battle.ActivityID:
			return invalid("GuildActivity[%d] does not match activity id %d", row.GetId(), in.Battle.ActivityID)
		case row.GetDailyLimit() == 0:
			return invalid("GuildActivity[%d] daily_limit must be non-zero", row.GetId())
		}
	}
	if len(in.Candidates) > maxTrialCandidates {
		return invalid("%d candidates exceed the cap %d", len(in.Candidates), maxTrialCandidates)
	}
	candidates := slices.Clone(in.Candidates)
	slices.Sort(candidates)
	candidates = slices.Compact(candidates)
	if len(candidates) > 0 && candidates[0] == 0 {
		return invalid("candidate player id 0")
	}

	plan := trialSettlePlan{candidates: candidates, result: pb.GuildTrialSettleResult_GUILD_TRIAL_SETTLE_RESULT_LOSS}
	switch {
	case row == nil:
		plan.result = pb.GuildTrialSettleResult_GUILD_TRIAL_SETTLE_RESULT_CONFIG_MISSING
	case in.Win:
		plan.result = pb.GuildTrialSettleResult_GUILD_TRIAL_SETTLE_RESULT_WIN
	}
	plan.grant = in.Win && row != nil && len(candidates) > 0
	if !plan.grant || in.Reward == nil {
		return plan, nil
	}

	rw := in.Reward
	if len(rw.Payload) == 0 || rw.LeaseUntilMs <= in.NowMs {
		return invalid("reward payload must be non-empty and lease (%d) after now (%d)", rw.LeaseUntilMs, in.NowMs)
	}
	plan.ops = make(map[uint64]TrialRewardOp, len(rw.Ops))
	for _, op := range rw.Ops {
		if op.PlayerID == 0 || op.OpID == 0 || op.LeaseToken == 0 {
			return invalid("reward op needs non-zero player (%d), op id (%d) and lease token", op.PlayerID, op.OpID)
		}
		if _, dup := plan.ops[op.PlayerID]; dup {
			return invalid("two reward ops for player %d", op.PlayerID)
		}
		plan.ops[op.PlayerID] = op
	}
	for _, playerID := range candidates {
		if _, ok := plan.ops[playerID]; !ok {
			return invalid("candidate %d has no reward op", playerID)
		}
	}
	return plan, nil
}

// trialCounterPeriodWritable:结算要写的个人周期键(开战时的游戏日)是否还在计数行清理永远碰不到的范围里。
//
// 计数行清理(asset_store.go)不持 seq 行守卫,只删 period_key ≤ DayKey(清理时刻 − max(保留期, minCounterCleanupAge)) 的行;
// 它与带上限 upsert 永不相遇,靠的是 upsert 只写"当前周期"。结算写的是历史周期,要自己保证:
// periodKey > DayKey(nowMs − minCounterCleanupAge + trialCounterCleanupMargin)。只要同时在跑的清理的墙钟不比 nowMs 晚出一个余量,
// 它的截止键就 ≤ 右边那个键 < periodKey,两者行集合不相交(DayKey 随时间单调不减)。
//
// 判假意味着这局的结果在 guild 这里至少滞留了约 7 天(battle 的结果记录与 Kafka 都只留 7 天,06 §6.34 T11):
// 当日计数行可能已被清掉,"每日次数"无从守起,也不能冒与清理在同一行上相遇的险 —— 不发奖,按确定性失败处理。
func trialCounterPeriodWritable(periodKey uint32, nowMs uint64) bool {
	oldest := time.UnixMilli(int64(nowMs)).Add(-(minCounterCleanupAge - trialCounterCleanupMargin))
	return periodKey > activity.DayKey(oldest)
}

// SettleTrialBattleTx 结算一局历练(06 §6.29 / §6.30,取锁序列按 §12.2 订正,见文件头)。
//
// 一个事务(op=trial_settle)里:锁帮会行 → 合服闸门 → 判重复 / 判归属 → [发奖的局:锁候选人成员行,在帮者为合格者 →
// 锁合格者的 seq 行 → 读各人该游戏日次数,未满者得奖 → 逐人分 seq、插物品指令行(未决已满者改记待入队)→
// 逐人占次数 → 当天计资金胜场未满则帮会资金 +guild_funds、胜场 +1 → 逐人加帮贡] → 对局行进 SETTLED → 插待入队物品行。
//
// 不复核帮会等级与入帮时长:那是开战前的门槛(建房、开战时已判),对局打完之后不再追究。
// 发奖名单 = 候选人 ∩ 结算时仍在本帮 ∩ 该游戏日次数未满(06 §6.49 #10,U2:候选含阵亡者)。
//
// 返回:Status 四种去向之一、error 为 nil,都表示这一局已有定论,调用方销账;
// 错误语义:
//   - ErrTrialInputInvalid(碰库之前)/ ErrActivityPoison(帮贡或资金溢出、结果过旧):**确定性失败**,整体回滚,
//     调用方走 MarkTrialBattlePoison 并销账,不重试;
//   - ErrZoneMerging / ErrWriteConflict(含 errRetryTx、1213 / 9007 重跑耗尽、1205、子预算到期)/ 其余错误:暂时性,
//     调用方退避后带着**新的 NowMs** 重调;沿用同一批 OpID 是安全的(上一轮已整体回滚)。
//
// 幂等与并发:同一帮的结算在 G 行锁上串行;重复事件(重发、重放、巡检器与消费者同时处理)后到者读到 SETTLED,
// 回 TrialSettleDuplicate,不做任何写(06 §6.34 T4 / T5)。
func (r *ActivityRepo) SettleTrialBattleTx(ctx context.Context, in TrialSettleInput) (TrialSettleResult, error) {
	plan, err := in.plan()
	if err != nil {
		return TrialSettleResult{}, err
	}

	var out TrialSettleResult
	err = r.guilds.inTx(ctx, opTrialSettle, func(ctx context.Context, tx *sql.Tx) error {
		res, err := r.settleTrialBattle(ctx, tx, in, plan)
		if err != nil {
			return err
		}
		// 重试契约:结果只在成功返回前赋给外层变量(guild_manage_repo.go retryOnDeadlock)。
		out = res
		return nil
	})
	switch {
	case err == nil:
	case errors.Is(err, ErrGuildGone):
		return TrialSettleResult{Status: TrialSettleGuildGone}, nil
	case errors.Is(err, errTrialAlreadySettled):
		return TrialSettleResult{Status: TrialSettleDuplicate}, nil
	case errors.Is(err, errTrialForeignGuild):
		return TrialSettleResult{Status: TrialSettleContextMismatch}, nil
	default:
		return TrialSettleResult{}, err
	}
	// 帮贡与资金都在帮会快照缓存里,提交后失效(Y-04)。没人得奖的局(负 / 平 / 配表缺行)没动缓存里的任何东西。
	if len(out.Rewarded) > 0 {
		r.guilds.invalidateAfterCommit(ctx, opTrialSettle, in.Battle.GuildID)
	}
	return out, nil
}

// trialMember 是结算锁住的一行成员:帮贡两列是锁内读到的旧值,新值在 Go 里算。
type trialMember struct {
	playerID                               uint64
	contributionTotal, contributionBalance uint64
}

// settleTrialBattle 是结算的事务体。语句顺序即取锁顺序,由 activity_repo_static_test.go 的 TestActivityLockOrderInSource
// 按源码顺序钉住:调换下面任何两步之前,先回文件头把"为什么不成环"重推一遍。
func (r *ActivityRepo) settleTrialBattle(ctx context.Context, tx *sql.Tx, in TrialSettleInput, plan trialSettlePlan) (TrialSettleResult, error) {
	battle := in.Battle

	// G:锁序位置 1,也是 T 行的守卫。之后对 guild 行的资金写是再写已持有行。
	zoneID, funds, err := lockTrialGuild(ctx, tx, battle.GuildID)
	if err != nil {
		return TrialSettleResult{}, err
	}
	if err := checkFence(ctx, in.Fence, zoneID); err != nil {
		return TrialSettleResult{}, err
	}
	// T 的普通读:重复事件与归属不符在这里就结束,不必先把奖发一遍再回滚。
	gate, err := readTrialGate(ctx, tx, battle)
	if err != nil {
		return TrialSettleResult{}, err
	}

	res := TrialSettleResult{Status: TrialSettleSettled, Result: plan.result}
	if plan.grant {
		if !trialCounterPeriodWritable(battle.PeriodKey, in.NowMs) {
			return TrialSettleResult{}, fmt.Errorf("%w: result of battle %d (period key %d) is too old to settle at %d",
				ErrActivityPoison, battle.BattleID, battle.PeriodKey, in.NowMs)
		}
		row := in.Activity

		// M:锁序位置 3。成员行锁也是下面建 seq 行的守卫(ensureSeqRowTx 的前置,C2)。
		members, err := lockTrialMembers(ctx, tx, battle.GuildID, plan.candidates)
		if err != nil {
			return TrialSettleResult{}, err
		}
		// Q:锁序位置 5。碰 O 之前锁完全部合格者的 seq 行(文件头第 7 条)。
		if err := lockTrialSeqGuards(ctx, tx, members, in.NowMs); err != nil {
			return TrialSettleResult{}, err
		}
		// C 的普通读:持有 Q 之后读,定得奖名单(文件头第 8 条)。
		rewarded, err := readTrialRewarded(ctx, tx, members, battle, row.GetDailyLimit())
		if err != nil {
			return TrialSettleResult{}, err
		}
		if trialAfterCounterReadHook != nil {
			trialAfterCounterReadHook(ctx)
		}
		// 溢出在任何写之前判掉:personal_contribution 有配表上限,实际不可达;真出现就是确定性失败。
		gain := row.GetPersonalContribution()
		for _, m := range rewarded {
			if m.contributionTotal > math.MaxUint64-gain || m.contributionBalance > math.MaxUint64-gain {
				return TrialSettleResult{}, fmt.Errorf("%w: contribution of member %d in guild %d plus %d (battle %d)",
					ErrActivityPoison, m.playerID, battle.GuildID, gain, battle.BattleID)
			}
		}

		// O:锁序位置 6。只插新行;未决已满者不插,记入待入队名单。
		if plan.ops != nil {
			res.Enqueued, res.Owed, err = r.enqueueTrialRewards(ctx, tx, in, plan.ops, rewarded)
			if err != nil {
				return TrialSettleResult{}, err
			}
		}
		// C:锁序位置 7。
		if err := occupyTrialCounters(ctx, tx, rewarded, battle, row.GetDailyLimit(), in.NowMs); err != nil {
			return TrialSettleResult{}, err
		}
		if len(rewarded) > 0 {
			// P:锁序位置 8;随后对 G / P / M 的写都是再写已持有的行。
			res.FundsGranted, res.CountedWins, err = grantTrialFunds(ctx, tx, battle, row, funds, in.NowMs)
			if err != nil {
				return TrialSettleResult{}, err
			}
			if err := creditTrialContribution(ctx, tx, battle, rewarded, gain); err != nil {
				return TrialSettleResult{}, err
			}
		}
		res.Rewarded = make([]uint64, 0, len(rewarded))
		for _, m := range rewarded {
			res.Rewarded = append(res.Rewarded, m.playerID)
		}
	}

	// T:锁序位置 9。补登记的行以对局结束时刻作登记时刻(06 §6.30)。
	createdMs := in.FinishedAtMs
	if createdMs == 0 {
		createdMs = in.NowMs
	}
	if err := writeTrialBattleState(ctx, tx, battle, gate, pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_SETTLED,
		plan.result, uint32(len(res.Rewarded)), createdMs, in.NowMs); err != nil {
		return TrialSettleResult{}, err
	}
	// W:锁序位置 10。
	if len(res.Owed) > 0 {
		if err := insertOwedRewards(ctx, tx, battle, res.Owed, in.Reward.Payload, in.NowMs); err != nil {
			return TrialSettleResult{}, err
		}
	}
	return res, nil
}

// lockTrialMembers 按 player_id 升序逐个点锁候选人在本帮的成员行,返回锁到的人(合格者)。
// candidates 已升序去重。锁不到行 = 结算时已不在本帮(战后退帮 / 被踢),没有奖励(06 §6.30 b 步)。
func lockTrialMembers(ctx context.Context, tx *sql.Tx, guildID uint64, candidates []uint64) ([]trialMember, error) {
	members := make([]trialMember, 0, len(candidates))
	for _, playerID := range candidates {
		var joinTimeMs uint64
		m := trialMember{playerID: playerID}
		err := tx.QueryRowContext(ctx, sqlLockActivityMember, guildID, playerID).
			Scan(&joinTimeMs, &m.contributionTotal, &m.contributionBalance)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("lock member %d of guild %d for trial settle: %w", playerID, guildID, err)
		}
		members = append(members, m)
	}
	return members, nil
}

// lockTrialSeqGuards 按 player_id 升序锁住每位合格者 GUILD_CREDIT 流的 seq 行,缺行先在事务内建。
// 前置:各人的成员行已由 lockTrialMembers 锁住(seq 行建行者的守卫,C2)。
// 不管这个人最后得不得奖、有没有物品都锁:它是计数行 upsert 的守卫(C6),得奖名单也要在持有它之后读才稳定。
func lockTrialSeqGuards(ctx context.Context, tx *sql.Tx, members []trialMember, nowMs uint64) error {
	const stream = assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT
	for _, m := range members {
		if err := ensureSeqRowTx(ctx, tx, m.playerID, stream, nowMs); err != nil {
			return err
		}
		found, err := lockRowExists(ctx, tx, sqlLockSeqGuard, m.playerID, uint32(stream))
		if err != nil {
			return fmt.Errorf("lock %s guard (player=%d) for trial settle: %w", guildPlayerOpSeqTable, m.playerID, err)
		}
		if !found {
			// 上一步刚保证过这一行存在;不在只可能是有人在事务外删了 seq 行 —— 守卫缺位时不写计数行(fail-closed)。
			return fmt.Errorf("%s row (player=%d stream=%d) vanished right after ensure", guildPlayerOpSeqTable, m.playerID, int32(stream))
		}
	}
	return nil
}

// readTrialRewarded 读每位合格者在开战那个游戏日的已用次数,返回次数未满的人(得奖者),保持升序。
// 前置:各人的 seq 行已锁住,读到的值在提交前不会被守规矩的写者改掉(文件头第 8 条)。
func readTrialRewarded(ctx context.Context, tx *sql.Tx, members []trialMember, battle TrialBattleKey, dailyLimit uint32) ([]trialMember, error) {
	rewarded := make([]trialMember, 0, len(members))
	for _, m := range members {
		var used uint32
		err := tx.QueryRowContext(ctx, sqlSelectActivityCounter, m.playerID,
			int32(pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_ACTIVITY), battle.ActivityID, battle.PeriodKey).Scan(&used)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("read activity %d usage of player %d (period %d): %w", battle.ActivityID, m.playerID, battle.PeriodKey, err)
		}
		if used < dailyLimit {
			rewarded = append(rewarded, m)
		}
	}
	return rewarded, nil
}

// enqueueTrialRewards 按 player_id 升序给每位得奖者分 seq、插一行物品指令。
// 某人本流未决指令已满(assetop.ErrTooManyPending)时不插行、不前进 seq,记入 owed —— 物品随后写进待入队表,绝不跳过(06 §6.9)。
// AllocateSeq 对 seq 行的 FOR UPDATE 落在本事务已持有的行上,不是新的取锁位置。
func (r *ActivityRepo) enqueueTrialRewards(ctx context.Context, tx *sql.Tx, in TrialSettleInput,
	ops map[uint64]TrialRewardOp, rewarded []trialMember) (enqueued []TrialEnqueuedReward, owed []uint64, err error) {
	const stream = assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT
	for _, m := range rewarded {
		op := ops[m.playerID]
		alloc, err := assetop.AllocateSeq(ctx, tx, r.seq, m.playerID, stream, assetop.DefaultLimits, in.NowMs)
		if errors.Is(err, assetop.ErrTooManyPending) {
			owed = append(owed, m.playerID)
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		record := activityRewardRow{
			OpID: op.OpID, PlayerID: m.playerID, GuildID: in.Battle.GuildID,
			ActivityID: in.Battle.ActivityID, PeriodKey: in.Battle.PeriodKey, Payload: in.Reward.Payload,
			NextAttemptMs: in.Reward.LeaseUntilMs, LeaseUntilMs: in.Reward.LeaseUntilMs, LeaseToken: op.LeaseToken,
			NowMs: in.NowMs,
		}.record(alloc)
		if err := insertAssetOp(ctx, tx, record); err != nil {
			return nil, nil, err
		}
		enqueued = append(enqueued, TrialEnqueuedReward{
			PlayerID: m.playerID, OpID: op.OpID, Seq: alloc.Seq, StreamEpoch: alloc.Epoch, LeaseToken: op.LeaseToken,
		})
	}
	return enqueued, owed, nil
}

// occupyTrialCounters 按 player_id 升序给每位得奖者占一次当日次数(带上限 upsert,周期键 = 开战时的游戏日)。
// 判到"达上限"说明 readTrialRewarded 读到的值已过期:回 errRetryTx,inTx 整事务回滚重跑,新一轮该人不再得奖(06 §6.30 e 步)。
func occupyTrialCounters(ctx context.Context, tx *sql.Tx, rewarded []trialMember, battle TrialBattleKey, dailyLimit uint32, nowMs uint64) error {
	for _, m := range rewarded {
		limited, err := upsertCounterWithLimit(ctx, tx, m.playerID, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_ACTIVITY,
			battle.ActivityID, battle.PeriodKey, 1, dailyLimit, nowMs)
		if err != nil {
			return err
		}
		if limited {
			return fmt.Errorf("%w: activity %d usage of player %d changed after it was read (battle %d)",
				errRetryTx, battle.ActivityID, m.playerID, battle.BattleID)
		}
	}
	return nil
}

// grantTrialFunds 给帮会记一场计资金的胜场(06 §6.30 f 步)。只在本局有人得奖时调用。
// funds 是本事务锁 G 时读到的资金。返回:是否发了资金、提交后当天已计资金胜场。
//
// 配表没配资金(guild_funds = 0)时不碰进度行。否则保证当天的进度行存在 → 主键点锁读回 →
// 胜场未到每日上限(activity.TrialFundsCounted)才发资金并 +1;到了上限只发个人奖(I15)。
func grantTrialFunds(ctx context.Context, tx *sql.Tx, battle TrialBattleKey, row *tablepb.GuildActivityTable, funds, nowMs uint64) (granted bool, countedWins uint32, err error) {
	if row.GetGuildFunds() == 0 {
		return false, 0, nil
	}
	guildID, activityID, periodKey := battle.GuildID, battle.ActivityID, battle.GuildPeriodKey
	if _, err := tx.ExecContext(ctx, sqlEnsureReunionProgress, guildID, activityID, periodKey, nowMs, nowMs); err != nil {
		return false, 0, fmt.Errorf("ensure %s (%d,%d,%d): %w", guildActivityProgressTable, guildID, activityID, periodKey, err)
	}
	progress, found, err := scanActivityProgress(tx.QueryRowContext(ctx, sqlLockActivityProgress, guildID, activityID, periodKey))
	if err != nil {
		return false, 0, fmt.Errorf("lock %s (%d,%d,%d): %w", guildActivityProgressTable, guildID, activityID, periodKey, err)
	}
	if !found {
		return false, 0, fmt.Errorf("%s (%d,%d,%d) vanished right after upsert", guildActivityProgressTable, guildID, activityID, periodKey)
	}
	if !activity.TrialFundsCounted(row, progress.Count) {
		return false, progress.Count, nil
	}
	grant := row.GetGuildFunds()
	if funds > math.MaxUint64-grant {
		return false, 0, fmt.Errorf("%w: funds of guild %d plus %d (battle %d)", ErrActivityPoison, guildID, grant, battle.BattleID)
	}
	if err := execExactlyOneRow(ctx, tx, fmt.Sprintf("grant trial %d funds to guild %d", activityID, guildID),
		sqlSetActivityFunds, funds+grant, guildID); err != nil {
		return false, 0, err
	}
	// progress.Count < guild_threshold ≤ MaxUint32,+1 不溢出。
	if err := execExactlyOneRow(ctx, tx, fmt.Sprintf("count trial win in %s (%d,%d,%d)", guildActivityProgressTable, guildID, activityID, periodKey),
		sqlSetTrialProgressCount, progress.Count+1, nowMs, guildID, activityID, periodKey); err != nil {
		return false, 0, err
	}
	return true, progress.Count + 1, nil
}

// creditTrialContribution 给每位得奖者加帮贡(再写 lockTrialMembers 已锁住的行;溢出已在调用前判过)。
func creditTrialContribution(ctx context.Context, tx *sql.Tx, battle TrialBattleKey, rewarded []trialMember, gain uint64) error {
	if gain == 0 {
		return nil
	}
	for _, m := range rewarded {
		if err := execExactlyOneRow(ctx, tx, fmt.Sprintf("credit trial %d contribution to member %d in guild %d", battle.ActivityID, m.playerID, battle.GuildID),
			sqlSetActivityContribution, m.contributionTotal+gain, m.contributionBalance+gain, battle.GuildID, m.playerID); err != nil {
			return err
		}
	}
	return nil
}

// insertOwedRewards 按 player_id 升序插待入队物品行。本事务的幂等闸门已保证这是该局的首次结算,
// 撞键(1062)不应出现;真出现按暂时性错误上抛,由调用方重试时走 Duplicate。
func insertOwedRewards(ctx context.Context, tx *sql.Tx, battle TrialBattleKey, owed []uint64, payload []byte, nowMs uint64) error {
	for _, playerID := range owed {
		if _, err := tx.ExecContext(ctx, sqlInsertOwedReward,
			playerID, battle.BattleID, battle.GuildID, battle.ActivityID, battle.PeriodKey, payload, nowMs); err != nil {
			return fmt.Errorf("insert %s (player=%d battle=%d): %w", guildTrialRewardOwedTable, playerID, battle.BattleID, err)
		}
	}
	return nil
}

// ── 毒消息标记 / 巡检判过期 ───────────────────────────────────

// TrialMarkOutcome 是 MarkTrialBattlePoison 的去向。零值无效。
type TrialMarkOutcome uint8

const (
	// TrialMarkDone:本次把对局行写成 SETTLED/POISON(已提交)。
	TrialMarkDone TrialMarkOutcome = iota + 1
	// TrialMarkAlreadySettled:对局行此前已是 SETTLED(正常结算或更早的标记),保持原样。
	TrialMarkAlreadySettled
	// TrialMarkGuildGone:帮会不存在,不写任何行。
	TrialMarkGuildGone
	// TrialMarkMismatch:登记行属于别的帮,保持原样。
	TrialMarkMismatch
	// TrialMarkMissing:没有登记行,而入参又不足以补一行(只给了 battle_id / guild_id)。
	TrialMarkMissing
)

// MarkTrialBattlePoison 把一局标成"确定性失败、不发奖"(06 §6.31):对局行进 SETTLED、结论 POISON。
//
// 事务 op=trial_mark:G FOR UPDATE → T 普通读 → 缺行则插入(battle 字段齐全时)/ 有行则主键点锁后点改。
// battle 至少给 BattleID 与 GuildID;结果事件带着完整上下文时把其余字段也填上,这样连登记都没成功的局也能留下一行审计
// (createdMs 取 finishedAtMs,0 则取 nowMs)。巡检器只有登记行的键时,缺行就无从补插,回 TrialMarkMissing。
//
// 五种去向都返回 nil error,表示"这一局不必再处理",调用方销账并提交 offset。
// 错误语义:ErrTrialInputInvalid(键为 0)/ ErrWriteConflict 与其余错误为暂时性,调用方下轮再试(不销账)。
// 不设合服闸门:只写一行状态,不动任何资产。
func (r *ActivityRepo) MarkTrialBattlePoison(ctx context.Context, battle TrialBattleKey, finishedAtMs, nowMs uint64) (TrialMarkOutcome, error) {
	if err := battle.validateIdentity(); err != nil {
		return 0, err
	}
	if err := validateTrialNow(nowMs); err != nil {
		return 0, err
	}
	createdMs := finishedAtMs
	if createdMs == 0 {
		createdMs = nowMs
	}
	err := r.guilds.inTx(ctx, opTrialMark, func(ctx context.Context, tx *sql.Tx) error {
		if _, _, err := lockTrialGuild(ctx, tx, battle.GuildID); err != nil {
			return err
		}
		gate, err := readTrialGate(ctx, tx, battle)
		if err != nil {
			return err
		}
		if !gate.exists && !battle.complete() {
			return errTrialRowMissing
		}
		return writeTrialBattleState(ctx, tx, battle, gate, pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_SETTLED,
			pb.GuildTrialSettleResult_GUILD_TRIAL_SETTLE_RESULT_POISON, 0, createdMs, nowMs)
	})
	switch {
	case err == nil:
		return TrialMarkDone, nil
	case errors.Is(err, errTrialAlreadySettled):
		return TrialMarkAlreadySettled, nil
	case errors.Is(err, ErrGuildGone):
		return TrialMarkGuildGone, nil
	case errors.Is(err, errTrialForeignGuild):
		return TrialMarkMismatch, nil
	case errors.Is(err, errTrialRowMissing):
		return TrialMarkMissing, nil
	default:
		return 0, err
	}
}

// ExpireTrialBattle 把一局长时间没有结果的对局从 STARTED 判为 EXPIRED(06 §6.33 第 2 步)。battleID / guildID 取自
// ListOverdueTrialBattles 读到的行。返回 true = 本次完成了这次迁移。
//
// 事务 op=trial_mark:G FOR UPDATE → T 普通读 → 仍是 STARTED 才主键点锁后点改。
// 返回 false 且 error 为 nil:行已不是 STARTED(刚被结算 / 已判过期)、行不在了、或帮会已解散(解散同事务删了它的在途行)。
// EXPIRED 不是终态:迟到的结果仍会把它结算成 SETTLED(I22)。
// 错误语义:ErrTrialInputInvalid / ErrWriteConflict;guildID 与登记行不符为内部错误(巡检器的键取自同一行,不应出现)。
func (r *ActivityRepo) ExpireTrialBattle(ctx context.Context, battleID, guildID, nowMs uint64) (bool, error) {
	battle := TrialBattleKey{BattleID: battleID, GuildID: guildID}
	if err := battle.validateIdentity(); err != nil {
		return false, err
	}
	if err := validateTrialNow(nowMs); err != nil {
		return false, err
	}
	expired := false
	err := r.guilds.inTx(ctx, opTrialMark, func(ctx context.Context, tx *sql.Tx) error {
		expired = false // 重试契约:每一轮从头判
		if _, _, err := lockTrialGuild(ctx, tx, guildID); err != nil {
			return err
		}
		gate, err := readTrialGate(ctx, tx, battle)
		if err != nil {
			return err
		}
		if !gate.exists || gate.state != pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_STARTED {
			return nil
		}
		// 有行时 writeTrialBattleState 只用到 battle 的 BattleID / GuildID,其余字段不参与点改。
		if err := writeTrialBattleState(ctx, tx, battle, gate, pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_EXPIRED,
			pb.GuildTrialSettleResult_GUILD_TRIAL_SETTLE_RESULT_UNSPECIFIED, 0, 0, nowMs); err != nil {
			return err
		}
		expired = true
		return nil
	})
	switch {
	case err == nil:
		return expired, nil
	case errors.Is(err, ErrGuildGone), errors.Is(err, errTrialAlreadySettled):
		return false, nil
	default:
		return false, err
	}
}

// ── 待入队物品(guild_trial_reward_owed)────────────────────────

// OwedReward 是待入队物品表的一行。
type OwedReward struct {
	PlayerID, BattleID, GuildID uint64
	ActivityID, PeriodKey       uint32
	Payload                     []byte
	Attempts                    uint32
	CreatedMs                   uint64
}

// OwedRewardCursor 是 ListOwedRewards 的键集游标:上一页最后一行的 (created_ms, player_id, battle_id)。零值 = 从头读。
type OwedRewardCursor struct {
	CreatedMs, PlayerID, BattleID uint64
}

// Cursor 返回"读过这一行之后"的游标。
func (o OwedReward) Cursor() OwedRewardCursor {
	return OwedRewardCursor{CreatedMs: o.CreatedMs, PlayerID: o.PlayerID, BattleID: o.BattleID}
}

// OwedConvertStatus 是 ConvertOwedReward 的去向。零值无效。
type OwedConvertStatus uint8

const (
	// OwedConverted:已转成一行资产指令并删掉待入队行(已提交);重投循环随后投递。
	OwedConverted OwedConvertStatus = iota + 1
	// OwedStillFull:该玩家本流未决指令仍满,什么都没转;待入队行的 attempts 已 +1(尽力而为)。
	OwedStillFull
	// OwedGone:待入队行已不在(别的实例刚转走),什么都没做。
	OwedGone
)

// ConvertOwedReward 把一行待入队物品转成资产指令行(06 §6.32 第 2 步)。opID 由调用方在事务前发号(非 0)。
//
// 事务 op=trial_owed:AllocateSeq(锁 Q)→ 插 O(ACTIVITY_REWARD、PENDING、不持租约、next_attempt_ms = nowMs,重投循环立刻可领)
// → 完整主键点删 W。点删影响 0 行说明别的实例已经转走:整体回滚(刚插的指令行一并撤销),回 OwedGone。
// **不建 seq 行**:待入队行只会由结算在 seq 分配被拒时写下,那时该玩家本流的 seq 行已存在且永不删除;
// 所以这里不需要成员行守卫(玩家此时可能早已离帮,物品照发)。seq 行真不在则是内部错误(fail-closed)。
//
// 错误语义:ErrTrialInputInvalid / ErrWriteConflict / 其余内部错误,调用方本行记失败、下一轮再来(待入队行原样留着)。
// 未用上的 opID 浪费掉即可。
func (r *ActivityRepo) ConvertOwedReward(ctx context.Context, owed OwedReward, opID, nowMs uint64) (OwedConvertStatus, error) {
	switch {
	case owed.PlayerID == 0 || owed.BattleID == 0 || opID == 0:
		return 0, fmt.Errorf("%w: owed reward needs non-zero player (%d), battle (%d) and op id (%d)",
			ErrTrialInputInvalid, owed.PlayerID, owed.BattleID, opID)
	case len(owed.Payload) == 0:
		return 0, fmt.Errorf("%w: owed reward (player=%d battle=%d) has empty payload", ErrTrialInputInvalid, owed.PlayerID, owed.BattleID)
	}
	if err := validateTrialNow(nowMs); err != nil {
		return 0, err
	}
	const stream = assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT

	err := r.guilds.inTx(ctx, opTrialOwed, func(ctx context.Context, tx *sql.Tx) error {
		alloc, err := assetop.AllocateSeq(ctx, tx, r.seq, owed.PlayerID, stream, assetop.DefaultLimits, nowMs)
		if errors.Is(err, assetop.ErrTooManyPending) {
			return errOwedWindowFull
		}
		if err != nil {
			return err
		}
		record := activityRewardRow{
			OpID: opID, PlayerID: owed.PlayerID, GuildID: owed.GuildID,
			ActivityID: owed.ActivityID, PeriodKey: owed.PeriodKey, Payload: owed.Payload,
			NextAttemptMs: nowMs, NowMs: nowMs,
		}.record(alloc)
		if err := insertAssetOp(ctx, tx, record); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, sqlDeleteOwedReward, owed.PlayerID, owed.BattleID)
		if err != nil {
			return fmt.Errorf("delete %s (player=%d battle=%d): %w", guildTrialRewardOwedTable, owed.PlayerID, owed.BattleID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("delete %s (player=%d battle=%d): read rows affected: %w", guildTrialRewardOwedTable, owed.PlayerID, owed.BattleID, err)
		}
		switch affected {
		case 1:
			return nil
		case 0:
			return errOwedRowGone
		default:
			return fmt.Errorf("delete %s (player=%d battle=%d): deleted %d rows", guildTrialRewardOwedTable, owed.PlayerID, owed.BattleID, affected)
		}
	})
	switch {
	case err == nil:
		return OwedConverted, nil
	case errors.Is(err, errOwedRowGone):
		return OwedGone, nil
	case errors.Is(err, errOwedWindowFull):
		// 事务已回滚。attempts 只是诊断列:加不上不影响下一轮照常重试,所以只打日志、不改变"仍满"这个结论
		// (把它当错误返回,调用方会把一行正常排队的物品记成转换失败)。
		bumpCtx, cancel := context.WithTimeout(ctx, activityReadBudget)
		defer cancel()
		if _, err := r.db.ExecContext(bumpCtx, sqlBumpOwedRewardAttempts, owed.PlayerID, owed.BattleID); err != nil {
			logx.Errorf("[GuildActivity] bump attempts of %s (player=%d battle=%d) failed: %v",
				guildTrialRewardOwedTable, owed.PlayerID, owed.BattleID, err)
		}
		return OwedStillFull, nil
	default:
		return 0, err
	}
}

// ListOwedRewards 按 (created_ms, player_id, battle_id) 升序读一页待入队物品,从 after 之后开始,至多 limit 行。limit <= 0 返回空。
//
// 后台循环要**翻页读完**(每页用最后一行的 Cursor 作下一页的 after),不能每轮只取最老的一页:
// 队头若恰好是一批未决指令长期不消的玩家(背包一直满),后面的人会被饿死 —— 而"绝不跳过"是对每个人的承诺。
func (r *ActivityRepo) ListOwedRewards(ctx context.Context, after OwedRewardCursor, limit int) ([]OwedReward, error) {
	if limit <= 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, activityReadBudget)
	defer cancel()
	rows, err := r.db.QueryContext(ctx, sqlSelectOwedRewardsPage,
		after.CreatedMs, after.CreatedMs, after.PlayerID, after.PlayerID, after.BattleID, limit)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", guildTrialRewardOwedTable, err)
	}
	defer rows.Close()

	var out []OwedReward
	for rows.Next() {
		var o OwedReward
		if err := rows.Scan(&o.PlayerID, &o.BattleID, &o.GuildID, &o.ActivityID, &o.PeriodKey, &o.Payload, &o.Attempts, &o.CreatedMs); err != nil {
			return nil, fmt.Errorf("scan %s: %w", guildTrialRewardOwedTable, err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s: %w", guildTrialRewardOwedTable, err)
	}
	return out, nil
}

// CountOwedRewards 数待入队物品的行数,数到 limit 为止(积压 gauge 用;06 §6.32:上限 10000 行时只报 10000)。
func (r *ActivityRepo) CountOwedRewards(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(ctx, activityReadBudget)
	defer cancel()
	var n int
	if err := r.db.QueryRowContext(ctx, sqlCountOwedRewardsCapped, limit).Scan(&n); err != nil {
		return 0, fmt.Errorf("count %s: %w", guildTrialRewardOwedTable, err)
	}
	return n, nil
}

// OwedRewardCounts 返回本人各活动的待入队物品行数:activity_id → 行数(视图第 7c 步,加进 my_pending_reward_count)。
func (r *ActivityRepo) OwedRewardCounts(ctx context.Context, playerID uint64) (map[uint32]uint32, error) {
	ctx, cancel := context.WithTimeout(ctx, activityReadBudget)
	defer cancel()
	rows, err := r.db.QueryContext(ctx, sqlCountOwedRewardsByActivity, playerID)
	if err != nil {
		return nil, fmt.Errorf("count %s of player %d: %w", guildTrialRewardOwedTable, playerID, err)
	}
	defer rows.Close()

	out := make(map[uint32]uint32)
	for rows.Next() {
		var activityID, n uint32
		if err := rows.Scan(&activityID, &n); err != nil {
			return nil, fmt.Errorf("scan %s count of player %d: %w", guildTrialRewardOwedTable, playerID, err)
		}
		out[activityID] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s counts of player %d: %w", guildTrialRewardOwedTable, playerID, err)
	}
	return out, nil
}

// ── 历练的读(不加锁,1000ms 子预算)────────────────────────────

// TrialBattle 按 battle_id 读一行对局。found=false = 没有这一行。普通读,不持任何锁,不在任何事务里。
// 两个使用方:
//   - 视图判"battle:lock 指向的那一局是否仍是 STARTED"(06 §6.8 第 6 步);
//   - 结算入口的终态预读(logic.SettleTrialResult):读到 SETTLED 就当重复结果直接销账,不再开事务。
//     这依赖"SETTLED 行不可变"—— 写 T 的五条路径里,readTrialGate 见到 SETTLED 一律回 errTrialAlreadySettled,
//     解散只删 STARTED 行(deleteGuildTrialBattles)。将来若新增会改 / 删 SETTLED 行的路径(保留期清理、重放工具),
//     要回头确认这次预读仍然成立:读到 SETTLED 之后行被删掉无妨(那一局照样是"已有定论")。
//
// 不持锁读到的"不是 SETTLED"不能当结论用(随时可能过期),权威判定仍是持着 G 行锁的 readTrialGate(文件头第 10 条)。
func (r *ActivityRepo) TrialBattle(ctx context.Context, battleID uint64) (TrialBattleRow, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, activityReadBudget)
	defer cancel()
	var (
		row           TrialBattleRow
		state, result int32
	)
	err := r.db.QueryRowContext(ctx, sqlSelectTrialBattle, battleID).Scan(
		&row.BattleID, &row.GuildID, &row.ActivityID, &row.PeriodKey, &row.GuildPeriodKey, &row.InitiatorPlayerID,
		&state, &result, &row.RewardedCount, &row.CreatedMs, &row.SettledMs)
	if errors.Is(err, sql.ErrNoRows) {
		return TrialBattleRow{}, false, nil
	}
	if err != nil {
		return TrialBattleRow{}, false, fmt.Errorf("read %s %d: %w", guildTrialBattleTable, battleID, err)
	}
	row.State, row.Result = pb.GuildTrialBattleState(state), pb.GuildTrialSettleResult(result)
	return row, true, nil
}

// OverdueTrialBattle 是巡检器的一条候选:仍是 STARTED、登记早于给定时刻的对局。
type OverdueTrialBattle struct {
	BattleID, GuildID, CreatedMs uint64
}

// ListOverdueTrialBattles 返回登记时刻早于 createdBeforeMs 且仍为 STARTED 的对局,按登记时刻升序,至多 limit 行(06 §6.33 第 1 步)。
// 普通读:返回之后行可能已被结算,调用方后续的结算 / 判过期各自在事务里复核。
func (r *ActivityRepo) ListOverdueTrialBattles(ctx context.Context, createdBeforeMs uint64, limit int) ([]OverdueTrialBattle, error) {
	if limit <= 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, activityReadBudget)
	defer cancel()
	rows, err := r.db.QueryContext(ctx, sqlSelectOverdueTrialBattles,
		int32(pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_STARTED), createdBeforeMs, limit)
	if err != nil {
		return nil, fmt.Errorf("list overdue %s: %w", guildTrialBattleTable, err)
	}
	defer rows.Close()

	var out []OverdueTrialBattle
	for rows.Next() {
		var b OverdueTrialBattle
		if err := rows.Scan(&b.BattleID, &b.GuildID, &b.CreatedMs); err != nil {
			return nil, fmt.Errorf("scan overdue %s: %w", guildTrialBattleTable, err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate overdue %s: %w", guildTrialBattleTable, err)
	}
	return out, nil
}

// TrialRosterMembers 普通读名单里哪些人此刻是本帮成员:player_id → join_time_ms。不在返回里的人不是本帮成员。
// 给建房与确认开战做权威成员核对(06 §6.24 第 7 步 / §6.26 第 3 步;缓存快照可能陈旧)。
// 只用来拒绝开战,不构成发奖依据 —— 发奖时的成员资格由结算在锁内复核。playerIDs 为空返回空 map,至多 maxTrialCandidates 人。
func (r *ActivityRepo) TrialRosterMembers(ctx context.Context, guildID uint64, playerIDs []uint64) (map[uint64]uint64, error) {
	out := make(map[uint64]uint64, len(playerIDs))
	if len(playerIDs) == 0 {
		return out, nil
	}
	if len(playerIDs) > maxTrialCandidates {
		return nil, fmt.Errorf("%w: roster of %d players exceeds the cap %d", ErrTrialInputInvalid, len(playerIDs), maxTrialCandidates)
	}
	ctx, cancel := context.WithTimeout(ctx, activityReadBudget)
	defer cancel()
	args := make([]any, 0, len(playerIDs)+1)
	args = append(args, guildID)
	args = append(args, uint64Args(playerIDs)...)
	query := sqlSelectTrialRosterMembersHead + placeholders(len(playerIDs)) + sqlSelectTrialRosterMembersTail
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read trial roster members of guild %d: %w", guildID, err)
	}
	defer rows.Close()

	for rows.Next() {
		var playerID, joinTimeMs uint64
		if err := rows.Scan(&playerID, &joinTimeMs); err != nil {
			return nil, fmt.Errorf("scan trial roster member of guild %d: %w", guildID, err)
		}
		out[playerID] = joinTimeMs
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate trial roster members of guild %d: %w", guildID, err)
	}
	return out, nil
}

// ── 解散(X-14,B6b)────────────────────────────────────────────

// deleteGuildTrialBattles 删本帮**在途**(STARTED)的历练对局行,由 DisbandGuild 在删进度行之后、删 guild 行之前调用。
//
// 前置:调用方已持有该帮 guild 行 FOR UPDATE,且锁序更靠前的表(S / M / A / O / P)都已做完 —— 本函数之后只剩删 guild 行。
//
// 为什么必须删在途行:T 的写者都先锁 guild 行(文件头第 10 条),帮会行一删,这些行就再也没人能写 ——
// 既结算不了(回 GuildGone)也判不了过期,会永远停在 STARTED,卡在巡检器 `state = STARTED ORDER BY created_ms` 扫描的队头。
// 为什么**不**删已结算 / 已判过期的行:v1 不清理历练行,它们随帮会寿命线性增长(活跃的满员帮一天可上百行);
// 在解散事务的子预算(txBudgetDisband)里逐行点删全部历史,老帮会将每次都超预算、永远解散不掉。在途行数有上界
// (一人同时至多在一局里,不超过成员数),与历史长短无关。留下的行此后不可变,不在任何扫描里,只占存储;
// 06 §6.41 I12"解散删历练行"按此收窄为"删在途行",旧行的清理留给 v1.1 的保留期任务。
//
// 普通读候选(idx_0 (guild_id, state))→ 主键升序逐行点删(§12.2 第 2 条),不写 `DELETE … WHERE guild_id = ?`。
// 候选集完整:T 行的插入者与把行改成 / 改离 STARTED 的写者都先锁同一帮的 guild 行,解散持有它直到提交;
// RC 下候选读发生在拿到 guild 行锁之后,看得见此前已提交的全部行。同一把锁下没人能并发删改它们,点删影响 0 行不会发生;
// 真出现也只说明行已不在,不作错误。
func deleteGuildTrialBattles(ctx context.Context, tx *sql.Tx, guildID uint64) error {
	battleIDs, err := readIDColumn(ctx, tx, sqlSelectStartedTrialBattlesOfGuild,
		guildID, int32(pb.GuildTrialBattleState_GUILD_TRIAL_BATTLE_STATE_STARTED))
	if err != nil {
		return fmt.Errorf("read started %s of guild %d: %w", guildTrialBattleTable, guildID, err)
	}
	for _, battleID := range battleIDs {
		if _, err := tx.ExecContext(ctx, sqlDeleteTrialBattle, battleID); err != nil {
			return fmt.Errorf("delete %s %d of disbanded guild %d: %w", guildTrialBattleTable, battleID, guildID, err)
		}
	}
	return nil
}
