package routetool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"data_service/internal/store"

	"shared/placement"
)

// audit(只读):比对全局库路由表与 mapping Redis,分类计数差异。
//
//  1. 方向 A(库对 Redis):ScanRoutes 按聚簇键 (route_shard, player_id) 键集翻页(G1a;明细因此不按 player_id 升序),
//     每页一次 MGET 三族键,逐名 classifyRoute。库里有错片行(route_shard ≠ player_id % 64)时 ScanRoutes 报错、
//     本次审计退出码 2 —— 方向 A 是真库上唯一看得见错片行的检测器,点读(GetRoutes)看不见它;
//  2. 方向 B(Redis 对库):SCAN player:zone / player:placement / player:maprev 三族键,用集合去重
//     (一名玩家在多族键里只算一次);**方向 A 没比过的** id 才 GetRoutes 并比对 —— 既包括库里没有行的,
//     也包括方向 A 的键集翻页扫过之后才提交的行(db_first 下审计期间新登记的玩家)。后者若按「库里有行就跳过」,
//     这一行在本次审计里就完全没比对过:只漏报不误报,但「audit 为 0」就不覆盖它了;
//  3. merged_into:库的全量列表对 Redis 的全量列表;
//  4. 复核:有差异的玩家不超过 MaxRecheck 时,等 RecheckDelay 后重读这些 id 的两边,只有两次都在的差异才计入,
//     被复核清掉的记 transient_cleared;超过上限就全部照首读计入并打 WARN。
//
// 分类(按玩家、按种类各计一次;一名玩家可以同时落进多类):
//
//	db_only_unpublished  库里这一行不是墓碑,而三族键**全部**不在。W1 已提交未发布的孤儿行与「Redis 丢了已确认的发布」
//	                     第 1a 批分不开,都要等第 1b 批的全量预热补齐,所以单独计数(它也算差异,退出码 1)
//	missing_in_redis     库里这一列有值,Redis 缺这把键(三族键不是全部缺席)
//	missing_in_db        库里这一列是 NULL(或根本没有这一行),Redis 有值
//	value_mismatch       两边都有,值不同(落点按 Encode 后的字符串比,冻结态与稳定态视为不同)
//	redis_malformed      Redis 的键名或值不规范(含 maprev、merged_into)
//	db_invalid           库里这一行过不了 RouteRow.Placement() 的校验
//	redis_rev_ahead      maprev 合法且大于库里的 rev:库被回档的信号(提案 M3)
//
// 墓碑行(home 与落点都是 NULL)视为没有记录。maprev 缺席不算差异:第 1a 批 redis 模式下没有任何写者写它(-require-maprev 除外,见下)。
//
// 冻结期恢复口径(-frozen-recovery,2026-10-18 第 1b-1 批):全局库回档后的恢复流程 freeze → reconcile-from-cache → audit → unfreeze
// 里,反向修库只有「缓存 → 库」一个方向,冻结期间也没有人把库推回缓存,于是库里独有的行(回档前发布失败的孤儿行、回档后到冻结前
// 提交而发布失败的行)一定计成 db_only_unpublished,audit 永远到不了 0。这个口径下 db_only_unpublished 照常计数、照常打明细,
// 但**不计入退出码**;其余类别必须为 0。这些行解冻之后由第 1b-2 批 leader 的增量推送补到缓存(1b-2 之前仍只能等 audit 人工核对)。
// 只在冻结时才有意义(可写时孤儿行与「Redis 丢了已确认的发布」分不开),是否冻结由入口(routetoolcli)先查。
//
// 严格 maprev 口径(-require-maprev,2026-10-19 第 1b-2 批收尾,规格 spec_D1_batch1b2_2_3.md §4.6 第 2 项):提案 §7 P3
// 「挂就绪标记之前 audit 为 0」的门禁。打开时多一类差异(默认关,上面「maprev 缺席不算差异」的口径与统计行逐字不变):
//
//	redis_rev_behind     库行不是墓碑,缓存里至少有它的一把键,而 player:maprev 缺席或小于库 rev。值相同也算:
//	                     发布脚本按 maprev 做 set-if-newer,缺 maprev 的键会被遗留值守卫挡住(值不同时回 legacy_conflict),
//	                     maprev 落后的键会让下一次发布判错新旧 —— 挂标记之前必须由 leader 的推送(或二次导入)补齐
//
// 三族键全部不在的行已计 db_only_unpublished(它的 maprev 当然也缺),不再重复计成 redis_rev_behind。
// 与 -frozen-recovery 同时给时两个口径各管各的:-frozen-recovery 只豁免 db_only_unpublished,redis_rev_behind 照常计入退出码。
// 冻结期一般到不了 0(反向修库之后库 rev 领先缓存的行要等解冻后 leader 的增量推送补 maprev),所以两者同用只适合核对,不作门禁。

// AuditOptions 是 audit 的参数(cmd/route_tool 的命令行一一对应)。
type AuditOptions struct {
	// Batch 是一页 ScanRoutes / GetRoutes 的行数、SCAN 的 COUNT 提示、一次 MGET 的玩家数,[1, store.RouteBatchLimit]。
	Batch int
	// MaxReport 是 stderr 明细行的上限;计数始终精确。
	MaxReport int
	// RecheckDelay 是复核前的等待,给正在途中的 W1(提交 → 发布)走完的时间。
	RecheckDelay time.Duration
	// MaxRecheck 是复核的玩家数上限;首读有差异的玩家超过它就不复核、全部计入。
	MaxRecheck int
	// Target 非空时作为 stderr 第一行(连接目标,只含 host 与库名)。
	Target string
	// Stdout 只写一行统计;Stderr 写明细与告警。
	Stdout io.Writer
	Stderr io.Writer
	// Sleep 是复核前的等待;nil = 按 RecheckDelay 真睡(可被 ctx 取消)。单测注入它,在等待期间改两边的数据。
	Sleep func(ctx context.Context, d time.Duration) error
	// FrozenRecovery 是冻结期恢复口径(见文件头):db_only_unpublished 不计入退出码。调用方保证此时映射写者已冻结。
	FrozenRecovery bool
	// RequireMapRev 是严格 maprev 口径(见文件头):多计 redis_rev_behind,计入 Differences 与退出码。默认关。
	RequireMapRev bool
}

func (o AuditOptions) validate() error {
	if o.Batch < 1 || o.Batch > store.RouteBatchLimit {
		return fmt.Errorf("batch %d not in [1, %d]", o.Batch, store.RouteBatchLimit)
	}
	if o.MaxReport < 0 {
		return fmt.Errorf("max-report %d must not be negative", o.MaxReport)
	}
	if o.RecheckDelay < 0 {
		return fmt.Errorf("recheck-delay %s must not be negative", o.RecheckDelay)
	}
	if o.MaxRecheck < 0 {
		return fmt.Errorf("max-recheck %d must not be negative", o.MaxRecheck)
	}
	if o.Stdout == nil || o.Stderr == nil {
		return errors.New("stdout / stderr writers are required")
	}
	return nil
}

// AuditReport 是一次 audit 的计数。
type AuditReport struct {
	DBRows       int // 方向 A 扫到的库行数(含墓碑)
	RedisPlayers int // 方向 B 在三族键里看到的不同 player_id 数

	DBOnlyUnpublished int
	MissingInRedis    int
	MissingInDB       int
	ValueMismatch     int
	RedisMalformed    int
	DBInvalid         int
	RedisRevAhead     int
	// RedisRevBehind 只在 RequireMapRev 打开时计数(否则恒为 0)。
	RedisRevBehind int

	MergedIntoMissingInRedis int
	MergedIntoMissingInDB    int
	MergedIntoMismatch       int

	// TransientCleared 只做提示,不算差异。
	TransientCleared int

	// FrozenRecovery 记下本次是否按冻结期恢复口径判退出码(AuditOptions.FrozenRecovery 原样带出;不进统计行)。
	FrozenRecovery bool
	// RequireMapRev 记下本次是否按严格 maprev 口径计数(AuditOptions.RequireMapRev 原样带出);打开时统计行末尾多一段 redis_rev_behind。
	RequireMapRev bool
}

// Differences 是全部差异计数之和(不含 TransientCleared)。
func (r AuditReport) Differences() int {
	return r.DBOnlyUnpublished + r.MissingInRedis + r.MissingInDB + r.ValueMismatch + r.RedisMalformed +
		r.DBInvalid + r.RedisRevAhead + r.RedisRevBehind + r.MergedIntoMissingInRedis + r.MergedIntoMissingInDB + r.MergedIntoMismatch
}

// BlockingDifferences 是决定退出码的差异数,非 0 = 退出码 1:默认等于 Differences;冻结期恢复口径下不含 db_only_unpublished。
func (r AuditReport) BlockingDifferences() int {
	if r.FrozenRecovery {
		return r.Differences() - r.DBOnlyUnpublished
	}
	return r.Differences()
}

// SummaryLine 是 stdout 唯一的一行。严格 maprev 口径下末尾追加 ` redis_rev_behind=N`;默认口径逐字不变(脚本照旧解析)。
func (r AuditReport) SummaryLine() string {
	line := fmt.Sprintf("route_audit db_rows=%d redis_players=%d db_only_unpublished=%d missing_in_redis=%d missing_in_db=%d "+
		"value_mismatch=%d redis_malformed=%d db_invalid=%d redis_rev_ahead=%d merged_into_missing_in_redis=%d "+
		"merged_into_missing_in_db=%d merged_into_mismatch=%d transient_cleared=%d",
		r.DBRows, r.RedisPlayers, r.DBOnlyUnpublished, r.MissingInRedis, r.MissingInDB,
		r.ValueMismatch, r.RedisMalformed, r.DBInvalid, r.RedisRevAhead, r.MergedIntoMissingInRedis,
		r.MergedIntoMissingInDB, r.MergedIntoMismatch, r.TransientCleared)
	if r.RequireMapRev {
		line += fmt.Sprintf(" redis_rev_behind=%d", r.RedisRevBehind)
	}
	return line
}

// AuditExitCode 把一次 audit 的结果映射成退出码:出错 2,有差异 1(db_only_unpublished 也算,冻结期恢复口径除外),否则 0。
func AuditExitCode(rep AuditReport, err error) int {
	switch {
	case err != nil:
		return ExitFailed
	case rep.BlockingDifferences() > 0:
		return ExitAttention
	default:
		return ExitClean
	}
}

// diffKind 是一名玩家的一类差异(按玩家复核的那几类;merged_into 的三类是 zone 级的,不走复核)。
type diffKind uint8

const (
	diffDBOnlyUnpublished diffKind = iota + 1
	diffMissingInRedis
	diffMissingInDB
	diffValueMismatch
	diffRedisMalformed
	diffDBInvalid
	diffRedisRevAhead
	// diffRedisRevBehind 只由严格 maprev 口径产生(auditor.classify),classifyRoute 本身不产生它。
	diffRedisRevBehind
)

// String 与 stdout 统计行的键同名。
func (k diffKind) String() string {
	switch k {
	case diffDBOnlyUnpublished:
		return "db_only_unpublished"
	case diffMissingInRedis:
		return "missing_in_redis"
	case diffMissingInDB:
		return "missing_in_db"
	case diffValueMismatch:
		return "value_mismatch"
	case diffRedisMalformed:
		return "redis_malformed"
	case diffDBInvalid:
		return "db_invalid"
	case diffRedisRevAhead:
		return "redis_rev_ahead"
	case diffRedisRevBehind:
		return "redis_rev_behind"
	default:
		return "unknown"
	}
}

// add 给报告里对应的计数加 n。
func (r *AuditReport) add(k diffKind, n int) {
	switch k {
	case diffDBOnlyUnpublished:
		r.DBOnlyUnpublished += n
	case diffMissingInRedis:
		r.MissingInRedis += n
	case diffMissingInDB:
		r.MissingInDB += n
	case diffValueMismatch:
		r.ValueMismatch += n
	case diffRedisMalformed:
		r.RedisMalformed += n
	case diffDBInvalid:
		r.DBInvalid += n
	case diffRedisRevAhead:
		r.RedisRevAhead += n
	case diffRedisRevBehind:
		r.RedisRevBehind += n
	}
}

// classifyRoute 比对一名玩家的库行(nil = 库里没有这一行)与缓存三族键,返回差异种类(升序、不重复;无差异为 nil)。
// 方向 A、方向 B 与复核共用这一个函数,保证三处口径一致。
func classifyRoute(db *store.RouteRow, raw routeTriple) []diffKind {
	if db == nil {
		switch {
		case raw.absent():
			return nil
		case malformedTriple(raw):
			return []diffKind{diffRedisMalformed}
		default:
			// 库里没有行,而 Redis 认得这名玩家(含只有 maprev 的):库缺。
			return []diffKind{diffMissingInDB}
		}
	}
	dbPlc, err := db.PlacementValue()
	if err != nil {
		return []diffKind{diffDBInvalid}
	}
	if !db.IsTombstone() && raw.absent() {
		return []diffKind{diffDBOnlyUnpublished}
	}
	r, bad := parseRedisRoute(db.PlayerID, raw)
	if bad != nil {
		return []diffKind{diffRedisMalformed}
	}

	var kinds []diffKind
	add := func(k diffKind) {
		for _, have := range kinds {
			if have == k {
				return
			}
		}
		kinds = append(kinds, k)
	}
	// raw 里的值已过规范形式校验,可以直接与库值的缓存形态逐字节比。
	for _, col := range [2][2]string{{db.HomeValue(), raw.Zone}, {dbPlc, raw.Placement}} {
		dbVal, redisVal := col[0], col[1]
		switch {
		case dbVal == redisVal:
		case dbVal == "":
			add(diffMissingInDB)
		case redisVal == "":
			add(diffMissingInRedis)
		default:
			add(diffValueMismatch)
		}
	}
	if r.MapRev != 0 && r.MapRev > db.Rev {
		add(diffRedisRevAhead)
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })
	return kinds
}

// malformedTriple 报告三族键里有没有不规范的值。
func malformedTriple(raw routeTriple) bool {
	_, bad := parseRedisRoute(0, raw)
	return bad != nil
}

// classify 是本次 audit 的分类口径:classifyRoute 的结果,严格 maprev 口径下再加 redis_rev_behind(判据见文件头)。
// 方向 A、方向 B 与复核都经它,三处口径一致。
func (a *auditor) classify(db *store.RouteRow, raw routeTriple) []diffKind {
	kinds := classifyRoute(db, raw)
	if !a.opts.RequireMapRev || !mapRevBehind(db, raw, kinds) {
		return kinds
	}
	return append(kinds, diffRedisRevBehind) // 它的序号最大,追加之后仍是升序
}

// mapRevBehind 报告这名玩家的缓存 maprev 是否缺席或小于库 rev。库里没有行、库行无效、墓碑、三族键全无(已计 db_only_unpublished)、
// 缓存值畸形(已计 redis_malformed)都不算。
func mapRevBehind(db *store.RouteRow, raw routeTriple, kinds []diffKind) bool {
	if db == nil || db.IsTombstone() || raw.absent() {
		return false
	}
	for _, k := range kinds {
		if k == diffDBInvalid || k == diffRedisMalformed {
			return false
		}
	}
	r, bad := parseRedisRoute(db.PlayerID, raw)
	if bad != nil {
		return false
	}
	return r.MapRev == 0 || r.MapRev < db.Rev
}

// playerDiff 是一名玩家在某一次读取时的差异与两边的值(打明细用)。
type playerDiff struct {
	playerID uint64
	kinds    []diffKind
	db       *store.RouteRow // nil = 库里没有这一行
	raw      routeTriple
}

// auditor 是一次 audit 的状态。单 goroutine 使用。
type auditor struct {
	rd     mappingReader
	db     routeDB
	opts   AuditOptions
	report AuditReport
	detail *detailReporter

	// redisSeen 是方向 B 见过的 player_id(三族键去重)。
	redisSeen map[uint64]struct{}
	// dbSeen 是方向 A 比过的 player_id;方向 B 只补比不在里面的 id。按 100 万行估算约几十 MB,同 import 的 seen。
	dbSeen map[uint64]struct{}
	// badKeys 是判过畸形的键名(SCAN 可能回两次,不重复计数)。
	badKeys map[string]struct{}

	// firstCounts 是首读的差异计数(超过 MaxRecheck 时就是结论)。
	firstCounts map[diffKind]int
	// candidates 是首读有差异的玩家(复核对象);超过 MaxRecheck 即 overflow,丢弃并停止收集。
	candidates map[uint64]playerDiff
	overflow   bool
	// samples 是首读差异的前 MaxReport 名玩家,只在 overflow(不复核)时用来打明细。
	samples []playerDiff
}

// Audit 跑一次 audit(步骤见文件头)。只有完整跑完才往 Stdout 写统计行;出错时退出码为 2。
func Audit(ctx context.Context, rd mappingReader, db routeDB, opts AuditOptions) (AuditReport, error) {
	if err := opts.validate(); err != nil {
		return AuditReport{}, err
	}
	if opts.Sleep == nil {
		opts.Sleep = sleepCtx
	}
	if opts.Target != "" {
		fmt.Fprintln(opts.Stderr, opts.Target)
	}

	a := &auditor{
		rd:          rd,
		db:          db,
		opts:        opts,
		detail:      newDetailReporter(opts.Stderr, opts.MaxReport),
		redisSeen:   make(map[uint64]struct{}),
		dbSeen:      make(map[uint64]struct{}),
		badKeys:     make(map[string]struct{}),
		firstCounts: make(map[diffKind]int),
		candidates:  make(map[uint64]playerDiff),
	}
	a.report.FrozenRecovery = opts.FrozenRecovery
	a.report.RequireMapRev = opts.RequireMapRev

	steps := []func(context.Context) error{a.auditDBRows, a.auditRedisKeys, a.auditMergedInto, a.settle}
	for _, step := range steps {
		if err := step(ctx); err != nil {
			a.detail.finish()
			return a.report, err
		}
	}
	a.detail.finish()
	if opts.FrozenRecovery {
		// 统计行的格式不变(脚本照旧解析);口径写在 stderr。
		fmt.Fprintf(opts.Stderr, "NOTE: -frozen-recovery: db_only_unpublished=%d is reported but not counted in the exit code "+
			"(rows only the DB has; the reconcile leader of batch 1b-2 pushes them to the cache after unfreeze); every other category must be 0\n",
			a.report.DBOnlyUnpublished)
	}
	if opts.RequireMapRev {
		fmt.Fprintf(opts.Stderr, "NOTE: -require-maprev: redis_rev_behind=%d counts rows (not tombstones) whose cache keys exist but player:maprev is "+
			"absent or below the DB rev; it is counted in the exit code (also with -frozen-recovery, which only exempts db_only_unpublished)\n",
			a.report.RedisRevBehind)
	}
	fmt.Fprintln(opts.Stdout, a.report.SummaryLine())
	return a.report, nil
}

// auditDBRows 方向 A。游标是上一页最后一行的聚簇键 (route_shard, player_id)。
//
// 已知窗口(G1a 偏离 H6):审计期间新提交、且缓存三键全无的行(db_first 下已提交未发布),若落在已经扫过的片里,
// 本次方向 A 看不到、方向 B 也不会查它(Redis 里没有它的键),要到下一次 audit 才报。旧形状下 id 单调,这类行几乎都在
// 游标之后;多 login 号段交错时同样会漏,只是更少。
func (a *auditor) auditDBRows(ctx context.Context) error {
	var after store.RouteScanCursor
	for {
		rows, err := a.db.ScanRoutes(ctx, after, a.opts.Batch)
		if err != nil {
			return fmt.Errorf("scan route rows after (shard, player_id)=(%d, %d): %w", after.Shard, after.PlayerID, err)
		}
		if len(rows) == 0 {
			return nil
		}
		a.report.DBRows += len(rows)
		ids := make([]uint64, len(rows))
		for i := range rows {
			ids[i] = rows[i].PlayerID
		}
		triples, err := readRouteTriples(ctx, a.rd, ids)
		if err != nil {
			return err
		}
		for i := range rows {
			row := rows[i]
			a.dbSeen[row.PlayerID] = struct{}{}
			a.recordFirst(playerDiff{playerID: row.PlayerID, kinds: a.classify(&row, triples[i]), db: &row, raw: triples[i]})
		}
		if len(rows) < a.opts.Batch {
			return nil
		}
		after = store.RouteScanCursorAt(rows[len(rows)-1])
	}
}

// auditRedisKeys 方向 B。
func (a *auditor) auditRedisKeys(ctx context.Context) error {
	for _, prefix := range []string{homeZoneKeyPrefix, placement.KeyPrefix, placement.MapRevKeyPrefix} {
		err := scanKeys(ctx, a.rd, prefix+"*", int64(a.opts.Batch), func(keys []string) error {
			ids := make([]uint64, 0, len(keys))
			for _, key := range keys {
				id, ok := parseKeyPlayerID(key, prefix)
				if !ok {
					a.malformedKey(key, "key suffix is not a canonical non-zero player_id")
					continue
				}
				if _, dup := a.redisSeen[id]; dup {
					continue
				}
				a.redisSeen[id] = struct{}{}
				ids = append(ids, id)
			}
			return a.auditRedisOnlyPlayers(ctx, ids)
		})
		if err != nil {
			return err
		}
	}
	a.report.RedisPlayers = len(a.redisSeen)
	return nil
}

// auditRedisOnlyPlayers 比对一页 Redis 玩家里方向 A 没比过的那些:库里没有行的(classifyRoute(nil, …)),
// 以及方向 A 扫过之后才提交的行(按库行比对,口径与方向 A 相同)。方向 A 比过的 id 跳过,不重复计数。
func (a *auditor) auditRedisOnlyPlayers(ctx context.Context, ids []uint64) error {
	unchecked := make([]uint64, 0, len(ids))
	for _, id := range ids {
		if _, ok := a.dbSeen[id]; !ok {
			unchecked = append(unchecked, id)
		}
	}
	for start := 0; start < len(unchecked); start += a.opts.Batch {
		chunk := unchecked[start:min(start+a.opts.Batch, len(unchecked))]
		rows, err := a.db.GetRoutes(ctx, chunk)
		if err != nil {
			return fmt.Errorf("get route rows of %d players: %w", len(chunk), err)
		}
		triples, err := readRouteTriples(ctx, a.rd, chunk)
		if err != nil {
			return err
		}
		for i, id := range chunk {
			var dbRow *store.RouteRow
			if row, ok := rows[id]; ok {
				dbRow = &row
			}
			a.recordFirst(playerDiff{playerID: id, kinds: a.classify(dbRow, triples[i]), db: dbRow, raw: triples[i]})
		}
	}
	return nil
}

// auditMergedInto 第 3 步。merged_into 是 zone 级的,条数很少、几乎不变,不走复核。
func (a *auditor) auditMergedInto(ctx context.Context) error {
	dbRows, err := a.db.ListMergedInto(ctx)
	if err != nil {
		return err
	}
	scan, err := scanMergedInto(ctx, a.rd, int64(a.opts.Batch))
	if err != nil {
		return err
	}
	for _, bad := range scan.Malformed {
		a.malformedKey(bad.Key, fmt.Sprintf("value=%q: %s", bad.Value, bad.Reason))
	}

	inRedis := make(map[uint32]store.MergedIntoRow, len(scan.Rows))
	for _, r := range scan.Rows {
		inRedis[r.SrcZone] = r
	}
	inDB := make(map[uint32]bool, len(dbRows))
	for _, d := range dbRows {
		inDB[d.SrcZone] = true
		r, ok := inRedis[d.SrcZone]
		switch {
		case !ok:
			a.report.MergedIntoMissingInRedis++
			a.detail.line(fmt.Sprintf("DIFF kind=merged_into_missing_in_redis src=%d db=%s redis=none", d.SrcZone, mergedIntoText(d)))
		case r.DstZone != d.DstZone || r.RunID != d.RunID:
			a.report.MergedIntoMismatch++
			a.detail.line(fmt.Sprintf("DIFF kind=merged_into_mismatch src=%d db=%s redis=%s", d.SrcZone, mergedIntoText(d), mergedIntoText(r)))
		}
	}
	for _, r := range scan.Rows {
		if !inDB[r.SrcZone] {
			a.report.MergedIntoMissingInDB++
			a.detail.line(fmt.Sprintf("DIFF kind=merged_into_missing_in_db src=%d db=none redis=%s", r.SrcZone, mergedIntoText(r)))
		}
	}
	return nil
}

// recordFirst 记一名玩家的首读结果。
func (a *auditor) recordFirst(d playerDiff) {
	if len(d.kinds) == 0 {
		return
	}
	for _, k := range d.kinds {
		a.firstCounts[k]++
	}
	if len(a.samples) < a.opts.MaxReport {
		a.samples = append(a.samples, d)
	}
	if a.overflow {
		return
	}
	a.candidates[d.playerID] = d
	if len(a.candidates) > a.opts.MaxRecheck {
		a.overflow = true
		a.candidates = nil
	}
}

// settle 第 4 步:复核,或在超过上限时照首读计入。
func (a *auditor) settle(ctx context.Context) error {
	if a.overflow {
		fmt.Fprintf(a.opts.Stderr, "WARN: more than -max-recheck=%d players differ on the first read; "+
			"counting them without a recheck (transient differences included)\n", a.opts.MaxRecheck)
		for k, n := range a.firstCounts {
			a.report.add(k, n)
		}
		for _, d := range a.samples {
			a.printDiff(d)
		}
		return nil
	}
	if len(a.candidates) == 0 {
		return nil
	}
	if err := a.opts.Sleep(ctx, a.opts.RecheckDelay); err != nil {
		return fmt.Errorf("wait %s before the recheck: %w", a.opts.RecheckDelay, err)
	}

	ids := make([]uint64, 0, len(a.candidates))
	for id := range a.candidates {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for start := 0; start < len(ids); start += a.opts.Batch {
		chunk := ids[start:min(start+a.opts.Batch, len(ids))]
		rows, err := a.db.GetRoutes(ctx, chunk)
		if err != nil {
			return fmt.Errorf("recheck: get route rows of %d players: %w", len(chunk), err)
		}
		triples, err := readRouteTriples(ctx, a.rd, chunk)
		if err != nil {
			return fmt.Errorf("recheck: %w", err)
		}
		for i, id := range chunk {
			var dbRow *store.RouteRow
			if row, ok := rows[id]; ok {
				dbRow = &row
			}
			first := a.candidates[id]
			still := intersectKinds(first.kinds, a.classify(dbRow, triples[i]))
			a.report.TransientCleared += len(first.kinds) - len(still)
			for _, k := range still {
				a.report.add(k, 1)
			}
			if len(still) > 0 {
				a.printDiff(playerDiff{playerID: id, kinds: still, db: dbRow, raw: triples[i]})
			}
		}
	}
	return nil
}

// malformedKey 记一把 Redis 键(名或值)不规范、且无法归到某名玩家的情形,计入 redis_malformed(同一把键只计一次)。
func (a *auditor) malformedKey(key, reason string) {
	if _, dup := a.badKeys[key]; dup {
		return
	}
	a.badKeys[key] = struct{}{}
	a.report.RedisMalformed++
	a.detail.line(fmt.Sprintf("DIFF kind=%s player=- db=- redis=%s reason=%s", diffRedisMalformed, key, reason))
}

// printDiff 每类差异打一行明细。
func (a *auditor) printDiff(d playerDiff) {
	for _, k := range d.kinds {
		a.detail.line(fmt.Sprintf("DIFF kind=%s player=%d db=%s redis=%s", k, d.playerID, dbRowText(d.db), tripleText(d.raw)))
	}
}

// intersectKinds 返回两次读取都出现的差异种类(保持 first 的顺序)。
func intersectKinds(first, second []diffKind) []diffKind {
	var out []diffKind
	for _, k := range first {
		for _, s := range second {
			if k == s {
				out = append(out, k)
				break
			}
		}
	}
	return out
}

// sleepCtx 等 d,可被 ctx 取消。d <= 0 时不等。
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// dbRowText 是明细里库一侧的值。
func dbRowText(row *store.RouteRow) string {
	if row == nil {
		return "none"
	}
	return fmt.Sprintf("{home=%q placement=%q rev=%d}", row.HomeValue(), dbPlacementText(*row), row.Rev)
}

// tripleText 是明细里 Redis 一侧的值("" = 键不在)。
func tripleText(raw routeTriple) string {
	return fmt.Sprintf("{zone=%q placement=%q maprev=%q}", raw.Zone, raw.Placement, raw.MapRev)
}

// mergedIntoText 是明细里一条合走标记的值。
func mergedIntoText(r store.MergedIntoRow) string {
	return fmt.Sprintf("{dst=%d run_id=%q}", r.DstZone, r.RunID)
}
