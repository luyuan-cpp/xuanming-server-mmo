package svc

import (
	"context"
	"fmt"
	"os"
	"scene_manager/internal/config"
	"time"

	"shared/generated/table"
	"shared/snowflake"
	"shared/snowflakealloc"

	"github.com/segmentio/kafka-go"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type ServiceContext struct {
	Config      config.Config
	Redis       *redis.Redis
	Kafka       *kafka.Writer
	Etcd        *clientv3.Client
	SceneIDGen  *snowflake.Node
	snowflakeHd *snowflakealloc.Handle // 持有 worker id 的 etcd lease,进程退出时 Close
}

func NewServiceContext(c config.Config) *ServiceContext {
	etcdCli, err := clientv3.New(clientv3.Config{
		Endpoints:   c.Etcd.Hosts,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		panic("failed to create etcd client: " + err.Error())
	}

	table.LoadTables(c.TableDir, c.UseBinary)

	// 通过 shared/snowflakealloc 拿一个独立于 NodeInfo.NodeId 的 Snowflake worker id。
	// 关键点:
	//   - prefix="/scene_manager" 与历史 key 完全一致(老的 mustAllocNodeID 用的就是这套路径),
	//     所以同 hostname 重启仍然复用同一个 worker id —— 不破坏现网数据。
	//   - 这个 worker id 与 noderegistry 里分配的 NodeInfo.NodeId **解耦**,
	//     reRegister 切换 NodeInfo.NodeId 不会影响 Snowflake ID 生成。
	host, err := os.Hostname()
	if err != nil {
		panic(fmt.Sprintf("snowflake: failed to get hostname: %v", err))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hd, err := snowflakealloc.AllocateWithKeepAlive(ctx, etcdCli, "/scene_manager", host, snowflakealloc.Options{LeaseTTL: 60})
	if err != nil {
		panic(fmt.Sprintf("snowflake worker id alloc failed: %v", err))
	}
	logx.Infof("[scene_manager] snowflake worker id = %d (host=%s)", hd.WorkerID, host)

	return &ServiceContext{
		Config: c,
		Redis:  redis.MustNewRedis(c.Redis.RedisConf),
		Kafka: &kafka.Writer{
			Addr:                   kafka.TCP(c.Kafka.Brokers...),
			Balancer:               &kafka.LeastBytes{},
			AllowAutoTopicCreation: true,
			BatchTimeout:           10 * time.Millisecond,
			WriteTimeout:           1 * time.Second,
			Async:                  true,
		},
		Etcd:        etcdCli,
		SceneIDGen:  snowflake.NewNode(hd.WorkerID),
		snowflakeHd: hd,
	}
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

// Stop 释放 Snowflake worker id 的 etcd lease(以及 KeepAlive goroutine)。
// scene_manager 主流程在退出时应当调用,否则 worker id 要等 lease TTL 自然过期才能复用。
func (sc *ServiceContext) Stop() {
	if sc.snowflakeHd != nil {
		sc.snowflakeHd.Close()
		sc.snowflakeHd = nil
	}
}

