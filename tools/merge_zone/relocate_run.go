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
//   - 清单落盘。续跑时 S / T / 表集合 / topic 世代号必须与清单一致。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
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
	db       *sql.DB
	srcQ     map[string]string // 表 → S 库全限定名
	dstQ     map[string]string
	tables   []string // 升序
	cols     map[string][]string
	delStmt  map[string]*sql.Stmt
	copyStmt map[string]*sql.Stmt
}

func newSQLRelocateRows(ctx context.Context, db *sql.DB, srcSchema, dstSchema string, tables []string,
	cols map[string][]string) (*sqlRelocateRows, error) {
	r := &sqlRelocateRows{
		db: db, srcQ: map[string]string{}, dstQ: map[string]string{}, tables: append([]string(nil), tables...),
		cols: cols, delStmt: map[string]*sql.Stmt{}, copyStmt: map[string]*sql.Stmt{},
	}
	for _, t := range r.tables {
		if len(cols[t]) == 0 {
			r.Close()
			return nil, fmt.Errorf("no aligned columns for table %s", t)
		}
		r.srcQ[t] = srcSchema + "." + t
		r.dstQ[t] = dstSchema + "." + t
		del, err := db.PrepareContext(ctx, relocateDeleteSQL(r.dstQ[t]))
		if err != nil {
			r.Close()
			return nil, fmt.Errorf("prepare delete on %s: %w", r.dstQ[t], err)
		}
		r.delStmt[t] = del
		cp, err := db.PrepareContext(ctx, relocateCopySQL(r.dstQ[t], r.srcQ[t], cols[t]))
		if err != nil {
			r.Close()
			return nil, fmt.Errorf("prepare copy %s → %s: %w", r.srcQ[t], r.dstQ[t], err)
		}
		r.copyStmt[t] = cp
	}
	return r, nil
}

// Close 释放预编译语句。
func (r *sqlRelocateRows) Close() {
	for _, s := range r.delStmt {
		_ = s.Close()
	}
	for _, s := range r.copyStmt {
		_ = s.Close()
	}
}

func (r *sqlRelocateRows) copyPlayer(ctx context.Context, playerID uint64) (int, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	cold := 0
	for _, t := range r.tables {
		res, err := tx.StmtContext(ctx, r.delStmt[t]).ExecContext(ctx, playerID)
		if err != nil {
			return 0, fmt.Errorf("delete the old copy in %s: %w", r.dstQ[t], err)
		}
		n, _ := res.RowsAffected()
		cold += int(n)
		if _, err := tx.StmtContext(ctx, r.copyStmt[t]).ExecContext(ctx, playerID); err != nil {
			return 0, fmt.Errorf("copy %s → %s: %w", r.srcQ[t], r.dstQ[t], err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	committed = true
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

// ── 入口 ─────────────────────────────────────────────────────────

func runRelocateEntry(o options) {
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()
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
	log.Printf("    manifest=%s capability_zones=%v topic_generation=%d", manifestPath, o.capSpec.zones, o.kafkaTopicGen)

	db := mustOpenMySQL(ctx, o.mysqlDSN)
	defer db.Close()
	mapRdb := mustDial(ctx, "mapping", o.mappingAddr, o.mappingPwd, o.mappingDB)
	defer mapRdb.Close()
	// go/db 的排序锁与重试队列在它的 RedisClient 上:shared DB 0(-notice-redis-*)。
	sharedRdb := mustDial(ctx, "login/shared(DB0, go/db ordering locks)", o.noticeAddr, o.noticePwd, o.noticeDB)
	defer sharedRdb.Close()

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
	if resumed {
		if err := checkRelocateResume(m, s, t, tables, uint32(o.kafkaTopicGen), flagIDs); err != nil {
			log.Fatalf("manifest %s: %v", manifestPath, err)
		}
		log.Printf("RESUME: run_id=%s — %s (players come from the manifest; nothing is re-enumerated)", m.RunID, m.relocSummary())
	} else {
		players := flagIDs
		if len(players) == 0 {
			sp, err := collectStoragePlayers(ctx, mapRdb, s)
			if err != nil {
				log.Fatalf("R0: enumerate players placed on storage %d: %v", s, err)
			}
			players = sp.all()
			log.Printf("R0: storage %d holds %d players (%d by placement record, %d of them frozen; %d by home_zone without a record); "+
				"%d malformed placement records in the whole mapping are NOT included", s, len(players), len(sp.ByRecord), sp.Frozen,
				len(sp.ByHome), len(sp.Malformed))
		}
		if len(players) == 0 {
			log.Printf("Nothing to relocate: no player is placed on storage %d. No manifest written.", s)
			return
		}
		runID := newRelocateRunID(s, t, now)
		if !validRunID(runID) {
			log.Fatalf("internal: generated run_id %q cannot appear in a frozen placement value", runID)
		}
		m = newRelocateManifest(runID, s, t, uint32(o.kafkaTopicGen), tables, players, operatorTag(), now)
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
	reads, err := readPlacements(ctx, mapRdb, todo)
	if err != nil {
		log.Fatalf("R0: read placements: %v", err)
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
	defer rows.Close()
	save := func(m *relocateManifest) error { return saveRelocateManifest(manifestPath, m) }
	engine := newRelocateEngine(m, save, redisRelocatePlacements{rdb: mapRdb}, redisOrderingLocks{rdb: sharedRdb}, rows,
		capSet, o.relocateBatch, o.relocateLockWait)
	runErr := engine.run(ctx)
	// 出错路径上内存里的状态也可能已经推进(解冻了一部分),最后再落一次盘。
	if err := save(m); err != nil {
		log.Printf("ERROR: final manifest save failed: %v — the manifest on disk may lag behind Redis; a resume adopts this run's "+
			"freezes and switches from Redis", err)
	}
	log.Printf("=== Relocate %s → %s: %s ===", srcSchema, dstSchema, m.relocSummary())
	if runErr != nil {
		log.Fatalf("relocate stopped: %v. Players frozen by run_id=%s stay frozen (go/db defers their writes, nothing is lost; "+
			"db_placement_guard_total{outcome=\"frozen_deferred\"} keeps rising): resume with the same command, or give up with "+
			"-mode relocate-abort -manifest-path %s -apply", runErr, m.RunID, manifestPath)
	}
	reportRelocateOutcome(m, manifestPath)
}

// reportRelocateOutcome 打收尾摘要。不是所有人都切换成功时 exit 1:没切过去的人要么留在 S(skipped / unfrozen),
// 要么需要人看(cas_failed),都不能当成「搬完了」。要再搬用新的 -manifest-path 另起一次(终态不会被续跑重做)。
func reportRelocateOutcome(m *relocateManifest, manifestPath string) {
	c := m.relocStateCounts()
	for _, state := range []string{relocCASFailed, relocUnfrozen, relocSkipped} {
		if c[state] > 0 {
			log.Printf("%d players %s (first: %v)", c[state], state, m.relocSample(state))
		}
	}
	if c[relocSwitched] == len(m.Players) {
		log.Printf("All %d players now live in storage %d. The rows left in storage %d are cold copies (P-1); "+
			"`-mode storage-audit -storage %d` shows what still points there.", len(m.Players), m.TargetStorage, m.SourceStorage, m.SourceStorage)
		return
	}
	log.Printf("NOT all players were relocated (manifest %s). Players left in storage %d keep working there; relocate them with "+
		"a new -manifest-path once the cause is fixed. cas_failed players need a human: their placement record changed under this run.",
		manifestPath, m.SourceStorage)
	os.Exit(1)
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
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()
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
	log.Printf("=== Relocate ABORT run_id=%s (%d → %d, dry-run=%v): %s ===", m.RunID, m.SourceStorage, m.TargetStorage,
		o.dryRun, m.relocSummary())
	log.Printf("    Make sure the relocate process of this run has exited: both would rewrite the same manifest.")
	mapRdb := mustDial(ctx, "mapping", o.mappingAddr, o.mappingPwd, o.mappingDB)
	defer mapRdb.Close()

	engine := newRelocateEngine(m, nil, redisRelocatePlacements{rdb: mapRdb}, nil, nil, nil, max(o.relocateBatch, 1), 0)
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
		log.Printf("ERROR: save the aborted manifest: %v", serr)
	}
	if err != nil {
		log.Fatalf("relocate-abort stopped: %v (%s) — re-run it; unfreezing is idempotent", err, rep)
	}
	log.Printf("=== Relocate aborted: %s — %s ===", rep, m.relocSummary())
	if rep.Failed > 0 {
		log.Printf("%d players could not be unfrozen: their record is no longer this run's frozen value (first: %v)",
			rep.Failed, m.relocSample(relocCASFailed))
		os.Exit(1)
	}
}
