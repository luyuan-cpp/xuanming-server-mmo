package main

// 公会侧的三件事:zone_id 改写、榜单 ZSET 合并、缓存失效。
//
// 三点与旧实现的差别,每一条都是修一个真 bug:
//
//  1. 重名冲突路径删掉了。proto/guild/guild_db.proto 的 OptionUniqueKey="name_norm"
//     (生成 `uk_guild`)是**全局**唯一(不带 zone_id),所以
//     「源区有个公会叫 X,目标区也有个叫 X」在库层面根本不可能存在,
//     旧的 checkNameConflicts JOIN 永远返回空。它的害处不在于慢,而在于
//     `UPDATE ... AND guild_id NOT IN (...)` 这条**跳过冲突继续写**的分支
//     给人一种「冲突会被优雅处理」的错觉。真要哪天 uk_guild 改成
//     (zone_id, name_norm),正确做法是**中止**而不是跳过 —— 跳过会把源区公会
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
//
//  4. zone_id 改写只按清单逐条主键点更新(2026-09-21 死锁审计 #17)。旧的整区
//     `UPDATE guild SET zone_id = dst WHERE zone_id = src` 沿 idx_guild_0(zone_id)先锁二级项、
//     再回表锁主键,与 guild 服 DisbandGuild「主键 FOR UPDATE → DELETE 删二级项」反序成环;
//     guild 是全局服务、合服期间不停,DisbandGuild 的合服闸门只在客户端路径上查。
//     现在锁序与在线写者同为「主键 → 二级」,改写后再复查源区计数(见 migrateGuildZone)。
//     2026-09-28 起每个 id 一个显式短事务(SELECT … FOR UPDATE → UPDATE → COMMIT,A15),理由见
//     merge_run.go 的 rewriteZoneByPrimaryKey。
//
//  5. 榜单合并在维护锁内重读源榜,以重读结果为准(2026-09-28,A8,见 mergeGuildRank)。

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"slices"
	"strconv"

	"github.com/redis/go-redis/v9"
)

const (
	// defaultGuildSchema 镜像 go/guild/internal/data.DatabaseName。guild 服的 config.Validate
	// 强制 MySQL DSN 的库名等于它,所以生产上只有这一个合法值;-guild-schema 只为集成测试的
	// 一次性库留口子。merge_zone 是独立 module,不能 import go/guild,字面量由 merge_unit_test.go 守住。
	defaultGuildSchema = "mmorpg_guild"
	// guildTable / guildMemberTable 与 proto/guild/guild_db.proto 的 OptionTableName 一致。
	guildTable       = "guild"
	guildMemberTable = "guild_member"
	// guildPKColumn / guildZoneColumn:guild 表的主键(OptionPrimaryKey)与本步骤改写的唯一一列。
	guildPKColumn   = "guild_id"
	guildZoneColumn = "zone_id"
	// guildNameNormColumn 是帮名唯一键所在列(uk_guild)。重名探测比它,不比展示名:
	// 规范化(NFKC → TrimSpace → 小写)在 go/guild 侧完成,库里存的就是规范化结果。
	guildNameNormColumn = "name_norm"
)

// validateGuildSchemaName 在任何 SQL 之前验库名形状(形状定义见 player_rows.go 的 schemaNamePattern)。
func validateGuildSchemaName(schema string) error {
	if !schemaNamePattern.MatchString(schema) {
		return fmt.Errorf("-guild-schema %q is not a plain identifier ([A-Za-z0-9_], 1-64 chars)", schema)
	}
	return nil
}

func guildQualified(schema, table string) string { return schema + "." + table }

// assertGuildTablesReady 证明 schema 下的帮会两张表存在且是本工具认识的形状。
// 库不在、表不在(guild 从没跑过 -migrate)、列不在(结构不是本工具认识的)都返回错误,
// 调用方拒绝继续 —— 「查不到」与「没有公会」在这里分不开(-mysql-dsn 指错实例也是同一症状)。
func assertGuildTablesReady(ctx context.Context, db *sql.DB, schema string) error {
	if err := validateGuildSchemaName(schema); err != nil {
		return err
	}
	if err := assertSchemaExists(ctx, db, schema); err != nil {
		return err
	}
	if err := assertGuildTableShape(ctx, db, schema, guildTable, guildPKColumn, guildZoneColumn, "name", guildNameNormColumn); err != nil {
		return err
	}
	return assertGuildTableShape(ctx, db, schema, guildMemberTable, "guild_id")
}

// assertGuildTableShape:表在(列表非空)且带着本工具要读写的列。列表为空 = 表不存在,
// 单独报出来 —— 「表没建」和「表建了但结构不对」要给运维不同的下一步动作。
func assertGuildTableShape(ctx context.Context, db *sql.DB, schema, table string, wantCols ...string) error {
	cols, err := tableColumns(ctx, db, schema, table)
	if err != nil {
		return err
	}
	if len(cols) == 0 {
		return fmt.Errorf("%s does not exist — the guild service has never run its schema migration here",
			guildQualified(schema, table))
	}
	for _, want := range wantCols {
		if !slices.Contains(cols, want) {
			return fmt.Errorf("%s has no %s column (columns: %v) — not the guild shape this tool rewrites",
				guildQualified(schema, table), want, cols)
		}
	}
	return nil
}

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
func collectGuildIDsInZone(ctx context.Context, db *sql.DB, schema string, zone uint32) ([]uint64, error) {
	table := guildQualified(schema, guildTable)
	rows, err := db.QueryContext(ctx,
		"SELECT guild_id FROM "+table+" WHERE zone_id = ? ORDER BY guild_id", zone)
	if err != nil {
		return nil, fmt.Errorf("list %s in zone %d: %w", table, zone, err)
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
// 今天它恒为真(uk_guild 按 name_norm 全局唯一),留着是**契约断言**:如果哪天有人把
// uk_guild 改成 (zone_id, name_norm),这条查询会立刻开始命中,合服会停在这里
// 而不是悄悄漏掉几个公会。这就是「让冲突在任何写之前中止」的落点。
//
// 比的是 name_norm 而不是展示名:规范化在 go/guild 侧完成("青云门 " 与 "青云门"、
// "ABC" 与 "abc" 都是同一个 name_norm),按展示名比会漏掉这些等价名。
func assertNoGuildNameCollision(ctx context.Context, db *sql.DB, schema string, src, dst uint32) error {
	table := guildQualified(schema, guildTable)
	rows, err := db.QueryContext(ctx,
		"SELECT s.guild_id, d.guild_id, s.name"+
			" FROM "+table+" s JOIN "+table+" d ON s.name_norm = d.name_norm AND d.zone_id = ?"+
			" WHERE s.zone_id = ?", dst, src)
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
			"guild.name_norm is globally UNIQUE today (uk_guild), so this should be impossible — the schema changed under us. "+
			"Rename or disband the colliding guilds first; refusing to write anything", collisions, src, dst)
	}
	return nil
}

// migrateGuildZone 把清单 guildIDs 里当前 zone_id = src 的公会改成 dst,返回受影响行数;
// dry-run 返回「会被改」的行数,不写。
//
// **只动清单里的 id**,且逐条主键点更新 `UPDATE guild SET zone_id = dst WHERE guild_id = ? AND zone_id = src`
// (锁序论证见 rewriteZoneByPrimaryKey):取锁顺序与 guild 服 DisbandGuild 同为「主键 → 二级」,
// 撞上正在解散的公会只会等它提交,等到的是已删除的行就影响 0 行。
// 清单之外的源区公会(清单落盘之后经内部 / GM 路径建的)原地不动,由调用方改写后用 countGuildsInZone
// 复查并拒绝继续(见 merge_run.go 步骤 3)。重跑幂等:已经改过的行不再满足 zone_id = src。
func migrateGuildZone(ctx context.Context, db *sql.DB, schema string, guildIDs []uint64, src, dst uint32, dryRun bool) (int64, error) {
	table := guildQualified(schema, guildTable)
	if dryRun {
		n, err := countGuildsInZoneAmong(ctx, db, schema, guildIDs, src)
		if err != nil {
			return 0, err
		}
		log.Printf("[DRY-RUN] Would migrate zone_id %d → %d on %d of %d manifest guilds in %s",
			src, dst, n, len(guildIDs), table)
		return n, nil
	}
	return rewriteZoneByPrimaryKey(ctx, db, table, guildPKColumn, guildZoneColumn, guildIDs, src, dst)
}

// restoreGuildZone 是撤销:把清单 guildIDs 里当前 zone_id = dst 的公会改回 src,锁序与 migrateGuildZone
// 相同(逐条主键点更新,不用 `zone_id = ? AND guild_id IN (...)` 这种可能被规划成二级索引 range 的写法)。
// 不在清单里的(目标区原住民)与已经不在 dst 的(去了第三个 zone,不是这次合服造成的)一律不动。
// dry-run 数出「会被改回」的行数但不写。
func restoreGuildZone(ctx context.Context, db *sql.DB, schema string, guildIDs []uint64, src, dst uint32, dryRun bool) (int64, error) {
	table := guildQualified(schema, guildTable)
	if dryRun {
		n, err := countGuildsInZoneAmong(ctx, db, schema, guildIDs, dst)
		if err != nil {
			return 0, err
		}
		log.Printf("[DRY-RUN] Would restore zone_id=%d on %d of %d manifest guilds in %s", src, n, len(guildIDs), table)
		return n, nil
	}
	return rewriteZoneByPrimaryKey(ctx, db, table, guildPKColumn, guildZoneColumn, guildIDs, dst, src)
}

// countGuildsInZone 数 zone_id = zone 的公会。步骤 3 改写后的复查用;非锁定读(RC 下每条语句一个新快照)。
func countGuildsInZone(ctx context.Context, db *sql.DB, schema string, zone uint32) (int64, error) {
	table := guildQualified(schema, guildTable)
	var n int64
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM "+table+" WHERE zone_id = ?", zone).Scan(&n); err != nil {
		return 0, fmt.Errorf("count %s with zone_id=%d: %w", table, zone, err)
	}
	return n, nil
}

// countGuildsInZoneAmong 数 ids 里当前 zone_id = zone 的公会(dry-run 用)。非锁定读,
// 按 playerRowsBatchSize 分批 IN,不存在锁序问题。
func countGuildsInZoneAmong(ctx context.Context, db *sql.DB, schema string, ids []uint64, zone uint32) (int64, error) {
	table := guildQualified(schema, guildTable)
	var total int64
	for _, batch := range chunkUint64(ids, playerRowsBatchSize) {
		var n int64
		if err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM "+table+" WHERE zone_id = ? AND guild_id IN ("+inListLiteral(batch)+")", zone).Scan(&n); err != nil {
			return total, fmt.Errorf("count manifest guilds in %s with zone_id=%d: %w", table, zone, err)
		}
		total += n
	}
	return total, nil
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

// chooseRankWrite 是步骤 4 的纯判定:锁内重读的源榜决定写什么,清单快照只决定撤销依据。
//
//   - 重读非空:以重读为准,写它、记它(清单阶段的快照分数旧、缺后来进榜的成员)。
//   - 重读为空:一律不写。ZADD 与 DEL 在同一条 MULTI/EXEC 里,「源榜为空」只可能是
//     (a) 续跑,上一次的 MULTI/EXEC 已把它并进目标榜并删掉;
//     (b) guild 服合法清空(公会全部解散,或步骤 3 之后 RebuildRanks 按 MySQL 重建)。
//     两种情况下目标榜都已是 guild 服维护的最新状态,再 ZADD 快照只会回退分数、复活已解散的公会。
//     撤销依据:snapshotMayBeWritten(快照是上一次写入之前落盘的那一份,或旧版工具写的清单 —— 旧版步骤 4
//     写的正是它)时沿用快照,撤销按它把成员还回源区;快照只是清单阶段读的、从没交给过任何一次写时记为空,
//     撤销不再把这些(可能已解散的)成员 ZADD 回源区。
//
// 返回要写的集合、要记进清单(撤销依据)的集合,以及是否走了「源榜已不在」这一支。
func chooseRankWrite(reread, snapshot []manifestRankMember, snapshotMayBeWritten bool) (toWrite, toRecord []manifestRankMember, sourceGone bool) {
	if len(reread) > 0 {
		return reread, reread, false
	}
	if snapshotMayBeWritten {
		return nil, snapshot, true
	}
	return nil, nil, true
}

// mergeGuildRank 是合服步骤 4(2026-09-28,player-storage-placement.md §12 A8):拿 guild_rank:maintenance_lock
// → 锁内重读源榜 → 以重读结果为准 MULTI/EXEC 并进目标榜并删源榜。
//
// 为什么重读:清单阶段的快照(m.RankMembers)到这里隔着步骤 1~3b。guild 是全局服务、合服期间不停,分数
// 更新与榜单重建 / 回填(后者与本步骤同一把维护锁)都可能改过源榜。照旧快照 ZADD 会把分数写回旧值,
// 快照之后才进源榜的成员则被随后的 DEL 一并删掉 —— 它们从哪张榜上都消失了,而且撤销也找不回来。
// 只有锁内读到的,才与 MULTI/EXEC 之间没有别的维护写者。源榜已不在时不写(chooseRankWrite)。
//
// snapshotMayBeWritten:snapshot 是否可能已经被写进过目标榜(见 chooseRankWrite),只影响撤销依据。
// recordBeforeWrite 在 MULTI/EXEC 之前被调用(dry-run 不调用),调用方借它把「撤销依据」先落进清单
// (清单先于写):MULTI/EXEC 成功之后、步骤标记之前崩溃,续跑读到的与撤销按它 ZREM 的都是真正并进去的
// 那一份。它返回错误时一个字节都不写。返回记进清单的集合,以及是否走了「源榜已不在」这一支。
func mergeGuildRank(ctx context.Context, rdb *redis.Client, src, dst uint32, snapshot []manifestRankMember,
	snapshotMayBeWritten, dryRun bool, recordBeforeWrite func([]manifestRankMember) error) ([]manifestRankMember, bool, error) {
	release, err := acquireGuildRankLock(ctx, rdb, dryRun)
	if err != nil {
		return nil, false, fmt.Errorf("guild rank lock: %w", err)
	}
	defer release()
	reread, err := readZoneRankMembers(ctx, rdb, src)
	if err != nil {
		return nil, false, fmt.Errorf("re-read the source rank under %s: %w", guildRankLockKey, err)
	}
	toWrite, toRecord, sourceGone := chooseRankWrite(reread, snapshot, snapshotMayBeWritten)
	if !dryRun && recordBeforeWrite != nil {
		if err := recordBeforeWrite(toRecord); err != nil {
			return nil, sourceGone, fmt.Errorf("record the rank members in the manifest before writing: %w", err)
		}
	}
	// toWrite 为空时 mergeRankZSET 只幂等地 DEL 源键空壳,目标榜不动。
	if _, err := mergeRankZSET(ctx, rdb, src, dst, toWrite, dryRun); err != nil {
		return nil, sourceGone, err
	}
	return toRecord, sourceGone, nil
}

// mergeRankZSET 把 members 原子地并进目标区 ZSET 并删掉源区 ZSET。
//
// 原子性来自 TxPipelined(MULTI/EXEC):ZADD 与 DEL 在同一条事务里,
// 中途断连不会留下「源区没了、目标区也没有」的状态。
//
// 目标榜用 ZADD NX:目标榜里已有的源区公会成员只可能是步骤 3 把 zone_id 改成 dst 之后 guild 服写进去的
// (分数更新 / RebuildRanks 按 MySQL 重建),都比源榜里的新;公会 id 全局唯一,不会与目标区原住民撞名。
// 覆盖它们 = 把分数回退到源榜的旧值。
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
		pipe.ZAddNX(ctx, dstKey, zm...)
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
