package main

// merge_zone —— 停服维护窗口内把 source zone 合并进 target zone(合服)。
//
// ── Redis DB 分布(实地核对 2026-09-08;写错库 = 静默无效)────────────────
//
//	mapping  DB 0   player:zone:{id} / lock:player:{id} / merge:in_progress:{zone}
//	                player:placement:{id} / db:capability:zone:{z}(落点记录与 go/db 能力标记,
//	                player-storage-placement.md §4;go/db 的 Placement.Redis 指向这里)
//	                go/data_service/etc/data_service.yaml → MappingRedis(Host/Type only;
//	                go-zero RedisConf 没有 DB 字段 ⇒ 恒 DB 0)
//	guild    DB 2   guild_rank:zone:{z} / guild:v2:{id} / guild_rank:maintenance_lock
//	                go/guild/etc/guild.yaml → RedisClient.DB
//	shared   DB 0   player:session:{pid} / kafka:{retry,processing,dead}:queue:* /
//	                distributed:lock:kafka:ordering:*(go/db 排序锁,搬库 R2 等它)/
//	                PlayerAllData:{pid} / {MsgType}:{pid} / player_merge_notice:{pid}
//	                go/login / go/db / go/player_locator / scene_manager 都是 DB 0
//	data     独立    player:{id}:*(data_service 的按 region 分的 Redis Cluster)
//
// ── 步骤顺序(顺序本身是正确性的一部分;与 merge_run.go 的 runMerge 逐段对应)────
//
//	P1 preflight    源/目标 zone 库存在、发现玩家表、trade / guild 库表就绪(只读,不需要围栏)
//	R  resume       读既有清单:dry-run 预览一律拒读;src/dst 与本次一致(拿错清单在围栏之前即拒绝)
//	C  capability   首跑 pin 模式:-db-capability-zones(必填、无缺省;none = 不查)列出的每个 zone 都有 go/db 能力标记
//	               (只读,不需要围栏)。T-0 时 src / dst 已 zone-down,只能列仍在跑的 zone;dst 在 zone-up 之后用
//	               -mode capability-check 核对(runbook §8 Step 6)
//	F  fence        merge:in_progress:{src} 与 {dst} 打标(见 fence.go 的契约)—— 先于收集与预检
//	R2 resume check 续跑:玩家行模式必须一致、copy 模式下玩家表集合不得变化、pin 模式核对能力标记(围栏之下)
//	0  collect      扫一次 mapping 拿到源区玩家 id(续跑读清单,不重扫);之后所有步骤复用这一份
//	G  guard        空集合 / 源库有行却没有任何映射 → 拒绝(见 -allow-empty-source)
//	P2-P7 preflight 源区无活节点、Kafka 无积压、重试队列空、无锁、无会话、不在活队伍里
//	N  guild name   公会重名断言(冲突时一个字节都不写)
//	S  placement    清单玩家的落点记录:有冻结(搬库中)/ 畸形即拒绝;记下已有记录(copy 模式有记录者时
//	               -db-capability-zones 同样必填并照 C 的口径核对)
//	X  merged_into  源区已被合进别的 zone / 目标区自己已被合走(merge:merged_into:*)即拒绝(A10,先于任何写)
//	M  manifest     **在任何写之前**落盘清单(玩家/公会/ZSET 成员/表/商品/模式/已有落点);dry-run 只写 <path>.dryrun.json
//	1  player_rows  copy:撞号预检(全部表)→ 无落点记录者 zone_src_db → zone_dst_db 逐表拷贝 + 共享缓存失效  ← 最关键
//	1  pin_placement pin:无落点记录者钉 player:placement = "{src}:1"(不拷行、不拷 blob、不删缓存)
//	2  player_blobs data Redis 的 player:{id}:* 拷贝(仅多集群)
//	3  guild_mysql  guild.zone_id 按清单逐条改写 + guild:v2 缓存失效 + 复查源区
//	3b trade_mysql  mmorpg_trade.trade_listing.market_zone 按清单逐条改写 + 复查(seller_zone_at_listing 不改)
//	4  guild_rank   guild_rank:zone ZSET 合并(maintenance_lock 内重读源榜 + MULTI/EXEC)
//	5  player_mapping 先写 merge:merged_into:{src},再按清单逐键 CAS player:zone src→dst  ← 必须在 1/2 之后
//	6  hot_state    scene_manager 源区热状态清理(可选)
//	7  post_merge_flag  player_merge_notice:{pid} 打进 **login Redis(DB 0)**
//
// F 之后、M 之前的拒绝:首跑时本次什么都没写,正常释放围栏、非零退出(mergeFence.refuseAndRelease);
// 续跑(清单已存在且步骤 7 未标记完成)时上一次运行可能已写到一半,围栏保留、走 fenceKeptAbortMessage。
// M 之后的失败:围栏保留在本次 run_id 上,按打印的指引核对 → DEL → 用原命令续跑(fenceKeptAbortMessage)。
//
// 1 必须在 5 之前:mapping 一改,玩家就被路由到目标区,而他的行还在源库。
// 5 一旦跑完,collectPlayerIDsWithHomeZone 再也找不到这批人 —— 这正是清单
// 存在的理由(重跑读清单,不重新扫描)。
//
// ── 玩家行模式 -player-rows-mode(docs/design/player-storage-placement.md §10)────────
//
//	pin(默认) 合服只改归属,不搬玩家行:R 之后先核对能力标记(C,-db-capability-zones 列出的每个 zone 的 go/db
//	           已按落点选库;该参数没有缺省值,必须写仍在跑的 zone 或 none);步骤 1 换成 pin_placement —— 清单玩家无落点记录者钉 "{src}:1",有效落点
//	           保持源区库;不拷行、不拷 blob、不删玩家缓存。
//	copy       旧语义,兼容还没升级的 go/db:照旧拷行,但只处理没有落点记录的玩家(有记录者的行在其落点库)。
//	           清单玩家里有记录者时,同样要求能力标记(处理他们存盘的 go/db 必须按记录选库)。
//
// ── -db-capability-zones(§4.3,没有缺省值)──────────────────────────
//
//	能力标记是 90s 心跳,只证明「最近 90s 这个 zone 有新版 go/db 在跑」。值是逗号分隔的 zone 号,或字面量 none
//	(「此刻没有别的 zone 在跑,不检查」)。旧的缺省 "src,dst" 已删除:T-0 时 src / dst 已 zone-down,必然被拒。
//	  pin 合服 / pin 撤销 / copy 合服中清单玩家已有记录  必填,接受 none;列出的 src / dst 照常检查(已下线就被拒)
//	  copy 撤销                                          可选,给了就查
//	  -mode relocate                                      必填,不接受 none(搬库在线进行,在跑的 zone 都要列)
//	  -mode capability-check                              必填,不接受 none
//
// 两种模式都在 N 之后、M 之前扫一遍清单玩家的落点记录(S):有冻结记录(搬库进行中)即拒绝,与 -mode relocate
// 互斥(§9 末条);清单记下模式与已有记录,续跑必须同模式。
//
// ── 其余模式 ────────────────────────────────────────────────
//
//	-mode unmerge          按清单撤销(unmerge.go);pin 模式只改回 home,不动落点
//	-mode audit            只读审计 / -verify-merged 合服后验证(audit_*.go)
//	-mode pin-placement    -zone N:给 home==N 且无记录的玩家钉 "{N}:1"(pin_placement.go,在线安全)
//	-mode relocate         冻结式搬库 S → T(relocate*.go)
//	-mode relocate-abort   把一次搬库仍冻结的玩家解冻回 S(relocate.go)
//	-mode storage-audit    -storage N:有效落点为 N 的玩家数与 N 库冷副本(storage_audit.go,只读)
//	-mode capability-check -db-capability-zones <list>:逐 zone 报告能力标记 present / missing / unreadable,
//	                       exit 0 / 1 / 2(capability_check.go,只读;合服后 dst zone-up、开服之前必跑)
//	-backfill-home-zone    -zone N:回填 player:zone(backfill_home_zone.go)

import (
	"flag"
	"fmt"
	"math"
	"os"
	"strconv"
	"time"
)

const playerZoneKeyPrefix = "player:zone:"

// playerZoneKey 返回 player:zone:{id}(data_service 维护的 home_zone 映射,mapping Redis DB 0)。
func playerZoneKey(id uint64) string {
	return playerZoneKeyPrefix + strconv.FormatUint(id, 10)
}

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
	// friend 的 DB 3 已删(2026-09-18,friend 移植 F3):它唯一服务的键 friend:online:{pid}
	// 全仓没有写者、读者也已删除,审计改用 shared 的 player:session 判在线。
	defaultSharedRedisDB = 0 // login / db / player_locator / scene_manager
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
	skipTrade     bool
	tradeSchema   string
	guildSchema   string
	friendSchema  string

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

	backfill bool
	// zone 是 -zone:-backfill-home-zone 与 -mode pin-placement 的对象 zone。
	zone uint32

	// playerRowsMode 是 -player-rows-mode(pin / copy),只对 -mode merge 有意义;续跑 / 撤销 / 验证按清单记录的模式。
	playerRowsMode string
	// capZonesRaw 是 -db-capability-zones 原文;capSpec 是解析结果(没有缺省值,是否必填、能否 none 按模式定,
	// 见文件头「-db-capability-zones」与 requireCapabilityZones)。
	capZonesRaw string
	capSpec     capabilityZoneSpec

	relocatePlayerIDs string
	relocateSource    uint32
	relocateTarget    uint32
	relocateBatch     int
	relocateLockWait  time.Duration

	auditStorage uint32
}

func main() {
	var o options

	flag.StringVar(&o.mode, "mode", "merge",
		"Operation mode: 'merge' (default), 'audit' (read-only, audit_resources.go), 'unmerge' (reverse a run from its manifest; "+
			"runs the pre-flight gates against the TARGET zone first, so it needs -kafka-consumer-groups-cmd or -assume-kafka-drained too), "+
			"'pin-placement' (-zone N: pin \"N:1\" for players with home_zone=N and no placement record; safe online), "+
			"'relocate' (move player main data between placement stores: -relocate-source-storage / -relocate-target-storage), "+
			"'relocate-abort' (unfreeze what a relocate run left frozen: -manifest-path) or "+
			"'storage-audit' (read-only: -storage N — who is still placed on storage N, and its cold copies) or "+
			"'capability-check' (read-only: -db-capability-zones <list> — per-zone go/db capability marker present / missing / "+
			"unreadable, exit 0 / 1 / 2; run it for dst after its zone-up, before opening it)")
	flag.StringVar(&o.playerRowsMode, "player-rows-mode", playerRowsModePin,
		"Merge only: 'pin' (default) re-homes players without moving their rows — players without a placement record get "+
			"player:placement=\"<src>:1\" before the remap, and every -db-capability-zones zone must run a placement-aware go/db; "+
			"'copy' copies rows zone_<src>_db → zone_<dst>_db like before (players that already have a placement record are skipped). "+
			"A resumed run must use the manifest's mode; -mode unmerge and -verify-merged read the mode from the manifest")
	flag.StringVar(&o.capZonesRaw, "db-capability-zones", "",
		"Zones whose go/db must carry db:capability:zone:<z>=placement-routing-v1 (a 90s heartbeat: it only proves a "+
			"placement-aware go/db ran there within the last 90s). NO DEFAULT: comma-separated zone ids, or 'none' = no other "+
			"zone is running, check nothing. Required for a pin merge, a pin-mode unmerge and a copy merge whose manifest "+
			"players already have a placement record (these accept 'none'); src/dst went down at T-0, so list only the zones "+
			"still running and check dst with -mode capability-check after its zone-up. Required for -mode relocate (every "+
			"running zone; 'none' refused; the relocated players' home zones are added automatically) and -mode capability-check "+
			"('none' refused). The old 'src'/'dst' shorthands are refused")
	flag.StringVar(&o.relocatePlayerIDs, "relocate-player-ids", "",
		"-mode relocate: comma-separated player ids to move. Empty = every player whose effective placement is -relocate-source-storage")
	relocSrc := flag.Uint("relocate-source-storage", 0,
		"-mode relocate: storage id S the players are moved FROM (1..999999 = zone_<S>_db, >=1000000 = player_store_<S>_db). "+
			"Only players whose effective placement is S are frozen")
	relocDst := flag.Uint("relocate-target-storage", 0, "-mode relocate: storage id T the players are moved TO")
	flag.IntVar(&o.relocateBatch, "relocate-batch-size", 100,
		"-mode relocate / relocate-abort: players frozen, copied and switched together (R1..R5 run per batch)")
	flag.DurationVar(&o.relocateLockWait, "relocate-lock-wait", defaultRelocateLockWait,
		"-mode relocate: how long R2 waits for the go/db ordering locks of a frozen batch to clear before unfreezing the whole batch "+
			"(default a bit above go/db's 2m ordering-lock TTL)")
	auditStorage := flag.Uint("storage", 0, "-mode storage-audit: the storage id to audit")
	flag.BoolVar(&o.verifyMerged, "verify-merged", false,
		"Audit-only: run the post-merge verification assertions (runbook §5 Step 5) instead of the pre-merge gates. "+
			"Requires -manifest-path: every manifest player is checked one by one")

	srcZone := flag.Uint("source-zone", 0, "Source zone ID to merge FROM (required)")
	dstZone := flag.Uint("target-zone", 0, "Target zone ID to merge INTO (required)")
	flag.StringVar(&o.mysqlDSN, "mysql-dsn",
		"root:@tcp(127.0.0.1:3306)/mmorpg?charset=utf8mb4&parseTime=true&loc=Local",
		"MySQL DSN. Must reach the account tables, the per-zone databases zone_<N>_db, "+
			"and the service-owned databases -guild-schema / -trade-schema / -friend-schema on the same instance")

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

	// -friend-redis-addr / -friend-redis-password / -friend-redis-db 已删(2026-09-18,
	// friend 移植 F3)。它们只为 friend:online:{pid} 存在,而那把键全仓没有写者、读者也已删除;
	// 在线门禁改看 player:session(走 -notice-redis-* 那个共享 DB 0 句柄)。
	// ⚠ 还在用旧参数的运维脚本会因「flag provided but not defined」直接退出 —— 这是刻意的:
	// 静默忽略一个已删参数,会让人以为自己配的 friend 库真的被查过。

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
	flag.BoolVar(&o.skipTrade, "skip-trade-mysql", false,
		"Skip the trade_listing.market_zone rewrite (jubaozhai). ONLY for environments where the trade service was never deployed. "+
			"Without it a missing trade schema/table refuses the merge; with it -verify-merged reports the trade row as NOT VERIFIED")
	flag.StringVar(&o.tradeSchema, "trade-schema", defaultTradeSchema,
		"trade database reached through -mysql-dsn (go/trade forces MySQL.DBName=mmorpg_trade; override only for isolated tests)")
	flag.StringVar(&o.guildSchema, "guild-schema", defaultGuildSchema,
		"guild database reached through -mysql-dsn (go/guild forces the DSN database to be mmorpg_guild; override only for isolated tests)")
	flag.StringVar(&o.friendSchema, "friend-schema", defaultFriendSchema,
		"friend database reached through -mysql-dsn (go/friend forces MySQL.DBName=mmorpg_friend; override only for isolated tests)")
	flag.BoolVar(&o.skipMapping, "skip-player-mapping", false, "Skip the player:zone remapping")
	flag.BoolVar(&o.skipRows, "skip-player-rows", false,
		"-player-rows-mode copy only: skip copying player rows from zone_<src>_db to zone_<dst>_db. ONLY valid once player main data is "+
			"global (TiDB Phase 2); requires -i-know-global-player-table. Pin mode never copies rows and refuses this flag")
	flag.BoolVar(&o.knowGlobalPT, "i-know-global-player-table", false,
		"Attest that player main data is NOT per-zone any more (TiDB Phase 2 landed). Required with -skip-player-rows")
	flag.BoolVar(&o.migrateBlobs, "migrate-player-blobs", false, "Copy player:{id}:* keys between per-zone data Redis clusters (multi-cluster deployments only)")
	flag.BoolVar(&o.skipBlobs, "skip-player-blob-migration", false, "Skip the blob step even if -migrate-player-blobs was passed (emergency)")
	flag.BoolVar(&o.clearHotState, "clear-source-hot-state", false,
		"After remap: DEL source-zone player:{id}:location / scene:* / world_channels / node_load in the scene_manager Redis. Refuses unless scene_nodes:zone:{S}:load is empty")

	flag.BoolVar(&o.allowEmptySource, "allow-empty-source", false,
		"Allow -apply when the source zone maps zero players (otherwise refused: an empty mapping usually means the wrong -mapping-redis-db or a missing -backfill-home-zone)")
	flag.Int64Var(&o.expectedSrcPlayers, "expected-src-players", -1,
		"Player count recorded at the T-1 rehearsal. -apply refuses if the live count differs; -verify-merged cross-checks it against the manifest's player count")
	flag.StringVar(&o.manifestPath, "manifest-path", "",
		"Manifest file written before the first write (merge default ./merge_<src>_to_<dst>_<ts>.json; relocate default "+
			"./relocate_<S>_to_<T>_<ts>.json). Re-runs, -mode unmerge, -verify-merged and -mode relocate-abort consume it. "+
			"-dry-run never writes it: its preview goes to <path>.dryrun.json, which nothing consumes")
	flag.StringVar(&o.tableListPath, "table-list-json", defaultTableListPath,
		"go/db table list JSON used to discover the per-zone player tables")

	flag.StringVar(&o.kafkaGroup, "kafka-group", defaultKafkaGroup, "go/db consumer group id (db.yaml Kafka.GroupID) used for the lag pre-flight")
	flag.UintVar(&o.kafkaTopicGen, "kafka-topic-generation", 1,
		"go/db Kafka.TopicGeneration; topic = db_task_zone_<zone>[_g<gen>] (merge lag pre-flight; relocate R2 waits on the "+
			"go/db ordering locks of each player's home-zone topic)")
	flag.StringVar(&o.kafkaCLI, "kafka-consumer-groups-cmd", "", "Path to kafka-consumer-groups(.sh|.bat) for the lag pre-flight")
	flag.StringVar(&o.kafkaBootstrap, "kafka-bootstrap", "127.0.0.1:9092", "Kafka bootstrap server for -kafka-consumer-groups-cmd")
	flag.BoolVar(&o.assumeKafkaDrained, "assume-kafka-drained", false,
		"Operator attestation that the db_task backlog is drained (no lag is measured). Only for environments without the Kafka CLI")

	flag.DurationVar(&o.timeout, "timeout", 2*time.Hour, "Overall run timeout. The merge fence TTL is derived from it (timeout + 30m)")

	flag.BoolVar(&o.backfill, "backfill-home-zone", false,
		"Standalone mode: SET NX player:zone:{id}=<zone> for every row in zone_<zone>_db.player_database. Run this for EVERY existing zone before the first merge")
	zoneFlag := flag.Uint("zone", 0, "Zone ID for -backfill-home-zone and -mode pin-placement (required in those modes)")

	flag.Parse()
	o.sourceZone = uint32(*srcZone)
	o.targetZone = uint32(*dstZone)
	// 下面几个是 uint32 的落点 / zone 编号:超出范围直接拒绝,不能让 uint32() 静默截断成另一个库。
	o.zone = uint32Flag("zone", *zoneFlag)
	o.relocateSource = uint32Flag("relocate-source-storage", *relocSrc)
	o.relocateTarget = uint32Flag("relocate-target-storage", *relocDst)
	o.auditStorage = uint32Flag("storage", *auditStorage)

	// 未指定的专用地址 / 密码回落到默认 Redis。DB 号**不**回落:
	// 每个 DB 都有 yaml 里的确定值,猜错就是静默无效。
	o.mappingAddr = firstNonEmpty(o.mappingAddr, o.redisAddr)
	o.mappingPwd = firstNonEmpty(o.mappingPwd, o.redisPwd)
	o.noticeAddr = firstNonEmpty(o.noticeAddr, o.redisAddr)
	o.noticePwd = firstNonEmpty(o.noticePwd, o.redisPwd)
	o.sceneAddr = firstNonEmpty(o.sceneAddr, o.redisAddr)
	o.scenePwd = firstNonEmpty(o.scenePwd, o.redisPwd)
	o.dataPwd = firstNonEmpty(o.dataPwd, o.redisPwd)

	// -db-capability-zones 只在这里做语法解析;是否必填、能否 none 由各模式按 requireCapabilityZones 定。
	// -mode capability-check 例外:它的参数错误按「没查成」(exit 2)报,交给下面的 capabilityCheckZones ——
	// 这里的 fail 固定 exit 1,会与 capability-check 的 missing 同码(见 capability_check.go 文件头)。
	capSpec, err := parseCapabilityZoneSpec(o.capZonesRaw)
	if err != nil && o.mode != modeCapabilityCheck {
		fail("%v", err)
	}
	o.capSpec = capSpec

	// ── Backfill mode:先于 merge 的 source/target 检查分发。
	if o.backfill {
		if o.zone == 0 {
			fail("-backfill-home-zone requires -zone <non-zero>")
		}
		if err := requireWriteIntent(o); err != nil {
			fail("%v", err)
		}
		runBackfillEntry(backfillEntryParams{
			zone:        o.zone,
			mysqlDSN:    o.mysqlDSN,
			mappingAddr: o.mappingAddr,
			mappingPwd:  o.mappingPwd,
			mappingDB:   o.mappingDB,
			dryRun:      o.dryRun,
		})
		return
	}

	// 落点相关的模式不需要 -source-zone / -target-zone,先于那条检查分发。
	switch o.mode {
	case "pin-placement":
		if o.zone == 0 {
			fail("-mode pin-placement requires -zone <non-zero>")
		}
		if err := requireWriteIntent(o); err != nil {
			fail("%v", err)
		}
		runPinPlacementEntry(pinPlacementEntryParams{
			zone:        o.zone,
			mappingAddr: o.mappingAddr,
			mappingPwd:  o.mappingPwd,
			mappingDB:   o.mappingDB,
			dryRun:      o.dryRun,
			timeout:     o.timeout,
		})
		return

	case "relocate":
		if err := requireWriteIntent(o); err != nil {
			fail("%v", err)
		}
		if err := validateRelocateStorages(o.relocateSource, o.relocateTarget); err != nil {
			fail("%v", err)
		}
		if o.relocateBatch <= 0 || o.relocateLockWait <= 0 {
			fail("-relocate-batch-size and -relocate-lock-wait must be positive")
		}
		// 搬库在线进行:必须显式列出所有在跑的 zone,不接受 none(清单玩家的 home zone 由 R0 自动补查)。
		if err := requireCapabilityZones(o.capSpec, capabilityForRelocate); err != nil {
			fail("%v", err)
		}
		runRelocateEntry(o)
		return

	case modeCapabilityCheck:
		zones, code, err := capabilityCheckZones(o.capZonesRaw)
		if err != nil {
			failWithCode(code, "%v", err)
		}
		runCapabilityCheckEntry(capabilityCheckEntryParams{
			zones:       zones,
			mappingAddr: o.mappingAddr,
			mappingPwd:  o.mappingPwd,
			mappingDB:   o.mappingDB,
			timeout:     o.timeout,
		})
		return

	case "relocate-abort":
		if err := requireWriteIntent(o); err != nil {
			fail("%v", err)
		}
		if o.manifestPath == "" {
			fail("-mode relocate-abort requires -manifest-path <the manifest written by the relocate run>")
		}
		runRelocateAbortEntry(o)
		return

	case "storage-audit":
		if o.auditStorage == 0 {
			fail("-mode storage-audit requires -storage <non-zero storage id>")
		}
		runStorageAuditEntry(storageAuditEntryParams{
			storage:     o.auditStorage,
			mysqlDSN:    o.mysqlDSN,
			mappingAddr: o.mappingAddr,
			mappingPwd:  o.mappingPwd,
			mappingDB:   o.mappingDB,
			timeout:     o.timeout,
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
	// 库名会直接拼进 SQL 标识符:merge / audit / unmerge 都可能用到,任何模式下先验形状。
	if err := validateTradeSchemaName(o.tradeSchema); err != nil {
		fail("%v", err)
	}
	if err := validateGuildSchemaName(o.guildSchema); err != nil {
		fail("%v", err)
	}
	if err := validateFriendSchemaName(o.friendSchema); err != nil {
		fail("%v", err)
	}

	switch o.mode {
	case "audit":
		// 合服后验证逐 id 核对清单里的玩家(A6):没有清单就无从核对。旧的下界断言(目标区总数 >=
		// -expected-src-players)里本来就有目标区原住民,漏搬几个照样过线,已由逐 id 核对取代。
		if o.verifyMerged && o.manifestPath == "" {
			fail("-verify-merged requires -manifest-path <the manifest written by the merge being verified>: " +
				"every manifest player's mapping and main-data row is checked one by one")
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
			manifestPath:       o.manifestPath,
			expectedSrcPlayers: o.expectedSrcPlayers,
			topicGeneration:    uint32(o.kafkaTopicGen),
			tradeSchema:        o.tradeSchema,
			skipTrade:          o.skipTrade,
			// 与合服前置 P1 同一口径:两个帮会跳过开关都给了才算「这里没有帮会服务」。
			skipGuild:    o.skipGuild && o.skipRank,
			guildSchema:  o.guildSchema,
			friendSchema: o.friendSchema,
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
		mode, err := parsePlayerRowsMode(o.playerRowsMode)
		if err != nil {
			fail("%v", err)
		}
		o.playerRowsMode = mode
		if err := checkPinModeFlags(o); err != nil {
			fail("%v", err)
		}
		if err := checkMergeCapabilityFlag(o); err != nil {
			fail("%v", err)
		}
		runMerge(o)
		return

	default:
		fail("unknown -mode %q (expected 'merge', 'audit', 'unmerge', 'pin-placement', 'relocate', 'relocate-abort', "+
			"'storage-audit' or 'capability-check')", o.mode)
	}
}

// checkPinModeFlags 拒绝与 pin 模式矛盾的开关(player-storage-placement.md §10.1 第 3 条:pin 不拷玩家行、
// 不拷 data Redis blob)。静默忽略会让运维以为自己要的那一步做了。
//   - -skip-player-rows:pin 从来不拷行,这个开关(连同 -i-know-global-player-table 声明)只属于 copy 模式;
//   - -migrate-player-blobs:pin 模式不搬 player:{id}:* blob。给了这个开关说明运维知道 blob 必须跟着走
//     (data_service 按 home_zone 选 data Redis 集群,src / dst 不在同一个集群时 blob 不搬就读不到),
//     那就不能用 pin —— 用 copy 模式,或者等设计确定 pin 模式下 blob 的去留。
func checkPinModeFlags(o options) error {
	if o.playerRowsMode != playerRowsModePin {
		return nil
	}
	if o.skipRows {
		return fmt.Errorf("-skip-player-rows only applies to -player-rows-mode copy: pin mode never copies player rows")
	}
	if o.migrateBlobs && !o.skipBlobs {
		return fmt.Errorf("-migrate-player-blobs cannot be combined with -player-rows-mode pin: pin mode does not move " +
			"player:{id}:* blobs (player-storage-placement.md §10.1). If the source and target zones use different data " +
			"Redis clusters, run this merge with -player-rows-mode copy")
	}
	return nil
}

// checkMergeCapabilityFlag 是合服入口对 -db-capability-zones 的前置要求(纯函数):pin 模式必填(接受 none);
// copy 模式此刻不要求 —— 只有 S 段发现清单玩家已有落点记录时才需要,届时在围栏之下按同一口径拒绝。
func checkMergeCapabilityFlag(o options) error {
	if o.playerRowsMode != playerRowsModePin {
		return nil
	}
	return requireCapabilityZones(o.capSpec, capabilityForMerge)
}

// uint32Flag 把 flag.Uint 的值收窄成 uint32;超出范围直接拒绝 —— 截断会把人指到另一个 zone / 落点库。
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
	failWithCode(1, format, args...)
}

// failWithCode 与 fail 同一格式,但以调用方给的退出码结束(如 capability-check 的参数错误按 2 = 没查成)。
func failWithCode(code int, format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ERROR: "+format+"\n", args...)
	os.Exit(code)
}

func verbWrite(dry bool) string {
	if dry {
		return "would be migrated"
	}
	return "migrated"
}
