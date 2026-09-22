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
//     "两个正常玩法互相等死"的写法。2026-09-21 死锁修复(friend 事故 + 全仓审计 #2 #3 #6)后再加三条:
//     (a) 同表多行按主键升序**逐行**取锁;
//     (b) 锁定读 / UPDATE / DELETE 只做**完整主键等值**的点操作 —— 需要按二级条件找行时,先普通读(RC 语句级快照,
//     不加锁)取候选主键、排序,再逐行点锁 / 点删,WHERE 带原条件作提交点复核(影响 0 行 = 别人已处理,跳过);
//     guild_member 的主键 (guild_id, player_id) 之外另有 uk(player_id),完整主键等值会同时钉死 uk,
//     锁定 SELECT / UPDATE 一律 `FORCE INDEX (PRIMARY)`(单表 DELETE 不收索引提示,靠 EXPLAIN 回归钉住
//     它走 PRIMARY,且那一行在同事务里已先经 FORCE INDEX 锁读持有)。锁集由执行计划决定、不由 SQL 文本决定,
//     经二级索引加锁就是"先二级后主键",与按主键删行的"先主键后二级"在同一行上反序(事故 §3.3);
//     (c) 任何**插入或删除 guild_member 行**的事务(建帮、审批通过、踢人、退帮、解散)都先持有该玩家的
//     guild_player_state 行锁:同一玩家的成员行、uk 项与他名下全部申请行的写者由此串行,
//     "离帮删成员 + 删申请"与"别帮审批通过同一人"不再能在 uk_guild_member(p) 与 guild_application(G2,p) 上成环。
//     (d) **全局插入守卫**(G-OUT1 / G-C3):guild_player_state 里 player_id = 0 的哨兵行(globalInsertGuardPlayerID,
//     启动期由 EnsureGlobalInsertGuard 建好、永不删除)。持有它的只有三类事务,都在**事务内**用 lockGlobalInsertGuard 点锁:
//       - 唯一二级索引的查重插入者:建帮(插 guild / guild_member)、审批通过(插 guild_member)。InnoDB 对唯一二级索引查重时,
//         对每条等值项(含未 purge 的删除标记项)及其后第一条记录加 S next-key(取锁规则 R12,RC 不豁免);两个查重插入者
//         只要插入点落在同一段间隙,各自的插入意向锁就被对方的 S next-key 挡住 —— 同名建帮、名字相邻的两帮、uk 上相邻的
//         两个刚离帮玩家都能中,谁和谁相邻与名字 / id 数值无关,没有能分片的守卫,只能全局串行;
//       - 建状态行的短事务(ensurePlayerStateRows 缺行时):首次建行者都排在已存在的哨兵行上,而不是排在别人未提交的新记录上,
//         首插者回滚不再能把两个排队者的 S 锁继承成同一段间隙上的互等(G-C3)。
//     位置:表内按 player_id 升序,0 最小 —— 建帮 S(0) → S(p);审批通过 G(G) → S(0) → S(p);建状态行 S(0) → 插 S(p)。
//     哨兵只为串行化"唯一二级索引查重插入者"与"状态行首插者";其余事务(申请、撤回、拒绝、踢人、退帮、解散、任免、转让、
//     经济)一概不取它。为什么不与它们成环:
//       - 不取哨兵的事务从不请求 S(0),所以"等 S(0)"的边只从三类持有者之间出发,而它们在 S(0) 上排成一列;
//       - 等 S(0) 时手里还有别的锁的只有审批通过(恰好一把 guild(G));而**任何**锁已有 guild 行的事务都把它作为第一把锁,
//         等 guild 行的一方手里没有别的数据库锁,等待链走到 guild(G) 就断了,回不到持 S(0) 的一方;
//       - 持 S(0) 的一方此后只请求 S(p)(p > 0,升序)、本帮 / 本人的成员行与申请行、唯一索引查重与新行,从不回头等任何
//         guild 行(建帮不锁已有 guild 行,审批通过的那一把 guild(G) 早于 S(0) 已持有),也从不等比 p 更小的状态行。
//     哨兵行缺失时 fail-closed(lockGlobalInsertGuard 回内部错误,不当忙重试,也不在请求路径上补建)。唯一性仍由 uk 裁决,
//     哨兵只为消掉这两类 1213;它替掉了此前的 Redis 全局互斥:库内行锁对 InnoDB / TiDB 的死锁检测可见、随事务提交释放,
//     没有 TTL 提前到期与"持行锁等 Redis"的跨系统互等,Redis 故障也不再挡住建帮与审批。
//     (e) TiDB:凡是悲观事务也会改的行,多 key 写(删带二级索引的行、改二级索引列)一律进显式 RC 事务,不走自动提交
//     (G-C2,理由见 asset_store.go 文件头的 TiDB 附加规则)。本文件据此把撤回申请与申请后的惰性清理改成逐行 RC 短事务,
//     建状态行也在 (d) 的 RC 短事务里做。进事务之外,**带复核谓词的点删之前还要先做完整主键 FOR UPDATE 点锁**(死锁复核 V2):
//     这类 DELETE 不走 Point_Get 快路径,语句末尾并行锁 {行 key, PRIMARY key},会与别人的 Point_Get(先 PRIMARY key 后行 key)
//     各持一半;撤回、申请事务内删本人过期行、申请后的惰性清理三处因此都先 sqlSelectApplicationExpire 点锁再带复核点删。
//     纯完整主键等值的 sqlDeleteApplication(无复核条件)本身就走快路径,不需要另加点锁。
//     仍在事务外自动提交的只剩启动期建哨兵行的 INSERT IGNORE(EnsureGlobalInsertGuard)——
//     它只插新行(TiDB 的行 key 是新分配的 rowid,没人持有、也没人提交时需要它),不会"部分持有"别人要的 key。
//     MySQL 上它在会话级命名锁(GET_LOCK)下串行:同一时刻至多一个建哨兵者,首插者回滚也不会留下两个排队者互等(死锁复核 C1)。
//     EXPLAIN 回归见 guild_lock_plan_mysql_test.go,并发回归见 guild_lock_order_mysql_test.go。
//  3. **提交之后才失效缓存**(§6.4)。MySQL 是真相,失效失败不把已提交的写报成失败,
//     否则客户端会因为 Redis 抖动进入 RequiresReconnect 隔离,而数据其实已经写成功了。

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
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
//
// **超预算不会被内部重试**:它归一成 ErrWriteConflict,而 ErrWriteConflict 不是可重试错误,
// retryOnDeadlock 见到就返回,由客户端自己重试(收到的是 GuildBusyRetry tip)。
// "每次尝试各给一份完整子预算"那条理由只对**死锁重跑**成立,别把两件事混起来。
const txBudget = 1500 * time.Millisecond

// txBudgetDisband:解散是全文件最重的事务 —— 锁 guild → 逐行锁全部成员的状态行、再逐行锁成员行(上限 100,
// 见 90-consistency X-16)→ 逐行点删本帮申请 + 成员们在别帮的申请(I3)→ 提前截止捐献 → 逐行删成员 → 删帮会。
// 满员时约 300–400 条主键点语句(死锁修复后全部拆成点操作,换取锁集与执行计划无关),局域网下百毫秒量级。
// 给它 1500ms 在慢库上会变成"每次都在 1.5s 处被砍、客户端永远重试不成功"的外部活锁,
// 所以单独放宽。仍然远小于请求预算,放锁的初衷不变。
// 上线前应当用真库量一次满员帮解散的 p99 再定这两个数。
const txBudgetDisband = 2500 * time.Millisecond

// txBudgetFor:按 op 取子预算。**固定映射**,新增批次要改就在这里改,不许在调用点传数字。
func txBudgetFor(op string) time.Duration {
	if op == opDisband {
		return txBudgetDisband
	}
	return txBudget
}

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
	budget := txBudgetFor(op)
	txCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	err := r.runTxBody(txCtx, fn)
	if err == nil {
		return nil
	}
	// 子预算到期而请求还活着 = 库慢或锁排队,是"稍后重试"而不是"失败";
	// 请求本身被取消(客户端断开 / 上游超时)则原样返回,不许伪装成写冲突。
	//
	// 判据用 **txCtx 到期 + 父 ctx 的 deadline 还没到**,不用 `ctx.Err() == nil`:
	// 后者是在 runTxBody 返回**之后**才读的,父 ctx 若恰好在这两步之间到期(剩余时间落在
	// [budget, budget+ε]),一次本该回 tip 的超预算就会被误判成父取消、以 gRPC 错误抛出去,
	// 把客户端推进重连隔离。比时间戳没有这个窗口。
	if errors.Is(txCtx.Err(), context.DeadlineExceeded) && !parentDeadlineReached(ctx) {
		guildTxBudgetExceededTotal.Inc(op)
		logx.Errorf("[guild] %s: transaction exceeded its %v budget: %v", op, budget, err)
		return ErrWriteConflict
	}
	return err
}

// parentDeadlineReached:父 ctx 是否**因为自己的 deadline** 而结束。
// 没有 deadline(内部调用)恒为 false;被显式取消(客户端断开)也算已结束 —— 那种情况
// 本来就该把 ctx 错误原样返回,不能当成超预算。
func parentDeadlineReached(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	deadline, ok := ctx.Deadline()
	return ok && !deadline.After(time.Now())
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
// 三类原样返回,只有"结果不明"才归一:
//  1. **可重试**的提交期错误(1213 / TiDB 9007 / errRetryTx)与锁等待(1205)——
//     交给 retryOnDeadlock 去分类、计数、重跑。这条最容易写错:**TiDB 的写冲突 9007 正是由
//     COMMIT 语句报出来的**,在这里归一成 ErrWriteConflict,等于把 9007 的重试在它唯一出现的
//     位置又关掉一次(ErrWriteConflict 不是可重试错误,retryOnDeadlock 见到它会立刻返回)。
//  2. ErrTxDone / ctx 取消 / ctx 超时:结论明确(事务没提交,或调用方自己走了)。
//  3. 其余(连接被重置、驱动报未知错误):**结果不明**,COMMIT 可能已经在库里生效了,只是回执没收到。
//     打 ERROR 让人看见,对外归一到 ErrWriteConflict —— 客户端收到"稍后重试",重试时先读当前状态,
//     幂等分支会兜住已生效的那一半。归一时用 %w 把 ErrWriteConflict 包进去、原因用 %v 留在文本里,
//     方便排障时知道到底是什么驱动错误。
func classifyCommitErr(err error) error {
	if err == nil {
		return nil
	}
	if isRetryableTxError(err) || isLockWaitTimeout(err) {
		return err
	}
	if errors.Is(err, sql.ErrTxDone) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	logx.Errorf("[guild] commit outcome unknown: %v", err)
	return fmt.Errorf("commit outcome unknown (%v): %w", err, ErrWriteConflict)
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
		txDeadlockObserved(op, err)
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

// txDeadlockObserved:retryOnDeadlock 每判定一次"事务已整体回滚、可重跑"(1213 / 9007 / errRetryTx)调用一次
// (建状态行的守卫短事务走 inTx、启动期建哨兵行走 retryOnDeadlock,都在内;建 seq 行自死锁复核 C2 起在 T-D / T-S 事务内做,
// 随 inTx 一起计)。
// 生产实现计 guild_tx_deadlock_total。锁序并发回归(guild_lock_order_mysql_test.go)把它换成记录器、断言修复后
// 的场景 0 次 —— inTx 会把 1213 吸收掉重跑,只看返回值看不见死锁;go-zero 的指标在未启用 Prometheus 时
// Inc 是空操作,测试里也读不回来。与 invalidateGaveUp 同一惯例:包级可替换钩子,只许单测替换。
var txDeadlockObserved = func(op string, _ error) { guildTxDeadlockTotal.Inc(op) }

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

// sessionIsolation:连接级隔离级别,写进 DSN 的会话变量 transaction_isolation。值带单引号 ——
// go-sql-driver 把它原样拼进 `SET transaction_isolation = <值>`,不带引号 MySQL 会把 READ-COMMITTED 当表达式解析报错。
const sessionIsolation = "'READ-COMMITTED'"

// WithLockWaitTimeout 给 DSN 附加两个会话变量(go-sql-driver 把未知参数当作连接建立后 SET 的系统变量;
// 二者都是会话级可写,appuser 有权设置),并顺手清掉 clientFoundRows(90-consistency Y-20):
//
//   - innodb_lock_wait_timeout = LockWaitTimeoutSeconds:锁等待封顶(§6.2a);
//   - transaction_isolation = READ-COMMITTED(2026-09-21 死锁修复契约 P5):inTx 的 BeginTx 本来就显式 RC,
//     但**事务外**的自动提交语句(启动期建哨兵行的 INSERT IGNORE、资产指令的领取、读路径)走的是会话默认
//     隔离级别,MySQL 默认 RR。RR 下这些语句的锁定扫描带间隙 / next-key 锁,锁集比本包按 RC 推演的取锁表大,
//     判环结论对它们不成立;shared/assetop.AllocateSeq 的未决行普通读也以"调用事务是 RC"为硬前提。
//     全连接统一 RC,推演口径只有一种。TiDB 只在悲观事务模式下支持 RC,部署时 tidb_txn_mode 须为 pessimistic。
//     DSN 里若已带 tx_isolation(MySQL 5.7 的旧名)一并去掉:两条 SET 的先后由 map 遍历序决定,不可预测。
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
	delete(cfg.Params, "tx_isolation")
	cfg.Params["transaction_isolation"] = sessionIsolation
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
		Help:      "帮会写事务跑满子预算(默认 1500ms、disband 2500ms)被中止的次数。**不做内部重试**,对外回 GuildBusyRetry 由客户端重试。稳态应为 0;非 0 说明库慢或帮会行锁排队。",
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

	// 下面四个由 B5 / B6 使用(90-consistency Y-04 要求 B2s 一次把集合定全)。
	opUpgrade       = "upgrade"
	opAssetFinalize = "asset_finalize"
	opActivity      = "activity"
	opTrialSettle   = "trial_settle"

	// B5b 追加:捐献预留(T-D)与兑换预留(T-S)两个写事务的标签。
	// 原注释"后续批次只调用、不再改本文件"就此作废:离帮 / 被踢 / 解散要提前截止未决捐献
	// (05 §5.20、X-14),B5b 本来就必须改这个文件;而 T-D / T-S 若借用 opAssetFinalize,
	// 预留与终结的死锁 / 超预算指标会混在同一个 label 下,排障时分不清是哪条路径在抢锁。
	opDonate = "donate"
	opShop   = "shop"

	// 启动期建全局插入守卫哨兵行(EnsureGlobalInsertGuard)的标签:它的 1213 / 9007 重试与 1205 单独计,
	// 不混进任何请求路径的 label。
	opInsertGuard = "insert_guard"
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

	// sqlDeleteGuild:解散的最后一条写。完整主键等值;行已由同一事务的 sqlLockGuild 锁住。
	sqlDeleteGuild = `DELETE FROM guild WHERE guild_id = ?`

	// sqlEnsurePlayerState / sqlLockPlayerState:锁序位置 2(X-10)。
	// 建行必须在**业务事务之外**:对不存在的行做加锁读再插入,在 RR 下靠间隙锁互等、
	// 在 TiDB 下根本锁不住后续插入(01-storage.md §2.1 规则 3)。
	// 建行前先用 sqlSelectExistingPlayerStatesHead/Tail 普通读挑出已存在的行,只对缺的在全局插入守卫下的 RC 短事务里插
	// (见 ensurePlayerStateRows);启动期建哨兵行本身(EnsureGlobalInsertGuard)是唯一的自动提交用法,
	// 在会话级命名锁下同一时刻至多一个插入者(C1)。
	sqlEnsurePlayerState = `INSERT IGNORE INTO guild_player_state (player_id, updated_ms) VALUES (?, ?)`
	sqlLockPlayerState   = `SELECT player_id FROM guild_player_state WHERE player_id = ? FOR UPDATE`
	// 普通读(不加锁),IN 列表按 100 分块,由调用方拼 placeholders。
	sqlSelectExistingPlayerStatesHead = `SELECT player_id FROM guild_player_state WHERE player_id IN (`
	sqlSelectExistingPlayerStatesTail = `)`

	// sqlLockMemberRole:锁序位置 3,成员行唯一的锁定读。一次只锁一行,多行由调用方按 player_id 升序逐条调用
	// (lockMemberPair / DisbandGuild)。FORCE INDEX (PRIMARY) 的理由见文件头 (b):WHERE 同时钉死了
	// uk_guild_member(player_id),不强制时优化器可以在两个 const 路径之间任选,走 uk 就是"先二级后主键"。
	// 经济批次(B5b)的捐献预留 / 升级 / 对侧账也复用这一条。
	sqlLockMemberRole = `SELECT role FROM guild_member FORCE INDEX (PRIMARY) WHERE guild_id = ? AND player_id = ? FOR UPDATE`

	sqlCountMembers       = `SELECT COUNT(*) FROM guild_member WHERE guild_id = ?`
	sqlCountMembersByRole = `SELECT COUNT(*) FROM guild_member WHERE guild_id = ? AND role = ?`
	sqlSelectMemberRole   = `SELECT role FROM guild_member WHERE guild_id = ? AND player_id = ?`
	sqlSelectMemberGuild  = `SELECT guild_id FROM guild_member WHERE player_id = ?`
	sqlSelectReviewers    = `SELECT player_id FROM guild_member WHERE guild_id = ? AND role IN (?, ?) ORDER BY player_id`

	sqlInsertMember = `INSERT INTO guild_member (guild_id, player_id, role, join_time_ms, last_active_ms, contribution_total, contribution_balance)
	VALUES (?, ?, ?, ?, ?, 0, 0)`
	// role 不在任何二级索引里:这条 UPDATE 只碰聚簇记录。FORCE INDEX (PRIMARY) 只为搜索阶段不走 uk。
	sqlUpdateMemberRole = `UPDATE guild_member FORCE INDEX (PRIMARY) SET role = ? WHERE guild_id = ? AND player_id = ?`
	// 单表 DELETE 不收索引提示:行已在同一事务里经 sqlLockMemberRole 锁住,计划由 EXPLAIN 回归钉住。
	// uk 项 (p, G) 的写者(插 / 删 p 的成员行)都先持有 p 的 guild_player_state 行锁(文件头 (c)),彼此串行;
	// 但它**可能**有一个别的持锁者:另一名玩家 x 的查重插入(建帮 / 审批通过)对"x 的等值项后面第一条记录"加的
	// S next-key(R12),那一条恰好可能是 p 的活 uk 项。这时本语句 delete-mark 它要等那一方提交 —— 只是单向等待:
	// 查重插入者此后只碰 x 自己名下的申请行与(建帮时)新 guild 行,不等本事务持有的任何锁;且同一时刻至多一个
	// 查重插入者(文件头 (d) 的全局插入守卫,持到提交)。
	sqlDeleteMember = `DELETE FROM guild_member WHERE guild_id = ? AND player_id = ?`

	// 申请表:锁序位置 4。只存待审,通过 / 拒绝 / 撤回 / 过期 / 解散都删行。
	// 锁定读与删除一律完整主键等值(文件头 (b));按 player_id / guild_id 找行只用下面几条**普通读**取候选主键。
	// sqlSelectApplicationExpire 同时是带复核条件的两条点删(sqlDeleteExpiredApplication / sqlCancelApplication)之前的
	// 主键点锁(死锁复核 V2,文件头 (e)):**只许**完整主键等值 + FOR UPDATE、不带复核条件,TiDB 才走 Point_Get 快路径。
	sqlSelectApplicationExpire = `SELECT expire_ms FROM guild_application WHERE guild_id = ? AND player_id = ? FOR UPDATE`
	sqlInsertApplication       = `INSERT INTO guild_application (guild_id, player_id, apply_ms, expire_ms) VALUES (?, ?, ?, ?) ON DUPLICATE KEY UPDATE apply_ms = apply_ms`
	sqlRefreshApplication      = `UPDATE guild_application SET apply_ms = ?, expire_ms = ? WHERE guild_id = ? AND player_id = ?`
	sqlDeleteApplication       = `DELETE FROM guild_application WHERE guild_id = ? AND player_id = ?`
	// sqlDeleteExpiredApplication:惰性清理的点删,`expire_ms <= ?` 是提交点复核 —— 候选读之后被刷新续期的行不删。
	sqlDeleteExpiredApplication = `DELETE FROM guild_application WHERE guild_id = ? AND player_id = ? AND expire_ms <= ?`
	// sqlCancelApplication:撤回只删**未过期**的那一行;没删到再用 sqlDeleteExpiredApplication 顺手清掉过期行。
	sqlCancelApplication = `DELETE FROM guild_application WHERE guild_id = ? AND player_id = ? AND expire_ms > ?`

	// sqlSelectApplicationExists:审批通过前的预读(完整主键等值的普通读,不加锁;G4,见 precheckApprovedApplication)。
	sqlSelectApplicationExists = `SELECT 1 FROM guild_application WHERE guild_id = ? AND player_id = ?`

	// 候选普通读(不加锁,走哪个索引都不影响锁集)。IN 列表按 100 分块,由调用方拼 placeholders。
	sqlSelectApplicationKeysOfPlayersHead = `SELECT guild_id, player_id FROM guild_application WHERE player_id IN (`
	sqlSelectApplicationKeysOfPlayersTail = `)`
	sqlSelectApplicantsOfGuild            = `SELECT player_id FROM guild_application WHERE guild_id = ? ORDER BY player_id`
	sqlSelectExpiredApplicationsOfPlayer  = `SELECT guild_id FROM guild_application WHERE player_id = ? AND expire_ms <= ? ORDER BY guild_id`
	// 本帮过期行的候选,每次至多 purgeExpiredApplicationsPerApply 行(LIMIT 由调用方传入该常量,见 purgeExpiredApplicationsOfGuild)。
	sqlSelectExpiredApplicantsOfGuild = `SELECT player_id FROM guild_application WHERE guild_id = ? AND expire_ms <= ? ORDER BY player_id LIMIT ?`
	// sqlCountPendingApplicationsOfPlayer:每人待审上限的计数。普通读即可 —— 能让它变大的只有本人的 ApplyToGuild,
	// 而那必须先持有同一行 guild_player_state;别人(撤回 / 拒绝 / 解散)只会让它变小,计数只会偏保守(见 ApplyToGuild)。
	sqlCountPendingApplicationsOfPlayer = `SELECT COUNT(*) FROM guild_application WHERE player_id = ? AND expire_ms > ?`

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

// errPlayerStateRowMissing:事务内点锁状态行时行不存在。lockPlayerState 用 %w 包它,
// lockGlobalInsertGuard 据此把"哨兵行缺失"与其它锁错误区分开。
var errPlayerStateRowMissing = errors.New("guild_player_state row missing")

// lockPlayerState 锁住玩家的串行化状态行(锁序位置 2)。
// 行必须已由 ensurePlayerStateRows 在事务外建好;查不到说明建行那步被跳过了,fail-closed。
func lockPlayerState(ctx context.Context, tx *sql.Tx, playerID uint64) error {
	var locked uint64
	err := tx.QueryRowContext(ctx, sqlLockPlayerState, playerID).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("player %d (ensurePlayerStateRows skipped): %w", playerID, errPlayerStateRowMissing)
	}
	if err != nil {
		return fmt.Errorf("lock guild player state %d: %w", playerID, err)
	}
	return nil
}

// ── 全局插入守卫(哨兵行;文件头 (d),2026-09-21 死锁复核 G-OUT1 / G-C3) ─────────

// globalInsertGuardPlayerID:全局插入守卫 = guild_player_state 里这一行。0 永远不是真实玩家:login 的 PlayerId
// 由 snowflake 发号恒 > 0;ensurePlayerStateRows 另外拒绝把 0 当玩家建行 / 锁行,免得某个带 0 的请求在别的锁序位置
// 抢到这把全局锁。行由启动期 EnsureGlobalInsertGuard 建好、永不删除。
const globalInsertGuardPlayerID uint64 = 0

// errGlobalInsertGuardMissing:哨兵行不在。不是可重试的忙错误 —— 它说明部署 / 启动步骤漏了,
// 重试多少次都一样;按内部错误 fail-closed,建帮 / 审批通过 / 首次建状态行一律拒绝,绝不在请求路径上补建
// (那会让"建哨兵"本身变成一个没有守卫的并发首插)。
var errGlobalInsertGuardMissing = errors.New("全局插入守卫哨兵行缺失(guild_player_state.player_id=0),检查启动期 EnsureGlobalInsertGuard")

// insertGuardInitLockName:启动期建哨兵行的会话级命名锁(死锁复核 C1,见 EnsureGlobalInsertGuard)。
// GET_LOCK 的名字在整个 MySQL 实例(TiDB 集群)内共用、不分库,所以带上库名;与 schemamigrate 的迁移锁
// ("mmorpg_db_migrate:<库名>")不同名 —— 两者在启动期先后获取、从不嵌套。长度 43 字节,低于 MySQL 的 64 字节上限。
// 测试库名不同但共用这个名字:并行的测试包之间只会短暂串行,无害。
const insertGuardInitLockName = "mmorpg_guild_insert_guard_init:" + DatabaseName

// insertGuardInitLockWaitSeconds:GET_LOCK 的等待上限,必须明显短于调用方 ctx(guild.go 给 10s)。
// ctx 若先到期,驱动会直接断开连接,拿不到"0 = 等满仍被持有"这个结果,也就没有机会复读。
const insertGuardInitLockWaitSeconds = 5

// insertGuardInitReleaseTimeout:RELEASE_LOCK 的独立超时。调用方 ctx 可能已经到期,锁仍然要放。
const insertGuardInitReleaseTimeout = 2 * time.Second

// lockGlobalInsertGuard 在事务内点锁全局插入守卫(sqlLockPlayerState,player_id = 0,完整主键 const)。
// 调用位置必须满足文件头 (d):状态行表内的第一把锁(0 最小),且之后不再回头等任何 guild 行。
// 行不存在 → 包了 errGlobalInsertGuardMissing 的内部错误;其余错误原样(1213 / 1205 交给 inTx 分类)。
func lockGlobalInsertGuard(ctx context.Context, tx *sql.Tx) error {
	err := lockPlayerState(ctx, tx, globalInsertGuardPlayerID)
	if errors.Is(err, errPlayerStateRowMissing) {
		return fmt.Errorf("lock global insert guard: %w", errGlobalInsertGuardMissing)
	}
	return err
}

// EnsureGlobalInsertGuard 保证全局插入守卫哨兵行存在。**启动期**调用一次(guild.go:建表之后、任何 goroutine / RPC
// 之前,失败拒启);幂等,多实例同时首建也不会死锁。
//
// 先做普通读:行已在(常态)就直接返回,不取任何锁 —— 运行中的实例可能正持着它(建帮 / 审批通过),新实例启动不该排在后面。
//
// 缺行时(首次部署)先在一条专用连接上取会话级命名锁,再在**同一连接**上复读,仍缺才自动提交 INSERT IGNORE。
// 为什么需要命名锁(死锁复核 C1):不串行时,首个插入者 A 的 INSERT 还没提交,B、C 的前置普通读看不见它(RC 只读已提交),
// 各自发出 INSERT IGNORE,撞上 A 的记录排队等 S。A 一旦回滚(被 KILL、binlog / 刷盘失败),记录被删,B、C 排队中的 S
// 会按 lock_rec_inherit_to_gap 继承成后继记录前那段间隙上的间隙 S(查重锁带 inherit_all,RC 也继承),
// 双方的插入意向锁互相挡住,InnoDB 报 1213(手册 "Locks Set by Different SQL Statements" 三会话 INSERT 的示例);
// 删除标记项尚未 purge 时则是双方各持已授予的 S、再都要 X 去改写它(取锁规则 H2),同样 1213。
// 有了命名锁,同一时刻至多一个建哨兵者碰这个主键:首插者回滚时没有第二个排队者,继承下来的间隙锁只属于唯一的等待者,
// 挡不住它自己的插入意向。等命名锁的一方手里没有任何 InnoDB 锁(前置读是普通读),所以命名锁和行锁之间也连不成环;
// 持命名锁的一方可能等的行锁(手工 / 测试事务、旧版本实例留下的)其持有者都不等命名锁,同样不成环。
//
// 复读、INSERT、插后确认都走 conn,原因有两个:一是命名锁是会话级的,语句若落到池里另一条连接上,持锁会话断开(锁随之释放)时
// 那条 INSERT 可能还在服务端执行,串行就被打破;二是持着 conn 再向池要连接,池满时会自己卡死自己。
// GET_LOCK 返回 0(等满仍被持有)或 NULL(服务端出错)时复读一次:行已在(持锁者已建好、只是还没放锁)就算成功;
// 仍缺就拒启(fail-closed),**绝不**无锁插入 —— 无锁插入正是这里要消灭的并发首插。
// retryOnDeadlock 保留作兜底(TiDB 9007;将来若有人绕开命名锁插 0 号行);1205 归一成 ErrWriteConflict。
// 插完再普通读一次确认(INSERT IGNORE 会把非重复类的异常降级成告警,不能只看返回值)。now 只写进 updated_ms(诊断用)。
// TiDB 上本来没有这个环(查重用 X 悲观锁,没有 S 锁,也没有间隙继承),命名锁多余但无害;
// 同一启动流程里 schemamigrate 已经在用 GET_LOCK,不新增部署前提。
func (r *GuildRepo) EnsureGlobalInsertGuard(ctx context.Context, now uint64) error {
	guardIDs := []uint64{globalInsertGuardPlayerID}
	missing, err := missingPlayerStateRows(ctx, r.db, guardIDs)
	if err != nil {
		return fmt.Errorf("read global insert guard row: %w", err)
	}
	if len(missing) == 0 {
		return nil
	}

	conn, err := r.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("open connection for global insert guard: %w", err)
	}
	defer conn.Close() // 连接断开时服务端也会自动释放命名锁

	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)",
		insertGuardInitLockName, insertGuardInitLockWaitSeconds).Scan(&got); err != nil {
		return fmt.Errorf("get lock %s for global insert guard: %w", insertGuardInitLockName, err)
	}
	if !got.Valid || got.Int64 != 1 {
		if stillMissing, readErr := missingPlayerStateRows(ctx, conn, guardIDs); readErr == nil && len(stillMissing) == 0 {
			return nil
		}
		return fmt.Errorf("get lock %s for global insert guard: not acquired within %ds (valid=%t result=%d): %w",
			insertGuardInitLockName, insertGuardInitLockWaitSeconds, got.Valid, got.Int64, errGlobalInsertGuardMissing)
	}
	defer releaseInsertGuardInitLock(conn) // defer 后进先出:先放锁,再关连接

	if missing, err = missingPlayerStateRows(ctx, conn, guardIDs); err != nil {
		return fmt.Errorf("re-read global insert guard row under lock: %w", err)
	}
	if len(missing) == 0 {
		return nil // 前一个持锁者已经建好并提交
	}
	if err := retryOnDeadlock(ctx, opInsertGuard, func() error {
		_, err := conn.ExecContext(ctx, sqlEnsurePlayerState, globalInsertGuardPlayerID, now)
		return err
	}); err != nil {
		return fmt.Errorf("create global insert guard row: %w", err)
	}
	if missing, err = missingPlayerStateRows(ctx, conn, guardIDs); err != nil {
		return fmt.Errorf("re-read global insert guard row: %w", err)
	}
	if len(missing) != 0 {
		return fmt.Errorf("create global insert guard row: still missing after INSERT IGNORE: %w", errGlobalInsertGuardMissing)
	}
	return nil
}

// releaseInsertGuardInitLock 释放启动期建哨兵用的命名锁,用独立超时。
// 放锁失败只记日志,与 schemamigrate 的迁移锁同一口径:成功路径上行已建好,别的实例走普通读快路径,不再需要这把锁;
// 失败路径上调用方拒启退出,连接随之断开,服务端会自动释放。
func releaseInsertGuardInitLock(conn *sql.Conn) {
	releaseCtx, cancel := context.WithTimeout(context.Background(), insertGuardInitReleaseTimeout)
	defer cancel()
	var released sql.NullInt64
	if err := conn.QueryRowContext(releaseCtx, "SELECT RELEASE_LOCK(?)", insertGuardInitLockName).Scan(&released); err != nil {
		logx.Errorf("[guild] release lock %s failed (the server releases it when the connection closes): %v", insertGuardInitLockName, err)
	}
}

// missingPlayerStateRows 普通读(不加锁)ids 里哪些状态行还不存在,按入参顺序返回缺的那些。
// ids 须已升序去重;IN 列表按 100 分块。q 是 *sql.DB(事务外)或 *sql.Tx(守卫下复读)。
func missingPlayerStateRows(ctx context.Context, q queryer, ids []uint64) ([]uint64, error) {
	existing := make(map[uint64]struct{}, len(ids))
	const chunkSize = 100
	for start := 0; start < len(ids); start += chunkSize {
		batch := ids[start:min(start+chunkSize, len(ids))]
		found, err := readIDColumn(ctx, q,
			sqlSelectExistingPlayerStatesHead+placeholders(len(batch))+sqlSelectExistingPlayerStatesTail, uint64Args(batch)...)
		if err != nil {
			return nil, fmt.Errorf("read guild player state rows of %d players: %w", len(batch), err)
		}
		for _, id := range found {
			existing[id] = struct{}{}
		}
	}
	var missing []uint64
	for _, id := range ids {
		if _, ok := existing[id]; !ok {
			missing = append(missing, id)
		}
	}
	return missing, nil
}

// ensurePlayerStateRows 保证这些玩家的状态行存在,是 lockPlayerState 的前置
// (01-storage.md §2.1 规则 3:对不存在的行做加锁读再插入,RR 下靠间隙锁互等、TiDB 下锁不住后续插入)。
// **调用时本 goroutine 不得持有任何事务**:它总在调用方的业务事务之前调用(建帮、审批通过、申请、踢人、退帮、解散),
// 自己开的短事务要拿全局插入守卫,嵌在别的事务里就是"持业务行锁等全局锁",锁序推演不再成立。
//
// 两步:
//  1. 普通读挑出已存在的行(行建好后永不删除,常态下全都已存在,直接返回)。为什么不对全部 id 直接 INSERT IGNORE:
//     撞上已存在的主键会对它取 S 锁(TiDB 取 X),而状态行正是写事务里 FOR UPDATE 的那把串行化锁 —— 解散会把全体
//     成员的状态行锁到提交,这期间这些人的申请 / 退帮 / 被踢在"建行"这一步就会白等。普通读不碰锁。
//  2. 有缺的:一个 RC 短事务(经 inTx,op 用调用方的)里 lockGlobalInsertGuard → **守卫下再普通读一次** →
//     只对仍缺的按 player_id 升序 INSERT IGNORE → 提交。
//     守卫下复读的理由:别的首建者也都在这把守卫下建行、持到提交,拿到守卫时它们的行都已提交、RC 普通读看得见;
//     跳过它们,INSERT 就只落在真正不存在的主键上,不会撞上别人的记录去排 S 锁 —— 否则持着全局守卫去等某个
//     被解散事务锁住的状态行,会把全服的建帮 / 审批通过一起拖住。
//
// 为什么在守卫下建行(G-C3):旧写法是事务外逐条自动提交 INSERT IGNORE。首个插入者在提交前被回滚(连接被 KILL、
// 刷盘失败、实例关闭)时,排在它未提交记录上的两个同键插入者,其 S 锁在 RC 下被继承为后继记录上的间隙 S,
// 插入意向互相挡住,InnoDB 牺牲其一(1213)。现在首次建行者都排在**已存在、从不删除**的守卫行上,而不是排在
// 别人未提交的新记录上:同一时刻至多一个建行者碰到缺行的主键,首插者回滚后留下的间隙锁只属于它自己(或已随回滚释放),
// 不再有第二个排队者与之互等。
//
// 错误语义:player_id 0 是守卫本身,不是玩家 → 内部错误(不碰库);哨兵行缺失 → errGlobalInsertGuardMissing(内部错误);
// 1213 / 9007 重试耗尽、1205、超子预算 → ErrWriteConflict(inTx 同口径,logic 回 kGuildBusyRetry);其余包一层原样返回。
func (r *GuildRepo) ensurePlayerStateRows(ctx context.Context, op string, now uint64, playerIDs ...uint64) error {
	ids := slices.Clone(playerIDs)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) > 0 && ids[0] == globalInsertGuardPlayerID {
		return fmt.Errorf("ensure guild player state rows: player_id %d is the global insert guard, not a player", globalInsertGuardPlayerID)
	}

	missing, err := missingPlayerStateRows(ctx, r.db, ids)
	if err != nil {
		return err
	}
	if len(missing) == 0 {
		return nil
	}
	return r.inTx(ctx, op, func(ctx context.Context, tx *sql.Tx) error {
		if err := lockGlobalInsertGuard(ctx, tx); err != nil {
			return err
		}
		stillMissing, err := missingPlayerStateRows(ctx, tx, missing)
		if err != nil {
			return err
		}
		for _, playerID := range stillMissing { // missing 继承 ids 的升序:同表按主键升序插
			if _, err := tx.ExecContext(ctx, sqlEnsurePlayerState, playerID, now); err != nil {
				return fmt.Errorf("ensure guild player state row %d: %w", playerID, err)
			}
		}
		return nil
	})
}

// readIDColumn 跑一条只返回一列无符号整数的**普通读**,全部读完才返回(游标随之关闭:同一个 *sql.Tx 只有一条连接,
// 游标没关就发下一条语句会被驱动拒绝)。q 可以是 *sql.DB(事务外)或 *sql.Tx(事务内)。
func readIDColumn(ctx context.Context, q queryer, query string, args ...any) ([]uint64, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []uint64
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

// uint64Args 把 id 列表转成 database/sql 的可变参数。
func uint64Args(ids []uint64) []any {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args
}

// lockMemberRole 按完整主键点锁一行成员(锁序位置 3,sqlLockMemberRole 带 FORCE INDEX (PRIMARY)),返回权威 role;
// 行不存在返回 found=false(RC 下未命中不加锁)。
func lockMemberRole(ctx context.Context, tx *sql.Tx, guildID, playerID uint64) (role uint32, found bool, err error) {
	err = tx.QueryRowContext(ctx, sqlLockMemberRole, guildID, playerID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("lock member %d of guild %d: %w", playerID, guildID, err)
	}
	return role, true, nil
}

// lockMemberPair 锁住操作者与目标的成员行,返回两者的权威 role。
//
// 按 player_id 升序**两次**完整主键点锁(文件头 (a)(b))。旧写法 `player_id IN (?, ?) ORDER BY player_id FOR UPDATE`
// 的锁集依赖执行计划:走 PRIMARY 是升序 range,走 uk_guild_member 就是"先二级后主键",与按主键删成员行反序
// (事故报告 §7.2 已登记);两次点锁的取锁顺序由这里的代码决定,与计划无关。
// 两行都锁完才判存在性,错误优先级与旧写法一致:操作者不在 → ErrNotGuildMember,其次目标不在 → ErrTargetNotMember。
//
// 前置:actorID != targetID(调用方已断言)。万一传入相同 id,沿用旧写法的结论 —— 旧的 IN (?, ?) 只返回一行、
// 只记给操作者,于是"操作者在 → ErrTargetNotMember、不在 → ErrNotGuildMember";这里只锁一次,给出同样的答复。
func lockMemberPair(ctx context.Context, tx *sql.Tx, guildID, actorID, targetID uint64) (actorRole, targetRole uint32, err error) {
	if actorID == targetID {
		_, found, lockErr := lockMemberRole(ctx, tx, guildID, actorID)
		switch {
		case lockErr != nil:
			return 0, 0, lockErr
		case !found:
			return 0, 0, ErrNotGuildMember
		default:
			return 0, 0, ErrTargetNotMember
		}
	}

	low, high := min(actorID, targetID), max(actorID, targetID)
	lowRole, lowFound, err := lockMemberRole(ctx, tx, guildID, low)
	if err != nil {
		return 0, 0, err
	}
	highRole, highFound, err := lockMemberRole(ctx, tx, guildID, high)
	if err != nil {
		return 0, 0, err
	}

	var actorFound, targetFound bool
	if actorID == low {
		actorRole, actorFound, targetRole, targetFound = lowRole, lowFound, highRole, highFound
	} else {
		actorRole, actorFound, targetRole, targetFound = highRole, highFound, lowRole, lowFound
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

// ── 申请行的"候选普通读 + 主键点删"(friend 审计 #2 / #3) ────────────
//
// 旧写法是 `DELETE … WHERE player_id = ?` / `player_id IN (…)` / `guild_id = ?`(前缀范围)与
// `SELECT … WHERE player_id = ? FOR UPDATE`。按 player_id 的那几条只能经 idx_guild_application_0 取锁 ——
// "先二级后主键",与审批拒绝 / 过期 / 撤回"先按主键锁行、再删二级项"在同一行上反序成环(审计 #2,
// 典型:同一申请人被两帮同时审批,一通过一拒绝);IN 列表与前缀范围还可能被规划成全扫,锁到不相干的行。
// 现在一律:普通读取候选主键(不加锁)→ 按 (guild_id, player_id) 升序逐行 sqlDeleteApplication。
// 全部写者都是"先主键后二级",锁集与执行计划无关。

// applicationKey 是 guild_application 的完整主键。
type applicationKey struct{ guildID, playerID uint64 }

// compareApplicationKeys:主键序 (guild_id, player_id)。
func compareApplicationKeys(a, b applicationKey) int {
	if c := cmp.Compare(a.guildID, b.guildID); c != 0 {
		return c
	}
	return cmp.Compare(a.playerID, b.playerID)
}

// applicationKeysOfPlayers 普通读这些玩家名下全部申请的主键(I2 / I3 的删除候选),IN 按 100 分块。
//
// 候选集为什么完整:调用方都已持有这些玩家的 guild_player_state 行锁(建帮、审批通过、踢人、退帮、解散),
// 而能给他们插申请行的只有本人的 ApplyToGuild —— 它必须先拿同一把锁;并发的撤回 / 拒绝只会让集合变小
// (点删影响 0 行即跳过)。RC 每条语句新快照,看得见加锁之前已提交的全部行。
func applicationKeysOfPlayers(ctx context.Context, tx *sql.Tx, playerIDs []uint64) ([]applicationKey, error) {
	const chunkSize = 100
	var keys []applicationKey
	for start := 0; start < len(playerIDs); start += chunkSize {
		batch := playerIDs[start:min(start+chunkSize, len(playerIDs))]
		found, err := readApplicationKeys(ctx, tx,
			sqlSelectApplicationKeysOfPlayersHead+placeholders(len(batch))+sqlSelectApplicationKeysOfPlayersTail,
			uint64Args(batch)...)
		if err != nil {
			return nil, fmt.Errorf("read applications of %d players: %w", len(batch), err)
		}
		keys = append(keys, found...)
	}
	return keys, nil
}

// readApplicationKeys 跑一条返回 (guild_id, player_id) 两列的普通读,读完即关游标。
func readApplicationKeys(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]applicationKey, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []applicationKey
	for rows.Next() {
		var key applicationKey
		if err := rows.Scan(&key.guildID, &key.playerID); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return keys, nil
}

// applicationKeysOfGuild 普通读本帮全部申请的主键(解散用)。
// 候选集完整:给本帮插申请的 ApplyToGuild 先锁本帮 guild 行,解散持有它直到提交。
func applicationKeysOfGuild(ctx context.Context, tx *sql.Tx, guildID uint64) ([]applicationKey, error) {
	players, err := readIDColumn(ctx, tx, sqlSelectApplicantsOfGuild, guildID)
	if err != nil {
		return nil, fmt.Errorf("read applications of guild %d: %w", guildID, err)
	}
	keys := make([]applicationKey, len(players))
	for i, playerID := range players {
		keys[i] = applicationKey{guildID: guildID, playerID: playerID}
	}
	return keys, nil
}

// deleteApplicationRows 按主键 (guild_id, player_id) 升序逐行点删(文件头 (a)(b))。
// keys 可乱序、可重复:原地排序去重,调用方之后别再用这个切片。
// 影响 0 行 = 候选读之后已被并发的撤回 / 拒绝 / 另一侧的清理删掉,跳过,不是错误。
//
// 解散把"本帮的申请"与"成员们在别帮的申请"合成一个列表整体排序,而不是分两段删:两个解散事务会交叉删对方的行
// (解散 G1 删 (G1, m2),解散 G2 删 G2 成员 m2 名下的 (G1, m2);反之亦然),只有全局同一顺序才不会互等。
func deleteApplicationRows(ctx context.Context, tx *sql.Tx, keys []applicationKey) error {
	slices.SortFunc(keys, compareApplicationKeys)
	keys = slices.Compact(keys)
	for _, key := range keys {
		if _, err := tx.ExecContext(ctx, sqlDeleteApplication, key.guildID, key.playerID); err != nil {
			return fmt.Errorf("delete application (%d,%d): %w", key.guildID, key.playerID, err)
		}
	}
	return nil
}

// deleteApplicationsOfPlayer 是单个玩家的 I2 / I3:普通读他名下全部申请,再按主键升序逐行点删。
// 前置:调用方已持有该玩家的 guild_player_state 行锁(候选集完整性的前提,见 applicationKeysOfPlayers)。
// alsoHeld 是本事务已经 FOR UPDATE 持有的行(审批通过的那一条):一并放进删除列表 —— 再删一次不取新锁,
// 也保证它即使不在候选读里也一定被删。
func deleteApplicationsOfPlayer(ctx context.Context, tx *sql.Tx, playerID uint64, alsoHeld ...applicationKey) error {
	keys, err := applicationKeysOfPlayers(ctx, tx, []uint64{playerID})
	if err != nil {
		return err
	}
	return deleteApplicationRows(ctx, tx, append(keys, alsoHeld...))
}

// ── 成员管理事务(§7) ───────────────────────────────────────

// SetMemberRole 任免长老(只能设成员 / 长老两档)。
//
// 输入约束:actorID != targetID;constants.AssignableRole(role);officerCap != nil。
// 三者由 logic 先校验,这里再断言一次 —— repo 是可被 B5/B6 直接复用的接缝,不能依赖"上游一定校验过"。
// 不变量:role=3(帮主)只能经 TransferLeader 产生;长老数不超过 GuildLevel[level].max_officers。
// 事务边界:单事务 op=set_role,锁序 guild → guild_member(操作者与目标按 player_id 升序两次主键点锁,见 lockMemberPair)。
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
// 输入约束:actorID != targetID;now 为服务进程时钟毫秒(> 0,由 logic 传入)。
// 不变量:职位必须严格高于对方(canKick);被踢者的帮贡随成员行一起消失;
// 被踢者在本帮的未决捐献截止时间提前到 now(05 §5.20),让还没扣款的指令尽快中止、次数退回。
// 事务边界:事务外先普通读目标是不是本帮成员(不是就直接答复,G4),是才建被踢者的状态行;单事务 op=kick,
// 锁序 guild → guild_player_state(被踢者)→ guild_member(操作者与目标,主键升序逐行)→
// guild_application(I3,主键升序逐行)→ guild_asset_op。
// 状态行锁(文件头 (c)):删成员行的事务必须与"别帮审批通过同一人"串行,否则删成员行留下的 uk 项与删申请
// 会和对方的插成员 / 锁申请在 uk_guild_member(p) 与 guild_application(G2,p) 上成环。
// 错误语义:同 SetMemberRole 的哨兵集合(不含 ErrOfficerLimit);建状态行遇锁冲突归一成 ErrWriteConflict。
// 幂等:目标已经不在帮里 → ErrTargetNotMember(客户端据此刷新列表)。
func (r *GuildRepo) KickMember(ctx context.Context, guildID, actorID, targetID, now uint64) (MemberWriteResult, error) {
	if actorID == targetID {
		return MemberWriteResult{}, fmt.Errorf("kick member of guild %d: actor and target must differ", guildID)
	}
	// 完整性复核 G4:目标先普通读是不是本帮成员,不是就直接答复,不建状态行、不开事务。否则任何帮会成员拿随机 id
	// 调踢人,每次都会为一个不存在的玩家取一次全局插入守卫 S(0)(建状态行的短事务)并留下一行永不删除的垃圾状态行。
	// 预读只决定"要不要建行",授权与成员判定仍以锁内为准;预读说"不在"就按那一刻的已提交状态答复(等价于本次踢人
	// 排在对方入帮之前),答复的优先级与锁内一致(见 precheckKickTarget)。
	if err := r.precheckKickTarget(ctx, guildID, actorID, targetID); err != nil {
		return MemberWriteResult{}, err
	}
	// 事务外建行(规则 3):目标此刻是本帮成员,状态行缺失(老成员从没进过需要守卫的路径)时在这里补上。
	if err := r.ensurePlayerStateRows(ctx, opKick, now, targetID); err != nil {
		return MemberWriteResult{}, err
	}

	var out MemberWriteResult
	err := r.inTx(ctx, opKick, func(ctx context.Context, tx *sql.Tx) error {
		var res MemberWriteResult

		if _, err := lockGuildRow(ctx, tx, guildID); err != nil {
			return err
		}
		if err := lockPlayerState(ctx, tx, targetID); err != nil {
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
		// 这些行会在他离帮后按 I1 重新"复活"。持有他的状态行,候选集是完整的。
		if err := deleteApplicationsOfPlayer(ctx, tx, targetID); err != nil {
			return fmt.Errorf("delete applications of kicked member %d: %w", targetID, err)
		}
		// 与删成员行放在同一事务:两者要么都生效、要么都不生效。拆成提交后再补一条的话,
		// 中间崩溃会留下"人已离帮、捐献仍按原截止等满 asset_op_deadline_seconds"的行。
		if err := accelerateDonationDeadlines(ctx, tx, guildID, []uint64{targetID}, now); err != nil {
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
	r.invalidateAfterCommit(ctx, opKick, guildID, targetID)
	return out, nil
}

// precheckKickTarget(G4)普通读(不加锁)踢人目标是否本帮成员:是 → nil(照常建行、进事务);
// 不是 → 按锁内 lockGuildRow → lockMemberPair 的同一优先级答复:帮会不在 ErrGuildGone、操作者不在 ErrNotGuildMember、
// 否则 ErrTargetNotMember。logic 对前两者会顺手自愈陈旧映射,所以优先级不能丢。
func (r *GuildRepo) precheckKickTarget(ctx context.Context, guildID, actorID, targetID uint64) error {
	_, targetFound, err := r.MemberRole(ctx, guildID, targetID)
	if err != nil || targetFound {
		return err
	}
	if _, err := r.readOperatorRole(ctx, guildID, actorID); err != nil {
		return err
	}
	return ErrTargetNotMember
}

// precheckApprovedApplication(G4)普通读(不加锁)要批准的申请行在不在:在(含已过期,过期由锁内分支删行)→ nil;
// 不在 → 不建状态行、不取全局插入守卫,按锁内判定的同一优先级答复:帮会不在 ErrGuildGone、审批人不在 ErrNotGuildMember、
// 职位不够 ErrRankTooLow,否则 ErrApplicationNotFound。预读说"不在"就按那一刻的已提交状态答复(申请已被处理 / 撤回)。
func (r *GuildRepo) precheckApprovedApplication(ctx context.Context, guildID, actorID, applicantID uint64) error {
	var exists int
	err := r.db.QueryRowContext(ctx, sqlSelectApplicationExists, guildID, applicantID).Scan(&exists)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read application (%d,%d): %w", guildID, applicantID, err)
	}
	role, err := r.readOperatorRole(ctx, guildID, actorID)
	if err != nil {
		return err
	}
	if !canReviewApplications(role) {
		return ErrRankTooLow
	}
	return ErrApplicationNotFound
}

// readOperatorRole 普通读(不加锁)操作者在本帮的 role:帮会不在 → ErrGuildGone,操作者不在 → ErrNotGuildMember。
// 只用于"操作对象不存在"时组织错误答复(G4 的两个预读),**不**用于授权 —— 写路径的授权必须在事务里对锁住的行复核。
func (r *GuildRepo) readOperatorRole(ctx context.Context, guildID, actorID uint64) (uint32, error) {
	if _, err := r.authoritativeZoneID(ctx, guildID); err != nil {
		return 0, fmt.Errorf("read guild %d: %w", guildID, err)
	}
	role, found, err := r.MemberRole(ctx, guildID, actorID)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, ErrNotGuildMember
	}
	return role, nil
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
		// 改 leader_id 会 delete-mark / 插入 idx_guild_1 的项(同一 guild 行的二级项,主键 → 二级):域内没有语句经
		// idx_guild_1 加锁,那条二级项不会有别的持锁者,不引入新的等待边。
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
// 输入约束:now 为服务进程时钟毫秒(> 0,由 logic 传入)。
// 不变量:帮主不能退帮(必须先转让或解散),否则帮会会变成无主状态;
// 退帮者在本帮的未决捐献截止时间提前到 now(05 §5.20)。
// 事务边界:事务外先建本人的状态行;单事务 op=leave,锁序 guild → guild_player_state(本人)→ guild_member →
// guild_application(I3,主键升序逐行)→ guild_asset_op。状态行锁的理由同 KickMember(文件头 (c)):
// 修复前"退帮删成员 + 删申请"与"别帮审批通过同一人"会在 uk_guild_member(p) 与 guild_application(G2,p) 上成环。
// 错误语义:ErrGuildGone / ErrNotGuildMember / ErrLeaderCantLeave / ErrWriteConflict。
// 幂等:重放得到 ErrNotGuildMember,logic 复核映射为 0 后按成功处理。
func (r *GuildRepo) LeaveGuild(ctx context.Context, guildID, playerID, now uint64) (MemberWriteResult, error) {
	if err := r.ensurePlayerStateRows(ctx, opLeave, now, playerID); err != nil {
		return MemberWriteResult{}, err
	}

	var out MemberWriteResult
	err := r.inTx(ctx, opLeave, func(ctx context.Context, tx *sql.Tx) error {
		var res MemberWriteResult

		guild, err := lockGuildRow(ctx, tx, guildID)
		if err != nil {
			return err
		}
		if err := lockPlayerState(ctx, tx, playerID); err != nil {
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
		if err := deleteApplicationsOfPlayer(ctx, tx, playerID); err != nil { // I3
			return fmt.Errorf("delete applications of leaving member %d: %w", playerID, err)
		}
		if err := accelerateDonationDeadlines(ctx, tx, guildID, []uint64{playerID}, now); err != nil {
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
// 事务内碰到的申请行**全部属于本人**((*, playerID),都在 guild_player_state(p) 的守卫域里):
//   - 本人的过期行:普通读候选 → 主键升序逐行点删(带 expire_ms <= now 复核);
//   - "是否已申请过本帮":对 (guildID, playerID) 做完整主键 FOR UPDATE(const);
//   - 每人上限:普通读计数(见下方论证)。
//
// 本帮其他人的过期行**移到提交之后**,由 purgeExpiredApplicationsOfGuild 用逐行 RC 短事务的主键点删尽力清理
// (friend 审计 #3,二选一选了"移出"):那是纯卫生动作 —— 帮会上限判定的 sqlCountLiveApplicationsOfGuild
// 本来就只数未过期行 —— 留在事务里,锁集就越出了本人的守卫域(别人的申请行,别的事务可以不经任何守卫去删),
// 持有 guild 行与本人状态行期间还要多等别人;移出后本事务的申请行锁集只剩本人名下,判环只需考虑这一个守卫。
// 错误语义:ErrGuildGone / ErrGuildZoneMismatch / ErrPlayerAlreadyInGuild / ErrGuildFull /
// ErrApplicationLimit / ErrApplicationQueueFull / ErrWriteConflict。
// 幂等:重复申请同一帮 → Inserted=false(只刷新 apply_ms/expire_ms),不推送。
func (r *GuildRepo) ApplyToGuild(ctx context.Context, guildID, playerID uint64, requiredZone uint32, now uint64, rules ApplicationRules) (ApplyResult, error) {
	if rules.TTLMs == 0 || rules.MaxPerPlayer == 0 || rules.MaxPerGuild == 0 {
		return ApplyResult{}, fmt.Errorf("apply to guild %d: application rules not configured (%+v)", guildID, rules)
	}
	// 事务外建行:对不存在的行做加锁读再插入会互等成死锁(01-storage.md §2.1 规则 3)。
	if err := r.ensurePlayerStateRows(ctx, opApply, now, playerID); err != nil {
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

		if err := purgeExpiredApplicationsOfPlayer(ctx, tx, playerID, now); err != nil {
			return err
		}

		// "是否已申请过本帮":完整主键 FOR UPDATE(const,只锁这一行的聚簇记录)。锁住它是为了挡住并发的撤回 ——
		// 撤回不经任何守卫,读完不锁的话,刷新会落在一条刚被删掉的行上、却回"刷新成功"。
		// 查不到时 RC 下不加锁,随后插入的唯一性仍由主键兜底(见插入处 IODKU 的 RowsAffected 自检),
		// 串行化由 guild_player_state(p) 保证(规则 3 的本意:唯一性不靠"锁住不存在的行")。
		// 本人的过期行上一步已删,查得到的就是未过期行。
		existing, err := lockOwnApplication(ctx, tx, guildID, playerID)
		if err != nil {
			return err
		}

		if existing {
			// 刷新排在满员与两个上限判定**之前**:这一行本来就已经计入所有计数,
			// 契约也规定"同帮重复申请 = 刷新并成功",满员帮只拒绝**新**申请人。
			// 这条 UPDATE 是**幂等**的:玩家双击时两次申请可能落在同一毫秒,
			// apply_ms / expire_ms 与库里现值逐字相同 ⇒ RowsAffected = 0。
			// 那仍然是"刷新成功"(行就在那儿,值就是要写的值),所以这里不能断言恰好一行 ——
			// 断言会把一次正常双击变成内部错误。行的存在性已由上面的 FOR UPDATE 证明。
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
		// 每人上限用**普通读**计数(01-storage.md §2.1 规则 4 的"判定用加锁读"在这里由守卫取代):
		// 能让本人未过期申请数变大的只有本人的 ApplyToGuild,而它必须先持有本事务正持有的 guild_player_state(p);
		// 撤回 / 拒绝 / 过期 / 解散只会让它变小。RC 每条语句新快照,看得见加锁之前已提交的全部插入,
		// 所以计数不会漏数;只可能因为并发删除而偏大 —— 那是多拒一次,不会超发。
		// 旧写法 `SELECT … WHERE player_id = ? FOR UPDATE` 经 idx_guild_application_0 取锁,是审计 #2 的成环点之一。
		var pending uint32
		if err := tx.QueryRowContext(ctx, sqlCountPendingApplicationsOfPlayer, playerID, now).Scan(&pending); err != nil {
			return fmt.Errorf("count pending applications of player %d: %w", playerID, err)
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

		// 插入写成 IODKU(`ON DUPLICATE KEY UPDATE apply_ms = apply_ms`,更新部分是空操作),不是为了"撞了就更新":
		// (guildID, playerID) 可能是刚被撤回删掉、还没 purge 的删除标记记录。普通 INSERT 撞上它时先取 S、改写记录时
		// 再要 X;这之间若本人的另一条撤回请求(自动提交,不经任何守卫)已在这条记录上排队等 X,升级就排在它后面,
		// 两边互等成环(取锁规则 H2,friend 审计 #1 的同一机制)。IODKU 查重时直接取 X,没有升级。
		// 撞上**活**记录则走空更新、RowsAffected = 0(ClientFoundRows=false 由 WithLockWaitTimeout 强制):
		// 上面在 guild_player_state 锁下 FOR UPDATE 没读到它,说明有人绕过了串行化(或状态行被手工删掉)——
		// 不猜、不吞:回可重试的忙错误并整体回滚,玩家原地重试会命中上面的刷新分支。与原先 1062 分支的结局相同。
		result, err := tx.ExecContext(ctx, sqlInsertApplication, guildID, playerID, now, now+rules.TTLMs)
		if err != nil {
			return fmt.Errorf("insert application of player %d to guild %d: %w", playerID, guildID, err)
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("insert application of player %d to guild %d: read rows affected: %w", playerID, guildID, err)
		}
		if inserted != 1 {
			logx.Errorf("[guild] apply: live application (%d,%d) despite player state lock (rows affected %d)",
				guildID, playerID, inserted)
			return ErrWriteConflict
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
	// 提交之后尽力清理本帮的过期申请(见函数头:移出事务的理由)。失败只记日志,不影响本次结果。
	r.purgeExpiredApplicationsOfGuild(ctx, guildID, now)
	// 不失效任何缓存:pending_application_count 不进缓存,由请求者每次现算(§12.4)。
	return out, nil
}

// purgeExpiredApplicationsOfPlayer 在申请事务内删掉本人的过期申请:普通读候选 → 按 guild_id 升序逐行
// "sqlSelectApplicationExpire 主键点锁 → sqlDeleteExpiredApplication 点删",`expire_ms <= now` 是提交点复核。
// 前置:已持有本人的 guild_player_state 行锁(本人名下的行只有本人的申请会插、会刷新)。
// 旧写法 `DELETE … WHERE player_id = ? AND expire_ms <= ?` 经 idx_guild_application_0 / _1 取锁,"先二级后主键",
// 与审批过期分支"先按主键锁行、再删"在同一行上反序(审计 #2 (d))。
//
// 为什么点删前还要点锁(死锁复核 V2,只在 TiDB 成环):带复核谓词的 DELETE 不走 Point_Get 快路径,语句末尾并行锁
// {行 key, PRIMARY key};它又不是本事务在这一行上的第一把锁,而审批(G′,p) 的拒绝 / 过期分支、撤回、别人申请后的清理短事务
// 都以 Point_Get(先 PRIMARY key 后行 key)锁同一行 —— 双方可能各持一半互等(1213)。先点锁,大家都从 PRIMARY key 进入、
// 排成一列;随后点删那一批 key 已全部在手。MySQL 下点锁与点删锁的是同一条聚簇记录,锁集不变。
// 点锁读不到行 = 候选读之后已被拒绝 / 过期分支 / 撤回 / 清理删掉,跳过。
func purgeExpiredApplicationsOfPlayer(ctx context.Context, tx *sql.Tx, playerID, now uint64) error {
	guildIDs, err := readIDColumn(ctx, tx, sqlSelectExpiredApplicationsOfPlayer, playerID, now)
	if err != nil {
		return fmt.Errorf("read expired applications of player %d: %w", playerID, err)
	}
	for _, guildID := range guildIDs { // 查询已 ORDER BY guild_id,即主键升序
		found, err := lockRowExists(ctx, tx, sqlSelectApplicationExpire, guildID, playerID)
		if err != nil {
			return fmt.Errorf("lock expired application (%d,%d): %w", guildID, playerID, err)
		}
		if !found {
			continue
		}
		if _, err := tx.ExecContext(ctx, sqlDeleteExpiredApplication, guildID, playerID, now); err != nil {
			return fmt.Errorf("purge expired application (%d,%d): %w", guildID, playerID, err)
		}
	}
	return nil
}

// lockOwnApplication 对 (guildID, playerID) 做完整主键 FOR UPDATE,返回行是否存在。
func lockOwnApplication(ctx context.Context, tx *sql.Tx, guildID, playerID uint64) (bool, error) {
	var expireMs uint64
	err := tx.QueryRowContext(ctx, sqlSelectApplicationExpire, guildID, playerID).Scan(&expireMs)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock application (%d,%d): %w", guildID, playerID, err)
	}
	return true, nil
}

// purgeExpiredApplicationsPerApply:申请提交后顺带清理本帮过期申请的每次上限。
//
// 这是纯卫生动作,正确性不靠它:帮会队列上限的计数(sqlCountLiveApplicationsOfGuild)、列表(sqlListApplicants)
// 本来就按 expire_ms > now 过滤,过期行留在表里只占空间。它跑在**请求路径**上(提交之后、回包之前),每行一个 RC 短事务,
// 行数直接叠进回包耗时;每次申请顺带清一点,冷门帮攒下的积压会被之后的申请逐次清掉,不必一次清完拖长这一次回包。
const purgeExpiredApplicationsPerApply = 10

// purgeExpiredApplicationsOfGuild 在申请事务**提交之后**尽力清理本帮的过期申请(每次至多 purgeExpiredApplicationsPerApply 行)。
//
// 做法:普通读候选(不加锁)→ 按 player_id 升序**每行一个 RC 短事务**做主键点删,`expire_ms <= now` 复核
// (候选读之后被本人刷新续期的行不删)。每个事务只在一行上取锁:聚簇记录,然后是它自己的二级项 ——
// 修复后没有任何语句经 guild_application 的二级索引加锁,二级项不会有别的持锁者,所以它至多排队、不可能成环(取锁规则 R15)。
// 旧写法 `DELETE … WHERE guild_id = ? AND expire_ms <= ? LIMIT 100` 可能被规划到 idx_guild_application_1 的
// expire_ms 范围上,锁到别帮的过期行,与那一帮审批过期分支的"主键 → 二级"反序(审计 #3)。
//
// 为什么每行一个事务而不是自动提交(G-C2 管理域同形,文件头 (e)):删一行要动行、PRIMARY、idx_0、idx_1 四个 key,
// 同一行也会被审批的拒绝 / 过期分支、退帮 / 被踢 / 解散的申请点删、本人申请的刷新(改 expire_ms)以悲观事务改动;
// TiDB 下自动提交按乐观提交,可能先锁住 idx 项再在行 key 上撞到对方,对方提交时又要这条 idx 项 —— 只能等 TTL 打破。
// MySQL 下锁集与顺序不变。为什么不是整批一个事务:一行一事务,持锁面始终只有一行,不必再论证多行之间的先后。
// 事务首句先 sqlSelectApplicationExpire 主键点锁、再做带 `expire_ms <= now` 复核的点删(死锁复核 V2):带复核谓词的 DELETE
// 在 TiDB 上语句末尾并行锁 {行 key, PRIMARY key},不先点锁时只能靠"事务首批先锁主键、主键恰好是 PRIMARY key"这一
// client-go 实现细节才不与对方的 Point_Get 各持一半;点锁之后顺序由我们自己的语句决定。读不到行 = 已被别人删掉,什么都不做。
//
// 纯卫生动作:帮会队列上限的计数只数未过期行,清不清都不影响判定。所以失败只记 Info、不回错误,剩下的行
// 留给下一次申请;死锁 / 锁等待由 inTx 按 op=apply 计指标并有界重跑(按取锁表它们不该出现)。
func (r *GuildRepo) purgeExpiredApplicationsOfGuild(ctx context.Context, guildID, now uint64) {
	playerIDs, err := readIDColumn(ctx, r.db, sqlSelectExpiredApplicantsOfGuild, guildID, now, purgeExpiredApplicationsPerApply)
	if err != nil {
		logx.Infof("[guild] purge expired applications of guild %d: read candidates: %v", guildID, err)
		return
	}
	for _, playerID := range playerIDs {
		err := r.inTx(ctx, opApply, func(ctx context.Context, tx *sql.Tx) error {
			found, err := lockRowExists(ctx, tx, sqlSelectApplicationExpire, guildID, playerID)
			if err != nil {
				return fmt.Errorf("lock: %w", err)
			}
			if !found {
				return nil
			}
			_, err = tx.ExecContext(ctx, sqlDeleteExpiredApplication, guildID, playerID, now)
			return err
		})
		if err != nil {
			logx.Infof("[guild] purge expired applications of guild %d: delete expired application (%d,%d): %v",
				guildID, guildID, playerID, err)
			return
		}
	}
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
// 一个 RC 短事务(op=cancel),只碰 (guildID, playerID) 这一行:先 sqlSelectApplicationExpire 完整主键 FOR UPDATE 点锁,
// 再做带复核条件的主键点删(先聚簇记录、后它自己的二级项),与审批 / 申请 / 解散的点删同向;不持有守卫也无妨 ——
// 持锁面始终只有这一行,至多排队、不可能成环。计划由 EXPLAIN 回归钉住。
// 为什么不再是两条自动提交(G-C2 管理域同形,文件头 (e)):删一行要动行、PRIMARY、idx_0、idx_1 四个 key,同一行也会被
// 审批的拒绝 / 过期分支(先 FOR UPDATE 再删)与退帮 / 被踢 / 解散的申请点删以悲观事务改动;TiDB 下自动提交按乐观提交,
// 可能先锁住 idx 项、再在行 key 上撞到对方,而对方提交时要删的正是这条 idx 项 —— 只能等乐观锁 TTL 打破。
// 为什么点删前先点锁(死锁复核 V2):带 `expire_ms` 复核的 DELETE 不走 Point_Get 快路径,TiDB 下语句末尾并行锁
// {行 key, PRIMARY key};作为事务首句时"不部分持有"只靠 client-go 先锁主键这一实现细节。先点锁(PRIMARY key → 行 key),
// 与审批的 Point_Get 同序,随后两条点删要的 key 都已在手。
// MySQL 下锁集与顺序不变:点锁与点删锁的是同一条聚簇记录;点锁持有到提交,第二条点删不再重新排队。
// 语义:点锁读不到行 → ErrApplicationNotFound(已被拒绝 / 解散 / 别的撤回删掉,本事务什么都没写,回滚即可);
// 删掉一条**未过期**的行 → nil;行在但没删到(锁在手里,只可能是已过期)→ 带 `expire_ms <= now` 复核把这条过期行删掉并
// **提交**(errCommitThen),返回 ErrApplicationNotFound。幂等:重复撤回得到 ErrApplicationNotFound。
// 1205 / 1213 经 inTx 归一到 ErrWriteConflict。
func (r *GuildRepo) CancelApplication(ctx context.Context, guildID, playerID, now uint64) error {
	return r.inTx(ctx, opCancel, func(ctx context.Context, tx *sql.Tx) error {
		found, err := lockRowExists(ctx, tx, sqlSelectApplicationExpire, guildID, playerID)
		if err != nil {
			return fmt.Errorf("lock application (%d,%d): %w", guildID, playerID, err)
		}
		if !found {
			return ErrApplicationNotFound
		}
		result, err := tx.ExecContext(ctx, sqlCancelApplication, guildID, playerID, now)
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
		if _, err := tx.ExecContext(ctx, sqlDeleteExpiredApplication, guildID, playerID, now); err != nil {
			return fmt.Errorf("purge expired application (%d,%d): %w", guildID, playerID, err)
		}
		// 先提交这条过期行的删除、再回 NotFound:直接返回错误会让回滚把删除一起撤销。
		return errCommitThen{inner: ErrApplicationNotFound}
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
// 事务边界:单事务 op=review。实际取锁序列(p = 申请人):
//
//	guild(G) → [approve] guild_player_state(0)(全局插入守卫)→ guild_player_state(p) → guild_member(G, 审批人) →
//	guild_application(G,p) FOR UPDATE → [通过] 插 guild_member(G,p)(PK 新项 + uk(p) 新项;p 有未 purge 的删除标记项时,
//	查重另对 uk 上"p 之后第一条记录"加 S next-key)→ guild_application(p 名下其余行,主键升序点删)。
//
// 全局插入守卫 S(0)(文件头 (d)):通过分支是 uk_guild_member 的查重插入者,与建帮、别的审批通过在 S(0) 上串行,
// 两个查重插入者不会同时在 uk 上持有 S next-key(G-OUT1)。它排在 guild(G) 之后、S(p) 之前(表内 0 最小,P2)。
// 本事务在等 S(0) 时手里只有 guild(G);而任何锁已有 guild 行的事务都把 guild 行当第一把锁、等它时手里没有别的锁,
// 所以"持 S(0) 者 → … → 等 guild(G)"的链不存在;持 S(0) 之后本事务也不再等任何 guild 行。
// approve=false(拒绝)不取 S(0) 也不取 S(p)。approve=true 里的过期 / 跨区 / 1062 子分支是在锁住 (G,p) 之后才判出的,
// 判出之前已按通过分支的锁序拿了 S(0)、S(p):要让它们不取守卫,只能先普通读预判、锁内复核、复核翻转时整体重跑,
// 为罕见分支多一层重跑不值得(KISS);它们的锁集是通过分支前缀的子集,不引入新的等待边。
// 唯一的例外是"申请行根本不存在"(已被处理 / 撤回,或客户端拿着过期列表):事务外先普通读预判,不在就直接按锁内的
// 答复优先级回错,不建状态行、不取 S(0)(完整性复核 G4,见 precheckApprovedApplication);预读说在,仍以锁内为准。
//
// **表间全序 P1 的第二个登记例外**(第一个是建帮的新 guild 行):通过分支在锁住 guild_application(G,p) 之后才对
// guild_member 取新锁。不改序的理由:过期 / 跨区 / 1062 分支都要"删掉这条申请并提交"(errCommitThen),
// 先锁申请行、再判能不能批是这条事务的语义;把插成员提前,就得在还没确认申请存在时先插后撤,平白多出回滚面。
// 为什么这个例外不成环(逐个核对插成员新取的锁):
//   - PK (G,p) 与 uk(p) 的新项:能等它们的只有 p 的写者 —— 插 / 删 p 成员行的事务都先持有 guild_player_state(p)
//     (文件头 (c)),本事务正持有它;对 (G,p) 做锁定读的经济事务(捐献 / 兑换预留、兑换退帮贡)读不到未提交的新行,
//     在 RC 下只会排队等本事务提交,且它们不持有本事务要的任何锁(本事务之后只碰 p 名下的申请行)。
//     p 重回同一帮、PK (G,p) 还是未 purge 的删除标记记录时,插入按"先 S 后 X"改写它(取锁规则 H2):查重与改写在
//     同一个页闩内完成,锁定读者拿不到页闩就排不进这两步之间;能在页闩外长期持有这条记录锁的只有它的删除者,
//     而删除者已在本事务拿到 state(p) 之前提交。
//   - 查重对后继 uk(q)(另一名玩家 q)加的 S next-key:本事务可能要等 q 的删除者(退帮 / 被踢 / 解散,持有 uk(q) 的 X
//     直到提交)。那些事务持有的是 guild(Gq)、state(q)、q 的成员行,之后只碰 q 名下的申请行与 guild_asset_op,
//     从不等 guild_application(G,p)、state(p) 或 guild(G)(Gq == G 时它们一开始就排在本事务的 guild(G) 后面)。
//     反方向(q 的删除者等本事务的 S)同样是单向的,见 sqlDeleteMember 的注释。另一个查重插入者本来会与本事务在同一段
//     间隙上互等(G-OUT1),现由全局插入守卫 S(0) 串行化:它持 S(0) 到提交,本事务拿到 S(0) 时它的 S next-key 已随提交释放。
//   - TiDB:uk(p) key 的竞争者都经 state(p) 串行;没有 S 锁与间隙锁。S(0) 在 TiDB 下是一把普通的行 / PRIMARY key 悲观锁。
//
// 申请行一律按完整主键取锁:(guildID, applicant) 先 FOR UPDATE;通过分支清申请人名下其余申请(I2)时,
// 普通读候选后按主键升序逐行点删、已持有的那一行一并放进列表(不取新锁)。旧写法
// `DELETE … WHERE player_id = ?` 经 idx_guild_application_0 "先二级后主键",与别帮对同一人的拒绝分支
// "先主键后二级"在 (G2, applicant) 上成环(friend 审计 #2 (a),两帮同时审批同一人是常见玩法)。
// 拒绝 / 过期 / 跨区 / 1062 分支只动 (guildID, applicant) 这一行(后三者是 approve=true 的子分支,锁序见上)。
// 错误语义:ErrGuildGone / ErrNotGuildMember / ErrRankTooLow / ErrApplicationNotFound /
// ErrGuildFull(保留申请,回滚)/ ErrWriteConflict;通过分支遇哨兵行缺失 → errGlobalInsertGuardMissing(内部错误,fail-closed)。
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
		// G4:申请行不在就不建状态行、不取全局插入守卫(见 precheckApprovedApplication)。
		if err := r.precheckApprovedApplication(ctx, guildID, actorID, applicantID); err != nil {
			return ReviewResult{}, err
		}
		if err := r.ensurePlayerStateRows(ctx, opReview, now, applicantID); err != nil {
			return ReviewResult{}, err
		}
	}

	var out ReviewResult
	review := func(ctx context.Context, tx *sql.Tx) error {
		var res ReviewResult

		guild, err := lockGuildRow(ctx, tx, guildID)
		if err != nil {
			return err
		}
		// 只有通过分支会给申请人插成员行,所以只有它需要占申请人的串行化锁位(X-10),以及全局插入守卫(文件头 (d))。
		// 位置 2 必须在锁 guild_member 之前拿,否则同一玩家的"审批通过"与"申请"会逆序互等;
		// 表内 S(0) 在前、S(p) 在后(按主键升序,p > 0)。
		if approve {
			if err := lockGlobalInsertGuard(ctx, tx); err != nil {
				return err
			}
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
		// 72h 内的旧申请会按 I1 重新"复活"。(guildID, applicantID) 已在上面 FOR UPDATE 持有,复用它。
		if err := deleteApplicationsOfPlayer(ctx, tx, applicantID,
			applicationKey{guildID: guildID, playerID: applicantID}); err != nil {
			return fmt.Errorf("delete applications of approved player %d: %w", applicantID, err)
		}

		snapshot, err := txSnapshot(ctx, tx, guildID)
		if err != nil {
			return err
		}
		res.Guild, res.Approved = snapshot, true
		out = res
		return nil
	}

	if err := r.inTx(ctx, opReview, review); err != nil {
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
//
// 事务边界:事务外先对普通读出的成员逐个建状态行;单事务 op=disband,锁序
// guild → guild_player_state(全体成员,player_id 升序)→ guild_member(全体成员,主键升序逐行点锁,随即逐行删)→
// guild_application(本帮的 + 成员们在别帮的,合并后主键升序逐行点删)→ guild_asset_op(提前截止)→ guild(删行)。
//   - 删成员行紧跟在锁成员行之后、排在删申请之前(与踢人 / 退帮同一顺序;原先是"删申请 → 提前截止 → 删成员"):
//     删成员行要对 uk_guild_member 的项取 X,那是 guild_member 表上的新锁,放在申请表之后就违反表间全序(契约 P1)。
//     具体的环:审批通过 / 建帮给玩家 p 插成员行时,若 p 刚离过帮(uk 上留着删除标记项),查重会对"它后面的第一条 uk 项"
//     也加 S next-key(取锁规则 R12)—— 那可能正是本帮成员 q 的项;审批方随后要删 p 名下的申请,其中 (本帮, p)
//     已被本事务删掉并持有。本事务若此时才去删 q 的成员行,就要等审批方的 S,两边互等。先删成员再删申请,
//     本事务在 uk 上等的时候还没碰任何申请行,不成环。提前截止放在最后不影响它的前提:成员行删了,X 锁仍持有到提交,
//     捐献预留照样被挡在成员行锁外。
//   - 成员集合在持有 guild 行锁期间是稳定的:给本帮插 / 删成员行的事务(审批通过、踢人、退帮)都先锁 guild 行。
//     所以事务内在锁住 guild 行之后**重新普通读**一次成员 id,就是要删的全集;事务外那次只用来建状态行
//     (之后才入帮的人由审批通过自己建过行)。状态行事务内仍缺就 fail-closed(lockPlayerState 的既有语义)。
//   - 全体成员的状态行(文件头 (c)):删成员行的事务都要与"别帮审批通过同一人"串行,理由同 KickMember。
//   - 旧写法的前缀范围锁 / 删(`… WHERE guild_id = ? ORDER BY player_id FOR UPDATE`、`DELETE … WHERE guild_id = ?`、
//     `DELETE … WHERE player_id IN (…)`)锁集依赖执行计划,全扫时会锁到别帮的行;现在全部拆成完整主键点操作。
//   - 最后一条 DELETE guild 会对**同一行**的 uk_guild、idx_guild_0、idx_guild_1 项取 X(表序上排在后序表之后)。
//     域内没有任何语句经这三个二级索引加锁;唯一可能持有其中之一的是建帮查重对 uk_guild 后继项的 S next-key,
//     那一方插完 guild 行就只剩 COMMIT、不再等任何锁,所以本事务至多单向等待。merge_zone 经 idx_guild_0 整区改写
//     是另一条路径(friend 审计 #17),不在本服务。
//
// fence(friend 审计 #17 第 6 点):锁住 guild 行后用**行里的 zone_id** 调合服闸门,拒绝回 ErrZoneMerging。
// logic 在事务外按请求者归属区查过一次闸门,但那一步与合服置闸之间有检查到使用的窗口,内部调用路径更是完全不查;
// 解散要删 guild 行(连同 idx_guild_0(zone_id) 的项),与合服按 zone 整区改写同一批行,事务内再判一次才从调用关系上
// 消除并发。nil = 不设闸(单测与未配闸门的环境)。
// 输入约束:now 为服务进程时钟毫秒(> 0,由 logic 传入),用于建状态行与把全体成员在本帮的未决捐献截止时间提前到 now。
// 错误语义:ErrGuildGone / ErrRankTooLow(logic 映射为 kGuildNotLeader)/ ErrZoneMerging / ErrWriteConflict。
// 幂等:重放得到 ErrGuildGone。
// 扩展点:B5(捐献提前截止,已落)、B6(活动进度 / 历练战报)按表序插在提前截止之后、删帮会行之前
// (90-consistency X-14 原写"删申请之后、删成员之前",删成员已按上文前移,见交付说明)。
func (r *GuildRepo) DisbandGuild(ctx context.Context, guildID, actorID, now uint64, fence FenceFunc) (DisbandResult, error) {
	// 事务外建状态行(规则 3:事务内只对已存在的行点锁)。这次读不加锁,只决定给谁建行;
	// 帮会不存在时读到空集,照常进事务拿 ErrGuildGone。
	preread, err := readIDColumn(ctx, r.db, sqlSelectMemberIDs, guildID)
	if err != nil {
		return DisbandResult{}, fmt.Errorf("read members of guild %d before disband: %w", guildID, err)
	}
	if err := r.ensurePlayerStateRows(ctx, opDisband, now, preread...); err != nil {
		return DisbandResult{}, err
	}

	var out DisbandResult
	err = r.inTx(ctx, opDisband, func(ctx context.Context, tx *sql.Tx) error {
		var res DisbandResult

		guild, err := lockGuildRow(ctx, tx, guildID)
		if err != nil {
			return err
		}
		if guild.LeaderID != actorID {
			return ErrRankTooLow
		}
		if err := checkFence(ctx, fence, guild.ZoneID); err != nil {
			return err
		}
		res.ZoneID = guild.ZoneID

		memberIDs, err := lockAllMembers(ctx, tx, guildID)
		if err != nil {
			return err
		}
		res.MemberIDs = memberIDs
		// 成员行已逐行锁住,紧接着逐行删(主键升序;为什么排在删申请之前见函数头)。
		for _, playerID := range memberIDs {
			if err := execExactlyOneRow(ctx, tx, fmt.Sprintf("delete member %d of disbanded guild %d", playerID, guildID),
				sqlDeleteMember, guildID, playerID); err != nil {
				return err
			}
		}

		// I3:本帮的待审申请,以及成员们在别的帮会留下的申请(不删会在解散后按 I1 复活),合成一个列表按主键升序删。
		keys, err := applicationKeysOfGuild(ctx, tx, guildID)
		if err != nil {
			return err
		}
		memberKeys, err := applicationKeysOfPlayers(ctx, tx, memberIDs)
		if err != nil {
			return err
		}
		if err := deleteApplicationRows(ctx, tx, append(keys, memberKeys...)); err != nil {
			return fmt.Errorf("delete applications of disbanded guild %d: %w", guildID, err)
		}
		// B5 捐献提前截止。解散后帮会行不在了,这些捐献即便被 scene 扣成功也记不上资金(D2 计 orphan),
		// 所以更要让它们尽快改发中止。前提(accelerateDonationDeadlines 的候选集完整性):全体成员行在上面锁住
		// (已删,X 锁持有到提交),捐献预留必须先锁成员行才会插 op 行。
		if err := accelerateDonationDeadlines(ctx, tx, guildID, memberIDs, now); err != nil {
			return err
		}
		// 0 行 = 这一瞬间帮会已经被别的事务解散了(我们持有的行锁本该挡住,
		// 但库层面真发生时要给出**业务**答复而不是内部错误):回 ErrGuildGone,
		// logic 层会映射成 kGuildNotFound。>1 行则是 WHERE 写错,属于必须暴露的内部错误。
		result, err := tx.ExecContext(ctx, sqlDeleteGuild, guildID)
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

// lockAllMembers 锁住该帮全体成员的状态行与成员行,返回 player_id 升序列表。前置:已持有该帮 guild 行锁。
//
// 三步,顺序即锁序(位置 2 在位置 3 之前):
//  1. 普通读成员 id(ORDER BY player_id):持有 guild 行锁时成员集合稳定,RC 新快照看得见此前已提交的全部增删;
//  2. 按 player_id 升序逐个 lockPlayerState;
//  3. 按主键 (guild_id, player_id) 升序逐个 sqlLockMemberRole 点锁。
//
// 第 3 步点不到行说明"guild 行锁下成员集合稳定"这条不变量被破坏了(有路径不锁 guild 行就增删成员),
// 不猜、不跳过:回内部错误整体回滚,继续删只会留下半个帮会。
func lockAllMembers(ctx context.Context, tx *sql.Tx, guildID uint64) ([]uint64, error) {
	memberIDs, err := readIDColumn(ctx, tx, sqlSelectMemberIDs, guildID)
	if err != nil {
		return nil, fmt.Errorf("read members of guild %d: %w", guildID, err)
	}
	for _, playerID := range memberIDs {
		if err := lockPlayerState(ctx, tx, playerID); err != nil {
			return nil, err
		}
	}
	for _, playerID := range memberIDs {
		_, found, err := lockMemberRole(ctx, tx, guildID, playerID)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("member %d of guild %d vanished under the guild row lock", playerID, guildID)
		}
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
