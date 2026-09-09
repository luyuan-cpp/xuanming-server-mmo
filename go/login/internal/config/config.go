package config

import (
	"fmt"
	"time"

	"github.com/IBM/sarama"
	"github.com/zeromicro/go-zero/zrpc"

	"shared/idsegment"
)

// Config is the top-level configuration.
type Config struct {
	zrpc.RpcServerConf                    // Embedded go-zero RPC config (ListenOn, Etcd, etc.)
	Node               NodeConfig         `json:"Node"`
	Snowflake          SnowflakeConf      `json:"Snowflake"`
	Locker             LockerConf         `json:"Locker"`
	Account            AccountConf        `json:"Account"`
	Registry           RegistryConf       `json:"Registry"`
	Timeouts           TimeoutConf        `json:"Timeouts"`
	Kafka              KafkaConfig        `json:"Kafka"`
	PlayerLocatorRpc   zrpc.RpcClientConf `json:"PlayerLocatorRpc"` // player_locator gRPC client
	SceneManagerRpc    zrpc.RpcClientConf `json:"SceneManagerRpc"`  // scene_manager gRPC client
	// DataServiceRpc 是 data_service 的 gRPC 客户端(etcd 发现,Key=dataservice.rpc)。
	// login 目前只用它领 PlayerId 号段(AllocateIdSegment);IdSegment.Enabled=false 时
	// 根本不拨号,所以标 optional —— 但 Enabled=true 而这块缺失会在启动时明确拒绝。
	DataServiceRpc zrpc.RpcClientConf `json:"DataServiceRpc,optional"`
	// IdSegment 控制 PlayerId 的号段发号(docs/design/node-id-overhaul-plan-20260908.md §6)。
	// 字段语义见 shared/idsegment.Conf。**没写这块 = 号段关闭 = 纯 snowflake 老路径**,
	// 这就是设计稿 §6.4 说的「保留一个版本做回滚开关」。
	IdSegment idsegment.Conf `json:"IdSegment,optional"`
	// GateTokenSecret 已废弃:单个 string 无法不停服轮换,而且 gate token 与
	// 排队 token 共用同一把密钥,信任域没拆。保留字段只为让 deploy/*.yaml 与
	// tools/scripts/k8s_deploy.ps1 这些**仓外**部署产物在过渡期仍能起服
	// (ResolveSecrets 会按兼容路径读它并打 WARN)。新配置请写 Secrets 块。
	GateTokenSecret string `json:"GateTokenSecret,optional"`
	// Secrets 是按用途隔离的 HMAC 密钥集合,支持三段式不停服轮换。
	// 详见 secrets.go 的包注释。
	Secrets SecretsConf `json:"Secrets,optional"`
	// InternalAuth 是内部调用方身份声明(x-session-detail-bin)的验签策略。
	InternalAuth InternalAuthConf `json:"InternalAuth,optional"`
	// KillSwitch 是 RPC 级热关停闸门(shared/killswitch)的配置。
	KillSwitch    KillSwitchConf `json:"KillSwitch,optional"`
	TableDir      string         `json:",default=../../generated/tables"`
	AuthProviders AuthConfig     `json:"AuthProviders,optional"` // Third-party auth provider config
	// ClusterId 是部署级集群号:PlayerId(bwmarrin 13 位 node 段)切成 [cluster3][slot10]
	// 的 cluster 部分(docs/design/node-id-overhaul-plan-20260908.md §5)。**运维按集群
	// 一次性设定,策划不碰**;默认 0 = 单集群 / 存量 PlayerId 布局。etcd 槽位前缀带
	// c<cluster>,两个集群共用一个 etcd 也不会撞号。必须 < 8。
	ClusterId uint32 `json:"ClusterId,default=0"`
	// SnowflakeCacheDir 是 PlayerId 槽位的本地缓存目录(shared/snowflakealloc,设计稿 §3.5):
	// 启动时 etcd 不可达、且缓存里的上次水位确认在 F(2h)内,就用缓存的槽起服并后台重试
	// 注册。留空关闭。默认相对服务工作目录(go/login/)指向仓库 run/(已 gitignore)。
	SnowflakeCacheDir string `json:"SnowflakeCacheDir,default=../../run/snowflake"`
	// DevSkipAuth 是已废弃的不安全开关。保留字段只为让旧配置在启动时
	// 明确失败,不能因为结构体删字段而静默忽略、让开发者误以为仍生效。
	DevSkipAuth     bool                `json:"DevSkipAuth,optional"`
	PasswordAuth    PasswordAuthConf    `json:"PasswordAuth,optional"`
	DevPasswordAuth DevPasswordAuthConf `json:"DevPasswordAuth,optional"`
	TokenConfig     TokenConf           `json:"TokenConfig,optional"` // Access/refresh token TTL settings
	PreloadPool     PreloadPoolConf     `json:"PreloadPool,optional"` // Background goroutine pool for player data preload

	// LegacyGateLoginEnabled controls whether the deprecated path
	// "Client → cpp gate (TCP) → ClientPlayerLogin.Login" is still served.
	//
	// Background: ARCH §12 T+2 step. The new path (Java Gateway POST
	// /api/login → gRPC login) has been the recommended route since
	// 2026-05; the legacy path is kept as backwards compatibility with
	// throttled deprecation telemetry (legacy_login_count counter).
	// Once that counter trends below the agreed threshold per T+1 exit
	// criteria, ops flips this flag to false and the legacy branch
	// returns kLoginUnknownError, forcing the few remaining old
	// clients to fall through to /api/login (or their own retry path,
	// which typically re-resolves via /api/assign-gate).
	//
	// Detection is the same SessionDetails-based heuristic
	// loginlogic.go uses for telemetry: a non-zero SessionId in the
	// gRPC context means the call arrived through cpp gate's
	// HandleGrpcNodeMessage forwarder (legacy). When false here, that
	// branch short-circuits BEFORE acquiring locks or writing
	// player_locator state, so flipping the flag back is purely a
	// config change with no cleanup required.
	//
	// Default true preserves current behavior. Operators flip via
	// login.yaml; no restart-blocking dependency on other services.
	LegacyGateLoginEnabled bool `json:"LegacyGateLoginEnabled,default=true"`

	// Queue holds configuration for the AssignGate login queue. Disabled by
	// default so existing deployments keep the legacy "always assign" behavior;
	// flip Enabled=true once the queue path has been validated end-to-end. See
	// docs/design/login-queue-2026-05.md (added with this feature).
	Queue QueueConf `json:"Queue,optional"`

	// HomeZone 控制「按 data_service 的 player:zone 映射修正 zone」的两条链
	// (internal/logic/pkg/homezone 包注释)。整块可缺省,缺省 = 角色列表刷新开、
	// 进游戏重定向关(见 HomeZoneConf 各字段)。
	HomeZone HomeZoneConf `json:"HomeZone,optional"`
}

// HomeZoneConf 是合服后 zone 归属修正的开关。
//
// 两个开关的默认方向**刻意不同**:
//
//   - 角色列表刷新(RefreshRoleListDisabled)沿用 KillSwitchConf 的纪律:go-zero 对整块
//     缺失的 optional 结构体**不会**递归填内层 default,所以反向命名,零值 = 开着。
//     它是合服后的**主修正机制**:客户端按刷新后的 zone_id 选区,直接连到归属 zone 的
//     gate,根本不需要重定向。
//   - 进游戏重定向(RedirectOnEnterEnabled)正向命名,零值 = 关。它依赖客户端实现
//     SceneClientPlayerCommonRedirectToGate(msg 124:断开当前 gate、连 target_ip:port、
//     首包 ClientTokenVerifyRequest、再走 Login + EnterGame)。EnterGame RPC 在触发重定向
//     时已经回了成功并清掉登录会话,客户端若只是把 124 打个日志(Unity 端目前就是这样),
//     玩家就卡在源 zone 的 gate 上既进不了场景也无法重试。**必须等客户端实现了 124 才能
//     置 true**;参考实现见 robot/pkg/redirect.go(FollowRedirect)与
//     robot/logic/handler/scene_client_player_common_redirect_to_gate.go。
//
// 关掉角色列表刷新只在 homezone 自身出问题需要止血时用;关掉后 login 退回修复前行为
// (角色列表报建角 zone),不会拒绝任何请求。
type HomeZoneConf struct {
	// RefreshRoleListDisabled=true:Login 返回的角色列表不再用映射覆盖 zone_id。
	RefreshRoleListDisabled bool `json:"RefreshRoleListDisabled,default=false"`
	// RedirectOnEnterEnabled=true:EnterGame 按归属 zone 触发 scene_manager 的跨区重定向。
	// 默认 false —— 客户端实现 msg 124 之前不许打开(见类型注释)。即使打开,也只对
	// 「首次登录且 player_locator 里没有在场 scene」的请求生效:重连 / 顶号必须回到
	// 原来的 scene,不能被归属 zone 覆盖(entergamelogic.go homeZoneOverrideAllowed)。
	RedirectOnEnterEnabled bool `json:"RedirectOnEnterEnabled,default=false"`
	// RoleListLookupTimeout 是 Login 角色列表那一次 BatchGetPlayerHomeZone 的预算;
	// 0 = homezone.DefaultRoleListLookupTimeout(500ms)。登录链路上串行的一跳,
	// 超时按失败处理(保留建角 zone),宁可偶尔给旧 zone 也不拖慢所有登录。
	RoleListLookupTimeout time.Duration `json:"RoleListLookupTimeout,optional"`
	// EnterLookupTimeout 是 EnterGame 那一次 GetPlayerHomeZone 的预算;
	// 0 = homezone.DefaultEnterLookupTimeout(1.5s)。EnterGame 链路本身是异步的
	// (5 分钟预算),可以比角色列表宽松。
	EnterLookupTimeout time.Duration `json:"EnterLookupTimeout,optional"`
	// RegisterTimeout 是 CreatePlayer 写 player:zone 映射(RegisterPlayerZone)的预算;
	// 0 = homezone.DefaultRegisterTimeout(3s)。这条写失败建角**整体失败**
	// (createplayerlogic.go),所以给得比查询宽。
	RegisterTimeout time.Duration `json:"RegisterTimeout,optional"`
}

// QueueConf controls the login queue (Redis ZSET-backed) that throttles
// AssignGate when a zone's gates are at capacity.
//
// Knobs are deliberately conservative:
//   - DispatchInterval too low burns Redis QPS for no benefit (gate
//     PlayerCount only refreshes every few seconds via etcd anyway).
//   - AdmitTTL must outlive the worst-case "client received admit token →
//     reconnects to /assign-gate → connects to gate" round-trip; 60s covers
//     mobile-network jitter and short backoffs comfortably.
//   - SoftCapMultiplier (e.g. 1.5) is the headroom factor applied to the
//     largest observed PlayerCount when no explicit ZoneCapacityOverride is
//     set, so freshly-deployed zones aren't capped at "current load".
type QueueConf struct {
	Enabled             bool          `json:"Enabled,default=false"`
	DispatchInterval    time.Duration `json:"DispatchInterval,default=1s"`
	AdmitTTL            time.Duration `json:"AdmitTTL,default=60s"`
	QueueEntryTTL       time.Duration `json:"QueueEntryTTL,default=1h"`
	SoftCapMultiplier   float64       `json:"SoftCapMultiplier,default=1.5"`
	DefaultRetryAfterMs uint32        `json:"DefaultRetryAfterMs,default=2000"`
	DispatcherLockTTL   time.Duration `json:"DispatcherLockTTL,default=30s"`
	DispatcherLockKey   string        `json:"DispatcherLockKey,default=dispatcher:lock:login_queue"`
	// ZoneCapacityOverride lets ops pin the per-zone admission ceiling
	// without trusting gate-side soft caps. Key is zone_id (string for YAML),
	// value is the absolute number of concurrent online players permitted.
	ZoneCapacityOverride map[string]uint32 `json:"ZoneCapacityOverride,optional"`
}

// InternalAuthConf 控制内部调用方身份声明的验签闸门(见
// internal/logic/pkg/callerauth)。
//
// **生产模式恒强制验签,这里没有任何开关能关掉它** —— 唯一能放宽的是
// go-zero Mode=dev/test,而那是编排层的决定,不是业务配置。
type InternalAuthConf struct {
	// ForceEnforce 让 dev/test 也走强制档。上游(cpp gate / Java gateway)
	// 接完签名后,先在本地把它打开做联调,再上生产。
	ForceEnforce bool `json:"ForceEnforce,default=false"`

	// MaxClockSkew 是允许的时间戳偏移窗口(双向)。签名里带毫秒时间戳,
	// 超出这个窗口一律拒绝 —— 这是防重放的第一道闸,nonce 是第二道。
	//
	// 30s 的取法:内网 NTP 偏差通常在毫秒级,30s 已经给足容错;
	// 再放大只会线性放大重放窗口和 nonce 表的内存占用。
	MaxClockSkew time.Duration `json:"MaxClockSkew,default=30s"`

	// MaxNonceEntries 是 nonce 表的硬上限。超出后强制翻代(丢掉最旧的一代),
	// 重放窗口会临时缩短但**绝不会**放行验签失败的请求。
	// 默认 500000 ≈ 4000 QPS × 2×MaxClockSkew 的用量。
	MaxNonceEntries int `json:"MaxNonceEntries,default=500000"`

	// AllowedCallers 是调用方标识白名单(如 ["gate","gateway"])。
	// 留空表示不限制调用方名字,但签名仍然必须验过。
	AllowedCallers []string `json:"AllowedCallers,optional"`
}

// KillSwitchConf 配置 RPC 级热关停闸门(见 shared/killswitch 包注释)。
//
// **整块字段的零值就是期望行为**,这不是偷懒而是刻意的:go-zero 的
// conf.MustLoad 走的是 mapping.UnmarshalJsonMap(没开 WithDefault),
// 一个标了 optional 的结构体字段整块缺失时**不会**递归填内层 default
// (见 core/mapping/unmarshaler.go:processNamedFieldWithoutValue)。
// 于是任何一份没写 KillSwitch 块的 yaml —— 包括仓外的 deploy/*.yaml ——
// 都会拿到零值。所以这里绝不能用 `Enabled bool default=true`:那种写法
// 会让老配置静默地把止血阀关掉,而且要等到线上出事才发现。
//
// 零值 = 开着 watch + 库默认前缀/时限。这不会拒绝任何请求:killswitch
// 铁律 fail-open,etcd 连不上、前缀下没 key、值写坏了一律放行,只有运维
// 显式往 etcd 写 deny 才会关停。反过来默认不开才是危险的 —— 真出事时
// 没有止血阀可用。
type KillSwitchConf struct {
	// Disabled 完全不起 etcd watch,也不把拦截器挂进链(全部放行)。
	// 刻意用反向命名,理由见上:缺配置必须等于"开着"。
	// 只有在热关停机制自身出问题时才需要打开这个逃生阀。
	Disabled bool `json:"Disabled,default=false"`

	// Prefix 是规则在 etcd 里的前缀,留空用 killswitch.DefaultPrefix
	// ("/mmorpg/killswitch/")。改它等于换一套规则命名空间,必须与运维
	// 下发规则的路径一致,否则规则永远命不中(而且是静默命不中)。
	Prefix string `json:"Prefix,optional"`

	// StaleAfter 与 etcd 失联多久后主动作废本地规则快照、退回全放行。
	// 0 = 用库默认(1 分钟);负数 = 永不作废。
	StaleAfter time.Duration `json:"StaleAfter,optional"`

	// ResyncBackoff 全量同步失败后的重试间隔,<=0 用库默认(3s)。
	ResyncBackoff time.Duration `json:"ResyncBackoff,optional"`
}

// PreloadPoolConf controls the bounded goroutine pool used to fan out
// fire-and-forget player data preload tasks (Kafka -> DB -> Redis warm).
// Defaults are tuned for ~256 concurrent in-flight preloads, which is far
// more than the Kafka SyncProducer can drain (it is mutex-serialized), so
// the pool acts as a bounded queue + backpressure / overload shield.
type PreloadPoolConf struct {
	Size          int           `json:"Size,default=256"`          // Worker count
	StatsInterval time.Duration `json:"StatsInterval,default=30s"` // Periodic snapshot log interval (0 = disabled)
}

// AuthConfig holds third-party auth provider settings.
type AuthConfig struct {
	SaToken *SaTokenAuthConf `json:"SaToken,optional"`
	WeChat  *WeChatAuthConf  `json:"WeChat,optional"`
	QQ      *QQAuthConf      `json:"QQ,optional"`
	NetEase *NeteaseAuthConf `json:"NetEase,optional"`
}

// PasswordAuthConf 配置生产口令认证。MySQL DSN 含凭证，只允许通过命名的
// 环境变量注入，不能把 DSN/密码直接写进 YAML；运行时账号只需
// user_accounts 的 SELECT 权限。password 必须已由离线工具迁移为 Argon2id
// PHC 字符串；空值和旧明文都会 fail-closed。
type PasswordAuthConf struct {
	Enabled      bool   `json:"Enabled,default=false"`
	DSNEnv       string `json:"DSNEnv,optional"`
	MaxOpenConns int    `json:"MaxOpenConns,default=20"`
	MaxIdleConns int    `json:"MaxIdleConns,default=10"`
	// 单次 Argon2id 默认使用 64 MiB；并发 2 约占 128 MiB。代码硬上限为 8。
	KDFConcurrency int `json:"KDFConcurrency,default=2"`
	// 只约束等待 semaphore 的时间；调用方 context 更早取消时立即失败。
	KDFWaitTimeout time.Duration `json:"KDFWaitTimeout,default=500ms"`
}

// DevPasswordAuthConf 是唯一允许“不接账号库口令体系”的开发登录路径。
// 默认关闭；只有 go-zero Mode=dev/test 才允许开启，并同时要求共享密钥
// 和账号前缀白名单，避免重新引入
// “报上任意 account 即登录”的生产漏洞。共享密钥必须从环境变量注入，
// 不得写入受管配置文件。
type DevPasswordAuthConf struct {
	Enabled bool `json:"Enabled,default=false"`
	// SharedSecretEnv 只能填写环境变量名；共享密钥本身不得进入受管配置。
	SharedSecretEnv        string   `json:"SharedSecretEnv,optional"`
	AllowedAccountPrefixes []string `json:"AllowedAccountPrefixes,optional"`
}

// SaTokenAuthConf holds SA-Token Redis lookup settings.
type SaTokenAuthConf struct {
	Redis     RedisConf `json:"Redis"`
	TokenName string    `json:"TokenName,default=satoken"`
	LoginType string    `json:"LoginType,default=login"`
}

// WeChatAuthConf holds WeChat OAuth settings.
type WeChatAuthConf struct {
	AppId     string `json:"AppId"`
	AppSecret string `json:"AppSecret"`
	// Endpoint optionally overrides the api.weixin.qq.com base URL.
	// Set to a local sandbox mock (e.g. http://127.0.0.1:18090) when
	// real Open Platform credentials / network egress aren't available.
	// Production leaves this empty so the provider hits the real host.
	Endpoint string `json:"Endpoint,optional"`
}

// QQAuthConf holds QQ OAuth settings.
type QQAuthConf struct {
	AppId  string `json:"AppId"`
	AppKey string `json:"AppKey"`
	// Endpoint optionally overrides the graph.qq.com base URL.
	// Same semantics as WeChatAuthConf.Endpoint.
	Endpoint string `json:"Endpoint,optional"`
}

// NeteaseAuthConf holds NetEase auth settings.
type NeteaseAuthConf struct {
	AppKey    string `json:"AppKey"`
	AppSecret string `json:"AppSecret"`
}

// TokenConf holds access/refresh token TTL settings.
type TokenConf struct {
	AccessTokenTTL  time.Duration `json:"AccessTokenTTL,default=2h"`    // Access token lifetime (default 2 hours)
	RefreshTokenTTL time.Duration `json:"RefreshTokenTTL,default=720h"` // Refresh token lifetime (default 30 days)
}

// NodeConfig holds node-level settings including login duration limits.
type NodeConfig struct {
	ZoneId           uint32        `json:"ZoneId"`
	SessionExpireMin uint32        `json:"SessionExpireMin"` // Session idle timeout in minutes
	MaxLoginDevices  uint32        `json:"MaxLoginDevices"`
	RedisClient      RedisConf     `json:"RedisClient"`
	LeaseTTL         int64         `json:"LeaseTTL"` // Lease TTL in seconds
	QueueShardCount  uint64        `json:"QueueShardCount"`
	MaxLoginDuration time.Duration `json:"MaxLoginDuration"` // Max online duration per login (e.g. 24h, force logout on expiry)
	LogoutGraceTime  time.Duration `json:"LogoutGraceTime"`  // Grace period before forced logout (e.g. 5m, for player warning)
}

// RedisConf holds Redis client settings.
type RedisConf struct {
	Host         string        `json:"Host"`
	Password     string        `json:"Password"`
	DB           uint32        `json:"DB"`
	DefaultTTL   time.Duration `json:"DefaultTTL"`
	DialTimeout  time.Duration `json:"DialTimeout"`
	ReadTimeout  time.Duration `json:"ReadTimeout"`
	WriteTimeout time.Duration `json:"WriteTimeout"`
}

// KafkaConfig holds Kafka producer and consumer settings.
type KafkaConfig struct {
	Brokers []string `json:"Brokers"`
	GroupID string   `json:"GroupID"`
	Topic   string   `json:"Topic,optional"` // Derived from Node.ZoneId at startup
	// TopicGeneration is part of the immutable routing identity. Keep 1 for
	// db_task_zone_{ZoneId}; a partition-count change requires an offline drain
	// and a higher generation, which creates a new topic namespace.
	TopicGeneration  uint32                  `json:"TopicGeneration,default=1"`
	PartitionCnt     int32                   `json:"PartitionCnt"`
	InitialPartition int                     `json:"InitialPartition"` // Should match PartitionCnt
	DialTimeout      time.Duration           `json:"DialTimeout"`
	ReadTimeout      time.Duration           `json:"ReadTimeout"`
	WriteTimeout     time.Duration           `json:"WriteTimeout"`
	RetryMax         int                     `json:"RetryMax"`
	RetryBackoff     time.Duration           `json:"RetryBackoff"`
	ChannelBuffer    int                     `json:"ChannelBuffer"`
	SyncInterval     time.Duration           `json:"SyncInterval"`
	StatsInterval    time.Duration           `json:"StatsInterval"`
	CompressionType  sarama.CompressionCodec `json:"CompressionType"` // none/gzip/snappy
	Idempotent       bool                    `json:"Idempotent"`
	MaxOpenRequests  int                     `json:"MaxOpenRequests"`              // Must be 1 when idempotent
	RetentionMs      int64                   `json:"RetentionMs,default=86400000"` // Topic retention in ms (default 24h)
	// P1 数据安全加固 2026-06-03: 旧值 300000 (5min)
	// 太短,db service 卡住会丢数据
}

// SnowflakeConf holds snowflake ID generator settings.
type SnowflakeConf struct {
	Epoch    int64  `json:"Epoch"` // Epoch timestamp in milliseconds
	NodeBits uint32 `json:"NodeBits"`
	StepBits uint32 `json:"StepBits"`
}

// LockerConf holds distributed lock settings.
type LockerConf struct {
	AccountLockTTL uint32 `json:"AccountLockTTL"` // Account lock TTL in seconds
	PlayerLockTTL  uint32 `json:"PlayerLockTTL"`  // Player lock TTL in seconds
}

// AccountConf holds account-related settings.
type AccountConf struct {
	MaxDevicesPerAccount int64         `json:"MaxDevicesPerAccount"`
	MaxPlayersPerAccount int           `json:"MaxPlayersPerAccount,default=5"`
	CacheExpire          time.Duration `json:"CacheExpire"`
}

// RegistryConf holds service registry/discovery settings.
type RegistryConf struct {
	Etcd EtcdRegistryConf `json:"Etcd"`
}

// EtcdRegistryConf holds etcd registry settings.
type EtcdRegistryConf struct {
	Hosts       []string      `json:"Hosts"`
	Key         string        `json:"Key"`
	DialTimeout time.Duration `json:"DialTimeout"`
}

// TimeoutConf holds various timeout settings.
type TimeoutConf struct {
	EtcdDialTimeout         time.Duration `json:"EtcdDialTimeout"`
	ServiceDiscoveryTimeout time.Duration `json:"ServiceDiscoveryTimeout"`
	TaskWaitTimeout         time.Duration `json:"TaskWaitTimeout"`
	LoginTotalTimeout       time.Duration `json:"LoginTotalTimeout"` // Total login timeout including Redis/DB
	RoleCacheExpire         time.Duration `json:"RoleCacheExpire"`
}

var AppConfig Config

// DbTaskTopic returns the zone-specific Kafka topic for DB tasks.
func DbTaskTopic(zoneId uint32) string {
	return fmt.Sprintf("db_task_zone_%d", zoneId)
}

func DbTaskTopicForGeneration(zoneId, generation uint32) string {
	base := DbTaskTopic(zoneId)
	if generation <= 1 {
		return base
	}
	return fmt.Sprintf("%s_g%d", base, generation)
}
