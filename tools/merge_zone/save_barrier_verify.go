package main

// verify:save_barrier —— -verify-merged 的父快照提交屏障逐人核对(2026-10-20 修复 r2-mz#1;2026-10-22 按评审对齐 login)。
//
// 前向 copy 合服保留 player:{id}:save_barrier(player_rows.go 的 playerCacheKeysKeepBarrier)、删掉父快照,所以合服后每名
// 清单玩家的第一次登录都走 login 的冷加载(go/login/internal/logic/pkg/dataloader/parent_commit_barrier.go):
// beginParentLoad(键类型、owner 与屏障字段规范、pending 的代次不超过 owner)→ parentLoadSnapshot.route(屏障在途时只等补投)
// → 读两张 SQL 行(任一行缺失即拒)→ parentLoadSnapshot.settle(go/shared/savebarrier.Reconcile 判定)。
// judgeSaveBarrier 按同一顺序预判,结论分三档:
//   - info(barrierVerdictOK):login 走 V1 一致路径(pending == 两表 ACK == 两行围栏),不改屏障、不打采纳告警。
//   - warn:login 会采纳 SQL 自愈(Reconcile 判 adopted:屏障缺失而两行同版非零,或两行 / ACK 新于 pending)。不丢数据,
//     但不是正确合服留下的状态(多半是旧二进制删过屏障,或合服前 Redis 丢过已确认的写),每人一次 PARENT-LOAD-ADOPT-SQL。
//   - block:login 会拒登。
//
// 只有一格有意比 login 严:屏障在途(ACK 缺失或低于 pending)。身份戳等于实例当前身份时 login 只等补投,而 P4 已证补投桶
// 为空、等不来;实例重启过时 login 改为读 SQL,两行等于 pending 就采纳。verify 看不到实例身份,按前者报 block。
// 采纳路径上 login 还要过 C4(persist_schema_gen 倒退,enforce 模式拒绝),verify 不读 schema_gen,所以 warn 不等于保证放行。
//
// 判定样例表 go/shared/savebarrier/testdata/merge_copy_cold_load_cases.json 由本模块单测(judgeSaveBarrier)与 login 单测
// (真实冷加载入口)各跑一遍,两份判定任一边漂移都会让其中一边变红。各结论的处置见 docs/ops/merge-zone-runbook.md §10.1 末段。
//
// 范围:owner_epoch 非 0、owner 或屏障键畸形、或屏障里有 pending / ACK 的玩家。其余人(owner 为 0 或不存在且没有屏障)在
// Redis 里没有存盘历史,本行不要求屏障、只计数;他们的行在不在由 verify:manifest_rows 回答。读失败一律 INFRA(没查成,
// 不是通过)。键与版本格式都是镜像字面量(本模块是独立 go.mod,不引 shared),单测钉住。
//
// 屏障 hash 只按 pending 与两表 ACK 三个字段判,不按字段集合判:C++ 严格屏障写的 schema_gen(覆盖世代)/ schema_writer
// (写方令牌,docs/design/mixed-version-persist-field-protection.md §5.2)与 go/db 的 outbox_* 都是合法字段,前向 copy
// 合服原样保留,这里不读也不拒。login 冷加载同样只按这三个字段判定(schema_gen 只在快路径与采纳时比较)。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
)

// saveBarrierAckTables 是屏障 hash 里两张表的 ACK 字段名,也是 zone 库里两张带行内围栏的玩家表名:
// go/db player_cache_publish.go 以 task.MsgType 作 ACK 字段,login 冷加载逐表 HGET 同名字段、逐表核对 SQL 行围栏。
var saveBarrierAckTables = [2]string{"player_database", "player_database_1"}

const saveBarrierPendingField = "pending"

// saveBarrierReadBatch 是一条读 pipeline 里的玩家数(每人一条 GET owner_epoch 加一条 HMGET 屏障)。
const saveBarrierReadBatch = 500

// playerOwnerEpochKey 与 go/shared/ownerepoch.OwnerEpochKey 逐字一致:玩家 id 用花括号包成 hash tag
// (docs/design/redis-player-store-cluster.md §5.2;merge_zone 是独立 module,不引用 shared,只镜像)。
func playerOwnerEpochKey(pid uint64) string {
	return "player:{" + strconv.FormatUint(pid, 10) + "}:owner_epoch"
}

// saveBarrierVersion 与 go/shared/savebarrier.Version 逐字一致:两段各 20 位十进制、中间一个冒号,共 41 字节。
func saveBarrierVersion(ownerEpoch, saveSeq uint64) string {
	return fmt.Sprintf("%020d:%020d", ownerEpoch, saveSeq)
}

// parseSaveBarrierVersion 与 savebarrier.Parse 同口径:长度、冒号位置、纯数字、uint64 不溢出,缺一不可。
func parseSaveBarrierVersion(v string) (ownerEpoch, saveSeq uint64, ok bool) {
	if len(v) != 41 || v[20] != ':' {
		return 0, 0, false
	}
	for i := 0; i < len(v); i++ {
		if i != 20 && (v[i] < '0' || v[i] > '9') {
			return 0, 0, false
		}
	}
	e, eerr := strconv.ParseUint(v[:20], 10, 64)
	s, serr := strconv.ParseUint(v[21:], 10, 64)
	if eerr != nil || serr != nil {
		return 0, 0, false
	}
	return e, s, true
}

// saveBarrierObservation 是一名清单玩家被观测到的状态(纯数据,judgeSaveBarrier 只看它)。
type saveBarrierObservation struct {
	OwnerRaw         string    // player:{<id>}:owner_epoch 原值;键不存在为 ""(login 的 Lua 同样读成空串)
	OwnerWrongType   bool      // owner 键存在但不是 string(login 的 Lua 以 key type mismatch 拒绝)
	BarrierWrongType bool      // 屏障键存在但不是 hash(同上)
	Pending          string    // 屏障 hash 的 pending;字段或 hash 不存在为 ""
	Acks             [2]string // 两表 ACK(顺序同 saveBarrierAckTables);缺失为 ""
	// PlacementBad:落点记录畸形,go/db 对他 fail-closed,算不出该去哪个库读行。
	PlacementBad bool
	// RowFence:有效落点库两张表的行内围栏 saveBarrierVersion(epoch, seq);零围栏照实保留,判定时经 rowVersion 归一。
	RowFence   [2]string
	RowPresent [2]bool
}

// judgeSaveBarrier 的结论。字面量进 Notes,低基数;barrierVerdictOK 表示 login 走一致路径,或本行不要求屏障。
// 每个结论在 docs/ops/merge-zone-runbook.md §10.1 末段有一条处置,也出现在共享样例表里:改名要三处一起改。
const (
	barrierVerdictOK = ""

	// warn:login 采纳 SQL 自愈(savebarrier.Reconcile 判 adopted)。
	// 屏障缺失(或只剩 ACK)而两行同版非零(设计 V7)。
	barrierVerdictMissingAdoptable = "barrier_missing_sql_adoptable"
	// 屏障在,两行同版且新于 pending(V2),或 ACK 超前于 pending 而两行等于 ACK。
	barrierVerdictRowsAhead = "sql_rows_ahead_of_barrier"

	// block:login 拒登。
	// owner 键不是 string,或值不是规范十进制。
	barrierVerdictOwnerMalformed = "owner_epoch_malformed"
	// 屏障键不是 hash,或 pending / ACK 不是规范版本串,或 pending 的代次 / 序号为 0。
	barrierVerdictBarrierMalformed = "save_barrier_malformed"
	// pending 在,owner 为 0 或不存在。
	barrierVerdictWithoutOwner = "barrier_without_owner"
	// pending 的代次高于 owner。
	barrierVerdictOwnerBehind = "owner_epoch_behind_pending"
	// 屏障在途(ACK 缺失或低于 pending):文件头所说「有意比 login 严」的那一格。
	barrierVerdictUncommitted = "ack_not_equal_pending"
	// 落点记录畸形,go/db 对他 fail-closed,读不出行。
	barrierVerdictPlacementBad = "placement_record_malformed"
	// 屏障在,有效落点库缺一张表的行。
	barrierVerdictRowMissing = "sql_row_missing"
	// 屏障在,两行比某个 ACK 旧(SQL 落后),或两行不同版。
	barrierVerdictFenceMismatch = "sql_row_fence_not_equal_pending"
	// owner≠0、没有 pending,且缺行。源库同样缺行时是合服前就登录不了的人(r2-login#1 的受害者),save_barrier_restore
	// 也恢复不了;源库有行而目标库没有则是拷行出了问题(verify:manifest_rows 同样 block)。
	barrierVerdictMissingNoSQLRow = "barrier_missing_no_sql_row"
	// owner≠0、没有 pending,两行都是零围栏(围栏之前的历史行),要 save_barrier_restore -allow-legacy。
	barrierVerdictMissingZeroFence = "barrier_missing_zero_fence"
	// owner≠0、没有 pending,两行不同版。
	barrierVerdictMissingRowsDisagree = "barrier_missing_rows_disagree"
)

// barrierVerdictSeverity:通过是 info;login 采纳 SQL 的两个结论是 warn;其余都是 block(login 拒登)。
func barrierVerdictSeverity(v string) string {
	switch v {
	case barrierVerdictOK:
		return "info"
	case barrierVerdictMissingAdoptable, barrierVerdictRowsAhead:
		return "warn"
	default:
		return "block"
	}
}

// saveBarrierRequired:这名玩家在 Redis 里有存盘历史的迹象(owner_epoch 非 0、owner 或屏障键畸形、屏障里有 pending / ACK),
// login 冷加载会按屏障判他。其余人只计数(见文件头「范围」)。
func saveBarrierRequired(o saveBarrierObservation) bool {
	if o.OwnerWrongType || o.BarrierWrongType || o.Pending != "" || o.Acks[0] != "" || o.Acks[1] != "" {
		return true
	}
	owner, ok := parseOwnerEpoch(o.OwnerRaw)
	return !ok || owner != 0
}

// parseOwnerEpoch 与 login 同口径(ownerepoch.ParseEpoch 加规范十进制复核):空串(键不存在)为 0。
func parseOwnerEpoch(raw string) (uint64, bool) {
	if raw == "" {
		return 0, true
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || strconv.FormatUint(v, 10) != raw {
		return 0, false
	}
	return v, true
}

// judgeSaveBarrier 按 login 冷加载的顺序预判一名玩家(纯函数;顺序与三档含义见文件头)。
func judgeSaveBarrier(o saveBarrierObservation) string {
	if !saveBarrierRequired(o) {
		return barrierVerdictOK
	}
	// ① beginParentLoad:键类型,owner 与屏障三字段的格式(savebarrier.ClassifyBarrier 对不规范字段报错)。
	if o.OwnerWrongType {
		return barrierVerdictOwnerMalformed
	}
	owner, ok := parseOwnerEpoch(o.OwnerRaw)
	if !ok {
		return barrierVerdictOwnerMalformed
	}
	if o.BarrierWrongType || !barrierFieldsCanonical(o) {
		return barrierVerdictBarrierMalformed
	}
	// ① beginParentLoad:pending 的代次不超过 owner。没有 pending 而 owner 已铸 = 历史缺口,
	// login 只在两张 SQL 行可采纳时放行(settle 里 Reconcile 判 adopted),其余一律拒绝。
	historyGap := false
	if o.Pending == "" {
		historyGap = owner != 0
	} else {
		epoch, seq, _ := parseSaveBarrierVersion(o.Pending)
		switch {
		case epoch == 0 || seq == 0:
			return barrierVerdictBarrierMalformed
		case owner == 0:
			return barrierVerdictWithoutOwner
		case owner < epoch:
			return barrierVerdictOwnerBehind
		}
	}
	// ② route:屏障在途时 login 只等补投,P4 之后等不来。
	if barrierInflight(o) {
		return barrierVerdictUncommitted
	}
	// ③ 读两张 SQL 行:落点算不出、任一行缺失,login 都拒绝(「玩家 SQL 权威历史行缺失」)。
	if o.PlacementBad {
		return barrierVerdictPlacementBad
	}
	if !o.RowPresent[0] || !o.RowPresent[1] {
		if historyGap {
			return barrierVerdictMissingNoSQLRow
		}
		return barrierVerdictRowMissing
	}
	// ④ settle:savebarrier.Reconcile。
	rows := [2]string{rowVersion(o.RowFence[0]), rowVersion(o.RowFence[1])}
	switch reconcileSaveBarrier(o.Pending, o.Acks, rows) {
	case reconcileConsistent:
		if historyGap {
			// 走到这里 pending、ACK 与两行都为空:两行都是零围栏。login 以历史缺口拒绝。
			return barrierVerdictMissingZeroFence
		}
		return barrierVerdictOK
	case reconcileAdoptSQL:
		if historyGap {
			return barrierVerdictMissingAdoptable
		}
		return barrierVerdictRowsAhead
	default:
		if historyGap {
			return barrierVerdictMissingRowsDisagree
		}
		return barrierVerdictFenceMismatch
	}
}

// barrierFieldsCanonical:屏障三字段里非空的都是规范版本串(与 savebarrier.ClassifyBarrier 的 requireVersions 同口径)。
func barrierFieldsCanonical(o saveBarrierObservation) bool {
	for _, v := range []string{o.Pending, o.Acks[0], o.Acks[1]} {
		if v == "" {
			continue
		}
		if _, _, ok := parseSaveBarrierVersion(v); !ok {
			return false
		}
	}
	return true
}

// barrierInflight 与 savebarrier.ClassifyBarrier 的 BarrierInflight 同口径:pending 在,没有 ACK 高于它,
// 且不是两表 ACK 都等于它。字段已是规范定宽串或空串,字节序就是 (owner_epoch, save_seq) 的元组序。
func barrierInflight(o saveBarrierObservation) bool {
	if o.Pending == "" || o.Acks[0] > o.Pending || o.Acks[1] > o.Pending {
		return false
	}
	return o.Acks[0] != o.Pending || o.Acks[1] != o.Pending
}

// rowVersion 与 savebarrier.RowVersion 同口径:零围栏 (0,0) 表示这一行从未经围栏写入,记为空串。
func rowVersion(fence string) string {
	if fence == saveBarrierVersion(0, 0) {
		return ""
	}
	return fence
}

// reconcileVerdict 是 savebarrier.Reconcile 结论的合服视角:login 不放行的三种结论(等补投 / 等另一张表 / 人工)
// 对合服后的 verify 没有区别 —— P3 / P4 之后不会再有补投或积压任务追平,一律 reconcileRefused。
type reconcileVerdict uint8

const (
	reconcileConsistent reconcileVerdict = iota + 1 // savebarrier.VerdictConsistent
	reconcileAdoptSQL                               // savebarrier.VerdictAdoptSQL
	reconcileRefused                                // VerdictInflight / VerdictReconciling / VerdictSQLBehind
)

// reconcileSaveBarrier 镜像 go/shared/savebarrier.Reconcile,规则顺序一致:先排除「SQL 落后」(某个 ACK 高于两行),
// 再看两行是否同版,最后比较 SQL 与 pending。输入都是规范定宽版本串或空串。
func reconcileSaveBarrier(pending string, acks, rows [2]string) reconcileVerdict {
	if max(acks[0], acks[1]) > max(rows[0], rows[1]) {
		return reconcileRefused
	}
	if rows[0] != rows[1] {
		return reconcileRefused
	}
	both := rows[0]
	switch {
	case both == pending && acks[0] == both && acks[1] == both:
		return reconcileConsistent
	case both >= pending:
		return reconcileAdoptSQL
	default:
		return reconcileRefused
	}
}

// rowFenceReader 读 schema.table 里 ids 的行内围栏:player_id → saveBarrierVersion(fence_owner_epoch, fence_save_seq)。
// 没有行的 id 不出现在返回值里。单测经它注入替身(本 module 没有 sqlmock)。
type rowFenceReader func(ctx context.Context, schema, table string, ids []uint64) (map[uint64]string, error)

// sqlRowFenceReader 是 rowFenceReader 的 MySQL 实现。非锁定读、分批 IN;schema 只来自 storeDBName,table 只来自
// saveBarrierAckTables,都不是用户输入。缺 fence 列(还没跑围栏迁移)时查询报错,调用方报 INFRA。
func sqlRowFenceReader(db *sql.DB) rowFenceReader {
	return func(ctx context.Context, schema, table string, ids []uint64) (map[uint64]string, error) {
		if db == nil {
			return nil, errors.New("no MySQL handle")
		}
		out := make(map[uint64]string, len(ids))
		for _, batch := range chunkUint64(sortedUint64(ids), playerRowsBatchSize) {
			rows, err := db.QueryContext(ctx, fmt.Sprintf(
				"SELECT %s, fence_owner_epoch, fence_save_seq FROM %s.%s WHERE %s IN (%s)",
				playerIDColumn, schema, table, playerIDColumn, inListLiteral(batch)))
			if err != nil {
				return nil, fmt.Errorf("read row fences of %s.%s: %w", schema, table, err)
			}
			for rows.Next() {
				var id, epoch, seq uint64
				if err := rows.Scan(&id, &epoch, &seq); err != nil {
					rows.Close()
					return nil, fmt.Errorf("scan row fence of %s.%s: %w", schema, table, err)
				}
				out[id] = saveBarrierVersion(epoch, seq)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return nil, err
			}
		}
		return out, nil
	}
}

// verifySaveBarriers 是 -verify-merged 的 verify:save_barrier 一行(见文件头)。
func verifySaveBarriers(ctx context.Context, cfg auditConfig) ResourceAudit {
	return verifySaveBarriersWith(ctx, cfg, sqlRowFenceReader(cfg.db))
}

func verifySaveBarriersWith(ctx context.Context, cfg auditConfig, readFences rowFenceReader) ResourceAudit {
	r := ResourceAudit{Name: "verify:save_barrier", UniqueScope: "global"}
	m, err := cfg.verifyManifest()
	if err != nil {
		return infraAudit(r.Name, "%v", err)
	}
	r.SourceCount = int64(len(m.PlayerIDs))
	// 与 verify:manifest_rows 同一道闸:玩家行这一步没做完,行还不在 login 要读的库里,拿屏障去比它只会得出错误结论。
	mode := m.rowsMode()
	if gate := rowsStepFor(mode); !m.stepDone(gate) {
		r.Severity = "warn"
		r.Notes = fmt.Sprintf("NOT VERIFIED: step %s (-player-rows-mode %s) is not recorded as done in the manifest — "+
			"save barriers were not checked against the players' rows", gate, mode)
		return r
	}
	if cfg.sharedRDB == nil {
		return infraAudit(r.Name, "no shared (DB 0) Redis handle — owner_epoch and save_barrier live there")
	}
	obs, required, err := readSaveBarrierState(ctx, cfg.sharedRDB, m.PlayerIDs)
	if err != nil {
		return infraAudit(r.Name, "%v%s", err, budgetNote(ctx))
	}
	if len(required) == 0 {
		r.Severity = "info"
		r.Notes = fmt.Sprintf("none of the %d manifest players has save history in Redis (owner_epoch 0 or absent, "+
			"no save_barrier): no save barrier is required", len(m.PlayerIDs))
		return r
	}
	if err := readRequiredRowFences(ctx, cfg, required, obs, readFences); err != nil {
		return infraAudit(r.Name, "%v%s", err, budgetNote(ctx))
	}

	byVerdict := map[string][]uint64{}
	for _, id := range required {
		if v := judgeSaveBarrier(*obs[id]); v != barrierVerdictOK {
			byVerdict[v] = append(byVerdict[v], id)
		}
	}
	verdicts := make([]string, 0, len(byVerdict))
	for v := range byVerdict {
		verdicts = append(verdicts, v)
	}
	sort.Strings(verdicts)
	var refusedParts, adoptedParts []string
	refused, adopted := 0, 0
	for _, v := range verdicts {
		part := fmt.Sprintf("%s=%d (first ids: %v)", v, len(byVerdict[v]), sampleUint64(byVerdict[v]))
		if barrierVerdictSeverity(v) == "warn" {
			adopted += len(byVerdict[v])
			adoptedParts = append(adoptedParts, part)
		} else {
			refused += len(byVerdict[v])
			refusedParts = append(refusedParts, part)
		}
	}
	r.TargetCount = int64(len(required) - refused - adopted)
	r.ConflictCount = int64(refused + adopted)
	adoptedNote := ""
	if adopted > 0 {
		adoptedNote = fmt.Sprintf("%d would be admitted only by the login adopting their SQL rows (one PARENT-LOAD-ADOPT-SQL "+
			"each; no data loss, but not what a correct copy merge leaves behind): %s. ", adopted, strings.Join(adoptedParts, "; "))
	}
	switch {
	case refused > 0:
		r.Severity = "block"
		r.Notes = fmt.Sprintf("%d of the %d manifest players with save history would be refused by the login cold load: %s. %s"+
			"Keep the maintenance gate and do not zone-up; every reason has its own disposition in docs/ops/merge-zone-runbook.md "+
			"§10.1 (last part) — never delete a barrier to turn this row green", refused, len(required),
			strings.Join(refusedParts, "; "), adoptedNote)
	case adopted > 0:
		r.Severity = "warn"
		r.Notes = fmt.Sprintf("no manifest player would be refused, but %sConfirm the cause per docs/ops/merge-zone-runbook.md "+
			"§10.1 (last part) before zone-up", adoptedNote)
	default:
		r.Severity = "info"
		r.Notes = fmt.Sprintf("all %d manifest players with save history take the consistent cold-load path (committed "+
			"save_barrier equal to both row fences in their placement store); %d have no save history in Redis (no barrier required)",
			len(required), len(m.PlayerIDs)-len(required))
	}
	return r
}

// budgetNote:读失败是因为本行的时间预算用完时补一句,免得把超时当成 Redis / MySQL 故障。预算由 runAuditMode 给每个
// auditor 统一设定,清单人数很多时可能稳定超时 —— 那仍是 INFRA(没查成),不是通过。
func budgetNote(ctx context.Context) string {
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ""
	}
	return " — the per-auditor time budget set by runAuditMode ran out before every manifest player was checked; this is " +
		"not a pass: re-run -VerifyMerged, and if it keeps timing out at this manifest size the budget has to be raised in code"
}

// readSaveBarrierState 对每名清单玩家读 owner_epoch(GET)与屏障三字段(HMGET),每批一条 pipeline;返回全部观测与
// required(saveBarrierRequired 为真的人,按 ids 顺序)。两类键都在共享核心 Redis 上(与父快照同实例,login 的 Lua
// 要求如此)。键类型错(WRONGTYPE)是这名玩家自己的数据损坏,记进观测、由 judgeSaveBarrier 报出来(login 的 Lua 同样
// 以 key type mismatch 拒绝他);其余读错误整体报错,不能把读失败当成「没有屏障」。
func readSaveBarrierState(ctx context.Context, rdb redis.Cmdable, ids []uint64) (
	map[uint64]*saveBarrierObservation, []uint64, error) {
	obs := make(map[uint64]*saveBarrierObservation, len(ids))
	var required []uint64
	fields := []string{saveBarrierPendingField, saveBarrierAckTables[0], saveBarrierAckTables[1]}
	for _, batch := range chunkUint64(ids, saveBarrierReadBatch) {
		pipe := rdb.Pipeline()
		owners := make([]*redis.StringCmd, len(batch))
		barriers := make([]*redis.SliceCmd, len(batch))
		for i, id := range batch {
			owners[i] = pipe.Get(ctx, playerOwnerEpochKey(id))
			barriers[i] = pipe.HMGet(ctx, playerSaveBarrierKey(strconv.FormatUint(id, 10)), fields...)
		}
		// 不看 Exec 的返回值:它只是第一条失败命令的错误(redis.Nil、WRONGTYPE 也会占这个位置)。下面逐条判每个命令,
		// 网络错误、超时会出现在每条命令上,照样整体报错。
		_, _ = pipe.Exec(ctx)
		for i, id := range batch {
			o := &saveBarrierObservation{}
			obs[id] = o
			switch err := owners[i].Err(); {
			case err == nil:
				o.OwnerRaw = owners[i].Val()
			case errors.Is(err, redis.Nil):
				// 键不存在:OwnerRaw 保持 ""。
			case isWrongTypeReply(err):
				o.OwnerWrongType = true
			default:
				return nil, nil, fmt.Errorf("owner_epoch get for player %d: %w", id, err)
			}
			switch err := barriers[i].Err(); {
			case err == nil:
				vals := barriers[i].Val()
				if len(vals) != len(fields) {
					return nil, nil, fmt.Errorf("save_barrier hmget for player %d returned %d values", id, len(vals))
				}
				o.Pending, _ = vals[0].(string)
				o.Acks[0], _ = vals[1].(string)
				o.Acks[1], _ = vals[2].(string)
			case isWrongTypeReply(err):
				o.BarrierWrongType = true
			default:
				return nil, nil, fmt.Errorf("save_barrier hmget for player %d: %w", id, err)
			}
			if saveBarrierRequired(*o) {
				required = append(required, id)
			}
		}
	}
	return obs, required, nil
}

// isWrongTypeReply:Redis 以 WRONGTYPE 拒绝了这条命令(键存在但类型不对)。网络错误、超时不算。
func isWrongTypeReply(err error) bool {
	return redis.HasErrorPrefix(err, "WRONGTYPE")
}

// readRequiredRowFences 按有效落点(expectedMainRowStore:有记录用记录,没有用 dst)分库读 required 两张表的行内围栏。
func readRequiredRowFences(ctx context.Context, cfg auditConfig, required []uint64,
	obs map[uint64]*saveBarrierObservation, readFences rowFenceReader) error {
	routes, err := cfg.routeReads()
	if err != nil {
		return fmt.Errorf("%v (placement records decide which database login reads each player's rows from)", err)
	}
	reads, err := routes.placements(ctx, required)
	if err != nil {
		return err
	}
	if len(reads) != len(required) {
		return fmt.Errorf("placement read returned %d answers for %d players", len(reads), len(required))
	}
	byStore := map[uint32][]uint64{}
	for i, id := range required {
		store, ok := expectedMainRowStore(reads[i], cfg.dst)
		if !ok {
			obs[id].PlacementBad = true
			continue
		}
		byStore[store] = append(byStore[store], id)
	}
	for _, store := range sortedZones(storeSet(byStore)) {
		name, ok := storeDBName(store)
		if !ok {
			return fmt.Errorf("placement store %d has no database name", store)
		}
		ids := byStore[store]
		for t, table := range saveBarrierAckTables {
			fences, err := readFences(ctx, name, table, ids)
			if err != nil {
				return err
			}
			for _, id := range ids {
				if v, found := fences[id]; found {
					obs[id].RowFence[t], obs[id].RowPresent[t] = v, true
				}
			}
		}
	}
	return nil
}
