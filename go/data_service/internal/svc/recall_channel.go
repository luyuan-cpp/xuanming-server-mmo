package svc

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"data_service/internal/config"
	"data_service/internal/metrics"
	"data_service/internal/store"

	assetpb "proto/common/asset"
	base "proto/common/base"
	componentpb "proto/common/component"
	rfpb "proto/role_fence"
	"shared/accountdirectory"
	"shared/assetop"
	"shared/assetopwake"
	"shared/authoritysql"
	"shared/clusterredis"
	"shared/dirshard"
	"shared/drainproof"
	"shared/etcdwatch"
	"shared/noderegistry"
	"shared/playerredis"
	"shared/rolefence"
	"shared/safego"
	"shared/scenenode"
	"shared/workshard"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// GM 批量回收的资产通道(docs/design/exploit_loss_prevention_zh.md)。
//
// 和 trade、guild 同一套:outbox 行由 assetop.Loop 重投,玩家进场由 assetop:wake 立刻投递。多的只有派发:
// 计划冻结时不分配 seq,后台逐行持护栏锁分配,这样一批上万行不用在一个 RPC 里做完。
const (
	// RecallAssetCaller 是资产 RPC 白名单里的名字,scene 的 GM_RECALL 流只认它。
	RecallAssetCaller = "data_service"
	// RecallAssetSecretEnv 是签名密钥的环境变量,和 scene 同名同值。不做 dev 放行,本地由 tools/scripts/lib/assetop_dev_secret.ps1 注入。
	RecallAssetSecretEnv = "MMORPG_ASSET_OP_SECRET_DATA_SERVICE"
	// recallOpBizTag 是 op_id 的号段键,要登记进 IdSegment.Tags(三处,见 config.DefaultIdSegmentTags)。
	recallOpBizTag = "gm_recall_op"

	recallStoreProbeTimeout  = 5 * time.Second
	recallRedisPingTimeout   = 5 * time.Second
	recallEtcdDialTimeout    = 5 * time.Second
	recallWatcherName        = "scene"
	recallWatcherPoint       = "data_service.recall.watcher"
	recallMembersPoint       = "data_service.recall.members"
	recallLoopPoint          = "data_service.recall.loop"
	recallDispatchPoint      = "data_service.recall.dispatch"
	recallDispatchInterval   = time.Second
	recallDispatchBatch      = 64
	recallDispatchRoundLimit = 30 * time.Second
	// recallDispatchDeferMs 是派不了的行多久之后再看:角色冻结、流积压都不是秒级能变的,不值得每秒问一遍。
	recallDispatchDeferMs = 30_000

	// recallAssetOpWorkKind 是回收重投按片分工的归属键前缀:片号 = player_id % 64(缺口清单第 25 条,
	// docs/design/assetop-redelivery-sharding-20261110.md §10)。与别的服务的类别键都不同名,写死成常量,不进配置。
	recallAssetOpWorkKind = "gm-recall-assetop"
	// recallDispatchWorkKind 是派发(PLANNED → PENDING)按片分工的归属键前缀,片号同样是 player_id % 64(第四批,设计 §14.2)。
	// 不复用重投的类别键:派发只把行改成 PENDING,投递由重投那一片的负责者做,两件事不必落在同一个副本上。
	recallDispatchWorkKind = "gm-recall-dispatch"
)

// recallDispatchStore 是派发一轮用到的存储能力。*store.RecallStore 满足它;单测注入假实现,不连库。
type recallDispatchStore interface {
	// ListPlannedOwned 读 owned 那些片里到期的 PLANNED 行,条件在 LIMIT 之前;零值 owned 不过滤。
	ListPlannedOwned(ctx context.Context, nowMs uint64, limit int, owned workshard.Filter) ([]store.RecallPlanned, error)
	Dispatch(ctx context.Context, op store.RecallPlanned, nowMs uint64) (store.RecallDispatchResult, error)
	DeferPlanned(ctx context.Context, opID, until, nowMs uint64) error
}

// recallShardMembersPrefix 是回收重投成员表圈住的 etcd 前缀:本 zone 注册的 data_service 实例
// (DataServiceNodeService.rpc/zone/<z>/,与 internal/noderegistry 注册时的路径同形)。
//
// 为什么只圈本 zone、不圈全服(设计 §10.2):gm_recall_op 在流水库 <SnapshotMySQL.DBName>_ledger,K8s 上它是全集群一份,
// 各 zone 的 data_service 都连它;但 data_service 随 zone 部署(每个 zone namespace 一个 Deployment、一份 ConfigMap),
// Recall.Enabled 与库名都是各 zone 自己的配置。同一 zone 前缀下的副本出自同一份配置,圈得住「连同一个库、都在跑重投循环」;
// 圈全服就要假定所有 zone 的配置一致,哪个 zone 关了回收或连了别的库,落在它名下的片就没人做。按 zone 圈,
// 每一片由每个 zone 各一个副本扫(Z 个 zone 就是 Z 份),比分片前的「全部副本 × 全部行」少,且不会漏片。
func recallShardMembersPrefix(zoneID uint32) string {
	return noderegistry.RpcZonePrefix(base.ENodeType_name[int32(base.ENodeType_DataServiceNodeService)]+".rpc", zoneID)
}

// RecallStore 是 logic 依赖的回收存储能力。*store.RecallStore 满足它,单测注入假实现。
type RecallStore interface {
	Close() error
	ReadBatch(ctx context.Context, requestID uint64) (*store.RecallBatchRow, error)
	ListOps(ctx context.Context, requestID uint64) ([]*store.RecallOpRow, error)
	ConsumedSources(ctx context.Context, txIDs []uint64) (map[uint64]bool, error)
	CreateBatch(ctx context.Context, batch *store.RecallBatchRow, ops []*store.RecallOpPlan, nowMs uint64) error
	CancelBatch(ctx context.Context, requestID uint64, operator, reason string, nowMs uint64) error
	// ReadOpState、ResolveManuallyIfQuiet 是人工终结的两步:读行核证据,再带读到的租约与发送时刻终结。
	ReadOpState(ctx context.Context, opID uint64) (store.RecallOpState, error)
	ResolveManuallyIfQuiet(ctx context.Context, r assetop.ManualResolution, witness assetop.QuietWitness, nowMs uint64) (bool, error)
	// SourceState、RegisterReviewHandled 是 needs_review 指令交人工处置的登记:看这些流水登记过没有,再在一个事务里按已消费登记。
	SourceState(ctx context.Context, txIDs []uint64) (store.RecallSourceState, error)
	RegisterReviewHandled(ctx context.Context, h store.RecallReviewHandled, nowMs uint64) (bool, error)
}

// recallLoopConfig 是回收重投循环的配置。人工终结判「安静」用的租约也从这里取(RecallLeaseDuration),不各写一份。
func recallLoopConfig() assetop.LoopConfig {
	loopConfig := assetop.DefaultLoopConfig()
	// 不开自动放弃:放弃等于玩家躲够时间就把赃物带回来。不开日对账:按类 gauge 要选主,回收量小,最老年龄告警够用。
	loopConfig.AbandonAfter = 0
	loopConfig.AuditInterval = 0
	loopConfig.Streams = []assetpb.AssetOpStream{assetpb.AssetOpStream_ASSET_OP_STREAM_GM_RECALL}
	return loopConfig
}

// RecallLeaseDuration 是回收重投循环每领一行持有的租约。ResolveRecallOp 按它和 assetop.ManualQuietFloor 取较大者
// 算「安静」间隔(assetop.ManualQuietGap):距最后一次可能的投递超过这个间隔,才能判一行「从未应用」。
func RecallLeaseDuration() time.Duration {
	return recallLoopConfig().Lease
}

// configureRecall 装配回收存储和资产通道。存储总要有:护栏要靠它判挂起,关掉 Enabled 也不能让未结束的行失去保护。
// 存储起不来只记日志、保持缺席:MissingStores 带上 "recall",Store.Required=true 时 main 拒启。
// 通道只在 Enabled 时装配,这时依赖有问题直接拒启:资产路径没有降级。
func (s *ServiceContext) configureRecall(c config.Config, mysqlCfg store.MySQLConfig, directory *store.AccountDirectoryStore, getenv func(string) string) {
	if len(s.rowIDSegments) != 2 {
		logx.Errorf("[BatchRecall] audit row ids unavailable; recall store not assembled")
		return
	}
	opIDs, err := newRowIDSegment(s.IdSegmentStore, recallOpBizTag)
	if err != nil {
		logx.Errorf("[BatchRecall] op id segment init failed; recall store not assembled: %v", err)
		return
	}
	s.rowIDSegments = append(s.rowIDSegments, opIDs)
	ledgerCfg, err := mysqlCfg.ForSchema(dirshard.LedgerDBName(mysqlCfg.DBName))
	if err != nil {
		logx.Errorf("[BatchRecall] ledger database unavailable; recall store not assembled: %v", err)
		return
	}
	probe, cancel := context.WithTimeout(context.Background(), recallStoreProbeTimeout)
	defer cancel()
	recall, err := store.NewRecallStore(probe, ledgerCfg)
	if err != nil {
		logx.Errorf("[BatchRecall] recall store init failed: %v", err)
		return
	}
	if err := recall.Configure(s.rowIDSegments[1], recallFenceReader{states: directory}); err != nil {
		logx.Errorf("[BatchRecall] recall store wiring failed: %v", err)
		if closeErr := recall.Close(); closeErr != nil {
			logx.Errorf("[BatchRecall] close recall store: %v", closeErr)
		}
		return
	}
	s.Recall = recall
	s.recallStore = recall
	s.RecallOpIDs = opIDs
	if !c.Recall.Enabled {
		logx.Info("[BatchRecall] Recall.Enabled=false: dry run works, execution answers ErrCodeRecallDisabled, pending rows stay as they are")
		return
	}
	channel, err := newRecallChannel(c, recall, directory, s.RelayAdmission, getenv)
	if err != nil {
		panic(fmt.Sprintf("Recall.Enabled=true but the asset channel cannot be assembled: %v", err))
	}
	s.recall = channel
	s.RecallReady = true
}

// recallFenceParticipant 是流水库的角色护栏参与者。没有回收存储就接不上,协调者据此不装配。
func (s *ServiceContext) recallFenceParticipant(secret string) (rfpb.RoleFenceParticipantServer, error) {
	if s.recallStore == nil {
		return nil, errors.New("recall store is not assembled")
	}
	return rolefence.NewParticipantServer(s.recallStore.DB(), s.recallStore.PendingForRole, secret)
}

// recallFenceReader 让流水库的副本直接读本进程的目录,不绕 RPC。
type recallFenceReader struct {
	states RoleFenceStateReader
}

func (r recallFenceReader) GetRoleFenceState(ctx context.Context, player uint64) (*rfpb.RoleFenceState, error) {
	state, err := r.states.RoleFenceState(ctx, player)
	switch {
	case err == nil:
		return state, nil
	case errors.Is(err, accountdirectory.ErrMissing):
		return nil, rolefence.ErrRoleNotFound
	default:
		return nil, errors.Join(rolefence.ErrUnavailable, err)
	}
}

// recallLedger 让重投循环直接读本进程的存档账本,不绕 RPC。玩家没有写权行时回 (nil, nil),循环接着等。
type recallLedger struct {
	assets PlayerStoreAssetReader
}

func (l recallLedger) ReadPersistedLedger(ctx context.Context, playerID uint64) (*componentpb.PlayerAssetOpLedgerComp, error) {
	row, err := l.assets.ReadPlayerStoreAsset(ctx, playerID)
	if err != nil || row == nil {
		return nil, err
	}
	if ledger := row.Player.GetAssetOpLedger(); ledger != nil {
		return ledger, nil
	}
	return &componentpb.PlayerAssetOpLedgerComp{}, nil
}

func (l recallLedger) ReadSQLCommittedLedger(ctx context.Context, playerID uint64) (*componentpb.PlayerAssetOpLedgerComp, error) {
	return l.ReadPersistedLedger(ctx, playerID)
}

// RecallChannel 是回收的后台部分:节点镜像、重投循环、落地唤醒、派发。构造后除生命周期字段外只读。
type RecallChannel struct {
	// store 是派发读写的回收存储(生产是 *store.RecallStore,与重投循环同一个)。
	store       recallDispatchStore
	relay       *drainproof.Session
	loop        *assetop.Loop
	wake        *assetopwake.Subscriber
	watcher     *scenenode.Watcher
	conns       *scenenode.ConnCache
	redis       redis.UniversalClient // 控制集群句柄(ControlRedis 段):唤醒订阅与去重键
	playerRedis redis.UniversalClient
	etcd        *clientv3.Client
	// members 是重投循环按片分工的成员表(本 zone 注册的 stable data_service 实例,recallShardMembersPrefix)。
	// 构造时绑到 loop 上;本副本的身份在 etcd 注册之后由 ServiceContext.BindRecallWorkMember 补上。
	members *workshard.Membership
	// owns 判本副本是否负责派发的某个归属键(第四批,设计 §14.2)。生产是 members.Owns,与重投共用同一份成员表;
	// nil 表示不分工、派全部片(只有单测会这样造)。单测换成可控的判定,模拟成员表变化。
	owns func(key string) bool

	mu        sync.Mutex
	started   bool
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
}

var (
	recallMetricsOnce sync.Once
	recallAssetOps    *assetop.Metrics
	recallSceneNodes  *scenenode.Metrics
)

// recallChannelMetrics 只建一次:两者都用 promauto,重复构造会因指标重名 panic。
func recallChannelMetrics() (*assetop.Metrics, *scenenode.Metrics) {
	recallMetricsOnce.Do(func() {
		recallAssetOps = assetop.NewMetrics(prometheus.DefaultRegisterer, RecallAssetCaller)
		recallSceneNodes = scenenode.NewMetrics(prometheus.DefaultRegisterer, RecallAssetCaller)
	})
	return recallAssetOps, recallSceneNodes
}

func newRecallChannel(c config.Config, recall *store.RecallStore, assets PlayerStoreAssetReader, relay *drainproof.Session, getenv func(string) string) (_ *RecallChannel, err error) {
	if err := c.ValidateRecall(); err != nil {
		return nil, err
	}
	signer, err := assetop.NewSigner(RecallAssetCaller, getenv(RecallAssetSecretEnv))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", RecallAssetSecretEnv, err)
	}
	if len(c.Etcd.Hosts) == 0 {
		return nil, errors.New("Etcd.Hosts is required: scene nodes are discovered from etcd")
	}
	ch := &RecallChannel{store: recall, relay: relay}
	defer func() {
		if err != nil {
			ch.closeResources()
		}
	}()

	ping, cancel := context.WithTimeout(context.Background(), recallRedisPingTimeout)
	defer cancel()
	controlConf := c.EffectiveControlRedis()
	ch.redis, err = clusterredis.New(controlConf, clusterredis.ControlClusterKeys)
	if err != nil {
		return nil, fmt.Errorf("invalid ControlRedis config: %w", err)
	}
	if err = ch.redis.Ping(ping).Err(); err != nil {
		return nil, fmt.Errorf("connect control Redis (%s); landing wake lives there: %w", controlConf.Describe(), err)
	}
	playerConf := c.EffectivePlayerRedis()
	ch.playerRedis, err = playerredis.New(playerConf)
	if err != nil {
		return nil, fmt.Errorf("invalid PlayerRedis config: %w", err)
	}
	if err = ch.playerRedis.Ping(ping).Err(); err != nil {
		return nil, fmt.Errorf("connect player Redis (%s); player:{id}:location lives there: %w", playerConf.Describe(), err)
	}
	etcdConf := clientv3.Config{Endpoints: c.Etcd.Hosts, DialTimeout: recallEtcdDialTimeout}
	if c.Etcd.HasAccount() {
		etcdConf.Username = c.Etcd.User
		etcdConf.Password = c.Etcd.Pass
	}
	// keepalive:场景节点镜像的 watch 不会因为半开连接静默停住(etcdwatch 包注释)。
	ch.etcd, err = clientv3.New(etcdwatch.WithKeepAlive(etcdConf))
	if err != nil {
		return nil, fmt.Errorf("etcd client: %w", err)
	}

	assetMetrics, nodeMetrics := recallChannelMetrics()
	ch.conns = scenenode.NewConnCache()
	ch.watcher = scenenode.NewWatcher(recallWatcherName, scenenode.SceneNodeRpcPrefix, nodeMetrics.SetNodes,
		func(e scenenode.NodeEntry) { ch.conns.Remove(e.Endpoint) })
	locator := &scenenode.Locator{
		Reader:  scenenode.GoRedisReader{Client: ch.playerRedis},
		Watcher: ch.watcher,
		Conns:   ch.conns,
		Metrics: nodeMetrics,
	}
	caller := &assetop.Caller{
		Resolver:    locator,
		Signer:      signer,
		CallTimeout: assetop.DefaultCallTimeout,
		Requery:     assetop.DefaultRequery,
		Metrics:     assetMetrics,
	}

	ch.loop, err = assetop.NewLoop(recallLoopConfig(), recall, caller, assetMetrics, time.Now)
	if err != nil {
		return nil, err
	}
	ch.loop.Relay = relay
	ch.loop.Ledger = recallLedger{assets: assets}
	// 不挂 Manual:Loop.ResolveManually 不核证据。回收的人工终结只有 ResolveRecallOp 一个入口,先核已提交账本,
	// 再走带见证的结算(store.RecallStore.ResolveManuallyIfQuiet)。
	ch.loop.Rand = rand.Float64
	if err = ch.loop.CheckWiring(); err != nil {
		return nil, err
	}
	// 到期扫描按玩家分片(缺口清单第 25 条):每个副本只扫自己负责的片,不再都读同一个到期队头、在 Claim 上空撞。
	// 正确性仍由 Claim 的租约 CAS 与 Finalize 的 status CAS 保证。绑不上就拒启:绑了分工却扫全部行,扩副本时看不出来。
	ch.members = workshard.NewMembership(recallShardMembersPrefix(c.ZoneId))
	if err = ch.loop.BindWorkOwner(recallAssetOpWorkKind, ch.members.Owns); err != nil {
		return nil, fmt.Errorf("shard recall redelivery: %w", err)
	}
	// 派发也按玩家分片,用同一份成员表、另一个类别键(第四批,设计 §14.2)。
	ch.owns = ch.members.Owns
	ch.wake, err = assetopwake.New(ch.redis, assetopwake.Config{Service: RecallAssetCaller}, ch.loop, assetMetrics)
	if err != nil {
		return nil, err
	}
	return ch, nil
}

// Start 起节点镜像、重投循环、派发和落地唤醒。重复调用只生效一次。
func (r *RecallChannel) Start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return
	}
	r.started = true
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.goWithHold(recallWatcherPoint, func() { r.watcher.Run(ctx, r.etcd) })
	// 成员表先于重投循环起:第一拍读到之前成员表为空,本副本做全部片(保活优先)。
	r.goWithHold(recallMembersPoint, func() { r.members.Run(ctx, r.etcd) })
	r.goWithHold(recallLoopPoint, func() { r.loop.Run(ctx) })
	r.goWithHold(recallDispatchPoint, func() { r.runDispatch(ctx) })
	r.wake.Start(ctx)
	logx.Infof("[BatchRecall] asset channel started: due scan and dispatch sharded by player_id redelivery_kind=%s dispatch_kind=%s",
		recallAssetOpWorkKind, recallDispatchWorkKind)
}

// bindWorkMember 记下本副本注册 etcd 用的 node_uuid,重投循环从此按片分工。注册之前成员表若已有别的副本,本副本一片都不做。
func (r *RecallChannel) bindWorkMember(nodeUUID string) {
	r.members.SetSelf(nodeUUID)
}

func (r *RecallChannel) goWithHold(point string, run func()) {
	r.wg.Add(1)
	safego.Go(point, func() {
		defer r.wg.Done()
		defer func() {
			if recovered := recover(); recovered != nil {
				r.relay.Hold()
				panic(recovered)
			}
		}()
		run()
	})
}

// Stop 停后台并等它们退出,要排在 RELAY 排空之前:派发和重投的 SQL 写都在 RELAY 门禁内。等超时回错误,资源不关,由调用方 Hold。
func (r *RecallChannel) Stop(wait context.Context) error {
	r.mu.Lock()
	cancel := r.cancel
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	done := make(chan struct{})
	go func() {
		r.wake.Stop()
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-wait.Done():
		return wait.Err()
	}
	return r.closeResources()
}

func (r *RecallChannel) closeResources() error {
	r.closeOnce.Do(func() {
		if r.conns != nil {
			r.conns.Close()
		}
		if r.redis != nil {
			r.closeErr = errors.Join(r.closeErr, r.redis.Close())
		}
		if r.playerRedis != nil {
			r.closeErr = errors.Join(r.closeErr, r.playerRedis.Close())
		}
		if r.etcd != nil {
			r.closeErr = errors.Join(r.closeErr, r.etcd.Close())
		}
	})
	return r.closeErr
}

// runDispatch 每 recallDispatchInterval 派发一拍,直到 ctx 结束。
//
// 活性(data_service_recall_dispatch_last_run_unixtime,第四批,设计 §14.3;口径在 shared/looplive 的包注释):起来时写一次;
// 每一拍正常返回(成败都算)就写这一刻,panic 的一拍不写;ctx 结束、有意停止时删掉序列。值停住 = 循环卡在某一拍里,
// 或每一拍都 panic;分片之后它名下的片没人派,有告警 RecallDispatchLoopStalled。
func (r *RecallChannel) runDispatch(ctx context.Context) {
	ticker := time.NewTicker(recallDispatchInterval)
	defer ticker.Stop()
	metrics.RecallDispatchLive().Mark(time.Now())
	for {
		select {
		case <-ctx.Done():
			metrics.RecallDispatchLive().Stop()
			return
		case <-ticker.C:
			r.dispatchTick(ctx)
		}
	}
}

// dispatchTick 是派发循环的一拍,返回这一拍是否正常跑完。dispatchRound panic 时(它先打 Hold 再重新抛出)由 Round 里的 safego 兜住:
// 不写活性时刻,循环接着跑下一拍。
func (r *RecallChannel) dispatchTick(ctx context.Context) bool {
	return metrics.RecallDispatchLive().Round(recallDispatchPoint, func() { r.dispatchRound(ctx) })
}

// dispatchRound 派发一轮。每一轮走 RELAY 门禁。提交结局不明的失败打 Hold,闸门冻结不算:这一笔确定没写,下一轮接着做。
func (r *RecallChannel) dispatchRound(ctx context.Context) {
	finish, err := r.relay.Enter()
	if err != nil {
		return
	}
	defer finish()
	defer func() {
		if recovered := recover(); recovered != nil {
			r.relay.Hold()
			panic(recovered)
		}
	}()
	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), recallDispatchRoundLimit)
	defer cancel()

	nowMs := uint64(time.Now().UnixMilli())
	// 只派本副本负责的片(第四批,设计 §14.2):片号 = player_id % 64,归属每轮开头算一次,一轮里不变。
	// 正确性不靠分工:派不派得成仍由 Dispatch 在 role_fence 行锁下的状态 CAS 决定,两个副本扫到同一行只有一方派成。
	owned := workshard.SQLFilter(r.owns, recallDispatchWorkKind, "player_id")
	planned, err := r.store.ListPlannedOwned(work, nowMs, recallDispatchBatch, owned)
	if err != nil {
		// 纯读,没有结局未知,不打 Hold。Hold 不会自己清零,一次瞬断就把这个进程的排空证明打废了。
		metrics.ObserveRecallDispatchListError()
		logx.Errorf("[BatchRecall] list planned ops failed: %v", err)
		return
	}
	for _, op := range planned {
		if ctx.Err() != nil {
			return
		}
		result, err := r.store.Dispatch(work, op, nowMs)
		if err != nil {
			r.holdUnlessFrozen(err)
			metrics.ObserveRecallDispatch(metrics.RecallDispatchError)
			logx.Errorf("[BatchRecall] dispatch failed op_id=%d player=%d: %v", op.OpID, op.PlayerID, err)
			continue
		}
		switch result {
		case store.RecallDispatched:
			metrics.ObserveRecallDispatch(metrics.RecallDispatchSent)
		case store.RecallRoleGone:
			metrics.ObserveRecallDispatch(metrics.RecallDispatchRoleGone)
			logx.Errorf("[BatchRecall] role deleted or unknown, nothing to recall op_id=%d player=%d", op.OpID, op.PlayerID)
		case store.RecallLost:
			metrics.ObserveRecallDispatch(metrics.RecallDispatchLost)
		case store.RecallDeferred:
			metrics.ObserveRecallDispatch(metrics.RecallDispatchDeferred)
			if err := r.store.DeferPlanned(work, op.OpID, nowMs+recallDispatchDeferMs, nowMs); err != nil {
				r.holdUnlessFrozen(err)
				logx.Errorf("[BatchRecall] defer planned op failed op_id=%d: %v", op.OpID, err)
			}
		}
	}
}

func (r *RecallChannel) holdUnlessFrozen(err error) {
	if !authoritysql.IsWriterFrozen(err) {
		r.relay.Hold()
	}
}

// StartRecallChannel 起回收的后台。没装配时什么都不做。main 过了启动自检之后调用。
func (s *ServiceContext) StartRecallChannel() {
	if s.recall != nil {
		s.recall.Start()
	}
}

// BindRecallWorkMember 在 etcd 注册成功之后记下本副本的 node_uuid(缺口清单第 25 条:回收重投按片分工)。
// 回收通道没装配(Recall.Enabled=false)时是空操作。可在任意 goroutine 调用(成员表的身份是原子指针)。
func (s *ServiceContext) BindRecallWorkMember(nodeUUID string) {
	if s.recall != nil {
		s.recall.bindWorkMember(nodeUUID)
	}
}

// StopRecallChannel 停回收的后台并等它退出,排在 RELAY 排空之前。没起过时直接返回。
func (s *ServiceContext) StopRecallChannel(ctx context.Context) error {
	if s.recall == nil {
		return nil
	}
	return s.recall.Stop(ctx)
}

// 没有副作用的编译期核对:store 要满足 assetop 的可选能力,少一个唤醒和告警就静默失效。
var (
	_ assetop.Store               = (*store.RecallStore)(nil)
	_ assetop.PendingAgeReader    = (*store.RecallStore)(nil)
	_ assetop.PlayerPendingLister = (*store.RecallStore)(nil)
	_ assetop.OwnedDueLister      = (*store.RecallStore)(nil)
	_ RecallStore                 = (*store.RecallStore)(nil)
	_ recallDispatchStore         = (*store.RecallStore)(nil)
)
