package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"shared/dbplacement"
	"shared/dirshard"
	"shared/kafkautil"
	"shared/playerredis"
	"shared/redisauth"
	"shared/rolefence"
	"shared/secretenv"

	"github.com/zeromicro/go-zero/core/discov"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

type Config struct {
	zrpc.RpcServerConf
	// NameCacheRedis 只放可丢弃的读缓存(player:name:{id},routing.PlayerNameCache):里面没有任何权威数据,
	// 整个实例丢了,名字读路径回源一次目录,建角和归属都不受影响。不要往上面加要落盘或要保证一致的东西。
	NameCacheRedis redis.RedisConf

	// Snapshot MySQL(player_snapshot / transaction_log 审计库)
	SnapshotMySQL SnapshotMySQLConfig `json:",optional"`
	// 账号目录全局规则；各 zone 的 login 不能抬高这里的角色上限。
	AccountDirectory AccountDirectoryConfig `json:",optional"`
	// Directory 是目录分片和存档库连接池,见 DirectoryConfig。
	Directory DirectoryConfig `json:",optional"`
	// RoleFence 是角色护栏协调者的配置,见 RoleFenceConfig。
	RoleFence RoleFenceConfig `json:",optional"`
	// Recall 是 GM 批量回收的资产通道,见 RecallConfig。
	Recall RecallConfig `json:",optional"`
	// SharedRedis 是控制域:落地唤醒频道 assetop:wake 和去重键,必须和 scene 发布的是同一实例。实例指错的表现是永远收不到唤醒,且不报错。
	// 只有 Recall.Enabled=true 时才连。
	SharedRedis SharedRedisConf `json:",optional"`
	// SharedRedisPasswordEnv 是 NameCacheRedis 与 SharedRedis 的口令所在的环境变量名(shared/redisauth)。K8s 上口令只在
	// Secret redis-auth 里,经 secretKeyRef 注入;ConfigMap 里两段的口令写空串。不写 = 照用字面量(本机)。
	// 由 ResolveSharedRedisPassword 在常驻启动时填好,变量缺失或为空即拒启。
	SharedRedisPasswordEnv string `json:",optional"`
	// MySQLPasswordEnv 是 SnapshotMySQL 口令所在的环境变量名(docs/design/workload-secrets-via-k8s-secret.md §4.2)。K8s 上只有部署脚本
	// 渲染的 ConfigMap go-svc-data-service-config 写它:那份 ConfigMap 由 route-tool Job 直接挂载,口令经 Secret mysql-auth 注入。
	// 写者常驻进程与 -migrate 读运维预置 Secret 里的配置,口令写在 SnapshotMySQL.Password 里、不写本键。
	// 写了它,SnapshotMySQL.Password 必须写成空串(不写这一键会落回缺省值 apppass123,被当成字面量而拒绝);不写 = 照用字面量(本机)。
	// 由 ResolveMySQLPassword 在加载配置后、任何入口连库之前填好,变量缺失或为空即拒启。
	MySQLPasswordEnv string `json:",optional"`
	// PlayerRedis 是玩家域:读位置键 player:{id}:location。没写 Addrs 时指向 SharedRedis。只有 Recall.Enabled=true 时才连。
	PlayerRedis playerredis.Config `json:",optional"`

	// AdminToken 是运维专用 RPC 的共享口令,调用方放在 gRPC metadata 的 `x-admin-token` 里。
	//
	// 现在只守 ReleasePlayerName 的运维路径(不限登记时间的释放)。
	//
	// **空 = 该 RPC 整体停用**(fail closed),不是"免鉴权"。理由:data_service 在集群
	// 内网是无鉴权可达的,任何一个能拨到它的进程都能调;默认敞开意味着一次误配
	// 或一个跑错环境的脚本就能释放任意玩家的名字。所以默认值只能是"不能用",
	// 启用是一次显式的运维动作:维护窗口里临时下发,窗口结束就撤掉。
	//
	// 不放进 yaml 明文的话可以用 k8s Secret 挂成环境变量再由 ConfigMap 引用。
	AdminToken string `json:",optional"`

	// Prometheus /metrics HTTP listen address (host:port). Empty = disabled.
	// Scraped by Prometheus alongside scene_manager.
	// See go/data_service/internal/metrics/metrics.go for the metric list.
	MetricsListenAddr string `json:",optional"`

	// Schema 控制目录各片和流水库各表的建表/补列路径:proto 驱动的流水表 + 走手写 DDL、proto 只作列漂移守卫的目录表和路由表,
	// 见 internal/store/schema.go。
	// 整段可缺失,缺失语义见 SchemaConfig.AutoMigrate 与 ShouldAutoMigrate。
	Schema SchemaConfig `json:",optional"`

	// Store 见 StoreConfig。
	Store StoreConfig `json:",optional"`

	// RouteStore 是全局库 player_route 连接池与合服映射缓存的运行参数,见 RouteStoreConfig。整段可缺失。
	RouteStore RouteStoreConfig `json:",optional"`

	// Kafka 是 C++ scene 产出的 transaction_log_topic / player_snapshot_topic 的落库消费者
	// 配置。Brokers 为空 = 不消费(本地无 Kafka 时的合法形态);消费者启动失败不会拖死
	// data_service(Load/Save 是热路径),只记日志并每 30s 后台重试。
	Kafka KafkaConfig `json:",optional"`

	// AuditRetention 是 player_snapshot、transaction_log 的保留期。不配就用默认值。
	AuditRetention AuditRetentionConfig `json:",optional"`

	// IdSegment 是发号器(AllocateIdSegment)的配置,见 internal/idissuer 与
	// docs/design/player-data-tidb-scaling-plan.md §6。整段可缺失,缺失 = 生产安全值。
	IdSegment IdSegmentConfig `json:",optional"`

	// PlayerName 是玩家名字注册表(全服唯一)的运行参数,见 internal/store/player_name_store.go
	// 与 internal/logic/player_name_logic.go。整段可缺失:每个键的零值都当"没配",
	// 由 PlayerNameConfig.Normalize() 填上文档化的默认值(装配在 svc.NewServiceContext)。
	PlayerName PlayerNameConfig `json:",optional"`

	// ZoneId 只用于 C++ 约定的 etcd 注册路径
	// (DataServiceNodeService.rpc/zone/<ZoneId>/node_type/26/node_id/<N>,见 internal/noderegistry)。
	// data_service 按 zone namespace 各部署一份,但 DataServiceNodeService **不是** zone-scoped
	// 节点类型(C++ 发现侧只有 Gate/Scene/Login/PlayerLocator 按 zone 过滤),所以填哪个 zone
	// 都能被任何 scene 发现;写成 Pod 所在的 zone 只是为了排障时一眼对得上。
	// optional、默认 0:整段缺失也能注册(与 scene_manager / match 的 default=1 不同 —— 那两个
	// 靠它区分本地 dev-start-zones 每 zone 起的一份)。k8s ConfigMap 写部署 zone(${CurrentZoneId})。
	ZoneId uint32 `json:",optional"`

	// LeaseTTL 是 C++ 约定注册那把 etcd 租约的 TTL(秒),keepalive 按它续租;
	// 与 scene_manager / match 同形(顶层键、默认 60)。租约丢失 = C++ 侧看到 data_service
	// 消失,scene 的号段续段会失败(手里两段能顶 10~20 分钟),noderegistry 会 CAS 重夺 / 重分配 node_id。
	LeaseTTL int64 `json:",default=60"`
}

type AccountDirectoryConfig struct {
	MaxPlayersPerAccount int `json:",optional"`
	// 必须显式配置；使用同 TiDB 权威池跨 schema 强读，不新配另一份资产源。
	RoleTradeSQLProof RoleTradeSQLProofConfig `json:",optional"`
	// MaxOpenConn 是账号目录独立 SQL 池(store.NewAccountDirectoryStore → authoritysql.Open)的连接上限,经 PoolMaxOpenConn 取用。
	// 0 / 缺省 = DefaultAccountDirectoryMaxOpenConn;合法区间 [1, MaxAccountDirectoryMaxOpenConn]。越界不纠正成默认值而是不装配目录
	// (理由同 PlayerNameConfig.Validate):MissingStores 带上 account_directory,全球服务恒为 Store.Required=true,main 拒启。
	// 每次登录的 ReadAccountDirectory 都在这个池上开一个悲观事务,登录并发上来时它是第一个排队点,所以做成旋钮。
	// 它与 SnapshotMySQL / PlayerName / RouteStore 的池一样连同一台全局库、计入连接总账(每个副本多占 MaxOpenConn 条):
	// 调大前按 tools/scripts/k8s_deploy.ps1 的连接总账核对 max_connections,部署脚本从本键读取。
	MaxOpenConn int `json:",optional"`
}

// 账号目录池上限的缺省与边界(见 AccountDirectoryConfig.MaxOpenConn)。
const (
	// DefaultAccountDirectoryMaxOpenConn 沿用改成可配置之前写死在构造点的值。
	DefaultAccountDirectoryMaxOpenConn = 8
	// MaxAccountDirectoryMaxOpenConn = shared/authoritysql.Open 对单池的硬上限:更大的值那边一律拒绝开池,
	// 在这里先拒,启动日志能直接指到配置键。那边放宽时这里同步。
	MaxAccountDirectoryMaxOpenConn = 128
)

// PoolMaxOpenConn 返回账号目录池的实际上限:0 = DefaultAccountDirectoryMaxOpenConn,负数或超过
// MaxAccountDirectoryMaxOpenConn 返回错误(调用方不装配目录)。
func (c AccountDirectoryConfig) PoolMaxOpenConn() (int, error) {
	if c.MaxOpenConn == 0 {
		return DefaultAccountDirectoryMaxOpenConn, nil
	}
	if c.MaxOpenConn < 1 || c.MaxOpenConn > MaxAccountDirectoryMaxOpenConn {
		return 0, fmt.Errorf("AccountDirectory.MaxOpenConn must be in [1, %d] (0 = default %d), got %d",
			MaxAccountDirectoryMaxOpenConn, DefaultAccountDirectoryMaxOpenConn, c.MaxOpenConn)
	}
	return c.MaxOpenConn, nil
}

// DirectoryConfig 是目录分片(docs/design/horizontal-scale-logical-db.md §5.1)和存档库连接池(§6)。
type DirectoryConfig struct {
	// LogicalShards 是目录分片数 K,必须是 64 的约数。0 = 1。生产首次上线定 64,开发定 1。
	// 上线后不能改:每片的 directory_shard 记着 K,配错拒启。
	LogicalShards int `json:",optional"`
	// StoragePools 是连每个存档库的连接池,按需打开。
	StoragePools StoragePoolConfig `json:",optional"`
}

// StoragePoolConfig 零值按 store.NewPlayerStorePools 的缺省。这些连接计入每个存档库的连接总账。
type StoragePoolConfig struct {
	MaxOpenConn        int `json:",optional"`
	MaxIdleConn        int `json:",optional"`
	ConnMaxIdleSeconds int `json:",optional"`
}

// Shards 是实际的分片数:0 按 1。合法性由 dirshard.Validate 判。
func (d DirectoryConfig) Shards() int { return dirshard.Normalize(d.LogicalShards) }

type RoleTradeSQLProofConfig struct {
	Enabled           bool     `json:",optional"`
	AllowedStorageIDs []uint32 `json:",optional"`
	// 与go/db已有独立库白名单共源；空值使用DB_ALLOWED_DATABASES。没有配置绝不按Regions/当前Zone推导。
	AllowedDatabasesEnv string `json:",optional"`
}

func (c AccountDirectoryConfig) MaxRoles() int {
	if c.MaxPlayersPerAccount == 0 {
		return 5
	}
	return c.MaxPlayersPerAccount
}

// RecallConfig 是 GM 批量回收的资产通道(docs/design/exploit_loss_prevention_zh.md)。
//
// Enabled=false:预览照常,执行回 ErrCodeRecallDisabled,不连 Redis、不起循环。已有的未结束行留在库里,护栏照样挡过户。
// Enabled=true:依赖装配失败就拒启。资产路径没有降级:签名密钥 MMORPG_ASSET_OP_SECRET_DATA_SERVICE 必须和 scene 同值。
type RecallConfig struct {
	Enabled bool `json:",optional"`
}

// SharedRedisConf 是 SharedRedis 的连接参数。
type SharedRedisConf struct {
	Host     string `json:",optional"`
	Password string `json:",optional"`
	DB       int    `json:",optional"`
	// PoolSize 是连接池上限。0 / 不写 = go-redis 缺省 10 × GOMAXPROCS。
	PoolSize int `json:",optional"`
}

// recallPlayerRedisFallbackPoolSize、IOTimeoutMs:没写 PlayerRedis.Addrs 且 SharedRedis.PoolSize 也没写时的取值,和 trade 同口径。
const (
	recallPlayerRedisFallbackPoolSize    = 20
	recallPlayerRedisFallbackIOTimeoutMs = 3000
)

// ValidateRecall 只在 Recall.Enabled 时要求配置齐全。
func (c Config) ValidateRecall() error {
	if !c.Recall.Enabled {
		return nil
	}
	if c.SharedRedis.Host == "" {
		return errors.New("SharedRedis.Host is required when Recall.Enabled=true")
	}
	if c.SharedRedis.PoolSize < 0 {
		return fmt.Errorf("SharedRedis.PoolSize must not be negative, got %d", c.SharedRedis.PoolSize)
	}
	return nil
}

// EffectivePlayerRedis 返回生效的玩家域配置:写了 PlayerRedis.Addrs 就用它,否则指向 SharedRedis 那一个实例。
func (c Config) EffectivePlayerRedis() playerredis.Config {
	pool := c.SharedRedis.PoolSize
	if pool == 0 {
		pool = recallPlayerRedisFallbackPoolSize
	}
	return c.PlayerRedis.OrDefault(playerredis.Config{
		Addrs:           []string{c.SharedRedis.Host},
		Password:        c.SharedRedis.Password,
		PoolSizePerNode: min(pool, playerredis.MaxPoolSizePerNode),
		ReadTimeoutMs:   recallPlayerRedisFallbackIOTimeoutMs,
		WriteTimeoutMs:  recallPlayerRedisFallbackIOTimeoutMs,
	})
}

// ResolveSharedRedisPassword 按 SharedRedisPasswordEnv 填 NameCacheRedis.Pass 与 SharedRedis.Password,返回填好的副本;规则见 redisauth.Apply。
// 玩家域没写 Addrs 时回落到 SharedRedis(EffectivePlayerRedis),填完之后用的也是这个口令。
// -migrate 与发号器运维入口不连 Redis,只在常驻启动时调用(data_service.go)。
func (c Config) ResolveSharedRedisPassword(lookup func(string) (string, bool)) (Config, error) {
	err := redisauth.Apply(c.SharedRedisPasswordEnv, lookup,
		redisauth.Target{Field: "NameCacheRedis.Pass", Password: &c.NameCacheRedis.Pass},
		redisauth.Target{Field: "SharedRedis.Password", Password: &c.SharedRedis.Password})
	if err != nil {
		return Config{}, err
	}
	return c, nil
}

// ResolveMySQLPassword 按 MySQLPasswordEnv 填 SnapshotMySQL.Password,返回填好的副本;规则见 secretenv.Fill。
// 每个读配置后要连库的入口都调用:data_service.go 在 conf.MustLoad 之后、分流到 -migrate 与各运维入口之前;
// -route-tool 自己读配置(routetoolcli),在它 conf.Load 之后。
func (c Config) ResolveMySQLPassword(lookup func(string) (string, bool)) (Config, error) {
	err := secretenv.Fill("MySQLPasswordEnv", c.MySQLPasswordEnv, lookup,
		secretenv.Target{Field: "SnapshotMySQL.Password", Value: &c.SnapshotMySQL.Password})
	if err != nil {
		return Config{}, err
	}
	return c, nil
}

// RoleFenceConfig 是角色护栏协调者的配置(docs/design/horizontal-scale-logical-db.md §3.5)。
type RoleFenceConfig struct {
	// Participants 必须逐个对上 rolefence.Participants。少配一个就是漏冻结,所以不是「配了几个问几个」。
	// 整段缺失或不合法时不装配协调者,过户和删角全部拒绝。
	Participants []RoleFenceParticipantConfig `json:",optional"`
}

// RoleFenceParticipantConfig 是一个参与者的地址:Endpoints 直连,或 Etcd 服务发现,二选一。
// Etcd 不用 inherit:缺地址时继承顶层 Etcd 会解析到 data_service 自己。
type RoleFenceParticipantConfig struct {
	Name      string
	Endpoints []string        `json:",optional"`
	Etcd      discov.EtcdConf `json:",optional"`
}

// Validate 核参与者清单。
func (c RoleFenceConfig) Validate() error {
	seen := make(map[string]bool, len(c.Participants))
	for _, p := range c.Participants {
		if seen[p.Name] {
			return fmt.Errorf("RoleFence.Participants: %q listed twice", p.Name)
		}
		seen[p.Name] = true
		if hasEtcd := len(p.Etcd.Hosts) > 0 && p.Etcd.Key != ""; hasEtcd == (len(p.Endpoints) > 0) {
			return fmt.Errorf("RoleFence.Participants: %q needs exactly one of Endpoints and Etcd", p.Name)
		}
	}
	for _, name := range rolefence.Participants {
		if !seen[name] {
			return fmt.Errorf("RoleFence.Participants: %q is missing", name)
		}
	}
	if len(c.Participants) != len(rolefence.Participants) {
		return fmt.Errorf("RoleFence.Participants: must list exactly %v", rolefence.Participants)
	}
	return nil
}

// IdSegmentConfig 是发号器的配置。
type IdSegmentConfig struct {
	// AllowAutoSeed=true:登记处没有这个码、或者本地记录缺失时,第一次预留自动补齐(只给 dev 用,空环境起了就能建角);
	// false(生产):拒绝发号,返回 ErrCodeIdSegmentUnknownTag,不写任何记录。新地域由部署步骤
	// `data_service -id-issuer-init` 显式初始化,事故由 `-id-issuer-recover` 恢复(docs/ops/id-issuer-runbook.md)。
	//
	// 为什么生产不能自动初始化:记录不存在,要么是新地域,要么是 etcd 丢了数据。后者如果自动从头发号,
	// 新号可能和存量撞上,而撞号多数不报错,只会静默串档。这个判断要留给人,恢复只按幸存的记录取上界。
	//
	// 这里可以放心用裸 bool:go-zero 不下钻一个"整段 optional 且未出现"的嵌套结构,
	// 里面的 default 标签一个都不会回填 —— 但本键的安全值恰好就是零值 false,块缺失
	// 得到的正是生产语义(与 SchemaConfig.AutoMigrate 需要 *bool 的原因相反)。
	AllowAutoSeed bool `json:",optional"`

	// Tags 是允许发号的 biz_tag 清单。为空 = DefaultIdSegmentTags(原因同上:块缺失时
	// default 标签不生效,只能在 EffectiveTags 里兜)。新增一种永久身份 = 在这里加一个 tag。
	// 所有种类共用一个计数器,清单只用来拦住拼错的 tag。
	Tags []string `json:",optional"`

	// Issuer 是预留记录的位置。
	Issuer IdIssuerConfig `json:",optional"`
}

// IdIssuerConfig 是发号器预留记录的位置。三份记录都只增不减,一块号要三份都写成功才发,
// 所以任何一份或两份丢失、回退,剩下的都盖得住发过的号(docs/design/player-data-tidb-scaling-plan.md §6.3)。
type IdIssuerConfig struct {
	// Region 是地域码,0..63,写进计数器的最高位。0 只给 Mode=dev、test:这个字段漏写也是 0,
	// 其他模式拒绝启动。开始发号之后不能改;登记处只认它绑定的那两套 etcd,记录里的地域和这里不一致也拒绝预留。
	Region uint32 `json:",optional"`

	// PrimaryEtcd / SecondaryEtcd 是放两份本地记录的两套 etcd。生产必须各配一套,地址不能有交集:
	// 两份记录在同一套 etcd 上就失去了意义。dev(AllowAutoSeed=true)可以不写,回落到服务自己的
	// Etcd.Hosts,两份记录放同一套 etcd 的不同键。目前都不开认证,与现有 etcd 部署一致。
	PrimaryEtcd   []string `json:",optional"`
	SecondaryEtcd []string `json:",optional"`

	// RegistryEtcd 是全局号源登记处所在的 etcd:全部地域、全部物理集群、staging 和 prod 共用一套,
	// 地域码绑定和第三份预留记录都在这里。生产必须配,地址不能和 PrimaryEtcd、SecondaryEtcd 有交集。
	// dev 可以不写,回落到 PrimaryEtcd(再回落到 Etcd.Hosts),放不同的键。
	RegistryEtcd []string `json:",optional"`

	// RegistryTLS 是连登记处的双向 TLS(docs/ops/id-issuer-runbook.md §1)。登记处跨集群经私网访问:
	// 它只收世界 CA 签的客户端证书;发号器也只认世界 CA 签的服务端证书,连到别的环境误起的登记处,握手就失败。
	// 生产(AllowAutoSeed=false)三项必填;dev、test 的登记处在自己的 etcd 上,不开 TLS,三项必须为空(RegistryTLSFiles)。
	RegistryTLS RegistryTLSConfig `json:",optional"`
}

// RegistryTLSConfig 是登记处双向 TLS 的三个 PEM 文件:世界 CA、本地域的客户端证书和私钥。
// K8s 上由 Secret id-registry-client-tls 挂到 /etc/mmorpg/id-registry-tls/(ca.crt、tls.crt、tls.key)。
type RegistryTLSConfig struct {
	CAFile   string `json:",optional"`
	CertFile string `json:",optional"`
	KeyFile  string `json:",optional"`
}

// RegistryTLSFiles 核对登记处的 TLS 配置并返回三个路径。dev、test(AllowAutoSeed=true)返回空值:不开 TLS。
func (c IdSegmentConfig) RegistryTLSFiles() (RegistryTLSConfig, error) {
	files := c.Issuer.RegistryTLS
	set := 0
	for _, path := range []string{files.CAFile, files.CertFile, files.KeyFile} {
		if strings.TrimSpace(path) != "" {
			set++
		}
	}
	if c.AllowAutoSeed {
		if set != 0 {
			return RegistryTLSConfig{}, errors.New("IdSegment.Issuer.RegistryTLS must be empty when AllowAutoSeed=true: " +
				"a dev or test id registry is a key on its own plain etcd")
		}
		return RegistryTLSConfig{}, nil
	}
	if set != 3 {
		return RegistryTLSConfig{}, errors.New("IdSegment.Issuer.RegistryTLS needs CAFile, CertFile and KeyFile when AllowAutoSeed=false: " +
			"the shared id registry only accepts mutual TLS (docs/ops/id-issuer-runbook.md §1)")
	}
	return files, nil
}

// DefaultIdSegmentTags 是走号段的永久身份清单。
// 要与 etc/data_service.yaml 的 IdSegment.Tags、k8s_deploy.ps1 data-service ConfigMap 同步。
var DefaultIdSegmentTags = []string{"player", "guild", "item", "txlog", "snapshot", "trade_listing", "guild_asset_op", "friend_request", "trade_asset_op", "mail", "mail_asset_op", "rank_board", "rank_settlement", "rank_reward", "battle", "snapshot_row", "rollback_audit", "gm_recall_op"}

// EffectiveTags 是实际允许的 tag 清单:没配 = 默认清单。
// 返回副本,免得调用方改到包级默认值。
func (c IdSegmentConfig) EffectiveTags() []string {
	src := c.Tags
	if len(src) == 0 {
		src = DefaultIdSegmentTags
	}
	out := make([]string, len(src))
	copy(out, src)
	return out
}

// IssuerEndpoints 返回两份本地记录和登记处各自的 etcd 地址。fallback 是服务自己的 Etcd.Hosts,只有 dev 才能回落到它。
// 生产三组地址两两不能有交集;这只比字符串,运行期发号器再比 etcd 自己报的集群号。
func (c IdSegmentConfig) IssuerEndpoints(fallback []string) (primary, secondary, registry []string, err error) {
	primary, secondary, registry = c.Issuer.PrimaryEtcd, c.Issuer.SecondaryEtcd, c.Issuer.RegistryEtcd
	if c.AllowAutoSeed {
		// AllowAutoSeed 跳过独立性和地域码绑定的核对,登记处没有这个码就在第一次预留时直接认领,不走「下一个可认领的码」。
		// 这样的发号器连上共用登记处,就成了在用地域码的第二个发号环境(交接 docs/handoff/global-id-issuer-registry-20261105.md §7.3),
		// 所以 dev、test 的登记处只能放在自己的 etcd 上,显式给地址一律拒绝。部署脚本的 dev 档同样拒绝 -IdRegistryEtcd。
		if len(registry) != 0 {
			return nil, nil, nil, errors.New("IdSegment.Issuer.RegistryEtcd must be empty when AllowAutoSeed=true: " +
				"a dev or test issuer skips the region code binding checks, so its id registry stays on its own etcd and never on the shared one")
		}
		if len(primary) == 0 {
			primary = fallback
		}
		if len(secondary) == 0 {
			secondary = primary
		}
		registry = primary
		if len(primary) == 0 {
			return nil, nil, nil, errors.New("IdSegment.Issuer: no etcd endpoints (neither PrimaryEtcd nor Etcd.Hosts is set)")
		}
		return primary, secondary, registry, nil
	}
	if len(primary) == 0 || len(secondary) == 0 || len(registry) == 0 {
		return nil, nil, nil, errors.New("IdSegment.Issuer: PrimaryEtcd, SecondaryEtcd and RegistryEtcd are all required when AllowAutoSeed=false")
	}
	owner := make(map[string]string, len(primary)+len(secondary)+len(registry))
	for _, group := range []struct {
		name      string
		endpoints []string
	}{{"PrimaryEtcd", primary}, {"SecondaryEtcd", secondary}, {"RegistryEtcd", registry}} {
		for _, endpoint := range group.endpoints {
			key := strings.ToLower(strings.TrimSpace(endpoint))
			if other, shared := owner[key]; shared && other != group.name {
				return nil, nil, nil, fmt.Errorf("IdSegment.Issuer: %s and %s share %q; the three records must live on independent etcd clusters", other, group.name, endpoint)
			}
			owner[key] = group.name
		}
	}
	return primary, secondary, registry, nil
}

// StoreConfig 决定 MySQL 侧的 store(快照 / 流水 / 号段 / 名字注册表)装配不起来时进程怎么办。
type StoreConfig struct {
	// Required=true:四个 store 任一没装配起来(MySQL 启动瞬间不可达、迁移失败、表结构不对)就**拒绝启动**,
	// 而且发生在任何 etcd 注册之前。缺省 false = 沿用旧行为:降级运行,依赖它的 RPC 各自返回错误。
	//
	// 为什么多副本必须开:store 为 nil 的实例 gRPC 健康检查照样是 SERVING、两条 etcd 注册照常进行。
	// 单实例时这是"全挂",很显眼;多实例时是**按比例失败**(p2c / 随机挑到它的请求才失败),隐蔽得多;
	// 滚动更新还会把健康的旧 Pod 换成一个带病的新 Pod。K8s 的 ConfigMap 所有档位都写 true(k8s_deploy.ps1)。
	//
	// 用裸 bool:安全值恰好不是零值,但零值 = 旧语义,整段 `Store:` 缺失(本地 yaml)时行为不变
	// (与下面 SchemaConfig 用 *bool 的理由相反:那里零值是危险值)。
	Required bool `json:",optional"`
}

// ── 玩家归属映射(docs/design/player-route-mapping-durability-proposal.md §15)──

// RouteStore 段各键的默认值(Normalize 照抄这一份;etc/data_service.yaml 写的就是它们)。
const (
	DefaultRouteStoreMaxOpenConn          = 20
	DefaultRouteStoreMaxIdleConn          = 5
	DefaultRouteStoreMergedZoneRefreshSec = 5
)

// MaxRouteStoreMergedZoneRefreshSec 是 MergedZoneRefreshSec 的上界:它就是合服映射的滞后上限,
// 各服务 2 个周期内全部生效,周期再大,合服生效的等待就被它拖长了。
const MaxRouteStoreMergedZoneRefreshSec = 60

// RouteStoreConfig 是 player_route 连接池与合服映射缓存的运行参数。整段可缺失。
//
// 【为什么零值 = "没配"、默认值由 Normalize 填】理由同 PlayerNameConfig:go-zero 不下钻一个未出现的
// optional 结构,default 标签一个都不会回填。
//
// 【为什么配错拒启而不是纠正】理由同 PlayerNameConfig.Validate:配置与实际行为对不上,这种偏差不报错也没有日志。
type RouteStoreConfig struct {
	// MaxOpenConn / MaxIdleConn:route store **独立**连接池(会话级 RC + 锁等待 1s,不能与别的 store 共用池)。
	MaxOpenConn int `json:",optional"`
	MaxIdleConn int `json:",optional"`
	// MergedZoneRefreshSec:定时重读 zone_merged_into 的周期(秒),[1, 60];0 = 缺省 5。它是归属解析的滞后上限
	// (placement.MergedZoneCache,docs/design/player-data-tidb-scaling-plan.md §7.1)。启动时读不到拒启,之后读失败保留旧值。
	MergedZoneRefreshSec int `json:",optional"`
}

// Normalize 返回一份把"没配"(零值)换成默认值的副本。负数不纠正,交给 Validate 拒绝。
// 刻意返回值而不是原地改(同 PlayerNameConfig.Normalize)。
func (r RouteStoreConfig) Normalize() RouteStoreConfig {
	if r.MaxOpenConn == 0 {
		r.MaxOpenConn = DefaultRouteStoreMaxOpenConn
	}
	if r.MaxIdleConn == 0 {
		r.MaxIdleConn = DefaultRouteStoreMaxIdleConn
	}
	if r.MergedZoneRefreshSec == 0 {
		r.MergedZoneRefreshSec = DefaultRouteStoreMergedZoneRefreshSec
	}
	return r
}

// Validate 检查(通常是 Normalize 之后的)取值。任何一项非法都返回 error。
func (r RouteStoreConfig) Validate() error {
	if r.MaxOpenConn <= 0 {
		return fmt.Errorf("RouteStore.MaxOpenConn must be positive, got %d", r.MaxOpenConn)
	}
	if r.MaxIdleConn < 0 || r.MaxIdleConn > r.MaxOpenConn {
		// database/sql 会**静默**把 idle 压到 open,配置与实际行为从此对不上(同 PlayerNameConfig)。
		return fmt.Errorf("RouteStore.MaxIdleConn must be in [0, MaxOpenConn=%d], got %d", r.MaxOpenConn, r.MaxIdleConn)
	}
	if r.MergedZoneRefreshSec < 1 || r.MergedZoneRefreshSec > MaxRouteStoreMergedZoneRefreshSec {
		return fmt.Errorf("RouteStore.MergedZoneRefreshSec must be in [1, %d], got %d", MaxRouteStoreMergedZoneRefreshSec, r.MergedZoneRefreshSec)
	}
	return nil
}

// SchemaConfig 建表策略。
type SchemaConfig struct {
	// AutoMigrate 缺省(nil)= true:启动时用 proto2mysql 按 proto 定义对四张表逐张
	// CreateOrUpdateTable(建表 / 补列 / 补主键)。
	// 其中 player_name 走 schema.go 的手写 bootstrap DDL
	// (proto2mysql 把 string 渲染成 MEDIUMTEXT,TEXT 列上建不了唯一键),
	// 迁移对它们只做列漂移校验。
	// 另有路由表(player_route / zone_merged_into / route_meta)同样走手写 DDL,
	// 迁移按各自的 proto 只守列名与主键,并种 route_meta(id=1)。
	//
	// 生产必须显式设 false:存量大表上的 ALTER 会持有 MDL,阻塞期间所有 GM 回滚/流水查询停摆,
	// 这种事不该发生在某个副本的启动路径上;改为部署阶段显式跑一次 `data_service -f <yaml> -migrate`
	// (同一段代码、单进程、跑完即退)。设 false 时启动路径不碰任何 DDL。
	// 多副本同时启动本身是安全的:迁移由库级命名锁串行(store.acquireMigrateLock),后到的等先到的跑完。
	//
	// 为什么是 *bool 而不是 `bool json:",default=true"`:go-zero 的 mapping **不下钻**
	// 一个"整段 optional 且未出现"的嵌套结构,里面的 default 标签一个都不会回填
	// (与 go/db 的 BlobGuardConfig、login 的 IdSegment.Enabled 同一个坑)。用裸 bool
	// 时,一份没有 `Schema:` 段的 yaml 会得到 AutoMigrate=false —— 启动路径一条 DDL 都
	// 不跑,三个 store 打在一个没有表的库上,快照 / txlog / AllocateIdSegment 每一次
	// 调用都在运行期炸;而在收口手写 DDL 之前,store 自带的 CREATE TABLE IF NOT EXISTS
	// 还兜着这一层。指针把"没写"与"显式写了 false"区分开:nil = 用安全默认。
	//
	// 键名保持 AutoMigrate(不改成反向的 SkipAutoMigrate):
	// tools/scripts/k8s_deploy.ps1 的 data-service ConfigMap 会写死
	// `Schema: AutoMigrate: <dev 取服务 yaml / staging|prod 固定 false>`,改名会让那份
	// 生产配置变成一个被静默忽略的未知键,反而把生产切回启动期 DDL。
	AutoMigrate *bool `json:",optional"`
}

// ShouldAutoMigrate 是启动路径是否跑 DDL 的唯一判据:没配 = 建表(安全默认)。
func (c Config) ShouldAutoMigrate() bool {
	return c.Schema.AutoMigrate == nil || *c.Schema.AutoMigrate
}

// KafkaConfig 两条落库消费者的参数。分区数 transaction_log 6、snapshot 3 起步;(2026-11-06 起)两个审计 topic
// **只许加、可以原地加**:调大 TransactionLogPartitions / SnapshotPartitions、滚动 data_service,EnsureTopics 就把
// broker 上的分区加到配置值(kafkautil.TopicSpec.AllowPartitionGrowth,落库不依赖分区内顺序,
// docs/design/audit-pipeline-horizontal-scale-20261106.md §2.A)。**减**分区仍只能离线换代
// (TopicGeneration +1,停写 → 排空 → 三方同改),步骤见 docs/ops/audit-topic-generation-bump-runbook.md。
//
// 末尾六个键是两条消费者的批量 / 并发旋钮与 lag 探针:Snapshot* 与 LagProbeIntervalMs(G2a),
// TransactionLogMaxInflightFlushes(2026-11-06,流水按分区并行落库)。
// 它们没有 default 标签、零值 = "没配",由 Normalize 填默认、ValidateConsumers 拒绝越界值 ——
// 理由同 RouteStoreConfig:go-zero 不下钻一个未出现的 optional 结构,default 标签在整段 Kafka 缺失时一个都不回填。
type KafkaConfig struct {
	Brokers []string `json:",optional"`

	// TopicGeneration 是分区契约的**代号**,进 topic 名(见 EffectiveTransactionLogTopic)。
	// 与 go/db、go/login 的 Kafka.TopicGeneration 同一个机制,但这里**连 1 也带后缀**。
	//
	// 为什么必须带:broker 是 auto.create.topics.enable=true + num.partitions=1
	// (deploy/docker-compose.yml KAFKA_NUM_PARTITIONS=1),而 C++ scene 一发消息就把
	// 裸名字的 topic 自动建成 1 分区。EnsureTopics 拿 6/3 的契约去比,永远得到
	// "partition contract mismatch",两条消费者一条都起不来,唯一症状是每 30s 一条
	// Error 日志 —— 流水/快照全被保留期吃掉。带代号的名字与自动建出来的那两个裸名
	// topic 互不相干,第一次就能按契约建出 6/3 分区。
	//
	// (2026-11-06 更正)加分区不再要换代:落库不依赖同一玩家的消息顺序(流水 tx_id 主键 + INSERT IGNORE,
	// 快照 guid 唯一键),调大下面两个分区数即可原地加(见 KafkaConfig 类型注释)。broker 上被自动建成 1 分区的
	// 同名 topic 也会被加到配置值,不再永远 mismatch。只有**减**分区要把这个数 +1(换一批新 topic、老 topic 排空后删)。
	TopicGeneration uint32 `json:",default=1"`

	// TransactionLogTopic 是**基名**;实际使用的名字是 EffectiveTransactionLogTopic()。
	// 基名必须与 C++ 生产者的 topic 常量一致(全局单 topic,key=player_id)。
	TransactionLogTopic         string `json:",default=transaction_log_topic"`
	TransactionLogPartitions    int32  `json:",default=6"`
	TransactionLogConsumerGroup string `json:",default=data_service-transaction-log"`

	// SnapshotTopic 同上。单条消息(整份 PlayerAllData)代码注释估几百 KB、未实测;是否攒批由下面的
	// SnapshotBatchMaxRows 决定,默认 1 = 逐条落、逐条提交(2026-10-12 之前的行为)。
	SnapshotTopic         string `json:",default=player_snapshot_topic"`
	SnapshotPartitions    int32  `json:",default=3"`
	SnapshotConsumerGroup string `json:",default=data_service-player-snapshot"`

	// RetentionMs 两个 topic 的保留期,默认 30 天。审计数据宁可积压不许丢:
	// 消费者停下(DB 长时间不可用)期间的消息要在这个窗口内被补消费掉。
	// 注意它不是先到的上限:broker 的每分区 log.retention.bytes(prod 512MiB)在开服速率下几十秒到几分钟就能到,
	// 积压超过它的未消费快照会被删(本服务只检测,见 data_service_kafka_consumer_retention_gap_messages)。
	RetentionMs int64 `json:",default=2592000000"`
	// TxLogRetentionBytes、SnapshotRetentionBytes 是两个 topic 的每分区 retention.bytes,0 = 沿用 broker 的
	// log.retention.bytes。上面说的「积压超过它就被删」靠这里放宽,生产按 Kafka 盘预算显式配。
	TxLogRetentionBytes    int64 `json:",optional"`
	SnapshotRetentionBytes int64 `json:",optional"`

	// SnapshotBatchMaxRows:player_snapshot 每个分区一批最多多少条消息(含被跳过的坏消息),[1, MaxSnapshotBatchMaxRows]。
	// 0 = 没配 = 1(逐条,现状)。>1 时预读剔除已存在的 guid、按 guid 升序一条普通多行 INSERT 落库
	// (撞 1062 转逐行,见 store.InsertSnapshotsIfGuidAbsent),成功后提交本批最高 offset;批量失败自动逐条兜底。
	SnapshotBatchMaxRows int `json:",optional"`
	// SnapshotBatchMaxBytes:一批 data 字节的**软上限**,[MinSnapshotBatchMaxBytes, MaxSnapshotBatchMaxBytes];
	// 0 = 没配 = DefaultSnapshotBatchMaxBytes。每加入一条后检查,累计 >= 它即落库,所以一批最多超出「一条消息」的大小。
	SnapshotBatchMaxBytes int `json:",optional"`
	// SnapshotFlushIntervalMs:批非空后最多再等多久凑批,[0, MaxSnapshotFlushIntervalMs];0 = 不等,只拼已排队的消息。
	SnapshotFlushIntervalMs int `json:",optional"`
	// SnapshotMaxInflightFlushes:本实例同时落库的分区数上限(信号量),[1, max(1, SnapshotMySQL.MaxOpenConn−1)];0 = 没配 = 1。
	// 上限留一条连接:快照池同时服务 GM 快照 / 回档读,积压时不能被消费者占满;本旋钮也不新增任何 MySQL 连接。
	SnapshotMaxInflightFlushes int `json:",optional"`
	// TransactionLogMaxInflightFlushes:transaction_log 消费者本实例同时落库的分区数上限(信号量,2026-11-06),
	// [1, max(1, SnapshotMySQL.MaxOpenConn−1)];0 = 没配 = 1(同一时刻只有一条 INSERT,与改动前一条循环串行落库相同)。
	// 每个分区各攒各的批(200 行或 200ms),调大它一个副本才能同时写分到的几个分区;流水池与快照池同一份
	// SnapshotMySQL 配置、各自独立,上限同样留一条连接给 GM 流水查询 / 召回。本旋钮不新增 MySQL 连接。
	TransactionLogMaxInflightFlushes int `json:",optional"`
	// LagProbeIntervalMs:消费组 lag / 保留期丢失量探针的周期,0 = 关,否则 [MinLagProbeIntervalMs, MaxLagProbeIntervalMs]。
	// 只读 broker 元数据、不碰消费与落库;关掉时积压 / 保留期告警全部是哑的。
	LagProbeIntervalMs int `json:",optional"`
}

// Kafka 段 G2a 五个键的默认值与边界(Normalize / ValidateConsumers 照抄这一份;etc/data_service.yaml 与 runbook 引用它)。
const (
	DefaultSnapshotBatchMaxBytes = 4 << 20
	// MaxSnapshotBatchMaxRows 与 store.MaxSnapshotBatchRows 相等(单测钉住):超过它的批会被 store 拒绝,
	// 每一批都退回逐条兜底,静默变慢。
	MaxSnapshotBatchMaxRows  = 256
	MinSnapshotBatchMaxBytes = 64 << 10
	// MaxSnapshotBatchMaxBytes 远低于 MySQL 8.0 默认 max_allowed_packet 64MiB,也低于 TiDB txn-total-size-limit 100MB;
	// 但**高于** TiKV raft-entry-max-size 默认 8MB —— 今天 TiDB 上建不出唯一键、批量不生效(退路逐行);将来 TiDB 形态的
	// 去重让批量在 TiDB 上生效时,批字节须 <= 4MiB(超了由消费者的逐条兜底接住,不会卡死,只是变慢)。
	// 调大前还要核对目标库的 innodb_redo_log_capacity:单事务 BLOB 不得超过其 10%(默认 100MB → 10MB)。
	MaxSnapshotBatchMaxBytes   = 32 << 20
	MaxSnapshotFlushIntervalMs = 5000
	MinLagProbeIntervalMs      = 5000
	MaxLagProbeIntervalMs      = 600000

	// snapshotPoolFallbackMaxOpen 是 SnapshotMySQL.MaxOpenConn <= 0 时快照池的实际上限,
	// 与 store/mysql.go openMySQLWithDSN 的回落值相同(那里写死 5)。
	snapshotPoolFallbackMaxOpen = 5
)

// Normalize 返回一份把消费者旋钮(G2a 五个键与 TransactionLogMaxInflightFlushes)的"没配"(零值)换成默认值的副本;
// 已有键(TopicGeneration 等)的语义不在这里动。
// 负数不纠正,交给 ValidateConsumers 拒绝(同 RouteStoreConfig.Normalize)。
func (k KafkaConfig) Normalize() KafkaConfig {
	if k.SnapshotBatchMaxRows == 0 {
		k.SnapshotBatchMaxRows = 1
	}
	if k.SnapshotBatchMaxBytes == 0 {
		k.SnapshotBatchMaxBytes = DefaultSnapshotBatchMaxBytes
	}
	if k.SnapshotMaxInflightFlushes == 0 {
		k.SnapshotMaxInflightFlushes = 1
	}
	if k.TransactionLogMaxInflightFlushes == 0 {
		k.TransactionLogMaxInflightFlushes = 1
	}
	// SnapshotFlushIntervalMs / LagProbeIntervalMs 的零值本身就是合法语义(不等待 / 不探),不改。
	return k
}

// ValidateConsumers 检查(通常是 Normalize 之后的)消费者旋钮(G2a 五个键与 TransactionLogMaxInflightFlushes)。
// snapshotPoolMaxOpen 传 SnapshotMySQL.MaxOpenConn
// (<= 0 时按快照池的实际回落值 5 算)。任何一项越界都返回 error,与 Brokers 是否为空无关:
// 一份"开 Kafka 才炸"的配置不该等到开的那一刻才被发现(同 RouteStoreConfig.Validate)。
func (k KafkaConfig) ValidateConsumers(snapshotPoolMaxOpen int) error {
	if k.SnapshotBatchMaxRows < 1 || k.SnapshotBatchMaxRows > MaxSnapshotBatchMaxRows {
		return fmt.Errorf("Kafka.SnapshotBatchMaxRows must be in [1, %d], got %d", MaxSnapshotBatchMaxRows, k.SnapshotBatchMaxRows)
	}
	if k.SnapshotBatchMaxBytes < MinSnapshotBatchMaxBytes || k.SnapshotBatchMaxBytes > MaxSnapshotBatchMaxBytes {
		return fmt.Errorf("Kafka.SnapshotBatchMaxBytes must be in [%d, %d], got %d",
			MinSnapshotBatchMaxBytes, MaxSnapshotBatchMaxBytes, k.SnapshotBatchMaxBytes)
	}
	if k.SnapshotFlushIntervalMs < 0 || k.SnapshotFlushIntervalMs > MaxSnapshotFlushIntervalMs {
		return fmt.Errorf("Kafka.SnapshotFlushIntervalMs must be in [0, %d], got %d", MaxSnapshotFlushIntervalMs, k.SnapshotFlushIntervalMs)
	}
	pool := snapshotPoolMaxOpen
	if pool <= 0 {
		pool = snapshotPoolFallbackMaxOpen
	}
	maxInflight := max(1, pool-1)
	if k.SnapshotMaxInflightFlushes < 1 || k.SnapshotMaxInflightFlushes > maxInflight {
		return fmt.Errorf("Kafka.SnapshotMaxInflightFlushes must be in [1, %d] (SnapshotMySQL pool %d minus one connection kept for GM snapshot / rollback reads), got %d",
			maxInflight, pool, k.SnapshotMaxInflightFlushes)
	}
	// 流水池与快照池由同一份 SnapshotMySQL 配置各开一个(store.NewTransactionLogStore / NewSnapshotStore),上限同一个算法。
	if k.TransactionLogMaxInflightFlushes < 1 || k.TransactionLogMaxInflightFlushes > maxInflight {
		return fmt.Errorf("Kafka.TransactionLogMaxInflightFlushes must be in [1, %d] (transaction_log pool %d minus one connection kept for GM queries / recall), got %d",
			maxInflight, pool, k.TransactionLogMaxInflightFlushes)
	}
	if k.LagProbeIntervalMs != 0 && (k.LagProbeIntervalMs < MinLagProbeIntervalMs || k.LagProbeIntervalMs > MaxLagProbeIntervalMs) {
		return fmt.Errorf("Kafka.LagProbeIntervalMs must be 0 (off) or in [%d, %d], got %d",
			MinLagProbeIntervalMs, MaxLagProbeIntervalMs, k.LagProbeIntervalMs)
	}
	return nil
}

// EffectiveTransactionLogTopic 是消费者与 EnsureTopics 真正使用的 topic 名。
func (k KafkaConfig) EffectiveTransactionLogTopic() string {
	return topicForGeneration(k.TransactionLogTopic, k.TopicGeneration)
}

// EffectiveSnapshotTopic 同上。
func (k KafkaConfig) EffectiveSnapshotTopic() string {
	return topicForGeneration(k.SnapshotTopic, k.TopicGeneration)
}

// topicForGeneration 拼 `<base>_g<N>`。base 为空时返回空,交给 kafka.Start 的 validate
// 报错,而不是拼出一个只有后缀的名字。generation=0 只可能来自"整段 Kafka 未出现"
// (go-zero 不回填未出现结构里的 default),此时按第一代 1 处理:名字保持规范。
// 2026-11-11 起 go/db 的审计发件箱转发是这两个 topic 的生产者,两边共用 kafkautil.AuditTopicName,不各拼各的。
func topicForGeneration(base string, generation uint32) string {
	return kafkautil.AuditTopicName(base, generation)
}

type SnapshotMySQLConfig struct {
	// Backend:tidb | mysql,不写就是 tidb(docs/design/player-data-tidb-scaling-plan.md §9);整段缺席时是空串,同样按 tidb。
	// 校验在 ResolveGlobalAuthority,工具入口由 authoritysql.Open 拒绝非法值。
	Backend     string `json:",default=tidb"`
	Host        string `json:",default=127.0.0.1:3306"`
	User        string `json:",default=appuser"`
	Password    string `json:",default=apppass123"`
	DBName      string `json:",default=testdb"`
	MaxOpenConn int    `json:",default=5"`
	MaxIdleConn int    `json:",default=2"`
	// DbPlacement:按映射拨号(docs/design/horizontal-scale-logical-db.md §8.1),没写就固定连 Host。
	DbPlacement dbplacement.Config `json:",optional"`
}

// ── 玩家名字注册表(docs/design/guild-phase2/03-names.md §3.6a)──────────────

// PlayerNameConfig 是名字注册表的运行参数:连接池 + 三个时间窗。
//
// 【为什么不用 `json:",default=..."` 标签】go-zero 的 mapping **不下钻**一个"整段
// optional 且未出现"的嵌套结构,里面的 default 一个都不会回填(同 SchemaConfig.AutoMigrate
// 与 IdSegmentConfig 注释里的坑)。而本段的零值全部是**危险值**,不是安全值:
// CacheTTL=0 会让每次回填都被 SetPlayerNames 当成"不写无过期键"而报错,
// ReleaseWindow=0 会让建角失败后的条件释放什么都删不掉(留孤儿)。
// 所以默认值由 Normalize() 在代码里填,零值 = "没配",与"显式配了 0"不作区分:
// 这三个时间窗都没有"配成 0"的合法语义,不需要那一层区分。
//
// 数值本身**不是玩法数值**(玩法侧的 2–12 字在配表 RoleNameRule 里,由 login 把关),
// 而是运维旋钮:压测时调连接池、事故时调缓存时长,所以留在 yaml 而不是配表。
type PlayerNameConfig struct {
	// MaxOpenConn / MaxIdleConn 是名字 store **独立**连接池的大小。
	// 独立的理由:建角路径上每次都有一条同步 INSERT,与快照/号段共用 SnapshotMySQL
	// 那 5 条连接时,一次 GM 批量回滚就能把建角卡住。
	MaxOpenConn int `json:",optional"`
	MaxIdleConn int `json:",optional"`

	// ReleaseWindow 是**不带 x-admin-token** 的 ReleasePlayerName 能触碰的最大登记年龄:
	// 只有 created_ms >= now-ReleaseWindow 的行才允许删。
	//
	// 这道窗是安全边界,不是性能旋钮:这个 RPC 在集群内网无鉴权可达,没有窗口的话,
	// 任何能拨到 data_service 的进程都能把一个在役角色的名字释放掉,而名字一释放就可能
	// 立刻被别人占走,不可回滚。窗口只需覆盖 login 建角失败后的补偿(立即一次 + 约 10s
	// 后一次),10 分钟已经极其宽裕。运维清孤儿走 admin token 那条路,不限时间。
	ReleaseWindow time.Duration `json:",optional"`

	// CacheTTL 是 player:name:{id} 正缓存的存活时间(NameCacheRedis)。v1 不支持改名,名字**内容**不会变,
	// 所以可以给很长;上限的意义只在"名字被释放且 DEL 缓存失败"时封顶脏值。
	CacheTTL time.Duration `json:",optional"`

	// NegativeCacheTTL 是"库里确实没有这个 id 的名字"这一事实的缓存时间。
	// 它挡的是帮会成员列表里那些早于本功能建的老角色 —— 没有负缓存,每次开帮会面板
	// 都会把它们全部回源查库一遍。给得短是因为它会被"刚建好的角色"打脸:虽然 Reserve
	// 成功后会用 SET 覆盖掉负缓存,但覆盖发生在 data_service 这一侧,别的路径建的号
	// (回档恢复)只能等它过期。
	NegativeCacheTTL time.Duration `json:",optional"`
}

// 默认值集中在这里,yaml 与文档照抄这一份(设计 §3.6a)。
const (
	DefaultPlayerNameMaxOpenConn      = 20
	DefaultPlayerNameMaxIdleConn      = 5
	DefaultPlayerNameReleaseWindow    = 10 * time.Minute
	DefaultPlayerNameCacheTTL         = 24 * time.Hour
	DefaultPlayerNameNegativeCacheTTL = 60 * time.Second

	// MaxPlayerNameReleaseWindow 是 ReleaseWindow 的硬上限。超过它一律拒绝装配
	// (见 Validate),而不是"配多大算多大":这个窗口越大,无鉴权释放能够到的在役
	// 角色就越多。24 小时已经远超任何一次建角补偿的时间尺度,写得比它还大只可能是
	// 把它当成了缓存时长之类的东西填错了地方。
	MaxPlayerNameReleaseWindow = 24 * time.Hour
)

// Normalize 返回一份把"没配"(零值)换成默认值的副本。
//
// 刻意返回值而不是原地改:调用方(svc.NewServiceContext)拿到的 Config 是值拷贝,
// 显式赋值一次比藏在指针方法里的副作用好追。负数不在这里纠正 —— 那是配错了,
// 该由 Validate 拒掉,而不是被悄悄改成默认值(见 Validate 的理由)。
func (p PlayerNameConfig) Normalize() PlayerNameConfig {
	if p.MaxOpenConn == 0 {
		p.MaxOpenConn = DefaultPlayerNameMaxOpenConn
	}
	if p.MaxIdleConn == 0 {
		p.MaxIdleConn = DefaultPlayerNameMaxIdleConn
	}
	if p.ReleaseWindow == 0 {
		p.ReleaseWindow = DefaultPlayerNameReleaseWindow
	}
	if p.CacheTTL == 0 {
		p.CacheTTL = DefaultPlayerNameCacheTTL
	}
	if p.NegativeCacheTTL == 0 {
		p.NegativeCacheTTL = DefaultPlayerNameNegativeCacheTTL
	}
	return p
}

// Validate 检查(通常是 Normalize 之后的)取值是否可用。
//
// 【为什么配错要拒绝而不是纠正】纠正意味着运维看到的 yaml 与进程实际行为不一致,
// 而这几个键里有一个是安全边界(ReleaseWindow)。拒绝的代价是名字 store 不装配 →
// 建角被拒(fail-closed,症状明显、当场能看出来);纠正的代价是一个悄悄放宽了的
// 释放窗口在事故发生前谁都不知道。
func (p PlayerNameConfig) Validate() error {
	if p.MaxOpenConn <= 0 {
		return fmt.Errorf("PlayerName.MaxOpenConn must be positive, got %d", p.MaxOpenConn)
	}
	if p.MaxIdleConn < 0 {
		return fmt.Errorf("PlayerName.MaxIdleConn must not be negative, got %d", p.MaxIdleConn)
	}
	if p.MaxIdleConn > p.MaxOpenConn {
		// database/sql 会**静默**把 idle 压到 open,配置与实际行为从此对不上。
		return fmt.Errorf("PlayerName.MaxIdleConn (%d) must not exceed MaxOpenConn (%d): database/sql silently clamps it",
			p.MaxIdleConn, p.MaxOpenConn)
	}
	if p.ReleaseWindow <= 0 {
		return fmt.Errorf("PlayerName.ReleaseWindow must be positive, got %v", p.ReleaseWindow)
	}
	if p.ReleaseWindow > MaxPlayerNameReleaseWindow {
		return fmt.Errorf("PlayerName.ReleaseWindow %v exceeds the hard cap %v: an unauthenticated release must not reach names older than a create-player compensation",
			p.ReleaseWindow, MaxPlayerNameReleaseWindow)
	}
	if p.CacheTTL <= 0 {
		return fmt.Errorf("PlayerName.CacheTTL must be positive, got %v", p.CacheTTL)
	}
	if p.NegativeCacheTTL <= 0 {
		return fmt.Errorf("PlayerName.NegativeCacheTTL must be positive, got %v", p.NegativeCacheTTL)
	}
	return nil
}

// 审计表清理的默认值:快照是整档,保留期和 Kafka 的 RetentionMs 对齐;流水查刷物和追责要回溯得更久。
const (
	defaultAuditSnapshotDays       = 30
	defaultAuditTransactionLogDays = 180
	defaultAuditBatchSize          = 2000
	defaultAuditIntervalSeconds    = 3600
)

// AuditRetentionConfig 控制审计表清理循环(store.RunAuditRetention)。字段不大于 0 时用默认值。
type AuditRetentionConfig struct {
	// Disabled 关掉清理。
	Disabled           bool `json:",optional"`
	SnapshotDays       int  `json:",optional"`
	TransactionLogDays int  `json:",optional"`
	BatchSize          int  `json:",optional"`
	IntervalSeconds    int  `json:",optional"`
}

// Normalized 把不大于 0 的字段换成默认值。
func (c AuditRetentionConfig) Normalized() AuditRetentionConfig {
	if c.SnapshotDays <= 0 {
		c.SnapshotDays = defaultAuditSnapshotDays
	}
	if c.TransactionLogDays <= 0 {
		c.TransactionLogDays = defaultAuditTransactionLogDays
	}
	if c.BatchSize <= 0 {
		c.BatchSize = defaultAuditBatchSize
	}
	if c.IntervalSeconds <= 0 {
		c.IntervalSeconds = defaultAuditIntervalSeconds
	}
	return c
}
