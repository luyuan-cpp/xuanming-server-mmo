package svc

import (
	"context"
	"database/sql"
	"fmt"
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
	"shared/generated/table"
	"shared/idsegment"
	"shared/kafkacmd"
	"shared/kafkautil"
)

type ServiceContext struct {
	Config                   config.Config
	RedisClient              *redis.Client
	PlayerLocatorRedisClient *redis.Client
	DB                       *sql.DB
	KafkaWriter              *kafkago.Writer
	GateCommandBuilder       kafkautil.GateCommandBuilder
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
	// MergeMarkerRedisClient 指向 data_service 的 mapping Redis,只用来读
	// merge:in_progress:{zone}(合服闸门,见 internal/logic/merge_fence.go)。
	// 没配 MergeMarkerRedis 时为 nil = 闸门不生效。
	MergeMarkerRedisClient *redis.Client
	// MatchInternal 是 match 内部服务的客户端(同道历练确认开战,06-activities.md §6.22 / §6.26)。
	// 没配 MatchRpc 目标时是 **nil 接口** = 历练未开放;guild.go 把它原样赋给 logic.ActivityDeps.Match
	// (接口赋接口,nil 仍是 nil),并在 !Config.TrialResultActive() 时改传 nil —— 没有结算就不许开战。
	MatchInternal matchpb.MatchInternalClient
}

func NewServiceContext(c config.Config) *ServiceContext {
	table.LoadTables(c.TableDir, false)

	rdb := redis.NewClient(&redis.Options{
		Addr:     c.RedisClient.Host,
		Password: c.RedisClient.Password,
		DB:       c.RedisClient.DB,
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
	})
	if err := plRdb.Ping(context.Background()).Err(); err != nil {
		panic(fmt.Errorf("failed to connect player locator Redis: %w", err))
	}

	// 帮会写事务的锁等待封顶(设计 docs/design/guild-phase2/02-management.md §6.2a):
	// InnoDB 默认 innodb_lock_wait_timeout=50,而 RPC 的 ctx 到期后 go-sql-driver 只是关掉连接 ——
	// 服务端等锁的线程不跟着退出,会继续占着本事务已拿到的 guild 行锁,于是该帮会后续所有写
	// 连锁超时,最长 50 秒。帮会写事务都是几条主键 / 索引语句,等锁超过 1 秒即视为异常长事务。
	//
	// 钉在代码里而不是 yaml:K8s ConfigMap 与本地 yaml 各改一遍,等于给"某个环境忘了改"留后门,
	// 而这是正确性约束、不是部署偏好。解析失败一律拒启,且错误里不含 DSN 原文(它带口令)。
	// -migrate 走 guild.go 里另一条不经过 ServiceContext 的路径,刻意不加该参数:
	// DDL 等的是元数据锁(lock_wait_timeout),与行锁另有语义。
	dsn, err := data.WithLockWaitTimeout(c.MySQL.DataSource)
	if err != nil {
		panic(fmt.Errorf("failed to build MySQL DSN: %w", err))
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		panic(fmt.Errorf("failed to connect MySQL: %w", err))
	}
	if err := db.Ping(); err != nil {
		panic(fmt.Errorf("failed to ping MySQL: %w", err))
	}

	var w *kafkago.Writer
	if len(c.Kafka.Brokers) > 0 {
		w = &kafkago.Writer{
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

	sc := &ServiceContext{
		Config:                   c,
		RedisClient:              rdb,
		PlayerLocatorRedisClient: plRdb,
		DB:                       db,
		KafkaWriter:              w,
		GateCommandBuilder:       guildkafka.NewGateCommandBuilder(),
		MergeMarkerRedisClient:   newMergeMarkerRedis(c),
	}
	sc.initDataServiceClient()
	sc.initGuildIDSegment()
	// 必须在 initGuildIDSegment 之后:"开了号段却没配 data_service"由它先拒启,这里只剩启用 / 未启用两种形态。
	sc.initAssetOpIDSegment()
	sc.initMatchInternalClient()
	return sc
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
		logx.Info("[ServiceContext] MatchRpc 未配置:同道历练未开放(视图显示未开放,两个历练写 RPC 回 kGuildActivityNotOpen);灯会 / 团圆不受影响")
		return
	}
	rpcConf := s.Config.MatchRpc
	if !rpcConf.NonBlock {
		logx.Info("[ServiceContext] MatchRpc.NonBlock 未开启,已强制按非阻塞拨号(match 是弱依赖,不能卡住 guild 起服)")
		rpcConf.NonBlock = true
	}
	conn := zrpc.MustNewClient(rpcConf)
	s.MatchInternal = matchpb.NewMatchInternalClient(conn.Conn())
	logx.Infof("[ServiceContext] MatchInternal 客户端已建立(非阻塞拨号): timeout=%dms", rpcConf.Timeout)
}

// newMergeMarkerRedis 建合服闸门用的只读 Redis 客户端;没配 = nil = 闸门不生效。
//
// 与上面两个客户端刻意不同:**Ping 失败不 panic**。那两个是 guild 的命脉(缓存 /
// 在场查询),连不上就没有可用形态;这一个只喂一次 EXISTS,而且运行期读失败已经
// fail-closed(见 logic.mergeFenceTip:查不到就拒绝建帮)。为一个可选加固的
// 瞬时不可达而拒启,等于把"合服期多一道锁"换成"平时多一个启动失败点"。
func newMergeMarkerRedis(c config.Config) *redis.Client {
	if !c.MergeMarkerRedis.Enabled() {
		logx.Info("[ServiceContext] MergeMarkerRedis 未配置:合服闸门不生效,CreateGuild 不检查 merge:in_progress:{zone}(合服维护窗口内仍可在源 zone 建帮)")
		return nil
	}
	cli := redis.NewClient(&redis.Options{
		Addr:     c.MergeMarkerRedis.Host,
		Password: c.MergeMarkerRedis.Pass,
		DB:       c.MergeMarkerRedis.DB,
	})
	if err := cli.Ping(context.Background()).Err(); err != nil {
		logx.Errorf("[ServiceContext] 合服闸门 Redis %s DB=%d 暂不可达(客户端保留,会自动重连);在它恢复之前 CreateGuild 会 fail-closed 拒绝: %v",
			c.MergeMarkerRedis.Host, c.MergeMarkerRedis.DB, err)
		return cli
	}
	logx.Infof("[ServiceContext] 合服闸门已启用:%s DB=%d,键 merge:in_progress:{zone}", c.MergeMarkerRedis.Host, c.MergeMarkerRedis.DB)
	return cli
}

func (s *ServiceContext) Stop() {
	// 号段客户端最先关:Close 取消在途领段并等它退出,之后再关 Kafka / Redis / DB。
	// op_id 排在 guild_id 之前只是与建立顺序相反,两者互不依赖。
	if s.AssetOpIDSegment != nil {
		s.AssetOpIDSegment.Close()
	}
	if s.GuildIDSegment != nil {
		s.GuildIDSegment.Close()
	}
	if s.KafkaWriter != nil {
		if err := s.KafkaWriter.Close(); err != nil {
			logx.Errorf("Failed to close Kafka writer: %v", err)
		}
	}
	if s.RedisClient != nil {
		if err := s.RedisClient.Close(); err != nil {
			logx.Errorf("Failed to close Redis client: %v", err)
		}
	}
	if s.PlayerLocatorRedisClient != nil {
		if err := s.PlayerLocatorRedisClient.Close(); err != nil {
			logx.Errorf("Failed to close player locator Redis client: %v", err)
		}
	}
	if s.MergeMarkerRedisClient != nil {
		if err := s.MergeMarkerRedisClient.Close(); err != nil {
			logx.Errorf("Failed to close merge marker Redis client: %v", err)
		}
	}
	if s.DB != nil {
		if err := s.DB.Close(); err != nil {
			logx.Errorf("Failed to close MySQL: %v", err)
		}
	}
}
