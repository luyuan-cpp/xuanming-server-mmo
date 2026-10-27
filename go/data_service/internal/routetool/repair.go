package routetool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"data_service/internal/store"

	"google.golang.org/protobuf/proto"
	pb "proto/common/database"
	"shared/placement"
)

// reconcile-from-cache(第 1b-1 批,2026-10-18):db_first 下全局库被回档之后,把 mapping Redis 里较新的映射修回库
// (docs/design/player-route-mapping-durability-proposal.md §11.2「第 1b 批」、runbook §9.4)。
// 恢复流程是 freeze → reconcile-from-cache → audit -frozen-recovery → unfreeze,本文件只管第二步。
//
//  1. ReadRouteMeta:writable == 1 → 退出码 2,提示先 freeze(库内每批事务还会用 R18x 加锁复核一次);
//  2. SCAN merge:in_progress:*,有一把就退出码 3,一行都不写(冻结窗口内本不该有合服);
//  3. 强读正式正常冻结收据与当前 epoch / run；每批提交在同一 meta 行锁下复核；
//  4. 三遍 SCAN,**无状态去重**(内存只占一页,不像 import 那样保留 O(N) 的已见集合):
//     A 遍 player:zone:*;B 遍 player:placement:*,MGET 出的 zone 非空就跳过(A 遍处理过);
//     C 遍 player:maprev:*,zone 或落点非空就跳过,剩下的只有 maprev → MALFORMED。
//     每页先按 player_id 去重(SCAN 同一页也可能回重复键)、升序,再按 Batch 分块 MGET 三族键 → 规范形式校验 →
//     RepairRoutesFromCache(逐行判定见 store.decideRepair);
//  5. merge:merged_into:* → RepairMergedIntoFromCache(只增不删);
//  6. 结尾再查一次围栏,命中同样退出码 3(已写的保留,统计行照出)。
//
// 无状态去重成立的前提是**冻结期间没有人写 mapping Redis**:W1 被 writable=0 拦下(拦在发布之前)、合服被冻结窗口禁止、
// 第 1b-2 批的 leader 冻结时停止推送。有人违反时,B 遍可能因为「A、B 两遍之间新写了 zone」而跳过一名玩家,那名玩家漏修,
// 只有随后的 audit 能发现。跨页的重复键(SCAN rehash)会被修两次:第二次判 Matched(幂等),只让计数略多。
//
// 对 mapping Redis **只读**,对库只经 store.RouteStore 的两个修库原语。

// RepairNote 是 reconcile-from-cache 在 stderr 打的第一行。
const RepairNote = "NOTE: reconcile-from-cache writes the cache back into the DB where the cache is newer (maprev > db rev); " +
	"run it only while frozen after a global-DB rollback, and keep merge_zone stopped (proposal §11.2 batch 1b, runbook §9.4)"

// repairDB 是 reconcile-from-cache 对全局库的全部依赖,*store.RouteStore 天然满足(编译期断言在 repair_test.go)。
type repairDB interface {
	ReadRouteMeta(ctx context.Context) (store.RouteMeta, error)
	RepairRoutesFromCache(ctx context.Context, rows []store.RouteImportRow, opts store.RouteRepairOptions) (store.RouteRepairStats, error)
	RepairMergedIntoFromCache(ctx context.Context, rows []store.MergedIntoRow, dryRun bool) (store.MergedIntoImportStats, error)
}

// FrozenCatalog 提供同一永久冻结收据及当前目录，只读核验不能制造控制权限。
type FrozenCatalog interface {
	MaintenanceReceipt(context.Context, []byte) (*pb.SqlWriterMaintenanceReceipt, error)
	CurrentMeta(context.Context) (*pb.RouteMeta, error)
}

// RepairOptions 是 reconcile-from-cache 的参数(routetoolcli 的命令行一一对应)。
type RepairOptions struct {
	Maintenance   MaintenanceWork
	Catalog       FrozenCatalog
	FrozenReceipt *pb.SqlWriterMaintenanceReceipt
	// DryRun 只判定不写(库里只发不加锁的 SELECT),同样给出统计。命令行默认 true。
	DryRun bool
	// ResolveTies:同 rev 而值不同时用缓存覆盖(-resolve-ties=cache);默认只报告(-resolve-ties=report)。
	ResolveTies bool
	// Batch 是 SCAN 的 COUNT 提示、一次 MGET 的玩家数、一次修库事务的行数,[1, store.RouteBatchLimit]。
	Batch int
	// MaxReport 是 stderr 明细行的上限;计数始终精确。
	MaxReport int
	// FreezeGrace 是兼容参数，不参与正式排空或放行判断；不得为负。
	FreezeGrace time.Duration
	// Sleep 仅保留源码兼容；正式永久冻结收据替代固定等待，不调用它。
	Sleep func(ctx context.Context, d time.Duration) error
	// Target 非空时作为 stderr 第二行(连接目标,只含 host 与库名)。
	Target string
	// Stdout 只写一行统计;Stderr 写 NOTE、明细与告警。
	Stdout io.Writer
	Stderr io.Writer
}

func (o RepairOptions) validate() error {
	if o.Batch < 1 || o.Batch > store.RouteBatchLimit {
		return fmt.Errorf("batch %d not in [1, %d]", o.Batch, store.RouteBatchLimit)
	}
	if o.MaxReport < 0 {
		return fmt.Errorf("max-report %d must not be negative", o.MaxReport)
	}
	if o.FreezeGrace < 0 {
		return fmt.Errorf("freeze-grace %s must not be negative", o.FreezeGrace)
	}
	if o.Stdout == nil || o.Stderr == nil {
		return errors.New("stdout / stderr writers are required")
	}
	return nil
}

// RepairReport 是一次 reconcile-from-cache 的计数(dry-run 时是「如果写会怎样」)。
type RepairReport struct {
	DryRun bool
	// PlayersScanned 是三遍里交给判定的玩家数(跳过规则之后;跨页重复的 id 会被数两次)。
	PlayersScanned int
	Inserted       int
	Restored       int
	Filled         int
	Matched        int
	DBAhead        int
	Ties           int
	TiesResolved   int
	Conflicts      int
	// Malformed 按玩家计(键名后缀不规范的键也算一条)。
	Malformed int

	MergedIntoInserted  int
	MergedIntoMatched   int
	MergedIntoConflicts int
	MergedIntoMalformed int
}

// NeedsAttention 报告是否有需要人工处理的项(退出码 1):未覆盖的平局、冲突、畸形。
func (r RepairReport) NeedsAttention() bool {
	return r.Ties > 0 || r.Conflicts > 0 || r.Malformed > 0 || r.MergedIntoConflicts > 0 || r.MergedIntoMalformed > 0
}

// SummaryLine 是 stdout 唯一的一行。
func (r RepairReport) SummaryLine() string {
	return fmt.Sprintf("route_reconcile_from_cache dry_run=%t players_scanned=%d inserted=%d restored=%d filled=%d matched=%d "+
		"db_ahead=%d ties=%d ties_resolved=%d conflicts=%d malformed=%d merged_into_inserted=%d merged_into_matched=%d "+
		"merged_into_conflicts=%d merged_into_malformed=%d",
		r.DryRun, r.PlayersScanned, r.Inserted, r.Restored, r.Filled, r.Matched,
		r.DBAhead, r.Ties, r.TiesResolved, r.Conflicts, r.Malformed, r.MergedIntoInserted, r.MergedIntoMatched,
		r.MergedIntoConflicts, r.MergedIntoMalformed)
}

// RepairExitCode 把一次 reconcile-from-cache 的结果映射成退出码:围栏 3,出错 2(含没冻结),需要人工处理 1,否则 0。
func RepairExitCode(rep RepairReport, err error) int {
	switch {
	case errors.Is(err, ErrMergeFenceActive):
		return ExitFenced
	case err != nil:
		return ExitFailed
	case rep.NeedsAttention():
		return ExitAttention
	default:
		return ExitClean
	}
}

// repairer 是一次 reconcile-from-cache 的状态。单 goroutine 使用;不保留跨页的已见集合(见文件头)。
type repairer struct {
	rd     mappingReader
	db     repairDB
	opts   RepairOptions
	report RepairReport
	detail *detailReporter
}

// ReconcileFromCache 跑一次 reconcile-from-cache(步骤见文件头)。返回 ErrMergeFenceActive 时退出码为 3;
// 其它 error(没冻结、Redis / 库出错、超时)退出码为 2。只有跑到结尾才往 Stdout 写统计行(结尾命中围栏时也写)。
func ReconcileFromCache(ctx context.Context, rd mappingReader, db repairDB, opts RepairOptions) (RepairReport, error) {
	if opts.DryRun {
		return reconcileFromCache(ctx, rd, db, opts)
	}
	if opts.Maintenance == nil {
		return RepairReport{}, errors.New("实际恢复缺少有限维护工具接收门")
	}
	done, err := opts.Maintenance.Enter()
	if err != nil {
		return RepairReport{}, err
	}
	returned := false
	defer func() {
		if !returned {
			opts.Maintenance.Hold()
		}
		done()
	}()
	rep, err := reconcileFromCache(ctx, rd, db, opts)
	if err != nil && !errors.Is(err, ErrMergeFenceActive) {
		opts.Maintenance.Hold()
	}
	returned = true
	return rep, err
}

func reconcileFromCache(ctx context.Context, rd mappingReader, db repairDB, opts RepairOptions) (RepairReport, error) {
	if err := opts.validate(); err != nil {
		return RepairReport{DryRun: opts.DryRun}, err
	}
	if opts.Catalog == nil || opts.FrozenReceipt == nil {
		return RepairReport{}, errors.New("恢复缺少正式正常冻结原件")
	}
	frozen, err := opts.Catalog.MaintenanceReceipt(ctx, opts.FrozenReceipt.RequestId)
	if err != nil {
		return RepairReport{}, err
	}
	current, err := opts.Catalog.CurrentMeta(ctx)
	if err != nil {
		return RepairReport{}, err
	}
	if !proto.Equal(frozen, opts.FrozenReceipt) || frozen.Kind != pb.SqlWriterMaintenanceReceipt_FREEZE || frozen.Phase != pb.SqlWriterMaintenanceReceipt_FROZEN || frozen.RunPhase != pb.SqlWriterDrainRun_SQL_FROZEN || current.Generation != frozen.Generation || current.Writable != 0 || current.WriterEpoch != frozen.AfterEpoch || current.CatalogRevision != frozen.CatalogRevision || !bytes.Equal(current.FreezeRunId, frozen.RunId) {
		return RepairReport{}, errors.New("恢复所绑定的正常冻结与当前目录不一致")
	}
	fmt.Fprintln(opts.Stderr, RepairNote)
	if opts.Target != "" {
		fmt.Fprintln(opts.Stderr, opts.Target)
	}
	rp := &repairer{
		rd:     rd,
		db:     db,
		opts:   opts,
		report: RepairReport{DryRun: opts.DryRun},
		detail: newDetailReporter(opts.Stderr, opts.MaxReport),
	}

	meta, err := db.ReadRouteMeta(ctx)
	if err != nil {
		return rp.report, fmt.Errorf("read route_meta: %w", err)
	}
	if !meta.Frozen() {
		return rp.report, fmt.Errorf("%w: run `-route-tool freeze` first, then this", store.ErrRouteNotFrozen)
	}
	if err := rp.checkFences(ctx, "before the repair; nothing was written"); err != nil {
		return rp.report, err
	}
	// 正式冻结收据已证明排空；等待固定秒数不能替代证明，也不参与放行。

	for _, prefix := range []string{homeZoneKeyPrefix, placement.KeyPrefix, placement.MapRevKeyPrefix} {
		if err := rp.repairPass(ctx, prefix); err != nil {
			rp.detail.finish()
			return rp.report, err
		}
	}
	if err := rp.repairMergedInto(ctx); err != nil {
		rp.detail.finish()
		return rp.report, err
	}

	fenceErr := rp.checkFences(ctx, "after the repair; rows already written are kept (the repair is idempotent, rerun it after the merge window)")
	rp.detail.finish()
	if fenceErr != nil && !errors.Is(fenceErr, ErrMergeFenceActive) {
		// 复查本身失败:证明不了窗口内没有合服,按失败处理,不出统计行。
		return rp.report, fenceErr
	}
	fmt.Fprintln(opts.Stdout, rp.report.SummaryLine())
	return rp.report, fenceErr
}

// checkFences SCAN 一次围栏;有生效的就打 ABORT 并返回 ErrMergeFenceActive。
func (rp *repairer) checkFences(ctx context.Context, when string) error {
	fences, err := listActiveFences(ctx, rp.rd, int64(rp.opts.Batch))
	if err != nil {
		return err
	}
	if len(fences) == 0 {
		return nil
	}
	fmt.Fprintf(rp.opts.Stderr, "ABORT: %d active merge fence(s) %s: %s\n", len(fences), when, strings.Join(fences, " "))
	return fmt.Errorf("%w (%s): %s", ErrMergeFenceActive, when, strings.Join(fences, ", "))
}

// repairPass 是 A / B / C 三遍之一(prefix 决定跳过规则,见文件头第 4 步)。
func (rp *repairer) repairPass(ctx context.Context, prefix string) error {
	return scanKeys(ctx, rp.rd, prefix+"*", int64(rp.opts.Batch), func(keys []string) error {
		// 同一页先去重:SCAN 同一页也可能回重复键,重复的 id 进同一批会被 store 的严格升序校验整批拒绝。
		inPage := make(map[uint64]struct{}, len(keys))
		ids := make([]uint64, 0, len(keys))
		for _, key := range keys {
			id, ok := parseKeyPlayerID(key, prefix)
			if !ok {
				rp.malformedKey(key)
				continue
			}
			if _, dup := inPage[id]; dup {
				continue
			}
			inPage[id] = struct{}{}
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		for start := 0; start < len(ids); start += rp.opts.Batch {
			if err := rp.repairChunk(ctx, prefix, ids[start:min(start+rp.opts.Batch, len(ids))]); err != nil {
				return err
			}
		}
		return nil
	})
}

// repairChunk 读一块玩家(已升序、无重复)的三族键,按本遍的跳过规则筛掉别的遍负责的玩家,校验后整块交给 RepairRoutesFromCache。
func (rp *repairer) repairChunk(ctx context.Context, prefix string, ids []uint64) error {
	triples, err := readRouteTriples(ctx, rp.rd, ids)
	if err != nil {
		return err
	}
	rows := make([]store.RouteImportRow, 0, len(ids))
	for i, id := range ids {
		t := triples[i]
		switch prefix {
		case placement.KeyPrefix:
			if t.Zone != "" {
				continue // 有 zone:A 遍处理过
			}
		case placement.MapRevKeyPrefix:
			if t.Zone != "" || t.Placement != "" {
				continue // 有 zone 或落点:A / B 遍处理过
			}
		}
		rp.report.PlayersScanned++
		r, bad := parseRedisRoute(id, t)
		if bad != nil {
			rp.malformedPlayer(id, *bad)
			continue
		}
		if !r.hasMapping() {
			if r.MapRev != 0 {
				// 只有 maprev、没有归属也没有落点:发布脚本写不出这种形状(同 import 的口径)。
				rp.malformedPlayer(id, malformedValue{
					Key:    placement.MapRevKey(id),
					Value:  t.MapRev,
					Reason: "player:maprev present without player:zone / player:placement",
				})
			}
			// 三把键都已不在:SCAN 与 MGET 之间被删(冻结期间本不该发生),此刻没有可修的东西。
			continue
		}
		rows = append(rows, r.importRow())
	}
	if len(rows) == 0 {
		return nil
	}
	stats, err := rp.db.RepairRoutesFromCache(ctx, rows, store.RouteRepairOptions{DryRun: rp.opts.DryRun, ResolveTies: rp.opts.ResolveTies})
	if err != nil {
		return fmt.Errorf("repair %d players from the cache (player_id %d..%d, dry_run=%t): %w",
			len(rows), rows[0].PlayerID, rows[len(rows)-1].PlayerID, rp.opts.DryRun, err)
	}
	rp.report.Inserted += stats.Inserted
	rp.report.Restored += stats.Restored
	rp.report.Filled += stats.Filled
	rp.report.Matched += stats.Matched
	rp.report.DBAhead += stats.DBAhead
	rp.report.Ties += len(stats.Ties)
	rp.report.TiesResolved += len(stats.TiesResolved)
	rp.report.Conflicts += len(stats.Conflicts)
	for _, c := range stats.Ties {
		rp.detail.line("TIE " + repairConflictText(c))
	}
	for _, c := range stats.TiesResolved {
		rp.detail.line("TIE_RESOLVED " + repairConflictText(c))
	}
	for _, c := range stats.Conflicts {
		rp.detail.line("CONFLICT " + repairConflictText(c))
	}
	return nil
}

// repairMergedInto 是第 5 步。
func (rp *repairer) repairMergedInto(ctx context.Context) error {
	scan, err := scanMergedInto(ctx, rp.rd, int64(rp.opts.Batch))
	if err != nil {
		return err
	}
	for _, bad := range scan.Malformed {
		rp.report.MergedIntoMalformed++
		rp.detail.line(fmt.Sprintf("MALFORMED merged_into key=%s value=%q reason=%s", bad.Key, bad.Value, bad.Reason))
	}
	for start := 0; start < len(scan.Rows); start += rp.opts.Batch {
		chunk := scan.Rows[start:min(start+rp.opts.Batch, len(scan.Rows))]
		stats, err := rp.db.RepairMergedIntoFromCache(ctx, chunk, rp.opts.DryRun)
		if err != nil {
			return fmt.Errorf("repair %d merged_into rows (dry_run=%t): %w", len(chunk), rp.opts.DryRun, err)
		}
		rp.report.MergedIntoInserted += stats.Inserted
		rp.report.MergedIntoMatched += stats.Matched
		rp.report.MergedIntoConflicts += len(stats.Conflicts)
		for _, c := range stats.Conflicts {
			rp.detail.line(fmt.Sprintf("MERGED_INTO_CONFLICT src=%d db_dst=%d db_run_id=%q redis_dst=%d redis_run_id=%q",
				c.Redis.SrcZone, c.DB.DstZone, c.DB.RunID, c.Redis.DstZone, c.Redis.RunID))
		}
	}
	return nil
}

// malformedKey 记一把键名后缀不规范的键。不做跨页去重(无状态):SCAN 把同一把坏键回两次时会计两次。
func (rp *repairer) malformedKey(key string) {
	rp.report.Malformed++
	rp.detail.line(fmt.Sprintf("MALFORMED player=- key=%s value=%q reason=%s", key, "", "key suffix is not a canonical non-zero player_id"))
}

// malformedPlayer 记一名值不规范的玩家(整名跳过)。
func (rp *repairer) malformedPlayer(playerID uint64, bad malformedValue) {
	rp.report.Malformed++
	rp.detail.line(fmt.Sprintf("MALFORMED player=%d key=%s value=%q reason=%s", playerID, bad.Key, bad.Value, bad.Reason))
}

// repairConflictText 是 TIE / TIE_RESOLVED / CONFLICT 明细的后半段(库行不自洽时 db_placement 给出原因)。
func repairConflictText(c store.RouteImportConflict) string {
	return fmt.Sprintf("player=%d db_home=%d db_placement=%q db_rev=%d redis_home=%d redis_placement=%q redis_maprev=%d",
		c.PlayerID, c.DB.HomeZone, dbPlacementText(c.DB), c.DB.Rev, c.Redis.HomeZone, recordText(c.Redis.Placement), c.Redis.MapRev)
}
