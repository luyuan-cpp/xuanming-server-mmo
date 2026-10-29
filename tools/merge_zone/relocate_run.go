package main

// -mode relocate / -mode relocate-abort 的入口(R0)与三个接缝的生产实现(docs/design/player-storage-placement.md §9)。
// 状态机本体见 relocate.go,清单见 relocate_manifest.go。
//
// R0(任何写之前,拒绝即 exit 1、一个字节都不写):
//   - S≠T,两个落点库都在同一个 MySQL 实例上存在(同实例跨库 INSERT … SELECT 的前提,与 go/db 按需打开落点库
//     的前提相同,§6.1);
//   - 在 S 里发现玩家表,T 里同名表按列名对齐(alignedPlayerColumns,与合服拷行同一份判据);
//   - 能力标记:-db-capability-zones 必须显式列出在跑的 zone(没有缺省值;搬库在线进行,不接受 none,
//     main 里已由 requireCapabilityZones 拒绝),另外把清单玩家的 home_zone 全部并进去一起核实 —— 处理他们存盘的正是 home 所在 zone
//     的 go/db,它不按落点选库,切换之后照样把写落进自己的 zone 库;
//   - 清单落盘。续跑时 S / T / 表集合 / topic 世代号必须与清单一致;映射来源(-route-source)续跑与 relocate-abort
//     都必须与清单一致(relocateRouteSourceRefusal,B2-1)。
//
// 落点读写的来源(D1 第 2 批,规格 §6.3 W11 / R8、§13.3 第 2、3、6 条):-route-source=db 时 relocatePlacements 换成
// sqlRelocatePlacements(全局库 player_route 是真源,提交后发布到 mapping Redis 并 WAITAOF 确认),R0 的枚举与读也走库;
// -route-source=redis 时是今天的纯 Redis 路径(redisRelocatePlacements),原样保留。两条路都先核对库与 mapping Redis 是否成对
// (openRouteBackend)。状态机(relocate.go 的引擎、decideFreeze、R1–R5)一格不改:只换接缝的实现。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// ── 接缝的生产实现 ────────────────────────────────────────────────

// redisRelocatePlacements 在 mapping Redis 上读写落点(与 player:zone、合服围栏同库,CAS 才能一次看全)。
type redisRelocatePlacements struct{ rdb *redis.Client }

func (p redisRelocatePlacements) read(ctx context.Context, ids []uint64) ([]placementRead, error) {
	return readPlacements(ctx, p.rdb, ids)
}

func (p redisRelocatePlacements) cas(ctx context.Context, ops []placementCAS) ([]placementCASResult, error) {
	return runPlacementCAS(ctx, p.rdb, ops)
}

// relocateRouteStore 是 sqlRelocatePlacements 用到的库原语(生产 *mergeRouteStore,route_store.go;单测注入内存替身)。
type relocateRouteStore interface {
	// ReadRoutes 按 id 一致读;返回 map 里没有的 id = 库里没有这一行(权威缺席)。
	ReadRoutes(ctx context.Context, ids []uint64) (map[uint64]mergeRouteRow, error)
	// RelocateCAS 一个事务做完一批 CAS(≤ routeWriteBatchLimit 个、player_id 不重复);出错时一条都没提交。
	RelocateCAS(ctx context.Context, ops []placementCAS) ([]routeCASResult, error)
	// RequireWritable 是写开关预检:route_meta.writable != 1(映射写者被冻结,回档恢复进行中)即返回包着 errRouteNotWritable
	// 的 error,读不到开关同样报错。普通一致读,与每个写事务的第一句同一判据;只是预检,提交点仍由 RelocateCAS 事务内复核。
	RequireWritable(ctx context.Context) error
}

// RequireWritable 是 relocateRouteStore 与 storage-audit(storageAuditRouteAuthority)用的写开关预检(requireRouteWritable,
// 事务外普通读)。写在这里而不是 route_store.go:只有搬库读路径与 storage-audit 需要它,写事务自己在事务内读开关。
func (s *mergeRouteStore) RequireWritable(ctx context.Context) error {
	return requireRouteWritable(ctx, s.db)
}

// relocateRoutePublisher 是 sqlRelocatePlacements 用到的发布原语(生产 *routePublisher,route_publish.go;单测注入假 Redis)。
type relocateRoutePublisher interface {
	// catchUpRoutes:库 rev 领先缓存 maprev、缓存没有 maprev 或值不同的行按库值发布并 WAITAOF;确认不了即 error。
	catchUpRoutes(ctx context.Context, rows []mergeRouteRow) (routeCatchUpReport, error)
	// publishRoutes:把一批已提交的行发布并 WAITAOF;任一行没确认即 error(整批按未完成处理)。
	publishRoutes(ctx context.Context, rows []mergeRouteRow) (routePublishReport, error)
}

// sqlRelocatePlacements 是 -route-source=db 时 relocatePlacements 的实现(规格 §6.3 W11、§13.3 第 2 条):全局库是真源,
// mapping Redis 是读方(go/db 选库)看的副本。
//
//   - read:读库,把 storage_id / placement_version / frozen_run_id 编码成与 Redis 里逐字节相同的落点值串(home 同理,
//     routePlacementRead),判定函数不区分来源。返回之前,对「库 rev 领先缓存 maprev、缓存没有 maprev 或值不同」的行先按库值
//     发布 + WAITAOF(catchUpRoutes),确认不了就返回 error、一个结果都不给 —— 依赖步骤拿不到读结果就不会推进。
//     这关掉了「R1 冻结已提交、发布没确认 → 续跑按库认领冻结 → 直接进 R2」的窗口:R2 等的是 go/db 看到冻结之后不再写 S,
//     而 go/db 看的是缓存;缓存还是未冻结时就开始拷,拷走的不是最后一份。
//     冻结态的行比规格多做一步:不论缓存是否已是这一版,一律发布 + WAITAOF(同值写回 maprev,WAITAOF 覆盖此刻已在缓存里的
//     状态)。catch-up 对「已是这一版」的行不再确认,把落盘确认交给当时的发布者 —— 而那个发布者可能正是本工具上一次没确认成
//     的那一次(EVAL 已执行、WAITAOF 份数不足);redis-ha 下随后切主,被提升的副本上就没有这次冻结。冻结行每批至多一批人,代价小。
//     有行要发布时先查写开关(RequireWritable):route_meta.writable != 1 即返回包着 errRouteNotWritable 的 error、不发布 ——
//     与对账 leader「冻结后不推送」一致,随后的 CAS 也必然被拒(R8-5)。
//   - cas:每 routeWriteBatchLimit 个一个事务(RelocateCAS:事务内比对编码后的当前值 / home / 围栏 63 号片,写列、rev + 1,
//     稳定态 version 不回退),提交后把写成的行发布 + WAITAOF。**只有确认了的才回 Written**;发布确认不了时按接口约定返回
//     「与 ops 同序的前缀结果 + error」,前缀截在本批第一个写成的 op 之前(之前的非 Written 结局是确定的,可以给);
//     已提交未确认的行由续跑的 read 补发布(上一条),或由对账 leader 的增量推送补齐。
//
// 「提交即安全」(规格 §6.3 W11):「发布没确认」不等于「缓存没变」—— EVAL 与 WAITAOF 在同一条 pipeline 里,份数不足时 EVAL 已经
// 生效,只是没确认落盘(B2-6)。R1 冻结提交、发布没确认 —— 缓存是未冻结(go/db 照写 S,数据仍在 S)或未确认的冻结值(go/db 延后写),
// 两者都正确;引擎拿不到 Written 不进 R2,续跑先把冻结再发布确认一次。R5 切换提交、发布没确认 —— 缓存是 R1 已确认的冻结态
// (go/db 延后写)或未确认的 "{T}:{v+1}"(go/db 开始写 T,T 是 R4 比对过的完整一份;即便随后切主回到冻结态也只是延后写),都安全;
// 引擎拿不到 Written 不记 switched,续跑按库认领已切换(decideFreeze 的 freezeAdoptSwitched),认领之前先补发布。解冻同理。
//
// readOnly(dry-run)时 read 只读库、不发布(dry-run 一个字节都不写),cas 一律拒绝。
type sqlRelocatePlacements struct {
	store    relocateRouteStore
	pub      relocateRoutePublisher
	readOnly bool
}

func (p sqlRelocatePlacements) read(ctx context.Context, ids []uint64) ([]placementRead, error) {
	out, publishable, err := p.readDB(ctx, ids)
	if err != nil {
		return nil, err
	}
	if p.readOnly || len(publishable) == 0 {
		return out, nil
	}
	// 发布之前先看写开关(R8-5):冻结期间(回档恢复,runbook §9.4)对账 leader 停止推送,本工具也不往缓存回填库值 ——
	// 这时库可能是落后的那一份;随后的 CAS 反正会被 RelocateCAS 事务内的同一判据拒绝,这里提前 fail-closed、一个结果都不给。
	if err := p.store.RequireWritable(ctx); err != nil {
		if errors.Is(err, errRouteNotWritable) {
			return nil, fmt.Errorf("the route DB refuses route writes, so the state of %d players was NOT published to the mapping Redis "+
				"(nothing was decided or written): the route writers are frozen for a rollback recovery — follow runbook §9.4 "+
				"(freeze → reconcile-from-cache → audit → unfreeze) and re-run only after unfreeze: %w", len(publishable), err)
		}
		return nil, fmt.Errorf("check route_meta.writable before publishing the route DB state of %d players "+
			"(nothing was published, decided or written): %w", len(publishable), err)
	}
	var frozen, others []mergeRouteRow
	for _, row := range publishable {
		if row.FrozenRunID != "" {
			frozen = append(frozen, row)
		} else {
			others = append(others, row)
		}
	}
	if len(others) > 0 {
		if _, err := p.pub.catchUpRoutes(ctx, others); err != nil {
			return nil, fmt.Errorf("publish the route DB state of %d players to the mapping Redis before acting on it "+
				"(nothing was decided or written; re-run once the mapping Redis confirms writes%s): %w", len(others),
				routePublishRollbackHint(err), err)
		}
	}
	if len(frozen) > 0 {
		if _, err := p.pub.publishRoutes(ctx, frozen); err != nil {
			return nil, fmt.Errorf("confirm the frozen placements of %d players in the mapping Redis before acting on them "+
				"(nothing was decided or written; re-run once the mapping Redis confirms writes%s): %w", len(frozen),
				routePublishRollbackHint(err), err)
		}
	}
	return out, nil
}

// routePublishRollbackHint 是发布被确定性拒绝(errRoutePublishRejected)时补进搬库错误文案的回档提示(R8-5,只改文案,
// 拒绝的判据不变);其余错误返回空串。被拒的行若缓存 rev 比本次发布的库 rev 新、且 data_service 报了写者冻结,
// 说明全局库被回档过,原样重跑没有用。正常运行中另一个写者在「读库」与「MGET」之间提交并发布也会让缓存 rev 更新,
// 所以只给提示、不据此断言回档(不改 decideRouteCatchUp 的口径)。
func routePublishRollbackHint(err error) string {
	if !errors.Is(err, errRoutePublishRejected) {
		return ""
	}
	return "; if a rejected row shows a cached rev ahead of the rev being published and data_service_route_writable is 0 " +
		"(or data_service_route_reconcile_event_total{event=~\"freeze_.*\"} rose), the route DB was rolled back — " +
		"follow runbook §9.4 instead of re-running"
}

// readDB 读库并按 ids 顺序编码。publishable 是本次读到、可以发布的行(去重;库里没有的、两列都为 NULL 的、落点三列不自洽的
// 不在其中 —— 后两种的读结果是「没有 home / 畸形记录」,decideFreeze 一律跳过,不需要也不能发布)。
func (p sqlRelocatePlacements) readDB(ctx context.Context, ids []uint64) ([]placementRead, []mergeRouteRow, error) {
	out := make([]placementRead, len(ids))
	if len(ids) == 0 {
		return out, nil, nil
	}
	rows, err := p.store.ReadRoutes(ctx, ids)
	if err != nil {
		return nil, nil, fmt.Errorf("read player_route (%d players): %w", len(ids), err)
	}
	seen := make(map[uint64]bool, len(ids))
	var publishable []mergeRouteRow
	for i, id := range ids {
		row, ok := rows[id]
		out[i] = routePlacementRead(row, ok)
		if !ok || seen[id] {
			continue
		}
		seen[id] = true
		if _, perr := row.placementValue(); perr == nil && (row.HomeZone != 0 || row.StorageID != 0) {
			publishable = append(publishable, row)
		}
	}
	return out, publishable, nil
}

func (p sqlRelocatePlacements) cas(ctx context.Context, ops []placementCAS) ([]placementCASResult, error) {
	if len(ops) == 0 {
		return nil, nil
	}
	if p.readOnly {
		return nil, errors.New("internal: placement CAS on a read-only (dry-run) route backend")
	}
	out := make([]placementCASResult, 0, len(ops))
	for start := 0; start < len(ops); start += routeWriteBatchLimit {
		chunk := ops[start:min(start+routeWriteBatchLimit, len(ops))]
		results, err := p.store.RelocateCAS(ctx, chunk)
		if err != nil {
			return out, fmt.Errorf("relocate CAS in the route DB (%d players; nothing of this batch was committed): %w", len(chunk), err)
		}
		if len(results) != len(chunk) {
			return out, fmt.Errorf("%w: relocate CAS returned %d results for %d ops", errRouteInvariant, len(results), len(chunk))
		}
		firstWritten := len(results)
		var written []mergeRouteRow
		for i, res := range results {
			if res.Outcome != placementCASWritten {
				continue
			}
			if firstWritten == len(results) {
				firstWritten = i
			}
			written = append(written, res.Row)
		}
		if len(written) > 0 {
			if _, err := p.pub.publishRoutes(ctx, written); err != nil {
				for _, res := range results[:firstWritten] {
					out = append(out, res.placementCASResult)
				}
				return out, fmt.Errorf("publish %d placement change(s) committed in the route DB but NOT confirmed in the mapping Redis "+
					"(go/db sees either the previous placement or this one — both are safe; this one is just not confirmed durable "+
					"yet — and a resume publishes them again before acting on them%s): %w",
					len(written), routePublishRollbackHint(err), err)
			}
		}
		for _, res := range results {
			out = append(out, res.placementCASResult)
		}
	}
	return out, nil
}

// relocateRouteBackend 是按 -route-source 选出的搬库落点读写(规格 §6.3 W11 / R8)。
type relocateRouteBackend struct {
	places relocatePlacements // R1 / R5 / 解冻 / 撤销(db 源下读之前先发布确认)
	reads  routeReadSide      // R0 的整库枚举(-relocate-player-ids 留空时)与逐人读(只读、不发布)
	store  *mergeRouteStore   // 非 nil ⇔ -route-source=db;调用方 Close
}

// openRelocateRouteBackend 核对库与 mapping Redis 成对之后装出落点读写(openRouteBackend:redis 时见到水位键即拒,
// db 时要求水位键世代等于库世代,规格 §13.3 第 3 条)。runID 进写者标识(player_route.updated_by)。
// readOnly(dry-run)时 db 源的 read 不发布、cas 拒绝;redis 源不受影响(dry-run 本来就不调 cas)。
//
// 不并进 openRunRouteBackend(merge_run.go;交接 handoff-d1-remaining-20261019 §2.1 末尾要求两套入口「能合并就合并」,
// 2026-10-20 评审 R8-6 / MZ2-7 复核后只合并了库读 helper,入口保留三个,理由如下):三个入口共用的只有 openRouteBackend
// (成对核对 + 探针),那一步已经只有一份;各自剩下的几行恰好是彼此不同的部分 —— durability(这里 dry-run 也按 -waitaof-* 装,
// 合服只在写路径装,只读读方 openRouteReadSide 用零值)、写者标识(这里是搬库 run_id,合服是合服 run_id,读方是用途名)、
// 日志(合服按「先写库再发布」说)与交出的形状(这里要具体的 *mergeRouteStore 装 sqlRelocatePlacements 与 routeReadSideDB,
// 合服交 routeDBOps,它不含 RelocateCAS / ScanByStorage)。合成一个就得加 readOnly / 写者 / 日志开关再在调用方拆开,
// 并让 routeDBOps 的全部测试替身补方法;在未编译阶段这比三段十来行的装配更容易出错。
func openRelocateRouteBackend(ctx context.Context, ro routeEntryOptions, mapRdb *redis.Client, runID string, readOnly bool) (relocateRouteBackend, error) {
	dur := routeDurability{}
	if ro.source == routeSourceDB {
		d, err := newRouteDurability(ro.waitAOFLocal, ro.waitAOFReplicas, ro.waitAOFTimeoutMs)
		if err != nil {
			return relocateRouteBackend{}, err
		}
		dur = d
	}
	host, _ := os.Hostname()
	pub := newRoutePublisher(mapRdb, dur)
	st, err := openRouteBackend(ctx, ro.source, ro.dbDSN, newMergeRouteWriter(host, runID), pub)
	if err != nil {
		return relocateRouteBackend{}, err
	}
	if st == nil {
		return relocateRouteBackend{places: redisRelocatePlacements{rdb: mapRdb}, reads: routeReadSideRedis{rdb: mapRdb}}, nil
	}
	return relocateRouteBackend{
		places: sqlRelocatePlacements{store: st, pub: pub, readOnly: readOnly},
		reads:  routeReadSideDB{db: st},
		store:  st,
	}, nil
}

func (b relocateRouteBackend) source() string {
	if b.store != nil {
		return routeSourceDB
	}
	return routeSourceRedis
}

func (b relocateRouteBackend) close() {
	if b.store != nil {
		_ = b.store.Close()
	}
}

// redisOrderingLocks 在 go/db 的 RedisClient(shared DB 0)上探测排序锁。按 pipeline 分批 EXISTS:
// 探测的是确定的一组键,不用 SCAN(同 countExistingKeys)。
type redisOrderingLocks struct{ rdb *redis.Client }

func (l redisOrderingLocks) exists(ctx context.Context, keys []string) ([]bool, error) {
	if l.rdb == nil {
		return nil, errors.New("nil shared (go/db) redis handle")
	}
	out := make([]bool, 0, len(keys))
	for start := 0; start < len(keys); start += mappingScanCount {
		batch := keys[start:min(start+mappingScanCount, len(keys))]
		pipe := l.rdb.Pipeline()
		cmds := make([]*redis.IntCmd, len(batch))
		for i, k := range batch {
			cmds[i] = pipe.Exists(ctx, k)
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return nil, fmt.Errorf("exists go/db ordering locks (%d keys): %w", len(batch), err)
		}
		for _, c := range cmds {
			out = append(out, c.Val() > 0)
		}
	}
	return out, nil
}

// relocateDeleteSQL 是 R3 的第一句:删 T 里该玩家的旧行(冷副本)。WHERE 只有完整主键等值:锁的是这一名玩家的
// 主键记录;RC 下行不存在时不加间隙锁。
func relocateDeleteSQL(dstQ string) string {
	return "DELETE FROM " + dstQ + " WHERE " + playerIDColumn + " = ?"
}

// relocateCopySQL 是 R3 的第二句:按列名对位从 S 拷该玩家的行到 T(cols 来自 alignedPlayerColumns:两库列集合相同、
// 列名不含反引号)。SELECT 侧按主键等值读 S,RC 下是一致性读,不加锁(同 copyOneTable 的说明)。
func relocateCopySQL(dstQ, srcQ string, cols []string) string {
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = "`" + c + "`"
	}
	list := strings.Join(quoted, ",")
	return "INSERT INTO " + dstQ + " (" + list + ") SELECT " + list + " FROM " + srcQ + " WHERE " + playerIDColumn + " = ?"
}

// byteIdenticalRowCondition 拼 R4 的「逐字节相同」条件:每列转成 BINARY 再 NULL-safe 比较。
// 与 identicalRowCondition(合服续跑 / 撤销删行用)的差别只在字符串列:按排序规则比较时大小写不同的两个值也算相等,
// 转成 BINARY 之后不算。今天的玩家表只有 BIGINT 与 MEDIUMBLOB,两者结果相同;将来 proto 加了字符串字段,
// R4 仍然是逐字节的。cols 的约束同 identicalRowCondition。
func byteIdenticalRowCondition(cols []string) string {
	conds := make([]string, 0, len(cols))
	for _, c := range cols {
		conds = append(conds, fmt.Sprintf("CAST(d.`%s` AS BINARY) <=> CAST(s.`%s` AS BINARY)", c, c))
	}
	return strings.Join(conds, " AND ")
}

// sqlRelocateRows 是 relocateRows 的生产实现。两条 R3 语句按表各预编译一次,事务里用 tx.StmtContext 复用。
//
// 锁序(死锁审计口径,docs/ops/incident-friend-lock-order-deadlock-2026-09-21.md):R3 的事务只碰一名玩家,
// 按表名升序、每张表一条主键等值 DELETE + 一条 INSERT,持有的是该玩家在 T 各表的主键记录;S 侧是不加锁的一致性读。
// go/db 对同一名玩家的写一次只锁一张表的一行、且在切换之前根本不会路由到 T(T 不是当前落点,R1 已证明),
// 两边不可能互相等待成环。R4 的比对全是非锁定读。
type sqlRelocateRows struct {
	closeOnce sync.Once
	closeErr  error
	db        *sql.DB
	srcQ      map[string]string // 表 → S 库全限定名
	dstQ      map[string]string
	tables    []string // 升序
	cols      map[string][]string
	delStmt   map[string]*sql.Stmt
	copyStmt  map[string]*sql.Stmt
}

func newSQLRelocateRows(ctx context.Context, db *sql.DB, srcSchema, dstSchema string, tables []string,
	cols map[string][]string) (*sqlRelocateRows, error) {
	r := &sqlRelocateRows{
		db: db, srcQ: map[string]string{}, dstQ: map[string]string{}, tables: append([]string(nil), tables...),
		cols: cols, delStmt: map[string]*sql.Stmt{}, copyStmt: map[string]*sql.Stmt{},
	}
	for _, t := range r.tables {
		if len(cols[t]) == 0 {
			return nil, errors.Join(fmt.Errorf("no aligned columns for table %s", t), r.Close())
		}
		r.srcQ[t] = srcSchema + "." + t
		r.dstQ[t] = dstSchema + "." + t
		del, err := db.PrepareContext(ctx, relocateDeleteSQL(r.dstQ[t]))
		if err != nil {
			return nil, errors.Join(fmt.Errorf("prepare delete on %s: %w", r.dstQ[t], err), r.Close())
		}
		r.delStmt[t] = del
		cp, err := db.PrepareContext(ctx, relocateCopySQL(r.dstQ[t], r.srcQ[t], cols[t]))
		if err != nil {
			return nil, errors.Join(fmt.Errorf("prepare copy %s → %s: %w", r.srcQ[t], r.dstQ[t], err), r.Close())
		}
		r.copyStmt[t] = cp
	}
	if err := registerMaintenanceResource(ctx, r); err != nil {
		return nil, errors.Join(err, r.Close())
	}
	return r, nil
}

// Close 释放预编译语句。
func (r *sqlRelocateRows) Close() error {
	r.closeOnce.Do(func() {
		for _, s := range r.delStmt {
			r.closeErr = errors.Join(r.closeErr, s.Close())
		}
		for _, s := range r.copyStmt {
			r.closeErr = errors.Join(r.closeErr, s.Close())
		}
	})
	return r.closeErr
}

func (r *sqlRelocateRows) copyPlayer(ctx context.Context, playerID uint64) (cold int, err error) {
	if err := checkMaintenanceTool(ctx); err != nil {
		return 0, err
	}
	finish, err := enterMaintenanceWork(ctx)
	if err != nil {
		return 0, err
	}
	returned := false
	defer func() {
		if !returned || err != nil {
			holdMaintenanceTool(ctx)
		}
		finish()
	}()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() {
		rollbackErr := tx.Rollback()
		if rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
		}
	}()
	for _, t := range r.tables {
		res, err := tx.StmtContext(ctx, r.delStmt[t]).ExecContext(ctx, playerID)
		if err != nil {
			return 0, fmt.Errorf("delete the old copy in %s: %w", r.dstQ[t], err)
		}
		n, affectedErr := res.RowsAffected()
		if affectedErr != nil {
			return 0, affectedErr
		}
		cold += int(n)
		if _, err := tx.StmtContext(ctx, r.copyStmt[t]).ExecContext(ctx, playerID); err != nil {
			return 0, fmt.Errorf("copy %s → %s: %w", r.srcQ[t], r.dstQ[t], err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	returned = true
	return cold, nil
}

func (r *sqlRelocateRows) mismatched(ctx context.Context, ids []uint64) ([]uint64, error) {
	bad := map[uint64]bool{}
	for _, t := range r.tables {
		srcMissing, err := idsWithoutRow(ctx, r.db, r.srcQ[t], ids)
		if err != nil {
			return nil, err
		}
		dstMissing, err := idsWithoutRow(ctx, r.db, r.dstQ[t], ids)
		if err != nil {
			return nil, err
		}
		same, err := byteIdenticalIDs(ctx, r.db, r.srcQ[t], r.dstQ[t], r.cols[t], ids)
		if err != nil {
			return nil, err
		}
		for _, id := range rowsDiffer(ids, srcMissing, dstMissing, same) {
			bad[id] = true
		}
	}
	var out []uint64
	for id := range bad {
		out = append(out, id)
	}
	return sortedUint64(out), nil
}

// rowsDiffer 是 R4 一张表上的纯判定:两边都没有行 = 相同;一边有一边没有 = 不同;两边都有 = 看逐字节比较。
func rowsDiffer(ids, srcMissing, dstMissing []uint64, identical map[uint64]bool) []uint64 {
	noSrc := make(map[uint64]bool, len(srcMissing))
	for _, id := range srcMissing {
		noSrc[id] = true
	}
	noDst := make(map[uint64]bool, len(dstMissing))
	for _, id := range dstMissing {
		noDst[id] = true
	}
	var out []uint64
	for _, id := range ids {
		switch {
		case noSrc[id] && noDst[id]:
		case noSrc[id] != noDst[id]:
			out = append(out, id)
		case !identical[id]:
			out = append(out, id)
		}
	}
	return out
}

// byteIdenticalIDs 返回 ids 里 S 与 T 都有行、且逐字节相同的玩家。非锁定读;分批 IN。
func byteIdenticalIDs(ctx context.Context, db *sql.DB, srcQ, dstQ string, cols []string, ids []uint64) (map[uint64]bool, error) {
	cond := byteIdenticalRowCondition(cols)
	out := map[uint64]bool{}
	for _, batch := range chunkUint64(sortedUint64(ids), playerRowsBatchSize) {
		rows, err := db.QueryContext(ctx, fmt.Sprintf("SELECT d.%s FROM %s d JOIN %s s ON d.%s = s.%s WHERE d.%s IN (%s) AND (%s)",
			playerIDColumn, dstQ, srcQ, playerIDColumn, playerIDColumn, playerIDColumn, inListLiteral(batch), cond))
		if err != nil {
			return nil, fmt.Errorf("compare %s vs %s: %w", dstQ, srcQ, err)
		}
		for rows.Next() {
			var id uint64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ── 参数 ─────────────────────────────────────────────────────────

// parsePlayerIDList 解析 -relocate-player-ids(逗号分隔的十进制 player_id),升序去重。
func parsePlayerIDList(raw string) ([]uint64, error) {
	var out []uint64
	for _, tok := range strings.Split(raw, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		id, err := strconv.ParseUint(tok, 10, 64)
		if err != nil || id == 0 {
			return nil, fmt.Errorf("-relocate-player-ids: %q is not a player id", tok)
		}
		out = append(out, id)
	}
	return sortedUint64(out), nil
}

// validateRelocateStorages 校验 S / T:都要给、不能相同。编号本身不限家族(zone 库之间、zone 库与全局库之间都可以搬)。
func validateRelocateStorages(s, t uint32) error {
	switch {
	case s == 0 || t == 0:
		return errors.New("-relocate-source-storage and -relocate-target-storage are required (non-zero storage ids)")
	case s == t:
		return fmt.Errorf("-relocate-source-storage and -relocate-target-storage are both %d", s)
	}
	return nil
}

// checkRelocateResume 核对续跑时的参数与清单一致(S / T / 表集合 / topic 世代号),且清单没被撤销过。
func checkRelocateResume(m *relocateManifest, s, t uint32, tables []string, generation uint32, ids []uint64) error {
	if m.Aborted {
		return fmt.Errorf("run %s was aborted with -mode relocate-abort; start a new relocation with a new -manifest-path", m.RunID)
	}
	if m.SourceStorage != s || m.TargetStorage != t {
		return fmt.Errorf("the manifest is for storage %d → %d but this run is %d → %d", m.SourceStorage, m.TargetStorage, s, t)
	}
	if m.TopicGeneration != generation {
		return fmt.Errorf("the manifest waited on db_task topics of generation %d but this run uses -kafka-topic-generation %d",
			m.TopicGeneration, generation)
	}
	if err := checkResumeTables(m.Tables, tables); err != nil {
		return err
	}
	if len(ids) > 0 {
		have := make([]uint64, len(m.Players))
		for i, p := range m.Players {
			have[i] = p.PlayerID
		}
		if extra, missing := uint64sNotIn(ids, have), uint64sNotIn(have, ids); len(extra) > 0 || len(missing) > 0 {
			return fmt.Errorf("-relocate-player-ids differs from the manifest's players (not in manifest: %v; not in flag: %v) — "+
				"a resumed run takes its players from the manifest; use a new -manifest-path for a new set",
				sampleUint64(extra), sampleUint64(missing))
		}
	}
	return nil
}

// relocateRouteSourceRefusal 核对搬库清单记录的映射来源与本次 -route-source(B2-1;续跑与 -mode relocate-abort 共用,纯函数):
// 一致回 nil;不一致、或清单记录了不认识的值,一律拒绝 —— 冻结 / 切换写在哪一边,认领与解冻就只能在哪一边做。判定本身就是
// checkManifestRouteSource(与合服共用的那一份),这里只把拒绝文案换成按方向给出的出路。调用方在打开映射接缝之前调用,拒绝时
// 什么都没读、没写。
//
// 撤销也拒绝、不按「此刻的真源」放行(10-22 协调结论):db 源的 R1 已在库里提交、发布没确认,随后环境回退成 redis 模式且没按库
// 重灌缓存 —— 用 redis 源撤销会把这些人记成 skipped、把清单标成 Aborted,而库里的冻结从此成了没有任何清单认领的孤儿。
//
// 出路(都在拒绝文案里,TestRelocateRouteSourceRefusal 钉住):用清单记录的来源重跑同一条命令;那个来源过不了成对核对
// (openRelocateRouteBackend → openRouteBackend → decideRoutePairing,在任何读写之前)时按报错分两种:
//   - 清单是 db、报水位键不在:先按 runbook §15.2 第一行排查(等对账 leader 一拍、核对 RouteStore.Mode 与 -mapping-redis-addr)。
//     环境确实已回退成 redis 模式时,水位键只有 db_first 的对账 leader(或 db_first 下的 route_tool unfreeze)会写,
//     rewarm 只删不建 —— 本工具没有出路,停下升级;这批人保持冻结、go/db 延后他们的写,不丢数据。
//   - 清单是 redis(含没有这个字段的旧清单)、报水位键存在:环境已切到 db_first,同样没有出路,停下升级;不要 DEL 水位键
//     (§15.2:那等于关掉回档检测)。
//
// 这两种都是上线切换 / 回退跨过了一次没收尾的搬库(提案 §13.4 P1~P4 禁止 merge_zone 写模式);负责人决定前不加放行开关。
func relocateRouteSourceRefusal(recorded, current, manifestPath string) error {
	err := checkManifestRouteSource("relocate", recorded, current)
	got := recordedRouteSource(recorded)
	if err == nil || (got != routeSourceDB && got != routeSourceRedis) {
		return err
	}
	head := fmt.Sprintf("the relocate manifest was written with -route-source %s but this run uses -route-source %s", got, current)
	if recorded == "" {
		head = fmt.Sprintf("the relocate manifest has no route_source (written by the Redis-only tool, i.e. -route-source redis) but "+
			"this run uses -route-source %s", current)
	}
	keep := "Until then the frozen players stay frozen: go/db defers their writes, nothing is lost"
	if got == routeSourceDB {
		return fmt.Errorf("%s. This run's freezes and switches were committed in the global route DB (player_route.frozen_run_id): "+
			"resume it, or give it up with -mode relocate-abort -manifest-path %s -apply, ONLY with -route-source db -route-db-dsn "+
			"<the global route DB> (the same route arguments as the relocation). If that is refused with %q, follow runbook §15.2 "+
			"(wait one reconcile-leader tick, check RouteStore.Mode and -mapping-redis-addr); if data_service has been rolled back to "+
			"RouteStore.Mode=redis while this run was half done, no command of this tool can finish or undo it (only a db_first "+
			"reconcile leader re-creates that key; rewarm only drops it) — stop and escalate (runbook §15). %s. Never give it up "+
			"with -route-source redis: players whose freeze is only in the DB would be recorded as skipped and the manifest marked "+
			"aborted, leaving those DB freezes orphaned", head, manifestPath, routeWatermarkKey+" is missing", keep)
	}
	return fmt.Errorf("%s. This run froze its players in the mapping Redis only: resume it, or give it up with -mode relocate-abort "+
		"-manifest-path %s -apply, ONLY with -route-source redis (no -route-db-dsn). If that is refused with %q, data_service is "+
		"already on RouteStore.Mode=db_first — the rollout crossed P1~P4 (which forbid merge_zone writes) while this run was half "+
		"done, and no command of this tool can finish or undo it: stop and escalate (runbook §15); do NOT delete %s (runbook §15.2: "+
		"that disables rollback detection). %s", head, manifestPath, routeWatermarkKey+" exists on this mapping Redis",
		routeWatermarkKey, keep)
}

// ── 入口 ─────────────────────────────────────────────────────────

func runRelocateEntry(o options) {
	ctx, cancel := context.WithTimeout(maintenanceParent(o), o.timeout)
	defer cancel()
	if !o.dryRun {
		if err := checkMaintenanceTool(ctx); err != nil {
			log.Fatal(err)
		}
	}
	s, t := o.relocateSource, o.relocateTarget
	now := time.Now()
	manifestPath := o.manifestPath
	if manifestPath == "" {
		manifestPath = defaultRelocateManifestPath(s, t, now)
	}
	srcSchema, _ := storeDBName(s)
	dstSchema, _ := storeDBName(t)
	log.Printf("=== Relocate player main data %s → %s (dry-run=%v apply=%v batch=%d lock_wait=%s) ===",
		srcSchema, dstSchema, o.dryRun, o.apply, o.relocateBatch, o.relocateLockWait)
	log.Printf("    manifest=%s capability_zones=%v topic_generation=%d route_source=%s", manifestPath, o.capSpec.zones,
		o.kafkaTopicGen, o.routeSource)

	db := mustOpenMySQL(ctx, o.mysqlDSN)
	defer closeUnownedMaintenanceResource(ctx, db)
	mapRdb := mustDial(ctx, "mapping", o.mappingAddr, o.mappingPwd, o.mappingDB)
	defer closeUnownedMaintenanceResource(ctx, mapRdb)
	// go/db 的排序锁与重试队列在它的 RedisClient 上:shared DB 0(-notice-redis-*)。
	sharedRdb := mustDial(ctx, "login/shared(DB0, go/db ordering locks)", o.noticeAddr, o.noticePwd, o.noticeDB)
	defer closeUnownedMaintenanceResource(ctx, sharedRdb)

	// ── R0 ─────────────────────────────────────────────────
	for _, schema := range []string{srcSchema, dstSchema} {
		if err := assertSchemaExists(ctx, db, schema); err != nil {
			log.Fatalf("R0: %v (both placement stores must live on the MySQL instance behind -mysql-dsn; "+
				"create a player store with go/db `migrate -storage-id <id> -command up -create-database`)", err)
		}
	}
	candidates, err := loadTableListJSON(o.tableListPath)
	if err != nil {
		log.Fatalf("R0: %v (pass -table-list-json <path to generated/data/mysql_database_table_list.json>)", err)
	}
	tables, err := discoverPlayerTables(ctx, db, srcSchema, candidates)
	if err != nil {
		log.Fatalf("R0: %v", err)
	}
	if len(tables) == 0 {
		log.Fatalf("R0: no table in %s has a %s column (candidates: %v)", srcSchema, playerIDColumn, candidates)
	}
	cols := make(map[string][]string, len(tables))
	for _, table := range tables {
		c, err := alignedPlayerColumns(ctx, db, srcSchema, dstSchema, table)
		if err != nil {
			log.Fatalf("R0: %v", err)
		}
		cols[table] = c
	}
	log.Printf("R0 OK: %s and %s exist; player tables aligned by column name: %v", srcSchema, dstSchema, tables)

	flagIDs, err := parsePlayerIDList(o.relocatePlayerIDs)
	if err != nil {
		log.Fatalf("%v", err)
	}
	m, err := loadRelocateManifest(manifestPath)
	if err != nil {
		log.Fatalf("load relocate manifest: %v", err)
	}
	resumed := m != nil
	// run_id 在开库之前定下来:它进写者标识(player_route.updated_by)。续跑沿用清单的;首跑现生成,没东西可搬时丢弃。
	runID := newRelocateRunID(s, t, now)
	if resumed {
		if err := checkRelocateResume(m, s, t, tables, uint32(o.kafkaTopicGen), flagIDs); err != nil {
			log.Fatalf("manifest %s: %v", manifestPath, err)
		}
		// 续跑与撤销都只认清单记录的来源(B2-1,出路见 relocateRouteSourceRefusal);在打开映射接缝之前,拒绝时什么都没读写。
		if err := relocateRouteSourceRefusal(m.RouteSource, o.routeSource, manifestPath); err != nil {
			log.Fatalf("manifest %s: %v", manifestPath, err)
		}
		runID = m.RunID
	}
	if !validRunID(runID) {
		log.Fatalf("internal: run_id %q cannot appear in a frozen placement value", runID)
	}
	// 落点读写按 -route-source 选(db:全局库为真源,读之前先发布确认;redis:今天的路径),先核对库与 mapping Redis 成对。
	backend, err := openRelocateRouteBackend(ctx, routeEntryOptionsOf(o), mapRdb, runID, o.dryRun)
	if err != nil {
		log.Fatalf("R0: route source %q: %v", o.routeSource, err)
	}
	if maintenanceFromContext(ctx) == nil {
		defer backend.close()
	}
	log.Printf("R0 OK: player placements are read and written through %s", backend.source())
	if resumed {
		log.Printf("RESUME: run_id=%s — %s (players come from the manifest; nothing is re-enumerated)", m.RunID, m.relocSummary())
	} else {
		players := flagIDs
		if len(players) == 0 {
			sp, err := backend.reads.storagePlayers(ctx, s)
			if err != nil {
				log.Fatalf("R0: enumerate players placed on storage %d: %v", s, err)
			}
			players = sp.all()
			log.Printf("R0: storage %d holds %d players (%d by placement record, %d of them frozen; %d by home_zone without a record); "+
				"%d malformed placement records (%s) are NOT included", s, len(players), len(sp.ByRecord), sp.Frozen,
				len(sp.ByHome), len(sp.Malformed), backend.source())
		}
		if len(players) == 0 {
			log.Printf("Nothing to relocate: no player is placed on storage %d. No manifest written.", s)
			return
		}
		m = newRelocateManifest(runID, s, t, uint32(o.kafkaTopicGen), tables, players, operatorTag(), now)
		m.RouteSource = o.routeSource
	}

	// 能力标记(§4.3):-db-capability-zones 列出的每个 zone 都必须有,缺一个就整批拒绝(设计 R0)。
	// 另外把未到终态的清单玩家此刻的 home_zone 也核一遍 —— 处理他们存盘的正是 home 所在 zone 的 go/db,
	// 运维列漏了也不能放过。列表之外、没有标记的 home zone(例如早已合走、没人在跑的区)不整批拒绝:
	// 那些 zone 不进 capZones,R1 把 home 在那里的人跳过(decideFreeze),其余照常搬。
	if err := checkPlacementCapabilities(ctx, mapRdb, o.capSpec.zones); err != nil {
		log.Fatalf("R0: %v. Upgrade and restart go/db in those zones (it writes the marker on start-up) before relocating; "+
			"-db-capability-zones must list every zone whose go/db is running", err)
	}
	var todo []uint64
	for _, p := range m.Players {
		if !relocFinal(p.State) {
			todo = append(todo, p.PlayerID)
		}
	}
	// 与 R1 同一口径的读,不发布(规格 §13.3 第 6 条:R0 以前绕过了接缝直读 Redis;db 源读库、redis 源读 mapping Redis,
	// 值串与 R1 的读逐字节相同)。这里的结果只用来补查 home zone 的能力标记和 dry-run 预览,依赖缓存的决定都在引擎
	// freeze() / abort() 的那次 read 里,发布确认只在那里做 —— R0 因此在清单落盘之前一个字节都不写(R8-7)。
	reads, err := backend.reads.placements(ctx, todo)
	if err != nil {
		log.Fatalf("R0: read placements: %v", err)
	}
	if len(reads) != len(todo) {
		log.Fatalf("R0: placement read returned %d answers for %d players", len(reads), len(todo))
	}
	capSet := map[uint32]bool{}
	for _, z := range o.capSpec.zones {
		capSet[z] = true
	}
	extra := map[uint32]bool{}
	for _, rd := range reads {
		if rd.HomePresent && !capSet[rd.Home] {
			extra[rd.Home] = true
		}
	}
	if len(extra) > 0 {
		extraZones := sortedZones(extra)
		vals, err := readCapabilityMarkers(ctx, mapRdb, extraZones)
		if err != nil {
			log.Fatalf("R0: %v", err)
		}
		confirmed := capabilityConfirmed(extraZones, vals)
		var unconfirmed []uint32
		for _, z := range extraZones {
			if confirmed[z] {
				capSet[z] = true
			} else {
				unconfirmed = append(unconfirmed, z)
			}
		}
		if len(unconfirmed) > 0 {
			log.Printf("WARN: home zones %v are outside -db-capability-zones and carry no %s marker: players whose home is "+
				"there will be skipped — the go/db that writes them cannot be shown to route by placement",
				unconfirmed, capabilityRoutingV1)
		}
	}
	capZones := sortedZones(capSet)
	m.CapabilityZones = capZones
	log.Printf("R0 OK: %s confirmed in zones %v", capabilityRoutingV1, capZones)

	if o.dryRun {
		previewRelocation(m, reads, todo, capSet, manifestPath)
		return
	}
	if !resumed {
		if err := saveRelocateManifest(manifestPath, m); err != nil {
			log.Fatalf("write the relocate manifest before the first write (nothing was written): %v", err)
		}
		log.Printf("Manifest written to %s BEFORE any write (run_id=%s, %d players). Keep it: resume and -mode relocate-abort need it.",
			manifestPath, m.RunID, len(m.Players))
	}

	rows, err := newSQLRelocateRows(ctx, db, srcSchema, dstSchema, tables, cols)
	if err != nil {
		log.Fatalf("R0: %v", err)
	}
	defer closeUnownedMaintenanceResource(ctx, rows)
	save := func(m *relocateManifest) error { return saveRelocateManifest(manifestPath, m) }
	engine := newRelocateEngine(m, save, backend.places, redisOrderingLocks{rdb: sharedRdb}, rows,
		capSet, o.relocateBatch, o.relocateLockWait)
	runErr := engine.run(ctx)
	// 出错路径上内存里的状态也可能已经推进(解冻了一部分),最后再落一次盘。
	if err := save(m); err != nil {
		holdMaintenanceTool(ctx)
		log.Printf("ERROR: final manifest save failed: %v — the manifest on disk may lag behind the %s placements; a resume adopts "+
			"this run's freezes and switches from there", err, backend.source())
	}
	log.Printf("=== Relocate %s → %s: %s ===", srcSchema, dstSchema, m.relocSummary())
	if runErr != nil {
		// db 源多一句(R8-8):R1 冻结已在库里提交、发布没确认的那批人,缓存是未冻结(go/db 照写 S)或未确认的冻结值(go/db 延后写,
		// B2-6:EVAL 执行了、WAITAOF 份数不足)—— 他们的数据都还没拷,所以「nothing is lost」仍然成立;续跑先确认冻结再拷。
		// redis 源文案逐字不变。
		dbNote := ""
		if backend.source() == routeSourceDB {
			dbNote = "; players whose freeze was committed in the route DB but not confirmed in the mapping Redis are either still " +
				"written to S by go/db or already deferred, and were not copied (the resume confirms their freeze before copying)"
		}
		log.Fatalf("relocate stopped: %v. Players frozen by run_id=%s stay frozen (go/db defers their writes, nothing is lost; "+
			"db_placement_guard_total{outcome=\"frozen_deferred\"} keeps rising): resume with the same command, or give up with "+
			"-mode relocate-abort -manifest-path %s -apply. With -route-source=db a change committed in the route DB but not "+
			"confirmed in the mapping Redis is published by the resume before it is acted on%s", runErr, m.RunID, manifestPath, dbNote)
	}
	if err := reportRelocateOutcome(m, manifestPath); err != nil {
		normalToolFailure(ctx, err)
	}
}

// reportRelocateOutcome 打收尾摘要。不是所有人都切换成功时 exit 1:没切过去的人要么留在 S(skipped / unfrozen),
// 要么需要人看(cas_failed),都不能当成「搬完了」。要再搬用新的 -manifest-path 另起一次(终态不会被续跑重做)。
func reportRelocateOutcome(m *relocateManifest, manifestPath string) error {
	c := m.relocStateCounts()
	for _, state := range []string{relocCASFailed, relocUnfrozen, relocSkipped} {
		if c[state] > 0 {
			log.Printf("%d players %s (first: %v)", c[state], state, m.relocSample(state))
		}
	}
	if c[relocSwitched] == len(m.Players) {
		log.Printf("All %d players now live in storage %d. The rows left in storage %d are cold copies (P-1); "+
			"`-mode storage-audit -storage %d` shows what still points there.", len(m.Players), m.TargetStorage, m.SourceStorage, m.SourceStorage)
		return nil
	}
	log.Printf("NOT all players were relocated (manifest %s). Players left in storage %d keep working there; relocate them with "+
		"a new -manifest-path once the cause is fixed. cas_failed players need a human: their placement record changed under this run.",
		manifestPath, m.SourceStorage)
	return fmt.Errorf("relocate部分玩家未终局，保留原manifest")
}

// previewRelocation 是 dry-run:按 R1 的判定分类(不冻结、不拷贝),预览写到 <path>.dryrun.json。
func previewRelocation(m *relocateManifest, reads []placementRead, todo []uint64, capSet map[uint32]bool, manifestPath string) {
	byID := make(map[uint64]placementRead, len(todo))
	for i, id := range todo {
		byID[id] = reads[i]
	}
	would, skipped := 0, 0
	preview := *m
	preview.DryRun = true
	preview.Players = append([]relocatePlayer(nil), m.Players...)
	for i := range preview.Players {
		p := &preview.Players[i]
		if relocFinal(p.State) {
			continue
		}
		d := decideFreeze(*p, byID[p.PlayerID], m.SourceStorage, m.TargetStorage, m.RunID, capSet)
		switch d.kind {
		case freezeSkip:
			skipped++
			p.Reason = "would skip: " + d.reason
		default:
			would++
			p.Reason = "would freeze and relocate"
		}
	}
	path := dryRunManifestPath(manifestPath)
	if err := saveRelocateManifest(path, &preview); err != nil {
		log.Printf("WARN: write dry-run preview %s: %v", path, err)
	} else {
		log.Printf("DRY-RUN: preview written to %s (for humans only; -mode relocate / relocate-abort refuse to read it)", path)
	}
	log.Printf("DRY-RUN: %d players would be frozen and relocated %d → %d, %d would be skipped. Nothing was written.",
		would, m.SourceStorage, m.TargetStorage, skipped)
}

func runRelocateAbortEntry(o options) {
	ctx, cancel := context.WithTimeout(maintenanceParent(o), o.timeout)
	defer cancel()
	if !o.dryRun {
		if err := checkMaintenanceTool(ctx); err != nil {
			log.Fatal(err)
		}
	}
	m, err := loadRelocateManifest(o.manifestPath)
	if err != nil {
		log.Fatalf("load relocate manifest: %v", err)
	}
	if m == nil {
		log.Fatalf("relocate manifest %s does not exist", o.manifestPath)
	}
	if (o.relocateSource != 0 && o.relocateSource != m.SourceStorage) || (o.relocateTarget != 0 && o.relocateTarget != m.TargetStorage) {
		log.Fatalf("the manifest is for storage %d → %d but the flags say %d → %d",
			m.SourceStorage, m.TargetStorage, o.relocateSource, o.relocateTarget)
	}
	// 解冻只能在冻结写下的那一边做(B2-1:来源不一致即拒绝,拒绝文案给出路,理由见 relocateRouteSourceRefusal)。
	if err := relocateRouteSourceRefusal(m.RouteSource, o.routeSource, o.manifestPath); err != nil {
		log.Fatalf("manifest %s: %v", o.manifestPath, err)
	}
	log.Printf("=== Relocate ABORT run_id=%s (%d → %d, dry-run=%v): %s ===", m.RunID, m.SourceStorage, m.TargetStorage,
		o.dryRun, m.relocSummary())
	log.Printf("    Make sure the relocate process of this run has exited: both would rewrite the same manifest.")
	mapRdb := mustDial(ctx, "mapping", o.mappingAddr, o.mappingPwd, o.mappingDB)
	defer closeUnownedMaintenanceResource(ctx, mapRdb)
	// 与 relocate 同一个接缝:db 源下解冻写库、发布确认之后才算解冻;dry-run 只读库、不发布。
	backend, err := openRelocateRouteBackend(ctx, routeEntryOptionsOf(o), mapRdb, m.RunID, o.dryRun)
	if err != nil {
		log.Fatalf("route source %q: %v", o.routeSource, err)
	}
	if maintenanceFromContext(ctx) == nil {
		defer backend.close()
	}
	log.Printf("    player placements are read and written through %s", backend.source())

	engine := newRelocateEngine(m, nil, backend.places, nil, nil, nil, max(o.relocateBatch, 1), 0)
	rep, err := engine.abort(ctx, o.dryRun)
	if o.dryRun {
		if err != nil {
			log.Fatalf("relocate-abort dry-run: %v", err)
		}
		log.Printf("DRY-RUN: %s. Nothing was written.", rep)
		return
	}
	m.Aborted = true
	if serr := saveRelocateManifest(o.manifestPath, m); serr != nil {
		holdMaintenanceTool(ctx)
		log.Printf("ERROR: save the aborted manifest: %v", serr)
	}
	if err != nil {
		log.Fatalf("relocate-abort stopped: %v (%s) — re-run it; unfreezing is idempotent", err, rep)
	}
	log.Printf("=== Relocate aborted: %s — %s ===", rep, m.relocSummary())
	if rep.Failed > 0 {
		log.Printf("%d players could not be unfrozen: their record is no longer this run's frozen value (first: %v)",
			rep.Failed, m.relocSample(relocCASFailed))
		normalToolFailure(ctx, fmt.Errorf("relocate-abort仍有%d玩家不能解冻", rep.Failed))
		return
	}
}
