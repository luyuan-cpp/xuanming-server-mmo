package data

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"time"

	assetpb "proto/common/asset"
	tradepb "proto/trade"

	"shared/assetop"
	"shared/safego"

	"github.com/go-sql-driver/mysql"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// 行结构的事实源是 proto/trade/trade_table.proto 的 TradeAssetOpRecord(assetOpColumns 与它逐字段同序,
// asset_op_repo_test.go 守住)。改 proto 之后按 AGENTS §4.1 在仓库根重生成:
// `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`(生成器源码有变化或还没有工具时先跑 proto-gen-build;
// 未获客户端任务授权时加 `-ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`),再编译本模块。

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
	"`resolved_by`, `resolve_reason`, `last_send_ms`, `escrow_snapshot`, `receipt_ack_pending`, `business_consumed`"

// PendingStatus 是 outbox 的"待办"状态数值,给 assetop 的 seq 分配与重投循环用。
// 取 proto 枚举而不是写 0 / 1:§4.43 #23 明确 assetop.Status 不绑库值,库值由业务表自己定。
func PendingStatus() uint32 { return uint32(tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_PENDING) }

// AssetOpRepo 是通用资产通道 outbox(trade_asset_op)与 seq 分配器(trade_player_op_seq)的
// MySQL 实现,同时满足 assetop.Store。
//
// 契约:
//   - 除 InsertOp / AllocateSeqsTx 之外的方法自带 opTimeout 上限,调用方 ctx 更早到期时以 ctx 为准
//     (EnsureSeqRows 自己开短事务,所以也带 opTimeout);
//   - InsertOp / AllocateSeqsTx 只在调用方给的事务里执行,不自带超时(事务的预算由调用方统一给);
//   - 返回的 error 一律是存储故障,唯一的例外是 Claim 的 assetop.ErrPoisonRow(payload 解不开)。
type AssetOpRepo struct {
	db        *sql.DB
	opTimeout time.Duration
	tables    assetop.SeqTables
}

var (
	_ assetop.Store            = (*AssetOpRepo)(nil)
	_ assetop.ManualResolver   = (*AssetOpRepo)(nil)
	_ assetop.PendingAgeReader = (*AssetOpRepo)(nil)
	// 落地恢复(docs/design/landing-recovery-sync-digest.md §5.3 / §6.2 / §6.3)的三个可选能力,
	// 与 guild 的 GuildAssetStore 同口径实现:落地唤醒、日对账、自动放弃。
	_ assetop.PlayerPendingLister = (*AssetOpRepo)(nil)
	_ assetop.PendingClassCounter = (*AssetOpRepo)(nil)
	_ assetop.Abandoner           = (*AssetOpRepo)(nil)
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
// seq 行的建行
// (2026-09-21 死锁审计 #16 在 trade 的同形;2026-09-29 建行拆进它自己的 RC 短事务;
//
//	2026-10-30 撤掉全服排队的哨兵守卫行,docs/design/player-data-tidb-scaling-plan.md §8.1 第 2 条)
//
// ---------------------------------------------------------------------------
//
// # trade 的全局取锁顺序(唯一事实源;新增任何写路径先回来对一遍)
//
// 两段,**分属两个事务**,前一段提交之后后一段才开始:
//
//	[短事务 A:建 seq 行]  trade_player_op_seq 的缺失玩家行
//	                        (按 (player_id, stream) **升序** INSERT IGNORE)
//	                        ── COMMIT ──
//	[业务事务 B]            trade_listing → trade_order(P3 未落)
//	                        → trade_player_op_seq 的玩家行
//	                          (按 (player_id, stream) **升序** FOR UPDATE)
//	                        → trade_asset_op
//
// 逐条核对(2026-09-29 重做了一遍,新增写路径必须回来补):
//   - InsertListing / InsertFavorite / DeleteFavorite(listing_repo.go):单表单语句,整份文件
//     一条 FOR UPDATE 都没有,也不在同一事务里碰 seq / outbox —— 不参与本条全序。
//   - EnqueueEscrowDebit(reconcile/pipeline.go):**唯一**同时碰 seq 与 outbox 的路径。它先调
//     EnsureSeqRows(事务 A,自带提交),再开事务 B 调 AllocateSeqsTx(确认读 → 升序 FOR UPDATE)与 InsertOp。
//   - Finalize / ResolveManually / Abandon:RC 事务{trade_asset_op 主键点锁(sqlLockAssetOp)→ 带复核谓词的主键 CAS},
//     不碰 seq 表;点锁读不到行(只可能已被清理)按 CAS 落空处理。为什么先点锁见下方"op 行的写者:先主键点锁再 CAS"。
//   - Claim / ReleaseLease:自动提交的单行主键 UPDATE(RC,见 svc.BuildDSN),只改无索引列,变更集合只有行 key。
//   - Reschedule / markPoison:RC 事务{sqlLockAssetOp 主键点锁 → 带复核谓词的主键点改},与 guild 对齐(2026-10-18)。
//     它们改 idx_0 的 next_attempt_ms,自动提交在 TiDB 上会与终结各持一半互等、检测不到(真 TiDB 实测,事故文档 §9.7)。
//   - CleanupTerminalOps:逐行 RC 短事务{trade_asset_op 主键点锁 → 带复核条件的主键点删},只删终态行,
//     不碰 seq 表(seq 表的"永不删除"前提不受影响,见 TestSeqTableRowsAreNeverDeleted)。
//   - ListDue / OldestPendingCreatedMs / getOp / ListPendingByPlayer / CountPendingByClass /
//     ListAbandonCandidates / 清理的候选读:不加锁读。
//
// P3 落订单与卖家账时:商品 / 订单行的锁排在事务 B 的最前面(全序第一、二段),
// **而 EnsureSeqRows 要排在事务 B 之外、之前**(它自己就是事务 A)。
//
// # 为什么没有全局守卫行
//
// seq 行建出后永不删除,所以并发补行的后到者拿 S 看到的是**已提交**的重复键、判重即结束,不会升 X。只剩一种 1213:
// 首个插入者在提交前回滚(连接被 KILL、刷盘失败、实例关闭、上层 ctx 到期),排在它那条未提交记录后面做重复键检查的
// 多个 INSERT 会同时继承到同一段间隙上的锁,随后各自申请插入意向锁、被对方的间隙锁挡住,InnoDB 牺牲其一。
//
// 这里曾有一行哨兵 (player_id=0, stream=0),所有首次建行者先排在它上面来消掉这个环。代价是开服首日几乎每一次上架都是
// 首次建行,全服托管入队在这一行上串行。现在撤掉了:唯一性由主键裁决;TiDB 没有间隙锁,这个环本来就不存在;
// MySQL 档上这个环只在首插者回滚时才碰得上。生产连接走 authoritysql,写者闸门把 1213 变成不可重试的结果未知(事务已回滚),
// assetop.WithTxRetry 认不出,这次上架直接失败,玩家重试即可(INSERT IGNORE 幂等、不改纪元、没有内存副作用)。
//
// # 为什么建行放在独立的短事务里
//
// 建行不和业务事务持锁重叠:seq 行的 INSERT 持锁时间压到"每个缺失键一条 INSERT IGNORE",与 AllocateSeq、outbox 写入、
// 事务重试退避全部无关。稳态更省:EnsureSeqRows 的前置探针命中时连事务都不开。
//
// # 拆开之后为什么仍然正确:短事务提交之后、业务事务开始之前,seq 行不可能消失
//
//  1. 事务 A 是**提交**过的,不是"写了就算"。提交之后那几行是已提交数据,业务事务 B 回滚只回滚
//     它自己,碰不到 A 写下的行。
//  2. 全仓对 trade_player_op_seq **没有任何 DELETE / TRUNCATE / DROP**,所以行不会被业务代码删掉。
//     这条前提不靠人记:asset_op_seq_guard_test.go 的 TestSeqTableRowsAreNeverDeleted 扫 **trade
//     模块**全部非测试源码,出现 DELETE 就判红(判据范围与本段措辞必须一致,改一处要改另一处)。
//  3. 于是事务 B 里的确认读(同一条 sqlSeqRowExists)在 RC 下必然命中:RC 每条语句取一份新快照,
//     而 A 在 B 的 BeginTx **之前**就已经提交。
//  4. 万一真读不到(只可能是有人在库上手工删了行,或将来有人加了清理路径),**fail-closed**:
//     回 ErrSeqRowMissingInTx、计一次 trade_assetop_seq_row_missing_in_tx_total,本次上架失败。
//     绝不在事务 B 里就地补行:业务事务回滚会把建行一起带走,而且持业务锁去插 seq 行会把锁序搅乱。
//
// ⚠ 第 3 条的前提是"单个 MySQL 主库、同一个连接池"(本服务就是这么连的,见 svc.OpenMySQL)。
// 将来若把读切到从库 / follower read,确认读可能落在落后的快照上,这一条要重新论证。
//
// # 多个 key 时为什么必须按 (player_id, stream) 升序
//
// 玩家 seq 行一旦建出,后续所有分配都只是普通读命中 + 玩家行 FOR UPDATE。P3 的交付要在同一个事务里给买家 CREDIT 和
// 卖家 CREDIT,两笔交易若以相反顺序去锁两条**都已存在**的玩家行(买家A/卖家B 与 买家B/卖家A),就直接在
// trade_player_op_seq 上成环。
//
// 所以本文件把顺序钉死在**一个确定性全序**上:(player_id, stream) 升序,建行(事务 A)与分配
// (事务 B)都按它走,并且只由 EnsureSeqRows / AllocateSeqsTx 两个方法内部排序 —— 调用方拿不到
// 乱序的机会(机械守卫:TestSeqKeysAreProcessedInAscendingOrder)。
const (
	// sqlSeqRowExists 是**普通读**(完整主键等值,不带任何锁定子句;RC 下不取行锁),两处共用:
	//   - EnsureSeqRows 的前置探针:seq 行建出后永不删除,常态直接命中,连短事务都不必开;
	//   - AllocateSeqsTx 的确认读:业务事务里只用它确认行在,不在业务事务里建行。
	// 给它加锁定子句没有好处:RC 下未命中什么都拦不住,命中时白白在玩家 seq 行上排队。
	sqlSeqRowExists = "SELECT 1 FROM " + AssetOpSeqTableName + " WHERE `player_id` = ? AND `stream` = ?"
)

// ErrSeqRowMissingInTx:业务事务里确认 seq 行时读不到 —— 而 EnsureSeqRows 的短事务刚刚提交过它。
// 按"拆开之后为什么仍然正确"一节第 3 条的论证这不可能发生,能走到这里只剩两种解释:有人在库上手工删了行,
// 或者将来有人给这张表加了清理路径(那种情况下整套锁分析都要重做,见 TestSeqTableRowsAreNeverDeleted)。
// 所以这里**不就地补行**,直接 fail-closed。
var ErrSeqRowMissingInTx = errors.New("trade: 业务事务里确认 trade_player_op_seq 行时行不存在")

// SeqKey 是一条 seq 行的键。取名不叫 SeqRow 是因为它只是键,不含 next_seq / epoch。
//
// 它同时是本文件那条确定性全序的比较键:所有多 key 路径都按 (PlayerID, Stream) **升序**处理,
// 理由见上文"多个 key 时为什么必须按 (player_id, stream) 升序"。
type SeqKey struct {
	PlayerID uint64
	Stream   assetpb.AssetOpStream
}

// seqRowMissingInTxTotal 记业务事务确认 seq 行落空的次数。这条分支 fail-closed 且**静默**:错误沿调用栈返回给玩家,
// 除了日志没有别的信号(AGENTS §11.3:关键路径要有低基数指标)。
//
// 注册在 prometheus.DefaultRegisterer 上,与 svc.assetChannelMetrics 里的 assetop / scenenode 指标、
// 以及 svc 自己的 trade_* 计数器同一个 registry,由 svc.StartMetrics 的 promhttp.Handler() 一起暴露
// —— **不另起一套 registry**。
//
// **无 label**:这件事与具体玩家无关,player_id 只进日志(AGENTS §9)。
// 用普通 Counter 而不是 CounterVec:Counter 一注册就有一条值为 0 的序列,
// "从未发生"与"指标不存在"在告警规则里不会长得一样。
//
// 包级变量只初始化一次,所以 promauto 的 MustRegister 不会因重复注册 panic(单测里同样安全)。
var seqRowMissingInTxTotal = promauto.With(prometheus.DefaultRegisterer).NewCounter(prometheus.CounterOpts{
	Subsystem: "trade",
	Name:      "assetop_seq_row_missing_in_tx_total",
	Help:      "业务事务确认 seq 行时读不到(短事务刚建过)的次数;>0 说明有人删了 seq 行,按数据事故处理。",
})

// normalizeSeqKeys 校验、去重并按 (PlayerID, Stream) **升序**排序。
//
// 排序是正确性的一部分,不是整洁癖:它是本服务对多把玩家 seq 行锁的确定性全序,理由见上文
// "多个 key 时为什么必须按 (player_id, stream) 升序"。去重是因为同一个 key 传两次会在事务里
// 对同一行取两次锁 —— 无害但会让语句序列的机械断言失真。
//
// player_id=0 不是玩家,stream=0 是 UNSPECIFIED,assetop 不给它分配 seq,这里直接拒。
func normalizeSeqKeys(keys []SeqKey) ([]SeqKey, error) {
	out := make([]SeqKey, 0, len(keys))
	for _, k := range keys {
		if k.PlayerID == 0 || k.Stream == assetpb.AssetOpStream_ASSET_OP_STREAM_UNSPECIFIED {
			return nil, fmt.Errorf("trade: 非法的 seq 行键 (player=%d stream=%d)", k.PlayerID, int32(k.Stream))
		}
		out = append(out, k)
	}
	slices.SortFunc(out, func(a, b SeqKey) int {
		if c := cmp.Compare(a.PlayerID, b.PlayerID); c != 0 {
			return c
		}
		return cmp.Compare(int32(a.Stream), int32(b.Stream))
	})
	return slices.Compact(out), nil
}

// seqRowReader 是 *sql.DB 与 *sql.Tx 的公共子集,只为让"自动提交探针"与"事务内确认读"
// 共用同一段实现、同一条 SQL 常量。
type seqRowReader interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// missingSeqRows 用 sqlSeqRowExists(普通读,RC 下不取行锁)挑出还不存在的行,保持入参顺序。
func (r *AssetOpRepo) missingSeqRows(ctx context.Context, q seqRowReader, keys []SeqKey) ([]SeqKey, error) {
	missing := make([]SeqKey, 0, len(keys))
	for _, k := range keys {
		var exists int
		switch err := q.QueryRowContext(ctx, sqlSeqRowExists, k.PlayerID, uint32(k.Stream)).Scan(&exists); {
		case err == nil:
			continue
		case errors.Is(err, sql.ErrNoRows):
			missing = append(missing, k)
		default:
			return nil, fmt.Errorf("读 %s 行 (player=%d stream=%d): %w",
				AssetOpSeqTableName, k.PlayerID, int32(k.Stream), err)
		}
	}
	return missing, nil
}

// EnsureSeqRows 按需把 keys 的 seq 行建出来,**在它自己的 RC 短事务里完成并提交**。
//
// 它是全序里的"事务 A",必须在业务事务(事务 B)**开始之前**调用,不能塞进业务事务里
// (理由见上文"为什么建行放在独立的短事务里")。
//
// 形状:
//  1. 自动提交的普通读探针(每个 key 一条主键点查)。全部命中就**连事务都不开**、一条写语句都不发
//     —— 这是稳态路径(每玩家每流只有第一笔会缺行)。
//  2. 有缺行才开短事务:按升序逐个 INSERT IGNORE → 提交。
//
// 这里用 assetop.WithTxRetry(RC + 有界重试)。裸连接上它吸收 1213(首插者回滚,见上文"为什么没有全局守卫行")、
// 1205(锁等待超时)与 9007(TiDB 写冲突);走 authoritysql 的生产连接上这些错误已经是结果未知,不重试。
// INSERT IGNORE 撞行是空操作、不改纪元,没有任何内存副作用要复位,所以整体重跑安全。
//
// 预算:自带 opTimeout(constants.StoreOpTimeout),调用方 ctx 更早到期时以 ctx 为准。超时按失败返回,本次上架失败。
func (r *AssetOpRepo) EnsureSeqRows(ctx context.Context, nowMs uint64, keys ...SeqKey) error {
	ordered, err := normalizeSeqKeys(keys)
	if err != nil {
		return err
	}
	if len(ordered) == 0 {
		return nil
	}
	ctx, cancel := r.bounded(ctx)
	defer cancel()

	missing, err := r.missingSeqRows(ctx, r.db, ordered)
	if err != nil {
		return err
	}
	if len(missing) == 0 {
		return nil // 稳态:不开事务
	}

	return assetop.WithTxRetry(ctx, r.db, txRetryAttempts, IsRetryableTxError, func(tx *sql.Tx) error {
		return r.ensureSeqRowsTx(ctx, tx, nowMs, missing)
	})
}

// ensureSeqRowsTx 是短事务 A 的事务体:按升序建行。
//
// 刻意不导出:它只有在"调用方持有的这个事务马上就会提交、而且这个事务里不会再碰任何业务行"的
// 前提下才是安全的,而这个前提没法从签名上表达。唯一的生产调用方是 EnsureSeqRows;
// 集成测试在同包里直接调它,只为把"插了先别提交"的持锁方摆出来。
//
// keys 必须已经过 normalizeSeqKeys(升序、去重、无非法键)。
func (r *AssetOpRepo) ensureSeqRowsTx(ctx context.Context, tx *sql.Tx, nowMs uint64, keys []SeqKey) error {
	if tx == nil {
		return errors.New("trade: ensureSeqRowsTx 必须在一个事务里执行(tx 为 nil)")
	}
	for _, k := range keys {
		// 语句与纪元语义由 assetop 独占(ensureSeqRowFormat):INSERT IGNORE、next_seq 从 1 起、
		// 已存在是空操作且不改纪元。这里不抄第二份 —— 两份迟早分叉,而分叉的表现是纪元对不上、
		// scene 把整条流当成需要重置。
		if err := assetop.EnsureSeqRowTx(ctx, tx, r.tables, k.PlayerID, k.Stream, nowMs); err != nil {
			return fmt.Errorf("建 %s 行 (player=%d stream=%d): %w", AssetOpSeqTableName, k.PlayerID, int32(k.Stream), err)
		}
	}
	return nil
}

// AllocateSeqsTx 在**调用方的业务事务(事务 B)里**确认 seq 行在,并按 (player_id, stream) 升序
// 逐个分配 seq。返回 key → 分配结果。
//
// 为什么把"确认"和"分配"收进同一个方法:这两步的顺序与**多把玩家 seq 行锁之间的顺序**就是本服务
// 全序的最后一段,而顺序错了的表现是并发下偶发 1213,不是响亮的失败。收进来之后调用方拿不到乱序的
// 机会 —— P3 要同时给买家 CREDIT 和卖家 CREDIT,那正是最容易写反的形状。
//
// 契约:
//   - 事务必须是 READ COMMITTED(assetop.WithTxRetry 的默认值;AllocateSeq 自己也是这条硬契约);
//   - **必须先调过 EnsureSeqRows 且它已经返回成功**。本方法只做普通读确认,一行都不建;
//   - 本事务里的业务行锁(P3 的 trade_listing / trade_order)要排在本方法**之前**;
//   - player_id=0 或 stream=0 的键直接拒。
//
// 确认读读不到时回 ErrSeqRowMissingInTx 并计数,绝不就地补行(理由见 ErrSeqRowMissingInTx)。
// 确认全部做完才开始取第一把 X:这样"行没了"能在一把锁都没拿之前就失败。
func (r *AssetOpRepo) AllocateSeqsTx(ctx context.Context, tx *sql.Tx, nowMs uint64, keys ...SeqKey) (map[SeqKey]assetop.Alloc, error) {
	if tx == nil {
		return nil, errors.New("trade: AllocateSeqsTx 必须在调用方的业务事务里执行(tx 为 nil)")
	}
	ordered, err := normalizeSeqKeys(keys)
	if err != nil {
		return nil, err
	}
	if len(ordered) == 0 {
		return map[SeqKey]assetop.Alloc{}, nil
	}

	missing, err := r.missingSeqRows(ctx, tx, ordered)
	if err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		seqRowMissingInTxTotal.Inc()
		return nil, fmt.Errorf("%w (player=%d stream=%d):EnsureSeqRows 的短事务刚刚提交过它。"+
			"只可能是有人删了 seq 行或新加了清理路径 —— 本次不就地补行,"+
			"先查 %s 的数据与近期变更",
			ErrSeqRowMissingInTx, missing[0].PlayerID, int32(missing[0].Stream), AssetOpSeqTableName)
	}

	out := make(map[SeqKey]assetop.Alloc, len(ordered))
	for _, k := range ordered {
		a, err := assetop.AllocateSeq(ctx, tx, r.tables, k.PlayerID, k.Stream, assetop.DefaultLimits, nowMs)
		if err != nil {
			return nil, err
		}
		out[k] = a
	}
	return out, nil
}

// InsertOp 在调用方的事务里插入一行 outbox。主键冲突按故障返回:op_id 来自号段,撞号说明
// 发号源出了问题,绝不静默覆盖(同 InsertListing 的理由)。
//
// last_send_ms 按 LastSendMs 契约第 1 条写成**不小于 created_ms**(insertLastSendMs):插行即持租约,
// 提交后的同步投递紧接着就可能发出。生产者漏填(0)时不能写 0 —— 0 在放弃路径上是"Store 没维护",
// 虽然会 fail-closed 跳过,但下一次领取的懒初始化才会补上,中间那段窗口这一行没有任何安全依据。
func (r *AssetOpRepo) InsertOp(ctx context.Context, tx *sql.Tx, rec *tradepb.TradeAssetOpRecord) error {
	_, err := tx.ExecContext(ctx,
		"INSERT INTO "+AssetOpTableName+" ("+assetOpColumns+") VALUES "+
			"(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		rec.GetOpId(), rec.GetPlayerId(), rec.GetStream(), rec.GetStreamEpoch(), rec.GetSeq(),
		int32(rec.GetKind()), int32(rec.GetStatus()), rec.GetDurable(),
		rec.GetAttempts(), rec.GetNextAttemptMs(), rec.GetDeadlineMs(), rec.GetLeaseUntilMs(), rec.GetLeaseToken(),
		rec.GetTxType(), int32(rec.GetRefKind()), rec.GetRefId(), rec.GetPayload(),
		rec.GetLastOutcome(), rec.GetLastReason(), rec.GetCreatedMs(), rec.GetUpdatedMs(),
		// 新行一律是空留痕:人工终结只由 ResolveManually 写,插入路径绝不预置操作人。
		rec.GetResolvedBy(), rec.GetResolveReason(),
		insertLastSendMs(rec),
		rec.GetEscrowSnapshot(), rec.GetReceiptAckPending(), rec.GetBusinessConsumed(),
	)
	if err != nil {
		return fmt.Errorf("insert %s %d: %w", AssetOpTableName, rec.GetOpId(), err)
	}
	return nil
}

// insertLastSendMs 是插行时写进 last_send_ms 的值:max(记录上的 last_send_ms, created_ms)。
// 取较大值而不是无条件覆盖成 created_ms:last_send_ms 只能偏大(安全)不能偏小,生产者显式给了更晚的时刻就照用。
func insertLastSendMs(rec *tradepb.TradeAssetOpRecord) uint64 {
	return max(rec.GetLastSendMs(), rec.GetCreatedMs())
}

// ---------------------------------------------------------------------------
// assetop.Store
// ---------------------------------------------------------------------------
//
// # last_send_ms 的维护(docs/design/landing-recovery-sync-digest.md §6.3,以 go/shared/assetop/abandon.go
// 文件头「LastSendMs 契约」为准)
//
// 它是"此前任何一次投递请求**可能**发出的最晚时刻",自动放弃靠它证明"scene 不可能再收到这条 seq"。
// 本文件的五处写法与契约一一对应,改任何一处先回去读契约:
//  1. 插行:insertLastSendMs(不小于 created_ms);
//  2. 领取(sqlClaimAssetOp):在改写 lease_until_ms **之前**把旧租约折进来;
//  3. 重排(sqlRescheduleAssetOp):只在本次确实发过请求(!res.Local)时推进到 nowMs;
//  4. 毒行推迟(sqlPoisonAssetOp)与归还租约(sqlReleaseAssetOpLease):只清租约,不动它;
//  5. 存量行(加列前插入,值为 0)不做单独回填,在第 2 条的 CAS 里懒初始化为 GREATEST(created_ms, updated_ms)。
//
// 列是 schemamigrate 按 proto 自动 ADD COLUMN 加出来的(bigint unsigned NOT NULL DEFAULT 0),
// 存量行的 0 正是第 5 条要处理的形态,所以不需要额外迁移步骤。
//
// # op 行的写者:先主键点锁再 CAS(终结 / 人工终结 / 放弃 / 清理点删)
//
// 与 guild asset_store.go 文件头"TiDB 附加规则"同一条规则(死锁复核 V1),写法也同形:这四类写者都在 RC 事务里,
// **先**跑 sqlLockAssetOp(完整主键等值 FOR UPDATE,不带任何复核条件),**再**跑带复核谓词
// (`status = PENDING` / 令牌 / 保留期)的主键点改或点删。
//   - 为什么:TiDB 悲观事务里带复核谓词的 UPDATE / DELETE 不走 Point_Get 快路径,执行期间只读不锁,到语句末尾才把
//     {行 key, PRIMARY key(本表是非聚簇主键), 唯一键 (player_id, stream, stream_epoch, seq) 的 key} 按 region **并行**加锁
//     (tidb_lock_unchanged_keys 默认 ON,没改的唯一 key 也锁)。两个这样的写者(终结 ‖ 人工终结、过期租约持有者的终结 ‖ 放弃)
//     在此之前没有更早取到的公共锁时,可能各拿到一部分 key 再互等 —— 检测器看得见的 1213,会被重试吸收,但仍是死锁。
//     先点锁:Point_Get 的加锁顺序固定为"PRIMARY key → 行 key",同一 op 行的全部悲观写者先在 PRIMARY key 上排成一列。
//   - MySQL 下点锁与随后的主键写锁的是同一条聚簇记录,锁集与顺序都不变(聚簇 → 被改列所在的二级索引项),只多一次往返。
//   - 点锁读不到行 = 行已被清理(清理只删保留期外的终态行),与 CAS 影响 0 行同义:终结 / 人工终结回"本次没终结",
//     放弃回 ErrLeaseLost,清理计 0 行。
//   - 领取(Claim)与归还租约(ReleaseLease)只改无索引列,保留自动提交,不在此列(理由同 guild 文件头"领取"一条)。
const (
	// sqlLockAssetOp:op 行的主键点锁。**只许**是完整主键等值 + FOR UPDATE、不带任何复核条件
	// (TiDB 只有这种形状才走 Point_Get 快路径;复核条件留在随后的点改 / 点删里)。用在终结、人工终结、放弃与清理点删之前,
	// 理由见上方"op 行的写者:先主键点锁再 CAS"。与 guild 的同名语句同形。
	sqlLockAssetOp = "SELECT `op_id` FROM " + AssetOpTableName + " WHERE `op_id` = ? FOR UPDATE"

	// sqlClaimAssetOp:单行主键 CAS 领取。SET 子句的顺序是正确性的一部分:
	//   - last_send_ms 写在 lease_until_ms 与 updated_ms **之前**。MySQL 单表 UPDATE 按从左到右求值、
	//     后面的赋值看得见前面赋过的新值;写在最前面,"从左到右"与"一律用旧值"两种实现读到的都是**上一个**
	//     租约的 lease_until_ms 和**上一次**写下的 updated_ms(契约第 2、5 条要的正是旧值)。
	//   - 上一个租约正常收尾时 lease_until_ms 已清 0,GREATEST 不起作用;没收尾(发完请求就崩溃、重排落空 /
	//     出错)时它是那个已过期的非零值,而那次发送一定早于它(投递预算 < 租约),折进来就盖住了它。
	//   - IF(last_send_ms = 0, …) 是存量行的懒初始化:此前每次发送之后都有一次写 updated_ms 的重排或终结,
	//     没收尾的那次由 lease_until_ms 盖住,所以初值不小于任何真实发送。偏保守的代价只是放弃晚一个安全间隔。
	// lease_until_ms / updated_ms / last_send_ms 都不在任何二级索引里,这条语句仍只改聚簇记录。
	sqlClaimAssetOp = "UPDATE " + AssetOpTableName + " SET" +
		" `last_send_ms` = GREATEST(IF(`last_send_ms` = 0, GREATEST(`created_ms`, `updated_ms`), `last_send_ms`), `lease_until_ms`)," +
		" `lease_until_ms` = ?, `lease_token` = ?, `updated_ms` = ?" +
		" WHERE `op_id` = ? AND `status` = ? AND `lease_until_ms` < ?"

	// sqlRescheduleAssetOp:带领取令牌的重排。最后一项 last_send_ms 的两个参数是 (!res.Local, nowMs):
	// 本地合成的 NOT_HERE 根本没发出请求,推进它会让停放中的离线行永远"刚发过",自动放弃永远不会发生;
	// 传输失败、坏流号这类不是 Local 的结果一律按"发过"保守处理。nowMs 是 ProcessOne 的开始时刻,比真实
	// 发送早不到一个 OpBudget(< Lease),这段差由 AbandonSafetyGap ≥ Lease 的下限吸收(契约第 3 条)。
	sqlRescheduleAssetOp = "UPDATE " + AssetOpTableName +
		" SET `attempts` = `attempts` + 1, `next_attempt_ms` = ?, `lease_until_ms` = 0," +
		" `durable` = ?, `last_outcome` = ?, `last_reason` = ?, `updated_ms` = ?," +
		" `last_send_ms` = IF(?, GREATEST(`last_send_ms`, ?), `last_send_ms`)" +
		" WHERE `op_id` = ? AND `status` = ? AND `lease_token` = ?"

	// sqlPoisonAssetOp:毒行推迟,只清租约、不动 last_send_ms(契约第 4 条)。
	// 令牌之外还带 `status = PENDING`(与 guild 的 sqlPoisonAssetOp 同形):人工终结的 CAS 只看 status、不换令牌,
	// 可能恰好落在领取的 CAS 与这里之间;只凭令牌会把已终结的行改回"一小时后再来"、抹掉 last_outcome,
	// 还会把终态行的 next_attempt_ms(= 终结时刻,清理按它判龄)推后。
	sqlPoisonAssetOp = "UPDATE " + AssetOpTableName +
		" SET `last_outcome` = 0, `next_attempt_ms` = ?, `lease_until_ms` = 0, `updated_ms` = ?" +
		" WHERE `op_id` = ? AND `lease_token` = ? AND `status` = ?"

	// sqlFinalizeAssetOp:终结 CAS。同写 next_attempt_ms = 终结时刻、清租约(与 guild 的 sqlFinalizeAssetOp 同形):
	// 终态行的 next_attempt_ms 从此就是终结时刻,终态行清理(CleanupTerminalOps)按它判龄、走 idx_0。
	sqlFinalizeAssetOp = "UPDATE " + AssetOpTableName +
		" SET `status` = ?, `durable` = 1, `last_outcome` = ?, `last_reason` = ?," +
		" `lease_until_ms` = 0, `next_attempt_ms` = ?, `updated_ms` = ?" +
		" WHERE `op_id` = ? AND `status` = ?"

	// sqlResolveAssetOp:人工终结 CAS。不置 durable、不改 last_outcome(理由见 ResolveManually),
	// 同样同写 next_attempt_ms = 终结时刻。
	sqlResolveAssetOp = "UPDATE " + AssetOpTableName +
		" SET `status` = ?, `resolved_by` = ?, `resolve_reason` = ?, `lease_until_ms` = 0," +
		" `next_attempt_ms` = ?, `updated_ms` = ?" +
		" WHERE `op_id` = ? AND `status` = ?"
)

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
//
// 领取的同一条 CAS 里维护 last_send_ms(折旧租约 + 存量行懒初始化,见 sqlClaimAssetOp),
// 回读时填 Op.CreatedMs 与 Op.LastSendMs —— 自动放弃只认这两个回读值,0 一律 fail-closed 跳过。
// 落地唤醒(Loop.WakePlayer)与自动放弃(Loop.AbandonOnce)领行走的也是这同一个方法。
func (r *AssetOpRepo) Claim(ctx context.Context, opID, nowMs, leaseUntilMs, poisonUntilMs, token uint64) (assetop.Op, bool, error) {
	ctx, cancel := r.bounded(ctx)
	defer cancel()
	res, err := r.db.ExecContext(ctx, sqlClaimAssetOp,
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
		CreatedMs:     rec.GetCreatedMs(),
		LastSendMs:    rec.GetLastSendMs(),
	}, true, nil
}

// markPoison 把解不开 payload 的行推迟到 poisonUntilMs,并记下"最近一次结局未知"。
// 用本次领取的 lease_token 且仍为 PENDING 做 CAS:不推迟别的副本刚领走的同一行,也不改写领取之后
// 才被人工终结的行(sqlPoisonAssetOp)。只清租约,不动 last_send_ms(LastSendMs 契约第 4 条)。
// 失败只记在返回给调用方的错误里之外的日志层:这里没有 logger,推迟失败的后果是
// 下一轮再撞一次同一行(仍然跳过),不会丢数据,所以吞掉写错误但不吞解码错误。
//
// 写法与 Reschedule 同:RC 事务{sqlLockAssetOp 主键点锁 → 带令牌与状态复核的点改}。这条改了 idx_0 的 next_attempt_ms,
// TiDB 上若自动提交(乐观、按 region 并行 prewrite),会与终结 / 人工终结的「点锁 PRIMARY → 行 key」各持一半互等,
// 死锁检测器看不到,要等约 3s 的锁 TTL(2026-10-18 真 TiDB v8.5.2 实测;见 docs/ops/incident-friend-lock-order-deadlock-2026-09-21.md §9.7)。
func (r *AssetOpRepo) markPoison(ctx context.Context, opID, token, nowMs, poisonUntilMs uint64) {
	_ = assetop.WithTxRetry(ctx, r.db, txRetryAttempts, IsRetryableTxError, func(tx *sql.Tx) error {
		found, err := lockAssetOpRow(ctx, tx, opID)
		if err != nil || !found {
			return err // 行已被清理:没有可推迟的
		}
		_, err = tx.ExecContext(ctx, sqlPoisonAssetOp, poisonUntilMs, nowMs, opID, token, PendingStatus())
		return err
	})
}

// Finalize 把行终结成 status,并在同一事务里做对侧账。RowsAffected == 1 才算本次终结
// (返回 true);已被别的副本终结时返回 false,调用方不得重复入账。
//
// 锁序:全序的唯一事实源在本文件上方"seq 行的建行"一节,
// 与 guild 的 guild → guild_member → seq → op 同形。本方法本身只锁一行 trade_asset_op
// (先 sqlLockAssetOp 主键点锁、再主键 CAS,见"op 行的写者:先主键点锁再 CAS"),不碰 seq 表,所以它是全序的最后一段。
// 点锁读不到行(只可能已被清理)与 CAS 影响 0 行同义:回 (false, nil),不做对侧账。
//
// 本批没有订单与卖家账,所以事务里只有 outbox 一张表;对侧账的位置留在下面的 TODO 注释处,
// 由 P3 在**同一个事务**里补(拆成两个事务就会出现"发了货没改状态");商品 / 订单行的锁按全序排在 op 行点锁之前。
//
// 只接受四个终态(isFinalizableRecord):PENDING / UNSPECIFIED 写进去等于没终结或查不到结局;
// ABANDONED 只能经 Abandon(带令牌、六条证据齐全)写入。重投循环本来就只会传这四个,这道闸防的是绕过循环的调用者。
// 同写 next_attempt_ms = 终结时刻(sqlFinalizeAssetOp),终态行清理按它判龄。
func (r *AssetOpRepo) Finalize(ctx context.Context, op assetop.Op, status assetop.Status, res assetop.Result, nowMs uint64) (bool, error) {
	if status == assetop.StatusApplied && res.Partial {
		return false, errors.New("部分到账不得终结为完整交易成功")
	}
	final := StatusToRecord(status)
	if !isFinalizableRecord(final) {
		return false, fmt.Errorf("finalize %s %d: 终结状态非法 (%s)", AssetOpTableName, op.OpID, status)
	}
	ctx, cancel := r.bounded(ctx)
	defer cancel()
	finalized := false
	err := assetop.WithTxRetry(ctx, r.db, txRetryAttempts, IsRetryableTxError, func(tx *sql.Tx) error {
		finalized = false
		found, err := lockAssetOpRow(ctx, tx, op.OpID)
		if err != nil {
			return fmt.Errorf("finalize %s %d: %w", AssetOpTableName, op.OpID, err)
		}
		if !found {
			return nil // 行已被清理:与 CAS 落空同义,不做对侧账
		}
		var refKind uint32
		if err := tx.QueryRowContext(ctx, "SELECT ref_kind FROM "+AssetOpTableName+" WHERE op_id=?", op.OpID).Scan(&refKind); err != nil {
			return err
		}
		if refKind == uint32(tradepb.TradeAssetOpRefKind_TRADE_ASSET_OP_REF_KIND_ORDER) && (!res.SQLCommitted || !res.Durable) {
			return errors.New("交易合同终结缺少玩家SQL提交证据")
		}
		result, err := tx.ExecContext(ctx, sqlFinalizeAssetOp,
			int32(final), uint32(res.Outcome), res.Reason, nowMs, nowMs, op.OpID, PendingStatus())
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
		if res.Snapshot != nil && status == assetop.StatusApplied && op.Stream == assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT {
			snapshot, marshalErr := proto.MarshalOptions{Deterministic: true}.Marshal(res.Snapshot)
			if marshalErr != nil {
				return marshalErr
			}
			if _, err := tx.ExecContext(ctx, "UPDATE "+AssetOpTableName+" SET escrow_snapshot=?,receipt_ack_pending=1 WHERE op_id=?", snapshot, op.OpID); err != nil {
				return err
			}
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
// 与 Finalize 完全同形:同一个事务、先主键点锁(sqlLockAssetOp)、同一条 `status = PENDING` 的 CAS、
// RowsAffected == 1 才算本次终结并做对侧账。两条路径共用这个条件,人工与循环同时下手也只会有一个赢家;
// 共用点锁则让两者在 TiDB 上先在 PRIMARY key 上排队,不在语句末尾的并行批锁里各持一半。
//
// durable 刻意**不**置 1:这一行的终局来自人工判定,不是 scene 确认的落盘结局,
// 把它标成 durable 会让事后对账分不清"scene 真的答过"和"人写上去的"。
// 交易合同推进只看 status(CommerceRepo.LegResult,2026-10-20),人工终结的腿照常推进合同:
// REJECTED / ABORTED 走与 scene 判拒同一条补偿;实例扣款判 APPLIED 时本行没有托管原件,合同隔离为人工,不自动上架或发货。
func (r *AssetOpRepo) ResolveManually(ctx context.Context, m assetop.ManualResolution, nowMs uint64) (bool, error) {
	// 状态合法性、操作人与理由的长度由 assetop.Loop.ResolveManually 在进来之前校验;
	// 这里兜住本表自己的那一份:映射不出库值(落成 UNSPECIFIED 会让客服查不到结局)、PENDING,
	// 以及 ABANDONED —— 放弃只能由自动放弃在全部证据成立时产生,人工判定"放弃"应按证据选 ABORTED
	// (与 assetop.ManualResolution.validate 同一口径;那道校验在包级函数里,直接调本方法会绕过它)。
	final := StatusToRecord(m.Final)
	if !isFinalizableRecord(final) {
		return false, fmt.Errorf("manual resolve %s %d: 终结状态非法 (%s)", AssetOpTableName, m.OpID, m.Final)
	}
	ctx, cancel := r.bounded(ctx)
	defer cancel()
	resolved := false
	err := assetop.WithTxRetry(ctx, r.db, txRetryAttempts, IsRetryableTxError, func(tx *sql.Tx) error {
		resolved = false
		found, err := lockAssetOpRow(ctx, tx, m.OpID)
		if err != nil {
			return fmt.Errorf("manual resolve %s %d: %w", AssetOpTableName, m.OpID, err)
		}
		if !found {
			return nil // 行不存在或已被清理:与 CAS 落空同义,不做对侧账
		}
		result, err := tx.ExecContext(ctx, sqlResolveAssetOp,
			int32(final), m.Operator, m.Reason, nowMs, nowMs, m.OpID, PendingStatus())
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
//
// last_send_ms 只在本次确实发过请求(!res.Local)时推进到 nowMs(LastSendMs 契约第 3 条,见 sqlRescheduleAssetOp)。
//
// 写法:RC 事务{sqlLockAssetOp 主键点锁 → 带令牌复核的点改},与 Finalize / guild 的 Reschedule 同形
// (见"op 行的写者:先主键点锁再 CAS")。这条改 idx_0 的 next_attempt_ms;自动提交在 TiDB 上走乐观事务、提交时按 region
// 并行 prewrite 行 key 与索引 key,会与终结的「点锁 PRIMARY → 行 key」各持一半互等,死锁检测器看不到,要等约 3s 的锁 TTL
// 才解开(2026-10-18 真 TiDB v8.5.2:自动提交 31/40 次约 3s 停顿,改成本形状 0/40;事故文档 §9.7)。
// 点锁读不到行(已被清理)与 CAS 落空同义,回 ErrLeaseLost;ErrLeaseLost 不在 IsRetryableTxError 里,不会被重跑。
func (r *AssetOpRepo) Reschedule(ctx context.Context, op assetop.Op, nextAttemptMs uint64, res assetop.Result, nowMs uint64) error {
	ctx, cancel := r.bounded(ctx)
	defer cancel()
	return assetop.WithTxRetry(ctx, r.db, txRetryAttempts, IsRetryableTxError, func(tx *sql.Tx) error {
		found, err := lockAssetOpRow(ctx, tx, op.OpID)
		if err != nil {
			return fmt.Errorf("reschedule %s %d: %w", AssetOpTableName, op.OpID, err)
		}
		if !found {
			return fmt.Errorf("%w: op_id=%d(行已不存在)", assetop.ErrLeaseLost, op.OpID)
		}
		execRes, err := tx.ExecContext(ctx, sqlRescheduleAssetOp,
			nextAttemptMs, res.Durable, uint32(res.Outcome), res.Reason, nowMs,
			!res.Local, nowMs,
			op.OpID, PendingStatus(), op.LeaseToken)
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
	})
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

// ---------------------------------------------------------------------------
// 落地恢复:唤醒 / 日对账 / 自动放弃(docs/design/landing-recovery-sync-digest.md §5.3、§6.2、§6.3)
// ---------------------------------------------------------------------------
//
// 与 guild 的 GuildAssetStore 同口径实现,避免两份分叉。trade 的资产通道默认关闭、P3 未开工,
// 所以这里只有存储侧;对侧账(商品状态迁移、退款)与 Finalize 一样留在 P3 的 TODO 处。

const (
	// sqlListPendingByPlayer:落地唤醒按玩家列 PENDING 行,走 idx_trade_asset_op_1
	// (player_id, stream, stream_epoch, status, seq)的 player_id 前缀,status 在索引里过滤。
	// 非加锁读;不看 next_attempt_ms / lease_until_ms(唤醒就是要提前,租约由随后的 Claim CAS 判)。
	// 按 (stream, stream_epoch, seq) 升序:与索引顺序一致,也让同一条流里先发的 seq 先投。
	sqlListPendingByPlayer = "SELECT `op_id` FROM " + AssetOpTableName +
		" WHERE `player_id` = ? AND `status` = ? ORDER BY `stream` ASC, `stream_epoch` ASC, `seq` ASC LIMIT ?"

	// sqlListPendingFactsPage:日对账按主键分页非加锁读全部 PENDING 行的分类所需列。
	sqlListPendingFactsPage = "SELECT `op_id`, `stream`, `created_ms`, `attempts`, `last_outcome`, `last_reason` FROM " +
		AssetOpTableName + " WHERE `status` = ? AND `op_id` > ? ORDER BY `op_id` ASC LIMIT ?"

	// sqlListAbandonCandidates:自动放弃的候选(非加锁读,按 op_id 翻页)。只给候选,判定在持租约之后逐行复核。
	// 只列 abandonableStream(TRADE_DEBIT)的行,理由见 abandonableStream。
	sqlListAbandonCandidates = "SELECT `op_id` FROM " + AssetOpTableName +
		" WHERE `status` = ? AND `stream` = ? AND `created_ms` < ? AND `op_id` > ? ORDER BY `op_id` ASC LIMIT ?"

	// sqlAbandonAssetOp:放弃 CAS,以 (op_id, status=PENDING, lease_token) 为条件(assetop.Abandoner 契约)。
	// 不置 durable(不是 scene 确认的结局)、不改 last_outcome(保留最后一次真实答复作证据);
	// next_attempt_ms 同写终结时刻(与 sqlFinalizeAssetOp 一致,清理按它判龄)。
	// 交易合同的腿读结局只看 status、不看 durable(CommerceRepo.LegResult / LegOutcome.Final,2026-10-20),
	// 所以这里不置 durable 不会再让合同停在托管阶段。
	sqlAbandonAssetOp = "UPDATE " + AssetOpTableName +
		" SET `status` = ?, `lease_until_ms` = 0, `next_attempt_ms` = ?, `updated_ms` = ?" +
		" WHERE `op_id` = ? AND `status` = ? AND `lease_token` = ?"

	// sqlReleaseAssetOpLease:放弃判定"这一轮不放弃"之后归还租约。只清租约,不动 attempts / next_attempt_ms /
	// last_send_ms(LastSendMs 契约第 4 条)。lease_until_ms / updated_ms 都不在二级索引里,与 Claim 同为
	// 只改聚簇记录的自动提交单行 UPDATE。
	sqlReleaseAssetOpLease = "UPDATE " + AssetOpTableName + " SET `lease_until_ms` = 0, `updated_ms` = ?" +
		" WHERE `op_id` = ? AND `status` = ? AND `lease_token` = ?"
)

// auditPageSize 是日对账每页读多少行。按主键分页,不让一条大查询长时间占着连接(PendingClassCounter 契约)。
const auditPageSize = 500

// abandonableStream 是自动放弃**唯一**允许处理的流:TRADE_DEBIT(上架托管,从卖家扣出)。
//
// 为什么不放开 TRADE_CREDIT:trade 的对侧账还没有实现(P3,见 Abandon 的 TODO)。放弃一条 ESCROW_DEBIT 只是"scene 从未扣过
// 卖家",没有钱要退,只终结并留痕不会让任何人吃亏;放弃一条 DELIVER_CREDIT 却意味着"这次交付作废" —— 买家已付的款不按订单
// 退回,就是钱货两空。通道一开自动放弃默认就开着(AbandonAfterDays 默认 180),只靠 TODO 注释提醒"P3 上线 CREDIT 生产者
// 之前先补对侧账"拦不住漏补,所以在存储层把 CREDIT 挡在候选之外(玩家资产路径 fail-closed),Abandon 自己再按流拒一次。
// **对侧账在 Abandon 里与 Finalize 的 REJECTED / ABORTED 分支同一份逻辑落地之后**,再把 TRADE_CREDIT 加进来(改这里与两处用法,
// 并改 asset_op_recovery_test.go 的 TestAbandonOnlyTouchesDebitStream)。
//
// 日对账(CountPendingByClass)不按流区分放弃候选:超龄的 CREDIT 行仍会计进 abandon_candidate 类(口径在 assetop,本表只喂列)。
// v1 没有任何 CREDIT 生产者,这种行不存在;P3 若先上 CREDIT 生产者、后补对侧账,这一类的计数会多出"永远不会被放弃"的行。
const abandonableStream = assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT

// ListPendingByPlayer 实现 assetop.PlayerPendingLister:列出某玩家的 PENDING 行主键,条数 <= limit。
// 没有 PENDING 行时回 (nil, nil)。limit <= 0 直接回空,不发 SQL。
func (r *AssetOpRepo) ListPendingByPlayer(ctx context.Context, playerID uint64, limit int) ([]uint64, error) {
	if limit <= 0 {
		return nil, nil
	}
	ctx, cancel := r.bounded(ctx)
	defer cancel()
	ids, err := r.queryOpIDs(ctx, sqlListPendingByPlayer, playerID, PendingStatus(), limit)
	if err != nil {
		return nil, fmt.Errorf("list pending %s by player: %w", AssetOpTableName, err)
	}
	return ids, nil
}

// CountPendingByClass 实现 assetop.PendingClassCounter:按主键分页非加锁读全部 PENDING 行,
// 逐行交给 assetop.PendingTally(内部用 assetop.ClassifyPending)分类计数 —— 口径只在 assetop 一处,
// 这里不另写 CASE。abandonBeforeMs 由循环按 AbandonAfter 算好传入(0 = 不分出 abandon_candidate)。
//
// 每页各自带 opTimeout 上限;任何一页失败整轮作废(返回错误,循环保留上一轮的 gauge),不报半截计数。
func (r *AssetOpRepo) CountPendingByClass(ctx context.Context, nowMs, abandonBeforeMs uint64) ([]assetop.ClassCount, error) {
	tally := assetop.NewPendingTally(nowMs, abandonBeforeMs)
	var afterOpID uint64
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("count pending %s by class: %w", AssetOpTableName, err)
		}
		n, lastOpID, err := r.tallyPendingPage(ctx, tally, afterOpID)
		if err != nil {
			return nil, fmt.Errorf("count pending %s by class (after op_id %d): %w", AssetOpTableName, afterOpID, err)
		}
		if n < auditPageSize {
			return tally.Counts(), nil
		}
		afterOpID = lastOpID
	}
}

// tallyPendingPage 读一页(op_id > afterOpID 的至多 auditPageSize 行)并累加,返回本页行数与最后一个 op_id。
func (r *AssetOpRepo) tallyPendingPage(ctx context.Context, tally *assetop.PendingTally, afterOpID uint64) (int, uint64, error) {
	ctx, cancel := r.bounded(ctx)
	defer cancel()
	rows, err := r.db.QueryContext(ctx, sqlListPendingFactsPage, PendingStatus(), afterOpID, auditPageSize)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = rows.Close() }()
	var (
		n      int
		lastID uint64
	)
	for rows.Next() {
		var opID, createdMs uint64
		var stream, attempts, outcome, reason uint32
		if err := rows.Scan(&opID, &stream, &createdMs, &attempts, &outcome, &reason); err != nil {
			return 0, 0, fmt.Errorf("scan pending facts: %w", err)
		}
		tally.Add(assetop.PendingFacts{
			Stream:      assetpb.AssetOpStream(stream),
			CreatedMs:   createdMs,
			Attempts:    attempts,
			LastOutcome: assetpb.AssetOpOutcome(outcome),
			LastReason:  reason,
		})
		n++
		lastID = opID
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("iterate pending facts: %w", err)
	}
	return n, lastID, nil
}

// ListAbandonCandidates 实现 assetop.Abandoner:status=PENDING 且 stream=TRADE_DEBIT 且 created_ms < createdBeforeMs
// 且 op_id > afterOpID,按 op_id 升序,只回主键,条数 <= limit。非加锁读;真正的判定在持租约之后由 assetop 逐行复核。
// 只列 TRADE_DEBIT(abandonableStream):CREDIT 流的对侧账未落地之前一行都不进候选。
func (r *AssetOpRepo) ListAbandonCandidates(ctx context.Context, createdBeforeMs, afterOpID uint64, limit int) ([]uint64, error) {
	if limit <= 0 {
		return nil, nil
	}
	ctx, cancel := r.bounded(ctx)
	defer cancel()
	ids, err := r.queryOpIDs(ctx, sqlListAbandonCandidates,
		PendingStatus(), uint32(abandonableStream), createdBeforeMs, afterOpID, limit)
	if err != nil {
		return nil, fmt.Errorf("list abandon candidates %s: %w", AssetOpTableName, err)
	}
	return ids, nil
}

// Abandon 实现 assetop.Abandoner:以 (op_id, status=PENDING, lease_token) 为条件把行终结为 ABANDONED。
// RowsAffected == 0 → assetop.ErrLeaseLost(租约在判定期间被别人接管,别人可能已经发出请求,此时绝不能放弃)。
//
// 放弃的六条证据(持租约、玩家无位置、距最后一次可能的发送超过安全间隔、已落盘账本确定未见……)由
// assetop.Loop.AbandonOnce 在调用之前逐条拿到;本方法只负责"带令牌的终结"这最后一步。碰库之前拒绝两种入参(都不是租约
// 被接管,所以不回 ErrLeaseLost):
//   - 令牌为 0:0 是"没有租约"的形态,拿它做 CAS 不能证明"这一刻租约在我手里",而放弃不可逆;
//   - 流不是 TRADE_DEBIT:CREDIT 流的对侧账未落地,放弃交付会让买家钱货两空(abandonableStream)。候选读已按流过滤,
//     这里再拒一次,挡住绕过候选读的调用者。
//
// 与 Finalize 同形:一个事务(WithTxRetry,RC),先 sqlLockAssetOp 主键点锁、再这一条 trade_asset_op 的主键 CAS
// (见"op 行的写者:先主键点锁再 CAS")。点锁读不到行(已被清理,只删终态行)同样回 ErrLeaseLost。
func (r *AssetOpRepo) Abandon(ctx context.Context, op assetop.Op, nowMs uint64) error {
	if op.LeaseToken == 0 {
		return fmt.Errorf("abandon %s %d: 缺领取令牌,不放弃", AssetOpTableName, op.OpID)
	}
	if op.Stream != abandonableStream {
		return fmt.Errorf("abandon %s %d: 流 %d 不允许自动放弃(对侧账未落地之前只放弃 TRADE_DEBIT)",
			AssetOpTableName, op.OpID, int32(op.Stream))
	}
	ctx, cancel := r.bounded(ctx)
	defer cancel()
	return assetop.WithTxRetry(ctx, r.db, txRetryAttempts, IsRetryableTxError, func(tx *sql.Tx) error {
		found, err := lockAssetOpRow(ctx, tx, op.OpID)
		if err != nil {
			return fmt.Errorf("abandon %s %d: %w", AssetOpTableName, op.OpID, err)
		}
		if !found {
			return fmt.Errorf("%w: abandon op_id=%d(行已不存在)", assetop.ErrLeaseLost, op.OpID)
		}
		result, err := tx.ExecContext(ctx, sqlAbandonAssetOp,
			int32(tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_ABANDONED), nowMs, nowMs,
			op.OpID, PendingStatus(), op.LeaseToken)
		if err != nil {
			return fmt.Errorf("abandon %s %d: %w", AssetOpTableName, op.OpID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("abandon %s %d rows affected: %w", AssetOpTableName, op.OpID, err)
		}
		if affected == 0 {
			return fmt.Errorf("%w: abandon op_id=%d", assetop.ErrLeaseLost, op.OpID)
		}
		// TODO(P3,docs/design/landing-recovery-sync-digest.md §6.3「trade:对侧账尚未实现」):对侧账在这里,
		// 与 Finalize 的 REJECTED / ABORTED 分支**同一份**逻辑、同一个事务(拆开会出现"终结了却没退"):
		//   ESCROW_DEBIT   → 商品 ESCROWING → ESCROW_REJECTED(scene 从未扣过卖家,没有钱要退);
		//   DELIVER_CREDIT → 买家已付的款按订单退回(放弃意味着这次交付作废,不退就是买家钱货两空);
		//   RETURN_CREDIT  → 商品 RETURNING 停在原地,转人工(物品仍在托管里)。
		// 在那之前 CREDIT 流由 abandonableStream 挡在候选与本方法之外(存储层 fail-closed,不靠这段注释);
		// 补齐 CREDIT 两行之后再放开 abandonableStream。v1 只放弃 ESCROW_DEBIT,只终结并留痕
		// (审计依据:本行的 op_id / ref_id、assetop_abandoned_total 与 assetop 打的放弃日志)。
		//
		// 交易合同的扣款腿(ref_kind=ORDER)不等这条 TODO(2026-10-20,第一轮审计 §2.5 go-asset-services#1):
		// 合同引擎在锁住合同行的事务里读到 ABANDONED,按"没扣到"走与 scene 判拒同一条补偿(退回另一方已托管的资产、
		// 寄售重新开放),见 commerce.Engine.advance。补偿不在本事务里做:本方法只锁 op 行、合同推进只锁合同行再普通读 op 行,
		// 两边不成环;终态不再变化,合同下一轮必然读到。上面三行 TODO 只剩旧 P1 商品(ref_kind=LISTING)。
		return nil
	})
}

// ReleaseLease 实现 assetop.Abandoner:判定"这一轮不放弃"之后归还租约(sqlReleaseAssetOpLease)。
// RowsAffected == 0 → assetop.ErrLeaseLost(行已被别人接管或已终结,没有可还的)。
func (r *AssetOpRepo) ReleaseLease(ctx context.Context, op assetop.Op, nowMs uint64) error {
	ctx, cancel := r.bounded(ctx)
	defer cancel()
	result, err := r.db.ExecContext(ctx, sqlReleaseAssetOpLease, nowMs, op.OpID, PendingStatus(), op.LeaseToken)
	if err != nil {
		return fmt.Errorf("release lease %s %d: %w", AssetOpTableName, op.OpID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("release lease %s %d rows affected: %w", AssetOpTableName, op.OpID, err)
	}
	if affected == 0 {
		return fmt.Errorf("%w: release lease op_id=%d", assetop.ErrLeaseLost, op.OpID)
	}
	return nil
}

// queryOpIDs 跑一条只回 op_id 的非加锁读。
func (r *AssetOpRepo) queryOpIDs(ctx context.Context, query string, args ...any) ([]uint64, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
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

// ---------------------------------------------------------------------------
// 终态行清理(对齐 guild 的 GuildAssetStore.CleanupOnce 终态部分;docs/design/guild-phase2/05-economy.md §5.22)
// ---------------------------------------------------------------------------
//
// 保留期(默认 30 天,config.AssetOpConf.TerminalRetentionDays)外的终态行逐行删掉,表不再无界增长。
//   - 只删 APPLIED / REJECTED / ABORTED / ABANDONED(cleanupStatuses)。**不删** APPLIED_PARTIAL(部分发放要人工补偿,
//     证据不能自动消失,X-15)与 PENDING(永不删)。ABANDONED 与其它终态同等对待(落地恢复设计 §6.3「状态与展示」)。
//   - 判龄按 next_attempt_ms:终态行上它等于终结时刻(Finalize / ResolveManually / Abandon 同写),走 idx_0。
//   - 每批"普通读至多 500 个候选主键 → 按主键升序逐行一个 RC 短事务{主键点锁 → 带复核条件的主键点删}",
//     形状与 guild 的 execCleanupDelete 相同:每个短事务只锁这一行的聚簇记录;终态行没有任何别的写者
//     (领取 / 重排 / 毒行 / 终结 / 放弃都带 status = PENDING 条件),所以清理只会单向等待、不会成环。
//   - 每行只尝试一次,失败的行下一轮再删(条件仍满足,不丢任何东西)。
//
// 与 guild 不同的一处:guild 删终态行之前先推进"清理水位"(asset_op_cleanup_watermark.go),因为 data_service 的
// 回档检查要靠它判断哪一段流水已经可能被删(07-rollback-fail-closed.md §7.4.2)。trade 目前**没有**回档检查消费
// trade_asset_op(只有 guild 的 GuildInternal.ListAppliedAssetOpsSince),所以不写水位;P3 若让回档检查覆盖 trade,
// 必须先补同样的 write-ahead 水位再开清理,否则调大保留期之后那段已删的流水会被误当成"可证明且没有分歧"。
//
// 本组语句只碰 trade_asset_op,一行都不碰 trade_player_op_seq(seq 行"永不删除"是"seq 行的建行"一节的前提)。

// CleanupConf 是终态行清理的节律与保留期。配置名与 guild 相同(CleanupIntervalMinutes / TerminalRetentionDays)。
type CleanupConf struct {
	Interval          time.Duration
	TerminalRetention time.Duration
}

// 清理节律,与 guild 同值(05 §5.22):单批 500 行、批间 100ms、每轮至多 20 批,一批候选不足 500 行即停。
const (
	cleanupBatchSize  = 500
	cleanupBatchPause = 100 * time.Millisecond
	cleanupMaxBatches = 20
	// cleanupStmtBudget 是单行清理短事务(点锁 + 点删 + 提交)的上限,与 guild 同值。
	// 跑满说明库有状况,本轮到此为止,已删的行照常计数,下一轮接着删。
	cleanupStmtBudget = 2 * time.Second
)

// 清理点删之前的主键点锁与终结类写者共用 sqlLockAssetOp(同一形状,见"op 行的写者:先主键点锁再 CAS")。
const (
	sqlListCleanupTerminalOps = "SELECT `op_id` FROM " + AssetOpTableName +
		" WHERE `status` IN (?, ?, ?, ?) AND `receipt_ack_pending`=0 AND (`ref_kind`<>2 OR `business_consumed`=1) AND `next_attempt_ms` < ? ORDER BY `op_id` ASC LIMIT ?"
	sqlCleanupTerminalOp = "DELETE FROM " + AssetOpTableName +
		" WHERE `op_id` = ? AND `status` IN (?, ?, ?, ?) AND `receipt_ack_pending`=0 AND (`ref_kind`<>2 OR `business_consumed`=1) AND `next_attempt_ms` < ?"
)

// cleanupStatuses 是可被清理的四个终态的库值,顺序即两条清理语句里 IN (?, ?, ?, ?) 的参数顺序。
func cleanupStatuses() [4]int32 {
	return [4]int32{
		int32(tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED),
		int32(tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_REJECTED),
		int32(tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_ABORTED),
		int32(tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_ABANDONED),
	}
}

// cleanupDeletedTotal 是清理删掉的终态行数。与本文件上方的 seqRowMissingInTxTotal 同一个 registry、同一种无 label 形状:
// 长期为 0 而表在长,说明清理没在跑。
var cleanupDeletedTotal = promauto.With(prometheus.DefaultRegisterer).NewCounter(prometheus.CounterOpts{
	Subsystem: "trade",
	Name:      "assetop_cleanup_deleted_total",
	Help:      "终态资产指令清理删掉的行数(保留期外的 APPLIED / REJECTED / ABORTED / ABANDONED;APPLIED_PARTIAL 与 PENDING 永不删)。",
})

// recordCleanupDeleted:生产实现计指标;单测可替换为记录器(同 guild 的惯例,不去读全局注册表)。
var recordCleanupDeleted = func(n int64) { cleanupDeletedTotal.Add(float64(n)) }

// cleanupPoint 是清理 goroutine 与每一轮 recover 的 safego 点位名(safego_panic_total 的 label,必须是常量)。
const cleanupPoint = "trade.asset_op_cleanup"

// RunCleanup 阻塞运行终态行清理循环,ctx 取消即返回;调用方用 safego.Go(cleanupPoint 同名点位)启动,
// 形状与 guild 的 GuildAssetStore.RunCleanup 相同。
//
// 每个副本都跑:删除幂等,多副本同时跑无害;随机初始延迟 [0, Interval) 只是把各副本错开,少抢同一批行锁。
// 每一轮各自 recover:单轮 panic 只丢那一轮,清理不会从此静默停摆。
// Interval 或 TerminalRetention 非正时不启动(打 ERROR):config.Validate 在 CleanupEnabled 时已拦,这里只防绕过装配层。
func (r *AssetOpRepo) RunCleanup(ctx context.Context, c CleanupConf) {
	if c.Interval <= 0 || c.TerminalRetention <= 0 {
		logx.Errorf("[TradeAsset] Invalid cleanup config (interval=%v terminal_retention=%v); cleanup task not started",
			c.Interval, c.TerminalRetention)
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
		safego.Run(cleanupPoint, func() {
			deleted, err := r.CleanupTerminalOps(ctx, time.Now(), c.TerminalRetention)
			if err != nil && ctx.Err() == nil {
				logx.Errorf("[TradeAsset] Terminal asset op cleanup failed (deleted %d before the error): %v", deleted, err)
				return
			}
			if deleted > 0 {
				logx.Infof("[TradeAsset] Terminal asset op cleanup deleted %d row(s) older than %v", deleted, c.TerminalRetention)
			}
		})
		timer.Reset(c.Interval)
	}
}

// CleanupTerminalOps 跑一轮终态行清理(可测:时刻由调用方给),返回本轮删掉的行数。
// retention <= 0 拒绝执行(删光所有终态行不可能是有意的配置);时钟早于保留期时没有可删的行。
// 出错时已删的行数仍如实返回、照常计指标(每行各自提交,删掉的就是删掉了)。
func (r *AssetOpRepo) CleanupTerminalOps(ctx context.Context, now time.Time, retention time.Duration) (int64, error) {
	if retention <= 0 {
		return 0, fmt.Errorf("trade asset cleanup: 保留期必须为正(当前 %v)", retention)
	}
	nowMs := now.UnixMilli()
	retentionMs := retention.Milliseconds()
	if nowMs <= retentionMs {
		return 0, nil
	}
	cutoffMs := uint64(nowMs - retentionMs)

	var total int64
	for i := 0; i < cleanupMaxBatches; i++ {
		candidates, deleted, err := r.cleanupTerminalOpsBatch(ctx, cutoffMs)
		total += deleted
		if deleted > 0 {
			recordCleanupDeleted(deleted)
		}
		if err != nil {
			return total, fmt.Errorf("cleanup %s: %w", AssetOpTableName, err)
		}
		// 判"还有没有下一批"看候选数而不是删除数:复核落空的候选下一次候选读不会再读到它。
		if candidates < cleanupBatchSize {
			return total, nil
		}
		timer := time.NewTimer(cleanupBatchPause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return total, ctx.Err()
		case <-timer.C:
		}
	}
	return total, nil
}

// cleanupTerminalOpsBatch:普通读至多 cleanupBatchSize 个保留期外的终态 op_id(升序),逐行短事务点锁 + 点删并复核条件。
func (r *AssetOpRepo) cleanupTerminalOpsBatch(ctx context.Context, cutoffMs uint64) (int, int64, error) {
	st := cleanupStatuses()
	readCtx, cancel := r.bounded(ctx)
	opIDs, err := r.queryOpIDs(readCtx, sqlListCleanupTerminalOps, st[0], st[1], st[2], st[3], cutoffMs, cleanupBatchSize)
	cancel()
	if err != nil {
		return 0, 0, fmt.Errorf("list terminal %s: %w", AssetOpTableName, err)
	}
	var deleted int64
	for _, opID := range opIDs {
		n, err := r.deleteTerminalOp(ctx, opID, cutoffMs)
		deleted += n
		if err != nil {
			return len(opIDs), deleted, fmt.Errorf("delete terminal %s %d: %w", AssetOpTableName, opID, err)
		}
	}
	return len(opIDs), deleted, nil
}

// deleteTerminalOp 在一个 RC 短事务里删一行:先主键点锁(不带复核条件),再跑带复核条件的主键点删。
// 返回删掉的行数(0 或 1):点锁读不到 = 已被别的副本删掉;复核落空 = 条件已不成立。
// 超过 1 行只可能是 WHERE 写坏了(不再是完整主键等值),整体回滚并报错。尝试次数为 1(失败的行下一轮再删)。
func (r *AssetOpRepo) deleteTerminalOp(ctx context.Context, opID, cutoffMs uint64) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, cleanupStmtBudget)
	defer cancel()
	st := cleanupStatuses()
	var deleted int64
	err := assetop.WithTxRetry(ctx, r.db, 1, IsRetryableTxError, func(tx *sql.Tx) error {
		deleted = 0 // 重试契约:结果只在成功返回前写到外层
		found, err := lockAssetOpRow(ctx, tx, opID)
		if err != nil {
			return err
		}
		if !found {
			return nil // 已被别的副本删掉
		}
		result, err := tx.ExecContext(ctx, sqlCleanupTerminalOp, opID, st[0], st[1], st[2], st[3], cutoffMs)
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

// 交易合同(ref_kind=2)名下 op 行的锁定写,供 commerce 引擎在**已锁合同行**的事务里调用。
//
// 以前引擎直接发 `UPDATE trade_asset_op … WHERE ref_kind=2 AND ref_id=?`:在 TiDB 悲观事务里它走 idx_2,
// 读阶段先锁行 key、语句末尾才锁非聚簇 PRIMARY 与 uk;终结 / 人工终结 / 放弃的点锁是「先 PRIMARY 再行 key」,
// 两边在同一行的两把 key 上反序,真 TiDB v8.5.2 受控编排 5/5 报 1213、自然竞态 246 次里 10 次(2026-10-18,事故文档 §9.7)。
// 现在的形状:候选普通读(按 idx_2 拿 op_id,不加锁)→ 按 op_id 升序逐行 lockAssetOpRow → 主键点改,
// 锁序变成「合同行 → op 行(PRIMARY → 行 key)」,与终结同向,同组实验 0/5、0/246。
// 候选读不漏行:同一合同的 op 行只由 advance 在锁住合同行之后插入,调用方已持有合同行锁。
const (
	sqlListOpIDsByCommerce = "SELECT `op_id` FROM " + AssetOpTableName + " WHERE `ref_kind` = 2 AND `ref_id` = ? ORDER BY `op_id`"
	// 取消:把仍待办的买方托管扣款(stream=3)截止提前到现在;复核谓词与原批量语句逐字相同,只换成主键点改。
	sqlExpireEscrowDebitOp = "UPDATE " + AssetOpTableName + " SET `deadline_ms` = ? WHERE `op_id` = ? AND `stream` = 3 AND `status` = 1"
	sqlConsumeCommerceOp   = "UPDATE " + AssetOpTableName + " SET `business_consumed` = 1 WHERE `op_id` = ?"
)

// ExpireCommerceEscrowDebitsTx 把合同 commerceID 名下仍待办的托管扣款截止提前到 nowMs(取消合同时)。
func (r *AssetOpRepo) ExpireCommerceEscrowDebitsTx(ctx context.Context, tx *sql.Tx, commerceID, nowMs uint64) error {
	return r.updateCommerceOpsTx(ctx, tx, commerceID, "expire escrow debit", sqlExpireEscrowDebitOp, func(opID uint64) []any {
		return []any{nowMs, opID}
	})
}

// MarkCommerceOpsConsumedTx 把合同 commerceID 名下的 op 行标成业务已消费(合同结案时)。
func (r *AssetOpRepo) MarkCommerceOpsConsumedTx(ctx context.Context, tx *sql.Tx, commerceID uint64) error {
	return r.updateCommerceOpsTx(ctx, tx, commerceID, "mark consumed", sqlConsumeCommerceOp, func(opID uint64) []any {
		return []any{opID}
	})
}

// updateCommerceOpsTx:候选普通读 → 按 op_id 升序逐行「主键点锁 → 主键点改」。点锁读不到行(已被清理)就跳过。
// 错误原样 %w:1213 / 1205 / 9007 由调用方的 WithTxRetry 整事务重跑。
func (r *AssetOpRepo) updateCommerceOpsTx(ctx context.Context, tx *sql.Tx, commerceID uint64, what, stmt string, args func(opID uint64) []any) error {
	rows, err := tx.QueryContext(ctx, sqlListOpIDsByCommerce, commerceID)
	if err != nil {
		return fmt.Errorf("%s: list %s of commerce %d: %w", what, AssetOpTableName, commerceID, err)
	}
	var opIDs []uint64
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return fmt.Errorf("%s: scan op_id of commerce %d: %w", what, commerceID, err)
		}
		opIDs = append(opIDs, id)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("%s: list %s of commerce %d: %w", what, AssetOpTableName, commerceID, err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%s: list %s of commerce %d: %w", what, AssetOpTableName, commerceID, err)
	}
	for _, id := range opIDs { // SQL 已按 op_id 升序
		found, err := lockAssetOpRow(ctx, tx, id)
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		if !found {
			continue
		}
		if _, err := tx.ExecContext(ctx, stmt, args(id)...); err != nil {
			return fmt.Errorf("%s %s %d: %w", what, AssetOpTableName, id, err)
		}
	}
	return nil
}

// lockAssetOpRow 在调用方的事务里跑 sqlLockAssetOp(op 行的主键点锁),报告行是否存在。
// 必须是该事务里碰 trade_asset_op 的**第一条**语句,随后才是带复核谓词的点改 / 点删(见"op 行的写者:先主键点锁再 CAS")。
// 读不到行 = 已被清理(清理只删保留期外的终态行),回 (false, nil),由调用方按各自的"落空"语义处理。
// 错误原样 %w 包一层:1213 / 1205 / 9007 要能被 IsRetryableTxError 认出来,整事务重跑。
func lockAssetOpRow(ctx context.Context, tx *sql.Tx, opID uint64) (bool, error) {
	var locked uint64
	switch err := tx.QueryRowContext(ctx, sqlLockAssetOp, opID).Scan(&locked); {
	case err == nil:
		return true, nil
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	default:
		return false, fmt.Errorf("lock %s %d: %w", AssetOpTableName, opID, err)
	}
}

// getOp 按主键取全列。
func (r *AssetOpRepo) getOp(ctx context.Context, opID uint64) (*tradepb.TradeAssetOpRecord, error) {
	row := r.db.QueryRowContext(ctx, "SELECT "+assetOpColumns+" FROM "+AssetOpTableName+" WHERE `op_id` = ?", opID)
	rec, err := scanAssetOp(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// 刚 CAS 成功又查不到:只可能是被并发删了(清理只删保留期外的终态行,刚领到的未决行不在其列),按故障返回。
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
		&rec.ResolvedBy, &rec.ResolveReason, &rec.LastSendMs,
		&rec.EscrowSnapshot, &rec.ReceiptAckPending, &rec.BusinessConsumed,
	); err != nil {
		return nil, err
	}
	rec.Kind = tradepb.TradeAssetOpKind(kind)
	rec.Status = tradepb.TradeAssetOpStatus(status)
	rec.RefKind = tradepb.TradeAssetOpRefKind(refKind)
	return rec, nil
}

// StatusToRecord 把 assetop 的语义终态映射成本表的存储值。
// assetop.Status 刻意不绑库值(§4.43 #23),映射只此一处;它是单射,每个非 UNSPECIFIED 的库值恰好对应
// 一个语义值(asset_op_recovery_test.go 的 TestStatusToRecordIsABijection 机械守住"两边同改")。
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
	case assetop.StatusAbandoned:
		return tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_ABANDONED
	default:
		// 未知语义值不能落成 PENDING(会被循环反复领走),也不能落成 APPLIED(会伪装成功)。
		return tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_UNSPECIFIED
	}
}

// isFinalizableRecord:Finalize 与 ResolveManually 只接受这四个终态。
//   - PENDING 写进去等于没终结,UNSPECIFIED 会让客服查不到结局;
//   - ABANDONED 只能经 Abandon 写入(带领取令牌,且六条证据由 assetop.Loop.AbandonOnce 逐条复核过)。
func isFinalizableRecord(st tradepb.TradeAssetOpStatus) bool {
	switch st {
	case tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED,
		tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_REJECTED,
		tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_ABORTED,
		tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED_PARTIAL:
		return true
	default:
		return false
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
