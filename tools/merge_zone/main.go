package main

// merge_zone —— 停服维护窗口内把 source zone 合并进 target zone(合服)。
//
// ── Redis DB 分布(实地核对 2026-09-08;写错库 = 静默无效)────────────────
//
//	mapping  DB 0   player:zone:{id} / lock:player:{id} / merge:in_progress:{zone}
//	                go/data_service/etc/data_service.yaml → MappingRedis(Host/Type only;
//	                go-zero RedisConf 没有 DB 字段 ⇒ 恒 DB 0)
//	guild    DB 2   guild_rank:zone:{z} / guild:v2:{id} / guild_rank:maintenance_lock
//	                go/guild/etc/guild.yaml → RedisClient.DB
//	friend   DB 3   friend:online:{pid}
//	                go/friend/etc/friend.yaml → RedisClient.DB
//	shared   DB 0   player:session:{pid} / kafka:{retry,processing,dead}:queue:* /
//	                PlayerAllData:{pid} / {MsgType}:{pid} / player_merge_notice:{pid}
//	                go/login / go/db / go/player_locator / scene_manager 都是 DB 0
//	data     独立    player:{id}:*(data_service 的按 region 分的 Redis Cluster)
//
// ── 步骤顺序(顺序本身是正确性的一部分)────────────────────────────────
//
//	P  preflight    源/目标库存在、源区无活节点、Kafka 无积压、无锁、无会话
//	F  fence        merge:in_progress:{src} 与 {dst} 打标(见 fence.go 的契约)
//	0  collect      扫一次 mapping 拿到源区玩家 id;之后所有步骤复用这一份
//	G  guard        空集合 / 映射不全 → 拒绝(见 -allow-empty-source)
//	M  manifest     **在任何写之前**落盘清单(玩家/公会/ZSET 成员/表)
//	1  player_rows  zone_src_db → zone_dst_db 逐表拷贝 + 共享缓存失效  ← 最关键
//	2  player_blobs data Redis 的 player:{id}:* 拷贝(仅多集群)
//	3  guild_mysql  guild.zone_id 改写 + guild:v2 缓存失效
//	4  guild_rank   guild_rank:zone ZSET 合并(maintenance_lock + MULTI/EXEC)
//	5  player_mapping player:zone:{id} 改写            ← 必须在 1/2 之后
//	6  hot_state    scene_manager 源区热状态清理(可选)
//	7  post_merge_flag  player_merge_notice:{pid} 打进 **login Redis(DB 0)**
//
// 1 必须在 5 之前:mapping 一改,玩家就被路由到目标区,而他的行还在源库。
// 5 一旦跑完,collectPlayerIDsWithHomeZone 再也找不到这批人 —— 这正是清单
// 存在的理由(重跑读清单,不重新扫描)。

import (
	"flag"
	"fmt"
	"os"
	"time"
)

const playerZoneKeyPrefix = "player:zone:"

// 默认值集中在这里,便于与各服务 yaml 对照。
const (
	// data_service 的 mapping Redis **一定是 DB 0**:它用 go-zero 的
	// redis.MustNewRedis(config.MappingRedis),而 go-zero 的 RedisConf
	// (v1.10.0 core/stores/redis/conf.go)只有 Host/Type/User/Pass/Tls/
	// NonBlock/PingTimeout —— **没有 DB 字段**,yaml 里写 `DB: 15` 会被
	// 反序列化直接忽略。data_service.yaml 里那行 inert 的 DB 已删除。
	// 把这里改成 15 会让 fence 与 remap 都指向 data_service 从不碰的库:
	// fence 失效、remap 报告成功却一个 key 都没改 —— 正是合服最危险的静默失败。
	defaultMappingRedisDB = 0
	defaultGuildRedisDB   = 2 // guild.yaml RedisClient.DB
	defaultFriendRedisDB  = 3 // friend.yaml RedisClient.DB
	defaultSharedRedisDB  = 0 // login / db / player_locator / scene_manager
	// go/db/etc/db.yaml → Kafka.GroupID
	defaultKafkaGroup = "db_rpc_consumer_group"
	// 导表器产物;仓库根的相对路径(dev_tools.ps1 在仓库根跑 go run)。
	defaultTableListPath = "generated/data/mysql_database_table_list.json"
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

	friendAddr string
	friendPwd  string
	friendDB   int

	sceneAddr string
	scenePwd  string
	sceneDB   int

	sourceData   string
	targetData   string
	dataPwd      string
	sourceDataDB int
	targetDataDB int

	dryRun bool
	apply  bool

	skipGuild     bool
	skipRank      bool
	skipMapping   bool
	skipRows      bool
	knowGlobalPT  bool
	migrateBlobs  bool
	skipBlobs     bool
	clearHotState bool

	allowEmptySource   bool
	expectedSrcPlayers int64
	manifestPath       string
	tableListPath      string

	kafkaGroup         string
	kafkaTopicGen      uint
	kafkaCLI           string
	kafkaBootstrap     string
	assumeKafkaDrained bool

	timeout time.Duration

	backfill     bool
	backfillZone uint32
}

func main() {
	var o options

	flag.StringVar(&o.mode, "mode", "merge",
		"Operation mode: 'merge' (default), 'audit' (read-only, audit_resources.go), or 'unmerge' (reverse a run from its manifest)")
	flag.BoolVar(&o.verifyMerged, "verify-merged", false,
		"Audit-only: run the post-merge verification assertions (runbook §5 Step 5) instead of the pre-merge gates")

	srcZone := flag.Uint("source-zone", 0, "Source zone ID to merge FROM (required)")
	dstZone := flag.Uint("target-zone", 0, "Target zone ID to merge INTO (required)")
	flag.StringVar(&o.mysqlDSN, "mysql-dsn",
		"root:@tcp(127.0.0.1:3306)/mmorpg?charset=utf8mb4&parseTime=true&loc=Local",
		"MySQL DSN. Must reach BOTH the global tables (guild/friend) and the per-zone databases zone_<N>_db")

	flag.StringVar(&o.redisAddr, "redis-addr", "127.0.0.1:6379", "Default Redis address (used when a specific -*-redis-addr is empty)")
	flag.StringVar(&o.redisPwd, "redis-password", "", "Default Redis password")
	flag.IntVar(&o.redisDB, "redis-db", defaultGuildRedisDB, "GUILD Redis DB (guild_rank:zone / guild:v2 / guild_rank:maintenance_lock; guild.yaml default 2)")

	flag.StringVar(&o.mappingAddr, "mapping-redis-addr", "", "Player mapping Redis address (player:zone:*). Default: -redis-addr")
	flag.StringVar(&o.mappingPwd, "mapping-redis-password", "", "Player mapping Redis password. Default: -redis-password")
	flag.IntVar(&o.mappingDB, "mapping-redis-db", defaultMappingRedisDB,
		"MAPPING Redis DB. Always 0: go-zero's RedisConf has no DB field, so data_service's MappingRedis "+
			"lands on DB 0 no matter what the yaml says. Scanning the wrong DB silently remaps nothing")

	flag.StringVar(&o.noticeAddr, "notice-redis-addr", "", "LOGIN Redis address for player_merge_notice:{pid}. Default: -redis-addr")
	flag.StringVar(&o.noticePwd, "notice-redis-password", "", "LOGIN Redis password. Default: -redis-password")
	flag.IntVar(&o.noticeDB, "notice-redis-db", defaultSharedRedisDB,
		"LOGIN Redis DB (login.yaml Node.RedisClient.DB — 0). This handle also covers player:session:* and the go/db kafka retry/dead queues")

	flag.StringVar(&o.friendAddr, "friend-redis-addr", "", "FRIEND Redis address for friend:online:{pid}. Default: -redis-addr")
	flag.StringVar(&o.friendPwd, "friend-redis-password", "", "FRIEND Redis password. Default: -redis-password")
	flag.IntVar(&o.friendDB, "friend-redis-db", defaultFriendRedisDB, "FRIEND Redis DB (friend.yaml RedisClient.DB — 3)")

	flag.StringVar(&o.sceneAddr, "scene-redis-addr", "", "scene_manager Redis address (scene_nodes / player:{id}:location / world_channels). Default: -redis-addr")
	flag.StringVar(&o.scenePwd, "scene-redis-password", "", "scene_manager Redis password. Default: -redis-password")
	flag.IntVar(&o.sceneDB, "scene-redis-db", defaultSharedRedisDB, "scene_manager Redis DB (scene_manager_service.yaml Redis — 0)")

	flag.StringVar(&o.sourceData, "source-data-redis", "", "Comma-separated addr(s) for the SOURCE zone player data Redis (standalone or cluster)")
	flag.StringVar(&o.targetData, "target-data-redis", "", "Comma-separated addr(s) for the TARGET zone player data Redis")
	flag.StringVar(&o.dataPwd, "data-redis-password", "", "Password for source/target data Redis (overrides -redis-password when set)")
	flag.IntVar(&o.sourceDataDB, "source-data-redis-db", 0, "Source data Redis DB index (standalone only; cluster ignores)")
	flag.IntVar(&o.targetDataDB, "target-data-redis-db", 0, "Target data Redis DB index (standalone only; cluster ignores)")

	flag.BoolVar(&o.dryRun, "dry-run", false, "Print actions without writing")
	flag.BoolVar(&o.apply, "apply", false, "Required for any write")

	flag.BoolVar(&o.skipGuild, "skip-guild-mysql", false, "Skip the guild.zone_id update + guild cache invalidation")
	flag.BoolVar(&o.skipRank, "skip-guild-rank", false, "Skip the guild_rank:zone ZSET merge")
	flag.BoolVar(&o.skipMapping, "skip-player-mapping", false, "Skip the player:zone remapping")
	flag.BoolVar(&o.skipRows, "skip-player-rows", false,
		"Skip copying player rows from zone_<src>_db to zone_<dst>_db. ONLY valid once player main data is global (TiDB Phase 2); requires -i-know-global-player-table")
	flag.BoolVar(&o.knowGlobalPT, "i-know-global-player-table", false,
		"Attest that player main data is NOT per-zone any more (TiDB Phase 2 landed). Required with -skip-player-rows")
	flag.BoolVar(&o.migrateBlobs, "migrate-player-blobs", false, "Copy player:{id}:* keys between per-zone data Redis clusters (multi-cluster deployments only)")
	flag.BoolVar(&o.skipBlobs, "skip-player-blob-migration", false, "Skip the blob step even if -migrate-player-blobs was passed (emergency)")
	flag.BoolVar(&o.clearHotState, "clear-source-hot-state", false,
		"After remap: DEL source-zone player:{id}:location / scene:* / world_channels / node_load in the scene_manager Redis. Refuses unless scene_nodes:zone:{S}:load is empty")

	flag.BoolVar(&o.allowEmptySource, "allow-empty-source", false,
		"Allow -apply when the source zone maps zero players (otherwise refused: an empty mapping usually means the wrong -mapping-redis-db or a missing -backfill-home-zone)")
	flag.Int64Var(&o.expectedSrcPlayers, "expected-src-players", -1,
		"Player count recorded at the T-1 rehearsal. -apply refuses if the live count differs; -verify-merged uses it as the lower bound on target-zone rows")
	flag.StringVar(&o.manifestPath, "manifest-path", "",
		"Manifest file written before the first write (default ./merge_<src>_to_<dst>_<ts>.json). Re-runs and -mode unmerge consume it")
	flag.StringVar(&o.tableListPath, "table-list-json", defaultTableListPath,
		"go/db table list JSON used to discover the per-zone player tables")

	flag.StringVar(&o.kafkaGroup, "kafka-group", defaultKafkaGroup, "go/db consumer group id (db.yaml Kafka.GroupID) used for the lag pre-flight")
	flag.UintVar(&o.kafkaTopicGen, "kafka-topic-generation", 1, "go/db Kafka.TopicGeneration; topic = db_task_zone_<src>[_g<gen>]")
	flag.StringVar(&o.kafkaCLI, "kafka-consumer-groups-cmd", "", "Path to kafka-consumer-groups(.sh|.bat) for the lag pre-flight")
	flag.StringVar(&o.kafkaBootstrap, "kafka-bootstrap", "127.0.0.1:9092", "Kafka bootstrap server for -kafka-consumer-groups-cmd")
	flag.BoolVar(&o.assumeKafkaDrained, "assume-kafka-drained", false,
		"Operator attestation that the db_task backlog is drained (no lag is measured). Only for environments without the Kafka CLI")

	flag.DurationVar(&o.timeout, "timeout", 2*time.Hour, "Overall run timeout. The merge fence TTL is derived from it (timeout + 30m)")

	flag.BoolVar(&o.backfill, "backfill-home-zone", false,
		"Standalone mode: SET NX player:zone:{id}=<zone> for every row in zone_<zone>_db.player_database. Run this for EVERY existing zone before the first merge")
	bfZone := flag.Uint("zone", 0, "Zone ID for -backfill-home-zone (required in that mode)")

	flag.Parse()
	o.sourceZone = uint32(*srcZone)
	o.targetZone = uint32(*dstZone)
	o.backfillZone = uint32(*bfZone)

	// 未指定的专用地址 / 密码回落到默认 Redis。DB 号**不**回落:
	// 每个 DB 都有 yaml 里的确定值,猜错就是静默无效。
	o.mappingAddr = firstNonEmpty(o.mappingAddr, o.redisAddr)
	o.mappingPwd = firstNonEmpty(o.mappingPwd, o.redisPwd)
	o.noticeAddr = firstNonEmpty(o.noticeAddr, o.redisAddr)
	o.noticePwd = firstNonEmpty(o.noticePwd, o.redisPwd)
	o.friendAddr = firstNonEmpty(o.friendAddr, o.redisAddr)
	o.friendPwd = firstNonEmpty(o.friendPwd, o.redisPwd)
	o.sceneAddr = firstNonEmpty(o.sceneAddr, o.redisAddr)
	o.scenePwd = firstNonEmpty(o.scenePwd, o.redisPwd)
	o.dataPwd = firstNonEmpty(o.dataPwd, o.redisPwd)

	// ── Backfill mode:先于 merge 的 source/target 检查分发。
	if o.backfill {
		if o.backfillZone == 0 {
			fail("-backfill-home-zone requires -zone <non-zero>")
		}
		if err := requireWriteIntent(o); err != nil {
			fail("%v", err)
		}
		runBackfillEntry(backfillEntryParams{
			zone:        o.backfillZone,
			mysqlDSN:    o.mysqlDSN,
			mappingAddr: o.mappingAddr,
			mappingPwd:  o.mappingPwd,
			mappingDB:   o.mappingDB,
			dryRun:      o.dryRun,
		})
		return
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

	switch o.mode {
	case "audit":
		tables, err := loadTableListJSON(o.tableListPath)
		if err != nil {
			// 审计不因为清单缺失而整个失败:MySQL 无关的检查仍然有价值。
			// 但要说清楚,不能让人以为表检查跑过了。
			fmt.Fprintf(os.Stderr, "WARN: %v — verify:target_zone_rows will report INFRA\n", err)
		}
		runAuditEntry(auditEntryParams{
			src:                o.sourceZone,
			dst:                o.targetZone,
			mysqlDSN:           o.mysqlDSN,
			mappingAddr:        o.mappingAddr,
			mappingPwd:         o.mappingPwd,
			mappingDB:          o.mappingDB,
			rankAddr:           o.redisAddr,
			rankPwd:            o.redisPwd,
			rankDB:             o.redisDB,
			friendAddr:         o.friendAddr,
			friendPwd:          o.friendPwd,
			friendDB:           o.friendDB,
			sharedAddr:         o.noticeAddr,
			sharedPwd:          o.noticePwd,
			sharedDB:           o.noticeDB,
			sceneAddr:          o.sceneAddr,
			scenePwd:           o.scenePwd,
			sceneDB:            o.sceneDB,
			dataAddrs:          addrsFromCSV(o.sourceData),
			dataPwd:            o.dataPwd,
			dataDB:             o.sourceDataDB,
			verifyMerged:       o.verifyMerged,
			expectedSrcPlayers: o.expectedSrcPlayers,
			tableCandidates:    tables,
			topicGeneration:    uint32(o.kafkaTopicGen),
		})
		return

	case "unmerge":
		if err := requireWriteIntent(o); err != nil {
			fail("%v", err)
		}
		if o.manifestPath == "" {
			fail("-mode unmerge requires -manifest-path <file written by the original run>")
		}
		runUnmerge(o)
		return

	case "merge":
		if err := requireWriteIntent(o); err != nil {
			fail("%v", err)
		}
		if o.migrateBlobs && (o.sourceData == "" || o.targetData == "") {
			fail("-migrate-player-blobs requires -source-data-redis and -target-data-redis")
		}
		if o.skipRows && !o.knowGlobalPT {
			fail("-skip-player-rows drops the player main-data copy. Player data is per-zone today " +
				"(C++ scene → Kafka db_task_zone_{N} → go/db → MySQL zone_{N}_db); skipping it silently empties " +
				"every merged account. Pass -i-know-global-player-table only if the TiDB global player layer is live")
		}
		runMerge(o)
		return

	default:
		fail("unknown -mode %q (expected 'merge', 'audit' or 'unmerge')", o.mode)
	}
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
