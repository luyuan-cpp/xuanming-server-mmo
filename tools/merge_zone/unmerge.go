package main

// -mode unmerge:按清单**逐对象**撤销一次合服。
//
// 这不是「从备份还原」的替代品,而是它的补充。备份还原会把目标区在合服窗口
// 之后发生的一切一起抹掉;unmerge 只碰清单里列出的那些玩家 / 公会 / ZSET 成员,
// 目标区原住民一根汗毛不碰。适用面:合服刚跑完、还没开服,发现搞错了对象。
//
// 撤销顺序与合服**完全相反** —— 先把路由改回去,再往回搬数据。这样任何一步
// 失败时,玩家的归属都指向一个**确实有他数据**的 zone:
//
//	P  前置门禁(目标区口径,A7):目标区无活节点、目标 topic 与重试队列排空、清单玩家无锁无会话无活队伍
//	S  清单玩家不得有冻结的落点记录(撤销同样改 home,与搬库互斥,player-storage-placement.md §9 末条);
//	   有「落点记录 ≠ src」的人时要求 src 的能力标记;-db-capability-zones 列出的 zone 逐个核对(见下)
//	7' 清掉 player_merge_notice:{pid}(合服没发生过,不该弹公告)
//	5' player:zone:{id} 改回 src            ← 先做,路由回源区
//	5'a 删 merge:merged_into:{src}(A10:源区重新有人了,回填不该再被挡)
//	5'b 共享玩家缓存失效(A7,copy 模式):路由回到源区,从目标库读出的那份缓存不能再用
//	4' guild_rank ZSET 成员 ZADD 回 src、从 dst ZREM
//	3b' trade_listing.market_zone 改回 src(只改清单里、当前确实在 dst 的 listing_id)
//	3' guild.zone_id 改回 src(只改清单里的 guild_id)+ 缓存失效
//	1' 删目标库里那些**与源库逐字节相同**的玩家行(copy 模式;有落点记录的玩家不删)
//
// 玩家行模式按清单(§10.1 第 6、7 条):pin 模式只改回 home,**不动落点记录**(钉的是 "{src}:1",与改回后的
// home 一致),也没有拷过行、没有要失效的缓存 —— 5'b 与 1' 都不做。copy 模式照旧,但 1' 跳过此刻有落点记录的
// 玩家:他们的有效落点由记录决定、不随 home 改回,目标库里的那一行可能正是他的真源(例如合服之后被
// pin-placement 钉在了目标区),「与源库逐字节相同」证明不了它是可以删的拷贝。
//
// -db-capability-zones(没有缺省值,§4.3):pin 模式的撤销必填(接受 none),在围栏之前就拒绝;copy 模式的撤销
// 给了才查。列出的 zone 在 S 段逐个核对,src / dst 列进来照常查 —— 撤销时两者都已 zone-down,必然被拒,文案会说明;
// 撤销完成、两区 zone-up 之后用 -mode capability-check 核对。与上面「落点记录 ≠ src 时要求 src 标记」是两条独立检查。
//
// 前置门禁为什么是目标区(2026-09-28,player-storage-placement.md §12 A7):合服之后这批玩家的存盘走目标
// topic、落目标库。目标区还有节点在跑、目标 topic 还有没落库的存盘、玩家还在线时撤销 = 路由改回源区、
// 删掉目标库的行,那些存盘要么落进一个已没人读的库,要么随 1' 一起消失。
//
// 门禁(P / S)失败时的围栏处置:首次撤销时本次什么都没写,正常释放围栏、非零退出(mergeFence.refuseAndRelease);
// 若清单玩家里已有人被改回源区(上一次撤销在 5' 之后中途中止、运维 DEL 围栏后重跑 —— 半撤销状态),门禁 / S 段
// 失败改走 abortKeepingFence 保留围栏,不在半撤销状态下放开两个 zone 的建号 / 建帮(见 manifestPlayersBackAt)。
//
// 1' 的红线:只删逐字节相同的行。任何一行在合服后被改过(玩家登录过、
// 打过一场、领过邮件)就**拒绝删除并报出来** —— 那时删掉的不是「拷贝」,
// 而是合服后产生的唯一一份数据。源库的行本来就没删过,所以不删目标库的
// 那份只是留下一份多余副本,而 mapping 已经指回源区,不会被读到。
//
// blob 拷贝(player:{id}:*)同理不自动删:它们在目标 data Redis 上,
// mapping 指回源区之后没人会读,留着比删错安全。报告里会提示。

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// checkUnmergeCapabilityFlag 是撤销对 -db-capability-zones 的前置要求(纯函数):清单是 pin 模式时必填(接受 none);
// copy 模式不要求。
func checkUnmergeCapabilityFlag(rowsMode string, spec capabilityZoneSpec) error {
	if rowsMode != playerRowsModePin {
		return nil
	}
	return requireCapabilityZones(spec, capabilityForUnmerge)
}

func runUnmerge(o options) {
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()

	// dry-run 预览(<path>.dryrun.json)一律拒读(A3):它记的是彩排那一刻的名单,不是任何一次写入。
	m, err := loadRunManifest(o.manifestPath)
	if err != nil {
		log.Fatalf("load manifest: %v", err)
	}
	if m == nil {
		log.Fatalf("manifest %s does not exist. -mode unmerge can only reverse a run that wrote a manifest", o.manifestPath)
	}
	src, dst := m.SourceZone, m.TargetZone
	if o.sourceZone != 0 && o.targetZone != 0 {
		if err := validateManifestForRun(m, o.sourceZone, o.targetZone); err != nil {
			log.Fatalf("manifest mismatch: %v", err)
		}
	}
	// 模式只看清单(-player-rows-mode 对撤销不起作用):撤销的对象是当初那次合服,按它当时的做法反着来。
	rowsMode := m.rowsMode()
	if _, err := parsePlayerRowsMode(rowsMode); err != nil {
		log.Fatalf("manifest %s records an unknown player_rows_mode %q — refuse to guess", o.manifestPath, m.PlayerRowsMode)
	}
	// pin 合服留下的 "{src}:1" 记录在撤销之后仍在:与合服同一口径,-db-capability-zones 必填(接受 none)。
	// 围栏之前、任何写之前拒绝。copy 撤销不要求,给了才在 S 段核对。
	if err := checkUnmergeCapabilityFlag(rowsMode, o.capSpec); err != nil {
		log.Fatalf("%v", err)
	}

	log.Printf("=== UNMERGE zone %d → %d (reversing run_id=%s started %s; player rows mode=%s; dry-run=%v) ===",
		src, dst, m.RunID, m.StartedAt, rowsMode, o.dryRun)
	log.Printf("    scope: %d players, %d guilds, %d rank members, tables=%v",
		len(m.PlayerIDs), len(m.GuildIDs), len(m.RankMembers), m.Tables)
	log.Printf("    scope: %d trade listings (schema=%s; restored only where market_zone is still %d)",
		len(m.TradeListingIDs), o.tradeSchema, dst)
	log.Printf("    scope: guild schema=%s (restored only where zone_id is still %d)", o.guildSchema, dst)
	log.Printf("    NOTHING outside this list is touched — target-zone natives are safe by construction.")

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

	// 聚宝斋前置:清单里有商品 id = 当初真的搬过,库 / 表必须在。放在任何写之前 ——
	// -trade-schema 指错时,不能等 5' mapping / 4' ZSET 已经改回之后才在 3b' 失败(先证明,再写)。
	if len(m.TradeListingIDs) > 0 {
		if err := assertTradeListingReady(ctx, db, o.tradeSchema); err != nil {
			log.Fatalf("preflight: manifest lists %d trade listings to restore, but %v", len(m.TradeListingIDs), err)
		}
	}

	// 帮会前置:同理,清单里有公会 id = 当初真的搬过,独占库与两张表必须在(先证明,再写)。
	if len(m.GuildIDs) > 0 {
		if err := assertGuildTablesReady(ctx, db, o.guildSchema); err != nil {
			log.Fatalf("preflight: manifest lists %d guilds to restore, but %v", len(m.GuildIDs), err)
		}
	}

	lagSrc := buildLagSource(o)

	// 撤销期间同样上围栏:别人不许在这两个 zone 里建新对象。
	runID := newRunID(src, dst, time.Now())
	fence, err := acquireMergeFence(ctx, mapRdb, src, dst, runID, o.manifestPath, o.timeout, o.dryRun)
	if err != nil {
		log.Fatalf("merge fence: %v", err)
	}
	defer fence.release(context.Background())
	// abortKeepingFence:MySQL / 公会缓存改回到一半的失败走这里,围栏故意不释放并给出续跑指引
	// (撤销口径的文案,见 fenceKeptAbortMessage)。
	// dry-run 从没写过围栏,只报原因。
	abortKeepingFence := func(cause string) {
		if o.dryRun {
			log.Fatal(cause)
		}
		log.Fatal(fenceKeptAbortMessage(cause, fencedOpUnmerge, src, dst, runID, o.mappingAddr, o.mappingDB))
	}

	// 半撤销判定(只读):撤销不记步骤进度,「这次还没写」不等于「上一次没写过」。合服把清单玩家全部 CAS 成 dst;
	// 撤销里 5' 之前的写只有 7'(删公告标记,放开围栏也无害),5' 之后的任何中止都必然留下 player:zone == src。
	// 所以只看映射就够了。不用「merged_into 标记已不在」判:旧合服可能根本没写过这个标记,会误判成半撤销。
	// 读失败按半撤销处理(fail-closed:证明不了没写过,就不放开围栏)。
	halfUndone, herr := manifestPlayersBackAt(ctx, mapRdb, m.PlayerIDs, src)
	if herr != nil {
		log.Printf("WARN: cannot tell whether a previous unmerge already restored some manifest players (%v); "+
			"treating this run as a resume — a refusal below keeps the merge fence", herr)
		halfUndone = true
	} else if halfUndone {
		log.Printf("RESUME: some manifest players already map to zone %d — a previous unmerge stopped half-way; "+
			"a refusal below keeps the merge fence", src)
	}
	// refuse:P / S 段的统一拒绝出口。abortKeepingFence 走 log.Fatal,不跑 defer 的 release,围栏留在本次 run_id 上。
	refuse := func(format string, args ...any) {
		if halfUndone {
			abortKeepingFence(fmt.Sprintf("半撤销状态(上一次撤销已把部分清单玩家改回源区),本次不释放围栏:"+format, args...))
		}
		fence.refuseAndRelease(format, args...)
	}

	// P 前置门禁(目标区口径,A7):与合服共用 runPreflight,只是「必须已停干净的那个 zone」换成目标区。
	// 失败时本次一个字节都没写(围栏处置见 refuse)。
	if err := runPreflight(ctx, preflightDeps{
		sceneRdb:   sceneRdb,
		mappingRdb: mapRdb,
		sharedRdb:  sharedRdb,
		lag:        lagSrc,
	}, preflightParams{
		zone:            dst,
		scope:           "target",
		kafkaGroup:      o.kafkaGroup,
		topicGeneration: uint32(o.kafkaTopicGen),
		playerIDs:       m.PlayerIDs,
	}); err != nil {
		refuse("unmerge refused: %v", err)
	}

	// S 清单玩家的落点记录:冻结中的(搬库正在挪)一律拒绝 —— 撤销同样改 home(连带存盘 topic),与搬库互斥
	// (§9 末条)。同时记下此刻有记录的人,copy 模式的 1' 不删他们的行(理由见文件头)。还没写任何东西(围栏处置见 refuse)。
	placeReads, perr := readPlacements(ctx, mapRdb, m.PlayerIDs)
	if perr != nil {
		refuse("unmerge refused: read the manifest players' placement records: %v", perr)
	}
	if n, sample := frozenPlacementIDs(m.PlayerIDs, placeReads); n > 0 {
		refuse("unmerge refused: %d manifest players have a FROZEN placement record — a relocation is moving "+
			"them (first: %v). Finish or abort that relocation (-mode relocate / relocate-abort) first", n, sample)
	}
	placedNow := idsWithPlacementRecord(m.PlayerIDs, placeReads)

	// S(续)能力标记(§10.4 第 1 条的镜像):改回 home 之后,处理这批人存盘的是 src 的 go/db。落点记录不指向 src 的人
	// (合服后被 pin-placement 钉在 dst、或被 relocate 搬走)有效落点不随 home 改回,src 的 go/db 必须按记录选库;
	// 旧版按本进程 zone 选库,会把他们的存盘写进 zone_src_db 里的旧副本,与真源分叉且没有任何报错。
	// 畸形记录同样计入:旧版 go/db 会无视它、写进本 zone 库。只查 src:dst 在撤销之后不再处理这批人的存盘。
	if offSrc := idsPlacedOffStorage(m.PlayerIDs, placeReads, src); len(offSrc) > 0 {
		if err := checkPlacementCapabilities(ctx, mapRdb, []uint32{src}); err != nil {
			refuse("unmerge refused: %d manifest players have a placement record that does not point at zone %d "+
				"(first: %v). Their effective storage does not move back with home_zone, and after the unmerge their saves are "+
				"handled by zone %d's go/db, which must route by the record: %v. Start a placement-aware go/db in zone %d first "+
				"(it writes the marker on start-up)", len(offSrc), src, sampleUint64(offSrc), src, err, src)
		}
		log.Printf("unmerge: %d manifest players have a placement record off zone %d; %s confirmed in zone %d",
			len(offSrc), src, capabilityRoutingV1, src)
	}

	// S(续)-db-capability-zones:pin 撤销必填(已在围栏之前校验),copy 撤销给了才查。none = 运维声明此刻没有别的
	// zone 在跑,不查;列出的 zone 逐个核对,src / dst 列进来照常查(已下线就被拒,文案说明)。还没写任何东西。
	switch {
	case !o.capSpec.given:
	case o.capSpec.none:
		log.Printf("unmerge: -db-capability-zones none — no capability marker checked (operator attests that no other zone's "+
			"go/db is running). After zone-up, check zones %d and %d with -mode capability-check before opening them", src, dst)
	default:
		if err := checkCapabilityZoneSpec(ctx, mapRdb, o.capSpec, src, dst); err != nil {
			refuse("unmerge refused: -db-capability-zones: %v", err)
		}
		log.Printf("unmerge: %s confirmed in zones %v", capabilityRoutingV1, o.capSpec.zones)
	}

	// 7' 公告 flag。
	if n, err := deleteNoticeFlags(ctx, sharedRdb, m.PlayerIDs, o.dryRun); err != nil {
		log.Fatalf("clear post-merge flags: %v", err)
	} else {
		log.Printf("Post-merge flags: %d notice keys cleared", n)
	}

	// 5' mapping 改回 src。逐 id 写(而不是全量 SCAN):只有清单里的人回去。
	if n, err := restoreMappingForIDs(ctx, mapRdb, m.PlayerIDs, src, dst, o.dryRun); err != nil {
		log.Fatalf("restore mapping: %v", err)
	} else {
		log.Printf("Mapping: %d players restored to home_zone=%d", n, src)
	}

	// 5'a 合走标记(A10)。映射已改回源区,源区不再是「被合走的 zone」;留着它,源区以后就再也回填不了。
	// 只删与清单 dst / run_id 都对得上的那一把(clearMergedInto)。
	cleared, merr := clearMergedInto(ctx, mapRdb, src, dst, m.RunID, o.dryRun)
	if merr != nil {
		abortKeepingFence(fmt.Sprintf("clear %s:映射已改回源区,但标记删除失败:%v", mergedIntoKey(src), merr))
	}
	log.Printf("merged_into marker %s: cleared=%v", mergedIntoKey(src), cleared)

	// 5'b 共享玩家缓存失效(A7)。copy 模式下合服把行拷进了目标库,合服之后若有人经目标区读过这些玩家,
	// PlayerAllData / {MsgType}:{pid} 里装的就是目标库的那份;路由改回源区之后它不能再被命中。
	// 与合服步骤 1 的失效同一组键(清单的表 + 固定消息名),幂等。
	// pin 模式不拷行:有效落点一直是钉住的源区库,改回 home 不改变任何人读到的库,缓存里的就是真源的内容,不删(§10.1)。
	if rowsMode == playerRowsModePin {
		log.Printf("Player caches: kept (pin mode — no player's placement changed, so the cached data is still the live copy)")
	} else {
		inv, ierr := invalidatePlayerCaches(ctx, sharedRdb, m.PlayerIDs, m.Tables, o.dryRun)
		if ierr != nil {
			abortKeepingFence(fmt.Sprintf("invalidate player caches:映射已改回源区,但共享缓存失效失败:%v", ierr))
		}
		log.Printf("Player caches: %d shared cache keys invalidated for %d manifest players", inv, len(m.PlayerIDs))
	}

	// 4' ZSET。
	if len(m.RankMembers) > 0 {
		release, lerr := acquireGuildRankLock(ctx, guildRdb, o.dryRun)
		if lerr != nil {
			log.Fatalf("guild rank lock: %v", lerr)
		}
		n, rerr := unmergeRankZSET(ctx, guildRdb, src, dst, m.RankMembers, o.dryRun)
		release()
		if rerr != nil {
			log.Fatalf("restore rank ZSET: %v", rerr)
		}
		log.Printf("Redis rank: %d members restored to guild_rank:zone:%d", n, src)
	}

	// 3b' 聚宝斋商品 market_zone 改回,只针对清单里的 listing_id 且当前确实是 dst 的。
	// 与 3' 一样不看 -skip-* 开关:清单里有 id 就说明当初真的收集过。清单里没有 id
	// (旧清单 / 合服时 -skip-trade-mysql)就什么都不做。
	tradeRestored := 0
	if len(m.TradeListingIDs) > 0 {
		n, terr := restoreTradeMarketZone(ctx, db, o.tradeSchema, m.TradeListingIDs, src, dst, o.dryRun)
		if terr != nil {
			// mapping / ZSET 已经改回,围栏与 3' 同理不提前释放。
			abortKeepingFence(fmt.Sprintf("restore trade market_zone:改回中途失败:%v", terr))
		}
		tradeRestored = n
		verb := "restored"
		if o.dryRun {
			verb = "would be restored"
		}
		log.Printf("MySQL: %d of %d manifest trade listings %s to market_zone=%d", n, len(m.TradeListingIDs), verb, src)
	}

	// 3' guild.zone_id 改回,只针对清单里的 guild_id,逐条主键点更新(锁序见 restoreGuildZone):
	// 原先的 `zone_id = dst AND guild_id IN (...)` 可能被规划成 idx_guild_0 range,与目标区公会的解散反序成环。
	if len(m.GuildIDs) > 0 {
		n, gerr := restoreGuildZone(ctx, db, o.guildSchema, m.GuildIDs, src, dst, o.dryRun)
		if gerr != nil {
			abortKeepingFence(fmt.Sprintf("restore guild zone_id:改回中途失败:%v", gerr))
		}
		verb := "restored"
		if o.dryRun {
			verb = "would be restored"
		}
		log.Printf("MySQL: %d of %d manifest guilds %s to zone_id=%d", n, len(m.GuildIDs), verb, src)
		// 失效失败时 zone_id 已经改回,与 3' 写失败同属「改回到一半」:围栏同样保留、给续跑指引。
		// 重跑时 3' 按 `zone_id = dst` 幂等(已改回的影响 0 行),失效对全清单再做一遍。
		inv, ierr := invalidateGuildCaches(ctx, guildRdb, m.GuildIDs, o.dryRun)
		if ierr != nil {
			abortKeepingFence(fmt.Sprintf("restore guild zone_id:已改回 %d 行,但 guild:v2 缓存失效失败:%v", n, ierr))
		}
		log.Printf("Guild cache: %d guild:v2 entries invalidated", inv)
	}

	// 1' 目标库玩家行。只删逐字节相同的。pin 模式没拷过行(清单也不记表),这里什么都不删。
	// copy 模式跳过此刻有落点记录的人(S 段读到的):他们的有效落点由记录决定,不随 home 改回,
	// 目标库那一行可能就是真源 —— 逐字节相同证明不了它是可以删的拷贝。
	srcSchema, dstSchema := zoneDBName(src), zoneDBName(dst)
	deleteIDs := uint64sNotIn(m.PlayerIDs, placedNow)
	tablesToClean := m.Tables
	if rowsMode == playerRowsModePin {
		tablesToClean = nil
		log.Printf("Player rows: none deleted (pin mode never copied rows; placement records are left as they are)")
	} else if kept := len(m.PlayerIDs) - len(deleteIDs); kept > 0 {
		log.Printf("Player rows: %d manifest players have a placement record now — their rows are left untouched in %s",
			kept, dstSchema)
	}
	totalDeleted, totalRefused := 0, 0
	var refusedSample []uint64
	for _, t := range tablesToClean {
		deleted, refused, err := deletePlayerRows(ctx, db, srcSchema, dstSchema, t, deleteIDs, o.dryRun)
		if err != nil {
			log.Fatalf("delete target rows for %s: %v", t, err)
		}
		totalDeleted += deleted
		totalRefused += len(refused)
		if len(refusedSample) < 20 {
			for _, id := range refused {
				refusedSample = append(refusedSample, id)
				if len(refusedSample) >= 20 {
					break
				}
			}
		}
		log.Printf("%s.%s: %d rows deleted, %d refused (row differs from the source copy)", dstSchema, t, deleted, len(refused))
	}
	if totalRefused > 0 {
		log.Printf("REFUSED to delete %d target-zone rows because they no longer match the source copy "+
			"(first ids: %v). Those players have been played after the merge — deleting them would destroy "+
			"data that exists nowhere else. The mapping already points them back at zone %d, so the leftover "+
			"rows in %s are inert; decide manually whether to keep or merge them.",
			totalRefused, refusedSample, src, dstSchema)
	}

	log.Printf("=== Unmerge done (players=%d guilds=%d rank=%d trade_listings=%d/%d rows_deleted=%d rows_refused=%d) ===",
		len(m.PlayerIDs), len(m.GuildIDs), len(m.RankMembers), tradeRestored, len(m.TradeListingIDs), totalDeleted, totalRefused)
	if o.migrateBlobs {
		log.Printf("NOTE: player:{id}:* blobs copied into the target data Redis are NOT deleted. " +
			"With the mapping restored nobody reads them; delete manually only after verifying the source copies.")
	}
}

// manifestPlayersBackAt 报告清单玩家里是否已有人的 player:zone 等于 zone(撤销用 src 调:已有人被改回源区 =
// 上一次撤销走过了 5')。只读,一批一次 MGET。
func manifestPlayersBackAt(ctx context.Context, rdb *redis.Client, ids []uint64, zone uint32) (bool, error) {
	vals, present, err := readPlayerZones(ctx, rdb, ids)
	if err != nil {
		return false, err
	}
	return anyMappedTo(vals, present, zone), nil
}

// anyMappedTo 是 manifestPlayersBackAt 的纯判定:vals / present 与 readPlayerZones 的返回同形。
func anyMappedTo(vals []string, present []bool, zone uint32) bool {
	want := strconv.FormatUint(uint64(zone), 10)
	for i, v := range vals {
		if i < len(present) && present[i] && v == want {
			return true
		}
	}
	return false
}

// restoreMappingForIDs 把清单里的玩家改回 src —— 且**只**改那些当前值确实是
// dst 的键。已经被别的流程改成第三个 zone 的不动:那不是这次合服造成的,
// 撤销不该越权。
func restoreMappingForIDs(ctx context.Context, rdb *redis.Client, ids []uint64, src, dst uint32, dryRun bool) (int, error) {
	sVal := strconv.FormatUint(uint64(src), 10)
	dVal := strconv.FormatUint(uint64(dst), 10)
	restored := 0
	for _, batch := range chunkUint64(ids, mappingScanCount) {
		keys := make([]string, 0, len(batch))
		for _, id := range batch {
			keys = append(keys, playerZoneKeyPrefix+strconv.FormatUint(id, 10))
		}
		vals, err := rdb.MGet(ctx, keys...).Result()
		if err != nil {
			return restored, fmt.Errorf("mget mapping (%d keys): %w", len(keys), err)
		}
		var hits []string
		for i, k := range keys {
			s, ok := vals[i].(string)
			if !ok {
				// 键不存在:合服前它就没有映射(存量玩家),撤销后也不该凭空造一个。
				continue
			}
			switch s {
			case dVal:
				hits = append(hits, k)
			case sVal:
				// 已经是源区了(撤销重跑),幂等跳过。
			default:
				log.Printf("WARN: %s currently maps to zone %s (neither %d nor %d) — left untouched", k, s, src, dst)
			}
		}
		restored += len(hits)
		if dryRun || len(hits) == 0 {
			continue
		}
		pipe := rdb.Pipeline()
		for _, k := range hits {
			pipe.Set(ctx, k, sVal, 0)
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return restored, fmt.Errorf("restore mapping pipeline (%d keys): %w", len(hits), err)
		}
	}
	return restored, nil
}

// deleteNoticeFlags 清掉合服公告标记。撤销之后「合服」没有发生过,
// 玩家不该在下次登录时看到合服公告。
func deleteNoticeFlags(ctx context.Context, rdb *redis.Client, ids []uint64, dryRun bool) (int, error) {
	deleted := 0
	for _, batch := range chunkUint64(ids, stampBatchSize) {
		if dryRun {
			deleted += len(batch)
			continue
		}
		pipe := rdb.Pipeline()
		cmds := make([]*redis.IntCmd, 0, len(batch)*2)
		for _, id := range batch {
			idStr := strconv.FormatUint(id, 10)
			cmds = append(cmds, pipe.Del(ctx, mergeNoticeKeyPrefix+idStr), pipe.Del(ctx, forceRenameKeyPrefix+idStr))
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return deleted, fmt.Errorf("delete post-merge flags: %w", err)
		}
		for _, c := range cmds {
			deleted += int(c.Val())
		}
	}
	return deleted, nil
}
