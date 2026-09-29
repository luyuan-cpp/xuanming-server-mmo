package main

// 合服主流程。步骤顺序与理由见 main.go 顶部注释。
//
// 四条贯穿全流程的规矩:
//  1. **先证明,再写**。所有拒绝(preflight / guard)必须在第一次写之前跑完。
//  2. **先立围栏,再收集**。收集玩家 / 公会 / 商品与各项预检都在围栏之下做:围栏之前收集,
//     收集与立围栏之间新建的号既不在清单里,又没被挡住。
//  3. **清单先于写**。清单落盘之后才允许动第一个字节,否则出事时没人知道动过谁。
//     失败的处置以清单落盘为界:之后的失败走 abortKeepingFence(围栏保留,按文案续跑);之前的拒绝走 refuse:
//     首跑时本次什么都没写,mergeFence.refuseAndRelease 正常释放围栏、非零退出;续跑(清单已存在且步骤 7
//     未标记完成)时上一次运行可能已写到一半,同样走 abortKeepingFence 保留围栏(fenceKeptAbortMessage),
//     不在半合服状态下重新放开建号 / 建帮 / 进场。
//  4. **id 只收集一次**。remap 之后源区玩家在 mapping 里已经消失,任何
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
	log.Printf("    player rows: mode=%s capability_zones=%s", o.playerRowsMode, o.capSpec)
	if o.dryRun {
		log.Printf("    dry-run: the manifest preview goes to %s — a dry-run never writes -manifest-path itself",
			dryRunManifestPath(manifestPath))
	}
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

	// ── R: 既有清单 ─────────────────────────────────────────
	// 先尝试从既有清单读:重跑时 mapping 已经被改过,重新扫描会得到空集合。
	// dry-run 预览(<path>.dryrun.json / dry_run=true)一律拒读(A3):那是彩排那一刻的名单。
	existing, err := loadRunManifest(manifestPath)
	if err != nil {
		log.Fatalf("load manifest: %v", err)
	}
	if err := validateManifestForRun(existing, src, dst); err != nil {
		log.Fatalf("manifest mismatch: %v", err)
	}
	resumed := existing != nil && len(existing.PlayerIDs) > 0

	// ── C: 能力标记(pin 模式,§10.1 前置)─────────────────────────
	// pin 合服之后,清单玩家的 home 是 dst、落点记录是 src:处理他们存盘的 dst go/db 必须按记录选库。
	// 旧版 go/db 按本进程 zone 选库,会把他们的存盘写进 zone_dst_db —— 那里没有他们的行,与真源分叉。
	// copy 模式只在清单玩家里有记录者时才要求(见下面 S 段)。
	// -db-capability-zones 没有缺省值(pin 模式在 main 里已校验必填):列出的 zone 逐个查,src / dst 列进来照常查
	// (T-0 它们已下线,必然被拒,文案会说明);none = 运维声明此刻没有别的 zone 在跑,不查 —— dst 的能力改在它
	// zone-up 之后、开服之前用 -mode capability-check 核对(runbook §8 Step 6)。
	checkPinCapability := func() error {
		if o.capSpec.none {
			log.Printf("preflight C: -db-capability-zones none — no capability marker checked (operator attests that no other "+
				"zone's go/db is running). Zone %d must pass -mode capability-check -db-capability-zones %d after its zone-up, "+
				"before it opens", dst, dst)
			return nil
		}
		if err := checkCapabilityZoneSpec(ctx, mapRdb, o.capSpec, src, dst); err != nil {
			return fmt.Errorf("preflight C (-player-rows-mode pin): %v. Upgrade and restart go/db in those zones first (it writes "+
				"the marker on start-up and refreshes it every 30s), or merge with -player-rows-mode copy", err)
		}
		log.Printf("preflight C OK: %s confirmed in zones %v", capabilityRoutingV1, o.capSpec.zones)
		return nil
	}
	// 首跑:只读、先于围栏,拒绝时什么都没动,也没有需要保护的半截状态。
	// 续跑的三项清单校验(模式 / 表集合 / C)挪到围栏之后(见 F 段下方):运维此时已照中止文案 DEL 了上一次的
	// 围栏,两个 zone 处于半合服状态;围栏之前 log.Fatalf 等于让它们在没有围栏的情况下一直敞着。
	if existing == nil && o.playerRowsMode == playerRowsModePin {
		if err := checkPinCapability(); err != nil {
			log.Fatal(err)
		}
	}

	// ── F: 围栏(先于收集与预检,A2)──────────────────────────
	// 收集与预检都得在围栏之下做:之前的顺序是「收集 → 预检 → 立围栏」,收集与立围栏之间 RegisterPlayerZone
	// 进来的新号既不在清单里、又没被挡住,改映射时被漏在源区。
	fence, err := acquireMergeFence(ctx, mapRdb, src, dst, runID, manifestPath, o.timeout, o.dryRun)
	if err != nil {
		log.Fatalf("merge fence: %v", err)
	}
	defer fence.release(context.Background())
	// abortKeepingFence:清单落盘之后的失败走这里,围栏故意不释放并给出续跑指引(见 fenceKeptAbortMessage)。
	// dry-run 从没写过围栏,只报原因。
	abortKeepingFence := func(cause string) {
		if o.dryRun {
			log.Fatal(cause)
		}
		log.Fatal(fenceKeptAbortMessage(cause, fencedOpMerge, src, dst, runID, o.mappingAddr, o.mappingDB))
	}
	// refuse:从这里到清单落盘之间的统一拒绝出口。
	//   - 首跑(existing == nil):本次还一个字节都没写,fence.refuseAndRelease 释放围栏、非零退出。
	//   - 续跑(existing != nil:真清单只在 M 处写出,dry-run 预览已被 loadRunManifest 拒掉):上一次运行可能已经
	//     过了 M、写到一半(钉了落点、拷了几张表、改了 guild.zone_id / market_zone、合了榜单、改了一部分映射)。
	//     此时释放围栏会在半合服状态下重新放开建号 / 建帮 / 进场(A16):新号不在清单里,步骤 5 只按清单改映射,
	//     它会被留在已下线的源区。所以同样走 abortKeepingFence,围栏留在本次 run_id 上。
	//     判据用「清单存在」而不是某一步的完成标记:清单落盘之后、步骤 1 标完成之前也可能已提交了几张表。
	//   - 例外:清单已标记步骤 7 完成 = 上一次已跑完全程,此时 dst 可能已经开服,保留围栏只会挡住 dst,照常释放。
	halfDone := existing != nil && !existing.stepDone(stepPostMerge)
	refuse := func(format string, args ...any) {
		if halfDone {
			abortKeepingFence("续跑在清单更新之前被拒(上一次 -apply 已落清单,可能已写到一半,两个 zone 处于半合服状态," +
				"本次不释放围栏):" + fmt.Sprintf(format, args...))
		}
		fence.refuseAndRelease(format, args...)
	}

	// ── R2: 续跑的清单校验(围栏之下,理由见上面 C 段)────────────
	if existing != nil {
		// 续跑必须与清单同一个玩家行模式(player-storage-placement.md §10.1 第 7 条)。
		if err := validateManifestRowsMode(existing, o.playerRowsMode); err != nil {
			refuse("manifest %s: %v", manifestPath, err)
		}
		// 续跑时玩家表集合必须与清单一致(A5):清单的表是步骤 1 拷贝与撤销删行的唯一依据。
		// pin 模式不拷行、清单不记表,不比。
		if o.playerRowsMode == playerRowsModeCopy {
			if err := checkResumeTables(existing.Tables, playerTables); err != nil {
				refuse("manifest %s: %v", manifestPath, err)
			}
		}
		if o.playerRowsMode == playerRowsModePin {
			if err := checkPinCapability(); err != nil {
				refuse("%v", err)
			}
		}
	}

	// ── 0: 收集玩家 id(只此一次)────────────────────────────
	var playerIDs []uint64
	if resumed {
		playerIDs = existing.PlayerIDs
		log.Printf("RESUME: manifest %s lists %d players / %d guilds / %d rank members; "+
			"NOT re-scanning the mapping (after a remap the scan would return an empty set)",
			manifestPath, len(playerIDs), len(existing.GuildIDs), len(existing.RankMembers))
	} else {
		playerIDs, err = collectPlayerIDsWithHomeZone(ctx, mapRdb, src)
		if err != nil {
			refuse("list players in source zone: %v", err)
		}
		log.Printf("Mapping: %d players with home_zone=%d (%s db=%d)", len(playerIDs), src, o.mappingAddr, o.mappingDB)
	}

	// ── G: 守卫 ─────────────────────────────────────────────
	if err := guardPlayerSet(ctx, newMySQLPlayerIDSource(db, src), redisMappingPresence(mapRdb), playerIDs, o, resumed); err != nil {
		refuse("%v", err)
	}

	// ── P2~P7: 需要玩家清单的门禁(源区口径)────────────────
	if err := runPreflight(ctx, preflightDeps{
		sceneRdb:   sceneRdb,
		mappingRdb: mapRdb,
		sharedRdb:  sharedRdb,
		lag:        lagSrc,
	}, preflightParams{
		zone:            src,
		scope:           "source",
		kafkaGroup:      o.kafkaGroup,
		topicGeneration: uint32(o.kafkaTopicGen),
		playerIDs:       playerIDs,
	}); err != nil {
		refuse("%v", err)
	}

	// ── N: 公会重名断言(围栏之后、清单之前,A4)────────────────
	// 此前它在步骤 3 里、步骤 1 / 2 已经写过之后才跑;断言的意义是「冲突时一个字节都不写」,
	// 所以挪到这里。又必须在围栏之后:围栏之前建的帮会在它跑完之后才进源区,断言就白做了。
	if !o.skipGuild && !existing.stepDone(stepGuildMySQL) {
		if err := assertNoGuildNameCollision(ctx, db, o.guildSchema, src, dst); err != nil {
			refuse("guild: %v", err)
		}
	}

	// ── S: 清单玩家的落点扫描(围栏之后、清单之前,player-storage-placement.md §10.1 / §10.2)──
	// 冻结记录 = 搬库正在挪这个人:合服要改他的 home(连带存盘 topic),两件事不能叠在一起(§9 末条),拒绝;
	// 畸形记录同样拒绝(go/db 不按它选库,这里也判断不了他的行在哪)。其余已有记录记进清单:copy 模式这些人
	// 不拷行,pin 模式这些人不钉。围栏已立:之后 RegisterPlayerZone、搬库 R1、pin-placement 都碰不到这批人。
	// 「玩家行这一步」与改映射都做完之后不再扫:home 已经改了,互斥与分流都没有意义了。
	rowsStep := rowsStepFor(o.playerRowsMode)
	var scanned []manifestPlacement
	needScan := !existing.stepDone(rowsStep) || !existing.stepDone(stepPlayerMapping)
	if needScan {
		reads, rerr := readPlacements(ctx, mapRdb, playerIDs)
		if rerr != nil {
			refuse("read the manifest players' placement records: %v", rerr)
		}
		scanned, err = classifyMergePlacements(playerIDs, reads, o.playerRowsMode, src)
		if err != nil {
			refuse("placement: %v", err)
		}
		// 续跑:与首跑记下的逐人比对。围栏之下没有别的写者能动这批人的落点,不一致只能是人工改过。
		if existing != nil && existing.PlacementScanned {
			if n, sample := diffPlacementExisting(existing.PlacementExisting, scanned); n > 0 {
				refuse("placement: %d manifest players' placement records differ from what this merge recorded "+
					"before its first write (first: %v). Find out who changed them before resuming", n, sample)
			}
		}
		// copy 模式里有记录的人不拷行,他们的有效落点留在记录指向的库;合服之后处理他们存盘的是 dst 的 go/db,
		// 它必须按记录选库 —— 否则把他们的存盘写进 zone_dst_db,与真源分叉。没有记录的人不牵涉这一点。
		// 口径与 pin 模式的 C 相同:-db-capability-zones 必填(接受 none),列出的 zone 逐个查。
		if o.playerRowsMode == playerRowsModeCopy && len(scanned) > 0 {
			if err := requireCapabilityZones(o.capSpec, capabilityForMerge); err != nil {
				refuse("placement: %d manifest players already have a placement record and keep their rows "+
					"in that store, so -player-rows-mode copy needs placement-aware go/db as well: %v", len(scanned), err)
			}
			if o.capSpec.none {
				log.Printf("placement: -db-capability-zones none — no capability marker checked for the %d record holders "+
					"(operator attests that no other zone's go/db is running). Zone %d must pass -mode capability-check after "+
					"its zone-up, before it opens", len(scanned), dst)
			} else if err := checkCapabilityZoneSpec(ctx, mapRdb, o.capSpec, src, dst); err != nil {
				refuse("placement: %d manifest players already have a placement record and keep their rows "+
					"in that store, so -player-rows-mode copy needs placement-aware go/db as well: %v", len(scanned), err)
			}
		}
		log.Printf("placement scan OK: no frozen / malformed record among %d manifest players; %d already have a record "+
			"(mode=%s)", len(playerIDs), len(scanned), o.playerRowsMode)
	}

	// ── X: 合走标记(围栏之后、清单之前,A10)────────────────────
	// 步骤 5 的 markMergedInto 在「源区已被合进别的 zone」时拒绝,但那时步骤 1~4 都已提交(先证明,再写 被破坏):
	// 公会 / 商品 / 榜单已改到本次的 dst,映射一个没改,只能人工回退。这里在第一次写之前做同一判定。
	// 目标区自己已被合走(merge:merged_into:{dst} 存在)同样拒绝:那等于把玩家归属改到一个已下线的 zone。
	// 读不懂 / 读失败一律拒绝(fail-closed)。markMergedInto 里的检查保留,作为最后一道防线。
	// 步骤 5 已完成(或跳过改映射)时不再查:那时标记是本次合服自己写的。
	if !o.skipMapping && !existing.stepDone(stepPlayerMapping) {
		if err := checkMergedIntoBeforeMerge(ctx, mapRdb, src, dst); err != nil {
			refuse("%v", err)
		}
	}

	// ── M: 清单(在第一次写之前落盘)────────────────────────
	m := existing
	if m == nil {
		m = newMergeManifest(runID, src, dst, operatorTag(), now)
	}
	m.PlayerIDs = playerIDs
	// 模式写进清单(旧清单没有这个字段 = copy,与上面的校验同一口径)。
	m.PlayerRowsMode = o.playerRowsMode
	if needScan && !m.PlacementScanned {
		m.PlacementExisting = scanned
		m.PlacementScanned = true
	}
	// 表清单只在步骤 1 还没完成时写(A5):完成之后它就是撤销删行的依据,不能被本次发现覆盖。
	// 续跑时表集合已在上面校验过一致。pin 模式不拷行,不记表(撤销据此不删任何行)。
	if o.playerRowsMode == playerRowsModeCopy && !m.stepDone(stepPlayerRows) {
		m.Tables = playerTables
	}
	if !o.skipGuild || !o.skipRank {
		// 公会 id 与下面的聚宝斋同口径:只要 guild 步骤还没做完,每次都把「当前 zone_id=src」并进清单。
		// 步骤 3 只按清单逐条主键点更新(见 migrateGuildZone 的锁序说明),清单之外的源区公会原地不动、
		// 由改写后的复查拒绝继续;若这里仍只在「清单为空」时收集,上次复查中止的清单就永远收不进后来的
		// 公会,步骤 3 会一直中止。步骤做完之后不再收集:那时清单里的 id 就是撤销的唯一依据。
		// -skip-guild-mysql(只做榜单)时沿用原口径:清单为空才收集一次。
		if (!o.skipGuild && !m.stepDone(stepGuildMySQL)) || len(m.GuildIDs) == 0 {
			gids, gerr := collectGuildIDsInZone(ctx, db, o.guildSchema, src)
			if gerr != nil {
				refuse("list guilds in source zone: %v", gerr)
			}
			m.GuildIDs = sortedUint64(append(m.GuildIDs, gids...))
		}
		// 榜单快照只在清单里还没有、且步骤 4 还没做完时取一次。步骤 4 以维护锁内重读的源榜为准(A8),
		// 快照从不被写进目标榜(源榜已不在时不写,见 chooseRankWrite),只在「上一次写入之前落盘的那一份」
		// 时充当撤销依据 —— 所以这里不能每次重读覆盖它;步骤 4 做完之后它就是撤销依据,更不能再碰。
		// RankMembersUnwritten 标明「这份只是清单阶段读的,还没交给过任何一次写」。
		if len(m.RankMembers) == 0 && !m.stepDone(stepGuildRank) {
			members, rerr := readZoneRankMembers(ctx, guildRdb, src)
			if rerr != nil {
				refuse("read source rank ZSET: %v", rerr)
			}
			m.RankMembers = members
			m.RankMembersUnwritten = true
		}
	}
	// 聚宝斋商品 id:只要 trade 步骤还没做完,每次都把「当前 market_zone=src」并进清单。
	// 与公会的「清单为空才收集」不同:步骤 3b 只改清单里的 id,而清单可能是上次中断(3b 复查拒绝)
	// 的运行留下的 —— 之后新落到源区的商品不并进来就永远搬不走,3b 的复查会一直拒绝。
	// 步骤做完之后不再收集:那时源区已是 0,清单里的 id 就是撤销的唯一依据。
	if !o.skipTrade && !m.stepDone(stepTradeMySQL) {
		lids, terr := collectTradeListingIDsInZone(ctx, db, o.tradeSchema, src)
		if terr != nil {
			refuse("list trade listings in source zone: %v", terr)
		}
		m.TradeListingIDs = sortedUint64(append(m.TradeListingIDs, lids...))
	}
	log.Printf("Manifest: %d players, %d guilds, %d rank members, tables=%v",
		len(m.PlayerIDs), len(m.GuildIDs), len(m.RankMembers), m.Tables)
	log.Printf("Manifest: %d trade listings (trade skip=%v)", len(m.TradeListingIDs), o.skipTrade)
	if o.dryRun {
		// A3:预览写到 <path>.dryrun.json 并标 dry_run,-manifest-path 一个字节不碰。dry-run 之后
		// 不会再 persist,这里改 m 的标记不影响任何真清单。
		m.DryRun = true
		preview := dryRunManifestPath(manifestPath)
		if err := saveManifest(preview, m); err != nil {
			refuse("write dry-run manifest preview %s: %v", preview, err)
		}
		log.Printf("DRY-RUN: manifest preview written to %s (for humans only — -apply, -mode unmerge and "+
			"-verify-merged refuse to read it; %s was not touched)", preview, manifestPath)
	} else {
		if err := saveManifest(manifestPath, m); err != nil {
			refuse("write manifest before first write: %v", err)
		}
		log.Printf("Manifest written to %s BEFORE any write. Keep it: -mode unmerge and -verify-merged need it.", manifestPath)
	}

	// 清单已落盘:此后的失败一律保留围栏(abortKeepingFence)。
	persist := func(step, detail string) {
		m.markStep(step, detail)
		if err := saveManifest(manifestPath, m); err != nil {
			abortKeepingFence(fmt.Sprintf("步骤 %s 已执行,但清单落盘失败(步骤未记为完成,重跑会按幂等再做一遍):%v", step, err))
		}
	}

	var summary mergeSummary

	// ── 1: 玩家主数据行 ─────────────────────────────────────
	switch {
	case o.playerRowsMode == playerRowsModePin:
		// pin 模式(§10.1 第 2、3 条):不拷行、不拷 blob、不删玩家缓存。改映射(步骤 5)之前把清单里没有落点记录的
		// 人钉到 "{src}:1" —— 这正是他们此刻的有效落点(无记录 → home = src),钉住之后改 home 不再牵动数据落在哪。
		// 中途失败时已钉的人有效落点不变(记录 == 旧 home),围栏保留,续跑幂等(已钉的按 already_pinned 计)。
		if m.stepDone(stepPinPlacement) {
			log.Printf("step %s already done per manifest — skipping", stepPinPlacement)
			break
		}
		rep, perr := pinManifestPlacements(ctx, mapRdb, playerIDs, m.placementExistingMap(), src, o.dryRun)
		if perr != nil {
			abortKeepingFence(fmt.Sprintf("pin placement:步骤 %s 未标记完成(%s):%v", stepPinPlacement, rep, perr))
		}
		summary.pin = rep
		log.Printf("Placement pin (%s): %s — player rows stay in %s, nothing is copied", verbWrite(o.dryRun), rep, srcSchema)
		if !o.dryRun {
			persist(stepPinPlacement, rep.String())
		}
	case o.skipRows:
		log.Printf("SKIPPING player row copy (-skip-player-rows -i-know-global-player-table). " +
			"This is only correct once player main data is global.")
	case m.stepDone(stepPlayerRows):
		log.Printf("step %s already done per manifest — skipping", stepPlayerRows)
	default:
		// copy 模式只拷没有落点记录的人(§10.2):有记录的人的行在其落点库,拷过去也只是一份没人读的冷副本,
		// 他们的共享缓存也不用失效(路由与内容都没变)。
		copyIDs := uint64sNotIn(playerIDs, placementIDs(m.PlacementExisting))
		if skipped := len(playerIDs) - len(copyIDs); skipped > 0 {
			log.Printf("Player rows: %d manifest players already have a placement record — their rows stay in their placement "+
				"store and are not copied", skipped)
		}
		// 续跑时认得出上次已经拷完的表(逐列相同),首跑时目标库有任何一行都拒绝(A4)。
		policy := refuseExistingTargetRows
		if resumed {
			policy = acceptIdenticalTargetRows
		}
		// 表用清单里的(m.Tables):清单是撤销删行的依据,拷贝与撤销必须是同一组表。
		rep, rerr := copyPlayerRows(ctx, db, srcSchema, dstSchema, m.Tables, copyIDs, policy, o.dryRun)
		if rerr != nil {
			// 步骤 1 可能已提交了几张表(一表一事务),围栏保留,续跑按 acceptIdenticalTargetRows 认回来。
			abortKeepingFence(fmt.Sprintf("player rows:步骤 %s 未标记完成:%v", stepPlayerRows, rerr))
		}
		// 缓存失效紧跟拷贝:这些键不带 zone,是全服共享的一份,里面装着从
		// 源库读出来的内容。两个 zone 此刻都已下线,不会有人在这中间回填。
		n, cerr := invalidatePlayerCaches(ctx, sharedRdb, copyIDs, m.Tables, o.dryRun)
		if cerr != nil {
			abortKeepingFence(fmt.Sprintf("player rows:行已拷完,但共享缓存失效失败,步骤 %s 未标记完成:%v", stepPlayerRows, cerr))
		}
		rep.CacheDeleted = n
		summary.rows = rep
		summary.placedSkipped = len(playerIDs) - len(copyIDs)
		log.Printf("Player rows (%s): %s", verbWrite(o.dryRun), rep)
		if !o.dryRun {
			persist(stepPlayerRows, fmt.Sprintf("%s placed_skipped=%d", rep, summary.placedSkipped))
		}
	}

	// ── 2: data Redis blob ──────────────────────────────────
	if o.migrateBlobs && !o.skipBlobs && !m.stepDone(stepPlayerBlobs) {
		players, keys, berr := copyPlayerBlobs(ctx, o, playerIDs)
		if berr != nil {
			abortKeepingFence(fmt.Sprintf("player blob copy:步骤 %s 未标记完成:%v", stepPlayerBlobs, berr))
		}
		summary.blobPlayers, summary.blobKeys = players, keys
		if !o.dryRun {
			persist(stepPlayerBlobs, fmt.Sprintf("players=%d keys=%d", players, keys))
		}
	}

	// ── 3: guild MySQL + 缓存失效 ───────────────────────────
	if !o.skipGuild && !m.stepDone(stepGuildMySQL) {
		// 重名断言已挪到清单落盘之前(上面 N 段,A4):它的意义是「冲突时一个字节都不写」。
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

	// ── 4: guild_rank ZSET(维护锁内重读 + MULTI/EXEC,A8)──────
	if !o.skipRank && !m.stepDone(stepGuildRank) {
		members, sourceGone, rerr := mergeGuildRank(ctx, guildRdb, src, dst, m.RankMembers, !m.RankMembersUnwritten, o.dryRun,
			func(toRecord []manifestRankMember) error {
				// 清单先于写:把撤销依据先落盘(步骤不标完成),撤销按它原样还原。
				m.RankMembers = toRecord
				m.RankMembersUnwritten = false
				return saveManifest(manifestPath, m)
			})
		if rerr != nil {
			abortKeepingFence(fmt.Sprintf("guild rank:步骤 %s 未标记完成:%v", stepGuildRank, rerr))
		}
		summary.rankMembers = len(members)
		if sourceGone {
			// 源榜已不在(上一次的 MULTI/EXEC 已合走,或 guild 服合法清空):目标榜不动,只清理源键空壳。
			log.Printf("Redis rank: guild_rank:zone:%d is already gone — the target rank is left as the guild service keeps it, "+
				"only the empty source key is cleaned up; %d members kept in the manifest as the unmerge basis", src, len(members))
		} else {
			log.Printf("Redis rank: %d ZSET members %s guild_rank:zone %d → %d (re-read under the maintenance lock; ZADD NX)",
				len(members), verbWrite(o.dryRun), src, dst)
		}
		if !o.dryRun {
			persist(stepGuildRank, fmt.Sprintf("members=%d source_gone=%v", len(members), sourceGone))
		}
	}

	// ── 5: mapping remap(先写合走标记,再按清单逐键 CAS,A10 / A2)────
	if !o.skipMapping && !m.stepDone(stepPlayerMapping) {
		// 标记必须先于改映射:改到一半中止时它已经在,对源区的 -backfill-home-zone 从这一刻起就被挡住。
		if err := markMergedInto(ctx, mapRdb, src, dst, m.RunID, o.dryRun); err != nil {
			abortKeepingFence(fmt.Sprintf("player mapping:写 %s 失败,映射一个没改,步骤 %s 未标记完成:%v",
				mergedIntoKey(src), stepPlayerMapping, err))
		}
		rep, merr := remapPlayerMapping(ctx, mapRdb, m.PlayerIDs, src, dst, o.dryRun)
		if merr != nil {
			abortKeepingFence(fmt.Sprintf("player mapping:按清单改写中途失败(本次已改 %d 个),步骤 %s 未标记完成:%v",
				rep.Changed, stepPlayerMapping, merr))
		}
		if err := rep.complete(len(m.PlayerIDs)); err != nil {
			abortKeepingFence(fmt.Sprintf("player mapping:%v。步骤 %s 未标记完成;先查清这些键为什么不是源区 / 目标区"+
				"(被人改过?mapping Redis 丢过数据?),修好后重跑 —— 已改成目标区的会按「已是 dst」计入", err, stepPlayerMapping))
		}
		// 全量扫描只为告警(A2):清单之外仍指向源区的玩家不会被改,合服后留在已下线的 zone 里。
		// 清单是改映射的唯一依据 —— 这里不替人决定要不要搬他们,-verify-merged 的 verify:mapping_src 会拦住开服。
		outside, oerr := playersOutsideManifestWithHomeZone(ctx, mapRdb, src, m.PlayerIDs)
		switch {
		case oerr != nil:
			log.Printf("WARN: could not scan for players outside the manifest that still map to zone %d: %v", src, oerr)
		case len(outside) > 0:
			log.Printf("WARN: %d players NOT in the manifest still map to home_zone=%d (first ids: %v) — they were not "+
				"remapped and will be stranded in the merged-away zone. They reached the source zone outside this run's "+
				"fence; decide per player before opening (verify:mapping_src blocks until they are handled)",
				len(outside), src, sampleUint64(outside))
		}
		summary.mapChanged, summary.mapAlready, summary.mapOutside = rep.Changed, rep.Already, len(outside)
		log.Printf("Mapping Redis: %d manifest players %s to home_zone=%d, %d already there; %d outside the manifest still at %d  (%s db=%d)",
			rep.Changed, verbWrite(o.dryRun), dst, rep.Already, len(outside), src, o.mappingAddr, o.mappingDB)
		if !o.dryRun {
			persist(stepPlayerMapping, fmt.Sprintf("changed=%d already=%d outside=%d", rep.Changed, rep.Already, len(outside)))
		}
	}

	// ── 6: scene_manager 热状态 ─────────────────────────────
	if o.clearHotState && !m.stepDone(stepHotState) {
		hot, herr := clearSourceZoneHotState(ctx, sceneRdb, src, playerIDs, o.dryRun)
		if herr != nil {
			abortKeepingFence(fmt.Sprintf("clear source hot state:步骤 %s 未标记完成(删除幂等,重跑补齐):%v", stepHotState, herr))
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
		log.Printf("DRY-RUN: nothing was written to MySQL or Redis (only the preview %s). Record players_in_source=%d and pass it "+
			"back as -expected-src-players on the T-0 run.", dryRunManifestPath(manifestPath), len(playerIDs))
	} else {
		log.Printf("Manifest: %s — keep it: -verify-merged -manifest-path %s (-expected-src-players %d) and -mode unmerge need it",
			manifestPath, manifestPath, len(playerIDs))
	}
}

// mergeSummary 汇总每步计数,只为最后那行日志。
type mergeSummary struct {
	rows                  playerRowsReport
	pin                   pinReport // pin 模式的钉落点计数
	placedSkipped         int       // copy 模式里因已有落点记录而没拷行的人数
	blobPlayers, blobKeys int
	guildRows             int64
	guildCacheInvalidated int
	tradeListingRows      int64
	rankMembers           int
	mapChanged            int // 本次按清单 src → dst 改写的映射数
	mapAlready            int // 清单里已经是 dst 的映射数(续跑)
	mapOutside            int // 清单之外仍指向 src 的映射数(只告警)
	hotState              hotStateClearReport
	noticeStamped         int
	renameStamped         int
}

func (s mergeSummary) String() string {
	return fmt.Sprintf("player_rows=[%s] placed_skipped=%d pin=[%s] blob_players=%d blob_keys=%d guild_rows=%d guild_cache=%d "+
		"trade_listings=%d rank_entries=%d map_changed=%d map_already=%d map_outside=%d hot_locations=%d notice=%d rename=%d",
		s.rows, s.placedSkipped, s.pin, s.blobPlayers, s.blobKeys, s.guildRows, s.guildCacheInvalidated, s.tradeListingRows,
		s.rankMembers, s.mapChanged, s.mapAlready, s.mapOutside, s.hotState.LocationsDeleted, s.noticeStamped, s.renameStamped)
}

// ── 守卫(item 1)─────────────────────────────────────────────

// guardPlayerSet 拦住两类「跑了等于没跑」的合服:
//
//  1. 收集到 0 个玩家。绝大多数情况是 -mapping-redis-db 指错了库(默认此前
//     是 0,而真正的 mapping 在 15),或者存量玩家从没被 -backfill-home-zone
//     回填过。这时 -apply 会「成功」地什么都不做,运维看到 Done 就 zone-up,
//     源区玩家全部登不上而且没有任何报错。
//
//  2. 源库有行、却**没有任何** player:zone 映射的玩家(集合比较,2026-09-28,player-storage-placement.md
//     §12 A1)。他们不会被搬,合服后指向一个已下线的 zone。先跑 -backfill-home-zone。
//     旧写法只比「源库行数 <= 映射到源区的 id 数」,会被「有映射、没有行」的玩家抵消(例:映射指向源区、
//     首次存盘还没落库的新号):两边各差一个,行数对上了,没映射的那个照样被漏掉。现在逐行查:
//     rows 里的每个 player_id 都必须有映射 —— 指向哪个区不管,指向别区的是早先合出去的玩家留在源库的
//     冷副本,不归这次合服管。续跑时清单玩家已改到 dst、同样算「有映射」,口径不变。
//
// 还顺手核对 -expected-src-players:T-1 彩排记的数与现在不一致 = 源区在彩排
// 之后还有人写过,zone-down 不彻底。
func guardPlayerSet(ctx context.Context, rows playerIDSource, mapped mappingPresence, ids []uint64, o options, resumed bool) error {
	if len(ids) == 0 && !o.allowEmptySource {
		return fmt.Errorf("collected 0 players with home_zone=%d from %s db=%d. "+
			"A merge with an empty mapping is a silent no-op. Most likely causes: "+
			"(a) -mapping-redis-db is wrong (data_service MappingRedis is always DB %d — "+
			"go-zero RedisConf has no DB field), or "+
			"(b) legacy players were never backfilled — run `-backfill-home-zone -zone %d` for BOTH zones first. "+
			"Pass -allow-empty-source only if you really intend to merge a zone with no players",
			o.sourceZone, o.mappingAddr, o.mappingDB, defaultMappingRedisDB, o.sourceZone)
	}

	srcTable := zoneDBName(o.sourceZone) + ".player_database"
	scan, err := findUnmappedPlayers(ctx, rows, mapped)
	if err != nil {
		return fmt.Errorf("guard: compare %s with the player:zone mapping: %w", srcTable, err)
	}
	if scan.Unmapped > 0 {
		return fmt.Errorf("%s has %d rows and %d of them have NO player:zone mapping at all (first ids: %v). "+
			"Those players would be left behind pointing at a dead zone. "+
			"Run `-backfill-home-zone -zone %d -apply` first, then re-run this merge",
			srcTable, scan.Rows, scan.Unmapped, scan.Sample, o.sourceZone)
	}
	log.Printf("guard OK: all %d rows of %s have a player:zone mapping (%d map to home_zone=%d)",
		scan.Rows, srcTable, len(ids), o.sourceZone)

	if o.expectedSrcPlayers >= 0 && !resumed && int64(len(ids)) != o.expectedSrcPlayers {
		return fmt.Errorf("expected %d source players (-expected-src-players, recorded at the T-1 rehearsal) "+
			"but found %d. The source zone changed since the rehearsal — zone-down is not complete. Abort the merge",
			o.expectedSrcPlayers, len(ids))
	}
	return nil
}

// mappingPresence 报告 ids 各自的 player:zone:{id} 键是否存在,结果与 ids 等长、同序。
// 守卫只关心「有没有归属」,不关心指向哪里(见 guardPlayerSet 第 2 条)。生产实现是 redisMappingPresence,
// 单测注入内存替身。
type mappingPresence func(ctx context.Context, ids []uint64) ([]bool, error)

func redisMappingPresence(rdb *redis.Client) mappingPresence {
	return func(ctx context.Context, ids []uint64) ([]bool, error) {
		_, present, err := readPlayerZones(ctx, rdb, ids)
		return present, err
	}
}

// unmappedSampleSize 是拒绝 / 告警文案里列出的 id 个数上限。
const unmappedSampleSize = 20

// unmappedPlayerScan 是 A1 集合比较的结果。
type unmappedPlayerScan struct {
	Rows     int      // 源库 player_database 的行数
	Unmapped int      // 其中没有任何 player:zone 映射的行数
	Sample   []uint64 // 前 unmappedSampleSize 个没有映射的 id,进拒绝文案
}

// findUnmappedPlayers 按主键 keyset 分页读完源库的 player_id,逐批查映射是否存在。
// 分页口径与 -backfill-home-zone 相同(backfillBatchSize),两者看到的是同一个集合。
func findUnmappedPlayers(ctx context.Context, rows playerIDSource, mapped mappingPresence) (unmappedPlayerScan, error) {
	var scan unmappedPlayerScan
	var after uint64
	for {
		ids, err := rows.NextBatch(ctx, after, backfillBatchSize)
		if err != nil {
			return scan, err
		}
		if len(ids) == 0 {
			return scan, nil
		}
		scan.Rows += len(ids)
		after = ids[len(ids)-1]
		present, err := mapped(ctx, ids)
		if err != nil {
			return scan, err
		}
		if len(present) != len(ids) {
			return scan, fmt.Errorf("mapping lookup returned %d answers for %d ids", len(present), len(ids))
		}
		for i, ok := range present {
			if ok {
				continue
			}
			scan.Unmapped++
			if len(scan.Sample) < unmappedSampleSize {
				scan.Sample = append(scan.Sample, ids[i])
			}
		}
	}
}

// readPlayerZones 分批 MGET player:zone:{id},返回与 ids 等长、同序的值;键不存在的位置 present=false、值为空串。
func readPlayerZones(ctx context.Context, rdb *redis.Client, ids []uint64) ([]string, []bool, error) {
	vals := make([]string, 0, len(ids))
	present := make([]bool, 0, len(ids))
	for _, batch := range chunkUint64(ids, mappingScanCount) {
		keys := make([]string, len(batch))
		for i, id := range batch {
			keys[i] = playerZoneKey(id)
		}
		got, err := rdb.MGet(ctx, keys...).Result()
		if err != nil {
			return nil, nil, fmt.Errorf("mapping mget (%d keys): %w", len(keys), err)
		}
		for _, v := range got {
			s, ok := v.(string)
			vals = append(vals, s)
			present = append(present, ok)
		}
	}
	return vals, present, nil
}

// sampleUint64 取前 unmappedSampleSize 个,供文案列举。
func sampleUint64(ids []uint64) []uint64 {
	if len(ids) > unmappedSampleSize {
		return ids[:unmappedSampleSize]
	}
	return ids
}

// ── mapping remap ────────────────────────────────────────────

// mappingOutcome 是清单里一个玩家的 player:zone 在步骤 5 的结局。取值与 remapMappingScript 的返回码一致,
// dry-run 用 classifyMappingValue 按同一口径判。
type mappingOutcome int64

const (
	mappingMissing mappingOutcome = 0 // 键不存在:映射丢了
	mappingChanged mappingOutcome = 1 // 本次 src → dst(dry-run:将会改)
	mappingAlready mappingOutcome = 2 // 已经是 dst(续跑)
	mappingForeign mappingOutcome = 3 // 指向第三个 zone:不是这次合服能动的
)

// remapMappingScript 是步骤 5 的逐键 CAS:值 == src 才改成 dst,判断与写入在同一段 Lua 里原子完成。
// 旧写法 MGET 之后再 SET,两步之间被别人改过的值(例如运维同时用 data_service 的 RemapHomeZoneForMerge
// 改到了第三个 zone)会被这边无条件覆盖回 dst。
// KEYS[1]=player:zone:{id}  ARGV[1]=src  ARGV[2]=dst。返回 mappingOutcome。
var remapMappingScript = redis.NewScript(`
local cur = redis.call("GET", KEYS[1])
if cur == false then return 0 end
if cur == ARGV[1] then
  redis.call("SET", KEYS[1], ARGV[2])
  return 1
end
if cur == ARGV[2] then return 2 end
return 3
`)

// classifyMappingValue 是 remapMappingScript 判定部分的 Go 镜像(dry-run 只读,不能跑 CAS)。
func classifyMappingValue(cur string, present bool, sVal, dVal string) mappingOutcome {
	switch {
	case !present:
		return mappingMissing
	case cur == sVal:
		return mappingChanged
	case cur == dVal:
		return mappingAlready
	default:
		return mappingForeign
	}
}

// mappingRemapReport 是步骤 5 的计数。
type mappingRemapReport struct {
	Changed int // 本次 src → dst(dry-run:将会改)
	Already int // 已经是 dst
	Missing int // 键不存在
	Foreign int // 指向第三个 zone
	// Anomalies:Missing / Foreign 的前 unmappedSampleSize 个 id,进中止文案。
	Anomalies []uint64
}

func (r *mappingRemapReport) add(id uint64, o mappingOutcome) {
	switch o {
	case mappingChanged:
		r.Changed++
		return
	case mappingAlready:
		r.Already++
		return
	case mappingMissing:
		r.Missing++
	default:
		r.Foreign++
	}
	if len(r.Anomalies) < unmappedSampleSize {
		r.Anomalies = append(r.Anomalies, id)
	}
}

// complete 校验「已是 dst + 本次改成功 == 清单人数」(A2)。不等 = 清单里有人的映射丢了或指向第三个 zone,
// 他们没有被合过来;步骤不能标记完成。
func (r mappingRemapReport) complete(manifestPlayers int) error {
	if r.Changed+r.Already == manifestPlayers {
		return nil
	}
	return fmt.Errorf("only %d of the %d manifest players now map to the target zone (changed=%d already=%d); "+
		"%d have no player:zone key and %d map to a third zone (first ids: %v)",
		r.Changed+r.Already, manifestPlayers, r.Changed, r.Already, r.Missing, r.Foreign, r.Anomalies)
}

// remapPlayerMapping 把清单 ids 的 player:zone:{id} 从 src 改写成 dst(步骤 5,A2)。
//
// 只动清单里的键,逐键 Lua CAS(值 == src 才改),批量走 pipeline:
//   - 旧写法全量 SCAN、见到值 == src 就改:清单之外的人(立围栏之前漏进来的、上次中止后放开围栏时建的)
//     也被翻到 dst,而他们的行从没拷过 —— 合服后登录读到空号。
//   - 旧写法先 MGET 再 SET,两者之间的值变化会被覆盖;CAS 把判断与写放进同一段 Lua。
//
// 返回计数;调用方用 complete 校验人数。出错时返回已累计的部分。dry-run 只读,按同一口径分类。
func remapPlayerMapping(ctx context.Context, rdb *redis.Client, ids []uint64, src, dst uint32, dryRun bool) (mappingRemapReport, error) {
	var rep mappingRemapReport
	if len(ids) == 0 {
		return rep, nil
	}
	sVal := strconv.FormatUint(uint64(src), 10)
	dVal := strconv.FormatUint(uint64(dst), 10)
	if dryRun {
		vals, present, err := readPlayerZones(ctx, rdb, ids)
		if err != nil {
			return rep, err
		}
		for i, id := range ids {
			rep.add(id, classifyMappingValue(vals[i], present[i], sVal, dVal))
		}
		return rep, nil
	}
	// pipeline 里的 EVALSHA 遇到 NOSCRIPT 无法回退(整批已经发出),先 SCRIPT LOAD 一次(同 invalidateGuildCaches)。
	if err := remapMappingScript.Load(ctx, rdb).Err(); err != nil {
		return rep, fmt.Errorf("load mapping CAS script: %w", err)
	}
	for _, batch := range chunkUint64(ids, mappingScanCount) {
		pipe := rdb.Pipeline()
		cmds := make([]*redis.Cmd, len(batch))
		for i, id := range batch {
			cmds[i] = remapMappingScript.Run(ctx, pipe, []string{playerZoneKey(id)}, sVal, dVal)
		}
		if _, err := pipe.Exec(ctx); err != nil {
			// 这一批里哪些已经生效不确定(Exec 只报第一个错);CAS 幂等,重跑时已改的按「已是 dst」计。
			return rep, fmt.Errorf("mapping CAS pipeline (%d keys): %w", len(batch), err)
		}
		for i, cmd := range cmds {
			code, err := cmd.Int64()
			if err != nil {
				return rep, fmt.Errorf("mapping CAS %s: %w", playerZoneKey(batch[i]), err)
			}
			rep.add(batch[i], mappingOutcome(code))
		}
	}
	return rep, nil
}

// playersOutsideManifestWithHomeZone 全量扫描 mapping,返回仍指向 zone、但不在 manifest 里的玩家(升序)。
// 步骤 5 用它告警(A2):改映射只按清单,清单外的人留在原地。
func playersOutsideManifestWithHomeZone(ctx context.Context, rdb *redis.Client, zone uint32, manifest []uint64) ([]uint64, error) {
	all, err := collectPlayerIDsWithHomeZone(ctx, rdb, zone)
	if err != nil {
		return nil, err
	}
	return uint64sNotIn(all, manifest), nil
}

// uint64sNotIn 返回 a 里有、b 里没有的元素(保持 a 的顺序)。
func uint64sNotIn(a, b []uint64) []uint64 {
	have := make(map[uint64]struct{}, len(b))
	for _, v := range b {
		have[v] = struct{}{}
	}
	var out []uint64
	for _, v := range a {
		if _, ok := have[v]; !ok {
			out = append(out, v)
		}
	}
	return out
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
func rewriteZoneByPrimaryKey(ctx context.Context, db *sql.DB, table, pkCol, zoneCol string,
	ids []uint64, from, to uint32) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
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
	return rewriteZoneRows(ctx, sqlZoneRowTxBeginner(db, lockStmt, updateStmt), zoneRewriteRetry,
		zoneRewriteTarget{table: table, pkCol: pkCol, zoneCol: zoneCol}, ids, from, to)
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
	tx     *sql.Tx
	lock   *sql.Stmt // tx 内的 zoneLockSelectSQL
	update *sql.Stmt // tx 内的 zonePointUpdateSQL
}

func (t sqlZoneRowTx) lockZone(ctx context.Context, id uint64) (uint32, bool, error) {
	var zone uint32
	err := t.lock.QueryRowContext(ctx, id).Scan(&zone)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return zone, true, nil
}

func (t sqlZoneRowTx) rewriteZone(ctx context.Context, id uint64, from, to uint32) (int64, error) {
	res, err := t.update.ExecContext(ctx, to, id, from)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (t sqlZoneRowTx) commit() error   { return t.tx.Commit() }
func (t sqlZoneRowTx) rollback() error { return t.tx.Rollback() }

// sqlZoneRowTxBeginner 返回生产用的 beginZoneRowTx。lock / update 是在 db 上预编译好的语句,
// tx.StmtContext 把它们绑到本事务的连接上(同一连接上只 prepare 一次)。
func sqlZoneRowTxBeginner(db *sql.DB, lock, update *sql.Stmt) beginZoneRowTx {
	return func(ctx context.Context) (zoneRowTx, error) {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		return sqlZoneRowTx{tx: tx, lock: tx.StmtContext(ctx, lock), update: tx.StmtContext(ctx, update)}, nil
	}
}

// fencedOp 标明中止发生在哪条持围栏的路径上。合服与撤销「为什么不释放围栏」「重跑会做什么」
// 不是同一回事(合服按清单续跑,撤销没有步骤进度、靠每一步幂等),中止文案按它分开写。
type fencedOp int

const (
	fencedOpMerge   fencedOp = iota // -mode merge:runMerge 的步骤 3 / 3b
	fencedOpUnmerge                 // -mode unmerge:runUnmerge 的 3b' / 3'
)

// fenceKeptAbortMessage 是「写到一半中止、合服围栏故意留着」的统一中止文案:合服清单落盘之后任何一步的
// 失败(步骤 1~6 的写失败、复查失败、缓存失效失败、清单更新失败、改映射人数对不上);撤销 3' / 3b' 的
// 写失败与 5' 之后的标记 / 缓存失效失败;以及续跑(清单已存在)或半撤销状态下、第一次写之前的拒绝 —— 那时
// 上一次运行已写到一半(见 runMerge / runUnmerge 的 refuse)。首跑在清单落盘之前的拒绝不走这里,走 mergeFence.refuseAndRelease。
//
// 这些路径走 log.Fatal → os.Exit,defer 的 fence.release 不执行:merge:in_progress:{src,dst} 两把围栏
// 仍挂在**本次** run_id 上,直到 TTL(-timeout+30m,下限 1h)。**故意不在中止前释放**:
//   - 合服(与 tradeResidualAbortMessage 同理):数据已搬了一部分,续跑按清单走、不重扫玩家;放开围栏 =
//     放开这两个 zone 的建号与建帮,新号不在清单里,步骤 5 只按清单改映射,它会被留在已下线的源区。
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
			"不重扫玩家 —— 窗口里新建的号不在清单里,步骤 5 只按清单改映射,它会被留在已下线的源区"
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
