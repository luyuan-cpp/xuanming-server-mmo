package main

// 合服主流程。步骤顺序与理由见 main.go 顶部注释。
//
// 三条贯穿全流程的规矩:
//  1. **先证明,再写**。所有拒绝(preflight / guard)必须在第一次写之前跑完。
//  2. **清单先于写**。清单落盘之后才允许动第一个字节,否则出事时没人知道
//     动过谁。
//  3. **id 只收集一次**。remap 之后源区玩家在 mapping 里已经消失,任何
//     「再扫一遍」都会得到空集合并把「没做」误报成「做完了」。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
)

func runMerge(o options) {
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()

	src, dst := o.sourceZone, o.targetZone
	now := time.Now()
	runID := newRunID(src, dst, now)
	manifestPath := o.manifestPath
	if manifestPath == "" {
		manifestPath = defaultManifestPath(src, dst, now)
	}

	log.Printf("=== Merge zone %d → %d (dry-run=%v apply=%v run_id=%s) ===", src, dst, o.dryRun, o.apply, runID)
	log.Printf("    manifest=%s timeout=%s", manifestPath, o.timeout)
	log.Printf("    skip: guild=%v rank=%v mapping=%v player_rows=%v | blobs=%v(skip=%v) hot_state=%v",
		o.skipGuild, o.skipRank, o.skipMapping, o.skipRows, o.migrateBlobs, o.skipBlobs, o.clearHotState)
	log.Printf("    redis: mapping=%s/db%d guild=%s/db%d login=%s/db%d scene=%s/db%d",
		o.mappingAddr, o.mappingDB, o.redisAddr, o.redisDB, o.noticeAddr, o.noticeDB,
		o.sceneAddr, o.sceneDB)
	log.Printf("    trade: schema=%s skip=%v", o.tradeSchema, o.skipTrade)
	log.Printf("    guild: schema=%s skip_mysql=%v skip_rank=%v", o.guildSchema, o.skipGuild, o.skipRank)
	log.Printf("    friend: schema=%s (审计只读;合服不改写 friend 的行)", o.friendSchema)

	// ── 句柄 ────────────────────────────────────────────────
	db := mustOpenMySQL(ctx, o.mysqlDSN)
	defer db.Close()

	mapRdb := mustDial(ctx, "mapping", o.mappingAddr, o.mappingPwd, o.mappingDB)
	defer mapRdb.Close()
	guildRdb := mustDial(ctx, "guild", o.redisAddr, o.redisPwd, o.redisDB)
	defer guildRdb.Close()
	sharedRdb := mustDial(ctx, "login/shared(DB0)", o.noticeAddr, o.noticePwd, o.noticeDB)
	defer sharedRdb.Close()
	sceneRdb := mustDial(ctx, "scene_manager", o.sceneAddr, o.scenePwd, o.sceneDB)
	defer sceneRdb.Close()

	srcSchema, dstSchema := zoneDBName(src), zoneDBName(dst)

	// ── P: preflight ────────────────────────────────────────
	// P1 两个 zone 库都得在。目标库不存在 = 目标 zone 从没部署过,
	// 把玩家指过去等于指进虚空。
	for _, s := range []string{srcSchema, dstSchema} {
		if err := assertSchemaExists(ctx, db, s); err != nil {
			log.Fatalf("preflight P1: %v (is -mysql-dsn pointing at the right MySQL, and has that zone ever been deployed?)", err)
		}
	}
	log.Printf("preflight P1 OK: %s and %s both exist", srcSchema, dstSchema)

	tableCandidates, err := loadTableListJSON(o.tableListPath)
	if err != nil {
		log.Fatalf("preflight P1: %v (pass -table-list-json <path to generated/data/mysql_database_table_list.json>)", err)
	}
	playerTables, err := discoverPlayerTables(ctx, db, srcSchema, tableCandidates)
	if err != nil {
		log.Fatalf("preflight P1: %v", err)
	}
	if len(playerTables) == 0 {
		log.Fatalf("preflight P1: no table in %s has a %s column (candidates: %v). "+
			"Refusing to merge without knowing where player main data lives", srcSchema, playerIDColumn, tableCandidates)
	}
	log.Printf("preflight P1 OK: player tables in %s = %v", srcSchema, playerTables)

	// P1(续) 聚宝斋库。trade_listing.market_zone 带 home_zone 语义(见 trade_step.go),
	// 不改写 = 源区商品在 zone 市场里对所有人消失。库 / 表不在就拒绝:「查不到」与
	// 「没有商品」在这里分不开(-mysql-dsn 指错实例也是这个症状)。
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

	// P1(续) 帮会库。帮会表自二期 B1 起住在独占库 mmorpg_guild(D-14 §8),不在
	// -mysql-dsn 的默认库里。理由与聚宝斋同:「查不到」与「这个区没有公会」在查询结果上
	// 分不开,放过去 = 公会连同成员被漏在一个已下线的 zone 里。MySQL 步和榜单步都要读
	// guild 表,所以两个跳过开关都给了才算真正不碰帮会。
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

	lagSrc := buildLagSource(o)

	// ── 0: 收集玩家 id(只此一次)────────────────────────────
	// 先尝试从既有清单读:重跑时 mapping 已经被改过,重新扫描会得到空集合。
	existing, err := loadManifest(manifestPath)
	if err != nil {
		log.Fatalf("load manifest: %v", err)
	}
	if err := validateManifestForRun(existing, src, dst); err != nil {
		log.Fatalf("manifest mismatch: %v", err)
	}

	var playerIDs []uint64
	resumed := false
	if existing != nil && len(existing.PlayerIDs) > 0 {
		playerIDs = existing.PlayerIDs
		resumed = true
		log.Printf("RESUME: manifest %s lists %d players / %d guilds / %d rank members; "+
			"NOT re-scanning the mapping (after a remap the scan would return an empty set)",
			manifestPath, len(playerIDs), len(existing.GuildIDs), len(existing.RankMembers))
	} else {
		playerIDs, err = collectPlayerIDsWithHomeZone(ctx, mapRdb, src)
		if err != nil {
			log.Fatalf("list players in source zone: %v", err)
		}
		log.Printf("Mapping: %d players with home_zone=%d (%s db=%d)", len(playerIDs), src, o.mappingAddr, o.mappingDB)
	}

	// ── G: 守卫 ─────────────────────────────────────────────
	if err := guardPlayerSet(ctx, db, srcSchema, playerIDs, o, resumed); err != nil {
		log.Fatalf("%v", err)
	}

	// ── P(续): 需要玩家清单的门禁 ─────────────────────────
	if err := runPreflight(ctx, preflightDeps{
		sceneRdb:   sceneRdb,
		mappingRdb: mapRdb,
		sharedRdb:  sharedRdb,
		lag:        lagSrc,
	}, preflightParams{
		src:             src,
		dst:             dst,
		kafkaGroup:      o.kafkaGroup,
		topicGeneration: uint32(o.kafkaTopicGen),
		playerIDs:       playerIDs,
	}); err != nil {
		log.Fatalf("%v", err)
	}

	// ── F: 围栏 ─────────────────────────────────────────────
	fence, err := acquireMergeFence(ctx, mapRdb, src, dst, runID, manifestPath, o.timeout, o.dryRun)
	if err != nil {
		log.Fatalf("merge fence: %v", err)
	}
	defer fence.release(context.Background())
	// abortKeepingFence:写到一半的失败走这里,围栏故意不释放并给出续跑指引(见 fenceKeptAbortMessage)。
	// dry-run 从没写过围栏,只报原因。
	abortKeepingFence := func(cause string) {
		if o.dryRun {
			log.Fatal(cause)
		}
		log.Fatal(fenceKeptAbortMessage(cause, fencedOpMerge, src, dst, runID, o.mappingAddr, o.mappingDB))
	}

	// ── M: 清单(在第一次写之前落盘)────────────────────────
	m := existing
	if m == nil {
		m = newMergeManifest(runID, src, dst, operatorTag(), now)
	}
	m.PlayerIDs = playerIDs
	m.Tables = playerTables
	if !o.skipGuild || !o.skipRank {
		// 公会 id 与下面的聚宝斋同口径:只要 guild 步骤还没做完,每次都把「当前 zone_id=src」并进清单。
		// 步骤 3 只按清单逐条主键点更新(见 migrateGuildZone 的锁序说明),清单之外的源区公会原地不动、
		// 由改写后的复查拒绝继续;若这里仍只在「清单为空」时收集,T-1 dry-run 留下的清单或上次复查中止
		// 的清单就永远收不进后来的公会,步骤 3 会一直中止。步骤做完之后不再收集:那时清单里的 id 就是
		// 撤销的唯一依据。-skip-guild-mysql(只做榜单)时沿用原口径:清单为空才收集一次。
		if (!o.skipGuild && !m.stepDone(stepGuildMySQL)) || len(m.GuildIDs) == 0 {
			gids, gerr := collectGuildIDsInZone(ctx, db, o.guildSchema, src)
			if gerr != nil {
				log.Fatalf("list guilds in source zone: %v", gerr)
			}
			m.GuildIDs = sortedUint64(append(m.GuildIDs, gids...))
		}
		if len(m.RankMembers) == 0 {
			members, rerr := readZoneRankMembers(ctx, guildRdb, src)
			if rerr != nil {
				log.Fatalf("read source rank ZSET: %v", rerr)
			}
			m.RankMembers = members
		}
	}
	// 聚宝斋商品 id:只要 trade 步骤还没做完,每次都把「当前 market_zone=src」并进清单。
	// 与公会的「清单为空才收集」不同:步骤 3b 只改清单里的 id,而清单可能是 T-1 dry-run
	// 或上次中断(3b 复查拒绝)的运行留下的 —— 之后新落到源区的商品不并进来就永远搬不走,
	// 3b 的复查会一直拒绝。步骤做完之后不再收集:那时源区已是 0,清单里的 id 就是撤销的唯一依据。
	if !o.skipTrade && !m.stepDone(stepTradeMySQL) {
		lids, terr := collectTradeListingIDsInZone(ctx, db, o.tradeSchema, src)
		if terr != nil {
			log.Fatalf("list trade listings in source zone: %v", terr)
		}
		m.TradeListingIDs = sortedUint64(append(m.TradeListingIDs, lids...))
	}
	log.Printf("Manifest: %d players, %d guilds, %d rank members, tables=%v",
		len(m.PlayerIDs), len(m.GuildIDs), len(m.RankMembers), m.Tables)
	log.Printf("Manifest: %d trade listings (trade skip=%v)", len(m.TradeListingIDs), o.skipTrade)
	if err := saveManifest(manifestPath, m); err != nil {
		log.Fatalf("write manifest before first write: %v", err)
	}
	log.Printf("Manifest written to %s BEFORE any write. Keep it: -mode unmerge needs it.", manifestPath)

	persist := func(step, detail string) {
		m.markStep(step, detail)
		if err := saveManifest(manifestPath, m); err != nil {
			log.Fatalf("update manifest after %s: %v", step, err)
		}
	}

	var summary mergeSummary

	// ── 1: 玩家主数据行 ─────────────────────────────────────
	if o.skipRows {
		log.Printf("SKIPPING player row copy (-skip-player-rows -i-know-global-player-table). " +
			"This is only correct once player main data is global.")
	} else if m.stepDone(stepPlayerRows) {
		log.Printf("step %s already done per manifest — skipping", stepPlayerRows)
	} else {
		rep, rerr := copyPlayerRows(ctx, db, srcSchema, dstSchema, playerTables, playerIDs, o.dryRun)
		if rerr != nil {
			log.Fatalf("player rows: %v", rerr)
		}
		// 缓存失效紧跟拷贝:这些键不带 zone,是全服共享的一份,里面装着从
		// 源库读出来的内容。两个 zone 此刻都已下线,不会有人在这中间回填。
		n, cerr := invalidatePlayerCaches(ctx, sharedRdb, playerIDs, playerTables, o.dryRun)
		if cerr != nil {
			log.Fatalf("invalidate player caches: %v", cerr)
		}
		rep.CacheDeleted = n
		summary.rows = rep
		log.Printf("Player rows (%s): %s", verbWrite(o.dryRun), rep)
		if !o.dryRun {
			persist(stepPlayerRows, rep.String())
		}
	}

	// ── 2: data Redis blob ──────────────────────────────────
	if o.migrateBlobs && !o.skipBlobs && !m.stepDone(stepPlayerBlobs) {
		players, keys, berr := copyPlayerBlobs(ctx, o, playerIDs)
		if berr != nil {
			log.Fatalf("player blob copy: %v", berr)
		}
		summary.blobPlayers, summary.blobKeys = players, keys
		if !o.dryRun {
			persist(stepPlayerBlobs, fmt.Sprintf("players=%d keys=%d", players, keys))
		}
	}

	// ── 3: guild MySQL + 缓存失效 ───────────────────────────
	if !o.skipGuild && !m.stepDone(stepGuildMySQL) {
		if err := assertNoGuildNameCollision(ctx, db, o.guildSchema, src, dst); err != nil {
			log.Fatalf("guild: %v", err)
		}
		// 只改清单里的 id,逐条主键点更新(锁序理由见 migrateGuildZone)。锁冲突重试耗尽 / 其他写失败时
		// 已有一部分公会搬过去了,与 3b 复查中止同理**不提前释放围栏**,文案给出核对 run_id → DEL → 重跑。
		rows, gerr := migrateGuildZone(ctx, db, o.guildSchema, m.GuildIDs, src, dst, o.dryRun)
		if gerr != nil {
			abortKeepingFence(fmt.Sprintf("guild MySQL:改写 zone_id 中途失败,步骤 %s 未标记完成:%v",
				stepGuildMySQL, gerr))
		}
		summary.guildRows = rows
		// guild 是全局服务,不随 zone-down 重启;30 分钟的 guild:v2 缓存里
		// 存着旧 zone_id,不失效就等于合服后半小时公会还挂在死掉的区上。
		// 放在复查之前:复查中止时,已经搬过去的那些公会的缓存也已与库一致(失效幂等,重跑再做一遍无害)。
		// 失效失败时 zone_id 已经改写,与上面的写失败是同一种「写到一半」:围栏同样保留、给续跑指引。
		// 重跑时本步骤仍未完成,改写按 `zone_id = src` 幂等(已搬的影响 0 行),失效对全清单再做一遍。
		inv, ierr := invalidateGuildCaches(ctx, guildRdb, m.GuildIDs, o.dryRun)
		if ierr != nil {
			abortKeepingFence(fmt.Sprintf("guild MySQL:zone_id 已改写 %d 行,但 guild:v2 缓存失效失败,步骤 %s 未标记完成:%v",
				rows, stepGuildMySQL, ierr))
		}
		summary.guildCacheInvalidated = inv
		// 复查:源区还有公会 = 清单落盘之后又有公会进了源区(guild 服只有客户端建帮路径读合服围栏,
		// 内部 / GM 路径不读)。与 3b 同口径:中止且**不 persist**,重跑时清单阶段先把它们并进清单再搬;
		// 只打 WARN 继续是 fail-open —— 步骤一旦标记完成就不再收集,那几个公会会被留在已下线的 zone 里。
		if !o.dryRun {
			left, cerr := countGuildsInZone(ctx, db, o.guildSchema, src)
			if cerr != nil {
				abortKeepingFence(fmt.Sprintf("guild MySQL:改写后复查源区失败,步骤 %s 未标记完成:%v",
					stepGuildMySQL, cerr))
			}
			if left > 0 {
				abortKeepingFence(fmt.Sprintf("guild MySQL:清单内 %d 个公会已改写 %d 行,但仍有 %d 个公会 zone_id=%d —— "+
					"它们在清单落盘之后才进入源区(内部 / GM 建帮路径不读合服围栏)。清单之外的公会一个没动,"+
					"步骤 %s 未标记完成;先停掉这些建帮入口,重跑时清单阶段会先把它们并进清单再搬",
					len(m.GuildIDs), rows, left, src, stepGuildMySQL))
			}
		}
		log.Printf("MySQL: %d guild rows %s for zone %d → %d (%d guilds in manifest); %d guild:v2 cache entries invalidated",
			rows, verbWrite(o.dryRun), src, dst, len(m.GuildIDs), inv)
		if !o.dryRun {
			persist(stepGuildMySQL, fmt.Sprintf("rows=%d manifest_guilds=%d cache_invalidated=%d", rows, len(m.GuildIDs), inv))
		}
	}

	// ── 3b: 聚宝斋 trade_listing.market_zone ─────────────────
	// 放在 mapping 翻转(5)之前,与 guild 同理:所有 zone 列先改完再翻路由。
	if !o.skipTrade && !m.stepDone(stepTradeMySQL) {
		// 只改清单里的 id(清单先于写):清单之外的商品一个字节都不动。
		rows, terr := migrateTradeMarketZone(ctx, db, o.tradeSchema, m.TradeListingIDs, src, dst, o.dryRun)
		if terr != nil {
			// 与步骤 3 同理:走到这里玩家行 / 公会已经搬过,围栏不提前释放。
			abortKeepingFence(fmt.Sprintf("trade MySQL:改写 market_zone 中途失败,步骤 %s 未标记完成:%v",
				stepTradeMySQL, terr))
		}
		summary.tradeListingRows = rows
		// 复查:源区还有商品 = 清单落盘之后又有商品落到源区(P1 trade 不读合服围栏)。
		// 中止且**不 persist**:重跑时本步骤仍未完成,清单阶段会先把它们并进清单再搬。
		// 只打 WARN 继续是 fail-open —— 步骤一旦标记完成就不会再收集,那几条既搬不走、也撤不回。
		// log.Fatal 不跑 defer:围栏留在本次 run_id 上,且**故意不提前释放**(理由见
		// tradeResidualAbortMessage),文案里写明核对 run_id → DEL → 重跑。
		if !o.dryRun {
			left, cerr := countTradeListingsInZone(ctx, db, o.tradeSchema, src)
			if cerr != nil {
				abortKeepingFence(fmt.Sprintf("trade MySQL:改写后复查源区失败,步骤 %s 未标记完成:%v",
					stepTradeMySQL, cerr))
			}
			if left > 0 {
				log.Fatal(tradeResidualAbortMessage(rows, len(m.TradeListingIDs), left, src, dst,
					runID, o.mappingAddr, o.mappingDB))
			}
		}
		log.Printf("MySQL: %d trade_listing rows %s (market_zone %d → %d; seller_zone_at_listing untouched)",
			rows, verbWrite(o.dryRun), src, dst)
		if !o.dryRun {
			persist(stepTradeMySQL, fmt.Sprintf("rows=%d manifest_listings=%d", rows, len(m.TradeListingIDs)))
		}
	}

	// ── 4: guild_rank ZSET(互斥锁 + MULTI/EXEC)────────────
	if !o.skipRank && !m.stepDone(stepGuildRank) {
		release, lerr := acquireGuildRankLock(ctx, guildRdb, o.dryRun)
		if lerr != nil {
			log.Fatalf("guild rank lock: %v", lerr)
		}
		n, rerr := mergeRankZSET(ctx, guildRdb, src, dst, m.RankMembers, o.dryRun)
		release()
		if rerr != nil {
			log.Fatalf("merge ranking: %v", rerr)
		}
		summary.rankMembers = n
		log.Printf("Redis rank: %d ZSET members %s guild_rank:zone %d → %d", n, verbWrite(o.dryRun), src, dst)
		if !o.dryRun {
			persist(stepGuildRank, fmt.Sprintf("members=%d", n))
		}
	}

	// ── 5: mapping remap ────────────────────────────────────
	if !o.skipMapping && !m.stepDone(stepPlayerMapping) {
		matched, updated, merr := remapPlayerMapping(ctx, mapRdb, src, dst, o.dryRun)
		if merr != nil {
			log.Fatalf("player mapping: %v", merr)
		}
		summary.mapMatched, summary.mapUpdated = matched, updated
		log.Printf("Mapping Redis: %d players matched, %d home_zone values %s  (%s db=%d)",
			matched, updated, verbWrite(o.dryRun), o.mappingAddr, o.mappingDB)
		if !o.dryRun {
			persist(stepPlayerMapping, fmt.Sprintf("matched=%d updated=%d", matched, updated))
		}
	}

	// ── 6: scene_manager 热状态 ─────────────────────────────
	if o.clearHotState && !m.stepDone(stepHotState) {
		hot, herr := clearSourceZoneHotState(ctx, sceneRdb, src, playerIDs, o.dryRun)
		if herr != nil {
			log.Fatalf("clear source hot state: %v", herr)
		}
		summary.hotState = hot
		log.Printf("Scene hot state (%s): %s", verbWrite(o.dryRun), hot)
		if !o.dryRun {
			persist(stepHotState, hot.String())
		}
	}

	// ── 7: 合服公告 flag(login Redis, DB 0)────────────────
	if !m.stepDone(stepPostMerge) {
		notice, rename, serr := stampPostMergeFlags(ctx, sharedRdb, playerIDs, nil, stampNowUnixMs(), o.dryRun)
		if serr != nil {
			// 打标失败不回滚合服:合服本身已经成功,少几个人看不到公告是
			// 可恢复的(重跑 -apply 会补齐)。
			log.Printf("WARN: post-merge stamp had errors: %v", serr)
		}
		summary.noticeStamped, summary.renameStamped = notice, rename
		log.Printf("Post-merge flags: %d notice keys %s to %s db=%d, %d force-rename keys",
			notice, verbWrite(o.dryRun), o.noticeAddr, o.noticeDB, rename)
		if !o.dryRun && serr == nil {
			persist(stepPostMerge, fmt.Sprintf("notice=%d rename=%d", notice, rename))
		}
	}

	log.Printf("=== Done (players_in_source=%d %s) ===", len(playerIDs), summary.String())
	if o.dryRun {
		log.Printf("DRY-RUN: nothing was written. Record players_in_source=%d and pass it back as "+
			"-expected-src-players on the T-0 run and on -verify-merged.", len(playerIDs))
	} else {
		log.Printf("Manifest: %s — keep it for -verify-merged (-expected-src-players %d) and for -mode unmerge",
			manifestPath, len(playerIDs))
	}
}

// mergeSummary 汇总每步计数,只为最后那行日志。
type mergeSummary struct {
	rows                  playerRowsReport
	blobPlayers, blobKeys int
	guildRows             int64
	guildCacheInvalidated int
	tradeListingRows      int64
	rankMembers           int
	mapMatched            int
	mapUpdated            int
	hotState              hotStateClearReport
	noticeStamped         int
	renameStamped         int
}

func (s mergeSummary) String() string {
	return fmt.Sprintf("player_rows=[%s] blob_players=%d blob_keys=%d guild_rows=%d guild_cache=%d trade_listings=%d "+
		"rank_entries=%d map_matched=%d map_updated=%d hot_locations=%d notice=%d rename=%d",
		s.rows, s.blobPlayers, s.blobKeys, s.guildRows, s.guildCacheInvalidated, s.tradeListingRows,
		s.rankMembers, s.mapMatched, s.mapUpdated, s.hotState.LocationsDeleted, s.noticeStamped, s.renameStamped)
}

// ── 守卫(item 1)─────────────────────────────────────────────

// guardPlayerSet 拦住两类「跑了等于没跑」的合服:
//
//  1. 收集到 0 个玩家。绝大多数情况是 -mapping-redis-db 指错了库(默认此前
//     是 0,而真正的 mapping 在 15),或者存量玩家从没被 -backfill-home-zone
//     回填过。这时 -apply 会「成功」地什么都不做,运维看到 Done 就 zone-up,
//     源区玩家全部登不上而且没有任何报错。
//
//  2. 映射不全:zone_src_db.player_database 的行数 > 收集到的 id 数。差额
//     就是**有行、没映射**的玩家 —— 他们不会被搬,合服后指向一个已下线的
//     zone。先跑 -backfill-home-zone。
//
// 还顺手核对 -expected-src-players:T-1 彩排记的数与现在不一致 = 源区在彩排
// 之后还有人写过,zone-down 不彻底。
func guardPlayerSet(ctx context.Context, db *sql.DB, srcSchema string, ids []uint64, o options, resumed bool) error {
	if len(ids) == 0 && !o.allowEmptySource {
		return fmt.Errorf("collected 0 players with home_zone=%d from %s db=%d. "+
			"A merge with an empty mapping is a silent no-op. Most likely causes: "+
			"(a) -mapping-redis-db is wrong (data_service MappingRedis is always DB %d — "+
			"go-zero RedisConf has no DB field), or "+
			"(b) legacy players were never backfilled — run `-backfill-home-zone -zone %d` for BOTH zones first. "+
			"Pass -allow-empty-source only if you really intend to merge a zone with no players",
			o.sourceZone, o.mappingAddr, o.mappingDB, defaultMappingRedisDB, o.sourceZone)
	}

	// 重跑时 mapping 已经被改过,源库行数与清单里的 id 数天然一致,
	// 但「源库现在还有多少行」这条查询依然有效(行还在源库,没删)。
	var rows int64
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM "+srcSchema+".player_database").Scan(&rows); err != nil {
		return fmt.Errorf("count %s.player_database: %w", srcSchema, err)
	}
	if rows > int64(len(ids)) {
		return fmt.Errorf("%s.player_database has %d rows but only %d players map to home_zone=%d. "+
			"The %d players without a mapping would be left behind pointing at a dead zone. "+
			"Run `-backfill-home-zone -zone %d -apply` first, then re-run this merge",
			srcSchema, rows, len(ids), o.sourceZone, rows-int64(len(ids)), o.sourceZone)
	}
	log.Printf("guard OK: %s.player_database has %d rows <= %d mapped players", srcSchema, rows, len(ids))

	if o.expectedSrcPlayers >= 0 && !resumed && int64(len(ids)) != o.expectedSrcPlayers {
		return fmt.Errorf("expected %d source players (-expected-src-players, recorded at the T-1 rehearsal) "+
			"but found %d. The source zone changed since the rehearsal — zone-down is not complete. Abort the merge",
			o.expectedSrcPlayers, len(ids))
	}
	return nil
}

// ── mapping remap ────────────────────────────────────────────

// remapPlayerMapping 把 player:zone:{id} 从 src 改写成 dst。
//
// 每批 SCAN 一次 MGET 取值、命中的 key 一条 pipeline 批量 SET,而不是逐键
// GET + 逐键 SET:mapping 里是**全服**玩家的映射(不只源区),百万级映射下
// 旧写法是两百万次往返。
func remapPlayerMapping(ctx context.Context, rdb *redis.Client, src, dst uint32, dryRun bool) (matched, updated int, err error) {
	sVal := strconv.FormatUint(uint64(src), 10)
	dVal := strconv.FormatUint(uint64(dst), 10)
	var cur uint64
	for {
		var keys []string
		keys, cur, err = rdb.Scan(ctx, cur, playerZoneKeyPrefix+"*", mappingScanCount).Result()
		if err != nil {
			return matched, updated, fmt.Errorf("scan %s*: %w", playerZoneKeyPrefix, err)
		}
		if len(keys) > 0 {
			vals, gerr := rdb.MGet(ctx, keys...).Result()
			if gerr != nil {
				return matched, updated, fmt.Errorf("mapping mget (%d keys): %w", len(keys), gerr)
			}
			var hits []string
			for i, k := range keys {
				if s, ok := vals[i].(string); ok && s == sVal {
					hits = append(hits, k)
				}
			}
			matched += len(hits)
			if !dryRun && len(hits) > 0 {
				pipe := rdb.Pipeline()
				for _, k := range hits {
					pipe.Set(ctx, k, dVal, 0)
				}
				if _, perr := pipe.Exec(ctx); perr != nil {
					return matched, updated, fmt.Errorf("mapping set pipeline (%d keys): %w", len(hits), perr)
				}
				updated += len(hits)
			}
		}
		if cur == 0 {
			break
		}
	}
	return matched, updated, nil
}

// ── 句柄与小工具 ──────────────────────────────────────────────

// mustOpenMySQL 打开合服 / 撤销共用的 MySQL 句柄,会话隔离级别固定为 READ COMMITTED。
//
// 为什么显式设 RC(2026-09-21 死锁审计 #17):
//   - 此前直接用运维给的 DSN,未设隔离级别 = 实例默认 RR。RR 下 UPDATE 对扫描到的二级项加 next-key,
//     对不存在的主键加间隙锁,锁集会越出清单;在线服务(guild / trade / friend)的写事务都是 RC,
//     本工具与它们同口径,锁行为才能按同一套「主键 → 二级」推演。
//   - RC 下 INSERT ... SELECT 对源表做一致性读、不加 S 锁(RR 下对源行加共享 next-key)。合服时源区
//     已下线、没有写者,读到的内容与 RR 相同。
//
// 风险:binlog_format=STATEMENT 的实例在 RC 下拒绝 InnoDB 写。在线服务已经在同一实例上以 RC 写,
// 这不是新约束;真遇上时第一条写就会响亮失败,不会静默写一半。
// DSN 本身含密码,任何日志都不打印它(ParseDSN 的错误也不回显 DSN 原文)。
func mustOpenMySQL(ctx context.Context, dsn string) *sql.DB {
	rcDSN, err := readCommittedDSN(dsn)
	if err != nil {
		log.Fatalf("mysql dsn: %v", err)
	}
	db, err := sql.Open("mysql", rcDSN)
	if err != nil {
		log.Fatalf("mysql open: %v", err)
	}
	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("mysql ping: %v", err)
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

// readCommittedDSN 把 dsn 的会话隔离级别改成 READ COMMITTED,其余参数原样保留。
// 用 ParseDSN / FormatDSN 改结构而不是手拼字符串:参数的 URL 编码、密码里的特殊字符都交给驱动处理。
// 运维在 DSN 里显式写了别的隔离级别也会被覆盖(并打一行日志):本工具的锁序推演只对 RC 成立。
func readCommittedDSN(dsn string) (string, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return "", fmt.Errorf("parse -mysql-dsn: %w", err)
	}
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	if v, ok := cfg.Params[mysqlIsolationParam]; ok && v != mysqlReadCommitted {
		log.Printf("-mysql-dsn 里的 %s=%s 被改成 %s:合服的锁序推演只对 READ COMMITTED 成立",
			mysqlIsolationParam, v, mysqlReadCommitted)
	}
	if v, ok := cfg.Params[mysqlLegacyIsolationParam]; ok {
		log.Printf("-mysql-dsn 里的旧参数 %s=%s 已删除,改用 %s=%s", mysqlLegacyIsolationParam, v,
			mysqlIsolationParam, mysqlReadCommitted)
		delete(cfg.Params, mysqlLegacyIsolationParam)
	}
	cfg.Params[mysqlIsolationParam] = mysqlReadCommitted
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

// lockRetryPolicy 是单行点更新遇到锁冲突时的有界重试(次数上限 + 指数退避封顶 + 可取消)。
//
// 收敛论证:被重试的是**单行、自动提交**的 UPDATE。1213 时 InnoDB 回滚整个事务(即这一条语句),
// 1205 时回滚这条语句,自动提交下两者都什么也没留下;WHERE 里带着旧 zone,重跑幂等(已改过的行不再命中)。
// 点更新在拿到那一行的主键锁之前不持有任何锁,按推演不会成环;真出现 1213 / 1205 只能是对端长事务或
// 执行计划没走主键,这时有限次数后交给人,而不是无限重试把维护窗口耗光。
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
		log.Printf("WARN: %s 锁冲突(第 %d/%d 次):%v —— %s 后重试", what, attempt, attempts, err, wait)
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

// zonePointUpdateSQL 是「按主键逐行改 zone 列」唯一的 SQL 形状,参数依次为 (to, pk, from)。
// table 已是 schema.table(库名过了 schemaNamePattern),pkCol / zoneCol 是本工具的常量,不来自输入。
// 集成测试对同一个函数的产物做 EXPLAIN,断言走满主键 —— 测试里不另抄一份 SQL。
func zonePointUpdateSQL(table, pkCol, zoneCol string) string {
	return "UPDATE " + table + " SET " + zoneCol + " = ? WHERE " + pkCol + " = ? AND " + zoneCol + " = ?"
}

// rewriteZoneByPrimaryKey 把 ids 里当前 zoneCol = from 的行逐条改成 to,返回实际改动的行数。
// 不在 ids 里的行一个都不碰;已经不在 from 的(改过了 / 去了第三个 zone / 已被删除)影响 0 行。
//
// 锁序(2026-09-21 死锁审计 #17,与 docs/ops/incident-friend-lock-order-deadlock-2026-09-21.md 同一口径:
// 守卫之后的锁定写一律是完整主键的等值点更新):
//
//	旧写法 `UPDATE t SET zone = dst WHERE zone = src [AND pk IN (...)]` 可被规划成沿 zone 二级索引的
//	range 扫描:先对二级项加 X、再回表锁主键。在线写者(guild 服 DisbandGuild;P3 起 trade 的商品状态
//	迁移)是先 `WHERE pk = ? FOR UPDATE` 锁主键、再在 DELETE / UPDATE 里改同一行的二级项 —— 两边反序,
//	合服扫到那一行时 1213,且牺牲者可能是合服这条大语句。
//	现在逐条 `WHERE pk = ? AND zone = ?`:主键等值是 const 访问,必走主键;每条自动提交,同一时刻最多
//	持有一行的主键锁及其自身的二级项,拿到主键锁之前什么都不持有,取锁顺序与在线写者同为
//	「主键 → 二级」,只会排队、不会成环。等到的若是已删除的行,影响 0 行。
//
// ids 升序处理:每条独立提交,顺序本身不影响成环;升序只为日志与重跑可复现 —— 将来若改成「N 条一个
// 事务」,升序就是必要条件(且一个事务里只能改这一张表)。
// 成本:每行一次往返。语句只 Prepare 一次;不预编译时驱动每条都要 prepare / exec / close 三次往返。
// 失败:锁冲突按 zoneRewriteRetry 有界重试;其余错误或重试耗尽立即返回,已改的行留在 to。调用方的
// 步骤不标记完成,重跑按 `zone = from` 幂等补齐。
func rewriteZoneByPrimaryKey(ctx context.Context, db *sql.DB, table, pkCol, zoneCol string,
	ids []uint64, from, to uint32) (int64, error) {
	ids = sortedUint64(ids)
	if len(ids) == 0 {
		return 0, nil
	}
	stmt, err := db.PrepareContext(ctx, zonePointUpdateSQL(table, pkCol, zoneCol))
	if err != nil {
		return 0, fmt.Errorf("prepare %s.%s 点更新:%w", table, zoneCol, err)
	}
	defer stmt.Close()

	var total int64
	for _, id := range ids {
		what := fmt.Sprintf("%s %s=%d", table, pkCol, id)
		var n int64
		err := zoneRewriteRetry.do(ctx, what, func() error {
			res, err := stmt.ExecContext(ctx, to, id, from)
			if err != nil {
				return err
			}
			n, err = res.RowsAffected()
			return err
		})
		if err != nil {
			// 这一层就是完整上下文(表.列、方向、出错的主键、已改行数),调用方原样上抛、不再包一层。
			return total, fmt.Errorf("%s.%s %d → %d 在 %s=%d 处失败(此前已改 %d 行):%w",
				table, zoneCol, from, to, pkCol, id, total, err)
		}
		total += n
	}
	return total, nil
}

// fencedOp 标明中止发生在哪条持围栏的路径上。合服与撤销「为什么不释放围栏」「重跑会做什么」
// 不是同一回事(合服按清单续跑,撤销没有步骤进度、靠每一步幂等),中止文案按它分开写。
type fencedOp int

const (
	fencedOpMerge   fencedOp = iota // -mode merge:runMerge 的步骤 3 / 3b
	fencedOpUnmerge                 // -mode unmerge:runUnmerge 的 3b' / 3'
)

// fenceKeptAbortMessage 是「写到一半中止、合服围栏故意留着」的统一中止文案:合服步骤 3 / 3b 的写失败、
// 复查失败、步骤 3 改写后的缓存失效失败;撤销 3' / 3b' 的写失败与 3' 之后的缓存失效失败。
//
// 这些路径走 log.Fatal → os.Exit,defer 的 fence.release 不执行:merge:in_progress:{src,dst} 两把围栏
// 仍挂在**本次** run_id 上,直到 TTL(-timeout+30m,下限 1h)。**故意不在中止前释放**:
//   - 合服(与 tradeResidualAbortMessage 同理):数据已搬了一部分,续跑按清单走、不重扫玩家;放开围栏 =
//     放开这两个 zone 的建号与建帮,新号不在清单里,mapping 却会在步骤 5 被整体翻到 dst,它的行从没拷过。
//   - 撤销:mapping 已指回源区,MySQL / 公会缓存只改回了一部分,两个 zone 处于半撤销状态;撤销不记步骤
//     进度,重跑靠每一步按对象当前值过滤、幂等,围栏要保持到人工核对之后。
//
// 围栏要一直挂到运维排除故障、核对 run_id、手工 DEL、立刻重跑为止,把「无围栏」窗口压到人工可控的几秒。
// 文案必须写全:本次 run_id(供 GET 核对,不误删别人的围栏)、两把键名、mapping Redis 位置、
// 「不 DEL 直接重跑会被拒」—— 重跑会生成新 run_id,SETNX 撞上残留围栏。字面量由 lock_order_test.go 守住。
func fenceKeptAbortMessage(cause string, op fencedOp, src, dst uint32, runID, mappingAddr string, mappingDB int) string {
	// 未知 op 落到中性表述:文案只影响人读,不影响正确性,不值得在 log.Fatal 的路上再 panic。
	why := "两个 zone 的数据处于半改状态,围栏须保持到人工核对之后"
	fixHint := ""
	rerun := "每一步都幂等"
	switch op {
	case fencedOpMerge:
		why = "合服写到一半,数据已搬了一部分:提前释放会重新放开这两个 zone 的建号 / 建帮,而续跑按清单走、" +
			"不重扫玩家 —— 窗口里新建的号不在清单里,mapping 却会在步骤 5 被整体翻到目标区,它的行从没拷过"
		fixHint = ";源区残留对象:先停掉对应的写入口"
		rerun = "合服读清单续跑,已标记完成的步骤不重做"
	case fencedOpUnmerge:
		why = "撤销做到一半:mapping 已指回源区,MySQL / 公会缓存只改回了一部分,两个 zone 处于半撤销状态," +
			"围栏要保持到人工核对之后,不在此之前放开这两个 zone 的建号 / 建帮"
		rerun = "撤销不记步骤进度,每一步都按对象当前值过滤、幂等,可整体重跑"
	}
	srcKey, dstKey := mergeFenceKey(src), mergeFenceKey(dst)
	return fmt.Sprintf("%s。合服围栏 %s 与 %s 故意保留(LEFT IN PLACE),仍归 run_id=%s:%s。续跑步骤:"+
		"(1) 按上面的原因排除故障(锁冲突:查 SHOW ENGINE INNODB STATUS 与 information_schema.innodb_trx 里的长事务;"+
		"Redis 失败:先恢复对应 Redis 的连通%s);"+
		"(2) 确认没有存活的 merge_zone 进程;"+
		"(3) 在 mapping Redis(%s db=%d)上 GET %s,核对其 run_id 是 %s;"+
		"(4) DEL %s %s;"+
		"(5) 立即用原命令重跑(%s)。"+
		"不做 (4) 直接重跑会被围栏拒绝(\"another merge is already fencing zone\"),直到围栏 TTL 过期",
		cause, srcKey, dstKey, runID, why,
		fixHint,
		mappingAddr, mappingDB, srcKey, runID,
		srcKey, dstKey,
		rerun)
}

func mustDial(ctx context.Context, label, addr, pwd string, dbIndex int) *redis.Client {
	c := redis.NewClient(&redis.Options{Addr: addr, Password: pwd, DB: dbIndex})
	if err := c.Ping(ctx).Err(); err != nil {
		log.Fatalf("%s redis ping (%s db=%d): %v", label, addr, dbIndex, err)
	}
	return c
}

// buildLagSource 选一个 Kafka 积压来源。两个都没配 = 返回 nil,preflight 拒绝。
func buildLagSource(o options) kafkaLagSource {
	if o.kafkaCLI != "" {
		return kafkaCLILagSource{cmdPath: o.kafkaCLI, bootstrap: o.kafkaBootstrap}
	}
	if o.assumeKafkaDrained {
		log.Printf("⚠️  -assume-kafka-drained: NO Kafka lag was measured. " +
			"This attestation goes into the merge record; if a db_task backlog existed, " +
			"those saves are still in Kafka and will never reach the target zone.")
		return attestedLagSource{}
	}
	return nil
}

// copyPlayerBlobs 是 data Redis 的 player:{id}:* 拷贝步骤。
//
// 与旧实现的差别:拷完之后核对「拷到的玩家数 == len(ids)」,不一致直接
// **中止**而不是打个 WARN。少拷一个玩家 = 那个玩家在目标区的 data_service
// 里什么都没有,而 mapping 马上就要指过去。
func copyPlayerBlobs(ctx context.Context, o options, ids []uint64) (players, keys int, err error) {
	sAddrs := addrsFromCSV(o.sourceData)
	tAddrs := addrsFromCSV(o.targetData)
	if sameDataBackend(sAddrs, tAddrs, o.sourceDataDB, o.targetDataDB) {
		log.Println("Data Redis: source and target endpoints identical — skipping player blob copy (single backend)")
		return 0, 0, nil
	}
	srcData := newDataRedisClient(sAddrs, o.dataPwd, o.sourceDataDB)
	dstData := newDataRedisClient(tAddrs, o.dataPwd, o.targetDataDB)
	defer srcData.Close()
	defer dstData.Close()
	if err := srcData.Ping(ctx).Err(); err != nil {
		return 0, 0, fmt.Errorf("source data redis ping: %w", err)
	}
	if err := dstData.Ping(ctx).Err(); err != nil {
		return 0, 0, fmt.Errorf("target data redis ping: %w", err)
	}

	var withoutKeys []uint64
	for _, pid := range ids {
		n, cerr := copyPlayerStringKeys(ctx, srcData, dstData, pid, o.dryRun)
		if cerr != nil {
			return players, keys, fmt.Errorf("player %d blob copy: %w", pid, cerr)
		}
		if n > 0 {
			keys += n
			players++
		} else {
			withoutKeys = append(withoutKeys, pid)
		}
	}
	if players != len(ids) {
		// 「没有键」有一种合法解释:玩家从没上线过,data_service 里确实空。
		// 但它与「拷贝漏了」在结果上不可区分,而后者是数据丢失。停下来让人看。
		sample := withoutKeys
		if len(sample) > 20 {
			sample = sample[:20]
		}
		return players, keys, fmt.Errorf(
			"blob copy covered %d of %d players; %d had no player:{id}:* key in the source data Redis "+
				"(first ids: %v). Either those players never had data_service state, or the scan missed a "+
				"cluster node. Verify before continuing — the mapping remap is about to point them at the target",
			players, len(ids), len(withoutKeys), sample)
	}
	verb := "copied"
	if o.dryRun {
		verb = "would copy"
	}
	log.Printf("Player blobs: %s keys for %d players (total string keys: %d)", verb, players, keys)
	return players, keys, nil
}
