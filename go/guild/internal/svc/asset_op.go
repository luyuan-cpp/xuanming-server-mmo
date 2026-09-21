package svc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	clientv3 "go.etcd.io/etcd/client/v3"

	"guild/internal/config"
	"guild/internal/data"
	assetpb "proto/common/asset"
	dspb "proto/data_service"
	"shared/assetop"
	"shared/idsegment"
	"shared/safego"
	"shared/scenenode"
)

// 通用资产通道(docs/design/guild-phase2/04-asset-channel.md §S4)在 guild 侧的装配(B5b,05-economy.md §5.29)。
//
// 一句话:guild 在 mmorpg_guild 里写一行"待办资产指令"(guild_asset_op),按 player:{id}:location
// 找到玩家当前所在的 scene 节点,调它的 AssetDebit / AssetCredit / AssetAbortDebit,直到拿到
// **已落盘**的终局才把这一行结掉(对侧账在 data.GuildAssetStore.Finalize 里)。正确性在 scene 的
// seq 账本里,本文件只负责接线与启停;形状照 go/trade/internal/svc/assetchannel.go。
const (
	// AssetOpBizTag 是 op_id 的号段业务键(guild_db.proto 的 GuildAssetOpRecord.op_id)。
	// 与 guild_id 分开:两者值域各自连续,对账时一眼能看出这个 id 是帮会还是资产指令。
	// data_service 的 BootstrapTags 四处已由 B5a 登记(05-economy.md 顶部 B5a 订正表)。
	AssetOpBizTag = "guild_asset_op"

	// AssetOpCaller 是本服务在资产 RPC 白名单里的名字。scene 按 (stream, caller) 双向校验:
	// GUILD_DEBIT / GUILD_CREDIT 只认 "guild",拿 trade 的密钥签 guild 的流会被拒。
	AssetOpCaller = "guild"

	// AssetOpSecretEnv 是签名密钥的环境变量名(§4.32 的 MMORPG_ASSET_OP_SECRET_<CALLER>)。
	// **不做 dev 放行**:本机由 tools/scripts/go_services.ps1 / start_game.ps1 从
	// run/secrets/assetop-dev.env 注入(与 scene 同一把),预发 / 生产由部署侧注入。
	AssetOpSecretEnv = "MMORPG_ASSET_OP_SECRET_GUILD"

	// sceneWatcherName 是 etcd 节点镜像的名字,进 scenenode_nodes{kind} 这一个 label。
	sceneWatcherName = "scene"

	// assetOpIDWarmTimeout 启动时同步领第一段 op_id 的预算,与 guildIDWarmTimeout 同值。
	assetOpIDWarmTimeout = 5 * time.Second
)

// ErrAssetSecretMissing:没配 AssetOpSecretEnv,或密钥去首尾空白后短于 32 字节。
// AssetOp.Enabled=true 时它是**启动致命错**(guild.go 里 logx.Must):资产路径 fail-closed,
// 没有签名器就没有"先写进 outbox、以后再投"的中间态 —— 写进去也永远投不出去。
var ErrAssetSecretMissing = errors.New("guild: 资产通道密钥未配置")

// NewAssetOpSigner 从环境变量取密钥建签名器。密钥值绝不进日志 / 指标 / 错误文本(AGENTS §11.3):
// 错误里只有变量名与 assetop 给出的"太短",没有值、也没有值的长度。
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

// GuildAssetStreams 返回 guild 独占的两条资产流(不变量 I6:每条流只有一个服务分配 seq)。
// 每次返回新切片,调用方改了也影响不到别人。
func GuildAssetStreams() []assetpb.AssetOpStream {
	return []assetpb.AssetOpStream{
		assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT,
		assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT,
	}
}

// LoopConfigFrom 把 AssetOp 配置换算成 assetop.LoopConfig。
//
// retryBase 来自配表 GuildRule.asset_op_retry_base_ms(logic.AssetOpRetryBase,须在
// logic.ValidateEconomyTables 之后取):它是策划口径的"多久重试一次",不进 yaml。
//
// AwaitDurableDelay 不在 Y-06 的配置项里,取 assetop.DefaultLoopConfig 的值(唯一事实源在那边)。
// Streams 必须填:留空时循环不刷 assetop_pending_oldest_age_seconds,卡死行的年龄告警永远没有序列。
// 合法性由 assetop.NewLoop 校验;config.Validate 已按同形约束先拦一道。
func LoopConfigFrom(c config.AssetOpConf, retryBase time.Duration) assetop.LoopConfig {
	return assetop.LoopConfig{
		Interval:              msToDuration(c.ReconcileIntervalMs),
		Batch:                 c.ReconcileBatch,
		Workers:               c.Workers,
		Lease:                 msToDuration(c.LeaseMs),
		OpBudget:              msToDuration(c.OpBudgetMs),
		BaseBackoff:           retryBase,
		MaxBackoff:            msToDuration(c.MaxBackoffMs),
		AwaitDurableDelay:     assetop.DefaultLoopConfig().AwaitDurableDelay,
		PoisonDelay:           msToDuration(c.PoisonDelayMs),
		LedgerReadMinAttempts: uint32(c.LedgerReadMinAttempts),
		Streams:               GuildAssetStreams(),
	}
}

// CleanupConfFrom 把 AssetOp 的清理参数换算成 data.CleanupConf(§5.22)。
func CleanupConfFrom(c config.AssetOpConf) data.CleanupConf {
	const day = 24 * time.Hour
	return data.CleanupConf{
		Interval:          time.Duration(c.CleanupIntervalMinutes) * time.Minute,
		TerminalRetention: time.Duration(c.TerminalRetentionDays) * day,
		CounterRetention:  time.Duration(c.CounterRetentionDays) * day,
	}
}

func msToDuration(ms int) time.Duration {
	return time.Duration(ms) * time.Millisecond
}

// AssetPipeline 是资产通道在进程内的一整套运行件:scene 节点镜像(watcher)、连接缓存、
// 重投循环。Loop 同时供 logic 的同步投递使用(EconomyDeps.Loop)。
//
// 生命周期:NewAssetPipeline → Start → Stop。Stop 会等两个后台 goroutine 真正退出,
// **必须先于** etcd 客户端与 MySQL 的关闭(guild.go 的 defer 顺序)。
type AssetPipeline struct {
	// Loop 重投循环;构造后只读。
	Loop *assetop.Loop

	watcher *scenenode.Watcher
	conns   *scenenode.ConnCache

	// mu 只保护 started / stopped / cancel,让 Start 与 Stop 的先后无论怎么交错都不会
	// 在 Stop 的 wg.Wait 之后再 wg.Add(WaitGroup 不允许 Add 与 Wait 并发)。
	mu      sync.Mutex
	started bool
	stopped bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// NewAssetPipeline 建签名器、scene 定位件、Caller 与重投循环。只在 AssetOp.Enabled=true 时调用;
// 返回错误一律启动致命(密钥缺失 / 过短、循环参数非法、退避基数大于 MaxBackoffMs 等)。
//
// locatorRedis 读 player:{id}:location:必须与 scene / scene_manager 写这把键的是**同一实例同一 DB**
// (本地是 svcCtx.PlayerLocatorRedisClient;K8s 必须指向 SharedRedis),指错的表现是所有玩家
// "不在线"、资产指令永远 PENDING,且没有任何报错。
//
// 不连任何东西:ConnCache 惰性拨号,watcher 在 Start 里才连 etcd,Redis 在第一次定位时才读。
func NewAssetPipeline(c config.AssetOpConf, retryBase time.Duration, locatorRedis *redis.Client, store assetop.Store) (*AssetPipeline, error) {
	if store == nil {
		return nil, errors.New("guild: 资产管线缺 outbox 存储(GuildAssetStore)")
	}
	if locatorRedis == nil {
		return nil, errors.New("guild: 资产管线缺位置键 Redis(player:{id}:location)")
	}
	signer, err := NewAssetOpSigner()
	if err != nil {
		return nil, err
	}

	assetMetrics, nodeMetrics := assetChannelMetrics()
	conns := scenenode.NewConnCache()
	watcher := scenenode.NewWatcher(sceneWatcherName, scenenode.SceneNodeRpcPrefix, nodeMetrics.SetNodes,
		// 节点从 etcd 消失时丢掉它的连接:留着只会让下一次投递卡在一个已经没人听的地址上。
		func(e scenenode.NodeEntry) { conns.Remove(e.Endpoint) })
	locator := &scenenode.Locator{
		Reader:  scenenode.GoRedisReader{Client: locatorRedis},
		Watcher: watcher,
		Conns:   conns,
		Metrics: nodeMetrics,
	}
	caller := &assetop.Caller{
		Resolver: locator,
		Signer:   signer,
		// 单次资产 RPC 上限 800ms(= assetop.DefaultCallTimeout);投递侧实际拿 OpBudget − 700ms,
		// 装得下"一次调用 + 快路径重查",慢路径转 AwaitDurable 重排(assetop settleBudget 注释)。
		CallTimeout: assetop.DefaultCallTimeout,
		Requery:     assetop.DefaultRequery,
		Metrics:     assetMetrics,
	}
	loop, err := assetop.NewLoop(LoopConfigFrom(c, retryBase), store, caller, assetMetrics, time.Now)
	if err != nil {
		conns.Close()
		return nil, fmt.Errorf("guild: 资产重投循环参数非法(AssetOp 段 + GuildRule.asset_op_retry_base_ms): %w", err)
	}
	// Loop.Ledger 留 nil:读已落盘账本(data_service.GetPlayerAssetOpLedger)由 B5d 接(Y-06)。
	// Loop.Manual 不挂:人工终结 v1 只做离线 CLI(90 清单 D4),进程里没有调用者;
	// assetopfix 走包级 assetop.ResolveManually,不经 Loop。
	return &AssetPipeline{Loop: loop, watcher: watcher, conns: conns}, nil
}

// Start 起 scene 节点镜像与重投循环两个后台 goroutine。ctx 取消或 Stop 都会让两者退出;
// 重复调用、Stop 之后再调都是空操作;nil 接收者安全。
//
// 先起 watcher 再起循环:反过来也能工作(镜像没同步时定位回 node_unknown,循环按退避重来),只是白跑一轮。
// etcdCli 为 nil 时 watcher 打 ERROR 后直接返回,定位一律失败(fail-closed),循环照跑但行都留在 PENDING。
func (p *AssetPipeline) Start(ctx context.Context, etcdCli *clientv3.Client) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started || p.stopped {
		return
	}
	p.started = true
	runCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel

	p.wg.Add(2)
	// wg.Done 放在 fn 自己的 defer 里:safego 兜住 panic 时 defer 照样执行,Stop 不会因为
	// 一条 goroutine 炸掉而永远等下去。
	safego.Go("guild.scene_node_watch", func() {
		defer p.wg.Done()
		p.watcher.Run(runCtx, etcdCli)
	})
	safego.Go("guild.asset_op_reconcile", func() {
		defer p.wg.Done()
		p.Loop.Run(runCtx)
	})
	logx.Infof("[guild] 资产通道已启动: goroutine guild.scene_node_watch + guild.asset_op_reconcile(caller=%s)", AssetOpCaller)
}

// Stop 取消两个后台 goroutine、**等它们退出**,再关到各 scene 节点的连接。幂等;nil 接收者安全;
// 没 Start 过也可以调(只关连接缓存)。
//
// 为什么必须等:Loop 落库那一步用 context.WithoutCancel + 700ms(assetop settleContext),
// 取消 ctx 拦不住它。不等就去关 DB,终局写不回 outbox,行留在 PENDING、下一轮再投一次 ——
// 钱不丢(scene 对同一 seq 只读答复),但 Finalize 报错、白投一轮。等待时长有上界:
// Tick 等本批 worker 结束,每行最多 OpBudget(投递 + 落库),Store 的每条 SQL 都有自己的子预算。
//
// 在途的资产 RPC 被连接关闭打断不会丢数据:那一行仍是 PENDING,下次循环照常重投(不变量 I2)。
func (p *AssetPipeline) Stop() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	cancel := p.cancel
	p.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	p.wg.Wait()
	if p.conns != nil {
		p.conns.Close()
	}
	logx.Info("[guild] 资产通道已停止:重投循环与 scene 节点镜像均已退出")
}

// initAssetOpIDSegment 按 IdSegment 配置建 op_id 号段客户端(biz_tag = guild_asset_op),写法同
// initGuildIDSegment。必须在 initGuildIDSegment 之后调:"开了号段却没配 data_service"已在那里拒启。
//
// 号段关闭(或没有 data_service 客户端)时保持 nil:guild.go 据此把 EconomyDeps.OpIDs 留成 **nil 接口**,
// 捐献 / 兑换回 kGuildIdGenUnavailable。没有 snowflake 回退 —— op_id 是 outbox 主键,自造 id 会撞主键。
// AssetOp.Enabled=true 时 config.Validate 已要求号段开启,走不到 nil 分支。
func (s *ServiceContext) initAssetOpIDSegment() {
	cfg := s.Config.IdSegment
	if !cfg.Enabled || s.DataServiceClient == nil {
		logx.Infof("[id-segment] %s minting: disabled (IdSegment.Enabled=%v, data_service client configured=%v); "+
			"guild donate / shop requests cannot mint op ids", AssetOpBizTag, cfg.Enabled, s.DataServiceClient != nil)
		return
	}
	seg, err := idsegment.New(
		idsegment.Adapt(s.DataServiceClient.AllocateIdSegment,
			func(bizTag string, step uint32) *dspb.AllocateIdSegmentRequest {
				return &dspb.AllocateIdSegmentRequest{BizTag: bizTag, Step: step}
			}),
		// 与 guild_id 共用同一份 Step / MinStep / MaxStep:初值之后客户端按消耗速度在界内自适应,
		// 捐献比建帮频繁得多,会自己走到较大的 step。
		idsegment.Options{
			BizTag:  AssetOpBizTag,
			Step:    cfg.StepOrDefault(),
			MinStep: cfg.MinStepOrDefault(),
			MaxStep: cfg.MaxStepOrDefault(),
		},
	)
	if err != nil {
		panic(fmt.Errorf("guild asset op id segment client: %w", err))
	}
	s.AssetOpIDSegment = seg
	logx.Infof("[id-segment] %s minting: segment initial_step=%d step_bounds=[%d, %d] (no snowflake fallback)",
		AssetOpBizTag, cfg.StepOrDefault(), cfg.MinStepOrDefault(), cfg.MaxStepOrDefault())
}

// WarmAssetOpIDSegment 启动时同步领第一段 op_id;失败只告警(之后每次 Next 会自己再试)。
// 目的同 WarmGuildIDSegment:把"data_service 的 BootstrapTags 漏登记 guild_asset_op"尽早暴露在启动日志里。
func (s *ServiceContext) WarmAssetOpIDSegment() {
	if s.AssetOpIDSegment == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), assetOpIDWarmTimeout)
	defer cancel()
	if err := s.AssetOpIDSegment.Warm(ctx); err != nil {
		logx.Errorf("[id-segment] %s first segment not available yet (data_service.AllocateIdSegment): %v "+
			"— donate / shop fail with kGuildIdGenUnavailable until data_service answers; "+
			"in production also confirm %s is in data_service BootstrapTags", AssetOpBizTag, err, AssetOpBizTag)
	}
}

// ---------------------------------------------------------------------------
// 指标
// ---------------------------------------------------------------------------

// assetop / scenenode 的指标句柄在包级建一次:两者都用 promauto,重复构造会因指标重名 panic,
// 而单测可能多次调 NewAssetPipeline。service label 固定为 "guild"。
var (
	assetChannelMetricsOnce sync.Once
	assetOpMetrics          *assetop.Metrics
	sceneNodeMetrics        *scenenode.Metrics
)

func assetChannelMetrics() (*assetop.Metrics, *scenenode.Metrics) {
	assetChannelMetricsOnce.Do(func() {
		assetOpMetrics = assetop.NewMetrics(prometheus.DefaultRegisterer, AssetOpCaller)
		sceneNodeMetrics = scenenode.NewMetrics(prometheus.DefaultRegisterer, AssetOpCaller)
	})
	return assetOpMetrics, sceneNodeMetrics
}
