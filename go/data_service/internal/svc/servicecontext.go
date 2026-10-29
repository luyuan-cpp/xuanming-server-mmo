package svc

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"data_service/internal/config"
	"data_service/internal/guildcheck"
	"data_service/internal/metrics"
	"data_service/internal/routereconcile"
	"data_service/internal/routing"
	"data_service/internal/store"

	guildpb "proto/guild"
	loginpb "proto/login"
	"shared/accountdirectory"
	"shared/authoritysql"
	"shared/drainproof"
	"shared/placement"
	"shared/serviceauth"
	"shared/sqlpoolstats"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// SnapshotStore 描述 logic 层实际依赖的快照/审计能力。使用接口既让启动失败时
// 可以明确保持 nil，也允许单测注入失败实现验证 fail-closed 行为。
type SnapshotStore interface {
	Close() error
	InsertSnapshot(context.Context, *store.SnapshotRow) (uint64, error)
	// InsertSnapshotIfGuidAbsent 是 player_snapshot_topic 消费者的落库入口(source=1,按 guid 去重)。
	InsertSnapshotIfGuidAbsent(context.Context, *store.SnapshotRow) (uint64, bool, error)
	// InsertSnapshotsIfGuidAbsent 是 player_snapshot_topic 消费者的批量入口(G2a;返回新插入行数,批内与库内重复都不计)。
	// data_service.go 把本接口的值直接交给 kafka.Start 的 SnapshotSink,接口少了它就编译不过。
	InsertSnapshotsIfGuidAbsent(context.Context, []*store.SnapshotRow) (int64, error)
	GetSnapshotByID(context.Context, uint64) (*store.SnapshotRow, error)
	GetLatestSnapshotBefore(context.Context, uint64, uint64) (*store.SnapshotRow, error)
	ListSnapshotsMeta(context.Context, uint64, uint64, uint32) ([]*store.SnapshotMeta, error)
	GetSnapshotPlayerIDsByZone(context.Context, uint32, uint64) ([]uint64, error)
	// GetSnapshotPlayerTimesByZone 是 zone / 全服回档的计划:player_id → 该 zone 下最新快照的 created_at(秒)。
	// 帮会检查的 since 起点与执行期 R5 断言的下界都来自它(docs/design/guild-phase2/07-rollback-fail-closed.md §7.5.3-1)。
	GetSnapshotPlayerTimesByZone(context.Context, uint32, uint64) (map[uint64]uint64, error)
	InsertAuditLog(context.Context, *store.AuditLogRow) error
}

type TransactionLogStore interface {
	Close() error
	QueryLog(context.Context, *store.TransactionLogQuery) ([]*store.TransactionLogRow, uint32, error)
	// InsertBatchIgnore 是 transaction_log_topic 消费者的落库入口(tx_id 主键幂等)。
	InsertBatchIgnore(context.Context, []*store.TransactionLogRow) (int64, error)
}

// IdSegmentStore 是 AllocateIdSegment 的号段发号源(设计 §6.2)。
type IdSegmentStore interface {
	Close() error
	Allocate(ctx context.Context, bizTag string, step uint32) (lo, hi uint64, err error)
}

// PlayerNameStore 是玩家名字注册表的持久层(docs/design/guild-phase2/03-names.md §3.6a)。
//
// 之所以在这里再声明一次接口(而不是直接用 *store.PlayerNameStore):logic 的单测要能
// 注入一个假 store 验证"非法名字不碰库""缓存写失败仍返回成功"这类编排行为,而那些行为
// 与真 MySQL 无关。*store.PlayerNameStore 逐字满足本接口,装配处不需要任何适配。
//
// 【契约要点,换实现时必须一并满足】
//   - Reserve 的 name 是展示名、norm 是判重键,两者必须来自同一次 playername.Normalize;
//     返回 ReserveInserted / ReserveAlreadyOwned 时数据**已提交**,调用方可以安全写缓存。
//   - Release 只吃 norm;minCreatedMs=0 表示不限登记时间(仅限已过 admin 鉴权的路径)。
//   - BatchGet 返回**展示名**,缺席的 id 不出现在 map 里(不是错误),超过
//     store.PlayerNameBatchLimit 个 id 必须报错而不是截断。
type PlayerNameStore interface {
	Close() error
	Reserve(ctx context.Context, playerID uint64, name, norm string, nowMs uint64) (store.ReserveOutcome, uint64, error)
	Release(ctx context.Context, playerID uint64, norm string, minCreatedMs uint64) (store.ReleaseOutcome, error)
	BatchGet(ctx context.Context, playerIDs []uint64) (map[uint64]string, error)
}

// PlayerRouteStore 是玩家归属 / 落点映射的持久层(docs/design/player-route-mapping-durability-proposal.md,第 1a 批)。
//
// RegisterHome 那一半由消费方 routing 定义(routing.RouteStore:路由器只需要它),这里再加 Close 让
// ServiceContext 能收尾;*store.RouteStore 逐字满足,装配处不需要任何适配。用接口而不是具体类型,
// 理由同上面几个 store:单测可以注入假实现验证 db_first 的编排与 fail-closed 行为。
//
// 只在 RouteStore.Mode=db_first 时装配;redis 模式下恒为 nil,也没有任何代码读它。
//
// 第 3 批起再嵌 routing.RouteReader(回源读:R2 / R3 / R6 在缓存不可信时读库,R7 在 db_first 下一律读库):
// 同一个 *store.RouteStore 同时作为写入点与回源读者注入 Router(routeRouterOptions)。
type PlayerRouteStore interface {
	routing.RouteStore
	routing.RouteReader
	Close() error
}

// ErrRollbackTargetOnline 由 RollbackFence 实现在目标仍在线、正在登录，或旧
// epoch 写入尚未排空时返回。logic 层据此向调用方返回 ErrCodePlayerOnline。
var ErrRollbackTargetOnline = errors.New("rollback target is online or has active writes")

// RollbackFence 是应用级回档执行前必须持有的跨服务离线 epoch。
// Acquire* 成功后，直到 release 被调用，登录/Scene 激活和任何旧 epoch 存盘都
// 必须被阻断。一次普通的“当前是否在线”查询不满足这个接口（存在 TOCTOU）。
// 当前仓库尚无跨 login/player_locator/Scene/db 的实现，因此生产 ServiceContext
// 故意保持 nil，所有回档写 RPC fail-closed；测试可注入 fake 验证其余逻辑。
type RollbackFence interface {
	AcquirePlayer(context.Context, uint64) (release func(), err error)
	AcquireZone(context.Context, uint32) (release func(), err error)
	AcquireServer(context.Context, []uint32) (release func(), err error)
}

type ServiceContext struct {
	// 与所有业务池共用的本boot；main必须先关闭接纳并join真实工作，再Seal。
	Writer                    authoritysql.WriterConfig
	RootAdmission             *drainproof.Session
	RelayAdmission            *drainproof.Session
	RelayIngress              *drainproof.Session
	DrainRuntime              *drainproof.Runtime
	lifecycleMu               sync.Mutex
	closeOnce                 sync.Once
	closeErr                  error
	closeCompleted            bool
	upstreamClosers           []interface{ Close() error }
	AccountDirectory          accountdirectory.Directory
	AccountDirectoryAuth      *serviceauth.Verifier
	SceneManagerDirectoryAuth *serviceauth.Verifier
	AccountRoleTrades         accountdirectory.RoleTrades
	AccountRoleTradeAuth      *serviceauth.Verifier
	RoleAdmissions            accountdirectory.Admissions
	RoleAdmissionAuth         *serviceauth.Verifier
	accountDirectoryCloser    interface{ Close() error }
	Config                    config.Config
	Router                    *routing.Router
	SnapshotStore             SnapshotStore
	TxLogStore                TransactionLogStore
	IdSegmentStore            IdSegmentStore
	PlayerNameStore           PlayerNameStore // nil = 名字注册表不可用,Reserve/Release/BatchGet 全部 fail-closed
	RollbackFence             RollbackFence
	LoginAdminClient          loginpb.LoginAdminClient // nil when not configured

	// PlayerRouteStore 是 db_first 下 RegisterPlayerZone 的库内提交点(已经注入进 Router,这里只为 Close 与
	// MissingStores 留一份引用)。nil = redis 模式,或 db_first 但没装配起来(此时 RegisterPlayerZone fail-closed)。
	PlayerRouteStore PlayerRouteStore

	// RouteReconciler 是 db_first 下的对账 leader(D1 第 1b-2 批,internal/routereconcile)。nil = redis 模式、
	// Reconcile.Enabled=false,或没装配起来(后者 MissingStores 带上 "route_reconciler",Store.Required=true 时 main 拒启)。
	// 构造时不开连接、不起 goroutine;由 main 启停(Start / stop,关停顺序见 data_service.go),Close 不管它。
	RouteReconciler *routereconcile.Reconciler

	// GuildDivergence 是回档闸问 guild 的接缝(docs/design/guild-phase2/07-rollback-fail-closed.md §7.5.1)。
	//
	// ⚠ **nil = 未配置 = 三个 Rollback* 一律拒绝**(ErrCodeRollbackGuildCheckFailed,零写入,放行无效)。
	// 这与上一行 LoginAdminClient 的先例(nil = 跳过孤儿清理)**方向相反**:照抄那个先例写成
	// "没配就不查",就是让回档在 guild 缺席时复制资产(fail-open)。测试 D2 钉住。
	GuildDivergence guildcheck.GuildDivergenceChecker

	// GuildCheckSleep 是回档闸两段等待(沉降 30s、写后复查前 10s,07 §7.7)的注入点。
	// nil = 真实计时器等待(可被 ctx 取消);单测注入,既不真睡,也能断言"检查发生在沉降之后"(AGENTS §11.4)。
	GuildCheckSleep func(ctx context.Context, d time.Duration) error
}

// autoMigrateTimeout 启动路径建表/补列的总时限。CreateOrUpdateTable 对存量大表的
// MODIFY COLUMN 会重建表,dev 库量小够用;生产不走这条路(Schema.AutoMigrate=false,
// 部署阶段跑 `data_service -migrate`,那条路不设时限)。
const autoMigrateTimeout = 5 * time.Minute

// routePublishProbeMargin 是 db_first 装配时 WAITAOF 能力探针在 WaitAOFTimeoutMs 之外多给的时间
// (连 mapping Redis、PING、回包)。
const routePublishProbeMargin = 3 * time.Second

// routeReadyProbeTimeout 是 ReadRequiresReady=true 时启动期试跑一次读脚本的时限(连 mapping Redis、EVAL 含两次 INFO、回包)。
const routeReadyProbeTimeout = 3 * time.Second

// MySQLConfigOf 把 yaml 的 SnapshotMySQL 段转成 store 的连接参数;main 的 -migrate 入口也用它。
func MySQLConfigOf(c config.Config) store.MySQLConfig {
	return store.MySQLConfig{
		Host:        c.SnapshotMySQL.Host,
		User:        c.SnapshotMySQL.User,
		Password:    c.SnapshotMySQL.Password,
		DBName:      c.SnapshotMySQL.DBName,
		MaxOpenConn: c.SnapshotMySQL.MaxOpenConn,
		MaxIdleConn: c.SnapshotMySQL.MaxIdleConn,
		RequireTiDB: c.SnapshotMySQL.RequireTiDB,
	}
}

// MissingStores 返回没装配起来的 MySQL store 的名字(固定顺序);全部就绪时返回 nil。
// main 在 Store.Required=true 时据此拒绝启动(见 config.StoreConfig)。
// player_route 只在 RouteStore.Mode=db_first 时才是必需的:redis 模式根本不装配它,不能算缺。
// route_reconciler(对账 leader)只在 RouteStore.ReconcileEnabled()(db_first 且 Reconcile.Enabled 不是 false)时才是必需的
// (核实意见 should「构造失败只打 ERROR」:所有副本因同一原因装不起来时,既不补发孤儿行、也不做回档检测,指标上也看不出来);
// player_route 缺席时它必然也缺,两项各自上报。
func (s *ServiceContext) MissingStores() []string {
	var missing []string
	if s.AccountDirectory == nil || s.AccountDirectoryAuth == nil {
		missing = append(missing, "account_directory")
	}
	if s.SnapshotStore == nil {
		missing = append(missing, "snapshot")
	}
	if s.TxLogStore == nil {
		missing = append(missing, "transaction_log")
	}
	if s.IdSegmentStore == nil {
		missing = append(missing, "id_segment")
	}
	if s.PlayerNameStore == nil {
		missing = append(missing, "player_name")
	}
	if s.Config.RouteStore.DBFirst() && s.PlayerRouteStore == nil {
		missing = append(missing, "player_route")
	}
	if s.Config.RouteStore.ReconcileEnabled() && s.RouteReconciler == nil {
		missing = append(missing, "route_reconciler")
	}
	return missing
}

// RouteReadGateUnassembled 返回非 nil = 读方门禁开着(RouteStore.ReadRequiresReady=true)而 route store 没装配起来
// (启动探针跑不起来、迁移失败、WAITAOF 探针不过、开库失败、空库引导拒绝,原因见此前的 [ServiceContext] / [route-bootstrap] ERROR)。
// main 据此**无条件**拒启,不看 Store.Required(M1-5,2026-10-22;规格 §13.2 第 2 条「读方开关为 true 时启动探针跑不起来 → 拒启」,
// 与 go/db、guild 的读方开关同口径)。理由:没有回源读者,缓存不可信时的每一次归属读都回 Unavailable(码 30)、每一次 W1 都
// fail-closed,本副本也装不起对账 leader —— 这样的实例进了发现池只会把登录分流到必败的副本上;Store.Required=false 的「缺 store
// 降级启动」是给回档 / 快照这类旁路能力的,不适用于读路径。全球配置(ResolveGlobalAuthority)恒要求 Store.Required=true,
// 那时 MissingStores 已先拒启,本判据只在 LegacyNonGlobal 的 dev / test 夹具上多拦一道;ReadRequiresReady=false(组件缺省)时恒回 nil。
func (s *ServiceContext) RouteReadGateUnassembled() error {
	if !s.Config.RouteStore.ReadRequiresReady || s.PlayerRouteStore != nil {
		return nil
	}
	return errors.New("RouteStore.ReadRequiresReady=true but the route store (the player_route fallback reader) is not assembled: " +
		"every untrusted home zone read would answer Unavailable and every RegisterPlayerZone would fail closed")
}

func NewServiceContext(c config.Config) *ServiceContext {
	return NewServiceContextWithRouteDangerOwner(c, nil)
}

// 危险回调由进程真实root/relay owner注入；不得在reconcile worker内等待自身退出。
func NewServiceContextWithRouteDangerOwner(c config.Config, onDanger func(context.Context, *store.RouteEmergencyError) error) *ServiceContext {
	return NewServiceContextWithAdmission(c, drainproof.New(), drainproof.New(), onDanger)
}

// 门禁必须在构造前确定；prepared SOURCE 与正常服务复用同一 SQL boot，不能运行中换 gate。
func NewServiceContextWithAdmission(c config.Config, root, relay *drainproof.Session, onDanger func(context.Context, *store.RouteEmergencyError) error) *ServiceContext {
	if root == nil || relay == nil || root == relay {
		panic("data_service 缺独立 SOURCE/RELAY 门禁")
	}
	resolved, err := c.ResolveGlobalAuthority(os.Getenv)
	if err != nil {
		panic(fmt.Sprintf("全球SQL装配拒绝: %v", err))
	}
	c = resolved
	if !c.LegacyNonGlobal {
		if err := validateGlobalCallerSecrets(os.Getenv); err != nil {
			panic(err.Error())
		}
	}
	// PlayerName 段的零值**全是危险值**(CacheTTL=0 会让每次缓存回填都失败,
	// ReleaseWindow=0 会让建角失败后的条件释放什么都删不掉),而 go-zero 不下钻一个
	// 未出现的 optional 结构、填不了 default 标签。所以默认值在这里一次性补齐:
	// ServiceContext.Config 是 logic 层唯一的配置来源,规范化放在这里,读取点就不必
	// 各自兜底(那种兜底迟早有人漏掉,而漏掉的症状是缓存静默失效,没人会注意到)。
	c.PlayerName = c.PlayerName.Normalize()
	// RouteStore 段同理(零值 = 没配);合法性 main 启动时已校验,routing.NewRouter 还会再校验一次(非法即 panic)。
	c.RouteStore = c.RouteStore.Normalize()
	if c.RouteStore.ReconcileEnabled() && onDanger == nil {
		panic("data_service对账缺真实危险关闭owner，拒绝启动")
	}

	mysqlCfg := MySQLConfigOf(c)
	var writerConfig authoritysql.WriterConfig
	// 常驻进程没有 schema 凭据。历史上缺省启动迁移的行为在这里收口，
	// 显式要求自动迁移则拒启；未配置时仅使用部署阶段已准备的表。
	if c.Schema.AutoMigrate != nil && *c.Schema.AutoMigrate {
		panic("常驻 data_service 禁止 Schema.AutoMigrate，请使用独立 -migrate 部署入口")
	}
	if mysqlCfg.Host != "" {
		writer, err := authoritysql.LoadWriterConfig(os.Getenv)
		if err != nil {
			panic(fmt.Sprintf("data_service 写者启动授权拒绝: %v", err))
		}
		writerConfig = writer
		mysqlCfg.Permission = authoritysql.Writer(writerConfig)
	}
	schemaReady := true
	logx.Info("[ServiceContext] 常驻启动只读核对表形状；schema 与号段初值必须由独立 -migrate 入口预建")

	var (
		snapshotStore       SnapshotStore
		transactionLogStore TransactionLogStore
		idSegmentStore      IdSegmentStore
		playerNameStore     PlayerNameStore
	)
	if schemaReady {
		if ss, err := store.NewSnapshotStore(mysqlCfg); err != nil {
			logx.Errorf("[ServiceContext] snapshot store init failed (rollback disabled): %v", err)
		} else if ss != nil {
			snapshotStore = ss
		}

		if txLog, err := store.NewTransactionLogStore(mysqlCfg); err != nil {
			logx.Errorf("[ServiceContext] transaction log store init failed (recall/query disabled): %v", err)
		} else if txLog != nil {
			transactionLogStore = txLog
		}

		if seg, err := store.NewIdSegmentStore(mysqlCfg, store.IdSegmentOptions{AllowAutoSeed: c.IdSegment.AllowAutoSeed}); err != nil {
			logx.Errorf("[ServiceContext] id segment store init failed (AllocateIdSegment disabled): %v", err)
		} else if seg != nil {
			idSegmentStore = seg
		}

		// 名字注册表用**独立连接池**:建角路径上每次都有一条同步 INSERT,与快照/号段
		// 共用 SnapshotMySQL 那几条连接时,一次 GM 批量回滚就能把全服建角卡住。
		// 只覆盖池大小,库/账号仍是同一个全局库 —— player_name 与 id_segment 同库。
		//
		// 配置非法时**不装配**(nil),而不是纠正成默认值:那三个时间窗里有一个是安全
		// 边界(ReleaseWindow),被悄悄放宽了谁都不知道;nil 的症状是建角当场全线拒绝,
		// 启动日志里写清楚是哪个键 —— 这正是 fail-closed 该有的样子。
		if err := c.PlayerName.Validate(); err != nil {
			logx.Errorf("[ServiceContext] player name store init failed (CreatePlayer disabled): invalid PlayerName config: %v", err)
		} else {
			nameCfg := mysqlCfg
			nameCfg.MaxOpenConn = c.PlayerName.MaxOpenConn
			nameCfg.MaxIdleConn = c.PlayerName.MaxIdleConn
			if names, err := store.NewPlayerNameStore(nameCfg); err != nil {
				logx.Errorf("[ServiceContext] player name store init failed (CreatePlayer disabled): %v", err)
			} else if names != nil {
				playerNameStore = names
				logx.Infof("[ServiceContext] player name registry ready: release_window=%v cache_ttl=%v negative_cache_ttl=%v",
					c.PlayerName.ReleaseWindow, c.PlayerName.CacheTTL, c.PlayerName.NegativeCacheTTL)
			}
		}
	}
	var directory *store.AccountDirectoryStore
	var directoryAuth *serviceauth.Verifier
	newDirectory := store.NewAccountDirectoryStore
	if c.LegacyNonGlobal {
		newDirectory = store.NewLegacyAccountDirectoryStore
	}
	if schemaReady {
		// 池上限非法时不装配(不纠正成默认值,理由见 config.AccountDirectoryConfig.MaxOpenConn):MissingStores 带上 account_directory,
		// 全球服务恒为 Store.Required=true,main 拒启。
		if auth, err := serviceauth.New(map[string]string{"login": os.Getenv(accountdirectory.SecretEnv)}); err != nil {
			logx.Errorf("[account-directory] internal caller key is unavailable; account directory RPCs disabled")
		} else if maxOpen, err := c.AccountDirectory.PoolMaxOpenConn(); err != nil {
			logx.Errorf("[account-directory] invalid AccountDirectory config; account directory RPCs disabled: %v", err)
		} else if ds, err := newDirectory(mysqlCfg, mysqlCfg.RequireTiDB || c.AccountDirectory.RoleTradeSQLProof.Enabled || (c.Mode != "dev" && c.Mode != "test"), c.AccountDirectory.MaxRoles(), maxOpen); err != nil {
			logx.Errorf("[account-directory] SQL store init failed: %v", err)
		} else {
			if !c.LegacyNonGlobal {
				if !c.RouteStore.DBFirst() {
					err = errors.New("正式建角初始提交要求 SQL 权威路由")
				} else {
					err = ds.ConfigureInitialPlayerCommit(c.AccountDirectory.RoleTradeSQLProof.AllowedStorageIDs)
				}
			}
			if err != nil {
				_ = ds.Close()
				logx.Errorf("[account-directory] initial SQL commit unavailable: %v", err)
			} else {
				directory = ds
				directoryAuth = auth
			}
		}
	}

	// 映射提交点的库(docs/design/player-route-mapping-durability-proposal.md §4,第 1a 批):只在 db_first 时装配。
	// 失败只记 ERROR、保持 nil:RegisterPlayerZone 届时 fail-closed(ErrRouteStore),Store.Required=true 时
	// MissingStores 带上 "player_route",main 拒启;ReadRequiresReady=true 时不论 Store.Required 都拒启(RouteReadGateUnassembled,M1-5)。
	//
	// 同一个 *store.RouteStore 交给两个消费方:PlayerRouteStore(Router 的写入点与回源读者)与对账 leader(routereconcile.Store)。
	// 两处都在判空之后才赋给接口(规格 d 节「nil 接口陷阱」:把 nil 的 *store.RouteStore 直接转成接口,就是「非 nil 接口包着 nil 指针」,
	// 判空拦不住、调用即 panic)。
	routeStore := newPlayerRouteStore(c, mysqlCfg, schemaReady)
	var (
		playerRouteStore PlayerRouteStore
		reconcileStore   routereconcile.Store
	)
	if routeStore != nil {
		playerRouteStore = routeStore
		reconcileStore = routeStore
	}

	// 对账 leader(D1 第 1b-2 批):只在 ReconcileEnabled(db_first 且 Reconcile.Enabled 不是 false)时构造;构造不开连接。
	// 失败只记 ERROR、保持 nil;Store.Required=true 时 MissingStores 带上 "route_reconciler",main 拒启(核实意见 should)。
	routeReconciler, err := newRouteReconciler(c, reconcileStore, productionRouteReconcilerDeps(onDanger))
	if err != nil {
		panic(fmt.Sprintf("data_service对账owner装配失败，拒绝启动: %v", err))
	}

	// Optional: login admin gRPC client for orphan account cleanup during rollback
	var loginClient loginpb.LoginAdminClient
	var upstreamClosers []interface{ Close() error }
	if c.LoginAdminRpc.Etcd.Key != "" {
		conn := zrpc.MustNewClient(c.LoginAdminRpc)
		loginClient = loginpb.NewLoginAdminClient(conn.Conn())
		upstreamClosers = append(upstreamClosers, conn.Conn())
		logx.Info("[ServiceContext] login admin RPC client connected")
	}

	// 只有 store 非 nil 时才注入(规格 d 节「nil 接口陷阱」):playerRouteStore 只在 routeStore 非 nil 时才被赋值(见上),
	// 这里的判空才可靠。第 3 批起同一个实例也作为回源读者注入(routeRouterOptions)。
	routerOpts := routeRouterOptions(playerRouteStore)

	// 读路径指标的启动预置(理由同 metrics.PrimeRoute:sql_error 这类可能一辈子只发生几次的结局,序列不预先存在的话
	// `increase(...) > 0` 恰好漏掉第一次):
	//   - R7(GetAllPlayerIDsInZone)在 db_first 下一律读库、与读方开关无关,每次都计 route_read_total{path="enumerate"},
	//     所以它的两条序列只要 db_first 就预置(M1-6,2026-10-23);
	//   - 其余 11 条 path × result 与 verdict 只在读方开关打开时才会出现,只在那时预置(开关只许 db_first,Validate 已钉住)。
	if c.RouteStore.DBFirst() {
		metrics.PrimeRouteEnumerate()
		if c.RouteStore.ReadRequiresReady {
			metrics.PrimeRouteRead()
		}
	}

	divergence := newGuildDivergenceChecker(c, func(closer interface{ Close() error }) { upstreamClosers = append(upstreamClosers, closer) })
	s := &ServiceContext{
		upstreamClosers:      upstreamClosers,
		Writer:               writerConfig,
		RootAdmission:        root,
		RelayAdmission:       relay,
		RelayIngress:         drainproof.New(),
		Config:               c,
		Router:               routing.NewRouter(c, routerOpts...),
		SnapshotStore:        snapshotStore,
		AccountDirectoryAuth: directoryAuth,
		TxLogStore:           transactionLogStore,
		IdSegmentStore:       idSegmentStore,
		PlayerNameStore:      playerNameStore,
		PlayerRouteStore:     playerRouteStore,
		RouteReconciler:      routeReconciler,
		LoginAdminClient:     loginClient,
		GuildDivergence:      divergence,
	}
	if directory != nil {
		s.AccountDirectory = directory
		s.accountDirectoryCloser = directory
		var verifier store.RoleFreezeVerifier
		if c.AccountDirectory.RoleTradeSQLProof.Enabled && c.RouteStore.DBFirst() {
			if v, err := store.NewSQLRoleFreezeVerifier(c.AccountDirectory.RoleTradeSQLProof.AllowedStorageIDs); err != nil {
				logx.Errorf("[account-role-trade] SQL proof storage allowlist invalid; Confirm disabled")
			} else {
				verifier = v
			}
		} else {
			logx.Info("[account-role-trade] explicit TiDB SQL proof/db_first configuration absent; Confirm disabled")
		}
		trades := store.NewAccountRoleTradeStore(directory, verifier)
		if err := trades.ConfigureGuildExit(c.AccountDirectory.MustLeaveGuild(), c.AccountDirectory.RoleTradeGuildSchema); err != nil {
			logx.Error("[account-role-trade] guild authority schema absent; new role freezing disabled")
		}
		if err := trades.ConfigureAssetContracts(c.AccountDirectory.RoleTradeTradeSchema, c.AccountDirectory.RoleTradeGuildSchema, c.AccountDirectory.RoleTradeMailSchema, c.AccountDirectory.RoleTradeMatchSchema, c.AccountDirectory.RoleTradeInboxSchema); err != nil {
			logx.Error("[account-role-trade] 跨域挂起权威schema或表列不可用；冻结、删除及维护导入保持拒绝")
		}
		s.AccountRoleTrades = trades
		s.RoleAdmissions = store.NewAccountRoleAdmissionStore(directory)
		if auth, err := serviceauth.New(map[string]string{"scene_manager": os.Getenv("MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE_MANAGER")}); err != nil {
			logx.Error("[account-directory] scene_manager独立只读密钥不可用；仅其Lookup关闭")
		} else {
			s.SceneManagerDirectoryAuth = auth
		}
		if auth, err := serviceauth.New(map[string]string{"scene": os.Getenv(accountdirectory.SceneSecretEnv)}); err != nil {
			logx.Errorf("[role-admission] internal scene key unavailable; admission/drain RPCs disabled")
		} else {
			s.RoleAdmissionAuth = auth
		}
		if auth, err := serviceauth.New(map[string]string{"trade": os.Getenv(accountdirectory.TradeSecretEnv)}); err != nil {
			logx.Errorf("[account-role-trade] internal trade key unavailable; role trade RPCs disabled")
		} else {
			s.AccountRoleTradeAuth = auth
		}
	}
	return s
}

// routeReconcilerDeps 是 newRouteReconciler 的外部依赖(单测接缝):mapping Redis 客户端的构造、本机名、预热令牌与 leader 标识的随机源。
// 生产值见 productionRouteReconcilerDeps;单测注入假实现,不连 Redis、不碰环境。
type routeReconcilerDeps struct {
	onDanger func(context.Context, *store.RouteEmergencyError) error
	newCache func(mapping redis.RedisConf, rc config.RouteStoreConfig) (routereconcile.Cache, error)
	hostname func() (string, error)
	random   io.Reader
}

// productionRouteReconcilerDeps:独立 go-redis 客户端(routereconcile.NewRedisCache,不经 go-zero 熔断器)、os.Hostname、crypto/rand。
func productionRouteReconcilerDeps(onDanger func(context.Context, *store.RouteEmergencyError) error) routeReconcilerDeps {
	return routeReconcilerDeps{
		onDanger: onDanger,
		newCache: func(mapping redis.RedisConf, rc config.RouteStoreConfig) (routereconcile.Cache, error) {
			// 先判错再转接口:NewRedisCache 失败时回的是 nil 的 *RedisCache,直接 return 会包出非 nil 接口。
			cache, err := routereconcile.NewRedisCache(mapping, rc)
			if err != nil {
				return nil, err
			}
			return cache, nil
		},
		hostname: os.Hostname,
		random:   rand.Reader,
	}
}

// newRouteReconciler 装配对账 leader(纯装配,不开连接、不起 goroutine;svc 单测的接缝,核实意见 should)。
//   - 不要求 leader(redis 模式、Reconcile.Enabled=false)→ (nil, nil):不是错误,MissingStores 也不算它缺;
//   - 要求 leader 而 route store 没装配起来(rs 为 nil)、mapping Redis 配置不合用(Type 不是 node 等)、leader 标识生成失败
//     → (nil, error):调用方只记 ERROR,MissingStores 带上 "route_reconciler"。
//
// c.RouteStore 必须是 Normalize 过的(NewServiceContext 里已经做了);rs 为 nil 时必须是字面量 nil 接口。
func newRouteReconciler(c config.Config, rs routereconcile.Store, deps routeReconcilerDeps) (*routereconcile.Reconciler, error) {
	if !c.RouteStore.ReconcileEnabled() {
		return nil, nil
	}
	if rs == nil {
		return nil, errors.New("RouteStore.Mode=db_first but the route store (player_route) is not assembled; see the route store error above")
	}
	if deps.onDanger == nil {
		return nil, errors.New("对账缺真实root/relay关闭owner")
	}
	cache, err := deps.newCache(c.MappingRedis, c.RouteStore)
	if err != nil {
		return nil, fmt.Errorf("mapping redis client for the reconcile leader: %w", err)
	}
	host, err := deps.hostname()
	if err != nil {
		// 取不到主机名不拒启:leader 标识里的 host 用 "unknown",随机后缀仍保证副本之间不同名。
		logx.Errorf("[ServiceContext] os.Hostname failed, the route reconcile leader id falls back to unknown: %v", err)
		host = ""
	}
	leaderID, err := store.NewRouteLeaderID(host, deps.random)
	if err != nil {
		_ = cache.Close()
		return nil, fmt.Errorf("route reconcile leader id: %w", err)
	}
	r, err := routereconcile.New(c.RouteStore, routereconcile.Deps{Store: rs, Cache: cache, LeaderID: leaderID, Random: deps.random, OnDanger: deps.onDanger})
	if err != nil {
		_ = cache.Close()
		return nil, err
	}
	logx.Infof("[ServiceContext] route reconcile leader assembled: leader_id=%s arm_ready_marker=%t (starts with the service)",
		leaderID, r.ArmEnabled())
	return r, nil
}

// newPlayerRouteStore 装配 db_first 的库内提交点。redis 模式、启动期迁移失败(schemaReady=false)、
// 或 mapping Redis 过不了 WAITAOF 能力探针(routing.ProbeWaitAOF)时返回 nil;第 3 批起,ReadRequiresReady=true 而
// 读脚本在 mapping Redis 上跑不起来(probeRouteReadyReadAtStartup)时同样返回 nil。
//
// 连接参数用全局库(MySQLConfigOf),池大小覆盖为 RouteStore 段的值 —— route store 的会话变量(RC + 锁等待 1s)
// 不能带给别的 store,所以它总是自己开池(store.NewRouteStore)。写者标识 "data_service@{hostname}" 写进
// player_route.updated_by,排障时能看出是哪个副本写的;hostname 取不到时 NewRouteWriterID 用 "unknown"。
// 返回具体类型(失败为 nil):同一个实例要交给 PlayerRouteStore 与对账 leader 两个接口,调用方判空之后再分别赋值
// (第 1b-2 批起;此前返回接口,判空口径不变)。
func newPlayerRouteStore(c config.Config, mysqlCfg store.MySQLConfig, schemaReady bool) *store.RouteStore {
	if !c.RouteStore.DBFirst() {
		return nil
	}
	if !schemaReady {
		logx.Errorf("[ServiceContext] RouteStore.Mode=db_first but the schema migration failed; route store not assembled (RegisterPlayerZone will fail closed)")
		return nil
	}
	if err := c.RouteStore.Validate(); err != nil {
		logx.Errorf("[ServiceContext] route store init failed (RegisterPlayerZone will fail closed): invalid RouteStore config: %v", err)
		return nil
	}
	// WAITAOF 的能力探针放在开库之前:AOF 配错是确定性的,装配起来只会让每一次 W1 都失败、
	// 再经 go-zero 熔断器把同一 mapping Redis 上的登录读也拖下水(routing.ProbeWaitAOF 的注释)。
	probeCtx, cancel := context.WithTimeout(context.Background(), time.Duration(c.RouteStore.WaitAOFTimeoutMs)*time.Millisecond+routePublishProbeMargin)
	probeErr := routing.ProbeWaitAOF(probeCtx, c.MappingRedis, c.RouteStore)
	cancel()
	if probeErr != nil {
		logx.Errorf("[ServiceContext] route store not assembled (RegisterPlayerZone will fail closed): mapping redis cannot confirm AOF durability "+
			"as RouteStore.WaitAOF* requires: %v", probeErr)
		return nil
	}
	if !probeRouteReadyReadAtStartup(c) {
		return nil
	}
	routeCfg := mysqlCfg
	routeCfg.MaxOpenConn = c.RouteStore.MaxOpenConn
	routeCfg.MaxIdleConn = c.RouteStore.MaxIdleConn
	host, err := os.Hostname()
	if err != nil {
		logx.Errorf("[ServiceContext] os.Hostname failed, route writer id falls back to unknown: %v", err)
	}
	rs, err := store.NewRouteStore(routeCfg, store.RouteStoreOptions{
		FenceShards: uint32(c.RouteStore.FenceShards),
		Writer:      store.NewRouteWriterID("data_service", host),
	})
	if err != nil {
		logx.Errorf("[ServiceContext] route store init failed (RegisterPlayerZone will fail closed; %s): %v", store.OpenRouteStoreHint("<yaml>", err), err)
		return nil
	}
	if !c.LegacyNonGlobal {
		cache, err := redis.NewRedis(c.MappingRedis)
		if err == nil {
			err = checkEmptyRouteBootstrap(context.Background(), rs, cache, c.AccountDirectory.RoleTradeSQLProof.AllowedStorageIDs)
		}
		if err != nil {
			_ = rs.Close()
			logx.Errorf("[route-bootstrap] 全球路由启动拒绝: %v", err)
			return nil
		}
	}
	logx.Infof("[ServiceContext] route store ready (RouteStore.Mode=db_first): fence_shards=%d publish_attempts=%d waitaof_numlocal=%d waitaof_numreplicas=%d waitaof_timeout_ms=%d",
		c.RouteStore.FenceShards, c.RouteStore.PublishAttempts, c.RouteStore.NumLocal(), c.RouteStore.NumReplicas(), c.RouteStore.WaitAOFTimeoutMs)
	return rs
}

// probeRouteReadyReadAtStartup 是第 3 批读方门禁的启动探针(规格 §13.2 第 2 条):ReadRequiresReady=false 时什么都不做、
// 回 true;true 时在 mapping Redis 上试跑一次读脚本(不带映射键,routing.ProbeRouteReadyRead),跑不起来(INFO 在脚本里
// 不可用、ACL 拒绝、连不上)回 false —— 调用方不装配 route store,main 据 RouteReadGateUnassembled **无论 Store.Required
// 取值都拒启**(M1-5,2026-10-22;此前只经 MissingStores 在 Store.Required=true 时拒启)。
// 跑得起来就照常装配、回 true,只按结论分级记日志(A2-11,2026-10-22):
//   - not_master / identity_unavailable(routeReadyProbeNeverArms):确定性配置问题 —— 就绪标记对这个实例永远挂不上,
//     读方会永久回源库。记 ERROR,但**不**拒启、不拒装配:读结果仍然正确(只是每次读多一次库查询),哨兵切主期间启动的 Pod
//     也可能短暂碰到 not_master,拒启会把「正确但慢」变成故障(核实意见);
//   - 其余(absent、identity_mismatch、malformed、newer_version、trusted):启动时的正常状态(leader 预热完才挂),记 Info。
//
// 放在开库之前:与 WAITAOF 探针同理,配置错误挡在启动期,不让它变成每一次读都回源、每一次回源都多两次失败的 INFO。
func probeRouteReadyReadAtStartup(c config.Config) bool {
	if !c.RouteStore.ReadRequiresReady {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), routeReadyProbeTimeout)
	defer cancel()
	verdict, err := routing.ProbeRouteReadyRead(ctx, c.MappingRedis)
	if err != nil {
		logx.Errorf("[ServiceContext] route store not assembled (RouteStore.ReadRequiresReady=true but the route ready read script cannot run "+
			"on the mapping redis; data_service refuses to start regardless of Store.Required): %v", err)
		return false
	}
	if routeReadyProbeNeverArms(verdict) {
		logx.Errorf("[ServiceContext] route ready read probe: verdict=%s on mapping redis %s: it is not a master, or the script cannot read "+
			"run_id / role / master_replid from INFO, so the ready marker can never be trusted on this instance and every home zone read "+
			"falls back to player_route (correct, one global DB query per read). Check that MappingRedis points at the master "+
			"(or the haproxy master endpoint) and that ACL allows INFO inside scripts; a brief not_master during a sentinel failover is expected. "+
			"The route store is assembled as usual", verdict, c.MappingRedis.Host)
		return true
	}
	logx.Infof("[ServiceContext] route ready read probe ok: verdict=%s (anything but trusted is normal until the reconcile leader arms the marker; "+
		"readers fall back to player_route meanwhile)", verdict)
	return true
}

// routeReadyProbeNeverArms:启动探针的结论是 not_master 或 identity_unavailable —— 挂标记脚本同样要求本实例是主、身份取得到,
// 就绪标记对这个 MappingRedis 永远不会生效(A2-11)。routing.ProbeRouteReadyRead 不论标记在不在都会如实回报这两种
// (见 routing.probeRouteReadyVerdict),所以首次启动、标记还没挂时也能报出来。纯函数,单测直接测。
func routeReadyProbeNeverArms(v placement.RouteReadyVerdict) bool {
	return v == placement.RouteReadyNotMaster || v == placement.RouteReadyIdentityUnavailable
}

// routeRouterOptions 把装配好的 route store 同时作为写入点(WithRouteStore)与回源读者(WithRouteReader,第 3 批)交给 Router。
// rs 为 nil(redis 模式或没装配起来)时一个选项都不给。判空可靠的前提同 NewServiceContext 的说明:newPlayerRouteStore 的
// 失败分支返回字面量 nil,不会把一个 nil 的 *store.RouteStore 包成非 nil 接口。纯函数,单测直接测。
func routeRouterOptions(rs PlayerRouteStore) []routing.RouterOption {
	if rs == nil {
		return nil
	}
	return []routing.RouterOption{routing.WithRouteStore(rs), routing.WithRouteReader(rs)}
}

// newGuildDivergenceChecker 装配回档闸问 guild 的接缝(07 §7.5.1、§7.5.5)。
//
// 返回 nil = 回档一律被拒,**不是**"跳过检查"(与 LoginAdminClient 的 nil 语义相反,见 ServiceContext.GuildDivergence)。
// 任何失败都只记日志、不拒启:这三项配置只关系到回档,而 guild 启动要用 data_service(领号段、查归属),
// 这里再反向阻塞就是启动死锁(07 §7.5.5)。
func newGuildDivergenceChecker(c config.Config, track ...func(interface{ Close() error })) guildcheck.GuildDivergenceChecker {
	if err := c.ValidateGuildCheck(); err != nil {
		logx.Errorf("[ServiceContext] guild check config invalid; every Rollback* will be rejected (ErrCodeRollbackGuildCheckFailed): %v", err)
		return nil
	}
	if !c.HasGuildInternalRpc() {
		// 合法形态(k8s 今天就是这样:guild 还没有 manifest,07 §7.5.5 末条),所以是 Info;
		// 每次回档被拒时 logic 会再打一条带 "guild check unavailable: not configured" 的 ERROR。
		logx.Infof("[ServiceContext] GuildInternalRpc has no Endpoints/Target; every Rollback* will be rejected (guild check unavailable: not configured)")
		return nil
	}
	// 强制 NonBlock,不信 yaml:guild 不在线时 zrpc 默认的阻塞拨号会把 data_service 的启动按住。
	// 追加在 NewClient 内部由 yaml 决定的选项之后,后者被覆盖。
	cli, err := zrpc.NewClient(c.GuildInternalRpc, zrpc.WithNonBlock())
	if err != nil {
		logx.Errorf("[ServiceContext] GuildInternalRpc client build failed; every Rollback* will be rejected (ErrCodeRollbackGuildCheckFailed): %v", err)
		return nil
	}
	logx.Infof("[ServiceContext] guild divergence checker ready: endpoints=%v target=%q timeout=%dms margin=%dms budget=%ds",
		c.GuildInternalRpc.Endpoints, c.GuildInternalRpc.Target, c.GuildInternalRpc.Timeout,
		c.GuildClockSkewMarginMs, c.GuildCheckBudgetSeconds)
	if len(track) == 1 && track[0] != nil {
		track[0](cli.Conn())
	}
	return guildcheck.New(guildpb.NewGuildInternalClient(cli.Conn()))
}

// SQLPoolSamples 是 /metrics 的连接池快照来源(shared/sqlpoolstats.Source,G2b):只报装配成功的 store,一个 store 一个池,
// 类别即 store 名(metrics.Pool*)。字段在 NewServiceContext 之后不再写(cmd/debugutil 是另一个进程,不注册池指标),
// 抓取时并发读是安全的;sql.DB.Stats 只取一把内存锁,不做 I/O。
func (s *ServiceContext) SQLPoolSamples() []sqlpoolstats.Sample {
	if s == nil {
		return nil
	}
	return sqlPoolSamples(s.namedStores(), store.DBStatsOf)
}

// namedStores 列出五个 store 与各自的池类别(固定顺序)。单测据此核对「哪个字段报成哪个类别」。
func (s *ServiceContext) namedStores() []namedStore {
	return []namedStore{
		{pool: metrics.PoolSnapshot, store: s.SnapshotStore},
		{pool: metrics.PoolTransactionLog, store: s.TxLogStore},
		{pool: metrics.PoolIdSegment, store: s.IdSegmentStore},
		{pool: metrics.PoolPlayerName, store: s.PlayerNameStore},
		{pool: metrics.PoolPlayerRoute, store: s.PlayerRouteStore},
	}
}

// namedStore 是一个 store 与它的池类别。store 是接口字段转成的 any:接口为 nil 时 any 也是 nil,
// DBStatsOf 返回 false;单测的假实现不命中 DBStatsOf 的类型分支,同样跳过。
type namedStore struct {
	pool  string
	store any
}

// sqlPoolSamples 是 SQLPoolSamples 的纯逻辑部分;statsOf 显式传入,单测用假实现(真 store 的 db 字段在包外造不出来)。
func sqlPoolSamples(stores []namedStore, statsOf func(any) (sql.DBStats, bool)) []sqlpoolstats.Sample {
	samples := make([]sqlpoolstats.Sample, 0, len(stores))
	for _, ns := range stores {
		stats, ok := statsOf(ns.store)
		if !ok {
			continue
		}
		samples = append(samples, sqlpoolstats.Sample{Pool: ns.pool, Stats: stats})
	}
	return samples
}

// BindDrainRuntime 只认已消费的 Writer 身份；进程所有池与角色共用该 boot，不重读 grant。
func (s *ServiceContext) BindDrainRuntime(runtime *drainproof.Runtime) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if runtime == nil || s.DrainRuntime != nil || s.RootAdmission == nil || s.RelayAdmission == nil ||
		runtime.Seed("SOURCE") == nil || runtime.Seed("RELAY") == nil {
		return drainproof.ErrHold
	}
	if err := runtime.BindBoot(s.Writer.InstanceID(), s.Writer.WriterKey()); err != nil {
		return err
	}
	s.DrainRuntime = runtime
	return nil
}

// CloseRootAndRelayIngress 是 worker 可同步调用的止血接缝；不等待 worker 自身，也不关闭已接受工作需要的池。
func (s *ServiceContext) CloseRootAndRelayIngress() {
	if s.RootAdmission != nil {
		s.RootAdmission.CloseAdmission()
	}
	if s.RelayIngress != nil {
		s.RelayIngress.CloseAdmission()
	}
}

// Close 保留旧入口，但不能把未完成的关闭结果吞成成功或提前销毁 SQL。
func (s *ServiceContext) Close() {
	if err := s.CloseAndReport(); err != nil {
		logx.Errorf("data_service 关闭 HOLD，保留未决 owner/SQL: %v", err)
	}
}

// CloseAndReport 只释放已经由进程 owner 真实 join 的资源；超时、未知或角色未关闭时保留池。
func (s *ServiceContext) CloseAndReport() error {
	if s == nil {
		return nil
	}
	s.lifecycleMu.Lock()
	if s.closeCompleted {
		previous := s.closeErr
		s.lifecycleMu.Unlock()
		return previous
	}
	s.lifecycleMu.Unlock()
	if s.RootAdmission == nil || s.RelayAdmission == nil || s.RelayIngress == nil ||
		!s.RootAdmission.Closed() || !s.RelayAdmission.Closed() || !s.RelayIngress.Closed() ||
		s.RootAdmission.Pending() != 0 || s.RelayAdmission.Pending() != 0 || s.RelayIngress.Pending() != 0 {
		return drainproof.ErrHold
	}
	for _, gate := range []*drainproof.Session{s.RootAdmission, s.RelayIngress, s.RelayAdmission} {
		if err := gate.Wait(context.Background()); err != nil {
			return err
		}
	}
	s.closeOnce.Do(func() {
		s.Writer.Seal()
		for _, closer := range s.upstreamClosers {
			if closer != nil {
				s.closeErr = errors.Join(s.closeErr, closer.Close())
			}
		}
		if s.accountDirectoryCloser != nil {
			s.closeErr = errors.Join(s.closeErr, s.accountDirectoryCloser.Close())
		}
		if s.Router != nil {
			s.closeErr = errors.Join(s.closeErr, s.Router.CloseAndReport())
		}
		if s.SnapshotStore != nil {
			s.closeErr = errors.Join(s.closeErr, s.SnapshotStore.Close())
		}
		if s.TxLogStore != nil {
			s.closeErr = errors.Join(s.closeErr, s.TxLogStore.Close())
		}
		if s.IdSegmentStore != nil {
			s.closeErr = errors.Join(s.closeErr, s.IdSegmentStore.Close())
		}
		if s.PlayerNameStore != nil {
			s.closeErr = errors.Join(s.closeErr, s.PlayerNameStore.Close())
		}
		if s.PlayerRouteStore != nil {
			s.closeErr = errors.Join(s.closeErr, s.PlayerRouteStore.Close())
		}
		if s.closeErr != nil {
			s.RelayAdmission.Hold()
		}
		s.lifecycleMu.Lock()
		s.closeCompleted = true
		s.lifecycleMu.Unlock()
	})
	return s.closeErr
}
