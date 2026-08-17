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
	Redis  *redis.Redis
	Etcd   *clientv3.Client
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
	// 同 hostname 重启复用同一 worker id,Handle.NewNode 注入前任高水位地板。
	host, err := os.Hostname()
	if err != nil {
		panic(fmt.Sprintf("snowflake: failed to get hostname: %v", err))
	}
	allocCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hd, err := snowflakealloc.AllocateWithKeepAlive(allocCtx, etcdCli, "/match", host, snowflakealloc.Options{LeaseTTL: 60})
	if err != nil {
		panic(fmt.Sprintf("snowflake worker id alloc failed: %v", err))
	}
	logx.Infof("[match] snowflake worker id = %d (host=%s)", hd.WorkerID, host)

	sc := &ServiceContext{
		Config:             c,
		Redis:              redis.MustNewRedis(c.Redis.RedisConf),
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
