package data

// 帮会管理与审批的全部写事务(设计 docs/design/guild-phase2/02-management.md §6–§8)。
//
// 本文件与 guild_repo.go 的分工:guild_repo.go 管"缓存 + 读路径 + 建帮 + 排行",
// 这里管"需要多行锁、需要权威 role 复核的写"。两者共用同一个 *GuildRepo 接收者,
// 因此哨兵错误、缓存失效原语、loadGuild 都直接复用那边的定义,不另起一套。
//
// 三条贯穿全文件的纪律:
//
//  1. **授权只看 MySQL**。Redis 里的 role / leader_id 只是读加速,陈旧一次就等于越权一次;
//     所有权限判定都在事务里对 FOR UPDATE 锁住的行做(§2.2 的纯函数在锁内调用)。
//  2. **锁序固定**(§6.1 + 01-storage.md §2.1 规则 1):
//     guild(1) → guild_player_state(2) → guild_member(3) → guild_application(4)。
//     任何事务都不得在持有靠后表的行锁之后回头去锁靠前的表 —— 那是唯一会造成
//     "两个正常玩法互相等死"的写法。
//  3. **提交之后才失效缓存**(§6.4)。MySQL 是真相,失效失败不把已提交的写报成失败,
//     否则客户端会因为 Redis 抖动进入 RequiresReconnect 隔离,而数据其实已经写成功了。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/metric"

	"shared/safego"

	"guild/internal/constants"
)

// ── 事务基座(§6.2) ────────────────────────────────────────────

// ErrWriteConflict:死锁重试耗尽,或锁等待超时(1205,已被 DSN 的 innodb_lock_wait_timeout=1 封顶)。
//
// logic 层把它映射成业务 tip kGuildBusyRetry,**不是** gRPC 错误:两个帮会同时审批同一个申请人
// 是正常玩法,回 gRPC Aborted 会让客户端整段隔离到重登,代价远大于"请稍后重试"。
var ErrWriteConflict = errors.New("guild write conflict (deadlock retries exhausted or lock wait timeout)")

// errCommitThen:事务函数要求"先提交已经做完的写,再把 inner 当业务结果返回"。
// 唯一用途是惰性清理:发现申请已过期时,既要把那一行真的删掉(提交),又要回 NotFound。
// 直接返回错误会让 defer Rollback 把删除一起撤销,过期行就永远留在表里。
type errCommitThen struct{ inner error }

func (e errCommitThen) Error() string { return e.inner.Error() }

func (e errCommitThen) Unwrap() error { return e.inner }

// maxTxAttempts:InnoDB 判定死锁(1213)或 TiDB 判定写冲突(9007)时该事务已被整体回滚,
// 重跑是安全的 —— 前提是事务函数内没有非数据库副作用(本文件所有 fn 都满足:
// 推送与缓存失效都在提交之后)。
//
// 锁等待超时(1205)刻意不重试:锁等待本身已被 innodb_lock_wait_timeout=1 封顶,
// 再等一轮只会白白吃掉同步预算,最后仍然是同一个结论。
const maxTxAttempts = 3

// txBudget:单次事务尝试的子预算(90-consistency part2 §2 第 8 条)。
//
// 为什么要比请求预算再短一截:请求预算(4000 − 500 = 3500ms)是"整个 RPC 最多跑多久",
// 而事务持有的是**帮会行锁**——同一个帮会的所有写都排在它后面。一次帮会写事务只有几条
// 主键 / 索引语句,正常在个位数毫秒内完成;跑满 1.5s 说明库出了状况,这时候尽早放锁
// 让后面的人得到"稍后重试",比让整个帮会卡满 3.5s 要好。
const txBudget = 1500 * time.Millisecond

// inTx:帮会写事务的**唯一**入口(90-consistency part2 §2 第 1 条)。
// 经济(B5)与活动(B6)的 repo 持有 *GuildRepo 并复用它,不各写一份重试助手。
//
// 隔离级别固定 READ COMMITTED(D7):RR 的间隙锁在 TiDB 上不存在,靠它串行化申请上限
// 会在迁库后静默失效;上限改由 guild_player_state 的行锁守(X-10)。
// fn 的第一个参数是**事务子预算的 ctx**,不是请求 ctx。调用点一律把形参也命名为 ctx
// (刻意遮蔽外层同名变量),这样闭包里每一条语句都自动挂在子预算上 —— 漏挂一条,
// 那条语句就会一直等到请求预算耗尽,子预算等于白设。
// 提交之后的副作用(推送、缓存失效)都在闭包**之外**,用的仍是请求 ctx,不受遮蔽影响。
func (r *GuildRepo) inTx(ctx context.Context, op string, fn func(ctx context.Context, tx *sql.Tx) error) error {
	return retryOnDeadlock(ctx, op, func() error { return r.runTxOnce(ctx, op, fn) })
}

func (r *GuildRepo) runTxOnce(ctx context.Context, op string, fn func(ctx context.Context, tx *sql.Tx) error) error {
	// 每次尝试各给一份完整子预算:重试是新的一轮加锁,沿用上一轮剩下的时间会让
	// 第 3 次尝试几乎必然超时,把"本可成功的重试"变成失败。总时长仍由请求 ctx 封顶。
	txCtx, cancel := context.WithTimeout(ctx, txBudget)
	defer cancel()

	err := r.runTxBody(txCtx, fn)
	if err == nil {
		return nil
	}
	// 子预算到期而请求还活着 = 库慢或锁排队,是"稍后重试"而不是"失败";
	// 请求本身被取消(客户端断开 / 上游超时)则原样返回,不许伪装成写冲突。
	if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		guildTxBudgetExceededTotal.Inc(op)
		logx.Errorf("[guild] %s: transaction exceeded its %v budget: %v", op, txBudget, err)
		return ErrWriteConflict
	}
	return err
}

func (r *GuildRepo) runTxBody(ctx context.Context, fn func(ctx context.Context, tx *sql.Tx) error) error {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := fn(ctx, tx); err != nil {
		var commitThen errCommitThen
		if errors.As(err, &commitThen) {
			if commitErr := commitTx(tx); commitErr != nil {
				return commitErr
			}
			return commitThen.inner
		}
		return err
	}
	return commitTx(tx)
}

// commitTx:COMMIT 失败时**结果不明**——网络在发出 COMMIT 之后断开,事务可能已经在库里生效。
// 这种情况不能当作"失败"原样抛给客户端(客户端会以为没生效而重做,可能重复发生效果),
// 也不能当作成功。按 90-consistency part2 §2 第 4 条:打 ERROR 让人看见,对外归一到
// ErrWriteConflict —— 客户端收到的是"稍后重试",重试时会先读当前状态,幂等分支会兜住已生效的那一半。
func commitTx(tx *sql.Tx) error { return classifyCommitErr(tx.Commit()) }

// classifyCommitErr 单独拆出来是为了能不连库直接测:提交阶段的分类是**语义**判断,
// 不该为了验证它去起一个数据库。
//
// ErrTxDone / ctx 取消 / ctx 超时这三类的结论是**明确**的(事务没提交,或调用方自己走了),
// 原样返回;其余(连接被重置、驱动报未知错误)结果不明,归一到 ErrWriteConflict。
func classifyCommitErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrTxDone) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	logx.Errorf("[guild] commit outcome unknown: %v", err)
	return ErrWriteConflict
}

// retryOnDeadlock 与数据库解耦:run 是"跑一次事务"的闭包,单测用假错误直接驱动它。
//
// 契约:run 每次调用都必须是可重跑的(见 maxTxAttempts);调用方在 run 里累积的结果
// 一律在闭包内部声明、成功返回前才赋给外层变量,否则重跑会把上一轮的残留一起带出去(§6.2)。
func retryOnDeadlock(ctx context.Context, op string, run func() error) error {
	for attempt := 1; ; attempt++ {
		err := run()
		switch {
		case isLockWaitTimeout(err):
			guildTxLockWaitTimeoutTotal.Inc(op)
			return ErrWriteConflict
		case !isRetryableTxError(err):
			return err
		}
		guildTxDeadlockTotal.Inc(op)
		if attempt >= maxTxAttempts {
			logx.Errorf("[guild] %s: deadlock persisted after %d attempts: %v", op, attempt, err)
			return ErrWriteConflict
		}
		// 10–50ms 随机退避:两个互相回滚的事务若同时重来,大概率再撞一次;
		// 抖动的作用是把它们错开,而不是"多等一会儿"。
		backoff := 10*time.Millisecond + time.Duration(rand.Int64N(int64(40*time.Millisecond)))
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// errRetryTx:调用方主动要求重跑本次事务的哨兵(90-consistency part2 §2 第 3 条,B6 结算用)。
// 与 1213/9007 同等对待:整体回滚后重来一次。business 分支只在"重跑一定能拿到新状态"时用它。
var errRetryTx = errors.New("guild: retry transaction")

// isRetryableTxError:哪些错误意味着"事务已整体回滚,重跑是安全的"。
//
//   - 1213 = InnoDB 死锁(本地 MySQL)
//   - 9007 = TiDB 写冲突(乐观事务提交期检测到冲突,语义与死锁等价)。迁 TiDB 后
//     这是最常见的一类,漏了它写冲突就会以内部错误抛给客户端(违反 D6:统一回 GuildBusyRetry)。
func isRetryableTxError(err error) bool {
	if errors.Is(err, errRetryTx) {
		return true
	}
	switch mysqlErrNumber(err) {
	case 1213, 9007:
		return true
	default:
		return false
	}
}

func isLockWaitTimeout(err error) bool { return mysqlErrNumber(err) == 1205 }

// mysqlErrNumber 走 errors.As:本文件所有 SQL 错误都用 %w 包过一层带上下文,
// 直接类型断言会漏判,死锁就会被当成内部错误抛给玩家。
func mysqlErrNumber(err error) uint16 {
	var mysqlErr *mysqlDriver.MySQLError
	if errors.As(err, &mysqlErr) {
		return mysqlErr.Number
	}
	return 0
}

// ── 锁等待封顶(§6.2a) ────────────────────────────────────────

// LockWaitTimeoutSeconds:帮会写事务都是几条主键 / 索引语句(毫秒级),等锁超过 1s 即视为异常长事务。
//
// InnoDB 默认 50s。RPC 的 ctx 到期后 go-sql-driver 只是关掉连接,服务端等锁的线程在锁等待期间
// 一般不检测对端断连,会继续占着本事务已拿到的 guild 行锁 —— 该帮后续所有写连锁超时最长 50s。
const LockWaitTimeoutSeconds = 1

// WithLockWaitTimeout 给 DSN 附加会话变量 innodb_lock_wait_timeout(go-sql-driver 把未知参数
// 当作连接建立后 SET 的系统变量;该变量是会话级可写,appuser 有权设置),并顺手清掉
// clientFoundRows(90-consistency Y-20)。
//
// 为什么在代码里改而不改 yaml:这是正确性约束不是部署偏好,K8s ConfigMap 与本地 yaml 各改一遍
// 就等于给"某个环境忘了改"留了后门。-migrate 模式**不**经过它:DDL 等的是元数据锁,另有语义。
//
// clientFoundRows=true 会让 UPDATE 的 RowsAffected 变成"匹配行数"而不是"实际改动行数",
// 本文件多处用 RowsAffected==1 做写入自检,那个语义翻转会让自检永远通过、写丢了也看不见。
//
// 解析失败返回**不含原文**的错误:DSN 里有口令,不能进日志。
func WithLockWaitTimeout(dsn string) (string, error) {
	cfg, err := mysqlDriver.ParseDSN(dsn)
	if err != nil {
		return "", errors.New("MySQL.DataSource 无法解析(原文含口令,不打印)")
	}
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	cfg.Params["innodb_lock_wait_timeout"] = strconv.Itoa(LockWaitTimeoutSeconds)
	cfg.ClientFoundRows = false
	return cfg.FormatDSN(), nil
}

// ── 指标(§6.2) ──────────────────────────────────────────────

// label 只有 op,取值是下面那组固定常量 —— 绝不放 guild_id / player_id,
// 那是无上界的高基数维度,会把 Prometheus 的时间序列打爆(AGENTS.md §9)。
const guildMetricNamespace = "guild"

var (
	guildTxDeadlockTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: guildMetricNamespace,
		Subsystem: "tx",
		Name:      "deadlock_total",
		Help:      "帮会写事务遇到死锁(MySQL 1213)或写冲突(TiDB 9007)并整体重跑的次数。稳态应接近 0;持续上升说明有事务违反了锁序。",
		Labels:    []string{"op"},
	})

	guildTxBudgetExceededTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: guildMetricNamespace,
		Subsystem: "tx",
		Name:      "budget_exceeded_total",
		Help:      "帮会写事务跑满子预算(1500ms)被中止、对外回 GuildBusyRetry 的次数。稳态应为 0;非 0 说明库慢或帮会行锁排队。",
		Labels:    []string{"op"},
	})

	guildTxLockWaitTimeoutTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: guildMetricNamespace,
		Subsystem: "tx",
		Name:      "lock_wait_timeout_total",
		Help:      "帮会写事务锁等待超过 innodb_lock_wait_timeout 的次数。>0 说明有长事务占着同一帮的行锁。",
		Labels:    []string{"op"},
	})

	guildCacheInvalidateFailedTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: guildMetricNamespace,
		Subsystem: "cache",
		Name:      "invalidate_failed_total",
		Help:      "提交后缓存失效在同步尝试与后台有界重试之后仍然失败的次数。>0 意味着有玩家会读到最长 Cache.DefaultTTL 的陈旧快照。",
		Labels:    []string{"op"},
	})
)

// 写事务的 op 取值集合(指标 label + 失效日志)。**固定集合**,新增批次只能往这里加,
// 不许在调用点写字面量 —— 否则 label 基数就不再可证。
const (
	opCreate        = "create"
	opSetRole       = "set_role"
	opKick          = "kick"
	opTransfer      = "transfer"
	opLeave         = "leave"
	opApply         = "apply"
	opCancel        = "cancel"
	opReview        = "review"
	opDisband       = "disband"
	opAnnouncement  = "announcement"
	opVerifyMapping = "verify_mapping"
	opScore         = "score"

	// 下面四个由 B5 / B6 使用(90-consistency Y-04 要求 B2s 一次把集合定全,
	// 后续批次只调用、不再改本文件)。
	opUpgrade       = "upgrade"
	opAssetFinalize = "asset_finalize"
	opActivity      = "activity"
	opTrialSettle   = "trial_settle"
)

// ── 权限判定(§2.2,纯函数,事务内调用) ──────────────────────

// 一律比 constants.Rank 不比 role 原值:role 编码不连续(2 是空号),
// `role >= RoleOfficer` 这种写法会把未知编码一起放进来,等于凭空发权限。

func canAssignRole(actorRole uint32) bool { return constants.Rank(actorRole) == constants.RankLeader }

func canTransferLeader(actorRole uint32) bool {
	return constants.Rank(actorRole) == constants.RankLeader
}

func canReviewApplications(actorRole uint32) bool {
	return constants.Rank(actorRole) >= constants.RankOfficer
}

// canKick:职位必须严格高于对方,且自己至少是长老。
// 帮主可踢长老与成员;长老只能踢成员;没有人能踢帮主;未知编码(RankNone)既不能踢人也不能被踢。
func canKick(actorRole, targetRole uint32) bool {
	a, t := constants.Rank(actorRole), constants.Rank(targetRole)
	return a >= constants.RankOfficer && t != constants.RankNone && a > t
}

// demotedLeaderRole:转让之后原帮主的新角色。长老位有空 → 长老;否则降为成员。
// 契约刻意选择"宁可降成成员也不超编":超编会让后续任免逻辑里的上限判定永久失真。
func demotedLeaderRole(officersAfterTarget, maxOfficers uint32) uint32 {
	if officersAfterTarget < maxOfficers {
		return constants.RoleOfficer
	}
	return constants.RoleMember
}

// ── 结果类型(§6.6、§8) ─────────────────────────────────────

// MemberWriteResult:成员管理写的结果。
// Guild 是**提交前在同一事务内**读到的权威快照(已含本次写),既作响应体也作推送收件人来源;
// 不用提交后的 GetGuild —— 那一步若 Redis 失败,已提交的写会被报成失败。
type MemberWriteResult struct {
	Guild   *GuildData
	Changed bool // false = 幂等无操作(如任免为同一角色),logic 不推送
}

// OfficerCapFunc 由 logic 注入:按帮会等级查长老上限。ok=false 表示 GuildLevel 配表缺该等级行,
// 调用方必须 fail-closed(返回 ErrGuildLevelConfigMissing),不得默认放行。
type OfficerCapFunc func(level uint32) (maxOfficers uint32, ok bool)

// ApplicationRules:入帮申请的配表规则,由 logic 从 GuildRule 表现算后传入。
// 三个字段都必须 > 0:0 代表配表没读到,repo 一律拒绝而不是当成"无限制"。
type ApplicationRules struct {
	TTLMs        uint64 // GuildRule.application_expire_hours × 3_600_000
	MaxPerPlayer uint32 // GuildRule.max_pending_applications_per_player
	MaxPerGuild  uint32 // GuildRule.max_pending_applications_per_guild
}

// ApplyResult:ApplyToGuild 的结果。
type ApplyResult struct {
	Inserted    bool     // false = 同帮重复申请,只刷新了有效期(契约:刷新即成功)
	ReviewerIDs []uint64 // 仅 Inserted 时有值:该帮长老与帮主,player_id 升序(推送收件人)
}

// ReviewResult:ReviewApplication 的结果。Guild 同样是事务内权威快照。
type ReviewResult struct {
	Guild    *GuildData
	Approved bool
}

// DisbandResult:解散的结果。
type DisbandResult struct {
	// ZoneID 是**删除事务内 FOR UPDATE 读到的**归属区,是清榜唯一可信的输入:
	// 行删掉之后再也读不到权威 zone,而缓存里的 ZoneID 在合服之后会整整一个 TTL 指向源区,
	// 按它 ZREM 会把条目永远留在目标区榜上(榜首一个查不到名字的幽灵帮会)。
	ZoneID uint32
	// MemberIDs 是解散前的全部成员,player_id 升序;既是推送收件人,也是要失效的映射键。
	MemberIDs []uint64
}

// ApplicationRow:本人视角的一条申请(ListMyApplications)。
type ApplicationRow struct{ GuildID, ApplyMs, ExpireMs uint64 }

// ApplicantRow:帮会视角的一条待审申请(ListApplicants)。
type ApplicantRow struct{ PlayerID, ApplyMs, ExpireMs uint64 }

// ── 提交后的缓存失效与陈旧映射自愈(§6.4) ────────────────────

// invalidateRetryDelays:同步失效失败后,后台按此间隔重试(总计约 2.1s,落在 3s 预算内)。
// 单测把它临时改成 1ms 级别。
var invalidateRetryDelays = []time.Duration{100 * time.Millisecond, 400 * time.Millisecond, 1600 * time.Millisecond}

// invalidateGaveUp:重试全部耗尽时调用。生产实现计指标,单测替换为记录器。
var invalidateGaveUp = func(op string) { guildCacheInvalidateFailedTotal.Inc(op) }

// cacheTarget:一次失效动作的目标。guildID != 0 → 帮会快照键;否则 playerID → 玩家映射键。
type cacheTarget struct {
	guildID  uint64
	playerID uint64
}

// invalidateAfterCommit:**所有**提交后失效的唯一入口(本文件的新事务,以及 guild_repo.go 里
// 改造后的 CreateGuild 与公告写)。
//
// 输入约束:op 取本文件固定集合;guildID == 0 表示只失效玩家映射;players 里的 0 被忽略。
// 语义:先用请求 ctx 同步失效一次,失败的键交给后台协程(context.Background + 3s 预算)
// 按 invalidateRetryDelays 有界重试;全部耗尽才计 guild_cache_invalidate_failed_total{op} 并记 ERROR。
// **永不返回错误**:MySQL 已经是真相,把缓存失效的失败报成写失败只会让客户端白白进入隔离。
// 幂等:失效 Lua 本身是 INCR generation + DEL,重复执行无害。
func (r *GuildRepo) invalidateAfterCommit(ctx context.Context, op string, guildID uint64, players ...uint64) {
	targets := make([]cacheTarget, 0, 1+len(players))
	if guildID != 0 {
		targets = append(targets, cacheTarget{guildID: guildID})
	}
	for _, playerID := range players {
		if playerID != 0 {
			targets = append(targets, cacheTarget{playerID: playerID})
		}
	}
	if len(targets) == 0 {
		return
	}

	remaining, lastErr := r.invalidateTargets(ctx, targets)
	if len(remaining) == 0 {
		return
	}
	logx.Infof("[guild] %s: cache invalidation deferred for %d keys: %v", op, len(remaining), lastErr)

	safego.Go("guild.cache_invalidate", func() {
		bg, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		pending, err := remaining, lastErr
	retry:
		for _, delay := range invalidateRetryDelays {
			timer := time.NewTimer(delay)
			select {
			case <-bg.Done():
				timer.Stop()
				break retry
			case <-timer.C:
			}
			// 每轮只重试上一轮仍然失败的键,避免把已经成功的 generation 又 INCR 一遍。
			if pending, err = r.invalidateTargets(bg, pending); len(pending) == 0 {
				return
			}
		}
		logx.Errorf("[guild] %s: cache invalidation gave up on %d keys: %v", op, len(pending), err)
		invalidateGaveUp(op)
	})
}

// invalidateTargets 逐键失效,返回仍然失败的键与最后一个错误(供日志定位)。
func (r *GuildRepo) invalidateTargets(ctx context.Context, targets []cacheTarget) ([]cacheTarget, error) {
	var (
		failed  []cacheTarget
		lastErr error
	)
	for _, target := range targets {
		var err error
		if target.guildID != 0 {
			err = r.invalidateGuildCache(ctx, target.guildID)
		} else {
			err = r.invalidatePlayerGuildCache(ctx, target.playerID)
		}
		if err != nil {
			failed = append(failed, target)
			lastErr = err
		}
	}
	return failed, lastErr
}

// VerifyPlayerGuildID 以 MySQL(uk_guild_member 唯一索引点查)复核缓存映射 cached。
//
// 为什么需要它:GetPlayerGuildID 连 0 也缓存 Cache.DefaultTTL(30 分钟)。
// 若"审批通过"之后缓存失效连后台重试都失败(Redis 长时间故障),刚入帮的玩家会在这半小时里
// 读到 0,什么帮会操作都做不了;被踢的人则一直读到旧帮。
//
// 语义:相同 → 原样返回且不写 Redis(不制造无谓的 generation 翻代);不同 → 失效映射键后返回 MySQL 值。
// Redis 失败不影响返回值 —— MySQL 是真相。开销:一次唯一索引点查,只在"未入帮"或"快照不含本人"时发生。
func (r *GuildRepo) VerifyPlayerGuildID(ctx context.Context, playerID, cached uint64) (uint64, error) {
	actual, err := r.loadPlayerGuildFromMySQL(ctx, playerID)
	if err != nil {
		return 0, err
	}
	if actual == cached {
		return actual, nil
	}
	r.invalidateAfterCommit(ctx, opVerifyMapping, 0, playerID)
	return actual, nil
}

// ResolvePlayerGuild:客户端 GetPlayerGuild 的权威解析,(nil, nil) 表示未入帮。
//
// 与"直接读缓存"的差别只在坏路径:缓存说 0 或者缓存快照里没有本人时,用 MySQL 复核一次,
// 而不是把陈旧结论直接回给玩家。内部调用(无会话)不走这里,免得给高频内部读加 MySQL 负担。
//
// 最后一步的 fail-closed 很关键:guild_member 说玩家在 G,而 G 的成员快照里没有他,
// 说明两份 MySQL 数据互相矛盾,这时**返回内部错误**而不是"当作未入帮" —— 后者会让玩家
// 在数据损坏的情况下去建新帮,把矛盾扩大成两个帮。
func (r *GuildRepo) ResolvePlayerGuild(ctx context.Context, playerID uint64) (*GuildData, error) {
	guildID, err := r.GetPlayerGuildID(ctx, playerID)
	if err != nil {
		return nil, err
	}
	if guildID == 0 {
		if guildID, err = r.VerifyPlayerGuildID(ctx, playerID, 0); err != nil {
			return nil, err
		}
		if guildID == 0 {
			return nil, nil
		}
	}

	guild, err := r.GetGuild(ctx, guildID)
	if err != nil {
		return nil, err
	}
	if guild != nil && guildHasMember(guild, playerID) {
		return guild, nil
	}

	// 帮已不存在,或快照里没有本人:两种情况都说明 guild:v2:{id} 与映射至少有一个是陈旧的。
	r.invalidateAfterCommit(ctx, opVerifyMapping, guildID)
	verified, err := r.VerifyPlayerGuildID(ctx, playerID, guildID)
	if err != nil {
		return nil, err
	}
	if verified == 0 {
		return nil, nil
	}
	// 刻意**不经缓存**:Redis 仍然坏着的时候,GetGuild 会把同一份旧快照再读回来。
	fresh, err := r.loadGuildFromMySQL(ctx, verified)
	if err != nil {
		return nil, err
	}
	if fresh == nil {
		return nil, nil
	}
	if !guildHasMember(fresh, playerID) {
		return nil, fmt.Errorf("guild %d snapshot disagrees with uk_guild_member for player %d", verified, playerID)
	}
	return fresh, nil
}

func guildHasMember(guild *GuildData, playerID uint64) bool {
	for i := range guild.Members {
		if guild.Members[i].PlayerID == playerID {
			return true
		}
	}
	return false
}

// ── 申请推送冷却(§12.2) ─────────────────────────────────────

// ApplyPushCooldown:同一 (帮会, 申请人) 在此窗口内至多推一次 APPLICATION_RECEIVED,
// 挡住"申请 → 撤回 → 申请"的刷屏。键 60s 自然过期,不需要清理。
const ApplyPushCooldown = 60 * time.Second

func applyPushKey(guildID, playerID uint64) string {
	return fmt.Sprintf("guild:apply_push:%d:%d", guildID, playerID)
}

// TryMarkApplyPush:拿到冷却键返回 true(可以推),键已存在或 Redis 出错返回 false(不推)。
//
// Redis 出错时选择**不推**而不是照推:推送本来就只承诺"至多一次",审批人打开申请页或刷新
// 立刻能看到全部待审申请,少一条提示的代价远小于 Redis 抖动期间的刷屏。
func (r *GuildRepo) TryMarkApplyPush(ctx context.Context, guildID, playerID uint64) bool {
	marked, err := r.rdb.SetNX(ctx, applyPushKey(guildID, playerID), 1, ApplyPushCooldown).Result()
	if err != nil {
		logx.Infof("[guild] apply push gate unavailable for guild %d: %v", guildID, err)
		return false
	}
	return marked
}

// ── 公共 SQL 与加锁助手(§7) ─────────────────────────────────

const (
	// sqlLockGuild:锁序位置 1。无行 → ErrGuildGone(帮会可能刚被解散)。
	sqlLockGuild = `SELECT level, leader_id, zone_id, max_members FROM guild WHERE guild_id = ? FOR UPDATE`

	// sqlEnsurePlayerState / sqlLockPlayerState:锁序位置 2(X-10)。
	// 建行必须在**事务外**自动提交:对不存在的行做加锁读再插入,在 RR 下靠间隙锁互等、
	// 在 TiDB 下根本锁不住后续插入(01-storage.md §2.1 规则 3)。
	sqlEnsurePlayerState = `INSERT IGNORE INTO guild_player_state (player_id, updated_ms) VALUES (?, ?)`
	sqlLockPlayerState   = `SELECT player_id FROM guild_player_state WHERE player_id = ? FOR UPDATE`

	// sqlLockMemberPair:锁序位置 3。用**一条**语句同时锁操作者与目标 ——
	// InnoDB 按主键 (guild_id, player_id) 升序扫描加锁,天然满足"同表多行按主键升序"。
	// 分成两条 SELECT 就要靠调用方记得排序,迟早有人写反。
	sqlLockMemberPair = `SELECT player_id, role FROM guild_member
	WHERE guild_id = ? AND player_id IN (?, ?) ORDER BY player_id FOR UPDATE`
	sqlLockMemberRole = `SELECT role FROM guild_member WHERE guild_id = ? AND player_id = ? FOR UPDATE`
	sqlLockMemberIDs  = `SELECT player_id FROM guild_member WHERE guild_id = ? ORDER BY player_id FOR UPDATE`

	sqlCountMembers       = `SELECT COUNT(*) FROM guild_member WHERE guild_id = ?`
	sqlCountMembersByRole = `SELECT COUNT(*) FROM guild_member WHERE guild_id = ? AND role = ?`
	sqlSelectMemberRole   = `SELECT role FROM guild_member WHERE guild_id = ? AND player_id = ?`
	sqlSelectMemberGuild  = `SELECT guild_id FROM guild_member WHERE player_id = ?`
	sqlSelectReviewers    = `SELECT player_id FROM guild_member WHERE guild_id = ? AND role IN (?, ?) ORDER BY player_id`

	sqlInsertMember = `INSERT INTO guild_member (guild_id, player_id, role, join_time_ms, last_active_ms, contribution_total, contribution_balance)
	VALUES (?, ?, ?, ?, ?, 0, 0)`
	sqlUpdateMemberRole   = `UPDATE guild_member SET role = ? WHERE guild_id = ? AND player_id = ?`
	sqlDeleteMember       = `DELETE FROM guild_member WHERE guild_id = ? AND player_id = ?`
	sqlDeleteGuildMembers = `DELETE FROM guild_member WHERE guild_id = ?`

	// 申请表:锁序位置 4。只存待审,通过 / 拒绝 / 撤回 / 过期 / 解散都删行。
	sqlSelectApplicationExpire = `SELECT expire_ms FROM guild_application WHERE guild_id = ? AND player_id = ? FOR UPDATE`
	sqlLockMyApplications      = `SELECT guild_id FROM guild_application WHERE player_id = ? FOR UPDATE`
	sqlInsertApplication       = `INSERT INTO guild_application (guild_id, player_id, apply_ms, expire_ms) VALUES (?, ?, ?, ?)`
	sqlRefreshApplication      = `UPDATE guild_application SET apply_ms = ?, expire_ms = ? WHERE guild_id = ? AND player_id = ?`
	sqlDeleteApplication       = `DELETE FROM guild_application WHERE guild_id = ? AND player_id = ?`
	sqlDeletePlayerApplication = `DELETE FROM guild_application WHERE player_id = ?`
	sqlDeleteGuildApplications = `DELETE FROM guild_application WHERE guild_id = ?`
	// 惰性清理:本人的过期行无上限(至多 MaxPerPlayer 条),本帮的过期行加 LIMIT 100 封顶,
	// 免得一个冷门帮攒了几千条过期申请时,某个倒霉玩家的申请事务把它们全删一遍。
	sqlDeleteExpiredOfPlayer = `DELETE FROM guild_application WHERE player_id = ? AND expire_ms <= ?`
	sqlDeleteExpiredOfGuild  = `DELETE FROM guild_application WHERE guild_id = ? AND expire_ms <= ? LIMIT 100`

	// sqlCountLiveApplicationsOfGuild:按"有效申请"判据 I1 过滤 —— expire_ms > now
	// **且**申请人没有 guild_member 行。已经入了别的帮的人留下的申请行不占本帮的待审名额。
	sqlCountLiveApplicationsOfGuild = `SELECT COUNT(*) FROM guild_application a
	LEFT JOIN guild_member m ON m.player_id = a.player_id
	WHERE a.guild_id = ? AND a.expire_ms > ? AND m.player_id IS NULL`

	sqlListMyApplications = `SELECT guild_id, apply_ms, expire_ms FROM guild_application
	WHERE player_id = ? AND expire_ms > ? ORDER BY apply_ms DESC, guild_id ASC LIMIT ?`
	sqlListApplicants = `SELECT a.player_id, a.apply_ms, a.expire_ms FROM guild_application a
	LEFT JOIN guild_member m ON m.player_id = a.player_id
	WHERE a.guild_id = ? AND a.expire_ms > ? AND m.player_id IS NULL
	ORDER BY a.apply_ms ASC, a.player_id ASC LIMIT ?`

	sqlUpdateAnnouncement = `UPDATE guild SET announcement = ? WHERE guild_id = ?`
)

// guildLockRow 是 sqlLockGuild 读到的权威帮会字段。
type guildLockRow struct {
	Level      uint32
	LeaderID   uint64
	ZoneID     uint32
	MaxMembers uint32
}

func lockGuildRow(ctx context.Context, tx *sql.Tx, guildID uint64) (guildLockRow, error) {
	var row guildLockRow
	err := tx.QueryRowContext(ctx, sqlLockGuild, guildID).
		Scan(&row.Level, &row.LeaderID, &row.ZoneID, &row.MaxMembers)
	if errors.Is(err, sql.ErrNoRows) {
		return row, ErrGuildGone
	}
	if err != nil {
		return row, fmt.Errorf("lock guild %d: %w", guildID, err)
	}
	return row, nil
}

// lockPlayerState 锁住玩家的串行化状态行(锁序位置 2)。
// 行必须已由 ensurePlayerStateRow 在事务外建好;查不到说明建行那步被跳过了,fail-closed。
func lockPlayerState(ctx context.Context, tx *sql.Tx, playerID uint64) error {
	var locked uint64
	err := tx.QueryRowContext(ctx, sqlLockPlayerState, playerID).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("guild_player_state row for player %d missing (ensurePlayerStateRow skipped)", playerID)
	}
	if err != nil {
		return fmt.Errorf("lock guild player state %d: %w", playerID, err)
	}
	return nil
}

// ensurePlayerStateRow 在事务外(自动提交)保证状态行存在。
// 幂等:INSERT IGNORE 撞主键就什么都不做。
func (r *GuildRepo) ensurePlayerStateRow(ctx context.Context, playerID, now uint64) error {
	if _, err := r.db.ExecContext(ctx, sqlEnsurePlayerState, playerID, now); err != nil {
		return fmt.Errorf("ensure guild player state row %d: %w", playerID, err)
	}
	return nil
}

// lockMemberPair 一次锁住操作者与目标的成员行,返回两者的权威 role。
// 前置:actorID != targetID(调用方已断言)。操作者不在结果里 → ErrNotGuildMember;目标不在 → ErrTargetNotMember。
func lockMemberPair(ctx context.Context, tx *sql.Tx, guildID, actorID, targetID uint64) (actorRole, targetRole uint32, err error) {
	low, high := actorID, targetID
	if low > high {
		low, high = high, low
	}
	rows, err := tx.QueryContext(ctx, sqlLockMemberPair, guildID, low, high)
	if err != nil {
		return 0, 0, fmt.Errorf("lock members of guild %d: %w", guildID, err)
	}
	defer rows.Close()

	var actorFound, targetFound bool
	for rows.Next() {
		var (
			playerID uint64
			role     uint32
		)
		if err := rows.Scan(&playerID, &role); err != nil {
			return 0, 0, fmt.Errorf("scan locked member of guild %d: %w", guildID, err)
		}
		switch playerID {
		case actorID:
			actorRole, actorFound = role, true
		case targetID:
			targetRole, targetFound = role, true
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("iterate locked members of guild %d: %w", guildID, err)
	}
	if !actorFound {
		return 0, 0, ErrNotGuildMember
	}
	if !targetFound {
		return 0, 0, ErrTargetNotMember
	}
	return actorRole, targetRole, nil
}

func countMembers(ctx context.Context, tx *sql.Tx, guildID uint64) (uint32, error) {
	var count uint32
	if err := tx.QueryRowContext(ctx, sqlCountMembers, guildID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count members of guild %d: %w", guildID, err)
	}
	return count, nil
}

// countMembersByRole 在同一把 guild 行锁下数某个职位的人数。
// 所有改 role / 增删成员的事务都先锁 guild 行,所以这个计数不会与并发写交错。
func countMembersByRole(ctx context.Context, tx *sql.Tx, guildID uint64, role uint32) (uint32, error) {
	var count uint32
	if err := tx.QueryRowContext(ctx, sqlCountMembersByRole, guildID, role).Scan(&count); err != nil {
		return 0, fmt.Errorf("count members with role %d of guild %d: %w", role, guildID, err)
	}
	return count, nil
}

// txSnapshot 在最后一次写之后、提交之前读事务内权威快照(§6.3)。
// 走 loadGuild(ctx, tx, …) 因此读得到本事务自己的写;返回 nil 说明帮会行在本事务里已经不存在了。
func txSnapshot(ctx context.Context, tx *sql.Tx, guildID uint64) (*GuildData, error) {
	guild, err := loadGuild(ctx, tx, guildID)
	if err != nil {
		return nil, err
	}
	if guild == nil {
		return nil, ErrGuildGone
	}
	return guild, nil
}

// execExactlyOneRow 把"写了几行"变成硬断言。
// 写入自检不是洁癖:RowsAffected 为 0 意味着刚刚在锁内读到的行在写的时候不见了,
// 这种情况下继续提交会产生一份"响应说成功、库里没变"的假成功。
func execExactlyOneRow(ctx context.Context, tx *sql.Tx, what, query string, args ...any) error {
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: read rows affected: %w", what, err)
	}
	if affected != 1 {
		return fmt.Errorf("%s: expected exactly 1 row affected, got %d", what, affected)
	}
	return nil
}

// placeholders 生成 n 个 "?" 的逗号列表。n <= 0 时返回空串,调用方必须自己跳过空批。
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}

// deleteApplicationsOfPlayers 按不变式 I3 删除这些玩家的全部申请,占位符每批封顶 100 个
// (与 constants.MaxGuildMembersCap 同量级;超长 IN 列表会让优化器退化成全表扫)。
func deleteApplicationsOfPlayers(ctx context.Context, tx *sql.Tx, playerIDs []uint64) error {
	const chunkSize = 100
	for start := 0; start < len(playerIDs); start += chunkSize {
		batch := playerIDs[start:min(start+chunkSize, len(playerIDs))]
		args := make([]any, len(batch))
		for i, playerID := range batch {
			args[i] = playerID
		}
		query := `DELETE FROM guild_application WHERE player_id IN (` + placeholders(len(batch)) + `)`
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("delete applications of %d removed members: %w", len(batch), err)
		}
	}
	return nil
}

// ── 成员管理事务(§7) ───────────────────────────────────────

// SetMemberRole 任免长老(只能设成员 / 长老两档)。
//
// 输入约束:actorID != targetID;constants.AssignableRole(role);officerCap != nil。
// 三者由 logic 先校验,这里再断言一次 —— repo 是可被 B5/B6 直接复用的接缝,不能依赖"上游一定校验过"。
// 不变量:role=3(帮主)只能经 TransferLeader 产生;长老数不超过 GuildLevel[level].max_officers。
// 事务边界:单事务 op=set_role,锁序 guild → guild_member(操作者与目标一条语句)。
// 错误语义:ErrGuildGone / ErrNotGuildMember / ErrTargetNotMember / ErrRankTooLow /
// ErrOfficerLimit / ErrGuildLevelConfigMissing / ErrWriteConflict;其余为内部错误。
// 幂等:目标已经是该角色 → 成功且 Changed=false,不写库、不失效缓存、不推送。
func (r *GuildRepo) SetMemberRole(ctx context.Context, guildID, actorID, targetID uint64, role uint32, officerCap OfficerCapFunc) (MemberWriteResult, error) {
	if actorID == targetID {
		return MemberWriteResult{}, fmt.Errorf("set member role in guild %d: actor and target must differ", guildID)
	}
	if !constants.AssignableRole(role) {
		return MemberWriteResult{}, fmt.Errorf("set member role in guild %d: role %d is not assignable", guildID, role)
	}
	if officerCap == nil {
		return MemberWriteResult{}, fmt.Errorf("set member role in guild %d: officer cap lookup not injected", guildID)
	}

	var out MemberWriteResult
	err := r.inTx(ctx, opSetRole, func(ctx context.Context, tx *sql.Tx) error {
		var res MemberWriteResult // 每次尝试从零开始(§6.2 结果变量约定)

		guild, err := lockGuildRow(ctx, tx, guildID)
		if err != nil {
			return err
		}
		actorRole, targetRole, err := lockMemberPair(ctx, tx, guildID, actorID, targetID)
		if err != nil {
			return err
		}
		if !canAssignRole(actorRole) {
			return ErrRankTooLow
		}
		// 防御分支:帮主的职位只能经转让变更,任免接口一律拒绝碰他。
		if constants.Rank(targetRole) == constants.RankLeader {
			return ErrRankTooLow
		}

		if targetRole == role {
			snapshot, err := txSnapshot(ctx, tx, guildID)
			if err != nil {
				return err
			}
			res.Guild, res.Changed = snapshot, false
			out = res
			return nil
		}

		if role == constants.RoleOfficer {
			maxOfficers, ok := officerCap(guild.Level)
			if !ok {
				return ErrGuildLevelConfigMissing
			}
			officers, err := countMembersByRole(ctx, tx, guildID, constants.RoleOfficer)
			if err != nil {
				return err
			}
			if officers >= maxOfficers {
				return ErrOfficerLimit
			}
		}

		if err := execExactlyOneRow(ctx, tx, fmt.Sprintf("set role of member %d in guild %d", targetID, guildID),
			sqlUpdateMemberRole, role, guildID, targetID); err != nil {
			return err
		}

		snapshot, err := txSnapshot(ctx, tx, guildID)
		if err != nil {
			return err
		}
		res.Guild, res.Changed = snapshot, true
		out = res
		return nil
	})
	if err != nil {
		return MemberWriteResult{}, err
	}
	// 幂等分支没有写,缓存里那份快照仍然是对的,不必翻代。
	if out.Changed {
		r.invalidateAfterCommit(ctx, opSetRole, guildID)
	}
	return out, nil
}

// KickMember 把目标踢出帮会。
//
// 输入约束:actorID != targetID。不变量:职位必须严格高于对方(canKick);被踢者的帮贡随成员行一起消失。
// 事务边界:单事务 op=kick,锁序 guild → guild_member → guild_application(I3)。
// 错误语义:同 SetMemberRole 的哨兵集合(不含 ErrOfficerLimit)。
// 幂等:目标已经不在帮里 → ErrTargetNotMember(客户端据此刷新列表)。
func (r *GuildRepo) KickMember(ctx context.Context, guildID, actorID, targetID uint64) (MemberWriteResult, error) {
	if actorID == targetID {
		return MemberWriteResult{}, fmt.Errorf("kick member of guild %d: actor and target must differ", guildID)
	}

	var out MemberWriteResult
	err := r.inTx(ctx, opKick, func(ctx context.Context, tx *sql.Tx) error {
		var res MemberWriteResult

		if _, err := lockGuildRow(ctx, tx, guildID); err != nil {
			return err
		}
		actorRole, targetRole, err := lockMemberPair(ctx, tx, guildID, actorID, targetID)
		if err != nil {
			return err
		}
		if !canKick(actorRole, targetRole) {
			return ErrRankTooLow
		}

		if err := execExactlyOneRow(ctx, tx, fmt.Sprintf("kick member %d from guild %d", targetID, guildID),
			sqlDeleteMember, guildID, targetID); err != nil {
			return err
		}
		// I3:成员行消失即清该玩家的全部申请。成员期间这些申请按 I1 本来就无效,
		// 删掉语义不变;它兜住的是"申请与他帮审批并发"留下的竞态残留 —— 不删的话,
		// 这些行会在他离帮后按 I1 重新"复活"。
		if _, err := tx.ExecContext(ctx, sqlDeletePlayerApplication, targetID); err != nil {
			return fmt.Errorf("delete applications of kicked member %d: %w", targetID, err)
		}

		snapshot, err := txSnapshot(ctx, tx, guildID)
		if err != nil {
			return err
		}
		res.Guild, res.Changed = snapshot, true
		out = res
		return nil
	})
	if err != nil {
		return MemberWriteResult{}, err
	}
	r.invalidateAfterCommit(ctx, opKick, guildID, targetID)
	return out, nil
}

// TransferLeader 把帮主转让给目标,原帮主按长老位余量降为长老或成员。
//
// 输入约束:actorID != targetID;officerCap != nil。
// 不变量:事务结束时 guild.leader_id 与唯一一行 role=3 必须指向同一人(第 9 步硬断言,不满足即回滚)。
// 事务边界:单事务 op=transfer,锁序 guild → guild_member。
// 错误语义:ErrLeaderMismatch(双存储已被破坏,fail-closed 不写)、ErrGuildLevelConfigMissing,
// 其余同 SetMemberRole。幂等:重放时操作者已不是帮主 → ErrRankTooLow。
func (r *GuildRepo) TransferLeader(ctx context.Context, guildID, actorID, targetID uint64, officerCap OfficerCapFunc) (MemberWriteResult, error) {
	if actorID == targetID {
		return MemberWriteResult{}, fmt.Errorf("transfer leader of guild %d: actor and target must differ", guildID)
	}
	if officerCap == nil {
		return MemberWriteResult{}, fmt.Errorf("transfer leader of guild %d: officer cap lookup not injected", guildID)
	}

	var out MemberWriteResult
	err := r.inTx(ctx, opTransfer, func(ctx context.Context, tx *sql.Tx) error {
		var res MemberWriteResult

		guild, err := lockGuildRow(ctx, tx, guildID)
		if err != nil {
			return err
		}
		actorRole, targetRole, err := lockMemberPair(ctx, tx, guildID, actorID, targetID)
		if err != nil {
			return err
		}
		if !canTransferLeader(actorRole) {
			return ErrRankTooLow
		}
		// guild.leader_id 与 guild_member.role=3 是同一事实的两份存储。对不上时不猜哪份对,
		// 直接拒写:在这种状态下转让会让两份存储分叉得更远。
		if guild.LeaderID != actorID {
			logx.Errorf("[guild] transfer: guild %d leader_id=%d disagrees with role=3 holder %d",
				guildID, guild.LeaderID, actorID)
			return ErrLeaderMismatch
		}

		maxOfficers, ok := officerCap(guild.Level)
		if !ok {
			return ErrGuildLevelConfigMissing
		}
		officers, err := countMembersByRole(ctx, tx, guildID, constants.RoleOfficer)
		if err != nil {
			return err
		}
		// 目标如果本来就是长老,转让后他腾出一个长老位,原帮主正好可以补进去。
		if targetRole == constants.RoleOfficer && officers > 0 {
			officers--
		}
		oldLeaderRole := demotedLeaderRole(officers, maxOfficers)

		// 三条写顺序固定:先 guild 行(锁序位置 1 的表已持锁),再目标、再原帮主。
		if err := execExactlyOneRow(ctx, tx, fmt.Sprintf("set leader of guild %d", guildID),
			`UPDATE guild SET leader_id = ? WHERE guild_id = ?`, targetID, guildID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, sqlUpdateMemberRole, constants.RoleLeader, guildID, targetID); err != nil {
			return fmt.Errorf("promote member %d to leader of guild %d: %w", targetID, guildID, err)
		}
		if _, err := tx.ExecContext(ctx, sqlUpdateMemberRole, oldLeaderRole, guildID, actorID); err != nil {
			return fmt.Errorf("demote former leader %d of guild %d: %w", actorID, guildID, err)
		}

		leaders, err := countMembersByRole(ctx, tx, guildID, constants.RoleLeader)
		if err != nil {
			return err
		}
		if leaders != 1 {
			return fmt.Errorf("transfer leader of guild %d: expected exactly 1 leader after write, got %d", guildID, leaders)
		}

		snapshot, err := txSnapshot(ctx, tx, guildID)
		if err != nil {
			return err
		}
		res.Guild, res.Changed = snapshot, true
		out = res
		return nil
	})
	if err != nil {
		return MemberWriteResult{}, err
	}
	r.invalidateAfterCommit(ctx, opTransfer, guildID)
	return out, nil
}

// LeaveGuild:玩家主动退帮(取代旧的 RemoveMember)。
//
// 不变量:帮主不能退帮(必须先转让或解散),否则帮会会变成无主状态。
// 事务边界:单事务 op=leave,锁序 guild → guild_member → guild_application(I3)。
// 错误语义:ErrGuildGone / ErrNotGuildMember / ErrLeaderCantLeave / ErrWriteConflict。
// 幂等:重放得到 ErrNotGuildMember,logic 复核映射为 0 后按成功处理。
func (r *GuildRepo) LeaveGuild(ctx context.Context, guildID, playerID uint64) (MemberWriteResult, error) {
	var out MemberWriteResult
	err := r.inTx(ctx, opLeave, func(ctx context.Context, tx *sql.Tx) error {
		var res MemberWriteResult

		guild, err := lockGuildRow(ctx, tx, guildID)
		if err != nil {
			return err
		}
		var role uint32
		err = tx.QueryRowContext(ctx, sqlLockMemberRole, guildID, playerID).Scan(&role)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotGuildMember
		}
		if err != nil {
			return fmt.Errorf("lock leaving member %d of guild %d: %w", playerID, guildID, err)
		}
		// 两份存储任一说他是帮主就拒绝:这里宁可多拒一次,也不能放出"帮主已退、leader_id 还指着他"的状态。
		if constants.Rank(role) == constants.RankLeader || guild.LeaderID == playerID {
			return ErrLeaderCantLeave
		}

		if err := execExactlyOneRow(ctx, tx, fmt.Sprintf("remove leaving member %d from guild %d", playerID, guildID),
			sqlDeleteMember, guildID, playerID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, sqlDeletePlayerApplication, playerID); err != nil { // I3
			return fmt.Errorf("delete applications of leaving member %d: %w", playerID, err)
		}

		snapshot, err := txSnapshot(ctx, tx, guildID)
		if err != nil {
			return err
		}
		res.Guild, res.Changed = snapshot, true
		out = res
		return nil
	})
	if err != nil {
		return MemberWriteResult{}, err
	}
	r.invalidateAfterCommit(ctx, opLeave, guildID, playerID)
	return out, nil
}

// ── 申请事务(§8) ──────────────────────────────────────────

// ApplyToGuild 提交入帮申请;同帮重复申请 = 刷新有效期并成功。
//
// 输入约束:rules 三个字段都 > 0(0 代表配表没读到,按配置错误拒绝而不是当"无限制");
// requiredZone > 0 时校验帮会归属区;now 由 logic 传入(服务进程时钟,毫秒)。
// 不变量:同一玩家的并发申请由 guild_player_state 行锁串行,因此"每人待审上限"在 TiDB 上也成立(X-10)。
// 事务边界:单事务 op=apply,锁序 guild → guild_player_state → guild_member(非锁定读)→ guild_application。
// 错误语义:ErrGuildGone / ErrGuildZoneMismatch / ErrPlayerAlreadyInGuild / ErrGuildFull /
// ErrApplicationLimit / ErrApplicationQueueFull / ErrWriteConflict。
// 幂等:重复申请同一帮 → Inserted=false(只刷新 apply_ms/expire_ms),不推送。
func (r *GuildRepo) ApplyToGuild(ctx context.Context, guildID, playerID uint64, requiredZone uint32, now uint64, rules ApplicationRules) (ApplyResult, error) {
	if rules.TTLMs == 0 || rules.MaxPerPlayer == 0 || rules.MaxPerGuild == 0 {
		return ApplyResult{}, fmt.Errorf("apply to guild %d: application rules not configured (%+v)", guildID, rules)
	}
	// 事务外建行:对不存在的行做加锁读再插入会互等成死锁(01-storage.md §2.1 规则 3)。
	if err := r.ensurePlayerStateRow(ctx, playerID, now); err != nil {
		return ApplyResult{}, err
	}

	var out ApplyResult
	err := r.inTx(ctx, opApply, func(ctx context.Context, tx *sql.Tx) error {
		var res ApplyResult

		guild, err := lockGuildRow(ctx, tx, guildID)
		if err != nil {
			return err
		}
		if requiredZone != 0 && guild.ZoneID != requiredZone {
			return ErrGuildZoneMismatch
		}
		if err := lockPlayerState(ctx, tx, playerID); err != nil {
			return err
		}

		// 非锁定一致性读:加锁读会在 uk_guild_member 上取间隙锁,与并发审批的 INSERT 形成更多死锁环。
		// "申请人已入帮"的竞态由 I1(列表与计数过滤)与审批时的 1062 分支兜住,这里不加锁不会产生错误的成员关系。
		var currentGuild uint64
		err = tx.QueryRowContext(ctx, sqlSelectMemberGuild, playerID).Scan(&currentGuild)
		switch {
		case err == nil:
			return ErrPlayerAlreadyInGuild
		case errors.Is(err, sql.ErrNoRows):
		default:
			return fmt.Errorf("read current guild of player %d: %w", playerID, err)
		}

		if _, err := tx.ExecContext(ctx, sqlDeleteExpiredOfPlayer, playerID, now); err != nil {
			return fmt.Errorf("purge expired applications of player %d: %w", playerID, err)
		}
		if _, err := tx.ExecContext(ctx, sqlDeleteExpiredOfGuild, guildID, now); err != nil {
			return fmt.Errorf("purge expired applications of guild %d: %w", guildID, err)
		}

		// 加锁读本人剩余(即全部未过期)的申请:上限判定必须基于加锁读,
		// RC 下的普通快照读看不到刚提交的并发插入(01-storage.md §2.1 规则 4)。
		existing, pending, err := lockPendingApplications(ctx, tx, playerID, guildID)
		if err != nil {
			return err
		}

		if existing {
			// 刷新排在满员与两个上限判定**之前**:这一行本来就已经计入所有计数,
			// 契约也规定"同帮重复申请 = 刷新并成功",满员帮只拒绝**新**申请人。
			// 这条 UPDATE 是**幂等**的:玩家双击时两次申请可能落在同一毫秒,
			// apply_ms / expire_ms 与库里现值逐字相同 ⇒ RowsAffected = 0。
			// 那仍然是"刷新成功"(行就在那儿,值就是要写的值),所以这里不能断言恰好一行 ——
			// 断言会把一次正常双击变成内部错误。行的存在性已由上面 FOR UPDATE 的 existing 证明。
			if _, err := tx.ExecContext(ctx, sqlRefreshApplication, now, now+rules.TTLMs, guildID, playerID); err != nil {
				return fmt.Errorf("refresh application of player %d to guild %d: %w", playerID, guildID, err)
			}
			res.Inserted = false
			out = res
			return nil
		}

		memberCount, err := countMembers(ctx, tx, guildID)
		if err != nil {
			return err
		}
		if memberCount >= guild.MaxMembers {
			return ErrGuildFull
		}
		if pending >= rules.MaxPerPlayer {
			return ErrApplicationLimit
		}
		var liveOfGuild uint32
		if err := tx.QueryRowContext(ctx, sqlCountLiveApplicationsOfGuild, guildID, now).Scan(&liveOfGuild); err != nil {
			return fmt.Errorf("count live applications of guild %d: %w", guildID, err)
		}
		if liveOfGuild >= rules.MaxPerGuild {
			return ErrApplicationQueueFull
		}

		if _, err := tx.ExecContext(ctx, sqlInsertApplication, guildID, playerID, now, now+rules.TTLMs); err != nil {
			if isDuplicateKey(err) {
				// 走到这里说明有人绕过了 guild_player_state 的串行化(或该行被手工删掉),
				// 不猜、不吞:回可重试的忙错误,玩家原地重试会命中上面的刷新分支。
				logx.Errorf("[guild] apply: duplicate application (%d,%d) despite player state lock", guildID, playerID)
				return ErrWriteConflict
			}
			return fmt.Errorf("insert application of player %d to guild %d: %w", playerID, guildID, err)
		}

		reviewers, err := listReviewers(ctx, tx, guildID)
		if err != nil {
			return err
		}
		res.Inserted, res.ReviewerIDs = true, reviewers
		out = res
		return nil
	})
	if err != nil {
		return ApplyResult{}, err
	}
	// 不失效任何缓存:pending_application_count 不进缓存,由请求者每次现算(§12.4)。
	return out, nil
}

// lockPendingApplications 加锁读出本人全部未过期申请,返回"是否已申请过 guildID"与总条数。
// 调用前必须已删掉本人的过期行,否则计数会把过期行也算进上限。
func lockPendingApplications(ctx context.Context, tx *sql.Tx, playerID, guildID uint64) (existing bool, pending uint32, err error) {
	rows, err := tx.QueryContext(ctx, sqlLockMyApplications, playerID)
	if err != nil {
		return false, 0, fmt.Errorf("lock pending applications of player %d: %w", playerID, err)
	}
	defer rows.Close()

	for rows.Next() {
		var appliedGuildID uint64
		if err := rows.Scan(&appliedGuildID); err != nil {
			return false, 0, fmt.Errorf("scan pending application of player %d: %w", playerID, err)
		}
		pending++
		if appliedGuildID == guildID {
			existing = true
		}
	}
	if err := rows.Err(); err != nil {
		return false, 0, fmt.Errorf("iterate pending applications of player %d: %w", playerID, err)
	}
	return existing, pending, nil
}

// listReviewers 返回该帮的长老与帮主(player_id 升序),即 APPLICATION_RECEIVED 的收件人。
func listReviewers(ctx context.Context, tx *sql.Tx, guildID uint64) ([]uint64, error) {
	rows, err := tx.QueryContext(ctx, sqlSelectReviewers, guildID, constants.RoleOfficer, constants.RoleLeader)
	if err != nil {
		return nil, fmt.Errorf("list reviewers of guild %d: %w", guildID, err)
	}
	defer rows.Close()

	var reviewers []uint64
	for rows.Next() {
		var playerID uint64
		if err := rows.Scan(&playerID); err != nil {
			return nil, fmt.Errorf("scan reviewer of guild %d: %w", guildID, err)
		}
		reviewers = append(reviewers, playerID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reviewers of guild %d: %w", guildID, err)
	}
	return reviewers, nil
}

// CancelApplication 撤回一条申请。
//
// 不开事务:单行单锁的删除,autocommit 足够(锁等待同样受 innodb_lock_wait_timeout=1 封顶)。
// 语义:删掉一条**未过期**的行 → nil;没删到 → 顺手把可能存在的过期行也删掉,返回 ErrApplicationNotFound。
// 幂等:重复撤回得到 ErrApplicationNotFound。1205 / 1213 经 retryOnDeadlock 归一到 ErrWriteConflict。
func (r *GuildRepo) CancelApplication(ctx context.Context, guildID, playerID, now uint64) error {
	return retryOnDeadlock(ctx, opCancel, func() error {
		result, err := r.db.ExecContext(ctx,
			`DELETE FROM guild_application WHERE guild_id = ? AND player_id = ? AND expire_ms > ?`,
			guildID, playerID, now)
		if err != nil {
			return fmt.Errorf("cancel application (%d,%d): %w", guildID, playerID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("cancel application (%d,%d): read rows affected: %w", guildID, playerID, err)
		}
		if affected == 1 {
			return nil
		}
		if _, err := r.db.ExecContext(ctx, sqlDeleteApplication, guildID, playerID); err != nil {
			return fmt.Errorf("purge expired application (%d,%d): %w", guildID, playerID, err)
		}
		return ErrApplicationNotFound
	})
}

// ListMyApplications 返回本人未过期的申请,至多 limit 条,按申请时间倒序。
// 展示用的非锁定读(不参与任何上限判定)。limit 为 0 时直接返回空。
func (r *GuildRepo) ListMyApplications(ctx context.Context, playerID, now uint64, limit uint32) ([]ApplicationRow, error) {
	if limit == 0 {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx, sqlListMyApplications, playerID, now, limit)
	if err != nil {
		return nil, fmt.Errorf("list applications of player %d: %w", playerID, err)
	}
	defer rows.Close()

	var out []ApplicationRow
	for rows.Next() {
		var row ApplicationRow
		if err := rows.Scan(&row.GuildID, &row.ApplyMs, &row.ExpireMs); err != nil {
			return nil, fmt.Errorf("scan application of player %d: %w", playerID, err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate applications of player %d: %w", playerID, err)
	}
	return out, nil
}

// ListApplicants 返回该帮的待审申请人,按不变式 I1 过滤(未过期且申请人没有成员行),
// 按申请时间正序,至多 limit 条。展示用的非锁定读。
func (r *GuildRepo) ListApplicants(ctx context.Context, guildID, now uint64, limit uint32) ([]ApplicantRow, error) {
	if limit == 0 {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx, sqlListApplicants, guildID, now, limit)
	if err != nil {
		return nil, fmt.Errorf("list applicants of guild %d: %w", guildID, err)
	}
	defer rows.Close()

	var out []ApplicantRow
	for rows.Next() {
		var row ApplicantRow
		if err := rows.Scan(&row.PlayerID, &row.ApplyMs, &row.ExpireMs); err != nil {
			return nil, fmt.Errorf("scan applicant of guild %d: %w", guildID, err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate applicants of guild %d: %w", guildID, err)
	}
	return out, nil
}

// MemberRole 非锁定读权威 role,供只读 RPC(如 ListGuildApplications)做授权。
// 只能用于读路径:写路径的授权必须在事务里对锁住的行复核,否则并发降级就能被绕过。
func (r *GuildRepo) MemberRole(ctx context.Context, guildID, playerID uint64) (role uint32, found bool, err error) {
	err = r.db.QueryRowContext(ctx, sqlSelectMemberRole, guildID, playerID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read role of member %d in guild %d: %w", playerID, guildID, err)
	}
	return role, true, nil
}

// CountLiveApplications 按 I1 统计该帮的待审申请数,供 GuildInfo.pending_application_count 现算。
// 刻意不进缓存:它随任意玩家的申请 / 撤回而变,缓存它等于给每次申请都加一次跨帮失效。
func (r *GuildRepo) CountLiveApplications(ctx context.Context, guildID, now uint64) (uint32, error) {
	var count uint32
	if err := r.db.QueryRowContext(ctx, sqlCountLiveApplicationsOfGuild, guildID, now).Scan(&count); err != nil {
		return 0, fmt.Errorf("count live applications of guild %d: %w", guildID, err)
	}
	return count, nil
}

// ReviewApplication 审批一条入帮申请。
//
// 输入约束:actorID != applicantID;approve 为真时 applicantZone 必须 > 0
// (logic 必须先查到申请人归属区;查不到就不许放人进来,fail-closed)。
// 不变量:通过时申请人恰好获得一行 guild_member,并按 I2 清掉他在**所有**帮会的申请。
// 事务边界:单事务 op=review,锁序 guild → guild_player_state(仅通过分支)→ guild_member → guild_application。
// 错误语义:ErrGuildGone / ErrNotGuildMember / ErrRankTooLow / ErrApplicationNotFound /
// ErrGuildFull(保留申请,回滚)/ ErrWriteConflict。
// 幂等:申请已被处理 / 已过期 / 申请人已入他帮 / 归属区与帮会不一致 → 一律删行并回 ErrApplicationNotFound,
// 对审批者来说都是"这条已经不能批了";分得更细只会泄露申请人的状态。
func (r *GuildRepo) ReviewApplication(ctx context.Context, guildID, actorID, applicantID uint64, approve bool, applicantZone uint32, now uint64) (ReviewResult, error) {
	if actorID == applicantID {
		return ReviewResult{}, fmt.Errorf("review application of guild %d: reviewer and applicant must differ", guildID)
	}
	if approve && applicantZone == 0 {
		return ReviewResult{}, fmt.Errorf("review application of guild %d: applicant %d home zone unknown", guildID, applicantID)
	}
	if approve {
		if err := r.ensurePlayerStateRow(ctx, applicantID, now); err != nil {
			return ReviewResult{}, err
		}
	}

	var out ReviewResult
	err := r.inTx(ctx, opReview, func(ctx context.Context, tx *sql.Tx) error {
		var res ReviewResult

		guild, err := lockGuildRow(ctx, tx, guildID)
		if err != nil {
			return err
		}
		// 只有通过分支会给申请人插成员行,所以只有它需要占申请人的串行化锁位(X-10)。
		// 位置 2 必须在锁 guild_member 之前拿,否则同一玩家的"审批通过"与"申请"会逆序互等。
		if approve {
			if err := lockPlayerState(ctx, tx, applicantID); err != nil {
				return err
			}
		}

		var actorRole uint32
		err = tx.QueryRowContext(ctx, sqlLockMemberRole, guildID, actorID).Scan(&actorRole)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotGuildMember
		}
		if err != nil {
			return fmt.Errorf("lock reviewer %d of guild %d: %w", actorID, guildID, err)
		}
		if !canReviewApplications(actorRole) {
			return ErrRankTooLow
		}

		var expireMs uint64
		err = tx.QueryRowContext(ctx, sqlSelectApplicationExpire, guildID, applicantID).Scan(&expireMs)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrApplicationNotFound
		}
		if err != nil {
			return fmt.Errorf("lock application (%d,%d): %w", guildID, applicantID, err)
		}
		if expireMs <= now {
			if _, err := tx.ExecContext(ctx, sqlDeleteApplication, guildID, applicantID); err != nil {
				return fmt.Errorf("delete expired application (%d,%d): %w", guildID, applicantID, err)
			}
			// 先提交删除、再回 NotFound:直接返回错误会让 defer Rollback 把删除一起撤销。
			return errCommitThen{inner: ErrApplicationNotFound}
		}

		if !approve {
			if _, err := tx.ExecContext(ctx, sqlDeleteApplication, guildID, applicantID); err != nil {
				return fmt.Errorf("reject application (%d,%d): %w", guildID, applicantID, err)
			}
			snapshot, err := txSnapshot(ctx, tx, guildID)
			if err != nil {
				return err
			}
			res.Guild, res.Approved = snapshot, false
			out = res
			return nil
		}

		// zone 复核:unmerge 只把清单里帮会的 guild.zone_id 改回源区,不动 guild_application。
		// 合服开服到回滚之间提交的"目标区玩家 → 源区帮"申请在回滚后仍然存在,
		// 批准它就会造出一个跨区成员。这一刀与旧 AddMemberInZone 是同一条防线。
		if guild.ZoneID != applicantZone {
			if _, err := tx.ExecContext(ctx, sqlDeleteApplication, guildID, applicantID); err != nil {
				return fmt.Errorf("delete cross-zone application (%d,%d): %w", guildID, applicantID, err)
			}
			return errCommitThen{inner: ErrApplicationNotFound}
		}

		memberCount, err := countMembers(ctx, tx, guildID)
		if err != nil {
			return err
		}
		if memberCount >= guild.MaxMembers {
			// 回滚并**保留**申请:人满只是暂时的,不该因为一次审批把申请人的名额烧掉。
			return ErrGuildFull
		}

		if _, err := tx.ExecContext(ctx, sqlInsertMember,
			guildID, applicantID, constants.RoleMember, now, now); err != nil {
			if isDuplicateKey(err) {
				// uk_guild_member 拒绝 = 申请人在别处已经入帮(两帮并发审批同一人)。
				// InnoDB 只回滚这条语句,所以可以接着删申请行再提交。
				if _, err := tx.ExecContext(ctx, sqlDeleteApplication, guildID, applicantID); err != nil {
					return fmt.Errorf("delete application of already-joined applicant (%d,%d): %w", guildID, applicantID, err)
				}
				return errCommitThen{inner: ErrApplicationNotFound}
			}
			return fmt.Errorf("insert approved member %d into guild %d: %w", applicantID, guildID, err)
		}

		// I2:成员行出现即清该玩家在所有帮会的申请。漏了这一步,他日后退帮时
		// 72h 内的旧申请会按 I1 重新"复活"。
		if _, err := tx.ExecContext(ctx, sqlDeletePlayerApplication, applicantID); err != nil {
			return fmt.Errorf("delete applications of approved player %d: %w", applicantID, err)
		}

		snapshot, err := txSnapshot(ctx, tx, guildID)
		if err != nil {
			return err
		}
		res.Guild, res.Approved = snapshot, true
		out = res
		return nil
	})
	if err != nil {
		return ReviewResult{}, err
	}
	// 拒绝分支只动了 guild_application,它不进缓存,不必翻代。
	if out.Approved {
		r.invalidateAfterCommit(ctx, opReview, guildID, applicantID)
	}
	return out, nil
}

// DisbandGuild 解散帮会(取代旧的 DeleteGuild)。
//
// 授权读 MySQL 的 leader_id,不读缓存的 LeaderID —— 缓存在转让之后最长陈旧 30 分钟,
// 按它授权等于让前帮主还能解散帮会。
// 事务边界:单事务 op=disband,锁序 guild → guild_member(全部成员)→ guild_application。
// 错误语义:ErrGuildGone / ErrRankTooLow(logic 映射为 kGuildNotLeader)/ ErrWriteConflict。
// 幂等:重放得到 ErrGuildGone。
// 扩展点:B5(捐献提前截止)、B6(活动进度 / 历练战报)按 90-consistency X-14 的步骤插在删申请之后、删成员之前。
func (r *GuildRepo) DisbandGuild(ctx context.Context, guildID, actorID uint64) (DisbandResult, error) {
	var out DisbandResult
	err := r.inTx(ctx, opDisband, func(ctx context.Context, tx *sql.Tx) error {
		var res DisbandResult

		guild, err := lockGuildRow(ctx, tx, guildID)
		if err != nil {
			return err
		}
		if guild.LeaderID != actorID {
			return ErrRankTooLow
		}
		res.ZoneID = guild.ZoneID

		memberIDs, err := lockAllMembers(ctx, tx, guildID)
		if err != nil {
			return err
		}
		res.MemberIDs = memberIDs

		if _, err := tx.ExecContext(ctx, sqlDeleteGuildApplications, guildID); err != nil {
			return fmt.Errorf("delete applications of guild %d: %w", guildID, err)
		}
		// I3:成员们在别的帮会留下的申请也要删,否则解散后它们会按 I1 复活。
		if err := deleteApplicationsOfPlayers(ctx, tx, memberIDs); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, sqlDeleteGuildMembers, guildID); err != nil {
			return fmt.Errorf("delete members of guild %d: %w", guildID, err)
		}
		// 0 行 = 这一瞬间帮会已经被别的事务解散了(我们持有的行锁本该挡住,
		// 但库层面真发生时要给出**业务**答复而不是内部错误):回 ErrGuildGone,
		// logic 层会映射成 kGuildNotFound。>1 行则是 WHERE 写错,属于必须暴露的内部错误。
		result, err := tx.ExecContext(ctx, `DELETE FROM guild WHERE guild_id = ?`, guildID)
		if err != nil {
			return fmt.Errorf("delete guild %d: %w", guildID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("delete guild %d: read rows affected: %w", guildID, err)
		}
		switch {
		case affected == 0:
			return ErrGuildGone
		case affected > 1:
			return fmt.Errorf("delete guild %d: deleted %d rows", guildID, affected)
		}

		out = res
		return nil
	})
	if err != nil {
		return DisbandResult{}, err
	}
	r.invalidateAfterCommit(ctx, opDisband, guildID, out.MemberIDs...)
	return out, nil
}

// lockAllMembers 按主键升序锁住该帮全部成员行,返回 player_id 升序列表。
func lockAllMembers(ctx context.Context, tx *sql.Tx, guildID uint64) ([]uint64, error) {
	rows, err := tx.QueryContext(ctx, sqlLockMemberIDs, guildID)
	if err != nil {
		return nil, fmt.Errorf("lock members of guild %d: %w", guildID, err)
	}
	defer rows.Close()

	var memberIDs []uint64
	for rows.Next() {
		var playerID uint64
		if err := rows.Scan(&playerID); err != nil {
			return nil, fmt.Errorf("scan member of guild %d: %w", guildID, err)
		}
		memberIDs = append(memberIDs, playerID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate members of guild %d: %w", guildID, err)
	}
	return memberIDs, nil
}

// UpdateAnnouncement 改公告并返回事务内权威快照。
//
// 授权在锁内按 MySQL 的 role 复核(canSetAnnouncement:长老与帮主)。
// 事务边界:单事务 op=announcement,锁序 guild → guild_member。
// 错误语义:ErrGuildGone / ErrAnnouncementForbidden / ErrWriteConflict。
// 幂等:写同样的文本也算一次成功(不做"内容未变"的短路,公告文本相同但玩家期望看到成功回包)。
// 入参 text 的长度由 logic 按 constants.MaxAnnouncementBytes 校验,repo 不重复裁剪。
func (r *GuildRepo) UpdateAnnouncement(ctx context.Context, guildID, playerID uint64, text string) (*GuildData, error) {
	var out *GuildData
	err := r.inTx(ctx, opAnnouncement, func(ctx context.Context, tx *sql.Tx) error {
		var snapshot *GuildData

		if _, err := lockGuildRow(ctx, tx, guildID); err != nil {
			return err
		}
		var role uint32
		err := tx.QueryRowContext(ctx, sqlLockMemberRole, guildID, playerID).Scan(&role)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrAnnouncementForbidden
		}
		if err != nil {
			return fmt.Errorf("lock announcement operator %d in guild %d: %w", playerID, guildID, err)
		}
		if !canSetAnnouncement(role) {
			return ErrAnnouncementForbidden
		}

		if _, err := tx.ExecContext(ctx, sqlUpdateAnnouncement, text, guildID); err != nil {
			return fmt.Errorf("update announcement of guild %d: %w", guildID, err)
		}

		snapshot, err = txSnapshot(ctx, tx, guildID)
		if err != nil {
			return err
		}
		out = snapshot
		return nil
	})
	if err != nil {
		return nil, err
	}
	r.invalidateAfterCommit(ctx, opAnnouncement, guildID)
	return out, nil
}
