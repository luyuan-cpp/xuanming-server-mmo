package svc

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync/atomic"

	"github.com/bwmarrin/snowflake"
	"github.com/panjf2000/ants/v2"
	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/zrpc"

	"login/internal/config"
	"login/internal/dispatcher"
	"login/internal/kafka"
	"login/internal/logic/pkg/homezone"
	"login/internal/logic/pkg/loginqueue"
	"login/internal/logic/pkg/node"
	"login/internal/logic/pkg/playernamereg"
	"login/internal/logic/pkg/token"
	login_proto "proto/common/base"
	dspb "proto/data_service"
	plpb "proto/player_locator"
	smpb "proto/scene_manager"
	"shared/clientendpoint"
	"shared/generated/table"
	"shared/idsegment"
	"shared/safego"
	"time"
)

type ServiceContext struct {
	RedisClient *redis.Client
	// SnowFlake 是 PlayerId 发号器。包了一层 fence 闸(见 player_id_gen.go):
	// 失去 worker id 所有权后必须一个号都发不出去,不能靠"停服"兜底。
	SnowFlake *PlayerIDGen
	// DataServiceClient 是 data_service 的 gRPC 客户端:PlayerId 号段、HomeZone
	// 映射查询、角色名登记(PlayerNames)共用。IdSegment.Enabled=false 且 DataServiceRpc 没配目标时为 nil。
	DataServiceClient dspb.DataServiceClient
	// PlayerIDSegment 是 PlayerId 号段客户端(shared/idsegment,biz_tag="player");
	// IdSegment.Enabled=false 时为 nil。
	PlayerIDSegment *idsegment.Client
	// PlayerIDMinter 是建角发号的**唯一**入口:号段优先,按 IdSegment 配置决定要不要
	// 回退到 SnowFlake。handler 不许绕过它直接碰 SnowFlake / PlayerIDSegment。
	PlayerIDMinter *idsegment.Minter
	// HomeZone 查 data_service 的 player:zone 映射(合服后的真归属)。Login 用它
	// 修正角色列表的 zone_id,EnterGame 用它决定 EnterScene.ZoneId。永不为 nil;
	// 没配 DataServiceRpc 时内部 Client 为 nil,所有查询按「不可用→保留存量」处理。
	HomeZone *homezone.Resolver
	// PlayerNames 是 data_service 角色名登记表(全服唯一)的客户端,与 HomeZone 复用同一条
	// DataServiceClient 连接。永不为 nil;没配 DataServiceRpc 时内部客户端为 nil,所有方法
	// 返回 playernamereg.ErrUnavailable —— CreatePlayer 据此拒绝建角(登记是唯一性的提交点,
	// fail-closed),角色列表 / EnterGame 的名字回源则按「查不到」放行(展示数据,fail-open)。
	PlayerNames         *playernamereg.Client
	NodeInfo            login_proto.NodeInfo
	KafkaClient         *kafka.KeyOrderedKafkaProducer
	ExpandMonitor       *kafka.ExpandMonitor
	PlayerLocatorClient plpb.PlayerLocatorClient
	SceneManagerClient  smpb.SceneManagerClient
	GateWatcher         *node.NodeWatcher
	TokenManager        *token.Manager

	// LoginQueue is the Redis ZSET-backed AssignGate queue. Nil when
	// Queue.Enabled=false in config — handlers MUST tolerate nil and fall
	// back to the legacy fast path. The dispatcher goroutine is owned by
	// this struct and started in Start() when LoginQueue != nil.
	LoginQueue       *loginqueue.Queue
	QueueDispatcher  *loginqueue.Dispatcher
	queueCapProvider loginqueue.CapacityProvider

	// PreloadPool runs background tasks (e.g. Kafka DB-preload) without spawning
	// an unbounded number of goroutines per login. Configured non-blocking so
	// Submit returns ErrPoolOverload immediately when full instead of stalling
	// the caller (which would defeat the whole point of going async).
	PreloadPool *ants.Pool

	// TaskResultDispatcher converts DB task-result delivery from BLPOP polling
	// into a callback registry driven by a single Redis Pub/Sub subscriber.
	// Hot paths register a callback for a taskID and return immediately.
	TaskResultDispatcher *dispatcher.TaskResultDispatcher

	// preloadSubmitted / preloadDropped track pool throughput for periodic
	// stats logging. Atomic-only (no mutex) so the hot path stays cheap.
	preloadSubmitted atomic.Uint64
	preloadDropped   atomic.Uint64

	// preloadStatsStop signals the stats logger goroutine to exit on shutdown.
	preloadStatsStop chan struct{}

	// gateDrainStop 取消 startGateDrainMonitor 起的 gate 排空判定循环;nil = 没起。
	gateDrainStop context.CancelFunc
}

func NewServiceContext() *ServiceContext {
	ctx := context.Background()

	table.LoadTables(config.AppConfig.TableDir, false)

	// Register auth providers based on config
	InitAuthProviders()

	// Initialize Redis client
	redisHost := config.AppConfig.Node.RedisClient.Host
	redisPassword := config.AppConfig.Node.RedisClient.Password
	redisDB := int(config.AppConfig.Node.RedisClient.DB)

	redisClient := redis.NewClient(&redis.Options{
		Addr:             redisHost,
		Password:         redisPassword,
		DB:               redisDB,
		DisableIndentity: true, // suppress CLIENT SETINFO on Redis < 7.2
	})

	if err := redisClient.Ping(ctx).Err(); err != nil {
		panic(fmt.Errorf("failed to connect Redis: %w", err))
	}

	kafkaClient, err := kafka.NewKeyOrderedKafkaProducer(config.AppConfig.Kafka)

	if err != nil {
		logx.Error(err)
		panic(err)
	}

	monitor, err := kafka.NewExpandMonitor(
		config.AppConfig.Kafka.Brokers, // Kafka broker addresses
		config.AppConfig.Kafka.Topic,   // Consumer group ID
		kafkaClient,
		1*time.Second,
	)

	if err != nil {
		logx.Error(err)
		panic(err)
	}

	// Initialize player_locator gRPC client (discovered via etcd)
	plConn := zrpc.MustNewClient(config.AppConfig.PlayerLocatorRpc)
	plClient := plpb.NewPlayerLocatorClient(plConn.Conn())

	// Initialize scene_manager gRPC client (discovered via etcd)
	logx.Infof("[config-debug] SceneManagerRpc.Timeout=%d, PlayerLocatorRpc.Timeout=%d, ServerTimeout=%d",
		config.AppConfig.SceneManagerRpc.Timeout, config.AppConfig.PlayerLocatorRpc.Timeout, config.AppConfig.Timeout)
	smConn := zrpc.MustNewClient(config.AppConfig.SceneManagerRpc)
	smClient := smpb.NewSceneManagerClient(smConn.Conn())

	// Initialize GateWatcher for gate node discovery (load balancing)
	gateNodeType := uint32(login_proto.ENodeType_GateNodeService)
	gatePrefix := node.BuildRpcPrefix(
		node.GetRpcPrefix(gateNodeType),
		config.AppConfig.Node.ZoneId,
		gateNodeType,
	)
	etcdClient, err := node.NewEtcdClient()
	if err != nil {
		panic(fmt.Errorf("failed to create etcd client for GateWatcher: %w", err))
	}
	gateWatcher := node.NewNodeWatcher(etcdClient, gatePrefix)

	// Initialize token manager for access/refresh tokens
	tokenMgr := token.NewManager(redisClient, token.Config{
		AccessTokenTTL:  config.AppConfig.TokenConfig.AccessTokenTTL,
		RefreshTokenTTL: config.AppConfig.TokenConfig.RefreshTokenTTL,
	})

	// Register access_token auth provider (requires TokenManager)
	RegisterAccessTokenProvider(tokenMgr)

	// Bounded background-task pool. Size is configurable; non-blocking + pre-alloc
	// keep Submit cost O(1) and fail fast under overload. Kafka SyncProducer is
	// internally serialized by a mutex, so a very large worker count buys nothing,
	// but a generous ceiling absorbs short bursts.
	poolSize := config.AppConfig.PreloadPool.Size
	if poolSize <= 0 {
		poolSize = 256
	}
	preloadPool, err := ants.NewPool(
		poolSize,
		ants.WithNonblocking(true),
		ants.WithPreAlloc(true),
		ants.WithPanicHandler(func(p any) {
			logx.Errorf("preload pool task panic: %v", p)
		}),
	)
	if err != nil {
		panic(fmt.Errorf("failed to create preload goroutine pool: %w", err))
	}

	sc := &ServiceContext{
		RedisClient:          redisClient,
		KafkaClient:          kafkaClient,
		ExpandMonitor:        monitor,
		PlayerLocatorClient:  plClient,
		SceneManagerClient:   smClient,
		GateWatcher:          gateWatcher,
		TokenManager:         tokenMgr,
		PreloadPool:          preloadPool,
		TaskResultDispatcher: dispatcher.NewTaskResultDispatcher(redisClient, 30*time.Second),
	}
	sc.initLoginQueue()
	sc.initPlayerIDMinter()
	sc.initHomeZoneResolver() // 依赖 initPlayerIDMinter 可能已拨好的 DataServiceClient
	sc.initPlayerNames()      // 依赖 initHomeZoneResolver 兜底拨好的 DataServiceClient
	return sc
}

// initPlayerNames 给 CreatePlayer / Login / EnterGame 装上角色名登记表的客户端。
//
// 必须在 initHomeZoneResolver 之后调用:号段关着(IdSegment.Enabled=false)时是它兜底拨的
// DataServiceClient,这里只复用同一条连接、不再拨号。DataServiceClient 为 nil 的后果
// (建角整体被拒)已由 initHomeZoneResolver 按 ERROR 报过,这里不重复刷。
//
// 顺带在启动期把 RoleNameRule 配表自检一次:规则行缺失 / 不合法时,每一次建角都会被
// fail-closed 拒绝(见 createplayerlogic.go 6a'),运维必须在起服日志里就看到,而不是等
// 玩家来报「建不了角」。只报 ERROR 不 panic,与 home zone 接线缺失同一档:登录、进游戏
// 不依赖这张表,不该因为它把整个 login 拉下来。
func (s *ServiceContext) initPlayerNames() {
	s.PlayerNames = playernamereg.New(s.DataServiceClient)
	rules, spec, attempts, err := playernamereg.Rules()
	if err != nil {
		logx.Errorf("[player-name] RoleNameRule config invalid: CreatePlayer will be REJECTED until the table is fixed: %v", err)
		return
	}
	logx.Infof("[player-name] registry client ready: name_len=[%d,%d] generated_prefix=%q suffix_len=%d max_generate_attempts=%d",
		rules.MinRunes, rules.MaxRunes, spec.Prefix, spec.SuffixLen, attempts)
}

// GateTokenSigningSecret 返回签发 gate 连接票据用的主密钥。
//
// 票据由 cpp gate 校验,login 只签不验,所以这里只需要主密钥;
// 轮换时 gate 侧要先认新旧两把,再由这边切主密钥。
func (s *ServiceContext) GateTokenSigningSecret() []byte {
	return activeSecrets().GateToken.Primary()
}

// QueueTokenSigningSecret 返回签发排队 token 的主密钥。
//
// **不再与 gate token 共用一把**:原来两者复用 GateTokenSecret
// (理由记在 docs/design/login-queue-2026-05.md:194),信任域没拆,
// 任一侧泄露另一侧同时失守。现在是独立的 Secrets.QueueToken。
func (s *ServiceContext) QueueTokenSigningSecret() []byte {
	return activeSecrets().QueueToken.Primary()
}

// QueueTokenVerifySecrets 返回校验排队 token 时要依次尝试的全部密钥:
// 主密钥在前,只验不签的旧密钥在后。轮换期新旧 token 都验得过靠的就是它。
func (s *ServiceContext) QueueTokenVerifySecrets() [][]byte {
	return activeSecrets().QueueToken.Candidates()
}

// activeSecrets 兜住「config.ActiveSecrets 还没被 main 赋值」的情况。
// 正常启动路径一定先 ResolveSecrets 再建 ServiceContext;这里返回空集合
// 而不是 panic,是为了让只构造 ServiceContext 的单测不被迫去搭密钥配置
// —— 空密钥集合下 Sign 返回 nil、Verify 恒 false,方向仍是 fail-closed。
func activeSecrets() *config.ResolvedSecrets {
	if config.ActiveSecrets != nil {
		return config.ActiveSecrets
	}
	return &config.ResolvedSecrets{
		GateToken:    &config.SecretSet{},
		QueueToken:   &config.SecretSet{},
		InternalAuth: &config.SecretSet{},
	}
}

// QueueCapacityProvider returns the CapacityProvider the AssignGate handler
// and the dispatcher both consume. It's lazily initialized in initLoginQueue
// so tests can override it (or skip queue setup entirely) without touching
// the GateWatcher.
func (s *ServiceContext) QueueCapacityProvider() loginqueue.CapacityProvider {
	return s.queueCapProvider
}

// gateWatcherCapacityProvider adapts NodeWatcher to the loginqueue.CapacityProvider
// interface. Filtering by zone happens here; sorting (least-loaded first) is the
// caller's job in PickAndSignGateToken so the dispatcher and the fast path agree.
type gateWatcherCapacityProvider struct {
	watcher *node.NodeWatcher
	caps    map[string]uint32 // zone_id (decimal string) → capacity ceiling
	// rdb 用来查 gate 排空标记。缩容前被标记 draining 的 gate 不再接新玩家。
	// nil 时跳过过滤(队列关闭 / 早期初始化路径)。
	rdb *redis.Client
	// requireClientEndpoint 取自配置 RequireClientEndpoint:true 时没自报客户端地址的 gate
	// 不进候选集(见 buildGateCandidates)。启动期定值,运行中不变。
	requireClientEndpoint bool
}

func (g *gateWatcherCapacityProvider) CandidatesForZone(ctx context.Context, zoneID uint32) ([]loginqueue.GateCandidate, error) {
	nodes, err := g.watcher.FetchAllNodes()
	if err != nil {
		return nil, err
	}
	return candidatesFromNodes(ctx, g.rdb, nodes, zoneID, g.requireClientEndpoint), nil
}

// candidatesFromNodes 是 CandidatesForZone 取完节点之后的全部逻辑。单独拆出来,是为了让
// 「先去重、后排空过滤」这条顺序有单测守住:CandidatesForZone 依赖具体的 etcd NodeWatcher,测不到。
//
// 选客户端地址 + 按地址去重必须在排空过滤之前:排空标记按 node_id 打在活着的那台上,
// 若先排空过滤,它被剔掉后,同地址上旧 node_id 的陈旧影子(没被标记)会顶上来被选中,
// 玩家连过去照样被新 gate 以 token_gate_node_mismatch 拒绝。
//
// 剔除正在排空的 gate 放在这里而不是 PickGate 里,是因为这是**所有** gate 选择路径的唯一收口
// (队列 dispatcher 与非队列快路径都经过它),而 PickGate 是个纯函数、拿不到 Redis。
// rdb 为 nil 时跳过排空过滤。失败方向是放行不是拦截,全部被标记时也会放行 —— 见 FilterDrainingGates。
func candidatesFromNodes(ctx context.Context, rdb *redis.Client, nodes []*login_proto.NodeInfo,
	zoneID uint32, require bool) []loginqueue.GateCandidate {
	out := buildGateCandidates(nodes, zoneID, require)
	if rdb != nil {
		out = loginqueue.FilterDrainingGates(out, loginqueue.DrainingGates(ctx, rdb, out))
	}
	return out
}

func (g *gateWatcherCapacityProvider) ZoneCapacity(zoneID uint32) uint32 {
	return loginqueue.ZoneCapacityFromMap(g.caps, zoneID)
}

// gateChoice 是 buildGateCandidates 的中间态:去重要用 launch_time,而 GateCandidate
// 刻意不带它(那是 loginqueue 的选择投影,不该为去重扩字段)。
type gateChoice struct {
	candidate  loginqueue.GateCandidate
	launchTime uint64
}

// buildGateCandidates 把 etcd 里的 gate NodeInfo 投影成下发候选。无 I/O,可直接单测;
// 副作用只有 shared/clientendpoint 的计数与本文件的节流 ERROR 日志。
//
// 顺序是契约(集群外入口 D78):
//
//  1. 丢掉没有 endpoint 的畸形记录与不属于 zoneID 的 gate(zoneID=0 表示不按 zone 过滤);
//  2. clientendpoint.Select 选客户端地址,选不出的 gate 跳过。GateCandidate.IP/Port 就是选中的
//     客户端地址:它们只下发给客户端,签票不绑地址(GateTokenPayload 只带 node_id / zone_id);
//  3. 按选中的地址 DedupeNewest,同一地址只留 launch_time 最大者,见 clientendpoint.DedupeNewest。
//
// 排空过滤不在这里,由调用方在本函数**之后**做(见 CandidatesForZone)。
// 输出保持 nodes 的相对顺序;最终挑哪台由 loginqueue.PickGate 按负载决定,与顺序无关。
func buildGateCandidates(nodes []*login_proto.NodeInfo, zoneID uint32, require bool) []loginqueue.GateCandidate {
	choices := make([]gateChoice, 0, len(nodes))
	for _, n := range nodes {
		if n == nil || n.Endpoint == nil {
			continue
		}
		if zoneID != 0 && n.ZoneId != zoneID {
			continue
		}
		host, port, ok := clientendpoint.Select(
			n.GetClientEndpoint().GetIp(), n.GetClientEndpoint().GetPort(),
			n.Endpoint.Ip, n.Endpoint.Port, require)
		if !ok {
			logGateSkippedWithoutClientEndpoint(n, require)
			continue
		}
		choices = append(choices, gateChoice{
			candidate: loginqueue.GateCandidate{
				NodeID:      n.NodeId,
				IP:          host,
				Port:        port,
				PlayerCount: n.PlayerCount,
				ZoneID:      n.ZoneId,
			},
			launchTime: n.LaunchTime,
		})
	}

	choices = clientendpoint.DedupeNewest(choices,
		func(c gateChoice) string {
			return net.JoinHostPort(c.candidate.IP, strconv.FormatUint(uint64(c.candidate.Port), 10))
		},
		func(c gateChoice) uint64 { return c.launchTime })

	out := make([]loginqueue.GateCandidate, 0, len(choices))
	for _, c := range choices {
		out = append(out, c.candidate)
	}
	return out
}

// skippedGateLogEvery 是「有 gate 因没有可下发的客户端地址被跳过」这条 ERROR 的最小间隔。
//
// CandidatesForZone 每次 AssignGate、dispatcher 每个 tick 都会跑,不节流就是按登录 QPS × gate 数
// 刷盘。持续性看 mmorpg_client_endpoint_select_total{result="rejected"},日志只负责告诉运维是哪台;
// 同一窗口内其余被跳过的 gate 不再逐条打印。
const skippedGateLogEvery = 30 * time.Second

var (
	// skippedGateLogClock 是节流计时的单调时钟基准:只用 time.Since 取差值,
	// 墙钟回拨不会把日志压住。
	skippedGateLogClock = time.Now()
	// skippedGateLogLast 是上一次打印时距基准的纳秒数;0 = 还没打过。CAS 保证并发下每个窗口只打一条。
	skippedGateLogLast atomic.Int64
)

func logGateSkippedWithoutClientEndpoint(n *login_proto.NodeInfo, require bool) {
	now := int64(time.Since(skippedGateLogClock)) + 1 // +1 让 0 专指「还没打过」
	last := skippedGateLogLast.Load()
	if last != 0 && now-last < int64(skippedGateLogEvery) {
		return
	}
	if !skippedGateLogLast.CompareAndSwap(last, now) {
		return
	}
	// require=true:gate 没自报 clientEndpoint(查它的 CLIENT_ENDPOINT_SOURCE / 版本);
	// require=false:连 endpoint 都不可用,是一条坏记录。
	logx.Errorf("[GateSelect] gate skipped: no client-reachable address "+
		"(node_id=%d zone=%d endpoint=%s:%d client_endpoint=%s:%d RequireClientEndpoint=%v); "+
		"further skips are not logged for %s, see mmorpg_client_endpoint_select_total{result=\"rejected\"}",
		n.GetNodeId(), n.GetZoneId(),
		n.GetEndpoint().GetIp(), n.GetEndpoint().GetPort(),
		n.GetClientEndpoint().GetIp(), n.GetClientEndpoint().GetPort(),
		require, skippedGateLogEvery)
}

// initLoginQueue is called from NewServiceContext when Queue.Enabled=true.
// Splitting it out keeps the no-queue path completely free of loginqueue
// imports and Redis writes (helpful for env-by-env rollout).
func (s *ServiceContext) initLoginQueue() {
	cfg := config.AppConfig.Queue

	// queueCapProvider is consumed by BOTH the queue path AND the fast path
	// (signFastPath in assigngatelogic.go), so it has to be wired up
	// regardless of cfg.Enabled. The original guard skipped it whenever the
	// queue was off, which made every AssignGate call to a Queue.Enabled=false
	// login crash on a nil-receiver method call inside CandidatesForZone.
	// Found during 3-zone × 15000 stress 2026-05.
	s.queueCapProvider = &gateWatcherCapacityProvider{
		watcher:               s.GateWatcher,
		caps:                  cfg.ZoneCapacityOverride,
		rdb:                   s.RedisClient,
		requireClientEndpoint: config.AppConfig.RequireClientEndpoint,
	}
	logx.Infof("[GateSelect] RequireClientEndpoint=%v (true: gates without clientEndpoint are never handed to clients)",
		config.AppConfig.RequireClientEndpoint)

	if !cfg.Enabled {
		return
	}
	s.LoginQueue = loginqueue.New(
		s.RedisClient,
		cfg.QueueEntryTTL,
		cfg.AdmitTTL,
		s.QueueTokenSigningSecret(),
	)

	// activeZonesProvider: union of (zones we have gates for) ∪ (zones with
	// non-empty queues). The latter handles the rare case of a zone whose
	// gates all crashed mid-drain — we still need to walk it so admitted
	// entries can clear via TTL even without dispatcher action.
	activeZones := func(ctx context.Context) []uint32 {
		seen := make(map[uint32]struct{})
		nodes, _ := s.GateWatcher.FetchAllNodes()
		for _, n := range nodes {
			if n.ZoneId != 0 {
				seen[n.ZoneId] = struct{}{}
			}
		}
		out := make([]uint32, 0, len(seen))
		for z := range seen {
			out = append(out, z)
		}
		return out
	}

	s.QueueDispatcher = loginqueue.NewDispatcher(
		s.LoginQueue,
		s.queueCapProvider,
		s.RedisClient,
		// dispatcher 这条参数是给 gate token 签名用的(它只挑 gate、
		// 真正签名在 consume 时由 handler 做),所以传 gate 密钥不是队列密钥。
		s.GateTokenSigningSecret(),
		10*time.Minute, // gateTokenTTL — must match assigngatelogic.gateTokenTTL (Round 19, R17 R2 收尾)
		cfg.DispatchInterval,
		cfg.SoftCapMultiplier,
		cfg.DispatcherLockTTL,
		cfg.DispatcherLockKey,
		activeZones,
	)
	// Identify this pod in the dispatcher_is_leader gauge so Grafana can
	// answer "which replica is leading right now". Hostname is the simplest
	// stable identifier across both bare-metal and K8s deployments; falling
	// back to the etcd-allocated NodeUuid would also work but isn't set
	// until SetNodeId() runs later in startup.
	if hostname, err := os.Hostname(); err == nil && hostname != "" {
		s.QueueDispatcher.SetPodID(hostname)
	}
}

func (s *ServiceContext) Start() {
	s.ExpandMonitor.Start()
	s.startPreloadStatsLogger()
	s.warmPlayerIDSegment()
	if s.TaskResultDispatcher != nil {
		s.TaskResultDispatcher.Start()
	}
	if s.QueueDispatcher != nil {
		s.QueueDispatcher.Start()
	}
	s.startGateDrainMonitor()
}

// startGateDrainMonitor 起 gate 排空判定循环(集群外入口 D87:标 draining → 等 drained → 删 Pod
// 的中间一步)。k8s_gate_drain.ps1 标完 gate:{id}:draining 后,靠它在在线掉到阈值或等到期限时
// 写 gate:{id}:drained,脚本见到标记才删 Pod。不起它,drained 永远不会出现,gate 滚动只能等脚本超时。
//
// 快照取 GateWatcher 的原始节点,**不能**复用 CandidatesForZone / buildGateCandidates:那条链会跳过
// 缺客户端地址的 gate、按地址去重、剔除 draining,而正在排空的 gate 恰恰是这里要看的。
//
// 每个 login 副本各跑一份,不抢 dispatcher 锁:一轮判定只按 draining 标记写 / 清 drained 标记,
// 幂等,多副本并发写的是同一个值。GateWatcher 按本 login 的 zone 前缀建,只判本 zone 的 gate。
func (s *ServiceContext) startGateDrainMonitor() {
	cfg := config.AppConfig.GateDrain
	if s.GateWatcher == nil || cfg.Interval <= 0 {
		logx.Errorf("[GateDrain] monitor NOT started (GateDrain.Interval=%s, gate watcher present=%v): "+
			"gate:{id}:drained will never be written and k8s_gate_drain.ps1 can only time out",
			cfg.Interval, s.GateWatcher != nil)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.gateDrainStop = cancel
	loginqueue.StartGateDrainMonitor(ctx, s.RedisClient,
		newGateDrainSnapshotFunc(s.GateWatcher.FetchAllNodes), gateDrainPolicy(cfg), cfg.Interval)
}

// newGateDrainSnapshotFunc 把「取 gate 原始节点」包成排空判定的快照函数。
// fetch 以函数注入而不是直接拿 *node.NodeWatcher,是为了让接线测试不依赖 etcd。
// 传进来的 ctx 不往下传:FetchAllNodes 自带 ServiceDiscoveryTimeout 上界。
func newGateDrainSnapshotFunc(fetch func() ([]*login_proto.NodeInfo, error)) loginqueue.GateSnapshotFunc {
	return func(context.Context) ([]loginqueue.GateOnline, error) {
		nodes, err := fetch()
		if err != nil {
			return nil, err
		}
		return gateDrainSnapshot(nodes), nil
	}
}

// gateDrainSnapshot 按 node_id 投影 gate 的在线数。除 nil 外一条不丢:陈旧记录、缺客户端地址、
// 正在排空的 gate 都要留下 —— 排空标记按 node_id 打,判定也按 node_id 做,
// 没在排空的那些由 EvaluateDrainingGates 顺手清掉残留的 drained 标记。
// etcd 键是 .../node_id/<id>,同一前缀下一个 node_id 只有一条,所以不再按 node_id 去重。
func gateDrainSnapshot(nodes []*login_proto.NodeInfo) []loginqueue.GateOnline {
	out := make([]loginqueue.GateOnline, 0, len(nodes))
	for _, n := range nodes {
		if n == nil {
			continue
		}
		out = append(out, loginqueue.GateOnline{NodeID: n.NodeId, PlayerCount: n.PlayerCount})
	}
	return out
}

// gateDrainPolicy 把配置换成 loginqueue 的判定参数。
// Deadline 向上取整到秒:不足 1s 的正数若截成 0,会被读成「永不因超时放行」,语义反了;
// <=0 一律按 0(永不因超时放行)处理 —— 负数不能被理解成「立刻放行」。
func gateDrainPolicy(cfg config.GateDrainConf) loginqueue.GateDrainPolicy {
	var deadlineSeconds int64
	if cfg.Deadline > 0 {
		deadlineSeconds = int64((cfg.Deadline + time.Second - 1) / time.Second)
	}
	return loginqueue.GateDrainPolicy{
		DrainedBelowPlayers: cfg.DrainedBelowPlayers,
		DeadlineSeconds:     deadlineSeconds,
	}
}

func (c *ServiceContext) SetNodeId(nodeId int64) {
	// Set package-level vars BEFORE creating the node — NewNode reads these at creation time.
	snowflake.Epoch = config.AppConfig.Snowflake.Epoch
	snowflake.NodeBits = uint8(config.AppConfig.Snowflake.NodeBits)
	snowflake.StepBits = uint8(config.AppConfig.Snowflake.StepBits)

	node, err := snowflake.NewNode(nodeId)
	if err != nil {
		logx.Errorf("Failed to create snowflake node (nodeId=%d): %v", nodeId, err)
		panic(fmt.Errorf("snowflake.NewNode(%d): %w", nodeId, err))
	}

	c.SnowFlake = NewPlayerIDGen(node)
}

// SendBindSessionToGate sends a BindSession command to the target Gate via Kafka.
// This replaces Centre's BindSessionToGate RPC.
//
// 寻址与两道 fail-closed 守卫都在 buildGateCommandMessage 里(gate_command.go),
// 这里只负责组业务字段并把算好的 (topic, partition) 原样交给生产者。
func (s *ServiceContext) SendBindSessionToGate(gateID string, gateInstanceID string,
	sessionID uint32, playerID uint64, sessionVersion uint32, enterGsType uint32) error {

	msg, err := buildBindSessionCommand(gateID, gateInstanceID, sessionID, playerID, enterGsType)
	if err != nil {
		return err
	}

	if err := s.KafkaClient.SendToTopicPartition(msg.Topic, msg.Partition, msg.Payload, msg.PartitionKey); err != nil {
		return fmt.Errorf("send gate bind session command to %s/%d: %w", msg.Topic, msg.Partition, err)
	}

	return nil
}

// KickSessionOnGate sends a KickPlayer command to the target Gate via Kafka.
func (s *ServiceContext) KickSessionOnGate(gateID string, gateInstanceID string, sessionID uint32, playerID uint64) error {
	msg, err := buildKickPlayerCommand(gateID, gateInstanceID, sessionID, playerID)
	if err != nil {
		return err
	}

	if err := s.KafkaClient.SendToTopicPartition(msg.Topic, msg.Partition, msg.Payload, msg.PartitionKey); err != nil {
		return fmt.Errorf("send gate kick command to %s/%d: %w", msg.Topic, msg.Partition, err)
	}

	return nil
}

// PushTipToSession 经 gate 给指定会话的客户端推一条 tip(SendTipToClient)。
//
// 这是 login 唯一的"面向客户端的异步通知"原语:EnterGame 的 gRPC 应答在提交预加载后就已
// 同步回了成功,之后异步链上的失败只能靠它告诉客户端(契约见 entergamelogic.go 的
// notifyEnterGameFailed)。它只送通知、不改任何状态 —— 踢线请用 KickSessionOnGate。
//
// 与 Bind / Kick 同一个生产者、同一条寻址收口(buildGateCommandMessage 的三道 fail-closed
// 守卫照样生效)。发送是同步的,时长上界由 sarama 生产者自身的超时与重试配置决定,这里不另加
// 重试:目标会话可能已经断开,重发没有意义。tipID 必须来自导表器生成的枚举,不许手写数字。
func (s *ServiceContext) PushTipToSession(gateID string, gateInstanceID string, sessionID uint32, playerID uint64, tipID uint32) error {
	msg, err := buildPushTipCommand(gateID, gateInstanceID, sessionID, playerID, tipID)
	if err != nil {
		return err
	}

	if err := s.KafkaClient.SendToTopicPartition(msg.Topic, msg.Partition, msg.Payload, msg.PartitionKey); err != nil {
		return fmt.Errorf("send gate push tip command to %s/%d: %w", msg.Topic, msg.Partition, err)
	}

	return nil
}

func (s *ServiceContext) Stop() {
	s.ExpandMonitor.Stop()
	if s.PlayerIDSegment != nil {
		s.PlayerIDSegment.Close()
	}
	if s.preloadStatsStop != nil {
		close(s.preloadStatsStop)
		s.preloadStatsStop = nil
	}
	if s.gateDrainStop != nil {
		s.gateDrainStop()
		s.gateDrainStop = nil
	}
	if s.TaskResultDispatcher != nil {
		s.TaskResultDispatcher.Stop()
	}
	if s.QueueDispatcher != nil {
		s.QueueDispatcher.Stop()
	}
	if s.PreloadPool != nil {
		s.PreloadPool.Release()
	}
}

// SubmitPreload submits a background task to the preload pool and updates
// throughput counters. Returns false if the pool is saturated (task dropped);
// callers should treat that as a soft failure (Scene-side retry compensates).
func (s *ServiceContext) SubmitPreload(task func()) bool {
	if err := s.PreloadPool.Submit(task); err != nil {
		s.preloadDropped.Add(1)
		return false
	}
	s.preloadSubmitted.Add(1)
	return true
}

// startPreloadStatsLogger emits a periodic snapshot of pool utilization and
// the cumulative submit/drop counters. Disabled when StatsInterval <= 0.
func (s *ServiceContext) startPreloadStatsLogger() {
	interval := config.AppConfig.PreloadPool.StatsInterval
	if interval <= 0 || s.PreloadPool == nil {
		return
	}
	s.preloadStatsStop = make(chan struct{})
	stop := s.preloadStatsStop
	safego.Go("login.preload_stats_logger", func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		var lastSubmitted, lastDropped uint64
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				submitted := s.preloadSubmitted.Load()
				dropped := s.preloadDropped.Load()
				dSubmitted := submitted - lastSubmitted
				dDropped := dropped - lastDropped
				lastSubmitted, lastDropped = submitted, dropped
				logx.Infof("[preload-pool] running=%d free=%d cap=%d submitted_total=%d dropped_total=%d submitted_delta=%d dropped_delta=%d",
					s.PreloadPool.Running(), s.PreloadPool.Free(), s.PreloadPool.Cap(),
					submitted, dropped, dSubmitted, dDropped)
			}
		}
	})
}
