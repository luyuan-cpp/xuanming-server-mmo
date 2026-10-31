package main

// 合服主流程(别名合服)。步骤顺序与理由见 main.go 顶部注释。
//
// 三条贯穿全流程的规矩:
//  1. **先证明,再写**。所有拒绝(前置检查、映射冲突、公会重名)都在第一次写之前跑完。
//  2. **清单先于写**。清单落盘之后才允许动第一个字节:撤销只认清单里列出的对象。
//  3. **映射先于业务合并**。映射生效、缓存刷新之后,新建的号 / 公会 / 商品都归到目标区,业务步骤才不会有漏网的行。
//
// 失败一律非零退出、步骤不标记完成;同一个 -manifest-path 重跑按 stepDone 续跑,每一步本身幂等。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"shared/authoritysql"
)

func runMerge(o options) {
	if err := requireSameSQLDomain(o.mysqlDSN, o.routeDBDSN); err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(maintenanceParent(o), o.timeout)
	defer cancel()
	var freeze *normalFreezeProof
	var freezeDB *sql.DB
	if !o.dryRun {
		if err := checkMaintenanceTool(ctx); err != nil {
			log.Fatal(err)
		}
		var freezeErr error
		freeze, freezeDB, freezeErr = openNormalFreezeProof(ctx, o)
		if freezeErr != nil {
			log.Fatal(freezeErr)
		}
		ctx = withNormalFreezeProof(ctx, freeze)
	}

	src, dst := o.sourceZone, o.targetZone
	now := time.Now()
	runID := newRunID(src, dst, now)
	manifestPath := o.manifestPath
	if manifestPath == "" {
		manifestPath = defaultManifestPath(src, dst, now)
	}

	log.Printf("=== Merge zone %d → %d (dry-run=%v apply=%v run_id=%s) ===", src, dst, o.dryRun, o.apply, runID)
	log.Printf("    manifest=%s timeout=%s settle=%s", manifestPath, o.timeout, o.mergedIntoSettle)
	if o.dryRun {
		log.Printf("    dry-run: the manifest preview goes to %s — a dry-run never writes -manifest-path itself",
			dryRunManifestPath(manifestPath))
	}
	log.Printf("    skip: guild=%v rank=%v trade=%v", o.skipGuild, o.skipRank, o.skipTrade)
	log.Printf("    redis: mapping=%s/db%d guild=%s/db%d login=%s/db%d scene=%s/db%d",
		o.mappingAddr, o.mappingDB, o.redisAddr, o.redisDB, o.noticeAddr, o.noticeDB, o.sceneAddr, o.sceneDB)
	log.Printf("    trade: schema=%s | guild: schema=%s", o.tradeSchema, o.guildSchema)

	// ── 句柄 ────────────────────────────────────────────────
	db := mustOpenMySQL(ctx, o.mysqlDSN, o.sqlBackend)
	defer closeUnownedMaintenanceResource(ctx, db)
	teamAuthority, err := openTeamAuthority(ctx, o.mysqlDSN, firstNonEmpty(o.teamSchema, defaultTeamSchema), o.sqlBackend)
	if err != nil {
		log.Fatalf("组队权威门禁: %v", err)
	}
	if err := registerMaintenanceResource(ctx, teamAuthority.db); err != nil {
		log.Fatal(err)
	}
	defer closeUnownedMaintenanceResource(ctx, teamAuthority.db)

	mapRdb := mustDial(ctx, "mapping", o.mappingAddr, o.mappingPwd, o.mappingDB)
	defer closeUnownedMaintenanceResource(ctx, mapRdb)
	guildRdb := mustDial(ctx, "guild", o.redisAddr, o.redisPwd, o.redisDB)
	defer closeUnownedMaintenanceResource(ctx, guildRdb)
	sharedRdb := mustDial(ctx, "login/shared(DB0)", o.noticeAddr, o.noticePwd, o.noticeDB)
	defer closeUnownedMaintenanceResource(ctx, sharedRdb)
	sceneRdb := mustDial(ctx, "scene_manager", o.sceneAddr, o.scenePwd, o.sceneDB)
	defer closeUnownedMaintenanceResource(ctx, sceneRdb)
	// preflight P2 要 etcd 注册表的正面证据(scene_registry.go)。在任何写入之前就校验参数。
	sceneNodes, rerr := newEtcdGatewayRegistry(o.etcdEndpoints)
	if rerr != nil {
		log.Fatalf("-etcd-endpoints is required: preflight P2 needs positive evidence that zone %d has no registered scene node: %v", src, rerr)
	}
	log.Printf("    scene-node registry: etcd endpoints=%s", strings.Join(sceneNodes.endpoints, ","))

	rb, err := openRunRouteBackend(ctx, routeEntryOptionsOf(o), mapRdb)
	if err != nil {
		log.Fatalf("route DB: %v", err)
	}
	if maintenanceFromContext(ctx) == nil {
		defer rb.Close()
	}

	// ── P1: 库表就绪 ────────────────────────────────────────
	// 两个 zone 库都得在。目标库不存在 = 目标 zone 从没部署过,把源区合过去等于指进虚空。
	srcSchema, dstSchema := zoneDBName(src), zoneDBName(dst)
	for _, s := range []string{srcSchema, dstSchema} {
		if err := assertSchemaExists(ctx, db, s); err != nil {
			log.Fatalf("preflight P1: %v (is -mysql-dsn pointing at the right database, and has that zone ever been deployed?)", err)
		}
	}
	log.Printf("preflight P1 OK: %s and %s both exist", srcSchema, dstSchema)

	// 聚宝斋库。trade_listing.market_zone 带 home_zone 语义(见 trade_step.go),不改写 = 源区商品在 zone 市场里对所有人消失。
	// 库 / 表不在就拒绝:「查不到」与「没有商品」在这里分不开(-mysql-dsn 指错实例也是这个症状)。
	if o.skipTrade {
		log.Printf("preflight P1: trade SKIPPED (-skip-trade-mysql) — %s.market_zone will NOT be rewritten",
			tradeListingQualified(o.tradeSchema))
	} else {
		if err := assertTradeListingReady(ctx, db, o.tradeSchema); err != nil {
			log.Fatalf("preflight P1: %v (run the trade migration first — `trade -f etc/trade.yaml -migrate` — "+
				"or pass -skip-trade-mysql ONLY if the trade service was never deployed in this environment)", err)
		}
		log.Printf("preflight P1 OK: %s exists", tradeListingQualified(o.tradeSchema))
	}

	// 帮会库。帮会表住在独占库 mmorpg_guild,不在 -mysql-dsn 的默认库里。理由与聚宝斋同:放过去 = 公会连同成员被漏在
	// 一个已下线的 zone 里。MySQL 步和榜单步都要读 guild 表,所以两个跳过开关都给了才算真正不碰帮会。
	if o.skipGuild && o.skipRank {
		log.Printf("preflight P1: guild SKIPPED (-skip-guild-mysql -skip-guild-rank) — %s.zone_id will NOT be rewritten",
			guildQualified(o.guildSchema, guildTable))
	} else {
		if err := assertGuildTablesReady(ctx, db, o.guildSchema); err != nil {
			log.Fatalf("preflight P1: %v (run the guild migration first — `guild -f etc/guild.yaml -migrate` — "+
				"or pass -skip-guild-mysql and -skip-guild-rank ONLY if the guild service was never deployed in this environment)", err)
		}
		log.Printf("preflight P1 OK: %s and %s exist",
			guildQualified(o.guildSchema, guildTable), guildQualified(o.guildSchema, guildMemberTable))
	}

	// ── R: 既有清单 ─────────────────────────────────────────
	// dry-run 预览(<path>.dryrun.json / dry_run=true)一律拒读:那是彩排那一刻的名单。
	existing, err := loadRunManifest(manifestPath)
	if err != nil {
		log.Fatalf("load manifest: %v", err)
	}
	if err := validateManifestForRun(existing, src, dst); err != nil {
		log.Fatalf("manifest mismatch: %v", err)
	}
	if err := refuseManifestBeingUnmerged(existing); err != nil {
		log.Fatalf("%v", err)
	}

	// ── P2 / P6 / P7: 源区已停干净 ──────────────────────────
	// 合服不改 home_zone,源区玩家随时可以按 home_zone 重查,续跑也一样。之前别名进源区的玩家也算在内。
	players, err := collectPlayerIDsResolvingThrough(ctx, rb.db, src)
	if err != nil {
		log.Fatalf("list players in source zone from player_route: %v", err)
	}
	log.Printf("Route: %d players resolve through zone %d (their rows stay as they are; they resolve to zone %d once the mapping is live)",
		len(players), src, dst)
	if err := runPreflight(ctx, preflightDeps{
		teamAuthority: teamAuthority,
		sceneRdb:      sceneRdb,
		sceneNodes:    sceneNodes,
		sharedRdb:     sharedRdb,
	}, preflightParams{
		readOnly:  o.dryRun,
		zone:      src,
		scope:     "source",
		playerIDs: players,
	}); err != nil {
		log.Fatalf("%v", err)
	}

	// ── X: 映射冲突 ─────────────────────────────────────────
	if err := checkMergedIntoOwnedByManifest(ctx, rb, existing, src, dst); err != nil {
		log.Fatalf("%v", err)
	}
	if err := stepCheckMergedIntoBeforeMerge(ctx, rb, src, dst); err != nil {
		log.Fatalf("%v", err)
	}
	if err := checkResumeMergedInto(ctx, rb, existing, src, dst); err != nil {
		log.Fatalf("manifest %s: %v", manifestPath, err)
	}

	// ── N: 公会重名断言 ─────────────────────────────────────
	if !o.skipGuild && !existing.stepDone(stepGuildMySQL) {
		if err := assertNoGuildNameCollision(ctx, db, o.guildSchema, src, dst); err != nil {
			log.Fatalf("guild: %v", err)
		}
	}

	// ── M: 清单(在第一次写之前落盘)────────────────────────
	m := existing
	if m == nil {
		m = newMergeManifest(runID, src, dst, operatorTag(), now)
	}
	if err := fillMergeManifest(ctx, m, db, guildRdb, o); err != nil {
		log.Fatalf("%v", err)
	}
	log.Printf("Manifest: %d guilds, %d rank members, %d trade listings", len(m.GuildIDs), len(m.RankMembers), len(m.TradeListingIDs))
	if o.dryRun {
		// 预览写到 <path>.dryrun.json 并标 dry_run,-manifest-path 一个字节不碰。
		m.DryRun = true
		preview := dryRunManifestPath(manifestPath)
		if err := saveManifest(preview, m); err != nil {
			log.Fatalf("write dry-run manifest preview %s: %v", preview, err)
		}
		log.Printf("DRY-RUN: manifest preview written to %s (for humans only — -apply, -mode unmerge and "+
			"-verify-merged refuse to read it; %s was not touched)", preview, manifestPath)
	} else {
		if err := saveManifest(manifestPath, m); err != nil {
			log.Fatalf("write manifest before first write: %v", err)
		}
		log.Printf("Manifest written to %s BEFORE any write. Keep it: -mode unmerge and -verify-merged need it.", manifestPath)
	}

	// ── 1~6: 写 ─────────────────────────────────────────────
	gate := writeGate(func() error {
		if o.dryRun {
			return nil
		}
		if err := checkMaintenanceTool(ctx); err != nil {
			return err
		}
		return freeze.check(ctx, freezeDB)
	})
	ops := &liveMergeSteps{o: o, src: src, dst: dst, m: m, manifestPath: manifestPath, db: db, guildRdb: guildRdb, rb: rb, gate: gate}
	persist := func(step, detail string) error {
		m.markStep(step, detail)
		return saveManifest(manifestPath, m)
	}
	if err := runMergeSteps(ctx, mergeStepPlan(ops, o), m, gate, o.dryRun, persist); err != nil {
		log.Fatalf("%v — re-run the same command with -manifest-path %s to resume: finished steps are skipped and every step is idempotent",
			err, manifestPath)
	}

	if o.dryRun {
		log.Printf("=== Done === DRY-RUN: nothing was written to SQL or Redis (only the preview %s).", dryRunManifestPath(manifestPath))
		return
	}
	log.Printf("=== Done === Manifest: %s — keep it: -verify-merged -manifest-path %s and -mode unmerge need it", manifestPath, manifestPath)
}

// fillMergeManifest 把这一次要动的业务对象并进清单(第一次写之前调用)。
func fillMergeManifest(ctx context.Context, m *mergeManifest, db *sql.DB, guildRdb *redis.Client, o options) error {
	src := m.SourceZone
	if !o.skipGuild || !o.skipRank {
		// 只要 guild 步骤还没做完,每次都把「当前 zone_id=src」并进清单。步骤做完之后不再收集:那时清单里的 id 就是撤销的唯一依据。
		// -skip-guild-mysql(只做榜单)时清单为空才收集一次。
		if (!o.skipGuild && !m.stepDone(stepGuildMySQL)) || len(m.GuildIDs) == 0 {
			gids, err := collectGuildIDsInZone(ctx, db, o.guildSchema, src)
			if err != nil {
				return fmt.Errorf("list guilds in source zone: %w", err)
			}
			m.GuildIDs = sortedUint64(append(m.GuildIDs, gids...))
		}
		// 榜单快照只在清单里还没有、且榜单步骤还没做完时取一次。步骤以维护锁内重读的源榜为准,快照从不被写进目标榜,
		// 只在「上一次写入之前落盘的那一份」时充当撤销依据 —— 所以这里不能每次重读覆盖它。
		// RankMembersUnwritten 标明「这份只是清单阶段读的,还没交给过任何一次写」。
		if len(m.RankMembers) == 0 && !m.stepDone(stepGuildRank) {
			members, err := readZoneRankMembers(ctx, guildRdb, src)
			if err != nil {
				return fmt.Errorf("read source rank ZSET: %w", err)
			}
			m.RankMembers = members
			m.RankMembersUnwritten = true
		}
	}
	// 聚宝斋商品 id:只要 trade 步骤还没做完,每次都把「当前 market_zone=src」并进清单;步骤做完之后不再收集。
	if !o.skipTrade && !m.stepDone(stepTradeMySQL) {
		lids, err := collectTradeListingIDsInZone(ctx, db, o.tradeSchema, src)
		if err != nil {
			return fmt.Errorf("list trade listings in source zone: %w", err)
		}
		m.TradeListingIDs = sortedUint64(append(m.TradeListingIDs, lids...))
	}
	return nil
}

// ── 清单落盘之后的步骤 ────────────────────────────────────────

// mergeStepOps 是清单落盘之后每一步实际做的写,返回写进清单的一行摘要。生产实现 liveMergeSteps;单测注入记录顺序的假实现。
type mergeStepOps interface {
	writeMergedInto(ctx context.Context) (string, error)
	settleMergedInto(ctx context.Context) (string, error)
	migrateGuilds(ctx context.Context) (string, error)
	migrateTrade(ctx context.Context) (string, error)
	mergeRank(ctx context.Context) (string, error)
}

// mergeStep 是清单落盘之后的一步。skip 是 -skip-* 跳过:不做、也不标记完成(之后不带开关重跑还能补上)。
type mergeStep struct {
	name string
	skip bool
	run  func(ctx context.Context) (string, error)
}

// mergeStepPlan 给出清单落盘之后的步骤,数组顺序就是执行顺序,理由见 main.go 顶部注释:映射必须先于任何业务合并。
func mergeStepPlan(ops mergeStepOps, o options) []mergeStep {
	return []mergeStep{
		{name: stepMergedInto, run: ops.writeMergedInto},
		{name: stepMergedIntoSettle, run: ops.settleMergedInto},
		{name: stepGuildMySQL, skip: o.skipGuild, run: ops.migrateGuilds},
		{name: stepTradeMySQL, skip: o.skipTrade, run: ops.migrateTrade},
		{name: stepGuildRank, skip: o.skipRank, run: ops.mergeRank},
	}
}

// runMergeSteps 按顺序跑步骤:清单已标记完成的跳过(续跑),每一步开始之前查 gate,做完立刻落盘,全部做完再写完成步骤。
// 任何失败都立即返回,该步骤不标记完成。dry-run 不落盘。
func runMergeSteps(ctx context.Context, steps []mergeStep, m *mergeManifest, gate writeGate, dryRun bool,
	persist func(step, detail string) error) error {
	for _, s := range steps {
		if s.skip {
			log.Printf("step %s skipped by flag — not marked done", s.name)
			continue
		}
		if m.stepDone(s.name) {
			log.Printf("step %s already done per manifest — skipping", s.name)
			continue
		}
		if err := gate.check(); err != nil {
			return fmt.Errorf("step %s: the write gate failed before it started, nothing of it was written: %w", s.name, err)
		}
		detail, err := s.run(ctx)
		if err != nil {
			return fmt.Errorf("step %s failed and is NOT marked done: %w", s.name, err)
		}
		log.Printf("step %s (%s): %s", s.name, verbWrite(dryRun), detail)
		if dryRun {
			continue
		}
		if err := persist(s.name, detail); err != nil {
			return fmt.Errorf("step %s ran but the manifest could not be saved, so it is NOT marked done: %w", s.name, err)
		}
	}
	if dryRun || m.stepDone(stepMergeDone) {
		return nil
	}
	if err := persist(stepMergeDone, "all steps finished"); err != nil {
		return fmt.Errorf("step %s: the manifest could not be saved: %w", stepMergeDone, err)
	}
	return nil
}

// liveMergeSteps 是 mergeStepOps 的生产实现。
type liveMergeSteps struct {
	o            options
	src, dst     uint32
	m            *mergeManifest
	manifestPath string
	db           *sql.DB
	guildRdb     *redis.Client
	rb           *routeBackend
	gate         writeGate
}

func (s *liveMergeSteps) writeMergedInto(ctx context.Context) (string, error) {
	if err := stepMarkMergedInto(ctx, s.rb, s.src, s.dst, s.m.RunID, s.o.dryRun); err != nil {
		return "", fmt.Errorf("write zone_merged_into(%d): %w", s.src, err)
	}
	return fmt.Sprintf("zone_merged_into(%d)=%d run_id=%s", s.src, s.dst, s.m.RunID), nil
}

// settleMergedInto 等各服务的映射缓存刷新。之后新建的号 / 公会 / 商品都已归到目标区,滞后窗口内落到源区的由下面的重收兜底。
func (s *liveMergeSteps) settleMergedInto(ctx context.Context) (string, error) {
	if s.o.dryRun {
		return fmt.Sprintf("would wait %s for the mapping caches", s.o.mergedIntoSettle), nil
	}
	log.Printf("waiting %s for the services to reload zone_merged_into", s.o.mergedIntoSettle)
	if err := sleepCtx(ctx, s.o.mergedIntoSettle); err != nil {
		return "", err
	}
	return fmt.Sprintf("waited %s", s.o.mergedIntoSettle), nil
}

// migrateGuilds 改写公会 zone_id。只改清单里的 id,逐条主键点更新(锁序理由见 rewriteZoneByPrimaryKey)。
// 重跑时改写按 `zone_id = src` 幂等(已搬的影响 0 行),失效对全清单再做一遍。
func (s *liveMergeSteps) migrateGuilds(ctx context.Context) (string, error) {
	o, m := s.o, s.m
	// 映射生效之前最后一个读到旧缓存的建帮请求已经结束:这里重收一次,兜住清单落盘到缓存刷新之间落到源区的公会。
	gids, err := collectGuildIDsInZone(ctx, s.db, o.guildSchema, s.src)
	if err != nil {
		return "", fmt.Errorf("list guilds in source zone: %w", err)
	}
	m.GuildIDs = sortedUint64(append(m.GuildIDs, gids...))
	if !o.dryRun {
		if err := saveManifest(s.manifestPath, m); err != nil {
			return "", fmt.Errorf("record the guilds in the manifest before rewriting: %w", err)
		}
	}
	rows, err := migrateGuildZone(ctx, s.db, o.guildSchema, m.GuildIDs, s.src, s.dst, o.dryRun, s.gate)
	if err != nil {
		return "", err
	}
	// guild 是全局服务,不随 zone-down 重启;30 分钟的 guild:v2 缓存里存着旧 zone_id,不失效就等于合服后半小时公会还挂在死掉的区上。
	// 放在复查之前:复查中止时,已经搬过去的那些公会的缓存也已与库一致。
	inv, err := invalidateGuildCaches(ctx, s.guildRdb, m.GuildIDs, o.dryRun)
	if err != nil {
		return "", fmt.Errorf("zone_id rewritten on %d rows, but invalidating guild:v2 caches failed: %w", rows, err)
	}
	if !o.dryRun {
		left, err := countGuildsInZone(ctx, s.db, o.guildSchema, s.src)
		if err != nil {
			return "", fmt.Errorf("re-check the source zone after rewriting: %w", err)
		}
		if left > 0 {
			return "", fmt.Errorf("%d guild(s) still carry zone_id=%d: they reached the source zone after this step collected its list "+
				"(an internal / GM path, or a service whose mapping cache is older than -merged-into-settle-sec). "+
				"Stop that path; the resumed run adds them to the manifest before rewriting", left, s.src)
		}
	}
	return fmt.Sprintf("rows=%d manifest_guilds=%d cache_invalidated=%d", rows, len(m.GuildIDs), inv), nil
}

// migrateTrade 改写聚宝斋 market_zone,口径同 migrateGuilds。seller_zone_at_listing 不改:它是上架时的原值,审计用。
func (s *liveMergeSteps) migrateTrade(ctx context.Context) (string, error) {
	o, m := s.o, s.m
	lids, err := collectTradeListingIDsInZone(ctx, s.db, o.tradeSchema, s.src)
	if err != nil {
		return "", fmt.Errorf("list trade listings in source zone: %w", err)
	}
	m.TradeListingIDs = sortedUint64(append(m.TradeListingIDs, lids...))
	if !o.dryRun {
		if err := saveManifest(s.manifestPath, m); err != nil {
			return "", fmt.Errorf("record the listings in the manifest before rewriting: %w", err)
		}
	}
	rows, err := migrateTradeMarketZone(ctx, s.db, o.tradeSchema, m.TradeListingIDs, s.src, s.dst, o.dryRun, s.gate)
	if err != nil {
		return "", err
	}
	if !o.dryRun {
		left, err := countTradeListingsInZone(ctx, s.db, o.tradeSchema, s.src)
		if err != nil {
			return "", fmt.Errorf("re-check the source zone after rewriting: %w", err)
		}
		if left > 0 {
			return "", fmt.Errorf("%d listing(s) still carry market_zone=%d: they reached the source zone after this step collected its list "+
				"(a seeding path, or a service whose mapping cache is older than -merged-into-settle-sec). "+
				"Stop that path; the resumed run adds them to the manifest before rewriting", left, s.src)
		}
	}
	return fmt.Sprintf("rows=%d manifest_listings=%d", rows, len(m.TradeListingIDs)), nil
}

// mergeRank 合并公会榜(维护锁内重读 + MULTI/EXEC,见 mergeGuildRank)。清单先于写:把撤销依据先落盘(步骤不标完成)。
func (s *liveMergeSteps) mergeRank(ctx context.Context) (string, error) {
	m := s.m
	members, sourceGone, err := mergeGuildRank(ctx, s.guildRdb, s.src, s.dst, m.RankMembers, !m.RankMembersUnwritten, s.o.dryRun,
		func(toRecord []manifestRankMember) error {
			m.RankMembers = toRecord
			m.RankMembersUnwritten = false
			return saveManifest(s.manifestPath, m)
		})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("members=%d source_gone=%v", len(members), sourceGone), nil
}

// ── 路由库装配 ───────────────────────────────────────────────

// writeGate 是写步骤每批之前的判据:本工具身份仍有效、原 NORMAL 冻结收据仍是当前结局。非 nil 即中止。nil gate 不查。
type writeGate func() error

func (g writeGate) check() error {
	if g == nil {
		return nil
	}
	return g()
}

// routeEntryOptions 是各入口开路由库需要的参数(main 装配:routeEntryOptionsOf)。dbDSN 含口令,不进日志。
type routeEntryOptions struct {
	dbDSN            string
	sqlBackend       authoritysql.Backend
	waitAOFLocal     int
	waitAOFReplicas  int
	waitAOFTimeoutMs int
}

func routeEntryOptionsOf(o options) routeEntryOptions {
	return routeEntryOptions{
		dbDSN:            o.routeDBDSN,
		sqlBackend:       o.sqlBackend,
		waitAOFLocal:     o.waitAOFLocal,
		waitAOFReplicas:  o.waitAOFReplicas,
		waitAOFTimeoutMs: o.waitAOFTimeoutMs,
	}
}

// routeHomeScanner 是按 home_zone 键集翻页读 player_route 的接缝(只读查询,门禁与统计用)。
type routeHomeScanner interface {
	ScanByHome(ctx context.Context, zone uint32, after routeScanCursor, limit int) ([]mergeRouteRow, error)
	ListMergedInto(ctx context.Context) (map[uint32]uint32, error)
}

// routeDBOps 是写步骤依赖的库原语(生产 *mergeRouteStore,route_store.go;单测注入假实现)。
type routeDBOps interface {
	routeHomeScanner
	ReadMergedInto(ctx context.Context, src uint32) (dst uint32, runID string, found bool, err error)
	MarkMergedInto(ctx context.Context, src, dst uint32, markRunID string) (string, error)
	ClearMergedInto(ctx context.Context, src, dst uint32, markRunID string) (bool, error)
}

// routeCacheOps 是发布层(生产 *routePublisher,route_publish.go):合走标记的镜像,带 WAITAOF。
type routeCacheOps interface {
	setMergedIntoMirror(ctx context.Context, src uint32, value string) error
	deleteMergedIntoMirror(ctx context.Context, src uint32, value string) (bool, error)
}

// routeBackend 是一次运行的路由库与发布层。
type routeBackend struct {
	db    routeDBOps
	pub   routeCacheOps
	close func() error
}

// Close 关闭路由库连接池。
func (rb *routeBackend) Close() {
	if rb == nil || rb.close == nil {
		return
	}
	if err := rb.close(); err != nil {
		log.Printf("WARN: close the route DB pool: %v", err)
	}
}

// openRunRouteBackend 在连上 mapping Redis 之后、任何读写映射之前调用:开路由库(探针读回会话与 route_meta)→ 要求水位键存在、
// 可解码、世代等于该库 route_meta.generation(库与 Redis 成对)→ 返回库与发布层。
func openRunRouteBackend(ctx context.Context, ro routeEntryOptions, mapRdb *redis.Client) (*routeBackend, error) {
	dur, err := newRouteDurability(ro.waitAOFLocal, ro.waitAOFReplicas, ro.waitAOFTimeoutMs)
	if err != nil {
		return nil, err
	}
	pub := newRoutePublisher(mapRdb, dur)
	st, err := openPairedRouteStore(ctx, ro.dbDSN, ro.sqlBackend, pub)
	if err != nil {
		return nil, err
	}
	log.Printf("route DB: zone_merged_into in the global DB is authoritative; the mapping is committed there first and then mirrored "+
		"to the mapping Redis with WAITAOF %d %d (timeout %s)", dur.NumLocal, dur.NumReplicas, dur.Timeout)
	return &routeBackend{db: st, pub: pub, close: st.Close}, nil
}

// collectPlayerIDsWithHomeZoneDB 按 home_zone 键集翻页读完 player_route(idx_route_home,ScanByHome 逐行校验),返回升序去重的
// player_id。翻页游标是聚簇键 (route_shard, player_id),与返回值的 player_id 顺序不同,所以最后统一排序。一页不足 routeReadBatchLimit 即翻完。
func collectPlayerIDsWithHomeZoneDB(ctx context.Context, db routeHomeScanner, zone uint32) ([]uint64, error) {
	var out []uint64
	var after routeScanCursor
	for {
		page, err := db.ScanByHome(ctx, zone, after, routeReadBatchLimit)
		if err != nil {
			return nil, fmt.Errorf("scan player_route by home_zone=%d: %w", zone, err)
		}
		for _, row := range page {
			out = append(out, row.PlayerID)
		}
		if len(page) < routeReadBatchLimit {
			return sortedUint64(out), nil
		}
		after = routeCursorOf(page[len(page)-1].PlayerID)
	}
}

// mergedZoneMaxHops 是解析链的长度上限,与 shared/placement.ResolveMergedZone 相同。
const mergedZoneMaxHops = 16

// zonesResolvingThrough 返回归属解析会经过 zone 的所有区:它自己,加上之前别名进来的。成环或超长拒绝。
func zonesResolvingThrough(merges map[uint32]uint32, zone uint32) ([]uint32, error) {
	out := []uint32{zone}
	for src := range merges {
		cur := src
		for hop := 0; ; hop++ {
			if hop >= mergedZoneMaxHops {
				return nil, fmt.Errorf("zone_merged_into chain from zone %d is longer than %d hops or has a cycle", src, mergedZoneMaxHops)
			}
			next, ok := merges[cur]
			if !ok {
				break
			}
			if next == zone {
				out = append(out, src)
				break
			}
			cur = next
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// collectPlayerIDsResolvingThrough 列出归属解析经过 zone 的玩家。门禁不能只看 home_zone=zone:已有 4→5 时合 5→6,
// home_zone=4 的玩家此刻也在 5 区。漏掉他们的话,他们还在线、还在队伍里也会放行,队伍记录的 zone 留在已合走的区。
func collectPlayerIDsResolvingThrough(ctx context.Context, db routeHomeScanner, zone uint32) ([]uint64, error) {
	merges, err := db.ListMergedInto(ctx)
	if err != nil {
		return nil, err
	}
	zones, err := zonesResolvingThrough(merges, zone)
	if err != nil {
		return nil, err
	}
	var out []uint64
	for _, z := range zones {
		ids, err := collectPlayerIDsWithHomeZoneDB(ctx, db, z)
		if err != nil {
			return nil, err
		}
		out = append(out, ids...)
	}
	return sortedUint64(out), nil
}

// ── 句柄与小工具 ──────────────────────────────────────────────

// zoneDBName 镜像 go/db/internal/config.ZoneDBName;merge_zone 是独立 module,不引主工程包。改一处必须同步另一处。
func zoneDBName(zone uint32) string {
	return fmt.Sprintf("zone_%d_db", zone)
}

// schemaNamePattern:MySQL 未加引号标识符里最保守的子集,长度上限 64 同 MySQL。
// 库名不是绑定参数,只能直接拼进语句,所以形状校验是这条拼接唯一的注入防线;-trade-schema / -guild-schema / -friend-schema 共用它。
var schemaNamePattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

// assertSchemaExists 证明库存在。
func assertSchemaExists(ctx context.Context, db *sql.DB, schema string) error {
	var name string
	err := db.QueryRowContext(ctx,
		"SELECT SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", schema).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("database %s does not exist", schema)
	}
	if err != nil {
		return fmt.Errorf("look up schema %s: %w", schema, err)
	}
	return nil
}

// tableColumns 返回表的列名(按 ORDINAL_POSITION)。
func tableColumns(ctx context.Context, db *sql.DB, schema, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT COLUMN_NAME FROM information_schema.COLUMNS
		  WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? ORDER BY ORDINAL_POSITION`, schema, table)
	if err != nil {
		return nil, fmt.Errorf("columns of %s.%s: %w", schema, table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s.%s has no columns (does it exist?)", schema, table)
	}
	return out, nil
}

// idListBatchSize 是分批 IN (...) 计数的批量(非锁定读)。
const idListBatchSize = 1000

// chunkUint64 分批。批次边界只由长度决定,所以同一份有序 id 列表每次分法一致。
func chunkUint64(in []uint64, size int) [][]uint64 {
	if size <= 0 {
		size = 1
	}
	var out [][]uint64
	for start := 0; start < len(in); start += size {
		end := start + size
		if end > len(in) {
			end = len(in)
		}
		out = append(out, in[start:end])
	}
	return out
}

// inListLiteral 把 id 拼成 SQL IN 列表。**只接受 uint64**,所以不存在注入面:
// 数字直接格式化比占位符省一次协议往返,也让 EXPLAIN 出来的 SQL 可读。
func inListLiteral(ids []uint64) string {
	var b strings.Builder
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatUint(id, 10))
	}
	return b.String()
}

// mustOpenMySQL保留旧调用名；全球合服/撤销只交付按档位过了当前读探针的句柄。
//
// 为什么显式设 RC(2026-09-21 死锁审计 #17):
//   - 此前直接用运维给的 DSN,未设隔离级别 = 实例默认 RR。RR 下 UPDATE 对扫描到的二级项加 next-key,
//     对不存在的主键加间隙锁,锁集会越出清单;在线服务(guild / trade / friend)的写事务都是 RC,
//     本工具与它们同口径,锁行为才能按同一套「主键 → 二级」推演。
//
// 风险:binlog_format=STATEMENT 的实例在 RC 下拒绝 InnoDB 写。在线服务已经在同一实例上以 RC 写,
// 这不是新约束;真遇上时第一条写就会响亮失败,不会静默写一半。
// DSN 本身含密码,任何日志都不打印它(ParseDSN 的错误也不回显 DSN 原文)。
func mustOpenMySQL(ctx context.Context, dsn string, backend authoritysql.Backend) *sql.DB {
	db, err := openCurrentSession(ctx, dsn, backend)
	if err != nil {
		log.Fatalf("全局SQL当前读连接: %v", err)
	}
	return db
}

const (
	// mysqlIsolationParam 经 go-sql-driver 的 DSN 参数变成连接建立时的 `SET transaction_isolation = ...`
	// (v1.9.2 connection.go handleParams:未知参数一律当会话变量 SET),连接池里每条新连接都会带上。
	mysqlIsolationParam = "transaction_isolation"
	// mysqlLegacyIsolationParam 是同一个变量的旧名(MySQL 5.7 早期;8.0 已删)。两个名字同时出现时,驱动按
	// map 迭代顺序拼进同一条 SET,最后生效的是哪一个不确定 —— 所以一律删掉旧名。
	mysqlLegacyIsolationParam = "tx_isolation"
	// mysqlReadCommitted 带引号:值原样拼进 SET 语句,READ-COMMITTED 里的连字符不加引号就是语法错。
	mysqlReadCommitted = "'READ-COMMITTED'"
)

// readCommittedDSN保留旧调用名；固定RC，tidb 档同时固定全局TiDB当前强读策略，保留连接目标与非权威策略参数。
// 用 ParseDSN / FormatDSN 改结构而不是手拼字符串:参数的 URL 编码、密码里的特殊字符都交给驱动处理。
// 历史快照、旧副本、低精度TSO和旧隔离级别一律覆盖；不回显原参数值，防止配置进入日志。
func readCommittedDSN(dsn string, backend authoritysql.Backend) (string, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return "", fmt.Errorf("parse -mysql-dsn: %w", err)
	}
	pinCurrentSession(cfg, backend)
	return cfg.FormatDSN(), nil
}

// ── 按主键逐行改 zone 列(guild.zone_id / trade_listing.market_zone 共用)─────────

const (
	mysqlErrDeadlock        = 1213 // ER_LOCK_DEADLOCK
	mysqlErrLockWaitTimeout = 1205 // ER_LOCK_WAIT_TIMEOUT
	tidbErrWriteConflict    = 9007 // TiDB 乐观事务写冲突(全局数据层迁往 TiDB 后同样可能出现)
)

// isRetryableLockConflict 判断错误是否是可重试的锁冲突。取值与 go/guild、go/trade 的事务重试判定一致
// (1213 / 1205 / 9007);本工具是独立 module,不能 import 它们。
func isRetryableLockConflict(err error) bool {
	var me *mysql.MySQLError
	if !errors.As(err, &me) {
		return false
	}
	switch me.Number {
	case mysqlErrDeadlock, mysqlErrLockWaitTimeout, tidbErrWriteConflict:
		return true
	default:
		return false
	}
}

// lockRetryPolicy 是单行改写遇到锁冲突时的有界重试(次数上限 + 指数退避封顶 + 可取消)。
//
// 收敛论证:被重试的是**一个 id 一个的显式短事务**(rewriteOneZoneRow:锁主键行 → 改写 → 提交)。
// 1213 时 InnoDB 回滚整个事务,1205 时只回滚那条语句 —— 所以出错的尝试一律由 rewriteOneZoneRow 自己
// 回滚整个事务,重试从一个新事务开始,上一次什么也没留下;UPDATE 的 WHERE 带着旧 zone,重跑幂等
// (已改过的行不再命中)。事务在拿到那一行的主键锁之前不持有任何锁,按推演不会成环;真出现 1213 / 1205
// 只能是对端长事务或执行计划没走主键,这时有限次数后交给人,而不是无限重试把维护窗口耗光。
// 不加抖动:本工具是单进程串行的唯一写者,没有需要彼此错开的同伴重试。
type lockRetryPolicy struct {
	attempts    int
	baseBackoff time.Duration
	maxBackoff  time.Duration
	// sleep 为 nil 时用 sleepCtx;单测注入记录型实现,不依赖真实墙钟。
	sleep func(ctx context.Context, d time.Duration) error
}

// zoneRewriteRetry:最坏情况 5 次 × innodb_lock_wait_timeout(默认 50s)+ 退避约 3s,仍远小于 -timeout 默认 2h。
var zoneRewriteRetry = lockRetryPolicy{attempts: 5, baseBackoff: 200 * time.Millisecond, maxBackoff: 5 * time.Second}

// backoff 返回第 retry 次重试(从 0 起)前的等待:base·2^retry,封顶 maxBackoff。
func (p lockRetryPolicy) backoff(retry int) time.Duration {
	d := p.baseBackoff
	for i := 0; i < retry && d < p.maxBackoff; i++ {
		d *= 2
	}
	return min(d, p.maxBackoff)
}

// do 执行 op,仅对 isRetryableLockConflict 认可的错误重试。每次尝试前先看 ctx;op 必须可整体重跑。
// what 只进日志与最终错误文案。
func (p lockRetryPolicy) do(ctx context.Context, what string, op func() error) error {
	sleep := p.sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	attempts := max(p.attempts, 1)
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return errors.Join(lastErr, err)
		}
		err := op()
		if err == nil {
			return nil
		}
		if !isRetryableLockConflict(err) {
			return err
		}
		lastErr = err
		if attempt == attempts {
			break // 最后一次失败后不必再睡
		}
		wait := p.backoff(attempt - 1)
		log.Printf("WARN: %s lock conflict (attempt %d/%d): %v -- retrying after %s", what, attempt, attempts, err, wait)
		if serr := sleep(ctx, wait); serr != nil {
			return errors.Join(lastErr, serr)
		}
	}
	return fmt.Errorf("%s:锁冲突重试 %d 次仍失败:%w", what, attempts, lastErr)
}

// sleepCtx 可取消地等待 d。
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// zonePointUpdateSQL 是「按主键逐行改 zone 列」唯一的 UPDATE 形状,参数依次为 (to, pk, from)。
// table 已是 schema.table(库名过了 schemaNamePattern),pkCol / zoneCol 是本工具的常量,不来自输入。
// 集成测试对同一个函数的产物做 EXPLAIN,断言走满主键 —— 测试里不另抄一份 SQL。
func zonePointUpdateSQL(table, pkCol, zoneCol string) string {
	return "UPDATE " + table + " SET " + zoneCol + " = ? WHERE " + pkCol + " = ? AND " + zoneCol + " = ?"
}

// zoneLockSelectSQL 是单行事务的第一句:按完整主键等值锁住那一行、读出当前 zone,参数为 (pk)。
// WHERE 里只有主键:锁的是主键记录,不经过 zone 二级索引(集成测试同样对它做 EXPLAIN 钉住 PRIMARY)。
func zoneLockSelectSQL(table, pkCol, zoneCol string) string {
	return "SELECT " + zoneCol + " FROM " + table + " WHERE " + pkCol + " = ? FOR UPDATE"
}

// zoneRewriteTarget 描述一张被逐行改 zone 列的表:SQL 由它拼,失败文案也用它。
type zoneRewriteTarget struct {
	table   string // schema.table
	pkCol   string
	zoneCol string
}

// zoneRowTx 是「按主键改一行 zone 列」的单行事务。生产实现 sqlZoneRowTx 包一个 *sql.Tx;
// 单测(lock_order_test.go)注入内存替身,验证加锁顺序、跳过、重试与回滚 —— 不需要真库。
type zoneRowTx interface {
	// lockZone:SELECT zone FROM t WHERE pk = ? FOR UPDATE。行不存在(已被删除)返回 found=false。
	lockZone(ctx context.Context, id uint64) (zone uint32, found bool, err error)
	// rewriteZone:UPDATE t SET zone = to WHERE pk = ? AND zone = from,返回影响行数。
	rewriteZone(ctx context.Context, id uint64, from, to uint32) (int64, error)
	commit() error
	rollback() error
}

// beginZoneRowTx 开一个单行事务。
type beginZoneRowTx func(ctx context.Context) (zoneRowTx, error)

// rewriteZoneByPrimaryKey 把 ids 里当前 zoneCol = from 的行逐条改成 to,返回实际改动的行数。
// 不在 ids 里的行一个都不碰;已经不在 from 的(改过了 / 去了第三个 zone / 已被删除)不改、计 0 行。
//
// 锁序(2026-09-21 死锁审计 #17,与 docs/ops/incident-friend-lock-order-deadlock-2026-09-21.md 同一口径:
// 守卫之后的锁定写一律是完整主键的等值点操作):
//
//	旧写法 `UPDATE t SET zone = dst WHERE zone = src [AND pk IN (...)]` 可被规划成沿 zone 二级索引的
//	range 扫描:先对二级项加 X、再回表锁主键。在线写者(guild 服 DisbandGuild;P3 起 trade 的商品状态
//	迁移)是先 `WHERE pk = ? FOR UPDATE` 锁主键、再在 DELETE / UPDATE 里改同一行的二级项 —— 两边反序,
//	合服扫到那一行时 1213,且牺牲者可能是合服这条大语句。
//	现在逐条按主键:主键等值是 const 访问,必走主键;同一时刻最多持有一行的主键锁及其自身的二级项,
//	拿到主键锁之前什么都不持有,取锁顺序与在线写者同为「主键 → 二级」,只会排队、不会成环。
//
// 每个 id 一个显式短事务:`SELECT … WHERE pk = ? FOR UPDATE` → 带 zone 复核的 UPDATE → COMMIT
// (2026-09-28,player-storage-placement.md §12 A15)。此前是每行一条自动提交的 UPDATE:MySQL 上等价,
// 但 TiDB 即便开了悲观事务,自动提交语句默认仍走乐观提交(pessimistic-auto-commit 默认关),不在执行时
// 排队拿行锁,而是在 2PC 提交时与 DisbandGuild 已持有的悲观锁互等 / 冲突。显式事务先用 FOR UPDATE 在执行期
// 拿到主键锁,MySQL 与 TiDB 悲观模式下行为一致;行已不在 from(或已删除)就不写,回滚放锁。
// 隔离级别由连接上的 transaction_isolation(readCommittedDSN)定成 RC,不在 BeginTx 里再指定 ——
// 那会让驱动每个事务多发一句 SET TRANSACTION。TiDB 侧的前提是 tidb_txn_mode=pessimistic(部署前提)。
//
// ids 升序处理:每个事务只碰一行,顺序本身不影响成环;升序只为日志与重跑可复现 —— 将来若改成「N 条一个
// 事务」,升序就是必要条件(且一个事务里只能改这一张表)。
// 成本:每行 4 次往返(BEGIN / SELECT / UPDATE / COMMIT)。两条语句各 Prepare 一次,事务里用 tx.Stmt 复用。
// 失败:锁冲突按 zoneRewriteRetry 有界重试整个事务;其余错误或重试耗尽立即返回,已提交的行留在 to。
// 调用方的步骤不标记完成,重跑按 `zone = from` 幂等补齐。
// gate 非 nil 时每个 id 的事务开始之前查一次(工具身份与冻结收据,见 writeGate);非 nil 错误即停,
// 此前已提交的行留在 to(与其它中途失败同一口径)。
func rewriteZoneByPrimaryKey(ctx context.Context, db *sql.DB, table, pkCol, zoneCol string,
	ids []uint64, from, to uint32, gate writeGate) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	if err := checkMaintenanceTool(ctx); err != nil {
		return 0, err
	}
	lockStmt, err := db.PrepareContext(ctx, zoneLockSelectSQL(table, pkCol, zoneCol))
	if err != nil {
		return 0, fmt.Errorf("prepare %s.%s 行锁读:%w", table, zoneCol, err)
	}
	defer lockStmt.Close()
	updateStmt, err := db.PrepareContext(ctx, zonePointUpdateSQL(table, pkCol, zoneCol))
	if err != nil {
		return 0, fmt.Errorf("prepare %s.%s 点更新:%w", table, zoneCol, err)
	}
	defer updateStmt.Close()
	return rewriteZoneRows(ctx, gatedZoneRowTxBeginner(sqlZoneRowTxBeginner(db, lockStmt, updateStmt), gate), zoneRewriteRetry,
		zoneRewriteTarget{table: table, pkCol: pkCol, zoneCol: zoneCol}, ids, from, to)
}

// gatedZoneRowTxBeginner 在每个单行事务开始之前查 gate;gate 为 nil 时原样返回 begin。
// gate 的错误不是锁冲突,lockRetryPolicy 不重试,rewriteZoneRows 带着「此前已改 N 行」原样上抛。
func gatedZoneRowTxBeginner(begin beginZoneRowTx, gate writeGate) beginZoneRowTx {
	if gate == nil {
		return begin
	}
	return func(ctx context.Context) (zoneRowTx, error) {
		if err := gate.check(); err != nil {
			return nil, err
		}
		return begin(ctx)
	}
}

// rewriteZoneRows 是 rewriteZoneByPrimaryKey 的算法本体(与 SQL 无关,单测直接驱动):
// 升序逐个 id,每个 id 一个事务,锁冲突按 retry 重试整个事务。
func rewriteZoneRows(ctx context.Context, begin beginZoneRowTx, retry lockRetryPolicy, target zoneRewriteTarget,
	ids []uint64, from, to uint32) (int64, error) {
	var total int64
	for _, id := range sortedUint64(ids) {
		what := fmt.Sprintf("%s %s=%d", target.table, target.pkCol, id)
		var n int64
		err := retry.do(ctx, what, func() error {
			var err error
			n, err = rewriteOneZoneRow(ctx, begin, id, from, to)
			return err
		})
		if err != nil {
			// 这一层就是完整上下文(表.列、方向、出错的主键、已改行数),调用方原样上抛、不再包一层。
			return total, fmt.Errorf("%s.%s %d → %d 在 %s=%d 处失败(此前已改 %d 行):%w",
				target.table, target.zoneCol, from, to, target.pkCol, id, total, err)
		}
		total += n
	}
	return total, nil
}

// rewriteOneZoneRow 是一个 id 的完整事务:锁主键行 → 复核 zone → 改写 → 提交。行不存在或已不在 from 时
// 不写,回滚放锁,计 0 行。任何一步出错都回滚整个事务再返回:1205 只回滚语句、不回滚事务,
// 重试必须从一个干净的新事务开始,否则上一次拿到的锁会一直挂着。
func rewriteOneZoneRow(ctx context.Context, begin beginZoneRowTx, id uint64, from, to uint32) (int64, error) {
	tx, err := begin(ctx)
	if err != nil {
		return 0, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.rollback()
		}
	}()
	zone, found, err := tx.lockZone(ctx, id)
	if err != nil {
		return 0, err
	}
	if !found || zone != from {
		return 0, nil
	}
	n, err := tx.rewriteZone(ctx, id, from, to)
	if err != nil {
		return 0, err
	}
	if err := tx.commit(); err != nil {
		return 0, err
	}
	committed = true
	return n, nil
}

// sqlZoneRowTx 是 zoneRowTx 的生产实现:一个 *sql.Tx 加上绑定到它的两条预编译语句。
type sqlZoneRowTx struct {
	tx       *sql.Tx
	lock     *sql.Stmt // tx 内的 zoneLockSelectSQL
	update   *sql.Stmt // tx 内的 zonePointUpdateSQL
	ctx      context.Context
	finish   func()
	finished *sync.Once
}

func (t sqlZoneRowTx) lockZone(ctx context.Context, id uint64) (uint32, bool, error) {
	var zone uint32
	err := t.lock.QueryRowContext(ctx, id).Scan(&zone)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		holdMaintenanceTool(t.ctx)
		return 0, false, err
	}
	return zone, true, nil
}

func (t sqlZoneRowTx) rewriteZone(ctx context.Context, id uint64, from, to uint32) (int64, error) {
	res, err := t.update.ExecContext(ctx, to, id, from)
	if err != nil {
		holdMaintenanceTool(t.ctx)
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		holdMaintenanceTool(t.ctx)
	}
	return n, err
}

func (t sqlZoneRowTx) terminal(op func() error) (err error) {
	returned := false
	defer func() {
		if !returned || (err != nil && !errors.Is(err, sql.ErrTxDone)) {
			holdMaintenanceTool(t.ctx)
		}
		t.finished.Do(t.finish)
	}()
	err = op()
	returned = true
	return
}
func (t sqlZoneRowTx) commit() error   { return t.terminal(t.tx.Commit) }
func (t sqlZoneRowTx) rollback() error { return t.terminal(t.tx.Rollback) }

// sqlZoneRowTxBeginner 返回生产用的 beginZoneRowTx。lock / update 是在 db 上预编译好的语句,
// tx.StmtContext 把它们绑到本事务的连接上(同一连接上只 prepare 一次)。
func sqlZoneRowTxBeginner(db *sql.DB, lock, update *sql.Stmt) beginZoneRowTx {
	return func(ctx context.Context) (zoneRowTx, error) {
		if err := checkMaintenanceTool(ctx); err != nil {
			return nil, err
		}
		proof := normalFreezeFromContext(ctx)
		if proof == nil {
			return nil, errNormalFreezeProof
		}
		finish, err := enterMaintenanceWork(ctx)
		if err != nil {
			return nil, err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			holdMaintenanceTool(ctx)
			finish()
			return nil, err
		}
		if err := proof.lockTransaction(ctx, tx); err != nil {
			rollbackErr := tx.Rollback()
			holdMaintenanceTool(ctx)
			finish()
			return nil, errors.Join(err, rollbackErr)
		}
		return sqlZoneRowTx{tx: tx, lock: tx.StmtContext(ctx, lock), update: tx.StmtContext(ctx, update), ctx: ctx, finish: finish, finished: &sync.Once{}}, nil
	}
}

func mustDial(ctx context.Context, label, addr, pwd string, dbIndex int) *redis.Client {
	opts := &redis.Options{Addr: addr, Password: pwd, DB: dbIndex}
	if maintenanceFromContext(ctx) != nil {
		opts.MaxRetries = -1
	}
	c := redis.NewClient(opts)
	if err := bindMaintenanceRedis(ctx, c); err != nil {
		log.Fatal(errors.Join(err, c.Close()))
	}
	if err := c.Ping(ctx).Err(); err != nil {
		log.Fatalf("%s redis ping (%s db=%d): %v", label, addr, dbIndex, err)
	}
	return c
}
