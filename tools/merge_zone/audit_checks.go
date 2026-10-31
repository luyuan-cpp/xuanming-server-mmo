package main

// 审计的两组检查:
//
//  A. 合服**前**的真门禁(pre-merge)。扫描失败一律 Severity=block;句柄缺失让整个审计以 exit 2 结束(见 infraAudit)。
//  B. 合服**后**的验证(-verify-merged)。四行 block 级断言,行名与 mergeRecoveryRowNames、go/db barrierrestore 同一组。

import (
	"context"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// auditNoteInfraPrefix 由 auditor 设置,表示「这不是数据问题,是我们根本没查成」。
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

// auditOnlinePresence 查源区玩家还在不在线。证据面只有一个:player:session:{pid} —— player_locator 写,**DB 0**。
// 非零 = zone-down 没做完 / 还有进程在 ACK,block。扫描失败也是 block(而且是 INFRA 级):查不到就不能说「没人在线」。
func auditOnlinePresence(ctx context.Context, cfg auditConfig) ResourceAudit {
	r := ResourceAudit{Name: "online_presence", UniqueScope: "per_zone"}
	routes, err := cfg.routeReads()
	if err != nil {
		return infraAudit(r.Name, "%v — cannot enumerate source-zone players", err)
	}
	pids, err := collectPlayerIDsResolvingThrough(ctx, routes, cfg.src)
	if err != nil {
		return infraAudit(r.Name, "%v", err)
	}
	r.SourceCount = int64(len(pids))

	if cfg.sharedRDB == nil {
		return infraAudit(r.Name, "no shared (DB 0) Redis handle — cannot check player:session:{pid}")
	}
	sessions, err := countExistingKeys(ctx, cfg.sharedRDB, pids, "player:session:")
	if err != nil {
		return infraAudit(r.Name, "player:session scan failed: %v", err)
	}

	r.TargetCount = int64(sessions)
	r.ConflictCount = int64(sessions)
	if sessions > 0 {
		r.Severity = "block"
		r.Notes = fmt.Sprintf("%d players still have player:session (DB %d). "+
			"Investigate — zone-down is incomplete.", sessions, cfg.sharedDBIndex)
		return r
	}
	r.Severity = "info"
	r.Notes = fmt.Sprintf("0 player:session (DB %d) for %d source players", cfg.sharedDBIndex, len(pids))
	return r
}

// auditSourceZoneNodes 查源区场景节点:etcd 注册表(正面证据)与负载集。与 merge 的 preflight P2 同一判据
// (assertZoneDown),放进审计是为了让 T-1 彩排就能发现「zone-down 其实没做干净」。
func auditSourceZoneNodes(ctx context.Context, cfg auditConfig) ResourceAudit {
	r := ResourceAudit{Name: "source_scene_nodes", UniqueScope: "per_zone"}
	if cfg.sceneRDB == nil {
		return infraAudit(r.Name, "no scene_manager Redis handle (-scene-redis-addr/-scene-redis-db)")
	}
	if cfg.sceneNodes == nil {
		return infraAudit(r.Name, "no etcd scene-node registry (-etcd-endpoints): an empty load set alone does not prove the zone is down")
	}
	registered, err := cfg.sceneNodes.SceneNodesInZone(ctx, cfg.src)
	if err != nil {
		return infraAudit(r.Name, "read etcd scene-node registry: %v", err)
	}
	if registered > 0 {
		r.SourceCount = int64(registered)
		r.Severity = "block"
		r.Notes = fmt.Sprintf("etcd still registers %d scene node(s) of zone %d — zone-down is not complete", registered, cfg.src)
		return r
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
	r.Notes = fmt.Sprintf("no scene node of zone %d registered in etcd; %s is empty", cfg.src, key)
	return r
}

// ── B. 合服后验证(-verify-merged)─────────────────────────────

// verifyMergedInto:这次合服走完了,映射还在、是这份清单写的,Redis 标记与库逐字节一致。
//
// 玩家行不搬,home_zone=src 的人数只是统计(合服之后也不会变成 0),进 SourceCount / TargetCount 供人核对。
// 镜像缺失时处置是等对账 leader 把它补回来再验一遍(它按库只增不改),不要手工写。
func verifyMergedInto(ctx context.Context, cfg auditConfig) ResourceAudit {
	r := ResourceAudit{Name: "verify:merged_into", UniqueScope: "global"}
	m, err := cfg.verifyManifest()
	if err != nil {
		return infraAudit(r.Name, "%v", err)
	}
	routes, err := cfg.routeReads()
	if err != nil {
		return infraAudit(r.Name, "%v", err)
	}
	if cfg.mappingDB == nil {
		return infraAudit(r.Name, "no mapping Redis handle")
	}
	srcPlayers, err := collectPlayerIDsWithHomeZoneDB(ctx, routes, cfg.src)
	if err != nil {
		return infraAudit(r.Name, "%v", err)
	}
	dstPlayers, err := collectPlayerIDsWithHomeZoneDB(ctx, routes, cfg.dst)
	if err != nil {
		return infraAudit(r.Name, "%v", err)
	}
	r.SourceCount, r.TargetCount = int64(len(srcPlayers)), int64(len(dstPlayers))

	if !m.stepDone(stepMergeDone) {
		r.Severity = "block"
		r.Notes = fmt.Sprintf("the manifest does not record step %s: this merge did not finish, re-run it with the same -manifest-path", stepMergeDone)
		return r
	}
	dst, runID, found, err := routes.ReadMergedInto(ctx, cfg.src)
	if err != nil {
		return infraAudit(r.Name, "%v", err)
	}
	switch {
	case !found:
		r.Severity = "block"
		r.Notes = fmt.Sprintf("zone_merged_into has no row for zone %d: players of zone %d do not resolve to zone %d", cfg.src, cfg.src, cfg.dst)
		return r
	case dst != cfg.dst:
		r.Severity = "block"
		r.Notes = fmt.Sprintf("zone_merged_into(%d) maps to zone %d, not %d", cfg.src, dst, cfg.dst)
		return r
	case runID != m.RunID:
		r.Severity = "block"
		r.Notes = fmt.Sprintf("zone_merged_into(%d) was written by run_id %s, not by this manifest's run_id %s — is this the manifest of "+
			"the merge being verified?", cfg.src, runID, m.RunID)
		return r
	}
	want, err := encodeRouteMergedIntoValue(cfg.dst, m.RunID)
	if err != nil {
		return infraAudit(r.Name, "%v", err)
	}
	key := mergedIntoKey(cfg.src)
	raw, err := cfg.mappingDB.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		r.Severity = "block"
		r.Notes = fmt.Sprintf("%s is missing while zone_merged_into(%d) is committed — wait for the data_service reconcile leader to add it "+
			"(or re-run the merge step), then verify again", key, cfg.src)
		return r
	}
	if err != nil {
		return infraAudit(r.Name, "get %s: %v", key, err)
	}
	if raw != want {
		r.Severity = "block"
		r.Notes = fmt.Sprintf("%s = %q differs from zone_merged_into(%d) %q", key, raw, cfg.src, want)
		return r
	}
	r.Severity = "info"
	r.Notes = fmt.Sprintf("zone %d resolves to zone %d (run_id %s); %s matches the DB; %d players keep home_zone=%d",
		cfg.src, cfg.dst, runID, key, len(srcPlayers), cfg.src)
	return r
}

// verifyGuildZoneDrained: guild WHERE zone_id=src 必须是 0。
func verifyGuildZoneDrained(ctx context.Context, cfg auditConfig) ResourceAudit {
	r := ResourceAudit{Name: "verify:guild_zone", UniqueScope: "global"}
	// 与合服 P1 同一口径:-skip-guild-mysql 与 -skip-guild-rank 同时给出 = 这里没有部署帮会服务,合服没有改写
	// guild.zone_id。报 SKIPPED(warn),不伪装成通过,也不因为查不到表而恒报 INFRA(exit 2)。
	// 只给其中一个开关时 P1 照样要求帮会表存在,这里照常断言(源区残留即 block,是正确结论)。
	if cfg.skipGuild {
		r.Severity = "warn"
		r.Notes = "SKIPPED (-skip-guild-mysql -skip-guild-rank): the guild service is declared absent here — " +
			"guild.zone_id was not rewritten by this merge and is NOT VERIFIED"
		return r
	}
	if cfg.db == nil {
		return infraAudit(r.Name, "no MySQL handle")
	}
	if err := validateGuildSchemaName(cfg.guildSchema); err != nil {
		return infraAudit(r.Name, "%v", err)
	}
	guildTableName := guildQualified(cfg.guildSchema, guildTable)
	if err := cfg.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM "+guildTableName+" WHERE zone_id = ?", cfg.src).Scan(&r.SourceCount); err != nil {
		return infraAudit(r.Name, "count %s zone_id=%d: %v", guildTableName, cfg.src, err)
	}
	if err := cfg.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM "+guildTableName+" WHERE zone_id = ?", cfg.dst).Scan(&r.TargetCount); err != nil {
		return infraAudit(r.Name, "count %s zone_id=%d: %v", guildTableName, cfg.dst, err)
	}
	if r.SourceCount != 0 {
		r.Severity = "block"
		r.Notes = fmt.Sprintf("%d %s rows still carry zone_id=%d", r.SourceCount, guildTableName, cfg.src)
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
	// 口径同 verifyGuildZoneDrained:两个帮会跳过开关都给 = 帮会服务不存在,榜单既没合并也无从核对。
	if cfg.skipGuild {
		r.Severity = "warn"
		r.Notes = "SKIPPED (-skip-guild-mysql -skip-guild-rank): the guild service is declared absent here — " +
			"the guild_rank ZSETs were not merged by this merge and are NOT VERIFIED"
		return r
	}
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
	if err := validateGuildSchemaName(cfg.guildSchema); err != nil {
		return infraAudit(r.Name, "%v", err)
	}
	var guildRows int64
	if err := cfg.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM "+guildQualified(cfg.guildSchema, guildTable)+" WHERE zone_id = ?",
		cfg.dst).Scan(&guildRows); err != nil {
		return infraAudit(r.Name, "count %s zone_id=%d: %v", guildQualified(cfg.guildSchema, guildTable), cfg.dst, err)
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
