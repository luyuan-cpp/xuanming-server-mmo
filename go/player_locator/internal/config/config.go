package config

import (
	"time"

	"github.com/zeromicro/go-zero/zrpc"
)

type Config struct {
	zrpc.RpcServerConf
	RedisClient     RedisConf          `json:"RedisClient"`
	Kafka           KafkaConf          `json:"Kafka"`
	Node            NodeConf           `json:"Node"`
	Registry        RegistryConf       `json:"Registry"`
	Lease           LeaseConf          `json:"Lease"`
	TableDir        string             `json:",default=../../generated/tables"`
	SceneManagerRpc zrpc.RpcClientConf `json:"SceneManagerRpc"` // SceneManager gRPC client (via etcd)

	// KillSwitchPrefix 是 RPC 级热关停规则在 etcd 里的前缀(shared/killswitch)。
	// 留空即用 killswitch.DefaultPrefix(/mmorpg/killswitch/)—— 这是安全默认值:
	// 前缀下没有任何 key 就是全部放行。只有多套环境共用一个 etcd 集群、需要各自
	// 独立的止血阀时才需要改它。
	KillSwitchPrefix string `json:"KillSwitchPrefix,optional"`
}

type RedisConf struct {
	Host     string `json:"Host"`
	Password string `json:"Password"`
	DB       int    `json:"DB"`
}

type KafkaConf struct {
	Brokers []string `json:"Brokers"`
}

type NodeConf struct {
	ZoneId   uint32 `json:"ZoneId"`
	LeaseTTL int64  `json:"LeaseTTL"` // etcd lease TTL (seconds)
}

type RegistryConf struct {
	Etcd EtcdConf `json:"Etcd"`
}

type EtcdConf struct {
	Hosts       []string      `json:"Hosts"`
	DialTimeout time.Duration `json:"DialTimeout"`
}

type LeaseConf struct {
	DefaultTTLSeconds uint32        `json:"DefaultTTLSeconds"` // disconnect lease, default 30
	PollInterval      time.Duration `json:"PollInterval"`      // lease monitor poll interval, default 1s
	BatchSize         int           `json:"BatchSize"`         // max expired leases per tick, default 100
	// ReconcileIntervalSeconds 是会话对账扫描的周期(默认 60,0=默认,-1=关闭)。
	// 对账扫描兜住"gate 整机崩溃 → 断线回调不执行 → 会话永久 ONLINE"的缺口:
	// State==ONLINE 且 gate_instance_id 连续两轮不在 etcd 存活集内的会话,
	// 补投 DISCONNECTING + 租约,恢复「所有会话终点必经租约链」的闭环。
	ReconcileIntervalSeconds int64 `json:"ReconcileIntervalSeconds,optional"`
}

var AppConfig Config
