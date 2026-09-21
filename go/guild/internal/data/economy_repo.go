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
//  2. **锁序**:guild → guild_member → guild_player_op_seq → guild_asset_op → guild_daily_counter(tables.go 的顺序)。
//     guild 行在 T-D / T-S 里只做非锁定读:它只提供 zone 与等级,锁它会让同帮所有捐献在一把行锁上排队。
//  3. **SQL 不写 status / kind / stream / counter_kind / tx_type 的数字字面量**,一律绑定生成常量(90 part2 §3 末行)。
//  4. **合服闸门在事务内**,zone 取本事务读到的 guild.zone_id,不取缓存(缓存合服后最长陈旧一个 TTL)。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

// economyReadBudget:经济读查询与事务外 autocommit 语句的子预算(90 part2 §2 第 8 条的 1000ms 档)。
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
	sqlLockMemberBalance    = `SELECT contribution_balance FROM guild_member WHERE guild_id = ? AND player_id = ? FOR UPDATE`
	sqlDebitContribution    = `UPDATE guild_member SET contribution_balance = contribution_balance - ?
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
// 由离帮 / 被踢 / 解散三个事务调用,放在删申请之后、删成员之前(X-14);锁序 guild_member → guild_asset_op。
//
// 只动 deadline_ms / next_attempt_ms / updated_ms:**不抢租约**(lease_until_ms 不动),同步投递还握着租约的行
// 要等租约到期才被循环领走;next_attempt_ms 取 LEAST 是为了把退避中的行拉回"现在",而不是把已到期的行推后。
// deadline_ms > now 过滤掉已经到期的行,重放本语句是 no-op。只作用于 DONATE:商店 / 活动发奖的物品属于玩家,照常投递。
// IN 占位符按 100 分块(X-14:成员上限 100,与 deleteApplicationsOfPlayers 同一口径)。
//
// now 必须 > 0:deadline_ms = 0 在本表的语义是"永不中止",传 0 会把所有未决捐献改成永不中止 —— fail-closed 拒绝。
func accelerateDonationDeadlines(ctx context.Context, tx *sql.Tx, guildID uint64, playerIDs []uint64, now uint64) error {
	if now == 0 {
		return fmt.Errorf("accelerate donation deadlines of guild %d: now must be > 0", guildID)
	}
	const chunkSize = 100
	for start := 0; start < len(playerIDs); start += chunkSize {
		batch := playerIDs[start:min(start+chunkSize, len(playerIDs))]
		args := make([]any, 0, len(batch)+8)
		args = append(args, now, now, now)
		for _, playerID := range batch {
			args = append(args, playerID)
		}
		args = append(args,
			uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT),
			PendingStatus(),
			guildID,
			int32(pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE),
			now)
		query := "UPDATE " + guildAssetOpTable +
			" SET `deadline_ms` = ?, `next_attempt_ms` = LEAST(`next_attempt_ms`, ?), `updated_ms` = ?" +
			" WHERE `player_id` IN (" + placeholders(len(batch)) + ")" +
			" AND `stream` = ? AND `status` = ? AND `guild_id` = ? AND `kind` = ? AND `deadline_ms` > ?"
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("accelerate donation deadlines of %d members of guild %d: %w", len(batch), guildID, err)
		}
	}
	return nil
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
// 事务外先 EnsureSeqRow(GUILD_DEBIT);事务 op=donate,锁序 guild(非锁定读)→ guild_member → seq → op → counter:
// 读帮会 zone / 等级 → 等级门槛 → 合服闸门 → 锁本人成员行 → 分 seq → 写 PENDING 行(28 列全写)→ 占今日次数。
// 错误语义:ErrGuildGone / ErrGuildLevelTooLow / ErrZoneMerging / ErrNotGuildMember /
// 包了 assetop.ErrTooManyPending 的错误(errors.Is 可判)/ ErrDonateLimit / ErrWriteConflict;其余为内部错误。
// 任何拒绝都整体回滚:seq 不前进、行不落、次数不占。
func (r *EconomyRepo) ReserveDonation(ctx context.Context, in DonationReserve) (Reserved, error) {
	if err := in.validate(); err != nil {
		return Reserved{}, err
	}
	const stream = assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT
	if err := r.ensureSeqRow(ctx, in.PlayerID, stream, in.NowMs); err != nil {
		return Reserved{}, err
	}

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
		var role uint32
		err = tx.QueryRowContext(ctx, sqlLockMemberRole, in.GuildID, in.PlayerID).Scan(&role)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotGuildMember
		}
		if err != nil {
			return fmt.Errorf("lock donating member %d of guild %d: %w", in.PlayerID, in.GuildID, err)
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
// 事务外先 EnsureSeqRow(GUILD_CREDIT);事务 op=shop,锁序 guild(非锁定读)→ guild_member → seq → op → counter,
// 最后回到已锁的成员行扣帮贡(同一行再次 UPDATE 不算新加锁)。商店指令 deadline_ms 恒 0:物品属于玩家,永不中止。
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
	if err := r.ensureSeqRow(ctx, in.PlayerID, stream, in.NowMs); err != nil {
		return ShopReserved{}, err
	}

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

// ensureSeqRow 在事务**外**(autocommit)建 seq 行:对不存在的行"加锁读再插入"会让并发的首次请求死锁。
func (r *EconomyRepo) ensureSeqRow(ctx context.Context, playerID uint64, stream assetpb.AssetOpStream, nowMs uint64) error {
	ctx, cancel := context.WithTimeout(ctx, economyReadBudget)
	defer cancel()
	if err := assetop.EnsureSeqRow(ctx, r.db, r.seq, playerID, stream, nowMs); err != nil {
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
