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
	"fmt"
	"log"
	"strconv"
	"time"

	_ "github.com/go-sql-driver/mysql"
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
	log.Printf("    redis: mapping=%s/db%d guild=%s/db%d login=%s/db%d friend=%s/db%d scene=%s/db%d",
		o.mappingAddr, o.mappingDB, o.redisAddr, o.redisDB, o.noticeAddr, o.noticeDB,
		o.friendAddr, o.friendDB, o.sceneAddr, o.sceneDB)

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

	// ── M: 清单(在第一次写之前落盘)────────────────────────
	m := existing
	if m == nil {
		m = newMergeManifest(runID, src, dst, operatorTag(), now)
	}
	m.PlayerIDs = playerIDs
	m.Tables = playerTables
	if !o.skipGuild || !o.skipRank {
		if len(m.GuildIDs) == 0 {
			gids, gerr := collectGuildIDsInZone(ctx, db, src)
			if gerr != nil {
				log.Fatalf("list guilds in source zone: %v", gerr)
			}
			m.GuildIDs = gids
		}
		if len(m.RankMembers) == 0 {
			members, rerr := readZoneRankMembers(ctx, guildRdb, src)
			if rerr != nil {
				log.Fatalf("read source rank ZSET: %v", rerr)
			}
			m.RankMembers = members
		}
	}
	log.Printf("Manifest: %d players, %d guilds, %d rank members, tables=%v",
		len(m.PlayerIDs), len(m.GuildIDs), len(m.RankMembers), m.Tables)
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
		if err := assertNoGuildNameCollision(ctx, db, src, dst); err != nil {
			log.Fatalf("guild: %v", err)
		}
		rows, gerr := migrateGuildZone(ctx, db, src, dst, o.dryRun)
		if gerr != nil {
			log.Fatalf("guild MySQL: %v", gerr)
		}
		summary.guildRows = rows
		// guild 是全局服务,不随 zone-down 重启;30 分钟的 guild:v2 缓存里
		// 存着旧 zone_id,不失效就等于合服后半小时公会还挂在死掉的区上。
		inv, ierr := invalidateGuildCaches(ctx, guildRdb, m.GuildIDs, o.dryRun)
		if ierr != nil {
			log.Fatalf("invalidate guild caches: %v", ierr)
		}
		summary.guildCacheInvalidated = inv
		log.Printf("MySQL: %d guild rows %s for zone %d → %d; %d guild:v2 cache entries invalidated",
			rows, verbWrite(o.dryRun), src, dst, inv)
		if !o.dryRun {
			persist(stepGuildMySQL, fmt.Sprintf("rows=%d cache_invalidated=%d", rows, inv))
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
	rankMembers           int
	mapMatched            int
	mapUpdated            int
	hotState              hotStateClearReport
	noticeStamped         int
	renameStamped         int
}

func (s mergeSummary) String() string {
	return fmt.Sprintf("player_rows=[%s] blob_players=%d blob_keys=%d guild_rows=%d guild_cache=%d "+
		"rank_entries=%d map_matched=%d map_updated=%d hot_locations=%d notice=%d rename=%d",
		s.rows, s.blobPlayers, s.blobKeys, s.guildRows, s.guildCacheInvalidated,
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

func mustOpenMySQL(ctx context.Context, dsn string) *sql.DB {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Fatalf("mysql open: %v", err)
	}
	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("mysql ping: %v", err)
	}
	return db
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
