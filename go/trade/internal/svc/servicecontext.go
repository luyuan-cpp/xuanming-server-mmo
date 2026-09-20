// Package svc 是 trade 服务的依赖装配:etcd 客户端、mmorpg_trade 连接池、data_service 客户端、
// listing_id 号段客户端、通用资产通道(assetchannel.go),以及本服务的低基数 Prometheus 计数器。
package svc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"trade/internal/config"

	dspb "proto/data_service"

	"shared/assetop"
	"shared/idsegment"

	_ "github.com/go-sql-driver/mysql"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/zrpc"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// ListingIDBizTag 是 id_segment 表里 listing_id 的业务键。必须登记进 data_service 的 BootstrapTags
// (代码默认清单、etc/data_service.yaml、K8s ConfigMap 三处),否则生产 AllowAutoSeed=false 下发号被拒。
const ListingIDBizTag = "trade_listing"

const (
	// etcdDialTimeout 与 chat 同值。
	etcdDialTimeout = 5 * time.Second
	// mysqlPingTimeout 启动期探库上限,与 data_service store.openMySQL 同值。
	mysqlPingTimeout = 5 * time.Second
	// listingIDWarmTimeout 启动时同步领第一段的预算:只为把"BootstrapTags 漏登记 / data_service 不通"
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
	Assets *AssetChannel

	stopOnce sync.Once
}

// NewServiceContext 建全部依赖。任何一项建不出来都 panic(启动致命):
// 没有库、没有 data_service 客户端、没有号段客户端的 trade 没有可用形态。
func NewServiceContext(c config.Config) *ServiceContext {
	etcdCli, err := clientv3.New(clientv3.Config{
		Endpoints:   c.Etcd.Hosts,
		DialTimeout: etcdDialTimeout,
	})
	if err != nil {
		panic("trade: failed to create etcd client: " + err.Error())
	}

	db, err := OpenMySQL(c.MySQL)
	if err != nil {
		panic(fmt.Errorf("trade: %w", err))
	}

	conn := zrpc.MustNewClient(c.DataServiceRpc)
	dsClient := dspb.NewDataServiceClient(conn.Conn())

	seg, err := NewListingIDSegment(c.IdSegment, dsClient)
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
			panic(fmt.Errorf("trade: AssetOp.SecretEnv=%q 与本服务实际读取的 %s 不一致:"+
				"yaml / config.go / ConfigMap 三处必须逐字一致", c.AssetOp.SecretEnv, AssetOpSecretEnv))
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
		assets, err = newAssetChannel(c, db, dsClient)
		if err != nil {
			panic(fmt.Errorf("trade: 资产通道装配失败: %w", err))
		}
	} else {
		logx.Infof("[trade] 资产通道已关闭(AssetOp.Enabled=false,默认值):不拨共享 Redis、"+
			"不建 scene 节点镜像、不起 outbox 重投循环。浏览 / 详情 / 收藏不受影响,"+
			"上架托管与交付一律以错误返回。要打开:配置 AssetOp.Enabled=true 且由部署侧注入 %s(≥%d 字节)",
			AssetOpSecretEnv, assetop.MinSecretLen)
	}

	return &ServiceContext{
		Config:            c,
		Etcd:              etcdCli,
		DB:                db,
		DataServiceClient: dsClient,
		ListingIDSegment:  seg,
		Assets:            assets,
	}
}

// StartAssetChannel 起 scene 节点镜像与资产重投循环。ctx 取消即两者退出。
// 放在起 gRPC 之前调用:循环只依赖 MySQL 与 etcd,早一点开始把积压的 outbox 投出去。
// AssetOp.Enabled=false 时 Assets 为 nil,这里是空操作(不起任何后台 goroutine)。
func (sc *ServiceContext) StartAssetChannel(ctx context.Context) {
	sc.Assets.Start(ctx, sc.Etcd)
}

// BuildDSN 拼 mmorpg_trade 的 DSN,与 go/data_service/internal/store/mysql.go buildDSN 同口径:
// sql_mode=%27STRICT_TRANS_TABLES%27 强制会话级严格模式(%27 是转义的单引号)。标题 / 描述是自由文本,
// 非严格模式下超长写入会**静默截断且无报错**,在连接层兜底。
// 返回值含密码,只能交给 sql.Open,绝不打日志。
func BuildDSN(c config.MySQLConf) string {
	return fmt.Sprintf("%s:%s@tcp(%s)/%s?parseTime=true&charset=utf8mb4&sql_mode=%%27STRICT_TRANS_TABLES%%27",
		c.User, c.Password, c.Host, c.DBName)
}

// MySQLTarget 是连接目标的可读描述(不含密码),供横幅与错误信息使用。
func MySQLTarget(c config.MySQLConf) string {
	return fmt.Sprintf("%s/%s (user=%s)", c.Host, c.DBName, c.User)
}

// OpenMySQL 建池、设上限、ping。返回错误时池已关闭;错误信息点名库名
// (库不存在时 -migrate 以 1 退出并点名库名,D-14 第 5 条)。
func OpenMySQL(c config.MySQLConf) (*sql.DB, error) {
	db, err := sql.Open("mysql", BuildDSN(c))
	if err != nil {
		return nil, fmt.Errorf("open MySQL %s: %w", MySQLTarget(c), err)
	}
	db.SetMaxOpenConns(c.MaxOpenConn)
	db.SetMaxIdleConns(c.MaxIdleConn)

	ctx, cancel := context.WithTimeout(context.Background(), mysqlPingTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping MySQL %s 失败(库 %s 不存在时先按 deploy/mysql-init/00_init_zone_dbs.sql 建库并授权): %w",
			MySQLTarget(c), c.DBName, err)
	}
	return db, nil
}

// NewListingIDSegment 按 IdSegment 配置建 listing_id 号段客户端。Enabled=false 或客户端为 nil 直接报错:
// trade 没有 snowflake,静默退化只会让所有上架在运行期失败。
func NewListingIDSegment(cfg idsegment.Conf, client dspb.DataServiceClient) (*idsegment.Client, error) {
	if !cfg.Enabled {
		return nil, errors.New("IdSegment.Enabled=false: trade mints listing_id only from id_segment (biz_tag=" + ListingIDBizTag + ")")
	}
	if client == nil {
		return nil, errors.New("data_service client is nil: listing_id minting needs data_service.AllocateIdSegment")
	}
	return idsegment.New(
		idsegment.Adapt(client.AllocateIdSegment,
			func(bizTag string, step uint32) *dspb.AllocateIdSegmentRequest {
				return &dspb.AllocateIdSegmentRequest{BizTag: bizTag, Step: step}
			}),
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
			"in production also check that %s is in data_service BootstrapTags", ListingIDBizTag, err, ListingIDBizTag)
	}
}

// Stop 依次关号段、DB、etcd。幂等。
// 调用方须保证 noderegistry.Close() 与 killswitch 的 ctx 取消都**先于**它(二者共用 etcd 连接)。
func (sc *ServiceContext) Stop() {
	sc.stopOnce.Do(func() {
		// 资产通道先停:循环里在途的投递要在关 DB / etcd 之前收手。
		sc.Assets.Close()
		if sc.ListingIDSegment != nil {
			sc.ListingIDSegment.Close()
		}
		if sc.DB != nil {
			if err := sc.DB.Close(); err != nil {
				logx.Errorf("[trade] MySQL close failed: %v", err)
			}
		}
		if sc.Etcd != nil {
			if err := sc.Etcd.Close(); err != nil {
				logx.Errorf("[trade] etcd client close failed: %v", err)
			}
		}
	})
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
