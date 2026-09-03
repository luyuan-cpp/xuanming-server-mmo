package svc

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"match/internal/config"
	"match/internal/discovery"
	matchkafka "match/internal/kafka"
	"match/internal/metrics"

	"shared/kafkautil"
	"shared/snowflake"
	"shared/snowflakealloc"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// SceneNodeRpcPrefix / BattleNodeRpcPrefix 是 match 侧 watch 的两个发现前缀。
// 与 load_reporter 的 sceneNodePrefix 同写法:字符串字面量,不依赖生成的枚举名。
const (
	SceneNodeRpcPrefix  = "SceneNodeService.rpc/"
	BattleNodeRpcPrefix = "BattleNodeService.rpc/"
)

type ServiceContext struct {
	Config config.Config

	// 双存储(设计文档 cross-zone-matchmaking.md D2):
	//   - MatchRedis:match 私有 key(match:* / challenge:* / spectate:*),
	//     可指向 Redis Cluster;队列三类 key 共用 {mq} hash tag 同 slot。
	//   - SharedRedis:跨运行时契约 key(player:{id}:location /
	//     player:session:{id} / battle:lock:{id}),写者是 scene_manager /
	//     player_locator / C++ scene,match 只读。C++ 无集群客户端,所以
	//     这条永远是既有共享库。
	// MatchRedis 未配置时两者是同一句柄(本地单库形态,行为与改前一致)。
	MatchRedis  *redis.Redis
	SharedRedis *redis.Redis
	// Redis 是 SharedRedis 的过渡别名:保留一版避免外部引用断裂,
	// 新代码一律按 key 归属显式选 MatchRedis / SharedRedis,不要再用它。
	Redis *redis.Redis

	Etcd *clientv3.Client
	// Kafka 只用于挑战(切磋)S2C 经 gate-{id} 的推送,同步 RequireOne
	// (与 friend/guild 的 servicecontext 同口径)。
	Kafka              *kafka.Writer
	GateCommandBuilder kafkautil.GateCommandBuilder

	// BattleIDGen 生产 battle_id 与 challenge_id(shared/snowflake 17-bit
	// worker 布局)。SnowFlake 节点隔离不变量(宪法 §7):这两类 id 只能由
	// match 节点生产,worker id 通过 etcd 前缀 "/match" 独立分配,与其它
	// 节点类型的 worker 池互不相干。
	BattleIDGen *snowflake.Node
	snowflakeHd *snowflakealloc.Handle

	// InstanceID 是本进程一次性 UUID,用作 matcher 凑单锁的持有者标记
	// (释放锁时比对,避免误删别的实例续拿的锁)。
	InstanceID string

	// SceneNodes / BattleNodes 是 etcd list-watch 内存镜像:
	//   - SceneNodes:按 player:{id}:location 里的 (zone_id, node_id) 定位
	//     scene 节点 gRPC 地址(PrepareBattle / CancelBattlePrepare 对端);
	//   - BattleNodes:全局池,v1 随机选一个执行 CreateBattle。
	SceneNodes  *discovery.NodeWatcher
	BattleNodes *discovery.NodeWatcher

	stopOnce sync.Once
}

func NewServiceContext(c config.Config) *ServiceContext {
	etcdCli, err := clientv3.New(clientv3.Config{
		Endpoints:   c.Etcd.Hosts,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		panic("failed to create etcd client: " + err.Error())
	}

	// 通过 shared/snowflakealloc 拿独立的 Snowflake worker id(照 scene_manager):
	// prefix="/match" 与 NodeInfo.NodeId 解耦,reRegister 换业务 node_id 不影响发号;
	// 同亲和键重启复用同一 worker id,Handle.NewNode 注入前任高水位地板。
	//
	// 亲和键 = hostname#ListenOn 而非裸 hostname(设计决策 D5b):snowflakealloc
	// 按亲和键复用 worker id 且不校验旧持有者是否存活(allocator.go 的 CAS 只比
	// nodeKey 的 value),同一主机上两个 match 进程(本地双 zone / -Counts match=2)
	// 用裸 hostname 会拿到同一 worker id → battle_id / challenge_id 撞号。加上监听
	// 地址后同机多进程各自一把键;同一进程重启仍复用原 id。
	host, err := os.Hostname()
	if err != nil {
		panic(fmt.Sprintf("snowflake: failed to get hostname: %v", err))
	}
	affinityKey := fmt.Sprintf("%s#%s", host, c.ListenOn)
	allocCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hd, err := snowflakealloc.AllocateWithKeepAlive(allocCtx, etcdCli, "/match", affinityKey, snowflakealloc.Options{LeaseTTL: 60})
	if err != nil {
		panic(fmt.Sprintf("snowflake worker id alloc failed: %v", err))
	}
	logx.Infof("[match] snowflake worker id = %d (affinity=%s)", hd.WorkerID, affinityKey)

	matchRds, sharedRds := NewRedisHandles(c)
	sc := &ServiceContext{
		Config:             c,
		MatchRedis:         matchRds,
		SharedRedis:        sharedRds,
		Redis:              sharedRds,
		Etcd:               etcdCli,
		GateCommandBuilder: matchkafka.NewGateCommandBuilder(),
		BattleIDGen:        hd.NewNode(),
		snowflakeHd:        hd,
		InstanceID:         uuid.New().String(),
	}

	kafkaWriteTimeout := time.Duration(c.KafkaWriteTimeoutSeconds) * time.Second
	if kafkaWriteTimeout <= 0 {
		kafkaWriteTimeout = 5 * time.Second
	}
	sc.Kafka = &kafka.Writer{
		Addr: kafka.TCP(c.Kafka.Brokers...),
		// Hash 按消息 Key(player_id)选分区(项目不变量:kafka key =
		// 业务实体 ID,同一玩家的推送必须有序)。
		Balancer:               &kafka.Hash{},
		AllowAutoTopicCreation: true,
		BatchTimeout:           10 * time.Millisecond,
		MaxAttempts:            1,
		WriteTimeout:           kafkaWriteTimeout,
		// kafka.Writer 的 RequiredAcks 零值是 RequireNone(fire-and-forget),
		// 显式 RequireOne:挑战弹窗丢了对端只能干等 60s 超时。
		RequiredAcks: kafka.RequireOne,
		Async:        false,
	}

	sc.SceneNodes = discovery.NewNodeWatcher("scene", SceneNodeRpcPrefix,
		metrics.SetDiscoveredNodes,
		func(entry discovery.NodeEntry) { discovery.RemoveEndpointConn(entry.Endpoint) })
	sc.BattleNodes = discovery.NewNodeWatcher("battle", BattleNodeRpcPrefix,
		metrics.SetDiscoveredNodes,
		func(entry discovery.NodeEntry) { discovery.RemoveEndpointConn(entry.Endpoint) })

	return sc
}

// NewRedisHandles 按配置建立 (MatchRedis, SharedRedis) 两个句柄。
// MatchRedis.Host 为空即未配置,私有 key 回落到共享库,两者返回同一句柄
// (向后兼容:本地/既有部署不改 yaml 照跑)。拆成独立函数是为了不依赖
// etcd 就能测试回落逻辑。
func NewRedisHandles(c config.Config) (matchRds, sharedRds *redis.Redis) {
	sharedRds = redis.MustNewRedis(c.Redis.RedisConf)
	if c.MatchRedis.Host == "" {
		return sharedRds, sharedRds
	}
	matchRds = redis.MustNewRedis(c.MatchRedis)
	logx.Infof("[match] MatchRedis 独立配置生效 host=%s type=%s(契约 key 仍走 Redis host=%s)",
		c.MatchRedis.Host, c.MatchRedis.Type, c.Redis.Host)
	return matchRds, sharedRds
}

// SnowflakeLost 在本进程失去 Snowflake worker id 的 etcd 租约时关闭。
// 此时 etcd 可以把同一 worker id 分给别的进程,继续发号就是确定性撞号,
// 主流程必须 fence 后退出(见 match_service.go)。
func (sc *ServiceContext) SnowflakeLost() <-chan struct{} {
	if sc.snowflakeHd == nil {
		return nil
	}
	return sc.snowflakeHd.Lost()
}

// Stop 先关 Kafka writer(flush 在途批次),再释放 Snowflake worker lease。
// 幂等,正常 defer 与失租强退路径可安全共用。
func (sc *ServiceContext) Stop() {
	sc.stopOnce.Do(func() {
		if sc.Kafka != nil {
			if err := sc.Kafka.Close(); err != nil {
				logx.Errorf("[match] Kafka writer close/flush failed: %v", err)
			}
		}
		if sc.snowflakeHd != nil {
			sc.snowflakeHd.Close()
			sc.snowflakeHd = nil
		}
	})
}
