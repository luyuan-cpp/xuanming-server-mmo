package svc

import (
	"context"
	"fmt"
	"os"
	"scene_manager/internal/config"
	"scene_manager/internal/metrics"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	dspb "proto/data_service"
	"shared/generated/table"
	"shared/kafkacmd"
	"shared/snowflake"
	"shared/snowflakealloc"

	"github.com/segmentio/kafka-go"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
)

// KafkaWriter 把同步发布与关闭收进同一接口，使 Stop 能统一释放 writer，
// 也让生命周期测试无需连接真实 broker。
type KafkaWriter interface {
	WriteMessages(context.Context, ...kafka.Message) error
	Close() error
}

// HomeZoneClient 是 scene_manager 对 data_service 的**唯一**依赖面:查玩家归属 zone。
// 刻意只收窄到这一个方法:dspb.DataServiceClient 天然满足它,而单测用假实现替换时
// 不必把整个 DataServiceClient 的十几个方法都桩出来。
type HomeZoneClient interface {
	GetPlayerHomeZone(ctx context.Context, in *dspb.GetPlayerHomeZoneRequest, opts ...grpc.CallOption) (*dspb.GetPlayerHomeZoneResponse, error)
}

type ServiceContext struct {
	Config config.Config
	Redis  *redis.Redis
	Kafka  KafkaWriter
	Etcd   *clientv3.Client
	// HomeZone 查 data_service 的 player:zone 映射,EnterScene 用它填
	// RoutePlayerEvent.home_zone_id。yaml 没配 DataServiceRpc 时为 nil,
	// 调用方按「本地单区联调」处理(归属 = gate zone,WARN 计数)。
	HomeZone      HomeZoneClient
	RoleDirectory RoleDirectory
	// OwnerEpochMint 是铸造 owner_epoch 的执行面(墙钟地板 + 铸造 EVAL 与 WAITAOF 同一条连接,owner_epoch_mint.go)。
	// 生产恒非 nil;logic 遇到 nil 一律拒绝铸造(fail-closed),不退回 Redis 字段上的无确认路径。
	OwnerEpochMint OwnerEpochMinter
	SceneIDGen     *snowflake.Node
	snowflakeHd    *snowflakealloc.Handle // 持有 worker id 的 etcd lease,进程退出时 Close
	// ownerEpochMintRedis 是 OwnerEpochMint 的生产实现本体,只为 Stop 时关闭它的专用连接池。
	ownerEpochMintRedis *RedisOwnerEpochMinter
	stopOnce            sync.Once
	// kafkaDeliveryFailures 统计 writer Completion 给出的终态失败。
	kafkaDeliveryFailures atomic.Uint64
}

func NewServiceContext(c config.Config) *ServiceContext {
	// 只读目录是必需依赖；在分配etcd租约/号段及连接其它后端前检查，缺密钥不带病发现。
	roleSecret := configuredRoleDirectorySecret(c)
	// 铸造落盘确认的取值越界同样在占用任何外部资源之前拒启(fail-closed,口径同 go/db 的 RedisClient.WaitAOF*)。
	if err := c.OwnerEpochMint.Validate(); err != nil {
		panic("scene_manager 配置非法: " + err.Error())
	}
	etcdCli, err := clientv3.New(clientv3.Config{
		Endpoints:   c.Etcd.Hosts,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		panic("failed to create etcd client: " + err.Error())
	}

	table.LoadTables(c.TableDir, c.UseBinary)

	// 通过 shared/snowflakealloc 申领一个独立于 NodeInfo.NodeId 的 Snowflake 槽位。
	// 关键点:
	//   - kind="scene-manager",槽池按 kind / cluster 隔离;LegacyPrefix="/scene_manager"
	//     让 cluster 0 在滚动升级期仍读旧布局的水位 / 活 id(发布一版后可删)。
	//   - 节点名 = hostname_监听端口,而不是裸 hostname:多副本部署后同一台机器
	//     (本地 dev 双实例、k8s 同节点多 pod 用 hostNetwork 等)会跑多个实例,
	//     端口是实例槽位的稳定标识,同槽位优雅重启仍复用同一个槽,本地缓存也各自一份;
	//     换 key 的首次启动会申领新槽(最久未用 + 隔离期),前任高水位地板保证不会撞 ID。
	//   - 这个槽与 noderegistry 里分配的 NodeInfo.NodeId **解耦**,
	//     reRegister 切换 NodeInfo.NodeId 不会影响 Snowflake ID 生成。
	//   - ClusterId 由运维按集群设定(默认 0),worker id = cluster<<12 | slot。
	host, err := os.Hostname()
	if err != nil {
		panic(fmt.Sprintf("snowflake: failed to get hostname: %v", err))
	}
	nodeName := host
	if i := strings.LastIndex(c.ListenOn, ":"); i >= 0 {
		nodeName = host + "_" + c.ListenOn[i+1:]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hd, err := snowflakealloc.AllocateWithKeepAlive(ctx, etcdCli, "scene-manager", nodeName, snowflakealloc.Options{
		LeaseTTL:     60,
		ClusterID:    c.ClusterId,
		LegacyPrefix: "/scene_manager",
		CachePath:    snowflakealloc.DefaultCachePath(c.SnowflakeCacheDir, "scene-manager", nodeName),
	})
	if err != nil {
		panic(fmt.Sprintf("snowflake worker id alloc failed: %v", err))
	}
	logx.Infof("[scene_manager] snowflake %s worker_id=%d (node=%s)", hd.LogFields(), hd.WorkerID, nodeName)

	sc := &ServiceContext{
		Config: c,
		Redis:  redis.MustNewRedis(c.Redis.RedisConf),
		Etcd:   etcdCli,
		// Handle.NewNode 会注入前任高水位作地板(见 snowflakealloc),
		// 裸 snowflake.NewNode 只有"不在构造秒发号"的点排除,顶不住时钟偏斜接管。
		SceneIDGen:  hd.NewNode(),
		snowflakeHd: hd,
	}
	sc.ownerEpochMintRedis = mustStartOwnerEpochMinter(c)
	sc.OwnerEpochMint = sc.ownerEpochMintRedis
	kafkaWriteTimeout := c.EffectiveKafkaWriteTimeout()
	sc.Kafka = &kafka.Writer{
		Addr: kafka.TCP(c.Kafka.Brokers...),
		// 控制面命令 topic(gate-cmd_gN)必须按 node_id % P 落到**指定分区**:
		// 消费端 assign 的就是那一个分区,落错分区 = 目标 gate 永远收不到,
		// 而 Kafka 一个错都不报(docs/design/control-plane-topic-partitioning-20260908.md)。
		// kafka-go 的 Writer 在写入路径上忽略 Message.Partition、只问 Balancer,
		// CommandPartitionBalancer 就是把它接回来的那一环。
		//
		// 其它 topic 仍走 Hash:Key 都是 player_id(项目不变量:kafka key = 业务实体 ID,
		// 同一玩家的事件必须有序),LeastBytes 按字节量选分区、完全忽略 Key ——
		// 玩家连续两次切场景的两条 RoutePlayer 会落到不同分区被乱序消费,最终应用旧路由。
		//
		// 命令 topic 上保序同样成立、而且更强:同一个 gate 的全部命令都在同一个分区里。
		Balancer:               &kafkacmd.CommandPartitionBalancer{Fallback: &kafka.Hash{}},
		AllowAutoTopicCreation: true,
		BatchTimeout:           10 * time.Millisecond,
		// WriteMessages 的调用 context 不可用超时取消：kafka-go 明确允许
		// context 返回后批次继续投递，那会让 EnterScene 回滚后 Gate 又收到
		// 旧路由。改由 writer 自身的一次有界 produce 决定终态。
		MaxAttempts:  1,
		WriteTimeout: kafkaWriteTimeout,
		// kafka.Writer 结构体的 RequiredAcks 零值是 RequireNone(协议级
		// fire-and-forget),显式提到 RequireOne:路由命令丢了玩家就进不去
		// 场景,只能靠客户端重试兜底。
		RequiredAcks: kafka.RequireOne,
		// 路由与重定向属于控制面提交点：只有 broker ACK 后 EnterScene 才能
		// 返回并缓存成功。Async=true 会让 WriteMessages 恒返回 nil，导致
		// broker 故障也被去重缓存成成功，故必须同步等待 RequireOne ACK。
		Async:      false,
		Completion: sc.handleKafkaCompletion,
	}
	rpc := newHomeZoneClient(c.DataServiceRpc)
	sc.HomeZone = rpc
	if rpc == nil {
		panic("scene_manager必须配置DataServiceRpc以强读原角色修订")
	}
	sc.RoleDirectory = &roleDirectoryClient{rpc: rpc, secret: roleSecret}
	return sc
}

// mustStartOwnerEpochMinter 建铸造专用客户端并做启动探针,任何一步失败都 panic(拒启)。
//
// 开启 WAITAOF 时先探一次:appendonly no、Redis < 7.2、NumReplicas 与拓扑不符都是确定性错误,留到运行期就是每一次
// 首登 / 换节点都因「铸造未落盘」回滚失败。两项都配 0 时不探、只打一行 ERROR:这时铸造出来的 epoch 没有落盘确认,
// Redis 崩溃 / 切主丢掉它会让现任持有者被当成废黜(设计 §3 player-save#2),只许没有 AOF 的环境这样配。
func mustStartOwnerEpochMinter(c config.Config) *RedisOwnerEpochMinter {
	minter, err := NewRedisOwnerEpochMinter(c.Redis.RedisConf, c.OwnerEpochMint, WallClockMs)
	if err != nil {
		panic("owner epoch mint client init failed: " + err.Error())
	}
	if !minter.WaitsForAOF() {
		logx.Errorf("[OwnerEpochMint] WAITAOF disabled (OwnerEpochMint.WaitAOFNumLocal=0 and WaitAOFNumReplicas=0): " +
			"minted owner epochs are routed without a durability confirmation")
		return minter
	}
	if err := minter.Probe(context.Background()); err != nil {
		_ = minter.Close()
		panic("owner epoch mint WAITAOF probe failed: " + err.Error())
	}
	logx.Infof("[OwnerEpochMint] WAITAOF enabled: numlocal=%d numreplicas=%d timeout=%s pool=%d",
		c.OwnerEpochMint.NumLocal(), c.OwnerEpochMint.NumReplicas(), c.OwnerEpochMint.EffectiveWaitAOFTimeout(), ownerEpochMintPoolSize)
	return minter
}

// newHomeZoneClient 按 DataServiceRpc 拨 data_service;没配目标(本地单区联调)返回 nil。
//
// 与 login 的 initHomeZoneResolver 同一判据(Etcd.Key / Endpoints / Target 任一非空
// 即视为已配置)。这里不 panic 也不降级成假客户端:nil 是一个显式状态,EnterScene
// 看到 nil 会按 gate zone 处理并 WARN 计数,多 zone 部署漏配能在指标上看见。
func newHomeZoneClient(c zrpc.RpcClientConf) dspb.DataServiceClient {
	if c.Etcd.Key == "" && len(c.Endpoints) == 0 && c.Target == "" {
		logx.Error("[home-zone] DataServiceRpc not configured: RoutePlayerEvent.home_zone_id will fall back to " +
			"the gate zone for every player (only acceptable for single-zone dev)")
		return nil
	}
	conn := zrpc.MustNewClient(c)
	return dspb.NewDataServiceClient(conn.Conn())
}

func (sc *ServiceContext) handleKafkaCompletion(messages []kafka.Message, err error) {
	if err == nil {
		metrics.ObserveKafkaDelivery("acked", len(messages))
		return
	}

	sc.kafkaDeliveryFailures.Add(uint64(len(messages)))
	metrics.ObserveKafkaDelivery("failed", len(messages))
	for _, m := range messages {
		// 同步 writer 会把该错误返给调用方；Completion 额外提供低基数指标
		// 与逐消息日志，但不在这里递归重试，避免 broker 故障形成热循环。
		logx.Errorf("[scene_manager] kafka delivery failed before EnterScene commit: topic=%s key=%s err=%v",
			m.Topic, string(m.Key), err)
	}
}

// KafkaDeliveryFailures 返回进程生命周期内的终态投递失败数。
// 这里只保留低基数计数；player id 进日志，不进入指标 label。
func (sc *ServiceContext) KafkaDeliveryFailures() uint64 {
	return sc.kafkaDeliveryFailures.Load()
}

// SnowflakeLost 在本进程不再持有 Snowflake worker id 的 etcd 租约时关闭。
// 此时 etcd 可以把同一个 worker id 分给别的进程,继续用 SceneIDGen 发号就是确定性撞号,
// 主流程必须停止服务。见 snowflakealloc.Handle.Lost 的说明。
func (sc *ServiceContext) SnowflakeLost() <-chan struct{} {
	if sc.snowflakeHd == nil {
		return nil
	}
	return sc.snowflakeHd.Lost()
}

// Stop 先关闭 Kafka writer并等待 Completion 返回；之后才释放 Snowflake
// worker lease。幂等保证
// 正常 defer 与失租强退路径可以安全共用。
func (sc *ServiceContext) Stop() {
	sc.stopOnce.Do(func() {
		if sc.Kafka != nil {
			if err := sc.Kafka.Close(); err != nil {
				logx.Errorf("[scene_manager] Kafka writer close/flush failed: %v", err)
			}
		}
		if failures := sc.KafkaDeliveryFailures(); failures > 0 {
			logx.Errorf("[scene_manager] stopping with %d terminal Kafka delivery failures", failures)
		}
		if sc.ownerEpochMintRedis != nil {
			if err := sc.ownerEpochMintRedis.Close(); err != nil {
				logx.Errorf("[OwnerEpochMint] mint redis client close failed: %v", err)
			}
		}
		if sc.snowflakeHd != nil {
			sc.snowflakeHd.Close()
			sc.snowflakeHd = nil
		}
	})
}
