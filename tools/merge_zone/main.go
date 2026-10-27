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
//	guild    DB 0   guild_rank:zone:{z} / guild:v2:{id} / guild_rank:maintenance_lock
//	                go/guild/etc/guild.yaml → RedisClient.DB(2026-10-23 由 2 改为 0)
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
// 以上「DEL」只适用于 -route-source=redis;db 源的围栏真源是全局库 zone_merge_fence,镜像不能删,续跑用原命令加
// -take-over-fence <run_id>(routeFenceKeptAbortMessage,fence.go「-route-source=db:库围栏」)。
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
	"strconv"
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
	defaultGuildRedisDB   = 0 // guild.yaml RedisClient.DB(2026-10-23 由 2 改为 0:Redis Cluster 只有 DB 0)
	// friend 的 DB 3 已删(2026-09-18,friend 移植 F3):它唯一服务的键 friend:online:{pid}
	// 全仓没有写者、读者也已删除,审计改用 shared 的 player:session 判在线。
	defaultSharedRedisDB = 0 // login / db / player_locator / scene_manager
	// go/db/etc/db.yaml → Kafka.GroupID。它有两个身份:legacy 全 zone 共用的消费组名(-kafka-legacy-group 的缺省),
	// 以及按 zone 推导组名的基名 —— db_rpc_consumer_group_z<zone>(dbConsumerGroupForZone,
	// 与 k8s_deploy.ps1 切组后的 ConfigMap、go_services.ps1 的 -Zone N 本机启动同名)。
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
	etcdEndpoints string
	skipTrade     bool
	tradeSchema   string
	guildSchema   string
	friendSchema  string
	teamSchema    string

	allowEmptySource   bool
	expectedSrcPlayers int64
	manifestPath       string
	tableListPath      string

	// kafkaGroup 是 -kafka-group(非空时 P3 只看它);kafkaLegacyGroup 是 -kafka-legacy-group,
	// 只在 kafkaGroup 为空时与 db_rpc_consumer_group_z<zone> 一起作 P3 的候选,见 kafkaGroupCandidates。
	kafkaGroup                   string
	kafkaLegacyGroup             string
	kafkaTopicGen                uint
	kafkaCLI                     string
	kafkaBootstrap               string
	assumeKafkaDrained           bool
	maintenanceRunID             string
	freezeTicketID               string
	freezeRequestID              string
	freezeReceiptSHA256          string
	frozenWriterEpoch            uint64
	maintenanceRuntimePlan       string
	maintenanceRuntimeDirectory  string
	maintenanceRuntimeSHA256     string
	normalRuntimePlan            string
	normalRuntimeDirectory       string
	normalRuntimeSHA256          string
	recoveryAuditOutput          string
	maintenancePublicationFile   string
	maintenancePublicationSHA256 string
	maintenance                  *maintenanceTool

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

	// routeSource 是 -route-source(db / redis,读写映射的模式必填、无缺省,见 checkRouteSourceFlags);routeDBDSN 是
	// -route-db-dsn(全局库,含口令,不进日志)。waitAOF* 是 -route-source=db 时每批发布末尾 WAITAOF 的份数与等待
	// (newRouteDurability 校验)。
	routeSource      string
	routeDBDSN       string
	waitAOFLocal     int
	waitAOFReplicas  int
	waitAOFTimeoutMs int
	// takeOverFenceRunID 是 -take-over-fence:-route-source=db 的 merge / unmerge 续跑时接手该 run_id 留在库里的围栏
	// (checkTakeOverFenceFlag 校验;语义见 fence.go「-route-source=db:库围栏」)。
	takeOverFenceRunID string
}

func main() {
	var o options

	flag.StringVar(&o.mode, "mode", "merge",
		"Operation mode: 'merge' (default), 'audit' (read-only, audit_resources.go), 'unmerge' (reverse a run from its manifest; "+
			"runs the pre-flight gates against the TARGET zone and requires the current NORMAL consumed freeze receipt), "+
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
	flag.IntVar(&o.redisDB, "redis-db", defaultGuildRedisDB, "GUILD Redis DB (guild_rank:zone / guild:v2 / guild_rank:maintenance_lock; guild.yaml default 0)")

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
	flag.StringVar(&o.teamSchema, "team-schema", defaultTeamSchema, "全球Team权威schema，与-mysql-dsn的TiDB入口共域；不得用Redis证明无队")
	flag.BoolVar(&o.skipMapping, "skip-player-mapping", false, "Skip the player:zone remapping")
	flag.BoolVar(&o.skipRows, "skip-player-rows", false,
		"-player-rows-mode copy only: skip copying player rows from zone_<src>_db to zone_<dst>_db. ONLY valid once player main data is "+
			"global (TiDB Phase 2); requires -i-know-global-player-table. Pin mode never copies rows and refuses this flag")
	flag.BoolVar(&o.knowGlobalPT, "i-know-global-player-table", false,
		"Attest that player main data is NOT per-zone any more (TiDB Phase 2 landed). Required with -skip-player-rows")
	flag.BoolVar(&o.migrateBlobs, "migrate-player-blobs", false, "Copy player:{id}:* keys between per-zone data Redis clusters (multi-cluster deployments only)")
	flag.BoolVar(&o.skipBlobs, "skip-player-blob-migration", false, "Skip the blob step even if -migrate-player-blobs was passed (emergency)")
	flag.BoolVar(&o.clearHotState, "clear-source-hot-state", false,
		"After remap: DEL source-zone player:{id}:location / scene:* / world_channels / node_load in the scene_manager Redis, then (last step) SREM S from scene_manager:zones. Refuses unless the etcd scene-node registry (-etcd-endpoints) has no node of zone S and scene_nodes:zone:{S}:load is empty")
	flag.StringVar(&o.etcdEndpoints, "etcd-endpoints", "",
		"Comma-separated etcd client URLs (http://host:2379), read through the etcd v3 JSON gateway. Required with -clear-source-hot-state: positive evidence that zone S has no registered scene node")

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

	flag.StringVar(&o.kafkaGroup, "kafka-group", "",
		"Explicit go/db consumer group for the lag pre-flight (only this group is checked); empty = derive "+
			"db_rpc_consumer_group_z<zone> plus -kafka-legacy-group and use the single group that holds committed offsets "+
			"on the topic (both holding offsets = refused: finalize the handoff with kafka_offset_reset.ps1 -FinalizeHandoff first)")
	flag.StringVar(&o.kafkaLegacyGroup, "kafka-legacy-group", defaultKafkaGroup,
		"Legacy shared go/db consumer group checked next to db_rpc_consumer_group_z<zone> when -kafka-group is empty; "+
			"pass an empty value to check only the zone group")
	flag.UintVar(&o.kafkaTopicGen, "kafka-topic-generation", 1,
		"go/db Kafka.TopicGeneration; topic = db_task_zone_<zone>[_g<gen>] (merge lag pre-flight; relocate R2 waits on the "+
			"go/db ordering locks of each player's home-zone topic)")
	flag.StringVar(&o.kafkaCLI, "kafka-consumer-groups-cmd", "", "Path to kafka-consumer-groups(.sh|.bat) for the lag pre-flight")
	flag.StringVar(&o.kafkaBootstrap, "kafka-bootstrap", "127.0.0.1:9092", "Kafka bootstrap server for -kafka-consumer-groups-cmd")
	flag.BoolVar(&o.assumeKafkaDrained, "assume-kafka-drained", false,
		"已废弃：人工声明不能替代本轮NORMAL已消费冻结收据，指定即拒绝")
	flag.StringVar(&o.maintenanceRunID, "maintenance-run-id", "", "本轮正式维护run的32位小写hex，不是merge清单run_id")
	flag.StringVar(&o.freezeTicketID, "freeze-ticket-id", "", "本轮NORMAL已消费freeze ticket的32位小写hex")
	flag.StringVar(&o.freezeRequestID, "freeze-request-id", "", "已提交SQL_FROZEN请求的32位小写hex")
	flag.StringVar(&o.freezeReceiptSHA256, "freeze-receipt-sha256", "", "已提交SQL_FROZEN永久收据的64位小写hex")
	flag.Uint64Var(&o.frozenWriterEpoch, "frozen-writer-epoch", 0, "SQL_FROZEN收据实际after_epoch，必须仍是当前writer epoch")
	flag.StringVar(&o.maintenanceRuntimePlan, "maintenance-runtime-plan", "", "P0已固定的单次MAINTENANCE工具私有runtime原件")
	flag.StringVar(&o.maintenanceRuntimeDirectory, "maintenance-runtime-directory", "", "本工具boot私有控制目录，不复用旧boot")
	flag.StringVar(&o.maintenanceRuntimeSHA256, "maintenance-runtime-sha256", "", "runtime原件SHA256，必须与P0来源一致")
	flag.StringVar(&o.normalRuntimePlan, "normal-runtime-plan", "", "在线有限NORMAL工具固定SOURCE runtime；无run由launcher独立RETIRE")
	flag.StringVar(&o.normalRuntimeDirectory, "normal-runtime-directory", "", "在线工具SOURCE私有目录")
	flag.StringVar(&o.normalRuntimeSHA256, "normal-runtime-sha256", "", "在线SOURCE runtime原件SHA256")
	flag.StringVar(&o.recoveryAuditOutput, "recovery-audit-output", "", "仅audit -verify-merged：输出正式P7 MergeRecoveryAudit私有原件")
	flag.StringVar(&o.maintenancePublicationFile, "maintenance-publication-file", "", "P0 MAINTENANCE全集实际closure及SourcePin的正式PublicationEvidence")
	flag.StringVar(&o.maintenancePublicationSHA256, "maintenance-publication-sha256", "", "维护闭合全集原件SHA256")

	flag.DurationVar(&o.timeout, "timeout", 2*time.Hour, "Overall run timeout. The merge fence TTL is derived from it (timeout + 30m)")

	flag.BoolVar(&o.backfill, "backfill-home-zone", false,
		"Standalone mode: SET NX player:zone:{id}=<zone> for every row in zone_<zone>_db.player_database. Run this for EVERY existing zone before the first merge")
	zoneFlag := flag.Uint("zone", 0, "Zone ID for -backfill-home-zone and -mode pin-placement (required in those modes)")

	flag.StringVar(&o.routeSource, "route-source", "",
		"Where the player route mapping is authoritative. NO DEFAULT: required by every mode that reads or writes player:zone / "+
			"player:placement (merge, unmerge, audit, pin-placement, relocate, relocate-abort, storage-audit, -backfill-home-zone). "+
			"'db' when data_service runs RouteStore.Mode=db_first (writes go to the global DB first and are then published to the "+
			"mapping Redis; needs -route-db-dsn); 'redis' only while every data_service still runs RouteStore.Mode=redis (legacy path)")
	flag.StringVar(&o.routeDBDSN, "route-db-dsn", "",
		"-route-source=db: DSN of the global database that holds player_route (dev: testdb, K8s: mmorpg_global). The session is "+
			"pinned to READ-COMMITTED with innodb_lock_wait_timeout=1; the DSN is never logged")
	flag.IntVar(&o.waitAOFLocal, "waitaof-local", 1, "-route-source=db: WAITAOF numlocal after each publish batch (0 or 1)")
	flag.IntVar(&o.waitAOFReplicas, "waitaof-replicas", 0,
		"-route-source=db: WAITAOF numreplicas after each publish batch (single instance 0, redis-ha 1)")
	flag.IntVar(&o.waitAOFTimeoutMs, "waitaof-timeout-ms", 1500,
		"-route-source=db: WAITAOF timeout per attempt in milliseconds (1..2000; 0 would block forever)")
	flag.StringVar(&o.takeOverFenceRunID, "take-over-fence", "",
		"-route-source=db, -mode merge / unmerge only: resume after an abort by taking over the merge fence that the aborted run "+
			"<run_id> left in zone_merge_fence, in one DB transaction (the zones are never unfenced). Pass the run_id printed by the "+
			"abort message with the same -manifest-path and zone pair (both are checked against the fence), and only after confirming "+
			"that no merge_zone process of that run is alive: a live old process has its next mapping batch refused by the DB, but its "+
			"other writes (rows, blobs, guild, trade, rank, hot state, manifest file) only stop at its next renewal (up to "+
			routeFenceRenewMaxInterval.String()+" later). "+
			"If the take-over itself fails half-way, the fence is kept, never released")

	flag.Parse()
	// 未接实际资源owner的旁路不得凭同一个可执行文件取得维护写资格。
	if err := checkMaintenanceMode(o); err != nil {
		fail("%v", err)
	}
	if o.assumeKafkaDrained {
		fail("-assume-kafka-drained已废弃；人工声明不能替代正式非SQL闭合与NORMAL冻结收据")
	}
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

	// -route-source 没有缺省值:读写映射的模式都必须显式选(规格 §13.3 第 3 条)。读写映射的模式都已接好 db 源(W5–W12 与 R8 读方;
	// 第一波的 refuseUnwiredDBRouteSource 已删除);库与 mapping Redis 是否成对在各入口连上之后核对(openRunRouteBackend /
	// openRelocateRouteBackend / openRouteReadSide)。
	if err := checkRouteSourceFlags(o); err != nil {
		fail("%v", err)
	}
	if err := checkTakeOverFenceFlag(o); err != nil {
		fail("%v", err)
	}
	// 同时读取玩家原件与路由原件的入口必须共域，审计/回填/搬库也不能把两个TiDB集群拼成一份事实。
	if o.routeSource == routeSourceDB && (o.backfill || o.mode == "audit" || o.mode == "storage-audit" || o.mode == "relocate" || o.mode == "relocate-abort") {
		if err := requireTeamRouteDomain(o.mysqlDSN, o.routeDBDSN, o.routeSource); err != nil {
			fail("%v", err)
		}
	}
	if routeSourceRequired(o) && o.routeSource == routeSourceRedis {
		fmt.Fprintln(os.Stderr, "WARN: -route-source=redis: the mapping Redis is written directly (legacy path); valid only while "+
			"every data_service runs RouteStore.Mode=redis")
	}

	// ── Backfill mode:先于 merge 的 source/target 检查分发。
	if o.backfill {
		if o.zone == 0 {
			fail("-backfill-home-zone requires -zone <non-zero>")
		}
		if err := requireWriteIntent(o); err != nil {
			fail("%v", err)
		}
		if !o.dryRun {
			if err := executeMaintenanceTool(o, runMaintenanceBackfill); err != nil {
				fail("%v", err)
			}
			return
		}
		runBackfillEntry(backfillEntryParams{
			zone:        o.zone,
			mysqlDSN:    o.mysqlDSN,
			mappingAddr: o.mappingAddr,
			mappingPwd:  o.mappingPwd,
			mappingDB:   o.mappingDB,
			dryRun:      o.dryRun,
			route:       routeEntryOptionsOf(o),
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
		if err := executeNormalTool(o, func(bound options) {
			runPinPlacementEntry(pinPlacementEntryParams{
				zone:        o.zone,
				mappingAddr: o.mappingAddr,
				mappingPwd:  o.mappingPwd,
				mappingDB:   o.mappingDB,
				dryRun:      o.dryRun,
				timeout:     o.timeout,
				route:       routeEntryOptionsOf(o),
				parent:      maintenanceParent(bound),
			})
		}); err != nil {
			fail("%v", err)
		}
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
		if err := executeNormalTool(o, runRelocateEntry); err != nil {
			fail("%v", err)
		}
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
		if err := executeNormalTool(o, runRelocateAbortEntry); err != nil {
			fail("%v", err)
		}
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
			route:       routeEntryOptionsOf(o),
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
	if !schemaNamePattern.MatchString(o.teamSchema) {
		fail("team-schema不是有效SQL标识符")
	}

	switch o.mode {
	case "audit":
		// 合服后验证逐 id 核对清单里的玩家(A6):没有清单就无从核对。旧的下界断言(目标区总数 >=
		// -expected-src-players)里本来就有目标区原住民,漏搬几个照样过线,已由逐 id 核对取代。
		if o.verifyMerged && o.manifestPath == "" {
			fail("-verify-merged requires -manifest-path <the manifest written by the merge being verified>: " +
				"every manifest player's mapping and main-data row is checked one by one")
		}
		if o.recoveryAuditOutput != "" {
			if err := runMergeRecoveryAuditEntry(o); err != nil {
				fail("P7业务审核未完成，保持冻结: %v", err)
			}
			return
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
			etcdEndpoints:      o.etcdEndpoints,
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
		if err := checkDataRedisBlobFlags(o); err != nil {
			fail("%v", err)
		}
		if err := checkMergeCapabilityFlag(o); err != nil {
			fail("%v", err)
		}
		if err := executeMaintenanceTool(o, runMerge); err != nil {
			fail("%v", err)
		}
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

// checkDataRedisBlobFlags 拒绝「声明了两套不同的 data Redis,却没让 blob 跟着走」的合服(纯函数)。
//
// 第 2 步拷 blob 只看 -migrate-player-blobs,与 pin / copy 模式无关(merge_run.go 的 stepPlayerBlobs 分支)。
// data_service 按 home_zone 选 data Redis 集群:src / dst 不在同一个后端时,第 5 步改完映射,data_service 去 dst
// 读 player:{id}:*,拿到的是「成功、空数据、Version=0」,源集群里的数据没人再读 —— 全程没有报错。
// 运维用 -source-data-redis / -target-data-redis 声明了两个不同的后端,就说明这是多集群合服,此时必须显式选择:
// -migrate-player-blobs(只能配 copy 模式,见 checkPinModeFlags),或 -skip-player-blob-migration(已自行搬好)。
// 两个地址没给全时工具无从判断拓扑,保持原行为;多集群部署合服必须给全,见 player-storage-placement.md。
func checkDataRedisBlobFlags(o options) error {
	if o.migrateBlobs || o.skipBlobs || o.sourceData == "" || o.targetData == "" {
		return nil
	}
	if sameDataBackend(addrsFromCSV(o.sourceData), addrsFromCSV(o.targetData), o.sourceDataDB, o.targetDataDB) {
		return nil
	}
	return fmt.Errorf("-source-data-redis (%s db=%d) and -target-data-redis (%s db=%d) are different backends, "+
		"but -migrate-player-blobs is not set: after the mapping remap data_service reads player:{id}:* from the "+
		"target and finds nothing. Pass -migrate-player-blobs with -player-rows-mode copy, or "+
		"-skip-player-blob-migration if the blobs have been moved by other means",
		o.sourceData, o.sourceDataDB, o.targetData, o.targetDataDB)
}

// checkMergeCapabilityFlag 是合服入口对 -db-capability-zones 的前置要求(纯函数):pin 模式必填(接受 none);
// copy 模式此刻不要求 —— 只有 S 段发现清单玩家已有落点记录时才需要,届时在围栏之下按同一口径拒绝。
func checkMergeCapabilityFlag(o options) error {
	if o.playerRowsMode != playerRowsModePin {
		return nil
	}
	return requireCapabilityZones(o.capSpec, capabilityForMerge)
}

// -route-source 的两个取值(规格 §6.4、§13.3 第 3 条)。
const (
	routeSourceDB    = "db"
	routeSourceRedis = "redis"
)

// routeSourceRequired 报告本次模式是否读写 player:zone / player:placement,从而必须显式给 -route-source。
// 只有 -mode capability-check(只读 db:capability:zone:*)不需要;未知模式交给后面的「unknown -mode」报错。
// 只读的 audit / storage-audit 也要:-route-source=db 时它们要读库(规格 §6.3 R8,storage-audit 决定库能否退役)。
func routeSourceRequired(o options) bool {
	if o.backfill {
		return true
	}
	switch o.mode {
	case "merge", "unmerge", "audit", "pin-placement", "relocate", "relocate-abort", "storage-audit":
		return true
	}
	return false
}

// checkRouteSourceFlags 是 -route-source / -route-db-dsn / -waitaof-* 的前置校验(纯函数,规格 §13.3 第 3 条):
//   - 读写映射的模式必须显式给 -route-source=db|redis,没有缺省值:缺省取哪一边,都会在另一种部署里写错库;
//   - db:-route-db-dsn 必填,-waitaof-* 必须合法(newRouteDurability);
//   - redis:不接受 -route-db-dsn(给了却不用,会让人以为库被写过)。
//
// capability-check 不读写映射,不看这几个参数。库与 Redis 是否成对(水位键)要连上之后才能核对,见 route_store.go 的 openRouteBackend。
func checkRouteSourceFlags(o options) error {
	if !routeSourceRequired(o) {
		return nil
	}
	switch o.routeSource {
	case "":
		return fmt.Errorf("-route-source is required and has no default: pass -route-source=db (with -route-db-dsn) when " +
			"data_service runs RouteStore.Mode=db_first, or -route-source=redis only while every data_service still runs " +
			"RouteStore.Mode=redis")
	case routeSourceDB:
		if o.routeDBDSN == "" {
			return fmt.Errorf("-route-source=db requires -route-db-dsn (the global database that holds player_route)")
		}
		_, err := newRouteDurability(o.waitAOFLocal, o.waitAOFReplicas, o.waitAOFTimeoutMs)
		return err
	case routeSourceRedis:
		if o.routeDBDSN != "" {
			return fmt.Errorf("-route-db-dsn is only used with -route-source=db; with -route-source=redis nothing would be written to it")
		}
		return nil
	default:
		return fmt.Errorf("-route-source %q: want db or redis", o.routeSource)
	}
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

// maintenanceTool只拥有本次P0固定的单boot工具；资源和后台任务随它一起闭合。
type maintenanceCloser interface{ Close() error }
type maintenanceTool struct {
	normalSQL     *authoritysql.WriterConfig
	normalOutcome error
	runtime       *drainproof.Runtime
	session       *drainproof.Session
	catalogDB     *sql.DB
	check         func(context.Context) error
	context       context.Context
	mu            sync.Mutex
	resources     []maintenanceCloser
	registered    map[maintenanceCloser]bool
	redisBound    map[redis.UniversalClient]bool
	stops         []func(context.Context) error
	closing       bool
	stopOnce      sync.Once
	stopErr       error
	finishOnce    sync.Once
	finishErr     error
}
type maintenanceContextKey struct{}

func checkMaintenanceMode(o options) error {
	if o.dryRun || o.mode == "audit" || o.mode == "storage-audit" || o.mode == "capability-check" {
		return nil
	}
	if o.backfill || o.mode == "merge" || o.mode == "unmerge" || o.mode == "pin-placement" || o.mode == "relocate" || o.mode == "relocate-abort" {
		return nil
	}
	return fmt.Errorf("模式 %q 尚未绑定P0单次维护工具身份，拒绝实际写入", o.mode)
}
func maintenanceFromContext(ctx context.Context) *maintenanceTool {
	t, _ := ctx.Value(maintenanceContextKey{}).(*maintenanceTool)
	return t
}

func normalToolFromContext(ctx context.Context) bool {
	t := maintenanceFromContext(ctx)
	return t != nil && t.normalSQL != nil
}
func normalToolFailure(ctx context.Context, err error) {
	if err == nil {
		return
	}
	if owner := maintenanceFromContext(ctx); owner != nil {
		owner.mu.Lock()
		owner.normalOutcome = errors.Join(owner.normalOutcome, err)
		owner.mu.Unlock()
		owner.hold()
	}
}

func openToolSQLPool(ctx context.Context, dsn string) (*sql.DB, error) {
	t := maintenanceFromContext(ctx)
	if t == nil || t.normalSQL == nil {
		return sql.Open("mysql", dsn)
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	inner, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	connector, err := authoritysql.PinCurrentSession(inner, true, authoritysql.Writer(*t.normalSQL))
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(connector), nil
}

// 在线pin/relocate是NORMAL，始终writable=1和正式own64/epoch；不使用P6冻结工具身份。
func executeNormalTool(o options, run func(options)) error {
	if o.dryRun {
		run(o)
		return nil
	}
	if o.timeout <= 0 {
		return drainproof.ErrHold
	}
	config, err := authoritysql.LoadWriterConfig(os.Getenv)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()
	var runtime *drainproof.Runtime
	if o.normalRuntimePlan != "" {
		runtime, err = drainproof.LoadRuntime(o.normalRuntimePlan, o.normalRuntimeDirectory, o.normalRuntimeSHA256)
		if err != nil {
			config.Seal()
			return err
		}
		if runtime.Request.RunPhase == pb.SqlWriterDrainRun_SQL_FROZEN || runtime.Request.RunPhase == pb.SqlWriterDrainRun_SQL_RELEASED || runtime.Seed("SOURCE") == nil || runtime.Seed("RELAY") != nil {
			config.Seal()
			return drainproof.ErrHold
		}
		if err = runtime.BindBoot(config.InstanceID(), config.WriterKey()); err != nil {
			config.Seal()
			return err
		}
	} else if o.normalRuntimeDirectory != "" || o.normalRuntimeSHA256 != "" {
		config.Seal()
		return drainproof.ErrHold
	}
	owner := &maintenanceTool{normalSQL: &config, runtime: runtime, session: drainproof.New(), check: func(context.Context) error { return nil }}
	owner.context = context.WithValue(ctx, maintenanceContextKey{}, owner)
	o.maintenance = owner
	returned := false
	defer func() {
		if !returned {
			owner.hold()
		}
	}()
	finish, err := owner.session.Enter()
	if err != nil {
		return err
	}
	func() { defer finish(); run(o) }()
	returned = true
	if err = owner.stop(ctx); err != nil {
		return err
	}
	owner.session.CloseAdmission()
	if err = owner.session.WaitIdle(ctx); err != nil {
		owner.hold()
		return err
	}
	owner.mu.Lock()
	owner.closing = true
	resources := append([]maintenanceCloser(nil), owner.resources...)
	owner.mu.Unlock()
	config.Seal()
	for i := len(resources) - 1; i >= 0; i-- {
		err = errors.Join(err, resources[i].Close())
	}
	if err != nil {
		owner.hold()
	}
	if err = errors.Join(err, owner.session.Wait(ctx), owner.normalOutcome); err != nil {
		return err
	}
	if runtime != nil {
		return runtime.Write("SOURCE", owner.session)
	}
	return nil // 无run不造unbound closure；launcher按实际registered boot独立RETIRE。
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
func registerMaintenanceStop(ctx context.Context, stop func(context.Context) error) error {
	t := maintenanceFromContext(ctx)
	if t == nil {
		return nil
	}
	if stop == nil {
		return drainproof.ErrHold
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closing || t.session.Closed() {
		return drainproof.ErrClosed
	}
	t.stops = append(t.stops, stop)
	return nil
}
func closeUnownedMaintenanceResource(ctx context.Context, r maintenanceCloser) {
	if maintenanceFromContext(ctx) == nil && r != nil {
		if err := r.Close(); err != nil {
			log.Printf("只读资源关闭失败: %v", err)
		}
	}
}
func (t *maintenanceTool) stop(ctx context.Context) error {
	t.stopOnce.Do(func() {
		t.stopErr = drainproof.ErrHold
		returned := false
		defer func() {
			if !returned {
				t.hold()
			}
		}()
		t.mu.Lock()
		stops := append([]func(context.Context) error(nil), t.stops...)
		t.mu.Unlock()
		var stopErr error
		for i := len(stops) - 1; i >= 0; i-- {
			stopErr = errors.Join(stopErr, stops[i](ctx))
		}
		t.stopErr = stopErr
		returned = true
		if t.stopErr != nil {
			t.hold()
		}
	})
	return t.stopErr
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
func redisUnknown(err error) bool { return err != nil && !errors.Is(err, redis.Nil) }
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
func bindMaintenanceRedisNode(ctx context.Context, client *redis.Client) error {
	return installMaintenanceRedisHook(ctx, client)
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
	db, err := authoritysql.Open(ctx, o.routeDBDSN, proof.authoritySchema, true, 1, authoritysql.Reader())
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

// 先join真正后台，再永久拒新命令并等待accepted；UNKNOWN不阻止资源关闭，但阻止正式成功原件。
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
	if err := t.stop(ctx); err != nil {
		return err
	} // 尚未join时不能关闭资源。
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

// 原fence.writeContext只有cancel没有join；这里保同一判据并登记真正done。
func maintenanceFenceContext(parent context.Context, fence runFence) (context.Context, error) {
	t := maintenanceFromContext(parent)
	if t == nil {
		return fence.writeContext(parent), nil
	}
	work, fail := context.WithCancelCause(parent)
	watch, stop := context.WithCancel(parent)
	done := make(chan struct{})
	finish, err := t.session.Enter()
	if err != nil {
		stop()
		fail(err)
		return nil, err
	}
	if err = registerMaintenanceStop(parent, func(ctx context.Context) error {
		stop()
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}); err != nil {
		finish()
		stop()
		fail(err)
		return nil, err
	}
	go func() {
		returned := false
		defer close(done)
		defer finish()
		defer func() {
			if !returned {
				t.hold()
			}
		}()
		ticker := time.NewTicker(routeFenceWatchInterval)
		defer ticker.Stop()
		for {
			select {
			case <-watch.Done():
				returned = true
				return
			case <-ticker.C:
				if err := fence.check(); err != nil {
					t.hold()
					fail(err)
					returned = true
					return
				}
			}
		}
	}()
	return work, nil
}
func finishMaintenanceFence(ctx context.Context, fence runFence) error {
	t := maintenanceFromContext(ctx)
	if t == nil {
		fence.release(ctx)
		return nil
	}
	if err := t.stop(ctx); err != nil {
		return err
	}
	// 原release只WARN，不能据此发布工具成功；在同一共享原语核全64片和镜像的真实返回。
	f, ok := fence.(*routeMergeFence)
	if !ok {
		t.hold()
		return drainproof.ErrHold
	}
	releaseCtx, cancel := context.WithTimeout(ctx, routeFenceReleaseTimeout)
	defer cancel()
	f.mu.Lock()
	held := f.held
	f.held = false
	f.mu.Unlock()
	if !held {
		t.hold()
		return drainproof.ErrHold
	}
	released, err := f.store.ReleaseFence(releaseCtx, f.zones, f.runID)
	if err != nil {
		t.hold()
		return err
	}
	for _, zone := range f.zones {
		if released[zone] != int64(routeFenceShards) {
			t.hold()
			return fmt.Errorf("zone %d 释放围栏片数不完整", zone)
		}
		deleted, err := f.mirror.deleteFenceMirror(releaseCtx, zone, f.grant.HolderJSON)
		if err != nil {
			t.hold()
			return err
		}
		if !deleted {
			t.hold()
			return fmt.Errorf("zone %d 围栏镜像不再属于本次工具", zone)
		}
	}
	return nil
}

// 回填算法仍由原领域函数承担；装配在本文件统一，所有真实句柄进入同一个TOOL owner。
func runMaintenanceBackfill(o options) {
	ctx := maintenanceParent(o)
	if err := checkMaintenanceTool(ctx); err != nil {
		log.Fatal(err)
	}
	proof, _, err := openNormalFreezeProof(ctx, o)
	if err != nil {
		log.Fatal(err)
	}
	ctx = withNormalFreezeProof(ctx, proof)
	db := mustOpenMySQL(ctx, o.mysqlDSN)
	if err := assertSchemaExists(ctx, db, zoneDBName(o.zone)); err != nil {
		log.Fatal(err)
	}
	mapping := mustDial(ctx, "mapping", o.mappingAddr, o.mappingPwd, o.mappingDB)
	routes, err := openRunRouteBackend(ctx, routeEntryOptionsOf(o), mapping, newToolRunID("backfill", o.zone, time.Now()))
	if err != nil {
		log.Fatal(err)
	}
	rep, err := stepBackfillHomeZone(ctx, routes, mapping, newMySQLPlayerIDSource(db, o.zone), o.zone, false)
	if err != nil {
		log.Fatalf("backfill: %v (partial progress: %s)", err, rep)
	}
	log.Printf("回填完成，等待工具闭合: %s", rep)
}

// P7只调用原审核器。正式proto适配由独占schema owner合入；这里不写并行审核JSON。
// verifyFrozen必须强读同NORMAL consumed/current SQL_FROZEN，审核前后均执行；不得以闭合代业务结果。
func runClosedMergeAudit(ctx context.Context, cfg auditConfig, plan *pb.SqlWriterMaintenanceReceipt_Plan,
	frozen *pb.SqlWriterMaintenanceReceipt, publication *pb.SqlWriterMaintenanceReceipt_PublicationEvidence,
	verifyFrozen func(context.Context) error) ([]ResourceAudit, error) {
	if verifyFrozen == nil || !cfg.verify || cfg.manifest == nil || cfg.manifestErr != nil || cfg.routeSource != routeSourceDB ||
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

func mergeRecoveryRowNames() []string {
	return []string{"player.name (global)", "friend", "friend_request", "guild_member",
		"verify:mapping_src", "verify:manifest_mapping", "verify:guild_zone", "verify:guild_rank", "verify:trade_listing",
		"verify:manifest_rows", "verify:save_barrier", "verify:source_hot_state", "verify:merge_fence", "verify:route_cache"}
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
		!filepath.IsAbs(o.maintenancePublicationFile) || o.expectedSrcPlayers < 0 {
		return drainproof.ErrHold
	}
	if err := requireTeamRouteDomain(o.mysqlDSN, o.routeDBDSN, o.routeSource); err != nil {
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
		db, err := authoritysql.Open(ctx, raw, cfg.DBName, true, maxOpen, authoritysql.Reader())
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
	if !validRunID(manifest.RunID) || recordedRouteSource(manifest.RouteSource) != routeSourceDB {
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
	shared, err := dial(o.noticeAddr, o.noticePwd, o.noticeDB)
	if err != nil {
		return err
	}
	scene, err := dial(o.sceneAddr, o.scenePwd, o.sceneDB)
	if err != nil {
		return err
	}
	// 只读Reader池构造route store：nil TOOL上下文仅跑session/meta读探针，写方法没有授权。
	routeStore, meta, err := newMergeRouteStoreFromDB(ctx, authority, newMergeRouteWriter("audit", "p7"))
	if err != nil {
		return err
	}
	if err := requireCurrentTiDB(ctx, game); err != nil {
		return err
	}
	watermark, present, err := (&routePublisher{rdb: mapping}).readWatermark(ctx)
	if err != nil {
		return err
	}
	if err := decideRoutePairing(routeSourceDB, watermark, present, meta.Generation); err != nil {
		return err
	}
	cfg := auditConfig{db: game, mappingDB: mapping, rankRDB: rank, sharedRDB: shared, sceneRDB: scene, sharedDBIndex: o.noticeDB,
		src: o.sourceZone, dst: o.targetZone, verify: true, timeout: 30 * time.Second, expectedSrcPlayers: o.expectedSrcPlayers,
		manifest: manifest, topicGeneration: uint32(o.kafkaTopicGen), tradeSchema: o.tradeSchema, skipTrade: o.skipTrade,
		guildSchema: o.guildSchema, skipGuild: o.skipGuild && o.skipRank, friendSchema: o.friendSchema,
		routeSource: routeSourceDB, routes: routeReadSideDB{db: routeStore}, routeDB: routeStore}
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
	for _, client := range []*redis.Client{mapping, rank, shared, scene} {
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
		kind := pb.SqlWriterMaintenanceReceipt_MergeRecoveryCheck_OBSERVATION
		if row.Name == "player.name (global)" {
			kind = pb.SqlWriterMaintenanceReceipt_MergeRecoveryCheck_RULE
		}
		audit.Checks = append(audit.Checks, &pb.SqlWriterMaintenanceReceipt_MergeRecoveryCheck{Name: row.Name, SourceCount: row.SourceCount,
			TargetCount: row.TargetCount, UniqueScope: row.UniqueScope, ConflictCount: row.ConflictCount, Severity: row.Severity,
			Status: pb.SqlWriterMaintenanceReceipt_MergeRecoveryCheck_COMPLETE, Notes: row.Notes, QuerySourcePins: sources, Kind: kind})
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
	required := []string{"main.go", "audit_resources.go", "audit_checks.go", "trade_step.go", "save_barrier_verify.go",
		"storage_audit.go", "route_store.go", "route_publish.go", "manifest.go", "player_rows.go", "scene_hot_state.go", "player_blob_migrate.go"}
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
