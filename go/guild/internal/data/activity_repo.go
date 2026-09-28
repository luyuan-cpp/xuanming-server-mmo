package data

// 帮会活动(B6a:元宵灯会 / 中秋团圆)的两个写事务、解散时的进度清理,以及活动视图要的三个读
// (设计 docs/design/guild-phase2/06-activities.md §6.8 / §6.9 / §6.11 / §6.12 / §6.14;
// 90-consistency X-13 / X-14 / Y-04 与 92-handoff.md §12.2 / §12.4 的订正优先于 06 正文 —— 06 写于死锁修复之前)。
//
// 与 economy_repo.go 同一套基座:写事务只走 GuildRepo.inTx(READ COMMITTED、1213 / 9007 整事务重跑、1205 与子预算到期
// 归一成 ErrWriteConflict,logic 回 GuildBusyRetry);资产指令同一套写法(事务内 ensureSeqRowTx → assetop.AllocateSeq →
// insertAssetOp,只插 PENDING 行、不自己改玩家资产);计数行同一条带上限 upsert(upsertCounterWithLimit,X-13)。
// 本文件只多出进度表 guild_activity_progress(记号 P,锁序位置 8,见 tables.go)。
//
// 规则判定(阈值是否达成、资金该不该发、入帮满没满 N 小时、个人 / 帮会周期键)一律调 guild/internal/activity 的纯函数,
// 不在这里另写一份:同一条规则要在视图、预检、事务三处得出相同结论(activity 包头)。repo 不读墙钟,
// 两个周期键都由调用方给的 NowMs 按 activity 包的规则现算,调用方无法传进一对彼此矛盾的键。
//
// # 取锁序列(表间全序 G < S < M < A < Q < O < C < P,92-handoff §12.2 第 1 条 + tables.go)
//
// 点灯 / 团圆(participate,op=activity):
//
//	G(guild_id) FOR UPDATE → M(G,p) 主键点锁(FORCE INDEX (PRIMARY))→ [Q(p, GUILD_CREDIT) 缺行则在事务内建]
//	→ Q(p, GUILD_CREDIT) FOR UPDATE(有物品奖励时由 AllocateSeq 锁,否则 sqlLockSeqGuard)→ [插 O 新行]
//	→ C(p, ACTIVITY, activity_id, 游戏日) 带上限 upsert → P(G, activity_id, 帮会期键) upsert → P 主键点锁读回
//	→ 对本事务已持有行的再写:G.funds、P 的锁存 / 资金标记、M 的帮贡。
//
// 解散(guild_manage_repo.go 的 DisbandGuild 调 deleteGuildActivityProgress):
//
//	… → O↑(提前截止)→ P 普通读候选 → P 主键升序逐行点删 → 删 G。
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

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

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

// activityRewardRecord 造一行活动发奖指令(06 §6.9 的列表)。
//
// 刻意为 0 的列:deadline_ms(永不中止,物品属于玩家,背包满就保持 PENDING 等腾出空间,C6);
// contribution_delta / funds_delta(帮贡与资金已在本事务里记完,Finalize 对 ACTIVITY_REWARD 只做 CAS、不做对侧账);
// ref_count(活动发奖不退任何计数,asset_store.go counterRefund 对本 kind 恒不退)。
// period_key 记本人游戏日键,只作审计;next_attempt_ms = 租约到期时刻,同步投递握着租约期间重投循环不碰它。
func activityRewardRecord(in ActivityTxInput, alloc assetop.Alloc, dayKey uint32) *pb.GuildAssetOpRecord {
	rw := in.Reward
	return &pb.GuildAssetOpRecord{
		OpId:          rw.OpID,
		PlayerId:      in.PlayerID,
		Stream:        uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT),
		Seq:           alloc.Seq,
		GuildId:       in.GuildID,
		Kind:          pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_ACTIVITY_REWARD,
		Status:        pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING,
		NextAttemptMs: rw.LeaseUntilMs,
		DeadlineMs:    0,
		Payload:       rw.Payload,
		RefId:         in.Activity.GetId(),
		PeriodKey:     dayKey,
		CreatedMs:     in.NowMs,
		UpdatedMs:     in.NowMs,
		LeaseUntilMs:  rw.LeaseUntilMs,
		LeaseToken:    rw.LeaseToken,
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
