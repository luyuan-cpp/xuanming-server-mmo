package assetop

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	// 退避抖动用 math/rand 而不是 reconcile.go 那样的 crypto/rand:抖动只是把重试打散,
	// 被猜到不产生任何后果;需要不可预测的是租约令牌(reconcile.go newLeaseToken)。
	mathrand "math/rand/v2"
	"regexp"
	"time"

	assetpb "proto/common/asset"
)

// seq 分配(规格 §4.19 seq.go、§4.37 seq.go)。
//
// 本文件只提供**在调用方事务里**执行的两段 SQL,不持有连接、不建表、不决定表结构。
// 表名由调用方给,因此必须先过白名单正则:它会被拼进 SQL,是唯一的注入面。
//
// 文件下半段是这两段 SQL 所依赖的事务基座(隔离级 / 重试 / 退避)。它和 seq 放在一起,
// 是因为 seq 的正确性完全建立在「同一把锁序、同一个隔离级、失败时整事务重跑」之上;
// 拆开之后很容易出现「分配用一套事务参数、业务写用另一套」。

// tableNameRe 是允许的表名形状:小写字母开头,后接小写字母 / 数字 / 下划线,总长 ≤63。
var tableNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// SeqTables 指明业务方的两张表,以及该表里「未决」在 status 列上的**数值**。
//
// PendingStatus 必须与业务 Store 写 outbox 行时用的数值一致;assetop 自己的
// StatusPending 只表达语义,不参与 SQL(见 types.go 的 Status 注释)。
type SeqTables struct {
	SeqTable      string
	OpTable       string
	PendingStatus uint32
}

// NewSeqTables 校验表名并返回配置。在装配层调用一次即可,失败就别启动服务。
func NewSeqTables(seq, op string, pendingStatus uint32) (SeqTables, error) {
	t := SeqTables{SeqTable: seq, OpTable: op, PendingStatus: pendingStatus}
	if err := t.validate(); err != nil {
		return SeqTables{}, err
	}
	return t, nil
}

func (t SeqTables) validate() error {
	if !tableNameRe.MatchString(t.SeqTable) {
		return fmt.Errorf("assetop: 非法的 seq 表名 %q", t.SeqTable)
	}
	if !tableNameRe.MatchString(t.OpTable) {
		return fmt.Errorf("assetop: 非法的 op 表名 %q", t.OpTable)
	}
	return nil
}

// Alloc 是一次分配的结果:纪元 + 流水号。两个一起才能唯一定位一笔业务。
type Alloc struct {
	Epoch uint64
	Seq   uint64
}

// EnsureSeqRow 保证 (player, stream) 的 seq 行存在,纪元取**建行时刻**的毫秒。
//
// 刻意在事务**外**用 autocommit 执行:两个并发请求同时对一张不存在的行做
// 「加锁读 → 没有 → 插入」时,共享锁升级会死锁。INSERT IGNORE 天然幂等,
// 已存在时不会改动纪元 —— 纪元一旦写下就不再变,库重建后重新建行自然得到更大的纪元
// (规格 §4.29;按库恢复后由运维手册显式抬高)。
func EnsureSeqRow(ctx context.Context, db *sql.DB, t SeqTables, playerID uint64, s assetpb.AssetOpStream, nowMs uint64) error {
	if err := t.validate(); err != nil {
		return err
	}
	if nowMs == 0 {
		return errors.New("assetop: 纪元必须为正(nowMs=0)")
	}
	q := fmt.Sprintf("INSERT IGNORE INTO %s (player_id, stream, next_seq, epoch, updated_ms) VALUES (?, ?, 1, ?, ?)", t.SeqTable)
	if _, err := db.ExecContext(ctx, q, playerID, uint32(s), nowMs, nowMs); err != nil {
		return fmt.Errorf("assetop: 建 seq 行失败(player=%d stream=%d): %w", playerID, s, err)
	}
	return nil
}

// AllocateSeq 在**调用方的业务事务内**分配下一个 seq,并顺带回报当前纪元。
//
// 锁序(帮会):guild → guild_member → guild_player_op_seq → guild_asset_op。
// 本函数按这个顺序先锁 seq 行、再锁未决 op 行;调用方必须把本函数放在业务锁之后、
// 插入 op 行之前,否则两条路径的加锁顺序相反就会死锁。
//
// 未决行用**加锁读**而不是 COUNT(*):业务事务里通常已经做过普通读,MySQL 可重复读的
// 一致性快照看不到并发分配者刚插入的行;加锁读总读最新版本,MySQL 与 TiDB 都支持。
//
// 返回 ErrTooManyPending 表示守卫拒绝(不是故障):该玩家该流积压太多,再发就可能让
// 未决 seq 滑出 scene 的窗口,那时结局就查不回来了。
func AllocateSeq(ctx context.Context, tx *sql.Tx, t SeqTables, playerID uint64, s assetpb.AssetOpStream, lim Limits, nowMs uint64) (Alloc, error) {
	if err := t.validate(); err != nil {
		return Alloc{}, err
	}
	if err := lim.validate(); err != nil {
		return Alloc{}, err
	}

	var nextSeq, epoch uint64
	seqQuery := fmt.Sprintf("SELECT next_seq, epoch FROM %s WHERE player_id = ? AND stream = ? FOR UPDATE", t.SeqTable)
	if err := tx.QueryRowContext(ctx, seqQuery, playerID, uint32(s)).Scan(&nextSeq, &epoch); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Alloc{}, ErrSeqRowMissing
		}
		return Alloc{}, fmt.Errorf("assetop: 读 seq 行失败(player=%d stream=%d): %w", playerID, s, err)
	}
	if epoch == 0 {
		return Alloc{}, ErrSeqRowCorrupt
	}

	// 只数**本纪元**的未决行:旧纪元的未决行不该挡住新操作,它们会各自拿到真实旧结局
	// 或 UNKNOWN,走人工处置(规格 §4.29)。多取一行是为了区分「刚好到上限」和「超了」。
	pendingQuery := fmt.Sprintf(
		"SELECT seq FROM %s WHERE player_id = ? AND stream = ? AND stream_epoch = ? AND status = ? ORDER BY seq LIMIT ? FOR UPDATE",
		t.OpTable)
	rows, err := tx.QueryContext(ctx, pendingQuery, playerID, uint32(s), epoch, t.PendingStatus, int(lim.MaxPending)+1)
	if err != nil {
		return Alloc{}, fmt.Errorf("assetop: 读未决行失败(player=%d stream=%d): %w", playerID, s, err)
	}
	pending := make([]uint64, 0, int(lim.MaxPending)+1)
	for rows.Next() {
		var seq uint64
		if err := rows.Scan(&seq); err != nil {
			rows.Close()
			return Alloc{}, fmt.Errorf("assetop: 解未决行失败(player=%d stream=%d): %w", playerID, s, err)
		}
		pending = append(pending, seq)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Alloc{}, fmt.Errorf("assetop: 遍历未决行失败(player=%d stream=%d): %w", playerID, s, err)
	}
	if err := rows.Close(); err != nil {
		return Alloc{}, fmt.Errorf("assetop: 关闭未决行游标失败(player=%d stream=%d): %w", playerID, s, err)
	}

	if uint32(len(pending)) >= lim.MaxPending {
		return Alloc{}, fmt.Errorf("%w (player=%d stream=%d pending=%d)", ErrTooManyPending, playerID, s, len(pending))
	}
	if len(pending) > 0 && nextSeq-pending[0] >= lim.MaxSpan {
		return Alloc{}, fmt.Errorf("%w (player=%d stream=%d span=%d)", ErrTooManyPending, playerID, s, nextSeq-pending[0])
	}

	bumpQuery := fmt.Sprintf("UPDATE %s SET next_seq = next_seq + 1, updated_ms = ? WHERE player_id = ? AND stream = ?", t.SeqTable)
	res, err := tx.ExecContext(ctx, bumpQuery, nowMs, playerID, uint32(s))
	if err != nil {
		return Alloc{}, fmt.Errorf("assetop: 推进 next_seq 失败(player=%d stream=%d): %w", playerID, s, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return Alloc{}, fmt.Errorf("assetop: 读 next_seq 更新行数失败(player=%d stream=%d): %w", playerID, s, err)
	}
	// 行刚刚被 FOR UPDATE 锁住,更新不到只可能是有人在事务外删了它。
	// 这时继续下去会把同一个 seq 发第二次,必须失败(fail-closed)。
	if affected != 1 {
		return Alloc{}, fmt.Errorf("assetop: 推进 next_seq 影响 %d 行(player=%d stream=%d)", affected, playerID, s)
	}

	return Alloc{Epoch: epoch, Seq: nextSeq}, nil
}

// ── 事务基座:隔离级 + 重试 + 退避 ─────────────────────────────

// 事务重试的默认次数与退避区间。
//
// 次数 3 取自 90-consistency part2 §2 第 6 条(后台写 `WithTxRetry(ctx, db, 3, …)`);
// 聚宝斋的同步业务写用 2,由调用方自己覆盖。
//
// 退避为什么是毫秒级、而不是沿用 reconcile 的秒级 BaseBackoff:这段等待花的是**调用方
// 事务子预算**里的时间(同条第 8 条:帮会写事务 1500ms、Finalize 2000ms),秒级退避会让
// 第一次重试就把子预算睡穿,把「本可成功的重试」变成必然超时。取值与帮会同步路径的
// 「10–50ms 随机退避」(同条第 3 条)同一量级:attempts=3 时两次退避合计约 24–36ms。
// 封顶 200ms 只是防调用方把 attempts 调得很大;真正兜底的是 ctx —— 退避是可取消的等待。
const (
	defaultTxAttempts    = 3
	defaultTxBaseBackoff = 10 * time.Millisecond

	// minTxBaseBackoff 是 BaseBackoff 的下限。取 2ms 而不是 1ms 的理由见 validate():
	// ±20% 抖动的下界要在毫秒取整后仍然 >= 1ms,否则"退避"对一半的重试是空话。
	minTxBaseBackoff = 2 * time.Millisecond
	defaultTxMaxBackoff  = 200 * time.Millisecond
)

// TxRetryConfig 是一次「带重试的业务事务」的全部参数。零值不可用,请从 DefaultTxRetryConfig 改。
type TxRetryConfig struct {
	// Attempts 是总尝试次数(不是重试次数),必须 >= 1。
	Attempts int
	// IsRetryable 判定「事务已整体回滚,重跑是安全的」。为 nil 时一次都不重试。
	IsRetryable func(error) bool
	// Isolation 是事务隔离级。零值 sql.LevelDefault = 跟随连接默认(通常 RR),
	// **不是**本包的推荐值;推荐值见 DefaultTxRetryConfig 的注释。
	Isolation sql.IsolationLevel
	// BaseBackoff / MaxBackoff 是退避区间,形状与 NextAttemptMs 一致(指数 + ±20% 抖动)。
	// BaseBackoff 不得小于 minTxBaseBackoff(2ms):退避按毫秒取整,再小的话抖动下界会被抹成 0。
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	// Rand 是抖动随机源,为 nil 时取中值(不抖动)。生产不要留 nil,见 DefaultTxRetryConfig。
	Rand func() float64

	// sleep 是退避等待的注入点,只给本包单测用:让「退避了几次、每次多久」可断言,
	// 而不必让测试真的睡过去(AGENTS §11.4:单测不依赖真实墙钟)。
	sleep func(context.Context, time.Duration) error
}

// DefaultTxRetryConfig 是本包推荐的事务参数。
//
// 隔离级默认 READ COMMITTED,而不是「跟随连接默认」,理由四条:
//  1. 90-consistency part2 §2 规则 2/6(裁决 D7)要求帮会全部写事务、含经本函数的后台写
//     统一 RC;而规则 6 给出的调用形式里没有隔离级参数 —— 默认值不是 RC,这条规则在代码里
//     就没有落脚点。
//  2. RR 的间隙锁正是本包在躲的死锁源:reconcile 的领取之所以拆成「非锁定读 + 主键 CAS」,
//     就是因为范围 UPDATE 的 next-key 锁会与业务事务里 AllocateSeq 的 FOR UPDATE + 插新行
//     互相等待,而被选为牺牲者的往往是玩家请求(见 reconcile.go 的 Store 注释)。
//  3. 目标库是 TiDB,那里没有间隙锁。本地 MySQL 跑 RR、线上 TiDB 跑另一套语义,等于本地
//     永远测不出线上行为;钉死 RC 让两边一致。
//  4. 「跟随连接默认」等于让同一份资产代码的语义由 DSN / 运维配置决定 —— 会丢钱的路径不接受
//     这种不确定性(AGENTS §11.3 fail-closed)。
//
// 代价:RC 在 MySQL 上要求 `binlog_format=ROW`(STATEMENT 下写入报 1665;MySQL 8 默认 ROW,
// 本仓 deploy/k8s/manifests/infra/mysql.yaml 已显式写了)。确需跟随连接默认的调用方,
// 把 Isolation 显式写成 sql.LevelDefault。
//
// 换 RC 不会削弱本包的守卫:同一 (player, stream) 的分配都先在 seq 行上拿 FOR UPDATE
// (全局锁序第一步)而彼此串行,未决行数 / 跨度的判定因此不依赖间隙锁;重复 seq 由唯一键
// (player_id, stream, stream_epoch, seq) 兜住;Finalize / Reschedule 是主键 CAS,与隔离级无关。
//
// **审稿异议与结论(2026-09-19,记下来免得被反复提起)**:有意见认为 D7 的作用域只有帮会,
// 而 go/guild 目前一行 WithTxRetry 都没有,真正受这个默认值影响的只有 go/trade —— 一条
// 没被规格要求 RC、也没人单独验证过的路径,所以默认值应退回 sql.LevelDefault、把 RC 留到
// 帮会落码时显式传。
// **不采纳**,因为上面四条里只有第 1 条是帮会专属:第 3 条(线上是 TiDB,本来就没有间隙锁)
// 与第 4 条(会丢钱的路径不接受"语义由 DSN 决定")对 trade 同样成立,而退回 LevelDefault
// 恰恰是把语义交还给运维配置 —— 那正是第 4 条要消灭的东西。
// 要留意的是代价那一条:RC 对 MySQL 的 binlog_format=ROW 有要求,新接入方若跑在别的
// MySQL 配置上,失败是启动即报 1665(响亮),不是静默走错语义。
func DefaultTxRetryConfig() TxRetryConfig {
	return TxRetryConfig{
		Attempts:    defaultTxAttempts,
		Isolation:   sql.LevelReadCommitted,
		BaseBackoff: defaultTxBaseBackoff,
		MaxBackoff:  defaultTxMaxBackoff,
		Rand:        mathrand.Float64,
	}
}

func (c TxRetryConfig) validate() error {
	if c.Attempts < 1 {
		return fmt.Errorf("assetop: WithTxRetry 的 attempts 必须 >= 1(当前 %d)", c.Attempts)
	}
	// 阈值是 2ms 不是 1ms:抖动是 ±20%,base=1ms 时下界 0.8ms 按毫秒取整就是 0,
	// 约一半的重试实际等 0 —— 那道"必须 >= 1ms"的守卫兑现不了它自己承诺的东西。
	// 2ms 时下界 1.6ms 仍取整到 1ms,每一次重试都真的退避。
	if c.BaseBackoff < minTxBaseBackoff {
		return fmt.Errorf("assetop: BaseBackoff 必须 >= %v(当前 %v):退避按毫秒取整,"+
			"再小的话 ±20%% 抖动的下界会被抹成 0,等于部分重试根本不退避", minTxBaseBackoff, c.BaseBackoff)
	}
	if c.MaxBackoff < c.BaseBackoff {
		return fmt.Errorf("assetop: 退避区间非法(base=%v max=%v)", c.BaseBackoff, c.MaxBackoff)
	}
	return nil
}

// WithTxRetry 在死锁 / 锁等待超时 / TiDB 写冲突时把**整个业务事务**重跑一遍,
// 参数取 DefaultTxRetryConfig(隔离级 READ COMMITTED、10ms 起退避 + ±20% 抖动)。
//
// 保留这个形状是因为 90-consistency part2 §2 第 6 条规定的后台写调用形式就是它
// (`WithTxRetry(ctx, db, 3, 分类函数, fn)`),而同条第 2 条又要求这些事务是 RC ——
// 两条规则只有在默认值自带 RC 时才同时成立。要改隔离级或退避区间走 WithTxRetryConfig。
//
// shared 不引 MySQL 驱动(否则所有依赖 shared 的服务都被迫拖上它),所以错误分类由调用方注入:
// 帮会(B5)与聚宝斋的实现都是 errors.As 到 *mysql.MySQLError 且 Number ∈ {1213, 1205, 9007}
// —— 9007(TiDB 写冲突)必须在内:迁库之后它会取代死锁成为最常见的一类,漏判就等于不重试。
// isRetryable 为 nil 时一次都不重试 —— 宁可让调用方看见错误,也不要在这里猜。
//
// fn 必须是**可重跑**的:它拿到的是一个全新事务,上一轮的任何内存副作用都要自己复位。
func WithTxRetry(ctx context.Context, db *sql.DB, attempts int, isRetryable func(error) bool, fn func(*sql.Tx) error) error {
	cfg := DefaultTxRetryConfig()
	cfg.Attempts = attempts
	cfg.IsRetryable = isRetryable
	return WithTxRetryConfig(ctx, db, cfg, fn)
}

// WithTxRetryConfig 是带重试的业务事务的完整入口:隔离级、次数、退避区间都由调用方定。
//
// 两次尝试之间**必须**退避:死锁里被回滚的那一方如果立刻按同样的锁序重跑,对手多半还握着
// 同一批行,于是第二次、第三次撞在同一个地方,attempts 被空耗掉,调用方看到的是「重试用尽」
// 而不是「稍后重试本来能成」。抖动是为了不让同一批被同一行挡住的事务在同一毫秒一起回来。
//
// 退避是**可取消**的等待:ctx(通常是事务子预算)到期就立刻返回,不会先睡满再去开一个注定
// 超时的事务;返回值把最后一次业务错误与 ctx 错误 Join 在一起,两边现场都不丢。
// 注意退避花的是调用方 ctx 的预算,调大 Attempts 时要连带复核子预算够不够。
func WithTxRetryConfig(ctx context.Context, db *sql.DB, cfg TxRetryConfig, fn func(*sql.Tx) error) error {
	if db == nil || fn == nil {
		return errors.New("assetop: WithTxRetry 缺少 db 或 fn")
	}
	if err := cfg.validate(); err != nil {
		return err
	}
	opts := &sql.TxOptions{Isolation: cfg.Isolation}
	sleep := cfg.sleep
	if sleep == nil {
		sleep = sleepCtx
	}

	var lastErr error
	for i := 0; i < cfg.Attempts; i++ {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return errors.Join(lastErr, err)
			}
			return err
		}
		err := runTx(ctx, db, opts, fn)
		if err == nil {
			return nil
		}
		lastErr = err
		if cfg.IsRetryable == nil || !cfg.IsRetryable(err) {
			return err
		}
		// 最后一次尝试失败后不必再睡:那只会把同一个结论晚几十毫秒告诉调用方。
		if i == cfg.Attempts-1 {
			break
		}
		if waitErr := sleep(ctx, txBackoff(uint32(i), cfg.BaseBackoff, cfg.MaxBackoff, cfg.Rand)); waitErr != nil {
			return errors.Join(lastErr, waitErr)
		}
	}
	return fmt.Errorf("assetop: 事务重试 %d 次仍失败: %w", cfg.Attempts, lastErr)
}

// txBackoff 复用 NextAttemptMs 的退避形状(指数 + ±20% 抖动),不另起一套参数:
// 同一个包里两份退避语义,迟早会在其中一份上漏掉抖动或漏掉封顶。
// 传 nowMs=0,返回的就是这一次该等多久。它按毫秒取整,所以 BaseBackoff 不得小于 1ms(validate 已拦)。
func txBackoff(attempt uint32, base, max time.Duration, rnd func() float64) time.Duration {
	return time.Duration(NextAttemptMs(0, attempt, base, max, rnd)) * time.Millisecond
}

// 可取消的等待用同包 caller.go 的 sleepCtx,不在这里再写一份:
// 两份逐字符相同的实现正是 txBackoff 自己反对的「同一个包里两份同样的语义」(DRY)。
// 语义见 sleepCtx 的注释:不用 time.Sleep,在一个已经没有预算的 ctx 上睡满,
// 等于把「早点告诉调用方稍后重试」拖成「超时失败」(AGENTS §11.3:重试必须支持取消)。

// runTx 跑一次事务,保证无论哪条路径都不会漏掉 Rollback。
// 回滚失败不吞:它和业务错误一起返回,免得连接被留在事务里还没人知道。
func runTx(ctx context.Context, db *sql.DB, opts *sql.TxOptions, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("assetop: 开启事务失败: %w", err)
	}
	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return errors.Join(err, fmt.Errorf("assetop: 回滚失败: %w", rbErr))
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("assetop: 提交失败: %w", err)
	}
	return nil
}
