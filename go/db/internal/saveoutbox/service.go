package saveoutbox

import (
	"context"
	"errors"
	"sync"
	"time"

	dbconfig "db/internal/config"
	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
)

type Service struct {
	cancel  context.CancelFunc
	done    chan struct{}
	pub     *kafkaPublisher
	parent  *redis.Client
	mapping *redis.Client
	once    sync.Once
	replay  *Replay
	drainMu sync.Mutex
	stopErr error
}

// Start只接主parentRedis，不把只读OwnerEpochRedis误当可写父缓存实例。
// G4a拆分跨实例原子提交未解决前拒绝该拓扑；legacy不具备SQL双ACK，不能开启补投回收。
// health 为本 zone 主库的冷却判定(落点库注册表)，nil 表示不判冷却。
// route 为本实例 go/db 消费者的读方就绪闸(*kafka.KeyOrderedKafkaConsumer)，不得为 nil：补投与消费同看一把闸(r2-rr#5)。
func Start(ctx context.Context, app dbconfig.Config, parent, mapping *redis.Client, health StoreHealth, route RouteGate) (*Service, error) {
	if app.ServerConfig.Fencing.EffectiveMode() != dbconfig.FencingModeDual {
		logx.Infof("[SaveOutbox] legacy模式无SQL双ACK，持久存盘补投未启用")
		return nil, nil
	}
	mainRedis := app.ServerConfig.RedisClient
	if owner := app.ServerConfig.OwnerEpochRedis; owner != nil &&
		(owner.Hosts != mainRedis.Hosts || owner.DB != mainRedis.DB) {
		return nil, errors.New("save outbox严格保存要求owner与父缓存同一Redis实例，当前拆分拓扑未获支持")
	}
	if parent == nil || parent.Options().Addr != mainRedis.Hosts || parent.Options().DB != mainRedis.DB {
		return nil, errors.New("save outbox主父缓存Redis装配不匹配")
	}
	if mapping == nil {
		return nil, errors.New("save outbox权威home_zone Redis依赖缺失")
	}
	pub, err := newKafkaPublisher(app.ServerConfig.Kafka.Brokers, topicFor(app.ZoneId, app.ServerConfig.Kafka.TopicGeneration))
	if err != nil {
		return nil, err
	}
	// 不修改共享客户端Options；专用小池明确遵循每拍context截止，最多2~4条连接。
	parentReplay, mappingReplay := replayRedis(parent), (*redis.Client)(nil)
	if parent == mapping {
		mappingReplay = parentReplay
	} else {
		mappingReplay = replayRedis(mapping)
	}
	replay, err := New(parentReplay, mappingReplay, pub, health, route, Config{ZoneID: app.ZoneId, TopicGeneration: app.ServerConfig.Kafka.TopicGeneration}, nil)
	if err != nil {
		_ = parentReplay.Close()
		if mappingReplay != parentReplay {
			_ = mappingReplay.Close()
		}
		_ = pub.close()
		return nil, err
	}
	loopCtx, cancel := context.WithCancel(ctx)
	s := &Service{replay: replay, cancel: cancel, done: make(chan struct{}), pub: pub, parent: parentReplay, mapping: mappingReplay}
	go func() {
		defer close(s.done)
		replay.Run(loopCtx)
	}()
	return s, nil
}

func (s *Service) Stop() {
	if err := s.StopAndReport(context.Background()); err != nil {
		logx.Errorf("[SaveOutbox] 排空HOLD: %v", err)
	}
}

// StopAndReport先join普通/历史owner，再close等待writer内部pending批次；close错误不能冒充已确认。
func (s *Service) StopAndReport(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.cancel()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
	}
	s.drainMu.Lock()
	defer s.drainMu.Unlock()
	s.once.Do(func() {
		s.stopErr = errors.Join(s.pub.close(), s.parent.Close())
		if s.mapping != s.parent {
			s.stopErr = errors.Join(s.stopErr, s.mapping.Close())
		}
	})
	return s.stopErr
}

// DrainHistorical只暂停普通轮询；不关闭同boot发布器，直到P3真正QUIESCED才StopAndReport。
func (s *Service) DrainHistorical(ctx context.Context, topics []string) (int, error) {
	if s == nil {
		return 0, errors.New("历史drain relay未装配")
	}
	s.cancel()
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-s.done:
	}
	s.drainMu.Lock()
	defer s.drainMu.Unlock()
	if err := s.pub.allowHistorical(topics); err != nil {
		return 0, err
	}
	return s.replay.DrainHistorical(ctx, topics)
}

func replayRedis(source *redis.Client) *redis.Client {
	options := *source.Options()
	options.ContextTimeoutEnabled, options.MaxRetries = true, -1
	options.DialTimeout, options.ReadTimeout, options.WriteTimeout = time.Second, time.Second, time.Second
	options.PoolTimeout, options.DialerRetries = time.Second, 1
	options.PoolSize, options.MaxActiveConns, options.MaxConcurrentDials = 2, 2, 2
	options.MinIdleConns, options.MaxIdleConns = 0, 2
	return redis.NewClient(&options)
}

func (s *Service) FrontierDigest(ctx context.Context) ([]byte, error) {
	if s == nil {
		return nil, errors.New("补投owner缺失")
	}
	if err := s.StopAndReport(ctx); err != nil {
		return nil, err
	}
	return s.pub.sends.SettledDigest(ctx)
}
