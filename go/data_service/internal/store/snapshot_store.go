package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/logx"
)

// player_snapshot.source 列的取值契约。两种来源的 data 列格式**不兼容**:
//   - SnapshotSourceDataService(0):GM / data_service 自己写入,data 是 JSON
//     {fields: map[string][]byte}(Redis 字段图),snapshot_type 是 data_service 的 SnapshotType。
//   - SnapshotSourceSceneKafka(1):C++ scene 经 player_snapshot_topic 落库,data 是收到的
//     PlayerSnapshotEntry 原始 proto 字节(内含两个 player_database blob + schema_version),
//     snapshot_type 是 C++ 的 SnapshotTrigger 原值。统一两套枚举是以后的 proto 改动。
//
// 回滚 / diff / 列表等所有现有读路径在学会解 proto blob 之前必须只看 source=0,
// 否则 rollback_logic 的 json.Unmarshal 会在第一条 C++ 快照上炸掉今天能用的 GM 回滚。
const (
	SnapshotSourceDataService uint32 = 0
	SnapshotSourceSceneKafka  uint32 = 1

	// SnapshotOperatorSceneNode 是 source=1 行的 operator 固定值。
	SnapshotOperatorSceneNode = "scene-node"
)

// SnapshotRow represents a row in the player_snapshot table.
type SnapshotRow struct {
	ID           uint64
	PlayerID     uint64
	ZoneID       uint32
	SnapshotType uint32
	CreatedAt    uint64
	Reason       string
	Operator     string
	Data         []byte // source=0: JSON snapshotData; source=1: raw PlayerSnapshotEntry bytes
	SnapshotGuid uint64 // source=1: C++ SnapshotId (SnowFlake); 0 for GM rows
	Source       uint32 // SnapshotSourceDataService | SnapshotSourceSceneKafka
}

// AuditLogRow represents a row in the rollback_audit_log table.
type AuditLogRow struct {
	PlayerID              uint64
	ZoneID                uint32
	RollbackType          uint32 // 1=player, 2=zone, 3=server
	SnapshotIDUsed        uint64
	PreRollbackSnapshotID uint64
	TargetTime            uint64
	PlayersAffected       uint32
	PlayersFailed         uint32
	OrphansCleaned        uint32
	Reason                string
	Operator              string
	CreatedAt             uint64
}

const (
	// snapshotGuidInsertAttempts 是 InsertSnapshotIfGuidAbsent 对 InnoDB 死锁(1213)的就地尝试上限(含首次)。
	// 为什么是有界重试、为什么一定收敛,见 InsertSnapshotIfGuidAbsent 的「并发语义」;用尽后 1213 原样
	// 交给消费者的通用重试(kafka.SnapshotConsumer.insertWithRetry),不在这里无限转。
	snapshotGuidInsertAttempts = 3

	// snapshotGuidDeadlockBackoffMin / Max:两次尝试之间的等待取 [Min, Max] 内的均匀随机值。
	// 抖动是为了把两个刚互杀过的写者错开,不让它们在同一毫秒再同时拿同一段间隙锁;上限压在 50ms,
	// 单次调用就地最多多睡 (snapshotGuidInsertAttempts-1)×Max = 100ms,远小于消费者 1s 的通用退避,
	// 两层叠加不会把单条消息的最坏等待放大到超出消费者原有预算的量级。
	snapshotGuidDeadlockBackoffMin = 10 * time.Millisecond
	snapshotGuidDeadlockBackoffMax = 50 * time.Millisecond
)

// insertSnapshotIfGuidAbsentSQL 是按 guid 去重落库的那一条语句(语义见 InsertSnapshotIfGuidAbsent)。
// 抽成常量是为了让真库回归用例执行**同一段文本**来扮演另一个并发写者,两边不各抄一份、不会漂移。
const insertSnapshotIfGuidAbsentSQL = `INSERT INTO player_snapshot
	 (player_id, zone_id, snapshot_type, created_at, reason, operator, data, snapshot_guid, source)
	 SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?
	 FROM DUAL
	 WHERE NOT EXISTS (SELECT 1 FROM player_snapshot AS existing WHERE existing.snapshot_guid = ?)`

// SnapshotStore provides CRUD operations for player snapshots and audit logs.
// 表结构由 MigrateSchema 按 proto 建,本 store 不再碰 DDL。
type SnapshotStore struct {
	db *sql.DB

	// deadlockRetries 累计 InsertSnapshotIfGuidAbsent 因 1213 就地重跑语句的次数。
	// 只给日志与真库回归用例用:用例要证明"死锁确实撞上了、并且被就地重跑吸收",
	// 否则"最终成功"分不清是重试生效了,还是这一轮根本没撞上。
	deadlockRetries atomic.Uint64
}

// NewSnapshotStore opens the pool. It does NOT create tables — see schema.go.
func NewSnapshotStore(cfg MySQLConfig) (*SnapshotStore, error) {
	db, err := openMySQL(cfg)
	if err != nil {
		return nil, err
	}
	logx.Infof("[SnapshotStore] connected to %s/%s", cfg.Host, cfg.DBName)
	return &SnapshotStore{db: db}, nil
}

// Close releases the database connection.
func (s *SnapshotStore) Close() error {
	return s.db.Close()
}

// InsertSnapshot saves a snapshot and returns the auto-increment ID.
// GM 路径调用方不设 Source / SnapshotGuid,即落成 source=0、guid=0。
func (s *SnapshotStore) InsertSnapshot(ctx context.Context, row *SnapshotRow) (uint64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO player_snapshot
		 (player_id, zone_id, snapshot_type, created_at, reason, operator, data, snapshot_guid, source)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.PlayerID, row.ZoneID, row.SnapshotType, row.CreatedAt, row.Reason, row.Operator, row.Data,
		row.SnapshotGuid, row.Source,
	)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return uint64(id), nil
}

// InsertSnapshotIfGuidAbsent 按 snapshot_guid 去重落库,返回 inserted=false 时
// id 是已存在那行的自增 id。
//
// 去重由**一条语句**完成:`INSERT ... SELECT ... WHERE NOT EXISTS (...)`(insertSnapshotIfGuidAbsentSQL)。
// 拆成"先 SELECT 再 INSERT"的老写法在两次往返之间没有任何锁:滚动更新的窗口里
// (Deployment 默认 RollingUpdate 会让新旧两个 pod 同时在线)两个消费者实例足以把同一条
// 快照写成两行 source=1。
//
// # 并发语义(MySQL/InnoDB,REPEATABLE READ)
//
// 本 store 的 DSN 不设隔离级别,走库的全局默认 RR。RR 下 INSERT ... SELECT 的源端读(这里是 NOT EXISTS
// 子查询,走 idx(snapshot_guid))是**加锁读**:在 guid 所在位置加共享 next-key 锁;guid 不存在时锁住的是
// 它所在的那段间隙。snapshot_guid 是 SnowFlake、单调递增,新 guid 几乎总比表里所有 guid 都大,所以这段
// 间隙几乎总是 (max_guid, +∞)。于是只要有两个写者并发 —— **同 guid 或不同 guid 都一样** ——:
//  1. 两边的子查询都判定"不存在",各拿到同一段间隙上的 S 锁(间隙 S 锁彼此兼容);
//  2. 两边都要往这段间隙里插自己的 guid,各自申请插入意向锁,又各被对方的 S 间隙锁挡住;
//  3. InnoDB 检测到环,回滚其中一条语句(自动提交 = 整个事务),它收到 1213;另一方插入并提交。
//
// 所以"并发时只会有一个成功、另一个看到 0 行受影响"并不成立:另一个先吃到的是 1213。
//
// 本函数对 1213 **就地**有界重跑同一条语句(最多 snapshotGuidInsertAttempts 次,间隔 10–50ms 抖动,
// ctx 可取消),不让它冒泡成消费者 1s 一次的通用重试和一条 ERROR。收敛论证:
//   - 被牺牲方的语句已整条回滚、锁全部放掉,胜者的插入意向锁随即拿到,语句完成并自动提交。
//   - 同 guid:重跑时子查询在胜者那条索引记录上加 S 锁,胜者未提交就等它提交(单向等待,不成环);
//     之后加锁读(读最新已提交版本)看到这一行,NOT EXISTS 为假、0 行受影响,走下面"查已存在 id"
//     的分支,返回 inserted=false。一次重跑即收敛。
//   - 不同 guid:胜者这一行已经进了索引、把原来那段间隙切开;胜者未提交时本方最多单向等它。
//     唯一会再次成环的情形是胜者**紧接着又写下一条**快照、又落进同一段间隙 —— 每成一次环都有一方
//     提交,整体一直在前进。单条消息连输 snapshotGuidInsertAttempts 次时,1213 交给消费者按 1s 间隔
//     再试(最多 DBMaxAttempts 次),1s 的错峰足以把两个写者的节奏打散;消费者最终放弃也只是停在当前
//     offset 不提交(宁可滞后不丢),既不丢也不重。
//   - 能并发的写者本来就只有"短时两个"这个量级:部署不变量是 data-service replicas=1 + Recreate
//     (deploy/k8s/manifests/go-svc/data-service.yaml),只有本地多开 / 手工改副本数时才会出现并发写者,
//     也才会看到本函数"就地吸收了 1213"的日志。
//
// 为什么只就地重试 1213、不重试 1205(锁等待超时):1213 在环形成的那一刻就报出,就地重跑的代价是几十
// 毫秒;1205 则说明本条语句已经白等了一整个 innodb_lock_wait_timeout(默认 50s,多半是被别的长事务压住),
// 就地再等 snapshotGuidInsertAttempts 次,会把消费者单条消息的最坏等待从 DBMaxAttempts×50s 放大到
// DBMaxAttempts×150s。1205 按原样交给消费者的 1s 重试。关掉 innodb_deadlock_detect 的库上,死锁会以 1205
// 的形式出现,同样走消费者重试。
//
// **不要**把这条语句或 DSN 改成 READ COMMITTED 来"消掉间隙锁":RC 下子查询是一致性读、不加任何锁,
// 两个并发写者会同时判定"不存在"并各插一行,去重直接失效。同理,DB 侧的去重保证依赖 RR 加锁读:
// 库的全局隔离级别被改成 RC、或迁到没有间隙锁的库上时它就不再成立,所以部署上仍保持消费者单实例。
//
// 为什么不是 UNIQUE 键:GM 行的 guid 恒为 0,proto2mysql 又做不出可空唯一列,UNIQUE 会让
// 第二条 GM 行就撞键。根治办法是在 STORED 生成列 NULLIF(snapshot_guid,0) 上建唯一键、写入改成普通单行
// INSERT(撞 1062 再查已有 id):去重不再依赖 NOT EXISTS 子查询的 S 间隙锁,不同 guid 的写者互不相干,
// 本节上面那条"两个写者在同一段间隙互等插入意向锁"的环随之消失。但它**消不掉**InnoDB 固有的一种情形:
// 同一个 guid 有 ≥2 个后到者排在先到者后面、而先到者随后回滚(被选为别处死锁的牺牲者、ctx 取消断连),
// 或 purge 在排队期间清掉了删除标记记录 —— 排队者的锁被继承成后继记录上的间隙锁,彼此再互等插入意向锁
// 成环(手册 "Locks Set by Different SQL Statements" 三会话例的变体),SQL 层去不掉。所以根治之后
// 仍须保留本函数的有界 1213 重试(次数上限 + 抖动退避 + ctx 可取消),不能因为"改成唯一键了"就删掉。
// 这一步还要先清掉历史重复行、确认 schema 同步不会删掉 proto 里没有的列与键、确认 TiDB 支持,
// 是一次独立的迁移(审计 #11 第二步),尚未做,等决策。
// 索引本身由 store.MigrateSchema 的 ensureIndexes 保证存在 —— 没有它这条语句会退化成
// 每次全表扫,并且间隙锁的范围会放大到整表。
func (s *SnapshotStore) InsertSnapshotIfGuidAbsent(ctx context.Context, row *SnapshotRow) (id uint64, inserted bool, err error) {
	if row.SnapshotGuid == 0 {
		return 0, false, fmt.Errorf("snapshot_guid must be non-zero for check-then-insert")
	}

	var res sql.Result
	reruns, err := retryOnDeadlock(ctx, snapshotGuidInsertAttempts, snapshotGuidDeadlockBackoff, func() error {
		var execErr error
		res, execErr = s.db.ExecContext(ctx, insertSnapshotIfGuidAbsentSQL,
			row.PlayerID, row.ZoneID, row.SnapshotType, row.CreatedAt, row.Reason, row.Operator, row.Data,
			row.SnapshotGuid, row.Source, row.SnapshotGuid,
		)
		return execErr
	})
	if reruns > 0 {
		s.deadlockRetries.Add(uint64(reruns))
		logx.Infof("[SnapshotStore] snapshot guid=%d 因 InnoDB 死锁(1213)就地重跑了 %d 次,结果 err=%v:有并发写者在 "+
			"idx(snapshot_guid) 同一段间隙上与本方互等插入意向锁,只有多实例写这张表时才会出现(部署不变量是单实例,"+
			"见 InsertSnapshotIfGuidAbsent 的「并发语义」)", row.SnapshotGuid, reruns, err)
	}
	if err != nil {
		// 就地预算用尽的 1213 也走这里:原样交给调用方(消费者)的通用重试,由它负责错峰与最终放弃。
		return 0, false, fmt.Errorf("insert snapshot guid %d: %w", row.SnapshotGuid, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("insert snapshot guid %d rows affected: %w", row.SnapshotGuid, err)
	}
	if affected > 0 {
		newID, err := res.LastInsertId()
		if err != nil {
			return 0, false, fmt.Errorf("insert snapshot guid %d last insert id: %w", row.SnapshotGuid, err)
		}
		return uint64(newID), true, nil
	}

	// 0 行受影响 = 这个 guid 已经在库里;把已存在那行的 id 回给调用方(日志/审计要用)。
	var existing uint64
	if err := s.db.QueryRowContext(ctx,
		`SELECT id FROM player_snapshot WHERE snapshot_guid = ? LIMIT 1`, row.SnapshotGuid,
	).Scan(&existing); err != nil {
		if err == sql.ErrNoRows {
			// 只可能是别人刚把它删了(retention 清理)。不当故障:重放会再落一次。
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("lookup snapshot_guid %d: %w", row.SnapshotGuid, err)
	}
	return existing, false, nil
}

// retryOnDeadlock 执行 op;op 返回 InnoDB 死锁(1213)时等 backoff() 后重跑,总尝试次数不超过 attempts。
//
// 契约:
//   - 只重跑 1213(isDeadlockMySQL)。1205 与其它错误立即原样返回,原因见 InsertSnapshotIfGuidAbsent。
//   - op 必须是"整条回滚后可原样重跑"的操作:这里只用于单条自动提交语句,InnoDB 选中牺牲者时
//     已把它整条回滚,重跑不会重复写。
//   - reruns 是因 1213 重跑 op 的次数;最后一次仍是 1213 时返回该错误(reruns = attempts-1)。
//   - 等待可被 ctx 取消:取消时立即返回,错误同时带上最后一次的 1213 与 ctx 的原因,调用方两者都能 errors.Is。
func retryOnDeadlock(ctx context.Context, attempts int, backoff func() time.Duration, op func() error) (reruns int, err error) {
	for attempt := 1; ; attempt++ {
		err = op()
		if err == nil || !isDeadlockMySQL(err) || attempt >= attempts {
			return reruns, err
		}
		if waitErr := sleepCtx(ctx, backoff()); waitErr != nil {
			return reruns, errors.Join(err, waitErr)
		}
		reruns++
	}
}

// snapshotGuidDeadlockBackoff 在 [snapshotGuidDeadlockBackoffMin, snapshotGuidDeadlockBackoffMax] 内均匀取一个等待时长。
func snapshotGuidDeadlockBackoff() time.Duration {
	return jitteredBackoff(snapshotGuidDeadlockBackoffMin, snapshotGuidDeadlockBackoffMax)
}

// isDeadlockMySQL 只认 1213(错误号常量见 id_segment_store.go)。刻意不复用 isRetryableMySQL(它把 1205 也算进来):
// 就地重跑 1205 会把消费者的最坏等待放大数倍,见 InsertSnapshotIfGuidAbsent。
func isDeadlockMySQL(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == mysqlErrDeadlock
}

// ── 本包锁冲突重试共用的两个原语(SnapshotStore 与 PlayerNameStore 都用,不各写一份)──────────────

// jitteredBackoff 在 [lo, hi] 内均匀取一个等待时长(即以区间中点为基准的 ± 抖动)。
//
// 抖动的目的:两个刚互杀过(1213)的事务如果按固定间隔同时回来,多半还会按同样的顺序撞在同一批锁上,
// 次数预算被空耗;随机错开之后,胜者通常已经提交,重来的一方只剩单向等待。
// hi <= lo 时返回 lo(区间退化成一个点),不让 rand.N 收到非正参数而 panic。
func jitteredBackoff(lo, hi time.Duration) time.Duration {
	if hi <= lo {
		return lo
	}
	return lo + rand.N(hi-lo+1)
}

// sleepCtx 可取消的等待:ctx 先结束就立即返回 ctx.Err()。不用 time.Sleep —— 在一个已经没有预算的
// ctx 上睡满,等于把"早点告诉调用方"拖成"超时失败"。
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// GetSnapshotByID loads a snapshot by primary key. source=1 行对回滚读路径不可见(见文件头)。
func (s *SnapshotStore) GetSnapshotByID(ctx context.Context, id uint64) (*SnapshotRow, error) {
	row := &SnapshotRow{}
	err := s.db.QueryRowContext(ctx,
		`SELECT id, player_id, zone_id, snapshot_type, created_at, reason, operator, data, snapshot_guid, source
		 FROM player_snapshot WHERE id = ? AND source = ?`, id, SnapshotSourceDataService,
	).Scan(&row.ID, &row.PlayerID, &row.ZoneID, &row.SnapshotType, &row.CreatedAt,
		&row.Reason, &row.Operator, &row.Data, &row.SnapshotGuid, &row.Source)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return row, err
}

// GetLatestSnapshotBefore returns the most recent source=0 snapshot for a player before the given time.
func (s *SnapshotStore) GetLatestSnapshotBefore(ctx context.Context, playerID, beforeTime uint64) (*SnapshotRow, error) {
	row := &SnapshotRow{}
	err := s.db.QueryRowContext(ctx,
		`SELECT id, player_id, zone_id, snapshot_type, created_at, reason, operator, data, snapshot_guid, source
		 FROM player_snapshot
		 WHERE player_id = ? AND created_at <= ? AND source = ?
		 ORDER BY created_at DESC LIMIT 1`, playerID, beforeTime, SnapshotSourceDataService,
	).Scan(&row.ID, &row.PlayerID, &row.ZoneID, &row.SnapshotType, &row.CreatedAt,
		&row.Reason, &row.Operator, &row.Data, &row.SnapshotGuid, &row.Source)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return row, err
}

// ListSnapshots returns metadata (no data blob) for a player's source=0 snapshots.
func (s *SnapshotStore) ListSnapshots(ctx context.Context, playerID, beforeTime uint64, limit uint32) ([]*SnapshotRow, error) {
	metas, err := s.ListSnapshotsMeta(ctx, playerID, beforeTime, limit)
	if err != nil {
		return nil, err
	}
	result := make([]*SnapshotRow, 0, len(metas))
	for _, m := range metas {
		r := m.SnapshotRow
		result = append(result, &r)
	}
	return result, nil
}

// SnapshotMeta is ListSnapshotsMeta's row: metadata plus data size, no blob.
type SnapshotMeta struct {
	SnapshotRow
	DataSizeBytes uint32
}

// ListSnapshotsMeta lists a player's source=0 snapshots newest first.
func (s *SnapshotStore) ListSnapshotsMeta(ctx context.Context, playerID, beforeTime uint64, limit uint32) ([]*SnapshotMeta, error) {
	if limit == 0 {
		limit = 20
	}

	var (
		rows *sql.Rows
		err  error
	)
	if beforeTime > 0 {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, player_id, zone_id, snapshot_type, created_at, reason, operator, snapshot_guid, source, COALESCE(LENGTH(data),0)
			 FROM player_snapshot
			 WHERE player_id = ? AND created_at <= ? AND source = ?
			 ORDER BY created_at DESC LIMIT ?`, playerID, beforeTime, SnapshotSourceDataService, limit)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, player_id, zone_id, snapshot_type, created_at, reason, operator, snapshot_guid, source, COALESCE(LENGTH(data),0)
			 FROM player_snapshot
			 WHERE player_id = ? AND source = ?
			 ORDER BY created_at DESC LIMIT ?`, playerID, SnapshotSourceDataService, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []*SnapshotMeta
	for rows.Next() {
		m := &SnapshotMeta{}
		if err := rows.Scan(&m.ID, &m.PlayerID, &m.ZoneID, &m.SnapshotType, &m.CreatedAt,
			&m.Reason, &m.Operator, &m.SnapshotGuid, &m.Source, &m.DataSizeBytes); err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

// GetSnapshotPlayerIDsByZone returns all player IDs with a source=0 snapshot in a zone
// at or before beforeTime. Uses the zone_id stored at snapshot time.
func (s *SnapshotStore) GetSnapshotPlayerIDsByZone(ctx context.Context, zoneID uint32, beforeTime uint64) ([]uint64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT player_id FROM player_snapshot
		 WHERE zone_id = ? AND created_at <= ? AND source = ?`, zoneID, beforeTime, SnapshotSourceDataService)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []uint64
	for rows.Next() {
		var pid uint64
		if err := rows.Scan(&pid); err != nil {
			return nil, err
		}
		ids = append(ids, pid)
	}
	return ids, rows.Err()
}

// SceneSnapshotMeta 是 source=1(C++ scene)快照的元数据,给将来的 GM 列表 / 恢复路径用。
type SceneSnapshotMeta struct {
	ID            uint64 // player_snapshot.id(自增)
	SnapshotGuid  uint64 // C++ SnapshotId
	ZoneID        uint32
	Trigger       uint32 // C++ SnapshotTrigger 原值(snapshot_type 列)
	CreatedAt     uint64 // = PlayerSnapshotEntry.snapshot_time
	DataSizeBytes uint32
}

// ListSceneSnapshotsByPlayer lists a player's source=1 rows newest first (no blob).
// 尚未接 RPC:source=1 的恢复路径是下一阶段。
func (s *SnapshotStore) ListSceneSnapshotsByPlayer(ctx context.Context, playerID uint64, limit uint32) ([]*SceneSnapshotMeta, error) {
	if limit == 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, snapshot_guid, zone_id, snapshot_type, created_at, COALESCE(LENGTH(data),0)
		 FROM player_snapshot
		 WHERE player_id = ? AND source = ?
		 ORDER BY created_at DESC, id DESC LIMIT ?`, playerID, SnapshotSourceSceneKafka, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []*SceneSnapshotMeta
	for rows.Next() {
		m := &SceneSnapshotMeta{}
		if err := rows.Scan(&m.ID, &m.SnapshotGuid, &m.ZoneID, &m.Trigger, &m.CreatedAt, &m.DataSizeBytes); err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

// InsertAuditLog writes a rollback audit record.
func (s *SnapshotStore) InsertAuditLog(ctx context.Context, row *AuditLogRow) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO rollback_audit_log
		 (player_id, zone_id, rollback_type, snapshot_id_used, pre_rollback_snapshot_id,
		  target_time, players_affected, players_failed, orphans_cleaned, reason, operator, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.PlayerID, row.ZoneID, row.RollbackType, row.SnapshotIDUsed,
		row.PreRollbackSnapshotID, row.TargetTime, row.PlayersAffected,
		row.PlayersFailed, row.OrphansCleaned, row.Reason, row.Operator, row.CreatedAt,
	)
	return err
}

// DeleteOldSnapshots removes snapshots (both sources) older than the given timestamp.
// Used for retention policies.
func (s *SnapshotStore) DeleteOldSnapshots(ctx context.Context, olderThan uint64) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM player_snapshot WHERE created_at < ?`, olderThan)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
