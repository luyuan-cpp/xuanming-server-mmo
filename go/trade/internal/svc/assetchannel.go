package svc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"trade/internal/config"
	"trade/internal/constants"
	"trade/internal/data"
	"trade/internal/reconcile"

	dspb "proto/data_service"

	"shared/assetop"
	"shared/idsegment"
	"shared/safego"
	"shared/scenenode"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// 通用资产通道(docs/design/guild-phase2/04-asset-channel.md §S4)在 trade 侧的装配。
//
// 一句话:trade 在自己库里写一行"待办资产指令"(outbox),按 player:{id}:location 找到
// 玩家当前所在的 scene 节点,调它的 SceneNodeGrpc.AssetDebit / AssetCredit / AssetAbortDebit,
// 直到拿到**已落盘**的终局才把这一行结掉。正确性全在 scene 侧的 seq 账本里,本层只负责接线。
const (
	// AssetOpCaller 是本服务在资产 RPC 白名单里的名字。scene 按 (stream, caller) 双向校验:
	// TRADE_DEBIT / TRADE_CREDIT 只认 "trade",拿 guild 的密钥签 trade 的流会被拒(§4.32)。
	AssetOpCaller = "trade"

	// AssetOpSecretEnv 是签名密钥的环境变量名。每个调用方一把,去首尾空白后不足 32 字节视同未配。
	// **不做 dev 放行**:资产路径一律 fail-closed,本地由 tools/scripts/go_services.ps1 显式注入开发值。
	AssetOpSecretEnv = "MMORPG_ASSET_OP_SECRET_TRADE"

	// AssetOpIDBizTag 是 op_id 的号段业务键(proto/trade/trade_table.proto 的 TradeAssetOpRecord.op_id)。
	// 与 listing_id 分开:两者值域各自连续,对账时一眼能看出这个 id 是商品还是资产指令。
	//
	// **必须**登记进 data_service 的 BootstrapTags **四处**,否则生产(AllowAutoSeed=false)
	// 发号被拒 = 一上架就失败(本地 dev 自动播种,不会暴露):
	//   1. go/data_service/internal/config/config.go  DefaultIdSegmentBootstrapTags
	//   2. go/data_service/internal/store/id_segment_store.go  DefaultIdSegmentBootstrapTags
	//      (config 不能 import store,那边自己写了一份;两份注释互指"必须一致")
	//   3. go/data_service/etc/data_service.yaml      IdSegment.BootstrapTags
	//   4. tools/scripts/k8s_deploy.ps1               data-service ConfigMap 的同一行
	AssetOpIDBizTag = "trade_asset_op"

	// sceneWatcherName 是 etcd 节点镜像的名字,进 scenenode_nodes{kind} 这一个 label。
	sceneWatcherName = "scene"

	// sharedRedisPingTimeout 启动期探共享 Redis 的上限,与 mysqlPingTimeout 同值。
	sharedRedisPingTimeout = 5 * time.Second

	// assetOpIDWarmTimeout 启动时同步领第一段 op_id 的预算,与 listingIDWarmTimeout 同值。
	assetOpIDWarmTimeout = 5 * time.Second
)

// ErrAssetSecretMissing:没配 AssetOpSecretEnv,或密钥短于 32 字节。
// 这**不是**启动致命错:P1 的浏览 / 详情 / 收藏不碰资产,照常服务;但托管与交付整条路不构造,
// 任何上架尝试都会当场失败,而不是写进 outbox 之后永远投不出去。
var ErrAssetSecretMissing = errors.New("trade: 资产通道密钥未配置")

// AssetChannel 是资产通道的一整套依赖。Pipeline 为 nil 表示通道未启用(密钥缺失),
// 调用方必须判空:nil 管线上不存在"先写进 outbox 再说"的中间态。
type AssetChannel struct {
	// Redis 共享单库客户端,只读 player:{id}:location。
	Redis *redis.Client
	// Conns 到各 scene 节点的 gRPC 连接缓存(按 endpoint)。
	Conns *scenenode.ConnCache
	// Watcher etcd 里 SceneNodeService.rpc/ 前缀的节点镜像。
	Watcher *scenenode.Watcher
	// Locator 位置键 + 节点镜像 → 某个 scene 节点的客户端。
	Locator *scenenode.Locator
	// Caller 一次投递 + durable 重查;密钥缺失时为 nil。
	Caller *assetop.Caller
	// Ops outbox / seq 的 MySQL 实现(同时是 assetop.Store)。
	Ops *data.AssetOpRepo
	// OpIDs op_id 号段客户端(biz_tag = trade_asset_op)。
	OpIDs *idsegment.Client
	// Pipeline 入队 + 重投循环的门面;密钥缺失时为 nil。
	Pipeline *reconcile.Pipeline

	startOnce sync.Once
	stopOnce  sync.Once
	stopLoops context.CancelFunc
}

// Enabled 报告资产通道是否可用(能签名 = 能真的发出资产 RPC)。
func (a *AssetChannel) Enabled() bool { return a != nil && a.Pipeline != nil }

// newAssetChannel 建整套依赖。返回错误 = 启动致命(共享 Redis 连不上、outbox 表名非法、
// op_id 号段建不出来);密钥缺失只降级,不返回错误。
func newAssetChannel(c config.Config, db *sql.DB, ds dspb.DataServiceClient) (*AssetChannel, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:     c.SharedRedis.Host,
		Password: c.SharedRedis.Password,
		DB:       c.SharedRedis.DB,
	})
	pingCtx, cancel := context.WithTimeout(context.Background(), sharedRedisPingTimeout)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		_ = rdb.Close()
		// 探不通就不起服:位置键读不到时资产指令一律 NOT_HERE,表现是"上架永远卡在处理中",
		// 比起服失败难查得多。地址只含 host:port,不含密码。
		return nil, fmt.Errorf("连接共享 Redis %s (DB %d) 失败(位置键 player:{id}:location 在这里,"+
			"必须与 C++ scene / scene_manager 同一实例): %w", c.SharedRedis.Host, c.SharedRedis.DB, err)
	}

	ops, err := data.NewAssetOpRepo(db, constants.StoreOpTimeout)
	if err != nil {
		_ = rdb.Close()
		return nil, err
	}

	opIDs, err := NewAssetOpIDSegment(c.IdSegment, ds)
	if err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("op_id segment client: %w", err)
	}

	assetMetrics, nodeMetrics := assetChannelMetrics()
	conns := scenenode.NewConnCache()
	watcher := scenenode.NewWatcher(sceneWatcherName, scenenode.SceneNodeRpcPrefix, nodeMetrics.SetNodes,
		// 节点从 etcd 消失时丢掉它的连接:留着只会让下一次投递卡在一个已经没人听的地址上。
		func(e scenenode.NodeEntry) { conns.Remove(e.Endpoint) })
	locator := &scenenode.Locator{
		Reader:  scenenode.GoRedisReader{Client: rdb},
		Watcher: watcher,
		Conns:   conns,
		Metrics: nodeMetrics,
	}

	ch := &AssetChannel{
		Redis:   rdb,
		Conns:   conns,
		Watcher: watcher,
		Locator: locator,
		Ops:     ops,
		OpIDs:   opIDs,
	}

	signer, err := NewAssetOpSigner()
	if err != nil {
		// **不可达的防御性兜底**:调用方(NewServiceContext)在 AssetOp.Enabled=true 分支里
		// 已经先跑过一次 NewAssetOpSigner,失败即 panic 拒启 —— 资产路径 fail-closed,
		// 不降级(AGENTS §11.3)。走到这里只可能是两次调用之间环境变量被改掉,
		// 或者将来有人绕开 NewServiceContext 直接调本函数。
		//
		// 所以这里也**返回错误**,由上层统一拒启,不再自己降级:留一条"悄悄降级"的路,
		// 就等于给 fail-closed 开了个后门,而它的表现是商品永久卡在 ESCROWING。
		return nil, fmt.Errorf("trade: 资产通道签名器不可用(NewServiceContext 本应已拦下): %w", err)
	}

	ch.Caller = &assetop.Caller{
		Resolver:    locator,
		Signer:      signer,
		CallTimeout: reconcile.CallTimeout,
		Requery:     assetop.DefaultRequery,
		Metrics:     assetMetrics,
	}
	pipeline, err := reconcile.New(reconcile.Deps{
		Ops:     ops,
		Caller:  ch.Caller,
		OpIDs:   opIDs,
		Metrics: assetMetrics,
	})
	if err != nil {
		// 到这里只剩"循环参数非法"一种可能,那是代码里的常量写错了,必须炸在启动。
		ch.Close()
		return nil, err
	}
	ch.Pipeline = pipeline
	return ch, nil
}

// Start 起 etcd 节点镜像与重投循环。ctx 取消即两者退出;重复调用只生效一次。
//
// 顺序:先起 watcher 再起循环。反过来也能工作(镜像没同步时 Resolve 回 node_unknown,
// 循环按退避重来),只是白跑一轮。
func (a *AssetChannel) Start(ctx context.Context, etcd *clientv3.Client) {
	if a == nil {
		return
	}
	a.startOnce.Do(func() {
		loopCtx, cancel := context.WithCancel(ctx)
		a.stopLoops = cancel
		safego.Go("trade.scenenode.watcher", func() { a.Watcher.Run(loopCtx, etcd) })
		if a.Pipeline == nil {
			// 装配层已保证:AssetOp.Enabled=true 走到这里 Pipeline 必非 nil(密钥不可用会在
			// NewServiceContext 就拒启),Enabled=false 则整个 AssetChannel 是 nil、上面就返回了。
			// 留这条分支只为"将来有人绕开装配层直接构造"时不静默起半个循环。
			logx.Error("[trade] 资产通道装配不完整(Pipeline 为空):只起 scene 节点镜像,不起重投循环 —— " +
				"这不该发生,说明有人绕开了 NewServiceContext 的开关与密钥校验")
			return
		}
		a.Pipeline.Start(loopCtx)
	})
}

// Close 停循环并关连接。幂等;调用方须保证它先于 etcd 客户端的关闭。
//
// 在途的资产 RPC 被连接关闭打断不会丢数据:那一行仍是 PENDING,下次循环照常重投,
// scene 见过的 seq 只会把原结局再答一遍(不变量 I2)。
func (a *AssetChannel) Close() {
	if a == nil {
		return
	}
	a.stopOnce.Do(func() {
		if a.stopLoops != nil {
			a.stopLoops()
		}
		if a.OpIDs != nil {
			a.OpIDs.Close()
		}
		if a.Conns != nil {
			a.Conns.Close()
		}
		if a.Redis != nil {
			if err := a.Redis.Close(); err != nil {
				logx.Errorf("[trade] 共享 Redis 关闭失败: %v", err)
			}
		}
	})
}

// WarmAssetOpIDSegment 启动时同步领第一段 op_id;失败只告警(之后每次 Next 会自己再试)。
// 与 listing_id 同一口径:提前把"BootstrapTags 漏登记 trade_asset_op"暴露在启动日志里。
func (a *AssetChannel) WarmAssetOpIDSegment() {
	if a == nil || a.OpIDs == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), assetOpIDWarmTimeout)
	defer cancel()
	if err := a.OpIDs.Warm(ctx); err != nil {
		logx.Errorf("[trade] op_id 号段暂不可用(data_service.AllocateIdSegment biz_tag=%s): %v "+
			"—— 在 data_service 答复之前,上架托管会以 kServiceUnavailable 失败;"+
			"生产环境还要确认 %s 已在 data_service 的 BootstrapTags 里", AssetOpIDBizTag, err, AssetOpIDBizTag)
	}
}

// NewAssetOpSigner 从环境变量取密钥建签名器。密钥值绝不进日志 / 指标 / 错误文本(AGENTS §9)。
func NewAssetOpSigner() (*assetop.Signer, error) {
	secret := os.Getenv(AssetOpSecretEnv)
	if secret == "" {
		return nil, fmt.Errorf("%w: 环境变量 %s 未设置", ErrAssetSecretMissing, AssetOpSecretEnv)
	}
	signer, err := assetop.NewSigner(AssetOpCaller, secret)
	if err != nil {
		return nil, fmt.Errorf("%w: 环境变量 %s: %w", ErrAssetSecretMissing, AssetOpSecretEnv, err)
	}
	return signer, nil
}

// NewAssetOpIDSegment 按 IdSegment 配置建 op_id 号段客户端(biz_tag = trade_asset_op)。
// 与 listing_id 共用同一份 Step / MinStep / MaxStep:两者都是"每次上架各取一个"的量级。
// 同样没有 snowflake 回退 —— 发不出号就当场拒绝上架,绝不自造 id(自造 = outbox 撞主键)。
func NewAssetOpIDSegment(cfg idsegment.Conf, client dspb.DataServiceClient) (*idsegment.Client, error) {
	if !cfg.Enabled {
		return nil, errors.New("IdSegment.Enabled=false: op_id 只由号段发(biz_tag=" + AssetOpIDBizTag + ")")
	}
	if client == nil {
		return nil, errors.New("data_service client is nil: op_id 发号需要 data_service.AllocateIdSegment")
	}
	return idsegment.New(
		idsegment.Adapt(client.AllocateIdSegment,
			func(bizTag string, step uint32) *dspb.AllocateIdSegmentRequest {
				return &dspb.AllocateIdSegmentRequest{BizTag: bizTag, Step: step}
			}),
		idsegment.Options{
			BizTag:  AssetOpIDBizTag,
			Step:    cfg.StepOrDefault(),
			MinStep: cfg.MinStepOrDefault(),
			MaxStep: cfg.MaxStepOrDefault(),
		},
	)
}

// ---------------------------------------------------------------------------
// 指标
// ---------------------------------------------------------------------------

// assetop / scenenode 的指标句柄在包级建一次:两者都用 promauto,重复构造会因指标重名 panic,
// 而 NewServiceContext 在测试里可能被调多次。service label 固定为 "trade"。
var (
	assetChannelMetricsOnce sync.Once
	assetOpMetrics          *assetop.Metrics
	sceneNodeMetrics        *scenenode.Metrics
)

func assetChannelMetrics() (*assetop.Metrics, *scenenode.Metrics) {
	assetChannelMetricsOnce.Do(func() {
		assetOpMetrics = assetop.NewMetrics(prometheus.DefaultRegisterer, "trade")
		sceneNodeMetrics = scenenode.NewMetrics(prometheus.DefaultRegisterer, "trade")
	})
	return assetOpMetrics, sceneNodeMetrics
}
