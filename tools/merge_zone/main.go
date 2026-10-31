package main

// merge_zone —— 停服维护窗口内把 source zone 合并进 target zone(别名合服,docs/design/player-data-tidb-scaling-plan.md §7.1)。
//
// 合服只写一条「源区 → 目标区」的映射(全局库 zone_merged_into):玩家行、路由行都不动,home_zone 还是源区,各服务读归属时
// 经 placement.ResolveMergedZone 解析成最终区。按区存的业务数据(公会 zone_id、聚宝斋 market_zone、公会榜)仍由本工具在
// 维护窗口里合并。路由库是唯一真源,mapping Redis 里的 merge:merged_into:{src} 只是它的镜像。
//
// ── Redis DB 分布(写错库 = 静默无效)────────────────────────────────
//
//	mapping  DB 0   merge:merged_into:{src} / mapping:route:watermark
//	                go/data_service/etc/data_service.yaml → MappingRedis(go-zero RedisConf 没有 DB 字段 ⇒ 恒 DB 0)
//	guild    DB 0   guild_rank:zone:{z} / guild:v2:{id} / guild_rank:maintenance_lock
//	                go/guild/etc/guild.yaml → RedisClient.DB(2026-10-23 由 2 改为 0:Redis Cluster 只有 DB 0)
//	shared   DB 0   player:session:{pid}(-notice-redis-*;go/login / go/player_locator 都是 DB 0)
//	scene    DB 0   scene_nodes:zone:{z}:load(-scene-redis-*;scene_manager_service.yaml Redis)
//
// ── -mode merge 的步骤顺序(顺序本身是正确性的一部分;与 merge_run.go 的 runMerge / mergeStepPlan 逐段对应)────
//
//	P1 preflight    源 / 目标 zone 库存在,guild / trade 库表就绪(只读)
//	R  resume       读既有清单:dry-run 预览一律拒读;src / dst 必须与本次一致
//	P2 zone down    源区场景节点全下线(etcd 注册表的正面证据 + 负载集为空)
//	P6 sessions     home_zone=src 的玩家没有 player:session(zone-down 没做干净的信号)
//	P7 teams        home_zone=src 的玩家不在任何活队伍里(全球 SQL team_membership;队伍记录写着成员的 zone,合服不迁移)
//	X  merged_into  映射冲突检查:源区已合进别的 zone / 目标区自己已被合走 即拒绝;清单说映射已写而库里没有(已撤销)也拒绝
//	N  guild name   公会重名断言(冲突时一个字节都不写)
//	M  manifest     **在第一次写之前**落盘清单(公会 / 榜单成员 / 商品);dry-run 只写 <path>.dryrun.json
//	1  merged_into  写全局库 zone_merged_into,再写 Redis 标记 merge:merged_into:{src}(+ WAITAOF)  ← 必须在业务合并之前
//	2  settle       等各服务的映射缓存刷新(-merged-into-settle-sec,默认 2 个刷新周期共 10 秒)
//	3  guild_mysql  重收一次源区公会并进清单 → guild.zone_id 按清单逐条改写 + guild:v2 缓存失效 + 复查源区
//	4  trade_mysql  重收一次源区商品并进清单 → trade_listing.market_zone 按清单逐条改写 + 复查(seller_zone_at_listing 不改)
//	5  guild_rank   guild_rank:zone ZSET 合并(maintenance_lock 内重读源榜 + MULTI/EXEC)
//	6  merge_done   全部完成的标记
//
// 1 必须在 3~5 之前:映射生效之后,新建的号、新建的公会、新上架的商品都归到目标区,业务合并不会有漏网的行,所以不需要任何闸门。
// 2 兜的是缓存滞后窗口:data_service 定时重读映射(默认 5 秒),等 2 个周期之后旧缓存已不存在;滞后窗口内落到源区的对象由 3、4 的
// 重收并进清单。M 仍在第一次写之前:撤销只认清单里的对象。
//
// 任何一步失败都非零退出、不标记完成;同一个 -manifest-path 重跑按 stepDone 续跑,每一步本身幂等。
// 合服不动玩家,所以「源区玩家」随时可以按 home_zone 重查,续跑不依赖任何玩家名单。
//
// ── 其余模式 ────────────────────────────────────────────────
//
//	-mode unmerge   按清单把榜单 / 商品 / 公会写回,最后删映射与 Redis 标记(unmerge.go)。合服之后新建的号按自己的 home_zone
//	                留在各自的区,不需要处理;目标区又被合走时必须先撤销那一次(逆序撤销)
//	-mode audit     只读审计:合服前门禁 / -verify-merged 合服后验证(audit_*.go)。验证行收敛成 verify:merged_into、
//	                verify:guild_zone、verify:trade_market_zone、verify:guild_rank,go/db barrierrestore 校验同一组行名

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	pb "proto/common/database"
	"shared/authoritysql"
	"shared/drainproof"
)

// 默认值集中在这里,便于与各服务 yaml 对照。
const (
	// data_service 的 mapping Redis **一定是 DB 0**:它用 go-zero 的
	// redis.MustNewRedis(config.MappingRedis),而 go-zero 的 RedisConf
	// (v1.10.0 core/stores/redis/conf.go)只有 Host/Type/User/Pass/Tls/
	// NonBlock/PingTimeout —— **没有 DB 字段**,yaml 里写 `DB: 15` 会被
	// 反序列化直接忽略。把这里改成别的库,镜像就写到 data_service 从不碰的地方。
	defaultMappingRedisDB = 0
	defaultGuildRedisDB   = 0 // guild.yaml RedisClient.DB(2026-10-23 由 2 改为 0:Redis Cluster 只有 DB 0)
	defaultSharedRedisDB  = 0 // login / player_locator / scene_manager
	// defaultMergedIntoSettleSec 是 -merged-into-settle-sec 的缺省值:data_service 默认每 5 秒重读一次 zone_merged_into,
	// 等 2 个周期(§7.1 落码口径「写完映射后等 2 个周期」)。
	defaultMergedIntoSettleSec = 10
)

// options 是 flag 解析结果的全部载体。把它单独拎出来,是为了让 runMerge /
// runUnmerge 能在测试里被直接调用,而不必经过 flag 包。
type options struct {
	mode         string
	verifyMerged bool

	sourceZone uint32
	targetZone uint32
	mysqlDSN   string

	redisAddr string
	redisPwd  string
	redisDB   int

	mappingAddr string
	mappingPwd  string
	mappingDB   int

	noticeAddr string
	noticePwd  string
	noticeDB   int

	sceneAddr string
	scenePwd  string
	sceneDB   int

	dryRun bool
	apply  bool

	skipGuild     bool
	skipRank      bool
	skipTrade     bool
	etcdEndpoints string
	tradeSchema   string
	guildSchema   string
	friendSchema  string
	teamSchema    string

	manifestPath string
	// mergedIntoSettle 是 -merged-into-settle-sec:写完映射之后、业务合并之前的等待。
	mergedIntoSettle time.Duration

	maintenanceRunID             string
	freezeTicketID               string
	freezeRequestID              string
	freezeReceiptSHA256          string
	frozenWriterEpoch            uint64
	maintenanceRuntimePlan       string
	maintenanceRuntimeDirectory  string
	maintenanceRuntimeSHA256     string
	recoveryAuditOutput          string
	maintenancePublicationFile   string
	maintenancePublicationSHA256 string
	maintenance                  *maintenanceTool

	timeout time.Duration

	// routeDBDSN 是 -route-db-dsn(全局库,含口令,不进日志)。waitAOF* 是写 Redis 标记时 WAITAOF 的份数与等待
	// (newRouteDurability 校验)。
	routeDBDSN       string
	waitAOFLocal     int
	waitAOFReplicas  int
	waitAOFTimeoutMs int

	// sqlBackend 来自环境变量 MMORPG_SQL_BACKEND(tidb | mysql,空 = tidb),本工具不读服务 yaml。
	// 业务库、路由库、Team 库必须同一档:它们本来就要求是同一个 SQL 入口(requireSameSQLDomain)。
	sqlBackend authoritysql.Backend
}

func main() {
	var o options

	flag.StringVar(&o.mode, "mode", "merge",
		"Operation mode: 'merge' (default: record zone_merged_into, then merge guild / trade / guild rank data), "+
			"'unmerge' (reverse a run from its manifest: restore rank / trade / guild data, then delete the mapping; "+
			"runs the pre-flight gates against the TARGET zone and requires the current NORMAL consumed freeze receipt) or "+
			"'audit' (read-only, audit_resources.go)")
	flag.BoolVar(&o.verifyMerged, "verify-merged", false,
		"Audit-only: run the post-merge verification assertions instead of the pre-merge gates. "+
			"Requires -manifest-path: the mapping and the manifest's run_id are checked against each other")

	srcZone := flag.Uint("source-zone", 0, "Source zone ID to merge FROM (required)")
	dstZone := flag.Uint("target-zone", 0, "Target zone ID to merge INTO (required)")
	flag.StringVar(&o.mysqlDSN, "mysql-dsn",
		"root:@tcp(127.0.0.1:3306)/mmorpg?charset=utf8mb4&parseTime=true&loc=Local",
		"Global SQL DSN. Must reach the per-zone databases zone_<N>_db and the service-owned databases "+
			"-guild-schema / -trade-schema / -friend-schema on the same instance")

	flag.StringVar(&o.redisAddr, "redis-addr", "127.0.0.1:6379", "Default Redis address (used when a specific -*-redis-addr is empty)")
	flag.StringVar(&o.redisPwd, "redis-password", "", "Default Redis password")
	flag.IntVar(&o.redisDB, "redis-db", defaultGuildRedisDB, "GUILD Redis DB (guild_rank:zone / guild:v2 / guild_rank:maintenance_lock; guild.yaml default 0)")

	flag.StringVar(&o.mappingAddr, "mapping-redis-addr", "", "Mapping Redis address (merge:merged_into mirror, mapping:route:watermark). Default: -redis-addr")
	flag.StringVar(&o.mappingPwd, "mapping-redis-password", "", "Mapping Redis password. Default: -redis-password")
	flag.IntVar(&o.mappingDB, "mapping-redis-db", defaultMappingRedisDB,
		"MAPPING Redis DB. Always 0: go-zero's RedisConf has no DB field, so data_service's MappingRedis "+
			"lands on DB 0 no matter what the yaml says")

	flag.StringVar(&o.noticeAddr, "notice-redis-addr", "", "LOGIN shared Redis address (player:session:*). Default: -redis-addr")
	flag.StringVar(&o.noticePwd, "notice-redis-password", "", "LOGIN shared Redis password. Default: -redis-password")
	flag.IntVar(&o.noticeDB, "notice-redis-db", defaultSharedRedisDB, "LOGIN shared Redis DB (login.yaml Node.RedisClient.DB — 0)")

	flag.StringVar(&o.sceneAddr, "scene-redis-addr", "", "scene_manager Redis address (scene_nodes:zone:*:load). Default: -redis-addr")
	flag.StringVar(&o.scenePwd, "scene-redis-password", "", "scene_manager Redis password. Default: -redis-password")
	flag.IntVar(&o.sceneDB, "scene-redis-db", defaultSharedRedisDB, "scene_manager Redis DB (scene_manager_service.yaml Redis — 0)")

	flag.BoolVar(&o.dryRun, "dry-run", false, "Print actions without writing")
	flag.BoolVar(&o.apply, "apply", false, "Required for any write")

	flag.BoolVar(&o.skipGuild, "skip-guild-mysql", false, "Skip the guild.zone_id update + guild cache invalidation")
	flag.BoolVar(&o.skipRank, "skip-guild-rank", false, "Skip the guild_rank:zone ZSET merge")
	flag.BoolVar(&o.skipTrade, "skip-trade-mysql", false,
		"Skip the trade_listing.market_zone rewrite (jubaozhai). ONLY for environments where the trade service was never deployed. "+
			"Without it a missing trade schema/table refuses the merge; with it -verify-merged reports the trade row as NOT VERIFIED")
	flag.StringVar(&o.tradeSchema, "trade-schema", defaultTradeSchema,
		"trade database reached through -mysql-dsn (go/trade forces MySQL.DBName=mmorpg_trade; override only for isolated tests)")
	flag.StringVar(&o.guildSchema, "guild-schema", defaultGuildSchema,
		"guild database reached through -mysql-dsn (go/guild forces the DSN database to be mmorpg_guild; override only for isolated tests)")
	flag.StringVar(&o.friendSchema, "friend-schema", defaultFriendSchema,
		"friend database reached through -mysql-dsn (go/friend forces MySQL.DBName=mmorpg_friend; override only for isolated tests)")
	flag.StringVar(&o.teamSchema, "team-schema", defaultTeamSchema, "全球Team权威schema，与-mysql-dsn的TiDB入口共域；不得用Redis证明无队")
	flag.StringVar(&o.etcdEndpoints, "etcd-endpoints", "",
		"Comma-separated etcd client URLs (http://host:2379), read through the etcd v3 JSON gateway. Required by -mode merge / unmerge: "+
			"positive evidence that the zone being stopped has no registered scene node")

	flag.StringVar(&o.manifestPath, "manifest-path", "",
		"Manifest file written before the first write (merge default ./merge_<src>_to_<dst>_<ts>.json). "+
			"Re-runs, -mode unmerge and -verify-merged consume it. "+
			"-dry-run never writes it: its preview goes to <path>.dryrun.json, which nothing consumes")
	settleSec := flag.Int("merged-into-settle-sec", defaultMergedIntoSettleSec,
		"-mode merge: seconds to wait after recording zone_merged_into, before the guild / trade / rank steps. data_service re-reads "+
			"the table every 5s; the default covers 2 refresh periods. 0 only for isolated tests")

	flag.StringVar(&o.maintenanceRunID, "maintenance-run-id", "", "本轮正式维护run的32位小写hex，不是merge清单run_id")
	flag.StringVar(&o.freezeTicketID, "freeze-ticket-id", "", "本轮NORMAL已消费freeze ticket的32位小写hex")
	flag.StringVar(&o.freezeRequestID, "freeze-request-id", "", "已提交SQL_FROZEN请求的32位小写hex")
	flag.StringVar(&o.freezeReceiptSHA256, "freeze-receipt-sha256", "", "已提交SQL_FROZEN永久收据的64位小写hex")
	flag.Uint64Var(&o.frozenWriterEpoch, "frozen-writer-epoch", 0, "SQL_FROZEN收据实际after_epoch，必须仍是当前writer epoch")
	flag.StringVar(&o.maintenanceRuntimePlan, "maintenance-runtime-plan", "", "P0已固定的单次MAINTENANCE工具私有runtime原件")
	flag.StringVar(&o.maintenanceRuntimeDirectory, "maintenance-runtime-directory", "", "本工具boot私有控制目录，不复用旧boot")
	flag.StringVar(&o.maintenanceRuntimeSHA256, "maintenance-runtime-sha256", "", "runtime原件SHA256，必须与P0来源一致")
	flag.StringVar(&o.recoveryAuditOutput, "recovery-audit-output", "", "仅audit -verify-merged：输出正式P7 MergeRecoveryAudit私有原件")
	flag.StringVar(&o.maintenancePublicationFile, "maintenance-publication-file", "", "P0 MAINTENANCE全集实际closure及SourcePin的正式PublicationEvidence")
	flag.StringVar(&o.maintenancePublicationSHA256, "maintenance-publication-sha256", "", "维护闭合全集原件SHA256")

	flag.DurationVar(&o.timeout, "timeout", 2*time.Hour, "Overall run timeout")

	flag.StringVar(&o.routeDBDSN, "route-db-dsn", "",
		"DSN of the global database that holds zone_merged_into and player_route (dev: testdb, K8s: mmorpg_global): the only source of "+
			"truth for the mapping, required by every mode. The session is pinned to READ-COMMITTED with innodb_lock_wait_timeout=1; "+
			"the DSN is never logged")
	flag.IntVar(&o.waitAOFLocal, "waitaof-local", 1, "WAITAOF numlocal after writing the merge:merged_into mirror (0 or 1)")
	flag.IntVar(&o.waitAOFReplicas, "waitaof-replicas", 0,
		"WAITAOF numreplicas after writing the merge:merged_into mirror (single instance 0, redis-ha 1)")
	flag.IntVar(&o.waitAOFTimeoutMs, "waitaof-timeout-ms", 1500,
		"WAITAOF timeout per attempt in milliseconds (1..2000; 0 would block forever)")

	flag.Parse()
	if err := checkMode(o); err != nil {
		fail("%v", err)
	}
	backend, err := authoritysql.LoadBackend(os.Getenv)
	if err != nil {
		fail("%v", err)
	}
	o.sqlBackend = backend
	// 超出范围直接拒绝,不能让 uint32() 静默截断成另一个 zone。
	o.sourceZone = uint32Flag("source-zone", *srcZone)
	o.targetZone = uint32Flag("target-zone", *dstZone)
	if *settleSec < 0 {
		fail("-merged-into-settle-sec must be >= 0")
	}
	o.mergedIntoSettle = time.Duration(*settleSec) * time.Second

	// 未指定的专用地址 / 密码回落到默认 Redis。DB 号**不**回落:
	// 每个 DB 都有 yaml 里的确定值,猜错就是静默无效。
	o.mappingAddr = firstNonEmpty(o.mappingAddr, o.redisAddr)
	o.mappingPwd = firstNonEmpty(o.mappingPwd, o.redisPwd)
	o.noticeAddr = firstNonEmpty(o.noticeAddr, o.redisAddr)
	o.noticePwd = firstNonEmpty(o.noticePwd, o.redisPwd)
	o.sceneAddr = firstNonEmpty(o.sceneAddr, o.redisAddr)
	o.scenePwd = firstNonEmpty(o.scenePwd, o.redisPwd)

	if err := checkRouteFlags(o); err != nil {
		fail("%v", err)
	}

	// unmerge 的 zone 从清单里读 —— 那才是权威(撤销的对象必须与当初合的一致)。
	// 其余模式必须显式给两个 zone。
	if o.mode != "unmerge" {
		if o.sourceZone == 0 || o.targetZone == 0 {
			fmt.Fprintln(os.Stderr, "ERROR: -source-zone and -target-zone are required (non-zero)")
			flag.Usage()
			os.Exit(1)
		}
	}
	if o.sourceZone != 0 && o.sourceZone == o.targetZone {
		fail("source and target zone must be different")
	}
	// 库名会直接拼进 SQL 标识符:任何模式下先验形状。
	if err := validateTradeSchemaName(o.tradeSchema); err != nil {
		fail("%v", err)
	}
	if err := validateGuildSchemaName(o.guildSchema); err != nil {
		fail("%v", err)
	}
	if err := validateFriendSchemaName(o.friendSchema); err != nil {
		fail("%v", err)
	}
	if !schemaNamePattern.MatchString(o.teamSchema) {
		fail("team-schema不是有效SQL标识符")
	}

	switch o.mode {
	case "audit":
		// 合服后验证要拿清单的 run_id 与库里的映射互相核对:没有清单就无从核对。
		if o.verifyMerged && o.manifestPath == "" {
			fail("-verify-merged requires -manifest-path <the manifest written by the merge being verified>")
		}
		// 同时读取业务原件与路由原件的入口必须共域,审计不能把两个TiDB集群拼成一份事实。
		if err := requireSameSQLDomain(o.mysqlDSN, o.routeDBDSN); err != nil {
			fail("%v", err)
		}
		if o.recoveryAuditOutput != "" {
			if err := runMergeRecoveryAuditEntry(o); err != nil {
				fail("P7业务审核未完成，保持冻结: %v", err)
			}
			return
		}
		runAuditEntry(auditEntryParams{
			src:           o.sourceZone,
			dst:           o.targetZone,
			mysqlDSN:      o.mysqlDSN,
			mappingAddr:   o.mappingAddr,
			mappingPwd:    o.mappingPwd,
			mappingDB:     o.mappingDB,
			rankAddr:      o.redisAddr,
			rankPwd:       o.redisPwd,
			rankDB:        o.redisDB,
			sharedAddr:    o.noticeAddr,
			sharedPwd:     o.noticePwd,
			sharedDB:      o.noticeDB,
			sceneAddr:     o.sceneAddr,
			scenePwd:      o.scenePwd,
			sceneDB:       o.sceneDB,
			etcdEndpoints: o.etcdEndpoints,
			verifyMerged:  o.verifyMerged,
			manifestPath:  o.manifestPath,
			tradeSchema:   o.tradeSchema,
			skipTrade:     o.skipTrade,
			// 与合服前置 P1 同一口径:两个帮会跳过开关都给了才算「这里没有帮会服务」。
			skipGuild:    o.skipGuild && o.skipRank,
			guildSchema:  o.guildSchema,
			friendSchema: o.friendSchema,
			route:        routeEntryOptionsOf(o),
		})
		return

	case "unmerge":
		if err := requireWriteIntent(o); err != nil {
			fail("%v", err)
		}
		if o.manifestPath == "" {
			fail("-mode unmerge requires -manifest-path <file written by the original run>")
		}
		if err := executeMaintenanceTool(o, runUnmerge); err != nil {
			fail("%v", err)
		}
		return

	case "merge":
		if err := requireWriteIntent(o); err != nil {
			fail("%v", err)
		}
		if err := executeMaintenanceTool(o, runMerge); err != nil {
			fail("%v", err)
		}
		return
	}
}

// checkMode 拒绝未知模式。删掉的模式(pin-placement / relocate / storage-audit 等)走到这里同样被拒:
// 静默忽略会让人以为自己要的那件事做了。
func checkMode(o options) error {
	switch o.mode {
	case "merge", "unmerge", "audit":
		return nil
	}
	return fmt.Errorf("unknown -mode %q (expected 'merge', 'unmerge' or 'audit')", o.mode)
}

// checkRouteFlags 是 -route-db-dsn / -waitaof-* 的前置校验(纯函数):三个模式都读路由库,路由库是唯一真源,DSN 必填;
// WAITAOF 参数在连上任何东西之前校验。库与 Redis 是否成对(水位键)要连上之后才能核对,见 openPairedRouteStore。
func checkRouteFlags(o options) error {
	if o.routeDBDSN == "" {
		return errors.New("-route-db-dsn is required: the global database that holds zone_merged_into is the only source of truth for the mapping")
	}
	_, err := newRouteDurability(o.waitAOFLocal, o.waitAOFReplicas, o.waitAOFTimeoutMs)
	return err
}

// uint32Flag 把 flag.Uint 的值收窄成 uint32;超出范围直接拒绝 —— 截断会把人指到另一个 zone。
func uint32Flag(name string, v uint) uint32 {
	if uint64(v) > math.MaxUint32 {
		fail("-%s %d is out of range (max %d)", name, v, uint64(math.MaxUint32))
	}
	return uint32(v)
}

// requireWriteIntent 强制 -dry-run / -apply 二选一。两个都给按 dry-run 处理
// 是错的(运维会以为写了),所以直接拒绝。
func requireWriteIntent(o options) error {
	switch {
	case o.dryRun && o.apply:
		return fmt.Errorf("pass exactly one of -dry-run and -apply, not both")
	case !o.dryRun && !o.apply:
		return fmt.Errorf("refuse to modify data: pass -dry-run to preview, or -apply to write")
	}
	return nil
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ERROR: "+format+"\n", args...)
	os.Exit(1)
}

func verbWrite(dry bool) string {
	if dry {
		return "would be migrated"
	}
	return "migrated"
}

// maintenanceTool只拥有本次P0固定的单boot工具；资源和后台任务随它一起闭合。
type maintenanceCloser interface{ Close() error }
type maintenanceTool struct {
	runtime    *drainproof.Runtime
	session    *drainproof.Session
	catalogDB  *sql.DB
	check      func(context.Context) error
	context    context.Context
	mu         sync.Mutex
	resources  []maintenanceCloser
	registered map[maintenanceCloser]bool
	redisBound map[redis.UniversalClient]bool
	closing    bool
	finishOnce sync.Once
	finishErr  error
}
type maintenanceContextKey struct{}

func maintenanceFromContext(ctx context.Context) *maintenanceTool {
	t, _ := ctx.Value(maintenanceContextKey{}).(*maintenanceTool)
	return t
}

func maintenanceParent(o options) context.Context {
	if o.maintenance != nil {
		return o.maintenance.context
	}
	return context.Background()
}
func (t *maintenanceTool) hold() { t.session.Hold() }
func holdMaintenanceTool(ctx context.Context) {
	if t := maintenanceFromContext(ctx); t != nil {
		t.hold()
	}
}
func checkMaintenanceTool(ctx context.Context) error {
	t := maintenanceFromContext(ctx)
	if t == nil || t.session == nil || t.check == nil {
		return drainproof.ErrHold
	}
	t.mu.Lock()
	closed := t.closing
	t.mu.Unlock()
	if closed || t.session.Closed() {
		return drainproof.ErrClosed
	}
	if err := t.check(ctx); err != nil {
		t.hold()
		return err
	}
	return nil
}
func enterMaintenanceWork(ctx context.Context) (func(), error) {
	if t := maintenanceFromContext(ctx); t != nil {
		return t.session.Enter()
	}
	return func() {}, nil // 只读入口；真实写另经checkMaintenanceTool。
}
func registerMaintenanceResource(ctx context.Context, r maintenanceCloser) error {
	t := maintenanceFromContext(ctx)
	if t == nil {
		return nil
	}
	if r == nil {
		return drainproof.ErrHold
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closing || t.session.Closed() {
		return drainproof.ErrClosed
	}
	if t.registered == nil {
		t.registered = map[maintenanceCloser]bool{}
	}
	if !t.registered[r] {
		t.registered[r] = true
		t.resources = append(t.resources, r)
	}
	return nil
}
func closeUnownedMaintenanceResource(ctx context.Context, r maintenanceCloser) {
	if maintenanceFromContext(ctx) == nil && r != nil {
		if err := r.Close(); err != nil {
			log.Printf("只读资源关闭失败: %v", err)
		}
	}
}

// Hook捕获固定owner，连同Background释放命令也经过同一关闭线性化点。
type maintenanceRedisHook struct{ tool *maintenanceTool }

func (h maintenanceRedisHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (conn net.Conn, err error) {
		finish, err := h.tool.session.Enter()
		if err != nil {
			return nil, err
		}
		returned := false
		defer func() {
			if !returned || err != nil {
				h.tool.hold()
			}
			finish()
		}()
		conn, err = next(ctx, network, addr)
		returned = true
		return
	}
}
func (h maintenanceRedisHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) (err error) {
		finish, err := h.tool.session.Enter()
		if err != nil {
			return err
		}
		returned := false
		defer func() {
			if !returned || redisUnknown(err) || redisUnknown(cmd.Err()) {
				h.tool.hold()
			}
			finish()
		}()
		err = next(ctx, cmd)
		returned = true
		return
	}
}

// redisUnknown 判断这条命令的结局是否未知。NOSCRIPT 是服务器确定没有执行,redis.Script 会回退成 EVAL;
// Redis 重启或切主之后第一次跑脚本就会碰到,不能因此把工具打成 HOLD。
func redisUnknown(err error) bool {
	return err != nil && !errors.Is(err, redis.Nil) && !redis.HasErrorPrefix(err, "NOSCRIPT")
}
func (h maintenanceRedisHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) (err error) {
		finish, err := h.tool.session.Enter()
		if err != nil {
			return err
		}
		returned := false
		defer func() {
			unknown := !returned || redisUnknown(err)
			for _, cmd := range cmds {
				unknown = unknown || redisUnknown(cmd.Err())
			}
			if unknown {
				h.tool.hold()
			}
			finish()
		}()
		err = next(ctx, cmds)
		returned = true
		return
	}
}
func bindMaintenanceRedis(ctx context.Context, client redis.UniversalClient) error {
	if err := installMaintenanceRedisHook(ctx, client); err != nil {
		return err
	}
	return registerMaintenanceResource(ctx, client)
}
func installMaintenanceRedisHook(ctx context.Context, client redis.UniversalClient) error {
	t := maintenanceFromContext(ctx)
	if t == nil {
		return nil
	}
	if client == nil {
		return drainproof.ErrHold
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closing || t.session.Closed() {
		return drainproof.ErrClosed
	}
	if t.redisBound == nil {
		t.redisBound = map[redis.UniversalClient]bool{}
	}
	if !t.redisBound[client] {
		client.AddHook(maintenanceRedisHook{tool: t})
		t.redisBound[client] = true
	}
	return nil
}

func loadMaintenanceTool(ctx context.Context, o options) (*maintenanceTool, error) {
	proof, err := parseNormalFreezeProof(o)
	if err != nil {
		return nil, err
	}
	runtime, err := drainproof.LoadMaintenanceRuntime(o.maintenanceRuntimePlan, o.maintenanceRuntimeDirectory, o.maintenanceRuntimeSHA256)
	if err != nil {
		return nil, err
	}
	physical, err := hex.DecodeString(os.Getenv("MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256"))
	if err != nil || len(physical) != 32 || !bytes.Equal(physical, runtime.Plan.PhysicalClusterIdentity) ||
		!bytes.Equal(proof.runID, runtime.Plan.RunId) || proof.authoritySchema != runtime.Plan.AuthoritySchema {
		return nil, drainproof.ErrHold
	}
	db, err := authoritysql.Open(ctx, o.routeDBDSN, proof.authoritySchema, o.sqlBackend, 1, authoritysql.Reader())
	if err != nil {
		return nil, err
	}
	var principal string
	if err = db.QueryRowContext(ctx, "SELECT CURRENT_USER()").Scan(&principal); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	catalog, err := authoritysql.NewCatalog(db, authoritysql.CatalogConfig{
		AuthoritySchema: proof.authoritySchema, Generation: runtime.Request.Generation,
		ProtectedSchemas: runtime.Plan.ProtectedSchemas, PhysicalAuthoritySHA256: physical, Principal: principal, RequireReconcile: true})
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	check := func(ctx context.Context) error {
		frozen, err := runtime.CheckMaintenance(ctx, catalog, proof.ticketID)
		if err != nil {
			return err
		}
		if !bytes.Equal(frozen.RequestId, proof.requestID) || !bytes.Equal(frozen.ReceiptDigest, proof.receiptDigest) || frozen.AfterEpoch != proof.epoch {
			return drainproof.ErrHold
		}
		return nil
	}
	if err = check(ctx); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	session, err := runtime.BindMaintenance(ctx, catalog, proof.ticketID)
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	t := &maintenanceTool{runtime: runtime, session: session, catalogDB: db, check: check}
	t.context = context.WithValue(ctx, maintenanceContextKey{}, t)
	return t, nil
}

// 先永久拒新命令并等待accepted；UNKNOWN不阻止资源关闭，但阻止正式成功原件。
func (t *maintenanceTool) finish(ctx context.Context) error {
	t.finishOnce.Do(func() {
		t.finishErr = drainproof.ErrHold
		returned := false
		defer func() {
			if !returned {
				t.hold()
			}
		}()
		t.finishErr = t.finishOnceJoined(ctx)
		returned = true
	})
	return t.finishErr
}
func (t *maintenanceTool) finishOnceJoined(ctx context.Context) error {
	t.session.CloseAdmission()
	if err := t.session.WaitIdle(ctx); err != nil {
		t.hold()
		return err
	}
	t.mu.Lock()
	t.closing = true
	resources := append([]maintenanceCloser(nil), t.resources...)
	t.mu.Unlock()
	var closeErr error
	for i := len(resources) - 1; i >= 0; i-- {
		closeErr = errors.Join(closeErr, resources[i].Close())
	}
	if closeErr != nil {
		t.hold()
	}
	checkErr := t.check(ctx)
	if checkErr != nil {
		t.hold()
	}
	if t.catalogDB != nil {
		closeErr = errors.Join(closeErr, t.catalogDB.Close())
		if closeErr != nil {
			t.hold()
		}
	}
	if err := errors.Join(t.session.Wait(ctx), closeErr, checkErr); err != nil {
		return err
	}
	if err := t.runtime.Write("MAINTENANCE", t.session); err != nil {
		t.hold()
		return err
	}
	return nil
}
func executeMaintenanceTool(o options, run func(options)) error {
	if o.dryRun {
		run(o)
		return nil
	}
	if o.timeout <= 0 {
		return fmt.Errorf("维护工具timeout必须大于0")
	}
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()
	t, err := loadMaintenanceTool(ctx, o)
	if err != nil {
		return err
	}
	o.maintenance = t
	returned := false
	defer func() {
		if !returned {
			t.hold()
		}
	}() // panic继续传播，Fatal退出也不会写成功原件。
	finish, err := t.session.Enter()
	if err != nil {
		return err
	}
	func() { defer finish(); run(o) }()
	returned = true
	return t.finish(t.context)
}

// P7只调用原审核器。正式proto适配由独占schema owner合入；这里不写并行审核JSON。
// verifyFrozen必须强读同NORMAL consumed/current SQL_FROZEN，审核前后均执行；不得以闭合代业务结果。
func runClosedMergeAudit(ctx context.Context, cfg auditConfig, plan *pb.SqlWriterMaintenanceReceipt_Plan,
	frozen *pb.SqlWriterMaintenanceReceipt, publication *pb.SqlWriterMaintenanceReceipt_PublicationEvidence,
	verifyFrozen func(context.Context) error) ([]ResourceAudit, error) {
	if verifyFrozen == nil || !cfg.verify || cfg.manifest == nil || cfg.manifestErr != nil ||
		cfg.src == 0 || cfg.dst == 0 || cfg.src == cfg.dst || cfg.manifest.SourceZone != cfg.src || cfg.manifest.TargetZone != cfg.dst {
		return nil, drainproof.ErrHold
	}
	if err := verifyFrozen(ctx); err != nil {
		return nil, err
	}
	if err := verifyClosedMaintenanceForAudit(plan, frozen, publication); err != nil {
		return nil, err
	}
	rows := runAuditMode(ctx, cfg)
	if err := errors.Join(ctx.Err(), verifyFrozen(ctx), validateMergeRecoveryRows(rows)); err != nil {
		return rows, err
	}
	// 审核期间任何closure/source pin变化都不能作为P7原件。
	if err := verifyClosedMaintenanceForAudit(plan, frozen, publication); err != nil {
		return rows, err
	}
	return rows, nil
}

func verifyClosedMaintenanceForAudit(plan *pb.SqlWriterMaintenanceReceipt_Plan, frozen *pb.SqlWriterMaintenanceReceipt,
	publication *pb.SqlWriterMaintenanceReceipt_PublicationEvidence) error {
	if plan == nil || frozen == nil || publication == nil || len(plan.MaintenanceIssuers) == 0 ||
		frozen.Kind != pb.SqlWriterMaintenanceReceipt_FREEZE || frozen.Phase != pb.SqlWriterMaintenanceReceipt_FROZEN ||
		frozen.RunPhase != pb.SqlWriterDrainRun_SQL_FROZEN || frozen.CommittedMs == 0 || !bytes.Equal(plan.RunId, frozen.RunId) ||
		len(publication.LogicalRoles) != len(plan.MaintenanceIssuers) || len(publication.SourceEvidence) != len(plan.MaintenanceIssuers) ||
		len(publication.ProtoReflect().GetUnknown()) != 0 {
		return drainproof.ErrHold
	}
	decodedPlan := &pb.SqlWriterMaintenanceReceipt_Plan{}
	if proto.Unmarshal(frozen.PlanProto, decodedPlan) != nil || !proto.Equal(plan, decodedPlan) {
		return drainproof.ErrHold
	}
	inventory, err := (proto.MarshalOptions{Deterministic: true}).Marshal(&pb.SqlWriterMaintenanceReceipt_PublicationEvidence{LogicalRoles: plan.MaintenanceIssuers})
	if err != nil {
		return err
	}
	digest := sha256.Sum256(inventory)
	if !bytes.Equal(digest[:], publication.IssuerInventoryDigest) {
		return drainproof.ErrHold
	}
	expected := map[string]*pb.SqlWriterMaintenanceReceipt_IssuerClosure{}
	for _, seed := range plan.MaintenanceIssuers {
		if seed == nil || len(seed.IssuerId) != 16 || expected[string(seed.IssuerId)] != nil {
			return drainproof.ErrHold
		}
		expected[string(seed.IssuerId)] = seed
	}
	actual := map[string]*pb.SqlWriterMaintenanceReceipt_IssuerClosure{}
	for _, closed := range publication.LogicalRoles {
		if closed == nil || actual[string(closed.IssuerId)] != nil || closed.AcceptingClosedMs < frozen.CommittedMs {
			return drainproof.ErrHold
		}
		if err := drainproof.VerifyClosure(closed); err != nil {
			return err
		}
		identity := proto.Clone(closed).(*pb.SqlWriterMaintenanceReceipt_IssuerClosure)
		identity.AcceptingClosedMs = 0
		identity.AcceptedSettledMs = 0
		identity.AcceptedFrontierDigest = nil
		identity.UnresolvedPublications = 0
		if !proto.Equal(identity, expected[string(closed.IssuerId)]) {
			return drainproof.ErrHold
		}
		actual[string(closed.IssuerId)] = closed
	}
	seen := map[string]bool{}
	for _, pin := range publication.SourceEvidence {
		raw, err := drainproof.ReadPin(pin, 16<<20)
		if err != nil {
			return err
		}
		closed := &pb.SqlWriterMaintenanceReceipt_IssuerClosure{}
		if (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, closed) != nil || seen[string(closed.IssuerId)] ||
			!proto.Equal(actual[string(closed.IssuerId)], closed) {
			return drainproof.ErrHold
		}
		seen[string(closed.IssuerId)] = true
	}
	if len(seen) != len(expected) {
		return drainproof.ErrHold
	}
	return nil
}

// mergeRecoveryRowNames 是 P7 审计的完整行名目录,与 go/db/internal/barrierrestore/restore.go 的 verifyMergeRecoveryAudit 同一组。
func mergeRecoveryRowNames() []string {
	return []string{"verify:merged_into", "verify:guild_zone", "verify:trade_market_zone", "verify:guild_rank"}
}

func validateMergeRecoveryRows(rows []ResourceAudit) error {
	expected := map[string]bool{}
	for _, name := range mergeRecoveryRowNames() {
		expected[name] = true
	}
	if len(rows) != len(expected) {
		return drainproof.ErrHold
	}
	for _, row := range rows {
		note := strings.ToUpper(row.Notes)
		if !expected[row.Name] || row.Severity != "info" || row.SourceCount < 0 || row.TargetCount < 0 || row.ConflictCount != 0 || isInfraAudit(row) ||
			strings.Contains(note, "SKIP") || strings.Contains(note, "UNKNOWN") || strings.Contains(note, "NOT VERIFIED") || strings.Contains(note, "NOT IMPLEMENTED") {
			return drainproof.ErrHold
		}
		delete(expected, row.Name)
	}
	if len(expected) != 0 {
		return drainproof.ErrHold
	}
	return nil
}

// P7是独立只读运行：不BindBoot、不复用已关闭TOOL的资源，不生成第二套审核格式。
func runMergeRecoveryAuditEntry(o options) (err error) {
	if !o.verifyMerged || o.mode != "audit" || o.dryRun || !filepath.IsAbs(o.recoveryAuditOutput) ||
		!filepath.IsAbs(o.maintenancePublicationFile) {
		return drainproof.ErrHold
	}
	if err := requireSameSQLDomain(o.mysqlDSN, o.routeDBDSN); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	proof, err := parseNormalFreezeProof(o)
	if err != nil {
		return err
	}
	runtime, err := drainproof.LoadMaintenanceRuntime(o.maintenanceRuntimePlan, o.maintenanceRuntimeDirectory, o.maintenanceRuntimeSHA256)
	if err != nil {
		return err
	}
	physical, err := hex.DecodeString(os.Getenv("MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256"))
	if err != nil || len(physical) != 32 || !bytes.Equal(physical, runtime.Plan.PhysicalClusterIdentity) ||
		!bytes.Equal(proof.runID, runtime.Plan.RunId) || proof.authoritySchema != runtime.Plan.AuthoritySchema {
		return drainproof.ErrHold
	}
	var resources []maintenanceCloser
	defer func() {
		for i := len(resources) - 1; i >= 0; i-- {
			err = errors.Join(err, resources[i].Close())
		}
	}()
	openSQL := func(raw string, maxOpen int) (*sql.DB, error) {
		cfg, err := mysql.ParseDSN(raw)
		if err != nil {
			return nil, drainproof.ErrHold
		}
		found := false
		for _, schema := range runtime.Plan.ProtectedSchemas {
			found = found || schema == cfg.DBName
		}
		if !found {
			return nil, drainproof.ErrHold
		}
		db, err := authoritysql.Open(ctx, raw, cfg.DBName, o.sqlBackend, maxOpen, authoritysql.Reader())
		if err != nil {
			return nil, err
		}
		resources = append(resources, db)
		return db, nil
	}
	authority, err := openSQL(o.routeDBDSN, 4)
	if err != nil {
		return err
	}
	var principal string
	if err = authority.QueryRowContext(ctx, "SELECT CURRENT_USER()").Scan(&principal); err != nil {
		return err
	}
	catalog, err := authoritysql.NewCatalog(authority, authoritysql.CatalogConfig{AuthoritySchema: proof.authoritySchema,
		Generation: runtime.Request.Generation, ProtectedSchemas: runtime.Plan.ProtectedSchemas,
		PhysicalAuthoritySHA256: physical, Principal: principal, RequireReconcile: true})
	if err != nil {
		return err
	}
	var frozen *pb.SqlWriterMaintenanceReceipt
	verifyFrozen := func(ctx context.Context) error {
		current, err := runtime.CheckMaintenance(ctx, catalog, proof.ticketID)
		if err != nil {
			return err
		}
		if !bytes.Equal(current.RequestId, proof.requestID) || !bytes.Equal(current.ReceiptDigest, proof.receiptDigest) || current.AfterEpoch != proof.epoch {
			return drainproof.ErrHold
		}
		if frozen != nil && !proto.Equal(frozen, current) {
			return drainproof.ErrHold
		}
		frozen = current
		return nil
	}
	if err = verifyFrozen(ctx); err != nil {
		return err
	}
	publicationRaw, err := authoritysql.ReadPrivateControlFile(o.maintenancePublicationFile, 16<<20)
	if err != nil {
		return err
	}
	publicationSHA, err := hex.DecodeString(o.maintenancePublicationSHA256)
	if err != nil || len(publicationSHA) != 32 {
		return drainproof.ErrHold
	}
	publicationSum := sha256.Sum256(publicationRaw)
	if !bytes.Equal(publicationSum[:], publicationSHA) {
		return drainproof.ErrHold
	}
	publication := &pb.SqlWriterMaintenanceReceipt_PublicationEvidence{}
	if err = (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(publicationRaw, publication); err != nil {
		return err
	}
	if err = verifyClosedMaintenanceForAudit(runtime.Plan, frozen, publication); err != nil {
		return err
	}
	manifestRaw, err := os.ReadFile(o.manifestPath)
	if err != nil {
		return err
	}
	if len(manifestRaw) == 0 || len(manifestRaw) > 16<<20 || !filepath.IsAbs(o.manifestPath) {
		return drainproof.ErrHold
	}
	manifestSHA := sha256.Sum256(manifestRaw)
	manifest, err := loadVerifyManifest(o.manifestPath, o.sourceZone, o.targetZone)
	if err != nil {
		return err
	}
	if !validRunID(manifest.RunID) {
		return drainproof.ErrHold
	}
	// 原领域loader负责种类/版本/预览规则；另比对所解析的结构确实来自这份已pin字节。
	var pinnedManifest mergeManifest
	if err := json.Unmarshal(manifestRaw, &pinnedManifest); err != nil {
		return err
	}
	if pinnedManifest.Steps == nil {
		pinnedManifest.Steps = map[string]manifestStep{}
	}
	pinnedJSON, err := json.Marshal(&pinnedManifest)
	if err != nil {
		return err
	}
	loadedJSON, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if !bytes.Equal(pinnedJSON, loadedJSON) {
		return drainproof.ErrHold
	}
	manifestPin := &pb.SqlWriterMaintenanceReceipt_SourcePin{AbsolutePath: o.manifestPath, Sha256: manifestSHA[:]}
	sources, err := mergeRecoveryQueryPins(runtime.Plan)
	if err != nil {
		return err
	}
	game, err := openSQL(o.mysqlDSN, 4)
	if err != nil {
		return err
	}
	dial := func(addr, password string, db int) (*redis.Client, error) {
		client := redis.NewClient(&redis.Options{Addr: addr, Password: password, DB: db, MaxRetries: -1})
		resources = append(resources, client)
		if err := client.Ping(ctx).Err(); err != nil {
			return nil, err
		}
		if err := verifyMergeAuditRedisTarget(ctx, client, runtime.Plan); err != nil {
			return nil, err
		}
		return client, nil
	}
	mapping, err := dial(o.mappingAddr, o.mappingPwd, o.mappingDB)
	if err != nil {
		return err
	}
	rank, err := dial(o.redisAddr, o.redisPwd, o.redisDB)
	if err != nil {
		return err
	}
	// 只读Reader池构造route store：只跑session/meta读探针，写方法没有TOOL身份与冻结证明，一律拒绝。
	routeStore, meta, err := newMergeRouteStoreFromDB(ctx, authority, o.sqlBackend)
	if err != nil {
		return err
	}
	if err := requireCurrentSession(ctx, game, o.sqlBackend); err != nil {
		return err
	}
	watermark, present, err := (&routePublisher{rdb: mapping}).readWatermark(ctx)
	if err != nil {
		return err
	}
	if err := decideRoutePairing(watermark, present, meta.Generation); err != nil {
		return err
	}
	cfg := auditConfig{db: game, mappingDB: mapping, rankRDB: rank, src: o.sourceZone, dst: o.targetZone, verify: true,
		manifest: manifest, tradeSchema: o.tradeSchema, skipTrade: o.skipTrade, guildSchema: o.guildSchema,
		skipGuild: o.skipGuild && o.skipRank, friendSchema: o.friendSchema, routes: routeStore}
	started := uint64(time.Now().UnixMilli())
	for _, closed := range publication.LogicalRoles {
		if closed.AcceptedSettledMs > started {
			return drainproof.ErrHold
		}
	}
	rows, auditErr := runClosedMergeAudit(ctx, cfg, runtime.Plan, frozen, publication, verifyFrozen)
	printAuditReport(rows)
	if auditErr != nil {
		return auditErr
	}
	for _, client := range []*redis.Client{mapping, rank} {
		if err := verifyMergeAuditRedisTarget(ctx, client, runtime.Plan); err != nil {
			return err
		}
	}
	if _, err := drainproof.ReadPin(manifestPin, 16<<20); err != nil {
		return err
	}
	if _, err := mergeRecoveryQueryPins(runtime.Plan); err != nil {
		return err
	}
	if current, err := authoritysql.ReadPrivateControlFile(o.maintenancePublicationFile, 16<<20); err != nil || !bytes.Equal(publicationRaw, current) {
		return errors.Join(drainproof.ErrHold, err)
	}
	var instance [16]byte
	if _, err := rand.Read(instance[:]); err != nil {
		return err
	}
	planSHA := sha256.Sum256(frozen.PlanProto)
	ended := uint64(time.Now().UnixMilli())
	if ended < started {
		return drainproof.ErrHold
	}
	audit := &pb.SqlWriterMaintenanceReceipt_MergeRecoveryAudit{AuditInstanceId: instance[:], RunId: frozen.RunId,
		Generation: frozen.Generation, AttemptRevision: frozen.AttemptRevision, PlanDigest: planSHA[:], OriginalFreezeReceiptId: frozen.RequestId,
		OriginalFreezeReceiptDigest: frozen.ReceiptDigest, MaintenancePublication: publication, MergeManifest: manifestPin,
		MergeRunId: manifest.RunID, SourceZone: cfg.src, TargetZone: cfg.dst, VerifyMerged: true, ObservedStartMs: started,
		ObservedEndMs: ended, AuditSourcePins: sources}
	for _, row := range rows {
		audit.Checks = append(audit.Checks, &pb.SqlWriterMaintenanceReceipt_MergeRecoveryCheck{Name: row.Name, SourceCount: row.SourceCount,
			TargetCount: row.TargetCount, UniqueScope: row.UniqueScope, ConflictCount: row.ConflictCount, Severity: row.Severity,
			Status: pb.SqlWriterMaintenanceReceipt_MergeRecoveryCheck_COMPLETE, Notes: row.Notes, QuerySourcePins: sources, Kind: pb.SqlWriterMaintenanceReceipt_MergeRecoveryCheck_OBSERVATION})
	}
	// 所有只读资源真实Close成功后才发布P7原件；失败不能留下一个看似可释放的文件。
	for i := len(resources) - 1; i >= 0; i-- {
		err = errors.Join(err, resources[i].Close())
	}
	resources = nil
	if err != nil {
		return err
	}
	raw, err := (protojson.MarshalOptions{EmitUnpopulated: true}).Marshal(audit)
	if err != nil {
		return err
	}
	return authoritysql.WritePrivateControlFile(o.recoveryAuditOutput, raw)
}

func mergeRecoveryQueryPins(plan *pb.SqlWriterMaintenanceReceipt_Plan) ([]*pb.SqlWriterMaintenanceReceipt_SourcePin, error) {
	if plan == nil {
		return nil, drainproof.ErrHold
	}
	required := []string{"main.go", "audit_resources.go", "audit_checks.go", "trade_step.go", "route_store.go", "route_publish.go", "manifest.go"}
	result := []*pb.SqlWriterMaintenanceReceipt_SourcePin{}
	for _, name := range required {
		var selected *pb.SqlWriterMaintenanceReceipt_SourcePin
		for _, pin := range plan.SourcePins {
			if pin != nil && strings.HasSuffix(strings.ReplaceAll(pin.AbsolutePath, "\\", "/"), "/tools/merge_zone/"+name) {
				if selected != nil {
					return nil, drainproof.ErrHold
				}
				selected = pin
			}
		}
		if selected == nil {
			return nil, fmt.Errorf("P0缺少真实审核/查询源码pin: %s", name)
		}
		if _, err := drainproof.ReadPin(selected, 16<<20); err != nil {
			return nil, err
		}
		result = append(result, selected)
	}
	return result, nil
}

func verifyMergeAuditRedisTarget(ctx context.Context, client *redis.Client, plan *pb.SqlWriterMaintenanceReceipt_Plan) error {
	if client == nil || plan == nil {
		return drainproof.ErrHold
	}
	info, err := client.Info(ctx, "server").Result()
	if err != nil {
		return err
	}
	runID := ""
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "run_id:") {
			if runID != "" {
				return drainproof.ErrHold
			}
			runID = strings.TrimPrefix(line, "run_id:")
		}
	}
	if len(runID) != 40 || !routeLowerHexOfLen(runID, 40, 40) {
		return drainproof.ErrHold
	}
	opts := client.Options()
	for _, target := range plan.RedisTargets {
		if target != nil && target.Address == opts.Addr && target.Db == int32(opts.DB) && bytes.Equal(target.ServerRunId, []byte(runID)) {
			return nil
		}
	}
	return fmt.Errorf("审核Redis目标/DB/实际server run_id不在固定P0目录")
}
