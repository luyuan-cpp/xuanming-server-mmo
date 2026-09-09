package config

import (
	"time"

	"github.com/zeromicro/go-zero/zrpc"

	"shared/idsegment"
)

type Config struct {
	zrpc.RpcServerConf
	RedisClient        RedisConf    `json:"RedisClient"`
	PlayerLocatorRedis RedisConf    `json:"PlayerLocatorRedis"`
	MySQL              MySQLConf    `json:"MySQL"`
	Node               NodeConf     `json:"Node"`
	Registry           RegistryConf `json:"Registry"`
	Cache              CacheConf    `json:"Cache"`
	Kafka              KafkaConf    `json:"Kafka"`
	TableDir           string       `json:",default=../../generated/tables"`

	// MergeMarkerRedis 指向 data_service 的 **mapping Redis**(player:zone:{id} 所在的
	// 那个实例/DB),guild 只从里面读一把键:merge:in_progress:{zone}。
	// 契约见 go/data_service/internal/routing/router.go 顶部的「合服闸门」注释:
	// 键存在即该 zone 正在合服,值(JSON)与 TTL 都不解析,由 tools/merge_zone 写入与删除。
	//
	// **整段可缺失 = 闸门不生效**(CreateGuild 不做这项检查),启动时会打一条
	// "MergeMarkerRedis 未配置" 的 Info 日志。这是刻意的:guild 与 data_service 目前没有
	// 任何连线,强制它连一个新 Redis 会让所有还没配这段的环境直接起不来;而闸门本身
	// 是合服维护窗口里的加固,不是常态运行的必需品。真要在合服里用,就必须配上 ——
	// 没配的后果是维护窗口内玩家仍能在源 zone 建帮,那个公会不会被 merge_zone 搬走。
	MergeMarkerRedis MergeMarkerRedisConf `json:"MergeMarkerRedis,optional"`

	// DataServiceRpc 是 data_service 的 gRPC 客户端(etcd 发现,Key=dataservice.rpc),
	// 与 login 的 PlayerLocatorRpc / SceneManagerRpc 同一套 zrpc+etcd 写法。
	// guild 目前只用它领 guild_id 号段(AllocateIdSegment);IdSegment.Enabled=false 时
	// 根本不拨号,所以标 optional —— 但 Enabled=true 而这块缺失会在启动时明确拒绝。
	DataServiceRpc zrpc.RpcClientConf `json:"DataServiceRpc,optional"`
	// IdSegment 控制 guild_id 的号段发号(docs/design/node-id-overhaul-plan-20260908.md §6)。
	// 字段语义见 shared/idsegment.Conf。**没写这块 = 号段关闭 = 纯 snowflake 老路径**
	// (设计稿 §6.4「保留一个版本做回滚开关」)。
	IdSegment idsegment.Conf `json:"IdSegment,optional"`

	// KillSwitchPrefix 是 RPC 级热关停规则在 etcd 里的前缀(shared/killswitch)。
	// 留空即用 killswitch.DefaultPrefix(/mmorpg/killswitch/)—— 这是安全默认值:
	// 前缀下没有任何 key 就是全部放行。只有多套环境共用一个 etcd 集群、需要各自
	// 独立的止血阀时才需要改它。
	KillSwitchPrefix string `json:"KillSwitchPrefix,optional"`

	// ClusterId 是部署级集群号:snowflake 17 位 worker 段的 [cluster5] 部分
	// (docs/design/node-id-overhaul-plan-20260908.md §5)。**运维按集群一次性设定,
	// 策划不碰**;默认 0 = 单集群 / 存量 id 布局。etcd 槽位前缀带 c<cluster>,两个集群
	// 共用一个 etcd 也不会撞号。必须 < 32。
	ClusterId uint32 `json:"ClusterId,default=0"`

	// SnowflakeCacheDir 是 snowflake 槽位的本地缓存目录(shared/snowflakealloc,设计稿 §3.5):
	// 启动时 etcd 不可达、且缓存里的上次水位确认在 F(2h)内,就用缓存的槽起服并后台重试
	// 注册。留空关闭。默认相对服务工作目录(go/guild/)指向仓库 run/(已 gitignore);
	// k8s 想要"etcd 不通也能起"就挂一个持久卷进来,写失败只告警不影响正确性。
	SnowflakeCacheDir string `json:"SnowflakeCacheDir,default=../../run/snowflake"`
}

type RedisConf struct {
	Host     string `json:"Host"`
	Password string `json:"Password"`
	DB       int    `json:"DB"`
}

// MergeMarkerRedisConf 是合服标记所在 Redis 的连接参数。
//
// 字段单独定义而不复用上面的 RedisConf,只为把 optional 语义钉死在类型上:
// 这三个键**全部** optional,Host 为空就是"没配",而 RedisConf 的 Host 是必填。
// 密码字段叫 Pass(与 go-zero redis.RedisConf 同名),因为运维填这段时对着的是
// data_service 的 MappingRedis 那份 yaml。
type MergeMarkerRedisConf struct {
	Host string `json:"Host,optional"`
	Pass string `json:"Pass,optional"`
	DB   int    `json:"DB,optional"`
}

// Enabled 报告合服闸门是否配置了。Host 是唯一判据:没有地址就连不上,
// 其余两项都有合法的零值(无密码、DB 0)。
func (c MergeMarkerRedisConf) Enabled() bool {
	return c.Host != ""
}

type MySQLConf struct {
	DataSource string `json:"DataSource"`
}

type NodeConf struct {
	ZoneId   uint32 `json:"ZoneId"`
	LeaseTTL int64  `json:"LeaseTTL"`
}

type RegistryConf struct {
	Etcd EtcdConf `json:"Etcd"`
}

type EtcdConf struct {
	Hosts       []string      `json:"Hosts"`
	DialTimeout time.Duration `json:"DialTimeout"`
}

type CacheConf struct {
	DefaultTTL time.Duration `json:"DefaultTTL"`
	MaxMembers uint32        `json:"MaxMembers"`
}

type KafkaConf struct {
	Brokers []string `json:"Brokers"`
}

var AppConfig Config
