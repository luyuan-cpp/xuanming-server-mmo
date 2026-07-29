package svc

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"

	"github.com/bwmarrin/snowflake"
	"github.com/panjf2000/ants/v2"
	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/zrpc"
	"google.golang.org/protobuf/proto"

	game "login/generated/pb/game"
	"login/internal/config"
	"login/internal/dispatcher"
	"login/internal/kafka"
	"login/internal/logic/pkg/loginqueue"
	"login/internal/logic/pkg/node"
	"login/internal/logic/pkg/token"
	login_proto "proto/common/base"
	kafkapb "proto/contracts/kafka"
	plpb "proto/player_locator"
	smpb "proto/scene_manager"
	"shared/generated/table"
	"time"
)

type ServiceContext struct {
	RedisClient         *redis.Client
	SnowFlake           *snowflake.Node
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
		redisClient,
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
	return sc
}

// QueueHmacSecret returns the secret used to sign opaque queue tokens.
// We deliberately reuse GateTokenSecret: the queue token has a different
// wire shape (base64(JSON|.|hmac)) than the gate token (proto-marshalled
// GateTokenPayload), so cross-format confusion attacks aren't possible,
// and rotating one secret rotates both — simpler operational model.
func (s *ServiceContext) QueueHmacSecret() []byte {
	return []byte(config.AppConfig.GateTokenSecret)
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
}

func (g *gateWatcherCapacityProvider) CandidatesForZone(ctx context.Context, zoneID uint32) ([]loginqueue.GateCandidate, error) {
	nodes, err := g.watcher.FetchAllNodes()
	if err != nil {
		return nil, err
	}
	out := make([]loginqueue.GateCandidate, 0, len(nodes))
	for _, n := range nodes {
		if n.Endpoint == nil {
			continue
		}
		if zoneID != 0 && n.ZoneId != zoneID {
			continue
		}
		out = append(out, loginqueue.GateCandidate{
			NodeID:      n.NodeId,
			IP:          n.Endpoint.Ip,
			Port:        n.Endpoint.Port,
			PlayerCount: n.PlayerCount,
			ZoneID:      n.ZoneId,
		})
	}

	// 剔除正在排空的 gate。放在这里而不是 PickGate 里,是因为这是**所有**
	// gate 选择路径的唯一收口(队列 dispatcher 与非队列快路径都经过它),
	// 而 PickGate 是个纯函数、拿不到 Redis。
	//
	// 失败方向是放行不是拦截,全部被标记时也会放行 —— 见 FilterDrainingGates。
	if g.rdb != nil {
		out = loginqueue.FilterDrainingGates(out, loginqueue.DrainingGates(ctx, g.rdb, out))
	}
	return out, nil
}

func (g *gateWatcherCapacityProvider) ZoneCapacity(zoneID uint32) uint32 {
	return loginqueue.ZoneCapacityFromMap(g.caps, zoneID)
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
		watcher: s.GateWatcher,
		caps:    cfg.ZoneCapacityOverride,
		rdb:     s.RedisClient,
	}

	if !cfg.Enabled {
		return
	}
	s.LoginQueue = loginqueue.New(
		s.RedisClient,
		cfg.QueueEntryTTL,
		cfg.AdmitTTL,
		s.QueueHmacSecret(),
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
		s.QueueHmacSecret(),
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
	if s.TaskResultDispatcher != nil {
		s.TaskResultDispatcher.Start()
	}
	if s.QueueDispatcher != nil {
		s.QueueDispatcher.Start()
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

	c.SnowFlake = node
}

// SendBindSessionToGate sends a BindSession command to the target Gate via Kafka.
// This replaces Centre's BindSessionToGate RPC.
func (s *ServiceContext) SendBindSessionToGate(gateID string, gateInstanceID string,
	sessionID uint32, playerID uint64, sessionVersion uint32, enterGsType uint32) error {

	eventId := uint32(game.ContractsKafkaBindSessionEventEventId)

	cmd := &kafkapb.GateCommand{
		SessionId:        sessionID,
		PlayerId:         playerID,
		TargetInstanceId: gateInstanceID,
		EventId:          eventId,
		EnterGsType:      enterGsType,
	}

	gateNodeID, err := strconv.ParseUint(gateID, 10, 32)
	if err != nil {
		return fmt.Errorf("parse gate id %q: %w", gateID, err)
	}
	cmd.TargetGateId = uint32(gateNodeID)

	data, err := proto.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal gate bind session command: %w", err)
	}

	topic := fmt.Sprintf("gate-%s", gateID)
	partitionKey := strconv.FormatUint(playerID, 10)
	if err := s.KafkaClient.SendToTopic(topic, data, partitionKey); err != nil {
		return fmt.Errorf("send gate bind session command to %s: %w", topic, err)
	}

	return nil
}

// KickSessionOnGate sends a KickPlayer command to the target Gate via Kafka.
func (s *ServiceContext) KickSessionOnGate(gateID string, gateInstanceID string, sessionID uint32, playerID uint64) error {
	eventId := uint32(game.ContractsKafkaKickPlayerEventEventId)

	cmd := &kafkapb.GateCommand{
		SessionId:        sessionID,
		PlayerId:         playerID,
		TargetInstanceId: gateInstanceID,
		EventId:          eventId,
	}

	gateNodeID, err := strconv.ParseUint(gateID, 10, 32)
	if err != nil {
		return fmt.Errorf("parse gate id %q: %w", gateID, err)
	}
	cmd.TargetGateId = uint32(gateNodeID)

	data, err := proto.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal gate kick command: %w", err)
	}

	topic := fmt.Sprintf("gate-%s", gateID)
	partitionKey := strconv.FormatUint(cmd.PlayerId, 10)
	if err := s.KafkaClient.SendToTopic(topic, data, partitionKey); err != nil {
		return fmt.Errorf("send gate kick command to %s: %w", topic, err)
	}

	return nil
}

func (s *ServiceContext) Stop() {
	s.ExpandMonitor.Stop()
	if s.preloadStatsStop != nil {
		close(s.preloadStatsStop)
		s.preloadStatsStop = nil
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
	go func() {
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
	}()
}
