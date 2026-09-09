package main

// 公会侧的三件事:zone_id 改写、榜单 ZSET 合并、缓存失效。
//
// 三点与旧实现的差别,每一条都是修一个真 bug:
//
//  1. 重名冲突路径删掉了。deploy/mysql-init/guild_friend_tables.sql 里
//     `UNIQUE KEY uk_name (name)` 是**全局**唯一(不带 zone_id),所以
//     「源区有个公会叫 X,目标区也有个叫 X」在库层面根本不可能存在,
//     旧的 checkNameConflicts JOIN 永远返回空。它的害处不在于慢,而在于
//     `UPDATE ... AND guild_id NOT IN (...)` 这条**跳过冲突继续写**的分支
//     给人一种「冲突会被优雅处理」的错觉。真要哪天 uk_name 改成
//     (zone_id, name),正确做法是**中止**而不是跳过 —— 跳过会把源区公会
//     留在一个已经下线的 zone 里,等于把整个公会连同成员一起丢掉。
//     所以现在:检测到任何同名行 → 直接报错,一个字节都不写。
//
//  2. ZSET 合并做成一次事务(MULTI/EXEC)。旧实现用普通 Pipeline:
//     ZADD 与 DEL 是两条独立命令,中间断连会留下「源区 ZSET 已删、目标区
//     没加进去」—— 榜单数据无声消失,而且 ZSET 是缓存、MySQL 没有它的分数
//     快照(guild.score 是权威分,但 ZSET 里可能是重建前的旧值),补不回来。
//     TxPipelined 把两条命令包进 MULTI/EXEC,要么都生效要么都不生效。
//
//  3. 改完 zone_id 之后必须让 guild 服的缓存失效。guild 是**全局服务**,
//     不随 zone-down 重启;它的 guild:v2:{id} 缓存 TTL 是 30 分钟
//     (guild.yaml 的 DefaultTTL),里面存着旧的 zone_id。不失效的话,
//     合服后半小时内玩家看到的公会仍属于那个已经不存在的区。
//     失效方式镜像 guild_repo.go 的 invalidateVersionedCacheScript:
//     INCR generation 再 DEL 数据键 —— 只 DEL 不 INCR 会被一个正在途中的
//     旧 reader 用旧快照重新填回来(那正是 generation 机制存在的原因)。

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strconv"

	"github.com/redis/go-redis/v9"
)

// invalidateGuildVersionedCacheScript 逐字镜像
// go/guild/internal/data/guild_repo.go::invalidateVersionedCacheScript。
// KEYS[1]=guild:v2:cache_generation:{id}  KEYS[2]=guild:v2:{id}
var invalidateGuildVersionedCacheScript = redis.NewScript(`
redis.call("INCR", KEYS[1])
redis.call("DEL", KEYS[2])
return 1
`)

func guildCacheKey(id uint64) string { return "guild:v2:" + strconv.FormatUint(id, 10) }
func guildCacheGenerationKey(id uint64) string {
	return "guild:v2:cache_generation:" + strconv.FormatUint(id, 10)
}
func guildZoneRankKey(zone uint32) string {
	return "guild_rank:zone:" + strconv.FormatUint(uint64(zone), 10)
}

// ── MySQL ─────────────────────────────────────────────────────

// collectGuildIDsInZone 取源区公会 id。必须在 UPDATE **之前**调用:
// 改完 zone_id 之后就再也分不清哪些是搬过来的、哪些是目标区原住民,
// 而缓存失效与撤销都只能作用于「搬过来的那些」。
func collectGuildIDsInZone(ctx context.Context, db *sql.DB, zone uint32) ([]uint64, error) {
	rows, err := db.QueryContext(ctx, "SELECT guild_id FROM guild WHERE zone_id = ? ORDER BY guild_id", zone)
	if err != nil {
		return nil, fmt.Errorf("list guilds in zone %d: %w", zone, err)
	}
	defer rows.Close()
	var out []uint64
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// assertNoGuildNameCollision 在写之前确认没有同名公会跨这两个 zone 存在。
//
// 今天它恒为真(uk_name 全局唯一),留着是**契约断言**:如果哪天有人把
// uk_name 改成 (zone_id, name),这条查询会立刻开始命中,合服会停在这里
// 而不是悄悄漏掉几个公会。这就是「让冲突在任何写之前中止」的落点。
func assertNoGuildNameCollision(ctx context.Context, db *sql.DB, src, dst uint32) error {
	rows, err := db.QueryContext(ctx,
		`SELECT s.guild_id, d.guild_id, s.name
		   FROM guild s JOIN guild d ON s.name = d.name AND d.zone_id = ?
		  WHERE s.zone_id = ?`, dst, src)
	if err != nil {
		return fmt.Errorf("guild name collision probe: %w", err)
	}
	defer rows.Close()
	var collisions int
	for rows.Next() {
		var srcID, dstID uint64
		var name string
		if err := rows.Scan(&srcID, &dstID, &name); err != nil {
			return err
		}
		collisions++
		log.Printf("GUILD NAME COLLISION: source guild_id=%d and target guild_id=%d both named %q", srcID, dstID, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if collisions > 0 {
		return fmt.Errorf("%d guild name collision(s) across zone %d and %d. "+
			"guild.name is globally UNIQUE today, so this should be impossible — the schema changed under us. "+
			"Rename or disband the colliding guilds first; refusing to write anything", collisions, src, dst)
	}
	return nil
}

// migrateGuildZone 把 zone_id 从 src 改成 dst。返回受影响行数。
func migrateGuildZone(ctx context.Context, db *sql.DB, src, dst uint32, dryRun bool) (int64, error) {
	if dryRun {
		var count int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild WHERE zone_id = ?", src).Scan(&count); err != nil {
			return 0, err
		}
		log.Printf("[DRY-RUN] Would migrate %d guild rows", count)
		return count, nil
	}
	res, err := db.ExecContext(ctx, "UPDATE guild SET zone_id = ? WHERE zone_id = ?", dst, src)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ── Redis:缓存失效 ───────────────────────────────────────────

// invalidateGuildCaches 对每个搬过来的公会做 INCR generation + DEL 数据键。
// 分批 pipeline;单个公会失败不影响其他公会,但整体错误会上抛(缓存没清
// 干净 = 玩家看到旧 zone,属于必须知道的失败)。
func invalidateGuildCaches(ctx context.Context, rdb *redis.Client, guildIDs []uint64, dryRun bool) (int, error) {
	if rdb == nil || len(guildIDs) == 0 {
		return 0, nil
	}
	if dryRun {
		return len(guildIDs), nil
	}
	// 必须先 SCRIPT LOAD。Script.Run 在**普通调用**上会自己处理 NOSCRIPT 回退,
	// 但在 pipeline 里回退不可能发生:整批 EVALSHA 已经发出去了,回来的是
	// 一片 NOSCRIPT。先 Load 一次,后面每批都命中同一个 sha。
	if err := invalidateGuildVersionedCacheScript.Load(ctx, rdb).Err(); err != nil {
		return 0, fmt.Errorf("load guild cache invalidation script: %w", err)
	}
	done := 0
	for _, batch := range chunkUint64(guildIDs, 200) {
		pipe := rdb.Pipeline()
		cmds := make([]*redis.Cmd, 0, len(batch))
		for _, id := range batch {
			cmds = append(cmds, invalidateGuildVersionedCacheScript.Run(ctx, pipe,
				[]string{guildCacheGenerationKey(id), guildCacheKey(id)}))
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return done, fmt.Errorf("invalidate guild caches: %w", err)
		}
		for _, c := range cmds {
			if err := c.Err(); err != nil {
				return done, fmt.Errorf("invalidate guild cache: %w", err)
			}
			done++
		}
	}
	return done, nil
}

// ── Redis:榜单 ZSET ──────────────────────────────────────────

// readZoneRankMembers 读源区 ZSET 全量(成员 + 分数)。合并前先读一份进清单,
// 撤销时按它原样还原。
func readZoneRankMembers(ctx context.Context, rdb *redis.Client, zone uint32) ([]manifestRankMember, error) {
	key := guildZoneRankKey(zone)
	zs, err := rdb.ZRangeWithScores(ctx, key, 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("zrange %s: %w", key, err)
	}
	out := make([]manifestRankMember, 0, len(zs))
	for _, z := range zs {
		out = append(out, manifestRankMember{Member: fmt.Sprint(z.Member), Score: z.Score})
	}
	return out, nil
}

// mergeRankZSET 把 members 原子地并进目标区 ZSET 并删掉源区 ZSET。
//
// 原子性来自 TxPipelined(MULTI/EXEC):ZADD 与 DEL 在同一条事务里,
// 中途断连不会留下「源区没了、目标区也没有」的状态。
func mergeRankZSET(
	ctx context.Context,
	rdb *redis.Client,
	src, dst uint32,
	members []manifestRankMember,
	dryRun bool,
) (int, error) {
	srcKey, dstKey := guildZoneRankKey(src), guildZoneRankKey(dst)
	if len(members) == 0 {
		log.Printf("Redis: source %s is empty, nothing to merge", srcKey)
		// 源区 ZSET 可能是个空壳键(全成员被 ZREM 过);删掉它让
		// -verify-merged 的 EXISTS 断言能过。
		if !dryRun {
			if err := rdb.Del(ctx, srcKey).Err(); err != nil {
				return 0, fmt.Errorf("del %s: %w", srcKey, err)
			}
		}
		return 0, nil
	}
	if dryRun {
		log.Printf("[DRY-RUN] Would merge %d members %s → %s (single MULTI/EXEC)", len(members), srcKey, dstKey)
		return len(members), nil
	}
	zm := make([]redis.Z, 0, len(members))
	for _, m := range members {
		zm = append(zm, redis.Z{Score: m.Score, Member: m.Member})
	}
	if _, err := rdb.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.ZAdd(ctx, dstKey, zm...)
		pipe.Del(ctx, srcKey)
		return nil
	}); err != nil {
		return 0, fmt.Errorf("atomic rank merge %s → %s: %w", srcKey, dstKey, err)
	}
	return len(members), nil
}

// unmergeRankZSET 是撤销:把清单里的成员 ZADD 回源区、从目标区 ZREM 掉。
// 同样一条 MULTI/EXEC。只动清单里列出的成员,目标区原有成员一根汗毛不碰。
func unmergeRankZSET(
	ctx context.Context,
	rdb *redis.Client,
	src, dst uint32,
	members []manifestRankMember,
	dryRun bool,
) (int, error) {
	if len(members) == 0 {
		return 0, nil
	}
	srcKey, dstKey := guildZoneRankKey(src), guildZoneRankKey(dst)
	if dryRun {
		log.Printf("[DRY-RUN] Would restore %d members to %s and ZREM them from %s", len(members), srcKey, dstKey)
		return len(members), nil
	}
	zm := make([]redis.Z, 0, len(members))
	rem := make([]any, 0, len(members))
	for _, m := range members {
		zm = append(zm, redis.Z{Score: m.Score, Member: m.Member})
		rem = append(rem, m.Member)
	}
	if _, err := rdb.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.ZAdd(ctx, srcKey, zm...)
		pipe.ZRem(ctx, dstKey, rem...)
		return nil
	}); err != nil {
		return 0, fmt.Errorf("atomic rank unmerge %s ← %s: %w", srcKey, dstKey, err)
	}
	return len(members), nil
}
