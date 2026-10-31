package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"shared/authoritysql"
	"shared/drainproof"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/netx"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/zrpc"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	"guild/internal/activity"
	"guild/internal/config"
	"guild/internal/constants"
	"guild/internal/data"
	guildkafka "guild/internal/kafka"
	"guild/internal/logic"
	"guild/internal/node"
	"guild/internal/server"
	"guild/internal/session"
	"guild/internal/svc"
	base "proto/common/base"
	kafkapb "proto/contracts/kafka"
	pb "proto/guild"
	"schemamigrate"
	"shared/buildinfo"
	"shared/grpcstats"
	"shared/idsegment"
	"shared/kafkautil"
	"shared/killswitch"
	"shared/playerredis"
	"shared/safego"
	"shared/serverbase"
	"shared/snowflakealloc"
)

var configFile = flag.String("f", "etc/guild.yaml", "config file path")

var showVersion = flag.Bool("version", false, "打印版本信息并退出")

// migrateOnly:`guild -f <yaml> -migrate` 只对 mmorpg_guild 跑一次 schemamigrate.Up 后退出
// (退出码 0 成功 / 1 错误 / 3 锁忙 / 4 需人工),不连 Redis / etcd / data_service,供将来的
// guild-migrate Job 使用(port-decisions D-14 §4,形状照 go/trade)。
var migrateOnly = flag.Bool("migrate", false, "run schemamigrate.Up on mmorpg_guild and exit (0 ok / 1 error / 3 lock busy / 4 manual)")

const migrateRemedyCommand = "guild -f etc/guild.yaml -migrate"

// 启动期等迁移锁:本地 go_services 允许 guild 多开,首次建表时实例间会互等 GET_LOCK。
// 没有编排器替我们重拉,所以在进程内重试。测试里把 lockBusyRetryDelay 置 0。
const lockBusyAttempts = 3

var lockBusyRetryDelay = 2 * time.Second

const nodeType = uint32(base.ENodeType_GuildNodeService)

// economySyncBudget 是捐献 / 兑换提交后同步投递一次的预算上限(05-economy.md §5.25 第 8 步)。
// 实际预算还会被请求截止时间截短(RequestBudget − 1000ms 尾部余量,见 logic.EconomyDeps),
// 不足 300ms 就不同步投递、交给重投循环。与 AssetOp.OpBudgetMs 默认值同为 2500ms 只是巧合:
// ProcessOne 内部自己按 OpBudget 切投递 / 落库,这里只限"本请求最多陪它等多久"。
const economySyncBudget = 2500 * time.Millisecond

var maintenancePlan = flag.String("maintenance-drain-plan", "", "正式 typed 排空 runtime 原件")
var maintenanceDir = flag.String("maintenance-drain-dir", "", "私有排空原件目录")
var maintenanceSHA = flag.String("maintenance-drain-sha256", "", "runtime 原文件 SHA256")
var maintenanceControlDir = flag.String("maintenance-drain-control-dir", os.Getenv("MMORPG_MAINTENANCE_CONTROL_DIR"), "同一 boot 接收排空请求的私有目录")

func main() {
	flag.Parse()
	// 版本行直写 stdout,先于读配置与 logx 初始化:进程在配置 / 依赖阶段就崩溃时也已留下"跑的是哪一版"
	// (shared/buildinfo;理由同 cpp/nodes/gate/gate_version.h 头注释)。
	fmt.Println(buildinfo.StartupLine("guild"))
	if *showVersion {
		return
	}
	conf.MustLoad(*configFile, &config.AppConfig)

	// -migrate:只建表然后退出,不碰 Redis / etcd / Kafka。
	if *migrateOnly {
		os.Exit(runMigration(config.AppConfig))
	}
	// 全局历练不能在结果topic只有单副本时先注册服务、开放开战，再后台重试。
	if err := ensureTrialResultTopic(config.AppConfig, kafkautil.EnsureTopics); err != nil {
		logx.Must(fmt.Errorf("guild global result topic durability: %w", err))
	}

	runtime, root, relay, err := serverbase.LoadPublicationRuntime(*maintenancePlan, *maintenanceDir, *maintenanceSHA)
	if err != nil {
		panic(err)
	}
	writer, err := authoritysql.LoadWriterConfig(os.Getenv)
	if err != nil {
		panic(err)
	}
	svcCtx := svc.NewServiceContextWithAdmission(config.AppConfig, writer, root, relay)

	// 主生命周期接管后，只有真实 join 成功才释放外层依赖。
	ownerReady, shutdownComplete := false, false
	defer func() {
		if !ownerReady {
			svcCtx.Stop()
		}
	}()

	// 帮会规则表 / 等级表自洽校验(设计 docs/design/guild-phase2/02-management.md §3.3):
	// 缺行或数值越界一律拒启,**不降级成警告**。
	//
	// 理由:成员上限、长老上限、申请上限全部是"用时现查配表",查不到时这些判据会静默取到 0,
	// 表现成"谁都进不去 / 长老名额无限",而不是一条报错 —— 带着错配表活着比崩掉危险得多。
	// 位置:NewServiceContext 刚 LoadTables,所以校验只能在它之后;又放在建表与节点注册之前,
	// 配置不对的实例连 etcd 都不该注册进去,免得流量先被路由过来再出问题。
	if err := logic.ValidateGuildTables(); err != nil {
		logx.Must(fmt.Errorf("guild config tables invalid: %w", err))
	}
	// 帮会经济(B5b)配表:GuildDonate / GuildShop / 物品堆叠,以及 GuildRule 的资产列
	// (asset_op_deadline_seconds、asset_op_retry_base_ms)。同样拒启不降级:截止时长查成 0 = 捐献永不中止,
	// 退避基数查成 0 = 重投循环构造失败。它内部会先重跑 validateGuildTables,与上一条重复无害。
	if err := logic.ValidateEconomyTables(); err != nil {
		logx.Must(fmt.Errorf("guild economy tables invalid: %w", err))
	}
	// 帮会活动(B6a)配表:GuildActivity 全表 + GuildRule 的三列活动参数(06-activities.md §6.2.4、90 X-16)。
	// 同样拒启不降级:同类型两行档期重叠 = 帮会资金发两次;奖励物品不在 Item 表 = scene 永久拒收而帮贡已经发出。
	// 这类错误在运行期只会静默发生、且不可逆。
	if err := activity.ValidateTables(); err != nil {
		logx.Must(fmt.Errorf("guild activity tables invalid: %w", err))
	}

	// 表结构不对的实例不能对外服务,也不能注册进 etcd:建表 / 核对放在节点注册之前。
	if err := ensureSchema(context.Background(), svcCtx.DB, config.AppConfig, schemamigrate.Up, schemamigrate.Plan); err != nil {
		logx.Must(err)
	}

	// Initialize data repo with singleflight + cache-aside
	repo := data.NewGuildRepo(svcCtx.RedisClient, svcCtx.DB, config.AppConfig.Cache.DefaultTTL)
	if err := repo.BindPublication(relay); err != nil {
		panic(err)
	}
	// 合服映射(docs/design/player-data-tidb-scaling-plan.md §7.1):同区比较、按区查询、公会榜都按它解析。
	// guild 没有全局库连接,从 data_service 取,读不到拒绝启动。放在榜单重建之前:重建要按它算最终区。
	mergedZones, err := svc.LoadMergedZones(context.Background(), svcCtx.DataServiceClient)
	if err != nil {
		logx.Must(fmt.Errorf("guild merged zones: %w", err))
	}
	repo.BindMergedZones(mergedZones)
	mergedZones.Start()
	defer mergedZones.Stop()
	sqlBackend := authoritysql.Backend(config.AppConfig.MySQL.Backend)
	if err := repo.ConfigureRoleAuthority(config.AppConfig.AccountRoleSchema, sqlBackend); err != nil {
		logx.Must(err)
	}
	if err := repo.ConfigureMatchAuthority(config.AppConfig.MatchAuthoritySchema, sqlBackend); err != nil {
		logx.Must(err)
	}
	loginRedis, err := playerredis.New(config.AppConfig.LoginRedis.ClientConfig(playerredis.Config{
		PoolSizePerNode: 8, DialTimeoutMs: 5000, ReadTimeoutMs: 3000, WriteTimeoutMs: 3000, PoolTimeoutMs: 1000,
	}))
	if err != nil {
		logx.Must(fmt.Errorf("guild LoginRedis config: %w", err))
	}
	defer func() {
		if !ownerReady || shutdownComplete {
			loginRedis.Close()
		}
	}()
	loginProbe, loginCancel := context.WithTimeout(context.Background(), 3*time.Second)
	loginErr := loginRedis.Ping(loginProbe).Err()
	loginCancel()
	if loginErr != nil {
		logx.Must(errors.New("显式全局LoginRedis不可用"))
	}

	// 数据库版本下限(纵深防御,2026-09-21 死锁修复第四轮;判定与理由见 data/server_version.go):
	// 帮会写事务"不成环"的推演只对 TiDB(悲观 + RC)与 MySQL 8.0.29+ 成立 —— 审批通过重插删除标记成员行的 S → X 升级
	// 依赖 InnoDB Bug #11745929 的修复。低于下限、MariaDB 或版本串解析不出一律拒启,不降级成告警:
	// 带着会成环的锁语义上线,出事时只表现为偶发的 1213 与"稍后重试",很难回溯到版本。
	// 版本不对的实例不该注册进 etcd。
	verCtx, verCancel := context.WithTimeout(context.Background(), 5*time.Second)
	dbVersion, verErr := repo.CheckServerVersion(verCtx)
	verCancel()
	if verErr != nil {
		logx.Must(fmt.Errorf("guild database version check: %w", verErr))
	}
	logx.Infof("[guild] Database version %s meets minimum requirement (TiDB or MySQL 8.0.29+)", dbVersion)

	// Register node with etcd
	host, port, err := splitHostPort(config.AppConfig.ListenOn)
	if err != nil {
		logx.Must(fmt.Errorf("parse listen address: %w", err))
	}
	host, err = nodeAdvertiseHost(host)
	if err != nil {
		logx.Must(err)
	}

	// 写进 NodeInfo 的是对外可拨的 IP,不是监听地址:K8s 上 ListenOn 是 0.0.0.0(监听所有网卡),原样写进去路由服拨不通
	// (2026-10-16 补 K8s 清单时发现;本地 127.0.0.1 不受影响)。口径与 trade / chat / client_rpc_router 相同。
	n, err := node.NewNode(nodeType, advertisedHost(host), port)
	if err != nil {
		logx.Must(fmt.Errorf("create node: %w", err))
	}
	defer func() {
		if !ownerReady || shutdownComplete {
			n.Close()
		}
	}()

	if err := n.KeepAlive(); err != nil {
		logx.Must(fmt.Errorf("keep alive: %w", err))
	}
	logx.Infof("Guild node registered: id=%d uuid=%s", n.Info.NodeId, n.Info.NodeUuid)

	// 通过 shared/snowflakealloc 独立申领 Snowflake 槽位(worker id = cluster<<12 | slot)。
	//
	// 注意:**不再用 n.Info.NodeId 当 Snowflake worker id**。
	// 理由:
	//   1. NodeInfo.NodeId 是 C++ 服务发现使用的逻辑节点编号,有可能因为 reRegister
	//      CAS 失败而变化(参考 scene_manager 的 reRegister 设计)。
	//   2. Snowflake worker id 一旦变化,可能在同一毫秒里和上一个 worker id 的 ID 序列冲突
	//      (理论上不会,但毫秒级时钟回退 + worker id 跳变是公认的潜在风险)。
	//   3. 同 hostname 优雅重启复用同槽,Snowflake 时间戳单调性更稳。
	//   4. 与 scene_manager 模式一致,降低维护成本。
	//
	// kind="guild" 与其他服务不同,槽池互相隔离;ClusterId 由运维按集群设定(默认 0)。
	// LegacyPrefix="/guild" 让 cluster 0 在滚动升级期仍读旧布局的水位 / 活 id(发布一版后可删)。
	etcdCli, err := clientv3.New(clientv3.Config{
		Endpoints:   config.AppConfig.Registry.Etcd.Hosts,
		DialTimeout: config.AppConfig.Registry.Etcd.DialTimeout,
	})
	if err != nil {
		logx.Must(fmt.Errorf("snowflake etcd client: %w", err))
	}
	defer func() {
		if !ownerReady || shutdownComplete {
			etcdCli.Close()
		}
	}()

	host_name, err := os.Hostname()
	if err != nil {
		logx.Must(fmt.Errorf("hostname: %w", err))
	}
	sfCtx, sfCancel := context.WithTimeout(context.Background(), 10*time.Second)
	sfHandle, err := snowflakealloc.AllocateWithKeepAlive(sfCtx, etcdCli, "guild", host_name, snowflakealloc.Options{
		LeaseTTL:     60,
		ClusterID:    config.AppConfig.ClusterId,
		LegacyPrefix: "/guild",
		// 缓存文件名带监听端口:同主机多实例各自一份,不互相覆盖。
		CachePath: snowflakealloc.DefaultCachePath(config.AppConfig.SnowflakeCacheDir, "guild",
			fmt.Sprintf("%s_%d", host_name, port)),
	})
	sfCancel()
	if err != nil {
		logx.Must(fmt.Errorf("snowflake worker id alloc: %w", err))
	}
	defer func() {
		if !ownerReady || shutdownComplete {
			sfHandle.Close()
		}
	}()
	logx.Infof("Guild snowflake %s worker_id=%d (host=%s)", sfHandle.LogFields(), sfHandle.WorkerID, host_name)

	// RPC 级热关停(shared/killswitch):线上某个方法把 DB 打爆时,往 etcd 写一个
	// key 就能秒级把它短路掉,不必走一遍构建-发布-滚动更新。
	//
	// 复用上面这个 etcdCli(snowflake worker id 分配用的那条连接),不另开第三条 ——
	// 连的是同一个集群。Start 非阻塞:客户端为 nil、etcd 连不上、前缀下没有 key,
	// 一律放行(fail-open),因此这里既不需要判错也不需要 logx.Must。
	ksCtx, ksCancel := context.WithCancel(context.Background())
	// ⚠️ 这个 defer 必须写在 `defer etcdCli.Close()` 之后:defer 是后进先出,
	// 先取消 watch 循环,再关 etcd 客户端,否则 watch 会撞上一个已经 Close 的 client。
	defer ksCancel()
	ks := killswitch.New(killswitch.Config{Prefix: config.AppConfig.KillSwitchPrefix})
	ks.Start(ksCtx, etcdCli)

	// 用 Handle.NewNode 而不是裸 snowflake.NewNode:它会把**前任在这个 worker id 上的
	// 高水位**当地板注入(etcd 里的持久水位),顶住跨机时钟偏斜接管、本机时钟回拨、
	// 前任借过逻辑秒这三类"启动 guard 挡不住"的重号。
	sf := sfHandle.NewNode()

	// repo 已在建表之后创建。
	// 每次启动从 MySQL 权威快照重建全局 / 分区榜;失败拒绝启动,避免对外提供不完整榜单。
	if err := repo.RebuildRanks(context.Background()); err != nil {
		panic(fmt.Errorf("rebuild guild ranks from MySQL: %w", err))
	}
	onlineResolver := logic.NewOnlineStatusResolver(svcCtx.PlayerRedis)

	// guild_id 只由号段发(svcCtx.GuildIDSegment,IdSegment.Enabled 时已在 NewServiceContext
	// 里接好 data_service),号段失败即建帮失败。
	guildIDs := svc.NewGuildIDMinter(svcCtx.GuildIDSegment)
	if finish, err := root.Enter(); err == nil {
		func() { defer finish(); svcCtx.WarmGuildIDSegment() }()
	}

	// ── 帮会经济:资产通道(B5b,docs/design/guild-phase2/05-economy.md §5.29)──
	// 经济仓储与 outbox Store 无论通道开关都建:升级与两个读接口不经资产通道,关着通道也照常服务;
	// 清理任务也只依赖 Store。
	economyRepo, err := data.NewEconomyRepo(repo)
	if err != nil {
		logx.Must(fmt.Errorf("guild economy repo: %w", err))
	}
	assetStore, err := data.NewGuildAssetStore(repo)
	if err != nil {
		logx.Must(fmt.Errorf("guild asset store: %w", err))
	}

	// op_id 发号器。号段关着时 svcCtx.AssetOpIDSegment 是 nil *Client:必须让 assetOpIDs 保持
	// **nil 接口**,不能把 nil 指针包进 Minter 或直接赋给接口 —— 那样 logic 里 `OpIDs == nil` 为 false,
	// 每次捐献都会去调一个空客户端。op_id 是 outbox 主键,
	// 号段失败就整体拒绝(kGuildIdGenUnavailable),绝不自造 id。
	var assetOpIDs logic.IDMinter
	if svcCtx.AssetOpIDSegment != nil {
		assetOpIDs = &idsegment.Minter{Name: svc.AssetOpBizTag, Segment: svcCtx.AssetOpIDSegment}
	}

	// 资产通道只在 AssetOp.Enabled 时装配(裁决 D):关着时不建签名器 / 定位件 / 循环,
	// EconomyDeps.Loop 为 nil,捐献与兑换在发号与建行之前就回 kGuildAssetPending。
	// 开着时密钥缺失 / 过短、循环参数非法都在这里拒启(资产路径 fail-closed,不降级)。
	var assetPipe *svc.AssetPipeline
	if config.AppConfig.AssetOp.Enabled {
		// 配置与配表的交叉约束(插行租约 vs 捐献截止、退避基数 vs 封顶):两边各自校验都过、组合起来却会让
		// 同步投递被跳过的捐献未扣款即被中止。只在通道开启时判 —— 关着时不插行也不起循环。
		assetOp := config.AppConfig.AssetOp
		if err := logic.ValidateAssetOpTiming(
			time.Duration(assetOp.LeaseMs)*time.Millisecond,
			time.Duration(assetOp.ReconcileIntervalMs)*time.Millisecond,
			time.Duration(assetOp.MaxBackoffMs)*time.Millisecond,
		); err != nil {
			logx.Must(fmt.Errorf("guild asset op timing invalid: %w", err))
		}
		assetPipe, err = svc.NewAssetPipeline(config.AppConfig.AssetOp, logic.AssetOpRetryBase(),
			svcCtx.PlayerRedis, svcCtx.PlayerLocatorRedisClient, assetStore)
		if err != nil {
			logx.Must(fmt.Errorf("guild asset pipeline: %w", err))
		}
		// 已落盘账本(在线 / 离线终结与自动放弃都靠它)必须紧跟建管线、在把 Loop 交给同步投递路径
		// (下面的 EconomyDeps.Loop / ActivityDeps.Loop)与 Start 之前接上 —— Loop.Ledger 是无锁字段。
		// 没配 DataServiceRpc 时随后的 CheckWiring 拒启。
		assetPipe.Loop.Relay = relay
		assetPipe.AttachPersistedLedger(svcCtx.DataServiceClient)
		if err := assetPipe.Loop.CheckWiring(); err != nil {
			logx.Must(fmt.Errorf("guild asset pipeline wiring: %w", err))
		}
		// 日对账 + 自动放弃(落地恢复 §6.2 / §6.3)的选主:锁在 guild 全局 Redis(全体副本共享),只有当选副本跑维护。
		// 同样必须在 Start 之前接上。竞选者标识用节点 uuid(每个进程实例唯一,与历练巡检租约的持有者标识同源)。
		// 自动放弃另需 Loop.Ledger(上一行)。
		assetPipe.AttachMaintenanceLeader(svcCtx.RedisClient, n.Info.NodeUuid)
		if finish, err := relay.Enter(); err == nil {
			func() { defer finish(); svcCtx.WarmAssetOpIDSegment() }()
		}
	} else {
		// go-zero logx 没有 WARN 级;沿用本文件"降级形态打一条 ERROR"的写法(同 DataServiceRpc 未配置)。
		logx.Error("Guild: AssetOp.Enabled=false; asset channel disabled: guild donations / shop exchanges all return kGuildAssetPending, " +
			"upgrades and read APIs remain available (08-save-owner-fence.md section 8.3 gates must pass before enabling in shared / staging environments)")
	}
	economy := logic.EconomyDeps{
		Repo:       economyRepo,
		OpIDs:      assetOpIDs,
		Now:        time.Now,
		SyncBudget: economySyncBudget,
		// 插行租约与重投循环同一个值(裁决 G):通道关着时这里是 0,logic 取自己的默认值,但那时也不会插行。
		Lease: time.Duration(config.AppConfig.AssetOp.LeaseMs) * time.Millisecond,
	}
	if assetPipe != nil {
		economy.Loop = assetPipe.Loop
	}

	// ── 帮会活动(B6a:元宵灯会 / 中秋团圆,docs/design/guild-phase2/06-activities.md §6.6)──
	// **无条件装配,不设配置开关**:活动开不开由配表决定(GuildActivity.enabled 与档期),关着时写 RPC 回
	// kGuildActivityNotOpen、视图显示 DISABLED,客户端看到的是一句明确的提示。依赖经 WithActivities 写进
	// GuildLogic.activities 字段(90 Y-02,不是包级状态)。反过来不装配,五个 RPC 虽也回 kGuildActivityNotOpen
	// (logic.activitiesNotWiredTip),但那是"装配被改坏"的兜底:每次打 ERROR、活动页整页拿不到、故障指标看不见 ——
	// 它不是开关,"关活动"只许改表(紧急止血另有 killswitch)。
	// 物品奖励与经济共用同一条资产通道:同一个 Loop(AssetOp.Enabled=false 时为 nil → 带物品的活动在发号前
	// 回 kGuildAssetPending,不带物品的灯会照常)、同一个 op_id 发号器、同一个插行租约。
	activityRepo, err := data.NewActivityRepo(repo)
	if err != nil {
		logx.Must(fmt.Errorf("guild activity repo: %w", err))
	}
	// ── 同道历练(B6b,06-activities.md §6.22–§6.33)──
	// 邀请SQL权威与结果记录仓储无条件装配：历练此刻开不开,只由下面的
	// trialMatch 是不是 nil 决定;而结算、巡检器、待入队循环在历练关着时也要跑(关之前留下的在途对局与待入队物品
	// 仍要处理完),它们需要这两个仓储。
	//   - 邀请/同意/占位使用同TiDB原角色P锁，Redis只保留巡检租约，清空不会丢失原件；
	//   - 结果记录读 match 权威库的 SQL 终局(MatchAuthoritySchema,启动期已核列);
	//   - 战斗锁在玩家域(PlayerRedis):它必须与 scene 写 battle:lock:{id} 的是同一个存储域。
	trialLobby, err := data.NewSQLTrialLobbyRepo(svcCtx.RedisClient, repo, guildIDs.Mint)
	if err != nil {
		logx.Must(fmt.Errorf("guild trial lobby repo: %w", err))
	}
	trialResults, err := data.NewTrialResultRecords(repo)
	if err != nil {
		logx.Must(fmt.Errorf("guild trial result records: %w", err))
	}
	// **没有结算就不许开战**:结果消费跑不起来(没显式开 Activity.TrialResult.Enabled,或没配 Kafka.Brokers)时,
	// 把 Match 留成 nil 接口 → 视图显示未开放、两个历练写 RPC 回 kGuildActivityNotOpen。否则玩家打完一局,
	// 帮贡、资金、物品都无人发放。
	// 注意 nil 接口的坑(同上面的 assetOpIDs):svcCtx.MatchInternal 本身是接口类型,没配 MatchRpc 时
	// 是 nil 接口;这里显式判过再赋值,trialMatch 要么是可用的客户端、要么是 nil 接口,不会出现"装着 nil 指针的非 nil 接口"。
	trialResultActive := config.AppConfig.TrialResultActive()
	matchConfigured := svcCtx.MatchInternal != nil
	var trialMatch logic.TrialBattleStarter
	if trialResultActive && matchConfigured {
		trialMatch = svcCtx.MatchInternal
	}
	var trialCancellation logic.TrialCancellation
	if matchConfigured {
		trialCancellation, err = logic.NewTrialCancellation(svcCtx.MatchInternal, os.Getenv("MMORPG_MATCH_INTERNAL_SECRET_GUILD"))
		if err != nil {
			logx.Must(errors.New("历练原请求取消签名依赖未就绪"))
		}
	}
	activities := logic.ActivityDeps{
		Repo:         activityRepo,
		Loop:         economy.Loop,
		OpIDs:        assetOpIDs,
		Now:          time.Now,
		SyncBudget:   economySyncBudget,
		Lease:        economy.Lease,
		Lobby:        trialLobby,
		Match:        trialMatch,
		Cancellation: trialCancellation,
		Results:      trialResults,
		BattleLocks:  svcCtx.PlayerRedis,
		// 没配 MatchRpc 时 Timeout 是 0,logic 取自己的默认值(1500ms),但那时也不会调 match。
		MatchBudget: time.Duration(config.AppConfig.MatchRpc.Timeout) * time.Millisecond,
	}
	logx.Infof("[guild] Guild activities wired (Lantern Festival / Mid-Autumn Reunion / Guild Trial): item_reward_channel=%t trial_enabled=%t (MatchRpc_configured=%t result_consumer=%t)",
		activities.Loop != nil, trialMatch != nil, matchConfigured, trialResultActive)
	logTrialClosedReason(config.AppConfig, matchConfigured)

	// 帮会按 zone 隔离:客户端请求的 zone 取 data_service 的玩家归属映射(logic/home_zone.go)。
	// 成员 / 帮主展示名走同一个 data_service 客户端(logic/player_name_resolver.go,B3b)。两者语义不同 ——
	// 归属区查不到要拒绝请求(fail-closed),名字取不到只是少显示几个字(fail-open)。
	// 走到这里 DataServiceClient 一定不是 nil:上面读合服映射时没有客户端已经拒绝启动。
	homeZones := logic.NewDataServiceHomeZone(svcCtx.DataServiceClient, logic.DefaultHomeZoneLookupTimeout)
	playerNames := logic.NewDataServicePlayerNames(svcCtx.DataServiceClient, logic.DefaultPlayerNameLookupTimeout)
	// 推送出口与"申请推送冷却"在这里一次装配(设计 §13.7 / §10.1)。
	//
	// NewGuildNotifier 自己判 nil:没配 Kafka(Brokers 为空 → KafkaWriter 为 nil)时它退回
	// NoopNotifier,所以这里不必再包一层 if:这里拿到的已经是接口值,没有"具体类型的 nil 指针装进接口后 != nil"这个坑。
	//
	// TryMarkApplyPush 是 Redis SetNX 冷却键,挡的是"申请 → 撤回 → 申请"对审批人的刷屏;
	// 不注入 = 总是放行,只有单测才会走那条默认。
	//
	// WithPlayerNames 收到 nil 接口时什么都不做(90-consistency.md Y-02:依赖只经函数式 Option 一次装配)。
	//
	// WithSyncBumper:落地恢复摘要(docs/design/landing-recovery-sync-digest.md §3)。guild 的所有 bump 点(含资产终结的
	// OnAssetFinalized)都经 logic 的 syncBumper 发出,**不接这一项,guild 四个域的 bump 全不生效**(静默退化为 Noop)。
	// svcCtx.PlayerSyncBumper 由 NewServiceContext 无条件建好(指向核心 Redis DB 0 的专用客户端),NewGuildSyncBumper 自己判 nil。
	// 配额不能挤占正常仓储池；go-redis 必须启用 context 截止及短套接字/取连接预算。
	quotaRedis := redis.NewClient(&redis.Options{
		Addr: config.AppConfig.RedisClient.Host, Password: config.AppConfig.RedisClient.Password, DB: config.AppConfig.RedisClient.DB,
		PoolSize: 4, MaxActiveConns: 4, MaxRetries: -1, DialerRetries: 1, ContextTimeoutEnabled: true,
		DialTimeout: logic.ApplicationQuotaBudget, ReadTimeout: logic.ApplicationQuotaBudget,
		WriteTimeout: logic.ApplicationQuotaBudget, PoolTimeout: logic.ApplicationQuotaBudget,
	})
	defer func() {
		if !ownerReady || shutdownComplete {
			quotaRedis.Close()
		}
	}()
	applicationQuota, err := logic.NewApplicationQuota(quotaRedis, config.AppConfig.ApplicationQuotaPerMinute)
	if err != nil {
		logx.Must(err)
	}
	guildLogic := logic.NewGuildLogic(repo, guildIDs, onlineResolver, homeZones,
		logic.WithApplicationQuota(applicationQuota),
		logic.WithNotifier(logic.NewGuildNotifier(svcCtx.KafkaWriter, svcCtx.GateCommandBuilder, svcCtx.PlayerRedis, relay)),
		logic.WithApplyPushGate(repo.TryMarkApplyPush),
		logic.WithPlayerNames(playerNames),
		logic.WithEconomy(economy),
		logic.WithActivities(activities),
		logic.WithSyncBumper(logic.NewGuildSyncBumper(svcCtx.PlayerSyncBumper, relay)))

	// 终结推送回调。必须在重投循环与 gRPC server 启动**之前**赋值:此后只读,不需要同步。
	// 晚于它们赋值 = 启动初期被循环终结的行不推送,且与读它的 worker 构成数据竞争。
	assetStore.OnFinalized = guildLogic.OnAssetFinalized

	// Start gRPC server
	serverRegistered := make(chan struct{})
	s := zrpc.MustNewServer(config.AppConfig.RpcServerConf, func(grpcServer *grpc.Server) {
		defer close(serverRegistered)
		pb.RegisterGuildServiceServer(grpcServer, server.NewGuildServer(guildLogic))
		// 帮会内部服务(B5d-2a,docs/design/guild-phase2/07-rollback-fail-closed.md §7.4.4):data_service 回档前检查用的只读 RPC。
		// 方法不进 session.ClientMethods,客户端来源一律 PermissionDenied;拦截器链对它同样生效。
		// assetStore 在上面无条件建好(logx.Must 兜底),与通道开关无关:回档检查读的是历史终态行,通道关着也要答得出来。
		// 保留期是"可证明窗口"的下界,用 svc.RollbackProofRetention 取:配了值时与清理用的换算相同;
		// AssetOp 整段缺失(通道关闭的合法形态,字段是 0)时回落到默认值 —— 原样传 0 会让这个 RPC 恒回 Unavailable,
		// data_service 的所有回档被拒且放行无效,上一行说的"通道关着也要答得出来"就不成立了。
		pb.RegisterGuildInternalServer(grpcServer, server.NewGuildInternalServer(assetStore,
			svc.RollbackProofRetention(config.AppConfig.AssetOp), time.Now))
		if config.AppConfig.Mode == service.DevMode || config.AppConfig.Mode == service.TestMode {
			reflection.Register(grpcServer)
		}
	})
	// 必须在 MustNewServer 之后:go-zero 指标的全局开关由它内部的 SetUp → StartAgent 打开,之前写的样本会被丢弃
	// (理由与告警口径见 server.PrimeGuildInternalMetrics)。
	server.PrimeGuildInternalMetrics()
	svc.PrimeMergedZoneMetrics()
	s.AddUnaryInterceptors(serverbase.RootAdmission(root))
	s.AddUnaryInterceptors(buildUnaryInterceptors(ks, config.AppConfig.RequestBudget(), session.OriginalIdentityInterceptor(loginRedis, repo.CheckOriginalRole))...)
	defer s.Stop()

	// 资产通道后台 goroutine(guild.scene_node_watch / guild.asset_op_reconcile / guild.asset_op_cleanup,
	// 以及落地恢复的 guild.asset_op_maintenance(选主后的日对账 + 自动放弃)与 assetopwake 的唤醒订阅)。
	//
	// ⚠️ 它们的 stop 必须写在 `defer s.Stop()` 之后:defer 后进先出,进程退出时它们**最先**执行,
	// 从而早于 ksCancel、sfHandle.Close、etcdCli.Close、n.Close 与 svcCtx.Stop(关 MySQL / Redis)。
	// 此时 s.Start 已经返回(gRPC 在途请求已排空),不会再有同步投递用到 Loop。
	// 反过来的后果:循环落库那一步用 WithoutCancel + 700ms,取消拦不住,DB 先关就写不回终局,
	// 行留在 PENDING、下一轮再投一次;watcher 撞上已 Close 的 etcd 客户端同理。
	// 两个 stop 都会**等** goroutine 真正退出才返回;写在 s.Stop 之前执行还让它们的停机日志不被 logx.Close 吞掉。
	if assetPipe != nil {
		assetPipe.Start(context.Background(), etcdCli)
		defer assetPipe.Stop()
	}
	if config.AppConfig.AssetOp.CleanupEnabled {
		stopCleanup := startAssetOpCleanup(assetStore, svc.CleanupConfFrom(config.AppConfig.AssetOp), root)
		if err := svcCtx.BindSourceStop(func(ctx context.Context) error {
			done := make(chan struct{})
			go func() { stopCleanup(); close(done) }()
			select {
			case <-done:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}); err != nil {
			panic(err)
		}
		defer stopCleanup()
	}

	// 同道历练的后台(B6b,06-activities.md §6.28 / §6.32 / §6.33):待入队物品循环、巡检器、对局结果消费者。
	//
	// 位置约束有三条,都与上面的资产通道同理:
	//   1. 在 MustNewServer 之后启动:它们一跑就写指标(guild_trial_*),go-zero 指标开关打开之前写的样本会被丢弃;
	//   2. 在 assetStore.OnFinalized 赋值与 assetPipe.Start 之后启动:结算提交后会同步投递物品指令(Loop.ProcessOne),
	//      指令终结时要读那个回调;
	//   3. 它们的 stop 写在 assetPipe.Stop 的 defer **之后**(后进先出 → 先于它执行):结算的同步投递用的就是那条 Loop,
	//      使用者要先停;又都早于 svcCtx.Stop(关 MySQL / Redis)。每个 stop 都会等 goroutine 真正退出才返回 ——
	//      正在进行的结算事务随 ctx 取消回滚,那条消息的位点没有提交,重启后重放,由幂等闸门兜住。
	//
	// 后台两个循环**无条件跑**(不看历练此刻开没开,理由见上面的活动依赖装配)。它们没有外部开关,
	// 起不来只可能是装配错误,所以拒启。
	stopTrialBackground, err := guildLogic.StartTrialBackground(logic.TrialBackgroundConf{
		RelayAdmission: relay,
		OwedInterval:   config.AppConfig.Activity.OwedLoopInterval(),
		SweepInterval:  config.AppConfig.Activity.TrialSweepInterval(),
		ResultOverdue:  config.AppConfig.Activity.TrialResultOverdue(),
		Abandon:        config.AppConfig.Activity.TrialAbandon(),
		// 巡检租约的持有者标识:节点 uuid 每个进程实例唯一,排障时看 guild:trial:sweep:lease 的值就知道是谁在扫。
		Owner: n.Info.NodeUuid,
	})
	if err != nil {
		logx.Must(fmt.Errorf("guild trial background loops: %w", err))
	}
	defer stopTrialBackground()

	// 结果消费者只看"结果消费是否打开"(TrialResultActive),**不看 MatchRpc 配没配**:运维删掉 MatchRpc 来关历练时,
	// 关之前已经开打的对局仍要结算。全局模式已在注册前严格核验topic；
	// 仅显式本地旧MySQL模式保留首次连接失败后后台每30s恢复的兼容行为。
	stopTrialResults := func() {}
	if trialResultActive {
		var err error
		stopTrialResults, err = startTrialResultConsumerWithOwner(config.AppConfig, guildLogic, relay, svcCtx.BindConsumerOwner)
		if err != nil {
			logx.Must(fmt.Errorf("guild trial result consumer: %w", err))
		}
		defer stopTrialResults()
	}

	if err := svcCtx.BindRelayOwners(repo, func(closeCtx context.Context) error {
		done := make(chan struct{})
		go func() { stopTrialBackground(); stopTrialResults(); close(done) }()
		select {
		case <-done:
		case <-closeCtx.Done():
			return closeCtx.Err()
		}
		return assetPipe.StopAndReport(closeCtx)
	}); err != nil {
		panic(err)
	}

	// Lost() 关闭 = 本进程**确认**不再是这个槽的持有者(slots key 被挂到了别的 uuid 上:
	// 运维手动清理 / 水位机制被绕过 / 旧版本二进制)。再用 sf 发一个公会 ID 就是确定性撞号,
	// 所以主动退出,让编排把进程拉起来 —— 重启会申领一个新槽,并以前任水位为地板。
	//
	// ⏱ 时间预算:lease 抖动 / etcd 不可达**不再**触发 Lost()(snowflakealloc 会自己 reclaim);
	// 这段时间的安全由发号器内部的水位年龄自 fence 兜住 —— 距上次水位写成功超过 F(2h)
	// Generate 返回 ErrWatermarkStale(暂态,写成功即恢复),建帮整体失败但进程不退出。
	//
	// ⚠️ 顺序不能反,而且**不能只调 s.Stop()**:go-zero 的 zrpc.RpcServer.Stop() 实测
	// (v1.9.2 / v1.10.0 同)只有一行 logx.Close(),既不拒新请求也不排空在途 ——
	// 靠它"停服"等于什么都没做,进程会带着已失效的 worker id 一直服务下去。
	// 所以正确性由 Fence() 保证(之后 Generate 一律 ErrFenced,建帮整体失败),
	// 可用性由进程退出 + 编排重拉保证。
	//
	// 用 safego.Go 而不是裸 `go func`:这条看门狗一旦 panic(比如 Lost() 通道被
	// 重复关闭),裸 goroutine 会把整个进程当场打死,日志里只剩一段 runtime 栈;
	// safego 兜住后会打稳定事件名 + 计 safego_panic_total{point="guild.snowflake_fence_watch"},
	// 看门狗失效这件事变成可告警的,而不是伪装成一次"正常"的进程退出。
	safego.Go("guild.snowflake_fence_watch", func() {
		<-sfHandle.Lost()
		sf.Fence() // ① 先关闸:此后一个号都发不出去,撞号从机制上不可能
		logx.Error("Guild snowflake worker id lease lost; generator fenced, exiting to let the orchestrator restart " +
			"(zrpc Stop() only closes the logger and cannot stop serving)")
		logx.Close() // ② 冲掉日志缓冲,别把上面这条 ERROR 丢了
		os.Exit(1)   // ③ 退出;重启后拿新租约,并被 snowflake 的启动 guard 兜住
	})

	logx.Infof("Starting Guild RPC server at %s...", config.AppConfig.ListenOn)
	controlCtx, cancelControl := context.WithCancel(context.Background())
	controlDone := make(chan struct{})
	go func() {
		defer close(controlDone)
		if err := serverbase.DriveDrainRuntime(controlCtx, runtime, *maintenanceControlDir, root, relay, svcCtx.BindDrainRuntime, svcCtx.CloseSource, svcCtx.StopAndReport, func(prepareCtx context.Context) (*serverbase.PreparedPublication, error) {
			select {
			case <-serverRegistered:
			case <-prepareCtx.Done():
				return nil, prepareCtx.Err()
			}
			probeHost, probePort, err := splitHostPort(config.AppConfig.ListenOn)
			if err != nil {
				return nil, err
			}
			if probeHost == "" || probeHost == "0.0.0.0" || probeHost == "::" {
				probeHost = "127.0.0.1"
			}
			address := net.JoinHostPort(probeHost, strconv.FormatUint(uint64(probePort), 10))
			ticker := time.NewTicker(50 * time.Millisecond)
			defer ticker.Stop()
			for {
				connection, err := (&net.Dialer{Timeout: time.Second}).DialContext(prepareCtx, "tcp", address)
				if err == nil {
					if err = connection.Close(); err != nil {
						return nil, err
					}
					break
				}
				select {
				case <-prepareCtx.Done():
					return nil, prepareCtx.Err()
				case <-ticker.C:
				}
			}
			if root.Pending() != 0 {
				return nil, drainproof.ErrHold
			}
			if trialResultActive {
				owner := svcCtx.ConsumerOwner
				if owner == nil {
					return nil, drainproof.ErrHold
				}
				bound := svcCtx.DrainRuntime
				expected, err := owner.ExpectedPartitions(bound)
				if err != nil {
					return nil, err
				}
				digest, raw, err := owner.PreparedFrontier(prepareCtx, bound, "SOURCE", expected)
				if err != nil {
					return nil, err
				}
				return &serverbase.PreparedPublication{Digest: digest, Raw: raw, WithReady: func(openCtx context.Context, commit func() ([]byte, error)) ([]byte, error) {
					return kafkautil.WithConsumerReadiness(openCtx, raw, []*kafkautil.BoundConsumerGroup{owner}, commit)
				}}, nil
			}
			return &serverbase.PreparedPublication{NoConsumers: true}, nil
		}); err != nil && controlCtx.Err() == nil {
			logx.Errorf("guild 排空 HOLD: %v", err)
		}
	}()
	defer func() {
		root.CloseAdmission()
		cancelControl()
		<-controlDone
		closeCtx, cancelClose := context.WithTimeout(context.Background(), time.Minute)
		defer cancelClose()
		if err := svcCtx.StopAndReport(closeCtx); err != nil {
			logx.Errorf("guild 退出保留资源，排空 HOLD: %v", err)
			return
		}
		shutdownComplete = true
	}()
	ownerReady = true
	s.Start()
}

// startAssetOpCleanup 起终态资产指令 / 过期计数行的清理 goroutine(guild.asset_op_cleanup,§5.22),
// 返回的 stop 取消它并**等它退出**:清理语句跑在 MySQL 上,必须在 svcCtx.Stop 关库之前停下。
// 与资产通道开关独立:通道关着时历史终态行照样要清。
func startAssetOpCleanup(store *data.GuildAssetStore, c data.CleanupConf, admissions ...*drainproof.Session) (stop func()) {
	var root *drainproof.Session
	if len(admissions) == 1 {
		root = admissions[0]
	}
	if len(admissions) > 1 {
		panic("cleanup SOURCE 重复绑定")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	safego.Go("guild.asset_op_cleanup", func() {
		// close 放在 defer 里:safego 兜住 panic 时它照样执行,stop 不会永远等下去。
		defer close(done)
		if root == nil {
			store.RunCleanup(ctx, c)
			return
		}
		if c.Interval <= 0 {
			root.Hold()
			return
		}
		timer := time.NewTimer(time.Duration(rand.Int64N(int64(c.Interval))))
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			if ctx.Err() != nil {
				return
			}
			safego.Run("guild.asset_op_cleanup", func() {
				finish, err := root.Enter()
				if err != nil {
					return
				}
				defer finish()
				defer func() {
					if recovered := recover(); recovered != nil {
						root.Hold()
						panic(recovered)
					}
				}()
				work, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
				defer cancel()
				if err := store.CleanupOnce(work, time.Now(), c); err != nil {
					root.Hold()
					logx.Errorf("guild cleanup HOLD: %v", err)
				}
			})
			timer.Reset(c.Interval)
		}
	})
	logx.Infof("[guild] Asset cleanup started: goroutine guild.asset_op_cleanup interval=%v terminal_retention=%v counter_retention=%v",
		c.Interval, c.TerminalRetention, c.CounterRetention)
	return func() {
		cancel()
		<-done
	}
}

// trialResultsPoint 是历练结果消费 goroutine 的 safego 点位名(safego_panic_total 的 label,必须是常量)。
const trialResultsPoint = "guild.kafka.trial_results"

// startTrialResultConsumer 起历练对局结果的消费 goroutine(guild.kafka.trial_results;06-activities.md §6.28):
// Kafka topic match-results、消费组 guild-trial,只把帮会同道历练的结果交给 SettleTrialResult。
// 返回的 stop 取消它并**等它退出**(契约同 startAssetOpCleanup):结算跑在 MySQL / Redis 上,必须在关库之前停下。
//
// 返回 error 只可能是参数非法(config.Validate 与 TrialResultActive 已经挡过一遍,走到这里是装配错误)。
// **Kafka 连不上不在此列**:消费者自己在后台每 30s 重试确保 topic,真正连上时才打
// "历练结果消费者启动 topic=… group=…"那一行(部署验收看的就是它)。
//
// 接线只有两处:
//   - 结算入口:消费者只关心"提交位点还是退避重调",所以丢掉 SettleTrialResult 的去向、只留 error。
//     幂等闸门、毒消息标记都在 SettleTrialResult 里,这里不重复;
//   - 两个计数出口:指标在 logic 包注册,消费者(internal/kafka)不 import logic,经回调计数。
func startTrialResultConsumer(c config.Config, guildLogic *logic.GuildLogic, admissions ...*drainproof.Session) (stop func(), err error) {
	if len(admissions) > 1 {
		return nil, drainproof.ErrHold
	}
	var relay *drainproof.Session
	if len(admissions) == 1 {
		relay = admissions[0]
	}
	return startTrialResultConsumerWithOwner(c, guildLogic, relay, nil)
}

func startTrialResultConsumerWithOwner(c config.Config, guildLogic *logic.GuildLogic, relay *drainproof.Session, bind func(*kafkautil.BoundConsumerGroup) error) (stop func(), err error) {
	consumerConfig := trialConsumerConfigFrom(c)
	consumerConfig.RelayAdmission = relay
	resultConf := c.Activity.TrialResult
	consumer, err := guildkafka.NewTrialResultConsumer(consumerConfig, func(ctx context.Context, ev *kafkapb.BattleResultEvent) error {
		_, err := guildLogic.SettleTrialResult(ctx, ev)
		return err
	}, guildkafka.TrialResultCounters{
		DecodeError:  logic.CountTrialResultDecodeError,
		HandlerRetry: logic.CountTrialResultHandlerRetry,
	})
	if err != nil {
		return nil, err
	}
	if bind != nil {
		if err = bind(consumer.Owner()); err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	safego.Go(trialResultsPoint, func() {
		// close 放在 defer 里:safego 兜住 panic 时它照样执行,stop 不会永远等下去。
		defer close(done)
		consumer.Run(ctx)
	})
	logx.Infof("[guild] Trial result consumption scheduled: goroutine %s topic=%s group=%s partitions=%d (a separate startup log follows Kafka connection)",
		trialResultsPoint, resultConf.Topic, resultConf.GroupID, resultConf.Partitions)
	return func() {
		cancel()
		<-done
	}, nil
}

func trialConsumerConfigFrom(c config.Config) guildkafka.TrialResultConsumerConfig {
	return guildkafka.TrialResultConsumerConfig{
		Brokers: c.Kafka.Brokers, Topic: c.Activity.TrialResult.Topic,
		GroupID: c.Activity.TrialResult.GroupID, Partitions: c.Activity.TrialResult.Partitions,
		RequireDurability: c.TrialResultRequiresDurability(),
	}
}

func ensureTrialResultTopic(c config.Config, ensure func([]string, []kafkautil.TopicSpec) error) error {
	if !c.TrialResultRequiresDurability() || !c.TrialResultActive() {
		return nil
	}
	cfg := trialConsumerConfigFrom(c)
	return ensure(cfg.Brokers, []kafkautil.TopicSpec{cfg.TopicSpec()})
}

// logTrialClosedReason 在同道历练因为**配置**而关闭时打一行原因(开着时什么都不打)。
// 历练的三个开关分散在三段配置里(MatchRpc、Activity.TrialResult.Enabled、Kafka.Brokers),任何一个没配,
// 玩家看到的都只是一句"活动未开放";这一行让运维不必对着 yaml 猜是哪一个。
// 灯会 / 团圆不受这三个开关影响。配表层面的关闭(GuildActivity.enabled、档期)不归这里管。
func logTrialClosedReason(c config.Config, matchConfigured bool) {
	resultActive := c.TrialResultActive()
	switch {
	case c.Activity.TrialResult.Enabled && !resultActive:
		// 06 §6.22:Enabled 而没有 Brokers 不拒启(本地不起 Kafka 是正常形态),但必须出声。
		// go-zero logx 没有 WARN 级,沿用本文件"降级形态打一条 ERROR"的写法。
		logx.Error("Guild: Activity.TrialResult.Enabled=true but Kafka.Brokers empty; trial result consumption disabled, guild trial closed " +
			"(battles cannot start without settlement); existing in-flight battles rely on the inspector recovering result records")
	case !resultActive && matchConfigured:
		logx.Error("Guild: MatchRpc configured but Activity.TrialResult.Enabled not explicitly enabled; trial result consumption disabled, guild trial closed " +
			"(battles cannot start without settlement); set Enabled: true under Activity.TrialResult to enable trials")
	case !resultActive:
		logx.Info("[guild] Guild trial closed: MatchRpc not configured and Activity.TrialResult.Enabled not enabled")
	case !matchConfigured:
		logx.Info("[guild] Guild trial closed: MatchRpc not configured (result consumption continues; battles started before closure will still settle)")
	}
}

// buildUnaryInterceptors 组装本服务的一元拦截器链。
//
// 返回的切片顺序**就是执行顺序**:go-zero 把它们原样交给
// grpc.ChainUnaryInterceptor,第一个是最外层。
//
// 抽成函数而不是直接在 main 里连着调 AddUnaryInterceptors,是为了让链本身可测 ——
// main() 没法在单测里跑起来,而"哪次重构顺手把 killswitch 那行删了"是最容易发生、
// 又最难被发现的回归:开关删掉之后一切照常工作,只有真出事那天才发现止血阀是假的。
// 见 guild_test.go。
func buildUnaryInterceptors(ks *killswitch.Switch, budget time.Duration, identity ...grpc.UnaryServerInterceptor) []grpc.UnaryServerInterceptor {
	chain := []grpc.UnaryServerInterceptor{
		// ① 流量统计放最外层:被热关停短路掉的请求也必须被统计到。
		//    否则"关停生效后这个方法的 QPS 归零"会被误读成客户端不再调用了。
		grpcstats.New(grpcstats.Options{}).UnaryServerInterceptor(),

		// ② 热关停紧跟其后,尽早短路:命中之后 in-band 定性、handler、
		//    以及 handler 里的 Redis / MySQL / 发号器访问统统不做 —— 止血阀的全部意义
		//    就是"别为一个已经关停的方法做任何无谓的工作"。
		//    它放在 serverbase 之前也意味着被关停的调用不进 rpc_duration_seconds:
		//    那条指标衡量的是业务链路,而关停请求根本没走业务链路,
		//    它们的账记在 killswitch_blocked_total{method} 上。
		ks.UnaryServerInterceptor(),

		// ③ 会话与方法准入(internal/session):客户端来源的身份只认 gate 注入的会话,
		//    内部方法(UpdateGuildScore)对客户端一律 PermissionDenied。放在热关停之后:
		//    被关停的方法不必解码会话;放在 serverbase 之前:被拒绝的调用没进业务链路,
		//    不该计入 rpc_duration_seconds(grpcstats 仍统计到它们)。
		session.UnaryServerInterceptor(session.ClientMethods),

		// ④ in-band 故障拦截器:本服务的 handler 一律 `return resp, nil`,把失败塞进
		//    响应体的 TipInfoMessage —— gRPC status 恒 OK,go-zero 自带的指标拦截器会把
		//    每一次「发号器被 fence」都记成一次成功请求。这层把响应体里的码读出来定性,
		//    故障打日志 + 计数,业务拒绝只计数。它不改响应内容、不吞错,对客户端无感。
		//    段归属、已分配范围、哪个码算故障(发号器被 fence)全部由 Tip.xlsx 生成的
		//    全局段表 + 故障表统一判定;TipClassifier 只是本服务固定下来的定性接缝。
		serverbase.UnaryInterceptor(serverbase.Options{
			TipClassifier: constants.TipClassifier(),
		}),

		// ⑤ 整请求业务预算(Timeout − 500ms):让归属区查询、发号、Redis / MySQL 这些 I/O
		//    在 zrpc 服务端超时之前失败,客户端拿到的是 in-band tip 而不是 DeadlineExceeded。
		//    放最内层:前四层都是本地判断,不该占业务预算。
		requestBudgetInterceptor(budget),
	}
	return append(chain, identity...)
}

// requestBudgetInterceptor 给 handler 的 ctx 套上整请求业务预算(与 go/trade 的 RequestBudget 同义)。
func requestBudgetInterceptor(budget time.Duration) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		return handler(ctx, req)
	}
}

// schemaRunner 是 schemamigrate.Up / schemamigrate.Plan 的函数形状;抽出来让启动期建表策略可单测。
type schemaRunner func(ctx context.Context, db *sql.DB, opts schemamigrate.Options) (schemamigrate.Report, error)

// schemaOptions 是启动路径与 -migrate 共用的迁移选项:库名断言用代码常量(Validate 已保证配置与之相等),
// 表清单只来自 data.Tables();锁等待 / 语句超时取 schemamigrate 默认值。
func schemaOptions() schemamigrate.Options {
	return schemamigrate.Options{
		Database: data.DatabaseName,
		Tables:   data.Tables(),
		Logf:     logx.Infof,
	}
}

// runWithLockRetry 在 GET_LOCK 被别的实例占住时重试。本地 go_services 允许 guild 多开,
// 首次建表时两个实例会互等;没有编排器替我们重拉,所以在进程内重试。
// -migrate 入口刻意不用它:退出码 3 交给 K8s Job 的 backoff。
func runWithLockRetry(ctx context.Context, runner schemaRunner, db *sql.DB, opts schemamigrate.Options) (schemamigrate.Report, error) {
	var report schemamigrate.Report
	var err error
	for attempt := 1; attempt <= lockBusyAttempts; attempt++ {
		report, err = runner(ctx, db, opts)
		if !errors.Is(err, schemamigrate.ErrLockBusy) || attempt == lockBusyAttempts {
			return report, err
		}
		logx.Infof("[guild] Migration lock busy; retrying after %v (%d/%d)", lockBusyRetryDelay, attempt, lockBusyAttempts)
		select {
		case <-time.After(lockBusyRetryDelay):
		case <-ctx.Done():
			return report, ctx.Err()
		}
	}
	return report, err
}

// ensureSchema 是启动期建表策略(port-decisions D-14 第 4 条):
//   - 常驻入口拒绝 AutoMigrate=true；表结构必须先由独立迁移命令准备。
//   - false:只跑只读 Plan;有待执行语句或需人工项 → 返回错误,并带上补救命令。
//
// 两种模式都额外把"缺普通索引"从 schemamigrate 的 Warning 升级成阻断:它不会自动补建,
// 放过去等于带着缺索引的表对外服务(设计 §6.4)。
func ensureSchema(ctx context.Context, db *sql.DB, c config.Config, up, plan schemaRunner) error {
	opts := schemaOptions()
	if c.ShouldAutoMigrate() {
		return errors.New("常驻服务禁止自动DDL，请关闭AutoMigrate并显式运行 -migrate")
	}

	report, err := runWithLockRetry(ctx, plan, db, opts)
	logReport("schemamigrate.Plan", report)
	if err != nil {
		return fmt.Errorf("[guild] Schema.AutoMigrate=false,启动期检查表结构失败(库 %s),拒绝启动;确认库可达后执行 %s: %w",
			data.DatabaseName, migrateRemedyCommand, err)
	}
	if !report.Clean() {
		return fmt.Errorf("[guild] Schema.AutoMigrate=false 且库 %s 与 proto/guild/guild_db.proto 不一致"+
			"(待执行 %d 条、需人工 %d 项),拒绝启动;先执行 %s",
			data.DatabaseName, len(report.Statements), len(report.Manual), migrateRemedyCommand)
	}
	return missingIndexError(report)
}

// missingIndexWarnings 挑出"缺索引"告警。前缀取自 go/schemamigrate/plan.go 的文案;
// guild_test.go 的真库用例负责守住两边一致(文案漂移时会红)。
func missingIndexWarnings(r schemamigrate.Report) []string {
	var missing []string
	for _, w := range r.Warnings {
		if strings.HasPrefix(w, "缺索引") {
			missing = append(missing, w)
		}
	}
	return missing
}

func missingIndexError(r schemamigrate.Report) error {
	missing := missingIndexWarnings(r)
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("[guild] 库 %s 缺 %d 个 proto 声明的普通索引,拒绝启动"+
		"(schemamigrate 不自动补建;开发期按 docs/design/guild-phase2/01-storage.md §6.4 删库重建): %s",
		data.DatabaseName, len(missing), strings.Join(missing, "; "))
}

// runMigration 是 -migrate 的实现:只连 MySQL,不起 gRPC、不连 etcd / data_service。
// 输出走 stdout / stderr,退出码按 schemamigrate.ExitCode(D-14),部署脚本与 K8s Job 直接读。
// 只打印库名,不打印 DSN(含口令)。
func runMigration(c config.Config) int {
	fmt.Printf("guild schema migration: database=%s\n", data.DatabaseName)
	pingCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	db, err := svc.OpenSchemaMySQL(pingCtx, c.MySQL)
	cancel()
	if err != nil {
		code := schemamigrate.ExitCode(schemamigrate.Report{}, err)
		fmt.Fprintf(os.Stderr, "schema migration FAILED (exit %d): %v\n", code, err)
		return code
	}
	defer func() { _ = db.Close() }()

	report, err := schemamigrate.Up(context.Background(), db, schemaOptions())
	printReport(os.Stdout, report)
	code := schemamigrate.ExitCode(report, err)
	if code == 0 {
		if missErr := missingIndexError(report); missErr != nil {
			fmt.Fprintf(os.Stderr, "schema migration needs manual action (exit %d): %v\n", schemamigrate.ExitManual, missErr)
			return schemamigrate.ExitManual
		}
	}
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "schema migration FAILED (exit %d): %v\n", code, err)
	case code != 0:
		fmt.Fprintf(os.Stderr, "schema migration needs manual action (exit %d): %d item(s) listed above\n", code, len(report.Manual))
	default:
		fmt.Printf("schema migration OK: %s synced from proto/guild/guild_db.proto (%d statement(s) executed)\n",
			data.DatabaseName, len(report.Statements))
	}
	return code
}

func ensureStartupSchema(c config.Config) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	db, err := svc.OpenReadOnlyMySQL(ctx, c.MySQL)
	cancel()
	if err != nil {
		return err
	}
	defer db.Close()
	return ensureSchema(context.Background(), db, c, schemamigrate.Up, schemamigrate.Plan)
}

// reportLines 把迁移报告展开成可读行(Statements / Warnings / Manual 各一段)。
func reportLines(r schemamigrate.Report) []string {
	var lines []string
	for _, s := range r.Statements {
		lines = append(lines, "  statement: "+s)
	}
	for _, w := range r.Warnings {
		lines = append(lines, "  warning:   "+w)
	}
	for _, m := range r.Manual {
		lines = append(lines, "  MANUAL:    "+m)
	}
	return lines
}

func printReport(w io.Writer, r schemamigrate.Report) {
	for _, line := range reportLines(r) {
		fmt.Fprintln(w, line)
	}
}

func logReport(stage string, r schemamigrate.Report) {
	for _, line := range reportLines(r) {
		logx.Infof("[guild] %s%s", stage, line)
	}
}

// isUnspecifiedHost:""、0.0.0.0、:: 这类"监听所有网卡"的 host,不能原样写进 NodeInfo。
func isUnspecifiedHost(host string) bool {
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

// advertisedHost 决定写进 NodeInfo 的 IP(与 go/trade、go/chat 的 advertisedHost 同一口径):
// POD_IP 优先(K8s 清单经 downward API 注入)→ ListenOn 的具体 host → 本机第一个非 loopback IP → 127.0.0.1。
func advertisedHost(listenHost string) string {
	if podIP := os.Getenv("POD_IP"); podIP != "" {
		return podIP
	}
	if !isUnspecifiedHost(listenHost) {
		return listenHost
	}
	if ip := netx.InternalIp(); ip != "" {
		return ip
	}
	return "127.0.0.1"
}

func splitHostPort(address string) (string, uint32, error) {
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return "", 0, err
	}
	portInt, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, err
	}
	return host, uint32(portInt), nil
}

// POD_IP来自编排器的显式注入；通配监听地址不能作为场景和路由服的拨号目标。
func nodeAdvertiseHost(listenHost string) (string, error) {
	if value, present := os.LookupEnv("POD_IP"); present {
		ip := net.ParseIP(strings.TrimSpace(value))
		if ip == nil || ip.IsUnspecified() {
			return "", errors.New("POD_IP必须是可拨号的非通配IP地址")
		}
		return ip.String(), nil
	}
	if listenHost == "" {
		return "", errors.New("通配监听必须显式注入POD_IP作为帮会注册地址")
	}
	if ip := net.ParseIP(listenHost); ip != nil && ip.IsUnspecified() {
		return "", errors.New("通配监听必须显式注入POD_IP作为帮会注册地址")
	}
	return listenHost, nil
}
