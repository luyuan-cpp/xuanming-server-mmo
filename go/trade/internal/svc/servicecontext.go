// Package svc 是 trade 服务的依赖装配:etcd 客户端、mmorpg_trade 连接池、data_service 客户端、
// listing_id 号段客户端、通用资产通道(assetchannel.go),以及本服务的低基数 Prometheus 计数器。
package svc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"google.golang.org/grpc"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"trade/internal/commerce"
	"trade/internal/config"
	"trade/internal/data"

	dspb "proto/data_service"

	"shared/assetop"
	"shared/authoritysql"
	"shared/drainproof"
	"shared/idsegment"

	_ "github.com/go-sql-driver/mysql"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/zrpc"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// ListingIDBizTag 是发号器里 listing_id 的业务键。必须登记进 data_service 的 Tags
// (代码默认清单、etc/data_service.yaml、K8s ConfigMap 三处),否则生产 AllowAutoSeed=false 下发号被拒。
const ListingIDBizTag = "trade_listing"

const (
	// etcdDialTimeout 与 chat 同值。
	etcdDialTimeout = 5 * time.Second
	// mysqlPingTimeout 启动期探库上限,与 data_service store.openMySQL 同值。
	mysqlPingTimeout = 5 * time.Second
	// listingIDWarmTimeout 启动时同步领第一段的预算:只为把"Tags 漏登记 / data_service 不通"
	// 尽早暴露在启动日志里;失败不阻止起服,之后每次 Next 会自己再试(号段是弱依赖)。
	listingIDWarmTimeout = 5 * time.Second
)

type ServiceContext struct {
	Config config.Config

	// Etcd 供 shared/noderegistry 注册与 shared/killswitch 监听共用。
	// 与 go-zero 自己的 etcd 注册无关:Etcd.Key 为空,go-zero 不写 trade.rpc key。
	Etcd *clientv3.Client

	// DB 是 mmorpg_trade 的连接池。表由 schemamigrate 建(trade.go 启动期或 -migrate),本包不碰 DDL。
	DB *sql.DB

	// DataServiceClient 查 home_zone(BatchGetPlayerHomeZone)与领号段(AllocateIdSegment)。
	// 无条件拨号(Validate 已保证有目标);NonBlock 下 data_service 暂未起时不阻塞起服。
	DataServiceClient dspb.DataServiceClient

	// ListingIDSegment 是 listing_id 号段客户端(biz_tag=trade_listing)。没有 snowflake 回退:
	// 号段失败 = 本次上架失败(in-band kServiceUnavailable),绝不自造 id。
	ListingIDSegment *idsegment.Client

	// Assets 是通用资产通道(guild-phase2/04-asset-channel.md §S4)在 trade 侧的装配:
	// 共享 Redis 位置键 → scene 节点镜像 → AssetDebit / AssetCredit,外加 outbox 与重投循环。
	//
	// **整个字段可能为 nil**:AssetOp.Enabled=false(默认)时通道根本不装配。调用托管 / 交付前
	// 应判 Assets.Enabled(),好给玩家一个明确的"功能未开放";漏判也不会 panic —— AssetChannel
	// 的方法都带空接收者守卫,reconcile 同理,业务侧拿到的是 reconcile.ErrSignerMissing。
	Assets        *AssetChannel
	Commerce      *commerce.Engine
	Mall          *commerce.Mall
	Rmb           *commerce.RmbConsignments
	RmbIdentity   *RmbIdentity
	rmbLoginRedis *redis.Client

	// 同一 boot 的 SQL 身份与两个独立角色；P1 不关闭 RELAY 或 SQL 池。
	WriterConfig     authoritysql.WriterConfig
	RootAdmission    *drainproof.Session
	RelayAdmission   *drainproof.Session
	RelayIngress     *drainproof.Session
	DrainRuntime     *drainproof.Runtime
	lifecycleMu      sync.Mutex
	sourceMu         sync.Mutex
	sourceStop       func(context.Context) error
	sourceClosed     bool
	resourcesClosed  bool
	resourceCloseErr error
	directoryConn    *grpc.ClientConn
}

// NewServiceContext 建全部依赖。任何一项建不出来都 panic(启动致命):
// 没有库、没有 data_service 客户端、没有号段客户端的 trade 没有可用形态。
func NewServiceContext(c config.Config) *ServiceContext {
	writer, err := authoritysql.LoadWriterConfig(os.Getenv)
	if err != nil {
		panic(fmt.Errorf("trade SQL boot 装配失败: %w", err))
	}
	return NewServiceContextWithAdmission(c, writer, drainproof.New(), drainproof.New())
}

// 新 boot 的 prepared 门禁在构造时显式传入，不能在后台或 handler 已持有门禁后替换它。
func NewServiceContextWithAdmission(c config.Config, writer authoritysql.WriterConfig, root, relay *drainproof.Session) *ServiceContext {
	if len(writer.InstanceID()) != 16 || len(writer.WriterKey()) != 16 || root == nil || relay == nil || root == relay {
		panic("trade SQL boot 与 SOURCE/RELAY 装配缺失或角色复用")
	}
	etcdCli, err := clientv3.New(clientv3.Config{
		Endpoints:   c.Etcd.Hosts,
		DialTimeout: etcdDialTimeout,
	})
	if err != nil {
		panic("trade: failed to create etcd client: " + err.Error())
	}

	db, err := openMySQL(c.MySQL, authoritysql.Writer(writer))
	if err != nil {
		panic(fmt.Errorf("trade: %w", err))
	}

	conn := zrpc.MustNewClient(c.DataServiceRpc)
	dsClient := dspb.NewDataServiceClient(conn.Conn())

	seg, err := NewListingIDSegment(c.IdSegment, dsClient, relay)
	if err != nil {
		panic(fmt.Errorf("trade: listing id segment client: %w", err))
	}
	logx.Infof("[trade] listing_id minting: segment biz_tag=%s initial_step=%d step_bounds=[%d, %d] (no snowflake fallback)",
		ListingIDBizTag, c.IdSegment.StepOrDefault(), c.IdSegment.MinStepOrDefault(), c.IdSegment.MaxStepOrDefault())

	// 资产通道(guild-phase2/04-asset-channel.md §S4)。**默认关闭**:配置里没有
	// AssetOp.Enabled=true 就一件依赖都不建 —— 不拨共享 Redis、不建 scene 节点镜像、
	// 不起 outbox 重投循环。开关判在这里而不是通道内部,是因为"关"应当等于**没有装配**;
	// "装好了但不用"那种关法总会在某处漏判,而这里只有一个判点。
	var assets *AssetChannel
	if c.AssetOp.Enabled {
		// 密钥来源必须与代码实际读的是同一个变量。config.Validate 只能校验形状,这条等值
		// 只有装配层知道:不一致的表现是"配置说注入了、代码读的是别的",scene 一律回 27008。
		if c.AssetOp.SecretEnv != AssetOpSecretEnv {
			// **不回显配置里那个值**:这条不一致最常见的成因就是有人把密钥本身贴进了 SecretEnv,
			// 回显等于把它抄进启动日志(AGENTS §11.3)。只报本服务期望的变量名,足够定位。
			panic(fmt.Errorf("trade: AssetOp.SecretEnv 与本服务实际读取的 %s 不一致"+
				"(得到一个长度 %d 的值,此处刻意不回显内容):yaml / config.go / ConfigMap 三处必须逐字一致",
				AssetOpSecretEnv, len(c.AssetOp.SecretEnv)))
		}
		// Enabled=true 但密钥缺失 / 去空白后不足 32 字节 → **拒启**,不降级。
		// 玩家资产路径 fail-closed(AGENTS §11.3):一个"开着却签不出名"的 trade 会把每一次
		// 托管都写进 outbox 再永远投不出去,商品永久卡在 ESCROWING,比起不来难查得多。
		// NewAssetOpSigner 的错误文本不含密钥值,可以原样进启动日志。
		if _, err := NewAssetOpSigner(); err != nil {
			panic(fmt.Errorf("trade: AssetOp.Enabled=true 但签名密钥不可用: %w"+
				" —— 本机用 tools/scripts/start_game.ps1 注入开发值(仅限本机 dev),"+
				"预发 / 生产由部署侧注入;不打算开通道就把 AssetOp.Enabled 改回 false", err))
		}
		// 共享 Redis 连不上即启动致命(理由见 newAssetChannel)。
		assets, err = newAssetChannel(c, db, dsClient, relay)
		if err != nil {
			panic(fmt.Errorf("trade: 资产通道装配失败: %w", err))
		}
	} else {
		logx.Infof("[trade] Asset channel disabled (AssetOp.Enabled=false, default): no shared Redis dial, "+
			"scene node mirror, or outbox redelivery loop. Browse / detail / favorites unaffected; "+
			"listing escrow and delivery all return errors. Enable with AssetOp.Enabled=true and deployment-injected %s (at least %d bytes)",
			AssetOpSecretEnv, assetop.MinSecretLen)
	}

	var commerceEngine *commerce.Engine
	var mall *commerce.Mall
	if assets != nil {
		titles := map[uint32]string{}
		for key, value := range c.Commerce.ItemTitles {
			id, parseErr := strconv.ParseUint(key, 10, 32)
			if parseErr != nil || id == 0 || strconv.FormatUint(id, 10) != key {
				panic("商城道具名称目录配置号非法")
			}
			titles[uint32(id)] = value
		}
		commerceEngine = &commerce.Engine{Repo: &data.CommerceRepo{DB: db, Ops: assets.Ops}, IDs: seg, OpIDs: assets.OpIDs,
			Policy:  commerce.Policy{Items: c.Commerce.ItemConfigs, ItemTitles: titles, Pets: c.Commerce.PetConfigs, Currencies: c.Commerce.CurrencyTypes, AllowInstances: c.Commerce.AllowInstances},
			Applier: assets.Caller, Ledger: persistedLedger(dsClient), Now: time.Now}
		roles, roleErr := commerce.NewRoleAuthority(c.AccountRoleSchema)
		if roleErr != nil {
			panic("交易角色提交互锁必须显式配置同事务域全局目录")
		}
		commerceEngine.Roles = roles
		assets.Commerce = commerceEngine
		if c.Mall.URL != "" {
			mall, err = commerce.NewMall(c.Mall.URL, os.Getenv(c.Mall.GatewaySecretEnv), os.Getenv(c.Mall.DeliverySecretEnv), commerceEngine)
			if err != nil {
				panic(fmt.Errorf("商城装配失败: %w", err))
			}
			assets.Mall = mall
		}
	}
	sc := &ServiceContext{
		WriterConfig:      writer,
		RootAdmission:     root,
		RelayAdmission:    relay,
		RelayIngress:      drainproof.New(),
		Config:            c,
		Etcd:              etcdCli,
		DB:                db,
		DataServiceClient: dsClient,
		directoryConn:     conn.Conn(),
		ListingIDSegment:  seg,
		Assets:            assets,
		Commerce:          commerceEngine,
		Mall:              mall,
	}
	if sc.Assets != nil {
		sc.Assets.RelayAdmission = relay
		sc.Assets.RootAdmission = root
		if err := sc.Assets.Pipeline.BindRelayAdmission(relay); err != nil {
			panic(err)
		}
	}
	if err := sc.initRmb(); err != nil {
		panic(err)
	}
	return sc
}

// StartAssetChannel 起 scene 节点镜像与资产重投循环。ctx 取消即退出。
// 放在起 gRPC 之前调用:循环只依赖 MySQL 与 etcd,早一点开始把积压的 outbox 投出去。
// AssetOp.Enabled=false 时 Assets 为 nil,这里整体是空操作(不起任何后台 goroutine)。
func (sc *ServiceContext) StartAssetChannel(ctx context.Context) {
	sc.Assets.Start(ctx, sc.Etcd)
}

// BuildDSN 拼 mmorpg_trade 的 DSN,与 go/data_service/internal/store/mysql.go buildDSN 同口径:
// sql_mode=%27STRICT_TRANS_TABLES%27 强制会话级严格模式(%27 是转义的单引号)。标题 / 描述是自由文本,
// 非严格模式下超长写入会**静默截断且无报错**,在连接层兜底。
//
// transaction_isolation=%27READ-COMMITTED%27 把整个连接池的会话隔离级别设成 RC(驱动建连时发 SET),
// 与 go/friend 的 BuildDSN 同口径。显式事务本来就经 assetop.WithTxRetry 固定 RC(建 seq 行的短事务
// 与业务事务都走它);这一项管的是事务**之外**的自动提交语句(Claim / Reschedule / 毒行的主键 UPDATE、
// InsertListing(DeleteFavorite 2026-10-16 起改走 WithTxRetry 单语句事务),以及 data.EnsureSeqRows 那条
// 自动提交的普通读探针 —— 玩家 seq 行的建行 2026-09-29 起在它自己的 RC 短事务里),
// 它们原先落在服务器全局默认的 REPEATABLE-READ 上,删不到行 / 撞上删除标记时会拿间隙锁或 next-key 锁,
// 与别人的插入意向锁互等。RC 下只剩记录锁。自动提交语句每条各取新快照,读语义与 RR 下相同。
// 前提同 WithTxRetry:binlog_format=ROW。
//
// 返回值含密码,只能交给 sql.Open,绝不打日志。
func BuildDSN(c config.MySQLConf) string {
	return fmt.Sprintf("%s:%s@tcp(%s)/%s?parseTime=true&charset=utf8mb4&sql_mode=%%27STRICT_TRANS_TABLES%%27"+
		"&transaction_isolation=%%27READ-COMMITTED%%27",
		c.User, c.Password, c.Host, c.DBName)
}

// MySQLTarget 是连接目标的可读描述(不含密码),供横幅与错误信息使用。
func MySQLTarget(c config.MySQLConf) string {
	return fmt.Sprintf("%s/%s (user=%s)", c.Host, c.DBName, c.User)
}

// OpenMySQL 建池、设上限、ping。返回错误时池已关闭;错误信息点名库名
// (库不存在时 -migrate 以 1 退出并点名库名,D-14 第 5 条)。
func OpenMySQL(c config.MySQLConf) (*sql.DB, error) {
	writer, err := authoritysql.LoadWriterConfig(os.Getenv)
	if err != nil {
		return nil, err
	}
	return openMySQL(c, authoritysql.Writer(writer))
}

// OpenSchemaMySQL只属于显式迁移入口。
func OpenSchemaMySQL(c config.MySQLConf) (*sql.DB, error) {
	permission, err := authoritysql.LoadSchemaPermission(os.Getenv)
	if err != nil {
		return nil, err
	}
	return openMySQL(c, permission)
}

func openMySQL(c config.MySQLConf, permission authoritysql.Permission) (*sql.DB, error) {
	ctx, cancel := context.WithTimeout(context.Background(), mysqlPingTimeout)
	defer cancel()
	return authoritysql.Open(ctx, BuildDSN(c), c.DBName, authoritysql.Backend(c.Backend), c.MaxOpenConn, permission)
}

// NewListingIDSegment 按 IdSegment 配置建 listing_id 号段客户端。Enabled=false 或客户端为 nil 直接报错:
// trade 没有 snowflake,静默退化只会让所有上架在运行期失败。
func NewListingIDSegment(cfg idsegment.Conf, client dspb.DataServiceClient, roles ...*drainproof.Session) (*idsegment.Client, error) {
	if !cfg.Enabled {
		return nil, errors.New("IdSegment.Enabled=false: trade mints listing_id only from the id issuer (biz_tag=" + ListingIDBizTag + ")")
	}
	if client == nil {
		return nil, errors.New("data_service client is nil: listing_id minting needs data_service.AllocateIdSegment")
	}
	allocate := idsegment.Adapt(client.AllocateIdSegment, func(bizTag string, step uint32) *dspb.AllocateIdSegmentRequest {
		return &dspb.AllocateIdSegmentRequest{BizTag: bizTag, Step: step}
	})
	return idsegment.New(bindAllocationOwner(allocate, roles...),
		idsegment.Options{
			BizTag:  ListingIDBizTag,
			Step:    cfg.StepOrDefault(),
			MinStep: cfg.MinStepOrDefault(),
			MaxStep: cfg.MaxStepOrDefault(),
		},
	)
}

// WarmListingIDSegment 启动时同步领第一段;失败只告警(之后每次 Next 会自己再试)。
func (sc *ServiceContext) WarmListingIDSegment() {
	if sc.ListingIDSegment == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), listingIDWarmTimeout)
	defer cancel()
	if err := sc.ListingIDSegment.Warm(ctx); err != nil {
		logx.Errorf("[trade] listing_id first segment not available yet (data_service.AllocateIdSegment biz_tag=%s): %v "+
			"— SeedListing fails closed with kServiceUnavailable until data_service answers; "+
			"in production also check that %s is in data_service Tags", ListingIDBizTag, err, ListingIDBizTag)
	}
}

// BindDrainRuntime 绑定已经消费的同一 SQL boot；不会重新读 grant 或替换门禁。
// 实际 main 仍须将 RootAdmission 接进 RPC/cleanup，并将 RelayAdmission 接进资产与回执链。
func (sc *ServiceContext) BindDrainRuntime(runtime *drainproof.Runtime) error {
	sc.lifecycleMu.Lock()
	defer sc.lifecycleMu.Unlock()
	if runtime == nil || sc.DrainRuntime != nil || sc.RootAdmission == nil || sc.RelayAdmission == nil ||
		sc.RootAdmission == sc.RelayAdmission || runtime.Seed("SOURCE") == nil || runtime.Seed("RELAY") == nil {
		return drainproof.ErrHold
	}
	if err := runtime.BindBoot(sc.WriterConfig.InstanceID(), sc.WriterConfig.WriterKey()); err != nil {
		return err
	}
	sc.DrainRuntime = runtime
	return nil
}

// BindSourceStop 由 main 交付唯一 cleanup 发起者；停止只影响取新，不取消已接纳 SQL。
func (sc *ServiceContext) BindSourceStop(stop func(context.Context) error) error {
	sc.sourceMu.Lock()
	defer sc.sourceMu.Unlock()
	if stop == nil || sc.sourceStop != nil || sc.sourceClosed {
		return drainproof.ErrHold
	}
	sc.sourceStop = stop
	return nil
}
func (sc *ServiceContext) CloseSource(ctx context.Context) error {
	sc.sourceMu.Lock()
	defer sc.sourceMu.Unlock()
	if sc.RootAdmission == nil {
		return drainproof.ErrHold
	}
	if sc.sourceClosed {
		return nil
	}
	sc.RootAdmission.CloseAdmission()
	if sc.sourceStop != nil {
		if err := sc.sourceStop(ctx); err != nil {
			sc.RootAdmission.Hold()
			return err
		}
	}
	if err := sc.RootAdmission.Wait(ctx); err != nil {
		return err
	}
	// listing_id 同时被商城/RMB恢复使用；实际 Allocate 与预取归 RELAY，直到 P3 join 后关闭。
	sc.sourceClosed = true
	return nil
}
func (sc *ServiceContext) StopAndReport(ctx context.Context) error {
	sc.lifecycleMu.Lock()
	defer sc.lifecycleMu.Unlock()
	if sc.resourcesClosed {
		return sc.resourceCloseErr
	}
	if err := sc.CloseSource(ctx); err != nil {
		return err
	}
	if sc.RelayAdmission == nil {
		return drainproof.ErrHold
	}
	if sc.RelayIngress != nil {
		sc.RelayIngress.CloseAdmission()
		if err := sc.RelayIngress.Wait(ctx); err != nil {
			return err
		}
	}
	if err := sc.Assets.CloseAndReport(ctx); err != nil {
		sc.RelayAdmission.Hold()
		return err
	}
	if sc.ListingIDSegment != nil {
		digest, err := sc.ListingIDSegment.DrainAndReport(ctx)
		if err != nil {
			sc.RelayAdmission.Hold()
			return err
		}
		if err = sc.RelayAdmission.MergeSettled(digest); err != nil {
			return err
		}
	}
	if err := sc.RelayAdmission.WaitIdle(ctx); err != nil {
		return err
	}
	sc.RelayAdmission.CloseAdmission()
	if err := sc.RelayAdmission.Wait(ctx); err != nil {
		return err
	}
	sc.WriterConfig.Seal()
	var result error
	if sc.directoryConn != nil {
		result = errors.Join(result, sc.directoryConn.Close())
	}
	if sc.rmbLoginRedis != nil {
		result = errors.Join(result, sc.rmbLoginRedis.Close())
	}
	if sc.DB != nil {
		result = errors.Join(result, sc.DB.Close())
	}
	if sc.Etcd != nil {
		result = errors.Join(result, sc.Etcd.Close())
	}
	sc.resourcesClosed = true
	sc.resourceCloseErr = result
	if result != nil {
		sc.RelayAdmission.Hold()
	}
	return result
}
func (sc *ServiceContext) Stop() {
	if err := sc.StopAndReport(context.Background()); err != nil {
		logx.Errorf("trade 排空 HOLD: %v", err)
	}
}

// 号段客户端可后台预取；每一次真正 Allocate 在同 boot RELAY 内登记，根返回不会丢掉后继。
func bindAllocationOwner(allocate idsegment.AllocateFunc, roles ...*drainproof.Session) idsegment.AllocateFunc {
	if len(roles) == 0 {
		return allocate
	}
	if len(roles) != 1 || roles[0] == nil {
		panic("trade 号段 RELAY owner 缺失")
	}
	owner := roles[0]
	return func(ctx context.Context, tag string, step uint32) (lo, hi uint64, resultErr error) {
		finish, err := owner.Enter()
		if err != nil {
			return 0, 0, err
		}
		defer finish()
		defer func() {
			if recovered := recover(); recovered != nil {
				owner.Hold()
				panic(recovered)
			}
			if resultErr != nil {
				owner.Hold()
			}
		}()
		return allocate(ctx, tag, step)
	}
}

// ---------------------------------------------------------------------------
// Prometheus 指标
// ---------------------------------------------------------------------------
//
// 放在 svc 而不是 main:logic 要记 result,而 logic 不能 import main。
// label 只有 result 一个有限枚举;player_id 一律只进日志(AGENTS.md §9)。

// result label 取值(低基数,全集就是下面这些)。
const (
	ResultOK       = "ok"       // 成功
	ResultUnmapped = "unmapped" // home_zone 映射里没有这名玩家(数据状态)
	ResultRejected = "rejected" // SeedListing 被拒:非 dev/test、参数非法、卖家 home_zone 未映射
	ResultError    = "error"    // data_service / MySQL / 号段故障
)

var (
	homeZoneLookupTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: "trade",
		Name:      "home_zone_lookup_total",
		Help:      "BatchGetPlayerHomeZone lookups by result (ok|unmapped|error).",
	}, []string{"result"})

	seedListingTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: "trade",
		Name:      "seed_listing_total",
		Help:      "TradeAdmin.SeedListing calls by result (ok|rejected|error).",
	}, []string{"result"})

	registerMetricsOnce sync.Once
)

// ObserveHomeZoneLookup 记录一次 home_zone 查询的结果。
func ObserveHomeZoneLookup(result string) {
	homeZoneLookupTotal.WithLabelValues(result).Inc()
}

// ObserveSeedListing 记录一次 SeedListing 的终态。
func ObserveSeedListing(result string) {
	seedListingTotal.WithLabelValues(result).Inc()
}

// StartMetrics 启动 Prometheus /metrics 端点;addr 为空则关闭(与 chat 同模式)。
// 计数器只在这里注册进默认 registry:单测里没调 StartMetrics 也能正常 Inc,不会重复注册 panic。
func StartMetrics(addr string) {
	if addr == "" {
		logx.Info("[trade] MetricsListenAddr empty; Prometheus /metrics endpoint disabled")
		return
	}
	registerMetricsOnce.Do(func() {
		prometheus.MustRegister(homeZoneLookupTotal, seedListingTotal)
		// 预建全部 result 序列(值 0):否则"从未出错"与"指标不存在"在告警规则里长得一样。
		for _, r := range []string{ResultOK, ResultUnmapped, ResultError} {
			homeZoneLookupTotal.WithLabelValues(r)
		}
		for _, r := range []string{ResultOK, ResultRejected, ResultError} {
			seedListingTotal.WithLabelValues(r)
		}
	})
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logx.Infof("[trade] Prometheus /metrics listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logx.Errorf("[trade] metrics HTTP server exited: %v", err)
		}
	}()
}
