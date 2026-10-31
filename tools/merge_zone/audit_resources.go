package main

// -mode audit:合服的只读审计。
//
//   - 合服前(缺省):源区是否停干净、会被业务合并牵动的几张表的体量与数据质量,写进维护单给人看;
//   - 合服后(-verify-merged):四行 block 级断言,verify:merged_into、verify:guild_zone、verify:trade_market_zone、verify:guild_rank。
//
// 不修复任何东西;main.go 的 merge / unmerge 是仅有的写者。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// ResourceAudit is a single per-resource report row.
//
// Conceptually it answers: "for resource X, what does merging zone src→dst
// look like?" Fields are deliberately denormalized; the audit prints them
// as a markdown table so ops can eyeball before the maintenance window.
type ResourceAudit struct {
	Name          string // resource short name (e.g. "friend", "verify:guild_zone")
	SourceCount   int64  // rows / keys in source zone
	TargetCount   int64  // rows / keys in target zone
	UniqueScope   string // "global" / "per_zone" / "none" — affects merge safety
	ConflictCount int64  // post-merge collision count
	Severity      string // "info" / "warn" / "block"
	Notes         string // free-form observations + remediation hint
}

// routeAuditReader 是审计读路由库的接缝(生产 *mergeRouteStore;单测注入假实现)。全部是事务外一致读、不加锁。
type routeAuditReader interface {
	routeHomeScanner
	ReadMergedInto(ctx context.Context, src uint32) (dst uint32, runID string, found bool, err error)
}

// auditConfig bundles inputs every per-resource auditor needs.
//
// We pass this around instead of a global so the auditor can be unit-tested
// with mocked DB / Redis. Keep this struct small — anything zone-wide goes
// into a closure if needed.
// Redis DB 分布(实地核对 2026-09-08,来源见每行括注)。审计跨了几个句柄,把它们混成一个句柄
// 正是旧审计恒不告警的根因,所以这里一人一格。
//
//	mapping  DB 0   merge:merged_into:{src} / mapping:route:watermark
//	                (go/data_service/etc/data_service.yaml MappingRedis)
//	guild    DB 0   guild_rank:zone:{z} / guild:v2:{id} / guild_rank:maintenance_lock
//	                (go/guild/etc/guild.yaml RedisClient;2026-10-23 由 DB 2 改为 0)
//	shared   DB 0   player:session:{pid}(go/login, go/player_locator 都是 DB 0)
type auditConfig struct {
	db        *sql.DB       // 全局 SQL:帮会表在 guildSchema、好友在 friendSchema、聚宝斋在 tradeSchema
	mappingDB *redis.Client // DB 0(go-zero RedisConf 无 DB 字段)
	rankRDB   *redis.Client // guild 的 RedisClient,DB 0
	sharedRDB *redis.Client // DB 0
	sceneRDB  *redis.Client // scene_manager Redis (DB 0 by default)
	// sceneNodes: etcd 场景节点注册表(scene_registry.go)。nil = 没给 -etcd-endpoints 或地址非法。
	sceneNodes sceneRegistry

	// 只用于把 DB 号打进 Notes —— 审计输出必须自己说清「我查的是哪个库」,
	// 否则「0 在线」这种结论没法复核。
	sharedDBIndex int

	src    uint32 // source zone (merging FROM)
	dst    uint32 // target zone (merging INTO)
	verify bool   // post-merge verification mode (-verify-merged)

	// manifest / manifestErr:-verify-merged 用的那份清单,由 runAuditEntry 按 -manifest-path 读。
	// 读不出 / 是 dry-run 预览 / src-dst 对不上时 manifest 为 nil、manifestErr 说明原因,相关断言报 INFRA。
	manifest    *mergeManifest
	manifestErr error
	// tradeSchema / skipTrade:聚宝斋库名与跳过声明(-trade-schema / -skip-trade-mysql)。
	// 跳过时 verify:trade_market_zone 标 warn「NOT VERIFIED」,不伪装成通过。
	tradeSchema string
	skipTrade   bool
	// guildSchema:帮会独占库名(-guild-schema,默认 mmorpg_guild)。帮会表不在
	// -mysql-dsn 的默认库里(D-14 §8),所有帮会 SQL 都要按「库名.表名」限定。
	guildSchema string
	// skipGuild:运维声明这里没有部署帮会服务(-skip-guild-mysql 与 -skip-guild-rank 同时给出,与合服前置
	// P1 同一口径)。guild_member、verify:guild_zone、verify:guild_rank 三行审计都据此报 SKIPPED(warn),
	// 不伪装成通过,也不因为查不到表而报 INFRA。
	skipGuild bool
	// friendSchema:好友独占库名(-friend-schema,默认 mmorpg_friend,port-decisions D-14)。
	// 与 tradeSchema 同一口径:经同一个 -mysql-dsn 以「库名.表名」访问,flag 只为集成测试
	// 的一次性库留口子(否则集成测试只能去动真实的 mmorpg_friend)。
	friendSchema string
	// routes:路由库的读方。nil = 没装成(mapping Redis 连不上 / 库打不开 / 库与 Redis 不成对,原因在 routeErr),
	// 依赖它的检查一律报 INFRA。
	routes   routeAuditReader
	routeErr error
}

// routeReads 返回路由库读方;没装成时返回原因(调用方报 INFRA,不能当作「没有玩家」)。
func (c auditConfig) routeReads() (routeAuditReader, error) {
	switch {
	case c.routes != nil:
		return c.routes, nil
	case c.routeErr != nil:
		return nil, c.routeErr
	}
	return nil, errors.New("no route DB read side (mapping Redis unreachable, or the route DB was not set up)")
}

// verifyManifest 返回 -verify-merged 核对用的清单;没有可用清单时返回原因(调用方报 INFRA)。
func (c auditConfig) verifyManifest() (*mergeManifest, error) {
	if c.manifestErr != nil {
		return nil, c.manifestErr
	}
	if c.manifest == nil {
		return nil, errors.New("no manifest loaded (-verify-merged requires -manifest-path)")
	}
	return c.manifest, nil
}

// auditEntryParams is the pure-data input for runAuditEntry. main.go owns
// flag parsing; audit_resources.go owns the audit semantics. This struct
// is the seam between them.
type auditEntryParams struct {
	src      uint32
	dst      uint32
	mysqlDSN string

	mappingAddr string
	mappingPwd  string
	mappingDB   int

	rankAddr string
	rankPwd  string
	rankDB   int

	sharedAddr string
	sharedPwd  string
	sharedDB   int

	sceneAddr string
	scenePwd  string
	sceneDB   int
	// etcdEndpoints: -etcd-endpoints 原文;空 = 没给,source_scene_nodes 报 INFRA。
	etcdEndpoints string

	verifyMerged bool
	manifestPath string
	tradeSchema  string
	skipTrade    bool
	skipGuild    bool
	guildSchema  string
	friendSchema string
	// route 是路由库参数(main 装配,routeEntryOptionsOf;DSN 含口令,不进日志)。
	route routeEntryOptions
}

// firstNonEmpty returns a if non-empty, else b. Avoids a one-line helper
// being copy-pasted into multiple call sites in main.go.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// openAuditRoutes 开路由库并核对它与 mapping Redis 成对。审计从不写:探针只读,发布器只用来读水位键,WAITAOF 参数用不到。
func openAuditRoutes(ctx context.Context, ro routeEntryOptions, mapping *redis.Client) (*mergeRouteStore, error) {
	if mapping == nil {
		return nil, errors.New("no mapping Redis handle: cannot check whether the route DB and the mapping Redis are paired " +
			"(" + routeWatermarkKey + ")")
	}
	return openPairedRouteStore(ctx, ro.dbDSN, ro.sqlBackend, newRoutePublisher(mapping, routeDurability{}))
}

// runAuditEntry is the -mode=audit dispatch from main.go. It opens the
// connections, calls runAuditMode, prints the report, and exits non-zero
// if any auditor returned Severity="block".
//
// Exit code policy:
//
//	0 — clean, no blockers
//	1 — at least one block-severity finding (do NOT proceed with merge)
//	2 — infrastructure error (couldn't even connect to MySQL/Redis)
//
// Ops scripts can branch on exit code without parsing stdout.
func runAuditEntry(p auditEntryParams) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	db, err := openCurrentSession(ctx, p.mysqlDSN, p.route.sqlBackend)
	if err != nil {
		log.Printf("ERROR: mysql open: %v — MySQL-backed checks will report INFRA", err)
		db = nil
	} else {
		defer db.Close()
	}

	// 每个 DB 一个句柄。ping 失败**不再**降级成静默跳过:句柄留 nil,对应
	// auditor 会返回 INFRA 级 block,整个审计以 exit 2 结束。
	dial := func(label, addr, pwd string, dbIndex int) *redis.Client {
		c := redis.NewClient(&redis.Options{Addr: addr, Password: pwd, DB: dbIndex})
		if err := c.Ping(ctx).Err(); err != nil {
			log.Printf("ERROR: %s redis ping (%s db=%d): %v", label, addr, dbIndex, err)
			_ = c.Close()
			return nil
		}
		return c
	}
	mapRdb := dial("mapping", p.mappingAddr, p.mappingPwd, p.mappingDB)
	rankRdb := dial("guild", p.rankAddr, p.rankPwd, p.rankDB)
	sharedRdb := dial("shared(DB0)", p.sharedAddr, p.sharedPwd, p.sharedDB)
	sceneRdb := dial("scene_manager", p.sceneAddr, p.scenePwd, p.sceneDB)
	for _, c := range []*redis.Client{mapRdb, rankRdb, sharedRdb, sceneRdb} {
		if c != nil {
			defer c.Close()
		}
	}

	var sceneNodes sceneRegistry
	if p.etcdEndpoints != "" {
		if reg, err := newEtcdGatewayRegistry(p.etcdEndpoints); err != nil {
			log.Printf("ERROR: %v", err)
		} else {
			sceneNodes = reg
		}
	}

	cfg := auditConfig{
		db:            db,
		mappingDB:     mapRdb,
		rankRDB:       rankRdb,
		sharedRDB:     sharedRdb,
		sceneRDB:      sceneRdb,
		sceneNodes:    sceneNodes,
		sharedDBIndex: p.sharedDB,
		src:           p.src,
		dst:           p.dst,
		verify:        p.verifyMerged,
		tradeSchema:   p.tradeSchema,
		skipTrade:     p.skipTrade,
		skipGuild:     p.skipGuild,
		guildSchema:   p.guildSchema,
		friendSchema:  p.friendSchema,
	}
	// 路由库读方:开库前先核对库与 mapping Redis 成对。装不成不整个退出:其余检查照跑,依赖它的检查报 INFRA,
	// 整个审计以 exit 2 结束(结论不可信)。
	routeStore, rerr := openAuditRoutes(ctx, p.route, mapRdb)
	if rerr != nil {
		cfg.routeErr = fmt.Errorf("route DB: %w", rerr)
		log.Printf("ERROR: %v — the route-backed checks will report INFRA", cfg.routeErr)
	} else {
		cfg.routes = routeStore
		defer routeStore.Close()
	}
	if p.verifyMerged {
		// 读不出不整个退出:其余断言照跑,依赖清单的那一条报 INFRA,整个审计以 exit 2 结束(结论不可信)。
		cfg.manifest, cfg.manifestErr = loadVerifyManifest(p.manifestPath, p.src, p.dst)
		if cfg.manifestErr != nil {
			log.Printf("ERROR: %v — the manifest-backed verification will report INFRA", cfg.manifestErr)
		} else {
			log.Printf("verify: manifest %s (run_id=%s)", p.manifestPath, cfg.manifest.RunID)
		}
	}

	mode := "pre-merge"
	if p.verifyMerged {
		mode = "POST-MERGE VERIFICATION"
	}
	log.Printf("=== Merge-zone audit (%s) src=%d dst=%d ===", mode, p.src, p.dst)

	results := runAuditMode(ctx, cfg)
	blockCount, _ := printAuditReport(results)

	// exit 2 优先于 exit 1:「审计本身没跑成」和「审计跑成了并发现问题」
	// 对 ops 脚本是两件事。
	infra := 0
	for _, r := range results {
		if isInfraAudit(r) {
			infra++
		}
	}
	if infra > 0 {
		log.Printf("Audit could not complete: %d check(s) failed for infrastructure reasons. "+
			"The result is NOT a clean bill of health.", infra)
		os.Exit(2)
	}
	if blockCount > 0 {
		os.Exit(1)
	}
}

// runAuditMode is the -mode=audit entry point invoked from main.go.
//
// The contract:
//   - Read-only. Never writes. Refusing to write is a safety property of the
//     audit — main.go is the only writer, this file is the inspector.
//   - Returns a non-nil slice; never nil.
//
// -verify-merged 的四行与 mergeRecoveryRowNames 一一对应,P7 审计与 go/db barrierrestore 按这组行名校验;
// 这里加行必须两边同批改。
func runAuditMode(ctx context.Context, cfg auditConfig) []ResourceAudit {
	var auditors []func(context.Context, auditConfig) ResourceAudit
	if cfg.verify {
		auditors = []func(context.Context, auditConfig) ResourceAudit{
			verifyMergedInto,             // 映射已写、是这份清单写的,Redis 标记与库一致
			verifyGuildZoneDrained,       // guild.zone_id=src 必须为 0
			verifyTradeMarketZoneDrained, // trade_listing.market_zone=src 必须为 0(trade_step.go)
			verifyGuildRankZSets,         // 源榜已删,目标榜成员数与公会行数一致
		}
	} else {
		auditors = []func(context.Context, auditConfig) ResourceAudit{
			auditPlayerNameConflicts, // 名字全服唯一(data_service player_name),合服无冲突 —— 只出一条 info 说明行
			auditFriend,              // mmorpg_friend.friend:keyed by player_id;survives merge automatically
			auditFriendRequest,       // mmorpg_friend.friend_request:same
			auditGuildMembers,        // global guild_member table; surfaces volume + zone-cross hints
			auditSourceZoneNodes,     // 源区场景节点:etcd 注册表 + 负载集
			auditOnlinePresence,      // player:session(DB 0)
		}
	}

	results := make([]ResourceAudit, 0, len(auditors))
	for _, fn := range auditors {
		// Each auditor gets its own derived context so a slow scan on one
		// resource cannot stall the rest.
		// 30s is generous — if any auditor needs more, it's broken.
		auditCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		results = append(results, fn(auditCtx, cfg))
		cancel()
	}
	return results
}

// printAuditReport renders results as a markdown-style table on stdout.
//
// The format is deliberately copy-pasteable into the merge runbook so ops
// can attach it to the maintenance ticket. Severity drives the marker
// column ([!] block / [.] warn / [ ] info) — block means "do NOT proceed
// with merge until resolved."
func printAuditReport(results []ResourceAudit) (blockCount, warnCount int) {
	log.Println()
	log.Println("=== Merge-zone resource audit ===")
	log.Println()
	log.Printf("%-3s %-22s %10s %10s %12s %10s  %s",
		"sev", "resource", "src_count", "dst_count", "unique_scope", "conflicts", "notes")
	log.Println(strings.Repeat("─", 110))
	for _, r := range results {
		marker := " "
		switch r.Severity {
		case "block":
			marker = "!"
			blockCount++
		case "warn":
			marker = "."
			warnCount++
		}
		log.Printf("[%s] %-22s %10d %10d %12s %10d  %s",
			marker, r.Name, r.SourceCount, r.TargetCount, r.UniqueScope, r.ConflictCount, r.Notes)
	}
	log.Println()
	log.Printf("Audit summary: %d block(s), %d warn(s), %d info row(s).", blockCount, warnCount, len(results)-blockCount-warnCount)
	log.Println()
	if blockCount > 0 {
		log.Println("⚠️  At least one resource is in BLOCK state. Resolve before running -apply.")
		log.Println("   See docs/ops/merge-zone-runbook.md §4.2 / §6 for guidance.")
		log.Println()
	}
	return
}

// ─── per-resource auditors ─────────────────────────────────────────────
//
// Each auditor returns a single ResourceAudit. On error it returns a
// row with Severity="warn" and the error in Notes — never panics, never
// fails the whole audit. Audit is best-effort by design: a missing
// optional table (e.g. chat_history not deployed yet) should not break
// the merge gate.

// auditPlayerNameConflicts —— 角色名口径的说明行:名字全服唯一,合服无冲突。
//
// 角色名的唯一性**不按 zone**:真源是 data_service 全局库的 player_name 表(uk_player_name 全服唯一,
// docs/design/guild-phase2/03-names.md §3.16),login 建角时先 ReservePlayerName 占名再建角。两个 zone 里不可能各有一个
// 「剑圣」,合服也就没有重名可撞。注册表在全局库,合服不搬不改。
//
// 不查库:唯一性由 data_service 的唯一键在写入时保证,不是合服期能靠扫描发现的问题;这里查一遍只会制造第二份
// 「名字是否唯一」的判据。级别刻意是 info 而不是 block —— 一条守不住任何东西的 block 只会逼运维学会跳过门禁。
func auditPlayerNameConflicts(ctx context.Context, cfg auditConfig) ResourceAudit {
	return ResourceAudit{
		Name:        "player.name (global)",
		UniqueScope: "global",
		Severity:    "info",
		Notes:       "名字在 data_service 全局库 player_name 全服唯一,合服无冲突、不改名;force_rename 管线保持未启用。",
	}
}

// ── friend(独占库 mmorpg_friend)────────────────────────────────────────
//
// 2026-09-18(friend 移植 F3):friend 的表从共享库 `mmorpg` 搬到独占库
// `mmorpg_friend`(port-decisions D-14),建表由 go/schemamigrate 按
// proto/friend/friend_table.proto 做。原先建这三张表的 deploy/mysql-init/guild_friend_tables.sql
// 已于 2026-09-19 **整个文件删除**:帮会二期把公会表迁进 mmorpg_guild、本次把好友表迁进
// mmorpg_friend,两边各搬走一半后它已无内容(建库改在 deploy/mysql-init/00_init_zone_dbs.sql)。
//
// 本次**只改连接目标,不加任何迁移逻辑**:已核对 friend / friend_request 的行里
// 没有 zone_id / home_zone 之类的分区列,合服后按 player_id 自动存活 —— 这一点
// 与搬库之前完全一样,搬库只换了这两张表住在哪个 schema 里。

// defaultFriendSchema 镜像 go/friend/internal/data.DatabaseName。friend 服的 config.Validate
// 强制 MySQL.DBName 等于它,所以生产上只有这一个合法值;-friend-schema 只为集成测试的
// 一次性库留口子。merge_zone 是独立 module,不能 import go/friend,字面量靠评审守住。
const defaultFriendSchema = "mmorpg_friend"

// validateFriendSchemaName 在任何 SQL 之前验库名形状。库名直接拼进语句(标识符不能用
// 占位符),这是这条拼接唯一的注入防线。
//
// 刻意复用 merge_run.go 的公共 schemaNamePattern 而不是再抄一份正则:三个 flag
// (-guild-schema / -trade-schema / -friend-schema)防的是同一件事(「是不是一个朴素
// 标识符」),抄三份迟早会漂移成三套注入防线。
func validateFriendSchemaName(schema string) error {
	if !schemaNamePattern.MatchString(schema) {
		return fmt.Errorf("-friend-schema %q is not a plain identifier ([A-Za-z0-9_], 1-64 chars)", schema)
	}
	return nil
}

// friendTableQualified 把库名拼到表名前。schema 为空时回落到默认库:审计是只读的,
// 与其因为调用方漏传而查一个空库名(SQL 语法错 → INFRA → exit 2),不如查生产上唯一合法的那个。
func friendTableQualified(schema, table string) string {
	if schema == "" {
		schema = defaultFriendSchema
	}
	return schema + "." + table
}

// auditFriend — verify friend table won't break under merge.
//
// `friend(player_id, friend_player_id)` is keyed by player_id alone (no
// zone_id column — see proto/friend/friend_table.proto). Because
// player_id is globally unique (mmo_cross_server_architecture.md §"player_id
// never encodes zone"), friend rows survive merge automatically.
//
// What we still want to flag:
//   - Pre-existing rows where one side has home_zone=src and the other
//     has home_zone=dst. They were already cross-zone friends and that's
//     fine, but ops should know the volume — high counts suggest active
//     cross-zone play, which raises confidence the merge is overdue.
//   - Orphan rows (friend_player_id not in any zone's mapping). These
//     are pre-existing data quality issues, not caused by merge, but
//     surfacing them now lets ops fix opportunistically.
func auditFriend(ctx context.Context, cfg auditConfig) ResourceAudit {
	r := ResourceAudit{Name: "friend", UniqueScope: "global"}
	if cfg.db == nil {
		return infraAudit(r.Name, "no MySQL handle")
	}
	if err := validateFriendSchemaName(firstNonEmpty(cfg.friendSchema, defaultFriendSchema)); err != nil {
		return infraAudit(r.Name, "%v", err)
	}
	table := friendTableQualified(cfg.friendSchema, "friend")
	// 库 / 表不在一律 INFRA(exit 2),不降级成「info: 没有这张表」:friend 已经是常驻服务,
	// 「查不到」与「没有好友关系」在这里分不开(-mysql-dsn 指错实例也是同一个症状),
	// 而本文件 2026-05-23 的教训正是「优雅降级」把审计读成了干净通过。
	if err := cfg.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&r.SourceCount); err != nil {
		return infraAudit(r.Name, "count %s failed: %v (friend 已迁到独占库 mmorpg_friend;"+
			"库没建见 deploy/mysql-init/00_init_zone_dbs.sql,表没建跑 friend -f etc/friend.yaml -migrate)", table, err)
	}
	r.TargetCount = r.SourceCount // same global table; we don't subdivide
	r.Severity = "info"
	r.Notes = table + ": global table keyed by player_id only — survives merge automatically. No action."
	return r
}

// auditFriendRequest — pending requests across zones.
//
// Same shape as friend table (no zone_id). Pending requests stay pending
// across merge; on first login post-merge the recipient sees them as
// usual. The one quirk: a request from src player to dst player was
// previously "cross-zone" and may have been throttled by client UX; after
// merge it's local. Worth a one-line note, not a block.
func auditFriendRequest(ctx context.Context, cfg auditConfig) ResourceAudit {
	r := ResourceAudit{Name: "friend_request", UniqueScope: "global"}
	if cfg.db == nil {
		return infraAudit(r.Name, "no MySQL handle")
	}
	if err := validateFriendSchemaName(firstNonEmpty(cfg.friendSchema, defaultFriendSchema)); err != nil {
		return infraAudit(r.Name, "%v", err)
	}
	table := friendTableQualified(cfg.friendSchema, "friend_request")
	// status = 1 是 FRIEND_REQUEST_PENDING(proto/friend/friend.proto FriendRequestStatus),
	// 数字与枚举的对应关系没变,搬库只换了 schema。
	if err := cfg.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM "+table+" WHERE status = 1").Scan(&r.SourceCount); err != nil {
		return infraAudit(r.Name, "count %s failed: %v (friend 已迁到独占库 mmorpg_friend)", table, err)
	}
	r.TargetCount = r.SourceCount
	r.Severity = "info"
	r.Notes = table + ": pending requests survive merge as-is. Cross-zone requests become local automatically."
	return r
}

// auditGuildMembers — surface guild_member volume + zero-orphan check.
//
// guild_member(guild_id, player_id, ...) — keyed by player_id, no zone
// column. After main.go's guild MySQL step runs, every guild row has
// zone_id rewritten to dst; guild_member rows stay valid because they
// reference the same guild_id. We just count to give ops a sanity
// number and verify there's no guild_member pointing at a guild that
// no longer exists post-merge (orphan defensive check).
//
// 失败口径(2026-09-28,player-storage-placement.md §12 A9):帮会没被声明跳过时,库 / 表查不到与孤儿查询
// 出错一律 INFRA(exit 2)。此前计数失败降级成 info「table not present」、孤儿查询的错误被 `_ =` 吞掉:
// 帮会是常驻服务,「查不到」与「没有成员」在结果上分不开(-mysql-dsn 指错实例也是同一症状),本文件
// 2026-05-23 的教训正是「优雅降级」把审计读成了干净通过。运维显式声明没有帮会服务(-skip-guild-mysql
// -skip-guild-rank)时报 SKIPPED(warn):没查就不能看起来像通过。
func auditGuildMembers(ctx context.Context, cfg auditConfig) ResourceAudit {
	r := ResourceAudit{Name: "guild_member", UniqueScope: "global"}
	if cfg.skipGuild {
		r.Severity = "warn"
		r.Notes = "SKIPPED (-skip-guild-mysql -skip-guild-rank): the guild service is declared absent here — guild_member was not checked"
		return r
	}
	if cfg.db == nil {
		return infraAudit(r.Name, "no MySQL handle")
	}
	if err := validateGuildSchemaName(cfg.guildSchema); err != nil {
		return infraAudit(r.Name, "%v", err)
	}
	memberTable := guildQualified(cfg.guildSchema, guildMemberTable)
	if err := cfg.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+memberTable).Scan(&r.SourceCount); err != nil {
		return infraAudit(r.Name, "count %s failed: %v (帮会表住独占库 %s:库没建 / 表没建跑 guild -f etc/guild.yaml -migrate;"+
			"确实没部署帮会服务才给 -skip-guild-mysql -skip-guild-rank)", memberTable, err, cfg.guildSchema)
	}
	r.TargetCount = r.SourceCount
	// Orphan check: members pointing at non-existent guilds.
	var orphans int64
	if err := cfg.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM "+memberTable+" m"+
			" LEFT JOIN "+guildQualified(cfg.guildSchema, guildTable)+" g ON m.guild_id = g.guild_id"+
			" WHERE g.guild_id IS NULL").Scan(&orphans); err != nil {
		return infraAudit(r.Name, "orphan check on %s failed: %v", memberTable, err)
	}
	r.ConflictCount = orphans
	if orphans > 0 {
		r.Severity = "warn"
		r.Notes = fmt.Sprintf("%d %s rows reference non-existent guilds (pre-existing data quality issue, not merge-caused)", orphans, memberTable)
	} else {
		r.Severity = "info"
		r.Notes = "rows survive merge automatically (guild_id stable). 0 orphans."
	}
	return r
}
