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
//	7' 清掉 player_merge_notice:{pid}(合服没发生过,不该弹公告)
//	5' player:zone:{id} 改回 src            ← 先做,路由回源区
//	4' guild_rank ZSET 成员 ZADD 回 src、从 dst ZREM
//	3' guild.zone_id 改回 src(只改清单里的 guild_id)+ 缓存失效
//	1' 删目标库里那些**与源库逐字节相同**的玩家行
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

func runUnmerge(o options) {
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()

	m, err := loadManifest(o.manifestPath)
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

	log.Printf("=== UNMERGE zone %d → %d (reversing run_id=%s started %s; dry-run=%v) ===",
		src, dst, m.RunID, m.StartedAt, o.dryRun)
	log.Printf("    scope: %d players, %d guilds, %d rank members, tables=%v",
		len(m.PlayerIDs), len(m.GuildIDs), len(m.RankMembers), m.Tables)
	log.Printf("    NOTHING outside this list is touched — target-zone natives are safe by construction.")

	db := mustOpenMySQL(ctx, o.mysqlDSN)
	defer db.Close()
	mapRdb := mustDial(ctx, "mapping", o.mappingAddr, o.mappingPwd, o.mappingDB)
	defer mapRdb.Close()
	guildRdb := mustDial(ctx, "guild", o.redisAddr, o.redisPwd, o.redisDB)
	defer guildRdb.Close()
	sharedRdb := mustDial(ctx, "login/shared(DB0)", o.noticeAddr, o.noticePwd, o.noticeDB)
	defer sharedRdb.Close()

	// 撤销期间同样上围栏:别人不许在这两个 zone 里建新对象。
	runID := newRunID(src, dst, time.Now())
	fence, err := acquireMergeFence(ctx, mapRdb, src, dst, runID, o.manifestPath, o.timeout, o.dryRun)
	if err != nil {
		log.Fatalf("merge fence: %v", err)
	}
	defer fence.release(context.Background())

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

	// 3' guild.zone_id 改回,只针对清单里的 guild_id。
	if len(m.GuildIDs) > 0 {
		if o.dryRun {
			log.Printf("[DRY-RUN] Would set zone_id=%d on %d guild rows", src, len(m.GuildIDs))
		} else {
			restored := 0
			for _, batch := range chunkUint64(m.GuildIDs, playerRowsBatchSize) {
				res, err := db.ExecContext(ctx,
					fmt.Sprintf("UPDATE guild SET zone_id = ? WHERE zone_id = ? AND guild_id IN (%s)", inListLiteral(batch)),
					src, dst)
				if err != nil {
					log.Fatalf("restore guild zone_id: %v", err)
				}
				n, _ := res.RowsAffected()
				restored += int(n)
			}
			log.Printf("MySQL: %d guild rows restored to zone_id=%d", restored, src)
		}
		if n, err := invalidateGuildCaches(ctx, guildRdb, m.GuildIDs, o.dryRun); err != nil {
			log.Fatalf("invalidate guild caches: %v", err)
		} else {
			log.Printf("Guild cache: %d guild:v2 entries invalidated", n)
		}
	}

	// 1' 目标库玩家行。只删逐字节相同的。
	srcSchema, dstSchema := zoneDBName(src), zoneDBName(dst)
	totalDeleted, totalRefused := 0, 0
	var refusedSample []uint64
	for _, t := range m.Tables {
		deleted, refused, err := deletePlayerRows(ctx, db, srcSchema, dstSchema, t, m.PlayerIDs, o.dryRun)
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

	log.Printf("=== Unmerge done (players=%d guilds=%d rank=%d rows_deleted=%d rows_refused=%d) ===",
		len(m.PlayerIDs), len(m.GuildIDs), len(m.RankMembers), totalDeleted, totalRefused)
	if o.migrateBlobs {
		log.Printf("NOTE: player:{id}:* blobs copied into the target data Redis are NOT deleted. " +
			"With the mapping restored nobody reads them; delete manually only after verifying the source copies.")
	}
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
