package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"

	"github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/logx"
)

// Leaf-segment 号段发号(设计 docs/design/node-id-overhaul-plan-20260908.md §6.2)。
// 一个持有者(login / guild / C++ scene 经 AllocateIdSegment RPC)一次领走 [lo, hi),
// 表里只记"已分配到(不含)"。没有 lease、没有时钟、没有 worker id:冻结 / 重启 /
// 时钟回拨都不影响唯一性,库只在续段时被碰一次。

const (
	// IdSegmentCap 号段值域上限(不含):2^55,与 idSegmentBootstrapDDL 的 CHECK 常量一致。
	// 存量 snowflake 号最小 ≈ 6.7e16 > 2^55 ≈ 3.6e16,两个区间永不相交,新旧号可共存。
	IdSegmentCap uint64 = 1 << 55

	// IdSegmentMaxStep 单次领段长度上限。设计按"10 分钟峰值发号量"配 step,item 的
	// 1,000,000 是最大预期;上限再给 10 倍余量,拦住把 step 传成 hi 一类的调用错误。
	IdSegmentMaxStep uint32 = 10_000_000

	// IdSegmentFirstID 新 biz_tag 的起始号。0 保持非法,与 kInvalidGuid / player_id 0 语义一致。
	IdSegmentFirstID uint64 = 1

	// IdSegmentBootstrapStep 迁移预建行时写入的 step。它只在调用方传 step=0("沿用行上的")
	// 时才起作用:login / guild 传 idsegment.Conf.Step(默认 100),C++ scene 按类型自带 step,
	// 且非 0 的请求 step 会回写覆盖它。所以这里不需要按 tag 配表,取与 Go 客户端默认一致的 100。
	IdSegmentBootstrapStep uint32 = 100

	// idSegmentTxAttempts 事务级重试上限:只重试 MySQL 1213(死锁)/ 1205(锁等待超时)
	// 和"首次使用需要先种行"(仅 AllowAutoSeed=true)这两种可预期的情况。
	idSegmentTxAttempts = 4
)

// DefaultIdSegmentBootstrapTags 是设计 §6.4 / §7.5 第 1 条里走号段的五种永久身份,
// 与 config.DefaultIdSegmentBootstrapTags 同一份清单(config 包不能被本包 import,反向也不行)。
// 迁移路径以 config 的 EffectiveBootstrapTags 为准;这份给 main 的 -migrate 摘要和测试用。
var DefaultIdSegmentBootstrapTags = []string{"player", "guild", "item", "txlog", "snapshot"}

var (
	// ErrIdSegmentExhausted:max_id + step 会越过 2^55,fail-closed,表状态零变更。
	ErrIdSegmentExhausted = errors.New("id segment: max_id + step would cross the 2^55 cap")
	// ErrIdSegmentInvalidStep:step 不在 1..IdSegmentMaxStep;或首次使用某 biz_tag 时 step=0
	// (表里还没有可继承的 step)。
	ErrIdSegmentInvalidStep = errors.New("id segment: step must be within 1..10000000")
	// ErrIdSegmentInvalidTag:biz_tag 不合法。表主键是 VARCHAR(64),而 dev(AllowAutoSeed=true)
	// 下任何字符串都会在首次使用时自动种一行,BootstrapTags 也直接进这张表,所以必须收紧
	// 字符集,免得一个手滑的调用方 / 一份手滑的 yaml 往表里塞垃圾行。
	ErrIdSegmentInvalidTag = errors.New("id segment: biz_tag must match ^[a-z0-9_]{1,64}$")
	// ErrIdSegmentUnknownTag:表里没有这个 biz_tag 的行,且本 store 不允许自动补种
	// (生产形态)。缺行是 ID 安全事件而不是"首次使用":见 IdSegmentOptions.AllowAutoSeed。
	ErrIdSegmentUnknownTag = errors.New("id segment: biz_tag has no row and runtime auto-seed is disabled; create it with `data_service -f <yaml> -migrate` (IdSegment.BootstrapTags)")
)

var bizTagPattern = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// IsValidBizTag 是 Allocate 用的同一条 biz_tag 规则,给 logic 层做指标 label 消毒:
// 不合法的 tag 不能原样进 Prometheus label(基数无界)。
func IsValidBizTag(tag string) bool {
	return bizTagPattern.MatchString(tag)
}

// IdSegmentOptions 号段 store 的行为开关,来自 config.IdSegmentConfig。
type IdSegmentOptions struct {
	// AllowAutoSeed=true:Allocate 遇到表里没有的 biz_tag 时自动种一行从 1 起(仅 dev)。
	//
	// 生产必须 false(设计 §7.5 第 7 条):id_segment 的行只会在全局库被 drop 重建、或从旧
	// 备份恢复时消失,而消费表(player_database / guild / 玩家 blob 里的物品)还留着已发出的
	// 号。此时从 1 重发,login 的 INSERT ... ON DUPLICATE KEY UPDATE 会静默覆盖别人的角色行,
	// 没有任何报错。计数器必须有一条"库被重置也活得下来"的底线 —— 那就是"缺行即拒绝",
	// 行只能由迁移显式创建(bootstrapIdSegmentRows + raiseIdSegmentFloor)。
	AllowAutoSeed bool
}

// IdSegmentStore 号段表的读写。
type IdSegmentStore struct {
	db            *sql.DB
	allowAutoSeed bool
}

// NewIdSegmentStore opens its own pool on the global DB (same pattern as the other stores).
func NewIdSegmentStore(cfg MySQLConfig, opts IdSegmentOptions) (*IdSegmentStore, error) {
	db, err := openMySQL(cfg)
	if err != nil {
		return nil, err
	}
	logx.Infof("[IdSegmentStore] connected to %s/%s allow_auto_seed=%t", cfg.Host, cfg.DBName, opts.AllowAutoSeed)
	return &IdSegmentStore{db: db, allowAutoSeed: opts.AllowAutoSeed}, nil
}

// Close releases the database connection.
func (s *IdSegmentStore) Close() error {
	return s.db.Close()
}

type idSegmentRow struct {
	maxID   uint64
	step    uint32
	version uint64
}

// Allocate 领走 [lo, hi) 并推进 max_id。
//
// 并发策略:悲观锁 `SELECT ... FOR UPDATE` + 同一事务里 `UPDATE ... WHERE version=?`。
// 选它而不是纯乐观 CAS 重读循环的原因:这张表只有几行、每个持有者十分钟才来一次,
// 行锁把同 biz_tag 的并发领段直接串行化,不需要写"冲突就重来"的循环;version 仍然
// 参与 WHERE,作为对锁语义的二次断言(RowsAffected != 1 即视为异常,不静默)。
//
// 首次使用某个 biz_tag:种子行 **在事务之外** 用 INSERT IGNORE 写入,然后重跑事务。
// 不能在持有 FOR UPDATE 间隙锁的事务里 INSERT —— 两个连接同时对不存在的行 FOR UPDATE
// 会各拿到共享间隙锁,再各自 INSERT 就互相等待,MySQL 只能杀掉一个(1213)。
//
// step:请求给了非 0 就用请求的(并回写到行上,下次 0 就继承它);0 = 沿用行上的。
func (s *IdSegmentStore) Allocate(ctx context.Context, bizTag string, step uint32) (lo, hi uint64, err error) {
	if !bizTagPattern.MatchString(bizTag) {
		return 0, 0, ErrIdSegmentInvalidTag
	}
	if step > IdSegmentMaxStep {
		return 0, 0, ErrIdSegmentInvalidStep
	}

	for attempt := 1; attempt <= idSegmentTxAttempts; attempt++ {
		lo, hi, found, err := s.tryAllocate(ctx, bizTag, step)
		if err != nil {
			if isRetryableMySQL(err) && attempt < idSegmentTxAttempts {
				logx.Infof("[IdSegmentStore] biz_tag=%s allocate retry %d/%d after lock error: %v",
					bizTag, attempt, idSegmentTxAttempts, err)
				continue
			}
			return 0, 0, err
		}
		if found {
			return lo, hi, nil
		}

		// 表里没有这个 biz_tag。
		if !s.allowAutoSeed {
			// 生产形态:缺行是 ID 安全事件(库被重置 / 从备份恢复 / 漏配 BootstrapTags),
			// 不写任何行,让调用方拿到明确的故障码而不是一段从 1 开始的重号。
			return 0, 0, fmt.Errorf("%w (biz_tag=%s)", ErrIdSegmentUnknownTag, bizTag)
		}
		// dev 形态:首次使用自动种行。没有可继承的 step 就没法定义"一段"。
		if step == 0 {
			return 0, 0, fmt.Errorf("%w: biz_tag %s has no row yet and request step is 0", ErrIdSegmentInvalidStep, bizTag)
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT IGNORE INTO id_segment (biz_tag, max_id, step, version) VALUES (?, ?, ?, 0)`,
			bizTag, IdSegmentFirstID, step); err != nil {
			return 0, 0, fmt.Errorf("seed id_segment row for %s: %w", bizTag, err)
		}
		// 故意打 Error 而不是 Info(go-zero 没有 Warn 级别,本仓库也不用 Slow):补种恰恰是
		// 生产环境会被拒绝的那个事件,它在 dev 出现意味着这个 tag 没进 IdSegment.BootstrapTags,
		// 必须显眼到有人去补配置,而不是淹没在启动日志里。
		logx.Errorf("[IdSegmentStore] AUTO-SEEDED biz_tag=%s first_id=%d step=%d (IdSegment.AllowAutoSeed=true, dev only): "+
			"this tag is not in IdSegment.BootstrapTags; add it there. In production (AllowAutoSeed=false) this call would have been rejected with ErrCodeIdSegmentUnknownTag",
			bizTag, IdSegmentFirstID, step)
	}
	return 0, 0, fmt.Errorf("id segment: biz_tag %s could not be allocated after %d attempts", bizTag, idSegmentTxAttempts)
}

// tryAllocate 一次事务:锁行 → 校验 → 推进。found=false 表示行不存在(事务已回滚,未写任何东西)。
func (s *IdSegmentStore) tryAllocate(ctx context.Context, bizTag string, reqStep uint32) (lo, hi uint64, found bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, false, fmt.Errorf("begin id_segment tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	var cur idSegmentRow
	err = tx.QueryRowContext(ctx,
		`SELECT max_id, step, version FROM id_segment WHERE biz_tag = ? FOR UPDATE`, bizTag,
	).Scan(&cur.maxID, &cur.step, &cur.version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, fmt.Errorf("lock id_segment row %s: %w", bizTag, err)
	}

	useStep := reqStep
	if useStep == 0 {
		useStep = cur.step
	}
	if useStep == 0 || useStep > IdSegmentMaxStep {
		return 0, 0, true, fmt.Errorf("%w: row step=%d request step=%d", ErrIdSegmentInvalidStep, cur.step, reqStep)
	}

	lo = cur.maxID
	if lo < IdSegmentFirstID {
		// 手工 INSERT 了 max_id=0 的行:0 不能发出去,从 1 起。
		lo = IdSegmentFirstID
	}
	hi = lo + uint64(useStep)
	// 上限校验在 UPDATE 之前:表状态零变更,CHECK 只是兜底。
	if hi >= IdSegmentCap {
		return 0, 0, true, fmt.Errorf("%w: biz_tag=%s max_id=%d step=%d", ErrIdSegmentExhausted, bizTag, cur.maxID, useStep)
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE id_segment SET max_id = ?, step = ?, version = version + 1 WHERE biz_tag = ? AND version = ?`,
		hi, useStep, bizTag, cur.version)
	if err != nil {
		return 0, 0, true, fmt.Errorf("advance id_segment %s: %w", bizTag, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, 0, true, fmt.Errorf("advance id_segment %s rows affected: %w", bizTag, err)
	}
	if n != 1 {
		// FOR UPDATE 之下不应发生;发生了就是锁语义被破坏(例如有人绕过本 store 改 version),必须报错而不是静默。
		return 0, 0, true, fmt.Errorf("id segment: CAS on %s affected %d rows (expected 1) despite row lock", bizTag, n)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, true, fmt.Errorf("commit id_segment %s: %w", bizTag, err)
	}
	committed = true
	return lo, hi, true, nil
}

// isRetryableMySQL 只把死锁(1213)和锁等待超时(1205)视为可重试。
func isRetryableMySQL(err error) bool {
	var me *mysql.MySQLError
	if !errors.As(err, &me) {
		return false
	}
	return me.Number == 1213 || me.Number == 1205
}

// ── 迁移期的行管理(由 schema.go 的 migrateSchemaOn 在表就位之后调用)─────────────

// bootstrapIdSegmentRows 给每个 tag 用 INSERT IGNORE 预建一行(max_id=1, step=100, version=0)。
// 幂等:已有的行一个字节都不碰(尤其**绝不降低** max_id),所以生产每次部署重复跑都安全。
// 这是生产环境创建号段行的唯一途径(运行期补种只在 dev 开着,见 IdSegmentOptions)。
func bootstrapIdSegmentRows(ctx context.Context, db *sql.DB, tags []string) error {
	for _, tag := range tags {
		if !bizTagPattern.MatchString(tag) {
			// 一份手滑的 yaml 不能往主键表里塞垃圾行;拒绝整次迁移比静默跳过更容易被发现。
			return fmt.Errorf("%w: IdSegment.BootstrapTags entry %q", ErrIdSegmentInvalidTag, tag)
		}
		res, err := db.ExecContext(ctx,
			`INSERT IGNORE INTO id_segment (biz_tag, max_id, step, version) VALUES (?, ?, ?, 0)`,
			tag, IdSegmentFirstID, IdSegmentBootstrapStep)
		if err != nil {
			return fmt.Errorf("bootstrap id_segment row %s: %w", tag, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("bootstrap id_segment row %s rows affected: %w", tag, err)
		}
		if n == 1 {
			logx.Infof("[schema] id_segment: bootstrapped biz_tag=%s max_id=%d step=%d version=0", tag, IdSegmentFirstID, IdSegmentBootstrapStep)
		} else {
			logx.Infof("[schema] id_segment: biz_tag=%s already present, left untouched", tag)
		}
	}
	return nil
}

// idSegmentFloorSource 一种永久身份的消费表:迁移时拿它的最大号给 id_segment 的水位兜底。
type idSegmentFloorSource struct {
	bizTag string
	table  string
	column string
}

// raiseIdSegmentFloor 把 src.bizTag 行的 max_id 抬到 MAX(消费表主键)+1(只在它落后时)。
//
// 目标是从备份恢复 / 库被重置这种场景:id_segment 行回到了旧水位(或被 bootstrap 重新种成 1),
// 而消费表里已经有更大的号。不变量是 max_id > 已发出的最大号("已分配到,不含"),所以判据是
// MAX >= max_id 就抬(MAX == max_id 也算落后:max_id 说 [.., max_id) 已发,而 max_id 本身却已在表里)。
//
// 三个"跳过"都是刻意的:消费表不在同一个库(zone 库的 player_database 通常与全局库不同库,
// 迁移只能看见自己的 DSN)、表存在但列不对、id_segment 里没有这一行(行由 bootstrap 负责,
// 不在 BootstrapTags 里的 tag 不归本函数创建)。跳过不等于安全:恢复全局库前仍须按运维手册
// 人工核对 max_id ≥ 消费表最大号,本函数只是能自动查到时的一道兜底。
//
// `column < 2^55` 过滤掉存量 snowflake 号(≥ 6.7e16,设计 §6.3):它们不是号段发的,
// 拿它们抬水位会把号段值域一口气烧到上限。UPDATE 带 `max_id < ?` 守卫,与并发中的
// Allocate 互斥安全:发号已经把水位推过去了就是 0 行,永远不会降低。
func raiseIdSegmentFloor(ctx context.Context, db *sql.DB, dbName string, src idSegmentFloorSource) error {
	var present int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
		dbName, src.table, src.column).Scan(&present); err != nil {
		return fmt.Errorf("probe consuming table %s.%s for id_segment floor: %w", src.table, src.column, err)
	}
	if present == 0 {
		logx.Infof("[schema] id_segment floor check for biz_tag=%s skipped: %s.%s not in database %s (consuming table lives elsewhere)",
			src.bizTag, src.table, src.column, dbName)
		return nil
	}

	var maxID uint64
	err := db.QueryRowContext(ctx, `SELECT max_id FROM id_segment WHERE biz_tag = ?`, src.bizTag).Scan(&maxID)
	if errors.Is(err, sql.ErrNoRows) {
		logx.Infof("[schema] id_segment floor check for biz_tag=%s skipped: no row (not in IdSegment.BootstrapTags)", src.bizTag)
		return nil
	}
	if err != nil {
		return fmt.Errorf("read id_segment row %s for floor check: %w", src.bizTag, err)
	}

	var maxConsumed uint64
	if err := db.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(`"+src.column+"`), 0) FROM `"+src.table+"` WHERE `"+src.column+"` < ?",
		IdSegmentCap).Scan(&maxConsumed); err != nil {
		return fmt.Errorf("read MAX(%s.%s) for id_segment floor: %w", src.table, src.column, err)
	}
	if maxConsumed < maxID {
		logx.Infof("[schema] id_segment floor check biz_tag=%s ok: max_id=%d > MAX(%s.%s)=%d", src.bizTag, maxID, src.table, src.column, maxConsumed)
		return nil
	}

	floor := maxConsumed + 1
	res, err := db.ExecContext(ctx,
		`UPDATE id_segment SET max_id = ?, version = version + 1 WHERE biz_tag = ? AND max_id < ?`,
		floor, src.bizTag, floor)
	if err != nil {
		return fmt.Errorf("raise id_segment floor for %s to %d: %w", src.bizTag, floor, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("raise id_segment floor for %s rows affected: %w", src.bizTag, err)
	}
	if n == 1 {
		logx.Errorf("[schema] id_segment counter was behind the consuming table — backup restore? biz_tag=%s max_id=%d MAX(%s.%s)=%d; raised max_id to %d. "+
			"Every id in [%d, %d) minted after the restored counter was reused; audit the consuming tables for overwritten rows (see docs/design/data_service_role_and_scope.md runbook)",
			src.bizTag, maxID, src.table, src.column, maxConsumed, floor, maxID, floor)
	} else {
		// 并发的 Allocate 在两次查询之间把水位推过了 floor:守卫生效,什么都没改。
		logx.Infof("[schema] id_segment floor for biz_tag=%s already advanced past %d by a concurrent allocation; nothing changed", src.bizTag, floor)
	}
	return nil
}
