package data

// 帮会资产指令的 assetop.Store 实现(设计 05-economy.md §5.19–§5.23;顶部"B4b 落地后的接口终稿"、
// D2 决策覆盖、90-consistency X-03 与 07-rollback-fail-closed.md §7.4.1 的订正优先于正文)。
//
// 它回答四件事:哪些行到期了(ListDue)、这一行归我处理(Claim)、这一轮没结论改天再来(Reschedule)、
// 结论定了一次性落库并做对侧账(Finalize / ResolveManually)。另有清理任务与 assetopfix 用的两个读。
//
// 与 go/trade/internal/data/asset_op_repo.go 同形,**有意不同的两处**:
//  1. Finalize 与 ResolveManually 都同写 next_attempt_ms = now(07 §7.4.1 硬要求):B5d 的回档检查按
//     "终态行的 next_attempt_ms = 终结时刻"查 `next_attempt_ms > since`,漏写 = 漏行 = 回档复制资产(fail-open),
//     清理判龄也会偏早。assetop.Store 的 Finalize 契约注释(reconcile.go:136-138)与 trade 都漏了它,别照抄。
//  2. 对侧账在同一事务里真的做了(资金 / 帮贡 / 次数 / 限购),trade 那边还只是 TODO。
//
// 锁序与业务写事务一致:guild → guild_member → guild_player_op_seq → guild_asset_op → guild_daily_counter
// (全序见 economy_repo.go 文件头)。本文件的每条锁定语句都是完整主键等值点操作(2026-09-21 死锁修复契约 P3):
//   - 领取按 op_id 点改(自动提交,理由见下);重排 / 毒行 / 终结 / 人工终结在事务里**先** sqlLockAssetOp 主键点锁、
//     再按 op_id 带复核条件点改(死锁复核 V1,理由见下方 TiDB 附加规则)。MySQL 下取锁顺序都是"聚簇记录 → 被改列所在的二级索引项";
//   - 对侧账锁 guild 行、成员行(guild_member 带 FORCE INDEX (PRIMARY),理由同 economy_repo.go 的 sqlLockMemberBalance);
//   - 终结在退次数 / 退限购分支另锁 guild_player_op_seq(p, op 的流)作计数行守卫(死锁复核 C6,sqlLockSeqGuard);
//   - 清理改成"普通读候选主键 → 逐行 RC 短事务{主键点锁 → 带复核条件的主键点删}",不再有经二级索引的范围 DELETE。
//
// 终结对 op 行的主键 CAS 不需要 seq 行守卫(friend 审计 #4 的修法 B 未采用):assetop.AllocateSeq 的未决行查询已改成
// 普通读(修法 A),预留事务对 op 表除自己插入的新行外不持任何锁,终结的主键 CAS 与它之间没有可反序的资源。
// 这条结论依赖**预留事务是 READ COMMITTED**(普通读才看得见前一个分配者已提交的未决行):guild 的 inTx 与
// 本文件用的 assetop.WithTxRetry(DefaultTxRetryConfig)都固定 RC,改隔离级之前先回来重判这里。
// 退款分支那把 seq 行锁管的是**计数行**(C6,TiDB 下 IODKU 与 Point_Get 在计数行上各持一半互等),与上面这条无关。
//
// TiDB 附加规则(2026-09-21 死锁复核 G-C2 / C5 / V1):**凡是悲观事务会锁到的行(行 key 或任一唯一 key,含非聚簇 PRIMARY),
// 多 key 写(改到二级索引列、删行)一律进显式 RC 事务,且先用快路径主键点锁**,不走自动提交。
// TiDB 默认 pessimistic-auto-commit=false,自动提交语句按乐观两阶段提交;7 张表都是 SHARD_ROW_ID_BITS + PRE_SPLIT_REGIONS,
// 行 key 与各索引 key 落在不同 region,prewrite 按 region 并行,于是可能先写上一部分 key 的乐观锁、再在另一个 key 上
// 撞到悲观事务的锁;对方随后要的正是被乐观锁占着的那个 key —— 这种互等不在 TiKV 死锁检测器的等待图里,
// 只能等锁超时或乐观锁 TTL 过期。环不需要对方改非唯一索引项:只要对方锁住这一行的行 key 或任一唯一 key 就够。
// **只进显式事务还不够,写之前必须先点锁**(V1 订正:此前这里写的"进事务后写者都先在行 key 上串行"不成立)。
// 带复核谓词的 UPDATE / DELETE(`op_id = ? AND status = ? …`)不走 Point_Get 快路径:执行期间只读不锁,到语句末尾才把
// {行 key, PRIMARY key, uk_guild_asset_op key}(tidb_lock_unchanged_keys 默认 ON,没改的唯一 key 也锁)按 region **并行**
// 加悲观锁,批内顺序应用控制不了。两条这样的语句(如提前截止 ‖ 捐献被拒的终结、提前截止 ‖ 重排、人工终结 ‖ 重排)在它们
// 之前没有更早取到的公共锁时,可能各拿到一部分 key 再互等 —— 检测器看得见的 1213,会被重试吸收,但仍是死锁。
// 所以每个写者在这类语句之前,**在同一事务里**先跑 sqlLockAssetOp(完整主键等值 FOR UPDATE,不带任何复核条件):
// 它走 Point_Get 快路径,加锁顺序固定为"PRIMARY key → 行 key"两次独立加锁,同一 op 行的全部悲观写者都先在 PRIMARY key
// 上排成一列;拿到它的一方随后语句末尾那一批里只剩 uk key 是新锁,而没有任何人能不先拿 PRIMARY key 就去锁这一行的 uk key
// (领取只写行 key,见下;预留插的是新行新 uk 值)。点锁读不到行 = 行已被清理(只删终态行),按"已被别人处理"走原语义。
// MySQL 下点锁与随后的主键 UPDATE 锁的是同一条聚簇记录,锁集与顺序都不变(聚簇 → 二级),只多一次往返。
//   - 重排(Reschedule)、毒行(markPoison)改 next_attempt_ms(idx_0),与提前截止 / 终结 / 人工终结改同一行:进事务,首句点锁;
//   - 终结 / 人工终结(terminate):lockCounterparty(guild / 成员 / seq 行,锁序靠前的表)之后、CAS 之前点锁;
//   - 提前截止(economy_repo.go 的 accelerateDonationDeadlines):每个候选先点锁、再带复核条件点改;
//   - 领取(Claim)**保留自动提交,且不许包进事务**:自动提交 UPDATE 只有在 !InTxn 时才带 SkipWriteUntouchedIndices
//     (tidb pkg/executor/write.go),它只改无索引列,变更集合只有行 key 一个;lock_unchanged_keys 对乐观事务直接返回
//     (addUnchangedKeysForLockByRow 的 !IsPessimistic 分支)。单 key 不可能"只拿到一部分",只会单向等待。
//     包进事务反而会让它在语句末尾并行锁 PRIMARY / uk_guild_asset_op,与带额外谓词的 CAS 写者之间出现可检测的 1213,
//     还给热路径多 3 次往返。同理**不要**在没有重新推演 Claim 的情况下打开 TiDB 的 pessimistic-auto-commit:
//     打开后 Claim 成了悲观语句,同样会带上 PRIMARY / UKO 的并行批锁;单开它也消不掉清理与退款的环,只把 TTL 互等变成 1213。
//   - 清理点删(死锁复核 C5):旧周期计数行会被终结的退款(sqlRefundCounter,Point_Get 锁 PRIMARY key → 行 key)锁到,
//     自动提交的乐观 DELETE 与它可能各持一半 —— 进 RC 短事务,先 sqlLockCleanupCounter / sqlLockAssetOp 快路径点锁,
//     与退款同序。保留期外的终态 op 行没有别的悲观写者,只是共用同一个 helper(execCleanupDelete)。
//     **前提**(C5 补遗):清理不持 seq 行守卫,所以它只许与退款相遇、不许与预留的带上限 upsert 相遇 —— 后者在语句末尾
//     并行锁 {行 key, PRIMARY key},与清理的 Point_Get 在同一行上可各持一半(与 C6 同一机制)。这由截止键保证:计数行清理的
//     截止时刻至少是 now − minCounterCleanupAge(8 天),上一周期(含切周 / 切日后仍在途的预留要写的那一期)永不进清理范围。
//
// 计数行 guild_daily_counter 上的写者与取锁全序(修后;记号 M = guild_member、Q = guild_player_op_seq、
// O = guild_asset_op、C = guild_daily_counter):
//   - 带上限 upsert(T-D / T-S):M → Q(p, stream) FOR UPDATE → 插 O → 语句末尾整批锁 C 本行 {行 key, PRIMARY key};
//     只写 period_key = 请求开头 start 所在周期的行;
//   - 退次数 / 退限购(终结 / 人工终结):[M] → Q(p, stream) → O 点锁 → C Point_Get 点改(PRIMARY key → 行 key);
//   - 清理:C Point_Get 点锁(PRIMARY key → 行 key)→ 带范围复核的点删;只删 period_key ≤ 截止键的行(counterCleanupCutoffs)。
//   upsert 与退款先在同一 Q 上串行;退款与清理在 C 上同序;upsert 与清理的行集合不相交(截止键所在周期早于任何
//   开始于 24h 内的请求的周期,见 minCounterCleanupAge),所以计数行上不存在反序对。
//   新增计数写者要么先持 Q,要么同样只碰截止键以前的行。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/metric"
	"google.golang.org/protobuf/proto"

	assetpb "proto/common/asset"
	pb "proto/guild"

	"shared/assetop"
	"shared/gameday"
	"shared/safego"
)

// Store 各方法的子预算(90 part2 §2 第 8 条)。
//
// Finalize 的 2000ms 只在 assetopfix 路径上是真实上限:重投循环调 Finalize / Reschedule 时传进来的是
// settleContext(reconcile.go:552,700ms、不继承取消),子预算取两者较小值,实际只有 700ms。
// 这是 assetop 的取舍(落库必须在投递预算之外有独立时间),本文件不去抬高它。
const (
	storeReadBudget     = 1000 * time.Millisecond // ListDue / Reschedule / 各种读
	storeClaimBudget    = 1000 * time.Millisecond
	storeFinalizeBudget = 2000 * time.Millisecond // Finalize / ResolveManually(含事务外那一次不可变列读)
)

// backgroundTxAttempts:后台写事务(Finalize / ResolveManually,以及 2026-09-21 起的 Reschedule / 毒行推迟)的
// 总尝试次数(90 part2 §2 第 6 条)。
// 清理是逐行 RC 短事务(死锁复核 C5 起;之前是逐行 autocommit 点删),每行只尝试 1 次、不在本轮重试(失败的行下一轮再删)。
const backgroundTxAttempts = 3

// 清理节律(05 §5.22):单批 500 行、批间 100ms、每类每轮至多 20 批,一批候选不足 500 行即停。
// 一批 = 一次候选普通读(≤ 500 行主键)+ 逐行 RC 短事务{主键点锁 → 主键点删};批间 sleep 给业务写事务让路。
const (
	cleanupBatchSize  = 500
	cleanupBatchPause = 100 * time.Millisecond
	cleanupMaxBatches = 20
	// cleanupStmtBudget:单行清理短事务(点锁 + 点删 + 提交)的上限。只锁一行,锁等待已被 DSN 封顶 1s;预算放到 2s 是为了
	// 让 1205 先于 ctx 到期暴露(错误更好判读)。跑满说明库有状况,本轮到此为止,已删的行照常计数,下一轮接着删。
	cleanupStmtBudget = 2 * time.Second

	// 计数行的周期键:日键 8 位(YYYYMMDD)、周键 6 位(YYYYWW),数值域不相交,
	// 日 / 周两类各用一段 BETWEEN 取候选、做点删复核,互不误删(gameday.PeriodKey 的注释)。
	dayKeyFloor  = 19700101
	weekKeyFloor = 100000

	// minCounterCleanupAge:计数行清理截止时刻离 now 的最小距离(死锁复核 C5 补遗);CounterRetention 比它短时按它算。
	//
	// 为什么:清理短事务不持 seq 行守卫,它能与计数行的其他写者安全相遇,前提是只碰带上限 upsert 已不可能再写的旧周期行。
	// 游戏周恰好 7 天:保留期取配置下限 7 天时,切周后整整一周 WeekKey(now − 7d) 都等于上一周 W−1,清理会删 W−1 的全部行;
	// 而跨切周的兑换预留(T-S)用请求开头的 start 算周期键,经 economyCaller、跨服务发号之后才在切周之后 upsert W−1 行
	// (副本时钟偏差 < 1s 也会造成同样的跨期,gameday 包头写明这是接受的)。两者同行相遇:
	//   - TiDB 下 upsert 语句末尾并行锁 {行 key, PRIMARY key},清理 Point_Get 先 PRIMARY key 后行 key,可各持一半互等(1213);
	//   - 清理先删掉后 upsert 又会重建一行 used_count = n 的 W−1 计数,把该周期的限购绕过一次。
	// 取 8 天:相差 7 天的两个时刻必落在相邻两个游戏周(固定时区、无夏令时),所以 WeekKey(now − 8d) < WeekKey(now − 24h);
	// 日键同理只会更早(DayKey(now − 8d) < DayKey(now − 24h))。于是请求开头 start 晚于"清理时刻 − 24h"的预留,
	// 写的周期键一定大于本轮截止键;预留事务跑在秒级请求预算内,24h 余量足够。默认保留 30 天时不起作用。
	// 夹紧放在清理自身而不只靠配置校验:绕过 config 直接构造 CleanupConf(测试、工具)也删不到上一周期。
	minCounterCleanupAge = 8 * 24 * time.Hour
)

// 清理指标的 table label 与 orphan 指标的两个 label,全部取自固定集合(不带任何 id,AGENTS §9)。
const (
	cleanupTableAssetOp = "guild_asset_op"
	cleanupTableCounter = "guild_daily_counter"

	orphanKindDonate = "donate"
	orphanKindShop   = "shop"

	orphanWhatGuildGone        = "guild_gone"         // 捐献扣款成功,但绑定的帮会已解散:资金与帮贡都记不上
	orphanWhatMemberGone       = "member_gone"        // 捐献扣款成功,帮会还在但捐献者已离帮:只记资金、跳过帮贡
	orphanWhatRefundMemberGone = "refund_member_gone" // 兑换被永久拒绝,但兑换者已不是该帮成员:帮贡退不回去
)

var (
	guildAssetOrphanTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: guildMetricNamespace,
		Subsystem: "asset",
		Name:      "orphan_total",
		Help:      "资产指令终结时对侧账无处可记的次数(D2:捐献绑定的帮会已解散 / 捐献者已离帮 / 兑换退帮贡时已离帮)。不做补偿,只计数与打 INFO;持续上升说明离帮提前截止没有生效。",
		Labels:    []string{"kind", "what"},
	})

	guildAssetCleanupDeletedTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: guildMetricNamespace,
		Subsystem: "asset",
		Name:      "cleanup_deleted_total",
		Help:      "清理任务删除的行数(终态资产指令 / 过期计数行)。长期为 0 而表在长,说明清理没在跑。",
		Labels:    []string{"table"},
	})
)

// recordAssetOrphan / recordCleanupDeleted:生产实现计指标,单测替换为记录器(同 invalidateGaveUp 的惯例,
// 不去读 go-zero 的全局注册表)。
var (
	recordAssetOrphan    = func(kind, what string) { guildAssetOrphanTotal.Inc(kind, what) }
	recordCleanupDeleted = func(table string, n int64) { guildAssetCleanupDeletedTotal.Add(float64(n), table) }
)

const (
	sqlListDueSegmentFresh = "SELECT `op_id` FROM " + guildAssetOpTable +
		" WHERE `status` = ? AND `next_attempt_ms` <= ? AND `lease_until_ms` < ? AND `attempts` < ?" +
		" ORDER BY `next_attempt_ms` ASC, `op_id` ASC LIMIT ?"
	sqlListDueSegmentAged = "SELECT `op_id` FROM " + guildAssetOpTable +
		" WHERE `status` = ? AND `next_attempt_ms` <= ? AND `lease_until_ms` < ? AND `attempts` >= ?" +
		" ORDER BY `next_attempt_ms` ASC, `op_id` ASC LIMIT ?"

	// sqlLockAssetOp:op 行的主键点锁(死锁复核 C5 / V1)。**只许**是完整主键等值 + FOR UPDATE、不带任何复核条件:
	// TiDB 只有这种形状才走 Point_Get 快路径,加锁顺序固定为"PRIMARY key → 行 key";复核条件留在随后的点改 / 点删里。
	// 用在四类写者的写语句之前(同一事务):提前截止(economy_repo.go)、终结 / 人工终结(terminate)、重排、毒行推迟,
	// 以及清理点删(execCleanupDelete)。MySQL 下是 PRIMARY const,只锁这一条聚簇记录 —— 随后的主键写本来就要锁它,锁集不变。
	sqlLockAssetOp = "SELECT `op_id` FROM " + guildAssetOpTable + " WHERE `op_id` = ? FOR UPDATE"

	sqlClaimAssetOp = "UPDATE " + guildAssetOpTable + " SET `lease_until_ms` = ?, `lease_token` = ?, `updated_ms` = ?" +
		" WHERE `op_id` = ? AND `status` = ? AND `lease_until_ms` < ?"
	// 毒行推迟:令牌之外还要带 `status = PENDING`。人工终结(sqlResolveAssetOp)的 CAS 只看 status、不换令牌,
	// 它可能恰好落在 Claim 的 CAS 与这里之间;只凭令牌会把已终结的行改回"一小时后再来"、抹掉 last_outcome,
	// 破坏"终态行此后没有任何路径再改"(07 §7.2)与"终态行 next_attempt_ms = 终结时刻"(07 §7.4.1)。
	sqlPoisonAssetOp = "UPDATE " + guildAssetOpTable +
		" SET `last_outcome` = ?, `next_attempt_ms` = ?, `lease_until_ms` = 0, `updated_ms` = ?" +
		" WHERE `op_id` = ? AND `lease_token` = ? AND `status` = ?"
	sqlRescheduleAssetOp = "UPDATE " + guildAssetOpTable +
		" SET `attempts` = `attempts` + 1, `next_attempt_ms` = ?, `lease_until_ms` = 0," +
		" `durable` = ?, `last_outcome` = ?, `last_reason` = ?, `updated_ms` = ?" +
		" WHERE `op_id` = ? AND `status` = ? AND `lease_token` = ?"

	// 终结 CAS:`status = PENDING` 是"一次且仅一次"的最后一道闸,人工与自动同时下手也只有一个赢家。
	sqlFinalizeAssetOp = "UPDATE " + guildAssetOpTable +
		" SET `status` = ?, `durable` = 1, `last_outcome` = ?, `last_reason` = ?, `reason_tip_id` = ?," +
		" `lease_until_ms` = 0, `next_attempt_ms` = ?, `updated_ms` = ?" +
		" WHERE `op_id` = ? AND `status` = ?"
	// 人工终结:不置 durable(不是 scene 确认的落盘结局,置了会让事后对账分不清"scene 答过"与"人写的")、
	// 不改 last_outcome(保留最后一次 scene 真实答复作证据),同样同写 next_attempt_ms。
	sqlResolveAssetOp = "UPDATE " + guildAssetOpTable +
		" SET `status` = ?, `resolved_by` = ?, `resolve_reason` = ?, `lease_until_ms` = 0," +
		" `next_attempt_ms` = ?, `updated_ms` = ?" +
		" WHERE `op_id` = ? AND `status` = ?"

	// 终结前在事务外读的**不可变列**:插入之后没有任何路径改它们,读一次即可,不必占事务时间。
	// stream 给退次数 / 退限购分支的 seq 行守卫用(C6):取 op 行自身的列,不按 kind 反推(不造第二份 kind→stream 事实源)。
	sqlSelectAssetOpImmutable = "SELECT `player_id`, `guild_id`, `stream`, `kind`, `ref_id`, `ref_count`, `period_key`," +
		" `contribution_delta`, `funds_delta` FROM " + guildAssetOpTable + " WHERE `op_id` = ?"

	// sqlLockSeqGuard:终结在退次数 / 退限购分支对 guild_player_op_seq(p, op 的流)的点锁,计数行守卫(死锁复核 C6,仅 TiDB 成环)。
	// TiDB 下预留的带上限 upsert(IODKU)在语句末尾把计数行的 {行 key, PRIMARY key} 按 region 并行加锁,可能先拿到行 key、
	// PRIMARY key 还在途;退款的 sqlRefundCounter 是 Point_Get,先 PRIMARY key 后行 key —— 两者同时在途就各持一半互等(1213)。
	// 预留在 AllocateSeq 里已持同一 seq 行 X,这里让退款也先拿它:计数行的全部悲观写者都在 seq 行上排成一列,不再部分持有。
	// 完整主键等值、stream 绑定参数(不写字面量);MySQL 下是 PRIMARY const,只锁这一条聚簇记录。
	sqlLockSeqGuard = "SELECT `next_seq` FROM " + guildPlayerOpSeqTable + " WHERE `player_id` = ? AND `stream` = ? FOR UPDATE"

	sqlSelectAssetOpByID = "SELECT " + assetOpColumns + " FROM " + guildAssetOpTable + " WHERE `op_id` = ?"
	sqlListStuckAssetOps = "SELECT " + assetOpColumns + " FROM " + guildAssetOpTable +
		" WHERE `status` = ? AND `created_ms` < ? ORDER BY `created_ms` ASC, `op_id` ASC LIMIT ?"
	sqlOldestPendingAssetOp = "SELECT MIN(`created_ms`) FROM " + guildAssetOpTable + " WHERE `status` = ? AND `stream` = ?"

	// 对侧账。guild_member 的两条 UPDATE 带 FORCE INDEX (PRIMARY):WHERE 同时钉死了 uk_guild_member(player_id),
	// 理由同 economy_repo.go 的 sqlLockMemberBalance;行已在同一事务里先经 sqlLockMemberRole 锁住,
	// 帮贡列不在任何二级索引里,这两条只动聚簇记录。
	sqlLockGuildFunds     = `SELECT funds FROM guild WHERE guild_id = ? FOR UPDATE`
	sqlCreditGuildFunds   = `UPDATE guild SET funds = funds + ? WHERE guild_id = ?`
	sqlCreditContribution = `UPDATE guild_member FORCE INDEX (PRIMARY)
	SET contribution_total = contribution_total + ?, contribution_balance = contribution_balance + ?
	WHERE guild_id = ? AND player_id = ?`
	sqlRefundContribution = `UPDATE guild_member FORCE INDEX (PRIMARY)
	SET contribution_balance = contribution_balance + ?
	WHERE guild_id = ? AND player_id = ?`
	sqlRefundCounter = `UPDATE guild_daily_counter SET used_count = IF(used_count >= ?, used_count - ?, 0), updated_ms = ? WHERE player_id = ? AND counter_kind = ? AND ref_id = ? AND period_key = ?`
)

// 清理(05 §5.22)的候选读与点删(2026-09-21 死锁修复,契约 §2.2 第 3 条)。
//
// 旧写法是带 LIMIT 的范围 DELETE:终态行那条走 idx_guild_asset_op_0 (status, next_attempt_ms),"二级项 → 聚簇记录"
// 取锁,且 DELETE 没有 semi-consistent read,扫到被锁的行一律真等;一批 500 行的锁要到整批提交才放。它与旧的
// 提前截止(经 idx_2 / idx_1 扫到本玩家 / 本帮的历史终态行)在同一行上反序成环(friend 审计 #5、#15)。
//
// 现在每批:普通读候选主键(不加锁,走哪个索引都行)→ 按主键升序逐行一个 RC 短事务:完整主键点锁 → 点删,
// 点删的 WHERE 带原条件做提交点复核(影响 0 行 = 已被别人删 / 条件已不成立,跳过)。每个短事务只锁这一行的聚簇记录,
// 再按字典顺序 delete-mark 它的各个二级项(聚簇 → 二级)。修复后没有任何事务经二级索引去锁终态行或旧周期的计数行 ——
// 未决读是普通读、提前截止是候选读 + 主键点改、终结 / 重排 / 领取 / 毒行都是主键 CAS、退款是主键点改 —— 所以清理只会
// 单向等待,不可能成环;锁也只持有一行的时间,不会再让离帮 / 被踢 / 解散在整批清理后面排满 1s。
// 为什么是短事务而不是单条自动提交(死锁复核 C5,TiDB):见文件头 TiDB 附加规则与 execCleanupDelete。
const (
	// 终态行只删三种:**不含** APPLIED_PARTIAL(要人工补偿,证据不能自动消失,X-15 / 90 part2 §3)与 PENDING(永不删)。
	// 判龄按 next_attempt_ms —— 终态行上它等于终结时刻(Finalize / ResolveManually 同写)。
	// 按 op_id 升序取前 500:op_id 是雪花号、随创建时刻单调增,老的终态行天然排在前面。
	sqlListCleanupTerminalOps = "SELECT `op_id` FROM " + guildAssetOpTable +
		" WHERE `status` IN (?, ?, ?) AND `next_attempt_ms` < ? ORDER BY `op_id` ASC LIMIT ?"
	sqlCleanupTerminalOp = "DELETE FROM " + guildAssetOpTable +
		" WHERE `op_id` = ? AND `status` IN (?, ?, ?) AND `next_attempt_ms` < ?"

	// 计数行:4 列完整主键点删,period_key 的范围条件同时作复核(日键与周键各走一段,见 dayKeyFloor)。
	sqlListCleanupCounters = `SELECT player_id, counter_kind, ref_id, period_key FROM guild_daily_counter
	WHERE period_key BETWEEN ? AND ? ORDER BY player_id, counter_kind, ref_id, period_key LIMIT ?`
	sqlCleanupCounter = `DELETE FROM guild_daily_counter
	WHERE player_id = ? AND counter_kind = ? AND ref_id = ? AND period_key = ? AND period_key BETWEEN ? AND ?`

	// 清理点删前的点锁(死锁复核 C5)。只写完整主键等值、不带任何复核条件:TiDB 只有这种形状才走 Point_Get 快路径,
	// 加锁顺序固定为"PRIMARY 索引 key → 行 key"两次独立加锁,与 sqlRefundCounter(同为 4 列主键等值的快路径点改)同序,
	// 双方只会排队、不会各持一半;复核条件留在随后的点删里。MySQL 下是 PRIMARY const,只锁聚簇记录。
	// 预留的带上限 upsert(语句末尾并行锁)与它同序不了,靠截止键让两者永不碰同一行(minCounterCleanupAge,C5 补遗)。
	// 终态 op 行的点锁与其余 op 行写者共用 sqlLockAssetOp(同一形状,见上)。
	sqlLockCleanupCounter = `SELECT period_key FROM guild_daily_counter
	WHERE player_id = ? AND counter_kind = ? AND ref_id = ? AND period_key = ? FOR UPDATE`
)

// PendingStatus 是 guild_asset_op.status 的"未决"库值,给 assetop 的 seq 分配与本文件的 SQL 用。
// 取生成枚举而不是写 1:assetop.Status 刻意不绑库值,库值只由本表的枚举定。
func PendingStatus() uint32 { return uint32(pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING) }

// StatusToRecord 把 assetop 的语义状态映射成本表的库值。映射只此一处。
// 未知语义值落 UNSPECIFIED:既不能落成 PENDING(会被循环反复领走),也不能落成 APPLIED(伪装成功)。
func StatusToRecord(st assetop.Status) pb.GuildAssetOpStatus {
	switch st {
	case assetop.StatusPending:
		return pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING
	case assetop.StatusApplied:
		return pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED
	case assetop.StatusRejected:
		return pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_REJECTED
	case assetop.StatusAborted:
		return pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_ABORTED
	case assetop.StatusAppliedPartial:
		return pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED_PARTIAL
	default:
		return pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_UNSPECIFIED
	}
}

// isTerminalRecord:终结路径只接受四个终态。PENDING 写进去等于没终结,UNSPECIFIED 会让客服查不到结局。
func isTerminalRecord(st pb.GuildAssetOpStatus) bool {
	switch st {
	case pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED,
		pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_REJECTED,
		pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_ABORTED,
		pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED_PARTIAL:
		return true
	default:
		return false
	}
}

// isRetryableBackground:后台写的重试分类 = 1213 / 9007 / errRetryTx(isRetryableTxError)再加 1205。
// 与请求路径不同,后台写**重试** 1205:它没有玩家在等"稍后重试"的提示,锁等待超时之后再排一次队
// 比把一个已定的终局留在内存里、等下一轮重投再投一次 scene 划算得多。
func isRetryableBackground(err error) bool {
	return isRetryableTxError(err) || isLockWaitTimeout(err)
}

// FinalizedOp 是一次**本次**终结的摘要,供提交后的推送使用。
type FinalizedOp struct {
	OpID, PlayerID, GuildID uint64
	Kind                    pb.GuildAssetOpKind
	Status                  assetop.Status
}

// GuildAssetStore 是 guild_asset_op 的 assetop.Store 实现,另带人工终结与最老未决年龄两个可选能力。
//
// 线程模型:除 OnFinalized 外构造后只读;OnFinalized 必须在启动重投循环与 RPC 服务**之前**赋值,之后不再改
// (它没有锁,运行期改写与 worker 读取构成数据竞争)。
type GuildAssetStore struct {
	db     *sql.DB
	guilds *GuildRepo

	// OnFinalized 在**本次**终结(CAS 命中)且事务提交之后调用,guild.go 接到 logic 的推送;nil = 不推送(assetopfix)。
	// 它收到的 ctx 是调用 Finalize 时的 ctx(保留同步投递标记等值),不是本文件的子预算 ctx。
	OnFinalized func(context.Context, FinalizedOp)
}

var (
	_ assetop.Store            = (*GuildAssetStore)(nil)
	_ assetop.ManualResolver   = (*GuildAssetStore)(nil)
	_ assetop.PendingAgeReader = (*GuildAssetStore)(nil)
)

// NewGuildAssetStore。guilds 为 nil 时报错:对侧账要用它的缓存失效,连接池也取自它。
func NewGuildAssetStore(guilds *GuildRepo) (*GuildAssetStore, error) {
	if guilds == nil {
		return nil, errors.New("guild asset store: nil GuildRepo")
	}
	return &GuildAssetStore{db: guilds.db, guilds: guilds}, nil
}

// ── assetop.Store ────────────────────────────────────────────

// ListDue 非加锁一致性读,只回主键,走 idx_guild_asset_op_0 (status, next_attempt_ms)。
//
// **两段查询**(X-03 防饿死,契约见 assetop.Store.ListDue):第一段只取新行(attempts < FreshAttemptLimit)并占满
// limit;只有它不够时才发第二段取老行,且只补缺口。写成一条 SQL,退避封顶在 MaxBackoff 的老行永远"早就到期",
// 会霸占整批名额,新提交的捐献 / 兑换一次也轮不上。两段是两次独立的非锁读,期间 attempts 可能从 2 跳到 3,
// 同一行两段都出现,所以按 op_id 去重;第一段的 id 排前面,让 Claim 先抢新行。
func (s *GuildAssetStore) ListDue(ctx context.Context, nowMs uint64, limit int) ([]uint64, error) {
	if limit <= 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, storeReadBudget)
	defer cancel()

	fresh, err := s.listDueSegment(ctx, sqlListDueSegmentFresh, nowMs, limit)
	if err != nil {
		return nil, err
	}
	if len(fresh) >= limit {
		return fresh, nil
	}
	aged, err := s.listDueSegment(ctx, sqlListDueSegmentAged, nowMs, limit-len(fresh))
	if err != nil {
		return nil, err
	}
	seen := make(map[uint64]struct{}, len(fresh))
	for _, id := range fresh {
		seen[id] = struct{}{}
	}
	for _, id := range aged {
		if _, dup := seen[id]; dup {
			continue
		}
		fresh = append(fresh, id)
	}
	return fresh, nil
}

func (s *GuildAssetStore) listDueSegment(ctx context.Context, query string, nowMs uint64, limit int) ([]uint64, error) {
	rows, err := s.db.QueryContext(ctx, query, PendingStatus(), nowMs, nowMs, assetop.FreshAttemptLimit, limit)
	if err != nil {
		return nil, fmt.Errorf("list due %s: %w", guildAssetOpTable, err)
	}
	defer rows.Close()

	var ids []uint64
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan due %s: %w", guildAssetOpTable, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate due %s: %w", guildAssetOpTable, err)
	}
	return ids, nil
}

// Claim 单行主键 CAS 领取(autocommit),紧挨着处理前调用,租约从"领到这一行"起算。
// 刻意**不**进事务(死锁复核 C5 的结论):TiDB 下它只改无索引列,自动提交时变更集合只有行 key 一个,不会部分持有;
// 包进悲观事务反而会在语句末尾并行锁 PRIMARY / uk_guild_asset_op,见文件头 TiDB 附加规则。
// RowsAffected != 1 → (Op{}, false, nil):已被别的副本领走或已终结。
//
// payload 为空或解不开都是毒行:把它推迟到 poisonUntilMs(循环按 LoopConfig.PoisonDelay 算好的绝对时刻,
// **本文件不另写毒行延迟常量**)并回 assetop.ErrPoisonRow,循环计 decode 后继续下一行。空 payload 也算毒行:
// 下发一个空包,scene 会回 kAssetInvalidBundle,行被 REJECTED 终结并触发退款 / 退次数,而那不是真实的业务结局。
func (s *GuildAssetStore) Claim(ctx context.Context, opID, nowMs, leaseUntilMs, poisonUntilMs, token uint64) (assetop.Op, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, storeClaimBudget)
	defer cancel()

	result, err := s.db.ExecContext(ctx, sqlClaimAssetOp, leaseUntilMs, token, nowMs, opID, PendingStatus(), nowMs)
	if err != nil {
		return assetop.Op{}, false, fmt.Errorf("claim %s %d: %w", guildAssetOpTable, opID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return assetop.Op{}, false, fmt.Errorf("claim %s %d: read rows affected: %w", guildAssetOpTable, opID, err)
	}
	if affected != 1 {
		return assetop.Op{}, false, nil
	}

	rec, found, err := s.getRecord(ctx, opID)
	if err != nil {
		return assetop.Op{}, false, err
	}
	if !found {
		// 刚 CAS 成功又查不到:只可能是被并发删了(清理只删终态行),按故障返回。
		return assetop.Op{}, false, fmt.Errorf("claim %s %d: row vanished after claim", guildAssetOpTable, opID)
	}
	bundle := &assetpb.AssetBundle{}
	decodeErr := errors.New("empty payload")
	if len(rec.GetPayload()) > 0 {
		decodeErr = proto.Unmarshal(rec.GetPayload(), bundle)
	}
	if decodeErr != nil {
		s.markPoison(ctx, opID, token, nowMs, poisonUntilMs)
		return assetop.Op{}, false, fmt.Errorf("%w: op_id=%d: %v", assetop.ErrPoisonRow, opID, decodeErr)
	}
	return assetop.Op{
		OpID:        rec.GetOpId(),
		PlayerID:    rec.GetPlayerId(),
		Stream:      assetpb.AssetOpStream(rec.GetStream()),
		Seq:         rec.GetSeq(),
		StreamEpoch: rec.GetStreamEpoch(),
		// correlation_id 取 op_id:人工对账按 scene 流水的 correlation_id 定位唯一一行指令;
		// 必须与同步首投时 logic 填的值一致,否则重投会换一个 correlation。
		CorrelationID: rec.GetOpId(),
		TxType:        rec.GetTxType(),
		Bundle:        bundle,
		Attempts:      rec.GetAttempts(),
		DeadlineMs:    rec.GetDeadlineMs(),
		LeaseToken:    token,
		LastReason:    rec.GetLastReason(),
	}, true, nil
}

// markPoison 把毒行推迟到 poisonUntilMs,last_outcome 记"结局未知"。用本次领取的 lease_token 且仍为 PENDING 做 CAS:
// 不会推迟别的副本刚领走的同一行,也不会改写领取之后才被人工终结的行。写失败只记日志:后果是下一轮再撞一次
// 同一行(仍然跳过),不丢数据。
//
// 进显式 RC 短事务而不是自动提交(G-C2,见文件头 TiDB 附加规则):它改 next_attempt_ms(idx_0),与离帮 / 被踢 / 解散
// 的提前截止点改、终结 / 人工终结的 CAS 是同一行上的写者。事务首句 sqlLockAssetOp 主键点锁(V1):TiDB 下先与其余写者在
// PRIMARY key 上排队,再做带复核条件的 CAS,不会在语句末尾的并行批里与对方各持一半;MySQL 下锁集不变。
// 点锁读不到行 = 行已被清理(只删终态行),与 CAS 影响 0 行同义:什么都不写。
// 重试口径同 Finalize(1213 / 9007 / 1205,整事务已回滚,重跑安全)。
func (s *GuildAssetStore) markPoison(ctx context.Context, opID, token, nowMs, poisonUntilMs uint64) {
	err := assetop.WithTxRetry(ctx, s.db, backgroundTxAttempts, isRetryableBackground, func(tx *sql.Tx) error {
		found, err := lockRowExists(ctx, tx, sqlLockAssetOp, opID)
		if err != nil {
			return fmt.Errorf("lock %s %d: %w", guildAssetOpTable, opID, err)
		}
		if !found {
			return nil
		}
		_, err = tx.ExecContext(ctx, sqlPoisonAssetOp,
			uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN), poisonUntilMs, nowMs, opID, token, PendingStatus())
		return err
	})
	if err != nil {
		logx.Errorf("[GuildAsset] 推迟毒行失败 op_id=%d: %v", opID, err)
	}
}

// Reschedule 退回待办并推迟下一次投递,带 lease_token 做 CAS。
// RowsAffected == 0 → assetop.ErrLeaseLost:租约在处理期间被另一个副本接管,本次结果一个字都没写进去;
// 回 nil 会把"我的结果被丢弃"伪装成成功,assetop_reschedule_lost_total 就恒为 0。
// last_reason 照写 res.Reason:部分发放码(assetop.ReasonPartialApplied)的粘性由 assetop 的 carryPartialReason 负责,
// 这里不自作主张。
//
// 显式 RC 短事务,不用自动提交(G-C2,见文件头 TiDB 附加规则)。TiDB 下自动提交按乐观事务提交:本语句可能先把
// idx_0 旧项 (PENDING, 旧 next_attempt_ms, rowid) 锁上,再在行 key 上撞到提前截止 / 终结的悲观锁,而对方提交时要删的
// 正是这条 idx_0 旧项 —— 互等只能等 TTL 过期打破。
// 只进事务还不够(V1):带复核条件的 CAS 在 TiDB 上于语句末尾**并行**锁 {行 key, PRIMARY key, uk key},两个这样的写者
// 可能各持一半互等(可检测的 1213)。所以事务首句先 sqlLockAssetOp 主键点锁(Point_Get:PRIMARY key → 行 key),
// 同一 op 行上的全部悲观写者都先在 PRIMARY key 上排队,拿到它才发 CAS。
// 修后取锁全序(同一 op 行上所有写者一致):TiDB 为 PRIMARY key → 行 key → uk key;MySQL 为聚簇记录 → 被改列所在的二级项
// (点锁与 CAS 锁同一条聚簇记录,锁集不变)。
// 点锁读不到行 = 行已被清理(只删终态行,租约早已不在我手里),与 CAS 影响 0 行同义:回 ErrLeaseLost。
// 重试只接 1213 / 9007 / 1205(整事务已回滚,重跑安全)。ErrLeaseLost 不可重试,原样透传给 reconcile(errors.Is 可判)。
// 预算仍是 storeReadBudget;重投循环传进来的是 700ms 的 settleContext,取两者较小值(reconcile.go settleBudget 的注释
// 本来就按"这里走 WithTxRetry"估算)。
func (s *GuildAssetStore) Reschedule(ctx context.Context, op assetop.Op, nextAttemptMs uint64, res assetop.Result, nowMs uint64) error {
	ctx, cancel := context.WithTimeout(ctx, storeReadBudget)
	defer cancel()

	return assetop.WithTxRetry(ctx, s.db, backgroundTxAttempts, isRetryableBackground, func(tx *sql.Tx) error {
		found, err := lockRowExists(ctx, tx, sqlLockAssetOp, op.OpID)
		if err != nil {
			return fmt.Errorf("reschedule %s %d: lock: %w", guildAssetOpTable, op.OpID, err)
		}
		if !found {
			return fmt.Errorf("guild asset op %d: %w", op.OpID, assetop.ErrLeaseLost)
		}
		result, err := tx.ExecContext(ctx, sqlRescheduleAssetOp,
			nextAttemptMs, durableFlag(res.Durable), uint32(res.Outcome), res.Reason, nowMs,
			op.OpID, PendingStatus(), op.LeaseToken)
		if err != nil {
			return fmt.Errorf("reschedule %s %d: %w", guildAssetOpTable, op.OpID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("reschedule %s %d: read rows affected: %w", guildAssetOpTable, op.OpID, err)
		}
		if affected == 0 {
			return fmt.Errorf("guild asset op %d: %w", op.OpID, assetop.ErrLeaseLost)
		}
		return nil
	})
}

func durableFlag(durable bool) uint32 {
	if durable {
		return 1
	}
	return 0
}

// Finalize 把行终结成 status,并在**同一事务**里做对侧账;返回是否**本次**终结。
//
// 事务外先读不可变列(无行 → ERROR + (false, nil));事务经 assetop.WithTxRetry(RC,1213 / 1205 / 9007 重跑 3 次):
// 按锁序加锁(对侧账的行 → op 行主键点锁,V1)→ CAS(同写 next_attempt_ms = now,07 §7.4.1)→ RowsAffected == 1 才做对侧账。
// reason_tip_id 只在 REJECTED 时写 res.Reason(玩家看得到的拒绝原因),其余写 0。
// 提交后:动过 guild / 成员行 → 失效缓存;然后调 OnFinalized。
func (s *GuildAssetStore) Finalize(ctx context.Context, op assetop.Op, status assetop.Status, res assetop.Result, nowMs uint64) (bool, error) {
	final := StatusToRecord(status)
	if !isTerminalRecord(final) {
		return false, fmt.Errorf("finalize %s %d: status %s is not terminal", guildAssetOpTable, op.OpID, status)
	}
	var reasonTip uint32
	if status == assetop.StatusRejected {
		reasonTip = res.Reason
	}
	return s.terminate(ctx, op.OpID, status, nowMs, "finalize", sqlFinalizeAssetOp,
		int32(final), uint32(res.Outcome), res.Reason, reasonTip, nowMs, nowMs, op.OpID, PendingStatus())
}

// ResolveManually 是人工终结通道(assetopfix)。入参合法性(终态、操作人与理由长度)由 assetop.ResolveManually
// 先校验;这里仍兜住"映射不出终态库值"的情况,在碰库之前失败。
// 与 Finalize 共用同一把 `status = PENDING` 的 CAS 与同一份对侧账:人工与循环同时下手只有一个赢家;
// 对侧账若各写一份,迟早出现"只改了一边"的分叉。
func (s *GuildAssetStore) ResolveManually(ctx context.Context, r assetop.ManualResolution, nowMs uint64) (bool, error) {
	final := StatusToRecord(r.Final)
	if !isTerminalRecord(final) {
		return false, fmt.Errorf("manual resolve %s %d: status %s is not terminal", guildAssetOpTable, r.OpID, r.Final)
	}
	return s.terminate(ctx, r.OpID, r.Final, nowMs, "manual resolve", sqlResolveAssetOp,
		int32(final), r.Operator, r.Reason, nowMs, nowMs, r.OpID, PendingStatus())
}

// assetOpImmutable 是终结时要用的不可变列。
type assetOpImmutable struct {
	PlayerID, GuildID             uint64
	Stream                        uint32 // assetpb.AssetOpStream 的库值;只给 sqlLockSeqGuard 绑参数
	Kind                          pb.GuildAssetOpKind
	RefID, RefCount, PeriodKey    uint32
	ContributionDelta, FundsDelta uint64
}

// counterpartyLocks 是对侧账加锁的结果:行在不在,决定记账还是计 orphan。
type counterpartyLocks struct{ guildOK, memberOK bool }

// counterpartyOutcome 是对侧账做了什么。副作用(指标、日志)在提交**之后**才发出:
// 事务可能被重跑,在闭包里直接计数会把一次终结记成多次。
type counterpartyOutcome struct {
	touched     bool   // 改过 guild 或 guild_member 行 → 提交后失效缓存
	orphanKind  string // 非空 = 提交后计 guild_asset_orphan_total
	orphanWhat  string
	unknownKind bool // kind 不认识:只做了 CAS
}

// terminate 是 Finalize 与 ResolveManually 的共同骨架;两者只差 CAS 语句(casQuery / casArgs)。
func (s *GuildAssetStore) terminate(ctx context.Context, opID uint64, status assetop.Status, nowMs uint64,
	what, casQuery string, casArgs ...any) (bool, error) {
	dbCtx, cancel := context.WithTimeout(ctx, storeFinalizeBudget)
	defer cancel()

	row, found, err := s.readImmutable(dbCtx, opID)
	if err != nil {
		return false, fmt.Errorf("%s %s %d: %w", what, guildAssetOpTable, opID, err)
	}
	if !found {
		// 行不存在:清理只删终态行,所以这里是数据被人工删了或 op_id 传错。不终结、不报错,让调用方继续下一行。
		logx.Errorf("[GuildAsset] %s: op_id=%d 不存在,跳过", what, opID)
		return false, nil
	}

	var (
		finalized bool
		outcome   counterpartyOutcome
	)
	err = assetop.WithTxRetry(dbCtx, s.db, backgroundTxAttempts, isRetryableBackground, func(tx *sql.Tx) error {
		// 重试契约:每次尝试从零开始,结果只在成功返回前写到外层。
		finalized, outcome = false, counterpartyOutcome{}

		locks, err := lockCounterparty(dbCtx, tx, row, status)
		if err != nil {
			return err
		}
		// op 行主键点锁(V1):排在对侧账的 guild / 成员 / seq 行之后(op 表在锁序里靠后),CAS 之前。
		// TiDB 下 CAS 带 `status = PENDING` 复核、不走快路径,语句末尾并行锁 {行 key, PRIMARY key, uk key};先点锁让它与
		// 提前截止 / 重排 / 毒行 / 另一个终结者先在 PRIMARY key 上排队,不再各持一半。MySQL 下点锁与 CAS 锁同一条聚簇记录。
		// 读不到行 = 已被清理(只删终态行),与 CAS 影响 0 行同义:不终结、不做对侧账。
		opRowFound, err := lockRowExists(dbCtx, tx, sqlLockAssetOp, opID)
		if err != nil {
			return fmt.Errorf("lock %s %d: %w", guildAssetOpTable, opID, err)
		}
		if !opRowFound {
			return nil
		}
		result, err := tx.ExecContext(dbCtx, casQuery, casArgs...)
		if err != nil {
			return fmt.Errorf("terminal CAS: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("terminal CAS: read rows affected: %w", err)
		}
		if affected != 1 {
			return nil // 已被别的副本 / 人工终结:不是错误,也不做对侧账
		}
		done, err := applyCounterparty(dbCtx, tx, row, status, locks, nowMs)
		if err != nil {
			return err
		}
		finalized, outcome = true, done
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("%s %s %d: %w", what, guildAssetOpTable, opID, err)
	}
	if !finalized {
		return false, nil
	}

	switch {
	case outcome.orphanKind != "":
		recordAssetOrphan(outcome.orphanKind, outcome.orphanWhat)
		logx.Infof("[GuildAsset] 对侧账无处可记 kind=%s what=%s op_id=%d player_id=%d guild_id=%d funds=%d contribution=%d status=%s",
			outcome.orphanKind, outcome.orphanWhat, opID, row.PlayerID, row.GuildID, row.FundsDelta, row.ContributionDelta, status)
	case outcome.unknownKind:
		logx.Errorf("[GuildAsset] 未知 kind=%d,只终结不做对侧账 op_id=%d status=%s", int32(row.Kind), opID, status)
	}
	if outcome.touched {
		s.guilds.invalidateAfterCommit(ctx, opAssetFinalize, row.GuildID, row.PlayerID)
	}
	if s.OnFinalized != nil {
		s.OnFinalized(ctx, FinalizedOp{
			OpID:     opID,
			PlayerID: row.PlayerID,
			GuildID:  row.GuildID,
			Kind:     row.Kind,
			Status:   status,
		})
	}
	return true, nil
}

func (s *GuildAssetStore) readImmutable(ctx context.Context, opID uint64) (assetOpImmutable, bool, error) {
	var (
		row  assetOpImmutable
		kind int32
	)
	err := s.db.QueryRowContext(ctx, sqlSelectAssetOpImmutable, opID).Scan(
		&row.PlayerID, &row.GuildID, &row.Stream, &kind, &row.RefID, &row.RefCount, &row.PeriodKey,
		&row.ContributionDelta, &row.FundsDelta)
	if errors.Is(err, sql.ErrNoRows) {
		return assetOpImmutable{}, false, nil
	}
	if err != nil {
		return assetOpImmutable{}, false, fmt.Errorf("read immutable columns: %w", err)
	}
	row.Kind = pb.GuildAssetOpKind(kind)
	return row, true, nil
}

// counterRefund 判定本次终结要不要退次数 / 退限购,以及退哪类计数、退几份。**判定只此一处**:
// lockCounterparty 的 seq 行守卫与 applyCounterparty 的退款都调它,两边不许各写一份条件(迟早分叉 —— 守卫漏锁即 C6)。
//   - 只有 REJECTED / ABORTED 退;APPLIED / APPLIED_PARTIAL 不退;period_key == 0(不限购的商品,当初没占计数行)不退;
//   - DONATE 退今日次数 1 次;SHOP 退限购 ref_count 份(0 份不退);其余 kind(活动发奖、未知)不退。
func counterRefund(row assetOpImmutable, status assetop.Status) (pb.GuildDailyCounterKind, uint32, bool) {
	if (status != assetop.StatusRejected && status != assetop.StatusAborted) || row.PeriodKey == 0 {
		return 0, 0, false
	}
	switch row.Kind {
	case pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE:
		return pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_DONATE, 1, true
	case pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_SHOP:
		return pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_SHOP, row.RefCount, row.RefCount > 0
	}
	return 0, 0, false
}

// lockCounterparty 按锁序先锁对侧账要改的行(在 op 行点锁与 CAS 之前:op 表排在 guild / guild_member / guild_player_op_seq 之后)。
//
//   - DONATE + APPLIED:guild 行 FOR UPDATE;帮会在才锁成员行。成员行按 op.guild_id 找 —— D2:结算一律记给
//     发起时绑定的帮会,不看玩家此刻在哪个帮。
//   - SHOP + REJECTED / ABORTED:成员行 FOR UPDATE(要退帮贡)。
//   - 要退次数 / 退限购的(counterRefund 为真):最后再锁 guild_player_op_seq(p, op 的流)作计数行守卫(C6,sqlLockSeqGuard)。
//     兑换分支因此是 guild_member → seq → op → counter,与 T-S 同向;捐献分支是 seq → op → counter,与 T-D 的 member → seq
//     同向(T-D 先持的成员行,本事务从不要)。seq 行建出后永不删除;缺行(人工删过、或夹具直接插的 op 行)不当错误 ——
//     守卫只管锁序、不管正确性,在这里拒绝会让这条指令永远停在 PENDING;缺行时至多退化成修复前那次可被重试吸收的 TiDB 1213。
//     与 friend 审计 #4 无关:op 行上的反序由 AllocateSeq 的未决行普通读(修法 A)解决,那条结论不依赖这把锁。
//   - 其余组合不加锁:APPLIED_PARTIAL 不做对侧账。
func lockCounterparty(ctx context.Context, tx *sql.Tx, row assetOpImmutable, status assetop.Status) (counterpartyLocks, error) {
	var locks counterpartyLocks
	switch {
	case row.Kind == pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE && status == assetop.StatusApplied:
		ok, err := lockRowExists(ctx, tx, sqlLockGuildFunds, row.GuildID)
		if err != nil {
			return locks, fmt.Errorf("lock guild %d for donation: %w", row.GuildID, err)
		}
		locks.guildOK = ok
		if !ok {
			return locks, nil
		}
		if locks.memberOK, err = lockRowExists(ctx, tx, sqlLockMemberRole, row.GuildID, row.PlayerID); err != nil {
			return locks, fmt.Errorf("lock donor %d of guild %d: %w", row.PlayerID, row.GuildID, err)
		}
	case row.Kind == pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_SHOP &&
		(status == assetop.StatusRejected || status == assetop.StatusAborted):
		ok, err := lockRowExists(ctx, tx, sqlLockMemberRole, row.GuildID, row.PlayerID)
		if err != nil {
			return locks, fmt.Errorf("lock buyer %d of guild %d: %w", row.PlayerID, row.GuildID, err)
		}
		locks.memberOK = ok
	}
	if _, _, refund := counterRefund(row, status); refund {
		if _, err := lockRowExists(ctx, tx, sqlLockSeqGuard, row.PlayerID, row.Stream); err != nil {
			return locks, fmt.Errorf("lock seq guard (player=%d stream=%d): %w", row.PlayerID, row.Stream, err)
		}
	}
	return locks, nil
}

// lockRowExists 跑一条单列的加锁读,只关心"行在不在"。
func lockRowExists(ctx context.Context, tx *sql.Tx, query string, args ...any) (bool, error) {
	var ignored uint64
	err := tx.QueryRowContext(ctx, query, args...).Scan(&ignored)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// applyCounterparty 是 Finalize 与 ResolveManually 共用的对侧账(D2 覆盖后的 §5.19.3 第 4 步):
//
//	| kind            | 终态               | 动作                                                                 |
//	| 任意            | APPLIED_PARTIAL    | 无(只 CAS;部分发放转人工补偿,不退次数、不退帮贡)                    |
//	| DONATE          | APPLIED            | 帮会在:资金 += funds_delta;成员在:帮贡两列 += c;缺哪边计哪条 orphan |
//	| DONATE          | REJECTED / ABORTED | 退今日次数 1 次                                                      |
//	| SHOP            | REJECTED / ABORTED | 成员在:退 balance;否则 orphan{shop,refund_member_gone};有周期键则退限购 ref_count 份 |
//	| SHOP            | APPLIED            | 无                                                                   |
//	| ACTIVITY_REWARD | 任意               | 无(帮贡与资金在入队事务里已记完)                                     |
//	| 其它            | 任意               | 只 CAS,提交后 ERROR                                                  |
//
// 增量为 0 的那一列跳过:ClientFoundRows=false 下"加 0"的 UPDATE RowsAffected 为 0,会被恰好一行的自检误判。
func applyCounterparty(ctx context.Context, tx *sql.Tx, row assetOpImmutable, status assetop.Status,
	locks counterpartyLocks, nowMs uint64) (counterpartyOutcome, error) {
	var out counterpartyOutcome
	if status == assetop.StatusAppliedPartial {
		return out, nil
	}
	refunded := status == assetop.StatusRejected || status == assetop.StatusAborted

	switch row.Kind {
	case pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE:
		if status == assetop.StatusApplied {
			if !locks.guildOK {
				out.orphanKind, out.orphanWhat = orphanKindDonate, orphanWhatGuildGone
				return out, nil
			}
			if row.FundsDelta > 0 {
				if err := execExactlyOneRow(ctx, tx, fmt.Sprintf("credit funds of guild %d", row.GuildID),
					sqlCreditGuildFunds, row.FundsDelta, row.GuildID); err != nil {
					return out, err
				}
				out.touched = true
			}
			if !locks.memberOK {
				out.orphanKind, out.orphanWhat = orphanKindDonate, orphanWhatMemberGone
				return out, nil
			}
			if row.ContributionDelta > 0 {
				if err := execExactlyOneRow(ctx, tx, fmt.Sprintf("credit contribution of member %d in guild %d", row.PlayerID, row.GuildID),
					sqlCreditContribution, row.ContributionDelta, row.ContributionDelta, row.GuildID, row.PlayerID); err != nil {
					return out, err
				}
				out.touched = true
			}
		}
	case pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_SHOP:
		if refunded {
			switch {
			case !locks.memberOK:
				out.orphanKind, out.orphanWhat = orphanKindShop, orphanWhatRefundMemberGone
			case row.ContributionDelta > 0:
				if err := execExactlyOneRow(ctx, tx, fmt.Sprintf("refund contribution of member %d in guild %d", row.PlayerID, row.GuildID),
					sqlRefundContribution, row.ContributionDelta, row.GuildID, row.PlayerID); err != nil {
					return out, err
				}
				out.touched = true
			}
		}
	case pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_ACTIVITY_REWARD:
		// 只 CAS。
	default:
		out.unknownKind = true
	}
	// 退次数 / 退限购:与 lockCounterparty 的 seq 行守卫同一个判定(counterRefund),计数行仍是最后一张表。
	if kind, n, refund := counterRefund(row, status); refund {
		if err := refundCounter(ctx, tx, row, kind, n, nowMs); err != nil {
			return out, err
		}
	}
	return out, nil
}

// refundCounter 退回 n 次计数。period_key == 0 表示当初没占计数行(不限购的商品),跳过。
// 计数行可能已被清理(跨了保留期)或本来就被别的路径减到 0:影响 0 行无害,不做自检。
// IF 兜底到 0:计数是 unsigned 列,减穿会报 1690 而让整笔终结失败。
func refundCounter(ctx context.Context, tx *sql.Tx, row assetOpImmutable, kind pb.GuildDailyCounterKind, n uint32, nowMs uint64) error {
	if row.PeriodKey == 0 || n == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, sqlRefundCounter, n, n, nowMs,
		row.PlayerID, int32(kind), row.RefID, row.PeriodKey); err != nil {
		return fmt.Errorf("refund daily counter (player=%d ref=%d period=%d): %w", row.PlayerID, row.RefID, row.PeriodKey, err)
	}
	return nil
}

// OldestPendingCreatedMs 实现 assetop.PendingAgeReader,喂 assetop_pending_oldest_age_seconds{stream}。
// 每 30s 一次的聚合读,走 idx_guild_asset_op_0 的 status 前缀(只扫未决行)。
func (s *GuildAssetStore) OldestPendingCreatedMs(ctx context.Context, stream assetpb.AssetOpStream) (uint64, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, storeReadBudget)
	defer cancel()
	var oldest sql.NullInt64
	if err := s.db.QueryRowContext(ctx, sqlOldestPendingAssetOp, PendingStatus(), uint32(stream)).Scan(&oldest); err != nil {
		return 0, false, fmt.Errorf("oldest pending %s: %w", guildAssetOpTable, err)
	}
	if !oldest.Valid || oldest.Int64 < 0 {
		return 0, false, nil
	}
	return uint64(oldest.Int64), true, nil
}

// ── assetopfix 用的读 ────────────────────────────────────────

// GetOp 按主键读一整行。found=false = 行不存在。
func (s *GuildAssetStore) GetOp(ctx context.Context, opID uint64) (AssetOpRow, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, storeReadBudget)
	defer cancel()
	rec, found, err := s.getRecord(ctx, opID)
	if err != nil || !found {
		return AssetOpRow{}, found, err
	}
	return assetOpRowFrom(rec), true, nil
}

// ListStuck 列出创建时刻早于 createdBeforeMs 的未决行,按创建时刻升序,至多 limit 条。limit <= 0 返回空。
func (s *GuildAssetStore) ListStuck(ctx context.Context, createdBeforeMs uint64, limit int) ([]AssetOpRow, error) {
	if limit <= 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, storeReadBudget)
	defer cancel()
	return queryAssetOpRows(ctx, s.db, "list stuck asset ops", sqlListStuckAssetOps, PendingStatus(), createdBeforeMs, limit)
}

func (s *GuildAssetStore) getRecord(ctx context.Context, opID uint64) (*pb.GuildAssetOpRecord, bool, error) {
	rec, err := scanAssetOpRecord(s.db.QueryRowContext(ctx, sqlSelectAssetOpByID, opID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("select %s %d: %w", guildAssetOpTable, opID, err)
	}
	return rec, true, nil
}

// ── 清理(goroutine guild.asset_op_cleanup)─────────────────────

// CleanupConf 是清理任务的节律与保留期。
//
// TerminalRetention 同时是 B5d 回档检查"保留期可证明性"的下界(90 part2 §3 末行):
// 删掉的终态行回档检查就再也看不见,调小它之前先核对回档窗口。
// CounterRetention 短于 minCounterCleanupAge(8 天)时,清理按 8 天算(counterCleanupCutoffs,C5 补遗):只会多留,不会少留。
type CleanupConf struct {
	Interval          time.Duration
	TerminalRetention time.Duration
	CounterRetention  time.Duration
}

// RunCleanup 阻塞运行清理循环,ctx 取消即返回;调用方用 safego.Go("guild.asset_op_cleanup", …) 启动。
//
// 每个副本都跑:删除幂等,多副本同时跑无害;随机初始延迟 [0, Interval) 只是把各副本错开,少抢同一批行锁。
// 每一轮各自 recover:单轮 panic 只丢那一轮,清理不会从此静默停摆。
func (s *GuildAssetStore) RunCleanup(ctx context.Context, c CleanupConf) {
	if c.Interval <= 0 {
		logx.Errorf("[GuildAsset] 清理间隔非法(%v),清理任务不启动", c.Interval)
		return
	}
	timer := time.NewTimer(time.Duration(rand.Int64N(int64(c.Interval))))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		safego.Run("guild.asset_op_cleanup", func() {
			if err := s.CleanupOnce(ctx, time.Now(), c); err != nil && ctx.Err() == nil {
				logx.Errorf("[GuildAsset] 清理失败: %v", err)
			}
		})
		timer.Reset(c.Interval)
	}
}

// CleanupOnce 跑一轮清理(可测:时刻由调用方给)。三类(终态指令、过期日键计数、过期周键计数)各自分批,
// 每批"普通读候选主键 → 逐行 RC 短事务{主键点锁 → 主键点删}"(见 sqlListCleanupTerminalOps 上方的说明与 execCleanupDelete)。
// 一类失败不影响另外两类,错误合并返回;已删的行数照常计指标。
// 点删失败不在本轮重试:没删掉的行仍满足条件,下一轮(Interval 之后)自然再删,不丢任何东西。
func (s *GuildAssetStore) CleanupOnce(ctx context.Context, now time.Time, c CleanupConf) error {
	if c.TerminalRetention <= 0 || c.CounterRetention <= 0 {
		return fmt.Errorf("guild asset cleanup: retention must be positive (terminal=%v counter=%v)",
			c.TerminalRetention, c.CounterRetention)
	}
	nowMs := uint64(now.UnixMilli())
	terminalMs := uint64(c.TerminalRetention / time.Millisecond)
	var opCutoffMs uint64
	if nowMs > terminalMs {
		opCutoffMs = nowMs - terminalMs
	}
	dayCutoff, weekCutoff := counterCleanupCutoffs(now, c.CounterRetention)

	var errs []error
	if opCutoffMs > 0 {
		errs = append(errs, s.cleanupInBatches(ctx, cleanupTableAssetOp, func(ctx context.Context) (int, int64, error) {
			return s.cleanupTerminalOpsBatch(ctx, opCutoffMs)
		}))
	}
	errs = append(errs,
		s.cleanupInBatches(ctx, cleanupTableCounter, func(ctx context.Context) (int, int64, error) {
			return s.cleanupCountersBatch(ctx, uint32(dayKeyFloor), dayCutoff)
		}),
		s.cleanupInBatches(ctx, cleanupTableCounter, func(ctx context.Context) (int, int64, error) {
			return s.cleanupCountersBatch(ctx, uint32(weekKeyFloor), weekCutoff)
		}))
	return errors.Join(errs...)
}

// counterCleanupCutoffs 返回本轮计数行清理的日键 / 周键截止(含):period_key 落在 [floor, cutoff] 的行会被删。
// 截止时刻取 now − max(retention, minCounterCleanupAge),保证上一周期的行在切周 / 切日后至少再留 24h,
// 不与在途预留的带上限 upsert 相遇(理由见 minCounterCleanupAge)。纯函数,切周边界由单测钉住。
func counterCleanupCutoffs(now time.Time, retention time.Duration) (dayCutoff, weekCutoff uint32) {
	cutoff := now.Add(-max(retention, minCounterCleanupAge))
	return gameday.DayKey(cutoff), gameday.WeekKey(cutoff)
}

// cleanupBatch 跑一批清理:候选至多 cleanupBatchSize 行,返回候选行数与实际删掉的行数。
// 出错时已删的行数仍如实返回(每行的点锁 + 点删是各自提交的短事务,删掉的就是删掉了)。
type cleanupBatch func(ctx context.Context) (candidates int, deleted int64, err error)

// cleanupInBatches 反复跑同一类的批,直到某批候选不足 cleanupBatchSize 行、达到 cleanupMaxBatches 批或 ctx 结束。
// 判"还有没有下一批"看候选数而不是删除数:复核落空(0 行)的候选说明那一行已不满足条件,下一次候选读不会再读到它,
// 按删除数判会在"候选满批、个别落空"时提前收工。
func (s *GuildAssetStore) cleanupInBatches(ctx context.Context, table string, batch cleanupBatch) error {
	for i := 0; i < cleanupMaxBatches; i++ {
		candidates, deleted, err := batch(ctx)
		if deleted > 0 {
			recordCleanupDeleted(table, deleted)
		}
		if err != nil {
			return fmt.Errorf("cleanup %s: %w", table, err)
		}
		if candidates < cleanupBatchSize {
			return nil
		}
		timer := time.NewTimer(cleanupBatchPause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

// cleanupTerminalOpsBatch:普通读至多 500 个保留期外的三种终态 op_id(升序),逐行短事务点锁 + 点删并复核条件。
func (s *GuildAssetStore) cleanupTerminalOpsBatch(ctx context.Context, cutoffMs uint64) (int, int64, error) {
	applied := int32(pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED)
	rejected := int32(pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_REJECTED)
	aborted := int32(pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_ABORTED)

	readCtx, cancel := context.WithTimeout(ctx, storeReadBudget)
	opIDs, err := scanOpIDs(readCtx, s.db, sqlListCleanupTerminalOps, applied, rejected, aborted, cutoffMs, cleanupBatchSize)
	cancel()
	if err != nil {
		return 0, 0, fmt.Errorf("list terminal %s: %w", guildAssetOpTable, err)
	}
	var deleted int64
	for _, opID := range opIDs {
		n, err := s.execCleanupDelete(ctx, sqlLockAssetOp, []any{opID},
			sqlCleanupTerminalOp, opID, applied, rejected, aborted, cutoffMs)
		deleted += n
		if err != nil {
			return len(opIDs), deleted, fmt.Errorf("delete terminal %s %d: %w", guildAssetOpTable, opID, err)
		}
	}
	return len(opIDs), deleted, nil
}

// counterKey 是 guild_daily_counter 的完整主键。
type counterKey struct {
	PlayerID         uint64
	Kind             int32
	RefID, PeriodKey uint32
}

// cleanupCountersBatch:普通读至多 500 个 period_key 落在 [floor, cutoff] 的计数行主键(主键升序),逐行短事务点锁 + 点删并复核范围。
//
// 前提:cutoff 来自 counterCleanupCutoffs,从不删到仍可能被在途预留 upsert 写的上一周期行。本短事务不持 seq 行守卫,
// 所以它在计数行上只会遇到退款(sqlRefundCounter,Point_Get,与 sqlLockCleanupCounter 同为 PRIMARY key → 行 key),同序排队;
// 直接传一个更新的 cutoff 进来就会与 upsert 同行相遇(TiDB 各持一半,C5 补遗),也会让被删周期的限购被在途预留绕过一次。
func (s *GuildAssetStore) cleanupCountersBatch(ctx context.Context, floor, cutoff uint32) (int, int64, error) {
	keys, err := s.listCleanupCounters(ctx, floor, cutoff)
	if err != nil {
		return 0, 0, err
	}
	var deleted int64
	for _, k := range keys {
		n, err := s.execCleanupDelete(ctx, sqlLockCleanupCounter, []any{k.PlayerID, k.Kind, k.RefID, k.PeriodKey},
			sqlCleanupCounter, k.PlayerID, k.Kind, k.RefID, k.PeriodKey, floor, cutoff)
		deleted += n
		if err != nil {
			return len(keys), deleted, fmt.Errorf("delete daily counter (player=%d kind=%d ref=%d period=%d): %w",
				k.PlayerID, k.Kind, k.RefID, k.PeriodKey, err)
		}
	}
	return len(keys), deleted, nil
}

func (s *GuildAssetStore) listCleanupCounters(ctx context.Context, floor, cutoff uint32) ([]counterKey, error) {
	ctx, cancel := context.WithTimeout(ctx, storeReadBudget)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, sqlListCleanupCounters, floor, cutoff, cleanupBatchSize)
	if err != nil {
		return nil, fmt.Errorf("list expired daily counters: %w", err)
	}
	defer rows.Close()

	var keys []counterKey
	for rows.Next() {
		var k counterKey
		if err := rows.Scan(&k.PlayerID, &k.Kind, &k.RefID, &k.PeriodKey); err != nil {
			return nil, fmt.Errorf("scan expired daily counter: %w", err)
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate expired daily counters: %w", err)
	}
	return keys, nil
}

// execCleanupDelete 在一个 RC 短事务里删一行:先按完整主键点锁(lockQuery,不带复核条件),再跑带复核条件的主键点删(query),
// 返回删掉的行数(0 或 1)。
//
// 为什么进事务(死锁复核 C5,只在 TiDB 成环):TiDB 默认 pessimistic-auto-commit=false,自动提交的 DELETE 按乐观事务提交,
// prewrite 按 region 并行写行 key、PRIMARY key 与各索引 key(本表行 key 与索引 key 落在不同 region)。计数行清理若与
// 同一旧周期计数行的退款(sqlRefundCounter,悲观 Point_Get:先 PRIMARY key 后行 key)同时在途:退款先锁住 PRIMARY key,
// 清理 prewrite 行 key 成功、在 PRIMARY key 上撞到退款的悲观锁;退款再申请行 key 时撞到清理的乐观锁 —— 这种互等不在 TiKV
// 死锁检测器的等待图里,只能等 1s 锁超时或乐观锁 TTL 过期。前提在业务上可达:商店指令永不中止,背包满 / 离线可以一直 PENDING
// 超过计数保留期,之后被 scene 拒绝或经 assetopfix 终结时退的正是保留期外的旧周期计数行。
// 进了显式事务,两边都先走快路径点锁"PRIMARY key → 行 key",同序排队;MySQL 下锁集不变(聚簇记录 → 本行二级项),
// 只多 SET TRANSACTION / START TRANSACTION / 点锁 / COMMIT 几次往返。终态 op 行没有悲观写者(已证明),只是共用同一个 helper。
//
// 尝试次数为 1:点锁 / 点删失败的行下一轮再删(与之前"不在本轮重试"同一语义)。行已被别的副本删掉 → 点锁读不到 → 0 行。
// 超过 1 行只可能是 WHERE 写坏了(不再是完整主键等值):现在能整体回滚,返回错误。
func (s *GuildAssetStore) execCleanupDelete(ctx context.Context, lockQuery string, lockArgs []any, query string, args ...any) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, cleanupStmtBudget)
	defer cancel()
	var deleted int64
	err := assetop.WithTxRetry(ctx, s.db, 1, isRetryableBackground, func(tx *sql.Tx) error {
		deleted = 0 // 重试契约:结果只在成功返回前写到外层
		found, err := lockRowExists(ctx, tx, lockQuery, lockArgs...)
		if err != nil {
			return fmt.Errorf("lock: %w", err)
		}
		if !found {
			return nil // 已被别的副本删掉
		}
		result, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read rows affected: %w", err)
		}
		if n > 1 {
			return fmt.Errorf("point delete removed %d rows (WHERE is no longer a full primary-key match)", n)
		}
		deleted = n
		return nil
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}
