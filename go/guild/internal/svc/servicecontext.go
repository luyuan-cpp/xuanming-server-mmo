package svc

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	kafkago "github.com/segmentio/kafka-go"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/zrpc"

	"guild/internal/config"
	"guild/internal/data"
	guildkafka "guild/internal/kafka"
	dspb "proto/data_service"
	matchpb "proto/match"
	"shared/authoritysql"
	"shared/drainproof"
	"shared/generated/table"
	"shared/idsegment"
	"shared/kafkacmd"
	"shared/kafkautil"
	"shared/playerredis"
	"shared/playersync"
)

// playerSyncServiceName 是本服务在落地恢复摘要指标 player_sync_bump_total 里的 service label。
const playerSyncServiceName = "guild"

// 摘要 bump 专用 Redis 客户端的参数(见 newPlayerSyncRedis)。
const (
	// playerSyncRedisPoolSize:bump 是提交后的异步提示,串行逐人发,正常一批只占一条连接;
	// 给小池子是为了 Redis 卡住时堆积的 bump 协程只占这几条连接,不去挤在线查询与推送路由的连接池。
	playerSyncRedisPoolSize = 4
	// playerSyncRedisIOTimeoutMs:拨号与单次读写的兜底上限(毫秒),与 logic 的整批预算 guildSyncBumpBudget 同为 2s。
	// ctx 截止约束套接字读写,套接字截止取 ctx 截止与它两者中较早的一个,所以真正起作用的是整批预算。
	playerSyncRedisIOTimeoutMs = 2000
)

type ServiceContext struct {
	ConsumerOwner *kafkautil.BoundConsumerGroup
	// 同一 boot 的 SQL 身份与两个独立角色；P1 不关闭 RELAY 或 SQL 池。
	WriterConfig     authoritysql.WriterConfig
	RootAdmission    *drainproof.Session
	RelayAdmission   *drainproof.Session
	RelayIngress     *drainproof.Session
	DrainRuntime     *drainproof.Runtime
	lifecycleMu      sync.Mutex
	sourceMu         sync.Mutex
	relayStop        func(context.Context) error
	publicationRepo  *data.GuildRepo
	resourcesClosed  bool
	resourceCloseErr error
	sourceStop       func(context.Context) error
	sourceClosed     bool
	Config           config.Config
	RedisClient      *redis.Client
	// PlayerLocatorRedisClient 只剩控制域:订阅 assetop:wake,写唤醒去重键。玩家键一律走 PlayerRedis。
	PlayerLocatorRedisClient *redis.Client
	// PlayerRedis 是玩家域句柄(shared/playerredis):会话、位置、战斗锁。键一律带 {player_id},发送前经守卫核对。
	PlayerRedis        redis.UniversalClient
	DB                 *sql.DB
	KafkaWriter        *kafkago.Writer
	GateCommandBuilder kafkautil.GateCommandBuilder
	// DataServiceClient 用于查询角色归属区及领取 guild_id 号段。
	// 配置了 DataServiceRpc 就拨号,与 IdSegment.Enabled 无关。
	DataServiceClient dspb.DataServiceClient
	// GuildIDSegment 是 guild_id 号段客户端(shared/idsegment,biz_tag="guild");
	// IdSegment.Enabled=false 时为 nil。发号策略(是否回退 snowflake)在 guild.go 里
	// 用 NewGuildIDMinter 组装,因为 snowflake 节点在那里才申领得到。
	GuildIDSegment *idsegment.Client
	// AssetOpIDSegment 是资产指令 op_id 的号段客户端(biz_tag="guild_asset_op",B5b);
	// IdSegment.Enabled=false 时为 nil。**没有** snowflake 回退(op_id 是 outbox 主键),
	// guild.go 据 nil 把 EconomyDeps.OpIDs 留成 nil 接口。见 asset_op.go 的 initAssetOpIDSegment。
	AssetOpIDSegment *idsegment.Client
	// MatchInternal 是 match 内部服务的客户端(同道历练确认开战,06-activities.md §6.22 / §6.26)。
	// 没配 MatchRpc 目标时是 **nil 接口** = 历练未开放;guild.go 把它原样赋给 logic.ActivityDeps.Match
	// (接口赋接口,nil 仍是 nil),并在 !Config.TrialResultActive() 时改传 nil —— 没有结算就不许开战。
	MatchInternal matchpb.MatchInternalClient
	// PlayerSyncBumper 给玩家的落地恢复摘要 player:{id}:sync_digest 按域 +1
	// (docs/design/landing-recovery-sync-digest.md §3;guild 负责 GUILD / GUILD_APPLICATION / GUILD_ECONOMY /
	// GUILD_ACTIVITY 四个域)。guild.go 经 logic.WithSyncBumper(logic.NewGuildSyncBumper(...)) 交给 logic,
	// 由它在提交之后异步调用。
	//
	// 摘要是玩家域键,必须写在 C++ scene 读得到的那个存储域上,也就是与 scene / scene_manager 写 player:{id}:location 的同一个域:
	// 本服务里就是 PlayerRedis 那一份配置。指错的表现是 scene 读到的摘要永不变化 —— 客户端退化为"打开帮会界面才拉",且不报错。
	// 底下是专用客户端 PlayerSyncRedisClient(同一份玩家域配置、单独连接池),理由见 newPlayerSyncRedis。
	PlayerSyncBumper *playersync.Bumper
	// PlayerSyncRedisClient 是 PlayerSyncBumper 专用的玩家域客户端,只由 Stop 关闭;别的代码不要拿它发命令。
	PlayerSyncRedisClient redis.UniversalClient
}

func NewServiceContext(c config.Config) *ServiceContext {
	writer, err := authoritysql.LoadWriterConfig(os.Getenv)
	if err != nil {
		panic(fmt.Errorf("guild SQL boot 装配失败: %w", err))
	}
	return NewServiceContextWithAdmission(c, writer, drainproof.New(), drainproof.New())
}

// NewServiceContextWithAdmission 由实际进程 owner 显式传入同一 boot 的身份与角色门禁。
// 新 boot 的 prepared 门禁也在这里传入，不能在 handler 已持有门禁后替换它。
func NewServiceContextWithAdmission(c config.Config, writer authoritysql.WriterConfig, root, relay *drainproof.Session) *ServiceContext {
	if len(writer.InstanceID()) != 16 || len(writer.WriterKey()) != 16 || root == nil || relay == nil || root == relay {
		panic("guild SQL boot 与 SOURCE/RELAY 装配缺失或角色复用")
	}
	table.LoadTables(c.TableDir, false)

	// PoolSize:0 = go-redis 缺省 10 × GOMAXPROCS,负数已被 config.Validate 拒绝(见 config.RedisConf.PoolSize)。
	rdb := redis.NewClient(&redis.Options{
		Addr:     c.RedisClient.Host,
		Password: c.RedisClient.Password,
		DB:       c.RedisClient.DB,
		PoolSize: c.RedisClient.PoolSize,
	})

	if err := rdb.Ping(context.Background()).Err(); err != nil {
		panic(fmt.Errorf("failed to connect global Redis: %w", err))
	}

	plRedisConf := c.PlayerLocatorRedis
	if plRedisConf.Host == "" {
		plRedisConf = c.RedisClient
		plRedisConf.DB = 0
	}
	plRdb := redis.NewClient(&redis.Options{
		Addr:     plRedisConf.Host,
		Password: plRedisConf.Password,
		DB:       plRedisConf.DB,
		PoolSize: plRedisConf.PoolSize,
	})
	if err := plRdb.Ping(context.Background()).Err(); err != nil {
		panic(fmt.Errorf("failed to connect player locator Redis: %w", err))
	}
	playerRedisConf := c.EffectivePlayerRedis()
	playerRdb, err := playerredis.New(playerRedisConf)
	if err != nil {
		panic(fmt.Errorf("invalid PlayerRedis config: %w", err))
	}
	if err := playerRdb.Ping(context.Background()).Err(); err != nil {
		panic(fmt.Errorf("failed to connect player Redis (%s): %w", playerRedisConf.Describe(), err))
	}
	logx.Infof("[ServiceContext] player redis %s", playerRedisConf.Describe())

	// 帮会写事务的锁等待封顶(设计 docs/design/guild-phase2/02-management.md §6.2a):
	// InnoDB 默认 innodb_lock_wait_timeout=50,而 RPC 的 ctx 到期后 go-sql-driver 只是关掉连接 ——
	// 服务端等锁的线程不跟着退出,会继续占着本事务已拿到的 guild 行锁,于是该帮会后续所有写
	// 连锁超时,最长 50 秒。帮会写事务都是几条主键 / 索引语句,等锁超过 1 秒即视为异常长事务。
	//
	// 钉在代码里而不是 yaml:K8s ConfigMap 与本地 yaml 各改一遍,等于给"某个环境忘了改"留后门,
	// 而这是正确性约束、不是部署偏好。解析失败一律拒启,且错误里不含 DSN 原文(它带口令)。
	// 迁移使用独立强会话池；DDL的元数据锁与socket预算由迁移器单独约束。
	dbCtx, dbCancel := context.WithTimeout(context.Background(), 5*time.Second)
	db, err := openMySQL(dbCtx, c.MySQL, authoritysql.Writer(writer))
	dbCancel()
	if err != nil {
		panic(fmt.Errorf("failed to connect authoritative SQL: %w", err))
	}

	var w *kafkago.Writer
	if len(c.Kafka.Brokers) > 0 {
		w = &kafkago.Writer{
			Completion: func(_ []kafkago.Message, err error) {
				if err != nil {
					relay.Hold()
				}
			},
			Addr: kafkago.TCP(c.Kafka.Brokers...),
			// 控制面命令 topic(gate-cmd_gN)必须按 node_id % P 落到**指定分区**:
			// 消费端 assign 的就是那一个分区,落错分区 = 目标 gate 永远收不到,
			// 而 Kafka 一个错都不报(docs/design/control-plane-topic-partitioning-20260908.md)。
			// kafka-go 的 Writer 在写入路径上忽略 Message.Partition、只问 Balancer。
			//
			// 其它 topic 仍走 Hash 按 Key(player_id/gate_id)选分区,保证同一实体的
			// 推送有序(项目不变量:kafka key = 业务实体 ID)。LeastBytes 忽略 Key。
			Balancer: &kafkacmd.CommandPartitionBalancer{Fallback: &kafkago.Hash{}},
			// 帮会变更推送是"至多一次的提示拉取"(internal/logic/push.go 文件头纪律 2):
			// MaxAttempts=1 **不重试** —— 重试既可能让同一玩家的两条提示乱序,又会把
			// logic.guildPushBudget(3s)整段吃光,而漏掉的人下次打开帮会界面拉一次就对齐了。
			// WriteTimeout 与那个预算同值;BatchTimeout 若留默认 1s,每条推送都要白等一秒才成批发出。
			BatchTimeout: 10 * time.Millisecond,
			MaxAttempts:  1,
			WriteTimeout: 3 * time.Second,
			// kafka-go 的 RequiredAcks 零值是 RequireNone(fire-and-forget):
			// 写进 socket 即返回 nil,broker 端 leader 切换/落盘前崩溃全部不可见,
			// 于是 gate_push 依赖 WriteMessages 返回值的 fail-closed 语义形同虚设。
			// 与 scene_manager / player_locator 的 servicecontext 对齐,显式 RequireOne。
			RequiredAcks: kafkago.RequireOne,
		}
	}

	syncRdb, err := newPlayerSyncRedis(playerRedisConf)
	if err != nil {
		panic(fmt.Errorf("invalid PlayerRedis config: %w", err))
	}
	sc := &ServiceContext{
		WriterConfig:             writer,
		RootAdmission:            root,
		RelayAdmission:           relay,
		RelayIngress:             drainproof.New(),
		Config:                   c,
		RedisClient:              rdb,
		PlayerLocatorRedisClient: plRdb,
		PlayerRedis:              playerRdb,
		DB:                       db,
		KafkaWriter:              w,
		GateCommandBuilder:       guildkafka.NewGateCommandBuilder(),
		// 服务名 "guild" 只作 player_sync_bump_total 的 service label(有界常量,不带任何 id)。
		PlayerSyncBumper:      playersync.NewBumper(playersync.NewGoRedisEvaler(syncRdb), playerSyncServiceName),
		PlayerSyncRedisClient: syncRdb,
	}
	sc.initDataServiceClient()
	sc.initGuildIDSegment()
	// 必须在 initGuildIDSegment 之后:"开了号段却没配 data_service"由它先拒启,这里只剩启用 / 未启用两种形态。
	sc.initAssetOpIDSegment()
	sc.initMatchInternalClient()
	return sc
}

// OpenMySQL 只接受帮会独占库，逐物理连接初始化强会话；TiDB不满足时拒启。
// WithLockWaitTimeout继续强制clientFoundRows=false并去掉旧隔离别名。
func OpenMySQL(ctx context.Context, c config.MySQLConf) (*sql.DB, error) {
	writer, err := authoritysql.LoadWriterConfig(os.Getenv)
	if err != nil {
		return nil, err
	}
	return openMySQL(ctx, c, authoritysql.Writer(writer))
}

// OpenReadOnlyMySQL只用于常驻启动期的只读schema核对。
func OpenReadOnlyMySQL(ctx context.Context, c config.MySQLConf) (*sql.DB, error) {
	return openMySQL(ctx, c, authoritysql.Reader())
}

func openMySQL(ctx context.Context, c config.MySQLConf, permission authoritysql.Permission) (*sql.DB, error) {
	dsn, err := data.WithLockWaitTimeout(c.DataSource)
	if err != nil {
		return nil, err
	}
	maxOpen, maxIdle := c.EffectivePool()
	db, err := authoritysql.Open(ctx, dsn, data.DatabaseName, authoritysql.Backend(c.Backend), maxOpen, permission)
	if err != nil {
		return nil, err
	}
	db.SetMaxIdleConns(maxIdle)
	return db, nil
}

// OpenSchemaMySQL 只用于显式迁移，不能交给常驻启动检查或在线仓储。
func OpenSchemaMySQL(ctx context.Context, c config.MySQLConf) (*sql.DB, error) {
	dsn, err := data.WithLockWaitTimeout(c.DataSource)
	if err != nil {
		return nil, err
	}
	permission, err := authoritysql.LoadSchemaPermission(os.Getenv)
	if err != nil {
		return nil, err
	}
	return authoritysql.OpenForMigration(ctx, dsn, data.DatabaseName, authoritysql.Backend(c.Backend), permission)
}

// initMatchInternalClient 按 MatchRpc 建 match 内部服务客户端;没配目标时保持 nil 接口(历练未开放)。
//
// **强制 NonBlock**,不看 yaml 里写没写:match 对 guild 是弱依赖(只有历练确认开战用它),它没起、或晚于 guild 起,
// 都不该让帮会的建帮 / 审批 / 捐献 / 灯会一起起不来。阻塞拨号下 zrpc.MustNewClient 连不上会直接 Fatal ——
// 这是可用性约束、不是部署偏好,所以钉在代码里(与 WithLockWaitTimeout 钉进 DSN 同一个理由:
// ConfigMap 与本地 yaml 各改一遍,等于给"某个环境忘了写"留后门)。运行期 match 不可达时,
// logic 把调用失败映射成 tip kGuildTrialServiceBusy(不是 gRPC 错误)。
func (s *ServiceContext) initMatchInternalClient() {
	if !s.Config.MatchRpcConfigured() {
		logx.Info("[ServiceContext] MatchRpc not configured: guild trial closed (view shows closed, both trial write RPCs return kGuildActivityNotOpen); Lantern Festival / Reunion unaffected")
		return
	}
	rpcConf := s.Config.MatchRpc
	if !rpcConf.NonBlock {
		logx.Info("[ServiceContext] MatchRpc.NonBlock disabled; forced nonblocking dial (match is an optional dependency and must not block guild startup)")
		rpcConf.NonBlock = true
	}
	conn := zrpc.MustNewClient(rpcConf)
	s.MatchInternal = matchpb.NewMatchInternalClient(conn.Conn())
	logx.Infof("[ServiceContext] MatchInternal client created (nonblocking dial): timeout=%dms", rpcConf.Timeout)
}

// newPlayerSyncRedis 建落地恢复摘要 bump 专用的玩家域客户端。地址、口令与 PlayerRedis 完全相同(conf 就是生效的玩家域配置),
// 但单独一个客户端:小连接池(playerSyncRedisPoolSize)不和在线查询、资产循环、推送路由抢连接。
// logic.AsyncGuildSyncBumper 的整批预算(2s)要封住**单条**命令,靠的是 playerredis 缺省打开的 ContextTimeoutEnabled。
//
// 不在启动时 Ping:同一份配置的 PlayerRedis 刚 Ping 过;摘要是可丢的提示,运行期不可达只计
// player_sync_bump_total{result="error"},不该为它多一个拒启点。
func newPlayerSyncRedis(conf playerredis.Config) (redis.UniversalClient, error) {
	conf.PoolSizePerNode = playerSyncRedisPoolSize
	conf.DialTimeoutMs = playerSyncRedisIOTimeoutMs
	conf.ReadTimeoutMs = playerSyncRedisIOTimeoutMs
	conf.WriteTimeoutMs = playerSyncRedisIOTimeoutMs
	conf.DisableContextTimeout = false
	return playerredis.New(conf)
}

// BindDrainRuntime 将维护原件绑定到已经消费的同一 SQL boot，不重新读 grant。
// 实际 main 仍须接 RPC、cleanup、trial 的 SOURCE 与资产/结果消费的 RELAY。
func (s *ServiceContext) BindDrainRuntime(runtime *drainproof.Runtime) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if runtime == nil || s.DrainRuntime != nil || s.RootAdmission == nil || s.RelayAdmission == nil ||
		s.RootAdmission == s.RelayAdmission || runtime.Seed("SOURCE") == nil || runtime.Seed("RELAY") == nil {
		return drainproof.ErrHold
	}
	if err := runtime.BindBoot(s.WriterConfig.InstanceID(), s.WriterConfig.WriterKey()); err != nil {
		return err
	}
	s.DrainRuntime = runtime
	return nil
}

// CloseSource 关闭根门禁，停止并 join 清理发起者，等待 RPC 后关闭只供根调用的 guild 号源。
// 资产号源、结果消费、RELAY 与 SQL 保持开放供已接纳工作派生和完成。
func (s *ServiceContext) BindSourceStop(stop func(context.Context) error) error {
	s.sourceMu.Lock()
	defer s.sourceMu.Unlock()
	if stop == nil || s.sourceStop != nil || s.sourceClosed {
		return drainproof.ErrHold
	}
	s.sourceStop = stop
	return nil
}
func (s *ServiceContext) CloseSource(ctx context.Context) error {
	s.sourceMu.Lock()
	defer s.sourceMu.Unlock()
	if s.sourceClosed {
		return nil
	}
	if s.RootAdmission == nil {
		return drainproof.ErrHold
	}
	s.RootAdmission.CloseAdmission()
	if s.sourceStop != nil {
		if err := s.sourceStop(ctx); err != nil {
			s.RootAdmission.Hold()
			return err
		}
	}
	if err := s.RootAdmission.Wait(ctx); err != nil {
		return err
	}
	if s.GuildIDSegment != nil {
		digest, err := s.GuildIDSegment.DrainAndReport(ctx)
		if err != nil {
			s.RootAdmission.Hold()
			return err
		}
		if err = s.RootAdmission.MergeClosedSettled(digest); err != nil {
			return err
		}
	}
	s.sourceClosed = true
	return nil
}

// StopAndReport 必须先取得资产 TX2/APPLIED、结果消费与发布链的真实 RELAY 前沿，
// 才能 Seal 同一 WriterConfig 并关 SQL 池；缺失绑定或实际 join 失败时保留共享资源。
func (s *ServiceContext) BindRelayOwners(repo *data.GuildRepo, stop func(context.Context) error) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if repo == nil || stop == nil || s.publicationRepo != nil || s.relayStop != nil {
		return drainproof.ErrHold
	}
	s.publicationRepo = repo
	s.relayStop = stop
	return nil
}
func (s *ServiceContext) StopAndReport(ctx context.Context) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.resourcesClosed {
		return s.resourceCloseErr
	}
	if err := s.CloseSource(ctx); err != nil {
		return err
	}
	if s.RelayAdmission == nil || s.publicationRepo == nil || s.relayStop == nil {
		return drainproof.ErrHold
	}
	s.RelayIngress.CloseAdmission()
	if err := s.RelayIngress.Wait(ctx); err != nil {
		return err
	}
	if s.ConsumerOwner != nil {
		raw, err := s.ConsumerOwner.CloseAndReport(ctx)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(raw)
		if err = s.RelayAdmission.MergeSettled(digest[:]); err != nil {
			return err
		}
	}
	if err := s.relayStop(ctx); err != nil {
		s.RelayAdmission.Hold()
		return err
	}
	if s.AssetOpIDSegment != nil {
		digest, err := s.AssetOpIDSegment.DrainAndReport(ctx)
		if err != nil {
			s.RelayAdmission.Hold()
			return err
		}
		if err = s.RelayAdmission.MergeSettled(digest); err != nil {
			return err
		}
	}
	// 调用入口和所有 worker 已 join，缓存失败重试仍由 repo 自己的实际 owner 等待。
	digest, err := s.publicationRepo.ClosePublication(ctx)
	if err != nil {
		return err
	}
	if err = s.RelayAdmission.MergeSettled(digest); err != nil {
		return err
	}
	if err = s.RelayAdmission.WaitIdle(ctx); err != nil {
		return err
	}
	if s.KafkaWriter != nil {
		if err = s.KafkaWriter.Close(); err != nil {
			s.RelayAdmission.Hold()
			return err
		}
	}
	s.RelayAdmission.CloseAdmission()
	if err = s.RelayAdmission.Wait(ctx); err != nil {
		return err
	}
	s.WriterConfig.Seal()
	var result error
	if s.PlayerSyncRedisClient != nil {
		result = errors.Join(result, s.PlayerSyncRedisClient.Close())
	}
	if s.PlayerRedis != nil {
		result = errors.Join(result, s.PlayerRedis.Close())
	}
	if s.PlayerLocatorRedisClient != nil {
		result = errors.Join(result, s.PlayerLocatorRedisClient.Close())
	}
	if s.RedisClient != nil {
		result = errors.Join(result, s.RedisClient.Close())
	}
	if s.DB != nil {
		result = errors.Join(result, s.DB.Close())
	}
	s.resourcesClosed = true
	s.resourceCloseErr = result
	if result != nil {
		s.RelayAdmission.Hold()
	}
	return result
}
func (s *ServiceContext) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := s.StopAndReport(ctx); err != nil {
		logx.Errorf("guild 排空 HOLD: %v", err)
	}
}

// BindConsumerOwner 只登记本服务真实消费组，不能在维护时换成另一个代次 owner。
func (sc *ServiceContext) BindConsumerOwner(owner *kafkautil.BoundConsumerGroup) error {
	sc.lifecycleMu.Lock()
	defer sc.lifecycleMu.Unlock()
	if owner == nil || sc.ConsumerOwner != nil {
		return drainproof.ErrHold
	}
	sc.ConsumerOwner = owner
	return nil
}
