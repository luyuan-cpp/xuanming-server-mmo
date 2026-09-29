package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	assetpb "proto/common/asset"
	tradepb "proto/trade"

	"shared/assetop"

	"github.com/go-sql-driver/mysql"
	"google.golang.org/protobuf/proto"
)

// 编译前置(写下本文件时未满足):本文件用到 TradeAssetOpRecord 的 resolved_by(22)/
// resolve_reason(23)。这两个字段只在 proto/trade/trade_table.proto 里,生成物还停在
// 字段 21 —— 直接 go build 会报 `rec.ResolvedBy undefined`,那不是代码写错,是**没跑
// 生成**。先跑仓库既有的 proto 生成(dev.bat proto),再 build/vet/test。
// 生成会改到:go/proto/trade/trade_table.pb.go、cpp/generated/proto/trade/trade_table.pb.{h,cc}、
// generated/proto/{_unified,db,login}/proto/trade/trade_table.proto;robot/vendor/proto/trade/
// trade_table.pb.go 是 `replace proto => ../go/proto` 的 vendor 副本,按仓库惯例一并同步。

// 表名。唯一事实源仍是 proto/trade/trade_table.proto 的 OptionTableName;这里是手写 SQL
// 需要的字面量,两边必须一致。改表名要同改这两处(schemamigrate 会按 proto 的名字建表,
// 改漏一边的表现是"表建出来了但所有查询报 table doesn't exist",启动期查不出来)。
const (
	AssetOpSeqTableName = "trade_player_op_seq"
	AssetOpTableName    = "trade_asset_op"
)

// assetOpColumns 是 outbox 行的全列,顺序即 scanAssetOp / InsertOp 的参数顺序。
const assetOpColumns = "`op_id`, `player_id`, `stream`, `stream_epoch`, `seq`, `kind`, `status`, `durable`, " +
	"`attempts`, `next_attempt_ms`, `deadline_ms`, `lease_until_ms`, `lease_token`, `tx_type`, " +
	"`ref_kind`, `ref_id`, `payload`, `last_outcome`, `last_reason`, `created_ms`, `updated_ms`, " +
	"`resolved_by`, `resolve_reason`"

// PendingStatus 是 outbox 的"待办"状态数值,给 assetop 的 seq 分配与重投循环用。
// 取 proto 枚举而不是写 0 / 1:§4.43 #23 明确 assetop.Status 不绑库值,库值由业务表自己定。
func PendingStatus() uint32 { return uint32(tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_PENDING) }

// AssetOpRepo 是通用资产通道 outbox(trade_asset_op)与 seq 分配器(trade_player_op_seq)的
// MySQL 实现,同时满足 assetop.Store。
//
// 契约:
//   - 除 InsertOp / EnsureSeqRowsTx 之外的方法自带 opTimeout 上限,调用方 ctx 更早到期时以 ctx 为准;
//   - InsertOp / EnsureSeqRowsTx 只在调用方给的事务里执行,不自带超时(事务的预算由调用方统一给);
//   - 返回的 error 一律是存储故障,唯一的例外是 Claim 的 assetop.ErrPoisonRow(payload 解不开)。
type AssetOpRepo struct {
	db        *sql.DB
	opTimeout time.Duration
	tables    assetop.SeqTables
}

var (
	_ assetop.Store          = (*AssetOpRepo)(nil)
	_ assetop.ManualResolver = (*AssetOpRepo)(nil)
)

// NewAssetOpRepo。opTimeout 是每次调用的上限(constants.StoreOpTimeout),必须 > 0。
// 表名经 assetop.NewSeqTables 校验(正则 ^[a-z][a-z0-9_]{0,62}$),防止拼接注入。
func NewAssetOpRepo(db *sql.DB, opTimeout time.Duration) (*AssetOpRepo, error) {
	tables, err := assetop.NewSeqTables(AssetOpSeqTableName, AssetOpTableName, PendingStatus())
	if err != nil {
		return nil, fmt.Errorf("trade: 资产 outbox 表名非法: %w", err)
	}
	return &AssetOpRepo{db: db, opTimeout: opTimeout, tables: tables}, nil
}

// DB 暴露连接池,供调用方用 assetop.WithTxRetry 开业务事务(seq 分配必须与业务写同事务)。
func (r *AssetOpRepo) DB() *sql.DB { return r.db }

// Tables 返回 seq / outbox 的表名对,供 assetop.AllocateSeq / EnsureSeqRowTx 使用。
func (r *AssetOpRepo) Tables() assetop.SeqTables { return r.tables }

func (r *AssetOpRepo) bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, r.opTimeout)
}

// ---------------------------------------------------------------------------
// seq 行的建行守卫(2026-09-21 死锁审计 #16 在 trade 的同形,2026-09-28 根治)
// ---------------------------------------------------------------------------
//
// # trade 的全局取锁顺序(唯一事实源;新增任何写路径先回来对一遍)
//
//	trade_listing → trade_order(P3 未落) → trade_player_op_seq 的哨兵守卫行 (0, 0)
//	  → trade_player_op_seq 的玩家行 (player_id, stream) → trade_asset_op
//
// 落码时(2026-09-28)对全部写路径逐条核对的结论:
//   - InsertListing / InsertFavorite / DeleteFavorite(listing_repo.go):单表单语句,整份文件
//     一条 FOR UPDATE 都没有,也不在同一事务里碰 seq / outbox —— 不参与本条全序。
//   - EnqueueEscrowDebit(reconcile/pipeline.go):**唯一**同时碰 seq 与 outbox 的事务。顺序是
//     EnsureSeqRowsTx(普通读 →(仅缺行时)守卫行 FOR UPDATE → INSERT IGNORE)
//     → AllocateSeq(玩家 seq 行 FOR UPDATE + 未决行普通读)→ InsertOp(插 outbox 行)。
//   - Finalize / ResolveManually:事务里只有一条 trade_asset_op 的主键 CAS,不碰 seq 表。
//   - Claim / Reschedule / markPoison:自动提交的单行主键 UPDATE(RC,见 svc.BuildDSN)。
//   - ListDue / OldestPendingCreatedMs / getOp:不加锁读。
//
// **没有任何路径在拿到守卫行之后再去取业务行锁,也没有任何路径先拿玩家 seq 行再拿守卫行**,
// 所以本次新增的那一条边("守卫行 → 玩家 seq 行")与全序同向 —— 不是用新环换旧环。
// P3 落订单与卖家账时,商品 / 订单行的锁必须排在 EnsureSeqRowsTx **之前**(全序第一、二段)。
//
// # 为什么要守卫行(旧写法哪里错)
//
// 旧写法在**业务事务之外**用自动提交 INSERT IGNORE 建 seq 行。seq 行建出后永不删除(本服务对
// trade_player_op_seq 没有任何 DELETE),所以并发补行的后到者拿 S 看到的是**已提交**的重复键、
// 判重即结束,不会升 X。只剩一种 1213:首个插入者在提交前回滚(连接被 KILL、刷盘失败、实例关闭、
// 上层 ctx 到期),排在它那条未提交记录后面做重复键检查的多个 INSERT 会同时继承到同一段间隙上的锁,
// 随后各自申请插入意向锁、被对方的间隙锁挡住,InnoDB 牺牲其一。
//
// 有界重试能把它吸收掉(旧代码就是这么兜的),但**能从 SQL 层消掉的环不该用重试兜底**:让所有首次
// 建行者先排在一行**已存在、已提交**的守卫行上,同一时刻最多一个插入者,排队的是守卫行上的记录锁;
// 守卫持有者回滚只是放锁,不会把锁继承成间隙锁。环被消掉,正确性不再取决于重试次数够不够。
//
// # 守卫行选谁:哨兵行 (player_id=0, stream=0)
//
// trade 没有天然的 per-player 行可锁(帮会用的是 guild_member(G, p),玩家同一时刻只在一个帮):
//   - trade_listing 主键是 listing_id,同一卖家可以并发上架两件商品,锁两件不同的商品行串行不了同一玩家;
//   - trade_favorite 主键是 (player_id, listing_id),不保证存在,且与上架毫无业务关系;
//   - trade_order 要到 P3 才有,而且同样是 per-order。
//
// 所以守卫行取 seq 表自己的哨兵行 (player_id=0, stream=0):
//   - player_id=0 不可能是真玩家:PlayerId 由 login 的 bwmarrin/snowflake 发,不发 0(AGENTS §7 不变量 1);
//   - stream=0 是 ASSET_OP_STREAM_UNSPECIFIED,不是 trade 独占的两条流之一,assetop 永不给它分配 seq;
//   - 全仓对 trade_player_op_seq 只有三条语句,**全是完整主键等值**:assetop.AllocateSeq 的
//     `FOR UPDATE` 点查与 `UPDATE ... WHERE player_id = ? AND stream = ?` 点更新,以及本文件的
//     sqlSeqRowExists 普通读。没有任何 COUNT / MIN / 范围扫描 / 巡检会把哨兵行算进去
//     (grep 证据:`trade_player_op_seq` 与 `AssetOpSeqTableName` 的全部出现处)。
//     **新增任何按 stream 分组或全表聚合的查询时必须显式排除 (0, 0)**,并补一条断言。
//
// 代价:所有**首次**建行全局串行在这一行上,且持锁到业务事务提交。这只发生在"某玩家某条流的第一笔"
// (每玩家每流一次,行建出后永不再走这条分支),不是稳态路径;稳态下普通读直接命中,连守卫行都不碰。
//
// # 滚动升级窗口
//
// 旧版本副本仍在事务外自动提交建行,不经守卫。新旧混跑期间原环仍可能出现(与 guild 同一条结论),
// 所以本项要停服切换或按 zone 逐个切换,不能新旧副本长期并存。
const (
	// seqGuardPlayerID / seqGuardStream 是哨兵守卫行的完整主键。理由见上文。
	seqGuardPlayerID uint64                = 0
	seqGuardStream   assetpb.AssetOpStream = assetpb.AssetOpStream_ASSET_OP_STREAM_UNSPECIFIED

	// sqlSeqRowExists 是 EnsureSeqRowsTx 的前置**普通读**(完整主键等值,不带任何锁定子句;RC 下不取行锁)。
	// seq 行建出后永不删除,常态直接命中、不必再发 INSERT,也就不必去碰守卫行。
	sqlSeqRowExists = "SELECT 1 FROM " + AssetOpSeqTableName + " WHERE `player_id` = ? AND `stream` = ?"

	// sqlLockSeqGuardRow 是守卫行的**锁定点查**:完整主键等值 + FOR UPDATE(EXPLAIN 应为
	// key=PRIMARY、key_len=12 = bigint unsigned 8 + int unsigned 4,两列都 NOT NULL 所以没有空值字节)。
	// RC 下未命中不取间隙锁,所以"哨兵行不存在"只能 fail-closed(ErrSeqGuardRowMissing),
	// 绝不退回"事务外自动提交建行" —— 那会把审计 #16 的环原样装回去。
	sqlLockSeqGuardRow = sqlSeqRowExists + " FOR UPDATE"
)

// ErrSeqGuardRowMissing:哨兵守卫行不在库里。启动期 bootstrap(svc.ServiceContext.StartAssetChannel
// 调 EnsureSeqGuardRow)失败或被跳过时会看到它。fail-closed:首次上架失败,而不是绕过守卫建行。
var ErrSeqGuardRowMissing = errors.New("trade: trade_player_op_seq 的哨兵守卫行 (player_id=0, stream=0) 不存在")

// SeqKey 是一条 seq 行的键。取名不叫 SeqRow 是因为它只是键,不含 next_seq / epoch。
type SeqKey struct {
	PlayerID uint64
	Stream   assetpb.AssetOpStream
}

// EnsureSeqGuardRow 建哨兵守卫行 (0, 0)。**启动期 bootstrap 专用**,不在请求路径上调。
//
// 何时建:服务启动、schemamigrate 建表之后(go/trade/trade.go 的 ensureSchema 先于
// svcCtx.StartAssetChannel)。刻意不放进 schemamigrate:那一层只按 proto 出 DDL、不播种业务行,
// 放进去会让"补一行数据"和"改表结构"共用一份需要人工确认的迁移台账。
//
// 为什么这里可以用有界重试(而守卫路径不许):它自己就是"事务外同键并发插入 + 首插者回滚"那一种
// 1213 —— 守卫行自己没有守卫可排,这是 InnoDB 固有、SQL 层去不掉的情形。收敛论证:语句幂等
// (行已存在是空操作,不改纪元)、跑在自动提交里、没有任何副作用要复位,所以整体重跑安全;牺牲者重跑时
// 要么撞上胜者已提交的行(空操作成功),要么自己插入成功 —— 两种都终止。退避带 ±20% 抖动且可取消
// (assetop.EnsureSeqRowRetry 用 DefaultTxRetryConfig:3 次、10ms 起),ctx 到期立刻返回。
// 这一行永不删除,所以整个集群生命周期里这条重试路径至多在"第一批 trade 副本同时启动"时被走到。
func (r *AssetOpRepo) EnsureSeqGuardRow(ctx context.Context, nowMs uint64) error {
	ctx, cancel := r.bounded(ctx)
	defer cancel()
	if err := assetop.EnsureSeqRowRetry(ctx, r.db, r.tables, seqGuardPlayerID, seqGuardStream, nowMs, IsRetryableTxError); err != nil {
		return fmt.Errorf("建 %s 的哨兵守卫行 (player=%d stream=%d) 失败: %w",
			AssetOpSeqTableName, seqGuardPlayerID, int32(seqGuardStream), err)
	}
	return nil
}

// EnsureSeqRowsTx 在**调用方的业务事务内**按需建 seq 行:先普通读,缺行才锁守卫行 + 建行。
//
// 契约(违反即把死锁装回去):
//   - 调用方事务必须是 READ COMMITTED(assetop.WithTxRetry 的默认值)。普通读要看得见前一个
//     建行者已提交的行,靠的正是 RC 的"每条语句一份新快照";RR 下快照固定在事务第一条普通读,
//     会看不见而多走一次(无害但白锁守卫行)。
//   - **必须在本事务的第一次 assetop.AllocateSeq 之前调用,并把本事务要用到的全部 (player, stream)
//     一次传进来**。这不是风格问题:keys 分两次传的话,第二次很可能发生在已经拿到某个玩家 seq 行 X 锁
//     之后 —— 于是出现"持玩家 seq 行、等守卫行"的事务,与"持守卫行、等玩家 seq 行"的事务正好反序成环。
//     P3 的交付要同时给买家 CREDIT 和卖家 CREDIT,正是这种两个 key 的形状,所以这里收成一次调用、
//     由本函数保证"守卫行永远排在任何玩家 seq 行之前"。
//   - 调用方不得把哨兵键 (0, 0) 混进 keys:那等于拿守卫行当业务流用。这里直接拒。
//
// 返回 ErrSeqGuardRowMissing 表示 bootstrap 没做成(见 EnsureSeqGuardRow),本次业务写失败。
func (r *AssetOpRepo) EnsureSeqRowsTx(ctx context.Context, tx *sql.Tx, nowMs uint64, keys ...SeqKey) error {
	if tx == nil {
		return errors.New("trade: EnsureSeqRowsTx 必须在调用方的业务事务里执行(tx 为 nil)")
	}
	missing := make([]SeqKey, 0, len(keys))
	for _, k := range keys {
		if k.PlayerID == seqGuardPlayerID || k.Stream == seqGuardStream {
			return fmt.Errorf("trade: 非法的 seq 行键 (player=%d stream=%d):player_id=0 与 stream=0 是哨兵守卫行的键",
				k.PlayerID, int32(k.Stream))
		}
		var exists int
		switch err := tx.QueryRowContext(ctx, sqlSeqRowExists, k.PlayerID, uint32(k.Stream)).Scan(&exists); {
		case err == nil:
			continue // 常态:行早已建出,不碰守卫行、不发写语句
		case errors.Is(err, sql.ErrNoRows):
			missing = append(missing, k)
		default:
			return fmt.Errorf("读 %s 行 (player=%d stream=%d): %w", AssetOpSeqTableName, k.PlayerID, int32(k.Stream), err)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	// 守卫行必须在**任何**建行之前拿到:所有首次建行者由此串行,插入者一回滚也只是放掉一把记录锁。
	var guard int
	switch err := tx.QueryRowContext(ctx, sqlLockSeqGuardRow, seqGuardPlayerID, uint32(seqGuardStream)).Scan(&guard); {
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w:启动期 bootstrap 未完成(svc.ServiceContext.StartAssetChannel 调 "+
			"EnsureSeqGuardRow,失败会打 ERROR 日志)。本次不绕过守卫建行:那会把并发首次建行的 1213 装回去", ErrSeqGuardRowMissing)
	default:
		return fmt.Errorf("锁 %s 的哨兵守卫行失败: %w", AssetOpSeqTableName, err)
	}

	for _, k := range missing {
		// 语句与纪元语义由 assetop 独占(ensureSeqRowFormat):INSERT IGNORE、next_seq 从 1 起、
		// 已存在是空操作且不改纪元。这里不抄第二份 —— 两份迟早分叉,而分叉的表现是纪元对不上、
		// scene 把整条流当成需要重置。
		if err := assetop.EnsureSeqRowTx(ctx, tx, r.tables, k.PlayerID, k.Stream, nowMs); err != nil {
			return fmt.Errorf("建 %s 行 (player=%d stream=%d): %w", AssetOpSeqTableName, k.PlayerID, int32(k.Stream), err)
		}
	}
	return nil
}

// InsertOp 在调用方的事务里插入一行 outbox。主键冲突按故障返回:op_id 来自号段,撞号说明
// 发号源出了问题,绝不静默覆盖(同 InsertListing 的理由)。
func (r *AssetOpRepo) InsertOp(ctx context.Context, tx *sql.Tx, rec *tradepb.TradeAssetOpRecord) error {
	_, err := tx.ExecContext(ctx,
		"INSERT INTO "+AssetOpTableName+" ("+assetOpColumns+") VALUES "+
			"(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		rec.GetOpId(), rec.GetPlayerId(), rec.GetStream(), rec.GetStreamEpoch(), rec.GetSeq(),
		int32(rec.GetKind()), int32(rec.GetStatus()), rec.GetDurable(),
		rec.GetAttempts(), rec.GetNextAttemptMs(), rec.GetDeadlineMs(), rec.GetLeaseUntilMs(), rec.GetLeaseToken(),
		rec.GetTxType(), int32(rec.GetRefKind()), rec.GetRefId(), rec.GetPayload(),
		rec.GetLastOutcome(), rec.GetLastReason(), rec.GetCreatedMs(), rec.GetUpdatedMs(),
		// 新行一律是空留痕:人工终结只由 ResolveManually 写,插入路径绝不预置操作人。
		rec.GetResolvedBy(), rec.GetResolveReason(),
	)
	if err != nil {
		return fmt.Errorf("insert %s %d: %w", AssetOpTableName, rec.GetOpId(), err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// assetop.Store
// ---------------------------------------------------------------------------

// ListDue 非加锁一致性读,只取主键,走 (status, next_attempt_ms) 索引。
// 不用范围 UPDATE 领取:那会在 RR 下加 next-key 锁,与业务事务里 AllocateSeq 的
// FOR UPDATE + 插入新 op 行互相等待,被判死锁牺牲的是玩家的请求(§4.37)。
//
// **两段查询,不是一条 SQL**(X-03 防饿死,契约见 assetop.Store.ListDue):
// 第一段只取新行(attempts < FreshAttemptLimit)并占满 limit;只有它不够时才发第二段取老行,
// 且第二段只补缺口。写成一条 SQL 会饿死新行:老行的退避被 MaxBackoff 封顶在 60s,于是它们
// 永远"早就到期",按 next_attempt_ms 排序必定霸占整批名额,新提交的托管指令永远排不上号。
//
// 两段谓词互斥,但这是**两次独立的非锁读**,期间某行的 attempts 可能刚好从 2 跳到 3 而被两段
// 都取到,所以仍要按 op_id 去重。第一段的 id 排在前面,让 Claim 先抢新行。
func (r *AssetOpRepo) ListDue(ctx context.Context, nowMs uint64, limit int) ([]uint64, error) {
	if limit <= 0 {
		return nil, nil
	}
	ctx, cancel := r.bounded(ctx)
	defer cancel()

	fresh, err := r.listDueSegment(ctx, nowMs, limit, true)
	if err != nil {
		return nil, err
	}
	if len(fresh) >= limit {
		return fresh, nil
	}

	aged, err := r.listDueSegment(ctx, nowMs, limit-len(fresh), false)
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

// listDueSegment 跑 ListDue 的其中一段。freshOnly=true 取 attempts < FreshAttemptLimit 的新行,
// false 取 >= 的老行;两段共用同样的到期条件与排序。
// 排序带 op_id 是为了确定性:同毫秒到期的行在多个副本上顺序一致,减少抢同一行的概率。
func (r *AssetOpRepo) listDueSegment(ctx context.Context, nowMs uint64, limit int, freshOnly bool) ([]uint64, error) {
	cmp := ">="
	if freshOnly {
		cmp = "<"
	}
	rows, err := r.db.QueryContext(ctx,
		"SELECT `op_id` FROM "+AssetOpTableName+
			" WHERE `status` = ? AND `next_attempt_ms` <= ? AND `lease_until_ms` < ?"+
			" AND `attempts` "+cmp+" ?"+
			" ORDER BY `next_attempt_ms` ASC, `op_id` ASC LIMIT ?",
		PendingStatus(), nowMs, nowMs, assetop.FreshAttemptLimit, limit)
	if err != nil {
		return nil, fmt.Errorf("list due %s (fresh=%v): %w", AssetOpTableName, freshOnly, err)
	}
	defer func() { _ = rows.Close() }()
	var ids []uint64
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan due %s: %w", AssetOpTableName, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate due %s: %w", AssetOpTableName, err)
	}
	return ids, nil
}

// Claim 单行主键 CAS 领取(autocommit),紧挨着处理前调用,租约从"领到这一行"起算。
// RowsAffected == 0 表示这一行已被别的副本领走(或已终结),返回 (Op{}, false, nil)。
//
// payload 解不开的毒行不能卡住整个循环:把它推迟到 poisonUntilMs 并回 assetop.ErrPoisonRow,
// 循环记 decode 计数后继续下一行(§4.37)。
//
// poisonUntilMs 由 assetop 的重投循环按 LoopConfig.PoisonDelay 算好传进来。**这里不许再自写
// 一份毒行延迟常量**:写了的话 yaml 里改 PoisonDelay 不生效,两份值迟早分叉,而分叉的表现是
// "配置改了没用",没有任何报错。
func (r *AssetOpRepo) Claim(ctx context.Context, opID, nowMs, leaseUntilMs, poisonUntilMs, token uint64) (assetop.Op, bool, error) {
	ctx, cancel := r.bounded(ctx)
	defer cancel()
	res, err := r.db.ExecContext(ctx,
		"UPDATE "+AssetOpTableName+" SET `lease_until_ms` = ?, `lease_token` = ?, `updated_ms` = ?"+
			" WHERE `op_id` = ? AND `status` = ? AND `lease_until_ms` < ?",
		leaseUntilMs, token, nowMs, opID, PendingStatus(), nowMs)
	if err != nil {
		return assetop.Op{}, false, fmt.Errorf("claim %s %d: %w", AssetOpTableName, opID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return assetop.Op{}, false, fmt.Errorf("claim %s %d rows affected: %w", AssetOpTableName, opID, err)
	}
	if affected != 1 {
		return assetop.Op{}, false, nil
	}

	rec, err := r.getOp(ctx, opID)
	if err != nil {
		return assetop.Op{}, false, err
	}
	bundle := &assetpb.AssetBundle{}
	if len(rec.GetPayload()) > 0 {
		if err := proto.Unmarshal(rec.GetPayload(), bundle); err != nil {
			r.markPoison(ctx, opID, token, nowMs, poisonUntilMs)
			return assetop.Op{}, false, fmt.Errorf("%w: op_id=%d: %v", assetop.ErrPoisonRow, opID, err)
		}
	}
	return assetop.Op{
		OpID:        rec.GetOpId(),
		PlayerID:    rec.GetPlayerId(),
		Stream:      assetpb.AssetOpStream(rec.GetStream()),
		Seq:         rec.GetSeq(),
		StreamEpoch: rec.GetStreamEpoch(),
		// correlation_id 取 op_id,**不是** ref_id:§4.38 的人工终结以 scene 流水
		// correlation_id 定位唯一一行 outbox。取 ref_id 的话,同一件商品的托管与回退
		// 两条 op 会在 transaction_log 里落成同一个 correlation_id,取证就失效了。
		// 这里必须与 reconcile 首次投递时填的值一致,否则重投会换一个 correlation。
		CorrelationID: rec.GetOpId(),
		TxType:        rec.GetTxType(),
		Bundle:        bundle,
		Attempts:      rec.GetAttempts(),
		DeadlineMs:    rec.GetDeadlineMs(),
		LeaseToken:    token,
		LastReason:    rec.GetLastReason(),
	}, true, nil
}

// markPoison 把解不开 payload 的行推迟到 poisonUntilMs,并记下"最近一次结局未知"。
// 用本次领取的 lease_token 做 CAS,避免推迟别的副本刚领走的同一行。
// 失败只记在返回给调用方的错误里之外的日志层:这里没有 logger,推迟失败的后果是
// 下一轮再撞一次同一行(仍然跳过),不会丢数据,所以吞掉写错误但不吞解码错误。
func (r *AssetOpRepo) markPoison(ctx context.Context, opID, token, nowMs, poisonUntilMs uint64) {
	_, _ = r.db.ExecContext(ctx,
		"UPDATE "+AssetOpTableName+" SET `last_outcome` = 0, `next_attempt_ms` = ?, `lease_until_ms` = 0, `updated_ms` = ?"+
			" WHERE `op_id` = ? AND `lease_token` = ?",
		poisonUntilMs, nowMs, opID, token)
}

// Finalize 把行终结成 status,并在同一事务里做对侧账。RowsAffected == 1 才算本次终结
// (返回 true);已被别的副本终结时返回 false,调用方不得重复入账。
//
// 锁序:全序的唯一事实源在本文件上方"seq 行的建行守卫"一节(含哨兵守卫行那一段),
// 与 guild 的 guild → guild_member → seq → op 同形。本方法本身只锁一行 trade_asset_op
// (主键 CAS),不碰 seq 表,所以它是全序的最后一段。
//
// 本批没有订单与卖家账,所以事务里只有 outbox 一张表;对侧账的位置留在下面的 TODO 注释处,
// 由 P3 在**同一个事务**里补(拆成两个事务就会出现"发了货没改状态")。
func (r *AssetOpRepo) Finalize(ctx context.Context, op assetop.Op, status assetop.Status, res assetop.Result, nowMs uint64) (bool, error) {
	ctx, cancel := r.bounded(ctx)
	defer cancel()
	finalized := false
	err := assetop.WithTxRetry(ctx, r.db, txRetryAttempts, IsRetryableTxError, func(tx *sql.Tx) error {
		finalized = false
		result, err := tx.ExecContext(ctx,
			"UPDATE "+AssetOpTableName+" SET `status` = ?, `durable` = 1, `last_outcome` = ?, `last_reason` = ?, `updated_ms` = ?"+
				" WHERE `op_id` = ? AND `status` = ?",
			int32(StatusToRecord(status)), uint32(res.Outcome), res.Reason, nowMs, op.OpID, PendingStatus())
		if err != nil {
			return fmt.Errorf("finalize %s %d: %w", AssetOpTableName, op.OpID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("finalize %s %d rows affected: %w", AssetOpTableName, op.OpID, err)
		}
		if affected != 1 {
			return nil // 已被别的副本终结:不是错误,也不做对侧账
		}
		finalized = true
		// P3 对侧账在这里(同一事务):
		//   ESCROW_DEBIT  APPLIED → 商品 ESCROWING → LISTED;REJECTED/ABORTED → ESCROW_REJECTED;
		//   DELIVER_CREDIT APPLIED → 订单 DELIVERING → DELIVERED + 卖家入账;
		//   RETURN_CREDIT  APPLIED → 商品 RETURNING → RETURNED。
		//   APPLIED_PARTIAL 一律不入账、不退款,转人工补偿(§4.33)。
		return nil
	})
	if err != nil {
		return false, err
	}
	return finalized, nil
}

// ResolveManually 是 §4.38 的人工终结通道:把一行卡死的 outbox 按人工判定直接落成终局,
// 并留下操作人与依据。它**不查证据、不猜结论** —— 调用方(管理入口)已经按 scene 流水的
// correlation_id = op_id 判定过,本方法只负责落库 + 留痕 + 与自动路径同一把 CAS。
//
// 与 Finalize 完全同形:同一个事务、同一条 `status = PENDING` 的 CAS、RowsAffected == 1
// 才算本次终结并做对侧账。两条路径共用这个条件,人工与循环同时下手也只会有一个赢家。
//
// durable 刻意**不**置 1:这一行的终局来自人工判定,不是 scene 确认的落盘结局,
// 把它标成 durable 会让事后对账分不清"scene 真的答过"和"人写上去的"。
func (r *AssetOpRepo) ResolveManually(ctx context.Context, m assetop.ManualResolution, nowMs uint64) (bool, error) {
	// 状态合法性、操作人与理由的长度由 assetop.Loop.ResolveManually 在进来之前校验;
	// 这里只兜住"映射不出库值"这一种:落成 UNSPECIFIED 会让客服查不到结局。
	final := StatusToRecord(m.Final)
	if final == tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_UNSPECIFIED ||
		final == tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_PENDING {
		return false, fmt.Errorf("manual resolve %s %d: 终结状态非法 (%s)", AssetOpTableName, m.OpID, m.Final)
	}
	ctx, cancel := r.bounded(ctx)
	defer cancel()
	resolved := false
	err := assetop.WithTxRetry(ctx, r.db, txRetryAttempts, IsRetryableTxError, func(tx *sql.Tx) error {
		resolved = false
		result, err := tx.ExecContext(ctx,
			"UPDATE "+AssetOpTableName+" SET `status` = ?, `resolved_by` = ?, `resolve_reason` = ?, `updated_ms` = ?"+
				" WHERE `op_id` = ? AND `status` = ?",
			int32(final), m.Operator, m.Reason, nowMs, m.OpID, PendingStatus())
		if err != nil {
			return fmt.Errorf("manual resolve %s %d: %w", AssetOpTableName, m.OpID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("manual resolve %s %d rows affected: %w", AssetOpTableName, m.OpID, err)
		}
		if affected != 1 {
			return nil // 行不存在或已被终结:不是错误,也不做对侧账
		}
		resolved = true
		// P3 对侧账在这里,与 Finalize 的 TODO 处**同一份**逻辑(抽成一个内部函数,
		// 两条路径都调它);人工与自动走两份对侧账就会出现只有一边改了商品状态。
		return nil
	})
	if err != nil {
		return false, err
	}
	return resolved, nil
}

// Reschedule 退回待办并推迟下一次投递。带 lease_token 做 CAS:租约已被别人接管时本次更新落空,
// 不会把别的副本刚排好的时间覆盖掉。
//
// 落空(RowsAffected == 0)必须回 assetop.ErrLeaseLost,这是契约的一部分。以前这里写的是
// `_, err :=`,把 RowsAffected 丢掉、一律回 nil —— 于是"我这一轮的结果被另一个副本丢弃了"
// 在指标里完全看不见:循环把它当成功,`assetop_reschedule_lost_total` 恒为 0,租约打架
// 只能靠人肉比对日志时间戳才发现。
func (r *AssetOpRepo) Reschedule(ctx context.Context, op assetop.Op, nextAttemptMs uint64, res assetop.Result, nowMs uint64) error {
	ctx, cancel := r.bounded(ctx)
	defer cancel()
	execRes, err := r.db.ExecContext(ctx,
		"UPDATE "+AssetOpTableName+" SET `attempts` = `attempts` + 1, `next_attempt_ms` = ?, `lease_until_ms` = 0,"+
			" `durable` = ?, `last_outcome` = ?, `last_reason` = ?, `updated_ms` = ?"+
			" WHERE `op_id` = ? AND `status` = ? AND `lease_token` = ?",
		nextAttemptMs, res.Durable, uint32(res.Outcome), res.Reason, nowMs, op.OpID, PendingStatus(), op.LeaseToken)
	if err != nil {
		return fmt.Errorf("reschedule %s %d: %w", AssetOpTableName, op.OpID, err)
	}
	affected, err := execRes.RowsAffected()
	if err != nil {
		return fmt.Errorf("reschedule %s %d rows affected: %w", AssetOpTableName, op.OpID, err)
	}
	if affected == 0 {
		return fmt.Errorf("%w: op_id=%d", assetop.ErrLeaseLost, op.OpID)
	}
	return nil
}

// OldestPendingCreatedMs 实现 assetop 的可选接口,喂 assetop_pending_oldest_age_seconds。
// 每 30s 一次的聚合查询;本批表小,先不为它单独建索引(积压真上来了再按 (status, stream) 补)。
func (r *AssetOpRepo) OldestPendingCreatedMs(ctx context.Context, stream assetpb.AssetOpStream) (uint64, bool, error) {
	ctx, cancel := r.bounded(ctx)
	defer cancel()
	var oldest sql.NullInt64
	err := r.db.QueryRowContext(ctx,
		"SELECT MIN(`created_ms`) FROM "+AssetOpTableName+" WHERE `status` = ? AND `stream` = ?",
		PendingStatus(), uint32(stream)).Scan(&oldest)
	if err != nil {
		return 0, false, fmt.Errorf("oldest pending %s: %w", AssetOpTableName, err)
	}
	if !oldest.Valid || oldest.Int64 < 0 {
		return 0, false, nil
	}
	return uint64(oldest.Int64), true, nil
}

// getOp 按主键取全列。
func (r *AssetOpRepo) getOp(ctx context.Context, opID uint64) (*tradepb.TradeAssetOpRecord, error) {
	row := r.db.QueryRowContext(ctx, "SELECT "+assetOpColumns+" FROM "+AssetOpTableName+" WHERE `op_id` = ?", opID)
	rec, err := scanAssetOp(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// 刚 CAS 成功又查不到:只可能是被并发删了(本服务从不删 outbox 行),按故障返回。
			return nil, fmt.Errorf("select %s %d: 行在领取后消失", AssetOpTableName, opID)
		}
		return nil, fmt.Errorf("select %s %d: %w", AssetOpTableName, opID, err)
	}
	return rec, nil
}

// scanAssetOp 按 assetOpColumns 的列序扫一行。枚举列先扫 int32 再转型,不依赖
// database/sql 对具名整数类型的反射转换(同 scanListing)。
func scanAssetOp(row rowScanner) (*tradepb.TradeAssetOpRecord, error) {
	rec := &tradepb.TradeAssetOpRecord{}
	var kind, status, refKind int32
	if err := row.Scan(
		&rec.OpId, &rec.PlayerId, &rec.Stream, &rec.StreamEpoch, &rec.Seq,
		&kind, &status, &rec.Durable,
		&rec.Attempts, &rec.NextAttemptMs, &rec.DeadlineMs, &rec.LeaseUntilMs, &rec.LeaseToken,
		&rec.TxType, &refKind, &rec.RefId, &rec.Payload,
		&rec.LastOutcome, &rec.LastReason, &rec.CreatedMs, &rec.UpdatedMs,
		&rec.ResolvedBy, &rec.ResolveReason,
	); err != nil {
		return nil, err
	}
	rec.Kind = tradepb.TradeAssetOpKind(kind)
	rec.Status = tradepb.TradeAssetOpStatus(status)
	rec.RefKind = tradepb.TradeAssetOpRefKind(refKind)
	return rec, nil
}

// StatusToRecord 把 assetop 的语义终态映射成本表的存储值。
// assetop.Status 刻意不绑库值(§4.43 #23),映射只此一处。
func StatusToRecord(s assetop.Status) tradepb.TradeAssetOpStatus {
	switch s {
	case assetop.StatusPending:
		return tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_PENDING
	case assetop.StatusApplied:
		return tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED
	case assetop.StatusRejected:
		return tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_REJECTED
	case assetop.StatusAborted:
		return tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_ABORTED
	case assetop.StatusAppliedPartial:
		return tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED_PARTIAL
	default:
		// 未知语义值不能落成 PENDING(会被循环反复领走),也不能落成 APPLIED(会伪装成功)。
		return tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_UNSPECIFIED
	}
}

// txRetryAttempts 是业务写路径撞死锁 / 写冲突时的整体重试次数(§4.37:attempts=2)。
const txRetryAttempts = 2

// 毒行推迟时长**刻意不在这里**:它是 assetop.LoopConfig.PoisonDelay(由 trade.yaml 的
// PoisonDelayMs 喂进来),循环算好绝对时刻经 Claim 的 poisonUntilMs 传进来。
// 这里再写一份 const 就是第二份真相 —— 改 yaml 不生效,且不会有任何报错。

// IsRetryableTxError 报告事务错误是否值得整体重试。shared/assetop 不引 MySQL 驱动,
// 分类由调用方注入(§4.37);取值与 guild 一致:
//
//	1213 死锁 / 1205 锁等待超时 / 9007 TiDB 写冲突。
func IsRetryableTxError(err error) bool {
	var myErr *mysql.MySQLError
	if !errors.As(err, &myErr) {
		return false
	}
	switch myErr.Number {
	case 1213, 1205, 9007:
		return true
	default:
		return false
	}
}
