package data

// 帮会经济的三个写事务(捐献预留 T-D、兑换预留 T-S、升级 T-U)与经济读查询
// (设计 docs/design/guild-phase2/05-economy.md §5.15–§5.21;顶部三个覆盖块、90-consistency part2 §2/§3、
// X-13、X-14 的订正优先于正文)。
//
// 与 asset_store.go 的分工:这里只**发起**资产指令(写 PENDING 行、占次数 / 扣帮贡)与做纯 guild 表的升级;
// 指令的领取、重排、终结与对侧账全部在 asset_store.go(assetop.Store 的实现)。两边共用本文件的列清单与扫描函数,
// 列序只有一个定义点。
//
// 贯穿全文件的纪律:
//
//  1. **写事务只走 GuildRepo.inTx**(90 part2 §2 第 1 条):READ COMMITTED、1213 / 9007 整事务重跑、
//     1205 与子预算到期归一成 ErrWriteConflict、COMMIT 结果不明归一成 ErrWriteConflict。本文件不另写重试助手。
//  2. **锁序**(guild_db.proto 文件头的全序,tables.go 同序):guild → guild_player_state → guild_member →
//     guild_application → guild_player_op_seq → guild_asset_op → guild_daily_counter;同表多行按主键升序逐行取锁。
//     guild 行在 T-D / T-S 里只做非锁定读:它只提供 zone 与等级,锁它会让同帮所有捐献在一把行锁上排队。
//     锁定语句一律是**完整主键等值点操作**(2026-09-21 死锁修复契约 P3):
//     - guild_member 上的锁定读与 UPDATE 带 `FORCE INDEX (PRIMARY)`:WHERE 同时钉死了 uk_guild_member(player_id),
//       被规划到唯一键就是"二级 → 主键"取锁,与离帮 / 被踢 / 解散"按主键删成员行(主键 → 二级)"在同一行上反序;
//     - 需要按二级条件找行时(提前截止)先普通读候选主键,再按主键升序逐行"主键点锁 → 点改",WHERE 带原条件做提交点复核
//       (点锁是 TiDB 的要求,死锁复核 V1,见 asset_store.go 文件头 TiDB 附加规则)。
//     assetop.AllocateSeq 只锁 seq 行(完整主键点查),本纪元未决行是**普通读**(friend 审计 #4 修法 A,2026-09-21 起):
//     T-D / T-S 对 guild_asset_op 除自己插入的新行外不持任何锁,终结(asset_store.go)对 op 行的主键 CAS 与它们之间
//     没有可反序的资源(终结在退次数 / 退限购分支另锁 seq 行,是计数行的守卫,见下一条与 asset_store.go 的 lockCounterparty)。
//     普通读看得全前一个分配者已提交的未决行,靠的是 READ COMMITTED 的"每条语句一份新快照" —— inTx 固定 RC,不得改。
//     seq 行在事务内、锁住本人成员行之后按需建(死锁复核 C2,ensureSeqRowTx):成员行就是 seq 行全部建行者的守卫,
//     首插者回滚时没有排队者,不再有"继承间隙 S、插入意向互挡"的 1213。
//     计数行 guild_daily_counter 的每个悲观写者(本文件的带上限 upsert、终结的退次数 / 退限购)都先持同一 (p, stream) 的
//     seq 行(死锁复核 C6):TiDB 下 IODKU 在语句末尾并行锁 {行 key, PRIMARY key},退款的 Point_Get 是先 PRIMARY key 后行 key,
//     两者一旦同时在途就可能各持一半互等;seq 行把它们排成一列。以后新增计数写者(如 B6 活动计数)同样要先持 seq 行。
//     唯一不持 seq 行的是清理短事务(asset_store.go,Point_Get 点锁 → 点删):它只删截止键(now − max(保留期, 8 天))
//     以前的周期行,本文件的 upsert 只写请求开头 start 所在周期的行,两者行集合不相交(C5 补遗,minCounterCleanupAge);
//     清理与退款同为 Point_Get,同序。计数行上的完整全序见 asset_store.go 文件头。
//  3. **SQL 不写 status / kind / stream / counter_kind / tx_type 的数字字面量**,一律绑定生成常量(90 part2 §3 末行)。
//  4. **合服闸门在事务内**,zone 取本事务读到的 guild.zone_id,不取缓存(缓存合服后最长陈旧一个 TTL)。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	assetpb "proto/common/asset"
	rollbackpb "proto/common/rollback"
	pb "proto/guild"

	"shared/assetop"

	"guild/internal/constants"
)

// 资产两表的表名。唯一事实源是 proto/guild/guild_db.proto 的 OptionTableName;这里是手写 SQL 需要的字面量,
// 改表名要两边同改(schemamigrate 按 proto 建表,漏改的表现是运行期 "table doesn't exist")。
const (
	guildPlayerOpSeqTable = "guild_player_op_seq"
	guildAssetOpTable     = "guild_asset_op"
)

// assetOpColumns 是 guild_asset_op 的全列,**顺序 = GuildAssetOpRecord 的 proto 字段序**(1..28)。
// 插入参数(assetOpInsertArgs)与扫描(scanAssetOpRecord)都按它排;加字段忘了同步三处,
// 表现是"新列永远零值"或运行期 Scan 数量不符,编译期都看不出来 —— TestAssetOpColumnsCoverEveryProtoField 机械比对。
const assetOpColumns = "`op_id`, `player_id`, `stream`, `seq`, `guild_id`, `kind`, `status`, `durable`, " +
	"`attempts`, `next_attempt_ms`, `deadline_ms`, `payload`, `ref_id`, `ref_count`, `period_key`, " +
	"`contribution_delta`, `funds_delta`, `reason_tip_id`, `created_ms`, `updated_ms`, " +
	"`lease_until_ms`, `lease_token`, `tx_type`, `last_outcome`, `last_reason`, `stream_epoch`, " +
	"`resolved_by`, `resolve_reason`"

// assetOpColumnCount 与 assetOpColumns 的列数一致,由单测守住。
const assetOpColumnCount = 28

// sqlInsertAssetOp 显式写全 28 列:payload / resolved_by / resolve_reason 在库里是**可空**列
// (MEDIUMBLOB / MEDIUMTEXT,92-handoff §10.3 第 5 条),漏写就是 NULL,读路径要多一层判空才不出错。
var sqlInsertAssetOp = "INSERT INTO " + guildAssetOpTable + " (" + assetOpColumns + ") VALUES (" +
	placeholders(assetOpColumnCount) + ")"

// economyReadBudget:经济读查询的子预算(90 part2 §2 第 8 条的 1000ms 档;seq 行自死锁复核 C2 起在事务内建,不再用它)。
// 读的都是主键 / 索引点查,跑满 1s 说明库出了状况,尽早把"稍后再试"还给玩家比吃满请求预算好。
const economyReadBudget = 1000 * time.Millisecond

// 计数行带上限 upsert(X-13,取代"锁不存在的行再插入")。
//
// 为什么不先 SELECT … FOR UPDATE 再 INSERT:对不存在的主键加锁读在 RR 下靠间隙锁互等、在 TiDB 下锁不住后续插入
// (01-storage.md §2.1 规则 3);upsert 一条语句就把"占用 / 达上限"判完,也不再需要 1062 重试。
//
// 赋值顺序**不能换**:MySQL 按从左到右求值,后一个赋值看得到前一个赋值的新值。updated_ms 在前,
// 它的条件读到的 used_count 仍是旧值;若把 used_count 放前面,updated_ms 的判断会基于已加过的新值而永远不刷新。
// RowsAffected:1 = 新插入,2 = 已累加,0 = 达上限(值没变)。这依赖 ClientFoundRows=false(WithLockWaitTimeout 强制)。
const sqlUpsertCounterWithLimit = `INSERT INTO guild_daily_counter (player_id, counter_kind, ref_id, period_key, used_count, updated_ms)
VALUES (?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE
  updated_ms = IF(used_count + ? <= ?, ?, updated_ms),
  used_count = IF(used_count + ? <= ?, used_count + ?, used_count)`

const (
	sqlSelectGuildZoneLevel = `SELECT zone_id, level FROM guild WHERE guild_id = ?`
	// guild_member 的两条锁定语句带 FORCE INDEX (PRIMARY):完整主键等值同时钉死了 uk_guild_member(player_id),
	// 优化器若选唯一键,取锁顺序就成了"uk 项 → 聚簇记录",与离帮 / 被踢 / 解散按主键删这一行(聚簇 → uk 项)反序成环
	// (lock-rules H1)。MySQL 与 TiDB 都认这个子句;实际计划由 economy_lock_plan_mysql_test.go 的 EXPLAIN 回归钉住。
	// 帮贡列不在任何二级索引里,UPDATE 只锁聚簇记录这一把锁。
	sqlLockMemberBalance = `SELECT contribution_balance FROM guild_member FORCE INDEX (PRIMARY)
	WHERE guild_id = ? AND player_id = ? FOR UPDATE`
	sqlDebitContribution = `UPDATE guild_member FORCE INDEX (PRIMARY) SET contribution_balance = contribution_balance - ?
	WHERE guild_id = ? AND player_id = ? AND contribution_balance >= ?`

	sqlLockGuildForUpgrade = `SELECT level, funds, zone_id FROM guild WHERE guild_id = ? FOR UPDATE`
	sqlUpgradeGuild        = `UPDATE guild SET level = ?, funds = funds - ?, max_members = ?
	WHERE guild_id = ? AND level = ? AND funds >= ?`
	sqlSelectMemberIDs = `SELECT player_id FROM guild_member WHERE guild_id = ? ORDER BY player_id`

	sqlSelectDonateUsage = `SELECT ref_id, used_count FROM guild_daily_counter
	WHERE player_id = ? AND counter_kind = ? AND period_key = ?`
	sqlSelectShopUsage = `SELECT ref_id, period_key, used_count FROM guild_daily_counter
	WHERE player_id = ? AND counter_kind = ? AND period_key IN (?, ?)`
	sqlSelectMemberContribution = `SELECT contribution_total, contribution_balance FROM guild_member
	WHERE guild_id = ? AND player_id = ?`
	sqlSelectOpState = "SELECT `status`, `last_reason`, `reason_tip_id` FROM " + guildAssetOpTable + " WHERE `op_id` = ?"

	// 待结算:走 idx_guild_asset_op_2 的 (player_id, stream) 前缀;按 (stream_epoch, seq) 排序是 90 part2 §3 的订正 ——
	// 纪元进唯一键之后,只按 seq 排会把库恢复前后两个纪元的行交错在一起。
	sqlSelectPendingOps = "SELECT " + assetOpColumns + " FROM " + guildAssetOpTable +
		" WHERE `player_id` = ? AND `stream` = ? AND `status` = ? ORDER BY `stream_epoch` ASC, `seq` ASC LIMIT ?"
	// 最近结果:沿 uk_guild_asset_op 倒序,最多扫 limit 行,与历史行数无关;含 PENDING,由调用方在 Go 里过滤。
	sqlSelectRecentOps = "SELECT " + assetOpColumns + " FROM " + guildAssetOpTable +
		" WHERE `player_id` = ? AND `stream` = ? ORDER BY `stream_epoch` DESC, `seq` DESC LIMIT ?"

	// 离帮 / 被踢 / 解散的提前截止,拆成"候选普通读 + 主键点改"两步(friend 审计 #5 / #10,见 accelerateDonationDeadlines)。
	// 候选读的 IN 列表按批在运行期拼接(前缀 + 占位符 + 后缀);它**不带任何锁定子句**,走哪个索引都不加行锁。
	sqlSelectAccelerateCandidatesHead = "SELECT `op_id` FROM " + guildAssetOpTable + " WHERE `player_id` IN ("
	sqlSelectAccelerateCandidatesTail = ") AND `stream` = ? AND `status` = ? AND `guild_id` = ? AND `kind` = ?" +
		" AND `deadline_ms` > ? ORDER BY `op_id`"
	// sqlAccelerateDonationDeadline:完整主键等值点改。`status = PENDING AND deadline_ms > now` 是提交点复核:
	// 候选读之后该行已被终结 / 已被提前(或本语句重放),影响 0 行,是 no-op 而不是错误。
	// kind / guild_id / player_id / stream 插入后不可变,候选读判过就够,这里不重复。
	// 只改 deadline_ms(无索引)、next_attempt_ms(在 idx_guild_asset_op_0)、updated_ms(无索引):
	// 取锁顺序是"聚簇记录 → idx_0 项",与终结 / 人工终结 / 重排 / 毒行 / 领取 / 清理点删同向。
	// 调用方在它之前先对同一 op_id 跑 sqlLockAssetOp(V1,TiDB 下让同一行的悲观写者先在 PRIMARY key 上排队)。
	sqlAccelerateDonationDeadline = "UPDATE " + guildAssetOpTable +
		" SET `deadline_ms` = ?, `next_attempt_ms` = LEAST(`next_attempt_ms`, ?), `updated_ms` = ?" +
		" WHERE `op_id` = ? AND `status` = ? AND `deadline_ms` > ?"

	// sqlSeqRowExists:ensureSeqRowTx 的前置普通读(完整主键等值,不带锁定子句;RC 下不取行锁)。
	// seq 行建出后永不删除,常态直接命中、不必再发 INSERT IGNORE。
	sqlSeqRowExists = "SELECT 1 FROM " + guildPlayerOpSeqTable + " WHERE `player_id` = ? AND `stream` = ?"

	// sqlEnsureSeqRow:事务内建 seq 行(死锁复核 C2,见 ensureSeqRowTx)。
	// **与 shared/assetop.EnsureSeqRow 的建行语句逐字同义**(列、next_seq 从 1 起、纪元 = 建行时刻毫秒、已存在即空操作不改纪元)。
	// 这是一次有记录的 DRY 偏离(AGENTS §11 取舍序:正确性 > 可维护):assetop 目前只有吃 *sql.DB 的自动提交版建行,
	// 而消掉 C2 的环必须在守卫行锁之下、在同一事务里建行;本会话无权改 go/shared。assetop 提供事务版建行
	// (EnsureSeqRowTx)之后,删掉本常量、ensureSeqRowTx 改调它即可。两边不许悄悄分叉:
	// TestEnsureSeqRowTx_MatchesAssetopEnsureSeqRow 在真库上逐列比对两条路径建出的行。
	sqlEnsureSeqRow = "INSERT IGNORE INTO " + guildPlayerOpSeqTable + " (player_id, stream, next_seq, epoch, updated_ms) VALUES (?, ?, 1, ?, ?)"
)

// FenceFunc:合服闸门。nil = 不设闸;返回 ErrZoneMerging(或包它)= 拒绝。zone 一律取事务内读到的 guild.zone_id。
type FenceFunc func(ctx context.Context, zoneID uint32) error

// GuildSeqTables 返回帮会的 seq 表 / 指令表与"未决"的库值。表名经 assetop 的正则校验(它们会被拼进 SQL)。
func GuildSeqTables() (assetop.SeqTables, error) {
	return assetop.NewSeqTables(guildPlayerOpSeqTable, guildAssetOpTable,
		uint32(pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING))
}

// EconomyRepo 是经济写事务与读查询的入口。它持有 *GuildRepo 以复用 inTx 与提交后缓存失效
// (90 part2 §2 第 1 条),不另起连接池、不另写事务基座。
//
// 线程模型:构造后只读,可被多个请求 goroutine 共享。
type EconomyRepo struct {
	guilds *GuildRepo
	db     *sql.DB
	seq    assetop.SeqTables
}

// NewEconomyRepo。guilds 为 nil 时报错:没有它就没有 inTx,也就没有统一的隔离级与重试语义。
func NewEconomyRepo(guilds *GuildRepo) (*EconomyRepo, error) {
	if guilds == nil {
		return nil, errors.New("guild economy repo: nil GuildRepo")
	}
	seq, err := GuildSeqTables()
	if err != nil {
		return nil, fmt.Errorf("guild economy repo: %w", err)
	}
	return &EconomyRepo{guilds: guilds, db: guilds.db, seq: seq}, nil
}

// ── 共用行结构与扫描 ─────────────────────────────────────────

// AssetOpRow 是一行资产指令的只读视图(读 RPC 与 assetopfix 用)。枚举列已转成生成类型。
type AssetOpRow struct {
	OpID, PlayerID, GuildID              uint64
	Stream                               assetpb.AssetOpStream
	Seq, StreamEpoch                     uint64
	Kind                                 pb.GuildAssetOpKind
	Status                               pb.GuildAssetOpStatus
	Attempts                             uint32
	NextAttemptMs, DeadlineMs            uint64
	RefID, RefCount, PeriodKey           uint32
	ContributionDelta, FundsDelta        uint64
	ReasonTipID, LastOutcome, LastReason uint32
	CreatedMs, UpdatedMs                 uint64
	Payload                              []byte
}

// rowScanner 是 *sql.Row 与 *sql.Rows 的公共扫描接口。
type rowScanner interface {
	Scan(dest ...any) error
}

// scanAssetOpRecord 按 assetOpColumns 的列序扫一整行。
//
// 枚举列先扫成 int32 再转型,不依赖 database/sql 对具名整数类型的反射转换;
// resolved_by / resolve_reason 可空,扫进 sql.NullString;payload 可空,[]byte 对 NULL 天然得到 nil。
func scanAssetOpRecord(row rowScanner) (*pb.GuildAssetOpRecord, error) {
	rec := &pb.GuildAssetOpRecord{}
	var (
		kind, status              int32
		resolvedBy, resolveReason sql.NullString
	)
	if err := row.Scan(
		&rec.OpId, &rec.PlayerId, &rec.Stream, &rec.Seq, &rec.GuildId,
		&kind, &status, &rec.Durable, &rec.Attempts, &rec.NextAttemptMs, &rec.DeadlineMs,
		&rec.Payload, &rec.RefId, &rec.RefCount, &rec.PeriodKey,
		&rec.ContributionDelta, &rec.FundsDelta, &rec.ReasonTipId, &rec.CreatedMs, &rec.UpdatedMs,
		&rec.LeaseUntilMs, &rec.LeaseToken, &rec.TxType, &rec.LastOutcome, &rec.LastReason,
		&rec.StreamEpoch, &resolvedBy, &resolveReason,
	); err != nil {
		return nil, err
	}
	rec.Kind = pb.GuildAssetOpKind(kind)
	rec.Status = pb.GuildAssetOpStatus(status)
	rec.ResolvedBy = resolvedBy.String
	rec.ResolveReason = resolveReason.String
	return rec, nil
}

// assetOpInsertArgs 按 assetOpColumns 的列序给出插入参数。枚举列按整数绑定(列类型是 int)。
func assetOpInsertArgs(rec *pb.GuildAssetOpRecord) []any {
	return []any{
		rec.GetOpId(), rec.GetPlayerId(), rec.GetStream(), rec.GetSeq(), rec.GetGuildId(),
		int32(rec.GetKind()), int32(rec.GetStatus()), rec.GetDurable(), rec.GetAttempts(),
		rec.GetNextAttemptMs(), rec.GetDeadlineMs(), rec.GetPayload(), rec.GetRefId(), rec.GetRefCount(),
		rec.GetPeriodKey(), rec.GetContributionDelta(), rec.GetFundsDelta(), rec.GetReasonTipId(),
		rec.GetCreatedMs(), rec.GetUpdatedMs(), rec.GetLeaseUntilMs(), rec.GetLeaseToken(), rec.GetTxType(),
		rec.GetLastOutcome(), rec.GetLastReason(), rec.GetStreamEpoch(),
		rec.GetResolvedBy(), rec.GetResolveReason(),
	}
}

func insertAssetOp(ctx context.Context, tx *sql.Tx, rec *pb.GuildAssetOpRecord) error {
	if _, err := tx.ExecContext(ctx, sqlInsertAssetOp, assetOpInsertArgs(rec)...); err != nil {
		return fmt.Errorf("insert %s %d: %w", guildAssetOpTable, rec.GetOpId(), err)
	}
	return nil
}

func assetOpRowFrom(rec *pb.GuildAssetOpRecord) AssetOpRow {
	return AssetOpRow{
		OpID:              rec.GetOpId(),
		PlayerID:          rec.GetPlayerId(),
		GuildID:           rec.GetGuildId(),
		Stream:            assetpb.AssetOpStream(rec.GetStream()),
		Seq:               rec.GetSeq(),
		StreamEpoch:       rec.GetStreamEpoch(),
		Kind:              rec.GetKind(),
		Status:            rec.GetStatus(),
		Attempts:          rec.GetAttempts(),
		NextAttemptMs:     rec.GetNextAttemptMs(),
		DeadlineMs:        rec.GetDeadlineMs(),
		RefID:             rec.GetRefId(),
		RefCount:          rec.GetRefCount(),
		PeriodKey:         rec.GetPeriodKey(),
		ContributionDelta: rec.GetContributionDelta(),
		FundsDelta:        rec.GetFundsDelta(),
		ReasonTipID:       rec.GetReasonTipId(),
		LastOutcome:       rec.GetLastOutcome(),
		LastReason:        rec.GetLastReason(),
		CreatedMs:         rec.GetCreatedMs(),
		UpdatedMs:         rec.GetUpdatedMs(),
		Payload:           rec.GetPayload(),
	}
}

// queryAssetOpRows 跑一条返回 assetOpColumns 全列的查询。
func queryAssetOpRows(ctx context.Context, q queryer, what, query string, args ...any) ([]AssetOpRow, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	defer rows.Close()

	var out []AssetOpRow
	for rows.Next() {
		rec, err := scanAssetOpRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: scan: %w", what, err)
		}
		out = append(out, assetOpRowFrom(rec))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: iterate: %w", what, err)
	}
	return out, nil
}

// ── 事务内的共用步骤 ─────────────────────────────────────────

// readGuildZoneLevel 非锁定读帮会的 zone 与等级(RC 读最新已提交版本)。无行 → ErrGuildGone。
func readGuildZoneLevel(ctx context.Context, tx *sql.Tx, guildID uint64) (zoneID, level uint32, err error) {
	err = tx.QueryRowContext(ctx, sqlSelectGuildZoneLevel, guildID).Scan(&zoneID, &level)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, ErrGuildGone
	}
	if err != nil {
		return 0, 0, fmt.Errorf("read zone and level of guild %d: %w", guildID, err)
	}
	return zoneID, level, nil
}

// checkFence 调合服闸门。闸门返回的任何错误都按"拒绝"处理:读不出合服状态时放行,
// 合服期间的一笔资产写就可能在源区与目标区各记一份(fail-closed,与 B2 §11.1 同口径)。
// 非 ErrZoneMerging 的错误包成 ErrZoneMerging,原因留在文本里,logic 统一映射成 GuildZoneMerging。
func checkFence(ctx context.Context, fence FenceFunc, zoneID uint32) error {
	if fence == nil {
		return nil
	}
	err := fence(ctx, zoneID)
	if err == nil || errors.Is(err, ErrZoneMerging) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrZoneMerging, err)
}

// upsertCounterWithLimit 占用计数行 n 次;超过 limit 时一次都不占,返回 limited=true。
//
// 硬前提(死锁复核 C6):调用方已持有 guild_player_op_seq(p, 该计数所属的流)的 X —— 捐献计数属 GUILD_DEBIT、商店限购属
// GUILD_CREDIT(T-D / T-S 里 AllocateSeq 已锁住它)。计数行的全部悲观写者(这里与终结的 refundCounter)都先在这一行上串行:
// TiDB 下本语句在语句末尾把 {行 key, PRIMARY key} 按 region 并行加锁,可能先拿到行 key、PRIMARY key 还在途;
// 退款是 Point_Get,先 PRIMARY key 后行 key —— 两者同时在途就各持一半互等(1213)。以后新增计数写者也必须先持 seq 行。
// periodKey 必须是本请求开头 start 的周期键:清理不持 seq 行,它与本语句不相遇只靠"清理截止键至少早 24h"(minCounterCleanupAge),
// 拿一个更早的时刻算周期键写进来会破坏这个前提。
func upsertCounterWithLimit(ctx context.Context, tx *sql.Tx, playerID uint64, kind pb.GuildDailyCounterKind,
	refID, periodKey, n, limit uint32, nowMs uint64) (limited bool, err error) {
	result, err := tx.ExecContext(ctx, sqlUpsertCounterWithLimit,
		playerID, int32(kind), refID, periodKey, n, nowMs,
		n, limit, nowMs,
		n, limit, n)
	if err != nil {
		return false, fmt.Errorf("occupy daily counter (player=%d kind=%d ref=%d period=%d): %w",
			playerID, int32(kind), refID, periodKey, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("occupy daily counter (player=%d ref=%d): read rows affected: %w", playerID, refID, err)
	}
	switch affected {
	case 1, 2:
		return false, nil
	case 0:
		return true, nil
	default:
		// 主键 upsert 不可能动到 >2 行;真出现说明 ClientFoundRows 之类的会话语义被改了,自检必须暴露它。
		return false, fmt.Errorf("occupy daily counter (player=%d ref=%d): unexpected rows affected %d", playerID, refID, affected)
	}
}

// accelerateDonationDeadlines 把这些玩家在本帮的**未决捐献**截止时间提前到 now(05 §5.20,D2 覆盖第 4 条保留)。
// 由离帮 / 被踢 / 解散三个事务调用,放在删成员行、删申请之后(成员行 X 锁仍持有到提交,捐献预留照样被挡在外面;
// 2026-09-21 死锁修复把删成员前移,原 X-14 的"删申请之后、删成员之前"已不成立);锁序 guild_member → guild_asset_op。
//
// 只动 deadline_ms / next_attempt_ms / updated_ms:**不抢租约**(lease_until_ms 不动),同步投递还握着租约的行
// 要等租约到期才被循环领走;next_attempt_ms 取 LEAST 是为了把退避中的行拉回"现在",而不是把已到期的行推后。
// deadline_ms > now 过滤掉已经到期的行,重放是 no-op。只作用于 DONATE:商店 / 活动发奖的物品属于玩家,照常投递。
// IN 占位符按 100 分块(X-14:成员上限 100,与删申请同一口径)。
//
// # 为什么是"候选普通读 + 主键升序逐行点改",而不是一条多条件 UPDATE(2026-09-21 死锁修复,friend 审计 #5 / #10)
//
// 旧写法 `UPDATE … WHERE player_id IN (…) AND stream = ? AND status = ? AND guild_id = ? AND kind = ? AND deadline_ms > ?`
// 的候选路径全是二级索引(uk、idx_1、idx_2、idx_0),经二级索引的 UPDATE 没有 semi-consistent read,扫到的每一行都
// "先二级项、后聚簇记录"真的等锁 —— 与终结 CAS / 重排 / 毒行 / 清理 DELETE 的"先聚簇、后改二级项"在同一 op 行上反序:
// 走 idx_2 时撞 Finalize(DONATE, REJECTED/ABORTED) 与本玩家历史终态行的清理,走 idx_1 撞全帮的终态行清理,
// 走 idx_0 则锁遍全服未决行、与任意一行的重排成环。锁集由执行计划决定,测试库(小表)与线上还不一样。
//
// 现在两步:
//  1. 候选读:同样的条件做**普通读**(RC 语句级快照,不加任何行锁;TiDB 同样不锁),只取 op_id;
//  2. 把全部批次的 op_id 合并、去重、升序,逐行先 sqlLockAssetOp 主键点锁、再 sqlAccelerateDonationDeadline 点改
//     (完整主键等值;MySQL 下两条锁同一条聚簇记录,点改再 delete-mark / 插入它的 idx_0 项)。
//     先点锁是为 TiDB(死锁复核 V1):点改带复核谓词、不走 Point_Get 快路径,语句末尾并行锁 {行 key, PRIMARY key, uk key},
//     与同一行上捐献被拒 / 中止的终结、重排、毒行推迟可能各持一半;点锁让所有写者先在 PRIMARY key 上排队
//     (详见 asset_store.go 文件头 TiDB 附加规则)。MySQL 下锁集不变。
//
// 候选集为什么是完整的(正确性前提,调用方必须满足):三个调用方在调用前**已持有**这些 (guild_id, player_id) 的
// guild_member 行 X 锁(被踢:lockMemberPair 锁目标;离帮:锁本人;解散:锁全体成员),而 ReserveDonation 插入
// PENDING DONATE 行之前必先 `FOR UPDATE` 锁同一成员行 —— 候选读之后不可能再冒出新的未决捐献;候选读本身发生在
// 拿到成员锁之后,RC 下看得见此前已提交的全部行。候选读与点改之间行可能被终结或已被提前:点改的 WHERE 复核
// status / deadline_ms,影响 0 行即跳过,不做"恰好一行"的自检。行只会离开 PENDING,没有路径把它改回来。
//
// now 必须 > 0:deadline_ms = 0 在本表的语义是"永不中止",传 0 会把所有未决捐献改成永不中止 —— fail-closed 拒绝。
func accelerateDonationDeadlines(ctx context.Context, tx *sql.Tx, guildID uint64, playerIDs []uint64, now uint64) error {
	if now == 0 {
		return fmt.Errorf("accelerate donation deadlines of guild %d: now must be > 0", guildID)
	}
	opIDs, err := selectAccelerateCandidates(ctx, tx, guildID, playerIDs, now)
	if err != nil {
		return err
	}
	for _, opID := range opIDs {
		// 先主键点锁、再带复核条件点改(V1,理由见 asset_store.go 文件头 TiDB 附加规则)。读不到行 = 候选读之后已被清理
		// (只删终态行),与点改影响 0 行同义,跳过。
		found, err := lockRowExists(ctx, tx, sqlLockAssetOp, opID)
		if err != nil {
			return fmt.Errorf("lock %s %d for deadline acceleration (guild %d): %w", guildAssetOpTable, opID, guildID, err)
		}
		if !found {
			continue
		}
		if _, err := tx.ExecContext(ctx, sqlAccelerateDonationDeadline,
			now, now, now, opID, PendingStatus(), now); err != nil {
			return fmt.Errorf("accelerate donation deadline of %s %d (guild %d): %w", guildAssetOpTable, opID, guildID, err)
		}
	}
	return nil
}

// selectAccelerateCandidates 普通读这些玩家在本帮、本条件下的未决捐献 op_id,返回去重后的升序列表。
//
// 每批的游标在下一批查询之前就读完并关闭:同一个 *sql.Tx 只有一条连接,游标没关就发下一条语句会被驱动拒绝;
// 也正因为全部候选先读完、再统一排序,跨批次的点改才是全局按 op_id 升序(P2)。
func selectAccelerateCandidates(ctx context.Context, tx *sql.Tx, guildID uint64, playerIDs []uint64, now uint64) ([]uint64, error) {
	const chunkSize = 100
	var opIDs []uint64
	for start := 0; start < len(playerIDs); start += chunkSize {
		batch := playerIDs[start:min(start+chunkSize, len(playerIDs))]
		args := make([]any, 0, len(batch)+5)
		for _, playerID := range batch {
			args = append(args, playerID)
		}
		args = append(args,
			uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT),
			PendingStatus(),
			guildID,
			int32(pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE),
			now)
		query := sqlSelectAccelerateCandidatesHead + placeholders(len(batch)) + sqlSelectAccelerateCandidatesTail
		found, err := scanOpIDs(ctx, tx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("select pending donations of %d members of guild %d: %w", len(batch), guildID, err)
		}
		opIDs = append(opIDs, found...)
	}
	slices.Sort(opIDs)
	return slices.Compact(opIDs), nil
}

// scanOpIDs 跑一条只返回 op_id 一列的查询,读完即关游标。
func scanOpIDs(ctx context.Context, q queryer, query string, args ...any) ([]uint64, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []uint64
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan op_id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate op_id: %w", err)
	}
	return ids, nil
}

// ── T-D 捐献预留 ─────────────────────────────────────────────

// DonationReserve 是捐献预留的全部输入。时间与令牌全部由 logic 给(显式依赖),repo 不读墙钟。
type DonationReserve struct {
	OpID, PlayerID, GuildID     uint64
	DonateID                    uint32
	MinGuildLevel               uint32
	ContributionGain, FundsGain uint64
	DailyLimit                  uint32
	PeriodKey                   uint32 // gameday.DayKey(start)
	DeadlineMs                  uint64 // start + asset_op_deadline_seconds*1000
	LeaseUntilMs                uint64 // start + Lease
	LeaseToken                  uint64 // 非 0
	NowMs                       uint64
	Payload                     []byte // proto.Marshal(AssetBundle{Currencies:[{currency, cost}]}),非空
	Fence                       FenceFunc
}

// Reserved 是捐献预留的结果:本次分到的 (纪元, 流水号),同步投递要原样带给 scene。
type Reserved struct{ Seq, StreamEpoch uint64 }

// validate 在碰库之前拒掉畸形输入。这些都是调用方的编程错误(不是玩家可触发的拒绝),回内部错误。
// DailyLimit / DeadlineMs 为 0 尤其要拒:前者会让 upsert 永远判"达上限",后者在本表表示"永不中止"。
func (in DonationReserve) validate() error {
	switch {
	case in.OpID == 0 || in.PlayerID == 0 || in.GuildID == 0:
		return fmt.Errorf("reserve donation: ids must be non-zero (op=%d player=%d guild=%d)", in.OpID, in.PlayerID, in.GuildID)
	case in.NowMs == 0 || in.LeaseToken == 0:
		return fmt.Errorf("reserve donation op %d: now and lease token must be non-zero", in.OpID)
	case in.LeaseUntilMs <= in.NowMs || in.DeadlineMs <= in.NowMs:
		return fmt.Errorf("reserve donation op %d: lease (%d) and deadline (%d) must be after now (%d)",
			in.OpID, in.LeaseUntilMs, in.DeadlineMs, in.NowMs)
	case in.DailyLimit == 0 || in.PeriodKey == 0:
		return fmt.Errorf("reserve donation op %d: daily limit (%d) and period key (%d) must be non-zero",
			in.OpID, in.DailyLimit, in.PeriodKey)
	case len(in.Payload) == 0:
		return fmt.Errorf("reserve donation op %d: empty payload", in.OpID)
	}
	return nil
}

// ReserveDonation 捐献预留事务 T-D(05 §5.16,X-13 upsert 订正)。
//
// 事务 op=donate,锁序 guild(普通读)→ guild_member → seq(缺行先在事务内插)→ seq FOR UPDATE → op → counter:
// 读帮会 zone / 等级 → 等级门槛 → 合服闸门 → 锁本人成员行 → 按需建 seq 行(GUILD_DEBIT,ensureSeqRowTx)→ 分 seq →
// 写 PENDING 行(28 列全写)→ 占今日次数。
// 错误语义:ErrGuildGone / ErrGuildLevelTooLow / ErrZoneMerging / ErrNotGuildMember /
// 包了 assetop.ErrTooManyPending 的错误(errors.Is 可判)/ ErrDonateLimit / ErrWriteConflict;其余为内部错误。
// 任何拒绝都整体回滚:seq 不前进、行不落、次数不占(首次建出的 seq 行也随之回滚,下次再建,纪元取那一次的时刻)。
func (r *EconomyRepo) ReserveDonation(ctx context.Context, in DonationReserve) (Reserved, error) {
	if err := in.validate(); err != nil {
		return Reserved{}, err
	}
	const stream = assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT

	var out Reserved
	err := r.guilds.inTx(ctx, opDonate, func(ctx context.Context, tx *sql.Tx) error {
		zoneID, level, err := readGuildZoneLevel(ctx, tx, in.GuildID)
		if err != nil {
			return err
		}
		if level < in.MinGuildLevel {
			return ErrGuildLevelTooLow
		}
		if err := checkFence(ctx, in.Fence, zoneID); err != nil {
			return err
		}
		// 本人成员行加锁:同一玩家的并发捐献在这里串行,也让离帮事务(删这一行)与本事务互斥 ——
		// 离帮先提交,这里读不到行;本事务先提交,离帮事务里的提前截止能看到这一行。
		// 这一步是 accelerateDonationDeadlines 候选普通读"完整"的前提:插 PENDING DONATE 行之前必先持有成员行锁。
		// 以后若新增任何插入 DONATE 行的路径,也必须先锁同一成员行,否则离帮的提前截止会漏行。
		var role uint32
		err = tx.QueryRowContext(ctx, sqlLockMemberRole, in.GuildID, in.PlayerID).Scan(&role)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotGuildMember
		}
		if err != nil {
			return fmt.Errorf("lock donating member %d of guild %d: %w", in.PlayerID, in.GuildID, err)
		}
		// seq 行在成员行锁之下建(C2):同一 (p, DEBIT) 的建行者都已在这把锁上串行,见 ensureSeqRowTx。
		if err := ensureSeqRowTx(ctx, tx, in.PlayerID, stream, in.NowMs); err != nil {
			return err
		}

		alloc, err := assetop.AllocateSeq(ctx, tx, r.seq, in.PlayerID, stream, assetop.DefaultLimits, in.NowMs)
		if err != nil {
			return err // ErrTooManyPending 已由 assetop 用 %w 包好,原样上抛
		}
		if err := insertAssetOp(ctx, tx, &pb.GuildAssetOpRecord{
			OpId:              in.OpID,
			PlayerId:          in.PlayerID,
			Stream:            uint32(stream),
			Seq:               alloc.Seq,
			GuildId:           in.GuildID,
			Kind:              pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE,
			Status:            pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING,
			NextAttemptMs:     in.LeaseUntilMs, // 同步投递握着租约;租约到期前循环不会碰它
			DeadlineMs:        in.DeadlineMs,
			Payload:           in.Payload,
			RefId:             in.DonateID,
			RefCount:          1, // 捐献恒 1 次
			PeriodKey:         in.PeriodKey,
			ContributionDelta: in.ContributionGain,
			FundsDelta:        in.FundsGain,
			CreatedMs:         in.NowMs,
			UpdatedMs:         in.NowMs,
			LeaseUntilMs:      in.LeaseUntilMs,
			LeaseToken:        in.LeaseToken,
			TxType:            uint32(rollbackpb.TransactionType_TX_GUILD_DONATE),
			StreamEpoch:       alloc.Epoch,
		}); err != nil {
			return err
		}
		limited, err := upsertCounterWithLimit(ctx, tx, in.PlayerID, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_DONATE,
			in.DonateID, in.PeriodKey, 1, in.DailyLimit, in.NowMs)
		if err != nil {
			return err
		}
		if limited {
			return ErrDonateLimit
		}
		out = Reserved{Seq: alloc.Seq, StreamEpoch: alloc.Epoch}
		return nil
	})
	if err != nil {
		return Reserved{}, err
	}
	return out, nil
}

// ── T-S 兑换预留 ─────────────────────────────────────────────

// ShopReserve 是兑换预留的全部输入。Count 已由 logic 按 MaxBuyCount 校验;Cost = cost_contribution × Count。
type ShopReserve struct {
	OpID, PlayerID, GuildID         uint64
	GoodsID                         uint32
	Count                           uint32 // ≥1,logic 已按 MaxBuyCount 校验
	RequiredGuildLevel              uint32
	Cost                            uint64 // cost_contribution * Count
	LimitCount                      uint32 // 0 = 不限购
	PeriodKey                       uint32 // limit_period==0 时为 0
	LeaseUntilMs, LeaseToken, NowMs uint64
	Payload                         []byte // AssetBundle{Items:[{item_id, item_count*Count}]}
	Fence                           FenceFunc
}

// ShopReserved 是兑换预留的结果;BalanceAfter 是扣帮贡之后的可用余额(回包直接用,不再读一次)。
type ShopReserved struct{ Seq, StreamEpoch, BalanceAfter uint64 }

// validate 同 DonationReserve.validate。Cost 必须 > 0:扣 0 的 UPDATE 在 ClientFoundRows=false 下
// RowsAffected 为 0,"恰好 1 行"的写入自检会把一次合法兑换判成帮贡不足;配表校验已保证 cost_contribution ≥ 1。
// LimitCount 与 PeriodKey 必须同为 0 或同非 0:只有一边非 0 意味着要么占不到计数行、要么终结时退一个不存在的计数。
func (in ShopReserve) validate() error {
	switch {
	case in.OpID == 0 || in.PlayerID == 0 || in.GuildID == 0:
		return fmt.Errorf("reserve shop order: ids must be non-zero (op=%d player=%d guild=%d)", in.OpID, in.PlayerID, in.GuildID)
	case in.NowMs == 0 || in.LeaseToken == 0 || in.LeaseUntilMs <= in.NowMs:
		return fmt.Errorf("reserve shop order op %d: now / lease token / lease until invalid", in.OpID)
	case in.Count == 0 || in.Cost == 0:
		return fmt.Errorf("reserve shop order op %d: count (%d) and cost (%d) must be non-zero", in.OpID, in.Count, in.Cost)
	case (in.LimitCount == 0) != (in.PeriodKey == 0):
		return fmt.Errorf("reserve shop order op %d: limit count (%d) and period key (%d) must be both zero or both non-zero",
			in.OpID, in.LimitCount, in.PeriodKey)
	case len(in.Payload) == 0:
		return fmt.Errorf("reserve shop order op %d: empty payload", in.OpID)
	}
	return nil
}

// ReserveShopOrder 兑换预留事务 T-S(05 §5.17,X-13 upsert 订正)。
//
// 事务 op=shop,锁序 guild(普通读)→ guild_member → seq(缺行先在事务内插,GUILD_CREDIT,ensureSeqRowTx)→
// seq FOR UPDATE → op → counter,最后回到已锁的成员行扣帮贡(同一行再次 UPDATE 不算新加锁)。
// 商店指令 deadline_ms 恒 0:物品属于玩家,永不中止。
// 错误语义:ErrShopLimit / ErrGuildGone / ErrGuildLevelTooLow / ErrZoneMerging / ErrNotGuildMember /
// ErrContributionInsufficient / 包了 assetop.ErrTooManyPending 的错误 / ErrWriteConflict;其余为内部错误。
func (r *EconomyRepo) ReserveShopOrder(ctx context.Context, in ShopReserve) (ShopReserved, error) {
	if err := in.validate(); err != nil {
		return ShopReserved{}, err
	}
	// 纯判断先做(AGENTS §11.3):一次买的份数就超过周期限购,不必碰库。首次插入计数行时 upsert
	// 不会拦截 count > limit(VALUES 直接写入),所以这一刀不能省。
	if in.LimitCount > 0 && in.Count > in.LimitCount {
		return ShopReserved{}, ErrShopLimit
	}
	const stream = assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT

	var out ShopReserved
	err := r.guilds.inTx(ctx, opShop, func(ctx context.Context, tx *sql.Tx) error {
		zoneID, level, err := readGuildZoneLevel(ctx, tx, in.GuildID)
		if err != nil {
			return err
		}
		if level < in.RequiredGuildLevel {
			return ErrGuildLevelTooLow
		}
		if err := checkFence(ctx, in.Fence, zoneID); err != nil {
			return err
		}
		var balance uint64
		err = tx.QueryRowContext(ctx, sqlLockMemberBalance, in.GuildID, in.PlayerID).Scan(&balance)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotGuildMember
		}
		if err != nil {
			return fmt.Errorf("lock buying member %d of guild %d: %w", in.PlayerID, in.GuildID, err)
		}
		if balance < in.Cost {
			return ErrContributionInsufficient
		}
		// seq 行在成员行锁之下建(C2),理由同 ReserveDonation。
		if err := ensureSeqRowTx(ctx, tx, in.PlayerID, stream, in.NowMs); err != nil {
			return err
		}

		alloc, err := assetop.AllocateSeq(ctx, tx, r.seq, in.PlayerID, stream, assetop.DefaultLimits, in.NowMs)
		if err != nil {
			return err
		}
		if err := insertAssetOp(ctx, tx, &pb.GuildAssetOpRecord{
			OpId:              in.OpID,
			PlayerId:          in.PlayerID,
			Stream:            uint32(stream),
			Seq:               alloc.Seq,
			GuildId:           in.GuildID,
			Kind:              pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_SHOP,
			Status:            pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING,
			NextAttemptMs:     in.LeaseUntilMs,
			DeadlineMs:        0, // R7:商店永不中止
			Payload:           in.Payload,
			RefId:             in.GoodsID,
			RefCount:          in.Count,
			PeriodKey:         in.PeriodKey,
			ContributionDelta: in.Cost, // 已扣帮贡;永久拒绝时按它退回
			CreatedMs:         in.NowMs,
			UpdatedMs:         in.NowMs,
			LeaseUntilMs:      in.LeaseUntilMs,
			LeaseToken:        in.LeaseToken,
			TxType:            uint32(rollbackpb.TransactionType_TX_GUILD_SHOP),
			StreamEpoch:       alloc.Epoch,
		}); err != nil {
			return err
		}
		if in.LimitCount > 0 {
			limited, err := upsertCounterWithLimit(ctx, tx, in.PlayerID, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_SHOP,
				in.GoodsID, in.PeriodKey, in.Count, in.LimitCount, in.NowMs)
			if err != nil {
				return err
			}
			if limited {
				return ErrShopLimit
			}
		}
		// 行已锁、余额已判,这条理论上必中;不中说明锁内读到的行在写的时候变了,不能提交一份"扣了个寂寞"的订单。
		result, err := tx.ExecContext(ctx, sqlDebitContribution, in.Cost, in.GuildID, in.PlayerID, in.Cost)
		if err != nil {
			return fmt.Errorf("debit contribution of member %d in guild %d: %w", in.PlayerID, in.GuildID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("debit contribution of member %d in guild %d: read rows affected: %w", in.PlayerID, in.GuildID, err)
		}
		if affected != 1 {
			return ErrContributionInsufficient
		}
		out = ShopReserved{Seq: alloc.Seq, StreamEpoch: alloc.Epoch, BalanceAfter: balance - in.Cost}
		return nil
	})
	if err != nil {
		return ShopReserved{}, err
	}
	// 帮贡在成员快照里:缓存不失效,玩家会在一个 TTL 内看到兑换前的余额。
	r.guilds.invalidateAfterCommit(ctx, opShop, in.GuildID)
	return out, nil
}

// ensureSeqRowTx 在 T-D / T-S 事务内、**已持本人成员行 X 锁之后**,按需建 seq 行(死锁复核 C2,根除 G-C3 的 seq 行形态)。
//
// 旧写法是事务外自动提交 INSERT IGNORE:首个插入者在提交前被回滚(连接被 KILL、刷盘失败)时,排在它未提交记录上做
// 重复键检查的两个同键 INSERT IGNORE,其 S 锁在 RC 下照样被继承成后继记录前那段间隙上的间隙 S(查重锁带 inherit_all),
// 随后各自的插入意向锁被对方挡住,InnoDB 报 1213(手册 "Locks Set by Different SQL Statements" 三会话例)。
// 前置普通读挡不住首次建行的竞态 —— RC 下别人未提交的插入对它不可见。
//
// 守卫不变量:guild_player_op_seq 的每个建行者都先持 p 当前所在帮的 guild_member(G,p) X(ReserveDonation / ReserveShopOrder,
// sqlLockMemberRole / sqlLockMemberBalance)。p 同一时刻只在一个帮(uk(player_id));离帮 / 被踢 / 解散删这一行之前都要等它;
// 对旧帮发起的预留在 RC 下锁不到行(不存在或已删除标记)、直接回 ErrNotGuildMember,走不到这里。所以同一 (p, ·) 的在途
// 建行者至多一个:首插者回滚(业务拒绝、子预算到期、1213 重跑都会)时它的记录上没有排队者,不会留下继承的间隙 S。
// 本表不再有任何 S 锁(没有并发查重),RC 下 X 锁不被继承,插入意向锁永远不用等。新增的只有"成员行 → seq 行"这一条边,
// 与表间全序 P1 同向。TiDB 下是 PRIMARY key 与新行 key 的 X 悲观锁,同键竞争者都已在成员行上串行。
// **以后新增任何建 seq 行或调 AllocateSeq 的路径(如 B6 活动发奖要分配 CREDIT 流的 seq),都必须先持同一成员行锁**,
// 否则事务内插入者一回滚,排在它后面的查重 S 锁就会被继承成间隙 S,并发建行者互等 1213 —— 而事务内回滚远比自动提交常见。
//
// 先普通读:行建出后永不删除,常态直接命中、不发写语句;拿到成员行锁之后读,前一个持锁者已提交的行在 RC 下一定看得见。
// 缺行才 INSERT IGNORE(语句与 assetop.EnsureSeqRow 同义,见 sqlEnsureSeqRow 的 DRY 偏离说明;纪元 = 建行时刻毫秒)。
// 错误原样上抛,由 inTx 统一分类:1213 / 9007 整事务重跑,1205 与子预算到期归一成 ErrWriteConflict。
//
// 滚动升级窗口:旧版本副本的事务外自动提交建行不经过成员行守卫,新旧版本混跑期间原环仍可能出现;发布须停服切换或逐区切换。
func ensureSeqRowTx(ctx context.Context, tx *sql.Tx, playerID uint64, stream assetpb.AssetOpStream, nowMs uint64) error {
	var exists int
	switch err := tx.QueryRowContext(ctx, sqlSeqRowExists, playerID, uint32(stream)).Scan(&exists); {
	case err == nil:
		return nil
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("read %s row (player=%d stream=%d): %w", guildPlayerOpSeqTable, playerID, int32(stream), err)
	}
	// 与 assetop.EnsureSeqRow 同一条前置:纪元必须为正(调用方的 validate 已拒掉 NowMs == 0,这里是接缝上的第二道)。
	if nowMs == 0 {
		return fmt.Errorf("ensure %s row (player=%d stream=%d): epoch must be positive (nowMs=0)",
			guildPlayerOpSeqTable, playerID, int32(stream))
	}
	if _, err := tx.ExecContext(ctx, sqlEnsureSeqRow, playerID, uint32(stream), nowMs, nowMs); err != nil {
		return fmt.Errorf("ensure %s row (player=%d stream=%d): %w", guildPlayerOpSeqTable, playerID, int32(stream), err)
	}
	return nil
}

// ── T-U 升级 ─────────────────────────────────────────────────

// LevelLookup:按等级取 GuildLevel 行;ok=false = 配表缺行(→ ErrGuildLevelConfigMissing)。
type LevelLookup func(level uint32) (upgradeCostFunds uint64, maxMembers uint32, ok bool)

// UpgradeResult 是升级事务的结果。
type UpgradeResult struct {
	Changed   bool
	NewLevel  uint32
	MemberIDs []uint64 // Changed 时:升级后全体成员(推送收件人,调用方自己剔除操作者)
}

// UpgradeGuild 升级事务 T-U(05 §5.18)。只动 guild 的 MySQL,不经资产通道。
//
// 事务 op=upgrade,锁序 guild(FOR UPDATE)→ guild_member(本人,FOR UPDATE):
// 职位 < 长老 → ErrRankTooLow;合服闸门;expectedLevel != 0 且与当前等级不同 → 无改动提交(Changed=false,
// NewLevel = 当前等级),挡住重复点击连升两级;当前等级行 cost == 0 → ErrGuildMaxLevel;下一级行缺 →
// ErrGuildLevelConfigMissing;资金不足 → ErrFundsInsufficient。扣的是**当前等级行**的 upgrade_cost_funds
// (语义:"升到下一级所需资金"),新成员上限取下一级行的 max_members。
// 提交后 Changed 时失效帮会快照缓存;expected_level 对不上时**也**失效(见下方 staleView)。
func (r *EconomyRepo) UpgradeGuild(ctx context.Context, guildID, playerID uint64, expectedLevel uint32, levels LevelLookup, fence FenceFunc) (UpgradeResult, error) {
	if levels == nil {
		return UpgradeResult{}, fmt.Errorf("upgrade guild %d: level lookup not injected", guildID)
	}

	var (
		out UpgradeResult
		// staleView:expected_level 与库里等级不符,说明调用方看到的是过期快照。典型来源是上一次升级已提交但
		// COMMIT 回执丢失(classifyCommitErr 归为 ErrWriteConflict,当时没失效缓存)。这一支不扣钱,但必须顺手
		// 失效缓存,否则玩家在一个 TTL 内一直看到升级前的等级与资金 —— 幂等只兜住了"不扣第二次",没兜住视图。
		staleView bool
	)
	err := r.guilds.inTx(ctx, opUpgrade, func(ctx context.Context, tx *sql.Tx) error {
		var (
			level  uint32
			funds  uint64
			zoneID uint32
		)
		err := tx.QueryRowContext(ctx, sqlLockGuildForUpgrade, guildID).Scan(&level, &funds, &zoneID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrGuildGone
		}
		if err != nil {
			return fmt.Errorf("lock guild %d for upgrade: %w", guildID, err)
		}
		var role uint32
		err = tx.QueryRowContext(ctx, sqlLockMemberRole, guildID, playerID).Scan(&role)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotGuildMember
		}
		if err != nil {
			return fmt.Errorf("lock upgrading member %d of guild %d: %w", playerID, guildID, err)
		}
		// 比 Rank 不比 role 原值:role 编码不连续,`role >= RoleOfficer` 会把未知编码一起放进来。
		if constants.Rank(role) < constants.RankOfficer {
			return ErrRankTooLow
		}
		if err := checkFence(ctx, fence, zoneID); err != nil {
			return err
		}
		if expectedLevel != 0 && expectedLevel != level {
			out, staleView = UpgradeResult{Changed: false, NewLevel: level}, true
			return nil
		}

		cost, _, ok := levels(level)
		if !ok {
			return ErrGuildLevelConfigMissing
		}
		if cost == 0 {
			return ErrGuildMaxLevel
		}
		_, nextMaxMembers, ok := levels(level + 1)
		if !ok {
			return ErrGuildLevelConfigMissing
		}
		if funds < cost {
			return ErrFundsInsufficient
		}
		if err := execExactlyOneRow(ctx, tx, fmt.Sprintf("upgrade guild %d from level %d", guildID, level),
			sqlUpgradeGuild, level+1, cost, nextMaxMembers, guildID, level, cost); err != nil {
			return err
		}

		memberIDs, err := selectMemberIDs(ctx, tx, guildID)
		if err != nil {
			return err
		}
		// 重试契约:结果只在成功返回前赋给外层变量(§6.2)。
		out, staleView = UpgradeResult{Changed: true, NewLevel: level + 1, MemberIDs: memberIDs}, false
		return nil
	})
	if err != nil {
		return UpgradeResult{}, err
	}
	// 过期视图这一支极少走到,多一次 INCR + DEL 的代价可以忽略;LEVEL_UP 推送仍只在 Changed 时由调用方发。
	if out.Changed || staleView {
		r.guilds.invalidateAfterCommit(ctx, opUpgrade, guildID)
	}
	return out, nil
}

// selectMemberIDs 非锁定读该帮全部成员 id(player_id 升序),作推送收件人。
func selectMemberIDs(ctx context.Context, tx *sql.Tx, guildID uint64) ([]uint64, error) {
	rows, err := tx.QueryContext(ctx, sqlSelectMemberIDs, guildID)
	if err != nil {
		return nil, fmt.Errorf("list members of guild %d: %w", guildID, err)
	}
	defer rows.Close()

	var ids []uint64
	for rows.Next() {
		var playerID uint64
		if err := rows.Scan(&playerID); err != nil {
			return nil, fmt.Errorf("scan member of guild %d: %w", guildID, err)
		}
		ids = append(ids, playerID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate members of guild %d: %w", guildID, err)
	}
	return ids, nil
}

// ── 读(不加锁,1000ms 子预算)──────────────────────────────

// DonateUsage 返回本人某游戏日各捐献选项已用次数:donate_id → used_count。没有计数行的选项不在 map 里(即 0 次)。
func (r *EconomyRepo) DonateUsage(ctx context.Context, playerID uint64, dayKey uint32) (map[uint32]uint32, error) {
	ctx, cancel := context.WithTimeout(ctx, economyReadBudget)
	defer cancel()
	rows, err := r.db.QueryContext(ctx, sqlSelectDonateUsage,
		playerID, int32(pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_DONATE), dayKey)
	if err != nil {
		return nil, fmt.Errorf("read donate usage of player %d: %w", playerID, err)
	}
	defer rows.Close()

	out := make(map[uint32]uint32)
	for rows.Next() {
		var refID, used uint32
		if err := rows.Scan(&refID, &used); err != nil {
			return nil, fmt.Errorf("scan donate usage of player %d: %w", playerID, err)
		}
		out[refID] = used
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate donate usage of player %d: %w", playerID, err)
	}
	return out, nil
}

// ShopUsageKey 是商店用量的键:同一商品只可能按日或按周之一限购,但键里带上周期键,
// 调用方不必知道哪个商品是哪种周期就能直接查。
type ShopUsageKey struct{ GoodsID, PeriodKey uint32 }

// ShopUsage 返回本人本游戏日 / 本游戏周的商店用量。日键 8 位、周键 6 位,数值域不重叠,一条 IN 就够。
func (r *EconomyRepo) ShopUsage(ctx context.Context, playerID uint64, dayKey, weekKey uint32) (map[ShopUsageKey]uint32, error) {
	ctx, cancel := context.WithTimeout(ctx, economyReadBudget)
	defer cancel()
	rows, err := r.db.QueryContext(ctx, sqlSelectShopUsage,
		playerID, int32(pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_SHOP), dayKey, weekKey)
	if err != nil {
		return nil, fmt.Errorf("read shop usage of player %d: %w", playerID, err)
	}
	defer rows.Close()

	out := make(map[ShopUsageKey]uint32)
	for rows.Next() {
		var key ShopUsageKey
		var used uint32
		if err := rows.Scan(&key.GoodsID, &key.PeriodKey, &used); err != nil {
			return nil, fmt.Errorf("scan shop usage of player %d: %w", playerID, err)
		}
		out[key] = used
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate shop usage of player %d: %w", playerID, err)
	}
	return out, nil
}

// PendingOps 返回本人某条流上的未决指令,按 (stream_epoch, seq) 升序,至多 limit 条。limit <= 0 返回空。
func (r *EconomyRepo) PendingOps(ctx context.Context, playerID uint64, stream assetpb.AssetOpStream, limit int) ([]AssetOpRow, error) {
	if limit <= 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, economyReadBudget)
	defer cancel()
	return queryAssetOpRows(ctx, r.db, fmt.Sprintf("list pending asset ops of player %d", playerID),
		sqlSelectPendingOps, playerID, uint32(stream), PendingStatus(), limit)
}

// RecentOps 返回本人某条流上最新的 limit 条指令(含 PENDING),按 (stream_epoch, seq) 倒序。
// 状态 / 时间窗 / kind / 帮会过滤由调用方在 Go 里做:SQL 只负责"最多扫 limit 行"。
func (r *EconomyRepo) RecentOps(ctx context.Context, playerID uint64, stream assetpb.AssetOpStream, limit int) ([]AssetOpRow, error) {
	if limit <= 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, economyReadBudget)
	defer cancel()
	return queryAssetOpRows(ctx, r.db, fmt.Sprintf("list recent asset ops of player %d", playerID),
		sqlSelectRecentOps, playerID, uint32(stream), limit)
}

// MemberContribution 直读 MySQL 的帮贡两列(不走缓存:兑换刚扣完帮贡时缓存可能还没失效)。found=false = 不是该帮成员。
func (r *EconomyRepo) MemberContribution(ctx context.Context, guildID, playerID uint64) (total, balance uint64, found bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, economyReadBudget)
	defer cancel()
	err = r.db.QueryRowContext(ctx, sqlSelectMemberContribution, guildID, playerID).Scan(&total, &balance)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, fmt.Errorf("read contribution of member %d in guild %d: %w", playerID, guildID, err)
	}
	return total, balance, true, nil
}

// OpState 是同步投递之后回读的单行状态:结局以库为准,不以 ProcessOne 的返回值为准
// (后者在租约被接管、落库失败时都可能与库不一致)。
type OpState struct {
	Status      pb.GuildAssetOpStatus
	LastReason  uint32
	ReasonTipID uint32
}

// OpState 按主键回读一行的状态。found=false = 行不存在。
func (r *EconomyRepo) OpState(ctx context.Context, opID uint64) (OpState, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, economyReadBudget)
	defer cancel()
	var (
		status int32
		state  OpState
	)
	err := r.db.QueryRowContext(ctx, sqlSelectOpState, opID).Scan(&status, &state.LastReason, &state.ReasonTipID)
	if errors.Is(err, sql.ErrNoRows) {
		return OpState{}, false, nil
	}
	if err != nil {
		return OpState{}, false, fmt.Errorf("read state of %s %d: %w", guildAssetOpTable, opID, err)
	}
	state.Status = pb.GuildAssetOpStatus(status)
	return state, true, nil
}
