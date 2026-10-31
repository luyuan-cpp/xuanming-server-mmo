package svc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"shared/drainproof"
	"sync"
	"time"

	"trade/internal/commerce"
	"trade/internal/config"
	"trade/internal/constants"
	"trade/internal/data"
	"trade/internal/reconcile"

	dspb "proto/data_service"

	"shared/assetop"
	"shared/assetopwake"
	"shared/idsegment"
	"shared/leader"
	"shared/playerredis"
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
	// **必须**登记进 data_service 的 IdSegment.Tags **三处**,否则发号被拒 = 一上架就失败:
	//   1. go/data_service/internal/config/config.go  DefaultIdSegmentTags
	//   2. go/data_service/etc/data_service.yaml      IdSegment.Tags
	//   3. tools/scripts/k8s_deploy.ps1               data-service ConfigMap 的同一行
	AssetOpIDBizTag = "trade_asset_op"

	// sceneWatcherName 是 etcd 节点镜像的名字,进 scenenode_nodes{kind} 这一个 label。
	sceneWatcherName = "scene"

	// sharedRedisPingTimeout 启动期探共享 Redis 的上限,与 mysqlPingTimeout 同值。
	sharedRedisPingTimeout = 5 * time.Second

	// assetOpIDWarmTimeout 启动时同步领第一段 op_id 的预算,与 listingIDWarmTimeout 同值。
	assetOpIDWarmTimeout = 5 * time.Second

	// persistedLedgerReadTimeout 离线读已落盘账本(data_service.GetPlayerAssetOpLedger)的单次上限,与 guild 同值
	// (docs/design/guild-phase2/07-rollback-fail-closed.md §7.8.3)。调用方传入的是投递预算的剩余部分,
	// 读账本只是"可能省一次卡死"的优化,不值得吃掉其中的大头;data_service 一次 Redis GET 远小于它。
	persistedLedgerReadTimeout = 300 * time.Millisecond

	// maintenanceLeaderKey 是"资产通道维护(日对账 + 自动放弃)"选主锁的键,放在 SharedRedis(全体副本共享),
	// 与 guild 的 guild:assetop:maintenance:leader 同一个命名风格。值是竞选者标识 + 随机串,排障时 GET 一眼看出谁在跑。
	maintenanceLeaderKey = "trade:assetop:maintenance:leader"

	// assetOpCleanupPoint 是终态行清理 goroutine 的 safego 点位名(safego_panic_total 的 label,必须是常量)。
	assetOpCleanupPoint = "trade.asset_op_cleanup"

	// 资产通道后台 goroutine 的 safego 点位名(同上,必须是常量;名字沿用拆分前的取值,告警与看板不用改)。
	sceneWatcherPoint       = "trade.scenenode.watcher"
	assetOpLoopPoint        = "trade.assetop.loop"
	assetOpMaintenancePoint = "trade.asset_op_maintenance"
)

// ErrAssetSecretMissing:没配 AssetOpSecretEnv,或密钥短于 32 字节。
// 这**不是**启动致命错:P1 的浏览 / 详情 / 收藏不碰资产,照常服务;但托管与交付整条路不构造,
// 任何上架尝试都会当场失败,而不是写进 outbox 之后永远投不出去。
var ErrAssetSecretMissing = errors.New("trade: 资产通道密钥未配置")

// AssetChannel 是资产通道的一整套依赖。Pipeline 为 nil 表示通道未启用(密钥缺失),
// 调用方必须判空:nil 管线上不存在"先写进 outbox 再说"的中间态。
type AssetChannel struct {
	RelayAdmission *drainproof.Session
	RootAdmission  *drainproof.Session
	// Redis 是控制域客户端(SharedRedis):落地唤醒的跨副本去重键 assetop:wake:trade:{player_id}(SET NX EX,shared/assetopwake),
	// 与维护选主锁 trade:assetop:maintenance:leader(shared/leader,值带属主校验)。唤醒订阅的 assetop:wake 频道也在这一实例上。
	Redis *redis.Client
	// PlayerRedis 是玩家域句柄(shared/playerredis):读位置键 player:{id}:location(scenenode.Locator)。
	PlayerRedis redis.UniversalClient
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
	Commerce *commerce.Engine
	Mall     *commerce.Mall
	Rmb      *commerce.RmbConsignments
	// Wake 落地唤醒订阅(shared/assetopwake,设计 §5.3):订 SharedRedis 上的 assetop:wake,
	// 把玩家落地交给 Pipeline.WakePlayer。AssetOp.Wake 显式关闭时为 nil(nil 接收者的 Start / Stop 安全)。
	Wake *assetopwake.Subscriber

	// maintenance 是"日对账 + 自动放弃"(Pipeline.RunMaintenance,设计 §6.2 / §6.3)的选主器;
	// 对账与放弃都关着时为 nil(不竞选,省一把锁)。构造后只读。
	maintenance *leader.Elector

	startOnce       sync.Once
	stopOnce        sync.Once
	stopLoops       context.CancelFunc
	stopDone        chan struct{}
	closeMu         sync.Mutex
	resourcesClosed bool
	closeErr        error

	// mu 只保护 closed 与 Start 里的 wg.Add:让 Start 与 Close 无论怎么交错都不会在 wg.Wait 之后再 wg.Add
	// (WaitGroup 不允许 Add 与 Wait 并发)。closed 之后 Start 是空操作。
	mu     sync.Mutex
	closed bool
	// wg 记 Start 起的全部后台 goroutine:节点镜像(Watcher.Run)、重投循环(Pipeline.Run)、维护(选主 + RunMaintenance)。
	// Close 必须等它们全部退出之后才关 Conns / Redis(与 guild 的 AssetPipeline.Stop 同序):
	//   - 重投循环每一行的落库走 assetop 不继承取消的落库预留,取消 ctx 拦不住它,投递还在用 Conns;
	//   - 放弃那一行的提交同样走落库预留,卸任时带属主校验释放选主锁要用 SharedRedis;
	//   - 节点镜像的回调会往 Conns 里删连接(节点下线时),关缓存之前它必须先停。
	wg sync.WaitGroup
}

// Enabled 报告资产通道是否可用(能签名 = 能真的发出资产 RPC)。
func (a *AssetChannel) Enabled() bool { return a != nil && a.Pipeline != nil }

// newAssetChannel 建整套依赖。返回错误 = 启动致命(共享 Redis 连不上、outbox 表名非法、
// op_id 号段建不出来);密钥缺失只降级,不返回错误。
func newAssetChannel(c config.Config, db *sql.DB, ds dspb.DataServiceClient, roles ...*drainproof.Session) (*AssetChannel, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:     c.SharedRedis.Host,
		Password: c.SharedRedis.Password,
		DB:       c.SharedRedis.DB,
		// 共享 Redis 连接总账按它记账(0 = go-redis 缺省 10 × GOMAXPROCS;负数已被 config.Validate 拒绝)。
		PoolSize: c.SharedRedis.PoolSize,
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
	// 生效值(缺省时是 go-redis 算出来的 10 × GOMAXPROCS):核对 ConfigMap 镜像的 PoolSize 是否真的被加载。
	logx.Infof("Shared Redis client %s DB=%d pool_size=%d", c.SharedRedis.Host, c.SharedRedis.DB, rdb.Options().PoolSize)

	playerConf := c.EffectivePlayerRedis()
	playerRdb, err := playerredis.New(playerConf)
	if err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("invalid PlayerRedis config: %w", err)
	}
	if err := playerRdb.Ping(pingCtx).Err(); err != nil {
		_ = playerRdb.Close()
		_ = rdb.Close()
		// 探不通就不起服,理由同上。Describe 不含密码。
		return nil, fmt.Errorf("connect player Redis (%s) failed; player:{id}:location lives there: %w", playerConf.Describe(), err)
	}
	logx.Infof("Player Redis client %s", playerConf.Describe())

	ops, err := data.NewAssetOpRepo(db, constants.StoreOpTimeout)
	if err != nil {
		_ = playerRdb.Close()
		_ = rdb.Close()
		return nil, err
	}

	opIDs, err := NewAssetOpIDSegment(c.IdSegment, ds, roles...)
	if err != nil {
		_ = playerRdb.Close()
		_ = rdb.Close()
		return nil, fmt.Errorf("op_id segment client: %w", err)
	}

	assetMetrics, nodeMetrics := assetChannelMetrics()
	conns := scenenode.NewConnCache()
	watcher := scenenode.NewWatcher(sceneWatcherName, scenenode.SceneNodeRpcPrefix, nodeMetrics.SetNodes,
		// 节点从 etcd 消失时丢掉它的连接:留着只会让下一次投递卡在一个已经没人听的地址上。
		func(e scenenode.NodeEntry) { conns.Remove(e.Endpoint) })
	locator := &scenenode.Locator{
		Reader:  scenenode.GoRedisReader{Client: playerRdb},
		Watcher: watcher,
		Conns:   conns,
		Metrics: nodeMetrics,
	}

	ch := &AssetChannel{
		Redis:       rdb,
		PlayerRedis: playerRdb,
		Conns:       conns,
		Watcher:     watcher,
		Locator:     locator,
		Ops:         ops,
		OpIDs:       opIDs,
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
		// 落地恢复(docs/design/landing-recovery-sync-digest.md §6.4,与 guild 同口径):
		//   - Ledger:在线 / 离线终结与自动放弃的证据来源,缺了 reconcile.New 拒绝构造;
		//   - Recovery:停放档 / 自动放弃 / 日对账的参数。
		// 这些都只能在这里(任何 goroutine 读 Loop 之前)交给 reconcile.New。
		Ledger:   persistedLedger(ds),
		Recovery: recoveryFrom(c.AssetOp),
	})
	if err != nil {
		// 循环常量写错,或落地恢复参数越过 assetop 的硬边界(config.Validate 已先拦一道),都必须炸在启动。
		ch.Close()
		return nil, err
	}
	ch.Pipeline = pipeline

	// 落地唤醒(设计 §5.3)。订的是 SharedRedis:scene 在玩家进场完成时向同一实例 PUBLISH assetop:wake <player_id>。
	// 实例指错的表现是永远收不到唤醒、只剩原退避,且不报错。
	// counter 必须是 Loop 用的同一个 assetop.Metrics:assetop_wake_total 在一个注册表里只能注册一次,
	// 订阅端与 Loop.WakePlayer 各管一段 result。去重键 assetop:wake:trade:{player_id},与 guild 互不去重。
	if c.AssetOp.Wake.On() {
		wake, err := assetopwake.New(rdb, wakeConfigFrom(c.AssetOp.Wake), pipeline, assetMetrics)
		if err != nil {
			ch.Close()
			return nil, fmt.Errorf("trade: 落地唤醒订阅参数非法(AssetOp.Wake): %w", err)
		}
		ch.Wake = wake
	} else {
		logx.Info("[trade] AssetOp.Wake.Enabled=false: landing wake not subscribed; pending rows of returning players " +
			"wait for their own backoff (up to OfflineBackoffMs when parked)")
	}

	// 日对账 + 自动放弃只在选主当选的一个副本上跑(设计 §6.2):对账只读、放弃靠 Claim CAS 与令牌互斥,
	// 多副本同时跑不会出错,但按类 gauge 会有多份、候选扫描会重复,告警 max(...) 的口径也会被搅乱。
	if pipeline.MaintenanceEnabled() {
		id := maintenanceLeaderID()
		ch.maintenance = leader.New(leader.NewGoRedisStore(rdb), maintenanceLeaderKey, leader.Options{
			ID: id,
			OnStateChange: func(leading bool) {
				if leading {
					logx.Infof("[trade] Asset channel maintenance leader acquired (pending audit + automatic abandon run on this replica) id=%s", id)
					return
				}
				logx.Infof("[trade] Asset channel maintenance leader released id=%s", id)
			},
		})
	} else {
		logx.Info("[trade] Asset channel maintenance disabled (AssetOp.AuditIntervalMs=0 and AssetOp.AbandonAfterDays=0): " +
			"no leader election, no pending audit, no automatic abandon")
	}
	return ch, nil
}

// persistedLedger 建已落盘账本读取器(assetop.DataServiceLedger)。
// 没有 data_service 客户端时回 **nil 接口**(不是装着 nil 指针的接口):reconcile.New 经 Loop.CheckWiring 拒绝构造,服务拒启。
//
// 交易合同(commerce)的收据 ACK 也用它读 SQL 证据。trade 的 NewServiceContext 无条件拨 data_service(号段也要它),
// 生产上走不到 nil 分支。
func persistedLedger(ds dspb.DataServiceClient) assetop.LedgerReader {
	if ds == nil {
		logx.Error("[trade] data_service client absent: the committed asset ledger cannot be read")
		return nil
	}
	return &assetop.DataServiceLedger{Client: ds, Timeout: persistedLedgerReadTimeout}
}

// recoveryFrom 把 AssetOp 的落地恢复参数换算成 reconcile.Recovery(停放档 / 自动放弃 / 日对账),
// 与 guild 的 svc.LoopConfigFrom 对应的那几项逐项同形;0 = 关闭的语义两边一致。
// 合法性由 assetop.NewLoop 校验;config.Validate 已在 Enabled 时按同形约束先拦一道。
func recoveryFrom(c config.AssetOpConf) reconcile.Recovery {
	return reconcile.Recovery{
		OfflineAfterAttempts: uint32(max(c.OfflineAfterAttempts, 0)),
		OfflineBackoff:       msToDuration(c.OfflineBackoffMs),
		AbandonAfter:         time.Duration(c.AbandonAfterDays) * 24 * time.Hour,
		AbandonSafetyGap:     msToDuration(c.AbandonSafetyGapMs),
		AbandonBatch:         c.AbandonBatch,
		AbandonInterval:      msToDuration(c.AbandonIntervalMs),
		AuditInterval:        msToDuration(c.AuditIntervalMs),
	}
}

// wakeConfigFrom 把 AssetOp.Wake 换算成 assetopwake.Config。并发与超时取生效值(没写 = assetopwake 默认);
// 去重窗口不开放配置,留 0 让 assetopwake 取 DefaultDedupTTL(同 guild)。Service 进去重键,取本服务的调用方名。
func wakeConfigFrom(w config.AssetOpWakeConf) assetopwake.Config {
	return assetopwake.Config{
		Service:     AssetOpCaller,
		Concurrency: w.EffectiveConcurrency(),
		WakeTimeout: w.EffectiveTimeout(),
	}
}

// cleanupConfFrom 把 AssetOp 的清理参数换算成 data.CleanupConf(配置名与 guild 相同)。
func cleanupConfFrom(c config.AssetOpConf) data.CleanupConf {
	return data.CleanupConf{
		Interval:          time.Duration(c.CleanupIntervalMinutes) * time.Minute,
		TerminalRetention: time.Duration(c.TerminalRetentionDays) * 24 * time.Hour,
	}
}

func msToDuration(ms int) time.Duration {
	return time.Duration(ms) * time.Millisecond
}

// maintenanceLeaderID 是维护选主的竞选者标识,只进锁值前缀与日志(leader.Options.ID)。
// trade 在注册 etcd 之前就要起维护(node_uuid 还没有),所以用主机名(K8s 下即 Pod 名);取不到时退回调用方名 ——
// 锁值里另有每次竞选新生成的随机串,标识重复不影响属主校验。
func maintenanceLeaderID() string {
	if host, err := os.Hostname(); err == nil && host != "" {
		return host
	}
	return AssetOpCaller
}

// StartAssetOpCleanup 起终态行清理 goroutine(trade.asset_op_cleanup,对齐 guild 的 startAssetOpCleanup),
// 返回的 stop 取消它并**等它退出**:清理语句跑在 MySQL 上,必须在 svcCtx.Stop 关库之前停下。
//
// 与 AssetOp.Enabled 独立(同 guild):通道关着时历史终态行照样要清,所以它自己建一个只用于清理的 AssetOpRepo
// (不拨共享 Redis、不建节点镜像)。只删保留期外的 APPLIED / REJECTED / ABORTED / ABANDONED,
// PENDING 与 APPLIED_PARTIAL 永不删(data.AssetOpRepo.CleanupTerminalOps)。调用方只在 AssetOp.CleanupEnabled 时调用,
// 且必须在建表(ensureSchema)之后。
func StartAssetOpCleanup(db *sql.DB, c config.AssetOpConf, roots ...*drainproof.Session) (stop func(), err error) {
	if db == nil {
		return nil, errors.New("trade: 终态资产指令清理缺 MySQL 连接池")
	}
	repo, err := data.NewAssetOpRepo(db, constants.StoreOpTimeout)
	if err != nil {
		return nil, fmt.Errorf("trade: 终态资产指令清理装配失败: %w", err)
	}
	conf := cleanupConfFrom(c)
	if len(roots) > 1 || (len(roots) == 1 && roots[0] == nil) {
		return nil, drainproof.ErrHold
	}
	if conf.Interval <= 0 || conf.TerminalRetention <= 0 {
		return nil, errors.New("trade cleanup 时间预算非法")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	safego.Go(assetOpCleanupPoint, func() {
		// close 放在 defer 里:safego 兜住 panic 时它照样执行,stop 不会永远等下去。
		defer close(done)
		if len(roots) == 0 {
			repo.RunCleanup(ctx, conf)
			return
		}
		owner := roots[0]
		defer func() {
			if recovered := recover(); recovered != nil {
				owner.Hold()
				panic(recovered)
			}
		}()
		timer := time.NewTimer(time.Duration(rand.Int64N(int64(conf.Interval))))
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			finish, enterErr := owner.Enter()
			if enterErr == nil {
				func() {
					defer finish()
					budget, release := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
					defer release()
					if _, err := repo.CleanupTerminalOps(budget, time.Now(), conf.TerminalRetention); err != nil {
						owner.Hold()
						logx.Errorf("trade cleanup 失败: %v", err)
					}
				}()
			} else if !errors.Is(enterErr, drainproof.ErrClosed) {
				owner.Hold()
				return
			}
			timer.Reset(conf.Interval)
		}
	})
	logx.Infof("[trade] Asset cleanup started: goroutine %s interval=%v terminal_retention=%v",
		assetOpCleanupPoint, conf.Interval, conf.TerminalRetention)
	return func() {
		cancel()
		<-done
	}, nil
}

// Start 起 etcd 节点镜像与重投循环,以及(装配了的话)维护选主与落地唤醒订阅。ctx 取消即全部退出;
// 重复调用只生效一次;Close 之后再调是空操作。
//
// 顺序:先起 watcher 再起循环。反过来也能工作(镜像没同步时 Resolve 回 node_unknown,
// 循环按退避重来),只是白跑一轮。唤醒订阅最后起:它调的 Loop.WakePlayer 与 Tick 共用 Workers 预算,
// 早起晚起都不改变任何正确性论证(设计 L-4)。
// 维护在当选时以领导权 ctx 跑 RunMaintenance(启动先对账一轮,之后按间隔对账 / 放弃),丢锁或 ctx 取消即返回、
// 带属主校验释放锁(leader.Elector.Run 的收尾,自带 2s 预算 —— 所以 Close 必须等它、且早于关 Redis)。
// 节点镜像、重投循环与维护三个 goroutine 都记进 wg(Close 等它们全部退出再关连接,与 guild 的 AssetPipeline 同形);
// wg.Add 在 mu 内、closed 判断之后,Close 置 closed 之后才 wg.Wait,所以不会出现 Wait 之后再 Add。
func (a *AssetChannel) Start(ctx context.Context, etcd *clientv3.Client) {
	if a == nil {
		return
	}
	a.startOnce.Do(func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.closed {
			return
		}
		loopCtx, cancel := context.WithCancel(ctx)
		a.stopLoops = cancel
		a.wg.Add(1)
		// wg.Done 一律放在 fn 自己的 defer 里:safego 兜住 panic 时 defer 照样执行,Close 不会永远等下去。
		safego.Go(sceneWatcherPoint, func() {
			defer a.wg.Done()
			defer a.holdPanic()
			a.Watcher.Run(loopCtx, etcd)
		})
		if a.Pipeline == nil {
			// 装配层已保证:AssetOp.Enabled=true 走到这里 Pipeline 必非 nil(密钥不可用会在
			// NewServiceContext 就拒启),Enabled=false 则整个 AssetChannel 是 nil、上面就返回了。
			// 留这条分支只为"将来有人绕开装配层直接构造"时不静默起半个循环。
			logx.Error("[trade] Asset channel wiring incomplete (Pipeline nil): starting only scene node mirror, no redelivery loop -- " +
				"this should not happen; NewServiceContext switch and key validation were bypassed")
			return
		}
		a.wg.Add(1)
		safego.Go(assetOpLoopPoint, func() {
			defer a.wg.Done()
			defer a.holdPanic()
			a.Pipeline.Run(loopCtx) // 阻塞到 loopCtx 结束、本轮 Tick 的 worker 全部落库完
		})
		if a.Commerce != nil {
			a.wg.Add(1)
			safego.Go("trade.commerce.reconcile", func() {
				defer a.wg.Done()
				defer a.holdPanic()
				ticker := time.NewTicker(time.Second)
				defer ticker.Stop()
				phase := 0
				for {
					select {
					case <-loopCtx.Done():
						return
					case <-ticker.C:
						finish := func() {}
						if a.RelayAdmission != nil {
							accepted, err := a.RelayAdmission.Enter()
							if err != nil {
								return
							}
							finish = accepted
						}
						func() {
							defer finish()
							budget, cancel := context.WithTimeout(context.WithoutCancel(loopCtx), 10*time.Second)
							defer cancel()
							// 各通道保留子预算并轮换起点，离线角色/商城故障不能吃光其它通道整个10秒窗口。
							for n := 0; n < 3; n++ {
								child, release := context.WithTimeout(budget, 3*time.Second)
								var err error
								switch (phase + n) % 3 {
								case 0:
									err = a.Commerce.Tick(child)
								case 1:
									if a.Mall != nil {
										if a.RootAdmission == nil {
											err = a.Mall.Tick(child)
										} else {
											finishSource, enterErr := a.RootAdmission.Enter()
											if enterErr == nil {
												func() { defer finishSource(); err = a.Mall.Tick(child) }()
											} else if !errors.Is(enterErr, drainproof.ErrClosed) {
												err = enterErr
											}
										}
									}
								case 2:
									if a.Rmb != nil {
										err = a.Rmb.Tick(child)
									}
								}
								release()
								if err != nil && a.RelayAdmission != nil {
									a.RelayAdmission.Hold()
								}
								if err != nil && loopCtx.Err() == nil {
									logx.Errorf("交易恢复通道失败 phase=%d: %v", (phase+n)%3, err)
								}
							}
							phase = (phase + 1) % 3
						}()
					}
				}
			})
		}
		if a.maintenance != nil {
			a.wg.Add(1)
			safego.Go(assetOpMaintenancePoint, func() {
				defer a.wg.Done()
				defer a.holdPanic()
				a.maintenance.Run(loopCtx, a.Pipeline.RunMaintenance)
			})
		}
		a.Wake.Start(loopCtx) // nil 接收者安全:Wake 关闭时什么都不做
		logx.Infof("[trade] Asset channel started: maintenance_election=%t landing_wake=%t",
			a.maintenance != nil, a.Wake != nil)
	})
}

// Close 停循环并关连接。幂等;调用方须保证它先于 etcd 客户端、MySQL 的关闭。
//
// 顺序与 guild 的 AssetPipeline.Stop 相同:取消 → 等落地唤醒订阅停下 → wg.Wait() 等节点镜像、重投循环、维护全部退出 →
// 关号段客户端 / 连接缓存 / 共享 Redis。全部后台 goroutine 都被**等到退出**才往下关:
//   - 在途唤醒用的是 Loop 与连接缓存,最坏再等 WakeTimeout 加落库预留(assetopwake.Subscriber.Stop);
//   - 重投循环要等本轮 Tick 的 worker 结束,每行最多 OpBudget(投递 + 不继承取消的落库预留),Store 的每条 SQL 另有子预算;
//   - 放弃那一行的提交同样走落库预留,维护卸任时带属主校验释放锁(≤ 2s)要用 SharedRedis;
//   - 节点镜像取消即退出(etcd watch 随 ctx 结束)。
//
// 在途的资产 RPC 被连接关闭打断不会丢数据:那一行仍是 PENDING,下次循环照常重投,
// scene 见过的 seq 只会把原结局再答一遍(不变量 I2)。
func (a *AssetChannel) holdPanic() {
	if recovered := recover(); recovered != nil {
		if a.RelayAdmission != nil {
			a.RelayAdmission.Hold()
		}
		if a.RootAdmission != nil {
			a.RootAdmission.Hold()
		}
		panic(recovered)
	}
}
func (a *AssetChannel) stopAndJoin() <-chan struct{} {
	a.stopOnce.Do(func() {
		a.mu.Lock()
		a.closed = true
		stop := a.stopLoops
		a.stopDone = make(chan struct{})
		a.mu.Unlock()
		if stop != nil {
			stop()
		}
		go func() { a.Wake.Stop(); a.wg.Wait(); close(a.stopDone) }()
	})
	return a.stopDone
}

// CloseAndReport 先停止发起并实际 join；确定持久 PENDING 可以留给新 boot，未返回 RPC 不能冒充结局。
func (a *AssetChannel) CloseAndReport(ctx context.Context) error {
	if a == nil {
		return nil
	}
	a.closeMu.Lock()
	defer a.closeMu.Unlock()
	if a.resourcesClosed {
		return a.closeErr
	}
	if a.RelayAdmission == nil {
		return drainproof.ErrHold
	}
	select {
	case <-ctx.Done():
		a.RelayAdmission.Hold()
		return errors.Join(drainproof.ErrHold, ctx.Err())
	case <-a.stopAndJoin():
	}
	if a.OpIDs != nil {
		digest, err := a.OpIDs.DrainAndReport(ctx)
		if err != nil {
			return err
		}
		if err = a.RelayAdmission.MergeSettled(digest); err != nil {
			return err
		}
	}
	if err := a.RelayAdmission.WaitIdle(ctx); err != nil {
		return err
	}
	if a.Conns != nil {
		a.Conns.Close()
	}
	a.closeRedis()
	a.resourcesClosed = true
	return a.closeErr
}

// closeRedis 关两个 Redis 客户端,错误记进 closeErr。
func (a *AssetChannel) closeRedis() {
	if a.Redis != nil {
		a.closeErr = a.Redis.Close()
	}
	if a.PlayerRedis != nil {
		a.closeErr = errors.Join(a.closeErr, a.PlayerRedis.Close())
	}
}
func (a *AssetChannel) Close() {
	if a == nil {
		return
	}
	if a.RelayAdmission != nil {
		if err := a.CloseAndReport(context.Background()); err != nil {
			logx.Errorf("trade asset owner HOLD: %v", err)
		}
		return
	}
	// 仅构造失败、尚未交付 service owner 的资源清理，不生成维护原件。
	a.closeMu.Lock()
	defer a.closeMu.Unlock()
	if a.resourcesClosed {
		return
	}
	<-a.stopAndJoin()
	if a.OpIDs != nil {
		a.OpIDs.Close()
	}
	if a.Conns != nil {
		a.Conns.Close()
	}
	a.closeRedis()
	a.resourcesClosed = true
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
		logx.Errorf("[trade] op_id segment temporarily unavailable (data_service.AllocateIdSegment biz_tag=%s): %v "+
			"-- listing escrow will fail with kServiceUnavailable until data_service responds; "+
			"in production also confirm %s is in data_service BootstrapTags", AssetOpIDBizTag, err, AssetOpIDBizTag)
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
func NewAssetOpIDSegment(cfg idsegment.Conf, client dspb.DataServiceClient, roles ...*drainproof.Session) (*idsegment.Client, error) {
	if !cfg.Enabled {
		return nil, errors.New("IdSegment.Enabled=false: op_id 只由号段发(biz_tag=" + AssetOpIDBizTag + ")")
	}
	if client == nil {
		return nil, errors.New("data_service client is nil: op_id 发号需要 data_service.AllocateIdSegment")
	}
	allocate := idsegment.Adapt(client.AllocateIdSegment, func(bizTag string, step uint32) *dspb.AllocateIdSegmentRequest {
		return &dspb.AllocateIdSegmentRequest{BizTag: bizTag, Step: step}
	})
	return idsegment.New(bindAllocationOwner(allocate, roles...),
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
