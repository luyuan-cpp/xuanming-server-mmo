package routetool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"data_service/internal/store"

	"shared/placement"
)

// import-from-redis(提案 §7 P1):把 mapping Redis 里现存的归属 / 落点 / 合走标记导进全局库的路由表。
//
//  1. 开头原样快照一次 merge:merged_into:*;
//  2. A 遍:逐页 SCAN player:zone:*,每页一次 MGET 三族键(zone / placement / maprev),逐名校验规范形式,
//     任一值不规范就整名玩家跳过并记 MALFORMED;合法的按 player_id 升序凑批调 ImportRoutes;
//     处理过(含判畸形)的 id 记进 seen;
//  3. B 遍:SCAN player:placement:*,**按是否在 seen 里**跳过 —— 不是按「zone 非空」跳过:P1 期间 redis 模式
//     还在登记,A、B 两遍之间新写了 zone 的玩家在 A 遍里没被 SCAN 到,按 zone 跳过就会把他漏掉;
//  4. merged_into:SCAN merge:merged_into:*,校验后调 ImportMergedInto(同 src 而 dst / run_id 不同记冲突、不覆盖);
//  5. 结尾把 merged_into 快照与开头比对:有任何新增 / 消失 / 改值,退出码 3 —— 导入窗口内跑过合服 / 撤销合服
//     (见 snapshotMergedInto)。
//
// 退出码 3 之后**不能原样重跑**:窗口内被合服改写的玩家,库里已导入的是旧 home(src)、缓存里是新 home(dst),
// 而导入从不覆盖库里的非 NULL 值,重跑只会把它们判成 conflict(退出码 1)且永远收敛不了。P1 期间库还不是真源
// (全部副本仍是 redis 模式,没有任何读方),恢复办法是在窗口外清空 player_route / zone_merged_into、
// route_meta / route_reconcile 原地复位之后从头导入(docs/ops/merge-zone-runbook.md §9.3)。只有中断 / 超时 / 退出码 2
// 这类与合服无关的失败,才可以原样重跑(导入幂等)。
//
// 只能检测 merge / unmerge 写的 merged_into;其余写者的冻结窗口由人保证(ImportNote)。
// seen 按 100 万玩家估算约几十 MB,对一次性的运维工具可以接受。

// ImportNote 是 import-from-redis 在 stderr 打的第一行。
const ImportNote = "NOTE: 只能检测 merge/unmerge 改过 merge:merged_into:*;其余写者的冻结窗口由人保证(proposal §7 P1)"

// 退出码(cmd/route_tool 原样返回给 shell)。
const (
	// ExitClean 完成,且没有任何需要人工处理的项。
	ExitClean = 0
	// ExitAttention 完成,但有需要人工处理的项:import 的冲突 / 畸形 / rev 倒退,audit 的任一差异(含 db_only_unpublished)。
	ExitAttention = 1
	// ExitFailed 失败:参数、配置、连接、查询或写入出错(含 1205),或超时。
	ExitFailed = 2
	// ExitMergeChanged 只有 import 用:导入期间 merge:merged_into 变过(窗口内跑过合服),已中止。
	ExitMergeChanged = 3
)

// ErrMergeStateChanged 表示 import 结尾的 merge:merged_into 快照与开头不同:导入窗口内有合服 / 撤销合服跑过(退出码 3)。
var ErrMergeStateChanged = errors.New("route_tool: merge:merged_into changed during the import (a merge / unmerge ran inside the window)")

// routeDB 是本包对全局库的全部依赖,*store.RouteStore 天然满足。导入只走 ImportRoutes / ImportMergedInto
// (写入、加锁读、writable 检查都在 store 里);审计只走后三个一致读。
// ScanRoutes 沿聚簇键 (route_shard, player_id) 翻页(G1a),结果不是 player_id 序;游标用 store.RouteScanCursorAt 取。
type routeDB interface {
	ImportRoutes(ctx context.Context, rows []store.RouteImportRow, dryRun bool) (store.RouteImportStats, error)
	ImportMergedInto(ctx context.Context, rows []store.MergedIntoRow, dryRun bool) (store.MergedIntoImportStats, error)
	ScanRoutes(ctx context.Context, after store.RouteScanCursor, limit int) ([]store.RouteRow, error)
	GetRoutes(ctx context.Context, ids []uint64) (map[uint64]store.RouteRow, error)
	ListMergedInto(ctx context.Context) ([]store.MergedIntoRow, error)
}

// ImportOptions 是 import-from-redis 的参数(cmd/route_tool 的命令行一一对应)。
type ImportOptions struct {
	// DryRun 只做判定(库里只发不加锁的 SELECT),不写任何东西,同样给出统计。命令行默认 true。
	DryRun bool
	// Batch 是 SCAN 的 COUNT 提示、一次 MGET 的玩家数、一次 ImportRoutes 的行数,[1, store.RouteBatchLimit]。
	Batch int
	// MaxReport 是 stderr 明细行的上限;计数始终精确,不受它影响。
	MaxReport int
	// Target 非空时作为 stderr 第二行(连接目标,只含 host 与库名,不含 DSN / 口令)。
	Target string
	// Stdout 只写一行统计(给脚本解析);Stderr 写 NOTE、明细与告警。
	Stdout io.Writer
	Stderr io.Writer
}

func (o ImportOptions) validate() error {
	if o.Batch < 1 || o.Batch > store.RouteBatchLimit {
		return fmt.Errorf("batch %d not in [1, %d]", o.Batch, store.RouteBatchLimit)
	}
	if o.MaxReport < 0 {
		return fmt.Errorf("max-report %d must not be negative", o.MaxReport)
	}
	if o.Stdout == nil || o.Stderr == nil {
		return errors.New("stdout / stderr writers are required")
	}
	return nil
}

// ImportReport 是一次 import 的计数(dry-run 时是「如果写会怎样」)。
type ImportReport struct {
	DryRun bool
	// PlayersScanned 是 A、B 两遍看到的不同 player_id 数(键名后缀合法的;含值判畸形的)。
	PlayersScanned int
	Inserted       int
	Matched        int
	Filled         int
	DBAhead        int
	RevRegressions int
	Conflicts      int
	// Malformed 按玩家计(一名玩家多个畸形值只计一次),另含键名后缀不规范的键。
	Malformed int

	MergedIntoInserted  int
	MergedIntoMatched   int
	MergedIntoConflicts int
	MergedIntoMalformed int
}

// NeedsAttention 报告是否有需要人工处理的项(退出码 1)。merged_into 的冲突 / 畸形与玩家的同等对待:
// 它们同样没有被导入,同样需要人去核对。
func (r ImportReport) NeedsAttention() bool {
	return r.Conflicts > 0 || r.Malformed > 0 || r.RevRegressions > 0 ||
		r.MergedIntoConflicts > 0 || r.MergedIntoMalformed > 0
}

// SummaryLine 是 stdout 唯一的一行。
func (r ImportReport) SummaryLine() string {
	return fmt.Sprintf("route_import dry_run=%t players_scanned=%d inserted=%d matched=%d filled=%d db_ahead=%d "+
		"rev_regression=%d conflicts=%d malformed=%d merged_into_inserted=%d merged_into_matched=%d "+
		"merged_into_conflicts=%d merged_into_malformed=%d",
		r.DryRun, r.PlayersScanned, r.Inserted, r.Matched, r.Filled, r.DBAhead,
		r.RevRegressions, r.Conflicts, r.Malformed, r.MergedIntoInserted, r.MergedIntoMatched,
		r.MergedIntoConflicts, r.MergedIntoMalformed)
}

// ImportExitCode 把一次 import 的结果映射成退出码。
func ImportExitCode(rep ImportReport, err error) int {
	switch {
	case errors.Is(err, ErrMergeStateChanged):
		return ExitMergeChanged
	case err != nil:
		return ExitFailed
	case rep.NeedsAttention():
		return ExitAttention
	default:
		return ExitClean
	}
}

// importer 是一次 import 的状态。单 goroutine 使用。
type importer struct {
	rd     mappingReader
	db     routeDB
	opts   ImportOptions
	report ImportReport
	detail *detailReporter
	// seen 是处理过的 player_id(A 遍写入,B 遍据它跳过,两遍各自的 SCAN 重复也靠它去重)。
	seen map[uint64]struct{}
	// badKeys 是判过畸形的键名(SCAN 可能把同一把键回两次,不重复计数)。
	badKeys map[string]struct{}
	// pending 是校验通过、等着凑批的行。
	pending []store.RouteImportRow
	// mergedIntoAtStart 是开头那次 merge:merged_into:* 的原样快照(结尾与它比对)。
	mergedIntoAtStart map[string]string
}

// Import 跑一次 import-from-redis(步骤见文件头)。返回 ErrMergeStateChanged 时退出码为 3;
// 其它 error(Redis / 库 / 超时)退出码为 2。只有完整跑完(含结尾的 merged_into 复查)才往 Stdout 写统计行。
func Import(ctx context.Context, rd mappingReader, db routeDB, opts ImportOptions) (ImportReport, error) {
	if err := opts.validate(); err != nil {
		return ImportReport{DryRun: opts.DryRun}, err
	}
	fmt.Fprintln(opts.Stderr, ImportNote)
	if opts.Target != "" {
		fmt.Fprintln(opts.Stderr, opts.Target)
	}

	im := &importer{
		rd:      rd,
		db:      db,
		opts:    opts,
		report:  ImportReport{DryRun: opts.DryRun},
		detail:  newDetailReporter(opts.Stderr, opts.MaxReport),
		seen:    make(map[uint64]struct{}),
		badKeys: make(map[string]struct{}),
	}

	snap, err := snapshotMergedInto(ctx, rd, int64(opts.Batch))
	if err != nil {
		// 一行都还没写:按失败处理(退出码 2),原样重跑即可。
		return im.report, fmt.Errorf("snapshot merge:merged_into before the import (nothing was written): %w", err)
	}
	im.mergedIntoAtStart = snap

	for _, prefix := range []string{homeZoneKeyPrefix, placement.KeyPrefix} {
		if err := im.importPlayerPass(ctx, prefix); err != nil {
			im.detail.finish()
			return im.report, err
		}
	}
	if err := im.importMergedInto(ctx); err != nil {
		im.detail.finish()
		return im.report, err
	}

	// 结尾复查:merged_into 与开头不同(窗口内有合服 / 撤销合服跑完了)是退出码 3。
	const afterWhen = "after the import; rows already written are kept but must NOT be re-imported as is — " +
		"truncate the route tables and import from scratch after the window (merge-zone-runbook §9.3)"
	changeErr := im.checkMergedIntoUnchanged(ctx, afterWhen)
	im.detail.finish()
	if changeErr != nil && !errors.Is(changeErr, ErrMergeStateChanged) {
		// 复查本身失败(Redis 出错):证明不了窗口内没有合服,按失败处理,不出统计行。
		return im.report, changeErr
	}
	// 结尾命中时也出统计行:已写入的部分是真实的,运维需要知道写了多少。
	fmt.Fprintln(opts.Stdout, im.report.SummaryLine())
	return im.report, changeErr
}

// checkMergedIntoUnchanged 重读 merge:merged_into:* 并与开头的快照比对;有任何新增 / 消失 / 改值就打 ABORT
// 并返回 ErrMergeStateChanged。
func (im *importer) checkMergedIntoUnchanged(ctx context.Context, when string) error {
	now, err := snapshotMergedInto(ctx, im.rd, int64(im.opts.Batch))
	if err != nil {
		return fmt.Errorf("snapshot merge:merged_into %s: %w", when, err)
	}
	changes := diffMergedIntoSnapshots(im.mergedIntoAtStart, now)
	if len(changes) == 0 {
		return nil
	}
	fmt.Fprintf(im.opts.Stderr, "ABORT: merge:merged_into changed during the import (+added -removed ~changed) %s: %s\n",
		when, strings.Join(changes, " "))
	return fmt.Errorf("%w (%s): %s", ErrMergeStateChanged, when, strings.Join(changes, ", "))
}

// importPlayerPass 是 A 遍(prefix = player:zone:)或 B 遍(prefix = player:placement:)。
func (im *importer) importPlayerPass(ctx context.Context, prefix string) error {
	err := scanKeys(ctx, im.rd, prefix+"*", int64(im.opts.Batch), func(keys []string) error {
		ids := make([]uint64, 0, len(keys))
		for _, key := range keys {
			id, ok := parseKeyPlayerID(key, prefix)
			if !ok {
				im.malformedKey(key)
				continue
			}
			if _, dup := im.seen[id]; dup {
				continue
			}
			im.seen[id] = struct{}{}
			ids = append(ids, id)
		}
		return im.importPlayers(ctx, ids)
	})
	if err != nil {
		return err
	}
	return im.flush(ctx)
}

// importPlayers 按 Batch 分块 MGET 三族键,校验后放进 pending,够一批就导。
func (im *importer) importPlayers(ctx context.Context, ids []uint64) error {
	for start := 0; start < len(ids); start += im.opts.Batch {
		chunk := ids[start:min(start+im.opts.Batch, len(ids))]
		triples, err := readRouteTriples(ctx, im.rd, chunk)
		if err != nil {
			return err
		}
		for i, id := range chunk {
			im.report.PlayersScanned++
			r, bad := parseRedisRoute(id, triples[i])
			if bad != nil {
				im.malformedPlayer(id, *bad)
				continue
			}
			if !r.hasMapping() {
				if r.MapRev != 0 {
					// 只有 maprev、没有归属也没有落点:发布脚本写不出这种形状(W1 一定带 home),导进库就是一行墓碑。
					im.malformedPlayer(id, malformedValue{
						Key:    placement.MapRevKey(id),
						Value:  r.Raw.MapRev,
						Reason: "player:maprev present without player:zone / player:placement",
					})
				}
				// 三把键都已不在:SCAN 与 MGET 之间被删,此刻没有可导入的东西。
				continue
			}
			im.pending = append(im.pending, r.importRow())
		}
		if len(im.pending) >= im.opts.Batch {
			if err := im.flush(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

// flush 把 pending 按 player_id 升序、每 Batch 行一次交给 ImportRoutes(它要求严格升序、无重复;
// seen 保证同一个 id 整个运行期只进 pending 一次)。
func (im *importer) flush(ctx context.Context) error {
	if len(im.pending) == 0 {
		return nil
	}
	rows := im.pending
	im.pending = nil
	sort.Slice(rows, func(i, j int) bool { return rows[i].PlayerID < rows[j].PlayerID })
	for start := 0; start < len(rows); start += im.opts.Batch {
		chunk := rows[start:min(start+im.opts.Batch, len(rows))]
		stats, err := im.db.ImportRoutes(ctx, chunk, im.opts.DryRun)
		if err != nil {
			return fmt.Errorf("import %d players (player_id %d..%d, dry_run=%t): %w",
				len(chunk), chunk[0].PlayerID, chunk[len(chunk)-1].PlayerID, im.opts.DryRun, err)
		}
		im.report.Inserted += stats.Inserted
		im.report.Matched += stats.Matched
		im.report.Filled += stats.Filled
		im.report.DBAhead += stats.DBAhead
		im.report.RevRegressions += len(stats.RevRegressions)
		im.report.Conflicts += len(stats.Conflicts)
		for _, c := range stats.Conflicts {
			im.detail.line(fmt.Sprintf("CONFLICT player=%d db_home=%d db_placement=%q db_rev=%d redis_home=%d redis_placement=%q redis_maprev=%d",
				c.PlayerID, c.DB.HomeZone, dbPlacementText(c.DB), c.DB.Rev, c.Redis.HomeZone, recordText(c.Redis.Placement), c.Redis.MapRev))
		}
		for _, c := range stats.RevRegressions {
			im.detail.line(fmt.Sprintf("REV_REGRESSION player=%d db_rev=%d redis_maprev=%d", c.PlayerID, c.DB.Rev, c.Redis.MapRev))
		}
	}
	return nil
}

// importMergedInto 是第 4 步。
func (im *importer) importMergedInto(ctx context.Context) error {
	scan, err := scanMergedInto(ctx, im.rd, int64(im.opts.Batch))
	if err != nil {
		return err
	}
	for _, bad := range scan.Malformed {
		im.report.MergedIntoMalformed++
		im.detail.line(fmt.Sprintf("MALFORMED merged_into key=%s value=%q reason=%s", bad.Key, bad.Value, bad.Reason))
	}
	for start := 0; start < len(scan.Rows); start += im.opts.Batch {
		chunk := scan.Rows[start:min(start+im.opts.Batch, len(scan.Rows))]
		stats, err := im.db.ImportMergedInto(ctx, chunk, im.opts.DryRun)
		if err != nil {
			return fmt.Errorf("import %d merged_into rows (dry_run=%t): %w", len(chunk), im.opts.DryRun, err)
		}
		im.report.MergedIntoInserted += stats.Inserted
		im.report.MergedIntoMatched += stats.Matched
		im.report.MergedIntoConflicts += len(stats.Conflicts)
		for _, c := range stats.Conflicts {
			im.detail.line(fmt.Sprintf("MERGED_INTO_CONFLICT src=%d db_dst=%d db_run_id=%q redis_dst=%d redis_run_id=%q",
				c.Redis.SrcZone, c.DB.DstZone, c.DB.RunID, c.Redis.DstZone, c.Redis.RunID))
		}
	}
	return nil
}

// malformedKey 记一把键名后缀不规范的键(同一把键只计一次)。
func (im *importer) malformedKey(key string) {
	if _, dup := im.badKeys[key]; dup {
		return
	}
	im.badKeys[key] = struct{}{}
	im.report.Malformed++
	im.detail.line(fmt.Sprintf("MALFORMED player=- key=%s value=%q reason=%s", key, "", "key suffix is not a canonical non-zero player_id"))
}

// malformedPlayer 记一名值不规范的玩家(整名跳过,只计一次)。
func (im *importer) malformedPlayer(playerID uint64, bad malformedValue) {
	im.report.Malformed++
	im.detail.line(fmt.Sprintf("MALFORMED player=%d key=%s value=%q reason=%s", playerID, bad.Key, bad.Value, bad.Reason))
}

// ── import 与 audit 共用的输出 ─────────────────────────────────────────────────

// detailReporter 往 stderr 写明细行,最多 limit 行;超出的只计数,结尾补一行说明。计数器(stdout)始终精确。
type detailReporter struct {
	w          io.Writer
	limit      int
	printed    int
	suppressed int
}

func newDetailReporter(w io.Writer, limit int) *detailReporter {
	return &detailReporter{w: w, limit: limit}
}

func (d *detailReporter) line(s string) {
	if d.printed >= d.limit {
		d.suppressed++
		return
	}
	d.printed++
	fmt.Fprintln(d.w, s)
}

func (d *detailReporter) finish() {
	if d.suppressed > 0 {
		fmt.Fprintf(d.w, "... %d more detail line(s) suppressed by -max-report=%d; the counters on stdout are exact\n", d.suppressed, d.limit)
		d.suppressed = 0
	}
}

// dbPlacementText 是库里一行落点的缓存形态("" = 无落点);行本身不自洽时给出原因,不 panic、不吞掉。
func dbPlacementText(row store.RouteRow) string {
	v, err := row.PlacementValue()
	if err != nil {
		return "invalid: " + err.Error()
	}
	return v
}

// recordText 是一条落点记录的缓存形态(nil = "")。
func recordText(rec *placement.Record) string {
	if rec == nil {
		return ""
	}
	return rec.Encode()
}
