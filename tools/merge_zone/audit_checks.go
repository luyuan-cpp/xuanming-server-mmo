package main

// 审计的两组新检查:
//
//  A. 合服**前**的真门禁(pre-merge)。旧的 auditOnlineKeys 名义上是 block 级,
//     实际上永远拦不住任何东西:
//       - 它在 **mapping Redis(DB 0)** 上查 `friend:online:{pid}`,而这把键
//         由 go/friend 写在**它自己的 Redis(DB 3,go/friend/etc 的 RedisClient)**。
//         查错库 ⇒ 恒 0 ⇒ 恒「全部离线 ✅」。
//       - pipeline 的错误被 `_, _ = pipe.Exec(ctx)` 吞掉,超时 / WRONGTYPE
//         同样落到「0 在线」。
//       - 文档写了「exit 2 = 基础设施错误」,但 runAuditEntry 从来没返回过 2:
//         连不上 Redis 只打一行 WARN,然后审计「通过」。
//     修法:分开 friend Redis(DB 3)与共享 DB 0 两个句柄;扫描失败一律
//     Severity=block;句柄缺失让整个审计以 exit 2 结束(见 auditFatal)。
//
//  B. 合服**后**的验证(-verify-merged)。此前 -verify-merged 只是把标题里的
//     "pre-merge" 换成 "POST-MERGE VERIFICATION",一条断言都没有。这里把
//     runbook §5 Step 5 的那张表逐条实现成 block 级 auditor。

import (
	"context"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// auditFatal 由 auditor 设置,表示「这不是数据问题,是我们根本没查成」。
// runAuditEntry 见到它就以 exit 2 结束 —— 与 exit 1(查到了真问题)区分开,
// ops 脚本才能分辨「不许合服」和「审计本身坏了,结论不可信」。
const auditNoteInfraPrefix = "INFRA: "

func infraAudit(name, format string, args ...any) ResourceAudit {
	return ResourceAudit{
		Name:     name,
		Severity: "block",
		Notes:    auditNoteInfraPrefix + fmt.Sprintf(format, args...),
	}
}

func isInfraAudit(r ResourceAudit) bool {
	return len(r.Notes) >= len(auditNoteInfraPrefix) && r.Notes[:len(auditNoteInfraPrefix)] == auditNoteInfraPrefix
}

// ── A. 合服前门禁 ─────────────────────────────────────────────

// auditOnlinePresence 查源区玩家还在不在线。两个证据面:
//   - friend:online:{pid}  —— go/friend 写,60s TTL,**DB 3**
//   - player:session:{pid} —— player_locator 写,**DB 0**
//
// 任一非零 = zone-down 没做完 / 还有进程在 ACK,block。
// 扫描失败也是 block(而且是 INFRA 级):查不到就不能说「没人在线」。
func auditOnlinePresence(ctx context.Context, cfg auditConfig) ResourceAudit {
	r := ResourceAudit{Name: "online_presence", UniqueScope: "per_zone"}
	if cfg.mappingDB == nil {
		return infraAudit(r.Name, "no mapping Redis handle — cannot enumerate source-zone players")
	}
	pids, err := collectPlayerIDsWithHomeZone(ctx, cfg.mappingDB, cfg.src)
	if err != nil {
		return infraAudit(r.Name, "mapping scan failed: %v", err)
	}
	r.SourceCount = int64(len(pids))

	if cfg.friendRDB == nil {
		return infraAudit(r.Name, "no friend Redis handle (-friend-redis-addr/-friend-redis-db) — "+
			"friend:online:{pid} lives in the friend service DB (go/friend/etc: DB 3), not the mapping DB")
	}
	friendOnline, err := countExistingKeys(ctx, cfg.friendRDB, pids, "friend:online:")
	if err != nil {
		return infraAudit(r.Name, "friend:online scan failed: %v", err)
	}
	if cfg.sharedRDB == nil {
		return infraAudit(r.Name, "no shared (DB 0) Redis handle — cannot check player:session:{pid}")
	}
	sessions, err := countExistingKeys(ctx, cfg.sharedRDB, pids, "player:session:")
	if err != nil {
		return infraAudit(r.Name, "player:session scan failed: %v", err)
	}

	r.TargetCount = int64(friendOnline + sessions)
	r.ConflictCount = int64(sessions)
	if friendOnline > 0 || sessions > 0 {
		r.Severity = "block"
		r.Notes = fmt.Sprintf("%d players still have friend:online (DB %d), %d still have player:session (DB %d). "+
			"Wait for the 60s friend TTL or investigate — zone-down is incomplete.",
			friendOnline, cfg.friendDBIndex, sessions, cfg.sharedDBIndex)
		return r
	}
	r.Severity = "info"
	r.Notes = fmt.Sprintf("0 friend:online (DB %d) and 0 player:session (DB %d) for %d source players",
		cfg.friendDBIndex, cfg.sharedDBIndex, len(pids))
	return r
}

// auditPlayerLocks 查源区玩家有没有在持的分布式锁。
//   - lock:player:{id}     —— data_service router.go,**mapping Redis**
//   - player:{id}:__lock   —— data_service router.go,**data Redis**(可选句柄)
//
// 非零 = 有人正在改这些玩家的数据,合服搬走的会是半截状态。
func auditPlayerLocks(ctx context.Context, cfg auditConfig) ResourceAudit {
	r := ResourceAudit{Name: "player_locks", UniqueScope: "global"}
	if cfg.mappingDB == nil {
		return infraAudit(r.Name, "no mapping Redis handle")
	}
	pids, err := collectPlayerIDsWithHomeZone(ctx, cfg.mappingDB, cfg.src)
	if err != nil {
		return infraAudit(r.Name, "mapping scan failed: %v", err)
	}
	r.SourceCount = int64(len(pids))

	mappingLocks, err := countExistingKeys(ctx, cfg.mappingDB, pids, "lock:player:")
	if err != nil {
		return infraAudit(r.Name, "lock:player scan failed: %v", err)
	}
	dataLocks := 0
	dataNote := "data-Redis player:{id}:__lock not checked (no -source-data-redis)"
	if cfg.dataRDB != nil {
		for _, pid := range pids {
			n, derr := cfg.dataRDB.Exists(ctx, fmt.Sprintf("player:{%d}:__lock", pid)).Result()
			if derr != nil {
				return infraAudit(r.Name, "player:{id}:__lock scan failed: %v", derr)
			}
			if n > 0 {
				dataLocks++
			}
		}
		dataNote = fmt.Sprintf("%d player:{id}:__lock held in the data Redis", dataLocks)
	}
	r.ConflictCount = int64(mappingLocks + dataLocks)
	if r.ConflictCount > 0 {
		r.Severity = "block"
		r.Notes = fmt.Sprintf("%d lock:player:* held in the mapping Redis; %s. Someone is mid-write.", mappingLocks, dataNote)
		return r
	}
	r.Severity = "info"
	r.Notes = fmt.Sprintf("0 lock:player:* held; %s", dataNote)
	return r
}

// auditKafkaQueues 查 go/db 的重试 / 死信 / 处理中队列。
// 键名镜像 go/db/internal/kafka/key_ordered_consumer.go,住在 go/db 的
// RedisClient(db.yaml: DB 0)。非空 = 有存盘任务**没有**进 zone 库。
func auditKafkaQueues(ctx context.Context, cfg auditConfig) ResourceAudit {
	r := ResourceAudit{Name: "kafka_db_task_queues", UniqueScope: "per_zone"}
	if cfg.sharedRDB == nil {
		return infraAudit(r.Name, "no shared (DB 0) Redis handle — cannot inspect kafka:retry/dead queues")
	}
	topic := dbTaskTopic(cfg.src, cfg.topicGeneration)
	var total int64
	details := make([]string, 0, 3)
	for _, k := range []string{dbRetryQueueKey(topic), dbRetryProcessingKey(topic), dbDeadQueueKey(topic)} {
		n, err := cfg.sharedRDB.LLen(ctx, k).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			return infraAudit(r.Name, "llen %s: %v", k, err)
		}
		total += n
		details = append(details, fmt.Sprintf("%s=%d", k, n))
	}
	r.SourceCount = total
	if total > 0 {
		r.Severity = "block"
		r.Notes = fmt.Sprintf("%d db_task payload(s) never reached zone_%d_db (%v). Drain or triage before merging.",
			total, cfg.src, details)
		return r
	}
	r.Severity = "info"
	r.Notes = fmt.Sprintf("all empty (%v)", details)
	return r
}

// auditSourceZoneNodes 查源区场景节点负载集。与 merge 的 preflight P2 同一判据,
// 放进审计是为了让 T-1 彩排就能发现「zone-down 其实没做干净」。
func auditSourceZoneNodes(ctx context.Context, cfg auditConfig) ResourceAudit {
	r := ResourceAudit{Name: "source_scene_nodes", UniqueScope: "per_zone"}
	if cfg.sceneRDB == nil {
		return infraAudit(r.Name, "no scene_manager Redis handle (-scene-redis-addr/-scene-redis-db)")
	}
	key := fmt.Sprintf(sceneNodeLoadKeyFmt, cfg.src)
	n, err := cfg.sceneRDB.ZCard(ctx, key).Result()
	if err != nil {
		return infraAudit(r.Name, "zcard %s: %v", key, err)
	}
	r.SourceCount = n
	if n > 0 {
		r.Severity = "block"
		r.Notes = fmt.Sprintf("%s still has %d live scene node(s) — zone-down is not complete", key, n)
		return r
	}
	r.Severity = "info"
	r.Notes = key + " is empty"
	return r
}

// ── B. 合服后验证(-verify-merged)─────────────────────────────

// verifyMappingDrained: mapping 里 home_zone==src 的玩家数必须是 0。
func verifyMappingDrained(ctx context.Context, cfg auditConfig) ResourceAudit {
	r := ResourceAudit{Name: "verify:mapping_src", UniqueScope: "global"}
	if cfg.mappingDB == nil {
		return infraAudit(r.Name, "no mapping Redis handle")
	}
	pids, err := collectPlayerIDsWithHomeZone(ctx, cfg.mappingDB, cfg.src)
	if err != nil {
		return infraAudit(r.Name, "mapping scan failed: %v", err)
	}
	dstIDs, err := collectPlayerIDsWithHomeZone(ctx, cfg.mappingDB, cfg.dst)
	if err != nil {
		return infraAudit(r.Name, "mapping scan (dst) failed: %v", err)
	}
	r.SourceCount = int64(len(pids))
	r.TargetCount = int64(len(dstIDs))
	if len(pids) != 0 {
		r.Severity = "block"
		r.Notes = fmt.Sprintf("%d players still map to home_zone=%d — remap did not complete", len(pids), cfg.src)
		return r
	}
	if cfg.expectedSrcPlayers >= 0 && int64(len(dstIDs)) < cfg.expectedSrcPlayers {
		r.Severity = "block"
		r.Notes = fmt.Sprintf("home_zone=%d has only %d players but at least %d were expected "+
			"(-expected-src-players recorded at T-1)", cfg.dst, len(dstIDs), cfg.expectedSrcPlayers)
		return r
	}
	r.Severity = "info"
	r.Notes = fmt.Sprintf("home_zone=%d drained to 0; home_zone=%d now holds %d players", cfg.src, cfg.dst, len(dstIDs))
	return r
}

// verifyGuildZoneDrained: guild WHERE zone_id=src 必须是 0。
func verifyGuildZoneDrained(ctx context.Context, cfg auditConfig) ResourceAudit {
	r := ResourceAudit{Name: "verify:guild_zone", UniqueScope: "global"}
	if cfg.db == nil {
		return infraAudit(r.Name, "no MySQL handle")
	}
	if err := cfg.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild WHERE zone_id = ?", cfg.src).Scan(&r.SourceCount); err != nil {
		return infraAudit(r.Name, "count guild zone_id=%d: %v", cfg.src, err)
	}
	if err := cfg.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild WHERE zone_id = ?", cfg.dst).Scan(&r.TargetCount); err != nil {
		return infraAudit(r.Name, "count guild zone_id=%d: %v", cfg.dst, err)
	}
	if r.SourceCount != 0 {
		r.Severity = "block"
		r.Notes = fmt.Sprintf("%d guild rows still carry zone_id=%d", r.SourceCount, cfg.src)
		return r
	}
	r.Severity = "info"
	r.Notes = fmt.Sprintf("zone_id=%d drained; zone_id=%d holds %d guilds", cfg.src, cfg.dst, r.TargetCount)
	return r
}

// verifyGuildRankZSets: 源区 ZSET 不存在,且目标区 ZCARD == COUNT(guild WHERE zone_id=dst)。
//
// 后半条是真正有价值的那条:ZSET 是可重建缓存,合完之后它与 MySQL 的公会数
// 必须一致 —— 不一致意味着 ZADD 漏了成员(旧的非事务 pipeline 正是这么漏的),
// 表现为公会在榜单上凭空消失。
func verifyGuildRankZSets(ctx context.Context, cfg auditConfig) ResourceAudit {
	r := ResourceAudit{Name: "verify:guild_rank", UniqueScope: "per_zone"}
	if cfg.rankRDB == nil {
		return infraAudit(r.Name, "no guild (rank) Redis handle")
	}
	srcKey, dstKey := guildZoneRankKey(cfg.src), guildZoneRankKey(cfg.dst)
	exists, err := cfg.rankRDB.Exists(ctx, srcKey).Result()
	if err != nil {
		return infraAudit(r.Name, "exists %s: %v", srcKey, err)
	}
	r.SourceCount = exists
	dstCard, err := cfg.rankRDB.ZCard(ctx, dstKey).Result()
	if err != nil {
		return infraAudit(r.Name, "zcard %s: %v", dstKey, err)
	}
	r.TargetCount = dstCard
	if exists != 0 {
		r.Severity = "block"
		r.Notes = srcKey + " still exists — the rank merge did not delete the source ZSET"
		return r
	}
	if cfg.db == nil {
		r.Severity = "warn"
		r.Notes = fmt.Sprintf("%s removed; %s has %d members (no MySQL handle to cross-check against guild rows)", srcKey, dstKey, dstCard)
		return r
	}
	var guildRows int64
	if err := cfg.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild WHERE zone_id = ?", cfg.dst).Scan(&guildRows); err != nil {
		return infraAudit(r.Name, "count guild zone_id=%d: %v", cfg.dst, err)
	}
	r.ConflictCount = dstCard - guildRows
	if dstCard != guildRows {
		r.Severity = "block"
		r.Notes = fmt.Sprintf("%s has %d members but MySQL has %d guilds with zone_id=%d — "+
			"the ZSET and the authoritative table disagree", dstKey, dstCard, guildRows, cfg.dst)
		return r
	}
	r.Severity = "info"
	r.Notes = fmt.Sprintf("%s gone; %s has %d members == %d guild rows", srcKey, dstKey, dstCard, guildRows)
	return r
}

// verifyTargetZoneRows: zone_{dst}_db 的每张玩家表行数 >= 合过来的玩家数。
//
// 「>=」而不是「==」:目标区本来就有自己的玩家。下界用 -expected-src-players
// (T-1 记录的源区人数);没给就退化成「表存在且非空」的弱断言并标 warn ——
// 弱断言必须看起来就弱,不能伪装成通过。
func verifyTargetZoneRows(ctx context.Context, cfg auditConfig) ResourceAudit {
	r := ResourceAudit{Name: "verify:target_zone_rows", UniqueScope: "per_zone"}
	if cfg.db == nil {
		return infraAudit(r.Name, "no MySQL handle")
	}
	schema := zoneDBName(cfg.dst)
	if err := assertSchemaExists(ctx, cfg.db, schema); err != nil {
		return infraAudit(r.Name, "%v", err)
	}
	tables, err := discoverPlayerTables(ctx, cfg.db, schema, cfg.tableCandidates)
	if err != nil {
		return infraAudit(r.Name, "%v", err)
	}
	if len(tables) == 0 {
		return infraAudit(r.Name, "no player tables found in %s (checked %v)", schema, cfg.tableCandidates)
	}
	var minRows int64 = -1
	var worst string
	for _, t := range tables {
		var n int64
		if err := cfg.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+schema+"."+t).Scan(&n); err != nil {
			return infraAudit(r.Name, "count %s.%s: %v", schema, t, err)
		}
		if minRows < 0 || n < minRows {
			minRows, worst = n, t
		}
	}
	r.TargetCount = minRows
	if cfg.expectedSrcPlayers < 0 {
		r.Severity = "warn"
		r.Notes = fmt.Sprintf("%s player tables %v; smallest is %s with %d rows. "+
			"Pass -expected-src-players <N recorded at T-1> to turn this into a real assertion.",
			schema, tables, worst, minRows)
		return r
	}
	r.SourceCount = cfg.expectedSrcPlayers
	// player_database 是每个玩家必有的一行;player_database_1 /
	// player_centre_database 允许缺行(玩家没触发过对应功能),所以下界
	// 只对 player_database 断言,其余表只报数。
	var mainRows int64
	if err := cfg.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+schema+".player_database").Scan(&mainRows); err != nil {
		return infraAudit(r.Name, "count %s.player_database: %v", schema, err)
	}
	r.TargetCount = mainRows
	if mainRows < cfg.expectedSrcPlayers {
		r.Severity = "block"
		r.Notes = fmt.Sprintf("%s.player_database has %d rows but at least %d source players were merged in — "+
			"player main data did not arrive", schema, mainRows, cfg.expectedSrcPlayers)
		return r
	}
	r.Severity = "info"
	r.Notes = fmt.Sprintf("%s.player_database has %d rows >= %d merged source players (tables checked: %v)",
		schema, mainRows, cfg.expectedSrcPlayers, tables)
	return r
}

// verifySourceHotStateGone: 源区 scene_manager 热状态应当已经清掉。
// 只在给了 scene Redis 句柄时跑;没清也只是 warn —— 它不影响数据正确性,
// 只影响 scene_manager 的孤儿清理与登录时的三次判定开销。
func verifySourceHotStateGone(ctx context.Context, cfg auditConfig) ResourceAudit {
	r := ResourceAudit{Name: "verify:source_hot_state", UniqueScope: "per_zone"}
	if cfg.sceneRDB == nil {
		r.Severity = "warn"
		r.Notes = "no scene_manager Redis handle — source hot state not verified"
		return r
	}
	var leftovers int64
	for _, pat := range append(sourceZoneScanPatterns(cfg.src), fmt.Sprintf(sceneNodeLoadKeyFmt, cfg.src)) {
		var cur uint64
		for {
			keys, next, err := cfg.sceneRDB.Scan(ctx, cur, pat, 500).Result()
			if err != nil {
				return infraAudit(r.Name, "scan %s: %v", pat, err)
			}
			leftovers += int64(len(keys))
			cur = next
			if cur == 0 {
				break
			}
		}
	}
	r.SourceCount = leftovers
	if leftovers > 0 {
		r.Severity = "warn"
		r.Notes = fmt.Sprintf("%d source-zone scene_manager keys remain — re-run merge with -clear-source-hot-state", leftovers)
		return r
	}
	r.Severity = "info"
	r.Notes = "source-zone scene_manager keys are gone"
	return r
}

// verifyFenceReleased: merge:in_progress:{src|dst} 必须已经释放。
// 忘了释放 = data_service / guild 会永久拒绝新建玩家与公会。
func verifyFenceReleased(ctx context.Context, cfg auditConfig) ResourceAudit {
	r := ResourceAudit{Name: "verify:merge_fence", UniqueScope: "global"}
	if cfg.mappingDB == nil {
		return infraAudit(r.Name, "no mapping Redis handle")
	}
	var held []string
	for _, z := range []uint32{cfg.src, cfg.dst} {
		key := mergeFenceKey(z)
		n, err := cfg.mappingDB.Exists(ctx, key).Result()
		if err != nil {
			return infraAudit(r.Name, "exists %s: %v", key, err)
		}
		if n > 0 {
			held = append(held, key)
		}
	}
	r.SourceCount = int64(len(held))
	if len(held) > 0 {
		r.Severity = "block"
		r.Notes = fmt.Sprintf("merge fence still set: %v — data_service RegisterPlayerZone and guild CreateGuild "+
			"are refusing new objects in those zones. DEL them once no merge_zone process is alive.", held)
		return r
	}
	r.Severity = "info"
	r.Notes = "no merge:in_progress:* fence left behind"
	return r
}
