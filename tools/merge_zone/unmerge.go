package main

// -mode unmerge:按清单**逐对象**撤销一次合服。
//
// 这不是「从备份还原」的替代品,而是它的补充。备份还原会把目标区在合服窗口之后发生的一切一起抹掉;unmerge 只碰清单里列出的
// 公会 / 商品 / ZSET 成员,目标区原住民一根汗毛不碰。适用面:合服刚跑完、还没开服,发现搞错了对象。
//
// 玩家行与路由行合服时就没动过:home_zone 还是源区,删掉映射他们就回到源区。合服之后新建的号按自己的 home_zone 留在各自的区,
// 不需要处理。
//
//	P  前置门禁(目标区口径):目标区无活节点;home_zone=src 的玩家无会话、无活队伍
//	X  前置核对:目标区之后又被合走即拒绝(必须逆序撤销);源区的映射不是这份清单写的即拒绝
//	4' guild_rank ZSET 成员 ZADD 回 src、从 dst ZREM
//	3b' trade_listing.market_zone 改回 src(只改清单里、当前确实在 dst 的 listing_id)
//	3' guild.zone_id 改回 src(只改清单里的 guild_id)+ 缓存失效
//	1' 删 zone_merged_into,再按完整值删 merge:merged_into:{src}  ← 最后做:业务数据写回之前玩家仍解析到目标区,数据与归属一致
//
// 撤销不记步骤进度:每一步都按对象当前值过滤、幂等,失败非零退出,可整体重跑。

import (
	"context"
	"database/sql"
	"fmt"
	"log"

	"github.com/redis/go-redis/v9"
)

func runUnmerge(o options) {
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

	// dry-run 预览(<path>.dryrun.json)一律拒读:它记的是彩排那一刻的名单,不是任何一次写入。
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

	log.Printf("=== UNMERGE zone %d → %d (reversing run_id=%s started %s; dry-run=%v) ===", src, dst, m.RunID, m.StartedAt, o.dryRun)
	log.Printf("    scope: %d guilds, %d rank members, %d trade listings", len(m.GuildIDs), len(m.RankMembers), len(m.TradeListingIDs))
	log.Printf("    schemas: trade=%s guild=%s (restored only where the zone column is still %d)", o.tradeSchema, o.guildSchema, dst)
	log.Printf("    NOTHING outside this list is touched — target-zone natives are safe by construction.")

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
	// preflight P2(目标区口径)要 etcd 注册表的正面证据,见 scene_registry.go。写入之前校验参数。
	sceneNodes, rerr := newEtcdGatewayRegistry(o.etcdEndpoints)
	if rerr != nil {
		log.Fatalf("-etcd-endpoints is required: preflight P2 needs positive evidence that zone %d has no registered scene node: %v", dst, rerr)
	}

	// 清单里有商品 / 公会 id = 当初真的搬过,库 / 表必须在。放在任何写之前:-trade-schema 指错时,不能等榜单已经改回之后才失败。
	if len(m.TradeListingIDs) > 0 {
		if err := assertTradeListingReady(ctx, db, o.tradeSchema); err != nil {
			log.Fatalf("preflight: manifest lists %d trade listings to restore, but %v", len(m.TradeListingIDs), err)
		}
	}
	if len(m.GuildIDs) > 0 {
		if err := assertGuildTablesReady(ctx, db, o.guildSchema); err != nil {
			log.Fatalf("preflight: manifest lists %d guilds to restore, but %v", len(m.GuildIDs), err)
		}
	}

	rb, err := openRunRouteBackend(ctx, routeEntryOptionsOf(o), mapRdb)
	if err != nil {
		log.Fatalf("route DB: %v", err)
	}
	if maintenanceFromContext(ctx) == nil {
		defer rb.Close()
	}
	gate := writeGate(func() error {
		if o.dryRun {
			return nil
		}
		if err := checkMaintenanceTool(ctx); err != nil {
			return err
		}
		return freeze.check(ctx, freezeDB)
	})

	// P 前置门禁(目标区口径):与合服共用 runPreflight,只是「必须已停干净的那个 zone」换成目标区:
	// 此刻源区玩家解析到目标区,目标区还有节点在跑、玩家还在线就撤销,写回的就是一份他们正在改的数据。
	players, err := collectPlayerIDsResolvingThrough(ctx, rb.db, src)
	if err != nil {
		log.Fatalf("list players resolving through zone %d from player_route: %v", src, err)
	}
	if err := runPreflight(ctx, preflightDeps{
		teamAuthority: teamAuthority,
		sceneRdb:      sceneRdb,
		sceneNodes:    sceneNodes,
		sharedRdb:     sharedRdb,
	}, preflightParams{
		readOnly:  o.dryRun,
		zone:      dst,
		scope:     "target",
		playerIDs: players,
	}); err != nil {
		log.Fatalf("unmerge refused: %v", err)
	}
	// X 映射前置核对。
	if err := checkUnmergeAllowed(ctx, rb, m); err != nil {
		log.Fatalf("unmerge refused: %v", err)
	}

	// 撤销不记步骤进度,先在清单上记一笔:半途失败之后这份清单只能接着撤销,不能再拿去续跑合服。
	if !o.dryRun {
		m.UnmergeStarted = true
		if err := saveManifest(o.manifestPath, m); err != nil {
			log.Fatalf("save manifest: %v", err)
		}
	}
	ops := &liveUnmergeSteps{o: o, src: src, dst: dst, m: m, db: db, guildRdb: guildRdb, rb: rb, gate: gate}
	if err := runUnmergeSteps(ctx, ops, gate); err != nil {
		log.Fatalf("%v — every step is idempotent: re-run the same command to finish the unmerge", err)
	}
	log.Printf("=== Unmerge done (guilds=%d rank=%d trade_listings=%d; players resolving through zone %d: %d) ===",
		len(m.GuildIDs), len(m.RankMembers), len(m.TradeListingIDs), src, len(players))
}

// unmergeStepOps 是撤销每一步实际做的写。生产实现 liveUnmergeSteps;单测注入记录顺序的假实现。
type unmergeStepOps interface {
	restoreRank(ctx context.Context) error
	restoreTrade(ctx context.Context) error
	restoreGuilds(ctx context.Context) error
	clearMergedInto(ctx context.Context) error
}

// runUnmergeSteps 按撤销顺序跑:业务数据全部写回之后才删映射。中途失败时映射还在,玩家仍解析到目标区,重跑补齐。
func runUnmergeSteps(ctx context.Context, ops unmergeStepOps, gate writeGate) error {
	for _, s := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"guild_rank", ops.restoreRank},
		{"trade_mysql", ops.restoreTrade},
		{"guild_mysql", ops.restoreGuilds},
		{"merged_into", ops.clearMergedInto},
	} {
		if err := gate.check(); err != nil {
			return fmt.Errorf("unmerge step %s: the write gate failed before it started: %w", s.name, err)
		}
		if err := s.run(ctx); err != nil {
			return fmt.Errorf("unmerge step %s failed: %w", s.name, err)
		}
	}
	return nil
}

// liveUnmergeSteps 是 unmergeStepOps 的生产实现。
type liveUnmergeSteps struct {
	o        options
	src, dst uint32
	m        *mergeManifest
	db       *sql.DB
	guildRdb *redis.Client
	rb       *routeBackend
	gate     writeGate
}

func (s *liveUnmergeSteps) restoreRank(ctx context.Context) error {
	if len(s.m.RankMembers) == 0 {
		return nil
	}
	release, err := acquireGuildRankLock(ctx, s.guildRdb, s.o.dryRun)
	if err != nil {
		return fmt.Errorf("guild rank lock: %w", err)
	}
	defer release()
	n, err := unmergeRankZSET(ctx, s.guildRdb, s.src, s.dst, s.m.RankMembers, s.o.dryRun)
	if err != nil {
		return err
	}
	log.Printf("Redis rank: %d members restored to guild_rank:zone:%d", n, s.src)
	return nil
}

// restoreTrade 只针对清单里的 listing_id 且当前确实是 dst 的。清单里没有 id(合服时 -skip-trade-mysql)就什么都不做。
func (s *liveUnmergeSteps) restoreTrade(ctx context.Context) error {
	if len(s.m.TradeListingIDs) == 0 {
		return nil
	}
	n, err := restoreTradeMarketZone(ctx, s.db, s.o.tradeSchema, s.m.TradeListingIDs, s.src, s.dst, s.o.dryRun, s.gate)
	if err != nil {
		return err
	}
	log.Printf("SQL: %d of %d manifest trade listings %s to market_zone=%d", n, len(s.m.TradeListingIDs), verbRestore(s.o.dryRun), s.src)
	return nil
}

// restoreGuilds 只针对清单里的 guild_id,逐条主键点更新(锁序见 restoreGuildZone),改完失效缓存。
// 失效失败时重跑按 `zone_id = dst` 幂等(已改回的影响 0 行),失效对全清单再做一遍。
func (s *liveUnmergeSteps) restoreGuilds(ctx context.Context) error {
	if len(s.m.GuildIDs) == 0 {
		return nil
	}
	n, err := restoreGuildZone(ctx, s.db, s.o.guildSchema, s.m.GuildIDs, s.src, s.dst, s.o.dryRun, s.gate)
	if err != nil {
		return err
	}
	log.Printf("SQL: %d of %d manifest guilds %s to zone_id=%d", n, len(s.m.GuildIDs), verbRestore(s.o.dryRun), s.src)
	inv, err := invalidateGuildCaches(ctx, s.guildRdb, s.m.GuildIDs, s.o.dryRun)
	if err != nil {
		return fmt.Errorf("zone_id restored on %d rows, but invalidating guild:v2 caches failed: %w", n, err)
	}
	log.Printf("Guild cache: %d guild:v2 entries invalidated", inv)
	return nil
}

func (s *liveUnmergeSteps) clearMergedInto(ctx context.Context) error {
	if _, err := stepClearMergedInto(ctx, s.rb, s.src, s.dst, s.m.RunID, s.o.dryRun); err != nil {
		return fmt.Errorf("clear zone_merged_into(%d): %w", s.src, err)
	}
	return nil
}

func verbRestore(dry bool) string {
	if dry {
		return "would be restored"
	}
	return "restored"
}
